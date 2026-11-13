package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// SubBucketService holds the use cases of the sub-buckets of a bucket
// (ADR-0017): named first-level prefixes, each with a policy of its own.
// Every one passes the single authorizer first, with the actions of
// sub-buckets asked on the bucket, and every change carries its audit record
// into the Raft entry that makes it: a change is never made without its
// record.
type SubBucketService struct {
	store   ports.SubBucketStore
	authz   ports.Authorizer
	auditor *Auditor
	clock   ports.Clock
	logger  *slog.Logger
}

// NewSubBucketService returns a SubBucketService. The auditor builds the
// records of changes; without one every change is refused.
func NewSubBucketService(store ports.SubBucketStore, authz ports.Authorizer, auditor *Auditor, clock ports.Clock, logger *slog.Logger) *SubBucketService {
	return &SubBucketService{store: store, authz: authz, auditor: auditor, clock: clock, logger: logger}
}

// summaries returns the sub-buckets without their policies, never nil: a
// listing shows names and ages, and a policy only to whoever may read it
// (Get, with s3:GetSubBucketPolicy).
func summaries(subs []bucket.SubBucket) []bucket.SubBucket {
	out := make([]bucket.SubBucket, 0, len(subs))
	for _, sub := range subs {
		sub.Policy = nil
		out = append(out, sub)
	}
	return out
}

// withScope returns ctx with a request scope, unless it has one: every
// decision of one call is then made against one reading of the bucket's
// policy and its sub-buckets, and not one reading a decision.
func withScope(ctx context.Context) context.Context {
	if _, ok := ctx.Value(requestScopeKey{}).(*requestScope); ok {
		return ctx
	}
	return WithRequestScope(ctx)
}

// record builds the audit record of a change that was allowed. The
// sub-bucket is named in the details, as the request was asked on the
// bucket.
func (s *SubBucketService) record(principal iam.Principal, action iam.Action, bucketName string, details map[string]string) (audit.Record, error) {
	if s.auditor == nil {
		return audit.Record{}, fmt.Errorf("%w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	return s.auditor.Record(principal, action, iam.Resource{Bucket: bucketName}, audit.Succeeded, details), nil
}

// List returns the sub-buckets of a bucket in name order, without their
// policies, and an empty list - never nil - when there are none to show. A
// caller who holds
// s3:ListSubBuckets on the bucket is given every one. Any other caller is
// given those whose keys they may list - each decided as a listing of the
// bucket with the sub-bucket's prefix is (ObjectService.List), from the
// bucket's policy and the sub-bucket's - and a sub-bucket they may not list
// is left out, not refused: it is invisible.
//
// A caller to whom nothing of the bucket is visible - who may neither list
// the sub-buckets, nor the bucket, nor the keys of any of its sub-buckets -
// is refused s3:ListSubBuckets, and the refusal is audited; such a caller is
// not told whether the bucket is there either. The error wraps
// domain.ErrBucketNotFound otherwise.
func (s *SubBucketService) List(ctx context.Context, principal iam.Principal, bucketName string) ([]bucket.SubBucket, error) {
	ctx = withScope(ctx)
	asked := bucketRequest(principal, iam.ActionListSubBuckets, bucketName)
	everything, err := s.authz.Allowed(ctx, asked)
	if err != nil {
		return nil, fmt.Errorf("subbucket.List: %w", err)
	}
	if everything {
		subs, err := s.store.ListSubBuckets(ctx, bucketName)
		if err != nil {
			return nil, fmt.Errorf("subbucket.List: %w", err)
		}
		return summaries(subs), nil
	}
	whole, err := s.authz.Allowed(ctx, listRequest(principal, bucketName))
	if err != nil {
		return nil, fmt.Errorf("subbucket.List: %w", err)
	}
	subs, err := s.store.ListSubBuckets(ctx, bucketName)
	switch {
	case errors.Is(err, domain.ErrBucketNotFound) && !whole:
		return nil, s.refuse(ctx, asked, err)
	case err != nil:
		return nil, fmt.Errorf("subbucket.List: %w", err)
	}
	visible, err := s.mayList(ctx, principal, bucketName, subs)
	if err != nil {
		return nil, fmt.Errorf("subbucket.List: %w", err)
	}
	if len(visible) == 0 && !whole {
		if err := s.refuse(ctx, asked, nil); err != nil {
			return nil, err
		}
	}
	return summaries(visible), nil
}

// mayList returns those of the sub-buckets the caller may list the keys of.
// The authorizer decides from the same reading of the sub-buckets.
func (s *SubBucketService) mayList(ctx context.Context, principal iam.Principal, bucketName string, subs []bucket.SubBucket) ([]bucket.SubBucket, error) {
	primeSubBuckets(ctx, bucketName, subs)
	visible := []bucket.SubBucket{}
	for _, sub := range subs {
		listing := listRequest(principal, bucketName)
		listing.ListPrefix = sub.Prefix()
		allowed, err := s.authz.Allowed(ctx, listing)
		switch {
		case errors.Is(err, domain.ErrIntegrity):
			// The stored policy of this sub-bucket is damaged, which the
			// authorizer has logged; a listing of it is refused, and so it
			// is not shown.
			continue
		case err != nil:
			return nil, err
		case allowed:
			visible = append(visible, sub)
		}
	}
	return visible, nil
}

// refuse asks the authorizer the question a caller was found to see nothing
// by, so that the refusal is decided, logged and audited where every refusal
// is. Should the answer have changed in the meantime and allow it, what the
// call found is returned instead: notFound when the bucket was not there,
// otherwise an empty list.
func (s *SubBucketService) refuse(ctx context.Context, asked iam.Request, notFound error) error {
	if err := s.authz.Authorize(ctx, asked); err != nil {
		return fmt.Errorf("subbucket.List: %w", err)
	}
	if notFound != nil {
		return fmt.Errorf("subbucket.List: %w", notFound)
	}
	return nil
}

// Get returns a sub-bucket with its policy. The caller must hold
// s3:GetSubBucketPolicy on the bucket. The error wraps
// domain.ErrBucketNotFound or domain.ErrSubBucketNotFound.
func (s *SubBucketService) Get(ctx context.Context, principal iam.Principal, bucketName, name string) (bucket.SubBucket, error) {
	if err := s.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetSubBucketPolicy, bucketName)); err != nil {
		return bucket.SubBucket{}, fmt.Errorf("subbucket.Get: %w", err)
	}
	sub, err := s.store.GetSubBucket(ctx, bucketName, name)
	if err != nil {
		return bucket.SubBucket{}, fmt.Errorf("subbucket.Get: %w", err)
	}
	return sub, nil
}

