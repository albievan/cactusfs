package porttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// ReleaseSource is a ports.ReleaseSource that lists the releases it was
// given and serves files by their address. It hands its releases out as
// release.NewRelease makes them, so that what it was given may hold what a
// real source would drop.
type ReleaseSource struct {
	mu       sync.Mutex
	releases []release.Release
	files    map[string][]byte
	err      error
	fetches  map[string]int
}

var _ ports.ReleaseSource = (*ReleaseSource)(nil)

// NewReleaseSource returns a source of the releases, with the files by
// address.
func NewReleaseSource(releases []release.Release, files map[string][]byte) *ReleaseSource {
	s := &ReleaseSource{files: map[string][]byte{}, fetches: map[string]int{}}
	s.releases = slices.Clone(releases)
	for url, content := range files {
		s.files[url] = bytes.Clone(content)
	}
	return s
}

// SetError makes every later call fail with err wrapped; nil ends that.
func (s *ReleaseSource) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Fetches returns how often the file at that address was asked for.
func (s *ReleaseSource) Fetches(url string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches[url]
}

// Releases implements ports.ReleaseSource.
func (s *ReleaseSource) Releases(ctx context.Context) ([]release.Release, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("porttest.ReleaseSource: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, fmt.Errorf("porttest.ReleaseSource: %w", s.err)
	}
	out := make([]release.Release, 0, len(s.releases))
	for _, r := range s.releases {
		out = append(out, release.NewRelease(r.Version, r.Published, r.Prerelease, r.NotesURL, r.Assets))
	}
	return out, nil
}

