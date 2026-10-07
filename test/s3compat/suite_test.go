//go:build s3compat

package s3compat_test

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func put(t *testing.T, bucket, key string, data []byte) *s3.PutObjectOutput {
	t.Helper()
	out, err := client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("PutObject(%s): %v", key, err)
	}
	return out
}

func get(t *testing.T, bucket, key string) ([]byte, *s3.GetObjectOutput) {
	t.Helper()
	out, err := client().GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject(%s): %v", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return data, out
}

// The MD5 of "hello", in the two forms S3 uses: hexadecimal in an ETag and
// base64 in a Content-MD5 header; and the Content-MD5 of other content.
const (
	helloETag       = `"5d41402abc4b2a76b9719d911017c592"`
	helloContentMD5 = "XUFAKrxLKna5cZ2REBfFkg=="
	otherContentMD5 = "fXkwN6B2AYZXSwKC8vQ15w=="
)

func TestCreateBucket(t *testing.T) {
	name := newBucket(t)
	if _, err := client().HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Errorf("HeadBucket after CreateBucket: %v", err)
	}
}

func TestCreateBucketWithLocationConstraint(t *testing.T) {
	name := newBucket(t) + "-eu"
	_, err := client().CreateBucket(t.Context(), &s3.CreateBucketInput{
		Bucket:                    aws.String(name),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraintEuWest1},
	})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := client().DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Errorf("DeleteBucket: %v", err)
	}
}

func TestCreateBucketTwice(t *testing.T) {
	name := newBucket(t)
	_, err := client().CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(name)})
	if code := errorCode(err); code != "BucketAlreadyOwnedByYou" {
		t.Errorf("second CreateBucket: code = %q (%v), want BucketAlreadyOwnedByYou", code, err)
	}
}

func TestCreateBucketInvalidName(t *testing.T) {
	_, err := client().CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("Invalid_Name")})
	if code := errorCode(err); code != "InvalidBucketName" {
		t.Errorf("code = %q (%v), want InvalidBucketName", code, err)
	}
}

func TestHeadBucketMissing(t *testing.T) {
	_, err := client().HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("compat-does-not-exist")})
	if code := errorCode(err); code != "NotFound" {
		t.Errorf("code = %q (%v), want NotFound", code, err)
	}
}

func TestListBuckets(t *testing.T) {
	first, second := newBucket(t), newBucket(t)
	out, err := client().ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	var names []string
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
		if b.CreationDate == nil || b.CreationDate.IsZero() {
			t.Errorf("bucket %s has no creation date", aws.ToString(b.Name))
		}
	}
	if !slices.Contains(names, first) || !slices.Contains(names, second) {
		t.Errorf("ListBuckets = %v, want it to include %s and %s", names, first, second)
	}
	if !slices.IsSorted(names) {
		t.Errorf("ListBuckets is not in name order: %v", names)
	}
}

func TestGetBucketLocation(t *testing.T) {
	name := newBucket(t)
	out, err := client().GetBucketLocation(t.Context(), &s3.GetBucketLocationInput{Bucket: aws.String(name)})
	if err != nil {
		t.Fatalf("GetBucketLocation: %v", err)
	}
	if out.LocationConstraint != "" {
		t.Errorf("LocationConstraint = %q, want empty for us-east-1", out.LocationConstraint)
	}
}

func TestDeleteBucket(t *testing.T) {
	name := newBucket(t)
	if _, err := client().DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	_, err := client().HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String(name)})
	if code := errorCode(err); code != "NotFound" {
		t.Errorf("HeadBucket after delete: code = %q (%v), want NotFound", code, err)
	}
	_, err = client().DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String(name)})
	if code := errorCode(err); code != "NoSuchBucket" {
		t.Errorf("second DeleteBucket: code = %q (%v), want NoSuchBucket", code, err)
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	name := newBucket(t)
	put(t, name, "k", []byte("x"))
	_, err := client().DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String(name)})
	if code := errorCode(err); code != "BucketNotEmpty" {
		t.Errorf("code = %q (%v), want BucketNotEmpty", code, err)
	}
}

