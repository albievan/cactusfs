package s3api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func etagOf(data []byte) string {
	h := newETagHash()
	h.Write(data)
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

func contentMD5(data []byte) string {
	h := newETagHash()
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func crc32Of(data []byte) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(data)))
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBucketAndObjectLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")

	// Buckets.
	r := s.mustStatus(s.do(call{method: http.MethodPut, path: "/photos"}), http.StatusOK)
	if r.header.Get("Location") != "/photos" || r.header.Get("Server") != "CactusFS" {
		t.Errorf("CreateBucket headers = %v", r.header)
	}
	s.expectError(s.do(call{method: http.MethodPut, path: "/photos"}), http.StatusConflict, "BucketAlreadyOwnedByYou")
	s.expectError(s.do(call{method: http.MethodPut, path: "/Not_A_Bucket"}), http.StatusBadRequest, "InvalidBucketName")
	s.mustStatus(s.do(call{method: http.MethodHead, path: "/photos"}), http.StatusOK)
	if r := s.do(call{method: http.MethodHead, path: "/absent"}); r.status != http.StatusNotFound || len(r.body) != 0 {
		t.Errorf("HeadBucket(absent) = %d with %d body bytes; want 404 without a body", r.status, len(r.body))
	}

	var buckets listAllMyBucketsResult
	r = s.mustStatus(s.do(call{method: http.MethodGet, path: "/"}), http.StatusOK)
	if err := xml.Unmarshal(r.body, &buckets); err != nil || len(buckets.Buckets) != 1 || buckets.Buckets[0].Name != "photos" ||
		buckets.Buckets[0].CreationDate != "2026-10-07T12:00:00.000Z" || buckets.Owner.ID != s.keyID {
		t.Errorf("ListBuckets = %+v, %v", buckets, err)
	}
	if !bytes.HasPrefix(r.body, []byte(`<?xml version="1.0" encoding="UTF-8"?>`)) || r.header.Get("Content-Type") != "application/xml" {
		t.Errorf("ListBuckets is not an XML document: %q", r.body)
	}

	r = s.mustStatus(s.do(call{method: http.MethodGet, path: "/photos?location"}), http.StatusOK)
	if !strings.Contains(string(r.body), "<LocationConstraint") {
		t.Errorf("GetBucketLocation = %q", r.body)
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/absent?location"}), http.StatusNotFound, "NoSuchBucket")

	// Objects.
	data := random(t, 200_000)
	r = s.do(call{
		method: http.MethodPut, path: "/photos/2026/cactus%20flower.jpg", body: data,
		headers: map[string]string{
			"Content-Type": "image/jpeg", "Cache-Control": "max-age=60", "Content-Disposition": "inline",
			"Content-Language": "en", "X-Amz-Meta-Owner": "albie", "X-Amz-Meta-Camera": "pinhole",
		},
	})
	s.mustStatus(r, http.StatusOK)
	if r.header.Get("ETag") != etagOf(data) || r.header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Errorf("PutObject headers = %v, want ETag %s", r.header, etagOf(data))
	}
	if obs := s.seen.last(); obs.Operation != "PutObject" || obs.Status != 200 || obs.BytesIn != int64(len(data)) {
		t.Errorf("observation = %+v", obs)
	}

	r = s.mustStatus(s.do(call{method: http.MethodGet, path: "/photos/2026/cactus%20flower.jpg"}), http.StatusOK)
	if !bytes.Equal(r.body, data) {
		t.Error("GetObject returned different bytes")
	}
	for name, want := range map[string]string{
		"Content-Type": "image/jpeg", "Content-Length": "200000", "ETag": etagOf(data), "Accept-Ranges": "bytes",
		"Last-Modified": "Wed, 07 Oct 2026 12:00:00 GMT", "Cache-Control": "max-age=60", "Content-Disposition": "inline",
		"Content-Language": "en", "X-Amz-Meta-Owner": "albie", "X-Amz-Meta-Camera": "pinhole",
		"X-Amz-Server-Side-Encryption": "AES256",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("GetObject header %s = %q, want %q", name, got, want)
		}
	}
	if obs := s.seen.last(); obs.Operation != "GetObject" || obs.BytesOut != int64(len(data)) {
		t.Errorf("observation = %+v", obs)
	}

	r = s.mustStatus(s.do(call{method: http.MethodHead, path: "/photos/2026/cactus%20flower.jpg"}), http.StatusOK)
	if r.header.Get("Content-Length") != "200000" || r.header.Get("ETag") != etagOf(data) || len(r.body) != 0 {
		t.Errorf("HeadObject = %v with %d body bytes", r.header, len(r.body))
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/photos/absent"}), http.StatusNotFound, "NoSuchKey")
	s.expectError(s.do(call{method: http.MethodGet, path: "/absent/key"}), http.StatusNotFound, "NoSuchBucket")
	if r := s.do(call{method: http.MethodHead, path: "/photos/absent"}); r.status != http.StatusNotFound || len(r.body) != 0 {
		t.Errorf("HeadObject(absent) = %d with a %d-byte body", r.status, len(r.body))
	}
	s.expectError(s.do(call{method: http.MethodPut, path: "/absent/key", body: []byte("x")}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodPut, path: "/photos/" + strings.Repeat("k", 1025), body: []byte("x")}),
		http.StatusBadRequest, "KeyTooLongError")

	// An object without a content type gets S3's default.
	s.putObject("/photos/plain", []byte("x"))
	if r := s.do(call{method: http.MethodHead, path: "/photos/plain"}); r.header.Get("Content-Type") != "binary/octet-stream" {
		t.Errorf("default Content-Type = %q", r.header.Get("Content-Type"))
	}

	// Deleting.
	s.expectError(s.do(call{method: http.MethodDelete, path: "/photos"}), http.StatusConflict, "BucketNotEmpty")
	for _, path := range []string{"/photos/2026/cactus%20flower.jpg", "/photos/plain", "/photos/plain"} {
		if r := s.mustStatus(s.do(call{method: http.MethodDelete, path: path}), http.StatusNoContent); len(r.body) != 0 {
			t.Errorf("DeleteObject returned a body: %q", r.body)
		}
	}
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/photos"}), http.StatusNoContent)
	s.expectError(s.do(call{method: http.MethodDelete, path: "/photos"}), http.StatusNotFound, "NoSuchBucket")
	if n := len(s.drive.IDs()); n != 0 {
		t.Errorf("%d shards left after everything was deleted", n)
	}
}

