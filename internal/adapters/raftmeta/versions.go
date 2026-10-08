package raftmeta

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Object versions in the state (docs/adr/0008-object-versions.md).
//
// "objects/<bucket>/<key>" holds the current version of a key whenever the
// newest version is not a delete marker. "versions/<bucket>" holds every
// other version: those that are no longer current, and the delete markers.
// The invariant, kept by every function below:
//
//	objects[key] exists  <=>  the newest version of key is not a marker,
//	                          and objects[key] is that version.
//
// A key in "versions" is enc(object key) ‖ ^seq ‖ version ID: the versions of
// one object key are together, newest first.
//
// "versionids/<bucket>" finds a version in "versions" by its name without
// walking the versions of its key: enc(object key) ‖ version ID -> ^seq. It
// is derived from "versions" and rebuilt when it is missing. A key can have
// any number of versions; no request, and no entry being applied, may cost
// time in proportion to that number.

const (
	versionSuffixSize = 8 + object.IDSize
	// encTerminator ends an encoded object key. Inside the key a zero byte
	// is written as 0x00 0xFF, so the terminator 0x00 0x01 cannot occur
	// there, sorts before any continuation, and no encoded key is the
	// prefix of another.
	encEscape     = 0xFF
	encTerminator = 0x01
)

// encodeKey returns the order-preserving, prefix-free encoding of an object
// key. Object keys may contain any byte, a zero included.
func encodeKey(key string) []byte {
	out := make([]byte, 0, len(key)+2)
	for i := 0; i < len(key); i++ {
		out = append(out, key[i])
		if key[i] == 0 {
			out = append(out, encEscape)
		}
	}
	return append(out, 0, encTerminator)
}

// decodeKey splits a key of "versions" into the object key and what follows
// it. ok is false for bytes that encodeKey did not produce.
func decodeKey(stored []byte) (key string, rest []byte, ok bool) {
	var out []byte
	for i := 0; i < len(stored); i++ {
		if stored[i] != 0 {
			out = append(out, stored[i])
			continue
		}
		if i+1 >= len(stored) {
			return "", nil, false
		}
		switch stored[i+1] {
		case encEscape:
			out = append(out, 0)
			i++
		case encTerminator:
			return string(out), stored[i+2:], true
		default:
			return "", nil, false
		}
	}
	return "", nil, false
}

// versionKey returns the key of a version in "versions".
func versionKey(v versionRecord) ([]byte, error) {
	out := encodeKey(v.Key)
	out = binary.BigEndian.AppendUint64(out, ^v.Seq)
	if !v.Versioned {
		return append(out, make([]byte, object.IDSize)...), nil
	}
	id, err := object.ParseID(v.VersionID)
	if err != nil {
		return nil, fmt.Errorf("%w: stored version ID: %w", domain.ErrIntegrity, err)
	}
	return append(out, id[:]...), nil
}

func decodeVersion(raw []byte) (*versionRecord, error) {
	var v versionRecord
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
	}
	return &v, nil
}

// refOf names a stored version as clients do.
func refOf(v versionRecord) string {
	if !v.Versioned {
		return object.NullVersion
	}
	return v.VersionID
}

func versionIDsBucket() []byte { return []byte("versionids") }

// locator returns the key of a version in "versionids", for a version named
// as clients name it. ok is false for a name that is neither "null" nor an
// ID.
func locator(key, ref string) (out []byte, ok bool) {
	out = encodeKey(key)
	if ref == object.NullVersion {
		return append(out, make([]byte, object.IDSize)...), true
	}
	id, err := object.ParseID(ref)
	if err != nil {
		return nil, false
	}
	return append(out, id[:]...), true
}

