package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// nodeDrive is a drive on another node: when its node is down, nothing can
// be learnt about it.
type nodeDrive struct {
	ports.ShardStore
	down *atomic.Bool
}

func (d nodeDrive) offline() error {
	return fmt.Errorf("%w: drive %s: %w: node cannot be reached", domain.ErrDriveOffline, d.DriveID(), domain.ErrUnavailable)
}

func (d nodeDrive) Create(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	if d.down.Load() {
		return nil, d.offline()
	}
	writer, err := d.ShardStore.Create(ctx, id)
	return nodeWriter{ShardWriter: writer, drive: d}, err
}

func (d nodeDrive) Rewrite(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	if d.down.Load() {
		return nil, d.offline()
	}
	writer, err := d.ShardStore.Rewrite(ctx, id)
	return nodeWriter{ShardWriter: writer, drive: d}, err
}

// nodeWriter is a shard being written on another node: when the node goes
// down, the write fails and the shard is never committed.
type nodeWriter struct {
	ports.ShardWriter
	drive nodeDrive
}

func (w nodeWriter) Write(p []byte) (int, error) {
	if w.drive.down.Load() {
		return 0, w.drive.offline()
	}
	return w.ShardWriter.Write(p)
}

func (w nodeWriter) Commit(ctx context.Context) error {
	if w.drive.down.Load() {
		return w.drive.offline()
	}
	return w.ShardWriter.Commit(ctx)
}

func (d nodeDrive) Open(ctx context.Context, id object.ShardID) (ports.ShardReader, error) {
	if d.down.Load() {
		return nil, d.offline()
	}
	reader, err := d.ShardStore.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return nodeReader{ShardReader: reader, drive: d}, nil
}

func (d nodeDrive) Delete(ctx context.Context, id object.ShardID) error {
	if d.down.Load() {
		return d.offline()
	}
	return d.ShardStore.Delete(ctx, id)
}

type nodeReader struct {
	ports.ShardReader
	drive nodeDrive
}

func (r nodeReader) ReadAt(p []byte, off int64) (int, error) {
	if r.drive.down.Load() {
		return 0, r.drive.offline()
	}
	return r.ShardReader.ReadAt(p, off)
}

// clusterFixture is the six-drive fixture with every drive on a node of its
// own, any of which can be taken down.
type clusterFixture struct {
	*fixture
	down    []*atomic.Bool
	service *app.ObjectService
}

func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	c := &clusterFixture{fixture: erasureFixture(t)}
	stores := make([]ports.ShardStore, len(c.drives))
	for i, drive := range c.drives {
		c.down = append(c.down, &atomic.Bool{})
		stores[i] = nodeDrive{ShardStore: drive, down: c.down[i]}
	}
	service, err := app.NewObjectService(app.ObjectServiceConfig{
		Index: c.meta, Buckets: c.meta, Uploads: c.meta, Drives: fixed(t, c.topology, stores), Codec: porttest.NewCodec(),
		OnHeal: c.heals.add, KMS: c.kms, Authz: c.authz, Clock: c.clock, Entropy: porttest.Entropy(),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.service = service
	return c
}

func (c *clusterFixture) putThrough(t *testing.T, key string, data []byte) (object.Version, error) {
	t.Helper()
	return c.service.Put(context.Background(), porttest.Root(), app.PutInput{
		Bucket: "b", Key: key, Body: bytes.NewReader(data), Size: int64(len(data)),
	})
}

func (c *clusterFixture) getThrough(key string) ([]byte, error) {
	out, err := c.service.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: key})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(out.Body)
	return buf.Bytes(), err
}

func (c *clusterFixture) take(down bool, nodes ...int) {
	for _, i := range nodes {
		c.down[i].Store(down)
	}
}

