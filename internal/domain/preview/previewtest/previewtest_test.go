package previewtest_test

import (
	"bytes"
	"errors"
	"regexp"
	"strconv"
	"testing"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/preview/previewtest"
)

// TestTheCasesSayOneThing: each case is shown or refused, never both, and a
// refusal by the type alone is a refusal.
func TestTheCasesSayOneThing(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, c := range previewtest.Cases() {
		if names[c.Name] {
			t.Errorf("%s: twice", c.Name)
		}
		names[c.Name] = true
		shown, refused := c.Kind != "", c.Refusal != nil
		if shown == refused {
			t.Errorf("%s: kind %q and refusal %v", c.Name, c.Kind, c.Refusal)
		}
		if refused && !errors.Is(c.Refusal, domain.ErrNotPreviewable) && !errors.Is(c.Refusal, domain.ErrPreviewTooLarge) {
			t.Errorf("%s: refused with %v", c.Name, c.Refusal)
		}
		if c.ByType && !refused {
			t.Errorf("%s: refused by its type, and shown", c.Name)
		}
	}
	if len(names) < 30 {
		t.Errorf("%d cases", len(names))
	}
}

// TestTheGIFIsAScriptToo: the polyglot's text is a name, a comment that
// holds every byte of the GIF to its trailer, and a statement.
func TestTheGIFIsAScriptToo(t *testing.T) {
	t.Parallel()
	gif := previewtest.GIFScriptPolyglot()
	end := bytes.Index(gif, []byte("*/"))
	trailer := bytes.LastIndexByte(gif[:end], 0x3B)
	if !bytes.HasPrefix(gif, []byte("GIF89a/*")) || end < 0 || trailer != end-1 {
		t.Fatalf("the comment does not hold the GIF to its trailer: %q", gif)
	}
	if statement := string(gif[end:]); statement != "*/=1;alert('"+previewtest.Marker+"');//" {
		t.Fatalf("the statement is %q", statement)
	}
}

// TestThePDFsAreWhole: the offsets of every PDF made are those of its
// objects and its cross-reference table, so that a reader opens it.
func TestThePDFsAreWhole(t *testing.T) {
	t.Parallel()
	entry := regexp.MustCompile(`(\d{10}) 00000 n `)
	for name, pdf := range map[string][]byte{
		"polyglot": previewtest.PDFHTMLPolyglot(),
		"actions":  previewtest.PDFWithActions(),
		"bare":     previewtest.PDF("%comment\nacross lines", "<< /Type /Catalog >>"),
	} {
		if !bytes.HasPrefix(pdf, []byte("%PDF-1.7\n")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
			t.Errorf("%s: %q", name, pdf)
		}
		for i, match := range entry.FindAllSubmatch(pdf, -1) {
			at, _ := strconv.Atoi(string(match[1]))
			if !bytes.HasPrefix(pdf[at:], []byte(strconv.Itoa(i+1)+" 0 obj\n")) {
				t.Errorf("%s: object %d is not at %d", name, i+1, at)
			}
		}
		start := bytes.LastIndex(pdf, []byte("startxref\n"))
		at, err := strconv.Atoi(string(bytes.TrimSpace(bytes.TrimSuffix(pdf[start+len("startxref\n"):], []byte("%%EOF\n")))))
		if err != nil || !bytes.HasPrefix(pdf[at:], []byte("xref\n")) {
			t.Errorf("%s: startxref %d, %v", name, at, err)
		}
	}
	actions := previewtest.PDFWithActions()
	for _, asked := range []string{"/OpenAction", "/JavaScript", "/S /JavaScript", "/S /Launch", "/S /URI", "/Subtype /Link"} {
		if !bytes.Contains(actions, []byte(asked)) {
			t.Errorf("the PDF with actions has no %s", asked)
		}
	}
	html := previewtest.HTMLPDFPolyglot()
	if at := bytes.Index(html, []byte("%PDF-")); at <= 0 || at >= 1024 {
		t.Errorf("the HTML/PDF polyglot has its PDF at %d", at)
	}
}

// TestTheSizesAreAsSaid: a header, a padded image and one over a cap.
func TestTheSizesAreAsSaid(t *testing.T) {
	t.Parallel()
	if header := previewtest.PNGHeader(1, 2); len(header) != 33 {
		t.Errorf("a PNG header of %d bytes", len(header))
	}
	if png := previewtest.PNGOf(1, 1, 500); len(png) != 500 {
		t.Errorf("a PNG of %d bytes", len(png))
	}
	if over := previewtest.OneByteOver(1 << 20); len(over) != 1<<20+1 {
		t.Errorf("one byte over 1 MiB is %d bytes", len(over))
	}
	for _, markup := range [][]byte{previewtest.SVGWithScript(), previewtest.HTMLPage(), previewtest.PDFHTMLPolyglot()} {
		if !bytes.Contains(markup, []byte(previewtest.Marker)) || !bytes.Contains(markup, []byte("<script>")) {
			t.Errorf("no script and marker in %q", markup)
		}
	}
	// The texts that begin with a signature are text by every other test
	// Classify makes of text.
	for _, ascii := range [][]byte{previewtest.ASCIIGIFScript(), previewtest.ASCIIPDFPage()} {
		if !utf8.Valid(ascii) || bytes.IndexByte(ascii, 0) >= 0 || bytes.ContainsFunc(ascii, func(r rune) bool { return r > 0x7E }) {
			t.Errorf("not ASCII text: %q", ascii)
		}
	}
	if gif := previewtest.GIFFrameBomb(); len(gif) != 13+15+1 {
		t.Errorf("a GIF of %d bytes", len(gif))
	}
}
