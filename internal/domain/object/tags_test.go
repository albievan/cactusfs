package object_test

import (
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestValidateTags(t *testing.T) {
	t.Parallel()
	eleven := map[string]string{}
	for i := range 11 {
		eleven[strconv.Itoa(i)] = ""
	}
	tests := []struct {
		name string
		tags map[string]string
		max  int
		ok   bool
	}{
		{"none", nil, object.MaxObjectTags, true},
		{"some", map[string]string{"project": "apollo", "empty": ""}, object.MaxObjectTags, true},
		{"the longest", map[string]string{strings.Repeat("k", 128): strings.Repeat("ü", 256)}, object.MaxObjectTags, true},
		{"too many for an object", eleven, object.MaxObjectTags, false},
		{"not too many for a bucket", eleven, object.MaxBucketTags, true},
		{"an empty key", map[string]string{"": "v"}, object.MaxObjectTags, false},
		{"a long key", map[string]string{strings.Repeat("k", 129): ""}, object.MaxObjectTags, false},
		{"a long value", map[string]string{"k": strings.Repeat("v", 257)}, object.MaxObjectTags, false},
		{"a key that is not text", map[string]string{"\xff": ""}, object.MaxObjectTags, false},
		{"a value that is not text", map[string]string{"k": "\xff"}, object.MaxObjectTags, false},
	}
	for _, tt := range tests {
		err := object.ValidateTags(tt.tags, tt.max)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

// TestNormalizeTags: keys and values become lower case; twice is once; two
// keys that become one are refused, not merged.
func TestNormalizeTags(t *testing.T) {
	t.Parallel()
	got, err := object.NormalizeTags(map[string]string{"Project": "Cactus", "stage": "DRAFT", "Ünïcode": "ÄB"})
	want := map[string]string{"project": "cactus", "stage": "draft", "ünïcode": "äb"}
	if err != nil || !maps.Equal(got, want) {
		t.Errorf("NormalizeTags = %v, %v; want %v", got, err, want)
	}
	if got, err := object.NormalizeTags(nil); err != nil || got != nil {
		t.Errorf("NormalizeTags(nil) = %v, %v", got, err)
	}
	if _, err := object.NormalizeTags(map[string]string{"Env": "a", "env": "b"}); !errors.Is(err, domain.ErrInvalidTag) {
		t.Errorf("two keys that are one in lower case: %v, want %v", err, domain.ErrInvalidTag)
	}
	idempotent := func(tags map[string]string) bool {
		once, err := object.NormalizeTags(tags)
		if err != nil {
			return errors.Is(err, domain.ErrInvalidTag)
		}
		twice, err := object.NormalizeTags(once)
		return err == nil && maps.Equal(once, twice) && len(once) <= len(tags)
	}
	if err := quick.Check(idempotent, nil); err != nil {
		t.Error(err)
	}
}
