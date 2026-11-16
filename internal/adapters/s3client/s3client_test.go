package s3client_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/adapters/s3client"
	"github.com/albievan/cactusfs/internal/adapters/sealed"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// newID returns a version ID.
func newID(t *testing.T) object.ID {
	t.Helper()
	id, err := object.NewID(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// etagOf returns the entity tag the site gives content: what it is put as
// by its root into a bucket of its own.
func (s *site) etagOf(content []byte) string {
	s.t.Helper()
	s.makeScratch()
	resp, body := s.root(http.MethodPut, "/scratch/content", nil, content)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("put scratch = %d %s", resp.StatusCode, body)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`)
}

// makeScratch makes the bucket "scratch" of the site's root, which is no
// replica and takes ordinary writes, once.
func (s *site) makeScratch() {
	s.t.Helper()
	s.mu.Lock()
	made := s.scratch
	s.scratch = true
	s.mu.Unlock()
	if !made {
		if resp, body := s.root(http.MethodPut, "/scratch", nil, nil); resp.StatusCode != http.StatusOK {
			s.t.Fatalf("create scratch = %d %s", resp.StatusCode, body)
		}
	}
}

// version returns a version of key in bucketName at this deployment, of
// content, as the replicator hands it to the client.
func (s *site) version(bucketName, key string, content []byte, seq uint64) object.Version {
	s.t.Helper()
	return object.Version{
		Bucket: bucketName, Key: key, VersionID: newID(s.t), Versioned: true, Size: int64(len(content)),
		ETag: s.etagOf(content), ModifiedAt: siteNow.Add(-time.Hour).Add(123456789 * time.Nanosecond),
		Seq: seq, Replica: &object.Replica{From: source, Seq: seq},
	}
}

// failsWith fails the test unless err is of class want; ClassNone is no
// error.
func failsWith(t *testing.T, what string, err error, want delivery.Class) {
	t.Helper()
	if got := delivery.ClassOf(err); got != want {
		t.Errorf("%s: class %q (%v), want %q", what, got, err, want)
	}
	if err != nil && err.Error() != "delivery failed: "+string(delivery.ClassOf(err)) {
		t.Errorf("%s: the text of the error is %q, not its class alone", what, err)
	}
}

// lastFor returns the last request of method whose path is path.
func lastFor(t *testing.T, requests []seen, method, path string) seen {
	t.Helper()
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].method == method && requests[i].path == path {
			return requests[i]
		}
	}
	t.Fatalf("no %s %s among %d requests", method, path, len(requests))
	return seen{}
}

// signedAndBound fails the test unless every header of a request that is
// not of the transport is signed, and its payload is: by its SHA-256, or as
// signed chunks when chunked is set.
func signedAndBound(t *testing.T, r seen, chunked bool) {
	t.Helper()
	signed := signedHeaders(r.header)
	for name := range r.header {
		lower := strings.ToLower(name)
		switch lower {
		case "authorization", "user-agent", "content-length":
			continue
		}
		if !slices.Contains(signed, lower) {
			t.Errorf("%s %s: header %s is not signed (%v)", r.method, r.path, lower, signed)
		}
	}
	payload := r.header.Get("X-Amz-Content-Sha256")
	switch {
	case chunked && payload != "STREAMING-AWS4-HMAC-SHA256-PAYLOAD":
		t.Errorf("%s %s: payload %q, want signed chunks", r.method, r.path, payload)
	case !chunked && len(payload) != 64:
		t.Errorf("%s %s: payload %q, want its SHA-256", r.method, r.path, payload)
	}
}

// TestCheck: HEAD of the bucket and GET of its versioning, each with the
// signed source, answered by the site itself: the deployment the bucket is
// a replica of and its versioning for a bucket that is the replica of this
// source; not_replica for a bucket that is no replica, or another source's,
// whichever of the two questions is refused; no_such_bucket for a bucket
// that is not there.
func TestCheck(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", false)
	s.createBucket("locked", true)
	s.createPlain("plain", false)
	s.createPlain("other", false)
	if _, err := s.replication.SetReplicaMode(t.Context(), porttest.Root(), "other", "site-c"); err != nil {
		t.Fatal(err)
	}
	c := s.client(0)

	tests := []struct {
		bucket string
		want   ports.SiteBucket
		class  delivery.Class
	}{
		{"replica", ports.SiteBucket{Versioning: bucket.VersioningEnabled, ReplicaOf: source}, delivery.ClassNone},
		{"locked", ports.SiteBucket{Versioning: bucket.VersioningEnabled, ReplicaOf: source}, delivery.ClassNone},
		{"plain", ports.SiteBucket{}, delivery.ClassNotReplica},
		{"other", ports.SiteBucket{}, delivery.ClassNotReplica},
		{"missing", ports.SiteBucket{}, delivery.ClassNoSuchBucket},
	}
	for _, tt := range tests {
		got, err := c.Check(t.Context(), tt.bucket)
		failsWith(t, "Check "+tt.bucket, err, tt.class)
		if got != tt.want {
			t.Errorf("Check %s = %+v, want %+v", tt.bucket, got, tt.want)
		}
	}
	requests := s.seen()
	head, get := lastFor(t, requests, http.MethodHead, "/replica"), lastFor(t, requests, http.MethodGet, "/replica")
	for _, r := range []seen{head, get} {
		signedAndBound(t, r, false)
		if r.header.Get(s3client.HeaderReplicaSource) != source {
			t.Errorf("%s: source %q", r.method, r.header.Get(s3client.HeaderReplicaSource))
		}
	}
	if get.query != "versioning" {
		t.Errorf("GET query %q", get.query)
	}
}

// TestCheckOfAnotherService: what a service that does not know replica mode
// - a release before it, or another S3 service - answers: no source, or
// one that is no deployment ID, is none, and nothing would be sent to it.
// A 409 of the GET alone names its code.
func TestCheckOfAnotherService(t *testing.T) {
	t.Parallel()
	versioning := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
	answering := func(replicaOf string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if replicaOf != "" {
				w.Header().Set(s3client.HeaderReplicaSource, replicaOf)
			}
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, versioning)
			}
		}
	}
	tests := []struct {
		name   string
		script http.HandlerFunc
		want   ports.SiteBucket
		class  delivery.Class
	}{
		{"without the source", answering(""), ports.SiteBucket{Versioning: bucket.VersioningEnabled}, delivery.ClassNone},
		{"with no deployment ID", answering("not a deployment"), ports.SiteBucket{Versioning: bucket.VersioningEnabled}, delivery.ClassNone},
		{"with another deployment", answering("site-c"), ports.SiteBucket{Versioning: bucket.VersioningEnabled, ReplicaOf: "site-c"}, delivery.ClassNone},
		{"the GET refused 409", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `<Error><Code>InvalidBucketState</Code></Error>`)
			}
		}, ports.SiteBucket{}, delivery.ClassNotReplica},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t)
			s.set(func(s *site) { s.script = tt.script })
			got, err := s.client(0).Check(t.Context(), "replica")
			failsWith(t, tt.name, err, tt.class)
			if got != tt.want {
				t.Errorf("Check = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestPutReplica: a version crosses with its five replica headers, signed,
// as signed chunks, path-style; the site takes it with its bytes,
// representation headers, metadata, tags and lock; a key of any bytes is
// one path. A version of a size over several chunks, and an empty one, go
// as well.
func TestPutReplica(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", true)
	c := s.client(0)
	s3client.SetChunkSize(c, 8<<10)

	content := bytes.Repeat([]byte("a version, replicated. "), 2000)
	v := s.version("replica", "a key/with+signs!$&'()*,;=:@ é%", content, 4711)
	v.ContentType, v.ContentLanguage, v.CacheControl, v.ContentDisposition = "text/plain", "en", "no-cache", "inline"
	v.UserMetadata = map[string]string{"origin": "site a", "n": "1"}
	v.Tags = map[string]string{"stage": "draft one", "team": "a+b/c"}
	until := siteNow.Add(48 * time.Hour).Truncate(time.Millisecond)
	v.Lock = object.Lock{Retention: &object.Retention{Mode: object.Governance, Until: until}, LegalHold: true}
	if err := c.PutReplica(t.Context(), v, bytes.NewReader(content)); err != nil {
		t.Fatalf("PutReplica: %v (%v)", err, errors.Unwrap(err))
	}

	r := lastFor(t, s.seen(), http.MethodPut, "/replica/"+v.Key)
	signedAndBound(t, r, true)
	want := map[string]string{
		s3client.HeaderReplicaSource:    source,
		s3client.HeaderReplicaVersionID: v.VersionID.String(),
		s3client.HeaderReplicaSeq:       "4711",
		s3client.HeaderReplicaMTime:     v.ModifiedAt.UTC().Format(s3client.MTimeFormat),
		s3client.HeaderReplicaETag:      v.ETag,
		"X-Amz-Decoded-Content-Length":  strconv.Itoa(len(content)),
		"Content-Encoding":              "aws-chunked",
		"X-Amz-Tagging":                 "stage=draft+one&team=a%2Bb%2Fc",
		"X-Amz-Object-Lock-Mode":        "GOVERNANCE",
		"X-Amz-Object-Lock-Legal-Hold":  "ON",
	}
	for name, value := range want {
		if got := r.header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if strings.Contains(r.query, "x-id") || r.query != "" {
		t.Errorf("a put with a query %q", r.query)
	}
	if !strings.HasPrefix(r.header.Get("User-Agent"), "cactusfs/") {
		t.Errorf("User-Agent %q", r.header.Get("User-Agent"))
	}

	path := "/replica/" + strings.ReplaceAll(v.Key, "%", "%25")
	resp, got := s.root(http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("GET = %d, %d bytes", resp.StatusCode, len(got))
	}
	for name, value := range map[string]string{
		"Content-Type": "text/plain", "Content-Language": "en", "Cache-Control": "no-cache",
		"Content-Disposition": "inline", "X-Amz-Meta-Origin": "site a", "X-Amz-Meta-N": "1",
		"X-Amz-Object-Lock-Mode": "GOVERNANCE", "X-Amz-Object-Lock-Legal-Hold": "ON",
		"ETag": `"` + v.ETag + `"`,
	} {
		if resp.Header.Get(name) != value {
			t.Errorf("GET %s = %q, want %q", name, resp.Header.Get(name), value)
		}
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("the framing was stored: %q", resp.Header.Get("Content-Encoding"))
	}
	if _, tags := s.root(http.MethodGet, path+"?tagging", nil, nil); !bytes.Contains(tags, []byte("<Key>team</Key><Value>a+b/c</Value>")) {
		t.Errorf("tags = %s", tags)
	}

	empty := s.version("replica", "empty", nil, 4712)
	if err := c.PutReplica(t.Context(), empty, bytes.NewReader(nil)); err != nil {
		t.Fatalf("PutReplica of nothing: %v", err)
	}
	if resp, got := s.root(http.MethodGet, "/replica/empty", nil, nil); resp.StatusCode != http.StatusOK || len(got) != 0 {
		t.Errorf("GET of nothing = %d, %d bytes", resp.StatusCode, len(got))
	}

	null := s.version("replica", "null", []byte("before versions"), 1)
	null.Replica.Null, null.Versioned = true, false
	if err := c.PutReplica(t.Context(), null, strings.NewReader("before versions")); err != nil {
		t.Fatalf("PutReplica of the null version: %v", err)
	}
	if got := lastFor(t, s.seen(), http.MethodPut, "/replica/null").header.Get(s3client.HeaderReplicaVersionID); got != "null" {
		t.Errorf("the null version is sent as %q", got)
	}
}

// TestRetentionThatPassedIsLeftOut: a version whose retention ended before
// it is sent goes without it (ADR-0026, "Open", 7); its legal hold goes.
func TestRetentionThatPassedIsLeftOut(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", true)
	c := s.client(0)
	v := s.version("replica", "old", []byte("old"), 9)
	v.Lock = object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: siteNow.Add(-time.Second)}, LegalHold: true}
	if err := c.PutReplica(t.Context(), v, strings.NewReader("old")); err != nil {
		t.Fatalf("PutReplica: %v (%v)", err, errors.Unwrap(err))
	}
	r := lastFor(t, s.seen(), http.MethodPut, "/replica/old")
	if r.header.Get("X-Amz-Object-Lock-Mode") != "" || r.header.Get("X-Amz-Object-Lock-Retain-Until-Date") != "" ||
		r.header.Get("X-Amz-Object-Lock-Legal-Hold") != "ON" {
		t.Errorf("lock headers %v", r.header)
	}
}

// TestMultipart: an upload begun with the source, two parts of signed
// chunks, the upload listed among the replica uploads of its key and of no
// other, completed with the five replica headers; an upload aborted, and
// aborted again, which is no error.
func TestMultipart(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", false)
	c := s.client(0)
	s3client.SetUploadsPage(c, 2)

	first := bytes.Repeat([]byte{'p'}, 5<<20)
	second := []byte("the last part")
	whole := append(slices.Clone(first), second...)
	v := s.version("replica", "large", whole, 77)
	v.Parts, v.ContentType, v.UserMetadata = 2, "application/octet-stream", map[string]string{"parts": "2"}

	id, err := c.CreateUpload(t.Context(), v)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	create := lastFor(t, s.seen(), http.MethodPost, "/replica/large")
	signedAndBound(t, create, false)
	if create.header.Get(s3client.HeaderReplicaSource) != source || create.header.Get(s3client.HeaderReplicaSeq) != "" || create.query != "uploads" {
		t.Errorf("CreateUpload sent %q, %v", create.query, create.header)
	}
	// Uploads of other keys under the same prefix, and two more of the
	// key: a listing of several pages.
	others := []string{}
	for _, key := range []string{"large", "large", "large-not", "larger"} {
		other := v
		other.Key = key
		upload, err := c.CreateUpload(t.Context(), other)
		if err != nil {
			t.Fatalf("CreateUpload %s: %v", key, err)
		}
		if key == "large" {
			others = append(others, upload)
		}
	}
	listed, err := c.ListReplicaUploads(t.Context(), "replica", "large")
	if err != nil {
		t.Fatalf("ListReplicaUploads: %v", err)
	}
	wantListed := append([]string{id}, others...)
	slices.Sort(wantListed)
	if !slices.Equal(listed, wantListed) {
		t.Errorf("ListReplicaUploads = %v, want %v", listed, wantListed)
	}
	list := lastFor(t, s.seen(), http.MethodGet, "/replica")
	signedAndBound(t, list, false)
	if list.header.Get(s3client.HeaderReplicaSource) != source || !strings.Contains(list.query, "prefix=large") || !strings.Contains(list.query, "encoding-type=url") {
		t.Errorf("listing sent %q", list.query)
	}

	var parts []ports.SitePart
	for i, part := range [][]byte{first, second} {
		etag, err := c.PutPart(t.Context(), "replica", "large", id, i+1, int64(len(part)), bytes.NewReader(part))
		if err != nil {
			t.Fatalf("PutPart %d: %v", i+1, err)
		}
		parts = append(parts, ports.SitePart{Number: i + 1, ETag: etag})
	}
	put := lastFor(t, s.seen(), http.MethodPut, "/replica/large")
	signedAndBound(t, put, true)
	if put.header.Get(s3client.HeaderReplicaSource) != "" {
		t.Errorf("a part carries a replica header")
	}

	v.ETag = completedETag(t, s, first, second)
	if err := c.CompleteReplica(t.Context(), v, id, parts); err != nil {
		t.Fatalf("CompleteReplica: %v", err)
	}
	complete := lastFor(t, s.seen(), http.MethodPost, "/replica/large")
	signedAndBound(t, complete, false)
	for _, name := range []string{s3client.HeaderReplicaSource, s3client.HeaderReplicaVersionID, s3client.HeaderReplicaSeq, s3client.HeaderReplicaMTime, s3client.HeaderReplicaETag} {
		if complete.header.Get(name) == "" {
			t.Errorf("CompleteReplica without %s", name)
		}
	}
	resp, got := s.root(http.MethodGet, "/replica/large", nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, whole) || resp.Header.Get("X-Amz-Meta-Parts") != "2" {
		t.Fatalf("GET = %d, %d bytes, %v", resp.StatusCode, len(got), resp.Header)
	}
	if etag := strings.Trim(resp.Header.Get("ETag"), `"`); etag != v.ETag {
		t.Errorf("ETag %q, want %q", etag, v.ETag)
	}

	for _, upload := range others {
		if err := c.AbortUpload(t.Context(), "replica", "large", upload); err != nil {
			t.Fatalf("AbortUpload: %v", err)
		}
	}
	abort := lastFor(t, s.seen(), http.MethodDelete, "/replica/large")
	signedAndBound(t, abort, false)
	if abort.header.Get(s3client.HeaderReplicaSource) != source {
		t.Errorf("AbortUpload without the source")
	}
	if err := c.AbortUpload(t.Context(), "replica", "large", others[0]); err != nil {
		t.Errorf("AbortUpload of an upload that is gone: %v", err)
	}
	if listed, err := c.ListReplicaUploads(t.Context(), "replica", "large"); err != nil || len(listed) != 0 {
		t.Errorf("ListReplicaUploads after = %v, %v", listed, err)
	}
}

// completedETag returns the entity tag the site gives an upload of these
// parts: what it completes as, when its root uploads them into a bucket
// that is no replica.
func completedETag(t *testing.T, s *site, parts ...[]byte) string {
	t.Helper()
	s.makeScratch()
	resp, body := s.root(http.MethodPost, "/scratch/etag-of-parts?uploads", nil, nil)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	if resp.StatusCode != http.StatusOK || xml.Unmarshal(body, &initiated) != nil {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	doc := "<CompleteMultipartUpload>"
	for i, part := range parts {
		resp, body := s.root(http.MethodPut, fmt.Sprintf("/scratch/etag-of-parts?partNumber=%d&uploadId=%s", i+1, initiated.UploadID), nil, part)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("part = %d %s", resp.StatusCode, body)
		}
		doc += fmt.Sprintf("<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, resp.Header.Get("ETag"))
	}
	resp, body = s.root(http.MethodPost, "/scratch/etag-of-parts?uploadId="+initiated.UploadID, nil, []byte(doc+"</CompleteMultipartUpload>"))
	var completed struct {
		ETag string `xml:"ETag"`
	}
	if resp.StatusCode != http.StatusOK || xml.Unmarshal(body, &completed) != nil {
		t.Fatalf("complete = %d %s", resp.StatusCode, body)
	}
	return strings.Trim(completed.ETag, `"`)
}

// TestMarkerVersionAndLaterChanges: a delete marker sent with the source,
// its ID, its sequence and its time; HeadVersion of a version that is there,
// of one that is not, and of a marker; tags put and removed, a retention
// and a legal hold put, each on the version named.
func TestMarkerVersionAndLaterChanges(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", true)
	c := s.client(0)

	v := s.version("replica", "doc", []byte("the document"), 10)
	if err := c.PutReplica(t.Context(), v, strings.NewReader("the document")); err != nil {
		t.Fatalf("PutReplica: %v (%v)", err, errors.Unwrap(err))
	}
	// The copy has the source's ID at the site.
	resp, _ := s.root(http.MethodHead, "/replica/doc", nil, nil)
	there, err := object.ParseVersionRef(resp.Header.Get("X-Amz-Version-Id"))
	if err != nil || there != v.Ref() {
		t.Fatalf("the version at the site = %v, %v; want %v", there, err, v.Ref())
	}

	etag, found, err := c.HeadVersion(t.Context(), "replica", "doc", there)
	if err != nil || !found || etag != v.ETag {
		t.Errorf("HeadVersion = %q, %v, %v; want %q", etag, found, err, v.ETag)
	}
	head := lastFor(t, s.seen(), http.MethodHead, "/replica/doc")
	signedAndBound(t, head, false)
	if head.header.Get(s3client.HeaderReplicaSource) != source || head.query != "versionId="+there.String() {
		t.Errorf("HeadVersion sent %q, %v", head.query, head.header)
	}
	if etag, found, err := c.HeadVersion(t.Context(), "replica", "doc", object.VersionRef{ID: newID(t)}); err != nil || found || etag != "" {
		t.Errorf("HeadVersion of a version that is not there = %q, %v, %v", etag, found, err)
	}

	for _, tags := range []map[string]string{{"stage": "final"}, nil} {
		if err := c.PutTags(t.Context(), "replica", "doc", there, tags); err != nil {
			t.Fatalf("PutTags %v: %v", tags, err)
		}
		_, body := s.root(http.MethodGet, "/replica/doc?tagging&versionId="+there.String(), nil, nil)
		if has := bytes.Contains(body, []byte("<Key>stage</Key><Value>final</Value>")); has != (tags != nil) {
			t.Errorf("tags after %v: %s", tags, body)
		}
	}
	until := siteNow.Add(72 * time.Hour)
	if err := c.PutRetention(t.Context(), "replica", "doc", there, &object.Retention{Mode: object.Compliance, Until: until}); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if _, body := s.root(http.MethodGet, "/replica/doc?retention&versionId="+there.String(), nil, nil); !bytes.Contains(body, []byte("<Mode>COMPLIANCE</Mode>")) {
		t.Errorf("retention = %s", body)
	}
	// Object Lock refuses to shorten or remove it: each such change fails
	// alone (rejected), and is told from a refused key.
	for _, r := range []*object.Retention{{Mode: object.Compliance, Until: siteNow.Add(time.Hour)}, nil} {
		failsWith(t, fmt.Sprintf("PutRetention %v", r), c.PutRetention(t.Context(), "replica", "doc", there, r), delivery.ClassRejected)
	}
	if _, body := s.root(http.MethodGet, "/replica/doc?retention&versionId="+there.String(), nil, nil); !bytes.Contains(body, []byte("<RetainUntilDate>"+until.Format("2006-01-02T15:04:05"))) {
		t.Errorf("retention after the refusals = %s", body)
	}
	for _, on := range []bool{true, false} {
		if err := c.PutLegalHold(t.Context(), "replica", "doc", there, on); err != nil {
			t.Fatalf("PutLegalHold %v: %v", on, err)
		}
		_, body := s.root(http.MethodGet, "/replica/doc?legal-hold&versionId="+there.String(), nil, nil)
		if want := map[bool]string{true: "ON", false: "OFF"}[on]; !bytes.Contains(body, []byte("<Status>"+want+"</Status>")) {
			t.Errorf("legal hold after %v = %s", on, body)
		}
	}
	for _, r := range s.seen() {
		if r.path == "/replica/doc" && r.method == http.MethodPut && strings.Contains(r.query, "versionId=") {
			signedAndBound(t, r, false)
		}
	}

	marker := v
	marker.VersionID, marker.DeleteMarker, marker.Size, marker.ETag = newID(t), true, 0, ""
	marker.Replica = &object.Replica{From: source, Seq: 11}
	if err := c.PutMarker(t.Context(), marker); err != nil {
		t.Fatalf("PutMarker: %v", err)
	}
	del := lastFor(t, s.seen(), http.MethodDelete, "/replica/doc")
	signedAndBound(t, del, false)
	if del.query != "" || del.header.Get(s3client.HeaderReplicaVersionID) != marker.VersionID.String() ||
		del.header.Get(s3client.HeaderReplicaSeq) != "11" || del.header.Get(s3client.HeaderReplicaMTime) == "" ||
		del.header.Get(s3client.HeaderReplicaETag) != "" {
		t.Errorf("PutMarker sent %q, %v", del.query, del.header)
	}
	resp, _ = s.root(http.MethodHead, "/replica/doc", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD after the marker = %d", resp.StatusCode)
	}
	// The marker's ID at the site, from its listing of versions.
	_, listing := s.root(http.MethodGet, "/replica?versions", nil, nil)
	var versions struct {
		Markers []struct {
			VersionID string `xml:"VersionId"`
		} `xml:"DeleteMarker"`
	}
	if err := xml.Unmarshal(listing, &versions); err != nil || len(versions.Markers) != 1 {
		t.Fatalf("versions = %s, %v", listing, err)
	}
	markerRef, err := object.ParseVersionRef(versions.Markers[0].VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if etag, found, err := c.HeadVersion(t.Context(), "replica", "doc", markerRef); err != nil || !found || etag != "" {
		t.Errorf("HeadVersion of a marker = %q, %v, %v", etag, found, err)
	}
}

// failingReader yields its bytes and then fails.
type failingReader struct {
	data []byte
}

// Read implements io.Reader.
func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, errors.New("the disk went away")
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// TestBodyCutShortNeverCommits: a body that ends before its size, one that
// fails on the way, and one that goes on past its size are cut off after
// chunks were sent; the site commits nothing, and the failure is this
// side's: ClassInternal.
func TestBodyCutShortNeverCommits(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("x"), 40<<10)
	tests := []struct {
		name string
		body func() io.Reader
		size int64
		want error
	}{
		{"ends early", func() io.Reader { return bytes.NewReader(content[:30<<10]) }, int64(len(content)), s3client.ErrShortBody},
		{"fails on the way", func() io.Reader { return &failingReader{data: content[:20<<10]} }, int64(len(content)), nil},
		{"goes on past its size", func() io.Reader { return bytes.NewReader(content) }, int64(len(content) - 1), s3client.ErrLongBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t)
			s.createBucket("replica", false)
			c := s.client(0)
			s3client.SetChunkSize(c, 8<<10)
			v := s.version("replica", "cut", content, 3)
			v.Size = tt.size
			err := c.PutReplica(t.Context(), v, tt.body())
			failsWith(t, "PutReplica", err, delivery.ClassInternal)
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error %v, want %v", err, tt.want)
			}
			if len(s.seen()) == 0 {
				t.Fatalf("nothing was sent: the cut was not tested")
			}
			if resp, _ := s.root(http.MethodHead, "/replica/cut", nil, nil); resp.StatusCode != http.StatusNotFound {
				t.Errorf("HEAD after the cut = %d: something was committed", resp.StatusCode)
			}
		})
	}
}

