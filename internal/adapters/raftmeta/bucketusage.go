package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// What every bucket holds, in the state (ADR-0023, "Bucket usage"), and the
// quota that is judged by it (ADR-0024).
//
//	bucketusage   bucket name   -> what bucket.Usage.Record writes
//
// The record is the domain's, byte for byte: it is written with
// bucket.Usage.Record and read with bucket.ParseUsage, and kept through
// Usage.Add, Counting, Step and Finish and nothing else. This file decides
// only when they are called and with what.
//
// A record is made by an entry of schema 5 and by no other: exact and empty
// by the bucket.create that makes the bucket, or being counted by the first
// bucket.usage.walk for a bucket that was there before. From then on EVERY
// entry that changes the versions of a key or the parts of an upload keeps
// it, whatever its schema byte - a proposer that learnt the level a moment
// late writes a put as schema 4 into a cluster at level 5, and a count that
// missed it would be wrong for good. That is safe for the reason the byte is:
// every member decides alike from the log alone. No member of the release
// before can be past the entry that made the record - it stopped there - so
// every member that applies a later put has the record and keeps it.
//
// Nothing here exists in a state until an entry of schema 5 was applied to
// it: the bucket "bucketusage" is made when the first record is written, and
// while it is not there every function below returns at once, without having
// read or written anything. A state that was never written with schema 5 is
// byte for byte what the release before wrote.
//
// Which functions change a key or an upload, and so keep the record, each
// with ONE bucket.UsageDelta for a key and an upload (usageKeeper.add):
//
//	applyPutObject       the version it commits, less the null version it
//	                     replaces where versions are not kept or are suspended
//	applyCompleteUpload  the same, and all the parts of the upload out of
//	                     upload_bytes: one delta of the key and the upload
//	applyPutPart         the part, less the part of that number it replaces
//	applyAbortUpload     the parts of the upload
//	applyDeleteVersion   a version for good; the current version of a bucket
//	                     without versions; a delete marker, less the null
//	                     version it replaces while versioning is suspended
//	applyDeleteObject    the delete of schema 1
//	applyDeleteBucket    drops the record with the bucket
//
// What a lifecycle rule expires and what the collector of unfinished uploads
// aborts arrive as object.version.delete and upload.abort and are counted
// there. Tags, retention and legal hold change neither a size nor a count.

func bucketUsageBucket() []byte { return []byte("bucketusage") }

// readUsage returns the usage record of a bucket; the zero record and false
// when it has none. A record that cannot be read is an error that stops the
// state machine, as a damaged record of a bucket's use does: a member that
// went on with a record of its own making would hold other bytes than the
// others and say nothing.
func readUsage(tx *bbolt.Tx, name string) (bucket.Usage, bool, error) {
	records := tx.Bucket(bucketUsageBucket())
	if records == nil {
		return bucket.Usage{}, false, nil
	}
	raw := records.Get([]byte(name))
	if raw == nil {
		return bucket.Usage{}, false, nil
	}
	usage, err := bucket.ParseUsage(raw)
	if errors.Is(err, domain.ErrUnsupportedFormat) {
		return bucket.Usage{}, false, fmt.Errorf("stored usage of bucket %s: %w", name, err)
	}
	if err != nil {
		return bucket.Usage{}, false, fmt.Errorf("%w: stored usage of bucket %s: %s", domain.ErrIntegrity, name, err.Error())
	}
	return usage, true, nil
}

// writeUsage stores the usage record of a bucket and returns the bytes it
// stored: the domain's canonical record.
func writeUsage(tx *bbolt.Tx, name string, usage bucket.Usage) ([]byte, error) {
	raw, err := usage.Record()
	if err != nil {
		return nil, fmt.Errorf("usage of bucket %s: %w", name, err)
	}
	records, err := lazyBucket(tx, bucketUsageBucket(), true)
	if err != nil {
		return nil, err
	}
	return raw, records.Put([]byte(name), raw)
}

