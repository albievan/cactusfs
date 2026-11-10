package release

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// The files of a release besides its binaries (scripts/release.sh). Each
// list is signed by the release workflow into a Sigstore bundle of the same
// name and ".sigstore.json".
const (
	// SumsAsset is the SHA-256 of every binary, as sha256sum writes it.
	SumsAsset = "SHA256SUMS"
	// SumsBundleAsset is the Sigstore bundle of SumsAsset.
	SumsBundleAsset = "SHA256SUMS.sigstore.json"
	// DigestsAsset names the digests of the release's images.
	DigestsAsset = "digests.txt"
	// DigestsBundleAsset is the Sigstore bundle of DigestsAsset.
	DigestsBundleAsset = "digests.txt.sigstore.json"
)

// Bounds of what a release may say of itself. A source that says more is
// not believed further than these.
const (
	// MaxURLLength is the longest address of an asset or of the notes kept.
	MaxURLLength = 2048
	// MaxAssetNameLength is the longest name of an asset kept.
	MaxAssetNameLength = 255
)

// Asset is a file of a release.
type Asset struct {
	// Name is the file's name, as the release lists it.
	Name string
	// URL is where it is downloaded from: always https.
	URL string
	// Size is its size in bytes as the source states it; 0 when the
	// source did not say. A download is bounded by what the caller
	// expects, not by this.
	Size int64
}

// Release is a published release of the product as a source lists it, made
// by NewRelease. Nothing in it is trusted: what is installed is judged
// from the signed lists of the release, not from this.
type Release struct {
	// Version is the release's version.
	Version Version
	// Published is when it was published, as the source says; zero when
	// it did not.
	Published time.Time
	// Prerelease is true when the source marks the release as one or its
	// version is a pre-release.
	Prerelease bool
	// NotesURL is the address of the release notes, https only; empty
	// when the source gave none that may be kept. The console shows it as
	// a link and never fetches it.
	NotesURL string
	// Assets are the release's files with an https address and a name
	// that is a file's, each name once, in the order the source listed
	// them.
	Assets []Asset
}

// NewRelease makes a Release of what a source said of one. What may not be
// kept is dropped, and never the release: notes or an asset whose address is
// not https (or not a plain absolute address with a host, no user
// information, no fragment, no control character, at most MaxURLLength), an
// asset whose name is not a file's name, an asset of a negative size, and
// every asset whose name is listed more than once - which of them is meant
// cannot be told. A release is a pre-release when the source says so or
// its version is one.
func NewRelease(version Version, published time.Time, prerelease bool, notesURL string, assets []Asset) Release {
	r := Release{Version: version, Published: published, Prerelease: prerelease || version.IsPrerelease()}
	if httpsURL(notesURL) {
		r.NotesURL = notesURL
	}
	count := map[string]int{}
	for _, a := range assets {
		count[a.Name]++
	}
	for _, a := range assets {
		if count[a.Name] != 1 || !fileName(a.Name) || !httpsURL(a.URL) || a.Size < 0 {
			continue
		}
		r.Assets = append(r.Assets, a)
	}
	return r
}

// Asset returns the release's asset of that name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// httpsURL reports whether s is an address that may be kept: absolute,
// https, with a host, no user information, no fragment, nothing but
// printable ASCII and at most MaxURLLength bytes.
func httpsURL(s string) bool {
	if s == "" || len(s) > MaxURLLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && strings.HasPrefix(s, "https://") && u.Opaque == "" && u.Host != "" &&
		u.User == nil && u.Fragment == "" && !strings.Contains(s, "#") && !strings.Contains(u.Host, "@")
}

// fileName reports whether s may name a file of a release: not empty, at
// most MaxAssetNameLength bytes of printable ASCII without a space, no
// slash or backslash, and not "." or "..".
func fileName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > MaxAssetNameLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// Channel is which releases a deployment is offered.
type Channel string

// The channels.
const (
	// ChannelStable offers releases only, never a pre-release.
	ChannelStable Channel = "stable"
	// ChannelPrerelease offers pre-releases as well.
	ChannelPrerelease Channel = "prerelease"
)

// ParseChannel reads a channel's name, exactly as written above. Anything
// else is refused, wrapping domain.ErrInvalidArgument.
func ParseChannel(s string) (Channel, error) {
	switch c := Channel(s); c {
	case ChannelStable, ChannelPrerelease:
		return c, nil
	}
	return "", fmt.Errorf("%w: the update channel is %q or %q", domain.ErrInvalidArgument, ChannelStable, ChannelPrerelease)
}

// admits reports whether a release is offered on the channel. A channel
// that is neither of the two is read as stable: the stricter.
func (c Channel) admits(r Release) bool {
	return c == ChannelPrerelease || !r.Prerelease
}

