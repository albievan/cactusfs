package s3api

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

const sitePolicy = `{"Version":"2012-10-17","Statement":[
	{"Sid":"public","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]},
	{"Sid":"drafts","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/drafts/*"},
	{"Sid":"editor","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/editor"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::site/*"}]}`

// TestBucketPolicyOverHTTP: a bucket policy opens a bucket to readers who
// have not authenticated, and to nothing more, whatever it says.
func TestBucketPolicyOverHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	s.createBucket("site")
	s.createBucket("private")
	s.putObject("/site/index.html", []byte("<h1>hello</h1>"))
	s.putObject("/site/drafts/new.html", []byte("draft"))
	s.putObject("/private/secret", []byte("secret"))
	anon := func(method, path string, body []byte) reply {
		return s.do(call{method: method, path: path, body: body, unsigned: true})
	}

	// Before there is a policy, nobody is nobody.
	s.expectError(anon(http.MethodGet, "/site/index.html", nil), http.StatusForbidden, "AccessDenied")
	s.expectError(s.do(call{method: http.MethodGet, path: "/site?policy"}), http.StatusNotFound, "NoSuchBucketPolicy")

	for name, tt := range map[string]struct {
		body string
		code string
	}{
		"not JSON":             {"Statement: allow", "InvalidArgument"},
		"empty":                {"", "InvalidArgument"},
		"no principal":         {`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`, "InvalidArgument"},
		"about another bucket": {strings.ReplaceAll(sitePolicy, "site", "private"), "InvalidArgument"},
	} {
		r := s.do(call{method: http.MethodPut, path: "/site?policy", body: []byte(tt.body)})
		if code := s.errorCode(r); r.status != http.StatusBadRequest || code != tt.code {
			t.Errorf("PutBucketPolicy with %s = %d %s", name, r.status, code)
		}
	}
	// A policy the signature does not cover decides nothing.
	s.expectError(s.do(call{method: http.MethodPut, path: "/site?policy", body: []byte(sitePolicy), payloadHash: "UNSIGNED-PAYLOAD"}),
		http.StatusBadRequest, "InvalidRequest")
	s.expectError(anon(http.MethodPut, "/site?policy", []byte(sitePolicy)), http.StatusForbidden, "AccessDenied")
	s.expectError(anon(http.MethodGet, "/site/index.html", nil), http.StatusForbidden, "AccessDenied")

	s.mustStatus(s.do(call{method: http.MethodPut, path: "/site?policy", body: []byte(sitePolicy)}), http.StatusNoContent)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/site?policy"}), http.StatusOK); string(got.body) != sitePolicy {
		t.Errorf("GetBucketPolicy = %s", got.body)
	}

	// Anybody reads and lists - and does nothing else, though the policy
	// says they may write and delete.
	if got := s.mustStatus(anon(http.MethodGet, "/site/index.html", nil), http.StatusOK); string(got.body) != "<h1>hello</h1>" {
		t.Errorf("anonymous GET = %q", got.body)
	}
	s.mustStatus(anon(http.MethodHead, "/site/index.html", nil), http.StatusOK)
	if listing := s.mustStatus(anon(http.MethodGet, "/site?list-type=2", nil), http.StatusOK); !bytes.Contains(listing.body, []byte("<Key>index.html</Key>")) {
		t.Errorf("anonymous listing = %s", listing.body)
	}
	s.expectError(anon(http.MethodGet, "/site/drafts/new.html", nil), http.StatusForbidden, "AccessDenied")
	for name, r := range map[string]reply{
		"PUT":              anon(http.MethodPut, "/site/defaced.html", []byte("x")),
		"DELETE":           anon(http.MethodDelete, "/site/index.html", nil),
		"the policy":       anon(http.MethodGet, "/site?policy", nil),
		"deleting it":      anon(http.MethodDelete, "/site?policy", nil),
		"versions":         anon(http.MethodGet, "/site?versioning", nil),
		"uploads":          anon(http.MethodGet, "/site?uploads", nil),
		"beginning one":    anon(http.MethodPost, "/site/k?uploads", nil),
		"tags":             anon(http.MethodGet, "/site/index.html?tagging", nil),
		"another bucket":   anon(http.MethodGet, "/private/secret", nil),
		"a missing bucket": anon(http.MethodGet, "/missing/k", nil),
		"every bucket":     anon(http.MethodGet, "/", nil),
	} {
		if r.status != http.StatusForbidden {
			t.Errorf("anonymous %s = %d, want 403", name, r.status)
		}
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/site/index.html"}), http.StatusOK); string(got.body) != "<h1>hello</h1>" {
		t.Errorf("the page after the anonymous attempts = %q", got.body)
	}
	// What is missing is missing to those who may read, not forbidden.
	s.expectError(anon(http.MethodGet, "/site/missing.html", nil), http.StatusNotFound, "NoSuchKey")

	// A user the policy names needs no policy of their own; the Deny for
	// everyone is a Deny for them, and for a user their own policy allows.
	root := s.principal()
	if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: "editor"}); err != nil {
		t.Fatal(err)
	}
	creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: "editor"})
	if err != nil {
		t.Fatal(err)
	}
	editor := func(c call) reply {
		c.keyID, c.key = creds.AccessKeyID, []byte(creds.SecretKey)
		return s.do(c)
	}
	s.mustStatus(editor(call{method: http.MethodPut, path: "/site/about.html", body: []byte("about")}), http.StatusOK)
	s.expectError(editor(call{method: http.MethodPut, path: "/private/x", body: []byte("x")}), http.StatusForbidden, "AccessDenied")
	s.expectError(editor(call{method: http.MethodGet, path: "/site/drafts/new.html"}), http.StatusForbidden, "AccessDenied")
	s.expectError(editor(call{method: http.MethodPut, path: "/site?policy", body: []byte(sitePolicy)}), http.StatusForbidden, "AccessDenied")
	// Root is bound by no policy.
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/site/drafts/new.html"}), http.StatusOK)

	// Without the policy the bucket is closed again.
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/site?policy"}), http.StatusNoContent)
	s.expectError(anon(http.MethodGet, "/site/index.html", nil), http.StatusForbidden, "AccessDenied")
	s.expectError(editor(call{method: http.MethodPut, path: "/site/about.html", body: []byte("x")}), http.StatusForbidden, "AccessDenied")
	s.expectError(s.do(call{method: http.MethodGet, path: "/site?policy"}), http.StatusNotFound, "NoSuchBucketPolicy")
}

