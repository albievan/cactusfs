package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// SubBucketStore is the replicated store of the sub-buckets of buckets
// (ADR-0017). Every change is committed through Raft before the call
// returns and carries its audit record in the same entry, as changes to
// identities do; a record without an actor is not appended. Reads are
// strongly consistent. Until every member of the cluster knows sub-buckets
// a change is refused with domain.ErrUnavailable, and there are none to
// read.
type SubBucketStore interface {
	// PutSubBucket registers a sub-bucket of a bucket, or replaces the
	// policy of one that is registered; that one keeps the time it was
	// created. The error wraps domain.ErrBucketNotFound,
	// domain.ErrInvalidArgument for a name or a policy that cannot be
	// kept, and domain.ErrTooManySubBuckets when the bucket has
	// bucket.MaxSubBuckets others.
	PutSubBucket(ctx context.Context, bucketName string, s bucket.SubBucket, record audit.Record) error
	// DeleteSubBucket removes a sub-bucket that holds nothing: no object,
	// no version of one, no delete marker and no upload in progress under
	// its name. That is decided when the change is committed. The error
	// wraps domain.ErrBucketNotFound, domain.ErrSubBucketNotFound or
	// domain.ErrSubBucketNotEmpty.
	DeleteSubBucket(ctx context.Context, bucketName, name string, record audit.Record) error
	// GetSubBucket returns a sub-bucket. The error wraps
	// domain.ErrBucketNotFound or domain.ErrSubBucketNotFound.
	GetSubBucket(ctx context.Context, bucketName, name string) (bucket.SubBucket, error)
	// ListSubBuckets returns the sub-buckets of a bucket in name order.
	// The error wraps domain.ErrBucketNotFound.
	ListSubBuckets(ctx context.Context, bucketName string) ([]bucket.SubBucket, error)
}
