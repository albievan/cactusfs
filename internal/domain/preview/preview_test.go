package preview_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/preview"
)

// The first bytes of files of each kind, made here so that a test says what
// it is made of.

func pngHead(width, height uint32) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = binary.BigEndian.AppendUint32(b, width)
	b = binary.BigEndian.AppendUint32(b, height)
	return append(b, 8, 6, 0, 0, 0, 0xDE, 0xAD, 0xBE, 0xEF)
}

// gifScreen is the header of a GIF and its logical screen descriptor, with
// a global colour table of two colours.
func gifScreen(version string, width, height uint16) []byte {
	b := []byte(version)
	b = binary.LittleEndian.AppendUint16(b, width)
	b = binary.LittleEndian.AppendUint16(b, height)
	return append(b, 0x80, 0, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF)
}

// gif is a whole GIF: its screen, the blocks given, and the trailer.
func gif(version string, width, height uint16, blocks ...[]byte) []byte {
	b := gifScreen(version, width, height)
	for _, block := range blocks {
		b = append(b, block...)
	}
	return append(b, 0x3B)
}

// gifFrame is an image descriptor at left, top of the size given, with its
// data: a minimum code size and one sub-block. With local it has a colour
// table of its own, of four colours.
func gifFrame(left, top, width, height uint16, local bool) []byte {
	b := []byte{0x2C}
	for _, v := range []uint16{left, top, width, height} {
		b = binary.LittleEndian.AppendUint16(b, v)
	}
	if local {
		b = append(b, 0x81)
		b = append(b, make([]byte, 12)...)
	} else {
		b = append(b, 0x00)
	}
	return append(b, 0x02, 0x02, 0x4C, 0x01, 0x00)
}

// gifControl is a graphic control extension.
var gifControl = []byte{0x21, 0xF9, 0x04, 0x00, 0x0A, 0x00, 0x00, 0x00}

// gifComment is a comment extension of n bytes, in sub-blocks of 255.
func gifComment(n int) []byte {
	b := []byte{0x21, 0xFE}
	for n > 0 {
		size := min(n, 255)
		b = append(b, byte(size))
		b = append(b, bytes.Repeat([]byte{'c'}, size)...)
		n -= size
	}
	return append(b, 0x00)
}

// gifHead is a GIF of one frame as large as its screen.
func gifHead(version string, width, height uint16) []byte {
	return gif(version, width, height, gifControl, gifFrame(0, 0, width, height, false))
}

// segment is a JPEG marker segment of the length given - its two bytes of
// length included - filled with fill.
func segment(marker byte, length uint16, fill byte) []byte {
	b := binary.BigEndian.AppendUint16([]byte{0xFF, marker}, length)
	return append(b, bytes.Repeat([]byte{fill}, int(length)-2)...)
}

// frame is a frame header (SOFn) of three components.
func frame(marker byte, width, height uint16) []byte {
	b := []byte{0xFF, marker, 0x00, 0x11, 8}
	b = binary.BigEndian.AppendUint16(b, height)
	b = binary.BigEndian.AppendUint16(b, width)
	return append(b, 3, 1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1)
}

func jpegHead(parts ...[]byte) []byte {
	b := []byte{0xFF, 0xD8}
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func app(length uint16) []byte { return segment(0xE1, length, 'x') }

// riff is a WebP file of one chunk. The sizes of the container and of the
// chunk are not read, and are 0.
func riff(chunk string, data []byte) []byte {
	return append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), data...)
}

func webpLossy(width, height uint16) []byte {
	data := []byte{0x30, 0x01, 0x00, 0x9D, 0x01, 0x2A}
	data = binary.LittleEndian.AppendUint16(data, width)
	data = binary.LittleEndian.AppendUint16(data, height)
	return riff("VP8 ", append(data, 0, 0, 0, 0))
}

func webpLossless(width, height uint32) []byte {
	bits := (width - 1) | (height-1)<<14
	data := binary.LittleEndian.AppendUint32([]byte{0x2F}, bits)
	return riff("VP8L", append(data, 0, 0, 0, 0, 0, 0))
}

func webpExtended(width, height uint32) []byte {
	data := []byte{0x10, 0, 0, 0}
	data = append(data, binary.LittleEndian.AppendUint32(nil, width-1)[:3]...)
	data = append(data, binary.LittleEndian.AppendUint32(nil, height-1)[:3]...)
	return riff("VP8X", append(data, 0, 0))
}

