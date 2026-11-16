package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The deployments of the tests of replication: the source of the replica
// bucket, and another.
const (
	replicaSource = "site-a-0123456789abcdef"
	otherSource   = "site-c"
)

// replicaTime is when the versions the source sends were made there: long
// before the fixture's clock, so that a copy that kept the time of its
// arrival would be seen.
var replicaTime = time.Date(2025, 3, 4, 5, 6, 7, 891011121, time.UTC)

// replicaFixture is a bucket "b" that is the replica of replicaSource, a
// bucket "plain" that keeps versions and is no replica, and an object
// service and a replication service that write to one audit trail.
type replicaFixture struct {
	*fixture
	trail       *porttest.MemIAM
	auditor     *app.Auditor
	replication *app.ReplicationService
}

func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	ctx := context.Background()
	f := &replicaFixture{fixture: newFixture(t), trail: porttest.NewMemIAM()}
	f.auditor = app.NewAuditor(f.trail, f.clock, slog.New(slog.NewJSONHandler(f.logs, nil)))
	f.meta.Audit = f.trail
	f.objects = f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Auditor = f.auditor })
	f.replication = app.NewReplicationService(app.ReplicationServiceConfig{Buckets: f.meta, Store: f.meta, Authz: f.authz, Auditor: f.auditor})
	for _, name := range []string{"b", "plain"} {
		f.createBucket(t, name)
		f.versioning(t, name, bucket.VersioningEnabled)
	}
	if _, err := f.replication.SetReplicaMode(ctx, porttest.Root(), "b", replicaSource); err != nil {
		t.Fatalf("SetReplicaMode: %v", err)
	}
	return f
}