// ensureVersionIndex creates "versionids" from "versions" when the state
// does not have it.
func ensureVersionIndex(tx *bbolt.Tx) error {
	if tx.Bucket(versionIDsBucket()) != nil {
		return nil
	}
	index, err := tx.CreateBucket(versionIDsBucket())
	if err != nil {
		return err
	}
	holder := tx.Bucket(versionsBucket())
	return holder.ForEachBucket(func(name []byte) error {
		ids, err := index.CreateBucketIfNotExists(name)
		if err != nil {
			return err
		}
		return holder.Bucket(name).ForEach(func(k, _ []byte) error {
			if len(k) < versionSuffixSize {
				return fmt.Errorf("%w: stored version key", domain.ErrIntegrity)
			}
			split := len(k) - versionSuffixSize
			at := append(bytes.Clone(k[:split]), k[split+8:]...)
			return ids.Put(at, bytes.Clone(k[split:split+8]))
		})
	})
}

// history is the versions of one bucket inside a transaction.
type history struct {
	current *bbolt.Bucket // objects/<bucket>
	older   *bbolt.Bucket // versions/<bucket>; nil when the bucket never kept any
	ids     *bbolt.Bucket // versionids/<bucket>; nil exactly when older is
}

func openHistory(tx *bbolt.Tx, bucketName string, write bool) (history, error) {
	h := history{current: tx.Bucket(objectsBucket()).Bucket([]byte(bucketName))}
	holder := tx.Bucket(versionsBucket())
	if holder == nil {
		return h, nil
	}
	index := tx.Bucket(versionIDsBucket())
	if write {
		older, err := holder.CreateBucketIfNotExists([]byte(bucketName))
		if err != nil {
			return history{}, err
		}
		ids, err := index.CreateBucketIfNotExists([]byte(bucketName))
		if err != nil {
			return history{}, err
		}
		h.older, h.ids = older, ids
		return h, nil
	}
	h.older = holder.Bucket([]byte(bucketName))
	if h.older != nil {
		if h.ids = index.Bucket([]byte(bucketName)); h.ids == nil {
			return history{}, fmt.Errorf("%w: bucket %s has versions and no index of them", domain.ErrIntegrity, bucketName)
		}
	}
	return h, nil
}

