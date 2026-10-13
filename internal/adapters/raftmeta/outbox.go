package raftmeta

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// The outbox: what is still to be delivered to targets, in the state
// (ADR-0023, "The outbox"; ADR-0025; ADR-0026).
//
//	outbox        10 bytes: the index of the entry (8, big endian), then the
//	              number of the row within the entry (2) -> outboxRecord
//	outboxcursor  target name -> cursorRecord
//	meta          "outboxcount"   -> rows the outbox holds (uint64, big endian)
//	              "outboxdropped" -> rows pushed out at the bound since the
//	                                 deployment began (uint64, big endian)
//
// A row is appended in the transaction of the entry that changed an object,
// for the consumers the two documents of its bucket give
// (bucket.NotificationConfig.Match, bucket.ReplicationConfig.Replicates):
// derived from the applied entry and the state alone, so every member appends
// the same rows. Like the usage record it is kept by EVERY entry that changes
// an object, whatever its schema byte, while a document exists - and a
// document can only be made by an entry of schema 5 (targets.go).
//
// Which functions append, each one row for a change and all its consumers:
//
//	applyPutObject       "put"       the version it commits, by who made it
//	applyCompleteUpload  "complete"  the version it commits, by who completed
//	applyDeleteVersion   "marker"    the delete marker it makes, by who deleted
//	                     "delete"    the version it removes for good - the one
//	                                 named, or the current version of a bucket
//	                                 that keeps none
//	applyDeleteObject    "delete"    the delete of schema 1, by nobody
//	applySetTags         "tags"      } for replication only, and only of a
//	applySetRetention    "retention" } version that is being, or was,
//	applySetLegalHold    "legalhold" } replicated; by nobody
//
// A version that a put or a marker replaces where versions are not kept, or
// are suspended, is told by the row of what replaced it and has none of its
// own. A part, the creation or the abort of an upload, and an entry that was
// refused append nothing. A version that arrives as the copy of one of
// another deployment is told to notification targets as the put or the
// marker it is, and never sent on (replica.go).
//
// The outbox never grows without bound: a row that would be one more than
// the bound pushes the oldest out, and that is counted - in "outboxdropped",
// and in the cursor of every consumer of the row that had not passed it.
//
// None of this exists in a state until a row was appended or a target
// defined: the buckets are made when first written, and a bucket without a
// document costs a put nothing.

func outboxBucket() []byte       { return []byte("outbox") }
func outboxCursorBucket() []byte { return []byte("outboxcursor") }
func outboxCountKey() []byte     { return []byte("outboxcount") }
func outboxDroppedKey() []byte   { return []byte("outboxdropped") }

// outboxRecordVersion is the version of an outboxRecord, and
// cursorRecordVersion that of a cursorRecord.
const (
	outboxRecordVersion = 1
	cursorRecordVersion = 1
)

// outboxRecord is a row of the outbox as it is stored: delivery.Item without
// its place, which is the key. The fields are in this order; the size and
// the entity tag are left out for a delete marker, and who made the change
// when that was not recorded. It holds nothing of the object's contents, its
// metadata or its tags.
type outboxRecord struct {
	V      int    `json:"v"`
	Event  string `json:"e"`
	Bucket string `json:"b"`
	Key    string `json:"k"`
	// VersionRef names the version as object.VersionRef.String() does:
	// "null" or 32 hexadecimal characters.
	VersionRef string    `json:"vid"`
	Size       int64     `json:"sz,omitempty"`
	ETag       string    `json:"et,omitempty"`
	At         time.Time `json:"at"`
	By         string    `json:"by,omitempty"`
	// To are the consumers, as delivery.Consumer.String() writes them.
	To []string `json:"to"`
}

func itemToRecord(i delivery.Item) outboxRecord {
	to := make([]string, len(i.To))
	for n, consumer := range i.To {
		to[n] = consumer.String()
	}
	return outboxRecord{
		V: outboxRecordVersion, Event: string(i.Event), Bucket: i.Bucket, Key: i.Key, VersionRef: i.Version.String(),
		Size: i.Size, ETag: i.ETag, At: i.At.UTC(), By: i.By, To: to,
	}
}

