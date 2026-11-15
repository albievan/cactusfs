//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// The upgrade from entry schema 4 to entry schema 5 (ADR-0023, "Migration",
// "Rollback"), on the deployment the binary of commit 84334a7 wrote: the
// fixture testdata/schema-4, which is frozen and of which every test opens a
// copy. TestOpenSchema4Fixture says that everything the fixture holds is
// still there; this test says what becomes of its state.
const (
	// schema5FirstBucket is the bucket this binary creates over the
	// deployment: counted from its creation, where the fixture's buckets
	// are counted by the walk.
	schema5FirstBucket = "schema-five-first"
	// schema5Page is how many versions a page of the listing holds, and
	// schema5Pages how many such pages the bucket of many keys must fill at
	// the least.
	schema5Page  = 50
	schema5Pages = 6
	// schema5FixtureBuckets is how many buckets the fixture must hold for
	// the test to be worth its name.
	schema5FixtureBuckets = 7
	// schema5CountedWithin is how long the node's own counter may take to
	// bring every bucket of the fixture to an exact record: it looks at
	// every bucket at its first tick, and again every ten seconds when the
	// level was not yet 5 (app.UsageCounter).
	schema5CountedWithin = 2 * time.Minute
	// schema5DriftMetric counts the recounts that found other totals than
	// were kept, on the node whose entry ended them.
	schema5DriftMetric = "cactusfs_bucket_usage_drift_total"
)

// schema5State is what a state database says of what schema 5 keeps.
type schema5State struct {
	// Schema is the schema the state says it has.
	Schema uint64
	// Buckets is every bucket of the bbolt file, sorted.
	Buckets []string
	// Usage holds the usage records as they are stored, by bucket.
	Usage map[string]string
}