// TestNodesDownAreNotDamage: six drives on six nodes, three data and three
// parity shards. Up to three nodes can be down and every object is read.
// With more down, a read fails as unavailable - not as an integrity failure,
// and not as "no such object": nothing is known to be wrong, and the request
// can be repeated. Nothing is ever rewritten or removed because a node could
// not be reached.
func TestNodesDownAreNotDamage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c := newClusterFixture(t)
	data := randomBytes(t, 3_000_000)
	version, err := c.putThrough(t, "k", data)
	if err != nil {
		t.Fatal(err)
	}
	before := c.snapshot(t, version)

	for down := 1; down <= 3; down++ {
		c.take(true, down-1)
		got, err := c.getThrough("k")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get with %d nodes down: %v", down, err)
		}
	}
	// A fourth node goes: fewer shards can be reached than are needed.
	c.take(true, 3)
	_, err = c.getThrough("k")
	if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrObjectNotFound) {
		t.Fatalf("Get with four nodes down = %v, want %v and neither an integrity failure nor a missing object", err, domain.ErrUnavailable)
	}
	// Healing can do nothing, and must conclude nothing.
	result, err := c.service.Healer().HealObject(ctx, "b", "k")
	if err != nil || result.Offline != 4 || result.Damaged != 0 || result.Rebuilt != 0 || result.Unrecoverable {
		t.Errorf("heal with four nodes down = %+v, %v; want four shards offline and nothing else", result, err)
	}
	// A node goes down in the middle of a read.
	c.take(false, 0, 1, 2, 3)
	out, err := c.service.Get(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 1_100_000)
	if _, err := out.Body.Read(head[:1]); err != nil {
		t.Fatal(err)
	}
	c.take(true, 0, 1, 2, 3)
	var rest bytes.Buffer
	_, err = rest.ReadFrom(out.Body)
	_ = out.Body.Close()
	if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("a read that lost four nodes on the way = %v, want %v", err, domain.ErrUnavailable)
	}

	// Really damaged and partly out of reach: one shard is corrupt, three
	// nodes are down. Two good shards can be reached, three are needed. The
	// object may well be whole - nobody can tell - so it is not reported as
	// lost, and nothing is rewritten.
	c.take(false, 0, 1, 2, 3)
	drives := c.shardsOf(t, version)
	drives[5].Damage(shardID(version, 5))
	c.take(true, 0, 1, 2)
	result, err = c.service.Healer().HealObject(ctx, "b", "k")
	if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrIntegrity) || result.Unrecoverable || result.Rebuilt != 0 {
		t.Errorf("heal of a damaged object with three nodes down = %+v, %v; want unavailable, not unrecoverable", result, err)
	}
	// With one of them back there is enough to rebuild from: the damaged
	// shard is rebuilt, and the shards on the nodes still down are left for
	// later.
	c.take(false, 0)
	result, err = c.service.Healer().HealObject(ctx, "b", "k")
	if err != nil || result.Damaged != 1 || result.Rebuilt != 1 || result.Offline != 2 {
		t.Errorf("heal with two nodes down = %+v, %v; want the damaged shard rebuilt and two left offline", result, err)
	}
	c.take(false, 1, 2)
	after := c.snapshot(t, version)
	for i := range before {
		if !bytes.Equal(before[i], after[i]) {
			t.Errorf("shard %d differs from what was written", i)
		}
	}
	if n := c.shardCount(); n != sixDrives {
		t.Errorf("%d shards, want %d: unreachable nodes must cost no shard", n, sixDrives)
	}
}

// TestWritesNeedAWriteQuorumOfReachableDrives: a write goes on without the
// nodes that are down while a write quorum remains, and the object is healed
// onto them later. Below the quorum the write is refused and leaves nothing.
func TestWritesNeedAWriteQuorumOfReachableDrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c := newClusterFixture(t)
	data := randomBytes(t, 2_500_000)

	// Three data and three parity shards: the write quorum is four.
	c.take(true, 4, 5)
	version, err := c.putThrough(t, "degraded", data)
	if err != nil {
		t.Fatalf("Put with two nodes down: %v", err)
	}
	if n := c.shardCount(); n != 4 {
		t.Fatalf("%d shards after a write with two nodes down, want 4", n)
	}
	if got, err := c.getThrough("degraded"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get of the degraded object: %v", err)
	}

	c.take(true, 3)
	if _, err := c.putThrough(t, "refused", data); !errors.Is(err, domain.ErrInsufficientDrives) {
		t.Fatalf("Put with three nodes down = %v, want %v", err, domain.ErrInsufficientDrives)
	}
	if _, err := c.meta.GetObject(ctx, "b", "refused"); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("the refused write is visible: %v", err)
	}
	if n := c.shardCount(); n != 4 {
		t.Errorf("%d shards after a refused write, want the 4 of the object before", n)
	}
	// A node goes down while an object is being written: the write ends
	// below the quorum and nothing of it stays.
	c.take(false, 3)
	body := &trippingReader{data: data, at: 1_500_000, trip: func() { c.take(true, 2, 3) }}
	_, err = c.service.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "interrupted", Body: body, Size: int64(len(data))})
	if !errors.Is(err, domain.ErrInsufficientDrives) {
		t.Fatalf("Put that lost its quorum on the way = %v, want %v", err, domain.ErrInsufficientDrives)
	}
	c.take(false, 2, 3, 4, 5)
	if n := c.shardCount(); n != 4 {
		t.Errorf("%d shards after an interrupted write, want 4", n)
	}

	// The nodes are back: healing completes the degraded object.
	result, err := c.service.Healer().HealObject(ctx, "b", "degraded")
	if err != nil || result.Damaged != 2 || result.Rebuilt != 2 || result.Offline != 0 {
		t.Fatalf("heal after the nodes returned = %+v, %v", result, err)
	}
	drives := c.shardsOf(t, version)
	for _, lost := range []int{0, 1, 2} {
		_ = drives[lost].Delete(ctx, shardID(version, lost))
	}
	if got, err := c.getThrough("degraded"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get from the healed shards alone: %v", err)
	}
}

