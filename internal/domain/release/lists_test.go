package release_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
)

func hexOf(b byte) string { return strings.Repeat(fmt.Sprintf("%02x", b), 32) }

func sumOf(b byte) [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = b
	}
	return s
}

// sumsFile is SHA256SUMS as sha256sum writes it for the six binaries.
func sumsFile() string {
	var b strings.Builder
	for i, name := range []string{
		"cactusfs-darwin-amd64", "cactusfs-darwin-arm64", "cactusfs-linux-amd64",
		"cactusfs-linux-arm64", "cactusfs-windows-amd64.exe", "cactusfs-windows-arm64.exe",
	} {
		fmt.Fprintf(&b, "%s  %s\n", hexOf(byte(0xa0+i)), name)
	}
	return b.String()
}

func TestParseSums(t *testing.T) {
	t.Parallel()
	sums, err := release.ParseSums([]byte(sumsFile()))
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 6 || sums["cactusfs-linux-amd64"] != sumOf(0xa2) || sums["cactusfs-windows-arm64.exe"] != sumOf(0xa5) {
		t.Errorf("ParseSums = %x", sums)
	}
	// The last line may lack its newline.
	if s, err := release.ParseSums([]byte(hexOf(1) + "  a")); err != nil || s["a"] != sumOf(1) {
		t.Errorf("without a final newline: %v, %v", s, err)
	}
	bad := map[string]string{
		"empty":                     "",
		"a newline alone":           "\n",
		"an empty line":             hexOf(1) + "  a\n\n" + hexOf(2) + "  b\n",
		"binary mode":               hexOf(1) + " *a\n",
		"one space":                 hexOf(1) + " a\n",
		"three spaces":              hexOf(1) + "   a\n",
		"a tab":                     hexOf(1) + "\ta\n",
		"upper-case hex":            strings.ToUpper(hexOf(0xab)) + "  a\n",
		"short hex":                 hexOf(1)[:63] + "  a\n",
		"long hex":                  hexOf(1) + "0  a\n",
		"not hex":                   strings.Repeat("g", 64) + "  a\n",
		"an escaped name":           "\\" + hexOf(1) + "  a\\nb\n",
		"a carriage return":         hexOf(1) + "  a\r\n",
		"a path":                    hexOf(1) + "  dist/a\n",
		"a parent":                  hexOf(1) + "  ..\n",
		"a backslash":               hexOf(1) + "  a\\b\n",
		"no name":                   hexOf(1) + "  \n",
		"a name with a space":       hexOf(1) + "  a b\n",
		"a name listed twice":       hexOf(1) + "  a\n" + hexOf(1) + "  a\n",
		"twice with other sums":     hexOf(1) + "  a\n" + hexOf(2) + "  a\n",
		"a BSD line":                "SHA256 (a) = " + hexOf(1) + "\n",
		"too many lines":            strings.Repeat(hexOf(1)+"  a\n", 1) + manyLines(release.MaxListLines),
		"larger than a list may be": hexOf(1) + "  " + strings.Repeat("a", release.MaxListSize) + "\n",
	}
	for name, data := range bad {
		if s, err := release.ParseSums([]byte(data)); !errors.Is(err, domain.ErrInvalidArgument) || s != nil {
			t.Errorf("%s: ParseSums = %v, %v", name, s, err)
		}
	}
}

// manyLines is that many lines of distinct names.
func manyLines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s  f%d\n", hexOf(byte(i)), i)
	}
	return b.String()
}

// digestsFile is digests.txt as scripts/release.sh --publish writes it.
func digestsFile() string {
	return "ghcr.io/albievan/cactusfs:0.7.1-87 sha256:" + hexOf(0x11) + "\n" +
		"ghcr.io/albievan/cactusfs:0.7.1-87 linux/amd64 sha256:" + hexOf(0x22) + "\n" +
		"ghcr.io/albievan/cactusfs:0.7.1-87 linux/arm64 sha256:" + hexOf(0x33) + "\n" +
		"ghcr.io/albievan/charts/cactusfs:0.7.1 sha256:" + hexOf(0x44) + "\n" +
		"ghcr.io/albievan/cactusfs:0.7.1 sha256:" + hexOf(0x11) + "\n"
}

