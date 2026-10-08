package raftmeta

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// testCluster is a Raft group of stores in one process. They talk Raft over
// loopback TCP and reach the leader through direct calls, so that a test can
// cut a node off or make a call fail at a chosen point.
type testCluster struct {
	t    *testing.T
	base string

	mu      sync.Mutex
	stores  map[string]*Store
	members map[string]Member
	events  map[string]*recorder
	// down nodes cannot be reached by the others through the leader client.
	down map[string]bool
	// afterPropose, when set, replaces the error of a forwarded proposal
	// after the leader has processed it.
	afterPropose func(error) error
	// deaf nodes do not answer when asked for their term, though Raft
	// itself still reaches them.
	deaf map[string]bool
	// newer names the nodes that run the binary of the next release.
	newer map[string]bool
}

type recorder struct {
	mu     sync.Mutex
	events []ports.Event
}

func (r *recorder) Publish(_ context.Context, e ports.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) all() []ports.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func newTestCluster(t *testing.T) *testCluster {
	t.Helper()
	return &testCluster{
		t: t, base: t.TempDir(), stores: map[string]*Store{}, members: map[string]Member{},
		events: map[string]*recorder{}, down: map[string]bool{}, newer: map[string]bool{}, deaf: map[string]bool{},
	}
}

// leaderClient reaches the other stores of the test cluster on behalf of one.
type leaderClient struct {
	cluster *testCluster
	from    string
}

func (c leaderClient) target(id string) (*Store, error) {
	c.cluster.mu.Lock()
	defer c.cluster.mu.Unlock()
	store := c.cluster.stores[id]
	if store == nil || c.cluster.down[id] || c.cluster.down[c.from] {
		return nil, fmt.Errorf("%w: node %s cannot be reached", domain.ErrUnavailable, id)
	}
	return store, nil
}

func (c leaderClient) Propose(ctx context.Context, leaderID string, data []byte) ([]byte, error) {
	store, err := c.target(leaderID)
	if err != nil {
		return nil, err
	}
	result, err := store.ProposeLocal(ctx, data)
	c.cluster.mu.Lock()
	after := c.cluster.afterPropose
	c.cluster.mu.Unlock()
	if after != nil {
		err = after(err)
	}
	return result, err
}

func (c leaderClient) ReadIndex(ctx context.Context, leaderID string) (uint64, error) {
	store, err := c.target(leaderID)
	if err != nil {
		return 0, err
	}
	return store.ReadIndexLocal(ctx)
}

func (c leaderClient) Term(_ context.Context, nodeID string) (uint64, error) {
	store, err := c.target(nodeID)
	if err != nil {
		return 0, err
	}
	c.cluster.mu.Lock()
	deaf := c.cluster.deaf[nodeID]
	c.cluster.mu.Unlock()
	if deaf {
		return 0, fmt.Errorf("%w: node %s does not answer", domain.ErrUnavailable, nodeID)
	}
	return store.Term()
}

func clusterTiming() Timing {
	return Timing{
		// Short enough for quick elections, long enough that a busy
		// machine is not mistaken for a lost leader.
		Heartbeat: time.Second, Election: time.Second, LeaderLease: time.Second,
		Commit: 5 * time.Millisecond, LeaderWait: 8 * time.Second, TrailingLogs: 8,
	}
}

// start opens the store of a node, creating the node when it is new.
func (c *testCluster) start(id string, voter bool) *Store {
	c.t.Helper()
	c.mu.Lock()
	member, known := c.members[id]
	c.mu.Unlock()

	address := "127.0.0.1:0"
	if known {
		address = member.RaftAddr
	}
	var listener net.Listener
	var err error
	// The address of a restarted node may linger for a moment.
	for attempt := 0; ; attempt++ {
		if listener, err = (&net.ListenConfig{}).Listen(c.t.Context(), "tcp", address); err == nil || attempt > 50 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		c.t.Fatalf("listen: %v", err)
	}
	if _, ok := c.events[id]; !ok {
		c.events[id] = &recorder{}
	}
	store, err := Open(c.t.Context(), Config{
		Dir: filepath.Join(c.base, id), Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader, Timing: clusterTiming(),
		NodeID: id, Events: c.events[id],
		Cluster: &Cluster{
			Listener: listener, Advertise: listener.Addr().String(), Leader: leaderClient{cluster: c, from: id},
			Dial: func(ctx context.Context, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			},
		},
	})
	if err != nil {
		c.t.Fatalf("Open %s: %v", id, err)
	}
	if c.newer[id] {
		asNextRelease(store)
	}
	c.mu.Lock()
	c.stores[id] = store
	c.members[id] = Member{ID: id, RaftAddr: listener.Addr().String(), Voter: voter}
	c.mu.Unlock()
	c.t.Cleanup(func() { _ = store.Close() })
	return store
}

