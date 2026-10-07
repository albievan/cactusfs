package object

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/albievan/cactusfs/internal/domain"
)

// Parameters of the streaming encryption format. They are part of the on-disk
// format: changing any of them needs a new scheme name and an ADR
// (docs/adr/0002-encryption.md).
const (
	// StreamSchemeV1 names the format implemented in this file.
	StreamSchemeV1 = "AES256-GCM-STREAM-V1"
	// KeySize is the size of a data key: AES-256.
	KeySize = 32
	// NoncePrefixSize is the size of the random part of a segment nonce.
	NoncePrefixSize = 4
	// TagSize is the size of the GCM authentication tag of each segment.
	TagSize = 16
	// DefaultSegmentSize is the plaintext size of every segment but the last.
	DefaultSegmentSize = 64 << 10
	// MaxSegmentSize bounds the segment size a stored object may declare.
	MaxSegmentSize = 16 << 20
)

// Stream encrypts and decrypts one object with scheme StreamSchemeV1.
//
// The plaintext is cut into segments of a fixed size; only the last segment
// may be shorter, and an empty object is a single empty segment. Every segment
// is sealed with AES-256-GCM under the object's own data key:
//
//	nonce = noncePrefix (4 random bytes) || segment index (8 bytes, big endian)
//	AAD   = segment index (8 bytes, big endian) || final (1 byte) || caller AAD
//
// The data key is used for one object only, so a nonce never repeats under a
// key. Binding the index into the AAD makes reordered or substituted segments
// fail authentication; binding the final flag makes a truncated stream fail,
// because its last segment was not sealed as final.
type Stream struct {
	aead        cipher.AEAD
	prefix      [NoncePrefixSize]byte
	aad         []byte
	segmentSize int
}

