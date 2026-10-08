package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

const segment = object.DefaultSegmentSize

func TestPutAndGetRoundTrip(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, 1, segment - 1, segment, segment + 1, 3*segment + 77} {
		f := newFixture(t)
		f.createBucket(t, "b")
		data := randomBytes(t, size)

		version := f.put(t, "b", "dir/key", data)

		if version.Size != int64(size) || version.Bucket != "b" || version.Key != "dir/key" {
			t.Errorf("size %d: version = %+v", size, version)
		}
		if version.VersionID.IsZero() || version.DataID.IsZero() || version.VersionID == version.DataID {
			t.Errorf("size %d: identifiers not generated: %s, %s", size, version.VersionID, version.DataID)
		}
		if !version.ModifiedAt.Equal(testNow) {
			t.Errorf("size %d: ModifiedAt = %v, want the clock's time", size, version.ModifiedAt)
		}
		enc := version.Encryption
		if enc.Scheme != object.StreamSchemeV1 || enc.KMSKeyID != "test:1" || len(enc.WrappedKey) == 0 ||
			len(enc.NoncePrefix) != object.NoncePrefixSize || enc.SegmentSize != segment {
			t.Errorf("size %d: encryption = %+v", size, enc)
		}
		layout := version.Layout
		if layout.DataShards != 1 || layout.ParityShards != 0 || len(layout.Shards) != 1 ||
			layout.Shards[0].Size != layout.StoredSize || layout.StoredSize <= int64(size) {
			t.Errorf("size %d: layout = %+v", size, layout)
		}

		// Encrypted at rest: what the drive holds is not the object.
		stored, ok := f.driveOf(t, version).Bytes(object.ShardID{DataID: version.DataID})
		if !ok || int64(len(stored)) != layout.StoredSize {
			t.Fatalf("size %d: shard missing or wrong size", size)
		}
		if size >= 32 && bytes.Contains(stored, data[:32]) {
			t.Errorf("size %d: the drive holds plaintext", size)
		}

		got, out, err := f.get("b", "dir/key", nil)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: Get: %v", size, err)
		}
		if out.Range != (object.Range{Offset: 0, Length: int64(size)}) || out.Version.DataID != version.DataID {
			t.Errorf("size %d: Get output = %+v", size, out.Range)
		}
	}
}

func TestPutStoresRepresentationMetadataAndETag(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	in := app.PutInput{
		Bucket: "b", Key: "k", Body: strings.NewReader("hello"), Size: -1,
		ContentType: "text/plain", ContentEncoding: "identity", ContentDisposition: "inline",
		ContentLanguage: "en", CacheControl: "max-age=60", UserMetadata: map[string]string{"owner": "albie"},
		Verify: func() (string, error) { return "etag-from-adapter", nil },
	}
	if _, err := f.objects.Put(context.Background(), porttest.Root(), in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := f.objects.Head(context.Background(), porttest.Root(), "b", "k")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if got.ETag != "etag-from-adapter" || got.Size != 5 || got.ContentType != "text/plain" || got.ContentEncoding != "identity" ||
		got.ContentDisposition != "inline" || got.ContentLanguage != "en" || got.CacheControl != "max-age=60" ||
		!reflect.DeepEqual(got.UserMetadata, map[string]string{"owner": "albie"}) {
		t.Errorf("Head = %+v", got)
	}
}

// TestWritePathOrder pins the rule of the write path: the shard is durable
// before the version is committed through Raft, and Put returns only after
// both.
func TestWritePathOrder(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("first"))
	if got, want := f.events.list(), []string{"shard.commit", "index.put"}; !slices.Equal(got, want) {
		t.Fatalf("write path order = %v, want %v", got, want)
	}

	// An overwrite deletes the replaced data only after the new version is
	// committed.
	f.put(t, "b", "k", []byte("second"))
	if got, want := f.events.list()[2:], []string{"shard.commit", "index.put", "shard.delete"}; !slices.Equal(got, want) {
		t.Fatalf("overwrite order = %v, want %v", got, want)
	}
	if f.shardCount() != 1 {
		t.Errorf("%d shards after an overwrite, want 1", f.shardCount())
	}
	if got, _, err := f.get("b", "k", nil); err != nil || string(got) != "second" {
		t.Errorf("Get after overwrite = %q, %v", got, err)
	}
}