// Fetch implements ports.ReleaseSource.
func (s *ReleaseSource) Fetch(ctx context.Context, asset release.Asset, limit int64, w io.Writer) error {
	if !strings.HasPrefix(asset.URL, "https://") {
		return fmt.Errorf("porttest.ReleaseSource: %w: an asset is fetched over https only", domain.ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("porttest.ReleaseSource: %w", err)
	}
	s.mu.Lock()
	s.fetches[asset.URL]++
	content, known := s.files[asset.URL]
	err := s.err
	s.mu.Unlock()
	switch {
	case err != nil:
		return fmt.Errorf("porttest.ReleaseSource: %w", err)
	case !known:
		return errors.New("porttest.ReleaseSource: the source has no such file")
	case int64(len(content)) > limit:
		return fmt.Errorf("porttest.ReleaseSource: %w: the file is larger than %d bytes", domain.ErrEntityTooLarge, limit)
	}
	if _, err := w.Write(content); err != nil {
		return fmt.Errorf("porttest.ReleaseSource: %w", err)
	}
	return nil
}

// ScriptAsset is a file of a release of a ReleaseSourceScript.
type ScriptAsset struct {
	// Name is the file's name.
	Name string
	// Content is what it holds.
	Content []byte
	// Insecure has the source give the file an http address: a source
	// under test must leave it out.
	Insecure bool
}

// ScriptRelease is a release of a ReleaseSourceScript.
type ScriptRelease struct {
	// Tag is the release's tag, as the source names it; one that is no
	// version must be left out.
	Tag string
	// Prerelease is what the source says of it.
	Prerelease bool
	// Assets are its files.
	Assets []ScriptAsset
}

// ReleaseSourceScript is what a source under ReleaseSourceContract must
// list: the releases in this order, with their files.
type ReleaseSourceScript struct {
	// Releases are the releases.
	Releases []ScriptRelease
}

// releaseSourceScript returns the script of the contract: two releases, a
// pre-release by its version and one by what the source says, a tag that
// is no version, and a file with an http address.
func releaseSourceScript() ReleaseSourceScript {
	return ReleaseSourceScript{Releases: []ScriptRelease{
		{Tag: "v0.7.2", Assets: []ScriptAsset{
			{Name: "cactusfs-linux-amd64", Content: bytes.Repeat([]byte("binary "), 1000)},
			{Name: release.SumsAsset, Content: []byte("sums\n")},
			{Name: "cactusfs-linux-arm64", Content: []byte("over http"), Insecure: true},
		}},
		{Tag: "nightly", Assets: []ScriptAsset{{Name: "x", Content: []byte("x")}}},
		{Tag: "v0.8.0-rc.1", Prerelease: true, Assets: []ScriptAsset{{Name: release.DigestsAsset, Content: []byte("digests\n")}}},
		{Tag: "v0.8.0", Prerelease: true},
		{Tag: "v0.7.1"},
	}}
}

// ScriptedReleaseSource returns the source of this package that lists what
// the script says, at addresses of its own.
func ScriptedReleaseSource(script ReleaseSourceScript) *ReleaseSource {
	var releases []release.Release
	files := map[string][]byte{}
	for _, r := range script.Releases {
		version, err := release.ParseVersion(r.Tag)
		if err != nil {
			continue
		}
		var assets []release.Asset
		for _, a := range r.Assets {
			scheme := "https"
			if a.Insecure {
				scheme = "http"
			}
			url := scheme + "://releases.example/" + r.Tag + "/" + a.Name
			assets = append(assets, release.Asset{Name: a.Name, URL: url, Size: int64(len(a.Content))})
			files[url] = a.Content
		}
		releases = append(releases, release.Release{Version: version, Prerelease: r.Prerelease, Assets: assets})
	}
	return NewReleaseSource(releases, files)
}

// ReleaseSourceContract verifies that a source keeps the promises of
// ports.ReleaseSource. open returns a source that lists what the script
// says.
func ReleaseSourceContract(t *testing.T, open func(t *testing.T, script ReleaseSourceScript) ports.ReleaseSource) {
	t.Helper()
	script := releaseSourceScript()

	t.Run("the releases are listed in order, with https files only", func(t *testing.T) {
		t.Parallel()
		source := open(t, script)
		got, err := source.Releases(t.Context())
		if err != nil {
			t.Fatalf("Releases: %v", err)
		}
		var want []string
		for _, r := range script.Releases {
			if v, err := release.ParseVersion(r.Tag); err == nil {
				want = append(want, v.String())
			}
		}
		var listed []string
		for _, r := range got {
			listed = append(listed, r.Version.String())
			if r.Version.IsPrerelease() && !r.Prerelease {
				t.Errorf("%s is not marked as a pre-release", r.Version)
			}
			for _, a := range r.Assets {
				if !strings.HasPrefix(a.URL, "https://") {
					t.Errorf("%s of %s is at %s", a.Name, r.Version, a.URL)
				}
			}
		}
		if !slices.Equal(listed, want) {
			t.Errorf("Releases = %v, want %v", listed, want)
		}
		for _, r := range got {
			for _, s := range script.Releases {
				if v, err := release.ParseVersion(s.Tag); err == nil && v == r.Version && s.Prerelease && !r.Prerelease {
					t.Errorf("%s, which the source marks as a pre-release, is not one", r.Version)
				}
			}
		}
	})

	t.Run("a file is fetched whole, and not past its limit", func(t *testing.T) {
		t.Parallel()
		source := open(t, script)
		got, err := source.Releases(t.Context())
		if err != nil {
			t.Fatalf("Releases: %v", err)
		}
		fetched := 0
		for _, r := range got {
			for _, a := range r.Assets {
				want := scriptContent(script, r.Version, a.Name)
				var buf bytes.Buffer
				if err := source.Fetch(t.Context(), a, int64(len(want)), &buf); err != nil || !bytes.Equal(buf.Bytes(), want) {
					t.Errorf("Fetch(%s of %s) = %d bytes, %v; want %d", a.Name, r.Version, buf.Len(), err, len(want))
				}
				fetched++
				buf.Reset()
				limit := int64(len(want) - 1)
				err := source.Fetch(t.Context(), a, limit, &buf)
				if !errors.Is(err, domain.ErrEntityTooLarge) || int64(buf.Len()) > limit {
					t.Errorf("Fetch(%s of %s) one byte short of its size = %d bytes, %v", a.Name, r.Version, buf.Len(), err)
				}
			}
		}
		if fetched == 0 {
			t.Error("the source listed no file to fetch")
		}
	})

	t.Run("an address that is not https is not asked", func(t *testing.T) {
		t.Parallel()
		source := open(t, script)
		for _, url := range []string{"http://releases.example/x", "ftp://releases.example/x", "file:///etc/passwd", ""} {
			var buf bytes.Buffer
			if err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: url}, 1<<20, &buf); !errors.Is(err, domain.ErrInvalidArgument) || buf.Len() != 0 {
				t.Errorf("Fetch(%q) = %d bytes, %v", url, buf.Len(), err)
			}
		}
	})

	t.Run("a caller who went away is answered with that", func(t *testing.T) {
		t.Parallel()
		source := open(t, script)
		got, err := source.Releases(t.Context())
		if err != nil || len(got) == 0 || len(got[0].Assets) == 0 {
			t.Fatalf("Releases: %v, %v", got, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := source.Releases(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Releases after the caller went away: %v", err)
		}
		var buf bytes.Buffer
		if err := source.Fetch(ctx, got[0].Assets[0], 1<<20, &buf); !errors.Is(err, context.Canceled) {
			t.Errorf("Fetch after the caller went away: %v", err)
		}
	})
}

// scriptContent returns the content of a file of the script.
func scriptContent(script ReleaseSourceScript, version release.Version, name string) []byte {
	for _, r := range script.Releases {
		if v, err := release.ParseVersion(r.Tag); err != nil || v != version {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == name {
				return a.Content
			}
		}
	}
	return nil
}

// ReleaseVerifier is a ports.ReleaseVerifier that accepts the bundles
// FakeBundle makes, and nothing else. It stands for the Sigstore verifier in
// tests of what uses one; it proves nothing about anybody.
type ReleaseVerifier struct {
	mu    sync.Mutex
	calls int
}

var _ ports.ReleaseVerifier = (*ReleaseVerifier)(nil)

// fakeBundlePrefix begins every bundle of FakeBundle.
const fakeBundlePrefix = "porttest-fake-bundle "

// FakeBundle returns the bundle that ReleaseVerifier accepts for signed at
// the tag of version: the tag and the SHA-256 of signed, in text.
func FakeBundle(version release.Version, signed []byte) []byte {
	sum := sha256.Sum256(signed)
	return []byte(fakeBundlePrefix + version.Tag() + " " + hex.EncodeToString(sum[:]))
}

// Calls returns how often Verify was called.
func (v *ReleaseVerifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// Verify implements ports.ReleaseVerifier.
func (v *ReleaseVerifier) Verify(ctx context.Context, version release.Version, signed, bundle []byte) error {
	v.mu.Lock()
	v.calls++
	v.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("porttest.ReleaseVerifier: %w", err)
	}
	if !bytes.Equal(bundle, FakeBundle(version, signed)) {
		return fmt.Errorf("porttest.ReleaseVerifier: %w: the bundle is not of these bytes at %s", domain.ErrReleaseNotVerified, version.Tag())
	}
	return nil
}

// ReleaseVerifierContract verifies that a verifier keeps the promises of
// ports.ReleaseVerifier. sign returns a bundle that the verifier must
// accept for signed at the tag of version - one the release workflow would
// have made.
func ReleaseVerifierContract(t *testing.T, verifier ports.ReleaseVerifier, sign func(t *testing.T, version release.Version, signed []byte) []byte) {
	t.Helper()
	v071, err := release.ParseVersion("0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	v072, err := release.ParseVersion("0.7.2")
	if err != nil {
		t.Fatal(err)
	}
	signed := []byte("0000000000000000000000000000000000000000000000000000000000000000  cactusfs-linux-amd64\n")
	bundle := sign(t, v071, signed)

	t.Run("a bundle of the bytes at the tag of the version verifies", func(t *testing.T) {
		t.Parallel()
		if err := verifier.Verify(t.Context(), v071, signed, bundle); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})

	t.Run("everything else does not", func(t *testing.T) {
		t.Parallel()
		changed := bytes.Clone(signed)
		changed[0] ^= 1
		truncated := bundle[:len(bundle)/2]
		flipped := bytes.Clone(bundle)
		flipped[len(flipped)/2] ^= 1
		tests := []struct {
			name    string
			version release.Version
			signed  []byte
			bundle  []byte
		}{
			{"other bytes", v071, changed, bundle},
			{"no bytes", v071, nil, bundle},
			{"the bundle of an older version, passed off as newer", v072, signed, bundle},
			{"the bundle of a newer version, passed off as older", v071, signed, sign(t, v072, signed)},
			{"no bundle", v071, signed, nil},
			{"a bundle cut short", v071, signed, truncated},
			{"a bundle with a bit changed", v071, signed, flipped},
			{"a bundle that is no bundle", v071, signed, []byte(`{"mediaType":"nothing"}`)},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if err := verifier.Verify(t.Context(), tt.version, tt.signed, tt.bundle); !errors.Is(err, domain.ErrReleaseNotVerified) {
					t.Errorf("Verify = %v, want ErrReleaseNotVerified", err)
				}
			})
		}
	})

	t.Run("a caller who went away is not told yes", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := verifier.Verify(ctx, v071, signed, bundle); err == nil {
			t.Error("Verify after the caller went away = nil")
		}
	})
}

