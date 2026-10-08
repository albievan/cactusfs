package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	// IdempotencyTTL is how long the answer to a request with an
	// idempotency key is kept.
	IdempotencyTTL = 24 * time.Hour
	// idempotencyPending is how long a request may be in progress before
	// its key is free again: its node is taken to have stopped. It is
	// longer than any request but a very long upload takes, and far longer
	// than the clocks of two gateways differ.
	idempotencyPending = 15 * time.Minute
	// MaxIdempotentAnswer is the largest answer that is kept. A larger
	// one is not kept, and the request is carried out again when repeated.
	MaxIdempotentAnswer = 64 << 10
)

// IdempotentRequest names a request that carried an idempotency key.
type IdempotentRequest struct {
	// Scope is who the key belongs to: keys of different callers never
	// meet.
	Scope string
	// Key is the idempotency key as sent.
	Key string
	// Fingerprint describes the request, so that the same key with
	// another request is recognised.
	Fingerprint string
}

// IdempotentAnswer is the answer a request got.
type IdempotentAnswer struct {
	Status      int
	ContentType string
	Body        []byte
}

// Reservation is held by the one request that carries a key out.
type Reservation struct {
	id          string
	fingerprint string
	createdAt   time.Time
}

// IdempotencyService makes requests safe to repeat. The first request with a
// key reserves it, is carried out, and leaves its answer; a repeat gets that
// answer and changes nothing.
//
// Reserving and answering are two replicated writes around the change, not
// one with it. A node that stops in between leaves a reservation without an
// answer: repeats are refused while it is fresh, and carried out again once
// it is stale. A reservation is only ever removed by the request that made
// it, or as the stale one that was read: a repeat does not run beside the
// original unless the original takes longer than idempotencyPending. After a
// crash a repeat can run again - which a named resource answers with a
// conflict.
type IdempotencyService struct {
	store ports.IdempotencyStore
	kms   ports.KMS
	clock ports.Clock

	entropyMu sync.Mutex
	entropy   io.Reader
}

// NewIdempotencyService returns an IdempotencyService. entropy must be a
// cryptographically secure random source.
func NewIdempotencyService(store ports.IdempotencyStore, kms ports.KMS, clock ports.Clock, entropy io.Reader) *IdempotencyService {
	return &IdempotencyService{store: store, kms: kms, clock: clock, entropy: entropy}
}

