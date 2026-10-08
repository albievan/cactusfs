package porttest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Clock is a ports.Clock that tests set by hand.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock that reports now.
func NewClock(now time.Time) *Clock { return &Clock{now: now} }

// Now implements ports.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// KMS is an in-memory ports.KMS with a throw-away master key. It really
// encrypts, so a test that leaks a plaintext key into storage is caught.
type KMS struct {
	id   string
	aead cipher.AEAD

	// FailGenerate and FailUnwrap, when set, are returned by the
	// corresponding operation.
	FailGenerate error
	FailUnwrap   error
}

// NewKMS returns a KMS with a random master key and the given key ID.
func NewKMS(keyID string) (*KMS, error) {
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		return nil, fmt.Errorf("porttest.NewKMS: %w", err)
	}
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, fmt.Errorf("porttest.NewKMS: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("porttest.NewKMS: %w", err)
	}
	return &KMS{id: keyID, aead: aead}, nil
}

// KeyID implements ports.KMS.
func (k *KMS) KeyID() string { return k.id }

// GenerateDataKey implements ports.KMS.
func (k *KMS) GenerateDataKey(_ context.Context, aad []byte) (ports.DataKey, error) {
	if k.FailGenerate != nil {
		return ports.DataKey{}, k.FailGenerate
	}
	plaintext := make([]byte, 32)
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(plaintext); err != nil {
		return ports.DataKey{}, fmt.Errorf("porttest.KMS: %w", err)
	}
	if _, err := rand.Read(nonce); err != nil {
		return ports.DataKey{}, fmt.Errorf("porttest.KMS: %w", err)
	}
	return ports.DataKey{Plaintext: plaintext, Wrapped: k.aead.Seal(nonce, nonce, plaintext, aad)}, nil
}

// UnwrapDataKey implements ports.KMS.
func (k *KMS) UnwrapDataKey(_ context.Context, wrapped, aad []byte) ([]byte, error) {
	if k.FailUnwrap != nil {
		return nil, k.FailUnwrap
	}
	if len(wrapped) < k.aead.NonceSize() {
		return nil, fmt.Errorf("%w: wrapped key is too short", domain.ErrKeyUnavailable)
	}
	nonce, ciphertext := wrapped[:k.aead.NonceSize()], wrapped[k.aead.NonceSize():]
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: wrapped key failed authentication", domain.ErrKeyUnavailable)
	}
	return plaintext, nil
}

// Authorizer is a ports.Authorizer that applies the domain's decision and
// records every request it was asked about.
type Authorizer struct {
	mu       sync.Mutex
	requests []iam.Request
}

// Authorize implements ports.Authorizer.
func (a *Authorizer) Authorize(_ context.Context, req iam.Request) error {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	return iam.Decide(req)
}

// Requests returns every request seen so far.
func (a *Authorizer) Requests() []iam.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]iam.Request(nil), a.requests...)
}

// Root is the root principal used in tests.
func Root() iam.Principal {
	return iam.Principal{AccessKeyID: "ROOTACCESSKEY", Root: true}
}

// Entropy returns a deterministic, endless byte source for tests that need
// reproducible identifiers. It is not random.
func Entropy() io.Reader {
	return &entropy{state: 0x9E3779B97F4A7C15}
}

type entropy struct {
	state uint64
}

// Read fills p from a xorshift sequence; it never fails and never ends.
func (e *entropy) Read(p []byte) (int, error) {
	var word [8]byte
	for i := range p {
		if i%8 == 0 {
			e.state ^= e.state << 13
			e.state ^= e.state >> 7
			e.state ^= e.state << 17
			binary.BigEndian.PutUint64(word[:], e.state)
		}
		p[i] = word[i%8]
	}
	return len(p), nil
}

// Hasher is a fast ports.PasswordHasher for tests. It is not a password
// hash: it only lets tests see what was hashed and verified.
type Hasher struct {
	mu sync.Mutex
	// Verified counts the calls to Verify.
	Verified int
}

// Hash implements ports.PasswordHasher.
func (h *Hasher) Hash(password string) (string, error) {
	return "test-hash:" + password, nil
}

// Verify implements ports.PasswordHasher.
func (h *Hasher) Verify(encoded, password string) (bool, error) {
	h.mu.Lock()
	h.Verified++
	h.mu.Unlock()
	return encoded == "test-hash:"+password, nil
}

// Verifications returns how often Verify was called.
func (h *Hasher) Verifications() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Verified
}
