package porttest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// SampleVersion returns a fully populated object version for tests. fill
// varies the identifiers so that two samples differ.
// sampleVersionID is the version ID of a sample: the fill, sixteen times,
// which a test can name. It is never the zero ID, which no version has.
func sampleVersionID(fill byte) object.ID {
	id := object.ID(bytes.Repeat([]byte{fill}, object.IDSize))
	if id.IsZero() {
		id[object.IDSize-1] = 1
	}
	return id
}

// sampleDataID is the data ID of a sample. Stored data belongs to one
// version, so it is different for every bucket, key and fill.
func sampleDataID(bucketName, key string, fill byte) object.ID {
	sum := sha256.Sum256([]byte(bucketName + "\x00" + key + "\x00" + string(rune(fill))))
	return object.ID(sum[:object.IDSize])
}

// SampleVersion returns a version with every field set. Its version ID is
// the fill repeated; its data ID is its own.
func SampleVersion(bucketName, key string, fill byte) object.Version {
	return object.Version{
		Bucket:             bucketName,
		Key:                key,
		VersionID:          sampleVersionID(fill),
		DataID:             sampleDataID(bucketName, key, fill),
		Size:               int64(fill) * 1000,
		ETag:               "9e107d9d372bb6826bd81d3542a419d6",
		ModifiedAt:         time.Date(2026, 10, 7, 12, 30, 45, 123000000, time.UTC),
		ContentType:        "image/jpeg",
		ContentEncoding:    "gzip",
		ContentDisposition: `attachment; filename="a.jpg"`,
		ContentLanguage:    "en",
		CacheControl:       "no-cache",
		UserMetadata:       map[string]string{"owner": "albie", "empty": ""},
		Encryption: object.Encryption{
			Scheme:      object.StreamSchemeV1,
			KMSKeyID:    "local:test",
			WrappedKey:  bytes.Repeat([]byte{fill, 0xFF, 0x00}, 20),
			NoncePrefix: []byte{fill, 2, 3, 4},
			SegmentSize: object.DefaultSegmentSize,
		},
		Layout: object.Layout{
			DataShards:   1,
			ParityShards: 0,
			StripeSize:   1 << 20,
			StoredSize:   int64(fill)*1000 + 16,
			Shards:       []object.ShardLocation{{DriveID: "drive-a", Size: int64(fill)*1000 + 16}},
		},
	}
}

