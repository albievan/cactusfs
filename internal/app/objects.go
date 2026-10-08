package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// ObjectService holds the object use cases.
type ObjectService struct {
	index   ports.ObjectIndex
	buckets ports.BucketStore
	storage *shardStorage
	kms     ports.KMS
	authz   ports.Authorizer
	clock   ports.Clock
	logger  *slog.Logger
	healer  *Healer

	// entropy is the source of identifiers and nonce prefixes. It is read
	// under entropyMu because a source need not be safe for concurrent use.
	entropyMu sync.Mutex
	entropy   io.Reader
}

// ObjectServiceConfig lists what an ObjectService depends on.
type ObjectServiceConfig struct {
	Index   ports.ObjectIndex
	Buckets ports.BucketStore
	// Drives finds the drive topology and the drives, wherever they are.
	Drives ports.DriveSource
	// Codec computes parity.
	Codec ports.ErasureCodec
	// OnHeal, when not nil, is called with the outcome of every heal.
	OnHeal func(HealResult)
	KMS    ports.KMS
	Authz  ports.Authorizer
	Clock  ports.Clock
	// Entropy must be a cryptographically secure random source.
	Entropy io.Reader
	Logger  *slog.Logger
}

// NewObjectService returns an ObjectService.
func NewObjectService(cfg ObjectServiceConfig) (*ObjectService, error) {
	storage, err := newShardStorage(cfg.Drives, cfg.Codec, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("app.NewObjectService: %w", err)
	}
	return &ObjectService{
		index: cfg.Index, buckets: cfg.Buckets, storage: storage, kms: cfg.KMS,
		authz: cfg.Authz, clock: cfg.Clock, logger: cfg.Logger, entropy: cfg.Entropy,
		healer: newHealer(cfg.Index, storage, cfg.Logger, cfg.OnHeal),
	}, nil
}

// InFlight reports whether a write that uses the data ID has not been
// resolved yet: its shards exist but its version is neither committed nor
// definitely refused. The garbage collector must leave such shards alone.
func (s *ObjectService) InFlight(dataID object.ID) bool {
	return s.storage.isPinned(dataID)
}

// Healer returns the service that repairs damaged objects. Its Run method
// must be running for degraded reads and writes to be repaired.
func (s *ObjectService) Healer() *Healer {
	return s.healer
}

// PutInput describes an object to store.
type PutInput struct {
	Bucket string
	Key    string
	// Body is the object's content.
	Body io.Reader
	// Size is the declared length of Body, or -1 when it is not known.
	Size int64

	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	UserMetadata       map[string]string

	// Lock, when not empty, keeps the new version from being removed. The
	// bucket must have been created with Object Lock.
	Lock object.Lock

	// Verify is called once Body has been read to its end and before
	// anything becomes durable. It returns the entity tag of the content,
	// or an error wrapping domain.ErrBadDigest when the content does not
	// match a digest the client declared. The inbound adapter supplies it,
	// because which digests a client may declare is protocol.
	Verify func() (etag string, err error)
}

// Put stores an object and returns its committed version. The order is the
// write path of the architecture: encrypt, write the shards and flush them to
// stable storage, commit the version through Raft, and only then return.
func (s *ObjectService) Put(ctx context.Context, principal iam.Principal, in PutInput) (object.Version, error) {
	version, err := s.put(ctx, principal, in)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.Put: %w", err)
	}
	return version, nil
}

