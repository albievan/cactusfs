package binary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// wallClock is the time of the machine: the contract compares the start of
// a run with it.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// exeSuffix is what an executable's name ends in here.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// buildFake builds testdata/fakecactusfs, linked to say version or to behave
// as behaviour says, and returns its bytes. What differs from one build to
// the next is told through the environment (GOBIN, GOFLAGS).
func buildFake(t *testing.T, version, behaviour string) []byte {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "go", "install", "-trimpath", "./testdata/fakecactusfs")
	cmd.Env = append(os.Environ(), "GOBIN="+dir, "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH,
		"GOFLAGS=-ldflags=-X=main.fake="+version+"/"+behaviour)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go install of the fake binary: %v\n%s", err, output)
	}
	content, err := contentOf(filepath.Join(dir, "fakecactusfs"+exeSuffix()))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// contentOf reads the file at path, a file of the test's own.
func contentOf(path string) ([]byte, error) {
	return os.ReadFile(filepath.Clean(path))
}

// writeExecutable writes content to path with the mode the test binary
// has: one that may be executed.
func writeExecutable(t *testing.T, path string, content []byte) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
}

// fakes are the binaries of a test.
type fakes struct {
	// old is the executable's binary before the upgrade.
	old []byte
}

// fixture is a deployment of one node whose executable can be replaced.
type fixture struct {
	dir, exe string
	old      []byte
	target   release.Verified
	newBytes []byte
	source   *porttest.ReleaseSource
	cfg      Config
	restarts *counter
}

// counter counts calls of Restart.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *counter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// v071 is the version every test installs.
func v071(t *testing.T) release.Version {
	t.Helper()
	v, err := release.ParseVersion("0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// asset is the name of this platform's binary.
func asset() string { return release.BinaryAsset(runtime.GOOS, runtime.GOARCH) }

// assetURL is where the fake source serves this platform's binary.
func assetURL() string { return "https://releases.example/v0.7.1/" + asset() }

// newFixture puts the old binary at the executable's place in a directory of
// its own, and a source that serves served as release 0.7.1's binary; the
// verified SHA-256 is that of want.
func newFixture(t *testing.T, f fakes, served, want []byte) *fixture {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "cactusfs"+exeSuffix())
	writeExecutable(t, exe, f.old)
	sum := sha256.Sum256(want)
	sums := fmt.Appendf(nil, "%x  %s\n", sum, asset())
	target, err := release.NewVerifiedBinary(v071(t), sums, asset(), sum)
	if err != nil {
		t.Fatal(err)
	}
	source := porttest.NewReleaseSource([]release.Release{{
		Version: v071(t), Assets: []release.Asset{{Name: asset(), URL: assetURL()}},
	}}, map[string][]byte{assetURL(): served})
	restarts := &counter{}
	return &fixture{
		dir: dir, exe: exe, old: f.old, target: target, newBytes: want, source: source, restarts: restarts,
		cfg: Config{
			Source: source, Executable: exe, Nodes: func(context.Context) int { return 1 },
			Packaging: buildinfo.PackagingBinary, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			Restart: restarts.add, NodeID: "node-1", Build: buildinfo.Info{Version: "0.7.0", Build: "1"},
			Clock: wallClock{}, Logger: slog.New(slog.DiscardHandler),
		},
	}
}

// open returns the upgrader of the fixture.
func (f *fixture) open(t *testing.T) *Upgrader {
	t.Helper()
	u, err := New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// runToEnd starts an upgrade of the fixture's target and returns every
// progress reported, the last of which has ended.
func runToEnd(t *testing.T, u *Upgrader, target release.Verified) []release.Progress {
	t.Helper()
	var mu sync.Mutex
	var reports []release.Progress
	ended := make(chan struct{})
	err := u.Start(t.Context(), target, "user/alice", func(p release.Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, p)
		if p.State.Ended() {
			close(ended)
		}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(time.Minute):
		t.Fatal("the upgrade did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(reports)
}

// versionOf runs "<path> version" and returns its second word.
func versionOf(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), path, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", filepath.Base(path), err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		t.Fatalf("%s version said %q", filepath.Base(path), out)
	}
	return fields[1]
}

// leftovers returns the names of the upgrader's files in dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestNew(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "cactusfs")
	base := Config{
		Source: porttest.NewReleaseSource(nil, nil), Executable: exe, Nodes: func(context.Context) int { return 1 },
		Packaging: buildinfo.PackagingBinary, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Clock: wallClock{}, Logger: slog.New(slog.DiscardHandler),
	}
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"no source", func(c *Config) { c.Source = nil }},
		{"no count of nodes", func(c *Config) { c.Nodes = nil }},
		{"no clock", func(c *Config) { c.Clock = nil }},
		{"no logger", func(c *Config) { c.Logger = nil }},
		{"no executable", func(c *Config) { c.Executable = "" }},
		{"a relative executable", func(c *Config) { c.Executable = "cactusfs" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			tt.change(&cfg)
			if _, err := New(t.Context(), cfg); err == nil {
				t.Error("New = nil error")
			}
		})
	}

	t.Run("a crash's files are removed, and nothing else", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := base
		cfg.Executable = filepath.Join(dir, "cactusfs")
		keep := []string{"cactusfs", PreviousName, "data", "cactusfs-upgrade-not-ours"}
		gone := []string{tempPrefix + "123456", tempPrefix + "probe-9", tempPrefix + "77.exe"}
		for _, name := range slices.Concat(keep, gone) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(dir, tempPrefix+"a-directory"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := New(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
		for _, name := range keep {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		for _, name := range gone {
			if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was left: %v", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, tempPrefix+"a-directory")); err != nil {
			t.Errorf("a directory was removed: %v", err)
		}
	})

	t.Run("a directory that cannot be read is no reason not to start", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.Executable = filepath.Join(t.TempDir(), "missing", "cactusfs")
		if _, err := New(t.Context(), cfg); err != nil {
			t.Errorf("New: %v", err)
		}
	})
}