func TestParseDigests(t *testing.T) {
	t.Parallel()
	image, err := release.ParseDigests([]byte(digestsFile()))
	if err != nil {
		t.Fatal(err)
	}
	want := release.Image{
		Repository: "ghcr.io/albievan/cactusfs",
		List:       "sha256:" + hexOf(0x11),
		Tags:       []string{"0.7.1-87", "0.7.1"},
		Platforms:  map[string]string{"linux/amd64": "sha256:" + hexOf(0x22), "linux/arm64": "sha256:" + hexOf(0x33)},
	}
	if !reflect.DeepEqual(image, want) {
		t.Errorf("ParseDigests =\n%+v\nwant\n%+v", image, want)
	}
	if !image.Tagged(mustVersion(t, "0.7.1")) || image.Tagged(mustVersion(t, "0.7.2")) || image.Tagged(mustVersion(t, "0.7.1-rc.1")) {
		t.Error("Tagged")
	}
	// The rehearsal's file: a repository of one element, a registry with
	// a port, and no tag of the version.
	rehearsal := "localhost:5000/cactusfs:0.7.1-87 sha256:" + hexOf(1) + "\n" +
		"localhost:5000/cactusfs:0.7.1-87 linux/amd64 sha256:" + hexOf(2) + "\n"
	if image, err := release.ParseDigests([]byte(rehearsal)); err != nil || image.Repository != "localhost:5000/cactusfs" || image.Tagged(mustVersion(t, "0.7.1")) {
		t.Errorf("the rehearsal's file: %+v, %v", image, err)
	}
	if image, err := release.ParseDigests([]byte("cactusfs:1 sha256:" + hexOf(1) + "\ncactusfs:1 linux/arm64 sha256:" + hexOf(2))); err != nil || image.Repository != "cactusfs" {
		t.Errorf("a repository named cactusfs alone: %+v, %v", image, err)
	}

	line := func(ref, rest string) string { return ref + " " + rest + "\n" }
	list := func(b byte) string { return "sha256:" + hexOf(b) }
	const img = "ghcr.io/albievan/cactusfs:0.7.1"
	bad := map[string]string{
		"empty":                       "",
		"no image":                    line(img, list(1)),
		"no list":                     line(img, "linux/amd64 "+list(2)),
		"two lists":                   line(img+"-87", list(1)) + line(img, list(9)) + line(img, "linux/amd64 "+list(2)),
		"a tag twice":                 line(img, list(1)) + line(img, list(1)) + line(img, "linux/amd64 "+list(2)),
		"a platform twice":            line(img, list(1)) + line(img, "linux/amd64 "+list(2)) + line(img, "linux/amd64 "+list(2)),
		"two images with platforms":   line(img, list(1)) + line(img, "linux/amd64 "+list(2)) + line("ghcr.io/x/cactusfs:1", "linux/amd64 "+list(3)),
		"another image":               line("ghcr.io/albievan/other:1", list(1)) + line("ghcr.io/albievan/other:1", "linux/amd64 "+list(2)),
		"an image ending in cactusfs": line("ghcr.io/x/notcactusfs:1", list(1)) + line("ghcr.io/x/notcactusfs:1", "linux/amd64 "+list(2)),
		"no tag":                      line("ghcr.io/albievan/cactusfs", list(1)) + line(img, "linux/amd64 "+list(2)),
		"a port and no tag":           line("localhost:5000/cactusfs", list(1)) + line(img, "linux/amd64 "+list(2)),
		"an empty tag":                line("ghcr.io/albievan/cactusfs:", list(1)) + line(img, "linux/amd64 "+list(2)),
		"an empty element":            line("ghcr.io//cactusfs:1", list(1)) + line("ghcr.io//cactusfs:1", "linux/amd64 "+list(2)),
		"an upper-case repository":    line("ghcr.io/Albievan/cactusfs:1", list(1)) + line("ghcr.io/Albievan/cactusfs:1", "linux/amd64 "+list(2)),
		"a digest of sha512":          line(img, "sha512:"+hexOf(1)) + line(img, "linux/amd64 "+list(2)),
		"a digest without algorithm":  line(img, hexOf(1)) + line(img, "linux/amd64 "+list(2)),
		"a short digest":              line(img, list(1)[:70]) + line(img, "linux/amd64 "+list(2)),
		"an upper-case digest":        line(img, strings.ToUpper(list(0xab))) + line(img, "linux/amd64 "+list(2)),
		"a platform of three parts":   line(img, list(1)) + line(img, "linux/arm/v7 "+list(2)),
		"a platform of one part":      line(img, list(1)) + line(img, "linux "+list(2)),
		"two spaces":                  img + "  " + list(1) + "\n" + line(img, "linux/amd64 "+list(2)),
		"a tab":                       img + "\t" + list(1) + "\n" + line(img, "linux/amd64 "+list(2)),
		"four fields":                 line(img, "linux/amd64 x "+list(2)),
		"a carriage return":           img + " " + list(1) + "\r\n" + line(img, "linux/amd64 "+list(2)),
		"an empty line":               line(img, list(1)) + "\n" + line(img, "linux/amd64 "+list(2)),
		"a malformed chart line":      line(img, list(1)) + line(img, "linux/amd64 "+list(2)) + line("ghcr.io/albievan/charts/cactusfs:0.7.1", "nope"),
		"a tag with a slash":          line("ghcr.io/albievan/cactusfs:a/b", list(1)) + line(img, "linux/amd64 "+list(2)),
		"too many lines":              line(img, list(1)) + line(img, "linux/amd64 "+list(2)) + strings.Repeat(line("x:1", list(3)), release.MaxListLines),
	}
	for name, data := range bad {
		if image, err := release.ParseDigests([]byte(data)); !errors.Is(err, domain.ErrInvalidArgument) || !reflect.DeepEqual(image, release.Image{}) {
			t.Errorf("%s: ParseDigests = %+v, %v", name, image, err)
		}
	}
}

