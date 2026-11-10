// Package kubernetes is the upgrader of ADR-0029's mode "kubernetes": it
// moves the nodes of a deployment made by the chart to a verified release's
// image, one node at a time and the node it runs on last, through the
// in-cluster Kubernetes API, as a ports.Upgrader.
//
// It speaks to the API with plain net/http - no client library - with the
// token, the CA and the namespace of a projected volume that the chart
// mounts only when upgrade.fromConsole.enabled says so (DefaultAPIDir).
// The token is read again for every request; the CA is the only root the
// API's certificate is verified against; no proxy is used. What it may do
// is what the chart's Role grants: get and patch the StatefulSets of the
// nodes, get their pods, and evict them, each by name.
//
// A run, for each node in order: patch the StatefulSet's container image
// to <the repository it runs>@<the digest of the verified manifest list>
// and the pod template's app.kubernetes.io/version, keeping what it ran in
// PreviousAnnotation; evict its pod through the Eviction API, so that the
// PodDisruptionBudget is respected (a 429 is asked again until the node's
// time is up); wait until the new pod is Ready on the new image, the node
// reports the target's version, and the cluster is ready. The first node
// that does not is put back to what it ran and evicted again, and the run
// stops: nothing is patched after it.
//
// Nothing of a run is kept in this process that another node needs: the
// run is held by LockAnnotation on the StatefulSet of the first node by
// name, written with the StatefulSet's resourceVersion and kept alive by a
// heartbeat; its end is ResultAnnotation; how far it has come is read from
// the StatefulSets, the pods and each node's build. So any node answers
// Progress, a run whose holder was evicted - itself, last - is seen to be
// done once its pod is up, and a run that was cut off is resumed by
// starting it again: a node already at the target is skipped. The lock of
// a run seen done is replaced by its result "done" under the
// resourceVersion - by the restarted holder (Settle as it starts, or
// Progress), or by any node once the lock is stale.
package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// The defaults of Config.
const (
	// DefaultAPIDir is where the chart mounts the projected volume of
	// the token, the CA of the API (ca.crt) and the namespace.
	DefaultAPIDir = "/var/run/secrets/cactusfs.io/kubernetes"
	// DefaultContainer is the name of the node's container in the chart.
	DefaultContainer = "cactusfs"
	// DefaultNodeTimeout is how long a node is given to be evicted and
	// come up on the new image.
	DefaultNodeTimeout = 10 * time.Minute
	// DefaultPoll is how often a node is looked at while it comes up.
	DefaultPoll = 5 * time.Second
)

// askTimeout bounds asking a node for its build while reading the progress.
const askTimeout = 10 * time.Second

// Config is what an Upgrader needs.
type Config struct {
	// APIBase is the address of the in-cluster API, https://<host>:<port>
	// (APIBaseFromEnv). Empty: the upgrader is not available.
	APIBase string
	// TokenDir holds the files token, ca.crt and namespace;
	// DefaultAPIDir when empty.
	TokenDir string
	// HTTPClient, when not nil, is used instead of a client that trusts
	// the ca.crt of TokenDir alone. For tests.
	HTTPClient *http.Client
	// Namespace, when not empty, is used instead of the namespace of
	// TokenDir.
	Namespace string
	// Nodes returns the IDs of the nodes of the deployment - each the name
	// of its StatefulSet, whose pod is <ID>-0 - in the order they are to
	// be moved, read from the topology when it is called. Required.
	Nodes func(ctx context.Context) ([]string, error)
	// Self is the ID of the node this process is; it is moved last.
	// Required.
	Self string
	// Container is the name of the node's container; DefaultContainer
	// when empty.
	Container string
	// NodeBuild asks a node for its build (the settings RPC). Required.
	NodeBuild func(ctx context.Context, nodeID string) (cluster.Build, error)
	// Ready reports whether the cluster is ready (its /readyz): nil when
	// it is. Required.
	Ready func(ctx context.Context) error
	// NodeTimeout is how long a node is given; DefaultNodeTimeout when
	// zero. A lock whose heartbeat is older than twice this is taken over.
	NodeTimeout time.Duration
	// Poll is how often a node is looked at; DefaultPoll when zero.
	Poll time.Duration
	// Clock tells the time of the lock's heartbeat; the system's when nil.
	Clock ports.Clock
	// Logger receives what the upgrader does; nothing when nil.
	Logger *slog.Logger
}

