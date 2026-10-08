package s3api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// docVectors are the worked examples of the AWS documentation
// "Authenticating Requests: Using the Authorization Header (AWS Signature
// Version 4)", kept as test data in testdata/sigv4-examples.json. They are an
// independent check: the signatures were computed by AWS, not by this code.
type docVectors struct {
	AccessKeyID     string       `json:"access_key_id"`
	SecretAccessKey string       `json:"secret_access_key"`
	Scope           string       `json:"scope"`
	Host            string       `json:"host"`
	AmzDate         string       `json:"amz_date"`
	Examples        []docExample `json:"examples"`
}

type docExample struct {
	Name          string            `json:"name"`
	Method        string            `json:"method"`
	Target        string            `json:"target"`
	Headers       map[string]string `json:"headers"`
	SignedHeaders string            `json:"signed_headers"`
	Signature     string            `json:"signature"`
}

func loadDocVectors(t testing.TB) docVectors {
	t.Helper()
	root, err := os.OpenRoot("testdata")
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("sigv4-examples.json")
	if err != nil {
		t.Fatalf("read test vectors: %v", err)
	}
	var v docVectors
	if err := json.Unmarshal(data, &v); err != nil || len(v.Examples) == 0 {
		t.Fatalf("parse test vectors: %v", err)
	}
	return v
}

func (v docVectors) key() []byte { return []byte(v.SecretAccessKey) }

func (v docVectors) authorization(e docExample) string {
	return sigV4Algorithm + " Credential=" + v.AccessKeyID + "/" + v.Scope +
		",SignedHeaders=" + e.SignedHeaders + ",Signature=" + e.Signature
}

func docTime() time.Time {
	return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
}

func (v docVectors) request(t *testing.T, e docExample) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), e.Method, "http://"+v.Host+e.Target, nil)
	for name, value := range e.Headers {
		r.Header.Set(name, value)
	}
	r.Header.Set("Authorization", v.authorization(e))
	return r
}

func TestSigV4AgainstTheDocumentedExamples(t *testing.T) {
	t.Parallel()

	vectors := loadDocVectors(t)
	for _, example := range vectors.Examples {
		t.Run(example.Name, func(t *testing.T) {
			t.Parallel()
			r := vectors.request(t, example)
			sig, err := parseAuthorization(r.Header.Get("Authorization"))
			if err != nil {
				t.Fatalf("parseAuthorization: %v", err)
			}
			if sig.accessKeyID != vectors.AccessKeyID || sig.region != "us-east-1" || sig.scopeDate != "20130524" {
				t.Errorf("parsed signature = %+v", sig)
			}
			payload, err := verifySignature(r, sig, vectors.key(), docTime())
			if err != nil {
				t.Fatalf("verifySignature: %v", err)
			}
			if payload.payloadHash != example.Headers["x-amz-content-sha256"] || payload.chunks != nil {
				t.Errorf("verified = %+v", payload)
			}
		})
	}
}

