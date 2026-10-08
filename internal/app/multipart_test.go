package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// partTag stands for the entity tag the inbound adapter computes: any
// function of the content will do for the use cases.
func partTag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func (f *fixture) createUpload(t *testing.T, key string) object.Upload {
	t.Helper()
	upload, err := f.objects.CreateUpload(context.Background(), porttest.Root(), app.CreateUploadInput{
		Bucket: "b", Key: key, ContentType: "video/mp4", UserMetadata: map[string]string{"take": "3"},
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	return upload
}

func (f *fixture) uploadPart(t *testing.T, u object.Upload, number int, data []byte) object.Part {
	t.Helper()
	part, err := f.objects.UploadPart(context.Background(), porttest.Root(), app.UploadPartInput{
		Bucket: u.Bucket, Key: u.Key, UploadID: u.ID, Number: number, Body: bytes.NewReader(data), Size: int64(len(data)),
		Verify:    func() (string, error) { return partTag(data), nil },
		Checksums: func() map[string]string { return map[string]string{"x-amz-checksum-test": partTag(data)[:8]} },
	})
	if err != nil {
		t.Fatalf("UploadPart %d: %v", number, err)
	}
	return part
}

func listed(parts ...object.Part) []object.CompletedPart {
	out := make([]object.CompletedPart, len(parts))
	for i, p := range parts {
		out[i] = object.CompletedPart{Number: p.Number, ETag: p.ETag, Checksums: p.Checksums}
	}
	return out
}

func (f *fixture) complete(t *testing.T, u object.Upload, parts ...object.Part) object.Version {
	t.Helper()
	version, err := f.objects.CompleteUpload(context.Background(), porttest.Root(), app.CompleteUploadInput{
		Bucket: u.Bucket, Key: u.Key, UploadID: u.ID, Parts: listed(parts...), ETag: "etag-of-the-parts-3",
	})
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	return version
}

// threeParts uploads and completes an object of three parts and returns its
// version, its parts and its content.
func (f *fixture) threeParts(t *testing.T, key string) (object.Version, []object.Part, []byte) {
	t.Helper()
	u := f.createUpload(t, key)
	contents := [][]byte{randomBytes(t, object.MinPartSize+17), randomBytes(t, object.MinPartSize), randomBytes(t, 1000)}
	// Not in order: the order is what the completion says.
	third := f.uploadPart(t, u, 9, contents[2])
	first := f.uploadPart(t, u, 2, contents[0])
	second := f.uploadPart(t, u, 5, contents[1])
	parts := []object.Part{first, second, third}
	return f.complete(t, u, parts...), parts, slices.Concat(contents...)
}

// TestMultipartUpload: the parts of an upload become one object, readable
// whole and by any range, and nothing of the upload is left behind.
func TestMultipartUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	u := f.createUpload(t, "movie")
	if u.ID.IsZero() || u.Encryption.KeyMode != object.KeyModePartHKDF || len(u.Encryption.WrappedKey) == 0 || len(u.Encryption.NoncePrefix) != 0 {
		t.Fatalf("upload = %+v", u)
	}
	contents := [][]byte{randomBytes(t, object.MinPartSize+17), randomBytes(t, object.MinPartSize), randomBytes(t, 1000)}
	third := f.uploadPart(t, u, 9, contents[2])
	stale := f.uploadPart(t, u, 2, randomBytes(t, 4000))
	f.uploadPart(t, u, 7, randomBytes(t, 3000)) // will not be listed
	first := f.uploadPart(t, u, 2, contents[0]) // again: takes the place of stale
	second := f.uploadPart(t, u, 5, contents[1])
	if stale.DataID == first.DataID || bytes.Equal(stale.NoncePrefix, first.NoncePrefix) {
		t.Error("a part uploaded again has the data ID or the nonce prefix of the one before")
	}
	// The part that was replaced is gone from the drives at once.
	if want := 4 * sixDrives; f.shardCount() != want {
		t.Errorf("%d shards stored during the upload, want %d (four parts)", f.shardCount(), want)
	}

	parts, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{
		UploadRef: app.UploadRef{Bucket: "b", Key: "movie", UploadID: u.ID}, Max: 2,
	})
	if err != nil || len(parts.Parts) != 2 || !parts.Truncated || parts.NextAfter != 5 || parts.Parts[0].Number != 2 || parts.Upload.ID != u.ID {
		t.Fatalf("ListParts = %+v, %v", parts, err)
	}
	rest, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{
		UploadRef: app.UploadRef{Bucket: "b", Key: "movie", UploadID: u.ID}, After: parts.NextAfter, Max: 1000,
	})
	if err != nil || len(rest.Parts) != 2 || rest.Truncated || rest.Parts[1].Number != 9 || rest.Parts[1].Size != 1000 {
		t.Fatalf("ListParts, second page = %+v, %v", rest, err)
	}
	// Nothing is visible before the upload is completed.
	if _, _, err := f.get("b", "movie", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Fatalf("Get before completion = %v", err)
	}

	version := f.complete(t, u, first, second, third)
	want := slices.Concat(contents...)
	if version.Size != int64(len(want)) || version.Parts != 3 || version.DataID != u.ID || version.ETag != "etag-of-the-parts-3" ||
		version.ContentType != "video/mp4" || version.UserMetadata["take"] != "3" || len(version.Layout.Shards) != 0 {
		t.Errorf("version = %+v", version)
	}
	// The part that was not listed is gone from the drives.
	if count := 3 * sixDrives; f.shardCount() != count {
		t.Errorf("%d shards stored after completion, want %d (three parts)", f.shardCount(), count)
	}
	got, out, err := f.get("b", "movie", nil)
	if err != nil || !bytes.Equal(got, want) || out.Range.Length != int64(len(want)) {
		t.Fatalf("Get = %d bytes, %v; want the %d bytes of the parts in order", len(got), err, len(want))
	}

	// Any range, in particular across the boundaries of parts and of
	// segments.
	size := int64(len(want))
	edges := []int64{
		0, 1, object.DefaultSegmentSize - 1, object.DefaultSegmentSize, object.MinPartSize + 16, object.MinPartSize + 17,
		object.MinPartSize + 18, 2*object.MinPartSize + 16, 2*object.MinPartSize + 17, size - 1000, size - 1,
	}
	// A fixed sequence that reaches every pair of edges and, every third
	// time, somewhere between them.
	for i := range 60 {
		from := edges[i%len(edges)]
		to := edges[(i*7+3)%len(edges)]
		if i%3 == 0 {
			from, to = int64(i)*7919*104729%size, int64(i)*15485863%size
		}
		if from > to {
			from, to = to, from
		}
		spec := &object.RangeSpec{First: from, Last: to}
		got, out, err := f.get("b", "movie", spec)
		if err != nil || !bytes.Equal(got, want[from:to+1]) || out.Range.Offset != from {
			t.Fatalf("range %d-%d: %d bytes, %v; want %d", from, to, len(got), err, to-from+1)
		}
	}
	if got, _, err := f.get("b", "movie", &object.RangeSpec{Suffix: 1500}); err != nil || !bytes.Equal(got, want[size-1500:]) {
		t.Errorf("the last 1500 bytes: %d bytes, %v", len(got), err)
	}

	// The upload is over. Asking again to complete it is answered with
	// the object it made, for the client that did not hear the answer.
	if _, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "b", Key: "movie", UploadID: u.ID, Number: 1, Body: bytes.NewReader(nil), Size: 0,
	}); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("UploadPart after completion = %v", err)
	}
	again, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "b", Key: "movie", UploadID: u.ID, Parts: listed(first, second, third), ETag: "etag-of-the-parts-3",
	})
	if err != nil || again.VersionID != version.VersionID {
		t.Errorf("completing again = %+v, %v; want the version it made", again.VersionID, err)
	}

	// Deleting the object removes every part from the drives.
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "movie"}); err != nil {
		t.Fatal(err)
	}
	if f.shardCount() != 0 {
		t.Errorf("%d shards left after the object was deleted", f.shardCount())
	}
	// And then the upload is not found, by a retry either.
	if _, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "b", Key: "movie", UploadID: u.ID, Parts: listed(first), ETag: "x",
	}); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("completing an upload whose object is gone = %v", err)
	}
}

