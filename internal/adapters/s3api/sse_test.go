package s3api

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/object"
)

// customerHeaders returns the three headers that carry a customer key.
func customerHeaders(key []byte) map[string]string {
	sum := newETagHash()
	sum.Write(key)
	return map[string]string{
		headerSSECAlgo:   "AES256",
		headerSSECKey:    base64.StdEncoding.EncodeToString(key),
		headerSSECKeyMD5: base64.StdEncoding.EncodeToString(sum.Sum(nil)),
	}
}

func with(headers map[string]string, more ...string) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		out[k] = v
	}
	for i := 0; i+1 < len(more); i += 2 {
		if more[i+1] == "" {
			delete(out, more[i])
		} else {
			out[more[i]] = more[i+1]
		}
	}
	return out
}

// TestCustomerKeyOverHTTP: SSE-C as an S3 client uses it.
func TestCustomerKeyOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTLSTestServer(t)
	s.createBucket("vault")
	key, other := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	mine := customerHeaders(key)
	body := []byte("for the holder of the key")

	put := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/secret", body: body, headers: mine}), http.StatusOK)
	if put.header.Get(headerSSECAlgo) != "AES256" || put.header.Get(headerSSECKeyMD5) != mine[headerSSECKeyMD5] ||
		put.header.Get(headerSSE) != "" || put.header.Get(headerSSECKey) != "" {
		t.Errorf("PutObject headers = %v", put.header)
	}
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/secret", headers: mine}), http.StatusOK)
	if !bytes.Equal(got.body, body) || got.header.Get(headerSSECKeyMD5) != mine[headerSSECKeyMD5] || got.header.Get(headerSSECKey) != "" {
		t.Errorf("GetObject = %q, %v", got.body, got.header)
	}
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/secret", headers: mine}), http.StatusOK)
	if head.header.Get(headerSSECAlgo) != "AES256" || head.header.Get("Content-Length") != "25" {
		t.Errorf("HeadObject headers = %v", head.header)
	}
	ranged := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/secret", headers: with(mine, "Range", "bytes=4-6")}), http.StatusPartialContent)
	if string(ranged.body) != "the" {
		t.Errorf("a range = %q", ranged.body)
	}

	// Without the key, with another, with half of the headers.
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/secret"}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/secret", headers: customerHeaders(other)}), http.StatusForbidden, "AccessDenied")
	if r := s.do(call{method: http.MethodHead, path: "/vault/secret"}); r.status != http.StatusBadRequest {
		t.Errorf("HEAD without the key = %d", r.status)
	}
	if r := s.do(call{method: http.MethodHead, path: "/vault/secret", headers: customerHeaders(other)}); r.status != http.StatusForbidden {
		t.Errorf("HEAD with another key = %d", r.status)
	}
	for name, headers := range map[string]map[string]string{
		"no algorithm":             with(mine, headerSSECAlgo, ""),
		"no digest":                with(mine, headerSSECKeyMD5, ""),
		"no key":                   with(mine, headerSSECKey, ""),
		"a digest of another":      with(mine, headerSSECKeyMD5, customerHeaders(other)[headerSSECKeyMD5]),
		"a digest that is none":    with(mine, headerSSECKeyMD5, "!!"),
		"a short key":              customerHeaders(key[:16]),
		"a key that is not base64": with(mine, headerSSECKey, "not base64 at all"),
		"with SSE-S3":              with(mine, headerSSE, "AES256"),
		"with SSE-KMS":             with(mine, headerSSE, "aws:kms"),
	} {
		r := s.do(call{method: http.MethodPut, path: "/vault/refused", body: body, headers: headers})
		if code := s.errorCode(r); r.status != http.StatusBadRequest || code != "InvalidArgument" {
			t.Errorf("PutObject with %s = %d %s, want 400 InvalidArgument", name, r.status, code)
		}
	}
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault/refused", body: body, headers: with(mine, headerSSECAlgo, "AES128")}),
		http.StatusBadRequest, "InvalidEncryptionAlgorithmError")
	// A header sent twice, once empty, is a header that was sent: the
	// object is not stored as if no key had come with it.
	for _, name := range []string{headerSSECAlgo, headerSSECKey, headerSSECKeyMD5, headerSSE} {
		headers := mine
		if name == headerSSE {
			headers = map[string]string{headerSSE: "aws:kms"}
		}
		value := headers[name]
		doubled := func(r *http.Request) { r.Header[http.CanonicalHeaderKey(name)] = []string{"", value} }
		r := s.do(call{method: http.MethodPut, path: "/vault/refused", body: body, headers: headers, mutate: doubled})
		if r.status == http.StatusOK {
			t.Errorf("a put with %s sent twice, once empty, was stored", name)
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/refused"}), http.StatusNotFound, "NoSuchKey")

	// By version, too, it takes the key.
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/secret?versionId=null", headers: mine}), http.StatusOK)
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/secret?versionId=null"}), http.StatusBadRequest, "InvalidRequest")

	// Its entity tag is no digest of its content, here or in a listing.
	if tag := got.header.Get("ETag"); tag == etagOf(body) || len(tag) != len(etagOf(body)) {
		t.Errorf("ETag of the SSE-C object = %s; the MD5 of its content is %s", tag, etagOf(body))
	}

	// A key with an object that has none; a key where none is read.
	s.putObject("/vault/plain", []byte("plain"))
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/plain", headers: mine}), http.StatusBadRequest, "InvalidRequest")
	for name, c := range map[string]call{
		"deleting":           {method: http.MethodDelete, path: "/vault/secret", headers: mine},
		"listing":            {method: http.MethodGet, path: "/vault?list-type=2", headers: mine},
		"creating a bucket":  {method: http.MethodPut, path: "/another", headers: mine},
		"reading a lock":     {method: http.MethodGet, path: "/vault/secret?retention", headers: mine},
		"SSE-KMS on a read":  {method: http.MethodGet, path: "/vault/plain", headers: map[string]string{headerSSE: "aws:kms"}},
		"SSE-S3 on a delete": {method: http.MethodDelete, path: "/vault/plain", headers: map[string]string{headerSSE: "AES256"}},
		"SSE-S3 on a part":   {method: http.MethodPut, path: "/vault/plain?partNumber=1&uploadId=00000000000000000000000000000001", headers: map[string]string{headerSSE: "AES256"}},
	} {
		r := s.do(c)
		if code := s.errorCode(r); r.status != http.StatusBadRequest || code != "InvalidArgument" {
			t.Errorf("encryption headers when %s = %d %s, want 400 InvalidArgument", name, r.status, code)
		}
	}
	s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/secret", headers: mine}), http.StatusOK)

	// Deleting needs no key; nor does listing, which shows the object.
	listing := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault?list-type=2"}), http.StatusOK)
	if !bytes.Contains(listing.body, []byte("<Key>secret</Key>")) {
		t.Errorf("listing = %s", listing.body)
	}
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/vault/secret"}), http.StatusNoContent)

	// The key is in no log line.
	if logs := s.logs.String(); strings.Contains(logs, mine[headerSSECKey]) || bytes.Contains([]byte(logs), key) {
		t.Error("the customer key was logged")
	}
}

// TestCustomerKeyNeedsTLS: a key sent in the clear has been disclosed
// already; it is not used.
func TestCustomerKeyNeedsTLS(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("vault")
	mine := customerHeaders(bytes.Repeat([]byte{1}, 32))
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault/secret", body: []byte("x"), headers: mine}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/secret", headers: mine}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/secret"}), http.StatusNotFound, "NoSuchKey")
}

