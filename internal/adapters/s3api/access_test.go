package s3api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestReadsAreCountedThroughTheAPI: what S3 clients do, and what is recorded
// of it (ADR-0016). A GET answered 200 or 206 is a download - signed,
// presigned or by nobody in particular; HEAD, 304, 412 and a GET that failed
// are none; the source of a copy is counted apart. Whoever holds a presigned
// URL reads as its signer, marked as such.
func TestReadsAreCountedThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	s.createBucket("site")
	body := randomPart(t, object.MinPartSize)
	s.putObject("/site/index.html", body)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/site?policy", body: []byte(sitePolicy)}), http.StatusNoContent)
	const path = "/site/index.html"
	get := func(headers map[string]string) reply {
		return s.do(call{method: http.MethodGet, path: path, headers: headers})
	}
	anon := func(method string) reply { return s.do(call{method: method, path: path, unsigned: true}) }
	presigned := func(method string) reply {
		return s.do(call{method: method, path: s.presign(method, path, serverNow, 600, nil), unsigned: true})
	}

	// Signed by root: two downloads.
	s.mustStatus(get(nil), http.StatusOK)
	s.mustStatus(get(map[string]string{"Range": "bytes=0-99"}), http.StatusPartialContent)
	// None of these is a download.
	s.mustStatus(s.do(call{method: http.MethodHead, path: path}), http.StatusOK)
	s.mustStatus(get(map[string]string{"If-None-Match": etagOf(body)}), http.StatusNotModified)
	s.mustStatus(get(map[string]string{"If-Match": `"another"`}), http.StatusPreconditionFailed)
	s.mustStatus(get(map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(body)+1)}), http.StatusRequestedRangeNotSatisfiable)
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/site/nope"}), http.StatusNotFound)
	s.mustStatus(anon(http.MethodHead), http.StatusOK)
	s.mustStatus(presigned(http.MethodHead), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/site?list-type=2"}), http.StatusOK)
	// By nobody in particular, which the bucket's policy allows: one.
	s.mustStatus(anon(http.MethodGet), http.StatusOK)
	// With a URL root signed: three, apart from root's own.
	s.mustStatus(presigned(http.MethodGet), http.StatusOK)
	s.mustStatus(presigned(http.MethodGet), http.StatusOK)
	ranged := s.presign(http.MethodGet, path, serverNow, 600, map[string]string{"Range": "bytes=5-9"})
	s.mustStatus(s.do(call{method: http.MethodGet, path: ranged, unsigned: true, headers: map[string]string{"Range": "bytes=5-9"}}), http.StatusPartialContent)
	// A presigned URL that does not verify reads nothing.
	s.mustStatus(s.do(call{method: http.MethodGet, path: s.presign(http.MethodGet, path, serverNow.Add(-2*time.Hour), 600, nil), unsigned: true}), http.StatusForbidden)

	// The source of a copy, and of a copied part; a copy that is refused
	// is none.
	copyHeaders := map[string]string{"x-amz-copy-source": path}
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/site/copy.html", headers: copyHeaders}), http.StatusOK)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/no-such-bucket/copy.html", headers: copyHeaders}), http.StatusNotFound)
	upload := s.createUpload("/site/assembled", nil)
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/site/assembled?uploadId=" + upload + "&partNumber=1", headers: copyHeaders}), http.StatusOK)

	if err := s.access.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got, err := s.meta.ObjectAccess(ctx, "site", "index.html", object.VersionRef{Null: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Downloads != 6 || got.Copies != 2 || got.OtherReaders != 0 {
		t.Errorf("recorded %d downloads and %d copies, want 6 and 2", got.Downloads, got.Copies)
	}
	type row struct {
		who       string
		presigned bool
		downloads int64
		copies    int64
	}
	var rows []row
	for _, r := range got.Readers {
		rows = append(rows, row{r.Who, r.Presigned, r.Downloads, r.Copies})
	}
	want := []row{{"anonymous", false, 1, 0}, {"root", false, 2, 2}, {"root", true, 3, 0}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("readers = %v, want %v", rows, want)
	}
	// The copy is an object of its own, read by nobody yet, and says who
	// made it.
	access, err := s.meta.ObjectAccess(ctx, "site", "copy.html", object.VersionRef{Null: true})
	if err != nil || !access.Empty() {
		t.Errorf("access of the copy = %+v, %v", access, err)
	}
	if v, err := s.meta.GetObject(ctx, "site", "copy.html"); err != nil || v.CreatedBy != "root" {
		t.Errorf("the copy was made by %q, %v", v.CreatedBy, err)
	}
}

// TestPresignedRequestsSayTheyAre: the principal of a request that was
// authenticated by its URL says so, and that of a signed request does not -
// it is all the use cases know of how a caller came by its identity.
func TestPresignedRequestsSayTheyAre(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	h, ok := s.Config.Handler.(*Handler)
	if !ok {
		t.Fatalf("the test server serves a %T", s.Config.Handler)
	}
	for name, tt := range map[string]struct {
		path      string
		sign      bool
		presigned bool
		anonymous bool
	}{
		"a signed request":    {"/bkt/k", true, false, false},
		"a presigned request": {s.presign(http.MethodGet, "/bkt/k", serverNow, 600, nil), false, true, false},
		"an unsigned request": {"/bkt/k", false, false, true},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+tt.path, nil)
		if tt.sign {
			empty := sha256.Sum256(nil)
			signRequest(r, s.keyID, s.key, serverNow, hex.EncodeToString(empty[:]))
		}
		req := &request{Request: r, id: "test"}
		if err := h.authenticate(t.Context(), req); err != nil {
			t.Fatalf("%s: authenticate: %v", name, err)
		}
		if req.principal.Session.Presigned != tt.presigned || req.principal.Anonymous() != tt.anonymous {
			t.Errorf("%s: principal = %+v", name, req.principal)
		}
	}
}
