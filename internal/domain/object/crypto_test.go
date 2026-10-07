package object_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

const testSegment = 64

func testKey(fill byte) []byte { return bytes.Repeat([]byte{fill}, object.KeySize) }

func newStream(t testing.TB, key, prefix, aad []byte, segment int) *object.Stream {
	t.Helper()
	s, err := object.NewStream(key, prefix, aad, segment)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	return s
}

func defaultStream(t testing.TB) *object.Stream {
	t.Helper()
	return newStream(t, testKey(1), []byte{1, 2, 3, 4}, []byte("data-id"), testSegment)
}

func encrypt(t testing.TB, s *object.Stream, plaintext []byte) []byte {
	t.Helper()
	out, err := io.ReadAll(s.Encrypt(bytes.NewReader(plaintext)))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return out
}

func decryptAll(s *object.Stream, ciphertext []byte, size int64) ([]byte, error) {
	section := s.Locate(object.Range{Offset: 0, Length: size}, size)
	return io.ReadAll(s.Decrypt(bytes.NewReader(ciphertext), section, size))
}

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func TestNewStreamValidatesParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     []byte
		prefix  []byte
		segment int
	}{
		{"short key", make([]byte, 16), make([]byte, 4), 64},
		{"long key", make([]byte, 33), make([]byte, 4), 64},
		{"short prefix", testKey(1), make([]byte, 3), 64},
		{"long prefix", testKey(1), make([]byte, 12), 64},
		{"zero segment", testKey(1), make([]byte, 4), 0},
		{"negative segment", testKey(1), make([]byte, 4), -1},
		{"huge segment", testKey(1), make([]byte, 4), object.MaxSegmentSize + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := object.NewStream(tt.key, tt.prefix, nil, tt.segment); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("NewStream error = %v, want %v", err, domain.ErrInvalidArgument)
			}
		})
	}
}

func TestStreamRoundTrip(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, 1, testSegment - 1, testSegment, testSegment + 1, 2 * testSegment, 5*testSegment + 7} {
		s := defaultStream(t)
		plaintext := randomBytes(t, size)
		ciphertext := encrypt(t, s, plaintext)

		if got, want := int64(len(ciphertext)), s.CiphertextSize(int64(size)); got != want {
			t.Errorf("size %d: ciphertext is %d bytes, CiphertextSize says %d", size, got, want)
		}
		if size > 0 && bytes.Contains(ciphertext, plaintext[:min(size, 16)]) && size >= 16 {
			t.Errorf("size %d: ciphertext contains plaintext", size)
		}
		got, err := decryptAll(s, ciphertext, int64(size))
		if err != nil {
			t.Fatalf("size %d: decrypt: %v", size, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("size %d: round trip changed the data", size)
		}
	}
}

func TestStreamEncryptHandlesAwkwardReaders(t *testing.T) {
	t.Parallel()

	s := defaultStream(t)
	plaintext := randomBytes(t, 3*testSegment+5)
	want := encrypt(t, s, plaintext)

	for name, src := range map[string]io.Reader{
		"one byte at a time": iotest.OneByteReader(bytes.NewReader(plaintext)),
		"data with EOF":      iotest.DataErrReader(bytes.NewReader(plaintext)),
		"half reads":         iotest.HalfReader(bytes.NewReader(plaintext)),
	} {
		got, err := io.ReadAll(iotest.OneByteReader(s.Encrypt(src)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: ciphertext differs from a plain read", name)
		}
	}
}

func TestStreamEncryptReportsSourceErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	s := defaultStream(t)
	src := io.MultiReader(bytes.NewReader(randomBytes(t, testSegment+3)), iotest.ErrReader(boom))

	if _, err := io.ReadAll(s.Encrypt(src)); !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
}

