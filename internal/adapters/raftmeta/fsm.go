package raftmeta

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Names inside the state database. The layout is part of the snapshot format.
//
//	meta     "applied" -> index of the last applied Raft entry (uint64, big endian)
//	         "schema"  -> state schema version (uint64, big endian)
//	buckets  <bucket name> -> bucketRecord (JSON)
//	objects  <bucket name> (nested bucket) -> <object key> -> versionRecord (JSON)
//	versions <bucket name> (nested bucket) -> <enc(key) ‖ ^seq ‖ version ID>
//	         -> versionRecord (JSON): the versions that are no longer
//	         current, and the delete markers. See versions.go.
//	iam      "root" -> rootRecord (JSON)
//	cluster  "topology" -> topologyRecord (JSON)
//	nodes    <node ID> -> nodeRecord (JSON)
//	data     <data ID, 16 bytes> -> number of object versions that refer to it
//	         (uint64, big endian). Derived from "objects": it is rebuilt when
//	         missing, and exists so that the garbage collector can ask whether
//	         stored data is still in use without scanning every object.
//
// From state schema 3 on, and each only once something was written to it
// (access.go, subbuckets.go, tagdefs.go):
//
//	access      <bucket name> (nested bucket) -> <enc(key) ‖ version ID> -> accessRecord (JSON)
//	accessflush <node ID> -> flushRecord (JSON)
//	subbuckets  <bucket name> (nested bucket) -> <sub-bucket name> -> subBucketRecord (JSON)
//	tagdefs     <tag key> -> tagDefinitionRecord (JSON)
//
// From state schema 4 on, and only once something was written to it
// (bucketactivity.go):
//
//	bucketactivity <bucket name> -> activityRecord (JSON)
//
// State schema 1 is what entries of schema 1 produce. The first entry of
// schema 2 that is applied marks the state as schema 2, in its own
// transaction: until a cluster actually uses the newer entries, its state and
// its snapshots stay readable by the previous release, and afterwards the
// previous release refuses them instead of misreading them. The first entry
// of schema 3 marks it as schema 3 in the same way. Nothing that schema 3
// adds to the state exists before that: a state of schema 2 that was never
// written with schema 3 is the state the previous release wrote. And so for
// schema 4 (ADR-0019): its first entry marks the state, and until then a
// state of schema 3 is the state the release of schema 3 wrote.
const (
	stateSchemaVersion = 1
	stateSchemaTimed   = 2
	stateSchema3       = 3
	stateSchema4       = 4
	stateFileMode      = 0o600
)

func timeKey() []byte { return []byte("time") }

// applied is what the state machine knows about the entry it is applying.
type applied struct {
	// index is the entry's position in the Raft log.
	index uint64
	// at is the time of the entry: what the leader stamped, never earlier
	// than the time of any entry before it. Zero for an entry of schema 1.
	at time.Time
	// schema is the schema the entry was written in: what it may say.
	schema byte
}

// readTime returns the time of the newest timed entry in the state.
func readTime(tx *bbolt.Tx) time.Time {
	raw := tx.Bucket(metaBucket()).Get(timeKey())
	if len(raw) != 8 {
		return time.Time{}
	}
	nanos := binary.BigEndian.Uint64(raw)
	if nanos > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(0, int64(nanos)).UTC()
}

// stampState records the time of a timed entry and that the state now holds
// what only schema 2 can express. Time never goes back: an entry stamped
// earlier than one before it takes that one's time.
func stampState(tx *bbolt.Tx, at time.Time) (time.Time, error) {
	meta := tx.Bucket(metaBucket())
	if known := readTime(tx); at.Before(known) {
		at = known
	}
	nanos := at.UnixNano()
	if nanos < 0 {
		return time.Time{}, fmt.Errorf("%w: entry time before 1970", domain.ErrUnsupportedFormat)
	}
	if err := meta.Put(timeKey(), binary.BigEndian.AppendUint64(nil, uint64(nanos))); err != nil {
		return time.Time{}, err
	}
	return at.UTC(), raiseStateSchema(tx, stateSchemaTimed)
}

// readStateSchema returns the schema the state says it has.
func readStateSchema(tx *bbolt.Tx) uint64 {
	raw := tx.Bucket(metaBucket()).Get(schemaKey())
	if len(raw) != 8 {
		return stateSchemaVersion
	}
	return binary.BigEndian.Uint64(raw)
}

