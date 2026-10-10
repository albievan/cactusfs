package s3api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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

// presign returns path with a presigned query for the server's root key.
func (s *testServer) presign(method, path string, signedAt time.Time, expires int, headers map[string]string) string {
	s.t.Helper()
	signed := []string{"host"}
	for name := range headers {
		signed = append(signed, strings.ToLower(name))
	}
	slices.Sort(signed)
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	scopeDate := signedAt.UTC().Format(scopeDateFormat)
	target := path + separator + "X-Amz-Algorithm=" + sigV4Algorithm +
		"&X-Amz-Credential=" + url.QueryEscape(s.keyID+"/"+scopeDate+"/"+DefaultRegion+"/s3/aws4_request") +
		"&X-Amz-Date=" + signedAt.UTC().Format(amzDateFormat) + "&X-Amz-Expires=" + strconv.Itoa(expires) +
		"&X-Amz-SignedHeaders=" + url.QueryEscape(strings.Join(signed, ";"))
	r, err := http.NewRequestWithContext(s.t.Context(), method, s.URL+target, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	r.Host = r.URL.Host
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	sig := signature{
		accessKeyID: s.keyID, scopeDate: scopeDate, region: DefaultRegion,
		signedHeaders: signed, signedHeadersRaw: strings.Join(signed, ";"),
	}
	return target + "&X-Amz-Signature=" + expectedSignature(s.key, sig, signedAt.UTC().Format(amzDateFormat), canonicalRequest(r, sig, payloadUnsigned))
}

// TestPresignedURLs: a URL signed by the holder of a key lets whoever has it
// do exactly what was signed, until it expires.
func TestPresignedURLs(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	content := []byte("shared by link")

	// Upload and download with nothing but the URLs.
	put := s.presign(http.MethodPut, "/bkt/shared.txt", serverNow, 600, nil)
	s.mustStatus(s.do(call{method: http.MethodPut, path: put, body: content, unsigned: true}), http.StatusOK)
	get := s.presign(http.MethodGet, "/bkt/shared.txt", serverNow, 600, nil)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: get, unsigned: true}), http.StatusOK); !bytes.Equal(got.body, content) {
		t.Fatalf("presigned GET returned %q", got.body)
	}
	if obs := s.seen.last(); obs.Operation != "GetObject" {
		t.Errorf("operation of a presigned GET = %q", obs.Operation)
	}
	// With a signed header, and a query parameter of the operation.
	ranged := s.presign(http.MethodGet, "/bkt/shared.txt", serverNow, 600, map[string]string{"Range": "bytes=0-5"})
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: ranged, unsigned: true, headers: map[string]string{"Range": "bytes=0-5"}}), http.StatusPartialContent); string(got.body) != "shared" {
		t.Errorf("presigned ranged GET returned %q", got.body)
	}
	listing := s.presign(http.MethodGet, "/bkt?list-type=2&prefix=sh", serverNow, 600, nil)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: listing, unsigned: true}), http.StatusOK); !strings.Contains(string(got.body), "shared.txt") {
		t.Errorf("presigned listing = %s", got.body)
	}

	// An x-amz-* header carried in the signed query string means what the
	// header means: one that asks for something unimplemented is refused,
	// not ignored.
	hoisted := s.presign(http.MethodGet, "/bkt/shared.txt?x-amz-checksum-mode=ENABLED", serverNow, 600, nil)
	s.mustStatus(s.do(call{method: http.MethodGet, path: hoisted, unsigned: true}), http.StatusOK)
	locked := s.presign(http.MethodPut, "/bkt/locked.txt?x-amz-website-redirect-location=%2Felsewhere", serverNow, 600, nil)
	s.expectError(s.do(call{method: http.MethodPut, path: locked, body: content, unsigned: true}), http.StatusNotImplemented, "NotImplemented")
	twice := s.presign(http.MethodGet, "/bkt/shared.txt?x-amz-checksum-mode=ENABLED", serverNow, 600, map[string]string{"X-Amz-Checksum-Mode": "ENABLED"})
	s.expectError(s.do(call{method: http.MethodGet, path: twice, unsigned: true, headers: map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}}),
		http.StatusBadRequest, "AuthorizationQueryParametersError")

	refused := []struct {
		name   string
		c      call
		status int
		code   string
	}{
		{"for another method", call{method: http.MethodDelete, path: get}, http.StatusForbidden, "SignatureDoesNotMatch"},
		{"for another object", call{method: http.MethodGet, path: strings.Replace(get, "shared.txt", "other.txt", 1)}, http.StatusForbidden, "SignatureDoesNotMatch"},
		{"with a parameter added", call{method: http.MethodGet, path: get + "&versionId=x"}, http.StatusForbidden, "SignatureDoesNotMatch"},
		{"with a longer life", call{method: http.MethodGet, path: strings.Replace(get, "Expires=600", "Expires=6000", 1)}, http.StatusForbidden, "SignatureDoesNotMatch"},
		{"expired", call{method: http.MethodGet, path: s.presign(http.MethodGet, "/bkt/shared.txt", serverNow.Add(-time.Hour), 600, nil)}, http.StatusForbidden, "AccessDenied"},
		{"signed in the future", call{method: http.MethodGet, path: s.presign(http.MethodGet, "/bkt/shared.txt", serverNow.Add(time.Hour), 600, nil)}, http.StatusForbidden, "RequestTimeTooSkewed"},
		{"valid for longer than a week", call{method: http.MethodGet, path: s.presign(http.MethodGet, "/bkt/shared.txt", serverNow, 604801, nil)}, http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"with half a signature", call{method: http.MethodGet, path: "/bkt/shared.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=00"}, http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"with only an expiry", call{method: http.MethodGet, path: "/bkt/shared.txt?X-Amz-Expires=60"}, http.StatusBadRequest, "AuthorizationQueryParametersError"},
		{"with an unsigned x-amz header", call{method: http.MethodGet, path: get, headers: map[string]string{"X-Amz-Meta-A": "b"}}, http.StatusBadRequest, "AuthorizationHeaderMalformed"},
		{"Signature Version 2", call{method: http.MethodGet, path: "/bkt/shared.txt?AWSAccessKeyId=x&Signature=y&Expires=1"}, http.StatusBadRequest, "InvalidRequest"},
	}
	for _, tt := range refused {
		tt.c.unsigned = true
		r := s.do(tt.c)
		if r.err != nil {
			t.Fatalf("%s: %v", tt.name, r.err)
		}
		if got := s.errorCode(r); r.status != tt.status || got != tt.code {
			t.Errorf("a presigned URL %s = %d %s, want %d %s", tt.name, r.status, got, tt.status, tt.code)
		}
	}
	// Two signatures on one request: neither is chosen.
	s.expectError(s.do(call{method: http.MethodGet, path: get}), http.StatusBadRequest, "AuthorizationQueryParametersError")
	// The object is as it was after all of that.
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/shared.txt"}), http.StatusOK); !bytes.Equal(got.body, content) {
		t.Error("a refused presigned request changed the object")
	}
	// A URL acts as its signer does now, not as the signer did then: when
	// the key stops working, so do its URLs.
	if strings.Contains(s.logs.String(), string(s.key)) {
		t.Fatal("the secret key is in the log")
	}
}

