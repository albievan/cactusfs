package app_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// creatingDrive is a drive that counts the shards begun on it.
type creatingDrive struct {
	ports.ShardStore
	creates *atomic.Int32
}

func (d creatingDrive) Create(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	d.creates.Add(1)
	return d.ShardStore.Create(ctx, id)
}

// quotaFixture is a bucket "b" behind an object service with the pre-check
// of the quota in its write path; the shards begun on its drives and the
// refusals of the quota are counted.
type quotaFixture struct {
	*fixture
	creates  atomic.Int32
	refusals atomic.Int32
	// upload is the upload a setup began, for the parts a write adds.
	upload object.Upload
}

// newQuotaFixture returns the fixture whose pre-check reads the usage store
// advice makes of the metadata fake; nil reads the fake itself.
func newQuotaFixture(t *testing.T, advice func(*porttest.MemMetadata) ports.BucketUsageStore) *quotaFixture {
	t.Helper()
	f := &quotaFixture{fixture: newFixture(t)}
	f.createBucket(t, "b")
	var usage ports.BucketUsageStore = f.meta
	if advice != nil {
		usage = advice(f.meta)
	}
	quotas := app.NewQuotaService(app.QuotaServiceConfig{
		Buckets: f.meta, Usage: usage, Authz: f.authz, OnRefusal: func() { f.refusals.Add(1) },
	})
	drives := make([]ports.ShardStore, len(f.drives))
	for i, d := range f.drives {
		drives[i] = creatingDrive{ShardStore: d, creates: &f.creates}
	}
	f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) {
		cfg.Drives = fixed(t, f.topology, drives)
		cfg.Quota = quotas
	})
	return f
}

func (f *quotaFixture) setQuota(t *testing.T, quotaBytes int64) {
	t.Helper()
	if err := f.meta.SetBucketQuota(context.Background(), "b", quotaBytes, audit.Record{}); err != nil {
		t.Fatalf("SetBucketQuota(%d): %v", quotaBytes, err)
	}
}

// used returns what counts against the quota of "b".
func (f *quotaFixture) used(t *testing.T) int64 {
	t.Helper()
	records, err := f.meta.BucketUsages(context.Background(), []string{"b"})
	if err != nil {
		t.Fatalf("BucketUsages: %v", err)
	}
	return records[0].Usage.Used()
}

// noQuotaAdvice tells the pre-check that no bucket has a quota: the advice
// of a node whose copy of the state is behind, or of a writer that lost the
// race to another (ADR-0024).
type noQuotaAdvice struct{ ports.BucketUsageStore }

func (a noQuotaAdvice) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	out, err := a.BucketUsageStore.BucketUsages(ctx, names)
	for i := range out {
		out[i].QuotaBytes = 0
	}
	return out, err
}

// fullAdvice tells the pre-check, once full is set, that every bucket is
// full: whatever asks it then is refused.
type fullAdvice struct {
	ports.BucketUsageStore
	full *atomic.Bool
}

func (a fullAdvice) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	if !a.full.Load() {
		return a.BucketUsageStore.BucketUsages(ctx, names)
	}
	out := make([]ports.BucketUsage, len(names))
	for i := range out {
		out[i] = ports.BucketUsage{QuotaBytes: 1, Usage: bucket.Usage{Exact: true, UsageTotals: bucket.UsageTotals{StoredBytes: math.MaxInt32}}}
	}
	return out, nil
}

// The writes of the tests of quotas, into "b".
func quotaPut(key string, declared int64, n int) func(context.Context, *quotaFixture) error {
	return func(ctx context.Context, f *quotaFixture) error {
		_, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: key, Body: bytes.NewReader(make([]byte, n)), Size: declared})
		return err
	}
}

func quotaPart(number, n int) func(context.Context, *quotaFixture) error {
	return func(ctx context.Context, f *quotaFixture) error {
		data := make([]byte, n)
		_, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
			Bucket: "b", Key: f.upload.Key, UploadID: f.upload.ID, Number: number, Body: bytes.NewReader(data), Size: int64(n),
			Verify: func() (string, error) { return partTag(data), nil },
		})
		return err
	}
}

