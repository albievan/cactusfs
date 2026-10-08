package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Multipart uploads (ADR-0012). An upload has one data key from the KMS;
// every part is a stream of its own, encrypted under a key derived from it,
// erasure-coded and made durable like an object. Completing the upload
// commits, in one Raft entry, a version whose bytes are those of the listed
// parts in order.

// admit decides whether a principal may write a version with these
// properties to a key, before any byte is written. The state machine checks
// the bucket and the lock again at commit, which is what makes it atomic.
func (s *ObjectService) admit(
	ctx context.Context, principal iam.Principal, resource iam.Resource, metadata map[string]string, lock object.Lock,
) error {
	if err := object.ValidateKey(resource.Key); err != nil {
		return err
	}
	if err := object.ValidateUserMetadata(metadata); err != nil {
		return err
	}
	// Locking a version is a permission of its own, on top of writing it.
	if lock.Retention != nil {
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObjectRetention, Resource: resource}); err != nil {
			return err
		}
	}
	if lock.LegalHold {
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObjectLegalHold, Resource: resource}); err != nil {
			return err
		}
	}
	target, err := s.buckets.GetBucket(ctx, resource.Bucket)
	if err != nil {
		return err
	}
	if lock.IsZero() {
		return nil
	}
	if !target.ObjectLock {
		return fmt.Errorf("%w: bucket %s was not created with Object Lock", domain.ErrLockNotConfigured, resource.Bucket)
	}
	if lock.Retention != nil {
		return lock.Retention.Validate(s.clock.Now())
	}
	return nil
}

// CreateUploadInput describes the object a multipart upload is to make.
type CreateUploadInput struct {
	Bucket string
	Key    string

	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	UserMetadata       map[string]string

	// Lock, when not empty, keeps the completed version from being
	// removed. Whether a retention can still be kept is decided when the
	// upload is completed.
	Lock object.Lock
	// SSE says whose key it takes to read the object. With SSE-C every
	// part must come with the same key.
	SSE object.SSE
}

// CreateUpload begins a multipart upload.
func (s *ObjectService) CreateUpload(ctx context.Context, principal iam.Principal, in CreateUploadInput) (object.Upload, error) {
	upload, err := s.createUpload(ctx, principal, in)
	if err != nil {
		return object.Upload{}, fmt.Errorf("object.CreateUpload: %w", err)
	}
	return upload, nil
}

func (s *ObjectService) createUpload(ctx context.Context, principal iam.Principal, in CreateUploadInput) (object.Upload, error) {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObject, Resource: resource}); err != nil {
		return object.Upload{}, err
	}
	if err := s.admit(ctx, principal, resource, in.UserMetadata, in.Lock); err != nil {
		return object.Upload{}, err
	}
	random, err := s.random(object.IDSize)
	if err != nil {
		return object.Upload{}, err
	}
	upload := object.Upload{
		Bucket: in.Bucket, Key: in.Key, ID: object.ID(random), InitiatedAt: s.clock.Now().UTC(), Initiator: principal.Name(),
		ContentType: in.ContentType, ContentEncoding: in.ContentEncoding, ContentDisposition: in.ContentDisposition,
		ContentLanguage: in.ContentLanguage, CacheControl: in.CacheControl, UserMetadata: in.UserMetadata, Lock: in.Lock,
	}
	// One data key for the upload, bound to its ID. It encrypts nothing:
	// the key of every part is derived from it.
	enc := object.Encryption{KeyMode: object.KeyModePartHKDF}
	key, err := s.newDataKey(ctx, &enc, in.SSE, object.UploadKeyContext(upload.ID))
	if err != nil {
		return object.Upload{}, err
	}
	clear(key)
	upload.Encryption = enc
	if err := s.uploads.CreateUpload(ctx, upload); err != nil {
		return object.Upload{}, err
	}
	return upload, nil
}

// uploadKey returns the data key of an upload, or of the version it became.
// The caller clears it.
func (s *ObjectService) uploadKey(ctx context.Context, enc object.Encryption, id object.ID) ([]byte, error) {
	if enc.KeyMode != object.KeyModePartHKDF {
		return nil, fmt.Errorf("%w: key mode %q of an object made of parts", domain.ErrUnsupportedFormat, enc.KeyMode)
	}
	return s.unwrap(ctx, enc, object.UploadKeyContext(id))
}

