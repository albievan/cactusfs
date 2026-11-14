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

// TestTagMap: a list is a map but for a key given twice, which is refused
// and named; TagList is the way back, in key order.
func TestTagMap(t *testing.T) {
	t.Parallel()
	got, err := object.TagMap([]object.Tag{{Key: "stage", Value: "draft"}, {Key: "Stage", Value: "Draft"}, {Key: "empty"}})
	if want := map[string]string{"stage": "draft", "Stage": "Draft", "empty": ""}; err != nil || !maps.Equal(got, want) {
		t.Errorf("TagMap = %v, %v; want %v", got, err, want)
	}
	if got, err := object.TagMap(nil); err != nil || got != nil {
		t.Errorf("TagMap(nil) = %v, %v", got, err)
	}
	_, err = object.TagMap([]object.Tag{{Key: "a", Value: "secret"}, {Key: "a", Value: "secret"}})
	if !errors.Is(err, domain.ErrInvalidTag) || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("a key given twice, even with one value: %v", err)
	}
	if got := object.TagList(map[string]string{"b": "2", "a": "1"}); len(got) != 2 || got[0] != (object.Tag{Key: "a", Value: "1"}) || got[1].Key != "b" {
		t.Errorf("TagList = %v", got)
	}
	roundTrip := func(tags map[string]string) bool {
		back, err := object.TagMap(object.TagList(tags))
		return err == nil && maps.Equal(back, tags) && (len(tags) > 0 || back == nil)
	}
	if err := quick.Check(roundTrip, nil); err != nil {
		t.Error(err)
	}
}