// TestPutObjectDefaults is the call every SDK user makes: over TLS the SDK
// sends an aws-chunked body with a trailing CRC32.
func TestPutObjectDefaults(t *testing.T) {
	name := newBucket(t)
	data := randomBytes(t, 200_000)

	out := put(t, name, "object", data)
	if len(aws.ToString(out.ETag)) != 34 {
		t.Errorf("ETag = %s, want a quoted 32-character digest", aws.ToString(out.ETag))
	}
	if out.ServerSideEncryption != types.ServerSideEncryptionAes256 {
		t.Errorf("ServerSideEncryption = %q, want AES256", out.ServerSideEncryption)
	}
	if aws.ToString(out.ChecksumCRC32) == "" {
		t.Error("the response does not confirm the CRC32 the SDK sent")
	}
	if got, _ := get(t, name, "object"); !bytes.Equal(got, data) {
		t.Error("GetObject returned different bytes")
	}
}

func TestPutObjectEmpty(t *testing.T) {
	name := newBucket(t)
	put(t, name, "empty", nil)
	data, out := get(t, name, "empty")
	if len(data) != 0 || aws.ToInt64(out.ContentLength) != 0 {
		t.Errorf("empty object came back as %d bytes", len(data))
	}
}

// TestPutObjectLarge crosses the size at which the SDK sends
// "Expect: 100-continue" and many encryption segments and checksum blocks.
func TestPutObjectLarge(t *testing.T) {
	name := newBucket(t)
	data := randomBytes(t, 9<<20+12345)
	out := put(t, name, "large", data)
	got, read := get(t, name, "large")
	if !bytes.Equal(got, data) {
		t.Error("GetObject returned different bytes")
	}
	if aws.ToString(out.ETag) == "" || aws.ToString(out.ETag) != aws.ToString(read.ETag) {
		t.Errorf("ETag on upload %s differs from ETag on download %s", aws.ToString(out.ETag), aws.ToString(read.ETag))
	}
}

// TestPutObjectUnseekableBody sends a stream whose length the SDK cannot
// know: chunked transfer encoding with a trailing checksum.
func TestPutObjectUnseekableBody(t *testing.T) {
	name := newBucket(t)
	data := randomBytes(t, 70_000)
	_, err := client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(name), Key: aws.String("stream"), Body: io.MultiReader(bytes.NewReader(data)),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got, _ := get(t, name, "stream"); !bytes.Equal(got, data) {
		t.Error("GetObject returned different bytes")
	}
}

// TestETagIsTheMD5OfTheObject: clients compare the ETag of a single-part
// upload with the MD5 of what they sent.
func TestETagIsTheMD5OfTheObject(t *testing.T) {
	name := newBucket(t)
	out := put(t, name, "hello", []byte("hello"))
	if got := aws.ToString(out.ETag); got != helloETag {
		t.Errorf("PutObject ETag = %s, want %s", got, helloETag)
	}
	if _, read := get(t, name, "hello"); aws.ToString(read.ETag) != helloETag {
		t.Errorf("GetObject ETag = %s, want %s", aws.ToString(read.ETag), helloETag)
	}
	list, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name)})
	if err != nil || len(list.Contents) != 1 || aws.ToString(list.Contents[0].ETag) != helloETag {
		t.Errorf("ListObjectsV2 ETag: %v", err)
	}
}

func TestPutObjectContentMD5(t *testing.T) {
	name := newBucket(t)

	_, err := client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(name), Key: aws.String("good"), Body: strings.NewReader("hello"),
		ContentMD5: aws.String(helloContentMD5),
	})
	if err != nil {
		t.Fatalf("PutObject with a correct Content-MD5: %v", err)
	}

	_, err = client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(name), Key: aws.String("bad"), Body: strings.NewReader("hello"),
		ContentMD5: aws.String(otherContentMD5),
	})
	if code := errorCode(err); code != "BadDigest" {
		t.Errorf("PutObject with a wrong Content-MD5: code = %q (%v), want BadDigest", code, err)
	}
	_, err = client().HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(name), Key: aws.String("bad")})
	if code := errorCode(err); code != "NotFound" {
		t.Errorf("the rejected object exists: code = %q (%v)", code, err)
	}
}

func TestPutObjectSHA256Checksum(t *testing.T) {
	name := newBucket(t)
	data := randomBytes(t, 50_000)
	out, err := client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(name), Key: aws.String("sha"), Body: bytes.NewReader(data),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if aws.ToString(out.ChecksumSHA256) == "" {
		t.Error("the response does not confirm the SHA-256 checksum")
	}
}