func TestVerified(t *testing.T) {
	t.Parallel()
	v := mustVersion(t, "0.7.1")
	sums := []byte(sumsFile())
	bin, err := release.NewVerifiedBinary(v, sums, "cactusfs-linux-amd64", sumOf(0xa2))
	if err != nil {
		t.Fatal(err)
	}
	if bin.IsZero() || bin.Version() != v || bin.Kind() != release.KindBinary || bin.Asset() != "cactusfs-linux-amd64" ||
		bin.SHA256() != sumOf(0xa2) || bin.Digest() != "sha256:"+hexOf(0xa2) || !reflect.DeepEqual(bin.Image(), release.Image{}) {
		t.Errorf("the verified binary = %+v", bin)
	}

	digests := []byte(digestsFile())
	img, err := release.NewVerifiedImage(v, digests)
	if err != nil {
		t.Fatal(err)
	}
	if img.IsZero() || img.Version() != v || img.Kind() != release.KindImage || img.Asset() != "" || img.SHA256() != [32]byte{} ||
		img.Digest() != "sha256:"+hexOf(0x11) || img.Image().Repository != "ghcr.io/albievan/cactusfs" {
		t.Errorf("the verified image = %+v", img)
	}
	// What it hands out is a copy.
	got := img.Image()
	got.Platforms["linux/amd64"] = "changed"
	got.Tags[0] = "changed"
	if again := img.Image(); again.Platforms["linux/amd64"] == "changed" || again.Tags[0] == "changed" {
		t.Error("Image shares what it holds")
	}
	if p := img.Image().Platforms; !slices.Equal(slices.Sorted(maps.Keys(p)), []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("platforms = %v", p)
	}

	var zero release.Verified
	if !zero.IsZero() || zero.Digest() != "" {
		t.Error("the zero Verified is something")
	}

	refusals := []struct {
		name string
		make func() (release.Verified, error)
	}{
		{"a binary of no version", func() (release.Verified, error) {
			return release.NewVerifiedBinary(release.Version{}, sums, "cactusfs-linux-amd64", sumOf(0xa2))
		}},
		{"a binary of another sum", func() (release.Verified, error) {
			return release.NewVerifiedBinary(v, sums, "cactusfs-linux-amd64", sumOf(0xa3))
		}},
		{"a binary of the sum of another binary's... name", func() (release.Verified, error) {
			return release.NewVerifiedBinary(v, sums, "cactusfs-linux-arm64", sumOf(0xa2))
		}},
		{"a binary that is not listed", func() (release.Verified, error) {
			return release.NewVerifiedBinary(v, sums, "cactusfs-freebsd-amd64", sumOf(0xa2))
		}},
		{"a binary of a list that does not parse", func() (release.Verified, error) {
			return release.NewVerifiedBinary(v, []byte("not a list"), "cactusfs-linux-amd64", sumOf(0xa2))
		}},
		{"an image of no version", func() (release.Verified, error) {
			return release.NewVerifiedImage(release.Version{}, digests)
		}},
		{"an image the list does not tag with the version", func() (release.Verified, error) {
			return release.NewVerifiedImage(mustVersion(t, "0.7.2"), digests)
		}},
		{"an image of a list that does not parse", func() (release.Verified, error) {
			return release.NewVerifiedImage(v, []byte("x"))
		}},
	}
	for _, tt := range refusals {
		got, err := tt.make()
		if !errors.Is(err, domain.ErrReleaseNotVerified) || !got.IsZero() {
			t.Errorf("%s: %+v, %v", tt.name, got, err)
		}
	}
}

