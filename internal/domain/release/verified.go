package release

import (
	"crypto/subtle"
	"fmt"
	"maps"

	"github.com/albievan/cactusfs/internal/domain"
)

// Kind is what a Verified installs.
type Kind string

// The kinds of artifact.
const (
	// KindBinary is the binary of one platform, named by SHA256SUMS.
	KindBinary Kind = "binary"
	// KindImage is the image's manifest list, named by digests.txt.
	KindImage Kind = "image"
)

// Verified is an artifact of a release that may be installed: the only
// thing ports.Upgrader takes. It is made by NewVerifiedBinary or
// NewVerifiedImage, which hold the artifact to the signed list of its
// release; the zero Verified is none, and is refused.
//
// What this package cannot check is the signature of the list itself. A
// Verified is made only by a caller that has first had the list's Sigstore
// bundle verified for this very version through ports.ReleaseVerifier
// (app.UpdateService): the list passed here must be the bytes the verifier
// accepted. Nothing else in the product makes one.
type Verified struct {
	version Version
	kind    Kind
	asset   string
	sha256  [32]byte
	image   Image
}

// NewVerifiedBinary holds a binary to SHA256SUMS. sums is the SHA256SUMS of
// release version whose bundle verified; name is the binary's asset name
// (BinaryAsset); sum is the SHA-256 the caller computed of the bytes it
// holds and will install. It refuses, wrapping domain.ErrReleaseNotVerified,
// a version that is zero, a list that does not parse, a name the list does
// not have, and a sum that is not the listed one.
func NewVerifiedBinary(version Version, sums []byte, name string, sum [32]byte) (Verified, error) {
	if version.IsZero() {
		return Verified{}, fmt.Errorf("release.NewVerifiedBinary: %w: no version", domain.ErrReleaseNotVerified)
	}
	listed, err := ParseSums(sums)
	if err != nil {
		return Verified{}, fmt.Errorf("release.NewVerifiedBinary: %w: %w", domain.ErrReleaseNotVerified, err)
	}
	want, ok := listed[name]
	if !ok {
		return Verified{}, fmt.Errorf("release.NewVerifiedBinary: %w: %s lists no binary of that name", domain.ErrReleaseNotVerified, SumsAsset)
	}
	if subtle.ConstantTimeCompare(want[:], sum[:]) != 1 {
		return Verified{}, fmt.Errorf("release.NewVerifiedBinary: %w: the binary is not the one %s lists", domain.ErrReleaseNotVerified, SumsAsset)
	}
	return Verified{version: version, kind: KindBinary, asset: name, sha256: sum}, nil
}

// NewVerifiedImage holds the image to digests.txt. digests is the
// digests.txt of release version whose bundle verified. It refuses,
// wrapping domain.ErrReleaseNotVerified, a version that is zero, a list
// that does not parse, and a list that does not give the image the tag of
// the version - the line release.sh writes last, once the list is signed.
func NewVerifiedImage(version Version, digests []byte) (Verified, error) {
	if version.IsZero() {
		return Verified{}, fmt.Errorf("release.NewVerifiedImage: %w: no version", domain.ErrReleaseNotVerified)
	}
	image, err := ParseDigests(digests)
	if err != nil {
		return Verified{}, fmt.Errorf("release.NewVerifiedImage: %w: %w", domain.ErrReleaseNotVerified, err)
	}
	if !image.Tagged(version) {
		return Verified{}, fmt.Errorf("release.NewVerifiedImage: %w: %s does not tag the image %s", domain.ErrReleaseNotVerified, DigestsAsset, version)
	}
	return Verified{version: version, kind: KindImage, image: image}, nil
}

// IsZero reports whether v is the zero Verified, which is none.
func (v Verified) IsZero() bool { return v.kind == "" }

// Version returns the release the artifact is of.
func (v Verified) Version() Version { return v.version }

// Kind returns what the artifact is.
func (v Verified) Kind() Kind { return v.kind }

// Asset returns the name of the binary; "" for an image.
func (v Verified) Asset() string { return v.asset }

// SHA256 returns the SHA-256 of the binary; zero for an image.
func (v Verified) SHA256() [32]byte { return v.sha256 }

// Image returns what digests.txt says of the image, a copy; the zero Image
// for a binary.
func (v Verified) Image() Image {
	out := v.image
	out.Tags = append([]string(nil), v.image.Tags...)
	out.Platforms = maps.Clone(v.image.Platforms)
	return out
}

// Digest returns what is installed, as the audit trail records it:
// "sha256:<hex>" of the binary, or the digest of the image's list.
func (v Verified) Digest() string {
	switch v.kind {
	case KindBinary:
		return fmt.Sprintf("sha256:%x", v.sha256)
	case KindImage:
		return v.image.List
	}
	return ""
}