// decodeItem reads a stored row at its place. A row that is not what
// itemToRecord wrote is an error that stops the state machine.
func decodeItem(key, raw []byte) (delivery.Item, error) {
	damaged := func(err error) (delivery.Item, error) {
		return delivery.Item{}, fmt.Errorf("%w: stored outbox row: %s", domain.ErrIntegrity, err.Error())
	}
	seq, err := delivery.SeqFromKey(key)
	if err != nil {
		return damaged(err)
	}
	var r outboxRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return damaged(err)
	}
	if r.V != outboxRecordVersion {
		return delivery.Item{}, fmt.Errorf("%w: stored outbox row of version %d", domain.ErrUnsupportedFormat, r.V)
	}
	ref, err := object.ParseVersionRef(r.VersionRef)
	if err != nil {
		return damaged(err)
	}
	item := delivery.Item{
		Seq: seq, Event: delivery.Event(r.Event), Bucket: r.Bucket, Key: r.Key, Version: ref,
		Size: r.Size, ETag: r.ETag, At: r.At, By: r.By, To: make([]delivery.Consumer, len(r.To)),
	}
	for n, text := range r.To {
		if item.To[n], err = delivery.ParseConsumer(text); err != nil {
			return damaged(err)
		}
	}
	if err := item.Validate(); err != nil {
		return damaged(err)
	}
	return item, nil
}

// cursorRecord is how far delivery to one target has come, as it is stored:
// delivery.Cursor without the name, which is the key. Every field is always
// written; a place is delivery.Seq.String(), 20 hexadecimal characters.
type cursorRecord struct {
	V            int    `json:"v"`
	From         string `json:"from"`
	Acked        string `json:"acked"`
	AuditAcked   uint64 `json:"audit_acked"`
	Lost         uint64 `json:"lost"`
	ResyncNeeded bool   `json:"resync_needed"`
}

// readCursor returns the cursor of a target, and whether it has one: a
// target has a cursor exactly while it is defined.
func readCursor(tx *bbolt.Tx, target string) (delivery.Cursor, bool, error) {
	cursors := tx.Bucket(outboxCursorBucket())
	if cursors == nil {
		return delivery.Cursor{}, false, nil
	}
	raw := cursors.Get([]byte(target))
	if raw == nil {
		return delivery.Cursor{}, false, nil
	}
	cursor, err := decodeCursor(target, raw)
	return cursor, err == nil, err
}

func decodeCursor(target string, raw []byte) (delivery.Cursor, error) {
	var r cursorRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return delivery.Cursor{}, fmt.Errorf("%w: stored cursor of target %s: %w", domain.ErrIntegrity, target, err)
	}
	if r.V != cursorRecordVersion {
		return delivery.Cursor{}, fmt.Errorf("%w: stored cursor of target %s of version %d", domain.ErrUnsupportedFormat, target, r.V)
	}
	from, errFrom := delivery.ParseSeq(r.From)
	acked, errAcked := delivery.ParseSeq(r.Acked)
	if errFrom != nil || errAcked != nil {
		return delivery.Cursor{}, fmt.Errorf("%w: stored cursor of target %s: a place that is none", domain.ErrIntegrity, target)
	}
	return delivery.Cursor{Target: target, From: from, Acked: acked, AuditAcked: r.AuditAcked, Lost: r.Lost, ResyncNeeded: r.ResyncNeeded}, nil
}

func writeCursor(tx *bbolt.Tx, c delivery.Cursor) error {
	data, err := json.Marshal(cursorRecord{
		V: cursorRecordVersion, From: c.From.String(), Acked: c.Acked.String(), AuditAcked: c.AuditAcked, Lost: c.Lost, ResyncNeeded: c.ResyncNeeded,
	})
	if err != nil {
		return err
	}
	cursors, err := lazyBucket(tx, outboxCursorBucket(), true)
	if err != nil {
		return err
	}
	return cursors.Put([]byte(c.Target), data)
}

