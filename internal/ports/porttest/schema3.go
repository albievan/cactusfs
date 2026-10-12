package porttest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
)

// What entry schema 3 added to the metadata ports (ADR-0016, ADR-0017,
// ADR-0018), in memory and with the rules of the real store.

var (
	_ ports.ObjectAccessStore  = (*MemMetadata)(nil)
	_ ports.SubBucketStore     = (*MemMetadata)(nil)
	_ ports.TagDefinitionStore = (*MemMetadata)(nil)
)

// accessKey names a version among the records of who read what.
type accessKey struct {
	key string
	ref string
}

// audited runs a change and, when it took effect, hands its audit record to
// the trail. The caller holds the lock.
func (m *MemMetadata) audited(ctx context.Context, record audit.Record, change func() error) error {
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if record.Actor != "" {
		if err := record.Validate(); err != nil {
			return err
		}
	}
	if err := change(); err != nil {
		return err
	}
	if record.Actor != "" && m.Audit != nil {
		return m.Audit.AppendAudit(ctx, record)
	}
	return nil
}

// AddObjectReads implements ports.ObjectAccessStore.
func (m *MemMetadata) AddObjectReads(_ context.Context, batch ports.ReadBatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if err := cluster.ValidateNodeID(batch.Node); err != nil {
		return err
	}
	if len(batch.Items) > ports.MaxReadBatch {
		return fmt.Errorf("%w: a batch of %d reads", domain.ErrInvalidArgument, len(batch.Items))
	}
	if last, seen := m.flushes[batch.Node]; seen && last[0] == batch.Boot && batch.Seq <= last[1] {
		return nil
	}
	at := m.now()
	for _, item := range batch.Items {
		if item.Read.Validate() != nil {
			continue
		}
		for _, v := range m.objects[item.Bucket][item.Key] {
			// The version the name finds now, when it is still the one
			// that was read.
			if !v.Is(item.Version) || v.DeleteMarker || v.VersionID != item.VersionID {
				continue
			}
			if m.access[item.Bucket] == nil {
				m.access[item.Bucket] = map[accessKey]object.Access{}
			}
			place := accessKey{key: item.Key, ref: v.Ref().String()}
			m.access[item.Bucket][place] = m.access[item.Bucket][place].Add([]object.Read{item.Read}, at)
		}
	}
	m.flushes[batch.Node] = [2]uint64{batch.Boot, batch.Seq}
	return nil
}

// ObjectAccess implements ports.ObjectAccessStore.
func (m *MemMetadata) ObjectAccess(_ context.Context, bucketName, key string, ref object.VersionRef) (object.Access, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.objects[bucketName]
	if !ok {
		return object.Access{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	for _, v := range objects[key] {
		if v.Is(ref) {
			access := m.access[bucketName][accessKey{key: key, ref: v.Ref().String()}]
			access.Readers = slices.Clone(access.Readers)
			return access, nil
		}
	}
	return object.Access{}, fmt.Errorf("%w: %s/%s version %s", domain.ErrVersionNotFound, bucketName, key, ref)
}

func cloneSubBucket(s bucket.SubBucket) bucket.SubBucket {
	s.Policy = slices.Clone(s.Policy)
	if len(s.Policy) == 0 {
		s.Policy = nil
	}
	return s
}

// PutSubBucket implements ports.SubBucketStore.
func (m *MemMetadata) PutSubBucket(ctx context.Context, bucketName string, s bucket.SubBucket, record audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		if _, ok := m.buckets[bucketName]; !ok {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		if err := bucket.ValidateSubBucketName(s.Name); err != nil {
			return err
		}
		if len(s.Policy) > iam.MaxPolicyBytes {
			return fmt.Errorf("%w: sub-bucket policy of %d bytes", domain.ErrInvalidArgument, len(s.Policy))
		}
		stored := cloneSubBucket(s)
		stored.CreatedAt = s.CreatedAt.UTC()
		if existing, ok := m.subs[bucketName][s.Name]; ok {
			stored.CreatedAt = existing.CreatedAt
		} else if len(m.subs[bucketName]) >= bucket.MaxSubBuckets {
			return fmt.Errorf("%w: bucket %s has %d", domain.ErrTooManySubBuckets, bucketName, bucket.MaxSubBuckets)
		}
		if m.subs[bucketName] == nil {
			m.subs[bucketName] = map[string]bucket.SubBucket{}
		}
		m.subs[bucketName][s.Name] = stored
		return nil
	})
}

// DeleteSubBucket implements ports.SubBucketStore.
func (m *MemMetadata) DeleteSubBucket(ctx context.Context, bucketName, name string, record audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		if _, ok := m.buckets[bucketName]; !ok {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		sub, ok := m.subs[bucketName][name]
		if !ok {
			return fmt.Errorf("%w: %s/%s", domain.ErrSubBucketNotFound, bucketName, name)
		}
		// Every version of every key is in objects, delete markers too.
		for key := range m.objects[bucketName] {
			if strings.HasPrefix(key, sub.Prefix()) {
				return fmt.Errorf("%w: %s/%s", domain.ErrSubBucketNotEmpty, bucketName, name)
			}
		}
		for _, upload := range m.uploads[bucketName] {
			if strings.HasPrefix(upload.upload.Key, sub.Prefix()) {
				return fmt.Errorf("%w: %s/%s", domain.ErrSubBucketNotEmpty, bucketName, name)
			}
		}
		delete(m.subs[bucketName], name)
		return nil
	})
}

