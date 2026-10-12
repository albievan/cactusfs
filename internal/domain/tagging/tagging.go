// Package tagging holds the deployment's catalogue of tags: the tags an
// administrator defines for users to pick from (ADR-0018). The rules a bucket
// makes for the tags of its objects are in package bucket; what a tag may be
// at all is in package object.
package tagging

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
)

// Bounds of the catalogue. They are part of the replicated format: the state
// machine of every member refuses a definition by them, so changing one
// needs a rise of the entry schema.
const (
	// MaxDefinitions is how many definitions the catalogue can hold.
	MaxDefinitions = 200
	// MaxValues is how many values a definition can list.
	MaxValues = 50
	// MaxKeyBytes bounds the key of a definition, MaxDescriptionBytes its
	// description and MaxValueBytes each of its values.
	MaxKeyBytes         = 128
	MaxDescriptionBytes = 256
	MaxValueBytes       = 256
)

// Definition is one tag of the catalogue: its key, what it is for, and,
// optionally, the only values it may have. The catalogue is always in lower
// case, whatever a bucket does with the case of its tags.
type Definition struct {
	// Key is the key of the tag.
	Key string
	// Description says what the tag is for.
	Description string
	// Values, when not empty, are the values the tag may have, in the
	// order they were given. Without them it may have any value.
	Values []string
	// CreatedAt is when the tag was first defined.
	CreatedAt time.Time
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: tag definition: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Validate reports whether the definition can be kept: a key of 1 to 128
// bytes in lower case, a description of at most 256 bytes, and at most 50
// values, each of at most 256 bytes, in lower case and given once. The error
// wraps domain.ErrInvalidArgument.
func (d Definition) Validate() error {
	switch {
	case d.Key == "" || len(d.Key) > MaxKeyBytes:
		return invalid("the key has 1 to %d bytes", MaxKeyBytes)
	case !utf8.ValidString(d.Key) || d.Key != strings.ToLower(d.Key):
		return invalid("the key %q is written in lower case", d.Key)
	case len(d.Description) > MaxDescriptionBytes || !utf8.ValidString(d.Description):
		return invalid("the description has at most %d bytes", MaxDescriptionBytes)
	case len(d.Values) > MaxValues:
		return invalid("more than %d values", MaxValues)
	}
	for i, value := range d.Values {
		switch {
		case len(value) > MaxValueBytes:
			return invalid("a value is longer than %d bytes", MaxValueBytes)
		case !utf8.ValidString(value) || value != strings.ToLower(value):
			return invalid("the value %q is written in lower case", value)
		case slices.Contains(d.Values[:i], value):
			return invalid("the value %q is given twice", value)
		}
	}
	return nil
}

// Allows reports whether the tag may have the value: any, when the
// definition lists none.
func (d Definition) Allows(value string) bool {
	return len(d.Values) == 0 || slices.Contains(d.Values, value)
}

// Clone returns a copy that shares nothing with d.
func (d Definition) Clone() Definition {
	d.Values = slices.Clone(d.Values)
	return d
}