// trippingReader calls trip once, after at bytes have been read.
type trippingReader struct {
	data []byte
	off  int
	at   int
	trip func()
}

func (r *trippingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	if r.off >= r.at && r.trip != nil {
		r.trip()
		r.trip = nil
	}
	n := copy(p, r.data[r.off:min(r.off+64<<10, len(r.data))])
	r.off += n
	return n, nil
}

// TestCollectorAsksInTheRightOrder: a write commits and lets go of its pin
// while the collector is looking at its shards. Whatever the collector saw
// before, it must not remove them: after finding no pin it looks at the
// index again, and there the version is.
func TestCollectorAsksInTheRightOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	for _, d := range f.drives {
		d.Now = func() time.Time { return testNow.Add(-48 * time.Hour) } // every shard is old
	}
	version := porttest.SampleVersion("b", "racing", 7)
	for i, d := range f.drives {
		d.Put(object.ShardID{DataID: version.DataID, Index: i}, []byte("shard"), testNow.Add(-48*time.Hour))
	}

	// The write is pinned and uncommitted when the collector first asks the
	// index; it commits and unpins just as the collector asks for pins.
	committed := false
	collector := app.NewCollector(app.CollectorConfig{
		Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: time.Hour, Logger: slog.New(slog.DiscardHandler),
		InFlight: func(context.Context, object.ID) (bool, error) {
			if !committed {
				if _, err := f.meta.PutVersion(ctx, version); err != nil {
					t.Errorf("PutVersion: %v", err)
				}
				committed = true
			}
			return false, nil
		},
	})
	stats, err := collector.RunOnce(ctx)
	if err != nil || stats.Removed != 0 {
		t.Fatalf("collection racing a commit = %+v, %v; want nothing removed", stats, err)
	}
	if n := f.shardCount(); n != sixDrives {
		t.Fatalf("%d of %d shards of an object that was just committed are left", n, sixDrives)
	}

	// A node that cannot be asked may have a write in progress: keep.
	orphan := object.ID{0xAA}
	f.drives[0].Put(object.ShardID{DataID: orphan}, []byte("x"), testNow.Add(-48*time.Hour))
	unreachable := errors.New("node n3 cannot be reached")
	asked := 0
	collector = app.NewCollector(app.CollectorConfig{
		Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: time.Hour, Logger: slog.New(slog.DiscardHandler),
		InFlight: func(context.Context, object.ID) (bool, error) { asked++; return false, unreachable },
	})
	stats, err = collector.RunOnce(ctx)
	if err != nil || stats.Removed != 0 || stats.InFlight != 1 {
		t.Errorf("collection with a node that cannot be asked = %+v, %v; want the orphan kept", stats, err)
	}
	// Only data that looks unreferenced costs a question to the other nodes.
	if asked != 1 {
		t.Errorf("the other nodes were asked %d times, want once", asked)
	}
	// Every node answers "no": now it goes.
	collector = app.NewCollector(app.CollectorConfig{
		Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: time.Hour, Logger: slog.New(slog.DiscardHandler),
	})
	if stats, err := collector.RunOnce(ctx); err != nil || stats.Removed != 1 {
		t.Errorf("collection with every node answering = %+v, %v; want the orphan removed", stats, err)
	}
}

