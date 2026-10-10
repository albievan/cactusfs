package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// tagged stands for the adapter's digest: the entity tag is a function of
// the bytes that passed.
func tagged(body io.Reader) (io.Reader, func() (string, error)) {
	var seen bytes.Buffer
	return io.TeeReader(body, &seen), func() (string, error) { return partTag(seen.Bytes()), nil }
}

func allowing(actions ...string) iam.Principal {
	doc := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: actions, Resources: []string{"*"}}}}
	return iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{doc}}
}

// TestCopy: the bytes of one object become another, with the metadata and
// tags of the source or of the request, under keys of its own.
func TestCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "dst", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	data := randomBytes(t, 300_000)
	source, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{
		Bucket: "b", Key: "src", Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: "image/png",
		UserMetadata: map[string]string{"owner": "albie"}, Tags: map[string]string{"project": "apollo"},
		Verify: func() (string, error) { return partTag(data), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(bucketName, key string) []byte {
		t.Helper()
		got, _, err := f.get(bucketName, key, nil)
		if err != nil {
			t.Fatalf("Get %s/%s: %v", bucketName, key, err)
		}
		return got
	}
	in := app.CopyInput{
		Source:      app.GetInput{Bucket: "b", Key: "src"},
		Destination: app.PutInput{Bucket: "dst", Key: "copy", ContentType: "text/plain", UserMetadata: map[string]string{"owner": "other"}},
		Digest:      tagged,
	}
	out, err := f.objects.Copy(ctx, porttest.Root(), in)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	v := out.Version
	if !bytes.Equal(read("dst", "copy"), data) || v.Size != source.Size || v.ETag != source.ETag ||
		v.ContentType != "image/png" || v.UserMetadata["owner"] != "albie" || v.Tags["project"] != "apollo" {
		t.Errorf("the copy = %+v", v)
	}
	// It is an object of its own: other data, another key to it.
	if v.DataID == source.DataID || bytes.Equal(v.Encryption.WrappedKey, source.Encryption.WrappedKey) || out.Source.DataID != source.DataID {
		t.Error("the copy shares data or a key with its source")
	}
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "src"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read("dst", "copy"), data) {
		t.Error("the copy did not survive its source")
	}

	// With the metadata and the tags of the request.
	in.Source = app.GetInput{Bucket: "dst", Key: "copy"}
	in.Destination.Key, in.ReplaceMetadata, in.ReplaceTags = "replaced", true, true
	in.Destination.Tags = map[string]string{"stage": "final"}
	out, err = f.objects.Copy(ctx, porttest.Root(), in)
	if v := out.Version; err != nil || v.ContentType != "text/plain" || v.UserMetadata["owner"] != "other" || len(v.Tags) != 1 || v.Tags["stage"] != "final" {
		t.Errorf("a copy with replaced metadata = %+v, %v", out.Version, err)
	}
	// Onto itself: only to change something.
	same := app.CopyInput{Source: app.GetInput{Bucket: "dst", Key: "copy"}, Destination: app.PutInput{Bucket: "dst", Key: "copy"}, Digest: tagged}
	if _, err := f.objects.Copy(ctx, porttest.Root(), same); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a copy onto itself that changes nothing = %v", err)
	}
	same.ReplaceMetadata, same.Destination.ContentType = true, "application/json"
	if out, err := f.objects.Copy(ctx, porttest.Root(), same); err != nil || out.Version.ContentType != "application/json" || !bytes.Equal(read("dst", "copy"), data) {
		t.Errorf("a copy onto itself with new metadata = %+v, %v", out.Version, err)
	}
	// A range of the source.
	ranged := app.CopyInput{
		Source:      app.GetInput{Bucket: "dst", Key: "copy", Range: &object.RangeSpec{First: 100, Last: 199}},
		Destination: app.PutInput{Bucket: "dst", Key: "slice"}, Digest: tagged,
	}
	if out, err := f.objects.Copy(ctx, porttest.Root(), ranged); err != nil || out.Version.Size != 100 || !bytes.Equal(read("dst", "slice"), data[100:200]) {
		t.Errorf("a copy of a range = %+v, %v", out.Version, err)
	}

	for name, tt := range map[string]struct {
		in   app.CopyInput
		want error
	}{
		"a source that is not there":  {app.CopyInput{Source: app.GetInput{Bucket: "dst", Key: "missing"}, Destination: app.PutInput{Bucket: "dst", Key: "x"}}, domain.ErrObjectNotFound},
		"a source bucket that is not": {app.CopyInput{Source: app.GetInput{Bucket: "missing", Key: "k"}, Destination: app.PutInput{Bucket: "dst", Key: "x"}}, domain.ErrBucketNotFound},
		"a destination that is not":   {app.CopyInput{Source: app.GetInput{Bucket: "dst", Key: "copy"}, Destination: app.PutInput{Bucket: "missing", Key: "x"}}, domain.ErrBucketNotFound},
		"a condition that fails": {app.CopyInput{
			Source: app.GetInput{Bucket: "dst", Key: "copy", Conditions: object.Conditions{IfMatch: []string{"another"}}}, Destination: app.PutInput{Bucket: "dst", Key: "x"},
		}, domain.ErrPreconditionFailed},
		// "Not modified" is no answer to a copy: the copy is not made.
		"a source that has not changed": {app.CopyInput{
			Source: app.GetInput{Bucket: "dst", Key: "copy", Conditions: object.Conditions{IfNoneMatch: []string{"*"}}}, Destination: app.PutInput{Bucket: "dst", Key: "x"},
		}, domain.ErrPreconditionFailed},
		"a lock where there is none": {app.CopyInput{
			Source: app.GetInput{Bucket: "dst", Key: "copy"}, Destination: app.PutInput{Bucket: "dst", Key: "x", Lock: object.Lock{LegalHold: true}},
		}, domain.ErrLockNotConfigured},
	} {
		tt.in.Digest = tagged
		if _, err := f.objects.Copy(ctx, porttest.Root(), tt.in); !errors.Is(err, tt.want) {
			t.Errorf("a copy with %s = %v, want %v", name, err, tt.want)
		}
	}
	if _, _, err := f.get("dst", "x", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("a refused copy made an object: %v", err)
	}
}

