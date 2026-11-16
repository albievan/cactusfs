package s3client

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/sigv4"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestClassOf: what each status and code of an answer is called, for a
// copy (a put, a marker, a completion, a probe), a later change of a
// version, and a change of its retention. Only what is of the version
// itself is rejected; every other refusal holds the target.
func TestClassOf(t *testing.T) {
	t.Parallel()
	const all = -1
	tests := []struct {
		status int
		code   string
		kind   callKind
		want   delivery.Class
	}{
		{200, "", all, delivery.ClassNone},
		{204, "", all, delivery.ClassNone},
		{301, "", all, delivery.ClassRedirect},
		{307, "", all, delivery.ClassRedirect},
		{401, "", all, delivery.ClassAuth},
		{403, "", all, delivery.ClassAuth},
		{403, "AccessDenied", all, delivery.ClassAuth},
		{403, "InvalidAccessKeyId", all, delivery.ClassAuth},
		{403, "SignatureDoesNotMatch", all, delivery.ClassAuth},
		{403, "RequestTimeTooSkewed", all, delivery.ClassAuth},
		{403, "QuotaExceeded", all, delivery.ClassQuota},
		{400, "AuthorizationHeaderMalformed", all, delivery.ClassAuth},
		// Of the version itself: rejected, whatever the call.
		{400, "EntityTooLarge", all, delivery.ClassRejected},
		{413, "", all, delivery.ClassRejected},
		{413, "EntityTooLarge", all, delivery.ClassRejected},
		{400, "KeyTooLongError", all, delivery.ClassRejected},
		{400, "InvalidTag", all, delivery.ClassRejected},
		{400, "MetadataTooLarge", all, delivery.ClassRejected},
		// Of the request, the pair or the target: held.
		{400, "InvalidRequest", all, delivery.ClassServer},
		{400, "XAmzContentSHA256Mismatch", all, delivery.ClassServer},
		{400, "BadDigest", all, delivery.ClassServer},
		{400, "IncompleteBody", all, delivery.ClassServer},
		{400, "RequestTimeout", all, delivery.ClassServer},
		{400, "MalformedXML", all, delivery.ClassServer},
		{400, "", all, delivery.ClassServer},
		{405, "MethodNotAllowed", all, delivery.ClassServer},
		{412, "PreconditionFailed", all, delivery.ClassServer},
		// InvalidArgument: of a retention, which the other side judges by
		// its own clock, rejected; of anything else held (the cases
		// ADR-0026 names are refused before sending).
		{400, "InvalidArgument", kindCopy, delivery.ClassServer},
		{400, "InvalidArgument", kindChange, delivery.ClassServer},
		{400, "InvalidArgument", kindRetention, delivery.ClassRejected},
		// A key or a version that is not there: rejected for a later
		// change of it, held for a copy.
		{404, "NoSuchKey", kindCopy, delivery.ClassServer},
		{404, "NoSuchVersion", kindCopy, delivery.ClassServer},
		{404, "NoSuchKey", kindChange, delivery.ClassRejected},
		{404, "NoSuchVersion", kindChange, delivery.ClassRejected},
		{404, "NoSuchKey", kindRetention, delivery.ClassRejected},
		{404, "NoSuchVersion", kindRetention, delivery.ClassRejected},
		// A refusal by Object Lock of a request of a source: a later
		// change fails alone, without a stall; a copy, never refused so,
		// is held. The AccessDenied a refused key is answered with stays
		// auth.
		{403, "ObjectLocked", kindCopy, delivery.ClassServer},
		{403, "ObjectLocked", kindChange, delivery.ClassRejected},
		{403, "ObjectLocked", kindRetention, delivery.ClassRejected},
		// A request that does not take the replica headers it carries:
		// held whatever the call, a retention's included, where an
		// InvalidArgument would be rejected.
		{400, "ReplicaHeaderNotTaken", all, delivery.ClassServer},
		{404, "NoSuchBucket", all, delivery.ClassNoSuchBucket},
		{404, "NoSuchUpload", all, delivery.ClassServer},
		{404, "", all, delivery.ClassServer},
		{408, "", all, delivery.ClassServer},
		{409, "InvalidBucketState", all, delivery.ClassNotReplica},
		{409, "OperationAborted", all, delivery.ClassServer},
		{409, "", all, delivery.ClassServer},
		{429, "SlowDown", all, delivery.ClassServer},
		{500, "InternalError", all, delivery.ClassServer},
		{501, "NotImplemented", all, delivery.ClassNotReplica},
		{503, "ServiceUnavailable", all, delivery.ClassServer},
		{503, "QuotaExceeded", all, delivery.ClassQuota},
		{0, "", all, delivery.ClassServer},
		{999, "", all, delivery.ClassServer},
	}
	for _, tt := range tests {
		kinds := []callKind{tt.kind}
		if tt.kind == all {
			kinds = []callKind{kindCopy, kindChange, kindRetention}
		}
		for _, kind := range kinds {
			if got := classOf(tt.status, tt.code, kind); got != tt.want {
				t.Errorf("classOf(%d, %q, %d) = %q, want %q", tt.status, tt.code, kind, got, tt.want)
			}
		}
	}
}

