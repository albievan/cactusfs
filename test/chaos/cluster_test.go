//go:build chaos && !windows

package chaos_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.etcd.io/bbolt"

	bucketdomain "github.com/albievan/cactusfs/internal/domain/bucket"
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
				// The client reaches the node, and the node says no. A
				// request that never got an answer would prove nothing
				// about what the node does when it is cut off.
				if status := httpStatus(err); status < http.StatusInternalServerError {
					t.Fatalf("%s, cut off: a read ended with %v (status %d); want an answer from the node, and a 5xx", cut, err, status)
				}
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
		if status := httpStatus(err); status < http.StatusInternalServerError {
			t.Errorf("%s, cut off: Put ended with %v (status %d); want an answer from the node, and a 5xx", cut, err, status)
		}
		verifyCluster(ctx, t, c, others[1], objects, "on the majority side, with "+cut+" cut off")

		// The partition heals. The node catches up and serves the new
		// version; the write it refused does not exist.
		c.Rejoin(cut)
		// What the node last answered goes into the failure: "it did not
		// recover" is not something anybody can act on.
		var last error
		rejoined := time.Now()
		c.Eventually(cut+" to serve the current version after the partition", 90*time.Second, func() bool {
			got, err := c.Get(ctx, cut, clusterBucket, "contested")
			if err == nil && bytes.Equal(got, old) {
				t.Fatalf("%s served the overwritten version after rejoining", cut)
			}
			if last = err; err == nil && !bytes.Equal(got, fresh) {
				last = fmt.Errorf("%d bytes that are neither version", len(got))
			}
			if err != nil && time.Since(rejoined) > 80*time.Second {
				t.Logf("%s, %s after rejoining: %v", cut, time.Since(rejoined).Round(time.Second), last)
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

	// Change a byte in the middle of every shard file on node2 - to its
	// complement, whatever it was: a byte written over with a fixed value is
	// not changed where it already had that value, which is one shard in
	// 256, and then a shard short of what this test waits for.
	c.Shell("node2", `for f in $(find /data1/shards /data2/shards -type f); do
		size=$(stat -c %s "$f"); at=$((size / 2))
		was=$(dd if="$f" bs=1 skip=$at count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
		printf "\\$(printf '%03o' $((was ^ 255)))" | dd of="$f" bs=1 seek=$at conv=notrunc 2>/dev/null; done`)
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

const (
	// countedBucket is the bucket of TestClusterLeaderKilledWhileCounting.
	// It keeps versions, and holds nothing but delete markers: a record of
	// the walk each (ADR-0023), made without a shard.
	countedBucket = "counted"
	// countedMarkers is how many: six entries of the walk of
	// app.UsageWalkLimit records, one every 100 ms at the most - 600 ms of
	// counting at the least after the first, where a kill takes a fraction
	// of that. The killed leader's state says whether the kill came in
	// time, and the test tries again when it did not (countedAttempts).
	countedMarkers = 60_000
	// countedBatch is how many keys one DeleteObjects names: far fewer than
	// the thousand S3 takes, since each is a delete of its own - its
	// authorization and a commit through the leader, one after the other -
	// and the answer must come within the 30 s the harness's client waits
	// for one, on a small host with another cluster beside it.
	countedBatch = 50
	// countedDeleters is how many DeleteObjects are sent at once, four
	// through each node.
	countedDeleters = 16
	// countedWithin bounds the whole test: the markers alone are thousands
	// of commits, on a host whose rate nobody measured yet.
	countedWithin = 25 * time.Minute
	// countedAttempts is how often the test asks for a recount and kills
	// the leader before it gives up on killing it while it counts.
	countedAttempts = 3
	// gaugesAfter is how long after a change the gauges of quotas are
	// sure to say what a node's copy of the state held after it: a node
	// shows a summary again for ten seconds (cmd/cactusfs, quotaGaugesReuse),
	// from when it began to read it.
	gaugesAfter = 12 * time.Second
	// usageCounting is the gauge of the buckets whose usage is being
	// counted, and usageDrift the counter of recounts that found other
	// totals than were kept.
	usageCounting = "cactusfs_bucket_usage_counting"
	usageDrift    = "cactusfs_bucket_usage_drift_total"
)

// clusterAdmin makes one call, as root, to the admin listener of a node -
// the management API below /api/v1, and /metrics - and returns the status
// and the body of the answer.
func clusterAdmin(ctx context.Context, c *clustertest.Cluster, id, method, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+id+":9001"+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessKey+":"+c.SecretKey)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s through %s: %w", method, path, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s through %s: %w", method, path, id, err)
	}
	return resp.StatusCode, body, nil
}

