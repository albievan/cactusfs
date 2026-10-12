package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// BucketService holds the bucket use cases.
type BucketService struct {
	buckets ports.BucketStore
	authz   ports.Authorizer
	clock   ports.Clock
	// auditor records buckets made and removed; nil records nothing.
	auditor *Auditor
	// local, when set, reads the configuration of a bucket from this
	// node's own copy of the state, without asking the metadata leader.
	local BucketPolicySource
	// policies and activity, when both are set, are what ListDetailed reads
	// beyond the list of buckets: the policies of all buckets from this
	// node's state, page by page, and the record of the latest use of each.
	policies ports.BucketPolicies
	activity ports.BucketActivityStore
}

// WithDetails gives the service what ListDetailed reads beyond the list of
// buckets: where the policies of all buckets are walked page by page, as a
// search walks them, and the record of the latest uploads and downloads.
// Without it ListDetailed says of no bucket more than List does. It is
// called once, when the node is assembled.
func (s *BucketService) WithDetails(policies ports.BucketPolicies, activity ports.BucketActivityStore) *BucketService {
	s.policies, s.activity = policies, activity
	return s
}

// WithLocalConfig gives the service a way to read a bucket's configuration
// from this node's copy of the state. The cross-origin rules that are sent
// to browsers before anybody has authenticated are read that way: a caller
// without credentials must not be able to make the node ask the leader, and
// rules that are a moment old harm nobody.
func (s *BucketService) WithLocalConfig(source BucketPolicySource) *BucketService {
	s.local = source
	return s
}

// WithAuditor makes the service record every bucket it makes or removes in
// the audit trail. It is called once, when the node is assembled.
func (s *BucketService) WithAuditor(auditor *Auditor) *BucketService {
	s.auditor = auditor
	return s
}

// audited records a change to a bucket that has been made.
func (s *BucketService) audited(ctx context.Context, principal iam.Principal, action iam.Action, name string) {
	if s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, action, iam.Resource{Bucket: name}, audit.Succeeded, nil))
	}
}

// NewBucketService returns a BucketService.
func NewBucketService(buckets ports.BucketStore, authz ports.Authorizer, clock ports.Clock) *BucketService {
	return &BucketService{buckets: buckets, authz: authz, clock: clock}
}

// Create creates a bucket.
func (s *BucketService) Create(ctx context.Context, principal iam.Principal, name string) error {
	if err := s.authorize(ctx, principal, iam.ActionCreateBucket, name); err != nil {
		return fmt.Errorf("bucket.Create: %w", err)
	}
	if err := bucket.ValidateName(name); err != nil {
		return fmt.Errorf("bucket.Create: %w", err)
	}
	// Who created it is recorded as the audit trail names actors, like who
	// uploaded a version (ADR-0019). The store leaves it out while a member
	// of the cluster does not know the field yet.
	b := bucket.Bucket{Name: name, CreatedAt: s.clock.Now().UTC(), CreatedBy: actorOf(principal)}
	if err := s.buckets.CreateBucket(ctx, b); err != nil {
		return fmt.Errorf("bucket.Create: %w", err)
	}
	s.audited(ctx, principal, iam.ActionCreateBucket, name)
	return nil
}

// CreateWithObjectLock creates a bucket whose versions can be locked. Such a
// bucket keeps versions from the start and for good: that is decided here
// and cannot be decided later.
func (s *BucketService) CreateWithObjectLock(ctx context.Context, principal iam.Principal, name string) error {
	if err := s.authorize(ctx, principal, iam.ActionCreateBucket, name); err != nil {
		return fmt.Errorf("bucket.CreateWithObjectLock: %w", err)
	}
	if err := bucket.ValidateName(name); err != nil {
		return fmt.Errorf("bucket.CreateWithObjectLock: %w", err)
	}
	b := bucket.Bucket{Name: name, CreatedAt: s.clock.Now().UTC(), CreatedBy: actorOf(principal), ObjectLock: true}
	if err := s.buckets.CreateBucket(ctx, b); err != nil {
		return fmt.Errorf("bucket.CreateWithObjectLock: %w", err)
	}
	if s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionCreateBucket, iam.Resource{Bucket: name}, audit.Succeeded,
			map[string]string{"object_lock": "true"}))
	}
	return nil
}

