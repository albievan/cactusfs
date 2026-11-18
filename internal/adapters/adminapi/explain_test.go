package adminapi_test

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// explainPath is the path of an explanation asked with query.
func explainPath(query url.Values) string {
	return "/access/explain?" + query.Encode()
}

// grant makes a user whose one role holds one policy of these statements,
// and returns a bearer credential of theirs.
func grant(t *testing.T, f *fixture, name string, statements ...map[string]any) string {
	t.Helper()
	document := map[string]any{"Version": "2012-10-17", "Statement": statements}
	f.must(http.StatusCreated, call{method: "POST", path: "/policies", body: map[string]any{"name": name, "document": document}})
	f.must(http.StatusCreated, call{method: "POST", path: "/roles", body: map[string]any{"name": name, "policies": []string{name}}})
	return f.userKey(name, name)
}

// allow is a statement that allows actions on resources.
func allow(actions []string, resources ...string) map[string]any {
	return map[string]any{"Effect": "Allow", "Action": actions, "Resource": resources}
}

// explainFixture is a fixture with the bucket some-bucket and its
// sub-bucket reports, the read-only user dora with an API key, and the
// disabled user gone.
func explainFixture(t *testing.T) (f *fixture, doraKey string) {
	t.Helper()
	f = newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/reports", body: map[string]any{}})
	doraKey = f.userKey("dora", iam.RoleReadOnly)
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "gone", "roles": []string{iam.RoleReadOnly}}})
	f.must(http.StatusOK, call{method: "PUT", path: "/users/gone", body: map[string]any{"roles": []string{iam.RoleReadOnly}, "groups": []string{}, "disabled": true}})
	return f, doraKey
}