// TestOnlyTheActiveNodeScrubs: in a cluster the node that leads the metadata
// group scrubs. A node that does not lead does nothing, and one that stops
// leading in the middle of a pass stops there.
func TestOnlyTheActiveNodeScrubs(t *testing.T) {
	t.Parallel()

	f := erasureFixture(t)
	for i := range 6 {
		f.put(t, "b", fmt.Sprintf("k%d", i), randomBytes(t, 1000))
	}
	var active atomic.Bool
	var passes atomic.Int32
	var checked atomic.Int64
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
		Active: active.Load,
		OnPass: func(s app.ScrubStats) {
			checked.Add(int64(s.Objects))
			passes.Add(1)
		},
		Pause: func(ctx context.Context, _ time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(time.Millisecond):
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scrubber.Run(ctx, time.Hour); close(done) }()

	time.Sleep(50 * time.Millisecond)
	if passes.Load() != 0 {
		t.Fatal("a node that is not active scrubbed")
	}
	active.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for passes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if passes.Load() == 0 || checked.Load() < 6 {
		t.Fatalf("the active node made %d passes over %d objects", passes.Load(), checked.Load())
	}
	active.Store(false)
	time.Sleep(20 * time.Millisecond)
	settled := passes.Load()
	time.Sleep(50 * time.Millisecond)
	if passes.Load() != settled {
		t.Error("a node kept scrubbing after it stopped being active")
	}
	cancel()
	<-done
}

// TestEnsureTopologyOfACluster: the topology of a new cluster is recorded by
// one node, from the drives of all of them, spread across the nodes; the
// others wait for it and then answer for their own drives.
func TestEnsureTopologyOfACluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	meta := porttest.NewMemMetadata()
	nodes := map[string][]*porttest.MemShardStore{}
	for _, id := range []string{"n1", "n2", "n3", "n4"} {
		nodes[id] = newDrives(id+"-a", id+"-b")
	}
	others := func(self string) ([]cluster.DriveState, map[string]cluster.Labels) {
		var drives []cluster.DriveState
		labels := map[string]cluster.Labels{}
		for id, own := range nodes {
			if id == self {
				continue
			}
			labels[id] = cluster.Labels{Zone: "zone-" + id}
			for _, d := range own {
				drives = append(drives, cluster.DriveState{ID: d.DriveID(), NodeID: id, Empty: d.Empty(), DeploymentID: d.DeploymentID()})
			}
		}
		return drives, labels
	}
	options := func(self string, leader bool, reachable bool) app.TopologyOptions {
		opts := topologyOptions(false)
		opts.NodeID = self
		opts.Survey = func(context.Context) ([]cluster.DriveState, map[string]cluster.Labels, error) {
			switch {
			case !leader:
				return nil, nil, fmt.Errorf("%w: the leader records the topology", domain.ErrUnavailable)
			case !reachable:
				return nil, nil, fmt.Errorf("%w: no answer from n3", domain.ErrUnavailable)
			}
			drives, labels := others(self)
			return drives, labels, nil
		}
		return opts
	}

	// A node that does not lead waits; so does the leader while a node has
	// not reported. Nothing is recorded, nothing claimed.
	if _, err := app.EnsureTopology(ctx, meta, attachedDrives(nodes["n2"]...), options("n2", false, true)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("a follower before the topology exists: %v", err)
	}
	if _, err := app.EnsureTopology(ctx, meta, attachedDrives(nodes["n1"]...), options("n1", true, false)); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("the leader with a node missing: %v", err)
	}
	if _, err := meta.GetTopology(ctx); !errors.Is(err, domain.ErrNoTopology) {
		t.Fatal("a topology was recorded before every node had reported")
	}
	if nodes["n1"][0].DeploymentID() != "" {
		t.Fatal("a drive was claimed before the topology existed")
	}

	// A drive somewhere holds data: no new deployment, on any node's word.
	nodes["n3"][1].Put(object.ShardID{DataID: object.ID{1}}, []byte("old data"), testNow)
	if _, err := app.EnsureTopology(ctx, meta, attachedDrives(nodes["n1"]...), options("n1", true, true)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("a new cluster with data on a drive of another node: %v", err)
	}
	_ = nodes["n3"][1].Delete(ctx, object.ShardID{DataID: object.ID{1}})

	topology, err := app.EnsureTopology(ctx, meta, attachedDrives(nodes["n1"]...), options("n1", true, true))
	if err != nil {
		t.Fatalf("the leader with every node reporting: %v", err)
	}
	if len(topology.Sets) != 1 || topology.SetSize() != 8 || topology.NodeFailuresTolerated() != 2 || len(topology.NodeIDs()) != 4 {
		t.Fatalf("topology = %+v", topology)
	}
	// The leader claimed its own drives, and only those.
	if nodes["n1"][0].DeploymentID() != topology.DeploymentID || nodes["n2"][0].DeploymentID() != "" {
		t.Error("the leader claimed drives that are not its own, or not its own")
	}
	// The others find the topology and claim theirs. They ask nobody.
	for _, id := range []string{"n2", "n3", "n4"} {
		got, err := app.EnsureTopology(ctx, meta, attachedDrives(nodes[id]...), options(id, false, false))
		if err != nil || got.Version != topology.Version {
			t.Fatalf("node %s: %+v, %v", id, got, err)
		}
		for _, d := range nodes[id] {
			if d.DeploymentID() != topology.DeploymentID {
				t.Errorf("drive %s was not claimed by its node", d.DriveID())
			}
		}
	}
	// A node that starts while others are down minds only its own drives.
	if _, err := app.EnsureTopology(ctx, meta, attachedDrives(nodes["n4"]...), options("n4", false, false)); err != nil {
		t.Errorf("restart of n4 with the rest of the cluster unreachable: %v", err)
	}
	// With another node's drive attached, it does not start.
	swapped := attachedDrives(nodes["n4"][0], nodes["n3"][1])
	if _, err := app.EnsureTopology(ctx, meta, swapped, options("n4", false, false)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("n4 with a drive of n3: %v", err)
	}
}

