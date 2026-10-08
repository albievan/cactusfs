package raftmeta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Multipart uploads in the state (ADR-0012).
//
//	uploads/<bucket>      upload ID                       -> uploadRecord
//	uploadkeys/<bucket>   enc(key) ‖ upload ID            -> nothing; the listing order
//	parts/<bucket>        upload ID ‖ part number         -> partRecord, while in progress
//	extents/<bucket>      version data ID ‖ offset        -> partRecord, of a completed object
//
// The data ID of a version made by an upload is the upload's ID. The "data"
// index holds every ID in use: of plain versions, of parts, and of uploads
// and the versions they became. An ID in it is given to nothing else.
// maxDroppedParts bounds how many removed parts an answer names. The data of
// the rest is found by the collector, as all unreferenced data is.
const maxDroppedParts = 1000

const (
	cmdCreateUpload   = "upload.create"
	cmdPutPart        = "upload.part.put"
	cmdCompleteUpload = "upload.complete"
	cmdAbortUpload    = "upload.abort"
)

func uploadsBucket() []byte    { return []byte("uploads") }
func uploadKeysBucket() []byte { return []byte("uploadkeys") }
func partsBucket() []byte      { return []byte("parts") }
func extentsBucket() []byte    { return []byte("extents") }

// uploadRecord is a multipart upload in progress.
type uploadRecord struct {
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	ID          string    `json:"id"`
	InitiatedAt time.Time `json:"initiated_at"`
	Initiator   string    `json:"initiator,omitempty"`

	ContentType        string            `json:"content_type,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	ContentLanguage    string            `json:"content_language,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	UserMetadata       map[string]string `json:"user_metadata,omitempty"`

	Lock       *lockRecord      `json:"lock,omitempty"`
	Encryption encryptionRecord `json:"encryption"`
}