// TestSSEKMSOverHTTP: SSE-KMS with the deployment's key, and nothing else
// under that name.
func TestSSEKMSOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTLSTestServer(t)
	s.createBucket("vault")
	encode := func(json string) string { return base64.StdEncoding.EncodeToString([]byte(json)) }
	headers := map[string]string{headerSSE: "aws:kms", headerSSEKMSKey: "test:1", headerSSEContext: encode(`{"team":"finance"}`)}
	put := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/k", body: []byte("kms"), headers: headers}), http.StatusOK)
	if put.header.Get(headerSSE) != "aws:kms" || put.header.Get(headerSSEKMSKey) != "test:1" {
		t.Errorf("PutObject headers = %v", put.header)
	}
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/k"}), http.StatusOK)
	if string(got.body) != "kms" || got.header.Get(headerSSE) != "aws:kms" || got.header.Get(headerSSEKMSKey) != "test:1" {
		t.Errorf("GetObject = %q, %v", got.body, got.header)
	}
	// Without a key ID it is the deployment's key; said explicitly,
	// AES256 is the default.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/default-key", body: []byte("x"), headers: map[string]string{headerSSE: "aws:kms"}}), http.StatusOK)
	plain := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/aes", body: []byte("x"), headers: map[string]string{headerSSE: "AES256"}}), http.StatusOK)
	if plain.header.Get(headerSSE) != "AES256" {
		t.Errorf("SSE-S3 headers = %v", plain.header)
	}
	s.mustStatus(s.do(call{
		method: http.MethodPut, path: "/vault/no-bucket-key", body: []byte("x"),
		headers: map[string]string{headerSSE: "aws:kms", headerSSEBucketKey: "false"},
	}), http.StatusOK)

	for name, tt := range map[string]struct {
		headers map[string]string
		status  int
		code    string
	}{
		"another KMS key":              {with(headers, headerSSEKMSKey, "arn:aws:kms:us-east-1:1:key/other"), 400, "InvalidArgument"},
		"a context that is not base64": {with(headers, headerSSEContext, "{}"), 400, "InvalidArgument"},
		"a context that is not JSON":   {with(headers, headerSSEContext, encode("team=finance")), 400, "InvalidArgument"},
		"a context of numbers":         {with(headers, headerSSEContext, encode(`{"team":1}`)), 400, "InvalidArgument"},
		"a context that is a list":     {with(headers, headerSSEContext, encode(`["a"]`)), 400, "InvalidArgument"},
		"two contexts in one":          {with(headers, headerSSEContext, encode(`{"a":"b"}{"c":"d"}`)), 400, "InvalidArgument"},
		"a context without SSE-KMS":    {map[string]string{headerSSEContext: encode(`{"a":"b"}`)}, 400, "InvalidArgument"},
		"a KMS key with SSE-S3":        {map[string]string{headerSSE: "AES256", headerSSEKMSKey: "test:1"}, 400, "InvalidArgument"},
		"dual-layer":                   {map[string]string{headerSSE: "aws:kms:dsse"}, 501, "NotImplemented"},
		"a mode there is not":          {map[string]string{headerSSE: "aes256"}, 400, "InvalidArgument"},
		"a context with a tail":        {with(headers, headerSSEContext, encode(`{"a":"b"}}`)), 400, "InvalidArgument"},
		"a context that is null":       {with(headers, headerSSEContext, encode(`null`)), 400, "InvalidArgument"},
		"a context with a key twice":   {with(headers, headerSSEContext, encode(`{"a":"b","a":"c"}`)), 400, "InvalidArgument"},
		"too many pairs":               {with(headers, headerSSEContext, encode(`{"1":"","2":"","3":"","4":"","5":"","6":"","7":"","8":"","9":"","10":"","11":"","12":"","13":"","14":"","15":"","16":"","17":""}`)), 400, "InvalidArgument"},
		"too large a context":          {with(headers, headerSSEContext, encode(`{"a":"`+strings.Repeat("x", 5000)+`"}`)), 400, "InvalidArgument"},
		"a bucket key":                 {map[string]string{headerSSE: "aws:kms", headerSSEBucketKey: "true"}, 501, "NotImplemented"},
	} {
		r := s.do(call{method: http.MethodPut, path: "/vault/refused", body: []byte("x"), headers: tt.headers})
		if code := s.errorCode(r); r.status != tt.status || code != tt.code {
			t.Errorf("PutObject with %s = %d %s, want %d %s", name, r.status, code, tt.status, tt.code)
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/refused"}), http.StatusNotFound, "NoSuchKey")
}

// TestMultipartEncryptionOverHTTP: an upload under SSE-C, and one under
// SSE-KMS.
func TestMultipartEncryptionOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTLSTestServer(t)
	s.createBucket("vault")
	mine := customerHeaders(bytes.Repeat([]byte{1}, 32))
	other := customerHeaders(bytes.Repeat([]byte{2}, 32))

	created := s.mustStatus(s.do(call{method: http.MethodPost, path: "/vault/film?uploads", headers: mine}), http.StatusOK)
	if created.header.Get(headerSSECAlgo) != "AES256" || created.header.Get(headerSSECKeyMD5) != mine[headerSSECKeyMD5] {
		t.Errorf("CreateMultipartUpload headers = %v", created.header)
	}
	var result initiateMultipartUploadResult
	if err := xml.Unmarshal(created.body, &result); err != nil {
		t.Fatal(err)
	}
	base := "/vault/film?uploadId=" + result.UploadID
	first, second := randomPart(t, object.MinPartSize), []byte("the end")
	s.expectError(s.do(call{method: http.MethodPut, path: base + "&partNumber=1", body: second}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodPut, path: base + "&partNumber=1", body: second, headers: other}), http.StatusForbidden, "AccessDenied")
	one := s.mustStatus(s.do(call{method: http.MethodPut, path: base + "&partNumber=1", body: first, headers: mine}), http.StatusOK)
	two := s.mustStatus(s.do(call{method: http.MethodPut, path: base + "&partNumber=2", body: second, headers: mine}), http.StatusOK)
	if one.header.Get(headerSSECKeyMD5) != mine[headerSSECKeyMD5] || one.header.Get(headerSSE) != "" {
		t.Errorf("UploadPart headers = %v", one.header)
	}
	// Completing takes no key; a client that sends one - transfer
	// managers do - has it checked.
	complete := completeBody(listedPart(1, one.header.Get("ETag")), listedPart(2, two.header.Get("ETag")))
	s.expectError(s.do(call{method: http.MethodPost, path: base, body: complete, headers: other}), http.StatusForbidden, "AccessDenied")
	s.expectError(s.do(call{method: http.MethodGet, path: base, headers: other}), http.StatusForbidden, "AccessDenied")
	s.mustStatus(s.do(call{method: http.MethodGet, path: base}), http.StatusOK)
	done := s.mustStatus(s.do(call{method: http.MethodPost, path: base, body: complete, headers: mine}), http.StatusOK)
	if done.header.Get(headerSSECAlgo) != "AES256" || done.header.Get(headerSSECKeyMD5) != mine[headerSSECKeyMD5] {
		t.Errorf("CompleteMultipartUpload headers = %v", done.header)
	}
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/film", headers: mine}), http.StatusOK)
	if !bytes.Equal(got.body, append(append([]byte{}, first...), second...)) {
		t.Errorf("GetObject = %d bytes", len(got.body))
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/film"}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/film", headers: other}), http.StatusForbidden, "AccessDenied")

	// SSE-KMS is said once, when the upload begins.
	kms := s.createUpload("/vault/kms", map[string]string{headerSSE: "aws:kms"})
	etag := s.uploadPart("/vault/kms", kms, 1, []byte("part"))
	s.mustStatus(s.do(call{method: http.MethodPost, path: "/vault/kms?uploadId=" + kms, body: completeBody(listedPart(1, etag))}), http.StatusOK)
	if head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/kms"}), http.StatusOK); head.header.Get(headerSSE) != "aws:kms" {
		t.Errorf("HEAD of the SSE-KMS multipart object = %v", head.header)
	}
}

