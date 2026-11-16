package adminapi_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// siteTarget is the body of another deployment open to the buckets named,
// with a key made for the run: an access key ID and a secret nobody has.
func siteTarget(t *testing.T, buckets ...string) map[string]any {
	t.Helper()
	raw := make([]byte, 28)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	id, secret := "SITE"+strings.ToUpper(hex.EncodeToString(raw[:8])), hex.EncodeToString(raw[8:])
	return map[string]any{
		"kind": "s3", "endpoint": "https://s3.site-b.example", "region": "eu-west-1", "buckets": buckets,
		"auth": map[string]any{"mode": "sigv4", "id": id, "secret": secret},
	}
}

// TestBucketReplication: the replication configuration of a bucket read,
// set and removed through the management API (ADR-0026). Who may is the use
// case's to decide - s3:GetReplicationConfiguration and
// s3:PutReplicationConfiguration, which a bucket's administrator holds and a
// reader does not; it names another deployment the administrator opened to
// the bucket, and which of the ways a target fails is not told; a bucket
// that keeps no versions is 400 and a replica 409; a bucket without one is
// 404; every refusal is a problem; a change is safe to repeat; each change
// is in the trail.
func TestBucketReplication(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	reader := f.userKey("reader", iam.RoleReadOnly)
	for _, name := range []string{"photos", "plain", "copies"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	for _, name := range []string{"photos", "copies"} {
		if err := f.meta.SetBucketVersioning(t.Context(), name, bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.meta.SetBucketReplica(t.Context(), "copies", "site-z", audit.Record{}); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]map[string]any{
		"site-b":    siteTarget(t, "photos", "plain", "copies"),
		"elsewhere": siteTarget(t, "logs"),
		"hook":      {"kind": "webhook", "endpoint": "https://hooks.example.com", "buckets": []string{"photos"}},
	} {
		f.must(http.StatusOK, call{method: "PUT", path: "/targets/" + name, body: target})
	}

	path := "/buckets/photos/replication"
	put := map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{
		map[string]any{"id": "logs", "enabled": true, "priority": 2, "prefix": "logs/", "delete_markers": true},
		map[string]any{"id": "off", "enabled": false, "priority": 0, "prefix": "", "delete_markers": false},
	}}
	// As kept: an empty prefix left out, the rules in the order given.
	kept := map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{
		map[string]any{"id": "logs", "enabled": true, "priority": float64(2), "prefix": "logs/", "delete_markers": true},
		map[string]any{"id": "off", "enabled": false, "priority": float64(0), "delete_markers": false},
	}}
	withRule := func(rule map[string]any) map[string]any {
		return map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{rule}}
	}
	with := func(target, destination string) map[string]any {
		return map[string]any{"target": target, "destination_bucket": destination, "rules": []any{
			map[string]any{"id": "a", "enabled": true, "priority": 0, "delete_markers": false},
		}}
	}
	for _, step := range []struct {
		name   string
		c      call
		status int
		want   map[string]any
		code   string
		replay bool
	}{
		{name: "none yet", c: call{method: "GET", path: path}, status: http.StatusNotFound, code: "NoSuchEntity"},
		{name: "set", c: call{method: "PUT", path: path, body: put, headers: withKey("set-1")}, status: http.StatusOK, want: kept, replay: true},
		{name: "read back", c: call{method: "GET", path: path}, status: http.StatusOK, want: kept},
		{name: "read by the bucket's administrator", c: call{method: "GET", path: path, bearer: owner}, status: http.StatusOK, want: kept},
		{name: "read by a reader", c: call{method: "GET", path: path, bearer: reader}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "set by a reader", c: call{method: "PUT", path: path, bearer: reader, body: put}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "removed by a reader", c: call{method: "DELETE", path: path, bearer: reader}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "a target that is not there", c: call{method: "PUT", path: path, body: with("nowhere", "copy")}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a target closed to the bucket", c: call{method: "PUT", path: path, body: with("elsewhere", "copy")}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a webhook", c: call{method: "PUT", path: path, body: with("hook", "copy")}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a destination that is no bucket", c: call{method: "PUT", path: path, body: with("site-b", "No_Bucket")}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "no rules", c: call{method: "PUT", path: path, body: map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a bucket that keeps no versions", c: call{method: "PUT", path: "/buckets/plain/replication", body: with("site-b", "copy")}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a replica", c: call{method: "PUT", path: "/buckets/copies/replication", body: with("site-b", "copy")}, status: http.StatusConflict, code: "InvalidBucketState"},
		{name: "no body", c: call{method: "PUT", path: path, headers: map[string]string{"Content-Type": "application/json"}}, status: http.StatusBadRequest},
		// What the contract requires of a rule is given: the generated
		// decoder would take one left out for its zero value.
		{name: "a rule without an id", c: call{method: "PUT", path: path, body: withRule(map[string]any{"enabled": true, "priority": 0, "delete_markers": false})}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a rule of a null id", c: call{method: "PUT", path: path, body: withRule(map[string]any{"id": nil, "enabled": true, "priority": 0, "delete_markers": false})}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a rule without enabled", c: call{method: "PUT", path: path, body: withRule(map[string]any{"id": "a", "priority": 0, "delete_markers": false})}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a rule without a priority", c: call{method: "PUT", path: path, body: withRule(map[string]any{"id": "a", "enabled": true, "delete_markers": false})}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a rule without delete_markers", c: call{method: "PUT", path: path, body: withRule(map[string]any{"id": "a", "enabled": true, "priority": 0})}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a rule with delete_markers in another case and null", c: call{method: "PUT", path: path, body: withRule(map[string]any{"id": "a", "enabled": true, "priority": 0, "delete_markers": true, "Delete_Markers": nil})}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "rules in another case, one without an id", c: call{method: "PUT", path: path, body: map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{}, "Rules": []any{map[string]any{"enabled": true, "priority": 0, "delete_markers": false}}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a rule that is no object", c: call{method: "PUT", path: path, body: map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{"all"}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "two rules that tie", c: call{method: "PUT", path: path, body: map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{
			map[string]any{"id": "a", "enabled": true, "priority": 1, "prefix": "logs/", "delete_markers": false},
			map[string]any{"id": "b", "enabled": true, "priority": 1, "prefix": "logs/2026/", "delete_markers": false},
		}}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "a bucket that is not there", c: call{method: "PUT", path: "/buckets/nowhere/replication", body: put}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "of a bucket that is not there", c: call{method: "GET", path: "/buckets/nowhere/replication"}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "not changed by the refusals", c: call{method: "GET", path: path}, status: http.StatusOK, want: kept},
		{name: "set again by the bucket's administrator", c: call{method: "PUT", path: path, bearer: owner, body: put}, status: http.StatusOK, want: kept},
		{name: "removed", c: call{method: "DELETE", path: path, headers: withKey("remove-1")}, status: http.StatusNoContent, replay: true},
		{name: "none after", c: call{method: "GET", path: path}, status: http.StatusNotFound, code: "NoSuchEntity"},
		{name: "removed when there is none", c: call{method: "DELETE", path: path}, status: http.StatusNoContent},
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
			for _, word := range []string{"example", "logs", "webhook", "SITE"} {
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
		case record.Action == string(iam.ActionPutReplication) && record.Outcome == audit.Succeeded:
			changes = append(changes, record.Details["target"]+record.Details["rules"]+record.Details["replication"])
		case record.Outcome == audit.Denied && strings.HasSuffix(record.Action, "ReplicationConfiguration"):
			denials[record.Action]++
		}
	}
	if want := []string{"site-b2", "site-b2", "removed", "removed"}; !reflect.DeepEqual(changes, want) {
		t.Errorf("the trail holds the changes %v, want %v", changes, want)
	}
	if want := map[string]int{string(iam.ActionGetReplication): 1, string(iam.ActionPutReplication): 2}; !reflect.DeepEqual(denials, want) {
		t.Errorf("the trail holds the denials %v, want %v", denials, want)
	}
}

// TestTheRequiredFieldsOfARuleAreTheContracts: what the adapter requires of
// each rule of a replication configuration is what the contract lists as
// required of ReplicationRule, and the operation requires the array itself.
func TestTheRequiredFieldsOfARuleAreTheContracts(t *testing.T) {
	t.Parallel()
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(slices.Values(doc.Components.Schemas["ReplicationRule"].Required))
	pattern := "PUT " + adminapi.BasePath + "/buckets/{bucket}/replication"
	items := adminapi.RequiredItemFields()
	if len(items) != 1 || len(items[pattern]) != 1 {
		t.Fatalf("the items required are %v", items)
	}
	got := slices.Sorted(slices.Values(items[pattern]["rules"]))
	if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("each rule must give %v; the contract requires %v", got, want)
	}
	if !slices.Contains(adminapi.RequiredFields()[pattern], "rules") {
		t.Errorf("the operation does not require its rules: %v", adminapi.RequiredFields()[pattern])
	}
}

// TestBucketReplicationWithoutTheUseCase: a node that was not given the use
// case answers 501 to a caller it has authenticated, and 401 to one it has
// not.
func TestBucketReplicationWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.Replication = nil })
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for _, c := range []call{
		{method: "GET", path: "/buckets/photos/replication"},
		{method: "PUT", path: "/buckets/photos/replication", body: map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{}}},
		{method: "DELETE", path: "/buckets/photos/replication"},
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