// TestMultipartSmallAndEmpty: an upload of one small part, of one empty
// part, and with an empty last part.
func TestMultipartSmallAndEmpty(t *testing.T) {
	t.Parallel()
	f := erasureFixture(t)
	for name, contents := range map[string][][]byte{
		"one small part":        {[]byte("hello")},
		"one empty part":        {{}},
		"an empty last part":    {randomBytes(t, object.MinPartSize), {}},
		"a segment to the byte": {randomBytes(t, object.DefaultSegmentSize)},
	} {
		u := f.createUpload(t, name)
		var parts []object.Part
		for i, data := range contents {
			parts = append(parts, f.uploadPart(t, u, i+1, data))
		}
		version := f.complete(t, u, parts...)
		want := slices.Concat(contents...)
		got, _, err := f.get("b", name, nil)
		if err != nil || !bytes.Equal(got, want) || version.Size != int64(len(want)) || version.Parts != len(contents) {
			t.Errorf("%s: %d bytes, %v; want %d", name, len(got), err, len(want))
		}
		if len(want) > 0 {
			last := int64(len(want) - 1)
			if got, _, err := f.get("b", name, &object.RangeSpec{First: last, Last: last}); err != nil || !bytes.Equal(got, want[last:]) {
				t.Errorf("%s: the last byte: %v, %v", name, got, err)
			}
		}
	}
}

// TestCompleteUploadRefusals: what is not an object is refused before
// anything changes.
func TestCompleteUploadRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	u := f.createUpload(t, "k")
	big := f.uploadPart(t, u, 1, randomBytes(t, object.MinPartSize))
	small := f.uploadPart(t, u, 2, []byte("small"))
	tail := f.uploadPart(t, u, 3, []byte("tail"))
	wrongTag, wrongSum := big, big
	wrongTag.ETag = "ffff"
	wrongSum.Checksums = map[string]string{"x-amz-checksum-test": "nope"}
	for name, tt := range map[string]struct {
		parts []object.Part
		want  error
	}{
		"no parts":                   {nil, domain.ErrInvalidArgument},
		"out of order":               {[]object.Part{small, big}, domain.ErrPartOrder},
		"a part twice":               {[]object.Part{big, big}, domain.ErrPartOrder},
		"a part that was not sent":   {[]object.Part{big, {Number: 8, ETag: "x"}}, domain.ErrPartNotFound},
		"another entity tag":         {[]object.Part{wrongTag}, domain.ErrPartNotFound},
		"another checksum":           {[]object.Part{wrongSum}, domain.ErrPartNotFound},
		"a small part in the middle": {[]object.Part{big, small, tail}, domain.ErrPartTooSmall},
	} {
		_, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
			Bucket: "b", Key: "k", UploadID: u.ID, Parts: listed(tt.parts...), ETag: "x",
		})
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
	}
	if _, _, err := f.get("b", "k", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("a refused completion made an object: %v", err)
	}
	if f.shardCount() != 3*sixDrives {
		t.Errorf("a refused completion changed what is stored: %d shards", f.shardCount())
	}
	// The upload is still there, and can be completed.
	if v := f.complete(t, u, big, tail); v.Size != object.MinPartSize+4 {
		t.Errorf("size = %d", v.Size)
	}
	for name, ref := range map[string]app.UploadRef{
		"another key":    {Bucket: "b", Key: "other", UploadID: u.ID},
		"no such upload": {Bucket: "b", Key: "k", UploadID: object.ID{1}},
		"no such bucket": {Bucket: "missing", Key: "k", UploadID: u.ID},
	} {
		_, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
			Bucket: ref.Bucket, Key: ref.Key, UploadID: ref.UploadID, Number: 1, Body: bytes.NewReader([]byte("x")), Size: 1,
		})
		if !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("UploadPart to %s = %v", name, err)
		}
	}
	if f.shardCount() != 2*sixDrives {
		t.Errorf("a refused part left shards: %d stored", f.shardCount())
	}
	for _, number := range []int{0, -1, object.MaxParts + 1} {
		_, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{Bucket: "b", Key: "k", UploadID: u.ID, Number: number, Body: bytes.NewReader(nil)})
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("UploadPart number %d = %v", number, err)
		}
	}
}

