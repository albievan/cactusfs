//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/test/clustertest"
)

// objectAccess is the answer of GET /buckets/{bucket}/object/access.
type objectAccess struct {
	UploadedBy       string     `json:"uploaded_by"`
	UploadedAt       time.Time  `json:"uploaded_at"`
	DownloadCount    int64      `json:"download_count"`
	CopyCount        int64      `json:"copy_count"`
	LastDownloadedAt *time.Time `json:"last_downloaded_at"`
	Readers          []struct {
		Who       string `json:"who"`
		Presigned bool   `json:"presigned"`
		Downloads int64  `json:"downloads"`
		Copies    int64  `json:"copies"`
	} `json:"readers"`
	OtherReaders int64 `json:"other_readers"`
}

// clusterAdmin sends one request to the management API of a node of the
// cluster and returns the status and the body.
func clusterAdmin(ctx context.Context, t *testing.T, c *clustertest.Cluster, id, bearer, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+id+":9001/api/v1"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		t.Fatalf("%s %s through %s: %v", method, path, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s through %s: %v", method, path, id, err)
	}
	return resp.StatusCode, data
}

// TestObjectAccessAcrossTheCluster: who uploaded an object and who downloaded
// it, in the product as it is deployed (ADR-0016). An object is uploaded
// through one node and downloaded through the three others - signed, with a
// presigned URL, and through the management API. Every node counts what it
// served and hands it to the metadata group; the record is the same whichever
// node is asked, is not shown to who may merely read the object, and is still
// there when the leader has been restarted.
func TestObjectAccessAcrossTheCluster(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	// The shortest interval there is: the test waits for it.
	c := clustertest.Start(t, clustertest.Options{Env: map[string]string{"CACTUSFS_ACCESS_FLUSH_INTERVAL": "5s"}})
	root := c.AccessKey + ":" + c.SecretKey
	const key = "2026/q3.pdf"
	accessPath := "/buckets/reports/object/access?key=" + url.QueryEscape(key)

	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("reports")}); err != nil {
		t.Fatalf("CreateBucket through node1: %v", err)
	}
	content := randomBytes(t, 300_000)
	if err := c.Put(ctx, "node1", "reports", key, content); err != nil {
		t.Fatalf("Put through node1: %v", err)
	}
	// A user who may read every object, and that is all.
	if status, body := clusterAdmin(ctx, t, c, "node4", root, http.MethodPost, "/users", map[string]any{"name": "reader", "roles": []string{"read-only"}}); status != http.StatusCreated {
		t.Fatalf("create user = %d %s", status, body)
	}
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	status, body := clusterAdmin(ctx, t, c, "node4", root, http.MethodPost, "/users/reader/keys", map[string]any{})
	if err := json.Unmarshal(body, &created); err != nil || status != http.StatusCreated || created.SecretKey == "" {
		t.Fatalf("create key = %d %s (%v)", status, body, err)
	}
	reader := created.Key.AccessKeyID + ":" + created.SecretKey

	// Nobody has read it yet: the uploader and the time are known at once,
	// through a node that did not take the upload.
	var before objectAccess
	status, body = clusterAdmin(ctx, t, c, "node2", root, http.MethodGet, accessPath, nil)
	if err := json.Unmarshal(body, &before); err != nil || status != http.StatusOK {
		t.Fatalf("access before any read = %d %s (%v)", status, body, err)
	}
	if before.UploadedBy != "root" || before.UploadedAt.IsZero() || before.DownloadCount != 0 || before.LastDownloadedAt != nil || len(before.Readers) != 0 {
		t.Errorf("access before any read = %s", body)
	}

	// One: signed by root, through node2.
	if got, err := c.Get(ctx, "node2", "reports", key); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Get through node2: %v", err)
	}
	// Two: with a URL root signed, through node3, by whoever holds it.
	presigned, err := s3.NewPresignClient(c.S3("node3")).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("reports"), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if signed, err := url.Parse(presigned.URL); err != nil || signed.Host != "node3:9000" || signed.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("the presigned URL is %q (%v)", presigned.URL, err)
	}
	req, err := http.NewRequestWithContext(ctx, presigned.Method, presigned.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range presigned.SignedHeader {
		if http.CanonicalHeaderKey(name) != "Host" {
			req.Header[name] = values
		}
	}
	resp, err := c.HTTP("node3").Do(req)
	if err != nil {
		t.Fatalf("presigned GET through node3: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("presigned GET through node3 = %d, %d bytes, %v", resp.StatusCode, len(got), err)
	}
	// Three: through the management API of node4, by the user.
	status, got = clusterAdmin(ctx, t, c, "node4", reader, http.MethodGet, "/buckets/reports/object?key="+url.QueryEscape(key), nil)
	if status != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("download through the management API of node4 = %d, %d bytes", status, len(got))
	}
	// None: a look at the object, by root and by the user.
	if _, err := c.S3("node2").HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("reports"), Key: aws.String(key)}); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if status, body := clusterAdmin(ctx, t, c, "node4", reader, http.MethodGet, "/buckets/reports/object/metadata?key="+url.QueryEscape(key), nil); status != http.StatusOK {
		t.Fatalf("metadata = %d %s", status, body)
	}

	// Who may read the object may not see who read it - while the counts
	// are still on their way, and afterwards.
	if status, body := clusterAdmin(ctx, t, c, "node4", reader, http.MethodGet, accessPath, nil); status != http.StatusForbidden {
		t.Errorf("access for a read-only user = %d %s, want 403", status, body)
	}

	// Three nodes each counted one download; each hands it over within its
	// interval. Asked through the node that served none of them.
	rows := func(a objectAccess) string {
		out := ""
		for _, r := range a.Readers {
			out += fmt.Sprintf("%s presigned=%t downloads=%d copies=%d; ", r.Who, r.Presigned, r.Downloads, r.Copies)
		}
		return out
	}
	const wantRows = "root presigned=false downloads=1 copies=0; root presigned=true downloads=1 copies=0; user/reader presigned=false downloads=1 copies=0; "
	var access objectAccess
	c.Eventually("the three downloads to be recorded", time.Minute, func() bool {
		status, body := clusterAdmin(ctx, t, c, "node1", root, http.MethodGet, accessPath, nil)
		access = objectAccess{}
		if err := json.Unmarshal(body, &access); err != nil || status != http.StatusOK {
			t.Fatalf("access = %d %s (%v)", status, body, err)
		}
		return access.DownloadCount >= 3
	})
	if access.UploadedBy != "root" || !access.UploadedAt.Equal(before.UploadedAt) || access.DownloadCount != 3 || access.CopyCount != 0 ||
		access.LastDownloadedAt == nil || access.OtherReaders != 0 || rows(access) != wantRows {
		t.Errorf("access = %+v; readers: %s", access, rows(access))
	}
	// Every node says the same.
	for _, id := range clustertest.Nodes {
		var seen objectAccess
		status, body := clusterAdmin(ctx, t, c, id, root, http.MethodGet, accessPath, nil)
		if err := json.Unmarshal(body, &seen); err != nil || status != http.StatusOK || seen.DownloadCount != 3 || rows(seen) != wantRows {
			t.Errorf("access through %s = %d %s (%v)", id, status, body, err)
		}
	}
	if status, body := clusterAdmin(ctx, t, c, "node3", reader, http.MethodGet, accessPath, nil); status != http.StatusForbidden {
		t.Errorf("access for a read-only user = %d %s, want 403", status, body)
	}
	if status, body := clusterAdmin(ctx, t, c, "node3", root, http.MethodGet, "/buckets/reports/object/access?key=nope", nil); status != http.StatusNotFound {
		t.Errorf("access of an object there is not = %d %s, want 404", status, body)
	}

	// The leader is stopped as an orchestrator stops it, and comes back.
	// The record is in the replicated state: it is there under the new
	// leader, and on the old one when it has returned.
	var cluster struct {
		Leader string `json:"leader"`
	}
	status, body = clusterAdmin(ctx, t, c, "node4", root, http.MethodGet, "/cluster", nil)
	if err := json.Unmarshal(body, &cluster); err != nil || status != http.StatusOK || cluster.Leader == "" {
		t.Fatalf("cluster = %d %s (%v)", status, body, err)
	}
	leader := cluster.Leader
	c.Stop(leader)
	other := "node4"
	c.Eventually("the record to be served without "+leader, 2*time.Minute, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+other+":9001/api/v1"+accessPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+root)
		resp, err := c.Admin(other).Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var seen objectAccess
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&seen) == nil && seen.DownloadCount == 3 && rows(seen) == wantRows
	})
	c.Restart(leader)
	var after objectAccess
	status, body = clusterAdmin(ctx, t, c, leader, root, http.MethodGet, accessPath, nil)
	if err := json.Unmarshal(body, &after); err != nil || status != http.StatusOK {
		t.Fatalf("access through the returned %s = %d %s (%v)", leader, status, body, err)
	}
	if after.UploadedBy != "root" || after.DownloadCount != 3 || rows(after) != wantRows {
		t.Errorf("access after the restart of the leader = %s", body)
	}

	// The node that returned counts again, under another number of its
	// start, and what it counts is added - not taken for a batch that was
	// seen before.
	if got, err := c.Get(ctx, leader, "reports", key); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("Get through the returned %s: %v", leader, err)
	}
	c.Eventually("a download through the returned node to be recorded", time.Minute, func() bool {
		status, body := clusterAdmin(ctx, t, c, "node4", root, http.MethodGet, accessPath, nil)
		var seen objectAccess
		if err := json.Unmarshal(body, &seen); err != nil || status != http.StatusOK {
			t.Fatalf("access = %d %s (%v)", status, body, err)
		}
		return seen.DownloadCount == 4
	})
}