func TestPutObjectMissingBucket(t *testing.T) {
	_, err := client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("compat-does-not-exist"), Key: aws.String("k"), Body: strings.NewReader("x"),
	})
	if code := errorCode(err); code != "NoSuchBucket" {
		t.Errorf("code = %q (%v), want NoSuchBucket", code, err)
	}
}

func TestOverwriteObject(t *testing.T) {
	name := newBucket(t)
	first := put(t, name, "k", []byte("first version"))
	second := put(t, name, "k", []byte("hello"))
	data, out := get(t, name, "k")
	if string(data) != "hello" || aws.ToString(out.ETag) != helloETag || aws.ToString(first.ETag) == aws.ToString(second.ETag) {
		t.Errorf("after overwrite: %q, ETag %s", data, aws.ToString(out.ETag))
	}
}

func TestGetObjectRange(t *testing.T) {
	name := newBucket(t)
	data := randomBytes(t, 200_000)
	put(t, name, "k", data)

	tests := []struct {
		header string
		want   []byte
		rng    string
	}{
		{"bytes=0-99", data[:100], "bytes 0-99/200000"},
		{"bytes=65500-65600", data[65500:65601], "bytes 65500-65600/200000"},
		{"bytes=199990-", data[199990:], "bytes 199990-199999/200000"},
		{"bytes=-25", data[199975:], "bytes 199975-199999/200000"},
	}
	for _, tt := range tests {
		out, err := client().GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String(name), Key: aws.String("k"), Range: aws.String(tt.header),
		})
		if err != nil {
			t.Fatalf("GetObject(%s): %v", tt.header, err)
		}
		got, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if err != nil || !bytes.Equal(got, tt.want) || aws.ToString(out.ContentRange) != tt.rng {
			t.Errorf("Range %s: %d bytes, Content-Range %q, %v; want %d bytes, %q",
				tt.header, len(got), aws.ToString(out.ContentRange), err, len(tt.want), tt.rng)
		}
	}

	_, err := client().GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String(name), Key: aws.String("k"), Range: aws.String("bytes=200000-"),
	})
	if code := errorCode(err); code != "InvalidRange" {
		t.Errorf("unsatisfiable range: code = %q (%v), want InvalidRange", code, err)
	}
}

func TestGetObjectMissing(t *testing.T) {
	name := newBucket(t)
	_, err := client().GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("absent")})
	if code := errorCode(err); code != "NoSuchKey" {
		t.Errorf("code = %q (%v), want NoSuchKey", code, err)
	}
	_, err = client().GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("compat-does-not-exist"), Key: aws.String("k")})
	if code := errorCode(err); code != "NoSuchBucket" {
		t.Errorf("missing bucket: code = %q (%v), want NoSuchBucket", code, err)
	}
}

func TestObjectMetadataRoundTrip(t *testing.T) {
	name := newBucket(t)
	data := []byte("hello")
	_, err := client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(name), Key: aws.String("k"), Body: bytes.NewReader(data),
		ContentType: aws.String("text/plain; charset=utf-8"), CacheControl: aws.String("max-age=3600"),
		ContentDisposition: aws.String(`attachment; filename="a.txt"`), ContentLanguage: aws.String("en"),
		ContentEncoding: aws.String("identity"),
		Metadata:        map[string]string{"owner": "albie", "purpose": "compat"},
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := client().HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if aws.ToString(head.ContentType) != "text/plain; charset=utf-8" || aws.ToString(head.CacheControl) != "max-age=3600" ||
		aws.ToString(head.ContentDisposition) != `attachment; filename="a.txt"` || aws.ToString(head.ContentLanguage) != "en" ||
		aws.ToString(head.ContentEncoding) != "identity" {
		t.Errorf("representation headers = %+v", head)
	}
	if head.Metadata["owner"] != "albie" || head.Metadata["purpose"] != "compat" || len(head.Metadata) != 2 {
		t.Errorf("Metadata = %v", head.Metadata)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(data)) || aws.ToString(head.ETag) != helloETag ||
		head.LastModified == nil || head.ServerSideEncryption != types.ServerSideEncryptionAes256 {
		t.Errorf("HeadObject = length %d, ETag %s, modified %v, SSE %q",
			aws.ToInt64(head.ContentLength), aws.ToString(head.ETag), head.LastModified, head.ServerSideEncryption)
	}
}

func TestHeadObjectMissing(t *testing.T) {
	name := newBucket(t)
	_, err := client().HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(name), Key: aws.String("absent")})
	if code := errorCode(err); code != "NotFound" {
		t.Errorf("code = %q (%v), want NotFound", code, err)
	}
}

