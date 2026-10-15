package preview

import (
	"encoding/binary"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
)

// Expected says what a stored type lets an object be, before any of its
// bytes are read: the kind it names; "" for a type that leaves the kind to
// the bytes (application/octet-stream, or none); or an error wrapping
// domain.ErrNotPreviewable for a type that names no kind a preview has -
// SVG, HTML, XML and script among them. Such an object need not be read to
// be refused. Classify decides, once the bytes are there.
func Expected(storedType string) (Kind, error) {
	mediaType := MediaType(storedType)
	if mediaType == "" || mediaType == "application/octet-stream" {
		return "", nil
	}
	kind, known := kindOf(mediaType)
	if !known {
		return "", fmt.Errorf("%w: its type is of no kind a preview has", domain.ErrNotPreviewable)
	}
	return kind, nil
}

// GIFFrames checks the frames of a GIF as its bytes go by, all of them:
// ImageBounds sees the frames that begin within the first BoundsHeadSize
// bytes only, and a browser draws a frame larger than the screen at its own
// size. It is written the whole GIF, from its first byte, in pieces of any
// size; a write fails - and every write after it - at the image descriptor
// of a frame that reaches past the screen (domain.ErrPreviewTooLarge), and at
// a block that is no block of a GIF (domain.ErrNotPreviewable), where what
// follows can no longer be told apart. After the trailer nothing is looked
// at. A GIF that ends early is no failure of it: a browser draws what there
// is.
type GIFFrames struct {
	screen Bounds
	// step is the part of the GIF being read; the first have bytes of buf
	// are what of it has arrived.
	step gifStep
	buf  [gifScreenSize]byte
	have int
	// skip is how many bytes to pass over before the step begins.
	skip int
	done bool
	err  error
}

// gifStep is a part of a GIF that GIFFrames reads.
type gifStep int

const (
	// gifScreenStep is the signature and the logical screen descriptor.
	gifScreenStep gifStep = iota
	// gifBlockStep is the byte that introduces the next block.
	gifBlockStep
	// gifLabelStep is the label of an extension.
	gifLabelStep
	// gifDescriptorStep is the rest of an image descriptor.
	gifDescriptorStep
	// gifCodeSizeStep is the minimum code size before a frame's data.
	gifCodeSizeStep
	// gifSubBlockStep is the size of the next sub-block, 0 for the
	// terminator.
	gifSubBlockStep
)

// The sizes of the parts of a GIF that are read whole.
const (
	gifScreenSize     = 13
	gifDescriptorSize = 9
)

// size is how many bytes the step reads.
func (s gifStep) size() int {
	switch s {
	case gifScreenStep:
		return gifScreenSize
	case gifDescriptorStep:
		return gifDescriptorSize
	default:
		return 1
	}
}

// NewGIFFrames returns a check of the frames of one GIF.
func NewGIFFrames() *GIFFrames {
	return &GIFFrames{}
}

// Write implements io.Writer: it reads the next bytes of the GIF.
func (g *GIFFrames) Write(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	written := len(p)
	for len(p) > 0 && !g.done {
		if g.skip > 0 {
			n := min(g.skip, len(p))
			g.skip -= n
			p = p[n:]
			continue
		}
		need := g.step.size()
		n := copy(g.buf[g.have:need], p)
		g.have += n
		p = p[n:]
		if g.have < need {
			break
		}
		g.have = 0
		if err := g.read(g.buf[:need]); err != nil {
			g.err = err
			return 0, err
		}
	}
	return written, nil
}

// read takes in one whole step and says what comes next.
func (g *GIFFrames) read(b []byte) error {
	switch g.step {
	case gifScreenStep:
		if !has(b, signatureGIF7) && !has(b, signatureGIF9) {
			return fmt.Errorf("%w: the image is no GIF", domain.ErrNotPreviewable)
		}
		g.screen = Bounds{Width: uint32(binary.LittleEndian.Uint16(b[6:8])), Height: uint32(binary.LittleEndian.Uint16(b[8:10]))}
		g.skip, g.step = colourTable(b[10]), gifBlockStep
	case gifBlockStep:
		switch b[0] {
		case 0x21:
			g.step = gifLabelStep
		case 0x2C:
			g.step = gifDescriptorStep
		case 0x3B:
			g.done = true
		default:
			return fmt.Errorf("%w: a block of the image is malformed", domain.ErrNotPreviewable)
		}
	case gifLabelStep:
		g.step = gifSubBlockStep
	case gifDescriptorStep:
		left, top := uint32(binary.LittleEndian.Uint16(b[0:2])), uint32(binary.LittleEndian.Uint16(b[2:4]))
		width, height := uint32(binary.LittleEndian.Uint16(b[4:6])), uint32(binary.LittleEndian.Uint16(b[6:8]))
		if left+width > g.screen.Width || top+height > g.screen.Height {
			return fmt.Errorf("%w: a frame of the image reaches past its %d × %d screen", domain.ErrPreviewTooLarge, g.screen.Width, g.screen.Height)
		}
		g.skip, g.step = colourTable(b[8]), gifCodeSizeStep
	case gifCodeSizeStep:
		g.step = gifSubBlockStep
	case gifSubBlockStep:
		if b[0] == 0 {
			g.step = gifBlockStep
		} else {
			g.skip = int(b[0])
		}
	}
	return nil
}