// asNextRelease makes a store behave like the binary of a release that has
// a metadata version this binary does not have, in which creating a bucket
// is written differently.
func asNextRelease(s *Store) {
	const next = MetadataVersion + 1
	s.supported = next
	s.fsm.supported = next
	s.versionOf = func(e entry) byte {
		if e.Type == cmdCreateBucket {
			return next
		}
		return entrySchemaVersion
	}
}

func (c *testCluster) stop(id string) {
	c.t.Helper()
	c.mu.Lock()
	store := c.stores[id]
	delete(c.stores, id)
	c.mu.Unlock()
	if err := store.Close(); err != nil {
		c.t.Fatalf("Close %s: %v", id, err)
	}
}

// form starts voters and others and forms the group from them.
func (c *testCluster) form(voters, others []string) {
	c.t.Helper()
	for _, id := range voters {
		c.start(id, true)
	}
	for _, id := range others {
		c.start(id, false)
	}
	var members []Member
	for _, id := range slices.Concat(voters, others) {
		members = append(members, c.members[id])
	}
	// Every voter bootstraps with the same list; the others wait to hear
	// from the leader.
	for _, id := range voters {
		if err := c.stores[id].Bootstrap(members); err != nil {
			c.t.Fatalf("Bootstrap %s: %v", id, err)
		}
	}
	c.ready(slices.Concat(voters, others)...)
}

func (c *testCluster) ready(ids ...string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 30*time.Second)
	defer cancel()
	for _, id := range ids {
		if err := c.store(id).WaitReady(ctx); err != nil {
			c.t.Fatalf("node %s did not become ready: %v", id, err)
		}
	}
}

func (c *testCluster) store(id string) *Store {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stores[id]
}

// leader waits for a leader among the running stores and returns its ID.
func (c *testCluster) leader() string {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for id, store := range c.stores {
			if store.IsLeader() {
				c.mu.Unlock()
				return id
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatal("no leader was elected")
	return ""
}

func (c *testCluster) register(ids ...string) {
	c.t.Helper()
	for _, id := range ids {
		store := c.store(id)
		node := cluster.Node{
			ID: id, RPCAddr: id + ":9002", RaftAddr: c.members[id].RaftAddr, Roles: cluster.AllRoles(),
			MetadataVersion: int(store.supported),
		}
		if err := store.SetNode(c.t.Context(), node); err != nil {
			c.t.Fatalf("SetNode %s: %v", id, err)
		}
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEveryNodeOfAClusterHonoursTheContract: the whole metadata contract,
// asked of a node that neither leads nor votes. Every change it makes is
// forwarded to the leader and every read waits for the leader's index, so it
// must behave exactly like a store on its own.
func TestEveryNodeOfAClusterHonoursTheContract(t *testing.T) {
	t.Parallel()
	porttest.MetadataContract(t, func(t *testing.T) porttest.Metadata {
		c := newTestCluster(t)
		c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
		// Every member says which schema it applies, as nodes do when
		// they start: without that the cluster stays at the first.
		c.register("m1", "m2", "m3", "s4")
		// The node that is asked has to have seen the registrations
		// itself before it will write what needs the newer schema.
		eventually(t, "the node to see every member's version", func() bool {
			level, err := c.store("s4").entryVersion()
			return err == nil && level == MetadataVersion
		})
		return c.store("s4")
	})
}

// TestReadAfterWriteOnEveryNode: a change acknowledged on one node is seen by
// the very next read on any other, voter or not.
func TestReadAfterWriteOnEveryNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	c.form(nodes[:3], nodes[3:])

	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for i := range 60 {
		writer, reader := nodes[i%4], nodes[(i+1+i/4)%4]
		version := porttest.SampleVersion("bkt", fmt.Sprintf("k%d", i%3), byte(i))
		if _, err := c.store(writer).PutVersion(ctx, version); err != nil {
			t.Fatalf("PutVersion on %s: %v", writer, err)
		}
		got, err := c.store(reader).GetObject(ctx, "bkt", version.Key)
		if err != nil || got.VersionID != version.VersionID {
			t.Fatalf("write %d on %s, then read on %s: version %s, %v; want %s", i, writer, reader, got.VersionID, err, version.VersionID)
		}
		if referenced, err := c.store(reader).IsDataReferenced(ctx, version.DataID); err != nil || !referenced {
			t.Fatalf("write %d on %s: its data is not referenced on %s (%v)", i, writer, reader, err)
		}
	}

	// What the state machine answers travels back with the forwarded call:
	// the version an overwrite replaced, and a refusal as the same error.
	first := porttest.SampleVersion("bkt", "answer", 1)
	if _, err := c.store("s4").PutVersion(ctx, first); err != nil {
		t.Fatal(err)
	}
	replaced, err := c.store("s4").PutVersion(ctx, porttest.SampleVersion("bkt", "answer", 2))
	if err != nil || replaced.Removed == nil || replaced.Removed.DataID != first.DataID {
		t.Errorf("forwarded overwrite: replaced = %+v, %v; want the first version", replaced, err)
	}
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); !errors.Is(err, domain.ErrBucketAlreadyExists) ||
		errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("forwarded refusal = %v, want %v and a definite outcome", err, domain.ErrBucketAlreadyExists)
	}
}

// TestLeaderChange: the leader dies. The others elect a new one and go on;
// nothing acknowledged is lost; the old leader comes back as a follower and
// catches up.
func TestLeaderChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	acknowledged := map[string]string{}
	var written byte
	put := func(node string, i int) {
		t.Helper()
		written++
		version := porttest.SampleVersion("bkt", fmt.Sprintf("k%03d", i), written)
		if _, err := c.store(node).PutVersion(ctx, version); err != nil {
			t.Fatalf("PutVersion %d on %s: %v", i, node, err)
		}
		acknowledged[version.Key] = version.VersionID.String()
	}
	for i := range 10 {
		put("s4", i)
	}

	for round := range 3 {
		old := c.leader()
		c.stop(old)
		// The very next write, on a node that just lost its leader, waits
		// for the election instead of failing.
		for i := range 10 {
			put("s4", 100*(round+1)+i)
		}
		if now := c.leader(); now == old {
			t.Fatalf("round %d: the stopped node %s still leads", round, old)
		}
		c.start(old, true)
		c.ready(old)
		for key, want := range acknowledged {
			got, err := c.store(old).GetObject(ctx, "bkt", key)
			if err != nil || got.VersionID.String() != want {
				t.Fatalf("round %d: %s on the returned node: %s, %v; want %s", round, key, got.VersionID, err, want)
			}
		}
	}
}