func TestMode(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakes{old: []byte("old")}, []byte("new"), []byte("new"))
	if got := f.open(t).Mode(); got != release.ModeBinary {
		t.Errorf("Mode = %s", got)
	}
}

// Every reason the console cannot upgrade: Availability says no and why,
// with the commands to do it by hand, and Start refuses with nothing begun.
func TestUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Config)
		reason string
	}{
		{"the binary of an image", func(c *Config) { c.Packaging = buildinfo.PackagingImage }, "container image"},
		{"packaging not known", func(c *Config) { c.Packaging = "" }, "container image"},
		{"two nodes", func(c *Config) { c.Nodes = func(context.Context) int { return 2 } }, "2 nodes"},
		{"no node", func(c *Config) { c.Nodes = func(context.Context) int { return 0 } }, "0 nodes"},
		{"a directory that does not exist", func(c *Config) {
			c.Executable = filepath.Join(filepath.Dir(c.Executable), "missing", "cactusfs")
		}, "cannot be written"},
		{"a directory that is a file", func(c *Config) {
			c.Executable = filepath.Join(c.Executable, "cactusfs")
		}, "cannot be written"},
		{"a platform no release is built for", func(c *Config) { c.GOOS, c.GOARCH = "plan9", "386" }, "plan9/386"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakes{old: []byte("old")}, []byte("new"), []byte("new"))
			tt.change(&f.cfg)
			u := f.open(t)
			ok, reason, instructions := u.Availability(t.Context())
			if ok || !strings.Contains(reason, tt.reason) {
				t.Errorf("Availability = %v, %q; want no, saying %q", ok, reason, tt.reason)
			}
			if len(instructions) == 0 {
				t.Error("no instructions")
			}
			porttest.UnavailableUpgraderContract(t, u, f.target)
			if n := f.source.Fetches(assetURL()); n != 0 {
				t.Errorf("the binary was fetched %d times", n)
			}
			if f.restarts.count() != 0 {
				t.Error("the node was restarted")
			}
		})
	}
}

