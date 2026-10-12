package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/search"
	"github.com/albievan/cactusfs/internal/ports"
)

// Bounds of one call of SearchService.Search. A search walks the index of
// every bucket it looks into, and a walk holds the metadata store's read
// lock, which a change waiting to be applied waits for (see MaxUsageScan):
// so a call looks at a bounded number of keys, in chunks between which the
// lock is let go, for a bounded time, and says where it stopped.
const (
	// DefaultSearchLimit is how many results a call returns when it is not
	// told.
	DefaultSearchLimit = 100
	// MaxSearchLimit is the most results a call returns.
	MaxSearchLimit = 500
	// MaxSearchExamined is the most keys a call looks at.
	MaxSearchExamined = 50_000
	// MaxSearchBuckets is the most buckets a call looks at or into.
	MaxSearchBuckets = 200
	// MaxSearchTime is how long a call goes on beginning new work.
	MaxSearchTime = 2 * time.Second
	// MaxSearchContinuation is the longest continuation token, in bytes:
	// room for the longest key there is.
	MaxSearchContinuation = 4096

	// searchChunk is the most keys one walk of the index visits: about
	// twenty milliseconds of the read lock at four microseconds a key.
	searchChunk = 5_000
	// searchesAtOnce is how many searches a node runs at a time. Each is
	// one principal's, and holds the read lock of the metadata store for a
	// chunk at a time: eight of them are still a small share of a node, and
	// two principals who search without pause do not shut out everybody
	// else, as they did when there were two places.
	searchesAtOnce = 8
	// searchTokenVersion is the version of the continuation token.
	searchTokenVersion = 1
)

// The kinds of search result.
const (
	// SearchResultBucket is a bucket whose name answers to the query.
	SearchResultBucket = "bucket"
	// SearchResultObject is an object whose key answers to the query.
	SearchResultObject = "object"
)

// SearchInput is what a search is asked.
type SearchInput struct {
	// Query is what was typed: text, a wildcard or "rx:" and a regular
	// expression (package search).
	Query string
	// Bucket, when not empty, is the only bucket looked into. Its name is
	// not itself a result.
	Bucket string
	// Continuation is the NextContinuation of the call this one goes on
	// from, with the same Query and Bucket.
	Continuation string
	// Limit is the most results wanted: 1 to MaxSearchLimit, or zero for
	// DefaultSearchLimit.
	Limit int
}

// SearchResult is a bucket or an object that answers to a query.
type SearchResult struct {
	// Kind is SearchResultBucket or SearchResultObject.
	Kind string
	// Bucket is the bucket, or the bucket of the object.
	Bucket string
	// Key, Size, ModifiedAt and ETag are the object's; a bucket has none.
	Key        string
	Size       int64
	ModifiedAt time.Time
	ETag       string
}

// SearchOutput is one page of a search.
type SearchOutput struct {
	// Results are in the order they were found: buckets by name, each
	// followed by its objects by key.
	Results []SearchResult
	// MatchKind says how the query was read.
	MatchKind search.Kind
	// Examined is how many keys this call looked at.
	Examined int
	// Complete is false when a bound stopped the call before it had
	// looked at everything; NextContinuation then goes on from there.
	Complete         bool
	NextContinuation string
}

// SearchServiceConfig lists what a SearchService depends on.
type SearchServiceConfig struct {
	Buckets ports.BucketStore
	Index   ports.ObjectIndex
	// Policies gives the policies of all buckets from this node's state, a
	// page at a time, for the authorizer to decide every bucket from.
	Policies ports.BucketPolicies
	Authz    ports.Authorizer
	Clock    ports.Clock
	Logger   *slog.Logger
}

// searchBounds are the bounds of a call; a test makes them small.
type searchBounds struct {
	examined int
	buckets  int
	chunk    int
	time     time.Duration
}

