package release_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
)

func TestNewRelease(t *testing.T) {
	t.Parallel()
	published := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	good := release.Asset{Name: "cactusfs-linux-amd64", URL: "https://github.com/albievan/cactusfs/releases/download/v0.7.1/cactusfs-linux-amd64", Size: 40 << 20}
	sums := release.Asset{Name: release.SumsAsset, URL: "https://example.com/SHA256SUMS?x=1", Size: 600}
	tests := []struct {
		name       string
		version    string
		prerelease bool
		notes      string
		assets     []release.Asset
		wantPre    bool
		wantNotes  string
		wantAssets []release.Asset
	}{
		{
			"everything kept", "0.7.1", false, "https://github.com/albievan/cactusfs/releases/tag/v0.7.1",
			[]release.Asset{good, sums},
			false, "https://github.com/albievan/cactusfs/releases/tag/v0.7.1",
			[]release.Asset{good, sums},
		},
		{"a pre-release by its version", "0.8.0-rc.1", false, "", nil, true, "", nil},
		{"a pre-release by its source", "0.8.0", true, "", nil, true, "", nil},
		{"notes over http are dropped", "0.7.1", false, "http://example.com/notes", nil, false, "", nil},
		{"notes as script are dropped", "0.7.1", false, "javascript:alert(1)", nil, false, "", nil},
		{"notes with a user are dropped", "0.7.1", false, "https://user:pw@example.com/notes", nil, false, "", nil},
		{"notes with a fragment are dropped", "0.7.1", false, "https://example.com/notes#x", nil, false, "", nil},
		{"notes with a space are dropped", "0.7.1", false, "https://example.com/no tes", nil, false, "", nil},
		{"notes without a host are dropped", "0.7.1", false, "https:///notes", nil, false, "", nil},
		{"notes in upper case are dropped", "0.7.1", false, "HTTPS://example.com/notes", nil, false, "", nil},
		{"notes too long are dropped", "0.7.1", false, "https://example.com/" + strings.Repeat("a", release.MaxURLLength), nil, false, "", nil},
		{"assets that may not be kept are dropped, never the release", "0.7.1", false, "", []release.Asset{
			{Name: "a", URL: "http://example.com/a"},
			{Name: "b", URL: "file:///etc/passwd"},
			{Name: "../c", URL: "https://example.com/c"},
			{Name: "d\\e", URL: "https://example.com/d"},
			{Name: "", URL: "https://example.com/e"},
			{Name: "..", URL: "https://example.com/f"},
			{Name: "g h", URL: "https://example.com/g"},
			{Name: "neg", URL: "https://example.com/neg", Size: -1},
			{Name: strings.Repeat("n", release.MaxAssetNameLength+1), URL: "https://example.com/n"},
			{Name: "ctl\x00", URL: "https://example.com/n"},
			good,
		}, false, "", []release.Asset{good}},
		{"an asset named twice is dropped both times", "0.7.1", false, "", []release.Asset{
			good, sums, {Name: good.Name, URL: "https://evil.example/x"},
		}, false, "", []release.Asset{sums}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := release.NewRelease(mustVersion(t, tt.version), published, tt.prerelease, tt.notes, tt.assets)
			if r.Version != mustVersion(t, tt.version) || !r.Published.Equal(published) || r.Prerelease != tt.wantPre ||
				r.NotesURL != tt.wantNotes || !reflect.DeepEqual(r.Assets, tt.wantAssets) {
				t.Errorf("NewRelease = %+v", r)
			}
			for _, a := range tt.wantAssets {
				if got, ok := r.Asset(a.Name); !ok || got != a {
					t.Errorf("Asset(%s) = %+v, %v", a.Name, got, ok)
				}
			}
		})
	}
	if _, ok := release.NewRelease(mustVersion(t, "0.7.1"), published, false, "", nil).Asset("x"); ok {
		t.Error("an asset that is not there was found")
	}
}

func TestParseChannel(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"stable", "prerelease"} {
		if c, err := release.ParseChannel(s); err != nil || string(c) != s {
			t.Errorf("ParseChannel(%q) = %q, %v", s, c, err)
		}
	}
	for _, s := range []string{"", "Stable", "beta", "pre-release", "stable "} {
		if _, err := release.ParseChannel(s); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParseChannel(%q) = %v", s, err)
		}
	}
}

// rel makes the release of a version, a pre-release when the version is.
func rel(t testing.TB, version string) release.Release {
	t.Helper()
	return release.NewRelease(mustVersion(t, version), time.Time{}, false, "", nil)
}

