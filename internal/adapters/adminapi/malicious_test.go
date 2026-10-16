package adminapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/preview/previewtest"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// storeExactly puts an object as root with exactly the stored type given -
// none included, which an upload through the API would make
// application/octet-stream.
func (f *previewFixture) storeExactly(key, storedType string, data []byte) {
	f.t.Helper()
	if _, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
		Bucket: "photos", Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: storedType,
	}); err != nil {
		f.t.Fatalf("Put(%s): %v", key, err)
	}
}

// TestPreviewOfMaliciousFiles: every file of the malicious-file suite (PLAN
// O.11) through the API. A refusal is problem+json - 415 NotPreviewable or
// 413 PreviewTooLarge - with nothing of the file in it; what is shown is
// only ever every byte of it as an attachment of
// application/octet-stream, with nosniff and a sandbox, of the kind its
// bytes prove.
func TestPreviewOfMaliciousFiles(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	cases := previewtest.Cases()
	for i, c := range cases {
		f.storeExactly("m"+strconv.Itoa(i), c.StoredType, c.Data)
	}
	for i, c := range cases {
		r := f.do(call{method: "GET", path: "/buckets/photos/object/preview?key=m" + strconv.Itoa(i), bearer: f.root})
		if c.Refusal != nil {
			status, code := http.StatusUnsupportedMediaType, "NotPreviewable"
			if errors.Is(c.Refusal, domain.ErrPreviewTooLarge) {
				status, code = http.StatusRequestEntityTooLarge, "PreviewTooLarge"
			}
			if r.status != status || r.header.Get("Content-Type") != "application/problem+json" || r.problem(t)["code"] != code {
				t.Errorf("%s: %d %s %s, want %d %s", c.Name, r.status, r.header.Get("Content-Type"), r.body, status, code)
			}
			if bytes.Contains(r.body, []byte(previewtest.Marker)) || bytes.Contains(r.body, []byte("<script")) ||
				r.header.Get("X-CactusFS-Preview-Kind") != "" || r.header.Get("Content-Disposition") != "" {
				t.Errorf("%s: something of the file was sent: %v %s", c.Name, r.header, r.body)
			}
			continue
		}
		if r.status != http.StatusOK {
			t.Errorf("%s: %d %s, want 200", c.Name, r.status, r.body)
			continue
		}
		for name, want := range previewHeaders {
			if got := r.header.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", c.Name, name, got, want)
			}
		}
		if got := r.header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
			t.Errorf("%s: Content-Disposition = %q", c.Name, got)
		}
		if r.header.Get("X-CactusFS-Preview-Kind") != string(c.Kind) || !bytes.Equal(r.body, c.Data) {
			t.Errorf("%s: kind %q and %d bytes, want %q and %d", c.Name, r.header.Get("X-CactusFS-Preview-Kind"), len(r.body), c.Kind, len(c.Data))
		}
	}
}

// TestAPreviewOverTheCapReadsNothing: an object one byte larger than the
// cap is 413 by its size alone. Its shards fail every read, so any read of
// them would have answered otherwise.
func TestAPreviewOverTheCapReadsNothing(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t)
	f.storeExactly("over.png", "image/png", previewtest.OneByteOver(app.DefaultPreviewMaxSize))
	f.storeExactly("at.png", "image/png", previewtest.PNGOf(10, 10, 1<<20))
	if len(f.drive.IDs()) == 0 {
		t.Fatal("no shard was stored")
	}
	for _, id := range f.drive.IDs() {
		f.drive.Damage(id)
	}
	for _, tt := range []struct {
		key    string
		status int
		code   string
	}{
		{key: "over.png", status: http.StatusRequestEntityTooLarge, code: "PreviewTooLarge"},
		// The same damage, on an object within the cap, fails the read.
		{key: "at.png", status: http.StatusInternalServerError},
	} {
		r := f.do(call{method: "GET", path: "/buckets/photos/object/preview?key=" + tt.key, bearer: f.root})
		if r.status != tt.status || (tt.code != "" && r.problem(t)["code"] != tt.code) {
			t.Errorf("%s: %d %s, want %d %s", tt.key, r.status, r.body, tt.status, tt.code)
		}
	}
}
