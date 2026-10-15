// Package preview decides what kind of thing an object is, for the console
// to show it (ADR-0028): from the type it was stored with AND the signature
// its first bytes carry, never from its key. The type comes from whoever
// uploaded the object and can lie; the bytes alone can be made to look like
// two things at once. Only when both say the same is an object previewable,
// and SVG, HTML, XML and script never are, whatever they claim to be.
//
// It decides only. The bytes it is shown are not changed, and nothing of an
// object is ever rendered on the server: the console draws what the gateway
// hands it as an attachment.
package preview

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
)

// Kind is what an object is shown as. Its values are part of the management
// API's contract (the header X-CactusFS-Preview-Kind): never change one,
// only add.
type Kind string

// The kinds there are.
const (
	// KindPNG is a PNG image.
	KindPNG Kind = "png"
	// KindJPEG is a JPEG image.
	KindJPEG Kind = "jpeg"
	// KindGIF is a GIF image.
	KindGIF Kind = "gif"
	// KindWebP is a WebP image.
	KindWebP Kind = "webp"
	// KindPDF is a PDF document.
	KindPDF Kind = "pdf"
	// KindDOCX is a Word document of Office Open XML.
	KindDOCX Kind = "docx"
	// KindXLSX is an Excel workbook of Office Open XML.
	KindXLSX Kind = "xlsx"
	// KindText is plain text in UTF-8.
	KindText Kind = "text"
	// KindCSV is comma-separated values in UTF-8.
	KindCSV Kind = "csv"
	// KindJSON is JSON in UTF-8.
	KindJSON Kind = "json"
)

// Kinds returns every kind, in the order of the list above.
func Kinds() []Kind {
	return []Kind{KindPNG, KindJPEG, KindGIF, KindWebP, KindPDF, KindDOCX, KindXLSX, KindText, KindCSV, KindJSON}
}

// Image reports whether the kind is an image, whose size ImageBounds checks.
func (k Kind) Image() bool {
	switch k {
	case KindPNG, KindJPEG, KindGIF, KindWebP:
		return true
	}
	return false
}

// Textual reports whether the kind is text, of which a preview shows the
// beginning only.
func (k Kind) Textual() bool {
	switch k {
	case KindText, KindCSV, KindJSON:
		return true
	}
	return false
}

// Bounds of what is previewed.
const (
	// HeadSize is how many of an object's first bytes Classify needs to
	// see; it may be shown fewer when the object is smaller.
	HeadSize = 4 << 10
	// BoundsHeadSize is how many of an image's first bytes ImageBounds
	// looks for its size in. A JPEG may carry its metadata - a thumbnail,
	// a colour profile - before the frame that says how large it is.
	BoundsHeadSize = 256 << 10
	// MaxImageSide is the most pixels an image previewed may have on a
	// side.
	MaxImageSide = 16384
	// MaxImagePixels is the most pixels an image previewed may have.
	MaxImagePixels = 40_000_000
)

// kindOf names the kind a stored type names. A type that is not here names
// none: SVG, HTML, XML and script among them.
func kindOf(mediaType string) (Kind, bool) {
	switch mediaType {
	case "image/png":
		return KindPNG, true
	case "image/jpeg", "image/jpg":
		return KindJPEG, true
	case "image/gif":
		return KindGIF, true
	case "image/webp":
		return KindWebP, true
	case "application/pdf":
		return KindPDF, true
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return KindDOCX, true
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return KindXLSX, true
	case "text/plain":
		return KindText, true
	case "text/csv":
		return KindCSV, true
	case "application/json":
		return KindJSON, true
	}
	return "", false
}

// The signatures of the binary kinds: what their first bytes are.
const (
	signaturePNG  = "\x89PNG\r\n\x1a\n"
	signatureJPEG = "\xFF\xD8\xFF"
	signatureGIF7 = "GIF87a"
	signatureGIF9 = "GIF89a"
	signatureRIFF = "RIFF"
	signatureWebP = "WEBP"
	signaturePDF  = "%PDF-"
	signatureZip  = "PK\x03\x04"
)

