package porttest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// memUpload is a multipart upload in progress in a MemMetadata.
type memUpload struct {
	upload object.Upload
	parts  map[int]object.Part
}

func clonePart(p object.Part) object.Part {
	p.NoncePrefix = slices.Clone(p.NoncePrefix)
	p.Checksums = maps.Clone(p.Checksums)
	p.Seal = slices.Clone(p.Seal)
	p.Layout.Shards = slices.Clone(p.Layout.Shards)
	return p
}

func (m *MemMetadata) upload(bucketName, key string, id object.ID) (*memUpload, error) {
	u, ok := m.uploads[bucketName][id]
	if !ok || u.upload.Key != key {
		return nil, fmt.Errorf("%w: %s", domain.ErrUploadNotFound, id)
	}
	return u, nil
}

// CreateUpload implements ports.UploadStore.
func (m *MemMetadata) CreateUpload(_ context.Context, u object.Upload) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	b, ok := m.buckets[u.Bucket]
	if !ok {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, u.Bucket)
	}
	if err := object.ValidateKey(u.Key); err != nil {
		return err
	}
	switch {
	case u.ID.IsZero():
		return fmt.Errorf("%w: upload ID", domain.ErrInvalidArgument)
	case m.inUse(u.ID):
		return fmt.Errorf("%w: upload %s is in use", domain.ErrConflict, u.ID)
	case !b.ObjectLock && !u.Lock.IsZero():
		return fmt.Errorf("%w: bucket %s", domain.ErrLockNotConfigured, u.Bucket)
	case u.Encryption.KeyMode != object.KeyModePartHKDF:
		return fmt.Errorf("%w: key mode %q of an upload", domain.ErrInvalidArgument, u.Encryption.KeyMode)
	case u.ReplicaFrom != "" && object.ValidateReplicaSource(u.ReplicaFrom) != nil:
		return fmt.Errorf("%w: the source of a replica upload", domain.ErrInvalidArgument)
	case u.ReplicaFrom != b.ReplicaFrom:
		// A replica bucket takes the uploads of its source and no others;
		// no other bucket takes the upload of a source.
		return fmt.Errorf("%w: bucket %s and an upload from %q", domain.ErrInvalidBucketState, u.Bucket, u.ReplicaFrom)
	}
	if err := object.ValidateTags(u.Tags, object.MaxObjectTags); err != nil {
		return err
	}
	if m.uploads[u.Bucket] == nil {
		m.uploads[u.Bucket] = map[object.ID]*memUpload{}
	}
	u.Tags = maps.Clone(u.Tags)
	if len(u.Tags) == 0 {
		u.Tags = nil
	}
	u.Encryption.NoncePrefix = nil
	m.uploads[u.Bucket][u.ID] = &memUpload{upload: u, parts: map[int]object.Part{}}
	return nil
}

// GetUpload implements ports.UploadStore.
func (m *MemMetadata) GetUpload(_ context.Context, bucketName, key string, id object.ID) (object.Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.upload(bucketName, key, id)
	if err != nil {
		return object.Upload{}, err
	}
	return u.upload, nil
}

// PutPart implements ports.UploadStore.
func (m *MemMetadata) PutPart(_ context.Context, bucketName, key string, id object.ID, p object.Part) (*object.Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return nil, m.FailWrites
	}
	if m.FailParts != nil {
		return nil, m.FailParts
	}
	u, err := m.upload(bucketName, key, id)
	if err != nil {
		return nil, err
	}
	if err := object.ValidatePartNumber(p.Number); err != nil {
		return nil, err
	}
	switch {
	case p.Size < 0 || p.Size > object.MaxSize:
		return nil, fmt.Errorf("%w: part of %d bytes", domain.ErrEntityTooLarge, p.Size)
	case p.DataID.IsZero():
		return nil, fmt.Errorf("%w: part data ID", domain.ErrInvalidArgument)
	case m.inUse(p.DataID):
		return nil, fmt.Errorf("%w: part data %s is in use", domain.ErrConflict, p.DataID)
	}
	var replaced *object.Part
	net := p.Size
	if old, ok := u.parts[p.Number]; ok {
		replaced = &old
		net -= old.Size
	}
	if !m.admits(m.buckets[bucketName], net) {
		return nil, fmt.Errorf("%w: bucket %s", domain.ErrQuotaExceeded, bucketName)
	}
	stored := clonePart(p)
	stored.Index, stored.Offset, stored.Seal = 0, 0, nil
	u.parts[p.Number] = stored
	return replaced, nil
}

