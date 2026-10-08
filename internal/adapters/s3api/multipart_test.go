package s3api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/object"
)

func randomPart(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// createUpload begins a multipart upload and returns its ID.
func (s *testServer) createUpload(path string, headers map[string]string) string {
	s.t.Helper()
	r := s.mustStatus(s.do(call{method: http.MethodPost, path: path + "?uploads", headers: headers}), http.StatusOK)
	var result initiateMultipartUploadResult
	if err := xml.Unmarshal(r.body, &result); err != nil || len(result.UploadID) != 2*object.IDSize {
		s.t.Fatalf("CreateMultipartUpload answered %s (%v)", r.body, err)
	}
	return result.UploadID
}

func (s *testServer) uploadPart(path, uploadID string, number int, data []byte) string {
	s.t.Helper()
	r := s.mustStatus(s.do(call{method: http.MethodPut, path: fmt.Sprintf("%s?partNumber=%d&uploadId=%s", path, number, uploadID), body: data}), http.StatusOK)
	etag := r.header.Get("ETag")
	if etag != etagOf(data) {
		s.t.Fatalf("UploadPart ETag = %s, want the MD5 of the part", etag)
	}
	return etag
}

func completeBody(parts ...string) []byte {
	var b strings.Builder
	b.WriteString(`<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, p := range parts {
		b.WriteString("<Part>" + p + "</Part>")
	}
	b.WriteString(`</CompleteMultipartUpload>`)
	return []byte(b.String())
}

func listedPart(number int, etag string) string {
	return fmt.Sprintf("<PartNumber>%d</PartNumber><ETag>%s</ETag>", number, xmlEscape(etag))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// TestMultipartUploadOverHTTP: the multipart operations as an S3 client
// sends them.
func TestMultipartUploadOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("media")
	first, second := randomPart(t, object.MinPartSize), randomPart(t, 3000)

	id := s.createUpload("/media/films/one.mp4", map[string]string{"Content-Type": "video/mp4", "X-Amz-Meta-Take": "3"})
	path := "/media/films/one.mp4"
	tagOne := s.uploadPart(path, id, 1, first)
	s.uploadPart(path, id, 2, []byte("to be replaced"))
	tagTwo := s.uploadPart(path, id, 2, second)
	s.uploadPart(path, id, 3, []byte("not listed"))

	// A part with a checksum: verified, echoed, and remembered.
	crc := crc32.ChecksumIEEE(second)
	crcHeader := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc))
	withSum := s.mustStatus(s.do(call{
		method: http.MethodPut, path: path + "?partNumber=2&uploadId=" + id, body: second,
		headers: map[string]string{"x-amz-checksum-crc32": crcHeader},
	}), http.StatusOK)
	if withSum.header.Get("x-amz-checksum-crc32") != crcHeader {
		t.Errorf("UploadPart did not echo the checksum: %v", withSum.header)
	}
	s.expectError(s.do(call{
		method: http.MethodPut, path: path + "?partNumber=2&uploadId=" + id, body: second,
		headers: map[string]string{"x-amz-checksum-crc32": "AAAAAA=="},
	}), http.StatusBadRequest, "BadDigest")

	parts := s.mustStatus(s.do(call{method: http.MethodGet, path: path + "?uploadId=" + id + "&max-parts=2"}), http.StatusOK)
	var listedParts listPartsResult
	if err := xml.Unmarshal(parts.body, &listedParts); err != nil {
		t.Fatalf("ListParts: %v in %s", err, parts.body)
	}
	if len(listedParts.Parts) != 2 || !listedParts.IsTruncated || listedParts.NextPartNumberMarker != 2 || listedParts.MaxParts != 2 ||
		listedParts.Parts[0].ETag != tagOne || listedParts.Parts[0].Size != object.MinPartSize || listedParts.UploadID != id ||
		listedParts.Key != "films/one.mp4" || listedParts.Bucket != "media" || listedParts.StorageClass != "STANDARD" {
		t.Errorf("ListParts = %+v", listedParts)
	}
	rest := s.mustStatus(s.do(call{method: http.MethodGet, path: path + "?uploadId=" + id + "&part-number-marker=2"}), http.StatusOK)
	if !bytes.Contains(rest.body, []byte("<PartNumber>3</PartNumber>")) || bytes.Contains(rest.body, []byte("<PartNumber>2</PartNumber>")) ||
		!bytes.Contains(rest.body, []byte("<IsTruncated>false</IsTruncated>")) {
		t.Errorf("ListParts after part 2 = %s", rest.body)
	}

	uploads := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media?uploads&prefix=films/"}), http.StatusOK)
	var listedUploads listMultipartUploadsResult
	if err := xml.Unmarshal(uploads.body, &listedUploads); err != nil {
		t.Fatalf("ListMultipartUploads: %v in %s", err, uploads.body)
	}
	if len(listedUploads.Uploads) != 1 || listedUploads.Uploads[0].Key != "films/one.mp4" || listedUploads.Uploads[0].UploadID != id ||
		listedUploads.Prefix != "films/" || listedUploads.IsTruncated || listedUploads.Bucket != "media" {
		t.Errorf("ListMultipartUploads = %+v", listedUploads)
	}
	grouped := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media?uploads&delimiter=/"}), http.StatusOK)
	if !bytes.Contains(grouped.body, []byte("<CommonPrefixes><Prefix>films/</Prefix></CommonPrefixes>")) || bytes.Contains(grouped.body, []byte("<Upload>")) {
		t.Errorf("ListMultipartUploads with a delimiter = %s", grouped.body)
	}

	// Nothing is there before the upload is completed.
	s.expectError(s.do(call{method: http.MethodGet, path: path}), http.StatusNotFound, "NoSuchKey")

	// What is not an object is refused.
	for name, tt := range map[string]struct {
		body []byte
		code string
	}{
		"out of order":               {completeBody(listedPart(2, tagTwo), listedPart(1, tagOne)), "InvalidPartOrder"},
		"a part that was not sent":   {completeBody(listedPart(1, tagOne), listedPart(9, tagTwo)), "InvalidPart"},
		"another entity tag":         {completeBody(listedPart(1, tagTwo)), "InvalidPart"},
		"an entity tag that is none": {completeBody(listedPart(1, `"xyz"`)), "InvalidPart"},
		"a small part in the middle": {completeBody(listedPart(2, tagTwo), listedPart(3, etagOf([]byte("not listed")))), "EntityTooSmall"},
		"another checksum":           {completeBody(listedPart(1, tagOne), listedPart(2, tagTwo)+"<ChecksumCRC32>AAAAAA==</ChecksumCRC32>"), "InvalidPart"},
		"a checksum never declared":  {completeBody(listedPart(1, tagOne) + "<ChecksumSHA256>AAAA</ChecksumSHA256>"), "InvalidPart"},
		"no parts":                   {completeBody(), "MalformedXML"},
		"not XML":                    {[]byte("parts: 1, 2"), "MalformedXML"},
		"a part inside a part":       {completeBody(listedPart(1, tagOne) + "<Part>" + listedPart(2, tagTwo) + "</Part>"), "MalformedXML"},
		"two numbers for one part":   {completeBody(listedPart(1, tagOne) + "<PartNumber>2</PartNumber>"), "MalformedXML"},
		"an element not of S3":       {completeBody(listedPart(1, tagOne) + "<Skip>true</Skip>"), "MalformedXML"},
		"part number zero":           {completeBody(listedPart(0, tagOne)), "InvalidArgument"},
	} {
		r := s.do(call{method: http.MethodPost, path: path + "?uploadId=" + id, body: tt.body})
		if code := s.errorCode(r); r.status != http.StatusBadRequest || code != tt.code {
			t.Errorf("completing with %s = %d %s, want 400 %s", name, r.status, code, tt.code)
		}
	}
	// A body the signature does not cover decides nothing.
	s.expectError(s.do(call{
		method: http.MethodPost, path: path + "?uploadId=" + id, payloadHash: "UNSIGNED-PAYLOAD",
		body: completeBody(listedPart(1, tagOne)),
	}), http.StatusBadRequest, "InvalidRequest")
	s.expectError(s.do(call{method: http.MethodGet, path: path}), http.StatusNotFound, "NoSuchKey")

	done := s.mustStatus(s.do(call{
		method: http.MethodPost, path: path + "?uploadId=" + id,
		body: completeBody(listedPart(1, tagOne), listedPart(2, tagTwo)+"<ChecksumCRC32>"+crcHeader+"</ChecksumCRC32>"),
	}), http.StatusOK)
	var result completeMultipartUploadResult
	if err := xml.Unmarshal(done.body, &result); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v in %s", err, done.body)
	}
	// The entity tag of an object made of parts: the MD5 of the MD5s of
	// its parts, and their number.
	sum := newETagHash()
	for _, part := range [][]byte{first, second} {
		raw, _ := hex.DecodeString(strings.Trim(etagOf(part), `"`))
		sum.Write(raw)
	}
	wantTag := `"` + hex.EncodeToString(sum.Sum(nil)) + `-2"`
	if result.ETag != wantTag || result.Bucket != "media" || result.Key != "films/one.mp4" || result.Location != "/media/films/one.mp4" {
		t.Errorf("CompleteMultipartUpload = %+v, want ETag %s", result, wantTag)
	}

	got := s.mustStatus(s.do(call{method: http.MethodGet, path: path}), http.StatusOK)
	if !bytes.Equal(got.body, append(append([]byte{}, first...), second...)) {
		t.Errorf("GET returned %d bytes, want the two parts in order", len(got.body))
	}
	if got.header.Get("ETag") != wantTag || got.header.Get("Content-Type") != "video/mp4" || got.header.Get("X-Amz-Meta-Take") != "3" {
		t.Errorf("GET headers = %v", got.header)
	}
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: path}), http.StatusOK)
	if head.header.Get("ETag") != wantTag || head.header.Get("Content-Length") != fmt.Sprint(object.MinPartSize+3000) {
		t.Errorf("HEAD headers = %v", head.header)
	}
	// A range across the two parts.
	across := s.mustStatus(s.do(call{
		method: http.MethodGet, path: path,
		headers: map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", object.MinPartSize-5, object.MinPartSize+4)},
	}), http.StatusPartialContent)
	if !bytes.Equal(across.body, append(append([]byte{}, first[object.MinPartSize-5:]...), second[:5]...)) {
		t.Errorf("a range across the parts = %d bytes", len(across.body))
	}
	listing := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media?list-type=2"}), http.StatusOK)
	if !bytes.Contains(listing.body, []byte("<ETag>"+xmlEscape(wantTag)+"</ETag>")) {
		t.Errorf("the object is listed as %s", listing.body)
	}

	// The upload is over.
	s.expectError(s.do(call{method: http.MethodGet, path: path + "?uploadId=" + id}), http.StatusNotFound, "NoSuchUpload")
	s.expectError(s.do(call{method: http.MethodPut, path: path + "?partNumber=1&uploadId=" + id, body: []byte("x")}), http.StatusNotFound, "NoSuchUpload")
	s.expectError(s.do(call{method: http.MethodDelete, path: path + "?uploadId=" + id}), http.StatusNotFound, "NoSuchUpload")
	if none := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media?uploads"}), http.StatusOK); bytes.Contains(none.body, []byte("<Upload>")) {
		t.Errorf("uploads after completion = %s", none.body)
	}
	// A client that did not hear the answer and asks again hears it.
	again := s.mustStatus(s.do(call{
		method: http.MethodPost, path: path + "?uploadId=" + id, body: completeBody(listedPart(1, tagOne), listedPart(2, tagTwo)),
	}), http.StatusOK)
	if !bytes.Contains(again.body, []byte(xmlEscape(wantTag))) {
		t.Errorf("completing again = %s", again.body)
	}
}

