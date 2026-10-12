package raftmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Who read which version, in the state (ADR-0016).
//
//	access/<bucket>   enc(key) ‖ version ID   -> accessRecord
//	accessflush       node ID                 -> flushRecord
//
// A record of "access" is found as a version is found in "versionids": by
// the object key and the version's ID, sixteen zero bytes for the null
// version. It lives exactly as long as its version: every path that removes
// a version for good removes it (dropData), and a bucket takes its records
// with it.
//
// "accessflush" keeps, for every node, which batch of reads it sent last, so
// that a batch sent again is applied as nothing.
//
// Neither exists in a state until an entry of schema 3 writes to it: a state
// that was never written with schema 3 is byte for byte what the previous
// release wrote.

const cmdAddReads = "object.reads.add"

// accessRecordVersion is the version of a stored accessRecord.
const accessRecordVersion = 1

func accessBucket() []byte      { return []byte("access") }
func accessFlushBucket() []byte { return []byte("accessflush") }

// addReadsCmd adds what one node counted to the records of the versions
// that were read. Node, Boot and Seq name the batch.
type addReadsCmd struct {
	Node  string     `json:"node"`
	Boot  uint64     `json:"boot"`
	Seq   uint64     `json:"seq"`
	Items []readItem `json:"items"`
}

// readItem is a number of reads of one version by one reader.
type readItem struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	// Version names the version as clients do: "null" or an ID.
	Version string `json:"version"`
	// VersionID is the ID the version that was read has of its own, a null
	// version too. The name alone does not say which version was read:
	// "null" is whatever was put last. An item is recorded only when the
	// version the name finds has this ID (as deleteVersionCmd.Expect holds
	// a delete to the version it was decided for).
	VersionID string `json:"version_id"`
	Who       string `json:"who"`
	Presigned bool   `json:"presigned,omitempty"`
	Downloads int64  `json:"downloads,omitempty"`
	Copies    int64  `json:"copies,omitempty"`
}

// flushRecord is the last batch of reads a node had applied.
type flushRecord struct {
	Boot uint64 `json:"boot"`
	Seq  uint64 `json:"seq"`
}

// accessRecord is an object.Access as it is stored. V is the version of the
// record's own format.
type accessRecord struct {
	V              int            `json:"v"`
	Downloads      int64          `json:"downloads"`
	Copies         int64          `json:"copies"`
	LastAt         time.Time      `json:"last_at"`
	Readers        []readerRecord `json:"readers,omitempty"`
	OtherReaders   int64          `json:"other_readers,omitempty"`
	OtherDownloads int64          `json:"other_downloads,omitempty"`
	OtherCopies    int64          `json:"other_copies,omitempty"`
}

type readerRecord struct {
	Who       string    `json:"who"`
	Presigned bool      `json:"presigned,omitempty"`
	Downloads int64     `json:"downloads"`
	Copies    int64     `json:"copies"`
	LastAt    time.Time `json:"last_at"`
}

func accessToRecord(a object.Access) accessRecord {
	record := accessRecord{
		V: accessRecordVersion, Downloads: a.Downloads, Copies: a.Copies, LastAt: a.LastAt.UTC(),
		OtherReaders: a.OtherReaders, OtherDownloads: a.OtherDownloads, OtherCopies: a.OtherCopies,
	}
	for _, r := range a.Readers {
		record.Readers = append(record.Readers, readerRecord{
			Who: r.Who, Presigned: r.Presigned, Downloads: r.Downloads, Copies: r.Copies, LastAt: r.LastAt.UTC(),
		})
	}
	return record
}

func (r accessRecord) toDomain() object.Access {
	access := object.Access{
		Downloads: r.Downloads, Copies: r.Copies, LastAt: r.LastAt,
		OtherReaders: r.OtherReaders, OtherDownloads: r.OtherDownloads, OtherCopies: r.OtherCopies,
	}
	for _, reader := range r.Readers {
		access.Readers = append(access.Readers, object.Reader{
			Who: reader.Who, Presigned: reader.Presigned, Downloads: reader.Downloads, Copies: reader.Copies, LastAt: reader.LastAt,
		})
	}
	return access
}

