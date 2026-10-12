package bucket

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// ConfigTagRules is the kind of bucket configuration that holds the rules
// for the tags of the bucket's objects. It exists from entry schema 3 on
// (ADR-0018); ValidConfigKind does not name it, and the metadata store
// accepts it only where every member of the cluster knows it.
const ConfigTagRules = "tagrules"

// Bounds of tag rules.
const (
	// MaxRequiredTags is how many tags a bucket can require.
	MaxRequiredTags = 10
	// MaxAllowedValues is how many values a required tag can be held to.
	MaxAllowedValues = 50
	maxRuleText      = 256
)

// tagRulesVersion is the version of the stored document.
const tagRulesVersion = 1

// RequiredTag is one tag that an object must carry.
type RequiredTag struct {
	// SubBucket, when not empty, confines the rule to keys of that
	// sub-bucket: keys that begin with its name and a slash.
	SubBucket string `json:"sub_bucket,omitempty"`
	// Key is the key of the tag, as it is compared: in lower case when
	// the rules lower-case tags.
	Key string `json:"key"`
	// Values, when not empty, are the only values the tag may have.
	Values []string `json:"values,omitempty"`
}

// TagRules are the rules for the tags of a bucket's objects.
type TagRules struct {
	// Lowercase says that the keys and values of tags are stored in lower
	// case, whatever case they are sent in.
	Lowercase bool `json:"lowercase"`
	// CatalogueOnly says that an object may carry only tags the
	// deployment's catalogue defines.
	CatalogueOnly bool `json:"catalogue_only"`
	// Required are the tags every object must carry.
	Required []RequiredTag `json:"required"`
}

type tagRulesDocument struct {
	Version int `json:"version"`
	TagRules
}

func invalidTagRules(format string, args ...any) error {
	return fmt.Errorf("%w: tag rules: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Validate reports whether the rules can be kept.
func (r TagRules) Validate() error {
	if len(r.Required) > MaxRequiredTags {
		return invalidTagRules("more than %d required tags", MaxRequiredTags)
	}
	seen := map[string]bool{}
	for _, tag := range r.Required {
		if tag.Key == "" || len(tag.Key) > maxRuleText {
			return invalidTagRules("a required tag has a key of 1 to %d bytes", maxRuleText)
		}
		if r.Lowercase && tag.Key != strings.ToLower(tag.Key) {
			return invalidTagRules("the bucket lower-cases its tags: write the key %q in lower case", tag.Key)
		}
		if tag.SubBucket != "" {
			if err := ValidateSubBucketName(tag.SubBucket); err != nil {
				return invalidTagRules("%v", err)
			}
		}
		if id := tag.SubBucket + "\x00" + tag.Key; seen[id] {
			return invalidTagRules("the tag %q is required twice", tag.Key)
		} else {
			seen[id] = true
		}
		if len(tag.Values) > MaxAllowedValues {
			return invalidTagRules("more than %d values for the tag %q", MaxAllowedValues, tag.Key)
		}
		for i, value := range tag.Values {
			if len(value) > maxRuleText {
				return invalidTagRules("a value of the tag %q is longer than %d bytes", tag.Key, maxRuleText)
			}
			if r.Lowercase && value != strings.ToLower(value) {
				return invalidTagRules("the bucket lower-cases its tags: write the value %q in lower case", value)
			}
			if slices.Contains(tag.Values[:i], value) {
				return invalidTagRules("the value %q of the tag %q is given twice", value, tag.Key)
			}
		}
	}
	return nil
}

// Document returns the rules as the document that is stored.
func (r TagRules) Document() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.Required == nil {
		r.Required = []RequiredTag{}
	}
	return json.Marshal(tagRulesDocument{Version: tagRulesVersion, TagRules: r})
}

// ParseTagRules reads a stored document. An empty document is no rules. The
// error wraps domain.ErrInvalidArgument, or domain.ErrUnsupportedFormat for
// a document of a version this code does not know.
func ParseTagRules(document []byte) (TagRules, error) {
	if len(document) == 0 {
		return TagRules{}, nil
	}
	if len(document) > MaxConfigBytes {
		return TagRules{}, invalidTagRules("the document is longer than %d bytes", MaxConfigBytes)
	}
	var doc tagRulesDocument
	decoder := json.NewDecoder(strings.NewReader(string(document)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return TagRules{}, invalidTagRules("%v", err)
	}
	if decoder.More() {
		return TagRules{}, invalidTagRules("something follows the document")
	}
	if doc.Version != tagRulesVersion {
		return TagRules{}, fmt.Errorf("%w: tag rules of version %d", domain.ErrUnsupportedFormat, doc.Version)
	}
	if err := doc.Validate(); err != nil {
		return TagRules{}, err
	}
	return doc.TagRules, nil
}

// Defined says, for a tag key, whether the catalogue defines it and which
// values it allows; no values means any value.
type Defined func(key string) (values []string, ok bool)

// Check reports whether an object with this key may carry these tags. The
// tags are as they will be stored: already in lower case where the rules
// say so. defined is asked only when the rules hold tags to the catalogue,
// and may be nil otherwise. The error wraps domain.ErrTagRule and names the
// rule that was broken, never a tag's value.
func (r TagRules) Check(key string, tags map[string]string, defined Defined) error {
	for _, rule := range r.Required {
		if rule.SubBucket != "" && !strings.HasPrefix(key, rule.SubBucket+"/") {
			continue
		}
		value, ok := tags[rule.Key]
		if !ok {
			return fmt.Errorf("%w: the tag %q is required", domain.ErrTagRule, rule.Key)
		}
		if len(rule.Values) > 0 && !slices.Contains(rule.Values, value) {
			return fmt.Errorf("%w: the tag %q has a value that is not one of those allowed", domain.ErrTagRule, rule.Key)
		}
	}
	if r.CatalogueOnly {
		if defined == nil {
			return fmt.Errorf("%w: the bucket takes only tags of the catalogue, which could not be read", domain.ErrTagRule)
		}
		for tagKey, value := range tags {
			values, ok := defined(tagKey)
			if !ok {
				return fmt.Errorf("%w: the tag %q is not in the catalogue", domain.ErrTagRule, tagKey)
			}
			if len(values) > 0 && !slices.Contains(values, value) {
				return fmt.Errorf("%w: the tag %q has a value the catalogue does not list", domain.ErrTagRule, tagKey)
			}
		}
	}
	return nil
}

// Empty reports whether the rules ask for nothing.
func (r TagRules) Empty() bool {
	return !r.Lowercase && !r.CatalogueOnly && len(r.Required) == 0
}
