package raftmeta

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// shutdownWait bounds each wait of the tests below; none of them should
// take more than a moment.
const shutdownWait = 30 * time.Second

// TestAHeartbeatThatOutlivesRaftCannotReachTheClosedLog: Raft's transport
// answers a heartbeat on the goroutine of its connection, and Raft's
// shutdown does not wait for it. A heartbeat that has passed Raft's check for
// shutdown, and writes the term only once the store has closed, made Raft
// panic ("failed to save current term: database not open"). The heartbeat is
// held between that check and its write while the store closes: Close must
// return, the late write must neither reach the closed database nor fail, and
// the connection it came on must be closed.
func TestAHeartbeatThatOutlivesRaftCannotReachTheClosedLog(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Once the store is open, the first write of the term is the
	// heartbeat's: a node that is in no group yet starts no election (Raft
	// writes the term once while it starts). It is held where the heartbeat
	// is past Raft's check for shutdown and has not yet entered the gate.
	reached, release := make(chan struct{}), make(chan struct{})
	var armed, held atomic.Bool
	var late atomic.Int64
	store, err := Open(t.Context(), Config{
		Dir: dir, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader, Timing: clusterTiming(), NodeID: "m1",
		Cluster: &Cluster{
			Listener: listener, Advertise: listener.Addr().String(),
			Dial: func(ctx context.Context, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			},
		},
		seam: storeSeam{
			wrap: func(gate raftStore) raftStore {
				return &pausingStore{raftStore: gate, pause: func(op string, key []byte) {
					if armed.Load() && op == "SetUint64" && string(key) == "CurrentTerm" && held.CompareAndSwap(false, true) {
						close(reached)
						<-release
					}
				}}
			},
			park: countAndPark(&late),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// A test that fails while the heartbeat is held lets it go before the
	// store is closed (cleanups run last first), or it would wait for ever.
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	armed.Store(true)

	const term = 5
	leader, err := raft.NewTCPTransportWithLogger("127.0.0.1:0", nil, 1, time.Hour, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leader.Close() })
	answered := make(chan error, 1)
	go func() {
		heartbeat := &raft.AppendEntriesRequest{
			RPCHeader: raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax, ID: []byte("leader"), Addr: []byte(leader.LocalAddr())},
			Term:      term,
		}
		var resp raft.AppendEntriesResponse
		answered <- leader.AppendEntries("m1", raft.ServerAddress(listener.Addr().String()), heartbeat, &resp)
	}()
	select {
	case <-reached:
	case <-time.After(shutdownWait):
		t.Fatal("the heartbeat did not reach the store")
	}

	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(shutdownWait):
		t.Fatal("Close did not return while a heartbeat was under way")
	}
	// The node has stopped: the connection the heartbeat came on is closed,
	// so its leader is not left waiting for an answer.
	select {
	case err := <-answered:
		if err == nil {
			t.Fatal("the heartbeat was answered by a node that has stopped")
		}
	case <-time.After(shutdownWait):
		t.Fatal("the connection of the heartbeat was left open")
	}

	// Without the gate the write meets the closed database and Raft panics.
	letGo()
	deadline := time.Now().Add(shutdownWait)
	for late.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the late write of the term was not parked")
		}
		time.Sleep(time.Millisecond)
	}

	// The write that came after Close did not happen.
	logs, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, raftLogFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	got, err := logs.GetUint64([]byte("CurrentTerm"))
	if err != nil && !errors.Is(err, raftboltdb.ErrKeyNotFound) {
		t.Fatal(err)
	}
	if got == term {
		t.Fatalf("term on disk = %d: the write after Close was made", got)
	}
}