// systemClock is the clock of the system.
type systemClock struct{}

// Now implements ports.Clock.
func (systemClock) Now() time.Time { return time.Now() }

// Upgrader is the ports.Upgrader of the mode kubernetes.
type Upgrader struct {
	cfg Config
	api *api
	log *slog.Logger

	// mu guards running: whether a run of this process is under way.
	mu      sync.Mutex
	running bool
}

var _ ports.Upgrader = (*Upgrader)(nil)

// New returns an Upgrader of cfg. It refuses a Config without Nodes,
// NodeBuild, Ready or Self, a Self that cannot name a StatefulSet, and an
// APIBase that is not https://<host>:<port>; it reads no file and asks
// nobody.
func New(cfg Config) (*Upgrader, error) {
	switch {
	case cfg.Nodes == nil || cfg.NodeBuild == nil || cfg.Ready == nil:
		return nil, fmt.Errorf("kubernetes.New: %w: Nodes, NodeBuild and Ready are required", domain.ErrInvalidArgument)
	case !validName(cfg.Self):
		return nil, fmt.Errorf("kubernetes.New: %w: Self is not the name of a StatefulSet", domain.ErrInvalidArgument)
	case cfg.Namespace != "" && (len(cfg.Namespace) > 63 || !validName(cfg.Namespace)):
		return nil, fmt.Errorf("kubernetes.New: %w: Namespace is not a namespace's name", domain.ErrInvalidArgument)
	case cfg.NodeTimeout < 0 || cfg.Poll < 0:
		return nil, fmt.Errorf("kubernetes.New: %w: a negative duration", domain.ErrInvalidArgument)
	}
	if cfg.APIBase != "" {
		if err := checkAPIBase(cfg.APIBase); err != nil {
			return nil, fmt.Errorf("kubernetes.New: %w: %w", domain.ErrInvalidArgument, err)
		}
	}
	if cfg.TokenDir == "" {
		cfg.TokenDir = DefaultAPIDir
	}
	if cfg.Container == "" {
		cfg.Container = DefaultContainer
	}
	if cfg.NodeTimeout == 0 {
		cfg.NodeTimeout = DefaultNodeTimeout
	}
	if cfg.Poll == 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Upgrader{
		cfg: cfg,
		api: &api{base: cfg.APIBase, tokenDir: cfg.TokenDir, fixed: cfg.HTTPClient},
		log: logger.With("component", "upgrade", "mode", string(release.ModeKubernetes)),
	}, nil
}

// Mode implements ports.Upgrader.
func (u *Upgrader) Mode() release.Mode { return release.ModeKubernetes }

// instructions are what an operator does instead of, or after, an upgrade
// from the console.
func instructions() []string { return Instructions() }

// Instructions are what an operator of a deployment made by the chart does
// to upgrade it without the console, or after an upgrade from it: the helm
// upgrade that names the release's image by digest, and how to turn
// upgrades from the console on. An upgrader of another mode - upgrade-mode
// off - shows them in a Kubernetes cluster.
func Instructions() []string {
	return []string{
		"helm upgrade <release> oci://ghcr.io/albievan/charts/cactusfs --version <version> -n <namespace> --reuse-values --set image.digest=<the digest of the image list, from the release's digests.txt>",
		"To check for releases, set the update source through the chart's extraEnv - --set 'extraEnv[0].name=CACTUSFS_UPDATE_SOURCE' --set 'extraEnv[0].value=https://api.github.com/repos/albievan/cactusfs/releases' (or a mirror's address) - and let the pods reach it with a rule of networkPolicy.egressTo: --set 'networkPolicy.egressTo[0].to[0].ipBlock.cidr=<its address>/32' --set 'networkPolicy.egressTo[0].ports[0]=443' (an index past those egressTo already holds, such as Vault's).",
		"To upgrade from the console, give the chart --set upgrade.fromConsole.enabled=true and the addresses of the Kubernetes API server with --set upgrade.fromConsole.apiServer.cidrs={<address>/32}; the chart then sets CACTUSFS_UPGRADE_MODE=kubernetes. Upgrading needs the update source as well.",
		"After an upgrade from the console, the next helm upgrade must name the installed digest or a newer one (--set image.digest=...), or it puts the nodes' StatefulSets back to the image Helm knows.",
	}
}

