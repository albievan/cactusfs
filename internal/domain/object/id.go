// Package object holds the domain model for objects: identifiers, keys,
// version metadata, byte ranges, listing rules and the streaming encryption
// format.
package object

import (
	"encoding/hex"
	"fmt"
	"io"

	"github.com/albievan/cactusfs/internal/domain"
)

// IDSize is the size of an ID in bytes.
const IDSize = 16

// ID is an opaque, randomly generated 128-bit identifier. Stored data and
// object versions are addressed by IDs, never by bucket names or object keys,
// so nothing a client chooses ever becomes part of a filesystem path.
type ID [IDSize]byte

// NewID reads a fresh ID from random, which must be a cryptographically secure
// source.
func NewID(random io.Reader) (ID, error) {
	var id ID
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return ID{}, fmt.Errorf("object.NewID: %w", err)
	}
	return id, nil
}

// ParseID parses the lowercase hexadecimal form produced by String.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != hex.EncodedLen(IDSize) {
		return ID{}, fmt.Errorf("%w: %q: want %d hexadecimal characters", domain.ErrInvalidID, s, hex.EncodedLen(IDSize))
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ID{}, fmt.Errorf("%w: %q: want lowercase hexadecimal", domain.ErrInvalidID, s)
		}
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return ID{}, fmt.Errorf("%w: %q", domain.ErrInvalidID, s)
	}
	return id, nil
}

// String returns the ID as 32 lowercase hexadecimal characters.
func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

// IsZero reports whether the ID is the zero value, which is never generated.
func (id ID) IsZero() bool {
	return id == ID{}
}