// TestNoQuorumNoAnswer: without a majority of voters nothing can be written,
// and nothing is read either - a read could be stale, and a stale read
// breaks read-after-write. Requests fail as unavailable, in bounded time,
// and work again when the quorum is back.
func TestNoQuorumNoAnswer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}

	c.stop("m2")
	c.stop("m3")
	for _, id := range []string{"m1", "s4"} {
		begun := time.Now()
		if _, err := c.store(id).GetBucket(ctx, "bkt"); !errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("read on %s without a quorum = %v, want %v", id, err, domain.ErrUnavailable)
		}
		err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: "other"})
		if !errors.Is(err, domain.ErrUnavailable) && !errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("write on %s without a quorum = %v, want it to fail as unavailable or unknown", id, err)
		}
		if took := time.Since(begun); took > 20*time.Second {
			t.Errorf("requests on %s without a quorum took %s", id, took)
		}
		if err := c.store(id).Check(ctx); err == nil && id == "s4" {
			// s4 may still believe in the old leader for a moment; what
			// matters is that it answers nothing.
			continue
		}
	}
	// The local view keeps answering: it is for finding drives and nodes
	// while the group has no leader.
	if _, err := c.store("s4").Local().Nodes(ctx); err != nil {
		t.Errorf("local view without a quorum: %v", err)
	}

	c.start("m2", true)
	c.ready("m1", "m2", "s4")
	if _, err := c.store("s4").GetBucket(ctx, "bkt"); err != nil {
		t.Errorf("read after the quorum returned: %v", err)
	}
}

// TestJoinAndLeave: nodes are added to a running group, with and without a
// vote, and removed again.
func TestJoinAndLeave(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, nil)
	if err := c.store("m1").CreateBucket(ctx, bucket.Bucket{Name: "before"}); err != nil {
		t.Fatal(err)
	}

	// A node that is not the leader cannot change the membership.
	follower := "m1"
	if c.leader() == "m1" {
		follower = "m2"
	}
	if err := c.store(follower).AddMember(ctx, Member{ID: "x", RaftAddr: "127.0.0.1:1"}); !errors.Is(err, domain.ErrNotLeader) {
		t.Errorf("AddMember on a follower = %v, want %v", err, domain.ErrNotLeader)
	}
	if err := c.store(follower).RemoveMember(ctx, "m3"); !errors.Is(err, domain.ErrNotLeader) {
		t.Errorf("RemoveMember on a follower = %v, want %v", err, domain.ErrNotLeader)
	}
	if err := c.store(c.leader()).AddMember(ctx, Member{ID: "Not A Name", RaftAddr: "127.0.0.1:1"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("AddMember with a bad ID = %v, want %v", err, domain.ErrInvalidArgument)
	}

	// A new node knows of no group until the leader adds it.
	gateway := c.start("g4", false)
	if formed, err := gateway.Formed(); err != nil || formed {
		t.Fatalf("Formed on a new node = %v, %v; want false", formed, err)
	}
	if err := c.store(c.leader()).AddMember(ctx, c.members["g4"]); err != nil {
		t.Fatalf("AddMember g4: %v", err)
	}
	c.ready("g4")
	if formed, err := gateway.Formed(); err != nil || !formed {
		t.Errorf("Formed after joining = %v, %v; want true", formed, err)
	}
	if _, err := gateway.GetBucket(ctx, "before"); err != nil {
		t.Errorf("the joined node does not have the state from before it joined: %v", err)
	}
	if err := gateway.CreateBucket(ctx, bucket.Bucket{Name: "from-g4"}); err != nil {
		t.Errorf("write through the joined node: %v", err)
	}

	// Two more voters make five; then two voters can be lost.
	for _, id := range []string{"m5", "m6"} {
		c.start(id, true)
		if err := c.store(c.leader()).AddMember(ctx, c.members[id]); err != nil {
			t.Fatalf("AddMember %s: %v", id, err)
		}
		c.ready(id)
	}
	members, err := c.store("g4").Members()
	voters := 0
	for _, m := range members {
		if m.Voter {
			voters++
		}
	}
	if err != nil || len(members) != 6 || voters != 5 {
		t.Fatalf("Members = %+v, %v; want six with five voters", members, err)
	}
	c.stop("m1")
	c.stop("m2")
	if err := c.store("g4").CreateBucket(ctx, bucket.Bucket{Name: "with-three-of-five"}); err != nil {
		t.Fatalf("write with three of five voters: %v", err)
	}

	// The two dead voters are removed; the group is three again and can
	// lose one more.
	for _, id := range []string{"m1", "m2"} {
		if err := c.store(c.leader()).RemoveMember(ctx, id); err != nil {
			t.Fatalf("RemoveMember %s: %v", id, err)
		}
	}
	eventually(t, "the membership to shrink", func() bool {
		members, err := c.store("g4").Members()
		return err == nil && len(members) == 4
	})
	c.stop("m3")
	if err := c.store("g4").CreateBucket(ctx, bucket.Bucket{Name: "with-two-of-three"}); err != nil {
		t.Fatalf("write after the removal, with two of three voters: %v", err)
	}
}