// TestFailedPutLeavesNothingBehind covers every way a write can fail: the
// object must not become visible and its shard must not stay on a drive.
func TestFailedPutLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	body := func() io.Reader { return bytes.NewReader(bytes.Repeat([]byte("x"), 1000)) }

	tests := []struct {
		name     string
		sabotage func(f *fixture)
		input    func(in *app.PutInput)
		wantErr  error
	}{
		{name: "bucket does not exist", input: func(in *app.PutInput) { in.Bucket = "missing" }, wantErr: domain.ErrBucketNotFound},
		{name: "empty key", input: func(in *app.PutInput) { in.Key = "" }, wantErr: domain.ErrInvalidObjectKey},
		{name: "key too long", input: func(in *app.PutInput) { in.Key = strings.Repeat("k", 1025) }, wantErr: domain.ErrInvalidObjectKey},
		{name: "metadata too large", input: func(in *app.PutInput) {
			in.UserMetadata = map[string]string{"k": strings.Repeat("v", object.MaxUserMetadataSize)}
		}, wantErr: domain.ErrInvalidArgument},
		{name: "declared size over the limit", input: func(in *app.PutInput) { in.Size = object.MaxSize + 1 }, wantErr: domain.ErrEntityTooLarge},
		{name: "body shorter than declared", input: func(in *app.PutInput) { in.Size = 2000 }, wantErr: domain.ErrIncompleteBody},
		{name: "body longer than declared", input: func(in *app.PutInput) { in.Size = 10 }, wantErr: domain.ErrInvalidArgument},
		{name: "body fails", input: func(in *app.PutInput) {
			in.Body = io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom))
			in.Size = -1
		}, wantErr: boom},
		{name: "digest mismatch", input: func(in *app.PutInput) {
			in.Verify = func() (string, error) { return "", domain.ErrBadDigest }
		}, wantErr: domain.ErrBadDigest},
		{name: "KMS unavailable", sabotage: func(f *fixture) { f.kms.FailGenerate = boom }, wantErr: boom},
		{name: "drive refuses the shard", sabotage: func(f *fixture) {
			for _, d := range f.drives {
				d.FailCreate = boom
			}
		}, wantErr: domain.ErrInsufficientDrives},
		{name: "drive write fails", sabotage: func(f *fixture) {
			for _, d := range f.drives {
				d.FailWrite = boom
			}
		}, wantErr: domain.ErrInsufficientDrives},
		{name: "drive cannot make the shard durable", sabotage: func(f *fixture) {
			for _, d := range f.drives {
				d.FailCommit = boom
			}
		}, wantErr: domain.ErrInsufficientDrives},
		{name: "raft commit fails", sabotage: func(f *fixture) { f.meta.FailWrites = boom }, wantErr: boom},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucket(t, "b")
			if tt.sabotage != nil {
				tt.sabotage(f)
			}
			in := app.PutInput{Bucket: "b", Key: "k", Body: body(), Size: 1000}
			if tt.input != nil {
				tt.input(&in)
			}

			_, err := f.objects.Put(context.Background(), porttest.Root(), in)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "object.Put: ") {
				t.Errorf("error %q is not wrapped with the use case name", err)
			}
			f.meta.FailWrites = nil
			if _, err := f.meta.GetObject(context.Background(), "b", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
				t.Errorf("the object became visible despite the failure: %v", err)
			}
			if n := f.shardCount(); n != 0 {
				t.Errorf("%d shards left on the drives", n)
			}
			if slices.Contains(f.events.list(), "index.put") {
				t.Error("a version was committed")
			}
		})
	}
}

