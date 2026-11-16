package s3client

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/albievan/cactusfs/internal/adapters/sigv4"
)

// The payload of an object or a part is sent as signed chunks, implemented
// from the public AWS documentation ("Signature Calculations for the
// Authorization Header: Transferring Payload in Multiple Chunks
// (Chunked Upload) (AWS Signature Version 4)"):
//
//	<hex size>;chunk-signature=<signature>\r\n<data>\r\n   ...
//	0;chunk-signature=<signature>\r\n\r\n
//
// Each chunk is signed with the signature before it, beginning with the
// signature of the request; the last chunk is empty. A chunk that is changed,
// dropped, repeated or moved fails at the other deployment, and a body that
// ends before its last chunk commits nothing there.
const (
	// payloadStreamingSigned declares a payload of signed chunks.
	payloadStreamingSigned = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	// headerDecodedLength is the length of the payload inside the chunks.
	headerDecodedLength = "X-Amz-Decoded-Content-Length"
	// contentEncodingChunked names the framing, which the other
	// deployment takes off and does not store.
	contentEncodingChunked = "aws-chunked"
	// DefaultChunkSize is the payload of every chunk but the last. AWS
	// asks for at least 8 KiB.
	DefaultChunkSize = 64 << 10
	// chunkExtension is what stands between the size and the signature.
	chunkExtension = ";chunk-signature="
	// maxEmptyReads is how many reads that return nothing and no error a
	// src may answer, at its end, before it is taken for a failure
	// (io.Reader asks a reader not to make them).
	maxEmptyReads = 100
)

// Errors of the body of an upload.
var (
	// ErrShortBody is a body that ended before the size it was sent as.
	ErrShortBody = errors.New("s3client: the body ended before its size")
	// ErrLongBody is a body that goes on past the size it was sent as.
	ErrLongBody = errors.New("s3client: the body is longer than its size")
	// errClosed is a body read after it was closed.
	errClosed = errors.New("s3client: the body is closed")
)

// chunkedLength returns the length on the wire of size bytes sent as signed
// chunks of chunkSize.
func chunkedLength(size int64, chunkSize int) int64 {
	frame := func(n int64) int64 {
		return int64(len(strconv.FormatInt(n, 16))+len(chunkExtension)+sigv4.SignatureHexLength+2) + n + 2
	}
	full, rest := size/int64(chunkSize), size%int64(chunkSize)
	length := full*frame(int64(chunkSize)) + frame(0)
	if rest > 0 {
		length += frame(rest)
	}
	return length
}

// chunkedBody is the body of a request that sends exactly size bytes of src
// as signed chunks. It holds the signing key of one day and region - never
// the secret - and clears it when it is closed, has failed or has sent its
// last chunk.
//
// What src fails with, and a src that ends early or goes on past size, ends
// the body with an error that is kept (failure): the transport then cuts the
// request off, and the other deployment, short of the last chunk, commits
// nothing.
//
// The transport may close the body while a read of it waits for src. Close
// then does not wait: it marks the body closed, and that read clears the key
// when src returns.
type chunkedBody struct {
	// mu is held by a read, while it waits for src too.
	mu    sync.Mutex
	src   io.Reader
	left  int64
	chain sigv4.Chain
	data  []byte
	out   []byte
	done  bool
	// closed is set by Close.
	closed atomic.Bool
	// failedMu guards failed, the failure of src once there is one, which
	// is asked for while a read may wait.
	failedMu sync.Mutex
	failed   error
}

// newChunkedBody returns the body of size bytes of src, signed on from the
// chain, in chunks of chunkSize.
func newChunkedBody(src io.Reader, size int64, chain sigv4.Chain, chunkSize int) *chunkedBody {
	return &chunkedBody{src: src, left: size, chain: chain, data: make([]byte, chunkSize)}
}

// Read implements io.Reader.
func (b *chunkedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		clear(b.chain.Key)
		return 0, errClosed
	}
	if err := b.failure(); err != nil {
		return 0, err
	}
	for len(b.out) == 0 {
		if b.done {
			return 0, io.EOF
		}
		if err := b.next(); err != nil {
			clear(b.chain.Key)
			if !errors.Is(err, errClosed) {
				b.failedMu.Lock()
				b.failed = err
				b.failedMu.Unlock()
			}
			return 0, err
		}
	}
	n := copy(p, b.out)
	b.out = b.out[n:]
	return n, nil
}

// next frames the next chunk: the next bytes of src, or the empty last
// chunk once size bytes are framed and src is seen to end.
func (b *chunkedBody) next() error {
	n := int(min(int64(len(b.data)), b.left))
	if n > 0 {
		if err := b.fill(b.data[:n]); err != nil {
			return err
		}
		b.left -= int64(n)
	} else if err := b.ended(); err != nil {
		return err
	}
	if b.closed.Load() {
		return errClosed
	}
	digest := sha256.Sum256(b.data[:n])
	signature := b.chain.Next(sigv4.ChunkAlgorithm, hex.EncodeToString(digest[:]))
	frame := make([]byte, 0, 24+len(chunkExtension)+len(signature)+n)
	frame = strconv.AppendInt(frame, int64(n), 16)
	frame = append(frame, chunkExtension...)
	frame = append(frame, signature...)
	frame = append(frame, '\r', '\n')
	frame = append(frame, b.data[:n]...)
	frame = append(frame, '\r', '\n')
	b.out = frame
	if n == 0 {
		b.done = true
		clear(b.chain.Key)
	}
	return nil
}

// fill reads src until p is full, as io.ReadFull does - but a src that
// answers maxEmptyReads reads in a row with nothing and no error fails with
// io.ErrNoProgress instead of being asked forever.
func (b *chunkedBody) fill(p []byte) error {
	empty := 0
	for len(p) > 0 {
		n, err := b.src.Read(p)
		p = p[n:]
		switch {
		case len(p) == 0:
			return nil
		case errors.Is(err, io.EOF):
			return ErrShortBody
		case err != nil:
			return fmt.Errorf("s3client: reading the body: %w", err)
		case n == 0:
			empty++
			if empty == maxEmptyReads {
				return fmt.Errorf("s3client: reading the body: %w", io.ErrNoProgress)
			}
		default:
			empty = 0
		}
	}
	return nil
}

// ended reports whether src has nothing past the size: ErrLongBody when it
// does, the failure of src when it fails.
func (b *chunkedBody) ended() error {
	var one [1]byte
	for range maxEmptyReads {
		n, err := b.src.Read(one[:])
		switch {
		case n > 0:
			return ErrLongBody
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("s3client: reading the body: %w", err)
		}
	}
	return fmt.Errorf("s3client: reading the body: %w", io.ErrNoProgress)
}

// Close implements io.Closer. It clears the signing key, or leaves that to
// the read that waits for src; src is the caller's and is not closed.
func (b *chunkedBody) Close() error {
	b.closed.Store(true)
	if b.mu.TryLock() {
		clear(b.chain.Key)
		b.mu.Unlock()
	}
	return nil
}

// release ends the body once the request is done with it: it is closed,
// a read in flight - one the transport may still make after Do has returned
// - is waited for, and the signing key is cleared. After it nothing reads
// src.
func (b *chunkedBody) release() {
	b.closed.Store(true)
	b.mu.Lock()
	clear(b.chain.Key)
	b.mu.Unlock()
}

// failure returns what ended the body early, or nil. It does not wait for
// a read.
func (b *chunkedBody) failure() error {
	b.failedMu.Lock()
	defer b.failedMu.Unlock()
	return b.failed
}