// SearchService finds buckets and objects by name, for the search field of
// the console.
//
// A search is a read, like a listing, and shows nothing a listing would not:
// the name of a bucket only to who may list the buckets, the keys of a
// bucket only to who may list that bucket - each bucket decided by the
// authorizer, the bucket's own policy included. Nothing is written to the
// audit trail for a search, nor for the buckets it leaves out.
//
// What a call costs before it looks at a key does not depend on what the
// caller may see nor on the continuation it brings: one consistent listing
// of the buckets, one walk of their policies in this node's state - page by
// page, each policy hashed, and parsed only when the authorizer does not
// remember it from a search before - and one decision of the authorizer for
// every bucket there is. The root identity, bound by no bucket policy, causes
// no walk.
type SearchService struct {
	buckets  ports.BucketStore
	index    ports.ObjectIndex
	policies ports.BucketPolicies
	authz    ports.Authorizer
	clock    ports.Clock
	logger   *slog.Logger
	bounds   searchBounds

	// running holds who is searching on this node right now.
	mu      sync.Mutex
	running map[string]struct{}
}

// NewSearchService returns a SearchService.
func NewSearchService(cfg SearchServiceConfig) (*SearchService, error) {
	if cfg.Buckets == nil || cfg.Index == nil || cfg.Policies == nil || cfg.Authz == nil || cfg.Clock == nil || cfg.Logger == nil {
		return nil, fmt.Errorf("search.New: %w: a dependency is missing", domain.ErrInvalidArgument)
	}
	return &SearchService{
		buckets: cfg.Buckets, index: cfg.Index, policies: cfg.Policies, authz: cfg.Authz, clock: cfg.Clock, logger: cfg.Logger,
		bounds:  searchBounds{examined: MaxSearchExamined, buckets: MaxSearchBuckets, chunk: searchChunk, time: MaxSearchTime},
		running: map[string]struct{}{},
	}, nil
}

// searchToken is where a search stopped. It travels as base64url of its
// JSON.
type searchToken struct {
	// V is searchTokenVersion.
	V int `json:"v"`
	// Q is the SHA-256 of the query the token belongs to, in hex.
	Q string `json:"q"`
	// B is the bucket to go on with.
	B string `json:"b"`
	// K is the last key of B that was looked at; empty before the first.
	K []byte `json:"k"`
	// N says that the name of B itself has been looked at already.
	N bool `json:"n"`
}

// queryHash names a query in a token.
func queryHash(query string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(query)))
	return hex.EncodeToString(sum[:])
}

// encodeSearchToken makes the continuation of a search that stopped at
// bucket b, having looked at its keys up to and including k.
func encodeSearchToken(query, b, k string, named bool) (string, error) {
	raw, err := json.Marshal(searchToken{V: searchTokenVersion, Q: queryHash(query), B: b, K: []byte(k), N: named})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeSearchToken reads a continuation. The token is validated, not
// signed, and it need not be: it says where to go on and grants nothing.
// Every call decides again, bucket by bucket, what the caller may see, and
// only then looks at where the token points - among the buckets that are the
// caller's to see. So a token somebody made up can only skip ahead in what
// that caller would have been shown anyway: a bucket they may not list
// yields nothing whatever the token says of it, and the call does the same
// work before its first key whatever the token says.
func decodeSearchToken(token, query, bucketName string) (searchToken, error) {
	refuse := func(why string) (searchToken, error) {
		return searchToken{}, fmt.Errorf("%w: continuation: %s", domain.ErrInvalidArgument, why)
	}
	if len(token) > MaxSearchContinuation {
		return refuse("it is too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return refuse("it is not one this API gave out")
	}
	var at searchToken
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&at); err != nil || decoder.More() {
		return refuse("it is not one this API gave out")
	}
	switch {
	case at.V != searchTokenVersion:
		return refuse("it is of a version this node does not know")
	case at.Q != queryHash(query):
		return refuse("it belongs to another search")
	case bucket.ValidateName(at.B) != nil || len(at.K) > object.MaxKeyLength:
		// A token this API gave out names a bucket, and a bucket has a
		// valid name.
		return refuse("it is not one this API gave out")
	case bucketName != "" && at.B != bucketName:
		return refuse("it belongs to a search of another bucket")
	}
	return at, nil
}

// admit lets a search begin, or says that there are too many: one at a time
// for a principal, and searchesAtOnce for the node. The returned function
// ends it.
func (s *SearchService) admit(actor string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.running[actor]; busy {
		return nil, fmt.Errorf("%w: a search of yours is still running", domain.ErrTooManyRequests)
	}
	if len(s.running) >= searchesAtOnce {
		return nil, fmt.Errorf("%w: this node is searching for others", domain.ErrTooManyRequests)
	}
	s.running[actor] = struct{}{}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.running, actor)
	}, nil
}

