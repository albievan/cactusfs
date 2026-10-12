package porttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Schema4 is everything the contract of entry schema 4 exercises: the
// metadata ports, the reads that are handed in, and what ADR-0019 added - who
// created a bucket, and the latest uploads and downloads of every bucket.
type Schema4 interface {
	Metadata
	ports.ObjectAccessStore
	ports.BucketActivityStore
}

// Schema4Contract is the behaviour every store of who created a bucket and
// of the latest use of a bucket must have. open returns a new, empty store in
// which all of it can be written: every member of its cluster knows entry
// schema 4, and the cluster - if it is one that knows its nodes - knows the
// nodes "node-a" and "node-b", which hand in reads.
func Schema4Contract(t *testing.T, open func(t *testing.T) Schema4) {
	t.Helper()
	ctx := context.Background()

	withBucket := func(t *testing.T, names ...string) Schema4 {
		t.Helper()
		s := open(t)
		for _, name := range names {
			if err := s.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: contractTime}); err != nil {
				t.Fatalf("CreateBucket %s: %v", name, err)
			}
		}
		return s
	}
	// put commits a version of a key by somebody; fill keeps the samples
	// apart.
	put := func(t *testing.T, s Schema4, bucketName, key, by string, fill byte) object.Version {
		t.Helper()
		v := SampleVersion(bucketName, key, fill)
		v.CreatedBy = by
		result, err := s.PutVersion(ctx, v)
		if err != nil {
			t.Fatalf("PutVersion %s/%s: %v", bucketName, key, err)
		}
		return result.Stored
	}
	add := func(t *testing.T, s Schema4, node string, seq uint64, items ...ports.ObjectRead) {
		t.Helper()
		if err := s.AddObjectReads(ctx, ports.ReadBatch{Node: node, Boot: 1, Seq: seq, Items: items}); err != nil {
			t.Fatalf("AddObjectReads %s %d: %v", node, seq, err)
		}
	}
	activity := func(t *testing.T, s Schema4, name string) bucket.Activity {
		t.Helper()
		got, err := s.BucketActivities(ctx, []string{name})
		if err != nil || len(got) != 1 {
			t.Fatalf("BucketActivities %s = %+v, %v", name, got, err)
		}
		return got[0]
	}
	// uploads and downloads put the rows of a record in words: the key and
	// who, newest first. The times are the store's own and are asked about
	// apart.
	uploads := func(a bucket.Activity) string {
		rows := make([]string, 0, len(a.Uploads))
		for _, u := range a.Uploads {
			rows = append(rows, u.Key+" by "+u.By)
		}
		return strings.Join(rows, "; ")
	}
	downloads := func(a bucket.Activity) string {
		rows := make([]string, 0, len(a.Downloads))
		for _, d := range a.Downloads {
			rows = append(rows, fmt.Sprintf("%s by %s presigned=%t", d.Key, d.By, d.Presigned))
		}
		return strings.Join(rows, "; ")
	}
	// timed reports whether the rows carry a time of the store - one of
	// about now - and are newest first.
	timed := func(a bucket.Activity, since time.Time) bool {
		uploaded := make([]time.Time, 0, len(a.Uploads))
		for _, u := range a.Uploads {
			uploaded = append(uploaded, u.At)
		}
		downloaded := make([]time.Time, 0, len(a.Downloads))
		for _, d := range a.Downloads {
			downloaded = append(downloaded, d.At)
		}
		newestFirst := func(x, y time.Time) int { return y.Compare(x) }
		ok := slices.IsSortedFunc(uploaded, newestFirst) && slices.IsSortedFunc(downloaded, newestFirst)
		for _, at := range slices.Concat(uploaded, downloaded) {
			ok = ok && !at.Before(since.Add(-time.Minute)) && !at.After(time.Now().Add(time.Minute))
		}
		return ok
	}

	t.Run("who created a bucket is recorded", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		for name, by := range map[string]string{"by-root": "root", "by-user": "user/albie", "by-key": "key/AKID", "by-nobody": ""} {
			if err := s.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: contractTime, CreatedBy: by}); err != nil {
				t.Fatalf("CreateBucket %s: %v", name, err)
			}
			if got, err := s.GetBucket(ctx, name); err != nil || got.CreatedBy != by || !got.CreatedAt.Equal(contractTime) {
				t.Errorf("GetBucket %s = created by %q at %s, %v; want %q", name, got.CreatedBy, got.CreatedAt, err, by)
			}
		}
		listed, err := s.ListBuckets(ctx)
		if err != nil || len(listed) != 4 {
			t.Fatalf("ListBuckets = %+v, %v", listed, err)
		}
		for _, b := range listed {
			want := map[string]string{"by-root": "root", "by-user": "user/albie", "by-key": "key/AKID", "by-nobody": ""}[b.Name]
			if b.CreatedBy != want {
				t.Errorf("ListBuckets: %s created by %q, want %q", b.Name, b.CreatedBy, want)
			}
		}
		// It is said once, at creation: nothing that changes the bucket
		// afterwards changes it.
		if err := s.SetBucketVersioning(ctx, "by-user", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketConfig(ctx, "by-user", bucket.ConfigPolicy, []byte(`{"Statement":[]}`)); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBucket(ctx, "by-user"); err != nil || got.CreatedBy != "user/albie" || got.Versioning != bucket.VersioningEnabled {
			t.Errorf("after changes to the bucket: created by %q, versioning %q, %v", got.CreatedBy, got.Versioning, err)
		}
		// A bucket created again after its deletion is its new creator's.
		if err := s.DeleteBucket(ctx, "by-root"); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "by-root", CreatedAt: contractTime, CreatedBy: "user/second"}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBucket(ctx, "by-root"); err != nil || got.CreatedBy != "user/second" {
			t.Errorf("a bucket created again: created by %q, %v", got.CreatedBy, err)
		}
	})

	t.Run("a creator that cannot be recorded is refused", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		for what, by := range map[string]string{"a name too long": strings.Repeat("u", 257), "a name of two lines": "user/a\nuser/b", "a name with a NUL": "user/a\x00"} {
			err := s.CreateBucket(ctx, bucket.Bucket{Name: "refused", CreatedAt: contractTime, CreatedBy: by})
			if !errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrOutcomeUnknown) {
				t.Errorf("CreateBucket by %s = %v, want a definite %v", what, err, domain.ErrInvalidArgument)
			}
			if _, err := s.GetBucket(ctx, "refused"); !errors.Is(err, domain.ErrBucketNotFound) {
				t.Errorf("the bucket refused for %s exists: %v", what, err)
			}
		}
		// The longest there can be is kept whole.
		longest := strings.Repeat("u", 256)
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "longest", CreatedAt: contractTime, CreatedBy: longest}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBucket(ctx, "longest"); err != nil || got.CreatedBy != longest {
			t.Errorf("the longest creator there can be = %d bytes, %v", len(got.CreatedBy), err)
		}
	})

	t.Run("a bucket of which nothing is known has the zero record", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "quiet", "busy")
		put(t, s, "busy", "k", "root", 1)
		got, err := s.BucketActivities(ctx, []string{"quiet", "no-such-bucket", "busy", "quiet", ""})
		if err != nil || len(got) != 5 {
			t.Fatalf("BucketActivities = %+v, %v", got, err)
		}
		// In the order they were named, a name given twice answered twice.
		for i, empty := range []bool{true, true, false, true, true} {
			if got[i].Empty() != empty {
				t.Errorf("the record at %d = %+v, want empty: %t", i, got[i], empty)
			}
		}
		if none, err := s.BucketActivities(ctx, nil); err != nil || len(none) != 0 {
			t.Errorf("BucketActivities of no bucket = %+v, %v", none, err)
		}
	})

	t.Run("a question about too many buckets is refused", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		put(t, s, "b", "k", "root", 1)
		names := make([]string, ports.MaxBucketActivities+1)
		for i := range names {
			names[i] = fmt.Sprintf("bucket-%05d", i)
		}
		names[ports.MaxBucketActivities-1] = "b"
		if _, err := s.BucketActivities(ctx, names); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("BucketActivities of %d buckets = %v, want %v", len(names), err, domain.ErrInvalidArgument)
		}
		// As many as may be asked about are answered, each at its place.
		got, err := s.BucketActivities(ctx, names[:ports.MaxBucketActivities])
		if err != nil || len(got) != ports.MaxBucketActivities || got[ports.MaxBucketActivities-1].Empty() || !got[0].Empty() {
			t.Errorf("BucketActivities of %d buckets = %d records, %v", ports.MaxBucketActivities, len(got), err)
		}
	})

	t.Run("every version that is committed is an upload", func(t *testing.T) {
		t.Parallel()
		since := time.Now()
		s := withBucket(t, "b", "other")
		put(t, s, "b", "first", "user/albie", 1)
		put(t, s, "b", "second", "", 2)
		if got := activity(t, s, "b"); uploads(got) != "second by ; first by user/albie" || len(got.Downloads) != 0 || !timed(got, since) {
			t.Fatalf("after two puts = %+v", got)
		}
		// A key has one row: the later upload of it, at the front.
		put(t, s, "b", "first", "key/AKID", 3)
		if got := activity(t, s, "b"); uploads(got) != "first by key/AKID; second by " {
			t.Errorf("after the first key was put again = %q", uploads(got))
		}
		// The latest three keys are kept.
		put(t, s, "b", "third", "root", 4)
		put(t, s, "b", "fourth", "root", 5)
		if got := activity(t, s, "b"); uploads(got) != "fourth by root; third by root; first by key/AKID" || !timed(got, since) {
			t.Errorf("after five puts = %q", uploads(got))
		}
		// What is uploaded into one bucket says nothing of another.
		if got := activity(t, s, "other"); !got.Empty() {
			t.Errorf("a bucket nothing was put into = %+v", got)
		}
		// A put that is refused is no upload.
		if _, err := s.PutVersion(ctx, SampleVersion("b", "", 6)); err == nil {
			t.Fatal("a put without a key was accepted")
		}
		if _, err := s.PutVersion(ctx, SampleVersion("no-such-bucket", "k", 6)); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Fatalf("a put into no bucket = %v", err)
		}
		if got, err := s.BucketActivities(ctx, []string{"b", "no-such-bucket"}); err != nil || uploads(got[0]) != "fourth by root; third by root; first by key/AKID" || !got[1].Empty() {
			t.Errorf("after refused puts = %+v, %v", got, err)
		}
	})

	t.Run("a multipart upload is an upload once, when it is completed", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		u := SampleUpload("b", "large", 1)
		if err := s.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		part := SamplePart(u, 1, 10, 0)
		if _, err := s.PutPart(ctx, "b", u.Key, u.ID, part); err != nil {
			t.Fatal(err)
		}
		if got := activity(t, s, "b"); !got.Empty() {
			t.Errorf("an upload in progress and its part = %+v", got)
		}
		// One that is aborted never was an upload.
		aborted := SampleUpload("b", "aborted", 2)
		if err := s.CreateUpload(ctx, aborted); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AbortUpload(ctx, "b", aborted.Key, aborted.ID); err != nil {
			t.Fatal(err)
		}
		part.Seal = bytes.Repeat([]byte{1}, 32)
		if _, err := s.CompleteUpload(ctx, ports.Completion{
			Bucket: "b", Key: u.Key, ID: u.ID, VersionID: sampleVersionID(7), ETag: "etag", At: contractTime,
			CreatedBy: "key/AKID", Parts: []object.Part{part},
		}); err != nil {
			t.Fatalf("CompleteUpload: %v", err)
		}
		if got := activity(t, s, "b"); uploads(got) != "large by key/AKID" {
			t.Errorf("after the completion = %q", uploads(got))
		}
	})

	t.Run("what is no upload leaves the record as it is, and a row outlives its object", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "plain", "ver")
		if err := s.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		kept := put(t, s, "ver", "k", "user/albie", 1)
		gone := put(t, s, "plain", "k", "user/albie", 2)
		want := map[string]bucket.Activity{"ver": activity(t, s, "ver"), "plain": activity(t, s, "plain")}
		same := func(what string) {
			t.Helper()
			for name, before := range want {
				got := activity(t, s, name)
				if uploads(got) != uploads(before) || len(got.Downloads) != 0 || !got.Uploads[0].At.Equal(before.Uploads[0].At) {
					t.Errorf("after %s the record of %s = %+v, was %+v", what, name, got, before)
				}
			}
		}
		if _, err := s.SetTags(ctx, "ver", "k", kept.Ref(), map[string]string{"stage": "draft"}); err != nil {
			t.Fatal(err)
		}
		same("a change of tags")
		if _, err := s.DeleteObject(ctx, "ver", "k", ports.DeleteOptions{MarkerID: sampleVersionID(8), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		same("a delete marker")
		ref := kept.Ref()
		if _, err := s.DeleteObject(ctx, "ver", "k", ports.DeleteOptions{Version: &ref}); err != nil {
			t.Fatal(err)
		}
		same("the removal of the version")
		if _, err := s.DeleteObject(ctx, "plain", "k", ports.DeleteOptions{MarkerID: sampleVersionID(9), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetObject(ctx, "plain", gone.Key); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Fatalf("the deleted object = %v", err)
		}
		same("the deletion of the object")
		if err := s.SetBucketConfig(ctx, "plain", bucket.ConfigTagging, []byte(`{"Team":"Storage"}`)); err != nil {
			t.Fatal(err)
		}
		same("a change of the bucket's configuration")
	})

	t.Run("the downloads of a batch of reads are downloads from their buckets", func(t *testing.T) {
		t.Parallel()
		since := time.Now()
		s := withBucket(t, "b", "other")
		a, b := put(t, s, "b", "a", "root", 1), put(t, s, "b", "b", "root", 2)
		elsewhere := put(t, s, "other", "x", "root", 3)
		add(t, s, "node-a", 1,
			readOf(a, "user/albie", false, 2, 0),
			// The source of a copy was not downloaded.
			readOf(b, "user/copier", false, 0, 3),
			readOf(elsewhere, "anonymous", false, 1, 0),
			// A reader who came with a URL they signed is kept apart from
			// the signer's own reads, and both downloaded and copied.
			readOf(a, "key/AKID", true, 1, 1),
		)
		if got := activity(t, s, "b"); downloads(got) != "a by key/AKID presigned=true; a by user/albie presigned=false" || !timed(got, since) {
			t.Fatalf("after the first batch = %q", downloads(got))
		}
		if got := activity(t, s, "other"); downloads(got) != "x by anonymous presigned=false" || uploads(got) != "x by root" {
			t.Errorf("the other bucket = %+v", got)
		}
		// A key, a reader and the way they came have one row; the latest
		// three are kept, in the order they were handed in.
		add(t, s, "node-b", 1, readOf(b, "user/albie", false, 1, 0), readOf(a, "user/albie", false, 5, 0))
		if got := activity(t, s, "b"); downloads(got) != "a by user/albie presigned=false; b by user/albie presigned=false; a by key/AKID presigned=true" {
			t.Errorf("after the second batch = %q", downloads(got))
		}
		add(t, s, "node-a", 2, readOf(b, "root", false, 1, 0))
		want := "b by root presigned=false; a by user/albie presigned=false; b by user/albie presigned=false"
		if got := activity(t, s, "b"); downloads(got) != want || uploads(got) != "b by root; a by root" || !timed(got, since) {
			t.Errorf("after the third batch = %+v", got)
		}
		// A batch that is sent again is applied once: it does not come to
		// the front a second time.
		add(t, s, "node-b", 1, readOf(a, "key/AKID", true, 1, 0))
		if got := activity(t, s, "b"); downloads(got) != want {
			t.Errorf("after a batch that came twice = %q", downloads(got))
		}
	})

	t.Run("a read that cannot be recorded is no download", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		v := put(t, s, "b", "k", "root", 1)
		nowhere := readOf(v, "user/albie", false, 1, 0)
		nowhere.Bucket = "later"
		add(t, s, "node-a", 1,
			readOf(v, "", false, 1, 0),
			readOf(v, "user/a\nuser/b", false, 1, 0),
			readOf(v, strings.Repeat("u", 257), false, 1, 0),
			readOf(v, "user/albie", false, -1, 0),
			readOf(v, "user/albie", false, object.MaxReadCount+1, 0),
			nowhere,
		)
		if got := activity(t, s, "b"); len(got.Downloads) != 0 {
			t.Errorf("reads that cannot be recorded = %q", downloads(got))
		}
		// A read of a bucket there is not is nobody's - not that of a
		// bucket created under the name afterwards either.
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "later", CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got := activity(t, s, "later"); !got.Empty() {
			t.Errorf("a bucket created after a read named it = %+v", got)
		}
		// The download of a version that was replaced before it was
		// handed in is still a download from the bucket: bytes left it.
		put(t, s, "b", "k", "root", 2)
		add(t, s, "node-a", 2, readOf(v, "user/albie", false, 1, 0))
		if got := activity(t, s, "b"); downloads(got) != "k by user/albie presigned=false" {
			t.Errorf("the download of a version that was replaced = %q", downloads(got))
		}
	})

	t.Run("a bucket takes its record with it", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b", "other")
		v := put(t, s, "b", "k", "user/albie", 1)
		stays := put(t, s, "other", "k", "root", 2)
		add(t, s, "node-a", 1, readOf(v, "user/albie", false, 1, 0), readOf(stays, "root", false, 1, 0))
		if _, err := s.DeleteObject(ctx, "b", "k", ports.DeleteOptions{MarkerID: sampleVersionID(8), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got := activity(t, s, "b"); uploads(got) != "k by user/albie" || len(got.Downloads) != 1 {
			t.Fatalf("the record of an emptied bucket = %+v", got)
		}
		if err := s.DeleteBucket(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		if got := activity(t, s, "b"); !got.Empty() {
			t.Errorf("the record of a deleted bucket = %+v", got)
		}
		// A bucket created under the name of one that is gone begins
		// with nothing.
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got := activity(t, s, "b"); !got.Empty() {
			t.Errorf("a bucket created again has the record of the one before: %+v", got)
		}
		if got := activity(t, s, "other"); uploads(got) != "k by root" || downloads(got) != "k by root presigned=false" {
			t.Errorf("another bucket's record after the deletion = %+v", got)
		}
	})
}