func TestDeleteObject(t *testing.T) {
	name := newBucket(t)
	put(t, name, "k", []byte("x"))
	for attempt := 1; attempt <= 2; attempt++ {
		// Deleting an object that is already gone succeeds.
		if _, err := client().DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String("k")}); err != nil {
			t.Fatalf("DeleteObject, attempt %d: %v", attempt, err)
		}
	}
	_, err := client().GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if code := errorCode(err); code != "NoSuchKey" {
		t.Errorf("GetObject after delete: code = %q (%v), want NoSuchKey", code, err)
	}
}

func TestKeysWithSpecialCharacters(t *testing.T) {
	name := newBucket(t)
	keys := []string{
		"plain", "with space", "plus+sign", "percent%20literal", "slash/inside/key", "trailing/",
		"ünï/cødé/鍵", "quote'and\"double", "amp&ersand=equals", "hash#question?", "dots/../up", "a//b", "~tilde*star(paren)",
	}
	for _, key := range keys {
		put(t, name, key, []byte(key))
	}
	for _, key := range keys {
		if data, _ := get(t, name, key); string(data) != key {
			t.Errorf("key %q returned the content of %q", key, data)
		}
	}
	out, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name)})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	var listed []string
	for _, object := range out.Contents {
		listed = append(listed, aws.ToString(object.Key))
	}
	want := slices.Clone(keys)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Errorf("listed keys = %q\nwant %q", listed, want)
	}
}

func listingBucket(t *testing.T) string {
	t.Helper()
	name := newBucket(t)
	for _, key := range []string{"a.txt", "photos/2025/a.jpg", "photos/2025/b.jpg", "photos/2026/c.jpg", "photos/cover.jpg", "videos/x.mp4", "z.txt"} {
		put(t, name, key, []byte(key))
	}
	return name
}

func TestListObjectsV2(t *testing.T) {
	name := listingBucket(t)
	out, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name)})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(out.Contents) != 7 || aws.ToInt32(out.KeyCount) != 7 || aws.ToBool(out.IsTruncated) {
		t.Fatalf("listing has %d objects, KeyCount %d, truncated %v", len(out.Contents), aws.ToInt32(out.KeyCount), aws.ToBool(out.IsTruncated))
	}
	first := out.Contents[0]
	if aws.ToString(first.Key) != "a.txt" || aws.ToInt64(first.Size) != 5 || len(aws.ToString(first.ETag)) != 34 ||
		first.LastModified == nil || first.StorageClass != types.ObjectStorageClassStandard {
		t.Errorf("first entry = %+v", first)
	}

	prefixed, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name), Prefix: aws.String("photos/2025/")})
	if err != nil || len(prefixed.Contents) != 2 {
		t.Errorf("prefix listing has %d objects (%v), want 2", len(prefixed.Contents), err)
	}
	after, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name), StartAfter: aws.String("photos/cover.jpg")})
	if err != nil || len(after.Contents) != 2 || aws.ToString(after.Contents[0].Key) != "videos/x.mp4" {
		t.Errorf("start-after listing = %d objects (%v)", len(after.Contents), err)
	}
	_, err = client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("compat-does-not-exist")})
	if code := errorCode(err); code != "NoSuchBucket" {
		t.Errorf("missing bucket: code = %q (%v), want NoSuchBucket", code, err)
	}
}

func TestListObjectsV2Delimiter(t *testing.T) {
	name := listingBucket(t)
	out, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name), Delimiter: aws.String("/")})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	var keys, prefixes []string
	for _, object := range out.Contents {
		keys = append(keys, aws.ToString(object.Key))
	}
	for _, prefix := range out.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(prefix.Prefix))
	}
	if !slices.Equal(keys, []string{"a.txt", "z.txt"}) || !slices.Equal(prefixes, []string{"photos/", "videos/"}) {
		t.Errorf("top level: keys %q, prefixes %q", keys, prefixes)
	}

	sub, err := client().ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String(name), Prefix: aws.String("photos/"), Delimiter: aws.String("/"),
	})
	if err != nil || len(sub.Contents) != 1 || len(sub.CommonPrefixes) != 2 ||
		aws.ToString(sub.Contents[0].Key) != "photos/cover.jpg" || aws.ToString(sub.CommonPrefixes[0].Prefix) != "photos/2025/" {
		t.Errorf("one level down: %d objects, %d prefixes (%v)", len(sub.Contents), len(sub.CommonPrefixes), err)
	}
}