func quotaCopy(source, destination string) func(context.Context, *quotaFixture) error {
	return func(ctx context.Context, f *quotaFixture) error {
		_, err := f.objects.Copy(ctx, porttest.Root(), app.CopyInput{
			Source: app.GetInput{Bucket: "b", Key: source}, Destination: app.PutInput{Bucket: "b", Key: destination}, Digest: tagged,
		})
		return err
	}
}

func quotaCopyPart(source string, number int) func(context.Context, *quotaFixture) error {
	return func(ctx context.Context, f *quotaFixture) error {
		_, err := f.objects.CopyPart(ctx, porttest.Root(), app.CopyPartInput{
			Source: app.GetInput{Bucket: "b", Key: source},
			Part:   app.UploadPartInput{Bucket: "b", Key: f.upload.Key, UploadID: f.upload.ID, Number: number},
			Digest: tagged,
		})
		return err
	}
}

// filled returns a setup that puts objects of the sizes given, one key each.
func filled(sizes map[string]int) func(*testing.T, *quotaFixture) {
	return func(t *testing.T, f *quotaFixture) {
		t.Helper()
		for key, n := range sizes {
			f.put(t, "b", key, make([]byte, n))
		}
	}
}

// TestQuotaPreCheck: put, uploadPart, Copy and CopyPart ask the quota before
// any shard is written. A write whose declared size is over what is left
// is refused with nothing written - no shard begun, nothing counted - and
// the refusal is counted; a length that is not known is refused only when
// the bucket is full; what a write replaces is reckoned as the store
// reckons it, so that an overwrite of the same size where versions are not
// kept, and a part in place of one of the same size, pass a full bucket.
func TestQuotaPreCheck(t *testing.T) {
	t.Parallel()
	withUpload := func(sizes map[string]int, parts ...int) func(*testing.T, *quotaFixture) {
		return func(t *testing.T, f *quotaFixture) {
			t.Helper()
			filled(sizes)(t, f)
			f.upload = f.createUpload(t, "big.mp4")
			for i, n := range parts {
				f.uploadPart(t, f.upload, i+1, make([]byte, n))
			}
		}
	}
	for _, tt := range []struct {
		name    string
		quota   int64
		setup   func(*testing.T, *quotaFixture)
		write   func(context.Context, *quotaFixture) error
		refused bool
	}{
		{name: "a put within what is left", quota: 100, setup: filled(map[string]int{"a": 60}), write: quotaPut("c", 40, 40)},
		{name: "a put past what is left", quota: 100, setup: filled(map[string]int{"a": 60}), write: quotaPut("c", 41, 41), refused: true},
		{name: "a put of unknown length into a bucket with room", quota: 100, setup: filled(map[string]int{"a": 99}), write: quotaPut("c", -1, 1)},
		{name: "a put of unknown length into a full bucket", quota: 100, setup: filled(map[string]int{"a": 100}), write: quotaPut("c", -1, 1), refused: true},
		{name: "a put of unknown length into a bucket past its quota", quota: 100, setup: filled(map[string]int{"a": 150}), write: quotaPut("c", -1, 0), refused: true},
		{name: "a put of no bytes into a full bucket", quota: 100, setup: filled(map[string]int{"a": 100}), write: quotaPut("c", 0, 0)},
		{name: "an overwrite of the same size where versions are not kept", quota: 100, setup: filled(map[string]int{"a": 100}), write: quotaPut("a", 100, 100)},
		{name: "a larger overwrite where versions are not kept", quota: 100, setup: filled(map[string]int{"a": 100}), write: quotaPut("a", 101, 101), refused: true},
		{name: "an overwrite of unknown length where versions are not kept", quota: 100, setup: filled(map[string]int{"a": 100}), write: quotaPut("a", -1, 50)},
		{name: "an overwrite where versions are kept", quota: 100, setup: func(t *testing.T, f *quotaFixture) {
			t.Helper()
			f.versioning(t, "b", bucket.VersioningEnabled)
			filled(map[string]int{"a": 100})(t, f)
		}, write: quotaPut("a", 1, 1), refused: true},
		{name: "an overwrite of the null version where versioning is suspended", quota: 100, setup: func(t *testing.T, f *quotaFixture) {
			t.Helper()
			f.versioning(t, "b", bucket.VersioningEnabled)
			f.put(t, "b", "a", make([]byte, 40))
			f.versioning(t, "b", bucket.VersioningSuspended)
			f.put(t, "b", "a", make([]byte, 60))
		}, write: quotaPut("a", 60, 60)},
		{name: "a part within what is left", quota: 100, setup: withUpload(map[string]int{"a": 60}), write: quotaPart(1, 40)},
		{name: "a part past what is left", quota: 100, setup: withUpload(map[string]int{"a": 60}), write: quotaPart(1, 41), refused: true},
		{name: "a part of no bytes into a full bucket", quota: 100, setup: withUpload(map[string]int{"a": 60}, 40), write: quotaPart(2, 0)},
		{name: "a part in place of one of the same size in a full bucket", quota: 100, setup: withUpload(map[string]int{"a": 60}, 40), write: quotaPart(1, 40)},
		{name: "a larger part in place of one in a full bucket", quota: 100, setup: withUpload(map[string]int{"a": 60}, 40), write: quotaPart(1, 41), refused: true},
		{name: "a copy within what is left", quota: 100, setup: filled(map[string]int{"a": 50}), write: quotaCopy("a", "c")},
		{name: "a copy past what is left", quota: 100, setup: filled(map[string]int{"a": 60}), write: quotaCopy("a", "c"), refused: true},
		{name: "a part copied within what is left", quota: 100, setup: withUpload(map[string]int{"a": 50}), write: quotaCopyPart("a", 1)},
		{name: "a part copied past what is left", quota: 100, setup: withUpload(map[string]int{"a": 60}), write: quotaCopyPart("a", 1), refused: true},
		{name: "a bucket without a quota", setup: filled(map[string]int{"a": 1000}), write: quotaPut("c", 1000, 1000)},
		{name: "a bucket whose usage is being counted", quota: 100, setup: filled(map[string]int{"a": 100}), write: func(ctx context.Context, f *quotaFixture) error {
			// Neither the advice nor the store has a count to judge by.
			f.meta.ForgetUsage("b")
			return quotaPut("c", 1000, 1000)(ctx, f)
		}},
	} {
		f := newQuotaFixture(t, nil)
		tt.setup(t, f)
		if tt.quota != 0 {
			f.setQuota(t, tt.quota)
		}
		used := f.used(t)
		f.creates.Store(0)
		err := tt.write(t.Context(), f)
		switch {
		case tt.refused && !errors.Is(err, domain.ErrQuotaExceeded):
			t.Errorf("%s: error = %v, want %v", tt.name, err, domain.ErrQuotaExceeded)
		case !tt.refused && err != nil:
			t.Errorf("%s: error = %v", tt.name, err)
		}
		creates, refusals := f.creates.Load(), f.refusals.Load()
		switch {
		case tt.refused && (creates != 0 || refusals != 1):
			t.Errorf("%s: %d shards begun and %d refusals counted; want none begun and one counted", tt.name, creates, refusals)
		case !tt.refused && (creates == 0 || refusals != 0):
			t.Errorf("%s: %d shards begun and %d refusals counted; want shards and no refusal", tt.name, creates, refusals)
		}
		if tt.refused {
			if got := f.used(t); got != used {
				t.Errorf("%s: the bucket holds %d after the refusal, %d before", tt.name, got, used)
			}
		}
	}
}