// leaderOf asks a node who leads the metadata group.
func leaderOf(ctx context.Context, c *clustertest.Cluster, id string) (string, error) {
	status, body, err := clusterAdmin(ctx, c, id, http.MethodGet, "/api/v1/cluster")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/cluster through %s = %d", id, status)
	}
	var cluster struct {
		Leader string `json:"leader"`
	}
	if err := json.Unmarshal(body, &cluster); err != nil {
		return "", fmt.Errorf("GET /api/v1/cluster through %s: %w", id, err)
	}
	if cluster.Leader == "" {
		return "", fmt.Errorf("node %s knows of no leader", id)
	}
	return cluster.Leader, nil
}

// countedUsage asks a node for the usage of the counted bucket, and returns
// its figures when they are the exact count of the record.
func countedUsage(ctx context.Context, c *clustertest.Cluster, id string) (bucketdomain.UsageTotals, error) {
	status, body, err := clusterAdmin(ctx, c, id, http.MethodGet, "/api/v1/buckets/"+countedBucket+"/usage")
	if err != nil {
		return bucketdomain.UsageTotals{}, err
	}
	if status != http.StatusOK {
		return bucketdomain.UsageTotals{}, fmt.Errorf("the usage of %s through %s = %d %s", countedBucket, id, status, body)
	}
	var shown struct {
		ObjectCount int64  `json:"object_count"`
		TotalSize   int64  `json:"total_size"`
		Exact       bool   `json:"exact"`
		Versions    *int64 `json:"versions"`
		StoredBytes *int64 `json:"stored_bytes"`
		UploadBytes *int64 `json:"upload_bytes"`
	}
	if err := json.Unmarshal(body, &shown); err != nil {
		return bucketdomain.UsageTotals{}, fmt.Errorf("the usage of %s through %s: %w", countedBucket, id, err)
	}
	if !shown.Exact || shown.Versions == nil || shown.StoredBytes == nil || shown.UploadBytes == nil {
		return bucketdomain.UsageTotals{}, fmt.Errorf("the usage of %s through %s is not exact: %s", countedBucket, id, body)
	}
	return bucketdomain.UsageTotals{
		Objects: shown.ObjectCount, CurrentBytes: shown.TotalSize, Versions: *shown.Versions,
		StoredBytes: *shown.StoredBytes, UploadBytes: *shown.UploadBytes,
	}, nil
}

// metricOf scrapes a node's metrics and returns the value of a series
// without labels; an error when the node does not show it.
func metricOf(ctx context.Context, c *clustertest.Cluster, id, name string) (int64, error) {
	status, body, err := clusterAdmin(ctx, c, id, http.MethodGet, "/metrics")
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("the metrics of %s = %d", id, status)
	}
	for line := range strings.Lines(string(body)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), name+" "); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%s of %s = %q: %w", name, id, value, err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("%s is not among the metrics of %s", name, id)
}

// storedUsage reads the usage record of the counted bucket as it lies in
// the state database of a node that is stopped: what the node had applied,
// and not what some read path makes of it.
func storedUsage(t *testing.T, c *clustertest.Cluster, id string) bucketdomain.Usage {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, c.MetaFile(id, "state.db"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open the state database of %s: %v", id, err)
	}
	defer func() { _ = db.Close() }()
	var usage bucketdomain.Usage
	if err := db.View(func(tx *bbolt.Tx) error {
		records := tx.Bucket([]byte("bucketusage"))
		if records == nil {
			return errors.New("no usage records")
		}
		raw := records.Get([]byte(countedBucket))
		if raw == nil {
			return fmt.Errorf("no usage record of %s", countedBucket)
		}
		usage, err = bucketdomain.ParseUsage(raw)
		return err
	}); err != nil {
		t.Fatalf("the state of %s: %v", id, err)
	}
	return usage
}

