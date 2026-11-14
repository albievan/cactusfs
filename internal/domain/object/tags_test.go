package object_test

import (
	"errors"
	"maps"
	"net/url"
	"slices"
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

func TestParseTagging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  []object.Tag
		err   error
	}{
		{name: "pairs in the order given", query: "team=finance&stage=draft", want: []object.Tag{{Key: "team", Value: "finance"}, {Key: "stage", Value: "draft"}}},
		{name: "escaped", query: "a%20b=c+d&%40x=%2F", want: []object.Tag{{Key: "a b", Value: "c d"}, {Key: "@x", Value: "/"}}},
		// The header the console sends (taggingOf in web/src/ui/tags.ts,
		// encodeURIComponent: a space is %20, never +), byte for byte as its
		// tests expect it.
		{name: "as the console encodes it", query: "stage=draft&team=a%2Bb%3Dc%2Fd%20e&Owner=Ana%40x%3Ay", want: []object.Tag{{Key: "stage", Value: "draft"}, {Key: "team", Value: "a+b=c/d e"}, {Key: "Owner", Value: "Ana@x:y"}}},
		{name: "a lone surrogate as the console sends it", query: "k=a%EF%BF%BDb", want: []object.Tag{{Key: "k", Value: "a\uFFFDb"}}},
		{name: "a key given twice is kept twice", query: "a=1&a=2", want: []object.Tag{{Key: "a", Value: "1"}, {Key: "a", Value: "2"}}},
		{name: "no value", query: "a&b=", want: []object.Tag{{Key: "a"}, {Key: "b"}}},
		{name: "empty", query: "", want: []object.Tag{}},
		{name: "empty parts", query: "&a=1&&", want: []object.Tag{{Key: "a", Value: "1"}}},
		{name: "a semicolon", query: "a=1;b=2", err: domain.ErrInvalidArgument},
		{name: "an escape that is none", query: "a=%zz", err: domain.ErrInvalidArgument},
		{name: "a key that is no escape", query: "%=1", err: domain.ErrInvalidArgument},
		// How many a version may have is ValidateTags', after the caller
		// is authorized: all are read.
		{name: "more pairs than a version can have", query: strings.Repeat("k=v&", object.MaxObjectTags) + "k=v", want: slices.Repeat([]object.Tag{{Key: "k", Value: "v"}}, object.MaxObjectTags+1)},
		{name: "more pairs than a query string takes", query: strings.Repeat("k=v&", 10000) + "k=v", err: domain.ErrInvalidArgument},
		{name: "more pairs, and a bad escape", query: strings.Repeat("k=v&", object.MaxObjectTags) + "k=%zz", err: domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := object.ParseTagging(tt.query)
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("ParseTagging(%q) = %v, want %v", tt.query, err, tt.err)
			}
			if err != nil && len(tt.query) > 0 && strings.Contains(err.Error(), tt.query) {
				t.Errorf("the error holds what was given: %v", err)
			}
			if tt.err == nil && (got == nil || !slices.Equal(got, tt.want)) {
				t.Errorf("ParseTagging(%q) = %#v, want %#v", tt.query, got, tt.want)
			}
		})
	}
	// Tags written as a query string are read back as they were, in order.
	roundTrip := func(keys, values []string) bool {
		var parts []string
		var want []object.Tag
		for i, key := range keys[:min(len(keys), object.MaxObjectTags)] {
			value := ""
			if i < len(values) {
				value = values[i]
			}
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
			want = append(want, object.Tag{Key: key, Value: value})
		}
		got, err := object.ParseTagging(strings.Join(parts, "&"))
		if err != nil {
			return false
		}
		return slices.Equal(got, want)
	}
	if err := quick.Check(roundTrip, nil); err != nil {
		t.Error(err)
	}
}

// FuzzParseTagging: ParseTagging reads what url.ParseQuery reads, as the
// same pairs, and refuses what it refuses - as an argument error.
func FuzzParseTagging(f *testing.F) {
	for _, seed := range []string{"a=1&b=2", "a=1&a=2", "", "&&", "a;b", "%zz=1", "k=v&k=v&k=v&k=v&k=v&k=v&k=v&k=v&k=v&k=v&k=v", "=&=x&y"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		tags, err := object.ParseTagging(query)
		parsed, parseErr := url.ParseQuery(query)
		if (err == nil) != (parseErr == nil) {
			t.Fatalf("ParseTagging(%q) = %v, url.ParseQuery = %v", query, err, parseErr)
		}
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseTagging(%q): %v is no argument error", query, err)
			}
			return
		}
		got := map[string][]string{}
		for _, tag := range tags {
			got[tag.Key] = append(got[tag.Key], tag.Value)
		}
		if !maps.EqualFunc(got, map[string][]string(parsed), slices.Equal) {
			t.Fatalf("ParseTagging(%q) = %v, url.ParseQuery = %v", query, got, parsed)
		}
	})
}