// each visits the versions of a key that are in "versions", newest first,
// until visit returns false.
func (h history) each(key string, visit func(storedKey []byte, v *versionRecord) (bool, error)) error {
	if h.older == nil {
		return nil
	}
	prefix := encodeKey(key)
	cursor := h.older.Cursor()
	for k, raw := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, raw = cursor.Next() {
		v, err := decodeVersion(raw)
		if err != nil {
			return err
		}
		more, err := visit(bytes.Clone(k), v)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

// find returns the version of a key that ref names, and where it is:
// storedKey is nil when it is the current version in "objects".
func (h history) find(key, ref string) (found *versionRecord, storedKey []byte, err error) {
	current, err := readVersion(h.current, key)
	if err != nil {
		return nil, nil, err
	}
	if current != nil && refOf(*current) == ref {
		return current, nil, nil
	}
	at, ok := locator(key, ref)
	if !ok || h.older == nil {
		return nil, nil, nil
	}
	inverted := h.ids.Get(at)
	if inverted == nil {
		return nil, nil, nil
	}
	// The key in "versions": the object key, the sequence, the ID.
	split := len(at) - object.IDSize
	storedKey = append(append(bytes.Clone(at[:split]), inverted...), at[split:]...)
	raw := h.older.Get(storedKey)
	if raw == nil {
		return nil, nil, fmt.Errorf("%w: the index of versions names one that is not there", domain.ErrIntegrity)
	}
	if found, err = decodeVersion(raw); err != nil {
		return nil, nil, err
	}
	return found, storedKey, nil
}

// retire moves the current version of a key, if there is one, to "versions".
func (h history) retire(key string) error {
	current, err := readVersion(h.current, key)
	if err != nil || current == nil {
		return err
	}
	if err := h.putOlder(*current); err != nil {
		return err
	}
	return h.current.Delete([]byte(key))
}

func (h history) putOlder(v versionRecord) error {
	k, err := versionKey(v)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := h.older.Put(k, data); err != nil {
		return err
	}
	split := len(k) - versionSuffixSize
	return h.ids.Put(append(bytes.Clone(k[:split]), k[split+8:]...), bytes.Clone(k[split:split+8]))
}

// dropOlder takes a version out of "versions" and out of the index of it.
func (h history) dropOlder(storedKey []byte) error {
	if err := h.older.Delete(storedKey); err != nil {
		return err
	}
	split := len(storedKey) - versionSuffixSize
	return h.ids.Delete(append(bytes.Clone(storedKey[:split]), storedKey[split+8:]...))
}

// remove takes a version out of the state for good.
func (h history) remove(v versionRecord, storedKey []byte) error {
	if storedKey == nil {
		return h.current.Delete([]byte(v.Key))
	}
	return h.dropOlder(storedKey)
}

// promote restores the invariant after a version was removed: when the
// newest version left is not a delete marker, it is the current version.
func (h history) promote(key string) error {
	if h.current.Get([]byte(key)) != nil {
		return nil
	}
	var newest *versionRecord
	var newestKey []byte
	if err := h.each(key, func(k []byte, v *versionRecord) (bool, error) {
		newest, newestKey = v, k
		return false, nil
	}); err != nil || newest == nil || newest.DeleteMarker {
		return err
	}
	data, err := json.Marshal(*newest)
	if err != nil {
		return err
	}
	if err := h.current.Put([]byte(key), data); err != nil {
		return err
	}
	return h.dropOlder(newestKey)
}

func readBucket(tx *bbolt.Tx, name string) (*bucketRecord, error) {
	raw := tx.Bucket(bucketsBucket()).Get([]byte(name))
	if raw == nil {
		return nil, nil
	}
	var b bucketRecord
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%w: stored bucket: %w", domain.ErrIntegrity, err)
	}
	return &b, nil
}

func applySetVersioning(tx *bbolt.Tx, cmd versioningCmd) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	// The rule is part of the format: it says only which of three words
	// may follow which, and does not change between releases.
	if err := bucket.ValidateVersioningChange(bucket.Versioning(cmd.Status), b.ObjectLock); err != nil {
		return applyResult{err: err}, nil
	}
	b.Versioning = cmd.Status
	data, err := json.Marshal(*b)
	if err != nil {
		return applyResult{}, err
	}
	if err := tx.Bucket(bucketsBucket()).Put([]byte(cmd.Bucket), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventBucketVersioningChanged, Data: ports.BucketVersioningEvent{Bucket: cmd.Bucket, Status: cmd.Status}},
	}}, nil
}

// refusal is refuse, for a function that has more to return.
func refusal(err error, format string, args ...any) *applyResult {
	result, _ := refuse(err, format, args...)
	return &result
}

// removable reports, as a refusal, why a version may not be removed at the
// time of the entry: every path that takes a version out of the state for
// good asks here. bypass is the proposer's word that the caller may lift a
// governance retention. A version without a lock is always removable.
func removable(v *versionRecord, now applied, bypass bool) *applyResult {
	if v == nil {
		return nil
	}
	lock := v.Lock.toDomain()
	if lock.IsZero() {
		return nil
	}
	if now.at.IsZero() {
		result, _ := refuse(domain.ErrUnavailable, "an entry without a time cannot be judged against Object Lock")
		return &result
	}
	if err := lock.CheckDelete(now.at, bypass); err != nil {
		return &applyResult{err: err}
	}
	return nil
}

// dropData takes back the reference of a removed version to its stored data.
// For a version made of parts it removes the parts, and notes on v, which
// goes back to the proposer, the ones whose data is now to be removed.
func dropData(tx *bbolt.Tx, v *versionRecord) error {
	if v.DeleteMarker {
		return nil
	}
	if v.Parts > 0 {
		// Its data is that of its parts.
		id, err := object.ParseID(v.DataID)
		if err != nil {
			return fmt.Errorf("%w: stored data ID: %w", domain.ErrIntegrity, err)
		}
		if v.Dropped, err = dropExtents(tx, v.Bucket, id); err != nil {
			return err
		}
	}
	return addDataReference(tx.Bucket(dataBucket()), v.DataID, -1)
}