// partRecord is a part: of an upload in progress, or, with its place and its
// seal, of a completed object.
type partRecord struct {
	Number      int               `json:"number"`
	DataID      string            `json:"data_id"`
	Size        int64             `json:"size"`
	ETag        string            `json:"etag"`
	Checksums   map[string]string `json:"checksums,omitempty"`
	NoncePrefix []byte            `json:"nonce_prefix"`
	Layout      layoutRecord      `json:"layout"`
	ModifiedAt  time.Time         `json:"modified_at"`

	Index  int    `json:"index,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Seal   []byte `json:"seal,omitempty"`
}

// putPartCmd records a part of an upload.
type putPartCmd struct {
	Bucket   string     `json:"bucket"`
	Key      string     `json:"key"`
	UploadID string     `json:"upload_id"`
	Part     partRecord `json:"part"`
}

// uploadRefCmd names an upload: to abort it.
type uploadRefCmd struct {
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	UploadID string `json:"upload_id"`
}

// completeUploadCmd turns an upload into a version.
type completeUploadCmd struct {
	Bucket     string       `json:"bucket"`
	Key        string       `json:"key"`
	UploadID   string       `json:"upload_id"`
	VersionID  string       `json:"version_id"`
	ETag       string       `json:"etag"`
	ModifiedAt time.Time    `json:"modified_at"`
	Parts      []listedPart `json:"parts"`
}

// listedPart is a part as a completion names it: the number and the entity
// tag the client listed, the data of the stored part the proposer sealed,
// and the seal.
type listedPart struct {
	Number    int               `json:"number"`
	ETag      string            `json:"etag"`
	Checksums map[string]string `json:"checksums,omitempty"`
	DataID    string            `json:"data_id"`
	Seal      []byte            `json:"seal"`
}

func layoutToRecord(l object.Layout) layoutRecord {
	shards := make([]shardRecord, len(l.Shards))
	for i, s := range l.Shards {
		shards[i] = shardRecord{DriveID: s.DriveID, Size: s.Size}
	}
	return layoutRecord{DataShards: l.DataShards, ParityShards: l.ParityShards, StripeSize: l.StripeSize, StoredSize: l.StoredSize, Shards: shards}
}

func (r layoutRecord) toDomain() object.Layout {
	shards := make([]object.ShardLocation, len(r.Shards))
	for i, s := range r.Shards {
		shards[i] = object.ShardLocation{DriveID: s.DriveID, Size: s.Size}
	}
	return object.Layout{DataShards: r.DataShards, ParityShards: r.ParityShards, StripeSize: r.StripeSize, StoredSize: r.StoredSize, Shards: shards}
}

func partToRecord(p object.Part) partRecord {
	return partRecord{
		Number: p.Number, DataID: p.DataID.String(), Size: p.Size, ETag: p.ETag, Checksums: p.Checksums, NoncePrefix: p.NoncePrefix,
		Layout: layoutToRecord(p.Layout), ModifiedAt: p.ModifiedAt.UTC(), Index: p.Index, Offset: p.Offset, Seal: p.Seal,
	}
}

func (r partRecord) toDomain() (object.Part, error) {
	id, err := object.ParseID(r.DataID)
	if err != nil {
		return object.Part{}, fmt.Errorf("%w: stored part: data ID: %w", domain.ErrIntegrity, err)
	}
	return object.Part{
		Number: r.Number, DataID: id, Size: r.Size, ETag: r.ETag, Checksums: r.Checksums, NoncePrefix: r.NoncePrefix,
		Layout: r.Layout.toDomain(), ModifiedAt: r.ModifiedAt, Index: r.Index, Offset: r.Offset, Seal: r.Seal,
	}, nil
}

func uploadToRecord(u object.Upload) *uploadRecord {
	return &uploadRecord{
		Bucket: u.Bucket, Key: u.Key, ID: u.ID.String(), InitiatedAt: u.InitiatedAt.UTC(), Initiator: u.Initiator,
		ContentType: u.ContentType, ContentEncoding: u.ContentEncoding, ContentDisposition: u.ContentDisposition,
		ContentLanguage: u.ContentLanguage, CacheControl: u.CacheControl, UserMetadata: u.UserMetadata,
		Lock: lockToRecord(u.Lock),
		Encryption: encryptionRecord{
			Scheme: u.Encryption.Scheme, KMSKeyID: u.Encryption.KMSKeyID, WrappedKey: u.Encryption.WrappedKey,
			SegmentSize: u.Encryption.SegmentSize, KeyMode: u.Encryption.KeyMode,
		},
	}
}

func (r uploadRecord) toDomain() (object.Upload, error) {
	id, err := object.ParseID(r.ID)
	if err != nil {
		return object.Upload{}, fmt.Errorf("%w: stored upload: ID: %w", domain.ErrIntegrity, err)
	}
	return object.Upload{
		Bucket: r.Bucket, Key: r.Key, ID: id, InitiatedAt: r.InitiatedAt, Initiator: r.Initiator,
		ContentType: r.ContentType, ContentEncoding: r.ContentEncoding, ContentDisposition: r.ContentDisposition,
		ContentLanguage: r.ContentLanguage, CacheControl: r.CacheControl, UserMetadata: r.UserMetadata,
		Lock: r.Lock.toDomain(),
		Encryption: object.Encryption{
			Scheme: r.Encryption.Scheme, KMSKeyID: r.Encryption.KMSKeyID, WrappedKey: r.Encryption.WrappedKey,
			SegmentSize: r.Encryption.SegmentSize, KeyMode: r.Encryption.KeyMode,
		},
	}, nil
}

// nested returns the bucket of bucketName under a top-level bucket, creating
// it when create is set. It is nil when there is none.
func nested(tx *bbolt.Tx, parent []byte, bucketName string, create bool) (*bbolt.Bucket, error) {
	holder := tx.Bucket(parent)
	if holder == nil {
		return nil, fmt.Errorf("%w: metadata state has no %q", domain.ErrIntegrity, parent)
	}
	if create {
		return holder.CreateBucketIfNotExists([]byte(bucketName))
	}
	return holder.Bucket([]byte(bucketName)), nil
}

// partKey is the key of a part of an upload. The number is written in
// decimal with a fixed width, so that keys sort as numbers do.
func partKey(uploadID object.ID, number int) []byte {
	return fmt.Appendf(uploadID[:], "%05d", number)
}

// extentKey is the key of a part of a completed object: by the offset of its
// first byte, so that the part that holds a byte is found by seeking.
func extentKey(dataID object.ID, offset int64) []byte {
	return fmt.Appendf(dataID[:], "%020d", offset)
}

func decodePart(raw []byte) (*partRecord, error) {
	var p partRecord
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: stored part: %w", domain.ErrIntegrity, err)
	}
	return &p, nil
}

// readUpload returns an upload in progress, or nil: when there is none with
// the ID, or it is the upload of another key.
func readUpload(tx *bbolt.Tx, bucketName, key string, id object.ID) (*uploadRecord, error) {
	uploads, err := nested(tx, uploadsBucket(), bucketName, false)
	if err != nil || uploads == nil {
		return nil, err
	}
	raw := uploads.Get(id[:])
	if raw == nil {
		return nil, nil
	}
	var u uploadRecord
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("%w: stored upload: %w", domain.ErrIntegrity, err)
	}
	if u.Key != key {
		return nil, nil
	}
	return &u, nil
}

// freshID parses an ID an entry proposes for something new and reports, as a
// refusal, why it cannot be used: it is not an ID, or it is taken.
func freshID(tx *bbolt.Tx, raw, what string) (object.ID, *applyResult) {
	id, err := object.ParseID(raw)
	if err != nil || id.IsZero() {
		result, _ := refuse(domain.ErrInvalidArgument, "%s ID", what)
		return object.ID{}, &result
	}
	// An ID belongs to one thing. A second owner could have it taken from
	// under the first, by being removed itself.
	if tx.Bucket(dataBucket()).Get(id[:]) != nil {
		result, _ := refuse(domain.ErrConflict, "%s %s is in use", what, raw)
		return object.ID{}, &result
	}
	return id, nil
}

func applyCreateUpload(tx *bbolt.Tx, u uploadRecord) (applyResult, error) {
	b, err := readBucket(tx, u.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", u.Bucket)
	}
	if err := object.ValidateKey(u.Key); err != nil {
		return applyResult{err: err}, nil
	}
	id, refused := freshID(tx, u.ID, "upload")
	if refused != nil {
		return *refused, nil
	}
	// Whether a retention can still be kept is decided when the upload is
	// completed, on the time of that entry. That the bucket keeps locks
	// at all is known now.
	if !b.ObjectLock && !u.Lock.toDomain().IsZero() {
		return refuse(domain.ErrLockNotConfigured, "bucket %s was not created with Object Lock", u.Bucket)
	}
	if u.Encryption.KeyMode != object.KeyModePartHKDF {
		return refuse(domain.ErrInvalidArgument, "key mode %q of an upload", u.Encryption.KeyMode)
	}
	data, err := json.Marshal(u)
	if err != nil {
		return applyResult{}, err
	}
	uploads, err := nested(tx, uploadsBucket(), u.Bucket, true)
	if err != nil {
		return applyResult{}, err
	}
	keys, err := nested(tx, uploadKeysBucket(), u.Bucket, true)
	if err != nil {
		return applyResult{}, err
	}
	if err := uploads.Put(id[:], data); err != nil {
		return applyResult{}, err
	}
	if err := keys.Put(append(encodeKey(u.Key), id[:]...), []byte{}); err != nil {
		return applyResult{}, err
	}
	return applyResult{}, addDataReference(tx.Bucket(dataBucket()), u.ID, 1)
}

func applyPutPart(tx *bbolt.Tx, cmd putPartCmd) (applyResult, error) {
	uploadID, err := object.ParseID(cmd.UploadID)
	if err != nil {
		return refuse(domain.ErrUploadNotFound, "%s", cmd.UploadID)
	}
	upload, err := readUpload(tx, cmd.Bucket, cmd.Key, uploadID)
	if err != nil {
		return applyResult{}, err
	}
	if upload == nil {
		return refuse(domain.ErrUploadNotFound, "%s", cmd.UploadID)
	}
	p := cmd.Part
	if err := object.ValidatePartNumber(p.Number); err != nil {
		return applyResult{err: err}, nil
	}
	if p.Size < 0 || p.Size > object.MaxSize {
		return refuse(domain.ErrEntityTooLarge, "part of %d bytes", p.Size)
	}
	if _, refused := freshID(tx, p.DataID, "part data"); refused != nil {
		return *refused, nil
	}
	// A stored part has no place yet.
	p.Index, p.Offset, p.Seal = 0, 0, nil
	parts, err := nested(tx, partsBucket(), cmd.Bucket, true)
	if err != nil {
		return applyResult{}, err
	}
	key := partKey(uploadID, p.Number)
	var replaced []partRecord
	if raw := parts.Get(key); raw != nil {
		old, err := decodePart(raw)
		if err != nil {
			return applyResult{}, err
		}
		if err := addDataReference(tx.Bucket(dataBucket()), old.DataID, -1); err != nil {
			return applyResult{}, err
		}
		replaced = []partRecord{*old}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return applyResult{}, err
	}
	if err := parts.Put(key, data); err != nil {
		return applyResult{}, err
	}
	return applyResult{parts: replaced}, addDataReference(tx.Bucket(dataBucket()), p.DataID, 1)
}

// dropParts removes the parts of an upload from the state. keep names the
// data that something else has taken over; the reference of every other part
// to its data is taken back, and those parts are returned, up to
// maxDroppedParts of them.
func dropParts(tx *bbolt.Tx, bucketName string, uploadID object.ID, keep map[string]bool) ([]partRecord, error) {
	parts, err := nested(tx, partsBucket(), bucketName, false)
	if err != nil || parts == nil {
		return nil, err
	}
	var dropped []partRecord
	c := parts.Cursor()
	for k, raw := c.Seek(uploadID[:]); k != nil && bytes.HasPrefix(k, uploadID[:]); k, raw = c.Next() {
		p, err := decodePart(raw)
		if err != nil {
			return nil, err
		}
		if !keep[p.DataID] {
			if err := addDataReference(tx.Bucket(dataBucket()), p.DataID, -1); err != nil {
				return nil, err
			}
			if len(dropped) < maxDroppedParts {
				dropped = append(dropped, *p)
			}
		}
		if err := c.Delete(); err != nil {
			return nil, err
		}
	}
	return dropped, nil
}

// dropUpload removes the record of an upload, not its parts and not the
// reference of its ID.
func dropUpload(tx *bbolt.Tx, u uploadRecord, id object.ID) error {
	for parent, key := range map[string][]byte{
		string(uploadsBucket()):    id[:],
		string(uploadKeysBucket()): append(encodeKey(u.Key), id[:]...),
	} {
		index, err := nested(tx, []byte(parent), u.Bucket, false)
		if err != nil {
			return err
		}
		if index == nil {
			continue
		}
		if err := index.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func applyAbortUpload(tx *bbolt.Tx, cmd uploadRefCmd) (applyResult, error) {
	id, err := object.ParseID(cmd.UploadID)
	if err != nil {
		return refuse(domain.ErrUploadNotFound, "%s", cmd.UploadID)
	}
	upload, err := readUpload(tx, cmd.Bucket, cmd.Key, id)
	if err != nil {
		return applyResult{}, err
	}
	if upload == nil {
		return refuse(domain.ErrUploadNotFound, "%s", cmd.UploadID)
	}
	dropped, err := dropParts(tx, cmd.Bucket, id, nil)
	if err != nil {
		return applyResult{}, err
	}
	if err := dropUpload(tx, *upload, id); err != nil {
		return applyResult{}, err
	}
	return applyResult{parts: dropped}, addDataReference(tx.Bucket(dataBucket()), upload.ID, -1)
}

// applyCompleteUpload turns an upload into a version of its key. Whether the
// listed parts make an object is decided here, against the parts as this
// entry finds them: a part uploaded again since the proposer looked is seen.
// Nothing is changed before everything is known to be in order.
func applyCompleteUpload(tx *bbolt.Tx, cmd completeUploadCmd, now applied) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil || tx.Bucket(objectsBucket()).Bucket([]byte(cmd.Bucket)) == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	id, err := object.ParseID(cmd.UploadID)
	if err != nil {
		return refuse(domain.ErrUploadNotFound, "%s", cmd.UploadID)
	}
	upload, err := readUpload(tx, cmd.Bucket, cmd.Key, id)
	if err != nil {
		return applyResult{}, err
	}
	if upload == nil {
		return refuse(domain.ErrUploadNotFound, "%s", cmd.UploadID)
	}
	if versionID, err := object.ParseID(cmd.VersionID); err != nil || versionID.IsZero() {
		return refuse(domain.ErrInvalidArgument, "version ID of the completion")
	}

	parts, err := nested(tx, partsBucket(), cmd.Bucket, false)
	if err != nil {
		return applyResult{}, err
	}
	stored := make(map[int]*partRecord, len(cmd.Parts))
	var damaged error
	listed := make([]object.CompletedPart, len(cmd.Parts))
	for i, p := range cmd.Parts {
		listed[i] = object.CompletedPart{Number: p.Number, ETag: p.ETag, Checksums: p.Checksums}
	}
	total, err := object.CheckCompletion(listed, func(number int) (object.Part, bool) {
		if parts == nil {
			return object.Part{}, false
		}
		raw := parts.Get(partKey(id, number))
		if raw == nil {
			return object.Part{}, false
		}
		p, err := decodePart(raw)
		if err != nil {
			damaged = err
			return object.Part{}, false
		}
		stored[number] = p
		return object.Part{Number: p.Number, Size: p.Size, ETag: p.ETag, Checksums: p.Checksums}, true
	})
	if damaged != nil {
		return applyResult{}, damaged
	}
	if err != nil {
		return applyResult{err: err}, nil
	}
	keep := make(map[string]bool, len(cmd.Parts))
	for _, p := range cmd.Parts {
		// The seal was made for the data the proposer saw. If the part
		// was uploaded again since, it seals something else.
		if stored[p.Number].DataID != p.DataID {
			return refuse(domain.ErrConflict, "part %d was uploaded again while the upload was being completed", p.Number)
		}
		if len(p.Seal) != sha256.Size {
			return refuse(domain.ErrInvalidArgument, "seal of part %d", p.Number)
		}
		keep[p.DataID] = true
	}

	v := versionRecord{
		Bucket: upload.Bucket, Key: upload.Key, VersionID: cmd.VersionID, DataID: upload.ID,
		Size: total, ETag: cmd.ETag, ModifiedAt: cmd.ModifiedAt, Parts: len(cmd.Parts), Lock: upload.Lock,
		ContentType: upload.ContentType, ContentEncoding: upload.ContentEncoding, ContentDisposition: upload.ContentDisposition,
		ContentLanguage: upload.ContentLanguage, CacheControl: upload.CacheControl, UserMetadata: upload.UserMetadata,
		Encryption: upload.Encryption,
	}
	removed, refused, err := commitVersion(tx, b, &v, now)
	if err != nil {
		return applyResult{}, err
	}
	if refused != nil {
		return *refused, nil
	}

	// The version is in the state. Its parts follow it; the rest of the
	// upload goes.
	extents, err := nested(tx, extentsBucket(), cmd.Bucket, true)
	if err != nil {
		return applyResult{}, err
	}
	var offset int64
	for i, p := range cmd.Parts {
		part := *stored[p.Number]
		part.Index, part.Offset, part.Seal = i, offset, p.Seal
		data, err := json.Marshal(part)
		if err != nil {
			return applyResult{}, err
		}
		if err := extents.Put(extentKey(id, offset), data); err != nil {
			return applyResult{}, err
		}
		offset += part.Size
	}
	unlisted, err := dropParts(tx, cmd.Bucket, id, keep)
	if err != nil {
		return applyResult{}, err
	}
	if err := dropUpload(tx, *upload, id); err != nil {
		return applyResult{}, err
	}
	if removed != nil {
		if err := dropData(tx, removed); err != nil {
			return applyResult{}, err
		}
	}
	// One answer, one bound: it travels to the node that proposed.
	if removed != nil {
		unlisted = unlisted[:min(len(unlisted), maxDroppedParts-len(removed.Dropped))]
	}
	committed := v
	return applyResult{previous: removed, stored: &committed, parts: unlisted, events: []ports.Event{
		{Type: ports.EventObjectCreated, Time: v.ModifiedAt, Data: objectEvent(v)},
	}}, nil
}

// dropExtents removes the parts of a completed object that is being removed
// for good, takes back their references to their data, and returns them, up
// to maxDroppedParts of them.
func dropExtents(tx *bbolt.Tx, bucketName string, dataID object.ID) ([]partRecord, error) {
	extents, err := nested(tx, extentsBucket(), bucketName, false)
	if err != nil || extents == nil {
		return nil, err
	}
	var dropped []partRecord
	c := extents.Cursor()
	for k, raw := c.Seek(dataID[:]); k != nil && bytes.HasPrefix(k, dataID[:]); k, raw = c.Next() {
		p, err := decodePart(raw)
		if err != nil {
			return nil, err
		}
		if err := addDataReference(tx.Bucket(dataBucket()), p.DataID, -1); err != nil {
			return nil, err
		}
		if len(dropped) < maxDroppedParts {
			dropped = append(dropped, *p)
		}
		if err := c.Delete(); err != nil {
			return nil, err
		}
	}
	return dropped, nil
}

// dropUploads removes every upload in progress in a bucket that is being
// deleted, with its parts.
func dropUploads(tx *bbolt.Tx, bucketName string) error {
	index := tx.Bucket(dataBucket())
	parts, err := nested(tx, partsBucket(), bucketName, false)
	if err != nil {
		return err
	}
	if parts != nil {
		err := parts.ForEach(func(_, raw []byte) error {
			p, err := decodePart(raw)
			if err != nil {
				return err
			}
			return addDataReference(index, p.DataID, -1)
		})
		if err != nil {
			return err
		}
	}
	uploads, err := nested(tx, uploadsBucket(), bucketName, false)
	if err != nil {
		return err
	}
	if uploads != nil {
		err := uploads.ForEach(func(id, _ []byte) error {
			if len(id) != object.IDSize {
				return fmt.Errorf("%w: stored upload ID", domain.ErrIntegrity)
			}
			return addDataReference(index, object.ID(id).String(), -1)
		})
		if err != nil {
			return err
		}
	}
	for _, parent := range [][]byte{uploadsBucket(), uploadKeysBucket(), partsBucket(), extentsBucket()} {
		holder := tx.Bucket(parent)
		if holder != nil && holder.Bucket([]byte(bucketName)) != nil {
			if err := holder.DeleteBucket([]byte(bucketName)); err != nil {
				return err
			}
		}
	}
	return nil
}

// CreateUpload implements ports.UploadStore.
func (s *Store) CreateUpload(ctx context.Context, u object.Upload) error {
	if _, err := s.propose(ctx, entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}); err != nil {
		return fmt.Errorf("raftmeta.CreateUpload: %w", err)
	}
	return nil
}

// GetUpload implements ports.UploadStore.
func (s *Store) GetUpload(ctx context.Context, bucketName, key string, id object.ID) (object.Upload, error) {
	var upload object.Upload
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		record, err := readUpload(tx, bucketName, key, id)
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("%w: %s", domain.ErrUploadNotFound, id)
		}
		upload, err = record.toDomain()
		return err
	})
	if err != nil {
		return object.Upload{}, fmt.Errorf("raftmeta.GetUpload: %w", err)
	}
	return upload, nil
}

// PutPart implements ports.UploadStore.
func (s *Store) PutPart(ctx context.Context, bucketName, key string, id object.ID, p object.Part) (*object.Part, error) {
	result, err := s.propose(ctx, entry{Type: cmdPutPart, PutPart: &putPartCmd{
		Bucket: bucketName, Key: key, UploadID: id.String(), Part: partToRecord(p),
	}})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.PutPart: %w", err)
	}
	if len(result.parts) == 0 {
		return nil, nil
	}
	// The part is committed. An answer that cannot be read is not a
	// failure of the put.
	replaced, err := result.parts[0].toDomain()
	if err != nil {
		return nil, fmt.Errorf("raftmeta.PutPart: %w: %w", domain.ErrOutcomeUnknown, err)
	}
	return &replaced, nil
}

// partsOf reads the parts an answer names. One that cannot be read is left
// out: the collector finds its data.
func partsOf(records []partRecord) []object.Part {
	var out []object.Part
	for _, record := range records {
		if part, err := record.toDomain(); err == nil {
			out = append(out, part)
		}
	}
	return out
}

// removedParts returns the parts that went with a removed version.
func removedParts(removed *versionRecord) []object.Part {
	if removed == nil {
		return nil
	}
	return partsOf(removed.Dropped)
}

// ListParts implements ports.UploadStore.
func (s *Store) ListParts(ctx context.Context, bucketName, key string, id object.ID, after, limit int) ([]object.Part, error) {
	var out []object.Part
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		out = nil
		upload, err := readUpload(tx, bucketName, key, id)
		if err != nil {
			return err
		}
		if upload == nil {
			return fmt.Errorf("%w: %s", domain.ErrUploadNotFound, id)
		}
		parts, err := nested(tx, partsBucket(), bucketName, false)
		if err != nil || parts == nil {
			return err
		}
		c := parts.Cursor()
		for k, raw := c.Seek(partKey(id, max(after, 0)+1)); k != nil && bytes.HasPrefix(k, id[:]) && len(out) < limit; k, raw = c.Next() {
			record, err := decodePart(raw)
			if err != nil {
				return err
			}
			part, err := record.toDomain()
			if err != nil {
				return err
			}
			out = append(out, part)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListParts: %w", err)
	}
	return out, nil
}

// CompleteUpload implements ports.UploadStore.
func (s *Store) CompleteUpload(ctx context.Context, c ports.Completion) (ports.PutResult, error) {
	cmd := &completeUploadCmd{
		Bucket: c.Bucket, Key: c.Key, UploadID: c.ID.String(), VersionID: c.VersionID.String(),
		ETag: c.ETag, ModifiedAt: c.At.UTC(), Parts: make([]listedPart, len(c.Parts)),
	}
	for i, p := range c.Parts {
		cmd.Parts[i] = listedPart{Number: p.Number, ETag: p.ETag, Checksums: p.Checksums, DataID: p.DataID.String(), Seal: p.Seal}
	}
	result, err := s.propose(ctx, entry{Type: cmdCompleteUpload, CompleteUpload: cmd})
	if err != nil {
		return ports.PutResult{}, fmt.Errorf("raftmeta.CompleteUpload: %w", err)
	}
	// From here on the version is committed.
	stored, err := versionOfRecord(result.stored)
	if err == nil && stored == nil {
		err = errors.New("the leader did not say what it stored")
	}
	if err != nil {
		return ports.PutResult{}, fmt.Errorf("raftmeta.CompleteUpload: %w: %w", domain.ErrOutcomeUnknown, err)
	}
	removed, err := versionOfRecord(result.previous)
	if err != nil {
		return ports.PutResult{}, fmt.Errorf("raftmeta.CompleteUpload: %w: %w", domain.ErrOutcomeUnknown, err)
	}
	return ports.PutResult{Stored: *stored, Removed: removed, RemovedParts: append(removedParts(result.previous), partsOf(result.parts)...)}, nil
}

// AbortUpload implements ports.UploadStore.
func (s *Store) AbortUpload(ctx context.Context, bucketName, key string, id object.ID) ([]object.Part, error) {
	e := entry{Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: bucketName, Key: key, UploadID: id.String()}}
	result, err := s.propose(ctx, e)
	if err != nil {
		return nil, fmt.Errorf("raftmeta.AbortUpload: %w", err)
	}
	return partsOf(result.parts), nil
}

// ScanUploads implements ports.UploadStore. The whole scan sees one state.
func (s *Store) ScanUploads(ctx context.Context, bucketName string, start ports.UploadCursor, visit func(object.Upload) ports.ScanControl) error {
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		if b, err := readBucket(tx, bucketName); err != nil {
			return err
		} else if b == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		keys, err := nested(tx, uploadKeysBucket(), bucketName, false)
		if err != nil || keys == nil {
			return err
		}
		uploads, err := nested(tx, uploadsBucket(), bucketName, false)
		if err != nil || uploads == nil {
			return err
		}
		c := keys.Cursor()
		from := append(encodeKey(start.Key), start.After[:]...)
		k, _ := c.Seek(from)
		if !start.After.IsZero() && bytes.Equal(k, from) {
			k, _ = c.Next()
		}
		for k != nil {
			if len(k) < object.IDSize {
				return fmt.Errorf("%w: stored upload key", domain.ErrIntegrity)
			}
			raw := uploads.Get(k[len(k)-object.IDSize:])
			if raw == nil {
				return fmt.Errorf("%w: upload listed and not stored", domain.ErrIntegrity)
			}
			var record uploadRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored upload: %w", domain.ErrIntegrity, err)
			}
			upload, err := record.toDomain()
			if err != nil {
				return err
			}
			control := visit(upload)
			switch {
			case control.Stop:
				return nil
			case control.SeekTo != "":
				k, _ = c.Seek(encodeKey(control.SeekTo))
			default:
				k, _ = c.Next()
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("raftmeta.ScanUploads: %w", err)
	}
	return nil
}

// ObjectParts implements ports.ObjectIndex.
func (s *Store) ObjectParts(ctx context.Context, bucketName string, dataID object.ID, offset int64, limit int) ([]object.Part, error) {
	var out []object.Part
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		out = nil
		extents, err := nested(tx, extentsBucket(), bucketName, false)
		if err != nil || extents == nil {
			return err
		}
		c := extents.Cursor()
		at := extentKey(dataID, max(offset, 0))
		k, raw := c.Seek(at)
		if !bytes.Equal(k, at) {
			// The part that holds the byte begins before it.
			var pk, praw []byte
			if k == nil {
				pk, praw = c.Last()
			} else {
				pk, praw = c.Prev()
			}
			if pk != nil && bytes.HasPrefix(pk, dataID[:]) {
				k, raw = pk, praw
			} else {
				k, raw = c.Seek(at)
			}
		}
		for ; k != nil && bytes.HasPrefix(k, dataID[:]) && len(out) < limit; k, raw = c.Next() {
			record, err := decodePart(raw)
			if err != nil {
				return err
			}
			part, err := record.toDomain()
			if err != nil {
				return err
			}
			if part.Offset+part.Size <= offset && part.Size > 0 {
				continue
			}
			out = append(out, part)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ObjectParts: %w", err)
	}
	return out, nil
}