// TestSigV4RejectsAnyChange: a valid signature stops verifying as soon as
// anything it covers, or the key, changes.
func TestSigV4RejectsAnyChange(t *testing.T) {
	t.Parallel()
	vectors := loadDocVectors(t)
	example := vectors.Examples[0]
	wrongKey := append(vectors.key()[:len(vectors.key())-1], 'Z')

	tests := []struct {
		name   string
		change func(r *http.Request)
		key    []byte
		now    time.Time
		want   error
	}{
		{name: "another key", key: wrongKey, want: errAuthSignature},
		{name: "empty key", key: []byte{}, want: errAuthSignature},
		{name: "another path", change: func(r *http.Request) { r.URL.Path = "/other.txt" }, want: errAuthSignature},
		{name: "another method", change: func(r *http.Request) { r.Method = http.MethodDelete }, want: errAuthSignature},
		{name: "added query", change: func(r *http.Request) { r.URL.RawQuery = "acl" }, want: errAuthSignature},
		{name: "another host", change: func(r *http.Request) { r.Host = "evil.example.com" }, want: errAuthSignature},
		{name: "changed signed header", change: func(r *http.Request) { r.Header.Set("Range", "bytes=0-99") }, want: errAuthSignature},
		{name: "changed payload hash", change: func(r *http.Request) { r.Header.Set(headerContentSHA, payloadUnsigned) }, want: errAuthSignature},
		{name: "removed signed header", change: func(r *http.Request) { r.Header.Del("Range") }, want: errAuthMalformed},
		{name: "added unsigned x-amz header", change: func(r *http.Request) { r.Header.Set("X-Amz-Meta-Owner", "mallory") }, want: errAuthMalformed},
		{name: "missing payload hash", change: func(r *http.Request) {
			r.Header.Del(headerContentSHA)
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "x-amz-content-sha256;", "", 1))
		}, want: errAuthMissingContent},
		{name: "date moved to another day", change: func(r *http.Request) { r.Header.Set(headerAmzDate, "20130525T000000Z") }, want: errAuthMalformed},
		{name: "malformed date", change: func(r *http.Request) { r.Header.Set(headerAmzDate, "yesterday") }, want: errAuthMalformed},
		{name: "replayed 16 minutes later", now: docTime().Add(16 * time.Minute), want: errAuthSkew},
		{name: "signed 16 minutes in the future", now: docTime().Add(-16 * time.Minute), want: errAuthSkew},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := vectors.request(t, example)
			if tt.change != nil {
				tt.change(r)
			}
			key, now := vectors.key(), docTime()
			if tt.key != nil {
				key = tt.key
			}
			if !tt.now.IsZero() {
				now = tt.now
			}
			sig, err := parseAuthorization(r.Header.Get("Authorization"))
			if err != nil {
				t.Fatalf("parseAuthorization: %v", err)
			}
			if _, err := verifySignature(r, sig, key, now); !errors.Is(err, tt.want) {
				t.Errorf("verifySignature error = %v, want %v", err, tt.want)
			}
		})
	}

	t.Run("within the allowed skew", func(t *testing.T) {
		t.Parallel()
		r := vectors.request(t, example)
		sig, _ := parseAuthorization(r.Header.Get("Authorization"))
		for _, offset := range []time.Duration{14 * time.Minute, -14 * time.Minute} {
			if _, err := verifySignature(r, sig, vectors.key(), docTime().Add(offset)); err != nil {
				t.Errorf("offset %s: %v", offset, err)
			}
		}
	})
}

func TestSigV4WithDateHeaderOnly(t *testing.T) {
	t.Parallel()

	vectors := loadDocVectors(t)
	docKey := vectors.key
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+vectors.Host+"/k", nil)
	r.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	r.Header.Set(headerContentSHA, emptyPayloadSHA256)
	sig := signature{
		accessKeyID: vectors.AccessKeyID, scopeDate: "20130524", region: "us-east-1",
		signedHeaders: []string{"date", "host", "x-amz-content-sha256"}, signedHeadersRaw: "date;host;x-amz-content-sha256",
	}
	sig.signatureHex = expectedSignature(docKey(), sig, vectors.AmzDate, canonicalRequest(r, sig, emptyPayloadSHA256))

	if _, err := verifySignature(r, sig, docKey(), docTime()); err != nil {
		t.Errorf("verifySignature with a Date header: %v", err)
	}
	r.Header.Del("Date")
	if _, err := verifySignature(r, sig, docKey(), docTime()); !errors.Is(err, errAuthMalformed) {
		t.Errorf("without any date: error = %v, want %v", err, errAuthMalformed)
	}
	r.Header.Set("Date", "not a date")
	if _, err := verifySignature(r, sig, docKey(), docTime()); !errors.Is(err, errAuthMalformed) {
		t.Errorf("with a malformed Date: error = %v, want %v", err, errAuthMalformed)
	}
}

