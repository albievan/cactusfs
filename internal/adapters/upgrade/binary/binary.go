// Package binary is the upgrader of upgrade-mode binary (ADR-0029): it
// replaces the running executable of a deployment of one node with the
// binary of a verified release, and has the node restart into it.
//
// It installs only what the use case verified: a release.Verified of kind
// binary names the asset and its SHA-256, read from the release's signed
// SHA256SUMS. The binary is fetched again through ports.ReleaseSource into
// a file of a generated name beside the executable (os.CreateTemp, never a
// name taken from the release), hashed as it is written, and kept only
// when its SHA-256 is the verified one. It is then given the executable's
// mode, fsynced with its directory, and run once - "<new> version" must
// name the target - before it is moved into place:
//
//   - Unix: the old executable is hard-linked (or copied) to
//     cactusfs.previous, and the new one renamed over the executable in one
//     step, so the executable's path never names anything but a whole
//     binary, old or new. The node is then asked to restart (Config.Restart):
//     it stops gracefully and executes the new binary at the same path, with
//     the same arguments and environment.
//   - Windows: a running executable cannot be replaced, but it can be
//     renamed: it is renamed to cactusfs.previous.exe and the new one
//     renamed into its place. The process cannot execute itself again; the
//     state is restart-required, and whoever runs the service restarts it.
//
// A crash at any step leaves the executable's path with a whole binary,
// and at worst a temporary file, which New removes. What happens after the
// restart is not remembered: the progress lives in the memory of the process
// that did the work. The node that comes back up reports StateIdle and the
// target as its version, which is how the console reads an upgrade that
// ended in a restart.
package binary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// The bounds of an upgrade.
const (
	// MaxBinary is the largest binary fetched (the amendment of ADR-0023 by
	// ADR-0029).
	MaxBinary = 256 << 20
	// CheckTimeout bounds the run of "<new> version".
	CheckTimeout = 10 * time.Second
	// RunTimeout bounds a whole upgrade, from the fetch to the swap.
	RunTimeout = 30 * time.Minute
)

// The names of files in the executable's directory.
const (
	// PreviousName is the name the replaced executable is kept under, with
	// ".exe" on Windows.
	PreviousName = "cactusfs.previous"
	// tempPrefix begins the name of every file the upgrader makes beside
	// the executable before it is moved into place. Any file of that name
	// is the upgrader's, left behind by a crash, and New removes it.
	tempPrefix = ".cactusfs-upgrade-"
)

// Config is what an Upgrader needs.
type Config struct {
	// Source is where the binary is fetched from: the update source.
	Source ports.ReleaseSource
	// Executable is the absolute path of the running executable, with
	// symbolic links resolved (filepath.EvalSymlinks(os.Executable())),
	// read once when the process started: the path that is replaced, and
	// the one the node executes again.
	Executable string
	// Nodes returns how many nodes the deployment has. A binary is
	// replaced only when it is the deployment's one node.
	Nodes func(ctx context.Context) int
	// Packaging is how the running binary was shipped
	// (buildinfo.Packaging): only a buildinfo.PackagingBinary is replaced.
	Packaging string
	// GOOS and GOARCH name the platform whose binary is installed
	// (release.BinaryAsset): runtime.GOOS and runtime.GOARCH.
	GOOS, GOARCH string
	// Restart asks the node to stop gracefully and execute the executable
	// again; nil when it cannot (Windows), and the state is then
	// restart-required.
	Restart func()
	// NodeID is the node's ID, as its progress names it; empty for none.
	NodeID string
	// Build is the build that runs now, as the node's progress says until
	// the upgrade ends.
	Build buildinfo.Info
	// Clock tells the time of the progress.
	Clock ports.Clock
	// Logger is the node's log.
	Logger *slog.Logger
	// Lifetime is the node's own context, which ends when the node stops:
	// a run that has not begun to move the new binary into place ends
	// with it, and fails with the executable as it was. A swap that has
	// begun completes, so that the executable stays whole. Nil: the
	// context given to New.
	Lifetime context.Context
}

// Upgrader is the ports.Upgrader of upgrade-mode binary.
type Upgrader struct {
	cfg      Config
	dir      string
	previous string
	ext      string
	// reexec is whether the node executes the new binary itself.
	reexec bool
	// replace moves the checked binary at temp into the executable's
	// place and keeps the old one at previous: replaceByLink on Unix,
	// replaceAside on Windows.
	replace func(exe, previous, temp string, stage func(string) error) error
	// stage, when not nil, is called after each step of a run with its
	// name; an error stops the run there. Tests read the files at each
	// step through it, as a crash there would leave them.
	stage func(name string) error
	// checkTimeout bounds "<new> version".
	checkTimeout time.Duration

	mu       sync.Mutex
	progress release.Progress
	// replaced is the release the executable was replaced with by this
	// process; zero until then.
	replaced release.Version
}

var _ ports.Upgrader = (*Upgrader)(nil)

