package sigstore

import (
	"context"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// ReleaseVerifier is a Verifier as ports.ReleaseVerifier: the version is
// the domain's, and its tag is the one the identity names.
type ReleaseVerifier struct {
	verifier *Verifier
}

var _ ports.ReleaseVerifier = (*ReleaseVerifier)(nil)

// NewReleaseVerifier returns verifier as a ports.ReleaseVerifier.
func NewReleaseVerifier(verifier *Verifier) *ReleaseVerifier {
	return &ReleaseVerifier{verifier: verifier}
}

// Verify implements ports.ReleaseVerifier: Verifier.Verify for the version
// of the tag version.Tag(). The zero version, which no release has, is
// refused.
func (r *ReleaseVerifier) Verify(ctx context.Context, version release.Version, signed, bundle []byte) error {
	if version.IsZero() {
		return fmt.Errorf("sigstore.Verify: %w: no version", domain.ErrReleaseNotVerified)
	}
	return r.verifier.Verify(ctx, version.String(), signed, bundle)
}
