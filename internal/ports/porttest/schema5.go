package porttest

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// What entry schema 5 added to the metadata ports (ADR-0023 to ADR-0026), in
// memory and with the rules of the real store: the usage of buckets and
// their quotas, targets, the outbox with its cursors, replica buckets and
// the record of a resync.
//
// The usage is not kept here change by change, as the real store keeps it:
// it is counted by looking, every time it is asked for. That makes this the
// model a store that adds up changes is held against - what an exact record
// says is, by definition, what a recount finds.

var (
	_ ports.BucketUsageStore = (*MemMetadata)(nil)
	_ ports.TargetStore      = (*MemMetadata)(nil)
	_ ports.Outbox           = (*MemMetadata)(nil)
	_ ports.ReplicationStore = (*MemMetadata)(nil)
)

// memUsage is what is kept of the usage record of a bucket: whether it is
// exact, where a walk through it stands, and the greatest sequence. The
// totals are counted when they are asked for.
type memUsage struct {
	exact bool
	high  uint64
	// count is the phase and the cursor of a walk under way, or nil.
	count *bucket.UsageCount
}

// has reports whether a key has the version ref names.
func has(versions []object.Version, ref object.VersionRef) bool {
	return slices.ContainsFunc(versions, func(v object.Version) bool { return v.Is(ref) })
}

// outranks reports whether a is a newer version of their key than b: the one
// of the greater sequence, and of two at one sequence - which only a source
// at fault sends - the one whose ID is the lesser, the null version before
// every other. The order is total, so that it is the same in whatever order
// the versions arrived.
func outranks(a, b object.Version) bool {
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	var idA, idB object.ID
	if a.Versioned {
		idA = a.VersionID
	}
	if b.Versioned {
		idB = b.VersionID
	}
	return bytes.Compare(idA[:], idB[:]) < 0
}

// placedBySeq returns the versions of a key, newest first, with v among
// them. A version made here is the newest; the copy of a version of another
// deployment has its place by the sequence of its source, wherever that is.
func placedBySeq(versions []object.Version, v object.Version, replica bool) []object.Version {
	at := 0
	if replica {
		for at < len(versions) && outranks(versions[at], v) {
			at++
		}
	}
	return slices.Insert(slices.Clone(versions), at, v)
}

// replicaAllowed reports whether a change that makes a version or a delete
// marker may be made in the bucket: a replica bucket takes those of its
// source and no others, and no other bucket takes any of a source.
func (m *MemMetadata) replicaAllowed(b bucket.Bucket, replica *object.Replica) error {
	switch {
	case replica == nil && b.ReplicaFrom == "":
		return nil
	case replica == nil:
		return fmt.Errorf("%w: bucket %s is a replica and takes versions from its source only", domain.ErrInvalidBucketState, b.Name)
	}
	if err := replica.Validate(); err != nil {
		return err
	}
	if b.ReplicaFrom != replica.From {
		return fmt.Errorf("%w: bucket %s is no replica of that deployment", domain.ErrInvalidBucketState, b.Name)
	}
	return nil
}

// sequence returns the sequence of a version that is made now: that of the
// change, or - in a bucket that holds versions of another deployment at
// sequences of its own - one above the greatest there is.
func (m *MemMetadata) sequence(bucketName string) uint64 {
	if record := m.usage[bucketName]; record != nil {
		// The rule of the domain, which does not wrap at the last number.
		return bucket.Usage{High: record.high}.NextSeq(m.seq)
	}
	return m.seq
}

// raise keeps the greatest sequence of a bucket.
func (m *MemMetadata) raise(bucketName string, seq uint64) {
	if record := m.usage[bucketName]; record != nil {
		record.high = max(record.high, seq)
	}
}

// countedSize is what a size counts for in the usage of a bucket: the size,
// and nothing when it is below nothing. A version may be stored with such a
// size - nothing refuses the put - and counts for no bytes: in what a bucket
// holds, and in what a change adds or frees under a quota.
func countedSize(size int64) int64 {
	return max(size, 0)
}

