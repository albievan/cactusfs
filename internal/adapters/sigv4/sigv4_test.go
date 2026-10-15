package sigv4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	raw, err := os.ReadFile("testdata/sigv4-examples.json")
	if err != nil {
		t.Fatalf("read test vectors: %v", err)
	}
	var v docVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse test vectors: %v", err)
	}
	return v
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
	return r
}

// TestExpectedAgainstTheDocumentedExamples: the canonical request, the
// signing key and the signature give what AWS computed.
func TestExpectedAgainstTheDocumentedExamples(t *testing.T) {
	t.Parallel()
	vectors := loadDocVectors(t)
	for _, example := range vectors.Examples {
		t.Run(example.Name, func(t *testing.T) {
			t.Parallel()
			r := vectors.request(t, example)
			canonical := CanonicalRequest(r, strings.Split(example.SignedHeaders, ";"), example.SignedHeaders, example.Headers["x-amz-content-sha256"])
			got := Expected([]byte(vectors.SecretAccessKey), "20130524", "us-east-1", vectors.AmzDate, canonical)
			if got != example.Signature {
				t.Errorf("signature = %s, want %s", got, example.Signature)
			}
			if Scope("20130524", "us-east-1") != vectors.Scope {
				t.Errorf("scope = %s, want %s", Scope("20130524", "us-east-1"), vectors.Scope)
			}
		})
	}
}

// TestSignRequestAgainstTheDocumentedExamples: of the examples that sign
// what SignRequest signs - the host, the payload declaration and the date -
// SignRequest writes the signature AWS computed.
func TestSignRequestAgainstTheDocumentedExamples(t *testing.T) {
	t.Parallel()
	vectors := loadDocVectors(t)
	tried := 0
	for _, example := range vectors.Examples {
		if example.SignedHeaders != "host;x-amz-content-sha256;x-amz-date" {
			continue
		}
		tried++
		r := httptest.NewRequestWithContext(t.Context(), example.Method, "http://"+vectors.Host+example.Target, nil)
		r.Host = ""
		if err := SignRequest(r, vectors.AccessKeyID, []byte(vectors.SecretAccessKey), "us-east-1", EmptyPayloadSHA256, docTime()); err != nil {
			t.Fatalf("%s: SignRequest: %v", example.Name, err)
		}
		want := Algorithm + " Credential=" + vectors.AccessKeyID + "/" + vectors.Scope +
			", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + example.Signature
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("%s: Authorization = %q, want %q", example.Name, got, want)
		}
		if r.Host != vectors.Host || r.Header.Get(HeaderAmzDate) != vectors.AmzDate || r.Header.Get(HeaderContentSHA256) != EmptyPayloadSHA256 {
			t.Errorf("%s: host %q, headers %v", example.Name, r.Host, r.Header)
		}
	}
	if tried != 2 {
		t.Fatalf("%d examples tried, want 2", tried)
	}
}

func TestSignRequestSignsWhatAServerRequires(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "https://replica.example:9000/b/k?tagging", nil)
	r.Host = ""
	r.URL.Path = ""
	r.Header.Set("X-Amz-Meta-Owner", "  a   b ")
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Content-Md5", "AAAAAAAAAAAAAAAAAAAAAA==")
	r.Header.Set("User-Agent", "cactusfs")
	r.Header.Set("Authorization", "Bearer a token of before")
	if err := SignRequest(r, loadDocVectors(t).AccessKeyID, []byte("secret"), "us-east-1", UnsignedPayload, docTime()); err != nil {
		t.Fatal(err)
	}
	got := r.Header.Get("Authorization")
	if !strings.Contains(got, "SignedHeaders=content-md5;content-type;host;x-amz-content-sha256;x-amz-date;x-amz-meta-owner,") {
		t.Errorf("Authorization = %q", got)
	}
	if r.Host != "replica.example:9000" || r.URL.Path != "/" {
		t.Errorf("host %q, path %q", r.Host, r.URL.Path)
	}
	if len(r.Header.Values("Authorization")) != 1 {
		t.Errorf("Authorization = %v", r.Header.Values("Authorization"))
	}
}