// TestQuotaRefusedAtCommit: a write the pre-check let through - its advice
// was behind, or another writer won the race - and that the store refuses
// at commit with domain.ErrQuotaExceeded has its shards removed and returns
// that error; the refusal is counted once, and nothing is counted against
// the quota.
func TestQuotaRefusedAtCommit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		write func(context.Context, *quotaFixture) error
	}{
		{"a put", quotaPut("c", 41, 41)},
		{"a put of unknown length", quotaPut("c", -1, 41)},
		{"a part", quotaPart(1, 41)},
		{"a copy", quotaCopy("a", "c")},
		{"a part copied", quotaCopyPart("a", 1)},
	} {
		f := newQuotaFixture(t, func(m *porttest.MemMetadata) ports.BucketUsageStore { return noQuotaAdvice{m} })
		f.put(t, "b", "a", make([]byte, 60))
		f.upload = f.createUpload(t, "big.mp4")
		f.setQuota(t, 100)
		shards, used := f.shardCount(), f.used(t)
		f.creates.Store(0)
		err := tt.write(t.Context(), f)
		if !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, domain.ErrQuotaExceeded)
		}
		if f.creates.Load() == 0 {
			t.Errorf("%s: no shard was begun: the pre-check refused, not the store", tt.name)
		}
		if got := f.shardCount(); got != shards {
			t.Errorf("%s: %d shards on the drives after the refusal, %d before", tt.name, got, shards)
		}
		if got := f.used(t); got != used {
			t.Errorf("%s: the bucket holds %d after the refusal, %d before", tt.name, got, used)
		}
		if got := f.refusals.Load(); got != 1 {
			t.Errorf("%s: %d refusals counted, want 1", tt.name, got)
		}
	}
}

