package s3api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

const (
	checksumHeaderPrefix = "x-amz-checksum-"
	// crc64NVMEPolynomial is the reflected polynomial of CRC-64/NVME.
	crc64NVMEPolynomial = 0x9a6c9329ac4bc9b5
)

// Payload errors. They wrap domain errors so that use cases can pass them
// through, and are told apart when the response is written.
var (
	errSHA256Mismatch      = fmt.Errorf("%w: x-amz-content-sha256", domain.ErrBadDigest)
	errInvalidDigest       = fmt.Errorf("%w: Content-MD5 is not a base64 MD5 digest", domain.ErrInvalidArgument)
	errUnsupportedPayload  = errors.New("payload option is not supported")
	errUnsupportedChecksum = errors.New("checksum algorithm is not supported")
	errBadPayloadHeader    = fmt.Errorf("%w: payload headers", domain.ErrInvalidArgument)
)

// newChecksum returns the hash of an S3 checksum algorithm, by the name used
// in x-amz-checksum-<name>.
func newChecksum(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "crc32":
		return crc32.NewIEEE(), nil
	case "crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), nil
	case "crc64nvme":
		return crc64.New(crc64.MakeTable(crc64NVMEPolynomial)), nil
	case "sha256":
		return sha256.New(), nil
	default:
		return nil, fmt.Errorf("%w: %s", errUnsupportedChecksum, algorithm)
	}
}

// expectation is one digest the client declared for the payload.
type expectation struct {
	// header names the digest in error messages and in the response.
	header string
	hash   hash.Hash
	// want is the declared value; for a trailing checksum it is only known
	// once the body has been read, and comes from trailer.
	want    []byte
	trailer func() (string, bool)
	// mismatch is returned when the digest differs.
	mismatch error
}

// payload is the body of an upload together with everything the client said
// about it.
type payload struct {
	// body yields the object's bytes.
	body io.Reader
	// size is the declared length of body, or -1.
	size int64
	// contentEncoding is the Content-Encoding to store, without aws-chunked.
	contentEncoding string

	etag         hash.Hash
	expectations []expectation
	chunked      *chunkedReader
	// checksums are the verified checksum headers to echo in the response.
	checksums map[string]string
}

// newPayload interprets the payload headers of a signed upload. payloadHash
// is the x-amz-content-sha256 value the signature covers.
func newPayload(r *http.Request, payloadHash string, chunks *chunkSigner) (*payload, error) {
	p := &payload{
		size:            r.ContentLength,
		contentEncoding: r.Header.Get("Content-Encoding"),
		etag:            newETagHash(),
		checksums:       map[string]string{},
	}
	var source io.Reader = r.Body

	switch {
	case payloadHash == payloadUnsigned:
	case payloadHash == payloadStreamingTrailer:
		if err := p.useChunked(r, nil, true); err != nil {
			return nil, err
		}
		source = p.chunked
	case payloadHash == payloadStreamingSigned, payloadHash == payloadSignedTrailer:
		if chunks == nil {
			return nil, fmt.Errorf("%w: %s", errUnsupportedPayload, payloadHash)
		}
		if err := p.useChunked(r, chunks, payloadHash == payloadSignedTrailer); err != nil {
			return nil, err
		}
		source = p.chunked
	case strings.HasPrefix(payloadHash, payloadStreamingPrefix):
		// ECDSA-signed chunks (SigV4A) and whatever comes next.
		return nil, fmt.Errorf("%w: %s", errUnsupportedPayload, payloadHash)
	default:
		want, err := hex.DecodeString(payloadHash)
		if err != nil || len(want) != sha256.Size || payloadHash != strings.ToLower(payloadHash) {
			return nil, fmt.Errorf("%w: x-amz-content-sha256 is not a SHA-256 digest", errBadPayloadHeader)
		}
		p.expectations = append(p.expectations, expectation{
			header: strings.ToLower(headerContentSHA), hash: sha256.New(), want: want, mismatch: errSHA256Mismatch,
		})
	}

	if err := p.expectContentMD5(r); err != nil {
		return nil, err
	}
	if err := p.expectChecksumHeaders(r); err != nil {
		return nil, err
	}

	writers := []io.Writer{p.etag}
	for _, e := range p.expectations {
		writers = append(writers, e.hash)
	}
	p.body = io.TeeReader(source, io.MultiWriter(writers...))
	return p, nil
}

