package adminapi_test

import (
	"bytes"
	"net/http"
	"reflect"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// resyncCall is the start of a bucket's resync: a POST without a body, which
// says its media type all the same.
func resyncCall(bucketName, bearer string, headers map[string]string) call {
	h := map[string]string{"Content-Type": "application/json"}
	for name, value := range headers {
		h[name] = value
	}
	return call{method: "POST", path: "/buckets/" + bucketName + "/replication/resync", bearer: bearer, headers: h}
}

// TestBucketReplicationResync: a resync started through the management API
// (PLAN 8.1.6). Who may is the use case's to decide - admin:ResyncReplication,
// which a bucket's own administrator does not hold; it answers 202 with the
// record as stored, running from the beginning, also over one that had come
// some way; a bucket without a configuration is 404; the start is safe to
// repeat and in the trail.
func TestBucketReplicationResync(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	reader := f.userKey("reader", iam.RoleReadOnly)
	for _, name := range []string{"photos", "plain"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
		if err := f.meta.SetBucketVersioning(t.Context(), name, bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/targets/site-b", body: siteTarget(t, "photos", "plain")})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/replication", body: map[string]any{
		"target": "site-b", "destination_bucket": "copy",
		"rules": []any{map[string]any{"id": "all", "enabled": true, "priority": 0, "prefix": "", "delete_markers": false}},
	}})
	// A resync that had come some way.
	if err := f.meta.PutResync(t.Context(), bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "k", Version: "null", Scanned: 7, Sent: 2}); err != nil {
		t.Fatal(err)
	}
	started := map[string]any{"state": "running", "scanned": float64(0), "sent": float64(0)}
	for _, step := range []struct {
		name   string
		c      call
		status int
		want   map[string]any
		code   string
		replay bool
	}{
		{name: "started", c: resyncCall("photos", f.root, withKey("resync-1")), status: http.StatusAccepted, want: started, replay: true},
		{name: "started again", c: resyncCall("photos", f.root, nil), status: http.StatusAccepted, want: started},
		{name: "by the bucket's administrator", c: resyncCall("photos", owner, nil), status: http.StatusForbidden, code: "AccessDenied"},
		{name: "by a reader", c: resyncCall("photos", reader, nil), status: http.StatusForbidden, code: "AccessDenied"},
		{name: "without a credential", c: resyncCall("photos", "AKIDNOBODY:not-a-secret", nil), status: http.StatusUnauthorized, code: "Unauthenticated"},
		{name: "a bucket without a configuration", c: resyncCall("plain", f.root, nil), status: http.StatusNotFound, code: "NoSuchEntity"},
		{name: "no such bucket", c: resyncCall("missing", f.root, nil), status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "without its media type", c: call{method: "POST", path: "/buckets/photos/replication/resync", bearer: f.root}, status: http.StatusUnsupportedMediaType},
	} {
		r := f.do(step.c)
		if r.status != step.status {
			t.Fatalf("%s: %s %s = %d, want %d: %s", step.name, step.c.method, step.c.path, r.status, step.status, r.body)
		}
		switch {
		case step.code != "":
			if p := r.problem(t); p["code"] != step.code {
				t.Errorf("%s: problem = %s, want code %s", step.name, r.body, step.code)
			}
		case step.want != nil:
			if got := answerOf(t, r); !reflect.DeepEqual(got, step.want) {
				t.Errorf("%s: answered %v, want %v", step.name, got, step.want)
			}
		}
		if step.replay {
			again := f.do(step.c)
			if again.status != r.status || again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, r.body) {
				t.Errorf("%s: the repeat = %d, replayed %q: %s", step.name, again.status, again.header.Get("Idempotent-Replayed"), again.body)
			}
		}
	}
	if got, err := f.meta.GetResync(t.Context(), "photos"); err != nil || got != (bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}) {
		t.Errorf("the stored record = %+v, %v", got, err)
	}
	starts, denials := 0, 0
	for _, r := range f.trail() {
		if r.Action != string(iam.ActionResyncReplication) {
			continue
		}
		switch {
		case r.Outcome == audit.Succeeded && r.Actor == "root" && r.Resource == "arn:aws:s3:::photos" && r.Details["target"] == "site-b":
			starts++
		case r.Outcome == audit.Denied && (r.Actor == "user/owner" || r.Actor == "user/reader"):
			denials++
		}
	}
	// Two starts the store was given; the repeat was answered from the
	// record.
	if starts != 2 || denials != 2 {
		t.Errorf("the trail holds %d starts and %d denials, want 2 and 2", starts, denials)
	}
}

// TestResyncOnANodeWithoutTheUseCase: a node that was not given the use case
// answers 501.
func TestResyncOnANodeWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.Replication = nil })
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	if p := f.must(http.StatusNotImplemented, resyncCall("photos", f.root, nil)).problem(t); p["code"] != "NotImplemented" {
		t.Errorf("a resync on a node without the use case: problem = %v", p)
	}
}