// TestAbortUpload: an aborted upload leaves nothing, in the index or on the
// drives.
func TestAbortUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	u := f.createUpload(t, "k")
	f.uploadPart(t, u, 1, randomBytes(t, 5000))
	part := f.uploadPart(t, u, 2, randomBytes(t, 7000))
	ref := app.UploadRef{Bucket: "b", Key: "k", UploadID: u.ID}
	if err := f.objects.AbortUpload(ctx, porttest.Root(), app.UploadRef{Bucket: "b", Key: "other", UploadID: u.ID}); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("AbortUpload under another key = %v", err)
	}
	if err := f.objects.AbortUpload(ctx, porttest.Root(), ref); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	if f.shardCount() != 0 {
		t.Errorf("%d shards left after the upload was aborted", f.shardCount())
	}
	if err := f.objects.AbortUpload(ctx, porttest.Root(), ref); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("aborting twice = %v", err)
	}
	if _, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{UploadRef: ref, Max: 10}); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("ListParts after abort = %v", err)
	}
	if _, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "b", Key: "k", UploadID: u.ID, Parts: listed(part), ETag: "x",
	}); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("CompleteUpload after abort = %v", err)
	}
}

// TestListUploads: uploads in progress are listed by key, with prefix,
// delimiter and markers as objects are.
func TestListUploads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	for _, key := range []string{"a", "photos/1", "photos/1", "photos/2", "videos/x/1", "z"} {
		f.createUpload(t, key)
	}
	list := func(in app.ListUploadsInput) app.ListUploadsOutput {
		t.Helper()
		in.Bucket = "b"
		out, err := f.objects.ListUploads(ctx, porttest.Root(), in)
		if err != nil {
			t.Fatalf("ListUploads(%+v): %v", in, err)
		}
		return out
	}
	keys := func(out app.ListUploadsOutput) []string {
		var ks []string
		for _, u := range out.Uploads {
			ks = append(ks, u.Key)
		}
		return ks
	}
	if got := keys(list(app.ListUploadsInput{Max: 100})); !slices.Equal(got, []string{"a", "photos/1", "photos/1", "photos/2", "videos/x/1", "z"}) {
		t.Errorf("all = %v", got)
	}
	if got := keys(list(app.ListUploadsInput{Max: 100, Prefix: "photos/"})); !slices.Equal(got, []string{"photos/1", "photos/1", "photos/2"}) {
		t.Errorf("prefix photos/ = %v", got)
	}
	grouped := list(app.ListUploadsInput{Max: 100, Delimiter: "/"})
	if !slices.Equal(keys(grouped), []string{"a", "z"}) || !slices.Equal(grouped.CommonPrefixes, []string{"photos/", "videos/"}) {
		t.Errorf("delimiter / = %v, %v", keys(grouped), grouped.CommonPrefixes)
	}
	// Page by page, every upload once.
	var seen []object.Upload
	in := app.ListUploadsInput{Max: 2}
	for range 10 {
		page := list(in)
		seen = append(seen, page.Uploads...)
		if !page.Truncated {
			break
		}
		in.KeyMarker, in.UploadIDMarker = page.NextKeyMarker, page.NextUploadIDMarker
	}
	if len(seen) != 6 {
		t.Errorf("paging visited %d uploads, want 6", len(seen))
	}
	// Page by page with a delimiter: every entry once.
	var entries []string
	in = app.ListUploadsInput{Max: 1, Delimiter: "/"}
	for range 10 {
		page := list(in)
		entries = append(entries, keys(page)...)
		entries = append(entries, page.CommonPrefixes...)
		if !page.Truncated {
			break
		}
		in.KeyMarker, in.UploadIDMarker = page.NextKeyMarker, page.NextUploadIDMarker
	}
	if !slices.Equal(entries, []string{"a", "photos/", "videos/", "z"}) {
		t.Errorf("paging with a delimiter = %v", entries)
	}
	// A key marker without an upload ID continues after every upload of
	// that key.
	if got := keys(list(app.ListUploadsInput{Max: 100, KeyMarker: "photos/1"})); !slices.Equal(got, []string{"photos/2", "videos/x/1", "z"}) {
		t.Errorf("after photos/1 = %v", got)
	}
	if out := list(app.ListUploadsInput{Max: 0}); len(out.Uploads) != 0 || out.Truncated {
		t.Errorf("max 0 = %+v", out)
	}
	if _, err := f.objects.ListUploads(ctx, porttest.Root(), app.ListUploadsInput{Bucket: "missing", Max: 10}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("ListUploads of a missing bucket = %v", err)
	}
	if _, err := f.objects.ListUploads(ctx, porttest.Root(), app.ListUploadsInput{Bucket: "missing"}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("ListUploads of a missing bucket, max 0 = %v", err)
	}
}

