package object_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestCheckCompletion(t *testing.T) {
	t.Parallel()
	const big = object.MinPartSize
	stored := map[int]object.Part{
		1: {Number: 1, Size: big, ETag: "one", Checksums: map[string]string{"x-amz-checksum-crc32": "AAAAAA=="}},
		2: {Number: 2, Size: big + 1, ETag: "two"},
		3: {Number: 3, Size: 10, ETag: "three"},
		7: {Number: 7, Size: 1, ETag: "seven"},
		9: {Number: 9, Size: object.MaxMultipartSize, ETag: "huge"},
	}
	lookup := func(n int) (object.Part, bool) { p, ok := stored[n]; return p, ok }
	part := func(n int, etag string) object.CompletedPart { return object.CompletedPart{Number: n, ETag: etag} }

	tests := []struct {
		name   string
		listed []object.CompletedPart
		size   int64
		want   error
	}{
		{"all, in order", []object.CompletedPart{part(1, "one"), part(2, "two"), part(3, "three")}, 2*big + 11, nil},
		{"one small part", []object.CompletedPart{part(3, "three")}, 10, nil},
		{"with a gap in the numbers", []object.CompletedPart{part(1, "one"), part(7, "seven")}, big + 1, nil},
		{"some left out", []object.CompletedPart{part(2, "two")}, big + 1, nil},
		{"none", nil, 0, domain.ErrInvalidArgument},
		{"with the checksum that was verified", []object.CompletedPart{{Number: 1, ETag: "one", Checksums: map[string]string{"x-amz-checksum-crc32": "AAAAAA=="}}}, big, nil},
		{"with another checksum value", []object.CompletedPart{{Number: 1, ETag: "one", Checksums: map[string]string{"x-amz-checksum-crc32": "BBBBBB=="}}}, 0, domain.ErrPartNotFound},
		{"with a checksum that was never declared", []object.CompletedPart{{Number: 3, ETag: "three", Checksums: map[string]string{"x-amz-checksum-sha256": "x"}}}, 0, domain.ErrPartNotFound},
		{"out of order", []object.CompletedPart{part(2, "two"), part(1, "one")}, 0, domain.ErrPartOrder},
		{"a part twice", []object.CompletedPart{part(1, "one"), part(1, "one")}, 0, domain.ErrPartOrder},
		{"a part that was not uploaded", []object.CompletedPart{part(1, "one"), part(4, "four")}, 0, domain.ErrPartNotFound},
		{"another entity tag", []object.CompletedPart{part(1, "ONE")}, 0, domain.ErrPartNotFound},
		{"a small part that is not the last", []object.CompletedPart{part(3, "three"), part(7, "seven")}, 0, domain.ErrPartTooSmall},
		{"part number zero", []object.CompletedPart{part(0, "")}, 0, domain.ErrInvalidArgument},
		{"part number too great", []object.CompletedPart{part(object.MaxParts+1, "")}, 0, domain.ErrInvalidArgument},
		{"too large in all", []object.CompletedPart{part(1, "one"), part(9, "huge")}, 0, domain.ErrEntityTooLarge},
	}
	for _, tt := range tests {
		size, err := object.CheckCompletion(tt.listed, lookup)
		if !errors.Is(err, tt.want) || size != tt.size {
			t.Errorf("%s: %d, %v; want %d, %v", tt.name, size, err, tt.size, tt.want)
		}
	}
	for n, want := range map[int]bool{0: false, 1: true, object.MaxParts: true, object.MaxParts + 1: false, -1: false} {
		if err := object.ValidatePartNumber(n); (err == nil) != want {
			t.Errorf("ValidatePartNumber(%d) = %v", n, err)
		}
	}
	if !(object.Version{Parts: 2}).Multipart() || (object.Version{}).Multipart() {
		t.Error("Multipart")
	}
}

// TestPartKeys: every part has a key of its own, which is neither the
// upload's key nor any other part's, and which the same inputs give again.
func TestPartKeys(t *testing.T) {
	t.Parallel()
	property := func(key [object.KeySize]byte, a, b object.ID, n, m uint16) bool {
		na, nb := int(n)%object.MaxParts+1, int(m)%object.MaxParts+1
		ka, errA := object.PartKey(key[:], na, a)
		kb, errB := object.PartKey(key[:], nb, b)
		again, _ := object.PartKey(key[:], na, a)
		if errA != nil || errB != nil || len(ka) != object.KeySize || !bytes.Equal(ka, again) || bytes.Equal(ka, key[:]) {
			return false
		}
		return (a == b && na == nb) == bytes.Equal(ka, kb)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
	// Another upload key gives other part keys.
	var one, two [object.KeySize]byte
	two[0] = 1
	id := object.ID{7}
	k1, _ := object.PartKey(one[:], 1, id)
	k2, _ := object.PartKey(two[:], 1, id)
	if bytes.Equal(k1, k2) {
		t.Error("two upload keys give one part key")
	}
	if _, err := object.PartKey(one[:16], 1, id); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a short upload key = %v", err)
	}
	if _, err := object.PartKey(one[:], 0, id); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("part number zero = %v", err)
	}
}

