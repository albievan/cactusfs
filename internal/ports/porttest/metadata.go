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
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
)

// Metadata is the set of metadata ports a store implements together.
type Metadata interface {
	ports.BucketStore
	ports.ObjectIndex
	ports.RootCredentialStore
	ports.TopologyStore
	ports.UploadStore
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
	// uploads holds the multipart uploads in progress of every bucket,
	// and extents the parts of the versions that uploads made, by the
	// data ID of the version.
	uploads map[string]map[object.ID]*memUpload
	extents map[object.ID][]object.Part
	// config holds the configuration documents of buckets, by bucket and
	// kind.
	config map[string]map[string][]byte
	// access holds who read which version, by bucket and by key and
	// version; flushes the last batch of reads of every node; subs the
	// sub-buckets of every bucket; tagDefs the catalogue of tags.
	access  map[string]map[accessKey]object.Access
	flushes map[string][2]uint64
	subs    map[string]map[string]bucket.SubBucket
	tagDefs map[string]tagging.Definition
	// activity holds the latest uploads and downloads of every bucket.
	activity map[string]bucket.Activity

	// Audit, when set, receives the audit records that changes to
	// sub-buckets and to the catalogue of tags carry, as the real store
	// appends them to its one trail. Without it they are checked and
	// dropped.
	Audit ports.AuditLog
	// Now, when set, is the store's time: what Object Lock is judged on.
	// The real store uses the time its leader stamps entries with.
	Now func() time.Time
	// FailWrites, when set, is returned by every mutating call. Tests use
	// it to simulate a failed Raft commit.
	FailWrites error
	// FailParts, when set, is returned by PutPart.
	FailParts error
	// PolicyPage, when set, is how many buckets one page of BucketPolicies
	// reads.
	PolicyPage int
}

