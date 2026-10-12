package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/object"
)

// MaxReadBatch is how many items one batch of reads can carry.
const MaxReadBatch = 256

// ObjectRead is a number of reads of one version by one reader.
type ObjectRead struct {
	Bucket string
	Key    string
	// Version names the version that was read, as clients name it.
	Version object.VersionRef
	// VersionID is the ID of the version that was read, the one it has of
	// its own (object.Version.VersionID) - a null version has one too. A
	// name can come to mean another version before the read is recorded:
	// "null" names whatever was put last where no versions are kept. The ID
	// tells them apart, and a read is recorded only for the version that
	// still has both.
	VersionID object.ID
	// Read is who read it and how often.
	Read object.Read
}

// ReadBatch is what one node counted since its last batch. Node, Boot and
// Seq identify it: a batch that is sent again - because the outcome of
// sending it was not known - is recorded once.
type ReadBatch struct {
	// Node is the ID of the node that counted.
	Node string
	// Boot is a number the node chose when it started, and Seq the number
	// of the batch since then, each greater than the one before.
	Boot uint64
	Seq  uint64
	// Items are the reads, at most MaxReadBatch of them.
	Items []ObjectRead
}

// ListedVersion names a version that a listing has shown, for a question
// about several at once.
type ListedVersion struct {
	Key string
	// Version names the version as clients name it.
	Version object.VersionRef
	// VersionID is the ID the version has of its own, a null version too:
	// "null" may have come to name another version since the listing (see
	// ObjectRead.VersionID).
	VersionID object.ID
}

// ObjectAccessStore is the replicated record of who read which object
// version, how often and when (ADR-0016). It is a count, not evidence.
type ObjectAccessStore interface {
	// AddObjectReads adds a batch of reads to the records of the versions
	// they name, as of the store's own time. A batch the store has seen
	// from that node - the same Boot, and a Seq no greater than the last -
	// changes nothing and is not an error. An item whose version does not
	// exist - or is no longer the version that was read: it was overwritten
	// or deleted meanwhile - or whose read cannot be recorded, is left out;
	// the others are recorded. In a cluster, a batch of a node the cluster
	// does not know is left out whole. The error wraps
	// domain.ErrInvalidArgument for a batch that names no node or has too
	// many items, and domain.ErrUnavailable while a member of the cluster
	// does not know such records.
	AddObjectReads(ctx context.Context, batch ReadBatch) error
	// ObjectAccess returns what is known of the reading of a version: the
	// zero record when it was never read. The error wraps
	// domain.ErrBucketNotFound or domain.ErrVersionNotFound.
	ObjectAccess(ctx context.Context, bucketName, key string, ref object.VersionRef) (object.Access, error)
	// ObjectAccesses is ObjectAccess for several versions of one bucket,
	// answered from one view of the store: the record of versions[i] is at
	// i of what it returns. A version that is no longer there - or whose
	// name has come to mean another version than the one with VersionID -
	// has the zero record, as one that was never read has: a listing is not
	// refused for an object that was deleted behind it. A listing asks for
	// one page, so at most object.MaxListKeys versions; more wrap
	// domain.ErrInvalidArgument. The error wraps domain.ErrBucketNotFound.
	ObjectAccesses(ctx context.Context, bucketName string, versions []ListedVersion) ([]object.Access, error)
}
