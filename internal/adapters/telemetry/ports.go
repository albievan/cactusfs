package telemetry

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Metadata is the set of metadata ports a store implements together.
type Metadata interface {
	ports.BucketStore
	ports.ObjectIndex
	ports.RootCredentialStore
	ports.TopologyStore
}

// InstrumentKMS returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentKMS(next ports.KMS) ports.KMS {
	return &kms{t: t, next: next}
}

type kms struct {
	t    *Telemetry
	next ports.KMS
}

const portKMS = "kms"

// KeyID implements ports.KMS.
func (k *kms) KeyID() string { return k.next.KeyID() }

// GenerateDataKey implements ports.KMS.
func (k *kms) GenerateDataKey(ctx context.Context, aad []byte) (ports.DataKey, error) {
	ctx, done := k.t.call(ctx, portKMS, "GenerateDataKey")
	key, err := k.next.GenerateDataKey(ctx, aad)
	done(err)
	return key, err
}

// UnwrapDataKey implements ports.KMS.
func (k *kms) UnwrapDataKey(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	ctx, done := k.t.call(ctx, portKMS, "UnwrapDataKey")
	key, err := k.next.UnwrapDataKey(ctx, wrapped, aad)
	done(err)
	return key, err
}

// InstrumentShardStore returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentShardStore(next ports.ShardStore) ports.ShardStore {
	return &shardStore{t: t, next: next}
}

type shardStore struct {
	t    *Telemetry
	next ports.ShardStore
}

const portShards = "shards"

// DriveID implements ports.ShardStore.
func (s *shardStore) DriveID() string { return s.next.DriveID() }

// Create implements ports.ShardStore. The returned writer measures Commit,
// which is where the data is flushed to stable storage.
func (s *shardStore) Create(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	ctx, done := s.t.call(ctx, portShards, "Create")
	writer, err := s.next.Create(ctx, id)
	done(err)
	if err != nil {
		return nil, err
	}
	return &shardWriter{ShardWriter: writer, t: s.t}, nil
}

// Rewrite implements ports.ShardStore.
func (s *shardStore) Rewrite(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	ctx, done := s.t.call(ctx, portShards, "Rewrite")
	writer, err := s.next.Rewrite(ctx, id)
	done(err)
	if err != nil {
		return nil, err
	}
	return &shardWriter{ShardWriter: writer, t: s.t}, nil
}

// Open implements ports.ShardStore.
func (s *shardStore) Open(ctx context.Context, id object.ShardID) (ports.ShardReader, error) {
	ctx, done := s.t.call(ctx, portShards, "Open")
	reader, err := s.next.Open(ctx, id)
	done(err)
	return reader, err
}

// Delete implements ports.ShardStore.
func (s *shardStore) Delete(ctx context.Context, id object.ShardID) error {
	ctx, done := s.t.call(ctx, portShards, "Delete")
	err := s.next.Delete(ctx, id)
	done(err)
	return err
}

// List implements ports.ShardStore.
func (s *shardStore) List(ctx context.Context, visit func(ports.ShardInfo) error) error {
	ctx, done := s.t.call(ctx, portShards, "List")
	err := s.next.List(ctx, visit)
	done(err)
	return err
}

type shardWriter struct {
	ports.ShardWriter
	t *Telemetry
}

// Commit implements ports.ShardWriter.
func (w *shardWriter) Commit(ctx context.Context) error {
	ctx, done := w.t.call(ctx, portShards, "Commit")
	err := w.ShardWriter.Commit(ctx)
	done(err)
	return err
}

// InstrumentMetadata returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentMetadata(next Metadata) Metadata {
	return &metadata{t: t, next: next}
}

type metadata struct {
	t    *Telemetry
	next Metadata
}

const portMetadata = "metadata"

// CreateBucket implements ports.BucketStore.
func (m *metadata) CreateBucket(ctx context.Context, b bucket.Bucket) error {
	ctx, done := m.t.call(ctx, portMetadata, "CreateBucket")
	err := m.next.CreateBucket(ctx, b)
	done(err)
	return err
}

// DeleteBucket implements ports.BucketStore.
func (m *metadata) DeleteBucket(ctx context.Context, name string) error {
	ctx, done := m.t.call(ctx, portMetadata, "DeleteBucket")
	err := m.next.DeleteBucket(ctx, name)
	done(err)
	return err
}

