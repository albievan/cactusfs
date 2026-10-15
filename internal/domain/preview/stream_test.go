package preview_test

import (
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/preview"
)

func TestExpected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		storedType string
		want       preview.Kind
		refused    bool
	}{
		{storedType: "", want: ""},
		{storedType: "application/octet-stream", want: ""},
		{storedType: " Application/Octet-Stream; x=1", want: ""},
		{storedType: "image/png", want: preview.KindPNG},
		{storedType: "IMAGE/JPG", want: preview.KindJPEG},
		{storedType: "text/plain; charset=utf-8", want: preview.KindText},
		{storedType: "application/json", want: preview.KindJSON},
		{storedType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", want: preview.KindXLSX},
		{storedType: "text/html", refused: true},
		{storedType: "image/svg+xml", refused: true},
		{storedType: "application/xml", refused: true},
		{storedType: "text/javascript", refused: true},
		{storedType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", refused: true},
		{storedType: "video/mp4", refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.storedType, func(t *testing.T) {
			t.Parallel()
			got, err := preview.Expected(tt.storedType)
			if tt.refused {
				if !errors.Is(err, domain.ErrNotPreviewable) || got != "" {
					t.Fatalf("Expected(%q) = %q, %v; want ErrNotPreviewable", tt.storedType, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Expected(%q) = %q, %v; want %q", tt.storedType, got, err, tt.want)
			}
		})
	}
}

// TestExpectedAgreesWithClassify: a type Expected refuses is refused by
// Classify whatever the bytes, and a kind it names is the only kind
// Classify can find.
func TestExpectedAgreesWithClassify(t *testing.T) {
	t.Parallel()
	for _, storedType := range storedTypes() {
		expected, expectErr := preview.Expected(storedType)
		for _, head := range heads() {
			kind, err := preview.Classify(storedType, head)
			switch {
			case expectErr != nil && err == nil:
				t.Errorf("Expected(%q) refuses and Classify finds %q", storedType, kind)
			case err == nil && expected != "" && kind != expected:
				t.Errorf("Expected(%q) = %q and Classify finds %q", storedType, expected, kind)
			}
		}
	}
}

// writeGIF writes b to a new check in pieces of size n, and returns the
// first error and how many bytes the writes took in before it.
func writeGIF(b []byte, n int) (int, error) {
	frames := preview.NewGIFFrames()
	taken := 0
	for len(b) > 0 {
		piece := b[:min(n, len(b))]
		got, err := frames.Write(piece)
		taken += got
		if err != nil {
			return taken, err
		}
		if got != len(piece) {
			return taken, errors.New("a short write without an error")
		}
		b = b[len(piece):]
	}
	return taken, nil
}

func TestGIFFrames(t *testing.T) {
	t.Parallel()
	// Past the bytes ImageBounds looks at.
	far := gifComment(preview.BoundsHeadSize + 1000)
	tests := []struct {
		name string
		gif  []byte
		want error
	}{
		{name: "one frame", gif: gifHead("GIF89a", 30, 20)},
		{name: "GIF87a", gif: gifHead("GIF87a", 30, 20)},
		{name: "frames inside, local colour tables", gif: gif("GIF89a", 30, 20, gifControl, gifFrame(10, 5, 20, 15, true), gifFrame(0, 0, 30, 20, false))},
		{name: "a frame far on, inside", gif: gif("GIF89a", 30, 20, gifFrame(0, 0, 30, 20, false), far, gifFrame(0, 0, 30, 20, true))},
		{
			name: "a frame far on, wider than the screen",
			gif:  gif("GIF89a", 30, 20, gifFrame(0, 0, 30, 20, false), far, gifControl, gifFrame(0, 0, 65535, 65535, false)),
			want: domain.ErrPreviewTooLarge,
		},
		{
			name: "a frame far on, off the screen by its place",
			gif:  gif("GIF89a", 30, 20, gifFrame(0, 0, 30, 20, false), far, gifFrame(25, 0, 6, 20, false)),
			want: domain.ErrPreviewTooLarge,
		},
		{name: "the first frame wider than the screen", gif: gif("GIF89a", 1, 1, gifFrame(0, 0, 2, 1, false)), want: domain.ErrPreviewTooLarge},
		{name: "a malformed block", gif: append(gifHead("GIF89a", 3, 3)[:len(gifHead("GIF89a", 3, 3))-1], 0x99), want: domain.ErrNotPreviewable},
		{name: "no GIF", gif: pngHead(10, 10), want: domain.ErrNotPreviewable},
		{name: "what follows the trailer is not looked at", gif: append(gifHead("GIF89a", 3, 3), 0x2C, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0)},
		{name: "cut off in a frame's data", gif: gifHead("GIF89a", 3, 3)[:40]},
		{name: "cut off in the screen", gif: gifScreen("GIF89a", 3, 3)[:7]},
		{name: "empty", gif: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, n := range []int{1, 2, 7, 13, 4096, len(tt.gif) + 1} {
				_, err := writeGIF(tt.gif, n)
				if (tt.want == nil) != (err == nil) || (tt.want != nil && !errors.Is(err, tt.want)) {
					t.Fatalf("in pieces of %d: %v, want %v", n, err, tt.want)
				}
			}
		})
	}
}

// TestGIFFramesFailsForGood: once a write failed, every write fails, and
// nothing of the failing piece counts as taken.
func TestGIFFramesFailsForGood(t *testing.T) {
	t.Parallel()
	frames := preview.NewGIFFrames()
	bad := gif("GIF89a", 1, 1, gifFrame(0, 0, 2, 2, false))
	if n, err := frames.Write(bad); n != 0 || !errors.Is(err, domain.ErrPreviewTooLarge) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, err := frames.Write([]byte{0x3B}); n != 0 || !errors.Is(err, domain.ErrPreviewTooLarge) {
		t.Fatalf("Write after a failure = %d, %v", n, err)
	}
}

// gifsOf are the GIFs and pieces of GIFs the properties are checked on.
func gifsOf() [][]byte {
	out := [][]byte{
		gifHead("GIF89a", 30, 20),
		gif("GIF89a", 30, 20, gifComment(300), gifControl, gifFrame(10, 5, 20, 15, true), gifFrame(0, 0, 30, 20, false)),
		gif("GIF89a", 1, 1, gifFrame(0, 0, 65535, 65535, false)),
		gif("GIF89a", 30, 20, gifFrame(0, 0, 30, 20, false), gifFrame(29, 19, 2, 2, true)),
		append(gifScreen("GIF87a", 10, 10), 0x21, 0xFE, 0x10, 'a'),
	}
	return append(out, heads()...)
}

// checkGIFFrames holds the check to what does not depend on how the bytes
// are cut, and to ImageBounds: what it refuses for a frame past the screen
// in the bytes it looks at, so does the check.
func checkGIFFrames(t *testing.T, b []byte, n int) {
	t.Helper()
	whole, wholeErr := writeGIF(b, len(b)+1)
	if n < 1 {
		n = 1
	}
	pieces, piecesErr := writeGIF(b, n)
	if (wholeErr == nil) != (piecesErr == nil) || (wholeErr != nil && wholeErr.Error() != piecesErr.Error()) {
		t.Fatalf("whole: %v; in pieces of %d: %v", wholeErr, n, piecesErr)
	}
	if wholeErr == nil && (whole != len(b) || pieces != len(b)) {
		t.Fatalf("took %d and %d of %d bytes without an error", whole, pieces, len(b))
	}
	if wholeErr != nil && !errors.Is(wholeErr, domain.ErrPreviewTooLarge) && !errors.Is(wholeErr, domain.ErrNotPreviewable) {
		t.Fatalf("an error of no kind: %v", wholeErr)
	}
	if _, err := preview.ImageBounds(preview.KindGIF, b); err != nil && errors.Is(err, domain.ErrPreviewTooLarge) {
		// Too large by its screen, which the check does not judge, or by
		// a frame, which it must find too.
		if bounds, _ := preview.ImageBounds(preview.KindGIF, b); bounds.Width <= preview.MaxImageSide && bounds.Height <= preview.MaxImageSide &&
			uint64(bounds.Width)*uint64(bounds.Height) <= preview.MaxImagePixels && !errors.Is(wholeErr, domain.ErrPreviewTooLarge) {
			t.Fatalf("ImageBounds refuses a frame (%v) and the check says %v", err, wholeErr)
		}
	}
}

func TestGIFFramesProperties(t *testing.T) {
	t.Parallel()
	for _, b := range gifsOf() {
		for _, n := range []int{1, 3, 10, 64} {
			checkGIFFrames(t, b, n)
		}
	}
}

func FuzzGIFFrames(f *testing.F) {
	for _, b := range gifsOf() {
		f.Add(b, 5)
	}
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		checkGIFFrames(t, b, n%257)
	})
}
