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
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/albievan/cactusfs/test/clustertest"
)

// gridEntry is one upload or download of a bucket's latest, as the list of
// buckets gives it.
type gridEntry struct {
	At        time.Time `json:"at"`
	By        string    `json:"by"`
	Key       *string   `json:"key"`
	Presigned bool      `json:"presigned"`
}

// gridBucket is a bucket of GET /buckets?details=true.
type gridBucket struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Details   *struct {
		AccessIncluded bool    `json:"access_included"`
		PolicyIncluded bool    `json:"policy_included"`
		CreatedBy      *string `json:"created_by"`
		Activity       *struct {
			Uploads   []gridEntry `json:"uploads"`
			Downloads []gridEntry `json:"downloads"`
		} `json:"activity"`
		Policy *struct {
			Statements   int  `json:"statements"`
			OpenToAnyone bool `json:"open_to_anyone"`
		} `json:"policy"`
	} `json:"details"`
}

// activity returns the latest uploads and downloads of the bucket, and
// whether the list carried them at all.
func (b gridBucket) activity() (uploads, downloads []gridEntry, shown bool) {
	if b.Details == nil || !b.Details.AccessIncluded || b.Details.Activity == nil {
		return nil, nil, false
	}
	return b.Details.Activity.Uploads, b.Details.Activity.Downloads, true
}

// creator returns who the list says created the bucket; nobody when it does
// not say.
func (b gridBucket) creator() string {
	if b.Details == nil || b.Details.CreatedBy == nil {
		return ""
	}
	return *b.Details.CreatedBy
}

// gridRows writes rows of uploads or downloads on one line, in their order:
// "key by; " and "key by presigned; ".
func gridRows(entries []gridEntry) string {
	var out strings.Builder
	for _, e := range entries {
		key := "(no key)"
		if e.Key != nil {
			key = *e.Key
		}
		out.WriteString(key + " " + e.By)
		if e.Presigned {
			out.WriteString(" presigned")
		}
		out.WriteString("; ")
	}
	return out.String()
}

// gridCall sends one request to the management API of a node of the cluster,
// with the headers that say who asks, and returns the status, the headers of
// the answer and its body.
func gridCall(ctx context.Context, c *clustertest.Cluster, id string, headers map[string]string, method, path string, body any) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+id+":9001/api/v1"+path, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s through %s: %w", method, path, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s through %s: %w", method, path, id, err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// gridList asks a node for the buckets with their details, as the caller the
// headers name, and returns them by name beside the answer as it was sent.
func gridList(ctx context.Context, c *clustertest.Cluster, id string, headers map[string]string) (map[string]gridBucket, []byte, error) {
	status, _, body, err := gridCall(ctx, c, id, headers, http.MethodGet, "/buckets?details=true", nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, body, fmt.Errorf("the buckets with details through %s = %d %s", id, status, body)
	}
	var listed []gridBucket
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, body, fmt.Errorf("the buckets with details through %s: %w: %s", id, err, body)
	}
	buckets := map[string]gridBucket{}
	for i, b := range listed {
		if b.Details == nil {
			return nil, body, fmt.Errorf("bucket %s of a list with details, through %s, has none: %s", b.Name, id, body)
		}
		if i > 0 && listed[i-1].Name >= b.Name {
			return nil, body, fmt.Errorf("the buckets through %s are not in name order: %s", id, body)
		}
		buckets[b.Name] = b
	}
	return buckets, body, nil
}

// gridBearer is the header an API key authenticates to the management API
// with.
func gridBearer(accessKeyID, secretKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + accessKeyID + ":" + secretKey}
}

// gridS3 returns an S3 client that talks to one node of the cluster with an
// API key other than root's. Like the harness's own it does not retry.
func gridS3(c *clustertest.Cluster, id, accessKeyID, secretKey string) *s3.Client {
	cfg := aws.Config{
		Region:                     "us-east-1",
		Credentials:                credentials.NewStaticCredentialsProvider(accessKeyID, secretKey, ""),
		HTTPClient:                 c.HTTP(id),
		Retryer:                    func() aws.Retryer { return retry.AddWithMaxAttempts(retry.NewStandard(), 1) },
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + id + ":9000")
		o.UsePathStyle = true
	})
}

