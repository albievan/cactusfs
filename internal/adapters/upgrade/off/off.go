// Package off is the upgrader of upgrade-mode off, the default (ADR-0029):
// it installs nothing. What it does is say so, and give the operator the
// commands of the way the deployment is upgraded by hand - a binary to
// download and verify, the helm upgrade of a chart, or the image of a
// compose project - as text to copy.
package off

import (
	"context"
	"fmt"
	"slices"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/binary"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// Reason is why an upgrade cannot be started from the console: always the
// same, and naming the setting that would change it.
const Reason = "installing from the console is off (upgrade-mode)"

// Upgrader is the ports.Upgrader of upgrade-mode off.
type Upgrader struct {
	instructions []string
}

var _ ports.Upgrader = (*Upgrader)(nil)

// New returns the upgrader that installs nothing and gives instructions
// instead: those of the deployment's kind (binary.Instructions,
// kubernetes.Instructions, ImageInstructions).
func New(instructions []string) *Upgrader {
	return &Upgrader{instructions: slices.Clone(instructions)}
}

// ImageInstructions are the commands that upgrade a deployment of the
// container image that is not made by the chart - a compose project: verify
// the release's signed digests.txt and move each node to the image list it
// names, one node at a time.
func ImageInstructions() []string {
	return []string{
		"# the release to install, without the leading v",
		"VERSION=<version>",
		`BASE="` + binary.DownloadBase + `$VERSION"`,
		`curl -fL -O "$BASE/` + release.DigestsAsset + `" -O "$BASE/` + release.DigestsBundleAsset + `"`,
		"# the digests are signed by the release workflow at the tag of the version",
		"cosign verify-blob --new-bundle-format=true --bundle " + release.DigestsBundleAsset +
			` --certificate-identity "` + sigstore.DefaultIdentityPrefix + `$VERSION"` +
			" --certificate-oidc-issuer " + sigstore.DefaultIssuer + " " + release.DigestsAsset,
		"# set the image of the nodes to ghcr.io/albievan/cactusfs@<the digest of the image list in " + release.DigestsAsset + ">,",
		"# then recreate them one at a time, each once the one before it is ready (docker compose up -d --no-deps <node>)",
	}
}

// Mode implements ports.Upgrader: release.ModeOff.
func (u *Upgrader) Mode() release.Mode { return release.ModeOff }

// Availability implements ports.Upgrader: never, with Reason and the
// instructions.
func (u *Upgrader) Availability(context.Context) (ok bool, reason string, instructions []string) {
	return false, Reason, slices.Clone(u.instructions)
}

// Start implements ports.Upgrader: it refuses, wrapping
// domain.ErrUpgradeUnavailable, and begins nothing.
func (u *Upgrader) Start(context.Context, release.Verified, string, func(release.Progress)) error {
	return fmt.Errorf("off.Start: %w: %s", domain.ErrUpgradeUnavailable, Reason)
}

// Progress implements ports.Upgrader: no upgrade is ever started.
func (u *Upgrader) Progress(ctx context.Context) (release.Progress, error) {
	if err := ctx.Err(); err != nil {
		return release.Progress{}, fmt.Errorf("off.Progress: %w", err)
	}
	return release.Progress{State: release.StateIdle}, nil
}
