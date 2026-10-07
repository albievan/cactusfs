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
	// Fail before any byte is written when the bucket does not exist. The
	// state machine checks again at commit, which is what makes it atomic.
	if _, err := s.buckets.GetBucket(ctx, in.Bucket); err != nil {
		return object.Version{}, err
	}

	version := object.Version{
		Bucket: in.Bucket, Key: in.Key,
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
	replaced, err := s.index.PutVersion(ctx, version)
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
	if replaced != nil {
		s.storage.remove(ctx, replaced.DataID, replaced.Layout)
	}
	if degraded {
		// Committed on a quorum, but not on every drive: rebuild the rest.
		s.healer.Enqueue(version.Bucket, version.Key)
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
		version, err := s.head(ctx, principal, in.Bucket, in.Key)
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
		b.healer.Enqueue(b.shards.version.Bucket, b.shards.version.Key)
	}
	return b.shards.Close()
}

// Head returns the current version of an object without reading its content.
func (s *ObjectService) Head(ctx context.Context, principal iam.Principal, bucketName, key string) (object.Version, error) {
	version, err := s.head(ctx, principal, bucketName, key)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.Head: %w", err)
	}
	return version, nil
}

func (s *ObjectService) head(ctx context.Context, principal iam.Principal, bucketName, key string) (object.Version, error) {
	resource := iam.Resource{Bucket: bucketName, Key: key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionGetObject, Resource: resource}); err != nil {
		return object.Version{}, err
	}
	if err := object.ValidateKey(key); err != nil {
		return object.Version{}, err
	}
	return s.index.GetObject(ctx, bucketName, key)
}

// Delete removes an object. Deleting an object that does not exist succeeds,
// as S3 clients expect.
func (s *ObjectService) Delete(ctx context.Context, principal iam.Principal, bucketName, key string) error {
	resource := iam.Resource{Bucket: bucketName, Key: key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionDeleteObject, Resource: resource}); err != nil {
		return fmt.Errorf("object.Delete: %w", err)
	}
	if err := object.ValidateKey(key); err != nil {
		return fmt.Errorf("object.Delete: %w", err)
	}
	removed, err := s.index.DeleteObject(ctx, bucketName, key)
	if err != nil {
		return fmt.Errorf("object.Delete: %w", err)
	}
	if removed != nil {
		s.storage.remove(ctx, removed.DataID, removed.Layout)
	}
	return nil
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
