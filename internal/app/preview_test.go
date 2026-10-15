package app_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/preview"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// countingDrive counts the shards opened for reading.
type countingDrive struct {
	ports.ShardStore
	opens *atomic.Int64
}

func (d countingDrive) Open(ctx context.Context, id object.ShardID) (ports.ShardReader, error) {
	d.opens.Add(1)
	return d.ShardStore.Open(ctx, id)
}

// previewFixture is an access fixture whose object service counts the
// shards it opens, with a cap of previews of its own.
type previewFixture struct {
	*accessFixture
	opens *atomic.Int64
}

func newPreviewFixture(t *testing.T, maxSize int64) *previewFixture {
	t.Helper()
	f := &previewFixture{accessFixture: newAccessFixture(t), opens: new(atomic.Int64)}
	f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) {
		cfg.Access, cfg.AccessStore = f.recorder, f.meta
		drives := make([]ports.ShardStore, len(f.drives))
		for i, d := range f.drives {
			drives[i] = countingDrive{ShardStore: d, opens: f.opens}
		}
		cfg.Drives = fixed(t, f.topology, drives)
		cfg.PreviewMaxSize = maxSize
	})
	return f
}

// store puts an object as p, of the type given.
func (f *previewFixture) store(t *testing.T, p iam.Principal, key, contentType string, data []byte) object.Version {
	t.Helper()
	v, err := f.objects.Put(context.Background(), p, app.PutInput{
		Bucket: "b", Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: contentType,
	})
	if err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
	return v
}

// look previews an object as p and reads it to its end.
func (f *previewFixture) look(p iam.Principal, in app.PreviewInput) (app.PreviewOutput, []byte, error) {
	out, err := f.objects.Preview(context.Background(), p, in)
	if err != nil {
		return out, nil, err
	}
	data, err := io.ReadAll(out.Body)
	if closeErr := out.Body.Close(); err == nil {
		err = closeErr
	}
	return out, data, err
}

// pngOf is a PNG of the size given, n bytes long.
func pngOf(width, height uint32, n int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = binary.BigEndian.AppendUint32(b, width)
	b = binary.BigEndian.AppendUint32(b, height)
	b = append(b, 8, 6, 0, 0, 0, 0, 0, 0, 0)
	return append(b, bytes.Repeat([]byte{0x42}, max(0, n-len(b)))...)
}

// jpegOf is a JPEG of the size given whose frame header comes after the
// number of segments of metadata given, each of 60 000 bytes, and some data
// after it.
func jpegOf(width, height uint16, segments int) []byte {
	const size = 60000
	b := []byte{0xFF, 0xD8}
	for range segments {
		b = binary.BigEndian.AppendUint16(append(b, 0xFF, 0xE1), size+2)
		b = append(b, bytes.Repeat([]byte{'x'}, size)...)
	}
	b = append(b, 0xFF, 0xC0, 0x00, 0x11, 0x08)
	b = binary.BigEndian.AppendUint16(b, height)
	b = binary.BigEndian.AppendUint16(b, width)
	b = append(b, bytes.Repeat([]byte{0x01}, 10)...)
	return append(b, 0xFF, 0xD9)
}

// gifOf is a GIF of a 30 × 20 screen with one frame that fits, a comment of
// n bytes, and then a frame of the size given.
func gifOf(n int, width, height uint16) []byte {
	b := []byte("GIF89a\x1e\x00\x14\x00\x00\x00\x00")
	frame := func(width, height uint16) []byte {
		f := []byte{0x2C, 0, 0, 0, 0}
		f = binary.LittleEndian.AppendUint16(f, width)
		f = binary.LittleEndian.AppendUint16(f, height)
		return append(f, 0x00, 0x02, 0x02, 0x4C, 0x01, 0x00)
	}
	b = append(b, frame(30, 20)...)
	b = append(b, 0x21, 0xFE)
	for n > 0 {
		size := min(n, 255)
		b = append(append(b, byte(size)), bytes.Repeat([]byte{'c'}, size)...)
		n -= size
	}
	b = append(b, 0x00)
	b = append(b, frame(width, height)...)
	return append(b, 0x3B)
}

