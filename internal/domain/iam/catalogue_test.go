package iam_test

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// packageConstant is a string constant of the package's own source.
type packageConstant struct {
	name, value string
	// action says that its type is iam.Action.
	action bool
}

// isActionExpr reports whether expr is the type Action or a conversion to it.
func isActionExpr(expr ast.Expr) bool {
	if call, ok := expr.(*ast.CallExpr); ok {
		expr = call.Fun
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "Action"
}

// packageConstants reads every string constant the package declares in its
// source, its tests left out. An action declared any other way than as a
// constant of the type Action with a string literal - a variable of the
// type, a conversion, a constant whose value is another expression or is
// repeated from the line before - fails the test: it would escape every test
// that walks them.
func packageConstants(t *testing.T) []packageConstant {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var constants []packageConstant
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		read, faults := readConstants(t, name, nil)
		for _, fault := range faults {
			t.Error(fault)
		}
		if len(faults) > 0 {
			t.FailNow()
		}
		constants = append(constants, read...)
	}
	return constants
}

// readConstants is packageConstants of one file - read from src, or from the
// file name when src is nil - with the declarations it cannot see through
// returned, not failed.
func readConstants(t *testing.T, name string, src any) (constants []packageConstant, faults []string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		general, ok := node.(*ast.GenDecl)
		if !ok || (general.Tok != token.CONST && general.Tok != token.VAR) {
			return true
		}
		// A constant without a type or a value repeats the line
		// before it, as iota does.
		repeatsAction := false
		for _, spec := range general.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			isAction := values.Type != nil && isActionExpr(values.Type)
			for _, value := range values.Values {
				isAction = isAction || isActionExpr(value)
			}
			if values.Type == nil && len(values.Values) == 0 {
				isAction = repeatsAction
			}
			repeatsAction = isAction
			for i, ident := range values.Names {
				if general.Tok == token.VAR {
					if isAction {
						faults = append(faults, name+": the action "+ident.Name+" is a variable")
					}
					continue
				}
				var literal *ast.BasicLit
				if i < len(values.Values) {
					literal, _ = values.Values[i].(*ast.BasicLit)
				}
				if literal == nil || literal.Kind != token.STRING {
					if isAction {
						faults = append(faults, name+": the action "+ident.Name+" is not a string literal")
					}
					continue
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				constants = append(constants, packageConstant{name: ident.Name, value: text, action: isAction})
			}
		}
		return false
	})
	return constants, faults
}

// TestActionsAreDeclaredInSight: packageConstants refuses every way of
// declaring an action it cannot read, and reads one it can.
func TestActionsAreDeclaredInSight(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, source string
		faults       int
	}{
		{"a constant of the type", `const A Action = "s3:A"`, 0},
		{"an untyped string", `const A = "s3:A"`, 0},
		{"a variable of the type", `var A Action = "s3:A"`, 1},
		{"a variable converted", `var A = Action("s3:A")`, 1},
		{"a variable in a function", `func f() { var a Action; _ = a }`, 1},
		{"a constant converted", `const A = Action("s3:A")`, 1},
		{"a constant of another", `const A Action = B`, 1},
		{"a constant of a concatenation", `const A Action = "s3:" + "A"`, 1},
		{"a constant repeated", "const (\n\tA Action = \"s3:A\"\n\tB\n)", 1},
		{"a constant of iota", "const (\n\tA Action = iota\n\tB\n)", 2},
		{"a local constant", `func f() { const a Action = Action("s3:A"); _ = a }`, 1},
	}
	for _, tt := range tests {
		constants, faults := readConstants(t, "a.go", "package iam\n\ntype Action string\n\n"+tt.source+"\n")
		if len(faults) != tt.faults {
			t.Errorf("%s: faults %q, want %d", tt.name, faults, tt.faults)
		}
		if tt.faults == 0 && (len(constants) != 1 || constants[0].value != "s3:A") {
			t.Errorf("%s: read %+v", tt.name, constants)
		}
	}
}

