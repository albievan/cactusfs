package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/passwords"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// testCluster is a cluster of nodes in this process, each with its own
// drives, metadata directory and certificate, talking over loopback.
type testCluster struct {
	t         *testing.T
	dir       string
	authority *nodetls.Authority
	configs   map[string]serverConfig

	mu    sync.Mutex
	nodes map[string]*node
	logs  map[string]*bytes.Buffer
	// telemetry is made before any context is in play.
	telemetry map[string]*telemetry.Telemetry
}

func root() iam.Principal { return iam.Principal{AccessKeyID: "ROOT", Root: true} }

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}

// newTestCluster prepares a cluster: roles maps node IDs to their roles.
func newTestCluster(t *testing.T, roles map[string]string) *testCluster {
	t.Helper()
	authority, err := nodetls.NewAuthority(rand.Reader, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := &testCluster{
		t: t, dir: t.TempDir(), authority: authority, configs: map[string]serverConfig{},
		nodes: map[string]*node{}, logs: map[string]*bytes.Buffer{}, telemetry: map[string]*telemetry.Telemetry{},
	}
	for id := range roles {
		c.telemetry[id] = newTelemetry(t)
	}
	writeTestFile(t, filepath.Join(c.dir, "ca.pem"), string(authority.CertPEM))
	// One KMS key for the cluster.
	keyFile := filepath.Join(c.dir, "kms.json")
	if err := local.Create(keyFile, []byte(nodePassphrase), local.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1}, rand.Reader); err != nil {
		t.Fatal(err)
	}

	peers := map[string]string{}
	raft := map[string]string{}
	for id := range roles {
		peers[id], raft[id] = freeAddr(t), freeAddr(t)
	}
	for id, list := range roles {
		parsed, err := cluster.ParseRoles(list)
		if err != nil {
			t.Fatal(err)
		}
		cfg := serverConfig{
			NodeID: id, Roles: parsed, Peers: peers, RPCAddr: peers[id], RaftAddr: raft[id],
			AdvertiseRPC: peers[id], AdvertiseRaft: raft[id],
			MetaDir: filepath.Join(c.dir, id, "meta"), KMS: kmsLocal, KMSKeyFile: keyFile,
			NodeTLSCA: filepath.Join(c.dir, "ca.pem"), GCGrace: time.Hour,
			Labels:  cluster.Labels{Zone: "zone-" + id[len(id)-1:]},
			secrets: secrets{kmsPassphrase: nodePassphrase},
			raftTiming: raftmeta.Timing{
				// Long enough that a machine busy with other tests is not
				// taken for a lost leader.
				Heartbeat: time.Second, Election: time.Second, LeaderLease: time.Second,
				Commit: 5 * time.Millisecond, LeaderWait: 20 * time.Second,
			},
			passwordParams: passwords.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1},
		}
		if parsed.Has(cluster.RoleStorage) {
			cfg.Data = []string{filepath.Join(c.dir, id, "d1"), filepath.Join(c.dir, id, "d2")}
		}
		c.issue(id, &cfg)
		c.configs[id] = cfg
	}
	return c
}

func (c *testCluster) issue(id string, cfg *serverConfig) {
	c.t.Helper()
	certPEM, keyPEM, err := c.authority.Issue(id, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	cfg.NodeTLSCert, cfg.NodeTLSKey = filepath.Join(c.dir, id+".pem"), filepath.Join(c.dir, id+".key")
	writeTestFile(c.t, cfg.NodeTLSCert, string(certPEM))
	writeTestFile(c.t, cfg.NodeTLSKey, string(keyPEM))
}

// lockedBuffer is a log sink that several goroutines write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// open starts one node and waits until it is part of the cluster.
func (c *testCluster) open(ctx context.Context, id string) error {
	logs := &bytes.Buffer{}
	n, err := openNode(ctx, c.configs[id], c.telemetry[id], slog.New(slog.NewJSONHandler(&lockedBuffer{buf: logs}, nil)), io.Discard)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs[id] = logs
	if err != nil {
		return fmt.Errorf("node %s: %w", id, err)
	}
	c.nodes[id] = n
	c.t.Cleanup(func() { _ = n.Close() })
	return nil
}

// start starts the given nodes at the same time, as a cluster comes up.
func (c *testCluster) start(ids ...string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 90*time.Second)
	defer cancel()
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func() { errs <- c.open(ctx, id) }()
	}
	for range ids {
		if err := <-errs; err != nil {
			c.t.Fatalf("start: %v", err)
		}
	}
}

