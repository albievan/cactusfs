package porttest_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func TestReleaseSourceContract(t *testing.T) {
	t.Parallel()
	porttest.ReleaseSourceContract(t, func(_ *testing.T, script porttest.ReleaseSourceScript) ports.ReleaseSource {
		return porttest.ScriptedReleaseSource(script)
	})
}

// TestReleaseSource: what the contract does not ask of the fake - its
// failures on demand and its count of fetches.
func TestReleaseSource(t *testing.T) {
	t.Parallel()
	v, err := release.ParseVersion("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	asset := release.Asset{Name: "a", URL: "https://releases.example/a"}
	source := porttest.NewReleaseSource([]release.Release{{Version: v, Assets: []release.Asset{asset}}}, map[string][]byte{asset.URL: []byte("a")})
	source.SetError(domain.ErrUnavailable)
	if _, err := source.Releases(t.Context()); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Releases = %v", err)
	}
	var buf bytes.Buffer
	if err := source.Fetch(t.Context(), asset, 10, &buf); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Fetch = %v", err)
	}
	source.SetError(nil)
	if err := source.Fetch(t.Context(), release.Asset{URL: "https://releases.example/none"}, 10, &buf); err == nil {
		t.Error("a file the source does not have was fetched")
	}
	if err := source.Fetch(t.Context(), asset, 10, &buf); err != nil || buf.String() != "a" || source.Fetches(asset.URL) != 2 {
		t.Errorf("Fetch = %q, %v, %d fetches", buf.String(), err, source.Fetches(asset.URL))
	}
}

func TestReleaseVerifierContract(t *testing.T) {
	t.Parallel()
	verifier := &porttest.ReleaseVerifier{}
	porttest.ReleaseVerifierContract(t, verifier, func(_ *testing.T, version release.Version, signed []byte) []byte {
		return porttest.FakeBundle(version, signed)
	})
}

func TestUpgraderContract(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		mode    release.Mode
		target  func(testing.TB, string) release.Verified
		outcome release.State
	}{
		{release.ModeBinary, porttest.FakeVerifiedBinary, release.StateRestartRequired},
		{release.ModeKubernetes, porttest.FakeVerifiedImage, release.StateDone},
		{release.ModeKubernetes, porttest.FakeVerifiedImage, release.StateFailed},
	} {
		t.Run(string(tt.mode)+"/"+string(tt.outcome), func(t *testing.T) {
			t.Parallel()
			porttest.UpgraderContract(t, func(t *testing.T) (ports.Upgrader, release.Verified, func()) {
				up := porttest.NewUpgrader(porttest.UpgraderConfig{Mode: tt.mode, Nodes: []string{"n1", "n2"}, Outcome: tt.outcome})
				return up, tt.target(t, "0.7.1"), up.Finish
			})
		})
	}
}

func TestUnavailableUpgraderContract(t *testing.T) {
	t.Parallel()
	off := porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeOff, Unavailable: "upgrade-mode is off"})
	porttest.UnavailableUpgraderContract(t, off, porttest.FakeVerifiedImage(t, "0.7.1"))
	if off.Starts() != 0 {
		t.Errorf("an unavailable upgrader began %d runs", off.Starts())
	}
}

// TestUpgrader: what the contract does not ask of the fake - the kind of
// artifact its mode installs, its clock, the nodes it reports, and a run
// after a run.
func TestUpgrader(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	up := porttest.NewUpgrader(porttest.UpgraderConfig{
		Mode: release.ModeBinary, Nodes: []string{"n1"}, Instructions: []string{"cactusfs version"},
		Now: func() time.Time { return at },
	})
	if ok, reason, instructions := up.Availability(t.Context()); !ok || reason != "" || len(instructions) != 1 {
		t.Errorf("Availability = %v, %q, %v", ok, reason, instructions)
	}
	if err := up.Start(t.Context(), porttest.FakeVerifiedImage(t, "0.7.1"), "user/alice", nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a binary upgrader given an image: %v", err)
	}
	ended := make(chan release.Progress, 4)
	report := func(p release.Progress) {
		if p.State.Ended() {
			ended <- p
		}
	}
	for i, version := range []string{"0.7.1", "0.7.2"} {
		if err := up.Start(t.Context(), porttest.FakeVerifiedBinary(t, version), "user/alice", report); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		up.Finish()
		up.Finish() // twice is once
		p := <-ended
		if p.State != release.StateDone || !p.StartedAt.Equal(at) || len(p.Nodes) != 1 || p.Nodes[0].Version != version || p.Nodes[0].State != release.NodeDone {
			t.Errorf("run %d ended as %+v", i, p)
		}
	}
	if up.Starts() != 2 {
		t.Errorf("Starts = %d", up.Starts())
	}
}