// dropUsage removes the usage record of a bucket that is being deleted: a
// bucket created under the name later begins with a record of its own, or
// with none.
func dropUsage(tx *bbolt.Tx, name string) error {
	records := tx.Bucket(bucketUsageBucket())
	if records == nil {
		return nil
	}
	return records.Delete([]byte(name))
}

// usageKeeper keeps the usage record of one bucket for the entry that is
// being applied. kept is false for a bucket without a record, and then
// nothing is read, counted or written.
type usageKeeper struct {
	bucket string
	usage  bucket.Usage
	kept   bool
}

// keepUsage opens the usage record of a bucket for the entry being applied.
func keepUsage(tx *bbolt.Tx, bucketName string) (usageKeeper, error) {
	usage, kept, err := readUsage(tx, bucketName)
	return usageKeeper{bucket: bucketName, usage: usage, kept: kept}, err
}

// seq returns the sequence of a version the entry makes in the bucket: its
// index, or one above the greatest sequence the bucket holds
// (bucket.Usage.NextSeq). Without a record that is the index, as it always
// was.
func (k usageKeeper) seq(now applied) uint64 {
	return k.usage.NextSeq(now.index)
}

// keyCurrent is what a key adds to the objects of its bucket and to their
// bytes: one object of the size of its current version, or nothing.
type keyCurrent struct {
	objects, bytes int64
}

// current reads what a key counts for among the objects of the bucket, from
// "objects/<bucket>", which holds the newest version of a key exactly when
// that is not a delete marker (versions.go). It reads nothing for a bucket
// without a record.
func (k usageKeeper) current(index *bbolt.Bucket, key string) (keyCurrent, error) {
	if !k.kept || index == nil {
		return keyCurrent{}, nil
	}
	raw := index.Get([]byte(key))
	if raw == nil {
		return keyCurrent{}, nil
	}
	var v countedVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return keyCurrent{}, fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
	}
	return keyCurrent{objects: 1, bytes: countedSize(v.Size)}, nil
}

// admits reports whether a change may be made to the bucket, whose quota is
// quotaBytes (ADR-0024; bucket.Usage.Admits, which is part of the replicated
// format). A bucket without a record, and one that is being counted for the
// first time, has no figure a quota could be held to and admits everything;
// a quota cannot be set on either.
func (k usageKeeper) admits(quotaBytes int64, d bucket.UsageDelta) bool {
	if !k.kept || !k.usage.Exact {
		return true
	}
	return k.usage.Admits(quotaBytes, d)
}

// add keeps the record by one change: of one key, of one upload, or - the
// completion of an upload - of both. It is the one place a change of a
// bucket reaches its usage record. A change that held a count at a bound
// begins a recount in the record itself (bucket.Usage.Add) and is counted
// for the entry.
func (k *usageKeeper) add(tx *bbolt.Tx, d bucket.UsageDelta, now applied) error {
	if !k.kept {
		return nil
	}
	next, clamped := k.usage.Add(d)
	if clamped && now.clamped != nil {
		*now.clamped++
	}
	k.usage = next
	_, err := writeUsage(tx, k.bucket, next)
	return err
}

// removes takes a version that the entry removed for good out of a delta.
func removes(d *bucket.UsageDelta, v *versionRecord) {
	if v == nil {
		return
	}
	d.Versions--
	d.StoredBytes -= countedSize(v.Size)
	switch object.ReplicationStatus(v.Replication) {
	case object.ReplicationPending:
		d.ReplicationPending--
	case object.ReplicationFailed:
		d.ReplicationFailed--
	}
}

// refusedByQuota is the refusal of an entry that would take a bucket past
// its quota. The entry changed nothing.
func refusedByQuota(bucketName string) *applyResult {
	return refusal(domain.ErrQuotaExceeded, "bucket %s", bucketName)
}

