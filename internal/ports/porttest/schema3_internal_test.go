package porttest

import (
	"context"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// TestMemMetadataDeletesWhatABucketHeld: a bucket that is deleted leaves no
// place behind for the records of who read its versions, nor for its
// sub-buckets - what the real store proves on its nested buckets. Through the
// port this cannot be seen: a bucket is only deleted when its versions are
// gone, and each took its record with it.
func TestMemMetadataDeletesWhatABucketHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemMetadata()
	for _, name := range []string{"b", "other"} {
		if err := m.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		stored, err := m.PutVersion(ctx, SampleVersion(name, "k", 1))
		if err != nil {
			t.Fatal(err)
		}
		batch := ports.ReadBatch{Node: "node-a", Boot: 1, Seq: uint64(len(m.access) + 1), Items: []ports.ObjectRead{readOf(stored.Stored, "user/a", false, 1, 0)}}
		if err := m.AddObjectReads(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if err := m.PutSubBucket(ctx, name, bucket.SubBucket{Name: "reports", CreatedAt: contractTime}, audit.Record{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.access["b"]) != 1 || len(m.access["other"]) != 1 {
		t.Fatalf("the reads were recorded as %v", m.access)
	}
	// The version goes and its record with it; the bucket's place stays
	// until the bucket goes.
	ref := object.VersionRef{Null: true}
	if _, err := m.DeleteObject(ctx, "b", "k", ports.DeleteOptions{Version: &ref}); err != nil {
		t.Fatal(err)
	}
	if records, held := m.access["b"]; !held || len(records) != 0 {
		t.Fatalf("after the version was removed the bucket holds %v (a place for records: %v), want the place and no record", records, held)
	}
	if err := m.DeleteBucket(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, held := m.access["b"]; held {
		t.Error("the deleted bucket left its place for records of reads behind")
	}
	if _, held := m.subs["b"]; held {
		t.Error("the deleted bucket left its sub-buckets behind")
	}
	if len(m.access["other"]) != 1 || len(m.subs["other"]) != 1 {
		t.Errorf("another bucket's records and sub-buckets after the deletion: %v, %v", m.access["other"], m.subs["other"])
	}
}
