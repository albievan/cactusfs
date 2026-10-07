package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Credentials is an API key in the clear. It exists only in memory, on the
// way in from the operator or on the way out to be shown once.
type Credentials struct {
	AccessKeyID string
	SecretKey   string
}

// IdentityService bootstraps the deployment's root credential and resolves
// access keys for authentication.
type IdentityService struct {
	roots ports.RootCredentialStore
	kms   ports.KMS
	clock ports.Clock

	entropyMu sync.Mutex
	entropy   io.Reader
}

// NewIdentityService returns an IdentityService. entropy must be a
// cryptographically secure random source.
func NewIdentityService(roots ports.RootCredentialStore, kms ports.KMS, clock ports.Clock, entropy io.Reader) *IdentityService {
	return &IdentityService{roots: roots, kms: kms, clock: clock, entropy: entropy}
}

// EnsureRoot makes sure the deployment has a root credential. There is no
// built-in one.
//
// On a new deployment it stores provided, or generates a credential when
// provided is nil and returns it so that it can be shown to the operator
// once: the secret is stored sealed and cannot be shown again.
//
// On an existing deployment it proves that this node's KMS can open the
// stored secret, and, when provided is not nil, that provided matches it. A
// mismatch is an error rather than a silent choice of one over the other.
func (s *IdentityService) EnsureRoot(ctx context.Context, provided *Credentials) (generated *Credentials, err error) {
	stored, err := s.roots.GetRootCredential(ctx)
	switch {
	case err == nil:
		return nil, s.checkStoredRoot(ctx, stored, provided)
	case !errors.Is(err, domain.ErrNotBootstrapped):
		return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
	}

	creds := provided
	if creds == nil {
		if creds, err = s.generate(); err != nil {
			return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
		}
		generated = creds
	}
	if err := iam.ValidateAccessKeyID(creds.AccessKeyID); err != nil {
		return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
	}
	if err := iam.ValidateSecret(creds.SecretKey); err != nil {
		return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
	}

	sealed, err := s.seal(ctx, *creds)
	if err != nil {
		return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
	}
	root := iam.RootCredential{AccessKeyID: creds.AccessKeyID, Secret: sealed, CreatedAt: s.clock.Now().UTC()}
	err = s.roots.SetRootCredential(ctx, root)
	if errors.Is(err, domain.ErrOutcomeUnknown) {
		// The credential may have been stored. If it was, it must be shown
		// now: there is no second chance to show it.
		if stored, readErr := s.roots.GetRootCredential(ctx); readErr == nil && stored.AccessKeyID == creds.AccessKeyID {
			return generated, s.checkStoredRoot(ctx, stored, creds)
		}
	}
	if errors.Is(err, domain.ErrAlreadyBootstrapped) {
		// Another node of the cluster was first. Its credential is the
		// deployment's; one generated here was never stored and is not
		// shown to anybody.
		stored, err := s.roots.GetRootCredential(ctx)
		if err != nil {
			return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
		}
		return nil, s.checkStoredRoot(ctx, stored, provided)
	}
	if err != nil {
		return nil, fmt.Errorf("identity.EnsureRoot: %w", err)
	}
	return generated, nil
}

func (s *IdentityService) checkStoredRoot(ctx context.Context, stored iam.RootCredential, provided *Credentials) error {
	secret, err := s.open(ctx, stored)
	if err != nil {
		return fmt.Errorf("identity.EnsureRoot: stored root credential: %w", err)
	}
	defer clear(secret)
	if provided == nil {
		return nil
	}
	sameID := iam.SecretsEqual([]byte(provided.AccessKeyID), []byte(stored.AccessKeyID))
	sameSecret := iam.SecretsEqual([]byte(provided.SecretKey), secret)
	if !sameID || !sameSecret {
		return fmt.Errorf("identity.EnsureRoot: %w: the root credentials supplied differ from the ones this deployment "+
			"was bootstrapped with; remove them from the configuration or supply the original ones", domain.ErrInvalidArgument)
	}
	return nil
}

func (s *IdentityService) generate() (*Credentials, error) {
	s.entropyMu.Lock()
	defer s.entropyMu.Unlock()
	id, err := iam.GenerateAccessKeyID(s.entropy)
	if err != nil {
		return nil, err
	}
	secret, err := iam.GenerateSecret(s.entropy)
	if err != nil {
		return nil, err
	}
	return &Credentials{AccessKeyID: id, SecretKey: secret}, nil
}

// seal encrypts a secret under a fresh data key from the KMS. The access key
// ID is the associated data of both layers, so a sealed secret cannot be
// attached to another key.
func (s *IdentityService) seal(ctx context.Context, creds Credentials) (iam.SealedSecret, error) {
	aad := []byte(creds.AccessKeyID)
	key, err := s.kms.GenerateDataKey(ctx, aad)
	if err != nil {
		return iam.SealedSecret{}, err
	}
	defer clear(key.Plaintext)

	s.entropyMu.Lock()
	sealed, err := iam.Seal(key.Plaintext, s.entropy, []byte(creds.SecretKey), aad)
	s.entropyMu.Unlock()
	if err != nil {
		return iam.SealedSecret{}, err
	}
	sealed.KMSKeyID = s.kms.KeyID()
	sealed.WrappedKey = key.Wrapped
	return sealed, nil
}

func (s *IdentityService) open(ctx context.Context, root iam.RootCredential) ([]byte, error) {
	if root.Secret.KMSKeyID != s.kms.KeyID() {
		return nil, fmt.Errorf("%w: sealed under KMS key %s, this node uses %s",
			domain.ErrKeyUnavailable, root.Secret.KMSKeyID, s.kms.KeyID())
	}
	aad := []byte(root.AccessKeyID)
	key, err := s.kms.UnwrapDataKey(ctx, root.Secret.WrappedKey, aad)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return root.Secret.Open(key, aad)
}

// SecretFor returns the secret key of an access key, which an inbound adapter
// needs to verify a request signature, and the principal the key stands for.
// The caller must not log the secret and should clear it after use.
//
// The error wraps domain.ErrCredentialNotFound for an unknown access key.
func (s *IdentityService) SecretFor(ctx context.Context, accessKeyID string) ([]byte, iam.Principal, error) {
	root, err := s.roots.GetRootCredential(ctx)
	if err != nil {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", err)
	}
	if !iam.SecretsEqual([]byte(accessKeyID), []byte(root.AccessKeyID)) {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", domain.ErrCredentialNotFound)
	}
	secret, err := s.open(ctx, root)
	if err != nil {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", err)
	}
	return secret, iam.Principal{AccessKeyID: root.AccessKeyID, Root: true}, nil
}
