package s3api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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

	// signer, when not nil, verifies the signature every chunk carries;
	// the chunks are then "<hex size>;chunk-signature=<signature>".
	signer *chunkSigner
	// trailing says that trailers follow the last chunk.
	trailing bool
	// declared is the signature the current chunk came with, and sum the
	// hash of its data so far.
	declared string
	sum      hash.Hash
}

func newChunkedReader(src io.Reader) *chunkedReader {
	return &chunkedReader{src: bufio.NewReaderSize(src, 64<<10), trailers: map[string]string{}, trailing: true}
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
	if c.sum != nil {
		c.sum.Write(p[:n])
	}
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
		// And its signature is checked before anything of the next one
		// is handed on.
		if err := c.checkChunk(); err != nil {
			return err
		}
	}
	c.started = true

	line, err := c.readLine(maxChunkLine)
	if err != nil {
		return err
	}
	// Unsigned chunks have no extensions; anything after ';' is ignored.
	sizeText, extension, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
	if err != nil || size < 0 || size > maxChunkSize {
		return fmt.Errorf("%w: bad chunk size", errChunkFraming)
	}
	if c.signer != nil {
		declared, ok := strings.CutPrefix(strings.TrimSpace(extension), "chunk-signature=")
		if !ok || len(declared) != signatureHexLength {
			return fmt.Errorf("%w: chunk without a signature", errAuthSignature)
		}
		c.declared, c.sum = declared, sha256.New()
	}
	if size > 0 {
		c.remaining = size
		return nil
	}
	// The last chunk is empty, and signed like the others.
	if err := c.checkChunk(); err != nil {
		return err
	}
	return c.readTrailers()
}

// checkChunk verifies the signature of the chunk whose data has been read.
func (c *chunkedReader) checkChunk() error {
	if c.signer == nil || c.sum == nil {
		return nil
	}
	err := c.signer.check(chunkAlgorithm, c.declared, hex.EncodeToString(c.sum.Sum(nil)))
	c.sum = nil
	return err
}

// trailerSignature is the trailer that signs the other trailers of a signed
// stream.
const trailerSignature = "x-amz-trailer-signature"

func (c *chunkedReader) readTrailers() error {
	// What the trailer signature covers: every trailer, as "name:value\n".
	var signedText strings.Builder
	signature := ""
	for lines := 0; ; lines++ {
		line, err := c.readLine(maxTrailerLine)
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || lines == maxTrailers || !c.trailing || signature != "" {
			// No trailers were announced, or something follows the
			// signature of the trailers.
			return fmt.Errorf("%w: bad trailer", errChunkFraming)
		}
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if c.signer != nil && name == trailerSignature {
			signature = value
			continue
		}
		c.trailers[name] = value
		signedText.WriteString(name + ":" + value + "\n")
	}
	if c.signer != nil && c.trailing {
		if len(signature) != signatureHexLength {
			return fmt.Errorf("%w: trailers without a signature", errAuthSignature)
		}
		digest := sha256.Sum256([]byte(signedText.String()))
		if err := c.signer.check(trailerAlgorithm, signature, hex.EncodeToString(digest[:])); err != nil {
			return err
		}
	}
	c.done = true
	return nil
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