// UpgraderConfig is what an Upgrader of this package does.
type UpgraderConfig struct {
	// Mode is the mode it reports; it installs binaries in ModeBinary and
	// images otherwise.
	Mode release.Mode
	// Unavailable, when not empty, is the reason Availability says no.
	Unavailable string
	// Instructions are what Availability gives.
	Instructions []string
	// Nodes are the nodes of the deployment, in the order they are moved.
	Nodes []string
	// Outcome is the state a run ends in once Finish is called:
	// release.StateDone when empty.
	Outcome release.State
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Upgrader is a ports.Upgrader whose runs end when a test says so.
type Upgrader struct {
	cfg UpgraderConfig

	mu       sync.Mutex
	progress release.Progress
	finish   chan struct{}
	starts   int
}

var _ ports.Upgrader = (*Upgrader)(nil)

// NewUpgrader returns an upgrader that does as cfg says.
func NewUpgrader(cfg UpgraderConfig) *Upgrader {
	if cfg.Outcome == "" {
		cfg.Outcome = release.StateDone
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Instructions = slices.Clone(cfg.Instructions)
	cfg.Nodes = slices.Clone(cfg.Nodes)
	return &Upgrader{cfg: cfg}
}

// Starts returns how often a run was begun.
func (u *Upgrader) Starts() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.starts
}

// Finish lets the run under way end, in the state of UpgraderConfig.Outcome.
// It does nothing when no run is under way.
func (u *Upgrader) Finish() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finish != nil {
		close(u.finish)
		u.finish = nil
	}
}

