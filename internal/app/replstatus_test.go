package app_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// readLog records the order in which the status of a bucket's replication
// reads the metadata.
type readLog struct {
	mu    sync.Mutex
	reads []string
}

func (l *readLog) add(read string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads = append(l.reads, read)
}

func (l *readLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reads...)
}

// loggedBuckets is a bucket store whose reads of configurations are logged.
type loggedBuckets struct {
	ports.BucketStore
	log *readLog
}

func (b loggedBuckets) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	b.log.add("config")
	return b.BucketStore.GetBucketConfig(ctx, name, kind)
}

// loggedResyncs is a store of resyncs whose reads are logged.
type loggedResyncs struct {
	ports.ReplicationStore
	log *readLog
}

func (s loggedResyncs) GetResync(ctx context.Context, bucketName string) (bucket.Resync, error) {
	s.log.add("resync")
	return s.ReplicationStore.GetResync(ctx, bucketName)
}

// statusUsage says the same usage of every bucket, or fails.
type statusUsage struct {
	ports.BucketUsageStore
	usage bucket.Usage
	err   error
	log   *readLog
}

func (u statusUsage) BucketUsages(_ context.Context, names []string) ([]ports.BucketUsage, error) {
	if u.log != nil {
		u.log.add("usage")
	}
	if u.err != nil {
		return nil, u.err
	}
	out := make([]ports.BucketUsage, len(names))
	for i := range names {
		out[i] = ports.BucketUsage{Usage: u.usage}
	}
	return out, nil
}

// fixedCursors says the same cursor of the target site-b, or fails; its
// reads are logged.
type fixedCursors struct {
	ports.Outbox
	cursor delivery.Cursor
	err    error
	log    *readLog
}

func (c fixedCursors) OutboxCursors(context.Context) (ports.OutboxState, error) {
	if c.log != nil {
		c.log.add("cursors")
	}
	if c.err != nil {
		return ports.OutboxState{}, c.err
	}
	cursor := c.cursor
	cursor.Target = "site-b"
	return ports.OutboxState{Cursors: []delivery.Cursor{{Target: "hook", Lost: 99, ResyncNeeded: true}, cursor}}, nil
}

// statusService returns the use cases of the fixture reading the usage and
// the cursors given and asking the leader given; without cursors they are
// the fixture's store's.
func (f *replConfigFixture) statusService(buckets ports.BucketStore, store ports.ReplicationStore, usage ports.BucketUsageStore, leader ports.DeliveryStatusReader, cursors ...ports.Outbox) *app.ReplicationService {
	var outbox ports.Outbox = f.meta
	if len(cursors) > 0 {
		outbox = cursors[0]
	}
	return app.NewReplicationService(app.ReplicationServiceConfig{
		Buckets: buckets, Store: store, Targets: f.meta, Authz: f.authz, Auditor: f.auditor, Usage: usage, Delivery: leader, Outbox: outbox,
	})
}

// exactUsage is a usage record of a bucket that was counted, with the
// counts of replication given.
func exactUsage(pending, failed int64) bucket.Usage {
	return bucket.Usage{Exact: true, UsageTotals: bucket.UsageTotals{Versions: 9, ReplicationPending: pending, ReplicationFailed: failed}}
}

