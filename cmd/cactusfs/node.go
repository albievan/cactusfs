package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/adapters/erasurecodec"
	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	metadataStartTimeout = 30 * time.Second
	maxPassphraseFile    = 4 << 10
)

// systemClock is the real clock.
type systemClock struct{}

// Now implements ports.Clock.
func (systemClock) Now() time.Time { return time.Now() }

// node holds the adapters and use cases of a running node. It is assembled
// here, in the composition root, and nowhere else.
type node struct {
	drives []*disk.Drive
	meta   *raftmeta.Store

	buckets    *app.BucketService
	objects    *app.ObjectService
	identities *app.IdentityService
	authorizer *app.Authorizer
	clock      ports.Clock
	telemetry  *telemetry.Telemetry

	scrubber  *app.Scrubber
	collector *app.Collector
	// stopBackground cancels the background tasks; background is done when
	// they have returned.
	stopBackground context.CancelFunc
	background     sync.WaitGroup
}

// openNode opens the drives, the KMS and the metadata store, wires the use
// cases and makes sure the deployment has a root credential. Credentials
// generated for a new deployment are shown once on notice, which is the
// operator's terminal and never the log.
func openNode(
	ctx context.Context, cfg serverConfig, tel *telemetry.Telemetry, logger *slog.Logger, notice io.Writer,
) (*node, error) {
	n := &node{clock: systemClock{}, telemetry: tel}
	if err := n.open(ctx, cfg, logger, notice); err != nil {
		return nil, errors.Join(err, n.Close())
	}
	return n, nil
}

func (n *node) open(ctx context.Context, cfg serverConfig, logger *slog.Logger, notice io.Writer) error {
	stores := make([]ports.ShardStore, 0, len(cfg.Data))
	attached := make([]app.AttachedDrive, 0, len(cfg.Data))
	for _, dir := range cfg.Data {
		drive, err := disk.Open(ctx, dir, rand.Reader, n.clock.Now())
		if err != nil {
			return err
		}
		n.drives = append(n.drives, drive)
		stores = append(stores, n.telemetry.InstrumentShardStore(drive))
		attached = append(attached, drive)
		logger.InfoContext(ctx, "drive opened",
			slog.String("drive_id", drive.DriveID()), slog.String("dir", dir), slog.Bool("empty", drive.Empty()))
	}

	localKMS, err := openKMS(ctx, cfg, logger)
	if err != nil {
		return err
	}
	kms := n.telemetry.InstrumentKMS(localKMS)

	startCtx, cancel := context.WithTimeout(ctx, metadataStartTimeout)
	defer cancel()
	n.meta, err = raftmeta.Open(startCtx, raftmeta.Config{
		Dir: cfg.MetaDir, Logger: logger, Entropy: rand.Reader, Timing: cfg.raftTiming,
	})
	if err != nil {
		return err
	}

	// Every outbound port is reached through a decorator that traces and
	// counts the call.
	meta := n.telemetry.InstrumentMetadata(n.meta)

	topology, err := app.EnsureTopology(ctx, meta, attached, app.TopologyOptions{
		AllowReplacement: cfg.AllowDriveReplacement, Entropy: rand.Reader, Logger: logger,
	})
	if err != nil {
		return err
	}
	params, err := erasure.ParamsFor(topology.SetSize())
	if err != nil {
		return err
	}
	if params.Parity == 0 {
		logger.WarnContext(ctx, "fewer than four drives: objects are stored encrypted and checksummed on one drive each, "+
			"WITHOUT PARITY; the loss of a drive loses the objects on it", slog.Int("drives", len(stores)))
	} else {
		logger.InfoContext(ctx, "erasure coding enabled",
			slog.Int("erasure_sets", len(topology.Sets)), slog.Int("data_shards", params.Data),
			slog.Int("parity_shards", params.Parity), slog.Int("write_quorum", params.WriteQuorum()))
	}
	n.authorizer = app.NewAuthorizer(logger)
	n.buckets = app.NewBucketService(meta, n.authorizer, n.clock)
	n.objects, err = app.NewObjectService(app.ObjectServiceConfig{
		Index: meta, Buckets: meta, Topology: topology, Drives: stores, Codec: erasurecodec.New(), KMS: kms,
		OnHeal: n.telemetry.ObserveHeal,
		Authz:  n.authorizer, Clock: n.clock, Entropy: rand.Reader, Logger: logger,
	})
	if err != nil {
		return err
	}
	n.identities = app.NewIdentityService(meta, kms, n.clock, rand.Reader)
	n.scrubber = app.NewScrubber(app.ScrubberConfig{
		Buckets: meta, Index: meta, Healer: n.objects.Healer(), Logger: logger,
		BytesPerSecond: int64(cfg.ScrubRateMiB) << 20, OnPass: n.telemetry.ObserveScrub,
	})
	n.collector = app.NewCollector(app.CollectorConfig{
		Drives: stores, Index: meta, Clock: n.clock, Grace: cfg.GCGrace, Logger: logger, OnPass: n.telemetry.ObserveGC,
		// Nothing is collected while the metadata cannot be trusted, nor
		// what a write in progress has put on the drives.
		Ready: n.meta.Check, InFlight: n.objects.InFlight,
	})

	var provided *app.Credentials
	if cfg.secrets.rootAccessKey != "" {
		provided = &app.Credentials{AccessKeyID: cfg.secrets.rootAccessKey, SecretKey: cfg.secrets.rootSecretKey}
	}
	generated, err := n.identities.EnsureRoot(ctx, provided)
	if err != nil {
		return err
	}
	if generated != nil {
		logger.InfoContext(ctx, "root credentials generated for a new deployment; they are shown once on standard error")
		showCredentials(notice, *generated)
	}
	return nil
}