// applyPutObject commits a version. What becomes of the version that was
// current is decided here, from the state of the bucket as this entry finds
// it, and so identically on every replica.
func applyPutObject(tx *bbolt.Tx, v versionRecord, now applied) (applyResult, error) {
	b, err := readBucket(tx, v.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil || tx.Bucket(objectsBucket()).Bucket([]byte(v.Bucket)) == nil {
		return refuse(domain.ErrBucketNotFound, "%s", v.Bucket)
	}
	if v.DeleteMarker {
		return refuse(domain.ErrInvalidArgument, "a delete marker cannot be put")
	}
	// What the entry says is not taken on trust: an entry that cannot be
	// stored is refused, like any other that asks for what cannot be. It
	// must never stop the state machine - every replica would stop with
	// it, and stay stopped.
	if err := object.ValidateKey(v.Key); err != nil {
		return applyResult{err: err}, nil
	}
	versionID, errVersion := object.ParseID(v.VersionID)
	dataID, errData := object.ParseID(v.DataID)
	if errVersion != nil || errData != nil || versionID.IsZero() || dataID.IsZero() {
		return refuse(domain.ErrInvalidArgument, "version or data ID of the put")
	}
	// Stored data belongs to one version. A second version that named
	// the data of another could have it removed from under that one, by
	// being overwritten or deleted itself.
	if tx.Bucket(dataBucket()).Get(dataID[:]) != nil {
		return refuse(domain.ErrConflict, "data %s belongs to a version already", v.DataID)
	}
	// A put makes a version of one stream. One made of parts comes from
	// completing an upload, which is the only thing that may say so.
	if v.Parts != 0 || v.Encryption.KeyMode != "" {
		return refuse(domain.ErrInvalidArgument, "a put does not make a version of parts")
	}
	// What an answer names is never taken from an entry.
	v.Dropped = nil
	removed, refused, err := commitVersion(tx, b, &v, now)
	if err != nil {
		return applyResult{}, err
	}
	if refused != nil {
		return *refused, nil
	}
	if err := addDataReference(tx.Bucket(dataBucket()), v.DataID, 1); err != nil {
		return applyResult{}, err
	}
	if removed != nil {
		if err := dropData(tx, removed); err != nil {
			return applyResult{}, err
		}
	}
	stored := v
	return applyResult{previous: removed, stored: &stored, events: []ports.Event{
		{Type: ports.EventObjectCreated, Time: v.ModifiedAt, Data: objectEvent(v)},
	}}, nil
}

// applyDeleteObject is the delete of schema 1: it removes the current
// version of a key. It stays for logs written before versions existed.
func applyDeleteObject(tx *bbolt.Tx, cmd deleteObjectCmd) (applyResult, error) {
	index := tx.Bucket(objectsBucket()).Bucket([]byte(cmd.Bucket))
	if index == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b != nil && b.Versioning != "" {
		// A node of the previous release cannot be a member any more
		// once a bucket keeps versions; an entry like this one is a
		// mistake, and deleting history on it would be a worse one.
		return refuse(domain.ErrInvalidBucketState, "bucket %s keeps versions", cmd.Bucket)
	}
	previous, err := readVersion(index, cmd.Key)
	if err != nil || previous == nil {
		return applyResult{}, err
	}
	if refused := removable(previous, applied{}, false); refused != nil {
		return *refused, nil
	}
	if err := index.Delete([]byte(cmd.Key)); err != nil {
		return applyResult{}, err
	}
	if err := dropData(tx, previous); err != nil {
		return applyResult{}, err
	}
	return applyResult{previous: previous, events: []ports.Event{
		{Type: ports.EventObjectDeleted, Data: objectEvent(*previous)},
	}}, nil
}

// applyDeleteVersion deletes an object, or one version of it.
func applyDeleteVersion(tx *bbolt.Tx, cmd deleteVersionCmd, now applied) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil || tx.Bucket(objectsBucket()).Bucket([]byte(cmd.Bucket)) == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	if err := object.ValidateKey(cmd.Key); err != nil {
		return applyResult{err: err}, nil
	}
	state := bucket.Versioning(b.Versioning)
	h, err := openHistory(tx, cmd.Bucket, true)
	if err != nil {
		return applyResult{}, err
	}

	if cmd.VersionRef != "" {
		// One version, for good.
		target, storedKey, err := h.find(cmd.Key, cmd.VersionRef)
		if err != nil || target == nil {
			return applyResult{}, err
		}
		// This is where a locked version is kept. Nothing in the entry
		// lifts a legal hold or a compliance retention that has not
		// ended; the proposer's word lifts governance and nothing else;
		// and what "ended" means is the time of this entry, the same on
		// every replica.
		if refused := removable(target, now, cmd.Bypass); refused != nil {
			return *refused, nil
		}
		if err := h.remove(*target, storedKey); err != nil {
			return applyResult{}, err
		}
		if err := dropData(tx, target); err != nil {
			return applyResult{}, err
		}
		if err := h.promote(cmd.Key); err != nil {
			return applyResult{}, err
		}
		return applyResult{previous: target, events: []ports.Event{
			{Type: ports.EventObjectDeleted, Time: cmd.At, Data: objectEvent(*target)},
		}}, nil
	}

	if state == bucket.Unversioned {
		previous, err := readVersion(h.current, cmd.Key)
		if err != nil || previous == nil {
			return applyResult{}, err
		}
		if refused := removable(previous, now, false); refused != nil {
			return *refused, nil
		}
		if err := h.current.Delete([]byte(cmd.Key)); err != nil {
			return applyResult{}, err
		}
		if err := dropData(tx, previous); err != nil {
			return applyResult{}, err
		}
		return applyResult{previous: previous, events: []ports.Event{
			{Type: ports.EventObjectDeleted, Time: cmd.At, Data: objectEvent(*previous)},
		}}, nil
	}

	// The bucket keeps versions: the delete is itself a version, a marker.
	marker := versionRecord{
		Bucket: cmd.Bucket, Key: cmd.Key, VersionID: cmd.MarkerID, ModifiedAt: cmd.At.UTC(),
		// A marker has no data; the ID of none is all zeros.
		DataID: object.ID{}.String(),
		Seq:    now.index, DeleteMarker: true, Versioned: state == bucket.VersioningEnabled,
	}
	if _, err := object.ParseID(cmd.MarkerID); err != nil {
		return refuse(domain.ErrInvalidArgument, "delete marker ID")
	}
	if marker.Versioned {
		if taken, _, err := h.find(cmd.Key, cmd.MarkerID); err != nil {
			return applyResult{}, err
		} else if taken != nil {
			return refuse(domain.ErrConflict, "version %s of %s/%s exists", cmd.MarkerID, cmd.Bucket, cmd.Key)
		}
	}
	var removed *versionRecord
	if state == bucket.VersioningSuspended {
		// The marker is the null version; the null version there was goes.
		old, storedKey, err := h.find(cmd.Key, object.NullVersion)
		if err != nil {
			return applyResult{}, err
		}
		if refused := removable(old, now, false); refused != nil {
			return *refused, nil
		}
		if old != nil {
			if err := h.remove(*old, storedKey); err != nil {
				return applyResult{}, err
			}
			if err := dropData(tx, old); err != nil {
				return applyResult{}, err
			}
			removed = old
		}
	}
	if err := h.retire(cmd.Key); err != nil {
		return applyResult{}, err
	}
	if err := h.putOlder(marker); err != nil {
		return applyResult{}, err
	}
	events := []ports.Event{{Type: ports.EventObjectDeleteMarkerCreated, Time: cmd.At, Data: objectEvent(marker)}}
	if removed != nil {
		events = append(events, ports.Event{Type: ports.EventObjectDeleted, Time: cmd.At, Data: objectEvent(*removed)})
	}
	return applyResult{previous: removed, marker: &marker, events: events}, nil
}