func TestGetObjectRanges(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	data := random(t, 150_000)
	s.putObject("/bkt/k", data)

	tests := []struct {
		header     string
		wantStatus int
		wantRange  string
		want       []byte
	}{
		{"bytes=0-9", 206, "bytes 0-9/150000", data[:10]},
		{"bytes=65530-65545", 206, "bytes 65530-65545/150000", data[65530:65546]},
		{"bytes=149990-", 206, "bytes 149990-149999/150000", data[149990:]},
		{"bytes=-10", 206, "bytes 149990-149999/150000", data[149990:]},
		{"bytes=100-999999", 206, "bytes 100-149999/150000", data[100:]},
		{"bytes=0-", 206, "bytes 0-149999/150000", data},
		// Not a single satisfiable byte range: the whole object, as HTTP allows.
		{"bytes=0-9,20-29", 200, "", data},
		{"items=0-9", 200, "", data},
		{"bytes=9-0", 200, "", data},
		{"bytes=abc", 200, "", data},
	}
	for _, tt := range tests {
		r := s.do(call{method: http.MethodGet, path: "/bkt/k", headers: map[string]string{"Range": tt.header}})
		if r.err != nil || r.status != tt.wantStatus || r.header.Get("Content-Range") != tt.wantRange || !bytes.Equal(r.body, tt.want) {
			t.Errorf("Range %q: status %d, Content-Range %q, %d bytes (%v); want %d, %q, %d bytes",
				tt.header, r.status, r.header.Get("Content-Range"), len(r.body), r.err, tt.wantStatus, tt.wantRange, len(tt.want))
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k", headers: map[string]string{"Range": "bytes=150000-"}}),
		http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
}

func TestListObjectsV2(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	for _, key := range []string{"a.txt", "photos/1.jpg", "photos/2.jpg", "videos/x.mp4", "z%20last.txt"} {
		s.putObject("/bkt/"+key, []byte(key))
	}
	list := func(query string) listBucketResult {
		t.Helper()
		r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?list-type=2" + query}), http.StatusOK)
		var result listBucketResult
		if err := xml.Unmarshal(r.body, &result); err != nil {
			t.Fatalf("ListObjectsV2 response: %v\n%s", err, r.body)
		}
		return result
	}
	keys := func(r listBucketResult) []string {
		var out []string
		for _, c := range r.Contents {
			out = append(out, c.Key)
		}
		return out
	}

	all := list("")
	if !slices.Equal(keys(all), []string{"a.txt", "photos/1.jpg", "photos/2.jpg", "videos/x.mp4", "z last.txt"}) ||
		all.KeyCount != 5 || all.IsTruncated || all.Name != "bkt" || all.MaxKeys != 1000 {
		t.Errorf("full listing = %+v", all)
	}
	first := all.Contents[0]
	if first.ETag != etagOf([]byte("a.txt")) || first.Size != 5 || first.StorageClass != "STANDARD" ||
		first.LastModified != "2026-10-07T12:00:00.000Z" || first.Owner != nil {
		t.Errorf("first entry = %+v", first)
	}

	grouped := list("&delimiter=%2F")
	if !slices.Equal(keys(grouped), []string{"a.txt", "z last.txt"}) || len(grouped.CommonPrefixes) != 2 ||
		grouped.CommonPrefixes[0].Prefix != "photos/" || grouped.Delimiter != "/" || grouped.KeyCount != 4 {
		t.Errorf("grouped listing = %+v", grouped)
	}
	if got := list("&prefix=photos%2F&delimiter=%2F"); !slices.Equal(keys(got), []string{"photos/1.jpg", "photos/2.jpg"}) || got.Prefix != "photos/" {
		t.Errorf("prefix listing = %+v", got)
	}
	if got := list("&start-after=photos%2F2.jpg"); !slices.Equal(keys(got), []string{"videos/x.mp4", "z last.txt"}) || got.StartAfter != "photos/2.jpg" {
		t.Errorf("start-after listing = %+v", got)
	}
	if got := list("&fetch-owner=true"); got.Contents[0].Owner == nil || got.Contents[0].Owner.ID != s.keyID {
		t.Errorf("fetch-owner listing has no owner: %+v", got.Contents[0])
	}
	if got := list("&encoding-type=url"); got.Contents[4].Key != "z+last.txt" || got.EncodingType != "url" || got.Contents[1].Key != "photos%2F1.jpg" {
		t.Errorf("url-encoded listing = %+v", keys(got))
	}
	if got := list("&max-keys=0"); len(got.Contents) != 0 || got.IsTruncated {
		t.Errorf("max-keys=0 listing = %+v", got)
	}

	// Paging with the continuation token visits everything once.
	var paged []string
	query := "&max-keys=2&delimiter=%2F"
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		page := list(query)
		paged = append(paged, keys(page)...)
		for _, p := range page.CommonPrefixes {
			paged = append(paged, p.Prefix)
		}
		if !page.IsTruncated {
			if page.NextContinuationToken != "" {
				t.Error("a final page carries a continuation token")
			}
			break
		}
		query = "&max-keys=2&delimiter=%2F&continuation-token=" + page.NextContinuationToken
	}
	slices.Sort(paged)
	if !slices.Equal(paged, []string{"a.txt", "photos/", "videos/", "z last.txt"}) {
		t.Errorf("paged listing = %q", paged)
	}

	for _, bad := range []string{"&max-keys=-1", "&max-keys=many", "&continuation-token=%21%21", "&encoding-type=base64"} {
		s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?list-type=2" + bad}), http.StatusBadRequest, "InvalidArgument")
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/absent?list-type=2"}), http.StatusNotFound, "NoSuchBucket")
}

// TestEveryRequestIsAuthenticated: nothing is served, revealed or read
// without a valid signature.
func TestEveryRequestIsAuthenticated(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.putObject("/bkt/k", []byte("secret contents"))

	other := random(t, 40)
	tests := []struct {
		name   string
		call   call
		status int
		code   string
	}{
		{"no signature", call{unsigned: true}, 403, "AccessDenied"},
		{"wrong secret", call{key: other}, 403, "SignatureDoesNotMatch"},
		{"unknown access key", call{keyID: "NOSUCHACCESSKEY00000"}, 403, "InvalidAccessKeyId"},
		{"signed long ago", call{signedAt: serverNow.Add(-time.Hour)}, 403, "RequestTimeTooSkewed"},
		{"signed in the future", call{signedAt: serverNow.Add(time.Hour)}, 403, "RequestTimeTooSkewed"},
		{"tampered after signing", call{mutate: func(r *http.Request) { r.Header.Set("X-Amz-Meta-Owner", "mallory") }}, 400, "AuthorizationHeaderMalformed"},
		{"signature version 2", call{unsigned: true, headers: map[string]string{"Authorization": "AWS key:c2ln"}}, 400, "InvalidRequest"},
		{"basic credentials", call{unsigned: true, headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}}, 400, "InvalidRequest"},
		{"garbage authorization", call{unsigned: true, headers: map[string]string{"Authorization": "AWS4-HMAC-SHA256 nonsense"}}, 400, "AuthorizationHeaderMalformed"},
	}
	targets := []struct{ method, path string }{
		{http.MethodGet, "/bkt/k"},
		{http.MethodGet, "/bkt?list-type=2"},
		{http.MethodGet, "/"},
		{http.MethodPut, "/bkt/new"},
		{http.MethodDelete, "/bkt/k"},
		{http.MethodDelete, "/bkt"},
		{http.MethodPut, "/another"},
		// Whether a bucket or key exists must not be learnable either.
		{http.MethodGet, "/absent/k"},
		{http.MethodGet, "/absent?list-type=2"},
	}
	for _, tt := range tests {
		for _, target := range targets {
			c := tt.call
			c.method, c.path = target.method, target.path
			r := s.do(c)
			if r.err != nil {
				t.Fatalf("%s %s %s: %v", tt.name, target.method, target.path, r.err)
			}
			if code := s.errorCode(r); r.status != tt.status || code != tt.code {
				t.Errorf("%s: %s %s = %d %s, want %d %s", tt.name, target.method, target.path, r.status, code, tt.status, tt.code)
			}
			if bytes.Contains(r.body, []byte("secret contents")) {
				t.Errorf("%s: %s %s leaked object contents", tt.name, target.method, target.path)
			}
		}
	}

	// HEAD carries no error document, only the status.
	if r := s.do(call{method: http.MethodHead, path: "/bkt/k", unsigned: true}); r.status != http.StatusForbidden || len(r.body) != 0 {
		t.Errorf("unsigned HEAD = %d with a %d-byte body", r.status, len(r.body))
	}
	// Nothing was changed by any of it.
	if r := s.do(call{method: http.MethodGet, path: "/bkt/k"}); string(r.body) != "secret contents" {
		t.Errorf("the object changed: %q", r.body)
	}
	if r := s.do(call{method: http.MethodHead, path: "/bkt/new"}); r.status != http.StatusNotFound {
		t.Errorf("an unauthenticated PUT created an object: %d", r.status)
	}

	logs := s.logs.String()
	if !strings.Contains(logs, `"msg":"authentication failed"`) || !strings.Contains(logs, `"reason":"SignatureDoesNotMatch"`) {
		t.Error("authentication failures are not logged")
	}
	if strings.Contains(logs, string(s.key)) || strings.Contains(logs, string(other)) || strings.Contains(logs, "secret contents") {
		t.Error("the log contains a secret key or object contents")
	}
	if strings.Contains(logs, "Signature=") {
		t.Error("the log contains an Authorization header")
	}
}

func TestBodyOfAnUnauthenticatedUploadIsNotRead(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")

	r := s.do(call{method: http.MethodPut, path: "/bkt/k", body: random(t, 100_000), unsigned: true})
	s.expectError(r, http.StatusForbidden, "AccessDenied")
	if obs := s.seen.last(); obs.BytesIn != 0 || obs.Status != http.StatusForbidden {
		t.Errorf("observation = %+v; want no body bytes read", obs)
	}
}

func TestPresignedRequestsAreNotImplementedYet(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	for _, query := range []string{"?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=00", "?AWSAccessKeyId=x&Signature=y&Expires=1"} {
		s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k" + query, unsigned: true}), http.StatusNotImplemented, "NotImplemented")
	}
}

func TestUnsupportedOperationsAreRefusedNotMisrouted(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.putObject("/bkt/k", []byte("x"))

	notImplemented := []call{
		{method: http.MethodGet, path: "/bkt?acl"},
		{method: http.MethodPut, path: "/bkt?versioning"},
		{method: http.MethodGet, path: "/bkt?uploads"},
		{method: http.MethodPost, path: "/bkt?delete"},
		{method: http.MethodGet, path: "/bkt"}, // ListObjects version 1
		{method: http.MethodGet, path: "/bkt/k?tagging"},
		{method: http.MethodGet, path: "/bkt/k?versionId=1"},
		{method: http.MethodPut, path: "/bkt/k?partNumber=1&uploadId=u"},
		{method: http.MethodDelete, path: "/bkt/k?uploadId=u"},
		{method: http.MethodPut, path: "/bkt/copy", headers: map[string]string{"X-Amz-Copy-Source": "/bkt/k"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("x"), payloadHash: "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("x"), payloadHash: "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"},
	}
	for _, c := range notImplemented {
		s.expectError(s.do(c), http.StatusNotImplemented, "NotImplemented")
	}
	for _, c := range []call{
		{method: http.MethodPost, path: "/bkt/k"},
		{method: http.MethodPost, path: "/bkt"},
		{method: http.MethodPut, path: "/"},
		{method: http.MethodPatch, path: "/bkt/k"},
	} {
		s.expectError(s.do(c), http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
	// The object and the bucket are as they were: "?acl" was not treated as
	// a plain GET, nor the multipart DELETE as a delete of the object.
	if r := s.do(call{method: http.MethodGet, path: "/bkt/k"}); string(r.body) != "x" {
		t.Errorf("the object changed: %d %q", r.status, r.body)
	}
	// The SDK's x-id query parameter is not a subresource.
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k?x-id=GetObject"}), http.StatusOK)
}

func TestPayloadVerification(t *testing.T) {
	t.Parallel()
	data := []byte("the quick brown fox jumps over the lazy dog")
	wrongSHA := strings.Repeat("ab", 32)

	tests := []struct {
		name   string
		call   call
		status int
		code   string // empty: success
	}{
		{name: "literal SHA-256", call: call{body: data}, status: 200},
		{name: "wrong SHA-256", call: call{body: data, payloadHash: wrongSHA}, status: 400, code: "XAmzContentSHA256Mismatch"},
		{name: "uppercase SHA-256", call: call{body: data, payloadHash: strings.ToUpper(wrongSHA)}, status: 400, code: "InvalidArgument"},
		{name: "not a digest", call: call{body: data, payloadHash: "something"}, status: 400, code: "InvalidArgument"},
		{name: "unsigned payload", call: call{body: data, payloadHash: payloadUnsigned}, status: 200},
		{name: "Content-MD5", call: call{body: data, headers: map[string]string{"Content-MD5": contentMD5(data)}}, status: 200},
		{name: "wrong Content-MD5", call: call{body: data, headers: map[string]string{"Content-MD5": contentMD5([]byte("other"))}}, status: 400, code: "BadDigest"},
		{name: "malformed Content-MD5", call: call{body: data, headers: map[string]string{"Content-MD5": "not base64!"}}, status: 400, code: "InvalidDigest"},
		{name: "short Content-MD5", call: call{body: data, headers: map[string]string{"Content-MD5": "YWJj"}}, status: 400, code: "InvalidDigest"},
		{name: "CRC32 header", call: call{body: data, headers: map[string]string{"X-Amz-Checksum-Crc32": crc32Of(data)}}, status: 200},
		{name: "wrong CRC32 header", call: call{body: data, headers: map[string]string{"X-Amz-Checksum-Crc32": crc32Of([]byte("other"))}}, status: 400, code: "BadDigest"},
		{name: "malformed CRC32 header", call: call{body: data, headers: map[string]string{"X-Amz-Checksum-Crc32": "AAAA"}}, status: 400, code: "InvalidArgument"},
		{name: "unsupported checksum algorithm", call: call{body: data, headers: map[string]string{"X-Amz-Checksum-Sha1": "2jmj7l5rSw0yVb/vlWAYkK/YBwk="}}, status: 400, code: "InvalidRequest"},
		{name: "checksum mode header is not a checksum", call: call{body: data, headers: map[string]string{"X-Amz-Checksum-Mode": "ENABLED", "X-Amz-Sdk-Checksum-Algorithm": "CRC32"}}, status: 200},
		{name: "empty object", call: call{body: nil}, status: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, "")
			s.createBucket("bkt")
			c := tt.call
			c.method, c.path = http.MethodPut, "/bkt/k"

			r := s.do(c)
			if tt.code != "" {
				s.expectError(r, tt.status, tt.code)
				if head := s.do(call{method: http.MethodHead, path: "/bkt/k"}); head.status != http.StatusNotFound {
					t.Errorf("a rejected upload was stored: HEAD = %d", head.status)
				}
				if n := len(s.drive.IDs()); n != 0 {
					t.Errorf("a rejected upload left %d shards", n)
				}
				return
			}
			s.mustStatus(r, tt.status)
			if r.header.Get("ETag") != etagOf(c.body) {
				t.Errorf("ETag = %s, want %s", r.header.Get("ETag"), etagOf(c.body))
			}
			if want := c.headers["X-Amz-Checksum-Crc32"]; want != "" && r.header.Get("X-Amz-Checksum-Crc32") != want {
				t.Errorf("response checksum = %q, want %q echoed", r.header.Get("X-Amz-Checksum-Crc32"), want)
			}
			if got := s.do(call{method: http.MethodGet, path: "/bkt/k"}); !bytes.Equal(got.body, c.body) {
				t.Error("stored object differs from the upload")
			}
		})
	}
}

// awsChunked frames data as an AWS SDK does for
// STREAMING-UNSIGNED-PAYLOAD-TRAILER.
func awsChunked(data []byte, chunkSize int, trailers string) []byte {
	var b bytes.Buffer
	for len(data) > 0 {
		n := min(chunkSize, len(data))
		fmt.Fprintf(&b, "%x\r\n", n)
		b.Write(data[:n])
		b.WriteString("\r\n")
		data = data[n:]
	}
	b.WriteString("0\r\n" + trailers + "\r\n")
	return b.Bytes()
}

func chunkedCall(body []byte, decodedLength int, trailerName string) call {
	return call{
		method: http.MethodPut, path: "/bkt/k", body: body, payloadHash: payloadStreamingTrailer,
		headers: map[string]string{
			"Content-Encoding":             "aws-chunked",
			"X-Amz-Decoded-Content-Length": fmt.Sprint(decodedLength),
			"X-Amz-Trailer":                trailerName,
		},
	}
}

func TestAWSChunkedUploads(t *testing.T) {
	t.Parallel()
	data := random(t, 150_000)
	good := "x-amz-checksum-crc32:" + crc32Of(data) + "\r\n"

	tests := []struct {
		name   string
		call   call
		status int
		code   string
	}{
		{name: "one chunk", call: chunkedCall(awsChunked(data, len(data), good), len(data), "x-amz-checksum-crc32"), status: 200},
		{name: "many chunks", call: chunkedCall(awsChunked(data, 8192, good), len(data), "x-amz-checksum-crc32"), status: 200},
		{name: "wrong trailer checksum", call: chunkedCall(awsChunked(data, 8192, "x-amz-checksum-crc32:"+crc32Of([]byte("x"))+"\r\n"), len(data), "x-amz-checksum-crc32"), status: 400, code: "BadDigest"},
		{name: "missing trailer", call: chunkedCall(awsChunked(data, 8192, ""), len(data), "x-amz-checksum-crc32"), status: 400, code: "BadDigest"},
		{name: "malformed trailer value", call: chunkedCall(awsChunked(data, 8192, "x-amz-checksum-crc32:!!!\r\n"), len(data), "x-amz-checksum-crc32"), status: 400, code: "BadDigest"},
		{name: "data changed in transit", call: chunkedCall(awsChunked(append([]byte("X"), data[1:]...), 8192, good), len(data), "x-amz-checksum-crc32"), status: 400, code: "BadDigest"},
		{name: "shorter than declared", call: chunkedCall(awsChunked(data[:1000], 8192, good), len(data), "x-amz-checksum-crc32"), status: 400, code: "IncompleteBody"},
		{name: "longer than declared", call: chunkedCall(awsChunked(data, 8192, good), 1000, "x-amz-checksum-crc32"), status: 400, code: "InvalidArgument"},
		{name: "cut inside a chunk", call: chunkedCall(awsChunked(data, 8192, good)[:5000], len(data), "x-amz-checksum-crc32"), status: 400, code: "IncompleteBody"},
		{name: "no final chunk", call: chunkedCall(bytes.TrimSuffix(awsChunked(data, len(data), ""), []byte("0\r\n\r\n")), len(data), "x-amz-checksum-crc32"), status: 400, code: "IncompleteBody"},
		{name: "not chunked at all", call: chunkedCall(data, len(data), "x-amz-checksum-crc32"), status: 400, code: "IncompleteBody"},
		// A client streaming a body of unknown length sends no decoded
		// length; the trailing checksum still has to match.
		{name: "no decoded length", call: func() call {
			c := chunkedCall(awsChunked(data, 8192, good), len(data), "x-amz-checksum-crc32")
			delete(c.headers, "X-Amz-Decoded-Content-Length")
			return c
		}(), status: 200},
		{name: "no decoded length and a wrong checksum", call: func() call {
			c := chunkedCall(awsChunked(data, 8192, "x-amz-checksum-crc32:"+crc32Of([]byte("x"))+"\r\n"), len(data), "x-amz-checksum-crc32")
			delete(c.headers, "X-Amz-Decoded-Content-Length")
			return c
		}(), status: 400, code: "BadDigest"},
		{name: "malformed decoded length", call: func() call {
			c := chunkedCall(awsChunked(data, 8192, good), len(data), "x-amz-checksum-crc32")
			c.headers["X-Amz-Decoded-Content-Length"] = "lots"
			return c
		}(), status: 400, code: "InvalidArgument"},
		{name: "no trailer declared", call: chunkedCall(awsChunked(data, 8192, good), len(data), ""), status: 400, code: "InvalidArgument"},
		{name: "two trailers declared", call: chunkedCall(awsChunked(data, 8192, good), len(data), "x-amz-checksum-crc32,x-amz-checksum-sha256"), status: 400, code: "InvalidArgument"},
		{name: "unsupported trailer algorithm", call: chunkedCall(awsChunked(data, 8192, good), len(data), "x-amz-checksum-sha1"), status: 400, code: "InvalidRequest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, "")
			s.createBucket("bkt")

			r := s.do(tt.call)
			if tt.code != "" {
				s.expectError(r, tt.status, tt.code)
				if head := s.do(call{method: http.MethodHead, path: "/bkt/k"}); head.status != http.StatusNotFound {
					t.Errorf("a rejected upload was stored: HEAD = %d", head.status)
				}
				return
			}
			s.mustStatus(r, tt.status)
			if r.header.Get("ETag") != etagOf(data) || r.header.Get("X-Amz-Checksum-Crc32") != crc32Of(data) {
				t.Errorf("response headers = %v", r.header)
			}
			got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusOK)
			if !bytes.Equal(got.body, data) {
				t.Error("the stored object is not the decoded payload")
			}
			// aws-chunked is framing, not an encoding of the object.
			if enc := got.header.Get("Content-Encoding"); enc != "" {
				t.Errorf("stored Content-Encoding = %q, want none", enc)
			}
		})
	}

	t.Run("another content encoding is kept", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t, "")
		s.createBucket("bkt")
		c := chunkedCall(awsChunked(data, 8192, good), len(data), "x-amz-checksum-crc32")
		c.headers["Content-Encoding"] = "aws-chunked, gzip"
		s.mustStatus(s.do(c), http.StatusOK)
		if enc := s.do(call{method: http.MethodHead, path: "/bkt/k"}).header.Get("Content-Encoding"); enc != "gzip" {
			t.Errorf("stored Content-Encoding = %q, want gzip", enc)
		}
	})
}

