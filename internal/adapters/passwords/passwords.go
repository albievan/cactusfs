// Package passwords hashes console passwords with Argon2id
// (golang.org/x/crypto/argon2). It implements ports.PasswordHasher.
//
// A hash is stored in the PHC string format, which carries its parameters
// and salt:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// so that parameters can be raised later without invalidating the hashes
// that exist.
package passwords

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

// Params are the Argon2id cost parameters of new hashes.
type Params struct {
	// Time is the number of passes.
	Time uint32
	// MemoryKiB is the memory used, in KiB.
	MemoryKiB uint32
	// Threads is the degree of parallelism.
	Threads uint8
}

// DefaultParams returns the second recommended parameter set of RFC 9106:
// 3 passes over 64 MiB with 4 lanes.
func DefaultParams() Params {
	return Params{Time: 3, MemoryKiB: 64 << 10, Threads: 4}
}

const (
	saltSize = 16
	hashSize = 32
	// Bounds on the parameters accepted from a stored hash: below, it was
	// weakened; above, verifying it would exhaust the machine.
	minMemoryKiB = 8 << 10
	maxMemoryKiB = 256 << 10
	maxTime      = 16
	maxThreads   = 64
	version      = 19
)

func (p Params) valid() bool {
	return p.Time >= 1 && p.Time <= maxTime && p.MemoryKiB >= minMemoryKiB && p.MemoryKiB <= maxMemoryKiB &&
		p.Threads >= 1 && p.Threads <= maxThreads
}

// Hasher hashes and verifies passwords.
type Hasher struct {
	params  Params
	entropy io.Reader
	// slots bounds how many hashes are computed at once: each takes tens
	// of megabytes, and sign-in can be tried by anybody.
	slots chan struct{}
	// waiting counts the hashes that wait for a slot.
	waiting atomic.Int32
	// concealSlots is the pool of Conceal, apart from slots.
	concealSlots chan struct{}
}

// maxConcurrent is the number of hashes computed at the same time.
const maxConcurrent = 4

// maxWaiting is how many hashes may wait for a slot. Beyond that the caller
// is told to come back: a queue without end is memory without end.
const maxWaiting = 32

func (h *Hasher) acquire() (func(), error) {
	if h.waiting.Add(1) > maxWaiting {
		h.waiting.Add(-1)
		return nil, fmt.Errorf("%w: too many password checks at once", domain.ErrUnavailable)
	}
	h.slots <- struct{}{}
	h.waiting.Add(-1)
	return func() { <-h.slots }, nil
}

var _ ports.PasswordHasher = (*Hasher)(nil)

// New returns a Hasher that makes hashes with params. entropy must be a
// cryptographically secure random source; it supplies the salts.
func New(params Params, entropy io.Reader) (*Hasher, error) {
	if !params.valid() {
		return nil, fmt.Errorf("passwords.New: %w: Argon2id parameters out of range", domain.ErrInvalidArgument)
	}
	return &Hasher{
		params: params, entropy: entropy, slots: make(chan struct{}, maxConcurrent),
		concealSlots: make(chan struct{}, maxConcurrentConceal),
	}, nil
}

// Hash implements ports.PasswordHasher.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(h.entropy, salt); err != nil {
		return "", fmt.Errorf("passwords.Hash: %w", err)
	}
	release, err := h.acquire()
	if err != nil {
		return "", fmt.Errorf("passwords.Hash: %w", err)
	}
	defer release()
	sum := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Threads, hashSize)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", version, h.params.MemoryKiB, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}

// Verify implements ports.PasswordHasher.
func (h *Hasher) Verify(encoded, password string) (bool, error) {
	params, salt, want, err := parse(encoded)
	if err != nil {
		return false, fmt.Errorf("passwords.Verify: %w", err)
	}
	release, err := h.acquire()
	if err != nil {
		return false, fmt.Errorf("passwords.Verify: %w", err)
	}
	defer release()
	got := argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Threads, hashSize)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// The cost of Conceal. It is fixed, and not the cost of password hashes,
// which an operator may raise: every node of a deployment must conceal a
// value alike, or a request repeated through another node is taken for
// another request. Argon2id with 19 MiB, two passes and one lane is the
// least that OWASP's Password Storage Cheat Sheet recommends; what is
// concealed is kept for a day (app.IdempotencyTTL), and is the body of a
// request, of which a password is one part.
const (
	concealTime      = 2
	concealMemoryKiB = 19 << 10
	concealThreads   = 1
	// maxConcurrentConceal bounds how many values are concealed at once,
	// in a pool of their own: concealing is asked for before anybody is
	// authorized to anything, by whoever repeats a request, and must
	// never take a slot that a sign-in needs.
	maxConcurrentConceal = 2
	// concealWait is the longest a call waits for a slot of that pool.
	// There is no queue beyond it: a node that is busy concealing says so
	// at once, and the caller tries again in a moment.
	concealWait = 250 * time.Millisecond
)

// Conceal implements ports.PasswordHasher: Argon2id of value under salt,
// at the fixed cost above, in a pool of its own. When no slot of the pool
// is free within concealWait the error wraps domain.ErrTooManyRequests;
// when ctx ends first, it wraps the error of ctx.
func (h *Hasher) Conceal(ctx context.Context, value, salt []byte) ([]byte, error) {
	timer := time.NewTimer(concealWait)
	defer timer.Stop()
	select {
	case h.concealSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("passwords.Conceal: %w", ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("passwords.Conceal: %w: every slot for concealing is taken", domain.ErrTooManyRequests)
	}
	defer func() { <-h.concealSlots }()
	return argon2.IDKey(value, salt, concealTime, concealMemoryKiB, concealThreads, hashSize), nil
}

// parse reads a PHC string. The error wraps domain.ErrIntegrity: a stored
// hash that cannot be read is damage, not a wrong password.
func parse(encoded string) (Params, []byte, []byte, error) {
	bad := fmt.Errorf("%w: stored password hash is not an Argon2id hash this version reads", domain.ErrIntegrity)
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(version) {
		return Params{}, nil, nil, bad
	}
	var params Params
	for _, field := range strings.Split(parts[3], ",") {
		name, value, ok := strings.Cut(field, "=")
		number, err := strconv.ParseUint(value, 10, 32)
		if !ok || err != nil {
			return Params{}, nil, nil, bad
		}
		switch name {
		case "m":
			params.MemoryKiB = uint32(number)
		case "t":
			params.Time = uint32(number)
		case "p":
			if number > maxThreads {
				return Params{}, nil, nil, bad
			}
			params.Threads = uint8(number)
		default:
			return Params{}, nil, nil, bad
		}
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return Params{}, nil, nil, bad
	}
	sum, err := base64.RawStdEncoding.DecodeString(parts[5])
	// Only hashes of the length this adapter makes are read.
	if err != nil || len(sum) != hashSize || !params.valid() {
		return Params{}, nil, nil, bad
	}
	return params, salt, sum, nil
}