func applySetBucketLock(tx *bbolt.Tx, cmd bucketLockCmd) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	switch {
	case b == nil:
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	case !b.ObjectLock:
		return refuse(domain.ErrLockNotConfigured, "bucket %s was not created with Object Lock", cmd.Bucket)
	}
	if rule := cmd.Default; rule != nil {
		domainRule := bucket.DefaultRetention{Mode: object.RetentionMode(rule.Mode), Days: rule.Days, Years: rule.Years}
		if err := domainRule.Validate(); err != nil {
			return applyResult{err: err}, nil
		}
	}
	b.DefaultRetention = cmd.Default
	data, err := json.Marshal(*b)
	if err != nil {
		return applyResult{}, err
	}
	if err := tx.Bucket(bucketsBucket()).Put([]byte(cmd.Bucket), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventBucketLockChanged, Data: ports.BucketEvent{Bucket: cmd.Bucket}},
	}}, nil
}

// lockTarget finds the version a lock command is about, in a bucket whose
// versions can be locked.
func lockTarget(tx *bbolt.Tx, bucketName, key, ref string) (history, *versionRecord, []byte, *applyResult, error) {
	refused := func(err error, format string, args ...any) (history, *versionRecord, []byte, *applyResult, error) {
		result, _ := refuse(err, format, args...)
		return history{}, nil, nil, &result, nil
	}
	b, err := readBucket(tx, bucketName)
	if err != nil {
		return history{}, nil, nil, nil, err
	}
	switch {
	case b == nil || tx.Bucket(objectsBucket()).Bucket([]byte(bucketName)) == nil:
		return refused(domain.ErrBucketNotFound, "%s", bucketName)
	case !b.ObjectLock:
		return refused(domain.ErrLockNotConfigured, "bucket %s was not created with Object Lock", bucketName)
	}
	h, err := openHistory(tx, bucketName, true)
	if err != nil {
		return history{}, nil, nil, nil, err
	}
	target, storedKey, err := h.find(key, ref)
	if err != nil {
		return history{}, nil, nil, nil, err
	}
	switch {
	case target == nil:
		return refused(domain.ErrVersionNotFound, "%s/%s version %s", bucketName, key, ref)
	case target.DeleteMarker:
		return refused(domain.ErrMethodNotAllowed, "a delete marker cannot be locked")
	}
	return h, target, storedKey, nil, nil
}

