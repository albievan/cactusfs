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
	// SetBucketDefaultRetention sets the retention the bucket gives its
	// new versions; nil removes it. The error wraps
	// domain.ErrLockNotConfigured when the bucket was not created with
	// Object Lock.
	SetBucketDefaultRetention(ctx context.Context, name string, d *bucket.DefaultRetention) error
	// SetBucketConfig replaces one configuration document of a bucket -
	// its policy, its CORS rules, its tags - or, with nil, removes it.
	// The kinds are those bucket.ValidConfigKind names, and
	// bucket.ConfigTagRules: that one is refused with
	// domain.ErrUnavailable while a member of the cluster does not know
	// it (entry schema 3, ADR-0016).
	SetBucketConfig(ctx context.Context, name, kind string, document []byte) error
	// GetBucketConfig returns one configuration document of a bucket, or
	// nil when the bucket has none of that kind.
	GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error)
}

// BucketPolicies walks the policies of all buckets, a page at a time. It is
// for a request that needs a decision about every bucket there is - a search
// - and must neither ask the metadata leader once for each of them nor hold
// all of their policies at once.
type BucketPolicies interface {
	// BucketPolicies reads one page of buckets in name order, beginning
	// with the first whose name is not before from - the empty string
	// begins with the first bucket - and calls visit, in name order, with
	// the policy document of each bucket of the page that has one. It
	// returns the name of the first bucket it did not read, to be given as
	// from of the next call, or the empty string when it read the last. A
	// bucket whose name lies between from and next and that visit was not
	// called with has no policy. A page is bounded, in buckets and in
	// bytes, by the implementation, and may hold no policy at all; ctx is
	// checked at every bucket read and before every call of visit.
	//
	// A page is read from this node's own copy of the state: it does not
	// confirm with the leader that the copy is current, and whatever lock
	// the read takes is let go before visit is called and between two
	// pages. A node's copy never goes back, so whoever has just made a
	// strongly consistent read - of the list of buckets,
	// BucketStore.ListBuckets - is given, on every page, policies at least
	// as new as that read. The pages of one walk are not one snapshot: a
	// policy that is changed while the walk is under way is seen as it was
	// before the change or as it is after, never in part - what two
	// requests that follow one another are given.
	//
	// document is lent to visit for the duration of that one call: it must
	// not be changed, and whoever keeps any of it after returning must copy
	// it. An error of visit ends the call, which returns an error that
	// wraps it.
	BucketPolicies(ctx context.Context, from string, visit func(name string, document []byte) error) (next string, err error)
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
	//
	// v.CreatedBy, who commits the version, is recorded where every
	// member of the cluster knows the field (entry schema 3, ADR-0016).
	// Until then the version is committed without it and the put still
	// succeeds: PutResult.Stored says what was recorded.
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
	// delete markers included: keys in order from start, and the versions
	// of a key from the newest to the oldest. latest is set for the newest
	// version of its key. The cost of a scan is what it visits: not the
	// number of versions a key has. The error wraps
	// domain.ErrBucketNotFound.
	ScanVersions(ctx context.Context, bucketName string, start VersionCursor, visit func(v object.Version, latest bool) ScanControl) error
	// SetRetention replaces the retention of a version; nil removes it.
	// Whether it may is decided when the change is committed, by the rules
	// of object.Lock and on the store's own time. The error wraps
	// domain.ErrObjectLocked when it may not, domain.ErrLockNotConfigured
	// when the bucket was not created with Object Lock, and
	// domain.ErrVersionNotFound.
	SetRetention(ctx context.Context, bucketName, key string, ref object.VersionRef, r *object.Retention, bypassGovernance bool) (object.Version, error)
	// SetLegalHold puts a version under legal hold or takes it off. The
	// errors are those of SetRetention.
	SetLegalHold(ctx context.Context, bucketName, key string, ref object.VersionRef, on bool) (object.Version, error)
	// GetObject returns the current version of an object. The error wraps
	// domain.ErrBucketNotFound or domain.ErrObjectNotFound.
	GetObject(ctx context.Context, bucketName, key string) (object.Version, error)
	// SetTags replaces the tags of a version; nil removes them.
	SetTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) (object.Version, error)
	// ObjectParts returns up to limit parts of a version that is made of
	// parts, in order, beginning with the part that holds the byte at
	// offset. dataID is the version's. It returns none when the version
	// has no part there - or is gone.
	ObjectParts(ctx context.Context, bucketName string, dataID object.ID, offset int64, limit int) ([]object.Part, error)
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