func hashHex(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		// Length-prefixed, so that no two lists of parts hash alike.
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Begin reserves the key of a request. It returns the answer when the
// request was carried out before; otherwise a reservation, and the caller
// carries the request out and calls Complete.
//
// The error wraps domain.ErrIdempotencyKeyReused when the key belongs to
// another request, and domain.ErrRequestInProgress when the first request
// with the key has not finished.
func (s *IdempotencyService) Begin(ctx context.Context, req IdempotentRequest) (*IdempotentAnswer, Reservation, error) {
	if req.Scope == "" || req.Key == "" {
		return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w: no scope or key", domain.ErrInvalidArgument)
	}
	reservation := Reservation{id: hashHex(req.Scope, req.Key), fingerprint: hashHex(req.Fingerprint)}
	// A stale record is removed and the key reserved again: twice at
	// most, then somebody else is quicker and the caller is told to wait.
	for range 2 {
		now := s.clock.Now().UTC()
		reservation.createdAt = now
		err := s.store.BeginIdempotent(ctx, ports.IdempotencyRecord{
			ID: reservation.id, Fingerprint: reservation.fingerprint, CreatedAt: now,
		})
		if err == nil {
			return nil, reservation, nil
		}
		if !errors.Is(err, domain.ErrEntityExists) {
			return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", err)
		}
		existing, err := s.store.GetIdempotent(ctx, reservation.id)
		if errors.Is(err, domain.ErrEntityNotFound) {
			continue
		}
		if err != nil {
			return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", err)
		}
		age := now.Sub(existing.CreatedAt)
		stale := age > IdempotencyTTL || (!existing.Done && age > idempotencyPending)
		switch {
		case stale:
			if err := s.store.DeleteIdempotent(ctx, reservation.id, existing.CreatedAt); err != nil {
				return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", err)
			}
			continue
		case existing.Fingerprint != reservation.fingerprint:
			return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", domain.ErrIdempotencyKeyReused)
		case !existing.Done:
			return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", domain.ErrRequestInProgress)
		}
		body, err := s.open(ctx, reservation.id, existing.Body)
		if err != nil {
			return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", err)
		}
		return &IdempotentAnswer{Status: existing.Status, ContentType: existing.ContentType, Body: body}, Reservation{}, nil
	}
	return nil, Reservation{}, fmt.Errorf("idempotency.Begin: %w", domain.ErrRequestInProgress)
}

// Complete leaves the answer of a request for its repeats. An answer that
// says the server failed, or one too large to keep, is not kept: the key is
// freed and a repeat is carried out again.
func (s *IdempotencyService) Complete(ctx context.Context, r Reservation, answer IdempotentAnswer) error {
	if r.id == "" {
		return nil
	}
	if answer.Status >= 500 || len(answer.Body) > MaxIdempotentAnswer {
		return s.Abandon(ctx, r)
	}
	sealed, err := s.seal(ctx, r.id, answer.Body)
	if err != nil {
		return fmt.Errorf("idempotency.Complete: %w", err)
	}
	err = s.store.CompleteIdempotent(ctx, ports.IdempotencyRecord{
		ID: r.id, Fingerprint: r.fingerprint, CreatedAt: r.createdAt, Done: true,
		Status: answer.Status, ContentType: answer.ContentType, Body: sealed,
	})
	if err != nil {
		return fmt.Errorf("idempotency.Complete: %w", err)
	}
	return nil
}

// Abandon frees the key of a request that left no answer.
func (s *IdempotencyService) Abandon(ctx context.Context, r Reservation) error {
	if r.id == "" {
		return nil
	}
	if err := s.store.DeleteIdempotent(ctx, r.id, r.createdAt); err != nil {
		return fmt.Errorf("idempotency.Abandon: %w", err)
	}
	return nil
}

// Sweep forgets the answers that are older than IdempotencyTTL and returns
// how many there were.
func (s *IdempotencyService) Sweep(ctx context.Context) (int, error) {
	n, err := s.store.DeleteIdempotentBefore(ctx, s.clock.Now().Add(-IdempotencyTTL))
	if err != nil {
		return 0, fmt.Errorf("idempotency.Sweep: %w", err)
	}
	return n, nil
}

// seal encrypts an answer under a data key of its own, bound to the record
// it belongs to. Answers can hold a secret that is shown once.
func (s *IdempotencyService) seal(ctx context.Context, id string, body []byte) (iam.SealedSecret, error) {
	aad := []byte("idempotency:" + id)
	key, err := s.kms.GenerateDataKey(ctx, aad)
	if err != nil {
		return iam.SealedSecret{}, err
	}
	defer clear(key.Plaintext)
	s.entropyMu.Lock()
	sealed, err := iam.Seal(key.Plaintext, s.entropy, body, aad)
	s.entropyMu.Unlock()
	if err != nil {
		return iam.SealedSecret{}, err
	}
	sealed.KMSKeyID, sealed.WrappedKey = s.kms.KeyID(), key.Wrapped
	return sealed, nil
}

func (s *IdempotencyService) open(ctx context.Context, id string, sealed iam.SealedSecret) ([]byte, error) {
	if sealed.KMSKeyID != s.kms.KeyID() {
		return nil, fmt.Errorf("%w: sealed under KMS key %s, this node uses %s", domain.ErrKeyUnavailable, sealed.KMSKeyID, s.kms.KeyID())
	}
	aad := []byte("idempotency:" + id)
	key, err := s.kms.UnwrapDataKey(ctx, sealed.WrappedKey, aad)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return sealed.Open(key, aad)
}