// TestSignRequestSendsThePathItSigns: the path on the wire is the path of
// the canonical request, byte for byte, sub-delimiters included.
func TestSignRequestSendsThePathItSigns(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/b/!$&'()*+,;=:@", "/b/a b/ünï", "/b/%2F", "/b/a+b", "/"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://replica.example/", nil)
		r.URL.Path = path
		if err := SignRequest(r, loadDocVectors(t).AccessKeyID, []byte("secret"), "us-east-1", UnsignedPayload, docTime()); err != nil {
			t.Fatal(err)
		}
		if got, want := r.URL.EscapedPath(), URIEncode(path, false); got != want {
			t.Errorf("%q is sent as %q, signed as %q", path, got, want)
		}
		if got := r.URL.RequestURI(); !strings.HasPrefix(got, URIEncode(path, false)) {
			t.Errorf("%q: request line %q", path, got)
		}
	}
}

func TestSignRequestRefuses(t *testing.T) {
	t.Parallel()
	const secret = "the secret of the key"
	keyID := loadDocVectors(t).AccessKeyID
	tests := []struct {
		name   string
		change func(r *http.Request)
		keyID  string
		secret string
		region string
		hash   string
	}{
		{name: "no access key ID", keyID: "-"},
		{name: "an access key ID with a slash", keyID: "key/X"},
		{name: "an access key ID with a comma", keyID: "key,X"},
		{name: "no secret", secret: "-"},
		{name: "no region", region: "-"},
		{name: "a region with a space", region: "us east"},
		{name: "a payload declaration with an equals sign", hash: "a=b"},
		{name: "no host", change: func(r *http.Request) { r.Host, r.URL.Host = "", "" }},
		{name: "an opaque URL", change: func(r *http.Request) { r.URL.Opaque = "x:y" }},
		{name: "a header not in canonical form", change: func(r *http.Request) { r.Header["x-amz-meta-a"] = []string{"1"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://h.example/b/k", nil)
			if tt.change != nil {
				tt.change(r)
			}
			pick := func(given, otherwise string) string {
				switch given {
				case "":
					return otherwise
				case "-":
					return ""
				}
				return given
			}
			err := SignRequest(r, pick(tt.keyID, keyID), []byte(pick(tt.secret, secret)),
				pick(tt.region, "us-east-1"), pick(tt.hash, UnsignedPayload), docTime())
			if !errors.Is(err, errSign) {
				t.Fatalf("SignRequest = %v, want %v", err, errSign)
			}
			if strings.Contains(err.Error(), secret) || r.Header.Get("Authorization") != "" {
				t.Errorf("error %q, Authorization %q", err, r.Header.Get("Authorization"))
			}
		})
	}
	if err := SignRequest(nil, keyID, []byte(secret), "us-east-1", UnsignedPayload, docTime()); !errors.Is(err, errSign) {
		t.Errorf("no request: %v", err)
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
		if got := URIEncode(tt.in, tt.encodeSlash); got != tt.want {
			t.Errorf("URIEncode(%q, %v) = %q, want %q", tt.in, tt.encodeSlash, got, tt.want)
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
		"prefix=a/b":                 "prefix=a%2Fb",
		"prefix=a%20b&delimiter=%2f": "delimiter=%2F&prefix=a%20b",
		"k=a+b":                      "k=a%2Bb",
		"&&a=1&":                     "a=1",
		"k=%zz":                      "k=%25zz",
	}
	for raw, want := range tests {
		if got := CanonicalQuery(raw); got != want {
			t.Errorf("CanonicalQuery(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The chunked upload of the public AWS documentation ("Signature
// Calculations for the Authorization Header: Transferring Payload in
// Multiple Chunks"): 66560 bytes of 'a' in chunks of 64 KiB, signed in a
// chain from the seed signature of the request.
const (
	docChunkSeed  = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	docChunkOne   = "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648"
	docChunkTwo   = "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"
	docChunkFinal = "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func docChain(t *testing.T) Chain {
	t.Helper()
	vectors := loadDocVectors(t)
	return Chain{
		Key:       SigningKey([]byte(vectors.SecretAccessKey), "20130524", "us-east-1"),
		Timestamp: vectors.AmzDate, Scope: vectors.Scope, Previous: docChunkSeed,
	}
}

// TestChainAgainstTheDocumentedExample: the chain signs the documented
// chunks as AWS did, verifies them, and refuses them out of order.
func TestChainAgainstTheDocumentedExample(t *testing.T) {
	t.Parallel()
	chunks := []string{
		digestOf(bytes.Repeat([]byte("a"), 65536)), digestOf(bytes.Repeat([]byte("a"), 1024)), digestOf(nil),
	}
	want := []string{docChunkOne, docChunkTwo, docChunkFinal}

	signer := docChain(t)
	for i, digest := range chunks {
		if got := signer.Next(ChunkAlgorithm, digest); got != want[i] {
			t.Fatalf("chunk %d: %s, want %s", i, got, want[i])
		}
	}

	verifier := docChain(t)
	for i, digest := range chunks {
		if !verifier.Verify(ChunkAlgorithm, want[i], digest) {
			t.Fatalf("chunk %d does not verify", i)
		}
	}

	swapped := docChain(t)
	if swapped.Verify(ChunkAlgorithm, docChunkTwo, chunks[0]) {
		t.Error("the second signature verified the first chunk")
	}
	if swapped.Previous != docChunkSeed {
		t.Error("a link that failed moved the chain on")
	}
	if swapped.Verify(TrailerAlgorithm, docChunkOne, chunks[0]) {
		t.Error("a chunk signature verified as the trailers")
	}
	if !swapped.Verify(ChunkAlgorithm, docChunkOne, chunks[0]) {
		t.Error("the first chunk does not verify after a failed attempt")
	}
}

// FuzzChain: nothing but the signature Next makes verifies a link, and no
// declared signature the fuzzer finds is one.
func FuzzChain(f *testing.F) {
	f.Add([]byte("hello"), docChunkOne, false)
	f.Add([]byte{}, docChunkFinal, true)
	f.Fuzz(func(t *testing.T, data []byte, declared string, trailer bool) {
		algorithm := ChunkAlgorithm
		if trailer {
			algorithm = TrailerAlgorithm
		}
		chain := Chain{Key: []byte("k"), Timestamp: "20130524T000000Z", Scope: "s", Previous: docChunkSeed}
		signer := chain
		want := signer.Next(algorithm, digestOf(data))
		if chain.Verify(algorithm, declared, digestOf(data)) != (declared == want) {
			t.Fatalf("Verify(%q) disagrees with Next (%q)", declared, want)
		}
	})
}

// FuzzSignRequest: whatever request it is given, SignRequest refuses it or
// signs it with a signature that is the one a verifier computes from what it
// signed, and never puts the secret in an error.
func FuzzSignRequest(f *testing.F) {
	f.Add("GET", "/b/k", "prefix=a%2Fb&x=1", "v", "us-east-1")
	f.Add("PUT", "", "%zz", "  a\tb ", "eu-west-1")
	keyID := loadDocVectors(f).AccessKeyID
	f.Fuzz(func(t *testing.T, method, path, query, value, region string) {
		const secret = "a secret nobody prints"
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://s3.example/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Method, r.URL.Path, r.URL.RawQuery = method, path, query
		r.Header["X-Amz-Meta-Fuzz"] = []string{value}
		if err := SignRequest(r, keyID, []byte(secret), region, UnsignedPayload, docTime()); err != nil {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the secret is in %q", err)
			}
			return
		}
		authorization := r.Header.Get("Authorization")
		_, declared, found := strings.Cut(authorization, ", Signature=")
		signed := []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-meta-fuzz"}
		raw := strings.Join(signed, ";")
		want := Expected([]byte(secret), "20130524", region, docTime().Format(AmzDateFormat), CanonicalRequest(r, signed, raw, UnsignedPayload))
		if !found || declared != want || !strings.Contains(authorization, "SignedHeaders="+raw+",") {
			t.Fatalf("Authorization %q, want the signature %s", authorization, want)
		}
	})
}