// New returns the upgrader of cfg, and removes the files a run that crashed
// left beside the executable, under ctx. The executable must be an absolute
// path.
func New(ctx context.Context, cfg Config) (*Upgrader, error) {
	switch {
	case cfg.Source == nil || cfg.Nodes == nil || cfg.Clock == nil || cfg.Logger == nil:
		return nil, errors.New("binary.New: a source, the count of nodes, a clock and a logger are required")
	case cfg.Executable == "" || !filepath.IsAbs(cfg.Executable):
		return nil, fmt.Errorf("binary.New: %w: the executable must be an absolute path", domain.ErrInvalidArgument)
	}
	u := &Upgrader{
		cfg:          cfg,
		dir:          filepath.Dir(cfg.Executable),
		reexec:       runtime.GOOS != "windows" && cfg.Restart != nil,
		replace:      replaceByLink,
		checkTimeout: CheckTimeout,
	}
	if runtime.GOOS == "windows" {
		u.replace = replaceAside
	}
	if strings.EqualFold(filepath.Ext(cfg.Executable), ".exe") || cfg.GOOS == "windows" {
		u.ext = ".exe"
	}
	u.previous = filepath.Join(u.dir, PreviousName+u.ext)
	if u.cfg.Lifetime == nil {
		u.cfg.Lifetime = ctx
	}
	u.removeLeftovers(ctx)
	return u, nil
}

// removeLeftovers removes the files a run that crashed left beside the
// executable. A directory that cannot be read has none it could remove.
func (u *Upgrader) removeLeftovers(ctx context.Context) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), tempPrefix) || entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(u.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			u.cfg.Logger.WarnContext(ctx, "a file a past upgrade left behind could not be removed",
				slog.String("file", entry.Name()), slog.String("error", err.Error()))
			continue
		}
		u.cfg.Logger.InfoContext(ctx, "removed a file a past upgrade left behind", slog.String("file", entry.Name()))
	}
}

// Mode implements ports.Upgrader: release.ModeBinary.
func (u *Upgrader) Mode() release.Mode { return release.ModeBinary }

// Availability implements ports.Upgrader. An upgrade can be started when
// the binary was shipped as one, the deployment has one node, the
// executable's directory can be written to, a release is built for the
// platform, and the executable was not already replaced by this process.
// The instructions are the commands that do the same by hand.
func (u *Upgrader) Availability(ctx context.Context) (ok bool, reason string, instructions []string) {
	instructions = u.instructions()
	if u.cfg.Packaging != buildinfo.PackagingBinary {
		return false, "this binary is the one of a container image: move the deployment to the release's image instead", instructions
	}
	if release.BinaryAsset(u.cfg.GOOS, u.cfg.GOARCH) == "" {
		return false, "no release is built for " + u.cfg.GOOS + "/" + u.cfg.GOARCH, instructions
	}
	if n := u.cfg.Nodes(ctx); n != 1 {
		return false, fmt.Sprintf("the deployment has %d nodes: a binary is replaced from the console only on a deployment of one, and the nodes of a cluster are upgraded one at a time by hand", n), instructions
	}
	u.mu.Lock()
	replaced := u.replaced
	u.mu.Unlock()
	if !replaced.IsZero() {
		return false, "the executable was replaced with " + replaced.String() + ": restart the node to run it", instructions
	}
	if err := u.probe(); err != nil {
		u.cfg.Logger.InfoContext(ctx, "the directory of the executable cannot be written to; upgrades from the console are not available",
			slog.String("error", err.Error()))
		return false, "the directory of the executable cannot be written to by the node", instructions
	}
	return true, "", instructions
}

// probe makes and removes a file beside the executable.
func (u *Upgrader) probe() error {
	f, err := os.CreateTemp(u.dir, tempPrefix+"probe-*")
	if err != nil {
		return fmt.Errorf("binary.probe: %w", err)
	}
	name := f.Name()
	closeErr := f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("binary.probe: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("binary.probe: %w", closeErr)
	}
	return nil
}

// Start implements ports.Upgrader. It refuses what it cannot install
// before anything is begun; what goes wrong after - the release lists no
// such binary, the binary fetched is not the verified one, it does not run
// or names another version, the files cannot be moved - ends the run in
// release.StateFailed, with the old executable in place.
func (u *Upgrader) Start(ctx context.Context, target release.Verified, startedBy string, report func(release.Progress)) error {
	if ok, reason, _ := u.Availability(ctx); !ok {
		return fmt.Errorf("binary.Start: %w: %s", domain.ErrUpgradeUnavailable, reason)
	}
	if target.IsZero() {
		return fmt.Errorf("binary.Start: %w: nothing verified to install", domain.ErrReleaseNotVerified)
	}
	if target.Kind() != release.KindBinary {
		return fmt.Errorf("binary.Start: %w: the binary upgrader does not install a %s", domain.ErrInvalidArgument, target.Kind())
	}
	if want := release.BinaryAsset(u.cfg.GOOS, u.cfg.GOARCH); target.Asset() != want {
		return fmt.Errorf("binary.Start: %w: %s is not the binary of %s/%s", domain.ErrInvalidArgument, target.Asset(), u.cfg.GOOS, u.cfg.GOARCH)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("binary.Start: %w", err)
	}
	u.mu.Lock()
	if u.progress.State.Active() {
		u.mu.Unlock()
		return fmt.Errorf("binary.Start: %w", domain.ErrUpgradeRunning)
	}
	u.progress = release.Progress{
		State: release.StateVerifying, Target: target.Version(), StartedBy: startedBy, StartedAt: u.cfg.Clock.Now(),
	}
	if u.cfg.NodeID != "" {
		u.progress.Nodes = []release.NodeProgress{{
			ID: u.cfg.NodeID, Version: u.cfg.Build.Version, Build: u.cfg.Build.Build, State: release.NodePending,
		}}
	}
	first := u.progress.Normalized()
	u.mu.Unlock()

	// The run outlives the request that started it, not the node: it ends
	// when the node stops, until the swap has begun (swapping).
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RunTimeout)
	swapping, stop := u.boundByLifetime(runCtx, cancel)
	u.cfg.Logger.InfoContext(ctx, "upgrade started", slog.String("to", target.Version().String()),
		slog.String("binary", target.Asset()), slog.String("digest", target.Digest()), slog.String("by", startedBy))
	go func() {
		defer cancel()
		defer stop()
		u.run(runCtx, target, first, report, swapping)
	}()
	return nil
}