const corsBody = `<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<CORSRule><ID>app</ID><AllowedOrigin>https://app.example</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod>
		<AllowedHeader>content-type</AllowedHeader><AllowedHeader>x-amz-*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>600</MaxAgeSeconds></CORSRule>
	<CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>HEAD</AllowedMethod></CORSRule>
</CORSConfiguration>`

// TestCORSOverHTTP: cross-origin rules, the preflight that asks about them
// without credentials, and the answer to the request that follows.
func TestCORSOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("assets")
	s.createBucket("plain")
	s.putObject("/assets/app.js", []byte("js"))
	preflight := func(path, origin, method, headers string) reply {
		h := map[string]string{}
		for name, value := range map[string]string{"Origin": origin, "Access-Control-Request-Method": method, "Access-Control-Request-Headers": headers} {
			if value != "" {
				h[name] = value
			}
		}
		return s.do(call{method: http.MethodOptions, path: path, unsigned: true, headers: h})
	}

	s.expectError(s.do(call{method: http.MethodGet, path: "/assets?cors"}), http.StatusNotFound, "NoSuchCORSConfiguration")
	for name, body := range map[string]string{
		"no rules":              `<CORSConfiguration/>`,
		"a method that is none": `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>PATCH</AllowedMethod></CORSRule></CORSConfiguration>`,
		"two wildcards":         `<CORSConfiguration><CORSRule><AllowedOrigin>https://*.*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`,
		"an element not of S3":  `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowCredentials>true</AllowCredentials></CORSRule></CORSConfiguration>`,
		"not XML":               `origins: *`,
	} {
		if r := s.do(call{method: http.MethodPut, path: "/assets?cors", body: []byte(body)}); r.status != http.StatusBadRequest {
			t.Errorf("PutBucketCors with %s = %d", name, r.status)
		}
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/assets?cors", body: []byte(corsBody)}), http.StatusOK)
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/assets?cors"}), http.StatusOK)
	for _, want := range []string{"<AllowedOrigin>https://app.example</AllowedOrigin>", "<AllowedMethod>PUT</AllowedMethod>", "<MaxAgeSeconds>600</MaxAgeSeconds>", "<ExposeHeader>ETag</ExposeHeader>", "<ID>app</ID>"} {
		if !bytes.Contains(got.body, []byte(want)) {
			t.Errorf("GetBucketCors lacks %s: %s", want, got.body)
		}
	}

	// A preflight that a rule allows.
	ok := s.mustStatus(preflight("/assets/app.js", "https://app.example", "PUT", "Content-Type, X-Amz-Date"), http.StatusOK)
	if ok.header.Get("Access-Control-Allow-Origin") != "https://app.example" || ok.header.Get("Access-Control-Allow-Methods") != "GET, PUT" ||
		ok.header.Get("Access-Control-Allow-Headers") != "Content-Type, X-Amz-Date" || ok.header.Get("Access-Control-Max-Age") != "600" ||
		ok.header.Get("Access-Control-Expose-Headers") != "ETag" || !strings.HasPrefix(ok.header.Get("Vary"), "Origin") ||
		ok.header.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("preflight headers = %v", ok.header)
	}
	if any := s.mustStatus(preflight("/assets/app.js", "https://anywhere.test", "HEAD", ""), http.StatusOK); any.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight for any origin = %v", any.header)
	}
	// And the ones no rule allows: one answer, whatever the reason - a
	// bucket that is not there included.
	var refusals []string
	for name, r := range map[string]reply{
		"another origin":         preflight("/assets/app.js", "https://evil.test", "GET", ""),
		"another method":         preflight("/assets/app.js", "https://app.example", "DELETE", ""),
		"a header not allowed":   preflight("/assets/app.js", "https://app.example", "PUT", "Authorization"),
		"a bucket without rules": preflight("/plain/k", "https://app.example", "GET", ""),
		"a missing bucket":       preflight("/missing/k", "https://app.example", "GET", ""),
		"no origin":              preflight("/assets/app.js", "", "GET", ""),
		"no method":              preflight("/assets/app.js", "https://app.example", "", ""),
		"no bucket":              preflight("/", "https://app.example", "GET", ""),
	} {
		if code := s.errorCode(r); r.status != http.StatusForbidden || code != "AccessForbidden" || r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("preflight with %s = %d %s, %v", name, r.status, code, r.header)
		}
		// The request ID differs; nothing else may.
		body := string(r.body)
		if i := strings.Index(body, "<Resource>"); i >= 0 {
			body = body[:i]
		}
		refusals = append(refusals, body)
	}
	for _, body := range refusals {
		if body != refusals[0] {
			t.Errorf("two refused preflights are told apart: %q and %q", body, refusals[0])
		}
	}

	// The request itself: the headers that let a script see the answer,
	// for an origin a rule allows and for no other. Authentication is as
	// ever.
	actual := s.mustStatus(s.do(call{method: http.MethodGet, path: "/assets/app.js", headers: map[string]string{"Origin": "https://app.example"}}), http.StatusOK)
	if actual.header.Get("Access-Control-Allow-Origin") != "https://app.example" || actual.header.Get("Access-Control-Expose-Headers") != "ETag" {
		t.Errorf("headers of the cross-origin GET = %v", actual.header)
	}
	other := s.mustStatus(s.do(call{method: http.MethodGet, path: "/assets/app.js", headers: map[string]string{"Origin": "https://evil.test"}}), http.StatusOK)
	if other.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("another origin was given %v", other.header)
	}
	denied := s.do(call{method: http.MethodGet, path: "/assets/app.js", unsigned: true, headers: map[string]string{"Origin": "https://app.example"}})
	if denied.status != http.StatusForbidden {
		t.Errorf("a cross-origin GET without credentials = %d", denied.status)
	}

	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/assets?cors"}), http.StatusNoContent)
	s.expectError(preflight("/assets/app.js", "https://app.example", "GET", ""), http.StatusForbidden, "AccessForbidden")
	s.expectError(s.do(call{method: http.MethodPut, path: "/assets?cors", body: []byte(corsBody), unsigned: true}), http.StatusForbidden, "AccessDenied")
}