// UploadPartInput is one part of a multipart upload.
type UploadPartInput struct {
	Bucket   string
	Key      string
	UploadID object.ID
	Number   int
	// Body, Size and Verify are as in PutInput.
	Body   io.Reader
	Size   int64
	Verify func() (etag string, err error)
	// CustomerKey is the client's key, for an upload under SSE-C.
	CustomerKey []byte
	// Checksums, when not nil, is called after Verify succeeded and
	// returns the checksums the client declared for the part and that
	// were found to hold, by the name of the header that carried each.
	Checksums func() map[string]string
}

// UploadPart stores one part of a multipart upload, in place of any part
// that had its number. Like an object it is durable on a write quorum and
// committed through Raft before it is acknowledged.
func (s *ObjectService) UploadPart(ctx context.Context, principal iam.Principal, in UploadPartInput) (object.Part, error) {
	part, err := s.uploadPart(ctx, principal, in)
	if err != nil {
		return object.Part{}, fmt.Errorf("object.UploadPart: %w", err)
	}
	return part, nil
}

func (s *ObjectService) uploadPart(ctx context.Context, principal iam.Principal, in UploadPartInput) (object.Part, error) {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObject, Resource: resource}); err != nil {
		return object.Part{}, err
	}
	if err := object.ValidatePartNumber(in.Number); err != nil {
		return object.Part{}, err
	}
	if in.Size > object.MaxSize {
		return object.Part{}, fmt.Errorf("%w: %d bytes declared", domain.ErrEntityTooLarge, in.Size)
	}
	upload, err := s.uploads.GetUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return object.Part{}, err
	}
	random, err := s.random(object.IDSize + object.NoncePrefixSize)
	if err != nil {
		return object.Part{}, err
	}
	part := object.Part{Number: in.Number, DataID: object.ID(random[:object.IDSize]), NoncePrefix: random[object.IDSize:]}
	keys, err := s.partStream(ctx, upload.Encryption, upload.ID, part, in.CustomerKey)
	if err != nil {
		return object.Part{}, err
	}
	defer keys.tag.forget()
	err = s.write(ctx, part.DataID, keys.stream, in.Body, in.Size, keys.tag.verify(in.Verify), func(w written) (object.Extent, error) {
		part.Size, part.ETag, part.Layout = w.size, w.etag, w.layout
		part.ModifiedAt = s.clock.Now().UTC()
		// The checksums of a part under SSE-C were verified and are not
		// kept: they are digests of plaintext, and the record is read by
		// those who do not hold the key.
		if in.Checksums != nil && upload.Encryption.Mode != object.SSEC {
			part.Checksums = in.Checksums()
		}
		replaced, err := s.uploads.PutPart(ctx, in.Bucket, in.Key, in.UploadID, part)
		if err != nil {
			return part.Extent(), err
		}
		if replaced != nil {
			// The part that had this number has no reader any more.
			s.discardData(ctx, replaced.Extent())
		}
		if w.degraded {
			// Committed on a quorum, but not on every drive: rebuild the rest.
			s.healer.EnqueuePart(in.Bucket, in.Key, in.UploadID, in.Number)
		}
		return part.Extent(), nil
	})
	if err != nil {
		return object.Part{}, err
	}
	return part, nil
}

// partStream returns the stream that encrypts one part, under the key
// derived for it from the data key of its upload, and what makes its entity
// tag.
func (s *ObjectService) partStream(
	ctx context.Context, enc object.Encryption, uploadID object.ID, part object.Part, customerKey []byte,
) (keyed, error) {
	uploadKey, err := s.uploadDataKey(ctx, enc, uploadID, customerKey)
	if err != nil {
		return keyed{}, err
	}
	defer clear(uploadKey)
	stream, err := partStreamWith(uploadKey, enc, part)
	if err != nil {
		return keyed{}, err
	}
	tag, err := newTagger(uploadKey, enc)
	if err != nil {
		return keyed{}, err
	}
	return keyed{stream: stream, tag: tag}, nil
}

