package object

import (
	"fmt"
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
		if n := utf8.RuneCountInString(key); n == 0 || n > maxTagKey || !utf8.ValidString(key) {
			return fmt.Errorf("%w: tag key of %d characters", domain.ErrInvalidTag, n)
		}
		if n := utf8.RuneCountInString(value); n > maxTagValue || !utf8.ValidString(value) {
			return fmt.Errorf("%w: tag value of %d characters", domain.ErrInvalidTag, n)
		}
		if !tagText(key) || !tagText(value) {
			return fmt.Errorf("%w: a tag is letters, digits, spaces and + - = . _ : / @", domain.ErrInvalidTag)
		}
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