// decodeChunks reads a body of signed chunks as the other deployment does,
// verifying every signature with the chain, and returns its payload.
func decodeChunks(t *testing.T, wire []byte, chain sigv4.Chain) ([]byte, error) {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(wire))
	var payload []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil || !strings.HasSuffix(line, "\r\n") {
			return nil, fmt.Errorf("a chunk header: %q, %w", line, err)
		}
		sizeText, signature, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), chunkExtension)
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if !ok || err != nil || size < 0 {
			return nil, fmt.Errorf("a chunk header: %q", line)
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(r, data); err != nil || !bytes.HasSuffix(data, []byte("\r\n")) {
			return nil, fmt.Errorf("chunk data: %w", err)
		}
		data = data[:size]
		digest := sha256.Sum256(data)
		if !chain.Verify(sigv4.ChunkAlgorithm, signature, hex.EncodeToString(digest[:])) {
			return nil, errors.New("a chunk signature does not verify")
		}
		payload = append(payload, data...)
		if size == 0 {
			if rest, _ := io.ReadAll(r); len(rest) != 0 {
				return nil, fmt.Errorf("%d bytes after the last chunk", len(rest))
			}
			return payload, nil
		}
	}
}

// FuzzChunkedBody is the property of the chunked encoding: for any payload
// and chunk size, the body is as long as chunkedLength says, its chain
// verifies from the seed, and what it carries is the payload; a body read
// one byte at a time is the same; and a payload of another length is
// refused with ErrShortBody or ErrLongBody.
func FuzzChunkedBody(f *testing.F) {
	f.Add([]byte(""), uint16(1), int8(0))
	f.Add([]byte("hello, world"), uint16(5), int8(0))
	f.Add(bytes.Repeat([]byte{0xa5}, 3000), uint16(1024), int8(1))
	f.Add(bytes.Repeat([]byte{0x5a}, 4096), uint16(1024), int8(-1))
	f.Fuzz(func(t *testing.T, payload []byte, chunk uint16, delta int8) {
		chunkSize := int(chunk%4096) + 1
		key := sigv4.SigningKey([]byte("secret"), "20261116", "us-east-1")
		seed := sigv4.Chain{Key: key, Timestamp: "20261116T090000Z", Scope: sigv4.Scope("20261116", "us-east-1"), Previous: strings.Repeat("0", 64)}
		size := int64(len(payload))
		if delta != 0 {
			size = max(0, size+int64(delta))
		}

		chain := seed
		chain.Key = append([]byte(nil), key...)
		body := newChunkedBody(bytes.NewReader(payload), size, chain, chunkSize)
		wire, err := io.ReadAll(body)
		switch {
		case size < int64(len(payload)):
			if !errors.Is(err, ErrLongBody) || !errors.Is(body.failure(), ErrLongBody) {
				t.Fatalf("a longer payload: %v", err)
			}
			return
		case size > int64(len(payload)):
			if !errors.Is(err, ErrShortBody) || !errors.Is(body.failure(), ErrShortBody) {
				t.Fatalf("a shorter payload: %v", err)
			}
			return
		case err != nil:
			t.Fatalf("ReadAll: %v", err)
		}
		if int64(len(wire)) != chunkedLength(size, chunkSize) {
			t.Fatalf("%d bytes on the wire, chunkedLength says %d", len(wire), chunkedLength(size, chunkSize))
		}
		verify := seed
		got, err := decodeChunks(t, wire, verify)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("decoded %d bytes, %v", len(got), err)
		}
		if !bytes.Equal(chain.Key, make([]byte, len(chain.Key))) {
			t.Errorf("the signing key is not cleared after the last chunk")
		}

		// One byte at a time.
		chain.Key = append([]byte(nil), key...)
		slow := newChunkedBody(bytes.NewReader(payload), size, chain, chunkSize)
		var one [1]byte
		var again []byte
		for {
			n, err := slow.Read(one[:])
			again = append(again, one[:n]...)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(again, wire) {
			t.Errorf("read a byte at a time, the body differs")
		}
		if err := slow.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := slow.Read(one[:]); !errors.Is(err, errClosed) {
			t.Errorf("a read after Close: %v", err)
		}
	})
}