// TestPartsAreEncryptedUnderKeysOfTheirOwn: the same bytes sent as two parts
// are stored as two different ciphertexts, and neither holds the plaintext.
func TestPartsAreEncryptedUnderKeysOfTheirOwn(t *testing.T) {
	t.Parallel()
	f := newFixture(t) // one data shard: a shard is the ciphertext
	f.createBucket(t, "b")
	u := f.createUpload(t, "k")
	data := bytes.Repeat([]byte("the same plaintext, twice. "), 400)
	one, two := f.uploadPart(t, u, 1, data), f.uploadPart(t, u, 2, data)
	stored := func(p object.Part) []byte {
		t.Helper()
		for _, d := range f.drives {
			if raw, ok := d.Bytes(object.ShardID{DataID: p.DataID, Index: 0}); ok {
				return raw
			}
		}
		t.Fatalf("part %d is not stored", p.Number)
		return nil
	}
	a, b := stored(one), stored(two)
	if bytes.Equal(a, b) {
		t.Error("two parts with the same content are stored as the same bytes")
	}
	for _, raw := range [][]byte{a, b} {
		if bytes.Contains(raw, []byte("the same plaintext")) {
			t.Error("a stored part holds its plaintext")
		}
	}
	// Each is encrypted under the key derived for it - from the upload's
	// data key, its number and its data ID - and under no other: the
	// stored bytes open with that key alone.
	uploadKey, err := f.kms.UnwrapDataKey(context.Background(), u.Encryption.WrappedKey, object.UploadKeyContext(u.ID))
	if err != nil {
		t.Fatalf("UnwrapDataKey: %v", err)
	}
	open := func(p object.Part, number int, dataID object.ID, raw []byte) ([]byte, error) {
		key, err := object.PartKey(uploadKey, number, dataID)
		if err != nil {
			return nil, err
		}
		stream, err := object.NewStream(key, p.NoncePrefix, p.DataID[:], u.Encryption.SegmentSize)
		if err != nil {
			return nil, err
		}
		size := stream.CiphertextSize(p.Size)
		whole := object.Range{Offset: 0, Length: p.Size}
		return io.ReadAll(stream.Decrypt(bytes.NewReader(raw[:size]), stream.Locate(whole, p.Size), p.Size))
	}
	if got, err := open(one, 1, one.DataID, a); err != nil || !bytes.Equal(got, data) {
		t.Errorf("part 1 under its own key: %d bytes, %v", len(got), err)
	}
	if _, err := open(one, 2, one.DataID, a); err == nil {
		t.Error("part 1 opens under the key of another part number")
	}
	if _, err := open(one, 1, two.DataID, a); err == nil {
		t.Error("part 1 opens under the key of other data")
	}
	// The key is wrapped as an upload's key: it does not unwrap as the key
	// of a stream with the same ID.
	if _, err := f.kms.UnwrapDataKey(context.Background(), u.Encryption.WrappedKey, u.ID[:]); err == nil {
		t.Error("the key of an upload unwraps as the key of a stream")
	}
	if stream, err := object.NewStream(uploadKey, one.NoncePrefix, one.DataID[:], u.Encryption.SegmentSize); err == nil {
		whole := object.Range{Offset: 0, Length: one.Size}
		if _, err := io.ReadAll(stream.Decrypt(bytes.NewReader(a[:stream.CiphertextSize(one.Size)]), stream.Locate(whole, one.Size), one.Size)); err == nil {
			t.Error("part 1 opens under the upload's data key itself")
		}
	}

	// A node with another KMS key cannot begin to read the object.
	f.complete(t, u, two)
	otherKMS, err := porttest.NewKMS("test:2")
	if err != nil {
		t.Fatal(err)
	}
	other := f.objectService(t, otherKMS)
	if _, err := other.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: "k"}); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("Get with another KMS key = %v, want %v", err, domain.ErrKeyUnavailable)
	}
}

