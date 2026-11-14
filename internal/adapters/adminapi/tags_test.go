package adminapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// The operations of tags, tag rules and the catalogue of tags (ADR-0018,
// PLAN D.4). The handlers translate - tags as lists in the order given, a
// key given twice passed on for the domain to refuse, and answered in key
// order - and the use cases authorize, keep the rules and audit
// (BucketService, ObjectService, TagDefinitionService). These tests ask
// what a caller is told, and that a change is recorded once - also when its
// request is repeated with the same Idempotency-Key.

// tagList is a list of tags as the contract sends them: key, value, key,
// value...
func tagList(pairs ...string) []any {
	out := []any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"key": pairs[i], "value": pairs[i+1]})
	}
	return out
}

// someTags is a body of tags for a bucket or an object.
func someTags() map[string]any {
	return map[string]any{"tags": tagList("stage", "draft")}
}

// tagsAnswer is what the contract answers of the tags of a bucket or a
// version.
type tagsAnswer struct {
	Tags []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"tags"`
	VersionID *string `json:"version_id"`
}

// pairs is the answer's tags as key, value, key, value...
func (a tagsAnswer) pairs() []string {
	out := []string{}
	for _, tag := range a.Tags {
		out = append(out, tag.Key, tag.Value)
	}
	return out
}

// tagsOf reads the tags an answer carries; the list is never null.
func tagsOf(t *testing.T, r reply) tagsAnswer {
	t.Helper()
	if !bytes.Contains(r.body, []byte(`"tags":[`)) {
		t.Errorf("the tags of an answer are not a list: %s", r.body)
	}
	var answer tagsAnswer
	r.decode(t, &answer)
	return answer
}

// adminRecords counts the records of the trail from n on of action on the
// administrative resource that succeeded.
func adminRecords(f *fixture, from int, action, resource string) int {
	f.t.Helper()
	count := 0
	for _, r := range f.trail()[from:] {
		if r.Action == action && r.Resource == "arn:cactusfs:admin:::"+resource && r.Outcome == audit.Succeeded {
			count++
		}
	}
	return count
}

// tagOperation is an operation of tags, with the action it asks and a
// well-formed call of it.
type tagOperation struct {
	id, action string
	c          call
}

// tagOperations are the operations of tags, tag rules and the catalogue.
func tagOperations() []tagOperation {
	return []tagOperation{
		{"getBucketTags", "s3:GetBucketTagging", call{method: "GET", path: "/buckets/some-bucket/tags"}},
		{"putBucketTags", "s3:PutBucketTagging", call{method: "PUT", path: "/buckets/some-bucket/tags", body: someTags()}},
		{"deleteBucketTags", "s3:PutBucketTagging", call{method: "DELETE", path: "/buckets/some-bucket/tags"}},
		{"getObjectTags", "s3:GetObjectTagging", call{method: "GET", path: "/buckets/some-bucket/object/tags?key=k"}},
		{"putObjectTags", "s3:PutObjectTagging", call{method: "PUT", path: "/buckets/some-bucket/object/tags?key=k", body: someTags()}},
		{"deleteObjectTags", "s3:DeleteObjectTagging", call{method: "DELETE", path: "/buckets/some-bucket/object/tags?key=k"}},
		{"getBucketTagRules", string(iam.ActionGetBucketTagRules), call{method: "GET", path: "/buckets/some-bucket/tag-rules"}},
		{"putBucketTagRules", string(iam.ActionPutBucketTagRules), call{method: "PUT", path: "/buckets/some-bucket/tag-rules", body: map[string]any{
			"lowercase": true, "catalogue_only": false, "required": []any{},
		}}},
		{"deleteBucketTagRules", string(iam.ActionPutBucketTagRules), call{method: "DELETE", path: "/buckets/some-bucket/tag-rules"}},
		{"listTagDefinitions", string(iam.ActionListTagDefinitions), call{method: "GET", path: "/tag-definitions"}},
		{"createTagDefinition", string(iam.ActionPutTagDefinition), call{method: "POST", path: "/tag-definitions", body: map[string]any{"key": "team"}}},
		{"getTagDefinition", string(iam.ActionGetTagDefinition), call{method: "GET", path: "/tag-definitions/stage"}},
		{"putTagDefinition", string(iam.ActionPutTagDefinition), call{method: "PUT", path: "/tag-definitions/stage", body: map[string]any{
			"description": "How far a document is.", "values": []string{"draft", "final"},
		}}},
		{"deleteTagDefinition", string(iam.ActionDeleteTagDefinition), call{method: "DELETE", path: "/tag-definitions/stage"}},
	}
}

// TestTagOperationsAreDeclared: each operation is declared in the contract
// with the action its use case asks, and each that changes something with
// Idempotency-Key and X-CSRF-Token, as every other.
func TestTagOperationsAreDeclared(t *testing.T) {
	t.Parallel()
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
	for _, tt := range tagOperations() {
		found := false
		for path, operations := range doc.Paths {
			for method, operation := range operations {
				if operation.OperationID != tt.id {
					continue
				}
				found = true
				if operation.Action != tt.action || !strings.EqualFold(method, tt.c.method) {
					t.Errorf("%s (%s %s) needs %q, want %q (%s)", tt.id, method, path, operation.Action, tt.action, tt.c.method)
				}
				var refs []string
				for _, p := range operation.Parameters {
					refs = append(refs, p.Ref)
				}
				for _, p := range []string{"#/components/parameters/IdempotencyKey", "#/components/parameters/CSRFToken"} {
					if mutating := method != "get"; mutating != slices.Contains(refs, p) {
						t.Errorf("%s (%s %s): parameters %v, %s declared: %v", tt.id, method, path, refs, p, !mutating)
					}
				}
			}
		}
		if !found {
			t.Errorf("the contract has no operation %s", tt.id)
		}
	}
}

// TestABucketMadeHereLowerCasesItsTags: a bucket made through this API has
// the tag rule lowercase from the start (createBucket, ADR-0018); one made
// otherwise - by S3's CreateBucket - has no rules.
func TestABucketMadeHereLowerCasesItsTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	start := len(f.trail())
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	if err := f.meta.CreateBucket(t.Context(), bucket.Bucket{Name: "from-s3", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"some-bucket": `{"catalogue_only":false,"lowercase":true,"required":[]}`,
		"from-s3":     `{"catalogue_only":false,"lowercase":false,"required":[]}`,
	} {
		if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/" + name + "/tag-rules"}); strings.TrimSpace(string(r.body)) != want {
			t.Errorf("the rules of %s = %s, want %s", name, r.body, want)
		}
	}
	// One record of the creation, which says so.
	var made []audit.Record
	for _, r := range f.trail()[start:] {
		if r.Action == string(iam.ActionCreateBucket) {
			made = append(made, r)
		}
	}
	if len(made) != 1 || made[0].Details["tag_rules"] != "lowercase" || made[0].Outcome != audit.Succeeded {
		t.Errorf("the records of the creation = %+v", made)
	}
}

// TestABucketIsAnsweredAsItWasMade: the 201 of createBucket is the bucket
// as the creation wrote it - nothing is read or asked again - so a caller
// who may make buckets and not list them is answered 201, and so is the
// repeat with its Idempotency-Key; and a read that would fail after the
// creation changes nothing of the answer.
func TestABucketIsAnsweredAsItWasMade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	maker := map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Action": "s3:CreateBucket", "Resource": "*",
	}}}
	f.must(http.StatusCreated, call{method: "POST", path: "/policies", body: map[string]any{"name": "maker", "document": maker}})
	f.must(http.StatusCreated, call{method: "POST", path: "/roles", body: map[string]any{"name": "maker", "policies": []string{"maker"}}})
	bearer := f.userKey("maker", "maker")
	create := call{method: "POST", path: "/buckets", bearer: bearer, body: map[string]any{"name": "some-bucket"}, headers: map[string]string{"Idempotency-Key": "make-only"}}
	first := f.do(create)
	again := f.do(create)
	var made struct {
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"created_at"`
	}
	if first.status != http.StatusCreated || again.status != http.StatusCreated || again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(first.body, again.body) {
		t.Fatalf("a creation by a caller who may only create = %d %s, its repeat %d %s", first.status, first.body, again.status, again.body)
	}
	first.decode(t, &made)
	if made.Name != "some-bucket" || !made.CreatedAt.Equal(testNow) {
		t.Errorf("the creation answered %s", first.body)
	}
	if r := f.do(withBearer(call{method: "GET", path: "/buckets/some-bucket"}, bearer)); r.status != http.StatusForbidden {
		t.Errorf("the caller may list the bucket after all: %d", r.status)
	}

	// Reads of buckets fail from the creation on: the answer is the same.
	f.meta.FailGetBucket = fmt.Errorf("%w: reads", domain.ErrUnavailable)
	r := f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "other-bucket"}})
	f.meta.FailGetBucket = nil
	if r.decode(t, &made); made.Name != "other-bucket" {
		t.Errorf("the creation while reads fail answered %s", r.body)
	}
}

