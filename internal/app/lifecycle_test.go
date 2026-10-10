package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func (f *fixture) lifecycleWorker(active func() bool) *app.LifecycleWorker {
	return app.NewLifecycleWorker(app.LifecycleWorkerConfig{
		Buckets: f.meta, Index: f.meta, Uploads: f.meta, Objects: f.objects, Clock: f.clock,
		Logger: slog.New(slog.DiscardHandler), Active: active,
	})
}

func (f *fixture) putIn(t *testing.T, bucketName, key string, lock object.Lock) object.Version {
	t.Helper()
	v, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
		Bucket: bucketName, Key: key, Body: bytes.NewReader([]byte(key)), Size: int64(len(key)), Lock: lock,
	})
	if err != nil {
		t.Fatalf("Put %s/%s: %v", bucketName, key, err)
	}
	return v
}

func (f *fixture) versionsOf(t *testing.T, bucketName string) []string {
	t.Helper()
	var out []string
	err := f.meta.ScanVersions(context.Background(), bucketName, ports.VersionCursor{}, func(v object.Version, latest bool) ports.ScanControl {
		kind := "version"
		if v.DeleteMarker {
			kind = "marker"
		}
		if latest {
			kind += ",latest"
		}
		out = append(out, v.Key+":"+kind)
		return ports.ScanControl{}
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) setRules(t *testing.T, bucketName string, rules ...bucket.LifecycleRule) {
	t.Helper()
	if err := f.buckets.SetLifecycle(context.Background(), porttest.Root(), bucketName, &bucket.Lifecycle{Rules: rules}); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
}

// TestLifecycleInAPlainBucket: objects under a rule's prefix go when they
// are old enough, and nothing else does; uploads that were never finished
// are aborted.
func TestLifecycleInAPlainBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.setRules(t, "b",
		bucket.LifecycleRule{ID: "logs", Enabled: true, Prefix: "logs/", ExpirationDays: 2},
		bucket.LifecycleRule{ID: "uploads", Enabled: true, AbortIncompleteDays: 1},
		bucket.LifecycleRule{ID: "off", Enabled: false, Prefix: "keep/", ExpirationDays: 1},
	)
	f.putIn(t, "b", "logs/old", object.Lock{})
	f.putIn(t, "b", "keep/old", object.Lock{})
	f.putIn(t, "b", "other", object.Lock{})
	stale := f.createUpload(t, "logs/never-finished")
	f.uploadPart(t, stale, 1, randomBytes(t, 3000))
	worker := f.lifecycleWorker(nil)

	// Too early for anything.
	f.clock.Advance(20 * time.Hour)
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Buckets != 1 || stats.Expired+stats.Aborted+stats.Failed != 0 {
		t.Fatalf("an early pass = %+v, %v", stats, err)
	}
	f.clock.Advance(3 * 24 * time.Hour)
	f.putIn(t, "b", "logs/new", object.Lock{})
	fresh := f.createUpload(t, "fresh")
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.Expired != 1 || stats.Aborted != 1 || stats.Noncurrent != 0 || stats.Markers != 0 || stats.Kept != 0 || stats.Failed != 0 {
		t.Fatalf("pass = %+v, %v; want one object expired and one upload aborted", stats, err)
	}
	if got := f.versionsOf(t, "b"); fmt.Sprint(got) != "[keep/old:version,latest logs/new:version,latest other:version,latest]" {
		t.Errorf("after the pass the bucket holds %v", got)
	}
	if _, err := f.meta.GetUpload(ctx, "b", stale.Key, stale.ID); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("the stale upload: %v", err)
	}
	if _, err := f.meta.GetUpload(ctx, "b", fresh.Key, fresh.ID); err != nil {
		t.Errorf("the fresh upload: %v", err)
	}
	// What went is gone from the drives too: three objects are left.
	if f.shardCount() != 3*sixDrives {
		t.Errorf("%d shards stored after the pass, want those of three objects", f.shardCount())
	}
	// A second pass finds nothing to do.
	if again, err := worker.RunOnce(ctx); err != nil || again.Expired+again.Aborted != 0 {
		t.Errorf("second pass = %+v, %v", again, err)
	}
	// Without rules, nothing ever goes.
	if err := f.buckets.SetLifecycle(ctx, porttest.Root(), "b", nil); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(100 * 24 * time.Hour)
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Buckets != 0 || len(f.versionsOf(t, "b")) != 3 {
		t.Errorf("a pass without rules = %+v, %v", stats, err)
	}
}