func (c *testCluster) all() []string {
	var ids []string
	for id := range c.configs {
		ids = append(ids, id)
	}
	return ids
}

func (c *testCluster) node(id string) *node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nodes[id]
}

func (c *testCluster) stop(id string) {
	c.t.Helper()
	c.mu.Lock()
	n := c.nodes[id]
	delete(c.nodes, id)
	c.mu.Unlock()
	if err := n.Close(); err != nil {
		c.t.Fatalf("stop %s: %v", id, err)
	}
}

func (c *testCluster) put(id, key string, data []byte) error {
	_, err := c.node(id).objects.Put(c.t.Context(), root(), app.PutInput{
		Bucket: "bkt", Key: key, Body: bytes.NewReader(data), Size: int64(len(data)),
		Verify: func() (string, error) { return "etag", nil },
	})
	return err
}

func (c *testCluster) get(id, key string) ([]byte, error) {
	out, err := c.node(id).objects.Get(c.t.Context(), root(), app.GetInput{Bucket: "bkt", Key: key})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

func randomData(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// shardsOn counts the shard files on a node's drives.
func (c *testCluster) shardsOn(id string) int {
	c.t.Helper()
	total := 0
	for _, dir := range c.configs[id].Data {
		matches, err := filepath.Glob(filepath.Join(dir, "shards", "*", "*"))
		if err != nil {
			c.t.Fatal(err)
		}
		total += len(matches)
	}
	return total
}

func fourNodes() map[string]string {
	return map[string]string{"n1": "", "n2": "", "n3": "", "n4": "gateway,storage"}
}

// TestClusterStoresObjectsAcrossNodes: four nodes form a cluster. An object
// written through any of them is read through any other; its shards are
// spread over all four; it survives the loss of a node, and of a second for
// reading; and what was written while a node was away is rebuilt on it when
// it returns.
func TestClusterStoresObjectsAcrossNodes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t, fourNodes())
	c.start(c.all()...)

	// Eight drives on four nodes in four zones: one erasure set, four data
	// and four parity shards, two shards per node.
	topology, err := c.node("n4").meta.GetTopology(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Sets) != 1 || topology.SetSize() != 8 || len(topology.NodeIDs()) != 4 || topology.NodeFailuresTolerated() != 2 {
		t.Fatalf("topology: %d sets of %d drives on nodes %v, tolerating %d node failures",
			len(topology.Sets), topology.SetSize(), topology.NodeIDs(), topology.NodeFailuresTolerated())
	}
	// Neighbouring slots are on different nodes: the drives were dealt out
	// across the zones.
	for i, slot := range topology.Sets[0] {
		if next := topology.Sets[0][(i+1)%8]; next.NodeID == slot.NodeID {
			t.Errorf("slots %d and %d are both on node %s", i, i+1, slot.NodeID)
		}
	}
	// Every node recorded itself, and every drive is claimed.
	nodes, err := c.node("n1").meta.ListNodes(ctx)
	if err != nil || len(nodes) != 4 {
		t.Fatalf("ListNodes = %d nodes, %v", len(nodes), err)
	}
	for _, id := range c.all() {
		for _, drive := range c.node(id).drives {
			if drive.DeploymentID() != topology.DeploymentID {
				t.Errorf("drive %s of %s is not claimed for the deployment", drive.DriveID(), id)
			}
		}
	}

	// The voters were fixed when the cluster formed. A node without a vote
	// cannot talk itself into one, and it cannot speak Raft to anybody: the
	// Raft port admits voters only, whatever certificate is shown.
	var leaderID string
	for _, id := range c.all() {
		if c.node(id).meta.IsLeader() {
			leaderID = id
		}
	}
	n4 := c.node("n4")
	self := n4.self(c.configs["n4"])
	if err := n4.rpc.Join(ctx, leaderID, self.ID, self.RaftAddr, true); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a storage node asking for a vote = %v, want %v", err, domain.ErrInvalidArgument)
	}
	if err := n4.rpc.Join(ctx, leaderID, self.ID, nodetls.Address("n1", "127.0.0.1:1"), false); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("joining with another node's Raft address = %v, want %v", err, domain.ErrInvalidArgument)
	}
	if err := n4.rpc.Join(ctx, leaderID, self.ID, self.RaftAddr, false); err != nil {
		t.Errorf("joining again, as what it is: %v", err)
	}
	members, err := c.node(leaderID).meta.Members()
	voters := 0
	for _, m := range members {
		if m.Voter {
			voters++
		}
	}
	if err != nil || voters != 3 || len(members) != 4 {
		t.Errorf("members = %+v, %v; want four with three voters", members, err)
	}
	if conn, err := n4.identity.Dial(ctx, nodetls.Address("n1", c.configs["n1"].AdvertiseRaft)); err == nil {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("the Raft port of a voter served a node without a vote")
		}
		_ = conn.Close()
	}
	if !c.node("n4").mayReplicate(c.configs["n4"], "n2") || c.node("n1").mayReplicate(c.configs["n1"], "n4") ||
		c.node("n1").mayReplicate(c.configs["n1"], "stranger") {
		t.Error("mayReplicate does not follow the voters of the metadata group")
	}

	if err := c.node("n4").buckets.Create(ctx, root(), "bkt"); err != nil {
		t.Fatalf("CreateBucket on n4: %v", err)
	}
	objects := map[string][]byte{}
	ids := []string{"n1", "n2", "n3", "n4"}
	for i := range 12 {
		key, data := fmt.Sprintf("object-%02d", i), randomData(t, 1000+i*300_000)
		writer, reader := ids[i%4], ids[(i+1)%4]
		if err := c.put(writer, key, data); err != nil {
			t.Fatalf("Put %s through %s: %v", key, writer, err)
		}
		// Read-after-write, through another node.
		got, err := c.get(reader, key)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get %s through %s right after writing through %s: %v", key, reader, writer, err)
		}
		objects[key] = data
	}
	for _, id := range ids {
		if n := c.shardsOn(id); n != 2*len(objects) {
			t.Errorf("node %s holds %d shards, want 2 of each of the %d objects", id, n, len(objects))
		}
	}
	verify := func(through, when string) {
		t.Helper()
		for key, want := range objects {
			got, err := c.get(through, key)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s: Get %s through %s: %v", when, key, through, err)
			}
		}
	}

	// A node is lost. Nothing is unavailable.
	c.stop("n2")
	verify("n4", "with n2 down")
	for i := range 4 {
		key, data := fmt.Sprintf("degraded-%d", i), randomData(t, 400_000)
		if err := c.put(ids[(i%2)*2], key, data); err != nil { // through n1 and n3
			t.Fatalf("Put with n2 down: %v", err)
		}
		objects[key] = data
	}
	verify("n1", "with n2 down, after writing")
	// The collector cannot ask n2 whether it has a write in progress, so it
	// must not conclude that there is none.
	if _, err := c.node("n1").inFlightAnywhere(ctx, object.ID{1}); err == nil {
		t.Error("with a node down, the collector was told that no write is in progress anywhere")
	}

	// A second node is lost. Four shards of eight are left: every object
	// can still be read, and no write is accepted, because it could not be
	// made durable on a write quorum.
	c.stop("n4")
	verify("n3", "with n2 and n4 down")
	if err := c.put("n1", "refused", randomData(t, 1000)); !errors.Is(err, domain.ErrInsufficientDrives) {
		t.Fatalf("Put with two nodes down = %v, want %v", err, domain.ErrInsufficientDrives)
	}
	if _, err := c.get("n1", "refused"); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("the refused object: %v, want %v", err, domain.ErrObjectNotFound)
	}
	// Healing with half the shards out of reach finds nothing damaged and
	// touches nothing.
	result, err := c.node("n1").objects.Healer().HealObject(ctx, "bkt", "object-00")
	if err != nil || result.Offline != 4 || result.Damaged != 0 || result.Rebuilt != 0 {
		t.Errorf("heal with two nodes down = %+v, %v; want four shards offline and none damaged", result, err)
	}

	// Both return. What n2 missed is rebuilt on it by the scrubber of the
	// node that leads.
	c.start("n2", "n4")
	missing := 2*len(objects) - c.shardsOn("n2")
	if missing != 8 {
		t.Fatalf("n2 lacks %d shards, want the 8 of the four objects written without it", missing)
	}
	var leader *node
	for _, id := range ids {
		if c.node(id).meta.IsLeader() {
			leader = c.node(id)
		}
	}
	if leader == nil {
		t.Fatal("no node leads")
	}
	// A pass that runs before the leader has its connection to the returned
	// node back finds those shards out of reach and leaves them for the
	// next pass - it must not count them as damage, or as rebuilt.
	total := 0
	for deadline := time.Now().Add(30 * time.Second); total < 8; {
		stats, err := leader.scrubber.RunOnce(ctx)
		if err != nil || stats.Unrecoverable != 0 || stats.Objects != len(objects) {
			t.Fatalf("scrub after the nodes returned = %+v, %v", stats, err)
		}
		total += stats.ShardsRebuilt
		if time.Now().After(deadline) {
			t.Fatalf("the scrubber rebuilt %d shards on the returned node, want 8", total)
		}
	}
	if total != 8 {
		t.Fatalf("the scrubber rebuilt %d shards, want exactly the 8 that were missing", total)
	}
	if n := c.shardsOn("n2"); n != 2*len(objects) {
		t.Errorf("n2 holds %d shards after the scrub, want %d", n, 2*len(objects))
	}
	if busy, err := c.node("n1").inFlightAnywhere(ctx, object.ID{1}); err != nil || busy {
		t.Errorf("inFlightAnywhere with every node up = %v, %v", busy, err)
	}
	// Full redundancy is back: two other nodes can go.
	c.stop("n4")
	c.stop("n1")
	verify("n2", "with n1 and n4 down, after healing")
}