// TestABucketWhoseRulesCannotBeMadeIsNotMade: when the second entry of a
// creation fails, the caller is told why and the bucket is not there; the
// same request is made whole when it is repeated.
func TestABucketWhoseRulesCannotBeMadeIsNotMade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.meta.FailConfig = map[string]error{bucket.ConfigTagRules: fmt.Errorf("%w: the rules entry", domain.ErrUnavailable)}
	create := call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}, headers: map[string]string{"Idempotency-Key": "made-once"}}
	r := f.must(http.StatusServiceUnavailable, create)
	if p := r.problem(t); p["code"] != "ServiceUnavailable" {
		t.Errorf("a creation whose rules were not made: problem = %s", r.body)
	}
	if p := f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/some-bucket"}).problem(t); p["code"] != "NoSuchBucket" {
		t.Errorf("the bucket whose rules were not made: %v", p)
	}
	var undone []audit.Record
	for _, r := range f.trail() {
		if r.Action == string(iam.ActionCreateBucket) {
			undone = append(undone, r)
		}
	}
	if len(undone) != 1 || undone[0].Outcome != audit.Failed || undone[0].Details["undone"] != "true" || undone[0].Details["tag_rules"] != "failed" {
		t.Errorf("the records of the creation taken back = %+v", undone)
	}
	f.meta.FailConfig = nil
	f.must(http.StatusCreated, create)
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/tag-rules"}); !bytes.Contains(r.body, []byte(`"lowercase":true`)) {
		t.Errorf("the rules of the bucket made on the second try = %s", r.body)
	}
}