// TestLifecycleWhereVersionsAreKept: expiring makes a delete marker; older
// versions go a number of days after they were replaced; a marker that is
// all that is left of a key goes last.
func TestLifecycleWhereVersionsAreKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "ver", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := f.meta.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	f.setRules(t, "ver",
		bucket.LifecycleRule{ID: "expire", Enabled: true, ExpirationDays: 3, NoncurrentDays: 1},
		bucket.LifecycleRule{ID: "markers", Enabled: true, ExpiredObjectDeleteMarker: true},
	)
	f.putIn(t, "ver", "doc", object.Lock{})
	f.clock.Advance(time.Hour)
	f.putIn(t, "ver", "doc", object.Lock{})
	worker := f.lifecycleWorker(nil)

	// A day and a half later the first version has been replaced for more
	// than a day; the second is not three days old.
	f.clock.Advance(47 * time.Hour)
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Noncurrent != 1 || stats.Expired != 0 || stats.Markers != 0 {
		t.Fatalf("first pass = %+v, %v; want the replaced version removed", stats, err)
	}
	if got := f.versionsOf(t, "ver"); fmt.Sprint(got) != "[doc:version,latest]" {
		t.Fatalf("after the first pass: %v", got)
	}
	// Three days on, the current version expires: a marker, and nothing
	// removed.
	f.clock.Advance(3 * 24 * time.Hour)
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Expired != 1 || stats.Noncurrent != 0 || stats.Markers != 0 {
		t.Fatalf("second pass = %+v, %v; want the current version expired", stats, err)
	}
	if got := f.versionsOf(t, "ver"); fmt.Sprint(got) != "[doc:marker,latest doc:version]" {
		t.Fatalf("after the second pass: %v", got)
	}
	if f.shardCount() != sixDrives {
		t.Errorf("expiring removed data: %d shards", f.shardCount())
	}
	// At once again: the marker is not expired into another marker, and
	// the version under it was replaced only now.
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Expired+stats.Noncurrent+stats.Markers != 0 {
		t.Fatalf("a pass right after = %+v, %v", stats, err)
	}
	// Two days on, the version under the marker goes ...
	f.clock.Advance(2 * 24 * time.Hour)
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Noncurrent != 1 || stats.Markers != 0 {
		t.Fatalf("third pass = %+v, %v", stats, err)
	}
	if got := f.versionsOf(t, "ver"); fmt.Sprint(got) != "[doc:marker,latest]" || f.shardCount() != 0 {
		t.Fatalf("after the third pass: %v, %d shards", got, f.shardCount())
	}
	// ... and then the marker, which is all that is left.
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Markers != 1 {
		t.Fatalf("fourth pass = %+v, %v", stats, err)
	}
	if got := f.versionsOf(t, "ver"); len(got) != 0 {
		t.Errorf("at the end: %v", got)
	}
}

