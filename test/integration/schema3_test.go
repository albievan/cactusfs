//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
)

// stateOf reads, from the state database of a metadata directory that no
// store has open, the schema it says it has and which of the buckets that
// exist from schema 3 on it holds.
func stateOf(t *testing.T, metaDir string) (schema uint64, buckets []string) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(metaDir, "state.db"), 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.View(func(tx *bbolt.Tx) error {
		schema = binary.BigEndian.Uint64(tx.Bucket([]byte("meta")).Get([]byte("schema")))
		for _, name := range []string{"access", "accessflush", "subbuckets", "tagdefs"} {
			if tx.Bucket([]byte(name)) != nil {
				buckets = append(buckets, name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return schema, buckets
}

// TestUpgradeFromSchema2ToSchema3: the metadata of the schema-2 fixture is
// opened by this tree's metadata store. Migration: none (ADR-0016) - the
// state opens as it is, and stays a state the release that wrote it still
// opens for as long as nothing of schema 3 is written, whatever else is.
// Then a sub-bucket, tag rules and a tag definition are written over it, and
// the state says schema 3. A record of reads follows: a node on its own is at
// the level of this release, where a batch of reads is written as an entry of
// schema 4 (ADR-0019), so from then on the state says 4. All of it is there
// after a restart, beside everything the fixture held.
func TestUpgradeFromSchema2ToSchema3(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "schema-2", "meta"), dir)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	open := func() *raftmeta.Store {
		t.Helper()
		store, err := raftmeta.Open(ctx, raftmeta.Config{Dir: dir, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader})
		if err != nil {
			t.Fatalf("opening the schema-2 metadata: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	shut := func(store *raftmeta.Store) {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if schema, buckets := stateOf(t, dir); schema != 2 || len(buckets) != 0 {
		t.Fatalf("the fixture is not a state of schema 2: schema %d, buckets %v", schema, buckets)
	}

	// Opened, read, and written with what schema 2 has: still schema 2.
	store := open()
	policy, err := store.GetBucketConfig(ctx, schema2Bucket, bucket.ConfigPolicy)
	if err != nil || string(policy) != schema2Policy {
		t.Fatalf("the bucket policy of the fixture = %s, %v", policy, err)
	}
	summary, err := store.GetObject(ctx, schema2Bucket, "reports/summary.txt")
	if err != nil || summary.CreatedBy != "" {
		t.Fatalf("an object of the fixture: made by %q, %v; want nobody recorded", summary.CreatedBy, err)
	}
	if access, err := store.ObjectAccess(ctx, schema2Bucket, summary.Key, summary.Ref()); err != nil || !access.Empty() {
		t.Errorf("the record of reads of an object of the fixture = %+v, %v", access, err)
	}
	if subs, err := store.ListSubBuckets(ctx, schema2Bucket); err != nil || len(subs) != 0 {
		t.Errorf("sub-buckets of the fixture's bucket = %v, %v", subs, err)
	}
	if definitions, err := store.ListTagDefinitions(ctx); err != nil || len(definitions) != 0 {
		t.Errorf("the catalogue of the fixture = %v, %v", definitions, err)
	}
	if err := store.SetBucketConfig(ctx, schema2Bucket, bucket.ConfigTagging, []byte(`{"Team":"Storage"}`)); err != nil {
		t.Fatalf("a write of schema 2 over the fixture: %v", err)
	}
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "made-by-this-release", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("a bucket made over the fixture: %v", err)
	}
	if err := store.DeleteBucket(ctx, "made-by-this-release"); err != nil {
		t.Fatalf("a bucket deleted over the fixture: %v", err)
	}
	shut(store)
	if schema, buckets := stateOf(t, dir); schema != 2 || len(buckets) != 0 {
		t.Fatalf("after opening and writing what schema 2 has: schema %d, buckets %v; the release before could not go back to it", schema, buckets)
	}

	// Now what only schema 3 has. The fixture's keys under reports/ are
	// adopted by a sub-bucket of that name.
	store = open()
	created := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	record := audit.Record{Time: created, Actor: "root", Action: "s3:PutSubBucket", Resource: schema2Bucket + "/reports", Outcome: audit.Succeeded}
	if err := store.PutSubBucket(ctx, schema2Bucket, bucket.SubBucket{Name: "reports", CreatedAt: created, Policy: []byte(`{"Statement":[]}`)}, record); err != nil {
		t.Fatalf("a sub-bucket over the fixture: %v", err)
	}
	rules, err := bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{{SubBucket: "reports", Key: "stage"}}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketConfig(ctx, schema2Bucket, bucket.ConfigTagRules, rules); err != nil {
		t.Fatalf("tag rules over the fixture: %v", err)
	}
	if err := store.PutTagDefinition(ctx, tagging.Definition{Key: "stage", Values: []string{"draft", "final"}, CreatedAt: created}, audit.Record{}); err != nil {
		t.Fatalf("a tag definition over the fixture: %v", err)
	}
	// So far the state is of schema 3, and holds what was written of it.
	shut(store)
	if schema, buckets := stateOf(t, dir); schema != 3 || !slices.Equal(buckets, []string{"subbuckets", "tagdefs"}) {
		t.Fatalf("after a sub-bucket, tag rules and a definition: schema %d, buckets %v; want 3 and those two", schema, buckets)
	}
	store = open()
	batch := ports.ReadBatch{Node: store.NodeID(), Boot: 1, Seq: 1, Items: []ports.ObjectRead{
		{Bucket: schema2Bucket, Key: summary.Key, Version: summary.Ref(), VersionID: summary.VersionID, Read: object.Read{Who: "user/albie", Downloads: 2}},
	}}
	for range 2 {
		if err := store.AddObjectReads(ctx, batch); err != nil {
			t.Fatalf("reads of an object of the fixture: %v", err)
		}
	}
	check := func(store *raftmeta.Store) {
		t.Helper()
		access, err := store.ObjectAccess(ctx, schema2Bucket, summary.Key, summary.Ref())
		if err != nil || access.Downloads != 2 || len(access.Readers) != 1 || access.Readers[0].Who != "user/albie" || access.LastAt.IsZero() {
			t.Errorf("the record of reads = %+v, %v; want two downloads by one reader, the batch applied once", access, err)
		}
		sub, err := store.GetSubBucket(ctx, schema2Bucket, "reports")
		if err != nil || string(sub.Policy) != `{"Statement":[]}` || !sub.CreatedAt.Equal(created) {
			t.Errorf("the sub-bucket = %+v, %v", sub, err)
		}
		// It holds the keys the fixture had under its name.
		if err := store.DeleteSubBucket(ctx, schema2Bucket, "reports", audit.Record{}); !errors.Is(err, domain.ErrSubBucketNotEmpty) {
			t.Errorf("deleting the sub-bucket over the fixture's keys = %v, want %v", err, domain.ErrSubBucketNotEmpty)
		}
		if got, err := store.GetBucketConfig(ctx, schema2Bucket, bucket.ConfigTagRules); err != nil || !bytes.Equal(got, rules) {
			t.Errorf("the tag rules = %s, %v", got, err)
		}
		if definition, err := store.GetTagDefinition(ctx, "stage"); err != nil || len(definition.Values) != 2 {
			t.Errorf("the tag definition = %+v, %v", definition, err)
		}
		if err := store.DeleteTagDefinition(ctx, "stage", audit.Record{}); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("deleting a definition the rules name = %v, want %v", err, domain.ErrConflict)
		}
		// And what the fixture held is as it was.
		if got, err := store.GetBucketConfig(ctx, schema2Bucket, bucket.ConfigPolicy); err != nil || string(got) != schema2Policy {
			t.Errorf("the bucket policy of the fixture = %s, %v", got, err)
		}
		if got, err := store.GetObject(ctx, schema2Bucket, "reports/summary.txt"); err != nil || got.DataID != summary.DataID {
			t.Errorf("an object of the fixture after schema 3 was written: %v", err)
		}
		trail, err := store.ListAudit(ctx, 0, 1000)
		if err != nil || len(trail) == 0 || trail[len(trail)-1].Action != "s3:PutSubBucket" {
			t.Errorf("the audit trail does not end with the sub-bucket's record: %d records, %v", len(trail), err)
		}
	}
	check(store)
	shut(store)
	schema, buckets := stateOf(t, dir)
	if schema != 4 || len(buckets) != 4 {
		t.Errorf("after the reads were written: schema %d, buckets %v; want 4 and all four", schema, buckets)
	}
	// The log is replayed over the state at the next start, and changes
	// nothing in it.
	check(open())
}
