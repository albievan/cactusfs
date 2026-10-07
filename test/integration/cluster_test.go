//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/test/clustertest"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func statusOf(err error) int {
	var response *awshttp.ResponseError
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}
	return 0
}

// TestClusterOfContainers: four containers of the product image form a
// cluster over mutual TLS and serve S3 over TLS. It is the product as it is
// deployed: the image, its user, its read-only expectations, its volumes,
// its flags - nothing in the test process stands in for any of it.
func TestClusterOfContainers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	c := clustertest.Start(t, clustertest.Options{})
	nodes := clustertest.Nodes

	for _, id := range nodes {
		if !c.Ready(id) {
			t.Errorf("node %s is not ready", id)
		}
	}
	// The cluster formed as intended, and says so.
	logs := c.Logs("node1")
	for _, want := range []string{`"msg":"joined the cluster"`, `"data_shards":4`, `"parity_shards":4`, `"node_failures_tolerated":2`, `"cluster":true`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log of node1 lacks %s", want)
		}
	}
	// No secret is in any log.
	for _, id := range nodes {
		if log := c.Logs(id); strings.Contains(log, c.SecretKey) {
			t.Errorf("the log of %s contains the root secret", id)
		}
	}

	// A bucket made through one node exists on all.
	if _, err := c.S3("node4").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatalf("CreateBucket through node4: %v", err)
	}
	for _, id := range nodes {
		if _, err := c.S3(id).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("photos")}); err != nil {
			t.Errorf("HeadBucket through %s: %v", id, err)
		}
	}

	// Objects written through every node are read, at once, through every
	// other: read-after-write does not depend on which node is asked.
	objects := map[string][]byte{}
	for i := range 16 {
		key := fmt.Sprintf("dir/object-%02d", i)
		data := randomBytes(t, 1+i*i*40_000)
		writer := nodes[i%4]
		if err := c.Put(ctx, writer, "photos", key, data); err != nil {
			t.Fatalf("Put %s through %s: %v", key, writer, err)
		}
		for step := 1; step < 4; step++ {
			reader := nodes[(i+step)%4]
			got, err := c.Get(ctx, reader, "photos", key)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("Get %s through %s right after Put through %s: %v", key, reader, writer, err)
			}
		}
		objects[key] = data
	}
	// Overwrites and deletes are seen everywhere at once, too.
	for round := range 8 {
		data := randomBytes(t, 5000+round)
		writer, reader := nodes[round%4], nodes[(round+2)%4]
		if err := c.Put(ctx, writer, "photos", "changing", data); err != nil {
			t.Fatal(err)
		}
		if got, err := c.Get(ctx, reader, "photos", "changing"); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round %d: Get through %s after overwrite through %s: stale or failed: %v", round, reader, writer, err)
		}
	}
	if _, err := c.S3("node2").DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("changing")}); err != nil {
		t.Fatal(err)
	}
	for _, id := range nodes {
		if _, err := c.Get(ctx, id, "photos", "changing"); statusOf(err) != http.StatusNotFound {
			t.Errorf("Get of a deleted object through %s: %v", id, err)
		}
	}
	// Every node lists the same.
	for _, id := range nodes {
		listed, err := c.S3(id).ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("photos")})
		if err != nil || len(listed.Contents) != len(objects) {
			t.Errorf("ListObjectsV2 through %s: %d objects, %v; want %d", id, len(listed.Contents), err, len(objects))
		}
	}
	// A range, through a node that did not write the object.
	ranged, err := c.S3("node3").GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("dir/object-15"), Range: aws.String("bytes=1048000-1049999"),
	})
	if err != nil {
		t.Fatalf("ranged GetObject: %v", err)
	}
	part := new(bytes.Buffer)
	_, _ = part.ReadFrom(ranged.Body)
	_ = ranged.Body.Close()
	if !bytes.Equal(part.Bytes(), objects["dir/object-15"][1048000:1050000]) {
		t.Error("ranged read through another node returned the wrong bytes")
	}

	// The shards of every object are on all four nodes: two on each.
	for _, id := range nodes {
		count := strings.TrimSpace(c.Shell(id, "find /data1/shards /data2/shards -type f | wc -l"))
		if want := fmt.Sprint(2 * len(objects)); count != want {
			t.Errorf("node %s holds %s shard files, want %s", id, count, want)
		}
	}
	// Nothing an object is called reaches a file name, on any node.
	names := c.Shell("node1", "find /data1 /data2 | grep -ci 'object\\|photos' || true")
	if strings.TrimSpace(names) != "0" {
		t.Errorf("a bucket or key name appears in a path on a drive: %s", names)
	}

	// A node is stopped as an orchestrator stops it. The cluster serves on;
	// the node comes back with everything.
	c.Stop("node2")
	for key, want := range objects {
		got, err := c.Get(ctx, "node3", "photos", key)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Get %s with node2 stopped: %v", key, err)
		}
	}
	during := randomBytes(t, 600_000)
	if err := c.Put(ctx, "node1", "photos", "written-without-node2", during); err != nil {
		t.Fatalf("Put with node2 stopped: %v", err)
	}
	c.Restart("node2")
	if got, err := c.Get(ctx, "node2", "photos", "written-without-node2"); err != nil || !bytes.Equal(got, during) {
		t.Fatalf("Get through the returned node of what was written without it: %v", err)
	}
	if !strings.Contains(c.Logs("node2"), `"msg":"stopped"`) {
		t.Error("node2 did not log a clean stop")
	}

	// The whole cluster restarts. It is the same cluster afterwards.
	for _, id := range nodes {
		c.Stop(id)
	}
	for _, id := range nodes {
		c.StartNode(id)
	}
	c.WaitReady(nodes...)
	for key, want := range objects {
		got, err := c.Get(ctx, "node4", "photos", key)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Get %s after a restart of the whole cluster: %v", key, err)
		}
	}
	if strings.Count(c.Logs("node1"), "new deployment: drive topology recorded")+
		strings.Count(c.Logs("node2"), "new deployment: drive topology recorded")+
		strings.Count(c.Logs("node3"), "new deployment: drive topology recorded") != 1 {
		t.Error("the topology was recorded more or less than once in the life of the cluster")
	}
}