// TestChunkedBodyClose: a body closed before it is read clears its key and
// reads nothing.
func TestChunkedBodyClose(t *testing.T) {
	t.Parallel()
	chain := sigv4.Chain{Key: []byte{1, 2, 3}}
	body := newChunkedBody(strings.NewReader("abc"), 3, chain, 8)
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chain.Key, []byte{0, 0, 0}) {
		t.Errorf("key after Close = %v", chain.Key)
	}
	if n, err := body.Read(make([]byte, 8)); n != 0 || !errors.Is(err, errClosed) {
		t.Errorf("Read after Close = %d, %v", n, err)
	}
	if body.failure() != nil {
		t.Errorf("a close is no failure of the body")
	}
}

// TestParsers: the documents of answers as the S3 API writes them, and
// what is refused of them.
func TestParsers(t *testing.T) {
	t.Parallel()
	const head = `<?xml version="1.0" encoding="UTF-8"?>`
	if got := parseErrorCode([]byte(head + `<Error><Code>NoSuchBucket</Code><Message>m</Message></Error>`)); got != "NoSuchBucket" {
		t.Errorf("parseErrorCode = %q", got)
	}
	for _, doc := range []string{"", "<Error/>", "<Error><Code>a b</Code></Error>", "<NotError><Code>X</Code></NotError>", "<Error><Code>" + strings.Repeat("A", 65) + "</Code></Error>"} {
		if got := parseErrorCode([]byte(doc)); got != "" {
			t.Errorf("parseErrorCode(%q) = %q", doc, got)
		}
	}

	for doc, want := range map[string]bucket.Versioning{
		`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`: bucket.VersioningEnabled,
		`<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`:                                               bucket.VersioningSuspended,
		`<VersioningConfiguration/>`: bucket.Unversioned,
	} {
		if got, err := parseVersioning([]byte(head + doc)); err != nil || got != want {
			t.Errorf("parseVersioning(%s) = %q, %v", doc, got, err)
		}
	}
	for _, doc := range []string{"", "<VersioningConfiguration><Status>enabled</Status></VersioningConfiguration>", "<Error/>"} {
		if _, err := parseVersioning([]byte(doc)); !errors.Is(err, errDocument) {
			t.Errorf("parseVersioning(%q) = %v", doc, err)
		}
	}

	if id, err := parseInitiate([]byte(`<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>abc</UploadId></InitiateMultipartUploadResult>`)); err != nil || id != "abc" {
		t.Errorf("parseInitiate = %q, %v", id, err)
	}
	for _, doc := range []string{"<InitiateMultipartUploadResult/>", "<InitiateMultipartUploadResult><UploadId>a b</UploadId></InitiateMultipartUploadResult>"} {
		if _, err := parseInitiate([]byte(doc)); !errors.Is(err, errDocument) {
			t.Errorf("parseInitiate(%q) = %v", doc, err)
		}
	}

	page, err := parseListing([]byte(`<ListMultipartUploadsResult><EncodingType>url</EncodingType><IsTruncated>true</IsTruncated>`+
		`<NextKeyMarker>a+key</NextKeyMarker><NextUploadIdMarker>u2</NextUploadIdMarker>`+
		`<Upload><Key>a+key</Key><UploadId>u1</UploadId></Upload><Upload><Key>a%2Bkey</Key><UploadId>u9</UploadId></Upload>`+
		`<Upload><Key>a+key</Key><UploadId>u2</UploadId></Upload></ListMultipartUploadsResult>`), "a key")
	if err != nil || len(page.uploads) != 2 || page.uploads[0] != "u1" || page.uploads[1] != "u2" || !page.truncated ||
		page.nextKey != "a key" || page.nextUpload != "u2" {
		t.Errorf("parseListing = %+v, %v", page, err)
	}
	for _, doc := range []string{
		`<ListMultipartUploadsResult><EncodingType>base64</EncodingType></ListMultipartUploadsResult>`,
		`<ListMultipartUploadsResult><EncodingType>url</EncodingType><Upload><Key>%zz</Key><UploadId>u</UploadId></Upload></ListMultipartUploadsResult>`,
		`<ListMultipartUploadsResult><Upload><Key>k</Key><UploadId></UploadId></Upload></ListMultipartUploadsResult>`,
		`<ListMultipartUploadsResult><IsTruncated>true</IsTruncated></ListMultipartUploadsResult>`,
	} {
		if _, err := parseListing([]byte(doc), "k"); !errors.Is(err, errDocument) {
			t.Errorf("parseListing(%q) = %v", doc, err)
		}
	}

	if _, err := readBounded(strings.NewReader(strings.Repeat("a", 11)), 10); !errors.Is(err, errDocument) {
		t.Errorf("readBounded of a long body = %v", err)
	}
}

