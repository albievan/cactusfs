package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Replication between sites, at the destination (ADR-0026). A replica bucket
// takes new versions and delete markers from one source deployment, and from
// nobody else - that is the state's to decide, in apply. What is decided
// here is who may make a bucket a replica or promote it, and who may write a
// copy: s3:ReplicateObject (s3:ReplicateDelete for a marker), which no
// wildcard grants. The protocol of the copy - which headers, signed how - is
// the inbound adapter's.

// ReplicaMode says whether a bucket is the replica of another deployment's.
type ReplicaMode struct {
	// Source is the ID of the deployment the bucket takes its versions
	// from; empty when the bucket is no replica and takes writes.
	Source string
}

// Replica reports whether the bucket is a replica.
func (m ReplicaMode) Replica() bool {
	return m.Source != ""
}

// ReplicationService holds the use cases of replica mode (ADR-0026): reading
// it, and making a bucket a replica of another deployment or promoting it.
// Each passes the single authorizer with an admin: action on the bucket, so
// that a bucket's own administrator, who holds s3:*, can do neither; each
// change carries its audit record in the entry that makes it.
type ReplicationService struct {
	buckets ports.BucketStore
	store   ports.ReplicationStore
	authz   ports.Authorizer
	auditor *Auditor
}

// ReplicationServiceConfig lists what a ReplicationService depends on.
type ReplicationServiceConfig struct {
	Buckets ports.BucketStore
	Store   ports.ReplicationStore
	Authz   ports.Authorizer
	// Auditor builds the records of changes; without one every change is
	// refused.
	Auditor *Auditor
}

// NewReplicationService returns a ReplicationService.
func NewReplicationService(cfg ReplicationServiceConfig) *ReplicationService {
	return &ReplicationService{buckets: cfg.Buckets, store: cfg.Store, authz: cfg.Authz, auditor: cfg.Auditor}
}

// ReplicaMode returns whether a bucket is a replica, and of which
// deployment. It needs admin:GetBucketReplicaMode on the bucket. The error
// wraps domain.ErrAccessDenied and domain.ErrBucketNotFound.
func (r *ReplicationService) ReplicaMode(ctx context.Context, principal iam.Principal, bucketName string) (ReplicaMode, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetBucketReplicaMode, bucketName)); err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.ReplicaMode: %w", err)
	}
	mode, err := r.read(ctx, bucketName)
	if err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.ReplicaMode: %w", err)
	}
	return mode, nil
}

// SetReplicaMode makes a bucket the replica of the deployment source, or -
// with an empty source - promotes it: from then on it takes writes again,
// and nothing more from its source. It returns the mode the bucket has
// afterwards. It needs admin:PutBucketReplicaMode on the bucket, and the
// change carries its audit record in its entry.
//
// Naming the source the bucket already has, and promoting a bucket that is
// no replica, change nothing and are no error. The error wraps
// domain.ErrAccessDenied; domain.ErrInvalidArgument for a source that is no
// deployment ID (nothing is proposed); domain.ErrBucketNotFound;
// domain.ErrInvalidBucketState when the bucket does not keep versions or has
// a replication configuration of its own; domain.ErrConflict while its usage
// is not exact; domain.ErrBucketNotEmpty when it holds a version; and
// domain.ErrUnavailable until every member of the metadata group knows
// replication, or when there is no audit trail to record the change in.
func (r *ReplicationService) SetReplicaMode(ctx context.Context, principal iam.Principal, bucketName, source string) (ReplicaMode, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutBucketReplicaMode, bucketName)); err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w", err)
	}
	if source != "" {
		if err := object.ValidateReplicaSource(source); err != nil {
			return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w", err)
		}
	}
	if r.auditor == nil {
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	details := map[string]string{"source": source}
	if source == "" {
		details = map[string]string{"promoted": "true"}
	}
	record := r.auditor.Record(principal, iam.ActionPutBucketReplicaMode, iam.Resource{Bucket: bucketName}, audit.Succeeded, details)
	if err := r.store.SetBucketReplica(ctx, bucketName, source, record); err != nil {
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: %w", err)
	}
	// What the bucket is now is read after the change, without a second
	// authorization: the caller may change the mode and need not read it.
	mode, err := r.read(ctx, bucketName)
	if err != nil {
		// The mode was set; a repeat sets the same again.
		return ReplicaMode{}, fmt.Errorf("replication.SetReplicaMode: the mode was set, but cannot be read: %w", err)
	}
	return mode, nil
}