// TestMultipartRequestsThatAreNotServed: what is not a multipart operation,
// or asks for more than one is, is refused and never served as another.
func TestMultipartRequestsThatAreNotServed(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("media")
	s.putObject("/media/k", []byte("the object"))
	id := s.createUpload("/media/k", nil)
	etag := s.uploadPart("/media/k", id, 1, []byte("part"))
	base := "/media/k?uploadId=" + id

	for name, tt := range map[string]struct {
		c      call
		status int
		code   string
	}{
		"an upload ID that is none":      {call{method: http.MethodPut, path: "/media/k?partNumber=1&uploadId=../../etc", body: []byte("x")}, 404, "NoSuchUpload"},
		"an upload ID of another length": {call{method: http.MethodDelete, path: "/media/k?uploadId=abcd"}, 404, "NoSuchUpload"},
		"an upload of another key":       {call{method: http.MethodGet, path: "/media/other?uploadId=" + id}, 404, "NoSuchUpload"},
		"an upload in another bucket":    {call{method: http.MethodGet, path: "/missing/k?uploadId=" + id}, 404, "NoSuchUpload"},
		"a part without a number":        {call{method: http.MethodPut, path: base, body: []byte("x")}, 400, "InvalidArgument"},
		"a part number that is none":     {call{method: http.MethodPut, path: base + "&partNumber=one", body: []byte("x")}, 400, "InvalidArgument"},
		"part number zero":               {call{method: http.MethodPut, path: base + "&partNumber=0", body: []byte("x")}, 400, "InvalidArgument"},
		"part number 10001":              {call{method: http.MethodPut, path: base + "&partNumber=10001", body: []byte("x")}, 400, "InvalidArgument"},
		"a copy as a part":               {call{method: http.MethodPut, path: base + "&partNumber=2", headers: map[string]string{"x-amz-copy-source": "/media/k"}}, 501, "NotImplemented"},
		"a lock said with a part": {call{
			method: http.MethodPut, path: base + "&partNumber=2", body: []byte("x"),
			headers: map[string]string{headerLockLegalHold: "ON"},
		}, 400, "InvalidArgument"},
		"a lock said at completion": {call{
			method: http.MethodPost, path: base, body: completeBody(listedPart(1, etag)),
			headers: map[string]string{headerLockMode: "COMPLIANCE", headerLockUntil: "2099-01-01T00:00:00Z"},
		}, 400, "InvalidArgument"},
		"a checksum of the whole object, at the beginning": {call{
			method: http.MethodPost, path: "/media/k?uploads",
			headers: map[string]string{"x-amz-checksum-type": "FULL_OBJECT"},
		}, 501, "NotImplemented"},
		"a checksum of the whole object, at completion": {call{
			method: http.MethodPost, path: base, body: completeBody(listedPart(1, etag)),
			headers: map[string]string{"x-amz-checksum-type": "COMPOSITE"},
		}, 501, "NotImplemented"},
		"reading by part number":        {call{method: http.MethodGet, path: "/media/k?partNumber=1"}, 501, "NotImplemented"},
		"an unknown parameter":          {call{method: http.MethodPost, path: base + "&acl"}, 501, "NotImplemented"},
		"uploads with another":          {call{method: http.MethodPost, path: "/media/k?uploads&versionId=null"}, 501, "NotImplemented"},
		"beginning with a body":         {call{method: http.MethodPost, path: "/media/k?uploads", body: []byte("<x/>")}, 400, "InvalidArgument"},
		"beginning with GET":            {call{method: http.MethodGet, path: "/media/k?uploads"}, 405, "MethodNotAllowed"},
		"a post that is neither":        {call{method: http.MethodPost, path: "/media/k"}, 405, "MethodNotAllowed"},
		"max-parts that is none":        {call{method: http.MethodGet, path: base + "&max-parts=-1"}, 400, "InvalidArgument"},
		"a marker that is none":         {call{method: http.MethodGet, path: base + "&part-number-marker=x"}, 400, "InvalidArgument"},
		"max-uploads that is none":      {call{method: http.MethodGet, path: "/media?uploads&max-uploads=many"}, 400, "InvalidArgument"},
		"an upload marker that is none": {call{method: http.MethodGet, path: "/media?uploads&key-marker=k&upload-id-marker=zz"}, 400, "InvalidArgument"},
		"an encoding that is none":      {call{method: http.MethodGet, path: "/media?uploads&encoding-type=base64"}, 400, "InvalidArgument"},
		"uploads of a missing bucket":   {call{method: http.MethodGet, path: "/missing?uploads"}, 404, "NoSuchBucket"},
		"beginning in a missing bucket": {call{method: http.MethodPost, path: "/missing/k?uploads"}, 404, "NoSuchBucket"},
		"a lock without Object Lock": {call{
			method: http.MethodPost, path: "/media/k?uploads",
			headers: map[string]string{headerLockLegalHold: "ON"},
		}, 400, "InvalidRequest"},
		"without credentials": {call{method: http.MethodPost, path: "/media/k?uploads", unsigned: true}, 403, "AccessDenied"},
	} {
		r := s.do(tt.c)
		if code := s.errorCode(r); r.status != tt.status || code != tt.code {
			t.Errorf("%s = %d %s, want %d %s", name, r.status, code, tt.status, tt.code)
		}
	}
	// None of it touched the object or the upload.
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media/k"}), http.StatusOK); string(got.body) != "the object" {
		t.Errorf("the object is now %q", got.body)
	}
	if parts := s.mustStatus(s.do(call{method: http.MethodGet, path: base}), http.StatusOK); !bytes.Contains(parts.body, []byte("<PartNumber>1</PartNumber>")) {
		t.Errorf("the upload is now %s", parts.body)
	}

	// Aborting removes the upload; the object stays.
	s.mustStatus(s.do(call{method: http.MethodDelete, path: base}), http.StatusNoContent)
	s.expectError(s.do(call{method: http.MethodGet, path: base}), http.StatusNotFound, "NoSuchUpload")
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/media/k"}), http.StatusOK); string(got.body) != "the object" {
		t.Errorf("aborting an upload changed the object: %q", got.body)
	}
}

