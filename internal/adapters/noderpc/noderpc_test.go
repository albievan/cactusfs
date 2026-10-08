package noderpc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// pki is a node CA for a test.
type pki struct {
	t         *testing.T
	authority *nodetls.Authority
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	authority, err := nodetls.NewAuthority(rand.Reader, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &pki{t: t, authority: authority}
}

func (p *pki) identity(nodeID string) *nodetls.Identity {
	p.t.Helper()
	certPEM, keyPEM, err := p.authority.Issue(nodeID, time.Now())
	if err != nil {
		p.t.Fatal(err)
	}
	identity, err := nodetls.New(certPEM, keyPEM, p.authority.CertPEM)
	if err != nil {
		p.t.Fatal(err)
	}
	return identity
}

// fakeMeta stands in for the metadata store of the node that is asked.
type fakeMeta struct {
	// block, when set, holds every proposal until the caller gives up.
	block bool

	mu       sync.Mutex
	proposed [][]byte
	joined   []string
	left     []string
	err      error
}

// Subject reads the test's entries: "register <node>" and "remove <node>"
// are about a node's record, anything else is not.
func (m *fakeMeta) Subject(entry []byte) (registers, removes string, err error) {
	if node, ok := strings.CutPrefix(string(entry), "register "); ok {
		return node, "", nil
	}
	if node, ok := strings.CutPrefix(string(entry), "remove "); ok {
		return "", node, nil
	}
	if string(entry) == "garbage" {
		return "", "", fmt.Errorf("%w: not an entry", domain.ErrUnsupportedFormat)
	}
	return "", "", nil
}

func (m *fakeMeta) ProposeLocal(ctx context.Context, entry []byte) ([]byte, error) {
	if m.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.proposed = append(m.proposed, entry)
	return append([]byte("result of "), entry...), nil
}

func (m *fakeMeta) ReadIndexLocal(context.Context) (uint64, error) { return 42, m.err }
func (m *fakeMeta) Term() (uint64, error)                          { return 7, m.err }

func (m *fakeMeta) Join(_ context.Context, nodeID, raftAddr string, voter bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.joined = append(m.joined, fmt.Sprintf("%s %s %v", nodeID, raftAddr, voter))
	return m.err
}

func (m *fakeMeta) Leave(_ context.Context, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.left = append(m.left, nodeID)
	return m.err
}

// served is a node RPC server on loopback and what stands behind it.
type served struct {
	address  string
	meta     *fakeMeta
	drives   map[string]ports.ShardStore
	inFlight map[object.ID]bool
	server   *noderpc.Server
}

func serve(t *testing.T, p *pki, nodeID string, drives ...ports.ShardStore) *served {
	t.Helper()
	s := &served{meta: &fakeMeta{}, drives: map[string]ports.ShardStore{}, inFlight: map[object.ID]bool{}}
	for _, drive := range drives {
		s.drives[drive.DriveID()] = drive
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.address = listener.Addr().String()
	s.server = noderpc.NewServer(noderpc.ServerConfig{
		TLS: p.identity(nodeID).Server(), PeerNodeID: nodetls.PeerNodeID,
		Authz: app.NewAuthorizer(slog.New(slog.DiscardHandler), porttest.NewClock(testNow), nil),
		Info: func(context.Context) (noderpc.NodeInfo, error) {
			info := noderpc.NodeInfo{
				Node: cluster.Node{
					ID: nodeID, RPCAddr: s.address, RaftAddr: nodetls.Address(nodeID, "raft:9003"),
					Roles:  cluster.Roles{cluster.RoleMeta, cluster.RoleStorage},
					Labels: cluster.Labels{Region: "r", Zone: "z", Rack: "k", Host: "h"}, MetadataVersion: 1,
				},
				Formed: true,
			}
			for id := range s.drives {
				info.Drives = append(info.Drives, cluster.DriveState{ID: id, Empty: true, DeploymentID: "dep"})
			}
			return info, nil
		},
		InFlight: func(id object.ID) bool { return s.inFlight[id] },
		Meta:     s.meta,
		Drive: func(id string) (ports.ShardStore, bool) {
			drive, ok := s.drives[id]
			return drive, ok
		},
		Clock:  porttest.NewClock(testNow),
		Logger: slog.New(slog.DiscardHandler),
	})
	go func() { _ = s.server.Serve(listener) }()
	t.Cleanup(func() { s.server.Stop(time.Second) })
	return s
}

// clientFor returns a client with the identity of nodeID that finds nodes at
// the given addresses.
func clientFor(t *testing.T, identity *nodetls.Identity, addresses map[string]string) *noderpc.Client {
	t.Helper()
	client := noderpc.NewClient(identity.Client, func(_ context.Context, nodeID string) (string, bool) {
		address, ok := addresses[nodeID]
		return address, ok
	}, porttest.NewClock(testNow))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestRemoteDriveHonoursTheContract: a drive on another node, reached over
// mutual TLS, behaves exactly like a drive of this node - with a real drive
// behind it, and with the in-memory one.
func TestRemoteDriveHonoursTheContract(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	backings := map[string]func(t *testing.T) ports.ShardStore{
		"disk": func(t *testing.T) ports.ShardStore {
			drive, err := disk.Open(t.Context(), t.TempDir(), rand.Reader, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = drive.Close() })
			return drive
		},
		"memory": func(*testing.T) ports.ShardStore { return porttest.NewMemShardStore("drive-m") },
	}
	for name, backing := range backings {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			porttest.ShardStoreContract(t, func(t *testing.T) ports.ShardStore {
				local := backing(t)
				node := serve(t, p, "storage-1", local)
				client := clientFor(t, p.identity("gateway-1"), map[string]string{"storage-1": node.address})
				return client.Drive("storage-1", local.DriveID())
			})
		})
	}
}