// TestGetWithTamperedParts: the list of parts of an object is sealed. An
// object with a part moved, dropped, swapped for another or cut off is not
// read as an object: reading fails, and no byte that is returned is wrong.
func TestGetWithTamperedParts(t *testing.T) {
	t.Parallel()
	shift := func(parts []object.Part) []object.Part {
		var offset int64
		for i := range parts {
			parts[i].Index, parts[i].Offset = i, offset
			offset += parts[i].Size
		}
		return parts
	}
	tests := []struct {
		name string
		// tamper changes the parts; it may also change the version.
		tamper  func(parts []object.Part, other []object.Part) []object.Part
		version func(v *object.Version)
	}{
		{name: "two parts swapped", tamper: func(p, _ []object.Part) []object.Part { p[0], p[1] = p[1], p[0]; return shift(p) }},
		{
			name: "the last part dropped, and the size with it", tamper: func(p, _ []object.Part) []object.Part { return p[:2] },
			version: func(v *object.Version) { v.Size -= 1000; v.Parts = 2 },
		},
		{
			name: "the first part dropped, and the size with it", tamper: func(p, _ []object.Part) []object.Part { return shift(p[1:]) },
			version: func(v *object.Version) { v.Size -= object.MinPartSize + 17; v.Parts = 2 },
		},
		{name: "a part of another object in its place", tamper: func(p, other []object.Part) []object.Part {
			other[1].Index, other[1].Offset = p[1].Index, p[1].Offset
			p[1] = other[1]
			return p
		}},
		{name: "a part said to be another number", tamper: func(p, _ []object.Part) []object.Part { p[0].Number = 3; return p }},
		{name: "a part said to hold other data", tamper: func(p, other []object.Part) []object.Part {
			p[0].DataID, p[0].Layout = other[0].DataID, other[0].Layout
			return p
		}},
		{name: "a part said to be shorter", tamper: func(p, _ []object.Part) []object.Part { p[0].Size -= 10; return p }},
		{name: "a seal changed", tamper: func(p, _ []object.Part) []object.Part { p[0].Seal[0] ^= 1; return p }},
		{name: "a nonce prefix changed", tamper: func(p, _ []object.Part) []object.Part { p[0].NoncePrefix[0] ^= 1; return p }},
		{
			name: "the object said to be empty", tamper: func(p, _ []object.Part) []object.Part { return p },
			version: func(v *object.Version) { v.Size = 0 },
		},
		{
			name: "the object said to be empty, of one part", tamper: func(p, _ []object.Part) []object.Part { return p },
			version: func(v *object.Version) { v.Size, v.Parts = 0, 1 },
		},
		{
			name: "the object said to be empty, its parts gone", tamper: func(_, _ []object.Part) []object.Part { return nil },
			version: func(v *object.Version) { v.Size, v.Parts = 0, 1 },
		},
		{
			name: "the object said to be made of more parts", tamper: func(p, _ []object.Part) []object.Part { return p },
			version: func(v *object.Version) { v.Parts = 4 },
		},
		{
			name: "the object said not to be made of parts", tamper: func(p, _ []object.Part) []object.Part { return p },
			version: func(v *object.Version) { v.Parts = 0 },
		},
		{
			name: "the key said to encrypt a stream", tamper: func(p, _ []object.Part) []object.Part { return p },
			version: func(v *object.Version) { v.Encryption.KeyMode = "" },
		},
		{
			name: "the wrapped key of another object", tamper: func(p, _ []object.Part) []object.Part { return p },
			version: func(v *object.Version) { v.Encryption.WrappedKey[len(v.Encryption.WrappedKey)-1] ^= 1 },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := erasureFixture(t)
			version, _, want := f.threeParts(t, "movie")
			_, otherParts, _ := f.threeParts(t, "other")
			f.meta.TamperParts(version.DataID, func(parts []object.Part) []object.Part { return tt.tamper(parts, otherParts) })
			if tt.version != nil {
				f.meta.Tamper("b", "movie", tt.version)
			}

			out, err := f.objects.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: "movie"})
			var got []byte
			if err == nil {
				got, err = io.ReadAll(out.Body)
				_ = out.Body.Close()
			}
			if err == nil {
				t.Fatalf("Get read %d bytes without an error", len(got))
			}
			if !errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, domain.ErrKeyUnavailable) && !errors.Is(err, domain.ErrUnsupportedFormat) &&
				!errors.Is(err, domain.ErrShardNotFound) {
				t.Errorf("Get = %v, want an integrity error", err)
			}
			// Whatever was returned before the failure is the object's
			// own beginning, never anything else.
			if !bytes.HasPrefix(want, got) {
				t.Errorf("Get returned %d bytes that are not the beginning of the object", len(got))
			}
			// A range inside the damage fails too.
			from, to := int64(object.MinPartSize), int64(object.MinPartSize+100)
			if tt.name != "the last part dropped, and the size with it" && tt.name != "the object said to be made of more parts" {
				return
			}
			if got, _, err := f.get("b", "movie", &object.RangeSpec{First: from, Last: to}); err == nil {
				t.Errorf("a range of the tampered object read %d bytes without an error", len(got))
			}
		})
	}
}

// TestMultipartHealAndScrub: every part of an object made of parts is
// checked and repaired like an object.
func TestMultipartHealAndScrub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	version, parts, want := f.threeParts(t, "movie")
	drivesOf := func(p object.Part) []*porttest.MemShardStore {
		return f.shardsOf(t, object.Version{Layout: p.Layout})
	}
	// Two shards of the first part and one of the last are damaged.
	for _, i := range []int{0, 4} {
		drivesOf(parts[0])[i].Damage(object.ShardID{DataID: parts[0].DataID, Index: i})
	}
	drivesOf(parts[2])[1].Damage(object.ShardID{DataID: parts[2].DataID, Index: 1})

	// A read works around the damage ...
	if got, _, err := f.get("b", "movie", nil); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Get with damaged shards: %d bytes, %v", len(got), err)
	}
	// ... and healing repairs it: every shard of every part is looked at.
	result, err := f.objects.Healer().HealVersion(ctx, "b", "movie", version.Ref())
	if err != nil {
		t.Fatalf("HealVersion: %v", err)
	}
	if result.Shards != 3*sixDrives || result.Damaged != 3 || result.Rebuilt != 3 || result.Unrecoverable {
		t.Errorf("heal = %+v, want 18 shards, 3 damaged, 3 rebuilt", result)
	}
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
	})
	stats, err := scrubber.RunOnce(ctx)
	var stored int64
	for _, p := range parts {
		for _, s := range p.Layout.Shards {
			stored += s.Size
		}
	}
	if err != nil || stats.Objects != 1 || stats.Damaged != 0 || stats.Bytes != stored {
		t.Errorf("scrub after heal = %+v, %v; want one healthy object of %d stored bytes", stats, err, stored)
	}

	// More damage than parity covers, in one part: the object is lost,
	// and reading it says so rather than returning what is left.
	for i := range sixParity + 1 {
		drivesOf(parts[1])[i].Damage(object.ShardID{DataID: parts[1].DataID, Index: i})
	}
	if result, err := f.objects.Healer().HealVersion(ctx, "b", "movie", version.Ref()); !errors.Is(err, domain.ErrIntegrity) || !result.Unrecoverable {
		t.Errorf("heal of a part damaged beyond parity = %+v, %v", result, err)
	}
	out, err := f.objects.Get(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "movie"})
	if err == nil {
		got, readErr := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if !errors.Is(readErr, domain.ErrIntegrity) || !bytes.HasPrefix(want, got) || len(got) > object.MinPartSize+17 {
			t.Errorf("reading the lost object: %d bytes, %v", len(got), readErr)
		}
	} else if !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("Get of the lost object = %v", err)
	}
	// The part before the lost one can still be read.
	last := int64(object.MinPartSize)
	if got, _, err := f.get("b", "movie", &object.RangeSpec{First: 0, Last: last}); err != nil || !bytes.Equal(got, want[:last+1]) {
		t.Errorf("the healthy first part: %d bytes, %v", len(got), err)
	}
}