func (s *ObjectService) put(ctx context.Context, principal iam.Principal, in PutInput) (object.Version, error) {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObject, Resource: resource}); err != nil {
		return object.Version{}, err
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return object.Version{}, err
	}
	if err := object.ValidateUserMetadata(in.UserMetadata); err != nil {
		return object.Version{}, err
	}
	if in.Size > object.MaxSize {
		return object.Version{}, fmt.Errorf("%w: %d bytes declared", domain.ErrEntityTooLarge, in.Size)
	}
	// Locking a version is a permission of its own, on top of writing it.
	if in.Lock.Retention != nil {
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObjectRetention, Resource: resource}); err != nil {
			return object.Version{}, err
		}
	}
	if in.Lock.LegalHold {
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObjectLegalHold, Resource: resource}); err != nil {
			return object.Version{}, err
		}
	}
	// Fail before any byte is written when the bucket does not exist, or
	// cannot hold the lock that is asked for. The state machine checks
	// again at commit, which is what makes it atomic.
	target, err := s.buckets.GetBucket(ctx, in.Bucket)
	if err != nil {
		return object.Version{}, err
	}
	if !in.Lock.IsZero() {
		if !target.ObjectLock {
			return object.Version{}, fmt.Errorf("%w: bucket %s was not created with Object Lock", domain.ErrLockNotConfigured, in.Bucket)
		}
		if in.Lock.Retention != nil {
			if err := in.Lock.Retention.Validate(s.clock.Now()); err != nil {
				return object.Version{}, err
			}
		}
	}

	version := object.Version{
		Bucket: in.Bucket, Key: in.Key, Lock: in.Lock,
		ContentType: in.ContentType, ContentEncoding: in.ContentEncoding, ContentDisposition: in.ContentDisposition,
		ContentLanguage: in.ContentLanguage, CacheControl: in.CacheControl, UserMetadata: in.UserMetadata,
	}
	stream, err := s.newVersionKeys(ctx, &version)
	if err != nil {
		return object.Version{}, err
	}

	writer, err := s.storage.create(ctx, version.DataID)
	if err != nil {
		return object.Version{}, err
	}
	// From here until the version is committed or definitely refused, the
	// shards are unreferenced and must be kept from the collector.
	s.storage.pin(version.DataID)
	resolved := true
	defer func() {
		writer.abort()
		if resolved {
			s.storage.unpin(version.DataID)
		}
	}()

	body := &countingReader{reader: io.LimitReader(in.Body, object.MaxSize+1)}
	if _, err := io.Copy(writer, stream.Encrypt(body)); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// The transport reports a body that was cut short.
			return object.Version{}, fmt.Errorf("%w: %w", domain.ErrIncompleteBody, err)
		}
		return object.Version{}, err
	}
	switch {
	case body.count > object.MaxSize:
		return object.Version{}, fmt.Errorf("%w: more than %d bytes", domain.ErrEntityTooLarge, int64(object.MaxSize))
	case in.Size >= 0 && body.count < in.Size:
		return object.Version{}, fmt.Errorf("%w: got %d of %d bytes", domain.ErrIncompleteBody, body.count, in.Size)
	case in.Size >= 0 && body.count > in.Size:
		return object.Version{}, fmt.Errorf("%w: got %d bytes, %d declared", domain.ErrInvalidArgument, body.count, in.Size)
	}
	version.Size = body.count
	if in.Verify != nil {
		if version.ETag, err = in.Verify(); err != nil {
			return object.Version{}, err
		}
	}

	// Durable on a write quorum of drives first ...
	var degraded bool
	if version.Layout, degraded, err = writer.commit(ctx); err != nil {
		return object.Version{}, err
	}

	// ... then visible, through Raft. Nothing was acknowledged before this.
	version.ModifiedAt = s.clock.Now().UTC()
	committed, err := s.index.PutVersion(ctx, version)
	if err != nil {
		// Remove the shard only when the version was definitely not
		// committed. When the outcome is unknown the entry may still be
		// applied, after a restart for example, and would then point at
		// data that is gone; the unreferenced shard of a version that
		// never appears is left to the collector instead.
		if errors.Is(err, domain.ErrOutcomeUnknown) {
			// Stays pinned for as long as this process lives.
			resolved = false
		} else {
			s.storage.remove(ctx, version.DataID, version.Layout)
		}
		return object.Version{}, err
	}
	// What the put removed for good has no reader any more. A version that
	// is merely no longer current keeps its data.
	if replaced := committed.Removed; replaced != nil && !replaced.DeleteMarker {
		s.storage.remove(ctx, replaced.DataID, replaced.Layout)
	}
	version = committed.Stored
	if degraded {
		// Committed on a quorum, but not on every drive: rebuild the rest.
		s.healer.Enqueue(version)
	}
	return version, nil
}

