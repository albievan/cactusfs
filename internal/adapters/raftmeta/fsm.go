package raftmeta

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
)

// Names inside the state database. The layout is part of the snapshot format.
//
//	meta     "applied" -> index of the last applied Raft entry (uint64, big endian)
//	         "schema"  -> state schema version (uint64, big endian)
//	buckets  <bucket name> -> bucketRecord (JSON)
//	objects  <bucket name> (nested bucket) -> <object key> -> versionRecord (JSON)
//	iam      "root" -> rootRecord (JSON)
const (
	stateSchemaVersion = 1
	stateFileMode      = 0o600
)

func metaBucket() []byte    { return []byte("meta") }
func bucketsBucket() []byte { return []byte("buckets") }
func objectsBucket() []byte { return []byte("objects") }
func iamBucket() []byte     { return []byte("iam") }
func appliedKey() []byte    { return []byte("applied") }
func schemaKey() []byte     { return []byte("schema") }
func rootKey() []byte       { return []byte("root") }

// applyResult is what the state machine returns for one entry. err is a
// domain error for a command that was validly applied but refused (the state
// is unchanged), or a fatal error that has stopped the state machine.
type applyResult struct {
	err      error
	previous *versionRecord
}

// fsm is the Raft state machine. Its state lives in its own bbolt database,
// which is durable after every applied entry: the index of the last applied
// entry is written in the same transaction as the entry's effect, so an entry
// replayed after a restart is recognised and skipped.
type fsm struct {
	// root is the metadata directory; path is the state database inside it.
	root *os.Root
	path string

	mu     sync.RWMutex
	db     *bbolt.DB
	failed error // set when the state machine can no longer make progress
}

var _ raft.FSM = (*fsm)(nil)

func openFSM(root *os.Root) (*fsm, error) {
	path := filepath.Join(root.Name(), stateFile)
	db, err := openState(path)
	if err != nil {
		return nil, err
	}
	return &fsm{root: root, path: path, db: db}, nil
}

func openState(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, stateFileMode, &bbolt.Options{Timeout: lockTimeout})
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{metaBucket(), bucketsBucket(), objectsBucket(), iamBucket()} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(metaBucket())
		switch raw := meta.Get(schemaKey()); {
		case raw == nil:
			return meta.Put(schemaKey(), binary.BigEndian.AppendUint64(nil, stateSchemaVersion))
		case len(raw) != 8 || binary.BigEndian.Uint64(raw) != stateSchemaVersion:
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

	if f.failed != nil {
		return applyResult{err: f.failed}
	}
	if f.db == nil {
		return applyResult{err: domain.ErrShuttingDown}
	}

	var result applyResult
	err := f.db.Update(func(tx *bbolt.Tx) error {
		if log.Index <= readApplied(tx) {
			// Replayed after a restart: its effect is already in the state.
			return nil
		}
		e, err := decodeEntry(log.Data)
		if err != nil {
			return err
		}
		result, err = applyEntry(tx, e)
		if err != nil {
			return err
		}
		return tx.Bucket(metaBucket()).Put(appliedKey(), binary.BigEndian.AppendUint64(nil, log.Index))
	})
	if err != nil {
		// The entry is committed in the log but cannot be applied here. Going
		// on would make this replica diverge, so the state machine stops.
		f.failed = fmt.Errorf("metadata state machine stopped at entry %d: %w", log.Index, err)
		return applyResult{err: f.failed}
	}
	return result
}

// applyEntry performs one command inside tx. A returned error is fatal and
// rolls the transaction back; a refusal is reported in the result.
func applyEntry(tx *bbolt.Tx, e entry) (applyResult, error) {
	switch e.Type {
	case cmdCreateBucket:
		return applyCreateBucket(tx, *e.CreateBucket)
	case cmdDeleteBucket:
		return applyDeleteBucket(tx, e.DeleteBucket.Name)
	case cmdPutObject:
		return applyPutObject(tx, *e.PutObject)
	case cmdDeleteObject:
		return applyDeleteObject(tx, *e.DeleteObject)
	case cmdSetRoot:
		return applySetRoot(tx, *e.SetRoot)
	default:
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
	return applyResult{}, nil
}

func applyDeleteBucket(tx *bbolt.Tx, name string) (applyResult, error) {
	buckets := tx.Bucket(bucketsBucket())
	if buckets.Get([]byte(name)) == nil {
		return refuse(domain.ErrBucketNotFound, "%s", name)
	}
	objects := tx.Bucket(objectsBucket())
	if index := objects.Bucket([]byte(name)); index != nil {
		if first, _ := index.Cursor().First(); first != nil {
			return refuse(domain.ErrBucketNotEmpty, "%s", name)
		}
		if err := objects.DeleteBucket([]byte(name)); err != nil {
			return applyResult{}, err
		}
	}
	if err := buckets.Delete([]byte(name)); err != nil {
		return applyResult{}, err
	}
	return applyResult{}, nil
}

func applyPutObject(tx *bbolt.Tx, v versionRecord) (applyResult, error) {
	index := tx.Bucket(objectsBucket()).Bucket([]byte(v.Bucket))
	if index == nil {
		return refuse(domain.ErrBucketNotFound, "%s", v.Bucket)
	}
	previous, err := readVersion(index, v.Key)
	if err != nil {
		return applyResult{}, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return applyResult{}, err
	}
	if err := index.Put([]byte(v.Key), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{previous: previous}, nil
}

func applyDeleteObject(tx *bbolt.Tx, cmd deleteObjectCmd) (applyResult, error) {
	index := tx.Bucket(objectsBucket()).Bucket([]byte(cmd.Bucket))
	if index == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	previous, err := readVersion(index, cmd.Key)
	if err != nil || previous == nil {
		return applyResult{}, err
	}
	if err := index.Delete([]byte(cmd.Key)); err != nil {
		return applyResult{}, err
	}
	return applyResult{previous: previous}, nil
}

func applySetRoot(tx *bbolt.Tx, root rootRecord) (applyResult, error) {
	bucket := tx.Bucket(iamBucket())
	if bucket.Get(rootKey()) != nil {
		return applyResult{err: domain.ErrAlreadyBootstrapped}, nil
	}
	data, err := json.Marshal(root)
	if err != nil {
		return applyResult{}, err
	}
	return applyResult{}, bucket.Put(rootKey(), data)
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
	return nil
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