// readCounter returns one of the two counts of the outbox; nothing when it
// was never written.
func readCounter(tx *bbolt.Tx, key []byte) uint64 {
	raw := tx.Bucket(metaBucket()).Get(key)
	if len(raw) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

func writeCounter(tx *bbolt.Tx, key []byte, count uint64) error {
	return tx.Bucket(metaBucket()).Put(key, binary.BigEndian.AppendUint64(nil, count))
}

// consumersOf returns who a change of a version is for, by the two documents
// of its bucket: the notification targets whose rules match, sorted, and
// then the target the version is sent on to. by is who made the change,
// replica says that the version arrived as the copy of one of another
// deployment, and status is the replication status it had before the change.
//
// A bucket without a document is answered without reading anything.
func consumersOf(
	tx *bbolt.Tx, b *bucketRecord, event delivery.Event, key, by string, replica bool, status string,
) ([]delivery.Consumer, error) {
	var to []delivery.Consumer
	if document := delivered(tx, b, bucket.ConfigNotification); document != nil {
		config, err := bucket.ParseNotificationConfig(document)
		if err != nil {
			return nil, damagedDocument(b.Name, bucket.ConfigNotification, err)
		}
		to = append(to, config.Match(event, key)...)
	}
	if document := delivered(tx, b, bucket.ConfigReplication); document != nil {
		config, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			return nil, damagedDocument(b.Name, bucket.ConfigReplication, err)
		}
		to = append(to, config.Replicates(event, key, by, replica, object.ReplicationStatus(status))...)
	}
	return to, nil
}

// replicated reports whether one of the consumers sends the version on to
// another deployment.
func replicated(to []delivery.Consumer) bool {
	return slices.ContainsFunc(to, func(c delivery.Consumer) bool { return c.Kind == delivery.ConsumerReplication })
}

// place returns the place of the next row the entry appends.
func (a applied) place() delivery.Seq {
	seq := delivery.Seq{Index: a.index}
	if a.rows != nil {
		seq.Row = *a.rows
		*a.rows++
	}
	return seq
}

// bound returns how many rows the outbox holds.
func (a applied) bound() uint64 {
	if a.outboxRows > 0 {
		return a.outboxRows
	}
	return delivery.MaxOutboxRows
}

// rowOf returns the row that tells of a change of a version to the consumers
// given, without its place, and whether there is one: with no consumer there
// is none, and none for what cannot be kept as a row -
// delivery.Item.Validate, which is part of the replicated format. by is who
// made the change.
//
// The row says what a receiver is told and what a replicator needs to find
// the version: the time is that of the entry - of the newest timed entry,
// for one of the first schema - and a size below nothing is told as nothing,
// as it is counted. A name of an actor that cannot be kept is left out of
// the row, as bucket.ValidateCreator leaves it out of a record, and so is an
// entity tag of more than delivery.MaxItemETag bytes, which nothing holds an
// entry to.
//
// Whoever marks a version as waiting to be sent on asks here first (tellable):
// a version is PENDING exactly when the row that sends it on is appended.
func rowOf(tx *bbolt.Tx, now applied, event delivery.Event, v versionRecord, by string, to []delivery.Consumer) (delivery.Item, bool, error) {
	if len(to) == 0 {
		return delivery.Item{}, false, nil
	}
	ref := object.VersionRef{Null: true}
	if v.Versioned {
		id, err := object.ParseID(v.VersionID)
		if err != nil {
			return delivery.Item{}, false, fmt.Errorf("%w: stored version ID: %w", domain.ErrIntegrity, err)
		}
		ref = object.VersionRef{ID: id}
	}
	at := now.at
	if at.IsZero() {
		at = readTime(tx)
	}
	if bucket.ValidateCreator(by) != nil {
		by = ""
	}
	item := delivery.Item{Event: event, Bucket: v.Bucket, Key: v.Key, Version: ref, At: at, By: by, To: to}
	if !v.DeleteMarker {
		item.Size = countedSize(v.Size)
		if len(v.ETag) <= delivery.MaxItemETag {
			item.ETag = v.ETag
		}
	}
	return item, item.Validate() == nil, nil
}