// TestCopyPermissions: a copy is a read and a write, each with its own
// permission; tags that travel take the permissions for tags.
func TestCopyPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	if _, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "tagged", Body: bytes.NewReader([]byte("x")), Size: 1, Tags: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	f.put(t, "b", "plain", []byte("y"))
	copyOf := func(key string) app.CopyInput {
		return app.CopyInput{Source: app.GetInput{Bucket: "b", Key: key}, Destination: app.PutInput{Bucket: "b", Key: key + "-copy"}, Digest: tagged}
	}
	tests := []struct {
		name    string
		who     iam.Principal
		in      app.CopyInput
		allowed bool
	}{
		{"read and write", allowing("s3:GetObject", "s3:PutObject"), copyOf("plain"), true},
		{"only read", allowing("s3:GetObject"), copyOf("plain"), false},
		{"only write", allowing("s3:PutObject"), copyOf("plain"), false},
		{"nobody", anonymous(), copyOf("plain"), false},
		{"a tagged source, without the tag permissions", allowing("s3:GetObject", "s3:PutObject"), copyOf("tagged"), false},
		{"a tagged source, without reading tags", allowing("s3:GetObject", "s3:PutObject", "s3:PutObjectTagging"), copyOf("tagged"), false},
		{"a tagged source, without writing tags", allowing("s3:GetObject", "s3:PutObject", "s3:GetObjectTagging"), copyOf("tagged"), false},
		{"a tagged source, with both", allowing("s3:GetObject", "s3:PutObject", "s3:GetObjectTagging", "s3:PutObjectTagging"), copyOf("tagged"), true},
		{"a tagged source, its tags left behind", allowing("s3:GetObject", "s3:PutObject"), func() app.CopyInput { in := copyOf("tagged"); in.ReplaceTags = true; return in }(), true},
		{"a version of the source, without reading versions", allowing("s3:GetObject", "s3:PutObject"), func() app.CopyInput {
			in := copyOf("plain")
			in.Source.Version = &object.VersionRef{Null: true}
			return in
		}(), false},
	}
	for _, tt := range tests {
		_, err := f.objects.Copy(ctx, tt.who, tt.in)
		if (err == nil) != tt.allowed || (err != nil && !errors.Is(err, domain.ErrAccessDenied)) {
			t.Errorf("%s: %v, want allowed = %v", tt.name, err, tt.allowed)
		}
		if !tt.allowed {
			if _, _, err := f.get("b", tt.in.Destination.Key, nil); !errors.Is(err, domain.ErrObjectNotFound) {
				t.Errorf("%s: the refused copy made an object", tt.name)
			}
		} else if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: tt.in.Destination.Key}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCopyAcrossEncryptionAndParts: a copy reads whatever the source is -
