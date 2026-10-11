package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

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
	// keys resolves API keys; nil until WithIAM, when only the root
	// credential is known.
	keys    ports.IAMStore
	auditor *Auditor
	// root is the root credential once it has been read. It is written
	// once, when the deployment is bootstrapped, and never changes.
	rootMu sync.Mutex
	root   *iam.RootCredential
	// createdRoot says that this service stored the root credential: the
	// deployment is new, and this node is the one that began it.
	createdRoot atomic.Bool
	// touched is when each key's use was last recorded; touches carries
	// the ones that are due to the goroutine that records them.
	touchMu sync.Mutex
	touched map[string]time.Time
	touches chan touch

	entropyMu sync.Mutex
	entropy   io.Reader
}

// NewIdentityService returns an IdentityService. entropy must be a
// cryptographically secure random source.
func NewIdentityService(roots ports.RootCredentialStore, kms ports.KMS, clock ports.Clock, entropy io.Reader) *IdentityService {
	return &IdentityService{
		roots: roots, kms: kms, clock: clock, entropy: entropy,
		touched: map[string]time.Time{}, touches: make(chan touch, touchQueueSize),
	}
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
	if err == nil {
		s.createdRoot.Store(true)
	}
	if err == nil && s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.System("system:BootstrapRoot", "", map[string]string{
			"access_key_id": creds.AccessKeyID, "generated": strconv.FormatBool(generated != nil),
		}))
	}
	if errors.Is(err, domain.ErrOutcomeUnknown) {
		// The credential may have been stored. If it was, it must be shown
		// now: there is no second chance to show it.
		if stored, readErr := s.roots.GetRootCredential(ctx); readErr == nil && stored.AccessKeyID == creds.AccessKeyID {
			// A credential generated here and found stored was stored
			// by this node. One that was supplied may have been stored
			// by another node that was given the same: this node then
			// cannot say that it began the deployment, and does not.
			if generated != nil {
				s.createdRoot.Store(true)
			}
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

// CreatedRoot reports whether EnsureRoot stored the root credential: the
// deployment is new and this node began it. What is done once for a new
// deployment is done by the node for which this is true.
func (s *IdentityService) CreatedRoot() bool {
	return s.createdRoot.Load()
}

func (s *IdentityService) checkStoredRoot(ctx context.Context, stored iam.RootCredential, provided *Credentials) error {
	secret, err := s.open(ctx, stored.AccessKeyID, stored.Secret)
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

// open unseals the secret of an access key.
func (s *IdentityService) open(ctx context.Context, accessKeyID string, sealed iam.SealedSecret) ([]byte, error) {
	if sealed.KMSKeyID != s.kms.KeyID() {
		return nil, fmt.Errorf("%w: sealed under KMS key %s, this node uses %s",
			domain.ErrKeyUnavailable, sealed.KMSKeyID, s.kms.KeyID())
	}
	aad := []byte(accessKeyID)
	key, err := s.kms.UnwrapDataKey(ctx, sealed.WrappedKey, aad)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return sealed.Open(key, aad)
}

type touch struct {
	accessKeyID string
	at          time.Time
}

const (
	// touchInterval is how precisely a key's last use is recorded: at most
	// one write per key in this time.
	touchInterval = 5 * time.Minute
	// touchQueueSize bounds the last-use records that wait to be written.
	touchQueueSize = 256
	touchTimeout   = 30 * time.Second
)

// WithIAM makes the service know API keys, and record failed
// authentications. It is called once, when the node is assembled.
func (s *IdentityService) WithIAM(keys ports.IAMStore, auditor *Auditor) *IdentityService {
	s.keys, s.auditor = keys, auditor
	return s
}

// Run records when API keys were last used, until ctx is cancelled.
func (s *IdentityService) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-s.touches:
			writeCtx, cancel := context.WithTimeout(ctx, touchTimeout)
			// Best effort: the next use of the key records it again.
			_ = s.keys.TouchAPIKey(writeCtx, t.accessKeyID, t.at)
			cancel()
		}
	}
}