// TestABucketThatCannotBeTakenBackIsMade: when the second entry of a
// creation fails and the bucket cannot be deleted again, the bucket is
// there - so the caller is answered 201, the repeat with the same
// Idempotency-Key is answered the same, and the bucket has no tag rules;
// the trail records the creation with tag_rules=not_set.
func TestABucketThatCannotBeTakenBackIsMade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	start := len(f.trail())
	f.meta.FailConfig = map[string]error{bucket.ConfigTagRules: fmt.Errorf("%w: the rules entry", domain.ErrUnavailable)}
	f.meta.FailDeleteBucket = fmt.Errorf("%w: lost quorum", domain.ErrUnavailable)
	create := call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}, headers: map[string]string{"Idempotency-Key": "made-anyway"}}
	first := f.must(http.StatusCreated, create)
	again := f.must(http.StatusCreated, create)
	if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
		t.Errorf("the repeat of the creation: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	f.meta.FailConfig, f.meta.FailDeleteBucket = nil, nil
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket"})
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/tag-rules"}); strings.TrimSpace(string(r.body)) != `{"catalogue_only":false,"lowercase":false,"required":[]}` {
		t.Errorf("the rules of a bucket made without them = %s", r.body)
	}
	var made []audit.Record
	for _, r := range f.trail()[start:] {
		if r.Action == string(iam.ActionCreateBucket) {
			made = append(made, r)
		}
	}
	if len(made) != 1 || made[0].Outcome != audit.Succeeded || made[0].Details["tag_rules"] != "not_set" {
		t.Errorf("the records of the creation = %+v", made)
	}
	if !strings.Contains(f.logs.String(), `"level":"ERROR"`) || !strings.Contains(f.logs.String(), `"bucket":"some-bucket"`) {
		t.Errorf("the log does not say the bucket was made without its rule: %s", f.logs.String())
	}
}

// TestBucketTags: GET, PUT and DELETE /buckets/{bucket}/tags, the tags S3's
// GetBucketTagging and PutBucketTagging read and write.
func TestBucketTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	path := "/buckets/some-bucket/tags"
	start := len(f.trail())

	// None yet: 200, and an empty list.
	if got := tagsOf(t, f.must(http.StatusOK, call{method: "GET", path: path})); len(got.Tags) != 0 || got.VersionID != nil {
		t.Errorf("the tags of a bucket without = %+v", got)
	}

	// Set, with a key that makes it safe to repeat: kept in lower case, as
	// the bucket's rules say, and answered in key order. The repeat is the
	// answer that was given, and the change is recorded once.
	put := call{method: "PUT", path: path, body: map[string]any{"tags": tagList("Team", "Finance", "stage", "draft")}, headers: map[string]string{"Idempotency-Key": "tags-1"}}
	first := f.must(http.StatusOK, put)
	if got := tagsOf(t, first).pairs(); !slices.Equal(got, []string{"stage", "draft", "team", "finance"}) {
		t.Errorf("PUT answered %v", got)
	}
	again := f.must(http.StatusOK, put)
	if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) || first.header.Get("Idempotent-Replayed") != "" {
		t.Errorf("the repeat of a PUT: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	if n := succeeded(f, start, "s3:PutBucketTagging", "some-bucket"); n != 1 {
		t.Errorf("a PUT sent twice with one key was recorded %d times", n)
	}
	read := f.must(http.StatusOK, call{method: "GET", path: path})
	if !bytes.Equal(read.body, first.body) {
		t.Errorf("GET after PUT = %s, want %s", read.body, first.body)
	}

	// Tags that cannot be kept: 400 InvalidArgument, and the tags the
	// bucket had are kept. A key given twice reaches the domain and is
	// refused there, whatever its values; so are two keys that are one in
	// lower case.
	refused := map[string]map[string]any{
		"a key given twice":            {"tags": tagList("a", "1", "a", "1")},
		"two keys one in lower case":   {"tags": tagList("A", "1", "a", "2")},
		"a key no tag can have":        {"tags": tagList("a&b", "1")},
		"a body without tags":          {},
		"a body whose tags are null":   {"tags": nil},
		"more tags than a bucket has":  {"tags": tagList(slices.Repeat([]string{"k", "v"}, 51)...)},
		"a value that is too long ...": {"tags": tagList("k", strings.Repeat("v", 257))},
	}
	for name, body := range refused {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: path, body: body})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); !bytes.Equal(r.body, first.body) {
		t.Errorf("the tags after refused ones = %s, want %s", r.body, first.body)
	}

	// Removed, with a key: the repeat is the answer that was given.
	del := call{method: "DELETE", path: path, headers: map[string]string{"Idempotency-Key": "tags-2"}}
	f.must(http.StatusNoContent, del)
	if again := f.must(http.StatusNoContent, del); again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a DELETE was not replayed")
	}
	if n := succeeded(f, start, "s3:PutBucketTagging", "some-bucket"); n != 2 {
		t.Errorf("a PUT and a DELETE sent twice with one key each were recorded %d times", n)
	}
	if got := tagsOf(t, f.must(http.StatusOK, call{method: "GET", path: path})); len(got.Tags) != 0 {
		t.Errorf("the tags after DELETE = %+v", got)
	}

	// Without the rule, tags are kept as they are sent.
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/some-bucket/tag-rules"})
	if got := tagsOf(t, f.must(http.StatusOK, call{method: "PUT", path: path, body: map[string]any{"tags": tagList("Team", "Finance")}})).pairs(); !slices.Equal(got, []string{"Team", "Finance"}) {
		t.Errorf("PUT without the rule answered %v", got)
	}

	// No such bucket: 404 NoSuchBucket, to a caller who may ask.
	for _, c := range []call{
		{method: "GET", path: "/buckets/no-such-bucket/tags"},
		{method: "PUT", path: "/buckets/no-such-bucket/tags", body: someTags()},
		{method: "DELETE", path: "/buckets/no-such-bucket/tags"},
	} {
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchBucket" {
			t.Errorf("%s %s: problem = %v", c.method, c.path, p)
		}
	}
}