func tagBody(pairs ...string) []byte {
	var b strings.Builder
	b.WriteString(`<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet>`)
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString("<Tag><Key>" + pairs[i] + "</Key><Value>" + pairs[i+1] + "</Value></Tag>")
	}
	b.WriteString(`</TagSet></Tagging>`)
	return []byte(b.String())
}

// TestTaggingOverHTTP: tags of buckets, of objects and of versions.
func TestTaggingOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")

	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?tagging"}), http.StatusNotFound, "NoSuchTagSet")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?tagging", body: tagBody("team", "finance", "env", "prod")}), http.StatusNoContent)
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?tagging"}), http.StatusOK)
	if !bytes.Contains(got.body, []byte("<Tag><Key>env</Key><Value>prod</Value></Tag><Tag><Key>team</Key><Value>finance</Value></Tag>")) {
		t.Errorf("GetBucketTagging = %s", got.body)
	}
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt?tagging"}), http.StatusNoContent)
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?tagging"}), http.StatusNotFound, "NoSuchTagSet")

	// An object is created with tags, and says how many it has.
	put := s.do(call{method: http.MethodPut, path: "/bkt/report", body: []byte("q3"), headers: map[string]string{"x-amz-tagging": "project=apollo&stage=draft%20one"}})
	s.mustStatus(put, http.StatusOK)
	tags := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/report?tagging"}), http.StatusOK)
	if !bytes.Contains(tags.body, []byte("<Key>stage</Key><Value>draft one</Value>")) || !bytes.Contains(tags.body, []byte("<Key>project</Key><Value>apollo</Value>")) {
		t.Errorf("GetObjectTagging = %s", tags.body)
	}
	if head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/bkt/report"}), http.StatusOK); head.header.Get("x-amz-tagging-count") != "2" {
		t.Errorf("x-amz-tagging-count = %q", head.header.Get("x-amz-tagging-count"))
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt/report?tagging", body: tagBody("stage", "final")}), http.StatusOK)
	if tags := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/report?tagging"}), http.StatusOK); bytes.Contains(tags.body, []byte("apollo")) || !bytes.Contains(tags.body, []byte("final")) {
		t.Errorf("tags after replacing = %s", tags.body)
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/report"}), http.StatusOK); string(got.body) != "q3" || got.header.Get("x-amz-tagging-count") != "1" {
		t.Errorf("the object after tagging = %q, %v", got.body, got.header)
	}
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt/report?tagging"}), http.StatusNoContent)
	if tags := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/report?tagging"}), http.StatusOK); bytes.Contains(tags.body, []byte("<Tag>")) {
		t.Errorf("tags after deleting = %s", tags.body)
	}
	// Deleting the tags did not delete the object.
	s.mustStatus(s.do(call{method: http.MethodHead, path: "/bkt/report"}), http.StatusOK)

	eleven := []string{}
	for _, k := range strings.Split("a b c d e f g h i j k", " ") {
		eleven = append(eleven, k, "v")
	}
	for name, tt := range map[string]struct {
		c      call
		status int
	}{
		"eleven tags":           {call{method: http.MethodPut, path: "/bkt/report?tagging", body: tagBody(eleven...)}, 400},
		"a key twice":           {call{method: http.MethodPut, path: "/bkt/report?tagging", body: tagBody("a", "1", "a", "2")}, 400},
		"an empty key":          {call{method: http.MethodPut, path: "/bkt/report?tagging", body: tagBody("", "1")}, 400},
		"not XML":               {call{method: http.MethodPut, path: "/bkt/report?tagging", body: []byte("a=b")}, 400},
		"an unsigned body":      {call{method: http.MethodPut, path: "/bkt/report?tagging", body: tagBody("a", "1"), payloadHash: "UNSIGNED-PAYLOAD"}, 400},
		"a missing object":      {call{method: http.MethodGet, path: "/bkt/missing?tagging"}, 404},
		"a header with eleven":  {call{method: http.MethodPut, path: "/bkt/x", body: []byte("x"), headers: map[string]string{"x-amz-tagging": "a=1&b=1&c=1&d=1&e=1&f=1&g=1&h=1&i=1&j=1&k=1"}}, 400},
		"a header with a twice": {call{method: http.MethodPut, path: "/bkt/x", body: []byte("x"), headers: map[string]string{"x-amz-tagging": "a=1&a=2"}}, 400},
		"a header that is none": {call{method: http.MethodPut, path: "/bkt/x", body: []byte("x"), headers: map[string]string{"x-amz-tagging": "a=%zz"}}, 400},
		"tags on an upload":     {call{method: http.MethodPost, path: "/bkt/x?uploads", headers: map[string]string{"x-amz-tagging": "a=1"}}, 501},
		"tags on a read":        {call{method: http.MethodGet, path: "/bkt/report", headers: map[string]string{"x-amz-tagging": "a=1"}}, 501},
		"without credentials":   {call{method: http.MethodPut, path: "/bkt/report?tagging", body: tagBody("a", "1"), unsigned: true}, 403},
	} {
		if r := s.do(tt.c); r.status != tt.status {
			t.Errorf("%s = %d, want %d", name, r.status, tt.status)
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt/x"}), http.StatusNotFound, "NoSuchKey")

	// Tags belong to a version.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?versioning", body: versioningBody("Enabled")}), http.StatusOK)
	first := s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt/doc", body: []byte("one"), headers: map[string]string{"x-amz-tagging": "v=1"}}), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt/doc", body: []byte("two")}), http.StatusOK)
	old := "/bkt/doc?tagging&versionId=" + first.header.Get("x-amz-version-id")
	if tags := s.mustStatus(s.do(call{method: http.MethodGet, path: old}), http.StatusOK); !bytes.Contains(tags.body, []byte("<Key>v</Key><Value>1</Value>")) {
		t.Errorf("tags of the old version = %s", tags.body)
	}
	if tags := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/doc?tagging"}), http.StatusOK); bytes.Contains(tags.body, []byte("<Tag>")) {
		t.Errorf("tags of the new version = %s", tags.body)
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: old, body: tagBody("v", "1", "kept", "yes")}), http.StatusOK)
	if tags := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/doc?tagging"}), http.StatusOK); bytes.Contains(tags.body, []byte("<Tag>")) {
		t.Errorf("tagging the old version tagged the new one: %s", tags.body)
	}
}

