//go:build chaos && !windows

package chaos_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/test/clustertest"
)

const clusterBucket = "chaos"

// scrubFast makes the scrubber of the leading node pass every two seconds,
// unthrottled, so that a test can wait for it.
func scrubFast() clustertest.Options {
	return clustertest.Options{Env: map[string]string{"CACTUSFS_SCRUB_INTERVAL": "2s", "CACTUSFS_SCRUB_RATE_MIB": "0"}}
}

func startCluster(t *testing.T, opts clustertest.Options) (*clustertest.Cluster, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	c := clustertest.Start(t, opts)
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(clusterBucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	return c, ctx
}

func payload(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func httpStatus(err error) int {
	var response *awshttp.ResponseError
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}
	return 0
}

// populateCluster writes n objects of mixed sizes through all four nodes.
func populateCluster(ctx context.Context, t *testing.T, c *clustertest.Cluster, n int) map[string][]byte {
	t.Helper()
	objects := map[string][]byte{}
	for i := range n {
		key, data := fmt.Sprintf("object-%03d", i), payload(t, 1+i*i*9000)
		if err := c.Put(ctx, clustertest.Nodes[i%4], clusterBucket, key, data); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		objects[key] = data
	}
	return objects
}

func verifyCluster(ctx context.Context, t *testing.T, c *clustertest.Cluster, through string, objects map[string][]byte, when string) {
	t.Helper()
	for key, want := range objects {
		got, err := c.Get(ctx, through, clusterBucket, key)
		if err != nil {
			t.Fatalf("%s: Get %s through %s: %v", when, key, through, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: Get %s through %s returned other bytes than were written", when, key, through)
		}
	}
}

// putEventually writes an object through a node, trying again while the
// cluster is finding a leader. It returns how long that took.
func putEventually(ctx context.Context, t *testing.T, c *clustertest.Cluster, through, key string, data []byte, limit time.Duration) time.Duration {
	t.Helper()
	begun := time.Now()
	for {
		err := c.Put(ctx, through, clusterBucket, key, data)
		if err == nil {
			return time.Since(begun)
		}
		if time.Since(begun) > limit {
			t.Fatalf("Put %s through %s still fails after %s: %v", key, through, limit, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func shardFiles(c *clustertest.Cluster, id string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(c.Shell(id, "find /data1/shards /data2/shards -type f | wc -l")))
	return n
}

// rebuilt sums the shards the scrubbers of all nodes report to have rebuilt.
func rebuilt(c *clustertest.Cluster) int {
	total := 0
	pattern := regexp.MustCompile(`"msg":"scrub pass finished".*?"shards_rebuilt":(\d+)`)
	for _, id := range clustertest.Nodes {
		for _, match := range pattern.FindAllStringSubmatch(c.Logs(id), -1) {
			n, _ := strconv.Atoi(match[1])
			total += n
		}
	}
	return total
}

// TestClusterNodeKilledDuringWrites: a node dies, without warning, while
// clients are writing through the others. No write that was acknowledged is
// lost, the cluster keeps accepting writes, and when the node is back the
// scrubber rebuilds on it what it missed - which is then proven by taking
// two other nodes away.
func TestClusterNodeKilledDuringWrites(t *testing.T) {
	t.Parallel()
	c, ctx := startCluster(t, scrubFast())

	var mu sync.Mutex
	acknowledged := map[string][]byte{}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w, through := range []string{"node1", "node3", "node4"} {
		writers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key, data := fmt.Sprintf("w%d-%04d", w, i), payload(t, 20_000+i*997)
				if err := c.Put(ctx, through, clusterBucket, key, data); err == nil {
					mu.Lock()
					acknowledged[key] = data
					mu.Unlock()
				}
			}
		})
	}
	time.Sleep(3 * time.Second)
	c.Kill("node2")
	time.Sleep(6 * time.Second)
	close(stop)
	writers.Wait()

	mu.Lock()
	objects := acknowledged
	mu.Unlock()
	if len(objects) < 20 {
		t.Fatalf("only %d writes were acknowledged", len(objects))
	}
	for _, through := range []string{"node1", "node3", "node4"} {
		verifyCluster(ctx, t, c, through, objects, "with node2 killed")
	}
	// Writes are accepted with a node down.
	if err := c.Put(ctx, "node4", clusterBucket, "after-the-kill", payload(t, 100_000)); err != nil {
		t.Fatalf("Put after the kill: %v", err)
	}

	c.Restart("node2")
	verifyCluster(ctx, t, c, "node2", objects, "through the node that was killed, after its return")
	// Every object has two shards on every node again.
	want := 2 * (len(objects) + 1)
	c.Eventually("the scrubber to rebuild what node2 missed", 3*time.Minute, func() bool { return shardFiles(c, "node2") >= want })
	c.Kill("node1")
	c.Kill("node4")
	verifyCluster(ctx, t, c, "node2", objects, "with node1 and node4 down, after healing")
}

// TestClusterLosesItsLeader: each of the three voters is killed in turn, so
// the leader certainly is. Every time the others elect a new one and writes
// go on within seconds; nothing acknowledged is lost; a read through any node
// never returns an older version than the one last acknowledged.
func TestClusterLosesItsLeader(t *testing.T) {
	t.Parallel()
	c, ctx := startCluster(t, clustertest.Options{})
	objects := populateCluster(ctx, t, c, 8)

	version := 0
	current := payload(t, 4000)
	if err := c.Put(ctx, "node4", clusterBucket, "counter", current); err != nil {
		t.Fatal(err)
	}
	for _, victim := range []string{"node1", "node2", "node3"} {
		c.Kill(victim)
		survivors := []string{}
		for _, id := range clustertest.Nodes {
			if id != victim {
				survivors = append(survivors, id)
			}
		}
		for round := range 6 {
			version++
			next := payload(t, 4000+version)
			through := survivors[round%3]
			took := putEventually(ctx, t, c, through, "counter", next, 40*time.Second)
			if took > 25*time.Second {
				t.Errorf("with %s killed, a write through %s took %s to be accepted", victim, through, took)
			}
			current = next
			// Read-after-write through the other survivors.
			for _, reader := range survivors {
				got, err := c.Get(ctx, reader, clusterBucket, "counter")
				if err == nil && !bytes.Equal(got, current) {
					t.Fatalf("with %s killed: a read through %s returned an older version than the one just acknowledged", victim, reader)
				}
				if err != nil && httpStatus(err) < 500 {
					t.Fatalf("with %s killed: Get through %s: %v", victim, reader, err)
				}
			}
		}
		verifyCluster(ctx, t, c, survivors[0], objects, "with "+victim+" killed")
		c.Restart(victim)
		if got, err := c.Get(ctx, victim, clusterBucket, "counter"); err != nil || !bytes.Equal(got, current) {
			t.Fatalf("after %s returned, a read through it: stale or failed: %v", victim, err)
		}
	}
}

// TestClusterPartition: a node is cut off from the others while clients can
// still reach it. It must not answer from what it knew: an object that was
// overwritten on the other side of the partition must never be served in its
// old version, and no write may be acknowledged that the rest of the cluster
// does not have. Each voter is cut off in turn, so the leader is too.
func TestClusterPartition(t *testing.T) {
	t.Parallel()
	c, ctx := startCluster(t, clustertest.Options{})
	objects := populateCluster(ctx, t, c, 6)

	old := payload(t, 30_000)
	if err := c.Put(ctx, "node4", clusterBucket, "contested", old); err != nil {
		t.Fatal(err)
	}
	for _, cut := range []string{"node1", "node2", "node3"} {
		// It has the current version, and serves it.
		if got, err := c.Get(ctx, cut, clusterBucket, "contested"); err != nil || !bytes.Equal(got, old) {
			t.Fatalf("Get through %s before the partition: %v", cut, err)
		}
		c.Isolate(cut)

		// The majority goes on, and overwrites the object.
		others := []string{}
		for _, id := range clustertest.Nodes {
			if id != cut {
				others = append(others, id)
			}
		}
		fresh := payload(t, 30_000)
		putEventually(ctx, t, c, others[0], "contested", fresh, 40*time.Second)

		// The node that is cut off is asked, again and again. Whatever it
		// answers, it is never the old version.
		const asked = 6
		stale, refused := 0, 0
		for range asked {
			got, err := c.Get(ctx, cut, clusterBucket, "contested")
			switch {
			case err == nil && bytes.Equal(got, old):
				stale++
			case err == nil && !bytes.Equal(got, fresh):
				t.Fatalf("%s, cut off, returned bytes that were never written", cut)
			case err != nil:
				refused++
			}
			time.Sleep(150 * time.Millisecond)
		}
		if stale > 0 {
			t.Fatalf("%s, cut off from the cluster, served a version that had been overwritten (%d of %d reads)", cut, stale, asked)
		}
		if refused == 0 {
			t.Errorf("%s, cut off from the cluster, answered every read", cut)
		}
		// It acknowledges no write.
		err := c.Put(ctx, cut, clusterBucket, "written-in-isolation", payload(t, 1000))
		if err == nil {
			t.Fatalf("%s, cut off from the cluster, acknowledged a write", cut)
		}
		if status := httpStatus(err); status != 0 && status < http.StatusInternalServerError {
			t.Errorf("%s, cut off: Put failed with status %d, want a 5xx", cut, status)
		}
		verifyCluster(ctx, t, c, others[1], objects, "on the majority side, with "+cut+" cut off")

		// The partition heals. The node catches up and serves the new
		// version; the write it refused does not exist.
		c.Rejoin(cut)
		c.Eventually(cut+" to serve the current version after the partition", 90*time.Second, func() bool {
			got, err := c.Get(ctx, cut, clusterBucket, "contested")
			if err == nil && bytes.Equal(got, old) {
				t.Fatalf("%s served the overwritten version after rejoining", cut)
			}
			return err == nil && bytes.Equal(got, fresh)
		})
		if _, err := c.Get(ctx, others[0], clusterBucket, "written-in-isolation"); httpStatus(err) != http.StatusNotFound {
			t.Errorf("the write refused by %s exists: %v", cut, err)
		}
		verifyCluster(ctx, t, c, cut, objects, "through "+cut+" after the partition healed")
		old = fresh
	}
}

// TestClusterWithTwoNodesDown: with two of four nodes down, half the shards
// of every object are gone. Every object is still read. No write is
// accepted, because it could not be made durable on a write quorum - and
// none is acknowledged. When the nodes return, writes work again.
func TestClusterWithTwoNodesDown(t *testing.T) {
	t.Parallel()
	c, ctx := startCluster(t, clustertest.Options{})
	objects := populateCluster(ctx, t, c, 10)

	// Two nodes that leave the metadata group a majority: one voter, and
	// the node without a vote.
	c.Kill("node3")
	c.Kill("node4")
	for _, through := range []string{"node1", "node2"} {
		verifyCluster(ctx, t, c, through, objects, "with node3 and node4 down")
	}
	for i := range 5 {
		err := c.Put(ctx, "node1", clusterBucket, fmt.Sprintf("refused-%d", i), payload(t, 50_000))
		if status := httpStatus(err); err == nil || status != http.StatusServiceUnavailable {
			t.Fatalf("Put with two nodes down: %v (status %d), want 503", err, status)
		}
	}
	listed, err := c.S3("node2").ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(clusterBucket)})
	if err != nil || len(listed.Contents) != len(objects) {
		t.Fatalf("after refused writes the bucket lists %d objects, %v; want %d", len(listed.Contents), err, len(objects))
	}

	c.StartNode("node3")
	c.StartNode("node4")
	c.WaitReady("node3", "node4")
	putEventually(ctx, t, c, "node3", "accepted-again", payload(t, 50_000), 30*time.Second)
	verifyCluster(ctx, t, c, "node4", objects, "after the nodes returned")
}