// flipAfter changes one byte of a body once offset bytes of it have passed.
type flipAfter struct {
	body   io.ReadCloser
	offset int
	read   int
}

// Read implements io.Reader.
func (f *flipAfter) Read(p []byte) (int, error) {
	n, err := f.body.Read(p)
	if at := f.offset - f.read; at >= 0 && at < n {
		p[at] ^= 0x01
	}
	f.read += n
	return n, err
}

// Close implements io.Closer.
func (f *flipAfter) Close() error {
	return f.body.Close()
}

// TestChangedRequestIsRefused: a byte of a chunk changed on the way, a
// replica header changed on the way, and a chunk signature changed on the
// way are each refused by the site's verification, with nothing stored;
// the client says auth (a 403 of the signature).
func TestChangedRequestIsRefused(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("y"), 20<<10)
	tests := []struct {
		name   string
		tamper func(*http.Request)
	}{
		// The first chunk's header line is about 86 bytes; 500 is in its
		// data.
		{"a byte of a chunk", func(r *http.Request) { r.Body = &flipAfter{body: r.Body, offset: 500} }},
		// The signature of the second chunk begins after the first
		// chunk's frame.
		{"a chunk signature", func(r *http.Request) { r.Body = &flipAfter{body: r.Body, offset: 8<<10 + 90 + 20} }},
		{"a replica header", func(r *http.Request) { r.Header.Set(s3client.HeaderReplicaSeq, "999") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t)
			s.createBucket("replica", false)
			c := s.client(0)
			s3client.SetChunkSize(c, 8<<10)
			v := s.version("replica", "changed", content, 5)
			s.set(func(s *site) {
				s.tamper = func(r *http.Request) {
					if r.Method == http.MethodPut && r.URL.Path == "/replica/changed" {
						tt.tamper(r)
					}
				}
			})
			err := c.PutReplica(t.Context(), v, bytes.NewReader(content))
			failsWith(t, "PutReplica", err, delivery.ClassAuth)
			if resp, _ := s.root(http.MethodHead, "/replica/changed", nil, nil); resp.StatusCode != http.StatusNotFound {
				t.Errorf("HEAD after the change = %d: something was committed", resp.StatusCode)
			}
		})
	}
}

