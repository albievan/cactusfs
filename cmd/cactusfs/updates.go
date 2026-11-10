package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/albievan/cactusfs/internal/adapters/releases/github"
	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/binary"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/kubernetes"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/off"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// maxSettingFile bounds the files of update-ca and update-trusted-root: a
// bundle of CA certificates, a trusted root of a few kilobytes.
const maxSettingFile = 1 << 20

// openUpdates wires checking for releases and installing one (ADR-0029):
// the update source when update-source is set, the verifier of the release
// workflow's signatures, the upgrader of upgrade-mode, and the use case.
// Every adapter is reached through its decorator. lifetime is what an
// upgrade is started under: the node's own context, which ends when it
// stops.
func (n *node) openUpdates(lifetime context.Context, cfg serverConfig, view ports.ClusterView, others ports.NodeSettingsReader, logger *slog.Logger) error {
	build := buildinfo.Get()
	current, err := release.ParseVersion(build.Version)
	if err != nil {
		return fmt.Errorf("updates: the version of this build: %w", err)
	}
	// A configuration that was not parsed - one a test built by hand - has
	// the defaults: the stable channel and upgrade-mode off.
	if cfg.UpdateChannel == "" {
		cfg.UpdateChannel = defaultUpdateChannel
	}
	if cfg.UpgradeMode == "" {
		cfg.UpgradeMode = defaultUpgradeMode
	}
	channel, err := release.ParseChannel(cfg.UpdateChannel)
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	var source ports.ReleaseSource
	if cfg.UpdateSource != "" {
		ca, err := readSettingFile("update-ca", cfg.UpdateCA)
		if err != nil {
			return err
		}
		made, err := github.New(github.Config{URL: cfg.UpdateSource, CA: ca, Allow: cfg.EgressAllow, Version: build.Version})
		if err != nil {
			// github.New repeats nothing of the address.
			return fmt.Errorf("update-source: %w", err)
		}
		source = n.telemetry.InstrumentReleaseSource(made)
	}
	verifier, err := openVerifier(cfg)
	if err != nil {
		return err
	}
	upgrader, err := n.openUpgrader(lifetime, cfg, source, view, others, build, logger)
	if err != nil {
		return err
	}
	n.updates, err = app.NewUpdateService(app.UpdateServiceConfig{
		Source: source, Verifier: n.telemetry.InstrumentReleaseVerifier(verifier), Upgrader: n.telemetry.InstrumentUpgrader(upgrader),
		Authz: n.authorizer, Auditor: n.auditor, Clock: n.clock, Sessions: n.sessions,
		Current: current, Build: build.Build, Commit: build.Commit, Channel: channel,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Interval: cfg.UpdateCheckInterval,
		Classify: func(err error) string { return string(github.ClassOf(err)) },
		Entropy:  rand.Reader, Lifetime: lifetime, Logger: logger,
		OnCheck: n.telemetry.ObserveUpdateCheck,
	})
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	if source == nil {
		logger.InfoContext(lifetime, "releases are not checked for: no update source is configured")
	}
	return nil
}

// openVerifier returns the verifier of the release workflow's signatures,
// against the trusted root of update-trusted-root or the embedded one.
func openVerifier(cfg serverConfig) (*sigstore.ReleaseVerifier, error) {
	if cfg.UpdateTrustedRoot == "" {
		verifier, err := sigstore.Default()
		if err != nil {
			return nil, fmt.Errorf("updates: the embedded trusted root: %w", err)
		}
		return sigstore.NewReleaseVerifier(verifier), nil
	}
	root, err := readSettingFile("update-trusted-root", cfg.UpdateTrustedRoot)
	if err != nil {
		return nil, err
	}
	verifier, err := sigstore.NewVerifier(root)
	if err != nil {
		return nil, fmt.Errorf("update-trusted-root %s: %w", cfg.UpdateTrustedRoot, err)
	}
	return sigstore.NewReleaseVerifier(verifier), nil
}