// TestQuotaNeverRefuses: completing an upload, deleting, removing a version
// for good, aborting an upload and tagging are never refused for a quota,
// in a bucket that is full and in one past its quota; the pre-check is not
// asked - its advice here says every bucket is full - and the store takes
// them.
func TestQuotaNeverRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var full atomic.Bool
	f := newQuotaFixture(t, func(m *porttest.MemMetadata) ports.BucketUsageStore {
		return fullAdvice{BucketUsageStore: m, full: &full}
	})
	f.versioning(t, "b", bucket.VersioningEnabled)
	first := f.put(t, "b", "a", make([]byte, 50))
	upload := f.createUpload(t, "big.mp4")
	part := f.uploadPart(t, upload, 1, make([]byte, 50))
	other := f.createUpload(t, "other.mp4")
	f.setQuota(t, 100)
	full.Store(true)
	if used := f.used(t); used != 100 {
		t.Fatalf("the bucket holds %d, want it full at 100", used)
	}
	firstRef := first.Ref()
	steps := []struct {
		name string
		do   func() error
	}{
		{"completion of an upload", func() error {
			_, err := f.objects.CompleteUpload(ctx, porttest.Root(), app.CompleteUploadInput{
				Bucket: "b", Key: upload.Key, UploadID: upload.ID, Parts: listed(part), ETag: "etag-of-the-parts-1",
			})
			return err
		}},
		{"a delete that makes a marker", func() error {
			_, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "a"})
			return err
		}},
		{"tags", func() error {
			_, err := f.objects.SetTags(ctx, porttest.Root(), app.LockInput{Bucket: "b", Key: "big.mp4"}, []object.Tag{{Key: "k", Value: "v"}})
			return err
		}},
		{"an abort", func() error {
			return f.objects.AbortUpload(ctx, porttest.Root(), app.UploadRef{Bucket: "b", Key: other.Key, UploadID: other.ID})
		}},
		{"a quota below what the bucket holds", func() error {
			return f.meta.SetBucketQuota(ctx, "b", 1, audit.Record{})
		}},
		{"a version removed for good past the quota", func() error {
			_, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "a", Version: &firstRef})
			return err
		}},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Errorf("%s: %v", step.name, err)
		}
	}
	if got := f.refusals.Load(); got != 0 {
		t.Errorf("%d refusals counted, want none", got)
	}
	if used := f.used(t); used != 50 {
		t.Errorf("the bucket holds %d at the end, want the 50 of the completed upload", used)
	}
}

// fixedUsage answers one record for every bucket, or an error.
type fixedUsage struct {
	ports.BucketUsageStore
	record ports.BucketUsage
	err    error
}

func (u fixedUsage) BucketUsages(_ context.Context, names []string) ([]ports.BucketUsage, error) {
	if u.err != nil {
		return nil, u.err
	}
	out := make([]ports.BucketUsage, len(names))
	for i := range out {
		out[i] = u.record
	}
	return out, nil
}

