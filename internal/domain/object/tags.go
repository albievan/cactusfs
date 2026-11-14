package object

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
)

// Limits of tags, from the Amazon S3 API reference.
const (
	// MaxObjectTags is the greatest number of tags of an object version.
	MaxObjectTags = 10
	// MaxBucketTags is the greatest number of tags of a bucket.
	MaxBucketTags = 50
	maxTagKey     = 128
	maxTagValue   = 256
)

// ValidateTags reports whether tags can be kept: at most limit of them, each
// with a key of 1 to 128 characters and a value of at most 256. The error
// wraps domain.ErrInvalidTag.
func ValidateTags(tags map[string]string, limit int) error {
	if len(tags) > limit {
		return fmt.Errorf("%w: more than %d tags", domain.ErrInvalidTag, limit)
	}
	for key, value := range tags {
		if err := ValidateTagKey(key); err != nil {
			return err
		}
		if err := ValidateTagValue(value); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTagKey reports whether key can be the key of a tag: 1 to 128
// characters, each a letter, a digit, a space or one of + - = . _ : / @.
// The error wraps domain.ErrInvalidTag.
func ValidateTagKey(key string) error {
	if n := utf8.RuneCountInString(key); n == 0 || n > maxTagKey || !utf8.ValidString(key) {
		return fmt.Errorf("%w: tag key of %d characters", domain.ErrInvalidTag, n)
	}
	if !tagText(key) {
		return fmt.Errorf("%w: a tag is letters, digits, spaces and + - = . _ : / @", domain.ErrInvalidTag)
	}
	return nil
}

// ValidateTagValue reports whether value can be the value of a tag: at most
// 256 characters, of those a key may have. The error wraps
// domain.ErrInvalidTag.
func ValidateTagValue(value string) error {
	if n := utf8.RuneCountInString(value); n > maxTagValue || !utf8.ValidString(value) {
		return fmt.Errorf("%w: tag value of %d characters", domain.ErrInvalidTag, n)
	}
	if !tagText(value) {
		return fmt.Errorf("%w: a tag is letters, digits, spaces and + - = . _ : / @", domain.ErrInvalidTag)
	}
	return nil
}

// tagText reports whether s consists of the characters a tag may have, as
// the Amazon S3 reference lists them.
func tagText(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r) && !strings.ContainsRune("+-=._:/@", r) {
			return false
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// NormalizeTags returns the tags with every key and value in lower case. Two
// keys that become the same key are refused rather than one of them chosen:
// the error wraps domain.ErrInvalidTag. Normalizing what is normalized
// changes nothing. Whether the result can be kept is for ValidateTags, after:
// lower-casing can lengthen a string.
func NormalizeTags(tags map[string]string) (map[string]string, error) {
	if tags == nil {
		return nil, nil
	}
	out := make(map[string]string, len(tags))
	for key, value := range tags {
		lower := strings.ToLower(key)
		if _, twice := out[lower]; twice {
			return nil, fmt.Errorf("%w: tags %q are the same tag in lower case", domain.ErrInvalidTag, lower)
		}
		out[lower] = strings.ToLower(value)
	}
	return out, nil
}

// Tag is one tag as a caller gives it: a key and its value. The use cases
// take tags as a list of them, never as a map, so that a key given twice
// reaches the domain and is refused here (TagMap) rather than one of its
// values chosen on the way in.
type Tag struct {
	Key   string
	Value string
}

// TagMap returns a list of tags as a map from key to value; an empty list is
// nil. A key given twice is refused, whatever its values: which one it has
// must not be ours to choose. The error wraps domain.ErrInvalidTag and names
// the key, never a value. Whether the tags can be kept is for ValidateTags.
func TagMap(list []Tag) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(list))
	for _, tag := range list {
		if _, twice := out[tag.Key]; twice {
			return nil, fmt.Errorf("%w: the tag %q is given twice", domain.ErrInvalidTag, tag.Key)
		}
		out[tag.Key] = tag.Value
	}
	return out, nil
}

// TagList returns tags as a list in the order of their keys: what TagMap
// takes back to the same map.
func TagList(tags map[string]string) []Tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]Tag, 0, len(tags))
	for key, value := range tags {
		out = append(out, Tag{Key: key, Value: value})
	}
	slices.SortFunc(out, func(a, b Tag) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// ParseTagging reads the tags of an object version given as a URL query
// string - "stage=draft&team=finance", each key and value URL-encoded - as
// S3's x-amz-tagging and the admin API's X-CactusFS-Tagging give them. It
// returns every pair in the order given, a key given twice included: that
// one is TagMap's to refuse, as for tags given any other way; and how many
// there may be is ValidateTags', after the caller is authorized. An empty
// string is no tags (an empty list, not nil). It reads exactly what
// url.ParseQuery reads, and refuses what it refuses - a semicolon, an
// escape that is none, more pairs than it takes - with an error that wraps
// domain.ErrInvalidArgument and holds nothing of the string.
func ParseTagging(query string) ([]Tag, error) {
	if _, err := url.ParseQuery(query); err != nil {
		return nil, fmt.Errorf("%w: tags are not a query string", domain.ErrInvalidArgument)
	}
	// What url.ParseQuery takes, in the order given: each part between
	// ampersands that is not empty, split at its first equals sign.
	tags := []Tag{}
	for pair := range strings.SplitSeq(query, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, fmt.Errorf("%w: tags are not a query string", domain.ErrInvalidArgument)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, fmt.Errorf("%w: tags are not a query string", domain.ErrInvalidArgument)
		}
		tags = append(tags, Tag{Key: key, Value: value})
	}
	return tags, nil
}