// openUpgrader returns the upgrader of upgrade-mode, undecorated.
func (n *node) openUpgrader(
	ctx context.Context, cfg serverConfig, source ports.ReleaseSource, view ports.ClusterView, others ports.NodeSettingsReader, build buildinfo.Info,
	logger *slog.Logger,
) (ports.Upgrader, error) {
	switch release.Mode(cfg.UpgradeMode) {
	case release.ModeBinary:
		if source == nil {
			return nil, errors.New("upgrade-mode binary needs update-source")
		}
		made, err := binary.New(ctx, binary.Config{
			Source: source, Executable: cfg.executable, Packaging: buildinfo.Packaging(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			// A node on its own is a deployment of one; a cluster has the
			// nodes it recorded, of which none is replaced from the console.
			Nodes: func(ctx context.Context) int {
				if !cfg.clustered() {
					return 1
				}
				nodes, err := view.Nodes(ctx)
				if err != nil {
					return 0
				}
				return len(nodes)
			},
			Restart: cfg.restart, NodeID: n.id, Build: build, Clock: n.clock, Logger: logger, Lifetime: ctx,
		})
		if err != nil {
			return nil, fmt.Errorf("upgrade-mode binary: %w", err)
		}
		return made, nil
	case release.ModeKubernetes:
		dir := cfg.kubernetesAPIDir
		if dir == "" {
			dir = kubernetes.DefaultAPIDir
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("upgrade-mode kubernetes: this node has no token for the Kubernetes API in %s: "+
				"give the chart --set upgrade.fromConsole.enabled=true, or set upgrade-mode to off", dir)
		}
		made, err := kubernetes.New(kubernetes.Config{
			APIBase: kubernetes.APIBaseFromEnv(os.LookupEnv), TokenDir: dir, Self: n.id,
			Nodes: func(ctx context.Context) ([]string, error) { return upgradeOrder(ctx, view, n.id) },
			NodeBuild: func(ctx context.Context, id string) (cluster.Build, error) {
				return n.nodeBuild(ctx, id, others)
			},
			Ready: n.ready, Clock: n.clock, Logger: logger,
		})
		if err != nil {
			return nil, fmt.Errorf("upgrade-mode kubernetes: %w", err)
		}
		// A run whose holder moved itself last is done once every node
		// runs its target: its lock is replaced by its result.
		go made.Settle(ctx)
		return made, nil
	default:
		return off.New(offInstructions(cfg)), nil
	}
}

// offInstructions are the commands an operator runs instead of an upgrade
// from the console, for this node's kind of deployment: the binary it
// runs, the chart, or the image in another engine.
func offInstructions(cfg serverConfig) []string {
	switch {
	case buildinfo.Packaging() != buildinfo.PackagingImage:
		return binary.Instructions(cfg.executable, runtime.GOOS, runtime.GOARCH)
	case kubernetes.APIBaseFromEnv(os.LookupEnv) != "":
		return kubernetes.Instructions()
	default:
		return off.ImageInstructions()
	}
}

// upgradeOrder returns the nodes of the cluster in the order an upgrade
// moves them: those that are no voters of the metadata group first, then
// the voters, and this node last, each group by ID.
func upgradeOrder(ctx context.Context, view ports.ClusterView, self string) ([]string, error) {
	nodes, err := view.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	rank := func(node cluster.Node) int {
		switch {
		case node.ID == self:
			return 2
		case node.Roles.Has(cluster.RoleMeta):
			return 1
		default:
			return 0
		}
	}
	slices.SortStableFunc(nodes, func(a, b cluster.Node) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	ids := make([]string, len(nodes))
	for i, node := range nodes {
		ids[i] = node.ID
	}
	return ids, nil
}

// nodeBuild returns the build a node runs: this node's as it says it of
// itself, another's asked over node RPC.
func (n *node) nodeBuild(ctx context.Context, id string, others ports.NodeSettingsReader) (cluster.Build, error) {
	if id == n.id {
		settings, err := n.local.Local(ctx)
		if err != nil {
			return cluster.Build{}, err
		}
		return settings.Build, nil
	}
	if others == nil {
		return cluster.Build{}, fmt.Errorf("%w: node %s cannot be asked: this node is on its own", domain.ErrUnavailable, id)
	}
	settings, err := others.NodeSettings(ctx, id)
	if err != nil {
		return cluster.Build{}, err
	}
	return settings.Build, nil
}

// ready reports whether this node is ready, as /readyz does; unavailable
// until the server has given the node its readiness.
func (n *node) ready(ctx context.Context) error {
	health := n.readiness.Load()
	if health == nil {
		return fmt.Errorf("%w: the node is starting", domain.ErrUnavailable)
	}
	if readiness := health.Readiness(ctx); !readiness.Ready() {
		var errs []error
		for _, check := range readiness.Failed() {
			errs = append(errs, check.Err)
		}
		return fmt.Errorf("%w: %w", domain.ErrUnavailable, errors.Join(errs...))
	}
	return nil
}

// readSettingFile reads the file a setting names, of at most
// maxSettingFile bytes; nil when the setting is empty.
func readSettingFile(setting, path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", setting, path, err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", setting, path, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxSettingFile+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", setting, path, err)
	}
	if len(data) > maxSettingFile {
		return nil, fmt.Errorf("%s %s: larger than %d bytes", setting, path, maxSettingFile)
	}
	return data, nil
}