// tellable returns the consumers a change of a version will be told to: those
// given when its row can be kept, and none when it cannot. It is asked before
// the version is stored, so that marking it as waiting to be sent on and
// appending the row that sends it on are one decision: what tell would leave
// out is for nobody.
func tellable(tx *bbolt.Tx, now applied, event delivery.Event, v versionRecord, by string, to []delivery.Consumer) ([]delivery.Consumer, error) {
	if _, kept, err := rowOf(tx, now, event, v, by, to); err != nil || !kept {
		return nil, err
	}
	return to, nil
}

// tell appends the row of a change of a version to the outbox, for the
// consumers given; with none, nothing. by is who made the change. At the
// bound the oldest row goes, and is counted as lost for who had not passed
// it. What cannot be kept as a row (rowOf) is not appended: every member
// leaves the same row out.
func tell(tx *bbolt.Tx, now applied, event delivery.Event, v versionRecord, by string, to []delivery.Consumer) error {
	item, kept, err := rowOf(tx, now, event, v, by, to)
	if err != nil || !kept {
		return err
	}
	item.Seq = now.place()
	data, err := json.Marshal(itemToRecord(item))
	if err != nil {
		return err
	}
	rows, err := lazyBucket(tx, outboxBucket(), true)
	if err != nil {
		return err
	}
	key := item.Seq.Key()
	if err := rows.Put(key[:], data); err != nil {
		return err
	}
	count := readCounter(tx, outboxCountKey()) + 1
	for count > now.bound() {
		dropped, err := dropOldest(tx, rows)
		if err != nil {
			return err
		}
		if !dropped {
			break
		}
		count--
	}
	return writeCounter(tx, outboxCountKey(), count)
}

// dropOldest pushes the oldest row out of an outbox that is past its bound,
// and reports whether there was one. The row is lost to each of its
// consumers that had not passed it (delivery.Cursor.Dropped): only the
// cursors of its consumers are read, so the cost of a drop does not grow
// with the number of targets.
func dropOldest(tx *bbolt.Tx, rows *bbolt.Bucket) (bool, error) {
	key, raw := rows.Cursor().First()
	if key == nil {
		return false, nil
	}
	item, err := decodeItem(key, raw)
	if err != nil {
		return false, err
	}
	for _, consumer := range item.To {
		cursor, defined, err := readCursor(tx, consumer.Target)
		if err != nil {
			return false, err
		}
		if !defined {
			// Its target was deleted since: nobody is waiting for it.
			continue
		}
		if next := cursor.Dropped(item); next != cursor {
			if err := writeCursor(tx, next); err != nil {
				return false, err
			}
		}
	}
	if err := rows.Delete(bytes.Clone(key)); err != nil {
		return false, err
	}
	return true, writeCounter(tx, outboxDroppedKey(), readCounter(tx, outboxDroppedKey())+1)
}

