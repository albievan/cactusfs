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
	"time"

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
	// objects holds every version of every key, newest first.
	objects map[string]map[string][]object.Version
	// seq numbers the changes, as the Raft index does in the real store.
	seq  uint64
	root *iam.RootCredential
	topo *cluster.Topology

	// Now, when set, is the store's time: what Object Lock is judged on.
	// The real store uses the time its leader stamps entries with.
	Now func() time.Time
	// FailWrites, when set, is returned by every mutating call. Tests use
	// it to simulate a failed Raft commit.
	FailWrites error
}

// NewMemMetadata returns an empty MemMetadata.
func NewMemMetadata() *MemMetadata {
	return &MemMetadata{
		buckets: map[string]bucket.Bucket{},
		objects: map[string]map[string][]object.Version{},
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
	// As the real store does it: a bucket begins without versions, or -
	// created with Object Lock - keeping them.
	b.Versioning, b.DefaultRetention = bucket.Unversioned, nil
	if b.ObjectLock {
		b.Versioning = bucket.VersioningEnabled
	}
	m.buckets[b.Name] = b
	m.objects[b.Name] = map[string][]object.Version{}
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

// take removes the version ref names from a key's versions and returns it.
func take(versions []object.Version, ref object.VersionRef) ([]object.Version, *object.Version) {
	for i, v := range versions {
		if v.Is(ref) {
			removed := CloneVersion(v)
			return slices.Delete(slices.Clone(versions), i, i+1), &removed
		}
	}
	return versions, nil
}

func (m *MemMetadata) store(bucketName, key string, versions []object.Version) {
	if len(versions) == 0 {
		delete(m.objects[bucketName], key)
		return
	}
	m.objects[bucketName][key] = versions
}

// PutVersion implements ports.ObjectIndex.
func (m *MemMetadata) PutVersion(_ context.Context, v object.Version) (ports.PutResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return ports.PutResult{}, m.FailWrites
	}
	b, ok := m.buckets[v.Bucket]
	if !ok {
		return ports.PutResult{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, v.Bucket)
	}
	lock := v.Lock
	switch {
	case !b.ObjectLock && !lock.IsZero():
		return ports.PutResult{}, fmt.Errorf("%w: bucket %s", domain.ErrLockNotConfigured, v.Bucket)
	case b.ObjectLock && lock.Retention != nil:
		if err := lock.Retention.Validate(m.now()); err != nil {
			return ports.PutResult{}, err
		}
	case b.ObjectLock && b.DefaultRetention != nil:
		lock.Retention = &object.Retention{Mode: b.DefaultRetention.Mode, Until: b.DefaultRetention.Until(m.now())}
	}
	m.seq++
	stored := CloneVersion(v)
	stored.DeleteMarker, stored.Lock = false, lock
	versions := m.objects[v.Bucket][v.Key]
	var removed *object.Version
	switch b.Versioning {
	case bucket.VersioningEnabled:
		stored.Versioned, stored.Seq = true, m.seq
		for _, existing := range versions {
			if existing.Versioned && existing.VersionID == stored.VersionID {
				return ports.PutResult{}, fmt.Errorf("%w: version %s exists", domain.ErrConflict, stored.VersionID)
			}
		}
	case bucket.VersioningSuspended:
		stored.Versioned, stored.Seq = false, m.seq
		versions, removed = take(versions, object.VersionRef{Null: true})
	default:
		stored.Versioned, stored.Seq = false, 0
		versions, removed = take(versions, object.VersionRef{Null: true})
	}
	m.store(v.Bucket, v.Key, append([]object.Version{stored}, versions...))
	return ports.PutResult{Stored: CloneVersion(stored), Removed: removed}, nil
}

// DeleteObject implements ports.ObjectIndex.
func (m *MemMetadata) DeleteObject(_ context.Context, bucketName, key string, o ports.DeleteOptions) (ports.DeleteResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return ports.DeleteResult{}, m.FailWrites
	}
	b, ok := m.buckets[bucketName]
	if !ok {
		return ports.DeleteResult{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	m.seq++
	versions := m.objects[bucketName][key]
	if o.Version != nil {
		for _, v := range versions {
			if v.Is(*o.Version) {
				if err := v.Lock.CheckDelete(m.now(), o.BypassGovernance); err != nil {
					return ports.DeleteResult{}, err
				}
			}
		}
		rest, removed := take(versions, *o.Version)
		m.store(bucketName, key, rest)
		return ports.DeleteResult{Removed: removed}, nil
	}
	if b.Versioning == bucket.Unversioned {
		rest, removed := take(versions, object.VersionRef{Null: true})
		m.store(bucketName, key, rest)
		return ports.DeleteResult{Removed: removed}, nil
	}
	marker := object.Version{
		Bucket: bucketName, Key: key, VersionID: o.MarkerID, ModifiedAt: o.At.UTC(), Seq: m.seq,
		DeleteMarker: true, Versioned: b.Versioning == bucket.VersioningEnabled,
	}
	var removed *object.Version
	if b.Versioning == bucket.VersioningSuspended {
		versions, removed = take(versions, object.VersionRef{Null: true})
	} else {
		for _, existing := range versions {
			if existing.Versioned && existing.VersionID == marker.VersionID {
				return ports.DeleteResult{}, fmt.Errorf("%w: version %s exists", domain.ErrConflict, marker.VersionID)
			}
		}
	}
	m.store(bucketName, key, append([]object.Version{marker}, versions...))
	return ports.DeleteResult{Marker: &marker, Removed: removed}, nil
}

// SetBucketVersioning implements ports.BucketStore.
func (m *MemMetadata) SetBucketVersioning(_ context.Context, name string, state bucket.Versioning) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	b, ok := m.buckets[name]
	if !ok {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	}
	if err := bucket.ValidateVersioningChange(state, b.ObjectLock); err != nil {
		return err
	}
	b.Versioning = state
	m.buckets[name] = b
	return nil
}

func (m *MemMetadata) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// SetBucketDefaultRetention implements ports.BucketStore.
func (m *MemMetadata) SetBucketDefaultRetention(_ context.Context, name string, d *bucket.DefaultRetention) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	b, ok := m.buckets[name]
	switch {
	case !ok:
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	case !b.ObjectLock:
		return fmt.Errorf("%w: bucket %s", domain.ErrLockNotConfigured, name)
	}
	if d != nil {
		if err := d.Validate(); err != nil {
			return err
		}
		rule := *d
		d = &rule
	}
	b.DefaultRetention = d
	m.buckets[name] = b
	return nil
}

