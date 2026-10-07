// Package porttest provides in-memory fakes of the ports for unit tests, and
// contract tests that every implementation of a port, fake or real, must
// pass. Running the same contract against the fakes and the adapters keeps
// the fakes honest.
package porttest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
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

// MemMetadata is an in-memory Metadata. The zero value is not usable; call
// NewMemMetadata.
type MemMetadata struct {
	mu      sync.Mutex
	buckets map[string]bucket.Bucket
	objects map[string]map[string]object.Version
	root    *iam.RootCredential
	topo    *cluster.Topology

	// FailWrites, when set, is returned by every mutating call. Tests use
	// it to simulate a failed Raft commit.
	FailWrites error
}

// NewMemMetadata returns an empty MemMetadata.
func NewMemMetadata() *MemMetadata {
	return &MemMetadata{
		buckets: map[string]bucket.Bucket{},
		objects: map[string]map[string]object.Version{},
	}
}

// CreateBucket implements ports.BucketStore.
func (m *MemMetadata) CreateBucket(_ context.Context, b bucket.Bucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if _, ok := m.buckets[b.Name]; ok {
		return fmt.Errorf("%w: %s", domain.ErrBucketAlreadyExists, b.Name)
	}
	m.buckets[b.Name] = b
	m.objects[b.Name] = map[string]object.Version{}
	return nil
}

// DeleteBucket implements ports.BucketStore.
func (m *MemMetadata) DeleteBucket(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if _, ok := m.buckets[name]; !ok {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	}
	if len(m.objects[name]) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotEmpty, name)
	}
	delete(m.buckets, name)
	delete(m.objects, name)
	return nil
}

// GetBucket implements ports.BucketStore.
func (m *MemMetadata) GetBucket(_ context.Context, name string) (bucket.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[name]
	if !ok {
		return bucket.Bucket{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	}
	return b, nil
}

// ListBuckets implements ports.BucketStore.
func (m *MemMetadata) ListBuckets(context.Context) ([]bucket.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]bucket.Bucket, 0, len(m.buckets))
	for _, name := range slices.Sorted(maps.Keys(m.buckets)) {
		out = append(out, m.buckets[name])
	}
	return out, nil
}

// PutVersion implements ports.ObjectIndex.
func (m *MemMetadata) PutVersion(_ context.Context, v object.Version) (*object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return nil, m.FailWrites
	}
	objects, ok := m.objects[v.Bucket]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, v.Bucket)
	}
	var replaced *object.Version
	if old, ok := objects[v.Key]; ok {
		replaced = &old
	}
	objects[v.Key] = CloneVersion(v)
	return replaced, nil
}

// DeleteObject implements ports.ObjectIndex.
func (m *MemMetadata) DeleteObject(_ context.Context, bucketName, key string) (*object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return nil, m.FailWrites
	}
	objects, ok := m.objects[bucketName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	old, ok := objects[key]
	if !ok {
		return nil, nil
	}
	delete(objects, key)
	return &old, nil
}

// GetObject implements ports.ObjectIndex.
func (m *MemMetadata) GetObject(_ context.Context, bucketName, key string) (object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.objects[bucketName]
	if !ok {
		return object.Version{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	v, ok := objects[key]
	if !ok {
		return object.Version{}, fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, bucketName, key)
	}
	return CloneVersion(v), nil
}

// IsDataReferenced implements ports.ObjectIndex.
func (m *MemMetadata) IsDataReferenced(_ context.Context, dataID object.ID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, objects := range m.objects {
		for _, v := range objects {
			if v.DataID == dataID {
				return true, nil
			}
		}
	}
	return false, nil
}

// GetTopology implements ports.TopologyStore.
func (m *MemMetadata) GetTopology(context.Context) (cluster.Topology, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.topo == nil {
		return cluster.Topology{}, domain.ErrNoTopology
	}
	return m.topo.Clone(), nil
}

// SetTopology implements ports.TopologyStore.
func (m *MemMetadata) SetTopology(_ context.Context, t cluster.Topology) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if err := t.Validate(); err != nil {
		return err
	}
	var stored uint64
	if m.topo != nil {
		stored = m.topo.Version
	}
	if t.Version != stored {
		return fmt.Errorf("%w: topology is at version %d, the change was based on %d", domain.ErrConflict, stored, t.Version)
	}
	next := t.Clone()
	next.Version = stored + 1
	m.topo = &next
	return nil
}

// ScanObjects implements ports.ObjectIndex.
func (m *MemMetadata) ScanObjects(
	_ context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl,
) error {
	m.mu.Lock()
	objects, ok := m.objects[bucketName]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	keys := slices.Sorted(maps.Keys(objects))
	versions := make([]object.Version, len(keys))
	for i, key := range keys {
		versions[i] = CloneVersion(objects[key])
	}
	m.mu.Unlock()

	for i := sort.SearchStrings(keys, start); i < len(keys); {
		control := visit(versions[i])
		switch {
		case control.Stop:
			return nil
		case control.SeekTo != "":
			i = max(i+1, sort.SearchStrings(keys, control.SeekTo))
		default:
			i++
		}
	}
	return nil
}

// GetRootCredential implements ports.RootCredentialStore.
func (m *MemMetadata) GetRootCredential(context.Context) (iam.RootCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.root == nil {
		return iam.RootCredential{}, domain.ErrNotBootstrapped
	}
	return *m.root, nil
}

// SetRootCredential implements ports.RootCredentialStore.
func (m *MemMetadata) SetRootCredential(_ context.Context, c iam.RootCredential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if m.root != nil {
		return domain.ErrAlreadyBootstrapped
	}
	m.root = &c
	return nil
}

// CloneVersion returns a deep copy of v, so that a caller cannot change what
// a store holds.
func CloneVersion(v object.Version) object.Version {
	v.UserMetadata = maps.Clone(v.UserMetadata)
	v.Encryption.WrappedKey = slices.Clone(v.Encryption.WrappedKey)
	v.Encryption.NoncePrefix = slices.Clone(v.Encryption.NoncePrefix)
	v.Layout.Shards = slices.Clone(v.Layout.Shards)
	return v
}
