package adminapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// withKey is a header of an idempotency key.
func withKey(key string) map[string]string { return map[string]string{"Idempotency-Key": key} }

// recountCall is the recount of a bucket's usage: a POST without a body,
// which says its media type all the same.
func recountCall(bucketName string, headers map[string]string) call {
	h := map[string]string{"Content-Type": "application/json"}
	for name, value := range headers {
		h[name] = value
	}
	return call{method: "POST", path: "/buckets/" + bucketName + "/usage/recount", headers: h}
}

// answerOf decodes an answer into a map, as the contract shows it.
func answerOf(t *testing.T, r reply) map[string]any {
	t.Helper()
	var got map[string]any
	r.decode(t, &got)
	return got
}

// TestBucketQuota: the quota of a bucket read, set and removed through the
// management API (ADR-0024). Who may is the use case's to decide, with
// admin: actions that a bucket's own administrator does not hold; every
// refusal is a problem; a change is safe to repeat; and each change, and
// each denial, is in the trail.
func TestBucketQuota(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	auditor := f.userKey("auditor", iam.RoleAuditor)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	reader := f.userKey("reader", iam.RoleReadOnly)
	for _, name := range []string{"photos", "old"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	// A bucket that was there before usage was counted, and is being
	// counted for the first time: one entry of the walk has visited the
	// first of its two keys.
	for _, key := range []string{"a", "b"} {
		f.must(http.StatusOK, call{method: "PUT", path: "/buckets/old/object?key=" + key, raw: []byte("12345")})
	}
	f.meta.ForgetUsage("old")
	if _, err := f.meta.WalkBucketUsage(t.Context(), "old", 1, false); err != nil {
		t.Fatal(err)
	}

	quotaPath := "/buckets/photos/quota"
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
		{name: "no quota yet", c: call{method: "GET", path: quotaPath}, status: http.StatusOK, want: map[string]any{"used_bytes": float64(0), "exact": true}},
		{
			name: "set", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": 1000}, headers: withKey("set-1")}, status: http.StatusOK,
			want: map[string]any{"quota_bytes": float64(1000), "used_bytes": float64(0), "exact": true}, replay: true,
		},
		{
			name: "read by an auditor", c: call{method: "GET", path: quotaPath, bearer: auditor}, status: http.StatusOK,
			want: map[string]any{"quota_bytes": float64(1000), "used_bytes": float64(0), "exact": true},
		},
		{
			name: "the greatest quota", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": int64(bucket.MaxQuotaBytes)}}, status: http.StatusOK,
			want: map[string]any{"quota_bytes": float64(bucket.MaxQuotaBytes), "used_bytes": float64(0), "exact": true},
		},
		{name: "past the greatest", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": int64(bucket.MaxQuotaBytes) + 1}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "zero", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": 0}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "below nothing", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": -1}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "not a number", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": "1 GB"}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "no body", c: call{method: "PUT", path: quotaPath, headers: map[string]string{"Content-Type": "application/json"}}, status: http.StatusBadRequest, code: "InvalidArgument"},
		{
			name: "back to a thousand", c: call{method: "PUT", path: quotaPath, body: map[string]any{"quota_bytes": 1000}, headers: withKey("set-2")}, status: http.StatusOK,
			want: map[string]any{"quota_bytes": float64(1000), "used_bytes": float64(0), "exact": true}, replay: true,
		},
		{name: "set by an auditor", c: call{method: "PUT", path: quotaPath, bearer: auditor, body: map[string]any{"quota_bytes": 1}}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "lifted by the bucket's administrator", c: call{method: "PUT", path: quotaPath, bearer: owner, body: map[string]any{"quota_bytes": 1 << 40}}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "removed by the bucket's administrator", c: call{method: "DELETE", path: quotaPath, bearer: owner}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "read by the bucket's administrator", c: call{method: "GET", path: quotaPath, bearer: owner}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "read by a reader", c: call{method: "GET", path: quotaPath, bearer: reader}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "removed by an auditor", c: call{method: "DELETE", path: quotaPath, bearer: auditor}, status: http.StatusForbidden, code: "AccessDenied"},
		{name: "read without a credential", c: call{method: "GET", path: quotaPath, bearer: "AKIDNOBODY:not-a-secret"}, status: http.StatusUnauthorized, code: "Unauthenticated"},
		{name: "set without a credential", c: call{method: "PUT", path: quotaPath, bearer: "AKIDNOBODY:not-a-secret", body: map[string]any{"quota_bytes": 1}}, status: http.StatusUnauthorized, code: "Unauthenticated"},
		{name: "removed without a credential", c: call{method: "DELETE", path: quotaPath, bearer: "AKIDNOBODY:not-a-secret"}, status: http.StatusUnauthorized, code: "Unauthenticated"},
		{name: "read of no such bucket", c: call{method: "GET", path: "/buckets/missing/quota"}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "set on no such bucket", c: call{method: "PUT", path: "/buckets/missing/quota", body: map[string]any{"quota_bytes": 1}}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "removed from no such bucket", c: call{method: "DELETE", path: "/buckets/missing/quota"}, status: http.StatusNotFound, code: "NoSuchBucket"},
		{name: "read while counted", c: call{method: "GET", path: "/buckets/old/quota"}, status: http.StatusOK, want: map[string]any{"used_bytes": float64(0), "exact": false}},
		{name: "set while counted", c: call{method: "PUT", path: "/buckets/old/quota", body: map[string]any{"quota_bytes": 1}, headers: withKey("set-old")}, status: http.StatusConflict, code: "Conflict", replay: true},
		{name: "removed while counted", c: call{method: "DELETE", path: "/buckets/old/quota"}, status: http.StatusConflict, code: "Conflict"},
		{name: "removed", c: call{method: "DELETE", path: quotaPath, headers: withKey("delete-1")}, status: http.StatusNoContent, replay: true},
		{name: "removed when there is none", c: call{method: "DELETE", path: quotaPath}, status: http.StatusNoContent},
		{name: "none after the removal", c: call{method: "GET", path: quotaPath, bearer: auditor}, status: http.StatusOK, want: map[string]any{"used_bytes": float64(0), "exact": true}},
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
		case len(r.body) != 0:
			t.Errorf("%s: %d answered with a body: %s", step.name, r.status, r.body)
		}
		if step.replay {
			again := f.do(step.c)
			if again.status != r.status || again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, r.body) {
				t.Errorf("%s: the repeat = %d, replayed %q: %s", step.name, again.status, again.header.Get("Idempotent-Replayed"), again.body)
			}
		}
	}

	// Each change once in the trail, by its record, though two were
	// repeated; each denial of the four callers who may not.
	var set, removed, denied int
	for _, r := range f.trail() {
		switch {
		case r.Action == string(iam.ActionPutBucketQuota) && r.Outcome == audit.Succeeded:
			set++
		case r.Action == string(iam.ActionDeleteBucketQuota) && r.Outcome == audit.Succeeded:
			removed++
		case r.Resource == "arn:aws:s3:::photos" && r.Outcome == audit.Denied &&
			(r.Action == string(iam.ActionPutBucketQuota) || r.Action == string(iam.ActionDeleteBucketQuota) || r.Action == string(iam.ActionGetBucketQuota)):
			denied++
		}
	}
	if set != 3 || removed != 2 || denied != 6 {
		t.Errorf("the trail holds %d quotas set, %d removed, %d denials; want 3, 2 and 6", set, removed, denied)
	}
}

