//go:build integration && rollingupgrade && !windows

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/test/clustertest"
)

// schema4Commit is the commit whose release the upgrade to schema 5 starts
// from: the one whose binary wrote the fixture testdata/schema-4, and whose
// MetadataVersion is 4.
const schema4Commit = "84334a7"

// schema5Only names the buckets of bbolt that only an entry of schema 5
// makes (ADR-0023, "New in the state").
func schema5Only() []string {
	return []string{"bucketusage", "outbox", "outboxcursor", "resync", "targetrefs", "targets"}
}

// schema5MemberOf reads a state database a stopped node left in its volume.
func schema5MemberOf(t *testing.T, state []byte) schema5State {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.db"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	return schema5StateOf(t, dir)
}

// schema5Written is an object a rolling upgrade wrote, to be read back.
type schema5Written struct {
	bucket, key, sum string
	size             int
}

// TestRollingUpgradeFromSchema4ToSchema5: a cluster of the release of schema
// 4 is upgraded to this release one node at a time (ADR-0023, "Level rule
// and state schema", "Rollback"), with the binaries themselves: the three
// voters start from the image $CACTUSFS_TEST_IMAGE_ROLLING_FROM names - an
// image of commit 84334a7, whose MetadataVersion is 4 and whose binary wrote
// the fixture of schema 4 - and the fourth node from the image under test.
// Nothing stands in for the release before.
//
//	make test-rolling
//
// which is
//
//	git archive 84334a7 | docker build -t cactusfs-test:rolling-from -
//	CACTUSFS_TEST_IMAGE_ROLLING_FROM=cactusfs-test:rolling-from CACTUSFS_TEST_ROLLING_FROM_COMMIT=84334a7 \
//	    go test -tags='integration rollingupgrade' -run 'TestRollingUpgradeFromSchema4ToSchema5' -count=1 -timeout 40m ./test/integration/
//
// It is behind a build tag of its own because that image cannot be made
// from this tree: it needs the history of the repository, which an export of
// the source does not have. Without the image named, or with an image of
// another commit, it fails; it is never skipped.
//
// While any member is of the release before - through the node of this
// release, through a node of that one, and through each node as soon as it
// is upgraded: the deployment says it is at level 4; a bucket is created, an
// object put, read and deleted, a multipart upload completed - all succeed;
// what was written before is read back as the bytes it was; a quota is not
// taken; and the state of every member that is stopped - the node of this
// release, and each node of the release before as it leaves - says schema 4
// and holds nothing only schema 5 makes.
//
// When the last has registered that it knows schema 5 the deployment says
// level 5, and still the state says 4: registering is no entry of schema 5.
// The first bucket that is created is. From then on the state of every
// member says 5 and holds the usage record of that bucket, exact and the
// same bytes on every member - also after a change of leader, under which a
// second bucket is counted like the first - and everything that was written
// at level 4 is still the bytes it was.
//
// What this build does not do yet, and so is not asserted - each is of the
// step that brings it, which changes the lines that say so below:
//
//   - a quota is answered 501, not 503, below level 5, and 501 above it: the
//     use case of PUT /buckets/{bucket}/quota is PLAN 8.3.3's. What is held
//     here is that it is never taken below level 5, and never "unavailable"
//     above it;
//   - a bucket that was there before the upgrade has no usage record after
//     it: nothing counts without being asked before the worker of PLAN 8.3.1.
//     That the walk counts such a bucket to what it holds is proved on the
//     state of the fixture (TestUpgradeFromSchema4,
//     TestTheSchema4FixtureIsCountedToItsManifest) and on stores that stand
//     in for the two releases (TestRollingUpgradeToSchema5).
func TestRollingUpgradeFromSchema4ToSchema5(t *testing.T) {
	old := rollingFrom(t, schema4Commit)
	// Not beside the others: one cluster a test file, and every step of
	// this one waits for a node.
	nodes, voters := clustertest.Nodes, clustertest.Nodes[:3]
	images := map[string]string{}
	for _, id := range voters {
		images[id] = old
	}
	c := clustertest.Start(t, clustertest.Options{Images: images})
	for _, id := range voters {
		if c.ImageOf(id) != old {
			t.Fatalf("%s runs %s, want %s", id, c.ImageOf(id), old)
		}
	}
	if c.ImageOf("node4") == old {
		t.Fatalf("the image under test is %s, the image of the release before: nothing would be upgraded", old)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	root := gridBearer(c.AccessKey, c.SecretKey)
	// What a node of this tree registers, and what one of the release
	// before does.
	const mine, before = raftmeta.MetadataVersion, 4
	if mine != 5 {
		t.Fatalf("this tree is of metadata version %d: this test is of the upgrade to schema 5", mine)
	}
	levels := map[string]int{"node4": mine}
	for _, id := range voters {
		levels[id] = before
	}
	registered := func() {
		t.Helper()
		var got map[string]int
		var err error
		deadline := time.Now().Add(2 * time.Minute)
		for {
			if _, got, err = schema4Cluster(ctx, c, "node4", root); err == nil && maps.Equal(got, levels) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the metadata versions the nodes registered = %v (%v), want %v", got, err, levels)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	// level waits until a node of this release says the deployment is at
	// the level given: what the node itself would write at.
	level := func(id string, want int) {
		t.Helper()
		c.Eventually(fmt.Sprintf("%s to say the deployment is at level %d", id, want), 2*time.Minute, func() bool {
			view, _, err := settingsRead(t, c, id, root)
			return err == nil && view.Cluster.MetadataLevel == want
		})
	}
	// answering waits until a node answers the list of buckets: it has then
	// applied everything the list was read behind.
	answering := func(id string) {
		t.Helper()
		c.Eventually(id+" to answer", 2*time.Minute, func() bool {
			status, _, _, err := gridCall(ctx, c, id, root, http.MethodGet, "/buckets", nil)
			return err == nil && status == http.StatusOK
		})
	}

	var written []schema5Written
	put := func(id, bucketName, key string, size int) []byte {
		t.Helper()
		content := randomBytes(t, size)
		if err := c.Put(ctx, id, bucketName, key, content); err != nil {
			t.Fatalf("Put %s/%s through %s: %v", bucketName, key, id, err)
		}
		written = append(written, schema5Written{bucket: bucketName, key: key, sum: sumOf(content), size: size})
		return content
	}
	// readBack reads, through a node, everything that was written so far,
	// and holds it to the bytes it was.
	readBack := func(when, id string) {
		t.Helper()
		for _, w := range written {
			got, err := c.Get(ctx, id, w.bucket, w.key)
			if err != nil || len(got) != w.size || sumOf(got) != w.sum {
				t.Errorf("%s: %s/%s through %s is %d bytes, %v; want the %d that were written", when, w.bucket, w.key, id, len(got), err, w.size)
			}
		}
	}
	// use is everything that worked before schema 5, through one node: a
	// bucket created, an object put and read, one put and deleted, and a
	// multipart upload completed.
	var created []string
	use := func(id, name string) {
		t.Helper()
		client := c.S3(id)
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
			t.Fatalf("CreateBucket through %s at level 4: %v", id, err)
		}
		created = append(created, name)
		content := put(id, name, "k", 100_000)
		if got, err := c.Get(ctx, id, name, "k"); err != nil || !bytes.Equal(got, content) {
			t.Fatalf("Get through %s at level 4: %d bytes, %v", id, len(got), err)
		}
		if err := c.Put(ctx, id, name, "gone", []byte("soon")); err != nil {
			t.Fatalf("Put through %s at level 4: %v", id, err)
		}
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String("gone")}); err != nil {
			t.Fatalf("DeleteObject through %s at level 4: %v", id, err)
		}
		upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(name), Key: aws.String("multi")})
		if err != nil {
			t.Fatalf("CreateMultipartUpload through %s at level 4: %v", id, err)
		}
		reel := randomBytes(t, 70_000)
		part, err := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(name), Key: aws.String("multi"), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(reel)})
		if err != nil {
			t.Fatalf("UploadPart through %s at level 4: %v", id, err)
		}
		if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(name), Key: aws.String("multi"), UploadId: upload.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}},
		}); err != nil {
			t.Fatalf("CompleteMultipartUpload through %s at level 4: %v", id, err)
		}
		written = append(written, schema5Written{bucket: name, key: "multi", sum: sumOf(reel), size: len(reel)})
	}
	// quota asks a node of this release to give a bucket a quota, and
	// returns the status of the answer.
	quota := func(id, name string) int {
		t.Helper()
		status, _, body, err := gridCall(ctx, c, id, root, http.MethodPut, "/buckets/"+name+"/quota", map[string]any{"quota_bytes": 1 << 30})
		if err != nil {
			t.Fatalf("PUT the quota of %s through %s: %v", name, id, err)
		}
		if status >= 200 && status < 300 {
			t.Errorf("PUT the quota of %s through %s = %d %s", name, id, status, body)
		}
		return status
	}
	// notTaken says that a quota is not taken below level 5: 503 once the
	// use case is there (PLAN 8.3.3), 501 until then.
	notTaken := func(id, name string) {
		t.Helper()
		if status := quota(id, name); status != http.StatusNotImplemented && status != http.StatusServiceUnavailable {
			t.Errorf("at level 4, PUT the quota of %s through %s = %d; want 503, or 501 while the use case is not written", name, id, status)
		}
	}
	// below says of a state database that it is one the release of schema 4
	// opens: of schema 4, with nothing only schema 5 makes.
	below := func(when, id string, state schema5State) {
		t.Helper()
		if state.Schema != before || len(state.Usage) != 0 {
			t.Errorf("%s: the state of %s says schema %d and holds %d usage records; want %d and none", when, id, state.Schema, len(state.Usage), before)
		}
		for _, name := range schema5Only() {
			if slices.Contains(state.Buckets, name) {
				t.Errorf("%s: the state of %s holds %q, which only schema 5 makes", when, id, name)
			}
		}
	}

	// Three voters of the release before, and the node of this release.
	registered()
	level("node4", before)
	use("node4", "level-four")
	use(voters[0], "level-four-old")
	notTaken("node4", "level-four")
	readBack("at level 4", "node4")
	readBack("at level 4", voters[1])
	// What the node of this release holds: stopped, read, started again.
	answering("node4")
	c.Stop("node4")
	below("at level 4", "node4", schema5MemberOf(t, c.MetaFile("node4", "state.db")))
	c.Restart("node4")

	for i, id := range voters {
		// What the node of the release before holds when it is stopped
		// for good - once it has answered, and so has applied what there
		// is.
		answering(id)
		c.Stop(id)
		below(fmt.Sprintf("upgrade step %d", i), id, schema5MemberOf(t, c.MetaFile(id, "state.db")))
		c.Upgrade(id, "")
		c.WaitReady(id)
		if c.ImageOf(id) == old {
			t.Fatalf("%s still runs %s after its upgrade", id, old)
		}
		levels[id] = mine
		registered()
		if i == len(voters)-1 {
			break
		}
		// Still held by the members that are left: through the node that
		// was just upgraded, which says so itself.
		level(id, before)
		level("node4", before)
		use(id, "still-four-"+id)
		notTaken(id, "still-four-"+id)
		readBack(fmt.Sprintf("upgrade step %d", i), id)
		readBack(fmt.Sprintf("upgrade step %d", i), voters[len(voters)-1])
	}

	// Every member has registered that it knows schema 5: the deployment is
	// at level 5, as each node says of itself - and each must have seen it
	// before it is asked to write, or it writes as at level 4. Nothing of
	// schema 5 was written: registering is no entry of it, and this build
	// counts nothing without being asked (PLAN 8.3.1).
	for _, id := range nodes {
		level(id, mine)
	}
	readBack("at level 5, before anything of schema 5", "node1")
	answering("node4")
	c.Stop("node4")
	below("at level 5, before the first entry of schema 5", "node4", schema5MemberOf(t, c.MetaFile("node4", "state.db")))
	c.Restart("node4")
	answering("node4")
	level("node4", mine)

	// The first bucket that is created is the first entry of schema 5: it
	// is counted from the start, through whichever node its changes come.
	const first, second = "level-five", "level-five-after"
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(first)}); err != nil {
		t.Fatalf("CreateBucket through node1 at level 5: %v", err)
	}
	want := map[string]bucket.UsageTotals{}
	put("node2", first, "a", 100_000)
	if err := c.Put(ctx, "node3", first, "b", []byte("soon gone")); err != nil {
		t.Fatalf("Put through node3 at level 5: %v", err)
	}
	if _, err := c.S3("node4").DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(first), Key: aws.String("b")}); err != nil {
		t.Fatalf("DeleteObject through node4 at level 5: %v", err)
	}
	pending, err := c.S3("node4").CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(first), Key: aws.String("pending")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload through node4 at level 5: %v", err)
	}
	if _, err := c.S3("node2").UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(first), Key: aws.String("pending"), UploadId: pending.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(randomBytes(t, 70_000)),
	}); err != nil {
		t.Fatalf("UploadPart through node2 at level 5: %v", err)
	}
	want[first] = bucket.UsageTotals{Objects: 1, CurrentBytes: 100_000, Versions: 1, StoredBytes: 100_000, UploadBytes: 70_000}
	// The cluster is at level 5: whatever a quota is answered with, it is
	// not "unavailable". (501 until the use case is written, PLAN 8.3.3.)
	if status := quota("node4", first); status == http.StatusServiceUnavailable {
		t.Errorf("at level 5, PUT the quota of %s = %d", first, status)
	}
	for _, id := range nodes {
		readBack("after the first entry of schema 5", id)
	}

	// leading waits until the metadata group has a leader and returns it.
	// It asks the node that never leads.
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
	// stopped stops a node, when it has answered, and returns what its
	// state database holds.
	stopped := func(id string) schema5State {
		t.Helper()
		answering(id)
		c.Stop(id)
		return schema5MemberOf(t, c.MetaFile(id, "state.db"))
	}
	// alike says of the members that their states say schema 5 and hold
	// the usage records of exactly the buckets made at level 5 - each exact,
	// of the totals wanted, and the same bytes on every member - and none of
	// a bucket that was there before.
	alike := func(when string, members map[string]schema5State, counted ...string) {
		t.Helper()
		reference := members[nodes[0]]
		for _, id := range nodes {
			member, read := members[id]
			if !read {
				t.Fatalf("%s: the state of %s was not read", when, id)
			}
			if member.Schema != uint64(mine) {
				t.Errorf("%s: the state of %s says schema %d, want %d", when, id, member.Schema, mine)
			}
			if got := slices.Sorted(maps.Keys(member.Usage)); !slices.Equal(got, slices.Sorted(slices.Values(counted))) {
				t.Errorf("%s: the state of %s holds the usage records of %v; want those of %v, the buckets made at level 5, and of no bucket of before (%v)",
					when, id, got, counted, created)
			}
			if !maps.Equal(member.Usage, reference.Usage) {
				t.Errorf("%s: bucketusage on %s:\n%v\non %s:\n%v", when, id, member.Usage, nodes[0], reference.Usage)
			}
			for _, name := range counted {
				usage, err := bucket.ParseUsage([]byte(member.Usage[name]))
				if err != nil {
					t.Errorf("%s: the usage record of %s on %s: %v (%s)", when, name, id, err, member.Usage[name])
					continue
				}
				if !usage.Exact || usage.Walking() || usage.UsageTotals != want[name] {
					t.Errorf("%s: on %s the bucket %s holds exact = %v, %+v; want %+v", when, id, name, usage.Exact, usage.UsageTotals, want[name])
				}
			}
		}
	}
	// Every member holds so: each is stopped in turn and started again, but
	// for the leader of the metadata group, which is stopped last and stays
	// so - that is the change of leader.
	members := map[string]schema5State{}
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
	alike("at level 5", members, first)

	// Another node leads. Through the node that never does: a second
	// bucket, counted like the first, and an object more in the first. Each
	// is asked until it succeeds - the node may yet have to find out who
	// leads - and an attempt whose answer was lost leaves what its
	// repetition leaves: a bucket there is, a key with one object.
	var successor string
	c.Eventually("another node to lead the metadata group", 2*time.Minute, func() bool {
		var err error
		successor, _, err = schema4Cluster(ctx, c, "node4", root)
		return err == nil && slices.Contains(nodes, successor) && successor != leader
	})
	c.Eventually("a bucket to be created under "+successor, 2*time.Minute, func() bool {
		_, err := c.S3("node4").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(second)})
		return err == nil || apiCode(err) == "BucketAlreadyOwnedByYou" || apiCode(err) == "BucketAlreadyExists"
	})
	late := randomBytes(t, 30_000)
	for _, name := range []string{second, first} {
		c.Eventually("an upload into "+name+" under "+successor, 2*time.Minute, func() bool {
			return c.Put(ctx, "node4", name, "late", late) == nil
		})
		written = append(written, schema5Written{bucket: name, key: "late", sum: sumOf(late), size: len(late)})
	}
	want[second] = bucket.UsageTotals{Objects: 1, CurrentBytes: 30_000, Versions: 1, StoredBytes: 30_000}
	want[first] = bucket.UsageTotals{Objects: 2, CurrentBytes: 130_000, Versions: 2, StoredBytes: 130_000, UploadBytes: 70_000}

	// The old leader returns, and holds what the others do.
	c.Restart(leader)
	members = map[string]schema5State{}
	for _, id := range nodes {
		members[id] = stopped(id)
		c.Restart(id)
	}
	alike("after a change of leader", members, first, second)
	// With every member stopped and started once more, everything that was
	// written - at level 4, during the upgrade and at level 5 - is the bytes
	// it was, through every node.
	for _, id := range nodes {
		answering(id)
		readBack("after every member was restarted", id)
	}
	if len(written) < 11 || len(created) < 4 {
		t.Errorf("%d objects in %d buckets of before were read back: the test proves less than it says", len(written), len(created))
	}
}