// policyOf is a principal of the user name given with the statements given.
func policyOf(name string, statements ...iam.Statement) iam.Principal {
	return iam.Principal{
		AccessKeyID: "KEY" + name, User: name,
		Policies: []iam.Document{{Version: iam.PolicyVersion, Statements: statements}},
	}
}

func allow(actions ...string) iam.Statement {
	return iam.Statement{Effect: iam.Allow, Actions: actions, Resources: []string{"*"}}
}

func deny(actions ...string) iam.Statement {
	return iam.Statement{Effect: iam.Deny, Actions: actions, Resources: []string{"*"}}
}

// TestPreviewWhoMay: every row of the rule of ADR-0028, decided through the
// one authorizer - first the download, then the preview of the version
// resolved, with its maker as the owner.
func TestPreviewWhoMay(t *testing.T) {
	t.Parallel()
	const (
		get        = string(iam.ActionGetObject)
		getVersion = string(iam.ActionGetObjectVersion)
		look       = string(iam.ActionPreviewObject)
	)
	withBoundary := func(p iam.Principal, actions ...string) iam.Principal {
		boundary := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{allow(actions...)}}
		p.Boundary = &boundary
		return p
	}
	locked := policyOf("alice", allow("*"))
	locked.LockedSession = true
	builtIn := func(name, role string) iam.Principal { return roleUser(name, role) }

	tests := []struct {
		name      string
		principal iam.Principal
		// key is "alice.png", which alice made, unless named.
		key       string
		versioned bool
		want      error
	}{
		{name: "owner with the download", principal: policyOf("alice", allow(get))},
		{name: "owner without the download", principal: policyOf("alice", allow(look)), want: domain.ErrAccessDenied},
		{name: "owner without any policy", principal: policyOf("alice"), want: domain.ErrAccessDenied},
		{name: "assigned", principal: policyOf("bob", allow(get, look))},
		{name: "neither owner nor assigned", principal: policyOf("bob", allow(get)), want: domain.ErrAccessDenied},
		{name: "a Deny of the preview wins over the owner", principal: policyOf("alice", allow(get), deny(look)), want: domain.ErrAccessDenied},
		{name: "a Deny of the preview wins over the assignment", principal: policyOf("bob", allow(get, look), deny(look)), want: domain.ErrAccessDenied},
		{name: "a Deny of the download refuses an assignee", principal: policyOf("bob", allow(get, look), deny(get)), want: domain.ErrAccessDenied},
		{name: "a Deny of the download refuses the owner", principal: policyOf("alice", allow(get), deny(get)), want: domain.ErrAccessDenied},
		{name: "the owner's key without the preview", principal: withBoundary(policyOf("alice", allow(get)), get), want: domain.ErrAccessDenied},
		{name: "the owner's key with both", principal: withBoundary(policyOf("alice", allow(get)), get, look)},
		{name: "an assignee's key without the preview", principal: withBoundary(policyOf("bob", allow(get, look)), get), want: domain.ErrAccessDenied},
		{name: "a key never exceeds its owner", principal: withBoundary(policyOf("bob", allow(get)), get, look), want: domain.ErrAccessDenied},
		{name: "a locked session", principal: locked, want: domain.ErrSessionLocked},
		{name: "root", principal: porttest.Root()},
		{name: "anonymous", principal: anonymous(), want: domain.ErrAccessDenied},
		{name: "auditor", principal: builtIn("carol", iam.RoleAuditor), want: domain.ErrAccessDenied},
		{name: "read-only", principal: builtIn("dave", iam.RoleReadOnly)},
		{name: "read-write", principal: builtIn("erin", iam.RoleReadWrite)},
		{name: "bucket-admin", principal: builtIn("finn", iam.RoleBucketAdmin)},
		{name: "cluster-admin", principal: builtIn("gail", iam.RoleClusterAdmin)},
		{name: "a version root made is nobody's", principal: policyOf("alice", allow(get)), key: "root.png", want: domain.ErrAccessDenied},
		{name: "a version root made, assigned", principal: policyOf("bob", allow(get, look)), key: "root.png"},
		{name: "a version by name needs its own download", principal: policyOf("alice", allow(get)), versioned: true, want: domain.ErrAccessDenied},
		{name: "a version by name, owner", principal: policyOf("alice", allow(getVersion)), versioned: true},
		{name: "a version by name, assigned", principal: policyOf("bob", allow(getVersion, look)), versioned: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPreviewFixture(t, 0)
			data := pngOf(64, 48, 5000)
			alice := userNamed("alice", "s3:PutObject")
			made := f.store(t, alice, "alice.png", "image/png", data)
			f.store(t, porttest.Root(), "root.png", "image/png", data)
			if made.CreatedBy != "user/alice" {
				t.Fatalf("CreatedBy = %q", made.CreatedBy)
			}
			key := tt.key
			if key == "" {
				key = "alice.png"
			}
			in := app.PreviewInput{Bucket: "b", Key: key}
			if tt.versioned {
				in.VersionID = object.NullVersion
			}
			before := len(f.authz.Requests())
			out, got, err := f.look(tt.principal, in)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("Preview = %v, want %v", err, tt.want)
				}
				if f.opens.Load() != 0 {
					t.Fatalf("%d shards were opened for a refused preview", f.opens.Load())
				}
				return
			}
			if err != nil || out.Kind != preview.KindPNG || !bytes.Equal(got, data) || out.Length != int64(len(data)) || out.Truncated {
				t.Fatalf("Preview = %+v, %d bytes, %v", out, len(got), err)
			}
			// The questions asked: the download, then the preview, of the
			// version that was resolved and with its maker.
			asked := f.authz.Requests()[before:]
			wantDownload := iam.ActionGetObject
			if tt.versioned {
				wantDownload = iam.ActionGetObjectVersion
			}
			resource := iam.Resource{Bucket: "b", Key: key}
			if len(asked) != 2 || asked[0].Action != wantDownload || asked[0].Resource != resource || asked[0].ObjectCreator != "" ||
				asked[1].Action != iam.ActionPreviewObject || asked[1].Resource != resource || asked[1].ObjectCreator != out.Version.CreatedBy {
				t.Fatalf("asked %+v", asked)
			}
		})
	}
}

