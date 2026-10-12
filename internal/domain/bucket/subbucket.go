package bucket

import (
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// MaxSubBuckets is how many sub-buckets a bucket can have.
const MaxSubBuckets = 10

// SubBucket is a named part of a bucket: the keys that begin with its
// name and a slash. It is a range of keys and nothing else - S3 clients
// address its objects as keys of the bucket - with a policy of its own
// (ADR-0017). Sub-buckets do not nest.
type SubBucket struct {
	// Name is the first segment of the keys that belong to it.
	Name string
	// Policy is the sub-bucket's policy document, as it was sent; empty
	// when it has none.
	Policy []byte
	// CreatedAt is when the sub-bucket was registered.
	CreatedAt time.Time
}

// Prefix returns what the keys of the sub-bucket begin with.
func (s SubBucket) Prefix() string { return s.Name + "/" }

// ValidateSubBucketName reports whether name can name a sub-bucket: 1 to 63
// lower-case letters, digits and hyphens, beginning and ending with a letter
// or digit. No slash: a sub-bucket has no sub-buckets. The error wraps
// domain.ErrInvalidArgument.
//
// It is part of the replicated format: the state machine of every member
// refuses a sub-bucket by it, and it looks at bytes only so that every build
// decides alike. Changing what it accepts needs a rise of the entry schema.
func ValidateSubBucketName(name string) error {
	valid := len(name) >= 1 && len(name) <= 63
	for i := 0; valid && i < len(name); i++ {
		c := name[i]
		letterOrDigit := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		valid = letterOrDigit || (c == '-' && i > 0 && i < len(name)-1)
	}
	if !valid {
		return fmt.Errorf("%w: a sub-bucket is named with 1 to 63 lower-case letters, digits and hyphens", domain.ErrInvalidArgument)
	}
	return nil
}

// SubBucketOf returns the name of the sub-bucket a key would belong to: its
// first segment, when the key has one that can name a sub-bucket. Whether
// such a sub-bucket is registered is for the caller to look up.
func SubBucketOf(key string) (string, bool) {
	for i := 0; i < len(key) && i <= 63; i++ {
		if key[i] == '/' {
			name := key[:i]
			return name, ValidateSubBucketName(name) == nil
		}
	}
	return "", false
}