// newVersionKeys fills in the identifiers and the encryption parameters of a
// new version and returns the stream that encrypts it.
func (s *ObjectService) newVersionKeys(ctx context.Context, version *object.Version) (*object.Stream, error) {
	random := make([]byte, 2*object.IDSize+object.NoncePrefixSize)
	s.entropyMu.Lock()
	_, err := io.ReadFull(s.entropy, random)
	s.entropyMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("read randomness: %w", err)
	}
	copy(version.VersionID[:], random[:object.IDSize])
	copy(version.DataID[:], random[object.IDSize:2*object.IDSize])
	noncePrefix := random[2*object.IDSize:]

	// The data ID ties both the wrapped key and the ciphertext to this
	// version's data: neither can be moved to another object.
	key, err := s.kms.GenerateDataKey(ctx, version.DataID[:])
	if err != nil {
		return nil, err
	}
	defer clear(key.Plaintext)

	stream, err := object.NewStream(key.Plaintext, noncePrefix, version.DataID[:], object.DefaultSegmentSize)
	if err != nil {
		return nil, err
	}
	version.Encryption = object.Encryption{
		Scheme:      object.StreamSchemeV1,
		KMSKeyID:    s.kms.KeyID(),
		WrappedKey:  key.Wrapped,
		NoncePrefix: noncePrefix,
		SegmentSize: object.DefaultSegmentSize,
	}
	return stream, nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

// GetInput selects an object, and optionally a range of it, to read.
type GetInput struct {
	Bucket string
	Key    string
	// Range, when not nil, asks for part of the object.
	Range *object.RangeSpec
	// Version, when not nil, names the version to read instead of the
	// current one.
	Version *object.VersionRef
}

// GetOutput is an object being read.
type GetOutput struct {
	// Version is the object version being served.
	Version object.Version
	// Range is the part of the object in Body: the whole object unless a
	// range was asked for.
	Range object.Range
	// Body yields the bytes of Range. Every byte it returns has been
	// authenticated; a read fails with an error wrapping
	// domain.ErrIntegrity rather than return damaged data. The caller must
	// close it.
	Body io.ReadCloser
}

// Get opens an object for reading.
func (s *ObjectService) Get(ctx context.Context, principal iam.Principal, in GetInput) (GetOutput, error) {
	out, err := s.get(ctx, principal, in)
	if err != nil {
		return GetOutput{}, fmt.Errorf("object.Get: %w", err)
	}
	return out, nil
}

// getAttempts bounds how often Get starts over when the version it was about
// to read is replaced or deleted underneath it.
const getAttempts = 4

func (s *ObjectService) get(ctx context.Context, principal iam.Principal, in GetInput) (GetOutput, error) {
	var lastData object.ID
	for attempt := 1; ; attempt++ {
		version, err := s.head(ctx, principal, in.Bucket, in.Key, in.Version)
		if err != nil {
			return GetOutput{}, err
		}
		out, err := s.open(ctx, version, in)
		// A concurrent overwrite or delete removes the replaced shards
		// right after its commit. A reader that fetched the old version
		// just before finds some or all of them gone and must then serve
		// the new state, not an error: it looks again, and gives up only
		// when the same version is still current and its data is really
		// missing or damaged.
		gone := errors.Is(err, domain.ErrShardNotFound) || errors.Is(err, domain.ErrIntegrity)
		if gone && attempt < getAttempts && version.DataID != lastData {
			lastData = version.DataID
			continue
		}
		return out, err
	}
}

// open opens the stored data of one version for reading.
func (s *ObjectService) open(ctx context.Context, version object.Version, in GetInput) (GetOutput, error) {
	var err error
	span := object.Range{Offset: 0, Length: version.Size}
	if in.Range != nil {
		if span, err = in.Range.Resolve(version.Size); err != nil {
			return GetOutput{}, err
		}
	}

	stream, err := s.openVersionKeys(ctx, version)
	if err != nil {
		return GetOutput{}, err
	}
	if version.Layout.StoredSize != stream.CiphertextSize(version.Size) {
		return GetOutput{}, fmt.Errorf("%w: stored size %d does not match object size %d",
			domain.ErrIntegrity, version.Layout.StoredSize, version.Size)
	}
	shard, err := s.storage.open(ctx, version)
	if err != nil {
		return GetOutput{}, err
	}

	section := stream.Locate(span, version.Size)
	plain := bufio.NewReaderSize(
		stream.Decrypt(io.NewSectionReader(shard, section.Offset, section.Length), section, version.Size),
		version.Encryption.SegmentSize,
	)
	if _, err := plain.Discard(int(section.Skip)); err != nil {
		_ = shard.Close()
		return GetOutput{}, err
	}
	// Authenticate the first segment now, so that damage is reported as an
	// error rather than discovered after the response has begun.
	if _, err := plain.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		_ = shard.Close()
		return GetOutput{}, err
	}

	var body io.Reader = plain
	if span.Length > 0 {
		body = io.LimitReader(plain, span.Length)
	}
	return GetOutput{Version: version, Range: span, Body: &objectBody{Reader: body, shards: shard, healer: s.healer}}, nil
}