// ObjectLock returns the Object Lock configuration of a bucket: whether its
// versions can be locked, and the retention it gives new ones. The error
// wraps domain.ErrLockNotConfigured for a bucket created without.
func (s *BucketService) ObjectLock(ctx context.Context, principal iam.Principal, name string) (bucket.Bucket, error) {
	if err := s.authorize(ctx, principal, iam.ActionGetBucketObjectLock, name); err != nil {
		return bucket.Bucket{}, fmt.Errorf("bucket.ObjectLock: %w", err)
	}
	b, err := s.buckets.GetBucket(ctx, name)
	if err != nil {
		return bucket.Bucket{}, fmt.Errorf("bucket.ObjectLock: %w", err)
	}
	if !b.ObjectLock {
		return bucket.Bucket{}, fmt.Errorf("bucket.ObjectLock: %w: bucket %s was not created with Object Lock", domain.ErrLockNotConfigured, name)
	}
	return b, nil
}

// SetDefaultRetention sets the retention a bucket gives its new versions;
// nil removes it. Versions that exist keep what they have. The change is
// audited.
func (s *BucketService) SetDefaultRetention(ctx context.Context, principal iam.Principal, name string, d *bucket.DefaultRetention) error {
	if err := s.authorize(ctx, principal, iam.ActionPutBucketObjectLock, name); err != nil {
		return fmt.Errorf("bucket.SetDefaultRetention: %w", err)
	}
	details := map[string]string{"default_retention": "none"}
	if d != nil {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("bucket.SetDefaultRetention: %w", err)
		}
		details = map[string]string{"mode": string(d.Mode), "days": strconv.Itoa(d.Days), "years": strconv.Itoa(d.Years)}
	}
	if err := s.buckets.SetBucketDefaultRetention(ctx, name, d); err != nil {
		return fmt.Errorf("bucket.SetDefaultRetention: %w", err)
	}
	if s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutBucketObjectLock, iam.Resource{Bucket: name}, audit.Succeeded, details))
	}
	return nil
}

// Delete deletes an empty bucket.
func (s *BucketService) Delete(ctx context.Context, principal iam.Principal, name string) error {
	if err := s.authorize(ctx, principal, iam.ActionDeleteBucket, name); err != nil {
		return fmt.Errorf("bucket.Delete: %w", err)
	}
	if err := s.buckets.DeleteBucket(ctx, name); err != nil {
		return fmt.Errorf("bucket.Delete: %w", err)
	}
	s.audited(ctx, principal, iam.ActionDeleteBucket, name)
	return nil
}

// Get returns a bucket, which proves that it exists and that the principal
// may list it.
func (s *BucketService) Get(ctx context.Context, principal iam.Principal, name string) (bucket.Bucket, error) {
	if err := s.authorize(ctx, principal, iam.ActionListBucket, name); err != nil {
		return bucket.Bucket{}, fmt.Errorf("bucket.Get: %w", err)
	}
	b, err := s.buckets.GetBucket(ctx, name)
	if err != nil {
		return bucket.Bucket{}, fmt.Errorf("bucket.Get: %w", err)
	}
	return b, nil
}

// Versioning returns whether a bucket keeps the versions of its objects.
func (s *BucketService) Versioning(ctx context.Context, principal iam.Principal, name string) (bucket.Versioning, error) {
	if err := s.authorize(ctx, principal, iam.ActionGetBucketVersioning, name); err != nil {
		return "", fmt.Errorf("bucket.Versioning: %w", err)
	}
	b, err := s.buckets.GetBucket(ctx, name)
	if err != nil {
		return "", fmt.Errorf("bucket.Versioning: %w", err)
	}
	return b.Versioning, nil
}

// SetVersioning makes a bucket keep the versions of its objects, or stop
// making new ones. Versions that exist stay. The change is audited: with
// versioning suspended, an overwrite or a delete destroys data again.
func (s *BucketService) SetVersioning(ctx context.Context, principal iam.Principal, name string, state bucket.Versioning) error {
	if err := s.authorize(ctx, principal, iam.ActionPutBucketVersioning, name); err != nil {
		return fmt.Errorf("bucket.SetVersioning: %w", err)
	}
	if err := s.buckets.SetBucketVersioning(ctx, name, state); err != nil {
		return fmt.Errorf("bucket.SetVersioning: %w", err)
	}
	if s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutBucketVersioning, iam.Resource{Bucket: name}, audit.Succeeded,
			map[string]string{"status": string(state)}))
	}
	return nil
}