// namespace returns the namespace to work in.
func (u *Upgrader) namespace() (string, error) {
	if u.cfg.Namespace != "" {
		return u.cfg.Namespace, nil
	}
	return u.api.namespace()
}

// nodes returns the nodes in the order they are moved - as Nodes gives
// them, with holder last - and the node whose StatefulSet holds the lock:
// the first by name, so that every node agrees on it.
func (u *Upgrader) nodes(ctx context.Context, holder string) (order []string, lockNode string, err error) {
	ids, err := u.cfg.Nodes(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("the nodes of the cluster are not known: %w", err)
	}
	seen := map[string]bool{}
	var last []string
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !validName(id) || len(id) > 61 {
			return nil, "", fmt.Errorf("%w: the node %q cannot name a StatefulSet", domain.ErrInvalidArgument, id)
		}
		if id == holder {
			last = append(last, id)
			continue
		}
		order = append(order, id)
	}
	order = append(order, last...)
	if len(order) == 0 {
		return nil, "", errors.New("the cluster has no node")
	}
	return order, slices.Min(order), nil
}

// Availability implements ports.Upgrader: the upgrader can run when the API
// is known, the token directory is there, and the StatefulSet that holds
// the lock can be read and has the node's container.
func (u *Upgrader) Availability(ctx context.Context) (ok bool, reason string, steps []string) {
	steps = instructions()
	if u.cfg.APIBase == "" {
		return false, "the Kubernetes API is not known to this node (KUBERNETES_SERVICE_HOST is not set)", steps
	}
	if _, err := os.Stat(filepath.Join(u.cfg.TokenDir, tokenFile)); err != nil {
		return false, "this node has no token for the Kubernetes API in " + u.cfg.TokenDir + ": the chart's upgrade.fromConsole.enabled is off", steps
	}
	ns, err := u.namespace()
	if err != nil {
		return false, "the namespace of this node is not known", steps
	}
	_, lockNode, err := u.nodes(ctx, u.cfg.Self)
	if err != nil {
		return false, err.Error(), steps
	}
	sts, err := u.api.getStatefulSet(ctx, ns, lockNode)
	if err != nil {
		return false, fmt.Sprintf("the Kubernetes API did not let this node read the StatefulSet %s: %s", lockNode, describe(err)), steps
	}
	if sts.Spec.Template.Spec.image(u.cfg.Container) == "" {
		return false, fmt.Sprintf("the StatefulSet %s has no container %s", lockNode, u.cfg.Container), steps
	}
	return true, "", steps
}

// describe says what went wrong with a request, in words that name no
// secret: the status, or that the API could not be reached.
func describe(err error) string {
	var se *StatusError
	if errors.As(err, &se) {
		text := fmt.Sprintf("it answered %d", se.Status)
		if se.Reason != "" {
			text += " " + se.Reason
		}
		return text
	}
	if errors.Is(err, domain.ErrUnavailable) {
		return "it could not be reached"
	}
	return "the request could not be made"
}

// Start implements ports.Upgrader.
func (u *Upgrader) Start(ctx context.Context, target release.Verified, startedBy string, report func(release.Progress)) error {
	if target.IsZero() {
		return fmt.Errorf("kubernetes.Start: %w: nothing verified to install", domain.ErrReleaseNotVerified)
	}
	if target.Kind() != release.KindImage {
		return fmt.Errorf("kubernetes.Start: %w: the kubernetes upgrader installs an image, not a %s", domain.ErrInvalidArgument, target.Kind())
	}
	digest := target.Digest()
	label := versionLabel(target.Version())
	if !digestPattern.MatchString(digest) || label == "" {
		return fmt.Errorf("kubernetes.Start: %w: the image's digest or version cannot be written", domain.ErrInvalidArgument)
	}
	if ok, reason, _ := u.Availability(ctx); !ok {
		return fmt.Errorf("kubernetes.Start: %w: %s", domain.ErrUpgradeUnavailable, reason)
	}
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return fmt.Errorf("kubernetes.Start: %w", domain.ErrUpgradeRunning)
	}
	u.running = true
	u.mu.Unlock()
	r, err := u.begin(ctx, target, digest, startedBy, report)
	if err != nil {
		u.stopped()
		return fmt.Errorf("kubernetes.Start: %w", err)
	}
	u.log.InfoContext(ctx, "upgrade started", "target", r.lock.Target, "image", r.lock.Image, "run", r.lock.Run, "nodes", len(r.order))
	go r.execute(context.WithoutCancel(ctx))
	return nil
}

