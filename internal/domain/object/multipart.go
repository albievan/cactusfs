package object

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Limits of a multipart upload, from the Amazon S3 multipart upload limits.
const (
	// MinPartSize is the smallest part there can be, the last one of an
	// upload excepted.
	MinPartSize = 5 << 20
	// MaxParts is the greatest part number.
	MaxParts = 10000
	// MaxMultipartSize is the largest object a multipart upload can make.
	MaxMultipartSize = 5 << 40
)

// KeyModePartHKDF names how the parts of a multipart object are encrypted:
// the object's data key is not used on any data; every part has a key of its
// own, derived from it. See PartKey.
const KeyModePartHKDF = "part-hkdf-v1"

// The uses of an upload's data key, kept apart by the info of the derivation.
const (
	partKeyInfo = "cactusfs/part/v1"
	sealKeyInfo = "cactusfs/manifest/v1"
)

// UploadKeyContext is the context the KMS binds the data key of a multipart
// upload to: the upload's ID, and that it is an upload's key. A key wrapped
// for a stream does not unwrap as one to derive part keys from, nor the
// other way round.
func UploadKeyContext(uploadID ID) []byte {
	return append([]byte("cactusfs/upload/v1/"), uploadID[:]...)
}

// PartKey derives the key that encrypts one part of a multipart upload from
// the upload's data key, with HKDF-SHA256. The part's number and data ID are
// bound into the derivation: every part - and every new upload of the same
// part number - has another key, so that no two streams are ever encrypted
// under one, and a part cannot be read as another.
func PartKey(uploadKey []byte, number int, partDataID ID) ([]byte, error) {
	if len(uploadKey) != KeySize {
		return nil, fmt.Errorf("%w: data key of %d bytes", domain.ErrInvalidArgument, len(uploadKey))
	}
	if err := ValidatePartNumber(number); err != nil {
		return nil, err
	}
	// The number in decimal between separators, then the ID, which has a
	// fixed length: no two inputs give one info.
	info := fmt.Sprintf("%s/%d/%s", partKeyInfo, number, partDataID[:])
	key, err := hkdf.Key(sha256.New, uploadKey, nil, info, KeySize)
	if err != nil {
		return nil, fmt.Errorf("derive part key: %w", err)
	}
	return key, nil
}

// Manifest seals the place of every part in a completed object. A part's
// stream authenticates its own bytes; the seal authenticates which parts
// make the object, in which order, at which offsets, and how many there are,
// so that an object with a part dropped, moved or cut off is not read as an
// object. It is an HMAC-SHA256 under a key derived from the upload's data
// key, and so can be made and checked only with that key.
type Manifest struct {
	key []byte
}

// NewManifest returns the manifest of the upload with the given data key.
func NewManifest(uploadKey []byte) (*Manifest, error) {
	if len(uploadKey) != KeySize {
		return nil, fmt.Errorf("%w: data key of %d bytes", domain.ErrInvalidArgument, len(uploadKey))
	}
	key, err := hkdf.Key(sha256.New, uploadKey, nil, sealKeyInfo, KeySize)
	if err != nil {
		return nil, fmt.Errorf("derive seal key: %w", err)
	}
	return &Manifest{key: key}, nil
}

// Clear forgets the key.
func (m *Manifest) Clear() {
	clear(m.key)
}

// Seal returns the seal of p as part p.Index of count, at p.Offset of an
// object of total bytes.
func (m *Manifest) Seal(p Part, count int, total int64) []byte {
	mac := hmac.New(sha256.New, m.key)
	// Six numbers in decimal, each closed by a separator, then the ID,
	// which has a fixed length: no two places give one message.
	_, _ = fmt.Fprintf(mac, "%d/%d/%d/%d/%d/%d/", p.Index, count, p.Number, p.Offset, p.Size, total)
	mac.Write(p.DataID[:])
	return mac.Sum(nil)
}

// Verify reports whether p carries the seal of its place. The error wraps
// domain.ErrIntegrity.
func (m *Manifest) Verify(p Part, count int, total int64) error {
	if !hmac.Equal(p.Seal, m.Seal(p, count, total)) {
		return fmt.Errorf("%w: part %d is not sealed as part %d of %d of this object", domain.ErrIntegrity, p.Number, p.Index+1, count)
	}
	return nil
}

// Extent names stored data: an ID and where the shards under it are.
type Extent struct {
	DataID ID
	Layout Layout
}