// openVersionKeys unwraps the data key of a stored version and returns the
// stream that decrypts it.
func (s *ObjectService) openVersionKeys(ctx context.Context, version object.Version) (*object.Stream, error) {
	enc := version.Encryption
	if enc.Scheme != object.StreamSchemeV1 {
		return nil, fmt.Errorf("%w: encryption scheme %q", domain.ErrUnsupportedFormat, enc.Scheme)
	}
	if enc.KMSKeyID != s.kms.KeyID() {
		return nil, fmt.Errorf("%w: object is encrypted under KMS key %s, this node uses %s",
			domain.ErrKeyUnavailable, enc.KMSKeyID, s.kms.KeyID())
	}
	key, err := s.kms.UnwrapDataKey(ctx, enc.WrappedKey, version.DataID[:])
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return object.NewStream(key, enc.NoncePrefix, version.DataID[:], enc.SegmentSize)
}

type objectBody struct {
	io.Reader
	shards *streamReader
	healer *Healer
}

// Close releases the underlying shards. A read that had to work around a
// missing or damaged shard queues the object for healing.
func (b *objectBody) Close() error {
	if b.shards.Degraded() {
		b.healer.Enqueue(b.shards.version)
	}
	return b.shards.Close()
}

// Head returns the current version of an object without reading its content.
func (s *ObjectService) Head(ctx context.Context, principal iam.Principal, bucketName, key string) (object.Version, error) {
	version, err := s.head(ctx, principal, bucketName, key, nil)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.Head: %w", err)
	}
	return version, nil
}

// HeadVersion returns one named version of an object without reading its
// content. Reading a version by name needs a permission of its own. The
// error wraps domain.ErrVersionNotFound when there is no such version, and
// domain.ErrMethodNotAllowed when it is a delete marker: a marker has no
// content.
func (s *ObjectService) HeadVersion(ctx context.Context, principal iam.Principal, bucketName, key string, ref object.VersionRef) (object.Version, error) {
	version, err := s.head(ctx, principal, bucketName, key, &ref)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.HeadVersion: %w", err)
	}
	return version, nil
}

func (s *ObjectService) head(ctx context.Context, principal iam.Principal, bucketName, key string, ref *object.VersionRef) (object.Version, error) {
	action := iam.ActionGetObject
	if ref != nil {
		action = iam.ActionGetObjectVersion
	}
	resource := iam.Resource{Bucket: bucketName, Key: key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: action, Resource: resource}); err != nil {
		return object.Version{}, err
	}
	if err := object.ValidateKey(key); err != nil {
		return object.Version{}, err
	}
	if ref == nil {
		return s.index.GetObject(ctx, bucketName, key)
	}
	version, err := s.index.GetVersion(ctx, bucketName, key, *ref)
	if err != nil {
		return object.Version{}, err
	}
	if version.DeleteMarker {
		return object.Version{}, fmt.Errorf("%w: version %s of %s/%s is a delete marker", domain.ErrMethodNotAllowed, ref, bucketName, key)
	}
	return version, nil
}

// LockInput names the version whose lock is read or changed.
type LockInput struct {
	Bucket string
	Key    string
	// Version, when nil, means the current version.
	Version *object.VersionRef
}

