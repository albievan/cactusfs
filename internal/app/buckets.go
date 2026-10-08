package app

import (
	"context"
	"fmt"
	"strconv"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// BucketService holds the bucket use cases.
type BucketService struct {
	buckets ports.BucketStore
	authz   ports.Authorizer
	clock   ports.Clock
	// auditor records buckets made and removed; nil records nothing.
	auditor *Auditor
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
	if err := s.buckets.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: s.clock.Now().UTC()}); err != nil {
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
	b := bucket.Bucket{Name: name, CreatedAt: s.clock.Now().UTC(), ObjectLock: true}
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

func (s *BucketService) authorize(ctx context.Context, principal iam.Principal, action iam.Action, name string) error {
	return s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: action, Resource: iam.Resource{Bucket: name}})
}