// lazyBucket returns a top-level bucket of the state that exists only once
// something was written to it, creating it when create is set. It is nil
// when there is none.
func lazyBucket(tx *bbolt.Tx, name []byte, create bool) (*bbolt.Bucket, error) {
	if create {
		return tx.CreateBucketIfNotExists(name)
	}
	return tx.Bucket(name), nil
}

// lazyNested is nested for a top-level bucket that exists only once
// something was written to it.
func lazyNested(tx *bbolt.Tx, parent []byte, bucketName string, create bool) (*bbolt.Bucket, error) {
	holder, err := lazyBucket(tx, parent, create)
	if err != nil || holder == nil {
		return nil, err
	}
	if create {
		return holder.CreateBucketIfNotExists([]byte(bucketName))
	}
	return holder.Bucket([]byte(bucketName)), nil
}

// readAccess returns the record at a place in "access/<bucket>"; the zero
// record when there is none.
func readAccess(records *bbolt.Bucket, at []byte) (object.Access, error) {
	if records == nil {
		return object.Access{}, nil
	}
	raw := records.Get(at)
	if raw == nil {
		return object.Access{}, nil
	}
	var record accessRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return object.Access{}, fmt.Errorf("%w: stored access record: %w", domain.ErrIntegrity, err)
	}
	if record.V != accessRecordVersion {
		return object.Access{}, fmt.Errorf("%w: stored access record of version %d", domain.ErrUnsupportedFormat, record.V)
	}
	return record.toDomain(), nil
}

// applyAddReads adds a batch of reads to the records of the versions it
// names, as of the time of the entry: the leader's, the same on every
// replica (ADR-0008). A batch the state has seen from its node is applied as
// nothing. An item that cannot be recorded - its version is gone, or was
// replaced by another under the same name, or is a delete marker, or the read
// is no read - is left out and the rest applied: what a node counted over
// half a minute must not be lost to one object that was deleted meanwhile.
//
// A batch is its node's: in a cluster that knows its nodes, one that names a
// node the cluster does not know is applied as nothing, and is not
// remembered. (Whether the proposer is that node is for whoever takes the
// proposal to decide - see Store.Subject. Here it can only be asked whether
// there is such a node.) A state without nodes is that of a node on its own,
// which registers nobody.
func applyAddReads(tx *bbolt.Tx, cmd addReadsCmd, now applied) (applyResult, error) {
	if err := cluster.ValidateNodeID(cmd.Node); err != nil {
		return applyResult{err: err}, nil
	}
	if len(cmd.Items) > ports.MaxReadBatch {
		return refuse(domain.ErrInvalidArgument, "a batch of %d reads", len(cmd.Items))
	}
	nodes := tx.Bucket(nodesBucket())
	if first, _ := nodes.Cursor().First(); first != nil && nodes.Get([]byte(cmd.Node)) == nil {
		return applyResult{}, nil
	}
	flushes, err := lazyBucket(tx, accessFlushBucket(), true)
	if err != nil {
		return applyResult{}, err
	}
	if raw := flushes.Get([]byte(cmd.Node)); raw != nil {
		var last flushRecord
		if err := json.Unmarshal(raw, &last); err != nil {
			return applyResult{}, fmt.Errorf("%w: stored batch of node %s: %w", domain.ErrIntegrity, cmd.Node, err)
		}
		// The node numbers its batches upwards from its start. One that
		// is not after the last was sent before, and its outcome was not
		// known to the sender.
		if last.Boot == cmd.Boot && cmd.Seq <= last.Seq {
			return applyResult{}, nil
		}
	}
	for _, item := range cmd.Items {
		if err := addRead(tx, item, now.at); err != nil {
			return applyResult{}, err
		}
	}
	data, err := json.Marshal(flushRecord{Boot: cmd.Boot, Seq: cmd.Seq})
	if err != nil {
		return applyResult{}, err
	}
	// No event: an event must not carry who read what (ADR-0016).
	return applyResult{}, flushes.Put([]byte(cmd.Node), data)
}