// lockTarget authorizes a lock operation and resolves which version it is
// about.
func (s *ObjectService) lockTarget(ctx context.Context, principal iam.Principal, action iam.Action, in LockInput) (object.Version, error) {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: action, Resource: resource}); err != nil {
		return object.Version{}, err
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return object.Version{}, err
	}
	target, err := s.buckets.GetBucket(ctx, in.Bucket)
	if err != nil {
		return object.Version{}, err
	}
	if !target.ObjectLock {
		return object.Version{}, fmt.Errorf("%w: bucket %s was not created with Object Lock", domain.ErrLockNotConfigured, in.Bucket)
	}
	if in.Version == nil {
		return s.index.GetObject(ctx, in.Bucket, in.Key)
	}
	version, err := s.index.GetVersion(ctx, in.Bucket, in.Key, *in.Version)
	if err != nil {
		return object.Version{}, err
	}
	if version.DeleteMarker {
		return object.Version{}, fmt.Errorf("%w: a delete marker has no lock", domain.ErrMethodNotAllowed)
	}
	return version, nil
}

// Retention returns the retention of a version. The error wraps
// domain.ErrLockNotConfigured when the version has none.
func (s *ObjectService) Retention(ctx context.Context, principal iam.Principal, in LockInput) (object.Retention, error) {
	version, err := s.lockTarget(ctx, principal, iam.ActionGetObjectRetention, in)
	if err != nil {
		return object.Retention{}, fmt.Errorf("object.Retention: %w", err)
	}
	if version.Lock.Retention == nil {
		return object.Retention{}, fmt.Errorf("object.Retention: %w: the version has no retention", domain.ErrLockNotConfigured)
	}
	return *version.Lock.Retention, nil
}

// SetRetention replaces the retention of a version; nil removes it. A
// retention can always be extended. Shortening or removing a governance
// retention needs bypassGovernance and the permission to bypass; a
// compliance retention that has not ended can only be extended, by anybody.
// Whether the change is allowed is decided when it is committed.
func (s *ObjectService) SetRetention(
	ctx context.Context, principal iam.Principal, in LockInput, retention *object.Retention, bypassGovernance bool,
) (object.Version, error) {
	version, err := s.lockTarget(ctx, principal, iam.ActionPutObjectRetention, in)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.SetRetention: %w", err)
	}
	if bypassGovernance {
		resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionOverrideGovernance, Resource: resource}); err != nil {
			return object.Version{}, fmt.Errorf("object.SetRetention: %w", err)
		}
	}
	// Refuse early what is plainly not allowed; the commit decides.
	if err := version.Lock.CheckRetention(retention, s.clock.Now(), bypassGovernance); err != nil {
		return object.Version{}, fmt.Errorf("object.SetRetention: %w", err)
	}
	stored, err := s.index.SetRetention(ctx, in.Bucket, in.Key, version.Ref(), retention, bypassGovernance)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.SetRetention: %w", err)
	}
	return stored, nil
}

// LegalHold reports whether a version is under legal hold.
func (s *ObjectService) LegalHold(ctx context.Context, principal iam.Principal, in LockInput) (bool, error) {
	version, err := s.lockTarget(ctx, principal, iam.ActionGetObjectLegalHold, in)
	if err != nil {
		return false, fmt.Errorf("object.LegalHold: %w", err)
	}
	return version.Lock.LegalHold, nil
}

// SetLegalHold puts a version under legal hold or takes it off.
func (s *ObjectService) SetLegalHold(ctx context.Context, principal iam.Principal, in LockInput, on bool) (object.Version, error) {
	version, err := s.lockTarget(ctx, principal, iam.ActionPutObjectLegalHold, in)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.SetLegalHold: %w", err)
	}
	stored, err := s.index.SetLegalHold(ctx, in.Bucket, in.Key, version.Ref(), on)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.SetLegalHold: %w", err)
	}
	return stored, nil
}

// VersionListOptions select a page of the versions of a bucket.
type VersionListOptions struct {
	Prefix    string
	Delimiter string
	// KeyMarker and VersionMarker continue a listing: it resumes after
	// that version of that key, or, without a version, after the key.
	KeyMarker     string
	VersionMarker *object.VersionRef
	// MaxKeys bounds the number of versions plus common prefixes returned.
	MaxKeys int
}

// ListedVersion is one entry of a version listing.
type ListedVersion struct {
	Version object.Version
	// Latest says that this is the newest version of its key.
	Latest bool
}