func TestFailedOverwriteKeepsThePreviousVersion(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("original"))

	f.meta.FailWrites = errors.New("raft unavailable")
	_, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("new"), Size: 3})
	if err == nil {
		t.Fatal("Put succeeded although the commit failed")
	}
	f.meta.FailWrites = nil

	if got, _, err := f.get("b", "k", nil); err != nil || string(got) != "original" {
		t.Errorf("Get = %q, %v; want the original object", got, err)
	}
	if f.shardCount() != 1 {
		t.Errorf("%d shards, want only the original's", f.shardCount())
	}
}

func TestShardCleanupFailureIsLoggedNotReturned(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("original"))
	for _, d := range f.drives {
		d.FailDelete = errors.New("drive is read-only")
	}

	// The overwrite and the delete are committed; only the cleanup failed.
	f.put(t, "b", "k", []byte("replacement"))
	if _, err := f.objects.Delete(context.Background(), porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
		t.Fatalf("Delete = %v, want nil: the object is gone from the index", err)
	}
	if _, _, err := f.get("b", "k", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if got := strings.Count(f.logs.String(), "shard left behind for garbage collection"); got != 2 {
		t.Errorf("%d orphan warnings logged, want 2:\n%s", got, f.logs.String())
	}
}

func TestGetRanges(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	const size = 3*segment + 100
	data := randomBytes(t, size)
	f.put(t, "b", "k", data)

	tests := []struct {
		name    string
		spec    object.RangeSpec
		want    object.Range
		invalid bool
	}{
		{name: "first bytes", spec: object.RangeSpec{First: 0, Last: 9}, want: object.Range{Offset: 0, Length: 10}},
		{name: "inside one segment", spec: object.RangeSpec{First: 100, Last: 199}, want: object.Range{Offset: 100, Length: 100}},
		{name: "across a segment boundary", spec: object.RangeSpec{First: segment - 5, Last: segment + 4}, want: object.Range{Offset: segment - 5, Length: 10}},
		{name: "exactly one middle segment", spec: object.RangeSpec{First: segment, Last: 2*segment - 1}, want: object.Range{Offset: segment, Length: segment}},
		{name: "to the end", spec: object.RangeSpec{First: size - 50, Last: -1}, want: object.Range{Offset: size - 50, Length: 50}},
		{name: "suffix", spec: object.RangeSpec{Suffix: 150}, want: object.Range{Offset: size - 150, Length: 150}},
		{name: "last byte", spec: object.RangeSpec{First: size - 1, Last: size - 1}, want: object.Range{Offset: size - 1, Length: 1}},
		{name: "everything", spec: object.RangeSpec{First: 0, Last: -1}, want: object.Range{Offset: 0, Length: size}},
		{name: "beyond the end", spec: object.RangeSpec{First: size, Last: -1}, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, out, err := f.get("b", "k", &tt.spec)
			if tt.invalid {
				if !errors.Is(err, domain.ErrInvalidRange) {
					t.Fatalf("error = %v, want %v", err, domain.ErrInvalidRange)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if out.Range != tt.want {
				t.Errorf("Range = %+v, want %+v", out.Range, tt.want)
			}
			if !bytes.Equal(got, data[tt.want.Offset:tt.want.Offset+tt.want.Length]) {
				t.Error("wrong bytes for the range")
			}
		})
	}

	t.Run("any range of an empty object", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.createBucket(t, "b")
		f.put(t, "b", "empty", nil)
		if _, _, err := f.get("b", "empty", &object.RangeSpec{First: 0, Last: -1}); !errors.Is(err, domain.ErrInvalidRange) {
			t.Errorf("error = %v, want %v", err, domain.ErrInvalidRange)
		}
	})
}