// useChunked sets the payload up for an aws-chunked body: with signed chunks
// when signer is not nil, and with a trailing checksum when trailing is set.
func (p *payload) useChunked(r *http.Request, signer *chunkSigner, trailing bool) error {
	// The decoded length is absent when the client streams a body whose
	// length it does not know; the framing and the trailing checksum then
	// protect the payload on their own.
	p.size = -1
	if raw := r.Header.Get(headerDecodedLen); raw != "" {
		decoded, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || decoded < 0 {
			return fmt.Errorf("%w: %s is not a length", errBadPayloadHeader, headerDecodedLen)
		}
		p.size = decoded
	}
	if signer != nil && p.size < 0 {
		// A signed stream says how long its content is.
		return fmt.Errorf("%w: %s is required", errBadPayloadHeader, headerDecodedLen)
	}
	p.chunked = newChunkedReader(r.Body)
	p.chunked.signer, p.chunked.trailing = signer, trailing

	// aws-chunked is transfer framing, not part of the object.
	var kept []string
	for coding := range strings.SplitSeq(p.contentEncoding, ",") {
		if coding = strings.TrimSpace(coding); coding != "" && !strings.EqualFold(coding, chunkedEncoding) {
			kept = append(kept, coding)
		}
	}
	p.contentEncoding = strings.Join(kept, ",")

	if !trailing {
		return nil
	}
	trailer := strings.ToLower(strings.TrimSpace(r.Header.Get(headerTrailer)))
	algorithm, isChecksum := strings.CutPrefix(trailer, checksumHeaderPrefix)
	if !isChecksum || strings.Contains(trailer, ",") {
		return fmt.Errorf("%w: %s must name one x-amz-checksum-* trailer", errBadPayloadHeader, headerTrailer)
	}
	sum, err := newChecksum(algorithm)
	if err != nil {
		return err
	}
	chunked := p.chunked
	p.expectations = append(p.expectations, expectation{
		header: trailer, hash: sum, mismatch: domain.ErrBadDigest,
		trailer: func() (string, bool) { value, ok := chunked.trailers[trailer]; return value, ok },
	})
	return nil
}

func (p *payload) expectContentMD5(r *http.Request) error {
	value := r.Header.Get("Content-MD5")
	if value == "" {
		return nil
	}
	want, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(want) != p.etag.Size() {
		return errInvalidDigest
	}
	p.expectations = append(p.expectations, expectation{
		header: "content-md5", hash: newETagHash(), want: want, mismatch: domain.ErrBadDigest,
	})
	return nil
}

// expectChecksumHeaders registers every x-amz-checksum-<algorithm> request
// header as a digest to verify.
func (p *payload) expectChecksumHeaders(r *http.Request) error {
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		algorithm, ok := strings.CutPrefix(lower, checksumHeaderPrefix)
		if !ok || algorithm == "mode" || algorithm == "algorithm" || algorithm == "type" {
			continue
		}
		sum, err := newChecksum(algorithm)
		if err != nil {
			return err
		}
		want, err := base64.StdEncoding.DecodeString(values[0])
		if err != nil || len(want) != sum.Size() {
			return fmt.Errorf("%w: %s is not a base64 %s digest", errBadPayloadHeader, lower, algorithm)
		}
		p.expectations = append(p.expectations, expectation{header: lower, hash: sum, want: want, mismatch: domain.ErrBadDigest})
	}
	return nil
}

// verify is the use case's Verify callback: it is called when the body has
// been read to its end, checks every declared digest and returns the ETag.
func (p *payload) verify() (string, error) {
	if p.chunked != nil && !p.chunked.done {
		// The decoded length was reached but the terminating chunk and the
		// trailers were not there: read them now.
		if _, err := io.Copy(io.Discard, p.body); err != nil {
			return "", err
		}
		if !p.chunked.done {
			return "", fmt.Errorf("%w: aws-chunked body has no final chunk", domain.ErrIncompleteBody)
		}
	}
	for _, e := range p.expectations {
		want := e.want
		if e.trailer != nil {
			value, ok := e.trailer()
			decoded, err := base64.StdEncoding.DecodeString(value)
			if !ok || err != nil {
				return "", fmt.Errorf("%w: trailer %s is missing or malformed", domain.ErrBadDigest, e.header)
			}
			want = decoded
		}
		got := e.hash.Sum(nil)
		if string(got) != string(want) {
			return "", fmt.Errorf("%w: %s", e.mismatch, e.header)
		}
		if strings.HasPrefix(e.header, checksumHeaderPrefix) {
			p.checksums[e.header] = base64.StdEncoding.EncodeToString(got)
		}
	}
	return hex.EncodeToString(p.etag.Sum(nil)), nil
}