// TestPreviewOwnsTheVersionShown: the owner of one version owns that one,
// and the version judged is the version read.
func TestPreviewOwnsTheVersionShown(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, 0)
	f.versioning(t, "b", bucket.VersioningEnabled)
	first := pngOf(10, 10, 3000)
	old := f.store(t, userNamed("alice", "s3:PutObject"), "k", "image/png", first)
	f.store(t, userNamed("bob", "s3:PutObject"), "k", "text/plain", []byte("bob's"))
	alice := policyOf("alice", allow(string(iam.ActionGetObject), string(iam.ActionGetObjectVersion)))

	if _, _, err := f.look(alice, app.PreviewInput{Bucket: "b", Key: "k"}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("the current version, bob's: %v", err)
	}
	out, got, err := f.look(alice, app.PreviewInput{Bucket: "b", Key: "k", VersionID: old.VersionID.String()})
	if err != nil || out.Version.VersionID != old.VersionID || out.Kind != preview.KindPNG || !bytes.Equal(got, first) {
		t.Fatalf("her own version: %+v, %v", out.Version, err)
	}
}

// TestPreviewRefusals: everything a preview is refused for besides who
// asks, and what is refused before a shard is read.
func TestPreviewRefusals(t *testing.T) {
	t.Parallel()
	html := []byte("<!doctype html><script>alert(1)</script>")
	tests := []struct {
		name        string
		contentType string
		data        []byte
		sse         object.SSE
		versionID   string
		key         string
		want        error
		// unread says that no shard may be opened.
		unread bool
	}{
		{name: "SSE-C", contentType: "image/png", data: pngOf(1, 1, 100), sse: object.SSE{Mode: object.SSEC, CustomerKey: customerKey(7)}, want: domain.ErrCustomerKeyRequired, unread: true},
		{name: "HTML", contentType: "text/html", data: html, want: domain.ErrNotPreviewable, unread: true},
		{name: "SVG", contentType: "image/svg+xml", data: []byte("<svg onload='x()'/>"), want: domain.ErrNotPreviewable, unread: true},
		{name: "one byte over the cap", contentType: "image/png", data: pngOf(1, 1, app.MinPreviewMaxSize+1), want: domain.ErrPreviewTooLarge, unread: true},
		{name: "over the cap, untyped", contentType: "", data: pngOf(1, 1, app.MinPreviewMaxSize+1), want: domain.ErrPreviewTooLarge, unread: true},
		{name: "HTML stored as PNG", contentType: "image/png", data: html, want: domain.ErrNotPreviewable},
		{name: "PNG stored as text", contentType: "text/plain", data: pngOf(1, 1, 100), want: domain.ErrNotPreviewable},
		{name: "a huge PNG header", contentType: "image/png", data: pngOf(65535, 65535, 100), want: domain.ErrPreviewTooLarge},
		{name: "a JPEG whose size is past 256 KiB", contentType: "image/jpeg", data: jpegOf(10, 10, preview.BoundsHeadSize/60000+1), want: domain.ErrNotPreviewable},
		{name: "a DOCX stored untyped", contentType: "application/octet-stream", data: []byte("PK\x03\x04rest"), want: domain.ErrNotPreviewable},
		{name: "a version ID that is none", contentType: "image/png", data: pngOf(1, 1, 100), versionID: "latest", want: domain.ErrInvalidArgument, unread: true},
		{name: "no such version", contentType: "image/png", data: pngOf(1, 1, 100), versionID: strings.Repeat("ab", 16), want: domain.ErrVersionNotFound, unread: true},
		{name: "no such object", contentType: "image/png", data: pngOf(1, 1, 100), key: "nothing", want: domain.ErrObjectNotFound, unread: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPreviewFixture(t, app.MinPreviewMaxSize)
			if _, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
				Bucket: "b", Key: "k", Body: bytes.NewReader(tt.data), Size: int64(len(tt.data)), ContentType: tt.contentType, SSE: tt.sse,
			}); err != nil {
				t.Fatal(err)
			}
			key := tt.key
			if key == "" {
				key = "k"
			}
			_, _, err := f.look(porttest.Root(), app.PreviewInput{Bucket: "b", Key: key, VersionID: tt.versionID})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Preview = %v, want %v", err, tt.want)
			}
			if tt.unread && f.opens.Load() != 0 {
				t.Fatalf("%d shards were opened", f.opens.Load())
			}
			// Nothing was shown, and nothing is counted.
			f.flush(t)
			if access := f.recorded(t, "b", "k"); access.Downloads != 0 {
				t.Fatalf("a refused preview counted: %+v", access)
			}
		})
	}
}

