package s3api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
			if payload != example.Headers["x-amz-content-sha256"] {
				t.Errorf("payload hash = %q", payload)
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