// TestCatalogueHoldsEveryAction: every "s3:" and "admin:" constant of the
// package - an action the day it is declared - is in the catalogue once, and
// the catalogue holds nothing else: no cluster action, no name of the audit
// trail. Each entry says what it is.
func TestCatalogueHoldsEveryAction(t *testing.T) {
	t.Parallel()
	var declared []string
	for _, c := range packageConstants(t) {
		grantable := strings.HasPrefix(c.value, "s3:") || strings.HasPrefix(c.value, "admin:")
		if grantable {
			declared = append(declared, c.value)
		}
		if c.action && !grantable && !strings.HasPrefix(c.value, "cluster:") {
			t.Errorf("the action %s (%s) is neither grantable nor a cluster action", c.name, c.value)
		}
	}
	if len(declared) < 90 || !slices.Contains(declared, string(iam.ActionGetPolicyCatalogue)) || !slices.Contains(declared, string(iam.ActionExplainAccess)) {
		t.Fatalf("only %d actions were read from the package", len(declared))
	}
	kinds := map[string]bool{}
	for _, kind := range iam.ResourceKinds() {
		kinds[kind.Template] = true
	}
	var catalogued []string
	for _, action := range iam.Catalogue() {
		name := string(action.Name)
		if slices.Contains(catalogued, name) {
			t.Errorf("%s is in the catalogue twice", name)
		}
		catalogued = append(catalogued, name)
		if !slices.Contains(declared, name) {
			t.Errorf("%s is in the catalogue and is no action of the package", name)
		}
		if action.Group == "" || action.Description == "" || !strings.HasSuffix(action.Description, ".") || len(action.Resources) == 0 {
			t.Errorf("%s: a group, a sentence and the resources it is asked on are needed: %+v", name, action)
		}
		for _, template := range action.Resources {
			if !kinds[template] {
				t.Errorf("%s: the resource %s is not one of ResourceKinds", name, template)
			}
		}
	}
	for _, name := range declared {
		if !slices.Contains(catalogued, name) {
			t.Errorf("%s is an action of the package and not in the catalogue", name)
		}
	}
}

// instance makes the resource a template stands for, with names filled in:
// "site" for the bucket, "sub/k" for the key - a key of the sub-bucket "sub"
// - and "x" for any other name.
func instance(t *testing.T, template string) iam.Resource {
	t.Helper()
	var r iam.Resource
	switch {
	case template == "arn:aws:s3:::*":
	case strings.HasPrefix(template, "arn:aws:s3:::"):
		rest := strings.NewReplacer("{bucket}", "site", "{key}", "sub/k").Replace(strings.TrimPrefix(template, "arn:aws:s3:::"))
		r.Bucket, r.Key, _ = strings.Cut(rest, "/")
	case strings.HasPrefix(template, "arn:cactusfs:admin:::"):
		r.Admin = strings.ReplaceAll(strings.TrimPrefix(template, "arn:cactusfs:admin:::"), "{name}", "x")
	default:
		t.Fatalf("a template of no kind: %s", template)
	}
	want := strings.NewReplacer("{bucket}", "site", "{key}", "sub/k", "{name}", "x").Replace(template)
	if r.String() != want {
		t.Fatalf("the template %s is the resource %s, not %s", template, r, want)
	}
	return r
}

func allowJSON(t testing.TB, effect, principal string, actions []string, resources ...string) string {
	t.Helper()
	statement := map[string]any{"Effect": effect, "Action": actions, "Resource": resources}
	if principal != "" {
		statement["Principal"] = json.RawMessage(principal)
	}
	doc, err := json.Marshal(map[string]any{"Version": iam.PolicyVersion, "Statement": []any{statement}})
	if err != nil {
		t.Fatal(err)
	}
	return string(doc)
}