// read returns the mode of a bucket without asking anyone.
func (r *ReplicationService) read(ctx context.Context, bucketName string) (ReplicaMode, error) {
	b, err := r.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return ReplicaMode{}, err
	}
	return ReplicaMode{Source: b.ReplicaFrom}, nil
}

// ReplicaWrite says that a write is the copy of a version or of a delete
// marker of another deployment (ADR-0026): what the source says of it. The
// inbound adapter reads it from the request; this package decides whether
// the caller may write it and whether the bucket takes it.
type ReplicaWrite struct {
	// Source is the ID of the deployment the version comes from.
	Source string
	// Version is the ID the version has there, or the null version.
	Version object.VersionRef
	// Seq is the sequence the version has there, from 1 to
	// object.MaxReplicaSeq.
	Seq uint64
	// ModifiedAt is when the version was made there. It is shown, never
	// decided on.
	ModifiedAt time.Time
	// ETag is the entity tag the version has there, without quotes: the
	// one the destination computes must be the same. Empty for a delete
	// marker, which has none.
	ETag string
}

// replica is what the write says of itself to the store.
func (w ReplicaWrite) replica() *object.Replica {
	return &object.Replica{From: w.Source, Seq: w.Seq, Null: w.Version.Null}
}

// validate reports whether the write names a source, a version and a
// sequence a replica can have, and - but for a marker - an entity tag. The
// error wraps domain.ErrInvalidArgument.
func (w ReplicaWrite) validate(marker bool) error {
	if err := w.replica().Validate(); err != nil {
		return err
	}
	switch {
	case !w.Version.Null && w.Version.ID.IsZero():
		return fmt.Errorf("%w: a replica names the version it is the copy of", domain.ErrInvalidArgument)
	case w.ModifiedAt.IsZero():
		return fmt.Errorf("%w: a replica says when its version was made", domain.ErrInvalidArgument)
	case marker && w.ETag != "":
		return fmt.Errorf("%w: a delete marker has no entity tag", domain.ErrInvalidArgument)
	case !marker && w.ETag == "":
		return fmt.Errorf("%w: a replica says the entity tag of its version", domain.ErrInvalidArgument)
	}
	return nil
}

// verify wraps the verification of a copy's content: the entity tag the
// destination computes must be the one the source says the version has,
// and a copy whose content is another is refused before anything of it is
// durable. The error wraps domain.ErrBadDigest.
func (w ReplicaWrite) verify(next func() (string, error)) func() (string, error) {
	return func() (string, error) {
		if next == nil {
			return "", fmt.Errorf("%w: the content of a replica cannot be verified", domain.ErrBadDigest)
		}
		etag, err := next()
		if err != nil {
			return "", err
		}
		if etag != w.ETag {
			return "", fmt.Errorf("%w: the content of the replica is not the version its source has", domain.ErrBadDigest)
		}
		return etag, nil
	}
}

// errReplicaSSE refuses a copy that asks for an encryption of its own -
// SSE-KMS or SSE-C; SSE-S3 is the default and asks for nothing: no key
// crosses between deployments, and a copy is encrypted under the
// destination's default (ADR-0026).
var errReplicaSSE = fmt.Errorf("%w: a replica is encrypted as this deployment encrypts, and asks for no encryption of its own", domain.ErrInvalidArgument)

// replicaAction is the action a copy needs: s3:ReplicateDelete for a delete
// marker, s3:ReplicateObject for everything else.
func replicaAction(marker bool) iam.Action {
	if marker {
		return iam.ActionReplicateDelete
	}
	return iam.ActionReplicateObject
}

// admitSource decides, before anything is written, whether the bucket takes
// copies from source: it must be the replica of exactly that deployment. The
// store decides again when the copy is committed; this spares the shards of
// a copy that cannot be committed. The error wraps domain.ErrBucketNotFound
// and domain.ErrInvalidBucketState.
func (s *ObjectService) admitSource(ctx context.Context, bucketName, source string) error {
	b, err := s.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return err
	}
	if b.ReplicaFrom != source {
		// Whether the bucket is the replica of another deployment, or of
		// none, is not told: either way it takes nothing from this one.
		return fmt.Errorf("%w: bucket %s is no replica of that deployment", domain.ErrInvalidBucketState, bucketName)
	}
	return nil
}

// admitReplica decides whether a copy may be written - by whom, of what,
// into which bucket - before anything is written: the caller holds the
// replicate action on the object, the write says what a copy must say, and
// the bucket is the replica of its source.
func (s *ObjectService) admitReplica(ctx context.Context, principal iam.Principal, resource iam.Resource, w ReplicaWrite, marker bool) error {
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: replicaAction(marker), Resource: resource}); err != nil {
		return err
	}
	if err := w.validate(marker); err != nil {
		return err
	}
	return s.admitSource(ctx, resource.Bucket, w.Source)
}