// MetadataContract is the behaviour every Metadata implementation must have.
// open returns a new, empty store.
func MetadataContract(t *testing.T, open func(t *testing.T) Metadata) {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	t.Run("buckets", func(t *testing.T) {
		store := open(t)

		if got, err := store.ListBuckets(ctx); err != nil || len(got) != 0 {
			t.Fatalf("ListBuckets on an empty store = %v, %v", got, err)
		}
		if _, err := store.GetBucket(ctx, "missing"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("GetBucket(missing) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
		if err := store.DeleteBucket(ctx, "missing"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("DeleteBucket(missing) error = %v, want %v", err, domain.ErrBucketNotFound)
		}

		for _, name := range []string{"zebra", "alpha", "mango"} {
			if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: created}); err != nil {
				t.Fatalf("CreateBucket(%s): %v", name, err)
			}
		}
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "alpha", CreatedAt: created.Add(time.Hour)}); !errors.Is(err, domain.ErrBucketAlreadyExists) {
			t.Errorf("CreateBucket(duplicate) error = %v, want %v", err, domain.ErrBucketAlreadyExists)
		}

		got, err := store.GetBucket(ctx, "alpha")
		if err != nil || got.Name != "alpha" || !got.CreatedAt.Equal(created) {
			t.Errorf("GetBucket(alpha) = %+v, %v; the duplicate create must not have changed it", got, err)
		}
		list, err := store.ListBuckets(ctx)
		if err != nil || len(list) != 3 || list[0].Name != "alpha" || list[1].Name != "mango" || list[2].Name != "zebra" {
			t.Errorf("ListBuckets = %+v, %v; want alpha, mango, zebra", list, err)
		}

		if _, err := store.PutVersion(ctx, SampleVersion("mango", "k", 1)); err != nil {
			t.Fatalf("PutVersion: %v", err)
		}
		if err := store.DeleteBucket(ctx, "mango"); !errors.Is(err, domain.ErrBucketNotEmpty) {
			t.Errorf("DeleteBucket(non-empty) error = %v, want %v", err, domain.ErrBucketNotEmpty)
		}
		if _, err := store.DeleteObject(ctx, "mango", "k", ports.DeleteOptions{}); err != nil {
			t.Fatalf("DeleteObject: %v", err)
		}
		if err := store.DeleteBucket(ctx, "mango"); err != nil {
			t.Errorf("DeleteBucket(emptied) = %v", err)
		}
		if _, err := store.GetBucket(ctx, "mango"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("GetBucket(deleted) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
		// A deleted bucket can be created again and starts empty.
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "mango", CreatedAt: created}); err != nil {
			t.Fatalf("CreateBucket(recreated): %v", err)
		}
		if _, err := store.GetObject(ctx, "mango", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("GetObject in a recreated bucket error = %v, want %v", err, domain.ErrObjectNotFound)
		}
	})

	t.Run("objects", func(t *testing.T) {
		store := open(t)
		for _, name := range []string{"one", "two"} {
			if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: created}); err != nil {
				t.Fatalf("CreateBucket: %v", err)
			}
		}

		if _, err := store.PutVersion(ctx, SampleVersion("missing", "k", 1)); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("PutVersion(missing bucket) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
		if _, err := store.GetObject(ctx, "missing", "k"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("GetObject(missing bucket) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
		if _, err := store.DeleteObject(ctx, "missing", "k", ports.DeleteOptions{}); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("DeleteObject(missing bucket) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
		if _, err := store.GetObject(ctx, "one", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("GetObject(missing key) error = %v, want %v", err, domain.ErrObjectNotFound)
		}
		if removed, err := store.DeleteObject(ctx, "one", "k", ports.DeleteOptions{}); err != nil || removed.Removed != nil || removed.Marker != nil {
			t.Errorf("DeleteObject(missing key) = %v, %v; want nil, nil", removed, err)
		}

		first := SampleVersion("one", "k", 1)
		replaced, err := store.PutVersion(ctx, first)
		if err != nil || replaced.Removed != nil || replaced.Stored.Versioned || replaced.Stored.DataID != first.DataID {
			t.Fatalf("first PutVersion = %v, %v; want nil, nil", replaced, err)
		}
		got, err := store.GetObject(ctx, "one", "k")
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		if !versionsEqual(got, first) {
			t.Errorf("GetObject returned\n%+v\nwant\n%+v", got, first)
		}

		second := SampleVersion("one", "k", 5)
		replaced, err = store.PutVersion(ctx, second)
		if err != nil || replaced.Removed == nil || replaced.Removed.DataID != first.DataID {
			t.Fatalf("second PutVersion = %+v, %v; want the first version back", replaced, err)
		}
		if got, err := store.GetObject(ctx, "one", "k"); err != nil || got.DataID != second.DataID {
			t.Errorf("GetObject after overwrite = %+v, %v", got, err)
		}

		// The same key in another bucket is another object.
		if replaced, err := store.PutVersion(ctx, SampleVersion("two", "k", 9)); err != nil || replaced.Removed != nil {
			t.Errorf("PutVersion in another bucket = %v, %v; want nil, nil", replaced, err)
		}

		removed, err := store.DeleteObject(ctx, "one", "k", ports.DeleteOptions{})
		if err != nil || removed.Removed == nil || removed.Removed.DataID != second.DataID || removed.Marker != nil {
			t.Fatalf("DeleteObject = %+v, %v; want the current version back", removed, err)
		}
		if _, err := store.GetObject(ctx, "one", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("GetObject after delete error = %v, want %v", err, domain.ErrObjectNotFound)
		}
		if got, err := store.GetObject(ctx, "two", "k"); err != nil || got.Bucket != "two" {
			t.Errorf("the object in the other bucket was affected: %+v, %v", got, err)
		}
	})

	t.Run("keys are opaque", func(t *testing.T) {
		store := open(t)
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: created}); err != nil {
			t.Fatalf("CreateBucket: %v", err)
		}
		keys := []string{"../../etc/passwd", "a\x00b", "a/", "a", "a//b", "CON", "ünï/cødé", "a\xffz"[:1] + "~"}
		for i, key := range keys {
			if _, err := store.PutVersion(ctx, SampleVersion("b", key, byte(i+1))); err != nil {
				t.Fatalf("PutVersion(%q): %v", key, err)
			}
		}
		for i, key := range keys {
			got, err := store.GetObject(ctx, "b", key)
			if err != nil || got.Key != key || got.VersionID != SampleVersion("b", key, byte(i+1)).VersionID {
				t.Errorf("GetObject(%q) = %+v, %v", key, got, err)
			}
		}
	})

	t.Run("scan", func(t *testing.T) {
		store := open(t)
		for _, name := range []string{"b", "other"} {
			if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: created}); err != nil {
				t.Fatalf("CreateBucket: %v", err)
			}
		}
		for i, key := range []string{"d", "a", "c/2", "c/1", "b", "e"} {
			if _, err := store.PutVersion(ctx, SampleVersion("b", key, byte(i+1))); err != nil {
				t.Fatalf("PutVersion: %v", err)
			}
		}
		if _, err := store.PutVersion(ctx, SampleVersion("other", "zz", 1)); err != nil {
			t.Fatalf("PutVersion: %v", err)
		}

		scan := func(start string, control func(key string) ports.ScanControl) []string {
			var keys []string
			err := store.ScanObjects(ctx, "b", start, func(v object.Version) ports.ScanControl {
				if v.Bucket != "b" {
					t.Errorf("scan of b visited an object of bucket %q", v.Bucket)
				}
				keys = append(keys, v.Key)
				return control(v.Key)
			})
			if err != nil {
				t.Fatalf("ScanObjects: %v", err)
			}
			return keys
		}
		proceed := func(string) ports.ScanControl { return ports.ScanControl{} }

		tests := []struct {
			name    string
			start   string
			control func(string) ports.ScanControl
			want    []string
		}{
			{"everything in key order", "", proceed, []string{"a", "b", "c/1", "c/2", "d", "e"}},
			{"start is inclusive", "c/1", proceed, []string{"c/1", "c/2", "d", "e"}},
			{"start between keys", "c", proceed, []string{"c/1", "c/2", "d", "e"}},
			{"start past the end", "z", proceed, nil},
			{"stop", "", func(k string) ports.ScanControl { return ports.ScanControl{Stop: k == "b"} }, []string{"a", "b"}},
			{"seek skips keys", "", func(k string) ports.ScanControl {
				if k == "c/1" {
					return ports.ScanControl{SeekTo: "c0"}
				}
				return ports.ScanControl{}
			}, []string{"a", "b", "c/1", "d", "e"}},
			{"seek past the end", "", func(k string) ports.ScanControl {
				if k == "a" {
					return ports.ScanControl{SeekTo: "zzz"}
				}
				return ports.ScanControl{}
			}, []string{"a"}},
		}
		for _, tt := range tests {
			if got := scan(tt.start, tt.control); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("%s: visited %q, want %q", tt.name, got, tt.want)
			}
		}

		if err := store.ScanObjects(ctx, "missing", "", func(object.Version) ports.ScanControl { return ports.ScanControl{} }); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ScanObjects(missing bucket) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
	})

	t.Run("data references", func(t *testing.T) {
		store := open(t)
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: created}); err != nil {
			t.Fatalf("CreateBucket: %v", err)
		}
		first, second := SampleVersion("b", "k", 1), SampleVersion("b", "k", 5)
		referenced := func(v object.Version) bool {
			t.Helper()
			ok, err := store.IsDataReferenced(ctx, v.DataID)
			if err != nil {
				t.Fatalf("IsDataReferenced: %v", err)
			}
			return ok
		}

		if referenced(first) {
			t.Error("data is referenced before any version exists")
		}
		if _, err := store.PutVersion(ctx, first); err != nil {
			t.Fatal(err)
		}
		if !referenced(first) {
			t.Error("the data of the current version is not referenced")
		}
		// An overwrite releases the old data and holds the new.
		if _, err := store.PutVersion(ctx, second); err != nil {
			t.Fatal(err)
		}
		if referenced(first) || !referenced(second) {
			t.Errorf("after overwrite: old referenced = %v, new referenced = %v", referenced(first), referenced(second))
		}
		// Stored data belongs to one version. A second version that named
		// it could have it removed from under the first, by being
		// overwritten or deleted itself; such a put is refused.
		other := second
		other.Key = "other"
		other.VersionID = object.ID(bytes.Repeat([]byte{9}, object.IDSize))
		if _, err := store.PutVersion(ctx, other); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("a put that names the data of another version = %v, want %v", err, domain.ErrConflict)
		}
		if _, err := store.GetObject(ctx, "b", "other"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("the refused put left an object: %v", err)
		}
		// Nor is an ID that is not one, or a key that is not one, stored.
		for name, change := range map[string]func(v *object.Version){
			"no data ID":    func(v *object.Version) { v.DataID = object.ID{} },
			"no version ID": func(v *object.Version) { v.VersionID = object.ID{} },
			"no key":        func(v *object.Version) { v.Key = "" },
		} {
			bad := SampleVersion("b", "bad", 7)
			change(&bad)
			if _, err := store.PutVersion(ctx, bad); !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrInvalidObjectKey) {
				t.Errorf("a put with %s = %v, want it refused as invalid", name, err)
			}
		}
		// The store still works after what it refused.
		if _, err := store.DeleteObject(ctx, "b", "k", ports.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if referenced(second) {
			t.Error("data is still referenced after its last version was deleted")
		}
	})

	t.Run("versions", func(t *testing.T) { versionsContract(t, open(t)) })
	t.Run("object lock", func(t *testing.T) { lockContract(t, open(t)) })
	t.Run("multipart uploads", func(t *testing.T) { uploadsContract(t, open) })
	t.Run("bucket configuration and tags", func(t *testing.T) {
		store := open(t)
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if got, err := store.GetBucketConfig(ctx, "b", bucket.ConfigPolicy); err != nil || got != nil {
			t.Errorf("configuration of a new bucket = %q, %v", got, err)
		}
		for kind, document := range map[string]string{bucket.ConfigPolicy: `{"a":1}`, bucket.ConfigCORS: `{"rules":[]}`, bucket.ConfigTagging: `{"k":"v"}`} {
			if err := store.SetBucketConfig(ctx, "b", kind, []byte(document)); err != nil {
				t.Fatalf("SetBucketConfig %s: %v", kind, err)
			}
		}
		if got, err := store.GetBucketConfig(ctx, "b", bucket.ConfigCORS); err != nil || string(got) != `{"rules":[]}` {
			t.Errorf("GetBucketConfig = %q, %v", got, err)
		}
		// One kind is replaced and removed without touching the others.
		if err := store.SetBucketConfig(ctx, "b", bucket.ConfigCORS, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.GetBucketConfig(ctx, "b", bucket.ConfigCORS); got != nil {
			t.Errorf("a removed configuration = %q", got)
		}
		if got, _ := store.GetBucketConfig(ctx, "b", bucket.ConfigPolicy); string(got) != `{"a":1}` {
			t.Errorf("the policy after the CORS rules were removed = %q", got)
		}
		// The bucket itself is as it was.
		if b, err := store.GetBucket(ctx, "b"); err != nil || b.Name != "b" || !b.CreatedAt.Equal(created) {
			t.Errorf("the bucket after its configuration changed = %+v, %v", b, err)
		}
		for name, tt := range map[string]struct {
			bucketName, kind string
			document         []byte
			want             error
		}{
			"a missing bucket":    {"missing", bucket.ConfigPolicy, []byte("x"), domain.ErrBucketNotFound},
			"a kind there is not": {"b", "lifecycle-of-another-day", []byte("x"), domain.ErrInvalidArgument},
			"too large":           {"b", bucket.ConfigPolicy, bytes.Repeat([]byte("x"), bucket.MaxConfigBytes+1), domain.ErrInvalidArgument},
		} {
			if err := store.SetBucketConfig(ctx, tt.bucketName, tt.kind, tt.document); !errors.Is(err, tt.want) {
				t.Errorf("SetBucketConfig with %s = %v, want %v", name, err, tt.want)
			}
		}
		if _, err := store.GetBucketConfig(ctx, "missing", bucket.ConfigPolicy); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("GetBucketConfig of a missing bucket = %v", err)
		}

		// Tags of a version, in a bucket without Object Lock.
		v := SampleVersion("b", "k", 1)
		v.Tags = map[string]string{"born": "tagged"}
		if _, err := store.PutVersion(ctx, v); err != nil {
			t.Fatal(err)
		}
		if got, err := store.GetObject(ctx, "b", "k"); err != nil || got.Tags["born"] != "tagged" {
			t.Errorf("tags of a version put with them = %v, %v", got.Tags, err)
		}
		tagged, err := store.SetTags(ctx, "b", "k", v.Ref(), map[string]string{"stage": "final"})
		if err != nil || len(tagged.Tags) != 1 || tagged.Tags["stage"] != "final" {
			t.Fatalf("SetTags = %v, %v", tagged.Tags, err)
		}
		if got, _ := store.GetObject(ctx, "b", "k"); got.Tags["stage"] != "final" || got.DataID != v.DataID || got.ETag != v.ETag {
			t.Errorf("the version after tagging = %+v", got)
		}
		if cleared, err := store.SetTags(ctx, "b", "k", v.Ref(), nil); err != nil || len(cleared.Tags) != 0 {
			t.Errorf("SetTags to none = %v, %v", cleared.Tags, err)
		}
		eleven := map[string]string{}
		for i := range 11 {
			eleven[strconv.Itoa(i)] = ""
		}
		if _, err := store.SetTags(ctx, "b", "k", v.Ref(), eleven); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("eleven tags = %v", err)
		}
		if _, err := store.SetTags(ctx, "b", "missing", v.Ref(), nil); !errors.Is(err, domain.ErrVersionNotFound) {
			t.Errorf("SetTags of a missing version = %v", err)
		}
		// The configuration goes with the bucket.
		if _, err := store.DeleteObject(ctx, "b", "k", ports.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteBucket(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.GetBucketConfig(ctx, "b", bucket.ConfigPolicy); got != nil {
			t.Errorf("a policy outlived its bucket: %q", got)
		}
	})
	t.Run("a delete on a condition", func(t *testing.T) {
		// Whoever decided on a reading of the state that a version is to
		// go says which version it read; if another has taken its place,
		// nothing is deleted.
		store := open(t)
		for _, b := range []bucket.Bucket{{Name: "plain"}, {Name: "ver"}} {
			if err := store.CreateBucket(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		marker := func(fill byte) ports.DeleteOptions {
			return ports.DeleteOptions{MarkerID: object.ID(bytes.Repeat([]byte{fill}, object.IDSize)), At: created}
		}
		old, fresh := SampleVersion("plain", "k", 1), SampleVersion("plain", "k", 2)
		if _, err := store.PutVersion(ctx, old); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutVersion(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		// Decided for the old version; the key was written again since.
		late := marker(40)
		late.Expect = old.VersionID
		if _, err := store.DeleteObject(ctx, "plain", "k", late); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("a delete decided for a version that was replaced = %v, want %v", err, domain.ErrConflict)
		}
		if got, err := store.GetObject(ctx, "plain", "k"); err != nil || got.VersionID != fresh.VersionID {
			t.Fatalf("the refused delete removed the object: %v", err)
		}
		// By name, too: "null" names whatever the null version is now.
		null := object.VersionRef{Null: true}
		late.Version = &null
		if _, err := store.DeleteObject(ctx, "plain", "k", late); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a delete of version null decided for another null version = %v", err)
		}
		right := marker(41)
		right.Version, right.Expect = &null, fresh.VersionID
		if result, err := store.DeleteObject(ctx, "plain", "k", right); err != nil || result.Removed == nil || result.Removed.VersionID != fresh.VersionID {
			t.Errorf("a delete decided for the version that is there = %+v, %v", result, err)
		}
		// Nothing there at all is not what was expected either.
		gone := marker(42)
		gone.Expect = fresh.VersionID
		if _, err := store.DeleteObject(ctx, "plain", "k", gone); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a conditional delete of a key that is gone = %v", err)
		}

		// Where versions are kept: no marker over a version that was not
		// the one judged, and a marker is itself something to expect.
		first, err := store.PutVersion(ctx, SampleVersion("ver", "k", 3))
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.PutVersion(ctx, SampleVersion("ver", "k", 4))
		if err != nil {
			t.Fatal(err)
		}
		over := marker(43)
		over.Expect = first.Stored.VersionID
		if _, err := store.DeleteObject(ctx, "ver", "k", over); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a marker over a version that was not the one judged = %v", err)
		}
		over.Expect = second.Stored.VersionID
		made, err := store.DeleteObject(ctx, "ver", "k", over)
		if err != nil || made.Marker == nil {
			t.Fatalf("a marker over the version that was judged = %+v, %v", made, err)
		}
		ref := made.Marker.Ref()
		wrong := marker(44)
		wrong.Version, wrong.Expect = &ref, second.Stored.VersionID
		if _, err := store.DeleteObject(ctx, "ver", "k", wrong); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("removing a marker expected to be something else = %v", err)
		}
		// The newest version is the marker: a delete without a version
		// that expects it is carried out, and makes another.
		again := marker(45)
		again.Expect = made.Marker.VersionID
		if result, err := store.DeleteObject(ctx, "ver", "k", again); err != nil || result.Marker == nil {
			t.Errorf("a delete expecting the marker that is newest = %+v, %v", result, err)
		}
		// An older version, by name, with the right expectation.
		firstRef := first.Stored.Ref()
		older := marker(46)
		older.Version, older.Expect = &firstRef, first.Stored.VersionID
		if result, err := store.DeleteObject(ctx, "ver", "k", older); err != nil || result.Removed == nil {
			t.Errorf("removing an older version that is the one expected = %+v, %v", result, err)
		}
	})
	t.Run("whose key it takes", func(t *testing.T) {
		// What a version records about its encryption comes back as it
		// went in, for every mode.
		store := open(t)
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		for i, change := range []func(e *object.Encryption){
			func(e *object.Encryption) { e.Mode, e.Context = object.SSEKMS, []byte(`{"team":"finance"}`) },
			func(e *object.Encryption) {
				e.Mode, e.KeyCheck = object.SSEC, bytes.Repeat([]byte{7}, 16)
			},
		} {
			v := SampleVersion("b", "k"+strconv.Itoa(i), byte(i+1))
			change(&v.Encryption)
			if _, err := store.PutVersion(ctx, v); err != nil {
				t.Fatalf("PutVersion: %v", err)
			}
			got, err := store.GetObject(ctx, "b", v.Key)
			if err != nil || !reflect.DeepEqual(got.Encryption, v.Encryption) {
				t.Errorf("encryption of %s = %+v, %v; want %+v", v.Key, got.Encryption, err, v.Encryption)
			}
		}
	})

	t.Run("topology", func(t *testing.T) {
		store := open(t)
		if _, err := store.GetTopology(ctx); !errors.Is(err, domain.ErrNoTopology) {
			t.Errorf("GetTopology on a new store error = %v, want %v", err, domain.ErrNoTopology)
		}
		first, err := cluster.NewTopology([]string{"d0", "d1", "d2", "d3"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetTopology(ctx, cluster.Topology{}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("SetTopology(empty) error = %v, want %v", err, domain.ErrInvalidArgument)
		}
		first.DeploymentID = "deployment-1"
		if err := store.SetTopology(ctx, first); err != nil {
			t.Fatalf("SetTopology: %v", err)
		}
		stored, err := store.GetTopology(ctx)
		if err != nil || stored.Version != 1 || stored.DeploymentID != "deployment-1" || !reflect.DeepEqual(stored.DriveIDs(), first.DriveIDs()) {
			t.Fatalf("GetTopology = %+v, %v", stored, err)
		}
		// A change based on the state before the first one is refused.
		if err := store.SetTopology(ctx, first); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("SetTopology(stale) error = %v, want %v", err, domain.ErrConflict)
		}
		replaced := stored.Replace(cluster.Replacement{Position: cluster.Position{Set: 0, Slot: 2}, Old: "d2", New: "d9"})
		if err := store.SetTopology(ctx, replaced); err != nil {
			t.Fatalf("SetTopology(replacement): %v", err)
		}
		got, err := store.GetTopology(ctx)
		if err != nil || got.Version != 2 {
			t.Fatalf("GetTopology = %+v, %v", got, err)
		}
		if current, ok := got.Current("d2"); !ok || current != "d9" {
			t.Errorf("Current(d2) = %q, %v; want d9", current, ok)
		}
		if !reflect.DeepEqual(got.Sets[0][2].Retired, []string{"d2"}) {
			t.Errorf("retired drives = %v", got.Sets[0][2].Retired)
		}
	})

	t.Run("root credential", func(t *testing.T) {
		store := open(t)
		if _, err := store.GetRootCredential(ctx); !errors.Is(err, domain.ErrNotBootstrapped) {
			t.Errorf("GetRootCredential on a new store error = %v, want %v", err, domain.ErrNotBootstrapped)
		}
		want := iam.RootCredential{
			AccessKeyID: "ROOTKEY",
			Secret: iam.SealedSecret{
				KMSKeyID: "local:test", WrappedKey: []byte{1, 2, 3}, Nonce: []byte{4, 5}, Ciphertext: []byte{6, 0, 255},
			},
			CreatedAt: created,
		}
		if err := store.SetRootCredential(ctx, want); err != nil {
			t.Fatalf("SetRootCredential: %v", err)
		}
		got, err := store.GetRootCredential(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("GetRootCredential = %+v, %v; want %+v", got, err, want)
		}
		other := want
		other.AccessKeyID = "ATTACKER"
		if err := store.SetRootCredential(ctx, other); !errors.Is(err, domain.ErrAlreadyBootstrapped) {
			t.Errorf("second SetRootCredential error = %v, want %v", err, domain.ErrAlreadyBootstrapped)
		}
		if got, err := store.GetRootCredential(ctx); err != nil || got.AccessKeyID != "ROOTKEY" {
			t.Errorf("the root credential was replaced: %+v, %v", got, err)
		}
	})
}

