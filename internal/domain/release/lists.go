package release

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// Bounds of the signed lists of a release. Both are a few hundred bytes; a
// list larger than this is not a release's.
const (
	// MaxListSize is the largest SHA256SUMS or digests.txt read.
	MaxListSize = 64 << 10
	// MaxListLines is the most lines either may have.
	MaxListLines = 256
)

// Sums is what SHA256SUMS says: the SHA-256 of each binary of a release,
// by the binary's name.
type Sums map[string][32]byte

// ParseSums reads SHA256SUMS as sha256sum writes it in text mode: one line
// per file, "<64 lower-case hex digits>  <name>" (two spaces), each ending
// in "\n" (the last may not). Anything else is refused, wrapping
// domain.ErrInvalidArgument: another shape of line (binary mode's " *", an
// escaped name, a carriage return, an empty line), a name that is no
// file's, a name listed twice, no line at all, more than MaxListLines lines
// or MaxListSize bytes. The error names the line by its number only.
func ParseSums(data []byte) (Sums, error) {
	lines, err := splitList(data)
	if err != nil {
		return nil, fmt.Errorf("release.ParseSums: %w", err)
	}
	sums := Sums{}
	for i, line := range lines {
		digest, name, ok := strings.Cut(line, "  ")
		sum, good := hexSum(digest)
		if !ok || !good || !fileName(name) {
			return nil, fmt.Errorf("release.ParseSums: %w: line %d is not \"<sha-256>  <name>\"", domain.ErrInvalidArgument, i+1)
		}
		if _, twice := sums[name]; twice {
			return nil, fmt.Errorf("release.ParseSums: %w: line %d names a file listed before", domain.ErrInvalidArgument, i+1)
		}
		sums[name] = sum
	}
	return sums, nil
}

// splitList checks the bounds of a list and returns its lines, without
// their "\n". A list has at least one line, and no line is empty.
func splitList(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: the list is empty", domain.ErrInvalidArgument)
	}
	if len(data) > MaxListSize {
		return nil, fmt.Errorf("%w: the list is larger than %d bytes", domain.ErrInvalidArgument, MaxListSize)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > MaxListLines {
		return nil, fmt.Errorf("%w: the list has more than %d lines", domain.ErrInvalidArgument, MaxListLines)
	}
	for i, line := range lines {
		if line == "" {
			return nil, fmt.Errorf("%w: line %d is empty", domain.ErrInvalidArgument, i+1)
		}
	}
	return lines, nil
}

// hexSum reads 64 lower-case hex digits.
func hexSum(s string) ([32]byte, bool) {
	var sum [32]byte
	if len(s) != 2*len(sum) || strings.ToLower(s) != s {
		return sum, false
	}
	if _, err := hex.Decode(sum[:], []byte(s)); err != nil {
		return [32]byte{}, false
	}
	return sum, true
}

// Image is what digests.txt says of the product's image: the manifest list
// that is installed by its digest, and the image of each platform in it.
type Image struct {
	// Repository is the image's repository as digests.txt names it, its
	// last element "cactusfs": "ghcr.io/albievan/cactusfs".
	Repository string
	// List is the digest of the manifest list: "sha256:<64 hex digits>".
	List string
	// Tags are the tags digests.txt gives the list, in the order of the
	// file: the release's "<version>-<build>" and "<version>".
	Tags []string
	// Platforms are the digests of the image of each platform, by
	// "<os>/<arch>": "linux/amd64".
	Platforms map[string]string
}

// Tagged reports whether digests.txt gives the list the tag of that
// version, exactly.
func (i Image) Tagged(v Version) bool {
	for _, tag := range i.Tags {
		if tag == v.String() {
			return true
		}
	}
	return false
}