// VersionListPage is one page of the versions of a bucket: keys in order, the
// versions of a key from the newest to the oldest, delete markers included.
type VersionListPage struct {
	Versions       []ListedVersion
	CommonPrefixes []string
	Truncated      bool
	// NextKeyMarker and NextVersionMarker continue the listing.
	NextKeyMarker     string
	NextVersionMarker *object.VersionRef
}

// ListVersions returns one page of every version of the objects in a bucket.
func (s *ObjectService) ListVersions(
	ctx context.Context, principal iam.Principal, bucketName string, opts VersionListOptions,
) (VersionListPage, error) {
	resource := iam.Resource{Bucket: bucketName}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionListBucketVersions, Resource: resource}); err != nil {
		return VersionListPage{}, fmt.Errorf("object.ListVersions: %w", err)
	}
	if opts.MaxKeys < 0 {
		return VersionListPage{}, fmt.Errorf("object.ListVersions: %w: negative max-keys", domain.ErrInvalidArgument)
	}
	limit := min(opts.MaxKeys, object.MaxListKeys)
	if limit == 0 {
		// Still answer for a missing bucket.
		if _, err := s.buckets.GetBucket(ctx, bucketName); err != nil {
			return VersionListPage{}, fmt.Errorf("object.ListVersions: %w", err)
		}
		return VersionListPage{}, nil
	}

	var page VersionListPage
	// Where to begin: at the prefix, or after what the previous page ended
	// with - a version of a key, a key, or a common prefix, which stands
	// for every key under it.
	start := ports.VersionCursor{Key: opts.Prefix}
	skipKey := ""
	switch group, grouped := object.CommonPrefix(opts.KeyMarker, opts.Prefix, opts.Delimiter); {
	case opts.KeyMarker == "" || opts.KeyMarker < opts.Prefix:
	case opts.VersionMarker != nil:
		start = ports.VersionCursor{Key: opts.KeyMarker, After: opts.VersionMarker}
	case grouped && group == opts.KeyMarker:
		end, more := object.PrefixEnd(group)
		if !more {
			return page, nil
		}
		start.Key = end
	default:
		start.Key, skipKey = opts.KeyMarker, opts.KeyMarker
	}
	count := 0
	err := s.index.ScanVersions(ctx, bucketName, start, func(v object.Version, latest bool) ports.ScanControl {
		switch {
		case !strings.HasPrefix(v.Key, opts.Prefix):
			if v.Key < opts.Prefix {
				return ports.ScanControl{SeekTo: opts.Prefix}
			}
			// Keys are ordered: past the prefix nothing can match.
			return ports.ScanControl{Stop: true}
		case v.Key == skipKey:
			// Every version of the marker's key was on an earlier page.
			return ports.ScanControl{SeekTo: v.Key + "\x00"}
		case count == limit:
			page.Truncated = true
			return ports.ScanControl{Stop: true}
		}
		count++
		if group, ok := object.CommonPrefix(v.Key, opts.Prefix, opts.Delimiter); ok {
			page.CommonPrefixes = append(page.CommonPrefixes, group)
			page.NextKeyMarker, page.NextVersionMarker = group, nil
			// Jump over every other key of the group.
			end, more := object.PrefixEnd(group)
			return ports.ScanControl{Stop: !more, SeekTo: end}
		}
		page.Versions = append(page.Versions, ListedVersion{Version: v, Latest: latest})
		ref := v.Ref()
		page.NextKeyMarker, page.NextVersionMarker = v.Key, &ref
		return ports.ScanControl{}
	})
	if err != nil {
		return VersionListPage{}, fmt.Errorf("object.ListVersions: %w", err)
	}
	if !page.Truncated {
		page.NextKeyMarker, page.NextVersionMarker = "", nil
	}
	return page, nil
}

// DeleteInput names what to delete.
type DeleteInput struct {
	Bucket string
	Key    string
	// Version, when not nil, names one version to remove for good.
	Version *object.VersionRef
	// BypassGovernance asks to remove a version although a governance
	// retention holds it. The caller needs the permission to; without it
	// the request is refused, never carried out without the bypass.
	BypassGovernance bool
}

