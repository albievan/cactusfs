package s3api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/s3client"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// The deployment the replica bucket "replica" takes its versions from, and the
// time its versions were made there.
const (
	replicaSourceID = "site-a-7f3e"
	replicaMTime    = "2025-03-04T05:06:07.891011121Z"
)

// replicaServer is the test server with a bucket "replica" that is the replica of
// replicaSourceID, a bucket "plain" that keeps versions and is no replica,
// and keys of users for the requests: one that holds the replicate actions
// by name and nothing else, one that may put and delete objects, and one of
// the bucket's administrator.
type replicaServer struct {
	*testServer
	keys map[string]userKey
}

// userKey is the API key of a user.
type userKey struct {
	id     string
	secret []byte
}

func newReplicaServer(t *testing.T) *replicaServer {
	t.Helper()
	ctx := context.Background()
	s := &replicaServer{testServer: newTestServer(t, ""), keys: map[string]userKey{}}
	for _, name := range []string{"replica", "plain"} {
		s.createBucket(name)
		s.mustStatus(s.do(call{method: http.MethodPut, path: "/" + name + "?versioning", body: versioningBody("Enabled")}), http.StatusOK)
	}
	if err := s.meta.SetBucketReplica(ctx, "replica", replicaSourceID, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	root := s.principal()
	policies := map[string]string{
		"replicator": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ReplicateObject","s3:ReplicateDelete"],"Resource":["arn:aws:s3:::replica","arn:aws:s3:::replica/*","arn:aws:s3:::plain","arn:aws:s3:::plain/*"]}]}`,
		"writer":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:DeleteObject","s3:GetObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Resource":["arn:aws:s3:::replica/*","arn:aws:s3:::plain/*"]}]}`,
	}
	roles := map[string]string{"replicator": "replicator", "writer": "writer", "admin": iam.RoleBucketAdmin}
	for name, document := range policies {
		if err := s.iam.PutPolicy(ctx, root, name, []byte(document), true); err != nil {
			t.Fatal(err)
		}
		if err := s.iam.PutRole(ctx, root, iam.Role{Name: name, Policies: []string{name}}, true); err != nil {
			t.Fatal(err)
		}
	}
	for user, role := range roles {
		if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: user, Roles: []string{role}}); err != nil {
			t.Fatal(err)
		}
		creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: user})
		if err != nil {
			t.Fatal(err)
		}
		s.keys[user] = userKey{id: creds.AccessKeyID, secret: []byte(creds.SecretKey)}
	}
	return s
}

// as sends a call signed with the key of a user.
func (s *replicaServer) as(user string, c call) reply {
	s.t.Helper()
	key := s.keys[user]
	c.keyID, c.key = key.id, key.secret
	return s.do(c)
}

// replicaHeadersOf are the headers of the copy of a version: its source,
// ID, sequence and time, and the entity tag of data.
func replicaHeadersOf(id object.ID, seq uint64, data []byte) map[string]string {
	return map[string]string{
		headerReplicaSource: replicaSourceID, headerReplicaVersionID: id.String(),
		headerReplicaSeq: strconv.FormatUint(seq, 10), headerReplicaMTime: replicaMTime, headerReplicaETag: etagOf(data),
	}
}

// newVersionID returns a version ID the source made.
func newVersionID(t *testing.T) object.ID {
	t.Helper()
	var id object.ID
	copy(id[:], random(t, object.IDSize))
	return id
}

// without returns headers without some of them.
func without(headers map[string]string, names ...string) map[string]string {
	out := map[string]string{}
	for name, value := range headers {
		out[name] = value
	}
	for _, name := range names {
		delete(out, name)
	}
	return out
}

// presignPayload is presign for the root key at serverNow, valid for ten
// minutes, with headers that declare the payload in x-amz-content-sha256:
// its signature covers payload, as a presigned request that signs the
// declaration does.
func (s *replicaServer) presignPayload(method, path, payload string, headers map[string]string) string {
	s.t.Helper()
	signed := []string{"host"}
	for name := range headers {
		signed = append(signed, strings.ToLower(name))
	}
	slices.Sort(signed)
	scopeDate := serverNow.Format(scopeDateFormat)
	target := path + "?X-Amz-Algorithm=" + sigV4Algorithm +
		"&X-Amz-Credential=" + url.QueryEscape(s.keyID+"/"+scopeDate+"/"+DefaultRegion+"/s3/aws4_request") +
		"&X-Amz-Date=" + serverNow.Format(amzDateFormat) + "&X-Amz-Expires=600" +
		"&X-Amz-SignedHeaders=" + url.QueryEscape(strings.Join(signed, ";"))
	r, err := http.NewRequestWithContext(s.t.Context(), method, s.URL+target, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	r.Host = r.URL.Host
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	sig := signature{accessKeyID: s.keyID, scopeDate: scopeDate, region: DefaultRegion, signedHeaders: signed, signedHeadersRaw: strings.Join(signed, ";")}
	return target + "&X-Amz-Signature=" + expectedSignature(s.key, sig, serverNow.Format(amzDateFormat), canonicalRequest(r, sig, payload))
}

// addAfterSigning adds a header that the signature does not cover.
func addAfterSigning(name, value string) func(r *http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// TestReplicaPutOverHTTP: a copy is taken only when every replica header is
// signed, its payload is signed, its caller holds s3:ReplicateObject, its
// headers are all there and can be read, and the bucket is the replica of
// its source. A table of refusals, one row for each condition that is
// missing, each answered 403, 400 or 409 with nothing stored.
func TestReplicaPutOverHTTP(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	data := []byte("the content of a version at the source")
	sum := sha256.Sum256(data)
	payload := hex.EncodeToString(sum[:])

	type row struct {
		name   string
		user   string
		path   string
		header func(headers map[string]string) map[string]string
		call   func(c *call)
		status int
		code   string
	}
	same := func(h map[string]string) map[string]string { return h }
	rows := []row{
		{name: "a header that is not signed", user: "replicator", header: func(h map[string]string) map[string]string {
			return without(h, headerReplicaSeq)
		}, call: func(c *call) { c.mutate = addAfterSigning(headerReplicaSeq, "42") }, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "an unknown replica header that is not signed", user: "replicator", header: same, call: func(c *call) {
			c.mutate = addAfterSigning(headerReplicaPrefix+"origin", "elsewhere")
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "an unsigned payload", user: "replicator", header: same, call: func(c *call) {
			c.payloadHash = payloadUnsigned
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "an unsigned stream", user: "replicator", header: func(h map[string]string) map[string]string {
			return with(with(h, "Content-Encoding", "aws-chunked"), headerDecodedLen, strconv.Itoa(len(data)))
		}, call: func(c *call) {
			c.payloadHash, c.body = payloadStreamingTrailer, awsChunked(data, 1<<16, "x-amz-checksum-crc32:"+crc32Of(data)+"\r\n")
			c.headers["X-Amz-Trailer"] = "x-amz-checksum-crc32"
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "s3:PutObject", user: "writer", header: same, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "s3:* of the bucket's administrator", user: "admin", header: same, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "the source missing", user: "replicator", header: func(h map[string]string) map[string]string {
			return without(h, headerReplicaSource)
		}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "the entity tag missing", user: "replicator", header: func(h map[string]string) map[string]string {
			return without(h, headerReplicaETag)
		}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a sequence that cannot be read", user: "replicator", header: func(h map[string]string) map[string]string {
			return with(h, headerReplicaSeq, "forty-two")
		}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a time that is not UTC", user: "replicator", header: func(h map[string]string) map[string]string {
			return with(h, headerReplicaMTime, "2025-03-04T07:06:07+02:00")
		}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "an unknown replica header", user: "replicator", header: func(h map[string]string) map[string]string {
			return with(h, headerReplicaPrefix+"origin", "elsewhere")
		}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "another content", user: "replicator", header: func(h map[string]string) map[string]string {
			return with(h, headerReplicaETag, etagOf([]byte("another")))
		}, status: http.StatusBadRequest, code: "BadDigest"},
		{name: "a bucket that is no replica", user: "replicator", path: "/plain/k", header: same, status: http.StatusConflict, code: "InvalidBucketState"},
		{name: "the replica of another source", user: "replicator", header: func(h map[string]string) map[string]string {
			return with(h, headerReplicaSource, "site-c")
		}, status: http.StatusConflict, code: "InvalidBucketState"},
	}
	for _, r := range rows {
		id := newVersionID(t)
		path := "/replica/k"
		if r.path != "" {
			path = r.path
		}
		c := call{method: http.MethodPut, path: path, body: data, headers: r.header(replicaHeadersOf(id, 42, data))}
		if r.call != nil {
			r.call(&c)
		}
		shards := len(s.drive.IDs())
		got := s.as(r.user, c)
		if status, code := answer(got); status != r.status || code != r.code {
			t.Errorf("%s: %d %s, want %d %s", r.name, status, code, r.status, r.code)
		}
		bucketName, key, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if _, err := s.meta.GetVersion(context.Background(), bucketName, key, object.VersionRef{ID: id}); !errors.Is(err, domain.ErrVersionNotFound) && !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("%s: the version was stored (%v)", r.name, err)
		}
		if n := len(s.drive.IDs()); n != shards {
			t.Errorf("%s: %d shards after the refusal, %d before", r.name, n, shards)
		}
	}

	// A presigned URL is refused, though it signs every header and the
	// payload: its holder chooses when it is used.
	id := newVersionID(t)
	headers := with(replicaHeadersOf(id, 42, data), headerContentSHA, payload)
	presigned := s.presignPayload(http.MethodPut, "/replica/k", payload, headers)
	if status, code := answer(s.do(call{method: http.MethodPut, path: presigned, body: data, unsigned: true, headers: headers})); status != http.StatusForbidden || code != "AccessDenied" {
		t.Errorf("a presigned copy: %d %s, want 403 AccessDenied", status, code)
	}

	// Every condition met: stored as its source has it.
	got := s.as("replicator", call{method: http.MethodPut, path: "/replica/k", body: data, headers: replicaHeadersOf(id, 42, data)})
	s.mustStatus(got, http.StatusOK)
	if got.header.Get(headerVersionID) != id.String() || got.header.Get("ETag") != etagOf(data) {
		t.Errorf("the answer to a copy = %v", got.header)
	}
	stored, err := s.meta.GetVersion(context.Background(), "replica", "k", object.VersionRef{ID: id})
	if err != nil || stored.Seq != 42 || stored.ModifiedAt.Format(time.RFC3339Nano) != replicaMTime || stored.Replication != object.ReplicationReplica {
		t.Errorf("the stored copy = %+v, %v", stored, err)
	}
	// The same again: it is there, which the source reads as done.
	if status, code := answer(s.as("replicator", call{method: http.MethodPut, path: "/replica/k", body: data, headers: replicaHeadersOf(id, 42, data)})); status != http.StatusConflict {
		t.Errorf("the copy again: %d %s, want 409", status, code)
	}
	// The status of a copy is said on GET and HEAD.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := s.mustStatus(s.do(call{method: method, path: "/replica/k?versionId=" + id.String()}), http.StatusOK)
		if r.header.Get(headerReplicationStatus) != string(object.ReplicationReplica) || r.header.Get("Last-Modified") != "Tue, 04 Mar 2025 05:06:07 GMT" {
			t.Errorf("%s of a copy: %v", method, r.header)
		}
	}
}

// TestAReplicaBucketTakesNoOrdinaryWrite: a put, an upload and a delete
// into a replica bucket are 409 InvalidBucketState for everybody, the root
// identity too; a version that is no copy says no replication status.
func TestAReplicaBucketTakesNoOrdinaryWrite(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	for _, c := range []call{
		{method: http.MethodPut, path: "/replica/k", body: []byte("mine")},
		{method: http.MethodPost, path: "/replica/k?uploads"},
		{method: http.MethodDelete, path: "/replica/k"},
	} {
		shards := len(s.drive.IDs())
		s.expectError(s.do(c), http.StatusConflict, "InvalidBucketState")
		if n := len(s.drive.IDs()); n != shards {
			t.Errorf("%s %s left %d shards, %d before", c.method, c.path, n, shards)
		}
	}
	s.putObject("/plain/k", []byte("mine"))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if r := s.mustStatus(s.do(call{method: method, path: "/plain/k"}), http.StatusOK); r.header.Get(headerReplicationStatus) != "" {
			t.Errorf("%s of a version no rule is about says %q", method, r.header.Get(headerReplicationStatus))
		}
	}
}

// TestReplicaHeadersOnOtherOperations: the headers of a replica are refused
// on every request that is none, and is no question or later change of a
// source either - a read, a HEAD that names no version, a copy, a part, a
// listing of objects, versions or parts, the delete of a version, a change
// that names no version, a listing of uploads grouped by a delimiter - and
// never passed over.
func TestReplicaHeadersOnOtherOperations(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	version := "versionId=" + strings.Repeat("a", 32)
	for _, c := range []call{
		{method: http.MethodGet, path: "/replica/k"},
		{method: http.MethodGet, path: "/replica/k?" + version},
		{method: http.MethodHead, path: "/replica/k"},
		{method: http.MethodGet, path: "/replica?list-type=2"},
		{method: http.MethodGet, path: "/replica?versions"},
		{method: http.MethodGet, path: "/replica?location"},
		{method: http.MethodGet, path: "/replica?uploads&prefix=k&delimiter=/"},
		{method: http.MethodGet, path: "/replica/k?uploadId=" + strings.Repeat("a", 32)},
		{method: http.MethodGet, path: "/replica/k?tagging&" + version},
		{method: http.MethodGet, path: "/replica/k?retention&" + version},
		{method: http.MethodPut, path: "/replica/k?tagging", body: []byte("<Tagging><TagSet></TagSet></Tagging>")},
		{method: http.MethodPut, path: "/replica/k?legal-hold", body: []byte("<LegalHold><Status>ON</Status></LegalHold>")},
		{method: http.MethodPut, path: "/replica/k?partNumber=1&uploadId=" + strings.Repeat("a", 32), body: []byte("x")},
		{method: http.MethodDelete, path: "/replica/k?" + version},
		{method: http.MethodPut, path: "/replica/k", headers: map[string]string{headerCopySource: "/plain/k"}},
		{method: http.MethodPost, path: "/replica?delete", body: []byte("<Delete></Delete>")},
		{method: http.MethodPut, path: "/replica?versioning", body: versioningBody("Enabled")},
	} {
		c.headers = with(without(c.headers), headerReplicaSource, replicaSourceID)
		// A request that takes no replica header has a code of its own; a
		// listing of uploads takes it, and refuses the delimiter beside it.
		want := codeReplicaHeaderNotTaken
		if strings.Contains(c.path, "delimiter") {
			want = "InvalidArgument"
		}
		status, code := answer(s.as("replicator", c))
		if c.method == http.MethodHead {
			// An answer to HEAD has no body to name the code.
			code = want
		}
		if status != http.StatusBadRequest || code != want {
			t.Errorf("%s %s with a replica header = %d %s, want 400 %s", c.method, c.path, status, code, want)
		}
	}
}

// TestReplicaDeleteOverHTTP: the copy of a delete marker - its source,
// ID, sequence and time, no entity tag - needs s3:ReplicateDelete and the
// conditions of a copy; the marker is made as its source made it.
func TestReplicaDeleteOverHTTP(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	data := []byte("kept")
	kept := newVersionID(t)
	s.mustStatus(s.as("replicator", call{method: http.MethodPut, path: "/replica/k", body: data, headers: replicaHeadersOf(kept, 5, data)}), http.StatusOK)
	marker := func(id object.ID) map[string]string { return without(replicaHeadersOf(id, 6, nil), headerReplicaETag) }
	for _, r := range []struct {
		name    string
		user    string
		path    string
		headers map[string]string
		call    func(c *call)
		status  int
		code    string
	}{
		{name: "the time not signed", user: "replicator", headers: without(marker(newVersionID(t)), headerReplicaMTime), call: func(c *call) {
			c.mutate = addAfterSigning(headerReplicaMTime, replicaMTime)
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "an unsigned payload", user: "replicator", headers: marker(newVersionID(t)), call: func(c *call) {
			c.payloadHash = payloadUnsigned
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "s3:DeleteObject", user: "writer", headers: marker(newVersionID(t)), status: http.StatusForbidden, code: "AccessDenied"},
		{name: "s3:* of the bucket's administrator", user: "admin", headers: marker(newVersionID(t)), status: http.StatusForbidden, code: "AccessDenied"},
		{name: "the time missing", user: "replicator", headers: without(marker(newVersionID(t)), headerReplicaMTime), status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "an entity tag", user: "replicator", headers: replicaHeadersOf(newVersionID(t), 6, nil), status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a version named", user: "replicator", path: "/replica/k?versionId=" + kept.String(), headers: marker(newVersionID(t)), status: http.StatusBadRequest, code: codeReplicaHeaderNotTaken},
		{name: "a bucket that is no replica", user: "replicator", path: "/plain/k", headers: marker(newVersionID(t)), status: http.StatusConflict, code: "InvalidBucketState"},
	} {
		path := "/replica/k"
		if r.path != "" {
			path = r.path
		}
		c := call{method: http.MethodDelete, path: path, headers: r.headers}
		if r.call != nil {
			r.call(&c)
		}
		if status, code := answer(s.as(r.user, c)); status != r.status || code != r.code {
			t.Errorf("%s: %d %s, want %d %s", r.name, status, code, r.status, r.code)
		}
		if current, err := s.meta.GetObject(context.Background(), "replica", "k"); err != nil || current.VersionID != kept {
			t.Errorf("%s: the key's current version = %+v, %v", r.name, current.Ref(), err)
		}
	}

	id := newVersionID(t)
	got := s.mustStatus(s.as("replicator", call{method: http.MethodDelete, path: "/replica/k", headers: marker(id)}), http.StatusNoContent)
	if got.header.Get(headerDeleteMarker) != "true" || got.header.Get(headerVersionID) != id.String() {
		t.Errorf("the answer to the copy of a marker = %v", got.header)
	}
	stored, err := s.meta.GetVersion(context.Background(), "replica", "k", object.VersionRef{ID: id})
	if err != nil || !stored.DeleteMarker || stored.Seq != 6 || stored.Replication != object.ReplicationReplica {
		t.Errorf("the stored marker = %+v, %v", stored, err)
	}
}

// TestReplicaUploadOverHTTP: a version of parts is sent as a multipart
// upload: begun with the source alone, its parts sent as any part, and
// completed with every replica header.
func TestReplicaUploadOverHTTP(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	create := func(headers map[string]string) reply {
		return s.as("replicator", call{method: http.MethodPost, path: "/replica/large?uploads", headers: headers})
	}
	// The source alone; any other header is refused.
	s.expectError(create(map[string]string{headerReplicaSource: replicaSourceID, headerReplicaSeq: "4"}), http.StatusBadRequest, "InvalidArgument")
	s.expectError(s.as("writer", call{method: http.MethodPost, path: "/replica/large?uploads", headers: map[string]string{headerReplicaSource: replicaSourceID}}),
		http.StatusForbidden, "AccessDenied")
	s.expectError(s.as("replicator", call{method: http.MethodPost, path: "/plain/large?uploads", headers: map[string]string{headerReplicaSource: replicaSourceID}}),
		http.StatusConflict, "InvalidBucketState")
	r := s.mustStatus(create(map[string]string{headerReplicaSource: replicaSourceID}), http.StatusOK)
	var begun initiateMultipartUploadResult
	if err := xml.Unmarshal(r.body, &begun); err != nil {
		t.Fatal(err)
	}

	data := []byte("the only part of the version")
	part := s.mustStatus(s.as("replicator", call{method: http.MethodPut, path: "/replica/large?partNumber=1&uploadId=" + begun.UploadID, body: data}), http.StatusOK)
	// A writer sends no part of a copy, lists none and aborts none, though
	// it may abort and list uploads.
	partPath := "/replica/large?partNumber=2&uploadId=" + begun.UploadID
	s.expectError(s.as("writer", call{method: http.MethodPut, path: partPath, body: data}), http.StatusForbidden, "AccessDenied")
	s.expectError(s.as("writer", call{method: http.MethodGet, path: "/replica/large?uploadId=" + begun.UploadID}), http.StatusForbidden, "AccessDenied")
	s.expectError(s.as("writer", call{method: http.MethodDelete, path: "/replica/large?uploadId=" + begun.UploadID}), http.StatusForbidden, "AccessDenied")
	// The source sends no part whose payload is not signed, and no part
	// that is the copy of another object.
	s.expectError(s.as("replicator", call{method: http.MethodPut, path: partPath, body: data, payloadHash: payloadUnsigned}), http.StatusForbidden, "AccessDenied")
	s.putObject("/plain/source", data)
	s.expectError(s.do(call{method: http.MethodPut, path: partPath, headers: map[string]string{headerCopySource: "/plain/source"}}), http.StatusBadRequest, "InvalidArgument")
	// None of the three refusals left a part.
	if r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/replica/large?uploadId=" + begun.UploadID}), http.StatusOK); strings.Contains(string(r.body), "<PartNumber>2</PartNumber>") {
		t.Errorf("a refused part was kept: %s", r.body)
	}

	body := []byte(fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, part.header.Get("ETag")))
	digest, err := hex.DecodeString(strings.Trim(part.header.Get("ETag"), `"`))
	if err != nil {
		t.Fatal(err)
	}
	sum := newETagHash()
	sum.Write(digest)
	etag := hex.EncodeToString(sum.Sum(nil)) + "-1"
	id := newVersionID(t)
	headers := with(replicaHeadersOf(id, 99, nil), headerReplicaETag, etag)
	complete := "/replica/large?uploadId=" + begun.UploadID
	// Another entity tag: refused; the upload is still there.
	s.expectError(s.as("replicator", call{method: http.MethodPost, path: complete, body: body, headers: with(headers, headerReplicaETag, "0123456789abcdef0123456789abcdef-1")}),
		http.StatusBadRequest, "BadDigest")
	// Not signed: refused.
	if status, code := answer(s.as("replicator", call{
		method: http.MethodPost, path: complete, body: body, headers: without(headers, headerReplicaSeq),
		mutate: addAfterSigning(headerReplicaSeq, "99"),
	})); status != http.StatusForbidden || code != "AccessDenied" {
		t.Errorf("a completion with a header not signed: %d %s", status, code)
	}
	// Without the headers of a copy: the upload of a copy is not completed.
	s.expectError(s.do(call{method: http.MethodPost, path: complete, body: body}), http.StatusConflict, "InvalidBucketState")

	done := s.mustStatus(s.as("replicator", call{method: http.MethodPost, path: complete, body: body, headers: headers}), http.StatusOK)
	if done.header.Get(headerVersionID) != id.String() {
		t.Errorf("the answer to the completion of a copy = %v", done.header)
	}
	head := s.mustStatus(s.do(call{method: http.MethodHead, path: "/replica/large"}), http.StatusOK)
	if head.header.Get(headerReplicationStatus) != string(object.ReplicationReplica) || head.header.Get("ETag") != `"`+etag+`"` {
		t.Errorf("HEAD of the copy = %v", head.header)
	}
}

// TestParseReplicaHeaders: what each kind of request takes, read exactly.
func TestParseReplicaHeaders(t *testing.T) {
	t.Parallel()
	id := "0123456789abcdef0123456789abcdef"
	full := map[string]string{
		headerReplicaSource: "site-a", headerReplicaVersionID: id, headerReplicaSeq: "42",
		headerReplicaMTime: "2025-03-04T05:06:07.5Z", headerReplicaETag: `"` + strings.Repeat("ab", 16) + `"`,
	}
	headerOf := func(values map[string]string) http.Header {
		h := http.Header{}
		for name, value := range values {
			h.Set(name, value)
		}
		return h
	}
	tests := []struct {
		name   string
		kind   replicaKind
		header http.Header
		want   app.ReplicaWrite
		ok     bool
	}{
		{"a put", replicaPut, headerOf(full), app.ReplicaWrite{
			Source: "site-a", Version: object.VersionRef{ID: mustID(t, id)}, Seq: 42,
			ModifiedAt: time.Date(2025, 3, 4, 5, 6, 7, 500_000_000, time.UTC), ETag: strings.Repeat("ab", 16),
		}, true},
		{"the null version", replicaPut, headerOf(with(full, headerReplicaVersionID, "null")), app.ReplicaWrite{
			Source: "site-a", Version: object.VersionRef{Null: true}, Seq: 42,
			ModifiedAt: time.Date(2025, 3, 4, 5, 6, 7, 500_000_000, time.UTC), ETag: strings.Repeat("ab", 16),
		}, true},
		{"a version of parts", replicaComplete, headerOf(with(full, headerReplicaETag, strings.Repeat("cd", 16)+"-10000")), app.ReplicaWrite{
			Source: "site-a", Version: object.VersionRef{ID: mustID(t, id)}, Seq: 42,
			ModifiedAt: time.Date(2025, 3, 4, 5, 6, 7, 500_000_000, time.UTC), ETag: strings.Repeat("cd", 16) + "-10000",
		}, true},
		{"the greatest sequence", replicaPut, headerOf(with(full, headerReplicaSeq, strconv.FormatUint(object.MaxReplicaSeq, 10))), app.ReplicaWrite{
			Source: "site-a", Version: object.VersionRef{ID: mustID(t, id)}, Seq: object.MaxReplicaSeq,
			ModifiedAt: time.Date(2025, 3, 4, 5, 6, 7, 500_000_000, time.UTC), ETag: strings.Repeat("ab", 16),
		}, true},
		{"a marker", replicaMarker, headerOf(without(full, headerReplicaETag)), app.ReplicaWrite{
			Source: "site-a", Version: object.VersionRef{ID: mustID(t, id)}, Seq: 42,
			ModifiedAt: time.Date(2025, 3, 4, 5, 6, 7, 500_000_000, time.UTC),
		}, true},
		{"an upload begun", replicaCreate, headerOf(map[string]string{headerReplicaSource: "site-a"}), app.ReplicaWrite{Source: "site-a"}, true},
		{"a question", replicaAsk, headerOf(map[string]string{headerReplicaSource: "site-a"}), app.ReplicaWrite{Source: "site-a"}, true},
		{"a question with a version", replicaAsk, headerOf(map[string]string{headerReplicaSource: "site-a", headerReplicaVersionID: id}), app.ReplicaWrite{}, false},
		{"a question of no source", replicaAsk, headerOf(map[string]string{headerReplicaSource: "site a"}), app.ReplicaWrite{}, false},
		{"a question with every header", replicaAsk, headerOf(full), app.ReplicaWrite{}, false},

		{"a marker with an entity tag", replicaMarker, headerOf(full), app.ReplicaWrite{}, false},
		{"an upload begun with a sequence", replicaCreate, headerOf(map[string]string{headerReplicaSource: "site-a", headerReplicaSeq: "1"}), app.ReplicaWrite{}, false},
		{"no source", replicaPut, headerOf(without(full, headerReplicaSource)), app.ReplicaWrite{}, false},
		{"no time", replicaPut, headerOf(without(full, headerReplicaMTime)), app.ReplicaWrite{}, false},
		{"a source that is none", replicaPut, headerOf(with(full, headerReplicaSource, "site a")), app.ReplicaWrite{}, false},
		{"a version in upper case", replicaPut, headerOf(with(full, headerReplicaVersionID, strings.ToUpper(id))), app.ReplicaWrite{}, false},
		{"a version of zeros", replicaPut, headerOf(with(full, headerReplicaVersionID, strings.Repeat("0", 32))), app.ReplicaWrite{}, false},
		{"sequence 0", replicaPut, headerOf(with(full, headerReplicaSeq, "0")), app.ReplicaWrite{}, false},
		{"a sequence with a leading zero", replicaPut, headerOf(with(full, headerReplicaSeq, "042")), app.ReplicaWrite{}, false},
		{"a sequence with a sign", replicaPut, headerOf(with(full, headerReplicaSeq, "+42")), app.ReplicaWrite{}, false},
		{"a sequence beyond 2^62", replicaPut, headerOf(with(full, headerReplicaSeq, strconv.FormatUint(object.MaxReplicaSeq+1, 10))), app.ReplicaWrite{}, false},
		{"a sequence beyond 2^64", replicaPut, headerOf(with(full, headerReplicaSeq, "18446744073709551616")), app.ReplicaWrite{}, false},
		{"a time with an offset", replicaPut, headerOf(with(full, headerReplicaMTime, "2025-03-04T05:06:07+00:00")), app.ReplicaWrite{}, false},
		{"a time in lower case", replicaPut, headerOf(with(full, headerReplicaMTime, "2025-03-04t05:06:07z")), app.ReplicaWrite{}, false},
		{"a time that is no time", replicaPut, headerOf(with(full, headerReplicaMTime, "yesterday")), app.ReplicaWrite{}, false},
		{"a time of thirteen fractional digits", replicaPut, headerOf(with(full, headerReplicaMTime, "2025-03-04T05:06:07.1234567891234Z")), app.ReplicaWrite{}, false},
		{"a time with a comma for the point", replicaPut, headerOf(with(full, headerReplicaMTime, "2025-03-04T05:06:07,5Z")), app.ReplicaWrite{}, false},
		{"a time with a point and no digit", replicaPut, headerOf(with(full, headerReplicaMTime, "2025-03-04T05:06:07.Z")), app.ReplicaWrite{}, false},
		{"an entity tag too short", replicaPut, headerOf(with(full, headerReplicaETag, "abcd")), app.ReplicaWrite{}, false},
		{"an entity tag in upper case", replicaPut, headerOf(with(full, headerReplicaETag, strings.Repeat("AB", 16))), app.ReplicaWrite{}, false},
		{"parts of none", replicaPut, headerOf(with(full, headerReplicaETag, strings.Repeat("ab", 16)+"-0")), app.ReplicaWrite{}, false},
		{"too many parts", replicaPut, headerOf(with(full, headerReplicaETag, strings.Repeat("ab", 16)+"-10001")), app.ReplicaWrite{}, false},
		{"a number of parts with a leading zero", replicaPut, headerOf(with(full, headerReplicaETag, strings.Repeat("ab", 16)+"-01")), app.ReplicaWrite{}, false},
		{"an entity tag half quoted", replicaPut, headerOf(with(full, headerReplicaETag, `"`+strings.Repeat("ab", 16))), app.ReplicaWrite{}, false},
		{"an unknown replica header", replicaPut, headerOf(with(full, headerReplicaPrefix+"origin", "x")), app.ReplicaWrite{}, false},
	}
	twice := headerOf(full)
	twice.Add(headerReplicaSeq, "43")
	tests = append(tests, struct {
		name   string
		kind   replicaKind
		header http.Header
		want   app.ReplicaWrite
		ok     bool
	}{"a header given twice", replicaPut, twice, app.ReplicaWrite{}, false})
	// A name written by hand in another case, beside the canonical one or
	// in its place: net/http never gives one.
	for name, header := range map[string]http.Header{
		"a name in another case beside the canonical one": func() http.Header {
			h := headerOf(full)
			h["x-cactusfs-replica-seq"] = []string{"43"}
			return h
		}(),
		"a name in another case alone": func() http.Header {
			h := headerOf(without(full, headerReplicaSource))
			h["X-CACTUSFS-REPLICA-SOURCE"] = []string{"site-a"}
			return h
		}(),
	} {
		got, err := parseReplicaHeaders(header, replicaPut)
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s = %+v, %v; want %v", name, got, err, domain.ErrInvalidArgument)
		}
	}
	for _, tt := range tests {
		got, err := parseReplicaHeaders(tt.header, tt.kind)
		switch {
		case tt.ok && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case !tt.ok && !errors.Is(err, domain.ErrInvalidArgument):
			t.Errorf("%s = %+v, %v; want %v", tt.name, got, err, domain.ErrInvalidArgument)
		case tt.ok && (got.Source != tt.want.Source || got.Version != tt.want.Version || got.Seq != tt.want.Seq ||
			!got.ModifiedAt.Equal(tt.want.ModifiedAt) || got.ETag != tt.want.ETag):
			t.Errorf("%s = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

// clientHeaders writes the replica headers of a version as package
// s3client writes them: its names, the ID or "null", the sequence in
// decimal, the time in s3client.MTimeFormat, the entity tag unquoted - and
// the entity tag only for a kind that takes one.
func clientHeaders(source string, version object.VersionRef, seq uint64, mtime time.Time, etag string, kind replicaKind) http.Header {
	h := http.Header{}
	h.Set(s3client.HeaderReplicaSource, source)
	if kind == replicaCreate || kind == replicaAsk {
		return h
	}
	id := object.NullVersion
	if !version.Null {
		id = version.ID.String()
	}
	h.Set(s3client.HeaderReplicaVersionID, id)
	h.Set(s3client.HeaderReplicaSeq, strconv.FormatUint(seq, 10))
	h.Set(s3client.HeaderReplicaMTime, mtime.UTC().Format(s3client.MTimeFormat))
	if kind != replicaMarker {
		h.Set(s3client.HeaderReplicaETag, etag)
	}
	return h
}

// TestHeadersAsTheClientWritesThem: the two sides of a replica request
// cannot drift. The names s3client sends are the ones read here, in the
// canonical form the parser takes; the times it writes in MTimeFormat - a
// whole second, one nanosecond, the last nanosecond of a second, the first
// and last days RFC 3339 can carry - read back to the nanosecond; "null"
// is the null version; the sequences 1 and 2^62 are taken.
func TestHeadersAsTheClientWritesThem(t *testing.T) {
	t.Parallel()
	for client, server := range map[string]string{
		s3client.HeaderReplicaSource: headerReplicaSource, s3client.HeaderReplicaVersionID: headerReplicaVersionID,
		s3client.HeaderReplicaSeq: headerReplicaSeq, s3client.HeaderReplicaMTime: headerReplicaMTime,
		s3client.HeaderReplicaETag: headerReplicaETag,
	} {
		if strings.ToLower(client) != server || http.CanonicalHeaderKey(client) != client {
			t.Errorf("the client sends %q, the server reads %q", client, server)
		}
	}
	id := mustID(t, "0123456789abcdef0123456789abcdef")
	etag := strings.Repeat("ab", 16)
	tests := []struct {
		name    string
		kind    replicaKind
		version object.VersionRef
		seq     uint64
		mtime   time.Time
		etag    string
	}{
		{"a put", replicaPut, object.VersionRef{ID: id}, 42, time.Date(2025, 3, 4, 5, 6, 7, 891011121, time.UTC), etag},
		{"a whole second", replicaPut, object.VersionRef{ID: id}, 1, time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC), etag},
		{"one nanosecond", replicaPut, object.VersionRef{ID: id}, 1, time.Date(2025, 3, 4, 5, 6, 7, 1, time.UTC), etag},
		{"the last nanosecond", replicaPut, object.VersionRef{ID: id}, 1, time.Date(2025, 3, 4, 5, 6, 7, 999_999_999, time.UTC), etag},
		{"a time not in UTC", replicaPut, object.VersionRef{ID: id}, 1, time.Date(2025, 3, 4, 5, 6, 7, 10, time.FixedZone("x", 7200)), etag},
		{"the first day", replicaPut, object.VersionRef{ID: id}, 1, time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC), etag},
		{"the last day", replicaPut, object.VersionRef{ID: id}, 1, time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC), etag},
		{"the null version", replicaPut, object.VersionRef{Null: true}, 7, time.Date(2025, 3, 4, 5, 6, 7, 5, time.UTC), etag},
		{"the greatest sequence", replicaPut, object.VersionRef{ID: id}, object.MaxReplicaSeq, time.Date(2025, 3, 4, 5, 6, 7, 5, time.UTC), etag},
		{"a completion", replicaComplete, object.VersionRef{ID: id}, 9, time.Date(2025, 3, 4, 5, 6, 7, 5, time.UTC), etag + "-10000"},
		{"a marker", replicaMarker, object.VersionRef{ID: id}, 9, time.Date(2025, 3, 4, 5, 6, 7, 5, time.UTC), ""},
		{"an upload begun", replicaCreate, object.VersionRef{}, 0, time.Time{}, ""},
		{"a question", replicaAsk, object.VersionRef{}, 0, time.Time{}, ""},
	}
	for _, tt := range tests {
		got, err := parseReplicaHeaders(clientHeaders("site-a", tt.version, tt.seq, tt.mtime, tt.etag, tt.kind), tt.kind)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got.Source != "site-a" || got.Version != tt.version || got.Seq != tt.seq || !got.ModifiedAt.Equal(tt.mtime) || got.ETag != tt.etag {
			t.Errorf("%s = %+v", tt.name, got)
		}
	}
}

// FuzzHeadersAsTheClientWritesThem: any version the client may send - a
// sequence from 1 to 2^62, a time from year 1 to 9999 to the nanosecond,
// an ID or the null version - is read back exactly as it was written.
func FuzzHeadersAsTheClientWritesThem(f *testing.F) {
	f.Add(uint64(1), int64(0), int64(0), false, []byte("0123456789abcdef"))
	f.Add(uint64(object.MaxReplicaSeq), int64(253402300799), int64(999_999_999), true, []byte{})
	f.Add(uint64(42), int64(-62135596800), int64(1), false, []byte{0xff})
	f.Fuzz(func(t *testing.T, seq uint64, seconds, nanos int64, null bool, raw []byte) {
		seq = seq%object.MaxReplicaSeq + 1
		const first, last = -62135596800, 253402300799 // 0001-01-01 and 9999-12-31T23:59:59, UTC
		if seconds < first || seconds > last {
			seconds = first + (seconds%(last-first)+(last-first))%(last-first)
		}
		mtime := time.Unix(seconds, (nanos%1e9+1e9)%1e9).UTC()
		if mtime.IsZero() {
			mtime = mtime.Add(time.Nanosecond)
		}
		var id object.ID
		copy(id[:], raw)
		id[0] |= 1
		version := object.VersionRef{ID: id}
		if null {
			version = object.VersionRef{Null: true}
		}
		etag := hex.EncodeToString(id[:16]) + "-3"
		got, err := parseReplicaHeaders(clientHeaders("site-a", version, seq, mtime, etag, replicaComplete), replicaComplete)
		if err != nil {
			t.Fatalf("%d %v %v: %v", seq, mtime, version, err)
		}
		if got.Version != version || got.Seq != seq || !got.ModifiedAt.Equal(mtime) || got.ETag != etag {
			t.Fatalf("read %+v, wrote %d %v %v %s", got, seq, mtime, version, etag)
		}
	})
}

func mustID(t *testing.T, s string) object.ID {
	t.Helper()
	id, err := object.ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// FuzzReplicaHeaders: whatever the headers say, the parser answers a write
// a copy can be or an invalid argument, and never panics; what it takes
// it reads back as it was written.
// It also adds, as mask says, a header of the prefix named by extra (a name
// that is none of the five, or one given a second time), a second value of
// the source, and the source under a name in another case than net/http
// gives it: whichever of these is there, the answer is an invalid argument.
func FuzzReplicaHeaders(f *testing.F) {
	f.Add("site-a", "0123456789abcdef0123456789abcdef", "42", "2025-03-04T05:06:07.891011121Z", `"0123456789abcdef0123456789abcdef-3"`, uint8(0), "origin", "site-b", uint8(0))
	f.Add("site-a", "null", "1", "2025-03-04T05:06:07Z", "0123456789abcdef0123456789abcdef", uint8(1), "SEQ", "", uint8(1))
	f.Add("", "", "", "", "", uint8(2), "", "x", uint8(2))
	f.Add("site a", "ZZ", "-1", "2025-13-04T05:06:07Z", "\"", uint8(3), "a b", "site-a", uint8(4))
	f.Add("site-a", "0123456789abcdef0123456789abcdef", "42", "2025-03-04T05:06:07Z", "0123456789abcdef0123456789abcdef", uint8(0), "etag", "y", uint8(7))
	f.Fuzz(func(t *testing.T, source, version, seq, mtime, etag string, which uint8, extra, second string, mask uint8) {
		kind := replicaKind(which % 5)
		h := http.Header{}
		for name, value := range map[string]string{
			headerReplicaSource: source, headerReplicaVersionID: version, headerReplicaSeq: seq, headerReplicaMTime: mtime, headerReplicaETag: etag,
		} {
			h[http.CanonicalHeaderKey(name)] = []string{value}
		}
		sourceKey := http.CanonicalHeaderKey(headerReplicaSource)
		if mask&1 != 0 {
			// Another name of the prefix, or one of the five again.
			name := http.CanonicalHeaderKey(headerReplicaPrefix + extra)
			h[name] = append(h[name], second)
		}
		if mask&2 != 0 {
			h[sourceKey] = append(h[sourceKey], second)
		}
		if mask&4 != 0 {
			h[strings.ToUpper(headerReplicaSource)] = h[sourceKey]
			delete(h, sourceKey)
		}
		w, err := parseReplicaHeaders(h, kind)
		if mask&7 != 0 {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("with mask %d: %+v, %v; want an invalid argument", mask&7, w, err)
			}
			return
		}
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("an error that is no invalid argument: %v", err)
			}
			return
		}
		// Only the kinds that take every header can take all five.
		if kind != replicaPut && kind != replicaComplete {
			t.Fatalf("a %d took every header", kind)
		}
		if object.ValidateReplicaSource(w.Source) != nil || w.Seq == 0 || w.Seq > object.MaxReplicaSeq ||
			w.ModifiedAt.Location() != time.UTC || strings.Contains(w.ETag, `"`) {
			t.Fatalf("took %+v", w)
		}
		if w.Version.String() != version || strconv.FormatUint(w.Seq, 10) != seq {
			t.Fatalf("read %+v from %q, %q", w, version, seq)
		}
		if again, err := time.Parse(time.RFC3339Nano, mtime); err != nil || !again.Equal(w.ModifiedAt) {
			t.Fatalf("read the time %v from %q", w.ModifiedAt, mtime)
		}
		if _, fraction, ok := strings.Cut(strings.TrimSuffix(mtime, "Z"), "."); strings.Contains(mtime, ",") || (ok && len(fraction) > 9) {
			t.Fatalf("took the time %q", mtime)
		}
	})
}

// TestReplicaInSignedChunks: a copy may be sent in signed chunks, as a
// version too large to be read twice is; a chunk changed on its way is
// refused, and nothing is stored.
func TestReplicaInSignedChunks(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	content := bytes.Repeat([]byte("a copy signed in pieces. "), 4000)
	id := newVersionID(t)
	upload := func(tamper bool) reply {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, s.URL+"/replica/streamed", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = r.URL.Host
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set(headerDecodedLen, strconv.Itoa(len(content)))
		for name, value := range replicaHeadersOf(id, 8, content) {
			r.Header.Set(name, value)
		}
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
		if tamper {
			sent[len(sent)/2] ^= 1
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
	s.expectError(upload(true), http.StatusForbidden, "SignatureDoesNotMatch")
	if n := len(s.drive.IDs()); n != 0 {
		t.Errorf("%d shards after a changed chunk", n)
	}
	got := s.mustStatus(upload(false), http.StatusOK)
	if got.header.Get(headerVersionID) != id.String() {
		t.Errorf("the answer to a copy in chunks = %v", got.header)
	}
	if r := s.mustStatus(s.do(call{method: http.MethodGet, path: "/replica/streamed"}), http.StatusOK); !bytes.Equal(r.body, content) {
		t.Error("the copy is not what was sent in signed chunks")
	}
}

// TestUnsignedReplicasAreAudited: a copy refused because its signature does
// not cover a replica header or its payload is in the audit trail as a
// failed authentication, under the key it was signed with.
func TestUnsignedReplicasAreAudited(t *testing.T) {
	t.Parallel()
	data := []byte("x")
	// One server each: the auditor counts a second failure from the same
	// address as a repeat of the first.
	for reason, c := range map[string]call{
		"ReplicaHeaderNotSigned": {
			method: http.MethodPut, path: "/replica/k", body: data, headers: without(replicaHeadersOf(newVersionID(t), 1, data), headerReplicaSeq),
			mutate: addAfterSigning(headerReplicaSeq, "1"),
		},
		"ReplicaPayloadNotSigned": {method: http.MethodPut, path: "/replica/k", body: data, headers: replicaHeadersOf(newVersionID(t), 1, data), payloadHash: payloadUnsigned},
	} {
		s := newReplicaServer(t)
		s.expectError(s.as("replicator", c), http.StatusForbidden, "AccessDenied")
		s.auditor.Flush(context.Background())
		records, err := s.iamStore.ListAudit(context.Background(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range records {
			found = found || (r.Outcome == audit.Failed && r.AccessKeyID == s.keys["replicator"].id && r.Details["reason"] == reason)
		}
		if !found {
			t.Errorf("no failed authentication for %s in the trail", reason)
		}
	}
}

// question is a question a source asks of the destination (ADR-0026), as
// package s3client asks it.
type question struct {
	name   string
	method string
	// path is of the bucket "replica"; at names it in another bucket.
	path string
}

// at returns the path of q in another bucket.
func (q question) at(bucketName string) string {
	return "/" + bucketName + strings.TrimPrefix(q.path, "/replica")
}

// questions are the five requests of the questions of a source, of the key
// k and its version id.
func questions(id object.ID) []question {
	return []question{
		{"the bucket probe", http.MethodHead, "/replica"},
		{"the versioning probe", http.MethodGet, "/replica?versioning"},
		{"a version", http.MethodHead, "/replica/k?versionId=" + id.String()},
		{"the replica uploads of a key", http.MethodGet, "/replica?uploads&encoding-type=url&max-uploads=100&prefix=k"},
		{"the abort of an upload", http.MethodDelete, "/replica/k?uploadId=" + id.String()},
	}
}

// TestReplicaQuestionsAreRefused: a question of a source is judged as
// s3:ReplicateObject and refused, as a copy is, unless its header is
// signed, its payload is signed, its caller holds the replicate action,
// its header can be read, and the bucket is the replica of exactly that
// source. A table of refusals, one row a missing condition, for each of
// the five questions.
func TestReplicaQuestionsAreRefused(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	id := newVersionID(t)
	source := map[string]string{headerReplicaSource: replicaSourceID}
	rows := []struct {
		name    string
		user    string
		bucket  string
		headers map[string]string
		call    func(c *call)
		status  int
		code    string
	}{
		{name: "the source not signed", user: "replicator", headers: map[string]string{}, call: func(c *call) {
			c.mutate = addAfterSigning(headerReplicaSource, replicaSourceID)
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "an unsigned payload", user: "replicator", headers: source, call: func(c *call) {
			c.payloadHash = payloadUnsigned
		}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "a writer that reads, lists and aborts", user: "writer", headers: source, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "s3:* of the bucket's administrator", user: "admin", headers: source, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "a source that is none", user: "replicator", headers: map[string]string{headerReplicaSource: "site a"}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a sequence besides", user: "replicator", headers: with(source, headerReplicaSeq, "1"), status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a bucket that is no replica", user: "replicator", bucket: "plain", headers: source, status: http.StatusConflict, code: "InvalidBucketState"},
		{name: "the replica of another source", user: "replicator", headers: map[string]string{headerReplicaSource: "site-c"}, status: http.StatusConflict, code: "InvalidBucketState"},
		// The replicate key's policy names no other bucket; the root
		// identity, which holds the replicate actions, is told.
		{name: "a bucket not in the key's policy", user: "replicator", bucket: "missing", headers: source, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "a bucket that is not there", user: "root", bucket: "missing", headers: source, status: http.StatusNotFound, code: "NoSuchBucket"},
	}
	for _, q := range questions(id) {
		for _, r := range rows {
			path := q.path
			if r.bucket != "" {
				path = q.at(r.bucket)
			}
			c := call{method: q.method, path: path, headers: with(r.headers)}
			if r.call != nil {
				r.call(&c)
			}
			var got reply
			if r.user == "root" {
				got = s.do(c)
			} else {
				got = s.as(r.user, c)
			}
			status, code := answer(got)
			if q.method == http.MethodHead {
				// An answer to HEAD has no body to name the code.
				code = r.code
			}
			if status != r.status || code != r.code {
				t.Errorf("%s, %s: %d %s, want %d %s", q.name, r.name, status, code, r.status, r.code)
			}
			if got.header.Get(headerReplicaSource) != "" || got.header.Get("ETag") != "" {
				t.Errorf("%s, %s: a refusal tells %v", q.name, r.name, got.header)
			}
		}
	}
	// Each denial is in the audit trail, as every other is.
	s.auditor.Flush(context.Background())
	records, err := s.iamStore.ListAudit(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{}
	for _, r := range records {
		if r.Outcome == audit.Denied && r.Action == string(iam.ActionReplicateObject) {
			denied[r.AccessKeyID] = true
		}
	}
	if !denied[s.keys["writer"].id] || !denied[s.keys["admin"].id] {
		t.Errorf("the denials of the questions are not in the trail: %v", denied)
	}
}

// TestReplicaQuestionsOverHTTP: what each question answers, and no more. The
// bucket probes answer the versioning and the source, and nothing of a
// listing; HEAD of a version its entity tag and no other header of it, a
// 404 when it is not there and a 405 for a delete marker; the listing only
// the uploads of the key that the source began, page by page; the abort
// only such an upload. The key that answers holds the replicate actions
// alone, and reads, lists and removes nothing with them.
func TestReplicaQuestionsOverHTTP(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	ctx := context.Background()
	source := map[string]string{headerReplicaSource: replicaSourceID}
	ask := func(method, path string) reply {
		return s.as("replicator", call{method: method, path: path, headers: with(source)})
	}

	// The bucket.
	head := s.mustStatus(ask(http.MethodHead, "/replica"), http.StatusOK)
	if head.header.Get(headerReplicaSource) != replicaSourceID {
		t.Errorf("HEAD of the bucket = %v", head.header)
	}
	get := s.mustStatus(ask(http.MethodGet, "/replica?versioning"), http.StatusOK)
	if get.header.Get(headerReplicaSource) != replicaSourceID || !bytes.Contains(get.body, []byte("<Status>Enabled</Status>")) {
		t.Errorf("GET of the versioning = %v %s", get.header, get.body)
	}
	// Without the header the same key is refused both, as any other
	// read of the bucket.
	for _, path := range []string{"/replica", "/replica?versioning"} {
		method := http.MethodGet
		if path == "/replica" {
			method = http.MethodHead
		}
		if status, _ := answer(s.as("replicator", call{method: method, path: path})); status != http.StatusForbidden {
			t.Errorf("%s %s without the source = %d, want 403", method, path, status)
		}
	}

	// A version, a marker, and a version that is not there.
	data := []byte("the content of a version")
	kept := newVersionID(t)
	s.mustStatus(s.as("replicator", call{method: http.MethodPut, path: "/replica/k", body: data, headers: with(
		replicaHeadersOf(kept, 1, data), "X-Amz-Meta-Secret", "not for the source", "Content-Type", "text/plain",
	)}), http.StatusOK)
	marker := newVersionID(t)
	s.mustStatus(s.as("replicator", call{method: http.MethodDelete, path: "/replica/k", headers: without(replicaHeadersOf(marker, 2, nil), headerReplicaETag)}), http.StatusNoContent)
	version := s.mustStatus(ask(http.MethodHead, "/replica/k?versionId="+kept.String()), http.StatusOK)
	if version.header.Get("ETag") != etagOf(data) {
		t.Errorf("HEAD of the version: ETag %q, want %q", version.header.Get("ETag"), etagOf(data))
	}
	for _, name := range []string{
		"Content-Type", "Last-Modified", "X-Amz-Version-Id", "X-Amz-Meta-Secret", "X-Amz-Tagging-Count", "X-Amz-Replication-Status",
		"X-Amz-Server-Side-Encryption", "Accept-Ranges", "X-Amz-Delete-Marker", headerReplicaSource,
	} {
		if version.header.Get(name) != "" {
			t.Errorf("HEAD of the version tells %s: %q", name, version.header.Get(name))
		}
	}
	if len(version.body) != 0 {
		t.Errorf("HEAD of the version has a body of %d bytes", len(version.body))
	}
	if status, _ := answer(ask(http.MethodHead, "/replica/k?versionId="+marker.String())); status != http.StatusMethodNotAllowed {
		t.Errorf("HEAD of a marker = %d, want 405", status)
	}
	if status, _ := answer(ask(http.MethodHead, "/replica/k?versionId="+newVersionID(t).String())); status != http.StatusNotFound {
		t.Errorf("HEAD of a version that is not there = %d, want 404", status)
	}
	if status, _ := answer(ask(http.MethodHead, "/replica/missing?versionId=null")); status != http.StatusNotFound {
		t.Errorf("HEAD of a null version that is not there = %d, want 404", status)
	}
	// A condition or a customer key, which an ordinary HEAD acts on, is
	// refused with the question.
	for name, value := range map[string]string{"If-Match": etagOf(data), "X-Amz-Server-Side-Encryption-Customer-Algorithm": "AES256"} {
		c := call{method: http.MethodHead, path: "/replica/k?versionId=" + kept.String(), headers: with(source, name, value)}
		if status, _ := answer(s.as("replicator", c)); status != http.StatusBadRequest {
			t.Errorf("HEAD of a version with %s = %d, want 400", name, status)
		}
	}
	// The key reads nothing with the replicate actions alone.
	for _, c := range []call{
		{method: http.MethodGet, path: "/replica/k"},
		{method: http.MethodGet, path: "/replica/k?versionId=" + kept.String()},
		{method: http.MethodHead, path: "/replica/k?versionId=" + kept.String()},
		{method: http.MethodGet, path: "/replica?list-type=2"},
		{method: http.MethodGet, path: "/replica?versions"},
		{method: http.MethodGet, path: "/replica?uploads"},
		{method: http.MethodGet, path: "/replica/k?tagging&versionId=" + kept.String()},
		{method: http.MethodDelete, path: "/replica/k?versionId=" + kept.String()},
	} {
		if status, _ := answer(s.as("replicator", c)); status != http.StatusForbidden {
			t.Errorf("%s %s by the replicate key = %d, want 403", c.method, c.path, status)
		}
	}

	// Uploads: three of the source for the key, one of another key, one
	// that is no copy and one of another source. The two last are begun
	// in "mixed" before it became the replica of the source.
	begin := func(path string, headers map[string]string, user string) string {
		r := s.mustStatus(s.as(user, call{method: http.MethodPost, path: path, headers: headers}), http.StatusOK)
		var begun initiateMultipartUploadResult
		if err := xml.Unmarshal(r.body, &begun); err != nil {
			t.Fatal(err)
		}
		return begun.UploadID
	}
	var mine []string
	for range 2 {
		mine = append(mine, begin("/replica/k?uploads", with(source), "replicator"))
	}
	// One begun by another identity of this deployment - the root, which
	// holds the replicate actions: the listing does not name it.
	mine = append(mine, begin("/replica/k?uploads", with(source), ""))
	begin("/replica/k2?uploads", with(source), "replicator")
	slices.Sort(mine)

	s.createBucket("mixed")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/mixed?versioning", body: versioningBody("Enabled")}), http.StatusOK)
	ordinary := begin("/mixed/k?uploads", nil, "")
	if err := s.meta.SetBucketReplica(ctx, "mixed", "site-c", audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := s.iam.PutPolicy(ctx, s.principal(), "replicator", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ReplicateObject","s3:ReplicateDelete"],"Resource":["arn:aws:s3:::replica","arn:aws:s3:::replica/*","arn:aws:s3:::plain","arn:aws:s3:::plain/*","arn:aws:s3:::mixed","arn:aws:s3:::mixed/*"]}]}`), false); err != nil {
		t.Fatal(err)
	}
	another := begin("/mixed/k?uploads", map[string]string{headerReplicaSource: "site-c"}, "replicator")
	if err := s.meta.SetBucketReplica(ctx, "mixed", replicaSourceID, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	mixed := begin("/mixed/k?uploads", with(source), "replicator")

	type listing = listMultipartUploadsResult
	list := func(bucketName, query string) listing {
		r := s.mustStatus(ask(http.MethodGet, "/"+bucketName+"?uploads&encoding-type=url&prefix=k"+query), http.StatusOK)
		var l listing
		if err := xml.Unmarshal(r.body, &l); err != nil {
			t.Fatalf("listing %s: %v", r.body, err)
		}
		return l
	}
	ids := func(l listing) []string {
		var out []string
		for _, e := range l.Uploads {
			if e.Key != "k" {
				t.Errorf("an upload of %q is listed", e.Key)
			}
			if caller := s.keys["replicator"].id; e.Initiator.ID != caller || e.Initiator.DisplayName != caller || e.Owner.ID != caller {
				t.Errorf("an upload is listed with initiator %+v and owner %+v, not the caller %s", e.Initiator, e.Owner, caller)
			}
			out = append(out, e.UploadID)
		}
		return out
	}
	if got := ids(list("replica", "")); !slices.Equal(got, mine) {
		t.Errorf("the uploads of k = %v, want %v", got, mine)
	}
	if got := ids(list("mixed", "")); !slices.Equal(got, []string{mixed}) {
		t.Errorf("the uploads of k in mixed = %v, want only %v (not %s, %s)", got, mixed, ordinary, another)
	}
	// Page by page, as the client follows them.
	first := list("replica", "&max-uploads=2")
	if !first.IsTruncated || first.NextKeyMarker != "k" || first.NextUploadIDMarker != mine[1] || !slices.Equal(ids(first), mine[:2]) {
		t.Errorf("the first page = %+v", first)
	}
	second := list("replica", "&max-uploads=2&key-marker=k&upload-id-marker="+mine[1])
	if second.IsTruncated || !slices.Equal(ids(second), mine[2:]) {
		t.Errorf("the second page = %+v", second)
	}
	for query, want := range map[string]int{"&key-marker=a": 3, "&key-marker=k": 0, "&key-marker=l": 0, "&max-uploads=0": 0} {
		if got := len(list("replica", query).Uploads); got != want {
			t.Errorf("listing with %s = %d uploads, want %d", query, got, want)
		}
	}

	// The abort: only an upload of the source, and another is "not
	// there" to it; each that was refused is still there.
	for _, upload := range []string{ordinary, another} {
		if _, code := answer(ask(http.MethodDelete, "/mixed/k?uploadId="+upload)); code != "NoSuchUpload" {
			t.Errorf("the abort of an upload not the source's = %s, want NoSuchUpload", code)
		}
		if _, err := s.meta.GetUpload(ctx, "mixed", "k", mustID(t, upload)); err != nil {
			t.Errorf("an upload not the source's was aborted: %v", err)
		}
	}
	s.mustStatus(ask(http.MethodDelete, "/mixed/k?uploadId="+mixed), http.StatusNoContent)
	if _, code := answer(ask(http.MethodDelete, "/mixed/k?uploadId="+mixed)); code != "NoSuchUpload" {
		t.Errorf("the abort again = %s, want NoSuchUpload", code)
	}
	if got := ids(list("mixed", "")); len(got) != 0 {
		t.Errorf("after the abort: %v", got)
	}
}

// TestReplicaChangesOverHTTP: a later change of a version - its tags,
// retention or legal hold - may carry the source; it is then taken only for
// a version named by its ID in the replica of that source, never with a
// bypass, and a refusal by Object Lock is 403 ObjectLocked rather than the
// AccessDenied of a refused key. Without the source it is answered as
// before.
func TestReplicaChangesOverHTTP(t *testing.T) {
	t.Parallel()
	s := newReplicaServer(t)
	ctx := context.Background()
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/locked", headers: map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}}), http.StatusOK)
	if err := s.meta.SetBucketReplica(ctx, "locked", replicaSourceID, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	data := []byte("kept")
	id := newVersionID(t)
	until := serverNow.Add(72 * time.Hour).UTC().Format(time.RFC3339)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/locked/k", body: data, headers: with(replicaHeadersOf(id, 1, data),
		"X-Amz-Object-Lock-Mode", "COMPLIANCE", "X-Amz-Object-Lock-Retain-Until-Date", until)}), http.StatusOK)
	source := map[string]string{headerReplicaSource: replicaSourceID}
	retention := func(until string) []byte {
		return []byte("<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>" + until + "</RetainUntilDate></Retention>")
	}
	version := "versionId=" + id.String()
	sooner := serverNow.Add(time.Hour).UTC().Format(time.RFC3339)
	later := serverNow.Add(96 * time.Hour).UTC().Format(time.RFC3339)
	for _, r := range []struct {
		name    string
		method  string
		path    string
		body    []byte
		headers map[string]string
		status  int
		code    string
	}{
		{"a retention shortened by a source", http.MethodPut, "/locked/k?retention&" + version, retention(sooner), source, http.StatusForbidden, "ObjectLocked"},
		{"a retention removed by a source", http.MethodPut, "/locked/k?retention&" + version, []byte("<Retention></Retention>"), source, http.StatusForbidden, "ObjectLocked"},
		{"a retention shortened by anybody else", http.MethodPut, "/locked/k?retention&" + version, retention(sooner), nil, http.StatusForbidden, "AccessDenied"},
		{"a bypass with the source", http.MethodPut, "/locked/k?retention&" + version, retention(later), with(source, "X-Amz-Bypass-Governance-Retention", "true"), http.StatusBadRequest, "InvalidArgument"},
		{"tags in a bucket that is no replica", http.MethodPut, "/plain/k?tagging&" + version, []byte("<Tagging><TagSet></TagSet></Tagging>"), source, http.StatusConflict, "InvalidBucketState"},
		{
			"a legal hold of another source", http.MethodPut, "/locked/k?legal-hold&" + version, []byte("<LegalHold><Status>ON</Status></LegalHold>"),
			map[string]string{headerReplicaSource: "site-c"},
			http.StatusConflict, "InvalidBucketState",
		},
		{"tags removed with the source not signed", http.MethodDelete, "/locked/k?tagging&" + version, nil, nil, http.StatusForbidden, "AccessDenied"},
		{"a retention extended by a source", http.MethodPut, "/locked/k?retention&" + version, retention(later), source, http.StatusOK, ""},
		{"a legal hold put by a source", http.MethodPut, "/locked/k?legal-hold&" + version, []byte("<LegalHold><Status>ON</Status></LegalHold>"), source, http.StatusOK, ""},
		{"tags put by a source", http.MethodPut, "/locked/k?tagging&" + version, []byte("<Tagging><TagSet><Tag><Key>a</Key><Value>b</Value></Tag></TagSet></Tagging>"), source, http.StatusOK, ""},
		{"tags removed by a source", http.MethodDelete, "/locked/k?tagging&" + version, nil, source, http.StatusNoContent, ""},
	} {
		c := call{method: r.method, path: r.path, body: r.body, headers: with(r.headers)}
		if r.name == "tags removed with the source not signed" {
			c.mutate = addAfterSigning(headerReplicaSource, replicaSourceID)
		}
		got := s.do(c)
		if status, code := answer(got); status != r.status || code != r.code {
			t.Errorf("%s: %d %s, want %d %s", r.name, status, code, r.status, r.code)
		}
	}
	stored, err := s.meta.GetVersion(ctx, "locked", "k", object.VersionRef{ID: id})
	if err != nil || stored.Lock.Retention == nil || stored.Lock.Retention.Until.Format(time.RFC3339) != later || !stored.Lock.LegalHold || len(stored.Tags) != 0 {
		t.Errorf("the version after the changes = %+v, %v", stored.Lock, err)
	}
}