// TestQuotaAdmit: the pre-check by itself. It judges by the rule of the
// store (bucket.Usage.Admits) on what a write adds less what it frees,
// reads what it frees only when it has to, judges a length that is not
// known as one byte, and lets everything through where there is nothing to
// judge by.
func TestQuotaAdmit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	holding := func(used, quota int64) ports.BucketUsage {
		return ports.BucketUsage{QuotaBytes: quota, Usage: bucket.Usage{Exact: true, UsageTotals: bucket.UsageTotals{StoredBytes: used - used/4, UploadBytes: used / 4}}}
	}
	broken := errors.New("the state cannot be read")
	freeing := func(n int64, err error) func(context.Context) (int64, error) {
		return func(context.Context) (int64, error) { return n, err }
	}
	for _, tt := range []struct {
		name     string
		usage    fixedUsage
		declared int64
		frees    func(context.Context) (int64, error)
		wantErr  error
		// wantFrees says whether what the write frees is read.
		wantFrees bool
	}{
		{name: "within", usage: fixedUsage{record: holding(60, 100)}, declared: 40},
		{name: "one byte past", usage: fixedUsage{record: holding(60, 100)}, declared: 41, wantErr: domain.ErrQuotaExceeded},
		{name: "past, freeing enough", usage: fixedUsage{record: holding(100, 100)}, declared: 41, frees: freeing(41, nil), wantFrees: true},
		{name: "past, freeing too little", usage: fixedUsage{record: holding(100, 100)}, declared: 41, frees: freeing(40, nil), wantErr: domain.ErrQuotaExceeded, wantFrees: true},
		{name: "past, freeing below nothing", usage: fixedUsage{record: holding(100, 100)}, declared: 1, frees: freeing(-5, nil), wantErr: domain.ErrQuotaExceeded, wantFrees: true},
		{name: "past, what it frees not readable", usage: fixedUsage{record: holding(100, 100)}, declared: 1, frees: freeing(0, broken), wantErr: broken, wantFrees: true},
		{name: "unknown length with room", usage: fixedUsage{record: holding(99, 100)}, declared: -1},
		{name: "unknown length into a full bucket", usage: fixedUsage{record: holding(100, 100)}, declared: -1, wantErr: domain.ErrQuotaExceeded},
		{name: "unknown length freeing something", usage: fixedUsage{record: holding(100, 100)}, declared: -1, frees: freeing(1, nil), wantFrees: true},
		{name: "nothing into a bucket past its quota", usage: fixedUsage{record: holding(500, 100)}, declared: 0},
		{name: "the greatest quota", usage: fixedUsage{record: holding(bucket.MaxQuotaBytes-object.MaxSize, bucket.MaxQuotaBytes)}, declared: object.MaxSize},
		{name: "a count held at its bound", usage: fixedUsage{record: holding(math.MaxInt64, bucket.MaxQuotaBytes)}, declared: 1, wantErr: domain.ErrQuotaExceeded},
		{name: "no quota", usage: fixedUsage{record: ports.BucketUsage{Usage: bucket.Usage{Exact: true, UsageTotals: bucket.UsageTotals{StoredBytes: 1 << 40}}}}, declared: 1 << 40},
		{name: "not counted yet", usage: fixedUsage{record: ports.BucketUsage{QuotaBytes: 1}}, declared: 1 << 40},
		{name: "a record that cannot be read", usage: fixedUsage{err: broken}, declared: 1, wantErr: broken},
	} {
		var refusals int
		quotas := app.NewQuotaService(app.QuotaServiceConfig{Usage: tt.usage, OnRefusal: func() { refusals++ }})
		var asked bool
		frees := tt.frees
		if frees != nil {
			frees = func(ctx context.Context) (int64, error) {
				asked = true
				return tt.frees(ctx)
			}
		}
		err := quotas.Admit(ctx, "b", tt.declared, frees)
		if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
			t.Errorf("%s: Admit = %v, want %v", tt.name, err, tt.wantErr)
		}
		if asked != tt.wantFrees {
			t.Errorf("%s: what the write frees was read: %t, want %t", tt.name, asked, tt.wantFrees)
		}
		if want := errors.Is(tt.wantErr, domain.ErrQuotaExceeded); (refusals == 1) != want || refusals > 1 {
			t.Errorf("%s: %d refusals counted", tt.name, refusals)
		}
	}
}