// TestLifecycleNeverRemovesWhatALockKeeps: a rule that says a retained
// version is to go does not make it go.
func TestLifecycleNeverRemovesWhatALockKeeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.meta.Now = f.clock.Now
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "vault", ObjectLock: true, CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	f.setRules(t, "vault", bucket.LifecycleRule{ID: "everything", Enabled: true, ExpirationDays: 1, NoncurrentDays: 1})
	year := 365 * 24 * time.Hour
	compliance := f.putIn(t, "vault", "ledger", object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: testNow.Add(year)}})
	governance := f.putIn(t, "vault", "draft", object.Lock{Retention: &object.Retention{Mode: object.Governance, Until: testNow.Add(year)}})
	held := f.putIn(t, "vault", "evidence", object.Lock{LegalHold: true})
	free := f.putIn(t, "vault", "scratch", object.Lock{})
	worker := f.lifecycleWorker(nil)

	f.clock.Advance(3 * 24 * time.Hour)
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Expired != 4 || stats.Kept != 0 {
		t.Fatalf("first pass = %+v, %v; want four markers", stats, err)
	}
	f.clock.Advance(3 * 24 * time.Hour)
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.Noncurrent != 1 || stats.Kept != 3 || stats.Failed != 0 {
		t.Fatalf("second pass = %+v, %v; want one version removed and three kept by their locks", stats, err)
	}
	for _, v := range []object.Version{compliance, governance, held} {
		if got, err := f.meta.GetVersion(ctx, "vault", v.Key, v.Ref()); err != nil || got.DataID != v.DataID {
			t.Errorf("the locked version of %s: %v", v.Key, err)
		}
	}
	if _, err := f.meta.GetVersion(ctx, "vault", free.Key, free.Ref()); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Errorf("the version without a lock: %v", err)
	}
	if f.shardCount() != 3*sixDrives {
		t.Errorf("%d shards stored, want those of the three locked versions", f.shardCount())
	}
	// However often it tries.
	for range 3 {
		f.clock.Advance(24 * time.Hour)
		if stats, err := worker.RunOnce(ctx); err != nil || stats.Kept != 3 || stats.Noncurrent != 0 {
			t.Fatalf("a later pass = %+v, %v", stats, err)
		}
	}
	// And the worker asks for nothing a client could not: every request
	// it made was a delete or an abort by the lifecycle service, none
	// with the governance bypass.
	asked := 0
	for _, req := range f.authz.Requests() {
		removes := req.Action == iam.ActionDeleteObject || req.Action == iam.ActionDeleteObjectVersion || req.Action == iam.ActionAbortMultipartUpload
		if !removes && req.Principal.Service == "" {
			continue // the puts of this test, as root
		}
		asked++
		if req.Principal.Service != iam.ServiceLifecycle || !removes || req.Principal.Root || req.Principal.AccessKeyID != "" {
			t.Errorf("the worker asked for %s as %q", req.Action, req.Principal.Name())
		}
	}
	if asked == 0 {
		t.Error("no request of the worker was seen")
	}
}

// TestLifecycleAcrossBatches: a bucket larger than one batch of the walk is
// handled whole, and a key whose versions span two batches as one key.
func TestLifecycleAcrossBatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "ver", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := f.meta.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	f.setRules(t, "ver",
		bucket.LifecycleRule{ID: "old", Enabled: true, NoncurrentDays: 1},
		bucket.LifecycleRule{ID: "markers", Enabled: true, ExpiredObjectDeleteMarker: true},
	)
	// 499 keys with one version each, then a key with three versions that
	// begins as the last entry of the first batch, then a lone marker.
	for i := range 499 {
		f.putIn(t, "ver", fmt.Sprintf("a/%04d", i), object.Lock{})
	}
	for range 3 {
		f.putIn(t, "ver", "b/spans", object.Lock{})
	}
	f.putIn(t, "ver", "c/gone", object.Lock{})
	f.clock.Advance(time.Hour)
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "ver", Key: "c/gone"}); err != nil {
		t.Fatal(err)
	}
	worker := f.lifecycleWorker(nil)
	f.clock.Advance(3 * 24 * time.Hour)
	stats, err := worker.RunOnce(ctx)
	// Two older versions of b/spans and the one under the marker of
	// c/gone; the marker of c/gone is not alone in this pass.
	if err != nil || stats.Noncurrent != 3 || stats.Markers != 0 || stats.Expired != 0 || stats.Failed != 0 {
		t.Fatalf("pass = %+v, %v", stats, err)
	}
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Markers != 1 || stats.Noncurrent != 0 {
		t.Fatalf("second pass = %+v, %v", stats, err)
	}
	if got := f.versionsOf(t, "ver"); len(got) != 500 || got[499] != "b/spans:version,latest" {
		t.Errorf("%d versions left, the last %q", len(got), got[len(got)-1])
	}
}