// TestSignedStreamingUpload: an upload in signed chunks through the whole
// handler, and one with a chunk changed on the way.
func TestSignedStreamingUpload(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	content := bytes.Repeat([]byte("signed in pieces. "), 5000)

	upload := func(tamper func(body []byte) []byte) reply {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, s.URL+"/bkt/streamed", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = r.URL.Host
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set(headerDecodedLen, strconv.Itoa(len(content)))
		signRequest(r, s.keyID, s.key, serverNow, payloadStreamingSigned)
		authorization := r.Header.Get("Authorization")
		seed := authorization[strings.LastIndex(authorization, "=")+1:]
		sig := signature{accessKeyID: s.keyID, scopeDate: serverNow.Format(scopeDateFormat), region: DefaultRegion}
		signer := &chunkSigner{key: signingKey(s.key, sig), timestamp: serverNow.Format(amzDateFormat), scope: scopeOf(sig), previous: seed}
		var body bytes.Buffer
		for rest := content; ; {
			chunk := rest[:min(len(rest), 32<<10)]
			rest = rest[len(chunk):]
			digest := sha256.Sum256(chunk)
			text := chunkAlgorithm + "\n" + signer.timestamp + "\n" + signer.scope + "\n" + signer.previous + "\n" + emptyPayloadSHA256 + "\n" + hex.EncodeToString(digest[:])
			signer.previous = hex.EncodeToString(hmacSHA256(signer.key, text))
			fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n%s\r\n", len(chunk), signer.previous, chunk)
			if len(chunk) == 0 {
				break
			}
		}
		sent := body.Bytes()
		if tamper != nil {
			sent = tamper(sent)
		}
		r.Body = io.NopCloser(bytes.NewReader(sent))
		r.ContentLength = int64(len(sent))
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return reply{status: resp.StatusCode, header: resp.Header, body: data}
	}

	// A chunk changed in transit: refused, and nothing is stored.
	s.expectError(upload(func(body []byte) []byte {
		body[len(body)/2] ^= 1
		return body
	}), http.StatusForbidden, "SignatureDoesNotMatch")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/streamed"}), http.StatusNotFound, "NoSuchKey")

	s.mustStatus(upload(nil), http.StatusOK)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/streamed"}), http.StatusOK); !bytes.Equal(got.body, content) {
		t.Fatal("the object is not what was uploaded in signed chunks")
	}
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/bkt/streamed"}), http.StatusOK)
	if head.header.Get("Content-Encoding") != "" {
		t.Errorf("aws-chunked was stored as the object's Content-Encoding: %q", head.header.Get("Content-Encoding"))
	}
}