// locked finds the version a lock change is about and applies change to it.
func (m *MemMetadata) locked(bucketName, key string, ref object.VersionRef, change func(v *object.Version) error) (object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return object.Version{}, m.FailWrites
	}
	b, ok := m.buckets[bucketName]
	switch {
	case !ok:
		return object.Version{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	case !b.ObjectLock:
		return object.Version{}, fmt.Errorf("%w: bucket %s", domain.ErrLockNotConfigured, bucketName)
	}
	versions := m.objects[bucketName][key]
	for i := range versions {
		if !versions[i].Is(ref) {
			continue
		}
		if versions[i].DeleteMarker {
			return object.Version{}, fmt.Errorf("%w: a delete marker cannot be locked", domain.ErrMethodNotAllowed)
		}
		if err := change(&versions[i]); err != nil {
			return object.Version{}, err
		}
		m.seq++
		return CloneVersion(versions[i]), nil
	}
	return object.Version{}, fmt.Errorf("%w: %s/%s version %s", domain.ErrVersionNotFound, bucketName, key, ref)
}

// SetRetention implements ports.ObjectIndex.
func (m *MemMetadata) SetRetention(
	_ context.Context, bucketName, key string, ref object.VersionRef, r *object.Retention, bypassGovernance bool,
) (object.Version, error) {
	return m.locked(bucketName, key, ref, func(v *object.Version) error {
		if err := v.Lock.CheckRetention(r, m.now(), bypassGovernance); err != nil {
			return err
		}
		v.Lock.Retention = nil
		if r != nil {
			next := *r
			v.Lock.Retention = &next
		}
		return nil
	})
}