// FuzzParseSums: what is read names files only, each once, and reads back
// the same when written again as sha256sum writes it.
func FuzzParseSums(f *testing.F) {
	f.Add([]byte(sumsFile()))
	f.Add([]byte(hexOf(1) + "  a"))
	f.Add([]byte(hexOf(1) + " *a\n"))
	f.Add([]byte(hexOf(1) + "  a\n" + hexOf(1) + "  a\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		sums, err := release.ParseSums(data)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) || sums != nil {
				t.Fatalf("ParseSums: %v, %v", sums, err)
			}
			return
		}
		if len(sums) == 0 || len(sums) > release.MaxListLines {
			t.Fatalf("ParseSums read %d sums", len(sums))
		}
		var b strings.Builder
		for _, name := range slices.Sorted(maps.Keys(sums)) {
			if strings.ContainsAny(name, "/\\ \r\n\t") || name == ".." {
				t.Fatalf("ParseSums took the name %q", name)
			}
			sum := sums[name]
			fmt.Fprintf(&b, "%x  %s\n", sum, name)
		}
		again, err := release.ParseSums([]byte(b.String()))
		if err != nil || !maps.Equal(again, sums) {
			t.Fatalf("written again, %q reads as %v, %v", b.String(), again, err)
		}
	})
}

// FuzzParseDigests: what is read is an image of a repository named
// cactusfs, with a list and digests of the one shape.
func FuzzParseDigests(f *testing.F) {
	f.Add([]byte(digestsFile()))
	f.Add([]byte("localhost:5000/cactusfs:1 sha256:" + hexOf(1) + "\nlocalhost:5000/cactusfs:1 linux/amd64 sha256:" + hexOf(2)))
	f.Add([]byte("cactusfs:1 linux/amd64 sha256:" + hexOf(2)))
	f.Fuzz(func(t *testing.T, data []byte) {
		image, err := release.ParseDigests(data)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) || !reflect.DeepEqual(image, release.Image{}) {
				t.Fatalf("ParseDigests: %+v, %v", image, err)
			}
			return
		}
		if image.Repository != "cactusfs" && !strings.HasSuffix(image.Repository, "/cactusfs") {
			t.Fatalf("the image of %q", image.Repository)
		}
		if len(image.Tags) == 0 || len(image.Platforms) == 0 {
			t.Fatalf("an image without tags or platforms: %+v", image)
		}
		digests := append([]string{image.List}, slices.Collect(maps.Values(image.Platforms))...)
		for _, d := range digests {
			hexPart, ok := strings.CutPrefix(d, "sha256:")
			if !ok || len(hexPart) != 64 || strings.ToLower(hexPart) != hexPart {
				t.Fatalf("the digest %q", d)
			}
		}
	})
}

// TestVerifiedBinaryOfRealBytes: the sum a caller computes of the bytes it
// holds is what is held to the list.
func TestVerifiedBinaryOfRealBytes(t *testing.T) {
	t.Parallel()
	binary := []byte("the bytes of a binary")
	sum := sha256.Sum256(binary)
	list := fmt.Sprintf("%x  cactusfs-darwin-arm64\n", sum)
	v := mustVersion(t, "1.0.0")
	if _, err := release.NewVerifiedBinary(v, []byte(list), "cactusfs-darwin-arm64", sum); err != nil {
		t.Errorf("the bytes the list names: %v", err)
	}
	binary[0] ^= 1
	if _, err := release.NewVerifiedBinary(v, []byte(list), "cactusfs-darwin-arm64", sha256.Sum256(binary)); !errors.Is(err, domain.ErrReleaseNotVerified) {
		t.Errorf("one bit changed: %v", err)
	}
}