// GetBucket implements ports.BucketStore.
func (m *metadata) GetBucket(ctx context.Context, name string) (bucket.Bucket, error) {
	ctx, done := m.t.call(ctx, portMetadata, "GetBucket")
	b, err := m.next.GetBucket(ctx, name)
	done(err)
	return b, err
}

// ListBuckets implements ports.BucketStore.
func (m *metadata) ListBuckets(ctx context.Context) ([]bucket.Bucket, error) {
	ctx, done := m.t.call(ctx, portMetadata, "ListBuckets")
	list, err := m.next.ListBuckets(ctx)
	done(err)
	return list, err
}

// PutVersion implements ports.ObjectIndex.
func (m *metadata) PutVersion(ctx context.Context, v object.Version) (ports.PutResult, error) {
	ctx, done := m.t.call(ctx, portMetadata, "PutVersion")
	result, err := m.next.PutVersion(ctx, v)
	done(err)
	return result, err
}

// DeleteObject implements ports.ObjectIndex.
func (m *metadata) DeleteObject(ctx context.Context, bucketName, key string, o ports.DeleteOptions) (ports.DeleteResult, error) {
	ctx, done := m.t.call(ctx, portMetadata, "DeleteObject")
	result, err := m.next.DeleteObject(ctx, bucketName, key, o)
	done(err)
	return result, err
}

// GetVersion implements ports.ObjectIndex.
func (m *metadata) GetVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (object.Version, error) {
	ctx, done := m.t.call(ctx, portMetadata, "GetVersion")
	v, err := m.next.GetVersion(ctx, bucketName, key, ref)
	done(err)
	return v, err
}

// ScanVersions implements ports.ObjectIndex.
func (m *metadata) ScanVersions(
	ctx context.Context, bucketName string, start ports.VersionCursor, visit func(object.Version, bool) ports.ScanControl,
) error {
	ctx, done := m.t.call(ctx, portMetadata, "ScanVersions")
	err := m.next.ScanVersions(ctx, bucketName, start, visit)
	done(err)
	return err
}

// SetBucketVersioning implements ports.BucketStore.
func (m *metadata) SetBucketVersioning(ctx context.Context, name string, state bucket.Versioning) error {
	ctx, done := m.t.call(ctx, portMetadata, "SetBucketVersioning")
	err := m.next.SetBucketVersioning(ctx, name, state)
	done(err)
	return err
}

// GetObject implements ports.ObjectIndex.
func (m *metadata) GetObject(ctx context.Context, bucketName, key string) (object.Version, error) {
	ctx, done := m.t.call(ctx, portMetadata, "GetObject")
	v, err := m.next.GetObject(ctx, bucketName, key)
	done(err)
	return v, err
}

// IsDataReferenced implements ports.ObjectIndex.
func (m *metadata) IsDataReferenced(ctx context.Context, dataID object.ID) (bool, error) {
	ctx, done := m.t.call(ctx, portMetadata, "IsDataReferenced")
	referenced, err := m.next.IsDataReferenced(ctx, dataID)
	done(err)
	return referenced, err
}

// GetTopology implements ports.TopologyStore.
func (m *metadata) GetTopology(ctx context.Context) (cluster.Topology, error) {
	ctx, done := m.t.call(ctx, portMetadata, "GetTopology")
	t, err := m.next.GetTopology(ctx)
	done(err)
	return t, err
}

// SetTopology implements ports.TopologyStore.
func (m *metadata) SetTopology(ctx context.Context, t cluster.Topology) error {
	ctx, done := m.t.call(ctx, portMetadata, "SetTopology")
	err := m.next.SetTopology(ctx, t)
	done(err)
	return err
}

// ScanObjects implements ports.ObjectIndex.
func (m *metadata) ScanObjects(
	ctx context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl,
) error {
	ctx, done := m.t.call(ctx, portMetadata, "ScanObjects")
	err := m.next.ScanObjects(ctx, bucketName, start, visit)
	done(err)
	return err
}

// GetRootCredential implements ports.RootCredentialStore.
func (m *metadata) GetRootCredential(ctx context.Context) (iam.RootCredential, error) {
	ctx, done := m.t.call(ctx, portMetadata, "GetRootCredential")
	c, err := m.next.GetRootCredential(ctx)
	done(err)
	return c, err
}

// SetRootCredential implements ports.RootCredentialStore.
func (m *metadata) SetRootCredential(ctx context.Context, c iam.RootCredential) error {
	ctx, done := m.t.call(ctx, portMetadata, "SetRootCredential")
	err := m.next.SetRootCredential(ctx, c)
	done(err)
	return err
}