func TestChecksumAlgorithms(t *testing.T) {
	t.Parallel()

	// Check values of the algorithms for the ASCII string "123456789".
	want := map[string]string{
		"crc32":     "cbf43926",
		"crc32c":    "e3069283",
		"crc64nvme": "ae8b14860a799888",
		"sha256":    "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
	}
	for algorithm, check := range want {
		h, err := newChecksum(algorithm)
		if err != nil {
			t.Fatalf("%s: %v", algorithm, err)
		}
		h.Write([]byte("123456789"))
		if got := hex.EncodeToString(h.Sum(nil)); got != check {
			t.Errorf("%s(\"123456789\") = %s, want %s", algorithm, got, check)
		}
	}
	for _, algorithm := range []string{"sha1", "md5", "", "crc16"} {
		if _, err := newChecksum(algorithm); !errors.Is(err, errUnsupportedChecksum) {
			t.Errorf("newChecksum(%q) error = %v, want %v", algorithm, err, errUnsupportedChecksum)
		}
	}
}

func TestVirtualHostedStyle(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "s3.cactus.test")

	s.mustStatus(s.do(call{method: http.MethodPut, path: "/", host: "photos.s3.cactus.test"}), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/dir/key", host: "photos.s3.cactus.test:9000", body: []byte("vhost")}), http.StatusOK)

	// The same object by path style, through the service host.
	if r := s.do(call{method: http.MethodGet, path: "/photos/dir/key", host: "s3.cactus.test"}); string(r.body) != "vhost" {
		t.Errorf("path-style GET = %d %q", r.status, r.body)
	}
	if r := s.do(call{method: http.MethodGet, path: "/dir/key", host: "PHOTOS.S3.Cactus.Test"}); string(r.body) != "vhost" {
		t.Errorf("virtual-hosted GET with another case = %d %q", r.status, r.body)
	}
	r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/?list-type=2", host: "photos.s3.cactus.test"}), http.StatusOK)
	if !strings.Contains(string(r.body), "<Key>dir/key</Key>") {
		t.Errorf("virtual-hosted listing = %s", r.body)
	}
	// A host outside the domain is path style.
	if r := s.do(call{method: http.MethodGet, path: "/photos/dir/key", host: "photos.example.com"}); string(r.body) != "vhost" {
		t.Errorf("path-style GET on another host = %d %q", r.status, r.body)
	}
}