// TestLifecycleRulesThatCannotBeRead: rules that are damaged remove nothing,
// and do not stop the rules of other buckets from being carried out.
func TestLifecycleRulesThatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "a-damaged", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	f.putIn(t, "a-damaged", "k", object.Lock{})
	f.putIn(t, "b", "k", object.Lock{})
	for _, document := range []string{"not rules", `{"rules":[]}`, `{"rules":[{"enabled":true}]}`} {
		if err := f.meta.SetBucketConfig(ctx, "a-damaged", bucket.ConfigLifecycle, []byte(document)); err != nil {
			t.Fatal(err)
		}
		f.setRules(t, "b", bucket.LifecycleRule{Enabled: true, ExpirationDays: 1})
		f.clock.Advance(3 * 24 * time.Hour)
		stats, err := f.lifecycleWorker(nil).RunOnce(ctx)
		if err != nil || stats.Buckets != 1 || len(f.versionsOf(t, "a-damaged")) != 1 {
			t.Errorf("with the rules %q: %+v, %v", document, stats, err)
		}
		if len(f.versionsOf(t, "b")) != 0 {
			t.Errorf("with the rules %q of another bucket damaged, the healthy bucket was not handled", document)
		}
		f.putIn(t, "b", "k", object.Lock{})
	}
	if _, err := f.buckets.Lifecycle(ctx, porttest.Root(), "a-damaged"); err != nil {
		t.Errorf("reading rules that validate badly but parse: %v", err)
	}
}

// TestLifecycleRunsOnOneNode: a node that does not lead does nothing.
func TestLifecycleRunsOnOneNode(t *testing.T) {
	t.Parallel()
	f := erasureFixture(t)
	f.setRules(t, "b", bucket.LifecycleRule{Enabled: true, ExpirationDays: 1})
	f.putIn(t, "b", "k", object.Lock{})
	f.clock.Advance(3 * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	f.lifecycleWorker(func() bool { return false }).Run(ctx, 5*time.Millisecond)
	if got := f.versionsOf(t, "b"); len(got) != 1 {
		t.Errorf("a standby node carried out rules: %v", got)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.lifecycleWorker(nil).Run(ctx, 5*time.Millisecond)
	}()
	for ctx.Err() == nil && len(f.versionsOf(t, "b")) != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := f.versionsOf(t, "b"); len(got) != 0 {
		t.Errorf("the active node did not carry out the rule: %v", got)
	}
}

// TestLifecycleConfiguration: who may read and set the rules of a bucket.
func TestLifecycleConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	rules := &bucket.Lifecycle{Rules: []bucket.LifecycleRule{{ID: "r", Enabled: true, ExpirationDays: 7}}}
	may := func(actions ...string) iam.Principal {
		doc := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: actions, Resources: []string{"*"}}}}
		return iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{doc}}
	}
	if got, err := f.buckets.Lifecycle(ctx, porttest.Root(), "b"); err != nil || got != nil {
		t.Errorf("Lifecycle of a bucket without rules = %+v, %v", got, err)
	}
	for name, p := range map[string]iam.Principal{"nobody": anonymous(), "a writer of objects": may("s3:PutObject", "s3:DeleteObject"), "a reader of rules": may("s3:GetLifecycleConfiguration")} {
		if err := f.buckets.SetLifecycle(ctx, p, "b", rules); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("SetLifecycle by %s = %v", name, err)
		}
	}
	if _, err := f.buckets.Lifecycle(ctx, may("s3:PutLifecycleConfiguration"), "b"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Lifecycle by one who may only set = %v", err)
	}
	if err := f.buckets.SetLifecycle(ctx, porttest.Root(), "b", &bucket.Lifecycle{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetLifecycle with no rules = %v", err)
	}
	if err := f.buckets.SetLifecycle(ctx, porttest.Root(), "missing", rules); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("SetLifecycle of a missing bucket = %v", err)
	}
	if err := f.buckets.SetLifecycle(ctx, may("s3:PutLifecycleConfiguration"), "b", rules); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
	got, err := f.buckets.Lifecycle(ctx, may("s3:GetLifecycleConfiguration"), "b")
	if err != nil || got == nil || len(got.Rules) != 1 || got.Rules[0] != rules.Rules[0] {
		t.Errorf("Lifecycle = %+v, %v", got, err)
	}
	if err := f.buckets.SetLifecycle(ctx, porttest.Root(), "b", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := f.buckets.Lifecycle(ctx, porttest.Root(), "b"); err != nil || got != nil {
		t.Errorf("Lifecycle after removal = %+v, %v", got, err)
	}
}