func TestUnsupportedOperationsAreRefusedNotMisrouted(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.putObject("/bkt/k", []byte("x"))

	notImplemented := []call{
		{method: http.MethodGet, path: "/bkt?acl"},
		{method: http.MethodPut, path: "/bkt?versioning&mfa=1"},
		{method: http.MethodPut, path: "/bkt/k?versionId=null", body: []byte("x")},
		{method: http.MethodPost, path: "/bkt?delete&versionId=null"},
		{method: http.MethodGet, path: "/bkt?uploads&acl"},
		{method: http.MethodGet, path: "/bkt"}, // ListObjects version 1
		{method: http.MethodGet, path: "/bkt/k?tagging&acl"},
		{method: http.MethodPut, path: "/bkt/k?partNumber=1&uploadId=u&tagging"},
		{method: http.MethodDelete, path: "/bkt/k?uploadId=u&versionId=null"},
		{method: http.MethodGet, path: "/bkt/k?partNumber=1"},
		{method: http.MethodPut, path: "/bkt/copy", headers: map[string]string{"X-Amz-Copy-Source": "/bkt/k"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("x"), payloadHash: "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD"},
	}
	for _, c := range notImplemented {
		s.expectError(s.do(c), http.StatusNotImplemented, "NotImplemented")
	}
	// A request about an upload is about the upload, never about the
	// object of the same key: this one deletes nothing.
	s.expectError(s.do(call{method: http.MethodDelete, path: "/bkt/k?uploadId=u"}), http.StatusNotFound, "NoSuchUpload")
	s.mustStatus(s.do(call{method: http.MethodHead, path: "/bkt/k"}), http.StatusOK)
	for _, c := range []call{
		{method: http.MethodPost, path: "/bkt/k"},
		{method: http.MethodPost, path: "/bkt"},
		{method: http.MethodPut, path: "/"},
		{method: http.MethodPatch, path: "/bkt/k"},
	} {
		s.expectError(s.do(c), http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
	// Sub-resources and headers that are newer than this code, or spelled
	// so that a lenient parser would drop them, are refused all the same:
	// the rule is an allow-list.
	s.createBucket("empty-bucket")
	for _, c := range []call{
		{method: http.MethodDelete, path: "/empty-bucket?metadataTable"},
		{method: http.MethodDelete, path: "/empty-bucket?someFutureFeature=1"},
		{method: http.MethodPut, path: "/bkt/k?renameObject"},
		{method: http.MethodGet, path: "/bkt/k?response-content-type=text%2Fhtml"},
		{method: http.MethodGet, path: "/?max-buckets=1"},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"If-None-Match": "*"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"If-Match": `"abc"`}},
		{method: http.MethodGet, path: "/bkt/k", headers: map[string]string{"If-None-Match": `"abc"`}},
		{method: http.MethodGet, path: "/bkt/k", headers: map[string]string{"If-Modified-Since": "Wed, 07 Oct 2026 12:00:00 GMT"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Website-Redirect-Location": "/elsewhere"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Server-Side-Encryption": "aws:kms:dsse"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Server-Side-Encryption-Bucket-Key-Enabled": "true"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Tagging-Directive": "REPLACE"}},
		{method: http.MethodDelete, path: "/bkt/k", headers: map[string]string{"X-Amz-Tagging": "a=b"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Acl": "public-read"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Storage-Class": "GLACIER"}},
		{method: http.MethodPut, path: "/bkt/k", body: []byte("y"), headers: map[string]string{"X-Amz-Some-Future-Header": "1"}},
		{method: http.MethodPut, path: "/public-bucket", headers: map[string]string{"X-Amz-Acl": "public-read-write"}},
	} {
		s.expectError(s.do(c), http.StatusNotImplemented, "NotImplemented")
	}
	// A query string that does not parse is an error, not an empty query.
	for _, path := range []string{"/bkt/k?uploadId=abc;x", "/bkt/k?%zz=1"} {
		s.expectError(s.do(call{method: http.MethodDelete, path: path}), http.StatusBadRequest, "InvalidURI")
	}
	s.mustStatus(s.do(call{method: http.MethodHead, path: "/empty-bucket"}), http.StatusOK)
	if r := s.do(call{method: http.MethodHead, path: "/public-bucket"}); r.status != http.StatusNotFound {
		t.Errorf("a bucket was created with an ACL that is not implemented: HEAD = %d", r.status)
	}
	// What is implemented, said explicitly, is accepted.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt/explicit", body: []byte("z"), headers: map[string]string{
		"X-Amz-Server-Side-Encryption": "AES256", "X-Amz-Storage-Class": "STANDARD",
	}}), http.StatusOK)

	// The object and the bucket are as they were: "?acl" was not treated as
	// a plain GET, nor the multipart DELETE as a delete of the object.
	if r := s.do(call{method: http.MethodGet, path: "/bkt/k"}); string(r.body) != "x" {
		t.Errorf("the object changed: %d %q", r.status, r.body)
	}
	// The SDK's x-id query parameter is not a subresource.
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k?x-id=GetObject"}), http.StatusOK)
}

// rawRequest sends bytes over a plain connection and returns what comes back
// once the server answers or closes. It lets a test cut a request short in
// ways an HTTP client never would.
func (s *testServer) rawRequest(head string, body []byte) string {
	s.t.Helper()
	conn, err := (&net.Dialer{}).DialContext(s.t.Context(), "tcp", s.Listener.Addr().String())
	if err != nil {
		s.t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		s.t.Fatal(err)
	}
	if _, err := conn.Write(append([]byte(head), body...)); err != nil {
		s.t.Fatalf("write: %v", err)
	}
	// Half-close: the client is gone in the middle of its body.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	reply, _ := io.ReadAll(conn)
	return string(reply)
}

// signedHead builds the request line and headers of a signed PUT whose body
// the test then sends by hand.
func (s *testServer) signedHead(path string, headers map[string]string) string {
	s.t.Helper()
	r, err := http.NewRequestWithContext(s.t.Context(), http.MethodPut, s.URL+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	r.Host = r.URL.Host
	signRequest(r, s.keyID, s.key, serverNow, payloadUnsigned)
	var b strings.Builder
	fmt.Fprintf(&b, "PUT %s HTTP/1.1\r\nHost: %s\r\n", path, r.Host)
	for name, values := range r.Header {
		fmt.Fprintf(&b, "%s: %s\r\n", name, values[0])
	}
	for name, value := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", name, value)
	}
	b.WriteString("\r\n")
	return b.String()
}

// TestUploadCutShortIsNeverStored: a client that disappears in the middle of
// its body has not uploaded an object. With an unsigned payload and no
// declared length or digest, only the transport knows the body was cut - and
// what it reports must not be read as the end of the object.
func TestUploadCutShortIsNeverStored(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		headers map[string]string
		body    string
	}{
		// A 65,536-byte chunk is announced; 1,000 bytes arrive.
		"chunked transfer cut inside a chunk": {
			headers: map[string]string{"Transfer-Encoding": "chunked"},
			body:    "10000\r\n" + strings.Repeat("x", 1000),
		},
		"chunked transfer without its final chunk": {
			headers: map[string]string{"Transfer-Encoding": "chunked"},
			body:    "3e8\r\n" + strings.Repeat("x", 1000) + "\r\n",
		},
		"content length not reached": {
			headers: map[string]string{"Content-Length": "65536"},
			body:    strings.Repeat("x", 1000),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, "")
			s.createBucket("bkt")
			original := random(t, 300_000)
			s.putObject("/bkt/k", original)

			reply := s.rawRequest(s.signedHead("/bkt/k", tt.headers), []byte(tt.body))

			if strings.HasPrefix(reply, "HTTP/1.1 200") {
				t.Errorf("the truncated upload was acknowledged: %.80q", reply)
			}
			got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusOK)
			if !bytes.Equal(got.body, original) {
				t.Fatalf("the object was replaced by %d bytes of a truncated upload", len(got.body))
			}
			if n := len(s.drive.IDs()); n != 1 {
				t.Errorf("%d shards on the drive, want only the original's", n)
			}
		})
	}
}

