package bucket_test

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestTagRulesDocument(t *testing.T) {
	t.Parallel()
	rules := bucket.TagRules{
		Lowercase: true, CatalogueOnly: true,
		Required: []bucket.RequiredTag{{Key: "project"}, {SubBucket: "reports", Key: "stage", Values: []string{"draft", "final"}}},
	}
	document, err := rules.Document()
	if err != nil {
		t.Fatal(err)
	}
	back, err := bucket.ParseTagRules(document)
	if err != nil || !reflect.DeepEqual(back, rules) {
		t.Errorf("ParseTagRules(Document()) = %+v, %v; want %+v", back, err, rules)
	}
	// No document is no rules.
	if none, err := bucket.ParseTagRules(nil); err != nil || !none.Empty() {
		t.Errorf("ParseTagRules(nil) = %+v, %v", none, err)
	}
	if rules.Empty() {
		t.Error("rules that ask for something are empty")
	}
}

func TestTagRulesRefused(t *testing.T) {
	t.Parallel()
	many := make([]bucket.RequiredTag, bucket.MaxRequiredTags+1)
	for i := range many {
		many[i].Key = "k" + strings.Repeat("x", i)
	}
	values := make([]string, bucket.MaxAllowedValues+1)
	for i := range values {
		values[i] = strings.Repeat("v", i+1)
	}
	for name, rules := range map[string]bucket.TagRules{
		"too many required tags":       {Required: many},
		"a required tag without a key": {Required: []bucket.RequiredTag{{Key: ""}}},
		"a key that is too long":       {Required: []bucket.RequiredTag{{Key: strings.Repeat("k", 257)}}},
		// A key or a value no tag can have would refuse every object.
		"a key of 129 characters":           {Required: []bucket.RequiredTag{{Key: strings.Repeat("k", 129)}}},
		"a key with a comma":                {Required: []bucket.RequiredTag{{Key: "a,b"}}},
		"a key with an ampersand":           {Required: []bucket.RequiredTag{{Key: "a&b"}}},
		"a key that is not text":            {Required: []bucket.RequiredTag{{Key: "k\xff"}}},
		"a value of 257 characters":         {Required: []bucket.RequiredTag{{Key: "a", Values: []string{strings.Repeat("ü", 257)}}}},
		"a value with a semicolon":          {Required: []bucket.RequiredTag{{Key: "a", Values: []string{"x;y"}}}},
		"a value with a newline":            {Required: []bucket.RequiredTag{{Key: "a", Values: []string{"x\ny"}}}},
		"the same tag twice":                {Required: []bucket.RequiredTag{{Key: "a"}, {Key: "a"}}},
		"too many values":                   {Required: []bucket.RequiredTag{{Key: "a", Values: values}}},
		"a value twice":                     {Required: []bucket.RequiredTag{{Key: "a", Values: []string{"x", "x"}}}},
		"a value that is too long":          {Required: []bucket.RequiredTag{{Key: "a", Values: []string{strings.Repeat("v", 257)}}}},
		"a key in capitals, lower-casing":   {Lowercase: true, Required: []bucket.RequiredTag{{Key: "Project"}}},
		"a value in capitals, lower-casing": {Lowercase: true, Required: []bucket.RequiredTag{{Key: "a", Values: []string{"Draft"}}}},
		"a sub-bucket that cannot be one":   {Required: []bucket.RequiredTag{{SubBucket: "No/Such", Key: "a"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := rules.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Validate = %v, want %v", err, domain.ErrInvalidArgument)
			}
			if _, err := rules.Document(); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Document = %v, want %v", err, domain.ErrInvalidArgument)
			}
		})
	}
	// Every key and value a tag can have may be required: the longest, of
	// every kind of character, and the empty value.
	widest := bucket.TagRules{Required: []bucket.RequiredTag{
		{Key: strings.Repeat("ö", 128), Values: []string{strings.Repeat("ü", 256), ""}},
		{Key: "cost centre:2026/q4+x=y.z_w@v-u", Values: []string{"a b+c-d=e.f_g:h/i@j"}},
	}}
	if err := widest.Validate(); err != nil {
		t.Errorf("keys and values a tag can have: %v", err)
	}
	// The same tag may be required of two sub-buckets, or of one and of all.
	fine := bucket.TagRules{Required: []bucket.RequiredTag{{Key: "a"}, {SubBucket: "x", Key: "a"}, {SubBucket: "y", Key: "a"}}}
	if err := fine.Validate(); err != nil {
		t.Errorf("the same key for different sub-buckets: %v", err)
	}
	for name, document := range map[string]string{
		"not JSON":             "{",
		"an unknown field":     `{"version":1,"lowercase":false,"catalogue_only":false,"required":[],"more":1}`,
		"something after":      `{"version":1,"lowercase":false,"catalogue_only":false,"required":[]} {}`,
		"too long":             `{"version":1,"required":[],"x":"` + strings.Repeat("a", bucket.MaxConfigBytes) + `"}`,
		"rules that are wrong": `{"version":1,"lowercase":true,"catalogue_only":false,"required":[{"key":"A"}]}`,
	} {
		if _, err := bucket.ParseTagRules([]byte(document)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: ParseTagRules = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	if _, err := bucket.ParseTagRules([]byte(`{"version":2,"required":[]}`)); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("a document of a later version: %v, want %v", err, domain.ErrUnsupportedFormat)
	}
}

