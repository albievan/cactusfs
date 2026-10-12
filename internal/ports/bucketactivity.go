package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// MaxBucketActivities is how many buckets one call of BucketActivities may
// name. The list of buckets is not paged, and one question is asked for all
// of it: the bound is far above what a deployment has, and there so that a
// call cannot hold a view of the store for as long as it likes. A record is
// one point read of at most six rows.
const MaxBucketActivities = 10000

// BucketActivityStore is the replicated record of the latest uploads into
// and downloads from every bucket (ADR-0019). The store keeps it by itself,
// from the versions that are committed and the reads that are handed in
// (ObjectIndex, UploadStore, ObjectAccessStore): nothing writes it through
// this port. Like the record of who read a version it is a count, not
// evidence.
type BucketActivityStore interface {
	// BucketActivities returns what is known of the latest use of the
	// buckets named, answered from one view of the store: the record of
	// names[i] is at i of what it returns. A bucket of which nothing is
	// recorded - none was uploaded to or downloaded from since the cluster
	// began to keep the record, or there is no such bucket - has the zero
	// record: a list of buckets is not refused for a bucket that was
	// deleted behind it. More than MaxBucketActivities names wrap
	// domain.ErrInvalidArgument.
	BucketActivities(ctx context.Context, names []string) ([]bucket.Activity, error)
}