func TestParseAuthorization(t *testing.T) {
	t.Parallel()

	const (
		docKeyID = "EXAMPLEKEYID00000000"
		docScope = "20130524/us-east-1/s3/aws4_request"
		good     = "Credential=" + docKeyID + "/" + docScope + ", SignedHeaders=host;x-amz-date, Signature=" +
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	)
	if _, err := parseAuthorization(sigV4Algorithm + " " + good); err != nil {
		t.Fatalf("valid header with spaces after commas: %v", err)
	}

	malformed := map[string]string{
		"no parameters":           sigV4Algorithm,
		"missing signature":       sigV4Algorithm + " Credential=" + docKeyID + "/" + docScope + ",SignedHeaders=host",
		"missing credential":      sigV4Algorithm + " SignedHeaders=host,Signature=" + strings.Repeat("a", 64),
		"duplicate parameter":     sigV4Algorithm + " " + good + ",Signature=" + strings.Repeat("a", 64),
		"unknown parameter":       sigV4Algorithm + " " + good + ",Extra=1",
		"empty value":             sigV4Algorithm + " " + strings.Replace(good, "host;x-amz-date", "", 1),
		"short scope":             sigV4Algorithm + " " + strings.Replace(good, "/us-east-1", "", 1),
		"another service":         sigV4Algorithm + " " + strings.Replace(good, "/s3/", "/ec2/", 1),
		"wrong terminator":        sigV4Algorithm + " " + strings.Replace(good, "aws4_request", "aws4", 1),
		"bad scope date":          sigV4Algorithm + " " + strings.Replace(good, "20130524", "2013-05-24", 1),
		"empty region":            sigV4Algorithm + " " + strings.Replace(good, "us-east-1", "", 1),
		"bad access key ID":       sigV4Algorithm + " " + strings.Replace(good, docKeyID, "bad key!", 1),
		"unsorted signed headers": sigV4Algorithm + " " + strings.Replace(good, "host;x-amz-date", "x-amz-date;host", 1),
		"uppercase signed header": sigV4Algorithm + " " + strings.Replace(good, "host;x-amz-date", "Host;x-amz-date", 1),
		"empty signed header":     sigV4Algorithm + " " + strings.Replace(good, "host;x-amz-date", "host;;x-amz-date", 1),
		"short signature":         sigV4Algorithm + " " + good[:len(good)-2],
		"non-hex signature":       sigV4Algorithm + " " + good[:len(good)-2] + "zz",
		"too long":                sigV4Algorithm + " " + good + strings.Repeat(" ", maxAuthorizationHeader),
	}
	for name, header := range malformed {
		if _, err := parseAuthorization(header); !errors.Is(err, errAuthMalformed) {
			t.Errorf("%s: error = %v, want %v", name, err, errAuthMalformed)
		}
	}

	for name, header := range map[string]string{
		"signature version 2": "AWS " + docKeyID + ":frJIUN8DYpKDtOLCwo//yllqDzg=",
		"basic":               "Basic dXNlcjpwYXNz",
		"empty":               "",
	} {
		if _, err := parseAuthorization(header); !errors.Is(err, errAuthUnsupported) {
			t.Errorf("%s: error = %v, want %v", name, err, errAuthUnsupported)
		}
	}
}

func TestURIEncode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"/test.txt", false, "/test.txt"},
		{"/photos/2026/a b.jpg", false, "/photos/2026/a%20b.jpg"},
		{"/a+b", false, "/a%2Bb"},
		{"/key$with*chars", false, "/key%24with%2Achars"},
		{"/ünï", false, "/%C3%BCn%C3%AF"},
		{"a/b", true, "a%2Fb"},
		{"AZaz09-_.~", true, "AZaz09-_.~"},
		{"", false, ""},
	}
	for _, tt := range tests {
		if got := uriEncode(tt.in, tt.encodeSlash); got != tt.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", tt.in, tt.encodeSlash, got, tt.want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"":                           "",
		"lifecycle":                  "lifecycle=",
		"prefix=J&max-keys=2":        "max-keys=2&prefix=J",
		"b=2&a=1&a=0":                "a=0&a=1&b=2",
		"prefix=a%2Fb&x-id=GetObj":   "prefix=a%2Fb&x-id=GetObj",
		"prefix=a/b":                 "prefix=a%2Fb",
		"prefix=a%20b&delimiter=%2f": "delimiter=%2F&prefix=a%20b",
		"k=a+b":                      "k=a%2Bb",
		"&&a=1&":                     "a=1",
		"k=%zz":                      "k=%25zz",
	}
	for raw, want := range tests {
		if got := canonicalQuery(raw); got != want {
			t.Errorf("canonicalQuery(%q) = %q, want %q", raw, got, want)
		}
	}
}