// totals counts a bucket by looking at it: all of it, or - with a walk under
// way - what the walk has been to.
func (m *MemMetadata) totals(bucketName string, count *bucket.UsageCount) bucket.UsageTotals {
	var totals bucket.UsageTotals
	for key, versions := range m.objects[bucketName] {
		if count != nil && count.Phase == bucket.UsagePhaseKeys && key > count.Cursor {
			continue
		}
		for i, v := range versions {
			totals.Versions++
			totals.StoredBytes += countedSize(v.Size)
			if i == 0 && !v.DeleteMarker {
				totals.Objects++
				totals.CurrentBytes += countedSize(v.Size)
			}
			switch v.Replication {
			case object.ReplicationPending:
				totals.ReplicationPending++
			case object.ReplicationFailed:
				totals.ReplicationFailed++
			}
		}
	}
	for id, upload := range m.uploads[bucketName] {
		if count != nil && (count.Phase == bucket.UsagePhaseKeys || (count.Phase == bucket.UsagePhaseUploads && id.String() > count.Cursor)) {
			continue
		}
		for _, part := range upload.parts {
			totals.UploadBytes += countedSize(part.Size)
		}
	}
	return totals
}

// usageOf returns the usage record of a bucket; the zero record when it has
// none.
func (m *MemMetadata) usageOf(bucketName string) bucket.Usage {
	record := m.usage[bucketName]
	if record == nil {
		return bucket.Usage{}
	}
	usage := bucket.Usage{Exact: record.exact, High: record.high}
	if record.exact {
		usage.UsageTotals = m.totals(bucketName, nil)
	}
	if record.count != nil {
		count := *record.count
		count.UsageTotals = m.totals(bucketName, &count)
		usage.Count = &count
	}
	return usage
}

// admits reports whether a change that adds net bytes to what a bucket holds
// keeps within its quota. A bucket without an exact record has no quota to
// keep to.
func (m *MemMetadata) admits(b bucket.Bucket, net int64) bool {
	record := m.usage[b.Name]
	if record == nil || !record.exact {
		return true
	}
	usage := bucket.Usage{Exact: true, UsageTotals: m.totals(b.Name, nil)}
	return usage.Admits(b.QuotaBytes, bucket.UsageDelta{StoredBytes: net})
}

// ForgetUsage drops the usage record of a bucket, as a bucket has none that
// was made before every member of the cluster knew entry schema 5: the next
// WalkBucketUsage begins its first count. Nothing does this through a port.
func (m *MemMetadata) ForgetUsage(bucketName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.usage, bucketName)
}

// BucketUsages implements ports.BucketUsageStore.
func (m *MemMetadata) BucketUsages(_ context.Context, names []string) ([]ports.BucketUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(names) > ports.MaxBucketUsages {
		return nil, fmt.Errorf("%w: %d buckets, at most %d", domain.ErrInvalidArgument, len(names), ports.MaxBucketUsages)
	}
	out := make([]ports.BucketUsage, len(names))
	for i, name := range names {
		out[i] = ports.BucketUsage{Usage: m.usageOf(name), QuotaBytes: m.buckets[name].QuotaBytes}
	}
	return out, nil
}

// WalkBucketUsage implements ports.BucketUsageStore.
func (m *MemMetadata) WalkBucketUsage(_ context.Context, name string, limit int, restart bool) (ports.UsageWalk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return ports.UsageWalk{}, m.FailWrites
	}
	if limit < 1 || limit > ports.MaxUsageWalk {
		return ports.UsageWalk{}, fmt.Errorf("%w: a walk of %d records, from 1 to %d", domain.ErrInvalidArgument, limit, ports.MaxUsageWalk)
	}
	if _, ok := m.buckets[name]; !ok {
		return ports.UsageWalk{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	}
	record := m.usage[name]
	switch {
	case record == nil:
		record = &memUsage{count: &bucket.UsageCount{Phase: bucket.UsagePhaseKeys}}
		m.usage[name] = record
	case restart:
		record.count = &bucket.UsageCount{Phase: bucket.UsagePhaseKeys}
	case record.count == nil:
		return ports.UsageWalk{Usage: m.usageOf(name)}, nil
	}
	m.seq++
	count := record.count
	if count.Phase == bucket.UsagePhaseKeys {
		// Whole keys, in order, until limit records were visited.
		keys := slices.Sorted(maps.Keys(m.objects[name]))
		at, _ := slices.BinarySearch(keys, count.Cursor+"\x00")
		for ; at < len(keys) && limit > 0; at++ {
			for _, v := range m.objects[name][keys[at]] {
				record.high = max(record.high, v.Seq)
			}
			count.Cursor = keys[at]
			limit -= len(m.objects[name][keys[at]])
		}
		if at == len(keys) {
			count.Phase, count.Cursor = bucket.UsagePhaseUploads, ""
		}
	}
	if count.Phase == bucket.UsagePhaseUploads && limit > 0 {
		// Then the parts, upload by upload.
		ids := make([]string, 0, len(m.uploads[name]))
		parts := map[string]int{}
		for id, upload := range m.uploads[name] {
			ids = append(ids, id.String())
			parts[id.String()] = len(upload.parts)
		}
		slices.Sort(ids)
		at, _ := slices.BinarySearch(ids, count.Cursor+"\x00")
		for ; at < len(ids) && limit > 0; at++ {
			count.Cursor = ids[at]
			limit -= max(parts[ids[at]], 1)
		}
		if at == len(ids) {
			// The walk has been through everything: what it found is
			// what the bucket holds. Counted by looking, it cannot differ
			// from what was kept.
			record.exact, record.count = true, nil
		}
	}
	return ports.UsageWalk{Usage: m.usageOf(name)}, nil
}

