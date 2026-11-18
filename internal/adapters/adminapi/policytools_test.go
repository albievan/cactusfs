package adminapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// contractOperation is an operation of the contract, as the tests of the
// policy builder read it.
type contractOperation struct {
	OperationID string `json:"operationId"`
	Action      string `json:"x-cactusfs-action"`
	Parameters  []struct {
		Ref string `json:"$ref"`
	} `json:"parameters"`
}

// contractOperations returns every operation of the contract by its
// operationId, with its method and path.
func contractOperations(t *testing.T) map[string]struct {
	method, path string
	op           contractOperation
} {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]contractOperation `json:"paths"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct {
		method, path string
		op           contractOperation
	}{}
	for path, operations := range doc.Paths {
		for method, op := range operations {
			out[op.OperationID] = struct {
				method, path string
				op           contractOperation
			}{strings.ToUpper(method), path, op}
		}
	}
	return out
}

// TestContractActionsAreInTheCatalogue (ADR-0032, Decision 1): every action
// the contract names as what an operation needs is an action of the
// catalogue - a policy can grant it, and the builder offers it.
func TestContractActionsAreInTheCatalogue(t *testing.T) {
	t.Parallel()
	named := 0
	for id, operation := range contractOperations(t) {
		if operation.op.Action == "" {
			continue
		}
		named++
		if _, ok := iam.LookupAction(iam.Action(operation.op.Action)); !ok {
			t.Errorf("%s (%s %s) needs %q, which is not in iam.Catalogue()", id, operation.method, operation.path, operation.op.Action)
		}
	}
	if named < 80 {
		t.Errorf("only %d operations name an action: the walk of the contract found too few", named)
	}
}

// TestPolicyBuilderOperationsAreDeclared: the five operations of the policy
// builder are in the contract where ADR-0032 puts them, with the action
// their use case asks, and the three validations with Idempotency-Key and
// X-CSRF-Token, as every POST.
func TestPolicyBuilderOperationsAreDeclared(t *testing.T) {
	t.Parallel()
	operations := contractOperations(t)
	tests := []struct {
		id, method, path, action string
		changes                  bool
	}{
		{"getPolicyCatalogue", "GET", "/policy-catalogue", "admin:GetPolicyCatalogue", false},
		{"validatePolicy", "POST", "/policy-validation", "admin:PutPolicy", true},
		{"validateBucketPolicy", "POST", "/buckets/{bucket}/policy/validation", "s3:PutBucketPolicy", true},
		{"validateSubBucketPolicy", "POST", "/buckets/{bucket}/subbuckets/{name}/policy/validation", "s3:PutSubBucket", true},
		{"explainAccess", "GET", "/access/explain", "admin:ExplainAccess", false},
	}
	for _, tt := range tests {
		got, ok := operations[tt.id]
		if !ok {
			t.Errorf("%s is not in the contract", tt.id)
			continue
		}
		if got.method != tt.method || got.path != tt.path || got.op.Action != tt.action {
			t.Errorf("%s is %s %s needing %q, want %s %s needing %q", tt.id, got.method, got.path, got.op.Action, tt.method, tt.path, tt.action)
		}
		var refs []string
		for _, p := range got.op.Parameters {
			refs = append(refs, p.Ref)
		}
		for _, p := range []string{"#/components/parameters/IdempotencyKey", "#/components/parameters/CSRFToken"} {
			if slices.Contains(refs, p) != tt.changes {
				t.Errorf("%s: parameters %v, want %s: %v", tt.id, refs, p, tt.changes)
			}
		}
	}
}

// deniedIn reports whether the trail, from the record at from, holds the
// refusal of action to actor - with reason, when reason is not empty.
func deniedIn(f *fixture, from int, actor, action, reason string) bool {
	f.t.Helper()
	for _, r := range f.trail()[from:] {
		if r.Outcome == audit.Denied && r.Actor == actor && r.Action == action && (reason == "" || r.Details["reason"] == reason) {
			return true
		}
	}
	return false
}

// refusedWithoutDetail fails the test unless r is a 403 AccessDenied that
// says nothing more: no detail, no faults.
func refusedWithoutDetail(t *testing.T, what string, r reply) {
	t.Helper()
	if r.status != http.StatusForbidden {
		t.Errorf("%s = %d, want 403: %s", what, r.status, r.body)
		return
	}
	if p := r.problem(t); p["code"] != "AccessDenied" || p["detail"] != nil || p["faults"] != nil {
		t.Errorf("%s: refusal = %s", what, r.body)
	}
}

// TestPolicyCatalogueOverHTTP: the catalogue is the domain's, answered to a
// caller who holds admin:GetPolicyCatalogue - root, cluster-admin by its
// wildcard, auditor by admin:Get*, bucket-admin by name - and refused,
// audited and without detail, to one who does not.
func TestPolicyCatalogueOverHTTP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if r := f.do(call{method: "GET", path: "/policy-catalogue"}); r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
		t.Errorf("the catalogue without a credential = %d %s", r.status, r.body)
	}
	want := iam.Catalogue()
	for _, role := range []string{"", iam.RoleClusterAdmin, iam.RoleAuditor, iam.RoleBucketAdmin} {
		bearer := f.root
		if role != "" {
			bearer = f.userKey("cat-"+role, role)
		}
		var got api.PolicyCatalogue
		f.must(http.StatusOK, call{method: "GET", path: "/policy-catalogue", bearer: bearer}).decode(t, &got)
		if len(got.Actions) != len(want) || got.Version != iam.PolicyVersion || got.Limits.MaxBytes != iam.MaxPolicyBytes ||
			len(got.ConditionKeys) != len(iam.ConditionKeys()) || len(got.Resources) != len(iam.ResourceKinds()) ||
			got.BucketPolicyPrincipals.UserArnPrefix != iam.BucketPolicyPrincipals().UserARNPrefix {
			t.Fatalf("the catalogue as %q = %d actions, version %q, limits %+v; want %d actions", role, len(got.Actions), got.Version, got.Limits, len(want))
		}
		for i, a := range got.Actions {
			w := want[i]
			if a.Name != string(w.Name) || a.Group != w.Group || a.Read != w.Read || a.ByNameOnly != w.ByNameOnly ||
				a.InBucketPolicy != w.InBucketPolicy || a.Anonymous != w.Anonymous || a.InSubBucketPolicy != w.InSubBucketPolicy ||
				fmt.Sprint(a.Resources) != fmt.Sprint(w.Resources) {
				t.Errorf("action %d = %+v, want %+v", i, a, w)
			}
		}
	}
	reader := f.userKey("reader", iam.RoleReadOnly)
	from := len(f.trail())
	refusedWithoutDetail(t, "the catalogue as a read-only user", f.do(call{method: "GET", path: "/policy-catalogue", bearer: reader}))
	if !deniedIn(f, from, "user/reader", string(iam.ActionGetPolicyCatalogue), "") {
		t.Error("the refusal of the catalogue is not in the audit trail")
	}
}

// Documents of each kind: one each validation takes, and one it refuses,
// with where its fault is.
const (
	identityValid = `{"Version":"2012-10-17","Statement":[{"Sid":"read","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/*"}]}`
	identityWrong = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/*"},` +
		`{"Effect":"Maybe","Action":"s3:GetObject","Resource":"*"}]}`
	bucketValid = `{"Version":"2012-10-17","Statement":[{"Sid":"read","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/*"}]}`
	bucketWrong = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other-bucket/*"}]}`
	subValid    = `{"Version":"2012-10-17","Statement":[{"Sid":"read","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/dora"},` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/reports/*"}]}`
	subWrong = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/dora"},` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/other/*"}]}`
)

// policyBody is a JSON body with the document in field, as written, and
// the other fields given.
func policyBody(field, document string, other map[string]string) []byte {
	parts := []string{fmt.Sprintf("%q:%s", field, document)}
	for name, value := range other {
		parts = append(parts, fmt.Sprintf("%q:%q", name, value))
	}
	slices.Sort(parts)
	return []byte("{" + strings.Join(parts, ",") + "}")
}

// TestValidationOverHTTP (ADR-0032, Decision 2): for each put that takes a
// policy, its validation answers 200 for a document it takes, with what each
// statement does; 200 with valid false and the located faults for one it
// refuses - and the put of that document is refused 400 with its code and
// the same faults in the problem. A caller without the put's action is
// refused the validation, 403 without detail, audited; one without a
// credential 401. Nothing is stored.
func TestValidationOverHTTP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "dora", "roles": []string{iam.RoleReadOnly}}})
	f.must(http.StatusCreated, call{method: "POST", path: "/policies", body: map[string]any{"name": "existing", "document": json.RawMessage(identityValid)}})
	media := map[string]string{"Content-Type": "application/json"}

	tests := []struct {
		name                  string
		validate, field       string
		other                 map[string]string
		put, putPath, putCode string
		putField              string
		putOther              map[string]string
		action                iam.Action
		valid, wrong          string
		pointer               string
		statement             int
		buckets               bool
	}{
		{
			name: "createPolicy", validate: "/policy-validation", field: "document", other: map[string]string{"name": "checked"},
			put: "POST", putPath: "/policies", putCode: "InvalidArgument", putField: "document", putOther: map[string]string{"name": "checked"},
			action: iam.ActionPutPolicy, valid: identityValid, wrong: identityWrong, pointer: "/Statement/1/Effect", statement: 1,
		},
		{
			name: "updatePolicy", validate: "/policy-validation", field: "document", other: map[string]string{"name": "existing"},
			put: "PUT", putPath: "/policies/existing", putCode: "InvalidArgument", putField: "document",
			action: iam.ActionPutPolicy, valid: identityValid, wrong: identityWrong, pointer: "/Statement/1/Effect", statement: 1,
		},
		{
			name: "createAPIKey", validate: "/policy-validation", field: "document", other: map[string]string{"key_owner": "dora"},
			put: "POST", putPath: "/users/dora/keys", putCode: "InvalidArgument", putField: "inline_policy",
			action: iam.ActionCreateKey, valid: identityValid, wrong: identityWrong, pointer: "/Statement/1/Effect", statement: 1,
		},
		{
			name: "putBucketPolicy", validate: "/buckets/some-bucket/policy/validation", field: "document",
			put: "PUT", putPath: "/buckets/some-bucket/policy", putCode: "InvalidPolicy", putField: "document",
			action: iam.ActionPutBucketPolicy, valid: bucketValid, wrong: bucketWrong, pointer: "/Statement/0/Resource", statement: 0, buckets: true,
		},
		{
			name: "putSubBucket", validate: "/buckets/some-bucket/subbuckets/reports/policy/validation", field: "policy",
			put: "PUT", putPath: "/buckets/some-bucket/subbuckets/reports", putCode: "InvalidPolicy", putField: "policy",
			action: iam.ActionPutSubBucket, valid: subValid, wrong: subWrong, pointer: "/Statement/0/Resource", statement: 0, buckets: true,
		},
	}
	for n, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A document it takes: what each statement does.
			var taken api.PolicyValidation
			f.must(http.StatusOK, call{method: "POST", path: tt.validate, raw: policyBody(tt.field, tt.valid, tt.other), headers: media}).decode(t, &taken)
			if !taken.Valid || len(taken.Faults) != 0 || taken.Truncated || len(taken.Statements) != 1 {
				t.Fatalf("the validation of a document it takes = %+v", taken)
			}
			s := taken.Statements[0]
			if s.Index != 0 || s.Sid == nil || *s.Sid != "read" || s.Effect != api.StatementAnalysisEffect(iam.Allow) ||
				!slices.Contains(s.Actions, string(iam.ActionGetObject)) || s.UnmatchedPatterns == nil || s.NeverAnonymous == nil {
				t.Errorf("the analysis of the statement = %+v", s)
			}
			if (s.OpenToAnyone != nil) != tt.buckets {
				t.Errorf("open_to_anyone is given: %v, want %v", s.OpenToAnyone != nil, tt.buckets)
			}

			// A document it refuses: valid false, every fault located.
			var refused api.PolicyValidation
			r := f.must(http.StatusOK, call{method: "POST", path: tt.validate, raw: policyBody(tt.field, tt.wrong, tt.other), headers: media})
			r.decode(t, &refused)
			if refused.Valid || len(refused.Faults) == 0 || len(refused.Statements) != 0 || refused.Faults[0].Pointer != tt.pointer ||
				refused.Faults[0].Statement == nil || *refused.Faults[0].Statement != tt.statement || refused.Faults[0].Reason == "" {
				t.Fatalf("the validation of a document it refuses = %s", r.body)
			}

			// The put of it: 400, its code, the same faults.
			put := f.must(http.StatusBadRequest, call{method: tt.put, path: tt.putPath, raw: policyBody(tt.putField, tt.wrong, tt.putOther), headers: media})
			var problem api.Problem
			put.decode(t, &problem)
			if problem.Code != tt.putCode || problem.Faults == nil {
				t.Fatalf("the put of the refused document = %s", put.body)
			}
			if got, want := mustJSON(t, *problem.Faults), mustJSON(t, refused.Faults); string(got) != string(want) {
				t.Errorf("the put's faults = %s, the validation's = %s", got, want)
			}

			// A caller without the put's action: refused, audited, told
			// nothing; one without a credential: 401.
			reader := f.userKey(fmt.Sprintf("reader-%d", n), iam.RoleReadOnly)
			from := len(f.trail())
			refusedWithoutDetail(t, tt.name+"'s validation as a read-only user",
				f.do(call{method: "POST", path: tt.validate, raw: policyBody(tt.field, tt.wrong, tt.other), headers: media, bearer: reader}))
			if !deniedIn(f, from, fmt.Sprintf("user/reader-%d", n), string(tt.action), "") {
				t.Errorf("the refusal of %s's validation is not in the audit trail as one of %s", tt.name, tt.action)
			}
			if r := f.do(call{method: "POST", path: tt.validate, raw: policyBody(tt.field, tt.valid, tt.other), headers: media}); r.status != http.StatusUnauthorized {
				t.Errorf("%s's validation without a credential = %d", tt.name, r.status)
			}
		})
	}
	// Nothing was stored by a validation, nor by a refused put.
	f.must(http.StatusNotFound, call{method: "GET", path: "/policies/checked"})
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/some-bucket/subbuckets/reports"})
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/policy"}); strings.TrimSpace(string(r.body)) != "{}" {
		t.Errorf("the bucket's policy after validations = %s", r.body)
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: "/users/dora/keys"}); strings.Contains(string(r.body), "access_key_id") {
		t.Errorf("a key was made: %s", r.body)
	}
}

// TestAValidationRequestThatIsNoneIsRefused: what is wrong with the request
// itself, and not with its document, is a 400 InvalidArgument without
// faults - and a validation of a bucket that is not there is no error.
func TestAValidationRequestThatIsNoneIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	media := map[string]string{"Content-Type": "application/json"}
	for name, c := range map[string]call{
		"no document":            {path: "/policy-validation", raw: []byte(`{"name":"p"}`)},
		"a null document":        {path: "/policy-validation", raw: []byte(`{"document":null}`)},
		"a name and a key owner": {path: "/policy-validation", raw: policyBody("document", identityValid, map[string]string{"name": "p", "key_owner": "dora"})},
		"a name that is none":    {path: "/policy-validation", raw: policyBody("document", identityValid, map[string]string{"name": "no name"})},
		"an empty name":          {path: "/policy-validation", raw: policyBody("document", identityValid, map[string]string{"name": ""})},
		"an empty key owner":     {path: "/policy-validation", raw: policyBody("document", identityValid, map[string]string{"key_owner": ""})},
		"not JSON":               {path: "/buckets/some-bucket/policy/validation", raw: []byte(`{"document":`)},
		"no policy":              {path: "/buckets/some-bucket/subbuckets/reports/policy/validation", raw: []byte(`{}`)},
		"no sub-bucket name":     {path: "/buckets/some-bucket/subbuckets/Reports/policy/validation", raw: policyBody("policy", subValid, nil)},
	} {
		c.method, c.headers = "POST", media
		r := f.must(http.StatusBadRequest, c)
		if p := r.problem(t); p["code"] != "InvalidArgument" || p["faults"] != nil {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	// The bucket is not looked up: its name only confines the document.
	var v api.PolicyValidation
	f.must(http.StatusOK, call{
		method: "POST", path: "/buckets/no-such-bucket/policy/validation", headers: media,
		raw: policyBody("document", strings.ReplaceAll(bucketValid, "some-bucket", "no-such-bucket"), nil),
	}).decode(t, &v)
	if !v.Valid {
		t.Errorf("the validation of a policy of a bucket that is not there = %+v", v)
	}
}

// TestAValidationKeepsNoKeyAndNeedsTheCSRFToken: a validation takes
// Idempotency-Key and does nothing with it, as unlocking does - the same
// key with another body is carried out again, nothing is reserved or
// replayed - and, made with a console session, needs the session's CSRF
// token, as every POST does. An explanation is a GET, and needs none.
func TestAValidationKeepsNoKeyAndNeedsTheCSRFToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	admin := consoleOf(t, f, "builder", iam.RoleClusterAdmin)
	records := f.watched.idempotent.Load()
	routes := []struct{ path, field, valid, wrong string }{
		{"/policy-validation", "document", identityValid, identityWrong},
		{"/buckets/some-bucket/policy/validation", "document", bucketValid, bucketWrong},
		{"/buckets/some-bucket/subbuckets/reports/policy/validation", "policy", subValid, subWrong},
	}
	for _, route := range routes {
		for _, document := range []string{route.valid, route.wrong} {
			r := f.must(http.StatusOK, call{
				method: "POST", path: route.path, raw: policyBody(route.field, document, nil), client: admin.client,
				headers: map[string]string{"Content-Type": "application/json", httpsession.CSRFHeader: admin.csrf, "Idempotency-Key": "one-key"},
			})
			if r.header.Get("Idempotent-Replayed") != "" {
				t.Errorf("POST %s was answered from a record", route.path)
			}
		}
		r := f.do(call{
			method: "POST", path: route.path, raw: policyBody(route.field, route.valid, nil), client: admin.client,
			headers: map[string]string{"Content-Type": "application/json"},
		})
		if r.status != http.StatusForbidden || r.problem(t)["code"] != "CSRFTokenInvalid" {
			t.Errorf("POST %s with a session and no CSRF token = %d %s", route.path, r.status, r.body)
		}
	}
	if n := f.watched.idempotent.Load(); n != records {
		t.Errorf("validations reserved %d idempotency keys", n-records)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/access/explain?user=builder&action=admin:ListUsers&admin_resource=user", client: admin.client})
}

// TestABoundKeyMayNotCheckAKeysPolicy: an API key with an inline policy of
// its own may not make keys, and so may not check the policy of one either:
// a validation with key_owner made with it is refused, audited with the
// reason CreateAPIKey gives, whatever its owner may do. It may still check
// a named policy, as it may put one.
func TestABoundKeyMayNotCheckAKeysPolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "boss", "roles": []string{iam.RoleClusterAdmin}}})
	everything := json.RawMessage(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	f.must(http.StatusCreated, call{method: "POST", path: "/users/boss/keys", body: map[string]any{"inline_policy": everything}}).decode(t, &created)
	bound := created.Key.AccessKeyID + ":" + created.SecretKey
	media := map[string]string{"Content-Type": "application/json"}

	from := len(f.trail())
	refusedWithoutDetail(t, "a key's validation by a bound key", f.do(call{
		method: "POST", path: "/policy-validation", raw: policyBody("document", identityValid, map[string]string{"key_owner": "boss"}), headers: media, bearer: bound,
	}))
	if !deniedIn(f, from, "user/boss", string(iam.ActionCreateKey), "an API key with a policy of its own cannot make keys") {
		t.Error("the refusal is not in the audit trail with its reason")
	}
	f.must(http.StatusOK, call{
		method: "POST", path: "/policy-validation", raw: policyBody("document", identityValid, map[string]string{"name": "p"}), headers: media, bearer: bound,
	})
}

// TestAPutsFaultsAreItsOwnDocuments: a problem carries faults only for a
// 400 that refuses the policy document of the request - not for another
// 400 of a put that takes one, nor for any other answer.
func TestAPutsFaultsAreItsOwnDocuments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	media := map[string]string{"Content-Type": "application/json"}
	for name, c := range map[string]call{
		"a sub-bucket name that is none": {method: "PUT", path: "/buckets/some-bucket/subbuckets/Reports", raw: policyBody("policy", subWrong, nil)},
		"a body that is not JSON":        {method: "PUT", path: "/policies/x", raw: []byte(`{"document":`)},
		"a policy field twice":           {method: "PUT", path: "/policies/x", raw: []byte(`{"document":{},"Document":` + identityWrong + `}`)},
	} {
		c.headers = media
		r := f.must(http.StatusBadRequest, c)
		if p := r.problem(t); p["faults"] != nil {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	// A refusal of the put's action says nothing of the document either:
	// a put authorizes before it parses.
	reader := f.userKey("reader", iam.RoleReadOnly)
	refusedWithoutDetail(t, "a put of a refused document by a caller who may not put", f.do(call{
		method: "POST", path: "/policies", raw: policyBody("document", identityWrong, map[string]string{"name": "x"}), headers: media, bearer: reader,
	}))
}