// uploadDataKey returns the key the part keys of an upload are derived from,
// which for SSE-C takes the client's key and proves it is the right one. The
// caller clears it.
func (s *ObjectService) uploadDataKey(ctx context.Context, enc object.Encryption, uploadID object.ID, customerKey []byte) ([]byte, error) {
	if err := admitKey(enc, customerKey); err != nil {
		return nil, err
	}
	kmsKey, err := s.uploadKey(ctx, enc, uploadID)
	if err != nil {
		return nil, err
	}
	return dataKey(kmsKey, enc, customerKey)
}

// checkUploadKey checks a customer key that came with a request about an
// upload which does not need one: completing it, listing its parts. A client
// may send it; if it does, it must be the upload's.
func (s *ObjectService) checkUploadKey(ctx context.Context, upload object.Upload, customerKey []byte) error {
	if len(customerKey) == 0 {
		return nil
	}
	key, err := s.uploadDataKey(ctx, upload.Encryption, upload.ID, customerKey)
	if err != nil {
		return err
	}
	clear(key)
	return nil
}

func partStreamWith(uploadKey []byte, enc object.Encryption, part object.Part) (*object.Stream, error) {
	key, err := object.PartKey(uploadKey, part.Number, part.DataID)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	// The part's data ID ties the ciphertext to this part's data.
	return object.NewStream(key, part.NoncePrefix, part.DataID[:], enc.SegmentSize)
}

// CompleteUploadInput lists the parts that make the object.
type CompleteUploadInput struct {
	Bucket   string
	Key      string
	UploadID object.ID
	// Parts are the parts in the order of the object, each with the entity
	// tag the client was given for it.
	Parts []object.CompletedPart
	// ETag is the entity tag of the object these parts make. The inbound
	// adapter computes it from the entity tags listed, because how it is
	// formed is protocol; it holds only if the listed tags are those of
	// the stored parts, which completing checks.
	ETag string
	// CustomerKey may be given for an upload under SSE-C, and is then
	// checked. Completing does not need it.
	CustomerKey []byte
}

// completeAttempts bounds how often completing starts over because a part
// was uploaded again while it was at work.
const completeAttempts = 3

// partPage is the number of parts read from the index at a time.
const partPage = 1000

// CompleteUpload turns a multipart upload into a version of its key. The
// version appears, and the upload ends, in one Raft entry.
func (s *ObjectService) CompleteUpload(ctx context.Context, principal iam.Principal, in CompleteUploadInput) (object.Version, error) {
	version, err := s.completeUpload(ctx, principal, in)
	if err != nil {
		return object.Version{}, fmt.Errorf("object.CompleteUpload: %w", err)
	}
	return version, nil
}

func (s *ObjectService) completeUpload(ctx context.Context, principal iam.Principal, in CompleteUploadInput) (object.Version, error) {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObject, Resource: resource}); err != nil {
		return object.Version{}, err
	}
	upload, err := s.uploads.GetUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if errors.Is(err, domain.ErrUploadNotFound) {
		// A client that did not hear the answer asks again. If the
		// object this upload made is the current one, that is the answer.
		if current, getErr := s.index.GetObject(ctx, in.Bucket, in.Key); getErr == nil && current.Multipart() && current.DataID == in.UploadID {
			return current, nil
		}
	}
	if err != nil {
		return object.Version{}, err
	}
	if err := s.checkUploadKey(ctx, upload, in.CustomerKey); err != nil {
		return object.Version{}, err
	}
	if upload.Encryption.Mode == object.SSEC {
		// No checksum of a part was kept (see uploadPart); none can be
		// compared. The entity tags are.
		parts := slices.Clone(in.Parts)
		for i := range parts {
			parts[i].Checksums = nil
		}
		in.Parts = parts
	}
	uploadKey, err := s.uploadKey(ctx, upload.Encryption, upload.ID)
	if err != nil {
		return object.Version{}, err
	}
	manifest, err := object.NewManifest(uploadKey)
	clear(uploadKey)
	if err != nil {
		return object.Version{}, err
	}
	defer manifest.Clear()
	random, err := s.random(object.IDSize)
	if err != nil {
		return object.Version{}, err
	}

	for attempt := 1; ; attempt++ {
		completion := ports.Completion{
			Bucket: in.Bucket, Key: in.Key, ID: in.UploadID, VersionID: object.ID(random), ETag: in.ETag, At: s.clock.Now().UTC(),
		}
		if completion.Parts, err = s.sealParts(ctx, in, manifest); err != nil {
			return object.Version{}, err
		}
		// The state machine decides, against the parts as they are when
		// the entry is applied. The check above is the same one, made
		// early: it spares the entry, and it is what the seals are for.
		result, err := s.uploads.CompleteUpload(ctx, completion)
		if errors.Is(err, domain.ErrConflict) && attempt < completeAttempts {
			continue
		}
		if err != nil {
			return object.Version{}, err
		}
		s.discard(ctx, result.Removed, result.RemovedParts)
		return result.Stored, nil
	}
}

