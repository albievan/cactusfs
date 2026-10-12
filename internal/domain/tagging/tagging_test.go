package tagging_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/tagging"
)

func values(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "v" + strconv.Itoa(i)
	}
	return out
}

func TestDefinitionValidate(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		definition tagging.Definition
		valid      bool
	}{
		"a key alone":                 {tagging.Definition{Key: "stage"}, true},
		"a key, text and values":      {tagging.Definition{Key: "stage", Description: "Where the work is.", Values: []string{"draft", "final", ""}}, true},
		"the longest key":             {tagging.Definition{Key: strings.Repeat("k", tagging.MaxKeyBytes)}, true},
		"the longest description":     {tagging.Definition{Key: "k", Description: strings.Repeat("d", tagging.MaxDescriptionBytes)}, true},
		"as many values as there can": {tagging.Definition{Key: "k", Values: values(tagging.MaxValues)}, true},
		"the longest value":           {tagging.Definition{Key: "k", Values: []string{strings.Repeat("v", tagging.MaxValueBytes)}}, true},
		"lower case beyond ASCII":     {tagging.Definition{Key: "größe", Values: []string{"über"}}, true},
		"characters without a case":   {tagging.Definition{Key: "cost-centre:2026/q4", Values: []string{"a+b=c", "x_y@z"}}, true},

		"no key":                    {tagging.Definition{}, false},
		"a key too long":            {tagging.Definition{Key: strings.Repeat("k", tagging.MaxKeyBytes+1)}, false},
		"a key in upper case":       {tagging.Definition{Key: "Stage"}, false},
		"a key in upper case, Ä":    {tagging.Definition{Key: "Äpfel"}, false},
		"a key that is not text":    {tagging.Definition{Key: "k\xff"}, false},
		"a description too long":    {tagging.Definition{Key: "k", Description: strings.Repeat("d", tagging.MaxDescriptionBytes+1)}, false},
		"a description not text":    {tagging.Definition{Key: "k", Description: "\xc3\x28"}, false},
		"too many values":           {tagging.Definition{Key: "k", Values: values(tagging.MaxValues + 1)}, false},
		"a value too long":          {tagging.Definition{Key: "k", Values: []string{strings.Repeat("v", tagging.MaxValueBytes+1)}}, false},
		"a value in upper case":     {tagging.Definition{Key: "k", Values: []string{"draft", "Final"}}, false},
		"a value that is not text":  {tagging.Definition{Key: "k", Values: []string{"\xff"}}, false},
		"a value given twice":       {tagging.Definition{Key: "k", Values: []string{"draft", "final", "draft"}}, false},
		"the empty value twice":     {tagging.Definition{Key: "k", Values: []string{"", ""}}, false},
		"a key of 128 runes, wider": {tagging.Definition{Key: strings.Repeat("ö", tagging.MaxKeyBytes)}, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := test.definition.Validate()
			if test.valid && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !test.valid && !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Validate() = %v, want %v", err, domain.ErrInvalidArgument)
			}
		})
	}
}

func TestDefinitionAllows(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		values []string
		value  string
		want   bool
	}{
		"no values listed: any value": {nil, "anything", true},
		"no values listed: none":      {nil, "", true},
		"a listed value":              {[]string{"draft", "final"}, "final", true},
		"a value that is not listed":  {[]string{"draft", "final"}, "review", false},
		"another case is another":     {[]string{"draft"}, "Draft", false},
		"empty, when not listed":      {[]string{"draft"}, "", false},
		"empty, when listed":          {[]string{"draft", ""}, "", true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := (tagging.Definition{Key: "stage", Values: test.values}).Allows(test.value); got != test.want {
				t.Errorf("Allows(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestDefinitionClone(t *testing.T) {
	t.Parallel()
	original := tagging.Definition{
		Key: "stage", Description: "Where the work is.", Values: []string{"draft", "final"},
		CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
	}
	clone := original.Clone()
	clone.Values[0] = "changed"
	if original.Values[0] != "draft" {
		t.Error("changing the clone changed the original")
	}
	if clone.Key != original.Key || clone.Description != original.Description || !clone.CreatedAt.Equal(original.CreatedAt) || len(clone.Values) != 2 {
		t.Errorf("Clone() = %+v, want a copy of %+v", clone, original)
	}
	if empty := (tagging.Definition{Key: "k"}).Clone(); empty.Values != nil {
		t.Errorf("the clone of a definition without values has %v", empty.Values)
	}
}

// FuzzDefinitionValidate: whatever a definition holds, Validate answers and
// does not panic, and what it accepts is within every bound it names.
func FuzzDefinitionValidate(f *testing.F) {
	f.Add("stage", "Where the work is.", "draft", "final")
	f.Add("", "", "", "")
	f.Add("K", "\xff", "a", "a")
	f.Fuzz(func(t *testing.T, key, description, first, second string) {
		d := tagging.Definition{Key: key, Description: description, Values: []string{first, second}}
		err := d.Validate()
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if key == "" || len(key) > tagging.MaxKeyBytes || key != strings.ToLower(key) || len(description) > tagging.MaxDescriptionBytes ||
			first == second || first != strings.ToLower(first) || second != strings.ToLower(second) ||
			len(first) > tagging.MaxValueBytes || len(second) > tagging.MaxValueBytes {
			t.Fatalf("Validate accepted %+v", d)
		}
	})
}
