package object

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
)

// How an object's data key is managed, as S3 names it. Every object is
// encrypted; the mode says whose key it takes to read it.
const (
	// SSES3 is the default: a data key from the deployment's KMS.
	SSES3 = ""
	// SSEKMS is the same key hierarchy, asked for by name, with an
	// encryption context bound to the wrapped key.
	SSEKMS = "aws:kms"
	// SSEC adds a key the client holds: the object is encrypted under a
	// key derived from the KMS data key and the client's key together, and
	// the client's key is never stored.
	SSEC = "sse-c"
)

// Limits of an SSE-KMS encryption context.
const (
	maxContextPairs = 16
	maxContextBytes = 2048
)

const (
	customerKeyInfo   = "cactusfs/sse-c/v1"
	customerCheckInfo = "cactusfs/sse-c/check/v1"
	customerTagInfo   = "cactusfs/sse-c/etag/v1"
	// keyCheckSize is the length of the value that tells a wrong customer
	// key from damaged data.
	keyCheckSize = 16
)

// SSE is what a request says about the encryption of the object it writes.
type SSE struct {
	// Mode is SSES3, SSEKMS or SSEC.
	Mode string
	// KMSKeyID, with SSEKMS, names the KMS key the client expects. Empty
	// means the deployment's key, which is the only one there is.
	KMSKeyID string
	// Context, with SSEKMS, is the encryption context.
	Context map[string]string
	// CustomerKey, with SSEC, is the client's 256-bit key.
	CustomerKey []byte
}

// Validate reports whether the request is one that can be kept. The error
// wraps domain.ErrInvalidArgument.
func (s SSE) Validate() error {
	switch s.Mode {
	case SSES3:
		if s.KMSKeyID != "" || len(s.Context) != 0 || len(s.CustomerKey) != 0 {
			return fmt.Errorf("%w: a KMS key, an encryption context or a customer key without the encryption they belong to", domain.ErrInvalidArgument)
		}
	case SSEKMS:
		if len(s.CustomerKey) != 0 {
			return fmt.Errorf("%w: a customer key with SSE-KMS", domain.ErrInvalidArgument)
		}
		if len(s.Context) > maxContextPairs {
			return fmt.Errorf("%w: encryption context of more than %d pairs", domain.ErrInvalidArgument, maxContextPairs)
		}
		if raw, err := canonicalContext(s.Context); err != nil || len(raw) > maxContextBytes {
			return fmt.Errorf("%w: encryption context larger than %d bytes", domain.ErrInvalidArgument, maxContextBytes)
		}
	case SSEC:
		if len(s.CustomerKey) != KeySize {
			return fmt.Errorf("%w: a customer key is %d bytes", domain.ErrInvalidArgument, KeySize)
		}
		if s.KMSKeyID != "" || len(s.Context) != 0 {
			return fmt.Errorf("%w: a KMS key or an encryption context with SSE-C", domain.ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: server-side encryption %q", domain.ErrInvalidArgument, s.Mode)
	}
	return nil
}

// canonicalContext writes an encryption context in one way only: a JSON
// object with its keys sorted, which is what encoding/json makes of a map.
func canonicalContext(context map[string]string) ([]byte, error) {
	if len(context) == 0 {
		return nil, nil
	}
	return json.Marshal(context)
}

// Apply records the mode of a request in the encryption parameters of what
// is being written: the mode and the canonical encryption context. It
// records no key, and nothing a key could be guessed from.
func (s SSE) Apply(enc *Encryption) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, err := canonicalContext(s.Context)
	if err != nil {
		return fmt.Errorf("%w: encryption context: %w", domain.ErrInvalidArgument, err)
	}
	enc.Mode, enc.Context = s.Mode, raw
	return nil
}

// ContextMap returns the encryption context of stored data as it was given.
func (e Encryption) ContextMap() map[string]string {
	if len(e.Context) == 0 {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(e.Context, &out); err != nil {
		return nil
	}
	return out
}

// KeyContext returns the context the KMS binds a data key to: base - the ID
// of the data or the upload - and, for SSE-KMS and SSE-C, the mode and the
// encryption context. The default mode adds nothing, so that keys wrapped
// before there were modes still unwrap. A record whose mode or context was
// changed names another context, and its key does not unwrap.
func (e Encryption) KeyContext(base []byte) []byte {
	if e.Mode == SSES3 {
		return base
	}
	// Every piece is written with its length, so that no two different
	// triples of base, mode and context give the same bytes.
	out := fmt.Appendf(nil, "cactusfs/sse/v1/%d/%d/%d/", len(base), len(e.Mode), len(e.Context))
	out = append(out, base...)
	out = append(out, e.Mode...)
	return append(out, e.Context...)
}

// KnownMode reports whether mode is one this binary reads.
func KnownMode(mode string) bool {
	return mode == SSES3 || mode == SSEKMS || mode == SSEC
}

// CustomerDataKey returns the key that takes the place of the KMS data key
// for an object under SSE-C: HKDF-SHA256 over the client's key, salted with
// the KMS data key. It takes both to compute; neither alone gives it.
func CustomerDataKey(kmsKey, customerKey []byte) ([]byte, error) {
	if len(kmsKey) != KeySize || len(customerKey) != KeySize {
		return nil, fmt.Errorf("%w: keys of %d and %d bytes", domain.ErrInvalidArgument, len(kmsKey), len(customerKey))
	}
	key, err := hkdf.Key(sha256.New, customerKey, kmsKey, customerKeyInfo, KeySize)
	if err != nil {
		return nil, fmt.Errorf("derive customer data key: %w", err)
	}
	return key, nil
}

// KeyCheck returns the value that is stored to tell, on a later read, that
// the client's key is the one the object was written with. It is derived
// from the derived key for this purpose alone: it takes the KMS data key to
// test a guess of the client's key against it.
func KeyCheck(customerDataKey []byte) []byte {
	check, err := hkdf.Key(sha256.New, customerDataKey, nil, customerCheckInfo, keyCheckSize)
	if err != nil {
		// Only a length HKDF cannot give, which sixteen bytes is not.
		return nil
	}
	return check
}

// OpaqueTag turns an entity tag that is a digest of the plaintext into one
// that is not: an HMAC of it under a key derived from the object's derived
// key, as long as the tag it replaces. An object under SSE-C is listed with
// its entity tag to callers who do not hold its key, and stored with it in
// metadata the operator can read; a digest of the plaintext there would let
// either confirm a guess of the content.
func OpaqueTag(customerDataKey []byte, etag string) (string, error) {
	key, err := hkdf.Key(sha256.New, customerDataKey, nil, customerTagInfo, KeySize)
	if err != nil {
		return "", fmt.Errorf("derive tag key: %w", err)
	}
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(etag))
	return hex.EncodeToString(mac.Sum(nil)[:16]), nil
}

// VerifyKeyCheck reports whether a derived key is the one a stored check
// value was made from, in constant time. The error wraps
// domain.ErrCustomerKeyMismatch.
func VerifyKeyCheck(customerDataKey, check []byte) error {
	if expected := KeyCheck(customerDataKey); len(expected) == 0 || !hmac.Equal(expected, check) {
		return fmt.Errorf("%w", domain.ErrCustomerKeyMismatch)
	}
	return nil
}
