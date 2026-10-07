package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// BucketStore is the replicated store of bucket configuration. Every change
// is committed through Raft before the call returns; reads are strongly
// consistent.
type BucketStore interface {
	// CreateBucket adds a bucket. The error wraps
	// domain.ErrBucketAlreadyExists when the name is taken.
	CreateBucket(ctx context.Context, b bucket.Bucket) error
	// DeleteBucket removes an empty bucket. The error wraps
	// domain.ErrBucketNotFound or domain.ErrBucketNotEmpty.
	DeleteBucket(ctx context.Context, name string) error
	// GetBucket returns a bucket. The error wraps domain.ErrBucketNotFound.
	GetBucket(ctx context.Context, name string) (bucket.Bucket, error)
	// ListBuckets returns every bucket in name order.
	ListBuckets(ctx context.Context) ([]bucket.Bucket, error)
}

// ObjectIndex is the replicated index from bucket and key to object version.
// It sits behind this port so that it can move to sharded Raft groups later.
// Every change is committed through Raft before the call returns: the commit
// is the only point at which a written object becomes visible. Reads are
// strongly consistent.
type ObjectIndex interface {
	// PutVersion makes v the current version of its object and returns the
	// version it replaced, if any. The error wraps domain.ErrBucketNotFound
	// when the bucket does not exist at commit time.
	PutVersion(ctx context.Context, v object.Version) (replaced *object.Version, err error)
	// DeleteObject removes an object and returns the version that was
	// current, or nil when the object did not exist. The error wraps
	// domain.ErrBucketNotFound.
	DeleteObject(ctx context.Context, bucketName, key string) (removed *object.Version, err error)
	// GetObject returns the current version of an object. The error wraps
	// domain.ErrBucketNotFound or domain.ErrObjectNotFound.
	GetObject(ctx context.Context, bucketName, key string) (object.Version, error)
	// ScanObjects visits the current versions in a bucket in key order,
	// beginning with the first key greater than or equal to start. visit
	// steers the scan through its return value. The error wraps
	// domain.ErrBucketNotFound.
	ScanObjects(ctx context.Context, bucketName, start string, visit func(object.Version) ScanControl) error
}

// ScanControl tells ObjectIndex.ScanObjects what to do after a visit. The
// zero value continues with the next key.
type ScanControl struct {
	// Stop ends the scan.
	Stop bool
	// SeekTo, when not empty, continues with the first key greater than or
	// equal to it instead of the next key. It must be greater than the key
	// just visited.
	SeekTo string
}

// RootCredentialStore is the replicated store of the deployment's root
// credential.
type RootCredentialStore interface {
	// GetRootCredential returns the root credential. The error wraps
	// domain.ErrNotBootstrapped when none has been set.
	GetRootCredential(ctx context.Context) (iam.RootCredential, error)
	// SetRootCredential stores the root credential of a new deployment. The
	// error wraps domain.ErrAlreadyBootstrapped when one exists.
	SetRootCredential(ctx context.Context, c iam.RootCredential) error
}