// raiseStateSchema marks the state as holding what only the given schema
// can express. The schema of a state never goes down.
func raiseStateSchema(tx *bbolt.Tx, schema uint64) error {
	if readStateSchema(tx) >= schema {
		return nil
	}
	return tx.Bucket(metaBucket()).Put(schemaKey(), binary.BigEndian.AppendUint64(nil, schema))
}

func metaBucket() []byte    { return []byte("meta") }
func bucketsBucket() []byte { return []byte("buckets") }
func objectsBucket() []byte { return []byte("objects") }
func iamBucket() []byte     { return []byte("iam") }
func appliedKey() []byte    { return []byte("applied") }
func schemaKey() []byte     { return []byte("schema") }
func rootKey() []byte       { return []byte("root") }
func clusterBucket() []byte { return []byte("cluster") }
func topologyKey() []byte   { return []byte("topology") }
func dataBucket() []byte    { return []byte("data") }
func nodesBucket() []byte   { return []byte("nodes") }

// applyResult is what the state machine returns for one entry. err is a
// domain error for a command that was validly applied but refused (the state
// is unchanged), or a fatal error that has stopped the state machine.
type applyResult struct {
	err      error
	previous *versionRecord
	// stored is the version a put committed; marker the delete marker a
	// delete made.
	stored *versionRecord
	marker *versionRecord
	// parts are parts of an upload that the entry removed: one that was
	// uploaded again, those a completion did not list, those of an upload
	// that was aborted. The proposer may remove their data.
	parts []partRecord
	// events describe what the entry changed; none when it was refused.
	events []ports.Event
	// fatal marks err as a failure to apply, as opposed to a refusal: the
	// entry is in the log but its effect is not in the state.
	fatal bool
}

// fsm is the Raft state machine. Its state lives in its own bbolt database,
// which is durable after every applied entry: the index of the last applied
// entry is written in the same transaction as the entry's effect, so an entry
// replayed after a restart is recognised and skipped.
type fsm struct {
	// root is the metadata directory; path is the state database inside it.
	root *os.Root
	path string

	// supported is the newest entry schema version this state machine can
	// apply.
	supported byte
	// publish receives the events of every entry that took effect, after
	// its transaction has committed. onFail is told when the state machine
	// stops. Both may be nil.
	publish func(ports.Event)
	onFail  func(error)
	// beforeApply, when set by a test (under mu), runs before every
	// command is applied; it stands in for a slow disk.
	beforeApply func()

	// seen is the index of the last command entry whose effect is in the
	// state. Reads on any node wait for it to reach the leader's.
	seen atomic.Uint64

	mu     sync.RWMutex
	db     *bbolt.DB
	failed error // set when the state machine can no longer make progress
}

// advance records that the state reflects the command entry at index.
func (f *fsm) advance(index uint64) {
	for {
		seen := f.seen.Load()
		if index <= seen || f.seen.CompareAndSwap(seen, index) {
			return
		}
	}
}

// loadSeen sets seen from what the state database records.
func (f *fsm) loadSeen() error {
	return f.db.View(func(tx *bbolt.Tx) error {
		f.seen.Store(readApplied(tx))
		return nil
	})
}

var _ raft.FSM = (*fsm)(nil)

func openFSM(root *os.Root) (*fsm, error) {
	path := filepath.Join(root.Name(), stateFile)
	db, err := openState(path)
	if err != nil {
		return nil, err
	}
	f := &fsm{root: root, path: path, db: db, supported: MetadataVersion}
	if err := f.loadSeen(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return f, nil
}

func openState(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, stateFileMode, &bbolt.Options{Timeout: lockTimeout})
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range append([][]byte{
			metaBucket(), bucketsBucket(), objectsBucket(), versionsBucket(), iamBucket(), clusterBucket(), nodesBucket(),
			uploadsBucket(), uploadKeysBucket(), partsBucket(), extentsBucket(),
		}, iamBuckets()...) {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if err := ensureDataIndex(tx); err != nil {
			return err
		}
		if err := ensureVersionIndex(tx); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket())
		switch raw := meta.Get(schemaKey()); {
		case raw == nil:
			return meta.Put(schemaKey(), binary.BigEndian.AppendUint64(nil, stateSchemaVersion))
		case len(raw) != 8 || binary.BigEndian.Uint64(raw) < stateSchemaVersion || binary.BigEndian.Uint64(raw) > stateSchema4:
			return fmt.Errorf("%w: metadata state schema", domain.ErrUnsupportedFormat)
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open state database: %w", err)
	}
	return db, nil
}