// TestUnreachableLeader: what a node may conclude when its call to the leader
// fails. If the entry certainly was not logged, the node tries again and the
// caller never notices. If it may have been, the caller must be told so - it
// decides whether shards are kept.
func TestUnreachableLeader(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})

	// The answer is lost after the leader committed the entry.
	lost := errors.New("connection reset")
	c.mu.Lock()
	c.afterPropose = func(error) error { return fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, lost) }
	c.mu.Unlock()
	err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "maybe"})
	if !errors.Is(err, domain.ErrOutcomeUnknown) || !errors.Is(err, lost) {
		t.Fatalf("CreateBucket whose answer was lost = %v, want %v", err, domain.ErrOutcomeUnknown)
	}
	c.mu.Lock()
	c.afterPropose = nil
	c.mu.Unlock()
	if _, err := c.store("s4").GetBucket(ctx, "maybe"); err != nil {
		t.Errorf("the entry of unknown outcome did take effect, and must be visible: %v", err)
	}

	// The leader cannot be reached at all; it comes back before the wait is
	// over. Nothing was sent, so the node simply tries again.
	leader := c.leader()
	c.mu.Lock()
	c.down[leader] = true
	c.mu.Unlock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.mu.Lock()
		delete(c.down, leader)
		c.mu.Unlock()
	}()
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "patient"}); err != nil {
		t.Errorf("CreateBucket while the leader was briefly unreachable = %v, want success", err)
	}

	// It stays unreachable: the request fails as unavailable, definitely.
	c.mu.Lock()
	c.down["s4"] = true
	c.mu.Unlock()
	err = c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "never"})
	if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("CreateBucket with the leader unreachable = %v, want a definite %v", err, domain.ErrUnavailable)
	}
	if _, err := c.store("s4").GetBucket(ctx, "patient"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("read with the leader unreachable = %v, want %v", err, domain.ErrUnavailable)
	}
	c.mu.Lock()
	delete(c.down, "s4")
	c.mu.Unlock()
	if _, err := c.store("s4").GetBucket(ctx, "never"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("the bucket of the failed request: %v, want %v", err, domain.ErrBucketNotFound)
	}
}

