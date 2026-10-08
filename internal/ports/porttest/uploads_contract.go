package porttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// SampleUpload returns a multipart upload with every field set.
func SampleUpload(bucketName, key string, fill byte) object.Upload {
	v := SampleVersion(bucketName, key, fill)
	sum := sha256.Sum256([]byte("upload\x00" + bucketName + "\x00" + key + "\x00" + string(rune(fill))))
	return object.Upload{
		Bucket: bucketName, Key: key, ID: object.ID(sum[:object.IDSize]),
		InitiatedAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), Initiator: "albie",
		ContentType: v.ContentType, ContentEncoding: v.ContentEncoding, ContentDisposition: v.ContentDisposition,
		ContentLanguage: v.ContentLanguage, CacheControl: v.CacheControl, UserMetadata: v.UserMetadata,
		Encryption: object.Encryption{
			Scheme: object.StreamSchemeV1, KMSKeyID: "local:test", WrappedKey: bytes.Repeat([]byte{fill, 0xEE}, 30),
			SegmentSize: object.DefaultSegmentSize, KeyMode: object.KeyModePartHKDF,
		},
	}
}

// SamplePart returns part number of an upload, of the given size. serial
// tells one upload of the part from the next.
func SamplePart(u object.Upload, number int, size int64, serial byte) object.Part {
	sum := sha256.Sum256(fmt.Appendf(nil, "part\x00%s/%d/%d", u.ID[:], number, serial))
	return object.Part{
		Number: number, DataID: object.ID(sum[:object.IDSize]), Size: size,
		ETag:        "0cc175b9c0f1b6a831c399e2697726" + string("0123456789abcdef"[serial%16]) + string("0123456789abcdef"[number%16]),
		Checksums:   map[string]string{"x-amz-checksum-crc32": "AAAAAA=="},
		NoncePrefix: []byte{serial, 1, 2, 3},
		Layout: object.Layout{
			DataShards: 1, StripeSize: 1 << 20, StoredSize: size + 16,
			Shards: []object.ShardLocation{{DriveID: "drive-a", Size: size + 16}},
		},
		ModifiedAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC),
	}
}

// sealOf stands for a seal: thirty-two bytes that tell one from another.
func sealOf(b byte) []byte {
	return bytes.Repeat([]byte{b}, sha256.Size)
}

// completionOf lists parts for a completion, as the use case would.
func completionOf(u object.Upload, versionFill byte, parts ...object.Part) ports.Completion {
	c := ports.Completion{
		Bucket: u.Bucket, Key: u.Key, ID: u.ID, VersionID: sampleVersionID(versionFill),
		ETag: "d41d8cd98f00b204e9800998ecf8427e-" + strconv.Itoa(len(parts)), At: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
	}
	for i, p := range parts {
		p.Seal = sealOf(byte(i + 1))
		c.Parts = append(c.Parts, p)
	}
	return c
}