// applySetBucketQuota gives a bucket a quota, or with 0 bytes removes it.
// It is refused until the bucket's usage record is exact: a limit cannot be
// held to a count that is not known.
//
// What it refuses by - bucket.ValidateQuota - is part of the replicated
// format.
func applySetBucketQuota(tx *bbolt.Tx, cmd bucketQuotaCmd) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	if cmd.Bytes != 0 {
		if err := bucket.ValidateQuota(cmd.Bytes); err != nil {
			return applyResult{err: err}, nil
		}
	}
	usage, kept, err := readUsage(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if !kept || !usage.Exact {
		return refuse(domain.ErrConflict, "bucket %s is still being counted", cmd.Bucket)
	}
	b.QuotaBytes = cmd.Bytes
	data, err := json.Marshal(*b)
	if err != nil {
		return applyResult{}, err
	}
	if err := tx.Bucket(bucketsBucket()).Put([]byte(cmd.Bucket), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventBucketQuotaChanged, Data: ports.BucketQuotaEvent{Bucket: cmd.Bucket, QuotaBytes: cmd.Bytes}},
	}}, nil
}

// applyWalkBucketUsage is one entry of the walk that counts a bucket. It
// gives a bucket without a record one, in counting state; with Restart it
// begins the walk again, as a recount of a record that is exact; and
// otherwise it goes on where the record says the walk is. An entry for a
// record that is exact and not being counted changes nothing.
//
// The entry visits whole keys - every version of a key at once - in key
// order until it has visited Limit records, and when the keys are done, with
// what is left of Limit, the uploads in the order of their IDs, each for its
// parts or for one record when it has none. The entry that ends the uploads
// finishes the walk. All of it is read from the state inside this
// transaction and decided by the record and the entry alone.
func applyWalkBucketUsage(tx *bbolt.Tx, cmd usageWalkCmd) (applyResult, error) {
	if cmd.Limit < 1 || cmd.Limit > ports.MaxUsageWalk {
		return refuse(domain.ErrInvalidArgument, "a walk of %d records, from 1 to %d", cmd.Limit, ports.MaxUsageWalk)
	}
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil || tx.Bucket(objectsBucket()).Bucket([]byte(cmd.Bucket)) == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	usage, kept, err := readUsage(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	switch {
	case !kept, cmd.Restart:
		usage = usage.Counting()
	case !usage.Walking():
		raw, err := usage.Record()
		return applyResult{usage: raw}, err
	}
	limit := cmd.Limit
	var drift bool
	if usage.Count.Phase == bucket.UsagePhaseKeys {
		step, visited, err := walkKeys(tx, cmd.Bucket, usage.Count.Cursor, limit)
		if err != nil {
			return applyResult{}, err
		}
		limit -= visited
		if usage, err = usage.Step(step); err != nil {
			return applyResult{err: err}, nil
		}
	}
	if usage.Count.Phase == bucket.UsagePhaseUploads && limit > 0 {
		step, err := walkUploads(tx, cmd.Bucket, usage.Count.Cursor, limit)
		if err != nil {
			return applyResult{}, err
		}
		if usage, err = usage.Step(step); err != nil {
			return applyResult{err: err}, nil
		}
	}
	if usage.Count.Phase == bucket.UsagePhaseDone {
		if usage, drift, err = usage.Finish(); err != nil {
			return applyResult{err: err}, nil
		}
	}
	raw, err := writeUsage(tx, cmd.Bucket, usage)
	if err != nil {
		return applyResult{}, err
	}
	return applyResult{usage: raw, drift: drift}, nil
}

// countedVersion is what the walk reads of a stored version.
type countedVersion struct {
	Size        int64  `json:"size"`
	Seq         uint64 `json:"seq"`
	Replication string `json:"replication"`
}

// countedPart is what the walk reads of a stored part.
type countedPart struct {
	Size int64 `json:"size"`
}

// countedSize is what a size counts for in the usage of a bucket: the size,
// and nothing when it is below nothing. No proposer writes such a size, but
// an object.put is not refused for one - the release of schema 4 applies it,
// and a member that refused it would hold another state than the others while
// the cluster is at level 4 - so a version of -600 bytes can be stored.
//
// EVERY size that reaches a usage record goes through here: what a change
// adds and what it takes away (commitVersion, usageKeeper.current, removes,
// applyPutPart) and what the walk finds (held). Were a size counted one way by
// the changes and another by the walk, the record would not be a recount, and
// a version of a size below nothing would make room under a quota that no
// byte was freed for. It is part of the replicated format.
func countedSize(size int64) int64 {
	return max(size, 0)
}

// held adds a size to a count as a count is kept: never below nothing
// (countedSize) and never past the last number there is. No bucket holds a
// size that would do either; the walk must still come to the same figure on
// every member whatever is stored.
func held(count, size int64) int64 {
	size = countedSize(size)
	if count > math.MaxInt64-size {
		return math.MaxInt64
	}
	return count + size
}

// cursorKey reports whether a stored key can be the cursor of the walk: what
// bucket.Usage.Step and the stored record take for one - valid UTF-8 of at
// most object.MaxKeyLength bytes. Every key an entry can name is one
// (object.ValidateKey, in apply); a key that is not can only have been
// stored by a release from before keys were validated there.
func cursorKey(key string) bool {
	return len(key) <= object.MaxKeyLength && utf8.ValidString(key)
}

// count adds one stored version to what a step of the walk counted.
func (v countedVersion) count(step *bucket.UsageStep, current bool) {
	step.Counted.Versions++
	step.Counted.StoredBytes = held(step.Counted.StoredBytes, v.Size)
	if current {
		step.Counted.Objects++
		step.Counted.CurrentBytes = held(step.Counted.CurrentBytes, v.Size)
	}
	switch object.ReplicationStatus(v.Replication) {
	case object.ReplicationPending:
		step.Counted.ReplicationPending++
	case object.ReplicationFailed:
		step.Counted.ReplicationFailed++
	}
	step.High = max(step.High, v.Seq)
}

// afterKey returns the place in "versions/<bucket>" behind every version of
// a key: the encoded key ends with 0x00 0x01 and is the prefix of nothing
// but the versions of its key, so the same bytes ending in 0x00 0x02 are
// behind all of them and before the first version of every key that follows.
func afterKey(key string) []byte {
	at := encodeKey(key)
	at[len(at)-1] = encTerminator + 1
	return at
}

// walkKeys is the first phase of the walk: from behind the key the cursor
// names it visits whole keys - the current version in "objects/<bucket>" and
// every other version in "versions/<bucket>", merged in key order - while
// limit records are not yet visited, and says how many it visited. The step
// ends the phase when no key is left.
//
// A step never ends on a key that cannot be its cursor (cursorKey): it counts
// that key and goes on, past limit if it must, to the next key that can be,
// counts that too and ends there - or, when no such key follows, ends the
// phase. So the walk has been to exactly the keys that are, byte for byte, at
// or before its cursor, whatever keys the bucket holds; which is the rule
// bucket.Usage keeps a count by while changes go on beside the walk. A key
// that can be no cursor never stops the walk and is never counted twice.
//
// The order is that of the keys compared byte for byte, which is the order
// bucket.Usage decides by whether a change is behind the walk: "objects" is
// keyed by the key itself, and encodeKey keeps the order of the keys it
// encodes (TestTheWalkGoesInTheOrderTheRecordComparesBy).
func walkKeys(tx *bbolt.Tx, bucketName, cursor string, limit int) (bucket.UsageStep, int, error) {
	h, err := openHistory(tx, bucketName, false)
	if err != nil {
		return bucket.UsageStep{}, 0, err
	}
	current := h.current.Cursor()
	currentKey, currentRaw := current.First()
	if cursor != "" {
		if currentKey, currentRaw = current.Seek([]byte(cursor)); bytes.Equal(currentKey, []byte(cursor)) {
			currentKey, currentRaw = current.Next()
		}
	}
	var older *bbolt.Cursor
	var olderKey, olderRaw []byte
	if h.older != nil {
		older = h.older.Cursor()
		if olderKey, olderRaw = older.First(); cursor != "" {
			olderKey, olderRaw = older.Seek(afterKey(cursor))
		}
	}
	var step bucket.UsageStep
	visited := 0
	// pending says that the step has counted a key behind its cursor: one
	// that can be no cursor. It cannot end before a key that can.
	pending := false
	for {
		// The next key is the lesser of what the two indexes hold next.
		var key string
		switch {
		case currentKey == nil && olderKey == nil:
			step.End = true
			return step, visited, nil
		case olderKey == nil:
			key = string(currentKey)
		default:
			decoded, _, ok := decodeKey(olderKey)
			if !ok {
				return bucket.UsageStep{}, 0, fmt.Errorf("%w: stored version key", domain.ErrIntegrity)
			}
			key = decoded
			if currentKey != nil && string(currentKey) < key {
				key = string(currentKey)
			}
		}
		if visited >= limit && !pending {
			return step, visited, nil
		}
		if currentKey != nil && string(currentKey) == key {
			var v countedVersion
			if err := json.Unmarshal(currentRaw, &v); err != nil {
				return bucket.UsageStep{}, 0, fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
			}
			v.count(&step, true)
			visited++
			currentKey, currentRaw = current.Next()
		}
		for prefix := encodeKey(key); olderKey != nil && bytes.HasPrefix(olderKey, prefix); olderKey, olderRaw = older.Next() {
			var v countedVersion
			if err := json.Unmarshal(olderRaw, &v); err != nil {
				return bucket.UsageStep{}, 0, fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
			}
			v.count(&step, false)
			visited++
		}
		if pending = !cursorKey(key); !pending {
			step.Cursor = key
		}
	}
}

// walkUploads is the second phase of the walk: from behind the upload the
// cursor names it visits the uploads in progress in the order of their IDs -
// which, as bytes and as the text the cursor is, is the same order - and
// counts the parts of each, while limit records are not yet visited. An
// upload is as many records as it has parts, and one when it has none. The
// step ends the phase when no upload is left.
func walkUploads(tx *bbolt.Tx, bucketName, cursor string, limit int) (bucket.UsageStep, error) {
	var step bucket.UsageStep
	uploads, err := nested(tx, uploadsBucket(), bucketName, false)
	if err != nil {
		return bucket.UsageStep{}, err
	}
	if uploads == nil {
		step.End = true
		return step, nil
	}
	parts, err := nested(tx, partsBucket(), bucketName, false)
	if err != nil {
		return bucket.UsageStep{}, err
	}
	ids := uploads.Cursor()
	id, _ := ids.First()
	if cursor != "" {
		last, err := object.ParseID(cursor)
		if err != nil {
			return bucket.UsageStep{}, fmt.Errorf("%w: the cursor of the walk through the uploads of bucket %s", domain.ErrIntegrity, bucketName)
		}
		if id, _ = ids.Seek(last[:]); bytes.Equal(id, last[:]) {
			id, _ = ids.Next()
		}
	}
	for visited := 0; ; id, _ = ids.Next() {
		if id == nil {
			step.End = true
			return step, nil
		}
		if visited >= limit {
			return step, nil
		}
		if len(id) != object.IDSize {
			return bucket.UsageStep{}, fmt.Errorf("%w: stored upload ID", domain.ErrIntegrity)
		}
		records := 0
		if parts != nil {
			c := parts.Cursor()
			for k, raw := c.Seek(id); k != nil && bytes.HasPrefix(k, id); k, raw = c.Next() {
				var p countedPart
				if err := json.Unmarshal(raw, &p); err != nil {
					return bucket.UsageStep{}, fmt.Errorf("%w: stored part: %w", domain.ErrIntegrity, err)
				}
				step.Counted.UploadBytes = held(step.Counted.UploadBytes, p.Size)
				records++
			}
		}
		visited += max(records, 1)
		step.Cursor = object.ID(id).String()
	}
}
