package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// MaxTargetSecretLength is the longest secret a target is given: a bearer
// token, a password, a token of NATS or the secret key of another
// deployment.
const MaxTargetSecretLength = 4096

// targetsResource is what the list of targets is, to the authorizer:
// arn:cactusfs:admin:::targets.
const targetsResource = "targets"

// TargetService holds the use cases that define the places outside the
// deployment that it sends to (ADR-0023, "Targets"). Every one of them
// passes the authorizer first; a change carries its audit record in the
// entry that makes it.
//
// What a target holds that may be a credential - its secret, the key
// requests to a webhook are signed with, the path of its endpoint - is
// sealed through the KMS here, before the target is proposed, each under
// additional data of its own that names the target and the field. Nothing
// of it is returned, logged, audited or put in an error: a target is shown
// as delivery.ShownTarget, and a signing key that is made is returned once,
// to the caller that made it.
type TargetService struct {
	store   ports.TargetStore
	authz   ports.Authorizer
	auditor *Auditor
	kms     ports.KMS
	clock   ports.Clock
	// names orders the changes of one target made through this service.
	names nameLocks

	entropyMu sync.Mutex
	entropy   io.Reader
}

// TargetServiceConfig lists what a TargetService depends on.
type TargetServiceConfig struct {
	Store ports.TargetStore
	Authz ports.Authorizer
	// Auditor builds the records of changes.
	Auditor *Auditor
	KMS     ports.KMS
	Clock   ports.Clock
	// Entropy must be a cryptographically secure random source.
	Entropy io.Reader
}

// NewTargetService returns a TargetService.
func NewTargetService(cfg TargetServiceConfig) *TargetService {
	return &TargetService{
		store: cfg.Store, authz: cfg.Authz, auditor: cfg.Auditor, kms: cfg.KMS, clock: cfg.Clock, entropy: cfg.Entropy,
	}
}

// TargetInput is what is said of a target when it is defined or changed.
// Everything is replaced as it is given but for what is written and never
// read, which a change without it keeps: the endpoint, and the secret.
type TargetInput struct {
	Name string
	Kind delivery.Kind
	// Endpoint is the whole URL, path and all. Empty keeps the endpoint
	// of a target that exists; a target that is new needs one.
	Endpoint string
	Region   string
	Subject  string
	CAPEM    []byte
	// Buckets are the buckets whose rules may name the target. Nil is an
	// empty list: none may.
	Buckets []string
	Audit   bool
	// Auth is how the deployment proves who it is to the target. A mode
	// that is not said is delivery.AuthNone.
	Auth delivery.Auth
	// Secret is the secret of Auth. Empty keeps the secret of a target
	// that exists, as long as Auth.Mode stays what it was; the mode
	// delivery.AuthNone has none.
	Secret string
	// RotateSigningKey asks for a new key that requests to a webhook are
	// signed with, in place of the one it has.
	RotateSigningKey bool
}

// TargetPutResult is the answer to a definition or a change of a target.
type TargetPutResult struct {
	// Target is the target as it may be shown.
	Target delivery.ShownTarget
	// SigningKey is the key requests to the webhook are signed with, when
	// this call made one: the target is new, or a new key was asked for.
	// It is in this answer and nowhere else, ever.
	SigningKey string
}

// allow asks the authorizer and, when it agrees, returns the audit record of
// the action as carried out. resource is "targets" or "target/<name>".
func (s *TargetService) allow(ctx context.Context, p iam.Principal, action iam.Action, resource string) (audit.Record, error) {
	r := iam.Resource{Admin: resource}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: p, Action: action, Resource: r}); err != nil {
		return audit.Record{}, err
	}
	return s.auditor.Record(p, action, r, audit.Succeeded, nil), nil
}