// TestClusterRestart: every node stops and the cluster comes back. Nothing is
// formed a second time, and everything is where it was.
func TestClusterRestart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t, fourNodes())
	c.start(c.all()...)
	before, err := c.node("n1").meta.GetTopology(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.node("n1").buckets.Create(ctx, root(), "bkt"); err != nil {
		t.Fatal(err)
	}
	data := randomData(t, 2_000_000)
	if err := c.put("n3", "kept", data); err != nil {
		t.Fatal(err)
	}

	for _, id := range c.all() {
		c.stop(id)
	}
	c.start(c.all()...)

	after, err := c.node("n4").meta.GetTopology(ctx)
	if err != nil || after.DeploymentID != before.DeploymentID || after.Version != before.Version {
		t.Fatalf("topology after the restart: deployment %q version %d, %v; want %q version %d",
			after.DeploymentID, after.Version, err, before.DeploymentID, before.Version)
	}
	for _, id := range c.all() {
		got, err := c.get(id, "kept")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get through %s after the restart: %v", id, err)
		}
	}
	// The root credential is the cluster's: whichever node generated it,
	// every node knows the same one.
	stored, err := c.node("n1").meta.GetRootCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range c.all() {
		if _, principal, err := c.node(id).identities.SecretFor(ctx, stored.AccessKeyID); err != nil || !principal.Root {
			t.Errorf("root credential on %s: %v", id, err)
		}
	}
}

