//go:build integration && !windows

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// The fixture in testdata/schema-4 is the data of a standalone deployment -
// four drives, the metadata directory and the sealed key file - as the
// binary of commit 84334a7 wrote it: entry schema 4, state schema 4
// (ADR-0019), the last before whatever schema 5 brings. It was written by
// TestWriteSchema4Fixture (schema4_fixture_test.go) and is never written
// again: a later binary cannot write what an earlier one would have.
//
// What it holds is not repeated here. The generator wrote it down, beside
// the data, in manifest.json: the buckets with who created them and how they
// are configured, every version with its size, the SHA-256 of its content,
// who uploaded it, how often it was downloaded and copied and by whom, and
// what keeps it; the delete markers and the upload in progress; the record
// of each bucket's latest use, as the state holds it, byte for byte; and the
// users, groups, roles, policies and API keys. The test below asserts
// against that and has no figures of its own.
//
// Its passphrase and root credentials are the ones of the other fixtures.
// The manifest also holds the two API keys of the deployment's users: they
// are test data like them, of a deployment that exists nowhere but here. So
// is the password of its user of the console (schema4FixturePassword).
const (
	schema4FixtureDir      = "schema-4"
	schema4FixtureManifest = "manifest.json"
	// schema4FixtureFlush is how often the node hands over what it counted:
	// the least the binary allows, because the tests wait for it.
	schema4FixtureFlush = "CACTUSFS_ACCESS_FLUSH_INTERVAL=5s"
	// schema4FixturePage is how many keys the test that opens the fixture
	// asks for at a time when it walks a bucket, and schema4FixtureSteps how
	// many steps the longest of those walks must take at the least: the
	// fixture is there for a walk that does not end in one.
	schema4FixturePage  = 50
	schema4FixtureSteps = 6
)

// schema4FixturePassword is the password of the fixture's user of the
// console: made of what the fixtures already say in the open, and of nothing
// that is a secret anywhere.
func schema4FixturePassword() string {
	return "console-" + fixtureSecretKey[:24]
}

// schema4FixtureRetention is the retention a version of the fixture is under.
type schema4FixtureRetention struct {
	Mode  string    `json:"mode"`
	Until time.Time `json:"until"`
}

// schema4FixtureObject is one version of an object of the fixture: what the
// schema-3 fixture says of a version, and what keeps it.
type schema4FixtureObject struct {
	schema3Object
	// Retention is nil where the version is under none.
	Retention *schema4FixtureRetention `json:"retention,omitempty"`
	LegalHold bool                     `json:"legal_hold,omitempty"`
	// ETag and Parts are those of a version that a multipart upload became:
	// its entity tag, without the quotes, and the parts it was completed
	// with. Both are empty for any other version.
	ETag  string        `json:"etag,omitempty"`
	Parts []schema3Part `json:"parts,omitempty"`
}

// schema4FixtureUsage is what a bucket of the fixture holds, in figures. The
// generator counted them from what it wrote, not from what the node said.
type schema4FixtureUsage struct {
	// Objects is how many keys have a current version that is not a delete
	// marker, and CurrentBytes the sizes of those versions together.
	Objects      int64 `json:"objects"`
	CurrentBytes int64 `json:"current_bytes"`
	// Versions is every stored version, the ones that are no longer current
	// too, and StoredBytes their sizes together: the bytes of the objects,
	// not what erasure coding makes of them. Markers is the delete markers,
	// which are not among the versions.
	Versions    int64 `json:"versions"`
	Markers     int64 `json:"markers"`
	StoredBytes int64 `json:"stored_bytes"`
	// Uploads is the multipart uploads in progress, and UploadBytes the
	// sizes of their parts together.
	Uploads     int64 `json:"uploads"`
	UploadBytes int64 `json:"upload_bytes"`
}

// schema4FixtureActivity is the record of a bucket's latest use: its rows,
// newest first, and the record as the state holds it.
type schema4FixtureActivity struct {
	Uploads   []schema4Upload   `json:"uploads"`
	Downloads []schema4Download `json:"downloads"`
	// Record is the stored record, byte for byte, and RecordSHA256 its
	// SHA-256. Both are empty for a bucket the state holds no record of.
	Record       string `json:"record"`
	RecordSHA256 string `json:"record_sha256"`
}

// uploaded puts an upload at the front of the rows, as ADR-0019 says: a key
// has one row, and no more rows are kept than the product keeps.
func (a *schema4FixtureActivity) uploaded(by, key string) {
	a.Uploads = slices.DeleteFunc(a.Uploads, func(u schema4Upload) bool { return u.Key == key })
	a.Uploads = slices.Insert(a.Uploads, 0, schema4Upload{By: by, Key: key})
	a.Uploads = a.Uploads[:min(len(a.Uploads), bucket.MaxRecentUploads)]
}

// downloaded puts a download at the front of the rows: a key, a reader and
// whether the reader came with a presigned URL have one row.
func (a *schema4FixtureActivity) downloaded(by, key string, presigned bool) {
	a.Downloads = slices.DeleteFunc(a.Downloads, func(d schema4Download) bool {
		return d.Key == key && d.By == by && d.Presigned == presigned
	})
	a.Downloads = slices.Insert(a.Downloads, 0, schema4Download{By: by, Presigned: presigned, Key: key})
	a.Downloads = a.Downloads[:min(len(a.Downloads), bucket.MaxRecentDownloads)]
}

// entries gives the rows as the list of buckets gives them, to be compared.
func (a *schema4FixtureActivity) entries() (uploads, downloads []gridEntry) {
	for _, u := range a.Uploads {
		uploads = append(uploads, gridEntry{At: u.At, By: u.By, Key: aws.String(u.Key)})
	}
	for _, d := range a.Downloads {
		downloads = append(downloads, gridEntry{At: d.At, By: d.By, Key: aws.String(d.Key), Presigned: d.Presigned})
	}
	return uploads, downloads
}