// SetLegalHold implements ports.ObjectIndex.
func (m *MemMetadata) SetLegalHold(_ context.Context, bucketName, key string, ref object.VersionRef, on bool) (object.Version, error) {
	return m.locked(bucketName, key, ref, func(v *object.Version) error {
		v.Lock.LegalHold = on
		return nil
	})
}

// GetObject implements ports.ObjectIndex.
func (m *MemMetadata) GetObject(_ context.Context, bucketName, key string) (object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.objects[bucketName]
	if !ok {
		return object.Version{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	versions := objects[key]
	if len(versions) == 0 || versions[0].DeleteMarker {
		return object.Version{}, fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, bucketName, key)
	}
	return CloneVersion(versions[0]), nil
}

// GetVersion implements ports.ObjectIndex.
func (m *MemMetadata) GetVersion(_ context.Context, bucketName, key string, ref object.VersionRef) (object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.objects[bucketName]
	if !ok {
		return object.Version{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	for _, v := range objects[key] {
		if v.Is(ref) {
			return CloneVersion(v), nil
		}
	}
	return object.Version{}, fmt.Errorf("%w: %s/%s version %s", domain.ErrVersionNotFound, bucketName, key, ref)
}

// IsDataReferenced implements ports.ObjectIndex.
func (m *MemMetadata) IsDataReferenced(_ context.Context, dataID object.ID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, objects := range m.objects {
		for _, versions := range objects {
			for _, v := range versions {
				if !v.DeleteMarker && v.DataID == dataID {
					return true, nil
				}
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

// snapshot returns the keys of a bucket in order and a copy of their
// versions.
func (m *MemMetadata) snapshot(bucketName string) ([]string, map[string][]object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.objects[bucketName]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	keys := slices.Sorted(maps.Keys(objects))
	copied := make(map[string][]object.Version, len(objects))
	for key, versions := range objects {
		for _, v := range versions {
			copied[key] = append(copied[key], CloneVersion(v))
		}
	}
	return keys, copied, nil
}

// ScanObjects implements ports.ObjectIndex.
func (m *MemMetadata) ScanObjects(
	_ context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl,
) error {
	keys, versions, err := m.snapshot(bucketName)
	if err != nil {
		return err
	}
	for i := sort.SearchStrings(keys, start); i < len(keys); {
		current := versions[keys[i]][0]
		if current.DeleteMarker {
			i++
			continue
		}
		control := visit(current)
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

// ScanVersions implements ports.ObjectIndex.
func (m *MemMetadata) ScanVersions(
	_ context.Context, bucketName string, start ports.VersionCursor, visit func(object.Version, bool) ports.ScanControl,
) error {
	keys, versions, err := m.snapshot(bucketName)
	if err != nil {
		return err
	}
	for i := sort.SearchStrings(keys, start.Key); i < len(keys); {
		next := i + 1
		list, first := versions[keys[i]], 0
		if start.After != nil && keys[i] == start.Key {
			first = len(list)
			for n, v := range list {
				if v.Is(*start.After) {
					first = n + 1
				}
			}
		}
		for n := first; n < len(list); n++ {
			control := visit(list[n], n == 0)
			if control.Stop {
				return nil
			}
			if control.SeekTo > keys[i] {
				next = max(next, sort.SearchStrings(keys, control.SeekTo))
				break
			}
		}
		i = next
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
	if v.Lock.Retention != nil {
		retention := *v.Lock.Retention
		v.Lock.Retention = &retention
	}
	return v
}
