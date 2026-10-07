package ports

import "context"

// DataKey is a freshly generated data key in both of its forms.
type DataKey struct {
	// Plaintext is the 32-byte key to encrypt with. It is never stored.
	Plaintext []byte
	// Wrapped is the key encrypted under the KMS key. It is stored next to
	// the data it protects.
	Wrapped []byte
}

// KMS is the key management service that wraps data keys (envelope
// encryption). There is no implementation that stores keys unencrypted.
type KMS interface {
	// KeyID identifies the KMS key in use. It is recorded with every wrapped
	// key so that a mismatch is detected rather than guessed at.
	KeyID() string
	// GenerateDataKey returns a new random data key. aad binds the wrapped
	// key to its owner; the same aad must be given to UnwrapDataKey.
	GenerateDataKey(ctx context.Context, aad []byte) (DataKey, error)
	// UnwrapDataKey returns the plaintext of a wrapped data key. The error
	// wraps domain.ErrKeyUnavailable when the key cannot be unwrapped.
	UnwrapDataKey(ctx context.Context, wrapped, aad []byte) ([]byte, error)
}