// TestGetNeverReturnsDamagedData is the read-side integrity rule.
func TestGetNeverReturnsDamagedData(t *testing.T) {
	t.Parallel()
	const size = 3*segment + 100
	sealed := segment + object.TagSize

	tests := []struct {
		name   string
		damage func(t *testing.T, f *fixture, v object.Version, stored []byte)
		// atOpen: the failure must be reported by Get itself, before any
		// byte could be sent to a client.
		atOpen  bool
		wantErr error
		// verified is how many bytes may precede the error.
		verified int
	}{
		{
			name: "bit flip in the first segment", atOpen: true, wantErr: domain.ErrIntegrity,
			damage: func(t *testing.T, f *fixture, v object.Version, s []byte) {
				s[10] ^= 1
				f.driveOf(t, v).Corrupt(object.ShardID{DataID: v.DataID}, s)
			},
		},
		{
			name: "bit flip in a later segment", wantErr: domain.ErrIntegrity, verified: 2 * segment,
			damage: func(t *testing.T, f *fixture, v object.Version, s []byte) {
				s[2*sealed+10] ^= 1
				f.driveOf(t, v).Corrupt(object.ShardID{DataID: v.DataID}, s)
			},
		},
		{
			name: "segments swapped", atOpen: true, wantErr: domain.ErrIntegrity,
			damage: func(t *testing.T, f *fixture, v object.Version, s []byte) {
				first := bytes.Clone(s[:sealed])
				copy(s, s[sealed:2*sealed])
				copy(s[sealed:], first)
				f.driveOf(t, v).Corrupt(object.ShardID{DataID: v.DataID}, s)
			},
		},
		{
			name: "shard truncated", atOpen: true, wantErr: domain.ErrIntegrity,
			damage: func(t *testing.T, f *fixture, v object.Version, s []byte) {
				f.driveOf(t, v).Corrupt(object.ShardID{DataID: v.DataID}, s[:2*sealed])
			},
		},
		{
			name: "shard extended", atOpen: true, wantErr: domain.ErrIntegrity,
			damage: func(t *testing.T, f *fixture, v object.Version, s []byte) {
				f.driveOf(t, v).Corrupt(object.ShardID{DataID: v.DataID}, append(s, 0))
			},
		},
		{
			name: "shard missing", atOpen: true, wantErr: domain.ErrShardNotFound,
			damage: func(t *testing.T, f *fixture, v object.Version, _ []byte) {
				_ = f.driveOf(t, v).Delete(context.Background(), object.ShardID{DataID: v.DataID})
			},
		},
		{
			name: "shard replaced by another object's", atOpen: true, wantErr: domain.ErrIntegrity,
			damage: func(t *testing.T, f *fixture, v object.Version, _ []byte) {
				other, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
					Bucket: "b", Key: "other", Body: bytes.NewReader(make([]byte, size)), Size: size,
				})
				if err != nil {
					t.Fatalf("Put: %v", err)
				}
				theirs, _ := f.driveOf(t, other).Bytes(object.ShardID{DataID: other.DataID})
				f.driveOf(t, v).Corrupt(object.ShardID{DataID: v.DataID}, theirs)
			},
		},
		{
			name: "drive cannot open the shard", atOpen: true, wantErr: errBoom,
			damage: func(_ *testing.T, f *fixture, _ object.Version, _ []byte) {
				for _, d := range f.drives {
					d.FailOpen = errBoom
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucket(t, "b")
			data := randomBytes(t, size)
			version := f.put(t, "b", "k", data)
			drive := f.driveOf(t, version)
			stored, _ := drive.Bytes(object.ShardID{DataID: version.DataID})
			tt.damage(t, f, version, stored)

			out, err := f.objects.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: "k"})
			if tt.atOpen {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Get error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = out.Body.Close() }()
			got, err := io.ReadAll(out.Body)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("read error = %v, want %v", err, tt.wantErr)
			}
			if len(got) > tt.verified || !bytes.HasPrefix(data, got) {
				t.Errorf("read %d bytes before the error; only %d precede the damage", len(got), tt.verified)
			}
		})
	}
}

var errBoom = errors.New("boom")

func TestGetWithTamperedOrForeignMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tamper  func(v *object.Version)
		wantErr error
	}{
		{"unknown encryption scheme", func(v *object.Version) { v.Encryption.Scheme = "ROT13" }, domain.ErrUnsupportedFormat},
		{"another KMS key ID", func(v *object.Version) { v.Encryption.KMSKeyID = "test:2" }, domain.ErrKeyUnavailable},
		{"wrapped key replaced", func(v *object.Version) { v.Encryption.WrappedKey = bytes.Repeat([]byte{1}, 60) }, domain.ErrKeyUnavailable},
		{"nonce prefix changed", func(v *object.Version) { v.Encryption.NoncePrefix = []byte{9, 9, 9, 9} }, domain.ErrIntegrity},
		{"malformed nonce prefix", func(v *object.Version) { v.Encryption.NoncePrefix = nil }, domain.ErrInvalidArgument},
		{"size lowered", func(v *object.Version) { v.Size -= 1 }, domain.ErrIntegrity},
		{"stored size changed", func(v *object.Version) { v.Layout.StoredSize++ }, domain.ErrIntegrity},
		{"shard size changed", func(v *object.Version) { v.Layout.Shards[0].Size-- }, domain.ErrIntegrity},
		{"unknown drive", func(v *object.Version) { v.Layout.Shards[0].DriveID = "drive-z" }, domain.ErrShardNotFound},
		{"erasure layout this binary cannot read", func(v *object.Version) { v.Layout.DataShards, v.Layout.ParityShards = 4, 2 }, domain.ErrUnsupportedFormat},
		{"data ID swapped", func(v *object.Version) { v.DataID[0] ^= 1 }, domain.ErrKeyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucket(t, "b")
			f.put(t, "b", "k", randomBytes(t, 2*segment))

			f.meta.Tamper("b", "k", tt.tamper)

			if got, _, err := f.get("b", "k", nil); !errors.Is(err, tt.wantErr) || len(got) != 0 {
				t.Errorf("Get = %d bytes, %v; want no bytes and %v", len(got), err, tt.wantErr)
			}
		})
	}
}

func TestGetWithAnotherMasterKeyFails(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("secret"))

	// Same key ID, different master key: a node pointed at the wrong key file.
	impostor, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatalf("NewKMS: %v", err)
	}
	other := f.objectService(t, impostor)
	if _, err := other.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: "k"}); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("error = %v, want %v", err, domain.ErrKeyUnavailable)
	}

	f.kms.FailUnwrap = errBoom
	if _, _, err := f.get("b", "k", nil); !errors.Is(err, errBoom) {
		t.Errorf("error with a failing KMS = %v, want %v", err, errBoom)
	}
}

func TestHeadAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("data"))

	if _, err := f.objects.Head(ctx, root, "b", "missing"); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("Head(missing) error = %v", err)
	}
	if _, err := f.objects.Head(ctx, root, "nope", "k"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("Head(missing bucket) error = %v", err)
	}
	if _, err := f.objects.Head(ctx, root, "b", ""); !errors.Is(err, domain.ErrInvalidObjectKey) {
		t.Errorf("Head(empty key) error = %v", err)
	}
	if _, _, err := f.get("nope", "k", nil); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("Get(missing bucket) error = %v", err)
	}

	if _, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.objects.Head(ctx, root, "b", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("Head after Delete error = %v", err)
	}
	if f.shardCount() != 0 {
		t.Errorf("%d shards after Delete, want 0", f.shardCount())
	}
	// Deleting what is not there succeeds, as S3 clients expect.
	if _, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
	if _, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "nope", Key: "k"}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("Delete(missing bucket) error = %v", err)
	}
	if _, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: ""}); !errors.Is(err, domain.ErrInvalidObjectKey) {
		t.Errorf("Delete(empty key) error = %v", err)
	}
	f.put(t, "b", "k2", []byte("data"))
	f.meta.FailWrites = errBoom
	if _, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k2"}); !errors.Is(err, errBoom) {
		t.Errorf("Delete with a failing commit = %v", err)
	}
	if f.shardCount() != 1 {
		t.Error("the shard was deleted although the delete was not committed")
	}
}

func TestObjectServiceAuthorizesEveryCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type call func(f *fixture, p iam.Principal, body io.Reader) error
	calls := map[iam.Action]call{
		iam.ActionPutObject: func(f *fixture, p iam.Principal, body io.Reader) error {
			_, err := f.objects.Put(ctx, p, app.PutInput{Bucket: "b", Key: "k", Body: body, Size: -1})
			return err
		},
		iam.ActionGetObject: func(f *fixture, p iam.Principal, _ io.Reader) error {
			_, err := f.objects.Get(ctx, p, app.GetInput{Bucket: "b", Key: "k"})
			return err
		},
		iam.ActionDeleteObject: func(f *fixture, p iam.Principal, _ io.Reader) error {
			_, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "b", Key: "k"})
			return err
		},
		iam.ActionListBucket: func(f *fixture, p iam.Principal, _ io.Reader) error {
			_, err := f.objects.List(ctx, p, "b", object.ListOptions{MaxKeys: 10})
			return err
		},
	}
	for action, run := range calls {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucket(t, "b")
			f.put(t, "b", "k", []byte("existing"))
			before := len(f.authz.Requests())

			// The body of a refused upload must not even be read.
			body := iotest.ErrReader(errors.New("the body was read"))
			if err := run(f, anonymous(), body); !errors.Is(err, domain.ErrAccessDenied) {
				t.Fatalf("error = %v, want %v", err, domain.ErrAccessDenied)
			}
			requests := f.authz.Requests()[before:]
			if len(requests) != 1 || requests[0].Action != action || requests[0].Resource.Bucket != "b" {
				t.Fatalf("authorizer saw %+v, want one request for %s on b", requests, action)
			}
			if action != iam.ActionListBucket && requests[0].Resource.Key != "k" {
				t.Errorf("resource = %+v, want key k", requests[0].Resource)
			}
			if got, _, err := f.get("b", "k", nil); err != nil || string(got) != "existing" {
				t.Errorf("the object changed: %q, %v", got, err)
			}
		})
	}

	t.Run("head", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		if _, err := f.objects.Head(ctx, anonymous(), "b", "k"); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("error = %v, want %v", err, domain.ErrAccessDenied)
		}
	})
}

func TestNewObjectServiceValidatesDrives(t *testing.T) {
	t.Parallel()

	two, err := cluster.NewTopology([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		topology cluster.Topology
		drives   []ports.ShardStore
	}{
		"no topology":                     {drives: []ports.ShardStore{porttest.NewMemShardStore("a")}},
		"duplicate drive IDs":             {two, []ports.ShardStore{porttest.NewMemShardStore("a"), porttest.NewMemShardStore("a")}},
		"empty drive ID":                  {two, []ports.ShardStore{porttest.NewMemShardStore("a"), porttest.NewMemShardStore("")}},
		"a drive of the topology missing": {two, []ports.ShardStore{porttest.NewMemShardStore("a")}},
	}
	for name, tt := range tests {
		if _, err := app.FixedDrives(tt.topology, tt.drives); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: error = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	if _, err := app.NewObjectService(app.ObjectServiceConfig{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("NewObjectService without drives: error = %v, want %v", err, domain.ErrInvalidArgument)
	}
}

func TestObjectsSpreadAcrossDrives(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	for i := range 40 {
		f.put(t, "b", "k"+string(rune('A'+i)), []byte("x"))
	}
	for _, d := range f.drives {
		if n := len(d.IDs()); n < 5 {
			t.Errorf("drive %s holds %d of 40 shards: placement is not spreading", d.DriveID(), n)
		}
	}
}

func TestPutWithoutEntropyFails(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	svc, err := app.NewObjectService(app.ObjectServiceConfig{
		Index: f.meta, Buckets: f.meta, Uploads: f.meta, Drives: fixed(t, f.topology, asStores(f.drives)),
		Codec: porttest.NewCodec(), KMS: f.kms,
		Authz: f.authz, Clock: f.clock, Entropy: iotest.ErrReader(errBoom),
	})
	if err != nil {
		t.Fatalf("NewObjectService: %v", err)
	}
	_, err = svc.Put(context.Background(), porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1})
	if !errors.Is(err, errBoom) {
		t.Errorf("error = %v, want %v", err, errBoom)
	}
}

// TestTruncatedUploadIsNotStored: a transport reports a body that was cut
// short with io.ErrUnexpectedEOF. With no declared length and no declared
// digest nothing else would catch it, and the prefix would be sealed and
// committed as if it were the whole object.
func TestTruncatedUploadIsNotStored(t *testing.T) {
	t.Parallel()

	for _, received := range []int{0, 1000, segment, segment + 1000} {
		f := newFixture(t)
		f.createBucket(t, "b")
		f.put(t, "b", "k", []byte("the existing object"))

		cut := io.MultiReader(bytes.NewReader(randomBytes(t, received)), iotest.ErrReader(io.ErrUnexpectedEOF))
		_, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: cut, Size: -1})

		if !errors.Is(err, domain.ErrIncompleteBody) {
			t.Fatalf("%d bytes then a cut: error = %v, want %v", received, err, domain.ErrIncompleteBody)
		}
		if got, _, err := f.get("b", "k", nil); err != nil || string(got) != "the existing object" {
			t.Errorf("%d bytes then a cut: the existing object is now %q (%v)", received, got, err)
		}
		if f.shardCount() != 1 {
			t.Errorf("%d bytes then a cut: %d shards, want only the existing object's", received, f.shardCount())
		}
	}
}