// Location proves that a bucket exists and that the principal may ask where
// it is. A deployment has a single location.
func (s *BucketService) Location(ctx context.Context, principal iam.Principal, name string) error {
	if err := s.authorize(ctx, principal, iam.ActionGetBucketLocation, name); err != nil {
		return fmt.Errorf("bucket.Location: %w", err)
	}
	if _, err := s.buckets.GetBucket(ctx, name); err != nil {
		return fmt.Errorf("bucket.Location: %w", err)
	}
	return nil
}

// List returns every bucket in name order.
func (s *BucketService) List(ctx context.Context, principal iam.Principal) ([]bucket.Bucket, error) {
	if err := s.authorize(ctx, principal, iam.ActionListAllMyBuckets, ""); err != nil {
		return nil, fmt.Errorf("bucket.List: %w", err)
	}
	buckets, err := s.buckets.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("bucket.List: %w", err)
	}
	return buckets, nil
}

// bucketRequest is the question of whether a principal may do something to
// a bucket. authorize asks it as the gate of every use case here, and
// ListDetailed as a filter of what the list carries: it is made here, once,
// so that the two cannot come to ask different questions.
func bucketRequest(principal iam.Principal, action iam.Action, name string) iam.Request {
	return iam.Request{Principal: principal, Action: action, Resource: iam.Resource{Bucket: name}}
}

func (s *BucketService) authorize(ctx context.Context, principal iam.Principal, action iam.Action, name string) error {
	return s.authz.Authorize(ctx, bucketRequest(principal, action, name))
}

// BucketDetail is a bucket of the list and what the caller may see of it
// beyond its name and its age.
type BucketDetail struct {
	// Bucket is the bucket. Its CreatedBy is empty unless AccessIncluded,
	// and then still empty when who created it was not recorded.
	Bucket bucket.Bucket
	// AccessIncluded says that the caller may see who uses the bucket:
	// Bucket.CreatedBy and Activity are then what is known. It is false for
	// a caller who does not hold admin:GetObjectAccess on the bucket and
	// when this node keeps no such record.
	AccessIncluded bool
	// KeysIncluded says that the rows of Activity name their objects: the
	// caller may list the bucket as well. Never set without AccessIncluded.
	KeysIncluded bool
	// Activity is the latest uploads into and downloads from the bucket;
	// the zero value unless AccessIncluded, and without the keys of the
	// objects unless KeysIncluded. A row of an object the caller does not
	// hold admin:GetObjectAccess on is left out, so there may be fewer rows
	// than are kept, or none.
	Activity bucket.Activity
	// PolicyIncluded says that the caller may read the policy of the
	// bucket and that it could be read: Policy is then its summary, nil for
	// a bucket without a policy.
	PolicyIncluded bool
	Policy         *iam.BucketPolicySummary
}