// GetSubBucket implements ports.SubBucketStore.
func (m *MemMetadata) GetSubBucket(_ context.Context, bucketName, name string) (bucket.SubBucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[bucketName]; !ok {
		return bucket.SubBucket{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	sub, ok := m.subs[bucketName][name]
	if !ok {
		return bucket.SubBucket{}, fmt.Errorf("%w: %s/%s", domain.ErrSubBucketNotFound, bucketName, name)
	}
	return cloneSubBucket(sub), nil
}

// ListSubBuckets implements ports.SubBucketStore.
func (m *MemMetadata) ListSubBuckets(_ context.Context, bucketName string) ([]bucket.SubBucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[bucketName]; !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	var out []bucket.SubBucket
	for _, name := range slices.Sorted(maps.Keys(m.subs[bucketName])) {
		out = append(out, cloneSubBucket(m.subs[bucketName][name]))
	}
	return out, nil
}

// PutTagDefinition implements ports.TagDefinitionStore.
func (m *MemMetadata) PutTagDefinition(ctx context.Context, d tagging.Definition, record audit.Record) error {
	if err := d.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		stored := d.Clone()
		stored.CreatedAt = d.CreatedAt.UTC()
		if len(stored.Values) == 0 {
			stored.Values = nil
		}
		if existing, ok := m.tagDefs[d.Key]; ok {
			stored.CreatedAt = existing.CreatedAt
		} else if len(m.tagDefs) >= tagging.MaxDefinitions {
			return fmt.Errorf("%w: the catalogue has %d tag definitions, as many as it can", domain.ErrInvalidArgument, tagging.MaxDefinitions)
		}
		m.tagDefs[d.Key] = stored
		return nil
	})
}

// DeleteTagDefinition implements ports.TagDefinitionStore.
func (m *MemMetadata) DeleteTagDefinition(ctx context.Context, key string, record audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		if _, ok := m.tagDefs[key]; !ok {
			return fmt.Errorf("%w: tag definition %s", domain.ErrEntityNotFound, key)
		}
		for _, name := range slices.Sorted(maps.Keys(m.config)) {
			// As the real store reads them: the keys of the required
			// tags, and nothing from a document that cannot be read.
			var rules struct {
				Required []struct {
					Key string `json:"key"`
				} `json:"required"`
			}
			if json.Unmarshal(m.config[name][bucket.ConfigTagRules], &rules) != nil {
				continue
			}
			for _, required := range rules.Required {
				if required.Key == key {
					return fmt.Errorf("%w: the tag rules of bucket %s require the tag %s", domain.ErrConflict, name, key)
				}
			}
		}
		delete(m.tagDefs, key)
		return nil
	})
}

// GetTagDefinition implements ports.TagDefinitionStore.
func (m *MemMetadata) GetTagDefinition(_ context.Context, key string) (tagging.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.tagDefs[key]
	if !ok {
		return tagging.Definition{}, fmt.Errorf("%w: tag definition %s", domain.ErrEntityNotFound, key)
	}
	return d.Clone(), nil
}

// ListTagDefinitions implements ports.TagDefinitionStore.
func (m *MemMetadata) ListTagDefinitions(context.Context) ([]tagging.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tagging.Definition
	for _, key := range slices.Sorted(maps.Keys(m.tagDefs)) {
		out = append(out, m.tagDefs[key].Clone())
	}
	return out, nil
}