// stopped records that the run of this process has ended.
func (u *Upgrader) stopped() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.running = false
}

// begin takes the lock and returns the run, not yet under way.
func (u *Upgrader) begin(ctx context.Context, target release.Verified, digest, startedBy string, report func(release.Progress)) (*run, error) {
	ns, err := u.namespace()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrUpgradeUnavailable, err)
	}
	order, lockNode, err := u.nodes(ctx, u.cfg.Self)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrUpgradeUnavailable, err)
	}
	sts, err := u.api.getStatefulSet(ctx, ns, lockNode)
	if err != nil {
		return nil, err
	}
	current := sts.Spec.Template.Spec.image(u.cfg.Container)
	if current == "" {
		return nil, fmt.Errorf("%w: the StatefulSet %s has no container %s", domain.ErrUpgradeUnavailable, lockNode, u.cfg.Container)
	}
	now := u.cfg.Clock.Now().UTC()
	if held, found, perr := parseLock(sts.Metadata.Annotations); found {
		if perr == nil && !u.abandoned(ctx, ns, order, held, now) {
			return nil, fmt.Errorf("%w: run %s of %s holds the lock", domain.ErrUpgradeRunning, held.Run, held.Target)
		}
		u.log.WarnContext(ctx, "upgrade: a lock whose run has ended is taken over", "node", lockNode)
	}
	id, err := runID()
	if err != nil {
		return nil, err
	}
	lock := lockRecord{
		Run:       id,
		Target:    target.Version().String(),
		Image:     repositoryOf(current) + "@" + digest,
		Holder:    u.cfg.Self,
		StartedBy: startedBy,
		StartedAt: now,
		Heartbeat: now,
	}
	err = u.api.patchStatefulSet(ctx, ns, lockNode, mergePatch, map[string]any{
		"metadata": map[string]any{
			"resourceVersion": sts.Metadata.ResourceVersion,
			"annotations":     map[string]any{LockAnnotation: encode(lock)},
		},
	})
	if statusOf(err) == http.StatusConflict {
		return nil, fmt.Errorf("%w: another node took the lock first", domain.ErrUpgradeRunning)
	}
	if err != nil {
		return nil, err
	}
	r := &run{
		u: u, ns: ns, lockNode: lockNode, order: order, lock: lock, target: target.Version(),
		label: versionLabel(target.Version()), report: report, beat: now,
		progress: release.Progress{
			State: release.StateRunning, Target: target.Version(), StartedBy: startedBy, StartedAt: now,
		},
	}
	for _, node := range order {
		r.progress.Nodes = append(r.progress.Nodes, release.NodeProgress{ID: node, State: release.NodePending})
	}
	return r, nil
}

// stale reports whether a lock's holder has not said it was alive for
// twice the time of a node.
func (u *Upgrader) stale(lock lockRecord, now time.Time) bool {
	return now.Sub(lock.Heartbeat) > 2*u.cfg.NodeTimeout
}

// abandoned reports whether a lock that is there no longer holds a run:
// its holder has not said it was alive for twice the time of a node; or
// its holder is this node, whose process holds no run (it was restarted -
// moved last, or cut off); or every node already runs its target, which
// is how the run of a holder that moved itself ends.
func (u *Upgrader) abandoned(ctx context.Context, ns string, order []string, lock lockRecord, now time.Time) bool {
	if u.stale(lock, now) || lock.Holder == u.cfg.Self {
		// Start has marked a run of this process as under way, and
		// refused before coming here if one was.
		return true
	}
	target, err := release.ParseVersion(lock.Target)
	if err != nil {
		return true
	}
	for _, node := range order {
		np, err := u.nodeProgress(ctx, ns, node, lock.Image, target)
		if err != nil || np.State != release.NodeDone {
			return false
		}
	}
	return true
}