// TestPartsInProgressAreHealedAndScrubbed: a part is acknowledged data from
// the moment it is committed, long before its upload is completed. One
// committed on a quorum but not on every drive is rebuilt at once, and the
// scrubber checks every part of every upload in progress.
func TestPartsInProgressAreHealedAndScrubbed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	u := f.createUpload(t, "k")
	f.drives[0].FailCreate = errBoom
	part := f.uploadPart(t, u, 1, randomBytes(t, 4000))
	f.drives[0].FailCreate = nil
	if f.shardCount() != sixDrives-1 {
		t.Fatalf("%d shards stored, want one missing", f.shardCount())
	}
	f.runHealer(t, func() bool { return len(f.heals.list()) > 0 })
	if heals := f.heals.list(); len(heals) != 1 || heals[0].Rebuilt != 1 || heals[0].Key != "k" {
		t.Errorf("heals = %+v, want the one missing shard rebuilt", heals)
	}
	if f.shardCount() != sixDrives {
		t.Errorf("%d shards stored after the heal, want %d", f.shardCount(), sixDrives)
	}

	// Bitrot in a part nobody has read yet is found by the scrubber.
	second := f.uploadPart(t, u, 2, randomBytes(t, 9000))
	drives := f.shardsOf(t, object.Version{Layout: second.Layout})
	drives[3].Damage(object.ShardID{DataID: second.DataID, Index: 3})
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Uploads: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
	})
	stats, err := scrubber.RunOnce(ctx)
	if err != nil || stats.Parts != 2 || stats.Damaged != 1 || stats.ShardsRebuilt != 1 || stats.Unrecoverable != 0 || stats.Bytes <= 0 {
		t.Errorf("scrub = %+v, %v; want two parts checked, one damaged and rebuilt", stats, err)
	}
	if again, err := scrubber.RunOnce(ctx); err != nil || again.Damaged != 0 || again.Parts != 2 {
		t.Errorf("second scrub = %+v, %v", again, err)
	}
	// Healing a part that is no longer there does nothing and is no error.
	if err := f.objects.AbortUpload(ctx, porttest.Root(), app.UploadRef{Bucket: "b", Key: "k", UploadID: u.ID}); err != nil {
		t.Fatal(err)
	}
	f.objects.Healer().EnqueuePart("b", "k", u.ID, part.Number)
	if stats, err := scrubber.RunOnce(ctx); err != nil || stats.Parts != 0 {
		t.Errorf("scrub after abort = %+v, %v", stats, err)
	}
	if f.shardCount() != 0 {
		t.Errorf("%d shards left", f.shardCount())
	}
}

// TestAnObjectMissingPartsIsNotHealthy: a version that says it has more parts
// than there are records of is reported, not passed over.
func TestAnObjectMissingPartsIsNotHealthy(t *testing.T) {
	t.Parallel()
	f := erasureFixture(t)
	version, _, _ := f.threeParts(t, "movie")
	f.meta.TamperParts(version.DataID, func(parts []object.Part) []object.Part { return parts[:2] })
	result, err := f.objects.Healer().HealVersion(context.Background(), "b", "movie", version.Ref())
	if !errors.Is(err, domain.ErrIntegrity) || !result.Unrecoverable {
		t.Errorf("heal of an object with a part record missing = %+v, %v", result, err)
	}
}

// TestUploadPartIsNotAcknowledgedBeforeItIsCommitted: a part whose record is
// refused leaves no shard, and one whose outcome is unknown keeps its shards.
func TestUploadPartIsNotAcknowledgedBeforeItIsCommitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	u := f.createUpload(t, "k")
	send := func() error {
		_, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
			Bucket: "b", Key: "k", UploadID: u.ID, Number: 1, Body: bytes.NewReader([]byte("part")), Size: 4,
		})
		return err
	}
	f.meta.FailParts = errBoom
	if err := send(); !errors.Is(err, errBoom) {
		t.Fatalf("UploadPart with a failing index = %v", err)
	}
	if f.shardCount() != 0 {
		t.Errorf("a part that was not committed left %d shards", f.shardCount())
	}
	f.meta.FailParts = domain.ErrOutcomeUnknown
	if err := send(); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("UploadPart with an unknown outcome = %v", err)
	}
	if f.shardCount() != sixDrives {
		t.Errorf("a part whose commit may still happen kept %d shards, want %d", f.shardCount(), sixDrives)
	}
	// A body that is cut short, or does not match its digest, stores
	// nothing either.
	f.meta.FailParts = nil
	before := f.shardCount()
	if _, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "b", Key: "k", UploadID: u.ID, Number: 2, Body: bytes.NewReader([]byte("short")), Size: 50,
	}); !errors.Is(err, domain.ErrIncompleteBody) {
		t.Errorf("a short body = %v", err)
	}
	if _, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "b", Key: "k", UploadID: u.ID, Number: 2, Body: bytes.NewReader([]byte("x")), Size: 1,
		Verify: func() (string, error) { return "", domain.ErrBadDigest },
	}); !errors.Is(err, domain.ErrBadDigest) {
		t.Errorf("a wrong digest = %v", err)
	}
	if f.shardCount() != before {
		t.Errorf("refused parts left shards: %d, were %d", f.shardCount(), before)
	}
	if parts, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{UploadRef: app.UploadRef{Bucket: "b", Key: "k", UploadID: u.ID}, Max: 10}); err != nil || len(parts.Parts) != 0 {
		t.Errorf("parts recorded: %+v, %v", parts.Parts, err)
	}
}

