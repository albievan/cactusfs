package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// TestReplicationResync: starting a resync needs admin:ResyncReplication on
// the bucket, which of the built-in roles only cluster-admin holds - not a
// bucket-admin, who holds s3:*; a refusal is audited and stores nothing. A
// start stores the record at its beginning, running, for the target the
// configuration names - also over a resync that had come some way, or was
// done - and is audited.
func TestReplicationResync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplConfigFixture(t)
	if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("all", 0, ""))); err != nil {
		t.Fatal(err)
	}
	started := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}
	roles := map[string]string{
		iam.RoleClusterAdmin: "admin-user", iam.RoleBucketAdmin: "bucket-user", iam.RoleReadWrite: "writer-user",
		iam.RoleReadOnly: "reader-user", iam.RoleAuditor: "auditor-user",
	}
	for role, user := range roles {
		// A resync that had been through before.
		along := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncDone, Key: "k", Version: object.NullVersion, Scanned: 9, Sent: 3}
		if err := f.meta.PutResync(ctx, along); err != nil {
			t.Fatal(err)
		}
		got, err := f.replication.Resync(ctx, f.as(t, f.user(t, user, role)), "photos")
		stored, readErr := f.meta.GetResync(ctx, "photos")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if role == iam.RoleClusterAdmin {
			if err != nil || got != started || stored != started {
				t.Errorf("by %s = %+v, %v; stored %+v; want %+v", role, got, err, stored, started)
			}
			continue
		}
		if !errors.Is(err, domain.ErrAccessDenied) || stored != along {
			t.Errorf("by %s = %v, stored %+v; want ErrAccessDenied and nothing stored", role, err, stored)
		}
	}
	var startedBy, denied []string
	for _, r := range f.trail(t) {
		if r.Action != string(iam.ActionResyncReplication) || r.Resource != "arn:aws:s3:::photos" {
			continue
		}
		switch r.Outcome {
		case audit.Succeeded:
			if r.Details["target"] == "site-b" && r.Details["resync"] == "started" {
				startedBy = append(startedBy, r.Actor)
			}
		case audit.Denied:
			denied = append(denied, r.Actor)
		}
	}
	if len(startedBy) != 1 || startedBy[0] != "user/admin-user" || len(denied) != 4 {
		t.Errorf("the trail says started by %v, denied to %v", startedBy, denied)
	}
}

// narrowedTargets reads every target as one open to no bucket of the tests.
type narrowedTargets struct {
	ports.TargetStore
}

func (n narrowedTargets) GetTarget(ctx context.Context, name string) (delivery.Target, error) {
	target, err := n.TargetStore.GetTarget(ctx, name)
	target.Buckets = []string{"elsewhere"}
	return target, err
}

// TestReplicationResyncRefused: what cannot be resynced is refused with the
// sentinel its answer is made from, and nothing is stored.
func TestReplicationResyncRefused(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		bucket string
		// service returns the use cases to ask, after making the
		// deployment so.
		service func(t *testing.T, f *replConfigFixture) *app.ReplicationService
		want    error
	}{
		"a bucket without a configuration": {bucket: "suspended", want: domain.ErrEntityNotFound},
		"a bucket that is not there":       {bucket: "missing", want: domain.ErrBucketNotFound},
		"no audit trail": {bucket: "photos", want: domain.ErrUnavailable, service: func(_ *testing.T, f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{Buckets: f.meta, Store: f.meta, Targets: f.meta, Authz: f.authz})
		}},
		"no targets on the node": {bucket: "photos", want: domain.ErrUnavailable, service: func(_ *testing.T, f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{Buckets: f.meta, Store: f.meta, Authz: f.authz, Auditor: f.auditor})
		}},
		// The store refuses to narrow a target a configuration names: a
		// read of the target as it was a moment before is the only way.
		"a target that no longer admits the bucket": {bucket: "photos", want: domain.ErrInvalidArgument, service: func(_ *testing.T, f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{
				Buckets: f.meta, Store: f.meta, Targets: narrowedTargets{TargetStore: f.meta}, Authz: f.authz, Auditor: f.auditor,
			})
		}},
		"a configuration that cannot be read": {bucket: "photos", want: domain.ErrIntegrity, service: func(_ *testing.T, f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{
				Buckets: failingConfig{BucketStore: f.meta, document: []byte(`{"v":1,"broken"`)}, Store: f.meta, Targets: f.meta, Authz: f.authz, Auditor: f.auditor,
			})
		}},
		"a configuration of a later release": {bucket: "photos", want: domain.ErrUnavailable, service: func(_ *testing.T, f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{
				Buckets: failingConfig{BucketStore: f.meta, document: []byte(`{"v":99}`)}, Store: f.meta, Targets: f.meta, Authz: f.authz, Auditor: f.auditor,
			})
		}},
		"metadata that cannot be read": {bucket: "photos", want: domain.ErrUnavailable, service: func(_ *testing.T, f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{
				Buckets: failingConfig{BucketStore: f.meta}, Store: f.meta, Targets: f.meta, Authz: f.authz, Auditor: f.auditor,
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReplConfigFixture(t)
			ctx := t.Context()
			if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("all", 0, ""))); err != nil {
				t.Fatal(err)
			}
			service := f.replication
			if tt.service != nil {
				service = tt.service(t, f)
			}
			if _, err := service.Resync(ctx, f.root, tt.bucket); !errors.Is(err, tt.want) {
				t.Errorf("Resync = %v, want %v", err, tt.want)
			}
			if got, err := f.meta.GetResync(ctx, "photos"); err != nil || got != (bucket.Resync{}) {
				t.Errorf("a refused resync stored %+v, %v", got, err)
			}
		})
	}
}