// TestClusterRefusesWhatItMustNotBecome: configurations that would make a
// cluster without a majority to lose, or with strangers in it.
func TestClusterRefusesWhatItMustNotBecome(t *testing.T) {
	t.Parallel()

	t.Run("two voters", func(t *testing.T) {
		t.Parallel()
		c := newTestCluster(t, map[string]string{"n1": "", "n2": "", "n3": "gateway,storage", "n4": "gateway,storage"})
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		errs := make(chan error, 4)
		for _, id := range c.all() {
			go func() { errs <- c.open(ctx, id) }()
		}
		refused := 0
		for range 4 {
			err := <-errs
			if err != nil && errors.Is(err, domain.ErrInvalidArgument) && strings.Contains(err.Error(), "3 or 5") {
				refused++
				// The others would wait for ever; one refusal is the verdict.
				cancel()
			}
		}
		if refused == 0 {
			t.Error("a cluster with two voters was not refused")
		}
	})

	t.Run("three storage nodes", func(t *testing.T) {
		t.Parallel()
		c := newTestCluster(t, map[string]string{"n1": "", "n2": "", "n3": "", "n4": "gateway"})
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		errs := make(chan error, 4)
		for _, id := range c.all() {
			go func() { errs <- c.open(ctx, id) }()
		}
		refused := 0
		for range 4 {
			if err := <-errs; err != nil && strings.Contains(err.Error(), "at least 4 nodes with the storage role") {
				refused++
				cancel()
			}
		}
		if refused == 0 {
			t.Error("a cluster with three storage nodes was not refused")
		}
	})

	t.Run("certificate of another node", func(t *testing.T) {
		t.Parallel()
		c := newTestCluster(t, fourNodes())
		cfg := c.configs["n1"]
		cfg.NodeTLSCert, cfg.NodeTLSKey = c.configs["n2"].NodeTLSCert, c.configs["n2"].NodeTLSKey
		c.configs["n1"] = cfg
		if err := c.open(t.Context(), "n1"); !errors.Is(err, domain.ErrInvalidArgument) || !strings.Contains(err.Error(), "certificate names") {
			t.Errorf("start with another node's certificate = %v", err)
		}
	})

	t.Run("certificate of another CA", func(t *testing.T) {
		t.Parallel()
		c := newTestCluster(t, fourNodes())
		other := newTestCluster(t, fourNodes())
		cfg := c.configs["n1"]
		cfg.NodeTLSCert, cfg.NodeTLSKey = other.configs["n1"].NodeTLSCert, other.configs["n1"].NodeTLSKey
		c.configs["n1"] = cfg
		if err := c.open(t.Context(), "n1"); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("start with a certificate of another CA = %v", err)
		}
	})
}

