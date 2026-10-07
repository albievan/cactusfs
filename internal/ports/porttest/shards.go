package porttest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// MemShardStore is an in-memory ports.ShardStore.
type MemShardStore struct {
	id string

	mu     sync.Mutex
	shards map[object.ShardID][]byte

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
	return &MemShardStore{id: driveID, shards: map[object.ShardID][]byte{}}
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
	return &memShardReader{Reader: bytes.NewReader(data), size: int64(len(data))}, nil
}

// Delete implements ports.ShardStore.
func (m *MemShardStore) Delete(_ context.Context, id object.ShardID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailDelete != nil {
		return m.FailDelete
	}
	delete(m.shards, id)
	return nil
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
	return nil
}

// Abort implements ports.ShardWriter.
func (w *memShardWriter) Abort() error {
	w.done = true
	return nil
}

type memShardReader struct {
	*bytes.Reader
	size int64
}

// Size implements ports.ShardReader.
func (r *memShardReader) Size() int64 { return r.size }

// Close implements ports.ShardReader.
func (r *memShardReader) Close() error { return nil }

var _ io.ReaderAt = (*memShardReader)(nil)