// sealParts reads the stored parts of an upload, checks that the listed ones
// make an object, and returns them with the seal of the place each takes.
func (s *ObjectService) sealParts(ctx context.Context, in CompleteUploadInput, manifest *object.Manifest) ([]object.Part, error) {
	stored := map[int]object.Part{}
	for after := 0; ; {
		page, err := s.uploads.ListParts(ctx, in.Bucket, in.Key, in.UploadID, after, partPage)
		if err != nil {
			return nil, err
		}
		for _, part := range page {
			stored[part.Number] = part
			after = part.Number
		}
		if len(page) < partPage {
			break
		}
	}
	total, err := object.CheckCompletion(in.Parts, func(number int) (object.Part, bool) {
		part, ok := stored[number]
		return part, ok
	})
	if err != nil {
		return nil, err
	}
	sealed := make([]object.Part, len(in.Parts))
	var offset int64
	for i, listed := range in.Parts {
		part := stored[listed.Number]
		// The state machine checks what the client listed, not what is
		// stored: the entry says the first.
		part.ETag, part.Checksums = listed.ETag, listed.Checksums
		part.Index, part.Offset = i, offset
		part.Seal = manifest.Seal(part, len(in.Parts), total)
		sealed[i] = part
		offset += part.Size
	}
	return sealed, nil
}

// UploadRef names a multipart upload.
type UploadRef struct {
	Bucket   string
	Key      string
	UploadID object.ID
}

// AbortUpload ends a multipart upload without making an object. Its parts
// are removed.
func (s *ObjectService) AbortUpload(ctx context.Context, principal iam.Principal, in UploadRef) error {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionAbortMultipartUpload, Resource: resource}); err != nil {
		return fmt.Errorf("object.AbortUpload: %w", err)
	}
	parts, err := s.uploads.AbortUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return fmt.Errorf("object.AbortUpload: %w", err)
	}
	s.discard(ctx, nil, parts)
	return nil
}

// ListPartsInput asks for the parts of an upload in progress.
type ListPartsInput struct {
	UploadRef
	// After is the part number after which the listing begins.
	After int
	// Max is the greatest number of parts to return; at most object.MaxListKeys.
	Max int
	// CustomerKey may be given for an upload under SSE-C, and is then
	// checked.
	CustomerKey []byte
}

// ListPartsOutput is one page of the parts of an upload.
type ListPartsOutput struct {
	Upload object.Upload
	Parts  []object.Part
	// Truncated reports that there are more parts; NextAfter is then the
	// number to continue after.
	Truncated bool
	NextAfter int
}

