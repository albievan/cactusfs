// Package sigstore verifies that a file of a release - SHA256SUMS or
// digests.txt - was signed by the release workflow of CactusFS at the tag of
// that release (PLAN P.4, ADR-0029).
//
// A release signs those files keyless (scripts/release.sh --publish): a
// short-lived certificate from Fulcio for the identity of the workflow
//
//	https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v<version>
//
// issued to GitHub's OIDC issuer, an entry in Rekor, and the whole written as
// a Sigstore bundle (<file>.sigstore.json). Verify checks such a bundle
// OFFLINE against a trusted root - by default the one of Sigstore's
// public-good instance embedded in the binary (trusted_root.json, refreshed
// by `make trusted-root`) - with sigstore-go, never with code of our own:
//
//   - the certificate chains to a Fulcio CA of the root, and carries an
//     embedded SCT of a CT log of the root;
//   - the bundle has an entry in a transparency log of the root, verified by
//     its inclusion proof or promise;
//   - the time of signing is observed by that log or a timestamp authority of
//     the root, and lies within the certificate's validity;
//   - the signature is over exactly the bytes handed in;
//   - the issuer of the identity is GitHub's, and the identity is the release
//     workflow at the tag v<version> - exactly, so that a release signed for
//     one version is never taken for another.
//
// Nothing is fetched: no TUF refresh, no query of Rekor. A refusal wraps
// domain.ErrReleaseNotVerified and says why; it never repeats the bytes it
// was handed.
package sigstore

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/albievan/cactusfs/internal/domain"
)

// The identity a release is signed as (.github/workflows/release.yml,
// scripts/release.sh).
const (
	// DefaultIssuer is the OIDC issuer of GitHub Actions, which Fulcio
	// writes into the certificate of a release.
	DefaultIssuer = "https://token.actions.githubusercontent.com"
	// DefaultIdentityPrefix is the identity of the release workflow up to
	// the version: the certificate of release <version> names exactly
	// DefaultIdentityPrefix + <version>.
	DefaultIdentityPrefix = "https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v"
)

// MaxBundle is the largest bundle Verify reads. A bundle of one signature
// is a few KiB.
const MaxBundle = 1 << 20

//go:embed trusted_root.json
var publicGoodRoot []byte

// PublicGoodTrustedRoot returns the trusted root of Sigstore's public-good
// instance that the binary embeds.
func PublicGoodTrustedRoot() []byte { return bytes.Clone(publicGoodRoot) }

// Verifier verifies the bundles of releases. It is safe for concurrent use.
type Verifier struct {
	verifier       *verify.Verifier
	issuer         string
	identityPrefix string
}

// config is what the options set.
type config struct {
	issuer         string
	identityPrefix string
}

// Option changes whom a Verifier believes.
type Option func(*config)

// WithIssuer sets the OIDC issuer the certificate must name, instead of
// DefaultIssuer.
func WithIssuer(issuer string) Option {
	return func(c *config) { c.issuer = issuer }
}

// WithIdentityPrefix sets the identity, up to the version, the certificate
// must name, instead of DefaultIdentityPrefix.
func WithIdentityPrefix(prefix string) Option {
	return func(c *config) { c.identityPrefix = prefix }
}

// Default returns the Verifier of the releases of CactusFS: the embedded
// public-good trusted root, DefaultIssuer and DefaultIdentityPrefix.
func Default() (*Verifier, error) {
	return NewVerifier(publicGoodRoot)
}

// NewVerifier returns a Verifier that believes the trusted root
// trustedRootJSON (the trusted_root.json of a Sigstore instance) and the
// identity of the options, by default the release workflow of CactusFS.
func NewVerifier(trustedRootJSON []byte, opts ...Option) (*Verifier, error) {
	cfg := config{issuer: DefaultIssuer, identityPrefix: DefaultIdentityPrefix}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.issuer == "" || cfg.identityPrefix == "" {
		return nil, errors.New("sigstore.NewVerifier: the issuer and the identity must not be empty")
	}
	trusted, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return nil, fmt.Errorf("sigstore.NewVerifier: the trusted root: %w", err)
	}
	verifier, err := verify.NewVerifier(trusted,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
		verify.WithSignedCertificateTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("sigstore.NewVerifier: %w", err)
	}
	return &Verifier{verifier: verifier, issuer: cfg.issuer, identityPrefix: cfg.identityPrefix}, nil
}

// Identity returns the identity the certificate of release version must
// name.
func (v *Verifier) Identity(version string) string { return v.identityPrefix + version }

// Verify returns nil when bundleJSON is a Sigstore bundle of a signature over
// exactly signed, made by the release workflow at the tag v<version>, that
// verifies against the trusted root; otherwise an error that wraps
// domain.ErrReleaseNotVerified, or the error of ctx when ctx has ended.
// version is the release without the "v" ("0.7.1", "0.8.0-rc.1").
func (v *Verifier) Verify(ctx context.Context, version string, signed, bundleJSON []byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sigstore.Verify: %w", err)
	}
	if !plausibleVersion(version) {
		return fmt.Errorf("sigstore.Verify: %w: the version is not of the form X.Y.Z[-pre]", domain.ErrReleaseNotVerified)
	}
	if len(bundleJSON) > MaxBundle {
		return fmt.Errorf("sigstore.Verify: %w: the bundle is larger than %d bytes", domain.ErrReleaseNotVerified, MaxBundle)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("sigstore.Verify: %w: not a Sigstore bundle: %w", domain.ErrReleaseNotVerified, err)
	}
	// A release signs a file (cosign sign-blob): a message signature. An
	// attestation (DSSE) is something else and is not taken for it.
	if b.GetMessageSignature() == nil {
		return fmt.Errorf("sigstore.Verify: %w: the bundle holds no message signature", domain.ErrReleaseNotVerified)
	}
	identity, err := verify.NewShortCertificateIdentity(v.issuer, "", v.Identity(version), "")
	if err != nil {
		return fmt.Errorf("sigstore.Verify: %w: %w", domain.ErrReleaseNotVerified, err)
	}
	policy := verify.NewPolicy(verify.WithArtifact(bytes.NewReader(signed)), verify.WithCertificateIdentity(identity))
	if _, err := v.verifier.Verify(&b, policy); err != nil {
		return fmt.Errorf("sigstore.Verify: %w: %w", domain.ErrReleaseNotVerified, err)
	}
	return nil
}

// plausibleVersion reports whether version can be the X.Y.Z[-pre] of a tag:
// it starts with a digit and holds only what semver allows in a version
// without build metadata. Whether it is a valid version is the domain's to
// say; this keeps a stray "v", "/" or "@" out of the identity.
func plausibleVersion(version string) bool {
	if version == "" || len(version) > 128 || version[0] < '0' || version[0] > '9' {
		return false
	}
	for _, r := range version {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}