// records returns the audit trail, with what was queued written first.
func (f *replicaFixture) records(t *testing.T) []audit.Record {
	t.Helper()
	f.auditor.Flush(context.Background())
	records, err := f.trail.ListAudit(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// replicator is the key a source holds: the replicate actions by name,
// and nothing else.
func replicator() iam.Principal {
	return allowing(string(iam.ActionReplicateObject), string(iam.ActionReplicateDelete))
}

// copyOf is what a source says of a version: its ID, its sequence, its time
// and its entity tag.
func copyOf(t *testing.T, seq uint64, etag string) app.ReplicaWrite {
	t.Helper()
	id, err := object.NewID(bytes.NewReader(randomBytes(t, object.IDSize)))
	if err != nil {
		t.Fatal(err)
	}
	return app.ReplicaWrite{Source: replicaSource, Version: object.VersionRef{ID: id}, Seq: seq, ModifiedAt: replicaTime, ETag: etag}
}

// putCopy sends a copy of data as the version w.
func (f *replicaFixture) putCopy(p iam.Principal, bucketName, key string, data []byte, w app.ReplicaWrite, change func(*app.PutInput)) (object.Version, error) {
	in := app.PutInput{
		Bucket: bucketName, Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: "text/plain",
		Verify: func() (string, error) { return partTag(data), nil }, Replica: &w,
	}
	if change != nil {
		change(&in)
	}
	return f.objects.Put(context.Background(), p, in)
}

// versionsOf returns the versions of a key, newest first.
func (f *replicaFixture) versionsOf(t *testing.T, bucketName, key string) []object.Version {
	t.Helper()
	page, err := f.objects.ListVersions(context.Background(), porttest.Root(), bucketName, app.VersionListOptions{Prefix: key, MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var out []object.Version
	for _, listed := range page.Versions {
		if listed.Version.Key == key {
			out = append(out, listed.Version)
		}
	}
	return out
}

// TestReplicaMode: a bucket is made a replica and promoted through the use
// case, with its audit record in the entry; what refuses it refuses before
// anything is recorded.
func TestReplicaMode(t *testing.T) {
	t.Parallel()
	root := porttest.Root()
	tests := []struct {
		name   string
		setup  func(t *testing.T, f *replicaFixture)
		bucket string
		source string
		want   error
		// mode is the source the bucket has afterwards.
		mode string
		// recorded says that the change is in the trail.
		recorded bool
	}{
		{name: "made a replica", bucket: "plain", source: otherSource, mode: otherSource, recorded: true},
		{name: "the source it has", bucket: "b", source: replicaSource, mode: replicaSource, recorded: true},
		{name: "promoted", bucket: "b", source: "", mode: "", recorded: true},
		{name: "a bucket that is no replica promoted", bucket: "plain", source: "", mode: "", recorded: true},
		{name: "a source that is no deployment ID", bucket: "plain", source: "site a", want: domain.ErrInvalidArgument},
		{name: "a source too long", bucket: "plain", source: string(bytes.Repeat([]byte("a"), object.MaxDeploymentIDLength+1)), want: domain.ErrInvalidArgument},
		{name: "no such bucket", bucket: "missing", source: otherSource, want: domain.ErrBucketNotFound},
		{name: "a bucket that keeps no versions", bucket: "unversioned", source: otherSource, want: domain.ErrInvalidBucketState, setup: func(t *testing.T, f *replicaFixture) {
			f.createBucket(t, "unversioned")
		}},
		{name: "a bucket that holds a version", bucket: "plain", source: otherSource, want: domain.ErrBucketNotEmpty, setup: func(t *testing.T, f *replicaFixture) {
			f.put(t, "plain", "k", []byte("mine"))
		}},
		{name: "another source while it holds a copy", bucket: "b", source: otherSource, want: domain.ErrBucketNotEmpty, setup: func(t *testing.T, f *replicaFixture) {
			if _, err := f.putCopy(replicator(), "b", "k", []byte("copy"), copyOf(t, 7, partTag([]byte("copy"))), nil); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newReplicaFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			before := len(f.records(t))
			mode, err := f.replication.SetReplicaMode(t.Context(), root, tt.bucket, tt.source)
			if !errors.Is(err, tt.want) {
				t.Fatalf("SetReplicaMode = %+v, %v; want %v", mode, err, tt.want)
			}
			records := f.records(t)
			if tt.want != nil {
				if len(records) != before {
					t.Errorf("a refused change is in the trail: %+v", records[before:])
				}
				return
			}
			if mode.Source != tt.mode || mode.Replica() != (tt.mode != "") {
				t.Errorf("SetReplicaMode = %+v, want the source %q", mode, tt.mode)
			}
			if read, err := f.replication.ReplicaMode(t.Context(), root, tt.bucket); err != nil || read != mode {
				t.Errorf("ReplicaMode = %+v, %v; want %+v", read, err, mode)
			}
			if got := len(records) - before; got != 1 {
				t.Fatalf("%d records of the change, want 1", got)
			}
			last := records[len(records)-1]
			if last.Action != string(iam.ActionPutBucketReplicaMode) || last.Resource != "arn:aws:s3:::"+tt.bucket || last.Outcome != audit.Succeeded {
				t.Errorf("the record of the change = %+v", last)
			}
			if want := map[string]string{"source": tt.source}; tt.source != "" && last.Details["source"] != tt.source {
				t.Errorf("the record's details = %v, want %v", last.Details, want)
			}
			if tt.source == "" && last.Details["promoted"] != "true" {
				t.Errorf("the record of a promotion = %v", last.Details)
			}
		})
	}

	t.Run("without an audit trail nothing is changed", func(t *testing.T) {
		t.Parallel()
		f := newReplicaFixture(t)
		blind := app.NewReplicationService(app.ReplicationServiceConfig{Buckets: f.meta, Store: f.meta, Authz: f.authz})
		if _, err := blind.SetReplicaMode(t.Context(), root, "b", ""); !errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("SetReplicaMode without an auditor = %v", err)
		}
		if mode, err := f.replication.ReplicaMode(t.Context(), root, "b"); err != nil || mode.Source != replicaSource {
			t.Errorf("ReplicaMode = %+v, %v", mode, err)
		}
		if _, err := f.replication.ReplicaMode(t.Context(), root, "missing"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("ReplicaMode of a bucket that is not there = %v", err)
		}
	})
}

// TestReplicaModeIsAuthorized: reading the mode needs
// admin:GetBucketReplicaMode, changing it admin:PutBucketReplicaMode - the
// bucket's own administrator holds neither - and every refusal is in the
// trail, with no change beside it.
func TestReplicaModeIsAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	f.meta.Audit = f.store
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := f.meta.SetBucketVersioning(ctx, "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	replication := app.NewReplicationService(app.ReplicationServiceConfig{Buckets: f.meta, Store: f.meta, Authz: f.authz, Auditor: f.auditor})
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
		{"get", iam.ActionGetBucketReplicaMode, func(p iam.Principal) error {
			_, err := replication.ReplicaMode(ctx, p, "photos")
			return err
		}, []string{iam.RoleClusterAdmin, iam.RoleAuditor}},
		{"set", iam.ActionPutBucketReplicaMode, func(p iam.Principal) error {
			_, err := replication.SetReplicaMode(ctx, p, "photos", replicaSource)
			if err == nil {
				// Promoted again for the next role.
				_, err = replication.SetReplicaMode(ctx, f.root, "photos", "")
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
		}
	}
	if b, err := f.meta.GetBucket(ctx, "photos"); err != nil || b.ReplicaFrom != "" {
		t.Errorf("the bucket after the refusals = replica of %q, %v", b.ReplicaFrom, err)
	}
}

// TestReplicaPut: a copy is committed as the version its source has - its
// ID, its time, its sequence - only by who holds s3:ReplicateObject, only
// into the replica of its source, and only when its content is the
// version's; each refusal stores nothing.
func TestReplicaPut(t *testing.T) {
	t.Parallel()
	data := []byte("the content of a version at the source")
	etag := partTag(data)
	tests := []struct {
		name      string
		principal iam.Principal
		bucket    string
		change    func(w *app.ReplicaWrite, in *app.PutInput)
		want      error
	}{
		{name: "a copy", principal: replicator(), bucket: "b"},
		{name: "the root identity", principal: porttest.Root(), bucket: "b"},
		{name: "s3:PutObject is not s3:ReplicateObject", principal: allowing("s3:PutObject", "s3:PutObjectTagging"), bucket: "b", want: domain.ErrAccessDenied},
		{name: "s3:* does not grant it", principal: userWith(iam.RoleBucketAdmin), bucket: "b", want: domain.ErrAccessDenied},
		{name: "nor does cluster-admin", principal: userWith(iam.RoleClusterAdmin), bucket: "b", want: domain.ErrAccessDenied},
		{name: "s3:ReplicateDelete is not s3:ReplicateObject", principal: allowing(string(iam.ActionReplicateDelete)), bucket: "b", want: domain.ErrAccessDenied},
		{name: "another source", principal: replicator(), bucket: "b", want: domain.ErrInvalidBucketState, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.Source = otherSource
		}},
		{name: "a bucket that is no replica", principal: replicator(), bucket: "plain", want: domain.ErrInvalidBucketState},
		{name: "no such bucket", principal: replicator(), bucket: "missing", want: domain.ErrBucketNotFound},
		{name: "a source that is no deployment ID", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.Source = "site a"
		}},
		{name: "sequence 0", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.Seq = 0
		}},
		{name: "a sequence beyond 2^62", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.Seq = object.MaxReplicaSeq + 1
		}},
		{name: "no version", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.Version = object.VersionRef{}
		}},
		{name: "no time", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.ModifiedAt = time.Time{}
		}},
		{name: "no entity tag", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.ETag = ""
		}},
		{name: "another content", principal: replicator(), bucket: "b", want: domain.ErrBadDigest, change: func(w *app.ReplicaWrite, _ *app.PutInput) {
			w.ETag = partTag([]byte("another"))
		}},
		{name: "no verification", principal: replicator(), bucket: "b", want: domain.ErrBadDigest, change: func(_ *app.ReplicaWrite, in *app.PutInput) {
			in.Verify = nil
		}},
		{name: "an encryption of its own", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(_ *app.ReplicaWrite, in *app.PutInput) {
			in.SSE = object.SSE{Mode: object.SSEC, CustomerKey: bytes.Repeat([]byte{7}, 32)}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newReplicaFixture(t)
			w := copyOf(t, 42, etag)
			var in *app.PutInput
			got, err := f.putCopy(tt.principal, tt.bucket, "k", data, w, func(p *app.PutInput) {
				if tt.change != nil {
					tt.change(p.Replica, p)
				}
				in = p
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Put = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				// Nothing is stored: no shard, no version, no record.
				if n := f.shardCount(); n != 0 {
					t.Errorf("%d shards left by a refused copy", n)
				}
				if tt.bucket != "missing" {
					if versions := f.versionsOf(t, tt.bucket, "k"); len(versions) != 0 {
						t.Errorf("a refused copy left versions: %+v", versions)
					}
				}
				for _, r := range f.records(t) {
					if r.Outcome == audit.Succeeded && r.Action == string(iam.ActionReplicateObject) {
						t.Errorf("a refused copy is in the trail as made: %+v", r)
					}
				}
				return
			}
			w = *in.Replica
			if got.VersionID != w.Version.ID || !got.ModifiedAt.Equal(replicaTime) || got.Seq != 42 ||
				got.Replication != object.ReplicationReplica || got.ETag != etag || !got.Versioned {
				t.Errorf("the copy = %+v; want the ID, the time and the sequence of its source", got)
			}
			stored, err := f.meta.GetVersion(context.Background(), "b", "k", w.Version)
			if err != nil || stored.VersionID != w.Version.ID || stored.Seq != 42 || stored.Replication != object.ReplicationReplica {
				t.Errorf("the stored copy = %+v, %v", stored, err)
			}
			if body, err := f.getVersion("k", w.Version); err != nil || !bytes.Equal(body, data) {
				t.Errorf("the copy reads %q, %v", body, err)
			}
			var made []audit.Record
			for _, r := range f.records(t) {
				if r.Action == string(iam.ActionReplicateObject) {
					made = append(made, r)
				}
			}
			if len(made) != 1 || made[0].Outcome != audit.Succeeded || made[0].Resource != "arn:aws:s3:::b/k" ||
				made[0].Details["source"] != replicaSource || made[0].Details["version"] != w.Version.String() || made[0].Details["seq"] != "42" {
				t.Errorf("the copy in the trail = %+v", made)
			}
			// The same version again changes nothing: its source reads the
			// conflict as "it is there".
			shards := f.shardCount()
			if _, err := f.putCopy(tt.principal, "b", "k", data, w, nil); !errors.Is(err, domain.ErrConflict) {
				t.Errorf("the same copy again = %v, want %v", err, domain.ErrConflict)
			}
			if f.shardCount() != shards {
				t.Errorf("a copy sent twice left %d shards, want %d", f.shardCount(), shards)
			}
		})
	}
}