func TestContentMD5MustBeSigned(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")

	data := []byte("payload")
	// Added after signing: an attacker on the path could otherwise swap it.
	r := s.do(call{method: http.MethodPut, path: "/bkt/k", body: data, mutate: func(r *http.Request) {
		r.Header.Set("Content-MD5", contentMD5(data))
	}})
	s.expectError(r, http.StatusBadRequest, "AuthorizationHeaderMalformed")
}

func TestCreateBucketBodyIsVerified(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")

	body := []byte(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>`)
	r := s.do(call{method: http.MethodPut, path: "/tampered", body: body, payloadHash: strings.Repeat("ab", 32)})
	s.expectError(r, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
	if r := s.do(call{method: http.MethodHead, path: "/tampered"}); r.status != http.StatusNotFound {
		t.Errorf("a bucket was created from a body that does not match its declared hash: HEAD = %d", r.status)
	}
}

// TestDamagedObjectTransferFailsOverHTTP2: production is TLS, where Go speaks
// HTTP/2. A response that has begun and then hits damaged data must end as a
// failed transfer there too.
func TestDamagedObjectTransferFailsOverHTTP2(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	data := random(t, 3_000_000)
	s.putObject("/bkt/k", data)

	version, err := s.objects.Head(t.Context(), s.principal(), "bkt", "k")
	if err != nil {
		t.Fatal(err)
	}
	id := object.ShardID{DataID: version.DataID}
	stored, _ := s.drive.Bytes(id)
	stored[len(stored)-100] ^= 1
	s.drive.Corrupt(id, stored)

	tlsServer := httptest.NewUnstartedServer(s.Config.Handler)
	tlsServer.EnableHTTP2 = true
	tlsServer.StartTLS()
	defer tlsServer.Close()

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tlsServer.URL+"/bkt/k", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = r.URL.Host
	signRequest(r, s.keyID, s.key, serverNow, emptyPayloadSHA256)
	resp, err := tlsServer.Client().Do(r)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("the test did not use HTTP/2: %s", resp.Proto)
	}
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("the transfer of a damaged object completed without an error: %d of %d bytes", len(got), len(data))
	}
	if len(got) >= len(data) || !bytes.HasPrefix(data, got) {
		t.Errorf("received %d bytes, not a verified prefix of the %d-byte object", len(got), len(data))
	}
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
		{name: "data changed in transit", call: chunkedCall(awsChunked(append([]byte{data[0] ^ 1}, data[1:]...), 8192, good), len(data), "x-amz-checksum-crc32"), status: 400, code: "BadDigest"},
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
		{domain.ErrInsufficientDrives, 503, "ServiceUnavailable"},
		{domain.ErrUnavailable, 503, "ServiceUnavailable"},
		{domain.ErrDriveOffline, 503, "ServiceUnavailable"},
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

func versioningBody(status string) []byte {
	return []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status + `</Status></VersioningConfiguration>`)
}

// TestVersioningThroughTheAPI: the versioning operations as an S3 client
// uses them.
func TestVersioningThroughTheAPI(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")

	// A bucket that never kept versions says so with an empty element,
	// and its objects have no version header.
	state := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?versioning"}), http.StatusOK)
	if strings.Contains(string(state.body), "<Status>") || !strings.Contains(string(state.body), "<VersioningConfiguration") {
		t.Errorf("versioning of a new bucket = %s", state.body)
	}
	if put := s.putObject("/bkt/k", []byte("before")); put.header.Get(headerVersionID) != "" {
		t.Errorf("a put into an unversioned bucket has a version ID: %q", put.header.Get(headerVersionID))
	}

	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?versioning", body: versioningBody("Enabled")}), http.StatusOK)
	state = s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?versioning"}), http.StatusOK)
	if !strings.Contains(string(state.body), "<Status>Enabled</Status>") {
		t.Errorf("versioning after enabling = %s", state.body)
	}
	first := s.putObject("/bkt/k", []byte("first")).header.Get(headerVersionID)
	second := s.putObject("/bkt/k", []byte("second")).header.Get(headerVersionID)
	if len(first) != 32 || len(second) != 32 || first == second {
		t.Fatalf("version IDs %q and %q", first, second)
	}
	for id, want := range map[string]string{"null": "before", first: "first", second: "second"} {
		got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=" + id}), http.StatusOK)
		if string(got.body) != want || got.header.Get(headerVersionID) != id {
			t.Errorf("GET versionId=%s = %q, header %q", id, got.body, got.header.Get(headerVersionID))
		}
		head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/bkt/k?versionId=" + id}), http.StatusOK)
		if head.header.Get(headerVersionID) != id || head.header.Get("Content-Length") != strconv.Itoa(len(want)) {
			t.Errorf("HEAD versionId=%s: %v", id, head.header)
		}
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusOK); string(got.body) != "second" || got.header.Get(headerVersionID) != second {
		t.Errorf("GET of the current version = %q, %q", got.body, got.header.Get(headerVersionID))
	}

	// Deleting the object makes a marker; the versions stay.
	deleted := s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt/k"}), http.StatusNoContent)
	marker := deleted.header.Get(headerVersionID)
	if deleted.header.Get(headerDeleteMarker) != "true" || len(marker) != 32 {
		t.Fatalf("DELETE in a versioned bucket: %v", deleted.header)
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusNotFound, "NoSuchKey")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=" + marker}), http.StatusMethodNotAllowed, "MethodNotAllowed")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=" + strings.Repeat("ab", 16)}), http.StatusNotFound, "NoSuchVersion")
	for _, bad := range []string{"", "1", "NULL", strings.Repeat("A", 32), strings.Repeat("0", 32), "../../etc"} {
		s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=" + url.QueryEscape(bad)}), http.StatusBadRequest, "InvalidArgument")
	}

	listing := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?versions"}), http.StatusOK)
	var listed struct {
		Entries []struct {
			XMLName   xml.Name
			Key       string `xml:"Key"`
			VersionID string `xml:"VersionId"`
			IsLatest  bool   `xml:"IsLatest"`
			Size      *int64 `xml:"Size"`
		} `xml:",any"`
	}
	if err := xml.Unmarshal(listing.body, &listed); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range listed.Entries {
		if e.XMLName.Local != "Version" && e.XMLName.Local != "DeleteMarker" {
			continue
		}
		entry := e.XMLName.Local + ":" + e.VersionID
		if e.IsLatest {
			entry += "*"
		}
		if (e.XMLName.Local == "DeleteMarker") != (e.Size == nil) {
			t.Errorf("entry %s: a marker has no size and a version has one", entry)
		}
		order = append(order, entry)
	}
	if want := []string{"DeleteMarker:" + marker + "*", "Version:" + second, "Version:" + first, "Version:null"}; !slices.Equal(order, want) {
		t.Fatalf("ListObjectVersions = %v, want %v\n%s", order, want, listing.body)
	}
	paged := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?versions&max-keys=2"}), http.StatusOK)
	if !strings.Contains(string(paged.body), "<IsTruncated>true</IsTruncated>") || !strings.Contains(string(paged.body), "<NextVersionIdMarker>"+second+"</NextVersionIdMarker>") {
		t.Errorf("first page of two = %s", paged.body)
	}
	rest := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?versions&key-marker=k&version-id-marker=" + second}), http.StatusOK)
	if strings.Contains(string(rest.body), second+"</VersionId>") || !strings.Contains(string(rest.body), first+"</VersionId>") {
		t.Errorf("the page after the marker = %s", rest.body)
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?versions&version-id-marker=" + second}), http.StatusBadRequest, "InvalidArgument")

	// Removing the marker restores the object; removing a version is final.
	undone := s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt/k?versionId=" + marker}), http.StatusNoContent)
	if undone.header.Get(headerDeleteMarker) != "true" || undone.header.Get(headerVersionID) != marker {
		t.Errorf("DELETE of a marker: %v", undone.header)
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusOK); string(got.body) != "second" {
		t.Errorf("after the marker was removed: %q", got.body)
	}
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt/k?versionId=" + first}), http.StatusNoContent)
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=" + first}), http.StatusNotFound, "NoSuchVersion")
	// Deleting it again, or what never was, is not an error.
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt/k?versionId=" + first}), http.StatusNoContent)

	// What is not understood is refused.
	for name, body := range map[string][]byte{
		"no status":         []byte(`<VersioningConfiguration/>`),
		"unknown status":    versioningBody("On"),
		"unknown element":   []byte(`<VersioningConfiguration><Status>Enabled</Status><Extra/></VersioningConfiguration>`),
		"another document":  []byte(`<Tagging><Status>Enabled</Status></Tagging>`),
		"a document type":   []byte(`<!DOCTYPE x [<!ENTITY a "Enabled">]><VersioningConfiguration><Status>&a;</Status></VersioningConfiguration>`),
		"not XML":           []byte(`{"Status":"Enabled"}`),
		"empty":             {},
		"nested too deeply": []byte(`<VersioningConfiguration><Status><Status><Status><Status><Status><Status><Status><Status>Enabled</Status></Status></Status></Status></Status></Status></Status></Status></VersioningConfiguration>`),
	} {
		s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?versioning", body: body}), http.StatusBadRequest, "MalformedXML")
		_ = name
	}
	mfa := []byte(`<VersioningConfiguration><Status>Enabled</Status><MfaDelete>Enabled</MfaDelete></VersioningConfiguration>`)
	s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?versioning", body: mfa}), http.StatusNotImplemented, "NotImplemented")
	s.expectError(s.do(call{method: http.MethodPut, path: "/no-such-bucket?versioning", body: versioningBody("Enabled")}), http.StatusNotFound, "NoSuchBucket")
	// A bucket with versions left is not empty.
	s.expectError(s.do(call{method: http.MethodDelete, path: "/bkt"}), http.StatusConflict, "BucketNotEmpty")
}

// TestDeleteObjects: many deletes in one request, each on its own merits.
func TestDeleteObjects(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?versioning", body: versioningBody("Enabled")}), http.StatusOK)
	kept := s.putObject("/bkt/a", []byte("a")).header.Get(headerVersionID)
	gone := s.putObject("/bkt/b", []byte("b")).header.Get(headerVersionID)
	s.putObject("/bkt/c&lt;", []byte("c"))

	body := []byte(`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<Object><Key>a</Key></Object>
		<Object><Key>b</Key><VersionId>` + gone + `</VersionId></Object>
		<Object><Key>never-there</Key></Object>
		<Object><Key>b</Key><VersionId>not-a-version</VersionId></Object>
	</Delete>`)
	result := s.mustStatus(s.do(call{method: http.MethodPost, path: "/bkt?delete", body: body}), http.StatusOK)
	var parsed struct {
		Deleted []struct {
			Key, VersionID        string
			DeleteMarker          bool
			DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId"`
		} `xml:"Deleted"`
		Errors []struct{ Key, Code string } `xml:"Error"`
	}
	if err := xml.Unmarshal(result.body, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Deleted) != 3 || len(parsed.Errors) != 1 || parsed.Errors[0].Code != "InvalidArgument" || parsed.Errors[0].Key != "b" {
		t.Fatalf("DeleteObjects = %s", result.body)
	}
	if d := parsed.Deleted[0]; d.Key != "a" || !d.DeleteMarker || len(d.DeleteMarkerVersionID) != 32 {
		t.Errorf("deleting a: %+v", d)
	}
	// "a" has a marker and keeps its version; "b" lost that version for good.
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/a"}), http.StatusNotFound, "NoSuchKey")
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/a?versionId=" + kept}), http.StatusOK)
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/b?versionId=" + gone}), http.StatusNotFound, "NoSuchVersion")

	quiet := []byte(`<Delete><Quiet>true</Quiet><Object><Key>c&amp;lt;</Key></Object></Delete>`)
	if r := s.mustStatus(s.do(call{method: http.MethodPost, path: "/bkt?delete", body: quiet}), http.StatusOK); strings.Contains(string(r.body), "<Deleted>") {
		t.Errorf("a quiet delete reported what it deleted: %s", r.body)
	}

	var many strings.Builder
	many.WriteString("<Delete>")
	for i := range 1001 {
		fmt.Fprintf(&many, "<Object><Key>k%d</Key></Object>", i)
	}
	many.WriteString("</Delete>")
	for name, bad := range map[string][]byte{
		"too many keys":   []byte(many.String()),
		"no keys":         []byte(`<Delete></Delete>`),
		"unknown element": []byte(`<Delete><Object><Key>a</Key><Bypass>true</Bypass></Object></Delete>`),
		"not XML":         []byte(`a,b,c`),
	} {
		r := s.do(call{method: http.MethodPost, path: "/bkt?delete", body: bad})
		if code := s.errorCode(r); r.status != http.StatusBadRequest || code != "MalformedXML" {
			t.Errorf("%s: %d %s", name, r.status, code)
		}
	}
	// A body that is not the one that was signed deletes nothing.
	tampered := s.do(call{
		method: http.MethodPost, path: "/bkt?delete", body: []byte(`<Delete><Object><Key>b</Key></Object></Delete>`),
		payloadHash: strings.Repeat("0", 64),
	})
	s.expectError(tampered, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/b"}), http.StatusNotFound)
	s.expectError(s.do(call{method: http.MethodPost, path: "/no-such-bucket?delete", body: quiet}), http.StatusNotFound, "NoSuchBucket")
}