// TestReplicationStatus: the status of a bucket's replication puts together
// the counts of the bucket's usage record, what the node that leads says of
// its target - only words that are a state or a class - the rows the
// target lost and whether it needs a resync, from its cursor in the
// metadata whatever the leader says, and the record of its resync, the same
// on every node. A leader of an older release makes the state
// "unsupported"; a bucket counted for the first time shows no counts; a
// bucket never resynced shows no resync. The configuration, the resync and
// the cursors are read before the usage, so that the usage, read from this
// node's copy, is never older than they are.
func TestReplicationStatus(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	along := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "k", Version: object.NullVersion, Scanned: 1200, Sent: 7}
	stalled := ports.TargetStatus{
		Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryStalled, Pending: 40, OldestPendingAt: at,
		LastErrorClass: delivery.ClassObjectLock, Lost: 3, ResyncNeeded: true, Failed: 99,
	}
	for name, tt := range map[string]struct {
		leader porttest.DeliveryStatusScript
		usage  bucket.Usage
		cursor delivery.Cursor
		resync *bucket.Resync
		want   app.ReplicationStatus
	}{
		"a stalled target, a resync under way": {
			leader: porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
				stalled, {Name: "site-c", Kind: delivery.KindS3, State: ports.DeliveryRetrying, Pending: 1, LastErrorClass: delivery.ClassTimeout},
			}},
			usage: exactUsage(12, 2), resync: &along, cursor: delivery.Cursor{Lost: 3, ResyncNeeded: true},
			want: app.ReplicationStatus{
				Target: "site-b", TargetState: ports.DeliveryStalled, LastErrorClass: delivery.ClassObjectLock, OldestPendingAt: at,
				Pending: 12, Failed: 2, Lost: 3, ResyncNeeded: true, Resync: &along,
			},
		},
		"nothing waits, never resynced": {
			leader: porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
				{Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryOK},
			}},
			usage: exactUsage(0, 0),
			want:  app.ReplicationStatus{Target: "site-b", TargetState: ports.DeliveryOK},
		},
		"the usage counted for the first time": {
			leader: porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
				{Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryRetrying, LastErrorClass: delivery.ClassConnect},
			}},
			usage: bucket.Usage{Count: &bucket.UsageCount{}},
			want:  app.ReplicationStatus{Target: "site-b", TargetState: ports.DeliveryRetrying, LastErrorClass: delivery.ClassConnect},
		},
		"words where a state and a class belong": {
			leader: porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
				{Name: "site-b", Kind: delivery.KindS3, State: porttest.DeliveryMark, LastErrorClass: porttest.DeliveryMark},
			}},
			usage: exactUsage(1, 0),
			want:  app.ReplicationStatus{Target: "site-b", TargetState: ports.DeliveryUnsupported, LastErrorClass: delivery.ClassInternal, Pending: 1},
		},
		"a leader of an older release": {
			leader: porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderOld},
			usage:  exactUsage(5, 1), resync: &along, cursor: delivery.Cursor{Lost: 4, ResyncNeeded: true},
			want: app.ReplicationStatus{
				Target: "site-b", TargetState: ports.DeliveryUnsupported, Pending: 5, Failed: 1, Lost: 4, ResyncNeeded: true, Resync: &along,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newReplConfigFixture(t)
			if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("all", 0, ""))); err != nil {
				t.Fatal(err)
			}
			if tt.resync != nil {
				if err := f.meta.PutResync(ctx, *tt.resync); err != nil {
					t.Fatal(err)
				}
			}
			log := &readLog{}
			service := f.statusService(loggedBuckets{BucketStore: f.meta, log: log}, loggedResyncs{ReplicationStore: f.meta, log: log},
				statusUsage{usage: tt.usage, log: log}, porttest.ScriptedDeliveryStatusReader(tt.leader), fixedCursors{cursor: tt.cursor, log: log})
			auditor := f.as(t, f.user(t, "auditor-user", iam.RoleAuditor))
			got, err := service.Status(ctx, auditor, "photos")
			if err != nil {
				t.Fatalf("Status = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Status =\n%+v\nwant\n%+v", got, tt.want)
			}
			if want := []string{"config", "resync", "cursors", "usage"}; !reflect.DeepEqual(log.all(), want) {
				t.Errorf("the reads were %v, want %v", log.all(), want)
			}
		})
	}
}