// boundByLifetime cancels a run's context, runCtx, when the node's lifetime
// ends - until swapping is called. swapping reports whether the swap may
// begin: false when runCtx has ended (the node stopped, or the run's time
// is up); once it has said true, the node's stopping no longer cancels the
// run, so that a swap that has begun completes. stop ends the watch.
func (u *Upgrader) boundByLifetime(runCtx context.Context, cancel context.CancelFunc) (swapping func() bool, stop func()) {
	var mu sync.Mutex
	detached := false
	done := make(chan struct{})
	lifetime := u.cfg.Lifetime.Done()
	go func() {
		select {
		case <-lifetime:
			mu.Lock()
			defer mu.Unlock()
			if !detached {
				cancel()
			}
		case <-done:
		case <-runCtx.Done():
		}
	}()
	swapping = func() bool {
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-lifetime:
			// Ended, whether or not the watch has seen it yet.
			cancel()
			return false
		default:
		}
		if runCtx.Err() != nil {
			return false
		}
		detached = true
		return true
	}
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }
	return swapping, stop
}

// Progress implements ports.Upgrader.
func (u *Upgrader) Progress(ctx context.Context) (release.Progress, error) {
	if err := ctx.Err(); err != nil {
		return release.Progress{}, fmt.Errorf("binary.Progress: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.progress.Normalized(), nil
}

// failure is why a run failed: what the progress says, in words that name
// no secret, and the error for the log.
type failure struct {
	reason string
	err    error
}

// run is an upgrade, after Start: install, then restart or say it must be.
//
// swapping is called once before the new binary is moved into place: false
// when the node has stopped, or the run's time is up, and nothing is moved;
// once it says true, the node's stopping no longer ends the run.
func (u *Upgrader) run(ctx context.Context, target release.Verified, first release.Progress, report func(release.Progress), swapping func() bool) {
	call := func(p release.Progress) {
		if report != nil {
			report(p)
		}
	}
	call(first)
	running := func() { call(u.set(release.StateRunning, release.NodeUpgrading, "", nil)) }
	if f := u.install(ctx, target, running, swapping); f != nil {
		u.cfg.Logger.ErrorContext(ctx, "upgrade failed; the executable was left as it was",
			slog.String("to", target.Version().String()), slog.String("reason", f.reason), slog.String("error", f.err.Error()))
		call(u.set(release.StateFailed, release.NodeFailed, f.reason, nil))
		return
	}
	if !u.reexec {
		u.cfg.Logger.InfoContext(ctx, "upgrade installed; the node runs the new binary once it is restarted",
			slog.String("to", target.Version().String()), slog.String("previous", u.previous))
		call(u.set(release.StateRestartRequired, release.NodeUpgrading, "", nil))
		return
	}
	// The executable is the target's: the process that answers next runs
	// it. Done is said before the restart, by the only process that knows.
	u.cfg.Logger.InfoContext(ctx, "upgrade installed; the node restarts into the new binary",
		slog.String("to", target.Version().String()), slog.String("previous", u.previous))
	call(u.set(release.StateDone, release.NodeDone, "", func(n *release.NodeProgress) {
		n.Version, n.Build = target.Version().String(), ""
	}))
	u.cfg.Restart()
}

// set moves the progress to state, every node to node with reason, changes
// each node further with change when given, and returns a copy.
func (u *Upgrader) set(state release.State, node release.NodeState, reason string, change func(*release.NodeProgress)) release.Progress {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.progress.State = state
	for i := range u.progress.Nodes {
		u.progress.Nodes[i].State, u.progress.Nodes[i].Reason = node, reason
		if change != nil {
			change(&u.progress.Nodes[i])
		}
	}
	return u.progress.Normalized()
}

// markReplaced records that the executable is now version's.
func (u *Upgrader) markReplaced(version release.Version) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.replaced = version
}

// at calls the test's hook of a step, when there is one.
func (u *Upgrader) at(name string) error {
	if u.stage == nil {
		return nil
	}
	return u.stage(name)
}
