// Package sigv4 is AWS Signature Version 4 as S3 uses it: the canonical
// request, the signing key, the signature and the chain that signs the
// chunks of a streamed payload, implemented from the public AWS
// documentation ("Authenticating Requests: Using the Authorization Header
// (AWS Signature Version 4)" and "Signature Calculations for the
// Authorization Header: Transferring Payload in Multiple Chunks").
//
// It is shared by what verifies a signature - the S3 API, which checks the
// requests of clients - and what makes one: SignRequest, for the requests a
// deployment sends to another S3 endpoint. Comparisons of signatures are in
// constant time.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The words and formats of the specification.
const (
	// Algorithm is the signing algorithm of a request.
	Algorithm = "AWS4-HMAC-SHA256"
	// Terminator ends the credential scope.
	Terminator = "aws4_request"
	// Service is the service of the credential scope: S3 and nothing else.
	Service = "s3"
	// AmzDateFormat is the layout of X-Amz-Date and of the timestamp of a
	// string to sign.
	AmzDateFormat = "20060102T150405Z"
	// ScopeDateFormat is the layout of the date of a credential scope.
	ScopeDateFormat = "20060102"
	// HeaderContentSHA256 declares the payload: its hash, or one of the
	// words of the payload options.
	HeaderContentSHA256 = "X-Amz-Content-Sha256"
	// HeaderAmzDate is the signing time.
	HeaderAmzDate = "X-Amz-Date"
	// ChunkAlgorithm is the algorithm of a link of the chain that signs a
	// chunk of a streamed payload.
	ChunkAlgorithm = "AWS4-HMAC-SHA256-PAYLOAD"
	// TrailerAlgorithm is the algorithm of the link that signs the
	// trailers of a streamed payload.
	TrailerAlgorithm = "AWS4-HMAC-SHA256-TRAILER"
	// EmptyPayloadSHA256 is the hash of no bytes, in hexadecimal.
	EmptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// UnsignedPayload declares a payload the signature does not cover.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// SignatureHexLength is the length of a signature in hexadecimal.
	SignatureHexLength = 2 * sha256.Size
)

// CanonicalRequest builds the canonical request of the specification for
// r: the method, the path and the query of its URL, the headers named by
// signedHeaders - lower case and sorted - with signedHeadersRaw, which is
// how the signature names them, and the payload declaration. The host is
// r.Host, as a server receives it. A content-length that is signed and is
// not among the headers is r.ContentLength.
func CanonicalRequest(r *http.Request, signedHeaders []string, signedHeadersRaw, payloadHash string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(URIEncode(r.URL.Path, false))
	b.WriteByte('\n')
	b.WriteString(CanonicalQuery(r.URL.RawQuery))
	b.WriteByte('\n')
	for _, name := range signedHeaders {
		b.WriteString(name)
		b.WriteByte(':')
		switch {
		case name == "host":
			b.WriteString(r.Host)
		case name == "content-length" && len(r.Header.Values(name)) == 0:
			b.WriteString(strconv.FormatInt(r.ContentLength, 10))
		default:
			for i, value := range r.Header.Values(name) {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(strings.Join(strings.Fields(value), " "))
			}
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(signedHeadersRaw)
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

// CanonicalQuery re-encodes and sorts the query string. A component that is
// not valid percent-encoding is kept as it is; a signature over it will then
// simply not match what the client computed.
func CanonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	pairs := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, URIEncode(unescape(name), true)+"="+URIEncode(unescape(value), true))
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

func unescape(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

// URIEncode percent-encodes every byte except the unreserved characters
// A-Z a-z 0-9 - _ . ~, and '/' unless encodeSlash is set, with uppercase
// hexadecimal digits, as the specification defines UriEncode.
func URIEncode(s string, encodeSlash bool) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0F])
		}
	}
	return b.String()
}

// HMACSHA256 returns the HMAC-SHA256 of data under key.
func HMACSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// Scope returns the credential scope of a day and a region:
// "<date>/<region>/s3/aws4_request".
func Scope(date, region string) string {
	return date + "/" + region + "/" + Service + "/" + Terminator
}

// SigningKey derives the key that signs the requests of one day and region
// from the secret. The copy of the secret it makes on the way is cleared.
func SigningKey(secret []byte, date, region string) []byte {
	seed := make([]byte, 0, len("AWS4")+len(secret))
	seed = append(append(seed, "AWS4"...), secret...)
	key := HMACSHA256(seed, date)
	clear(seed)
	key = HMACSHA256(key, region)
	key = HMACSHA256(key, Service)
	return HMACSHA256(key, Terminator)
}

// StringToSign returns the string to sign of a canonical request signed at
// timestamp (AmzDateFormat) under scope.
func StringToSign(timestamp, scope, canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	return Algorithm + "\n" + timestamp + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
}

// Signature returns the signature of stringToSign under the signing key,
// in hexadecimal.
func Signature(signingKey []byte, stringToSign string) string {
	return hex.EncodeToString(HMACSHA256(signingKey, stringToSign))
}

// Expected returns the signature a holder of secret would have produced for
// a canonical request signed at timestamp, on date in region.
func Expected(secret []byte, date, region, timestamp, canonical string) string {
	return Signature(SigningKey(secret, date, region), StringToSign(timestamp, Scope(date, region), canonical))
}

// Chain is the chain of signatures of a streamed payload: each chunk, and
// then the trailers, is signed with the signature before it, beginning with
// the signature of the request. It holds the signing key of one day and
// region - never the secret.
type Chain struct {
	// Key is the signing key (SigningKey).
	Key []byte
	// Timestamp is the signing time of the request, as AmzDateFormat.
	Timestamp string
	// Scope is the credential scope of the request.
	Scope string
	// Previous is the signature of the last link: at first, the seed - the
	// signature of the request.
	Previous string
}

// link returns the signature of the next link: a chunk whose data hashes
// to digestHex (ChunkAlgorithm), or trailers that do (TrailerAlgorithm).
func (c *Chain) link(algorithm, digestHex string) string {
	stringToSign := algorithm + "\n" + c.Timestamp + "\n" + c.Scope + "\n" + c.Previous + "\n"
	if algorithm == ChunkAlgorithm {
		stringToSign += EmptyPayloadSHA256 + "\n"
	}
	return hex.EncodeToString(HMACSHA256(c.Key, stringToSign+digestHex))
}

// Next signs the next link of the chain and returns its signature, which
// the chain goes on from.
func (c *Chain) Next(algorithm, digestHex string) string {
	c.Previous = c.link(algorithm, digestHex)
	return c.Previous
}

// Verify reports whether declared is the signature of the next link,
// compared in constant time. The chain goes on from it only when it is.
func (c *Chain) Verify(algorithm, declared, digestHex string) bool {
	want := c.link(algorithm, digestHex)
	if !iam.SecretsEqual([]byte(want), []byte(declared)) {
		return false
	}
	c.Previous = want
	return true
}
