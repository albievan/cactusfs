package app

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// CountObjects is the walk behind Usage, with a bound a test can reach.
func (s *ObjectService) CountObjects(ctx context.Context, bucketName string, limit int) (BucketUsage, error) {
	return s.countObjects(ctx, bucketName, limit)
}

// WithShardDeadlines gives the writes and heals of the service deadlines a
// test can reach: how long one shard may take to be created, to take one
// stripe and to be committed.
func (s *ObjectService) WithShardDeadlines(create, write, commit time.Duration) *ObjectService {
	s.storage.deadlines = shardDeadlines{create: create, write: write, commit: commit}
	return s
}

// SearchChunk is the most keys one walk of the index visits for a search.
const SearchChunk = searchChunk

// SearchesAtOnce is how many searches a node runs at a time.
const SearchesAtOnce = searchesAtOnce

// WithSearchBounds gives a search bounds a test can reach.
func (s *SearchService) WithSearchBounds(examined, buckets, chunk int, d time.Duration) *SearchService {
	s.bounds = searchBounds{examined: examined, buckets: buckets, chunk: chunk, time: d}
	return s
}

// EncodeSearchToken makes the continuation a search would give out.
func EncodeSearchToken(query, bucketName, key string, named bool) (string, error) {
	return encodeSearchToken(query, bucketName, key, named)
}

// ParsedPolicies says how many parsed bucket policies the authorizer
// remembers, and of how many bytes of documents.
func (a *Authorizer) ParsedPolicies() (entries, size int) {
	a.parsed.mu.Lock()
	defer a.parsed.mu.Unlock()
	return len(a.parsed.entries), a.parsed.bytes
}

// ParsedPolicy returns the parsed policy the authorizer remembers of a
// bucket, or nil. Two calls return the same pointer for as long as the policy
// was not parsed again.
func (a *Authorizer) ParsedPolicy(bucketName string) *iam.BucketPolicy {
	a.parsed.mu.Lock()
	defer a.parsed.mu.Unlock()
	return a.parsed.entries[bucketName].policy
}

// WithParsedPolicyBounds gives the authorizer's memory of parsed bucket
// policies bounds a test can reach.
func (a *Authorizer) WithParsedPolicyBounds(entries, size int) *Authorizer {
	a.parsed.maxEntries, a.parsed.maxBytes = entries, size
	return a
}

// PrimeBucketPolicies tells the request scope of ctx where the policies of
// all buckets are walked, as a search does.
func PrimeBucketPolicies(ctx context.Context, all ports.BucketPolicies) {
	primeBucketPolicies(ctx, all)
}

// KeepUploadLeases is one pass of the keeper of upload leases as Run makes
// it at a tick.
func (s *SessionService) KeepUploadLeases(ctx context.Context) { s.keepLeases(ctx, true) }

// WokenUploadLeases is the pass Run makes when it is woken - and only when
// it was: it reports whether an upload that began asked for a lease now.
func (s *SessionService) WokenUploadLeases(ctx context.Context) bool {
	select {
	case <-s.leases.wake:
		s.keepLeases(ctx, false)
		return true
	default:
		return false
	}
}

// MadeSessions says how many sessions the node remembers having begun.
func (s *SessionService) MadeSessions() int {
	s.leases.mu.Lock()
	defer s.leases.mu.Unlock()
	return len(s.leases.made)
}

// UnlockFailuresCounted says for how many sessions wrong passwords are
// counted.
func (s *SessionService) UnlockFailuresCounted() int {
	s.unlocks.mu.Lock()
	defer s.unlocks.mu.Unlock()
	return len(s.unlocks.failed)
}

// PasswordChangeKey is the key the mark of a user's last change of password
// is stored under.
func PasswordChangeKey(user string) string { return passwordChangeKey(user) }

// KeptUploadLeases says for how many sessions the node keeps an upload
// lease.
func (s *SessionService) KeptUploadLeases() int {
	s.leases.mu.Lock()
	defer s.leases.mu.Unlock()
	return len(s.leases.kept)
}

// UploadLeaseKey is the key a session's upload lease is stored under.
func UploadLeaseKey(user string, createdAt time.Time) string { return uploadLeaseKey(user, createdAt) }

// HashToken is what a session is stored under: the hash of its token.
func HashToken(token string) string { return hashToken(token) }

// UnlockAttempts is how many wrong passwords end a session.
const UnlockAttempts = unlockAttempts

// SignInAttempts is how many failed sign-ins and unlocks an address may make
// before it has to wait.
const SignInAttempts = signInAttempts

// The budget of failed sign-ins of one user name, from every address, and
// its window; how many addresses a limiter remembers.
const (
	UserSignInAttempts = userSignInAttempts
	UserSignInWindow   = userSignInWindow
	SignInAddresses    = signInAddresses
)

// HeldTargetNames returns how many names of targets have a lock that is held
// or waited for.
func HeldTargetNames(s *TargetService) int {
	s.names.mu.Lock()
	defer s.names.mu.Unlock()
	return len(s.names.held)
}

// AttemptLimiter is the limiter of failed sign-ins, for a test of how it
// chooses what to forget.
type AttemptLimiter struct{ l attemptLimiter }

// NewAttemptLimiter returns a limiter of limit attempts per key in window
// that remembers capacity windows.
func NewAttemptLimiter(limit int, window time.Duration, capacity int) *AttemptLimiter {
	return &AttemptLimiter{l: newAttemptLimiterOf(limit, window, capacity)}
}

// Reserve counts an attempt of key.
func (a *AttemptLimiter) Reserve(key string, now time.Time) bool { return a.l.reserve(key, now) }

// Forgive takes an attempt of key back.
func (a *AttemptLimiter) Forgive(key string) { a.l.forgive(key) }

// Live returns the count of every window that has not ended at now.
func (a *AttemptLimiter) Live(now time.Time) map[string]int {
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	live := map[string]int{}
	for key, w := range a.l.failed {
		if now.Sub(w.since) < a.l.window {
			live[key] = w.count
		}
	}
	return live
}