// ListDetailed returns every bucket in name order, as List does and allowed
// as List is, and of each bucket what the caller may see beyond that: who
// created it and its latest uploads and downloads, and a summary of its
// policy (ADR-0019).
//
// The list is the operation, and s3:ListAllMyBuckets its gate. What a bucket
// of the list carries is not a gate of anything, so it is asked with Allowed
// - the decision that is neither audited nor logged, which filters what a
// result carries and gates nothing: nobody asked for anything by name that
// was refused. Three questions a bucket, each the one the use case that
// answers for a single bucket asks: admin:GetObjectAccess on the bucket
// (accessRequest, as ObjectService.Access and ListWithAccess) for the creator
// and for whether anything of the activity is shown; s3:ListBucket
// (listRequest, as a listing) for the keys in the activity; s3:GetBucketPolicy
// (bucketRequest, as Policy) for the summary of the policy.
//
// And one more for every row of the activity that was read: a row is of an
// object, so it is shown only where Access would answer for that object -
// admin:GetObjectAccess on its key, the same accessRequest - and is left out
// whole otherwise, who and when with the key: a policy that denies the action
// by key holds here as it holds there. The action on the bucket stays
// required, which is the stricter reading: who holds it on keys only is shown
// no activity at all.
//
// What it costs does not grow by a question to the leader for each bucket:
// after the one consistent list, the policies of all buckets are walked once
// in this node's state, page by page - by the authorizer as it decides, and
// the summaries are of the policies that walk parsed (policySummary) - and
// the activity is read in one call for all the buckets it may be shown of.
// The rows, six a bucket at the most, are decided in memory: no bucket policy
// binds an admin action. A bucket whose stored policy is damaged is shown
// without its keys and without a summary, as a search leaves it out rather
// than fail for every bucket; any other failure to read what may be shown
// fails the call.
func (s *BucketService) ListDetailed(ctx context.Context, principal iam.Principal) ([]BucketDetail, error) {
	buckets, err := s.List(ctx, principal)
	if err != nil {
		return nil, err
	}
	// The store lists in name order; the walk of the policies depends on
	// it.
	slices.SortFunc(buckets, func(a, b bucket.Bucket) int { return cmp.Compare(a.Name, b.Name) })
	out := make([]BucketDetail, len(buckets))
	for i, b := range buckets {
		// Who created a bucket is for who may see who uses it: it is put
		// back below for the buckets the caller holds that on.
		b.CreatedBy = ""
		out[i] = BucketDetail{Bucket: b}
	}
	if s.policies == nil || s.activity == nil {
		return out, nil
	}
	// One reading of a bucket's policy serves the whole call, and from
	// here on the authorizer takes the policies from a walk of them all,
	// begun after the list above. The summaries are of that walk.
	ctx = WithRequestScope(ctx)
	primeBucketPolicies(ctx, s.policies)
	summaries := &policySummaries{all: s.policies}
	var shown []int
	for i := range out {
		// Deciding is work in this node's memory, a page of policies at
		// the most; a caller who has gone is not decided for.
		if err = ctx.Err(); err != nil {
			return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
		}
		d, name := &out[i], out[i].Bucket.Name
		if d.AccessIncluded, err = s.authz.Allowed(ctx, accessRequest(principal, name, "")); err != nil {
			return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
		}
		if d.AccessIncluded {
			d.Bucket.CreatedBy = buckets[i].CreatedBy
			shown = append(shown, i)
			// The stricter reading: a key is shown where a listing of
			// the bucket would show it.
			if d.KeysIncluded, err = s.mayShow(ctx, listRequest(principal, name)); err != nil {
				return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
			}
		}
		if d.PolicyIncluded, err = s.mayShow(ctx, bucketRequest(principal, iam.ActionGetBucketPolicy, name)); err != nil {
			return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
		}
		if d.PolicyIncluded {
			if d.Policy, d.PolicyIncluded, err = s.policySummary(ctx, summaries, name); err != nil {
				return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
			}
		}
	}
	// The records are read after the list, in one call - one for every
	// ports.MaxBucketActivities buckets: a bucket that was deleted in
	// between has the zero record.
	for start := 0; start < len(shown); start += ports.MaxBucketActivities {
		chunk := shown[start:min(len(shown), start+ports.MaxBucketActivities)]
		names := make([]string, len(chunk))
		for n, i := range chunk {
			names[n] = out[i].Bucket.Name
		}
		records, err := s.activity.BucketActivities(ctx, names)
		if err != nil {
			return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
		}
		if len(records) != len(names) {
			return nil, fmt.Errorf("bucket.ListDetailed: %w: %d records for %d buckets", domain.ErrIntegrity, len(records), len(names))
		}
		for n, i := range chunk {
			if out[i].Activity, err = s.activityShown(ctx, principal, names[n], records[n], out[i].KeysIncluded); err != nil {
				return nil, fmt.Errorf("bucket.ListDetailed: %w", err)
			}
		}
	}
	return out, nil
}

// mayShow reports whether a part of what is known of a bucket is the
// caller's to see. It filters what the list carries and gates nothing: a
// part that is left out leaves no record.
func (s *BucketService) mayShow(ctx context.Context, req iam.Request) (bool, error) {
	allowed, err := s.authz.Allowed(ctx, req)
	if errors.Is(err, domain.ErrIntegrity) {
		// The stored policy of this bucket is damaged, which the
		// authorizer has logged. The request it might have denied is
		// refused for that, and here the part is left out; the list goes
		// on rather than fail for every bucket there is. When the policies
		// cannot be read at all the authorizer says domain.ErrUnavailable,
		// and the list fails.
		return false, nil
	}
	return allowed, err
}

