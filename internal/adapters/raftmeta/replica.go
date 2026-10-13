package raftmeta

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Replication between sites, in the state (ADR-0026).
//
//	resync   bucket name -> resyncRecord (JSON, "v":1)
//
// and three fields of records that were there before: "replica_from" of a
// bucket record, which makes the bucket the replica of one deployment;
// "replication" of a version record, where the version stands; and
// "replica_from" of an upload record.
//
// A replica bucket is written by exactly one source. Every entry that would
// make a version or a delete marker in it is refused here, in apply, unless
// it says that what it makes is the copy of one of that deployment - so it
// holds whoever asks and whichever node took the request: the state decides,
// not the caller (replicaAllowed). Removing a version for good is judged as
// everywhere.
//
// The copy is stored under the ID it has at its source and AT THE SEQUENCE
// it has there, not at the index of the entry that brought it; it is the
// current version of its key exactly when no version of the key outranks it
// (outranks). So one set of replica entries gives the same "objects",
// "versions" and "versionids", byte for byte, in whatever order it arrives.
// A version made here afterwards - when the bucket was promoted - is given a
// sequence above the greatest the bucket holds (bucket.Usage.NextSeq), which
// is why a bucket is a replica only with an exact usage record.
//
// "resync" is made when the first record is written, and a state that never
// held a replica bucket has none of this.

func resyncBucket() []byte { return []byte("resync") }

// resyncRecordVersion is the version of a resyncRecord.
const resyncRecordVersion = 1

// resyncRecord is the record of a resync as it is stored: its version, and
// then the payload of replication.resync.put.
type resyncRecord struct {
	V int `json:"v"`
	resyncCmd
}

// replicaAllowed reports, as a refusal, why a change that makes a version or
// a delete marker may not be made in the bucket: a replica bucket takes
// those of its source and no others, and no other bucket takes any of a
// source. A replica that object.Replica.Validate refuses - a source that is
// no deployment ID, a sequence that is none or beyond object.MaxReplicaSeq -
// is refused before the bucket is asked. All of it is part of the replicated
// format.
func replicaAllowed(b *bucketRecord, replica *object.Replica) *applyResult {
	switch {
	case replica == nil && b.ReplicaFrom == "":
		return nil
	case replica == nil:
		return refusal(domain.ErrInvalidBucketState, "bucket %s is a replica and takes versions from its source only", b.Name)
	}
	if err := replica.Validate(); err != nil {
		return &applyResult{err: err}
	}
	if b.ReplicaFrom != replica.From {
		return refusal(domain.ErrInvalidBucketState, "bucket %s is no replica of that deployment", b.Name)
	}
	return nil
}

// outranks reports whether a is a newer version of their key than b: the one
// of the greater sequence, and of two at one sequence the one whose ID is the
// lesser, the null version before every other. That is the order of
// "versions/<bucket>", whose keys end in the inverted sequence and the ID, so
// the current version of a key is the one that would be first there.
//
// Two versions of a key never share a sequence in a deployment that numbered
// them itself; a source at fault may send two. The order is total all the
// same, so that what a replica bucket holds does not depend on the order the
// copies arrived in.
func outranks(a, b versionRecord) (bool, error) {
	keyA, err := versionKey(a)
	if err != nil {
		return false, err
	}
	keyB, err := versionKey(b)
	if err != nil {
		return false, err
	}
	return bytes.Compare(keyA, keyB) < 0, nil
}

// newest returns the newest version of a key: the current one, or - when the
// key has none, because its newest version is a delete marker or it has no
// version - the first of those in "versions". Nil when the key has none.
func (h history) newest(key string) (*versionRecord, error) {
	current, err := readVersion(h.current, key)
	if err != nil || current != nil {
		return current, err
	}
	var first *versionRecord
	err = h.each(key, func(_ []byte, v *versionRecord) (bool, error) {
		first = v
		return false, nil
	})
	return first, err
}