// trimOutbox removes the rows every target has passed: those at or before
// the lowest cursor there is, oldest first, at most delivery.MaxOutboxTrim of
// them for one entry. With no target left every row is behind every target.
func trimOutbox(tx *bbolt.Tx) error {
	rows := tx.Bucket(outboxBucket())
	if rows == nil {
		return nil
	}
	lowest, none := delivery.Seq{}, true
	if cursors := tx.Bucket(outboxCursorBucket()); cursors != nil {
		err := cursors.ForEach(func(name, raw []byte) error {
			cursor, err := decodeCursor(string(name), raw)
			if err != nil {
				return err
			}
			if none || lowest.After(cursor.Acked) {
				lowest, none = cursor.Acked, false
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// What goes is found first and removed then: a cursor of bbolt is not
	// to be moved on over what was deleted under it.
	var passed [][]byte
	c := rows.Cursor()
	for key, _ := c.First(); key != nil && len(passed) < delivery.MaxOutboxTrim; key, _ = c.Next() {
		seq, err := delivery.SeqFromKey(key)
		if err != nil {
			return fmt.Errorf("%w: stored outbox row: %s", domain.ErrIntegrity, err.Error())
		}
		if !none && seq.After(lowest) {
			break
		}
		passed = append(passed, bytes.Clone(key))
	}
	if len(passed) == 0 {
		return nil
	}
	for _, key := range passed {
		if err := rows.Delete(key); err != nil {
			return err
		}
	}
	count := readCounter(tx, outboxCountKey())
	return writeCounter(tx, outboxCountKey(), count-min(count, uint64(len(passed))))
}

// lastRow returns the place of the newest row of the outbox, and whether
// there is one.
func lastRow(tx *bbolt.Tx) (delivery.Seq, bool, error) {
	rows := tx.Bucket(outboxBucket())
	if rows == nil {
		return delivery.Seq{}, false, nil
	}
	key, _ := rows.Cursor().Last()
	if key == nil {
		return delivery.Seq{}, false, nil
	}
	seq, err := delivery.SeqFromKey(key)
	if err != nil {
		return delivery.Seq{}, false, fmt.Errorf("%w: stored outbox row: %s", domain.ErrIntegrity, err.Error())
	}
	return seq, true, nil
}

// applyAckOutbox says that a target was delivered everything up to a place,
// or was passed it; of the audit trail everything up to a sequence; and what
// became of versions that were to be replicated (ADR-0023, "outbox.ack", in
// the order given there).
//
//   - It is refused whole, with nothing changed: for a target that is not
//     there (domain.ErrEntityNotFound); and with domain.ErrInvalidArgument
//     for more than delivery.MaxAckResults results, any result for a target
//     that is not another deployment, a place that is not written as one,
//     and a result that delivery.Result.Validate refuses.
//   - An acknowledgement beyond the newest row counts as that row, and as
//     the cursor's own place when the outbox is empty.
//   - The cursor moves forward only (delivery.Cursor.Advanced); an
//     acknowledgement behind it is that of a node that led before, changes
//     nothing and is no error. The audit trail is acknowledged for a target
//     that is sent it, and for no other.
//   - Each result gives its status to the version it names - by a row that
//     is still there and was for this target as replication, or by itself,
//     of a bucket that is there and whose stored replication configuration
//     names this target - when the version still exists and is no replica,
//     and moves the two counts of the bucket's usage record with it.
//   - A result that is well formed and is not this target's to give - a row
//     that was for another target, or for this one as a notification; a
//     version of a bucket that is not there, has no replication
//     configuration, or one that names another target - changes nothing and
//     does not refuse the acknowledgement: the cursor moves and the other
//     results are taken. Such results are counted in the answer
//     (applyResult.unrelated), for who acknowledged to log.
//   - Then the rows every target has passed are trimmed.
//
// All of it is part of the replicated format.
func applyAckOutbox(tx *bbolt.Tx, cmd ackOutboxCmd, now applied) (applyResult, error) {
	cursor, defined, err := readCursor(tx, cmd.Target)
	if err != nil {
		return applyResult{}, err
	}
	target, err := readTarget(tx, cmd.Target)
	if err != nil {
		return applyResult{}, err
	}
	switch {
	case !defined || target == nil:
		return refuse(domain.ErrEntityNotFound, "target %s", cmd.Target)
	case len(cmd.Results) > delivery.MaxAckResults:
		return refuse(domain.ErrInvalidArgument, "%d results, at most %d", len(cmd.Results), delivery.MaxAckResults)
	case len(cmd.Results) > 0 && target.Kind != string(delivery.KindS3):
		return refuse(domain.ErrInvalidArgument, "results are of replication, and target %s is not another deployment", cmd.Target)
	}
	upto, err := delivery.ParseSeq(cmd.Upto)
	if err != nil {
		return applyResult{err: err}, nil
	}
	results := make([]delivery.Result, len(cmd.Results))
	for i, record := range cmd.Results {
		if results[i], err = record.toDomain(); err != nil {
			return refuse(domain.ErrInvalidArgument, "result %d names no row and no version", i)
		}
		if err := results[i].Validate(); err != nil {
			return applyResult{err: err}, nil
		}
	}

	// No acknowledgement passes what was not written yet.
	last, rows, err := lastRow(tx)
	if err != nil {
		return applyResult{}, err
	}
	if !rows {
		last = cursor.Acked
	}
	if upto.After(last) {
		upto = last
	}
	auditUpto := cmd.AuditUpto
	if !target.Audit {
		auditUpto = 0
	}
	if next := cursor.Advanced(upto, auditUpto); next != cursor {
		if err := writeCursor(tx, next); err != nil {
			return applyResult{}, err
		}
	}
	unrelated := 0
	for _, result := range results {
		mine, err := settle(tx, cmd.Target, result, now)
		if err != nil {
			return applyResult{}, err
		}
		if !mine {
			unrelated++
		}
	}
	return applyResult{unrelated: unrelated}, trimOutbox(tx)
}

// sentTo reports whether the versions of a bucket are sent on to a target:
// whether the bucket is there and its stored replication configuration names
// the target.
func sentTo(tx *bbolt.Tx, bucketName, target string) (bool, error) {
	b, err := readBucket(tx, bucketName)
	if err != nil || b == nil {
		return false, err
	}
	names, err := namedBy(bucketName, bucket.ConfigReplication, delivered(tx, b, bucket.ConfigReplication))
	if err != nil {
		return false, err
	}
	return slices.Contains(names, target), nil
}

// settle writes the status a result gives on the version it names, and keeps
// the usage record of its bucket by the change. It reports whether the result
// was the target's to give: one by a row, when the row is gone - trimmed or
// dropped, as happens - or was for the target as replication; one by a
// version, when the bucket is there and its stored replication configuration
// names the target. A result that is not changes nothing.
//
// Of those that are the target's, one that names what is gone - a row, a key
// or a version that is not there - changes nothing; neither does one for a
// version that arrived as a replica, whose status never changes, or for one
// that has the status already.
func settle(tx *bbolt.Tx, target string, r delivery.Result, now applied) (bool, error) {
	bucketName, key, ref := r.Bucket, r.Key, r.Version
	if r.ByRow() {
		rows := tx.Bucket(outboxBucket())
		if rows == nil {
			return true, nil
		}
		at := r.Seq.Key()
		raw := rows.Get(at[:])
		if raw == nil {
			return true, nil
		}
		item, err := decodeItem(at[:], raw)
		if err != nil {
			return false, err
		}
		if kind, consumer := item.For(target); !consumer || kind != delivery.ConsumerReplication {
			return false, nil
		}
		bucketName, key, ref = item.Bucket, item.Key, item.Version
	} else if sent, err := sentTo(tx, bucketName, target); err != nil || !sent {
		return false, err
	}
	if tx.Bucket(objectsBucket()).Bucket([]byte(bucketName)) == nil {
		return true, nil
	}
	h, err := openHistory(tx, bucketName, false)
	if err != nil {
		return false, err
	}
	v, storedKey, err := h.find(key, ref.String())
	if err != nil || v == nil {
		return true, err
	}
	was := object.ReplicationStatus(v.Replication)
	if was == object.ReplicationReplica || was == r.Status {
		return true, nil
	}
	v.Replication = string(r.Status)
	if err := h.replaceVersion(*v, storedKey); err != nil {
		return false, err
	}
	// The two counts of the bucket follow the status: out of the one the
	// version was counted in, into "failed" when that is what it is now.
	delta := bucket.UsageDelta{Key: key}
	switch was {
	case object.ReplicationPending:
		delta.ReplicationPending--
	case object.ReplicationFailed:
		delta.ReplicationFailed--
	}
	if r.Status == object.ReplicationFailed {
		delta.ReplicationFailed++
	}
	if delta.ReplicationPending == 0 && delta.ReplicationFailed == 0 {
		return true, nil
	}
	usage, err := keepUsage(tx, bucketName)
	if err != nil {
		return false, err
	}
	return true, usage.add(tx, delta, now)
}