// activityShown returns the record of a bucket as the caller is shown it. A
// row is kept only when the caller may see who uploaded and who read the
// object it names: the question ObjectService.Access asks of that object
// (accessRequest), asked of the key the row has - before the key is taken
// away - so that a row Access would refuse is left out whole, and leaves no
// record of a refusal. The rows that stay keep their order, and are without
// the keys of their objects for who may not list the bucket. The result
// shares nothing with the record.
func (s *BucketService) activityShown(ctx context.Context, principal iam.Principal, name string, a bucket.Activity, keys bool) (bucket.Activity, error) {
	var out bucket.Activity
	for _, row := range a.Uploads {
		allowed, err := s.authz.Allowed(ctx, accessRequest(principal, name, row.Key))
		if err != nil {
			return bucket.Activity{}, err
		}
		if !allowed {
			continue
		}
		if !keys {
			row.Key = ""
		}
		out.Uploads = append(out.Uploads, row)
	}
	for _, row := range a.Downloads {
		allowed, err := s.authz.Allowed(ctx, accessRequest(principal, name, row.Key))
		if err != nil {
			return bucket.Activity{}, err
		}
		if !allowed {
			continue
		}
		if !keys {
			row.Key = ""
		}
		out.Downloads = append(out.Downloads, row)
	}
	return out, nil
}

// policySummarizer is an authorizer that can say what the policy of a bucket
// is from the walk of all policies it decides the requests of a primed
// request scope by. *Authorizer is one.
type policySummarizer interface {
	walkedSummary(ctx context.Context, bucketName string) (summary *iam.BucketPolicySummary, readable bool, err error)
}

// policySummary returns the summary of the policy of a bucket of the list,
// nil when it has none; readable is false when the stored policy of this one
// bucket is damaged. The authorizer of a node answers from the policy it
// parsed, or remembers under the hash of the document its walk has just
// read, so that no policy is parsed a second time for its summary; for an
// authorizer that cannot, the policies are walked and parsed here.
func (s *BucketService) policySummary(ctx context.Context, own *policySummaries, name string) (summary *iam.BucketPolicySummary, readable bool, err error) {
	if walked, ok := s.authz.(policySummarizer); ok {
		return walked.walkedSummary(ctx, name)
	}
	return own.of(ctx, name)
}

// policySummaries is a walk of the policies of all buckets that ListDetailed
// makes for itself when its authorizer is not one that summarizes what it
// has walked (policySummarizer). It keeps the documents of the page that was
// read last.
//
// It belongs to one call, which asks about the buckets one after another in
// name order.
type policySummaries struct {
	all ports.BucketPolicies
	// The buckets from the name from up to, not including, next were read
	// last, and page holds the policy document of each of them that has
	// one. When done, there is no bucket from next on.
	started    bool
	from, next string
	done       bool
	page       map[string][]byte
}

// read reads the page of the policies that begins at from, and remembers it.
func (p *policySummaries) read(ctx context.Context, from string) (string, error) {
	page := map[string][]byte{}
	next, err := p.all.BucketPolicies(ctx, from, func(name string, document []byte) error {
		// The document is lent for the call.
		page[name] = bytes.Clone(document)
		return nil
	})
	if err != nil {
		return next, err
	}
	p.started, p.from, p.next, p.done, p.page = true, from, next, next == "", page
	return next, nil
}

// covers reports whether the page read last says whether a bucket has a
// policy.
func (p *policySummaries) covers(bucketName string) bool {
	return p.started && bucketName >= p.from && (p.done || bucketName < p.next)
}

// of returns the summary of the policy of a bucket, nil when it has none,
// reading on until the walk has come to that bucket. readable is false when
// the stored policy of this one bucket is damaged; the error says that a
// page of the policies could not be read, which says nothing of any bucket.
func (p *policySummaries) of(ctx context.Context, bucketName string) (summary *iam.BucketPolicySummary, readable bool, err error) {
	for !p.covers(bucketName) {
		// On from where the walk is; a bucket it has left behind, which a
		// list in name order does not ask about, is read by itself.
		from := p.next
		if !p.started {
			from = ""
		} else if bucketName < p.from {
			from = bucketName
		}
		next, err := p.read(ctx, from)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, false, fmt.Errorf("bucket policies: %w", ctx.Err())
		case errors.Is(err, domain.ErrIntegrity):
			// The store could not be read. That is not the damaged policy
			// of one bucket, which leaves the others shown.
			return nil, false, fmt.Errorf("%w: the policies of the buckets cannot be read: %s", domain.ErrUnavailable, err.Error())
		case err != nil:
			return nil, false, fmt.Errorf("bucket policies: %w", err)
		case next != "" && next <= from:
			return nil, false, fmt.Errorf("%w: the walk of the bucket policies went back from %q to %q", domain.ErrUnavailable, from, next)
		}
	}
	policy, damaged := storedBucketPolicy(p.page[bucketName], bucketName)
	switch {
	case damaged != nil:
		return nil, false, nil
	case policy == nil:
		return nil, true, nil
	}
	made := policy.Summary()
	return &made, true, nil
}