// runID returns a new name of a run.
func runID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("a run cannot be named: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Progress implements ports.Upgrader: read from the cluster, so that any
// node gives the same answer. Idle when no run has left its mark, or when
// the upgrader is not available here.
func (u *Upgrader) Progress(ctx context.Context) (release.Progress, error) {
	if err := ctx.Err(); err != nil {
		return release.Progress{}, fmt.Errorf("kubernetes.Progress: %w", err)
	}
	if u.cfg.APIBase == "" {
		return release.Progress{}.Normalized(), nil
	}
	if _, err := os.Stat(filepath.Join(u.cfg.TokenDir, tokenFile)); err != nil {
		return release.Progress{}.Normalized(), nil
	}
	ns, err := u.namespace()
	if err != nil {
		return release.Progress{}, fmt.Errorf("kubernetes.Progress: %w", err)
	}
	_, lockNode, err := u.nodes(ctx, "")
	if err != nil {
		return release.Progress{}, fmt.Errorf("kubernetes.Progress: %w", err)
	}
	sts, err := u.api.getStatefulSet(ctx, ns, lockNode)
	if err != nil {
		return release.Progress{}, fmt.Errorf("kubernetes.Progress: %w", err)
	}
	lock, locked, lockErr := parseLock(sts.Metadata.Annotations)
	result, ended := parseResult(sts.Metadata.Annotations)
	var p release.Progress
	var holder, image string
	switch {
	case locked && lockErr == nil:
		holder, image = lock.Holder, lock.Image
		p = release.Progress{State: release.StateRunning, StartedBy: lock.StartedBy, StartedAt: lock.StartedAt}
		p.Target, _ = release.ParseVersion(lock.Target)
	case ended:
		holder, image = result.Holder, result.Image
		p = release.Progress{State: result.State, StartedBy: result.StartedBy, StartedAt: result.StartedAt}
		p.Target, _ = release.ParseVersion(result.Target)
	default:
		return release.Progress{}.Normalized(), nil
	}
	order, _, err := u.nodes(ctx, holder)
	if err != nil {
		return release.Progress{}, fmt.Errorf("kubernetes.Progress: %w", err)
	}
	done := 0
	for _, node := range order {
		np, err := u.nodeProgress(ctx, ns, node, image, p.Target)
		if err != nil {
			return release.Progress{}, fmt.Errorf("kubernetes.Progress: %w", err)
		}
		if np.State == release.NodeDone {
			done++
		}
		p.Nodes = append(p.Nodes, np)
	}
	switch {
	case locked && done == len(p.Nodes):
		// The holder moved itself last and was stopped by it: its run
		// is done once its pod is up, whatever the lock still says. The
		// lock is replaced by the run's result, so that it is not left
		// behind - by the restarted holder, or once the lock is stale;
		// a holder that is alive ends its run itself.
		p.State = release.StateDone
		u.settle(ctx, ns, lockNode, sts, lock)
	case locked && u.stale(lock, u.cfg.Clock.Now()):
		p.State = release.StateFailed
		for i := range p.Nodes {
			if p.Nodes[i].State != release.NodeDone {
				p.Nodes[i].State = release.NodeFailed
				p.Nodes[i].Reason = "the run stopped: the node that ran it stopped answering; start the upgrade again to resume it"
				break
			}
		}
	case !locked && result.Node != "":
		for i := range p.Nodes {
			if p.Nodes[i].ID == result.Node {
				p.Nodes[i].State, p.Nodes[i].Reason = result.NodeState, result.Reason
			}
		}
	}
	return p.Normalized(), nil
}

// settle replaces lock, read from the StatefulSet sts of lockNode and whose
// every node was seen done, with the run's result "done": one merge patch
// under sts's resourceVersion, so that it changes nothing if the lock moved
// meanwhile. Only a lock whose run no live process holds is replaced: one
// whose holder is this node - restarted, for a run of this process is left
// to end itself - or whose heartbeat is stale. A holder that is alive may
// still be waiting for the cluster to be ready, and ends its run itself. It
// reports whether the lock was replaced; a failure is logged and tried
// again by the next caller.
func (u *Upgrader) settle(ctx context.Context, ns, lockNode string, sts *statefulSet, lock lockRecord) bool {
	u.mu.Lock()
	running := u.running
	u.mu.Unlock()
	if running || (lock.Holder != u.cfg.Self && !u.stale(lock, u.cfg.Clock.Now())) {
		return false
	}
	result := resultRecord{
		Run: lock.Run, Target: lock.Target, Image: lock.Image, Holder: lock.Holder,
		StartedBy: lock.StartedBy, StartedAt: lock.StartedAt, EndedAt: u.cfg.Clock.Now().UTC(),
		State: release.StateDone,
	}
	err := u.api.patchStatefulSet(ctx, ns, lockNode, mergePatch, map[string]any{
		"metadata": map[string]any{
			"resourceVersion": sts.Metadata.ResourceVersion,
			"annotations":     map[string]any{LockAnnotation: nil, ResultAnnotation: encode(result)},
		},
	})
	if err != nil {
		u.log.WarnContext(ctx, "upgrade: the lock of a run that is done could not be replaced by its result", "run", lock.Run, "error", describe(err))
		return false
	}
	u.log.InfoContext(ctx, "upgrade done: every node runs the target; the lock was replaced by the result", "run", lock.Run, "target", lock.Target)
	return true
}

// Settle is called once as the service starts, on every node of the mode
// kubernetes: while a lock is there whose run has every node at its
// target - the run of a holder that was restarted by moving itself last -
// it replaces the lock with the run's result "done", as Progress does, and
// under the same rule: only on the holder's own node, or once the lock is
// stale. It
// looks again every Poll until that is done, there is no lock, or twice a
// node's time has passed (the nodes may still be coming up), and returns
// when ctx ends. It asks nothing when the API is not known here.
func (u *Upgrader) Settle(ctx context.Context) {
	if u.cfg.APIBase == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(u.cfg.TokenDir, tokenFile)); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*u.cfg.NodeTimeout)
	defer cancel()
	for {
		if u.settleOnce(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.cfg.Poll):
		}
	}
}