// conflictingUploads says that a part was uploaded again, a number of times,
// before it lets a completion through.
type conflictingUploads struct {
	ports.UploadStore
	conflicts int
	calls     int
}

func (c *conflictingUploads) CompleteUpload(ctx context.Context, in ports.Completion) (ports.PutResult, error) {
	c.calls++
	if c.calls <= c.conflicts {
		return ports.PutResult{}, domain.ErrConflict
	}
	return c.UploadStore.CompleteUpload(ctx, in)
}

// TestCompleteUploadStartsOverWhenAPartChanged: a part uploaded again while
// the upload is being completed makes completing look again, a few times.
func TestCompleteUploadStartsOverWhenAPartChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for conflicts, wantErr := range map[int]error{1: nil, 2: nil, 3: domain.ErrConflict} {
		f := erasureFixture(t)
		uploads := &conflictingUploads{UploadStore: f.meta, conflicts: conflicts}
		svc, err := app.NewObjectService(app.ObjectServiceConfig{
			Index: f.meta, Buckets: f.meta, Uploads: uploads, Drives: fixed(t, f.topology, asStores(f.drives)), Codec: porttest.NewCodec(),
			KMS: f.kms, Authz: f.authz, Clock: f.clock, Entropy: porttest.Entropy(), Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		u, err := svc.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{Bucket: "b", Key: "k"})
		if err != nil {
			t.Fatal(err)
		}
		part, err := svc.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
			Bucket: "b", Key: "k", UploadID: u.ID, Number: 1, Body: bytes.NewReader([]byte("data")), Size: 4,
			Verify: func() (string, error) { return "tag", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{Bucket: "b", Key: "k", UploadID: u.ID, Parts: listed(part), ETag: "e"})
		if !errors.Is(err, wantErr) {
			t.Errorf("%d conflicts: %v, want %v", conflicts, err, wantErr)
		}
	}
}

// TestMultipartPermissions: beginning, sending parts and completing are
// writing the object; aborting and listing are permissions of their own.
func TestMultipartPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	policy := func(actions ...string) iam.Principal {
		doc := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{
			Effect: iam.Allow, Actions: actions, Resources: []string{"arn:aws:s3:::b", "arn:aws:s3:::b/*"},
		}}}
		return iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{doc}}
	}
	writer, reader := policy("s3:PutObject"), policy("s3:GetObject", "s3:ListBucket")
	janitor := policy("s3:AbortMultipartUpload", "s3:ListMultipartUploadParts", "s3:ListBucketMultipartUploads")

	u, err := f.objects.CreateUpload(ctx, writer, app.CreateUploadInput{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatalf("CreateUpload by a writer: %v", err)
	}
	if u.Initiator != writer.Name() {
		t.Errorf("initiator = %q", u.Initiator)
	}
	ref := app.UploadRef{Bucket: "b", Key: "k", UploadID: u.ID}
	part, err := f.objects.UploadPart(ctx, writer, app.UploadPartInput{
		Bucket: "b", Key: "k", UploadID: u.ID, Number: 1, Body: bytes.NewReader([]byte("x")), Size: 1,
		Verify: func() (string, error) { return "tag", nil },
	})
	if err != nil {
		t.Fatalf("UploadPart by a writer: %v", err)
	}
	denied := map[string]func(p iam.Principal) error{
		"CreateUpload": func(p iam.Principal) error {
			_, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "b", Key: "k"})
			return err
		},
		"UploadPart": func(p iam.Principal) error {
			_, err := f.objects.UploadPart(ctx, p, app.UploadPartInput{Bucket: "b", Key: "k", UploadID: u.ID, Number: 2, Body: bytes.NewReader(nil)})
			return err
		},
		"CompleteUpload": func(p iam.Principal) error {
			_, err := f.objects.CompleteUpload(ctx, p, app.CompleteUploadInput{Bucket: "b", Key: "k", UploadID: u.ID, Parts: listed(part), ETag: "e"})
			return err
		},
	}
	for name, call := range denied {
		for who, p := range map[string]iam.Principal{"a reader": reader, "a janitor": janitor, "nobody": anonymous()} {
			if err := call(p); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("%s by %s = %v, want %v", name, who, err, domain.ErrAccessDenied)
			}
		}
	}
	// Writing is not the permission to abort or to list.
	if err := f.objects.AbortUpload(ctx, writer, ref); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("AbortUpload by a writer = %v", err)
	}
	if _, err := f.objects.ListParts(ctx, writer, app.ListPartsInput{UploadRef: ref, Max: 10}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("ListParts by a writer = %v", err)
	}
	if _, err := f.objects.ListUploads(ctx, reader, app.ListUploadsInput{Bucket: "b", Max: 10}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("ListUploads by a reader = %v", err)
	}
	if out, err := f.objects.ListUploads(ctx, janitor, app.ListUploadsInput{Bucket: "b", Max: 10}); err != nil || len(out.Uploads) != 1 {
		t.Errorf("ListUploads by a janitor = %+v, %v", out, err)
	}
	if out, err := f.objects.ListParts(ctx, janitor, app.ListPartsInput{UploadRef: ref, Max: 10}); err != nil || len(out.Parts) != 1 {
		t.Errorf("ListParts by a janitor = %+v, %v", out, err)
	}
	if err := f.objects.AbortUpload(ctx, janitor, ref); err != nil {
		t.Errorf("AbortUpload by a janitor = %v", err)
	}
}

