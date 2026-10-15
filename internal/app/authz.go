package app

import (
	"context"
	"crypto/sha256"
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
	// parsed remembers, from one search to the next, what the policies of
	// buckets say. Nothing but a walk of all policies reads it.
	parsed policyCache
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

// bound reports whether the policy of a bucket has a say in a request.
// Whoever is bound by no policy - the root identity, a node - is asked
// nothing about one, and neither is an authorizer that was given none.
func (a *Authorizer) bound(req iam.Request) bool {
	return a.policies.Load() != nil && req.Resource.Bucket != "" && !req.Principal.Root && req.Principal.NodeID == "" &&
		req.Principal.Service == "" && strings.HasPrefix(string(req.Action), "s3:")
}

// bucketPolicy loads the policy of the bucket a request is about. A policy
// that cannot be read is an error: the request is refused, because the
// policy might have denied it.
func (a *Authorizer) bucketPolicy(ctx context.Context, req iam.Request) (*iam.BucketPolicy, error) {
	if !a.bound(req) {
		return nil, nil
	}
	raw, err := (*a.policies.Load()).GetBucketConfig(ctx, req.Resource.Bucket, bucket.ConfigPolicy)
	if errors.Is(err, domain.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return storedBucketPolicy(raw, req.Resource.Bucket)
}

// storedBucketPolicy reads the policy document a bucket has; nil when it has
// none.
func storedBucketPolicy(raw []byte, bucketName string) (*iam.BucketPolicy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	policy, err := iam.ParseBucketPolicy(raw, bucketName)
	if err != nil {
		// The stored policy is damaged; that is no fault of the request.
		// Only what it says is kept, not what it is: a damaged record must
		// not read as an invalid argument of the caller's.
		return nil, fmt.Errorf("%w: stored bucket policy: %s", domain.ErrIntegrity, err.Error())
	}
	return &policy, nil
}

// Bounds of what an authorizer remembers of parsed bucket policies. A parsed
// policy takes about 1.6 bytes of heap for a byte of its document
// (BenchmarkSearchDeciding reports it as heap/doc), so 128 MiB of documents
// are some 200 MiB of a node that has that many policies: 7 800 buckets whose
// policies are 16 KiB each, or every bucket of a deployment of 100 000 with
// policies of 1 KiB. Past either bound the policies that found no room are
// parsed again by every search that reads them - about half a millisecond for
// one of 16 KiB: slower, never wrong.
const (
	maxParsedPolicies    = 1 << 17
	maxParsedPolicyBytes = 128 << 20
)

// policyCache remembers the parsed policies of buckets by the name of the
// bucket, each under the SHA-256 of the document it was parsed from. It is
// only ever asked with the hash of a document that was just read, so it
// cannot answer with what an older document said: a policy that was changed
// has another hash and is parsed anew. A document that does not parse is
// never remembered - not as a policy and not as the absence of one.
//
// It is bounded in entries and in bytes of documents; what finds no room is
// not remembered. What a walk of all policies did not come by - the policy
// of a bucket that was deleted, a policy that was removed - is dropped when
// that walk ends.
type policyCache struct {
	maxEntries int
	maxBytes   int

	mu      sync.Mutex
	entries map[string]parsedPolicy
	bytes   int
	// walk numbers the walks of all policies that have begun.
	walk uint64
}

// parsedPolicy is one remembered policy. It is shared by every request that
// is decided from it and never changed.
type parsedPolicy struct {
	sum    [sha256.Size]byte
	policy *iam.BucketPolicy
	size   int
	// seen is the newest walk that had begun when the policy was last read.
	seen uint64
}

// begin numbers a walk of all policies.
func (c *policyCache) begin() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.walk++
	return c.walk
}

// get returns what the document of that hash, the policy of that bucket,
// was parsed to, when that is remembered.
func (c *policyCache) get(bucketName string, sum [sha256.Size]byte) (*iam.BucketPolicy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[bucketName]
	if !ok || entry.sum != sum {
		return nil, false
	}
	entry.seen = c.walk
	c.entries[bucketName] = entry
	return entry.policy, true
}

// put remembers what a document was parsed to, and reports whether there
// was room for it. What was remembered of the bucket before is forgotten
// either way: its document is no longer the one stored.
func (c *policyCache) put(bucketName string, sum [sha256.Size]byte, size int, policy *iam.BucketPolicy) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[bucketName]; ok {
		c.bytes -= old.size
		delete(c.entries, bucketName)
	}
	if len(c.entries) >= c.maxEntries || c.bytes+size > c.maxBytes {
		return false
	}
	if c.entries == nil {
		c.entries = map[string]parsedPolicy{}
	}
	c.entries[bucketName] = parsedPolicy{sum: sum, policy: policy, size: size, seen: c.walk}
	c.bytes += size
	return true
}