// TestBucketUsageFromTheRecord: the usage of a bucket that is counted is
// the record's - exact, complete, with what it keeps beyond its current
// versions and its quota - and that of a bucket being counted is what the
// index says as far as one request counts, without those figures. The
// quota is judged before a write through this API stores anything.
func TestBucketUsageFromTheRecord(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, name := range []string{"photos", "old"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	// Versions are kept: an overwrite keeps what was there. (Versioning is
	// S3's to set; the store is told here.)
	if err := f.meta.SetBucketVersioning(t.Context(), "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"aaaa", "bbbbbb", "cc"} {
		key := "a"
		if i == 2 {
			key = "b"
		}
		f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/object?key=" + key, raw: []byte(content)})
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/quota", body: map[string]any{"quota_bytes": 14}})

	usage := answerOf(t, f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/usage"}))
	want := map[string]any{
		"object_count": float64(2), "total_size": float64(8), "complete": true, "exact": true,
		"versions": float64(3), "stored_bytes": float64(12), "upload_bytes": float64(0), "quota_bytes": float64(14),
	}
	if !reflect.DeepEqual(usage, want) {
		t.Errorf("usage from the record = %v, want %v", usage, want)
	}
	if got := answerOf(t, f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/quota"})); got["used_bytes"] != float64(12) {
		t.Errorf("the quota says %v bytes are used, want 12", got["used_bytes"])
	}

	// Two bytes are left: three are refused before anything is stored, two
	// are taken.
	p := f.must(http.StatusForbidden, call{method: "PUT", path: "/buckets/photos/object?key=c", raw: []byte("ccc")}).problem(t)
	if p["code"] != "QuotaExceeded" {
		t.Errorf("a write past the quota = %v", p)
	}
	if n := f.refusals.Load(); n != 1 {
		t.Errorf("%d refusals counted, want the 1 of the pre-check", n)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/photos/object?key=c"})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/object?key=c", raw: []byte("cc")})

	// Being counted: the walk of the index, no figures of the record and
	// no quota.
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/old/object?key=k", raw: []byte("12345")})
	f.meta.ForgetUsage("old")
	old := answerOf(t, f.must(http.StatusOK, call{method: "GET", path: "/buckets/old/usage"}))
	if want := map[string]any{"object_count": float64(1), "total_size": float64(5), "complete": true, "exact": false}; !reflect.DeepEqual(old, want) {
		t.Errorf("usage of a bucket being counted = %v, want %v", old, want)
	}
}

// TestRecountBucketUsage: a recount is asked for with 202 - the count is
// begun - by a caller allowed admin:RecountBucketUsage on the bucket, and
// by nobody else; each recount and each denial is in the trail.
func TestRecountBucketUsage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	auditor := f.userKey("auditor", iam.RoleAuditor)
	owner := f.userKey("owner", iam.RoleBucketAdmin)
	for _, name := range []string{"photos", "old"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/old/object?key=k", raw: []byte("12345")})
	f.meta.ForgetUsage("old")

	first := f.must(http.StatusAccepted, recountCall("photos", withKey("recount-1")))
	if len(first.body) != 0 {
		t.Errorf("202 answered with a body: %s", first.body)
	}
	if again := f.must(http.StatusAccepted, recountCall("photos", withKey("recount-1"))); again.header.Get("Idempotent-Replayed") != "true" {
		t.Error("the repeat of a recount with its key was carried out again")
	}
	// A bucket counted for the first time is counted by the recount: it is
	// small, and one entry does it.
	f.must(http.StatusAccepted, recountCall("old", nil))
	usage := answerOf(t, f.must(http.StatusOK, call{method: "GET", path: "/buckets/old/usage"}))
	if usage["exact"] != true || usage["stored_bytes"] != float64(5) {
		t.Errorf("usage after the recount of a small bucket = %v", usage)
	}

	for name, tt := range map[string]struct {
		c      call
		status int
		code   string
	}{
		"an auditor":                 {withBearer(recountCall("photos", nil), auditor), http.StatusForbidden, "AccessDenied"},
		"the bucket's administrator": {withBearer(recountCall("photos", nil), owner), http.StatusForbidden, "AccessDenied"},
		"no credential":              {withBearer(recountCall("photos", nil), "AKIDNOBODY:not-a-secret"), http.StatusUnauthorized, "Unauthenticated"},
		"no such bucket":             {withBearer(recountCall("missing", nil), f.root), http.StatusNotFound, "NoSuchBucket"},
		"no media type":              {call{method: "POST", path: "/buckets/photos/usage/recount", bearer: f.root}, http.StatusUnsupportedMediaType, ""},
	} {
		r := f.do(tt.c)
		if r.status != tt.status {
			t.Errorf("%s: recount = %d, want %d: %s", name, r.status, tt.status, r.body)
			continue
		}
		if p := r.problem(t); tt.code != "" && p["code"] != tt.code {
			t.Errorf("%s: problem = %s, want code %s", name, r.body, tt.code)
		}
	}

	var recounts, denied int
	for _, r := range f.trail() {
		if r.Action != string(iam.ActionRecountBucketUsage) {
			continue
		}
		switch r.Outcome {
		case audit.Succeeded:
			recounts++
		case audit.Denied:
			denied++
		default:
		}
	}
	if recounts != 2 || denied != 2 {
		t.Errorf("the trail holds %d recounts and %d denials, want 2 and 2", recounts, denied)
	}
}

// TestQuotasBelowLevel5: until every member of the metadata group counts
// usage, everything of quotas answers 503 and says to try again, and the
// usage is what the index says, not exact (ADR-0024).
func TestQuotasBelowLevel5(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A cluster with a member of schema 4: its buckets are made without a
	// usage record.
	f.meta.Level = ports.UsageLevel - 1
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for _, c := range []call{
		{method: "GET", path: "/buckets/photos/quota"},
		{method: "PUT", path: "/buckets/photos/quota", body: map[string]any{"quota_bytes": 1}},
		{method: "DELETE", path: "/buckets/photos/quota"},
		recountCall("photos", nil),
	} {
		r := f.do(withBearer(c, f.root))
		if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" {
			t.Errorf("%s %s below level 5 = %d, Retry-After %q: %s", c.method, c.path, r.status, r.header.Get("Retry-After"), r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "ServiceUnavailable" || p["detail"] != nil {
			t.Errorf("%s %s below level 5: problem = %s", c.method, c.path, r.body)
		}
	}
	usage := answerOf(t, f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/usage"}))
	if want := map[string]any{"object_count": float64(0), "total_size": float64(0), "complete": true, "exact": false}; !reflect.DeepEqual(usage, want) {
		t.Errorf("usage below level 5 = %v, want %v", usage, want)
	}
	if b, err := f.meta.GetBucket(t.Context(), "photos"); err != nil || b.QuotaBytes != 0 {
		t.Errorf("below level 5 the quota is %d, %v", b.QuotaBytes, err)
	}
}

// TestQuotasOnANodeWithoutTheUseCases: a node that was not given the use
// cases of quotas answers their operations 501, and the usage as before.
func TestQuotasOnANodeWithoutTheUseCases(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.Quotas, c.Counter = nil, nil })
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for _, c := range []call{
		{method: "GET", path: "/buckets/photos/quota"},
		{method: "PUT", path: "/buckets/photos/quota", body: map[string]any{"quota_bytes": 1}},
		{method: "DELETE", path: "/buckets/photos/quota"},
		recountCall("photos", nil),
	} {
		if p := f.must(http.StatusNotImplemented, c).problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
			t.Errorf("%s %s on a node without the use case: problem = %v", c.method, c.path, p)
		}
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/usage"})
}

// TestTheQuotaAnswerIsTheContracts: the 200 of a quota carries the bytes as
// a JSON integer, even the greatest, which a float64 would round.
func TestTheQuotaAnswerIsTheContracts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	greatest := strconv.FormatInt(bucket.MaxQuotaBytes, 10)
	r := f.must(http.StatusOK, call{
		method: "PUT", path: "/buckets/photos/quota", raw: []byte(`{"quota_bytes":` + greatest + `}`),
		headers: map[string]string{"Content-Type": "application/json"},
	})
	var got struct {
		QuotaBytes json.Number `json:"quota_bytes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(r.body))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil || got.QuotaBytes.String() != greatest {
		t.Errorf("the greatest quota is answered as %q, %v; want %s", got.QuotaBytes, err, greatest)
	}
}