// answer is a scripted answer of the site.
func answer(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodHead || code == "" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>key secret-key-name</Message><Resource>/replica/secret-key-name</Resource></Error>`, code)
	}
}

// TestClasses: what each answer of the other deployment, and each failure
// to get one, is called (delivery.Class); a 409 OperationAborted of a
// replica write - the version is there already - is success.
func TestClasses(t *testing.T) {
	t.Parallel()
	content := []byte("classed")
	type send func(ctx context.Context, c *s3client.Client, v object.Version) error
	put := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		return c.PutReplica(ctx, v, bytes.NewReader(content))
	}
	check := func(ctx context.Context, c *s3client.Client, _ object.Version) error {
		_, err := c.Check(ctx, "replica")
		return err
	}
	marker := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		v.DeleteMarker, v.Size = true, 0
		return c.PutMarker(ctx, v)
	}
	complete := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		return c.CompleteReplica(ctx, v, "upload-1", []ports.SitePart{{Number: 1, ETag: "abc"}})
	}
	tags := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		return c.PutTags(ctx, "replica", "doc", v.Ref(), map[string]string{"a": "b"})
	}
	tests := []struct {
		name   string
		script http.HandlerFunc
		send   send
		want   delivery.Class
	}{
		{"403 AccessDenied", answer(http.StatusForbidden, "AccessDenied"), put, delivery.ClassAuth},
		{"403 SignatureDoesNotMatch", answer(http.StatusForbidden, "SignatureDoesNotMatch"), put, delivery.ClassAuth},
		{"403 of a HEAD", answer(http.StatusForbidden, ""), check, delivery.ClassAuth},
		{"403 QuotaExceeded", answer(http.StatusForbidden, "QuotaExceeded"), put, delivery.ClassQuota},
		{"400 AuthorizationHeaderMalformed", answer(http.StatusBadRequest, "AuthorizationHeaderMalformed"), put, delivery.ClassAuth},
		{"404 NoSuchBucket", answer(http.StatusNotFound, "NoSuchBucket"), put, delivery.ClassNoSuchBucket},
		{"404 of the bucket probe", answer(http.StatusNotFound, ""), check, delivery.ClassNoSuchBucket},
		{"404 NoSuchVersion of tags", answer(http.StatusNotFound, "NoSuchVersion"), tags, delivery.ClassRejected},
		{"404 without a code", answer(http.StatusNotFound, ""), put, delivery.ClassServer},
		{"409 InvalidBucketState", answer(http.StatusConflict, "InvalidBucketState"), put, delivery.ClassNotReplica},
		// A conflict is asked about with a HEAD, which this script answers
		// 409 as well: the 409 of a question, a bucket that is no replica
		// of this source (TestConflictIsAskedAbout).
		{"409 OperationAborted of a put", answer(http.StatusConflict, "OperationAborted"), put, delivery.ClassNotReplica},
		{"409 OperationAborted of a marker", answer(http.StatusConflict, "OperationAborted"), marker, delivery.ClassNotReplica},
		{"409 OperationAborted of a completion", answer(http.StatusConflict, "OperationAborted"), complete, delivery.ClassNotReplica},
		{"409 of a version probe", answer(http.StatusConflict, ""), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			_, _, err := c.HeadVersion(ctx, "replica", "doc", v.Ref())
			return err
		}, delivery.ClassNotReplica},
		{"409 of the bucket probe", answer(http.StatusConflict, ""), check, delivery.ClassNotReplica},
		// Object Lock refuses a later change: that change fails, and the
		// target goes on; a copy is never refused so, and is held.
		{"403 ObjectLocked of a retention", answer(http.StatusForbidden, "ObjectLocked"), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			return c.PutRetention(ctx, "replica", "doc", v.Ref(), nil)
		}, delivery.ClassRejected},
		{"403 ObjectLocked of tags", answer(http.StatusForbidden, "ObjectLocked"), tags, delivery.ClassRejected},
		{"403 ObjectLocked of a legal hold", answer(http.StatusForbidden, "ObjectLocked"), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			return c.PutLegalHold(ctx, "replica", "doc", v.Ref(), false)
		}, delivery.ClassRejected},
		{"403 ObjectLocked of a put", answer(http.StatusForbidden, "ObjectLocked"), put, delivery.ClassServer},
		// A deployment that does not take the source on a request holds the
		// target, a retention's included.
		{"400 ReplicaHeaderNotTaken of a retention", answer(http.StatusBadRequest, "ReplicaHeaderNotTaken"), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			return c.PutRetention(ctx, "replica", "doc", v.Ref(), nil)
		}, delivery.ClassServer},
		{"400 ReplicaHeaderNotTaken of a put", answer(http.StatusBadRequest, "ReplicaHeaderNotTaken"), put, delivery.ClassServer},
		{"403 AccessDenied of a retention", answer(http.StatusForbidden, "AccessDenied"), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			return c.PutRetention(ctx, "replica", "doc", v.Ref(), nil)
		}, delivery.ClassAuth},
		{"409 OperationAborted of tags", answer(http.StatusConflict, "OperationAborted"), tags, delivery.ClassServer},
		{"409 BucketNotEmpty", answer(http.StatusConflict, "BucketNotEmpty"), put, delivery.ClassServer},
		{"400 InvalidArgument of a put", answer(http.StatusBadRequest, "InvalidArgument"), put, delivery.ClassServer},
		{"400 InvalidArgument of a retention", answer(http.StatusBadRequest, "InvalidArgument"), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			return c.PutRetention(ctx, "replica", "doc", v.Ref(), &object.Retention{Mode: object.Governance, Until: siteNow.Add(time.Hour)})
		}, delivery.ClassRejected},
		{"400 InvalidRequest", answer(http.StatusBadRequest, "InvalidRequest"), put, delivery.ClassServer},
		{"400 XAmzContentSHA256Mismatch", answer(http.StatusBadRequest, "XAmzContentSHA256Mismatch"), put, delivery.ClassServer},
		{"400 InvalidTag", answer(http.StatusBadRequest, "InvalidTag"), put, delivery.ClassRejected},
		{"400 EntityTooLarge", answer(http.StatusBadRequest, "EntityTooLarge"), put, delivery.ClassRejected},
		{"405", answer(http.StatusMethodNotAllowed, "MethodNotAllowed"), tags, delivery.ClassServer},
		{"412", answer(http.StatusPreconditionFailed, "PreconditionFailed"), put, delivery.ClassServer},
		{"404 NoSuchKey of a put", answer(http.StatusNotFound, "NoSuchKey"), put, delivery.ClassServer},
		{"a version probe without an entity tag", answer(http.StatusOK, ""), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			_, _, err := c.HeadVersion(ctx, "replica", "doc", v.Ref())
			return err
		}, delivery.ClassServer},
		{"200 of a completion that is no result", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<Something/>`)
		}, complete, delivery.ClassServer},
		{"200 of a completion with an empty body", answer(http.StatusOK, ""), complete, delivery.ClassServer},
		{"200 of a completion that completed", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"x-1"</ETag></CompleteMultipartUploadResult>`)
		}, complete, delivery.ClassNone},
		{"400 IncompleteBody", answer(http.StatusBadRequest, "IncompleteBody"), put, delivery.ClassServer},
		{"413", answer(http.StatusRequestEntityTooLarge, ""), put, delivery.ClassRejected},
		{"429", answer(http.StatusTooManyRequests, "SlowDown"), put, delivery.ClassServer},
		{"500", answer(http.StatusInternalServerError, "InternalError"), put, delivery.ClassServer},
		{"501", answer(http.StatusNotImplemented, "NotImplemented"), put, delivery.ClassNotReplica},
		{"503", answer(http.StatusServiceUnavailable, "ServiceUnavailable"), put, delivery.ClassServer},
		{"503 of the bucket probe", answer(http.StatusServiceUnavailable, ""), check, delivery.ClassServer},
		{"301", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://example.com/elsewhere")
			w.WriteHeader(http.StatusMovedPermanently)
		}, put, delivery.ClassRedirect},
		{"200 of a completion that failed", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
		}, complete, delivery.ClassServer},
		{"200 of a completion that conflicted, and a HEAD without an entity tag", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<Error><Code>OperationAborted</Code></Error>`)
		}, complete, delivery.ClassServer},
		{"an answer longer than its bound", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>"+strings.Repeat("u", 20<<10)+"</UploadId></InitiateMultipartUploadResult>")
		}, func(ctx context.Context, c *s3client.Client, v object.Version) error {
			_, err := c.CreateUpload(ctx, v)
			return err
		}, delivery.ClassServer},
		{"an upload ID that is none", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>a b</UploadId></InitiateMultipartUploadResult>")
		}, func(ctx context.Context, c *s3client.Client, v object.Version) error {
			_, err := c.CreateUpload(ctx, v)
			return err
		}, delivery.ClassServer},
		{"a part without an entity tag", answer(http.StatusOK, ""), func(ctx context.Context, c *s3client.Client, _ object.Version) error {
			_, err := c.PutPart(ctx, "replica", "doc", "upload-1", 1, 3, strings.NewReader("abc"))
			return err
		}, delivery.ClassServer},
		{"403 of an abort", answer(http.StatusForbidden, "AccessDenied"), func(ctx context.Context, c *s3client.Client, _ object.Version) error {
			return c.AbortUpload(ctx, "replica", "doc", "upload-1")
		}, delivery.ClassAuth},
		{"403 of a version probe", answer(http.StatusForbidden, ""), func(ctx context.Context, c *s3client.Client, v object.Version) error {
			_, _, err := c.HeadVersion(ctx, "replica", "doc", v.Ref())
			return err
		}, delivery.ClassAuth},
		{"503 of a listing", answer(http.StatusServiceUnavailable, "ServiceUnavailable"), func(ctx context.Context, c *s3client.Client, _ object.Version) error {
			_, err := c.ListReplicaUploads(ctx, "replica", "doc")
			return err
		}, delivery.ClassServer},
		{"a listing that does not go on", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<ListMultipartUploadsResult><EncodingType>url</EncodingType><IsTruncated>true</IsTruncated><NextKeyMarker>doc</NextKeyMarker><NextUploadIdMarker>u</NextUploadIdMarker><Upload><Key>doc</Key><UploadId>u</UploadId></Upload></ListMultipartUploadsResult>`)
		}, func(ctx context.Context, c *s3client.Client, _ object.Version) error {
			_, err := c.ListReplicaUploads(ctx, "replica", "doc")
			return err
		}, delivery.ClassServer},
		{"versioning that is no document", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `<VersioningConfiguration><Status>Sometimes</Status></VersioningConfiguration>`)
			}
		}, check, delivery.ClassServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t)
			s.set(func(s *site) { s.script = tt.script })
			c := s.client(0)
			v := s.versionWithoutSite("replica", "doc", content, 12)
			err := tt.send(t.Context(), c, v)
			failsWith(t, tt.name, err, tt.want)
			if err != nil && strings.Contains(fmt.Sprintf("%+v", errors.Unwrap(err)), "secret-key-name") {
				t.Errorf("the error tells what the answer said: %+v", errors.Unwrap(err))
			}
			if len(s.seen()) == 0 {
				t.Errorf("nothing was sent")
			}
		})
	}
}

// versionWithoutSite is version with an entity tag of its own: for a site
// that only answers as scripted.
func (s *site) versionWithoutSite(bucketName, key string, content []byte, seq uint64) object.Version {
	return object.Version{
		Bucket: bucketName, Key: key, VersionID: newID(s.t), Versioned: true, Size: int64(len(content)),
		ETag: "0123456789abcdef0123456789abcdef", ModifiedAt: siteNow, Seq: seq,
		Replica: &object.Replica{From: source, Seq: seq},
	}
}

// TestRealRefusals: the real handler's own refusals - a secret it does not
// know, a bucket it does not have - are auth and no_such_bucket.
func TestRealRefusals(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", false)
	kms := newKMS(t)
	wrong, err := s3client.New(s.target(kms, []byte("not-the-secret-of-the-key")), sealed.KMS{KMS: kms}, s.egressClient(0),
		s3client.Options{Source: source, Clock: porttest.NewClock(siteNow)})
	if err != nil {
		t.Fatal(err)
	}
	v := s.version("replica", "doc", []byte("doc"), 1)
	failsWith(t, "PutReplica with a wrong secret", wrong.PutReplica(t.Context(), v, strings.NewReader("doc")), delivery.ClassAuth)
	_, err = wrong.Check(t.Context(), "replica")
	failsWith(t, "Check with a wrong secret", err, delivery.ClassAuth)

	c := s.client(0)
	v.Bucket = "missing"
	failsWith(t, "PutReplica to a bucket that is not there", c.PutReplica(t.Context(), v, strings.NewReader("doc")), delivery.ClassNoSuchBucket)
	_, err = c.Check(t.Context(), "missing")
	failsWith(t, "Check of a bucket that is not there", err, delivery.ClassNoSuchBucket)
}

// TestTimeout: a site that does not answer within the response timeout, and
// a deadline of the caller's that passes, are timeouts; a caller that
// cancels is this side's.
func TestTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := newSite(t)
	t.Cleanup(func() { close(release) })
	s.set(func(s *site) {
		s.script = func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusOK)
		}
	})
	slow := s.client(200 * time.Millisecond)
	v := s.versionWithoutSite("replica", "doc", []byte("doc"), 1)
	failsWith(t, "the response timeout", slow.PutReplica(t.Context(), v, strings.NewReader("doc")), delivery.ClassTimeout)

	c := s.client(0)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := c.Check(ctx, "replica")
	failsWith(t, "the caller's deadline", err, delivery.ClassTimeout)

	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	failsWith(t, "a deadline passed before", c.PutTags(expired, "replica", "doc", v.Ref(), nil), delivery.ClassTimeout)
	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	failsWith(t, "a cancelled caller", c.PutLegalHold(cancelled, "replica", "doc", v.Ref(), true), delivery.ClassInternal)
}

// TestRefusedBeforeSending: what is not a version to send is refused with
// nothing sent - a version of a customer's key as sse_c, everything else
// as this side's failure.
func TestRefusedBeforeSending(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	c := s.client(0)
	base := s.versionWithoutSite("replica", "doc", []byte("doc"), 1)
	change := func(f func(v *object.Version)) object.Version {
		v := base
		replica := *base.Replica
		v.Replica = &replica
		f(&v)
		return v
	}
	body := func() io.Reader { return strings.NewReader("doc") }
	tests := []struct {
		name string
		call func() error
		want delivery.Class
	}{
		{"SSE-C", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Encryption.Mode = object.SSEC }), body())
		}, delivery.ClassSSEC},
		{"SSE-C upload", func() error {
			_, err := c.CreateUpload(t.Context(), change(func(v *object.Version) { v.Encryption.Mode = object.SSEC }))
			return err
		}, delivery.ClassSSEC},
		{"no replica", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Replica = nil }), body())
		}, delivery.ClassInternal},
		{"another source", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Replica.From = "site-c" }), body())
		}, delivery.ClassInternal},
		{"sequence 0", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Replica.Seq = 0 }), body())
		}, delivery.ClassInternal},
		{"sequence past 2^62", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Replica.Seq = object.MaxReplicaSeq + 1 }), body())
		}, delivery.ClassInternal},
		{"no ID", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.VersionID = object.ID{} }), body())
		}, delivery.ClassInternal},
		{"no time", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.ModifiedAt = time.Time{} }), body())
		}, delivery.ClassInternal},
		{"no entity tag", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.ETag = "" }), body())
		}, delivery.ClassInternal},
		{"a marker as a put", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.DeleteMarker = true }), body())
		}, delivery.ClassInternal},
		{"too large", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Size = object.MaxSize + 1 }), body())
		}, delivery.ClassInternal},
		{"no body", func() error { return c.PutReplica(t.Context(), base, nil) }, delivery.ClassInternal},
		{"no bucket", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Bucket = "Not_A_Bucket" }), body())
		}, delivery.ClassInternal},
		{"no key", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.Key = "" }), body())
		}, delivery.ClassInternal},
		{"a marker without an ID", func() error {
			return c.PutMarker(t.Context(), change(func(v *object.Version) { v.DeleteMarker, v.VersionID = true, object.ID{} }))
		}, delivery.ClassInternal},
		{"a marker that is none", func() error { return c.PutMarker(t.Context(), base) }, delivery.ClassInternal},
		{"a header with a newline", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.UserMetadata = map[string]string{"a": "b\r\nX-Injected: 1"} }), body())
		}, delivery.ClassRejected},
		{"a header name that is no token", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) { v.UserMetadata = map[string]string{"a b": "c"} }), body())
		}, delivery.ClassRejected},
		{"more headers than a signature covers", func() error {
			return c.PutReplica(t.Context(), change(func(v *object.Version) {
				v.UserMetadata = map[string]string{}
				for i := range 70 {
					v.UserMetadata["m"+strconv.Itoa(i)] = "v"
				}
			}), body())
		}, delivery.ClassRejected},
		{"a part number of 0", func() error {
			_, err := c.PutPart(t.Context(), "replica", "doc", "upload", 0, 3, body())
			return err
		}, delivery.ClassInternal},
		{"a part without an upload", func() error {
			_, err := c.PutPart(t.Context(), "replica", "doc", "", 1, 3, body())
			return err
		}, delivery.ClassInternal},
		{"a completion without parts", func() error { return c.CompleteReplica(t.Context(), base, "upload", nil) }, delivery.ClassInternal},
		{"a completion of a part without an entity tag", func() error {
			return c.CompleteReplica(t.Context(), base, "upload", []ports.SitePart{{Number: 1}})
		}, delivery.ClassInternal},
		{"an abort without an upload", func() error { return c.AbortUpload(t.Context(), "replica", "doc", "a b") }, delivery.ClassInternal},
		{"a version that is none", func() error {
			_, _, err := c.HeadVersion(t.Context(), "replica", "doc", object.VersionRef{})
			return err
		}, delivery.ClassInternal},
		{"a check of no bucket", func() error {
			_, err := c.Check(t.Context(), "")
			return err
		}, delivery.ClassInternal},
		{"a listing of no key", func() error {
			_, err := c.ListReplicaUploads(t.Context(), "replica", "")
			return err
		}, delivery.ClassInternal},
	}
	for _, tt := range tests {
		failsWith(t, tt.name, tt.call(), tt.want)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

// TestNew: what New refuses, with an error that names nothing of the
// target but its name.
func TestNew(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	kms := newKMS(t)
	good := s.target(kms, nil)
	client := s.egressClient(0)
	opener := sealed.KMS{KMS: kms}
	opts := s3client.Options{Source: source}
	tests := []struct {
		name   string
		target func(delivery.Target) delivery.Target
		opener sealed.Opener
		opts   s3client.Options
		ok     bool
	}{
		{"good", func(t delivery.Target) delivery.Target { return t }, opener, opts, true},
		{"a webhook", func(t delivery.Target) delivery.Target { t.Kind = delivery.KindWebhook; return t }, opener, opts, false},
		{"no region", func(t delivery.Target) delivery.Target { t.Region = ""; return t }, opener, opts, false},
		{"no secret", func(t delivery.Target) delivery.Target { t.Secret = good.Secret; t.Secret.Ciphertext = nil; return t }, opener, opts, false},
		{"no opener", func(t delivery.Target) delivery.Target { return t }, nil, opts, false},
		{"no source", func(t delivery.Target) delivery.Target { return t }, opener, s3client.Options{}, false},
		{"a source that is none", func(t delivery.Target) delivery.Target { return t }, opener, s3client.Options{Source: "a b"}, false},
	}
	following, _, err := egress.NewClient(egress.Policy{Redirects: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s3client.New(good, opener, following, opts); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("New with an egress client that follows redirects = %v", err)
	}
	for _, tt := range tests {
		c, err := s3client.New(tt.target(good), tt.opener, client, tt.opts)
		if (err == nil) != tt.ok {
			t.Errorf("%s: New = %v", tt.name, err)
		}
		if err != nil && !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: %v is not ErrInvalidArgument", tt.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "example.com") {
			t.Errorf("%s: the error names the endpoint: %v", tt.name, err)
		}
		if tt.ok && c == nil {
			t.Errorf("%s: no client", tt.name)
		}
	}
	if _, err := s3client.New(good, opener, nil, opts); err == nil {
		t.Errorf("New without an egress client")
	}
}

// TestSecretNeverShown: the secret is in no printed form of the client, no
// log of it and no link of any error; a secret that does not open fails as
// this side's, and sends nothing.
func TestSecretNeverShown(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", false)
	c := s.client(0)
	secret := string(s.key)

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("client", slog.Any("client", c))
	for _, text := range []string{c.String(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), logged.String()} {
		if strings.Contains(text, secret) || strings.Contains(text, "example.com") || !strings.Contains(text, targetName) {
			t.Errorf("a printed form of the client: %s", text)
		}
	}

	_, err := s.client(0).Check(t.Context(), "missing")
	for link := err; link != nil; link = errors.Unwrap(link) {
		if text := fmt.Sprintf("%+v", link); strings.Contains(text, secret) || strings.Contains(text, "missing") {
			t.Errorf("a link of the error tells too much: %s", text)
		}
	}

	// A secret sealed for another target does not open.
	kms := newKMS(t)
	moved := s.target(kms, nil)
	moved.Secret = seal(t, kms, s.key, delivery.SecretAAD("another"))
	other, err := s3client.New(moved, sealed.KMS{KMS: kms}, s.egressClient(0), s3client.Options{Source: source, Clock: porttest.NewClock(siteNow)})
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.seen())
	_, err = other.Check(t.Context(), "replica")
	failsWith(t, "a secret that does not open", err, delivery.ClassInternal)
	if !errors.Is(err, s3client.ErrCredential) {
		t.Errorf("error %v is not ErrCredential", err)
	}
	if len(s.seen()) != before {
		t.Errorf("a request was sent without a credential")
	}
}

// conflictSite answers every write 409 OperationAborted - a completion
// with completion as its 200 body when that is set - and every HEAD as head
// says, and records the versionId each HEAD asked for.
func conflictSite(t *testing.T, head http.HandlerFunc, completion string) (*site, *atomic.Value) {
	t.Helper()
	asked := &atomic.Value{}
	s := newSite(t)
	s.set(func(s *site) {
		s.script = func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			switch {
			case r.Method == http.MethodHead:
				asked.Store(r.URL.Query().Get("versionId"))
				head(w, r)
			case completion != "" && r.Method == http.MethodPost:
				_, _ = io.WriteString(w, completion)
			default:
				answer(http.StatusConflict, "OperationAborted")(w, r)
			}
		}
	})
	return s, asked
}

// TestConflictAtTheSite: the 409 -> HeadVersion path against the site
// itself. A copy sent again is answered 409 OperationAborted, asked about
// with HEAD of its version - the site's own answer - and is done; the same
// ID with other bytes is not (server); a marker sent again is there (a
// 405); the null version sent again is asked about as "null". In a bucket
// that is no replica of this source every question and change of a source
// is not_replica.
func TestConflictAtTheSite(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", false)
	s.createPlain("plain", false)
	c := s.client(0)

	v := s.version("replica", "doc", []byte("the document"), 3)
	other := s.version("replica", "doc", []byte("other bytes"), 3)
	other.VersionID = v.VersionID
	null := s.version("replica", "nil", []byte("before versions"), 1)
	null.Replica.Null, null.Versioned = true, false
	marker := s.version("replica", "doc", nil, 4)
	marker.DeleteMarker, marker.Size, marker.ETag = true, 0, ""
	for _, first := range []error{
		c.PutReplica(t.Context(), v, strings.NewReader("the document")),
		c.PutReplica(t.Context(), null, strings.NewReader("before versions")),
		c.PutMarker(t.Context(), marker),
	} {
		if first != nil {
			t.Fatalf("the first copy: %v", first)
		}
	}

	tests := []struct {
		name string
		send func() error
		// asked is the versionId the HEAD that follows the 409 names.
		asked string
		want  delivery.Class
	}{
		{"a copy sent again", func() error { return c.PutReplica(t.Context(), v, strings.NewReader("the document")) }, v.VersionID.String(), delivery.ClassNone},
		{"the same ID with other bytes", func() error { return c.PutReplica(t.Context(), other, strings.NewReader("other bytes")) }, v.VersionID.String(), delivery.ClassServer},
		{"the null version sent again", func() error { return c.PutReplica(t.Context(), null, strings.NewReader("before versions")) }, "null", delivery.ClassNone},
		{"a marker sent again", func() error { return c.PutMarker(t.Context(), marker) }, marker.VersionID.String(), delivery.ClassNone},
	}
	for _, tt := range tests {
		before := len(s.seen())
		failsWith(t, tt.name, tt.send(), tt.want)
		requests := s.seen()[before:]
		if len(requests) != 2 || requests[1].method != http.MethodHead || requests[1].query != "versionId="+tt.asked {
			t.Errorf("%s: sent %+v, want the write and HEAD of versionId=%s", tt.name, requests, tt.asked)
			continue
		}
		signedAndBound(t, requests[1], false)
	}

	ref := v.Ref()
	for name, err := range map[string]error{
		"HeadVersion": func() error { _, _, err := c.HeadVersion(t.Context(), "plain", "doc", ref); return err }(),
		"ListReplicaUploads": func() error {
			_, err := c.ListReplicaUploads(t.Context(), "plain", "doc")
			return err
		}(),
		"AbortUpload":  c.AbortUpload(t.Context(), "plain", "doc", newID(t).String()),
		"PutTags":      c.PutTags(t.Context(), "plain", "doc", ref, map[string]string{"a": "b"}),
		"PutLegalHold": c.PutLegalHold(t.Context(), "plain", "doc", ref, true),
	} {
		failsWith(t, name+" in a bucket that is no replica", err, delivery.ClassNotReplica)
	}
}

// TestConflictIsAskedAbout: a 409 OperationAborted of a replica write - or
// one in the body of a completion's 200 - may be a version that is there,
// or a conflict that committed nothing. The version is asked for: the write
// is done only when it is there with the entity tag sent (a marker: there,
// with none), and is ClassServer otherwise.
func TestConflictIsAskedAbout(t *testing.T) {
	t.Parallel()
	const etag = "0123456789abcdef0123456789abcdef"
	withETag := func(tag string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("ETag", `"`+tag+`"`)
			w.WriteHeader(http.StatusOK)
		}
	}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	type send func(ctx context.Context, c *s3client.Client, v object.Version) error
	put := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		return c.PutReplica(ctx, v, strings.NewReader("doc"))
	}
	marker := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		v.DeleteMarker, v.Size, v.ETag = true, 0, ""
		return c.PutMarker(ctx, v)
	}
	complete := func(ctx context.Context, c *s3client.Client, v object.Version) error {
		return c.CompleteReplica(ctx, v, "upload-1", []ports.SitePart{{Number: 1, ETag: "abc"}})
	}
	const conflicted = `<Error><Code>OperationAborted</Code></Error>`
	tests := []struct {
		name       string
		head       http.HandlerFunc
		completion string
		send       send
		null       bool
		want       delivery.Class
	}{
		{"a put, and the version is not there", status(http.StatusNotFound), "", put, false, delivery.ClassServer},
		{"a put, and the version is there", withETag(etag), "", put, false, delivery.ClassNone},
		{"a put, and the null version is there", withETag(etag), "", put, true, delivery.ClassNone},
		{"a put, and the version is there with other bytes", withETag("ffff"), "", put, false, delivery.ClassServer},
		{"a put, and the version probe is refused", status(http.StatusForbidden), "", put, false, delivery.ClassAuth},
		{"a marker, and the marker is there", status(http.StatusMethodNotAllowed), "", marker, false, delivery.ClassNone},
		{"a marker, and a version with an entity tag is there", withETag(etag), "", marker, false, delivery.ClassServer},
		{"a marker, and nothing is there", status(http.StatusNotFound), "", marker, false, delivery.ClassServer},
		{"a completion, and the version is there", withETag(etag), "", complete, false, delivery.ClassNone},
		{"a completion, and the version is not there", status(http.StatusNotFound), "", complete, false, delivery.ClassServer},
		{"a completion's 200 that conflicted, and the version is there", withETag(etag), conflicted, complete, false, delivery.ClassNone},
		{"a completion's 200 that conflicted, and the version is not there", status(http.StatusNotFound), conflicted, complete, false, delivery.ClassServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, asked := conflictSite(t, tt.head, tt.completion)
			c := s.client(0)
			v := s.versionWithoutSite("replica", "doc", []byte("doc"), 7)
			v.ETag = etag
			if tt.null {
				v.Replica.Null, v.Versioned = true, false
			}
			failsWith(t, tt.name, tt.send(t.Context(), c, v), tt.want)
			want := v.VersionID.String()
			if tt.null {
				want = "null"
			}
			if got, _ := asked.Load().(string); got != want {
				t.Errorf("the HEAD asked for %q, want %q", got, want)
			}
		})
	}
}

// watchedReader is a src of zero bytes that records the reads of it that
// began after, or were still going on when, the call it was given to
// returned. Each read takes a while, so that one is in flight when an
// answer arrives.
type watchedReader struct {
	left     int64
	returned atomic.Bool
	late     atomic.Int64
}

// Read implements io.Reader.
func (w *watchedReader) Read(p []byte) (int, error) {
	if w.returned.Load() {
		w.late.Add(1)
	}
	defer func() {
		if w.returned.Load() {
			w.late.Add(1)
		}
	}()
	if w.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(2 * time.Millisecond)
	n := int(min(int64(len(p)), w.left, 4<<10))
	clear(p[:n])
	w.left -= int64(n)
	return n, nil
}

// TestNoReadAfterReturn: a site that answers 403 before it reads the body;
// once PutReplica and PutPart have returned, the src they were given is
// read no more - net/http may go on reading a body after RoundTrip returns,
// and the client waits for that read to end.
func TestNoReadAfterReturn(t *testing.T) {
	t.Parallel()
	const size = 8 << 20
	s := newSite(t)
	s.set(func(s *site) {
		s.script = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }
	})
	c := s.client(0)
	s3client.SetChunkSize(c, 4<<10)
	calls := map[string]func(src io.Reader) error{
		"PutReplica": func(src io.Reader) error {
			v := s.versionWithoutSite("replica", "doc", nil, 3)
			v.Size = size
			return c.PutReplica(t.Context(), v, src)
		},
		"PutPart": func(src io.Reader) error {
			_, err := c.PutPart(t.Context(), "replica", "doc", "upload-1", 1, size, src)
			return err
		},
	}
	for name, call := range calls {
		src := &watchedReader{left: size}
		err := call(src)
		src.returned.Store(true)
		if delivery.ClassOf(err) == delivery.ClassNone {
			t.Errorf("%s: no failure of a 403", name)
		}
		time.Sleep(100 * time.Millisecond)
		if late := src.late.Load(); late != 0 {
			t.Errorf("%s: the src was read %d times after the call returned", name, late)
		}
	}
}

// TestRetentionWithinTheMargin: a retention that ends 1 s after the
// client's clock, at a site whose clock is 2 s ahead, is left out - sent,
// the site would refuse the copy and the version would be FAILED - and the
// copy is taken; one that ends past RetentionMargin is sent.
func TestRetentionWithinTheMargin(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", true)
	clientNow := siteNow.Add(-2 * time.Second)
	c := s.clientAt(0, clientNow)

	soon := s.version("replica", "soon", []byte("soon"), 1)
	soon.Lock.Retention = &object.Retention{Mode: object.Governance, Until: clientNow.Add(time.Second)}
	if err := c.PutReplica(t.Context(), soon, strings.NewReader("soon")); err != nil {
		t.Fatalf("PutReplica of a retention within the margin: %v", err)
	}
	if got := lastFor(t, s.seen(), http.MethodPut, "/replica/soon").header.Get("X-Amz-Object-Lock-Mode"); got != "" {
		t.Errorf("a retention within the margin was sent: %q", got)
	}

	later := s.version("replica", "later", []byte("later"), 2)
	later.Lock.Retention = &object.Retention{Mode: object.Governance, Until: clientNow.Add(s3client.RetentionMargin + time.Minute)}
	if err := c.PutReplica(t.Context(), later, strings.NewReader("later")); err != nil {
		t.Fatalf("PutReplica of a retention past the margin: %v", err)
	}
	if got := lastFor(t, s.seen(), http.MethodPut, "/replica/later").header.Get("X-Amz-Object-Lock-Mode"); got != "GOVERNANCE" {
		t.Errorf("a retention past the margin was not sent: %q", got)
	}

	upload := s.version("replica", "upload", []byte("upload"), 3)
	upload.Lock.Retention = soon.Lock.Retention
	if _, err := c.CreateUpload(t.Context(), upload); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if got := lastFor(t, s.seen(), http.MethodPost, "/replica/upload").header.Get("X-Amz-Object-Lock-Mode"); got != "" {
		t.Errorf("CreateUpload sent a retention within the margin: %q", got)
	}
}

// randomInt returns a number from 0 up to n, drawn from crypto/rand.
func randomInt(t *testing.T, n int) int {
	t.Helper()
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		t.Fatal(err)
	}
	return int(v.Int64())
}

// TestChunksThroughTheServer is the property of the chunked encoding
// against the real handler: versions of random sizes, in chunks of random
// sizes, are taken by the site's verification and read back byte for byte.
func TestChunksThroughTheServer(t *testing.T) {
	t.Parallel()
	s := newSite(t)
	s.createBucket("replica", false)
	c := s.client(0)
	for i := range 24 {
		size := randomInt(t, 48<<10)
		if i == 0 {
			size = 0
		}
		chunk := 16 + randomInt(t, 16<<10)
		s3client.SetChunkSize(c, chunk)
		content := make([]byte, size)
		if _, err := rand.Read(content); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("p-%d", i)
		if err := c.PutReplica(t.Context(), s.version("replica", key, content, uint64(i+1)), bytes.NewReader(content)); err != nil {
			t.Fatalf("size %d in chunks of %d: %v", size, chunk, err)
		}
		if resp, got := s.root(http.MethodGet, "/replica/"+key, nil, nil); resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
			t.Fatalf("size %d in chunks of %d: GET = %d, %d bytes", size, chunk, resp.StatusCode, len(got))
		}
	}
}