// end is called when a walk has read the policies of all buckets, from the
// first to the last: what no walk has come by since this one began is of a
// bucket that is gone or has no policy any more, and is dropped. remembered
// is how many policies the walk found or put here; when that is all there
// are, nothing is looked through.
func (c *policyCache) end(began uint64, remembered int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if remembered >= len(c.entries) {
		return
	}
	for name, entry := range c.entries {
		if entry.seen < began {
			c.bytes -= entry.size
			delete(c.entries, name)
		}
	}
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
	// walk, when given, is where this request takes the policies of all
	// buckets from, page by page. The authorizer's own source is then not
	// asked, and policies stays empty: the request keeps one page.
	walk *policyWalk
}

// policyWalk is one request's way through the policies of all buckets, in
// name order. It keeps the page it has read last and nothing of the pages
// before: what a search needs to keep of a bucket is its decision, a bit.
type policyWalk struct {
	all ports.BucketPolicies
	// The buckets from the name from up to, not including, next have been
	// read, and page holds the policy of each of them that has one. When
	// done, there is no bucket from next on.
	started    bool
	from, next string
	done       bool
	page       map[string]readPolicy
	// began numbers the walk for the authorizer's memory of policies;
	// whole says that it began at the first bucket, and remembered how
	// many policies it found in that memory or put there.
	began      uint64
	whole      bool
	remembered int
}

// readPolicy is the policy of one bucket of a page, or why it cannot be
// read: the stored document is damaged.
type readPolicy struct {
	policy *iam.BucketPolicy
	err    error
}

// WithRequestScope returns a context for the handling of one request. The
// authorizer reads the policy of a bucket once per such context. It must not
// outlive the request.
func WithRequestScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestScopeKey{}, &requestScope{policies: map[string]*iam.BucketPolicy{}})
}