// made of parts, under a customer key - and writes what the request says.
func TestCopyAcrossEncryptionAndParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	_, _, want := f.threeParts(t, "film")
	key := customerKey(1)
	out, err := f.objects.Copy(ctx, porttest.Root(), app.CopyInput{
		Source: app.GetInput{Bucket: "b", Key: "film"}, Digest: tagged,
		Destination: app.PutInput{Bucket: "b", Key: "sealed", SSE: object.SSE{Mode: object.SSEC, CustomerKey: key}},
	})
	if err != nil || out.Version.Multipart() || out.Version.Encryption.Mode != object.SSEC || out.Version.Size != int64(len(want)) {
		t.Fatalf("a multipart object copied under a customer key = %+v, %v", out.Version, err)
	}
	if got, err := f.getWith("sealed", key); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the copy read with its key: %d bytes, %v", len(got), err)
	}
	// Back out again takes the key of the source.
	back := app.CopyInput{Source: app.GetInput{Bucket: "b", Key: "sealed"}, Destination: app.PutInput{Bucket: "b", Key: "open"}, Digest: tagged}
	if _, err := f.objects.Copy(ctx, porttest.Root(), back); !errors.Is(err, domain.ErrCustomerKeyRequired) {
		t.Errorf("copying an SSE-C object without its key = %v", err)
	}
	back.Source.CustomerKey = customerKey(2)
	if _, err := f.objects.Copy(ctx, porttest.Root(), back); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("copying an SSE-C object with another key = %v", err)
	}
	back.Source.CustomerKey = key
	if out, err := f.objects.Copy(ctx, porttest.Root(), back); err != nil || out.Version.Encryption.Mode != object.SSES3 {
		t.Fatalf("copying an SSE-C object with its key = %+v, %v", out.Version.Encryption, err)
	}
	if got, _, err := f.get("b", "open", nil); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the copy out of SSE-C: %d bytes, %v", len(got), err)
	}

	// As a part of an upload: a range of the source.
	u := f.createUpload(t, "assembled")
	first, err := f.objects.CopyPart(ctx, porttest.Root(), app.CopyPartInput{
		Source: app.GetInput{Bucket: "b", Key: "open", Range: &object.RangeSpec{First: 0, Last: object.MinPartSize - 1}}, Digest: tagged,
		Part: app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u.ID, Number: 1},
	})
	if err != nil || first.Size != object.MinPartSize || first.ETag != partTag(want[:object.MinPartSize]) {
		t.Fatalf("CopyPart = %+v, %v", first, err)
	}
	second := f.uploadPart(t, u, 2, []byte("the end"))
	f.complete(t, u, first, second)
	if got, _, err := f.get("b", "assembled", nil); err != nil || !bytes.Equal(got, append(append([]byte{}, want[:object.MinPartSize]...), "the end"...)) {
		t.Errorf("the object assembled from a copied part: %d bytes, %v", len(got), err)
	}
	u2 := f.createUpload(t, "assembled")
	before := f.shardCount()
	for name, tt := range map[string]struct {
		in   app.CopyPartInput
		want error
	}{
		"an upload that is not": {app.CopyPartInput{Source: app.GetInput{Bucket: "b", Key: "open"}, Part: app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u.ID, Number: 1}}, domain.ErrUploadNotFound},
		"a source that is not":  {app.CopyPartInput{Source: app.GetInput{Bucket: "b", Key: "missing"}, Part: app.UploadPartInput{Bucket: "b", Key: "x", UploadID: u.ID, Number: 1}}, domain.ErrObjectNotFound},
		"a range past the end": {app.CopyPartInput{
			Source: app.GetInput{Bucket: "b", Key: "open", Range: &object.RangeSpec{First: int64(len(want)) + 5, Last: int64(len(want)) + 9}},
			Part:   app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u2.ID, Number: 1},
		}, domain.ErrInvalidArgument},
		// A reader would get what there is; a part shorter than was asked
		// for, without a word, is not what its author assembled.
		"a range that ends after the source": {app.CopyPartInput{
			Source: app.GetInput{Bucket: "b", Key: "open", Range: &object.RangeSpec{First: 0, Last: int64(len(want)) + 100}},
			Part:   app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u2.ID, Number: 1},
		}, domain.ErrInvalidArgument},
		"no digest": {app.CopyPartInput{
			Source: app.GetInput{Bucket: "b", Key: "open"}, Part: app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u2.ID, Number: 1},
		}, domain.ErrInvalidArgument},
	} {
		if name != "no digest" {
			tt.in.Digest = tagged
		}
		if _, err := f.objects.CopyPart(ctx, porttest.Root(), tt.in); !errors.Is(err, tt.want) {
			t.Errorf("CopyPart with %s = %v, want %v", name, err, tt.want)
		}
	}
	if parts, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{UploadRef: app.UploadRef{Bucket: "b", Key: "assembled", UploadID: u2.ID}, Max: 10}); err != nil || len(parts.Parts) != 0 || f.shardCount() != before {
		t.Errorf("a refused copy of a part stored something: %+v, %v", parts.Parts, err)
	}
	// The last byte of the source is the last a range may name.
	if whole, err := f.objects.CopyPart(ctx, porttest.Root(), app.CopyPartInput{
		Source: app.GetInput{Bucket: "b", Key: "open", Range: &object.RangeSpec{First: 0, Last: int64(len(want)) - 1}}, Digest: tagged,
		Part: app.UploadPartInput{Bucket: "b", Key: "assembled", UploadID: u2.ID, Number: 1},
	}); err != nil || whole.Size != int64(len(want)) {
		t.Errorf("a range to the last byte = %+v, %v", whole.Size, err)
	}
	if _, err := f.objects.Copy(ctx, porttest.Root(), app.CopyInput{Source: app.GetInput{Bucket: "b", Key: "open"}, Destination: app.PutInput{Bucket: "b", Key: "x"}}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Copy without a digest = %v", err)
	}

	// A customer key is rotated by copying the object onto itself; and a
	// multipart object copied onto itself frees the parts it was made of.
	rotate := app.CopyInput{
		Source:      app.GetInput{Bucket: "b", Key: "sealed", CustomerKey: key},
		Destination: app.PutInput{Bucket: "b", Key: "sealed", SSE: object.SSE{Mode: object.SSEC, CustomerKey: customerKey(2)}}, Digest: tagged,
	}
	if _, err := f.objects.Copy(ctx, porttest.Root(), rotate); err != nil {
		t.Fatalf("rotating a customer key by a copy onto itself: %v", err)
	}
	if _, err := f.getWith("sealed", key); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("the old key after rotation = %v", err)
	}
	if got, err := f.getWith("sealed", customerKey(2)); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the new key after rotation: %d bytes, %v", len(got), err)
	}
	stored := f.shardCount()
	onto := app.CopyInput{Source: app.GetInput{Bucket: "b", Key: "film"}, Destination: app.PutInput{Bucket: "b", Key: "film", ContentType: "video/mp4"}, ReplaceMetadata: true, Digest: tagged}
	if out, err := f.objects.Copy(ctx, porttest.Root(), onto); err != nil || out.Version.Multipart() {
		t.Fatalf("a multipart object copied onto itself = %+v, %v", out.Version, err)
	}
	if got, _, err := f.get("b", "film", nil); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the object after being copied onto itself: %d bytes, %v", len(got), err)
	}
	if f.shardCount() != stored-2*sixDrives {
		t.Errorf("%d shards stored after three parts became one object, were %d", f.shardCount(), stored)
	}
	// A source that fails verification half-way leaves no destination.
	damaged := f.put(t, "b", "damaged", randomBytes(t, 3*1024*1024))
	drives := f.shardsOf(t, damaged)
	for i := range sixParity + 1 {
		raw, _ := drives[i].Bytes(shardID(damaged, i))
		raw[len(raw)-100] ^= 1
		drives[i].Corrupt(shardID(damaged, i), raw)
	}
	_, err = f.objects.Copy(ctx, porttest.Root(), app.CopyInput{Source: app.GetInput{Bucket: "b", Key: "damaged"}, Destination: app.PutInput{Bucket: "b", Key: "half"}, Digest: tagged})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("a copy of a source damaged beyond repair = %v", err)
	}
	if _, _, err := f.get("b", "half", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("half a copy was committed: %v", err)
	}
	if _, err := f.objects.CopyPart(ctx, allowing("s3:PutObject"), app.CopyPartInput{
		Source: app.GetInput{Bucket: "b", Key: "open"}, Digest: tagged, Part: app.UploadPartInput{Bucket: "b", Key: "x", UploadID: u.ID, Number: 1},
	}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("CopyPart without the permission to read the source = %v", err)
	}
}