func TestInternalFailuresRevealNothing(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	data := random(t, 300_000)
	s.putObject("/bkt/k", data)

	version, err := s.objects.Head(t.Context(), s.principal(), "bkt", "k")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	id := object.ShardID{DataID: version.DataID}
	stored, _ := s.drive.Bytes(id)

	// Damage in the first segment: reported before the response begins.
	damaged := bytes.Clone(stored)
	damaged[100] ^= 1
	s.drive.Corrupt(id, damaged)
	r := s.do(call{method: http.MethodGet, path: "/bkt/k"})
	s.expectError(r, http.StatusInternalServerError, "InternalError")
	for _, leak := range []string{"integrity", "segment", "shard", "drive-a", version.DataID.String()} {
		if strings.Contains(strings.ToLower(string(r.body)), leak) {
			t.Errorf("the error response mentions %q: %s", leak, r.body)
		}
	}
	if !strings.Contains(s.logs.String(), domain.ErrIntegrity.Error()) {
		t.Error("the cause was not logged")
	}

	// Damage further in: the response has begun, so it is cut short. The
	// client must see a failed transfer, never a complete-looking object.
	damaged = bytes.Clone(stored)
	damaged[len(damaged)-100] ^= 1
	s.drive.Corrupt(id, damaged)
	r = s.do(call{method: http.MethodGet, path: "/bkt/k"})
	if r.err == nil && len(r.body) == len(data) {
		t.Fatal("a damaged object was served in full without an error")
	}
	if r.err == nil {
		t.Errorf("the transfer of a damaged object did not fail: %d bytes of %d", len(r.body), len(data))
	}
	if !bytes.HasPrefix(data, r.body) {
		t.Error("bytes that are not part of the object were sent")
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrBucketNotFound, 404, "NoSuchBucket"},
		{domain.ErrObjectNotFound, 404, "NoSuchKey"},
		{domain.ErrBucketAlreadyExists, 409, "BucketAlreadyOwnedByYou"},
		{domain.ErrBucketNotEmpty, 409, "BucketNotEmpty"},
		{domain.ErrInvalidBucketName, 400, "InvalidBucketName"},
		{domain.ErrInvalidObjectKey, 400, "KeyTooLongError"},
		{domain.ErrInvalidRange, 416, "InvalidRange"},
		{domain.ErrInvalidArgument, 400, "InvalidArgument"},
		{domain.ErrAccessDenied, 403, "AccessDenied"},
		{domain.ErrCredentialNotFound, 403, "InvalidAccessKeyId"},
		{domain.ErrBadDigest, 400, "BadDigest"},
		{domain.ErrIncompleteBody, 400, "IncompleteBody"},
		{domain.ErrEntityTooLarge, 400, "EntityTooLarge"},
		{domain.ErrNotLeader, 503, "ServiceUnavailable"},
		{domain.ErrShuttingDown, 503, "ServiceUnavailable"},
		// Nothing about these may reach a client.
		{domain.ErrIntegrity, 500, "InternalError"},
		{domain.ErrShardNotFound, 500, "InternalError"},
		{domain.ErrKeyUnavailable, 500, "InternalError"},
		{domain.ErrUnsupportedFormat, 500, "InternalError"},
		{domain.ErrNotBootstrapped, 500, "InternalError"},
		{errors.New("anything else"), 500, "InternalError"},
		{io.ErrUnexpectedEOF, 500, "InternalError"},
		{errMalformedXML, 400, "MalformedXML"},
	}
	for _, tt := range tests {
		// As a use case returns it: wrapped with context.
		got := toAPIError(fmt.Errorf("object.Get: %w", tt.err))
		if got.status != tt.status || got.code != tt.code {
			t.Errorf("toAPIError(%v) = %d %s, want %d %s", tt.err, got.status, got.code, tt.status, tt.code)
		}
	}
	if msg := errInternal().Error(); !strings.HasPrefix(msg, "InternalError: ") {
		t.Errorf("apiError.Error() = %q", msg)
	}
}