// TestMultipartObjectLock: what keeps the object is said when the upload
// begins, needs the permission to say it, and holds from the moment the
// object exists.
func TestMultipartObjectLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.meta.Now = f.clock.Now
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "vault", ObjectLock: true, CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	until := testNow.Add(48 * time.Hour)
	lock := object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: until}}

	// Not in a bucket without Object Lock, not in the past, and not
	// without the permission to lock.
	if _, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{Bucket: "b", Key: "k", Lock: lock}); !errors.Is(err, domain.ErrLockNotConfigured) {
		t.Errorf("a lock in a bucket without Object Lock = %v", err)
	}
	past := object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: testNow.Add(-time.Hour)}}
	if _, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{Bucket: "vault", Key: "k", Lock: past}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a retention in the past = %v", err)
	}
	writer := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{{Version: iam.PolicyVersion, Statements: []iam.Statement{{
		Effect: iam.Allow, Actions: []string{"s3:PutObject"}, Resources: []string{"arn:aws:s3:::vault/*"},
	}}}}}
	if _, err := f.objects.CreateUpload(ctx, writer, app.CreateUploadInput{Bucket: "vault", Key: "k", Lock: lock}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a retention by one who may only write = %v", err)
	}
	if _, err := f.objects.CreateUpload(ctx, writer, app.CreateUploadInput{Bucket: "vault", Key: "k", Lock: object.Lock{LegalHold: true}}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a legal hold by one who may only write = %v", err)
	}

	u, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{Bucket: "vault", Key: "ledger", Lock: lock})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	part, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "vault", Key: "ledger", UploadID: u.ID, Number: 1, Body: bytes.NewReader([]byte("entries")), Size: 7,
		Verify: func() (string, error) { return "tag", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "vault", Key: "ledger", UploadID: u.ID, Parts: listed(part), ETag: "e",
	})
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if version.Lock.Retention == nil || version.Lock.Retention.Mode != object.Compliance || !version.Lock.Retention.Until.Equal(until) {
		t.Fatalf("lock of the completed version = %+v", version.Lock)
	}
	ref := version.Ref()
	_, err = f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "vault", Key: "ledger", Version: &ref, BypassGovernance: true})
	if !errors.Is(err, domain.ErrObjectLocked) {
		t.Errorf("deleting the locked version = %v, want %v", err, domain.ErrObjectLocked)
	}
	if f.shardCount() != sixDrives {
		t.Errorf("%d shards after the refused delete, want %d", f.shardCount(), sixDrives)
	}

	// A retention that ran out while the parts were being sent is not
	// accepted late: the completion is judged on its own time.
	late, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{Bucket: "vault", Key: "late", Lock: lock})
	if err != nil {
		t.Fatal(err)
	}
	latePart, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "vault", Key: "late", UploadID: late.ID, Number: 1, Body: bytes.NewReader([]byte("x")), Size: 1,
		Verify: func() (string, error) { return "tag", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(72 * time.Hour)
	if _, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "vault", Key: "late", UploadID: late.ID, Parts: listed(latePart), ETag: "e",
	}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("completing after the retention ran out = %v, want it refused", err)
	}
	if _, _, err := f.get("vault", "late", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("the refused completion made an object: %v", err)
	}
}

// TestReadWriteRoleRunsAWholeUpload: the built-in role that may write
// objects may also list and abort the uploads it begins.
func TestReadWriteRoleRunsAWholeUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	writer := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{iam.BuiltInPolicies()[iam.RoleReadWrite]}}
	reader := iam.Principal{AccessKeyID: "R", User: "r", Policies: []iam.Document{iam.BuiltInPolicies()[iam.RoleReadOnly]}}
	u, err := f.objects.CreateUpload(ctx, writer, app.CreateUploadInput{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ref := app.UploadRef{Bucket: "b", Key: "k", UploadID: u.ID}
	if _, err := f.objects.ListParts(ctx, writer, app.ListPartsInput{UploadRef: ref, Max: 10}); err != nil {
		t.Errorf("ListParts by read-write: %v", err)
	}
	// Asking for no parts is answered, with nothing to continue.
	if out, err := f.objects.ListParts(ctx, writer, app.ListPartsInput{UploadRef: ref, Max: 0}); err != nil || out.Truncated || len(out.Parts) != 0 {
		t.Errorf("ListParts with a limit of 0 = %+v, %v", out, err)
	}
	if _, err := f.objects.ListUploads(ctx, writer, app.ListUploadsInput{Bucket: "b", Max: 10}); err != nil {
		t.Errorf("ListUploads by read-write: %v", err)
	}
	for name, call := range map[string]func() error{
		"ListParts": func() error {
			_, err := f.objects.ListParts(ctx, reader, app.ListPartsInput{UploadRef: ref, Max: 10})
			return err
		},
		"AbortUpload": func() error { return f.objects.AbortUpload(ctx, reader, ref) },
		"CreateUpload": func() error {
			_, err := f.objects.CreateUpload(ctx, reader, app.CreateUploadInput{Bucket: "b", Key: "k"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s by read-only = %v", name, err)
		}
	}
	if err := f.objects.AbortUpload(ctx, writer, ref); err != nil {
		t.Errorf("AbortUpload by read-write: %v", err)
	}
}
