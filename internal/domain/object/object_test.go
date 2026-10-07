package object_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestID(t *testing.T) {
	t.Parallel()

	id, err := object.NewID(bytes.NewReader(bytes.Repeat([]byte{0xAB}, object.IDSize)))
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if got, want := id.String(), strings.Repeat("ab", object.IDSize); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if id.IsZero() {
		t.Error("IsZero() = true for a non-zero ID")
	}
	if !(object.ID{}).IsZero() {
		t.Error("IsZero() = false for the zero ID")
	}

	parsed, err := object.ParseID(id.String())
	if err != nil || parsed != id {
		t.Errorf("ParseID(String()) = %v, %v", parsed, err)
	}

	if _, err := object.NewID(iotest.ErrReader(errors.New("no entropy"))); err == nil {
		t.Error("NewID with a failing source returned nil error")
	}
	if _, err := object.NewID(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Error("NewID with a short source returned nil error")
	}
}

func TestParseIDRejectsEverythingElse(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"", "abc", strings.Repeat("a", 31), strings.Repeat("a", 33),
		strings.Repeat("A", 32), strings.Repeat("g", 32),
		"../../../../etc/passwd0000000000", strings.Repeat("a", 30) + "/.",
	} {
		if _, err := object.ParseID(input); !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("ParseID(%q) error = %v, want %v", input, err, domain.ErrInvalidID)
		}
	}
}

func FuzzParseID(f *testing.F) {
	f.Add(strings.Repeat("0", 32))
	f.Add("../x")
	f.Fuzz(func(t *testing.T, input string) {
		id, err := object.ParseID(input)
		if err != nil {
			return
		}
		if id.String() != input {
			t.Fatalf("ParseID(%q).String() = %q", input, id.String())
		}
		if strings.ContainsAny(input, "/\\.") {
			t.Fatalf("accepted %q, which could be read as a path", input)
		}
	})
}

func TestShardIDString(t *testing.T) {
	t.Parallel()

	id := object.ShardID{Index: 3}
	if got, want := id.String(), strings.Repeat("0", 32)+".3"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestValidateKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		valid bool
	}{
		{"simple", "a", true},
		{"nested", "photos/2026/cactus.jpg", true},
		{"traversal is just a name", "../../etc/passwd", true},
		{"windows reserved is just a name", "CON", true},
		{"unicode", "ключ/鍵/🌵", true},
		{"control characters", "a\x00b\nc", true},
		{"longest", strings.Repeat("k", object.MaxKeyLength), true},
		{"empty", "", false},
		{"too long", strings.Repeat("k", object.MaxKeyLength+1), false},
		{"invalid UTF-8", "bad\xff", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := object.ValidateKey(tt.key)
			if tt.valid && err != nil {
				t.Errorf("ValidateKey = %v, want nil", err)
			}
			if !tt.valid && !errors.Is(err, domain.ErrInvalidObjectKey) {
				t.Errorf("ValidateKey = %v, want %v", err, domain.ErrInvalidObjectKey)
			}
		})
	}
}

func TestValidateUserMetadata(t *testing.T) {
	t.Parallel()

	if err := object.ValidateUserMetadata(nil); err != nil {
		t.Errorf("nil metadata: %v", err)
	}
	fits := map[string]string{"k": strings.Repeat("v", object.MaxUserMetadataSize-1)}
	if err := object.ValidateUserMetadata(fits); err != nil {
		t.Errorf("metadata at the limit: %v", err)
	}
	tooBig := map[string]string{"k": strings.Repeat("v", object.MaxUserMetadataSize)}
	if err := object.ValidateUserMetadata(tooBig); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("metadata over the limit: error = %v, want %v", err, domain.ErrInvalidArgument)
	}
}

func TestRangeSpecResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    object.RangeSpec
		size    int64
		want    object.Range
		invalid bool
	}{
		{name: "first to last", spec: object.RangeSpec{First: 2, Last: 5}, size: 10, want: object.Range{Offset: 2, Length: 4}},
		{name: "single byte", spec: object.RangeSpec{First: 0, Last: 0}, size: 10, want: object.Range{Offset: 0, Length: 1}},
		{name: "open ended", spec: object.RangeSpec{First: 4, Last: -1}, size: 10, want: object.Range{Offset: 4, Length: 6}},
		{name: "last beyond the end is clamped", spec: object.RangeSpec{First: 4, Last: 99}, size: 10, want: object.Range{Offset: 4, Length: 6}},
		{name: "suffix", spec: object.RangeSpec{Suffix: 3}, size: 10, want: object.Range{Offset: 7, Length: 3}},
		{name: "suffix longer than the object", spec: object.RangeSpec{Suffix: 99}, size: 10, want: object.Range{Offset: 0, Length: 10}},
		{name: "first at the end", spec: object.RangeSpec{First: 10, Last: -1}, size: 10, invalid: true},
		{name: "first beyond the end", spec: object.RangeSpec{First: 11, Last: 12}, size: 10, invalid: true},
		{name: "last before first", spec: object.RangeSpec{First: 5, Last: 4}, size: 10, invalid: true},
		{name: "negative first", spec: object.RangeSpec{First: -1, Last: 4}, size: 10, invalid: true},
		{name: "negative suffix", spec: object.RangeSpec{Suffix: -1, Last: -1}, size: 10, invalid: true},
		{name: "any range of an empty object", spec: object.RangeSpec{First: 0, Last: -1}, size: 0, invalid: true},
		{name: "suffix of an empty object", spec: object.RangeSpec{Suffix: 1}, size: 0, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.spec.Resolve(tt.size)
			if tt.invalid {
				if !errors.Is(err, domain.ErrInvalidRange) {
					t.Fatalf("Resolve error = %v, want %v", err, domain.ErrInvalidRange)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Resolve = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func FuzzRangeSpecResolve(f *testing.F) {
	f.Add(int64(0), int64(-1), int64(0), int64(10))
	f.Add(int64(5), int64(2), int64(3), int64(0))
	f.Fuzz(func(t *testing.T, first, last, suffix, size int64) {
		if size < 0 {
			return
		}
		r, err := object.RangeSpec{First: first, Last: last, Suffix: suffix}.Resolve(size)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidRange) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > size {
			t.Fatalf("Resolve(%d,%d,%d) on size %d = %+v, outside the object", first, last, suffix, size, r)
		}
	})
}

func TestCommonPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key, prefix, delimiter string
		want                   string
		ok                     bool
	}{
		{"photos/2026/a.jpg", "", "/", "photos/", true},
		{"photos/2026/a.jpg", "photos/", "/", "photos/2026/", true},
		{"photos/2026/a.jpg", "photos/2026/", "/", "", false},
		{"a.jpg", "", "/", "", false},
		{"a--b--c", "a", "--", "a--", true},
		{"photos/a.jpg", "", "", "", false},
		{"photos/a.jpg", "videos/", "/", "", false},
		{"photos/", "", "/", "photos/", true},
	}
	for _, tt := range tests {
		got, ok := object.CommonPrefix(tt.key, tt.prefix, tt.delimiter)
		if got != tt.want || ok != tt.ok {
			t.Errorf("CommonPrefix(%q, %q, %q) = %q, %v; want %q, %v", tt.key, tt.prefix, tt.delimiter, got, ok, tt.want, tt.ok)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		prefix string
		want   string
		ok     bool
	}{
		{"photos/", "photos0", true},
		{"a", "b", true},
		{"a\xff", "b", true},
		{"a\xff\xff", "b", true},
		{"", "", false},
		{"\xff\xff", "", false},
	}
	for _, tt := range tests {
		got, ok := object.PrefixEnd(tt.prefix)
		if got != tt.want || ok != tt.ok {
			t.Errorf("PrefixEnd(%q) = %q, %v; want %q, %v", tt.prefix, got, ok, tt.want, tt.ok)
		}
	}
}

// FuzzPrefixEnd checks the defining property: the result is greater than the
// prefix itself and than any extension of it.
func FuzzPrefixEnd(f *testing.F) {
	f.Add("photos/", "2026/a.jpg")
	f.Add("\xff", "\xff\xff")
	f.Fuzz(func(t *testing.T, prefix, rest string) {
		end, ok := object.PrefixEnd(prefix)
		if !ok {
			return
		}
		if end <= prefix || end <= prefix+rest {
			t.Fatalf("PrefixEnd(%q) = %q is not greater than %q", prefix, end, prefix+rest)
		}
		if strings.HasPrefix(end, prefix) {
			t.Fatalf("PrefixEnd(%q) = %q still has the prefix", prefix, end)
		}
	})
}