// TestBucketTagRules: GET, PUT and DELETE /buckets/{bucket}/tag-rules.
func TestBucketTagRules(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	path := "/buckets/some-bucket/tag-rules"
	start := len(f.trail())

	rules := map[string]any{
		"lowercase": true, "catalogue_only": false,
		"required": []any{
			map[string]any{"key": "stage", "values": []string{"draft", "final"}},
			map[string]any{"key": "owner", "sub_bucket": "reports"},
		},
	}
	want := `{"catalogue_only":false,"lowercase":true,"required":[{"key":"stage","values":["draft","final"]},{"key":"owner","sub_bucket":"reports"}]}`
	put := call{method: "PUT", path: path, body: rules, headers: map[string]string{"Idempotency-Key": "rules-1"}}
	first := f.must(http.StatusOK, put)
	if strings.TrimSpace(string(first.body)) != want {
		t.Errorf("PUT answered %s, want %s", first.body, want)
	}
	again := f.must(http.StatusOK, put)
	if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
		t.Errorf("the repeat of a PUT: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	if n := succeeded(f, start, string(iam.ActionPutBucketTagRules), "some-bucket"); n != 1 {
		t.Errorf("a PUT sent twice with one key was recorded %d times", n)
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); strings.TrimSpace(string(r.body)) != want {
		t.Errorf("GET after PUT = %s, want %s", r.body, want)
	}

	// Rules that cannot be kept, and bodies without what a PUT must give:
	// 400 InvalidArgument, and the rules the bucket had are kept.
	refused := []map[string]any{
		{"lowercase": true, "catalogue_only": false, "required": []any{map[string]any{"key": "Stage"}}},
		{"lowercase": false, "catalogue_only": false, "required": []any{map[string]any{"key": "a&b"}}},
		{"lowercase": false, "catalogue_only": false, "required": []any{map[string]any{"key": "a"}, map[string]any{"key": "a"}}},
		{},
		{"lowercase": true, "required": []any{}},
		{"catalogue_only": true, "required": []any{}},
		{"lowercase": true, "catalogue_only": true},
		{"lowercase": true, "catalogue_only": true, "required": nil},
	}
	for _, body := range refused {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: path, body: body})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("rules %v: problem = %s", body, r.body)
		}
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); strings.TrimSpace(string(r.body)) != want {
		t.Errorf("the rules after refused ones = %s", r.body)
	}

	// Removed, with a key: all off.
	del := call{method: "DELETE", path: path, headers: map[string]string{"Idempotency-Key": "rules-2"}}
	f.must(http.StatusNoContent, del)
	if again := f.must(http.StatusNoContent, del); again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a DELETE was not replayed")
	}
	if n := succeeded(f, start, string(iam.ActionPutBucketTagRules), "some-bucket"); n != 2 {
		t.Errorf("a PUT and a DELETE sent twice with one key each were recorded %d times", n)
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); strings.TrimSpace(string(r.body)) != `{"catalogue_only":false,"lowercase":false,"required":[]}` {
		t.Errorf("the rules after DELETE = %s", r.body)
	}

	for _, c := range []call{
		{method: "GET", path: "/buckets/no-such-bucket/tag-rules"},
		{method: "PUT", path: "/buckets/no-such-bucket/tag-rules", body: map[string]any{"lowercase": true, "catalogue_only": false, "required": []any{}}},
		{method: "DELETE", path: "/buckets/no-such-bucket/tag-rules"},
	} {
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchBucket" {
			t.Errorf("%s %s: problem = %v", c.method, c.path, p)
		}
	}
}

