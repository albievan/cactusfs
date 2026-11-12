package object

import (
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Range is a resolved, non-empty span of an object's bytes.
type Range struct {
	// Offset is the position of the first byte.
	Offset int64
	// Length is the number of bytes.
	Length int64
}

// RangeSpec is a byte range as a client asks for it, before the size of the
// object is known. It mirrors the three forms of an HTTP byte range.
type RangeSpec struct {
	// First is the position of the first byte. Ignored when Suffix is set.
	First int64
	// Last is the position of the last byte, inclusive, or -1 for "to the
	// end". Ignored when Suffix is set.
	Last int64
	// Suffix, when greater than zero, asks for the final Suffix bytes.
	Suffix int64
	// If, when not nil, is the If-Range of the request: the range is
	// served only from the version it names, and from any other the
	// whole object is (RFC 9110, section 13.1.5).
	If *RangeValidator
}

// Applies reports whether the range is to be served from a version with
// the given entity tag and time of modification: always, unless its If
// names another version.
func (s RangeSpec) Applies(etag string, modified time.Time) bool {
	return s.If == nil || s.If.Holds(etag, modified)
}

// RangeValidator names the version a range was asked of: by its entity tag,
// or by its time of modification. One that names neither - the If-Range of
// a weak entity tag, or one that could not be read - holds for no version,
// so that a range of what may be another object is never served.
type RangeValidator struct {
	// ETag, without quotes, is the entity tag the version must have,
	// compared strongly.
	ETag string
	// Date, when ETag is empty, is the time of modification the version
	// must have, to the second, as HTTP dates are.
	Date time.Time
}

// Holds reports whether the validator names a version with the given entity
// tag and time of modification.
func (v RangeValidator) Holds(etag string, modified time.Time) bool {
	switch {
	case v.ETag != "":
		return v.ETag == etag
	case !v.Date.IsZero():
		return modified.Truncate(time.Second).Equal(v.Date)
	default:
		return false
	}
}

// Resolve applies the request to an object of the given size. The returned
// error wraps domain.ErrInvalidRange when no byte can be served.
func (s RangeSpec) Resolve(size int64) (Range, error) {
	if s.Suffix > 0 {
		if size == 0 {
			return Range{}, fmt.Errorf("%w: suffix range of an empty object", domain.ErrInvalidRange)
		}
		length := min(s.Suffix, size)
		return Range{Offset: size - length, Length: length}, nil
	}
	if s.First < 0 || s.Suffix < 0 || (s.Last >= 0 && s.Last < s.First) {
		return Range{}, fmt.Errorf("%w: malformed range", domain.ErrInvalidRange)
	}
	if s.First >= size {
		return Range{}, fmt.Errorf("%w: first byte %d is beyond the object size %d", domain.ErrInvalidRange, s.First, size)
	}
	last := s.Last
	if last < 0 || last >= size {
		last = size - 1
	}
	return Range{Offset: s.First, Length: last - s.First + 1}, nil
}
