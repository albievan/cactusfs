//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"log/slog"
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
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/ports"
)

// The upgrade from entry schema 4 to entry schema 5 (ADR-0023, "Migration",
// "Rollback"), on the deployment the binary of commit 84334a7 wrote: the
// fixture testdata/schema-4, which is frozen and of which every test opens a
// copy. TestOpenSchema4Fixture says that everything the fixture holds is
// still there; this test says what becomes of its state.
const (
	// schema5FirstBucket is the bucket whose creation is the first entry of
	// schema 5 the deployment's state applies.
	schema5FirstBucket = "schema-five-first"
	// schema5WalkLimit is how many records one entry of the walk visits,
	// and schema5WalkSteps how many entries the walk through the bucket of
	// many keys must take at the least.
	schema5WalkLimit = 50
	schema5WalkSteps = 6
	// schema5FixtureBuckets is how many buckets the fixture must hold for
	// the test to be worth its name.
	schema5FixtureBuckets = 7
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

// TestUpgradeFromSchema4: the schema-4 deployment that the binary of commit
// 84334a7 wrote is opened by this binary. Migration: none for the state
// (ADR-0023). It opens as it is; a node that runs alone writes at level 5
// from its first start, and still the state stays one of schema 4 - which
// the release that wrote it opens - while it is read, and while it is
// written to in every way that release knew: a put, a delete, a multipart
// upload. In this build the one thing a deployment does that is an entry of
// schema 5 is to create a bucket (nothing counts without being asked before
// PLAN 8.3.1, and no use case says who deletes yet): the first bucket that
// is created makes the state one of schema 5, and there the way back to the
// release of schema 4 ends. That bucket is counted from the start, and what
// is put into it, deleted from it and uploaded to it in parts moves its
// record by exactly what it is.
//
// The buckets that were there have no record until they are counted. No API
// asks for that yet (POST /buckets/{bucket}/usage/recount is of PLAN 8.3.3),
// so the metadata store of this tree is asked directly, on the deployment's
// own state, as TestUpgradeFromSchema3ToSchema4 asks it: entries of the walk
// of fifty records each bring every bucket of the fixture to exactly the
// figures its generator wrote down - the bucket of three hundred keys in six
// entries or more, the upload in progress with its three parts, the object a
// multipart upload became - and a recount finds no drift. Then the node,
// started again, takes a put, a delete that makes a marker, a part and an
// abort, and the records move by exactly those. Everything the fixture held
// reads back as the bytes it was, after the state became schema 5 as before.
//
// The same walk on the same state, without a node, is
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
	for _, b := range contents.Buckets {
		names = append(names, b.Name)
		want[b.Name] = schema5Totals(b.Usage)
		switch {
		case b.Usage.Objects >= schema5WalkSteps*schema5WalkLimit:
			many = b
		case b.Versions && !b.ObjectLock && len(b.Markers) > 0:
			versioned = b
		case !b.Versions && len(b.Uploads) > 0:
			plain = b
		}
	}
	if many == nil || versioned == nil || plain == nil || len(plain.Uploads[0].Parts) != 3 {
		t.Fatalf("the fixture has no bucket of %d keys, none that keeps versions and holds a marker, or none with an upload in progress of three parts",
			schema5WalkSteps*schema5WalkLimit)
	}

	// schema4 says of the deployment's state, while no node has it open,
	// that it is still the one the release of schema 4 opens: of schema 4,
	// with the buckets of bbolt that release wrote and no usage record.
	schema4 := func(when string) {
		t.Helper()
		state := schema5StateOf(t, meta)
		if state.Schema != 4 || len(state.Usage) != 0 || !slices.Equal(state.Buckets, contents.StateBuckets) {
			t.Fatalf("%s: a state of schema %d with %d usage records and the buckets %v; want schema 4, none, and %v",
				when, state.Schema, len(state.Usage), state.Buckets, contents.StateBuckets)
		}
	}
	schema4("the fixture")
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
	if err := json.Unmarshal(body, &settings); err != nil || status != http.StatusOK || settings.Cluster.MetadataLevel != raftmeta.MetadataVersion || raftmeta.MetadataVersion != 5 {
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
		for pages := s3.NewListObjectVersionsPaginator(node.root, &s3.ListObjectVersionsInput{Bucket: aws.String(b.Name), MaxKeys: aws.Int32(schema5WalkLimit)}); pages.HasMorePages(); {
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
	stop()
	schema4("after this binary opened and read the deployment")

	// 2. Written to, in every way the release of schema 4 knew, into a
	// bucket that was there: none of it is an entry of schema 5, and none
	// of it is counted - there is no record to keep.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	writer := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	const transient = "schema-five/transient.txt"
	if _, err := writer.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(plain.Name), Key: aws.String(transient), Body: strings.NewReader("before the first entry of schema 5")}); err != nil {
		t.Fatalf("PutObject with the key of the fixture's user: %v", err)
	}
	if _, err := writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(plain.Name), Key: aws.String(transient)}); err != nil {
		t.Fatalf("DeleteObject with the key of the fixture's user: %v", err)
	}
	begun, err := writer.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String(transient)})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if _, err := writer.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(plain.Name), Key: aws.String(transient), UploadId: begun.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(make([]byte, 2048)),
	}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if _, err := writer.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String(transient), UploadId: begun.UploadId}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	stop()
	schema4("after a put, a delete and a multipart upload by this binary")

	// 3. The first bucket that is created: the first entry of schema 5.
	// It is counted from the start, and every change of it is.
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
	// What the node itself counts, by walking the current versions, is what
	// the record must say of them.
	var counted struct {
		ObjectCount int64 `json:"object_count"`
		TotalSize   int64 `json:"total_size"`
		Complete    bool  `json:"complete"`
	}
	status, _, body = adminCall(ctx, t, http.MethodGet, node.admin+"/buckets/"+schema5FirstBucket+"/usage", schema3RootBearer(), nil)
	if err := json.Unmarshal(body, &counted); err != nil || status != http.StatusOK || counted.ObjectCount != want[schema5FirstBucket].Objects || counted.TotalSize != want[schema5FirstBucket].CurrentBytes {
		t.Errorf("the usage of %s as the node walks it = %d %s (%v); want %d objects of %d bytes", schema5FirstBucket, status, body, err,
			want[schema5FirstBucket].Objects, want[schema5FirstBucket].CurrentBytes)
	}
	stop()

	// records says of the deployment's state that it is of schema 5 and
	// holds exactly the usage records named, each exact and of the totals
	// wanted, and returns the records as they are stored.
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
				t.Errorf("%s: %s holds exact = %v, %+v; want %+v (%s)", when, name, usage.Exact, usage.UsageTotals, want[name], state.Usage[name])
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
	// The new bucket has its record; no bucket that was there has one: in
	// this build nothing counts them without being asked.
	records("after the first bucket of schema 5", schema5FirstBucket)

	// 4. The buckets that were there are counted: entries of the walk, fifty
	// records each, proposed through the metadata store of this tree on the
	// deployment's own state.
	open := func() *raftmeta.Store {
		t.Helper()
		store, err := raftmeta.Open(ctx, raftmeta.Config{Dir: meta, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader})
		if err != nil {
			t.Fatalf("opening the metadata of the deployment: %v", err)
		}
		return store
	}
	store := open()
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = store.Close()
		}
	})
	before, err := store.BucketUsages(ctx, names)
	if err != nil {
		t.Fatalf("BucketUsages over the deployment: %v", err)
	}
	for i, u := range before {
		if u.Usage != (bucket.Usage{}) || u.QuotaBytes != 0 {
			t.Errorf("the bucket %s of the fixture has the usage %+v before it was counted", names[i], u)
		}
	}
	steps := map[string]int{}
	var sum bucket.UsageTotals
	for _, name := range names {
		for {
			walk, err := store.WalkBucketUsage(ctx, name, schema5WalkLimit, false)
			if err != nil {
				t.Fatalf("entry %d of the walk through %s: %v", steps[name]+1, name, err)
			}
			steps[name]++
			if !walk.Usage.Walking() {
				if walk.Drift || !walk.Usage.Exact || walk.Usage.UsageTotals != want[name] {
					t.Errorf("%s was counted, in %d entries, to exact = %v, %+v, with drift = %v; the manifest says %+v",
						name, steps[name], walk.Usage.Exact, walk.Usage.UsageTotals, walk.Drift, want[name])
				}
				break
			}
			// Nothing may be read from the totals of a first count.
			if walk.Usage.Exact || walk.Usage.UsageTotals != (bucket.UsageTotals{}) {
				t.Errorf("entry %d of the first count of %s = exact %v, %+v", steps[name], name, walk.Usage.Exact, walk.Usage.UsageTotals)
			}
			if steps[name] > 100 {
				t.Fatalf("the walk through %s does not come to an end", name)
			}
		}
		sum = schema5Moved(sum, want[name])
	}
	// Guards against proving less than is said.
	if steps[many.Name] < schema5WalkSteps {
		t.Errorf("the walk through %s, of %d keys, took %d entries of %d records; want at least %d", many.Name, many.Usage.Objects, steps[many.Name], schema5WalkLimit, schema5WalkSteps)
	}
	if sum.Objects == 0 || sum.CurrentBytes == 0 || sum.Versions <= sum.Objects || sum.StoredBytes <= sum.CurrentBytes || sum.UploadBytes == 0 ||
		want[plain.Name].UploadBytes == 0 || want[versioned.Name].Versions <= want[versioned.Name].Objects {
		t.Errorf("the buckets of the fixture together hold %+v: the test proves less than it says", sum)
	}
	// A recount finds what was counted.
	for _, name := range names {
		for n := 0; ; n++ {
			walk, err := store.WalkBucketUsage(ctx, name, schema5WalkLimit, n == 0)
			if err != nil || walk.Drift || !walk.Usage.Exact || walk.Usage.UsageTotals != want[name] {
				t.Fatalf("entry %d of the recount of %s = %+v, %v; want %+v, exact and without drift", n+1, name, walk, err, want[name])
			}
			if !walk.Usage.Walking() {
				break
			}
			if n > 100 {
				t.Fatalf("the recount of %s does not come to an end", name)
			}
		}
	}
	// The usage of every bucket is one question now, from one view.
	all := append(slices.Clone(names), schema5FirstBucket)
	var usages []ports.BucketUsage
	if usages, err = store.BucketUsages(ctx, all); err != nil || len(usages) != len(all) {
		t.Fatalf("BucketUsages over the deployment = %d records, %v", len(usages), err)
	}
	for i, u := range usages {
		if !u.Usage.Exact || u.Usage.UsageTotals != want[all[i]] {
			t.Errorf("the store reads the usage of %s as exact = %v, %+v; want %+v", all[i], u.Usage.Exact, u.Usage.UsageTotals, want[all[i]])
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	records("after every bucket was counted", all...)

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
	stop()
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{UploadBytes: 4096})
	records("after a put, a delete marker and a part", all...)

	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	if _, err := node.root.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(plain.Name), Key: aws.String(film.Key), UploadId: aws.String(film.UploadID)}); err != nil {
		t.Fatalf("the abort of the upload of the fixture: %v", err)
	}
	stop()
	held := int64(4096)
	for _, p := range film.Parts {
		held += p.Size
	}
	want[plain.Name] = schema5Moved(want[plain.Name], bucket.UsageTotals{UploadBytes: -held})
	if want[plain.Name].UploadBytes != 0 {
		t.Fatalf("after its one upload was aborted %s is to hold no bytes of parts, and the test reckons %d", plain.Name, want[plain.Name].UploadBytes)
	}
	stored := records("after an abort", all...)

	// 6. A restart changes no record: the log is replayed over the state,
	// and nothing is counted a second time.
	node, stop = schema4FixtureServer(ctx, t, bin, dir)
	listed, err := node.root.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(listed.Buckets) != len(all) {
		t.Fatalf("ListBuckets after a restart = %d buckets, %v; want %d", len(listed.Buckets), err, len(all))
	}
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) {
		t.Fatal("a secret was written to standard error")
	}
	stop()
	if again := records("after a restart", all...); !maps.Equal(again, stored) {
		t.Errorf("after a restart the usage records are\n%v\nwant\n%v", again, stored)
	}
	t.Logf("the walk took, by bucket, %v entries of %d records", steps, schema5WalkLimit)

	// The fixture itself was not touched.
	if after := schema5Sums(t, fixture); !maps.Equal(after, frozen) || len(frozen) < 1000 {
		t.Errorf("the fixture changed while the test ran, or is not the fixture: %d files, were %d", len(after), len(frozen))
	}
}