// Search returns one page of what answers to a query among the buckets and
// objects the principal may list.
//
// The error wraps domain.ErrInvalidArgument for a query, a limit or a
// continuation that is none; domain.ErrTooManyRequests when the principal is
// searching already or the node is busy searching; domain.ErrAccessDenied
// - the one refusal of a search that is audited - when the principal may
// list neither the buckets nor any bucket, or not the bucket it named; and
// domain.ErrUnavailable together with domain.ErrTooManyBuckets when there
// are so many buckets that deciding which of them the principal may see took
// all the time a call has. Such a call can be repeated with some hope while
// the authorizer does not yet remember the policies - after a restart, each
// call parses more of them and the next gets further; when it remembers them
// all and the time is still not enough, the deployment is past what a search
// by anybody but the root identity can do.
func (s *SearchService) Search(ctx context.Context, principal iam.Principal, in SearchInput) (SearchOutput, error) {
	limit := in.Limit
	switch {
	case limit == 0:
		limit = DefaultSearchLimit
	case limit < 0 || limit > MaxSearchLimit:
		return SearchOutput{}, fmt.Errorf("search.Search: %w: limit must be 1 to %d", domain.ErrInvalidArgument, MaxSearchLimit)
	}
	query, err := search.Parse(in.Query)
	if err != nil {
		return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
	}
	var at searchToken
	if in.Continuation != "" {
		if at, err = decodeSearchToken(in.Continuation, in.Query, in.Bucket); err != nil {
			return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
		}
	}
	done, err := s.admit(actorOf(principal))
	if err != nil {
		return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
	}
	defer done()

	// One reading of a bucket's policy serves the whole call.
	ctx = WithRequestScope(ctx)
	w := &searchWalk{
		s: s, principal: principal, query: query, limit: limit, started: s.clock.Now(),
		may: map[string]bool{}, out: SearchOutput{MatchKind: query.Kind(), Results: []SearchResult{}},
	}
	var names []string
	if in.Bucket != "" {
		// A bucket the caller names is asked for, not filtered: the gate
		// is the one a listing of it passes, and a refusal is audited. It
		// comes before the look at the bucket, so that who may not list it
		// does not learn whether it is there.
		if err = s.authz.Authorize(ctx, listRequest(principal, in.Bucket)); err != nil {
			return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
		}
		if _, err = s.buckets.GetBucket(ctx, in.Bucket); err != nil {
			return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
		}
		names, w.may[in.Bucket], w.objectsOnly = []string{in.Bucket}, true, true
	} else if names, err = w.visible(ctx); err != nil {
		return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
	}

	// Only now is the token looked at, and only among the buckets that are
	// the caller's to see: nothing done so far depended on it.
	stopped, err := w.walk(ctx, names[sort.SearchStrings(names, at.B):], at)
	if err != nil {
		return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
	}
	if stopped == nil {
		w.out.Complete = true
	} else if w.out.NextContinuation, err = encodeSearchToken(in.Query, stopped.bucket, stopped.key, stopped.named); err != nil {
		return SearchOutput{}, fmt.Errorf("search.Search: %w", err)
	}
	// What was searched for is not said: a query can be as telling as a
	// key.
	s.logger.DebugContext(ctx, "search",
		slog.String("kind", string(w.out.MatchKind)), slog.Int("examined", w.out.Examined), slog.Int("results", len(w.out.Results)),
		slog.Int("buckets", w.visited), slog.Bool("complete", w.out.Complete), slog.Duration("took", s.clock.Now().Sub(w.started)))
	return w.out, nil
}

// listRequest is the question a listing of a bucket asks the authorizer
// (ObjectService.List); a search asks the same, so that it sees into exactly
// the buckets a listing would.
func listRequest(principal iam.Principal, bucketName string) iam.Request {
	return iam.Request{Principal: principal, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: bucketName}}
}

// searchWalk is one call of Search on its way through the buckets.
type searchWalk struct {
	s         *SearchService
	principal iam.Principal
	query     search.Query
	limit     int
	started   time.Time
	out       SearchOutput

	// listAll says that the caller may list the buckets, and so see their
	// names; objectsOnly that one bucket was named and its name is no
	// result.
	listAll     bool
	objectsOnly bool
	// may remembers which buckets the caller may list.
	may map[string]bool
	// visited counts the buckets looked at or into, chunks the walks of
	// the index.
	visited int
	chunks  int
}