// List returns every target in name order, as it may be shown.
func (s *TargetService) List(ctx context.Context, p iam.Principal) ([]delivery.ShownTarget, error) {
	if _, err := s.allow(ctx, p, iam.ActionListTargets, targetsResource); err != nil {
		return nil, fmt.Errorf("targets.List: %w", err)
	}
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		return nil, fmt.Errorf("targets.List: %w", err)
	}
	out := make([]delivery.ShownTarget, len(targets))
	for i, t := range targets {
		out[i] = shownOf(t)
	}
	return out, nil
}

// Get returns a target as it may be shown: without a secret, of which it
// says whether there is one, and without the path of its endpoint.
func (s *TargetService) Get(ctx context.Context, p iam.Principal, name string) (delivery.ShownTarget, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetTarget, "target/"+name); err != nil {
		return delivery.ShownTarget{}, fmt.Errorf("targets.Get: %w", err)
	}
	t, err := s.store.GetTarget(ctx, name)
	if err != nil {
		return delivery.ShownTarget{}, fmt.Errorf("targets.Get: %w", err)
	}
	return shownOf(t), nil
}

// Delete removes a target. The error wraps domain.ErrEntityNotFound, and
// domain.ErrEntityInUse while a bucket's configuration names the target.
func (s *TargetService) Delete(ctx context.Context, p iam.Principal, name string) error {
	record, err := s.allow(ctx, p, iam.ActionDeleteTarget, "target/"+name)
	if err == nil {
		var unlock func()
		if unlock, err = s.names.lock(ctx, name); err == nil {
			err = s.store.DeleteTarget(ctx, name, record)
			unlock()
		}
	}
	if err != nil {
		return fmt.Errorf("targets.Delete: %w", err)
	}
	return nil
}

// Put defines a target, or changes one, and returns it as it may be shown -
// with the signing key of a webhook when the call made one.
//
// The endpoint is judged by delivery.CheckEndpoint and the certificates by
// delivery.CheckCA, and the whole target by delivery.Target.Validate, before
// anything is sealed: a request that is refused costs the KMS nothing. The
// endpoint is cut into its origin, kept in the clear, and its path, sealed.
// The kind of a target cannot be changed (domain.ErrConflict): what was
// matched for one kind is not to be handed to another, and the target is
// deleted and defined again. The error wraps domain.ErrInvalidArgument for a
// target that cannot be kept, domain.ErrEntityInUse for a change the store
// refuses because bucket configurations name the target, and
// domain.ErrUnavailable until every member of the metadata group knows
// targets.
//
// What a change keeps - the secret, the endpoint, the signing key - is read
// from the store and proposed again. Changes and deletions of one name made
// through this service are carried out one at a time, so that two of them
// cannot interleave between the reading and the proposal: a change that
// keeps the signing key cannot undo a rotation made through the same
// gateway. Across gateways nothing orders them: the last change committed
// wins, whole, and may put back what another gateway's change replaced a
// moment before.
//
// A call whose outcome is not known (domain.ErrOutcomeUnknown) may have
// made a signing key that nobody was shown: it is replaced by asking for a
// new one.
func (s *TargetService) Put(ctx context.Context, p iam.Principal, in TargetInput) (TargetPutResult, error) {
	record, err := s.allow(ctx, p, iam.ActionPutTarget, "target/"+in.Name)
	if err != nil {
		return TargetPutResult{}, fmt.Errorf("targets.Put: %w", err)
	}
	unlock, err := s.names.lock(ctx, in.Name)
	if err != nil {
		return TargetPutResult{}, fmt.Errorf("targets.Put: %w", err)
	}
	defer unlock()
	out, err := s.put(ctx, in, record)
	if err != nil {
		return TargetPutResult{}, fmt.Errorf("targets.Put: %w", err)
	}
	return out, nil
}

// presence stands, in the draft of a target that is validated before
// anything is sealed, for a sealed field that will be there. It holds
// nothing: the draft is judged by whether its fields are there, as
// Validate judges every sealed field, and is never stored.
func presence() iam.SealedSecret {
	return iam.SealedSecret{Ciphertext: []byte{0}}
}