// interposed lets a client act between the moment the worker has read the
// state and its first removal.
type interposed struct {
	app.LifecycleObjects
	before func()
	fail   error
	calls  int
}

func (i *interposed) Delete(ctx context.Context, p iam.Principal, in app.DeleteInput) (app.DeleteOutput, error) {
	i.calls++
	if i.before != nil {
		before := i.before
		i.before = nil
		before()
	}
	if i.fail != nil {
		return app.DeleteOutput{}, i.fail
	}
	return i.LifecycleObjects.Delete(ctx, p, in)
}

func (f *fixture) workerOver(objects app.LifecycleObjects, active func() bool) *app.LifecycleWorker {
	return app.NewLifecycleWorker(app.LifecycleWorkerConfig{
		Buckets: f.meta, Index: f.meta, Uploads: f.meta, Objects: objects, Clock: f.clock,
		Logger: slog.New(slog.DiscardHandler), Active: active,
	})
}

// TestLifecycleDoesNotRemoveWhatWasWrittenMeanwhile: a pass decides on one
// reading of the state and removes afterwards. What a client writes in
// between is not what the rule was about, and stays.
func TestLifecycleDoesNotRemoveWhatWasWrittenMeanwhile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		versioning bucket.Versioning
		rule       bucket.LifecycleRule
		// before prepares the key; meanwhile is what the client does
		// after the worker has looked.
		before    func(f *fixture)
		meanwhile func(f *fixture)
		want      string
	}{
		{
			name: "an object overwritten, where no versions are kept", versioning: bucket.Unversioned,
			rule:      bucket.LifecycleRule{Enabled: true, ExpirationDays: 1},
			before:    func(f *fixture) { f.putIn(t, "race", "k", object.Lock{}) },
			meanwhile: func(f *fixture) { f.putIn(t, "race", "k", object.Lock{}) },
			want:      "[k:version,latest]",
		},
		{
			name: "a new version, where versions are kept", versioning: bucket.VersioningEnabled,
			rule:      bucket.LifecycleRule{Enabled: true, ExpirationDays: 1},
			before:    func(f *fixture) { f.putIn(t, "race", "k", object.Lock{}) },
			meanwhile: func(f *fixture) { f.putIn(t, "race", "k", object.Lock{}) },
			// No marker over the fresh version.
			want: "[k:version,latest k:version]",
		},
		{
			name: "the null version written again, where versioning is suspended", versioning: bucket.VersioningSuspended,
			rule: bucket.LifecycleRule{Enabled: true, NoncurrentDays: 1},
			before: func(f *fixture) {
				// A null version under a newer, versioned one.
				f.putIn(t, "race", "k", object.Lock{})
				_ = f.meta.SetBucketVersioning(context.Background(), "race", bucket.VersioningEnabled)
				f.clock.Advance(time.Hour)
				f.putIn(t, "race", "k", object.Lock{})
				_ = f.meta.SetBucketVersioning(context.Background(), "race", bucket.VersioningSuspended)
			},
			// The client's put is the new null version, and current: a
			// rule about older versions must not remove it.
			meanwhile: func(f *fixture) { f.putIn(t, "race", "k", object.Lock{}) },
			want:      "[k:version,latest k:version]",
		},
		{
			name: "a lone null marker replaced by an object", versioning: bucket.VersioningSuspended,
			rule: bucket.LifecycleRule{Enabled: true, ExpiredObjectDeleteMarker: true},
			before: func(f *fixture) {
				if _, err := f.objects.Delete(context.Background(), porttest.Root(), app.DeleteInput{Bucket: "race", Key: "k"}); err != nil {
					t.Fatal(err)
				}
			},
			meanwhile: func(f *fixture) { f.putIn(t, "race", "k", object.Lock{}) },
			want:      "[k:version,latest]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := erasureFixture(t)
			if err := f.meta.CreateBucket(context.Background(), bucket.Bucket{Name: "race", CreatedAt: testNow}); err != nil {
				t.Fatal(err)
			}
			if tt.versioning != bucket.Unversioned {
				if err := f.meta.SetBucketVersioning(context.Background(), "race", tt.versioning); err != nil {
					t.Fatal(err)
				}
			}
			f.setRules(t, "race", tt.rule)
			tt.before(f)
			f.clock.Advance(5 * 24 * time.Hour)
			objects := &interposed{LifecycleObjects: f.objects, before: func() { tt.meanwhile(f) }}
			stats, err := f.workerOver(objects, nil).RunOnce(context.Background())
			if err != nil || objects.calls == 0 {
				t.Fatalf("pass = %+v, %v (%d removals attempted)", stats, err, objects.calls)
			}
			if stats.Skipped != 1 || stats.Expired+stats.Noncurrent+stats.Markers != 0 {
				t.Errorf("pass = %+v; want the one removal skipped", stats)
			}
			if got := fmt.Sprint(f.versionsOf(t, "race")); got != tt.want {
				t.Errorf("the bucket holds %s, want %s", got, tt.want)
			}
			// What the client wrote reads.
			if got, _, err := f.get("race", "k", nil); err != nil || string(got) != "k" {
				t.Errorf("the object written meanwhile: %q, %v", got, err)
			}
		})
	}
}