func TestStartRefusesWhatItDoesNotInstall(t *testing.T) {
	t.Parallel()
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	otherName := release.BinaryAsset(other, runtime.GOARCH)
	otherSum := sha256.Sum256([]byte("other"))
	otherPlatform, err := release.NewVerifiedBinary(v071(t), fmt.Appendf(nil, "%x  %s\n", otherSum, otherName), otherName, otherSum)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name   string
		ctx    context.Context
		target release.Verified
		want   error
	}{
		{"nothing verified", t.Context(), release.Verified{}, domain.ErrReleaseNotVerified},
		{"an image", t.Context(), porttest.FakeVerifiedImage(t, "0.7.1"), domain.ErrInvalidArgument},
		{"the binary of another platform", t.Context(), otherPlatform, domain.ErrInvalidArgument},
		{"a caller who went away", cancelled, porttest.FakeVerifiedBinary(t, "0.7.1"), context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakes{old: []byte("old")}, []byte("new"), []byte("new"))
			u := f.open(t)
			target := tt.target
			if errors.Is(tt.want, context.Canceled) {
				target = f.target
			}
			reported := false
			err := u.Start(tt.ctx, target, "user/alice", func(release.Progress) { reported = true })
			if !errors.Is(err, tt.want) {
				t.Errorf("Start = %v, want %v", err, tt.want)
			}
			if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateIdle {
				t.Errorf("Progress = %+v, %v", p, err)
			}
			if reported || f.source.Fetches(assetURL()) != 0 {
				t.Error("a refused start did something")
			}
		})
	}
}

func TestProgressOfACallerWhoWentAway(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakes{old: []byte("old")}, []byte("new"), []byte("new"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.open(t).Progress(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Progress = %v", err)
	}
}

// Every way a run can fail after it has begun: it ends failed, with a
// reason, the old executable in place and runnable, nothing of the run left
// in the directory, nothing kept as the previous binary, no restart.
func TestARunThatFailsLeavesTheExecutable(t *testing.T) {
	t.Parallel()
	old := buildFake(t, "0.7.0", "")
	good := buildFake(t, "0.7.1", "")
	tests := []struct {
		name   string
		served []byte
		want   []byte
		config func(c *Config)
		tune   func(u *Upgrader)
		reason string
	}{
		{
			name: "the release lists no binary of the platform", served: good, want: good,
			config: func(c *Config) {
				c.Source = porttest.NewReleaseSource([]release.Release{{
					Version: v071(t), Assets: []release.Asset{{Name: "cactusfs-plan9-386", URL: assetURL()}},
				}}, map[string][]byte{assetURL(): good})
			},
			reason: "lists no " + asset(),
		},
		{
			name: "the source does not list the release", served: good, want: good,
			config: func(c *Config) { c.Source = porttest.NewReleaseSource(nil, nil) },
			reason: "lists no " + asset(),
		},
		{
			name: "the source cannot be reached", served: good, want: good,
			config: func(c *Config) {
				source := porttest.NewReleaseSource(nil, nil)
				source.SetError(domain.ErrUnavailable)
				c.Source = source
			},
			reason: "could not be asked",
		},
		{
			name: "the binary is not the one the list names", served: append(bytes.Clone(good), 0), want: good,
			reason: "is not the one the signed SHA256SUMS lists",
		},
		{name: "the binary names another version", served: buildFake(t, "0.7.2", ""), reason: "did not name 0.7.1"},
		{name: "the binary says something else", served: buildFake(t, "0.7.1", "other"), reason: "did not name 0.7.1"},
		{name: "the binary fails", served: buildFake(t, "0.7.1", "fail"), reason: "did not run"},
		{
			name: "the binary does not answer in time", served: buildFake(t, "0.7.1", "hang"),
			tune:   func(u *Upgrader) { u.checkTimeout = time.Second },
			reason: "did not run",
		},
		{name: "the binary is no program", served: []byte("#!/bin/sh\nexit 0\n"), reason: "did not run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := tt.want
			if want == nil {
				want = tt.served
			}
			f := newFixture(t, fakes{old: old}, tt.served, want)
			if tt.config != nil {
				tt.config(&f.cfg)
			}
			u := f.open(t)
			if tt.tune != nil {
				tt.tune(u)
			}
			reports := runToEnd(t, u, f.target)
			last := reports[len(reports)-1]
			if last.State != release.StateFailed || len(last.Nodes) != 1 ||
				last.Nodes[0].State != release.NodeFailed || !strings.Contains(last.Nodes[0].Reason, tt.reason) {
				t.Errorf("the last progress = %+v; want failed, saying %q", last, tt.reason)
			}
			if got, err := os.ReadFile(f.exe); err != nil || !bytes.Equal(got, old) {
				t.Errorf("the executable was changed: %v", err)
			}
			if got := versionOf(t, f.exe); got != "0.7.0" {
				t.Errorf("the executable says %s", got)
			}
			if names := leftovers(t, f.dir); len(names) != 0 {
				t.Errorf("left in the directory: %v", names)
			}
			if _, err := os.Stat(filepath.Join(f.dir, PreviousName+exeSuffix())); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a previous binary was kept: %v", err)
			}
			if f.restarts.count() != 0 {
				t.Error("the node was restarted")
			}
			if ok, reason, _ := u.Availability(t.Context()); !ok {
				t.Errorf("after a failure, Availability = no: %s", reason)
			}
		})
	}
}