// primeBucketPolicies tells the request scope of ctx where the policies of
// all buckets are read page by page, so that the authorizer decides the
// requests of this scope from those pages instead of reading bucket by
// bucket. That is how one request gets a decision about every bucket there
// is - a search - without a question to the leader for each. The first page
// is read when the first request that a bucket policy binds is decided:
// whoever is bound by none causes no reading. The scope expects to be asked
// about buckets in name order, as a search asks; asked about a bucket before
// the page it holds, it begins again at that bucket.
//
// It changes where the authorizer finds a policy and nothing of how it
// decides: who is bound by a policy, how a document is read and what follows
// from it stay in scopedPolicy and decide. It stays inside this package: a
// use case names a store, never an adapter what a caller sent. Without a
// request scope it does nothing.
func primeBucketPolicies(ctx context.Context, all ports.BucketPolicies) {
	scope, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	if scope == nil {
		return
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.walk = &policyWalk{all: all}
	// What was read before is older than what will be read now.
	clear(scope.policies)
}

// scopedPolicy is bucketPolicy, read once per request scope - or, when the
// scope was told where the policies of all buckets are, taken from the walk
// of them all.
func (a *Authorizer) scopedPolicy(ctx context.Context, req iam.Request) (*iam.BucketPolicy, error) {
	scope, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	if scope == nil || !a.bound(req) {
		return a.bucketPolicy(ctx, req)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.walk != nil {
		return a.walkedPolicy(ctx, scope.walk, req.Resource.Bucket)
	}
	if policy, ok := scope.policies[req.Resource.Bucket]; ok {
		return policy, nil
	}
	policy, err := a.bucketPolicy(ctx, req)
	if err != nil {
		return nil, err
	}
	scope.policies[req.Resource.Bucket] = policy
	return policy, nil
}

// walkedPolicy returns the policy of a bucket from a walk of the policies of
// all buckets, reading on until the walk has come to that bucket. A bucket
// the walk has passed without being shown a policy has none.
//
// The error wraps domain.ErrIntegrity when the stored policy of this one
// bucket is damaged, and never otherwise: a page that cannot be read - be it
// for damage - says nothing of any bucket, and is domain.ErrUnavailable.
func (a *Authorizer) walkedPolicy(ctx context.Context, w *policyWalk, bucketName string) (*iam.BucketPolicy, error) {
	if !w.started || bucketName < w.from {
		// The first question begins at the first bucket. One about a
		// bucket the walk has left behind begins another walk there, which
		// is no longer one of all buckets.
		begin := ""
		if w.started {
			begin = bucketName
		}
		*w = policyWalk{all: w.all, started: true, from: begin, next: begin, began: a.parsed.begin(), whole: begin == ""}
	}
	for !w.done && bucketName >= w.next {
		if err := a.readPolicies(ctx, w); err != nil {
			return nil, err
		}
	}
	read := w.page[bucketName]
	return read.policy, read.err
}

// walkedSummary returns the summary of the policy of a bucket, nil when it
// has none, from the walk of the policies of all buckets that the request
// scope of ctx was primed with: the walk the requests of that scope are
// decided by. For a bucket that was just decided the policy is on the page
// the walk holds, and nothing is read; for a caller whom no bucket policy
// binds, who caused no reading, the walk is made here. Either way the policy
// is the one readPolicies parsed, or remembered under the hash of the
// document it had just read - never what an older document said - and it is
// not parsed again for its summary.
//
// readable is false when the stored policy of this one bucket is damaged;
// the error says that a page of the policies could not be read, which says
// nothing of any bucket, or that ctx carries no such walk. It decides
// nothing: it classifies a document for whoever was allowed to read it.
func (a *Authorizer) walkedSummary(ctx context.Context, bucketName string) (summary *iam.BucketPolicySummary, readable bool, err error) {
	scope, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	if scope == nil {
		return nil, false, fmt.Errorf("bucket policies: %w: the request was not told where they are", domain.ErrUnavailable)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.walk == nil {
		return nil, false, fmt.Errorf("bucket policies: %w: the request was not told where they are", domain.ErrUnavailable)
	}
	policy, err := a.walkedPolicy(ctx, scope.walk, bucketName)
	switch {
	case errors.Is(err, domain.ErrIntegrity):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case policy == nil:
		return nil, true, nil
	}
	made := policy.Summary()
	return &made, true, nil
}

// readPolicies reads the next page of a walk. Each document is hashed, and
// parsed unless the authorizer remembers what a document of that hash, of
// that bucket, was parsed to. Nothing of a document is kept.
func (a *Authorizer) readPolicies(ctx context.Context, w *policyWalk) error {
	page := make(map[string]readPolicy, len(w.page))
	remembered := 0
	next, err := w.all.BucketPolicies(ctx, w.next, func(bucketName string, document []byte) error {
		sum := sha256.Sum256(document)
		policy, known := a.parsed.get(bucketName, sum)
		if !known {
			var err error
			if policy, err = storedBucketPolicy(document, bucketName); err != nil {
				// This bucket's, and no reason to stop reading the others.
				page[bucketName] = readPolicy{err: err}
				return nil
			}
			known = a.parsed.put(bucketName, sum, len(document), policy)
		}
		if known {
			remembered++
		}
		page[bucketName] = readPolicy{policy: policy}
		return nil
	})
	switch {
	case err != nil && ctx.Err() != nil:
		// The request is gone; that is what its failure is.
		return fmt.Errorf("bucket policies: %w", ctx.Err())
	case errors.Is(err, domain.ErrIntegrity):
		// The store could not be read. That is not the damaged policy of
		// one bucket, which leaves the others decided.
		return fmt.Errorf("%w: the policies of the buckets cannot be read: %s", domain.ErrUnavailable, err.Error())
	case err != nil:
		return fmt.Errorf("bucket policies: %w", err)
	case next != "" && next <= w.next:
		return fmt.Errorf("%w: the walk of the bucket policies went back from %q to %q", domain.ErrUnavailable, w.next, next)
	}
	w.from, w.next, w.done, w.page = w.next, next, next == "", page
	w.remembered += remembered
	if w.done && w.whole {
		a.parsed.end(w.began, w.remembered)
	}
	return nil
}

var _ ports.Authorizer = (*Authorizer)(nil)

// NewAuthorizer returns an Authorizer. The clock gives the time that
// conditions on time are tested against. auditor may be nil, in which case
// refusals are logged only.
func NewAuthorizer(logger *slog.Logger, clock ports.Clock, auditor *Auditor) *Authorizer {
	return &Authorizer{
		logger: logger, clock: clock, auditor: auditor,
		parsed: policyCache{maxEntries: maxParsedPolicies, maxBytes: maxParsedPolicyBytes},
	}
}

// decide is the one decision behind Authorize and Allowed. It returns the
// request as it was judged - with the time and the policy of its bucket -
// and the refusal, which is nil when the request is allowed. The error says
// that no decision could be made: the policy of the bucket could not be
// read, and that has been logged.
func (a *Authorizer) decide(ctx context.Context, req iam.Request) (judged iam.Request, refusal, err error) {
	// The time of the decision is this node's, never the caller's.
	req.Now = a.clock.Now()
	if req.Principal.LockedSession {
		// What a locked session is refused, the domain refuses before any
		// policy has a say (ADR-0027): asked without the policy of the
		// bucket, it answers that already, and no policy is read for it.
		// The decision is the domain's; this only spares the reading.
		if refusal := iam.Decide(req); errors.Is(refusal, domain.ErrSessionLocked) {
			return req, refusal, nil
		}
	}
	policy, err := a.scopedPolicy(ctx, req)
	if err != nil {
		a.logger.ErrorContext(ctx, "the policy of a bucket cannot be read; the request is refused",
			slog.String("bucket", req.Resource.Bucket), slog.Any("error", err))
		return req, nil, fmt.Errorf("authorize: %w", err)
	}
	req.BucketPolicy = policy
	return req, iam.Decide(req), nil
}

// Allowed implements ports.Authorizer: the decision of Authorize without
// its record. A refusal is neither audited nor logged, so this is for
// filtering what a caller is shown and never the gate of an operation.
//
// The refusal of a locked session is no "no": it is returned as the error,
// which wraps domain.ErrSessionLocked. What is left out of a list for it
// would read as "there is nothing", and a locked session is to be told that
// it is locked. Because that error ends the operation that asked - it is
// its refusal, and for a search the only one - it is recorded as Authorize
// records it; the auditor counts the repeats of one caller, so a filter that
// asks a hundred times writes one record.
func (a *Authorizer) Allowed(ctx context.Context, req iam.Request) (bool, error) {
	req, refusal, err := a.decide(ctx, req)
	if err != nil {
		return false, err
	}
	if errors.Is(refusal, domain.ErrSessionLocked) {
		a.refused(ctx, req, refusal)
		return false, fmt.Errorf("authorize: %w", refusal)
	}
	return refusal == nil, nil
}

// refused writes a refusal to the log and to the audit trail.
func (a *Authorizer) refused(ctx context.Context, req iam.Request, refusal error) {
	message, reason := "authorization denied", "not allowed by policy"
	if errors.Is(refusal, domain.ErrSessionLocked) {
		message, reason = "refused: the session is locked", "the session is locked"
	}
	a.logger.WarnContext(ctx, message,
		slog.String("principal", req.Principal.Name()),
		slog.String("action", string(req.Action)),
		slog.String("resource", req.Resource.String()),
	)
	if a.auditor != nil {
		a.auditor.Denied(ctx, req, reason)
	}
}

// Authorize implements ports.Authorizer.
func (a *Authorizer) Authorize(ctx context.Context, req iam.Request) error {
	req, refusal, err := a.decide(ctx, req)
	if err != nil {
		return err
	}
	if refusal != nil {
		a.refused(ctx, req, refusal)
	}
	return refusal
}
