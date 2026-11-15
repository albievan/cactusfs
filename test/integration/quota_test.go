//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/test/clustertest"
)

const (
	// quotaBucket is the bucket of TestBucketQuota: made with S3's
	// CreateBucket, so it keeps no versions.
	quotaBucket = "quota"
	// quotaMiB is the size of every object of the test, and quotaLimit
	// the bucket's quota: ten of them.
	quotaMiB   = 1 << 20
	quotaLimit = 10 * quotaMiB
	// quotaWriters is how many writers race for the ten places.
	quotaWriters = 16
	// quotaPart is the size of a part of the test's multipart upload: the
	// least a part that is not the last may be.
	quotaPart = 5 * quotaMiB
	// quotaSettles is how long the usage read through the leader may take
	// to show what was written: an answer is the node's own copy of the
	// state, current on the leader as soon as an entry is applied.
	quotaSettles = 30 * time.Second
	// quotaRefusals counts the writes a node refused for a quota.
	quotaRefusals = "cactusfs_quota_refusals_total"
	// quotaGrace is the collector's grace period the cluster runs with: the
	// least a node takes (cmd/cactusfs, minGCGrace). The test does not wait
	// it out; it makes every shard file older than it (quotaShardsAged).
	quotaGrace = "1h"
)

// quotaFigures are what a bucket's exact usage says, and its quota.
type quotaFigures struct {
	Objects, CurrentBytes, Versions, StoredBytes, UploadBytes, QuotaBytes int64
}

// quotaUsage asks a node, as the caller the headers name, for the usage of
// the test's bucket, and returns its figures when they are the exact count
// the metadata store keeps; an error when they are not, or the answer is
// not 200.
func quotaUsage(ctx context.Context, c *clustertest.Cluster, id string, headers map[string]string) (quotaFigures, error) {
	status, _, body, err := gridCall(ctx, c, id, headers, http.MethodGet, "/buckets/"+quotaBucket+"/usage", nil)
	if err != nil {
		return quotaFigures{}, err
	}
	if status != http.StatusOK {
		return quotaFigures{}, fmt.Errorf("the usage of %s through %s = %d %s", quotaBucket, id, status, body)
	}
	var shown struct {
		ObjectCount int64  `json:"object_count"`
		TotalSize   int64  `json:"total_size"`
		Complete    bool   `json:"complete"`
		Exact       bool   `json:"exact"`
		Versions    *int64 `json:"versions"`
		StoredBytes *int64 `json:"stored_bytes"`
		UploadBytes *int64 `json:"upload_bytes"`
		QuotaBytes  *int64 `json:"quota_bytes"`
	}
	if err := json.Unmarshal(body, &shown); err != nil {
		return quotaFigures{}, fmt.Errorf("the usage of %s through %s, %s: %w", quotaBucket, id, body, err)
	}
	if !shown.Exact || !shown.Complete || shown.Versions == nil || shown.StoredBytes == nil || shown.UploadBytes == nil {
		return quotaFigures{}, fmt.Errorf("the usage of %s through %s is not the exact count: %s", quotaBucket, id, body)
	}
	got := quotaFigures{
		Objects: shown.ObjectCount, CurrentBytes: shown.TotalSize, Versions: *shown.Versions,
		StoredBytes: *shown.StoredBytes, UploadBytes: *shown.UploadBytes,
	}
	if shown.QuotaBytes != nil {
		got.QuotaBytes = *shown.QuotaBytes
	}
	return got, nil
}

// quotaRefused says whether an answer of S3 is the refusal of a quota: 403
// QuotaExceeded, and nothing else.
func quotaRefused(err error) bool {
	return statusOf(err) == http.StatusForbidden && apiCode(err) == "QuotaExceeded"
}

// quotaMetric scrapes a node's metrics as root and returns the value of a
// series without labels; an error when the node does not show it.
func quotaMetric(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string, name string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+id+":9001/metrics", nil)
	if err != nil {
		return 0, fmt.Errorf("the metrics of %s: %w", id, err)
	}
	for header, value := range root {
		req.Header.Set(header, value)
	}
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		return 0, fmt.Errorf("the metrics of %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("the metrics of %s: %w", id, err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the metrics of %s = %d", id, resp.StatusCode)
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

