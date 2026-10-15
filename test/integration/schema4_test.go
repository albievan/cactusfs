//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/test/clustertest"
)

// schema4Member is what the state database of one member of the metadata
// group holds of what schema 4 keeps, as bytes: what members must agree on
// is what is stored, not what a read path makes of it.
type schema4Member struct {
	// Schema is the schema the state says it has.
	Schema uint64
	// Buckets holds the record of every bucket, by name.
	Buckets map[string]string
	// Kept says whether the state has a place for records of buckets' use
	// at all, and Activity holds the records, by bucket.
	Kept     bool
	Activity map[string]string
}

// schema4MemberOf reads a state database a stopped node left in its volume.
func schema4MemberOf(t *testing.T, state []byte) schema4Member {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, state, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open the state database of a member: %v", err)
	}
	defer func() { _ = db.Close() }()
	member := schema4Member{Buckets: map[string]string{}, Activity: map[string]string{}}
	if err := db.View(func(tx *bbolt.Tx) error {
		member.Schema = binary.BigEndian.Uint64(tx.Bucket([]byte("meta")).Get([]byte("schema")))
		if err := tx.Bucket([]byte("buckets")).ForEach(func(name, raw []byte) error {
			member.Buckets[string(name)] = string(raw)
			return nil
		}); err != nil {
			return err
		}
		records := tx.Bucket([]byte("bucketactivity"))
		if records == nil {
			return nil
		}
		member.Kept = true
		return records.ForEach(func(name, raw []byte) error {
			member.Activity[string(name)] = string(raw)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return member
}

// schema4Cluster asks a node what the cluster is: which node leads the
// metadata group, and the metadata version every node registered with.
func schema4Cluster(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string) (leader string, levels map[string]int, err error) {
	status, _, body, err := gridCall(ctx, c, id, root, http.MethodGet, "/cluster", nil)
	if err != nil {
		return "", nil, err
	}
	if status != http.StatusOK {
		return "", nil, fmt.Errorf("the cluster through %s = %d %s", id, status, body)
	}
	var answer struct {
		Leader string `json:"leader"`
		Nodes  []struct {
			ID              string `json:"id"`
			MetadataVersion int    `json:"metadata_version"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", nil, fmt.Errorf("the cluster through %s: %w", id, err)
	}
	levels = map[string]int{}
	for _, node := range answer.Nodes {
		levels[node.ID] = node.MetadataVersion
	}
	return answer.Leader, levels, nil
}

// schema4Recorded is what a cluster whose nodes are all of this tree does
// once every member has registered what this tree applies - schema 4 and the
// schema after it (ADR-0019, ADR-0023) - and the second half of a rolling
// upgrade to it: a bucket has its creator, an upload and a download are in
// the record of the bucket, and every member holds the same - asked through the
// management API of each node, and read from the state database of each
// member while it is stopped. Then the leader of the metadata group is
// stopped: the cluster goes on recording under another, the old leader
// returns and holds what the others hold.
//
// bucketName names the bucket it creates, and a second with "-after" behind
// it. unrecorded names buckets that were created while the cluster was below
// level 4: they have no creator and no record, and that does not change.
func schema4Recorded(t *testing.T, c *clustertest.Cluster, bucketName string, unrecorded []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	root := gridBearer(c.AccessKey, c.SecretKey)
	nodes := clustertest.Nodes
	after := bucketName + "-after"

	// Every node is of this tree when this is asked, and registers what this
	// tree applies: exactly that, which is the fourth schema or a later one.
	// A release of a later schema does everything below as the release of
	// schema 4 did (ADR-0023).
	const mine = raftmeta.MetadataVersion
	c.Eventually(fmt.Sprintf("every node to have registered that it knows schema %d", mine), 2*time.Minute, func() bool {
		_, levels, err := schema4Cluster(ctx, c, "node4", root)
		if err != nil || len(levels) != len(nodes) {
			return false
		}
		for _, level := range levels {
			if level != mine {
				return false
			}
		}
		return true
	})
	// And every node has seen it: a node writes a bucket in the schema it
	// believes the cluster takes, and one that has not yet applied the last
	// registration would write it below - without its creator, or as an
	// entry of a schema below this tree's - for good.
	for _, id := range nodes {
		c.Eventually(fmt.Sprintf("%s to say the deployment is at level %d", id, mine), 2*time.Minute, func() bool {
			view, _, err := settingsRead(t, c, id, root)
			return err == nil && view.Cluster.MetadataLevel == mine
		})
	}

	// awaitDownloads waits until a node says the latest downloads of the
	// bucket are these rows. A node that cannot answer for a moment - there
	// is no leader while one is being elected - is asked again.
	awaitDownloads := func(id, want string) {
		t.Helper()
		var got string
		var err error
		deadline := time.Now().Add(2 * time.Minute)
		for {
			var buckets map[string]gridBucket
			if buckets, _, err = gridList(ctx, c, id, root); err == nil {
				_, downloads, _ := buckets[bucketName].activity()
				if got = gridRows(downloads); got == want {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the latest downloads of %s through %s = %q (%v); waited for %q", bucketName, id, got, err, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	// agreed waits until every node gives the same grid, byte for byte,
	// and returns it. A node that has just returned answers when it has
	// caught up; until then it is asked again.
	agreed := func(when string) map[string]gridBucket {
		t.Helper()
		var bodies map[string][]byte
		var buckets map[string]gridBucket
		deadline := time.Now().Add(2 * time.Minute)
		for {
			bodies = map[string][]byte{}
			same := true
			for _, id := range nodes {
				listed, body, err := gridList(ctx, c, id, root)
				if err != nil {
					same = false
					bodies[id] = []byte(err.Error())
					continue
				}
				bodies[id], buckets = body, listed
				same = same && bytes.Equal(body, bodies[nodes[0]])
			}
			if same {
				return buckets
			}
			if time.Now().After(deadline) {
				for _, id := range nodes {
					t.Errorf("%s: the grid through %s: %s", when, id, bodies[id])
				}
				t.Fatalf("%s: the nodes do not give the same grid", when)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	// answered reports whether a node answers with the grid: it then has
	// applied everything the grid was read behind.
	answered := func(id string) bool {
		_, _, err := gridList(ctx, c, id, root)
		return err == nil
	}
	// stopped stops a node, when it has answered, and returns what its
	// state database holds.
	stopped := func(id string) schema4Member {
		t.Helper()
		c.Eventually(id+" to answer", 2*time.Minute, func() bool { return answered(id) })
		c.Stop(id)
		return schema4MemberOf(t, c.MetaFile(id, "state.db"))
	}
	// alike compares what the members hold of schema 4: the same bucket
	// records and the same records of use, byte for byte, in states that all
	// say the schema of this tree. (A bucket that is created at the level
	// of this tree is an entry of its schema: of the fifth, ADR-0023.)
	alike := func(when string, members map[string]schema4Member) schema4Member {
		t.Helper()
		first := members[nodes[0]]
		for _, id := range nodes {
			member, read := members[id]
			if !read {
				t.Fatalf("%s: the state of %s was not read", when, id)
			}
			if member.Schema != uint64(mine) || !member.Kept {
				t.Errorf("%s: the state of %s says schema %d, a place for records of buckets' use: %t; want %d and one", when, id, member.Schema, member.Kept, mine)
			}
			if !maps.Equal(member.Activity, first.Activity) {
				t.Errorf("%s: bucketactivity on %s:\n%v\non %s:\n%v", when, id, member.Activity, nodes[0], first.Activity)
			}
			if !maps.Equal(member.Buckets, first.Buckets) {
				t.Errorf("%s: the bucket records on %s:\n%v\non %s:\n%v", when, id, member.Buckets, nodes[0], first.Buckets)
			}
		}
		// What was created below level 4 has no creator and no record.
		for _, name := range unrecorded {
			if record, ok := first.Buckets[name]; !ok || strings.Contains(record, "created_by") {
				t.Errorf("%s: the record of %s, created below level 4, is %q", when, name, record)
			}
			if record, ok := first.Activity[name]; ok {
				t.Errorf("%s: %s, used only below level 4, has the record %s", when, name, record)
			}
		}
		return first
	}

	// A bucket through one node, an upload through a second, downloads
	// through the two others - each waited for, so that their order is
	// known.
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Fatalf("CreateBucket through node1: %v", err)
	}
	content := randomBytes(t, 200_000)
	if err := c.Put(ctx, "node2", bucketName, "first", content); err != nil {
		t.Fatalf("Put through node2: %v", err)
	}
	if got, err := c.Get(ctx, "node3", bucketName, "first"); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Get through node3: %d bytes, %v", len(got), err)
	}
	awaitDownloads("node1", "first root; ")
	if got, err := gridPresignedGet(ctx, c, "node4", c.S3("node4"), bucketName, "first"); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("a presigned GET through node4: %d bytes, %v", len(got), err)
	}
	awaitDownloads("node1", "first root presigned; first root; ")

	// Every node says so.
	check := func(when string, buckets map[string]gridBucket, wantUploads, wantDownloads string, created ...string) {
		t.Helper()
		uploads, downloads, shown := buckets[bucketName].activity()
		if !shown || gridRows(uploads) != wantUploads || gridRows(downloads) != wantDownloads {
			t.Errorf("%s: the record of %s: uploads %q, downloads %q; want %q and %q", when, bucketName, gridRows(uploads), gridRows(downloads), wantUploads, wantDownloads)
		}
		for _, name := range created {
			if by := buckets[name].creator(); by != "root" {
				t.Errorf("%s: bucket %s is created by %q, want root", when, name, by)
			}
		}
		for _, name := range unrecorded {
			uploads, downloads, shown := buckets[name].activity()
			if !shown || buckets[name].creator() != "" || len(uploads)+len(downloads) != 0 {
				t.Errorf("%s: bucket %s, created below level 4: creator %q, uploads %q, downloads %q; want none", when, name, buckets[name].creator(), gridRows(uploads), gridRows(downloads))
			}
		}
	}
	check("at level 4", agreed("at level 4"), "first root; ", "first root presigned; first root; ", bucketName)

	// leading waits until the metadata group has a leader and returns it.
	// It asks the node that never leads, which is running whenever it is
	// asked.
	leading := func() string {
		t.Helper()
		var leader string
		c.Eventually("the metadata group to have a leader", 2*time.Minute, func() bool {
			var err error
			leader, _, err = schema4Cluster(ctx, c, "node4", root)
			return err == nil && slices.Contains(nodes, leader)
		})
		return leader
	}
	// And every member holds so: each is stopped in turn and started
	// again, but for the leader of the metadata group, which is stopped
	// last and stays so - that is the change of leader.
	members := map[string]schema4Member{}
	for _, id := range nodes {
		if id != leading() {
			members[id] = stopped(id)
			c.Restart(id)
		}
	}
	leader := leading()
	members[leader] = stopped(leader)
	// (A node that led when its turn came and no longer does.)
	for _, id := range nodes {
		if _, read := members[id]; !read {
			members[id] = stopped(id)
			c.Restart(id)
		}
	}
	held := alike("at level 4", members)
	if record := held.Buckets[bucketName]; !strings.Contains(record, `"created_by":"root"`) {
		t.Errorf("the record of %s on every member is %s; want its creator", bucketName, record)
	}
	if record := held.Activity[bucketName]; !strings.HasPrefix(record, `{"v":1,"uploads":[{"at":"`) || strings.Count(record, `"key":"first"`) != 3 {
		t.Errorf("the record of the use of %s on every member is %s; want one upload and two downloads of its one key", bucketName, record)
	}

	// Another node leads. Through the node that never does - it has no
	// vote - a bucket, an upload and a download, recorded like the others.
	var successor string
	c.Eventually("another node to lead the metadata group", 2*time.Minute, func() bool {
		var err error
		successor, _, err = schema4Cluster(ctx, c, "node4", root)
		return err == nil && slices.Contains(nodes, successor) && successor != leader
	})
	// Each is asked until it succeeds: the node may yet have to find out
	// who leads and which node is gone. An attempt whose answer was lost
	// may have taken effect, and what it leaves is the same - a bucket
	// there is, a key with one row, a reader with one row.
	c.Eventually("a bucket to be created under "+successor, 2*time.Minute, func() bool {
		_, err := c.S3("node4").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(after)})
		return err == nil || apiCode(err) == "BucketAlreadyOwnedByYou" || apiCode(err) == "BucketAlreadyExists"
	})
	c.Eventually("an upload under "+successor, 2*time.Minute, func() bool {
		return c.Put(ctx, "node4", bucketName, "second", content) == nil
	})
	c.Eventually("a download under "+successor, 2*time.Minute, func() bool {
		got, err := c.Get(ctx, "node4", bucketName, "second")
		return err == nil && bytes.Equal(got, content)
	})
	const (
		wantUploads   = "second root; first root; "
		wantDownloads = "second root; first root presigned; first root; "
	)
	awaitDownloads("node4", wantDownloads)

	// The old leader returns, and says and holds what the others do.
	c.Restart(leader)
	check("after a change of leader", agreed("after a change of leader"), wantUploads, wantDownloads, bucketName, after)
	if now, _, err := schema4Cluster(ctx, c, leader, root); err != nil || now == "" {
		t.Errorf("the leader of the metadata group, asked of the returned %s = %q, %v", leader, now, err)
	}
	members = map[string]schema4Member{}
	for _, id := range nodes {
		members[id] = stopped(id)
		c.Restart(id)
	}
	held = alike("after a change of leader", members)
	if record := held.Buckets[after]; !strings.Contains(record, `"created_by":"root"`) {
		t.Errorf("the record of %s, created under %s, is %s on every member; want its creator", after, successor, record)
	}
	if record := held.Activity[bucketName]; strings.Count(record, `"key":"second"`) != 2 || strings.Count(record, `"key":"first"`) != 3 {
		t.Errorf("the record of the use of %s on every member is %s; want what was recorded under both leaders", bucketName, record)
	}
	if record, ok := held.Activity[after]; ok {
		t.Errorf("%s, which nobody used, has the record %s", after, record)
	}
	// With every member stopped and started once more, the grid is what
	// it was.
	check("after every member was restarted", agreed("after every member was restarted"), wantUploads, wantDownloads, bucketName, after)
}

// TestBucketActivityIsTheSameOnEveryMember: a cluster of four nodes of this
// release is at this release's level from its formation - every node
// registers that it knows schema 4, and the schema after it - and from then
// on records who creates a bucket and each
// bucket's latest uploads and downloads (ADR-0019). The record is derived by
// every member from the log, so every member must hold the same: the three
// that vote and the one that does not, asked through their APIs and read
// from their state databases, before and after a change of leader.
//
// This is the half of a rolling upgrade from schema 3 that needs no node of
// the release before. The other half - three members of that release and one
// of this - is TestRollingUpgradeFromSchema3ToSchema4, which needs an image
// of that release and is built with the tag "rollingupgrade"; with members
// that stand in for that release it is TestRollingUpgradeToSchema4 of
// internal/adapters/raftmeta.
func TestBucketActivityIsTheSameOnEveryMember(t *testing.T) {
	t.Parallel()
	// The shortest interval there is: the test waits for it.
	c := clustertest.Start(t, clustertest.Options{Env: map[string]string{"CACTUSFS_ACCESS_FLUSH_INTERVAL": "5s"}})
	schema4Recorded(t, c, "level-four", nil)
}
