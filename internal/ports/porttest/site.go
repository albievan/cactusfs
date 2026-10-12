package porttest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// SiteStore is the metadata of the deployment a SiteClient speaks to: what a
// test looks into to see what arrived.
type SiteStore interface {
	Metadata
	ports.ReplicationStore
}

// MemSite is a ports.SiteClient whose other deployment is a metadata store
// in the same process. It stores what it is sent as that deployment would -
// by the rules of its replica buckets - and keeps no bytes: a body is read
// to its end and counted. The zero value is not usable; call NewMemSite.
type MemSite struct {
	// Store is the other deployment.
	Store SiteStore

	mu     sync.Mutex
	serial uint64
	calls  []string
	bytes  int64
	// fail, when not nil, is what every call returns.
	fail error
}

var _ ports.SiteClient = (*MemSite)(nil)

// NewMemSite returns a client of the deployment whose metadata store is
// given.
func NewMemSite(store SiteStore) *MemSite {
	return &MemSite{Store: store}
}

// Fail makes every call from now on fail with err, as a deployment that
// cannot be reached or refuses the key does; nil lets them through again.
func (s *MemSite) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// Calls returns the calls that were made, in order, as "<call> <bucket>/<key>".
func (s *MemSite) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// BytesSent returns how many bytes of bodies were read.
func (s *MemSite) BytesSent() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// begin records a call and returns the failure a test asked for, if any.
func (s *MemSite) begin(call, bucketName, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, strings.TrimSuffix(call+" "+bucketName+"/"+key, "/"))
	return s.fail
}

// id returns an ID nothing else has.
func (s *MemSite) id(kind string) object.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serial++
	sum := sha256.Sum256(fmt.Appendf(nil, "memsite\x00%s\x00%d", kind, s.serial))
	return object.ID(sum[:object.IDSize])
}

// classed turns what the other deployment's store says into what its S3 API
// would answer, by class. That a version is there already is success.
func classed(err error) error {
	var failure *delivery.Error
	switch {
	case err == nil, errors.Is(err, domain.ErrConflict):
		return nil
	case errors.As(err, &failure):
		return err
	case errors.Is(err, domain.ErrBucketNotFound):
		return delivery.Failed(delivery.ClassNoSuchBucket, err)
	case errors.Is(err, domain.ErrInvalidBucketState):
		return delivery.Failed(delivery.ClassNotReplica, err)
	case errors.Is(err, domain.ErrQuotaExceeded):
		return delivery.Failed(delivery.ClassQuota, err)
	case errors.Is(err, domain.ErrAccessDenied), errors.Is(err, domain.ErrCredentialNotFound):
		return delivery.Failed(delivery.ClassAuth, err)
	case errors.Is(err, domain.ErrUnavailable), errors.Is(err, domain.ErrNotLeader):
		return delivery.Failed(delivery.ClassServer, err)
	case errors.Is(err, context.DeadlineExceeded):
		return delivery.Failed(delivery.ClassTimeout, err)
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrEntityTooLarge), errors.Is(err, domain.ErrObjectLocked),
		errors.Is(err, domain.ErrVersionNotFound), errors.Is(err, domain.ErrLockNotConfigured), errors.Is(err, domain.ErrUploadNotFound),
		errors.Is(err, domain.ErrPartNotFound), errors.Is(err, domain.ErrPartTooSmall), errors.Is(err, domain.ErrMethodNotAllowed):
		return delivery.Failed(delivery.ClassRejected, err)
	}
	return delivery.Failed(delivery.ClassInternal, err)
}

// drain reads a body to its end and reports whether it had size bytes.
func (s *MemSite) drain(body io.Reader, size int64) error {
	n, err := io.Copy(io.Discard, body)
	s.mu.Lock()
	s.bytes += n
	s.mu.Unlock()
	switch {
	case err != nil:
		return delivery.Failed(delivery.ClassInternal, err)
	case n != size:
		return delivery.Failed(delivery.ClassRejected, fmt.Errorf("%w: a body of %d bytes for %d", domain.ErrIncompleteBody, n, size))
	}
	return nil
}