// FuzzSigV4 feeds arbitrary Authorization headers, paths and queries through
// parsing and verification: nothing may panic, and nothing may verify without
// the key.
func FuzzSigV4(f *testing.F) {
	vectors := loadDocVectors(f)
	for _, example := range vectors.Examples {
		f.Add(vectors.authorization(example), example.Target, "")
	}
	f.Add("AWS4-HMAC-SHA256 Credential=a/b/c/s3/aws4_request,SignedHeaders=host,Signature=00", "/%zz", "a=%")
	f.Add("", "/", "&&=&")

	f.Fuzz(func(t *testing.T, authorization, path, query string) {
		sig, err := parseAuthorization(authorization)
		if err != nil {
			if !errors.Is(err, errAuthMalformed) && !errors.Is(err, errAuthUnsupported) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+vectors.Host+"/", nil)
		r.URL.Path, r.URL.RawQuery = path, query
		r.Header.Set(headerAmzDate, vectors.AmzDate)
		r.Header.Set(headerContentSHA, emptyPayloadSHA256)
		_ = canonicalRequest(r, sig, emptyPayloadSHA256)

		// A signature that verifies under a key the fuzzer never saw would
		// be a forgery.
		if _, err := verifySignature(r, sig, []byte("a key the fuzzer does not know"), docTime()); err == nil {
			t.Fatalf("a fuzzed header verified: %q", authorization)
		}
	})
}

// The presigned URL of the public AWS documentation ("Authenticating
// Requests: Using Query Parameters"): GET test.txt in examplebucket, valid
// for a day from 2013-05-24.
const (
	docPresignedHost  = "examplebucket.s3.amazonaws.com"
	docPresignedQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host"
	docPresignedSignature = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
)

func presignedRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+docPresignedHost+"/test.txt?"+query, nil)
}

func TestPresignedAgainstTheDocumentedExample(t *testing.T) {
	t.Parallel()
	vectors := loadDocVectors(t)
	good := docPresignedQuery + "&X-Amz-Signature=" + docPresignedSignature
	verify := func(t *testing.T, query string, now time.Time, key []byte) error {
		t.Helper()
		r := presignedRequest(t, query)
		p, err := parsePresigned(r.URL.Query())
		if err != nil {
			return err
		}
		_, err = verifyPresigned(r, p, key, now)
		return err
	}
	if err := verify(t, good, docTime().Add(time.Hour), vectors.key()); err != nil {
		t.Fatalf("the documented presigned URL does not verify: %v", err)
	}
	wrongKey := append(vectors.key()[:len(vectors.key())-1], 'Z')
	tests := []struct {
		name  string
		query string
		now   time.Time
		key   []byte
		want  error
	}{
		{"at the moment of signing", good, docTime(), vectors.key(), nil},
		{"in its last second", good, docTime().Add(24 * time.Hour), vectors.key(), nil},
		{"a second after it expired", good, docTime().Add(24*time.Hour + time.Second), vectors.key(), errAuthExpired},
		{"signed in the future", good, docTime().Add(-time.Hour), vectors.key(), errAuthSkew},
		{"another key", good, docTime(), wrongKey, errAuthSignature},
		{"a longer life than was signed", strings.Replace(good, "Expires=86400", "Expires=604800", 1), docTime(), vectors.key(), errAuthSignature},
		{"another object parameter added", good + "&versionId=1", docTime(), vectors.key(), errAuthSignature},
		{"a response header override added", good + "&response-content-type=text%2Fhtml", docTime(), vectors.key(), errAuthSignature},
		{"signature changed", docPresignedQuery + "&X-Amz-Signature=" + strings.Repeat("0", 64), docTime(), vectors.key(), errAuthSignature},
		{"no expiry", strings.Replace(good, "&X-Amz-Expires=86400", "", 1), docTime(), vectors.key(), errAuthQuery},
		{"expiry of zero", strings.Replace(good, "Expires=86400", "Expires=0", 1), docTime(), vectors.key(), errAuthQuery},
		{"expiry of more than a week", strings.Replace(good, "Expires=86400", "Expires=604801", 1), docTime(), vectors.key(), errAuthQuery},
		{"negative expiry", strings.Replace(good, "Expires=86400", "Expires=-1", 1), docTime(), vectors.key(), errAuthQuery},
		{"expiry given twice", good + "&X-Amz-Expires=604800", docTime(), vectors.key(), errAuthQuery},
		{"signature given twice", good + "&X-Amz-Signature=" + docPresignedSignature, docTime(), vectors.key(), errAuthQuery},
		{"bad date", strings.Replace(good, "20130524T000000Z", "2013-05-24", 1), docTime(), vectors.key(), errAuthQuery},
		{"date of another day than the scope", strings.Replace(good, "Date=20130524T", "Date=20130525T", 1), docTime().Add(24 * time.Hour), vectors.key(), errAuthQuery},
		{"another algorithm", strings.Replace(good, "AWS4-HMAC-SHA256", "AWS4-ECDSA-P256-SHA256", 1), docTime(), vectors.key(), errAuthUnsupported},
		{"host not signed", strings.Replace(good, "SignedHeaders=host", "SignedHeaders=range", 1), docTime(), vectors.key(), errAuthMalformed},
		{"a comma in the credential", strings.Replace(good, "AKIAIOSFODNN7EXAMPLE", "AKIA%2CSignature%3D00", 1), docTime(), vectors.key(), errAuthQuery},
	}
	for _, tt := range tests {
		if err := verify(t, tt.query, tt.now, tt.key); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}
	// A header the signer did not sign cannot ride along.
	r := presignedRequest(t, good)
	r.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
	p, err := parsePresigned(r.URL.Query())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyPresigned(r, p, vectors.key(), docTime()); !errors.Is(err, errAuthMalformed) {
		t.Errorf("an unsigned x-amz header on a presigned request = %v", err)
	}
	// Nor can a presigned request claim signed chunks.
	r = presignedRequest(t, good)
	r.Header.Set(headerContentSHA, payloadStreamingSigned)
	if _, err := verifyPresigned(r, p, vectors.key(), docTime()); err == nil {
		t.Error("a presigned request with a streaming payload verified")
	}
	if got := withoutParameter("a=1&X-Amz-Signature=x&b=%20&X-Amz-Signature=y", querySignature); got != "a=1&b=%20" {
		t.Errorf("withoutParameter = %q", got)
	}
}