// ensureDataIndex creates the data-reference index when the state does not
// have one, from the object index it is derived from. A state written before
// the index existed is brought up to date here, deterministically.
func ensureDataIndex(tx *bbolt.Tx) error {
	if tx.Bucket(dataBucket()) != nil {
		return nil
	}
	index, err := tx.CreateBucket(dataBucket())
	if err != nil {
		return err
	}
	// Every version that has stored data counts, current or not: the
	// collector removes what this index does not know.
	for _, parent := range [][]byte{objectsBucket(), versionsBucket()} {
		holder := tx.Bucket(parent)
		err := holder.ForEachBucket(func(name []byte) error {
			return holder.Bucket(name).ForEach(func(_, raw []byte) error {
				var v versionRecord
				if err := json.Unmarshal(raw, &v); err != nil {
					return fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
				}
				if v.DeleteMarker {
					return nil
				}
				return addDataReference(index, v.DataID, 1)
			})
		})
		if err != nil {
			return err
		}
	}
	// A state from before the index has no uploads; one that has them is
	// counted all the same, so that this function is right whenever it runs.
	for _, parent := range [][]byte{partsBucket(), extentsBucket()} {
		holder := tx.Bucket(parent)
		if holder == nil {
			continue
		}
		err := holder.ForEachBucket(func(name []byte) error {
			return holder.Bucket(name).ForEach(func(_, raw []byte) error {
				p, err := decodePart(raw)
				if err != nil {
					return err
				}
				return addDataReference(index, p.DataID, 1)
			})
		})
		if err != nil {
			return err
		}
	}
	if holder := tx.Bucket(uploadsBucket()); holder != nil {
		return holder.ForEachBucket(func(name []byte) error {
			return holder.Bucket(name).ForEach(func(id, _ []byte) error {
				if len(id) != object.IDSize {
					return fmt.Errorf("%w: stored upload ID", domain.ErrIntegrity)
				}
				return addDataReference(index, object.ID(id).String(), 1)
			})
		})
	}
	return nil
}

// addDataReference changes the number of versions that refer to a data ID.
func addDataReference(index *bbolt.Bucket, dataID string, delta int) error {
	id, err := object.ParseID(dataID)
	if err != nil {
		return fmt.Errorf("%w: stored data ID: %w", domain.ErrIntegrity, err)
	}
	var count uint64
	if raw := index.Get(id[:]); len(raw) == 8 {
		count = binary.BigEndian.Uint64(raw)
	}
	switch {
	case delta > 0:
		count++
	case count > 0:
		count--
	}
	if count == 0 {
		return index.Delete(id[:])
	}
	return index.Put(id[:], binary.BigEndian.AppendUint64(nil, count))
}

func (f *fsm) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return nil
	}
	err := f.db.Close()
	f.db = nil
	return err
}

// health returns the error that stopped the state machine, if any.
func (f *fsm) health() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.failed
}

// view runs fn in a read transaction.
func (f *fsm) view(fn func(tx *bbolt.Tx) error) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.db == nil {
		return domain.ErrShuttingDown
	}
	return f.db.View(fn)
}

// appliedIndex returns the index of the last entry reflected in the state.
func (f *fsm) appliedIndex() (uint64, error) {
	var index uint64
	err := f.view(func(tx *bbolt.Tx) error {
		index = readApplied(tx)
		return nil
	})
	return index, err
}