func (s *TargetService) put(ctx context.Context, in TargetInput, record audit.Record) (TargetPutResult, error) {
	if err := delivery.ValidateTargetName(in.Name); err != nil {
		return TargetPutResult{}, err
	}
	if !in.Kind.Valid() {
		return TargetPutResult{}, fmt.Errorf("%w: a target is of kind webhook, nats or s3", domain.ErrInvalidArgument)
	}
	existing, err := s.store.GetTarget(ctx, in.Name)
	exists := err == nil
	if err != nil && !errors.Is(err, domain.ErrEntityNotFound) {
		return TargetPutResult{}, err
	}
	if exists && existing.Kind != in.Kind {
		return TargetPutResult{}, fmt.Errorf("%w: target %s is a %s target; the kind of a target cannot be changed - delete it and define it again",
			domain.ErrConflict, in.Name, existing.Kind)
	}
	if err := delivery.CheckCA(in.CAPEM); err != nil {
		return TargetPutResult{}, err
	}
	mode := in.Auth.Mode
	if mode == "" {
		mode = delivery.AuthNone
	}
	if len(in.Secret) > MaxTargetSecretLength {
		return TargetPutResult{}, fmt.Errorf("%w: a secret of a target is at most %d bytes", domain.ErrInvalidArgument, MaxTargetSecretLength)
	}
	if in.RotateSigningKey && in.Kind != delivery.KindWebhook {
		return TargetPutResult{}, fmt.Errorf("%w: only requests to a webhook are signed with a key of the target", domain.ErrInvalidArgument)
	}
	now := s.clock.Now().UTC()
	t := delivery.Target{
		Name: in.Name, Kind: in.Kind, Region: in.Region, Subject: in.Subject, CAPEM: in.CAPEM, Buckets: in.Buckets,
		Audit: in.Audit, Auth: delivery.Auth{Mode: mode, ID: in.Auth.ID}, CreatedAt: now, UpdatedAt: now,
	}
	if t.Buckets == nil {
		t.Buckets = []string{}
	}
	if exists {
		t.CreatedAt, t.Refs = existing.CreatedAt, existing.Refs
	}

	// What is told of the change in the audit trail: what was done to the
	// endpoint and to the credentials, and never what they are.
	details := map[string]string{"kind": string(in.Kind), "created": strconv.FormatBool(!exists)}

	// First everything is decided, and nothing sealed. The endpoint:
	// judged, then cut into the origin, in the clear, and the path, to be
	// sealed. None given keeps the one the target has.
	var path string
	switch {
	case in.Endpoint != "":
		if _, err := delivery.CheckEndpoint(in.Kind, in.Endpoint); err != nil {
			return TargetPutResult{}, err
		}
		t.Endpoint, path = delivery.SplitEndpoint(in.Endpoint)
		if path == "/" {
			path = ""
		}
		details["endpoint"] = "set"
	case exists:
		t.Endpoint, t.SealedPath = existing.Endpoint, existing.SealedPath
		details["endpoint"] = "kept"
	default:
		return TargetPutResult{}, fmt.Errorf("%w: a target that is new needs an endpoint", domain.ErrInvalidArgument)
	}
	details["origin"] = t.Origin()

	// The secret: one that is given is to be sealed; none given keeps the
	// one the target has while the way it is used stays the same.
	switch {
	case in.Secret != "":
		details["secret"] = "set"
	case mode == delivery.AuthNone:
		details["secret"] = "none"
	case exists && existing.Auth.Mode == mode && existing.SecretSet():
		t.Secret = existing.Secret
		details["secret"] = "kept"
	default:
		return TargetPutResult{}, fmt.Errorf("%w: auth mode %s needs a secret, and there is none to keep", domain.ErrInvalidArgument, mode)
	}

	// The signing key of a webhook: made with the target, and when a new
	// one is asked for; otherwise kept.
	makeKey := in.Kind == delivery.KindWebhook && (!exists || in.RotateSigningKey || !existing.SignKeySet())
	switch {
	case makeKey:
		details["signing_key"] = "made"
	case in.Kind == delivery.KindWebhook:
		t.SignKey = existing.SignKey
		details["signing_key"] = "kept"
	}

	// The draft, with what is to be sealed standing in by its presence, is
	// judged before the KMS is asked for anything.
	draft := t
	if path != "" {
		draft.SealedPath = presence()
	}
	if in.Secret != "" {
		draft.Secret = presence()
	}
	if makeKey {
		draft.SignKey = presence()
	}
	if err := draft.Validate(); err != nil {
		return TargetPutResult{}, err
	}

	// Then what is to be sealed is sealed.
	if path != "" {
		if t.SealedPath, err = s.seal(ctx, []byte(path), delivery.PathAAD(in.Name)); err != nil {
			return TargetPutResult{}, err
		}
	}
	if in.Secret != "" {
		if t.Secret, err = s.seal(ctx, []byte(in.Secret), delivery.SecretAAD(in.Name)); err != nil {
			return TargetPutResult{}, err
		}
	}
	var signingKey string
	if makeKey {
		s.entropyMu.Lock()
		signingKey, err = iam.GenerateSecret(s.entropy)
		s.entropyMu.Unlock()
		if err != nil {
			return TargetPutResult{}, err
		}
		if t.SignKey, err = s.seal(ctx, []byte(signingKey), delivery.SignKeyAAD(in.Name)); err != nil {
			return TargetPutResult{}, err
		}
	}
	if err := t.Validate(); err != nil {
		return TargetPutResult{}, err
	}
	record.Details = boundAll(details)
	if err := s.store.PutTarget(ctx, t, record); err != nil {
		return TargetPutResult{}, err
	}
	// The answer is made from what was proposed, and not read back: a
	// reading that failed now would lose the signing key, which cannot be
	// had again. The count of references is the one that was read.
	return TargetPutResult{Target: shownOf(t), SigningKey: signingKey}, nil
}