// NewStream returns a Stream for one object. key is the object's data key,
// noncePrefix its random nonce prefix and aad the associated data that ties
// the ciphertext to the object.
func NewStream(key, noncePrefix, aad []byte, segmentSize int) (*Stream, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("object.NewStream: %w: key is %d bytes, want %d", domain.ErrInvalidArgument, len(key), KeySize)
	}
	if len(noncePrefix) != NoncePrefixSize {
		return nil, fmt.Errorf("object.NewStream: %w: nonce prefix is %d bytes, want %d",
			domain.ErrInvalidArgument, len(noncePrefix), NoncePrefixSize)
	}
	if segmentSize <= 0 || segmentSize > MaxSegmentSize {
		return nil, fmt.Errorf("object.NewStream: %w: segment size %d", domain.ErrInvalidArgument, segmentSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("object.NewStream: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("object.NewStream: %w", err)
	}
	s := &Stream{aead: aead, aad: append([]byte(nil), aad...), segmentSize: segmentSize}
	copy(s.prefix[:], noncePrefix)
	return s, nil
}

// SegmentSize returns the plaintext size of every segment but the last.
func (s *Stream) SegmentSize() int {
	return s.segmentSize
}

// SegmentCount returns the number of segments of an object of the given
// plaintext size.
func (s *Stream) SegmentCount(plainSize int64) int64 {
	if plainSize <= 0 {
		return 1
	}
	return (plainSize + int64(s.segmentSize) - 1) / int64(s.segmentSize)
}

// CiphertextSize returns the length of the encrypted stream of an object of
// the given plaintext size.
func (s *Stream) CiphertextSize(plainSize int64) int64 {
	return plainSize + s.SegmentCount(plainSize)*TagSize
}

// Section is the part of an encrypted stream that covers a plaintext range.
type Section struct {
	// FirstSegment is the index of the first segment to decrypt.
	FirstSegment int64
	// Segments is the number of segments to decrypt.
	Segments int64
	// Offset and Length locate those segments in the encrypted stream.
	Offset int64
	Length int64
	// Skip is the number of plaintext bytes to discard from the first
	// segment before the range begins.
	Skip int64
}

// Locate returns the section of the encrypted stream that must be read to
// serve r from an object of the given plaintext size. A zero-length range is
// only meaningful for an empty object and selects its single segment.
func (s *Stream) Locate(r Range, plainSize int64) Section {
	seg := int64(s.segmentSize)
	first := r.Offset / seg
	last := first
	if r.Length > 0 {
		last = (r.Offset + r.Length - 1) / seg
	}
	end := min((last+1)*(seg+TagSize), s.CiphertextSize(plainSize))
	return Section{
		FirstSegment: first,
		Segments:     last - first + 1,
		Offset:       first * (seg + TagSize),
		Length:       end - first*(seg+TagSize),
		Skip:         r.Offset - first*seg,
	}
}

func (s *Stream) nonceAndAAD(index uint64, final bool) (nonce, aad []byte) {
	nonce = make([]byte, 0, NoncePrefixSize+8)
	nonce = append(nonce, s.prefix[:]...)
	nonce = binary.BigEndian.AppendUint64(nonce, index)

	// Fixed-width fields first, so that the layout is unambiguous whatever
	// the length of the caller's associated data.
	aad = make([]byte, 0, 9+len(s.aad))
	aad = binary.BigEndian.AppendUint64(aad, index)
	if final {
		aad = append(aad, 1)
	} else {
		aad = append(aad, 0)
	}
	aad = append(aad, s.aad...)
	return nonce, aad
}

// seal appends the sealed segment to dst.
func (s *Stream) seal(dst, plaintext []byte, index uint64, final bool) []byte {
	nonce, aad := s.nonceAndAAD(index, final)
	return s.aead.Seal(dst, nonce, plaintext, aad)
}

// open appends the opened segment to dst. The error wraps domain.ErrIntegrity.
func (s *Stream) open(dst, ciphertext []byte, index uint64, final bool) ([]byte, error) {
	nonce, aad := s.nonceAndAAD(index, final)
	plaintext, err := s.aead.Open(dst, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: segment %d failed authentication", domain.ErrIntegrity, index)
	}
	return plaintext, nil
}

// Encrypt returns a reader that yields the encrypted stream of the plaintext
// read from src.
func (s *Stream) Encrypt(src io.Reader) io.Reader {
	return &encryptReader{
		stream:  s,
		src:     src,
		current: make([]byte, 0, s.segmentSize),
		next:    make([]byte, 0, s.segmentSize),
		sealed:  make([]byte, 0, s.segmentSize+TagSize),
	}
}

type encryptReader struct {
	stream  *Stream
	src     io.Reader
	current []byte // the segment about to be sealed
	next    []byte // the segment after it, read ahead to learn whether current is final
	sealed  []byte // backing array of pending
	pending []byte // sealed bytes not yet returned
	index   uint64
	started bool
	done    bool
	err     error
}

func (r *encryptReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.sealNext(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *encryptReader) sealNext() error {
	var err error
	if !r.started {
		r.started = true
		if r.current, err = readSegment(r.src, r.current); err != nil {
			return err
		}
	}
	// A short segment means the source is exhausted; do not read it again.
	if len(r.current) == cap(r.current) {
		if r.next, err = readSegment(r.src, r.next); err != nil {
			return err
		}
	} else {
		r.next = r.next[:0]
	}

	final := len(r.next) == 0
	r.pending = r.stream.seal(r.sealed[:0], r.current, r.index, final)
	r.index++
	if final {
		r.done = true
		return nil
	}
	r.current, r.next = r.next, r.current
	return nil
}

// readSegment fills buf up to its capacity from src. Only io.EOF ends the
// plaintext: the returned slice is then simply shorter, possibly empty. Any
// other error is returned, in particular io.ErrUnexpectedEOF, which is how a
// transport reports a body that was cut short. Treating that as the end would
// seal a truncated object as if it were complete.
func readSegment(src io.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	for len(buf) < cap(buf) {
		n, err := src.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return buf[:0], fmt.Errorf("object.Stream: read plaintext: %w", err)
		}
	}
	return buf, nil
}

// Decrypt returns a reader that yields the plaintext of a section of the
// encrypted stream. src must deliver exactly the bytes of that section;
// plainSize is the plaintext size of the whole object.
//
// The reader returns an error wrapping domain.ErrIntegrity, and no plaintext
// of the affected segment, when a segment fails authentication, when the
// section ends early, or when bytes follow it.
func (s *Stream) Decrypt(src io.Reader, section Section, plainSize int64) io.Reader {
	return &decryptReader{
		stream: s,
		src:    src,
		index:  section.FirstSegment,
		end:    section.FirstSegment + section.Segments,
		total:  s.SegmentCount(plainSize),
		sealed: make([]byte, s.segmentSize+TagSize),
		opened: make([]byte, 0, s.segmentSize),
	}
}

type decryptReader struct {
	stream  *Stream
	src     io.Reader
	index   int64 // next segment to open
	end     int64 // one past the last segment of the section
	total   int64 // segments in the whole object
	sealed  []byte
	opened  []byte // backing array of pending
	pending []byte // plaintext not yet returned
	err     error
}

func (r *decryptReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.openNext(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *decryptReader) openNext() error {
	if r.index >= r.end {
		// The section is complete; anything that follows was not written by us.
		var probe [1]byte
		switch n, err := io.ReadFull(r.src, probe[:]); {
		case n > 0:
			return fmt.Errorf("%w: unexpected bytes after segment %d", domain.ErrIntegrity, r.index-1)
		case errors.Is(err, io.EOF):
			return io.EOF
		default:
			return fmt.Errorf("object.Stream: read ciphertext: %w", err)
		}
	}

	final := r.index == r.total-1
	n, err := io.ReadFull(r.src, r.sealed)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// Only the final segment may be short, and never shorter than its tag.
		if !final || n < TagSize {
			return fmt.Errorf("%w: stream ends inside segment %d", domain.ErrIntegrity, r.index)
		}
	default:
		return fmt.Errorf("object.Stream: read ciphertext: %w", err)
	}

	if r.index < 0 {
		return fmt.Errorf("%w: negative segment index", domain.ErrInvalidArgument)
	}
	plaintext, err := r.stream.open(r.opened[:0], r.sealed[:n], uint64(r.index), final)
	if err != nil {
		return err
	}
	r.pending = plaintext
	r.index++
	// An empty final segment yields no bytes; loop again to report EOF.
	return nil
}