// TestReplicaBucketTakesNothingElse: in a replica bucket an ordinary put, a
// multipart upload and a delete that would make a marker are refused for
// everybody, the root identity too; reading and removing a version are as
// everywhere.
func TestReplicaBucketTakesNothingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplicaFixture(t)
	root := porttest.Root()
	data := []byte("copy")
	w := copyOf(t, 3, partTag(data))
	if _, err := f.putCopy(replicator(), "b", "k", data, w, nil); err != nil {
		t.Fatal(err)
	}
	shards := f.shardCount()
	refusals := map[string]error{}
	_, refusals["a put"] = f.objects.Put(ctx, root, app.PutInput{Bucket: "b", Key: "k", Body: bytes.NewReader(data), Size: 4, Verify: func() (string, error) { return "e", nil }})
	_, refusals["an upload"] = f.objects.CreateUpload(ctx, root, app.CreateUploadInput{Bucket: "b", Key: "k"})
	_, refusals["a delete"] = f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k"})
	for name, err := range refusals {
		if !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("%s into a replica bucket = %v, want %v", name, err, domain.ErrInvalidBucketState)
		}
	}
	if f.shardCount() != shards {
		t.Errorf("the refusals left %d shards, want %d", f.shardCount(), shards)
	}
	if versions := f.versionsOf(t, "b", "k"); len(versions) != 1 || versions[0].VersionID != w.Version.ID {
		t.Errorf("the versions of the key = %+v", versions)
	}
	// Removing the copy for good is judged as everywhere.
	if out, err := f.objects.Delete(ctx, root, app.DeleteInput{Bucket: "b", Key: "k", Version: &w.Version}); err != nil || out.Removed == nil {
		t.Errorf("removing the copy = %+v, %v", out, err)
	}
	// Once promoted, the bucket takes writes again, newer than any copy.
	if _, err := f.putCopy(replicator(), "b", "k", data, copyOf(t, 1<<40, partTag(data)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.replication.SetReplicaMode(ctx, root, "b", ""); err != nil {
		t.Fatal(err)
	}
	mine := f.put(t, "b", "k", []byte("mine"))
	if current, err := f.meta.GetObject(ctx, "b", "k"); err != nil || current.VersionID != mine.VersionID {
		t.Errorf("after promotion the current version = %+v, %v; want the local put", current.Ref(), err)
	}
	if _, err := f.putCopy(replicator(), "b", "k", data, copyOf(t, 9, partTag(data)), nil); !errors.Is(err, domain.ErrInvalidBucketState) {
		t.Errorf("a copy into a promoted bucket = %v", err)
	}
}

// TestReplicaOrder: copies are stored at their source's sequence, so the
// current version of a key is the one of the greatest, whatever order they
// arrive in; a null version is stored as the null version.
func TestReplicaOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplicaFixture(t)
	newer, older := []byte("newer"), []byte("older")
	wNewer, wOlder := copyOf(t, 20, partTag(newer)), copyOf(t, 10, partTag(older))
	for _, c := range []struct {
		data []byte
		w    app.ReplicaWrite
	}{{newer, wNewer}, {older, wOlder}} {
		if _, err := f.putCopy(replicator(), "b", "k", c.data, c.w, nil); err != nil {
			t.Fatal(err)
		}
	}
	if current, err := f.meta.GetObject(ctx, "b", "k"); err != nil || current.VersionID != wNewer.Version.ID {
		t.Errorf("the current version = %+v, %v; want the one of the greater sequence", current.Ref(), err)
	}
	null := []byte("from before versions")
	wNull := copyOf(t, 1, partTag(null))
	wNull.Version = object.VersionRef{Null: true}
	got, err := f.putCopy(replicator(), "b", "k", null, wNull, nil)
	if err != nil || got.Versioned || got.Seq != 1 {
		t.Fatalf("the null copy = %+v, %v", got, err)
	}
	if body, err := f.getVersion("k", object.VersionRef{Null: true}); err != nil || !bytes.Equal(body, null) {
		t.Errorf("the null version reads %q, %v", body, err)
	}
	if versions := f.versionsOf(t, "b", "k"); len(versions) != 3 || versions[0].VersionID != wNewer.Version.ID || versions[2].Versioned {
		t.Errorf("the versions, newest first = %+v", versions)
	}
}