// TestMultipartObjectLockOverHTTP: the lock headers of the request that
// begins an upload keep the object it makes.
func TestMultipartObjectLockOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/vault", headers: map[string]string{headerLockEnabled: "true"}}), http.StatusOK)
	id := s.createUpload("/vault/ledger", map[string]string{headerLockMode: "COMPLIANCE", headerLockUntil: "2099-01-01T00:00:00Z"})
	etag := s.uploadPart("/vault/ledger", id, 1, []byte("entries"))
	done := s.mustStatus(s.do(call{method: http.MethodPost, path: "/vault/ledger?uploadId=" + id, body: completeBody(listedPart(1, etag))}), http.StatusOK)
	version := done.header.Get("x-amz-version-id")
	if version == "" {
		t.Fatalf("no version ID on the completion: %v", done.header)
	}
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/vault/ledger"}), http.StatusOK)
	if head.header.Get(headerLockMode) != "COMPLIANCE" || !strings.HasPrefix(head.header.Get(headerLockUntil), "2099-01-01") {
		t.Errorf("lock headers of the completed object = %v", head.header)
	}
	s.expectError(s.do(call{
		method: http.MethodDelete, path: "/vault/ledger?versionId=" + version, headers: map[string]string{headerLockOverride: "true"},
	}), http.StatusForbidden, "AccessDenied")
	if got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/vault/ledger?versionId=" + version}), http.StatusOK); string(got.body) != "entries" {
		t.Errorf("the locked object reads %q", got.body)
	}
}