// TestLifecycleWhereVersioningIsSuspended: the null version as current, as
// an older version, and as a lone marker.
func TestLifecycleWhereVersioningIsSuspended(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "sus", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	enable := func(state bucket.Versioning) {
		t.Helper()
		if err := f.meta.SetBucketVersioning(ctx, "sus", state); err != nil {
			t.Fatal(err)
		}
	}
	// "old": a null version under a versioned one. "cur": only a null
	// version. "gone": a null marker and nothing else.
	f.putIn(t, "sus", "old", object.Lock{})
	enable(bucket.VersioningEnabled)
	f.clock.Advance(time.Hour)
	f.putIn(t, "sus", "old", object.Lock{})
	enable(bucket.VersioningSuspended)
	f.putIn(t, "sus", "cur", object.Lock{})
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "sus", Key: "gone"}); err != nil {
		t.Fatal(err)
	}
	f.setRules(t, "sus",
		bucket.LifecycleRule{ID: "old", Enabled: true, NoncurrentDays: 1},
		bucket.LifecycleRule{ID: "markers", Enabled: true, ExpiredObjectDeleteMarker: true},
	)
	f.clock.Advance(5 * 24 * time.Hour)
	stats, err := f.lifecycleWorker(nil).RunOnce(ctx)
	if err != nil || stats.Noncurrent != 1 || stats.Markers != 1 || stats.Expired != 0 || stats.Skipped+stats.Failed != 0 {
		t.Fatalf("pass = %+v, %v; want the older null version and the lone marker removed", stats, err)
	}
	if got := fmt.Sprint(f.versionsOf(t, "sus")); got != "[cur:version,latest old:version,latest]" {
		t.Errorf("after the pass: %s", got)
	}
	// Expiring the current null version replaces it with a null marker,
	// which is then alone and goes on the pass after.
	f.setRules(t, "sus",
		bucket.LifecycleRule{ID: "expire", Enabled: true, Prefix: "cur", ExpirationDays: 1},
		bucket.LifecycleRule{ID: "markers", Enabled: true, ExpiredObjectDeleteMarker: true},
	)
	if stats, err := f.lifecycleWorker(nil).RunOnce(ctx); err != nil || stats.Expired != 1 {
		t.Fatalf("expiring pass = %+v, %v", stats, err)
	}
	if got := fmt.Sprint(f.versionsOf(t, "sus")); got != "[cur:marker,latest old:version,latest]" {
		t.Errorf("after expiring: %s", got)
	}
	if stats, err := f.lifecycleWorker(nil).RunOnce(ctx); err != nil || stats.Markers != 1 {
		t.Fatalf("marker pass = %+v, %v", stats, err)
	}
	if got := fmt.Sprint(f.versionsOf(t, "sus")); got != "[old:version,latest]" || f.shardCount() != sixDrives {
		t.Errorf("at the end: %s, %d shards", got, f.shardCount())
	}
}