// TestRollingUpgrade: binaries of release N and N+1 run side by side while a
// cluster is upgraded node by node. Release N+1 writes one command in a new
// way that release N cannot apply. No node may log such an entry while any
// member is still on N - its state machine would stop - and every node may
// once all are on N+1.
func TestRollingUpgrade(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	c.form(nodes[:3], nodes[3:])
	c.register(nodes...)
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "from-release-n"}); err != nil {
		t.Fatal(err)
	}

	for i, id := range nodes {
		// The node is stopped and comes back with the new binary.
		c.stop(id)
		c.mu.Lock()
		c.newer[id] = true
		c.mu.Unlock()
		c.start(id, id != "s4")
		c.ready(id)

		// Before it has registered its new version, and while older nodes
		// remain, it must not use the new format - whether it leads or
		// forwards to a leader of either release.
		name := fmt.Sprintf("during-upgrade-%d", i)
		err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: name})
		if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Fatalf("node %s on N+1, with members on N: CreateBucket = %v, want a definite %v", id, err, domain.ErrUnavailable)
		}
		c.register(id)
		// Everything that both releases understand keeps working, on every
		// node, throughout.
		for _, other := range nodes {
			version := porttest.SampleVersion("from-release-n", "k-"+other, byte(i))
			if _, err := c.store(other).PutVersion(ctx, version); err != nil {
				t.Fatalf("upgrade step %d: PutVersion on %s: %v", i, other, err)
			}
			if err := c.store(other).Check(ctx); err != nil {
				t.Fatalf("upgrade step %d: node %s is not healthy: %v", i, other, err)
			}
		}
		if i < len(nodes)-1 {
			if err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: name}); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("node %s registered, older members remain: CreateBucket = %v, want %v", id, err, domain.ErrUnavailable)
			}
		}
	}

	// All four are on N+1: the new format is in use, everywhere.
	for i, id := range nodes {
		name := fmt.Sprintf("after-upgrade-%d", i)
		if err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatalf("after the upgrade: CreateBucket on %s: %v", id, err)
		}
		for _, other := range nodes {
			if _, err := c.store(other).GetBucket(ctx, name); err != nil {
				t.Errorf("after the upgrade: bucket of %s on %s: %v", id, other, err)
			}
		}
	}

	// A leader refuses an entry newer than the cluster's level even when a
	// node that miscounted sends it one.
	if err := c.store("s4").RemoveNode(ctx, "m2"); err != nil {
		t.Fatal(err)
	}
	data, err := encodeEntry(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "smuggled"}}, MetadataVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store(c.leader()).ProposeLocal(ctx, data); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("ProposeLocal of an entry above the cluster's level = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
	if _, err := c.store("s4").GetBucket(ctx, "smuggled"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("the refused entry took effect: %v", err)
	}
}