// showCredentials prints generated root credentials. This is the only place
// a secret is ever written, and it is written once.
func showCredentials(w io.Writer, creds app.Credentials) {
	_, _ = fmt.Fprintf(w, `
CactusFS generated root credentials for this new deployment.
They are shown once and cannot be recovered. Store them now.

  Access key ID: %s
  Secret key:    %s

To choose the credentials yourself, set %s and %s
before the first start.

`, creds.AccessKeyID, creds.SecretKey, envRootKeyID, envRootKeyValue)
}

// openKMS opens the configured KMS. The key file of the local KMS is created
// only for a deployment that has no metadata yet: once objects exist, a
// missing key file means the wrong path was given, and creating a new key
// there would leave every object unreadable while looking healthy.
func openKMS(ctx context.Context, cfg serverConfig, logger *slog.Logger) (ports.KMS, error) {
	passphrase, err := readPassphrase(cfg)
	if err != nil {
		return nil, err
	}
	defer clear(passphrase)

	exists, err := local.Exists(cfg.KMSKeyFile)
	if err != nil {
		return nil, err
	}
	if !exists {
		started, err := metadataExists(cfg.MetaDir)
		if err != nil {
			return nil, err
		}
		if started {
			return nil, fmt.Errorf("KMS key file %s is missing, but this deployment already has metadata in %s: "+
				"point --kms-key-file at the original key file", cfg.KMSKeyFile, cfg.MetaDir)
		}
		if err := local.Create(cfg.KMSKeyFile, passphrase, local.DefaultParams(), rand.Reader); err != nil {
			return nil, err
		}
		logger.WarnContext(ctx, "created a new local KMS key file; back it up together with its passphrase: "+
			"without both, stored data cannot be decrypted", slog.String("key_file", cfg.KMSKeyFile))
	}
	kms, err := local.Open(cfg.KMSKeyFile, passphrase)
	if err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "KMS opened", slog.String("kms", kmsLocal), slog.String("key_id", kms.KeyID()))
	return kms, nil
}

// metadataExists reports whether the metadata directory has been used.
func metadataExists(dir string) (bool, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect metadata directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	switch _, err := root.Lstat("node.json"); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect metadata directory: %w", err)
	}
}

// readPassphrase returns the KMS passphrase from the environment or from the
// passphrase file. Surrounding whitespace of a file is not part of it.
func readPassphrase(cfg serverConfig) ([]byte, error) {
	if cfg.KMSPassphraseFile == "" {
		return []byte(cfg.secrets.kmsPassphrase), nil
	}
	root, err := os.OpenRoot(filepath.Dir(cfg.KMSPassphraseFile))
	if err != nil {
		return nil, fmt.Errorf("read KMS passphrase file: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(cfg.KMSPassphraseFile))
	if err != nil {
		return nil, fmt.Errorf("read KMS passphrase file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxPassphraseFile+1))
	if err != nil {
		return nil, fmt.Errorf("read KMS passphrase file: %w", err)
	}
	if len(data) > maxPassphraseFile {
		return nil, errors.New("read KMS passphrase file: file is too large to be a passphrase")
	}
	passphrase := bytes.TrimSpace(data)
	if len(passphrase) == 0 {
		return nil, errors.New("read KMS passphrase file: file is empty")
	}
	return passphrase, nil
}

// startBackground starts healing, scrubbing and garbage collection. They run
// until Close.
func (n *node) startBackground(ctx context.Context, cfg serverConfig) {
	ctx, n.stopBackground = context.WithCancel(context.WithoutCancel(ctx))
	n.background.Go(func() { n.objects.Healer().Run(ctx) })
	if cfg.ScrubInterval > 0 {
		n.background.Go(func() { n.scrubber.Run(ctx, cfg.ScrubInterval) })
	}
	if cfg.GCInterval > 0 {
		n.background.Go(func() { n.collector.Run(ctx, cfg.GCInterval) })
	}
}

// Close stops the background tasks, then the metadata store, and releases
// the drives.
func (n *node) Close() error {
	if n.stopBackground != nil {
		n.stopBackground()
		n.background.Wait()
		n.stopBackground = nil
	}
	var errs []error
	if n.meta != nil {
		errs = append(errs, n.meta.Close())
		n.meta = nil
	}
	for _, drive := range n.drives {
		errs = append(errs, drive.Close())
	}
	n.drives = nil
	return errors.Join(errs...)
}

// describe returns the drive IDs for the startup log.
func (n *node) describe() string {
	ids := make([]string, len(n.drives))
	for i, drive := range n.drives {
		ids[i] = drive.DriveID()
	}
	return strings.Join(ids, ",")
}