// levelOf is the metadata level of the fake, as the composition root gives
// the store's to the use case.
func levelOf(m *porttest.MemMetadata) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return m.MetadataLevel() }
}

// readsFailAfterSet is a usage store whose records cannot be read once a
// quota was set.
type readsFailAfterSet struct {
	ports.BucketUsageStore
	set bool
}

func (r *readsFailAfterSet) SetBucketQuota(ctx context.Context, name string, quotaBytes int64, record audit.Record) error {
	r.set = true
	return r.BucketUsageStore.SetBucketQuota(ctx, name, quotaBytes, record)
}

func (r *readsFailAfterSet) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	if r.set {
		return nil, errors.New("the state cannot be read")
	}
	return r.BucketUsageStore.BucketUsages(ctx, names)
}

// TestQuotaService: what Get, Set and Delete answer and refuse, step by
// step on one bucket: a quota from 1 to 2^62, nothing proposed for one out
// of bounds, domain.ErrConflict while the bucket is being counted, every
// change in the trail through its entry and nothing for what was refused.
func TestQuotaService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	f.meta.Audit = f.store
	for _, name := range []string{"photos", "old"} {
		if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.meta.Fill("photos", "k%03d", 3, 10); err != nil {
		t.Fatal(err)
	}
	f.meta.ForgetUsage("old")
	quotas := app.NewQuotaService(app.QuotaServiceConfig{Buckets: f.meta, Usage: f.meta, Authz: f.authz, Auditor: f.auditor, Level: levelOf(f.meta)})
	set := func(name string, n int64) func() (app.BucketQuota, error) {
		return func() (app.BucketQuota, error) { return quotas.Set(ctx, f.root, name, n) }
	}
	get := func(name string) func() (app.BucketQuota, error) {
		return func() (app.BucketQuota, error) { return quotas.Get(ctx, f.root, name) }
	}
	remove := func(name string) func() (app.BucketQuota, error) {
		return func() (app.BucketQuota, error) { return app.BucketQuota{}, quotas.Delete(ctx, f.root, name) }
	}
	none := app.BucketQuota{UsedBytes: 30, Exact: true}
	base := len(f.trail(t))
	for _, step := range []struct {
		name string
		do   func() (app.BucketQuota, error)
		want app.BucketQuota
		err  error
	}{
		{"get without a quota", get("photos"), none, nil},
		{"set", set("photos", 1000), app.BucketQuota{QuotaBytes: 1000, UsedBytes: 30, Exact: true}, nil},
		{"get", get("photos"), app.BucketQuota{QuotaBytes: 1000, UsedBytes: 30, Exact: true}, nil},
		{"set below what it holds", set("photos", 1), app.BucketQuota{QuotaBytes: 1, UsedBytes: 30, Exact: true}, nil},
		{"set the greatest", set("photos", bucket.MaxQuotaBytes), app.BucketQuota{QuotaBytes: bucket.MaxQuotaBytes, UsedBytes: 30, Exact: true}, nil},
		{"set beyond the greatest", set("photos", bucket.MaxQuotaBytes+1), app.BucketQuota{}, domain.ErrInvalidArgument},
		{"set nothing", set("photos", 0), app.BucketQuota{}, domain.ErrInvalidArgument},
		{"set below nothing", set("photos", -1), app.BucketQuota{}, domain.ErrInvalidArgument},
		{"what was refused changed nothing", get("photos"), app.BucketQuota{QuotaBytes: bucket.MaxQuotaBytes, UsedBytes: 30, Exact: true}, nil},
		{"delete", remove("photos"), app.BucketQuota{}, nil},
		{"get after delete", get("photos"), none, nil},
		{"delete when there is none", remove("photos"), app.BucketQuota{}, nil},
		{"get while counted", get("old"), app.BucketQuota{}, nil},
		{"set while counted", set("old", 1000), app.BucketQuota{}, domain.ErrConflict},
		{"delete while counted", remove("old"), app.BucketQuota{}, domain.ErrConflict},
		{"get of no bucket", get("missing"), app.BucketQuota{}, domain.ErrBucketNotFound},
		{"set of no bucket", set("missing", 1000), app.BucketQuota{}, domain.ErrBucketNotFound},
		{"delete of no bucket", remove("missing"), app.BucketQuota{}, domain.ErrBucketNotFound},
	} {
		got, err := step.do()
		if !errors.Is(err, step.err) || (step.err == nil) != (err == nil) || got != step.want {
			t.Errorf("%s = %+v, %v; want %+v, %v", step.name, got, err, step.want, step.err)
		}
	}
	// Five changes, each in the trail through its entry, with the quota
	// it set; nothing for what was refused.
	var changes []audit.Record
	for _, r := range f.trail(t)[base:] {
		if r.Outcome == audit.Succeeded {
			changes = append(changes, r)
		}
	}
	want := []struct {
		action iam.Action
		quota  string
	}{
		{iam.ActionPutBucketQuota, "1000"},
		{iam.ActionPutBucketQuota, "1"},
		{iam.ActionPutBucketQuota, "4611686018427387904"},
		{iam.ActionDeleteBucketQuota, ""},
		{iam.ActionDeleteBucketQuota, ""},
	}
	if len(changes) != len(want) {
		t.Fatalf("%d changes in the trail, want %d: %+v", len(changes), len(want), changes)
	}
	for i, w := range want {
		r := changes[i]
		if r.Action != string(w.action) || r.Resource != "arn:aws:s3:::photos" || r.Details["quota_bytes"] != w.quota || r.Actor != "root" {
			t.Errorf("change %d in the trail = %+v, want %s with quota %q", i, r, w.action, w.quota)
		}
	}
}