func FuzzStrictXML(f *testing.F) {
	f.Add(versioningBody("Enabled"))
	f.Add([]byte(`<Delete><Quiet>true</Quiet><Object><Key>a</Key><VersionId>null</VersionId></Object></Delete>`))
	f.Add([]byte(`<!DOCTYPE a [<!ENTITY b "c">]><Delete/>`))
	f.Add([]byte(`<?xml version="1.0"?><?php x?><Delete/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var cfg versioningConfiguration
		if err := decodeStrictXML(bytes.NewReader(data), maxRequestXML, &cfg, "VersioningConfiguration", "Status", "MfaDelete"); err == nil {
			if bytes.Contains(data, []byte("<!DOCTYPE")) || bytes.Contains(data, []byte("<!ENTITY")) {
				t.Fatalf("a document with a document type was accepted: %q", data)
			}
		}
		var del deleteRequest
		if err := decodeStrictXML(bytes.NewReader(data), maxDeleteXML, &del, "Delete", "Quiet", "Object*", "Key", "VersionId"); err == nil {
			if len(del.Objects) > 0 && bytes.Count(data, []byte("<Object")) < len(del.Objects) {
				t.Fatalf("more objects decoded than the document names: %q", data)
			}
		}
	})
}

// TestStrictDocuments: a body that says what to destroy is read as exactly
// what it says, or not at all.
func TestStrictDocuments(t *testing.T) {
	t.Parallel()
	del := []string{"Delete", "Quiet", "Object*", "Key", "VersionId"}
	tests := map[string]string{
		"an object hidden inside another": `<Delete><Object><Key>a</Key><Object><Key>hidden</Key></Object></Object></Delete>`,
		"two keys in one object":          `<Delete><Object><Key>a</Key><Key>b</Key></Object></Delete>`,
		"two version IDs in one object":   `<Delete><Object><Key>a</Key><VersionId>null</VersionId><VersionId>null</VersionId></Object></Delete>`,
		"quiet said twice":                `<Delete><Quiet>true</Quiet><Quiet>false</Quiet><Object><Key>a</Key></Object></Delete>`,
		"text beside elements":            `<Delete><Quiet>true<Object><Key>a</Key></Object></Quiet></Delete>`,
		"a second document":               `<Delete><Object><Key>a</Key></Object></Delete><Delete><Object><Key>b</Key></Object></Delete>`,
		"another root":                    `<Object><Key>a</Key></Object>`,
		"a key inside a key":              `<Delete><Object><Key>a<Key>b</Key></Key></Object></Delete>`,
	}
	for name, body := range tests {
		var in deleteRequest
		if err := decodeStrictXML(strings.NewReader(body), maxDeleteXML, &in, del...); err == nil {
			t.Errorf("%s: accepted, with %d objects", name, len(in.Objects))
		}
	}
	var in deleteRequest
	good := `<?xml version="1.0" encoding="UTF-8"?>
		<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Quiet>false</Quiet>
		<Object><Key>a</Key></Object><Object><Key>b</Key><VersionId>null</VersionId></Object></Delete>`
	if err := decodeStrictXML(strings.NewReader(good), maxDeleteXML, &in, del...); err != nil || len(in.Objects) != 2 || in.Objects[1].VersionID == nil {
		t.Fatalf("a well-formed document: %+v, %v", in, err)
	}
	var cfg versioningConfiguration
	twice := `<VersioningConfiguration><Status>Enabled</Status><Status>Suspended</Status></VersioningConfiguration>`
	if err := decodeStrictXML(strings.NewReader(twice), maxRequestXML, &cfg, "VersioningConfiguration", "Status", "MfaDelete"); err == nil {
		t.Errorf("a status said twice was accepted as %q", cfg.Status)
	}
}

// TestBodiesThatDestroyMustBeSigned: the body of DeleteObjects and of
// PutBucketVersioning decides what is destroyed. A signature that does not
// cover it - an unsigned payload, a presigned URL - is not enough.
func TestBodiesThatDestroyMustBeSigned(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.putObject("/bkt/k", []byte("kept"))
	body := []byte(`<Delete><Object><Key>k</Key></Object></Delete>`)

	unsigned := s.do(call{method: http.MethodPost, path: "/bkt?delete", body: body, payloadHash: payloadUnsigned})
	s.expectError(unsigned, http.StatusBadRequest, "InvalidRequest")
	presigned := s.presign(http.MethodPost, "/bkt?delete", serverNow, 600, nil)
	s.expectError(s.do(call{method: http.MethodPost, path: presigned, body: body, unsigned: true}), http.StatusBadRequest, "InvalidRequest")
	suspend := s.do(call{method: http.MethodPut, path: "/bkt?versioning", body: versioningBody("Suspended"), payloadHash: payloadUnsigned})
	s.expectError(suspend, http.StatusBadRequest, "InvalidRequest")
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusOK); string(got.body) != "kept" {
		t.Fatal("a request whose body was not signed deleted an object")
	}

	// A digest in a signed header binds the body as well as its SHA-256.
	digest := newETagHash()
	digest.Write(body)
	sum := digest.Sum(nil)
	withDigest := s.do(call{
		method: http.MethodPost, path: "/bkt?delete", body: body, payloadHash: payloadUnsigned,
		headers: map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum)},
	})
	s.mustStatus(withDigest, http.StatusOK)
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/k"}), http.StatusNotFound, "NoSuchKey")
}

func retentionBody(mode string, until time.Time) []byte {
	return []byte(`<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>` + mode + `</Mode><RetainUntilDate>` +
		until.UTC().Format(time.RFC3339) + `</RetainUntilDate></Retention>`)
}

// TestObjectLockThroughTheAPI: Object Lock as an S3 client uses it. The
// caller is the root credential throughout, which is the point: nothing it
// sends removes a compliance-locked version.
func TestObjectLockThroughTheAPI(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault", headers: map[string]string{headerLockEnabled: "true"}}), http.StatusOK)
	s.createBucket("plain")

	// The bucket keeps versions, and says that its versions can be locked.
	if state := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault?versioning"}), http.StatusOK); !strings.Contains(string(state.body), "<Status>Enabled</Status>") {
		t.Fatalf("versioning of a bucket created with Object Lock = %s", state.body)
	}
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault?versioning", body: versioningBody("Suspended")}), http.StatusConflict, "InvalidBucketState")
	cfg := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault?object-lock"}), http.StatusOK)
	if !strings.Contains(string(cfg.body), "<ObjectLockEnabled>Enabled</ObjectLockEnabled>") || strings.Contains(string(cfg.body), "<Rule>") {
		t.Errorf("lock configuration of a new bucket = %s", cfg.body)
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/plain?object-lock"}), http.StatusNotFound, "ObjectLockConfigurationNotFoundError")

	// A locked version, put with headers.
	until := serverNow.Add(30 * 24 * time.Hour)
	put := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/ledger", body: []byte("entries"), headers: map[string]string{
		headerLockMode: "COMPLIANCE", headerLockUntil: until.Format(time.RFC3339),
	}}), http.StatusOK)
	version := put.header.Get(headerVersionID)
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/ledger"}), http.StatusOK)
	if head.header.Get(headerLockMode) != "COMPLIANCE" || !strings.HasPrefix(head.header.Get(headerLockUntil), until.Format("2006-01-02T15:04:05")) {
		t.Errorf("lock headers of the version: %v", head.header)
	}
	shown := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/ledger?retention"}), http.StatusOK)
	if !strings.Contains(string(shown.body), "<Mode>COMPLIANCE</Mode>") {
		t.Errorf("GetObjectRetention = %s", shown.body)
	}

	// Nothing removes it: not a delete of the version, with or without
	// the bypass header, not a batch delete, not a shorter retention.
	for _, headers := range []map[string]string{nil, {headerLockOverride: "true"}} {
		s.expectError(s.do(call{method: http.MethodDelete, path: "/vault/ledger?versionId=" + version, headers: headers}), http.StatusForbidden, "AccessDenied")
	}
	batch := []byte(`<Delete><Object><Key>ledger</Key><VersionId>` + version + `</VersionId></Object></Delete>`)
	result := s.mustStatus(s.do(call{method: http.MethodPost, path: "/vault?delete", body: batch, headers: map[string]string{headerLockOverride: "true"}}), http.StatusOK)
	if !strings.Contains(string(result.body), "<Code>AccessDenied</Code>") || strings.Contains(string(result.body), "<Deleted>") {
		t.Errorf("DeleteObjects of a compliance-locked version = %s", result.body)
	}
	for name, body := range map[string][]byte{
		"shorter":    retentionBody("COMPLIANCE", until.Add(-time.Hour)),
		"governance": retentionBody("GOVERNANCE", until.Add(time.Hour)),
		"none":       []byte(`<Retention/>`),
	} {
		r := s.do(call{method: http.MethodPut, path: "/vault/ledger?retention", body: body, headers: map[string]string{headerLockOverride: "true"}})
		if code := s.errorCode(r); r.status != http.StatusForbidden || code != "AccessDenied" {
			t.Errorf("compliance retention made %s = %d %s", name, r.status, code)
		}
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/ledger?retention&versionId=" + version, body: retentionBody("COMPLIANCE", until.Add(time.Hour))}), http.StatusOK)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/ledger?versionId=" + version}), http.StatusOK); string(got.body) != "entries" {
		t.Fatal("the compliance-locked version is not what was stored")
	}
	// Deleting the object is a marker: the version stays under it.
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/vault/ledger"}), http.StatusNoContent)
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/ledger?versionId=" + version}), http.StatusOK)
	// And the bucket cannot be removed around it.
	s.expectError(s.do(call{method: http.MethodDelete, path: "/vault"}), http.StatusConflict, "BucketNotEmpty")

	// Governance: root may bypass, when it says so.
	gov := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/draft", body: []byte("draft"), headers: map[string]string{
		headerLockMode: "GOVERNANCE", headerLockUntil: until.Format(time.RFC3339),
	}}), http.StatusOK).header.Get(headerVersionID)
	s.expectError(s.do(call{method: http.MethodDelete, path: "/vault/draft?versionId=" + gov}), http.StatusForbidden, "AccessDenied")
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/vault/draft?versionId=" + gov, headers: map[string]string{headerLockOverride: "true"}}), http.StatusNoContent)

	// Legal hold.
	held := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/evidence", body: []byte("exhibit"), headers: map[string]string{headerLockLegalHold: "ON"}}), http.StatusOK).header.Get(headerVersionID)
	if hold := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/evidence?legal-hold"}), http.StatusOK); !strings.Contains(string(hold.body), "<Status>ON</Status>") {
		t.Errorf("GetObjectLegalHold = %s", hold.body)
	}
	s.expectError(s.do(call{method: http.MethodDelete, path: "/vault/evidence?versionId=" + held, headers: map[string]string{headerLockOverride: "true"}}), http.StatusForbidden, "AccessDenied")
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/evidence?retention"}), http.StatusNotFound, "NoSuchObjectLockConfiguration")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/evidence?legal-hold", body: []byte(`<LegalHold><Status>OFF</Status></LegalHold>`)}), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/vault/evidence?versionId=" + held}), http.StatusNoContent)

	// The bucket's default retention.
	rule := []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>10</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault?object-lock", body: rule}), http.StatusOK)
	if cfg := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault?object-lock"}), http.StatusOK); !strings.Contains(string(cfg.body), "<Days>10</Days>") {
		t.Errorf("lock configuration with a rule = %s", cfg.body)
	}
	s.putObject("/vault/defaulted", []byte("x"))
	if head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/defaulted"}), http.StatusOK); head.header.Get(headerLockMode) != "GOVERNANCE" {
		t.Errorf("a version put into a bucket with a default retention: %v", head.header)
	}

	// What cannot be understood, or cannot be done here, is refused.
	refused := []struct {
		name   string
		c      call
		status int
		code   string
	}{
		{"a mode without a date", call{method: http.MethodPut, path: "/vault/x", body: []byte("x"), headers: map[string]string{headerLockMode: "COMPLIANCE"}}, 400, "InvalidArgument"},
		{"a date without a mode", call{method: http.MethodPut, path: "/vault/x", body: []byte("x"), headers: map[string]string{headerLockUntil: until.Format(time.RFC3339)}}, 400, "InvalidArgument"},
		{"a mode that does not exist", call{method: http.MethodPut, path: "/vault/x", body: []byte("x"), headers: map[string]string{headerLockMode: "FOREVER", headerLockUntil: until.Format(time.RFC3339)}}, 400, "InvalidArgument"},
		{"a date in the past", call{method: http.MethodPut, path: "/vault/x", body: []byte("x"), headers: map[string]string{headerLockMode: "GOVERNANCE", headerLockUntil: serverNow.Add(-time.Hour).Format(time.RFC3339)}}, 400, "InvalidArgument"},
		{"a legal hold that is neither", call{method: http.MethodPut, path: "/vault/x", body: []byte("x"), headers: map[string]string{headerLockLegalHold: "yes"}}, 400, "InvalidArgument"},
		{"a lock in a bucket without", call{method: http.MethodPut, path: "/plain/x", body: []byte("x"), headers: map[string]string{headerLockLegalHold: "ON"}}, 400, "InvalidRequest"},
		{"retention in a bucket without", call{method: http.MethodPut, path: "/plain/x?retention", body: retentionBody("GOVERNANCE", until)}, 400, "InvalidRequest"},
		{"a rule on a bucket without", call{method: http.MethodPut, path: "/plain?object-lock", body: rule}, 400, "InvalidRequest"},
		{"a rule with two periods", call{method: http.MethodPut, path: "/vault?object-lock", body: []byte(`<ObjectLockConfiguration><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`)}, 400, "InvalidArgument"},
		{"a rule of zero days", call{method: http.MethodPut, path: "/vault?object-lock", body: []byte(`<ObjectLockConfiguration><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>0</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)}, 400, "InvalidArgument"},
		{"a rule with an unknown element", call{method: http.MethodPut, path: "/vault?object-lock", body: []byte(`<ObjectLockConfiguration><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days><Hours>1</Hours></DefaultRetention></Rule></ObjectLockConfiguration>`)}, 400, "MalformedXML"},
		{"an unsigned retention body", call{method: http.MethodPut, path: "/vault/defaulted?retention", body: retentionBody("COMPLIANCE", until), payloadHash: payloadUnsigned}, 400, "InvalidRequest"},
		{"a bypass that is neither", call{method: http.MethodDelete, path: "/vault/defaulted", headers: map[string]string{headerLockOverride: "maybe"}}, 400, "InvalidArgument"},
		{"lock enabled that is neither", call{method: http.MethodPut, path: "/another", headers: map[string]string{headerLockEnabled: "yes"}}, 400, "InvalidArgument"},
		{"retention of a version that is not there", call{method: http.MethodGet, path: "/vault/ledger?retention&versionId=" + strings.Repeat("ab", 16)}, 404, "NoSuchVersion"},
	}
	for _, tt := range refused {
		r := s.do(tt.c)
		if r.err != nil {
			t.Fatalf("%s: %v", tt.name, r.err)
		}
		if code := s.errorCode(r); r.status != tt.status || code != tt.code {
			t.Errorf("%s = %d %s, want %d %s", tt.name, r.status, code, tt.status, tt.code)
		}
	}
	// At the end the first version is where it was.
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/ledger?versionId=" + version}), http.StatusOK); string(got.body) != "entries" {
		t.Fatal("the compliance-locked version changed")
	}
}

// TestOneRuleOneAnswer: a lock configuration that says two things is not
// read as either.
func TestOneRuleOneAnswer(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault", headers: map[string]string{headerLockEnabled: "true"}}), http.StatusOK)
	two := []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>` +
		`<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>3650</Days></DefaultRetention></Rule>` +
		`<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault?object-lock", body: two}), http.StatusBadRequest, "MalformedXML")
	if cfg := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault?object-lock"}), http.StatusOK); strings.Contains(string(cfg.body), "<Rule>") {
		t.Errorf("a refused configuration left a rule: %s", cfg.body)
	}
	// A date no record can hold is an invalid date, not a failure of ours.
	far := map[string]string{headerLockMode: "COMPLIANCE", headerLockUntil: "9999-12-31T23:59:59-10:00"}
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault/x", body: []byte("x"), headers: far}), http.StatusBadRequest, "InvalidArgument")
	// A version without a lock says nothing about one.
	s.putObject("/vault/plain", []byte("x"))
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/plain"}), http.StatusOK)
	if head.header.Get(headerLockMode) != "" || head.header.Get(headerLockUntil) != "" || head.header.Get(headerLockLegalHold) != "" {
		t.Errorf("lock headers of a version without a lock: %v", head.header)
	}
}