// NewMemMetadata returns an empty MemMetadata.
func NewMemMetadata() *MemMetadata {
	return &MemMetadata{
		buckets:  map[string]bucket.Bucket{},
		objects:  map[string]map[string][]object.Version{},
		uploads:  map[string]map[object.ID]*memUpload{},
		extents:  map[object.ID][]object.Part{},
		config:   map[string]map[string][]byte{},
		access:   map[string]map[accessKey]object.Access{},
		flushes:  map[string][2]uint64{},
		subs:     map[string]map[string]bucket.SubBucket{},
		tagDefs:  map[string]tagging.Definition{},
		activity: map[string]bucket.Activity{},
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
	// Who created it is kept, as by a cluster whose members all know entry
	// schema 4 - when it is a name that can be kept.
	if err := bucket.ValidateCreator(b.CreatedBy); err != nil {
		return err
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
	// Uploads that were never completed go with the bucket.
	delete(m.uploads, name)
	delete(m.config, name)
	// So do its sub-buckets and what is known of who read its versions.
	delete(m.subs, name)
	delete(m.access, name)
	// And what is known of its latest uploads and downloads.
	delete(m.activity, name)
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
	if err := object.ValidateKey(v.Key); err != nil {
		return ports.PutResult{}, err
	}
	if v.VersionID.IsZero() || v.DataID.IsZero() {
		return ports.PutResult{}, fmt.Errorf("%w: version or data ID of the put", domain.ErrInvalidArgument)
	}
	if m.inUse(v.DataID) {
		return ports.PutResult{}, fmt.Errorf("%w: data %s belongs to a version already", domain.ErrConflict, v.DataID)
	}
	if v.Parts != 0 || v.Encryption.KeyMode != "" {
		return ports.PutResult{}, fmt.Errorf("%w: a put does not make a version of parts", domain.ErrInvalidArgument)
	}
	if err := object.ValidateTags(v.Tags, object.MaxObjectTags); err != nil {
		return ports.PutResult{}, err
	}
	return m.commit(b, v)
}

// commit puts a new version of a key into the store, as the bucket keeps
// versions. When it refuses, nothing was changed.
func (m *MemMetadata) commit(b bucket.Bucket, v object.Version) (ports.PutResult, error) {
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
	parts := m.forget(removed)
	m.store(v.Bucket, v.Key, append([]object.Version{stored}, versions...))
	// Every version that is committed - by a put, a copy, a completion - is
	// an upload into its bucket, as of the store's time.
	m.activity[v.Bucket] = m.activity[v.Bucket].Uploaded(m.now(), stored.CreatedBy, stored.Key)
	return ports.PutResult{Stored: CloneVersion(stored), Removed: removed, RemovedParts: parts}, nil
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
	versions := m.objects[bucketName][key]
	if !o.Expect.IsZero() {
		var subject *object.Version
		for i := range versions {
			if o.Version == nil || versions[i].Is(*o.Version) {
				subject = &versions[i]
				break
			}
		}
		if subject == nil || subject.VersionID != o.Expect {
			return ports.DeleteResult{}, fmt.Errorf("%w: %s/%s is not the version the delete was decided for", domain.ErrConflict, bucketName, key)
		}
	}
	m.seq++
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
		return ports.DeleteResult{Removed: removed, RemovedParts: m.forget(removed)}, nil
	}
	if b.Versioning == bucket.Unversioned {
		rest, removed := take(versions, object.VersionRef{Null: true})
		m.store(bucketName, key, rest)
		return ports.DeleteResult{Removed: removed, RemovedParts: m.forget(removed)}, nil
	}
	marker := object.Version{
		Bucket: bucketName, Key: key, VersionID: o.MarkerID, ModifiedAt: o.At.UTC(), Seq: m.seq,
		DeleteMarker: true, Versioned: b.Versioning == bucket.VersioningEnabled,
	}
	var removed *object.Version
	var parts []object.Part
	if b.Versioning == bucket.VersioningSuspended {
		versions, removed = take(versions, object.VersionRef{Null: true})
		parts = m.forget(removed)
	} else {
		for _, existing := range versions {
			if existing.Versioned && existing.VersionID == marker.VersionID {
				return ports.DeleteResult{}, fmt.Errorf("%w: version %s exists", domain.ErrConflict, marker.VersionID)
			}
		}
	}
	m.store(bucketName, key, append([]object.Version{marker}, versions...))
	return ports.DeleteResult{Marker: &marker, Removed: removed, RemovedParts: parts}, nil
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
	return m.changed(bucketName, key, ref, true, change)
}

// changed changes something that is kept with a version. needLock refuses a
// bucket without Object Lock.
func (m *MemMetadata) changed(bucketName, key string, ref object.VersionRef, needLock bool, change func(v *object.Version) error) (object.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return object.Version{}, m.FailWrites
	}
	b, ok := m.buckets[bucketName]
	switch {
	case !ok:
		return object.Version{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	case needLock && !b.ObjectLock:
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
	return m.inUse(dataID), nil
}

// inUse reports whether anything owns an ID: a version, a part of an upload
// or of a completed object, or an upload.
func (m *MemMetadata) inUse(id object.ID) bool {
	for _, objects := range m.objects {
		for _, versions := range objects {
			for _, v := range versions {
				if !v.DeleteMarker && v.DataID == id {
					return true
				}
			}
		}
	}
	for _, uploads := range m.uploads {
		for uploadID, upload := range uploads {
			if uploadID == id {
				return true
			}
			for _, part := range upload.parts {
				if part.DataID == id {
					return true
				}
			}
		}
	}
	for _, parts := range m.extents {
		for _, part := range parts {
			if part.DataID == id {
				return true
			}
		}
	}
	return false
}

// forget drops what a version that was removed for good leaves behind, and
// returns the parts it was made of.
func (m *MemMetadata) forget(removed *object.Version) []object.Part {
	if removed == nil {
		return nil
	}
	// The record of who read a version goes with the version.
	delete(m.access[removed.Bucket], accessKey{key: removed.Key, ref: removed.Ref().String()})
	if !removed.Multipart() {
		return nil
	}
	parts := m.extents[removed.DataID]
	delete(m.extents, removed.DataID)
	return parts
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

// Tamper changes the stored record of the current version of a key in place,
// as nothing but a fault or an attacker with the metadata store in hand
// could. Tests use it to see what reading does with a record that lies.
func (m *MemMetadata) Tamper(bucketName, key string, change func(v *object.Version)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if versions := m.objects[bucketName][key]; len(versions) > 0 {
		change(&versions[0])
	}
}

// SetBucketConfig implements ports.BucketStore.
func (m *MemMetadata) SetBucketConfig(_ context.Context, name, kind string, document []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	switch _, ok := m.buckets[name]; {
	case !ok:
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	case !bucket.ValidConfigKind(kind) && kind != bucket.ConfigTagRules:
		return fmt.Errorf("%w: bucket configuration %q", domain.ErrInvalidArgument, kind)
	case len(document) > bucket.MaxConfigBytes:
		return fmt.Errorf("%w: bucket configuration of %d bytes", domain.ErrInvalidArgument, len(document))
	}
	if len(document) == 0 {
		delete(m.config[name], kind)
		return nil
	}
	if m.config[name] == nil {
		m.config[name] = map[string][]byte{}
	}
	m.config[name][kind] = append([]byte(nil), document...)
	return nil
}

// GetBucketConfig implements ports.BucketStore.
func (m *MemMetadata) GetBucketConfig(_ context.Context, name, kind string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[name]; !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	}
	if document := m.config[name][kind]; len(document) > 0 {
		return append([]byte(nil), document...), nil
	}
	return nil, nil
}

// memPolicyPage is how many buckets MemMetadata reads for one page of
// BucketPolicies: few, so that a test with a handful of buckets already goes
// through several pages.
const memPolicyPage = 3

// BucketPolicies implements ports.BucketPolicies, with pages of PolicyPage
// buckets (three when it is not set). A document is lent as the port says,
// and taken back: it is overwritten once visit has returned, so that a test
// finds out whoever kept one without copying it.
func (m *MemMetadata) BucketPolicies(ctx context.Context, from string, visit func(name string, document []byte) error) (string, error) {
	type found struct {
		name     string
		document []byte
	}
	var page []found
	next := ""
	err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		size := m.PolicyPage
		if size <= 0 {
			size = memPolicyPage
		}
		names := slices.Sorted(maps.Keys(m.buckets))
		at, _ := slices.BinarySearch(names, from)
		for i, name := range names[at:] {
			if i == size {
				next = name
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if document := m.config[name][bucket.ConfigPolicy]; len(document) > 0 {
				page = append(page, found{name: name, document: slices.Clone(document)})
			}
		}
		return nil
	}()
	if err != nil {
		return "", err
	}
	// As the real store does it: visit is called when the store's lock has
	// been let go.
	for _, p := range page {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		err := visit(p.name, p.document)
		clear(p.document)
		if err != nil {
			return "", fmt.Errorf("porttest.BucketPolicies: %w", err)
		}
	}
	return next, nil
}

// SetTags implements ports.ObjectIndex.
func (m *MemMetadata) SetTags(_ context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) (object.Version, error) {
	if err := object.ValidateTags(tags, object.MaxObjectTags); err != nil {
		return object.Version{}, err
	}
	return m.changed(bucketName, key, ref, false, func(v *object.Version) error {
		v.Tags = maps.Clone(tags)
		if len(v.Tags) == 0 {
			v.Tags = nil
		}
		return nil
	})
}