// TestManifestSealsThePlaceOfAPart: a seal holds for exactly the part, the
// place and the object it was made for.
func TestManifestSealsThePlaceOfAPart(t *testing.T) {
	t.Parallel()
	var key [object.KeySize]byte
	manifest, err := object.NewManifest(key[:])
	if err != nil {
		t.Fatal(err)
	}
	const count, total = 3, 1000
	part := object.Part{Number: 5, DataID: object.ID{1}, Size: 300, Index: 1, Offset: 400}
	part.Seal = manifest.Seal(part, count, total)
	if err := manifest.Verify(part, count, total); err != nil {
		t.Fatalf("the sealed part: %v", err)
	}
	changes := map[string]func(p *object.Part, count *int, total *int64){
		"another index":       func(p *object.Part, _ *int, _ *int64) { p.Index = 0 },
		"another offset":      func(p *object.Part, _ *int, _ *int64) { p.Offset = 0 },
		"another size":        func(p *object.Part, _ *int, _ *int64) { p.Size = 301 },
		"another number":      func(p *object.Part, _ *int, _ *int64) { p.Number = 2 },
		"other data":          func(p *object.Part, _ *int, _ *int64) { p.DataID[0] ^= 1 },
		"fewer parts":         func(_ *object.Part, count *int, _ *int64) { *count = 2 },
		"a shorter object":    func(_ *object.Part, _ *int, total *int64) { *total = 700 },
		"no seal":             func(p *object.Part, _ *int, _ *int64) { p.Seal = nil },
		"a seal cut short":    func(p *object.Part, _ *int, _ *int64) { p.Seal = p.Seal[:16] },
		"a seal with a flip":  func(p *object.Part, _ *int, _ *int64) { p.Seal = append([]byte{p.Seal[0] ^ 1}, p.Seal[1:]...) },
		"index and count off": func(p *object.Part, count *int, _ *int64) { p.Index, *count = 2, 4 },
	}
	for name, change := range changes {
		p, c, n := part, count, int64(total)
		change(&p, &c, &n)
		if err := manifest.Verify(p, c, n); !errors.Is(err, domain.ErrIntegrity) {
			t.Errorf("%s: %v, want %v", name, err, domain.ErrIntegrity)
		}
	}
	// Another upload's key does not make this upload's seals.
	key[0] = 1
	other, _ := object.NewManifest(key[:])
	if err := other.Verify(part, count, total); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("a seal under another key: %v", err)
	}
	other.Clear()
	if _, err := object.NewManifest(key[:5]); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a short key = %v", err)
	}
	if (object.Version{DataID: object.ID{3}}).Extent().DataID != (object.ID{3}) || part.Extent().DataID != part.DataID {
		t.Error("Extent")
	}
}

// TestSealsAreOfOnePlace: two parts, places or objects that differ in
// anything have different seals.
func TestSealsAreOfOnePlace(t *testing.T) {
	t.Parallel()
	var key [object.KeySize]byte
	manifest, err := object.NewManifest(key[:])
	if err != nil {
		t.Fatal(err)
	}
	type place struct {
		Index, Count, Number uint16
		Offset, Size, Total  uint32
		Data                 object.ID
	}
	seal := func(p place) []byte {
		part := object.Part{Number: int(p.Number), DataID: p.Data, Size: int64(p.Size), Index: int(p.Index), Offset: int64(p.Offset)}
		return manifest.Seal(part, int(p.Count), int64(p.Total))
	}
	property := func(a, b place) bool {
		return (a == b) == bytes.Equal(seal(a), seal(b))
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
	// Numbers that would run together without separators do not.
	if bytes.Equal(seal(place{Index: 1, Count: 23}), seal(place{Index: 12, Count: 3})) {
		t.Error("two places give one seal")
	}
	id := object.ID{1, 2, 3}
	if ctx := object.UploadKeyContext(id); !bytes.HasSuffix(ctx, id[:]) || len(ctx) <= object.IDSize {
		t.Errorf("upload key context = %q", ctx)
	}
}