// gridUser creates a user with the roles, and an API key of its own, through
// a node, and returns the key.
func gridUser(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string, name string, user map[string]any) (accessKeyID, secretKey string, err error) {
	user["name"] = name
	status, _, body, err := gridCall(ctx, c, id, root, http.MethodPost, "/users", user)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusCreated {
		return "", "", fmt.Errorf("create user %s = %d %s", name, status, body)
	}
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	// The answer holds the secret: it is not put into an error.
	status, _, body, err = gridCall(ctx, c, id, root, http.MethodPost, "/users/"+name+"/keys", map[string]any{})
	if err != nil {
		return "", "", err
	}
	if status != http.StatusCreated {
		return "", "", fmt.Errorf("create a key of %s = %d", name, status)
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return "", "", fmt.Errorf("create a key of %s: %w", name, err)
	}
	if created.Key.AccessKeyID == "" || created.SecretKey == "" {
		return "", "", fmt.Errorf("create a key of %s: the answer holds no key", name)
	}
	return created.Key.AccessKeyID, created.SecretKey, nil
}

// gridPresignedGet downloads an object with a URL the client's key signs,
// sent by whoever holds it: no header of the request says who.
func gridPresignedGet(ctx context.Context, c *clustertest.Cluster, id string, signer *s3.Client, bucketName, key string) ([]byte, error) {
	presigned, err := s3.NewPresignClient(signer).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("presign: %w", err)
	}
	signed, err := url.Parse(presigned.URL)
	if err != nil {
		return nil, fmt.Errorf("the presigned URL: %w", err)
	}
	if signed.Host != id+":9000" || signed.Query().Get("X-Amz-Signature") == "" {
		return nil, fmt.Errorf("the presigned URL is for %q, signed: %t", signed.Host, signed.Query().Get("X-Amz-Signature") != "")
	}
	req, err := http.NewRequestWithContext(ctx, presigned.Method, presigned.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("presigned GET: %w", err)
	}
	for name, values := range presigned.SignedHeader {
		if http.CanonicalHeaderKey(name) != "Host" {
			req.Header[name] = values
		}
	}
	resp, err := c.HTTP(id).Do(req)
	if err != nil {
		return nil, fmt.Errorf("presigned GET through %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("presigned GET through %s: %w", id, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presigned GET through %s = %d", id, resp.StatusCode)
	}
	return got, nil
}

// gridGet downloads an object with a signed request.
func gridGet(ctx context.Context, client *s3.Client, bucketName, key string) ([]byte, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("GetObject %s/%s: %w", bucketName, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	got, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("GetObject %s/%s: %w", bucketName, key, err)
	}
	return got, nil
}