// fakeRaftStore counts the calls that reach it; a call can be held inside it.
type fakeRaftStore struct {
	calls  atomic.Int64
	closed atomic.Bool
	// hold, when not nil, holds every call until it is closed; entered is
	// closed when the first call has come in.
	hold    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (f *fakeRaftStore) call() {
	f.calls.Add(1)
	if f.hold != nil {
		f.once.Do(func() { close(f.entered) })
		<-f.hold
	}
}

func (f *fakeRaftStore) FirstIndex() (uint64, error)      { f.call(); return 0, nil }
func (f *fakeRaftStore) LastIndex() (uint64, error)       { f.call(); return 0, nil }
func (f *fakeRaftStore) GetLog(uint64, *raft.Log) error   { f.call(); return nil }
func (f *fakeRaftStore) StoreLog(*raft.Log) error         { f.call(); return nil }
func (f *fakeRaftStore) StoreLogs([]*raft.Log) error      { f.call(); return nil }
func (f *fakeRaftStore) DeleteRange(uint64, uint64) error { f.call(); return nil }
func (f *fakeRaftStore) Set([]byte, []byte) error         { f.call(); return nil }
func (f *fakeRaftStore) Get([]byte) ([]byte, error)       { f.call(); return nil, nil }
func (f *fakeRaftStore) SetUint64([]byte, uint64) error   { f.call(); return nil }
func (f *fakeRaftStore) GetUint64([]byte) (uint64, error) { f.call(); return 0, nil }
func (f *fakeRaftStore) Close() error                     { f.closed.Store(true); return nil }

// countAndPark parks a late call for good, as the gate does, once it has
// counted it in late.
func countAndPark(late *atomic.Int64) func() {
	return func() {
		late.Add(1)
		parkForGood()
	}
}

// pausingStore is what Raft is given in place of the gate in a test: it
// calls pause at the start of every call, before the call enters the gate,
// to hold a caller where a stray heartbeat may be when the store closes.
type pausingStore struct {
	raftStore
	pause func(op string, key []byte)
}

func (p *pausingStore) FirstIndex() (uint64, error) {
	p.pause("FirstIndex", nil)
	return p.raftStore.FirstIndex()
}

func (p *pausingStore) LastIndex() (uint64, error) {
	p.pause("LastIndex", nil)
	return p.raftStore.LastIndex()
}

func (p *pausingStore) GetLog(index uint64, log *raft.Log) error {
	p.pause("GetLog", nil)
	return p.raftStore.GetLog(index, log)
}

func (p *pausingStore) StoreLog(log *raft.Log) error {
	p.pause("StoreLog", nil)
	return p.raftStore.StoreLog(log)
}

func (p *pausingStore) StoreLogs(logs []*raft.Log) error {
	p.pause("StoreLogs", nil)
	return p.raftStore.StoreLogs(logs)
}

func (p *pausingStore) DeleteRange(lo, hi uint64) error {
	p.pause("DeleteRange", nil)
	return p.raftStore.DeleteRange(lo, hi)
}

func (p *pausingStore) Set(key, val []byte) error {
	p.pause("Set", key)
	return p.raftStore.Set(key, val)
}

func (p *pausingStore) Get(key []byte) ([]byte, error) {
	p.pause("Get", key)
	return p.raftStore.Get(key)
}

func (p *pausingStore) SetUint64(key []byte, val uint64) error {
	p.pause("SetUint64", key)
	return p.raftStore.SetUint64(key, val)
}

func (p *pausingStore) GetUint64(key []byte) (uint64, error) {
	p.pause("GetUint64", key)
	return p.raftStore.GetUint64(key)
}

// gatedCalls is one call of each method of the gated store.
func gatedCalls() []struct {
	name string
	call func(g *gatedStore) error
} {
	return []struct {
		name string
		call func(g *gatedStore) error
	}{
		{"FirstIndex", func(g *gatedStore) error { _, err := g.FirstIndex(); return err }},
		{"LastIndex", func(g *gatedStore) error { _, err := g.LastIndex(); return err }},
		{"GetLog", func(g *gatedStore) error { return g.GetLog(1, &raft.Log{}) }},
		{"StoreLog", func(g *gatedStore) error { return g.StoreLog(&raft.Log{Index: 1}) }},
		{"StoreLogs", func(g *gatedStore) error { return g.StoreLogs([]*raft.Log{{Index: 1}}) }},
		{"DeleteRange", func(g *gatedStore) error { return g.DeleteRange(1, 2) }},
		{"Set", func(g *gatedStore) error { return g.Set([]byte("k"), []byte("v")) }},
		{"Get", func(g *gatedStore) error { _, err := g.Get([]byte("k")); return err }},
		{"SetUint64", func(g *gatedStore) error { return g.SetUint64([]byte("k"), 1) }},
		{"GetUint64", func(g *gatedStore) error { _, err := g.GetUint64([]byte("k")); return err }},
	}
}

// TestTheGateParksCallsAfterClose: every method of Raft's stores, called
// after Close, neither reaches the closed store nor returns (an error would
// make Raft panic, a success would claim a write that was not made).
func TestTheGateParksCallsAfterClose(t *testing.T) {
	t.Parallel()
	for _, tc := range gatedCalls() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &fakeRaftStore{}
			var late atomic.Int64
			gate := newGatedStore(inner, countAndPark(&late))
			if err := tc.call(gate); err != nil || inner.calls.Load() != 1 {
				t.Fatalf("open: err %v, %d calls; want the call made", err, inner.calls.Load())
			}
			if err := gate.Close(); err != nil || !inner.closed.Load() {
				t.Fatalf("Close: %v, closed %v", err, inner.closed.Load())
			}
			returned := make(chan error, 1)
			go func() { returned <- tc.call(gate) }()
			deadline := time.Now().Add(shutdownWait)
			for late.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the call after Close was not parked")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-returned:
				t.Fatalf("a call after Close returned %v", err)
			default:
			}
			if got := inner.calls.Load(); got != 1 {
				t.Fatalf("%d calls reached the store; want only the one before Close", got)
			}
			if err := gate.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
		})
	}
}