// TestPreviewKinds: what is shown, of how much, as what.
func TestPreviewKinds(t *testing.T) {
	t.Parallel()
	text := bytes.Repeat([]byte("line of text\n"), (app.PreviewTextSize/13)+100)
	tests := []struct {
		name        string
		contentType string
		data        []byte
		kind        preview.Kind
		// shown is how much of data is shown.
		shown     int
		truncated bool
	}{
		{name: "PNG", contentType: "image/png", data: pngOf(640, 480, 9000), kind: preview.KindPNG, shown: 9000},
		{name: "PNG untyped", contentType: "application/octet-stream", data: pngOf(640, 480, 300), kind: preview.KindPNG, shown: 300},
		{name: "a JPEG whose size is past 4 KiB", contentType: "image/jpeg", data: jpegOf(800, 600, 2), kind: preview.KindJPEG, shown: len(jpegOf(800, 600, 2))},
		{name: "a small JPEG", contentType: "image/jpeg", data: jpegOf(8, 6, 0), kind: preview.KindJPEG, shown: len(jpegOf(8, 6, 0))},
		{name: "PDF", contentType: "application/pdf", data: []byte("%PDF-1.7\n%%EOF\n"), kind: preview.KindPDF, shown: 15},
		{name: "DOCX", contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", data: []byte("PK\x03\x04docx"), kind: preview.KindDOCX, shown: 8},
		{name: "text", contentType: "text/plain; charset=utf-8", data: []byte("héllo"), kind: preview.KindText, shown: 6},
		{name: "empty text", contentType: "text/plain", data: nil, kind: preview.KindText, shown: 0},
		{name: "JSON", contentType: "application/json", data: []byte(`{"a":1}`), kind: preview.KindJSON, shown: 7},
		{name: "text over a MiB, past the cap", contentType: "text/csv", data: text, kind: preview.KindCSV, shown: app.PreviewTextSize, truncated: true},
		{name: "text of exactly a MiB", contentType: "text/plain", data: text[:app.PreviewTextSize], kind: preview.KindText, shown: app.PreviewTextSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPreviewFixture(t, app.MinPreviewMaxSize)
			f.store(t, porttest.Root(), "k", tt.contentType, tt.data)
			out, got, err := f.look(porttest.Root(), app.PreviewInput{Bucket: "b", Key: "k"})
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			if out.Kind != tt.kind || out.Truncated != tt.truncated || out.Length != int64(tt.shown) || !bytes.Equal(got, tt.data[:tt.shown]) {
				t.Fatalf("Preview = kind %s, truncated %v, length %d, %d bytes", out.Kind, out.Truncated, out.Length, len(got))
			}
		})
	}
}