func TestListObjectsV2Pagination(t *testing.T) {
	name := listingBucket(t)
	for _, delimiter := range []string{"", "/"} {
		var entries []string
		pages := 0
		paginator := s3.NewListObjectsV2Paginator(client(), &s3.ListObjectsV2Input{
			Bucket: aws.String(name), MaxKeys: aws.Int32(2), Delimiter: aws.String(delimiter),
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(t.Context())
			if err != nil {
				t.Fatalf("NextPage: %v", err)
			}
			pages++
			if n := len(page.Contents) + len(page.CommonPrefixes); n > 2 {
				t.Errorf("a page holds %d entries, limit 2", n)
			}
			for _, object := range page.Contents {
				entries = append(entries, aws.ToString(object.Key))
			}
			for _, prefix := range page.CommonPrefixes {
				entries = append(entries, aws.ToString(prefix.Prefix))
			}
		}
		slices.Sort(entries)
		want := []string{"a.txt", "photos/2025/a.jpg", "photos/2025/b.jpg", "photos/2026/c.jpg", "photos/cover.jpg", "videos/x.mp4", "z.txt"}
		if delimiter == "/" {
			want = []string{"a.txt", "photos/", "videos/", "z.txt"}
		}
		if !slices.Equal(entries, want) || pages < 2 {
			t.Errorf("delimiter %q: paged entries = %q in %d pages, want %q", delimiter, entries, pages, want)
		}
	}
}

func TestVirtualHostedStyle(t *testing.T) {
	name := newBucket(t)
	port := target.s3Addr[strings.LastIndex(target.s3Addr, ":"):]
	vhost := newClient(clientOptions{endpoint: "https://" + domain + port, pathStyle: false})

	data := []byte("addressed by host name")
	if _, err := vhost.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(name), Key: aws.String("dir/key"), Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("virtual-hosted PutObject: %v", err)
	}
	out, err := vhost.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("dir/key")})
	if err != nil {
		t.Fatalf("virtual-hosted GetObject: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("virtual-hosted GetObject = %q", got)
	}
	// The same object is reachable by path style.
	if viaPath, _ := get(t, name, "dir/key"); !bytes.Equal(viaPath, data) {
		t.Errorf("path-style GetObject = %q", viaPath)
	}
	list, err := vhost.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(name)})
	if err != nil || len(list.Contents) != 1 {
		t.Errorf("virtual-hosted ListObjectsV2: %d objects (%v)", len(list.Contents), err)
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	name := newBucket(t)
	put(t, name, "k", []byte("private"))
	intruder := newClient(clientOptions{endpoint: target.endpoint, pathStyle: true, secretKey: strings.Repeat("0", 46)})

	_, err := intruder.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if code := errorCode(err); code != "SignatureDoesNotMatch" {
		t.Errorf("GetObject: code = %q (%v), want SignatureDoesNotMatch", code, err)
	}
	_, err = intruder.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(name), Key: aws.String("k"), Body: strings.NewReader("overwritten")})
	if code := errorCode(err); code != "SignatureDoesNotMatch" {
		t.Errorf("PutObject: code = %q (%v), want SignatureDoesNotMatch", code, err)
	}
	if data, _ := get(t, name, "k"); string(data) != "private" {
		t.Errorf("the object was changed: %q", data)
	}
}

func TestAnonymousIsRejected(t *testing.T) {
	name := newBucket(t)
	put(t, name, "k", []byte("private"))
	anonymous := newClient(clientOptions{endpoint: target.endpoint, pathStyle: true, anonymous: true})

	_, err := anonymous.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if code := errorCode(err); code != "AccessDenied" {
		t.Errorf("GetObject: code = %q (%v), want AccessDenied", code, err)
	}
	_, err = anonymous.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if code := errorCode(err); code != "AccessDenied" {
		t.Errorf("ListBuckets: code = %q (%v), want AccessDenied", code, err)
	}
	// An anonymous caller cannot learn whether a bucket exists.
	_, err = anonymous.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("compat-does-not-exist"), Key: aws.String("k")})
	if code := errorCode(err); code != "AccessDenied" {
		t.Errorf("GetObject in a missing bucket: code = %q (%v), want AccessDenied", code, err)
	}
}
