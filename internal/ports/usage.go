package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// Bounds of the calls of BucketUsageStore.
const (
	// MaxBucketUsages is how many buckets one call of BucketUsages may
	// name, as MaxBucketActivities is for BucketActivities.
	MaxBucketUsages = 10000
	// MaxUsageWalk is the greatest limit of one entry of the walk. An
	// entry is applied by every member in one transaction, in which
	// nothing else changes.
	MaxUsageWalk = 100000
)

// BucketUsage is what a bucket holds and how much it may.
type BucketUsage struct {
	// Usage is the bucket's record; the zero record for a bucket that has
	// none - it is older than the record and has not been counted yet -
	// and for a bucket that does not exist.
	Usage bucket.Usage
	// QuotaBytes is the bucket's quota, or 0 when it has none.
	QuotaBytes int64
}

// UsageWalk is what one entry of the walk through a bucket did.
type UsageWalk struct {
	// Usage is the record after the entry. The walk goes on while
	// Usage.Walking says so.
	Usage bucket.Usage
	// Drift says that the entry ended a recount whose result is not what
	// the record held: a fault of whoever kept the record, which is for a
	// counter. The record now holds what the walk found.
	Drift bool
}

// BucketUsageStore is the replicated count of what every bucket holds, and
// the quota of a bucket (ADR-0023, ADR-0024). The store keeps the count by
// itself, from every change that is committed (ObjectIndex, UploadStore):
// nothing adds to it through this port. It exists from entry schema 5 on;
// until every member of the cluster knows it, there is no record to read
// and both changes wrap domain.ErrUnavailable.
type BucketUsageStore interface {
	// BucketUsages returns the records and the quotas of the buckets
	// named, from one view of the store: that of names[i] is at i of what
	// it returns. A list of buckets is not refused for a bucket that was
	// deleted behind it. More than MaxBucketUsages names wrap
	// domain.ErrInvalidArgument.
	//
	// It is answered from this node's own copy of the state and does not
	// confirm with the leader that the copy is current: it is asked
	// before every write, as advice. What is authoritative is the refusal
	// of the change itself (domain.ErrQuotaExceeded).
	BucketUsages(ctx context.Context, names []string) ([]BucketUsage, error)
	// WalkBucketUsage commits one entry of the walk that counts a bucket
	// (ADR-0023). The entry visits whole keys - every version of a key at
	// once - in key order until it has visited limit records, then the
	// parts of the uploads in progress, upload by upload; where it ends
	// is kept in the record, so that the next entry goes on from there,
	// whichever node proposes it.
	//
	//   - A bucket without a record is given one, in counting state: its
	//     first count.
	//   - With restart, a bucket whose record is exact is counted again
	//     while its totals go on being kept, and the result takes their
	//     place at the end; a walk that was under way begins again.
	//   - Without restart, a walk under way goes on, and a record that is
	//     exact and not being counted is returned as it is: there is
	//     nothing to do.
	//
	// limit is from 1 to MaxUsageWalk: domain.ErrInvalidArgument
	// otherwise. The error wraps domain.ErrBucketNotFound.
	WalkBucketUsage(ctx context.Context, name string, limit int, restart bool) (UsageWalk, error)
	// SetBucketQuota gives a bucket a quota of quotaBytes, from 1 to
	// bucket.MaxQuotaBytes, or with 0 removes the one it has. A quota
	// below what the bucket holds is accepted: nothing is removed, and
	// nothing more is taken until enough was deleted. The change carries
	// its audit record in the same entry, as SubBucketStore's do; a
	// record without an actor is not appended.
	//
	// The error wraps domain.ErrBucketNotFound, domain.ErrInvalidArgument,
	// and domain.ErrConflict while the bucket's record is not exact: a
	// limit cannot be held to a count that is not known.
	SetBucketQuota(ctx context.Context, name string, quotaBytes int64, record audit.Record) error
}