// The chunked upload of the public AWS documentation ("Signature
// Calculations for the Authorization Header: Transferring Payload in
// Multiple Chunks"): 66560 bytes of 'a' in chunks of 64 KiB.
const (
	docChunkSeed  = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	docChunkOne   = "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648"
	docChunkTwo   = "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"
	docChunkFinal = "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"
)

func docChunkedRequest(t *testing.T, body io.Reader) (*http.Request, signature) {
	t.Helper()
	vectors := loadDocVectors(t)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://s3.amazonaws.com/examplebucket/chunkObject.txt", body)
	r.ContentLength = 66824
	for name, value := range map[string]string{
		"x-amz-date": "20130524T000000Z", "x-amz-storage-class": "REDUCED_REDUNDANCY", "Content-Encoding": "aws-chunked",
		"x-amz-decoded-content-length": "66560", "x-amz-content-sha256": payloadStreamingSigned,
	} {
		r.Header.Set(name, value)
	}
	sig, err := parseAuthorization(sigV4Algorithm + " Credential=" + vectors.AccessKeyID + "/" + vectors.Scope +
		",SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class" +
		",Signature=" + docChunkSeed)
	if err != nil {
		t.Fatal(err)
	}
	return r, sig
}

func docChunkedBody(signatures [3]string) []byte {
	var b bytes.Buffer
	b.WriteString("10000;chunk-signature=" + signatures[0] + "\r\n")
	b.Write(bytes.Repeat([]byte("a"), 65536))
	b.WriteString("\r\n400;chunk-signature=" + signatures[1] + "\r\n")
	b.Write(bytes.Repeat([]byte("a"), 1024))
	b.WriteString("\r\n0;chunk-signature=" + signatures[2] + "\r\n\r\n")
	return b.Bytes()
}