// TestClusterLeaderKilledWhileCounting: the leader of the metadata group is
// killed while it counts the usage of a bucket (ADR-0023: the walk is in the
// record, "a new leader goes on from it"). A bucket holds 60 000 delete
// markers, six entries of the walk; a recount is asked of the leader, which
// proposes the first entry before it answers and the rest after, and the
// leader is killed at once. Its state database says that the bucket was
// being counted when it died (the test asks again, with the node back, when
// the kill came too late - at most three times). The others elect a leader,
// whose counter finishes the count: the gauge of buckets being counted
// comes back to 0 on it. The usage is then exactly what the bucket holds,
// no node counted drift, and a recount asked of the new leader finds the
// same, again without drift. The killed node, back, answers the same.
func TestClusterLeaderKilledWhileCounting(t *testing.T) {
	t.Parallel()
	// The test's own limit, longer than the one startCluster gives.
	c, _ := startCluster(t, clustertest.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), countedWithin)
	t.Cleanup(cancel)
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(countedBucket)}); err != nil {
		t.Fatalf("CreateBucket %s: %v", countedBucket, err)
	}
	if _, err := c.S3("node1").PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(countedBucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning %s: %v", countedBucket, err)
	}

	// A delete marker for each of 60 000 keys that never held an object,
	// countedBatch a request, through all four nodes. None is tried again:
	// a delete that is repeated makes a marker more. The first failure
	// stops the others, which then report nothing of their own.
	batches := make(chan int, countedMarkers/countedBatch)
	for b := range countedMarkers / countedBatch {
		batches <- b
	}
	close(batches)
	deleting, stop := context.WithCancel(ctx)
	defer stop()
	failures := make(chan error, countedDeleters)
	var deleters sync.WaitGroup
	begun := time.Now()
	for w := range countedDeleters {
		through := clustertest.Nodes[w%len(clustertest.Nodes)]
		client := c.S3(through)
		deleters.Go(func() {
			for b := range batches {
				if deleting.Err() != nil {
					return
				}
				names := make([]types.ObjectIdentifier, countedBatch)
				for i := range names {
					names[i].Key = aws.String(fmt.Sprintf("m/%07d", b*countedBatch+i))
				}
				out, err := client.DeleteObjects(deleting, &s3.DeleteObjectsInput{
					Bucket: aws.String(countedBucket), Delete: &types.Delete{Objects: names, Quiet: aws.Bool(true)},
				})
				switch {
				case deleting.Err() != nil:
					return
				case err != nil:
					failures <- fmt.Errorf("DeleteObjects of batch %d through %s: %w", b, through, err)
					stop()
					return
				case len(out.Errors) > 0:
					failures <- fmt.Errorf("DeleteObjects of batch %d through %s: %d keys refused, the first %s: %s",
						b, through, len(out.Errors), aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Code))
					stop()
					return
				}
			}
		})
	}
	deleters.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("making %d delete markers: %v", countedMarkers, err)
	}
	took := time.Since(begun)
	t.Logf("%d delete markers made in %s: %.0f commits a second, %d DeleteObjects of %d at a time",
		countedMarkers, took.Round(time.Second), float64(countedMarkers)/took.Seconds(), countedDeleters, countedBatch)
	// What the bucket holds: versions of no bytes, and nothing current.
	want := bucketdomain.UsageTotals{Versions: countedMarkers}
	exact := func(when, id string) {
		t.Helper()
		var got bucketdomain.UsageTotals
		var err error
		deadline := time.Now().Add(2 * time.Minute)
		for {
			if got, err = countedUsage(ctx, c, id); err == nil && got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the usage of %s through %s = %+v (%v); want exact, %+v", when, countedBucket, id, got, err, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	leader := func(when string, not ...string) string {
		t.Helper()
		var now string
		c.Eventually(when+": a leader", 2*time.Minute, func() bool {
			var err error
			now, err = leaderOf(ctx, c, "node4")
			return err == nil && !slices.Contains(not, now)
		})
		return now
	}
	exact("with the markers made", leader("with the markers made"))

	// A recount is asked of the leader, which is killed as soon as it has
	// answered: by then it has proposed the first entry of the walk, and
	// proposes the next ones every 100 ms.
	var killed string
	var killedAt time.Time
	for attempt := 1; ; attempt++ {
		counter := leader("before the recount")
		status, body, err := clusterAdmin(ctx, c, counter, http.MethodPost, "/api/v1/buckets/"+countedBucket+"/usage/recount")
		if err != nil || status != http.StatusAccepted {
			t.Fatalf("POST the recount of %s through %s = %d %s (%v); want 202", countedBucket, counter, status, body, err)
		}
		c.Kill(counter)
		killedAt = time.Now()
		stored := storedUsage(t, c, counter)
		// A recount keeps the record exact, and the totals kept, while it
		// counts.
		if !stored.Exact || stored.UsageTotals != want {
			t.Fatalf("the record of %s on %s, killed during a recount: exact %t, %+v; want exact, %+v", countedBucket, counter, stored.Exact, stored.UsageTotals, want)
		}
		if stored.Walking() {
			t.Logf("%s was killed while it counted %s: phase %d, at %q, %d versions counted", counter, countedBucket,
				stored.Count.Phase, stored.Count.Cursor, stored.Count.Versions)
			killed = counter
			break
		}
		if attempt == countedAttempts {
			t.Fatalf("%d times the recount of %d records ended before its leader was killed: the bucket must hold more", attempt, countedMarkers)
		}
		t.Logf("the recount had ended on %s before it was killed; once more", counter)
		c.Restart(counter)
		exact("after "+counter+" came back", counter)
	}

	// The others go on with the count, under a new leader, from where the
	// record says it was. Its gauge says when it is done - once it is sure
	// to have been read after the kill.
	time.Sleep(time.Until(killedAt.Add(gaugesAfter)))
	c.Eventually("the count to finish under a new leader", 2*time.Minute, func() bool {
		now, err := leaderOf(ctx, c, "node4")
		if err != nil || now == killed {
			return false
		}
		counting, err := metricOf(ctx, c, now, usageCounting)
		return err == nil && counting == 0
	})
	survivors := slices.DeleteFunc(slices.Clone(clustertest.Nodes), func(id string) bool { return id == killed })
	noDrift := func(when string) {
		t.Helper()
		for _, id := range survivors {
			drift, err := metricOf(ctx, c, id, usageDrift)
			if err != nil {
				t.Fatalf("%s: %v", when, err)
			}
			if drift != 0 {
				t.Errorf("%s: %s counted %d recounts that found other totals than were kept; want 0", when, id, drift)
			}
		}
	}
	finisher := leader("after the count", killed)
	exact("after the count finished under "+finisher, finisher)
	noDrift("after the count finished under " + finisher)

	// The count equals a recount: one asked of the new leader, and left to
	// end, finds the same totals, and no drift.
	status, body, err := clusterAdmin(ctx, c, finisher, http.MethodPost, "/api/v1/buckets/"+countedBucket+"/usage/recount")
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("POST the recount of %s through %s = %d %s (%v); want 202", countedBucket, finisher, status, body, err)
	}
	asked := time.Now()
	time.Sleep(time.Until(asked.Add(gaugesAfter)))
	c.Eventually("the second recount to finish", 2*time.Minute, func() bool {
		now, err := leaderOf(ctx, c, "node4")
		if err != nil {
			return false
		}
		counting, err := metricOf(ctx, c, now, usageCounting)
		return err == nil && counting == 0
	})
	exact("after a recount", leader("after a recount", killed))
	noDrift("after a recount")

	// The node that was killed comes back to the same record.
	c.Restart(killed)
	exact("with "+killed+" back", killed)
}
