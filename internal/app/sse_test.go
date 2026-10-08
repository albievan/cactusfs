package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func customerKey(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, object.KeySize)
}

func (f *fixture) putWith(t *testing.T, key string, data []byte, sse object.SSE) object.Version {
	t.Helper()
	v, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
		Bucket: "b", Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), SSE: sse,
	})
	if err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
	return v
}

func (f *fixture) getWith(key string, customer []byte) ([]byte, error) {
	out, err := f.objects.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: "b", Key: key, CustomerKey: customer})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return readAll(out.Body)
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var out bytes.Buffer
	_, err := out.ReadFrom(r)
	return out.Bytes(), err
}

// TestCustomerKeys: an object written with a key the client holds is read
// with that key and with no other, and the key is stored nowhere.
func TestCustomerKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucket(t, "b")
	data := bytes.Repeat([]byte("customer data "), 9000)
	key := customerKey(1)
	verify := func() (string, error) { return partTag(data), nil }
	version, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{
		Bucket: "b", Key: "secret", Body: bytes.NewReader(data), Size: int64(len(data)), Verify: verify,
		SSE: object.SSE{Mode: object.SSEC, CustomerKey: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := version.Encryption
	if enc.Mode != object.SSEC || len(enc.KeyCheck) != 16 || len(enc.WrappedKey) == 0 {
		t.Fatalf("encryption = %+v", enc)
	}
	// Its entity tag is no digest of its content: it is listed to callers
	// without the key, and stored where the operator reads.
	if version.ETag == partTag(data) || len(version.ETag) != len(partTag(data)) {
		t.Errorf("entity tag of an SSE-C object = %q, the digest of its content is %q", version.ETag, partTag(data))
	}
	plainTagged, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "plain-tagged", Body: bytes.NewReader(data), Size: int64(len(data)), Verify: verify})
	if err != nil || plainTagged.ETag != partTag(data) {
		t.Errorf("entity tag of a plain object = %q, %v", plainTagged.ETag, err)
	}
	// Neither the key nor anything it could be read from is in what is
	// stored: not in the record, not on the drives, not in the logs.
	record := string(enc.WrappedKey) + string(enc.KeyCheck) + string(enc.NoncePrefix) + version.ETag
	logs := f.logs.String()
	for _, form := range []string{string(key), base64.StdEncoding.EncodeToString(key), hex.EncodeToString(key)} {
		if strings.Contains(record, form) || strings.Contains(logs, form) {
			t.Error("the customer key is in the record or in a log")
		}
	}
	for _, d := range f.drives {
		for _, id := range d.IDs() {
			if raw, _ := d.Bytes(id); bytes.Contains(raw, key) || bytes.Contains(raw, []byte("customer data")) {
				t.Error("the customer key or the plaintext is on a drive")
			}
		}
	}

	if got, err := f.getWith("secret", key); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get with the key: %d bytes, %v", len(got), err)
	}
	if got, err := f.getWith("secret", nil); !errors.Is(err, domain.ErrCustomerKeyRequired) || len(got) != 0 {
		t.Errorf("Get without the key = %d bytes, %v", len(got), err)
	}
	if got, err := f.getWith("secret", customerKey(2)); !errors.Is(err, domain.ErrCustomerKeyMismatch) || len(got) != 0 {
		t.Errorf("Get with another key = %d bytes, %v", len(got), err)
	}
	// Its metadata, too, is for the holder of the key.
	stat := func(customer []byte) error {
		_, err := f.objects.Stat(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "secret", CustomerKey: customer})
		return err
	}
	if err := stat(key); err != nil {
		t.Errorf("Stat with the key: %v", err)
	}
	if err := stat(nil); !errors.Is(err, domain.ErrCustomerKeyRequired) {
		t.Errorf("Stat without the key = %v", err)
	}
	if err := stat(customerKey(2)); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("Stat with another key = %v", err)
	}
	// A key for an object that has none is a mistake of the client.
	f.put(t, "b", "plain", []byte("plain"))
	if _, err := f.getWith("plain", key); !errors.Is(err, domain.ErrCustomerKeyUnexpected) {
		t.Errorf("Get of a plain object with a key = %v", err)
	}
	if _, err := f.objects.Stat(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "plain", CustomerKey: key}); !errors.Is(err, domain.ErrCustomerKeyUnexpected) {
		t.Errorf("Stat of a plain object with a key = %v", err)
	}
	if _, err := f.objects.Stat(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "plain"}); err != nil {
		t.Errorf("Stat of a plain object: %v", err)
	}

	// The KMS is still needed: with the client's key and another KMS key
	// nothing is read.
	otherKMS, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	other := f.objectService(t, otherKMS)
	if _, err := other.Get(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "secret", CustomerKey: key}); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("Get with the customer key and another KMS key = %v, want %v", err, domain.ErrKeyUnavailable)
	}

	// A record changed to say the object has no customer key, or another
	// check value, does not open it.
	for name, change := range map[string]func(v *object.Version){
		"said to be the default":   func(v *object.Version) { v.Encryption.Mode = object.SSES3 },
		"said to be SSE-KMS":       func(v *object.Version) { v.Encryption.Mode = object.SSEKMS },
		"another check value":      func(v *object.Version) { v.Encryption.KeyCheck = object.KeyCheck(customerKey(2)) },
		"no check value":           func(v *object.Version) { v.Encryption.KeyCheck = nil },
		"said to be made of parts": func(v *object.Version) { v.Parts = 1; v.Encryption.KeyMode = object.KeyModePartHKDF },
	} {
		f.putWith(t, "tampered", data, object.SSE{Mode: object.SSEC, CustomerKey: key})
		f.meta.Tamper("b", "tampered", change)
		for _, customer := range [][]byte{key, nil, customerKey(2)} {
			if got, err := f.getWith("tampered", customer); err == nil {
				t.Errorf("%s: Get read %d bytes", name, len(got))
			}
		}
	}
	if _, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "bad", Body: bytes.NewReader(nil), SSE: object.SSE{Mode: object.SSEC}}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Put with SSE-C and no key = %v", err)
	}
	if f.shardCountOf("bad") != 0 {
		t.Error("a refused put stored something")
	}
}