// TestAnOlderNodeStopsRatherThanMisapply: if an entry of a newer format does
// reach a node that cannot apply it, that node's state machine stops and the
// node leaves the group; it never guesses. The rest of the cluster goes on.
func TestAnOlderNodeStopsRatherThanMisapply(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.newer["m1"], c.newer["m2"], c.newer["m3"] = true, true, true
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	// Only the newer nodes are registered, so the cluster's level is 2; the
	// old node s4 is a member the others do not know the version of... and
	// so the level stays 1 until s4 is taken out of the count.
	c.register("m1", "m2", "m3")
	if err := c.store("m1").CreateBucket(ctx, bucket.Bucket{Name: "held-back"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("CreateBucket with an unregistered member = %v, want %v", err, domain.ErrUnavailable)
	}

	// Force the situation the rules exist to prevent: s4 claims a version
	// it does not have.
	lying := cluster.Node{ID: "s4", RPCAddr: "s4:9002", Roles: cluster.AllRoles(), MetadataVersion: MetadataVersion + 1}
	if err := c.store("m1").SetNode(ctx, lying); err != nil {
		t.Fatal(err)
	}
	// m1 may need a moment to see the registration it just made.
	eventually(t, "the new format to be used", func() bool {
		return c.store("m1").CreateBucket(ctx, bucket.Bucket{Name: "too-new"}) == nil
	})
	eventually(t, "the old node to stop", func() bool {
		return errors.Is(c.store("s4").Check(ctx), domain.ErrUnsupportedFormat)
	})
	if _, err := c.store("s4").GetBucket(ctx, "too-new"); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("read on the stopped node = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "x"}); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("write on the stopped node = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := c.store(id).GetBucket(ctx, "too-new"); err != nil {
			t.Errorf("node %s: %v", id, err)
		}
	}
}

// TestEventsComeFromAppliedEntries: every node derives the same events, with
// the same IDs, from the entries it applies - once each, in order, only for
// changes that took effect, and not again when a node restarts.
func TestEventsComeFromAppliedEntries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	c.form(nodes[:3], nodes[3:])

	store := c.store("s4")
	version := porttest.SampleVersion("bkt", "k", 1)
	steps := []func() error{
		func() error {
			return store.CreateBucket(ctx, bucket.Bucket{Name: "bkt", CreatedAt: time.Unix(1000, 0)})
		},
		func() error { _, err := store.PutVersion(ctx, version); return err },
		func() error { _, err := store.DeleteObject(ctx, "bkt", "k", ports.DeleteOptions{}); return err },
		func() error { return store.DeleteBucket(ctx, "bkt") },
		func() error {
			return store.SetNode(ctx, cluster.Node{ID: "s4", RPCAddr: "a:1", Roles: cluster.Roles{cluster.RoleStorage}, MetadataVersion: 1})
		},
		func() error { return store.RemoveNode(ctx, "s4") },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// Refusals and changes that change nothing are not events.
	if err := store.DeleteBucket(ctx, "bkt"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Fatal(err)
	}
	if err := store.RemoveNode(ctx, "s4"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteObject(ctx, "bkt", "k", ports.DeleteOptions{}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Fatal(err)
	}

	want := []string{
		ports.EventBucketCreated, ports.EventObjectCreated, ports.EventObjectDeleted, ports.EventBucketDeleted,
		ports.EventNodeRegistered, ports.EventNodeRemoved,
	}
	var reference []ports.Event
	for _, id := range nodes {
		c.ready(id)
		recorded := c.events[id]
		eventually(t, "the events on "+id, func() bool { return len(recorded.all()) >= len(want) })
		events := recorded.all()
		var types []string
		for i, e := range events {
			types = append(types, e.Type)
			if i > 0 && e.Sequence <= events[i-1].Sequence {
				t.Errorf("node %s: events out of order: %d after %d", id, e.Sequence, events[i-1].Sequence)
			}
		}
		if !slices.Equal(types, want) {
			t.Fatalf("node %s: events %v, want %v", id, types, want)
		}
		if reference == nil {
			reference = events
			continue
		}
		for i := range events {
			if !reflect.DeepEqual(events[i], reference[i]) {
				t.Errorf("node %s: event %d is %+v, on the first node %+v", id, i, events[i], reference[i])
			}
		}
	}
	created, ok := reference[1].Data.(ports.ObjectEvent)
	if !ok || created.Key != "k" || created.VersionID != object.NullVersion || created.Size != version.Size {
		t.Errorf("object-created payload = %+v", reference[1].Data)
	}
	if !reference[0].Time.Equal(time.Unix(1000, 0)) {
		t.Errorf("bucket-created time = %v, want the creation time of the bucket", reference[0].Time)
	}

	// A restart replays the log; the state already has those entries, and
	// no event is published a second time.
	c.stop("m2")
	c.start("m2", true)
	c.ready("m2")
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "after-restart"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new event on m2", func() bool { return len(c.events["m2"].all()) > len(want) })
	if events := c.events["m2"].all(); len(events) != len(want)+1 || events[len(want)].Type != ports.EventBucketCreated {
		t.Errorf("after a restart m2 has %d events, want %d", len(events), len(want)+1)
	}
}

// TestANodeFarBehindIsSentTheSnapshot: a node was away for longer than the log
// reaches back. The leader sends it a snapshot, which replaces its state
// while the node is running; afterwards it has everything, and reads on it
// are as current as anywhere.
func TestANodeFarBehindIsSentTheSnapshot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	if err := c.store("m1").CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	c.stop("s4")

	versions := map[string]string{}
	for i := range 200 {
		version := porttest.SampleVersion("bkt", fmt.Sprintf("k%03d", i), byte(i%251))
		// On a busy machine the leader can change in the middle of two
		// hundred writes. Putting the same version again is harmless, so an
		// unknown outcome is simply tried again.
		for attempt := 0; ; attempt++ {
			_, err := c.store("m1").PutVersion(ctx, version)
			if err == nil {
				break
			}
			if attempt == 20 || (!errors.Is(err, domain.ErrOutcomeUnknown) && !errors.Is(err, domain.ErrUnavailable)) {
				t.Fatal(err)
			}
		}
		versions[version.Key] = version.VersionID.String()
	}
	// Every voter snapshots and truncates its log to a few entries.
	for _, id := range []string{"m1", "m2", "m3"} {
		if err := c.store(id).Snapshot(); err != nil {
			t.Fatalf("Snapshot on %s: %v", id, err)
		}
	}

	returned := c.start("s4", false)
	c.ready("s4")
	for key, want := range versions {
		got, err := returned.GetObject(ctx, "bkt", key)
		if err != nil || got.VersionID.String() != want {
			t.Fatalf("%s on the node that was sent the snapshot: %s, %v", key, got.VersionID, err)
		}
	}
	// And it keeps up from there.
	last := porttest.SampleVersion("bkt", "after", 9)
	if _, err := c.store("m2").PutVersion(ctx, last); err != nil {
		t.Fatal(err)
	}
	if got, err := returned.GetObject(ctx, "bkt", "after"); err != nil || got.VersionID != last.VersionID {
		t.Errorf("a write after the snapshot, read on that node: %v", err)
	}
	if applied, err := returned.AppliedIndex(); err != nil || applied < 200 {
		t.Errorf("AppliedIndex = %d, %v", applied, err)
	}
}

// TestNodeKeepsItsName: Raft and the drive topology know a node by the name
// of its first start.
func TestNodeKeepsItsName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func(id string) (*Store, error) {
		return Open(t.Context(), Config{
			Dir: dir, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader, NodeID: id,
			Timing: Timing{Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond, LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond},
		})
	}
	store, err := open("node-a")
	if err != nil || store.NodeID() != "node-a" {
		t.Fatalf("Open: %v", err)
	}
	if store.Leader() != "node-a" || !store.IsLeader() {
		t.Errorf("a node on its own leads its group: leader %q", store.Leader())
	}
	_ = store.Close()
	if _, err := open("node-b"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Open under another name = %v, want %v", err, domain.ErrInvalidArgument)
	}
	again, err := open("")
	if err != nil || again.NodeID() != "node-a" {
		t.Errorf("Open without a name: %v", err)
	}
	if again != nil {
		_ = again.Close()
	}
	if _, err := Open(t.Context(), Config{Dir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader, NodeID: "Bad Name"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Open with an invalid name = %v, want %v", err, domain.ErrInvalidArgument)
	}
}

// TestALeaderMustBeConfirmedByVoters: the read index is only as good as the
// proof that the node giving it still leads. That proof is a majority of the
// VOTERS saying they are in the leader's term. Nodes without a vote do not
// count, however many of them answer: a leader that has lost the other
// voters but still reaches storage nodes may have been replaced without
// knowing, and must answer no read - on itself or for anybody else.
func TestALeaderMustBeConfirmedByVoters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4", "s5"})
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	leader := c.leader()
	var voters []string
	for _, id := range []string{"m1", "m2", "m3"} {
		if id != leader {
			voters = append(voters, id)
		}
	}

	// One other voter confirms: a majority of three with the leader itself.
	c.mu.Lock()
	c.deaf[voters[0]] = true
	c.mu.Unlock()
	for _, id := range []string{leader, "s4", "s5"} {
		if _, err := c.store(id).GetBucket(ctx, "bkt"); err != nil {
			t.Fatalf("read on %s with one voter silent: %v", id, err)
		}
	}

	// No other voter confirms. Raft still replicates to everybody and both
	// storage nodes would answer - which proves nothing.
	c.mu.Lock()
	c.deaf[voters[1]] = true
	c.mu.Unlock()
	for _, id := range []string{leader, "s4", "s5"} {
		if _, err := c.store(id).GetBucket(ctx, "bkt"); !errors.Is(err, domain.ErrUnavailable) && !errors.Is(err, domain.ErrNotLeader) {
			t.Fatalf("read on %s with no voter confirming the leader = %v, want it refused", id, err)
		}
	}
	if _, err := c.store(leader).ReadIndexLocal(ctx); !errors.Is(err, domain.ErrNotLeader) {
		t.Errorf("ReadIndexLocal without confirmation = %v, want %v", err, domain.ErrNotLeader)
	}

	c.mu.Lock()
	c.deaf = map[string]bool{}
	c.mu.Unlock()
	if _, err := c.store("s5").GetBucket(ctx, "bkt"); err != nil {
		t.Errorf("read once the voters answer again: %v", err)
	}
}