func TestStreamRangeReads(t *testing.T) {
	t.Parallel()

	s := defaultStream(t)
	const size = 4*testSegment + 10
	plaintext := randomBytes(t, size)
	ciphertext := encrypt(t, s, plaintext)

	for offset := int64(0); offset < size; offset += 13 {
		for _, length := range []int64{1, 2, testSegment - 1, testSegment, testSegment + 1, size} {
			length = min(length, size-offset)
			r := object.Range{Offset: offset, Length: length}
			section := s.Locate(r, size)

			src := bytes.NewReader(ciphertext[section.Offset : section.Offset+section.Length])
			plain := s.Decrypt(src, section, size)
			if _, err := io.CopyN(io.Discard, plain, section.Skip); err != nil {
				t.Fatalf("range %+v: skip: %v", r, err)
			}
			got := make([]byte, length)
			if _, err := io.ReadFull(plain, got); err != nil {
				t.Fatalf("range %+v: read: %v", r, err)
			}
			if !bytes.Equal(got, plaintext[offset:offset+length]) {
				t.Fatalf("range %+v: wrong bytes", r)
			}
		}
	}
}

// TestStreamDetectsTampering is the security property of the format: no
// modification of the stored stream yields plaintext without an error.
func TestStreamDetectsTampering(t *testing.T) {
	t.Parallel()

	const size = 3*testSegment + 20
	sealed := testSegment + object.TagSize
	s := defaultStream(t)
	plaintext := randomBytes(t, size)
	ciphertext := encrypt(t, s, plaintext)

	swap := func(c []byte, i, j int) []byte {
		out := bytes.Clone(c)
		copy(out[i*sealed:(i+1)*sealed], c[j*sealed:(j+1)*sealed])
		copy(out[j*sealed:(j+1)*sealed], c[i*sealed:(i+1)*sealed])
		return out
	}

	tests := map[string][]byte{
		"bit flip in first segment":    flip(ciphertext, 0),
		"bit flip in a tag":            flip(ciphertext, sealed-1),
		"bit flip in last segment":     flip(ciphertext, len(ciphertext)-1),
		"segments reordered":           swap(ciphertext, 0, 1),
		"last segment dropped":         ciphertext[:3*sealed],
		"whole segments dropped":       ciphertext[:2*sealed],
		"cut inside a segment":         ciphertext[:sealed+5],
		"cut inside the last tag":      ciphertext[:len(ciphertext)-1],
		"first segment dropped":        ciphertext[sealed:],
		"segment duplicated":           append(bytes.Clone(ciphertext[:sealed]), ciphertext...),
		"bytes appended":               append(bytes.Clone(ciphertext), 0),
		"segment appended":             append(bytes.Clone(ciphertext), ciphertext[:sealed]...),
		"empty":                        nil,
		"another object's ciphertext":  encrypt(t, newStream(t, testKey(1), []byte{1, 2, 3, 4}, []byte("other-id"), testSegment), plaintext),
		"another key":                  encrypt(t, newStream(t, testKey(2), []byte{1, 2, 3, 4}, []byte("data-id"), testSegment), plaintext),
		"another nonce prefix":         encrypt(t, newStream(t, testKey(1), []byte{9, 9, 9, 9}, []byte("data-id"), testSegment), plaintext),
		"shorter object, same segment": encrypt(t, s, plaintext[:testSegment]),
	}

	for name, tampered := range tests {
		got, err := decryptAll(s, tampered, size)
		if !errors.Is(err, domain.ErrIntegrity) {
			t.Errorf("%s: error = %v, want %v", name, err, domain.ErrIntegrity)
		}
		if len(got) > 0 && !bytes.HasPrefix(plaintext, got) {
			t.Errorf("%s: returned bytes that are not a verified prefix of the plaintext", name)
		}
		if len(got) == size {
			t.Errorf("%s: returned the whole plaintext despite the error", name)
		}
	}
}

func TestStreamTruncationToSegmentBoundaryIsDetected(t *testing.T) {
	t.Parallel()

	// An attacker who drops trailing segments and also lowers the recorded
	// size still fails: the new last segment was not sealed as final.
	s := defaultStream(t)
	ciphertext := encrypt(t, s, randomBytes(t, 3*testSegment))
	truncated := ciphertext[:2*(testSegment+object.TagSize)]

	if _, err := decryptAll(s, truncated, 2*testSegment); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("error = %v, want %v", err, domain.ErrIntegrity)
	}
}

func TestStreamDecryptReportsSourceErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	s := defaultStream(t)
	section := s.Locate(object.Range{Length: 10}, 10)

	_, err := io.ReadAll(s.Decrypt(iotest.ErrReader(boom), section, 10))
	if !errors.Is(err, boom) || errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("error = %v, want %v and not an integrity error", err, boom)
	}
}

func TestStreamSegmentMath(t *testing.T) {
	t.Parallel()

	s := defaultStream(t)
	tests := []struct {
		size       int64
		segments   int64
		ciphertext int64
	}{
		{0, 1, 16},
		{1, 1, 17},
		{64, 1, 80},
		{65, 2, 97},
		{128, 2, 160},
		{129, 3, 177},
	}
	for _, tt := range tests {
		if got := s.SegmentCount(tt.size); got != tt.segments {
			t.Errorf("SegmentCount(%d) = %d, want %d", tt.size, got, tt.segments)
		}
		if got := s.CiphertextSize(tt.size); got != tt.ciphertext {
			t.Errorf("CiphertextSize(%d) = %d, want %d", tt.size, got, tt.ciphertext)
		}
	}
	if s.SegmentSize() != testSegment {
		t.Errorf("SegmentSize() = %d, want %d", s.SegmentSize(), testSegment)
	}

	got := s.Locate(object.Range{Offset: 70, Length: 60}, 200)
	want := object.Section{FirstSegment: 1, Segments: 2, Offset: 80, Length: 160, Skip: 6}
	if got != want {
		t.Errorf("Locate = %+v, want %+v", got, want)
	}
	got = s.Locate(object.Range{}, 0)
	want = object.Section{FirstSegment: 0, Segments: 1, Offset: 0, Length: 16, Skip: 0}
	if got != want {
		t.Errorf("Locate(empty object) = %+v, want %+v", got, want)
	}
}

func flip(b []byte, i int) []byte {
	out := bytes.Clone(b)
	out[i] ^= 0x01
	return out
}

// FuzzStreamRoundTrip: whatever the plaintext and segment size, decryption
// returns the plaintext, and a single flipped bit anywhere is detected.
func FuzzStreamRoundTrip(f *testing.F) {
	f.Add([]byte(nil), uint8(1), uint16(0))
	f.Add([]byte("hello"), uint8(4), uint16(3))
	f.Add(bytes.Repeat([]byte("x"), 300), uint8(64), uint16(299))

	f.Fuzz(func(t *testing.T, plaintext []byte, segment uint8, flipAt uint16) {
		if segment == 0 {
			segment = 1
		}
		s := newStream(t, testKey(7), []byte{0, 0, 0, 1}, []byte("fuzz"), int(segment))
		ciphertext := encrypt(t, s, plaintext)

		got, err := decryptAll(s, ciphertext, int64(len(plaintext)))
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip failed: err=%v", err)
		}

		tampered := flip(ciphertext, int(flipAt)%len(ciphertext))
		if _, err := decryptAll(s, tampered, int64(len(plaintext))); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("bit flip at %d not detected: err=%v", int(flipAt)%len(ciphertext), err)
		}
	})
}

// FuzzStreamDecrypt: arbitrary bytes never decrypt successfully and never
// make the reader panic.
func FuzzStreamDecrypt(f *testing.F) {
	f.Add([]byte(nil), int64(0))
	f.Add(bytes.Repeat([]byte{0}, 16), int64(0))
	f.Add(bytes.Repeat([]byte{1}, 200), int64(150))

	f.Fuzz(func(t *testing.T, ciphertext []byte, size int64) {
		if size < 0 || size > 1<<20 {
			return
		}
		s := defaultStream(t)
		if _, err := decryptAll(s, ciphertext, size); err == nil {
			t.Fatalf("garbage of %d bytes decrypted as an object of size %d", len(ciphertext), size)
		}
	})
}