// TestReplicaDelete: the copy of a delete marker is made with the ID, the
// time and the sequence of its source by who holds s3:ReplicateDelete, and
// removes nothing; each refusal leaves the key as it was.
func TestReplicaDelete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		principal iam.Principal
		bucket    string
		change    func(in *app.DeleteInput)
		want      error
	}{
		{name: "a marker", principal: replicator(), bucket: "b"},
		{name: "the null marker", principal: replicator(), bucket: "b", change: func(in *app.DeleteInput) {
			in.Replica.Version = object.VersionRef{Null: true}
		}},
		{name: "s3:DeleteObject is not s3:ReplicateDelete", principal: allowing("s3:DeleteObject"), bucket: "b", want: domain.ErrAccessDenied},
		{name: "s3:ReplicateObject is not s3:ReplicateDelete", principal: allowing(string(iam.ActionReplicateObject)), bucket: "b", want: domain.ErrAccessDenied},
		{name: "s3:* does not grant it", principal: userWith(iam.RoleBucketAdmin), bucket: "b", want: domain.ErrAccessDenied},
		{name: "a version named", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(in *app.DeleteInput) {
			in.Version = &object.VersionRef{Null: true}
		}},
		{name: "a retention lifted", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(in *app.DeleteInput) {
			in.BypassGovernance = true
		}},
		{name: "an entity tag", principal: replicator(), bucket: "b", want: domain.ErrInvalidArgument, change: func(in *app.DeleteInput) {
			in.Replica.ETag = partTag([]byte("x"))
		}},
		{name: "another source", principal: replicator(), bucket: "b", want: domain.ErrInvalidBucketState, change: func(in *app.DeleteInput) {
			in.Replica.Source = otherSource
		}},
		{name: "a bucket that is no replica", principal: replicator(), bucket: "plain", want: domain.ErrInvalidBucketState},
		{name: "a key that is none", principal: replicator(), bucket: "b", want: domain.ErrInvalidObjectKey, change: func(in *app.DeleteInput) {
			in.Key = ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newReplicaFixture(t)
			data := []byte("copy")
			kept := copyOf(t, 5, partTag(data))
			if _, err := f.putCopy(replicator(), "b", "k", data, kept, nil); err != nil {
				t.Fatal(err)
			}
			if tt.bucket == "plain" {
				f.put(t, "plain", "k", data)
			}
			w := copyOf(t, 6, "")
			in := app.DeleteInput{Bucket: tt.bucket, Key: "k", Replica: &w}
			if tt.change != nil {
				tt.change(&in)
			}
			out, err := f.objects.Delete(t.Context(), tt.principal, in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Delete = %+v, %v; want %v", out, err, tt.want)
			}
			versions := f.versionsOf(t, tt.bucket, "k")
			if tt.want != nil {
				if len(versions) != 1 {
					t.Errorf("a refused marker changed the key: %+v", versions)
				}
				return
			}
			marker := out.Marker
			if marker == nil || !marker.DeleteMarker || out.Removed != nil || marker.Seq != 6 || !marker.ModifiedAt.Equal(replicaTime) ||
				marker.Replication != object.ReplicationReplica || marker.VersionID.IsZero() {
				t.Fatalf("Delete = %+v; want a marker at the source's sequence and time", out)
			}
			if !w.Version.Null && marker.VersionID != w.Version.ID {
				t.Errorf("the marker has the ID %s, want its source's %s", marker.VersionID, w.Version.ID)
			}
			if w.Version.Null == marker.Versioned {
				t.Errorf("the marker is versioned %t; the source's null version is %t", marker.Versioned, w.Version.Null)
			}
			if len(versions) != 2 || !versions[0].DeleteMarker || versions[1].VersionID != kept.Version.ID {
				t.Errorf("the versions after the marker = %+v", versions)
			}
			made := 0
			for _, r := range f.records(t) {
				if r.Action == string(iam.ActionReplicateDelete) && r.Outcome == audit.Succeeded && r.Details["operation"] == "DeleteObject" {
					made++
				}
			}
			if made != 1 {
				t.Errorf("%d records of the marker, want 1", made)
			}
		})
	}
}

// replicaUpload begins the multipart upload of a copy in "b" as p.
func (f *replicaFixture) replicaUpload(t *testing.T, p iam.Principal) object.Upload {
	t.Helper()
	upload, err := f.objects.CreateUpload(context.Background(), p, app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: replicaSource})
	if err != nil {
		t.Fatalf("CreateUpload of a copy: %v", err)
	}
	return upload
}

// partOf sends one part of an upload as p.
func (f *replicaFixture) partOf(p iam.Principal, u object.Upload, data []byte) (object.Part, error) {
	return f.objects.UploadPart(context.Background(), p, app.UploadPartInput{
		Bucket: u.Bucket, Key: u.Key, UploadID: u.ID, Number: 1, Body: bytes.NewReader(data), Size: int64(len(data)),
		Verify: func() (string, error) { return partTag(data), nil }, PayloadSigned: true,
	})
}

