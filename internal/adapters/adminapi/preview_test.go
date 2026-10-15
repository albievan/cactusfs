package adminapi_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/preview"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// pngOf is a PNG of the size given, n bytes long.
func pngOf(width, height uint32, n int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = binary.BigEndian.AppendUint32(b, width)
	b = binary.BigEndian.AppendUint32(b, height)
	b = append(b, 8, 6, 0, 0, 0, 0, 0, 0, 0)
	return append(b, bytes.Repeat([]byte{0x42}, max(0, n-len(b)))...)
}

// previewFixture is a fixture with a bucket, and the object service of its
// handler at hand.
type previewFixture struct {
	*fixture
	objects *app.ObjectService
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	return newPreviewFixtureWith(t, nil)
}

// newPreviewFixtureWith is newPreviewFixture with a handler whose
// configuration the test completes.
func newPreviewFixtureWith(t *testing.T, tune func(*adminapi.Config)) *previewFixture {
	t.Helper()
	var objects *app.ObjectService
	f := newFixtureWith(t, func(c *adminapi.Config) {
		objects = c.Objects
		if tune != nil {
			tune(c)
		}
	})
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	return &previewFixture{fixture: f, objects: objects}
}

// store uploads an object through the API, as the bearer given.
func (f *previewFixture) store(bearer, key, contentType string, data []byte) {
	f.t.Helper()
	f.must(http.StatusOK, call{
		method: "PUT", path: "/buckets/photos/object?key=" + key, raw: data, bearer: bearer,
		headers: map[string]string{"Content-Type": contentType},
	})
}

// The headers of every preview, whatever it holds (ADR-0028).
var previewHeaders = map[string]string{
	"Content-Type":                 "application/octet-stream",
	"X-Content-Type-Options":       "nosniff",
	"Content-Security-Policy":      "sandbox; default-src 'none'",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Cache-Control":                "no-store",
}