func FuzzCompleteMultipartUpload(f *testing.F) {
	f.Add(completeBody(listedPart(1, `"0cc175b9c0f1b6a831c399e269772661"`), listedPart(2, "0cc175b9c0f1b6a831c399e269772661")))
	f.Add(completeBody(listedPart(1, "x") + "<ChecksumCRC32>AAAAAA==</ChecksumCRC32>"))
	f.Add([]byte(`<CompleteMultipartUpload><Part><Part><PartNumber>1</PartNumber></Part></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`<!DOCTYPE a [<!ENTITY b "c">]><CompleteMultipartUpload/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var in completeMultipartUpload
		err := decodeStrictXML(bytes.NewReader(data), maxCompleteXML, &in,
			"CompleteMultipartUpload", "Part*", "PartNumber", "ETag", "ChecksumCRC32", "ChecksumCRC32C", "ChecksumCRC64NVME", "ChecksumSHA256")
		if err != nil {
			return
		}
		if bytes.Contains(data, []byte("<!DOCTYPE")) || bytes.Contains(data, []byte("<!ENTITY")) {
			t.Fatalf("a document with a document type was accepted: %q", data)
		}
		// No more parts are read than the document names.
		if len(in.Parts) > bytes.Count(data, []byte("<Part")) {
			t.Fatalf("%d parts decoded from %q", len(in.Parts), data)
		}
		parts := make([]object.CompletedPart, len(in.Parts))
		for i, p := range in.Parts {
			parts[i] = object.CompletedPart{Number: p.PartNumber, ETag: strings.Trim(strings.TrimSpace(p.ETag), `"`)}
		}
		// The entity tag is formed only from entity tags that are MD5s.
		if etag, err := multipartETag(parts); err == nil {
			for _, p := range parts {
				if raw, decodeErr := hex.DecodeString(p.ETag); decodeErr != nil || len(raw) != 16 {
					t.Fatalf("an entity tag was formed from %q", p.ETag)
				}
			}
			if !strings.HasSuffix(etag, fmt.Sprintf("-%d", len(parts))) {
				t.Fatalf("entity tag %q of %d parts", etag, len(parts))
			}
		}
	})
}