func (f *fixture) shardCountOf(key string) int {
	v, err := f.meta.GetObject(context.Background(), "b", key)
	if err != nil {
		return 0
	}
	return len(v.Layout.Shards)
}

// TestSSEKMS: an object written with SSE-KMS says so, names the key, and has
// its encryption context bound to the wrapped key.
func TestSSEKMS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucket(t, "b")
	data := []byte("under the deployment's key, by name")
	context1 := map[string]string{"department": "finance", "year": "2026"}
	version := f.putWith(t, "k", data, object.SSE{Mode: object.SSEKMS, KMSKeyID: "test:1", Context: context1})
	if enc := version.Encryption; enc.Mode != object.SSEKMS || enc.KMSKeyID != "test:1" || enc.ContextMap()["department"] != "finance" {
		t.Fatalf("encryption = %+v", version.Encryption)
	}
	// Reading takes no context from the client: it is in the record.
	if got, err := f.getWith("k", nil); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get: %q, %v", got, err)
	}
	// And it is bound: a record with another context, or none, or another
	// mode, does not unwrap.
	for name, change := range map[string]func(v *object.Version){
		"another context": func(v *object.Version) { v.Encryption.Context = []byte(`{"department":"hr","year":"2026"}`) },
		"no context":      func(v *object.Version) { v.Encryption.Context = nil },
		"the default":     func(v *object.Version) { v.Encryption.Mode, v.Encryption.Context = object.SSES3, nil },
	} {
		f.putWith(t, "tampered", data, object.SSE{Mode: object.SSEKMS, Context: context1})
		f.meta.Tamper("b", "tampered", change)
		if got, err := f.getWith("tampered", nil); !errors.Is(err, domain.ErrKeyUnavailable) || len(got) != 0 {
			t.Errorf("%s: Get = %d bytes, %v; want %v", name, len(got), err, domain.ErrKeyUnavailable)
		}
	}
	// A plain object's record changed to say SSE-KMS does not unwrap either.
	f.put(t, "b", "plain", data)
	f.meta.Tamper("b", "plain", func(v *object.Version) { v.Encryption.Mode = object.SSEKMS })
	if _, err := f.getWith("plain", nil); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("a plain object said to be SSE-KMS = %v", err)
	}
	// Only the deployment's key can be asked for.
	_, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{
		Bucket: "b", Key: "other", Body: bytes.NewReader(data), Size: int64(len(data)), SSE: object.SSE{Mode: object.SSEKMS, KMSKeyID: "arn:aws:kms:other"},
	})
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Put under a KMS key that is not ours = %v", err)
	}
	if _, _, err := f.get("b", "other", nil); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("the refused put made an object: %v", err)
	}
}