// searchStop is where a walk stopped: at a bucket, having looked at its keys
// up to and including key.
type searchStop struct {
	bucket string
	key    string
	named  bool
}

// visible returns, in name order, the buckets that are the caller's to see:
// those the caller may list and, for who may list the buckets, all of them.
// It refuses, through the gate, who may see none.
//
// Every bucket there is gets decided, whatever the caller brought: what this
// costs says nothing of which buckets are theirs, and nothing of where a
// continuation points. The buckets are listed in one consistent read; their
// policies are then taken from this node's own state, which is by then at
// least as new as the listing, page by page as the decisions come to them -
// not from the leader bucket by bucket - and every decision is the
// authorizer's, made from them. A call keeps a decision of every bucket and
// the policies of one page, never those of all buckets. Who is bound by no
// bucket policy, the root identity, causes no reading at all.
func (w *searchWalk) visible(ctx context.Context) ([]string, error) {
	all, err := w.s.buckets.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(all))
	for i, b := range all {
		names[i] = b.Name
	}
	// The store lists in name order; the walk depends on it.
	sort.Strings(names)
	w.may = make(map[string]bool, len(names))
	// From here on the authorizer takes the policies of the buckets from a
	// walk of them all, begun after the listing above.
	primeBucketPolicies(ctx, w.s.policies)
	// The names of buckets are for who may list the buckets.
	if w.listAll, err = w.s.authz.Allowed(ctx, iam.Request{Principal: w.principal, Action: iam.ActionListAllMyBuckets}); err != nil {
		return nil, err
	}
	// Deciding is work in this node's memory, and still it is given no more
	// than the time of a call; the time is looked at before every bucket,
	// and a decision reads a page of policies at the most. When the time is
	// not enough the call fails. It cannot stop and say where: a place among
	// buckets that are not the caller's must not be told, and a call that
	// went on from such a place would have to decide every bucket again.
	// What the authorizer has parsed until then it remembers, so the same
	// call made again gets further - unless nothing was left to parse.
	deciding := w.s.clock.Now()
	seen := make([]string, 0, len(names))
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if w.s.clock.Now().Sub(deciding) >= w.s.bounds.time {
			return nil, fmt.Errorf("%w: %w: the buckets could not be decided in the time of a search", domain.ErrUnavailable, domain.ErrTooManyBuckets)
		}
		var allowed bool
		if allowed, err = w.mayList(ctx, name); err != nil {
			return nil, err
		}
		if allowed || w.listAll {
			seen = append(seen, name)
		}
	}
	if len(seen) == 0 && !w.listAll {
		// Nothing is the caller's to see: they may not search at all, and
		// are told so - once, through the gate, which audits it - instead
		// of being shown an empty answer page after page.
		if err = w.s.authz.Authorize(ctx, iam.Request{Principal: w.principal, Action: iam.ActionListAllMyBuckets}); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

// mayList reports whether the caller may list a bucket. It filters and does
// not gate: a bucket that is left out leaves no record.
func (w *searchWalk) mayList(ctx context.Context, bucketName string) (bool, error) {
	if allowed, known := w.may[bucketName]; known {
		return allowed, nil
	}
	// SUB-BUCKETS (PLAN C.3, C.4): this is the one place where the scope of
	// a search is decided. When sub-buckets exist, a caller who may not
	// list the bucket but may list one of its sub-buckets is to be let in
	// here for the keys of that sub-bucket only - the request then carries
	// the prefix, as a listing's will, and the walk below is confined to
	// that range of keys.
	//
	// In a search of all buckets the policy this decision reads is the one
	// the walk of this node's state comes by; nothing is asked of the
	// leader.
	allowed, err := w.s.authz.Allowed(ctx, listRequest(w.principal, bucketName))
	switch {
	case errors.Is(err, domain.ErrIntegrity):
		// The stored policy of this bucket is damaged, which the
		// authorizer has logged. A listing of the bucket is refused for
		// that; the search goes on without it rather than fail for every
		// bucket there is. Only a damaged document reads so: when the
		// policies cannot be read at all - damage to the store included -
		// the authorizer says domain.ErrUnavailable, and the search fails.
		allowed = false
	case err != nil:
		return false, err
	}
	w.may[bucketName] = allowed
	return allowed, nil
}

// full reports whether the call has as many results as it was asked for.
func (w *searchWalk) full() bool { return len(w.out.Results) >= w.limit }

// late reports whether the call has gone on for as long as a call may.
func (w *searchWalk) late() bool { return w.s.clock.Now().Sub(w.started) >= w.s.bounds.time }

// walk goes through buckets that are the caller's to see, in name order,
// beginning where the token says. It returns where it stopped, or nil when
// it reached the end.
//
// It is given no other bucket, so a stop - and the token, which anybody can
// read - never names one the caller may not see, and neither the number of
// pages nor what a page says it examined tells of the others. A bound never
// stops the call before it has done something, so that every call gets
// further than the one before.
func (w *searchWalk) walk(ctx context.Context, names []string, at searchToken) (*searchStop, error) {
	for _, name := range names {
		after, named := "", false
		if name == at.B {
			after, named = string(at.K), at.N
		}
		if w.full() || w.visited >= w.s.bounds.buckets || (w.visited > 0 && w.late()) {
			return &searchStop{bucket: name, key: after, named: named}, nil
		}
		w.visited++
		if w.listAll && !w.objectsOnly && !named && w.query.Match(name) {
			w.out.Results = append(w.out.Results, SearchResult{Kind: SearchResultBucket, Bucket: name})
		}
		if !w.may[name] {
			continue
		}
		last, finished, err := w.objects(ctx, name, after)
		if err != nil {
			return nil, err
		}
		if !finished {
			return &searchStop{bucket: name, key: last, named: true}, nil
		}
	}
	return nil, nil
}

// searchKey is what a walk of the index keeps of a version.
type searchKey struct {
	key        string
	size       int64
	modifiedAt time.Time
	etag       string
	marker     bool
}

// objects looks at the keys of a bucket after the given one, a chunk at a
// time. It returns the last key it looked at and whether it reached the end
// of the bucket.
func (w *searchWalk) objects(ctx context.Context, bucketName, after string) (last string, finished bool, err error) {
	last = after
	var chunk []searchKey
	visit := func(room int, more *bool) func(object.Version) ports.ScanControl {
		return func(v object.Version) ports.ScanControl {
			if len(chunk) == room {
				*more = true
				return ports.ScanControl{Stop: true}
			}
			chunk = append(chunk, searchKey{key: v.Key, size: v.Size, modifiedAt: v.ModifiedAt, etag: v.ETag, marker: v.DeleteMarker})
			return ports.ScanControl{}
		}
	}
	for {
		room := min(w.s.bounds.chunk, w.s.bounds.examined-w.out.Examined)
		if w.full() || room <= 0 || (w.chunks > 0 && w.late()) {
			return last, false, nil
		}
		start := ""
		if last != "" {
			// The first key after the last one looked at.
			start = last + "\x00"
		}
		// Each walk is a consistent read of its own, and the store's lock
		// is let go between two of them. The walk only copies what it
		// needs: the matching, which can take longer than the reading, is
		// done after the lock is let go.
		chunk = chunk[:0]
		more := false
		err = w.s.index.ScanObjects(ctx, bucketName, start, visit(room, &more))
		if errors.Is(err, domain.ErrBucketNotFound) {
			// Deleted since the buckets were listed.
			return last, true, nil
		}
		if err != nil {
			return last, false, err
		}
		w.chunks++
		for i, k := range chunk {
			w.out.Examined++
			last = k.key
			// A delete marker says that the object is gone: no result.
			if k.marker || !w.query.Match(k.key) {
				continue
			}
			w.out.Results = append(w.out.Results, SearchResult{
				Kind: SearchResultObject, Bucket: bucketName, Key: k.key, Size: k.size, ModifiedAt: k.modifiedAt, ETag: k.etag,
			})
			if w.full() {
				// The keys of the chunk after this one were not looked
				// at; the next call begins with them.
				return last, !more && i == len(chunk)-1, nil
			}
		}
		if !more {
			return last, true, nil
		}
	}
}
