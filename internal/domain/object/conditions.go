package object

import (
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Conditions are what a request says a version must be for the request to
// be carried out: the conditional headers of HTTP (RFC 9110, section 13).
type Conditions struct {
	// IfMatch lists entity tags, without quotes, one of which the version
	// must have; "*" stands for any version there is.
	IfMatch []string
	// IfNoneMatch lists entity tags none of which the version may have.
	IfNoneMatch []string
	// IfModifiedSince and IfUnmodifiedSince compare the version's time of
	// modification, to the second, as HTTP dates are.
	IfModifiedSince   time.Time
	IfUnmodifiedSince time.Time
}

// IsZero reports whether there are no conditions.
func (c Conditions) IsZero() bool {
	return len(c.IfMatch) == 0 && len(c.IfNoneMatch) == 0 && c.IfModifiedSince.IsZero() && c.IfUnmodifiedSince.IsZero()
}

func tagAmong(etag string, tags []string) bool {
	for _, tag := range tags {
		if tag == "*" || tag == etag {
			return true
		}
	}
	return false
}

// Check judges a version, by its entity tag and its time of modification,
// in the order HTTP gives: If-Match, else If-Unmodified-Since; then
// If-None-Match, else If-Modified-Since.
//
// A failed If-Match or If-Unmodified-Since wraps
// domain.ErrPreconditionFailed. A version that If-None-Match or
// If-Modified-Since says the caller already has wraps domain.ErrNotModified
// when the request reads it, and domain.ErrPreconditionFailed otherwise: a
// copy whose source "has not changed" is a copy that is not made.
func (c Conditions) Check(etag string, modified time.Time, reads bool) error {
	modified = modified.Truncate(time.Second)
	switch {
	case len(c.IfMatch) > 0:
		if !tagAmong(etag, c.IfMatch) {
			return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
		}
	case !c.IfUnmodifiedSince.IsZero():
		if modified.After(c.IfUnmodifiedSince) {
			return fmt.Errorf("%w: If-Unmodified-Since", domain.ErrPreconditionFailed)
		}
	}
	have := domain.ErrNotModified
	if !reads {
		have = domain.ErrPreconditionFailed
	}
	switch {
	case len(c.IfNoneMatch) > 0:
		if tagAmong(etag, c.IfNoneMatch) {
			return fmt.Errorf("%w: If-None-Match", have)
		}
	case !c.IfModifiedSince.IsZero():
		if !modified.After(c.IfModifiedSince) {
			return fmt.Errorf("%w: If-Modified-Since", have)
		}
	}
	return nil
}