// TestConditionalReads: the conditions of a request are judged against the
// version it reads, and for an object under a customer key only once the
// caller has shown the key.
func TestConditionalReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	data := []byte("conditional")
	v, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: bytes.NewReader(data), Size: int64(len(data)), Verify: func() (string, error) { return "tag-1", nil }})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		c    object.Conditions
		want error
	}{
		{"the tag it has", object.Conditions{IfMatch: []string{"tag-1"}}, nil},
		{"another tag", object.Conditions{IfMatch: []string{"tag-0"}}, domain.ErrPreconditionFailed},
		{"not the tag it has", object.Conditions{IfNoneMatch: []string{"tag-1"}}, domain.ErrNotModified},
		{"modified since before", object.Conditions{IfModifiedSince: v.ModifiedAt.Add(-time.Hour)}, nil},
		{"modified since now", object.Conditions{IfModifiedSince: v.ModifiedAt}, domain.ErrNotModified},
		{"unmodified since before", object.Conditions{IfUnmodifiedSince: v.ModifiedAt.Add(-time.Hour)}, domain.ErrPreconditionFailed},
	}
	for _, tt := range tests {
		in := app.GetInput{Bucket: "b", Key: "k", Conditions: tt.c}
		out, err := f.objects.Get(ctx, porttest.Root(), in)
		if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
			t.Errorf("Get, %s: %v, want %v", tt.name, err, tt.want)
		}
		switch {
		case err == nil:
			got, _ := io.ReadAll(out.Body)
			_ = out.Body.Close()
			if !bytes.Equal(got, data) {
				t.Errorf("Get, %s: read %q", tt.name, got)
			}
		case out.Body != nil || out.Version.ETag != "tag-1":
			t.Errorf("Get, %s: with the verdict came a body, or not the version: %+v", tt.name, out.Version.ETag)
		}
		stat, err := f.objects.Stat(ctx, porttest.Root(), in)
		if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) || stat.ETag != "tag-1" {
			t.Errorf("Stat, %s: %v, version %q", tt.name, err, stat.ETag)
		}
	}
	// Under a customer key, a condition tells nothing to who has no key.
	key := customerKey(1)
	sealed := f.putWith(t, "sealed", data, object.SSE{Mode: object.SSEC, CustomerKey: key})
	matches := object.Conditions{IfNoneMatch: []string{sealed.ETag}}
	for name, tt := range map[string]struct {
		key  []byte
		want error
	}{"no key": {nil, domain.ErrCustomerKeyRequired}, "another key": {customerKey(2), domain.ErrCustomerKeyMismatch}, "the key": {key, domain.ErrNotModified}} {
		in := app.GetInput{Bucket: "b", Key: "sealed", CustomerKey: tt.key, Conditions: matches}
		if _, err := f.objects.Get(ctx, porttest.Root(), in); !errors.Is(err, tt.want) {
			t.Errorf("Get of an SSE-C object with %s and a condition = %v, want %v", name, err, tt.want)
		}
		if _, err := f.objects.Stat(ctx, porttest.Root(), in); !errors.Is(err, tt.want) {
			t.Errorf("Stat of an SSE-C object with %s and a condition = %v, want %v", name, err, tt.want)
		}
	}
}