// Check implements ports.SiteClient.
func (s *MemSite) Check(ctx context.Context, bucketName string) (ports.SiteBucket, error) {
	if err := s.begin("Check", bucketName, ""); err != nil {
		return ports.SiteBucket{}, classed(err)
	}
	b, err := s.Store.GetBucket(ctx, bucketName)
	if err != nil {
		return ports.SiteBucket{}, classed(err)
	}
	return ports.SiteBucket{Versioning: b.Versioning, ReplicaOf: b.ReplicaFrom}, nil
}

// arriving returns the version as the other deployment commits it: with
// what travels, and with stored data of its own.
func (s *MemSite) arriving(v object.Version) object.Version {
	out := CloneVersion(v)
	out.DataID = s.id("data")
	out.Encryption, out.Layout = object.Encryption{}, object.Layout{}
	out.CreatedBy, out.Replication, out.Parts = "key/replication", "", 0
	return out
}

// PutReplica implements ports.SiteClient.
func (s *MemSite) PutReplica(ctx context.Context, v object.Version, body io.Reader) error {
	if err := s.begin("PutReplica", v.Bucket, v.Key); err != nil {
		return classed(err)
	}
	// A body that is cut short commits nothing.
	if err := s.drain(body, v.Size); err != nil {
		return err
	}
	_, err := s.Store.PutVersion(ctx, s.arriving(v))
	return classed(err)
}

// CreateUpload implements ports.SiteClient.
func (s *MemSite) CreateUpload(ctx context.Context, v object.Version) (string, error) {
	if err := s.begin("CreateUpload", v.Bucket, v.Key); err != nil {
		return "", classed(err)
	}
	if v.Replica == nil {
		return "", delivery.Failed(delivery.ClassRejected, fmt.Errorf("%w: an upload of no source", domain.ErrInvalidArgument))
	}
	upload := object.Upload{
		Bucket: v.Bucket, Key: v.Key, ID: s.id("upload"), InitiatedAt: v.ModifiedAt, Initiator: "key/replication",
		ContentType: v.ContentType, ContentEncoding: v.ContentEncoding, ContentDisposition: v.ContentDisposition,
		ContentLanguage: v.ContentLanguage, CacheControl: v.CacheControl, UserMetadata: v.UserMetadata, Tags: v.Tags, Lock: v.Lock,
		Encryption: object.Encryption{KeyMode: object.KeyModePartHKDF}, ReplicaFrom: v.Replica.From,
	}
	if err := s.Store.CreateUpload(ctx, upload); err != nil {
		return "", classed(err)
	}
	return upload.ID.String(), nil
}

// PutPart implements ports.SiteClient.
func (s *MemSite) PutPart(ctx context.Context, bucketName, key, uploadID string, number int, size int64, body io.Reader) (string, error) {
	if err := s.begin("PutPart", bucketName, key); err != nil {
		return "", classed(err)
	}
	id, err := object.ParseID(uploadID)
	if err != nil {
		return "", delivery.Failed(delivery.ClassRejected, err)
	}
	if err := s.drain(body, size); err != nil {
		return "", err
	}
	part := object.Part{Number: number, DataID: s.id("part"), Size: size, ModifiedAt: time.Unix(0, 0).UTC()}
	part.ETag = part.DataID.String()
	if _, err := s.Store.PutPart(ctx, bucketName, key, id, part); err != nil {
		return "", classed(err)
	}
	return part.ETag, nil
}

// CompleteReplica implements ports.SiteClient.
func (s *MemSite) CompleteReplica(ctx context.Context, v object.Version, uploadID string, parts []ports.SitePart) error {
	if err := s.begin("CompleteReplica", v.Bucket, v.Key); err != nil {
		return classed(err)
	}
	id, err := object.ParseID(uploadID)
	if err != nil {
		return delivery.Failed(delivery.ClassRejected, err)
	}
	stored, err := s.Store.ListParts(ctx, v.Bucket, v.Key, id, 0, object.MaxParts)
	if err != nil {
		return classed(err)
	}
	completion := ports.Completion{Bucket: v.Bucket, Key: v.Key, ID: id, VersionID: v.VersionID, ETag: v.ETag, At: v.ModifiedAt, CreatedBy: "key/replication", Replica: v.Replica}
	seal := byte(0)
	for _, listed := range parts {
		seal++
		part := object.Part{Number: listed.Number, ETag: listed.ETag, Seal: sealOf(seal)}
		for _, candidate := range stored {
			if candidate.Number == listed.Number {
				part.DataID = candidate.DataID
			}
		}
		completion.Parts = append(completion.Parts, part)
	}
	_, err = s.Store.CompleteUpload(ctx, completion)
	return classed(err)
}