func TestSignedChunksAgainstTheDocumentedExample(t *testing.T) {
	t.Parallel()
	vectors := loadDocVectors(t)
	read := func(t *testing.T, body []byte) ([]byte, error) {
		t.Helper()
		r, sig := docChunkedRequest(t, bytes.NewReader(body))
		checked, err := verifySignature(r, sig, vectors.key(), docTime())
		if err != nil {
			t.Fatalf("the documented seed signature does not verify: %v", err)
		}
		if checked.chunks == nil {
			t.Fatal("no chunk signer for a signed streaming payload")
		}
		p, err := newPayload(r, checked.payloadHash, checked.chunks)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(p.body)
		if err != nil {
			return data, err
		}
		_, err = p.verify()
		return data, err
	}
	good := [3]string{docChunkOne, docChunkTwo, docChunkFinal}
	data, err := read(t, docChunkedBody(good))
	if err != nil || len(data) != 66560 || bytes.Count(data, []byte("a")) != 66560 {
		t.Fatalf("the documented chunked body: %d bytes, %v", len(data), err)
	}
	if len(docChunkedBody(good)) != 66824 {
		t.Fatalf("the body is %d bytes, the documentation says 66824", len(docChunkedBody(good)))
	}

	flip := func(body []byte, at int) []byte {
		out := bytes.Clone(body)
		out[at] ^= 1
		return out
	}
	firstData := len("10000;chunk-signature=") + 64 + 2
	tests := map[string][]byte{
		"a byte of the first chunk changed":  flip(docChunkedBody(good), firstData+100),
		"a byte of the second chunk changed": flip(docChunkedBody(good), firstData+65536+2+len("400;chunk-signature=")+64+2+7),
		"chunks swapped in the chain":        docChunkedBody([3]string{docChunkTwo, docChunkOne, docChunkFinal}),
		"final signature of another stream":  docChunkedBody([3]string{docChunkOne, docChunkTwo, docChunkOne}),
		"a chunk without a signature":        bytes.Replace(docChunkedBody(good), []byte(";chunk-signature="+docChunkTwo), nil, 1),
		"a short signature":                  bytes.Replace(docChunkedBody(good), []byte(docChunkTwo), []byte(docChunkTwo[:60]), 1),
		"a trailer nobody announced":         bytes.Replace(docChunkedBody(good), []byte(docChunkFinal+"\r\n"), []byte(docChunkFinal+"\r\nx-amz-checksum-crc32:AAAAAA==\r\n"), 1),
		"cut off before the last chunk":      docChunkedBody(good)[:firstData+65536+2+len("400;chunk-signature=")+64+2+1024+2],
	}
	for name, body := range tests {
		data, err := read(t, body)
		if err == nil {
			t.Errorf("%s: read %d bytes without an error", name, len(data))
			continue
		}
		if name != "cut off before the last chunk" && name != "a trailer nobody announced" && !errors.Is(err, errAuthSignature) {
			t.Errorf("%s: %v, want %v", name, err, errAuthSignature)
		}
	}
	// A truncated stream: the last data chunk is dropped and the final one
	// kept. The chain does not close.
	truncated := append([]byte("10000;chunk-signature="+docChunkOne+"\r\n"), bytes.Repeat([]byte("a"), 65536)...)
	truncated = append(truncated, []byte("\r\n0;chunk-signature="+docChunkFinal+"\r\n\r\n")...)
	if _, err := read(t, truncated); !errors.Is(err, errAuthSignature) {
		t.Errorf("a stream with a chunk removed: %v, want %v", err, errAuthSignature)
	}
}