// has reports whether b begins with prefix.
func has(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

// signature names the kind whose signature head begins with, or "" for
// none. A zip names no kind of its own: what an archive holds is for its
// stored type to say.
func signature(head []byte) Kind {
	switch {
	case has(head, signaturePNG):
		return KindPNG
	case has(head, signatureJPEG):
		return KindJPEG
	case has(head, signatureGIF7), has(head, signatureGIF9):
		return KindGIF
	case has(head, signatureRIFF) && len(head) >= 12 && string(head[8:12]) == signatureWebP:
		return KindWebP
	case has(head, signaturePDF):
		return KindPDF
	}
	return ""
}

// MediaType returns a stored type as Classify reads it: without its
// parameters and surrounding space, in lower case.
func MediaType(storedType string) string {
	mediaType, _, _ := strings.Cut(storedType, ";")
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// Classify decides what an object is shown as, from the type it was stored
// with and its first bytes (at most HeadSize of them are looked at). The
// stored type must name a kind and the bytes must carry that kind's
// signature: an image, a PDF, or - for DOCX and XLSX - a zip. A stored type
// of application/octet-stream, or none, lets the bytes decide, and then
// only for images and PDF. The text kinds need their type to be stored, and
// bytes that are UTF-8, hold no NUL and carry no binary signature.
//
// Anything else wraps domain.ErrNotPreviewable.
func Classify(storedType string, head []byte) (Kind, error) {
	if len(head) > HeadSize {
		head = head[:HeadSize]
	}
	mediaType := MediaType(storedType)
	found := signature(head)
	if mediaType == "" || mediaType == "application/octet-stream" {
		if found == "" {
			return "", fmt.Errorf("%w: its type is not stored and its bytes are of no kind a preview has", domain.ErrNotPreviewable)
		}
		return found, nil
	}
	kind, known := kindOf(mediaType)
	if !known {
		return "", fmt.Errorf("%w: its type is of no kind a preview has", domain.ErrNotPreviewable)
	}
	switch {
	case kind == KindDOCX || kind == KindXLSX:
		if !has(head, signatureZip) {
			return "", fmt.Errorf("%w: its bytes are not of the kind its type says", domain.ErrNotPreviewable)
		}
	case kind.Textual():
		if found != "" || has(head, signatureZip) {
			return "", fmt.Errorf("%w: its type says text and its bytes are of another kind", domain.ErrNotPreviewable)
		}
		if bytes.IndexByte(head, 0) >= 0 || !validUTF8(head) {
			return "", fmt.Errorf("%w: its type says text and its bytes are not UTF-8 text", domain.ErrNotPreviewable)
		}
	case found != kind:
		return "", fmt.Errorf("%w: its bytes are not of the kind its type says", domain.ErrNotPreviewable)
	}
	return kind, nil
}

// validUTF8 reports whether head is UTF-8, but for the last character,
// which may have been cut off where the head ends.
func validUTF8(head []byte) bool {
	if utf8.Valid(head) {
		return true
	}
	// Where the last character begins, at most three bytes from the end.
	for back := 1; back <= utf8.UTFMax-1 && back <= len(head); back++ {
		start := len(head) - back
		if utf8.RuneStart(head[start]) {
			return !utf8.FullRune(head[start:]) && utf8.Valid(head[:start])
		}
	}
	return false
}

// Bounds is the size of an image, in pixels.
type Bounds struct {
	Width, Height uint32
}

// ImageBounds reads the size of an image of the kind given from its first
// bytes (at most BoundsHeadSize of them are looked at), and refuses one
// that is larger on a side than MaxImageSide or of more pixels than
// MaxImagePixels: the console would have the browser decode it, and a few
// bytes of a file can claim billions of pixels.
//
// An image that is larger wraps domain.ErrPreviewTooLarge. One whose size
// cannot be read - not within the bytes looked at, or of no pixels - wraps
// domain.ErrNotPreviewable, as does a kind that is no image.
func ImageBounds(kind Kind, head []byte) (Bounds, error) {
	if len(head) > BoundsHeadSize {
		head = head[:BoundsHeadSize]
	}
	var (
		b  Bounds
		ok bool
	)
	switch kind {
	case KindPNG:
		b, ok = pngBounds(head)
	case KindJPEG:
		b, ok = jpegBounds(head)
	case KindGIF:
		var outside bool
		if b, ok, outside = gifBounds(head); ok && outside {
			return b, fmt.Errorf("%w: a frame of the image reaches past its %d × %d screen", domain.ErrPreviewTooLarge, b.Width, b.Height)
		}
	case KindWebP:
		b, ok = webpBounds(head)
	default:
		return Bounds{}, fmt.Errorf("%w: %q is no image", domain.ErrNotPreviewable, kind)
	}
	if !ok || b.Width == 0 || b.Height == 0 {
		return Bounds{}, fmt.Errorf("%w: the size of the image cannot be read from its first %d bytes", domain.ErrNotPreviewable, len(head))
	}
	if b.Width > MaxImageSide || b.Height > MaxImageSide || uint64(b.Width)*uint64(b.Height) > MaxImagePixels {
		return b, fmt.Errorf("%w: the image is %d × %d pixels; a preview has at most %d on a side and %d in all",
			domain.ErrPreviewTooLarge, b.Width, b.Height, MaxImageSide, MaxImagePixels)
	}
	return b, nil
}

// pngBounds reads the IHDR chunk, which must come first.
func pngBounds(head []byte) (Bounds, bool) {
	if len(head) < 24 || !has(head, signaturePNG) || string(head[12:16]) != "IHDR" {
		return Bounds{}, false
	}
	return Bounds{Width: binary.BigEndian.Uint32(head[16:20]), Height: binary.BigEndian.Uint32(head[20:24])}, true
}

// gifBounds reads the logical screen descriptor and walks the blocks that
// follow it within head - colour tables, extensions and the frames with
// their data - to the image descriptor of every frame there. The size of a
// GIF is its screen; a browser may draw a frame larger than the screen at
// its own size, so a frame that reaches past the screen is reported as
// outside, and refused: that is stricter than checking each frame against
// the caps alone. ok is false when the bytes are no GIF, a block is
// malformed, or no frame begins within head. Frames that begin after head
// are not seen.
func gifBounds(head []byte) (b Bounds, ok, outside bool) {
	if len(head) < 13 || (!has(head, signatureGIF7) && !has(head, signatureGIF9)) {
		return Bounds{}, false, false
	}
	b = Bounds{Width: uint32(binary.LittleEndian.Uint16(head[6:8])), Height: uint32(binary.LittleEndian.Uint16(head[8:10]))}
	at := 13 + colourTable(head[10])
	frames := 0
	for at < len(head) {
		switch head[at] {
		case 0x21:
			// An extension: its introducer, its label, its sub-blocks.
			at = subBlocks(head, at+2)
		case 0x2C:
			// An image descriptor: where the frame is, how large, and
			// whether a colour table of its own follows; then the
			// minimum code size and the data's sub-blocks.
			if at+10 > len(head) {
				return b, frames > 0, outside
			}
			left, top := uint32(binary.LittleEndian.Uint16(head[at+1:at+3])), uint32(binary.LittleEndian.Uint16(head[at+3:at+5]))
			width, height := uint32(binary.LittleEndian.Uint16(head[at+5:at+7])), uint32(binary.LittleEndian.Uint16(head[at+7:at+9]))
			if left+width > b.Width || top+height > b.Height {
				outside = true
			}
			frames++
			at = subBlocks(head, at+10+colourTable(head[at+9])+1)
		case 0x3B:
			// The trailer: there is nothing after it.
			return b, frames > 0, outside
		default:
			return Bounds{}, false, false
		}
	}
	return b, frames > 0, outside
}

// colourTable returns the length of the colour table that the flags of a
// screen or image descriptor say follows it, or 0.
func colourTable(flags byte) int {
	if flags&0x80 == 0 {
		return 0
	}
	return 3 << (flags&0x07 + 1)
}

// subBlocks skips the sub-blocks that begin at at, to the one past their
// terminator; past the end of head when they do not end within it.
func subBlocks(head []byte, at int) int {
	for at < len(head) {
		size := int(head[at])
		at++
		if size == 0 {
			return at
		}
		at += size
	}
	return len(head)
}

// webpBounds reads the first chunk of the RIFF container: a lossy frame
// ("VP8 "), a lossless one ("VP8L") or the extended header ("VP8X"), which
// holds the size of the canvas.
func webpBounds(head []byte) (Bounds, bool) {
	if signature(head) != KindWebP || len(head) < 30 {
		return Bounds{}, false
	}
	data := head[20:]
	switch string(head[12:16]) {
	case "VP8 ":
		// A frame tag of three bytes, the start code, then two sizes of
		// 14 bits, each with two bits of scaling above it.
		if data[3] != 0x9D || data[4] != 0x01 || data[5] != 0x2A {
			return Bounds{}, false
		}
		return Bounds{
			Width:  uint32(binary.LittleEndian.Uint16(data[6:8]) & 0x3FFF),
			Height: uint32(binary.LittleEndian.Uint16(data[8:10]) & 0x3FFF),
		}, true
	case "VP8L":
		// A signature byte, then the width and the height less one, of
		// 14 bits each.
		if data[0] != 0x2F {
			return Bounds{}, false
		}
		bits := binary.LittleEndian.Uint32(data[1:5])
		return Bounds{Width: bits&0x3FFF + 1, Height: (bits>>14)&0x3FFF + 1}, true
	case "VP8X":
		// Flags and reserved bytes, then the canvas's width and height
		// less one, of 24 bits each.
		return Bounds{Width: uint24(data[4:7]) + 1, Height: uint24(data[7:10]) + 1}, true
	}
	return Bounds{}, false
}

// uint24 reads three bytes, least significant first.
func uint24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

// jpegBounds walks the markers of a JPEG to its first frame header.
func jpegBounds(head []byte) (Bounds, bool) {
	if !has(head, signatureJPEG) {
		return Bounds{}, false
	}
	at := 2
	for at < len(head) {
		if head[at] != 0xFF {
			return Bounds{}, false
		}
		// Any number of 0xFF may stand before a marker.
		for at < len(head) && head[at] == 0xFF {
			at++
		}
		if at >= len(head) {
			return Bounds{}, false
		}
		marker := head[at]
		at++
		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			// Markers that stand alone, without a segment.
			continue
		case marker == 0xD9 || marker == 0xDA:
			// The end of the image, or its data, before any frame.
			return Bounds{}, false
		}
		if at+2 > len(head) {
			return Bounds{}, false
		}
		length := int(binary.BigEndian.Uint16(head[at : at+2]))
		if length < 2 {
			return Bounds{}, false
		}
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			// A frame header: its length, the precision, the height and
			// the width.
			if length < 7 || at+7 > len(head) {
				return Bounds{}, false
			}
			return Bounds{
				Width:  uint32(binary.BigEndian.Uint16(head[at+5 : at+7])),
				Height: uint32(binary.BigEndian.Uint16(head[at+3 : at+5])),
			}, true
		}
		at += length
	}
	return Bounds{}, false
}