// TestTheGateClosesAfterTheCallsUnderWay: a call that is in the store when
// Close is called finishes against the open store, and the store is closed
// only after it.
func TestTheGateClosesAfterTheCallsUnderWay(t *testing.T) {
	t.Parallel()
	for _, tc := range gatedCalls() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &fakeRaftStore{hold: make(chan struct{}), entered: make(chan struct{})}
			gate := newGatedStore(inner, nil)
			returned := make(chan error, 1)
			go func() { returned <- tc.call(gate) }()
			<-inner.entered
			closed := make(chan error, 1)
			go func() { closed <- gate.Close() }()
			select {
			case <-closed:
				t.Fatal("Close returned while a call was in the store")
			case <-time.After(10 * time.Millisecond):
			}
			if inner.closed.Load() {
				t.Fatal("the store was closed under a call")
			}
			close(inner.hold)
			if err := <-returned; err != nil {
				t.Fatalf("the call under way: %v", err)
			}
			if err := <-closed; err != nil || !inner.closed.Load() {
				t.Fatalf("Close: %v, closed %v", err, inner.closed.Load())
			}
		})
	}
}

// TestClosingTheStreamLayerClosesWhatItAccepted: Raft's transport closes
// only its listener when it closes and goes on reading the connections it
// has accepted; the stream layer closes them, so a stopped node takes no
// more commands.
func TestClosingTheStreamLayerClosesWhatItAccepted(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	layer := newStreamLayer(listener, nil, raft.ServerAddress(listener.Addr().String()))
	t.Cleanup(func() { _ = layer.Close() })

	const peers = 3
	clients := make([]net.Conn, 0, peers)
	var first net.Conn
	for i := range peers {
		client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		clients = append(clients, client)
		accepted, err := layer.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = accepted
		}
	}
	// One the transport closed itself is forgotten.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	layer.mu.Lock()
	open := len(layer.open)
	layer.mu.Unlock()
	if open != peers-1 {
		t.Fatalf("%d connections tracked; want %d", open, peers-1)
	}
	if err := layer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, client := range clients[1:] {
		if err := client.SetReadDeadline(time.Now().Add(shutdownWait)); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("peer %d reads %v after the layer closed; want EOF", i+1, err)
		}
	}
	if _, err := layer.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close: %v; want net.ErrClosed", err)
	}
}