// admitReplicaUpload decides whether a multipart upload may be begun as the
// copy of a version of source - or a question of that source answered: the
// caller holds s3:ReplicateObject on the resource, source is a deployment
// ID, and the bucket is its replica.
func (s *ObjectService) admitReplicaUpload(ctx context.Context, principal iam.Principal, resource iam.Resource, source string) error {
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource}); err != nil {
		return err
	}
	if err := object.ValidateReplicaSource(source); err != nil {
		return err
	}
	return s.admitSource(ctx, resource.Bucket, source)
}

// authorizeUpload is the gate of what is done to a multipart upload in
// progress - a part sent, the upload aborted, its parts listed - asked
// before the upload is read, which would tell whoever asks whether the
// bucket is there. An upload that is a copy (object.Upload.ReplicaFrom) is
// its source's: each of these needs s3:ReplicateObject, and the ordinary
// action (s3:PutObject, s3:AbortMultipartUpload,
// s3:ListMultipartUploadParts) does not do; any other upload needs the
// ordinary action. Which of the two the caller is asked for first is chosen
// by whether it holds the replicate action - Authorizer.Allowed, a question
// that is not recorded; the gate is Authorize either way, and
// authorizeUploadOf asks for the other action too when the upload turns
// out to need it. replicates says which was asked.
func (s *ObjectService) authorizeUpload(ctx context.Context, principal iam.Principal, resource iam.Resource, ordinary iam.Action) (replicates bool, err error) {
	replicates, err = s.authz.Allowed(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource})
	if err != nil {
		return false, err
	}
	action := ordinary
	if replicates {
		action = iam.ActionReplicateObject
	}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: action, Resource: resource}); err != nil {
		return false, err
	}
	return replicates, nil
}

// authorizeUploadOf completes the gate of authorizeUpload once the upload is
// known: a copy is touched only by who replicates, any other upload only by
// who holds the ordinary action. A caller that holds both is asked for both.
func (s *ObjectService) authorizeUploadOf(
	ctx context.Context, principal iam.Principal, resource iam.Resource, upload object.Upload, ordinary iam.Action, replicates bool,
) error {
	copied := upload.ReplicaFrom != ""
	switch {
	case copied == replicates:
		return nil
	case copied:
		return s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource})
	default:
		return s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: ordinary, Resource: resource})
	}
}

// admitPart decides, once the upload is known, whether this part may join
// it. A part of a copy is sent by its source and by nothing else: its payload
// must be covered by the request's signature (refused 403, and audited, as
// the adapter refuses an unsigned copy), and a copy of another object - an
// UploadPartCopy - is never one. A part of an upload that is no copy is not
// taken in a bucket that has become a replica since the upload was begun:
// it could never be completed there.
func (s *ObjectService) admitPart(ctx context.Context, principal iam.Principal, resource iam.Resource, upload object.Upload, in UploadPartInput) error {
	if upload.ReplicaFrom != "" {
		if in.copied {
			return fmt.Errorf("%w: the part of a replica upload is sent by its source, never copied", domain.ErrInvalidArgument)
		}
		if !in.PayloadSigned {
			if s.auditor != nil {
				s.auditor.Denied(ctx, iam.Request{Principal: principal, Action: iam.ActionReplicateObject, Resource: resource}, "ReplicaPayloadNotSigned")
			}
			return fmt.Errorf("%w: the payload of a part of a replica upload is not signed", domain.ErrAccessDenied)
		}
		return nil
	}
	b, err := s.buckets.GetBucket(ctx, resource.Bucket)
	if err != nil {
		return err
	}
	if b.ReplicaFrom != "" {
		return fmt.Errorf("%w: bucket %s is a replica and takes parts of its source's uploads only", domain.ErrInvalidBucketState, resource.Bucket)
	}
	return nil
}

// auditReplica writes into the audit trail that a copy was committed: a
// version or a marker with an ID, a time and a sequence of the caller's
// choosing is something only a replicate action allows, and each is said,
// as a retention that was lifted is. Nothing of the object but its name is
// in the record.
func (s *ObjectService) auditReplica(ctx context.Context, principal iam.Principal, resource iam.Resource, operation string, w ReplicaWrite, marker bool) {
	if s.auditor == nil {
		return
	}
	s.auditor.Succeeded(ctx, s.auditor.Record(principal, replicaAction(marker), resource, audit.Succeeded, map[string]string{
		"operation": operation, "source": w.Source, "version": w.Version.String(), "seq": strconv.FormatUint(w.Seq, 10),
	}))
}