// ListParts implements ports.UploadStore.
func (m *MemMetadata) ListParts(_ context.Context, bucketName, key string, id object.ID, after, limit int) ([]object.Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.upload(bucketName, key, id)
	if err != nil {
		return nil, err
	}
	var out []object.Part
	for number, part := range u.parts {
		if number > after {
			out = append(out, clonePart(part))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	if len(out) > limit {
		out = out[:max(limit, 0)]
	}
	return out, nil
}

// CompleteUpload implements ports.UploadStore.
func (m *MemMetadata) CompleteUpload(_ context.Context, c ports.Completion) (ports.PutResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return ports.PutResult{}, m.FailWrites
	}
	b, ok := m.buckets[c.Bucket]
	if !ok {
		return ports.PutResult{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, c.Bucket)
	}
	u, err := m.upload(c.Bucket, c.Key, c.ID)
	if err != nil {
		return ports.PutResult{}, err
	}
	if c.VersionID.IsZero() {
		return ports.PutResult{}, fmt.Errorf("%w: version ID of the completion", domain.ErrInvalidArgument)
	}
	listed := make([]object.CompletedPart, len(c.Parts))
	for i, p := range c.Parts {
		listed[i] = object.CompletedPart{Number: p.Number, ETag: p.ETag, Checksums: p.Checksums}
	}
	total, err := object.CheckCompletion(listed, func(number int) (object.Part, bool) {
		p, ok := u.parts[number]
		return p, ok
	})
	if err != nil {
		return ports.PutResult{}, err
	}
	parts := make([]object.Part, len(c.Parts))
	var offset int64
	for i, p := range c.Parts {
		stored := u.parts[p.Number]
		if stored.DataID != p.DataID {
			return ports.PutResult{}, fmt.Errorf("%w: part %d was uploaded again while the upload was being completed", domain.ErrConflict, p.Number)
		}
		if len(p.Seal) != 32 {
			return ports.PutResult{}, fmt.Errorf("%w: seal of part %d", domain.ErrInvalidArgument, p.Number)
		}
		parts[i] = clonePart(stored)
		parts[i].Index, parts[i].Offset, parts[i].Seal = i, offset, slices.Clone(p.Seal)
		offset += stored.Size
	}
	up := u.upload
	if (c.Replica == nil) != (up.ReplicaFrom == "") || (c.Replica != nil && c.Replica.From != up.ReplicaFrom) {
		return ports.PutResult{}, fmt.Errorf("%w: an upload of a source is completed as a replica of it, and no other upload is", domain.ErrInvalidBucketState)
	}
	var uploadBytes int64
	for _, part := range u.parts {
		uploadBytes += part.Size
	}
	result, err := m.commit(b, object.Version{
		Bucket: up.Bucket, Key: up.Key, VersionID: c.VersionID, DataID: up.ID, Size: total, ETag: c.ETag,
		ModifiedAt: c.At.UTC(), Parts: len(parts), Lock: up.Lock, Encryption: up.Encryption,
		ContentType: up.ContentType, ContentEncoding: up.ContentEncoding, ContentDisposition: up.ContentDisposition,
		ContentLanguage: up.ContentLanguage, CacheControl: up.CacheControl, UserMetadata: up.UserMetadata,
		Tags: maps.Clone(up.Tags), CreatedBy: c.CreatedBy, Replica: c.Replica,
	}, delivery.EventComplete, uploadBytes)
	if err != nil {
		return ports.PutResult{}, err
	}
	listedNumbers := make(map[int]bool, len(parts))
	for _, p := range parts {
		listedNumbers[p.Number] = true
	}
	result.RemovedParts = append(result.RemovedParts, sortedParts(u.parts, listedNumbers)...)
	delete(m.uploads[c.Bucket], c.ID)
	m.extents[c.ID] = parts
	return result, nil
}

// AbortUpload implements ports.UploadStore.
func (m *MemMetadata) AbortUpload(_ context.Context, bucketName, key string, id object.ID) ([]object.Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return nil, m.FailWrites
	}
	u, err := m.upload(bucketName, key, id)
	if err != nil {
		return nil, err
	}
	delete(m.uploads[bucketName], id)
	return sortedParts(u.parts, nil), nil
}

// sortedParts returns the parts of an upload in order of number, without
// those whose number is in skip.
func sortedParts(parts map[int]object.Part, skip map[int]bool) []object.Part {
	var out []object.Part
	for number, part := range parts {
		if !skip[number] {
			out = append(out, clonePart(part))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// ScanUploads implements ports.UploadStore.
func (m *MemMetadata) ScanUploads(_ context.Context, bucketName string, start ports.UploadCursor, visit func(object.Upload) ports.ScanControl) error {
	m.mu.Lock()
	if _, ok := m.buckets[bucketName]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	uploads := make([]object.Upload, 0, len(m.uploads[bucketName]))
	for _, u := range m.uploads[bucketName] {
		uploads = append(uploads, u.upload)
	}
	m.mu.Unlock()
	less := func(aKey string, aID object.ID, bKey string, bID object.ID) bool {
		if aKey != bKey {
			return aKey < bKey
		}
		return slices.Compare(aID[:], bID[:]) < 0
	}
	sort.Slice(uploads, func(i, j int) bool { return less(uploads[i].Key, uploads[i].ID, uploads[j].Key, uploads[j].ID) })
	seek := ""
	for _, u := range uploads {
		switch {
		case seek != "" && u.Key < seek:
			continue
		case u.Key < start.Key:
			continue
		case u.Key == start.Key && !start.After.IsZero() && !less(start.Key, start.After, u.Key, u.ID):
			continue
		}
		control := visit(u)
		if control.Stop {
			return nil
		}
		if control.SeekTo != "" {
			seek = control.SeekTo
		}
	}
	return nil
}

// ObjectParts implements ports.ObjectIndex.
func (m *MemMetadata) ObjectParts(_ context.Context, _ string, dataID object.ID, offset int64, limit int) ([]object.Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []object.Part
	for _, part := range m.extents[dataID] {
		if len(out) >= limit {
			break
		}
		if part.Offset+part.Size <= offset && (part.Size > 0 || part.Offset < offset) {
			continue
		}
		out = append(out, clonePart(part))
	}
	return out, nil
}

// TamperParts changes the stored parts of a completed object in place, as
// nothing but a fault or an attacker with the metadata store in hand could.
func (m *MemMetadata) TamperParts(dataID object.ID, change func(parts []object.Part) []object.Part) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extents[dataID] = change(m.extents[dataID])
}