// ListParts lists the parts of a multipart upload in progress.
func (s *ObjectService) ListParts(ctx context.Context, principal iam.Principal, in ListPartsInput) (ListPartsOutput, error) {
	resource := iam.Resource{Bucket: in.Bucket, Key: in.Key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionListMultipartUploadParts, Resource: resource}); err != nil {
		return ListPartsOutput{}, fmt.Errorf("object.ListParts: %w", err)
	}
	limit := min(max(in.Max, 0), object.MaxListKeys)
	upload, err := s.uploads.GetUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return ListPartsOutput{}, fmt.Errorf("object.ListParts: %w", err)
	}
	if err := s.checkUploadKey(ctx, upload, in.CustomerKey); err != nil {
		return ListPartsOutput{}, fmt.Errorf("object.ListParts: %w", err)
	}
	parts, err := s.uploads.ListParts(ctx, in.Bucket, in.Key, in.UploadID, in.After, limit+1)
	if err != nil {
		return ListPartsOutput{}, fmt.Errorf("object.ListParts: %w", err)
	}
	out := ListPartsOutput{Upload: upload, Parts: parts}
	if limit == 0 {
		// Nothing was asked for, and nothing is to be continued.
		out.Parts = nil
		return out, nil
	}
	if len(parts) > limit {
		out.Parts, out.Truncated = parts[:limit], true
	}
	if len(out.Parts) > 0 {
		out.NextAfter = out.Parts[len(out.Parts)-1].Number
	}
	return out, nil
}

// ListUploadsInput asks for the multipart uploads in progress in a bucket.
type ListUploadsInput struct {
	Bucket    string
	Prefix    string
	Delimiter string
	// KeyMarker and UploadIDMarker say where the listing begins: after the
	// upload with that ID of that key, or, without an ID, after every
	// upload of that key.
	KeyMarker      string
	UploadIDMarker object.ID
	Max            int
}

// ListUploadsOutput is one page of uploads in progress.
type ListUploadsOutput struct {
	Uploads        []object.Upload
	CommonPrefixes []string
	Truncated      bool
	// NextKeyMarker and NextUploadIDMarker continue a truncated listing.
	NextKeyMarker      string
	NextUploadIDMarker object.ID
}

// ListUploads lists the multipart uploads in progress in a bucket, in order
// of key and, for one key, of upload ID.
func (s *ObjectService) ListUploads(ctx context.Context, principal iam.Principal, in ListUploadsInput) (ListUploadsOutput, error) {
	resource := iam.Resource{Bucket: in.Bucket}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionListBucketMultipartUploads, Resource: resource}); err != nil {
		return ListUploadsOutput{}, fmt.Errorf("object.ListUploads: %w", err)
	}
	limit := min(max(in.Max, 0), object.MaxListKeys)
	var out ListUploadsOutput
	if limit == 0 {
		// Nothing is asked for; the bucket must still exist.
		if _, err := s.buckets.GetBucket(ctx, in.Bucket); err != nil {
			return ListUploadsOutput{}, fmt.Errorf("object.ListUploads: %w", err)
		}
		return out, nil
	}
	start := ports.UploadCursor{Key: in.Prefix}
	if in.KeyMarker != "" && in.KeyMarker >= in.Prefix {
		start = ports.UploadCursor{Key: in.KeyMarker, After: in.UploadIDMarker}
		group, grouped := "", false
		if strings.HasPrefix(in.KeyMarker, in.Prefix) {
			group, grouped = object.CommonPrefix(in.KeyMarker, in.Prefix, in.Delimiter)
		}
		switch {
		case grouped:
			// The marker is a common prefix, or a key inside one, that an
			// earlier page listed: continue after everything in it.
			end, more := object.PrefixEnd(group)
			if !more {
				return out, nil
			}
			start = ports.UploadCursor{Key: end}
		case in.UploadIDMarker.IsZero():
			// After every upload of the marker key.
			for i := range start.After {
				start.After[i] = 0xff
			}
		}
	}
	count := 0
	err := s.uploads.ScanUploads(ctx, in.Bucket, start, func(u object.Upload) ports.ScanControl {
		switch {
		case !strings.HasPrefix(u.Key, in.Prefix):
			// Keys are ordered: past the prefix nothing can match.
			return ports.ScanControl{Stop: true}
		case count == limit:
			out.Truncated = true
			return ports.ScanControl{Stop: true}
		}
		count++
		if group, ok := object.CommonPrefix(u.Key, in.Prefix, in.Delimiter); ok {
			out.CommonPrefixes = append(out.CommonPrefixes, group)
			out.NextKeyMarker, out.NextUploadIDMarker = group, object.ID{}
			// Jump over every other upload of the group.
			end, more := object.PrefixEnd(group)
			return ports.ScanControl{Stop: !more, SeekTo: end}
		}
		out.Uploads = append(out.Uploads, u)
		out.NextKeyMarker, out.NextUploadIDMarker = u.Key, u.ID
		return ports.ScanControl{}
	})
	if err != nil {
		return ListUploadsOutput{}, fmt.Errorf("object.ListUploads: %w", err)
	}
	if !out.Truncated {
		out.NextKeyMarker, out.NextUploadIDMarker = "", object.ID{}
	}
	return out, nil
}