func TestTagRulesCheck(t *testing.T) {
	t.Parallel()
	rules := bucket.TagRules{Required: []bucket.RequiredTag{
		{Key: "project"},
		{SubBucket: "reports", Key: "stage", Values: []string{"draft", "final"}},
	}}
	catalogue := func(key string) ([]string, bool) {
		switch key {
		case "project":
			return nil, true
		case "stage":
			return []string{"draft", "final", "old"}, true
		}
		return nil, false
	}
	for name, tt := range map[string]struct {
		rules   bucket.TagRules
		key     string
		tags    map[string]string
		defined bucket.Defined
		refused bool
	}{
		"everything required is there":             {rules, "reports/q1.txt", map[string]string{"project": "cactus", "stage": "draft"}, nil, false},
		"a required tag is missing":                {rules, "a.txt", map[string]string{"other": "x"}, nil, true},
		"no tags at all":                           {rules, "a.txt", nil, nil, true},
		"a rule of a sub-bucket, outside it":       {rules, "other/q1.txt", map[string]string{"project": "cactus"}, nil, false},
		"a rule of a sub-bucket, inside it":        {rules, "reports/q1.txt", map[string]string{"project": "cactus"}, nil, true},
		"a key that only begins like it":           {rules, "reports-old/q1.txt", map[string]string{"project": "cactus"}, nil, false},
		"a value that is not allowed":              {rules, "reports/q1.txt", map[string]string{"project": "cactus", "stage": "secret"}, nil, true},
		"more tags than required are fine":         {rules, "a.txt", map[string]string{"project": "cactus", "extra": "1"}, nil, false},
		"catalogue only, all defined":              {bucket.TagRules{CatalogueOnly: true}, "a", map[string]string{"project": "x", "stage": "old"}, catalogue, false},
		"catalogue only, a tag nobody defined":     {bucket.TagRules{CatalogueOnly: true}, "a", map[string]string{"colour": "green"}, catalogue, true},
		"catalogue only, a value it does not list": {bucket.TagRules{CatalogueOnly: true}, "a", map[string]string{"stage": "secret"}, catalogue, true},
		"catalogue only, no catalogue to ask":      {bucket.TagRules{CatalogueOnly: true}, "a", map[string]string{"project": "x"}, nil, true},
		"catalogue only, no tags":                  {bucket.TagRules{CatalogueOnly: true}, "a", nil, catalogue, false},
		"no rules":                                 {bucket.TagRules{}, "a", map[string]string{"Any": "Thing"}, nil, false},
		"catalogue only, no tags, no catalogue":    {bucket.TagRules{CatalogueOnly: true}, "a", nil, nil, false},
		"lower case, as kept":                      {bucket.TagRules{Lowercase: true}, "a", map[string]string{"stage": "draft"}, nil, false},
		"lower case, a key that is not":            {bucket.TagRules{Lowercase: true}, "a", map[string]string{"Stage": "draft"}, nil, true},
		"lower case, a value that is not":          {bucket.TagRules{Lowercase: true}, "a", map[string]string{"stage": "Secret"}, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tt.rules.Check(tt.key, tt.tags, tt.defined)
			if tt.refused != errors.Is(err, domain.ErrTagRule) || (!tt.refused && err != nil) {
				t.Errorf("Check = %v, refused wanted: %v", err, tt.refused)
			}
			// A refusal is an invalid tag to whoever does not know more,
			// and never tells a value.
			if err != nil && (!errors.Is(err, domain.ErrInvalidTag) || strings.Contains(err.Error(), "secret")) {
				t.Errorf("the refusal: %v", err)
			}
		})
	}
}

