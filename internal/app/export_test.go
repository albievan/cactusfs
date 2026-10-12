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