// TestPreview: the bytes, as an attachment of an opaque type that nothing
// renders, with what they are and how many of them there are.
func TestPreview(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	alice := f.userKey("alice", iam.RoleReadWrite)
	image := pngOf(320, 200, 7000)
	f.store(alice, "a/cat.png", "image/png", image)
	text := bytes.Repeat([]byte("0123456789abcdef"), app.PreviewTextSize/16+1)
	f.store(alice, "long.txt", "text/plain; charset=utf-8", text)
	var metadata struct {
		ETag string `json:"etag"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/metadata?key=a/cat.png"}).decode(t, &metadata)

	tests := []struct {
		name      string
		path      string
		body      []byte
		kind      preview.Kind
		truncated string
	}{
		{name: "an image", path: "/buckets/photos/object/preview?key=a/cat.png", body: image, kind: preview.KindPNG, truncated: "false"},
		{name: "a version by name", path: "/buckets/photos/object/preview?key=a/cat.png&version_id=null", body: image, kind: preview.KindPNG, truncated: "false"},
		{name: "the first MiB of text", path: "/buckets/photos/object/preview?key=long.txt", body: text[:app.PreviewTextSize], kind: preview.KindText, truncated: "true"},
	}
	for _, tt := range tests {
		r := f.must(http.StatusOK, call{method: "GET", path: tt.path, bearer: alice})
		for name, want := range previewHeaders {
			if got := r.header.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", tt.name, name, got, want)
			}
		}
		if got := r.header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
			t.Errorf("%s: Content-Disposition = %q", tt.name, got)
		}
		if r.header.Get("X-CactusFS-Preview-Kind") != string(tt.kind) || r.header.Get("X-CactusFS-Preview-Truncated") != tt.truncated ||
			r.header.Get("Content-Length") != strconv.Itoa(len(tt.body)) || !bytes.Equal(r.body, tt.body) || r.header.Get("Accept-Ranges") != "" {
			t.Errorf("%s: headers %v, %d bytes", tt.name, r.header, len(r.body))
		}
		if tt.kind == preview.KindPNG && r.header.Get("ETag") != metadata.ETag {
			t.Errorf("%s: ETag %q, want %q", tt.name, r.header.Get("ETag"), metadata.ETag)
		}
	}
	// A range is not a preview's: the whole is sent.
	r := f.must(http.StatusOK, call{method: "GET", path: tests[0].path, bearer: alice, headers: map[string]string{"Range": "bytes=0-9"}})
	if !bytes.Equal(r.body, image) {
		t.Errorf("a preview asked with a Range: %d bytes", len(r.body))
	}
	// HEAD would read and decrypt the object for nothing.
	if r := f.do(call{method: "HEAD", path: tests[0].path, bearer: alice}); r.status != http.StatusMethodNotAllowed || r.header.Get("Allow") != http.MethodGet {
		t.Errorf("HEAD = %d, Allow %q", r.status, r.header.Get("Allow"))
	}
	// Counted as downloads, by who previewed.
	if err := f.access.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var access struct {
		DownloadCount int64 `json:"download_count"`
		Readers       []struct {
			Who string `json:"who"`
		} `json:"readers"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/access?key=a/cat.png"}).decode(t, &access)
	if access.DownloadCount != 3 || len(access.Readers) != 1 || access.Readers[0].Who != "user/alice" {
		t.Errorf("access = %+v", access)
	}
}

// TestPreviewProblems: every refusal of the contract, as problem+json with
// its code, and nothing of the object sent with it.
func TestPreviewProblems(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	owner := f.userKey("alice", iam.RoleReadWrite)
	auditor := f.userKey("carol", iam.RoleAuditor)
	html := []byte("<!doctype html><script>document.cookie</script>")
	f.store(owner, "page.png", "image/png", html)
	f.store(owner, "page.html", "text/html", html)
	f.store(owner, "huge.png", "image/png", pngOf(65535, 65535, 500))
	f.store(owner, "cat.png", "image/png", pngOf(10, 10, 500))
	ctx := context.Background()
	data := pngOf(10, 10, 500)
	if _, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{
		Bucket: "photos", Key: "secret.png", Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: "image/png",
		SSE: object.SSE{Mode: object.SSEC, CustomerKey: bytes.Repeat([]byte{7}, object.KeySize)},
	}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		bearer string
		status int
		code   string
	}{
		{name: "neither owner nor assigned", path: "cat.png", bearer: auditor, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "no such bucket", path: "/buckets/nothing/object/preview?key=cat.png", status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "no such object", path: "nothing.png", status: http.StatusNotFound, code: "NoSuchObject"},
		{name: "no such version", path: "cat.png&version_id=" + strings.Repeat("ab", 16), status: http.StatusNotFound, code: "NoSuchVersion"},
		{name: "a version ID that is none", path: "cat.png&version_id=latest", status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "SSE-C", path: "secret.png", status: http.StatusConflict, code: "CustomerKeyRequired"},
		{name: "too many pixels", path: "huge.png", status: http.StatusRequestEntityTooLarge, code: "PreviewTooLarge"},
		{name: "HTML stored as HTML", path: "page.html", status: http.StatusUnsupportedMediaType, code: "NotPreviewable"},
		{name: "HTML stored as PNG", path: "page.png", status: http.StatusUnsupportedMediaType, code: "NotPreviewable"},
	}
	for _, tt := range tests {
		path := tt.path
		if !strings.HasPrefix(path, "/") {
			path = "/buckets/photos/object/preview?key=" + path
		}
		bearer := tt.bearer
		if bearer == "" {
			bearer = f.root
		}
		r := f.do(call{method: "GET", path: path, bearer: bearer})
		if r.status != tt.status || r.problem(t)["code"] != tt.code {
			t.Errorf("%s: %d %s, want %d %s", tt.name, r.status, r.body, tt.status, tt.code)
		}
		if bytes.Contains(r.body, []byte("<script>")) || r.header.Get("X-CactusFS-Preview-Kind") != "" {
			t.Errorf("%s: something of the object was sent: %s", tt.name, r.body)
		}
	}
	// The download of SSE-C through this API keeps its answer.
	if r := f.do(call{method: "GET", path: "/buckets/photos/object?key=secret.png", bearer: f.root}); r.status != http.StatusBadRequest {
		t.Errorf("a download of an SSE-C object = %d %s", r.status, r.body)
	}
	// The refusal is in the audit trail, with the action refused.
	denied := false
	for _, record := range f.trail() {
		if record.Outcome == audit.Denied && record.Actor == "user/carol" && record.Resource == "arn:aws:s3:::photos/cat.png" {
			denied = true
		}
	}
	if !denied {
		t.Error("the refused preview is not in the audit trail")
	}
}

