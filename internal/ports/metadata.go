package ports

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
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
	// SetBucketVersioning changes whether a bucket keeps versions. The
	// error wraps domain.ErrBucketNotFound, or what
	// bucket.ValidateVersioningChange refuses.
	SetBucketVersioning(ctx context.Context, name string, state bucket.Versioning) error
}

// ObjectIndex is the replicated index from bucket and key to object version.
// It sits behind this port so that it can move to sharded Raft groups later.
// Every change is committed through Raft before the call returns: the commit
// is the only point at which a written object becomes visible. Reads are
// strongly consistent.
type ObjectIndex interface {
	// PutVersion makes v the current version of its object. What becomes
	// of the version that was current is decided by the versioning state
	// of the bucket when the change is committed: it is removed, or kept
	// as a version that is no longer current. The error wraps
	// domain.ErrBucketNotFound when the bucket does not exist then.
	PutVersion(ctx context.Context, v object.Version) (PutResult, error)
	// DeleteObject deletes an object, or one of its versions. Without a
	// version named, what happens is decided by the versioning state of
	// the bucket when the change is committed: the object is removed, or a
	// delete marker becomes its newest version. With a version named, that
	// version is removed for good. Deleting what does not exist is not an
	// error. The error wraps domain.ErrBucketNotFound.
	DeleteObject(ctx context.Context, bucketName, key string, o DeleteOptions) (DeleteResult, error)
	// GetVersion returns one version of an object, which may be a delete
	// marker. The error wraps domain.ErrBucketNotFound or
	// domain.ErrVersionNotFound.
	GetVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (object.Version, error)
	// ScanVersions visits every version of every object in a bucket,
	// delete markers included: keys in order from the first one greater
	// than or equal to start, and the versions of a key from the newest to
	// the oldest. latest is set for the newest version of its key. The
	// error wraps domain.ErrBucketNotFound.
	ScanVersions(ctx context.Context, bucketName, start string, visit func(v object.Version, latest bool) ScanControl) error
	// GetObject returns the current version of an object. The error wraps
	// domain.ErrBucketNotFound or domain.ErrObjectNotFound.
	GetObject(ctx context.Context, bucketName, key string) (object.Version, error)
	// IsDataReferenced reports whether any object version refers to the
	// stored data with this ID. The garbage collector asks before it
	// removes a shard.
	IsDataReferenced(ctx context.Context, dataID object.ID) (bool, error)
	// ScanObjects visits the current versions in a bucket in key order,
	// beginning with the first key greater than or equal to start. visit
	// steers the scan through its return value. The error wraps
	// domain.ErrBucketNotFound.
	ScanObjects(ctx context.Context, bucketName, start string, visit func(object.Version) ScanControl) error
}

// PutResult is what committing a version did.
type PutResult struct {
	// Stored is the version as it was committed, with its sequence and
	// whether it has an ID of its own.
	Stored object.Version
	// Removed is the version the put removed for good, if any. Its stored
	// data is no longer referred to.
	Removed *object.Version
}

// DeleteOptions say what to delete, and carry what a delete marker needs.
type DeleteOptions struct {
	// Version, when not nil, names the one version to remove for good.
	Version *object.VersionRef
	// MarkerID and At are the ID and the time of the delete marker, should
	// the delete make one. The caller generates them: nothing is generated
	// while a change is applied.
	MarkerID object.ID
	At       time.Time
}

// DeleteResult is what committing a delete did.
type DeleteResult struct {
	// Marker is the delete marker that was made, if one was.
	Marker *object.Version
	// Removed is the version that was removed for good, if one was. Its
	// stored data is no longer referred to.
	Removed *object.Version
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

// TopologyStore is the replicated record of which drive is in which slot of
// which erasure set.
type TopologyStore interface {
	// GetTopology returns the topology. The error wraps
	// domain.ErrNoTopology when none has been set.
	GetTopology(ctx context.Context) (cluster.Topology, error)
	// SetTopology replaces the topology, provided the stored one still has
	// the version the change was based on: t.Version, which is zero for the
	// first topology. The stored version is then one higher. The error
	// wraps domain.ErrConflict when the stored topology has changed.
	SetTopology(ctx context.Context, t cluster.Topology) error
}