func readApplied(tx *bbolt.Tx) uint64 {
	raw := tx.Bucket(metaBucket()).Get(appliedKey())
	if len(raw) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

// Apply implements raft.FSM. It returns an applyResult.
func (f *fsm) Apply(log *raft.Log) any {
	if log.Type != raft.LogCommand {
		return applyResult{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeApply != nil {
		f.beforeApply()
	}

	if f.failed != nil {
		return applyResult{err: f.failed, fatal: true}
	}
	if f.db == nil {
		return applyResult{err: domain.ErrShuttingDown, fatal: true}
	}

	var result applyResult
	err := f.db.Update(func(tx *bbolt.Tx) error {
		if log.Index <= readApplied(tx) {
			// Replayed after a restart: its effect is already in the state.
			return nil
		}
		e, err := decodeEntry(log.Data, f.supported)
		if err != nil {
			return err
		}
		if log.Data[0] < entrySchemaTimed {
			// An entry of the first schema says nothing about time or
			// locks, whatever is written in it: only the leader gives
			// an entry its time, and only an entry of the second schema
			// carries one.
			e.At = time.Time{}
			if e.PutObject != nil {
				// Nor what the first schema has no word for: a binary
				// of that schema would store the version without.
				e.PutObject.Lock, e.PutObject.Parts, e.PutObject.Dropped, e.PutObject.Tags = nil, 0, nil, nil
				enc := &e.PutObject.Encryption
				enc.KeyMode, enc.Mode, enc.Context, enc.KeyCheck = "", "", nil, nil
			}
			if e.CreateBucket != nil {
				e.CreateBucket.ObjectLock, e.CreateBucket.Versioning, e.CreateBucket.DefaultRetention = false, "", nil
				e.CreateBucket.Config = nil
			}
		}
		if log.Data[0] < entrySchema3 {
			// Nor does an entry of the second schema say who made a
			// version, or give an upload tags: a binary of that schema
			// would store both without. (The commands of the third
			// schema are not decoded from such an entry at all, and the
			// tag rules of a bucket are refused where they are applied,
			// as that binary refuses a kind it does not know.)
			if e.PutObject != nil {
				e.PutObject.CreatedBy = ""
			}
			if e.CompleteUpload != nil {
				e.CompleteUpload.CreatedBy = ""
			}
			if e.CreateUpload != nil {
				e.CreateUpload.Tags = nil
			}
		}
		if log.Data[0] < entrySchema4 && e.CreateBucket != nil {
			// Nor does an entry of the third schema say who created a
			// bucket: a binary of that schema would store it without.
			e.CreateBucket.CreatedBy = ""
		}
		now := applied{index: log.Index, schema: log.Data[0]}
		if !e.At.IsZero() {
			if now.at, err = stampState(tx, e.At); err != nil {
				return err
			}
		}
		switch {
		case log.Data[0] >= entrySchema4:
			// The same for the release of schema 3, which would stop at
			// this entry whatever becomes of it (ADR-0019).
			if err := raiseStateSchema(tx, stateSchema4); err != nil {
				return err
			}
		case log.Data[0] >= entrySchema3:
			// From here on the state may hold what the previous release
			// cannot express, and that release must refuse it.
			if err := raiseStateSchema(tx, stateSchema3); err != nil {
				return err
			}
		}
		result, err = applyEntry(tx, e, now)
		if err != nil {
			return err
		}
		return tx.Bucket(metaBucket()).Put(appliedKey(), binary.BigEndian.AppendUint64(nil, log.Index))
	})
	if err != nil {
		// The entry is committed in the log but cannot be applied here. Going
		// on would make this replica diverge, so the state machine stops.
		f.failed = fmt.Errorf("metadata state machine stopped at entry %d: %w", log.Index, err)
		if f.onFail != nil {
			f.onFail(f.failed)
		}
		return applyResult{err: f.failed, fatal: true}
	}
	f.advance(log.Index)
	// The change is durable in the state; now it is a fact that can be told.
	// An entry replayed after a restart produces no events: it changed
	// nothing this time.
	for i := range result.events {
		result.events[i].ID = fmt.Sprintf("%016x-%d", log.Index, i)
		result.events[i].Sequence = log.Index
		if f.publish != nil {
			f.publish(result.events[i])
		}
	}
	return result
}

// applyEntry performs one command inside tx. A returned error is fatal and
// rolls the transaction back; a refusal is reported in the result.
func applyEntry(tx *bbolt.Tx, e entry, now applied) (applyResult, error) {
	switch e.Type {
	case cmdCreateBucket:
		return applyCreateBucket(tx, *e.CreateBucket)
	case cmdDeleteBucket:
		return applyDeleteBucket(tx, e.DeleteBucket.Name)
	case cmdPutObject:
		return applyPutObject(tx, *e.PutObject, now)
	case cmdSetVersioning:
		return applySetVersioning(tx, *e.SetVersioning)
	case cmdDeleteVersion:
		return applyDeleteVersion(tx, *e.DeleteVersion, now)
	case cmdSetBucketLock:
		return applySetBucketLock(tx, *e.SetBucketLock)
	case cmdSetRetention:
		return applySetRetention(tx, *e.SetRetention, now)
	case cmdSetLegalHold:
		return applySetLegalHold(tx, *e.SetLegalHold, now)
	case cmdSetBucketConfig:
		return applySetBucketConfig(tx, *e.SetBucketConfig, now)
	case cmdSetTags:
		return applySetTags(tx, *e.SetTags, now)
	case cmdCreateUpload:
		return applyCreateUpload(tx, *e.CreateUpload)
	case cmdPutPart:
		return applyPutPart(tx, *e.PutPart)
	case cmdCompleteUpload:
		return applyCompleteUpload(tx, *e.CompleteUpload, now)
	case cmdAbortUpload:
		return applyAbortUpload(tx, *e.AbortUpload)
	case cmdDeleteObject:
		return applyDeleteObject(tx, *e.DeleteObject)
	case cmdSetRoot:
		return applySetRoot(tx, *e.SetRoot)
	case cmdSetTopology:
		return applySetTopology(tx, *e.SetTopology)
	case cmdSetNode:
		return applySetNode(tx, *e.SetNode)
	case cmdRemoveNode:
		return applyRemoveNode(tx, e.RemoveNode.ID)
	case cmdAddReads:
		return applyAddReads(tx, *e.AddReads, now)
	case cmdPutSubBucket:
		return audited(tx, e.PutSubBucket.Audit)(applyPutSubBucket(tx, *e.PutSubBucket))
	case cmdDeleteSubBucket:
		return audited(tx, e.DeleteSubBucket.Audit)(applyDeleteSubBucket(tx, *e.DeleteSubBucket))
	case cmdPutTagDefinition:
		return audited(tx, e.PutTagDefinition.Audit)(applyPutTagDefinition(tx, *e.PutTagDefinition))
	case cmdDeleteTagDefinition:
		return audited(tx, e.DeleteTagDefinition.Audit)(applyDeleteTagDefinition(tx, *e.DeleteTagDefinition))
	default:
		if isIAMCommand(e.Type) {
			return applyIAM(tx, e.Type, *e.IAM)
		}
		return applyResult{}, fmt.Errorf("%w: metadata command %q", domain.ErrUnsupportedFormat, e.Type)
	}
}

func refuse(err error, format string, args ...any) (applyResult, error) {
	return applyResult{err: fmt.Errorf("%w: %s", err, fmt.Sprintf(format, args...))}, nil
}

func applyCreateBucket(tx *bbolt.Tx, b bucketRecord) (applyResult, error) {
	buckets := tx.Bucket(bucketsBucket())
	if buckets.Get([]byte(b.Name)) != nil {
		return refuse(domain.ErrBucketAlreadyExists, "%s", b.Name)
	}
	// A bucket begins without versions, or - created with Object Lock -
	// keeping them, for good. Nothing else is said at creation.
	b.Versioning, b.DefaultRetention = "", nil
	// Who created it is kept as it was said, when it can be: a name is
	// judged by its bytes alone, as every build judges it (and only an entry
	// of schema 4 still has one here).
	if err := bucket.ValidateCreator(b.CreatedBy); err != nil {
		return applyResult{err: err}, nil
	}
	if b.ObjectLock {
		b.Versioning = string(bucket.VersioningEnabled)
	}
	data, err := json.Marshal(b)
	if err != nil {
		return applyResult{}, err
	}
	if err := buckets.Put([]byte(b.Name), data); err != nil {
		return applyResult{}, err
	}
	if _, err := tx.Bucket(objectsBucket()).CreateBucketIfNotExists([]byte(b.Name)); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventBucketCreated, Time: b.CreatedAt, Data: ports.BucketEvent{Bucket: b.Name}},
	}}, nil
}