// withByte returns b with the byte at i set to c.
func withByte(b []byte, i int, c byte) []byte {
	b = slices.Clone(b)
	b[i] = c
	return b
}

const (
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

var zipHead = []byte("PK\x03\x04\x14\x00\x06\x00\x08\x00\x00\x00!\x00[Content_Types].xml")

// polyglot is a GIF that is also a script: its header opens a comment.
var polyglot = []byte("GIF89a/*\x01\x00\x00\x00\x00*/=1;alert(document.cookie);//")

func TestClassify(t *testing.T) {
	t.Parallel()
	png := pngHead(800, 600)
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	long := append(bytes.Repeat([]byte("a"), preview.HeadSize), 0)
	tests := []struct {
		name       string
		storedType string
		head       []byte
		want       preview.Kind // "" for not previewable
	}{
		{"png", "image/png", png, preview.KindPNG},
		{"png, type in capitals with parameters", " Image/PNG ; name=x.png", png, preview.KindPNG},
		{"jpeg", "image/jpeg", jpegHead(frame(0xC0, 10, 10)), preview.KindJPEG},
		{"jpeg as image/jpg", "image/jpg", jpegHead(frame(0xC0, 10, 10)), preview.KindJPEG},
		{"gif87a", "image/gif", gifHead("GIF87a", 1, 1), preview.KindGIF},
		{"gif89a", "image/gif", gifHead("GIF89a", 1, 1), preview.KindGIF},
		{"webp", "image/webp", webpLossy(10, 10), preview.KindWebP},
		{"pdf", "application/pdf", []byte("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n"), preview.KindPDF},
		{"docx", docxType, zipHead, preview.KindDOCX},
		{"xlsx", xlsxType + "; charset=binary", zipHead, preview.KindXLSX},
		{"text", "text/plain; charset=utf-8", []byte("hello, world\n"), preview.KindText},
		{"empty text", "text/plain", nil, preview.KindText},
		{"csv", "text/csv", []byte("a,b\n1,2\n"), preview.KindCSV},
		{"json", "application/json", []byte(`{"a":[1,2,"é"]}`), preview.KindJSON},
		{"text with a byte-order mark", "text/plain", []byte("\xEF\xBB\xBFhi"), preview.KindText},
		{"text cut in a character", "text/plain", []byte("caf\xC3"), preview.KindText},
		{"text cut in a four-byte character", "text/plain", []byte("x\xF0\x9F\x98"), preview.KindText},
		{"text longer than the head, NUL after it", "text/plain", long, preview.KindText},
		{"markup declared as text is text", "text/plain", html, preview.KindText},
		{"untyped png", "", png, preview.KindPNG},
		{"png as octet-stream", "application/octet-stream", png, preview.KindPNG},
		{"pdf as octet-stream", "Application/Octet-Stream; x=y", []byte("%PDF-1.4"), preview.KindPDF},

		{"html bytes declared image/png", "image/png", html, ""},
		{"png bytes declared text/html", "text/html", png, ""},
		{"png bytes declared text/plain", "text/plain", png, ""},
		{"jpeg bytes declared image/png", "image/png", jpegHead(frame(0xC0, 1, 1)), ""},
		{"png bytes declared application/pdf", "application/pdf", png, ""},
		{"pdf not at the start", "application/pdf", []byte("\n%PDF-1.7"), ""},
		{"html", "text/html", html, ""},
		{"html with a charset", "text/html; charset=utf-8", []byte("hi"), ""},
		{"xhtml", "application/xhtml+xml", html, ""},
		{"svg", "image/svg+xml", svg, ""},
		{"svg declared image/png", "image/png", svg, ""},
		{"svg untyped", "", svg, ""},
		{"xml", "application/xml", []byte("<a/>"), ""},
		{"text xml", "text/xml", []byte("<a/>"), ""},
		{"script", "application/javascript", []byte("alert(1)"), ""},
		{"text script", "text/javascript", []byte("alert(1)"), ""},
		{"polyglot as a gif", "image/gif", polyglot, preview.KindGIF},
		{"polyglot untyped", "", polyglot, preview.KindGIF},
		{"polyglot as script", "application/javascript", polyglot, ""},
		{"polyglot as text script", "text/javascript", polyglot, ""},
		{"polyglot as text", "text/plain", polyglot, ""},
		{"polyglot as html", "text/html", polyglot, ""},
		{"docx without a zip", docxType, []byte("hello"), ""},
		{"docx untyped", "", zipHead, ""},
		{"docx as octet-stream", "application/octet-stream", zipHead, ""},
		{"zip", "application/zip", zipHead, ""},
		{"zip declared text", "text/plain", zipHead, ""},
		{"empty png", "image/png", nil, ""},
		{"empty untyped", "", nil, ""},
		{"text with a NUL", "text/plain", []byte("a\x00b"), ""},
		{"utf-16 text", "text/plain", []byte("\xFF\xFEh\x00i\x00"), ""},
		{"text not utf-8", "text/csv", []byte("caf\xE9,1\n"), ""},
		{"text with a bad byte at the end", "application/json", []byte("{}\xFF"), ""},
		{"text with a stray continuation at the end", "text/plain", []byte("ab\x80"), ""},
		{"a type that names nothing", "image/tiff", []byte("II*\x00"), ""},
		{"a list of types", "image/png, text/html", png, ""},
		{"a type without a subtype", "image", png, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind, err := preview.Classify(tt.storedType, tt.head)
			if tt.want == "" {
				if !errors.Is(err, domain.ErrNotPreviewable) || kind != "" {
					t.Fatalf("Classify = %q, %v; want %v", kind, err, domain.ErrNotPreviewable)
				}
				return
			}
			if err != nil || kind != tt.want {
				t.Fatalf("Classify = %q, %v; want %q", kind, err, tt.want)
			}
		})
	}
}

func TestImageBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		kind preview.Kind
		head []byte
		want preview.Bounds
		err  error
	}{
		{"png", preview.KindPNG, pngHead(800, 600), preview.Bounds{Width: 800, Height: 600}, nil},
		{"png of the most pixels", preview.KindPNG, pngHead(16384, 2441), preview.Bounds{Width: 16384, Height: 2441}, nil},
		{"png of one row more", preview.KindPNG, pngHead(16384, 2442), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"png 65535 × 65535", preview.KindPNG, pngHead(65535, 65535), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"png of the largest size there is", preview.KindPNG, pngHead(0xFFFFFFFF, 0xFFFFFFFF), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"png one pixel too wide", preview.KindPNG, pngHead(16385, 1), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"png one pixel too high", preview.KindPNG, pngHead(1, 16385), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"png of no pixels", preview.KindPNG, pngHead(0, 10), preview.Bounds{}, domain.ErrNotPreviewable},
		{"png cut short", preview.KindPNG, pngHead(10, 10)[:20], preview.Bounds{}, domain.ErrNotPreviewable},
		{"png without IHDR first", preview.KindPNG, bytes.Replace(pngHead(10, 10), []byte("IHDR"), []byte("tEXt"), 1), preview.Bounds{}, domain.ErrNotPreviewable},
		{"png that is a jpeg", preview.KindPNG, jpegHead(frame(0xC0, 10, 10)), preview.Bounds{}, domain.ErrNotPreviewable},

		{"jpeg", preview.KindJPEG, jpegHead(segment(0xE0, 16, 0), frame(0xC0, 640, 480)), preview.Bounds{Width: 640, Height: 480}, nil},
		{"progressive jpeg", preview.KindJPEG, jpegHead(frame(0xC2, 64, 48)), preview.Bounds{Width: 64, Height: 48}, nil},
		{"jpeg with fill bytes", preview.KindJPEG, jpegHead([]byte{0xFF, 0xFF}, frame(0xC1, 3, 2)), preview.Bounds{Width: 3, Height: 2}, nil},
		{"jpeg with tables first", preview.KindJPEG, jpegHead(segment(0xC4, 32, 0), segment(0xDB, 67, 0), frame(0xC0, 5, 6)), preview.Bounds{Width: 5, Height: 6}, nil},
		{"jpeg with large metadata", preview.KindJPEG, jpegHead(app(60000), app(60000), app(60000), frame(0xC0, 4000, 3000)), preview.Bounds{Width: 4000, Height: 3000}, nil},
		{"jpeg with its frame past the bytes looked at", preview.KindJPEG, jpegHead(app(60000), app(60000), app(60000), app(60000), app(60000), frame(0xC0, 4, 3)), preview.Bounds{}, domain.ErrNotPreviewable},
		{"jpeg too large", preview.KindJPEG, jpegHead(frame(0xC0, 65535, 65535)), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"jpeg whose height comes later", preview.KindJPEG, jpegHead(frame(0xC0, 100, 0)), preview.Bounds{}, domain.ErrNotPreviewable},
		{"jpeg with data before a frame", preview.KindJPEG, jpegHead(segment(0xDA, 12, 0), frame(0xC0, 1, 1)), preview.Bounds{}, domain.ErrNotPreviewable},
		{"jpeg that ends before a frame", preview.KindJPEG, jpegHead([]byte{0xFF, 0xD9}), preview.Bounds{}, domain.ErrNotPreviewable},
		{"jpeg with a short segment", preview.KindJPEG, jpegHead([]byte{0xFF, 0xE0, 0x00, 0x01}), preview.Bounds{}, domain.ErrNotPreviewable},
		{"jpeg with a frame cut short", preview.KindJPEG, jpegHead(frame(0xC0, 1, 1)[:7]), preview.Bounds{}, domain.ErrNotPreviewable},
		{"jpeg with garbage between segments", preview.KindJPEG, jpegHead([]byte{0x00}, frame(0xC0, 1, 1)), preview.Bounds{}, domain.ErrNotPreviewable},

		{"gif", preview.KindGIF, gifHead("GIF89a", 320, 200), preview.Bounds{Width: 320, Height: 200}, nil},
		{"the polyglot has no frame", preview.KindGIF, polyglot, preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif with comments and a frame of its own colours", preview.KindGIF, gif("GIF89a", 30, 20, gifComment(600), gifControl, gifFrame(10, 5, 20, 15, true)), preview.Bounds{Width: 30, Height: 20}, nil},
		{"gif of two frames", preview.KindGIF, gif("GIF89a", 30, 20, gifFrame(0, 0, 30, 20, false), gifControl, gifFrame(1, 1, 2, 2, false)), preview.Bounds{Width: 30, Height: 20}, nil},
		{"gif cut in its frame's data", preview.KindGIF, gifHead("GIF89a", 30, 20)[:len(gifHead("GIF89a", 30, 20))-3], preview.Bounds{Width: 30, Height: 20}, nil},
		{"gif whose frame is larger than its screen", preview.KindGIF, gif("GIF89a", 1, 1, gifFrame(0, 0, 65535, 65535, false)), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"gif whose frame reaches past its screen", preview.KindGIF, gif("GIF89a", 10, 10, gifFrame(5, 5, 10, 10, false)), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"gif whose second frame is larger", preview.KindGIF, gif("GIF89a", 10, 10, gifFrame(0, 0, 10, 10, false), gifFrame(0, 0, 11, 10, false)), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"gif whose frame begins past the bytes looked at", preview.KindGIF, gif("GIF89a", 10, 10, gifComment(preview.BoundsHeadSize), gifFrame(0, 0, 10, 10, false)), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif with an extension cut short", preview.KindGIF, append(gifScreen("GIF89a", 10, 10), 0x21, 0xFE, 0x10, 'a', 'b', 'c'), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif with an extension and no frame", preview.KindGIF, gif("GIF89a", 10, 10, gifControl), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif with a descriptor cut short", preview.KindGIF, append(gifScreen("GIF89a", 10, 10), 0x2C, 0, 0, 0), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif with an unknown block", preview.KindGIF, append(gifScreen("GIF89a", 10, 10), 0x99), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif with an unknown block after a frame", preview.KindGIF, append(gifScreen("GIF89a", 10, 10), append(gifFrame(0, 0, 1, 1, false), 0x99)...), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif without its colour table", preview.KindGIF, gifScreen("GIF89a", 10, 10)[:15], preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif too large", preview.KindGIF, gifHead("GIF87a", 65535, 65535), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"gif of no pixels", preview.KindGIF, gifHead("GIF89a", 0, 0), preview.Bounds{}, domain.ErrNotPreviewable},
		{"gif cut short", preview.KindGIF, []byte("GIF89a\x01"), preview.Bounds{}, domain.ErrNotPreviewable},

		{"lossy webp", preview.KindWebP, webpLossy(1024, 768), preview.Bounds{Width: 1024, Height: 768}, nil},
		{"lossy webp with scaling bits", preview.KindWebP, webpLossy(0xC000|300, 0x4000|200), preview.Bounds{Width: 300, Height: 200}, nil},
		{"lossy webp without its start code", preview.KindWebP, bytes.Replace(webpLossy(10, 10), []byte{0x9D, 0x01, 0x2A}, []byte{0, 0, 0}, 1), preview.Bounds{}, domain.ErrNotPreviewable},
		{"lossless webp", preview.KindWebP, webpLossless(16384, 16), preview.Bounds{Width: 16384, Height: 16}, nil},
		{"lossless webp too large", preview.KindWebP, webpLossless(16384, 16384), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"lossless webp without its signature", preview.KindWebP, withByte(webpLossless(10, 10), 20, 0), preview.Bounds{}, domain.ErrNotPreviewable},
		{"extended webp", preview.KindWebP, webpExtended(5000, 4000), preview.Bounds{Width: 5000, Height: 4000}, nil},
		{"extended webp too large", preview.KindWebP, webpExtended(1<<24, 1<<24), preview.Bounds{}, domain.ErrPreviewTooLarge},
		{"webp of an unknown chunk", preview.KindWebP, riff("ALPH", make([]byte, 20)), preview.Bounds{}, domain.ErrNotPreviewable},
		{"webp cut short", preview.KindWebP, webpLossy(10, 10)[:25], preview.Bounds{}, domain.ErrNotPreviewable},

		{"a pdf is no image", preview.KindPDF, []byte("%PDF-1.7"), preview.Bounds{}, domain.ErrNotPreviewable},
		{"text is no image", preview.KindText, pngHead(1, 1), preview.Bounds{}, domain.ErrNotPreviewable},
		{"no kind", "", pngHead(1, 1), preview.Bounds{}, domain.ErrNotPreviewable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := preview.ImageBounds(tt.kind, tt.head)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("ImageBounds = %v, %v; want %v", got, err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ImageBounds = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestKinds(t *testing.T) {
	t.Parallel()
	kinds := preview.Kinds()
	want := []preview.Kind{"png", "jpeg", "gif", "webp", "pdf", "docx", "xlsx", "text", "csv", "json"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("Kinds = %v; want %v (the values are part of the contract)", kinds, want)
	}
	var images, texts []preview.Kind
	for _, kind := range kinds {
		if kind.Image() && kind.Textual() {
			t.Errorf("%s is an image and text", kind)
		}
		if kind.Image() {
			images = append(images, kind)
		}
		if kind.Textual() {
			texts = append(texts, kind)
		}
	}
	if !slices.Equal(images, kinds[:4]) || !slices.Equal(texts, kinds[7:]) {
		t.Errorf("images %v, text %v", images, texts)
	}
	if preview.Kind("svg").Image() || preview.Kind("html").Textual() {
		t.Error("a kind that is none is an image or text")
	}
}

// heads are the first bytes of one file of each kind, and some of none.
func heads() [][]byte {
	return [][]byte{
		pngHead(10, 10), jpegHead(frame(0xC0, 10, 10)), gifHead("GIF89a", 10, 10), webpLossy(10, 10), []byte("%PDF-1.7"),
		zipHead, []byte("plain text"), []byte("a,b\n"), []byte(`{"a":1}`), []byte("<svg/>"), []byte("<html>"), polyglot, nil,
		[]byte("a\x00b"), []byte("\xFF\xFE"),
	}
}

// storedTypes are types that name each kind, and some that name none.
func storedTypes() []string {
	return []string{
		"image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp", "application/pdf", docxType, xlsxType,
		"text/plain", "text/csv", "application/json", "application/octet-stream", "", "text/html", "image/svg+xml",
		"application/xml", "application/javascript", "application/zip",
	}
}

// TestTheCaseAndParametersOfATypeChangeNothing: a stored type in any case,
// with any parameters and space around it, is classified as it is plain.
func TestTheCaseAndParametersOfATypeChangeNothing(t *testing.T) {
	t.Parallel()
	all, types := heads(), storedTypes()
	property := func(which, head uint8, upper uint32, params string, spaces uint8) bool {
		plain := types[int(which)%len(types)]
		b := []byte(plain)
		for i := range b {
			if upper&(1<<(i%32)) != 0 && b[i] >= 'a' && b[i] <= 'z' {
				b[i] -= 'a' - 'A'
			}
		}
		pad := strings.Repeat(" ", int(spaces%3))
		dressed := pad + string(b) + pad
		if params != "" || spaces%2 == 1 {
			dressed += ";" + params
		}
		h := all[int(head)%len(all)]
		wantKind, wantErr := preview.Classify(plain, h)
		kind, err := preview.Classify(dressed, h)
		return kind == wantKind && errors.Is(err, domain.ErrNotPreviewable) == errors.Is(wantErr, domain.ErrNotPreviewable) &&
			preview.MediaType(dressed) == plain
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}

// checkClassify holds what Classify answers to what must be true of it,
// whatever it is given.
func checkClassify(t *testing.T, storedType string, head []byte) {
	t.Helper()
	kind, err := preview.Classify(storedType, head)
	if err != nil {
		if kind != "" || !errors.Is(err, domain.ErrNotPreviewable) || errors.Is(err, domain.ErrPreviewTooLarge) {
			t.Fatalf("Classify(%q) = %q, %v", storedType, kind, err)
		}
		return
	}
	if !slices.Contains(preview.Kinds(), kind) {
		t.Fatalf("Classify(%q) = %q, a kind that is none", storedType, kind)
	}
	mediaType := preview.MediaType(storedType)
	// Markup and script are never previewable, whatever their bytes.
	for _, never := range []string{"html", "svg", "javascript", "ecmascript"} {
		if strings.Contains(mediaType, never) || strings.HasSuffix(mediaType, "xml") {
			t.Fatalf("Classify(%q) = %q", storedType, kind)
		}
	}
	// The bytes carry the kind's signature, and the type names the kind or
	// leaves it to the bytes.
	if len(head) > preview.HeadSize {
		head = head[:preview.HeadSize]
	}
	switch kind {
	case preview.KindPNG:
		ok := bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n"))
		if !ok {
			t.Fatalf("Classify(%q) = png of %x", storedType, head)
		}
	case preview.KindPDF:
		if !bytes.HasPrefix(head, []byte("%PDF-")) {
			t.Fatalf("Classify(%q) = pdf of %x", storedType, head)
		}
	case preview.KindDOCX, preview.KindXLSX:
		if !bytes.HasPrefix(head, []byte("PK\x03\x04")) || (mediaType != docxType && mediaType != xlsxType) {
			t.Fatalf("Classify(%q) = %s of %x", storedType, kind, head)
		}
	case preview.KindText, preview.KindCSV, preview.KindJSON:
		if bytes.IndexByte(head, 0) >= 0 || mediaType == "" || mediaType == "application/octet-stream" {
			t.Fatalf("Classify(%q) = %s of %x", storedType, kind, head)
		}
	}
	// Case and parameters change nothing.
	again, err := preview.Classify(strings.ToUpper(storedType)+"; q=1", head)
	if err != nil || again != kind {
		t.Fatalf("Classify(%q) = %q, and with parameters %q, %v", storedType, kind, again, err)
	}
}

func FuzzClassify(f *testing.F) {
	for _, storedType := range storedTypes() {
		for _, head := range heads() {
			f.Add(storedType, head)
		}
	}
	f.Fuzz(func(t *testing.T, storedType string, head []byte) {
		checkClassify(t, storedType, head)
	})
}

func FuzzImageBounds(f *testing.F) {
	for _, head := range heads() {
		for _, kind := range preview.Kinds() {
			f.Add(string(kind), head)
		}
	}
	f.Add("png", pngHead(65535, 65535))
	f.Add("gif", gif("GIF89a", 1, 1, gifFrame(0, 0, 65535, 65535, false)))
	f.Add("gif", gif("GIF89a", 30, 20, gifComment(300), gifControl, gifFrame(10, 5, 20, 15, true), gifFrame(0, 0, 30, 20, false)))
	f.Add("gif", append(gifScreen("GIF87a", 10, 10), 0x21, 0xFE, 0x10, 'a'))
	f.Add("webp", webpLossless(100, 100))
	f.Add("webp", webpExtended(100, 100))
	f.Add("jpeg", jpegHead(app(100), frame(0xC2, 1, 1)))
	f.Fuzz(func(t *testing.T, kind string, head []byte) {
		b, err := preview.ImageBounds(preview.Kind(kind), head)
		if err != nil {
			if !errors.Is(err, domain.ErrNotPreviewable) && !errors.Is(err, domain.ErrPreviewTooLarge) {
				t.Fatalf("ImageBounds = %v", err)
			}
			return
		}
		if !preview.Kind(kind).Image() || b.Width == 0 || b.Height == 0 || b.Width > preview.MaxImageSide ||
			b.Height > preview.MaxImageSide || uint64(b.Width)*uint64(b.Height) > preview.MaxImagePixels {
			t.Fatalf("ImageBounds(%q) = %+v", kind, b)
		}
		// What is read is read from the bytes of the kind: an image of a
		// kind its signature is not of has no size.
		if k, err := preview.Classify("", head); err != nil || k != preview.Kind(kind) {
			t.Fatalf("ImageBounds(%q) = %+v of bytes classified as %q, %v", kind, b, k, err)
		}
	})
}
