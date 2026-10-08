package ports

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// IdempotencyRecord is what is kept about a request that carried an
// idempotency key: enough to recognise the request when it is repeated, and
// the answer it got.
type IdempotencyRecord struct {
	// ID identifies the key within its scope: a hash, never the key.
	ID string
	// Fingerprint is a hash of the request the key was first used for.
	Fingerprint string
	// Done is false while the request is being carried out.
	Done bool
	// Status, ContentType and Body are the answer. The body is sealed
	// through the KMS: an answer can hold a secret that is shown once.
	Status      int
	ContentType string
	Body        iam.SealedSecret
	CreatedAt   time.Time
}

// IdempotencyStore is the replicated store of idempotency records.
type IdempotencyStore interface {
	// BeginIdempotent records that a request has begun. It fails with
	// domain.ErrEntityExists when the ID is taken, and changes nothing.
	BeginIdempotent(ctx context.Context, r IdempotencyRecord) error
	// GetIdempotent fails with domain.ErrEntityNotFound for an unknown ID.
	GetIdempotent(ctx context.Context, id string) (IdempotencyRecord, error)
	// CompleteIdempotent stores the answer of a request that had begun. A
	// record that is gone, or done already, is left as it is.
	CompleteIdempotent(ctx context.Context, r IdempotencyRecord) error
	// DeleteIdempotent forgets the record with this ID that was begun at
	// createdAt. A record begun at another time belongs to another
	// request and is left alone; so is one that does not exist.
	DeleteIdempotent(ctx context.Context, id string, createdAt time.Time) error
	// DeleteIdempotentBefore forgets the records made before a time and
	// returns how many there were.
	DeleteIdempotentBefore(ctx context.Context, before time.Time) (int, error)
}
