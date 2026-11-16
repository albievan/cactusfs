package adminapi_test

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestBucketNotifications: the notification rules of a bucket read, set and
// removed through the management API (ADR-0025). Who may is the use case's
// to decide - s3:GetBucketNotification and s3:PutBucketNotification, which
// a bucket's administrator holds and a reader does not; a rule names a
// target the administrator opened to the bucket, and which of the ways a
// target fails is not told; every refusal is a problem; a change is safe to
// repeat; each change is in the trail.
func TestBucketNotifications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	reader := f.userKey("reader", iam.RoleReadOnly)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for name, target := range map[string]map[string]any{
		"hook":      {"kind": "webhook", "endpoint": "https://hooks.example.com", "buckets": []string{"photos"}},
		"bus":       {"kind": "nats", "endpoint": "tls://nats.example.com", "subject": "cactusfs.events", "buckets": []string{"p*"}},
		"elsewhere": {"kind": "webhook", "endpoint": "https://hooks.example.com", "buckets": []string{"logs"}},
	} {
		f.must(http.StatusOK, call{method: "PUT", path: "/targets/" + name, body: target})
	}

	path := "/buckets/photos/notifications"
	rules := []any{
		map[string]any{"id": "uploads", "target": "hook", "events": []string{"complete", "put"}, "prefix": "in/", "suffix": ".jpg"},
		map[string]any{"target": "bus", "events": []string{"delete", "marker"}, "as": "topic"},
		map[string]any{"target": "hook", "events": []string{"put"}, "prefix": ""},
	}
	// As kept: the ID given to the rule without one, a queue where none
	// was said, the kinds of change in their one order, an empty prefix
	// left out - and the rules in the order they were given.
	kept := map[string]any{"rules": []any{
		map[string]any{"id": "uploads", "as": "queue", "target": "hook", "events": []any{"put", "complete"}, "prefix": "in/", "suffix": ".jpg"},
		map[string]any{"id": "rule-1", "as": "topic", "target": "bus", "events": []any{"marker", "delete"}},
		map[string]any{"id": "rule-2", "as": "queue", "target": "hook", "events": []any{"put"}},
	}}
	none := map[string]any{"rules": []any{}}
	for _, step := range []struct {
		name   string
		c      call
		status int
		// want is the answer of a success; code that of a problem.
		want map[string]any
		code string
		// replay says that the call carries an idempotency key and its
		// repeat is answered from the record.
		replay bool
	}{
		{name: "no rules yet", c: call{method: "GET", path: path}, status: http.StatusOK, want: none},
		{name: "set", c: call{method: "PUT", path: path, body: map[string]any{"rules": rules}, headers: withKey("set-1")}, status: http.StatusOK, want: kept, replay: true},
		{name: "read back", c: call{method: "GET", path: path}, status: http.StatusOK, want: kept},
		{name: "read by the bucket's administrator", c: call{method: "GET", path: path, bearer: owner}, status: http.StatusOK, want: kept},
		{name: "read by a reader", c: call{method: "GET", path: path, bearer: reader}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "set by a reader", c: call{method: "PUT", path: path, bearer: reader, body: map[string]any{"rules": []any{}}}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "removed by a reader", c: call{method: "DELETE", path: path, bearer: reader}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "a target that is not there", c: call{method: "PUT", path: path, body: map[string]any{"rules": []any{map[string]any{"id": "a", "target": "nowhere", "events": []string{"put"}}}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a target closed to the bucket", c: call{method: "PUT", path: path, body: map[string]any{"rules": []any{map[string]any{"id": "a", "target": "elsewhere", "events": []string{"put"}}}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a kind of change that is none", c: call{method: "PUT", path: path, body: map[string]any{"rules": []any{map[string]any{"id": "a", "target": "hook", "events": []string{"copy"}}}}}, status: http.StatusBadRequest},
		{name: "an ID twice", c: call{method: "PUT", path: path, body: map[string]any{"rules": []any{
			map[string]any{"id": "a", "target": "hook", "events": []string{"put"}}, map[string]any{"id": "a", "target": "bus", "events": []string{"put"}},
		}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "no body", c: call{method: "PUT", path: path, headers: map[string]string{"Content-Type": "application/json"}}, status: http.StatusBadRequest},
		{name: "a bucket that is not there", c: call{method: "PUT", path: "/buckets/nowhere/notifications", body: map[string]any{"rules": rules}}, status: http.StatusNotFound},
		{name: "rules of a bucket that is not there", c: call{method: "GET", path: "/buckets/nowhere/notifications"}, status: http.StatusNotFound},
		{name: "not changed by the refusals", c: call{method: "GET", path: path}, status: http.StatusOK, want: kept},
		{name: "set again by the bucket's administrator", c: call{method: "PUT", path: path, bearer: owner, body: map[string]any{"rules": rules}}, status: http.StatusOK, want: kept},
		{name: "removed", c: call{method: "DELETE", path: path, headers: withKey("remove-1")}, status: http.StatusNoContent, replay: true},
		{name: "none after", c: call{method: "GET", path: path}, status: http.StatusOK, want: none},
		{name: "removed when there are none", c: call{method: "DELETE", path: path}, status: http.StatusNoContent},
		{name: "set once more", c: call{method: "PUT", path: path, body: map[string]any{"rules": rules}}, status: http.StatusOK, want: kept},
		{name: "removed by no rules", c: call{method: "PUT", path: path, body: map[string]any{"rules": []any{}}}, status: http.StatusOK, want: none},
		{name: "none at the end", c: call{method: "GET", path: path}, status: http.StatusOK, want: none},
	} {
		if step.c.bearer == "" {
			step.c.bearer = f.root
		}
		r := f.do(step.c)
		if r.status != step.status {
			t.Fatalf("%s: %s %s = %d, want %d: %s", step.name, step.c.method, step.c.path, r.status, step.status, r.body)
		}
		switch {
		case step.want != nil:
			if got := answerOf(t, r); !reflect.DeepEqual(got, step.want) {
				t.Errorf("%s: answered\n%v\nwant\n%v", step.name, got, step.want)
			}
		case r.status >= http.StatusBadRequest:
			p := r.problem(t)
			if step.code != "" && p["code"] != step.code {
				t.Errorf("%s: problem %s, want code %s", step.name, r.body, step.code)
			}
			// Which way a target fails, and where any is, is not told.
			for _, word := range []string{"hooks.example", "nats.example", "logs"} {
				if strings.Contains(string(r.body), word) {
					t.Errorf("%s: the problem tells %q: %s", step.name, word, r.body)
				}
			}
		}
		if step.replay {
			again := f.do(step.c)
			if again.status != r.status || !bytes.Equal(again.body, r.body) || again.header.Get("Idempotent-Replayed") != "true" {
				t.Errorf("%s: the repeat was answered %d %s, replayed %q", step.name, again.status, again.body, again.header.Get("Idempotent-Replayed"))
			}
		}
	}

	// Each change once in the trail, the repeats not again; each denial
	// under the action asked.
	var changes []string
	denials := map[string]int{}
	for _, record := range f.trail() {
		switch {
		case record.Action == string(iam.ActionPutBucketNotification) && record.Outcome == audit.Succeeded:
			changes = append(changes, record.Details["rules"]+record.Details["notification"])
		case record.Outcome == audit.Denied && strings.HasPrefix(record.Action, "s3:") && strings.HasSuffix(record.Action, "BucketNotification"):
			denials[record.Action]++
		}
	}
	if want := []string{"3", "3", "removed", "removed", "3", "removed"}; !reflect.DeepEqual(changes, want) {
		t.Errorf("the trail holds the changes %v, want %v", changes, want)
	}
	if want := map[string]int{string(iam.ActionGetBucketNotification): 1, string(iam.ActionPutBucketNotification): 2}; !reflect.DeepEqual(denials, want) {
		t.Errorf("the trail holds the denials %v, want %v", denials, want)
	}
}

// TestBucketNotificationsWithoutTheUseCase: a node that was not given the
// use case answers 501 to a caller it has authenticated, and 401 to one it
// has not.
func TestBucketNotificationsWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.Notifications = nil })
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for _, c := range []call{
		{method: "GET", path: "/buckets/photos/notifications"},
		{method: "PUT", path: "/buckets/photos/notifications", body: map[string]any{"rules": []any{}}},
		{method: "DELETE", path: "/buckets/photos/notifications"},
	} {
		if r := f.do(c); r.status != http.StatusUnauthorized {
			t.Errorf("%s %s without a credential = %d, want 401", c.method, c.path, r.status)
		}
		c.bearer = f.root
		r := f.do(c)
		if r.status != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501: %s", c.method, c.path, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "NotImplemented" {
			t.Errorf("%s %s: problem = %s", c.method, c.path, r.body)
		}
	}
}