// TestQuotaServiceFaults: below level 5 everything of quotas is
// unavailable (ADR-0024) and the pre-check admits everything, as the store
// keeps no quota; without the level Get is unavailable too, and a level that
// cannot be read fails it; without an audit trail nothing is changed; a
// quota that was set but cannot be read back is an error that says so.
func TestQuotaServiceFaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	// A cluster with a member of schema 4: its buckets have no usage
	// record, and the store refuses the changes of quotas.
	old := porttest.NewMemMetadata()
	old.Level = ports.UsageLevel - 1
	if err := old.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	below := app.NewQuotaService(app.QuotaServiceConfig{Buckets: old, Usage: old, Authz: f.authz, Auditor: f.auditor, Level: levelOf(old)})
	if _, err := below.Get(ctx, f.root, "photos"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Get below level 5 = %v", err)
	}
	if _, err := below.Set(ctx, f.root, "photos", 10); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Set below level 5 = %v", err)
	}
	if err := below.Delete(ctx, f.root, "photos"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Delete below level 5 = %v", err)
	}
	if b, err := old.GetBucket(ctx, "photos"); err != nil || b.QuotaBytes != 0 {
		t.Errorf("below level 5 the quota is %d, %v", b.QuotaBytes, err)
	}
	if err := below.Admit(ctx, "photos", 1<<40, nil); err != nil {
		t.Errorf("Admit below level 5 = %v, want everything admitted", err)
	}
	unknown := app.NewQuotaService(app.QuotaServiceConfig{Buckets: f.meta, Usage: f.meta, Authz: f.authz, Auditor: f.auditor})
	if _, err := unknown.Get(ctx, f.root, "photos"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Get without the level = %v", err)
	}
	broken := errors.New("the level cannot be read")
	unreadLevel := app.NewQuotaService(app.QuotaServiceConfig{
		Buckets: f.meta, Usage: f.meta, Authz: f.authz, Auditor: f.auditor,
		Level: func(context.Context) (int, error) { return 0, broken },
	})
	if _, err := unreadLevel.Get(ctx, f.root, "photos"); !errors.Is(err, broken) {
		t.Errorf("Get when the level cannot be read = %v", err)
	}

	unaudited := app.NewQuotaService(app.QuotaServiceConfig{Buckets: f.meta, Usage: f.meta, Authz: f.authz})
	if _, err := unaudited.Set(ctx, f.root, "photos", 10); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Set without an audit trail = %v", err)
	}
	if b, err := f.meta.GetBucket(ctx, "photos"); err != nil || b.QuotaBytes != 0 {
		t.Errorf("after a change without an audit trail the quota is %d, %v", b.QuotaBytes, err)
	}

	unread := app.NewQuotaService(app.QuotaServiceConfig{Buckets: f.meta, Usage: &readsFailAfterSet{BucketUsageStore: f.meta}, Authz: f.authz, Auditor: f.auditor})
	if _, err := unread.Set(ctx, f.root, "photos", 10); err == nil || !strings.Contains(err.Error(), "was set") {
		t.Errorf("Set that cannot be read back = %v", err)
	}
	if b, err := f.meta.GetBucket(ctx, "photos"); err != nil || b.QuotaBytes != 10 {
		t.Errorf("after a set that cannot be read back the quota is %d, %v", b.QuotaBytes, err)
	}
}