// TestReplicationStatusRefused: the status needs admin:GetReplicationStatus
// on the bucket, which of the built-in roles cluster-admin and auditor hold
// and a bucket's own administrator does not; every refusal is the sentinel
// its answer is made from, and a leader that does not answer in
// app.TargetStatusTimeout is unavailable.
func TestReplicationStatusRefused(t *testing.T) {
	t.Parallel()
	answering := porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
		{Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryOK},
	}}
	for name, tt := range map[string]struct {
		bucket string
		role   string
		// service returns the use cases to ask; nil is the fixture's, with
		// an exact usage and a leader that answers.
		service func(f *replConfigFixture) *app.ReplicationService
		want    error
	}{
		"a bucket's administrator":         {bucket: "photos", role: iam.RoleBucketAdmin, want: domain.ErrAccessDenied},
		"a writer":                         {bucket: "photos", role: iam.RoleReadWrite, want: domain.ErrAccessDenied},
		"a reader":                         {bucket: "photos", role: iam.RoleReadOnly, want: domain.ErrAccessDenied},
		"a bucket without a configuration": {bucket: "suspended", want: domain.ErrEntityNotFound},
		"a bucket that is not there":       {bucket: "missing", want: domain.ErrBucketNotFound},
		"a node that cannot read the usage": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, f.meta, nil, porttest.ScriptedDeliveryStatusReader(answering))
		}},
		"a node that cannot ask the leader": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, f.meta, statusUsage{usage: exactUsage(0, 0)}, nil)
		}},
		"no leader": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, f.meta, statusUsage{usage: exactUsage(0, 0)},
				porttest.ScriptedDeliveryStatusReader(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderGone}))
		}},
		"a leader that does not know the target yet": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, f.meta, statusUsage{usage: exactUsage(0, 0)},
				porttest.ScriptedDeliveryStatusReader(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering}))
		}},
		"a usage that cannot be read": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, f.meta, statusUsage{err: fmt.Errorf("%w: closed", domain.ErrUnavailable)},
				porttest.ScriptedDeliveryStatusReader(answering))
		}},
		"cursors that cannot be read": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, f.meta, statusUsage{usage: exactUsage(0, 0)}, porttest.ScriptedDeliveryStatusReader(answering),
				fixedCursors{err: fmt.Errorf("%w: closed", domain.ErrUnavailable)})
		}},
		"a node that cannot read the cursors": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return app.NewReplicationService(app.ReplicationServiceConfig{
				Buckets: f.meta, Store: f.meta, Targets: f.meta, Authz: f.authz, Auditor: f.auditor,
				Usage: statusUsage{usage: exactUsage(0, 0)}, Delivery: porttest.ScriptedDeliveryStatusReader(answering),
			})
		}},
		"a resync that cannot be read": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(f.meta, failingResyncs{ReplicationStore: f.meta}, statusUsage{usage: exactUsage(0, 0)},
				porttest.ScriptedDeliveryStatusReader(answering))
		}},
		"a configuration that cannot be read": {bucket: "photos", want: domain.ErrIntegrity, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(failingConfig{BucketStore: f.meta, document: []byte(`{"v":1,"broken"`)}, f.meta,
				statusUsage{usage: exactUsage(0, 0)}, porttest.ScriptedDeliveryStatusReader(answering))
		}},
		"a configuration of a later release": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(failingConfig{BucketStore: f.meta, document: []byte(`{"v":99}`)}, f.meta,
				statusUsage{usage: exactUsage(0, 0)}, porttest.ScriptedDeliveryStatusReader(answering))
		}},
		"metadata that cannot be read": {bucket: "photos", want: domain.ErrUnavailable, service: func(f *replConfigFixture) *app.ReplicationService {
			return f.statusService(failingConfig{BucketStore: f.meta}, f.meta, statusUsage{usage: exactUsage(0, 0)},
				porttest.ScriptedDeliveryStatusReader(answering))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newReplConfigFixture(t)
			if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("all", 0, ""))); err != nil {
				t.Fatal(err)
			}
			service := f.statusService(f.meta, f.meta, statusUsage{usage: exactUsage(0, 0)}, porttest.ScriptedDeliveryStatusReader(answering))
			if tt.service != nil {
				service = tt.service(f)
			}
			principal := f.root
			if tt.role != "" {
				principal = f.as(t, f.user(t, "a-"+tt.role, tt.role))
			}
			if _, err := service.Status(ctx, principal, tt.bucket); !errors.Is(err, tt.want) {
				t.Errorf("Status = %v, want %v", err, tt.want)
			}
		})
	}
}

// failingResyncs is a store of resyncs that cannot be read.
type failingResyncs struct {
	ports.ReplicationStore
}

func (failingResyncs) GetResync(context.Context, string) (bucket.Resync, error) {
	return bucket.Resync{}, fmt.Errorf("%w: the metadata cannot be read", domain.ErrUnavailable)
}

// TestReplicationStatusOfASilentLeader: a leader that is reached and never
// answers is unavailable after app.TargetStatusTimeout, not before; and a
// caller that gives up first is told its own reason.
func TestReplicationStatusOfASilentLeader(t *testing.T) {
	t.Parallel()
	f := newReplConfigFixture(t)
	if _, err := f.replication.SetConfiguration(t.Context(), f.root, "photos", replConfig("site-b", replRule("all", 0, ""))); err != nil {
		t.Fatal(err)
	}
	silent := porttest.ScriptedDeliveryStatusReader(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderSilent})
	service := f.statusService(f.meta, f.meta, statusUsage{usage: exactUsage(0, 0)}, silent)
	synctest.Test(t, func(t *testing.T) {
		began := time.Now()
		_, err := service.Status(context.Background(), f.root, "photos")
		if !errors.Is(err, domain.ErrUnavailable) || time.Since(began) != app.TargetStatusTimeout {
			t.Errorf("Status = %v after %v, want ErrUnavailable after %v", err, time.Since(began), app.TargetStatusTimeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := service.Status(ctx, f.root, "photos"); errors.Is(err, domain.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Status of a caller that gave up = %v, want its own deadline", err)
		}
	})
}