// TestClusterHealsCorruptionAndLoss: bitrot on every shard of one node, and
// every shard file deleted on another. Nobody reads the objects. The
// scrubber of the leading node finds both through node RPC and rewrites all
// of it; then the two nodes that were never damaged are taken away, and
// every object is read from the healed shards alone.
func TestClusterHealsCorruptionAndLoss(t *testing.T) {
	t.Parallel()
	c, ctx := startCluster(t, scrubFast())
	objects := populateCluster(ctx, t, c, 12)
	c.Eventually("a first scrub pass", 2*time.Minute, func() bool {
		for _, id := range clustertest.Nodes {
			if strings.Contains(c.Logs(id), `"msg":"scrub pass finished"`) {
				return true
			}
		}
		return false
	})
	before := rebuilt(c)

	// Flip a byte in the middle of every shard file on node2.
	c.Shell("node2", `for f in $(find /data1/shards /data2/shards -type f); do
		size=$(stat -c %s "$f"); printf '\377' | dd of="$f" bs=1 seek=$((size / 2)) conv=notrunc 2>/dev/null; done`)
	// Delete every shard file on node3.
	// (The scrubber may have begun to put them back by the time anyone
	// could count them.)
	c.Shell("node3", "find /data1/shards /data2/shards -type f -delete")

	want := 4 * len(objects) // two shards of every object on each of the two nodes
	c.Eventually("the scrubber to rebuild the damaged and deleted shards", 4*time.Minute, func() bool {
		return rebuilt(c)-before >= want && shardFiles(c, "node3") == 2*len(objects)
	})

	c.Kill("node1")
	c.Kill("node4")
	// node2 and node3 are two of three voters: the metadata group lives, and
	// their four shards per object are exactly the ones that were rebuilt.
	c.Eventually("node2 to serve without node1", time.Minute, func() bool {
		_, err := c.Get(ctx, "node2", clusterBucket, "object-000")
		return err == nil
	})
	verifyCluster(ctx, t, c, "node2", objects, "from the healed shards alone")
	verifyCluster(ctx, t, c, "node3", objects, "from the healed shards alone")
}