// addRead records one item of a batch, or leaves it out.
func addRead(tx *bbolt.Tx, item readItem, at time.Time) error {
	read := object.Read{Who: item.Who, Presigned: item.Presigned, Downloads: item.Downloads, Copies: item.Copies}
	if read.Validate() != nil || tx.Bucket(objectsBucket()).Bucket([]byte(item.Bucket)) == nil {
		return nil
	}
	h, err := openHistory(tx, item.Bucket, false)
	if err != nil {
		return err
	}
	target, _, err := h.find(item.Key, item.Version)
	if err != nil || target == nil || target.DeleteMarker {
		return err
	}
	// The name may have come to mean another version since the read: it is
	// the version that was read whose record this is, or nobody's.
	if read, err := object.ParseID(item.VersionID); err != nil || read.IsZero() || read.String() != target.VersionID {
		return nil
	}
	place, ok := locator(item.Key, refOf(*target))
	if !ok {
		return fmt.Errorf("%w: stored version ID", domain.ErrIntegrity)
	}
	records, err := lazyNested(tx, accessBucket(), item.Bucket, true)
	if err != nil {
		return err
	}
	access, err := readAccess(records, place)
	if err != nil {
		return err
	}
	data, err := json.Marshal(accessToRecord(access.Add([]object.Read{read}, at)))
	if err != nil {
		return err
	}
	return records.Put(place, data)
}

// dropAccess removes the record of who read a version that is being removed
// for good.
func dropAccess(tx *bbolt.Tx, v versionRecord) error {
	records, err := lazyNested(tx, accessBucket(), v.Bucket, false)
	if err != nil || records == nil {
		return err
	}
	place, ok := locator(v.Key, refOf(v))
	if !ok {
		return fmt.Errorf("%w: stored version ID", domain.ErrIntegrity)
	}
	return records.Delete(place)
}

var _ ports.ObjectAccessStore = (*Store)(nil)

// AddObjectReads implements ports.ObjectAccessStore.
func (s *Store) AddObjectReads(ctx context.Context, batch ports.ReadBatch) error {
	if err := cluster.ValidateNodeID(batch.Node); err != nil {
		return fmt.Errorf("raftmeta.AddObjectReads: %w", err)
	}
	if len(batch.Items) > ports.MaxReadBatch {
		return fmt.Errorf("raftmeta.AddObjectReads: %w: a batch of %d reads, at most %d", domain.ErrInvalidArgument, len(batch.Items), ports.MaxReadBatch)
	}
	cmd := &addReadsCmd{Node: batch.Node, Boot: batch.Boot, Seq: batch.Seq, Items: make([]readItem, len(batch.Items))}
	for i, item := range batch.Items {
		cmd.Items[i] = readItem{
			Bucket: item.Bucket, Key: item.Key, Version: item.Version.String(), VersionID: item.VersionID.String(),
			Who: item.Read.Who, Presigned: item.Read.Presigned, Downloads: item.Read.Downloads, Copies: item.Read.Copies,
		}
	}
	if _, err := s.propose(ctx, entry{Type: cmdAddReads, AddReads: cmd}); err != nil {
		return fmt.Errorf("raftmeta.AddObjectReads: %w", err)
	}
	return nil
}

// ObjectAccess implements ports.ObjectAccessStore.
func (s *Store) ObjectAccess(ctx context.Context, bucketName, key string, ref object.VersionRef) (object.Access, error) {
	var access object.Access
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		if tx.Bucket(objectsBucket()).Bucket([]byte(bucketName)) == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		h, err := openHistory(tx, bucketName, false)
		if err != nil {
			return err
		}
		target, _, err := h.find(key, ref.String())
		if err != nil {
			return err
		}
		if target == nil {
			return fmt.Errorf("%w: %s/%s version %s", domain.ErrVersionNotFound, bucketName, key, ref)
		}
		place, ok := locator(key, refOf(*target))
		if !ok {
			return fmt.Errorf("%w: stored version ID", domain.ErrIntegrity)
		}
		records, err := lazyNested(tx, accessBucket(), bucketName, false)
		if err != nil {
			return err
		}
		access, err = readAccess(records, place)
		return err
	})
	if err != nil {
		return object.Access{}, fmt.Errorf("raftmeta.ObjectAccess: %w", err)
	}
	return access, nil
}