// SetBucketQuota implements ports.BucketUsageStore.
func (m *MemMetadata) SetBucketQuota(ctx context.Context, name string, quotaBytes int64, record audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		b, ok := m.buckets[name]
		if !ok {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
		}
		if quotaBytes != 0 {
			if err := bucket.ValidateQuota(quotaBytes); err != nil {
				return err
			}
		}
		if usage := m.usage[name]; usage == nil || !usage.exact {
			return fmt.Errorf("%w: bucket %s is still being counted", domain.ErrConflict, name)
		}
		m.seq++
		b.QuotaBytes = quotaBytes
		m.buckets[name] = b
		return nil
	})
}

// PutTarget implements ports.TargetStore.
func (m *MemMetadata) PutTarget(ctx context.Context, t delivery.Target, record audit.Record) error {
	if err := t.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		stored := t.Clone()
		stored.Refs, stored.CreatedAt, stored.UpdatedAt = 0, t.CreatedAt.UTC(), t.UpdatedAt.UTC()
		existing, replaces := m.targets[t.Name]
		if !replaces && len(m.targets) >= delivery.MaxTargets {
			return fmt.Errorf("%w: there are %d targets, and a deployment has no more", domain.ErrInvalidArgument, delivery.MaxTargets)
		}
		if replaces {
			if existing.Refs > 0 && existing.Kind != t.Kind {
				return fmt.Errorf("%w: target %s is named by bucket configurations as a %s target", domain.ErrEntityInUse, t.Name, existing.Kind)
			}
			// A target goes on admitting every bucket whose document
			// names it: found by looking at the documents there are.
			for _, name := range m.namers(t.Name) {
				if !t.Admits(name) {
					return fmt.Errorf("%w: target %s is named by the configuration of bucket %s, which it would no longer admit", domain.ErrEntityInUse, t.Name, name)
				}
			}
			stored.Refs, stored.CreatedAt = existing.Refs, existing.CreatedAt
		}
		m.seq++
		m.targets[t.Name] = stored
		if !replaces {
			// The cursor is made with the target and begins at the change
			// that made it.
			here := delivery.Seq{Index: m.seq}
			m.cursors[t.Name] = delivery.Cursor{Target: t.Name, From: here, Acked: here}
		}
		return nil
	})
}

// DeleteTarget implements ports.TargetStore.
func (m *MemMetadata) DeleteTarget(ctx context.Context, name string, record audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		target, ok := m.targets[name]
		switch {
		case !ok:
			return fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, name)
		case target.Refs > 0:
			return fmt.Errorf("%w: target %s is named by %d bucket configurations", domain.ErrEntityInUse, name, target.Refs)
		}
		m.seq++
		delete(m.targets, name)
		delete(m.cursors, name)
		// The rows that only this target had not passed are behind every
		// target now: they go, so many for one change.
		m.trim()
		return nil
	})
}

// GetTarget implements ports.TargetStore.
func (m *MemMetadata) GetTarget(_ context.Context, name string) (delivery.Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.targets[name]
	if !ok {
		return delivery.Target{}, fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, name)
	}
	return target.Clone(), nil
}

// ListTargets implements ports.TargetStore.
func (m *MemMetadata) ListTargets(context.Context) ([]delivery.Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]delivery.Target, 0, len(m.targets))
	for _, name := range slices.Sorted(maps.Keys(m.targets)) {
		out = append(out, m.targets[name].Clone())
	}
	return out, nil
}