// DeleteOutput says what a delete did.
type DeleteOutput struct {
	// Marker is the delete marker that became the newest version, in a
	// bucket that keeps versions.
	Marker *object.Version
	// Removed is the version that was removed for good, if one was.
	Removed *object.Version
}

// Delete deletes an object, or one version of it. In a bucket that keeps
// versions, deleting an object adds a delete marker and removes nothing;
// naming a version removes that version for good, and needs the permission
// to do so. Deleting what does not exist succeeds, as S3 clients expect.
func (s *ObjectService) Delete(ctx context.Context, principal iam.Principal, in DeleteInput) (DeleteOutput, error) {
	action := iam.ActionDeleteObject
	if in.Version != nil {
		action = iam.ActionDeleteObjectVersion
	}
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: action, Resource: resource}); err != nil {
		return DeleteOutput{}, fmt.Errorf("object.Delete: %w", err)
	}
	if in.BypassGovernance {
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionOverrideGovernance, Resource: resource}); err != nil {
			return DeleteOutput{}, fmt.Errorf("object.Delete: %w", err)
		}
	}
	if err := object.ValidateKey(in.Key); err != nil {
		return DeleteOutput{}, fmt.Errorf("object.Delete: %w", err)
	}
	// Should the delete make a marker, this is its ID.
	s.entropyMu.Lock()
	markerID, err := object.NewID(s.entropy)
	s.entropyMu.Unlock()
	if err != nil {
		return DeleteOutput{}, fmt.Errorf("object.Delete: %w", err)
	}
	result, err := s.index.DeleteObject(ctx, in.Bucket, in.Key, ports.DeleteOptions{
		Version: in.Version, MarkerID: markerID, At: s.clock.Now().UTC(), BypassGovernance: in.BypassGovernance,
	})
	if err != nil {
		return DeleteOutput{}, fmt.Errorf("object.Delete: %w", err)
	}
	if removed := result.Removed; removed != nil && !removed.DeleteMarker {
		s.storage.remove(ctx, removed.DataID, removed.Layout)
	}
	return DeleteOutput{Marker: result.Marker, Removed: result.Removed}, nil
}

// List returns one page of a bucket listing.
func (s *ObjectService) List(
	ctx context.Context, principal iam.Principal, bucketName string, opts object.ListOptions,
) (object.ListPage, error) {
	resource := iam.Resource{Bucket: bucketName}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionListBucket, Resource: resource}); err != nil {
		return object.ListPage{}, fmt.Errorf("object.List: %w", err)
	}
	if opts.MaxKeys < 0 {
		return object.ListPage{}, fmt.Errorf("object.List: %w: negative max-keys", domain.ErrInvalidArgument)
	}
	limit := min(opts.MaxKeys, object.MaxListKeys)

	seek, skip, more := opts.StartKey()
	if limit == 0 || !more {
		// Still answer for a missing bucket.
		if _, err := s.buckets.GetBucket(ctx, bucketName); err != nil {
			return object.ListPage{}, fmt.Errorf("object.List: %w", err)
		}
		return object.ListPage{}, nil
	}

	var page object.ListPage
	count := 0
	err := s.index.ScanObjects(ctx, bucketName, seek, func(v object.Version) ports.ScanControl {
		switch {
		case !strings.HasPrefix(v.Key, opts.Prefix):
			// Keys are ordered: past the prefix nothing can match.
			return ports.ScanControl{Stop: true}
		case v.Key == skip:
			return ports.ScanControl{}
		case count == limit:
			page.Truncated = true
			return ports.ScanControl{Stop: true}
		}
		count++
		if group, ok := object.CommonPrefix(v.Key, opts.Prefix, opts.Delimiter); ok {
			page.CommonPrefixes = append(page.CommonPrefixes, group)
			page.NextContinuation = group
			// Jump over every other key of the group.
			end, more := object.PrefixEnd(group)
			return ports.ScanControl{Stop: !more, SeekTo: end}
		}
		page.Objects = append(page.Objects, v)
		page.NextContinuation = v.Key
		return ports.ScanControl{}
	})
	if err != nil {
		return object.ListPage{}, fmt.Errorf("object.List: %w", err)
	}
	if !page.Truncated {
		page.NextContinuation = ""
	}
	return page, nil
}