// schema5StateOf reads, from the state database of a metadata directory that
// no store has open, what schema 5 keeps: straight from the file, so that
// what is asserted is what is stored and not what some read path makes of it.
func schema5StateOf(t *testing.T, metaDir string) schema5State {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(metaDir, "state.db"), 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	defer func() { _ = db.Close() }()
	state := schema5State{Usage: map[string]string{}}
	if err := db.View(func(tx *bbolt.Tx) error {
		state.Schema = binary.BigEndian.Uint64(tx.Bucket([]byte("meta")).Get([]byte("schema")))
		if err := tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			state.Buckets = append(state.Buckets, string(name))
			return nil
		}); err != nil {
			return err
		}
		records := tx.Bucket([]byte("bucketusage"))
		if records == nil {
			return nil
		}
		return records.ForEach(func(name, raw []byte) error {
			state.Usage[string(name)] = string(raw)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(state.Buckets)
	return state
}

// schema5Totals gives the figures of the manifest as a usage record holds
// them (ADR-0023, "What is counted"): a delete marker is a version of no
// bytes, so the record's versions are the manifest's versions and its
// markers together. How many uploads are in progress is not in a record:
// only the bytes of their parts are.
func schema5Totals(u schema4FixtureUsage) bucket.UsageTotals {
	return bucket.UsageTotals{
		Objects: u.Objects, CurrentBytes: u.CurrentBytes, Versions: u.Versions + u.Markers, StoredBytes: u.StoredBytes, UploadBytes: u.UploadBytes,
	}
}

// schema5Moved returns totals changed by what a change adds and takes away.
func schema5Moved(t bucket.UsageTotals, by bucket.UsageTotals) bucket.UsageTotals {
	t.Objects, t.CurrentBytes, t.Versions = t.Objects+by.Objects, t.CurrentBytes+by.CurrentBytes, t.Versions+by.Versions
	t.StoredBytes, t.UploadBytes = t.StoredBytes+by.StoredBytes, t.UploadBytes+by.UploadBytes
	return t
}

// schema5Sums returns the SHA-256 of every file of a tree, by its path.
func schema5Sums(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	tree := os.DirFS(dir)
	if err := fs.WalkDir(tree, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		sums[path] = sumOf(content)
		return nil
	}); err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return sums
}

// schema5Shown is the usage of a bucket as the management API shows it.
type schema5Shown struct {
	ObjectCount int64  `json:"object_count"`
	TotalSize   int64  `json:"total_size"`
	Complete    bool   `json:"complete"`
	Exact       bool   `json:"exact"`
	Versions    *int64 `json:"versions"`
	StoredBytes *int64 `json:"stored_bytes"`
	UploadBytes *int64 `json:"upload_bytes"`
}

// schema5UsageOf asks the management API of a node, as root, for the usage
// of a bucket, and returns its figures and whether they are the exact count
// the metadata store keeps. Figures that are not exact are counted for the
// request, as far as one request counts, and have no versions, stored bytes
// or bytes of uploads to compare.
func schema5UsageOf(ctx context.Context, t *testing.T, node *schema3Node, name string) (bucket.UsageTotals, bool) {
	t.Helper()
	var shown schema5Shown
	status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/buckets/"+name+"/usage", schema3RootBearer(), nil)
	if err := json.Unmarshal(body, &shown); err != nil || status != http.StatusOK {
		t.Fatalf("the usage of %s = %d %s (%v)", name, status, body, err)
	}
	if !shown.Exact {
		return bucket.UsageTotals{Objects: shown.ObjectCount, CurrentBytes: shown.TotalSize}, false
	}
	if !shown.Complete || shown.Versions == nil || shown.StoredBytes == nil || shown.UploadBytes == nil {
		t.Fatalf("the usage of %s is exact and not complete, or without the figures of the record: %s", name, body)
	}
	return bucket.UsageTotals{
		Objects: shown.ObjectCount, CurrentBytes: shown.TotalSize, Versions: *shown.Versions, StoredBytes: *shown.StoredBytes, UploadBytes: *shown.UploadBytes,
	}, true
}

// schema5Counted waits, at most schema5CountedWithin, until the management
// API of a node answers the usage of every bucket named as exact - nothing
// but the node itself is there to count it - and says of each that its
// figures are those wanted.
func schema5Counted(ctx context.Context, t *testing.T, node *schema3Node, want map[string]bucket.UsageTotals, when string, of ...string) {
	t.Helper()
	waiting := slices.Clone(of)
	deadline := time.Now().Add(schema5CountedWithin)
	for {
		waiting = slices.DeleteFunc(waiting, func(name string) bool {
			got, exact := schema5UsageOf(ctx, t, node, name)
			if exact && got != want[name] {
				t.Errorf("%s: the management API answers the usage of %s as exact, %+v; want %+v", when, name, got, want[name])
			}
			return exact
		})
		if len(waiting) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the usage of %v is still not exact after %s: the node did not count it", when, waiting, schema5CountedWithin)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestUpgradeFromSchema4: the schema-4 deployment that the binary of commit
// 84334a7 wrote is opened by this binary. Migration: none for the state
// (ADR-0023). It opens as it is, and everything the fixture holds reads
// back as it was written: who created each bucket and its latest use, who
// uploaded and who read every version, every version as the bytes it was,
// the versions, the delete markers and the upload in progress.
//
// The buckets that were there are counted without being asked (ADR-0023,
// "Migration", "Rollback"). A node that runs alone writes at level 5 from
// its first start, and its counter (app.UsageCounter, wired at PLAN 8.3.3)
// proposes the walk for every bucket that has no exact record; an entry of
// the walk is of schema 5, so the state becomes one of schema 5 within
// seconds of the first start, by nobody's doing, and there the way back to
// the release of schema 4 ends - by design. The management API then answers
// the usage of every bucket of the fixture as exact and of exactly the
// figures its generator wrote down - the bucket of three hundred keys, the
// upload in progress with its three parts, the object a multipart upload
// became - and the state holds the records of exactly those buckets.
//
// From then on every change moves the records by exactly what it is: a put,
// a delete, a part and an abort in a bucket that was there; a bucket this
// binary creates, which is counted from its creation; a put and a delete
// that makes a marker in a bucket that keeps versions; a part more of the
// fixture's upload in progress, and then its abort. A recount of every
// bucket asked for through the management API finds no drift, and a
// restart changes no record. Everything the fixture held reads back as the
// bytes it was, after the state became schema 5 as before.
//
// The walk in many entries of fifty records, and a recount in as many, on
// the same state without a node, is
// TestTheSchema4FixtureIsCountedToItsManifest in internal/adapters/raftmeta.
func TestUpgradeFromSchema4(t *testing.T) {
	// Not beside the others, for the reason TestOpenSchema4Fixture gives;
	// and built before the clock of the test starts.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	fixture := filepath.Join("testdata", schema4FixtureDir)
	frozen := schema5Sums(t, fixture)
	// A copy: the fixture itself is never opened.
	copyTree(t, fixture, dir)
	meta := filepath.Join(dir, "meta")
	raw, err := fs.ReadFile(os.DirFS(dir), schema4FixtureManifest)
	if err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	var contents schema4FixtureContents
	if err := json.Unmarshal(raw, &contents); err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	if len(contents.Buckets) < schema5FixtureBuckets || contents.Writer.SecretKey == "" || contents.StateSchema != 4 {
		t.Fatalf("the manifest of the fixture: %d buckets, a state of schema %d: the test proves less than it says", len(contents.Buckets), contents.StateSchema)
	}
	var names []string
	want := map[string]bucket.UsageTotals{}
	var many, versioned, plain *schema4FixtureBucket
	var sum bucket.UsageTotals
	for _, b := range contents.Buckets {
		names = append(names, b.Name)
		want[b.Name] = schema5Totals(b.Usage)
		sum = schema5Moved(sum, want[b.Name])
		switch {
		case b.Usage.Objects >= schema5Pages*schema5Page:
			many = b
		case b.Versions && !b.ObjectLock && len(b.Markers) > 0:
			versioned = b
		case !b.Versions && len(b.Uploads) > 0:
			plain = b
		}
		// A recount that is asked for is begun by an entry of the walk of
		// app.UsageWalkLimit records, proposed before the answer: a bucket
		// of fewer records - a version or a marker each, a part of an
		// upload each, an upload without parts one - is counted again by
		// that entry alone, and any drift is told before the answer.
		walked := len(b.Objects) + len(b.Markers)
		for _, upload := range b.Uploads {
			walked += max(len(upload.Parts), 1)
		}
		if walked >= app.UsageWalkLimit {
			t.Fatalf("the bucket %s of the fixture holds %d records: a recount of it takes more than the entry that begins it", b.Name, walked)
		}
	}
	if many == nil || versioned == nil || plain == nil || len(plain.Uploads[0].Parts) != 3 {
		t.Fatalf("the fixture has no bucket of %d keys, none that keeps versions and holds a marker, or none with an upload in progress of three parts",
			schema5Pages*schema5Page)
	}
	// Guards against proving less than is said.
	if sum.Objects == 0 || sum.CurrentBytes == 0 || sum.Versions <= sum.Objects || sum.StoredBytes <= sum.CurrentBytes || sum.UploadBytes == 0 ||
		want[plain.Name].UploadBytes == 0 || want[versioned.Name].Versions <= want[versioned.Name].Objects {
		t.Fatalf("the buckets of the fixture together hold %+v: the test proves less than it says", sum)
	}

	// The deployment's state, before any node of this tree has opened it,
	// is the one the release of schema 4 opens: of schema 4, with the
	// buckets of bbolt that release wrote and no usage record.
	if state := schema5StateOf(t, meta); state.Schema != 4 || len(state.Usage) != 0 || !slices.Equal(state.Buckets, contents.StateBuckets) {
		t.Fatalf("the fixture: a state of schema %d with %d usage records and the buckets %v; want schema 4, none, and %v",
			state.Schema, len(state.Usage), state.Buckets, contents.StateBuckets)
	}
	// everything reads every version of the manifest, by its ID where it has
	// one, and holds it to the bytes it was; root reads, which is counted.
	everything := func(when string, node *schema3Node) {
		t.Helper()
		read := 0
		for _, b := range contents.Buckets {
			for _, o := range b.Objects {
				got, err := schema3Get(ctx, node.root, b.Name, &o.schema3Object)
				if err != nil || int64(len(got)) != o.Size || sumOf(got) != o.SHA256 {
					t.Errorf("%s: %s/%s %s is %d bytes, %v; want the %d it was", when, b.Name, o.Key, o.VersionID, len(got), err, o.Size)
					continue
				}
				o.count(schema3Root, false, 1, 0)
				read++
			}
		}
		if read < 300 {
			t.Errorf("%s: %d versions were read: the test proves less than it says", when, read)
		}
	}
	// noDrift says that no recount ended on the node, since it started,
	// with other totals than were kept: the counter is shown, and at 0.
	noDrift := func(when string, node *schema3Node) {
		t.Helper()
		status, _, body := adminCall(ctx, t, http.MethodGet, strings.TrimSuffix(node.admin, "/api/v1")+"/metrics", schema3RootBearer(), nil)
		if status != http.StatusOK {
			t.Fatalf("%s: the metrics = %d", when, status)
		}
		value, shown := "", false
		for line := range strings.Lines(string(body)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), schema5DriftMetric+" "); ok {
				value, shown = v, true
			}
		}
		if !shown || value != "0" {
			t.Errorf("%s: %s = %q (shown: %t); want 0", when, schema5DriftMetric, value, shown)
		}
	}
	// records says of the deployment's state, while no node has it open,
	// that it is of schema 5 and holds exactly the usage records named,
	// each exact, not being counted, and of the totals wanted, and returns
	// the records as they are stored.
	records := func(when string, of ...string) map[string]string {
		t.Helper()
		state := schema5StateOf(t, meta)
		if state.Schema != 5 {
			t.Fatalf("%s: a state of schema %d, want 5", when, state.Schema)
		}
		if got := slices.Sorted(maps.Keys(state.Usage)); !slices.Equal(got, slices.Sorted(slices.Values(of))) {
			t.Fatalf("%s: the state holds the usage records of %v; want those of %v", when, got, of)
		}
		for _, name := range of {
			usage, err := bucket.ParseUsage([]byte(state.Usage[name]))
			if err != nil {
				t.Fatalf("%s: the stored usage record of %s: %v (%s)", when, name, err, state.Usage[name])
			}
			if !usage.Exact || usage.Walking() || usage.UsageTotals != want[name] {
				t.Errorf("%s: %s holds exact = %v, being counted = %v, %+v; want %+v (%s)", when, name, usage.Exact, usage.Walking(), usage.UsageTotals, want[name], state.Usage[name])
			}
		}
		// Everything the release of schema 4 kept is still what the state
		// holds, beside what schema 5 brought.
		for _, name := range contents.StateBuckets {
			if !slices.Contains(state.Buckets, name) {
				t.Errorf("%s: the state no longer holds the bucket %s of what schema 4 keeps", when, name)
			}
		}
		return state.Usage
	}

	// 1. Opened and read. What is shown is the fixture's own: who created
	// each bucket and its latest use, who uploaded and who read every
	// version; every version is the bytes it was; the versions, the delete
	// markers and the upload in progress are listed as they were written.
	node, stop := schema4FixtureServer(ctx, t, bin, dir)
	var settings struct {
		Cluster struct {
			MetadataLevel int `json:"metadata_level"`
		} `json:"cluster"`
	}
	status, _, body := adminCall(ctx, t, http.MethodGet, node.admin+"/settings", schema3RootBearer(), nil)
	if err := json.Unmarshal(body, &settings); err != nil || status != http.StatusOK || settings.Cluster.MetadataLevel != raftmeta.MetadataVersion || raftmeta.MetadataVersion < 5 {
		t.Fatalf("the settings = %d (%v), metadata level %d; a node on its own writes at level %d from its first start", status, err, settings.Cluster.MetadataLevel, raftmeta.MetadataVersion)
	}
	for _, differs := range schema4FixtureShown(ctx, t, node, &contents, true) {
		t.Errorf("the buckets of the schema-4 deployment: %s", differs)
	}
	for _, differs := range schema3Recorded(ctx, t, node, contents.versions()) {
		t.Errorf("the record of the schema-4 deployment: %s", differs)
	}
	everything("opened by this binary", node)
	for _, b := range contents.Buckets {
		versions, markers := 0, 0
		for pages := s3.NewListObjectVersionsPaginator(node.root, &s3.ListObjectVersionsInput{Bucket: aws.String(b.Name), MaxKeys: aws.Int32(schema5Page)}); pages.HasMorePages(); {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Fatalf("versions of %s: %v", b.Name, err)
			}
			versions, markers = versions+len(page.Versions), markers+len(page.DeleteMarkers)
		}
		if versions != len(b.Objects) || markers != len(b.Markers) {
			t.Errorf("%s lists %d versions and %d markers; the manifest says %d and %d", b.Name, versions, markers, len(b.Objects), len(b.Markers))
		}
	}
	film := plain.Uploads[0]
	parts, err := node.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(plain.Name), Key: aws.String(film.Key), UploadId: aws.String(film.UploadID)})
	if err != nil || len(parts.Parts) != len(film.Parts) {
		t.Fatalf("the parts of the upload in progress of %s/%s: %v", plain.Name, film.Key, err)
	}
	// Nobody asked for anything to be counted: the node counts every
	// bucket of the fixture by itself, to exactly the manifest's figures,
	// and no first count is drift.
	schema5Counted(ctx, t, node, want, "counted by the node of its own accord", names...)
	noDrift("after the first count", node)
	stop()
	// The walk is of schema 5: the state is, and holds the records of the
	// buckets that were there and of no other.
	records("after this binary opened and read the deployment", names...)

	// 2. Written to, in every way the release of schema 4 knew, into a
	// bucket that was there: each change moves its record by exactly what
	// it is.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	writer := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	const transient = "schema-five/transient.txt"
	content := "written by this binary into a bucket of the fixture"
	size := int64(len(content))
	if _, err := writer.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(plain.Name), Key: aws.String(transient), Body: strings.NewReader(content)}); err != nil {
		t.Fatalf("PutObject with the key of the fixture's user: %v", err)
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{Objects: 1, CurrentBytes: size, Versions: 1, StoredBytes: size})
	schema5Counted(ctx, t, node, want, "after a put into a bucket of the fixture", plain.Name)
	// Where versions are not kept a delete removes.
	if _, err := writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(plain.Name), Key: aws.String(transient)}); err != nil {
		t.Fatalf("DeleteObject with the key of the fixture's user: %v", err)
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{Objects: -1, CurrentBytes: -size, Versions: -1, StoredBytes: -size})
	schema5Counted(ctx, t, node, want, "after a delete from a bucket of the fixture", plain.Name)
	begun, err := writer.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String(transient)})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if _, err := writer.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(plain.Name), Key: aws.String(transient), UploadId: begun.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(make([]byte, 2048)),
	}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{UploadBytes: 2048})
	schema5Counted(ctx, t, node, want, "after a part of an upload into a bucket of the fixture", plain.Name)
	if _, err := writer.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String(transient), UploadId: begun.UploadId}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{UploadBytes: -2048})
	schema5Counted(ctx, t, node, want, "after the abort of that upload", plain.Name)
	stop()
	if want[plain.Name] != schema5Totals(plain.Usage) {
		t.Fatalf("a put and its delete, a part and its abort leave %s at %+v as the test reckons; the manifest says %+v", plain.Name, want[plain.Name], schema5Totals(plain.Usage))
	}
	records("after a put, a delete and a multipart upload by this binary", names...)

	// 3. A bucket created by this binary: counted from the start, and every
	// change of it is.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	writer = node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	if _, err := writer.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema5FirstBucket)}); err != nil {
		t.Fatalf("CreateBucket with the key of the fixture's user: %v", err)
	}
	put := func(client *s3.Client, bucketName, key string, size int) {
		t.Helper()
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key), Body: bytes.NewReader(bytes.Repeat([]byte{'5'}, size))}); err != nil {
			t.Fatalf("PutObject %s/%s of %d bytes: %v", bucketName, key, size, err)
		}
	}
	put(writer, schema5FirstBucket, "a", 100)
	put(writer, schema5FirstBucket, "b", 50)
	// Where versions are not kept a put replaces, and a delete removes.
	put(writer, schema5FirstBucket, "a", 30)
	if _, err := writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(schema5FirstBucket), Key: aws.String("b")}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	// An upload left in progress with one part, and one that is aborted.
	for key, abort := range map[string]bool{"left": false, "aborted": true} {
		upload, err := writer.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(schema5FirstBucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("CreateMultipartUpload %s: %v", key, err)
		}
		if _, err := writer.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(schema5FirstBucket), Key: aws.String(key), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(make([]byte, 700)),
		}); err != nil {
			t.Fatalf("UploadPart of %s: %v", key, err)
		}
		if !abort {
			continue
		}
		if _, err := writer.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(schema5FirstBucket), Key: aws.String(key), UploadId: upload.UploadId}); err != nil {
			t.Fatalf("AbortMultipartUpload of %s: %v", key, err)
		}
	}
	// From here on versions are kept: a put adds one, and a delete makes a
	// marker, which is a version of no bytes.
	if _, err := node.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(schema5FirstBucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	put(writer, schema5FirstBucket, "a", 40)
	if _, err := writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(schema5FirstBucket), Key: aws.String("a")}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	put(writer, schema5FirstBucket, "c", 9)
	want[schema5FirstBucket] = bucket.UsageTotals{Objects: 1, CurrentBytes: 9, Versions: 4, StoredBytes: 79, UploadBytes: 700}
	// Exact from its creation: the management API answers from the record
	// at once, with every figure of it.
	if got, exact := schema5UsageOf(ctx, t, node, schema5FirstBucket); !exact || got != want[schema5FirstBucket] {
		t.Errorf("the usage of %s, created by this binary = exact %v, %+v; want exact, %+v", schema5FirstBucket, exact, got, want[schema5FirstBucket])
	}
	stop()
	all := append(slices.Clone(names), schema5FirstBucket)
	records("after a bucket was created by this binary", all...)

	// 4. A recount of every bucket, asked for through the management API:
	// each is counted again from its index and found to be what was kept.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	recount := map[string]string{"Content-Type": "application/json"}
	maps.Copy(recount, schema3RootBearer())
	for _, name := range all {
		if status, _, body := adminCall(ctx, t, http.MethodPost, node.admin+"/buckets/"+name+"/usage/recount", recount, nil); status != http.StatusAccepted {
			t.Fatalf("the recount of %s = %d %s; want 202", name, status, body)
		}
	}
	schema5Counted(ctx, t, node, want, "after a recount of every bucket", all...)
	noDrift("after a recount of every bucket", node)
	stop()
	records("after a recount of every bucket", all...)

	// 5. The node again: a put and a delete that makes a marker, in a bucket
	// that keeps versions; a part more of the upload that was in progress
	// when the fixture was written, and then that upload aborted. And
	// everything the fixture held is still the bytes it was.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	everything("after the state became schema 5", node)
	const added = "schema-five/added"
	put(node.root, versioned.Name, added, 123)
	want[versioned.Name] = schema5Moved(want[versioned.Name], bucket.UsageTotals{Objects: 1, CurrentBytes: 123, Versions: 1, StoredBytes: 123})
	deleted, err := node.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(versioned.Name), Key: aws.String(added)})
	if err != nil || !aws.ToBool(deleted.DeleteMarker) {
		t.Fatalf("DeleteObject in a bucket that keeps versions = a marker: %t, %v", err == nil && aws.ToBool(deleted.DeleteMarker), err)
	}
	want[versioned.Name] = schema5Moved(want[versioned.Name], bucket.UsageTotals{Objects: -1, CurrentBytes: -123, Versions: 1})
	if _, err := node.root.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(plain.Name), Key: aws.String(film.Key), UploadId: aws.String(film.UploadID),
		PartNumber: aws.Int32(film.Parts[len(film.Parts)-1].Number + 1), Body: bytes.NewReader(make([]byte, 4096)),
	}); err != nil {
		t.Fatalf("a part more of the upload of the fixture: %v", err)
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{UploadBytes: 4096})
	schema5Counted(ctx, t, node, want, "after a put, a delete marker and a part", versioned.Name, plain.Name)
	stop()
	records("after a put, a delete marker and a part", all...)

	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	if _, err := node.root.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String(film.Key), UploadId: aws.String(film.UploadID)}); err != nil {
		t.Fatalf("the abort of the upload of the fixture: %v", err)
	}
	held := int64(4096)
	for _, p := range film.Parts {
		held += p.Size
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{UploadBytes: -held})
	if want[plain.Name].UploadBytes != 0 {
		t.Fatalf("after its one upload was aborted %s is to hold no bytes of parts, and the test reckons %d", plain.Name, want[plain.Name].UploadBytes)
	}
	schema5Counted(ctx, t, node, want, "after an abort", plain.Name)
	stop()
	stored := records("after an abort", all...)

	// 6. A restart changes no record: the log is replayed over the state,
	// nothing is counted a second time, and the counter finds nothing to
	// count.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	listed, err := node.root.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(listed.Buckets) != len(all) {
		t.Fatalf("ListBuckets after a restart = %d buckets, %v; want %d", len(listed.Buckets), err, len(all))
	}
	schema5Counted(ctx, t, node, want, "after a restart", all...)
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) {
		t.Fatal("a secret was written to standard error")
	}
	stop()
	if again := records("after a restart", all...); !maps.Equal(again, stored) {
		t.Errorf("after a restart the usage records are\n%v\nwant\n%v", again, stored)
	}

	// The fixture itself was not touched.
	if after := schema5Sums(t, fixture); !maps.Equal(after, frozen) || len(frozen) < 1000 {
		t.Errorf("the fixture changed while the test ran, or is not the fixture: %d files, were %d", len(after), len(frozen))
	}
}
