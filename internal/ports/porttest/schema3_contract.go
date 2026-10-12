package porttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
)

// Schema3 is everything the contract of entry schema 3 exercises: the
// metadata ports, what ADR-0016, ADR-0017 and ADR-0018 added to them, and
// the audit trail that changes to sub-buckets and to the catalogue write to.
type Schema3 interface {
	Metadata
	ports.ObjectAccessStore
	ports.SubBucketStore
	ports.TagDefinitionStore
	ports.AuditLog
}

// readOf is one item of a batch of reads.
func readOf(v object.Version, who string, presigned bool, downloads, copies int64) ports.ObjectRead {
	return ports.ObjectRead{
		Bucket: v.Bucket, Key: v.Key, Version: v.Ref(), VersionID: v.VersionID,
		Read: object.Read{Who: who, Presigned: presigned, Downloads: downloads, Copies: copies},
	}
}

// Schema3Contract is the behaviour every store of who made and who read a
// version, of sub-buckets, of tag rules and of the catalogue of tags must
// have. open returns a new, empty store in which all of it can be written:
// every member of its cluster knows entry schema 3.
func Schema3Contract(t *testing.T, open func(t *testing.T) Schema3) {
	t.Helper()
	ctx := context.Background()
	noAudit := audit.Record{}

	withBucket := func(t *testing.T, names ...string) Schema3 {
		t.Helper()
		s := open(t)
		for _, name := range names {
			if err := s.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: contractTime}); err != nil {
				t.Fatalf("CreateBucket %s: %v", name, err)
			}
		}
		return s
	}
	put := func(t *testing.T, s Schema3, bucketName, key string, fill byte) object.Version {
		t.Helper()
		result, err := s.PutVersion(ctx, SampleVersion(bucketName, key, fill))
		if err != nil {
			t.Fatalf("PutVersion %s/%s: %v", bucketName, key, err)
		}
		return result.Stored
	}
	access := func(t *testing.T, s Schema3, v object.Version) object.Access {
		t.Helper()
		got, err := s.ObjectAccess(ctx, v.Bucket, v.Key, v.Ref())
		if err != nil {
			t.Fatalf("ObjectAccess %s/%s %s: %v", v.Bucket, v.Key, v.Ref(), err)
		}
		return got
	}
	add := func(t *testing.T, s Schema3, node string, boot, seq uint64, items ...ports.ObjectRead) {
		t.Helper()
		if err := s.AddObjectReads(ctx, ports.ReadBatch{Node: node, Boot: boot, Seq: seq, Items: items}); err != nil {
			t.Fatalf("AddObjectReads %s %d/%d: %v", node, boot, seq, err)
		}
	}
	trail := func(t *testing.T, s Schema3) []audit.Record {
		t.Helper()
		records, err := s.ListAudit(ctx, 0, 1000)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		return records
	}

	t.Run("who made a version is recorded", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")

		v := SampleVersion("b", "by-put", 1)
		v.CreatedBy = "user/albie"
		result, err := s.PutVersion(ctx, v)
		if err != nil || result.Stored.CreatedBy != "user/albie" {
			t.Fatalf("PutVersion = %+v, %v; want it to say who made the version", result.Stored.CreatedBy, err)
		}
		if got, err := s.GetObject(ctx, "b", "by-put"); err != nil || got.CreatedBy != "user/albie" {
			t.Errorf("GetObject: made by %q, %v", got.CreatedBy, err)
		}
		// A version put without it has none, and says so.
		if got := put(t, s, "b", "by-nobody", 2); got.CreatedBy != "" {
			t.Errorf("a put that names nobody is recorded as made by %q", got.CreatedBy)
		}

		// A completed upload is made by whoever completes it, not by
		// whoever began it.
		u := SampleUpload("b", "by-upload", 3)
		if err := s.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		part := SamplePart(u, 1, 10, 0)
		if _, err := s.PutPart(ctx, "b", u.Key, u.ID, part); err != nil {
			t.Fatal(err)
		}
		completion := completionOf(u, 4, part)
		completion.CreatedBy = "key/AKIDEXAMPLE"
		done, err := s.CompleteUpload(ctx, completion)
		if err != nil || done.Stored.CreatedBy != "key/AKIDEXAMPLE" {
			t.Fatalf("CompleteUpload: made by %q, %v", done.Stored.CreatedBy, err)
		}
		if got, err := s.GetObject(ctx, "b", "by-upload"); err != nil || got.CreatedBy != "key/AKIDEXAMPLE" {
			t.Errorf("GetObject of the completed upload: made by %q, %v", got.CreatedBy, err)
		}
	})

	t.Run("an upload gives its tags to the version", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")

		u := SampleUpload("b", "tagged", 1)
		u.Tags = map[string]string{"stage": "draft", "team": "ops"}
		if err := s.CreateUpload(ctx, u); err != nil {
			t.Fatalf("CreateUpload with tags: %v", err)
		}
		if got, err := s.GetUpload(ctx, "b", u.Key, u.ID); err != nil || !reflect.DeepEqual(got.Tags, u.Tags) {
			t.Errorf("GetUpload: tags %v, %v; want %v", got.Tags, err, u.Tags)
		}
		part := SamplePart(u, 1, 10, 0)
		if _, err := s.PutPart(ctx, "b", u.Key, u.ID, part); err != nil {
			t.Fatal(err)
		}
		done, err := s.CompleteUpload(ctx, completionOf(u, 2, part))
		if err != nil || !reflect.DeepEqual(done.Stored.Tags, u.Tags) {
			t.Fatalf("CompleteUpload: tags %v, %v; want %v", done.Stored.Tags, err, u.Tags)
		}
		if got, err := s.GetObject(ctx, "b", "tagged"); err != nil || !reflect.DeepEqual(got.Tags, u.Tags) {
			t.Errorf("GetObject: tags %v, %v", got.Tags, err)
		}

		// An upload without tags makes a version without.
		plain := SampleUpload("b", "plain", 3)
		if err := s.CreateUpload(ctx, plain); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetUpload(ctx, "b", plain.Key, plain.ID); err != nil || len(got.Tags) != 0 {
			t.Errorf("GetUpload of an upload without tags: %v, %v", got.Tags, err)
		}
		// Tags the version could not have are refused when the upload
		// begins, and nothing is left behind.
		many := SampleUpload("b", "many", 4)
		many.Tags = map[string]string{}
		for i := range object.MaxObjectTags + 1 {
			many.Tags["k"+strconv.Itoa(i)] = "v"
		}
		// (As an invalid argument: the store's refusal may come from
		// another node, and which argument it was does not travel. Whoever
		// wants to say "invalid tag" validates before asking the store.)
		if err := s.CreateUpload(ctx, many); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("CreateUpload with %d tags = %v, want %v", len(many.Tags), err, domain.ErrInvalidArgument)
		}
		if _, err := s.GetUpload(ctx, "b", many.Key, many.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("the refused upload: %v, want %v", err, domain.ErrUploadNotFound)
		}
	})

	t.Run("tag rules are a configuration of the bucket", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")

		rules := bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{{Key: "stage", Values: []string{"draft", "final"}}}}
		document, err := rules.Document()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBucketConfig(ctx, "b", bucket.ConfigTagRules); err != nil || got != nil {
			t.Errorf("tag rules of a bucket that has none = %q, %v", got, err)
		}
		if err := s.SetBucketConfig(ctx, "b", bucket.ConfigTagRules, document); err != nil {
			t.Fatalf("SetBucketConfig of tag rules: %v", err)
		}
		got, err := s.GetBucketConfig(ctx, "b", bucket.ConfigTagRules)
		if err != nil || !bytes.Equal(got, document) {
			t.Fatalf("GetBucketConfig = %q, %v; want %q", got, err, document)
		}
		if parsed, err := bucket.ParseTagRules(got); err != nil || !reflect.DeepEqual(parsed, rules) {
			t.Errorf("the stored rules read as %+v, %v", parsed, err)
		}
		// The bucket's other configuration is untouched by it.
		if err := s.SetBucketConfig(ctx, "b", bucket.ConfigPolicy, []byte(`{"p":1}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketConfig(ctx, "b", bucket.ConfigTagRules, nil); err != nil {
			t.Fatalf("removing the tag rules: %v", err)
		}
		if got, err := s.GetBucketConfig(ctx, "b", bucket.ConfigTagRules); err != nil || got != nil {
			t.Errorf("tag rules after their removal = %q, %v", got, err)
		}
		if got, err := s.GetBucketConfig(ctx, "b", bucket.ConfigPolicy); err != nil || string(got) != `{"p":1}` {
			t.Errorf("the policy after the tag rules were removed = %q, %v", got, err)
		}
		if err := s.SetBucketConfig(ctx, "b", bucket.ConfigTagRules, make([]byte, bucket.MaxConfigBytes+1)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("tag rules larger than a configuration can be = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if err := s.SetBucketConfig(ctx, "missing", bucket.ConfigTagRules, document); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("tag rules of a bucket that is not there = %v, want %v", err, domain.ErrBucketNotFound)
		}
	})

	t.Run("reads are added to the record of their version", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		v := put(t, s, "b", "k", 1)
		before := time.Now().Add(-time.Minute)

		if got := access(t, s, v); !got.Empty() || len(got.Readers) != 0 || !got.LastAt.IsZero() {
			t.Fatalf("the record of a version nobody read = %+v", got)
		}
		add(t, s, "node-a", 1, 1,
			readOf(v, "user/albie", false, 2, 0),
			readOf(v, "key/AKID", true, 1, 0),
			readOf(v, "anonymous", false, 0, 3),
			readOf(v, "user/albie", false, 1, 1),
		)
		got := access(t, s, v)
		if got.Downloads != 4 || got.Copies != 4 || got.OtherReaders != 0 {
			t.Fatalf("totals = %d downloads, %d copies, %d others; want 4, 4, 0", got.Downloads, got.Copies, got.OtherReaders)
		}
		if got.LastAt.Before(before) || got.LastAt.After(time.Now().Add(time.Minute)) {
			t.Errorf("last read at %v, want the store's time of the batch", got.LastAt)
		}
		type row struct {
			who       string
			presigned bool
			downloads int64
			copies    int64
		}
		var rows []row
		for _, r := range got.Readers {
			rows = append(rows, row{r.Who, r.Presigned, r.Downloads, r.Copies})
			if !r.LastAt.Equal(got.LastAt) {
				t.Errorf("reader %s last read at %v, the record at %v: one batch has one time", r.Who, r.LastAt, got.LastAt)
			}
		}
		want := []row{{"anonymous", false, 0, 3}, {"key/AKID", true, 1, 0}, {"user/albie", false, 3, 1}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("readers = %+v, want %+v", rows, want)
		}

		// A second batch, of another node, adds to the same record.
		add(t, s, "node-b", 1, 1, readOf(v, "key/AKID", true, 5, 0), readOf(v, "key/AKID", false, 1, 0))
		got = access(t, s, v)
		if got.Downloads != 10 || got.Copies != 4 || len(got.Readers) != 4 {
			t.Errorf("after a second batch: %d downloads, %d copies, %d readers; want 10, 4, 4", got.Downloads, got.Copies, len(got.Readers))
		}
		// The signer's own reads and those made with URLs it signed are
		// kept apart, the signer's own first.
		if len(got.Readers) == 4 && (got.Readers[1].Who != "key/AKID" || got.Readers[1].Presigned || got.Readers[1].Downloads != 1 ||
			got.Readers[2].Who != "key/AKID" || !got.Readers[2].Presigned || got.Readers[2].Downloads != 6) {
			t.Errorf("readers = %+v", got.Readers)
		}
	})

	t.Run("an item that cannot be recorded is left out and the rest recorded", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b", "ver")
		if err := s.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		v := put(t, s, "b", "k", 1)
		kept := put(t, s, "ver", "k", 2)
		deleted, err := s.DeleteObject(ctx, "ver", "k", ports.DeleteOptions{MarkerID: sampleVersionID(9), At: contractTime})
		if err != nil || deleted.Marker == nil {
			t.Fatalf("DeleteObject = %+v, %v", deleted, err)
		}

		gone := SampleVersion("b", "never-put", 7)
		// A read that names the version and has the ID of none, or of
		// another.
		withID := func(v object.Version, id object.ID) ports.ObjectRead {
			read := readOf(v, "user/x", false, 1, 0)
			read.VersionID = id
			return read
		}
		byID := readOf(v, "user/x", false, 1, 0)
		// The version of an object in a bucket that keeps none is "null":
		// its ID names nothing.
		byID.Version = object.VersionRef{ID: v.VersionID}
		add(t, s, "node-a", 1, 1,
			readOf(gone, "user/x", false, 1, 0),
			readOf(SampleVersion("no-such-bucket", "k", 1), "user/x", false, 1, 0),
			byID,
			readOf(*deleted.Marker, "user/x", false, 1, 0),
			readOf(v, "", false, 1, 0),
			readOf(v, "user/x", false, 0, 0),
			readOf(v, "user/x", false, -5, 0),
			readOf(v, "user/x", false, object.MaxReadCount+1, 0),
			withID(v, object.ID{}),
			withID(v, sampleVersionID(77)),
			readOf(v, "user/x", false, 1, 0),
			readOf(kept, "user/y", false, 0, 2),
		)
		if got := access(t, s, v); got.Downloads != 1 || got.Copies != 0 || len(got.Readers) != 1 || got.Readers[0].Who != "user/x" {
			t.Errorf("the version read once among items that are none = %+v", got)
		}
		// A version that is no longer current is still a version.
		if got := access(t, s, kept); got.Copies != 2 || len(got.Readers) != 1 {
			t.Errorf("the version under a delete marker = %+v", got)
		}
		if got := access(t, s, *deleted.Marker); !got.Empty() {
			t.Errorf("a delete marker was read: %+v", got)
		}
		if _, err := s.ObjectAccess(ctx, "b", "never-put", object.VersionRef{Null: true}); !errors.Is(err, domain.ErrVersionNotFound) {
			t.Errorf("ObjectAccess of a version that is not there = %v, want %v", err, domain.ErrVersionNotFound)
		}
		if _, err := s.ObjectAccess(ctx, "no-such-bucket", "k", object.VersionRef{Null: true}); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ObjectAccess in a bucket that is not there = %v, want %v", err, domain.ErrBucketNotFound)
		}
	})

	t.Run("a batch that is sent again is applied once", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		v := put(t, s, "b", "k", 1)
		one := readOf(v, "user/albie", false, 1, 0)

		steps := []struct {
			name      string
			node      string
			boot, seq uint64
			want      int64
		}{
			{"the first batch of a node", "node-a", 7, 1, 1},
			{"the same batch again", "node-a", 7, 1, 1},
			{"and a third time", "node-a", 7, 1, 1},
			{"the next batch", "node-a", 7, 2, 2},
			{"the one before it, late", "node-a", 7, 1, 2},
			{"the next but one", "node-a", 7, 4, 3},
			{"one that was skipped, late", "node-a", 7, 3, 3},
			{"the same numbers from another node", "node-b", 7, 1, 4},
			{"the node after a restart, counting from one", "node-a", 8, 1, 5},
			{"that batch again", "node-a", 8, 1, 5},
			{"another node is not disturbed by it", "node-b", 7, 1, 5},
			{"and goes on", "node-b", 7, 2, 6},
		}
		for _, step := range steps {
			add(t, s, step.node, step.boot, step.seq, one)
			if got := access(t, s, v).Downloads; got != step.want {
				t.Fatalf("%s (%s %d/%d): %d downloads, want %d", step.name, step.node, step.boot, step.seq, got, step.want)
			}
		}
	})

	t.Run("a batch that cannot be one is refused", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		v := put(t, s, "b", "k", 1)

		items := make([]ports.ObjectRead, ports.MaxReadBatch+1)
		for i := range items {
			items[i] = readOf(v, "user/"+strconv.Itoa(i), false, 1, 0)
		}
		if err := s.AddObjectReads(ctx, ports.ReadBatch{Node: "node-a", Boot: 1, Seq: 1, Items: items}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a batch of %d items = %v, want %v", len(items), err, domain.ErrInvalidArgument)
		}
		if err := s.AddObjectReads(ctx, ports.ReadBatch{Boot: 1, Seq: 1, Items: items[:1]}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a batch of no node = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if got := access(t, s, v); !got.Empty() {
			t.Fatalf("a refused batch was counted: %+v", got)
		}
		// The largest batch there can be is taken whole - under the
		// numbers of the batch that was refused, which were not used up.
		// More readers than a record names are folded into "others", and
		// no read is lost to that.
		add(t, s, "node-a", 1, 1, items[:ports.MaxReadBatch]...)
		got := access(t, s, v)
		if got.Downloads != ports.MaxReadBatch || len(got.Readers) != object.MaxReaders ||
			got.OtherReaders != ports.MaxReadBatch-object.MaxReaders || got.OtherDownloads != ports.MaxReadBatch-object.MaxReaders {
			t.Errorf("after %d readers: %d downloads, %d readers by name, %d others with %d downloads",
				ports.MaxReadBatch, got.Downloads, len(got.Readers), got.OtherReaders, got.OtherDownloads)
		}
	})

	t.Run("the record of a version goes with the version", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "plain", "ver")
		if err := s.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}

		// In a bucket that keeps no versions an overwrite starts at zero.
		first := put(t, s, "plain", "k", 1)
		add(t, s, "node-a", 1, 1, readOf(first, "user/a", false, 3, 0))
		second := put(t, s, "plain", "k", 2)
		if got := access(t, s, second); !got.Empty() {
			t.Errorf("after an overwrite the new version has the reads of the old: %+v", got)
		}
		// And so does an object that was deleted and put again.
		add(t, s, "node-a", 1, 2, readOf(second, "user/a", false, 3, 0))
		if _, err := s.DeleteObject(ctx, "plain", "k", ports.DeleteOptions{MarkerID: sampleVersionID(8), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ObjectAccess(ctx, "plain", "k", second.Ref()); !errors.Is(err, domain.ErrVersionNotFound) {
			t.Errorf("ObjectAccess of a deleted object = %v, want %v", err, domain.ErrVersionNotFound)
		}
		third := put(t, s, "plain", "k", 3)
		if got := access(t, s, third); !got.Empty() {
			t.Errorf("an object put again after its deletion has the reads from before: %+v", got)
		}

		// Where versions are kept each has its own record. Removing one
		// for good removes its record and no other.
		old := put(t, s, "ver", "k", 4)
		current := put(t, s, "ver", "k", 5)
		add(t, s, "node-a", 1, 3, readOf(old, "user/a", false, 1, 0), readOf(current, "user/b", false, 2, 0))
		if got := access(t, s, old); got.Downloads != 1 {
			t.Fatalf("the older version: %+v", got)
		}
		ref := old.Ref()
		if removed, err := s.DeleteObject(ctx, "ver", "k", ports.DeleteOptions{Version: &ref}); err != nil || removed.Removed == nil {
			t.Fatalf("removing the older version = %+v, %v", removed, err)
		}
		if _, err := s.ObjectAccess(ctx, "ver", "k", ref); !errors.Is(err, domain.ErrVersionNotFound) {
			t.Errorf("ObjectAccess of a removed version = %v, want %v", err, domain.ErrVersionNotFound)
		}
		if got := access(t, s, current); got.Downloads != 2 || len(got.Readers) != 1 || got.Readers[0].Who != "user/b" {
			t.Errorf("the version that stayed: %+v", got)
		}
		// A delete that makes a marker removes nothing, and no record.
		if _, err := s.DeleteObject(ctx, "ver", "k", ports.DeleteOptions{MarkerID: sampleVersionID(9), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got := access(t, s, current); got.Downloads != 2 {
			t.Errorf("after a delete marker: %+v", got)
		}
		// The same version, written again where it was removed, has no
		// past: the record did not outlive it.
		again, err := s.PutVersion(ctx, SampleVersion("ver", "k", 4))
		if err != nil {
			t.Fatal(err)
		}
		if got := access(t, s, again.Stored); !got.Empty() {
			t.Errorf("a version written again under the ID of a removed one has its reads: %+v", got)
		}
	})

	// A node counts for half a minute before it says what it counted, and
	// "null" names whatever was put last: the reads of a version that was
	// overwritten or deleted meanwhile are nobody's, and never those of the
	// version that took its name.
	t.Run("a read of a version that was replaced before it was recorded is not the new version's", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "plain", "ver", "suspended")
		if err := s.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		for _, status := range []bucket.Versioning{bucket.VersioningEnabled, bucket.VersioningSuspended} {
			if err := s.SetBucketVersioning(ctx, "suspended", status); err != nil {
				t.Fatal(err)
			}
		}

		// An overwrite between the read and the batch.
		first := put(t, s, "plain", "k", 1)
		second := put(t, s, "plain", "k", 2)
		add(t, s, "node-a", 1, 1, readOf(first, "user/first", false, 3, 1))
		if got := access(t, s, second); !got.Empty() || len(got.Readers) != 0 {
			t.Fatalf("the object put over the one that was read has its reader: %+v", got)
		}
		// Its own reads are its own, in the same batch as late ones.
		add(t, s, "node-a", 1, 2, readOf(first, "user/first", false, 1, 0), readOf(second, "user/second", false, 2, 0))
		if got := access(t, s, second); got.Downloads != 2 || len(got.Readers) != 1 || got.Readers[0].Who != "user/second" {
			t.Errorf("the record of the second object = %+v, want its own two downloads", got)
		}

		// Deleted and put again between the read and the batch.
		before := put(t, s, "plain", "again", 3)
		if _, err := s.DeleteObject(ctx, "plain", "again", ports.DeleteOptions{MarkerID: sampleVersionID(8), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		after := put(t, s, "plain", "again", 4)
		add(t, s, "node-a", 1, 3, readOf(before, "user/first", false, 1, 0))
		if got := access(t, s, after); !got.Empty() {
			t.Errorf("an object put again has the reader of the one deleted before it: %+v", got)
		}

		// The null version of a bucket whose versioning is suspended is
		// replaced by a put, like that of a bucket that keeps none.
		old := put(t, s, "suspended", "k", 5)
		replaced := put(t, s, "suspended", "k", 6)
		add(t, s, "node-a", 1, 4, readOf(old, "user/first", false, 1, 0))
		if got := access(t, s, replaced); !got.Empty() {
			t.Errorf("the null version put over another has its reader: %+v", got)
		}

		// Where versions are kept the name is the ID and nothing takes it
		// over: a read of a version that is no longer current is recorded
		// late as well as at once.
		older := put(t, s, "ver", "k", 7)
		current := put(t, s, "ver", "k", 8)
		add(t, s, "node-a", 1, 5, readOf(older, "user/first", false, 1, 0))
		if got := access(t, s, older); got.Downloads != 1 {
			t.Errorf("the late read of a version that is kept = %+v", got)
		}
		if got := access(t, s, current); !got.Empty() {
			t.Errorf("the current version has the read of the one before: %+v", got)
		}
	})

	// What can be asked through the port: a bucket of the same name has no
	// past. That nothing at all is left of the deleted one cannot be seen
	// from here - its versions had to go first, and each took its record -
	// and is asserted on each store's own state
	// (TestTheRecordOfReadsGoesWithItsVersion of the Raft store,
	// TestMemMetadataDeletesWhatABucketHeld of the one in memory).
	t.Run("a bucket takes the records of its versions with it", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b", "other")
		v := put(t, s, "b", "k", 1)
		elsewhere := put(t, s, "other", "k", 1)
		add(t, s, "node-a", 1, 1, readOf(v, "user/a", false, 2, 0), readOf(elsewhere, "user/a", false, 5, 0))

		if _, err := s.DeleteObject(ctx, "b", "k", ports.DeleteOptions{MarkerID: sampleVersionID(8), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteBucket(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ObjectAccess(ctx, "b", "k", v.Ref()); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ObjectAccess in a deleted bucket = %v, want %v", err, domain.ErrBucketNotFound)
		}
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got := access(t, s, put(t, s, "b", "k", 1)); !got.Empty() {
			t.Errorf("a bucket made again has the reads of the one before: %+v", got)
		}
		if got := access(t, s, elsewhere); got.Downloads != 5 {
			t.Errorf("another bucket's record after the deletion: %+v", got)
		}
	})

	t.Run("sub-buckets are registered, replaced and listed", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b", "other")
		policy := []byte(`{"Version":"2012-10-17","Statement":[]}`)

		if got, err := s.ListSubBuckets(ctx, "b"); err != nil || len(got) != 0 {
			t.Errorf("sub-buckets of a new bucket = %v, %v", got, err)
		}
		for _, name := range []string{"reports", "archive", "2026"} {
			sub := bucket.SubBucket{Name: name, CreatedAt: contractTime}
			if err := s.PutSubBucket(ctx, "b", sub, noAudit); err != nil {
				t.Fatalf("PutSubBucket %s: %v", name, err)
			}
		}
		listed, err := s.ListSubBuckets(ctx, "b")
		if err != nil || len(listed) != 3 || listed[0].Name != "2026" || listed[1].Name != "archive" || listed[2].Name != "reports" {
			t.Fatalf("ListSubBuckets = %+v, %v; want three in name order", listed, err)
		}
		if got, err := s.GetSubBucket(ctx, "b", "reports"); err != nil || got.Name != "reports" || !got.CreatedAt.Equal(contractTime) || len(got.Policy) != 0 {
			t.Errorf("GetSubBucket = %+v, %v", got, err)
		}
		// Putting one that is registered replaces its policy; it is as
		// old as it was.
		later := bucket.SubBucket{Name: "reports", CreatedAt: contractTime.Add(time.Hour), Policy: policy}
		if err := s.PutSubBucket(ctx, "b", later, noAudit); err != nil {
			t.Fatalf("replacing a sub-bucket: %v", err)
		}
		got, err := s.GetSubBucket(ctx, "b", "reports")
		if err != nil || !bytes.Equal(got.Policy, policy) || !got.CreatedAt.Equal(contractTime) {
			t.Errorf("the replaced sub-bucket = %+v, %v", got, err)
		}
		if listed, err := s.ListSubBuckets(ctx, "b"); err != nil || len(listed) != 3 {
			t.Errorf("after replacing one there are %d, %v", len(listed), err)
		}
		// And its policy is taken off the same way.
		if err := s.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "reports"}, noAudit); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetSubBucket(ctx, "b", "reports"); err != nil || len(got.Policy) != 0 {
			t.Errorf("the sub-bucket after its policy was removed = %+v, %v", got, err)
		}
		// A sub-bucket belongs to its bucket and to no other.
		if got, err := s.ListSubBuckets(ctx, "other"); err != nil || len(got) != 0 {
			t.Errorf("sub-buckets of another bucket = %v, %v", got, err)
		}
		if _, err := s.GetSubBucket(ctx, "other", "reports"); !errors.Is(err, domain.ErrSubBucketNotFound) {
			t.Errorf("GetSubBucket in another bucket = %v, want %v", err, domain.ErrSubBucketNotFound)
		}
		if err := s.DeleteSubBucket(ctx, "b", "archive", noAudit); err != nil {
			t.Fatalf("DeleteSubBucket: %v", err)
		}
		if _, err := s.GetSubBucket(ctx, "b", "archive"); !errors.Is(err, domain.ErrSubBucketNotFound) {
			t.Errorf("GetSubBucket of a deleted one = %v, want %v", err, domain.ErrSubBucketNotFound)
		}
	})

	t.Run("a sub-bucket that cannot be is refused", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		incomplete := audit.Record{Actor: "root", Outcome: audit.Succeeded}
		tests := map[string]struct {
			bucket string
			sub    bucket.SubBucket
			record audit.Record
			want   error
		}{
			"no name":                           {"b", bucket.SubBucket{}, noAudit, domain.ErrInvalidArgument},
			"a name with a slash":               {"b", bucket.SubBucket{Name: "a/b"}, noAudit, domain.ErrInvalidArgument},
			"a name in upper case":              {"b", bucket.SubBucket{Name: "Reports"}, noAudit, domain.ErrInvalidArgument},
			"a name of 64 letters":              {"b", bucket.SubBucket{Name: strings.Repeat("a", 64)}, noAudit, domain.ErrInvalidArgument},
			"a policy too large":                {"b", bucket.SubBucket{Name: "big", Policy: make([]byte, iam.MaxPolicyBytes+1)}, noAudit, domain.ErrInvalidArgument},
			"a bucket that is not":              {"missing", bucket.SubBucket{Name: "reports"}, noAudit, domain.ErrBucketNotFound},
			"an audit record without an action": {"b", bucket.SubBucket{Name: "reports"}, incomplete, domain.ErrInvalidArgument},
		}
		for name, test := range tests {
			test.sub.CreatedAt = contractTime
			if err := s.PutSubBucket(ctx, test.bucket, test.sub, test.record); !errors.Is(err, test.want) {
				t.Errorf("%s: PutSubBucket = %v, want %v", name, err, test.want)
			}
		}
		if got, err := s.ListSubBuckets(ctx, "b"); err != nil || len(got) != 0 {
			t.Errorf("after refusals the bucket has sub-buckets %v, %v", got, err)
		}
		// The largest policy there can be is kept.
		if err := s.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "big", Policy: bytes.Repeat([]byte{'x'}, iam.MaxPolicyBytes)}, noAudit); err != nil {
			t.Errorf("a policy of %d bytes: %v", iam.MaxPolicyBytes, err)
		}
		if _, err := s.ListSubBuckets(ctx, "missing"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ListSubBuckets of a bucket that is not = %v", err)
		}
		if _, err := s.GetSubBucket(ctx, "missing", "x"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("GetSubBucket in a bucket that is not = %v", err)
		}
		if err := s.DeleteSubBucket(ctx, "missing", "x", noAudit); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("DeleteSubBucket in a bucket that is not = %v", err)
		}
		if err := s.DeleteSubBucket(ctx, "b", "never", noAudit); !errors.Is(err, domain.ErrSubBucketNotFound) {
			t.Errorf("DeleteSubBucket of one that is not = %v", err)
		}
	})

	t.Run("a bucket has at most ten sub-buckets", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b", "other")
		for i := range bucket.MaxSubBuckets {
			if err := s.PutSubBucket(ctx, "b", bucket.SubBucket{Name: fmt.Sprintf("sub-%02d", i), CreatedAt: contractTime}, noAudit); err != nil {
				t.Fatalf("sub-bucket %d: %v", i+1, err)
			}
		}
		eleventh := bucket.SubBucket{Name: "one-too-many", CreatedAt: contractTime}
		if err := s.PutSubBucket(ctx, "b", eleventh, noAudit); !errors.Is(err, domain.ErrTooManySubBuckets) {
			t.Fatalf("the eleventh sub-bucket = %v, want %v", err, domain.ErrTooManySubBuckets)
		}
		if got, err := s.ListSubBuckets(ctx, "b"); err != nil || len(got) != bucket.MaxSubBuckets {
			t.Errorf("after the refusal there are %d, %v", len(got), err)
		}
		// The bound is on how many there are: one of the ten can still be
		// replaced, another bucket has its own ten, and when one goes
		// there is room for one.
		if err := s.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "sub-03", Policy: []byte("{}")}, noAudit); err != nil {
			t.Errorf("replacing one of ten: %v", err)
		}
		if err := s.PutSubBucket(ctx, "other", eleventh, noAudit); err != nil {
			t.Errorf("a sub-bucket of another bucket: %v", err)
		}
		if err := s.DeleteSubBucket(ctx, "b", "sub-00", noAudit); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSubBucket(ctx, "b", eleventh, noAudit); err != nil {
			t.Errorf("a tenth sub-bucket after one was deleted: %v", err)
		}
	})

	t.Run("a sub-bucket is deleted only when it holds nothing", func(t *testing.T) {
		t.Parallel()
		marker := func(t *testing.T, s Schema3, key string, fill byte) object.Version {
			t.Helper()
			deleted, err := s.DeleteObject(ctx, "b", key, ports.DeleteOptions{MarkerID: sampleVersionID(fill), At: contractTime})
			if err != nil || deleted.Marker == nil {
				t.Fatalf("DeleteObject = %+v, %v; want a delete marker", deleted, err)
			}
			return *deleted.Marker
		}
		remove := func(t *testing.T, s Schema3, v object.Version) {
			t.Helper()
			ref := v.Ref()
			if _, err := s.DeleteObject(ctx, "b", v.Key, ports.DeleteOptions{Version: &ref}); err != nil {
				t.Fatalf("removing version %s of %s: %v", ref, v.Key, err)
			}
		}
		// Each case fills the sub-bucket "reports" with one kind of thing
		// and returns how to take it out again, a step at a time: the
		// sub-bucket must be refused until the last step is done.
		tests := map[string]struct {
			versioned bool
			fill      func(t *testing.T, s Schema3) []func(t *testing.T, s Schema3)
		}{
			"a current object": {false, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				v := put(t, s, "b", "reports/q3.pdf", 1)
				return []func(*testing.T, Schema3){func(t *testing.T, s Schema3) { remove(t, s, v) }}
			}},
			"a version that is no longer current": {true, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				old := put(t, s, "b", "reports/q3.pdf", 1)
				current := put(t, s, "b", "reports/q3.pdf", 2)
				return []func(*testing.T, Schema3){
					// With the current one gone, the older one is all
					// there is - and it is still there.
					func(t *testing.T, s Schema3) { remove(t, s, current) },
					func(t *testing.T, s Schema3) { remove(t, s, old) },
				}
			}},
			"a version under a delete marker": {true, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				v := put(t, s, "b", "reports/q3.pdf", 1)
				m := marker(t, s, "reports/q3.pdf", 7)
				return []func(*testing.T, Schema3){
					func(t *testing.T, s Schema3) { remove(t, s, m) },
					func(t *testing.T, s Schema3) { remove(t, s, v) },
				}
			}},
			"a delete marker alone": {true, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				v := put(t, s, "b", "reports/q3.pdf", 1)
				m := marker(t, s, "reports/q3.pdf", 7)
				remove(t, s, v)
				return []func(*testing.T, Schema3){func(t *testing.T, s Schema3) { remove(t, s, m) }}
			}},
			"a delete marker of an object that never was": {true, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				m := marker(t, s, "reports/never", 7)
				return []func(*testing.T, Schema3){func(t *testing.T, s Schema3) { remove(t, s, m) }}
			}},
			"an upload in progress": {false, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				u := SampleUpload("b", "reports/big.bin", 1)
				if err := s.CreateUpload(ctx, u); err != nil {
					t.Fatal(err)
				}
				return []func(*testing.T, Schema3){func(t *testing.T, s Schema3) {
					if _, err := s.AbortUpload(ctx, "b", u.Key, u.ID); err != nil {
						t.Fatal(err)
					}
				}}
			}},
			"an upload in progress with a part": {true, func(t *testing.T, s Schema3) []func(*testing.T, Schema3) {
				u := SampleUpload("b", "reports/deep/big.bin", 1)
				if err := s.CreateUpload(ctx, u); err != nil {
					t.Fatal(err)
				}
				part := SamplePart(u, 1, 10, 0)
				if _, err := s.PutPart(ctx, "b", u.Key, u.ID, part); err != nil {
					t.Fatal(err)
				}
				return []func(*testing.T, Schema3){
					// Completing it makes an object, which is there too.
					func(t *testing.T, s Schema3) {
						if _, err := s.CompleteUpload(ctx, completionOf(u, 5, part)); err != nil {
							t.Fatal(err)
						}
					},
					func(t *testing.T, s Schema3) {
						ref := object.VersionRef{ID: sampleVersionID(5)}
						if _, err := s.DeleteObject(ctx, "b", u.Key, ports.DeleteOptions{Version: &ref}); err != nil {
							t.Fatal(err)
						}
					},
				}
			}},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s := withBucket(t, "b")
				if test.versioned {
					if err := s.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "reports", CreatedAt: contractTime}, noAudit); err != nil {
					t.Fatal(err)
				}
				steps := test.fill(t, s)
				for i, step := range steps {
					if err := s.DeleteSubBucket(ctx, "b", "reports", noAudit); !errors.Is(err, domain.ErrSubBucketNotEmpty) {
						t.Fatalf("before step %d of emptying it: DeleteSubBucket = %v, want %v", i, err, domain.ErrSubBucketNotEmpty)
					}
					if _, err := s.GetSubBucket(ctx, "b", "reports"); err != nil {
						t.Fatalf("the sub-bucket after a refused delete: %v", err)
					}
					step(t, s)
				}
				if err := s.DeleteSubBucket(ctx, "b", "reports", noAudit); err != nil {
					t.Errorf("DeleteSubBucket of the emptied sub-bucket: %v", err)
				}
			})
		}

		t.Run("what is beside it is not in it", func(t *testing.T) {
			t.Parallel()
			s := withBucket(t, "b", "other")
			if err := s.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "reports", CreatedAt: contractTime}, noAudit); err != nil {
				t.Fatal(err)
			}
			// Keys that begin like its name, a key that is its name, a
			// key at the top of the bucket, and its own name in another
			// bucket: none of them is under "reports/".
			for i, key := range []string{"reports", "reports-old/q1.pdf", "reportsq1", "report/s", "top", "z/reports/q1"} {
				put(t, s, "b", key, byte(i+1))
			}
			put(t, s, "other", "reports/q1.pdf", 1)
			u := SampleUpload("b", "reports.d/x", 1)
			if err := s.CreateUpload(ctx, u); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteSubBucket(ctx, "b", "reports", noAudit); err != nil {
				t.Errorf("DeleteSubBucket of an empty sub-bucket among other keys: %v", err)
			}
		})
	})

	t.Run("a bucket takes its sub-buckets with it", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b", "other")
		for _, name := range []string{"b", "other"} {
			if err := s.PutSubBucket(ctx, name, bucket.SubBucket{Name: "reports", CreatedAt: contractTime, Policy: []byte("{}")}, noAudit); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.DeleteBucket(ctx, "b"); err != nil {
			t.Fatalf("DeleteBucket of a bucket with an empty sub-bucket: %v", err)
		}
		if _, err := s.ListSubBuckets(ctx, "b"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ListSubBuckets of the deleted bucket = %v", err)
		}
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ListSubBuckets(ctx, "b"); err != nil || len(got) != 0 {
			t.Errorf("a bucket made again has the sub-buckets of the one before: %v, %v", got, err)
		}
		if got, err := s.ListSubBuckets(ctx, "other"); err != nil || len(got) != 1 {
			t.Errorf("another bucket's sub-buckets after the deletion: %v, %v", got, err)
		}
	})

	t.Run("tag definitions are stored, replaced and listed", func(t *testing.T) {
		t.Parallel()
		s := open(t)

		if got, err := s.ListTagDefinitions(ctx); err != nil || len(got) != 0 {
			t.Errorf("the catalogue of a new store = %v, %v", got, err)
		}
		stage := tagging.Definition{Key: "stage", Description: "Where the work is.", Values: []string{"draft", "review", "final"}, CreatedAt: contractTime}
		owner := tagging.Definition{Key: "owner", CreatedAt: contractTime}
		for _, d := range []tagging.Definition{stage, owner} {
			if err := s.PutTagDefinition(ctx, d, noAudit); err != nil {
				t.Fatalf("PutTagDefinition %s: %v", d.Key, err)
			}
		}
		got, err := s.GetTagDefinition(ctx, "stage")
		if err != nil || !reflect.DeepEqual(got.Values, stage.Values) || got.Description != stage.Description || !got.CreatedAt.Equal(contractTime) {
			t.Errorf("GetTagDefinition = %+v, %v; want %+v", got, err, stage)
		}
		listed, err := s.ListTagDefinitions(ctx)
		if err != nil || len(listed) != 2 || listed[0].Key != "owner" || listed[1].Key != "stage" || len(listed[0].Values) != 0 {
			t.Fatalf("ListTagDefinitions = %+v, %v; want two in key order", listed, err)
		}
		// Putting one that is defined replaces what is said of it; the
		// order of its values is kept, and it is as old as it was.
		stage.Description, stage.Values, stage.CreatedAt = "", []string{"final", "draft"}, contractTime.Add(time.Hour)
		if err := s.PutTagDefinition(ctx, stage, noAudit); err != nil {
			t.Fatalf("replacing a definition: %v", err)
		}
		got, err = s.GetTagDefinition(ctx, "stage")
		if err != nil || !reflect.DeepEqual(got.Values, []string{"final", "draft"}) || got.Description != "" || !got.CreatedAt.Equal(contractTime) {
			t.Errorf("the replaced definition = %+v, %v", got, err)
		}

		for name, d := range map[string]tagging.Definition{
			"no key":              {},
			"a key in upper case": {Key: "Stage"},
			"a value twice":       {Key: "k", Values: []string{"a", "a"}},
			"a value in upper":    {Key: "k", Values: []string{"A"}},
			"too many values":     {Key: "k", Values: make([]string, tagging.MaxValues+1)},
			"a long description":  {Key: "k", Description: strings.Repeat("d", tagging.MaxDescriptionBytes+1)},
		} {
			if err := s.PutTagDefinition(ctx, d, noAudit); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: PutTagDefinition = %v, want %v", name, err, domain.ErrInvalidArgument)
			}
		}
		if _, err := s.GetTagDefinition(ctx, "k"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("GetTagDefinition of one that is not = %v, want %v", err, domain.ErrEntityNotFound)
		}
		if err := s.DeleteTagDefinition(ctx, "k", noAudit); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("DeleteTagDefinition of one that is not = %v, want %v", err, domain.ErrEntityNotFound)
		}
		if err := s.DeleteTagDefinition(ctx, "owner", noAudit); err != nil {
			t.Fatalf("DeleteTagDefinition: %v", err)
		}
		if listed, err := s.ListTagDefinitions(ctx); err != nil || len(listed) != 1 || listed[0].Key != "stage" {
			t.Errorf("the catalogue after a deletion = %+v, %v", listed, err)
		}
	})

	t.Run("a definition that a rule names cannot be deleted", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "first", "second")
		for _, key := range []string{"stage", "team", "free"} {
			if err := s.PutTagDefinition(ctx, tagging.Definition{Key: key, CreatedAt: contractTime}, noAudit); err != nil {
				t.Fatal(err)
			}
		}
		setRules := func(bucketName string, rules bucket.TagRules) {
			t.Helper()
			document, err := rules.Document()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetBucketConfig(ctx, bucketName, bucket.ConfigTagRules, document); err != nil {
				t.Fatalf("SetBucketConfig: %v", err)
			}
		}
		setRules("first", bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{{Key: "stage"}}})
		// A rule confined to a sub-bucket names its tag like any other.
		setRules("second", bucket.TagRules{CatalogueOnly: true, Required: []bucket.RequiredTag{{Key: "other"}, {SubBucket: "reports", Key: "team", Values: []string{"ops"}}}})

		for _, key := range []string{"stage", "team"} {
			if err := s.DeleteTagDefinition(ctx, key, noAudit); !errors.Is(err, domain.ErrConflict) {
				t.Errorf("DeleteTagDefinition of %s, which a rule names = %v, want %v", key, err, domain.ErrConflict)
			}
			if _, err := s.GetTagDefinition(ctx, key); err != nil {
				t.Errorf("the definition of %s after a refused deletion: %v", key, err)
			}
		}
		// One that no rule names goes, whatever the rules say of the
		// catalogue as a whole.
		if err := s.DeleteTagDefinition(ctx, "free", noAudit); err != nil {
			t.Errorf("DeleteTagDefinition of a tag no rule names: %v", err)
		}
		// When the rule is changed, or the rules are removed, or the
		// bucket is, the definition can go.
		setRules("first", bucket.TagRules{Lowercase: true})
		if err := s.DeleteTagDefinition(ctx, "stage", noAudit); err != nil {
			t.Errorf("after the rule was changed: %v", err)
		}
		if err := s.DeleteBucket(ctx, "second"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTagDefinition(ctx, "team", noAudit); err != nil {
			t.Errorf("after the bucket was deleted: %v", err)
		}
	})

	t.Run("the catalogue holds two hundred definitions", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		for i := range tagging.MaxDefinitions {
			if err := s.PutTagDefinition(ctx, tagging.Definition{Key: fmt.Sprintf("tag-%03d", i), CreatedAt: contractTime}, noAudit); err != nil {
				t.Fatalf("definition %d: %v", i+1, err)
			}
		}
		extra := tagging.Definition{Key: "one-too-many", CreatedAt: contractTime}
		if err := s.PutTagDefinition(ctx, extra, noAudit); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Fatalf("definition %d = %v, want %v", tagging.MaxDefinitions+1, err, domain.ErrInvalidArgument)
		}
		if listed, err := s.ListTagDefinitions(ctx); err != nil || len(listed) != tagging.MaxDefinitions {
			t.Errorf("the catalogue has %d definitions, %v", len(listed), err)
		}
		// One of them can still be replaced, and one deleted makes room.
		if err := s.PutTagDefinition(ctx, tagging.Definition{Key: "tag-007", Description: "replaced"}, noAudit); err != nil {
			t.Errorf("replacing a definition of a full catalogue: %v", err)
		}
		if err := s.DeleteTagDefinition(ctx, "tag-000", noAudit); err != nil {
			t.Fatal(err)
		}
		if err := s.PutTagDefinition(ctx, extra, noAudit); err != nil {
			t.Errorf("a definition after one was deleted: %v", err)
		}
	})

	t.Run("changes carry their audit record", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		sub := bucket.SubBucket{Name: "reports", CreatedAt: contractTime}
		definition := tagging.Definition{Key: "stage", CreatedAt: contractTime}

		// A change that took effect is in the trail, with what it was
		// given, in the order of the changes.
		steps := []struct {
			action, resource string
			change           func(record audit.Record) error
		}{
			{"s3:PutSubBucket", "b/reports", func(r audit.Record) error { return s.PutSubBucket(ctx, "b", sub, r) }},
			{"admin:PutTagDefinition", "tag/stage", func(r audit.Record) error { return s.PutTagDefinition(ctx, definition, r) }},
			{"s3:DeleteSubBucket", "b/reports", func(r audit.Record) error { return s.DeleteSubBucket(ctx, "b", "reports", r) }},
			{"admin:DeleteTagDefinition", "tag/stage", func(r audit.Record) error { return s.DeleteTagDefinition(ctx, "stage", r) }},
		}
		for i, step := range steps {
			if err := step.change(auditOf(step.action, step.resource)); err != nil {
				t.Fatalf("%s: %v", step.action, err)
			}
			records := trail(t, s)
			if len(records) != i+1 {
				t.Fatalf("after %s the trail has %d records, want %d", step.action, len(records), i+1)
			}
			last := records[i]
			if last.Action != step.action || last.Resource != step.resource || last.Actor != "root" || last.Outcome != audit.Succeeded ||
				last.Sequence != uint64(i+1) || last.Details["k"] != "v" || last.Hash == "" {
				t.Errorf("the record of %s = %+v", step.action, last)
			}
			if i > 0 && last.PreviousHash != records[i-1].Hash {
				t.Errorf("the record of %s is not chained to the one before", step.action)
			}
		}

		// A change that was refused leaves none: the refusal is for
		// whoever asked to record.
		refused := map[string]error{
			"a sub-bucket of a missing bucket":  s.PutSubBucket(ctx, "missing", sub, auditOf("s3:PutSubBucket", "missing/reports")),
			"deleting a sub-bucket that is not": s.DeleteSubBucket(ctx, "b", "reports", auditOf("s3:DeleteSubBucket", "b/reports")),
			"deleting a definition that is not": s.DeleteTagDefinition(ctx, "stage", auditOf("admin:DeleteTagDefinition", "tag/stage")),
			"a definition that cannot be kept":  s.PutTagDefinition(ctx, tagging.Definition{Key: "UPPER"}, auditOf("admin:PutTagDefinition", "tag/UPPER")),
		}
		for name, err := range refused {
			if err == nil {
				t.Errorf("%s was not refused", name)
			}
		}
		// Nor does a change made without an actor.
		if err := s.PutSubBucket(ctx, "b", sub, noAudit); err != nil {
			t.Fatal(err)
		}
		if err := s.PutTagDefinition(ctx, definition, noAudit); err != nil {
			t.Fatal(err)
		}
		if records := trail(t, s); len(records) != len(steps) {
			t.Errorf("after refused and unaudited changes the trail has %d records, want %d", len(records), len(steps))
		}
		// A record that cannot be kept refuses the change it came with.
		bad := audit.Record{Actor: "root", Action: "s3:DeleteSubBucket", Outcome: "perhaps"}
		if err := s.DeleteSubBucket(ctx, "b", "reports", bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a change with an audit record that cannot be kept = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if _, err := s.GetSubBucket(ctx, "b", "reports"); err != nil {
			t.Errorf("the sub-bucket after that refusal: %v", err)
		}
		if err := s.DeleteTagDefinition(ctx, "stage", bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a deletion with an audit record that cannot be kept = %v, want %v", err, domain.ErrInvalidArgument)
		}
	})
}