// VersionCursor says where a scan of versions begins.
type VersionCursor struct {
	// Key is the first key to visit: the first one greater than or equal
	// to it.
	Key string
	// After, when not nil, names a version of Key: the scan begins with
	// the version after it. When Key has no such version any more, the
	// scan begins with the key after Key.
	After *object.VersionRef
}

// PutResult is what committing a version did.
type PutResult struct {
	// Stored is the version as it was committed, with its sequence and
	// whether it has an ID of its own.
	Stored object.Version
	// Removed is the version the put removed for good, if any. Its stored
	// data is no longer referred to.
	Removed *object.Version
	// RemovedParts are parts whose data nothing refers to any more: those
	// of Removed, when it was made of parts, and those a completion did
	// not use. The list may be cut short; the collector finds the rest.
	RemovedParts []object.Part
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
	// BypassGovernance says that the caller may lift a governance
	// retention and asked to. The use case authorizes that; the store
	// lifts nothing else on the strength of it.
	BypassGovernance bool
	// Expect, when not zero, makes the delete conditional: it is carried
	// out only if the version it is about - the one Version names, or
	// else the newest version of the key - has this version ID. If
	// another has taken its place the delete is refused with
	// domain.ErrConflict and changes nothing. Whoever decided on a
	// reading of the state that something is to go says here what it
	// read.
	Expect object.ID
}

// DeleteResult is what committing a delete did.
type DeleteResult struct {
	// Marker is the delete marker that was made, if one was.
	Marker *object.Version
	// Removed is the version that was removed for good, if one was. Its
	// stored data is no longer referred to.
	Removed *object.Version
	// RemovedParts are the parts of Removed, when it was made of parts.
	// The list may be cut short; the collector finds the rest.
	RemovedParts []object.Part
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

// UploadStore keeps multipart uploads in progress. Like the object index it
// is strongly consistent: a part that was acknowledged is seen by the
// completion that follows.
type UploadStore interface {
	// CreateUpload records a new upload. The error wraps
	// domain.ErrBucketNotFound when the bucket does not exist and
	// domain.ErrConflict when the ID is in use. An upload with tags is
	// refused with domain.ErrUnavailable while a member of the cluster
	// does not know them (entry schema 3): they could not be kept, and
	// the bucket's rules may require them.
	CreateUpload(ctx context.Context, u object.Upload) error
	// GetUpload returns an upload in progress. The error wraps
	// domain.ErrUploadNotFound when there is none with that ID for the key.
	GetUpload(ctx context.Context, bucketName, key string, id object.ID) (object.Upload, error)
	// PutPart records a part of an upload, in place of the part that had
	// its number, which it returns: nothing refers to that one any more.
	PutPart(ctx context.Context, bucketName, key string, id object.ID, p object.Part) (replaced *object.Part, err error)
	// ListParts returns up to limit parts of an upload with a number
	// greater than after, in ascending order of number.
	ListParts(ctx context.Context, bucketName, key string, id object.ID, after, limit int) ([]object.Part, error)
	// CompleteUpload turns an upload into a version of its key, in one
	// step: the version appears, and the upload and the parts that were
	// not listed are gone. Whether the listed parts make an object is
	// decided against the parts as they are stored at that moment
	// (object.CheckCompletion). The error wraps domain.ErrConflict when a
	// listed part is no longer the one that was sealed.
	CompleteUpload(ctx context.Context, c Completion) (PutResult, error)
	// AbortUpload removes an upload and its parts, and returns the parts:
	// nothing refers to their data any more. The list may be cut short.
	AbortUpload(ctx context.Context, bucketName, key string, id object.ID) ([]object.Part, error)
	// ScanUploads calls visit for the uploads in progress in a bucket, in
	// ascending order of key and then of ID, beginning after start.
	ScanUploads(ctx context.Context, bucketName string, start UploadCursor, visit func(object.Upload) ScanControl) error
}

// UploadCursor says where a scan of uploads begins: at the first upload of
// Key, or, when After is not zero, after the upload of Key with that ID.
type UploadCursor struct {
	Key   string
	After object.ID
}

// Completion is the request to complete a multipart upload.
type Completion struct {
	Bucket string
	Key    string
	ID     object.ID
	// VersionID is the ID of the version to be.
	VersionID object.ID
	// ETag is the entity tag of the object the listed parts make.
	ETag string
	At   time.Time
	// CreatedBy is who completes the upload, as the audit trail names
	// actors: the version's object.Version.CreatedBy. Like that of a put
	// it is recorded only where every member of the cluster knows it, and
	// the completion succeeds either way.
	CreatedBy string
	// Parts are the listed parts in order: for each its Number, ETag and
	// Checksums as the client named them, the DataID of the stored part
	// that was sealed, and the Seal of its place.
	Parts []object.Part
}
