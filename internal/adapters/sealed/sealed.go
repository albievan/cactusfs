// Package sealed opens the fields a target holds sealed - its secret, its
// signing key, the path of its endpoint (ADR-0023) - for the adapters that
// connect to it: the webhook and the NATS publishers of ADR-0025, and the
// replicator of ADR-0026.
//
// A field is opened when it is needed and not kept: whoever opens one clears
// what Open returns as soon as it is in the request or the connection that
// needs it. Nothing here caches a plaintext or a data key.
//
// The secret of an API key is opened the same way by app.IdentityService;
// it keeps its own copy because a use case may not import an adapter.
package sealed

import (
	"context"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Opener opens what a target holds sealed.
type Opener interface {
	// Open returns the plaintext of field, which was sealed with aad.
	// The caller clears it when it is done with it.
	Open(ctx context.Context, field iam.SealedSecret, aad []byte) ([]byte, error)
}

// KMS opens a sealed field of a target through the KMS that sealed it, as
// the secrets of API keys are opened: the data key is unwrapped with the
// field's additional data, which binds it to the target and the field, and
// cleared once the field is open.
type KMS struct {
	// KMS is the node's KMS.
	KMS ports.KMS
}

var _ Opener = KMS{}

// Open implements Opener. A field sealed under another KMS key than the
// node's, and a KMS that is not there, wrap domain.ErrKeyUnavailable; a
// field that does not open with aad wraps domain.ErrIntegrity. No error
// holds anything of the plaintext.
func (k KMS) Open(ctx context.Context, field iam.SealedSecret, aad []byte) ([]byte, error) {
	if k.KMS == nil {
		return nil, fmt.Errorf("sealed.Open: %w: no KMS", domain.ErrKeyUnavailable)
	}
	if field.KMSKeyID != k.KMS.KeyID() {
		return nil, fmt.Errorf("sealed.Open: %w: sealed under another KMS key", domain.ErrKeyUnavailable)
	}
	key, err := k.KMS.UnwrapDataKey(ctx, field.WrappedKey, aad)
	if err != nil {
		return nil, fmt.Errorf("sealed.Open: %w", err)
	}
	defer clear(key)
	plain, err := field.Open(key, aad)
	if err != nil {
		return nil, fmt.Errorf("sealed.Open: %w", err)
	}
	return plain, nil
}
