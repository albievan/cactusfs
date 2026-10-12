//go:build integration && !windows

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The fixture in testdata/schema-3 is the data of a standalone deployment -
// four drives, the metadata directory and the sealed key file - as the
// binary of commit d2d6861 wrote it: entry schema 3, state schema 3
// (ADR-0016), the last before whatever schema 4 brings. It was written by
// TestWriteSchema3Fixture (schema3_fixture_test.go) and is never written
// again: a later binary cannot write what an earlier one would have.
//
// What it holds is not repeated here. The generator wrote it down, beside
// the data, in manifest.json: the buckets, every version with its size, the
// SHA-256 of its content, who uploaded it, how often it was downloaded and
// copied and by whom, the delete markers and the upload in progress. The test
// below asserts against that and has no figures of its own.
//
// Its passphrase and root credentials are the ones of the other fixtures.
// The manifest also holds the API key of the one user of the deployment: it
// is test data like them, of a deployment that exists nowhere but here.
const (
	schema3Dir      = "schema-3"
	schema3Manifest = "manifest.json"
	// schema3Root is what the audit trail, and so a version, calls root.
	schema3Root = "root"
)

// schema3Reader is one reader of a version, as the record of reads names it.
type schema3Reader struct {
	Who       string `json:"who"`
	Presigned bool   `json:"presigned"`
	Downloads int64  `json:"downloads"`
	Copies    int64  `json:"copies"`
}

// schema3Object is one version of an object of the fixture.
type schema3Object struct {
	Key string `json:"key"`
	// VersionID is empty where the bucket keeps no versions.
	VersionID  string          `json:"version_id,omitempty"`
	Latest     bool            `json:"latest"`
	Size       int64           `json:"size"`
	SHA256     string          `json:"sha256"`
	UploadedBy string          `json:"uploaded_by"`
	Downloads  int64           `json:"downloads"`
	Copies     int64           `json:"copies"`
	Readers    []schema3Reader `json:"readers"`
}

// count adds reads of the version by one reader to what is expected of it.
func (o *schema3Object) count(who string, presigned bool, downloads, copies int64) {
	o.Downloads += downloads
	o.Copies += copies
	for i := range o.Readers {
		if o.Readers[i].Who == who && o.Readers[i].Presigned == presigned {
			o.Readers[i].Downloads += downloads
			o.Readers[i].Copies += copies
			return
		}
	}
	o.Readers = append(o.Readers, schema3Reader{Who: who, Presigned: presigned, Downloads: downloads, Copies: copies})
}

// schema3Rows puts readers in one order and in words, to be compared.
func schema3Rows(readers []schema3Reader) string {
	rows := make([]string, 0, len(readers))
	for _, r := range readers {
		rows = append(rows, fmt.Sprintf("%s presigned=%t downloads=%d copies=%d", r.Who, r.Presigned, r.Downloads, r.Copies))
	}
	slices.Sort(rows)
	return strings.Join(rows, "; ")
}

// schema3Marker is a delete marker of the fixture.
type schema3Marker struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
}