// namedBy returns the targets a stored document of the kind names; none for
// no document, and for one that cannot be read.
func (m *MemMetadata) namedBy(kind string, document []byte) []string {
	if kind == bucket.ConfigReplication {
		config, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			return nil
		}
		return config.Targets()
	}
	config, err := bucket.ParseNotificationConfig(document)
	if err != nil {
		return nil
	}
	return config.Targets()
}

// namers returns the buckets that have a document which names the target,
// in the order of their names.
func (m *MemMetadata) namers(target string) []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(m.config)) {
		for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
			if slices.Contains(m.namedBy(kind, m.config[name][kind]), target) {
				names = append(names, name)
				break
			}
		}
	}
	return names
}

// sentTo reports whether the versions of a bucket are sent on to a target:
// whether the bucket is there and its replication configuration names the
// target.
func (m *MemMetadata) sentTo(bucketName, target string) bool {
	if _, ok := m.buckets[bucketName]; !ok {
		return false
	}
	return slices.Contains(m.namedBy(bucket.ConfigReplication, m.config[bucketName][bucket.ConfigReplication]), target)
}

// refer adds by to the count of the documents that name each target.
func (m *MemMetadata) refer(names []string, by int) {
	for _, name := range names {
		if target, ok := m.targets[name]; ok {
			target.Refs = max(target.Refs+by, 0)
			m.targets[name] = target
		}
	}
}

// checkDelivered reports whether a notification or replication document can
// become the bucket's: it is in its canonical form, every target it names
// is there, of a kind that fits and admits the bucket, and - for
// replication - the bucket keeps versions and is no replica. No document
// removes the configuration and is always allowed.
func (m *MemMetadata) checkDelivered(b bucket.Bucket, kind string, document []byte) error {
	if len(document) == 0 {
		return nil
	}
	if len(b.Name) > bucket.MaxNameLength {
		return fmt.Errorf("%w: a bucket of a name of more than %d bytes has no %s document", domain.ErrInvalidArgument, bucket.MaxNameLength, kind)
	}
	consumer := delivery.ConsumerNotification
	var names []string
	if kind == bucket.ConfigReplication {
		config, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			return err
		}
		switch {
		case b.ReplicaFrom != "":
			return fmt.Errorf("%w: bucket %s is a replica and is not replicated again", domain.ErrInvalidBucketState, b.Name)
		case b.Versioning != bucket.VersioningEnabled:
			return fmt.Errorf("%w: bucket %s does not keep versions", domain.ErrInvalidBucketState, b.Name)
		}
		consumer, names = delivery.ConsumerReplication, config.Targets()
	} else {
		config, err := bucket.ParseNotificationConfig(document)
		if err != nil {
			return err
		}
		names = config.Targets()
	}
	for _, name := range names {
		target, ok := m.targets[name]
		switch {
		case !ok:
			return fmt.Errorf("%w: there is no target %s", domain.ErrInvalidArgument, name)
		case !target.Kind.Serves(consumer):
			return fmt.Errorf("%w: target %s is a %s target", domain.ErrInvalidArgument, name, target.Kind)
		case !target.Admits(b.Name):
			return fmt.Errorf("%w: target %s is not open to bucket %s", domain.ErrInvalidArgument, name, b.Name)
		}
	}
	return nil
}

// consumers returns who a change of a version is for, by the two documents
// of its bucket. by is who made the change, replica says that the version
// arrived as one, and status is the replication status it had before.
func (m *MemMetadata) consumers(event delivery.Event, v object.Version, by string, replica bool, status object.ReplicationStatus) []delivery.Consumer {
	var to []delivery.Consumer
	if config, err := bucket.ParseNotificationConfig(m.config[v.Bucket][bucket.ConfigNotification]); err == nil {
		to = append(to, config.Match(event, v.Key)...)
	}
	if config, err := bucket.ParseReplicationConfig(m.config[v.Bucket][bucket.ConfigReplication]); err == nil {
		to = append(to, config.Replicates(event, v.Key, by, replica, status)...)
	}
	return to
}

// replicated reports whether one of the consumers sends the version on.
func replicated(to []delivery.Consumer) bool {
	return slices.ContainsFunc(to, func(c delivery.Consumer) bool { return c.Kind == delivery.ConsumerReplication })
}