// The questions a source asks before it sends (ADR-0026, "What the source
// asks before it sends, and what it is told"): whether the bucket is its
// replica and keeps versions, whether a version is there and with which
// entity tag, which replica uploads of a key it left behind, and the abort
// of one of them. Each is judged as s3:ReplicateObject - no read, listing or
// abort action is asked, and none is enough - and refused, as a copy is,
// unless the bucket is the replica of exactly that source. Each answers no
// more than the source needs: never a byte of an object, nothing of a
// version but its entity tag, never an upload that is not that source's.

// ReplicaBucket answers the bucket probe of a source: the versioning of a
// bucket that is the replica of that source. That it answers at all says
// the bucket is that source's replica; nothing of what the bucket holds is
// told. It needs s3:ReplicateObject on the bucket. The error wraps
// domain.ErrAccessDenied, domain.ErrInvalidArgument for a source that is no
// deployment ID, domain.ErrBucketNotFound, and domain.ErrInvalidBucketState
// for a bucket that is no replica of that source; any other conflict is
// domain.ErrUnavailable, never domain.ErrConflict (questionFailed).
func (s *ObjectService) ReplicaBucket(ctx context.Context, principal iam.Principal, bucketName, source string) (bucket.Versioning, error) {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: bucketName}, source); err != nil {
		return "", fmt.Errorf("object.ReplicaBucket: %w", questionFailed(err))
	}
	b, err := s.buckets.GetBucket(ctx, bucketName)
	if err != nil {
		return "", fmt.Errorf("object.ReplicaBucket: %w", questionFailed(err))
	}
	return b.Versioning, nil
}

// questionFailed keeps the one conflict a question is refused with -
// domain.ErrInvalidBucketState, a bucket that is no replica of the source -
// apart from every other: the source reads a 409 to a HEAD, which has no
// body to name its code, as that one. Any other domain.ErrConflict is
// domain.ErrUnavailable, which the source asks again, and keeps nothing of
// the conflict but its text.
func questionFailed(err error) error {
	if errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrInvalidBucketState) {
		return fmt.Errorf("%w: a conflict while the question was answered (%v)", domain.ErrUnavailable, err.Error())
	}
	return err
}

// ReplicaVersionInput names the version a source asks about.
type ReplicaVersionInput struct {
	Bucket string
	Key    string
	// Version is the ID the version has at the source - which its copy
	// has here - or the null version.
	Version object.VersionRef
	// Source is the deployment that asks.
	Source string
}

// ReplicaVersion answers whether the copy of a version is there: its entity
// tag when it is, and nothing else of it. It needs s3:ReplicateObject on the
// object; s3:GetObjectVersion, which would let the version be read, is
// neither asked nor enough. The error wraps domain.ErrAccessDenied,
// domain.ErrInvalidArgument, domain.ErrBucketNotFound,
// domain.ErrInvalidBucketState for a bucket that is no replica of that
// source, domain.ErrVersionNotFound or domain.ErrObjectNotFound for a
// version that is not there, and domain.ErrMethodNotAllowed for a delete
// marker, which is there and has no entity tag. Any other conflict is
// domain.ErrUnavailable, never domain.ErrConflict (questionFailed).
func (s *ObjectService) ReplicaVersion(ctx context.Context, principal iam.Principal, in ReplicaVersionInput) (string, error) {
	etag, err := s.replicaVersion(ctx, principal, in)
	if err != nil {
		return "", fmt.Errorf("object.ReplicaVersion: %w", questionFailed(err))
	}
	return etag, nil
}

// replicaVersion is ReplicaVersion, its errors not yet kept apart.
func (s *ObjectService) replicaVersion(ctx context.Context, principal iam.Principal, in ReplicaVersionInput) (string, error) {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: in.Bucket, Key: in.Key}, in.Source); err != nil {
		return "", err
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return "", err
	}
	if !in.Version.Null && in.Version.ID.IsZero() {
		return "", fmt.Errorf("%w: no version is named", domain.ErrInvalidArgument)
	}
	version, err := s.index.GetVersion(ctx, in.Bucket, in.Key, in.Version)
	if err != nil {
		return "", err
	}
	if version.DeleteMarker {
		return "", fmt.Errorf("%w: the version is a delete marker", domain.ErrMethodNotAllowed)
	}
	return version.ETag, nil
}