// TestReplicaUpload: a version of parts is sent as a multipart upload that
// is begun, sent and completed with s3:ReplicateObject, into the replica of
// its source only, and completed as the version its source has.
func TestReplicaUpload(t *testing.T) {
	t.Parallel()
	data := []byte("the only part")
	const etag = "0123456789abcdef0123456789abcdef-1"

	t.Run("begun", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name      string
			principal iam.Principal
			in        app.CreateUploadInput
			want      error
		}{
			{"a copy", replicator(), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: replicaSource}, nil},
			{"s3:PutObject is not s3:ReplicateObject", allowing("s3:PutObject"), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: replicaSource}, domain.ErrAccessDenied},
			{"s3:* does not grant it", userWith(iam.RoleBucketAdmin), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: replicaSource}, domain.ErrAccessDenied},
			{"another source", replicator(), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: otherSource}, domain.ErrInvalidBucketState},
			{"a source that is none", replicator(), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: "site a"}, domain.ErrInvalidArgument},
			{"a bucket that is no replica", replicator(), app.CreateUploadInput{Bucket: "plain", Key: "large", ReplicaFrom: replicaSource}, domain.ErrInvalidBucketState},
			{"an encryption of its own", replicator(), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: replicaSource, SSE: object.SSE{Mode: object.SSEKMS}}, domain.ErrInvalidArgument},
		}
		for _, tt := range tests {
			f := newReplicaFixture(t)
			upload, err := f.objects.CreateUpload(t.Context(), tt.principal, tt.in)
			if !errors.Is(err, tt.want) {
				t.Errorf("%s: CreateUpload = %v, want %v", tt.name, err, tt.want)
				continue
			}
			if tt.want == nil && upload.ReplicaFrom != replicaSource {
				t.Errorf("%s: the upload is from %q", tt.name, upload.ReplicaFrom)
			}
		}
	})

	t.Run("parts", func(t *testing.T) {
		t.Parallel()
		f := newReplicaFixture(t)
		copied := f.replicaUpload(t, replicator())
		ordinary, err := f.objects.CreateUpload(t.Context(), porttest.Root(), app.CreateUploadInput{Bucket: "plain", Key: "large"})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name      string
			principal iam.Principal
			upload    object.Upload
			want      error
		}{
			{"the source's part of its copy", replicator(), copied, nil},
			{"the root identity's part of a copy", porttest.Root(), copied, nil},
			{"the root identity's part of an ordinary upload", porttest.Root(), ordinary, nil},
			{"a writer's part of an ordinary upload", allowing("s3:PutObject"), ordinary, nil},
			{"a writer's part of a copy", allowing("s3:PutObject"), copied, domain.ErrAccessDenied},
			{"the source's part of an ordinary upload", replicator(), ordinary, domain.ErrAccessDenied},
			{"nobody's", allowing("s3:GetObject"), copied, domain.ErrAccessDenied},
		} {
			if _, err := f.partOf(c.principal, c.upload, data); !errors.Is(err, c.want) {
				t.Errorf("%s = %v, want %v", c.name, err, c.want)
			}
		}

		// A part of a copy whose payload the signature does not cover:
		// refused, and the refusal is in the trail.
		_, err = f.objects.UploadPart(t.Context(), replicator(), app.UploadPartInput{
			Bucket: copied.Bucket, Key: copied.Key, UploadID: copied.ID, Number: 2, Body: bytes.NewReader(data), Size: int64(len(data)),
			Verify: func() (string, error) { return partTag(data), nil },
		})
		if !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("an unsigned part of a copy = %v, want %v", err, domain.ErrAccessDenied)
		}
		denied := false
		for _, r := range f.records(t) {
			denied = denied || (r.Outcome == audit.Denied && r.Details["reason"] == "ReplicaPayloadNotSigned")
		}
		if !denied {
			t.Error("the unsigned part of a copy is not in the trail")
		}
		// An ordinary part need not be signed.
		if _, err := f.objects.UploadPart(t.Context(), porttest.Root(), app.UploadPartInput{
			Bucket: ordinary.Bucket, Key: ordinary.Key, UploadID: ordinary.ID, Number: 2, Body: bytes.NewReader(data), Size: int64(len(data)),
			Verify: func() (string, error) { return partTag(data), nil },
		}); err != nil {
			t.Errorf("an unsigned ordinary part = %v", err)
		}
		// A copy of another object is never a part of a copy, signed or
		// not; into an ordinary upload it is.
		f.put(t, "plain", "source", data)
		shards := f.shardCount()
		copyPart := func(u object.Upload) error {
			_, err := f.objects.CopyPart(t.Context(), porttest.Root(), app.CopyPartInput{
				Source: app.GetInput{Bucket: "plain", Key: "source"},
				Part:   app.UploadPartInput{Bucket: u.Bucket, Key: u.Key, UploadID: u.ID, Number: 3, PayloadSigned: true},
				Digest: tagged,
			})
			return err
		}
		if err := copyPart(copied); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a copied part of a copy = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if f.shardCount() != shards {
			t.Errorf("%d shards after a refused copied part, want %d", f.shardCount(), shards)
		}
		if err := copyPart(ordinary); err != nil {
			t.Errorf("a copied part of an ordinary upload = %v", err)
		}

		// An ordinary upload begun before its bucket became a replica takes
		// no part after.
		f.createBucket(t, "late")
		f.versioning(t, "late", bucket.VersioningEnabled)
		before, err := f.objects.CreateUpload(t.Context(), porttest.Root(), app.CreateUploadInput{Bucket: "late", Key: "large"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.replication.SetReplicaMode(t.Context(), porttest.Root(), "late", replicaSource); err != nil {
			t.Fatal(err)
		}
		if _, err := f.partOf(porttest.Root(), before, data); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("a part of an ordinary upload in a bucket made a replica since = %v, want %v", err, domain.ErrInvalidBucketState)
		}
	})

	t.Run("aborted and listed", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name      string
			principal iam.Principal
			replica   bool
			want      error
		}{
			{"the source's copy, by the source", replicator(), true, nil},
			{"a copy, by the root identity", porttest.Root(), true, nil},
			{"a copy, by who may abort and list uploads", allowing("s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"), true, domain.ErrAccessDenied},
			{"a copy, by the bucket's administrator", userWith(iam.RoleBucketAdmin), true, domain.ErrAccessDenied},
			{"an ordinary upload, by who may abort and list uploads", allowing("s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"), false, nil},
			{"an ordinary upload, by the source", replicator(), false, domain.ErrAccessDenied},
		}
		for _, tt := range tests {
			f := newReplicaFixture(t)
			upload, err := f.objects.CreateUpload(t.Context(), porttest.Root(), app.CreateUploadInput{Bucket: "plain", Key: "large"})
			if tt.replica {
				upload, err = f.objects.CreateUpload(t.Context(), replicator(), app.CreateUploadInput{Bucket: "b", Key: "large", ReplicaFrom: replicaSource})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.partOf(porttest.Root(), upload, data); err != nil {
				t.Fatal(err)
			}
			ref := app.UploadRef{Bucket: upload.Bucket, Key: upload.Key, UploadID: upload.ID}
			listed, err := f.objects.ListParts(t.Context(), tt.principal, app.ListPartsInput{UploadRef: ref, Max: 10})
			if !errors.Is(err, tt.want) || (tt.want == nil && len(listed.Parts) != 1) {
				t.Errorf("%s: ListParts = %d parts, %v; want %v", tt.name, len(listed.Parts), err, tt.want)
			}
			if err := f.objects.AbortUpload(t.Context(), tt.principal, ref); !errors.Is(err, tt.want) {
				t.Errorf("%s: AbortUpload = %v, want %v", tt.name, err, tt.want)
			}
			_, err = f.meta.GetUpload(t.Context(), upload.Bucket, upload.Key, upload.ID)
			if gone := errors.Is(err, domain.ErrUploadNotFound); gone != (tt.want == nil) {
				t.Errorf("%s: the upload is gone %t after the abort", tt.name, gone)
			}
		}
	})

	t.Run("completed", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name      string
			principal iam.Principal
			change    func(t *testing.T, f *replicaFixture, in *app.CompleteUploadInput)
			want      error
		}{
			{name: "a copy", principal: replicator()},
			{name: "s3:PutObject is not s3:ReplicateObject", principal: allowing("s3:PutObject"), want: domain.ErrAccessDenied},
			{name: "another content", principal: replicator(), want: domain.ErrBadDigest, change: func(_ *testing.T, _ *replicaFixture, in *app.CompleteUploadInput) {
				in.ETag = "fedcba9876543210fedcba9876543210-1"
			}},
			{name: "not as a copy", principal: porttest.Root(), want: domain.ErrInvalidBucketState, change: func(_ *testing.T, _ *replicaFixture, in *app.CompleteUploadInput) {
				in.Replica = nil
			}},
			{name: "the copy of another source", principal: replicator(), want: domain.ErrInvalidBucketState, change: func(_ *testing.T, _ *replicaFixture, in *app.CompleteUploadInput) {
				in.Replica.Source = otherSource
			}},
			{name: "an upload that is no copy", principal: porttest.Root(), want: domain.ErrInvalidBucketState, change: func(t *testing.T, f *replicaFixture, in *app.CompleteUploadInput) {
				ordinary, err := f.objects.CreateUpload(t.Context(), porttest.Root(), app.CreateUploadInput{Bucket: "plain", Key: "large"})
				if err != nil {
					t.Fatal(err)
				}
				in.Bucket, in.UploadID = "plain", ordinary.ID
			}},
		}
		for _, tt := range tests {
			f := newReplicaFixture(t)
			upload := f.replicaUpload(t, replicator())
			part, err := f.partOf(replicator(), upload, data)
			if err != nil {
				t.Fatal(err)
			}
			w := copyOf(t, 77, etag)
			in := app.CompleteUploadInput{Bucket: "b", Key: "large", UploadID: upload.ID, Parts: listed(part), ETag: etag, Replica: &w}
			if tt.change != nil {
				tt.change(t, f, &in)
			}
			got, err := f.objects.CompleteUpload(t.Context(), tt.principal, in)
			if !errors.Is(err, tt.want) {
				t.Errorf("%s: CompleteUpload = %v, want %v", tt.name, err, tt.want)
				continue
			}
			if tt.want != nil {
				if versions := f.versionsOf(t, "b", "large"); len(versions) != 0 {
					t.Errorf("%s: a refused completion made %+v", tt.name, versions)
				}
				continue
			}
			if got.VersionID != w.Version.ID || got.Seq != 77 || !got.ModifiedAt.Equal(replicaTime) || got.Replication != object.ReplicationReplica || got.ETag != etag {
				t.Errorf("%s: the copy = %+v", tt.name, got)
			}
			// Sent again, by a source that did not hear the answer: the
			// version it made.
			again, err := f.objects.CompleteUpload(t.Context(), tt.principal, in)
			if err != nil || again.VersionID != got.VersionID {
				t.Errorf("%s: the completion again = %+v, %v", tt.name, again.Ref(), err)
			}
			// Sent again, but naming another version under the same ID:
			// the one there is not answered as if it were.
			for _, retry := range []struct {
				name   string
				change func(w *app.ReplicaWrite, in *app.CompleteUploadInput)
				want   error
			}{
				{"another entity tag", func(w *app.ReplicaWrite, in *app.CompleteUploadInput) {
					w.ETag, in.ETag = "fedcba9876543210fedcba9876543210-1", "fedcba9876543210fedcba9876543210-1"
				}, domain.ErrBadDigest},
				{"another sequence", func(w *app.ReplicaWrite, _ *app.CompleteUploadInput) { w.Seq = 78 }, domain.ErrConflict},
				{"another time", func(w *app.ReplicaWrite, _ *app.CompleteUploadInput) { w.ModifiedAt = w.ModifiedAt.Add(time.Second) }, domain.ErrConflict},
				{"another upload", func(_ *app.ReplicaWrite, in *app.CompleteUploadInput) {
					in.UploadID = newVersionIDFor(t)
				}, domain.ErrConflict},
			} {
				w2, in2 := *in.Replica, in
				in2.Replica = &w2
				retry.change(&w2, &in2)
				if v, err := f.objects.CompleteUpload(t.Context(), tt.principal, in2); !errors.Is(err, retry.want) {
					t.Errorf("%s: the completion again with %s = %+v, %v; want %v", tt.name, retry.name, v.Ref(), err, retry.want)
				}
			}
			// Another upload of the same version: it is there, which the
			// source reads as done.
			second := f.replicaUpload(t, replicator())
			part, err = f.partOf(replicator(), second, data)
			if err != nil {
				t.Fatal(err)
			}
			in.UploadID, in.Parts = second.ID, listed(part)
			if _, err := f.objects.CompleteUpload(t.Context(), tt.principal, in); !errors.Is(err, domain.ErrConflict) {
				t.Errorf("%s: a second copy of the version = %v, want %v", tt.name, err, domain.ErrConflict)
			}
		}
	})
}