// used notes that a key authenticated a request. The time is written to the
// replicated state at most once per touchInterval and key, and never on the
// request's own time.
func (s *IdentityService) used(accessKeyID string, now time.Time) {
	s.touchMu.Lock()
	defer s.touchMu.Unlock()
	if last, ok := s.touched[accessKeyID]; ok && now.Sub(last) < touchInterval {
		return
	}
	select {
	case s.touches <- touch{accessKeyID: accessKeyID, at: now}:
		s.touched[accessKeyID] = now
	default:
	}
}

// rootCredential returns the root credential, reading it once.
func (s *IdentityService) rootCredential(ctx context.Context) (iam.RootCredential, error) {
	s.rootMu.Lock()
	defer s.rootMu.Unlock()
	if s.root != nil {
		return *s.root, nil
	}
	root, err := s.roots.GetRootCredential(ctx)
	if err != nil {
		return iam.RootCredential{}, err
	}
	s.root = &root
	return root, nil
}

// SecretFor returns the secret key of an access key, which an inbound adapter
// needs to verify a request signature, and the principal the key stands for:
// the root identity, or a user with every policy that applies, resolved at
// this moment. The caller must not log the secret and should clear it after
// use.
//
// The error wraps domain.ErrCredentialNotFound for an access key that does
// not exist, has expired or belongs to a user who is disabled. A key that
// was revoked does not exist: revocation takes effect with the next request,
// on every node.
func (s *IdentityService) SecretFor(ctx context.Context, accessKeyID string) ([]byte, iam.Principal, error) {
	root, err := s.rootCredential(ctx)
	if err != nil {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", err)
	}
	if iam.SecretsEqual([]byte(accessKeyID), []byte(root.AccessKeyID)) {
		secret, err := s.open(ctx, root.AccessKeyID, root.Secret)
		if err != nil {
			return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", err)
		}
		return secret, iam.Principal{AccessKeyID: root.AccessKeyID, Root: true}, nil
	}
	if s.keys == nil || iam.ValidateAccessKeyID(accessKeyID) != nil {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", domain.ErrCredentialNotFound)
	}
	identity, err := s.keys.ResolveIdentity(ctx, accessKeyID)
	if err != nil {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", err)
	}
	now := s.clock.Now()
	switch {
	case identity.Key.Expired(now):
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w: the key has expired", domain.ErrCredentialNotFound)
	case identity.User.Disabled:
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w: the key's owner is disabled", domain.ErrCredentialNotFound)
	}
	secret, err := s.open(ctx, identity.Key.AccessKeyID, identity.Key.Secret)
	if err != nil {
		return nil, iam.Principal{}, fmt.Errorf("identity.SecretFor: %w", err)
	}
	return secret, iam.Principal{
		AccessKeyID: identity.Key.AccessKeyID, User: identity.User.Name,
		Policies: identity.Policies, Boundary: identity.Key.InlinePolicy,
	}, nil
}

// Authenticated is told by the inbound adapter that a request really was
// made with the secret of the principal's key: its signature verified. Only
// then does the key count as used - knowing a key's ID is not using it.
func (s *IdentityService) Authenticated(p iam.Principal) {
	if p.User != "" && p.AccessKeyID != "" {
		s.used(p.AccessKeyID, s.clock.Now())
	}
}

// AuthenticationFailed records that a request claiming an access key was
// not accepted: the key is unknown, or the signature does not verify. The
// inbound adapter calls it; the reason never contains a secret.
func (s *IdentityService) AuthenticationFailed(ctx context.Context, accessKeyID string, session iam.Session, reason string) {
	if s.auditor != nil {
		s.auditor.AuthenticationFailed(ctx, "auth:Authenticate", accessKeyID, session, reason)
	}
}
