package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// siteRegion is the region both deployments of the test sign for.
const siteRegion = "eu-1"

// s3Of serves the S3 API of a node that was started, as server.go does,
// over TLS when secure is set.
func s3Of(t *testing.T, n *node, secure bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(s3api.NewHandler(s3api.Config{
		Buckets: n.buckets, Objects: n.objects, Identities: n.identities, Notifications: n.notifications, Replication: n.replication,
		Clock: n.clock, Region: siteRegion, Logger: slog.New(slog.DiscardHandler),
	}))
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

// s3Caller signs and sends requests to the S3 API of a node.
type s3Caller struct {
	t      *testing.T
	base   string
	client *http.Client
	creds  aws.Credentials
}

// do sends one request, signed with the payload's SHA-256, and returns the
// headers and the body of the answer.
func (c s3Caller) do(method, path string, body []byte) (http.Header, []byte) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if err := v4.NewSigner().SignHTTP(c.t.Context(), c.creds, req, payload, "s3", siteRegion, time.Now()); err != nil {
		c.t.Fatal(err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode/100 != 2 {
		c.t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, read)
	}
	return resp.Header, read
}

// TestANodeReplicatesToAnotherDeployment holds the composition root to the
// replicator (PLAN 8.1.4): a node whose bucket is sent to another
// deployment sends what is written there - a version in one PUT, a version
// of two parts as an upload of the same parts, a delete marker - over TLS
// to the S3 API of that deployment, through its egress client and with the
// key that deployment issued, and each version becomes COMPLETED once it
// is there, with its ID, its time and its bytes. The other deployment is a
// second node of its own, in the same process.
func TestANodeReplicatesToAnotherDeployment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// The other deployment, B: the bucket "copies", a replica of A.
	cfgB := nodeConfig(t.TempDir())
	cfgB.Region = siteRegion
	b := mustStart(t, cfgB)
	keyB := shownCredentials(t, b.notice)
	rootB := iam.Principal{AccessKeyID: keyB.AccessKeyID, Root: true}
	siteB := s3Of(t, b.node, true)

	// This deployment, A: the bucket "photos", sent to B.
	cfgA := nodeConfig(t.TempDir())
	cfgA.Region = siteRegion
	cfgA.EgressAllow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	cfgA.DeliveryTimeout = 5 * time.Second
	cfgA.ReplicationWorkers = 2
	a := mustStart(t, cfgA)
	keyA := shownCredentials(t, a.notice)
	rootA := iam.Principal{AccessKeyID: keyA.AccessKeyID, Root: true}
	source := a.node.publishers.source
	if object.ValidateReplicaSource(source) != nil {
		t.Fatalf("the ID of this deployment is %q", source)
	}

	if err := b.node.buckets.Create(ctx, rootB, "copies"); err != nil {
		t.Fatal(err)
	}
	if err := b.node.buckets.SetVersioning(ctx, rootB, "copies", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := b.node.replication.SetReplicaMode(ctx, rootB, "copies", source); err != nil {
		t.Fatalf("SetReplicaMode: %v", err)
	}
	if err := a.node.buckets.Create(ctx, rootA, "photos"); err != nil {
		t.Fatal(err)
	}
	if err := a.node.buckets.SetVersioning(ctx, rootA, "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := a.node.targets.Put(ctx, rootA, app.TargetInput{
		Name: "site-b", Kind: delivery.KindS3, Endpoint: siteB.URL, Region: siteRegion, Buckets: []string{"photos"},
		CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: siteB.Certificate().Raw}),
		Auth:  delivery.Auth{Mode: delivery.AuthSigV4, ID: keyB.AccessKeyID}, Secret: keyB.SecretKey,
	}); err != nil {
		t.Fatalf("Put of the target: %v", err)
	}
	if _, err := a.node.replication.SetConfiguration(ctx, rootA, "photos", bucket.ReplicationConfig{
		Target: "site-b", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true, Markers: true}},
	}); err != nil {
		t.Fatalf("SetConfiguration: %v", err)
	}
	a.node.startBackground(ctx, cfgA)

	writer := s3Caller{t: t, base: s3Of(t, a.node, false).URL, client: http.DefaultClient, creds: aws.Credentials{
		AccessKeyID: keyA.AccessKeyID, SecretAccessKey: keyA.SecretKey,
	}}
	written := map[string][]byte{}
	var versions []object.VersionRef
	var keys []string
	for i := range 3 {
		key := "docs/" + strconv.Itoa(i) + ".txt"
		data := make([]byte, 1000+i*70_000)
		_, _ = rand.Read(data)
		header, _ := writer.do(http.MethodPut, "/photos/"+key, data)
		ref, err := object.ParseVersionRef(header.Get("X-Amz-Version-Id"))
		if err != nil {
			t.Fatalf("the version ID of %s: %v", key, err)
		}
		written[ref.String()] = data
		versions, keys = append(versions, ref), append(keys, key)
	}
	// A version of two parts.
	_, created := writer.do(http.MethodPost, "/photos/movie?uploads", nil)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(created, &initiated); err != nil || initiated.UploadID == "" {
		t.Fatalf("CreateMultipartUpload = %s, %v", created, err)
	}
	contents := [][]byte{make([]byte, object.MinPartSize), make([]byte, 4321)}
	complete := "<CompleteMultipartUpload>"
	for i, part := range contents {
		_, _ = rand.Read(part)
		header, _ := writer.do(http.MethodPut, "/photos/movie?partNumber="+strconv.Itoa(i+1)+"&uploadId="+initiated.UploadID, part)
		complete += "<Part><PartNumber>" + strconv.Itoa(i+1) + "</PartNumber><ETag>" + header.Get("ETag") + "</ETag></Part>"
	}
	header, _ := writer.do(http.MethodPost, "/photos/movie?uploadId="+initiated.UploadID, []byte(complete+"</CompleteMultipartUpload>"))
	movie, err := object.ParseVersionRef(header.Get("X-Amz-Version-Id"))
	if err != nil {
		t.Fatalf("the version of the parts: %v", err)
	}
	written[movie.String()] = bytes.Join(contents, nil)
	versions, keys = append(versions, movie), append(keys, "movie")
	// A delete marker, which the rule copies.
	header, _ = writer.do(http.MethodDelete, "/photos/docs/0.txt", nil)
	marker, err := object.ParseVersionRef(header.Get("X-Amz-Version-Id"))
	if err != nil {
		t.Fatalf("the version of the marker: %v", err)
	}

	// Every version and the marker become COMPLETED at A.
	deadline := time.Now().Add(60 * time.Second)
	for i, ref := range versions {
		for {
			v, err := a.node.objects.HeadVersion(ctx, rootA, "photos", keys[i], ref)
			if err != nil {
				t.Fatal(err)
			}
			if v.Replication == object.ReplicationCompleted {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s is %q, not COMPLETED", keys[i], ref, v.Replication)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// What arrived at B: each version under its ID, with its bytes and
	// as a replica; the marker as a marker.
	for i, ref := range versions {
		out, err := b.node.objects.Get(ctx, rootB, app.GetInput{Bucket: "copies", Key: keys[i], Version: &ref})
		if err != nil {
			t.Fatalf("the copy of %s at B: %v", keys[i], err)
		}
		got, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if err != nil || !bytes.Equal(got, written[ref.String()]) {
			t.Errorf("the copy of %s at B has %d bytes (%v), want the %d written", keys[i], len(got), err, len(written[ref.String()]))
		}
		if out.Version.Replication != object.ReplicationReplica {
			t.Errorf("the copy of %s at B is %q, want REPLICA", keys[i], out.Version.Replication)
		}
		original, err := a.node.objects.HeadVersion(ctx, rootA, "photos", keys[i], ref)
		if err != nil {
			t.Fatal(err)
		}
		if out.Version.ETag != original.ETag || !out.Version.ModifiedAt.Equal(original.ModifiedAt) || out.Version.Seq != original.Seq {
			t.Errorf("the copy of %s at B is %+v, the version at A %+v", keys[i], out.Version, original)
		}
	}
	for {
		_, err := b.node.objects.HeadVersion(ctx, rootB, "copies", "docs/0.txt", marker)
		if errors.Is(err, domain.ErrMethodNotAllowed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the marker at B: %v, want a delete marker", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// And the marker is COMPLETED at A, as the versions are.
	for {
		v, err := a.node.meta.GetVersion(ctx, "photos", "docs/0.txt", marker)
		if err != nil || !v.DeleteMarker {
			t.Fatalf("the marker at A: %+v, %v", v, err)
		}
		if v.Replication == object.ReplicationCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the marker at A is %q, not COMPLETED", v.Replication)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// B holds the copies and nothing else: a client of B cannot write
	// into its replica bucket.
	if _, err := b.node.objects.Put(context.WithoutCancel(ctx), rootB, app.PutInput{
		Bucket: "copies", Key: "mine", Body: bytes.NewReader([]byte("x")), Size: 1,
	}); !errors.Is(err, domain.ErrInvalidBucketState) {
		t.Errorf("a put of B's own into its replica bucket = %v", err)
	}
}