// readNotByName are the actions that read and do not begin with Get or
// List - reviewed one by one, and kept so on purpose. The built-in auditor
// holds "admin:Get*" and "admin:List*" and relies on this list: what is here
// reads, yet is no auditor's (a preview is the content of an object; an
// explanation discloses the permissions of others). An action added here is
// one more read the auditor is not given; one that reads and begins with Get
// or List is the auditor's at once.
func readNotByName() []iam.Action {
	return []iam.Action{iam.ActionPreviewObject, iam.ActionExplainAccess}
}

// TestCatalogueAgreesWithDecide: each flag of each action is what Decide and
// the parsers do with it, asked on every resource the action is asked on.
func TestCatalogueAgreesWithDecide(t *testing.T) {
	t.Parallel()
	everything := mustPolicy(t, allowJSON(t, "Allow", "", []string{"*"}, "*"))
	services := mustPolicy(t, allowJSON(t, "Allow", "", []string{"s3:*", "admin:*"}, "*"))
	auditor := iam.BuiltInPolicies()[iam.RoleAuditor]
	const site, alice = "site", `{"AWS":"arn:aws:iam:::user/alice"}`
	for _, action := range iam.Catalogue() {
		t.Run(string(action.Name), func(t *testing.T) {
			t.Parallel()
			name := string(action.Name)
			named := mustPolicy(t, allowJSON(t, "Allow", "", []string{name}, "*"))
			for _, template := range action.Resources {
				resource := instance(t, template)
				// by_name_only: no wildcard grants it; its name does.
				for policy, want := range map[*iam.Document]bool{&everything: !action.ByNameOnly, &services: !action.ByNameOnly, &named: true} {
					if err := decide(user("alice", *policy), action.Name, resource); want != (err == nil) {
						t.Errorf("on %s: allowed by %v = %v, want %v", resource, policy.Statements[0].Actions, err == nil, want)
					}
				}
				// read: the auditor, which holds what reads of
				// administration, holds no admin action that writes.
				if err := decide(user("alice", auditor), action.Name, resource); err == nil && !action.Read {
					t.Errorf("the auditor holds %s, which is not read", name)
				}
			}
			verb := name[strings.Index(name, ":")+1:]
			readByName := strings.HasPrefix(verb, "Get") || strings.HasPrefix(verb, "List")
			if action.Read != (readByName || slices.Contains(readNotByName(), action.Name)) {
				t.Errorf("read = %v; an action reads when it begins with Get or List, or is in readNotByName", action.Read)
			}

			// in_bucket_policy: a bucket policy may allow it, and then
			// lets the user it names do it on the bucket and its objects.
			userPolicy, err := iam.ParseBucketPolicy([]byte(allowJSON(t, "Allow", alice, []string{name}, "arn:aws:s3:::site", "arn:aws:s3:::site/*")), site)
			if action.InBucketPolicy != (err == nil) {
				t.Fatalf("in_bucket_policy = %v, and ParseBucketPolicy says %v", action.InBucketPolicy, err)
			}
			anyonePolicy, anyoneErr := iam.ParseBucketPolicy([]byte(allowJSON(t, "Allow", `"*"`, []string{name}, "arn:aws:s3:::site", "arn:aws:s3:::site/*")), site)
			if action.InBucketPolicy != (anyoneErr == nil) {
				t.Fatalf("in_bucket_policy = %v, and ParseBucketPolicy of anyone says %v", action.InBucketPolicy, anyoneErr)
			}
			if action.Anonymous && (!action.InBucketPolicy || !action.Read) {
				t.Errorf("anonymous, and not read in a bucket policy")
			}
			for _, template := range action.Resources {
				resource := instance(t, template)
				if !action.InBucketPolicy || resource.Bucket != site {
					// Without a bucket policy that names it, or outside
					// the bucket, no caller without credentials is let in.
					if err := iam.Decide(iam.Request{Action: action.Name, Resource: resource}); !errors.Is(err, domain.ErrAccessDenied) {
						t.Errorf("on %s: an anonymous caller was let in: %v", resource, err)
					}
					continue
				}
				request := iam.Request{Principal: user("alice"), Action: action.Name, Resource: resource, BucketPolicy: &userPolicy}
				if source, err := iam.Evaluate(request); err != nil || source != iam.SourceBucket {
					t.Errorf("on %s: the bucket policy does not let its user in: %s %v", resource, source, err)
				}
				request.Principal = user("bob")
				if err := iam.Decide(request); err == nil {
					t.Errorf("on %s: the bucket policy lets in a user it does not name", resource)
				}
				// anonymous: whom it lets in with "*", a caller without
				// credentials among them.
				anonymous := iam.Request{Action: action.Name, Resource: resource, BucketPolicy: &anyonePolicy}
				if err := iam.Decide(anonymous); action.Anonymous != (err == nil) {
					t.Errorf("on %s: anonymous = %v, and Decide says %v", resource, action.Anonymous, err)
				}
			}

			// in_sub_bucket_policy: the policy of a sub-bucket may name
			// it, and then lets the user it names do it in the sub-bucket.
			sub, err := iam.ParseSubBucketPolicy([]byte(allowJSON(t, "Allow", alice, []string{name}, "arn:aws:s3:::site/sub/*", "arn:aws:s3:::site")), site, "sub")
			if action.InSubBucketPolicy != (err == nil) {
				t.Fatalf("in_sub_bucket_policy = %v, and ParseSubBucketPolicy says %v", action.InSubBucketPolicy, err)
			}
			if !action.InSubBucketPolicy {
				return
			}
			if !action.InBucketPolicy {
				t.Errorf("in a sub-bucket's policy and not in a bucket's")
			}
			for _, template := range action.Resources {
				request := iam.Request{Principal: user("alice"), Action: action.Name, Resource: instance(t, template), SubBucketPolicy: &sub}
				if request.Resource.Key == "" {
					request.ListPrefix = "sub/"
				}
				if source, err := iam.Evaluate(request); err != nil || source != iam.SourceSubBucket {
					t.Errorf("on %s: the sub-bucket's policy does not let its user in: %s %v", request.Resource, source, err)
				}
			}
		})
	}
}