// FuzzParsers: no document an answer can carry makes a parser fail other
// than with errDocument, and what is taken from one is what a request can
// carry: a code of letters and digits, upload IDs of printable characters.
func FuzzParsers(f *testing.F) {
	f.Add([]byte(`<Error><Code>NoSuchBucket</Code></Error>`), "k")
	f.Add([]byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), "k")
	f.Add([]byte(`<InitiateMultipartUploadResult><UploadId>abc</UploadId></InitiateMultipartUploadResult>`), "k")
	f.Add([]byte(`<ListMultipartUploadsResult><EncodingType>url</EncodingType><IsTruncated>true</IsTruncated><NextKeyMarker>k</NextKeyMarker><Upload><Key>k</Key><UploadId>u</UploadId></Upload></ListMultipartUploadsResult>`), "k")
	f.Add([]byte(`<ListPartsResult><UploadId>u</UploadId><IsTruncated>true</IsTruncated><NextPartNumberMarker>1</NextPartNumberMarker><Part><PartNumber>1</PartNumber><ETag>"e"</ETag><Size>5</Size></Part></ListPartsResult>`), "u")
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY e "&e;&e;">]><Error><Code>&e;</Code></Error>`), "")
	f.Fuzz(func(t *testing.T, data []byte, key string) {
		if code := parseErrorCode(data); code != "" && !codeWord(code) {
			t.Fatalf("code %q", code)
		}
		if state, err := parseVersioning(data); err != nil {
			if !errors.Is(err, errDocument) {
				t.Fatalf("parseVersioning: %v", err)
			}
		} else if state != bucket.Unversioned && state != bucket.VersioningEnabled && state != bucket.VersioningSuspended {
			t.Fatalf("state %q", state)
		}
		if id, err := parseInitiate(data); err != nil {
			if !errors.Is(err, errDocument) {
				t.Fatalf("parseInitiate: %v", err)
			}
		} else if !validUploadID(id) {
			t.Fatalf("upload ID %q", id)
		}
		if parts, err := parseParts(data, key); err != nil {
			if !errors.Is(err, errDocument) {
				t.Fatalf("parseParts: %v", err)
			}
		} else {
			for _, part := range parts.parts {
				if object.ValidatePartNumber(part.Number) != nil || part.ETag == "" || !printableWord(part.ETag) || part.Size < 0 {
					t.Fatalf("part %+v", part)
				}
			}
		}
		page, err := parseListing(data, key)
		if err != nil {
			if !errors.Is(err, errDocument) {
				t.Fatalf("parseListing: %v", err)
			}
			return
		}
		for _, id := range page.uploads {
			if !validUploadID(id) {
				t.Fatalf("upload ID %q", id)
			}
		}
		if page.truncated && (page.nextKey == "" || (page.nextUpload != "" && !validUploadID(page.nextUpload))) {
			t.Fatalf("a truncated page without where it goes on: %+v", page)
		}
	})
}

// TestEncodeQuery: the query is sent as SigV4 encodes it canonically, so
// that what is signed is what is sent.
func TestEncodeQuery(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{{"uploads", ""}, {"prefix", "a b+c/é"}, {"max-uploads", "100"}}
	got := encodeQuery(pairs)
	if got != "uploads&prefix=a%20b%2Bc%2F%C3%A9&max-uploads=100" {
		t.Errorf("encodeQuery = %q", got)
	}
	if canonical := sigv4.CanonicalQuery(got); canonical != "max-uploads=100&prefix=a%20b%2Bc%2F%C3%A9&uploads=" {
		t.Errorf("CanonicalQuery = %q", canonical)
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/b?"+got, http.NoBody)
	if err != nil || r.URL.Query().Get("prefix") != "a b+c/é" {
		t.Errorf("the server reads prefix %q, %v", r.URL.Query().Get("prefix"), err)
	}
}

// TestHeaderChecks: what a header may hold and be named.
func TestHeaderChecks(t *testing.T) {
	t.Parallel()
	for value, ok := range map[string]bool{"": true, "a b\tc": true, "é": true, "a\r\nb": false, "a\x00": false, "\x7f": false} {
		if headerValue(value) != ok {
			t.Errorf("headerValue(%q) = %v", value, !ok)
		}
	}
	for name, ok := range map[string]bool{"X-Amz-Meta-A_b": true, "a.b~c": true, "": false, "a b": false, "a:b": false, "é": false} {
		if headerName(name) != ok {
			t.Errorf("headerName(%q) = %v", name, !ok)
		}
	}
}

// emptyReader answers every read with nothing and no error, after it has
// given its bytes.
type emptyReader struct {
	data  []byte
	reads int
}

// Read implements io.Reader.
func (e *emptyReader) Read(p []byte) (int, error) {
	e.reads++
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

// TestChunkedBodyEmptyReads: a src that gives nothing and no error, within
// a chunk or at its end, fails with io.ErrNoProgress after maxEmptyReads
// reads instead of being asked forever; the key is cleared.
func TestChunkedBodyEmptyReads(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		data []byte
		size int64
	}{
		{"within a chunk", []byte("ab"), 4},
		{"at the end", []byte("abcd"), 4},
	} {
		src := &emptyReader{data: tt.data}
		chain := sigv4.Chain{Key: []byte{1, 2, 3}}
		body := newChunkedBody(src, tt.size, chain, 8)
		_, err := io.ReadAll(body)
		if !errors.Is(err, io.ErrNoProgress) || !errors.Is(body.failure(), io.ErrNoProgress) {
			t.Errorf("%s: %v", tt.name, err)
		}
		if src.reads > maxEmptyReads+2 {
			t.Errorf("%s: %d reads", tt.name, src.reads)
		}
		if !bytes.Equal(chain.Key, []byte{0, 0, 0}) {
			t.Errorf("%s: the key is not cleared", tt.name)
		}
	}
}

// TestChunkedBodyRelease: release ends a body and clears its key; a read
// after it reads nothing of src.
func TestChunkedBodyRelease(t *testing.T) {
	t.Parallel()
	src := &emptyReader{data: []byte("abc")}
	chain := sigv4.Chain{Key: []byte{1, 2, 3}}
	body := newChunkedBody(src, 3, chain, 8)
	body.release()
	if n, err := body.Read(make([]byte, 8)); n != 0 || !errors.Is(err, errClosed) || src.reads != 0 {
		t.Errorf("Read after release = %d, %v, %d reads of src", n, err, src.reads)
	}
	if !bytes.Equal(chain.Key, []byte{0, 0, 0}) {
		t.Errorf("the key is not cleared")
	}
}
