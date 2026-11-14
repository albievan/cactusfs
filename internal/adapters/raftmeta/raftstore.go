package raftmeta

import (
	"io"
	"sync"

	"github.com/hashicorp/raft"
)

// raftStore is what Raft keeps its log and its term and vote in: the
// BoltStore of raft-boltdb, which closes.
type raftStore interface {
	raft.LogStore
	raft.StableStore
	io.Closer
}

// gatedStore keeps Raft's log and stable store from being used after it has
// been closed.
//
// Raft's shutdown does not wait for every caller of the stores. The network
// transport answers a heartbeat on the goroutine of its connection, outside
// Raft's routines: a heartbeat that has passed Raft's check for shutdown may
// write the term after Shutdown has returned. Raft panics when that write
// fails, as it would against a closed bbolt database. So Close waits for the
// calls under way, and a call that comes later never reaches the database: it
// is parked for good. It neither fails, which would make Raft panic, nor
// reports a write that was not made, which would let Raft answer a leader on
// the strength of a term or vote it never kept. Only such a stray handler can
// call after Close - Raft's own routines have stopped, and the store's
// operations are refused once it is closed - so nothing waits for a parked
// call, and there is at most one for each connection open when the node
// stopped, as the transport's connections are closed with it.
//
// A parked call is a goroutine that never ends, and what it refers to is
// never freed: the stopped node's *raft.Raft, its state machine, the closed
// BoltStore and the buffers of the connection it came on stay in memory
// until the process ends. That is bounded by one call for each connection
// that had a heartbeat in flight when the node stopped, and a node is stopped
// once in a process's life outside tests.
type gatedStore struct {
	inner raftStore

	// mu is held for reading by every call and for writing by Close.
	mu     sync.RWMutex
	closed bool
	// park holds a call that came after Close; it never returns.
	park func()
}

var (
	_ raft.LogStore    = (*gatedStore)(nil)
	_ raft.StableStore = (*gatedStore)(nil)
)

// newGatedStore guards inner. park, when not nil, replaces parkForGood: the
// package's tests pass one that notes the call before it parks it, and it
// must never return either.
func newGatedStore(inner raftStore, park func()) *gatedStore {
	if park == nil {
		park = parkForGood
	}
	return &gatedStore{inner: inner, park: park}
}

// parkForGood blocks the calling goroutine for the rest of the process.
func parkForGood() {
	select {}
}

// enter lets a call in, or parks it for good when the store is closed. The
// call must run the function it returns when it is done.
func (g *gatedStore) enter() (leave func()) {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		g.park()
	}
	return g.mu.RUnlock
}

// Close waits for the calls under way and closes the store. Calls that come
// later are parked (see gatedStore).
func (g *gatedStore) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	return g.inner.Close()
}

// FirstIndex implements raft.LogStore.
func (g *gatedStore) FirstIndex() (uint64, error) {
	defer g.enter()()
	return g.inner.FirstIndex()
}

// LastIndex implements raft.LogStore.
func (g *gatedStore) LastIndex() (uint64, error) {
	defer g.enter()()
	return g.inner.LastIndex()
}

// GetLog implements raft.LogStore.
func (g *gatedStore) GetLog(index uint64, log *raft.Log) error {
	defer g.enter()()
	return g.inner.GetLog(index, log)
}

// StoreLog implements raft.LogStore.
func (g *gatedStore) StoreLog(log *raft.Log) error {
	defer g.enter()()
	return g.inner.StoreLog(log)
}

// StoreLogs implements raft.LogStore.
func (g *gatedStore) StoreLogs(logs []*raft.Log) error {
	defer g.enter()()
	return g.inner.StoreLogs(logs)
}

// DeleteRange implements raft.LogStore.
func (g *gatedStore) DeleteRange(lo, hi uint64) error {
	defer g.enter()()
	return g.inner.DeleteRange(lo, hi)
}

// Set implements raft.StableStore.
func (g *gatedStore) Set(key, val []byte) error {
	defer g.enter()()
	return g.inner.Set(key, val)
}

// Get implements raft.StableStore.
func (g *gatedStore) Get(key []byte) ([]byte, error) {
	defer g.enter()()
	return g.inner.Get(key)
}

// SetUint64 implements raft.StableStore.
func (g *gatedStore) SetUint64(key []byte, val uint64) error {
	defer g.enter()()
	return g.inner.SetUint64(key, val)
}

// GetUint64 implements raft.StableStore.
func (g *gatedStore) GetUint64(key []byte) (uint64, error) {
	defer g.enter()()
	return g.inner.GetUint64(key)
}