func shard(fill byte, index int) object.ShardID {
	var id object.ID
	for i := range id {
		id[i] = fill
	}
	return object.ShardID{DataID: id, Index: index}
}

// TestShardStreams: large shards cross in chunks and come back intact; a
// writer that is abandoned, or whose caller is cancelled, leaves nothing; a
// rewrite that is abandoned leaves the old shard.
func TestShardStreams(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := newPKI(t)
	local := porttest.NewMemShardStore("d1")
	node := serve(t, p, "storage-1", local)
	drive := clientFor(t, p.identity("gateway-1"), map[string]string{"storage-1": node.address}).Drive("storage-1", "d1")

	data := make([]byte, 5<<20+12345)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	writer, err := drive.Create(ctx, shard(1, 0))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// One write far larger than a message, then small ones.
	if n, err := writer.Write(data[:4<<20]); err != nil || n != 4<<20 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	for off := 4 << 20; off < len(data); off += 100_000 {
		if _, err := writer.Write(data[off:min(off+100_000, len(data))]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if _, ok := local.Bytes(shard(1, 0)); ok {
		t.Error("the shard is visible before its commit")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := writer.Abort(); err != nil {
		t.Errorf("Abort after Commit: %v", err)
	}
	if stored, _ := local.Bytes(shard(1, 0)); !bytes.Equal(stored, data) {
		t.Fatal("the shard on the other node differs from what was written")
	}

	reader, err := drive.Open(ctx, shard(1, 0))
	if err != nil || reader.Size() != int64(len(data)) {
		t.Fatalf("Open: size %d, %v", reader.Size(), err)
	}
	// A read larger than one message, and one across the end.
	got := make([]byte, len(data))
	if n, err := reader.ReadAt(got, 0); err != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("ReadAt of the whole shard: %d, %v", n, err)
	}
	tail := make([]byte, 1000)
	if n, err := reader.ReadAt(tail, int64(len(data))-10); n != 10 || err == nil || !bytes.Equal(tail[:10], data[len(data)-10:]) {
		t.Errorf("ReadAt across the end: %d, %v", n, err)
	}
	if _, err := reader.ReadAt(tail, -1); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("ReadAt at a negative offset = %v", err)
	}

	// Abandoned writers.
	abandoned, err := drive.Create(ctx, shard(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = abandoned.Write([]byte("partial"))
	if err := abandoned.Abort(); err != nil {
		t.Errorf("Abort: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	orphan, err := drive.Create(cancelled, shard(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = orphan.Write([]byte("partial"))
	cancel()
	rewrite, err := drive.Rewrite(ctx, shard(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rewrite.Write([]byte("replacement that never commits"))
	_ = rewrite.Abort()

	// Give the other node a moment to notice, then look.
	time.Sleep(200 * time.Millisecond)
	if ids := local.IDs(); len(ids) != 1 || ids[0] != shard(1, 0) {
		t.Errorf("shards on the other node = %v, want only the committed one", ids)
	}
	if stored, _ := local.Bytes(shard(1, 0)); !bytes.Equal(stored, data) {
		t.Error("an abandoned rewrite changed the shard")
	}
	if err := orphan.Commit(ctx); err == nil {
		t.Error("Commit of a writer whose caller was cancelled succeeded")
	}
	if _, err := writer.Write([]byte("x")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Write after Commit = %v", err)
	}
	if err := writer.Commit(ctx); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("second Commit = %v", err)
	}
}

// TestErrorsKeepTheirMeaningAcrossNodes: what a drive on another node reports
// arrives as the same domain error, and a node that cannot be reached is
// never mistaken for a shard that is missing or damaged.
func TestErrorsKeepTheirMeaningAcrossNodes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := newPKI(t)
	local := porttest.NewMemShardStore("d1")
	node := serve(t, p, "storage-1", local)
	addresses := map[string]string{"storage-1": node.address}
	client := clientFor(t, p.identity("gateway-1"), addresses)
	drive := client.Drive("storage-1", "d1")

	if _, err := drive.Open(ctx, shard(9, 0)); !errors.Is(err, domain.ErrShardNotFound) {
		t.Errorf("Open of a missing shard = %v, want %v", err, domain.ErrShardNotFound)
	}
	local.Put(shard(1, 0), []byte("bits"), testNow.Add(-90*time.Minute))
	local.Damage(shard(1, 0))
	reader, err := drive.Open(ctx, shard(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadAt(make([]byte, 4), 0); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("read of a damaged shard = %v, want %v", err, domain.ErrIntegrity)
	}
	if _, err := drive.Create(ctx, shard(1, 0)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Create of an existing shard = %v, want %v", err, domain.ErrInvalidArgument)
	}
	// An error the domain has no name for stays an error, of no class.
	local.FailOpen = errors.New("too many open files")
	_, err = drive.Open(ctx, shard(1, 0))
	if err == nil || errors.Is(err, domain.ErrShardNotFound) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrDriveOffline) {
		t.Errorf("an unclassified failure on the other node = %v", err)
	}
	local.FailOpen = nil
	local.FailWrite = errors.New("no space left")
	writer, err := drive.Create(ctx, shard(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := writer.Write(make([]byte, 100))
	if commitErr := writer.Commit(ctx); writeErr == nil && commitErr == nil {
		t.Error("a write that failed on the other node was reported as committed")
	}
	local.FailWrite = nil

	// The age of a shard is judged by the clock of the node that has it.
	var listed []ports.ShardInfo
	if err := drive.List(ctx, func(info ports.ShardInfo) error { listed = append(listed, info); return nil }); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || !listed[0].ModifiedAt.Equal(testNow.Add(-90*time.Minute)) {
		t.Errorf("List = %+v, want one shard that is 90 minutes old", listed)
	}
	stop := errors.New("stop")
	if err := drive.List(ctx, func(ports.ShardInfo) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("List with a failing visit = %v", err)
	}

	// A drive the node does not have, a node that is not there, a node
	// whose address nobody knows: offline, every time.
	offline := map[string]ports.ShardStore{
		"unknown drive":   client.Drive("storage-1", "no-such-drive"),
		"unknown address": client.Drive("storage-9", "d1"),
	}
	addresses["storage-2"] = "127.0.0.1:1"
	offline["node down"] = client.Drive("storage-2", "d1")
	for name, drive := range offline {
		checks := map[string]error{}
		_, checks["Open"] = drive.Open(ctx, shard(1, 0))
		_, checks["Create"] = drive.Create(ctx, shard(5, 0))
		_, checks["Rewrite"] = drive.Rewrite(ctx, shard(5, 0))
		checks["Delete"] = drive.Delete(ctx, shard(1, 0))
		checks["List"] = drive.List(ctx, func(ports.ShardInfo) error { return nil })
		for op, err := range checks {
			if !errors.Is(err, domain.ErrDriveOffline) || errors.Is(err, domain.ErrShardNotFound) || errors.Is(err, domain.ErrIntegrity) {
				t.Errorf("%s: %s = %v, want %v and nothing about the shard", name, op, err, domain.ErrDriveOffline)
			}
		}
	}

	// The node goes away under an open reader and an open writer.
	reader, err = client.Drive("storage-1", "d1").Open(ctx, shard(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	writer, err = drive.Create(ctx, shard(6, 0))
	if err != nil {
		t.Fatal(err)
	}
	node.server.Stop(100 * time.Millisecond)
	if _, err := reader.ReadAt(make([]byte, 4), 0); !errors.Is(err, domain.ErrDriveOffline) {
		t.Errorf("read after the node stopped = %v, want %v", err, domain.ErrDriveOffline)
	}
	_, _ = writer.Write(make([]byte, 10))
	if err := writer.Commit(ctx); !errors.Is(err, domain.ErrDriveOffline) {
		t.Errorf("commit after the node stopped = %v, want %v", err, domain.ErrDriveOffline)
	}
}

// TestOnlyNodesOfTheClusterAreServed: node RPC answers nobody without a
// certificate of the cluster's node CA, and nobody is taken for another node.
func TestOnlyNodesOfTheClusterAreServed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, foreign := newPKI(t), newPKI(t)
	node := serve(t, p, "meta-1", porttest.NewMemShardStore("d1"))
	addresses := map[string]string{"meta-1": node.address, "meta-2": node.address}

	// A node of another cluster: refused in the handshake.
	stranger := clientFor(t, foreign.identity("gateway-1"), addresses)
	if _, err := stranger.Info(ctx, "meta-1", node.address); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Info with a certificate of another CA = %v, want the node to be unreachable", err)
	}
	if _, err := stranger.Drive("meta-1", "d1").Open(ctx, shard(1, 0)); !errors.Is(err, domain.ErrDriveOffline) {
		t.Errorf("Open with a certificate of another CA = %v", err)
	}
	if _, err := stranger.Propose(ctx, "meta-1", []byte("entry")); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("Propose with a certificate of another CA = %v, want a definite failure", err)
	}
	if len(node.meta.proposed) != 0 {
		t.Fatal("an entry from a stranger reached the metadata store")
	}

	// The address answers, but as another node than the one meant.
	member := clientFor(t, p.identity("gateway-1"), addresses)
	if _, err := member.Info(ctx, "meta-2", node.address); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Info of meta-2 at the address of meta-1 = %v, want it refused", err)
	}
	if _, err := member.Term(ctx, "meta-2"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Term of meta-2 at the address of meta-1 = %v", err)
	}

	// A member is served, and described.
	info, err := member.Info(ctx, "meta-1", node.address)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Node.ID != "meta-1" || !info.Formed || info.Node.Labels.Zone != "z" || info.Node.MetadataVersion != 1 ||
		!info.Node.Roles.Has(cluster.RoleMeta) || info.Node.RaftAddr != "meta-1@raft:9003" ||
		len(info.Drives) != 1 || info.Drives[0].ID != "d1" || info.Drives[0].NodeID != "meta-1" || info.Drives[0].DeploymentID != "dep" {
		t.Errorf("Info = %+v", info)
	}

	// A node joins and leaves as itself only.
	if err := member.Join(ctx, "meta-1", "gateway-1", "gateway-1@g:9003", false); err != nil {
		t.Errorf("Join as itself: %v", err)
	}
	if err := member.Join(ctx, "meta-1", "meta-3", "meta-3@g:9003", true); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Join under another node's name = %v, want %v", err, domain.ErrAccessDenied)
	}
	// A node's record is its own too: nobody else may write or remove it,
	// through whatever entry. Everything else any node may propose.
	if _, err := member.Propose(ctx, "meta-1", []byte("register gateway-1")); err != nil {
		t.Errorf("a node registering itself: %v", err)
	}
	for _, entry := range []string{"register meta-1", "remove meta-1", "register gateway-2"} {
		if _, err := member.Propose(ctx, "meta-1", []byte(entry)); !errors.Is(err, domain.ErrAccessDenied) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("gateway-1 proposing %q = %v, want a definite %v", entry, err, domain.ErrAccessDenied)
		}
	}
	if _, err := member.Propose(ctx, "meta-1", []byte("remove gateway-1")); err != nil {
		t.Errorf("a node removing its own record: %v", err)
	}
	if _, err := member.Propose(ctx, "meta-1", []byte("garbage")); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("an entry that cannot be read = %v", err)
	}
	if got := len(node.meta.proposed); got != 2 {
		t.Errorf("%d entries reached the metadata store, want the two the node was entitled to", got)
	}
	if err := member.Leave(ctx, "meta-1", "meta-1"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Leave for another node = %v, want %v", err, domain.ErrAccessDenied)
	}
	if err := member.Leave(ctx, "meta-1", "gateway-1"); err != nil {
		t.Errorf("Leave as itself: %v", err)
	}
	if len(node.meta.joined) != 1 || node.meta.joined[0] != "gateway-1 gateway-1@g:9003 false" || len(node.meta.left) != 1 {
		t.Errorf("joined %v, left %v", node.meta.joined, node.meta.left)
	}
}

// switchAuthorizer records requests and can be told to refuse them all.
type switchAuthorizer struct {
	inner porttest.Authorizer
	deny  atomic.Bool
}

func (a *switchAuthorizer) Authorize(ctx context.Context, req iam.Request) error {
	if a.deny.Load() {
		return domain.ErrAccessDenied
	}
	return a.inner.Authorize(ctx, req)
}

// TestEveryCallPassesTheAuthorizer: the server asks the one authorizer about
// every call, as the node whose certificate the caller showed, and serves
// nothing it refuses.
func TestEveryCallPassesTheAuthorizer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := newPKI(t)
	authz := &switchAuthorizer{}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	meta := &fakeMeta{}
	server := noderpc.NewServer(noderpc.ServerConfig{
		TLS: p.identity("meta-1").Server(), PeerNodeID: nodetls.PeerNodeID, Authz: authz, Meta: meta,
		Info: func(context.Context) (noderpc.NodeInfo, error) {
			return noderpc.NodeInfo{Node: cluster.Node{ID: "meta-1"}}, nil
		},
		InFlight: func(object.ID) bool { return false },
		Drive:    func(string) (ports.ShardStore, bool) { return porttest.NewMemShardStore("d1"), true },
		Clock:    porttest.NewClock(testNow), Logger: slog.New(slog.DiscardHandler),
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(time.Second) })
	client := clientFor(t, p.identity("gateway-7"), map[string]string{"meta-1": listener.Addr().String()})

	if _, err := client.Term(ctx, "meta-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Drive("meta-1", "d1").Create(ctx, shard(1, 0)); err != nil {
		t.Fatal(err)
	}
	requests := authz.inner.Requests()
	if len(requests) != 2 {
		t.Fatalf("%d authorization requests for 2 calls", len(requests))
	}
	for _, req := range requests {
		if req.Principal.NodeID != "gateway-7" || req.Principal.AccessKeyID != "" || req.Principal.Root || req.Action != iam.ActionNodeRPC {
			t.Errorf("authorization request = %+v", req)
		}
	}

	authz.deny.Store(true)
	if _, err := client.Propose(ctx, "meta-1", []byte("x")); err == nil {
		t.Error("Propose succeeded although the authorizer refused")
	}
	if _, err := client.Drive("meta-1", "d1").Create(ctx, shard(2, 0)); err == nil {
		t.Error("Create succeeded although the authorizer refused")
	}
	if len(meta.proposed) != 0 {
		t.Error("a refused call reached the metadata store")
	}
}

// TestCallsToTheLeader: what the metadata store is told when it asks the
// leader. An answer is passed on as it is; a leader that cannot be reached is
// a definite failure; an answer that never came is an unknown outcome.
func TestCallsToTheLeader(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := newPKI(t)
	node := serve(t, p, "meta-1")
	addresses := map[string]string{"meta-1": node.address, "meta-2": "127.0.0.1:1"}
	client := clientFor(t, p.identity("gateway-1"), addresses)

	result, err := client.Propose(ctx, "meta-1", []byte("entry"))
	if err != nil || string(result) != "result of entry" {
		t.Errorf("Propose = %q, %v", result, err)
	}
	if index, err := client.ReadIndex(ctx, "meta-1"); err != nil || index != 42 {
		t.Errorf("ReadIndex = %d, %v", index, err)
	}
	if term, err := client.Term(ctx, "meta-1"); err != nil || term != 7 {
		t.Errorf("Term = %d, %v", term, err)
	}
	node.inFlight[shard(3, 0).DataID] = true
	if busy, err := client.InFlight(ctx, "meta-1", shard(3, 0).DataID); err != nil || !busy {
		t.Errorf("InFlight = %v, %v", busy, err)
	}
	if busy, err := client.InFlight(ctx, "meta-1", shard(4, 0).DataID); err != nil || busy {
		t.Errorf("InFlight of other data = %v, %v", busy, err)
	}

	// The node answers that it does not lead: definite.
	node.meta.err = fmt.Errorf("%w: raft state is Follower", domain.ErrNotLeader)
	if _, err := client.Propose(ctx, "meta-1", []byte("e")); !errors.Is(err, domain.ErrNotLeader) || errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("Propose to a follower = %v, want a definite %v", err, domain.ErrNotLeader)
	}
	if _, err := client.ReadIndex(ctx, "meta-1"); !errors.Is(err, domain.ErrNotLeader) {
		t.Errorf("ReadIndex on a follower = %v", err)
	}
	if err := client.Join(ctx, "meta-1", "gateway-1", "gateway-1@g:1", false); !errors.Is(err, domain.ErrNotLeader) {
		t.Errorf("Join on a follower = %v", err)
	}
	// The leader says the outcome is unknown: that is passed on.
	node.meta.err = fmt.Errorf("%w: leadership lost", domain.ErrOutcomeUnknown)
	if _, err := client.Propose(ctx, "meta-1", []byte("e")); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("Propose with an unknown outcome on the leader = %v", err)
	}
	node.meta.err = nil

	// Nobody is listening: definite, nothing was sent.
	for name, call := range map[string]func() error{
		"Propose":   func() error { _, err := client.Propose(ctx, "meta-2", []byte("e")); return err },
		"ReadIndex": func() error { _, err := client.ReadIndex(ctx, "meta-2"); return err },
		"Term":      func() error { _, err := client.Term(ctx, "meta-2"); return err },
		"InFlight":  func() error { _, err := client.InFlight(ctx, "meta-2", object.ID{}); return err },
		"Join":      func() error { return client.Join(ctx, "meta-2", "gateway-1", "gateway-1@g:1", false) },
		"Leave":     func() error { return client.Leave(ctx, "meta-2", "gateway-1") },
		"unknown":   func() error { _, err := client.Term(ctx, "meta-9"); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("%s to a node that is down = %v, want a definite %v", name, err, domain.ErrUnavailable)
		}
	}

	// The request arrived, and then the answer did not come.
	slow := serve(t, p, "meta-3")
	addresses["meta-3"] = slow.address
	if _, err := client.Term(ctx, "meta-3"); err != nil {
		t.Fatal(err)
	}
	// The leader has the request and is working on it when the caller's
	// patience ends: the entry may be in the log.
	slow.meta.block = true
	// Long enough that a busy machine has handed the request over before
	// the patience ends: a call that ends before that is a definite
	// failure, and not what is tested here.
	// And if the patience does end before the handover - the machine can
	// be that busy - the call says, rightly, that nothing happened; it is
	// made again, because that is not the case under test either.
	var proposeErr error
	for range 5 {
		short, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, proposeErr = client.Propose(short, "meta-3", []byte("late"))
		cancel()
		if errors.Is(proposeErr, domain.ErrOutcomeUnknown) || !errors.Is(proposeErr, domain.ErrUnavailable) {
			break
		}
	}
	if !errors.Is(proposeErr, domain.ErrOutcomeUnknown) {
		t.Errorf("Propose whose answer did not come = %v, want %v", proposeErr, domain.ErrOutcomeUnknown)
	}

	if err := client.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := client.Term(ctx, "meta-1"); !errors.Is(err, domain.ErrShuttingDown) {
		t.Errorf("call on a closed client = %v", err)
	}
}