// TestLifecycleOfAKeyWithManyVersions: a key with more old versions than
// two batches of the walk loses them all in one pass.
func TestLifecycleOfAKeyWithManyVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "ver", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := f.meta.SetBucketVersioning(ctx, "ver", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	f.setRules(t, "ver", bucket.LifecycleRule{Enabled: true, NoncurrentDays: 1})
	for range 1201 {
		f.putIn(t, "ver", "busy", object.Lock{})
	}
	f.putIn(t, "ver", "z-after", object.Lock{})
	f.clock.Advance(5 * 24 * time.Hour)
	stats, err := f.lifecycleWorker(nil).RunOnce(ctx)
	if err != nil || stats.Noncurrent != 1200 || stats.Failed+stats.Skipped != 0 {
		t.Fatalf("pass = %+v, %v; want all 1200 older versions removed at once", stats, err)
	}
	if got := fmt.Sprint(f.versionsOf(t, "ver")); got != "[busy:version,latest z-after:version,latest]" {
		t.Errorf("after the pass: %s", got)
	}
}

// TestLifecyclePassesThatDoNotEnd: a removal that fails is counted and the
// pass goes on; a node that stops leading stops removing; and what a pass
// did before it stopped is reported all the same.
func TestLifecyclePassesThatDoNotEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := erasureFixture(t)
	f.setRules(t, "b", bucket.LifecycleRule{Enabled: true, ExpirationDays: 1})
	for _, key := range []string{"a", "b", "c"} {
		f.putIn(t, "b", key, object.Lock{})
	}
	f.clock.Advance(5 * 24 * time.Hour)

	failing := &interposed{LifecycleObjects: f.objects, fail: domain.ErrOutcomeUnknown}
	stats, err := f.workerOver(failing, nil).RunOnce(ctx)
	if err != nil || stats.Failed != 3 || stats.Expired != 0 || failing.calls != 3 {
		t.Fatalf("a pass whose removals fail = %+v, %v after %d attempts", stats, err, failing.calls)
	}
	if len(f.versionsOf(t, "b")) != 3 {
		t.Error("a failed removal removed something")
	}

	// Leading for one removal, and then not.
	stepping := &interposed{LifecycleObjects: f.objects}
	var seen []app.LifecycleStats
	worker := app.NewLifecycleWorker(app.LifecycleWorkerConfig{
		Buckets: f.meta, Index: f.meta, Uploads: f.meta, Objects: stepping, Clock: f.clock, Logger: slog.New(slog.DiscardHandler),
		Active: func() bool { return stepping.calls == 0 },
		OnPass: func(s app.LifecycleStats) { seen = append(seen, s) },
	})
	runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	worker.Run(runCtx, time.Hour)
	if got := len(f.versionsOf(t, "b")); got != 2 {
		t.Errorf("%d objects left; a node that stopped leading after one removal should have removed one", got)
	}
	if len(seen) != 1 || seen[0].Expired != 1 {
		t.Errorf("what the interrupted pass did was reported as %+v", seen)
	}
}