// newVersionIDFor returns a random ID, as of an upload or a version.
func newVersionIDFor(t *testing.T) object.ID {
	t.Helper()
	id, err := object.NewID(bytes.NewReader(randomBytes(t, object.IDSize)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestReplicaQuestionsAreAuthorized: each question of a source is judged as
// s3:ReplicateObject and refused unless the bucket is the replica of exactly
// that source - a table of refusals for each of the four - and the actions
// an ordinary read, listing or abort needs are neither asked nor enough.
func TestReplicaQuestionsAreAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplicaFixture(t)
	upload := f.replicaUpload(t, replicator())
	type ask func(p iam.Principal, bucketName, source string) error
	questions := map[string]ask{
		"ReplicaBucket": func(p iam.Principal, bucketName, source string) error {
			_, err := f.objects.ReplicaBucket(ctx, p, bucketName, source)
			return err
		},
		"ReplicaVersion": func(p iam.Principal, bucketName, source string) error {
			_, err := f.objects.ReplicaVersion(ctx, p, app.ReplicaVersionInput{Bucket: bucketName, Key: "large", Version: object.VersionRef{ID: upload.ID}, Source: source})
			if errors.Is(err, domain.ErrVersionNotFound) || errors.Is(err, domain.ErrObjectNotFound) {
				// Admitted: there is no such version.
				return nil
			}
			return err
		},
		"ListReplicaUploads": func(p iam.Principal, bucketName, source string) error {
			_, err := f.objects.ListReplicaUploads(ctx, p, app.ReplicaUploadsInput{Bucket: bucketName, Key: "large", Source: source, Max: 10})
			return err
		},
		"AbortReplicaUpload": func(p iam.Principal, bucketName, source string) error {
			err := f.objects.AbortReplicaUpload(ctx, p, app.UploadRef{Bucket: bucketName, Key: "large", UploadID: newVersionIDFor(t)}, source)
			if errors.Is(err, domain.ErrUploadNotFound) {
				// Admitted: there is no such upload.
				return nil
			}
			return err
		},
	}
	ordinary := allowing(string(iam.ActionGetObjectVersion), string(iam.ActionGetObject), string(iam.ActionListBucket),
		string(iam.ActionGetBucketVersioning), string(iam.ActionListBucketMultipartUploads), string(iam.ActionAbortMultipartUpload))
	tests := []struct {
		name      string
		principal iam.Principal
		bucket    string
		source    string
		want      error
	}{
		{"the replicate key", replicator(), "b", replicaSource, nil},
		{"the root identity", porttest.Root(), "b", replicaSource, nil},
		{"the ordinary actions", ordinary, "b", replicaSource, domain.ErrAccessDenied},
		{"s3:*", allowing("s3:*"), "b", replicaSource, domain.ErrAccessDenied},
		{"a source that is none", replicator(), "b", "site a", domain.ErrInvalidArgument},
		{"no source", replicator(), "b", "", domain.ErrInvalidArgument},
		{"a bucket that is no replica", replicator(), "plain", replicaSource, domain.ErrInvalidBucketState},
		{"the replica of another source", replicator(), "b", otherSource, domain.ErrInvalidBucketState},
		{"a bucket that is not there", replicator(), "missing", replicaSource, domain.ErrBucketNotFound},
	}
	for name, q := range questions {
		for _, tt := range tests {
			if err := q(tt.principal, tt.bucket, tt.source); !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("%s, %s = %v, want %v", name, tt.name, err, tt.want)
			}
		}
	}
	// The upload asked about with the wrong source is still there. (That
	// each denial is in the trail is the authorizer's; the S3 adapter's
	// tests see it there.)
	if _, err := f.meta.GetUpload(ctx, "b", "large", upload.ID); err != nil {
		t.Errorf("the upload: %v", err)
	}
}

// TestReplicaQuestions: what each question answers - the versioning of the
// replica; the entity tag of a version and nothing else, a marker as
// ErrMethodNotAllowed, a version that is not there as not found; the uploads
// of exactly the key that the source began, page by page; the abort of such
// an upload only.
func TestReplicaQuestions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplicaFixture(t)
	p := replicator()

	if state, err := f.objects.ReplicaBucket(ctx, p, "b", replicaSource); err != nil || state != bucket.VersioningEnabled {
		t.Errorf("ReplicaBucket = %q, %v", state, err)
	}

	data := []byte("a version")
	w := copyOf(t, 3, partTag(data))
	if _, err := f.putCopy(p, "b", "k", data, w, nil); err != nil {
		t.Fatal(err)
	}
	marker := copyOf(t, 4, "")
	if _, err := f.objects.Delete(ctx, p, app.DeleteInput{Bucket: "b", Key: "k", Replica: &marker}); err != nil {
		t.Fatal(err)
	}
	notFound := func(err error) bool {
		return errors.Is(err, domain.ErrVersionNotFound) || errors.Is(err, domain.ErrObjectNotFound)
	}
	versions := []struct {
		name    string
		key     string
		version object.VersionRef
		etag    string
		want    func(error) bool
	}{
		{"a version", "k", w.Version, partTag(data), func(err error) bool { return err == nil }},
		{"a marker", "k", marker.Version, "", func(err error) bool { return errors.Is(err, domain.ErrMethodNotAllowed) }},
		{"a version that is not there", "k", object.VersionRef{ID: newVersionIDFor(t)}, "", notFound},
		{"a null version that is not there", "k", object.VersionRef{Null: true}, "", notFound},
		{"a key that is not there", "none", w.Version, "", notFound},
		{"no version named", "k", object.VersionRef{}, "", func(err error) bool { return errors.Is(err, domain.ErrInvalidArgument) }},
		{"a key that is none", "", w.Version, "", func(err error) bool { return err != nil && !notFound(err) }},
	}
	for _, tt := range versions {
		etag, err := f.objects.ReplicaVersion(ctx, p, app.ReplicaVersionInput{Bucket: "b", Key: tt.key, Version: tt.version, Source: replicaSource})
		if !tt.want(err) || etag != tt.etag {
			t.Errorf("%s = %q, %v; want %q", tt.name, etag, err, tt.etag)
		}
	}

	// Uploads: three of the source for "large", one of another key, and
	// in "mixed" one that is no copy, one of another source and one of the
	// source.
	var mine []object.ID
	for range 3 {
		mine = append(mine, f.replicaUpload(t, p).ID)
	}
	if _, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "b", Key: "large2", ReplicaFrom: replicaSource}); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(mine, func(a, b object.ID) int { return bytes.Compare(a[:], b[:]) })
	f.createBucket(t, "mixed")
	f.versioning(t, "mixed", bucket.VersioningEnabled)
	ordinary, err := f.objects.CreateUpload(ctx, porttest.Root(), app.CreateUploadInput{Bucket: "mixed", Key: "large"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.replication.SetReplicaMode(ctx, porttest.Root(), "mixed", otherSource); err != nil {
		t.Fatal(err)
	}
	another, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "mixed", Key: "large", ReplicaFrom: otherSource})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.replication.SetReplicaMode(ctx, porttest.Root(), "mixed", replicaSource); err != nil {
		t.Fatal(err)
	}
	mixed, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "mixed", Key: "large", ReplicaFrom: replicaSource})
	if err != nil {
		t.Fatal(err)
	}

	ids := func(out app.ReplicaUploadsOutput) []object.ID {
		var got []object.ID
		for _, u := range out.Uploads {
			if u.Key != "large" || u.ReplicaFrom != replicaSource {
				t.Errorf("listed %+v", u)
			}
			got = append(got, u.ID)
		}
		return got
	}
	listings := []struct {
		name      string
		bucket    string
		in        app.ReplicaUploadsInput
		want      []object.ID
		truncated bool
	}{
		{"every upload of the key", "b", app.ReplicaUploadsInput{Max: 100}, mine, false},
		{"more than the greatest page", "b", app.ReplicaUploadsInput{Max: object.MaxListKeys + 1}, mine, false},
		{"a first page", "b", app.ReplicaUploadsInput{Max: 2}, mine[:2], true},
		{"the page after", "b", app.ReplicaUploadsInput{Max: 2, KeyMarker: "large", UploadIDMarker: mine[1]}, mine[2:], false},
		{"a marker before the key", "b", app.ReplicaUploadsInput{Max: 100, KeyMarker: "a"}, mine, false},
		{"the key as the marker", "b", app.ReplicaUploadsInput{Max: 100, KeyMarker: "large"}, nil, false},
		{"a marker after the key", "b", app.ReplicaUploadsInput{Max: 100, KeyMarker: "z"}, nil, false},
		{"nothing asked for", "b", app.ReplicaUploadsInput{Max: 0}, nil, false},
		{"only the source's", "mixed", app.ReplicaUploadsInput{Max: 100}, []object.ID{mixed.ID}, false},
	}
	for _, tt := range listings {
		in := tt.in
		in.Bucket, in.Key, in.Source = tt.bucket, "large", replicaSource
		out, err := f.objects.ListReplicaUploads(ctx, p, in)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got := ids(out); !slices.Equal(got, tt.want) || out.Truncated != tt.truncated {
			t.Errorf("%s = %v (truncated %v), want %v (%v)", tt.name, got, out.Truncated, tt.want, tt.truncated)
		}
		if tt.truncated && out.NextUploadIDMarker != tt.want[len(tt.want)-1] {
			t.Errorf("%s: next %v", tt.name, out.NextUploadIDMarker)
		}
	}
	if _, err := f.objects.ListReplicaUploads(ctx, p, app.ReplicaUploadsInput{Bucket: "b", Key: "", Source: replicaSource, Max: 1}); err == nil {
		t.Errorf("a listing of no key = %v", err)
	}

	for _, u := range []object.Upload{ordinary, another} {
		ref := app.UploadRef{Bucket: "mixed", Key: "large", UploadID: u.ID}
		if err := f.objects.AbortReplicaUpload(ctx, p, ref, replicaSource); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("the abort of an upload that is not the source's = %v", err)
		}
		if _, err := f.meta.GetUpload(ctx, "mixed", "large", u.ID); err != nil {
			t.Errorf("an upload that is not the source's was aborted: %v", err)
		}
	}
	ref := app.UploadRef{Bucket: "mixed", Key: "large", UploadID: mixed.ID}
	if err := f.objects.AbortReplicaUpload(ctx, p, ref, replicaSource); err != nil {
		t.Errorf("the abort of the source's upload: %v", err)
	}
	if err := f.objects.AbortReplicaUpload(ctx, p, ref, replicaSource); !errors.Is(err, domain.ErrUploadNotFound) {
		t.Errorf("the abort again = %v", err)
	}
}

