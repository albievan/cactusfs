package adminapi_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestBucketReplicationStatus: GET /buckets/{bucket}/replication/status
// (PLAN 8.1.7) answers, to who may read it - admin:GetReplicationStatus,
// which an auditor holds and a bucket's own administrator does not - the
// counts of the bucket's usage record, what the node that leads says of the
// target (a time and a class that are none left out, and only words where a
// word belongs), the rows the target lost and whether it needs a resync from
// its cursor - not from the leader, which may be of an older release - and
// the stored record of the resync; a leader of an older
// release makes the state "unsupported"; a bucket without a configuration is
// 404, no leader 503, no credential 401.
func TestBucketReplicationStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	auditor := f.userKey("auditor", iam.RoleAuditor)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	reader := f.userKey("reader", iam.RoleReadOnly)
	for _, name := range []string{"photos", "plain"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
		if err := f.meta.SetBucketVersioning(ctx, name, bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/targets/site-b", body: siteTarget(t, "photos", "plain")})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/replication", body: map[string]any{
		"target": "site-b", "destination_bucket": "copy",
		"rules": []any{map[string]any{"id": "all", "enabled": true, "priority": 0, "prefix": "", "delete_markers": false}},
	}})
	// Three versions a rule matched - PENDING - one of which the other
	// deployment refused: FAILED. The outbox keeps two rows, so the
	// target loses one and needs a resync: its cursor says so.
	f.meta.MaxOutboxRows = 2
	var versions []object.Version
	for i, key := range []string{"a", "b", "c"} {
		put, err := f.meta.PutVersion(ctx, porttest.SampleVersion("photos", key, byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, put.Stored)
	}
	failed := delivery.Result{Bucket: "photos", Key: "c", Version: versions[2].Ref(), Status: object.ReplicationFailed}
	if _, err := f.meta.AckOutbox(ctx, "site-b", delivery.Seq{}, 0, []delivery.Result{failed}); err != nil {
		t.Fatal(err)
	}
	along := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "a", Version: object.NullVersion, Scanned: 1200, Sent: 7}
	if err := f.meta.PutResync(ctx, along); err != nil {
		t.Fatal(err)
	}
	at := testNow.Add(-time.Hour)
	f.leader.set(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
		{
			Name: "site-b", Kind: delivery.KindS3, State: ports.DeliveryStalled, Pending: 40, OldestPendingAt: at,
			LastErrorClass: delivery.ClassObjectLock, Lost: 3, ResyncNeeded: true,
		},
	}})

	var got map[string]any
	r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/replication/status", bearer: auditor})
	r.decode(t, &got)
	want := map[string]any{
		"target": "site-b", "target_state": "stalled", "last_error_class": "object_lock", "oldest_pending_at": at.Format(time.RFC3339),
		"pending": float64(2), "failed": float64(1), "lost": float64(1), "resync_needed": true,
		"resync": map[string]any{"state": "running", "scanned": float64(1200), "sent": float64(7)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status\n%v\nwant\n%v", got, want)
	}

	// Words where a word belongs that cannot be one, and a leader of an
	// older release.
	f.leader.set(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderAnswering, Statuses: []ports.TargetStatus{
		{Name: "site-b", Kind: delivery.KindS3, State: porttest.DeliveryMark, LastErrorClass: porttest.DeliveryMark},
	}})
	r = f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/replication/status"})
	if strings.Contains(string(r.body), "MARK") {
		t.Errorf("the words of the leader were handed on: %s", r.body)
	}
	f.leader.set(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderOld})
	got = nil
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/replication/status"}).decode(t, &got)
	want = map[string]any{
		"target": "site-b", "target_state": "unsupported", "pending": float64(2), "failed": float64(1), "lost": float64(1),
		"resync_needed": true, "resync": map[string]any{"state": "running", "scanned": float64(1200), "sent": float64(7)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status with an old leader\n%v\nwant\n%v", got, want)
	}

	f.leader.set(porttest.DeliveryStatusScript{Leader: porttest.DeliveryLeaderGone})
	for name, tt := range map[string]struct {
		c      call
		status int
		code   string
	}{
		"no credential":                    {call{method: "GET", path: "/buckets/photos/replication/status", bearer: ""}, http.StatusUnauthorized, "Unauthenticated"},
		"a credential that is none":        {call{method: "GET", path: "/buckets/photos/replication/status", bearer: "AKIDNOBODY:not-a-secret"}, http.StatusUnauthorized, "Unauthenticated"},
		"the bucket's administrator":       {call{method: "GET", path: "/buckets/photos/replication/status", bearer: owner}, http.StatusForbidden, "AccessDenied"},
		"a reader":                         {call{method: "GET", path: "/buckets/photos/replication/status", bearer: reader}, http.StatusForbidden, "AccessDenied"},
		"a bucket without a configuration": {call{method: "GET", path: "/buckets/plain/replication/status", bearer: f.root}, http.StatusNotFound, "NoSuchEntity"},
		"no such bucket":                   {call{method: "GET", path: "/buckets/missing/replication/status", bearer: f.root}, http.StatusNotFound, "NoSuchBucket"},
		"no leader":                        {call{method: "GET", path: "/buckets/photos/replication/status", bearer: f.root}, http.StatusServiceUnavailable, "ServiceUnavailable"},
	} {
		r := f.do(tt.c)
		if r.status != tt.status {
			t.Errorf("%s: %d, want %d: %s", name, r.status, tt.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != tt.code {
			t.Errorf("%s: problem = %s, want code %s", name, r.body, tt.code)
		}
	}
}