// TestMultipartWithCustomerKey: every part of an upload under SSE-C comes
// with the key, completing needs none, and reading needs it again.
func TestMultipartWithCustomerKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	key := customerKey(1)
	u, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{
		Bucket: "b", Key: "film", SSE: object.SSE{Mode: object.SSEC, CustomerKey: key},
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if u.Encryption.Mode != object.SSEC || len(u.Encryption.KeyCheck) == 0 {
		t.Fatalf("upload = %+v", u.Encryption)
	}
	send := func(number int, data []byte, customer []byte) (object.Part, error) {
		return f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
			Bucket: "b", Key: "film", UploadID: u.ID, Number: number, Body: bytes.NewReader(data), Size: int64(len(data)),
			Verify: func() (string, error) { return partTag(data), nil }, CustomerKey: customer,
		})
	}
	if _, err := send(1, []byte("x"), nil); !errors.Is(err, domain.ErrCustomerKeyRequired) {
		t.Errorf("a part without the key = %v", err)
	}
	if _, err := send(1, []byte("x"), customerKey(2)); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("a part with another key = %v", err)
	}
	if f.shardCount() != 0 {
		t.Errorf("refused parts stored %d shards", f.shardCount())
	}
	contents := [][]byte{randomBytes(t, object.MinPartSize), randomBytes(t, 4321)}
	var parts []object.Part
	for i, data := range contents {
		part, err := send(i+1, data, key)
		if err != nil {
			t.Fatalf("part %d: %v", i+1, err)
		}
		parts = append(parts, part)
	}
	// Neither the entity tag nor a checksum of a part is a digest of its
	// plaintext, in the record or in what ListParts shows.
	for i, part := range parts {
		if part.ETag == partTag(contents[i]) || len(part.Checksums) != 0 {
			t.Errorf("part %d is recorded with tag %q and checksums %v", i+1, part.ETag, part.Checksums)
		}
	}
	// A client that lists the checksum it was given completes all the
	// same; a key sent with the completion is checked.
	withSums := listed(parts...)
	for i := range withSums {
		withSums[i].Checksums = map[string]string{"x-amz-checksum-test": "whatever the client kept"}
	}
	if _, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "b", Key: "film", UploadID: u.ID, Parts: withSums, ETag: "e", CustomerKey: customerKey(2),
	}); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("completing with another key = %v", err)
	}
	ref := app.UploadRef{Bucket: "b", Key: "film", UploadID: u.ID}
	if _, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{UploadRef: ref, Max: 10, CustomerKey: customerKey(2)}); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("ListParts with another key = %v", err)
	}
	if out, err := f.objects.ListParts(ctx, porttest.Root(), app.ListPartsInput{UploadRef: ref, Max: 10}); err != nil || len(out.Parts) != 2 {
		t.Errorf("ListParts without a key = %v", err)
	}
	version, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
		Bucket: "b", Key: "film", UploadID: u.ID, Parts: withSums, ETag: "e", CustomerKey: key,
	})
	if err != nil || version.Encryption.Mode != object.SSEC {
		t.Fatalf("CompleteUpload = %+v, %v", version.Encryption, err)
	}
	// A range across the two parts.
	ranged, err := f.objects.Get(ctx, porttest.Root(), app.GetInput{
		Bucket: "b", Key: "film", CustomerKey: key, Range: &object.RangeSpec{First: object.MinPartSize - 3, Last: object.MinPartSize + 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	across, _ := readAll(ranged.Body)
	_ = ranged.Body.Close()
	if !bytes.Equal(across, append(append([]byte{}, contents[0][object.MinPartSize-3:]...), contents[1][:3]...)) {
		t.Errorf("a range across the parts = %d bytes", len(across))
	}
	// The record relabelled does not open it.
	f.meta.Tamper("b", "film", func(v *object.Version) { v.Encryption.Mode = object.SSES3 })
	for _, customer := range [][]byte{key, nil} {
		if got, err := f.getWith("film", customer); err == nil {
			t.Errorf("a relabelled multipart object read %d bytes", len(got))
		}
	}
	f.meta.Tamper("b", "film", func(v *object.Version) { v.Encryption.Mode = object.SSEC })
	want := append(append([]byte{}, contents[0]...), contents[1]...)
	if got, err := f.getWith("film", key); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Get with the key: %d bytes, %v", len(got), err)
	}
	if _, err := f.getWith("film", nil); !errors.Is(err, domain.ErrCustomerKeyRequired) {
		t.Errorf("Get without the key = %v", err)
	}
	if _, err := f.getWith("film", customerKey(2)); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("Get with another key = %v", err)
	}
	if _, err := f.objects.Stat(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "film", CustomerKey: key}); err != nil {
		t.Errorf("Stat with the key: %v", err)
	}
	if _, err := f.objects.Stat(ctx, porttest.Root(), app.GetInput{Bucket: "b", Key: "film", CustomerKey: customerKey(2)}); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
		t.Errorf("Stat with another key = %v", err)
	}
	// Healing needs no key: it works on what is stored.
	if result, err := f.objects.Healer().HealVersion(ctx, "b", "film", version.Ref()); err != nil || result.Shards != 2*sixDrives {
		t.Errorf("heal = %+v, %v", result, err)
	}
	// A plain upload refuses a key that was never part of it.
	plain := f.createUpload(t, "plain")
	if kept := f.uploadPart(t, plain, 2, []byte("p")); len(kept.Checksums) == 0 {
		t.Error("a plain part lost its checksums")
	}
	if _, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "b", Key: "plain", UploadID: plain.ID, Number: 1, Body: bytes.NewReader([]byte("x")), Size: 1, CustomerKey: key,
	}); !errors.Is(err, domain.ErrCustomerKeyUnexpected) {
		t.Errorf("a key with a part of a plain upload = %v", err)
	}
	// SSE-KMS for an upload: the context is bound to the upload's key.
	kms, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{
		Bucket: "b", Key: "kms", SSE: object.SSE{Mode: object.SSEKMS, Context: map[string]string{"a": "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	part, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: "b", Key: "kms", UploadID: kms.ID, Number: 1, Body: bytes.NewReader([]byte("kms part")), Size: 8,
		Verify: func() (string, error) { return "tag", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := f.complete(t, kms, part)
	if done.Encryption.Mode != object.SSEKMS || done.Encryption.ContextMap()["a"] != "1" {
		t.Errorf("completed SSE-KMS version = %+v", done.Encryption)
	}
	if got, err := f.getWith("kms", nil); err != nil || string(got) != "kms part" {
		t.Errorf("Get of the SSE-KMS multipart object: %q, %v", got, err)
	}
	f.meta.Tamper("b", "kms", func(v *object.Version) { v.Encryption.Context = []byte(`{"a":"2"}`) })
	if _, err := f.getWith("kms", nil); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("another context on the multipart object = %v", err)
	}
}