// openParts opens a version that is made of parts for reading span.
func (s *ObjectService) openParts(ctx context.Context, version object.Version, span object.Range, customerKey []byte) (io.ReadCloser, error) {
	if version.Encryption.SegmentSize <= 0 {
		return nil, fmt.Errorf("%w: segment size %d", domain.ErrIntegrity, version.Encryption.SegmentSize)
	}
	kmsKey, err := s.uploadKey(ctx, version.Encryption, version.DataID)
	if err != nil {
		return nil, err
	}
	// The part list is sealed under the KMS data key alone: completing an
	// upload makes the seals, and has no customer key to make them with.
	manifest, err := object.NewManifest(kmsKey)
	if err != nil {
		clear(kmsKey)
		return nil, err
	}
	uploadKey, err := dataKey(kmsKey, version.Encryption, customerKey)
	if err != nil {
		manifest.Clear()
		return nil, err
	}
	body := &partsBody{
		ctx: ctx, service: s, version: version, key: uploadKey, manifest: manifest,
		offset: span.Offset, remaining: span.Length, next: -1,
	}
	if version.Size == 0 {
		// Nothing to read; but that the object is empty is a claim of
		// the record like any other, and is checked.
		if err := body.verifyEmpty(); err != nil {
			_ = body.Close()
			return nil, err
		}
		return body, nil
	}
	if span.Length == 0 {
		return body, nil
	}
	// Open the first part and authenticate its first segment now, so that
	// damage is reported as an error rather than discovered after the
	// response has begun.
	if err := body.advance(); err != nil {
		_ = body.Close()
		return nil, err
	}
	if _, err := body.plain.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		_ = body.Close()
		return nil, err
	}
	return body, nil
}

// partsBody reads a range of a version made of parts: part after part, each
// one checked against the seal of its place before a byte of it is read.
type partsBody struct {
	// ctx is the context of the request that reads; io.Reader has none.
	ctx     context.Context
	service *ObjectService
	version object.Version
	// key is the data key of the upload, and manifest the seal made from
	// it; both are forgotten when the body is closed.
	key      []byte
	manifest *object.Manifest

	// offset is where the next byte is in the object; remaining is how
	// many are still to be read.
	offset    int64
	remaining int64
	// queue holds parts fetched and not yet opened; next is the index the
	// next part must have, or -1 before the first.
	queue []object.Part
	next  int

	// The part being read.
	plain    *bufio.Reader
	left     int64
	shards   *streamReader
	degraded bool
}

// readPartPage is the number of part records a read fetches at a time: a
// ranged read touches few parts.
const readPartPage = 16

// verifyEmpty checks an object that says it is empty: it has one part, which
// is empty, is sealed as the only part of an empty object, and whose stored
// stream is that of no bytes.
func (b *partsBody) verifyEmpty() error {
	parts, err := b.service.index.ObjectParts(b.ctx, b.version.Bucket, b.version.DataID, 0, 2)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return fmt.Errorf("%w: no part of %s", domain.ErrShardNotFound, b.version.DataID)
	}
	part := parts[0]
	if len(parts) != 1 || part.Index != 0 || part.Offset != 0 || part.Size != 0 {
		return fmt.Errorf("%w: %s is said to be empty and has parts that are not", domain.ErrIntegrity, b.version.DataID)
	}
	if err := b.manifest.Verify(part, b.version.Parts, 0); err != nil {
		return err
	}
	stream, err := partStreamWith(b.key, b.version.Encryption, part)
	if err != nil {
		return err
	}
	if part.Layout.StoredSize != stream.CiphertextSize(0) {
		return fmt.Errorf("%w: stored size %d of an empty part", domain.ErrIntegrity, part.Layout.StoredSize)
	}
	shards, err := b.service.storage.open(b.ctx, part.Extent())
	if err != nil {
		return err
	}
	defer func() {
		b.degraded = b.degraded || shards.Degraded()
		_ = shards.Close()
	}()
	section := stream.Locate(object.Range{}, 0)
	_, err = io.Copy(io.Discard, stream.Decrypt(io.NewSectionReader(shards, section.Offset, section.Length), section, 0))
	return err
}

