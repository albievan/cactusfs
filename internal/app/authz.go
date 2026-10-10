package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Authorizer is the single authorization decision point. It applies the
// domain's rules - default deny, explicit deny wins, a key never exceeds its
// owner - to the principal a request was authenticated as, and records every
// refusal in the audit trail and in the log. Neither ever contains a secret.
type Authorizer struct {
	logger  *slog.Logger
	clock   ports.Clock
	auditor *Auditor
	// policies reads the policy of a bucket; empty when there are none.
	// A node gives it to its authorizer once the metadata store is open,
	// by which time the authorizer already answers for node RPC.
	policies atomic.Pointer[BucketPolicySource]
}

// BucketPolicySource is where the authorizer reads the policy of a bucket.
// ports.BucketStore is one.
type BucketPolicySource interface {
	GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error)
}

// WithBucketPolicies makes the authorizer take the policy of the bucket a
// request is about into its decision. It may be called while the authorizer
// is in use.
func (a *Authorizer) WithBucketPolicies(source BucketPolicySource) *Authorizer {
	a.policies.Store(&source)
	return a
}

// bucketPolicy loads the policy of the bucket a request is about. Whoever is
// bound by no policy - the root identity, a node - is asked nothing about
// one. A policy that cannot be read is an error: the request is refused,
// because the policy might have denied it.
func (a *Authorizer) bucketPolicy(ctx context.Context, req iam.Request) (*iam.BucketPolicy, error) {
	source := a.policies.Load()
	if source == nil || req.Resource.Bucket == "" || req.Principal.Root || req.Principal.NodeID != "" ||
		!strings.HasPrefix(string(req.Action), "s3:") {
		return nil, nil
	}
	raw, err := (*source).GetBucketConfig(ctx, req.Resource.Bucket, bucket.ConfigPolicy)
	if errors.Is(err, domain.ErrBucketNotFound) || (err == nil && len(raw) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	policy, err := iam.ParseBucketPolicy(raw, req.Resource.Bucket)
	if err != nil {
		// The stored policy is damaged; that is no fault of the request.
		// Only what it says is kept, not what it is: a damaged record must
		// not read as an invalid argument of the caller's.
		return nil, fmt.Errorf("%w: stored bucket policy: %s", domain.ErrIntegrity, err.Error())
	}
	return &policy, nil
}

// requestScopeKey marks a context that carries what one request has already
// learned.
type requestScopeKey struct{}

// requestScope remembers the policies of the buckets one request has asked
// about. A request is decided against one reading of a bucket's policy: a
// DeleteObjects of a thousand keys reads it once, not a thousand times.
type requestScope struct {
	mu       sync.Mutex
	policies map[string]*iam.BucketPolicy
}

// WithRequestScope returns a context for the handling of one request. The
// authorizer reads the policy of a bucket once per such context. It must not
// outlive the request.
func WithRequestScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestScopeKey{}, &requestScope{policies: map[string]*iam.BucketPolicy{}})
}

// scopedPolicy is bucketPolicy, read once per request scope.
func (a *Authorizer) scopedPolicy(ctx context.Context, req iam.Request) (*iam.BucketPolicy, error) {
	scope, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	if scope == nil {
		return a.bucketPolicy(ctx, req)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if policy, ok := scope.policies[req.Resource.Bucket]; ok {
		return policy, nil
	}
	policy, err := a.bucketPolicy(ctx, req)
	if err != nil {
		return nil, err
	}
	if a.policies.Load() != nil && req.Resource.Bucket != "" {
		scope.policies[req.Resource.Bucket] = policy
	}
	return policy, nil
}

var _ ports.Authorizer = (*Authorizer)(nil)

// NewAuthorizer returns an Authorizer. The clock gives the time that
// conditions on time are tested against. auditor may be nil, in which case
// refusals are logged only.
func NewAuthorizer(logger *slog.Logger, clock ports.Clock, auditor *Auditor) *Authorizer {
	return &Authorizer{logger: logger, clock: clock, auditor: auditor}
}

// Authorize implements ports.Authorizer.
func (a *Authorizer) Authorize(ctx context.Context, req iam.Request) error {
	// The time of the decision is this node's, never the caller's.
	req.Now = a.clock.Now()
	policy, err := a.scopedPolicy(ctx, req)
	if err != nil {
		a.logger.ErrorContext(ctx, "the policy of a bucket cannot be read; the request is refused",
			slog.String("bucket", req.Resource.Bucket), slog.Any("error", err))
		return fmt.Errorf("authorize: %w", err)
	}
	req.BucketPolicy = policy
	err = iam.Decide(req)
	if err != nil {
		a.logger.WarnContext(ctx, "authorization denied",
			slog.String("principal", req.Principal.Name()),
			slog.String("action", string(req.Action)),
			slog.String("resource", req.Resource.String()),
		)
		if a.auditor != nil {
			a.auditor.Denied(ctx, req, "not allowed by policy")
		}
	}
	return err
}
