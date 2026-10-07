package porttest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// SampleVersion returns a fully populated object version for tests. fill
// varies the identifiers so that two samples differ.
func SampleVersion(bucketName, key string, fill byte) object.Version {
	return object.Version{
		Bucket:             bucketName,
		Key:                key,
		VersionID:          object.ID(bytes.Repeat([]byte{fill}, object.IDSize)),
		DataID:             object.ID(bytes.Repeat([]byte{fill + 1}, object.IDSize)),
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
		if _, err := store.DeleteObject(ctx, "mango", "k"); err != nil {
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
		if _, err := store.DeleteObject(ctx, "missing", "k"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("DeleteObject(missing bucket) error = %v, want %v", err, domain.ErrBucketNotFound)
		}
		if _, err := store.GetObject(ctx, "one", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("GetObject(missing key) error = %v, want %v", err, domain.ErrObjectNotFound)
		}
		if removed, err := store.DeleteObject(ctx, "one", "k"); err != nil || removed != nil {
			t.Errorf("DeleteObject(missing key) = %v, %v; want nil, nil", removed, err)
		}

		first := SampleVersion("one", "k", 1)
		replaced, err := store.PutVersion(ctx, first)
		if err != nil || replaced != nil {
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
		if err != nil || replaced == nil || replaced.DataID != first.DataID {
			t.Fatalf("second PutVersion = %+v, %v; want the first version back", replaced, err)
		}
		if got, err := store.GetObject(ctx, "one", "k"); err != nil || got.DataID != second.DataID {
			t.Errorf("GetObject after overwrite = %+v, %v", got, err)
		}

		// The same key in another bucket is another object.
		if replaced, err := store.PutVersion(ctx, SampleVersion("two", "k", 9)); err != nil || replaced != nil {
			t.Errorf("PutVersion in another bucket = %v, %v; want nil, nil", replaced, err)
		}

		removed, err := store.DeleteObject(ctx, "one", "k")
		if err != nil || removed == nil || removed.DataID != second.DataID {
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

	t.Run("open missing", func(t *testing.T) {
		store := open(t)
		if _, err := store.Open(ctx, shard(7, 0)); !errors.Is(err, domain.ErrShardNotFound) {
			t.Errorf("Open(missing) error = %v, want %v", err, domain.ErrShardNotFound)
		}
	})
}