// operatorSample is a value of the kind each operator compares.
func operatorSample(operator string) string {
	switch operator {
	case iam.OpIPAddress, iam.OpNotIPAddress:
		return "10.0.0.0/8"
	case iam.OpBool:
		return "true"
	case iam.OpDateGreaterThan, iam.OpDateLessThan:
		return "2026-01-01T00:00:00Z"
	default:
		return "alice"
	}
}

func conditionJSON(t testing.TB, operator, key string, values ...string) []byte {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"Version": iam.PolicyVersion, "Statement": []any{map[string]any{
		"Effect": "Allow", "Action": "s3:GetObject", "Resource": "*",
		"Condition": map[string]any{operator: map[string]any{key: values}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestConditionKeysAgreeWithTheParser: every condition key of the package is
// listed once; every pair of a key and an operator that is listed parses,
// with a value of the key's kind, and every other pair of the names the
// package knows is refused.
func TestConditionKeysAgreeWithTheParser(t *testing.T) {
	t.Parallel()
	var keys, operators []string
	for _, c := range packageConstants(t) {
		switch {
		case strings.HasPrefix(c.name, "Key") && strings.HasPrefix(c.value, "aws:"):
			keys = append(keys, c.value)
		case strings.HasPrefix(c.name, "Op"):
			operators = append(operators, c.value)
		}
	}
	if len(keys) < 4 || len(operators) < 9 {
		t.Fatalf("%d keys and %d operators were read from the package", len(keys), len(operators))
	}
	listed := map[string]iam.ConditionKey{}
	for _, key := range iam.ConditionKeys() {
		if _, twice := listed[key.Key]; twice {
			t.Errorf("%s is listed twice", key.Key)
		}
		listed[key.Key] = key
		if !slices.Contains(keys, key.Key) || len(key.Operators) == 0 || key.WhenUnknown == "" {
			t.Errorf("%s: a key of the package, with operators and what an unknown value means: %+v", key.Key, key)
		}
	}
	samples := map[iam.ValueKind]string{
		iam.ValueIPPrefix: "192.0.2.1", iam.ValueBool: "false", iam.ValueDateTime: "2027-06-30T12:00:00+02:00", iam.ValueString: "a*",
	}
	for _, key := range keys {
		entry, ok := listed[key]
		if !ok {
			t.Errorf("the condition key %s is not listed", key)
			continue
		}
		for _, operator := range operators {
			want := slices.Contains(entry.Operators, operator)
			if _, err := iam.ParsePolicy(conditionJSON(t, operator, key, operatorSample(operator))); want != (err == nil) {
				t.Errorf("%s on %s: listed = %v, and ParsePolicy says %v", operator, key, want, err)
			}
			if !want {
				continue
			}
			sample, ok := samples[entry.ValueKind]
			if !ok {
				t.Fatalf("%s: a value kind of no sample: %q", key, entry.ValueKind)
			}
			if _, err := iam.ParsePolicy(conditionJSON(t, operator, key, sample)); err != nil {
				t.Errorf("%s on %s: a value of its kind %s is refused: %v", operator, key, entry.ValueKind, err)
			}
			if _, err := iam.ParsePolicy(conditionJSON(t, operator, key, "not a value")); (entry.ValueKind == iam.ValueString) != (err == nil) {
				t.Errorf("%s on %s, of the kind %s: a value of no kind: %v", operator, key, entry.ValueKind, err)
			}
		}
	}
	// A key or an operator the package does not know is refused too.
	for _, pair := range [][2]string{{iam.OpStringEquals, "aws:Referer"}, {"StringEqualsIgnoreCase", iam.KeyUsername}} {
		if _, err := iam.ParsePolicy(conditionJSON(t, pair[0], pair[1], "x")); err == nil {
			t.Errorf("%s on %s parses", pair[0], pair[1])
		}
	}
}

// TestConditionKeysWhenUnknown: what the catalogue says an unknown value
// means is what Decide does.
func TestConditionKeysWhenUnknown(t *testing.T) {
	t.Parallel()
	object := iam.Resource{Bucket: "site", Key: "k"}
	condition := func(effect, operator, key, value string) iam.Document {
		return mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"`+effect+`","Action":"s3:GetObject","Resource":"*",
			"Condition":{"`+operator+`":{"`+key+`":"`+value+`"}}}]}`)
	}
	everything := mustPolicy(t, allowJSON(t, "Allow", "", []string{"*"}, "*"))
	nowhere := user("alice")
	nowhere.Session.SourceIP = netip.Addr{}
	tests := []struct {
		name     string
		policies []iam.Document
		allowed  bool
	}{
		{"an Allow on an address does not hold", []iam.Document{condition("Allow", iam.OpIPAddress, iam.KeySourceIP, "0.0.0.0/0")}, false},
		{"an Allow off an address does not hold", []iam.Document{condition("Allow", iam.OpNotIPAddress, iam.KeySourceIP, "10.0.0.0/8")}, false},
		{"a Deny on an address holds", []iam.Document{everything, condition("Deny", iam.OpIPAddress, iam.KeySourceIP, "10.0.0.0/8")}, false},
		{"a Deny off an address holds", []iam.Document{everything, condition("Deny", iam.OpNotIPAddress, iam.KeySourceIP, "0.0.0.0/0")}, false},
		{"nothing else refuses", []iam.Document{everything}, true},
	}
	for _, tt := range tests {
		nowhere.Policies = tt.policies
		if err := decide(nowhere, iam.ActionGetObject, object); tt.allowed != (err == nil) {
			t.Errorf("unknown address: %s: allowed = %v, want %v (%v)", tt.name, err == nil, tt.allowed, err)
		}
	}
	// A caller without credentials is tested as the user name "".
	names := []struct {
		operator, value string
		allowed         bool
	}{
		{iam.OpStringEquals, "alice", false},
		{iam.OpStringEquals, "", true},
		{iam.OpStringLike, "*", true},
		{iam.OpStringLike, "?*", false},
		{iam.OpStringNotEquals, "alice", true},
		{iam.OpStringNotEquals, "", false},
		{iam.OpStringNotLike, "a*", true},
		{iam.OpStringNotLike, "**", false},
	}
	for _, tt := range names {
		policy := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject",
			"Resource":"arn:aws:s3:::site/*","Condition":{"`+tt.operator+`":{"aws:username":"`+tt.value+`"}}}]}`, "site")
		err := iam.Decide(iam.Request{Action: iam.ActionGetObject, Resource: object, BucketPolicy: policy})
		if tt.allowed != (err == nil) {
			t.Errorf("anonymous, %s %q: allowed = %v, want %v (%v)", tt.operator, tt.value, err == nil, tt.allowed, err)
		}
	}
}