// TestObjectTags: GET, PUT and DELETE /buckets/{bucket}/object/tags, the
// tags S3's GetObjectTagging and PutObjectTagging read and write, of the
// current version or of one named.
func TestObjectTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=k", raw: []byte("content")})
	path := "/buckets/some-bucket/object/tags?key=k"

	got := tagsOf(t, f.must(http.StatusOK, call{method: "GET", path: path}))
	if len(got.Tags) != 0 || got.VersionID == nil || *got.VersionID != object.NullVersion {
		t.Errorf("the tags of a version without = %+v", got)
	}

	// Set, in lower case as the bucket's rules say, answered in key order;
	// the repeat with its key is the answer that was given.
	put := call{method: "PUT", path: path, body: map[string]any{"tags": tagList("Team", "Finance", "stage", "draft")}, headers: map[string]string{"Idempotency-Key": "object-tags-1"}}
	first := f.must(http.StatusOK, put)
	if got := tagsOf(t, first); !slices.Equal(got.pairs(), []string{"stage", "draft", "team", "finance"}) || got.VersionID == nil || *got.VersionID != "null" {
		t.Errorf("PUT answered %s", first.body)
	}
	if again := f.must(http.StatusOK, put); again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
		t.Errorf("the repeat of a PUT: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	for _, p := range []string{path, path + "&version_id=null"} {
		if r := f.must(http.StatusOK, call{method: "GET", path: p}); !bytes.Equal(r.body, first.body) {
			t.Errorf("GET %s after PUT = %s, want %s", p, r.body, first.body)
		}
	}

	// Tags that cannot be kept: 400 InvalidArgument, and the tags are kept.
	for name, body := range map[string]map[string]any{
		"a key given twice":          {"tags": tagList("a", "1", "a", "2")},
		"two keys one in lower case": {"tags": tagList("A", "1", "a", "2")},
		"more than ten":              {"tags": tagList(slices.Repeat([]string{"k", "v"}, 11)...)},
		"a body whose tags are null": {"tags": nil},
	} {
		if p := f.must(http.StatusBadRequest, call{method: "PUT", path: path, body: body}).problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("%s: problem = %v", name, p)
		}
	}

	// The bucket's rules: a tag they require missing, or with a value it
	// may not have, is refused 400 TagRuleViolated, whose detail names the
	// rule and never a value; removing the tags of an object that must
	// have one is refused too.
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/tag-rules", body: map[string]any{
		"lowercase": true, "catalogue_only": false, "required": []any{map[string]any{"key": "stage", "values": []string{"draft", "final"}}},
	}})
	for name, c := range map[string]call{
		"a required tag missing":   {method: "PUT", path: path, body: map[string]any{"tags": tagList("team", "secret-value")}},
		"a value it may not have":  {method: "PUT", path: path, body: map[string]any{"tags": tagList("stage", "secret-value")}},
		"the tags removed by PUT":  {method: "PUT", path: path, body: map[string]any{"tags": []any{}}},
		"the tags removed, DELETE": {method: "DELETE", path: path},
	} {
		r := f.must(http.StatusBadRequest, c)
		p := r.problem(t)
		if detail, _ := p["detail"].(string); p["code"] != "TagRuleViolated" || detail == "" || strings.Contains(detail, "secret-value") {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); !bytes.Equal(r.body, first.body) {
		t.Errorf("the tags after refused ones = %s, want %s", r.body, first.body)
	}

	// Removed where the rules allow it, with a key.
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/some-bucket/tag-rules"})
	del := call{method: "DELETE", path: path, headers: map[string]string{"Idempotency-Key": "object-tags-2"}}
	f.must(http.StatusNoContent, del)
	if again := f.must(http.StatusNoContent, del); again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a DELETE was not replayed")
	}
	if got := tagsOf(t, f.must(http.StatusOK, call{method: "GET", path: path})); len(got.Tags) != 0 {
		t.Errorf("the tags after DELETE = %+v", got)
	}

	// A version that is named: one there is not is 404 NoSuchVersion, a
	// name that is none 400, and a delete marker - which has no tags - 405.
	if err := f.meta.SetBucketVersioning(t.Context(), "some-bucket", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/some-bucket/object?key=k"})
	var marker string
	if err := f.meta.ScanVersions(t.Context(), "some-bucket", ports.VersionCursor{}, func(v object.Version, latest bool) ports.ScanControl {
		if v.DeleteMarker && latest {
			marker = v.Ref().String()
		}
		return ports.ScanControl{}
	}); err != nil || marker == "" {
		t.Fatalf("no delete marker: %v", err)
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		c := call{method: method, path: path + "&version_id=" + marker}
		if method == "PUT" {
			c.body = someTags()
		}
		if p := f.must(http.StatusMethodNotAllowed, c).problem(t); p["code"] != "MethodNotAllowed" {
			t.Errorf("%s of the tags of a delete marker: problem = %v", method, p)
		}
		c.path = path + "&version_id=" + strings.Repeat("0", 31) + "1"
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchVersion" {
			t.Errorf("%s of the tags of no version: problem = %v", method, p)
		}
		c.path = path + "&version_id=not-a-version"
		if p := f.must(http.StatusBadRequest, c).problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("%s of the tags of a version named wrongly: problem = %v", method, p)
		}
		c.path = "/buckets/some-bucket/object/tags?key=k"
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchObject" {
			t.Errorf("%s of the tags of an object whose current version is a delete marker: problem = %v", method, p)
		}
		c.path = "/buckets/no-such-bucket/object/tags?key=k"
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchBucket" {
			t.Errorf("%s of the tags in no bucket: problem = %v", method, p)
		}
	}
}

