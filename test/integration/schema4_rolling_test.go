//go:build integration && rollingupgrade && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/test/clustertest"
)

// The release a rolling upgrade starts from, as `make test-rolling` hands it
// to the tests: the image it built from the history of the repository, and
// the commit it built it from (ROLLING_FROM_COMMIT).
const (
	rollingFromImageEnv  = "CACTUSFS_TEST_IMAGE_ROLLING_FROM"
	rollingFromCommitEnv = "CACTUSFS_TEST_ROLLING_FROM_COMMIT"
	// schema3Commit is the last commit whose MetadataVersion is 3: the
	// release the upgrade to schema 4 starts from.
	schema3Commit = "0b7d825"
	// rollingCommitLength is the fewest characters a commit is named by.
	rollingCommitLength = 7
)

// rollingFrom returns the image of the release a rolling upgrade starts
// from. A test of a rolling upgrade is about one release before - what it
// asserts is what THAT release registers, writes and answers - so it names
// the commit, and fails when no image is named or the image is of another
// commit. It is never skipped.
func rollingFrom(t *testing.T, commit string) string {
	t.Helper()
	image, built := os.Getenv(rollingFromImageEnv), os.Getenv(rollingFromCommitEnv)
	if image == "" {
		t.Fatalf("%s names no image: this test upgrades a cluster of the release of commit %s, and needs an image of it (make test-rolling ROLLING_FROM_COMMIT=%s ROLLING_TEST=%s)",
			rollingFromImageEnv, commit, commit, t.Name())
	}
	// Either may be the short form of the other, of seven characters or
	// more: a shorter one names no commit.
	if len(built) < rollingCommitLength || len(commit) < rollingCommitLength || (!strings.HasPrefix(built, commit) && !strings.HasPrefix(commit, built)) {
		t.Fatalf("%s says the image %s is of commit %q: this test is of the upgrade from commit %s (make test-rolling ROLLING_FROM_COMMIT=%s ROLLING_TEST=%s)",
			rollingFromCommitEnv, image, built, commit, commit, t.Name())
	}
	return image
}

// schema4Downloads returns how often the record of an object (ADR-0016) says
// it was downloaded, asked of a node.
func schema4Downloads(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string, bucketName, key string) (int64, error) {
	status, _, body, err := gridCall(ctx, c, id, root, http.MethodGet, "/buckets/"+bucketName+"/object/access?key="+url.QueryEscape(key), nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("the record of %s/%s through %s = %d %s", bucketName, key, id, status, body)
	}
	var access objectAccess
	if err := json.Unmarshal(body, &access); err != nil {
		return 0, fmt.Errorf("the record of %s/%s through %s: %w", bucketName, key, id, err)
	}
	return access.DownloadCount, nil
}

// schema4HeldAt3 is the first half of a rolling upgrade to schema 4, on a
// cluster whose three voters run the release of schema 3 and whose fourth
// node runs this one. While any member is of the release before, the cluster
// is at level 3: through the node of this release, and through each node as
// soon as it is upgraded, a bucket is created, an object put and read - all
// succeed, the bucket has no creator, no record of a bucket's use is kept,
// and the state says schema 3, in the database of this release's node and in
// those the nodes of the release before leave behind. The voters are
// upgraded one at a time, each serving before the next is stopped.
//
// It returns when the last of them serves, with the names of the buckets it
// created: what schema4Recorded takes as created below level 4.
func schema4HeldAt3(t *testing.T, c *clustertest.Cluster, voters []string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	root := gridBearer(c.AccessKey, c.SecretKey)
	content := randomBytes(t, 100_000)
	// What a node of this tree registers: the newest schema it applies,
	// which is the fourth or a later one.
	const mine = raftmeta.MetadataVersion
	levels := map[string]int{"node4": mine}
	for _, id := range voters {
		levels[id] = 3
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
	var created []string
	// use creates a bucket, puts an object and reads it, all through one
	// node, and waits until the read is in the record of the object - a
	// batch of reads was applied - so that nothing of it is still on its
	// way when the cluster rises. Then it asks the node of this release
	// what the grid says of every bucket created so far: nothing.
	use := func(id, name string) {
		t.Helper()
		if _, err := c.S3(id).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
			t.Fatalf("CreateBucket through %s at level 3: %v", id, err)
		}
		created = append(created, name)
		if err := c.Put(ctx, id, name, "k", content); err != nil {
			t.Fatalf("Put through %s at level 3: %v", id, err)
		}
		if got, err := c.Get(ctx, id, name, "k"); err != nil || !bytes.Equal(got, content) {
			t.Fatalf("Get through %s at level 3: %d bytes, %v", id, len(got), err)
		}
		c.Eventually("the download through "+id+" to be recorded for the object", time.Minute, func() bool {
			downloads, err := schema4Downloads(ctx, c, "node4", root, name, "k")
			if err != nil {
				t.Fatal(err)
			}
			return downloads == 1
		})
		buckets, body, err := gridList(ctx, c, "node4", root)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range created {
			uploads, downloads, shown := buckets[name].activity()
			if !shown || buckets[name].creator() != "" || len(uploads)+len(downloads) != 0 {
				t.Errorf("at level 3, after %s was used through %s, the grid says of %s: %s; want no creator and no row", name, id, name, body)
			}
		}
	}
	// below says of a state database that it is one of schema 3: no
	// creator in any bucket record, no place for records of buckets' use.
	below := func(id string, member schema4Member) {
		t.Helper()
		if member.Schema != 3 || member.Kept || len(member.Buckets) != len(created) {
			t.Errorf("at level 3 the state of %s says schema %d, a place for records of buckets' use: %t, %d buckets; want 3, none, %d", id, member.Schema, member.Kept, len(member.Buckets), len(created))
		}
		for name, record := range member.Buckets {
			if strings.Contains(record, "created_by") {
				t.Errorf("at level 3 the state of %s holds bucket %s as %s", id, name, record)
			}
		}
	}

	registered()
	// Through the node of this release, whose use case names the creator
	// and whose store leaves it out; and through a node of the release
	// before, which knows of none.
	use("node4", "level-three")
	use(voters[0], "level-three-old")
	// A node of the release before answers the list as it always did.
	status, _, body, err := gridCall(ctx, c, voters[0], root, http.MethodGet, "/buckets?details=true", nil)
	if err != nil || status != http.StatusOK || bytes.Contains(body, []byte(`"details"`)) || !bytes.Contains(body, []byte("level-three")) {
		t.Errorf("the buckets through a node of the release before = %d %s (%v); want the list without details", status, body, err)
	}
	// What the node of this release holds: stopped, read, started again.
	c.Stop("node4")
	below("node4", schema4MemberOf(t, c.MetaFile("node4", "state.db")))
	c.Restart("node4")

	for i, id := range voters {
		// What the node of the release before holds when it is stopped
		// for good - once it has answered, and so has applied what there
		// is.
		c.Eventually(id+" to answer", 2*time.Minute, func() bool {
			status, _, _, err := gridCall(ctx, c, id, root, http.MethodGet, "/buckets", nil)
			return err == nil && status == http.StatusOK
		})
		c.Stop(id)
		below(id, schema4MemberOf(t, c.MetaFile(id, "state.db")))
		c.Upgrade(id, "")
		c.WaitReady(id)
		levels[id] = mine
		registered()
		if i < len(voters)-1 {
			// Still held by the members that are left: through the
			// node that was just upgraded.
			use(id, "still-three-"+id)
		}
	}
	return created
}