// TestNoBucketPolicyGivesTheGovernanceBypass: whoever may write a bucket's
// policy cannot use it to give themselves what no wildcard gives.
func TestNoBucketPolicyGivesTheGovernanceBypass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault", headers: map[string]string{headerLockEnabled: "true"}}), http.StatusOK)
	put := s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/draft", body: []byte("kept"), headers: map[string]string{
		headerLockMode: "GOVERNANCE", headerLockUntil: "2099-01-01T00:00:00Z",
	}}), http.StatusOK)
	version := put.header.Get("x-amz-version-id")
	root := s.principal()
	if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: "badmin", Roles: []string{"bucket-admin"}}); err != nil {
		t.Fatal(err)
	}
	creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: "badmin"})
	if err != nil {
		t.Fatal(err)
	}
	as := func(c call) reply {
		c.keyID, c.key = creds.AccessKeyID, []byte(creds.SecretKey)
		return s.do(c)
	}
	remove := call{method: http.MethodDelete, path: "/vault/draft?versionId=" + version, headers: map[string]string{headerLockOverride: "true"}}
	s.expectError(as(remove), http.StatusForbidden, "AccessDenied")
	for _, action := range []string{`"s3:BypassGovernanceRetention"`, `"s3:bypassgovernanceretention"`, `["s3:GetObject","s3:BypassGovernanceRetention"]`} {
		policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/badmin"},"Action":` + action + `,"Resource":"arn:aws:s3:::vault/*"}]}`
		s.expectError(as(call{method: http.MethodPut, path: "/vault?policy", body: []byte(policy)}), http.StatusBadRequest, "InvalidArgument")
	}
	// A wildcard is accepted as a policy, and gives everything but that.
	wild := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::vault/*"}]}`
	s.mustStatus(as(call{method: http.MethodPut, path: "/vault?policy", body: []byte(wild)}), http.StatusNoContent)
	s.expectError(as(remove), http.StatusForbidden, "AccessDenied")
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/draft?versionId=" + version}), http.StatusOK); string(got.body) != "kept" {
		t.Errorf("the retained version reads %q", got.body)
	}
	// A Deny of it is a policy like any other.
	deny := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:BypassGovernanceRetention","Resource":"arn:aws:s3:::vault/*"}]}`
	s.mustStatus(as(call{method: http.MethodPut, path: "/vault?policy", body: []byte(deny)}), http.StatusNoContent)
}

// TestAnonymousAccessByVirtualHost: what a policy opens is open however the
// bucket is addressed, and nothing else is.
func TestAnonymousAccessByVirtualHost(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "s3.cactus.test")
	s.createBucket("site")
	s.createBucket("private")
	s.putObject("/site/index.html", []byte("hello"))
	s.putObject("/private/secret", []byte("secret"))
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/site?policy", body: []byte(sitePolicy)}), http.StatusNoContent)
	anon := func(method, path, host string) reply {
		return s.do(call{method: method, path: path, host: host, unsigned: true})
	}
	if got := s.mustStatus(anon(http.MethodGet, "/index.html", "site.s3.cactus.test"), http.StatusOK); string(got.body) != "hello" {
		t.Errorf("anonymous GET by virtual host = %q", got.body)
	}
	for name, r := range map[string]reply{
		"another bucket":       anon(http.MethodGet, "/secret", "private.s3.cactus.test"),
		"a write":              anon(http.MethodPut, "/index.html", "site.s3.cactus.test"),
		"the policy":           anon(http.MethodGet, "/?policy", "site.s3.cactus.test"),
		"every bucket":         anon(http.MethodGet, "/", "s3.cactus.test"),
		"a bucket in a bucket": anon(http.MethodGet, "/private/secret", "site.s3.cactus.test"),
	} {
		if r.status == http.StatusOK {
			t.Errorf("anonymous %s by virtual host = %d %q", name, r.status, r.body)
		}
	}
}

// TestADamagedPolicyIsNotTheClientsFault: a stored policy that cannot be
// read refuses the request as a failure of the server, and root repairs it.
func TestADamagedPolicyIsNotTheClientsFault(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("site")
	s.putObject("/site/k", []byte("x"))
	if err := s.meta.SetBucketConfig(context.Background(), "site", "policy", []byte("damaged")); err != nil {
		t.Fatal(err)
	}
	if r := s.do(call{method: http.MethodGet, path: "/site/k", unsigned: true}); r.status != http.StatusInternalServerError {
		t.Errorf("anonymous GET under a damaged policy = %d, want 500", r.status)
	}
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/site/k"}), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/site?policy"}), http.StatusNoContent)
	s.expectError(s.do(call{method: http.MethodGet, path: "/site/k", unsigned: true}), http.StatusForbidden, "AccessDenied")
}

// FuzzCORSConfiguration: what is accepted as cross-origin rules has no more
// rules than the document names, and every header in it is a header name.
func FuzzCORSConfiguration(f *testing.F) {
	f.Add([]byte(corsBody))
	f.Add([]byte(`<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><ExposeHeader>a b: c</ExposeHeader></CORSRule></CORSConfiguration>`))
	f.Add([]byte(`<!DOCTYPE a [<!ENTITY b "c">]><CORSConfiguration/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var in corsConfiguration
		err := decodeStrictXML(bytes.NewReader(data), maxConfigBody, &in,
			"CORSConfiguration", "CORSRule*", "ID", "AllowedOrigin*", "AllowedMethod*", "AllowedHeader*", "ExposeHeader*", "MaxAgeSeconds")
		if err != nil {
			return
		}
		if bytes.Contains(data, []byte("<!DOCTYPE")) || len(in.Rules) > bytes.Count(data, []byte("<CORSRule")) {
			t.Fatalf("%d rules decoded from %q", len(in.Rules), data)
		}
		rules := bucket.CORS{}
		for _, r := range in.Rules {
			rules.Rules = append(rules.Rules, bucket.CORSRule{
				ID: r.ID, AllowedOrigins: r.AllowedOrigins, AllowedMethods: r.AllowedMethods, AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders,
			})
		}
		if rules.Validate() != nil {
			return
		}
		for _, rule := range rules.Rules {
			for _, value := range slices.Concat(rule.AllowedOrigins, rule.AllowedHeaders, rule.ExposeHeaders, rule.AllowedMethods) {
				if strings.ContainsAny(value, "\r\n") || value == "" {
					t.Fatalf("a value that cannot be a header was accepted: %q", value)
				}
			}
			for _, header := range rule.ExposeHeaders {
				if !bucket.HeaderName(header) {
					t.Fatalf("exposed header %q", header)
				}
			}
		}
	})
}

// FuzzTagging: tags are read from a document and from the header that
// creates an object alike: no key twice, and nothing the domain refuses.
func FuzzTagging(f *testing.F) {
	f.Add(string(tagBody("a", "1", "b", "2")), "a=1&b=2")
	f.Add(string(tagBody("a", "1", "a", "2")), "a=1&a=2")
	f.Add(`<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value><Key>k2</Key></Tag></TagSet></Tagging>`, "%00=x")
	f.Fuzz(func(t *testing.T, document, header string) {
		var in tagging
		if err := decodeStrictXML(strings.NewReader(document), maxConfigBody, &in, "Tagging", "TagSet", "Tag*", "Key", "Value"); err == nil {
			if len(in.Tags) > strings.Count(document, "<Tag") {
				t.Fatalf("%d tags decoded from %q", len(in.Tags), document)
			}
		}
		tags, err := tagsOf(http.Header{"X-Amz-Tagging": []string{header}})
		if err != nil {
			return
		}
		if object.ValidateTags(tags, object.MaxObjectTags) != nil {
			t.Fatalf("the header %q gave tags the domain refuses: %v", header, tags)
		}
		for key, value := range tags {
			if strings.ContainsAny(key+value, "\x00\r\n") {
				t.Fatalf("a tag with a control character: %q=%q", key, value)
			}
		}
	})
}
