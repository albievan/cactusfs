// Package release decides what a deployment may be upgraded to (ADR-0029):
// the versions of releases and their order, which of the releases a source
// lists are newer and on the channel the operator chose, which of those may
// be installed from the running version, and how the signed lists of a
// release - SHA256SUMS of the binaries, digests.txt of the image - are read.
//
// It decides only. Nothing here asks a server, checks a signature or writes
// a file: a release is fetched through ports.ReleaseSource, its bundle is
// verified through ports.ReleaseVerifier, and it is installed through
// ports.Upgrader. What this package adds is that an artifact is installed
// only as a Verified, which is made from the signed list and the digest of
// the bytes that are to be installed, and from nothing else.
package release

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// MaxVersionLength is the longest text ParseVersion reads. A version of the
// product is a dozen bytes; the bound is there so that a source that lies
// cannot make a version that costs to compare.
const MaxVersionLength = 128

// Version is a version of the product as semantic versioning 2.0 defines
// it: major.minor.patch and an optional pre-release. Build metadata is not
// part of it - the product's own build number is reported beside its
// version, never in it. The zero Version is 0.0.0, which no release has; a
// Version is made by ParseVersion.
//
// Versions are comparable with ==, which agrees with Compare: two versions
// are equal exactly when their text is.
type Version struct {
	major, minor, patch uint64
	// pre is the pre-release, without its "-": dot-separated identifiers,
	// each checked by ParseVersion. Empty for a release.
	pre string
}

// ParseVersion reads a version from "X.Y.Z" or "X.Y.Z-pre", with or without
// a leading "v" (the form of a release's tag). It refuses, wrapping
// domain.ErrInvalidArgument: build metadata ("+..."), a number with a
// leading zero or past 2^64-1, an empty or malformed pre-release identifier,
// a numeric identifier with a leading zero, and anything longer than
// MaxVersionLength. The error never repeats the text: it came from outside.
func ParseVersion(s string) (Version, error) {
	if len(s) > MaxVersionLength {
		return Version{}, fmt.Errorf("%w: a version is at most %d bytes", domain.ErrInvalidArgument, MaxVersionLength)
	}
	text := strings.TrimPrefix(s, "v")
	if strings.Contains(text, "+") {
		return Version{}, fmt.Errorf("%w: a version with build metadata is not taken", domain.ErrInvalidArgument)
	}
	core, pre, hasPre := strings.Cut(text, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: a version is major.minor.patch", domain.ErrInvalidArgument)
	}
	var numbers [3]uint64
	for i, part := range parts {
		n, ok := numeric(part)
		if !ok {
			return Version{}, fmt.Errorf("%w: a part of a version is a number without a leading zero", domain.ErrInvalidArgument)
		}
		numbers[i] = n
	}
	if hasPre {
		if err := checkPrerelease(pre); err != nil {
			return Version{}, err
		}
	}
	return Version{major: numbers[0], minor: numbers[1], patch: numbers[2], pre: pre}, nil
}

// numeric reads a numeric identifier: digits, no leading zero unless it is
// "0", and no larger than a uint64.
func numeric(s string) (uint64, bool) {
	if s == "" || !allDigits(s) || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// checkPrerelease checks the identifiers of a pre-release: each is not
// empty, of ASCII letters, digits and hyphens only, and when it is all
// digits it has no leading zero.
func checkPrerelease(pre string) error {
	if pre == "" {
		return fmt.Errorf("%w: a pre-release is not empty", domain.ErrInvalidArgument)
	}
	for _, id := range strings.Split(pre, ".") {
		if id == "" {
			return fmt.Errorf("%w: an identifier of a pre-release is not empty", domain.ErrInvalidArgument)
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
				return fmt.Errorf("%w: an identifier of a pre-release is of letters, digits and hyphens", domain.ErrInvalidArgument)
			}
		}
		if allDigits(id) && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("%w: a numeric identifier of a pre-release has no leading zero", domain.ErrInvalidArgument)
		}
	}
	return nil
}

// Major returns the major version.
func (v Version) Major() uint64 { return v.major }

// Minor returns the minor version.
func (v Version) Minor() uint64 { return v.minor }

// Patch returns the patch version.
func (v Version) Patch() uint64 { return v.patch }

// Prerelease returns the pre-release without its "-", or "" for a release.
func (v Version) Prerelease() string { return v.pre }

// IsPrerelease reports whether the version is a pre-release.
func (v Version) IsPrerelease() bool { return v.pre != "" }

// IsZero reports whether v is the zero Version, which no ParseVersion of a
// release returns but "0.0.0".
func (v Version) IsZero() bool { return v == Version{} }

// String returns the version as "X.Y.Z" or "X.Y.Z-pre", without a "v".
func (v Version) String() string {
	s := strconv.FormatUint(v.major, 10) + "." + strconv.FormatUint(v.minor, 10) + "." + strconv.FormatUint(v.patch, 10)
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}

// Tag returns the name of the version's tag: "v" and its String.
func (v Version) Tag() string { return "v" + v.String() }

// SameMinor reports whether v and w have the same major and minor version.
func (v Version) SameMinor(w Version) bool { return v.major == w.major && v.minor == w.minor }

// Compare returns -1 when v precedes w, 0 when they are equal and +1 when v
// follows w, by the precedence of semantic versioning 2.0: major, minor and
// patch as numbers; a pre-release precedes the release of the same numbers;
// two pre-releases by their identifiers from the left, numeric ones as
// numbers and before any alphanumeric one, alphanumeric ones in ASCII order,
// and a shorter list of identifiers first when it is a prefix of the other.
func (v Version) Compare(w Version) int {
	if c := compareUint(v.major, w.major); c != 0 {
		return c
	}
	if c := compareUint(v.minor, w.minor); c != 0 {
		return c
	}
	if c := compareUint(v.patch, w.patch); c != 0 {
		return c
	}
	switch {
	case v.pre == w.pre:
		return 0
	case v.pre == "":
		return 1
	case w.pre == "":
		return -1
	}
	a, b := strings.Split(v.pre, "."), strings.Split(w.pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	return compareUint(uint64(len(a)), uint64(len(b)))
}

// Compare is v.Compare(w), for slices.SortFunc and the like.
func Compare(v, w Version) int { return v.Compare(w) }

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareIdentifier compares two identifiers of a pre-release. Numeric ones
// have no leading zero, so the longer is the larger, and of the same length
// the order of their text is the order of their values - whatever their
// size.
func compareIdentifier(a, b string) int {
	an, bn := allDigits(a), allDigits(b)
	switch {
	case an && bn:
		if c := compareUint(uint64(len(a)), uint64(len(b))); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}