// TestClusterConfigRules: what the command line of a cluster member must say.
func TestClusterConfigRules(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := envMap(map[string]string{envKMSPhrase: nodePassphrase})
	base := []string{
		"--dev-plaintext", "--kms", "local", "--kms-key-file", "k.json", "--data", dir,
		"--node-tls-cert", "c.pem", "--node-tls-key", "c.key", "--node-tls-ca", "ca.pem",
	}
	with := func(args ...string) []string { return append(append([]string{}, base...), args...) }
	without := func(drop string, args ...string) []string {
		var out []string
		for i := 0; i < len(base); i++ {
			if base[i] == drop {
				i++
				continue
			}
			out = append(out, base[i])
		}
		return append(out, args...)
	}
	peers := "a=host-a:9002,b=host-b:9002,c=host-c:9002,d=host-d:9002"

	cfg, err := parseServerConfig(with("--node-id", "b", "--peers", peers, "--roles", "storage,gateway",
		"--topology-zone", "z1", "--topology-rack", "r7", "--topology-region", "eu", "--topology-host", "h"), env, io.Discard)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if !cfg.clustered() || cfg.NodeID != "b" || cfg.AdvertiseRPC != "host-b:9002" || cfg.AdvertiseRaft != "host-b:9003" ||
		cfg.RPCAddr != ":9002" || cfg.RaftAddr != ":9003" || len(cfg.Peers) != 4 || cfg.Roles.String() != "storage,gateway" ||
		cfg.Labels != (cluster.Labels{Region: "eu", Zone: "z1", Rack: "r7", Host: "h"}) {
		t.Errorf("config = %+v", cfg)
	}
	cfg, err = parseServerConfig(with("--node-id", "b", "--peers", peers, "--advertise-rpc", "public:19002",
		"--advertise-raft", "public:19003", "--raft-addr", "127.0.0.1:7003"), env, io.Discard)
	if err != nil || cfg.AdvertiseRPC != "public:19002" || cfg.AdvertiseRaft != "public:19003" {
		t.Errorf("advertised addresses: %+v, %v", cfg, err)
	}
	// A node without drives.
	if _, err := parseServerConfig(without("--data", "--node-id", "b", "--peers", peers, "--roles", "gateway,meta", "--meta-dir", dir), env, io.Discard); err != nil {
		t.Errorf("a gateway without drives: %v", err)
	}

	refused := map[string][]string{
		"no node ID":                       with("--peers", peers),
		"not among the peers":              with("--node-id", "x", "--peers", peers),
		"bad node ID":                      with("--node-id", "Not_A_Name", "--peers", peers),
		"malformed peer":                   with("--node-id", "a", "--peers", "a"),
		"peer without host":                with("--node-id", "a", "--peers", "a=:9002"),
		"peer listed twice":                with("--node-id", "a", "--peers", "a=h:1,a=h:2"),
		"bad peer ID":                      with("--node-id", "a", "--peers", "a=h:1,B!=h:2"),
		"unknown role":                     with("--node-id", "a", "--peers", peers, "--roles", "gateway,janitor"),
		"no node certificate":              without("--node-tls-cert", "--node-id", "a", "--peers", peers),
		"no node CA":                       without("--node-tls-ca", "--node-id", "a", "--peers", peers),
		"no shared key file":               without("--kms-key-file", "--node-id", "a", "--peers", peers),
		"storage role without drives":      without("--data", "--node-id", "a", "--peers", peers, "--meta-dir", dir),
		"drives without the storage role":  with("--node-id", "a", "--peers", peers, "--roles", "gateway"),
		"no drives and no metadata dir":    without("--data", "--node-id", "a", "--peers", peers, "--roles", "gateway"),
		"bad advertised RPC address":       with("--node-id", "a", "--peers", peers, "--advertise-rpc", "nohost"),
		"bad advertised Raft address":      with("--node-id", "a", "--peers", peers, "--advertise-raft", ":9003"),
		"bad RPC listen address":           with("--node-id", "a", "--peers", peers, "--rpc-addr", "nonsense"),
		"node certificate without cluster": with(),
		"roles without cluster":            without("--node-tls-cert", "--roles", "gateway"),
	}
	for name, args := range refused {
		if name == "roles without cluster" {
			args = []string{"--dev-plaintext", "--kms", "local", "--data", dir, "--roles", "storage"}
		}
		if _, err := parseServerConfig(args, env, io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestClusterIAM: identities are the cluster's, not a node's. A user and a
// key made through one node authenticate on every other at once; a
// revocation on one node is honoured by the next request on any other; and
// every node holds the same audit trail, which verifies.
func TestClusterIAM(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t, fourNodes())
	c.start(c.all()...)

	stored, err := c.node("n1").meta.GetRootCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admin := iam.Principal{AccessKeyID: stored.AccessKeyID, Root: true}
	if _, err := c.node("n1").iam.CreateUser(ctx, admin, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: "a console password"}); err != nil {
		t.Fatalf("CreateUser through n1: %v", err)
	}
	creds, _, err := c.node("n2").iam.CreateAPIKey(ctx, admin, app.APIKeyInput{Owner: "alice"})
	if err != nil {
		t.Fatalf("CreateAPIKey through n2: %v", err)
	}
	for _, id := range c.all() {
		secret, principal, err := c.node(id).identities.SecretFor(ctx, creds.AccessKeyID)
		if err != nil || string(secret) != creds.SecretKey || principal.User != "alice" || len(principal.Policies) != 1 {
			t.Fatalf("the key on %s: %+v, %v", id, principal, err)
		}
		// She writes through this node and reads through the next.
		if err := c.node(id).buckets.Create(ctx, admin, "bkt-"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.node(id).objects.Put(ctx, principal, app.PutInput{Bucket: "bkt-" + id, Key: "k", Body: strings.NewReader("x"), Size: 1}); err != nil {
			t.Errorf("alice writing through %s: %v", id, err)
		}
		// ...but makes no bucket: read-write does not allow it. The
		// refusal is audited by this node.
		if err := c.node(id).buckets.Create(ctx, principal, "alice-"+id); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("alice making a bucket through %s = %v", id, err)
		}
	}
	// A console session started on one node is known on another.
	signed, err := c.node("n3").sessions.SignIn(ctx, "alice", "a console password", iam.Session{})
	if err != nil {
		t.Fatalf("SignIn on n3: %v", err)
	}
	if p, err := c.node("n4").sessions.Authenticate(ctx, signed.Token, signed.CSRFToken, true, iam.Session{}); err != nil || p.User != "alice" {
		t.Errorf("the session on n4: %+v, %v", p, err)
	}

	// Revoked on n3: refused on every node by the next request.
	if err := c.node("n3").iam.DeleteAPIKey(ctx, admin, creds.AccessKeyID); err != nil {
		t.Fatal(err)
	}
	for _, id := range c.all() {
		if _, _, err := c.node(id).identities.SecretFor(ctx, creds.AccessKeyID); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("the revoked key on %s = %v, want %v", id, err, domain.ErrCredentialNotFound)
		}
	}

	// One trail, the same on every node.
	for _, id := range c.all() {
		c.node(id).auditor.Flush(ctx)
	}
	reference, err := c.node("n1").iam.AuditLog(ctx, admin, 0, 1000)
	if err != nil {
		t.Fatalf("AuditLog on n1: %v", err)
	}
	var changes, denials, signIns int
	for _, r := range reference {
		switch {
		case r.Outcome == audit.Denied:
			denials++
		case r.Action == "auth:SignIn":
			signIns++
		case strings.HasPrefix(r.Action, "admin:"):
			changes++
		}
	}
	if changes != 3 || denials != 4 || signIns != 1 {
		t.Errorf("trail: %d changes, %d denials, %d sign-ins; want 3, 4, 1", changes, denials, signIns)
	}
	for _, id := range []string{"n2", "n3", "n4"} {
		records, err := c.node(id).iam.AuditLog(ctx, admin, 0, 1000)
		if err != nil || !reflect.DeepEqual(records, reference) {
			t.Errorf("the audit trail on %s differs from the one on n1 (%d vs %d records, %v)", id, len(records), len(reference), err)
		}
	}
}