// TestSignedTrailers: the trailers of a signed stream are signed too, in
// the same chain.
func TestSignedTrailers(t *testing.T) {
	t.Parallel()
	key, timestamp, scope := []byte("signing key of the day"), "20261008T090000Z", "20261008/us-east-1/s3/aws4_request"
	seed := strings.Repeat("ab", 32)
	content := []byte("hello, signed trailers")
	sum := crc32.ChecksumIEEE(content)
	checksum := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))

	build := func(trailerValue string, signTrailer bool) []byte {
		signer := &chunkSigner{key: key, timestamp: timestamp, scope: scope, previous: seed}
		link := func(algorithm, digest string) string {
			text := algorithm + "\n" + timestamp + "\n" + scope + "\n" + signer.previous + "\n"
			if algorithm == chunkAlgorithm {
				text += emptyPayloadSHA256 + "\n"
			}
			signer.previous = hex.EncodeToString(hmacSHA256(key, text+digest))
			return signer.previous
		}
		digest := func(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
		var b bytes.Buffer
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n%s\r\n", len(content), link(chunkAlgorithm, digest(content)), content)
		fmt.Fprintf(&b, "0;chunk-signature=%s\r\n", link(chunkAlgorithm, digest(nil)))
		fmt.Fprintf(&b, "x-amz-checksum-crc32:%s\r\n", trailerValue)
		signature := link(trailerAlgorithm, digest([]byte("x-amz-checksum-crc32:"+checksum+"\n")))
		if signTrailer {
			fmt.Fprintf(&b, "x-amz-trailer-signature:%s\r\n", signature)
		}
		b.WriteString("\r\n")
		return b.Bytes()
	}
	read := func(body []byte) error {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://s3.example/b/k", bytes.NewReader(body))
		r.Header.Set(headerDecodedLen, strconv.Itoa(len(content)))
		r.Header.Set(headerTrailer, "x-amz-checksum-crc32")
		p, err := newPayload(r, payloadSignedTrailer, &chunkSigner{key: key, timestamp: timestamp, scope: scope, previous: seed})
		if err != nil {
			return err
		}
		if _, err := io.ReadAll(p.body); err != nil {
			return err
		}
		_, err = p.verify()
		return err
	}
	if err := read(build(checksum, true)); err != nil {
		t.Fatalf("a signed stream with signed trailers: %v", err)
	}
	other := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})
	if err := read(build(other, true)); !errors.Is(err, errAuthSignature) {
		t.Errorf("a trailer changed after signing: %v, want %v", err, errAuthSignature)
	}
	if err := read(build(checksum, false)); !errors.Is(err, errAuthSignature) {
		t.Errorf("trailers without their signature: %v, want %v", err, errAuthSignature)
	}
	// Without a decoded length a signed stream is not accepted.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://s3.example/b/k", nil)
	r.Header.Set(headerTrailer, "x-amz-checksum-crc32")
	if _, err := newPayload(r, payloadSignedTrailer, &chunkSigner{}); err == nil {
		t.Error("a signed stream without x-amz-decoded-content-length was accepted")
	}
	if _, err := newPayload(r, payloadStreamingSigned, nil); !errors.Is(err, errUnsupportedPayload) {
		t.Errorf("signed chunks on a request that has no signer: %v", err)
	}
}

func FuzzParsePresigned(f *testing.F) {
	f.Add(docPresignedQuery + "&X-Amz-Signature=" + docPresignedSignature)
	f.Add("X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=a,b&X-Amz-Date=x&X-Amz-Expires=9999999999999999999&X-Amz-SignedHeaders=;&X-Amz-Signature=")
	f.Add("X-Amz-Signature=%zz&X-Amz-Expires=1&X-Amz-Expires=2")
	f.Fuzz(func(t *testing.T, raw string) {
		query, err := url.ParseQuery(raw)
		if err != nil {
			return
		}
		p, err := parsePresigned(query)
		if err != nil {
			if !errors.Is(err, errAuthQuery) && !errors.Is(err, errAuthUnsupported) {
				t.Fatalf("unclassified error: %v", err)
			}
			return
		}
		if p.expires < time.Second || p.expires > maxPresignedExpiry || len(p.signatureHex) != signatureHexLength ||
			!slices.Contains(p.signedHeaders, p.signedHeaders[0]) || p.accessKeyID == "" {
			t.Fatalf("accepted %+v", p)
		}
		// Verification never panics on what parsing accepted.
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://h/k?"+query.Encode(), nil)
		_, _ = verifyPresigned(r, p, []byte("secret"), p.signedAt)
	})
}

func FuzzSignedChunks(f *testing.F) {
	good := docChunkedBody([3]string{docChunkOne, docChunkTwo, docChunkFinal})
	f.Add(good[:200], 66560)
	f.Add([]byte("0;chunk-signature="+docChunkFinal+"\r\n\r\n"), 0)
	f.Add([]byte("5;chunk-signature=zz\r\nhello\r\n0\r\n\r\n"), 5)
	f.Fuzz(func(t *testing.T, body []byte, decoded int) {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://h/b/k", bytes.NewReader(body))
		r.Header.Set(headerDecodedLen, strconv.Itoa(decoded))
		signer := &chunkSigner{key: []byte("k"), timestamp: "20130524T000000Z", scope: "s", previous: docChunkSeed}
		p, err := newPayload(r, payloadStreamingSigned, signer)
		if err != nil {
			return
		}
		data, err := io.ReadAll(io.LimitReader(p.body, 1<<20))
		if err != nil {
			return
		}
		// Nothing but a correctly signed chain is ever accepted, and no
		// input the fuzzer finds is one: it does not have the key.
		if _, err := p.verify(); err == nil && len(body) > 0 {
			t.Fatalf("a body signed by nobody verified: %d bytes out", len(data))
		}
	})
}
