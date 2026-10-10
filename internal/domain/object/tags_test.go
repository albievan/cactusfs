package object_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

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