// schema3Part is one part of an upload in progress.
type schema3Part struct {
	Number int32  `json:"number"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// schema3Upload is a multipart upload the fixture was left with.
type schema3Upload struct {
	Key      string        `json:"key"`
	UploadID string        `json:"upload_id"`
	Parts    []schema3Part `json:"parts"`
}

// schema3Bucket is one bucket of the fixture.
type schema3Bucket struct {
	Name     string `json:"name"`
	Versions bool   `json:"versions"`
	// Policy is the bucket policy as it was put, or empty.
	Policy  string           `json:"policy,omitempty"`
	Objects []*schema3Object `json:"objects"`
	Markers []schema3Marker  `json:"markers"`
	Uploads []schema3Upload  `json:"uploads"`
}

// schema3Writer is the user of the fixture and the API key it uploaded with.
type schema3Writer struct {
	User        string `json:"user"`
	Actor       string `json:"actor"`
	AccessKeyID string `json:"access_key_id"`
	SecretKey   string `json:"secret_key"`
}

// schema3Contents is manifest.json: what the generator wrote into the
// fixture, for the test that opens it.
type schema3Contents struct {
	// Commit is the tree whose binary wrote the fixture.
	Commit string `json:"commit"`
	// StateSchema and StateBuckets are what the state database said of
	// itself when the node had stopped (stateOf).
	StateSchema  uint64           `json:"state_schema"`
	StateBuckets []string         `json:"state_buckets"`
	Writer       schema3Writer    `json:"writer"`
	Buckets      []*schema3Bucket `json:"buckets"`
}

// schema3Node is a running standalone node on a schema-3 deployment.
type schema3Node struct {
	srv *server
	// root is an S3 client with the root credentials of the fixtures.
	root *s3.Client
	// s3 and admin are the bases of the two listeners, with their scheme.
	s3, admin string
}

// client is an S3 client of the node with the given API key.
func (n *schema3Node) client(accessKey, secretKey string) *s3.Client {
	return s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(n.s3)
		o.UsePathStyle = true
	})
}

// schema3Server starts this tree's binary on a deployment of four drives in
// dir, as schema2Server does, with whatever more the environment is to say.
func schema3Server(ctx context.Context, t *testing.T, bin, dir string, env ...string) *schema3Node {
	t.Helper()
	srv := startServerWith(ctx, t, bin, []string{
		"server",
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"),
		"--data", filepath.Join(dir, "drive3"), "--data", filepath.Join(dir, "drive4"),
		"--meta-dir", filepath.Join(dir, "meta"), "--kms", "local", "--kms-key-file", filepath.Join(dir, "kms.json"),
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
	}, append([]string{
		"CACTUSFS_DEV_PLAINTEXT=true", "CACTUSFS_KMS_PASSPHRASE=" + fixturePassphrase,
		"CACTUSFS_ROOT_ACCESS_KEY=" + fixtureAccessKey, "CACTUSFS_ROOT_SECRET_KEY=" + fixtureSecretKey,
	}, env...)...)
	addrs := waitForListening(t, srv.logs)
	node := &schema3Node{srv: srv, s3: "http://" + addrs["s3"], admin: "http://" + addrs["admin"] + "/api/v1"}
	node.root = node.client(fixtureAccessKey, fixtureSecretKey)
	return node
}

// schema3RootBearer is the root credential as the management API takes it.
func schema3RootBearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + fixtureAccessKey + ":" + fixtureSecretKey}
}

// sumOf is the SHA-256 of content, as the manifest writes it.
func sumOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// schema3Get downloads one version through S3, whole.
func schema3Get(ctx context.Context, client *s3.Client, bucket string, o *schema3Object) ([]byte, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(o.Key)}
	if o.VersionID != "" {
		in.VersionId = aws.String(o.VersionID)
	}
	out, err := client.GetObject(ctx, in)
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

// schema3Recorded asks the node, as root, who uploaded and who read every
// version of the manifest, and says what differs from the manifest: nothing
// when the record is what was written down.
func schema3Recorded(ctx context.Context, t *testing.T, node *schema3Node, contents *schema3Contents) []string {
	t.Helper()
	var differs []string
	for _, b := range contents.Buckets {
		for _, o := range b.Objects {
			path := node.admin + "/buckets/" + b.Name + "/object/access?key=" + url.QueryEscape(o.Key)
			if o.VersionID != "" {
				path += "&version_id=" + o.VersionID
			}
			status, _, body := adminCall(ctx, t, http.MethodGet, path, schema3RootBearer(), nil)
			var access objectAccess
			if err := json.Unmarshal(body, &access); err != nil || status != http.StatusOK {
				t.Fatalf("access of %s/%s %s = %d %s (%v)", b.Name, o.Key, o.VersionID, status, body, err)
			}
			readers := make([]schema3Reader, 0, len(access.Readers))
			for _, r := range access.Readers {
				readers = append(readers, schema3Reader{Who: r.Who, Presigned: r.Presigned, Downloads: r.Downloads, Copies: r.Copies})
			}
			got := fmt.Sprintf("by %s, %d downloads, %d copies, %d others: %s", access.UploadedBy, access.DownloadCount, access.CopyCount, access.OtherReaders, schema3Rows(readers))
			want := fmt.Sprintf("by %s, %d downloads, %d copies, %d others: %s", o.UploadedBy, o.Downloads, o.Copies, 0, schema3Rows(o.Readers))
			if got != want || access.UploadedAt.IsZero() || (access.LastDownloadedAt == nil) != (o.Downloads+o.Copies == 0) {
				differs = append(differs, fmt.Sprintf("%s/%s %s: %s; want %s", b.Name, o.Key, o.VersionID, got, want))
			}
		}
	}
	return differs
}

// TestUpgradeFromSchema3: a deployment written with entry schema 3 is opened
// by this binary. Nothing is lost: every version reads back as the bytes it
// was, with who uploaded it and who read it how often; the bucket policy
// still lets anybody read what it let anybody read; versions and the delete
// marker are where they were; the upload that was in progress can be listed
// and finished; the user's API key still signs; and what is read and written
// afterwards is added to the record, not put in its place.
func TestUpgradeFromSchema3(t *testing.T) {
	t.Parallel()
	// Built before the clock of the test starts: on a cold cache the build
	// alone can take longer than the test may.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	// A copy: the fixture itself is never opened.
	copyTree(t, filepath.Join("testdata", schema3Dir), dir)
	raw, err := fs.ReadFile(os.DirFS(dir), schema3Manifest)
	if err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	var contents schema3Contents
	if err := json.Unmarshal(raw, &contents); err != nil || len(contents.Buckets) == 0 || contents.Writer.SecretKey == "" {
		t.Fatalf("the manifest of the fixture: %d buckets, %v", len(contents.Buckets), err)
	}
	if schema, buckets := stateOf(t, filepath.Join(dir, "meta")); schema != contents.StateSchema || !slices.Equal(buckets, contents.StateBuckets) {
		t.Fatalf("the fixture is not the state its manifest says: schema %d, buckets %v; want %d and %v", schema, buckets, contents.StateSchema, contents.StateBuckets)
	}

	node := schema3Server(ctx, t, bin, dir)
	writer := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	// Who uploaded and who read, first: nothing of this test has been
	// read yet, so the record is the fixture's own.
	for _, differs := range schema3Recorded(ctx, t, node, &contents) {
		t.Errorf("the record of the schema-3 deployment: %s", differs)
	}

	uploads, anonymous := 0, 0
	for _, b := range contents.Buckets {
		// Every version, by its ID where it has one, is the bytes it was.
		// Root reads them, and that is counted.
		for _, o := range b.Objects {
			got, err := schema3Get(ctx, node.root, b.Name, o)
			if err != nil || int64(len(got)) != o.Size || sumOf(got) != o.SHA256 {
				t.Errorf("%s/%s %s of the schema-3 deployment: %d bytes, %v; want %d", b.Name, o.Key, o.VersionID, len(got), err, o.Size)
				continue
			}
			o.count(schema3Root, false, 1, 0)
		}

		// The versions and the markers, as the bucket lists them.
		listed, err := node.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(b.Name)})
		if err != nil {
			t.Fatalf("versions of %s: %v", b.Name, err)
		}
		if len(listed.Versions) != len(b.Objects) || len(listed.DeleteMarkers) != len(b.Markers) {
			t.Fatalf("versions of %s: %d and %d markers; want %d and %d", b.Name, len(listed.Versions), len(listed.DeleteMarkers), len(b.Objects), len(b.Markers))
		}
		want := map[string]bool{}
		for _, o := range b.Objects {
			id := o.VersionID
			if id == "" {
				id = "null"
			}
			want[o.Key+" "+id] = o.Latest
		}
		for _, v := range listed.Versions {
			name := aws.ToString(v.Key) + " " + aws.ToString(v.VersionId)
			if latest, ok := want[name]; !ok || latest != aws.ToBool(v.IsLatest) {
				t.Errorf("%s lists the version %s, latest %t; the manifest: there %t, latest %t", b.Name, name, aws.ToBool(v.IsLatest), ok, latest)
			}
		}
		for _, m := range b.Markers {
			if !slices.ContainsFunc(listed.DeleteMarkers, func(d types.DeleteMarkerEntry) bool {
				return aws.ToString(d.Key) == m.Key && aws.ToString(d.VersionId) == m.VersionID && aws.ToBool(d.IsLatest)
			}) {
				t.Errorf("%s has no delete marker %s of %s that is the latest", b.Name, m.VersionID, m.Key)
			}
			if _, err := node.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.Name), Key: aws.String(m.Key)}); apiCode(err) != "NoSuchKey" {
				t.Errorf("the object under the delete marker of %s/%s: %v", b.Name, m.Key, err)
			}
		}
		state, err := node.root.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(b.Name)})
		if err != nil || (state.Status == types.BucketVersioningStatusEnabled) != b.Versions {
			t.Errorf("versioning of %s = %q, %v; the manifest says %t", b.Name, state.Status, err, b.Versions)
		}

		// The policy, as it was written - and it still decides: who it
		// lets read without a credential reads, and is counted as nobody
		// in particular; whom it does not, does not.
		if b.Policy != "" {
			policy, err := node.root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(b.Name)})
			if err != nil || aws.ToString(policy.Policy) != b.Policy {
				t.Errorf("the policy of %s = %s, %v", b.Name, aws.ToString(policy.Policy), err)
			}
		}
		for _, o := range b.Objects {
			if !o.Latest {
				continue
			}
			wasRead := slices.ContainsFunc(o.Readers, func(r schema3Reader) bool { return r.Who == "anonymous" })
			status, _, got := adminCall(ctx, t, http.MethodGet, node.s3+"/"+b.Name+"/"+o.Key, nil, nil)
			switch {
			case wasRead && (status != http.StatusOK || sumOf(got) != o.SHA256):
				t.Errorf("%s/%s without a credential = %d, %d bytes; the fixture's policy let it be read", b.Name, o.Key, status, len(got))
			case wasRead:
				o.count("anonymous", false, 1, 0)
				anonymous++
			case status != http.StatusForbidden:
				t.Errorf("%s/%s without a credential = %d, want 403", b.Name, o.Key, status)
			}
		}

		// The uploads that were in progress are still there, with their
		// parts, and are finished by this binary - by root, who is then
		// the uploader of what they become.
		inProgress, err := node.root.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(b.Name)})
		// Asked first of the error alone: with one there is no answer to
		// count, and saying so must not be what fails.
		if err != nil {
			t.Fatalf("uploads in progress in %s: %v", b.Name, err)
		}
		if len(inProgress.Uploads) != len(b.Uploads) {
			t.Fatalf("uploads in progress in %s = %+v; want %d", b.Name, inProgress.Uploads, len(b.Uploads))
		}
		for _, u := range b.Uploads {
			uploads++
			if !slices.ContainsFunc(inProgress.Uploads, func(m types.MultipartUpload) bool {
				return aws.ToString(m.Key) == u.Key && aws.ToString(m.UploadId) == u.UploadID
			}) {
				t.Fatalf("%s does not list the upload %s of %s", b.Name, u.UploadID, u.Key)
			}
			parts, err := node.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(b.Name), Key: aws.String(u.Key), UploadId: aws.String(u.UploadID)})
			if err != nil || len(parts.Parts) != len(u.Parts) {
				t.Fatalf("parts of the upload of %s/%s = %+v, %v; want %d", b.Name, u.Key, parts, err, len(u.Parts))
			}
			var completed []types.CompletedPart
			size := int64(0)
			for i, p := range u.Parts {
				if aws.ToInt32(parts.Parts[i].PartNumber) != p.Number || aws.ToInt64(parts.Parts[i].Size) != p.Size {
					t.Fatalf("part %d of the upload of %s/%s: number %d, %d bytes; want %d and %d", i, b.Name, u.Key,
						aws.ToInt32(parts.Parts[i].PartNumber), aws.ToInt64(parts.Parts[i].Size), p.Number, p.Size)
				}
				completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: parts.Parts[i].ETag})
				size += p.Size
			}
			if _, err := node.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
				Bucket: aws.String(b.Name), Key: aws.String(u.Key), UploadId: aws.String(u.UploadID),
				MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
			}); err != nil {
				t.Fatalf("CompleteMultipartUpload of the old upload of %s/%s: %v", b.Name, u.Key, err)
			}
			// What it became is the parts, one after the other. With
			// one part the manifest says what that is; with more, the
			// parts are checked by their sizes alone. The fixture's
			// upload is in a bucket that keeps no versions, so what
			// it became is the null version of its key.
			finished := &schema3Object{Key: u.Key, Latest: true, Size: size, UploadedBy: schema3Root}
			got, err := schema3Get(ctx, node.root, b.Name, finished)
			if err != nil || int64(len(got)) != size {
				t.Fatalf("the finished upload of %s/%s: %d bytes, %v; want %d", b.Name, u.Key, len(got), err, size)
			}
			finished.SHA256 = sumOf(got)
			if len(u.Parts) == 1 && finished.SHA256 != u.Parts[0].SHA256 {
				t.Errorf("the finished upload of %s/%s is not the part that was uploaded", b.Name, u.Key)
			}
			finished.count(schema3Root, false, 1, 0)
			b.Objects = append(b.Objects, finished)
		}
		b.Uploads = nil
	}
	if uploads == 0 || anonymous == 0 {
		t.Errorf("the fixture had %d uploads in progress and %d objects anybody may read: the test proved less than it says", uploads, anonymous)
	}

	// What this binary adds works on the deployment: the user's key, kept
	// under the KMS key of the fixture, still signs - an upload and a
	// download of it, with the user as the uploader and the one reader.
	first := contents.Buckets[0]
	added := &schema3Object{Key: "after/upgrade.txt", Latest: true, UploadedBy: contents.Writer.Actor}
	content := []byte("written after the schema-3 deployment was opened")
	added.Size, added.SHA256 = int64(len(content)), sumOf(content)
	if first.Versions {
		t.Fatalf("the first bucket of the manifest, %s, keeps versions: the upload below would need its version ID", first.Name)
	}
	if _, err := writer.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(first.Name), Key: aws.String(added.Key), Body: strings.NewReader(string(content))}); err != nil {
		t.Fatalf("PutObject with the key of the fixture's user: %v", err)
	}
	if got, err := schema3Get(ctx, writer, first.Name, added); err != nil || sumOf(got) != added.SHA256 {
		t.Fatalf("GetObject with the key of the fixture's user: %d bytes, %v", len(got), err)
	}
	added.count(contents.Writer.Actor, false, 1, 0)
	first.Objects = append(first.Objects, added)
	node.srv.stop(t)

	// After a restart: what the node counted was handed over when it
	// stopped, and was added to what the fixture held; and everything
	// still reads.
	node = schema3Server(ctx, t, bin, dir)
	for _, differs := range schema3Recorded(ctx, t, node, &contents) {
		t.Errorf("the record after this binary read and wrote: %s", differs)
	}
	for _, b := range contents.Buckets {
		for _, o := range b.Objects {
			if got, err := schema3Get(ctx, node.root, b.Name, o); err != nil || sumOf(got) != o.SHA256 {
				t.Errorf("%s/%s %s after a restart: %d bytes, %v", b.Name, o.Key, o.VersionID, len(got), err)
			}
		}
	}
	node.srv.stop(t)
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) {
		t.Fatal("a secret was written to standard error")
	}
}