// TestRollingUpgradeFromSchema3ToSchema4: a cluster of the release of
// schema 3 is upgraded to this release one node at a time (ADR-0019), with
// the binaries themselves: the three voters start from the image
// $CACTUSFS_TEST_IMAGE_ROLLING_FROM names - an image of commit 0b7d825, the
// last whose MetadataVersion is 3 - and the fourth node from the image under
// test. Nothing stands in for the release before.
//
//	make test-rolling ROLLING_FROM_COMMIT=0b7d825 ROLLING_TEST=TestRollingUpgradeFromSchema3ToSchema4
//
// which is
//
//	git archive 0b7d825 | docker build -t cactusfs-test:rolling-from -
//	CACTUSFS_TEST_IMAGE_ROLLING_FROM=cactusfs-test:rolling-from CACTUSFS_TEST_ROLLING_FROM_COMMIT=0b7d825 \
//	    go test -tags='integration rollingupgrade' -run 'TestRollingUpgradeFromSchema3ToSchema4' -count=1 -timeout 40m ./test/integration/
//
// This tree is of a later schema than the fourth: its nodes register that,
// the cluster rises past 4 when the last voter has been upgraded, and what
// is asserted of the level and of the states from then on is the schema of
// this tree (schema4Recorded): every node is of it by then.
// The upgrade from the release of schema 4 is
// TestRollingUpgradeFromSchema4ToSchema5.
//
// It is behind a build tag of its own because that image cannot be made
// from this tree: it needs the history of the repository, which an export of
// the source does not have. Without the image named it fails; it is never
// skipped.
//
// While any member is of the release before: create, put and get succeed,
// no bucket has a creator, no record is kept, the state says 3
// (schema4HeldAt3). When the last has registered that it knows schema 4, or
// a later one: the creator is recorded, the record is written, and it is the same on every
// member, also after a change of leader (schema4Recorded) - while the
// buckets of before stay as they were.
func TestRollingUpgradeFromSchema3ToSchema4(t *testing.T) {
	old := rollingFrom(t, schema3Commit)
	t.Parallel()
	voters := clustertest.Nodes[:3]
	images := map[string]string{}
	for _, id := range voters {
		images[id] = old
	}
	// The shortest interval there is: the test waits for it.
	c := clustertest.Start(t, clustertest.Options{Env: map[string]string{"CACTUSFS_ACCESS_FLUSH_INTERVAL": "5s"}, Images: images})
	for _, id := range voters {
		if c.ImageOf(id) != old {
			t.Fatalf("%s runs %s, want %s", id, c.ImageOf(id), old)
		}
	}
	if c.ImageOf("node4") == old {
		t.Fatalf("the image under test is %s, the image of the release before: nothing would be upgraded", old)
	}
	created := schema4HeldAt3(t, c, voters)
	schema4Recorded(t, c, "level-four", created)
}
