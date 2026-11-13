package local

import (
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
)

// UnwrapAsAVersion1Node opens a wrapped key as the release before version 2
// did (ADR-0031): version 1 only, anything else refused as malformed. It
// stands for a node of that release sharing the key file mid-upgrade.
func (k *KMS) UnwrapAsAVersion1Node(wrapped, aad []byte) ([]byte, error) {
	nonceSize := k.aead.NonceSize()
	if len(wrapped) < 1+nonceSize || wrapped[0] != wrapVersion1 {
		return nil, fmt.Errorf("%w: malformed wrapped key", domain.ErrKeyUnavailable)
	}
	nonce, ciphertext := wrapped[1:1+nonceSize], wrapped[1+nonceSize:]
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, k.wrapAAD(aad))
	if err != nil || len(plaintext) != keySize {
		return nil, fmt.Errorf("%w: key was not wrapped for this owner", domain.ErrKeyUnavailable)
	}
	return plaintext, nil
}