// Available returns the releases that are newer than current and offered on
// the channel, newest first, each version once (the first a source listed).
func Available(current Version, releases []Release, channel Channel) []Release {
	var out []Release
	for _, r := range releases {
		if r.Version.Compare(current) <= 0 || !channel.admits(r) {
			continue
		}
		if slices.ContainsFunc(out, func(o Release) bool { return o.Version == r.Version }) {
			continue
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b Release) int { return b.Version.Compare(a.Version) })
	return out
}

// Reason is why a version may not be installed. Its values are part of the
// management API's contract: never change one, only add.
type Reason string

// The reasons a target is refused, in the order Allowed asks.
const (
	// ReasonEqual: the target is the running version.
	ReasonEqual Reason = "equal"
	// ReasonOlder: the target is older than the running version. Nothing
	// is downgraded from the console.
	ReasonOlder Reason = "older"
	// ReasonPrereleaseOnStable: the target is a pre-release and the
	// channel is stable.
	ReasonPrereleaseOnStable Reason = "prerelease-on-stable"
	// ReasonMajor: the target is of another major version.
	ReasonMajor Reason = "major"
	// ReasonSkippedMinor: the target is more than one minor version on.
	ReasonSkippedMinor Reason = "skipped-minor"
	// ReasonNotNewestPatch: the target is of the next minor version and
	// not the newest of its patches on the channel.
	ReasonNotNewestPatch Reason = "not-newest-patch"
	// ReasonUnknownRelease: the target is of the next minor version and
	// not among the releases, so that it cannot be the newest of them.
	ReasonUnknownRelease Reason = "unknown-release"
)

// Refusal is the error of Allowed. It wraps domain.ErrUpgradeNotAllowed and
// says which rule refused, in words that name only versions.
type Refusal struct {
	// Reason is the rule that refused.
	Reason Reason
	// Current is the running version.
	Current Version
	// Target is the version that was asked for.
	Target Version
}

// Error implements error.
func (r *Refusal) Error() string {
	return fmt.Sprintf("%v: %s to %s: %s", domain.ErrUpgradeNotAllowed, r.Current, r.Target, r.Reason)
}

// Unwrap returns domain.ErrUpgradeNotAllowed.
func (r *Refusal) Unwrap() error { return domain.ErrUpgradeNotAllowed }

// ReasonOf returns the reason of a Refusal err wraps, or "" when it wraps
// none.
func ReasonOf(err error) Reason {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// Allowed decides whether target may be installed where current runs. Only
// two kinds of target are: a newer version of the running minor version
// (major and minor equal), and the newest version of the next minor
// version (same major) among the releases the channel offers - because
// version N runs alongside N+1, and nothing else is tested together. A
// pre-release is never allowed on the stable channel, whatever its numbers.
//
// Every refusal is a *Refusal wrapping domain.ErrUpgradeNotAllowed, with
// the first reason that holds in the order of the Reason constants.
func Allowed(current, target Version, releases []Release, channel Channel) error {
	refuse := func(reason Reason) error { return &Refusal{Reason: reason, Current: current, Target: target} }
	switch c := target.Compare(current); {
	case c == 0:
		return refuse(ReasonEqual)
	case c < 0:
		return refuse(ReasonOlder)
	}
	if target.IsPrerelease() && channel != ChannelPrerelease {
		return refuse(ReasonPrereleaseOnStable)
	}
	for _, r := range releases {
		if r.Version == target && !channel.admits(r) {
			return refuse(ReasonPrereleaseOnStable)
		}
	}
	switch {
	case target.major != current.major:
		return refuse(ReasonMajor)
	case target.minor == current.minor:
		return nil
	case target.minor != current.minor+1:
		return refuse(ReasonSkippedMinor)
	}
	known := false
	for _, r := range releases {
		if !target.SameMinor(r.Version) || !channel.admits(r) {
			continue
		}
		if r.Version == target {
			known = true
		}
		if r.Version.Compare(target) > 0 {
			return refuse(ReasonNotNewestPatch)
		}
	}
	if !known {
		return refuse(ReasonUnknownRelease)
	}
	return nil
}

// NextStep returns the release the deployment can go to next: the newest of
// Available that Allowed admits. The second result is false when there is
// none.
func NextStep(current Version, releases []Release, channel Channel) (Release, bool) {
	for _, r := range Available(current, releases, channel) {
		if Allowed(current, r.Version, releases, channel) == nil {
			return r, true
		}
	}
	return Release{}, false
}

// BinaryAsset returns the name of the release's binary for a platform, in
// the names of Go's GOOS and GOARCH: "cactusfs-<os>-<arch>", and ".exe" on
// windows. It returns "" for a platform no release is built for (the six of
// RELEASE_BINARIES in scripts/release.sh).
func BinaryAsset(goos, goarch string) string {
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return ""
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return ""
	}
	name := "cactusfs-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}