// advance opens the part that holds the byte at b.offset.
func (b *partsBody) advance() error {
	b.closePart()
	if len(b.queue) == 0 {
		parts, err := b.service.index.ObjectParts(b.ctx, b.version.Bucket, b.version.DataID, b.offset, readPartPage)
		if err != nil {
			return err
		}
		if len(parts) == 0 {
			// What a version looks like just after it was removed.
			return fmt.Errorf("%w: no part of %s at offset %d", domain.ErrShardNotFound, b.version.DataID, b.offset)
		}
		b.queue = parts
	}
	part := b.queue[0]
	b.queue = b.queue[1:]
	within := b.offset - part.Offset
	switch {
	case within < 0 || within >= part.Size:
		return fmt.Errorf("%w: part %d does not hold offset %d", domain.ErrIntegrity, part.Number, b.offset)
	case b.next >= 0 && (part.Index != b.next || within != 0):
		return fmt.Errorf("%w: part %d does not follow the part before it", domain.ErrIntegrity, part.Number)
	}
	if err := b.manifest.Verify(part, b.version.Parts, b.version.Size); err != nil {
		return err
	}
	b.next = part.Index + 1

	stream, err := partStreamWith(b.key, b.version.Encryption, part)
	if err != nil {
		return err
	}
	if part.Layout.StoredSize != stream.CiphertextSize(part.Size) {
		return fmt.Errorf("%w: stored size %d does not match part size %d", domain.ErrIntegrity, part.Layout.StoredSize, part.Size)
	}
	shards, err := b.service.storage.open(b.ctx, part.Extent())
	if err != nil {
		return err
	}
	span := object.Range{Offset: within, Length: min(part.Size-within, b.remaining)}
	section := stream.Locate(span, part.Size)
	plain := bufio.NewReaderSize(
		stream.Decrypt(io.NewSectionReader(shards, section.Offset, section.Length), section, part.Size),
		b.version.Encryption.SegmentSize,
	)
	if _, err := plain.Discard(int(section.Skip)); err != nil {
		_ = shards.Close()
		return err
	}
	b.plain, b.left, b.shards = plain, span.Length, shards
	return nil
}

func (b *partsBody) closePart() {
	if b.shards == nil {
		return
	}
	b.degraded = b.degraded || b.shards.Degraded()
	_ = b.shards.Close()
	b.shards, b.plain, b.left = nil, nil, 0
}

// Read implements io.Reader. Every byte it returns has been authenticated.
func (b *partsBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if b.remaining == 0 {
			return 0, io.EOF
		}
		if b.left == 0 {
			if err := b.advance(); err != nil {
				return 0, err
			}
		}
		n, err := b.plain.Read(p[:min(int64(len(p)), b.left)])
		b.left -= int64(n)
		b.remaining -= int64(n)
		b.offset += int64(n)
		if errors.Is(err, io.EOF) {
			if b.left > 0 {
				return n, fmt.Errorf("%w: part ended %d bytes early", domain.ErrIntegrity, b.left)
			}
			err = nil
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// Close releases the shards and forgets the key. A read that had to work
// around a missing or damaged shard queues the object for healing.
func (b *partsBody) Close() error {
	b.closePart()
	clear(b.key)
	b.manifest.Clear()
	if b.degraded {
		b.service.healer.Enqueue(b.version)
	}
	return nil
}