// The upgrade itself: the new binary at the executable's place with its
// mode, the old one kept, the directory clean; on Unix the node is asked to
// restart once, on Windows it must be restarted.
func TestTheBinaryIsReplaced(t *testing.T) {
	t.Parallel()
	old := buildFake(t, "0.7.0", "")
	good := buildFake(t, "0.7.1", "")
	for _, restart := range []bool{true, false} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakes{old: old}, good, good)
			if !restart {
				f.cfg.Restart = nil
			}
			u := f.open(t)
			before, err := os.Stat(f.exe)
			if err != nil {
				t.Fatal(err)
			}
			reports := runToEnd(t, u, f.target)

			var states []release.State
			for _, p := range reports {
				states = append(states, p.State)
				if p.Target != v071(t) || p.StartedBy != "user/alice" {
					t.Errorf("progress %+v", p)
				}
			}
			end, node, restarts := release.StateRestartRequired, release.NodeUpgrading, 0
			if restart && runtime.GOOS != "windows" {
				end, node, restarts = release.StateDone, release.NodeDone, 1
			}
			if want := []release.State{release.StateVerifying, release.StateRunning, end}; !slices.Equal(states, want) {
				t.Errorf("states = %v, want %v", states, want)
			}
			last := reports[len(reports)-1]
			if len(last.Nodes) != 1 || last.Nodes[0].ID != "node-1" || last.Nodes[0].State != node {
				t.Errorf("the node = %+v", last.Nodes)
			}
			// Done is said before the restart is asked for (the process that
			// answers next runs the new binary), so the restart may follow
			// the last report by a moment.
			for deadline := time.Now().Add(10 * time.Second); f.restarts.count() < restarts && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			if f.restarts.count() != restarts {
				t.Errorf("restarted %d times, want %d", f.restarts.count(), restarts)
			}
			if got := versionOf(t, f.exe); got != "0.7.1" {
				t.Errorf("the executable says %s", got)
			}
			previous := filepath.Join(f.dir, PreviousName+exeSuffix())
			if got := versionOf(t, previous); got != "0.7.0" {
				t.Errorf("the previous binary says %s", got)
			}
			after, err := os.Stat(f.exe)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && after.Mode().Perm() != before.Mode().Perm() {
				t.Errorf("the mode is %v, was %v", after.Mode().Perm(), before.Mode().Perm())
			}
			if names := leftovers(t, f.dir); len(names) != 0 {
				t.Errorf("left in the directory: %v", names)
			}
			if p, err := u.Progress(t.Context()); err != nil || p.State != end {
				t.Errorf("Progress = %+v, %v", p, err)
			}
			ok, reason, _ := u.Availability(t.Context())
			if ok || !strings.Contains(reason, "restart the node") {
				t.Errorf("after the swap, Availability = %v, %q", ok, reason)
			}
			if err := u.Start(t.Context(), f.target, "user/bob", nil); !errors.Is(err, domain.ErrUpgradeUnavailable) {
				t.Errorf("a second Start = %v", err)
			}
		})
	}
}

