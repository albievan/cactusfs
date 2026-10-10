package s3api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestCopyObjectOverHTTP: CopyObject as an S3 client sends it.
func TestCopyObjectOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("src")
	s.createBucket("dst")
	body := []byte("the bytes that are copied")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/src/dir/a b.txt", body: body, headers: map[string]string{
		"Content-Type": "text/plain", "X-Amz-Meta-Owner": "albie", "x-amz-tagging": "project=apollo",
	}}), http.StatusOK)
	copyTo := func(path, source string, headers map[string]string) reply {
		h := map[string]string{"x-amz-copy-source": source}
		for name, value := range headers {
			h[name] = value
		}
		return s.do(call{method: http.MethodPut, path: path, headers: h})
	}

	done := s.mustStatus(copyTo("/dst/copy.txt", "/src/dir/a%20b.txt", nil), http.StatusOK)
	var result copyObjectResult
	if err := xml.Unmarshal(done.body, &result); err != nil || result.ETag != etagOf(body) || result.LastModified == "" {
		t.Fatalf("CopyObject answered %s (%v)", done.body, err)
	}
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/dst/copy.txt"}), http.StatusOK)
	if !bytes.Equal(got.body, body) || got.header.Get("Content-Type") != "text/plain" || got.header.Get("X-Amz-Meta-Owner") != "albie" ||
		got.header.Get("x-amz-tagging-count") != "1" || got.header.Get("ETag") != etagOf(body) {
		t.Errorf("the copy = %q, %v", got.body, got.header)
	}
	// The source without a leading slash, and with the metadata and tags
	// of the request.
	s.mustStatus(copyTo("/dst/replaced", "src/dir/a%20b.txt", map[string]string{
		"x-amz-metadata-directive": "REPLACE", "Content-Type": "application/json", "X-Amz-Meta-Stage": "final",
		"x-amz-tagging-directive": "REPLACE", "x-amz-tagging": "a=1&b=2",
	}), http.StatusOK)
	replaced := s.mustStatus(s.do(call{method: http.MethodHead, path: "/dst/replaced"}), http.StatusOK)
	if replaced.header.Get("Content-Type") != "application/json" || replaced.header.Get("X-Amz-Meta-Stage") != "final" ||
		replaced.header.Get("X-Amz-Meta-Owner") != "" || replaced.header.Get("x-amz-tagging-count") != "2" {
		t.Errorf("the copy with replaced metadata = %v", replaced.header)
	}
	// Conditions on the source.
	s.mustStatus(copyTo("/dst/if", "/src/dir/a%20b.txt", map[string]string{"x-amz-copy-source-if-match": etagOf(body)}), http.StatusOK)
	s.expectError(copyTo("/dst/never", "/src/dir/a%20b.txt", map[string]string{"x-amz-copy-source-if-match": `"another"`}), http.StatusPreconditionFailed, "PreconditionFailed")
	s.expectError(copyTo("/dst/never", "/src/dir/a%20b.txt", map[string]string{"x-amz-copy-source-if-none-match": etagOf(body)}), http.StatusPreconditionFailed, "PreconditionFailed")
	s.expectError(copyTo("/dst/never", "/src/dir/a%20b.txt", map[string]string{"x-amz-copy-source-if-unmodified-since": "Mon, 01 Jan 2001 00:00:00 GMT"}), http.StatusPreconditionFailed, "PreconditionFailed")

	// A version of the source, where versions are kept.
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/src?versioning", body: versioningBody("Enabled")}), http.StatusOK)
	first := s.mustStatus(s.do(call{method: http.MethodPut, path: "/src/doc", body: []byte("one")}), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/src/doc", body: []byte("two")}), http.StatusOK)
	old := s.mustStatus(copyTo("/dst/old", "/src/doc?versionId="+first.header.Get("x-amz-version-id"), nil), http.StatusOK)
	if old.header.Get("x-amz-copy-source-version-id") != first.header.Get("x-amz-version-id") {
		t.Errorf("x-amz-copy-source-version-id = %q", old.header.Get("x-amz-copy-source-version-id"))
	}
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/dst/old"}), http.StatusOK); string(got.body) != "one" {
		t.Errorf("the copy of an old version = %q", got.body)
	}

	for name, tt := range map[string]struct {
		r      reply
		status int
		code   string
	}{
		"a source that is not there":     {copyTo("/dst/never", "/src/missing", nil), 404, "NoSuchKey"},
		"a source bucket that is not":    {copyTo("/dst/never", "/missing/k", nil), 404, "NoSuchBucket"},
		"a destination that is not":      {copyTo("/missing/never", "/src/doc", nil), 404, "NoSuchBucket"},
		"a source that is only a bucket": {copyTo("/dst/never", "/src", nil), 400, "InvalidArgument"},
		"a source that is nothing":       {copyTo("/dst/never", "/", nil), 400, "InvalidArgument"},
		"a source badly escaped":         {copyTo("/dst/never", "/src/%zz", nil), 400, "InvalidArgument"},
		"a version that is none":         {copyTo("/dst/never", "/src/doc?versionId=zz", nil), 400, "InvalidArgument"},
		"a source with another query":    {copyTo("/dst/never", "/src/doc?acl", nil), 400, "InvalidArgument"},
		"onto itself, changing nothing":  {copyTo("/dst/copy.txt", "/dst/copy.txt", nil), 400, "InvalidArgument"},
		"a directive that is none":       {copyTo("/dst/never", "/src/doc", map[string]string{"x-amz-metadata-directive": "MERGE"}), 400, "InvalidArgument"},
		"tags without the directive":     {copyTo("/dst/never", "/src/doc", map[string]string{"x-amz-tagging": "a=1"}), 400, "InvalidArgument"},
		"a range, which is for parts":    {copyTo("/dst/never", "/src/doc", map[string]string{"x-amz-copy-source-range": "bytes=0-1"}), 400, "InvalidArgument"},
		"a condition that is no date":    {copyTo("/dst/never", "/src/doc", map[string]string{"x-amz-copy-source-if-modified-since": "yesterday"}), 400, "InvalidArgument"},
		"with a body":                    {s.do(call{method: http.MethodPut, path: "/dst/never", body: []byte("x"), headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 400, "InvalidArgument"},
		"a checksum to compute":          {copyTo("/dst/never", "/src/doc", map[string]string{"x-amz-checksum-algorithm": "CRC32"}), 501, "NotImplemented"},
		// A source on what is routed elsewhere is not a copy: it would
		// be ignored, and is refused instead.
		"a source on a PUT of tags":         {s.do(call{method: http.MethodPut, path: "/dst/copy.txt?tagging", body: tagBody("a", "1"), headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 400, "InvalidArgument"},
		"a source on a PUT of a retention":  {s.do(call{method: http.MethodPut, path: "/dst/copy.txt?retention", headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 400, "InvalidArgument"},
		"a source on a PUT of a legal hold": {s.do(call{method: http.MethodPut, path: "/dst/copy.txt?legal-hold", headers: map[string]string{"x-amz-copy-source": "/src/doc", "x-amz-metadata-directive": "REPLACE"}}), 400, "InvalidArgument"},
		"a write condition":                 {copyTo("/dst/never", "/src/doc", map[string]string{"If-None-Match": "*"}), 501, "NotImplemented"},
		"without credentials":               {s.do(call{method: http.MethodPut, path: "/dst/never", unsigned: true, headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 403, "AccessDenied"},
		// What belongs to a copy, on what is not one.
		"a directive on a plain put": {s.do(call{method: http.MethodPut, path: "/dst/never", body: []byte("x"), headers: map[string]string{"x-amz-metadata-directive": "REPLACE"}}), 400, "InvalidArgument"},
		"a source on a read":         {s.do(call{method: http.MethodGet, path: "/dst/copy.txt", headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 400, "InvalidArgument"},
		"a source on a delete":       {s.do(call{method: http.MethodDelete, path: "/dst/copy.txt", headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 400, "InvalidArgument"},
		"a source on a bucket":       {s.do(call{method: http.MethodPut, path: "/another", headers: map[string]string{"x-amz-copy-source": "/src/doc"}}), 400, "InvalidArgument"},
	} {
		if code := s.errorCode(tt.r); tt.r.status != tt.status || code != tt.code {
			t.Errorf("a copy with %s = %d %s, want %d %s", name, tt.r.status, code, tt.status, tt.code)
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/dst/never"}), http.StatusNotFound, "NoSuchKey")
	s.mustStatus(s.do(call{method: http.MethodHead, path: "/dst/copy.txt"}), http.StatusOK)
	if r := s.do(call{method: http.MethodHead, path: "/another"}); r.status != http.StatusNotFound {
		t.Errorf("a refused request made a bucket: %d", r.status)
	}
}

// TestCopyWithCustomerKeysOverHTTP: the source and the destination of a copy
// each have their key, in headers of their own.
func TestCopyWithCustomerKeysOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTLSTestServer(t)
	s.createBucket("vault")
	mine, theirs := customerHeaders(bytes.Repeat([]byte{1}, 32)), customerHeaders(bytes.Repeat([]byte{2}, 32))
	sourceKey := func(headers map[string]string) map[string]string {
		out := map[string]string{"x-amz-copy-source": "/vault/secret"}
		for name, value := range headers {
			out[strings.Replace(name, "x-amz-", "x-amz-copy-source-", 1)] = value
		}
		return out
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/secret", body: []byte("sealed"), headers: mine}), http.StatusOK)
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault/open", headers: sourceKey(nil)}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodPut, path: "/vault/open", headers: sourceKey(theirs)}), http.StatusForbidden, "AccessDenied")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/open", headers: sourceKey(mine)}), http.StatusOK)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/open"}), http.StatusOK); string(got.body) != "sealed" || got.header.Get(headerSSE) != "AES256" {
		t.Errorf("the copy out of SSE-C = %q, %v", got.body, got.header)
	}
	// Under another customer key at the destination.
	both := sourceKey(mine)
	for name, value := range theirs {
		both[name] = value
	}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault/rekeyed", headers: both}), http.StatusOK)
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/rekeyed", headers: mine}), http.StatusForbidden, "AccessDenied")
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/rekeyed", headers: theirs}), http.StatusOK); string(got.body) != "sealed" {
		t.Errorf("the copy under another key = %q", got.body)
	}
	// The key of the source in the query string of a presigned URL is
	// refused, like the key of the object itself: a URL is logged
	// wherever it goes.
	names := copySourceSSEHeaders()
	inQuery := "/vault/presigned?" + names[0] + "=AES256&" + names[1] + "=" + urlEscape(mine[headerSSECKey]) + "&" + names[2] + "=" + urlEscape(mine[headerSSECKeyMD5]) +
		"&x-amz-copy-source=" + urlEscape("/vault/secret")
	r := s.do(call{method: http.MethodPut, path: s.presign(http.MethodPut, inQuery, serverNow, 600, nil), unsigned: true})
	if code := s.errorCode(r); r.status != http.StatusBadRequest || code != "AuthorizationQueryParametersError" {
		t.Errorf("the customer key of a copy source in a presigned query = %d %s", r.status, code)
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/vault/presigned", headers: mine}), http.StatusNotFound, "NoSuchKey")
	if logs := s.logs.String(); strings.Contains(logs, mine[headerSSECKey]) || strings.Contains(logs, theirs[headerSSECKey]) {
		t.Error("a customer key was logged")
	}
}

// TestUploadPartCopyOverHTTP: a part whose bytes are a range of an object.
func TestUploadPartCopyOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("media")
	source := randomPart(t, object.MinPartSize+1000)
	s.putObject("/media/source", source)
	id := s.createUpload("/media/film", nil)
	base := "/media/film?uploadId=" + id
	partCopy := func(number int, headers map[string]string) reply {
		h := map[string]string{"x-amz-copy-source": "/media/source"}
		for name, value := range headers {
			h[name] = value
		}
		return s.do(call{method: http.MethodPut, path: fmt.Sprintf("%s&partNumber=%d", base, number), headers: h})
	}
	one := s.mustStatus(partCopy(1, map[string]string{"x-amz-copy-source-range": fmt.Sprintf("bytes=0-%d", object.MinPartSize-1)}), http.StatusOK)
	var first copyPartResult
	if err := xml.Unmarshal(one.body, &first); err != nil || first.ETag != etagOf(source[:object.MinPartSize]) {
		t.Fatalf("UploadPartCopy answered %s (%v)", one.body, err)
	}
	two := s.mustStatus(partCopy(2, map[string]string{"x-amz-copy-source-range": fmt.Sprintf("bytes=%d-%d", object.MinPartSize, object.MinPartSize+999)}), http.StatusOK)
	var second copyPartResult
	if err := xml.Unmarshal(two.body, &second); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		headers map[string]string
		status  int
		code    string
	}{
		"a range past the end":                        {map[string]string{"x-amz-copy-source-range": fmt.Sprintf("bytes=%d-%d", object.MinPartSize+2000, object.MinPartSize+3000)}, 400, "InvalidArgument"},
		"a range that ends after the source":          {map[string]string{"x-amz-copy-source-range": "bytes=0-999999999"}, 400, "InvalidArgument"},
		"a checksum to compute":                       {map[string]string{"x-amz-checksum-algorithm": "SHA256"}, 501, "NotImplemented"},
		"a range without an end":                      {map[string]string{"x-amz-copy-source-range": "bytes=5-"}, 400, "InvalidArgument"},
		"the last bytes":                              {map[string]string{"x-amz-copy-source-range": "bytes=-5"}, 400, "InvalidArgument"},
		"a range backwards":                           {map[string]string{"x-amz-copy-source-range": "bytes=9-3"}, 400, "InvalidArgument"},
		"a range that is none":                        {map[string]string{"x-amz-copy-source-range": "0-9"}, 400, "InvalidArgument"},
		"a directive, which is for copies of objects": {map[string]string{"x-amz-metadata-directive": "REPLACE"}, 400, "InvalidArgument"},
		"a condition that fails":                      {map[string]string{"x-amz-copy-source-if-match": `"another"`}, 412, "PreconditionFailed"},
	} {
		r := partCopy(3, tt.headers)
		if code := s.errorCode(r); r.status != tt.status || code != tt.code {
			t.Errorf("UploadPartCopy with %s = %d %s, want %d %s", name, r.status, code, tt.status, tt.code)
		}
	}
	s.mustStatus(s.do(call{method: http.MethodPost, path: base, body: completeBody(listedPart(1, first.ETag), listedPart(2, second.ETag))}), http.StatusOK)
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media/film"}), http.StatusOK); !bytes.Equal(got.body, source) {
		t.Errorf("the object assembled from copied parts = %d bytes", len(got.body))
	}
}

// TestConditionalReadsOverHTTP: If-Match and its kin on GET and HEAD.
func TestConditionalReadsOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	body := []byte("cached by a browser")
	s.putObject("/bkt/k", body)
	tag := etagOf(body)
	modified := s.mustStatus(s.do(call{method: http.MethodHead, path: "/bkt/k"}), http.StatusOK).header.Get("Last-Modified")
	for name, tt := range map[string]struct {
		headers map[string]string
		status  int
	}{
		"If-None-Match, the tag":            {map[string]string{"If-None-Match": tag}, 304},
		"If-None-Match, weak, among others": {map[string]string{"If-None-Match": `"x", W/` + tag}, 304},
		"If-None-Match, any":                {map[string]string{"If-None-Match": "*"}, 304},
		"If-None-Match, another":            {map[string]string{"If-None-Match": `"x"`}, 200},
		"If-Match, the tag":                 {map[string]string{"If-Match": tag}, 200},
		"If-Match, unquoted":                {map[string]string{"If-Match": strings.Trim(tag, `"`)}, 200},
		"If-Match, another":                 {map[string]string{"If-Match": `"x"`}, 412},
		// If-Match compares strongly: a weak tag satisfies it never.
		"If-Match, its tag said weakly": {map[string]string{"If-Match": "W/" + tag}, 412},
		"a date before any object":      {map[string]string{"If-Unmodified-Since": "Mon, 01 Jan 0001 00:00:00 GMT"}, 400},
		// The caller has it already, whatever range it asks for.
		"If-None-Match, with a range outside":  {map[string]string{"If-None-Match": tag, "Range": "bytes=500-600"}, 304},
		"If-Match fails, with a range outside": {map[string]string{"If-Match": `"x"`, "Range": "bytes=500-600"}, 412},
		"If-Modified-Since, its own time":      {map[string]string{"If-Modified-Since": modified}, 304},
		"If-Modified-Since, long before":       {map[string]string{"If-Modified-Since": "Mon, 01 Jan 2001 00:00:00 GMT"}, 200},
		"If-Unmodified-Since, its own time":    {map[string]string{"If-Unmodified-Since": modified}, 200},
		"If-Unmodified-Since, long before":     {map[string]string{"If-Unmodified-Since": "Mon, 01 Jan 2001 00:00:00 GMT"}, 412},
		"a date that is none":                  {map[string]string{"If-Modified-Since": "last week"}, 400},
		"a tag with a space in it":             {map[string]string{"If-Match": `"a b"`}, 400},
		"If-Match holds, the date would not":   {map[string]string{"If-Match": tag, "If-Unmodified-Since": "Mon, 01 Jan 2001 00:00:00 GMT"}, 200},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := s.do(call{method: method, path: "/bkt/k", headers: tt.headers})
			if r.status != tt.status {
				t.Errorf("%s with %s = %d, want %d", method, name, r.status, tt.status)
			}
			switch r.status {
			case http.StatusNotModified:
				if len(r.body) != 0 || r.header.Get("ETag") != tag || r.header.Get("Last-Modified") != modified {
					t.Errorf("%s with %s: a 304 with body %q and headers %v", method, name, r.body, r.header)
				}
			case http.StatusOK:
				if method == http.MethodGet && !bytes.Equal(r.body, body) {
					t.Errorf("GET with %s read %q", name, r.body)
				}
			}
		}
	}
	// With a range, and of a version.
	ranged := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=null", headers: map[string]string{"If-Match": tag, "Range": "bytes=0-5"}}), http.StatusPartialContent)
	if string(ranged.body) != "cached" {
		t.Errorf("a conditional ranged read = %q", ranged.body)
	}
	if r := s.do(call{method: http.MethodGet, path: "/bkt/k?versionId=null", headers: map[string]string{"If-None-Match": tag}}); r.status != http.StatusNotModified || r.header.Get("x-amz-version-id") != "null" {
		t.Errorf("a 304 for a named version = %d, %v", r.status, r.header)
	}
	if r := s.do(call{method: http.MethodGet, path: "/bkt/missing", headers: map[string]string{"If-None-Match": "*"}}); r.status != http.StatusNotFound {
		t.Errorf("a conditional read of what is not there = %d", r.status)
	}
}

// TestListObjectsV1OverHTTP: the first version of ListObjects, by marker.
func TestListObjectsV1OverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	for _, key := range []string{"a", "b", "dir/1", "dir/2", "e", "f g"} {
		s.putObject("/bkt/"+strings.ReplaceAll(key, " ", "%20"), []byte(key))
	}
	list := func(query string) listBucketResultV1 {
		t.Helper()
		r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt" + query}), http.StatusOK)
		var out listBucketResultV1
		if err := xml.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("ListObjects%s: %v in %s", query, err, r.body)
		}
		return out
	}
	keys := func(out listBucketResultV1) string {
		var names []string
		for _, entry := range out.Contents {
			names = append(names, entry.Key)
		}
		for _, prefix := range out.CommonPrefixes {
			names = append(names, prefix.Prefix)
		}
		return strings.Join(names, ",")
	}
	all := list("")
	if keys(all) != "a,b,dir/1,dir/2,e,f g" || all.IsTruncated || all.Name != "bkt" || all.MaxKeys != 1000 || all.Contents[0].Owner == nil ||
		all.Contents[0].ETag != etagOf([]byte("a")) || all.Contents[0].Size != 1 {
		t.Errorf("ListObjects = %+v", all)
	}
	if got := keys(list("?prefix=dir/")); got != "dir/1,dir/2" {
		t.Errorf("with a prefix: %s", got)
	}
	if got := keys(list("?delimiter=/")); got != "a,b,e,f g,dir/" {
		t.Errorf("with a delimiter: %s", got)
	}
	if got := keys(list("?marker=b")); got != "dir/1,dir/2,e,f g" {
		t.Errorf("after a marker: %s", got)
	}
	if got := list("?encoding-type=url&prefix=f"); keys(got) != "f+g" || got.EncodingType != "url" {
		t.Errorf("with url encoding: %+v", got)
	}
	// Page by page, with and without a delimiter: everything once.
	for _, extra := range []string{"", "&delimiter=/"} {
		var seen []string
		marker := ""
		for range 10 {
			page := list("?max-keys=2" + extra + "&marker=" + strings.ReplaceAll(marker, " ", "%20"))
			seen = append(seen, strings.Split(keys(page), ",")...)
			if !page.IsTruncated {
				break
			}
			if marker = page.NextMarker; marker == "" {
				t.Fatalf("a truncated page without a NextMarker: %+v", page)
			}
		}
		want := "a,b,dir/1,dir/2,e,f g"
		if extra != "" {
			want = "a,b,dir/,e,f g"
		}
		// Within a page the keys come before the prefixes; over all
		// pages it is every entry, once.
		slices.Sort(seen)
		if got := strings.Join(seen, ","); got != want {
			t.Errorf("paging%s visited %s, want %s", extra, got, want)
		}
	}
	for name, path := range map[string]string{
		"max-keys that is none": "/bkt?max-keys=many", "an encoding that is none": "/bkt?encoding-type=base64",
	} {
		if r := s.do(call{method: http.MethodGet, path: path}); r.status != http.StatusBadRequest {
			t.Errorf("ListObjects with %s = %d", name, r.status)
		}
	}
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?marker=a&continuation-token=x"}), http.StatusNotImplemented, "NotImplemented")
	s.expectError(s.do(call{method: http.MethodGet, path: "/missing"}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt", unsigned: true}), http.StatusForbidden, "AccessDenied")
}

// FuzzCopySource: the header that names what a copy reads gives a bucket and
// a key that are not empty, or is refused; it never panics.
func FuzzCopySource(f *testing.F) {
	f.Add("/bucket/dir/key%20name?versionId=null", `"abc", W/"def"`)
	f.Add("bucket/key", "*")
	f.Add("/bucket/%2e%2e/%2e%2e/etc", `"`)
	f.Add("//key?versionId=", "a,,b")
	f.Fuzz(func(t *testing.T, source, tags string) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://example.test/b/k", nil)
		if err != nil {
			return
		}
		r.Header.Set(headerCopySource, source)
		r.Header.Set(copySourceIfPrefix+"match", tags)
		in, err := copySourceOf(r)
		if err != nil {
			return
		}
		if in.Bucket == "" || in.Key == "" || strings.Contains(in.Bucket, "/") {
			t.Fatalf("the source %q was read as bucket %q, key %q", source, in.Bucket, in.Key)
		}
		for _, tag := range in.Conditions.IfMatch {
			// A weak tag is kept in a form no version has.
			tag = strings.TrimSuffix(strings.TrimPrefix(tag, `W/"`), `"`)
			if tag == "" || strings.ContainsAny(tag, `" `) {
				t.Fatalf("the condition %q gave the entity tag %q", tags, tag)
			}
		}
	})
}
