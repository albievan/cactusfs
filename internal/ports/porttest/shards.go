package porttest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// MemShardStore is an in-memory ports.ShardStore.
type MemShardStore struct {
	id string

	mu        sync.Mutex
	shards    map[object.ShardID][]byte
	committed map[object.ShardID]time.Time
	damaged   map[object.ShardID]bool

	// Now tells the store what time it is when a shard is committed; the
	// real time when nil.
	Now func() time.Time

	// FailCreate, FailWrite, FailCommit and FailOpen, when set, are returned
	// by the corresponding operation.
	FailCreate error
	FailWrite  error
	FailCommit error
	FailOpen   error
	// FailDelete, when set, is returned by Delete.
	FailDelete error
}

// NewMemShardStore returns an empty store that reports the given drive ID.
func NewMemShardStore(driveID string) *MemShardStore {
	return &MemShardStore{id: driveID, shards: map[object.ShardID][]byte{}, committed: map[object.ShardID]time.Time{}, damaged: map[object.ShardID]bool{}}
}

// DriveID implements ports.ShardStore.
func (m *MemShardStore) DriveID() string { return m.id }

// Create implements ports.ShardStore.
func (m *MemShardStore) Create(_ context.Context, id object.ShardID) (ports.ShardWriter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailCreate != nil {
		return nil, m.FailCreate
	}
	if _, ok := m.shards[id]; ok {
		return nil, fmt.Errorf("%w: shard %s already exists", domain.ErrInvalidArgument, id)
	}
	return &memShardWriter{store: m, id: id}, nil
}

// Open implements ports.ShardStore.
func (m *MemShardStore) Open(_ context.Context, id object.ShardID) (ports.ShardReader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailOpen != nil {
		return nil, m.FailOpen
	}
	data, ok := m.shards[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrShardNotFound, id)
	}
	return &memShardReader{Reader: bytes.NewReader(data), size: int64(len(data)), damaged: m.damaged[id]}, nil
}

// Delete implements ports.ShardStore.
func (m *MemShardStore) Delete(_ context.Context, id object.ShardID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailDelete != nil {
		return m.FailDelete
	}
	delete(m.shards, id)
	delete(m.committed, id)
	delete(m.damaged, id)
	return nil
}

// List implements ports.ShardStore.
func (m *MemShardStore) List(ctx context.Context, visit func(ports.ShardInfo) error) error {
	m.mu.Lock()
	infos := make([]ports.ShardInfo, 0, len(m.shards))
	for id := range m.shards {
		infos = append(infos, ports.ShardInfo{ID: id, CommittedAt: m.committed[id]})
	}
	m.mu.Unlock()
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(info); err != nil {
			return err
		}
	}
	return nil
}

// Put stores a shard directly, as if it had been committed at the given
// time. Tests use it to plant shards.
func (m *MemShardStore) Put(id object.ShardID, data []byte, committedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shards[id] = bytes.Clone(data)
	m.committed[id] = committedAt
}

// IDs returns the identifiers of every committed shard.
func (m *MemShardStore) IDs() []object.ShardID {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]object.ShardID, 0, len(m.shards))
	for id := range m.shards {
		ids = append(ids, id)
	}
	return ids
}

// Bytes returns the stored bytes of a shard and whether it exists.
func (m *MemShardStore) Bytes(id object.ShardID) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.shards[id]
	return bytes.Clone(data), ok
}

// Corrupt replaces the stored bytes of a shard, simulating damage that the
// stream authentication must catch.
func (m *MemShardStore) Corrupt(id object.ShardID, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shards[id] = bytes.Clone(data)
}

type memShardWriter struct {
	store *MemShardStore
	id    object.ShardID
	buf   bytes.Buffer
	done  bool
}

// Write implements ports.ShardWriter.
func (w *memShardWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, fmt.Errorf("%w: write after commit or abort", domain.ErrInvalidArgument)
	}
	w.store.mu.Lock()
	fail := w.store.FailWrite
	w.store.mu.Unlock()
	if fail != nil {
		return 0, fail
	}
	return w.buf.Write(p)
}

// Commit implements ports.ShardWriter.
func (w *memShardWriter) Commit(context.Context) error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	if w.done {
		return fmt.Errorf("%w: commit after commit or abort", domain.ErrInvalidArgument)
	}
	if w.store.FailCommit != nil {
		return w.store.FailCommit
	}
	if _, ok := w.store.shards[w.id]; ok {
		return fmt.Errorf("%w: shard %s already exists", domain.ErrInvalidArgument, w.id)
	}
	w.done = true
	w.store.shards[w.id] = bytes.Clone(w.buf.Bytes())
	now := time.Now()
	if w.store.Now != nil {
		now = w.store.Now()
	}
	w.store.committed[w.id] = now
	return nil
}

// Abort implements ports.ShardWriter.
func (w *memShardWriter) Abort() error {
	w.done = true
	return nil
}

// Damage makes a shard fail verification when it is read, the way a drive
// reports a block whose checksum does not match. Corrupt, in contrast,
// changes the bytes without the store noticing.
func (m *MemShardStore) Damage(id object.ShardID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.damaged[id] = true
}

type memShardReader struct {
	*bytes.Reader
	size    int64
	damaged bool
}

// ReadAt implements ports.ShardReader.
func (r *memShardReader) ReadAt(p []byte, off int64) (int, error) {
	if r.damaged {
		return 0, fmt.Errorf("%w: block checksum mismatch", domain.ErrIntegrity)
	}
	return r.Reader.ReadAt(p, off)
}

// Size implements ports.ShardReader.
func (r *memShardReader) Size() int64 { return r.size }

// Close implements ports.ShardReader.
func (r *memShardReader) Close() error { return nil }

var _ io.ReaderAt = (*memShardReader)(nil)
