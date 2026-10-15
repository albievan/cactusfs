package sigv4

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// errSign is what every refusal of SignRequest wraps.
var errSign = errors.New("cannot sign the request")

// SignRequest signs r for an S3 endpoint with SigV4 in the Authorization
// header, as of now, with the key accessKeyID and its secret, for region.
// payloadHash is the payload declaration: the SHA-256 of the body in
// hexadecimal, or UnsignedPayload over TLS.
//
// It sets X-Amz-Date, X-Amz-Content-Sha256 and Authorization, and signs the
// host, Content-Type and Content-MD5 when they are there, and every x-amz-*
// header - what a server that verifies requires to be signed and what it
// receives as it was sent. r.Host is set to the host of the URL when it is
// empty, an empty path to "/", and r.URL.RawPath to the path as the
// canonical request encodes it, so that the bytes sent are the bytes signed.
// Headers added to r after it is signed are not covered; an x-amz-* header
// added then fails verification.
//
// The secret is not kept, and is in no error.
func SignRequest(r *http.Request, accessKeyID string, secret []byte, region, payloadHash string, now time.Time) error {
	switch {
	case r == nil || r.URL == nil:
		return fmt.Errorf("sigv4.SignRequest: %w: no request", errSign)
	case r.URL.Opaque != "":
		return fmt.Errorf("sigv4.SignRequest: %w: an opaque URL", errSign)
	case !credentialWord(accessKeyID):
		return fmt.Errorf("sigv4.SignRequest: %w: the access key ID", errSign)
	case len(secret) == 0:
		return fmt.Errorf("sigv4.SignRequest: %w: no secret", errSign)
	case !credentialWord(region):
		return fmt.Errorf("sigv4.SignRequest: %w: the region", errSign)
	case !credentialWord(payloadHash):
		return fmt.Errorf("sigv4.SignRequest: %w: the payload declaration", errSign)
	}
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	if r.Host == "" {
		return fmt.Errorf("sigv4.SignRequest: %w: no host", errSign)
	}
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	// The path is sent as it is signed: encoded as the canonical request
	// encodes it, every byte but the unreserved ones and the slash.
	r.URL.RawPath = URIEncode(r.URL.Path, false)
	if r.Header == nil {
		r.Header = http.Header{}
	}
	now = now.UTC()
	timestamp, date := now.Format(AmzDateFormat), now.Format(ScopeDateFormat)
	r.Header.Del("Authorization")
	r.Header.Set(HeaderAmzDate, timestamp)
	r.Header.Set(HeaderContentSHA256, payloadHash)

	signed := []string{"host"}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || lower == "content-md5" || lower == "content-type" {
			if http.CanonicalHeaderKey(name) != name {
				// What net/http sends under a key it would not have
				// written is not what Values finds.
				return fmt.Errorf("sigv4.SignRequest: %w: header %q is not in canonical form", errSign, name)
			}
			signed = append(signed, lower)
		}
	}
	slices.Sort(signed)
	signed = slices.Compact(signed)
	signedRaw := strings.Join(signed, ";")

	signature := Expected(secret, date, region, timestamp, CanonicalRequest(r, signed, signedRaw, payloadHash))
	r.Header.Set("Authorization", Algorithm+" Credential="+accessKeyID+"/"+Scope(date, region)+
		", SignedHeaders="+signedRaw+", Signature="+signature)
	return nil
}

// credentialWord reports whether s can stand in the Authorization header as
// a part of the credential or as the payload declaration: printable ASCII
// without a space, a comma, a slash or an equals sign, which would change
// how the header is read.
func credentialWord(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' || c == ',' || c == '/' || c == '=' {
			return false
		}
	}
	return true
}