func versionsEqual(a, b object.Version) bool {
	if !a.ModifiedAt.Equal(b.ModifiedAt) {
		return false
	}
	a.ModifiedAt, b.ModifiedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// ShardStoreContract is the behaviour every ports.ShardStore implementation
// must have. open returns a new, empty store.
func ShardStoreContract(t *testing.T, open func(t *testing.T) ports.ShardStore) {
	t.Helper()
	ctx := context.Background()
	shard := func(fill byte, index int) object.ShardID {
		return object.ShardID{DataID: object.ID(bytes.Repeat([]byte{fill}, object.IDSize)), Index: index}
	}
	write := func(t *testing.T, store ports.ShardStore, id object.ShardID, data []byte) {
		t.Helper()
		w, err := store.Create(ctx, id)
		if err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
		// Write in uneven pieces to cross internal block boundaries.
		for rest := data; len(rest) > 0; {
			n := min(len(rest), 7919)
			if _, err := w.Write(rest[:n]); err != nil {
				t.Fatalf("Write: %v", err)
			}
			rest = rest[n:]
		}
		if err := w.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	read := func(t *testing.T, store ports.ShardStore, id object.ShardID) []byte {
		t.Helper()
		r, err := store.Open(ctx, id)
		if err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
		defer func() { _ = r.Close() }()
		data, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return data
	}

	t.Run("drive ID", func(t *testing.T) {
		store := open(t)
		first, second := store.DriveID(), store.DriveID()
		if first == "" || first != second {
			t.Errorf("DriveID() = %q then %q, want a stable, non-empty identifier", first, second)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		store := open(t)
		for i, size := range []int{0, 1, 4095, 4096, 4097, 300_000, 1<<20 + 13} {
			data := make([]byte, size)
			if _, err := rand.Read(data); err != nil {
				t.Fatalf("rand: %v", err)
			}
			id := shard(byte(i+1), i)
			write(t, store, id, data)

			r, err := store.Open(ctx, id)
			if err != nil {
				t.Fatalf("size %d: Open: %v", size, err)
			}
			if r.Size() != int64(size) {
				t.Errorf("size %d: Size() = %d", size, r.Size())
			}
			if err := r.Close(); err != nil {
				t.Errorf("size %d: Close: %v", size, err)
			}
			if got := read(t, store, id); !bytes.Equal(got, data) {
				t.Errorf("size %d: read back different bytes", size)
			}
		}
	})

	t.Run("random access", func(t *testing.T) {
		store := open(t)
		data := make([]byte, 700_001)
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("rand: %v", err)
		}
		id := shard(1, 0)
		write(t, store, id, data)
		r, err := store.Open(ctx, id)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = r.Close() }()

		for _, span := range [][2]int{{0, 1}, {0, 10}, {65535, 3}, {131071, 2}, {300_000, 200_000}, {700_000, 1}, {123, 699_878}} {
			buf := make([]byte, span[1])
			n, err := r.ReadAt(buf, int64(span[0]))
			if err != nil || n != span[1] || !bytes.Equal(buf, data[span[0]:span[0]+span[1]]) {
				t.Errorf("ReadAt(len %d, off %d) = %d, %v or wrong bytes", span[1], span[0], n, err)
			}
		}

		buf := make([]byte, 10)
		if n, err := r.ReadAt(buf, int64(len(data))-4); n != 4 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:4], data[len(data)-4:]) {
			t.Errorf("ReadAt across the end = %d, %v; want 4, io.EOF", n, err)
		}
		if n, err := r.ReadAt(buf, int64(len(data))); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt at the end = %d, %v; want 0, io.EOF", n, err)
		}
		if _, err := r.ReadAt(buf, -1); err == nil {
			t.Error("ReadAt with a negative offset returned nil error")
		}
	})

	t.Run("a shard is invisible until committed", func(t *testing.T) {
		store := open(t)
		id := shard(1, 0)
		w, err := store.Create(ctx, id)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.Write([]byte("pending")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := store.Open(ctx, id); !errors.Is(err, domain.ErrShardNotFound) {
			t.Errorf("Open before Commit error = %v, want %v", err, domain.ErrShardNotFound)
		}
		if err := w.Abort(); err != nil {
			t.Errorf("Abort: %v", err)
		}
		if _, err := store.Open(ctx, id); !errors.Is(err, domain.ErrShardNotFound) {
			t.Errorf("Open after Abort error = %v, want %v", err, domain.ErrShardNotFound)
		}
		// The identifier is free again after an abort.
		write(t, store, id, []byte("second attempt"))
		if got := read(t, store, id); string(got) != "second attempt" {
			t.Errorf("read %q after abort and rewrite", got)
		}
	})

	t.Run("shards are immutable", func(t *testing.T) {
		store := open(t)
		id := shard(1, 0)
		write(t, store, id, []byte("original"))

		w, err := store.Create(ctx, id)
		if err == nil {
			_, _ = w.Write([]byte("overwrite"))
			err = w.Commit(ctx)
			_ = w.Abort()
		}
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("writing an existing shard: error = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if got := read(t, store, id); string(got) != "original" {
			t.Errorf("the shard now holds %q", got)
		}
	})

	t.Run("abort after commit does nothing", func(t *testing.T) {
		store := open(t)
		id := shard(1, 0)
		w, err := store.Create(ctx, id)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.Write([]byte("kept")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := w.Abort(); err != nil {
			t.Errorf("Abort after Commit: %v", err)
		}
		if got := read(t, store, id); string(got) != "kept" {
			t.Errorf("Abort after Commit removed the shard: read %q", got)
		}
	})

	t.Run("shards are independent", func(t *testing.T) {
		store := open(t)
		write(t, store, shard(1, 0), []byte("one-zero"))
		write(t, store, shard(1, 1), []byte("one-one"))
		write(t, store, shard(2, 0), []byte("two-zero"))

		if err := store.Delete(ctx, shard(1, 0)); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.Open(ctx, shard(1, 0)); !errors.Is(err, domain.ErrShardNotFound) {
			t.Errorf("Open(deleted) error = %v, want %v", err, domain.ErrShardNotFound)
		}
		if got := read(t, store, shard(1, 1)); string(got) != "one-one" {
			t.Errorf("shard 1.1 = %q", got)
		}
		if got := read(t, store, shard(2, 0)); string(got) != "two-zero" {
			t.Errorf("shard 2.0 = %q", got)
		}
		if err := store.Delete(ctx, shard(1, 0)); err != nil {
			t.Errorf("Delete(already deleted) = %v, want nil", err)
		}
		if err := store.Delete(ctx, shard(9, 9)); err != nil {
			t.Errorf("Delete(never existed) = %v, want nil", err)
		}
	})

	t.Run("rewrite replaces in one step", func(t *testing.T) {
		store := open(t)
		id := shard(1, 0)
		write(t, store, id, []byte("damaged content"))

		w, err := store.Rewrite(ctx, id)
		if err != nil {
			t.Fatalf("Rewrite: %v", err)
		}
		if _, err := w.Write([]byte("rebuilt")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// Until the commit the old content is what readers get.
		if got := read(t, store, id); string(got) != "damaged content" {
			t.Errorf("before Commit the shard reads %q", got)
		}
		if err := w.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if got := read(t, store, id); string(got) != "rebuilt" {
			t.Errorf("after Commit the shard reads %q", got)
		}

		// An aborted rewrite leaves the shard as it was.
		w, err = store.Rewrite(ctx, id)
		if err != nil {
			t.Fatalf("Rewrite: %v", err)
		}
		if _, err := w.Write([]byte("never committed")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Abort(); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		if got := read(t, store, id); string(got) != "rebuilt" {
			t.Errorf("after an aborted rewrite the shard reads %q", got)
		}

		// Rewriting a shard that does not exist creates it.
		missing := shard(2, 0)
		w, err = store.Rewrite(ctx, missing)
		if err != nil {
			t.Fatalf("Rewrite(missing): %v", err)
		}
		if _, err := w.Write([]byte("created")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if got := read(t, store, missing); string(got) != "created" {
			t.Errorf("a rewritten missing shard reads %q", got)
		}
	})

	t.Run("list", func(t *testing.T) {
		store := open(t)
		listed := func() map[object.ShardID]ports.ShardInfo {
			t.Helper()
			out := map[object.ShardID]ports.ShardInfo{}
			err := store.List(ctx, func(info ports.ShardInfo) error {
				if _, dup := out[info.ID]; dup {
					t.Errorf("shard %s was listed twice", info.ID)
				}
				out[info.ID] = info
				return nil
			})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			return out
		}
		if got := listed(); len(got) != 0 {
			t.Fatalf("an empty store lists %d shards", len(got))
		}

		before := time.Now().Add(-time.Minute)
		want := []object.ShardID{shard(1, 0), shard(1, 5), shard(2, 0), shard(0xAB, 255)}
		for _, id := range want {
			write(t, store, id, []byte("data"))
		}
		// A shard being written is not listed.
		pending, err := store.Create(ctx, shard(9, 0))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pending.Abort() }()

		got := listed()
		if len(got) != len(want) {
			t.Fatalf("listed %d shards, want %d: %v", len(got), len(want), got)
		}
		for _, id := range want {
			info, ok := got[id]
			if !ok {
				t.Errorf("shard %s is not listed", id)
				continue
			}
			if info.ModifiedAt.Before(before) || info.ModifiedAt.After(time.Now().Add(time.Minute)) {
				t.Errorf("shard %s: ModifiedAt = %v, want about now", id, info.ModifiedAt)
			}
		}

		// The visitor may delete what it is shown, and may stop the listing.
		boom := errors.New("stop")
		visits := 0
		err = store.List(ctx, func(info ports.ShardInfo) error {
			visits++
			if err := store.Delete(ctx, info.ID); err != nil {
				t.Errorf("Delete during List: %v", err)
			}
			if visits == 2 {
				return boom
			}
			return nil
		})
		if !errors.Is(err, boom) || visits != 2 {
			t.Errorf("List stopped by its visitor: %d visits, error %v", visits, err)
		}
		if got := listed(); len(got) != len(want)-2 {
			t.Errorf("%d shards after deleting two during a listing, want %d", len(got), len(want)-2)
		}

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := store.List(cancelled, func(ports.ShardInfo) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Errorf("List with a cancelled context: %v", err)
		}
	})

	t.Run("open missing", func(t *testing.T) {
		store := open(t)
		if _, err := store.Open(ctx, shard(7, 0)); !errors.Is(err, domain.ErrShardNotFound) {
			t.Errorf("Open(missing) error = %v, want %v", err, domain.ErrShardNotFound)
		}
	})
}

// listing is what ScanVersions shows of a key: each version as "ref" or
// "ref(marker)", newest first, the latest marked with a star.
func listing(t *testing.T, store Metadata, bucketName, start string) []string {
	t.Helper()
	var out []string
	err := store.ScanVersions(context.Background(), bucketName, ports.VersionCursor{Key: start}, func(v object.Version, latest bool) ports.ScanControl {
		entry := v.Key + "@" + v.Ref().String()
		if !v.Ref().Null {
			entry = v.Key + "@" + v.Ref().String()[:2]
		}
		if v.DeleteMarker {
			entry += "(marker)"
		}
		if latest {
			entry += "*"
		}
		out = append(out, entry)
		return ports.ScanControl{}
	})
	if err != nil {
		t.Fatalf("ScanVersions: %v", err)
	}
	return out
}

// versionsContract is what a store must do with the versions of objects.
func versionsContract(t *testing.T, store Metadata) {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "v", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	put := func(key string, fill byte) ports.PutResult {
		t.Helper()
		result, err := store.PutVersion(ctx, SampleVersion("v", key, fill))
		if err != nil {
			t.Fatalf("PutVersion(%s, %d): %v", key, fill, err)
		}
		return result
	}
	del := func(key string, ref *object.VersionRef, marker byte) ports.DeleteResult {
		t.Helper()
		result, err := store.DeleteObject(ctx, "v", key, ports.DeleteOptions{
			Version: ref, MarkerID: object.ID(bytes.Repeat([]byte{marker}, object.IDSize)), At: created.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("DeleteObject(%s): %v", key, err)
		}
		return result
	}
	want := func(what string, wantList ...string) {
		t.Helper()
		if got := listing(t, store, "v", ""); !slices.Equal(got, wantList) {
			t.Fatalf("%s: versions are\n  %v\nwant\n  %v", what, got, wantList)
		}
	}
	current := func(key string) (object.Version, error) { return store.GetObject(ctx, "v", key) }
	id := func(fill byte) *object.VersionRef {
		return &object.VersionRef{ID: object.ID(bytes.Repeat([]byte{fill}, object.IDSize))}
	}
	null := &object.VersionRef{Null: true}
	referenced := func(fill byte) bool {
		t.Helper()
		ok, err := store.IsDataReferenced(ctx, SampleVersion("v", "k", fill).DataID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// A bucket that never kept versions: one null version per key, and a
	// delete removes it.
	if r := put("k", 0x10); r.Stored.Versioned || r.Removed != nil {
		t.Fatalf("put into an unversioned bucket = %+v", r)
	}
	want("unversioned", "k@null*")
	for _, state := range []bucket.Versioning{bucket.Unversioned, "On", "enabled"} {
		if err := store.SetBucketVersioning(ctx, "v", state); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("SetBucketVersioning(%q) = %v, want %v", state, err, domain.ErrInvalidArgument)
		}
	}
	if err := store.SetBucketVersioning(ctx, "nope", bucket.VersioningEnabled); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("SetBucketVersioning of a missing bucket = %v", err)
	}

	// Enabled: what was there stays, as the null version; every put is a
	// version of its own; nothing is removed.
	if err := store.SetBucketVersioning(ctx, "v", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if b, err := store.GetBucket(ctx, "v"); err != nil || b.Versioning != bucket.VersioningEnabled {
		t.Fatalf("GetBucket after enabling = %+v, %v", b, err)
	}
	r := put("k", 0x20)
	if !r.Stored.Versioned || r.Removed != nil || r.Stored.Seq == 0 || r.Stored.VersionID != id(0x20).ID {
		t.Fatalf("put into a versioned bucket = %+v", r)
	}
	put("k", 0x30)
	want("two versions over a null one", "k@30*", "k@20", "k@null")
	if v, err := current("k"); err != nil || v.VersionID != id(0x30).ID {
		t.Fatalf("current = %+v, %v", v, err)
	}
	for _, ref := range []*object.VersionRef{id(0x20), id(0x30), null} {
		v, err := store.GetVersion(ctx, "v", "k", *ref)
		if err != nil || !v.Is(*ref) || v.DeleteMarker {
			t.Errorf("GetVersion(%s) = %+v, %v", ref, v, err)
		}
	}
	if _, err := store.GetVersion(ctx, "v", "k", *id(0x99)); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Errorf("GetVersion of an unknown ID = %v", err)
	}
	if _, err := store.GetVersion(ctx, "nope", "k", *null); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("GetVersion in a missing bucket = %v", err)
	}

	// An ID a key already has is not given to another version of it.
	if _, err := store.PutVersion(ctx, SampleVersion("v", "k", 0x20)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a put with the ID of an existing version = %v, want %v", err, domain.ErrConflict)
	}
	want("after a refused put", "k@30*", "k@20", "k@null")

	// Deleting the object adds a marker and removes nothing.
	d := del("k", nil, 0x40)
	if d.Marker == nil || !d.Marker.DeleteMarker || !d.Marker.Versioned || d.Marker.VersionID != id(0x40).ID || d.Removed != nil {
		t.Fatalf("delete in a versioned bucket = %+v", d)
	}
	want("after a delete", "k@40(marker)*", "k@30", "k@20", "k@null")
	if _, err := current("k"); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Fatalf("current after a delete = %v", err)
	}
	if v, err := store.GetVersion(ctx, "v", "k", *id(0x40)); err != nil || !v.DeleteMarker {
		t.Errorf("GetVersion of the marker = %+v, %v", v, err)
	}
	for _, fill := range []byte{0x10, 0x20, 0x30} {
		if !referenced(fill) {
			t.Fatalf("the data of version %x is not referenced any more, although the version is there", fill)
		}
	}
	// The listing of current objects does not see the key.
	seen := 0
	if err := store.ScanObjects(ctx, "v", "", func(object.Version) ports.ScanControl { seen++; return ports.ScanControl{} }); err != nil || seen != 0 {
		t.Errorf("ScanObjects sees %d objects behind a marker (%v)", seen, err)
	}
	// A bucket with nothing but history is not empty.
	if err := store.DeleteBucket(ctx, "v"); !errors.Is(err, domain.ErrBucketNotEmpty) {
		t.Fatalf("DeleteBucket with versions left = %v, want %v", err, domain.ErrBucketNotEmpty)
	}

	// Removing the marker brings the object back.
	if d := del("k", id(0x40), 0x41); d.Removed == nil || !d.Removed.DeleteMarker || d.Marker != nil {
		t.Fatalf("removing the marker = %+v", d)
	}
	want("marker removed", "k@30*", "k@20", "k@null")
	if v, err := current("k"); err != nil || v.VersionID != id(0x30).ID {
		t.Fatalf("current after removing the marker = %+v, %v", v, err)
	}
	// Removing a version that is not current changes nothing else.
	if d := del("k", id(0x20), 0x42); d.Removed == nil || d.Removed.VersionID != id(0x20).ID {
		t.Fatalf("removing version 20 = %+v", d)
	}
	if referenced(0x20) || !referenced(0x30) {
		t.Fatal("data references after removing a version are wrong")
	}
	// Removing the current version makes the next one current.
	del("k", id(0x30), 0x43)
	want("current removed", "k@null*")
	if v, err := current("k"); err != nil || v.Versioned || v.DataID != SampleVersion("v", "k", 0x10).DataID {
		t.Fatalf("the null version did not become current: %+v, %v", v, err)
	}
	// Removing what is not there is not an error and removes nothing.
	if d := del("k", id(0x77), 0x44); d.Removed != nil || d.Marker != nil {
		t.Fatalf("removing an unknown version = %+v", d)
	}

	// Suspended: a put is the null version and replaces the one there is;
	// versions with IDs stay.
	put("k", 0x50)
	if err := store.SetBucketVersioning(ctx, "v", bucket.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	r = put("k", 0x60)
	if r.Stored.Versioned || r.Removed == nil || r.Removed.DataID != SampleVersion("v", "k", 0x10).DataID {
		t.Fatalf("put into a suspended bucket = %+v", r)
	}
	want("suspended put", "k@null*", "k@50")
	if referenced(0x10) {
		t.Fatal("the replaced null version is still referenced")
	}
	// A delete is a null marker, and replaces the null version.
	d = del("k", nil, 0x70)
	if d.Marker == nil || d.Marker.Versioned || d.Removed == nil || d.Removed.DataID != SampleVersion("v", "k", 0x60).DataID {
		t.Fatalf("delete in a suspended bucket = %+v", d)
	}
	want("suspended delete", "k@null(marker)*", "k@50")
	// And a put after that replaces the null marker.
	if r := put("k", 0x61); r.Removed == nil || !r.Removed.DeleteMarker {
		t.Fatalf("put over a null marker = %+v", r)
	}
	want("put over a null marker", "k@null*", "k@50")

	// Keys keep apart, whatever bytes they are made of, and come in order.
	if err := store.SetBucketVersioning(ctx, "v", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	odd := []string{"a", "a\x00", "a\x00\x01", "a\x00\u00ff", "a\x01", "ab", "\x00", "a/b"}
	for i, key := range odd {
		put(key, byte(0x80+i))
		del(key, nil, byte(0xa0+i))
	}
	var keys []string
	if err := store.ScanVersions(ctx, "v", ports.VersionCursor{}, func(v object.Version, latest bool) ports.ScanControl {
		if latest {
			keys = append(keys, v.Key)
			if !v.DeleteMarker {
				if v.Key != "k" {
					t.Errorf("latest version of %q is not its marker", v.Key)
				}
			}
		}
		return ports.ScanControl{}
	}); err != nil {
		t.Fatal(err)
	}
	wantKeys := append(slices.Clone(odd), "k")
	slices.Sort(wantKeys)
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("keys in the version listing = %q, want %q", keys, wantKeys)
	}
	for i, key := range odd {
		v, err := store.GetVersion(ctx, "v", key, *id(byte(0x80 + i)))
		if err != nil || v.Key != key {
			t.Errorf("GetVersion(%q) = %q, %v", key, v.Key, err)
		}
	}
	// A scan can start in the middle, and can jump.
	if got := listing(t, store, "v", "ab"); len(got) == 0 || !strings.HasPrefix(got[0], "ab@") {
		t.Errorf("scan from ab begins with %v", got)
	}
	var jumped []string
	if err := store.ScanVersions(ctx, "v", ports.VersionCursor{}, func(v object.Version, _ bool) ports.ScanControl {
		jumped = append(jumped, v.Key)
		if v.Key < "ab" {
			return ports.ScanControl{SeekTo: "ab"}
		}
		return ports.ScanControl{Stop: v.Key == "k"}
	}); err != nil {
		t.Fatal(err)
	}
	if len(jumped) < 3 || jumped[0] != "\x00" || jumped[1] != "ab" || jumped[len(jumped)-1] != "k" {
		t.Errorf("a scan that jumps to ab visited %q", jumped)
	}
	if err := store.ScanVersions(ctx, "nope", ports.VersionCursor{}, func(object.Version, bool) ports.ScanControl { return ports.ScanControl{} }); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("ScanVersions of a missing bucket = %v", err)
	}

	// Emptying the bucket means removing every version, markers included.
	var all []object.Version
	if err := store.ScanVersions(ctx, "v", ports.VersionCursor{}, func(v object.Version, _ bool) ports.ScanControl {
		all = append(all, v)
		return ports.ScanControl{}
	}); err != nil {
		t.Fatal(err)
	}
	for _, v := range all {
		ref := v.Ref()
		if _, err := store.DeleteObject(ctx, "v", v.Key, ports.DeleteOptions{Version: &ref}); err != nil {
			t.Fatal(err)
		}
	}
	want("emptied")
	for fill := range 256 {
		if referenced(byte(fill)) {
			t.Fatalf("data %x is still referenced in an empty bucket", fill)
		}
	}
	if err := store.DeleteBucket(ctx, "v"); err != nil {
		t.Fatalf("DeleteBucket of an emptied versioned bucket: %v", err)
	}
	// A bucket created again does not remember.
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "v", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.GetBucket(ctx, "v"); b.Versioning != bucket.Unversioned {
		t.Errorf("a recreated bucket is %q", b.Versioning)
	}
	want("recreated")
}

// lockContract is what a store must do with locked versions. Its times are
// real ones: the store judges on its own time, which nothing here can set.
func lockContract(t *testing.T, store Metadata) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, b := range []bucket.Bucket{{Name: "locked", ObjectLock: true, Versioning: bucket.VersioningSuspended}, {Name: "plain"}} {
		b.CreatedAt = now
		if err := store.CreateBucket(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// Object Lock is decided at creation, and brings versioning with it,
	// for good.
	if b, err := store.GetBucket(ctx, "locked"); err != nil || !b.ObjectLock || b.Versioning != bucket.VersioningEnabled || b.DefaultRetention != nil {
		t.Fatalf("a bucket created with Object Lock = %+v, %v", b, err)
	}
	if b, _ := store.GetBucket(ctx, "plain"); b.ObjectLock || b.Versioning != bucket.Unversioned {
		t.Fatalf("a plain bucket = %+v", b)
	}
	if err := store.SetBucketVersioning(ctx, "locked", bucket.VersioningSuspended); !errors.Is(err, domain.ErrInvalidBucketState) {
		t.Fatalf("suspending versioning on a bucket with Object Lock = %v, want %v", err, domain.ErrInvalidBucketState)
	}

	id := func(fill byte) object.VersionRef {
		return object.VersionRef{ID: object.ID(bytes.Repeat([]byte{fill}, object.IDSize))}
	}
	put := func(fill byte, lock object.Lock) (ports.PutResult, error) {
		v := SampleVersion("locked", "k", fill)
		v.Lock = lock
		return store.PutVersion(ctx, v)
	}
	remove := func(fill byte, bypass bool) error {
		ref := id(fill)
		_, err := store.DeleteObject(ctx, "locked", "k", ports.DeleteOptions{Version: &ref, BypassGovernance: bypass, At: now})
		return err
	}
	retention := func(mode object.RetentionMode, in time.Duration) *object.Retention {
		return &object.Retention{Mode: mode, Until: now.Add(in)}
	}
	exists := func(fill byte) bool {
		t.Helper()
		v, err := store.GetVersion(ctx, "locked", "k", id(fill))
		if err != nil && !errors.Is(err, domain.ErrVersionNotFound) {
			t.Fatal(err)
		}
		referenced, refErr := store.IsDataReferenced(ctx, SampleVersion("locked", "k", fill).DataID)
		if refErr != nil || (err == nil) != referenced {
			t.Fatalf("version %x exists %v, its data is referenced %v (%v)", fill, err == nil, referenced, refErr)
		}
		return err == nil && !v.DeleteMarker
	}

	// A lock asked for where there can be none is an error, not ignored.
	plain := SampleVersion("plain", "k", 0x01)
	plain.Lock = object.Lock{Retention: retention(object.Compliance, time.Hour)}
	if _, err := store.PutVersion(ctx, plain); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Fatalf("a put with retention into a bucket without Object Lock = %v, want %v", err, domain.ErrLockNotConfigured)
	}
	if _, err := store.SetRetention(ctx, "plain", "k", object.VersionRef{Null: true}, retention(object.Governance, time.Hour), false); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Errorf("SetRetention in a bucket without Object Lock = %v", err)
	}
	if _, err := store.SetLegalHold(ctx, "plain", "k", object.VersionRef{Null: true}, true); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Errorf("SetLegalHold in a bucket without Object Lock = %v", err)
	}
	if err := store.SetBucketDefaultRetention(ctx, "plain", &bucket.DefaultRetention{Mode: object.Governance, Days: 1}); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Errorf("a default retention on a bucket without Object Lock = %v", err)
	}

	// Compliance: nobody removes it, nobody shortens it.
	stored, err := put(0x10, object.Lock{Retention: retention(object.Compliance, time.Hour)})
	if err != nil || stored.Stored.Lock.Retention == nil || stored.Stored.Lock.Retention.Mode != object.Compliance {
		t.Fatalf("put with compliance retention = %+v, %v", stored.Stored.Lock, err)
	}
	for _, bypass := range []bool{false, true} {
		if err := remove(0x10, bypass); !errors.Is(err, domain.ErrObjectLocked) {
			t.Fatalf("removing a compliance-locked version (bypass %v) = %v, want %v", bypass, err, domain.ErrObjectLocked)
		}
	}
	if !exists(0x10) {
		t.Fatal("a compliance-locked version is gone")
	}
	for name, next := range map[string]*object.Retention{
		"shortened": retention(object.Compliance, time.Minute), "removed": nil, "made governance": retention(object.Governance, 2*time.Hour),
	} {
		if _, err := store.SetRetention(ctx, "locked", "k", id(0x10), next, true); !errors.Is(err, domain.ErrObjectLocked) {
			t.Errorf("compliance retention %s = %v, want %v", name, err, domain.ErrObjectLocked)
		}
	}
	extended, err := store.SetRetention(ctx, "locked", "k", id(0x10), retention(object.Compliance, 2*time.Hour), false)
	if err != nil || !extended.Lock.Retention.Until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("extending a compliance retention = %+v, %v", extended.Lock, err)
	}
	// Deleting the object - not the version - is a marker, and allowed.
	deleted, err := store.DeleteObject(ctx, "locked", "k", ports.DeleteOptions{MarkerID: id(0x11).ID, At: now})
	if err != nil || deleted.Marker == nil || !exists(0x10) {
		t.Fatalf("deleting an object whose version is locked = %+v, %v", deleted, err)
	}
	if _, err := store.SetRetention(ctx, "locked", "k", id(0x11), retention(object.Governance, time.Hour), false); !errors.Is(err, domain.ErrMethodNotAllowed) {
		t.Errorf("SetRetention on a delete marker = %v", err)
	}
	if _, err := store.SetRetention(ctx, "locked", "k", id(0x99), retention(object.Governance, time.Hour), false); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Errorf("SetRetention on an unknown version = %v", err)
	}
	// A bucket that holds a locked version cannot be deleted.
	if err := store.DeleteBucket(ctx, "locked"); !errors.Is(err, domain.ErrBucketNotEmpty) {
		t.Fatalf("DeleteBucket with a locked version = %v", err)
	}

	// Governance: lifted by the bypass and by nothing else.
	if _, err := put(0x20, object.Lock{Retention: retention(object.Governance, time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := remove(0x20, false); !errors.Is(err, domain.ErrObjectLocked) {
		t.Fatalf("removing a governance-locked version without bypass = %v", err)
	}
	if _, err := store.SetRetention(ctx, "locked", "k", id(0x20), nil, false); !errors.Is(err, domain.ErrObjectLocked) {
		t.Errorf("removing a governance retention without bypass = %v", err)
	}
	if err := remove(0x20, true); err != nil || exists(0x20) {
		t.Fatalf("removing a governance-locked version with bypass = %v", err)
	}

	// Legal hold: until it is taken off, whatever else is true.
	if _, err := put(0x30, object.Lock{LegalHold: true}); err != nil {
		t.Fatal(err)
	}
	if err := remove(0x30, true); !errors.Is(err, domain.ErrObjectLocked) {
		t.Fatalf("removing a version under legal hold = %v", err)
	}
	if v, err := store.SetLegalHold(ctx, "locked", "k", id(0x30), false); err != nil || v.Lock.LegalHold {
		t.Fatalf("taking a legal hold off = %+v, %v", v.Lock, err)
	}
	if err := remove(0x30, false); err != nil {
		t.Fatalf("removing a version whose legal hold was taken off: %v", err)
	}
	held, err := store.SetLegalHold(ctx, "locked", "k", id(0x10), true)
	if err != nil || !held.Lock.LegalHold || held.Lock.Retention == nil {
		t.Fatalf("a legal hold on a retained version = %+v, %v", held.Lock, err)
	}

	// A retention that is not in the future is refused; one that has
	// ended holds nothing.
	if _, err := put(0x40, object.Lock{Retention: retention(object.Compliance, -time.Hour)}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a put retained until the past = %v, want %v", err, domain.ErrInvalidArgument)
	}
	if exists(0x40) {
		t.Error("a refused put left a version")
	}
	short := 1500 * time.Millisecond
	if _, err := put(0x50, object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: time.Now().Add(short)}}); err != nil {
		t.Fatal(err)
	}
	if err := remove(0x50, true); !errors.Is(err, domain.ErrObjectLocked) {
		t.Fatalf("removing a version whose retention has not ended = %v", err)
	}
	time.Sleep(short + 500*time.Millisecond)
	if err := remove(0x50, false); err != nil {
		t.Fatalf("removing a version whose compliance retention has ended: %v", err)
	}

	// The bucket's default: given to what is put without a retention.
	for name, bad := range map[string]bucket.DefaultRetention{
		"no period": {Mode: object.Compliance}, "two periods": {Mode: object.Compliance, Days: 1, Years: 1}, "no mode": {Days: 1},
	} {
		if err := store.SetBucketDefaultRetention(ctx, "locked", &bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("default retention with %s = %v", name, err)
		}
	}
	if err := store.SetBucketDefaultRetention(ctx, "locked", &bucket.DefaultRetention{Mode: object.Governance, Days: 2}); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.GetBucket(ctx, "locked"); b.DefaultRetention == nil || b.DefaultRetention.Days != 2 {
		t.Fatalf("the default retention as stored = %+v", b.DefaultRetention)
	}
	defaulted, err := put(0x60, object.Lock{})
	if err != nil || defaulted.Stored.Lock.Retention == nil || defaulted.Stored.Lock.Retention.Mode != object.Governance {
		t.Fatalf("a put without retention into a bucket with a default = %+v, %v", defaulted.Stored.Lock, err)
	}
	if until := defaulted.Stored.Lock.Retention.Until; until.Before(now.Add(47*time.Hour)) || until.After(time.Now().Add(49*time.Hour)) {
		t.Errorf("default retention of two days ends %v", until)
	}
	// What a put asks for wins over the default.
	own, err := put(0x70, object.Lock{Retention: retention(object.Compliance, time.Hour)})
	if err != nil || own.Stored.Lock.Retention.Mode != object.Compliance {
		t.Fatalf("a put with its own retention = %+v, %v", own.Stored.Lock, err)
	}
	if err := store.SetBucketDefaultRetention(ctx, "locked", nil); err != nil {
		t.Fatal(err)
	}
	if plainPut, err := put(0x80, object.Lock{}); err != nil || !plainPut.Stored.Lock.IsZero() {
		t.Errorf("a put after the default was removed = %+v, %v", plainPut.Stored.Lock, err)
	}
	// After all of it the first version is still there, locked.
	if v, err := store.GetVersion(ctx, "locked", "k", id(0x10)); err != nil || v.Lock.Retention == nil || !v.Lock.LegalHold {
		t.Fatalf("the compliance-locked version at the end = %+v, %v", v.Lock, err)
	}
}