// AbortUpload implements ports.SiteClient.
func (s *MemSite) AbortUpload(ctx context.Context, bucketName, key, uploadID string) error {
	if err := s.begin("AbortUpload", bucketName, key); err != nil {
		return classed(err)
	}
	id, err := object.ParseID(uploadID)
	if err != nil {
		return delivery.Failed(delivery.ClassRejected, err)
	}
	_, err = s.Store.AbortUpload(ctx, bucketName, key, id)
	return classed(err)
}

// ListReplicaUploads implements ports.SiteClient.
func (s *MemSite) ListReplicaUploads(ctx context.Context, bucketName, key string) ([]string, error) {
	if err := s.begin("ListReplicaUploads", bucketName, key); err != nil {
		return nil, classed(err)
	}
	var ids []string
	err := s.Store.ScanUploads(ctx, bucketName, ports.UploadCursor{Key: key}, func(u object.Upload) ports.ScanControl {
		if u.Key != key {
			return ports.ScanControl{Stop: true}
		}
		// Only what a source began: an upload of a client of the other
		// deployment is not this deployment's to see.
		if u.ReplicaFrom != "" {
			ids = append(ids, u.ID.String())
		}
		return ports.ScanControl{}
	})
	if err != nil {
		return nil, classed(err)
	}
	return ids, nil
}

// PutMarker implements ports.SiteClient.
func (s *MemSite) PutMarker(ctx context.Context, v object.Version) error {
	if err := s.begin("PutMarker", v.Bucket, v.Key); err != nil {
		return classed(err)
	}
	if !v.DeleteMarker || v.Replica == nil {
		return delivery.Failed(delivery.ClassRejected, fmt.Errorf("%w: not a delete marker of a source", domain.ErrInvalidArgument))
	}
	_, err := s.Store.DeleteObject(ctx, v.Bucket, v.Key, ports.DeleteOptions{MarkerID: v.VersionID, At: v.ModifiedAt, By: "key/replication", Replica: v.Replica})
	return classed(err)
}

// HeadVersion implements ports.SiteClient.
func (s *MemSite) HeadVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (string, bool, error) {
	if err := s.begin("HeadVersion", bucketName, key); err != nil {
		return "", false, classed(err)
	}
	v, err := s.Store.GetVersion(ctx, bucketName, key, ref)
	switch {
	case errors.Is(err, domain.ErrVersionNotFound):
		return "", false, nil
	case err != nil:
		return "", false, classed(err)
	}
	return v.ETag, true, nil
}

// PutTags implements ports.SiteClient.
func (s *MemSite) PutTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) error {
	if err := s.begin("PutTags", bucketName, key); err != nil {
		return classed(err)
	}
	_, err := s.Store.SetTags(ctx, bucketName, key, ref, tags)
	return classed(err)
}

// PutRetention implements ports.SiteClient.
func (s *MemSite) PutRetention(ctx context.Context, bucketName, key string, ref object.VersionRef, r *object.Retention) error {
	if err := s.begin("PutRetention", bucketName, key); err != nil {
		return classed(err)
	}
	_, err := s.Store.SetRetention(ctx, bucketName, key, ref, r, false)
	return classed(err)
}

// PutLegalHold implements ports.SiteClient.
func (s *MemSite) PutLegalHold(ctx context.Context, bucketName, key string, ref object.VersionRef, on bool) error {
	if err := s.begin("PutLegalHold", bucketName, key); err != nil {
		return classed(err)
	}
	_, err := s.Store.SetLegalHold(ctx, bucketName, key, ref, on)
	return classed(err)
}