// TestPreviewCutsOffAGIFFramePastItsScreen: a frame larger than the screen
// that begins after the bytes looked at first ends the body with an error,
// short of its length - never with a success.
func TestPreviewCutsOffAGIFFramePastItsScreen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "a frame that fits", data: gifOf(preview.BoundsHeadSize+5000, 30, 20)},
		{name: "a frame past the screen, far on", data: gifOf(preview.BoundsHeadSize+5000, 65535, 65535), want: domain.ErrPreviewTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPreviewFixture(t, 0)
			f.store(t, porttest.Root(), "k", "image/gif", tt.data)
			out, got, err := f.look(porttest.Root(), app.PreviewInput{Bucket: "b", Key: "k"})
			if out.Kind != preview.KindGIF || out.Length != int64(len(tt.data)) {
				t.Fatalf("Preview = %+v", out)
			}
			if tt.want == nil {
				if err != nil || !bytes.Equal(got, tt.data) {
					t.Fatalf("read %d of %d bytes: %v", len(got), len(tt.data), err)
				}
				return
			}
			if !errors.Is(err, tt.want) || len(got) >= len(tt.data) {
				t.Fatalf("read %d of %d bytes: %v, want %v", len(got), len(tt.data), err, tt.want)
			}
		})
	}
}

// TestPreviewIsADownload: a preview counts once, when it is closed, for who
// asked.
func TestPreviewIsADownload(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, 0)
	f.store(t, porttest.Root(), "k", "image/png", pngOf(5, 5, 400))
	viewer := policyOf("ann", allow(string(iam.ActionGetObject), string(iam.ActionPreviewObject)))
	out, err := f.objects.Preview(context.Background(), viewer, app.PreviewInput{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	f.flush(t)
	if access := f.recorded(t, "b", "k"); access.Downloads != 0 {
		t.Fatalf("counted before it was closed: %+v", access)
	}
	for range 2 {
		if err := out.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f.flush(t)
	access := f.recorded(t, "b", "k")
	if access.Downloads != 1 || len(access.Readers) != 1 || access.Readers[0].Who != "user/ann" {
		t.Fatalf("recorded %+v", access)
	}
}

// TestPreviewsAtOnce: two at once for a caller, sixteen for the node; a
// preview ends when it is closed, or when it fails before it is handed out.
func TestPreviewsAtOnce(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, 0)
	f.store(t, porttest.Root(), "k", "image/png", pngOf(5, 5, 400))
	f.store(t, porttest.Root(), "html-as-png", "image/png", []byte("<html>"))
	in := app.PreviewInput{Bucket: "b", Key: "k"}
	viewer := func(i int) iam.Principal {
		return policyOf(fmt.Sprintf("u%d", i), allow(string(iam.ActionGetObject), string(iam.ActionPreviewObject)))
	}
	ctx := context.Background()

	// A preview refused after its bytes were looked at takes no place.
	for range app.PreviewsPerPrincipal + 1 {
		if _, err := f.objects.Preview(ctx, viewer(0), app.PreviewInput{Bucket: "b", Key: "html-as-png"}); !errors.Is(err, domain.ErrNotPreviewable) {
			t.Fatalf("html as png: %v", err)
		}
	}
	var open []io.Closer
	for range app.PreviewsPerPrincipal {
		out, err := f.objects.Preview(ctx, viewer(0), in)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, out.Body)
	}
	if _, err := f.objects.Preview(ctx, viewer(0), in); !errors.Is(err, domain.ErrTooManyRequests) {
		t.Fatalf("a third of the same caller: %v", err)
	}
	// Closing one makes room, closing it again no more.
	_ = open[0].Close()
	_ = open[0].Close()
	out, err := f.objects.Preview(ctx, viewer(0), in)
	if err != nil {
		t.Fatalf("after one was closed: %v", err)
	}
	open[0] = out.Body
	if _, err := f.objects.Preview(ctx, viewer(0), in); !errors.Is(err, domain.ErrTooManyRequests) {
		t.Fatalf("a closed preview was let go twice: %v", err)
	}
	// The node: up to sixteen of all callers.
	for i := 1; len(open) < app.PreviewsPerNode; i++ {
		out, err := f.objects.Preview(ctx, viewer(i), in)
		if err != nil {
			t.Fatalf("preview %d: %v", len(open)+1, err)
		}
		open = append(open, out.Body)
	}
	if _, err := f.objects.Preview(ctx, viewer(99), in); !errors.Is(err, domain.ErrTooManyRequests) {
		t.Fatalf("a seventeenth on the node: %v", err)
	}
	for _, body := range open {
		_ = body.Close()
	}
	if _, _, err := f.look(viewer(99), in); err != nil {
		t.Fatalf("once all were closed: %v", err)
	}
}

