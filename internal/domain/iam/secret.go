package iam

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"fmt"
	"io"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// Rules for API key material.
const (
	// MinAccessKeyIDLength and MaxAccessKeyIDLength bound an access key ID.
	MinAccessKeyIDLength = 3
	MaxAccessKeyIDLength = 128
	// MinSecretLength and MaxSecretLength bound a secret key.
	MinSecretLength = 16
	MaxSecretLength = 128

	generatedAccessKeyIDLength = 20
	generatedSecretLength      = 40
	sealKeySize                = 32
)

const (
	accessKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	secretAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

// ValidateAccessKeyID reports whether id may be used as an access key ID:
// letters, digits, hyphens and underscores only, so that it is safe in a
// SigV4 credential scope. The error wraps domain.ErrInvalidArgument.
func ValidateAccessKeyID(id string) error {
	if len(id) < MinAccessKeyIDLength || len(id) > MaxAccessKeyIDLength {
		return fmt.Errorf("%w: access key ID must be %d to %d characters",
			domain.ErrInvalidArgument, MinAccessKeyIDLength, MaxAccessKeyIDLength)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return fmt.Errorf("%w: access key ID may contain only letters, digits, '-' and '_'", domain.ErrInvalidArgument)
		}
	}
	return nil
}

// ValidateSecret reports whether secret is acceptable as a secret key. The
// error wraps domain.ErrInvalidArgument and never contains the secret.
func ValidateSecret(secret string) error {
	if len(secret) < MinSecretLength || len(secret) > MaxSecretLength {
		return fmt.Errorf("%w: secret key must be %d to %d characters",
			domain.ErrInvalidArgument, MinSecretLength, MaxSecretLength)
	}
	for i := 0; i < len(secret); i++ {
		if c := secret[i]; c < 0x21 || c > 0x7E {
			return fmt.Errorf("%w: secret key must be printable ASCII without spaces", domain.ErrInvalidArgument)
		}
	}
	return nil
}

// GenerateAccessKeyID returns a random access key ID read from random, which
// must be a cryptographically secure source.
func GenerateAccessKeyID(random io.Reader) (string, error) {
	return randomString(random, accessKeyAlphabet, generatedAccessKeyIDLength)
}

// GenerateSecret returns a random secret key with 240 bits of entropy, read
// from random, which must be a cryptographically secure source.
func GenerateSecret(random io.Reader) (string, error) {
	return randomString(random, secretAlphabet, generatedSecretLength)
}

// randomString draws length characters uniformly from alphabet, whose size
// must be a power of two so that masking introduces no bias.
func randomString(random io.Reader, alphabet string, length int) (string, error) {
	buf := make([]byte, length)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("iam: read randomness: %w", err)
	}
	var out strings.Builder
	out.Grow(length)
	for _, b := range buf {
		out.WriteByte(alphabet[int(b)&(len(alphabet)-1)])
	}
	return out.String(), nil
}

// SecretsEqual compares two secrets or signatures in constant time.
func SecretsEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// SealedSecret is a secret encrypted under a data key that is itself wrapped
// by a KMS key (envelope encryption).
type SealedSecret struct {
	// KMSKeyID identifies the KMS key that wraps the data key.
	KMSKeyID string
	// WrappedKey is the data key, wrapped by the KMS key.
	WrappedKey []byte
	// Nonce and Ciphertext are the AES-256-GCM encryption of the secret
	// under the data key.
	Nonce      []byte
	Ciphertext []byte
}

// Seal encrypts secret with AES-256-GCM under dataKey. aad ties the ciphertext
// to its owner, for example the access key ID. The caller fills in KMSKeyID
// and WrappedKey. A data key must be used for one Seal only.
func Seal(dataKey []byte, random io.Reader, secret, aad []byte) (SealedSecret, error) {
	aead, err := newAEAD(dataKey)
	if err != nil {
		return SealedSecret{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return SealedSecret{}, fmt.Errorf("iam.Seal: read randomness: %w", err)
	}
	return SealedSecret{Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, secret, aad)}, nil
}

// Open decrypts a sealed secret with its unwrapped data key. The error wraps
// domain.ErrIntegrity when the ciphertext, the key or aad do not match.
func (s SealedSecret) Open(dataKey, aad []byte) ([]byte, error) {
	aead, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}
	if len(s.Nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: sealed secret has a malformed nonce", domain.ErrIntegrity)
	}
	secret, err := aead.Open(nil, s.Nonce, s.Ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: sealed secret failed authentication", domain.ErrIntegrity)
	}
	return secret, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != sealKeySize {
		return nil, fmt.Errorf("%w: data key is %d bytes, want %d", domain.ErrInvalidArgument, len(key), sealKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("iam: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("iam: %w", err)
	}
	return aead, nil
}