// replaceVersion writes a version back where it was found.
func (h history) replaceVersion(v versionRecord, storedKey []byte) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if storedKey == nil {
		return h.current.Put([]byte(v.Key), data)
	}
	return h.older.Put(storedKey, data)
}

// applySetRetention changes the retention of a version, when the rules of
// Object Lock allow it at the time of the entry.
func applySetRetention(tx *bbolt.Tx, cmd retentionCmd, now applied) (applyResult, error) {
	h, target, storedKey, refused, err := lockTarget(tx, cmd.Bucket, cmd.Key, cmd.VersionRef)
	if err != nil || refused != nil {
		if refused != nil {
			return *refused, nil
		}
		return applyResult{}, err
	}
	if now.at.IsZero() {
		return refuse(domain.ErrUnavailable, "an entry without a time cannot be judged against Object Lock")
	}
	var next *object.Retention
	if cmd.Mode != "" {
		next = &object.Retention{Mode: object.RetentionMode(cmd.Mode), Until: cmd.RetainUntil}
	}
	lock := target.Lock.toDomain()
	if err := lock.CheckRetention(next, now.at, cmd.Bypass); err != nil {
		return applyResult{err: err}, nil
	}
	lock.Retention = next
	target.Lock = lockToRecord(lock)
	if err := h.replaceVersion(*target, storedKey); err != nil {
		return applyResult{}, err
	}
	stored := *target
	return applyResult{stored: &stored, events: []ports.Event{
		{Type: ports.EventObjectRetentionChanged, Time: now.at, Data: objectEvent(stored)},
	}}, nil
}