// TestCustomerKeyAndPresignedURLs: a presigned URL can be for an object under
// SSE-C when the key travels in headers the signature covers; a key in the
// URL itself is refused, signed or not.
func TestCustomerKeyAndPresignedURLs(t *testing.T) {
	t.Parallel()
	s := newTLSTestServer(t)
	s.createBucket("vault")
	mine := customerHeaders(bytes.Repeat([]byte{1}, 32))
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/secret", body: []byte("shared with the key"), headers: mine}), http.StatusOK)

	signed := s.presign(http.MethodGet, "/vault/secret", serverNow, 600, mine)
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: signed, unsigned: true, headers: mine}), http.StatusOK)
	if string(got.body) != "shared with the key" {
		t.Errorf("presigned GET with the key in headers = %q", got.body)
	}
	// The same URL without the headers: the signature covers them.
	if r := s.do(call{method: http.MethodGet, path: signed, unsigned: true}); r.status != http.StatusForbidden && r.status != http.StatusBadRequest {
		t.Errorf("presigned GET without the signed key headers = %d", r.status)
	}
	// The key in the query string, covered by the signature as SDK
	// presigners hoist other x-amz-* headers.
	inQuery := "/vault/secret?" + headerSSECAlgo + "=AES256&" + headerSSECKey + "=" + urlEscape(mine[headerSSECKey]) +
		"&" + headerSSECKeyMD5 + "=" + urlEscape(mine[headerSSECKeyMD5])
	r := s.do(call{method: http.MethodGet, path: s.presign(http.MethodGet, inQuery, serverNow, 600, nil), unsigned: true})
	if code := s.errorCode(r); r.status != http.StatusBadRequest || code != "AuthorizationQueryParametersError" {
		t.Errorf("a customer key in a presigned query = %d %s", r.status, code)
	}
}

