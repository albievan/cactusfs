package object

import (
	"fmt"

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