// TestANewLeaderAnswersOnlyWhenItHasCaughtUp: a change is acknowledged when
// the leader has applied it; the other voters have it in their logs and may
// be far from having applied it. When one of them becomes leader, its first
// read must wait until it has: answering from its state as it is would hide
// changes that were acknowledged under the old leader.
func TestANewLeaderAnswersOnlyWhenItHasCaughtUp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, nil)
	first := c.leader()
	if err := c.store(first).CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	c.ready("m1", "m2", "m3")

	// The other two apply slowly from here on.
	for _, id := range []string{"m1", "m2", "m3"} {
		if id != first {
			slow := c.store(id).fsm
			slow.mu.Lock()
			slow.beforeApply = func() { time.Sleep(150 * time.Millisecond) }
			slow.mu.Unlock()
		}
	}
	var last string
	for i := range 30 {
		version := porttest.SampleVersion("bkt", fmt.Sprintf("k%02d", i), 7)
		if _, err := c.store(first).PutVersion(ctx, version); err != nil {
			t.Fatal(err)
		}
		last = version.Key
	}
	// All thirty are acknowledged. The leader goes; a slow node takes over
	// with most of them still to apply.
	c.stop(first)
	next := c.leader()
	if _, err := c.store(next).GetObject(ctx, "bkt", last); err != nil {
		t.Fatalf("the first read on the new leader did not see a change acknowledged under the old one: %v", err)
	}
	for i := range 30 {
		if _, err := c.store(next).GetObject(ctx, "bkt", fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatalf("k%02d on the new leader: %v", i, err)
		}
	}
}

// TestSubject: what an entry is about, for the node that must decide whether
// its proposer may propose it.
func TestSubject(t *testing.T) {
	t.Parallel()
	store := openStore(t, t.TempDir())
	encode := func(e entry) []byte {
		data, err := encodeEntry(e, entrySchemaVersion)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	tests := []struct {
		entry             entry
		registers, remove string
	}{
		{entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "n1"}}, "n1", ""},
		{entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: "n2"}}, "", "n2"},
		{entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b"}}, "", ""},
		{entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "b", Key: "k"}}, "", ""},
	}
	for _, tt := range tests {
		registers, removes, err := store.Subject(encode(tt.entry))
		if err != nil || registers != tt.registers || removes != tt.remove {
			t.Errorf("Subject(%s) = %q, %q, %v", tt.entry.Type, registers, removes, err)
		}
	}
	if _, _, err := store.Subject([]byte("not an entry")); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("Subject of garbage = %v", err)
	}
}