func TestVaultConfigRules(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vaultArgs := []string{
		"--dev-plaintext", "--data", dir, "--kms", "vault", "--vault-addr", "https://vault.test:8200",
		"--vault-key", "cactusfs", "--vault-token-file", "/run/secrets/vault-token",
	}
	cfg, err := parseServerConfig(append(append([]string{}, vaultArgs...), "--vault-transit-mount", "kms", "--vault-ca-cert", "ca.pem",
		"--vault-server-name", "vault.internal", "--vault-namespace", "team", "--audit-export"), noEnv, io.Discard)
	if err != nil {
		t.Fatalf("parseServerConfig: %v", err)
	}
	if cfg.KMS != kmsVault || cfg.VaultAddr != "https://vault.test:8200" || cfg.VaultMount != "kms" || cfg.VaultKey != "cactusfs" ||
		cfg.VaultCACert != "ca.pem" || cfg.VaultServerName != "vault.internal" || cfg.VaultNamespace != "team" ||
		cfg.KMSKeyFile != "" || !cfg.AuditExport {
		t.Errorf("config = %+v", cfg)
	}
	// With the keys in Vault a cluster needs no shared key file.
	clustered := append(append([]string{}, vaultArgs...), "--node-id", "a", "--peers", "a=h:1,b=h:2,c=h:3,d=h:4",
		"--node-tls-cert", "c", "--node-tls-key", "k", "--node-tls-ca", "ca")
	if _, err := parseServerConfig(clustered, noEnv, io.Discard); err != nil {
		t.Errorf("a cluster member with Vault: %v", err)
	}
	passphrase := envMap(map[string]string{envKMSPhrase: nodePassphrase})
	refused := map[string]struct {
		args []string
		env  func(string) string
	}{
		"no address":              {[]string{"--dev-plaintext", "--data", dir, "--kms", "vault", "--vault-key", "k", "--vault-token-file", "t"}, noEnv},
		"no key":                  {[]string{"--dev-plaintext", "--data", dir, "--kms", "vault", "--vault-addr", "https://v", "--vault-token-file", "t"}, noEnv},
		"no token file":           {[]string{"--dev-plaintext", "--data", dir, "--kms", "vault", "--vault-addr", "https://v", "--vault-key", "k"}, noEnv},
		"vault with a passphrase": {vaultArgs, passphrase},
		"vault with a key file":   {append(append([]string{}, vaultArgs...), "--kms-key-file", "k.json"), noEnv},
		"local with vault flags":  {[]string{"--dev-plaintext", "--data", dir, "--kms", "local", "--vault-addr", "https://v"}, passphrase},
		"unknown KMS":             {[]string{"--dev-plaintext", "--data", dir, "--kms", "hsm"}, passphrase},
		"bad audit export":        {vaultArgs, envMap(map[string]string{envAuditExport: "maybe"})},
	}
	for name, tt := range refused {
		if _, err := parseServerConfig(tt.args, tt.env, io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