// applySetLegalHold puts a version under legal hold or takes it off.
func applySetLegalHold(tx *bbolt.Tx, cmd legalHoldCmd, now applied) (applyResult, error) {
	h, target, storedKey, refused, err := lockTarget(tx, cmd.Bucket, cmd.Key, cmd.VersionRef)
	if err != nil || refused != nil {
		if refused != nil {
			return *refused, nil
		}
		return applyResult{}, err
	}
	lock := target.Lock.toDomain()
	lock.LegalHold = cmd.On
	target.Lock = lockToRecord(lock)
	if err := h.replaceVersion(*target, storedKey); err != nil {
		return applyResult{}, err
	}
	stored := *target
	return applyResult{stored: &stored, events: []ports.Event{
		{Type: ports.EventObjectLegalHoldChanged, Time: now.at, Data: objectEvent(stored)},
	}}, nil
}

// commitVersion puts a new version of a key into the state, as the bucket
// keeps versions at the time of the entry, and returns the version it removed
// for good, if any. It fills in what the state decides: the sequence, and
// what keeps the version. When it refuses, nothing was changed. The
// references to stored data are the caller's to keep.
func commitVersion(tx *bbolt.Tx, b *bucketRecord, v *versionRecord, now applied) (*versionRecord, *applyResult, error) {
	// What keeps the version: what the put asked for, or else what the
	// bucket gives every version. Decided here, on the time of this entry.
	lock := v.Lock.toDomain()
	switch {
	case !b.ObjectLock && !lock.IsZero():
		return nil, refusal(domain.ErrLockNotConfigured, "bucket %s was not created with Object Lock", v.Bucket), nil
	case b.ObjectLock && now.at.IsZero():
		return nil, refusal(domain.ErrUnavailable, "an entry without a time cannot be judged against Object Lock"), nil
	case b.ObjectLock && lock.Retention != nil:
		if err := lock.Retention.Validate(now.at); err != nil {
			return nil, &applyResult{err: err}, nil
		}
	case b.ObjectLock && b.DefaultRetention != nil:
		rule := bucket.DefaultRetention{Mode: object.RetentionMode(b.DefaultRetention.Mode), Days: b.DefaultRetention.Days, Years: b.DefaultRetention.Years}
		lock.Retention = &object.Retention{Mode: rule.Mode, Until: rule.Until(now.at)}
	}
	v.Lock = lockToRecord(lock)
	state := bucket.Versioning(b.Versioning)
	h, err := openHistory(tx, v.Bucket, state != bucket.Unversioned)
	if err != nil {
		return nil, nil, err
	}
	var removed *versionRecord
	switch state {
	case bucket.VersioningEnabled:
		v.Versioned, v.Seq = true, now.index
		// IDs are random and do not repeat; an entry that names one the
		// key already has is a mistake, and two versions under one name
		// would make one of them unreachable.
		if taken, _, err := h.find(v.Key, v.VersionID); err != nil {
			return nil, nil, err
		} else if taken != nil {
			return nil, refusal(domain.ErrConflict, "version %s of %s/%s exists", v.VersionID, v.Bucket, v.Key), nil
		}
		if err := h.retire(v.Key); err != nil {
			return nil, nil, err
		}
	case bucket.VersioningSuspended:
		// The new version is the null version; the one there was goes.
		v.Versioned, v.Seq = false, now.index
		old, storedKey, err := h.find(v.Key, object.NullVersion)
		if err != nil {
			return nil, nil, err
		}
		if refused := removable(old, now, false); refused != nil {
			return nil, refused, nil
		}
		if old != nil {
			if err := h.remove(*old, storedKey); err != nil {
				return nil, nil, err
			}
			removed = old
		}
		if err := h.retire(v.Key); err != nil {
			return nil, nil, err
		}
	default:
		v.Versioned, v.Seq = false, 0
		if removed, err = readVersion(h.current, v.Key); err != nil {
			return nil, nil, err
		}
		if refused := removable(removed, now, false); refused != nil {
			return nil, refused, nil
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	if err := h.current.Put([]byte(v.Key), data); err != nil {
		return nil, nil, err
	}
	return removed, nil, nil
}