// Mode implements ports.Upgrader.
func (u *Upgrader) Mode() release.Mode { return u.cfg.Mode }

// Availability implements ports.Upgrader.
func (u *Upgrader) Availability(context.Context) (ok bool, reason string, instructions []string) {
	return u.cfg.Unavailable == "" && u.cfg.Mode != release.ModeOff, u.cfg.Unavailable, slices.Clone(u.cfg.Instructions)
}

// Start implements ports.Upgrader.
func (u *Upgrader) Start(ctx context.Context, target release.Verified, startedBy string, report func(release.Progress)) error {
	if ok, _, _ := u.Availability(ctx); !ok {
		return fmt.Errorf("porttest.Upgrader: %w", domain.ErrUpgradeUnavailable)
	}
	if target.IsZero() {
		return fmt.Errorf("porttest.Upgrader: %w: nothing verified to install", domain.ErrReleaseNotVerified)
	}
	want := release.KindImage
	if u.cfg.Mode == release.ModeBinary {
		want = release.KindBinary
	}
	if target.Kind() != want {
		return fmt.Errorf("porttest.Upgrader: %w: a %s upgrader does not install a %s", domain.ErrInvalidArgument, u.cfg.Mode, target.Kind())
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("porttest.Upgrader: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.progress.State.Active() {
		return fmt.Errorf("porttest.Upgrader: %w", domain.ErrUpgradeRunning)
	}
	u.starts++
	u.progress = release.Progress{
		State: release.StateVerifying, Target: target.Version(), StartedBy: startedBy, StartedAt: u.cfg.Now(),
	}
	for _, id := range u.cfg.Nodes {
		u.progress.Nodes = append(u.progress.Nodes, release.NodeProgress{ID: id, State: release.NodePending})
	}
	finish := make(chan struct{})
	u.finish = finish
	first := u.progress.Normalized()
	go u.run(first, finish, report)
	return nil
}

// run is a run: it reports that it has begun and is running, and ends when
// Finish is called.
func (u *Upgrader) run(first release.Progress, finish <-chan struct{}, report func(release.Progress)) {
	call := func(p release.Progress) {
		if report != nil {
			report(p)
		}
	}
	call(first)
	running := u.set(func(p *release.Progress) {
		p.State = release.StateRunning
		for i := range p.Nodes {
			p.Nodes[i].State = release.NodeUpgrading
		}
	})
	call(running)
	<-finish
	ended := u.set(func(p *release.Progress) {
		p.State = u.cfg.Outcome
		for i := range p.Nodes {
			p.Nodes[i].Version = p.Target.String()
			p.Nodes[i].State = release.NodeDone
			if u.cfg.Outcome == release.StateFailed {
				p.Nodes[i].State, p.Nodes[i].Reason = release.NodeRestored, "the node did not become ready"
			}
		}
	})
	call(ended)
}

// set changes the progress and returns a copy of it.
func (u *Upgrader) set(change func(*release.Progress)) release.Progress {
	u.mu.Lock()
	defer u.mu.Unlock()
	change(&u.progress)
	return u.progress.Normalized()
}

// Progress implements ports.Upgrader.
func (u *Upgrader) Progress(ctx context.Context) (release.Progress, error) {
	if err := ctx.Err(); err != nil {
		return release.Progress{}, fmt.Errorf("porttest.Upgrader: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.progress.Normalized(), nil
}

// FakeVerifiedBinary returns a release.Verified of a binary of the version:
// made, as only verified bytes may make one, from a SHA256SUMS of its own.
func FakeVerifiedBinary(t testing.TB, version string) release.Verified {
	t.Helper()
	v, err := release.ParseVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("binary " + version))
	name := release.BinaryAsset("linux", "amd64")
	verified, err := release.NewVerifiedBinary(v, fmt.Appendf(nil, "%x  %s\n", sum, name), name, sum)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

// FakeVerifiedImage returns a release.Verified of the image of the version,
// made from a digests.txt of its own.
func FakeVerifiedImage(t testing.TB, version string) release.Verified {
	t.Helper()
	v, err := release.ParseVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	list := sha256.Sum256([]byte("list " + version))
	platform := sha256.Sum256([]byte("platform " + version))
	digests := fmt.Sprintf("registry.example/cactusfs:%s sha256:%x\nregistry.example/cactusfs:%s linux/amd64 sha256:%x\n", v, list, v, platform)
	verified, err := release.NewVerifiedImage(v, []byte(digests))
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

// upgraderContractWait is how long the contract waits for a run to end once
// it may. A run of a real upgrader against the fakes of its test ends in
// far less; the bound is there so that one that hangs fails the test.
const upgraderContractWait = 30 * time.Second

// UnavailableUpgraderContract verifies that an upgrader that says it cannot
// upgrade does nothing: Availability says why, Start refuses target with
// domain.ErrUpgradeUnavailable and reports nothing, and the progress stays
// idle.
func UnavailableUpgraderContract(t *testing.T, upgrader ports.Upgrader, target release.Verified) {
	t.Helper()
	ok, reason, _ := upgrader.Availability(t.Context())
	if ok || reason == "" {
		t.Errorf("Availability = %v, %q; want no, and why", ok, reason)
	}
	reported := false
	err := upgrader.Start(t.Context(), target, "user/alice", func(release.Progress) { reported = true })
	if !errors.Is(err, domain.ErrUpgradeUnavailable) {
		t.Errorf("Start = %v, want ErrUpgradeUnavailable", err)
	}
	progress, err := upgrader.Progress(t.Context())
	if err != nil || progress.State != release.StateIdle || !progress.Target.IsZero() {
		t.Errorf("Progress = %+v, %v; want idle", progress, err)
	}
	if reported {
		t.Error("a refused start reported progress")
	}
}

// UpgraderContract verifies that an upgrader that can upgrade keeps the
// promises of ports.Upgrader. open returns such an upgrader, a target it
// installs, and finish, which lets the run of that target end: until it
// is called the run must not end by itself.
func UpgraderContract(t *testing.T, open func(t *testing.T) (upgrader ports.Upgrader, target release.Verified, finish func())) {
	t.Helper()

	t.Run("nothing verified, nothing begun", func(t *testing.T) {
		t.Parallel()
		upgrader, _, _ := open(t)
		reported := false
		err := upgrader.Start(t.Context(), release.Verified{}, "user/alice", func(release.Progress) { reported = true })
		if !errors.Is(err, domain.ErrReleaseNotVerified) {
			t.Errorf("Start of nothing verified = %v", err)
		}
		if progress, err := upgrader.Progress(t.Context()); err != nil || progress.State != release.StateIdle {
			t.Errorf("Progress = %+v, %v; want idle", progress, err)
		}
		if reported {
			t.Error("a refused start reported progress")
		}
	})

	t.Run("a run begins, outlives its start, is the only one, and ends", func(t *testing.T) {
		t.Parallel()
		upgrader, target, finish := open(t)
		if upgrader.Mode() == release.ModeOff {
			t.Errorf("an upgrader that can upgrade is of mode %s", release.ModeOff)
		}
		if ok, reason, _ := upgrader.Availability(t.Context()); !ok {
			t.Fatalf("Availability = no: %s", reason)
		}
		var mu sync.Mutex
		var reports []release.Progress
		ended := make(chan struct{})
		report := func(p release.Progress) {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, p)
			if p.State.Ended() && len(reports) > 0 && !slices.ContainsFunc(reports[:len(reports)-1], func(q release.Progress) bool { return q.State.Ended() }) {
				close(ended)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		before := time.Now().Add(-time.Second)
		if err := upgrader.Start(ctx, target, "user/alice", report); err != nil {
			t.Fatalf("Start: %v", err)
		}
		// What bounds the start does not bound the run.
		cancel()
		progress, err := upgrader.Progress(t.Context())
		if err != nil || !progress.State.Active() || progress.Target != target.Version() ||
			progress.StartedBy != "user/alice" || progress.StartedAt.Before(before) {
			t.Errorf("Progress of a run under way = %+v, %v", progress, err)
		}
		if err := upgrader.Start(t.Context(), target, "user/bob", nil); !errors.Is(err, domain.ErrUpgradeRunning) {
			t.Errorf("a second Start = %v, want ErrUpgradeRunning", err)
		}
		finish()
		select {
		case <-ended:
		case <-time.After(upgraderContractWait):
			t.Fatal("the run did not end")
		}
		mu.Lock()
		defer mu.Unlock()
		known := []release.State{release.StateVerifying, release.StateRunning, release.StateRestartRequired, release.StateFailed, release.StateDone}
		for i, p := range reports {
			if !slices.Contains(known, p.State) || p.Target != target.Version() || p.StartedBy != "user/alice" {
				t.Errorf("report %d = %+v", i, p)
			}
			if i < len(reports)-1 && p.State.Ended() {
				t.Errorf("report %d has ended, and more came", i)
			}
		}
		last := reports[len(reports)-1]
		progress, err = upgrader.Progress(t.Context())
		if err != nil || progress.State != last.State || progress.Target != last.Target {
			t.Errorf("Progress after the end = %+v, %v; the last report said %+v", progress, err, last)
		}
	})
}