// TestShardIsKeptWhenTheCommitOutcomeIsUnknown: an error from the metadata
// store does not always mean "not committed". When the entry may still be
// applied - after a restart, say - deleting its shard would leave the index
// pointing at nothing, and for an overwrite the previous version would be
// lost as well.
func TestShardIsKeptWhenTheCommitOutcomeIsUnknown(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("original"))

	f.meta.FailWrites = fmt.Errorf("%w: raft is shutting down", domain.ErrOutcomeUnknown)
	_, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("new"), Size: 3})
	if !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("error = %v, want %v", err, domain.ErrOutcomeUnknown)
	}
	if f.shardCount() != 2 {
		t.Fatalf("%d shards on the drives, want 2: the original's and the one whose commit may still happen", f.shardCount())
	}
	if slices.Contains(f.events.list(), "shard.delete") {
		t.Error("a shard was deleted although the outcome of the commit is unknown")
	}

	// The same for a delete: nothing is removed from the drives.
	if _, err := f.objects.Delete(context.Background(), porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k"}); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("Delete error = %v, want %v", err, domain.ErrOutcomeUnknown)
	}
	if f.shardCount() != 2 {
		t.Errorf("%d shards after a delete of unknown outcome, want 2", f.shardCount())
	}
}

// TestGetDuringOverwriteAndDelete: a reader sees the old object or the new
// one (or, after a delete, none), never an internal error because the shard
// it was about to open has just been replaced.
func TestGetDuringOverwriteAndDelete(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	f.put(t, "b", "k", []byte("version 0"))

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			body := fmt.Sprintf("version %d", i)
			_, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader(body), Size: int64(len(body))})
			if err != nil {
				t.Errorf("Put: %v", err)
				return
			}
			if i%5 == 0 {
				if _, err := f.objects.Delete(context.Background(), porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}
	}()

	for range 3000 {
		got, _, err := f.get("b", "k", nil)
		switch {
		case errors.Is(err, domain.ErrObjectNotFound):
		case err != nil:
			t.Fatalf("Get during concurrent writes: %v", err)
		case !strings.HasPrefix(string(got), "version "):
			t.Fatalf("Get returned %q", got)
		}
	}
	close(stop)
	<-writerDone
}

func TestGetReportsDataThatIsReallyMissing(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.createBucket(t, "b")
	version := f.put(t, "b", "k", []byte("data"))
	if err := f.driveOf(t, version).Delete(context.Background(), object.ShardID{DataID: version.DataID}); err != nil {
		t.Fatal(err)
	}
	before := len(f.authz.Requests())

	// The same version is still current, so looking again does not help.
	if _, _, err := f.get("b", "k", nil); !errors.Is(err, domain.ErrShardNotFound) {
		t.Fatalf("error = %v, want %v", err, domain.ErrShardNotFound)
	}
	if looks := len(f.authz.Requests()) - before; looks != 2 {
		t.Errorf("Get looked at the index %d times, want 2: once more to see whether the object changed", looks)
	}
}