func applyDeleteBucket(tx *bbolt.Tx, name string) (applyResult, error) {
	buckets := tx.Bucket(bucketsBucket())
	if buckets.Get([]byte(name)) == nil {
		return refuse(domain.ErrBucketNotFound, "%s", name)
	}
	// A bucket is empty when it has no object and no version of one:
	// versions that are no longer current, and delete markers, count.
	for _, parent := range [][]byte{objectsBucket(), versionsBucket()} {
		holder := tx.Bucket(parent)
		if index := holder.Bucket([]byte(name)); index != nil {
			if first, _ := index.Cursor().First(); first != nil {
				return refuse(domain.ErrBucketNotEmpty, "%s", name)
			}
		}
	}
	// Uploads that were never completed go with the bucket: they are
	// not objects, and nothing else could ever remove them.
	if err := dropUploads(tx, name); err != nil {
		return applyResult{}, err
	}
	// Its sub-buckets are parts of it, and the records of who read its
	// versions are about versions that are gone: both go with it. A state
	// that never held either has neither of the two to look in.
	for _, parent := range [][]byte{objectsBucket(), versionsBucket(), versionIDsBucket(), subBucketsBucket(), accessBucket()} {
		holder := tx.Bucket(parent)
		if holder != nil && holder.Bucket([]byte(name)) != nil {
			if err := holder.DeleteBucket([]byte(name)); err != nil {
				return applyResult{}, err
			}
		}
	}
	// And what was known of its latest uploads and downloads: a bucket
	// created under the name later begins with nothing (ADR-0019).
	if err := dropBucketActivity(tx, name); err != nil {
		return applyResult{}, err
	}
	if err := buckets.Delete([]byte(name)); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{{Type: ports.EventBucketDeleted, Data: ports.BucketEvent{Bucket: name}}}}, nil
}