// TestQuotaServiceIsAuthorized: cluster-admin reads, sets and removes a
// quota; auditor reads it; bucket-admin, whose s3:* is the bucket's, and
// the other roles do none of it. A refusal proposes nothing and is in the
// trail, for the action and the bucket.
func TestQuotaServiceIsAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	f.meta.Audit = f.store
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	quotas := app.NewQuotaService(app.QuotaServiceConfig{Buckets: f.meta, Usage: f.meta, Authz: f.authz, Auditor: f.auditor, Level: levelOf(f.meta)})
	if _, err := quotas.Set(ctx, f.root, "photos", 500); err != nil {
		t.Fatal(err)
	}
	roles := []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin, iam.RoleReadWrite, iam.RoleReadOnly, iam.RoleAuditor}
	principals := map[string]iam.Principal{}
	for _, role := range roles {
		principals[role] = f.as(t, f.user(t, "user-"+role, role))
	}
	operations := []struct {
		name    string
		action  iam.Action
		call    func(p iam.Principal) error
		allowed []string
	}{
		{"get", iam.ActionGetBucketQuota, func(p iam.Principal) error {
			_, err := quotas.Get(ctx, p, "photos")
			return err
		}, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{"set", iam.ActionPutBucketQuota, func(p iam.Principal) error {
			_, err := quotas.Set(ctx, p, "photos", 500)
			return err
		}, []string{iam.RoleClusterAdmin}},
		{"delete", iam.ActionDeleteBucketQuota, func(p iam.Principal) error {
			err := quotas.Delete(ctx, p, "photos")
			if err == nil {
				// Put back for the next role.
				_, err = quotas.Set(ctx, f.root, "photos", 500)
			}
			return err
		}, []string{iam.RoleClusterAdmin}},
	}
	for _, op := range operations {
		for _, role := range roles {
			err := op.call(principals[role])
			allowed := slices.Contains(op.allowed, role)
			switch {
			case allowed && err != nil:
				t.Errorf("%s by %s: %v", op.name, role, err)
			case !allowed && !errors.Is(err, domain.ErrAccessDenied):
				t.Errorf("%s by %s = %v, want %v", op.name, role, err, domain.ErrAccessDenied)
			}
			if allowed {
				continue
			}
			denied, changed := false, false
			for _, r := range f.trail(t) {
				mine := r.Actor == "user/user-"+role && r.Action == string(op.action) && r.Resource == "arn:aws:s3:::photos"
				denied = denied || (mine && r.Outcome == audit.Denied)
				changed = changed || (mine && r.Outcome == audit.Succeeded)
			}
			if !denied || changed {
				t.Errorf("%s by %s: denial in the trail %t, change in the trail %t; want a denial and no change", op.name, role, denied, changed)
			}
			if b, err := f.meta.GetBucket(ctx, "photos"); err != nil || b.QuotaBytes != 500 {
				t.Errorf("%s by %s: the quota is %d after the refusal, %v", op.name, role, b.QuotaBytes, err)
			}
		}
	}
	// The pre-check asks nobody: it is the write's, which was authorized.
	before := len(f.trail(t))
	if err := quotas.Admit(ctx, "photos", 1, nil); err != nil {
		t.Errorf("Admit: %v", err)
	}
	if after := len(f.trail(t)); after != before {
		t.Errorf("Admit wrote %d records to the trail", after-before)
	}
}