// TestTooManyPreviews: 429 with Retry-After, while the caller has as many
// previews running as it may.
func TestTooManyPreviews(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	f.store(f.root, "cat.png", "image/png", pngOf(10, 10, 500))
	ctx := context.Background()
	// Root's previews, as the use case knows the root identity: who asks
	// over the API is the same caller.
	var open []io.Closer
	for range app.PreviewsPerPrincipal {
		out, err := f.objects.Preview(ctx, porttest.Root(), app.PreviewInput{Bucket: "photos", Key: "cat.png"})
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, out.Body)
	}
	r := f.do(call{method: "GET", path: "/buckets/photos/object/preview?key=cat.png", bearer: f.root})
	if r.status != http.StatusTooManyRequests || r.problem(t)["code"] != "TooManyRequests" || r.header.Get("Retry-After") != "1" {
		t.Errorf("a third preview = %d, Retry-After %q, %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	for _, body := range open {
		_ = body.Close()
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/preview?key=cat.png"})
}

// TestPreviewInAConsoleSession: a preview is a read - it needs the session
// cookie and no CSRF token - and a credential of some kind.
func TestPreviewInAConsoleSession(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	browser := consoleOf(t, f.fixture, "dora", iam.RoleReadOnly)
	image := pngOf(10, 10, 500)
	f.store(f.root, "cat.png", "image/png", image)
	const path = "/buckets/photos/object/preview?key=cat.png"

	r := f.do(call{method: "GET", path: path, client: browser.client})
	if r.status != http.StatusOK || !bytes.Equal(r.body, image) || r.header.Get("X-CactusFS-Preview-Kind") != "png" {
		t.Errorf("a preview in a session without a CSRF token = %d %s", r.status, r.body)
	}
	if r := f.do(call{method: "GET", path: path, client: browser.client, headers: map[string]string{httpsession.CSRFHeader: browser.csrf}}); r.status != http.StatusOK {
		t.Errorf("with the token = %d", r.status)
	}
	if r := f.do(call{method: "GET", path: path}); r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
		t.Errorf("without a credential = %d %s", r.status, r.body)
	}
}

// TestAGIFFramePastItsScreenCutsThePreviewOff: a frame found too large once
// the answer has begun ends the transfer short of its length - the client
// sees an error, never a shorter image.
func TestAGIFFramePastItsScreenCutsThePreviewOff(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	frame := func(width, height uint16) []byte {
		b := []byte{0x2C, 0, 0, 0, 0}
		b = binary.LittleEndian.AppendUint16(b, width)
		b = binary.LittleEndian.AppendUint16(b, height)
		return append(b, 0x00, 0x02, 0x02, 0x4C, 0x01, 0x00)
	}
	gif := append([]byte("GIF89a\x1e\x00\x14\x00\x00\x00\x00"), frame(30, 20)...)
	gif = append(gif, 0x21, 0xFE)
	for range (preview.BoundsHeadSize + 5000) / 255 {
		gif = append(append(gif, 255), bytes.Repeat([]byte{'c'}, 255)...)
	}
	gif = append(append(append(gif, 0x00), frame(65535, 65535)...), 0x3B)
	f.store(f.root, "far.gif", "image/gif", gif)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+adminapi.BasePath+"/buckets/photos/object/preview?key=far.gif", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.root)
	resp, err := f.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(gif)) || err == nil || len(got) >= len(gif) {
		t.Fatalf("status %d, length %d, read %d of %d bytes: %v", resp.StatusCode, resp.ContentLength, len(got), len(gif), err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("the transfer ended with %v, want an unexpected EOF", err)
	}
	// No fault of the node: a warning, not an error.
	if level := abortedAt(t, f.logs.String()); level != "WARN" {
		t.Errorf("the preview cut off was logged at %q, want WARN; log:\n%s", level, f.logs.String())
	}
}

