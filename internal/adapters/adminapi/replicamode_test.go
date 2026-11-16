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

// TestBucketReplicaMode: whether a bucket is a replica, read and changed
// through the management API (ADR-0026). Who may is the use case's to
// decide, with admin: actions that a bucket's own administrator does not
// hold; every refusal is a problem - 409 InvalidBucketState and
// BucketNotEmpty among them; a change is safe to repeat; and each change,
// and each denial, is in the trail.
func TestBucketReplicaMode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	auditor := f.userKey("auditor", iam.RoleAuditor)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	for _, name := range []string{"copies", "plain", "used"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	for _, name := range []string{"copies", "used"} {
		if err := f.meta.SetBucketVersioning(t.Context(), name, bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/used/object?key=k", raw: []byte("mine")})

	const source = "site-a-0123"
	path := "/buckets/copies/replica"
	notReplica := map[string]any{"replica": false}
	replica := map[string]any{"replica": true, "source": source}
	for _, step := range []struct {
		name   string
		c      call
		status int
		want   map[string]any
		code   string
		replay bool
	}{
		{name: "no replica", c: call{method: "GET", path: path}, status: http.StatusOK, want: notReplica},
		{
			name: "made a replica", c: call{method: "PUT", path: path, body: map[string]any{"source": source}, headers: withKey("replica-1")},
			status: http.StatusOK, want: replica, replay: true,
		},
		{name: "read by an auditor", c: call{method: "GET", path: path, bearer: auditor}, status: http.StatusOK, want: replica},
		{name: "the same source again", c: call{method: "PUT", path: path, body: map[string]any{"source": source}}, status: http.StatusOK, want: replica},
		{name: "read by the bucket's administrator", c: call{method: "GET", path: path, bearer: owner}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "promoted by the bucket's administrator", c: call{method: "PUT", path: path, bearer: owner, body: map[string]any{"source": ""}}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "changed by an auditor", c: call{method: "PUT", path: path, bearer: auditor, body: map[string]any{"source": ""}}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "read without a credential", c: call{method: "GET", path: path, bearer: "AKIDNOBODY:not-a-secret"}, status: http.StatusUnauthorized, code: "Unauthenticated"},
		{name: "changed without a credential", c: call{method: "PUT", path: path, bearer: "AKIDNOBODY:not-a-secret", body: map[string]any{"source": ""}}, status: http.StatusUnauthorized, code: "Unauthenticated"},
		{name: "a source that is no deployment ID", c: call{method: "PUT", path: "/buckets/used/replica", body: map[string]any{"source": "site a"}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "no body", c: call{method: "PUT", path: path, headers: map[string]string{"Content-Type": "application/json"}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a bucket that keeps no versions", c: call{method: "PUT", path: "/buckets/plain/replica", body: map[string]any{"source": source}}, status: http.StatusConflict, code: "InvalidBucketState"},
		{
			name: "a bucket that holds a version", c: call{method: "PUT", path: "/buckets/used/replica", body: map[string]any{"source": source}, headers: withKey("used-1")},
			status: http.StatusConflict, code: "BucketNotEmpty", replay: true,
		},
		{name: "read of no such bucket", c: call{method: "GET", path: "/buckets/missing/replica"}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "set on no such bucket", c: call{method: "PUT", path: "/buckets/missing/replica", body: map[string]any{"source": source}}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{
			name: "promoted", c: call{method: "PUT", path: path, body: map[string]any{"source": ""}, headers: withKey("promote-1")},
			status: http.StatusOK, want: notReplica, replay: true,
		},
		{name: "no replica after the promotion", c: call{method: "GET", path: path, bearer: auditor}, status: http.StatusOK, want: notReplica},
	} {
		if step.c.bearer == "" {
			step.c.bearer = f.root
		}
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

	// Each change is in the trail, made in the entry that made it; each
	// refusal of the authorizer too, and no change beside it.
	changes, denials := 0, 0
	for _, r := range f.trail() {
		if r.Action != string(iam.ActionPutBucketReplicaMode) && r.Action != string(iam.ActionGetBucketReplicaMode) {
			continue
		}
		switch {
		case r.Outcome == audit.Succeeded && r.Actor == "root" && r.Resource == "arn:aws:s3:::copies":
			changes++
		case r.Outcome == audit.Denied && (r.Actor == "user/owner" || r.Actor == "user/auditor"):
			denials++
		case r.Outcome == audit.Succeeded:
			t.Errorf("a change by another, or of another bucket: %+v", r)
		}
	}
	// Made a replica, the same source again, promoted: three changes the
	// store was given; the repeats were answered from the record.
	if changes != 3 || denials != 3 {
		t.Errorf("the trail holds %d changes and %d denials of replica mode, want 3 and 3", changes, denials)
	}
}

// TestReplicaModeOnANodeWithoutTheUseCase: a node that was not given the
// use case answers its operations 501.
func TestReplicaModeOnANodeWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.Replication = nil })
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for _, c := range []call{
		{method: "GET", path: "/buckets/photos/replica"},
		{method: "PUT", path: "/buckets/photos/replica", body: map[string]any{"source": ""}},
	} {
		if p := f.must(http.StatusNotImplemented, c).problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
			t.Errorf("%s %s on a node without the use case: problem = %v", c.method, c.path, p)
		}
	}
}