// tell appends the row of a change to the outbox, for the consumers given;
// with none, nothing. At the bound the oldest row goes, and is counted as
// lost for who had not passed it.
func (m *MemMetadata) tell(event delivery.Event, v object.Version, by string, to []delivery.Consumer) {
	if len(to) == 0 {
		return
	}
	// Rows of one change are numbered within it.
	if m.row.Index != m.seq {
		m.row = delivery.Seq{Index: m.seq}
	}
	item := delivery.Item{
		Seq: m.row, Event: event, Bucket: v.Bucket, Key: v.Key, Version: v.Ref(),
		At: m.now(), By: by, To: slices.Clone(to),
	}
	if !v.DeleteMarker {
		// A size below nothing is told as nothing, as it is counted; and an
		// entity tag longer than a row keeps is left out of it.
		item.Size = countedSize(v.Size)
		if len(v.ETag) <= delivery.MaxItemETag {
			item.ETag = v.ETag
		}
	}
	m.row.Row++
	m.outbox = append(m.outbox, item)
	bound := m.MaxOutboxRows
	if bound <= 0 {
		bound = delivery.MaxOutboxRows
	}
	for len(m.outbox) > bound {
		oldest := m.outbox[0]
		m.outbox = m.outbox[1:]
		m.dropped++
		for name, cursor := range m.cursors {
			m.cursors[name] = cursor.Dropped(oldest)
		}
	}
}

// gone tells of a version that was removed for good, when one was.
func (m *MemMetadata) gone(removed *object.Version, by string) {
	if removed == nil {
		return
	}
	m.tell(delivery.EventDelete, *removed, by, m.consumers(delivery.EventDelete, *removed, by, false, removed.Replication))
}

// OutboxRows returns every row the outbox holds, oldest first, whoever it is
// for: what a store that keeps the outbox row for row is held against.
// Nothing reads the outbox this way through a port.
func (m *MemMetadata) OutboxRows() []delivery.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]delivery.Item, len(m.outbox))
	for i, item := range m.outbox {
		rows[i] = item.Clone()
	}
	return rows
}

// ReadOutbox implements ports.Outbox.
func (m *MemMetadata) ReadOutbox(_ context.Context, target string, after delivery.Seq, limit int) ([]delivery.Item, delivery.Seq, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cursor, ok := m.cursors[target]
	switch {
	case !ok:
		return nil, delivery.Seq{}, fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, target)
	case limit < 1 || limit > delivery.MaxOutboxRead:
		return nil, delivery.Seq{}, fmt.Errorf("%w: a read of %d rows, from 1 to %d", domain.ErrInvalidArgument, limit, delivery.MaxOutboxRead)
	}
	var items []delivery.Item
	upto := after
	for _, item := range m.outbox {
		// What was written before the target was defined is not the
		// target's, whatever name it was written for.
		if !item.Seq.After(after) || !item.Seq.After(cursor.From) {
			continue
		}
		if len(items) == limit {
			break
		}
		upto = item.Seq
		if _, mine := item.For(target); mine {
			items = append(items, item.Clone())
		}
	}
	return items, upto, nil
}

// find returns the version a result names, and whether the result was the
// target's to give: by a row, when the row is gone or was for the target as
// replication; by itself, when the bucket is there and its replication
// configuration names the target. A result that is not the target's names
// nothing.
func (m *MemMetadata) find(target string, r delivery.Result) (found *object.Version, mine bool) {
	bucketName, key, ref := r.Bucket, r.Key, r.Version
	if r.ByRow() {
		at := slices.IndexFunc(m.outbox, func(item delivery.Item) bool { return item.Seq == r.Seq })
		if at < 0 {
			return nil, true
		}
		if kind, consumer := m.outbox[at].For(target); !consumer || kind != delivery.ConsumerReplication {
			return nil, false
		}
		bucketName, key, ref = m.outbox[at].Bucket, m.outbox[at].Key, m.outbox[at].Version
	} else if !m.sentTo(bucketName, target) {
		return nil, false
	}
	versions := m.objects[bucketName][key]
	for i := range versions {
		if versions[i].Is(ref) {
			return &versions[i], true
		}
	}
	return nil, true
}