func TestCreateBucketBody(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")

	body := `<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>`
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/with-config", body: []byte(body)}), http.StatusOK)
	s.expectError(s.do(call{method: http.MethodPut, path: "/bad-xml", body: []byte("<CreateBucketConfiguration>")}), http.StatusBadRequest, "MalformedXML")
	s.expectError(s.do(call{method: http.MethodPut, path: "/too-big", body: bytes.Repeat([]byte(" "), maxRequestXML+1)}), http.StatusBadRequest, "MalformedXML")
	for _, name := range []string{"bad-xml", "too-big"} {
		if r := s.do(call{method: http.MethodHead, path: "/" + name}); r.status != http.StatusNotFound {
			t.Errorf("bucket %s was created despite its malformed body", name)
		}
	}
	if _, err := parseCreateBucketConfiguration(iotest.ErrReader(errors.New("boom"))); !errors.Is(err, domain.ErrIncompleteBody) {
		t.Errorf("unreadable body: error = %v", err)
	}
}

func TestParseRange(t *testing.T) {
	t.Parallel()

	tests := map[string]*object.RangeSpec{
		"bytes=0-9":                   {First: 0, Last: 9},
		"bytes=5-":                    {First: 5, Last: -1},
		"bytes=-7":                    {Suffix: 7},
		"bytes= 1-2":                  {First: 1, Last: 2},
		"":                            nil,
		"bytes=":                      nil,
		"bytes=-":                     nil,
		"bytes=-0":                    nil,
		"bytes=--5":                   nil,
		"bytes=a-b":                   nil,
		"bytes=5-1":                   nil,
		"bytes=1-2,4":                 nil,
		"lines=1-2":                   nil,
		"bytes=99999999999999999999-": nil,
	}
	for header, want := range tests {
		got := parseRange(header)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("parseRange(%q) = %+v, want %+v", header, got, want)
		}
	}
}