// TestIAMOnEveryNodeOfACluster: the whole IAM, audit and session contract,
// asked of a node that neither leads nor votes.
func TestIAMOnEveryNodeOfACluster(t *testing.T) {
	t.Parallel()
	porttest.IAMContract(t, func(t *testing.T) porttest.IAM {
		c := newTestCluster(t)
		c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
		return c.store("s4")
	})
}

// steppedClock is a clock a test moves by hand.
type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// TestTheLeaderStampsEntries: the time of an entry is the leader's, whoever
// proposed it; a member that has not said it applies schema 2 holds the
// cluster at schema 1, where entries carry no time.
func TestTheLeaderStampsEntries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	stateOf := func(id string) (uint64, time.Time) {
		var schema uint64
		var at time.Time
		if err := c.store(id).fsm.view(func(tx *bbolt.Tx) error {
			schema, at = binary.BigEndian.Uint64(tx.Bucket(metaBucket()).Get(schemaKey())), readTime(tx)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return schema, at
	}
	// Nobody has registered: every entry is of schema 1.
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "before"}); err != nil {
		t.Fatal(err)
	}
	if schema, at := stateOf(c.leader()); schema != stateSchemaVersion || !at.IsZero() {
		t.Fatalf("with unregistered members: state schema %d, time %v", schema, at)
	}
	c.register("m1", "m2", "m3", "s4")

	noon := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clocks := map[string]*steppedClock{}
	for _, id := range []string{"m1", "m2", "m3", "s4"} {
		clocks[id] = &steppedClock{now: noon.Add(-1000 * time.Hour)} // nobody's clock but the leader's counts
		c.store(id).clock = clocks[id]
	}
	leader := c.leader()
	clocks[leader].set(noon)
	eventually(t, "entries to be stamped", func() bool {
		if err := c.store("s4").SetNode(ctx, cluster.Node{ID: "s4", RPCAddr: "s4:9002", Roles: cluster.AllRoles(), MetadataVersion: MetadataVersion}); err != nil {
			return false
		}
		_, at := stateOf(leader)
		return at.Equal(noon)
	})
	// The leader's clock jumps back; time in the state does not.
	clocks[leader].set(noon.Add(-time.Hour))
	if err := c.store("s4").CreateBucket(ctx, bucket.Bucket{Name: "after-the-jump"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2", "m3", "s4"} {
		eventually(t, "node "+id+" to have the leader's time", func() bool {
			schema, at := stateOf(id)
			return schema == stateSchemaTimed && at.Equal(noon)
		})
	}
}

// TestVersionsWhenAMemberHasNotRegistered: once a bucket keeps versions, the
// cluster holds what only schema 2 expresses, and a member that has not said
// which schema it applies - one that was just added, or one whose record was
// removed - cannot take it back to schema 1: deletes still make markers and
// puts are still stamped.
func TestVersionsWhenAMemberHasNotRegistered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	c.register("m1", "m2", "m3", "s4")
	store := c.store("m1")
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "ver"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	first := porttest.SampleVersion("ver", "k", 1)
	if _, err := store.PutVersion(ctx, first); err != nil {
		t.Fatal(err)
	}
	// s4's record goes; s4 is still a member of the group.
	if err := store.RemoveNode(ctx, "s4"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "s4"} {
		marker := object.ID(bytes.Repeat([]byte{0x70 + id[1]}, object.IDSize))
		deleted, err := c.store(id).DeleteObject(ctx, "ver", "k", ports.DeleteOptions{MarkerID: marker, At: time.Now()})
		if err != nil || deleted.Marker == nil || deleted.Removed != nil {
			t.Fatalf("delete through %s with an unregistered member = %+v, %v; want a delete marker", id, deleted, err)
		}
	}
	if _, err := store.GetVersion(ctx, "ver", "k", first.Ref()); err == nil {
		// first was put while Enabled, so it has an ID of its own.
		t.Log("the version is still there")
	}
	second := porttest.SampleVersion("ver", "k", 3)
	put, err := c.store("s4").PutVersion(ctx, second)
	if err != nil || !put.Stored.Versioned || put.Removed != nil {
		t.Fatalf("put with an unregistered member = %+v, %v", put, err)
	}
	// Still stamped by the leader.
	var schema uint64
	var at time.Time
	if err := c.store(c.leader()).fsm.view(func(tx *bbolt.Tx) error {
		schema, at = binary.BigEndian.Uint64(tx.Bucket(metaBucket()).Get(schemaKey())), readTime(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if schema != stateSchemaTimed || time.Since(at) > time.Minute {
		t.Errorf("state schema %d, time of the newest entry %v: entries are no longer stamped", schema, at)
	}
	if got, err := store.GetVersion(ctx, "ver", "k", object.VersionRef{ID: first.VersionID}); err != nil || got.DataID != first.DataID {
		t.Errorf("the first version after two deletes and a put: %v", err)
	}
}