func names(rs []release.Release) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Version.String()
	}
	return out
}

func TestAvailable(t *testing.T) {
	t.Parallel()
	var listed []release.Release
	for _, v := range []string{"0.6.9", "0.7.0", "0.7.2", "0.7.1", "0.8.0-rc.1", "0.8.0", "0.8.1", "0.7.2", "1.0.0", "0.9.0-rc.2"} {
		listed = append(listed, rel(t, v))
	}
	// One the source marks as a pre-release, though its version is not.
	listed = append(listed, release.NewRelease(mustVersion(t, "0.8.2"), time.Time{}, true, "", nil))
	tests := []struct {
		current string
		channel release.Channel
		want    []string
	}{
		{"0.7.0", release.ChannelStable, []string{"1.0.0", "0.8.1", "0.8.0", "0.7.2", "0.7.1"}},
		{"0.7.0", release.ChannelPrerelease, []string{"1.0.0", "0.9.0-rc.2", "0.8.2", "0.8.1", "0.8.0", "0.8.0-rc.1", "0.7.2", "0.7.1"}},
		{"0.8.0-rc.1", release.ChannelStable, []string{"1.0.0", "0.8.1", "0.8.0"}},
		{"1.0.0", release.ChannelPrerelease, nil},
		{"0.7.0", release.Channel("nightly"), []string{"1.0.0", "0.8.1", "0.8.0", "0.7.2", "0.7.1"}},
	}
	for _, tt := range tests {
		got := names(release.Available(mustVersion(t, tt.current), listed, tt.channel))
		if !slices.Equal(got, tt.want) {
			t.Errorf("Available(%s, %s) = %v, want %v", tt.current, tt.channel, got, tt.want)
		}
	}
	if got := release.Available(mustVersion(t, "0.7.0"), nil, release.ChannelStable); len(got) != 0 {
		t.Errorf("Available of nothing = %v", got)
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()
	var listed []release.Release
	for _, v := range []string{"0.7.0", "0.7.1", "0.7.2", "0.8.0", "0.8.1", "0.9.0", "1.0.0", "0.10.0-rc.1", "0.9.1-rc.1"} {
		listed = append(listed, rel(t, v))
	}
	flagged := append(slices.Clone(listed), release.NewRelease(mustVersion(t, "0.7.3"), time.Time{}, true, "", nil))
	tests := []struct {
		name     string
		current  string
		target   string
		releases []release.Release
		channel  release.Channel
		want     release.Reason // "" when allowed
	}{
		{"a newer patch of the running minor", "0.7.0", "0.7.1", listed, release.ChannelStable, ""},
		{"the newest patch of the running minor", "0.7.0", "0.7.2", listed, release.ChannelStable, ""},
		{"a patch of the running minor that is not listed", "0.7.0", "0.7.9", listed, release.ChannelStable, ""},
		{"the newest patch of the next minor", "0.7.0", "0.8.1", listed, release.ChannelStable, ""},
		{"from a patch, the newest patch of the next minor", "0.7.2", "0.8.1", listed, release.ChannelStable, ""},
		{"the next minor of one release", "0.8.1", "0.9.0", listed, release.ChannelStable, ""},
		{"the release of a running pre-release", "0.9.1-rc.1", "0.9.1", append(slices.Clone(listed), rel(t, "0.9.1")), release.ChannelStable, ""},
		{"a pre-release of the next minor on its channel", "0.9.0", "0.10.0-rc.1", listed, release.ChannelPrerelease, ""},
		{"a pre-release of the running minor on its channel", "0.9.0", "0.9.1-rc.1", listed, release.ChannelPrerelease, ""},
		{"the running version", "0.7.1", "0.7.1", listed, release.ChannelStable, release.ReasonEqual},
		{"an older patch", "0.7.2", "0.7.1", listed, release.ChannelStable, release.ReasonOlder},
		{"an older minor", "0.8.0", "0.7.2", listed, release.ChannelStable, release.ReasonOlder},
		{"the pre-release of the running release", "0.9.1", "0.9.1-rc.1", listed, release.ChannelPrerelease, release.ReasonOlder},
		{"a pre-release on stable", "0.9.0", "0.9.1-rc.1", listed, release.ChannelStable, release.ReasonPrereleaseOnStable},
		{"a pre-release on a channel that is neither", "0.9.0", "0.9.1-rc.1", listed, release.Channel(""), release.ReasonPrereleaseOnStable},
		{"a release the source marks as a pre-release, on stable", "0.7.2", "0.7.3", flagged, release.ChannelStable, release.ReasonPrereleaseOnStable},
		{"a release the source marks as a pre-release, on its channel", "0.7.2", "0.7.3", flagged, release.ChannelPrerelease, ""},
		{"a major", "0.9.0", "1.0.0", listed, release.ChannelStable, release.ReasonMajor},
		{"a major from the last minor", "0.10.0", "1.0.0", listed, release.ChannelPrerelease, release.ReasonMajor},
		{"a skipped minor", "0.7.2", "0.9.0", listed, release.ChannelStable, release.ReasonSkippedMinor},
		{"a skipped minor of no release", "0.7.2", "0.12.0", listed, release.ChannelStable, release.ReasonSkippedMinor},
		{"not the newest patch of the next minor", "0.7.2", "0.8.0", listed, release.ChannelStable, release.ReasonNotNewestPatch},
		{
			"the release of the next minor while its pre-release is newer, on its channel", "0.8.1", "0.9.0",
			listed, release.ChannelPrerelease, release.ReasonNotNewestPatch,
		},
		{"the next minor that is not listed", "0.7.2", "0.8.5", listed, release.ChannelStable, release.ReasonUnknownRelease},
		{"the next minor with no releases at all", "0.7.2", "0.8.1", nil, release.ChannelStable, release.ReasonUnknownRelease},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			current, target := mustVersion(t, tt.current), mustVersion(t, tt.target)
			err := release.Allowed(current, target, tt.releases, tt.channel)
			if tt.want == "" {
				if err != nil {
					t.Errorf("Allowed(%s → %s) = %v", current, target, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrUpgradeNotAllowed) || release.ReasonOf(err) != tt.want {
				t.Fatalf("Allowed(%s → %s) = %v (%s), want %s", current, target, err, release.ReasonOf(err), tt.want)
			}
			var refusal *release.Refusal
			if !errors.As(err, &refusal) || refusal.Current != current || refusal.Target != target {
				t.Errorf("the refusal is %+v", refusal)
			}
			if !strings.Contains(err.Error(), string(tt.want)) || !strings.Contains(err.Error(), target.String()) {
				t.Errorf("the refusal reads %q", err)
			}
			if errors.Is(err, domain.ErrAccessDenied) {
				t.Error("a refusal of a version reads as a denial")
			}
		})
	}
	if release.ReasonOf(errors.New("other")) != "" || release.ReasonOf(nil) != "" {
		t.Error("ReasonOf of no refusal")
	}
	if wrapped := fmt.Errorf("updates.Start: %w", release.Allowed(mustVersion(t, "1.0.0"), mustVersion(t, "1.0.0"), nil, release.ChannelStable)); release.ReasonOf(wrapped) != release.ReasonEqual {
		t.Errorf("ReasonOf a wrapped refusal = %q", release.ReasonOf(wrapped))
	}
}

// TestAllowedNeverAdmitsWhatItMustNot holds Allowed, NextStep and Available
// to their rules over every pair of a set of versions, every channel, and
// the releases of the set: nothing older or equal, no other major, no
// skipped minor, no pre-release on stable, of the next minor only its
// newest; and NextStep is the newest of what Allowed admits.
func TestAllowedNeverAdmitsWhatItMustNot(t *testing.T) {
	t.Parallel()
	all := versions(t)
	var listed []release.Release
	for _, v := range all {
		listed = append(listed, release.NewRelease(v, time.Time{}, false, "", nil))
	}
	for _, channel := range []release.Channel{release.ChannelStable, release.ChannelPrerelease, ""} {
		for _, current := range all {
			available := release.Available(current, listed, channel)
			if !slices.IsSortedFunc(available, func(a, b release.Release) int { return b.Version.Compare(a.Version) }) {
				t.Fatalf("Available(%s, %s) is not newest first", current, channel)
			}
			var newestAllowed release.Version
			for _, r := range available {
				if r.Version.Compare(current) <= 0 || (channel != release.ChannelPrerelease && r.Prerelease) {
					t.Fatalf("Available(%s, %s) offers %s", current, channel, r.Version)
				}
			}
			for _, target := range all {
				err := release.Allowed(current, target, listed, channel)
				if err != nil {
					if !errors.Is(err, domain.ErrUpgradeNotAllowed) || release.ReasonOf(err) == "" {
						t.Fatalf("Allowed(%s → %s) = %v, which is no refusal", current, target, err)
					}
					continue
				}
				switch {
				case target.Compare(current) <= 0:
					t.Fatalf("Allowed(%s → %s): older or equal", current, target)
				case target.Major() != current.Major():
					t.Fatalf("Allowed(%s → %s): another major", current, target)
				case target.IsPrerelease() && channel != release.ChannelPrerelease:
					t.Fatalf("Allowed(%s → %s) on %q: a pre-release", current, target, channel)
				case target.Minor() != current.Minor() && target.Minor() != current.Minor()+1:
					t.Fatalf("Allowed(%s → %s): a skipped minor", current, target)
				}
				if target.Minor() == current.Minor()+1 {
					for _, r := range available {
						if r.Version.SameMinor(target) && r.Version.Compare(target) > 0 {
							t.Fatalf("Allowed(%s → %s) though %s is newer", current, target, r.Version)
						}
					}
				}
				if target.Compare(newestAllowed) > 0 {
					newestAllowed = target
				}
			}
			next, ok := release.NextStep(current, listed, channel)
			if ok != !newestAllowed.IsZero() || (ok && next.Version != newestAllowed) {
				t.Fatalf("NextStep(%s, %q) = %s, %v; the newest allowed is %s", current, channel, next.Version, ok, newestAllowed)
			}
		}
	}
}

func TestNextStep(t *testing.T) {
	t.Parallel()
	var listed []release.Release
	for _, v := range []string{"0.7.1", "0.7.2", "0.8.0", "0.8.1", "0.9.0", "0.9.1-rc.1"} {
		listed = append(listed, rel(t, v))
	}
	tests := []struct {
		current string
		channel release.Channel
		want    string // "" for none
	}{
		{"0.7.0", release.ChannelStable, "0.8.1"},
		{"0.7.2", release.ChannelStable, "0.8.1"},
		{"0.8.1", release.ChannelStable, "0.9.0"},
		{"0.9.0", release.ChannelStable, ""},
		{"0.9.0", release.ChannelPrerelease, "0.9.1-rc.1"},
		{"0.6.0", release.ChannelStable, "0.7.2"},
		{"0.5.0", release.ChannelStable, ""},
		{"1.0.0", release.ChannelStable, ""},
	}
	for _, tt := range tests {
		got, ok := release.NextStep(mustVersion(t, tt.current), listed, tt.channel)
		if (tt.want == "") == ok || (ok && got.Version.String() != tt.want) {
			t.Errorf("NextStep(%s, %s) = %s, %v; want %q", tt.current, tt.channel, got.Version, ok, tt.want)
		}
	}
}

func TestBinaryAsset(t *testing.T) {
	t.Parallel()
	tests := []struct{ goos, goarch, want string }{
		{"linux", "amd64", "cactusfs-linux-amd64"},
		{"linux", "arm64", "cactusfs-linux-arm64"},
		{"darwin", "amd64", "cactusfs-darwin-amd64"},
		{"darwin", "arm64", "cactusfs-darwin-arm64"},
		{"windows", "amd64", "cactusfs-windows-amd64.exe"},
		{"windows", "arm64", "cactusfs-windows-arm64.exe"},
		{"freebsd", "amd64", ""},
		{"linux", "386", ""},
		{"", "", ""},
		{"linux/../x", "amd64", ""},
	}
	for _, tt := range tests {
		if got := release.BinaryAsset(tt.goos, tt.goarch); got != tt.want {
			t.Errorf("BinaryAsset(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestProgress(t *testing.T) {
	t.Parallel()
	var zero release.Progress
	if got := zero.Normalized(); got.State != release.StateIdle || got.Nodes != nil {
		t.Errorf("the zero progress = %+v", got)
	}
	p := release.Progress{State: release.StateRunning, Nodes: []release.NodeProgress{{ID: "n1", State: release.NodeUpgrading}}}
	q := p.Normalized()
	q.Nodes[0].ID = "changed"
	if p.Nodes[0].ID != "n1" {
		t.Error("Normalized shares its nodes")
	}
	for _, tt := range []struct {
		state         release.State
		active, ended bool
	}{
		{release.StateIdle, false, false},
		{release.StateVerifying, true, false},
		{release.StateRunning, true, false},
		{release.StateRestartRequired, false, true},
		{release.StateFailed, false, true},
		{release.StateDone, false, true},
		{release.State("unknown"), false, false},
	} {
		if tt.state.Active() != tt.active || tt.state.Ended() != tt.ended {
			t.Errorf("%s: active %v, ended %v", tt.state, tt.state.Active(), tt.state.Ended())
		}
	}
}