func objectEvent(v versionRecord) ports.ObjectEvent {
	// The version is named as clients name it: a consumer can fetch by it.
	return ports.ObjectEvent{Bucket: v.Bucket, Key: v.Key, VersionID: refOf(v), Size: v.Size, ETag: v.ETag}
}

func versionsBucket() []byte { return []byte("versions") }

func applySetRoot(tx *bbolt.Tx, root rootRecord) (applyResult, error) {
	bucket := tx.Bucket(iamBucket())
	if bucket.Get(rootKey()) != nil {
		return applyResult{err: domain.ErrAlreadyBootstrapped}, nil
	}
	data, err := json.Marshal(root)
	if err != nil {
		return applyResult{}, err
	}
	if err := bucket.Put(rootKey(), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventRootKeySet, Time: root.CreatedAt, Data: ports.RootCredentialEvent{AccessKeyID: root.AccessKeyID}},
	}}, nil
}

// applySetTopology replaces the topology when the change is based on the
// stored version, and refuses it otherwise.
func applySetTopology(tx *bbolt.Tx, next topologyRecord) (applyResult, error) {
	if err := next.toDomain().Validate(); err != nil {
		return applyResult{err: err}, nil
	}
	bucket := tx.Bucket(clusterBucket())
	var stored topologyRecord
	if raw := bucket.Get(topologyKey()); raw != nil {
		if err := json.Unmarshal(raw, &stored); err != nil {
			return applyResult{}, fmt.Errorf("%w: stored topology: %w", domain.ErrIntegrity, err)
		}
	}
	if next.Version != stored.Version {
		return refuse(domain.ErrConflict, "topology is at version %d, the change was based on %d", stored.Version, next.Version)
	}
	next.Version = stored.Version + 1
	data, err := json.Marshal(next)
	if err != nil {
		return applyResult{}, err
	}
	if err := bucket.Put(topologyKey(), data); err != nil {
		return applyResult{}, err
	}
	topology := next.toDomain()
	return applyResult{events: []ports.Event{{Type: ports.EventTopologyChanged, Data: ports.TopologyEvent{
		DeploymentID: next.DeploymentID, Version: next.Version, ErasureSets: len(topology.Sets), DrivesPerSet: topology.SetSize(),
	}}}}, nil
}

// applySetNode records a node. Recording what is recorded already changes
// nothing and tells nobody.
func applySetNode(tx *bbolt.Tx, n nodeRecord) (applyResult, error) {
	// Only what every release decides alike is checked here: which roles
	// exist is a property of the binary, and replicas of two releases must
	// not disagree about an entry. The proposer validates the rest.
	if err := cluster.ValidateNodeID(n.ID); err != nil {
		return applyResult{err: err}, nil
	}
	data, err := json.Marshal(n)
	if err != nil {
		return applyResult{}, err
	}
	nodes := tx.Bucket(nodesBucket())
	if bytes.Equal(nodes.Get([]byte(n.ID)), data) {
		return applyResult{}, nil
	}
	if err := nodes.Put([]byte(n.ID), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventNodeRegistered, Data: ports.NodeEvent{NodeID: n.ID, Roles: n.Roles}},
	}}, nil
}

