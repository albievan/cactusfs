package ports

import (
	"context"
	"io"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// ReplicationStore is the replicated record of which bucket is the replica
// of another deployment's, and of the walk that sends another deployment
// what it lacks (ADR-0026). Every change is committed through Raft before
// the call returns; reads are strongly consistent. Until every member of
// the cluster knows entry schema 5 a change wraps domain.ErrUnavailable.
//
// Whether a bucket is a replica is read with BucketStore.GetBucket
// (bucket.Bucket.ReplicaFrom).
type ReplicationStore interface {
	// SetBucketReplica makes a bucket the replica of the deployment
	// source: from then on it takes versions and delete markers from that
	// deployment and from nobody else (ObjectIndex.PutVersion). With an
	// empty source the bucket is promoted: it takes writes again, and
	// nothing more from its source. The change carries its audit record
	// in the same entry; a record without an actor is not appended.
	//
	// A bucket becomes a replica only when it keeps versions and has no
	// replication configuration of its own (domain.ErrInvalidBucketState
	// otherwise), its usage record is exact (domain.ErrConflict until
	// then) and it holds no version at all (domain.ErrBucketNotEmpty):
	// the sequences of two deployments are unrelated, and a version the
	// bucket had would stay the current one of its key over what the
	// source sends. Naming the source the bucket already has, and
	// promoting a bucket that is no replica, change nothing and are no
	// error. The error wraps domain.ErrBucketNotFound, and
	// domain.ErrInvalidArgument for a source that
	// object.ValidateReplicaSource refuses.
	SetBucketReplica(ctx context.Context, name, source string, record audit.Record) error
	// PutResync stores how far the resync of a bucket has come, in place
	// of what was stored. A record that says the walk is done
	// (bucket.ResyncDone) also clears delivery.Cursor.ResyncNeeded of its
	// target. The error wraps domain.ErrBucketNotFound,
	// domain.ErrEntityNotFound when there is no such target, and
	// domain.ErrInvalidArgument for a record that bucket.Resync.Validate
	// refuses.
	PutResync(ctx context.Context, r bucket.Resync) error
	// GetResync returns the record of a bucket's resync; the zero record
	// for a bucket that was never resynced. The record goes with the
	// bucket. The error wraps domain.ErrBucketNotFound.
	GetResync(ctx context.Context, bucketName string) (bucket.Resync, error)
}

// SiteBucket is what another deployment says of a bucket that is to take
// replicas.
type SiteBucket struct {
	// Versioning is whether the bucket keeps versions.
	Versioning bucket.Versioning
	// ReplicaOf is the ID of the deployment the bucket is a replica of,
	// as the other deployment says it; empty when the bucket is none, or
	// when what answered does not know what a replica is. A source sends
	// nothing until it has read its own ID here.
	ReplicaOf string
}

// SitePart names one part of a multipart upload at another deployment.
type SitePart struct {
	Number int
	// ETag is the entity tag the other deployment gave for the part.
	ETag string
}

// SiteClient is another deployment, as the source of a replication speaks
// to it: an S3 client of that deployment's own API, with a key that the
// other deployment issued (ADR-0026). One client is for one target.
//
// A version travels as an object.Version whose Bucket is the bucket of the
// other deployment and whose Replica names this deployment and the sequence
// the version has here; its ID, time, entity tag, representation headers,
// user metadata, tags and lock travel with it. Nothing of its encryption
// does: the bytes are sent inside TLS and encrypted again where they
// arrive.
//
// Every error is a *delivery.Error: its class says what follows, and its
// text is the class and nothing else. An answer that says the other
// deployment already has what was sent is success, not an error.
type SiteClient interface {
	// Check asks what the other deployment says of a bucket. A bucket
	// that is not there is delivery.ClassNoSuchBucket.
	Check(ctx context.Context, bucketName string) (SiteBucket, error)
	// PutReplica sends a version of at most object.MaxSize bytes in one
	// request. body is read to its end, which must be v.Size bytes on; a
	// body cut short commits nothing.
	PutReplica(ctx context.Context, v object.Version, body io.Reader) error
	// CreateUpload begins the multipart upload a version of parts is sent
	// as, and returns its ID at the other deployment.
	CreateUpload(ctx context.Context, v object.Version) (uploadID string, err error)
	// PutPart sends one part of size bytes and returns its entity tag.
	PutPart(ctx context.Context, bucketName, key, uploadID string, number int, size int64, body io.Reader) (etag string, err error)
	// CompleteReplica completes the upload as the version v.
	CompleteReplica(ctx context.Context, v object.Version, uploadID string, parts []SitePart) error
	// AbortUpload ends an upload of this deployment's and gives up its
	// parts.
	AbortUpload(ctx context.Context, bucketName, key, uploadID string) error
	// ListReplicaUploads returns the IDs of the uploads in progress for a
	// key that this deployment began: what an attempt that was cut off
	// left behind.
	ListReplicaUploads(ctx context.Context, bucketName, key string) ([]string, error)
	// PutMarker sends a delete marker: v.DeleteMarker, with its ID, its
	// time and its Replica.
	PutMarker(ctx context.Context, v object.Version) error
	// HeadVersion asks whether the other deployment has a version, and
	// with which entity tag. It reads nothing of the object.
	HeadVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (etag string, found bool, err error)
	// PutTags replaces the tags of a version there; nil removes them.
	PutTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) error
	// PutRetention replaces the retention of a version there. Whether it
	// may is the other deployment's to say, by its own clock.
	PutRetention(ctx context.Context, bucketName, key string, ref object.VersionRef, r *object.Retention) error
	// PutLegalHold puts a version there under legal hold, or takes it
	// off.
	PutLegalHold(ctx context.Context, bucketName, key string, ref object.VersionRef, on bool) error
}