// Policy returns the policy of a bucket as it was given, or nil when the
// bucket has none.
func (s *BucketService) Policy(ctx context.Context, principal iam.Principal, name string) ([]byte, error) {
	return s.config(ctx, principal, iam.ActionGetBucketPolicy, name, bucket.ConfigPolicy, "bucket.Policy")
}

// SetPolicy replaces the policy of a bucket; nil removes it. The change is
// audited: a bucket policy is the one thing that can open a bucket to
// callers who have not authenticated.
func (s *BucketService) SetPolicy(ctx context.Context, principal iam.Principal, name string, document []byte) error {
	action := iam.ActionPutBucketPolicy
	if document == nil {
		action = iam.ActionDeleteBucketPolicy
	}
	if err := s.authorize(ctx, principal, action, name); err != nil {
		return fmt.Errorf("bucket.SetPolicy: %w", err)
	}
	details := map[string]string{"policy": "removed"}
	if document != nil {
		policy, err := iam.ParseBucketPolicy(document, name)
		if err != nil {
			return fmt.Errorf("bucket.SetPolicy: %w", err)
		}
		details = map[string]string{"statements": strconv.Itoa(len(policy.Document.Statements)), "open_to_anyone": strconv.FormatBool(policy.OpenToAnyone())}
	}
	if err := s.buckets.SetBucketConfig(ctx, name, bucket.ConfigPolicy, document); err != nil {
		return fmt.Errorf("bucket.SetPolicy: %w", err)
	}
	if s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, action, iam.Resource{Bucket: name}, audit.Succeeded, details))
	}
	return nil
}

// CORS returns the cross-origin rules of a bucket, or nil when it has none.
func (s *BucketService) CORS(ctx context.Context, principal iam.Principal, name string) (*bucket.CORS, error) {
	raw, err := s.config(ctx, principal, iam.ActionGetBucketCORS, name, bucket.ConfigCORS, "bucket.CORS")
	if err != nil || raw == nil {
		return nil, err
	}
	return decodeCORS(raw)
}

func decodeCORS(raw []byte) (*bucket.CORS, error) {
	var rules bucket.CORS
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("%w: stored CORS configuration: %w", domain.ErrIntegrity, err)
	}
	return &rules, nil
}

// SetCORS replaces the cross-origin rules of a bucket; nil removes them.
func (s *BucketService) SetCORS(ctx context.Context, principal iam.Principal, name string, rules *bucket.CORS) error {
	if err := s.authorize(ctx, principal, iam.ActionPutBucketCORS, name); err != nil {
		return fmt.Errorf("bucket.SetCORS: %w", err)
	}
	var document []byte
	if rules != nil {
		if err := rules.Validate(); err != nil {
			return fmt.Errorf("bucket.SetCORS: %w", err)
		}
		var err error
		if document, err = json.Marshal(rules); err != nil {
			return fmt.Errorf("bucket.SetCORS: %w", err)
		}
	}
	if err := s.buckets.SetBucketConfig(ctx, name, bucket.ConfigCORS, document); err != nil {
		return fmt.Errorf("bucket.SetCORS: %w", err)
	}
	if s.auditor != nil {
		details := map[string]string{"cors": "removed"}
		if rules != nil {
			details = map[string]string{"rules": strconv.Itoa(len(rules.Rules))}
		}
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutBucketCORS, iam.Resource{Bucket: name}, audit.Succeeded, details))
	}
	return nil
}

// CORSOf returns the cross-origin rules of a bucket for the answer to a
// browser, which is given before anybody has authenticated: it asks no
// permission and says nothing about whether the bucket exists. It returns
// nil for a bucket without rules, for one that is not there, and when the
// rules cannot be read.
func (s *BucketService) CORSOf(ctx context.Context, name string) *bucket.CORS {
	var source BucketPolicySource = s.buckets
	if s.local != nil {
		source = s.local
	}
	raw, err := source.GetBucketConfig(ctx, name, bucket.ConfigCORS)
	if err != nil || len(raw) == 0 {
		return nil
	}
	rules, err := decodeCORS(raw)
	if err != nil {
		return nil
	}
	return rules
}