// ReplicaUploadsInput asks for the replica uploads of one key that a source
// began and has neither completed nor aborted.
type ReplicaUploadsInput struct {
	Bucket string
	Key    string
	Source string
	// KeyMarker and UploadIDMarker continue a listing as in
	// ListUploadsInput: after the upload with that ID of that key, or,
	// without an ID, after every upload of that key. A marker before the
	// key lists from its first upload; one after it lists nothing.
	KeyMarker      string
	UploadIDMarker object.ID
	// Max is the greatest number of uploads to return; at most
	// object.MaxListKeys.
	Max int
}

// ReplicaUploadsOutput is one page of the replica uploads of a key.
type ReplicaUploadsOutput struct {
	// Uploads are the uploads of the key whose ReplicaFrom is the source,
	// in order of ID.
	Uploads []object.Upload
	// Truncated reports that there are more; the key as the key marker
	// and NextUploadIDMarker continue the listing.
	Truncated          bool
	NextUploadIDMarker object.ID
}

// ListReplicaUploads lists the uploads in progress of one key that the
// source began as copies, and no other: neither the uploads of other keys
// nor those that are no copy or are another source's. It needs
// s3:ReplicateObject on the object; s3:ListBucketMultipartUploads is
// neither asked nor enough. The error wraps domain.ErrAccessDenied,
// domain.ErrInvalidArgument, domain.ErrBucketNotFound and
// domain.ErrInvalidBucketState for a bucket that is no replica of that
// source.
func (s *ObjectService) ListReplicaUploads(ctx context.Context, principal iam.Principal, in ReplicaUploadsInput) (ReplicaUploadsOutput, error) {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: in.Bucket, Key: in.Key}, in.Source); err != nil {
		return ReplicaUploadsOutput{}, fmt.Errorf("object.ListReplicaUploads: %w", err)
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return ReplicaUploadsOutput{}, fmt.Errorf("object.ListReplicaUploads: %w", err)
	}
	limit := min(max(in.Max, 0), object.MaxListKeys)
	start := ports.UploadCursor{Key: in.Key}
	switch {
	case limit == 0, in.KeyMarker > in.Key, in.KeyMarker == in.Key && in.UploadIDMarker.IsZero():
		// Nothing is asked for, or the listing is past every upload of
		// the key.
		return ReplicaUploadsOutput{}, nil
	case in.KeyMarker == in.Key:
		start.After = in.UploadIDMarker
	}
	var out ReplicaUploadsOutput
	err := s.uploads.ScanUploads(ctx, in.Bucket, start, func(u object.Upload) ports.ScanControl {
		switch {
		case u.Key != in.Key:
			// Keys are ordered: past the key nothing is of it.
			return ports.ScanControl{Stop: true}
		case u.ReplicaFrom != in.Source:
			return ports.ScanControl{}
		case len(out.Uploads) == limit:
			out.Truncated = true
			return ports.ScanControl{Stop: true}
		}
		out.Uploads = append(out.Uploads, u)
		return ports.ScanControl{}
	})
	if err != nil {
		return ReplicaUploadsOutput{}, fmt.Errorf("object.ListReplicaUploads: %w", err)
	}
	if out.Truncated {
		out.NextUploadIDMarker = out.Uploads[len(out.Uploads)-1].ID
	}
	return out, nil
}

// AbortReplicaUpload gives up an upload that the source began as a copy,
// and no other: an upload of the key that is no copy, or is another
// source's, is to this source not there (domain.ErrUploadNotFound), so the
// answer tells it nothing of uploads that are not its own. It needs
// s3:ReplicateObject on the object. The error wraps domain.ErrAccessDenied,
// domain.ErrInvalidArgument, domain.ErrBucketNotFound,
// domain.ErrInvalidBucketState for a bucket that is no replica of that
// source, and domain.ErrUploadNotFound.
func (s *ObjectService) AbortReplicaUpload(ctx context.Context, principal iam.Principal, in UploadRef, source string) error {
	if err := s.admitReplicaUpload(ctx, principal, iam.Resource{Bucket: in.Bucket, Key: in.Key}, source); err != nil {
		return fmt.Errorf("object.AbortReplicaUpload: %w", err)
	}
	upload, err := s.uploads.GetUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return fmt.Errorf("object.AbortReplicaUpload: %w", err)
	}
	if upload.ReplicaFrom != source {
		return fmt.Errorf("object.AbortReplicaUpload: %w: no upload of that source", domain.ErrUploadNotFound)
	}
	parts, err := s.uploads.AbortUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return fmt.Errorf("object.AbortReplicaUpload: %w", err)
	}
	s.discard(ctx, nil, parts)
	return nil
}