// A crash at any step of a run leaves a whole binary at the executable's
// place: the files are read at each step, as a crash there would leave them,
// for both ways of moving the binary into place. Stopped there, the run
// puts back what it changed.
func TestACrashLeavesAWholeBinary(t *testing.T) {
	t.Parallel()
	old := buildFake(t, "0.7.0", "")
	good := buildFake(t, "0.7.1", "")
	ways := []struct {
		name    string
		replace func(exe, previous, temp string, stage func(string) error) error
		// aside is whether the executable's path is empty while the old
		// binary is kept: replaceAside's two renames.
		aside bool
	}{
		{"by link", replaceByLink, false},
		{"aside", replaceAside, true},
	}
	for _, way := range ways {
		for _, stage := range []string{stageWritten, stageChecked, stageKept} {
			t.Run(way.name+" "+stage, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, fakes{old: old}, good, good)
				u := f.open(t)
				u.replace = way.replace
				previous := filepath.Join(f.dir, PreviousName+exeSuffix())
				reached := false
				u.stage = func(name string) error {
					if name != stage {
						return nil
					}
					reached = true
					temps := leftovers(t, f.dir)
					if len(temps) != 1 {
						t.Errorf("at %s, the run's files are %v; want the new binary", name, temps)
					} else if got, err := os.ReadFile(filepath.Join(f.dir, temps[0])); err != nil || !bytes.Equal(got, good) {
						t.Errorf("at %s, the new binary is not whole: %v", name, err)
					}
					exe, kept := f.exe, false
					if name == stageKept {
						kept = true
						if got, err := contentOf(previous); err != nil || !bytes.Equal(got, old) {
							t.Errorf("at %s, the previous binary is not the old one: %v", name, err)
						}
						if way.aside {
							exe = previous
						}
					}
					if got, err := contentOf(exe); err != nil || !bytes.Equal(got, old) {
						t.Errorf("at %s, %s is not the old binary: %v", name, filepath.Base(exe), err)
					}
					if !kept {
						if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("at %s, a previous binary exists: %v", name, err)
						}
					}
					return errors.New("crashed")
				}
				reports := runToEnd(t, u, f.target)
				if !reached {
					t.Fatalf("the run never reached %s", stage)
				}
				if last := reports[len(reports)-1]; last.State != release.StateFailed {
					t.Errorf("the last progress = %+v", last)
				}
				if got, err := os.ReadFile(f.exe); err != nil || !bytes.Equal(got, old) {
					t.Errorf("after the run, the executable is not the old binary: %v", err)
				}
				if names := leftovers(t, f.dir); len(names) != 0 {
					t.Errorf("left in the directory: %v", names)
				}
				if f.restarts.count() != 0 {
					t.Error("the node was restarted")
				}
				// What a real crash leaves, the next start removes.
				if err := os.WriteFile(filepath.Join(f.dir, tempPrefix+"crashed"), good, 0o600); err != nil {
					t.Fatal(err)
				}
				f.open(t)
				if names := leftovers(t, f.dir); len(names) != 0 {
					t.Errorf("after a restart, left in the directory: %v", names)
				}
			})
		}
	}
}