func applyRemoveNode(tx *bbolt.Tx, id string) (applyResult, error) {
	// What the state remembers of the node's batches of reads goes with
	// it, whether the node had registered or not.
	if flushes := tx.Bucket(accessFlushBucket()); flushes != nil {
		if err := flushes.Delete([]byte(id)); err != nil {
			return applyResult{}, err
		}
	}
	nodes := tx.Bucket(nodesBucket())
	if nodes.Get([]byte(id)) == nil {
		return applyResult{}, nil
	}
	if err := nodes.Delete([]byte(id)); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{{Type: ports.EventNodeRemoved, Data: ports.NodeEvent{NodeID: id}}}}, nil
}

// readNodes returns every recorded node, ordered by ID.
func readNodes(tx *bbolt.Tx) ([]cluster.Node, error) {
	var nodes []cluster.Node
	err := tx.Bucket(nodesBucket()).ForEach(func(_, raw []byte) error {
		var record nodeRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("%w: stored node: %w", domain.ErrIntegrity, err)
		}
		nodes = append(nodes, record.toDomain())
		return nil
	})
	return nodes, err
}

// readTopology returns the stored topology. The error wraps
// domain.ErrNoTopology when there is none.
func readTopology(tx *bbolt.Tx) (cluster.Topology, error) {
	raw := tx.Bucket(clusterBucket()).Get(topologyKey())
	if raw == nil {
		return cluster.Topology{}, domain.ErrNoTopology
	}
	var record topologyRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return cluster.Topology{}, fmt.Errorf("%w: stored topology: %w", domain.ErrIntegrity, err)
	}
	return record.toDomain(), nil
}

// readVersion returns the stored version of key, or nil when there is none.
func readVersion(index *bbolt.Bucket, key string) (*versionRecord, error) {
	raw := index.Get([]byte(key))
	if raw == nil {
		return nil, nil
	}
	var v versionRecord
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
	}
	return &v, nil
}

// Snapshot implements raft.FSM. The read transaction is opened here, while
// Raft guarantees no Apply is running, so the snapshot is a consistent view
// even though Persist runs concurrently with later applies.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.db == nil {
		return nil, domain.ErrShuttingDown
	}
	tx, err := f.db.Begin(false)
	if err != nil {
		return nil, fmt.Errorf("raftmeta.Snapshot: %w", err)
	}
	return &snapshot{tx: tx}, nil
}

// snapshot is a consistent copy of the state database: the bbolt file as of
// one read transaction.
type snapshot struct {
	tx *bbolt.Tx
}

// Persist implements raft.FSMSnapshot.
func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := s.tx.WriteTo(sink); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("raftmeta.Persist: %w", err)
	}
	if err := sink.Close(); err != nil {
		return fmt.Errorf("raftmeta.Persist: %w", err)
	}
	return nil
}

// Release implements raft.FSMSnapshot.
func (s *snapshot) Release() {
	_ = s.tx.Rollback()
}

// Restore implements raft.FSM: it replaces the state with a snapshot taken on
// another replica. The new database is written beside the old one, flushed,
// verified and only then swapped in.
func (f *fsm) Restore(source io.ReadCloser) error {
	defer func() { _ = source.Close() }()
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.writeSnapshotFile(source); err != nil {
		return fmt.Errorf("raftmeta.Restore: %w", err)
	}
	// Open it first: a snapshot this binary cannot read must not replace a
	// state it can.
	candidate, err := openState(filepath.Join(f.root.Name(), restoreFile))
	if err != nil {
		_ = f.root.Remove(restoreFile)
		return fmt.Errorf("raftmeta.Restore: %w", err)
	}
	if err := candidate.Close(); err != nil {
		return fmt.Errorf("raftmeta.Restore: %w", err)
	}

	if f.db != nil {
		if err := f.db.Close(); err != nil {
			return fmt.Errorf("raftmeta.Restore: %w", err)
		}
		f.db = nil
	}
	if err := f.root.Rename(restoreFile, stateFile); err != nil {
		return fmt.Errorf("raftmeta.Restore: %w", err)
	}
	db, err := openState(f.path)
	if err != nil {
		f.failed = fmt.Errorf("metadata state machine stopped: %w", err)
		return fmt.Errorf("raftmeta.Restore: %w", err)
	}
	f.db = db
	f.failed = nil
	return f.loadSeen()
}

func (f *fsm) writeSnapshotFile(source io.Reader) error {
	file, err := f.root.OpenFile(restoreFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, stateFileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