// nameLocks orders the changes of one name: a holder of a name's lock reads
// what it keeps and proposes the change before the next holder reads. It is
// held by the service, and so orders what passes through one gateway.
type nameLocks struct {
	mu   sync.Mutex
	held map[string]*nameLock
}

// nameLock is the lock of one name: a channel of one slot, so that a caller
// can stop waiting when its context ends, and the number of callers that
// hold or wait for it, so that the lock goes once nobody does.
type nameLock struct {
	slot  chan struct{}
	users int
}

// lock waits until the name is free or ctx ends, and returns what frees it.
func (l *nameLocks) lock(ctx context.Context, name string) (func(), error) {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]*nameLock{}
	}
	entry := l.held[name]
	if entry == nil {
		entry = &nameLock{slot: make(chan struct{}, 1)}
		l.held[name] = entry
	}
	entry.users++
	l.mu.Unlock()
	leave := func() {
		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.held, name)
		}
		l.mu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
		return func() {
			<-entry.slot
			leave()
		}, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

// seal encrypts a value of a target under a data key of its own, wrapped by
// the KMS key, and bound by aad to the target and the field it belongs to
// (ADR-0023: as the secrets of API keys are sealed).
func (s *TargetService) seal(ctx context.Context, value, aad []byte) (iam.SealedSecret, error) {
	key, err := s.kms.GenerateDataKey(ctx, aad)
	if err != nil {
		return iam.SealedSecret{}, err
	}
	defer clear(key.Plaintext)
	s.entropyMu.Lock()
	sealed, err := iam.Seal(key.Plaintext, s.entropy, value, aad)
	s.entropyMu.Unlock()
	if err != nil {
		return iam.SealedSecret{}, err
	}
	sealed.KMSKeyID, sealed.WrappedKey = s.kms.KeyID(), key.Wrapped
	return sealed, nil
}

// shownOf returns a target as it may be shown, with a way of proving itself
// that is always named.
func shownOf(t delivery.Target) delivery.ShownTarget {
	shown := t.Shown()
	if shown.Auth.Mode == "" {
		shown.Auth.Mode = delivery.AuthNone
	}
	return shown
}
