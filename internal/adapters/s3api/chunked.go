package s3api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// aws-chunked decoding for the payload option
// STREAMING-UNSIGNED-PAYLOAD-TRAILER, implemented from the public AWS
// documentation ("Signature calculations for the Authorization Header:
// Transferring a payload in multiple chunks", trailing-header variant). It is
// what current AWS SDKs send by default over TLS:
//
//	<hex size>\r\n<data>\r\n   ...   0\r\n<name>:<value>\r\n\r\n
//
// The chunks carry no signatures; the body is protected by TLS and by the
// trailing checksum, which the caller verifies.
const (
	maxChunkLine     = 1 << 10
	maxTrailerLine   = 4 << 10
	maxTrailers      = 8
	maxChunkSize     = 1 << 30
	chunkedEncoding  = "aws-chunked"
	headerDecodedLen = "X-Amz-Decoded-Content-Length"
	headerTrailer    = "X-Amz-Trailer"
)

// errChunkFraming reports a body that is not valid aws-chunked framing.
var errChunkFraming = errors.New("malformed aws-chunked body")

// chunkedReader yields the decoded payload of an aws-chunked body and
// collects the trailers that follow it.
type chunkedReader struct {
	src       *bufio.Reader
	remaining int64 // bytes left in the current chunk
	started   bool
	done      bool // the final chunk and the trailers have been read
	trailers  map[string]string
	err       error
}

func newChunkedReader(src io.Reader) *chunkedReader {
	return &chunkedReader{src: bufio.NewReaderSize(src, 64<<10), trailers: map[string]string{}}
}

// Read implements io.Reader.
func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	for c.remaining == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.nextChunk(); err != nil {
			c.err = err
			return 0, err
		}
	}
	n, err := c.src.Read(p[:min(int64(len(p)), c.remaining)])
	c.remaining -= int64(n)
	if err != nil && (n == 0 || !errors.Is(err, io.EOF)) {
		c.err = truncated(err)
		return n, c.err
	}
	return n, nil
}

// nextChunk reads the header of the next chunk, and the trailers after the
// last one.
func (c *chunkedReader) nextChunk() error {
	if c.started {
		// The data of the previous chunk is followed by CRLF.
		if err := c.expectCRLF(); err != nil {
			return err
		}
	}
	c.started = true

	line, err := c.readLine(maxChunkLine)
	if err != nil {
		return err
	}
	// Unsigned chunks have no extensions; anything after ';' is ignored.
	sizeText, _, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
	if err != nil || size < 0 || size > maxChunkSize {
		return fmt.Errorf("%w: bad chunk size", errChunkFraming)
	}
	if size > 0 {
		c.remaining = size
		return nil
	}
	return c.readTrailers()
}

func (c *chunkedReader) readTrailers() error {
	for lines := 0; ; lines++ {
		line, err := c.readLine(maxTrailerLine)
		if err != nil {
			return err
		}
		if line == "" {
			c.done = true
			return nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || lines == maxTrailers {
			return fmt.Errorf("%w: bad trailer", errChunkFraming)
		}
		c.trailers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
}

func (c *chunkedReader) expectCRLF() error {
	var crlf [2]byte
	if _, err := io.ReadFull(c.src, crlf[:]); err != nil {
		return truncated(err)
	}
	if crlf != [2]byte{'\r', '\n'} {
		return fmt.Errorf("%w: chunk data is not followed by CRLF", errChunkFraming)
	}
	return nil
}

// readLine reads one CRLF-terminated line of at most limit bytes.
func (c *chunkedReader) readLine(limit int) (string, error) {
	var line []byte
	for {
		b, err := c.src.ReadByte()
		if err != nil {
			return "", truncated(err)
		}
		if b == '\n' {
			if len(line) == 0 || line[len(line)-1] != '\r' {
				return "", fmt.Errorf("%w: line is not terminated by CRLF", errChunkFraming)
			}
			return string(line[:len(line)-1]), nil
		}
		if len(line) == limit {
			return "", fmt.Errorf("%w: line too long", errChunkFraming)
		}
		line = append(line, b)
	}
}

// truncated turns the end of the body in the middle of the framing into the
// domain's incomplete-body error and passes other errors through.
func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: aws-chunked body ends early", domain.ErrIncompleteBody)
	}
	return err
}