// TestTheNodeStoppingEndsARunUntilTheSwapBegins: the run outlives the
// request that started it, not the node. When the node's lifetime ends
// before the swap, the run fails and the executable is the old binary;
// when it ends once the swap has begun, the swap completes and the
// executable is the new binary, whole.
func TestTheNodeStoppingEndsARunUntilTheSwapBegins(t *testing.T) {
	t.Parallel()
	old := buildFake(t, "0.7.0", "")
	good := buildFake(t, "0.7.1", "")
	tests := []struct {
		stage string
		// swapped says whether the executable is the new binary after.
		swapped bool
	}{
		{stageWritten, false},
		{stageChecked, false},
		{stageKept, true},
	}
	for _, tt := range tests {
		t.Run("stopped at "+tt.stage, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakes{old: old}, good, good)
			lifetime, stop := context.WithCancel(t.Context())
			defer stop()
			f.cfg.Lifetime = lifetime
			u := f.open(t)
			u.stage = func(name string) error {
				if name == tt.stage {
					stop()
				}
				return nil
			}
			reports := runToEnd(t, u, f.target)
			last := reports[len(reports)-1]
			want, wantBinary := release.StateFailed, old
			if tt.swapped {
				// A swap that completes ends as every completed swap does
				// here: done, and restarting, where the node executes
				// itself again; restart-required on Windows, where it
				// cannot.
				want, wantBinary = release.StateDone, good
				if runtime.GOOS == "windows" {
					want = release.StateRestartRequired
				}
			}
			if last.State != want {
				t.Fatalf("the last progress = %+v, want %s", last, want)
			}
			// Stopped at written, the run of "<new> version" ends, or the
			// swap is refused, whichever comes first; stopped at checked,
			// nothing else is left before the swap. Either way the node's
			// stopping is the reason, not the new binary.
			if !tt.swapped && (len(last.Nodes) != 1 || !strings.Contains(last.Nodes[0].Reason, "the node stopped before")) {
				t.Errorf("the reason = %+v", last.Nodes)
			}
			if got, err := os.ReadFile(f.exe); err != nil || !bytes.Equal(got, wantBinary) {
				t.Errorf("the executable is not the binary it should be: %v", err)
			}
			if names := leftovers(t, f.dir); len(names) != 0 {
				t.Errorf("left in the directory: %v", names)
			}
			if restarted := f.restarts.count() != 0; restarted != (tt.swapped && runtime.GOOS != "windows") {
				t.Errorf("restarted = %v", restarted)
			}
		})
	}
	t.Run("a node stopped before the start", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, fakes{old: old}, good, good)
		lifetime, stop := context.WithCancel(t.Context())
		f.cfg.Lifetime = lifetime
		u := f.open(t)
		stop()
		reports := runToEnd(t, u, f.target)
		if last := reports[len(reports)-1]; last.State != release.StateFailed {
			t.Errorf("the last progress = %+v", last)
		}
		if got, err := os.ReadFile(f.exe); err != nil || !bytes.Equal(got, old) {
			t.Errorf("the executable is not the old binary: %v", err)
		}
	})
}

// Both ways of moving a binary into place, on plain files.
func TestReplace(t *testing.T) {
	t.Parallel()
	for _, way := range []struct {
		name    string
		replace func(exe, previous, temp string, stage func(string) error) error
	}{{"by link", replaceByLink}, {"aside", replaceAside}} {
		t.Run(way.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			exe, previous, temp := filepath.Join(dir, "exe"), filepath.Join(dir, "previous"), filepath.Join(dir, "temp")
			write := func(name, content string) {
				if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			read := func(name string) string {
				content, err := contentOf(name)
				if err != nil {
					return "<" + err.Error() + ">"
				}
				return string(content)
			}
			none := func(string) error { return nil }

			write(exe, "old")
			write(previous, "older")
			write(temp, "new")
			if err := way.replace(exe, previous, temp, none); err != nil {
				t.Fatal(err)
			}
			if read(exe) != "new" || read(previous) != "old" {
				t.Errorf("exe %q, previous %q", read(exe), read(previous))
			}
			if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("temp: %v", err)
			}

			// The new binary cannot be moved: the old one stays in place.
			write(exe, "old")
			if err := way.replace(exe, previous, filepath.Join(dir, "missing"), none); err == nil {
				t.Error("replace of a missing file = nil")
			}
			if read(exe) != "old" {
				t.Errorf("after a failure, exe %q", read(exe))
			}
			// The old one cannot be kept: nothing changes.
			if err := way.replace(filepath.Join(dir, "no-exe"), previous, temp, none); err == nil {
				t.Error("replace of a missing executable = nil")
			}
		})
	}
}

func TestCopyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeExecutable(t, src, []byte("binary"))
	if err := copyFile(dir, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	got, err := contentOf(dst)
	if err != nil || string(got) != "binary" {
		t.Errorf("dst = %q, %v", got, err)
	}
	if a, b := mode(t, src), mode(t, dst); a != b {
		t.Errorf("the copy's mode is %v, the file's %v", b, a)
	}
	if err := copyFile(dir, "src", "dst"); err == nil {
		t.Error("a copy over a file that exists = nil")
	}
	if err := copyFile(dir, "missing", "other"); err == nil {
		t.Error("a copy of a missing file = nil")
	}
	if err := copyFile(dir, "src", "../escaped"); err == nil {
		t.Error("a copy out of the directory = nil")
	}
	if err := copyFile(filepath.Join(dir, "missing"), "src", "other"); err == nil {
		t.Error("a copy in a missing directory = nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "other")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed copy left a file: %v", err)
	}
}

// mode returns the permissions of path.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestUpgraderContract(t *testing.T) {
	t.Parallel()
	old := buildFake(t, "0.7.0", "")
	good := buildFake(t, "0.7.1", "")
	porttest.UpgraderContract(t, func(t *testing.T) (ports.Upgrader, release.Verified, func()) {
		f := newFixture(t, fakes{old: old}, good, good)
		u := f.open(t)
		gate := make(chan struct{})
		u.stage = func(name string) error {
			if name == stageChecked {
				<-gate
			}
			return nil
		}
		var once sync.Once
		finish := func() { once.Do(func() { close(gate) }) }
		t.Cleanup(finish)
		return u, f.target, finish
	})
}

func TestInstructions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		goos, goarch string
		exe          string
		want         []string
	}{
		{"linux", "amd64", "/opt/it's here/cactusfs", []string{
			"cactusfs-linux-amd64", "sha256sum --check", `'/opt/it'\''s here/cactusfs'`, `'/opt/it'\''s here/cactusfs.previous'`,
		}},
		{"darwin", "arm64", "/usr/local/bin/cactusfs", []string{"cactusfs-darwin-arm64", "shasum -a 256 --check"}},
		{"windows", "amd64", `C:\Program Files\it's\cactusfs.exe`, []string{
			"cactusfs-windows-amd64.exe", "Get-FileHash", `'C:\Program Files\it''s\cactusfs.exe'`, "cactusfs.previous.exe",
		}},
		{"plan9", "386", "/bin/cactusfs", []string{"cactusfs-<os>-<arch>"}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()
			u := &Upgrader{cfg: Config{GOOS: tt.goos, GOARCH: tt.goarch, Executable: tt.exe}}
			dir, base := tt.exe[:strings.LastIndexAny(tt.exe, `/\`)+1], "cactusfs.previous"
			if tt.goos == "windows" {
				base += ".exe"
			}
			u.previous = dir + base
			text := strings.Join(u.instructions(), "\n")
			want := slices.Concat(tt.want, []string{
				"cosign verify-blob", release.SumsBundleAsset, sigstore.DefaultIdentityPrefix + "$VERSION", sigstore.DefaultIssuer,
				DownloadBase + "$VERSION",
			})
			for _, w := range want {
				if !strings.Contains(text, w) {
					t.Errorf("the instructions do not say %q:\n%s", w, text)
				}
			}
		})
	}
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()
	b := &limitedBuffer{limit: 4}
	for _, p := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(p)); n != len(p) || err != nil {
			t.Errorf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if b.String() != "abcd" {
		t.Errorf("kept %q", b.String())
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ab", 3); got != "ab" {
		t.Errorf("truncate = %q", got)
	}
}