// settleOnce makes one attempt of Settle: true when there is nothing more
// to do.
func (u *Upgrader) settleOnce(ctx context.Context) bool {
	ns, err := u.namespace()
	if err != nil {
		return true
	}
	_, lockNode, err := u.nodes(ctx, "")
	if err != nil {
		return false
	}
	sts, err := u.api.getStatefulSet(ctx, ns, lockNode)
	if err != nil {
		return false
	}
	lock, locked, perr := parseLock(sts.Metadata.Annotations)
	if !locked || perr != nil {
		return true
	}
	target, err := release.ParseVersion(lock.Target)
	if err != nil {
		return true
	}
	order, _, err := u.nodes(ctx, lock.Holder)
	if err != nil {
		return false
	}
	for _, node := range order {
		np, err := u.nodeProgress(ctx, ns, node, lock.Image, target)
		if err != nil || np.State != release.NodeDone {
			return false
		}
	}
	return u.settle(ctx, ns, lockNode, sts, lock)
}

// nodeProgress reads where a node stands against image and target: done
// when its StatefulSet names image, its pod is Ready on it and the node
// reports target; upgrading when only the StatefulSet names it; pending
// otherwise.
func (u *Upgrader) nodeProgress(ctx context.Context, ns, node, image string, target release.Version) (release.NodeProgress, error) {
	np := release.NodeProgress{ID: node, State: release.NodePending}
	sts, err := u.api.getStatefulSet(ctx, ns, node)
	if err != nil {
		return np, err
	}
	p, err := u.api.getPod(ctx, ns, node+"-0")
	if err != nil {
		return np, err
	}
	askCtx, cancel := context.WithTimeout(ctx, askTimeout)
	build, buildErr := u.cfg.NodeBuild(askCtx, node)
	cancel()
	if buildErr == nil {
		np.Version, np.Build = build.Version, build.Build
	}
	if sts.Spec.Template.Spec.image(u.cfg.Container) != image {
		return np, nil
	}
	np.State = release.NodeUpgrading
	if p.readyWith(u.cfg.Container, image) && buildErr == nil && isVersion(build.Version, target) {
		np.State = release.NodeDone
	}
	return np, nil
}

// isVersion reports whether a version a node reports is target.
func isVersion(reported string, target release.Version) bool {
	v, err := release.ParseVersion(reported)
	return err == nil && v == target
}