// SiteClientContract is the behaviour every client of another deployment
// must have. open returns a client and the metadata store of the deployment
// it speaks to - new and empty, with every member knowing entry schema 5 -
// which the contract prepares and looks into. The client sends as the
// deployment whose ID is source; whatever stands between the client and the
// store (an S3 API, a key, a policy) lets it replicate into every bucket.
func SiteClientContract(t *testing.T, source string, open func(t *testing.T) (ports.SiteClient, SiteStore)) {
	t.Helper()
	ctx := context.Background()
	prepare := func(t *testing.T) (ports.SiteClient, SiteStore) {
		t.Helper()
		client, store := open(t)
		for _, name := range []string{"replica", "vault", "plain", "writable"} {
			if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: contractTime, ObjectLock: name == "vault"}); err != nil {
				t.Fatalf("CreateBucket %s: %v", name, err)
			}
		}
		for _, name := range []string{"replica", "writable"} {
			if err := store.SetBucketVersioning(ctx, name, bucket.VersioningEnabled); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"replica", "vault"} {
			if err := store.SetBucketReplica(ctx, name, source, auditOf("admin:PutBucketReplicaMode", name)); err != nil {
				t.Fatalf("SetBucketReplica %s: %v", name, err)
			}
		}
		return client, store
	}
	// sent is a version as its source sends it.
	sent := func(bucketName, key string, fill byte, seq uint64, size int64) object.Version {
		v := SampleVersion(bucketName, key, fill)
		v.Size, v.Seq, v.Versioned = size, seq, true
		v.Tags = map[string]string{"stage": "final"}
		v.Replica = &object.Replica{From: source, Seq: seq}
		return v
	}
	body := func(size int64) io.Reader { return io.LimitReader(zeroes{}, size) }
	class := func(err error) delivery.Class {
		t.Helper()
		var failure *delivery.Error
		if err != nil && !errors.As(err, &failure) {
			t.Errorf("an error of a client that says no class: %v", err)
		}
		return delivery.ClassOf(err)
	}

	t.Run("the bucket of the other deployment is asked about", func(t *testing.T) {
		t.Parallel()
		client, _ := prepare(t)
		for name, want := range map[string]ports.SiteBucket{
			"replica":  {Versioning: bucket.VersioningEnabled, ReplicaOf: source},
			"writable": {Versioning: bucket.VersioningEnabled},
			"plain":    {},
		} {
			if got, err := client.Check(ctx, name); err != nil || got != want {
				t.Errorf("Check(%s) = %+v, %v; want %+v", name, got, err, want)
			}
		}
		if _, err := client.Check(ctx, "missing"); class(err) != delivery.ClassNoSuchBucket {
			t.Errorf("Check of a bucket that is not there = %v", err)
		}
	})

	t.Run("a version arrives as the version it is at its source", func(t *testing.T) {
		t.Parallel()
		client, store := prepare(t)
		v := sent("replica", "photos/a.jpg", 1, 300, 1234)
		if err := client.PutReplica(ctx, v, body(v.Size)); err != nil {
			t.Fatalf("PutReplica: %v", err)
		}
		got, err := store.GetVersion(ctx, "replica", v.Key, v.Ref())
		if err != nil {
			t.Fatalf("the version at the other deployment: %v", err)
		}
		if got.VersionID != v.VersionID || got.Seq != 300 || !got.ModifiedAt.Equal(v.ModifiedAt) || got.Size != 1234 || got.ETag != v.ETag ||
			got.Replication != object.ReplicationReplica || got.ContentType != v.ContentType || got.UserMetadata["owner"] != "albie" || got.Tags["stage"] != "final" {
			t.Errorf("the version arrived as %+v", got)
		}
		// Whether the other deployment has a version is asked without
		// reading it.
		if etag, found, err := client.HeadVersion(ctx, "replica", v.Key, v.Ref()); err != nil || !found || etag != v.ETag {
			t.Errorf("HeadVersion = %q, %v, %v", etag, found, err)
		}
		if _, found, err := client.HeadVersion(ctx, "replica", v.Key, object.VersionRef{ID: sampleVersionID(99)}); err != nil || found {
			t.Errorf("HeadVersion of a version that is not there = %v, %v", found, err)
		}
		// Sent again, it is there: that is success, and nothing changed.
		if err := client.PutReplica(ctx, v, body(v.Size)); err != nil {
			t.Errorf("a version sent a second time = %v", err)
		}
		// An older version that arrives later does not become the current
		// one.
		older := sent("replica", "photos/a.jpg", 2, 100, 10)
		if err := client.PutReplica(ctx, older, body(older.Size)); err != nil {
			t.Fatal(err)
		}
		if current, err := store.GetObject(ctx, "replica", v.Key); err != nil || current.VersionID != v.VersionID {
			t.Errorf("the current version = %s, %v; want %s", current.VersionID, err, v.VersionID)
		}
		// A body that is cut short commits nothing.
		short := sent("replica", "photos/short.jpg", 3, 400, 1000)
		if err := client.PutReplica(ctx, short, body(999)); err == nil || class(err) == delivery.ClassNone {
			t.Errorf("a body cut short = %v", err)
		}
		if _, err := store.GetVersion(ctx, "replica", short.Key, short.Ref()); !errors.Is(err, domain.ErrVersionNotFound) {
			t.Errorf("the version of a body cut short: %v", err)
		}
		// Later changes of a version follow it.
		if err := client.PutTags(ctx, "replica", v.Key, v.Ref(), map[string]string{"stage": "archived"}); err != nil {
			t.Fatalf("PutTags: %v", err)
		}
		if got, err := store.GetVersion(ctx, "replica", v.Key, v.Ref()); err != nil || got.Tags["stage"] != "archived" {
			t.Errorf("the tags at the other deployment = %v, %v", got.Tags, err)
		}
	})

	t.Run("a delete marker arrives as a marker", func(t *testing.T) {
		t.Parallel()
		client, store := prepare(t)
		v := sent("replica", "k", 1, 10, 5)
		if err := client.PutReplica(ctx, v, body(v.Size)); err != nil {
			t.Fatal(err)
		}
		marker := object.Version{
			Bucket: "replica", Key: "k", VersionID: sampleVersionID(2), ModifiedAt: contractTime, DeleteMarker: true, Versioned: true,
			Replica: &object.Replica{From: source, Seq: 20},
		}
		for range 2 { // and a second time: it is there
			if err := client.PutMarker(ctx, marker); err != nil {
				t.Fatalf("PutMarker: %v", err)
			}
		}
		got, err := store.GetVersion(ctx, "replica", "k", marker.Ref())
		if err != nil || !got.DeleteMarker || got.Seq != 20 || !got.ModifiedAt.Equal(contractTime) || got.Replication != object.ReplicationReplica {
			t.Errorf("the marker arrived as %+v, %v", got, err)
		}
		if _, err := store.GetObject(ctx, "replica", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("the object behind the marker: %v", err)
		}
	})

	t.Run("a version of parts arrives through a multipart upload", func(t *testing.T) {
		t.Parallel()
		client, store := prepare(t)
		const first, second = object.MinPartSize, 321
		v := sent("replica", "large", 1, 50, first+second)
		// What an attempt that was cut off left behind is found and ended.
		stale, err := client.CreateUpload(ctx, v)
		if err != nil {
			t.Fatalf("CreateUpload: %v", err)
		}
		if _, err := client.PutPart(ctx, "replica", "large", stale, 1, first, body(first)); err != nil {
			t.Fatalf("PutPart: %v", err)
		}
		found, err := client.ListReplicaUploads(ctx, "replica", "large")
		if err != nil || len(found) != 1 || found[0] != stale {
			t.Fatalf("ListReplicaUploads = %v, %v; want %s", found, err, stale)
		}
		if none, err := client.ListReplicaUploads(ctx, "replica", "other"); err != nil || len(none) != 0 {
			t.Errorf("ListReplicaUploads of another key = %v, %v", none, err)
		}
		if err := client.AbortUpload(ctx, "replica", "large", stale); err != nil {
			t.Fatalf("AbortUpload: %v", err)
		}
		if left, err := client.ListReplicaUploads(ctx, "replica", "large"); err != nil || len(left) != 0 {
			t.Errorf("after the abort ListReplicaUploads = %v, %v", left, err)
		}

		id, err := client.CreateUpload(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		var parts []ports.SitePart
		for i, size := range []int64{first, second} {
			etag, err := client.PutPart(ctx, "replica", "large", id, i+1, size, body(size))
			if err != nil || etag == "" {
				t.Fatalf("PutPart %d = %q, %v", i+1, etag, err)
			}
			parts = append(parts, ports.SitePart{Number: i + 1, ETag: etag})
		}
		// A part cut short is no part.
		if _, err := client.PutPart(ctx, "replica", "large", id, 3, 100, body(99)); err == nil {
			t.Error("a part cut short was taken")
		}
		if err := client.CompleteReplica(ctx, v, id, parts); err != nil {
			t.Fatalf("CompleteReplica: %v", err)
		}
		got, err := store.GetVersion(ctx, "replica", "large", v.Ref())
		if err != nil || got.VersionID != v.VersionID || got.Seq != 50 || got.Size != first+second || !got.Multipart() || got.Parts != 2 ||
			got.Replication != object.ReplicationReplica || !got.ModifiedAt.Equal(v.ModifiedAt) {
			t.Errorf("the version of parts arrived as %+v, %v", got, err)
		}
	})

	t.Run("a lock travels and is the other deployment's to keep", func(t *testing.T) {
		t.Parallel()
		client, store := prepare(t)
		v := sent("vault", "ledger", 1, 10, 5)
		v.Lock = object.Lock{Retention: &object.Retention{Mode: object.Governance, Until: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}}
		if err := client.PutReplica(ctx, v, body(v.Size)); err != nil {
			t.Fatalf("PutReplica with a retention: %v", err)
		}
		got, err := store.GetVersion(ctx, "vault", "ledger", v.Ref())
		if err != nil || got.Lock.Retention == nil || !got.Lock.Retention.Until.Equal(v.Lock.Retention.Until) {
			t.Fatalf("the retention at the other deployment = %+v, %v", got.Lock, err)
		}
		longer := &object.Retention{Mode: object.Governance, Until: v.Lock.Retention.Until.Add(time.Hour)}
		if err := client.PutRetention(ctx, "vault", "ledger", v.Ref(), longer); err != nil {
			t.Fatalf("PutRetention: %v", err)
		}
		if err := client.PutLegalHold(ctx, "vault", "ledger", v.Ref(), true); err != nil {
			t.Fatalf("PutLegalHold: %v", err)
		}
		got, err = store.GetVersion(ctx, "vault", "ledger", v.Ref())
		if err != nil || !got.Lock.LegalHold || !got.Lock.Retention.Until.Equal(longer.Until) {
			t.Errorf("the lock at the other deployment = %+v, %v", got.Lock, err)
		}
		// What the other deployment's lock refuses is refused for the
		// version, and does not stall the target.
		shorter := &object.Retention{Mode: object.Governance, Until: v.Lock.Retention.Until}
		if err := client.PutRetention(ctx, "vault", "ledger", v.Ref(), shorter); class(err) != delivery.ClassRejected {
			t.Errorf("shortening a retention there = %v, want the class %s", err, delivery.ClassRejected)
		}
	})

	t.Run("what the other deployment refuses has a class", func(t *testing.T) {
		t.Parallel()
		client, store := prepare(t)
		// A bucket that is no replica takes nothing of a source: not one
		// that keeps versions and takes writes, and not one that keeps
		// none.
		for _, name := range []string{"writable", "plain"} {
			v := sent(name, "k", 1, 10, 5)
			if err := client.PutReplica(ctx, v, body(v.Size)); class(err) != delivery.ClassNotReplica {
				t.Errorf("a version sent to %s = %v, want the class %s", name, err, delivery.ClassNotReplica)
			}
			if _, err := store.GetObject(ctx, name, "k"); !errors.Is(err, domain.ErrObjectNotFound) {
				t.Errorf("a refused version is in %s: %v", name, err)
			}
		}
		missing := sent("missing", "k", 2, 10, 5)
		if err := client.PutReplica(ctx, missing, body(missing.Size)); class(err) != delivery.ClassNoSuchBucket {
			t.Errorf("a version sent to a bucket that is not there = %v", err)
		}
		// A full bucket there is the other deployment protecting itself.
		usage, ok := store.(ports.BucketUsageStore)
		if !ok {
			t.Fatal("the store of the other deployment has no quotas")
		}
		if err := usage.SetBucketQuota(ctx, "replica", 100, auditOf("admin:PutBucketQuota", "replica")); err != nil {
			t.Fatal(err)
		}
		over := sent("replica", "big", 3, 10, 101)
		if err := client.PutReplica(ctx, over, body(over.Size)); class(err) != delivery.ClassQuota {
			t.Errorf("a version past the quota there = %v, want the class %s", err, delivery.ClassQuota)
		}
		// No failure says more than its class.
		err := client.PutReplica(ctx, missing, body(missing.Size))
		if err == nil || err.Error() != "delivery failed: "+string(delivery.ClassNoSuchBucket) {
			t.Errorf("the text of a failure = %v", err)
		}
	})
}

