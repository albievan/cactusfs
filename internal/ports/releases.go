package ports

import (
	"context"
	"io"

	"github.com/albievan/cactusfs/internal/domain/release"
)

// ReleaseSource is where a deployment learns of releases and fetches their
// files: the update source an operator configured (ADR-0029). Nothing it
// says is trusted; what is installed is judged from a release's signed
// lists by ReleaseVerifier and the domain.
type ReleaseSource interface {
	// Releases returns the releases the source lists, each made by
	// release.NewRelease (no address but https is ever returned), in the
	// order the source gave them. A release whose version does not parse
	// is left out, never the call failed for it. An error wraps
	// domain.ErrUnavailable when the source could not be reached or
	// refused to answer for now (a limit of its rate), and the error of
	// ctx when ctx ended. The error names the source by no more than its
	// host: its address may hold a credential.
	Releases(ctx context.Context) ([]release.Release, error)
	// Fetch writes the asset's bytes to w, at most limit of them. When the
	// asset is larger than limit - by what the source declares, or by what
	// it sends - it fails wrapping domain.ErrEntityTooLarge, before
	// writing more than limit bytes. An asset whose address is not https
	// is refused wrapping domain.ErrInvalidArgument, and nothing is asked.
	// Other errors are those of Releases. On any error, what was written
	// to w is to be thrown away.
	Fetch(ctx context.Context, asset release.Asset, limit int64, w io.Writer) error
}

// ReleaseVerifier verifies that a signed list of a release - SHA256SUMS or
// digests.txt - was signed by the release workflow of this product for the
// tag of that version, from its Sigstore bundle, offline. It is the only
// place the trust in a release comes from; a release.Verified is made only
// from bytes it has accepted.
type ReleaseVerifier interface {
	// Verify returns nil when bundle is a signature of signed by the
	// release workflow at the tag of version (release.Version.Tag). Any
	// other outcome - a bundle that does not parse, of other bytes, of
	// another tag or workflow, a certificate or log entry that does not
	// verify against the trusted root - is an error wrapping
	// domain.ErrReleaseNotVerified; the error of ctx when ctx ended.
	Verify(ctx context.Context, version release.Version, signed, bundle []byte) error
}

// Upgrader installs a verified release on the deployment, in the way its
// mode does (release.Mode): it replaces the binary, or moves the nodes to
// the release's image one at a time. One upgrade runs at a time; how far it
// has come is read from where it happens, not kept in the metadata.
type Upgrader interface {
	// Mode returns how this upgrader installs.
	Mode() release.Mode
	// Availability reports whether an upgrade can be started here. When it
	// cannot, reason says why in words that name no secret, and
	// instructions are what an operator does instead, as text to copy;
	// both may also be given when it can.
	Availability(ctx context.Context) (ok bool, reason string, instructions []string)
	// Start begins installing target and returns once it has begun, or
	// with an error and nothing begun: domain.ErrUpgradeUnavailable when
	// Availability says no, domain.ErrUpgradeRunning when an upgrade is
	// under way, domain.ErrReleaseNotVerified for the zero Verified, and
	// domain.ErrInvalidArgument for an artifact of a kind this upgrader
	// does not install (an image for a binary). startedBy is the actor
	// who started it, as the audit trail names actors. The upgrade goes
	// on after Start has returned and whatever becomes of ctx, which
	// bounds the start only; report is called, from another goroutine,
	// with the progress every time it changes, the last time with a state
	// that has ended - and EndedElsewhere when another node recorded that
	// end (release.RunEnd), which its caller then does not record again.
	Start(ctx context.Context, target release.Verified, startedBy string, report func(release.Progress)) error
	// Progress returns how far the latest upgrade has come, normalized;
	// release.StateIdle when none was started.
	Progress(ctx context.Context) (release.Progress, error)
}