// TestPolicyLimits: each limit is where the parsers draw the line.
func TestPolicyLimits(t *testing.T) {
	t.Parallel()
	limits := iam.PolicyLimits()
	if limits != (iam.Limits{MaxBytes: 20 << 10, MaxStatements: 64, MaxValues: 128, MaxDepth: 8}) {
		t.Fatalf("the limits are %+v; ADR-0032 gives them to the contract", limits)
	}
	repeat := func(s string, n int) []string { return slices.Repeat([]string{s}, n) }
	statements := func(n int) string {
		statement := `{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`
		return `{"Version":"2012-10-17","Statement":[` + strings.Join(repeat(statement, n), ",") + `]}`
	}
	small := allowJSON(t, "Allow", "", []string{"s3:GetObject"}, "*")
	nested := func(depth int) string {
		return `{"Version":` + strings.Repeat("[", depth) + `"x"` + strings.Repeat("]", depth) + `,"Statement":[]}`
	}
	users := func(n int) string {
		names := make([]string, n)
		for i := range names {
			names[i] = `"arn:aws:iam:::user/u` + strconv.Itoa(i) + `"`
		}
		return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":[` + strings.Join(names, ",") +
			`]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	}
	tests := []struct {
		name     string
		doc      string
		bucket   bool
		accepted bool
	}{
		{"as large as may be", small + strings.Repeat(" ", limits.MaxBytes-len(small)), false, true},
		{"a byte larger", small + strings.Repeat(" ", limits.MaxBytes-len(small)+1), false, false},
		{"a bucket policy a byte larger", users(1) + strings.Repeat(" ", limits.MaxBytes-len(users(1))+1), true, false},
		{"as many statements as may be", statements(limits.MaxStatements), false, true},
		{"a statement more", statements(limits.MaxStatements + 1), false, false},
		{"as many actions as may be", allowJSON(t, "Allow", "", repeat("s3:GetObject", limits.MaxValues), "*"), false, true},
		{"an action more", allowJSON(t, "Allow", "", repeat("s3:GetObject", limits.MaxValues+1), "*"), false, false},
		{"as many resources as may be", allowJSON(t, "Allow", "", []string{"s3:GetObject"}, repeat("*", limits.MaxValues)...), false, true},
		{"a resource more", allowJSON(t, "Allow", "", []string{"s3:GetObject"}, repeat("*", limits.MaxValues+1)...), false, false},
		{"as many condition values as may be", string(conditionJSON(t, iam.OpStringEquals, iam.KeyUsername, repeat("u", limits.MaxValues)...)), false, true},
		{"a condition value more", string(conditionJSON(t, iam.OpStringEquals, iam.KeyUsername, repeat("u", limits.MaxValues+1)...)), false, false},
		{"as many principals as may be", users(limits.MaxValues), true, true},
		{"a principal more", users(limits.MaxValues + 1), true, false},
	}
	for _, tt := range tests {
		var err error
		if tt.bucket {
			_, err = iam.ParseBucketPolicy([]byte(tt.doc), "site")
		} else {
			_, err = iam.ParsePolicy([]byte(tt.doc))
		}
		if tt.accepted != (err == nil) {
			t.Errorf("%s: accepted = %v, want %v (%v)", tt.name, err == nil, tt.accepted, err)
		}
	}
	// A value may sit MaxDepth levels below the document, and no deeper:
	// the first is refused for what it is, the second for its depth.
	for depth, deep := range map[int]bool{limits.MaxDepth - 1: false, limits.MaxDepth: true} {
		_, err := iam.ParsePolicy([]byte(nested(depth)))
		if err == nil || deep != strings.Contains(err.Error(), "nested too deeply") {
			t.Errorf("a value %d levels below the document: %v", depth+1, err)
		}
	}
}

// TestCatalogueEntriesShareNothing: each entry has its own resources, and
// each call its own catalogue: changing what one entry holds changes no
// other entry, and nothing the next caller is given.
func TestCatalogueEntriesShareNothing(t *testing.T) {
	t.Parallel()
	catalogue, want := iam.Catalogue(), iam.Catalogue()
	for i := range catalogue {
		for j := range catalogue[i].Resources {
			catalogue[i].Resources[j] = "changed"
		}
		for k := range catalogue {
			if k != i && slices.Contains(catalogue[k].Resources, "changed") {
				t.Fatalf("changing the resources of %s changed those of %s", catalogue[i].Name, catalogue[k].Name)
			}
		}
		for j := range catalogue[i].Resources {
			catalogue[i].Resources[j] = want[i].Resources[j]
		}
	}
	for i, action := range iam.Catalogue() {
		catalogue[i].Resources[0] = "changed"
		if !slices.Equal(action.Resources, want[i].Resources) {
			t.Errorf("%s: a caller's change reached the next caller", action.Name)
		}
	}
}

// TestResourceKinds: every kind is named once and asked on by some action,
// "*" aside, and each is a resource the parsers accept.
func TestResourceKinds(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, action := range iam.Catalogue() {
		for _, template := range action.Resources {
			used[template] = true
		}
	}
	seen := map[string]bool{}
	for _, kind := range iam.ResourceKinds() {
		if seen[kind.Kind] || seen[kind.Template] || kind.Description == "" {
			t.Errorf("%+v: a kind and a template named once, with a description", kind)
		}
		seen[kind.Kind], seen[kind.Template] = true, true
		if kind.Template != "*" && !used[kind.Template] {
			t.Errorf("no action is asked on %s", kind.Template)
		}
		if _, err := iam.ParsePolicy([]byte(allowJSON(t, "Allow", "", []string{"s3:GetObject"}, kind.Template))); err != nil {
			t.Errorf("%s is refused: %v", kind.Template, err)
		}
	}
	if !seen["*"] {
		t.Error(`"*" is not a resource kind`)
	}
}

// TestBucketPolicyPrincipals: each form that names anyone is read as anyone,
// and a user is named by the prefix.
func TestBucketPolicyPrincipals(t *testing.T) {
	t.Parallel()
	forms := iam.BucketPolicyPrincipals()
	if len(forms.Anyone) == 0 || forms.UserARNPrefix != "arn:aws:iam:::user/" {
		t.Fatalf("forms: %+v", forms)
	}
	for _, form := range append(slices.Clone(forms.Anyone), `{"AWS":"`+forms.UserARNPrefix+`alice"}`) {
		policy, err := iam.ParseBucketPolicy([]byte(allowJSON(t, "Allow", form, []string{"s3:GetObject"}, "arn:aws:s3:::site/*")), "site")
		if err != nil {
			t.Errorf("%s: %v", form, err)
			continue
		}
		want := []string{iam.PrincipalAnyone}
		if strings.Contains(form, "user/") {
			want = []string{"alice"}
		}
		if !slices.Equal(policy.Principals[0], want) || policy.OpenToAnyone() != (want[0] == iam.PrincipalAnyone) {
			t.Errorf("%s reads as %v", form, policy.Principals[0])
		}
	}
}