// Tags returns the tags of a bucket.
func (s *BucketService) Tags(ctx context.Context, principal iam.Principal, name string) (map[string]string, error) {
	raw, err := s.config(ctx, principal, iam.ActionGetBucketTagging, name, bucket.ConfigTagging, "bucket.Tags")
	if err != nil || raw == nil {
		return nil, err
	}
	var tags map[string]string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, fmt.Errorf("bucket.Tags: %w: stored tags: %w", domain.ErrIntegrity, err)
	}
	return tags, nil
}

// SetTags replaces the tags of a bucket; none removes them.
func (s *BucketService) SetTags(ctx context.Context, principal iam.Principal, name string, tags map[string]string) error {
	if err := s.authorize(ctx, principal, iam.ActionPutBucketTagging, name); err != nil {
		return fmt.Errorf("bucket.SetTags: %w", err)
	}
	if err := object.ValidateTags(tags, object.MaxBucketTags); err != nil {
		return fmt.Errorf("bucket.SetTags: %w", err)
	}
	var document []byte
	if len(tags) > 0 {
		var err error
		if document, err = json.Marshal(tags); err != nil {
			return fmt.Errorf("bucket.SetTags: %w", err)
		}
	}
	if err := s.buckets.SetBucketConfig(ctx, name, bucket.ConfigTagging, document); err != nil {
		return fmt.Errorf("bucket.SetTags: %w", err)
	}
	if s.auditor != nil {
		details := map[string]string{"tags": strconv.Itoa(len(tags))}
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutBucketTagging, iam.Resource{Bucket: name}, audit.Succeeded, details))
	}
	return nil
}

// config reads one configuration document of a bucket for a caller who may.
func (s *BucketService) config(ctx context.Context, principal iam.Principal, action iam.Action, name, kind, operation string) ([]byte, error) {
	if err := s.authorize(ctx, principal, action, name); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	raw, err := s.buckets.GetBucketConfig(ctx, name, kind)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return raw, nil
}

// Lifecycle returns the lifecycle rules of a bucket, or nil when it has none.
func (s *BucketService) Lifecycle(ctx context.Context, principal iam.Principal, name string) (*bucket.Lifecycle, error) {
	raw, err := s.config(ctx, principal, iam.ActionGetLifecycle, name, bucket.ConfigLifecycle, "bucket.Lifecycle")
	if err != nil || raw == nil {
		return nil, err
	}
	rules, err := decodeLifecycle(raw)
	if err != nil {
		return nil, fmt.Errorf("bucket.Lifecycle: %w", err)
	}
	return rules, nil
}

func decodeLifecycle(raw []byte) (*bucket.Lifecycle, error) {
	var rules bucket.Lifecycle
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("%w: stored lifecycle configuration: %s", domain.ErrIntegrity, err.Error())
	}
	return &rules, nil
}

// SetLifecycle replaces the lifecycle rules of a bucket; nil removes them.
// The change is audited: rules delete data.
func (s *BucketService) SetLifecycle(ctx context.Context, principal iam.Principal, name string, rules *bucket.Lifecycle) error {
	if err := s.authorize(ctx, principal, iam.ActionPutLifecycle, name); err != nil {
		return fmt.Errorf("bucket.SetLifecycle: %w", err)
	}
	var document []byte
	details := map[string]string{"lifecycle": "removed"}
	if rules != nil {
		if err := rules.Validate(); err != nil {
			return fmt.Errorf("bucket.SetLifecycle: %w", err)
		}
		var err error
		if document, err = json.Marshal(rules); err != nil {
			return fmt.Errorf("bucket.SetLifecycle: %w", err)
		}
		details = map[string]string{"rules": strconv.Itoa(len(rules.Rules))}
	}
	if err := s.buckets.SetBucketConfig(ctx, name, bucket.ConfigLifecycle, document); err != nil {
		return fmt.Errorf("bucket.SetLifecycle: %w", err)
	}
	if s.auditor != nil {
		s.auditor.Succeeded(ctx, s.auditor.Record(principal, iam.ActionPutLifecycle, iam.Resource{Bucket: name}, audit.Succeeded, details))
	}
	return nil
}