// TestPreviewMaxSizeIsBounded: the cap of a service is the default, or one
// within its bounds.
func TestPreviewMaxSizeIsBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tests := []struct {
		size int64
		want int64
		ok   bool
	}{
		{size: 0, want: app.DefaultPreviewMaxSize, ok: true},
		{size: app.MinPreviewMaxSize, want: app.MinPreviewMaxSize, ok: true},
		{size: app.MaxPreviewMaxSize, want: app.MaxPreviewMaxSize, ok: true},
		{size: app.MinPreviewMaxSize - 1},
		{size: app.MaxPreviewMaxSize + 1},
		{size: -1},
	}
	for _, tt := range tests {
		cfg := app.ObjectServiceConfig{
			Index: f.meta, Buckets: f.meta, Uploads: f.meta, Drives: fixed(t, f.topology, []ports.ShardStore{f.drives[0], f.drives[1]}),
			Codec: porttest.NewCodec(), KMS: f.kms, Authz: f.authz, Clock: f.clock, Logger: slog.New(slog.DiscardHandler), PreviewMaxSize: tt.size,
		}
		svc, err := app.NewObjectService(cfg)
		if !tt.ok {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("PreviewMaxSize %d: %v", tt.size, err)
			}
			continue
		}
		if err != nil || svc.PreviewMaxSize() != tt.want {
			t.Errorf("PreviewMaxSize %d: %v, %v", tt.size, svc, err)
		}
	}
}

