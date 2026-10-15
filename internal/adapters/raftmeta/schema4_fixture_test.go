package raftmeta

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The migration of ADR-0023, proved on what the release of schema 4 wrote:
// the metadata directory of the fixture test/integration/testdata/schema-4 -
// the state database, the Raft log and the node file of a standalone
// deployment, written by the binary of commit 84334a7 and never by a later
// one - is opened by this store, through its real start-up, and counted.
//
// The fixture is frozen. This test opens a COPY of its metadata directory,
// and says at its end that the fixture holds the bytes it held.

// schema4FixtureMeta is where the fixture is, from this package.
func schema4FixtureMeta() string {
	return filepath.Join("..", "..", "..", "test", "integration", "testdata", "schema-4")
}

// Bounds of the test that counts the fixture: the limit of an entry of the
// walk, how many entries the walk through the bucket of many keys must take
// at the least, and how many buckets the fixture must hold for the test to
// be worth its name.
const (
	schema4FixtureWalk    = 50
	schema4FixtureSteps   = 6
	schema4FixtureBuckets = 7
	schema4FixtureMany    = "schema-four-many"
)

// fixtureUsage is what the manifest of the fixture says a bucket holds: its
// generator counted the figures from what it wrote, not from what the node
// said.
type fixtureUsage struct {
	Objects      int64 `json:"objects"`
	CurrentBytes int64 `json:"current_bytes"`
	// Versions is every stored version that is no delete marker, and
	// Markers the delete markers: the manifest counts them apart.
	Versions    int64 `json:"versions"`
	Markers     int64 `json:"markers"`
	StoredBytes int64 `json:"stored_bytes"`
	// Uploads is the multipart uploads in progress, and UploadBytes the
	// sizes of their parts together.
	Uploads     int64 `json:"uploads"`
	UploadBytes int64 `json:"upload_bytes"`
}

// totals gives the figures as a usage record holds them (ADR-0023, "What is
// counted"): a delete marker is a version of no bytes, so the record's
// versions are the manifest's versions and its markers together; the number
// of uploads is not in a record, only the bytes of their parts.
func (u fixtureUsage) totals() bucket.UsageTotals {
	return bucket.UsageTotals{
		Objects: u.Objects, CurrentBytes: u.CurrentBytes, Versions: u.Versions + u.Markers, StoredBytes: u.StoredBytes, UploadBytes: u.UploadBytes,
	}
}

// fixtureBucket is what this test reads of a bucket of the manifest.
type fixtureBucket struct {
	Name     string `json:"name"`
	Versions bool   `json:"versions"`
	Objects  []struct {
		Key  string `json:"key"`
		Size int64  `json:"size"`
		// ETag is set for the version a multipart upload became.
		ETag string `json:"etag"`
	} `json:"objects"`
	Uploads []struct {
		Key      string `json:"key"`
		UploadID string `json:"upload_id"`
		Parts    []struct {
			Number int   `json:"number"`
			Size   int64 `json:"size"`
		} `json:"parts"`
	} `json:"uploads"`
	Usage fixtureUsage `json:"usage"`
}

// fixtureManifest is what this test reads of the manifest of the fixture.
type fixtureManifest struct {
	Commit       string          `json:"commit"`
	StateSchema  uint64          `json:"state_schema"`
	StateBuckets []string        `json:"state_buckets"`
	Buckets      []fixtureBucket `json:"buckets"`
}

// sumsOf returns the SHA-256 of every file under a directory, by its path.
func sumsOf(t *testing.T, dir string) map[string]string {
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
		sum := sha256.Sum256(content)
		sums[path] = hex.EncodeToString(sum[:])
		return nil
	}); err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return sums
}