// quotaShards returns how many shard files the two drives of a node hold,
// and of how many objects (data IDs: a shard file is "<data ID>.<index>").
func quotaShards(t *testing.T, c *clustertest.Cluster, id string) (files, objects int) {
	t.Helper()
	out := c.Shell(id, `find /data1/shards /data2/shards -type f | wc -l
find /data1/shards /data2/shards -type f -exec basename {} \; | cut -d. -f1 | sort -u | wc -l`)
	fields := strings.Fields(out)
	if len(fields) != 2 {
		t.Fatalf("the shard files of %s: %q", id, out)
	}
	files, errFiles := strconv.Atoi(fields[0])
	objects, errObjects := strconv.Atoi(fields[1])
	if err := errors.Join(errFiles, errObjects); err != nil {
		t.Fatalf("the shard files of %s: %q: %v", id, out, err)
	}
	return files, objects
}

// quotaShardsAged makes every shard file of a node older than the grace
// period of the collector: what an hour does, without the hour.
func quotaShardsAged(c *clustertest.Cluster, id string) {
	c.Shell(id, `find /data1/shards /data2/shards -type f -exec touch -d '2000-01-01 00:00:00' {} +`)
}

// quotaOrphan plants on a node a shard that nothing refers to: a copy of one
// of its shard files, with its owner and mode, under a data ID no write made
// - the first four hexadecimal digits of the copied file's ID, which name the
// directory a shard of the ID lies in, then 28 times "f" - and index 0. It
// returns the path of the copy as the helper sees it.
func quotaOrphan(t *testing.T, c *clustertest.Cluster, id string) string {
	t.Helper()
	out := c.Shell(id, `set -e
f=$(find /data1/shards -type f | head -n 1)
[ -n "$f" ]
d=$(dirname "$f"); b=$(basename "$f")
o="$d/$(echo "$b" | cut -c1-4)ffffffffffffffffffffffffffff.0"
[ "$o" != "$f" ] && [ ! -e "$o" ]
cp -p "$f" "$o"
echo "$o"`)
	orphan := strings.TrimSpace(out)
	if !strings.HasPrefix(orphan, "/data1/shards/") || !strings.HasSuffix(orphan, "ffffffffffffffffffffffffffff.0") {
		t.Fatalf("the orphan shard planted on %s: %q", id, out)
	}
	return orphan
}

