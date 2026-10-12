//go:build integration && fixturegen && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The names of the fixture. The test that opens it does not know them: it
// reads them from the manifest.
const (
	schema3Plain    = "schema-three"
	schema3Public   = "schema-three-public"
	schema3Versions = "schema-three-versions"
	schema3User     = "fixture-writer"
	schema3Policy   = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::schema-three-public/public/*"}]}`
)

// schema3Content is n bytes that say nothing and do not repeat: a chain of
// SHA-256 sums begun with the label.
func schema3Content(label string, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	sum := sha256.Sum256([]byte(label))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}

// TestWriteSchema3Fixture writes testdata/schema-3 with the binary of the
// tree it is run in. It was run once, at commit d2d6861:
//
//	go test -tags='integration fixturegen' -run TestWriteSchema3Fixture -count=1 ./test/integration/
//
// and must not be run again on a later tree: the fixture is what THAT binary
// wrote. It is kept so that it can be seen how the fixture came to be, and
// refuses to write over one that exists.
//
// The node is a standalone one of four drives. It runs in a temporary
// directory and hands over what it counted every five seconds, the least the
// binary allows; when it has stopped, its directories are copied to the
// fixture, with a manifest of what was written (schema3Contents). It holds:
//
//	schema-three           keeps no versions. Objects uploaded with the API
//	                       key of the user fixture-writer and one by root;
//	                       downloads signed by root and by the user, with a
//	                       URL root presigned, and through the management
//	                       API; one object copied, and its copy; one never
//	                       read; an upload in progress of "film", one part
//	schema-three-public    a bucket policy: anybody reads under public/. One
//	                       object there, downloaded without a credential, and
//	                       one beside it that the policy does not reach
//	schema-three-versions  keeps versions. "doc" in two versions, by root and
//	                       by the user, the older one downloaded by its ID;
//	                       "gone", one version under a delete marker
//
// What entry schema 3 also brought is NOT in it: sub-buckets, tag rules, tag
// definitions and the tags of an upload being created (ADR-0016, -0017,
// -0018). The binary of d2d6861 has their commands and their place in the
// state, and nothing that proposes them: no route of the S3 API or of the
// management API reaches them, their use cases are not built. A fixture is
// what the binary's own APIs wrote, so they are left out, and the state of
// the fixture has the buckets "access" and "accessflush" and neither
// "subbuckets" nor "tagdefs". TestUpgradeFromSchema2ToSchema3 writes them
// through the metadata store.
func TestWriteSchema3Fixture(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", schema3Dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err == nil {
		t.Fatalf("%s exists: the fixture is written once, by the binary it is a fixture of", fixture)
	}
	commit, err := exec.CommandContext(t.Context(), "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("which commit this tree is: %v", err)
	}
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	node := schema3Server(ctx, t, bin, dir, "CACTUSFS_ACCESS_FLUSH_INTERVAL=5s")
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// The user of the deployment, and the key it uploads with.
	if status, _, body := adminCall(ctx, t, http.MethodPost, node.admin+"/users", schema3RootBearer(), map[string]any{"name": schema3User, "roles": []string{"read-write"}}); status != http.StatusCreated {
		t.Fatalf("create user = %d %s", status, body)
	}
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	status, _, body := adminCall(ctx, t, http.MethodPost, node.admin+"/users/"+schema3User+"/keys", schema3RootBearer(), map[string]any{})
	if err := json.Unmarshal(body, &created); err != nil || status != http.StatusCreated || created.SecretKey == "" {
		t.Fatalf("create key = %d (%v)", status, err)
	}
	contents := &schema3Contents{
		Commit: strings.TrimSpace(string(commit)),
		Writer: schema3Writer{User: schema3User, Actor: "user/" + schema3User, AccessKeyID: created.Key.AccessKeyID, SecretKey: created.SecretKey},
	}
	user := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	clients := map[string]*s3.Client{schema3Root: node.root, contents.Writer.Actor: user}

	bucket := func(name string, versions bool) *schema3Bucket {
		t.Helper()
		_, err := node.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
		must("CreateBucket "+name, err)
		if versions {
			_, err = node.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
				Bucket: aws.String(name), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
			})
			must("PutBucketVersioning "+name, err)
		}
		b := &schema3Bucket{Name: name, Versions: versions, Markers: []schema3Marker{}, Uploads: []schema3Upload{}}
		contents.Buckets = append(contents.Buckets, b)
		return b
	}
	// record writes a new version down, over whatever its key had.
	record := func(b *schema3Bucket, who, key, version string, content []byte) *schema3Object {
		for _, o := range b.Objects {
			if o.Key == key {
				o.Latest = false
			}
		}
		o := &schema3Object{Key: key, VersionID: version, Latest: true, Size: int64(len(content)), SHA256: sumOf(content), UploadedBy: who, Readers: []schema3Reader{}}
		b.Objects = append(b.Objects, o)
		return o
	}
	put := func(b *schema3Bucket, who, key string, content []byte) *schema3Object {
		t.Helper()
		out, err := clients[who].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key), Body: bytes.NewReader(content)})
		must("PutObject "+key, err)
		return record(b, who, key, aws.ToString(out.VersionId), content)
	}
	// download reads a version, signed, as often as it says.
	download := func(b *schema3Bucket, o *schema3Object, who string, times int) {
		t.Helper()
		for range times {
			got, err := schema3Get(ctx, clients[who], b.Name, o)
			must("GetObject "+o.Key, err)
			if sumOf(got) != o.SHA256 {
				t.Fatalf("GetObject %s: not what was put", o.Key)
			}
			o.count(who, false, 1, 0)
		}
	}
	// fetch reads a version over plain HTTP: with a presigned URL, without
	// a credential, or through the management API.
	fetch := func(o *schema3Object, address string, headers map[string]string, who string, presigned bool) {
		t.Helper()
		status, _, got := adminCall(ctx, t, http.MethodGet, address, headers, nil)
		if status != http.StatusOK || sumOf(got) != o.SHA256 {
			t.Fatalf("download of %s as %s = %d, %d bytes", o.Key, who, status, len(got))
		}
		o.count(who, presigned, 1, 0)
	}

	// (a) The bucket that keeps no versions.
	plain := bucket(schema3Plain, false)
	q1 := put(plain, contents.Writer.Actor, "reports/2026/q1.txt", []byte("first quarter"))
	q2 := put(plain, contents.Writer.Actor, "reports/2026/q2.txt", []byte("second quarter"))
	summary := put(plain, contents.Writer.Actor, "reports/summary.txt", schema3Content("summary", 3_000))
	blob := put(plain, contents.Writer.Actor, "data/blob.bin", schema3Content("blob", 4_096))
	put(plain, schema3Root, "notes/unread.txt", []byte("nobody reads this"))
	download(plain, q1, schema3Root, 3)
	download(plain, q1, contents.Writer.Actor, 2)
	download(plain, summary, contents.Writer.Actor, 1)
	presigned, err := s3.NewPresignClient(node.root).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(plain.Name), Key: aws.String(q2.Key)})
	must("presign", err)
	headers := map[string]string{}
	for name, values := range presigned.SignedHeader {
		if http.CanonicalHeaderKey(name) != "Host" {
			headers[name] = values[0]
		}
	}
	fetch(q2, presigned.URL, headers, schema3Root, true)
	fetch(summary, node.admin+"/buckets/"+plain.Name+"/object?key="+url.QueryEscape(summary.Key), schema3RootBearer(), schema3Root, false)
	// The copy: root reads the blob, as a copy, and is who uploaded what
	// it became.
	_, err = node.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(plain.Name), Key: aws.String("copies/blob.bin"), CopySource: aws.String(plain.Name + "/" + blob.Key)})
	must("CopyObject", err)
	blob.count(schema3Root, false, 0, 1)
	record(plain, schema3Root, "copies/blob.bin", "", schema3Content("blob", 4_096))

	// (d) The upload in progress, by the user. Its part is small: it is
	// the last, and a last part may be.
	reel := schema3Content("reel one", 5_000)
	upload, err := user.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String("film")})
	must("CreateMultipartUpload", err)
	_, err = user.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(plain.Name), Key: aws.String("film"), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(reel)})
	must("UploadPart", err)
	plain.Uploads = append(plain.Uploads, schema3Upload{Key: "film", UploadID: aws.ToString(upload.UploadId), Parts: []schema3Part{{Number: 1, Size: int64(len(reel)), SHA256: sumOf(reel)}}})

	// (b) The bucket with a policy.
	public := bucket(schema3Public, false)
	public.Policy = schema3Policy
	_, err = node.root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(public.Name), Policy: aws.String(schema3Policy)})
	must("PutBucketPolicy", err)
	readme := put(public, contents.Writer.Actor, "public/readme.txt", []byte("for everybody"))
	put(public, schema3Root, "private/ledger.txt", []byte("for nobody else"))
	for range 2 {
		fetch(readme, node.s3+"/"+public.Name+"/"+readme.Key, nil, "anonymous", false)
	}

	// (c) The bucket that keeps versions.
	versions := bucket(schema3Versions, true)
	older := put(versions, schema3Root, "doc", []byte("first"))
	put(versions, contents.Writer.Actor, "doc", []byte("second"))
	gone := put(versions, contents.Writer.Actor, "gone", []byte("soon"))
	download(versions, older, schema3Root, 1)
	marker, err := node.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(versions.Name), Key: aws.String(gone.Key)})
	must("DeleteObject", err)
	gone.Latest = false
	versions.Markers = append(versions.Markers, schema3Marker{Key: gone.Key, VersionID: aws.ToString(marker.VersionId)})
	if older.VersionID == "" || aws.ToString(marker.VersionId) == "" {
		t.Fatalf("the versioned bucket gave the version %q and the marker %q", older.VersionID, aws.ToString(marker.VersionId))
	}

	// Nothing is a fixture of reads that only a node's memory held: the
	// node hands its counts over through the metadata group, and is not
	// stopped before the management API shows every one of them.
	deadline := time.Now().Add(time.Minute)
	for {
		differs := schema3Recorded(ctx, t, node, contents)
		if len(differs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reads were not recorded as they were made:\n%s", strings.Join(differs, "\n"))
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	node.srv.stop(t)
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) {
		t.Fatal("a secret was written to standard error")
	}

	// What the state says of itself goes into the manifest, and the
	// stopped node's directories, with the manifest, are the fixture.
	contents.StateSchema, contents.StateBuckets = stateOf(t, filepath.Join(dir, "meta"))
	if contents.StateSchema != 3 {
		t.Fatalf("the state is of schema %d: this tree does not write a fixture of schema 3", contents.StateSchema)
	}
	manifest, err := json.MarshalIndent(contents, "", "  ")
	must("the manifest", err)
	must("the fixture's directory", os.MkdirAll(fixture, 0o700))
	copyTree(t, dir, fixture)
	// The repository keeps no empty directory, so the fixture has none:
	// what is opened from a clone is then exactly what is here. The binary
	// makes the ones it wants (a drive's tmp, the snapshots) when it
	// starts.
	var dirs []string
	must("the fixture's directories", filepath.WalkDir(fixture, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}))
	for _, path := range slices.Backward(dirs) {
		entries, err := os.ReadDir(path)
		must("the fixture's directories", err)
		if len(entries) == 0 {
			must("an empty directory of the fixture", os.Remove(path))
		}
	}
	must("the manifest", os.WriteFile(filepath.Join(fixture, schema3Manifest), append(manifest, '\n'), 0o600))
}
