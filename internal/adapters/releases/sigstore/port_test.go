package sigstore_test

import (
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestReleaseVerifierContract: the verifier keeps the promises of
// ports.ReleaseVerifier, with bundles a release workflow at the tag would
// have made.
func TestReleaseVerifierContract(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	verifier := sigstore.NewReleaseVerifier(newVerifier(t, f.root))
	porttest.ReleaseVerifierContract(t, verifier, func(t *testing.T, version release.Version, signed []byte) []byte {
		t.Helper()
		if want := "v" + version.String(); version.Tag() != want {
			t.Fatalf("Tag = %q, want %q", version.Tag(), want)
		}
		return sign(t, f.signer, identity(version.String()), sigstore.DefaultIssuer, signed)
	})
}

// TestReleaseVerifierRefusesTheZeroVersion: no release is 0.0.0, and none
// is verified for it.
func TestReleaseVerifierRefusesTheZeroVersion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	verifier := sigstore.NewReleaseVerifier(newVerifier(t, f.root))
	b := sign(t, f.signer, identity("0.0.0"), sigstore.DefaultIssuer, sums)
	if err := verifier.Verify(t.Context(), release.Version{}, sums, b); !errors.Is(err, domain.ErrReleaseNotVerified) {
		t.Fatalf("Verify of the zero version = %v, want ErrReleaseNotVerified", err)
	}
}