// TestTheBucketGridAcrossTheCluster: what the console's grid of buckets is
// made of - who created a bucket, its latest uploads and downloads, a summary
// of its policy (ADR-0019) - in the product as it is deployed. One bucket is
// created through S3 on one node with the API key of a user, another through
// the management API on a second node by a user of the console. Objects are
// uploaded through a third - a multipart upload, plain puts, a copy - and
// downloaded through the first two, signed and with presigned URLs, by three
// callers. The list is read on the fourth: the creators and the uploads at
// once, the downloads when the nodes that served them have handed their
// counts in. Each part is shown to who may see it and left out for who may
// not, and every node gives the same answer.
func TestTheBucketGridAcrossTheCluster(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	// The shortest interval there is: the test waits for it.
	c := clustertest.Start(t, clustertest.Options{Env: map[string]string{"CACTUSFS_ACCESS_FLUSH_INTERVAL": "5s"}})
	root := gridBearer(c.AccessKey, c.SecretKey)
	const (
		viaS3    = "grid-s3"
		viaAdmin = "grid-admin"
		film     = "film/large.bin"
		notes    = "notes/plain.txt"
		copied   = "copies/large.bin"
		open     = "public/open.txt"
	)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// list reads the grid on a node, and fails the test when it cannot.
	list := func(id string, who map[string]string) (map[string]gridBucket, []byte) {
		t.Helper()
		buckets, body, err := gridList(ctx, c, id, who)
		must("the grid", err)
		return buckets, body
	}
	// The clocks of the nodes and of the test are the clocks of one host,
	// give or take what a virtual machine drifts by.
	began := time.Now()
	recent := func(at time.Time) bool {
		return !at.Before(began.Add(-time.Minute)) && !at.After(time.Now().Add(time.Minute))
	}
	// ordered says that rows are newest first, each at a time of this test.
	ordered := func(what string, entries []gridEntry) {
		t.Helper()
		for i, e := range entries {
			if !recent(e.At) {
				t.Errorf("%s: row %d is of %s, which is not during this test (begun %s)", what, i, e.At, began)
			}
			if i > 0 && e.At.After(entries[i-1].At) {
				t.Errorf("%s: row %d (%s) is newer than the row before it (%s)", what, i, e.At, entries[i-1].At)
			}
		}
	}

	// Who does what. Each user has a key; the operator signs in to the
	// console as well, with a password made up for this run.
	makerID, makerSecret, err := gridUser(ctx, c, "node4", root, "maker", map[string]any{"roles": []string{"bucket-admin"}})
	must("the user who creates through S3", err)
	writerID, writerSecret, err := gridUser(ctx, c, "node4", root, "writer", map[string]any{"roles": []string{"read-write"}})
	must("the user who uploads", err)
	readerID, readerSecret, err := gridUser(ctx, c, "node4", root, "reader", map[string]any{"roles": []string{"read-only"}})
	must("the user who reads", err)
	password := "console-" + c.SecretKey[:24]
	_, _, err = gridUser(ctx, c, "node4", root, "operator", map[string]any{"roles": []string{"bucket-admin"}, "password": password})
	must("the user of the console", err)
	reader := gridBearer(readerID, readerSecret)

	// One bucket through S3 on node1, by the key of a user.
	if _, err := gridS3(c, "node1", makerID, makerSecret).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(viaS3)}); err != nil {
		t.Fatalf("CreateBucket through node1 with the key of a user: %v", err)
	}
	// The other through the management API on node2, by who signed in
	// there: the cookie, and the CSRF token for what changes something.
	status, header, body, err := gridCall(ctx, c, "node2", nil, http.MethodPost, "/session", map[string]any{"username": "operator", "password": password})
	must("signing in on node2", err)
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	cookie := strings.SplitN(header.Get("Set-Cookie"), ";", 2)[0]
	if err := json.Unmarshal(body, &session); err != nil || status != http.StatusOK || session.CSRFToken == "" || !strings.HasPrefix(cookie, "__Host-cactusfs-session=") {
		t.Fatalf("signing in on node2 = %d (%v), a cookie of %d bytes", status, err, len(cookie))
	}
	operator := map[string]string{"Cookie": cookie}
	status, _, body, err = gridCall(ctx, c, "node2", map[string]string{"Cookie": cookie, "X-CSRF-Token": session.CSRFToken}, http.MethodPost, "/buckets", map[string]any{"name": viaAdmin})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("a bucket through the management API of node2 = %d %s (%v)", status, body, err)
	}

	// Uploads, all through node3. A multipart upload, completed by the
	// user who began it: one upload, at its completion.
	content := randomBytes(t, 300_000)
	writer := gridS3(c, "node3", writerID, writerSecret)
	upload, err := writer.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(viaS3), Key: aws.String(film)})
	must("CreateMultipartUpload through node3", err)
	part, err := writer.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(viaS3), Key: aws.String(film), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(content),
	})
	must("UploadPart through node3", err)
	// Not yet an upload: nothing is committed.
	if buckets, body := list("node4", root); buckets[viaS3].creator() != "user/maker" || bytes.Contains(body, []byte(film)) {
		t.Errorf("an upload in progress is among the latest uploads, or the bucket has no creator: %s", body)
	}
	_, err = writer.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(viaS3), Key: aws.String(film), UploadId: upload.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}},
	})
	must("CompleteMultipartUpload through node3", err)
	// A plain put by the user; a copy by another, whose source is the
	// first bucket; a put by root; and the user's key again, by root: four
	// uploads of three keys.
	note := []byte("a note, put plainly")
	_, err = writer.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(viaAdmin), Key: aws.String(notes), Body: bytes.NewReader(note)})
	must("PutObject through node3 with the key of a user", err)
	_, err = gridS3(c, "node3", makerID, makerSecret).CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(viaAdmin), Key: aws.String(copied), CopySource: aws.String(viaS3 + "/" + film),
	})
	must("CopyObject through node3", err)
	must("Put by root through node3", c.Put(ctx, "node3", viaAdmin, open, []byte("for anybody")))
	must("Put by root through node3, over the key of the user", c.Put(ctx, "node3", viaAdmin, notes, note))

	// Read on the fourth node, at once: who created each bucket, and the
	// uploads - one row a key, the latest first. Nobody has downloaded.
	const (
		wantS3Uploads    = film + " user/writer; "
		wantAdminUploads = notes + " root; " + open + " root; " + copied + " user/maker; "
	)
	buckets, body := list("node4", root)
	if len(buckets) != 2 {
		t.Fatalf("the grid on node4 has %d buckets, want the two: %s", len(buckets), body)
	}
	for name, want := range map[string]struct{ by, uploads string }{
		viaS3:    {"user/maker", wantS3Uploads},
		viaAdmin: {"user/operator", wantAdminUploads},
	} {
		d := buckets[name].Details
		uploads, downloads, shown := buckets[name].activity()
		if !shown || buckets[name].creator() != want.by {
			t.Fatalf("bucket %s on node4, for root: want its creator %s and its activity: %s", name, want.by, body)
		}
		if got := gridRows(uploads); got != want.uploads {
			t.Errorf("the latest uploads of %s = %q, want %q", name, got, want.uploads)
		}
		ordered("the uploads of "+name, uploads)
		if len(downloads) != 0 {
			t.Errorf("the latest downloads of %s before anybody downloaded = %q", name, gridRows(downloads))
		}
		// Root may read the policy, and there is none.
		if !d.PolicyIncluded || d.Policy != nil {
			t.Errorf("the policy of %s, which has none, for root: %s", name, body)
		}
		if !recent(buckets[name].CreatedAt) {
			t.Errorf("bucket %s is created at %s, which is not during this test", name, buckets[name].CreatedAt)
		}
	}

	// awaitDownloads waits until node4 says the latest downloads of a
	// bucket are these rows, in this order.
	awaitDownloads := func(name, want string) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			buckets, body := list("node4", root)
			_, downloads, shown := buckets[name].activity()
			if !shown {
				t.Fatalf("bucket %s on node4, for root: %s", name, body)
			}
			got := gridRows(downloads)
			if got == want {
				ordered("the downloads of "+name, downloads)
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the latest downloads of %s = %q; waited a minute for %q", name, got, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	same := func(what string, got []byte, err error) {
		t.Helper()
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("%s: %d bytes, %v", what, len(got), err)
		}
	}
	// Downloads, through the two nodes that neither took the uploads nor
	// are asked for the grid. Each node hands in what it counted within its
	// interval, so each is waited for before the next is made: the order of
	// the rows is then the order of the downloads. One: signed by root.
	got, err := c.Get(ctx, "node1", viaS3, film)
	same("Get by root through node1", got, err)
	awaitDownloads(viaS3, film+" root; ")
	// Two: with a URL the user signed, through node2.
	got, err = gridPresignedGet(ctx, c, "node2", gridS3(c, "node2", writerID, writerSecret), viaS3, film)
	same("a presigned GET through node2", got, err)
	awaitDownloads(viaS3, film+" user/writer presigned; "+film+" root; ")
	// Three: signed by the reader, through node1 - and from the other
	// bucket through node2.
	got, err = gridGet(ctx, gridS3(c, "node1", readerID, readerSecret), viaS3, film)
	same("Get by the reader through node1", got, err)
	if got, err := gridGet(ctx, gridS3(c, "node2", readerID, readerSecret), viaAdmin, notes); err != nil || !bytes.Equal(got, note) {
		t.Fatalf("Get by the reader through node2: %d bytes, %v", len(got), err)
	}
	awaitDownloads(viaS3, film+" user/reader; "+film+" user/writer presigned; "+film+" root; ")
	awaitDownloads(viaAdmin, notes+" user/reader; ")
	// Four: with a URL root signed, which is another reader than root: a
	// fourth row, and the oldest of the four goes.
	got, err = gridPresignedGet(ctx, c, "node2", c.S3("node2"), viaS3, film)
	same("a GET presigned by root through node2", got, err)
	const wantS3Downloads = film + " root presigned; " + film + " user/reader; " + film + " user/writer presigned; "
	awaitDownloads(viaS3, wantS3Downloads)

	// The copy read its source, and node3 hands that in like a download.
	// It is none: when it is in the record of the object, the bucket's
	// latest downloads are as they were, and its maker is not among them.
	c.Eventually("the copy to be recorded for its source", time.Minute, func() bool {
		status, _, body, err := gridCall(ctx, c, "node4", root, http.MethodGet, "/buckets/"+viaS3+"/object/access?key="+url.QueryEscape(film), nil)
		var access objectAccess
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &access) != nil {
			t.Fatalf("the record of the copied object = %d %s (%v)", status, body, err)
		}
		return access.CopyCount == 1
	})
	awaitDownloads(viaS3, wantS3Downloads)

	// Deleting adds no upload and takes no row away: an object deleted
	// for good, and one under a delete marker in a bucket that now keeps
	// versions.
	if _, err := c.S3("node3").DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(viaAdmin), Key: aws.String(notes)}); err != nil {
		t.Fatalf("DeleteObject through node3: %v", err)
	}
	if _, err := c.S3("node3").PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(viaS3), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning through node3: %v", err)
	}
	marker, err := c.S3("node3").DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(viaS3), Key: aws.String(film)})
	if err != nil || !aws.ToBool(marker.DeleteMarker) {
		t.Fatalf("DeleteObject in a bucket that keeps versions: a marker: %t, %v", aws.ToBool(marker.DeleteMarker), err)
	}
	if _, err := c.Get(ctx, "node1", viaS3, film); statusOf(err) != http.StatusNotFound {
		t.Fatalf("Get of an object under a delete marker = %v, want 404", err)
	}

	// Policies, through S3, on two other nodes: one that lets anybody read
	// a part of the bucket, of two statements; one that only forbids.
	_, err = c.S3("node1").PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(viaAdmin), Policy: aws.String(`{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::` + viaAdmin + `/public/*"},` +
		`{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::` + viaAdmin + `/public/*"}]}`)})
	must("PutBucketPolicy through node1", err)
	_, err = c.S3("node2").PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(viaS3), Policy: aws.String(`{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::` + viaS3 + `/kept/*"}]}`)})
	must("PutBucketPolicy through node2", err)
	// The policy does what its summary will say: without a credential.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://node2:9000/"+viaAdmin+"/"+open, nil)
	must("a request without a credential", err)
	resp, err := c.HTTP("node2").Do(req)
	must("a GET without a credential through node2", err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a GET without a credential of what the policy opens = %d", resp.StatusCode)
	}
	awaitDownloads(viaAdmin, open+" anonymous; "+notes+" user/reader; ")

	// The grid for root, on the fourth node, when all of it has happened.
	buckets, body = list("node4", root)
	for name, want := range map[string]struct {
		by, uploads, downloads string
		statements             int
		open                   bool
	}{
		viaS3:    {"user/maker", wantS3Uploads, wantS3Downloads, 1, false},
		viaAdmin: {"user/operator", wantAdminUploads, open + " anonymous; " + notes + " user/reader; ", 2, true},
	} {
		d := buckets[name].Details
		uploads, downloads, shown := buckets[name].activity()
		if !shown || buckets[name].creator() != want.by {
			t.Fatalf("bucket %s on node4, for root: want its creator %s and its activity: %s", name, want.by, body)
		}
		if got := gridRows(uploads); got != want.uploads {
			t.Errorf("the latest uploads of %s after the deletions = %q, want %q: a deletion is no upload, and removes none", name, got, want.uploads)
		}
		if got := gridRows(downloads); got != want.downloads {
			t.Errorf("the latest downloads of %s = %q, want %q", name, got, want.downloads)
		}
		if !d.PolicyIncluded || d.Policy == nil || d.Policy.Statements != want.statements || d.Policy.OpenToAnyone != want.open {
			t.Errorf("the policy of %s for root: want %d statements, open to anyone: %t: %s", name, want.statements, want.open, body)
		}
	}

	// parts says which parts of its details a list carries for a bucket,
	// from the answer as it was sent: a part that is left out is not there
	// at all, not there and empty.
	parts := func(body []byte, name string) string {
		t.Helper()
		var listed []struct {
			Name    string                     `json:"name"`
			Details map[string]json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal(body, &listed); err != nil {
			t.Fatalf("the grid: %v: %s", err, body)
		}
		for _, b := range listed {
			if b.Name != name {
				continue
			}
			var out []string
			for _, part := range []string{"access_included", "created_by", "activity", "policy_included", "policy"} {
				if raw, ok := b.Details[part]; ok {
					if part == "access_included" || part == "policy_included" {
						part += "=" + string(raw)
					}
					out = append(out, part)
				}
			}
			if len(b.Details) != len(out) {
				t.Errorf("the details of %s have parts the contract does not name: %s", name, body)
			}
			return strings.Join(out, " ")
		}
		t.Fatalf("the grid does not list %s: %s", name, body)
		return ""
	}
	for _, name := range []string{viaS3, viaAdmin} {
		// Who may only read objects sees the buckets, and of their
		// details nothing: not the creator, not who used them, not
		// whether there is a policy - though one of the two lets anybody
		// read a part of the bucket.
		_, body := list("node4", reader)
		if got := parts(body, name); got != "access_included=false policy_included=false" {
			t.Errorf("the details of %s for a user of read-only = %q: %s", name, got, body)
		}
		// Who administers buckets, and nothing else, sees the summary
		// of the policy - with the cookie of node2, on node4.
		buckets, body := list("node4", operator)
		if got := parts(body, name); got != "access_included=false policy_included=true policy" {
			t.Errorf("the details of %s for a user of bucket-admin = %q: %s", name, got, body)
		}
		if d := buckets[name].Details; d == nil || d.Policy == nil || d.Policy.OpenToAnyone != (name == viaAdmin) {
			t.Errorf("the policy of %s for a user of bucket-admin: %s", name, body)
		}
		_, body = list("node4", root)
		if got := parts(body, name); got != "access_included=true created_by activity policy_included=true policy" {
			t.Errorf("the details of %s for root = %q: %s", name, got, body)
		}
	}
	// And a list that was not asked for details carries none.
	status, _, body, err = gridCall(ctx, c, "node4", root, http.MethodGet, "/buckets", nil)
	if err != nil || status != http.StatusOK || bytes.Contains(body, []byte(`"details"`)) || !bytes.Contains(body, []byte(viaS3)) {
		t.Errorf("the buckets without details = %d %s (%v)", status, body, err)
	}

	// A third bucket, by root, through the management API of node3: on
	// every node at once, with its creator. Deleted through node2: on none.
	const gone = "grid-gone"
	if status, _, body, err := gridCall(ctx, c, "node3", root, http.MethodPost, "/buckets", map[string]any{"name": gone}); err != nil || status != http.StatusCreated {
		t.Fatalf("a bucket through the management API of node3 = %d %s (%v)", status, body, err)
	}
	for _, id := range clustertest.Nodes {
		buckets, body := list(id, root)
		uploads, downloads, shown := buckets[gone].activity()
		if !shown || buckets[gone].creator() != "root" || len(uploads)+len(downloads) != 0 {
			t.Errorf("the bucket root created, through %s: %s", id, body)
		}
	}
	if status, _, body, err := gridCall(ctx, c, "node2", root, http.MethodDelete, "/buckets/"+gone, nil); err != nil || status != http.StatusNoContent {
		t.Fatalf("deleting a bucket through node2 = %d %s (%v)", status, body, err)
	}

	// Every node gives the same answer, byte for byte. Nothing is on its
	// way any more: every download above was waited for.
	_, want := list("node4", root)
	for _, id := range clustertest.Nodes {
		buckets, body := list(id, root)
		if _, listed := buckets[gone]; listed || len(buckets) != 2 {
			t.Errorf("the grid through %s after a bucket was deleted: %s", id, body)
		}
		if !bytes.Equal(body, want) {
			t.Errorf("the grid through %s:\n%s\nthrough node4:\n%s", id, body, want)
		}
	}
	// No secret is in what the nodes logged.
	for _, id := range clustertest.Nodes {
		if logs := c.Logs(id); strings.Contains(logs, password) || strings.Contains(logs, session.CSRFToken) ||
			strings.Contains(logs, writerSecret) || strings.Contains(logs, c.SecretKey) {
			t.Errorf("a secret is in the log of %s", id)
		}
	}
}