// Extent returns the stored data of a version that is not made of parts.
func (v Version) Extent() Extent {
	return Extent{DataID: v.DataID, Layout: v.Layout}
}

// Part is one uploaded part of a multipart upload: a stream of its own, with
// stored data of its own.
type Part struct {
	// Number is the part number, from 1 to MaxParts.
	Number int
	// DataID addresses the stored bytes of the part.
	DataID ID
	// Size is the length of the part as the client sees it.
	Size int64
	// ETag is the entity tag of the part, without quotes.
	ETag string
	// Checksums are the checksums the client declared for the part and
	// that were verified when it was stored, by the name of the header
	// that carried each.
	Checksums map[string]string
	// NoncePrefix is the random part of the nonces of the part's stream.
	NoncePrefix []byte
	// Layout says where the encrypted bytes of the part are.
	Layout     Layout
	ModifiedAt time.Time

	// The next three are set when the upload is completed. Index is the
	// place of the part among those of the object, from zero; Offset is
	// where its bytes begin in the object; Seal is the Manifest's seal of
	// both.
	Index  int
	Offset int64
	Seal   []byte
}

// Extent returns the stored data of the part.
func (p Part) Extent() Extent {
	return Extent{DataID: p.DataID, Layout: p.Layout}
}

// ValidatePartNumber reports whether n is a part number. The error wraps
// domain.ErrInvalidArgument.
func ValidatePartNumber(n int) error {
	if n < 1 || n > MaxParts {
		return fmt.Errorf("%w: part number %d is not between 1 and %d", domain.ErrInvalidArgument, n, MaxParts)
	}
	return nil
}

// Upload is a multipart upload in progress: what the object will be, and the
// key its parts are encrypted under.
type Upload struct {
	Bucket string
	Key    string
	// ID identifies the upload.
	ID          ID
	InitiatedAt time.Time
	// Initiator is who began the upload, as the audit trail names actors.
	Initiator string

	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	UserMetadata       map[string]string
	// Lock is what the completed version is to be kept by.
	Lock Lock
	// Encryption holds the wrapped data key of the upload; it has no nonce
	// prefix, because the key is used for no stream.
	Encryption Encryption
}

// CompletedPart names one part of a completion: its number, and the entity
// tag the client was given for it.
type CompletedPart struct {
	Number int
	ETag   string
	// Checksums are checksums the client lists for the part, by header
	// name. Each must be one that was verified when the part was stored.
	Checksums map[string]string
}

// CheckCompletion reports whether the parts a client lists make an object,
// given the parts that are stored. They must be listed in ascending order of
// number, each with the entity tag - and any checksum - of the stored part, and every part but
// the last must be at least MinPartSize. It returns the size of the object.
// A refusal wraps one of the domain.ErrPart... errors or
// domain.ErrEntityTooLarge.
func CheckCompletion(listed []CompletedPart, stored func(number int) (Part, bool)) (int64, error) {
	if len(listed) == 0 {
		return 0, fmt.Errorf("%w: a completion lists at least one part", domain.ErrInvalidArgument)
	}
	var total int64
	// before is the part listed before the current one, once it is known
	// to be stored.
	var before *Part
	previous := 0
	for _, want := range listed {
		if err := ValidatePartNumber(want.Number); err != nil {
			return 0, err
		}
		if want.Number <= previous {
			return 0, fmt.Errorf("%w: part %d follows part %d", domain.ErrPartOrder, want.Number, previous)
		}
		previous = want.Number
		part, ok := stored(want.Number)
		if !ok || part.ETag != want.ETag {
			return 0, fmt.Errorf("%w: part %d", domain.ErrPartNotFound, want.Number)
		}
		for name, value := range want.Checksums {
			if stored, ok := part.Checksums[name]; !ok || stored != value {
				return 0, fmt.Errorf("%w: part %d: checksum %s", domain.ErrPartNotFound, want.Number, name)
			}
		}
		// The part before this one is now known not to be the last.
		if before != nil && before.Size < MinPartSize {
			return 0, fmt.Errorf("%w: part %d is %d bytes, the minimum is %d", domain.ErrPartTooSmall, before.Number, before.Size, int64(MinPartSize))
		}
		before = &part
		total += part.Size
		if total > MaxMultipartSize {
			return 0, fmt.Errorf("%w: the parts add up to more than %d bytes", domain.ErrEntityTooLarge, int64(MaxMultipartSize))
		}
	}
	return total, nil
}