// TestReplicaChanges: a later change with a source is taken only for a
// version named by its ID, in the replica of exactly that source, and never
// with a bypass; the action asked is the ordinary one.
func TestReplicaChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplicaFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "locked", CreatedAt: testNow, ObjectLock: true, Versioning: bucket.VersioningEnabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.replication.SetReplicaMode(ctx, porttest.Root(), "locked", replicaSource); err != nil {
		t.Fatal(err)
	}
	data := []byte("locked")
	w := copyOf(t, 1, partTag(data))
	// Far ahead of every clock the store or the use case may judge by.
	until := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.putCopy(porttest.Root(), "locked", "k", data, w, func(in *app.PutInput) {
		in.Lock = object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: until}}
	}); err != nil {
		t.Fatal(err)
	}
	ref := w.Version
	changer := allowing(string(iam.ActionPutObjectTagging), string(iam.ActionPutObjectRetention), string(iam.ActionPutObjectLegalHold),
		string(iam.ActionDeleteObjectTagging), string(iam.ActionOverrideGovernance))
	on := func(bucketName, source string, version *object.VersionRef) app.LockInput {
		return app.LockInput{Bucket: bucketName, Key: "k", Version: version, ReplicaSource: source}
	}
	tests := []struct {
		name   string
		change func(in app.LockInput) error
		in     app.LockInput
		want   error
	}{
		{"tags", func(in app.LockInput) error {
			_, err := f.objects.SetTags(ctx, changer, in, []object.Tag{{Key: "a", Value: "b"}})
			return err
		}, on("locked", replicaSource, &ref), nil},
		{"a legal hold", func(in app.LockInput) error {
			_, err := f.objects.SetLegalHold(ctx, changer, in, true)
			return err
		}, on("locked", replicaSource, &ref), nil},
		{"a retention extended", func(in app.LockInput) error {
			_, err := f.objects.SetRetention(ctx, changer, in, &object.Retention{Mode: object.Compliance, Until: until.Add(time.Hour)}, false)
			return err
		}, on("locked", replicaSource, &ref), nil},
		{"a retention shortened", func(in app.LockInput) error {
			_, err := f.objects.SetRetention(ctx, changer, in, &object.Retention{Mode: object.Compliance, Until: until}, false)
			return err
		}, on("locked", replicaSource, &ref), domain.ErrObjectLocked},
		{"a bypass", func(in app.LockInput) error {
			_, err := f.objects.SetRetention(ctx, changer, in, &object.Retention{Mode: object.Compliance, Until: until.Add(2 * time.Hour)}, true)
			return err
		}, on("locked", replicaSource, &ref), domain.ErrInvalidArgument},
		{"the current version", func(in app.LockInput) error {
			_, err := f.objects.SetTags(ctx, changer, in, []object.Tag{{Key: "c", Value: "d"}})
			return err
		}, on("locked", replicaSource, nil), domain.ErrInvalidArgument},
		{"a source that is none", func(in app.LockInput) error {
			_, err := f.objects.SetTags(ctx, changer, in, []object.Tag{{Key: "c", Value: "d"}})
			return err
		}, on("locked", "site a", &ref), domain.ErrInvalidArgument},
		{"another source", func(in app.LockInput) error {
			_, err := f.objects.SetLegalHold(ctx, changer, in, false)
			return err
		}, on("locked", otherSource, &ref), domain.ErrInvalidBucketState},
		{"a bucket that is no replica", func(in app.LockInput) error {
			_, err := f.objects.SetTags(ctx, changer, in, []object.Tag{{Key: "c", Value: "d"}})
			return err
		}, on("plain", replicaSource, &ref), domain.ErrInvalidBucketState},
		{"the replicate key, which holds no change", func(in app.LockInput) error {
			_, err := f.objects.SetTags(ctx, replicator(), in, []object.Tag{{Key: "c", Value: "d"}})
			return err
		}, on("locked", replicaSource, &ref), domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		err := tt.change(tt.in)
		if (tt.want == nil && err != nil) || !errors.Is(err, tt.want) {
			t.Errorf("%s = %v, want %v", tt.name, err, tt.want)
		}
	}
	stored, err := f.meta.GetVersion(ctx, "locked", "k", ref)
	if err != nil || !stored.Lock.LegalHold || stored.Lock.Retention == nil || !stored.Lock.Retention.Until.Equal(until.Add(time.Hour)) || stored.Tags["a"] != "b" {
		t.Errorf("the version after the changes = %+v, %v", stored, err)
	}
}