// schema4FixtureRows writes rows on one line, in their order, as gridRows
// does - and, where the times are to be compared too, the times after them.
func schema4FixtureRows(entries []gridEntry, times bool) string {
	out := gridRows(entries)
	if !times {
		return out
	}
	for _, e := range entries {
		out += " " + e.At.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// schema4FixtureLifecycleRule is a rule of a bucket's lifecycle as it was put.
type schema4FixtureLifecycleRule struct {
	ID             string `json:"id"`
	Prefix         string `json:"prefix"`
	ExpirationDays int32  `json:"expiration_days"`
}

// schema4FixtureCORSRule is a rule of a bucket's CORS as it was put.
type schema4FixtureCORSRule struct {
	AllowedOrigins []string `json:"allowed_origins"`
	AllowedMethods []string `json:"allowed_methods"`
	MaxAgeSeconds  int32    `json:"max_age_seconds"`
}

// schema4FixtureBucket is one bucket of the fixture.
type schema4FixtureBucket struct {
	Name string `json:"name"`
	// CreatedBy is who created the bucket, as the audit trail names actors,
	// and CreatedThrough the API it was created through: "s3" or "admin".
	CreatedBy      string `json:"created_by"`
	CreatedThrough string `json:"created_through"`
	Versions       bool   `json:"versions"`
	// ObjectLock says the bucket was created with Object Lock.
	ObjectLock bool `json:"object_lock"`
	// Note says what whoever reads the manifest should know of the bucket.
	Note string `json:"note,omitempty"`
	// Policy is the bucket policy as it was put, or empty.
	Policy    string                        `json:"policy,omitempty"`
	Lifecycle []schema4FixtureLifecycleRule `json:"lifecycle,omitempty"`
	CORS      []schema4FixtureCORSRule      `json:"cors,omitempty"`
	Tags      map[string]string             `json:"tags,omitempty"`
	Objects   []*schema4FixtureObject       `json:"objects"`
	Markers   []schema3Marker               `json:"markers"`
	Uploads   []schema3Upload               `json:"uploads"`
	// Usage is what the three lists above come to (usage).
	Usage    schema4FixtureUsage    `json:"usage"`
	Activity schema4FixtureActivity `json:"activity"`
}

// usage counts what the bucket holds from its versions, its delete markers
// and its uploads in progress as they are written down.
func (b *schema4FixtureBucket) usage() schema4FixtureUsage {
	u := schema4FixtureUsage{Versions: int64(len(b.Objects)), Markers: int64(len(b.Markers)), Uploads: int64(len(b.Uploads))}
	for _, o := range b.Objects {
		u.StoredBytes += o.Size
		if o.Latest {
			u.Objects++
			u.CurrentBytes += o.Size
		}
	}
	for _, up := range b.Uploads {
		for _, p := range up.Parts {
			u.UploadBytes += p.Size
		}
	}
	return u
}

// schema4FixtureUser is a user of the fixture.
type schema4FixtureUser struct {
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
	Roles  []string `json:"roles"`
	// Console says the user has a password (schema4FixturePassword).
	Console bool `json:"console"`
}

// schema4FixtureGroup is a group of the fixture and the roles it gives.
type schema4FixtureGroup struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// schema4FixtureRole is a role of the fixture that is not built in, and its
// policies.
type schema4FixtureRole struct {
	Name     string   `json:"name"`
	Policies []string `json:"policies"`
}

// schema4FixtureTarget names an object.
type schema4FixtureTarget struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// schema4FixtureScoped is the API key of the fixture that an inline policy
// narrows: its owner may read more, through a group, than the key may.
type schema4FixtureScoped struct {
	schema3Writer
	// InlinePolicy is the policy as it was put.
	InlinePolicy string `json:"inline_policy"`
	// Allowed is an object the key reads; Refused is one its owner may read
	// and the key may not.
	Allowed schema4FixtureTarget `json:"allowed"`
	Refused schema4FixtureTarget `json:"refused"`
}

// schema4FixtureIAM is who and what the fixture's identities are, besides
// what every deployment has from its first start.
type schema4FixtureIAM struct {
	Users    []schema4FixtureUser  `json:"users"`
	Groups   []schema4FixtureGroup `json:"groups"`
	Roles    []schema4FixtureRole  `json:"roles"`
	Policies []string              `json:"policies"`
}

// schema4FixtureContents is manifest.json: what the generator wrote into the
// fixture, for the test that opens it.
type schema4FixtureContents struct {
	// Commit is the tree whose binary wrote the fixture.
	Commit string `json:"commit"`
	// StateSchema and StateBuckets are what the state database said of
	// itself when the node had stopped: its schema, and every bucket of the
	// bbolt file. MetadataLevel is what the node said it wrote at.
	StateSchema   uint64   `json:"state_schema"`
	StateBuckets  []string `json:"state_buckets"`
	MetadataLevel int      `json:"metadata_level"`
	// Writer is the user that uploads, and its API key; Scoped the user
	// that only reads, and its narrowed key.
	Writer  schema3Writer           `json:"writer"`
	Scoped  schema4FixtureScoped    `json:"scoped"`
	IAM     schema4FixtureIAM       `json:"iam"`
	Buckets []*schema4FixtureBucket `json:"buckets"`
}

// versions gives the versions of the fixture as the helpers of the schema-3
// fixture take them. The versions are the manifest's own, not copies: what is
// counted on the one is counted on the other.
func (c *schema4FixtureContents) versions() *schema3Contents {
	out := &schema3Contents{}
	for _, b := range c.Buckets {
		view := &schema3Bucket{Name: b.Name, Versions: b.Versions}
		for _, o := range b.Objects {
			view.Objects = append(view.Objects, &o.schema3Object)
		}
		out.Buckets = append(out.Buckets, view)
	}
	return out
}

// schema4FixtureStored reads, from the state database of a metadata
// directory that no store has open, every bucket of the bbolt file and the
// records of buckets' latest use as they are stored, byte for byte.
func schema4FixtureStored(t *testing.T, metaDir string) (buckets []string, records map[string]string) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(metaDir, "state.db"), 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	defer func() { _ = db.Close() }()
	records = map[string]string{}
	if err := db.View(func(tx *bbolt.Tx) error {
		if err := tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			buckets = append(buckets, string(name))
			return nil
		}); err != nil {
			return err
		}
		stored := tx.Bucket([]byte("bucketactivity"))
		if stored == nil {
			return nil
		}
		return stored.ForEach(func(name, raw []byte) error {
			records[string(name)] = string(raw)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(buckets)
	return buckets, records
}

// schema4FixtureGrid asks the node for the buckets with their details, as
// the caller the headers name, and returns them by name.
func schema4FixtureGrid(ctx context.Context, t *testing.T, node *schema3Node, headers map[string]string) map[string]gridBucket {
	t.Helper()
	status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets?details=true", headers, nil)
	var listed []gridBucket
	if err := json.Unmarshal(body, &listed); err != nil || status != http.StatusOK {
		t.Fatalf("the buckets with details = %d %s (%v)", status, body, err)
	}
	buckets := map[string]gridBucket{}
	for _, b := range listed {
		buckets[b.Name] = b
	}
	return buckets
}

// schema4FixtureShown asks the node, as root, for the buckets with their
// details and says what differs from the manifest: who created each bucket
// and its latest uploads and downloads, in their order - with their times
// where the manifest has them. Nothing when the list is what was written
// down.
func schema4FixtureShown(ctx context.Context, t *testing.T, node *schema3Node, contents *schema4FixtureContents, times bool) []string {
	t.Helper()
	shown := schema4FixtureGrid(ctx, t, node, schema3RootBearer())
	var differs []string
	if len(shown) != len(contents.Buckets) {
		differs = append(differs, fmt.Sprintf("%d buckets are listed; want %d", len(shown), len(contents.Buckets)))
	}
	for _, b := range contents.Buckets {
		listed, ok := shown[b.Name]
		uploads, downloads, included := listed.activity()
		if !ok || !included {
			differs = append(differs, fmt.Sprintf("%s: listed %t, with its latest use %t", b.Name, ok, included))
			continue
		}
		wantUploads, wantDownloads := b.Activity.entries()
		got := fmt.Sprintf("created by %q; uploads %s| downloads %s", listed.creator(), schema4FixtureRows(uploads, times), schema4FixtureRows(downloads, times))
		want := fmt.Sprintf("created by %q; uploads %s| downloads %s", b.CreatedBy, schema4FixtureRows(wantUploads, times), schema4FixtureRows(wantDownloads, times))
		if got != want {
			differs = append(differs, fmt.Sprintf("%s: %s; want %s", b.Name, got, want))
		}
	}
	return differs
}

// schema4FixtureSettled waits until the node shows, of every version of the
// manifest, the reads the manifest counts: until what the node counted has
// been handed over through the metadata group.
func schema4FixtureSettled(ctx context.Context, t *testing.T, node *schema3Node, contents *schema4FixtureContents) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		differs := schema3Recorded(ctx, t, node, contents.versions())
		if len(differs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reads were not recorded as they were made:\n%s", strings.Join(differs, "\n"))
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// schema4FixtureServer starts this tree's binary on the deployment in dir, as
// schema3Server does, handing over what it counted as often as the fixture's
// tests wait for - and reads the node's log while the node runs. A node that
// serves hundreds of requests writes more log than a pipe holds, and one whose
// log nobody reads stops at the next line it writes. The function it returns
// stops the node as server.stop does: SIGTERM, a clean exit, and a log that
// was JSON to its end and said that the node stopped.
func schema4FixtureServer(ctx context.Context, t *testing.T, bin, dir string) (*schema3Node, func()) {
	t.Helper()
	node := schema3Server(ctx, t, bin, dir, schema4FixtureFlush)
	var (
		read       = make(chan struct{})
		sawStopped bool
		notJSON    []string
	)
	go func() {
		defer close(read)
		for node.srv.logs.Scan() {
			var entry struct {
				Msg string `json:"msg"`
			}
			if err := json.Unmarshal(node.srv.logs.Bytes(), &entry); err != nil {
				notJSON = append(notJSON, node.srv.logs.Text())
				continue
			}
			if entry.Msg == "stopped" {
				sawStopped = true
			}
		}
	}()
	return node, func() {
		t.Helper()
		if err := node.srv.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send SIGTERM: %v", err)
		}
		// The log is read to its end before the process is waited for:
		// waiting closes the pipe.
		<-read
		if err := node.srv.cmd.Wait(); err != nil {
			t.Fatalf("server did not exit cleanly on SIGTERM: %v\n%s", err, node.srv.stderr.String())
		}
		for _, line := range notJSON {
			t.Errorf("log line is not JSON: %q", line)
		}
		if !sawStopped {
			t.Error("server exited without logging a clean stop")
		}
	}
}

// schema4FixtureSignIn signs a user in to the console of the node and
// returns the headers that say who asks: the cookie, and the CSRF token for
// what changes something.
func schema4FixtureSignIn(ctx context.Context, t *testing.T, node *schema3Node, user, password string) map[string]string {
	t.Helper()
	status, header, body := adminCall(ctx, t, http.MethodPost, node.admin+"/session", nil, map[string]any{"username": user, "password": password})
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	cookie := strings.SplitN(header.Get("Set-Cookie"), ";", 2)[0]
	// The answer holds the session: it is not put into what fails.
	if err := json.Unmarshal(body, &session); err != nil || status != http.StatusOK || session.CSRFToken == "" || cookie == "" {
		t.Fatalf("signing %s in = %d (%v), a cookie of %d bytes", user, status, err, len(cookie))
	}
	return map[string]string{"Cookie": cookie, "X-CSRF-Token": session.CSRFToken}
}

// TestOpenSchema4Fixture: the schema-4 deployment that the binary of commit
// 84334a7 wrote is opened by this binary. Before anything is written the
// state is the one the manifest describes: of schema 4, with the records of
// the buckets' latest use byte for byte as they were stored. Nothing is
// lost: who created each bucket and what was last uploaded to and downloaded
// from it; every version, as the bytes it was, with who uploaded it and who
// read it how often; the bucket policy, which still decides; the lifecycle,
// the CORS and the tags of a bucket; versions and the delete marker; the
// versions that are locked, which still cannot be deleted; the users, the
// group, the role and the policy, and the two API keys, one of which is
// still refused what its inline policy does not allow; the object that a
// multipart upload became, whole and with its entity tag; and the upload
// that was in progress, which is listed with its three parts and finished.
// Every bucket is walked a few keys at a time, to its end - the bucket of
// many keys in several steps - and holds what the manifest's figures say:
// they are counted again here, from a listing of the versions and of the
// parts. What is uploaded and downloaded afterwards comes to the front of
// the bucket's record, and the oldest row leaves it.
//
// What the state's schema is after the test has written is deliberately not
// asserted: a binary of a later schema may raise it.
func TestOpenSchema4Fixture(t *testing.T) {
	// Not beside the others: this test starts one node as a process and is
	// done in seconds when it has the machine, and tests like it ran out of
	// their time when four-node clusters of containers were starting around
	// them. Built before the clock of the test starts: on a cold cache the
	// build alone can take longer than the test may.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	// A copy: the fixture itself is never opened.
	copyTree(t, filepath.Join("testdata", schema4FixtureDir), dir)
	raw, err := fs.ReadFile(os.DirFS(dir), schema4FixtureManifest)
	if err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	var contents schema4FixtureContents
	if err := json.Unmarshal(raw, &contents); err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	if len(contents.Buckets) == 0 || contents.Writer.SecretKey == "" || contents.Scoped.SecretKey == "" {
		t.Fatalf("the manifest of the fixture: %d buckets, and not both keys", len(contents.Buckets))
	}

	// The state, before any node of this tree has opened it: straight from
	// the file.
	meta := filepath.Join(dir, "meta")
	state := schema4StateOf(t, meta)
	if state.Schema != contents.StateSchema || state.Schema != 4 {
		t.Fatalf("the fixture's state is of schema %d; its manifest says %d, and it is a fixture of schema 4", state.Schema, contents.StateSchema)
	}
	stored, records := schema4FixtureStored(t, meta)
	if !slices.Equal(stored, contents.StateBuckets) {
		t.Fatalf("the fixture's state holds the buckets %v; its manifest says %v", stored, contents.StateBuckets)
	}
	if len(state.Creators) != len(contents.Buckets) {
		t.Errorf("the fixture's state holds %d buckets; its manifest says %d", len(state.Creators), len(contents.Buckets))
	}
	full, partial, unused := 0, 0, 0
	for _, b := range contents.Buckets {
		if by, ok := state.Creators[b.Name]; !ok || by != b.CreatedBy || by == "" {
			t.Errorf("the state says %s was created by %q (there: %t); the manifest says %q", b.Name, by, ok, b.CreatedBy)
		}
		record, ok := records[b.Name]
		if record != b.Activity.Record || ok != (b.Activity.Record != "") || (ok && sumOf([]byte(record)) != b.Activity.RecordSHA256) {
			t.Errorf("the stored record of the latest use of %s (there: %t) is not, byte for byte, what the manifest says:\n%s\nwant\n%s", b.Name, ok, record, b.Activity.Record)
		}
		// And the rows the manifest lists are what that record says.
		parsed := state.Activity[b.Name]
		uploads, downloads := (&schema4FixtureActivity{Uploads: parsed.Uploads, Downloads: parsed.Downloads}).entries()
		wantUploads, wantDownloads := b.Activity.entries()
		if got, want := schema4FixtureRows(uploads, true)+"| "+schema4FixtureRows(downloads, true), schema4FixtureRows(wantUploads, true)+"| "+schema4FixtureRows(wantDownloads, true); got != want {
			t.Errorf("the stored record of %s says %s; the manifest lists %s", b.Name, got, want)
		}
		switch {
		case len(b.Activity.Uploads) == bucket.MaxRecentUploads && len(b.Activity.Downloads) == bucket.MaxRecentDownloads:
			full++
		case !ok:
			unused++
		default:
			partial++
		}
	}
	if len(records) != len(contents.Buckets)-unused {
		t.Errorf("the state holds %d records of buckets' latest use; the manifest lists %d", len(records), len(contents.Buckets)-unused)
	}
	if full == 0 || partial == 0 || unused == 0 {
		t.Errorf("the fixture has %d buckets with as many rows as are kept, %d with fewer and %d without a record: the test proved less than it says", full, partial, unused)
	}

	node, stop := schema4FixtureServer(ctx, t, bin, dir)
	writer := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	// The node writes at the level the fixture was written at, or above.
	var settings struct {
		Cluster struct {
			MetadataLevel int `json:"metadata_level"`
		} `json:"cluster"`
	}
	status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/settings", schema3RootBearer(), nil)
	if err := json.Unmarshal(body, &settings); err != nil || status != http.StatusOK || settings.Cluster.MetadataLevel < contents.MetadataLevel {
		t.Fatalf("the settings = %d (%v), metadata level %d; the fixture was written at %d", status, err, settings.Cluster.MetadataLevel, contents.MetadataLevel)
	}
	// Who created, who uploaded and who read, first: nothing of this test
	// has been written or read yet, so what is shown is the fixture's own.
	for _, differs := range schema4FixtureShown(ctx, t, node, &contents, true) {
		t.Errorf("the buckets of the schema-4 deployment: %s", differs)
	}
	for _, differs := range schema3Recorded(ctx, t, node, contents.versions()) {
		t.Errorf("the record of the schema-4 deployment: %s", differs)
	}

	// The identities: every user with its groups and roles, the group with
	// its roles, the role with its policies, the policy.
	for _, u := range contents.IAM.Users {
		var user struct {
			Groups      []string `json:"groups"`
			Roles       []string `json:"roles"`
			HasPassword bool     `json:"has_password"`
		}
		status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/users/"+u.Name, schema3RootBearer(), nil)
		if err := json.Unmarshal(body, &user); err != nil || status != http.StatusOK || !slices.Equal(user.Groups, u.Groups) || !slices.Equal(user.Roles, u.Roles) || user.HasPassword != u.Console {
			t.Errorf("the user %s = %d %s (%v); want the groups %v, the roles %v, a password: %t", u.Name, status, body, err, u.Groups, u.Roles, u.Console)
		}
		if u.Console {
			// The password, kept as its Argon2id hash, still signs in.
			who := schema4FixtureSignIn(ctx, t, node, u.Name, schema4FixturePassword())
			if status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets", map[string]string{"Cookie": who["Cookie"]}, nil); status != http.StatusOK {
				t.Errorf("the buckets, for %s signed in = %d %s", u.Name, status, body)
			}
		}
	}
	for _, g := range contents.IAM.Groups {
		var group struct {
			Roles []string `json:"roles"`
		}
		status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/groups/"+g.Name, schema3RootBearer(), nil)
		if err := json.Unmarshal(body, &group); err != nil || status != http.StatusOK || !slices.Equal(group.Roles, g.Roles) {
			t.Errorf("the group %s = %d %s (%v); want the roles %v", g.Name, status, body, err, g.Roles)
		}
	}
	for _, r := range contents.IAM.Roles {
		var role struct {
			Policies []string `json:"policies"`
			BuiltIn  bool     `json:"built_in"`
		}
		status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/roles/"+r.Name, schema3RootBearer(), nil)
		if err := json.Unmarshal(body, &role); err != nil || status != http.StatusOK || role.BuiltIn || !slices.Equal(role.Policies, r.Policies) {
			t.Errorf("the role %s = %d %s (%v); want the policies %v", r.Name, status, body, err, r.Policies)
		}
	}
	for _, name := range contents.IAM.Policies {
		var policy struct {
			Document map[string]any `json:"document"`
			BuiltIn  bool           `json:"built_in"`
		}
		status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/policies/"+name, schema3RootBearer(), nil)
		if err := json.Unmarshal(body, &policy); err != nil || status != http.StatusOK || policy.BuiltIn || len(policy.Document) == 0 {
			t.Errorf("the policy %s = %d %s (%v)", name, status, body, err)
		}
	}
	if len(contents.IAM.Users) == 0 || len(contents.IAM.Groups) == 0 || len(contents.IAM.Roles) == 0 || len(contents.IAM.Policies) == 0 {
		t.Errorf("the fixture has %d users, %d groups, %d roles and %d policies: the test proved less than it says",
			len(contents.IAM.Users), len(contents.IAM.Groups), len(contents.IAM.Roles), len(contents.IAM.Policies))
	}
	// The narrowed key is listed with its owner, and still has its policy.
	var keys []struct {
		AccessKeyID  string         `json:"access_key_id"`
		InlinePolicy map[string]any `json:"inline_policy"`
	}
	status, _, body = adminCall(ctx, t, http.MethodGet, node.admin+"/users/"+contents.Scoped.User+"/keys", schema3RootBearer(), nil)
	if err := json.Unmarshal(body, &keys); err != nil || status != http.StatusOK || len(keys) != 1 || keys[0].AccessKeyID != contents.Scoped.AccessKeyID || len(keys[0].InlinePolicy) == 0 {
		t.Errorf("the keys of %s = %d (%v), %d keys; want the one with its inline policy", contents.Scoped.User, status, err, len(keys))
	}

	uploads, anonymous, locked, configured := 0, 0, 0, 0
	steps, mostParts, multipart, noncurrent := 0, 0, 0, int64(0)
	for _, b := range contents.Buckets {
		// The figures of the manifest are what its own lists come to.
		if counted := b.usage(); counted != b.Usage {
			t.Errorf("the manifest says %s holds %+v; its lists come to %+v", b.Name, b.Usage, counted)
		}
		noncurrent += b.Usage.Versions - b.Usage.Objects
		// What keeps a version still keeps it: it says so, and it is not
		// deleted - by root, who does not say that it lifts a governance
		// retention, and for a legal hold not even when it says so.
		lock, err := node.root.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(b.Name)})
		switch {
		case b.ObjectLock && err != nil:
			t.Errorf("the Object Lock configuration of %s: %v", b.Name, err)
		case b.ObjectLock && (lock.ObjectLockConfiguration == nil || lock.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled):
			t.Errorf("the Object Lock configuration of %s = %+v", b.Name, lock.ObjectLockConfiguration)
		case !b.ObjectLock && apiCode(err) != "ObjectLockConfigurationNotFoundError":
			t.Errorf("the Object Lock configuration of %s, which was created without: %v", b.Name, err)
		}
		for _, o := range b.Objects {
			if o.Retention != nil {
				locked++
				kept, err := node.root.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String(b.Name), Key: aws.String(o.Key), VersionId: aws.String(o.VersionID)})
				switch {
				case err != nil:
					t.Errorf("the retention of %s/%s %s: %v", b.Name, o.Key, o.VersionID, err)
				case kept.Retention == nil || string(kept.Retention.Mode) != o.Retention.Mode || !aws.ToTime(kept.Retention.RetainUntilDate).Equal(o.Retention.Until):
					t.Errorf("the retention of %s/%s %s = %+v; want %s until %s", b.Name, o.Key, o.VersionID, kept.Retention, o.Retention.Mode, o.Retention.Until)
				}
				if !o.Retention.Until.After(time.Now().AddDate(1, 0, 0)) {
					t.Errorf("the retention of %s/%s ends %s: the fixture no longer proves that a lock holds", b.Name, o.Key, o.Retention.Until)
				}
				_, err = node.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.Name), Key: aws.String(o.Key), VersionId: aws.String(o.VersionID)})
				if apiCode(err) != "AccessDenied" {
					t.Errorf("deleting %s/%s %s, which is under retention: %v; want AccessDenied", b.Name, o.Key, o.VersionID, err)
				}
			}
			if o.LegalHold {
				locked++
				hold, err := node.root.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{Bucket: aws.String(b.Name), Key: aws.String(o.Key), VersionId: aws.String(o.VersionID)})
				switch {
				case err != nil:
					t.Errorf("the legal hold of %s/%s %s: %v", b.Name, o.Key, o.VersionID, err)
				case hold.LegalHold == nil || hold.LegalHold.Status != types.ObjectLockLegalHoldStatusOn:
					t.Errorf("the legal hold of %s/%s %s = %+v; want it on", b.Name, o.Key, o.VersionID, hold.LegalHold)
				}
				_, err = node.root.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(b.Name), Key: aws.String(o.Key), VersionId: aws.String(o.VersionID), BypassGovernanceRetention: aws.Bool(true),
				})
				if apiCode(err) != "AccessDenied" {
					t.Errorf("deleting %s/%s %s, which is under legal hold: %v; want AccessDenied", b.Name, o.Key, o.VersionID, err)
				}
			}
		}

		// Every version, by its ID where it has one, is the bytes it was -
		// the locked ones after what was just tried, too. Root reads them,
		// and that is counted.
		for _, o := range b.Objects {
			got, err := schema3Get(ctx, node.root, b.Name, &o.schema3Object)
			if err != nil || int64(len(got)) != o.Size || sumOf(got) != o.SHA256 {
				t.Errorf("%s/%s %s of the schema-4 deployment: %d bytes, %v; want %d", b.Name, o.Key, o.VersionID, len(got), err, o.Size)
				continue
			}
			o.count(schema3Root, false, 1, 0)
		}

		// The versions and the markers, as the bucket lists them: page
		// after page, to the end.
		var listed struct {
			Versions      []types.ObjectVersion
			DeleteMarkers []types.DeleteMarkerEntry
		}
		for pages := s3.NewListObjectVersionsPaginator(node.root, &s3.ListObjectVersionsInput{Bucket: aws.String(b.Name), MaxKeys: aws.Int32(schema4FixturePage)}); pages.HasMorePages(); {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Fatalf("versions of %s: %v", b.Name, err)
			}
			listed.Versions = append(listed.Versions, page.Versions...)
			listed.DeleteMarkers = append(listed.DeleteMarkers, page.DeleteMarkers...)
		}
		if len(listed.Versions) != len(b.Objects) || len(listed.DeleteMarkers) != len(b.Markers) {
			t.Fatalf("versions of %s: %d and %d markers; want %d and %d", b.Name, len(listed.Versions), len(listed.DeleteMarkers), len(b.Objects), len(b.Markers))
		}
		// What the bucket holds, counted from that listing - and, below,
		// from the listing of its uploads and of their parts - is what the
		// manifest's figures say.
		held := schema4FixtureUsage{Versions: int64(len(listed.Versions)), Markers: int64(len(listed.DeleteMarkers))}
		want := map[string]*schema4FixtureObject{}
		for _, o := range b.Objects {
			id := o.VersionID
			if id == "" {
				id = "null"
			}
			want[o.Key+" "+id] = o
		}
		for _, v := range listed.Versions {
			held.StoredBytes += aws.ToInt64(v.Size)
			if aws.ToBool(v.IsLatest) {
				held.Objects++
				held.CurrentBytes += aws.ToInt64(v.Size)
			}
			name := aws.ToString(v.Key) + " " + aws.ToString(v.VersionId)
			o, ok := want[name]
			if !ok {
				t.Errorf("%s lists the version %s, which the manifest does not have", b.Name, name)
				continue
			}
			if o.Latest != aws.ToBool(v.IsLatest) || o.Size != aws.ToInt64(v.Size) {
				t.Errorf("%s lists the version %s, latest %t, of %d bytes; the manifest: latest %t, of %d", b.Name, name, aws.ToBool(v.IsLatest), aws.ToInt64(v.Size), o.Latest, o.Size)
			}
			// The object a multipart upload became has the entity tag it
			// was given, in the listing and when it is asked for - and
			// was read whole above, like every version.
			if o.ETag == "" {
				continue
			}
			multipart++
			head, err := node.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.Name), Key: aws.String(o.Key), VersionId: v.VersionId})
			switch size := int64(0); {
			case err != nil:
				t.Errorf("HeadObject of %s/%s %s: %v", b.Name, o.Key, o.VersionID, err)
			case strings.Trim(aws.ToString(head.ETag), `"`) != o.ETag || strings.Trim(aws.ToString(v.ETag), `"`) != o.ETag || aws.ToInt64(head.ContentLength) != o.Size:
				t.Errorf("%s/%s %s has the entity tag %s (listed with %s) and %d bytes; want %q and %d", b.Name, o.Key, o.VersionID,
					aws.ToString(head.ETag), aws.ToString(v.ETag), aws.ToInt64(head.ContentLength), o.ETag, o.Size)
			default:
				for _, p := range o.Parts {
					size += p.Size
				}
				if size != o.Size || len(o.Parts) < 2 || !strings.HasSuffix(o.ETag, fmt.Sprintf("-%d", len(o.Parts))) {
					t.Errorf("%s/%s %s is of %d parts of %d bytes together, with the entity tag %s; it is %d bytes", b.Name, o.Key, o.VersionID, len(o.Parts), size, o.ETag, o.Size)
				}
			}
		}

		// The keys, as the bucket lists them a few at a time: every key
		// that has an object, once, in the order of the keys, and no page
		// with more than was asked for.
		var keys, walked []string
		for _, o := range b.Objects {
			if o.Latest {
				keys = append(keys, o.Key)
			}
		}
		slices.Sort(keys)
		taken := 0
		for pages := s3.NewListObjectsV2Paginator(node.root, &s3.ListObjectsV2Input{Bucket: aws.String(b.Name), MaxKeys: aws.Int32(schema4FixturePage)}); pages.HasMorePages(); {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Fatalf("the keys of %s, step %d: %v", b.Name, taken+1, err)
			}
			taken++
			if len(page.Contents) > schema4FixturePage || (len(page.Contents) == 0 && len(keys) > 0) {
				t.Errorf("step %d of the keys of %s has %d keys; %d were asked for", taken, b.Name, len(page.Contents), schema4FixturePage)
			}
			for _, c := range page.Contents {
				walked = append(walked, aws.ToString(c.Key))
				if o, ok := want[aws.ToString(c.Key)+" null"]; !b.Versions && (!ok || o.Size != aws.ToInt64(c.Size)) {
					t.Errorf("%s lists the key %s of %d bytes; the manifest: there %t", b.Name, aws.ToString(c.Key), aws.ToInt64(c.Size), ok)
				}
			}
		}
		if !slices.Equal(walked, keys) {
			t.Errorf("%s lists %d keys in %d steps; want the %d of the manifest, in their order:\n%v\nwant\n%v", b.Name, len(walked), taken, len(keys), walked, keys)
		}
		steps = max(steps, taken)
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
		versioning, err := node.root.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(b.Name)})
		if err != nil {
			t.Errorf("versioning of %s: %v", b.Name, err)
		} else if (versioning.Status == types.BucketVersioningStatusEnabled) != b.Versions {
			t.Errorf("versioning of %s = %q; the manifest says %t", b.Name, versioning.Status, b.Versions)
		}

		// The policy, as it was written - and it still decides: who it
		// lets read without a credential reads, and is counted as nobody
		// in particular; whom it does not, does not.
		if b.Policy != "" {
			policy, err := node.root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(b.Name)})
			if err != nil {
				t.Errorf("the policy of %s: %v", b.Name, err)
			} else if aws.ToString(policy.Policy) != b.Policy {
				t.Errorf("the policy of %s = %s", b.Name, aws.ToString(policy.Policy))
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

		// The lifecycle, the CORS and the tags of the bucket, as they
		// were put.
		if len(b.Lifecycle) > 0 {
			configured++
			lifecycle, err := node.root.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(b.Name)})
			if err != nil {
				t.Fatalf("the lifecycle of %s: %v", b.Name, err)
			}
			var rules []schema4FixtureLifecycleRule
			for _, r := range lifecycle.Rules {
				rule := schema4FixtureLifecycleRule{ID: aws.ToString(r.ID)}
				if r.Filter != nil {
					rule.Prefix = aws.ToString(r.Filter.Prefix)
				}
				if r.Expiration != nil {
					rule.ExpirationDays = aws.ToInt32(r.Expiration.Days)
				}
				if r.Status != types.ExpirationStatusEnabled {
					t.Errorf("the rule %s of the lifecycle of %s is %s", rule.ID, b.Name, r.Status)
				}
				rules = append(rules, rule)
			}
			if !slices.Equal(rules, b.Lifecycle) {
				t.Errorf("the lifecycle of %s = %+v; want %+v", b.Name, rules, b.Lifecycle)
			}
		}
		if len(b.CORS) > 0 {
			configured++
			cors, err := node.root.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(b.Name)})
			if err != nil {
				t.Fatalf("the CORS of %s: %v", b.Name, err)
			}
			if len(cors.CORSRules) != len(b.CORS) {
				t.Fatalf("the CORS of %s has %d rules; want %d", b.Name, len(cors.CORSRules), len(b.CORS))
			}
			for i, r := range cors.CORSRules {
				if !slices.Equal(r.AllowedOrigins, b.CORS[i].AllowedOrigins) || !slices.Equal(r.AllowedMethods, b.CORS[i].AllowedMethods) || aws.ToInt32(r.MaxAgeSeconds) != b.CORS[i].MaxAgeSeconds {
					t.Errorf("rule %d of the CORS of %s = %+v; want %+v", i, b.Name, r, b.CORS[i])
				}
			}
		}
		if len(b.Tags) > 0 {
			configured++
			tags, err := node.root.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(b.Name)})
			if err != nil {
				t.Fatalf("the tags of %s: %v", b.Name, err)
			}
			if got := tagsOf(tags.TagSet); !maps.Equal(got, b.Tags) {
				t.Errorf("the tags of %s = %v; want %v", b.Name, got, b.Tags)
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
		// The parts of every upload the bucket lists, before any is
		// finished: with them the bucket holds what the manifest says.
		held.Uploads = int64(len(inProgress.Uploads))
		for _, m := range inProgress.Uploads {
			stored, err := node.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(b.Name), Key: m.Key, UploadId: m.UploadId})
			if err != nil {
				t.Fatalf("parts of the upload %s of %s/%s: %v", aws.ToString(m.UploadId), b.Name, aws.ToString(m.Key), err)
			}
			for _, p := range stored.Parts {
				held.UploadBytes += aws.ToInt64(p.Size)
			}
		}
		if held != b.Usage {
			t.Errorf("%s holds %+v, by its versions, its uploads and their parts as they are listed; the manifest says %+v", b.Name, held, b.Usage)
		}
		for _, u := range b.Uploads {
			uploads++
			if !slices.ContainsFunc(inProgress.Uploads, func(m types.MultipartUpload) bool {
				return aws.ToString(m.Key) == u.Key && aws.ToString(m.UploadId) == u.UploadID
			}) {
				t.Fatalf("%s does not list the upload %s of %s", b.Name, u.UploadID, u.Key)
			}
			parts, err := node.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(b.Name), Key: aws.String(u.Key), UploadId: aws.String(u.UploadID)})
			if err != nil {
				t.Fatalf("parts of the upload of %s/%s: %v", b.Name, u.Key, err)
			}
			if len(parts.Parts) != len(u.Parts) {
				t.Fatalf("parts of the upload of %s/%s = %+v; want %d", b.Name, u.Key, parts.Parts, len(u.Parts))
			}
			mostParts = max(mostParts, len(u.Parts))
			var completed []types.CompletedPart
			size, small := int64(0), false
			for i, p := range u.Parts {
				if aws.ToInt32(parts.Parts[i].PartNumber) != p.Number || aws.ToInt64(parts.Parts[i].Size) != p.Size {
					t.Fatalf("part %d of the upload of %s/%s: number %d, %d bytes; want %d and %d", i, b.Name, u.Key,
						aws.ToInt32(parts.Parts[i].PartNumber), aws.ToInt64(parts.Parts[i].Size), p.Number, p.Size)
				}
				completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: parts.Parts[i].ETag})
				size += p.Size
				small = small || (i < len(u.Parts)-1 && p.Size < object.MinPartSize)
			}
			complete := func(with []types.CompletedPart) error {
				_, err := node.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
					Bucket: aws.String(b.Name), Key: aws.String(u.Key), UploadId: aws.String(u.UploadID),
					MultipartUpload: &types.CompletedMultipartUpload{Parts: with},
				})
				return err
			}
			// A part that is not the last must be of object.MinPartSize
			// when the upload is completed, and the fixture's parts are
			// small: all of them together are refused, and the upload
			// is still there with every part. It is then completed with
			// its last part alone, which may be of any size.
			last := u.Parts[len(u.Parts)-1]
			if small {
				if err := complete(completed); apiCode(err) != "EntityTooSmall" {
					t.Fatalf("CompleteMultipartUpload of the old upload of %s/%s with all its %d parts: %v; want EntityTooSmall", b.Name, u.Key, len(completed), err)
				}
				still, err := node.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(b.Name), Key: aws.String(u.Key), UploadId: aws.String(u.UploadID)})
				if err != nil {
					t.Fatalf("parts of the upload of %s/%s after its completion was refused: %v", b.Name, u.Key, err)
				}
				if len(still.Parts) != len(u.Parts) {
					t.Fatalf("the upload of %s/%s has %d parts after its completion was refused; want %d", b.Name, u.Key, len(still.Parts), len(u.Parts))
				}
				completed, size = completed[len(completed)-1:], last.Size
			}
			if err := complete(completed); err != nil {
				t.Fatalf("CompleteMultipartUpload of the old upload of %s/%s with %d of its parts: %v", b.Name, u.Key, len(completed), err)
			}
			// What it became is the parts it was completed with, one
			// after the other. With one part the manifest says what
			// that is; with more, the parts are checked by their sizes
			// alone. The fixture's upload is in a bucket that keeps no
			// versions, so what it became is the null version of its
			// key.
			finished := &schema4FixtureObject{schema3Object: schema3Object{Key: u.Key, Latest: true, Size: size, UploadedBy: schema3Root}}
			got, err := schema3Get(ctx, node.root, b.Name, &finished.schema3Object)
			if err != nil || int64(len(got)) != size {
				t.Fatalf("the finished upload of %s/%s: %d bytes, %v; want %d", b.Name, u.Key, len(got), err, size)
			}
			finished.SHA256 = sumOf(got)
			if len(completed) == 1 && finished.SHA256 != last.SHA256 {
				t.Errorf("the finished upload of %s/%s is not the part it was completed with", b.Name, u.Key)
			}
			finished.count(schema3Root, false, 1, 0)
			b.Objects = append(b.Objects, finished)
		}
		b.Uploads = nil
	}
	if uploads == 0 || anonymous == 0 || locked < 2 || configured < 3 {
		t.Errorf("the fixture had %d uploads in progress, %d objects anybody may read, %d locks and %d configurations of a bucket: the test proved less than it says", uploads, anonymous, locked, configured)
	}
	if steps < schema4FixtureSteps || mostParts < 3 || multipart == 0 || noncurrent == 0 {
		t.Errorf("the fixture had a bucket walked in %d steps, an upload in progress of %d parts, %d objects of a multipart upload and %d versions that are not current: the test proved less than it says",
			steps, mostParts, multipart, noncurrent)
	}

	// The narrowed key, kept under the KMS key of the fixture, still signs,
	// and its inline policy still narrows it: it reads what the policy
	// names; it does not read what its owner's group would let it read, and
	// it does not write.
	scoped := node.client(contents.Scoped.AccessKeyID, contents.Scoped.SecretKey)
	allowed, refused := contents.Scoped.Allowed, contents.Scoped.Refused
	read := false
	for _, b := range contents.Buckets {
		for _, o := range b.Objects {
			if b.Name != allowed.Bucket || o.Key != allowed.Key || !o.Latest {
				continue
			}
			got, err := schema3Get(ctx, scoped, b.Name, &o.schema3Object)
			if err != nil || sumOf(got) != o.SHA256 {
				t.Fatalf("GetObject of %s/%s with the narrowed key: %d bytes, %v", b.Name, o.Key, len(got), err)
			}
			o.count(contents.Scoped.Actor, false, 1, 0)
			read = true
		}
	}
	if !read {
		t.Errorf("the manifest has no object %s/%s for the narrowed key to read", allowed.Bucket, allowed.Key)
	}
	if _, err := scoped.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(refused.Bucket), Key: aws.String(refused.Key)}); apiCode(err) != "AccessDenied" {
		t.Errorf("GetObject of %s/%s with the narrowed key: %v; want AccessDenied", refused.Bucket, refused.Key, err)
	}
	if _, err := scoped.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(allowed.Bucket), Key: aws.String(allowed.Key), Body: strings.NewReader("not written")}); apiCode(err) != "AccessDenied" {
		t.Errorf("PutObject of %s/%s with the narrowed key: %v; want AccessDenied", allowed.Bucket, allowed.Key, err)
	}

	// What this binary adds works on the deployment. First everything read
	// so far is handed over, so that what follows is the latest there is.
	first := contents.Buckets[0]
	if first.Versions || len(first.Activity.Uploads) != bucket.MaxRecentUploads || len(first.Activity.Downloads) != bucket.MaxRecentDownloads {
		t.Fatalf("the first bucket of the manifest, %s, keeps versions or has fewer rows than are kept: what follows would not push a row out", first.Name)
	}
	schema4FixtureSettled(ctx, t, node, &contents)
	uploadsBefore, downloadsBefore, shown := schema4FixtureGrid(ctx, t, node, schema3RootBearer())[first.Name].activity()
	if !shown || len(uploadsBefore) != bucket.MaxRecentUploads || len(downloadsBefore) != bucket.MaxRecentDownloads {
		t.Fatalf("the latest use of %s before the upload: shown %t, %d uploads and %d downloads", first.Name, shown, len(uploadsBefore), len(downloadsBefore))
	}
	// The user's key still signs - an upload and a download of it, with the
	// user as the uploader and the one reader.
	added := &schema4FixtureObject{schema3Object: schema3Object{Key: "after/schema-four-fixture.txt", Latest: true, UploadedBy: contents.Writer.Actor}}
	content := []byte("written after the schema-4 deployment was opened")
	added.Size, added.SHA256 = int64(len(content)), sumOf(content)
	if _, err := writer.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(first.Name), Key: aws.String(added.Key), Body: strings.NewReader(string(content))}); err != nil {
		t.Fatalf("PutObject with the key of the fixture's user: %v", err)
	}
	if got, err := schema3Get(ctx, writer, first.Name, &added.schema3Object); err != nil || sumOf(got) != added.SHA256 {
		t.Fatalf("GetObject with the key of the fixture's user: %d bytes, %v", len(got), err)
	}
	added.count(contents.Writer.Actor, false, 1, 0)
	first.Objects = append(first.Objects, added)
	schema4FixtureSettled(ctx, t, node, &contents)
	// They are at the front of the bucket's record, the rows that were
	// there follow in their order, and the oldest of each has left.
	uploadsAfter, downloadsAfter, shown := schema4FixtureGrid(ctx, t, node, schema3RootBearer())[first.Name].activity()
	mine := gridEntry{By: contents.Writer.Actor, Key: aws.String(added.Key)}
	if got, want := gridRows(uploadsAfter), gridRows(append([]gridEntry{mine}, uploadsBefore[:len(uploadsBefore)-1]...)); !shown || got != want {
		t.Errorf("the latest uploads of %s after one more = %q (shown %t); want %q", first.Name, got, shown, want)
	}
	if got, want := gridRows(downloadsAfter), gridRows(append([]gridEntry{mine}, downloadsBefore[:len(downloadsBefore)-1]...)); !shown || got != want {
		t.Errorf("the latest downloads of %s after one more = %q (shown %t); want %q", first.Name, got, shown, want)
	}
	if len(uploadsAfter) == len(uploadsBefore) {
		for i, e := range uploadsAfter[1:] {
			if !e.At.Equal(uploadsBefore[i].At) || e.At.After(uploadsAfter[0].At) {
				t.Errorf("row %d of the latest uploads of %s is of %s; it was of %s, and the new one is of %s", i+1, first.Name, e.At, uploadsBefore[i].At, uploadsAfter[0].At)
			}
		}
	}
	stop()

	// After a restart: what the node counted was handed over, and was
	// added to what the fixture held; who created the buckets is who it
	// was; and everything still reads.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	for _, differs := range schema3Recorded(ctx, t, node, contents.versions()) {
		t.Errorf("the record after this binary read and wrote: %s", differs)
	}
	restarted := schema4FixtureGrid(ctx, t, node, schema3RootBearer())
	for _, b := range contents.Buckets {
		if by := restarted[b.Name].creator(); by != b.CreatedBy {
			t.Errorf("after a restart %s is created by %q; want %q", b.Name, by, b.CreatedBy)
		}
		for _, o := range b.Objects {
			if got, err := schema3Get(ctx, node.root, b.Name, &o.schema3Object); err != nil || sumOf(got) != o.SHA256 {
				t.Errorf("%s/%s %s after a restart: %d bytes, %v", b.Name, o.Key, o.VersionID, len(got), err)
			}
		}
	}
	stop()
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) ||
		strings.Contains(out, contents.Scoped.SecretKey) || strings.Contains(out, schema4FixturePassword()) {
		t.Fatal("a secret was written to standard error")
	}
}
