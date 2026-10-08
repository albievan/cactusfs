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

// history is the versions of one bucket inside a transaction.
type history struct {
	current *bbolt.Bucket // objects/<bucket>
	older   *bbolt.Bucket // versions/<bucket>; nil when the bucket never kept any
}

func openHistory(tx *bbolt.Tx, bucketName string, write bool) (history, error) {
	h := history{current: tx.Bucket(objectsBucket()).Bucket([]byte(bucketName))}
	holder := tx.Bucket(versionsBucket())
	if holder == nil {
		return h, nil
	}
	if write {
		older, err := holder.CreateBucketIfNotExists([]byte(bucketName))
		if err != nil {
			return history{}, err
		}
		h.older = older
		return h, nil
	}
	h.older = holder.Bucket([]byte(bucketName))
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
	err = h.each(key, func(k []byte, v *versionRecord) (bool, error) {
		if refOf(*v) == ref {
			found, storedKey = v, k
			return false, nil
		}
		return true, nil
	})
	return found, storedKey, err
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
	return h.older.Put(k, data)
}

// remove takes a version out of the state for good.
func (h history) remove(v versionRecord, storedKey []byte) error {
	if storedKey == nil {
		return h.current.Delete([]byte(v.Key))
	}
	return h.older.Delete(storedKey)
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
	return h.older.Delete(newestKey)
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

// dropData takes back the reference of a removed version to its stored data.
func dropData(tx *bbolt.Tx, v versionRecord) error {
	if v.DeleteMarker {
		return nil
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
	state := bucket.Versioning(b.Versioning)
	h, err := openHistory(tx, v.Bucket, state != bucket.Unversioned)
	if err != nil {
		return applyResult{}, err
	}
	var removed *versionRecord
	switch state {
	case bucket.VersioningEnabled:
		v.Versioned, v.Seq = true, now.index
		if err := h.retire(v.Key); err != nil {
			return applyResult{}, err
		}
	case bucket.VersioningSuspended:
		// The new version is the null version; the one there was goes.
		v.Versioned, v.Seq = false, now.index
		old, storedKey, err := h.find(v.Key, object.NullVersion)
		if err != nil {
			return applyResult{}, err
		}
		if old != nil {
			if err := h.remove(*old, storedKey); err != nil {
				return applyResult{}, err
			}
			removed = old
		}
		if err := h.retire(v.Key); err != nil {
			return applyResult{}, err
		}
	default:
		v.Versioned, v.Seq = false, 0
		if removed, err = readVersion(h.current, v.Key); err != nil {
			return applyResult{}, err
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return applyResult{}, err
	}
	if err := h.current.Put([]byte(v.Key), data); err != nil {
		return applyResult{}, err
	}
	if err := addDataReference(tx.Bucket(dataBucket()), v.DataID, 1); err != nil {
		return applyResult{}, err
	}
	if removed != nil {
		if err := dropData(tx, *removed); err != nil {
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
	if err := index.Delete([]byte(cmd.Key)); err != nil {
		return applyResult{}, err
	}
	if err := dropData(tx, *previous); err != nil {
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
		if err := h.remove(*target, storedKey); err != nil {
			return applyResult{}, err
		}
		if err := dropData(tx, *target); err != nil {
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
		if err := h.current.Delete([]byte(cmd.Key)); err != nil {
			return applyResult{}, err
		}
		if err := dropData(tx, *previous); err != nil {
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
	var removed *versionRecord
	if state == bucket.VersioningSuspended {
		// The marker is the null version; the null version there was goes.
		old, storedKey, err := h.find(cmd.Key, object.NullVersion)
		if err != nil {
			return applyResult{}, err
		}
		if old != nil {
			if err := h.remove(*old, storedKey); err != nil {
				return applyResult{}, err
			}
			if err := dropData(tx, *old); err != nil {
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