// conflictingBuckets is a bucket store whose reads of a bucket conflict.
type conflictingBuckets struct{ ports.BucketStore }

// GetBucket implements ports.BucketStore.
func (conflictingBuckets) GetBucket(context.Context, string) (bucket.Bucket, error) {
	return bucket.Bucket{}, fmt.Errorf("%w: the bucket changed while it was read", domain.ErrConflict)
}

// conflictingIndex is an object index whose reads of a version conflict.
type conflictingIndex struct{ ports.ObjectIndex }

// GetVersion implements ports.ObjectIndex.
func (conflictingIndex) GetVersion(context.Context, string, string, object.VersionRef) (object.Version, error) {
	return object.Version{}, fmt.Errorf("%w: the version changed while it was read", domain.ErrConflict)
}

// TestReplicaQuestionsKeepTheirConflictApart: the one conflict a question
// of HEAD is answered with is ErrInvalidBucketState - the source reads every
// 409 to a HEAD as a bucket that is no replica of it - so any other conflict
// on the way is ErrUnavailable (503), and never ErrConflict (409).
func TestReplicaQuestionsKeepTheirConflictApart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplicaFixture(t)
	buckets := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Buckets = conflictingBuckets{f.meta} })
	index := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Index = conflictingIndex{f.meta} })
	version := app.ReplicaVersionInput{Bucket: "b", Key: "k", Version: object.VersionRef{ID: newVersionIDFor(t)}, Source: replicaSource}
	bucketProbe := func(s *app.ObjectService, bucketName string) error {
		_, err := s.ReplicaBucket(ctx, replicator(), bucketName, replicaSource)
		return err
	}
	versionProbe := func(s *app.ObjectService, bucketName string) error {
		in := version
		in.Bucket = bucketName
		_, err := s.ReplicaVersion(ctx, replicator(), in)
		return err
	}
	tests := []struct {
		name   string
		ask    func(*app.ObjectService, string) error
		svc    *app.ObjectService
		bucket string
		want   error
	}{
		{"the bucket probe, a bucket read that conflicts", bucketProbe, buckets, "b", domain.ErrUnavailable},
		{"a version, a bucket read that conflicts", versionProbe, buckets, "b", domain.ErrUnavailable},
		{"a version, a version read that conflicts", versionProbe, index, "b", domain.ErrUnavailable},
		{"the bucket probe of a bucket that is no replica", bucketProbe, f.objects, "plain", domain.ErrInvalidBucketState},
		{"a version in a bucket that is no replica", versionProbe, f.objects, "plain", domain.ErrInvalidBucketState},
	}
	for _, tt := range tests {
		err := tt.ask(tt.svc, tt.bucket)
		if !errors.Is(err, tt.want) || (errors.Is(tt.want, domain.ErrUnavailable) && errors.Is(err, domain.ErrConflict)) {
			t.Errorf("%s = %v, want %v and no conflict", tt.name, err, tt.want)
		}
	}
}