// TestExplainAccessOverHTTP (ADR-0032, Decision 3): what the authorizer
// decides for a subject, and why, as root may read all of it; what is not
// a request is 400 with a fixed detail; a subject that is not there 404.
func TestExplainAccessOverHTTP(t *testing.T) {
	t.Parallel()
	f, doraKey := explainFixture(t)
	doraKeyID, _, _ := strings.Cut(doraKey, ":")
	q := func(pairs ...string) url.Values {
		v := url.Values{}
		for i := 0; i+1 < len(pairs); i += 2 {
			v.Set(pairs[i], pairs[i+1])
		}
		return v
	}
	read := q("user", "dora", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k")

	tests := []struct {
		name  string
		query url.Values
		check func(t *testing.T, e api.AccessExplanation)
	}{
		{"a user allowed by their policy", read, func(t *testing.T, e api.AccessExplanation) {
			if !e.Authenticates || e.Allowed == nil || !*e.Allowed || e.Source == nil || *e.Source != api.DecisionSourceIdentityPolicy ||
				e.Statements == nil || len(*e.Statements) == 0 || e.Subject.Kind != api.ExplainedSubjectKindUser || e.Subject.User == nil || *e.Subject.User != "dora" ||
				e.Request.Resource != "arn:aws:s3:::some-bucket/k" || e.Request.Action != "s3:GetObject" || !e.Assumed.SecureTransport ||
				e.Assumed.SourceIp != nil || !e.Assumed.Time.Equal(testNow) || len(e.NotConsidered) != 0 {
				t.Errorf("explanation = %+v", e)
			}
			if c := (*e.Statements)[0]; !c.Readable || c.Policy == nil || c.StatementIndex == nil || c.DocumentIndex == nil || c.Effect != api.CitedStatementEffect(iam.Allow) {
				t.Errorf("root's citation = %+v", c)
			}
		}},
		{
			"a user refused by default", q("user", "dora", "action", "s3:PutObject", "bucket", "some-bucket", "key", "k", "source_ip", "10.1.2.3", "secure_transport", "false"),
			func(t *testing.T, e api.AccessExplanation) {
				if !e.Authenticates || e.Allowed == nil || *e.Allowed || e.Assumed.SourceIp == nil || *e.Assumed.SourceIp != "10.1.2.3" || e.Assumed.SecureTransport {
					t.Errorf("explanation = %+v", e)
				}
			},
		},
		{"an API key", q("access_key_id", doraKeyID, "action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), func(t *testing.T, e api.AccessExplanation) {
			if e.Subject.Kind != api.ExplainedSubjectKindAccessKey || e.Subject.User == nil || *e.Subject.User != "dora" ||
				e.Subject.AccessKeyId == nil || *e.Subject.AccessKeyId != doraKeyID || e.Allowed == nil || !*e.Allowed {
				t.Errorf("explanation = %+v", e)
			}
		}},
		{"a caller without credentials", q("anonymous", "true", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), func(t *testing.T, e api.AccessExplanation) {
			if e.Subject.Kind != api.ExplainedSubjectKindAnonymous || e.Allowed == nil || *e.Allowed {
				t.Errorf("explanation = %+v", e)
			}
		}},
		{"a disabled user", q("user", "gone", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), func(t *testing.T, e api.AccessExplanation) {
			if e.Authenticates || e.NotAuthenticated == nil || *e.NotAuthenticated != "user_disabled" || e.Allowed != nil || e.Source != nil || e.Statements != nil {
				t.Errorf("explanation = %+v", e)
			}
		}},
		{"a preview", q("user", "dora", "action", "admin:PreviewObject", "bucket", "some-bucket", "key", "k"), func(t *testing.T, e api.AccessExplanation) {
			if !slices.Equal(e.NotConsidered, []api.AccessExplanationNotConsidered{"object_ownership"}) {
				t.Errorf("not considered = %v", e.NotConsidered)
			}
		}},
		{"an administrative action", q("user", "dora", "action", "admin:ListUsers", "admin_resource", "user"), func(t *testing.T, e api.AccessExplanation) {
			// read-only reads buckets and objects, not users.
			if e.Allowed == nil || *e.Allowed || e.Source == nil || *e.Source != api.DecisionSourceNone || e.Request.Resource != "arn:cactusfs:admin:::user" ||
				e.Statements == nil || len(*e.Statements) != 0 {
				t.Errorf("explanation = %+v", e)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e api.AccessExplanation
			f.must(http.StatusOK, call{method: "GET", path: explainPath(tt.query)}).decode(t, &e)
			tt.check(t, e)
		})
	}

	for name, tt := range map[string]struct {
		query        url.Values
		status       int
		code, detail string
	}{
		"no subject":                     {q("action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), 400, "InvalidArgument", "name one subject"},
		"two subjects":                   {q("user", "dora", "anonymous", "true", "action", "s3:GetObject", "bucket", "some-bucket"), 400, "InvalidArgument", "name one subject"},
		"a wildcard":                     {q("user", "dora", "action", "s3:Get*", "bucket", "some-bucket", "key", "k"), 400, "InvalidArgument", "not one of the catalogue"},
		"no address":                     {q("user", "dora", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k", "source_ip", "not-an-address"), 400, "InvalidArgument", "no IP address"},
		"a user not there":               {q("user", "nobody", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), 404, "NoSuchEntity", "not there"},
		"a key not there":                {q("access_key_id", "AKIDNOSUCHKEY", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), 404, "NoSuchEntity", "not there"},
		"no action":                      {q("user", "dora"), 400, "InvalidArgument", "action"},
		"an empty user beside anonymous": {q("user", "", "anonymous", "true", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k"), 400, "InvalidArgument", `"user" is given and empty`},
		"an empty key ID":                {q("access_key_id", "", "anonymous", "true", "action", "s3:GetObject", "bucket", "some-bucket"), 400, "InvalidArgument", `"access_key_id" is given and empty`},
		"an empty bucket":                {q("user", "dora", "action", "s3:ListAllMyBuckets", "bucket", ""), 400, "InvalidArgument", `"bucket" is given and empty`},
		"an empty key":                   {q("user", "dora", "action", "s3:ListBucket", "bucket", "some-bucket", "key", ""), 400, "InvalidArgument", `"key" is given and empty`},
		"an empty admin resource":        {q("user", "dora", "action", "admin:ListUsers", "admin_resource", ""), 400, "InvalidArgument", `"admin_resource" is given and empty`},
		"a prefix to a read":             {q("user", "dora", "action", "s3:GetObject", "bucket", "some-bucket", "key", "k", "list_prefix", "a"), 400, "InvalidArgument", "listing prefix"},
	} {
		r := f.do(call{method: "GET", path: explainPath(tt.query), bearer: f.root})
		if r.status != tt.status {
			t.Errorf("%s = %d, want %d: %s", name, r.status, tt.status, r.body)
			continue
		}
		p := r.problem(t)
		if detail, _ := p["detail"].(string); p["code"] != tt.code || !strings.Contains(detail, tt.detail) || strings.Contains(detail, "explain.") {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	if r := f.do(call{method: "GET", path: explainPath(read)}); r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
		t.Errorf("an explanation without a credential = %d %s", r.status, r.body)
	}
}

// TestExplainIsRefusedWithoutItsRights: a caller without admin:ExplainAccess
// on the subject is refused; so is one who may ask about the subject but
// may not read the bucket's policy, or a sub-bucket's, that the answer
// depends on - 403 without detail, audited as a refusal of
// admin:ExplainAccess. With the rights the answer is given, and a citation
// in a policy the caller may not read keeps only its source and effect.
func TestExplainIsRefusedWithoutItsRights(t *testing.T) {
	t.Parallel()
	f, _ := explainFixture(t)
	reader := f.userKey("reader", iam.RoleReadOnly)
	onUsers := allow([]string{"admin:ExplainAccess"}, "arn:cactusfs:admin:::user/*")
	subject := grant(t, f, "subject-only", onUsers)
	bucketToo := grant(t, f, "bucket-too", onUsers, allow([]string{"s3:GetBucketPolicy"}, "arn:aws:s3:::some-bucket"))
	both := grant(t, f, "both", onUsers, allow([]string{"s3:GetBucketPolicy", "s3:GetSubBucketPolicy"}, "arn:aws:s3:::some-bucket"))
	object := func(key string) string {
		return explainPath(url.Values{"user": {"dora"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {key}})
	}
	listUsers := explainPath(url.Values{"user": {"dora"}, "action": {"admin:ListUsers"}, "admin_resource": {"user"}})

	tests := []struct {
		name, actor, bearer, path, reason string
		want                              int
	}{
		{"no right to ask", "user/reader", reader, object("k"), "", http.StatusForbidden},
		{"no right to read the bucket's policy", "user/subject-only", subject, object("k"), "may not read the policy the answer depends on", http.StatusForbidden},
		{"an admin action needs no bucket's policy", "user/subject-only", subject, listUsers, "", http.StatusOK},
		{"the bucket's policy, outside any sub-bucket", "user/bucket-too", bucketToo, object("k"), "", http.StatusOK},
		{"no right to read the sub-bucket's policy", "user/bucket-too", bucketToo, object("reports/q3.csv"), "may not read the policy the answer depends on", http.StatusForbidden},
		{"one that could be a sub-bucket's but is none", "user/bucket-too", bucketToo, object("other/q3.csv"), "may not read the policy the answer depends on", http.StatusForbidden},
		{"both", "user/both", both, object("reports/q3.csv"), "", http.StatusOK},
	}
	for _, tt := range tests {
		from := len(f.trail())
		r := f.do(call{method: "GET", path: tt.path, bearer: tt.bearer})
		if tt.want == http.StatusForbidden {
			refusedWithoutDetail(t, tt.name, r)
			if !deniedIn(f, from, tt.actor, string(iam.ActionExplainAccess), tt.reason) {
				t.Errorf("%s: the refusal is not in the audit trail as one of admin:ExplainAccess", tt.name)
			}
			continue
		}
		if r.status != tt.want {
			t.Errorf("%s = %d, want %d: %s", tt.name, r.status, tt.want, r.body)
			continue
		}
		var e api.AccessExplanation
		r.decode(t, &e)
		// dora (read-only) may read the object and may not list users.
		if e.Allowed == nil || *e.Allowed != (tt.path != listUsers) || e.Statements == nil || (len(*e.Statements) == 0) != (tt.path == listUsers) {
			t.Errorf("%s: explanation = %s", tt.name, r.body)
			continue
		}
		// dora's policy is built in; none of these callers may read it.
		for _, c := range *e.Statements {
			if c.Readable || c.Policy != nil || c.Sid != nil || c.StatementIndex != nil || c.DocumentIndex != nil {
				t.Errorf("%s: a citation the caller may not read = %+v", tt.name, c)
			}
		}
	}
}

// TestExplainIsAudited (ADR-0032, Decision 3, step 7): every answer given
// after authorization - a decision, a subject that would not authenticate,
// a subject not there, a request that is none - is one record of
// admin:ExplainAccess in the trail, appended before it is answered, saying
// what was answered and never what a policy says.
func TestExplainIsAudited(t *testing.T) {
	t.Parallel()
	f, _ := explainFixture(t)
	tests := []struct {
		query   url.Values
		status  int
		answer  string
		outcome audit.Outcome
	}{
		{url.Values{"user": {"dora"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}}, 200, "decision", audit.Succeeded},
		{url.Values{"user": {"gone"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}}, 200, "not_authenticated", audit.Succeeded},
		{url.Values{"user": {"nobody"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}}, 404, "no_such_entity", audit.Failed},
		{url.Values{"user": {"dora"}, "action": {"s3:Get*"}, "bucket": {"some-bucket"}, "key": {"k"}}, 400, "invalid_argument", audit.Failed},
	}
	for _, tt := range tests {
		before, err := f.store.ListAudit(t.Context(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		r := f.do(call{method: "GET", path: explainPath(tt.query), bearer: f.root})
		if r.status != tt.status {
			t.Errorf("%v = %d, want %d: %s", tt.query, r.status, tt.status, r.body)
			continue
		}
		// Synchronously: in the trail by the time the answer is read.
		after, err := f.store.ListAudit(t.Context(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var mine []audit.Record
		for _, record := range after[len(before):] {
			if record.Action == string(iam.ActionExplainAccess) {
				mine = append(mine, record)
			}
		}
		if len(mine) != 1 || mine[0].Actor != "root" || mine[0].Outcome != tt.outcome || mine[0].Details["answer"] != tt.answer ||
			mine[0].Details["subject"] == "" || mine[0].RequestID != r.header.Get("X-Request-Id") {
			t.Errorf("%v: the records of the answer = %+v", tt.query, mine)
			continue
		}
		if text, _ := json.Marshal(mine[0]); strings.Contains(string(text), "Statement") || strings.Contains(string(text), "s3:Get*") {
			t.Errorf("%v: the record tells what was not asked for: %s", tt.query, text)
		}
	}
}

// TestExplainAnswersNothingItCannotRecord: an answer whose record cannot be
// appended is not given - 503 with Retry-After, or 500 when the store
// cannot say whether it was appended - and nothing of it is in the
// problem.
func TestExplainAnswersNothingItCannotRecord(t *testing.T) {
	t.Parallel()
	f, _ := explainFixture(t)
	tests := []struct {
		fault  error
		status int
		code   string
	}{
		{domain.ErrUnavailable, http.StatusServiceUnavailable, "ServiceUnavailable"},
		{errors.New("the disk is full"), http.StatusServiceUnavailable, "ServiceUnavailable"},
		{domain.ErrOutcomeUnknown, http.StatusInternalServerError, "InternalError"},
	}
	for _, tt := range tests {
		fault := tt.fault
		f.explainTrail.fail.Store(&fault)
		for _, query := range []url.Values{
			{"user": {"dora"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}},
			{"user": {"nobody"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}},
			{"user": {"gone"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}},
		} {
			r := f.do(call{method: "GET", path: explainPath(query), bearer: f.root})
			if r.status != tt.status {
				t.Errorf("%v with the trail failing (%v) = %d, want %d: %s", query, tt.fault, r.status, tt.status, r.body)
				continue
			}
			if p := r.problem(t); p["code"] != tt.code || p["detail"] != nil || strings.Contains(string(r.body), "allowed") || strings.Contains(string(r.body), "disk") {
				t.Errorf("%v: problem = %s", query, r.body)
			}
			if tt.status == http.StatusServiceUnavailable && r.header.Get("Retry-After") == "" {
				t.Errorf("%v: a 503 without Retry-After", query)
			}
		}
	}
	f.explainTrail.fail.Store(nil)
	f.must(http.StatusOK, call{method: "GET", path: explainPath(url.Values{"user": {"dora"}, "action": {"s3:GetObject"}, "bucket": {"some-bucket"}, "key": {"k"}})})
}

// constantsOf returns, sorted, the string values of the constants declared
// in the non-test Go files of dir that are of the named type, or - when
// typeName is empty - untyped and named with prefix.
func constantsOf(t *testing.T, dir, typeName, prefix string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				ident, typed := value.Type.(*ast.Ident)
				for i, n := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					match := (typeName != "" && typed && ident.Name == typeName) ||
						(typeName == "" && value.Type == nil && strings.HasPrefix(n.Name, prefix))
					literal, isLiteral := value.Values[i].(*ast.BasicLit)
					if !match || !isLiteral || literal.Kind != token.STRING {
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					values = append(values, text)
				}
			}
		}
	}
	slices.Sort(values)
	return values
}

// TestExplanationEnumsAreTheDomains: each enum of the contract that the
// policy builder answers with holds exactly the values the domain and the
// use cases can give, read from their source - a value the code gains must
// be added to the contract (values are only ever added to), and the
// contract names none the code cannot give.
func TestExplanationEnumsAreTheDomains(t *testing.T) {
	t.Parallel()
	type property struct {
		Enum  []string `json:"enum"`
		Items struct {
			Enum []string `json:"enum"`
		} `json:"items"`
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Enum       []string            `json:"enum"`
				Properties map[string]property `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	schemas := doc.Components.Schemas
	explanation := schemas["AccessExplanation"].Properties
	domainDir, appDir := filepath.Join("..", "..", "domain", "iam"), filepath.Join("..", "..", "app")
	tests := []struct {
		name           string
		contract, code []string
	}{
		{"DecisionSource, iam.Source", schemas["DecisionSource"].Enum, constantsOf(t, domainDir, "Source", "")},
		{"AccessExplanation.rule, iam.Rule", explanation["rule"].Enum, constantsOf(t, domainDir, "Rule", "")},
		{"CatalogueConditionKey.value_kind, iam.ValueKind", schemas["CatalogueConditionKey"].Properties["value_kind"].Enum, constantsOf(t, domainDir, "ValueKind", "")},
		{"AccessExplanation.not_authenticated, app.NotAuthenticated*", explanation["not_authenticated"].Enum, constantsOf(t, appDir, "", "NotAuthenticated")},
		{"AccessExplanation.not_considered, app.NotConsidered*", explanation["not_considered"].Items.Enum, constantsOf(t, appDir, "", "NotConsidered")},
		{"ExplainedSubject.kind, app.Subject*", schemas["ExplainedSubject"].Properties["kind"].Enum, constantsOf(t, appDir, "", "Subject")},
		{"StatementAnalysis.effect, iam.Effect", schemas["StatementAnalysis"].Properties["effect"].Enum, constantsOf(t, domainDir, "Effect", "")},
		{"CitedStatement.effect, iam.Effect", schemas["CitedStatement"].Properties["effect"].Enum, constantsOf(t, domainDir, "Effect", "")},
	}
	for _, tt := range tests {
		contract := slices.Sorted(slices.Values(tt.contract))
		if len(tt.code) == 0 || !slices.Equal(contract, tt.code) {
			t.Errorf("%s: the contract has %v, the code %v", tt.name, contract, tt.code)
		}
	}
}

// TestThePolicyBuilderOnANodeWithoutIt: a node that was not given the use
// cases of the policy builder answers each of its five operations 501
// NotImplemented without detail, as any operation whose use case it lacks.
func TestThePolicyBuilderOnANodeWithoutIt(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.PolicyTools, c.Explain = nil, nil })
	media := map[string]string{"Content-Type": "application/json"}
	for _, c := range []call{
		{method: "GET", path: "/policy-catalogue"},
		{method: "POST", path: "/policy-validation", raw: policyBody("document", identityValid, nil), headers: media},
		{method: "POST", path: "/buckets/some-bucket/policy/validation", raw: policyBody("document", bucketValid, nil), headers: media},
		{method: "POST", path: "/buckets/some-bucket/subbuckets/reports/policy/validation", raw: policyBody("policy", subValid, nil), headers: media},
		{method: "GET", path: "/access/explain?user=dora&action=s3:GetObject&bucket=some-bucket&key=k"},
	} {
		r := f.must(http.StatusNotImplemented, c)
		if p := r.problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
			t.Errorf("%s %s on a node without the use case: problem = %s", c.method, c.path, r.body)
		}
	}
}