// openFixtureStore opens the store of a standalone node on a metadata
// directory, as the node itself does: the Raft log that is there is the log
// it goes on from.
func openFixtureStore(t *testing.T, dir string) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, Config{
		Dir: dir, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader,
		Timing: Timing{Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond, LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("open the metadata of the schema-4 fixture: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// stateBuckets returns the names of the buckets of bbolt a state holds.
func stateBuckets(t *testing.T, f *fsm) []string {
	t.Helper()
	var names []string
	for _, name := range everyBucket(t, f) {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

// TestTheSchema4FixtureIsCountedToItsManifest: the metadata of the schema-4
// deployment is opened by this release. Being opened and read changes
// nothing of it: the state says 4 and holds the buckets of bbolt it held, and
// no bucket has a usage record. The first entry of schema 5 - a bucket that
// is created, as in the product - makes it a state of schema 5, and still no
// bucket that was there has a record. Then entries of the walk, fifty
// records each, count every bucket of the fixture to exactly the figures its
// generator wrote down: the bucket of three hundred keys in six entries or
// more; the upload in progress with its three parts; the object a multipart
// upload became, as one version of the bytes of its parts; the delete marker
// as a version of no bytes; the bucket nobody used, to nothing. A recount
// finds the same and no drift. A put, a delete marker, a part and an abort
// then move the records by exactly what they are, and all of it is there
// after a restart, byte for byte.
func TestTheSchema4FixtureIsCountedToItsManifest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fixture := schema4FixtureMeta()
	frozen := sumsOf(t, filepath.Join(fixture, "meta"))
	raw, err := fs.ReadFile(os.DirFS(fixture), "manifest.json")
	if err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	var manifest fixtureManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	if manifest.StateSchema != stateSchema4 || len(manifest.Buckets) < schema4FixtureBuckets || len(frozen) != 3 {
		t.Fatalf("the fixture is of state schema %d with %d buckets and %d files of metadata: the test proves less than it says",
			manifest.StateSchema, len(manifest.Buckets), len(frozen))
	}
	// A copy: the fixture itself is never opened.
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(fixture, "meta"))); err != nil {
		t.Fatalf("copy the metadata of the fixture: %v", err)
	}
	store := openFixtureStore(t, dir)
	var names []string
	want := map[string]bucket.UsageTotals{}
	for _, b := range manifest.Buckets {
		names = append(names, b.Name)
		want[b.Name] = b.Usage.totals()
	}
	usages := func(of ...string) []ports.BucketUsage {
		t.Helper()
		got, err := store.BucketUsages(ctx, of)
		if err != nil {
			t.Fatalf("BucketUsages: %v", err)
		}
		return got
	}
	uncounted := func(step string) {
		t.Helper()
		for i, u := range usages(names...) {
			if u.Usage != (bucket.Usage{}) || u.QuotaBytes != 0 {
				t.Errorf("%s: the bucket %s of the fixture has the usage %+v", step, names[i], u)
			}
		}
	}

	// Opened, and read: the buckets, every version of every bucket, every
	// upload and its parts. The state is the one that was written.
	opened := wholeState(t, store.fsm)
	listed, err := store.ListBuckets(ctx)
	if err != nil || len(listed) != len(manifest.Buckets) {
		t.Fatalf("ListBuckets = %d buckets, %v; the manifest lists %d", len(listed), err, len(manifest.Buckets))
	}
	uploads := map[string]int64{}
	for _, b := range manifest.Buckets {
		versions, bytesOf := int64(0), int64(0)
		if err := store.ScanVersions(ctx, b.Name, ports.VersionCursor{}, func(v object.Version, _ bool) ports.ScanControl {
			versions++
			bytesOf += v.Size
			return ports.ScanControl{}
		}); err != nil {
			t.Fatalf("the versions of %s: %v", b.Name, err)
		}
		if versions != want[b.Name].Versions || bytesOf != want[b.Name].StoredBytes {
			t.Errorf("%s lists %d versions of %d bytes; the manifest says %d of %d", b.Name, versions, bytesOf, want[b.Name].Versions, want[b.Name].StoredBytes)
		}
		if err := store.ScanUploads(ctx, b.Name, ports.UploadCursor{}, func(object.Upload) ports.ScanControl {
			uploads[b.Name]++
			return ports.ScanControl{}
		}); err != nil {
			t.Fatalf("the uploads of %s: %v", b.Name, err)
		}
		if uploads[b.Name] != b.Usage.Uploads {
			t.Errorf("%s lists %d uploads in progress; the manifest says %d", b.Name, uploads[b.Name], b.Usage.Uploads)
		}
	}
	if got := stateSchema(t, store.fsm); got != stateSchema4 {
		t.Fatalf("after being opened and read the state is of schema %d, want %d", got, stateSchema4)
	}
	if got := stateBuckets(t, store.fsm); !slices.Equal(got, manifest.StateBuckets) {
		t.Errorf("after being opened and read the state holds the buckets %v; the release of schema 4 wrote %v", got, manifest.StateBuckets)
	}
	if got := wholeState(t, store.fsm); !reflect.DeepEqual(got, opened) {
		t.Errorf("being read changed the state")
	}
	uncounted("after being opened and read")

	// The first entry of schema 5, as the product makes it: a bucket.
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "schema-five-first", CreatedAt: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC), CreatedBy: "root"}); err != nil {
		t.Fatalf("the first bucket of schema 5: %v", err)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema5 {
		t.Fatalf("after the first entry of schema 5 the state is of schema %d", got)
	}
	if u := usages("schema-five-first")[0]; !u.Usage.Exact || u.Usage.Walking() || u.Usage.UsageTotals != (bucket.UsageTotals{}) {
		t.Errorf("the usage of the first bucket of schema 5 = %+v", u)
	}
	// It did not count what was there: that is the walk's.
	uncounted("after the first entry of schema 5")

	// The walk, fifty records an entry, to the end of every bucket.
	steps := map[string]int{}
	var sum bucket.UsageTotals
	withUploads, withMarkers, withOlder, multipart, empty := 0, 0, 0, 0, 0
	for _, b := range manifest.Buckets {
		for {
			walk, err := store.WalkBucketUsage(ctx, b.Name, schema4FixtureWalk, false)
			if err != nil {
				t.Fatalf("entry %d of the walk through %s: %v", steps[b.Name]+1, b.Name, err)
			}
			steps[b.Name]++
			if !walk.Usage.Walking() {
				if walk.Drift || !walk.Usage.Exact {
					t.Errorf("the first count of %s ended exact = %v, with drift = %v", b.Name, walk.Usage.Exact, walk.Drift)
				}
				break
			}
			// Nothing may be read from the totals of a first count.
			if walk.Usage.Exact || walk.Usage.UsageTotals != (bucket.UsageTotals{}) {
				t.Errorf("entry %d of the first count of %s = exact %v, %+v", steps[b.Name], b.Name, walk.Usage.Exact, walk.Usage.UsageTotals)
			}
			if steps[b.Name] > 100 {
				t.Fatalf("the walk through %s does not come to an end", b.Name)
			}
		}
		got := usages(b.Name)[0].Usage
		if !got.Exact || got.Walking() || got.UsageTotals != want[b.Name] {
			t.Errorf("%s was counted, in %d entries, to exact = %v, %+v; the manifest says %+v", b.Name, steps[b.Name], got.Exact, got.UsageTotals, want[b.Name])
		}
		// The greatest sequence the walk found: a bucket that keeps no
		// versions holds them at the sequence 0 they always had.
		if (b.Versions && got.Versions > 0) != (got.High > 0) {
			t.Errorf("%s (keeps versions: %t) holds %d versions and its greatest sequence is %d", b.Name, b.Versions, got.Versions, got.High)
		}
		sum.Objects, sum.CurrentBytes, sum.Versions = sum.Objects+got.Objects, sum.CurrentBytes+got.CurrentBytes, sum.Versions+got.Versions
		sum.StoredBytes, sum.UploadBytes = sum.StoredBytes+got.StoredBytes, sum.UploadBytes+got.UploadBytes
		if b.Usage.UploadBytes > 0 && b.Usage.Uploads > 0 {
			withUploads++
		}
		if b.Usage.Markers > 0 {
			withMarkers++
		}
		if b.Usage.Versions > b.Usage.Objects {
			withOlder++
		}
		if b.Usage == (fixtureUsage{}) {
			empty++
		}
		for _, o := range b.Objects {
			if o.ETag != "" && o.Size >= object.MinPartSize {
				multipart++
			}
		}
	}
	t.Logf("the walk took, by bucket, %v entries of %d records", steps, schema4FixtureWalk)
	// Guards against proving less than is said.
	if steps[schema4FixtureMany] < schema4FixtureSteps || want[schema4FixtureMany].Objects < schema4FixtureSteps*schema4FixtureWalk {
		t.Errorf("the walk through %s, of %d keys, took %d entries of %d records; want at least %d",
			schema4FixtureMany, want[schema4FixtureMany].Objects, steps[schema4FixtureMany], schema4FixtureWalk, schema4FixtureSteps)
	}
	if withUploads == 0 || withMarkers == 0 || withOlder == 0 || multipart == 0 || empty == 0 {
		t.Errorf("the fixture has %d buckets with parts of uploads in progress, %d with delete markers, %d with versions that are not current, "+
			"%d objects of a multipart upload and %d buckets that hold nothing: the test proves less than it says", withUploads, withMarkers, withOlder, multipart, empty)
	}
	if sum.Objects == 0 || sum.CurrentBytes == 0 || sum.Versions <= sum.Objects || sum.StoredBytes <= sum.CurrentBytes || sum.UploadBytes == 0 {
		t.Errorf("all the buckets of the fixture together hold %+v: the test proves less than it says", sum)
	}
	var three bool
	for _, b := range manifest.Buckets {
		for _, u := range b.Uploads {
			three = three || len(u.Parts) == 3
		}
	}
	if !three {
		t.Error("the fixture has no upload in progress of three parts")
	}

	// A recount finds what was counted, and what was kept meanwhile.
	for _, b := range manifest.Buckets {
		for n := 0; ; n++ {
			walk, err := store.WalkBucketUsage(ctx, b.Name, schema4FixtureWalk, n == 0)
			if err != nil || walk.Drift || !walk.Usage.Exact || walk.Usage.UsageTotals != want[b.Name] {
				t.Fatalf("entry %d of the recount of %s = %+v, %v; want %+v, exact and without drift", n+1, b.Name, walk, err, want[b.Name])
			}
			if !walk.Usage.Walking() {
				break
			}
			if n > 100 {
				t.Fatalf("the recount of %s does not come to an end", b.Name)
			}
		}
	}
	if drift := store.fsm.usageDrift.Load(); drift != 0 {
		t.Errorf("%d recounts of the fixture found other totals than the first count", drift)
	}

	// Changes move the records by exactly what they are.
	moved := func(what, name string, by bucket.UsageTotals) {
		t.Helper()
		next := want[name]
		next.Objects, next.CurrentBytes, next.Versions = next.Objects+by.Objects, next.CurrentBytes+by.CurrentBytes, next.Versions+by.Versions
		next.StoredBytes, next.UploadBytes = next.StoredBytes+by.StoredBytes, next.UploadBytes+by.UploadBytes
		want[name] = next
		if got := usages(name)[0].Usage; !got.Exact || got.UsageTotals != next {
			t.Errorf("after %s the bucket %s holds exact = %v, %+v; want %+v", what, name, got.Exact, got.UsageTotals, next)
		}
	}
	var versioned, plain *fixtureBucket
	for i := range manifest.Buckets {
		switch b := &manifest.Buckets[i]; {
		case b.Versions && versioned == nil && b.Usage.Markers > 0:
			versioned = b
		case !b.Versions && plain == nil && len(b.Uploads) > 0:
			plain = b
		}
	}
	if versioned == nil || plain == nil {
		t.Fatal("the fixture has no bucket that keeps versions and holds a marker, or none that keeps none and holds an upload")
	}
	v := porttest.SampleVersion(versioned.Name, "schema-five/put", 201)
	v.Size = 123
	if _, err := store.PutVersion(ctx, v); err != nil {
		t.Fatalf("a put into %s: %v", versioned.Name, err)
	}
	moved("a put", versioned.Name, bucket.UsageTotals{Objects: 1, CurrentBytes: 123, Versions: 1, StoredBytes: 123})
	marker := object.ID(bytes.Repeat([]byte{202}, object.IDSize))
	deleted, err := store.DeleteObject(ctx, versioned.Name, v.Key, ports.DeleteOptions{MarkerID: marker, At: time.Date(2026, 10, 13, 9, 1, 0, 0, time.UTC), By: "root"})
	if err != nil || deleted.Marker == nil {
		t.Fatalf("a delete in %s = %+v, %v; want a marker", versioned.Name, deleted, err)
	}
	// The marker is a version of no bytes, and its key has no object.
	moved("a delete marker", versioned.Name, bucket.UsageTotals{Objects: -1, CurrentBytes: -123, Versions: 1})
	// A part more of the upload that was in progress when the fixture was
	// written, and then that upload aborted: its three parts and the new
	// one go.
	old := plain.Uploads[0]
	id, err := object.ParseID(old.UploadID)
	if err != nil {
		t.Fatalf("the ID of the upload of the fixture: %v", err)
	}
	inProgress, err := store.GetUpload(ctx, plain.Name, old.Key, id)
	if err != nil {
		t.Fatalf("the upload of the fixture: %v", err)
	}
	if _, err := store.PutPart(ctx, plain.Name, old.Key, id, porttest.SamplePart(inProgress, len(old.Parts)+1, 4096, 9)); err != nil {
		t.Fatalf("a part of the upload of the fixture: %v", err)
	}
	moved("a part", plain.Name, bucket.UsageTotals{UploadBytes: 4096})
	held := int64(4096)
	for _, p := range old.Parts {
		held += p.Size
	}
	if _, err := store.AbortUpload(ctx, plain.Name, old.Key, id); err != nil {
		t.Fatalf("the abort of the upload of the fixture: %v", err)
	}
	moved("an abort", plain.Name, bucket.UsageTotals{UploadBytes: -held})
	if want[plain.Name].UploadBytes != 0 && len(plain.Uploads) == 1 {
		t.Errorf("after its one upload was aborted %s holds %d bytes of parts", plain.Name, want[plain.Name].UploadBytes)
	}

	// All of it after a restart, byte for byte.
	records := rawState(t, store.fsm, bucketUsageBucket())
	if len(records) != 1+len(names)+1 {
		t.Errorf("the state holds %d usage records, want one for each of the %d buckets of the fixture and one for the new bucket", len(records)-1, len(names))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openFixtureStore(t, dir)
	if got := rawState(t, store.fsm, bucketUsageBucket()); !reflect.DeepEqual(got, records) {
		t.Errorf("after a restart the usage records are\n%v\nwant\n%v", got, records)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema5 {
		t.Errorf("after a restart the state is of schema %d", got)
	}
	for i, u := range usages(names...) {
		if !u.Usage.Exact || u.Usage.Walking() || u.Usage.UsageTotals != want[names[i]] {
			t.Errorf("after a restart %s holds exact = %v, %+v; want %+v", names[i], u.Usage.Exact, u.Usage.UsageTotals, want[names[i]])
		}
	}
	// And the release of schema 4 no longer opens it: the way back ended
	// with the first entry of schema 5.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := openFSMOf(root, entrySchema4)
	if err == nil {
		_ = f.close()
	}
	if !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("the release of schema 4, opening the state after schema 5 was used = %v, want %v", err, domain.ErrUnsupportedFormat)
	}

	// The fixture itself was not touched.
	if after := sumsOf(t, filepath.Join(fixture, "meta")); !reflect.DeepEqual(after, frozen) {
		t.Errorf("the metadata of the fixture changed while the test ran:\n%v\nwas\n%v", after, frozen)
	}
}
