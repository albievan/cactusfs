package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// BucketAccess is what a bucket says of who may do what in it, beside the
// caller's own policies: the policy of the bucket and the sub-buckets it has,
// each with its policy (ADR-0017), as one reading of the state has them.
type BucketAccess struct {
	// Policy is the bucket's policy document; empty when it has none.
	Policy []byte
	// SubBuckets are the bucket's sub-buckets in name order.
	SubBuckets []bucket.SubBucket
}

// BucketAccessReader reads the policy of a bucket and its sub-buckets in
// one strongly consistent read: what the authorizer decides a request about
// a bucket from, with one question to the leader and not one for each. A
// reader of a node's own copy of the state must never be one - a Deny just
// put must hold for the next request on every node - and that is why it is
// a port of its own, which SubBucketLister is not.
type BucketAccessReader interface {
	// GetBucketAccess returns the policy and the sub-buckets of a bucket,
	// read together. The error wraps domain.ErrBucketNotFound.
	GetBucketAccess(ctx context.Context, bucketName string) (BucketAccess, error)
}

// SubBucketLister lists the sub-buckets of a bucket, with their policies.
// Every SubBucketStore is one, and reads consistently; a reader of a node's
// own copy of the state is one too, for a search, which reads the policies
// of buckets from that copy as well. The authorizer does not take one: it
// reads through a BucketAccessReader.
type SubBucketLister interface {
	// ListSubBuckets returns the sub-buckets of a bucket in name order.
	// The error wraps domain.ErrBucketNotFound.
	ListSubBuckets(ctx context.Context, bucketName string) ([]bucket.SubBucket, error)
}

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