// GetPolicy returns the policy document of a sub-bucket as it was put; nil
// when it has none. The caller must hold s3:GetSubBucketPolicy on the
// bucket. The error wraps domain.ErrBucketNotFound or
// domain.ErrSubBucketNotFound.
func (s *SubBucketService) GetPolicy(ctx context.Context, principal iam.Principal, bucketName, name string) ([]byte, error) {
	sub, err := s.Get(ctx, principal, bucketName, name)
	if err != nil {
		return nil, fmt.Errorf("subbucket.GetPolicy: %w", err)
	}
	if len(sub.Policy) == 0 {
		return nil, nil
	}
	return sub.Policy, nil
}

// Put registers a sub-bucket of a bucket with the policy given, or - when
// the bucket has one of that name - sets its policy in place of the one it
// had; it keeps the time it was created. An empty policy is a sub-bucket
// without a policy of its own, and one it had is removed. The caller must
// hold s3:PutSubBucket on the bucket.
//
// The policy is confined to the sub-bucket by iam.ParseSubBucketPolicy
// before anything is proposed: one it refuses is an error wrapping
// domain.ErrInvalidPolicy, and nothing is changed. A name a sub-bucket
// cannot have wraps domain.ErrInvalidArgument. The eleventh sub-bucket of a
// bucket is refused with domain.ErrTooManySubBuckets, decided when the
// change is committed. The error wraps domain.ErrBucketNotFound when there
// is no such bucket.
//
// It returns what this call put, with the time the sub-bucket was created
// as it is stored. Should that not be readable right after the change, which
// was made, the failure is logged and the time is this call's; should
// another writer have put another policy since, this call's is returned all
// the same - never another writer's document.
func (s *SubBucketService) Put(ctx context.Context, principal iam.Principal, bucketName, name string, policy []byte) (bucket.SubBucket, error) {
	if err := s.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutSubBucket, bucketName)); err != nil {
		return bucket.SubBucket{}, fmt.Errorf("subbucket.Put: %w", err)
	}
	details := map[string]string{"sub_bucket": name, "policy": "none"}
	if len(policy) == 0 {
		policy = nil
		if err := bucket.ValidateSubBucketName(name); err != nil {
			return bucket.SubBucket{}, fmt.Errorf("subbucket.Put: %w", err)
		}
	} else {
		parsed, err := iam.ParseSubBucketPolicy(policy, bucketName, name)
		if err != nil {
			return bucket.SubBucket{}, fmt.Errorf("subbucket.Put: %w", err)
		}
		details["policy"] = "set"
		details["statements"] = strconv.Itoa(len(parsed.Policy().Document.Statements))
	}
	record, err := s.record(principal, iam.ActionPutSubBucket, bucketName, details)
	if err != nil {
		return bucket.SubBucket{}, fmt.Errorf("subbucket.Put: %w", err)
	}
	put := bucket.SubBucket{Name: name, Policy: policy, CreatedAt: s.clock.Now().UTC()}
	if err := s.store.PutSubBucket(ctx, bucketName, put, record); err != nil {
		return bucket.SubBucket{}, fmt.Errorf("subbucket.Put: %w", err)
	}
	stored, err := s.store.GetSubBucket(ctx, bucketName, name)
	if err != nil {
		s.logger.WarnContext(ctx, "a sub-bucket that was put cannot be read back; its time of creation is taken as now",
			slog.String("bucket", bucketName), slog.String("sub_bucket", name), slog.Any("error", err))
		return put, nil
	}
	if bytes.Equal(stored.Policy, put.Policy) {
		return stored, nil
	}
	put.CreatedAt = stored.CreatedAt
	return put, nil
}

// Delete removes a sub-bucket and its policy; its keys are not deleted with
// it. Only an empty one is removed: one under whose name the bucket holds
// no object, no older version, no delete marker and no upload in progress,
// decided when the change is committed. The caller must hold
// s3:DeleteSubBucket on the bucket. The error wraps
// domain.ErrBucketNotFound, domain.ErrSubBucketNotFound or
// domain.ErrSubBucketNotEmpty.
func (s *SubBucketService) Delete(ctx context.Context, principal iam.Principal, bucketName, name string) error {
	if err := s.authz.Authorize(ctx, bucketRequest(principal, iam.ActionDeleteSubBucket, bucketName)); err != nil {
		return fmt.Errorf("subbucket.Delete: %w", err)
	}
	record, err := s.record(principal, iam.ActionDeleteSubBucket, bucketName, map[string]string{"sub_bucket": name})
	if err != nil {
		return fmt.Errorf("subbucket.Delete: %w", err)
	}
	if err := s.store.DeleteSubBucket(ctx, bucketName, name, record); err != nil {
		return fmt.Errorf("subbucket.Delete: %w", err)
	}
	return nil
}