// zeroes is a body of as many zero bytes as are read from it.
type zeroes struct{}

// Read implements io.Reader.
func (zeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// DeliveryMark is in what the leader of the script of DeliveryStatusContract
// says where only a word belongs. A reader that hands it on has failed.
const DeliveryMark = "https://MARK-7c1e.example/hook: connection refused"

// The leaders of the script of DeliveryStatusContract.
const (
	// DeliveryLeaderAnswering answers with DeliveryStatusScript.Statuses.
	DeliveryLeaderAnswering = "answering"
	// DeliveryLeaderOld is of a release that cannot be asked.
	DeliveryLeaderOld = "old"
	// DeliveryLeaderSilent is reached and never answers.
	DeliveryLeaderSilent = "silent"
	// DeliveryLeaderGone is no leader at all, or one that cannot be
	// reached.
	DeliveryLeaderGone = "gone"
)

// DeliveryStatusScript is what the node that leads does behind a reader
// under test.
type DeliveryStatusScript struct {
	// Leader is one of the DeliveryLeader... constants.
	Leader string
	// Statuses are what the leader says, exactly as it says them: not in
	// order, and with DeliveryMark where it pleases.
	Statuses []ports.TargetStatus
}

// DeliveryStatusReader is a ports.DeliveryStatusReader whose leader does as
// a script says.
type DeliveryStatusReader struct {
	script DeliveryStatusScript
}

var _ ports.DeliveryStatusReader = (*DeliveryStatusReader)(nil)

// ScriptedDeliveryStatusReader returns a reader whose leader does as the
// script says.
func ScriptedDeliveryStatusReader(script DeliveryStatusScript) *DeliveryStatusReader {
	return &DeliveryStatusReader{script: script}
}

// shownState returns a state as it may be shown: a word, or - for what can
// be the words of an error - that the leader cannot say.
func shownState(state string) string {
	if delivery.ShownClass(state) != delivery.Class(state) || state == "" {
		return ports.DeliveryUnsupported
	}
	return state
}

// DeliveryStatus implements ports.DeliveryStatusReader.
func (r *DeliveryStatusReader) DeliveryStatus(ctx context.Context, targets []string) ([]ports.TargetStatus, error) {
	switch r.script.Leader {
	case DeliveryLeaderOld:
		return nil, fmt.Errorf("%w: the leader", domain.ErrNotSupported)
	case DeliveryLeaderSilent:
		<-ctx.Done()
		return nil, fmt.Errorf("the leader: %w", ctx.Err())
	case DeliveryLeaderAnswering:
	default:
		return nil, fmt.Errorf("%w: no leader", domain.ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("the leader: %w", err)
	}
	var out []ports.TargetStatus
	for _, status := range r.script.Statuses {
		if len(targets) > 0 && !slices.Contains(targets, status.Name) {
			continue
		}
		// What arrives where a word belongs is shown only when it is one.
		status.State, status.LastErrorClass = shownState(status.State), delivery.ShownClass(string(status.LastErrorClass))
		out = append(out, status)
	}
	slices.SortFunc(out, func(a, b ports.TargetStatus) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// DeliveryStatusContract is the behaviour every reader of the status of
// delivery must have. open returns a reader behind which the node that
// leads does as the script says.
func DeliveryStatusContract(t *testing.T, open func(t *testing.T, script DeliveryStatusScript) ports.DeliveryStatusReader) {
	t.Helper()
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	statuses := []ports.TargetStatus{
		{Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryStalled, Pending: 40, OldestPendingAt: at, LastErrorClass: delivery.ClassAuth, Lost: 2, Failed: 3, ResyncNeeded: true},
		{Name: "hook", Kind: delivery.KindWebhook, State: ports.DeliveryOK, LastSuccessAt: at.Add(time.Minute), Rejected: 5},
		{Name: "bus", Kind: delivery.KindNATS, State: ports.DeliveryRetrying, Pending: 1, OldestPendingAt: at, LastErrorClass: delivery.ClassNoStream},
		// A leader that is wrong: the words of an error where a class
		// and a state belong.
		{Name: "wrong", Kind: delivery.KindWebhook, State: DeliveryMark, LastErrorClass: DeliveryMark},
		// A leader of a later release: words this one does not know.
		{Name: "later", Kind: delivery.KindWebhook, State: "draining", LastErrorClass: "throttled"},
	}
	answering := DeliveryStatusScript{Leader: DeliveryLeaderAnswering, Statuses: statuses}

	t.Run("the leader's answer is handed on in name order", func(t *testing.T) {
		t.Parallel()
		reader := open(t, answering)
		got, err := reader.DeliveryStatus(context.Background(), nil)
		if err != nil || len(got) != 5 {
			t.Fatalf("DeliveryStatus = %+v, %v", got, err)
		}
		names := make([]string, 0, len(got))
		for _, status := range got {
			names = append(names, status.Name)
		}
		if !slices.Equal(names, []string{"bus", "hook", "later", "site-b", "wrong"}) {
			t.Errorf("the targets are in the order %v", names)
		}
		site := got[3]
		if site.Kind != delivery.KindS3 || site.State != ports.DeliveryStalled || site.Pending != 40 || !site.OldestPendingAt.Equal(at) ||
			site.LastErrorClass != delivery.ClassAuth || site.Lost != 2 || site.Failed != 3 || !site.ResyncNeeded || !site.LastSuccessAt.IsZero() {
			t.Errorf("the status of the other deployment = %+v", site)
		}
		if hook := got[1]; hook.State != ports.DeliveryOK || hook.Rejected != 5 || !hook.LastSuccessAt.Equal(at.Add(time.Minute)) ||
			hook.LastErrorClass != delivery.ClassNone || !hook.OldestPendingAt.IsZero() {
			t.Errorf("the status of the webhook = %+v", hook)
		}
		// A word of a later release is shown as it is.
		if later := got[2]; later.State != "draining" || later.LastErrorClass != "throttled" {
			t.Errorf("words of a later release are shown as %q and %q", later.State, later.LastErrorClass)
		}
		// The words of an error, and an address, are not shown at all.
		if text := fmt.Sprintf("%+v", got); strings.Contains(text, "MARK-7c1e") || strings.Contains(text, "connection refused") {
			t.Errorf("what the leader said where a word belongs is handed on: %s", text)
		}
		if wrong := got[4]; wrong.LastErrorClass != delivery.ClassInternal || wrong.State != ports.DeliveryUnsupported {
			t.Errorf("a status that is no word is shown as %q and %q", wrong.State, wrong.LastErrorClass)
		}
	})

	t.Run("targets are asked for by name", func(t *testing.T) {
		t.Parallel()
		reader := open(t, answering)
		got, err := reader.DeliveryStatus(context.Background(), []string{"site-b", "missing", "bus"})
		if err != nil || len(got) != 2 || got[0].Name != "bus" || got[1].Name != "site-b" {
			t.Errorf("DeliveryStatus of two targets and one that is none = %+v, %v", got, err)
		}
		if none, err := reader.DeliveryStatus(context.Background(), []string{"missing"}); err != nil || len(none) != 0 {
			t.Errorf("DeliveryStatus of a name that is no target's = %+v, %v", none, err)
		}
		// A leader with no targets has nothing to say, which is an answer.
		empty := open(t, DeliveryStatusScript{Leader: DeliveryLeaderAnswering})
		if none, err := empty.DeliveryStatus(context.Background(), nil); err != nil || len(none) != 0 {
			t.Errorf("DeliveryStatus without targets = %+v, %v", none, err)
		}
	})

	t.Run("why there is no answer is said", func(t *testing.T) {
		t.Parallel()
		old := open(t, DeliveryStatusScript{Leader: DeliveryLeaderOld, Statuses: statuses})
		if got, err := old.DeliveryStatus(context.Background(), nil); !errors.Is(err, domain.ErrNotSupported) || len(got) != 0 {
			t.Errorf("a leader of an older release = %+v, %v; want %v", got, err, domain.ErrNotSupported)
		}
		gone := open(t, DeliveryStatusScript{Leader: DeliveryLeaderGone, Statuses: statuses})
		if got, err := gone.DeliveryStatus(context.Background(), nil); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrNotSupported) || len(got) != 0 {
			t.Errorf("no leader = %+v, %v; want %v", got, err, domain.ErrUnavailable)
		}
		silent := open(t, DeliveryStatusScript{Leader: DeliveryLeaderSilent, Statuses: statuses})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		began := time.Now()
		got, err := silent.DeliveryStatus(ctx, nil)
		if !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
			t.Errorf("a leader that never answers = %+v, %v; want %v", got, err, context.DeadlineExceeded)
		}
		if waited := time.Since(began); waited > 10*time.Second {
			t.Errorf("the call returned %s after its context ended", waited)
		}
	})
}