func uploadsContract(t *testing.T, open func(t *testing.T) Metadata) {
	t.Helper()
	ctx := context.Background()
	const big = object.MinPartSize
	referenced := func(t *testing.T, store Metadata, id object.ID) bool {
		t.Helper()
		ok, err := store.IsDataReferenced(ctx, id)
		if err != nil {
			t.Fatalf("IsDataReferenced: %v", err)
		}
		return ok
	}
	newStore := func(t *testing.T, buckets ...bucket.Bucket) Metadata {
		t.Helper()
		store := open(t)
		for _, b := range buckets {
			if err := store.CreateBucket(ctx, b); err != nil {
				t.Fatalf("CreateBucket: %v", err)
			}
		}
		return store
	}
	put := func(t *testing.T, store Metadata, u object.Upload, parts ...object.Part) {
		t.Helper()
		for _, p := range parts {
			if _, err := store.PutPart(ctx, u.Bucket, u.Key, u.ID, p); err != nil {
				t.Fatalf("PutPart %d: %v", p.Number, err)
			}
		}
	}

	t.Run("an upload and its parts", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"})
		u := SampleUpload("b", "movie", 1)
		if _, err := store.GetUpload(ctx, "b", "movie", u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Fatalf("GetUpload before it exists = %v", err)
		}
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatalf("CreateUpload: %v", err)
		}
		got, err := store.GetUpload(ctx, "b", "movie", u.ID)
		if err != nil || !reflect.DeepEqual(got, u) {
			t.Fatalf("GetUpload = %+v, %v; want %+v", got, err, u)
		}
		// An upload belongs to its key.
		if _, err := store.GetUpload(ctx, "b", "other", u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("GetUpload under another key = %v", err)
		}
		if _, err := store.PutPart(ctx, "b", "other", u.ID, SamplePart(u, 1, 10, 0)); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("PutPart under another key = %v", err)
		}
		if !referenced(t, store, u.ID) {
			t.Error("the ID of an upload in progress is not in use")
		}

		first, second, third := SamplePart(u, 1, big, 0), SamplePart(u, 2, big, 0), SamplePart(u, 7, 10, 0)
		put(t, store, u, third, first, second)
		all, err := store.ListParts(ctx, "b", "movie", u.ID, 0, 100)
		if err != nil || !reflect.DeepEqual(all, []object.Part{first, second, third}) {
			t.Fatalf("ListParts = %+v, %v", all, err)
		}
		page, err := store.ListParts(ctx, "b", "movie", u.ID, 1, 1)
		if err != nil || len(page) != 1 || page[0].Number != 2 {
			t.Errorf("ListParts after 1, at most 1 = %+v, %v", page, err)
		}
		if rest, err := store.ListParts(ctx, "b", "movie", u.ID, 7, 100); err != nil || len(rest) != 0 {
			t.Errorf("ListParts after the last = %+v, %v", rest, err)
		}
		for _, p := range all {
			if !referenced(t, store, p.DataID) {
				t.Errorf("the data of part %d is not referenced: the collector would remove it", p.Number)
			}
		}

		// A part uploaded again takes the place of the one before, which
		// comes back: nothing refers to its data any more.
		again := SamplePart(u, 2, big+5, 1)
		replaced, err := store.PutPart(ctx, "b", "movie", u.ID, again)
		if err != nil || replaced == nil || replaced.DataID != second.DataID {
			t.Fatalf("PutPart again = %+v, %v; want the part it replaced", replaced, err)
		}
		if referenced(t, store, second.DataID) || !referenced(t, store, again.DataID) {
			t.Error("after a part was uploaded again: the old data is referenced, or the new is not")
		}

		// What is refused, and leaves nothing behind.
		refused := map[string]struct {
			part object.Part
			want error
		}{
			"part number zero":      {SamplePart(u, 0, 1, 9), domain.ErrInvalidArgument},
			"part number too great": {SamplePart(u, object.MaxParts+1, 1, 9), domain.ErrInvalidArgument},
			"too large":             {SamplePart(u, 3, object.MaxSize+1, 9), domain.ErrEntityTooLarge},
			"data in use":           {func() object.Part { p := SamplePart(u, 3, 1, 9); p.DataID = first.DataID; return p }(), domain.ErrConflict},
			"the upload's own ID":   {func() object.Part { p := SamplePart(u, 3, 1, 9); p.DataID = u.ID; return p }(), domain.ErrConflict},
			"no data ID":            {func() object.Part { p := SamplePart(u, 3, 1, 9); p.DataID = object.ID{}; return p }(), domain.ErrInvalidArgument},
		}
		for name, tt := range refused {
			if _, err := store.PutPart(ctx, "b", "movie", u.ID, tt.part); !errors.Is(err, tt.want) {
				t.Errorf("PutPart with %s = %v, want %v", name, err, tt.want)
			}
		}
		if parts, _ := store.ListParts(ctx, "b", "movie", u.ID, 0, 100); len(parts) != 3 {
			t.Errorf("refused parts left something: %d parts", len(parts))
		}
	})

	t.Run("an upload that cannot begin", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"})
		taken := SampleVersion("b", "k", 1)
		if _, err := store.PutVersion(ctx, taken); err != nil {
			t.Fatal(err)
		}
		u := SampleUpload("b", "movie", 1)
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		for name, tt := range map[string]struct {
			change func(u *object.Upload)
			want   error
		}{
			"no bucket":                    {func(u *object.Upload) { u.Bucket = "missing" }, domain.ErrBucketNotFound},
			"no key":                       {func(u *object.Upload) { u.Key = "" }, domain.ErrInvalidObjectKey},
			"no ID":                        {func(u *object.Upload) { u.ID = object.ID{} }, domain.ErrInvalidArgument},
			"the ID of another":            {func(u *object.Upload) { u.ID = SampleUpload("b", "movie", 1).ID }, domain.ErrConflict},
			"the data ID of a version":     {func(u *object.Upload) { u.ID = taken.DataID }, domain.ErrConflict},
			"a lock without Object Lock":   {func(u *object.Upload) { u.Lock = object.Lock{LegalHold: true} }, domain.ErrLockNotConfigured},
			"a key that encrypts a stream": {func(u *object.Upload) { u.Encryption.KeyMode = "" }, domain.ErrInvalidArgument},
		} {
			bad := SampleUpload("b", "another", 2)
			tt.change(&bad)
			if err := store.CreateUpload(ctx, bad); !errors.Is(err, tt.want) {
				t.Errorf("CreateUpload with %s = %v, want %v", name, err, tt.want)
			}
		}
		// And a put may not take the ID of an upload for its data.
		v := SampleVersion("b", "thief", 3)
		v.DataID = u.ID
		if _, err := store.PutVersion(ctx, v); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a put that names an upload's ID as its data = %v, want %v", err, domain.ErrConflict)
		}
		// Nor say that it is made of parts.
		v = SampleVersion("b", "thief", 3)
		v.Parts = 2
		if _, err := store.PutVersion(ctx, v); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a put that says it is made of parts = %v, want %v", err, domain.ErrInvalidArgument)
		}
	})

	t.Run("complete", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"})
		old := SampleVersion("b", "movie", 9)
		if _, err := store.PutVersion(ctx, old); err != nil {
			t.Fatal(err)
		}
		u := SampleUpload("b", "movie", 1)
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		first, second, unused, last := SamplePart(u, 1, big, 0), SamplePart(u, 2, big+3, 0), SamplePart(u, 3, 77, 0), SamplePart(u, 4, 10, 0)
		put(t, store, u, first, second, unused, last)

		// What is not an object is refused, and changes nothing.
		for name, tt := range map[string]struct {
			c    ports.Completion
			want error
		}{
			"no parts":           {completionOf(u, 5), domain.ErrInvalidArgument},
			"out of order":       {completionOf(u, 5, second, first), domain.ErrPartOrder},
			"a part that is not": {completionOf(u, 5, first, SamplePart(u, 9, 1, 0)), domain.ErrPartNotFound},
			"another entity tag": {completionOf(u, 5, func() object.Part { p := first; p.ETag = "ffff"; return p }()), domain.ErrPartNotFound},
			"another checksum": {completionOf(u, 5, func() object.Part {
				p := first
				p.Checksums = map[string]string{"x-amz-checksum-crc32": "BBBBBB=="}
				return p
			}()), domain.ErrPartNotFound},
			"a small part in the middle":  {completionOf(u, 5, first, unused, last), domain.ErrPartTooSmall},
			"data that is not the part's": {completionOf(u, 5, func() object.Part { p := first; p.DataID = second.DataID; return p }()), domain.ErrConflict},
			"no version ID":               {func() ports.Completion { c := completionOf(u, 5, first); c.VersionID = object.ID{}; return c }(), domain.ErrInvalidArgument},
			"a seal that is none":         {func() ports.Completion { c := completionOf(u, 5, first); c.Parts[0].Seal = []byte{1}; return c }(), domain.ErrInvalidArgument},
			"another key":                 {func() ports.Completion { c := completionOf(u, 5, first); c.Key = "other"; return c }(), domain.ErrUploadNotFound},
			"no such upload":              {func() ports.Completion { c := completionOf(u, 5, first); c.ID = object.ID{9}; return c }(), domain.ErrUploadNotFound},
		} {
			if _, err := store.CompleteUpload(ctx, tt.c); !errors.Is(err, tt.want) {
				t.Errorf("CompleteUpload with %s = %v, want %v", name, err, tt.want)
			}
		}
		if current, err := store.GetObject(ctx, "b", "movie"); err != nil || current.DataID != old.DataID {
			t.Fatalf("a refused completion changed the object: %+v, %v", current, err)
		}
		if parts, _ := store.ListParts(ctx, "b", "movie", u.ID, 0, 100); len(parts) != 4 {
			t.Fatalf("a refused completion changed the upload: %d parts", len(parts))
		}

		completion := completionOf(u, 5, first, second, last)
		result, err := store.CompleteUpload(ctx, completion)
		if err != nil {
			t.Fatalf("CompleteUpload: %v", err)
		}
		stored := result.Stored
		if stored.DataID != u.ID || stored.Parts != 3 || stored.Size != 2*big+13 || stored.ETag != completion.ETag ||
			stored.VersionID != completion.VersionID || !stored.ModifiedAt.Equal(completion.At) ||
			stored.ContentType != u.ContentType || !reflect.DeepEqual(stored.UserMetadata, u.UserMetadata) ||
			!reflect.DeepEqual(stored.Encryption, u.Encryption) || len(stored.Layout.Shards) != 0 {
			t.Errorf("the completed version = %+v", stored)
		}
		// In a bucket that keeps no versions it replaced the object there
		// was, and the part that was not listed is named: nothing refers
		// to its data.
		if result.Removed == nil || result.Removed.DataID != old.DataID {
			t.Errorf("Removed = %+v, want the replaced version", result.Removed)
		}
		if len(result.RemovedParts) != 1 || result.RemovedParts[0].DataID != unused.DataID {
			t.Errorf("RemovedParts = %+v, want the part that was not listed", result.RemovedParts)
		}
		if current, err := store.GetObject(ctx, "b", "movie"); err != nil || !reflect.DeepEqual(current, stored) {
			t.Errorf("GetObject after completion = %+v, %v", current, err)
		}
		if _, err := store.GetUpload(ctx, "b", "movie", u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("the upload is still there after completion: %v", err)
		}
		if _, err := store.CompleteUpload(ctx, completion); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("completing twice = %v", err)
		}
		for id, want := range map[object.ID]bool{
			u.ID: true, first.DataID: true, second.DataID: true, last.DataID: true, unused.DataID: false, old.DataID: false,
		} {
			if referenced(t, store, id) != want {
				t.Errorf("after completion, data %s referenced = %v, want %v", id, !want, want)
			}
		}

		// The parts of the object, each in its place.
		parts, err := store.ObjectParts(ctx, "b", u.ID, 0, 100)
		if err != nil || len(parts) != 3 {
			t.Fatalf("ObjectParts = %+v, %v", parts, err)
		}
		var offset int64
		for i, p := range parts {
			want := []object.Part{first, second, last}[i]
			want.Index, want.Offset, want.Seal = i, offset, sealOf(byte(i+1))
			if !reflect.DeepEqual(p, want) {
				t.Errorf("part %d of the object = %+v, want %+v", i, p, want)
			}
			offset += p.Size
		}
		for at, wantFirst := range map[int64]int{0: 1, big - 1: 1, big: 2, big + 5: 2, 2*big + 3: 4, 2*big + 12: 4} {
			got, err := store.ObjectParts(ctx, "b", u.ID, at, 1)
			if err != nil || len(got) != 1 || got[0].Number != wantFirst {
				t.Errorf("ObjectParts at %d = %+v, %v; want part %d", at, got, err, wantFirst)
			}
		}
		if got, _ := store.ObjectParts(ctx, "b", u.ID, 2*big+13, 5); len(got) != 0 {
			t.Errorf("ObjectParts past the end = %+v", got)
		}
		if got, _ := store.ObjectParts(ctx, "b", object.ID{42}, 0, 5); len(got) != 0 {
			t.Errorf("ObjectParts of nothing = %+v", got)
		}

		// Removing the version removes its parts, and names them.
		deleted, err := store.DeleteObject(ctx, "b", "movie", ports.DeleteOptions{})
		if err != nil || deleted.Removed == nil || len(deleted.RemovedParts) != 3 {
			t.Fatalf("DeleteObject = %+v, %v; want the version and its three parts", deleted, err)
		}
		for _, id := range []object.ID{u.ID, first.DataID, second.DataID, last.DataID} {
			if referenced(t, store, id) {
				t.Errorf("data %s is still referenced after the object was deleted", id)
			}
		}
		if got, _ := store.ObjectParts(ctx, "b", u.ID, 0, 5); len(got) != 0 {
			t.Errorf("parts of a deleted object = %+v", got)
		}
	})

	t.Run("an empty last part", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"})
		u := SampleUpload("b", "k", 1)
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		first, empty := SamplePart(u, 1, big, 0), SamplePart(u, 2, 0, 0)
		put(t, store, u, first, empty)
		result, err := store.CompleteUpload(ctx, completionOf(u, 5, first, empty))
		if err != nil || result.Stored.Size != big || result.Stored.Parts != 2 {
			t.Fatalf("CompleteUpload = %+v, %v", result.Stored, err)
		}
		all, err := store.ObjectParts(ctx, "b", u.ID, 0, 10)
		if err != nil || len(all) != 2 || all[1].Offset != big || all[1].Size != 0 {
			t.Fatalf("ObjectParts = %+v, %v", all, err)
		}
		// A walk that continues where the first part ends finds the empty
		// one, once.
		if tail, _ := store.ObjectParts(ctx, "b", u.ID, big, 10); len(tail) != 1 || tail[0].Index != 1 {
			t.Errorf("ObjectParts at the end = %+v, want the empty last part", tail)
		}
	})

	t.Run("complete in a bucket that keeps versions and locks", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "vault", ObjectLock: true})
		before, err := store.PutVersion(ctx, SampleVersion("vault", "ledger", 9))
		if err != nil {
			t.Fatal(err)
		}
		old := before.Stored
		if err := store.SetBucketDefaultRetention(ctx, "vault", &bucket.DefaultRetention{Mode: object.Governance, Days: 1}); err != nil {
			t.Fatal(err)
		}
		u := SampleUpload("vault", "ledger", 1)
		u.Lock = object.Lock{LegalHold: true}
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		part := SamplePart(u, 1, 10, 0)
		put(t, store, u, part)
		result, err := store.CompleteUpload(ctx, completionOf(u, 5, part))
		if err != nil {
			t.Fatalf("CompleteUpload: %v", err)
		}
		if result.Removed != nil || !result.Stored.Versioned {
			t.Errorf("completion in a versioned bucket removed %+v, stored versioned = %v", result.Removed, result.Stored.Versioned)
		}
		// The hold the upload asked for, and the retention the bucket
		// gives every version, from the time of the completion.
		lock := result.Stored.Lock
		if !lock.LegalHold || lock.Retention == nil || lock.Retention.Mode != object.Governance {
			t.Errorf("lock of the completed version = %+v", lock)
		}
		ref := result.Stored.Ref()
		if _, err := store.DeleteObject(ctx, "vault", "ledger", ports.DeleteOptions{Version: &ref, BypassGovernance: true}); !errors.Is(err, domain.ErrObjectLocked) {
			t.Errorf("deleting the held version = %v, want %v", err, domain.ErrObjectLocked)
		}
		if parts, _ := store.ObjectParts(ctx, "vault", u.ID, 0, 5); len(parts) != 1 {
			t.Errorf("a refused delete removed the parts: %+v", parts)
		}
		if previous, err := store.GetVersion(ctx, "vault", "ledger", old.Ref()); err != nil || previous.DataID != old.DataID {
			t.Errorf("the version before is gone: %v", err)
		}
	})

	t.Run("abort", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"})
		u := SampleUpload("b", "k", 1)
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		first, second := SamplePart(u, 1, 10, 0), SamplePart(u, 2, 20, 0)
		put(t, store, u, first, second)
		if _, err := store.AbortUpload(ctx, "b", "other", u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("AbortUpload under another key = %v", err)
		}
		parts, err := store.AbortUpload(ctx, "b", "k", u.ID)
		if err != nil || len(parts) != 2 || parts[0].DataID != first.DataID || parts[1].DataID != second.DataID {
			t.Fatalf("AbortUpload = %+v, %v; want both parts", parts, err)
		}
		for _, id := range []object.ID{u.ID, first.DataID, second.DataID} {
			if referenced(t, store, id) {
				t.Errorf("data %s is still referenced after the upload was aborted", id)
			}
		}
		if _, err := store.AbortUpload(ctx, "b", "k", u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("aborting twice = %v", err)
		}
		if _, err := store.PutPart(ctx, "b", "k", u.ID, SamplePart(u, 3, 1, 0)); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("PutPart after abort = %v", err)
		}
		if _, err := store.ListParts(ctx, "b", "k", u.ID, 0, 10); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("ListParts after abort = %v", err)
		}
	})

	t.Run("scan", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"}, bucket.Bucket{Name: "other"})
		var want []object.Upload
		for i, key := range []string{"a", "dir/x", "dir/x", "dir/y", "dir\x00z", "e"} {
			u := SampleUpload("b", key, byte(i+1))
			if err := store.CreateUpload(ctx, u); err != nil {
				t.Fatal(err)
			}
			want = append(want, u)
		}
		if err := store.CreateUpload(ctx, SampleUpload("other", "a", 50)); err != nil {
			t.Fatal(err)
		}
		collect := func(start ports.UploadCursor, control func(u object.Upload) ports.ScanControl) []object.Upload {
			t.Helper()
			var got []object.Upload
			err := store.ScanUploads(ctx, "b", start, func(u object.Upload) ports.ScanControl {
				got = append(got, u)
				if control != nil {
					return control(u)
				}
				return ports.ScanControl{}
			})
			if err != nil {
				t.Fatalf("ScanUploads: %v", err)
			}
			return got
		}
		all := collect(ports.UploadCursor{}, nil)
		if len(all) != len(want) {
			t.Fatalf("ScanUploads visited %d uploads, want %d", len(all), len(want))
		}
		for i := 1; i < len(all); i++ {
			a, b := all[i-1], all[i]
			if a.Key > b.Key || (a.Key == b.Key && bytes.Compare(a.ID[:], b.ID[:]) >= 0) {
				t.Errorf("uploads out of order: %q %s before %q %s", a.Key, a.ID, b.Key, b.ID)
			}
		}
		// From a key; after one upload of a key; and jumping.
		// "dir\x00z" sorts before "dir/x": the order is that of the bytes.
		if got := collect(ports.UploadCursor{Key: "dir/x"}, nil); len(got) != 4 || got[0].Key != "dir/x" {
			t.Errorf("from dir/x: %d uploads, first %q", len(got), got[0].Key)
		}
		firstOfX := all[2]
		if got := collect(ports.UploadCursor{Key: "dir/x", After: firstOfX.ID}, nil); len(got) != 3 || got[0].ID != all[3].ID {
			t.Errorf("after the first upload of dir/x: %d uploads", len(got))
		}
		everyByte := object.ID(bytes.Repeat([]byte{0xff}, object.IDSize))
		if got := collect(ports.UploadCursor{Key: "dir/x", After: everyByte}, nil); len(got) != 2 || got[0].Key != "dir/y" {
			t.Errorf("after every upload of dir/x: %+v", got)
		}
		jumped := collect(ports.UploadCursor{}, func(u object.Upload) ports.ScanControl {
			if u.Key == "a" {
				return ports.ScanControl{SeekTo: "dir0"}
			}
			return ports.ScanControl{}
		})
		if len(jumped) != 2 || jumped[1].Key != "e" {
			t.Errorf("after a jump past dir/: %+v", jumped)
		}
		if got := collect(ports.UploadCursor{}, func(object.Upload) ports.ScanControl { return ports.ScanControl{Stop: true} }); len(got) != 1 {
			t.Errorf("a stopped scan visited %d", len(got))
		}
		if err := store.ScanUploads(ctx, "missing", ports.UploadCursor{}, func(object.Upload) ports.ScanControl { return ports.ScanControl{} }); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ScanUploads of a missing bucket = %v", err)
		}
	})

	t.Run("uploads go with their bucket", func(t *testing.T) {
		store := newStore(t, bucket.Bucket{Name: "b"})
		u := SampleUpload("b", "k", 1)
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		part := SamplePart(u, 1, 10, 0)
		put(t, store, u, part)
		// An upload in progress is not an object: the bucket is empty.
		if err := store.DeleteBucket(ctx, "b"); err != nil {
			t.Fatalf("DeleteBucket with an upload in progress: %v", err)
		}
		if referenced(t, store, u.ID) || referenced(t, store, part.DataID) {
			t.Error("the data of an upload is still referenced after its bucket was deleted")
		}
		// A bucket of the same name does not find the old upload.
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetUpload(ctx, "b", "k", u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("an upload outlived its bucket: %v", err)
		}
		if err := store.CreateUpload(ctx, u); err != nil {
			t.Errorf("the ID of an upload that went with its bucket cannot be used: %v", err)
		}
	})
}