// TestTagDefinitions: the catalogue of tags, /tag-definitions[/{key}].
func TestTagDefinitions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	start := len(f.trail())

	if r := f.must(http.StatusOK, call{method: "GET", path: "/tag-definitions"}); strings.TrimSpace(string(r.body)) != "[]" {
		t.Errorf("an empty catalogue = %s", r.body)
	}

	// Defined, with a key: 201, and the repeat is the answer that was
	// given, recorded once. A key defined already is 409.
	create := call{method: "POST", path: "/tag-definitions", body: map[string]any{
		"key": "stage", "description": "How far a document is.", "values": []string{"draft", "final"},
	}, headers: map[string]string{"Idempotency-Key": "define-1"}}
	first := f.must(http.StatusCreated, create)
	var stage struct {
		Key         string    `json:"key"`
		Description string    `json:"description"`
		Values      []string  `json:"values"`
		CreatedAt   time.Time `json:"created_at"`
	}
	first.decode(t, &stage)
	if stage.Key != "stage" || stage.Description != "How far a document is." || !slices.Equal(stage.Values, []string{"draft", "final"}) || stage.CreatedAt.IsZero() {
		t.Errorf("POST answered %s", first.body)
	}
	if again := f.must(http.StatusCreated, create); again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
		t.Errorf("the repeat of a POST: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	if n := adminRecords(f, start, string(iam.ActionPutTagDefinition), "tag-definition/stage"); n != 1 {
		t.Errorf("a POST sent twice with one key was recorded %d times", n)
	}
	if p := f.must(http.StatusConflict, call{method: "POST", path: "/tag-definitions", body: map[string]any{"key": "stage"}}).problem(t); p["code"] != "EntityAlreadyExists" {
		t.Errorf("a key defined already: problem = %v", p)
	}
	// Without values the tag may have any: the list is empty, not null.
	if r := f.must(http.StatusCreated, call{method: "POST", path: "/tag-definitions", body: map[string]any{"key": "team"}}); !bytes.Contains(r.body, []byte(`"values":[]`)) {
		t.Errorf("a definition without values = %s", r.body)
	}

	// Definitions that cannot be kept: 400 InvalidArgument.
	for name, c := range map[string]call{
		"a key in upper case":       {method: "POST", path: "/tag-definitions", body: map[string]any{"key": "Stage"}},
		"a key no tag can have":     {method: "POST", path: "/tag-definitions", body: map[string]any{"key": "a&b"}},
		"a body without a key":      {method: "POST", path: "/tag-definitions", body: map[string]any{"description": "no key"}},
		"a value in upper case":     {method: "PUT", path: "/tag-definitions/stage", body: map[string]any{"description": "", "values": []string{"Draft"}}},
		"a PUT without values":      {method: "PUT", path: "/tag-definitions/stage", body: map[string]any{"description": "x"}},
		"a PUT without description": {method: "PUT", path: "/tag-definitions/stage", body: map[string]any{"values": []string{}}},
		"a PUT of nothing":          {method: "PUT", path: "/tag-definitions/stage", body: map[string]any{}},
	} {
		if p := f.must(http.StatusBadRequest, c).problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("%s: problem = %v", name, p)
		}
	}

	// Changed: the time it was first defined is kept.
	put := call{method: "PUT", path: "/tag-definitions/stage", body: map[string]any{"description": "Changed.", "values": []string{}}, headers: map[string]string{"Idempotency-Key": "define-2"}}
	changed := f.must(http.StatusOK, put)
	var after struct {
		Description string    `json:"description"`
		Values      []string  `json:"values"`
		CreatedAt   time.Time `json:"created_at"`
	}
	changed.decode(t, &after)
	if after.Description != "Changed." || after.Values == nil || len(after.Values) != 0 || !after.CreatedAt.Equal(stage.CreatedAt) {
		t.Errorf("PUT answered %s", changed.body)
	}
	if again := f.must(http.StatusOK, put); again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, changed.body) {
		t.Errorf("the repeat of a PUT: replayed %q", again.header.Get("Idempotent-Replayed"))
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: "/tag-definitions/stage"}); !bytes.Equal(r.body, changed.body) {
		t.Errorf("GET after PUT = %s, want %s", r.body, changed.body)
	}
	// A PUT of a key not defined defines it.
	f.must(http.StatusOK, call{method: "PUT", path: "/tag-definitions/owner", body: map[string]any{"description": "", "values": []string{}}})
	var listed []struct {
		Key string `json:"key"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/tag-definitions"}).decode(t, &listed)
	if len(listed) != 3 || listed[0].Key != "owner" || listed[1].Key != "stage" || listed[2].Key != "team" {
		t.Errorf("the catalogue = %+v, want three in key order", listed)
	}

	// A bucket that takes only the catalogue's tags.
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=k", raw: []byte("content")})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/tag-rules", body: map[string]any{
		"lowercase": true, "catalogue_only": true, "required": []any{map[string]any{"key": "stage"}},
	}})
	if p := f.must(http.StatusBadRequest, call{method: "PUT", path: "/buckets/some-bucket/object/tags?key=k", body: map[string]any{"tags": tagList("stage", "draft", "colour", "red")}}).problem(t); p["code"] != "TagRuleViolated" {
		t.Errorf("a tag outside the catalogue: problem = %v", p)
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object/tags?key=k", body: map[string]any{"tags": tagList("stage", "anything", "team", "web")}})

	// A definition a rule requires cannot be removed: 409 EntityInUse,
	// with a detail that names no bucket.
	r := f.must(http.StatusConflict, call{method: "DELETE", path: "/tag-definitions/stage"})
	if p := r.problem(t); p["code"] != "EntityInUse" || bytes.Contains(r.body, []byte("some-bucket")) {
		t.Errorf("removing a definition a rule requires: problem = %s", r.body)
	}
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/some-bucket/tag-rules"})
	del := call{method: "DELETE", path: "/tag-definitions/stage", headers: map[string]string{"Idempotency-Key": "define-3"}}
	f.must(http.StatusNoContent, del)
	if again := f.must(http.StatusNoContent, del); again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a DELETE was not replayed")
	}
	if n := adminRecords(f, start, string(iam.ActionDeleteTagDefinition), "tag-definition/stage"); n != 1 {
		t.Errorf("a DELETE sent twice with one key was recorded %d times", n)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if p := f.must(http.StatusNotFound, call{method: method, path: "/tag-definitions/stage"}).problem(t); p["code"] != "NoSuchEntity" {
			t.Errorf("%s of a key not defined: problem = %v", method, p)
		}
	}
}

// TestTagOperationsAreAuthorized: 401 without a credential, and 403 to a
// caller without the action: read-only holds none of the tags' and reads
// the catalogue only.
func TestTagOperationsAreAuthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=k", raw: []byte("content")})
	f.must(http.StatusCreated, call{method: "POST", path: "/tag-definitions", body: map[string]any{"key": "stage"}})
	reader := f.userKey("reader", iam.RoleReadOnly)
	readable := map[string]int{"listTagDefinitions": http.StatusOK, "getTagDefinition": http.StatusOK}
	before := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/tag-rules"})

	for _, op := range tagOperations() {
		for name, bearer := range map[string]string{"no credential": "", "a credential that is none": "AKIDNOBODY:not-a-secret"} {
			asked := op.c
			asked.bearer = bearer
			if r := f.do(asked); r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
				t.Errorf("%s with %s = %d: %s", op.id, name, r.status, r.body)
			}
		}
		asked := op.c
		asked.bearer = reader
		r := f.do(asked)
		if want, ok := readable[op.id]; ok {
			if r.status != want {
				t.Errorf("%s by read-only = %d, want %d: %s", op.id, r.status, want, r.body)
			}
			continue
		}
		if r.status != http.StatusForbidden || r.problem(t)["code"] != "AccessDenied" {
			t.Errorf("%s by read-only = %d: %s", op.id, r.status, r.body)
		}
	}
	// Nothing was changed.
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/tag-rules"}); !bytes.Equal(r.body, before.body) {
		t.Errorf("the rules after refused changes = %s", r.body)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/tag-definitions/stage"})
	f.must(http.StatusNotFound, call{method: "GET", path: "/tag-definitions/team"})
}

// TestTagChangesNeedTheCSRFTokenOfASession: a console session's change
// without its CSRF token is refused 403 before anything else, and with it is
// made.
func TestTagChangesNeedTheCSRFTokenOfASession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=k", raw: []byte("content")})
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "dora", "roles": []string{iam.RoleClusterAdmin}, "password": password,
	}})
	client, csrf, signedIn := signIn(t, f, "dora", password)
	if signedIn.status != http.StatusOK || csrf == "" {
		t.Fatalf("signing in = %d", signedIn.status)
	}
	want := map[string]int{
		"getBucketTags": http.StatusOK, "putBucketTags": http.StatusOK, "deleteBucketTags": http.StatusNoContent,
		"getObjectTags": http.StatusOK, "putObjectTags": http.StatusOK, "deleteObjectTags": http.StatusNoContent,
		"getBucketTagRules": http.StatusOK, "putBucketTagRules": http.StatusOK, "deleteBucketTagRules": http.StatusNoContent,
		"listTagDefinitions": http.StatusOK, "createTagDefinition": http.StatusCreated,
		"putTagDefinition": http.StatusOK, "getTagDefinition": http.StatusOK, "deleteTagDefinition": http.StatusNoContent,
	}
	// In an order in which each is made: the definition put before it is
	// read and removed.
	operations := tagOperations()
	order := []string{
		"getBucketTags", "putBucketTags", "deleteBucketTags", "getObjectTags", "putObjectTags", "deleteObjectTags",
		"getBucketTagRules", "putBucketTagRules", "deleteBucketTagRules",
		"listTagDefinitions", "createTagDefinition", "putTagDefinition", "getTagDefinition", "deleteTagDefinition",
	}
	if len(order) != len(operations) {
		t.Fatalf("%d operations in order, %d operations", len(order), len(operations))
	}
	for n, id := range order {
		i := slices.IndexFunc(operations, func(op tagOperation) bool { return op.id == id })
		c := operations[i].c
		c.client = client
		if c.method != http.MethodGet {
			for _, keyed := range []bool{false, true} {
				c.headers = map[string]string{}
				if keyed {
					c.headers["Idempotency-Key"] = fmt.Sprintf("csrf-%d", n)
				}
				r := f.do(c)
				if r.status != http.StatusForbidden || r.problem(t)["code"] != "CSRFTokenInvalid" {
					t.Errorf("%s (keyed: %v) without the CSRF token = %d: %s", id, keyed, r.status, r.body)
				}
			}
			c.headers = map[string]string{httpsession.CSRFHeader: csrf}
		}
		if r := f.do(c); r.status != want[id] {
			t.Errorf("%s with the CSRF token = %d, want %d: %s", id, r.status, want[id], r.body)
		}
	}
}

// TestTheCatalogueOfANodeWithoutTheUseCase: a node that was not given the
// use case of the catalogue answers 501 to a caller it has authenticated,
// and 401 to one it has not.
func TestTheCatalogueOfANodeWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.TagDefinitions = nil })
	for _, op := range tagOperations() {
		if !strings.HasPrefix(op.c.path, "/tag-definitions") {
			continue
		}
		if r := f.do(op.c); r.status != http.StatusUnauthorized {
			t.Errorf("%s without a credential = %d", op.id, r.status)
		}
		if p := f.must(http.StatusNotImplemented, op.c).problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
			t.Errorf("%s: problem = %v", op.id, p)
		}
	}
}

// TestAnUploadWithTags: X-CactusFS-Tagging gives the version its tags, as
// S3's x-amz-tagging does - kept as the bucket's rules say, refused where
// they do not allow them, and asked as s3:PutObjectTagging as well. The
// tags are part of what makes two uploads with one Idempotency-Key the same
// request.
func TestAnUploadWithTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	upload := func(key, tagging string) call {
		return call{method: "PUT", path: "/buckets/some-bucket/object?key=" + key, raw: []byte("content"), headers: map[string]string{"X-CactusFS-Tagging": tagging}}
	}
	tagsAt := func(key string) []string {
		return tagsOf(t, f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/object/tags?key=" + key})).pairs()
	}
	stored := func(key string) bool {
		return f.do(withBearer(call{method: "GET", path: "/buckets/some-bucket/object/metadata?key=" + key}, f.root)).status == http.StatusOK
	}

	// Stored with its tags, lower-cased as the bucket made here keeps them.
	f.must(http.StatusOK, upload("tagged", "Stage=Draft&team=finance%20dept"))
	if got := tagsAt("tagged"); !slices.Equal(got, []string{"stage", "draft", "team", "finance dept"}) {
		t.Errorf("the tags of an upload = %v", got)
	}
	// An empty header is no tags.
	f.must(http.StatusOK, upload("empty", ""))
	if got := tagsAt("empty"); len(got) != 0 {
		t.Errorf("the tags of an upload with an empty header = %v", got)
	}

	// Tags that cannot be kept: 400, and nothing is stored.
	for name, tagging := range map[string]string{
		"a key given twice":        "a=1&a=2",
		"no query string":          "a=%zz",
		"a semicolon":              "a=1;b=2",
		"more than ten":            strings.Repeat("k=v&", 10) + "k=v",
		"a key no tag can have":    "a%26b=1",
		"two keys one lower-cased": "A=1&a=2",
	} {
		r := f.do(withBearer(upload("refused", tagging), f.root))
		if r.status != http.StatusBadRequest || r.problem(t)["code"] != "InvalidArgument" {
			t.Errorf("an upload with %s = %d %s", name, r.status, r.body)
		}
	}
	if stored("refused") {
		t.Error("an upload whose tags cannot be kept stored something")
	}

	// Under a rule that requires a tag: refused without it, or with a
	// value it may not have - TagRuleViolated, nothing stored - and made
	// with it.
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/tag-rules", body: map[string]any{
		"lowercase": true, "catalogue_only": false, "required": []any{map[string]any{"key": "stage", "values": []string{"draft", "final"}}},
	}})
	for name, c := range map[string]call{
		"without tags":     {method: "PUT", path: "/buckets/some-bucket/object?key=ruled", raw: []byte("content")},
		"without the tag":  upload("ruled", "team=web"),
		"with a bad value": upload("ruled", "stage=secret-value"),
	} {
		r := f.do(withBearer(c, f.root))
		p := r.problem(t)
		if detail, _ := p["detail"].(string); r.status != http.StatusBadRequest || p["code"] != "TagRuleViolated" || strings.Contains(detail, "secret-value") {
			t.Errorf("an upload %s under the rule = %d %s", name, r.status, r.body)
		}
	}
	if stored("ruled") {
		t.Error("an upload against the rules stored something")
	}
	f.must(http.StatusOK, upload("ruled", "STAGE=Final"))
	if got := tagsAt("ruled"); !slices.Equal(got, []string{"stage", "final"}) {
		t.Errorf("the tags of an upload under the rule = %v", got)
	}
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/some-bucket/tag-rules"})

	// Tagging is a permission of its own: read-write may store objects and
	// not tag them.
	writer := f.userKey("writer", iam.RoleReadWrite)
	if r := f.do(withBearer(upload("by-writer", "stage=draft"), writer)); r.status != http.StatusForbidden || r.problem(t)["code"] != "AccessDenied" {
		t.Errorf("an upload with tags by read-write = %d %s", r.status, r.body)
	}
	if stored("by-writer") {
		t.Error("an upload refused its tags stored something")
	}
	f.do(withBearer(call{method: "PUT", path: "/buckets/some-bucket/object?key=by-writer", raw: []byte("content")}, writer))
	if !stored("by-writer") {
		t.Error("read-write could not store an object without tags")
	}

	// The tags are part of what the key names: the repeat is answered from
	// the record, and the same key with other tags - or with none - is
	// another request.
	keyed := func(tagging string, set bool) call {
		headers := map[string]string{"Idempotency-Key": "upload-with-tags"}
		if set {
			headers["X-CactusFS-Tagging"] = tagging
		}
		return call{method: "PUT", path: "/buckets/some-bucket/object?key=keyed", raw: []byte("content"), headers: headers}
	}
	first := f.must(http.StatusOK, keyed("stage=draft", true))
	if again := f.must(http.StatusOK, keyed("stage=draft", true)); again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
		t.Error("the repeat of an upload with tags was not answered from the record")
	}
	for name, c := range map[string]call{"other tags": keyed("stage=final", true), "no tags": keyed("", false), "an empty tagging": keyed("", true)} {
		if p := f.must(http.StatusUnprocessableEntity, c).problem(t); p["code"] != "IdempotencyKeyReused" {
			t.Errorf("the same key with %s: problem = %v", name, p)
		}
	}
	if got := tagsAt("keyed"); !slices.Equal(got, []string{"stage", "draft"}) {
		t.Errorf("the tags of the upload made once = %v", got)
	}
}