// placeReplica puts the copy of a version or of a delete marker of another
// deployment among the versions of its key, by its sequence: as the current
// version when nothing outranks it and it is no marker, and among the others
// otherwise. The version that was current goes among the others when the
// copy outranks it. The history is open for writing.
//
// It keeps the invariant of versions.go: "objects" holds the newest version
// of a key exactly when that is not a delete marker.
func (h history) placeReplica(v versionRecord) error {
	newest, err := h.newest(v.Key)
	if err != nil {
		return err
	}
	if newest != nil {
		above, err := outranks(v, *newest)
		if err != nil {
			return err
		}
		if !above {
			return h.putOlder(v)
		}
	}
	// The copy is the newest version of its key.
	if err := h.retire(v.Key); err != nil {
		return err
	}
	if v.DeleteMarker {
		return h.putOlder(v)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return h.current.Put([]byte(v.Key), data)
}

// applySetBucketReplica makes a bucket the replica of a deployment, or - with
// no source - promotes it: from then on it takes writes again, and nothing
// more from its source.
//
// What it answers, in the order it is decided (ADR-0026): the bucket is not
// there; the source is no deployment ID; nothing changed and no refusal when
// the source is the one the bucket has, and when a bucket that is no replica
// is promoted; the bucket does not keep versions, or has a replication
// configuration of its own (domain.ErrInvalidBucketState); its usage record
// is not exact (domain.ErrConflict); the record says that it holds a version
// (domain.ErrBucketNotEmpty) - a delete marker is one. The sequences of two
// deployments are unrelated numbers: a version the bucket had would stay the
// current one of its key over what the source sends, with nothing lost and
// nothing saying so.
func applySetBucketReplica(tx *bbolt.Tx, cmd bucketReplicaCmd, now applied) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	if cmd.Source != "" {
		if err := object.ValidateReplicaSource(cmd.Source); err != nil {
			return applyResult{err: err}, nil
		}
	}
	if cmd.Source == b.ReplicaFrom {
		return applyResult{}, nil
	}
	if cmd.Source != "" {
		usage, kept, err := readUsage(tx, cmd.Bucket)
		if err != nil {
			return applyResult{}, err
		}
		switch {
		case b.Versioning != string(bucket.VersioningEnabled):
			return refuse(domain.ErrInvalidBucketState, "bucket %s does not keep versions", cmd.Bucket)
		case delivered(tx, b, bucket.ConfigReplication) != nil:
			return refuse(domain.ErrInvalidBucketState, "bucket %s is replicated itself", cmd.Bucket)
		case !kept || !usage.Exact:
			return refuse(domain.ErrConflict, "bucket %s is still being counted", cmd.Bucket)
		case usage.Versions != 0:
			return refuse(domain.ErrBucketNotEmpty, "bucket %s holds versions of its own", cmd.Bucket)
		}
	}
	b.ReplicaFrom = cmd.Source
	data, err := json.Marshal(*b)
	if err != nil {
		return applyResult{}, err
	}
	if err := tx.Bucket(bucketsBucket()).Put([]byte(cmd.Bucket), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventBucketReplicaModeChanged, Time: now.at, Data: ports.BucketReplicaModeEvent{Bucket: cmd.Bucket, Source: cmd.Source}},
	}}, nil
}

// applyPutResync stores how far the resync of a bucket has come, in place of
// what was stored. A record that says the walk is done clears "resync_needed"
// of the cursor of its target - the other deployment has been sent what the
// rows that were lost told of - when the stored replication configuration of
// the bucket names that target, and only then: a walk through a bucket that
// is not sent there has sent it nothing.
//
// The mark is of the target, not of the bucket (ADR-0026, "Resync"): one
// bucket that is done clears it for every bucket that is sent to the target.
// Who resyncs is to have been through all of them before it says so of the
// last (PLAN 8.1.6).
func applyPutResync(tx *bbolt.Tx, cmd resyncCmd) (applyResult, error) {
	if err := cmd.toDomain().Validate(); err != nil {
		return applyResult{err: err}, nil
	}
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	cursor, defined, err := readCursor(tx, cmd.Target)
	if err != nil {
		return applyResult{}, err
	}
	if !defined {
		return refuse(domain.ErrEntityNotFound, "target %s", cmd.Target)
	}
	data, err := json.Marshal(resyncRecord{V: resyncRecordVersion, resyncCmd: cmd})
	if err != nil {
		return applyResult{}, err
	}
	records, err := lazyBucket(tx, resyncBucket(), true)
	if err != nil {
		return applyResult{}, err
	}
	if err := records.Put([]byte(cmd.Bucket), data); err != nil {
		return applyResult{}, err
	}
	sent, err := sentTo(tx, cmd.Bucket, cmd.Target)
	if err != nil {
		return applyResult{}, err
	}
	if cmd.State == string(bucket.ResyncDone) && cursor.ResyncNeeded && sent {
		cursor.ResyncNeeded = false
		if err := writeCursor(tx, cursor); err != nil {
			return applyResult{}, err
		}
	}
	return applyResult{}, nil
}

// readResync returns the stored record of the resync of a bucket; the zero
// record and false when it was never resynced.
func readResync(tx *bbolt.Tx, bucketName string) (bucket.Resync, bool, error) {
	records := tx.Bucket(resyncBucket())
	if records == nil {
		return bucket.Resync{}, false, nil
	}
	raw := records.Get([]byte(bucketName))
	if raw == nil {
		return bucket.Resync{}, false, nil
	}
	var record resyncRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return bucket.Resync{}, false, fmt.Errorf("%w: stored resync of bucket %s: %w", domain.ErrIntegrity, bucketName, err)
	}
	if record.V != resyncRecordVersion {
		return bucket.Resync{}, false, fmt.Errorf("%w: stored resync of bucket %s of version %d", domain.ErrUnsupportedFormat, bucketName, record.V)
	}
	return record.toDomain(), true, nil
}

// dropResync removes the record of the resync of a bucket that is being
// deleted.
func dropResync(tx *bbolt.Tx, bucketName string) error {
	records := tx.Bucket(resyncBucket())
	if records == nil {
		return nil
	}
	return records.Delete([]byte(bucketName))
}