func urlEscape(s string) string {
	return strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(s)
}

// FuzzSSEHeaders: whatever the seven headers hold, reading them never
// panics; what comes out is a request the domain accepts; and a customer key
// comes out only of a request over TLS whose digest matches it.
func FuzzSSEHeaders(f *testing.F) {
	key := bytes.Repeat([]byte{1}, 32)
	mine := customerHeaders(key)
	f.Add("", "", "", "", "AES256", mine[headerSSECKey], mine[headerSSECKeyMD5], true)
	f.Add("aws:kms", "test:1", base64.StdEncoding.EncodeToString([]byte(`{"a":"b"}`)), "false", "", "", "", true)
	f.Add("AES256", "", "", "", "AES256", mine[headerSSECKey], mine[headerSSECKeyMD5], false)
	f.Add("aws:kms", "", "bnVsbA==", "true", "", "", "", true)
	f.Fuzz(func(t *testing.T, mode, kmsKey, context, bucketKey, algorithm, rawKey, digest string, secure bool) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://example.test/b/k", nil)
		if err != nil {
			return
		}
		if secure {
			r.TLS = &tls.ConnectionState{}
		}
		for name, value := range map[string]string{
			headerSSE: mode, headerSSEKMSKey: kmsKey, headerSSEContext: context, headerSSEBucketKey: bucketKey,
			headerSSECAlgo: algorithm, headerSSECKey: rawKey, headerSSECKeyMD5: digest,
		} {
			if value != "" {
				r.Header[http.CanonicalHeaderKey(name)] = []string{value}
			}
		}
		sse, err := sseOf(r)
		if err != nil {
			if len(sse.CustomerKey) != 0 {
				t.Fatal("a refused request returned a key")
			}
			return
		}
		if err := sse.Validate(); err != nil {
			t.Fatalf("sseOf accepted what the domain refuses: %v", err)
		}
		if sse.Mode == object.SSEC {
			sum := newETagHash()
			sum.Write(sse.CustomerKey)
			if !secure || base64.StdEncoding.EncodeToString(sum.Sum(nil)) != digest || algorithm != "AES256" || mode != "" {
				t.Fatalf("a customer key was accepted: secure=%v algorithm=%q mode=%q", secure, algorithm, mode)
			}
		} else if rawKey != "" || algorithm != "" || digest != "" {
			t.Fatal("customer key headers were there and were ignored")
		}
	})
}