// abortedAt returns the level the log says "response aborted" at, once; ""
// when it does not say it.
func abortedAt(t *testing.T, log string) string {
	t.Helper()
	level := ""
	for line := range strings.Lines(log) {
		var entry struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Msg != "response aborted" {
			continue
		}
		if level != "" {
			t.Errorf("the response was aborted twice: %s", log)
		}
		level = entry.Level
	}
	return level
}

// TestAStalledPreviewGivesItsPlaceBack: a client that stops taking the
// bytes of a preview does not keep the place it holds among the previews:
// the transfer ends once a write has waited for the stall, the body is
// closed and the place is free again.
func TestAStalledPreviewGivesItsPlaceBack(t *testing.T) {
	t.Parallel()
	const stall = time.Second
	f := newPreviewFixtureWith(t, func(c *adminapi.Config) { c.PreviewStall = stall })
	// Far more than the buffers of a connection hold.
	f.store(f.root, "big.png", "image/png", pngOf(10, 10, 24<<20))

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+adminapi.BasePath+"/buckets/photos/object/preview?key=big.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.root)
	resp, err := f.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	// The body is not read, and not closed until the end.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ctx := context.Background()
	in := app.PreviewInput{Bucket: "photos", Key: "big.png"}
	// Root's places, as the use case knows the root identity: one is held
	// by the stalled transfer.
	held, err := f.objects.Preview(ctx, porttest.Root(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Body.Close() }()
	if _, err := f.objects.Preview(ctx, porttest.Root(), in); !errors.Is(err, domain.ErrTooManyRequests) {
		t.Fatalf("while the stalled preview holds its place: %v", err)
	}
	deadline := time.Now().Add(stall + 10*time.Second)
	for {
		out, err := f.objects.Preview(ctx, porttest.Root(), in)
		if err == nil {
			_ = out.Body.Close()
			break
		}
		if !errors.Is(err, domain.ErrTooManyRequests) || time.Now().After(deadline) {
			t.Fatalf("the stalled preview still holds its place: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if level := abortedAt(t, f.logs.String()); level != "WARN" {
		t.Errorf("the stalled preview was logged at %q, want WARN", level)
	}
}

// TestAnAbortedDownloadIsAnError: a download that fails once it has begun -
// its data damaged - is logged as the error of the node it is.
func TestAnAbortedDownloadIsAnError(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	f.store(f.root, "cat.png", "image/png", pngOf(10, 10, 4<<20))
	for _, id := range f.drive.IDs() {
		data, _ := f.drive.Bytes(id)
		damaged := bytes.Clone(data)
		damaged[len(damaged)-100] ^= 0xFF
		f.drive.Corrupt(id, damaged)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+adminapi.BasePath+"/buckets/photos/object?key=cat.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.root)
	resp, err := f.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err == nil {
		t.Fatalf("status %d, read: %v", resp.StatusCode, err)
	}
	if level := abortedAt(t, f.logs.String()); level != "ERROR" {
		t.Errorf("an aborted download was logged at %q, want ERROR", level)
	}
}

// TestASlowPreviewIsNoStalledOne: the stall bounds each write, not the
// transfer: a client that takes longer than it in all, and keeps taking
// bytes, gets every one of them.
func TestASlowPreviewIsNoStalledOne(t *testing.T) {
	t.Parallel()
	const stall = 500 * time.Millisecond
	f := newPreviewFixtureWith(t, func(c *adminapi.Config) { c.PreviewStall = stall })
	image := pngOf(10, 10, 8<<20)
	f.store(f.root, "big.png", "image/png", image)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+adminapi.BasePath+"/buckets/photos/object/preview?key=big.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.root)
	resp, err := f.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	begun := time.Now()
	var got bytes.Buffer
	for {
		n, err := io.CopyN(&got, resp.Body, 1<<20)
		if errors.Is(err, io.EOF) || (err == nil && n == 0) {
			break
		}
		if err != nil {
			t.Fatalf("after %d bytes: %v", got.Len(), err)
		}
		time.Sleep(stall / 4)
	}
	if !bytes.Equal(got.Bytes(), image) || time.Since(begun) <= stall {
		t.Fatalf("read %d of %d bytes in %s", got.Len(), len(image), time.Since(begun))
	}
}