// TestTagRulesKeep: tags are kept in lower case where the rules say so, two
// keys that become one are refused, and what is kept can be a tag at all.
func TestTagRulesKeep(t *testing.T) {
	t.Parallel()
	lower := bucket.TagRules{Lowercase: true}
	eleven := map[string]string{}
	for _, k := range strings.Split("a b c d e f g h i j k", " ") {
		eleven[k] = "v"
	}
	for name, tt := range map[string]struct {
		rules bucket.TagRules
		tags  map[string]string
		want  map[string]string
		err   error
	}{
		"as sent, without the rule":     {bucket.TagRules{}, map[string]string{"Stage": "Draft"}, map[string]string{"Stage": "Draft"}, nil},
		"in lower case, with it":        {lower, map[string]string{"Stage": "Draft"}, map[string]string{"stage": "draft"}, nil},
		"none is nil":                   {lower, map[string]string{}, nil, nil},
		"two keys that become one":      {lower, map[string]string{"Stage": "a", "stage": "b"}, nil, domain.ErrInvalidTag},
		"more than the limit":           {bucket.TagRules{}, eleven, nil, domain.ErrInvalidTag},
		"a key no tag can have":         {bucket.TagRules{}, map[string]string{"a&b": "v"}, nil, domain.ErrInvalidTag},
		"longer in bytes in lower case": {lower, map[string]string{"k": strings.Repeat("\u023a", 256)}, map[string]string{"k": strings.Repeat("\u2c65", 256)}, nil},
	} {
		got, err := tt.rules.Keep(tt.tags, object.MaxObjectTags)
		if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) || !maps.Equal(got, tt.want) || (tt.want == nil && got != nil) {
			t.Errorf("%s: Keep = %v, %v; want %v, %v", name, got, err, tt.want, tt.err)
		}
		if err == nil {
			// What is kept keeps the rule of the case.
			if err := tt.rules.Check("k", got, nil); err != nil {
				t.Errorf("%s: what Keep kept breaks the rules: %v", name, err)
			}
		}
	}
}

func FuzzParseTagRules(f *testing.F) {
	f.Add([]byte(`{"version":1,"lowercase":true,"catalogue_only":false,"required":[{"key":"a","values":["x"]}]}`))
	f.Add([]byte(`{"version":1,"required":[{"sub_bucket":"reports","key":"stage"}]}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, document []byte) {
		rules, err := bucket.ParseTagRules(document)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("ParseTagRules: %v", err)
			}
			return
		}
		// What was read can be written, and reads as the same.
		again, err := rules.Document()
		if err != nil {
			t.Fatalf("rules that were read cannot be written: %v", err)
		}
		back, err := bucket.ParseTagRules(again)
		if err != nil {
			t.Fatalf("rules that were written cannot be read: %v", err)
		}
		if len(back.Required) != len(rules.Required) || back.Lowercase != rules.Lowercase || back.CatalogueOnly != rules.CatalogueOnly {
			t.Fatalf("rules changed on the way: %+v, then %+v", rules, back)
		}
		// Checking never panics, whatever the rules.
		_ = rules.Check("reports/x", map[string]string{"a": "x"}, func(string) ([]string, bool) { return nil, false })
	})
}

func TestSubBucketNames(t *testing.T) {
	t.Parallel()
	for name, valid := range map[string]bool{
		"reports": true, "a": true, "2026": true, "team-a": true, strings.Repeat("a", 63): true,
		"": false, "Reports": false, "-a": false, "a-": false, "a/b": false, "a.b": false, "a_b": false,
		strings.Repeat("a", 64): false, "naïve": false, "a b": false,
	} {
		if err := bucket.ValidateSubBucketName(name); (err == nil) != valid || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("ValidateSubBucketName(%q) = %v, valid wanted: %v", name, err, valid)
		}
	}
	for key, want := range map[string]string{
		"reports/2026/q1.txt": "reports", "a/b": "a", "top.txt": "", "/leading": "", "Reports/x": "", "team-a/x": "team-a",
		strings.Repeat("a", 64) + "/x": "", "reports/": "reports",
	} {
		got, ok := bucket.SubBucketOf(key)
		if (want == "") == ok || (ok && got != want) {
			t.Errorf("SubBucketOf(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
	if (bucket.SubBucket{Name: "reports"}).Prefix() != "reports/" {
		t.Error("the prefix of a sub-bucket")
	}
}