// AckOutbox implements ports.Outbox.
func (m *MemMetadata) AckOutbox(_ context.Context, target string, upto delivery.Seq, auditUpto uint64, results []delivery.Result) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return 0, m.FailWrites
	}
	cursor, ok := m.cursors[target]
	switch {
	case !ok:
		return 0, fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, target)
	case len(results) > delivery.MaxAckResults:
		return 0, fmt.Errorf("%w: %d results, at most %d", domain.ErrInvalidArgument, len(results), delivery.MaxAckResults)
	case len(results) > 0 && m.targets[target].Kind != delivery.KindS3:
		return 0, fmt.Errorf("%w: results are of replication, and target %s is not another deployment", domain.ErrInvalidArgument, target)
	}
	for _, result := range results {
		if err := result.Validate(); err != nil {
			return 0, err
		}
	}
	m.seq++
	// No acknowledgement passes what was not written yet.
	last := cursor.Acked
	if len(m.outbox) > 0 {
		last = m.outbox[len(m.outbox)-1].Seq
	}
	if upto.After(last) {
		upto = last
	}
	if !m.targets[target].Audit {
		auditUpto = 0
	}
	m.cursors[target] = cursor.Advanced(upto, auditUpto)
	unrelated := 0
	for _, result := range results {
		// Only a version that still exists, and never one that arrived as
		// a replica; and what was not the target's to say changes nothing,
		// and is counted.
		v, mine := m.find(target, result)
		if !mine {
			unrelated++
		}
		if v != nil && v.Replication != object.ReplicationReplica {
			v.Replication = result.Status
		}
	}
	// Rows that every target has passed go, so many for one
	// acknowledgement.
	m.trim()
	return unrelated, nil
}

// trim removes the rows every target has passed, oldest first, at most
// delivery.MaxOutboxTrim of them. With no target left, every row is one that
// every target has passed.
func (m *MemMetadata) trim() {
	lowest, none := delivery.Seq{}, true
	for _, c := range m.cursors {
		if none || lowest.After(c.Acked) {
			lowest, none = c.Acked, false
		}
	}
	trimmed := 0
	for trimmed < len(m.outbox) && trimmed < delivery.MaxOutboxTrim && (none || !m.outbox[trimmed].Seq.After(lowest)) {
		trimmed++
	}
	m.outbox = m.outbox[trimmed:]
}

// OutboxCursors implements ports.Outbox.
func (m *MemMetadata) OutboxCursors(context.Context) (ports.OutboxState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := ports.OutboxState{Dropped: m.dropped, Cursors: make([]delivery.Cursor, 0, len(m.cursors))}
	for range m.outbox {
		state.Rows++
	}
	for _, name := range slices.Sorted(maps.Keys(m.cursors)) {
		state.Cursors = append(state.Cursors, m.cursors[name])
	}
	return state, nil
}

// SetBucketReplica implements ports.ReplicationStore.
func (m *MemMetadata) SetBucketReplica(ctx context.Context, name, source string, record audit.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audited(ctx, record, func() error {
		b, ok := m.buckets[name]
		if !ok {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
		}
		if source != "" {
			if err := object.ValidateReplicaSource(source); err != nil {
				return err
			}
		}
		if source == b.ReplicaFrom {
			return nil
		}
		if source != "" {
			usage := m.usageOf(name)
			switch {
			case b.Versioning != bucket.VersioningEnabled:
				return fmt.Errorf("%w: bucket %s does not keep versions", domain.ErrInvalidBucketState, name)
			case len(m.config[name][bucket.ConfigReplication]) > 0:
				return fmt.Errorf("%w: bucket %s is replicated itself", domain.ErrInvalidBucketState, name)
			case !usage.Exact:
				return fmt.Errorf("%w: bucket %s is still being counted", domain.ErrConflict, name)
			case usage.Versions != 0:
				// The sequences of two deployments are unrelated: a version
				// that is here would stay current over what the source sends.
				return fmt.Errorf("%w: bucket %s holds versions of its own", domain.ErrBucketNotEmpty, name)
			}
		}
		m.seq++
		b.ReplicaFrom = source
		m.buckets[name] = b
		return nil
	})
}

// PutResync implements ports.ReplicationStore.
func (m *MemMetadata) PutResync(_ context.Context, r bucket.Resync) error {
	if err := r.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if _, ok := m.buckets[r.Bucket]; !ok {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, r.Bucket)
	}
	cursor, ok := m.cursors[r.Target]
	if !ok {
		return fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, r.Target)
	}
	m.seq++
	m.resyncs[r.Bucket] = r
	// The mark is cleared by a bucket that is sent to the target, and by no
	// other.
	if r.State == bucket.ResyncDone && m.sentTo(r.Bucket, r.Target) {
		cursor.ResyncNeeded = false
		m.cursors[r.Target] = cursor
	}
	return nil
}

// GetResync implements ports.ReplicationStore.
func (m *MemMetadata) GetResync(_ context.Context, bucketName string) (bucket.Resync, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[bucketName]; !ok {
		return bucket.Resync{}, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	return m.resyncs[bucketName], nil
}