func FuzzParseRange(f *testing.F) {
	for _, seed := range []string{"bytes=0-9", "bytes=-5", "bytes=5-", "bytes=9-0", "x", "bytes=1-2,3-4"} {
		f.Add(seed, int64(100))
	}
	f.Fuzz(func(t *testing.T, header string, size int64) {
		spec := parseRange(header)
		if spec == nil || size < 0 {
			return
		}
		r, err := spec.Resolve(size)
		if err != nil {
			return
		}
		if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > size {
			t.Fatalf("Range %q on %d bytes resolved to %+v", header, size, r)
		}
	})
}

// FuzzXML: no request body makes the XML parser panic or read without bound.
func FuzzXML(f *testing.F) {
	f.Add([]byte(`<CreateBucketConfiguration><LocationConstraint>eu</LocationConstraint></CreateBucketConfiguration>`))
	f.Add([]byte(`<a><b></a>`))
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY e "eeeeeeeeee">]><x>&e;&e;&e;</x>`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		cfg, err := parseCreateBucketConfiguration(bytes.NewReader(body))
		if err != nil {
			var api apiError
			if !errors.As(err, &api) || api.status != http.StatusBadRequest {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if len(cfg.LocationConstraint) > len(body) {
			t.Fatalf("parsed value is longer than the document: entity expansion?")
		}
	})
}

// FuzzChunkedReader: framing produced by awsChunked always decodes to the
// original data, and arbitrary bytes never panic or yield more data than they
// contain.
func FuzzChunkedReader(f *testing.F) {
	f.Add([]byte("hello world"), uint8(4), []byte(nil))
	f.Add([]byte(nil), uint8(1), []byte("5\r\nhello\r\n0\r\nx-amz-checksum-crc32:AAAA\r\n\r\n"))
	f.Add([]byte("x"), uint8(0), []byte("ffffffffffffffff\r\n"))
	f.Add([]byte("x"), uint8(0), []byte("1;ext=1\r\nx\r\n0\r\n\r\n"))

	f.Fuzz(func(t *testing.T, data []byte, chunk uint8, raw []byte) {
		size := int(chunk)%64 + 1
		framed := awsChunked(data, size, "x-amz-checksum-crc32:"+crc32Of(data)+"\r\n")
		reader := newChunkedReader(iotest.OneByteReader(bytes.NewReader(framed)))
		decoded, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(decoded, data) || !reader.done || reader.trailers["x-amz-checksum-crc32"] != crc32Of(data) {
			t.Fatalf("round trip failed: %v", err)
		}

		arbitrary := newChunkedReader(bytes.NewReader(raw))
		out, err := io.ReadAll(arbitrary)
		if len(out) > len(raw) {
			t.Fatalf("decoded %d bytes from %d", len(out), len(raw))
		}
		if err != nil && !errors.Is(err, errChunkFraming) && !errors.Is(err, domain.ErrIncompleteBody) {
			t.Fatalf("untyped error: %v", err)
		}
		if err == nil && !arbitrary.done {
			t.Fatal("EOF reported before the final chunk")
		}
	})
}

func TestChunkedReaderFraming(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body string
		want error
	}{
		"bad size":              {"zz\r\nhello\r\n0\r\n\r\n", errChunkFraming},
		"negative size":         {"-5\r\nhello\r\n0\r\n\r\n", errChunkFraming},
		"huge size":             {"ffffffffffff\r\n", errChunkFraming},
		"bare LF":               {"5\nhello\n0\n\n", errChunkFraming},
		"no CRLF after data":    {"5\r\nhelloXX0\r\n\r\n", errChunkFraming},
		"trailer without colon": {"0\r\nnot-a-trailer\r\n\r\n", errChunkFraming},
		"size line too long":    {strings.Repeat("0", maxChunkLine+1) + "\r\n", errChunkFraming},
		"too many trailers":     {"0\r\n" + strings.Repeat("a:b\r\n", maxTrailers+1) + "\r\n", errChunkFraming},
		"ends in the size":      {"5", domain.ErrIncompleteBody},
		"ends in the data":      {"5\r\nhel", domain.ErrIncompleteBody},
		"ends before trailers":  {"5\r\nhello\r\n0\r\n", domain.ErrIncompleteBody},
		"empty":                 {"", domain.ErrIncompleteBody},
	}
	for name, tt := range tests {
		reader := newChunkedReader(strings.NewReader(tt.body))
		if _, err := io.ReadAll(reader); !errors.Is(err, tt.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tt.want)
		}
		// The error is sticky.
		if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, tt.want) {
			t.Errorf("%s: second read error = %v, want %v", name, err, tt.want)
		}
	}

	boom := errors.New("connection reset")
	reader := newChunkedReader(io.MultiReader(strings.NewReader("5\r\nhe"), iotest.ErrReader(boom)))
	if _, err := io.ReadAll(reader); !errors.Is(err, boom) {
		t.Errorf("transport error = %v, want %v", err, boom)
	}
	if n, err := newChunkedReader(strings.NewReader("0\r\n\r\n")).Read(nil); n != 0 || err != nil {
		t.Errorf("zero-length read = %d, %v", n, err)
	}
}