// TestBucketQuota: a bucket's quota in the product as it is deployed
// (ADR-0024, PLAN 8.3.4), on four nodes.
//
// Root gives a bucket that keeps no versions a quota of 10 MiB. Sixteen
// writers each put an object of 1 MiB at once, four through each node:
// exactly ten succeed and the six others are answered 403 QuotaExceeded, and
// the usage says 10 MiB. That is exact and not "at most ten" by the design:
// the store decides in apply, where the entries are in one order, and admits
// a put exactly while the bucket holds less than its quota; the pre-check of
// a node refuses only on its own copy of the state, which is a prefix of the
// log, so it refuses a put only when ten were committed already (the bytes
// only grow during the race). Six refusals are counted, on whichever nodes
// refused them.
//
// Then: an overwrite of an object by one of the same size succeeds in the
// full bucket (it frees what it adds), a new object does not; a multipart
// upload is begun, and its part is refused, at the part. Six objects are
// deleted; the part is taken, the next part that would pass the quota is
// refused, and a put of 1 MiB fills the bucket exactly. The upload is
// aborted. A bucket-admin, who holds s3:* on the bucket, is refused a
// change and a removal of the quota (403 AccessDenied), each denial is in the
// audit trail, and the quota is as it was.
//
// Last, the collector. The refused writes leave no shard (the pre-check
// refuses before one is written, and a commit the store refuses has its
// shards removed at once), nor do the overwrite, the deletes and the abort.
// So a shard nothing refers to is planted on every node, every shard file is
// made older than the collector's grace period, and after two passes that
// began after that, each node has removed exactly its orphan and holds the
// two shards of each object the bucket holds, and nothing else.
//
// The usage is read through the leader, and polled until it is what was
// written: a follower answers from its own copy of the state, which may lag
// the leader's by the time an entry takes to reach it. A write that must be
// taken after a delete is sent through a node whose copy has been seen to
// have the delete: its pre-check judges on that copy.
//
// Not parallel: the container tests of milestone 8 are not (PLAN, Milestone
// 8: "never t.Parallel, one cluster a test file"), so that each holds the
// engine's host alone. "Exactly ten" does not depend on it.
func TestBucketQuota(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	c := clustertest.Start(t, clustertest.Options{Env: map[string]string{
		// The collector passes every two seconds; the grace is the least.
		"CACTUSFS_GC_INTERVAL": "2s", "CACTUSFS_GC_GRACE": quotaGrace,
	}})
	nodes := clustertest.Nodes
	root := gridBearer(c.AccessKey, c.SecretKey)

	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(quotaBucket)}); err != nil {
		t.Fatalf("CreateBucket %s through node1: %v", quotaBucket, err)
	}
	// The quota is taken once the cluster writes at metadata level 5 (503
	// until then) and the bucket is counted (409 until then: a bucket made
	// below level 5 is counted by the leader without being asked).
	var last string
	c.Eventually("the quota of "+quotaBucket+" to be taken", 2*time.Minute, func() bool {
		status, _, body, err := gridCall(ctx, c, "node2", root, http.MethodPut, "/buckets/"+quotaBucket+"/quota", map[string]any{"quota_bytes": quotaLimit})
		switch {
		case err != nil:
			last = err.Error()
			return false
		case status == http.StatusOK:
			return true
		case status == http.StatusServiceUnavailable || status == http.StatusConflict:
			last = fmt.Sprintf("%d %s", status, body)
			return false
		default:
			t.Fatalf("PUT the quota of %s through node2 = %d %s; want 200 (or 503, 409 while the cluster is new)", quotaBucket, status, body)
			return false
		}
	})
	if last != "" {
		t.Logf("the quota was taken after %s", last)
	}

	// settles waits until the usage read through the leader is want.
	settles := func(when string, want quotaFigures) {
		t.Helper()
		want.QuotaBytes = quotaLimit
		var got quotaFigures
		var err error
		deadline := time.Now().Add(quotaSettles)
		for {
			var leader string
			if leader, err = askLeader(ctx, c, "node1"); err == nil {
				if got, err = quotaUsage(ctx, c, leader, root); err == nil && got == want {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the usage through the leader = %+v (%v); want %+v", when, got, err, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	// refusals says that the nodes together counted so many refusals.
	refusals := func(when string, want int64) {
		t.Helper()
		var sum int64
		for _, id := range nodes {
			n, err := quotaMetric(ctx, c, id, root, quotaRefusals)
			if err != nil {
				t.Fatalf("%s: %v", when, err)
			}
			sum += n
		}
		if sum != want {
			t.Errorf("%s: the nodes counted %d refusals for a quota; want %d", when, sum, want)
		}
	}
	// absent says that a key the bucket refused is not stored.
	absent := func(when, key string) {
		t.Helper()
		_, err := c.S3("node3").HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(quotaBucket), Key: aws.String(key)})
		if statusOf(err) != http.StatusNotFound {
			t.Errorf("%s: HeadObject %s/%s = %v; want 404: the refused write was stored", when, quotaBucket, key, err)
		}
	}
	settles("with the quota set", quotaFigures{})

	// Sixteen writers, four through each node, all at once.
	contents := make([][]byte, quotaWriters)
	answers := make([]error, quotaWriters)
	for i := range quotaWriters {
		contents[i] = randomBytes(t, quotaMiB)
	}
	begin := make(chan struct{})
	var writers sync.WaitGroup
	for i := range quotaWriters {
		client := c.S3(nodes[i%len(nodes)])
		key := fmt.Sprintf("race-%02d", i)
		writers.Go(func() {
			<-begin
			_, answers[i] = client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(quotaBucket), Key: aws.String(key), Body: bytes.NewReader(contents[i]),
			})
		})
	}
	close(begin)
	writers.Wait()
	var stored, refused []int
	for i, err := range answers {
		switch {
		case err == nil:
			stored = append(stored, i)
		case quotaRefused(err):
			refused = append(refused, i)
		default:
			t.Errorf("PutObject race-%02d through %s: %v (status %d); want success or 403 QuotaExceeded", i, nodes[i%len(nodes)], err, statusOf(err))
		}
	}
	if t.Failed() || len(stored) != 10 || len(refused) != quotaWriters-10 {
		t.Fatalf("of %d writers of 1 MiB into a quota of 10 MiB, %d succeeded and %d were refused for the quota; want exactly 10 and %d",
			quotaWriters, len(stored), len(refused), quotaWriters-10)
	}
	// What was taken reads back through another node than took it; what
	// was refused is not there.
	for _, i := range stored {
		key, through := fmt.Sprintf("race-%02d", i), nodes[(i+1)%len(nodes)]
		if got, err := c.Get(ctx, through, quotaBucket, key); err != nil || !bytes.Equal(got, contents[i]) {
			t.Errorf("Get %s through %s after the race: %d bytes, %v", key, through, len(got), err)
		}
	}
	for _, i := range refused {
		absent("after the race", fmt.Sprintf("race-%02d", i))
	}
	settles("after the race", quotaFigures{Objects: 10, CurrentBytes: quotaLimit, Versions: 10, StoredBytes: quotaLimit})
	refusals("after the race", int64(len(refused)))
	// The quota, read through the leader as well, says the same: full.
	leader, err := askLeader(ctx, c, "node2")
	if err != nil {
		t.Fatal(err)
	}
	status, _, body, err := gridCall(ctx, c, leader, root, http.MethodGet, "/buckets/"+quotaBucket+"/quota", nil)
	var quota struct {
		QuotaBytes *int64 `json:"quota_bytes"`
		UsedBytes  int64  `json:"used_bytes"`
		Exact      bool   `json:"exact"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &quota) != nil ||
		quota.QuotaBytes == nil || *quota.QuotaBytes != quotaLimit || !quota.Exact || quota.UsedBytes != quotaLimit {
		t.Fatalf("GET the quota of %s through the leader, %s = %d %s (%v); want %d bytes used of %d, exact", quotaBucket, leader, status, body, err, quotaLimit, quotaLimit)
	}

	// An overwrite of the same size frees what it adds: taken in the full
	// bucket. A new object is not.
	overwritten := fmt.Sprintf("race-%02d", stored[0])
	again := randomBytes(t, quotaMiB)
	if err := c.Put(ctx, "node3", quotaBucket, overwritten, again); err != nil {
		t.Fatalf("PutObject %s of the same size through node3, in the full bucket: %v; want success", overwritten, err)
	}
	if got, err := c.Get(ctx, "node1", quotaBucket, overwritten); err != nil || !bytes.Equal(got, again) {
		t.Fatalf("Get %s through node1 after its overwrite: %d bytes, %v", overwritten, len(got), err)
	}
	settles("after an overwrite", quotaFigures{Objects: 10, CurrentBytes: quotaLimit, Versions: 10, StoredBytes: quotaLimit})
	if err := c.Put(ctx, "node2", quotaBucket, "one-more", randomBytes(t, quotaMiB)); !quotaRefused(err) {
		t.Fatalf("PutObject one-more through node2, in the full bucket: %v (status %d); want 403 QuotaExceeded", err, statusOf(err))
	}
	absent("after a put into the full bucket", "one-more")

	// A multipart upload is begun - that adds nothing - and is refused at
	// the part.
	upload, err := c.S3("node4").CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(quotaBucket), Key: aws.String("film")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload through node4, in the full bucket: %v", err)
	}
	part := func(through string, number int32) error {
		_, err := c.S3(through).UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(quotaBucket), Key: aws.String("film"), UploadId: upload.UploadId,
			PartNumber: aws.Int32(number), Body: bytes.NewReader(randomBytes(t, quotaPart)),
		})
		return err
	}
	if err := part("node1", 1); !quotaRefused(err) {
		t.Fatalf("UploadPart 1 of 5 MiB through node1, in the full bucket: %v (status %d); want 403 QuotaExceeded", err, statusOf(err))
	}

	// Six objects are deleted: 6 MiB are free. The first part is taken; the
	// second would pass the quota, and is refused.
	for n, i := range stored[4:] {
		key, through := fmt.Sprintf("race-%02d", i), nodes[n%len(nodes)]
		if _, err := c.S3(through).DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(quotaBucket), Key: aws.String(key)}); err != nil {
			t.Fatalf("DeleteObject %s through %s: %v", key, through, err)
		}
	}
	settles("after six deletes", quotaFigures{Objects: 4, CurrentBytes: 4 * quotaMiB, Versions: 4, StoredBytes: 4 * quotaMiB})
	// sees waits until a node's own copy of the state - what its pre-check
	// judges on - says what the leader's says: a write that must be taken
	// after a delete is sent through a node that has the delete.
	sees := func(when, id string, want quotaFigures) {
		t.Helper()
		want.QuotaBytes = quotaLimit
		var got quotaFigures
		var err error
		deadline := time.Now().Add(quotaSettles)
		for {
			if got, err = quotaUsage(ctx, c, id, root); err == nil && got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the usage through %s = %+v (%v); want %+v", when, id, got, err, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	sees("after six deletes", "node2", quotaFigures{Objects: 4, CurrentBytes: 4 * quotaMiB, Versions: 4, StoredBytes: 4 * quotaMiB})
	if err := part("node2", 1); err != nil {
		t.Fatalf("UploadPart 1 of 5 MiB through node2, with 6 MiB free: %v; want success", err)
	}
	if err := part("node3", 2); !quotaRefused(err) {
		t.Fatalf("UploadPart 2 of 5 MiB through node3, with 1 MiB free: %v (status %d); want 403 QuotaExceeded", err, statusOf(err))
	}
	settles("with one part of the upload", quotaFigures{Objects: 4, CurrentBytes: 4 * quotaMiB, Versions: 4, StoredBytes: 4 * quotaMiB, UploadBytes: quotaPart})
	// After deleting, a put succeeds: it fills the bucket exactly.
	sees("with one part of the upload", "node4", quotaFigures{Objects: 4, CurrentBytes: 4 * quotaMiB, Versions: 4, StoredBytes: 4 * quotaMiB, UploadBytes: quotaPart})
	if err := c.Put(ctx, "node4", quotaBucket, "after-deleting", randomBytes(t, quotaMiB)); err != nil {
		t.Fatalf("PutObject after-deleting of 1 MiB through node4, with 1 MiB free: %v; want success", err)
	}
	settles("full again", quotaFigures{Objects: 5, CurrentBytes: 5 * quotaMiB, Versions: 5, StoredBytes: 5 * quotaMiB, UploadBytes: quotaPart})
	// An abort is never refused, and gives the part's bytes back.
	if _, err := c.S3("node1").AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(quotaBucket), Key: aws.String("film"), UploadId: upload.UploadId,
	}); err != nil {
		t.Fatalf("AbortMultipartUpload through node1, in the full bucket: %v", err)
	}
	settles("after the abort", quotaFigures{Objects: 5, CurrentBytes: 5 * quotaMiB, Versions: 5, StoredBytes: 5 * quotaMiB})
	// The race's six, the new object and the two parts.
	refusals("after the upload", int64(len(refused))+3)

	// A bucket's administrator cannot lift its limit, nor remove it.
	ownerID, ownerSecret, err := gridUser(ctx, c, "node1", root, "quota-owner", map[string]any{"roles": []string{"bucket-admin"}})
	if err != nil {
		t.Fatal(err)
	}
	owner := gridBearer(ownerID, ownerSecret)
	for _, change := range []struct {
		through, method string
		body            any
	}{
		{"node2", http.MethodPut, map[string]any{"quota_bytes": int64(1) << 40}},
		{"node3", http.MethodDelete, nil},
	} {
		status, header, body, err := gridCall(ctx, c, change.through, owner, change.method, "/buckets/"+quotaBucket+"/quota", change.body)
		if err != nil || status != http.StatusForbidden || previewProblem(header, body) != "AccessDenied" {
			t.Errorf("%s the quota of %s as a bucket-admin through %s = %d %s (%v); want 403 AccessDenied", change.method, quotaBucket, change.through, status, body, err)
		}
	}
	settles("after a bucket-admin was refused", quotaFigures{Objects: 5, CurrentBytes: 5 * quotaMiB, Versions: 5, StoredBytes: 5 * quotaMiB})
	// Both denials are in the trail.
	c.Eventually("the denials of the bucket-admin in the audit trail", time.Minute, func() bool {
		denied := map[string]bool{}
		for _, record := range previewAudit(ctx, t, c, "node4", root) {
			if record.Actor == "user/quota-owner" && record.AccessKeyID == ownerID && record.Outcome == "denied" &&
				record.Resource == "arn:aws:s3:::"+quotaBucket {
				denied[record.Action] = true
			}
		}
		return denied["admin:PutBucketQuota"] && denied["admin:DeleteBucketQuota"]
	})

	// The collector. Nothing refused or removed left a shard behind, so one
	// that nothing refers to is planted on every node; then every shard
	// file is made older than the grace period. Two passes that began after
	// that remove exactly the orphan on every node, and keep every shard of
	// what the bucket holds. The count of removals is read before the files
	// are aged (until then the orphan is young, and kept); that of passes
	// after, so that the second pass counted since began after the aging.
	const held = 5
	orphans := map[string]string{}
	removedBefore, passesAfter := map[string]int64{}, map[string]int64{}
	// What each node holds beyond what the bucket holds, the orphan
	// included: the collector is to remove exactly that. More than the
	// orphan is a shard a best-effort removal left behind - the
	// collector's too.
	extra := map[string]int64{}
	for _, id := range nodes {
		orphans[id] = quotaOrphan(t, c, id)
		files, objects := quotaShards(t, c, id)
		if files < 2*held+1 || objects < held+1 {
			t.Fatalf("%s holds %d shard files of %d objects with the orphan; want %d of %d at the least", id, files, objects, 2*held+1, held+1)
		}
		if extra[id] = int64(files - 2*held); extra[id] > 1 {
			t.Logf("%s holds %d shard files beyond the orphan and what the bucket holds", id, extra[id]-1)
		}
		if removedBefore[id], err = quotaMetric(ctx, c, id, root, "cactusfs_gc_shards_removed_total"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range nodes {
		quotaShardsAged(c, id)
	}
	for _, id := range nodes {
		if passesAfter[id], err = quotaMetric(ctx, c, id, root, "cactusfs_gc_passes_total"); err != nil {
			t.Fatal(err)
		}
	}
	collected := func(id string) (passes, removed int64, err error) {
		if passes, err = quotaMetric(ctx, c, id, root, "cactusfs_gc_passes_total"); err != nil {
			return 0, 0, err
		}
		if removed, err = quotaMetric(ctx, c, id, root, "cactusfs_gc_shards_removed_total"); err != nil {
			return 0, 0, err
		}
		return passes - passesAfter[id], removed - removedBefore[id], nil
	}
	c.Eventually("two passes of the collector on every node since the shards were aged", 2*time.Minute, func() bool {
		for _, id := range nodes {
			passes, removed, err := collected(id)
			if err != nil || passes < 2 || removed < extra[id] {
				return false
			}
		}
		return true
	})
	for _, id := range nodes {
		if _, removed, err := collected(id); err != nil || removed != extra[id] {
			t.Errorf("the collector of %s removed %d shards since the orphan was aged (%v); want %d, the orphan and nothing referenced", id, removed, err, extra[id])
		}
		files, objects := quotaShards(t, c, id)
		if files != 2*held || objects != held {
			t.Errorf("%s holds %d shard files of %d objects after the collector; want exactly %d of %d, what the bucket holds", id, files, objects, 2*held, held)
		}
		if gone := strings.TrimSpace(c.Shell(id, "if [ -e '"+orphans[id]+"' ]; then echo there; else echo gone; fi")); gone != "gone" {
			t.Errorf("the orphan shard %s of %s is %s after the collector", orphans[id], id, gone)
		}
	}
	// And everything the bucket holds reads back.
	for _, key := range []string{overwritten, "after-deleting"} {
		if _, err := c.Get(ctx, "node2", quotaBucket, key); err != nil {
			t.Errorf("Get %s through node2 after the collector: %v", key, err)
		}
	}
	if got, err := c.Get(ctx, "node2", quotaBucket, overwritten); err == nil && !bytes.Equal(got, again) {
		t.Errorf("Get %s through node2 after the collector: other bytes than its overwrite", overwritten)
	}
	for _, i := range stored[1:4] {
		key := fmt.Sprintf("race-%02d", i)
		if got, err := c.Get(ctx, "node4", quotaBucket, key); err != nil || !bytes.Equal(got, contents[i]) {
			t.Errorf("Get %s through node4 after the collector: %d bytes, %v", key, len(got), err)
		}
	}
}