// TestTopologyFromBeforeSlotsNamedTheirNode: a deployment created by the
// release before clusters has a topology whose slots name no node. Its node
// was alone, so the drives are that node's: at the first start of this
// release the topology is rewritten to say so, once, and the deployment goes
// on as it was. Such a deployment cannot be made a member of a cluster.
func TestTopologyFromBeforeSlotsNamedTheirNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	meta := porttest.NewMemMetadata()
	drives := newDrives("d0", "d1", "d2", "d3")
	// As the earlier release recorded it: no node anywhere.
	old, err := cluster.NewTopology([]string{"d0", "d1", "d2", "d3"})
	if err != nil {
		t.Fatal(err)
	}
	old.DeploymentID = "0123456789abcdef0123456789abcdef"
	if !old.Unassigned() {
		t.Fatal("NewTopology names a node")
	}
	if err := meta.SetTopology(ctx, old); err != nil {
		t.Fatal(err)
	}
	for _, d := range drives {
		d.Put(object.ShardID{DataID: object.ID{1}}, []byte("data from the earlier release"), testNow)
		if err := d.Claim(ctx, old.DeploymentID); err != nil {
			t.Fatal(err)
		}
	}

	opts := topologyOptions(false)
	opts.NodeID = "node-a"
	clustered := opts
	clustered.Survey = func(context.Context) ([]cluster.DriveState, map[string]cluster.Labels, error) { return nil, nil, nil }
	if _, err := app.EnsureTopology(ctx, meta, attachedDrives(drives...), clustered); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("a standalone deployment started as a cluster member: %v", err)
	}

	migrated, err := app.EnsureTopology(ctx, meta, attachedDrives(drives...), opts)
	if err != nil {
		t.Fatalf("first start of this release on the earlier release's deployment: %v", err)
	}
	if migrated.Unassigned() || migrated.Version != 2 || migrated.DeploymentID != old.DeploymentID ||
		!slices.Equal(migrated.DriveIDs(), old.DriveIDs()) || !slices.Equal(migrated.NodeIDs(), []string{"node-a"}) {
		t.Fatalf("migrated topology = %+v", migrated)
	}
	// Nothing else changed, and the next start changes nothing at all.
	if n := len(drives[0].IDs()); n != 1 {
		t.Errorf("drive holds %d shards after the migration", n)
	}
	again, err := app.EnsureTopology(ctx, meta, attachedDrives(drives...), opts)
	if err != nil || again.Version != 2 {
		t.Errorf("second start: version %d, %v", again.Version, err)
	}
	// An empty topology is not "unassigned".
	if (cluster.Topology{}).Unassigned() {
		t.Error("an empty topology reports itself unassigned")
	}
}
