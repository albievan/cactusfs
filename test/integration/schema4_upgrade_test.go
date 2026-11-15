//go:build integration && !windows

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// schema4Upload and schema4Download are the rows of a stored record of a
// bucket's latest use, as ADR-0019 writes them.
type schema4Upload struct {
	At  time.Time `json:"at"`
	By  string    `json:"by"`
	Key string    `json:"key"`
}

type schema4Download struct {
	At        time.Time `json:"at"`
	By        string    `json:"by"`
	Presigned bool      `json:"presigned"`
	Key       string    `json:"key"`
}

// schema4Activity is a stored record of a bucket's latest use.
type schema4Activity struct {
	V         int               `json:"v"`
	Uploads   []schema4Upload   `json:"uploads"`
	Downloads []schema4Download `json:"downloads"`
}

// schema4State is what a state database says of what schema 4 keeps.
type schema4State struct {
	// Schema is the schema the state says it has.
	Schema uint64
	// Creators is who created each bucket, as its record says; empty for
	// a bucket whose record does not say.
	Creators map[string]string
	// Kept says whether the state has a place for records of buckets' use
	// at all, and Activity holds the records, by bucket.
	Kept     bool
	Activity map[string]schema4Activity
}

// schema4StateOf reads, from the state database of a metadata directory that
// no store has open, what schema 4 keeps: straight from the file, so that
// what is asserted is what is stored and not what some read path makes of it.
func schema4StateOf(t *testing.T, metaDir string) schema4State {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(metaDir, "state.db"), 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	defer func() { _ = db.Close() }()
	state := schema4State{Creators: map[string]string{}, Activity: map[string]schema4Activity{}}
	if err := db.View(func(tx *bbolt.Tx) error {
		state.Schema = binary.BigEndian.Uint64(tx.Bucket([]byte("meta")).Get([]byte("schema")))
		if err := tx.Bucket([]byte("buckets")).ForEach(func(name, raw []byte) error {
			var record struct {
				CreatedBy string `json:"created_by"`
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			state.Creators[string(name)] = record.CreatedBy
			return nil
		}); err != nil {
			return err
		}
		records := tx.Bucket([]byte("bucketactivity"))
		if records == nil {
			return nil
		}
		state.Kept = true
		return records.ForEach(func(name, raw []byte) error {
			var record schema4Activity
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			state.Activity[string(name)] = record
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

// schema3Totals gives what a bucket of the schema-3 fixture holds as a usage
// record counts it (ADR-0023, "What is counted"): the current versions and
// their bytes, every version - a delete marker is one of no bytes - and its
// bytes, and the bytes of the parts of the uploads in progress.
func schema3Totals(b *schema3Bucket) bucket.UsageTotals {
	u := bucket.UsageTotals{Versions: int64(len(b.Objects) + len(b.Markers))}
	for _, o := range b.Objects {
		u.StoredBytes += o.Size
		if o.Latest {
			u.Objects++
			u.CurrentBytes += o.Size
		}
	}
	for _, upload := range b.Uploads {
		for _, p := range upload.Parts {
			u.UploadBytes += p.Size
		}
	}
	return u
}

// TestUpgradeFromSchema3ToSchema4: the schema-3 deployment that the binary
// of commit d2d6861 wrote is opened by this binary. Migration: none
// (ADR-0019, ADR-0023). The state opens as it is, and everything the
// manifest lists is there. Before any node of this tree opens it, it is the
// state of schema 3 the manifest says; once one has, it is not: a node that
// runs alone is at the level of this release from its first start, and its
// counter (app.UsageCounter) counts the usage of every bucket of the
// fixture by itself, with the walk of ADR-0023 - an entry of schema 5. So
// the state says 5 within seconds of the first start, by nobody's doing, and
// there the way back to the release that wrote it ends (ADR-0023,
// "Rollback"). The node answers the usage of every bucket as exact and of
// exactly what the manifest lists; the state holds their records, and still
// nothing else that schema 3 did not keep: no record of a bucket's latest
// use, no creator.
//
// The first upload gives the bucket that was uploaded to a record of its
// latest use, and no other has one; it moves that bucket's usage by exactly
// what it is. A bucket created from then on has its creator, and is counted
// from its creation; those of the fixture have no creator. All of it is there
// after a restart, beside everything the fixture held.
func TestUpgradeFromSchema3ToSchema4(t *testing.T) {
	// Not beside the others: this test starts one node as a process and is
	// done in seconds when it has the machine, and it ran out of its three
	// minutes when four-node clusters of containers were starting around it.
	// Built before the clock of the test starts, as in TestUpgradeFromSchema3.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	began := time.Now()
	// A copy: the fixture itself is never opened.
	copyTree(t, filepath.Join("testdata", schema3Dir), dir)
	meta := filepath.Join(dir, "meta")
	raw, err := fs.ReadFile(os.DirFS(dir), schema3Manifest)
	if err != nil {
		t.Fatalf("the manifest of the fixture: %v", err)
	}
	var contents schema3Contents
	if err := json.Unmarshal(raw, &contents); err != nil || len(contents.Buckets) < 2 || contents.Writer.SecretKey == "" {
		t.Fatalf("the manifest of the fixture: %d buckets, %v", len(contents.Buckets), err)
	}
	var names []string
	want := map[string]bucket.UsageTotals{}
	var sum bucket.UsageTotals
	for _, b := range contents.Buckets {
		names = append(names, b.Name)
		want[b.Name] = schema3Totals(b)
		sum = schema5Moved(sum, want[b.Name])
	}
	// Guards against proving less than is said: versions that are not
	// current, a delete marker, and parts of an upload in progress.
	if sum.Objects == 0 || sum.Versions <= sum.Objects || sum.StoredBytes <= sum.CurrentBytes || sum.UploadBytes == 0 {
		t.Fatalf("the buckets of the fixture together hold %+v: the test proves less than it says", sum)
	}
	// untouched says of a state that it is of the schema given, with the
	// buckets of the state the fixture's manifest names and nothing else of
	// what schema 4 keeps: no place for records of buckets' use, no creator.
	untouched := func(when string, schema uint64) {
		t.Helper()
		if got, buckets := stateOf(t, meta); got != schema || !slices.Equal(buckets, contents.StateBuckets) {
			t.Fatalf("%s: schema %d, buckets %v; want %d and %v", when, got, buckets, schema, contents.StateBuckets)
		}
		state := schema4StateOf(t, meta)
		if state.Schema != schema || state.Kept || len(state.Creators) != len(names) {
			t.Fatalf("%s: schema %d, a place for records of buckets' use: %t, %d buckets; want a state of schema %d without", when, state.Schema, state.Kept, len(state.Creators), schema)
		}
		for name, by := range state.Creators {
			if by != "" {
				t.Fatalf("%s: bucket %s is recorded as created by %q", when, name, by)
			}
		}
	}
	// counted says of the state, while no node has it open, that it holds
	// the usage records of exactly the buckets named, each exact, not being
	// counted, and of the totals wanted.
	counted := func(when string, of ...string) {
		t.Helper()
		state := schema5StateOf(t, meta)
		if got := slices.Sorted(maps.Keys(state.Usage)); !slices.Equal(got, slices.Sorted(slices.Values(of))) {
			t.Fatalf("%s: the state holds the usage records of %v; want those of %v", when, got, of)
		}
		for _, name := range of {
			usage, err := bucket.ParseUsage([]byte(state.Usage[name]))
			if err != nil {
				t.Fatalf("%s: the stored usage record of %s: %v (%s)", when, name, err, state.Usage[name])
			}
			if !usage.Exact || usage.Walking() || usage.UsageTotals != want[name] {
				t.Errorf("%s: %s holds exact = %v, being counted = %v, %+v; want %+v", when, name, usage.Exact, usage.Walking(), usage.UsageTotals, want[name])
			}
		}
	}
	// Before any node of this tree has opened it.
	untouched("the fixture", 3)
	if state := schema5StateOf(t, meta); len(state.Usage) != 0 {
		t.Fatalf("the fixture holds %d usage records", len(state.Usage))
	}

	// Opened and read by this binary: everything the manifest lists is
	// there - asked about without downloading, which is counted and handed
	// in as an entry when the node stops.
	node := schema3Server(ctx, t, bin, dir)
	for _, differs := range schema3Recorded(ctx, t, node, &contents) {
		t.Errorf("the record of the schema-3 deployment: %s", differs)
	}
	for _, b := range contents.Buckets {
		for _, o := range b.Objects {
			in := &s3.HeadObjectInput{Bucket: aws.String(b.Name), Key: aws.String(o.Key)}
			if o.VersionID != "" {
				in.VersionId = aws.String(o.VersionID)
			}
			head, err := node.root.HeadObject(ctx, in)
			if err != nil {
				t.Errorf("%s/%s %s of the schema-3 deployment: %v", b.Name, o.Key, o.VersionID, err)
				continue
			}
			if size := aws.ToInt64(head.ContentLength); size != o.Size {
				t.Errorf("%s/%s %s of the schema-3 deployment: %d bytes, want %d", b.Name, o.Key, o.VersionID, size, o.Size)
			}
		}
		listed, err := node.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(b.Name)})
		if err != nil || len(listed.Versions) != len(b.Objects) || len(listed.DeleteMarkers) != len(b.Markers) {
			t.Fatalf("versions of %s: %d and %d markers, %v; want %d and %d", b.Name, len(listed.Versions), len(listed.DeleteMarkers), err, len(b.Objects), len(b.Markers))
		}
		if b.Policy != "" {
			policy, err := node.root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(b.Name)})
			if err != nil || aws.ToString(policy.Policy) != b.Policy {
				t.Errorf("the policy of %s = %s, %v", b.Name, aws.ToString(policy.Policy), err)
			}
		}
	}
	// Nobody asked for anything to be counted: the node counts every
	// bucket of the fixture by itself, to exactly what the manifest lists.
	schema5Counted(ctx, t, node, want, "counted by the node of its own accord", names...)
	node.srv.stop(t)
	// The walk is of schema 5: the state is, holds the records of the
	// buckets that were there, and nothing else schema 4 would keep.
	untouched("after this binary opened and read the deployment", 5)
	counted("after this binary opened and read the deployment", names...)

	// The first upload, with the key of the fixture's user, and a download
	// of it. The node hands the download in when it stops.
	first := contents.Buckets[0]
	if first.Versions {
		t.Fatalf("the first bucket of the manifest, %s, keeps versions: the download below would need a version ID", first.Name)
	}
	added := &schema3Object{Key: "after/schema-four.txt", Latest: true, UploadedBy: contents.Writer.Actor}
	content := []byte("written after the schema-3 deployment was opened by the binary of schema 4")
	added.Size, added.SHA256 = int64(len(content)), sumOf(content)
	node = schema3Server(ctx, t, bin, dir)
	writer := node.client(contents.Writer.AccessKeyID, contents.Writer.SecretKey)
	if _, err := writer.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(first.Name), Key: aws.String(added.Key), Body: strings.NewReader(string(content))}); err != nil {
		t.Fatalf("PutObject with the key of the fixture's user: %v", err)
	}
	if got, err := schema3Get(ctx, writer, first.Name, added); err != nil || sumOf(got) != added.SHA256 {
		t.Fatalf("GetObject with the key of the fixture's user: %d bytes, %v", len(got), err)
	}
	// Where versions are not kept, a put of a new key adds an object.
	want[first.Name] = schema5Moved(want[first.Name], bucket.UsageTotals{Objects: 1, CurrentBytes: added.Size, Versions: 1, StoredBytes: added.Size})
	schema5Counted(ctx, t, node, want, "after the first upload", first.Name)
	node.srv.stop(t)

	// used says of a state that it is of the schema given and holds, of the
	// bucket that was uploaded to, the upload - and the download, while it is
	// among the latest three - and returns the state.
	used := func(when string, schema uint64, downloaded bool) schema4State {
		t.Helper()
		state := schema4StateOf(t, meta)
		if state.Schema != schema || !state.Kept {
			t.Fatalf("%s: schema %d, a place for records of buckets' use: %t; want a state of schema %d with one", when, state.Schema, state.Kept, schema)
		}
		record := state.Activity[first.Name]
		if record.V != 1 || len(record.Uploads) != 1 || record.Uploads[0].Key != added.Key || record.Uploads[0].By != contents.Writer.Actor {
			t.Fatalf("%s: the uploads of %s = %+v; want the one upload, by %s", when, first.Name, record, contents.Writer.Actor)
		}
		if at := record.Uploads[0].At; at.Before(began.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
			t.Errorf("%s: the upload is recorded at %s, which is not when it was made", when, at)
		}
		if downloaded && !slices.ContainsFunc(record.Downloads, func(d schema4Download) bool {
			return d.Key == added.Key && d.By == contents.Writer.Actor && !d.Presigned && !d.At.Before(record.Uploads[0].At)
		}) {
			t.Fatalf("%s: the downloads of %s = %+v; want the one by %s", when, first.Name, record.Downloads, contents.Writer.Actor)
		}
		// The buckets of the fixture were created before anybody was
		// recorded as their creator, and stay so.
		for _, name := range names {
			if by := state.Creators[name]; by != "" {
				t.Errorf("%s: bucket %s of the fixture is recorded as created by %q", when, name, by)
			}
		}
		return state
	}
	// The record of the upload is of schema 4; the state is of schema 5
	// already, by the walk, and stays so.
	state := used("after the first upload", 5, true)
	if len(state.Activity) != 1 {
		t.Errorf("after the first upload there are records of %d buckets, want only that of %s: %+v", len(state.Activity), first.Name, state.Activity)
	}
	// Everything schema 3 kept is still what the state holds.
	if _, buckets := stateOf(t, meta); !slices.Equal(buckets, contents.StateBuckets) {
		t.Errorf("after the first upload the state holds %v of what schema 3 keeps, want %v", buckets, contents.StateBuckets)
	}

	counted("after the first upload", names...)

	// Who creates a bucket is recorded from now on. The use cases and the
	// management API say it in a later step; here the metadata store of this
	// tree is asked directly, on the deployment's own state. The bucket is
	// counted from its creation: it holds nothing.
	const made = "made-at-schema-four"
	store, err := raftmeta.Open(ctx, raftmeta.Config{Dir: meta, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader})
	if err != nil {
		t.Fatalf("opening the metadata of the deployment: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if b, err := store.GetBucket(ctx, first.Name); err != nil || b.CreatedBy != "" {
		t.Errorf("a bucket of the fixture is created by %q, %v; want nobody recorded", b.CreatedBy, err)
	}
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: made, CreatedAt: time.Now(), CreatedBy: contents.Writer.Actor}); err != nil {
		t.Fatalf("a bucket created over the deployment: %v", err)
	}
	if b, err := store.GetBucket(ctx, made); err != nil || b.CreatedBy != contents.Writer.Actor {
		t.Errorf("the bucket created over the deployment is created by %q, %v; want %s", b.CreatedBy, err, contents.Writer.Actor)
	}
	activities, err := store.BucketActivities(ctx, append(slices.Clone(names), made))
	if err != nil || len(activities) != len(names)+1 {
		t.Fatalf("BucketActivities over the deployment = %+v, %v", activities, err)
	}
	for i, a := range activities {
		switch {
		case i == 0 && (len(a.Uploads) != 1 || a.Uploads[0].Key != added.Key || a.Uploads[0].By != contents.Writer.Actor ||
			!a.Uploads[0].At.Equal(state.Activity[first.Name].Uploads[0].At) || len(a.Downloads) != 1):
			t.Errorf("the record of %s as the store reads it = %+v", first.Name, a)
		case i > 0 && !a.Empty():
			t.Errorf("the store reads a record of a bucket that was not used: %+v", a)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After a restart: the binary opens the state of schema 4, every
	// version of the manifest is the bytes it was, and the new bucket is
	// among the others.
	node = schema3Server(ctx, t, bin, dir)
	for _, b := range contents.Buckets {
		for _, o := range b.Objects {
			if got, err := schema3Get(ctx, node.root, b.Name, o); err != nil || int64(len(got)) != o.Size || sumOf(got) != o.SHA256 {
				t.Errorf("%s/%s %s after the state became schema 4: %d bytes, %v; want %d", b.Name, o.Key, o.VersionID, len(got), err, o.Size)
			}
		}
	}
	if got, err := schema3Get(ctx, node.root, first.Name, added); err != nil || sumOf(got) != added.SHA256 {
		t.Errorf("the object uploaded by this binary, after a restart: %d bytes, %v", len(got), err)
	}
	listed, err := node.root.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(listed.Buckets) != len(names)+1 {
		t.Fatalf("ListBuckets after a restart = %d buckets, %v; want %d", len(listed.Buckets), err, len(names)+1)
	}
	want[made] = bucket.UsageTotals{}
	all := append(slices.Clone(names), made)
	schema5Counted(ctx, t, node, want, "after a restart", all...)
	node.srv.stop(t)

	// (Root has read more of the first bucket since than a record names:
	// the user's download is no longer among the latest three.) The state is
	// of schema 5 since the node's first walk (ADR-0023); what becomes of a
	// deployment of schema 4 is TestUpgradeFromSchema4. Every bucket has
	// its record, of exactly what it holds.
	state = used("after a restart", 5, false)
	counted("after a restart", all...)
	if record := state.Activity[first.Name]; len(record.Downloads) != bucket.MaxRecentDownloads || record.Downloads[0].By != schema3Root {
		t.Errorf("after a restart the downloads of %s = %+v; want the latest %d, by %s", first.Name, record.Downloads, bucket.MaxRecentDownloads, schema3Root)
	}
	if by := state.Creators[made]; by != contents.Writer.Actor {
		t.Errorf("after a restart the bucket created at schema 4 is recorded as created by %q, want %s", by, contents.Writer.Actor)
	}
	// What root read after the restart was handed in when the node stopped:
	// downloads from the buckets of the fixture, and still no upload into
	// any but the first.
	for _, b := range contents.Buckets[1:] {
		record := state.Activity[b.Name]
		if len(record.Uploads) != 0 || len(record.Downloads) == 0 || record.Downloads[0].By != schema3Root {
			t.Errorf("after a restart the record of %s = %+v; want downloads by %s and no upload", b.Name, record, schema3Root)
		}
	}
	if record, ok := state.Activity[made]; ok {
		t.Errorf("a bucket nobody used has the record %+v", record)
	}
	if out := node.srv.stderr.String(); strings.Contains(out, fixtureSecretKey) || strings.Contains(out, contents.Writer.SecretKey) {
		t.Fatal("a secret was written to standard error")
	}
}
