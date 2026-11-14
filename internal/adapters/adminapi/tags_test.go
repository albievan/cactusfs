package adminapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The operations of tags, tag rules and the catalogue of tags (PLAN D.0,
// ADR-0018) are in the contract ahead of their use cases. Until PLAN D.4
// writes them, each answers 501 as a problem to a caller it has
// authenticated - before the authorizer is asked, as the other operations
// that are not written yet do (TestOperationsThatAreNotWrittenYet) - and 401
// to one it has not. Each that changes something is declared as every other
// such operation is: with Idempotency-Key and X-CSRF-Token, and a session
// without its CSRF token is refused before it is told anything else.

// notWrittenYet asks the operation the contract names id, as c, of every
// kind of caller, and checks the contract's declaration of it against
// action.
func notWrittenYet(t *testing.T, id, action string, c call) {
	t.Helper()

	// The contract declares it as its kind of operation is declared.
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Action      string `json:"x-cactusfs-action"`
			Parameters  []struct {
				Ref string `json:"$ref"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			if operation.OperationID != id {
				continue
			}
			found = true
			if operation.Action != action {
				t.Errorf("%s (%s %s) needs %q, want %q", id, method, path, operation.Action, action)
			}
			var refs []string
			for _, p := range operation.Parameters {
				refs = append(refs, p.Ref)
			}
			for _, p := range []string{"#/components/parameters/IdempotencyKey", "#/components/parameters/CSRFToken"} {
				if mutating := method != "get"; mutating != slices.Contains(refs, p) {
					t.Errorf("%s (%s %s): parameters %v, %s declared: %v", id, method, path, refs, p, !mutating)
				}
			}
		}
	}
	if !found {
		t.Fatalf("the contract has no operation %s", id)
	}

	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=k", raw: []byte("content")})

	// Without a credential, and with one nobody has: 401.
	for name, bearer := range map[string]string{"no credential": "", "a credential that is none": "AKIDNOBODY:not-a-secret"} {
		asked := c
		asked.bearer = bearer
		r := f.do(asked)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s %s with %s = %d, want 401: %s", c.method, c.path, name, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "Unauthenticated" {
			t.Errorf("%s %s with %s: problem = %s", c.method, c.path, name, r.body)
		}
	}

	// With an API key, and again with a key that makes it safe to repeat:
	// 501, and nothing more is said.
	tries := []call{c}
	if c.method != http.MethodGet {
		keyed := c
		keyed.headers = map[string]string{"Idempotency-Key": "d0-" + id}
		tries = append(tries, keyed, keyed)
	}
	for n, asked := range tries {
		asked.bearer = f.root
		r := f.do(asked)
		if r.status != http.StatusNotImplemented {
			t.Errorf("%s %s (try %d) = %d, want 501: %s", c.method, c.path, n, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "NotImplemented" || p["detail"] != nil || p["status"] != float64(http.StatusNotImplemented) {
			t.Errorf("%s %s (try %d): problem = %s", c.method, c.path, n, r.body)
		}
	}

	// A console session: a change without the session's CSRF token is
	// refused 403 before it is told anything else; with it, 501.
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "dora", "roles": []string{iam.RoleClusterAdmin}, "password": password,
	}})
	client, csrf, signedIn := signIn(t, f, "dora", password)
	if signedIn.status != http.StatusOK || csrf == "" {
		t.Fatalf("signing in = %d", signedIn.status)
	}
	asked := c
	asked.client = client
	if c.method != http.MethodGet {
		r := f.do(asked)
		if r.status != http.StatusForbidden {
			t.Errorf("%s %s by a session without its CSRF token = %d, want 403: %s", c.method, c.path, r.status, r.body)
		} else if p := r.problem(t); p["code"] != "CSRFTokenInvalid" {
			t.Errorf("%s %s by a session without its CSRF token: problem = %s", c.method, c.path, r.body)
		}
		asked.headers = map[string]string{httpsession.CSRFHeader: csrf}
	}
	if r := f.do(asked); r.status != http.StatusNotImplemented {
		t.Errorf("%s %s by a session = %d, want 501: %s", c.method, c.path, r.status, r.body)
	} else if p := r.problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
		t.Errorf("%s %s by a session: problem = %s", c.method, c.path, r.body)
	}
}

// someTags is a body of tags for a bucket or an object.
func someTags() map[string]any {
	return map[string]any{"tags": []any{map[string]any{"key": "stage", "value": "draft"}}}
}

// TestGetBucketTagsIsNotWrittenYet: GET /buckets/{bucket}/tags.
func TestGetBucketTagsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "getBucketTags", "s3:GetBucketTagging", call{method: "GET", path: "/buckets/some-bucket/tags"})
}

// TestPutBucketTagsIsNotWrittenYet: PUT /buckets/{bucket}/tags.
func TestPutBucketTagsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "putBucketTags", "s3:PutBucketTagging", call{method: "PUT", path: "/buckets/some-bucket/tags", body: someTags()})
	// What the contract requires of the body is there before anything
	// else is said, as for every operation.
	f := newFixture(t)
	r := f.must(http.StatusBadRequest, call{method: "PUT", path: "/buckets/some-bucket/tags", body: map[string]any{}})
	if p := r.problem(t); p["code"] != "InvalidArgument" {
		t.Errorf("a body without tags: problem = %s", r.body)
	}
}

// TestDeleteBucketTagsIsNotWrittenYet: DELETE /buckets/{bucket}/tags, asked
// as S3 asks DeleteBucketTagging.
func TestDeleteBucketTagsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "deleteBucketTags", "s3:PutBucketTagging", call{method: "DELETE", path: "/buckets/some-bucket/tags"})
}

// TestGetObjectTagsIsNotWrittenYet: GET /buckets/{bucket}/object/tags, of
// the current version and of one named.
func TestGetObjectTagsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	for name, path := range map[string]string{
		"the current version": "/buckets/some-bucket/object/tags?key=k",
		"a version":           "/buckets/some-bucket/object/tags?key=k&version_id=null",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			notWrittenYet(t, "getObjectTags", "s3:GetObjectTagging", call{method: "GET", path: path})
		})
	}
}

// TestPutObjectTagsIsNotWrittenYet: PUT /buckets/{bucket}/object/tags.
func TestPutObjectTagsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "putObjectTags", "s3:PutObjectTagging", call{method: "PUT", path: "/buckets/some-bucket/object/tags?key=k", body: someTags()})
	f := newFixture(t)
	r := f.must(http.StatusBadRequest, call{method: "PUT", path: "/buckets/some-bucket/object/tags?key=k", body: map[string]any{"tags": nil}})
	if p := r.problem(t); p["code"] != "InvalidArgument" {
		t.Errorf("a body whose tags are null: problem = %s", r.body)
	}
}

// TestDeleteObjectTagsIsNotWrittenYet: DELETE /buckets/{bucket}/object/tags.
func TestDeleteObjectTagsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "deleteObjectTags", "s3:DeleteObjectTagging", call{method: "DELETE", path: "/buckets/some-bucket/object/tags?key=k"})
}

// TestGetBucketTagRulesIsNotWrittenYet: GET /buckets/{bucket}/tag-rules.
func TestGetBucketTagRulesIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "getBucketTagRules", string(iam.ActionGetBucketTagRules), call{method: "GET", path: "/buckets/some-bucket/tag-rules"})
}

// TestPutBucketTagRulesIsNotWrittenYet: PUT /buckets/{bucket}/tag-rules. A
// PUT replaces the rules whole, so every field is required: one left out
// would switch a rule off.
func TestPutBucketTagRulesIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "putBucketTagRules", string(iam.ActionPutBucketTagRules), call{
		method: "PUT", path: "/buckets/some-bucket/tag-rules", body: map[string]any{
			"lowercase": true, "catalogue_only": false,
			"required": []any{map[string]any{"key": "stage", "values": []string{"draft", "final"}, "sub_bucket": "reports"}},
		},
	})
	f := newFixture(t)
	for _, body := range []map[string]any{
		{},
		{"lowercase": true, "required": []any{}},
		{"catalogue_only": true, "required": []any{}},
		{"lowercase": true, "catalogue_only": true},
		{"lowercase": true, "catalogue_only": true, "required": nil},
	} {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: "/buckets/some-bucket/tag-rules", body: body})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("rules %v: problem = %s", body, r.body)
		}
	}
}

// TestDeleteBucketTagRulesIsNotWrittenYet: DELETE
// /buckets/{bucket}/tag-rules, asked as setting them is.
func TestDeleteBucketTagRulesIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "deleteBucketTagRules", string(iam.ActionPutBucketTagRules), call{method: "DELETE", path: "/buckets/some-bucket/tag-rules"})
}

// TestListTagDefinitionsIsNotWrittenYet: GET /tag-definitions.
func TestListTagDefinitionsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "listTagDefinitions", string(iam.ActionListTagDefinitions), call{method: "GET", path: "/tag-definitions"})
}

// TestCreateTagDefinitionIsNotWrittenYet: POST /tag-definitions, with values
// and without.
func TestCreateTagDefinitionIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]map[string]any{
		"with values":    {"key": "stage", "description": "How far a document is.", "values": []string{"draft", "final"}},
		"without values": {"key": "stage"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			notWrittenYet(t, "createTagDefinition", string(iam.ActionPutTagDefinition), call{method: "POST", path: "/tag-definitions", body: body})
		})
	}
	f := newFixture(t)
	r := f.must(http.StatusBadRequest, call{method: "POST", path: "/tag-definitions", body: map[string]any{"description": "no key"}})
	if p := r.problem(t); p["code"] != "InvalidArgument" {
		t.Errorf("a definition without a key: problem = %s", r.body)
	}
}

// TestGetTagDefinitionIsNotWrittenYet: GET /tag-definitions/{key}.
func TestGetTagDefinitionIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "getTagDefinition", string(iam.ActionGetTagDefinition), call{method: "GET", path: "/tag-definitions/stage"})
}

// TestPutTagDefinitionIsNotWrittenYet: PUT /tag-definitions/{key}. A PUT
// replaces the definition whole, so both fields are required: values left
// out would let the tag have any value.
func TestPutTagDefinitionIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "putTagDefinition", string(iam.ActionPutTagDefinition), call{
		method: "PUT", path: "/tag-definitions/stage", body: map[string]any{"description": "How far a document is.", "values": []string{}},
	})
	f := newFixture(t)
	for _, body := range []map[string]any{{"description": "x"}, {"values": []string{}}, {}} {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: "/tag-definitions/stage", body: body})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("a definition %v: problem = %s", body, r.body)
		}
	}
}

// TestDeleteTagDefinitionIsNotWrittenYet: DELETE /tag-definitions/{key}.
func TestDeleteTagDefinitionIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "deleteTagDefinition", string(iam.ActionDeleteTagDefinition), call{method: "DELETE", path: "/tag-definitions/stage"})
}

// TestAnUploadWithTagsIsNotMadeYet: the contract gives the upload its tags
// (X-CactusFS-Tagging) ahead of the use case that keeps them (PLAN D.4).
// Until then an upload that gives tags is not made at all - stored without
// them, the version would not be what was asked for - and is answered 501;
// one without tags is made as before. The tags are part of what makes two
// uploads with one Idempotency-Key the same request.
func TestAnUploadWithTagsIsNotMadeYet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	tagged := map[string]string{"X-CactusFS-Tagging": "stage=draft&team=finance"}
	for name, headers := range map[string]map[string]string{
		"tags":           tagged,
		"an empty value": {"X-CactusFS-Tagging": ""},
	} {
		r := f.must(http.StatusNotImplemented, call{method: "PUT", path: "/buckets/some-bucket/object?key=tagged", raw: []byte("content"), headers: headers})
		if p := r.problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
			t.Errorf("an upload with %s: problem = %s", name, r.body)
		}
	}
	if r := f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/some-bucket/object/metadata?key=tagged"}); r.problem(t)["code"] != "NoSuchObject" {
		t.Errorf("an upload with tags stored something: %s", r.body)
	}

	// A 501 is no answer that is kept, so the tags' part in what makes two
	// uploads with one key the same request is seen from an upload made
	// without them: its repeat is answered from the record, and the same
	// key with tags is another request - refused, not answered with the
	// version made without them.
	keyed := func(tags string, set bool) call {
		headers := map[string]string{"Idempotency-Key": "upload-with-tags"}
		if set {
			headers["X-CactusFS-Tagging"] = tags
		}
		return call{method: "PUT", path: "/buckets/some-bucket/object?key=keyed", raw: []byte("content"), headers: headers}
	}
	f.must(http.StatusOK, keyed("", false))
	if r := f.must(http.StatusOK, keyed("", false)); r.header.Get("Idempotent-Replayed") != "true" {
		t.Error("the repeat of an upload was not answered from the record")
	}
	for name, c := range map[string]call{"tags": keyed("stage=draft", true), "an empty tagging": keyed("", true)} {
		if p := f.must(http.StatusUnprocessableEntity, c).problem(t); p["code"] != "IdempotencyKeyReused" {
			t.Errorf("the same key with %s: problem = %v", name, p)
		}
	}

	// Without tags, an upload is made as ever.
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=plain", raw: []byte("content")})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/object/metadata?key=plain"})
}