// ParseDigests reads digests.txt as scripts/release.sh writes it, lines of
// two shapes:
//
//	<repository>:<tag> <digest>              an image list, or a chart
//	<repository>:<tag> <os>/<arch> <digest>  the image of a platform
//
// with a digest "sha256:<64 lower-case hex digits>". The product's image is
// the one repository that has lines of platforms; its last element must be
// "cactusfs", every list line of it must name the same digest, and each
// platform must be named once. Lines of other repositories - the chart's -
// are read for their shape and not returned.
//
// Anything else is refused, wrapping domain.ErrInvalidArgument: a line of
// another shape, a repository or tag of characters an image reference does
// not have, a list or platform given twice, no image or two of them, an
// image without a list, and the bounds of ParseSums. The error names the
// line by its number only.
func ParseDigests(data []byte) (Image, error) {
	lines, err := splitList(data)
	if err != nil {
		return Image{}, fmt.Errorf("release.ParseDigests: %w", err)
	}
	type entry struct {
		repository, tag, platform, digest string
	}
	entries := make([]entry, 0, len(lines))
	image := ""
	for i, line := range lines {
		fields := strings.Split(line, " ")
		var e entry
		ok := false
		switch len(fields) {
		case 2:
			e.digest = fields[1]
			ok = true
		case 3:
			e.platform, e.digest = fields[1], fields[2]
			ok = platform(e.platform)
		}
		if ok {
			e.repository, e.tag, ok = reference(fields[0])
		}
		if !ok || !digest(e.digest) {
			return Image{}, fmt.Errorf("release.ParseDigests: %w: line %d is not \"<repository>:<tag> [<os>/<arch>] <digest>\"", domain.ErrInvalidArgument, i+1)
		}
		if e.platform != "" {
			if image != "" && image != e.repository {
				return Image{}, fmt.Errorf("release.ParseDigests: %w: line %d names the platforms of a second image", domain.ErrInvalidArgument, i+1)
			}
			image = e.repository
		}
		entries = append(entries, e)
	}
	if image == "" {
		return Image{}, fmt.Errorf("release.ParseDigests: %w: no image has platforms", domain.ErrInvalidArgument)
	}
	if image != "cactusfs" && !strings.HasSuffix(image, "/cactusfs") {
		return Image{}, fmt.Errorf("release.ParseDigests: %w: the image is not of a repository named cactusfs", domain.ErrInvalidArgument)
	}
	out := Image{Repository: image, Platforms: map[string]string{}}
	for i, e := range entries {
		switch {
		case e.repository != image:
			continue
		case e.platform != "":
			if _, twice := out.Platforms[e.platform]; twice {
				return Image{}, fmt.Errorf("release.ParseDigests: %w: line %d names a platform named before", domain.ErrInvalidArgument, i+1)
			}
			out.Platforms[e.platform] = e.digest
		default:
			if out.List != "" && out.List != e.digest {
				return Image{}, fmt.Errorf("release.ParseDigests: %w: line %d names a second list", domain.ErrInvalidArgument, i+1)
			}
			for _, tag := range out.Tags {
				if tag == e.tag {
					return Image{}, fmt.Errorf("release.ParseDigests: %w: line %d names a tag named before", domain.ErrInvalidArgument, i+1)
				}
			}
			out.List = e.digest
			out.Tags = append(out.Tags, e.tag)
		}
	}
	if out.List == "" {
		return Image{}, fmt.Errorf("release.ParseDigests: %w: the image has no list", domain.ErrInvalidArgument)
	}
	return out, nil
}

// reference splits "<repository>:<tag>": the tag is after the last colon,
// which comes after the last slash (a registry may name a port). A
// repository is lower-case letters, digits and ". _ - / :", its elements
// not empty; a tag is letters, digits and ". _ -", at most 128, as OCI
// says.
func reference(s string) (repository, tag string, ok bool) {
	colon := strings.LastIndexByte(s, ':')
	if colon < 0 || colon < strings.LastIndexByte(s, '/') {
		return "", "", false
	}
	repository, tag = s[:colon], s[colon+1:]
	if tag == "" || len(tag) > 128 || repository == "" || len(repository) > 255 {
		return "", "", false
	}
	for i := 0; i < len(tag); i++ {
		if c := tag[i]; !lowerOrDigit(c) && (c < 'A' || c > 'Z') && c != '.' && c != '_' && c != '-' {
			return "", "", false
		}
	}
	for _, element := range strings.Split(repository, "/") {
		if element == "" {
			return "", "", false
		}
	}
	for i := 0; i < len(repository); i++ {
		if c := repository[i]; !lowerOrDigit(c) && c != '.' && c != '_' && c != '-' && c != '/' && c != ':' {
			return "", "", false
		}
	}
	return repository, tag, true
}

// platform reports whether s is "<os>/<arch>", each of lower-case letters
// and digits.
func platform(s string) bool {
	goos, arch, ok := strings.Cut(s, "/")
	return ok && word(goos) && word(arch)
}

func word(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !lowerOrDigit(s[i]) {
			return false
		}
	}
	return true
}

func lowerOrDigit(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

// digest reports whether s is "sha256:<64 lower-case hex digits>".
func digest(s string) bool {
	sum, ok := strings.CutPrefix(s, "sha256:")
	if !ok {
		return false
	}
	_, ok = hexSum(sum)
	return ok
}