// TestPreviewOfDataThatIsGone: a version whose shards cannot be read is
// looked up again, and then refused - as a download is - and the place it
// took among the previews is given back each time.
func TestPreviewOfDataThatIsGone(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, 0)
	v := f.store(t, porttest.Root(), "k", "image/png", pngOf(5, 5, 400))
	for _, drive := range f.shardsOf(t, v) {
		for _, id := range drive.IDs() {
			if err := drive.Delete(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range app.PreviewsPerPrincipal + 1 {
		if _, _, err := f.look(porttest.Root(), app.PreviewInput{Bucket: "b", Key: "k"}); !errors.Is(err, domain.ErrShardNotFound) {
			t.Fatalf("a preview of data that is gone: %v", err)
		}
	}
	if opens := f.opens.Load(); opens == 0 {
		t.Fatal("no shard was asked for")
	}
}

// panickingKMS is a KMS that panics on unwrapping while on is set.
type panickingKMS struct {
	ports.KMS
	on *atomic.Bool
}

func (k panickingKMS) UnwrapDataKey(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	if k.on.Load() {
		panic("the KMS fell over")
	}
	return k.KMS.UnwrapDataKey(ctx, wrapped, aad)
}

// TestAPreviewThatPanicsGivesItsPlaceBack: whatever ends a preview between
// taking its place and handing out its body - a panic included - gives the
// place back.
func TestAPreviewThatPanicsGivesItsPlaceBack(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, 0)
	on := new(atomic.Bool)
	f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) {
		cfg.KMS = panickingKMS{KMS: f.kms, on: on}
	})
	f.store(t, porttest.Root(), "k", "image/png", pngOf(5, 5, 400))
	in := app.PreviewInput{Bucket: "b", Key: "k"}
	on.Store(true)
	for range app.PreviewsPerPrincipal + 1 {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the preview did not panic")
				}
			}()
			_, _ = f.objects.Preview(context.Background(), porttest.Root(), in)
		}()
	}
	on.Store(false)
	var open []io.Closer
	for range app.PreviewsPerPrincipal {
		out, err := f.objects.Preview(context.Background(), porttest.Root(), in)
		if err != nil {
			t.Fatalf("after the panics: %v", err)
		}
		open = append(open, out.Body)
	}
	for _, body := range open {
		_ = body.Close()
	}
}

// overwritingDrive runs hook once, before the first shard is opened, and
// lets every open wait for it.
type overwritingDrive struct {
	ports.ShardStore
	once *sync.Once
	hook func()
}

func (d overwritingDrive) Open(ctx context.Context, id object.ShardID) (ports.ShardReader, error) {
	d.once.Do(d.hook)
	return d.ShardStore.Open(ctx, id)
}

// TestPreviewJudgesAgainWhatReplacedTheVersion: alice's version is replaced
// by bob's between its judgement and its opening; the preview starts again
// and judges bob's - which alice neither owns nor is assigned, so she is
// refused - and asks with bob as the maker.
func TestPreviewJudgesAgainWhatReplacedTheVersion(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, 0)
	once := new(sync.Once)
	var hookErr error
	f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) {
		drives := make([]ports.ShardStore, len(f.drives))
		for i, d := range f.drives {
			drives[i] = overwritingDrive{ShardStore: d, once: once, hook: func() {
				data := []byte("bob's now")
				_, hookErr = f.objects.Put(context.Background(), userNamed("bob", "s3:PutObject"), app.PutInput{
					Bucket: "b", Key: "k", Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: "text/plain",
				})
			}}
		}
		cfg.Drives = fixed(t, f.topology, drives)
	})
	f.store(t, userNamed("alice", "s3:PutObject"), "k", "image/png", pngOf(5, 5, 400))
	alice := policyOf("alice", allow(string(iam.ActionGetObject)))
	before := len(f.authz.Requests())

	_, _, err := f.look(alice, app.PreviewInput{Bucket: "b", Key: "k"})
	if hookErr != nil {
		t.Fatalf("bob's overwrite: %v", hookErr)
	}
	if !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("Preview of what bob put in the meantime = %v, want AccessDenied", err)
	}
	var creators []string
	for _, req := range f.authz.Requests()[before:] {
		if req.Action == iam.ActionPreviewObject {
			creators = append(creators, req.ObjectCreator)
		}
	}
	if len(creators) != 2 || creators[0] != "user/alice" || creators[1] != "user/bob" {
		t.Fatalf("the previews were asked with the makers %q, want alice's and then bob's", creators)
	}
}
