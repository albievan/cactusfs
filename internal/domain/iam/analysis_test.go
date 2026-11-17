package iam_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// analysed returns every action the analysis knows: those a Deny of "*"
// covers.
func analysed(t testing.TB) []iam.Action {
	t.Helper()
	analyses := iam.AnalysePolicy(mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"*","Resource":"*"}]}`))
	return analyses[0].Actions
}

// TestAnalysedActionsAreTheCatalogue: the analysis knows every action of
// the catalogue - each s3: and admin: action a policy can grant, in the
// catalogue's order - and no other.
func TestAnalysedActionsAreTheCatalogue(t *testing.T) {
	t.Parallel()
	var catalogue []iam.Action
	for _, entry := range iam.Catalogue() {
		catalogue = append(catalogue, entry.Name)
	}
	if got := analysed(t); !slices.Equal(got, catalogue) {
		t.Errorf("the analysis knows\n%v\nwant\n%v", got, catalogue)
	}
}

func TestAnalysis(t *testing.T) {
	t.Parallel()
	actionsOf := func(prefix string) []iam.Action {
		var out []iam.Action
		for _, action := range analysed(t) {
			if strings.HasPrefix(string(action), prefix) {
				out = append(out, action)
			}
		}
		return out
	}
	byNameOnly := []iam.Action{iam.ActionOverrideGovernance, iam.ActionReplicateObject, iam.ActionReplicateDelete}
	allButByName := slices.DeleteFunc(slices.Clone(analysed(t)), func(a iam.Action) bool { return slices.Contains(byNameOnly, a) })
	identity := func(effect, actions string) string {
		return `{"Version":"2012-10-17","Statement":[{"Sid":"one","Effect":"` + effect + `","Action":` + actions + `,"Resource":"*"}]}`
	}
	bucketDoc := func(effect, principal, actions string) string {
		return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},` +
			`{"Effect":"` + effect + `","Principal":` + principal + `,"Action":` + actions + `,"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]}]}`
	}
	tests := []struct {
		name string
		// analyse parses a document and analyses it; the statement of
		// interest is the last.
		analyse   func() []iam.StatementAnalysis
		want      iam.StatementAnalysis
		statement int
	}{
		{
			"misspellings show", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Allow", `["s3:GetObjet","s3:GetObject*","admin:Lst*"]`)))
			},
			iam.StatementAnalysis{
				Sid: "one", Effect: iam.Allow, UnmatchedPatterns: []string{"s3:GetObjet", "admin:Lst*"},
				Actions: []iam.Action{
					// In the catalogue's order.
					iam.ActionGetObject, iam.ActionGetObjectTagging, iam.ActionGetObjectVersion, iam.ActionGetObjectRetention,
					iam.ActionGetObjectLegalHold,
				},
			},
			0,
		},
		{
			"everything allowed, but what only a name allows", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Allow", `"*"`)))
			},
			iam.StatementAnalysis{Sid: "one", Effect: iam.Allow, Actions: allButByName},
			0,
		},
		{
			"everything denied", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Deny", `"*"`)))
			},
			iam.StatementAnalysis{Sid: "one", Effect: iam.Deny, Actions: analysed(t)},
			0,
		},
		{
			"all of admin", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Allow", `"ADMIN:*"`)))
			},
			iam.StatementAnalysis{Sid: "one", Effect: iam.Allow, Actions: actionsOf("admin:")},
			0,
		},
		{
			"a wildcard of what only a name allows grants nothing", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Allow", `["s3:Bypass*","s3:Replicate*"]`)))
			},
			iam.StatementAnalysis{Sid: "one", Effect: iam.Allow, UnmatchedPatterns: []string{"s3:Bypass*", "s3:Replicate*"}},
			0,
		},
		{
			"denied by the same wildcard", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Deny", `"s3:Replicate*"`)))
			},
			iam.StatementAnalysis{Sid: "one", Effect: iam.Deny, Actions: []iam.Action{iam.ActionReplicateObject, iam.ActionReplicateDelete}},
			0,
		},
		{
			"allowed by name, in another case", func() []iam.StatementAnalysis {
				return iam.AnalysePolicy(mustPolicy(t, identity("Allow", `"S3:bypassgovernanceretention"`)))
			},
			iam.StatementAnalysis{Sid: "one", Effect: iam.Allow, Actions: []iam.Action{iam.ActionOverrideGovernance}},
			0,
		},
		{
			"a public bucket", func() []iam.StatementAnalysis {
				return iam.AnalyseBucketPolicy(*mustBucketPolicy(t, publicRead, "site"))
			},
			iam.StatementAnalysis{
				Sid: "read", Effect: iam.Allow, OpenToAnyone: true,
				Actions:          []iam.Action{iam.ActionListBucket, iam.ActionGetObject},
				AnonymousActions: []iam.Action{iam.ActionListBucket, iam.ActionGetObject},
			},
			0,
		},
		{
			"writing for anyone is writing for every user", func() []iam.StatementAnalysis {
				return iam.AnalyseBucketPolicy(*mustBucketPolicy(t, bucketDoc("Allow", `{"AWS":["arn:aws:iam:::user/alice","*"]}`, `["s3:PutObject","s3:GetObject"]`), "site"))
			},
			iam.StatementAnalysis{
				Index: 1, Effect: iam.Allow, OpenToAnyone: true,
				Actions:          []iam.Action{iam.ActionGetObject, iam.ActionPutObject},
				AnonymousActions: []iam.Action{iam.ActionGetObject},
				NeverAnonymous:   []iam.Action{iam.ActionPutObject},
			},
			1,
		},
		{
			"a deny for anyone is not open", func() []iam.StatementAnalysis {
				return iam.AnalyseBucketPolicy(*mustBucketPolicy(t, bucketDoc("Deny", `"*"`, `"s3:*"`), "site"))
			},
			iam.StatementAnalysis{Index: 1, Effect: iam.Deny, Actions: actionsOf("s3:")},
			1,
		},
		{
			"a statement for users is not open", func() []iam.StatementAnalysis {
				return iam.AnalyseBucketPolicy(*mustBucketPolicy(t, bucketDoc("Allow", `{"AWS":"arn:aws:iam:::user/alice"}`, `"s3:GetObject"`), "site"))
			},
			iam.StatementAnalysis{Index: 1, Effect: iam.Allow, Actions: []iam.Action{iam.ActionGetObject}},
			1,
		},
		{
			"a sub-bucket", func() []iam.StatementAnalysis {
				return iam.AnalyseSubBucketPolicy(*mustSubBucketPolicy(t, readReports, "site", "reports"))
			},
			iam.StatementAnalysis{Effect: iam.Allow, Actions: []iam.Action{iam.ActionListBucket, iam.ActionGetObject}},
			0,
		},
		{
			"a sub-bucket's deny for anyone", func() []iam.StatementAnalysis {
				return iam.AnalyseSubBucketPolicy(*mustSubBucketPolicy(t, subDocument(subStatement("Allow", aliceARN, `"s3:GetObject"`, reportKeys),
					subStatement("Deny", `"*"`, `"s3:PutObject"`, reportKeys)), "site", "reports"))
			},
			iam.StatementAnalysis{Index: 1, Effect: iam.Deny, Actions: []iam.Action{iam.ActionPutObject}},
			1,
		},
	}
	for _, tt := range tests {
		analyses := tt.analyse()
		got := analyses[tt.statement]
		if got.Index != tt.want.Index || got.Sid != tt.want.Sid || got.Effect != tt.want.Effect || got.OpenToAnyone != tt.want.OpenToAnyone ||
			!slices.Equal(got.Actions, tt.want.Actions) || !slices.Equal(got.AnonymousActions, tt.want.AnonymousActions) ||
			!slices.Equal(got.NeverAnonymous, tt.want.NeverAnonymous) || !slices.Equal(got.UnmatchedPatterns, tt.want.UnmatchedPatterns) {
			t.Errorf("%s:\n got %+v\nwant %+v", tt.name, got, tt.want)
		}
	}
	if len(iam.AnalyseSubBucketPolicy(iam.SubBucketPolicy{})) != 0 {
		t.Error("the policy of no sub-bucket has statements")
	}
}

// coveredByDecide reports whether a statement of only the pattern, resource
// "*" and no condition matches the action in Decide: an Allow lets a user
// with no other policy in; a Deny keeps out a user whom another statement
// allows the action by its name.
func coveredByDecide(t *testing.T, effect iam.Effect, pattern string, action iam.Action) bool {
	t.Helper()
	statement := iam.Statement{Effect: effect, Actions: []string{pattern}, Resources: []string{"*"}}
	policies := []iam.Document{{Version: iam.PolicyVersion, Statements: []iam.Statement{statement}}}
	if effect == iam.Deny {
		named := iam.Statement{Effect: iam.Allow, Actions: []string{string(action)}, Resources: []string{"*"}}
		policies = append(policies, iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{named}})
	}
	err := iam.Decide(iam.Request{
		Principal: iam.Principal{AccessKeyID: "K", User: "u", Policies: policies},
		Action:    action, Resource: iam.Resource{Bucket: "b", Key: "k"}, Now: policyNow,
	})
	return (err == nil) == (effect == iam.Allow)
}

// TestAnalysisAgreesWithDecide: an action is covered by a statement exactly
// when a statement of only that action pattern, resource "*" and no
// condition matches it in Decide; and a pattern is unmatched exactly when it
// covers no action. Checked for patterns made from the names of the actions
// - cut short, wildcarded, in other cases - and for every action named.
func TestAnalysisAgreesWithDecide(t *testing.T) {
	t.Parallel()
	actions := analysed(t)
	checked := 0
	agrees := func(effect iam.Effect, pattern string) bool {
		document, err := json.Marshal(map[string]any{"Version": iam.PolicyVersion, "Statement": []map[string]any{
			{"Effect": string(effect), "Action": pattern, "Resource": "*"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		doc, err := iam.ParsePolicy(document)
		if err != nil {
			return true // not a pattern the language has
		}
		checked++
		analysis := iam.AnalysePolicy(doc)[0]
		for _, action := range actions {
			if covered := slices.Contains(analysis.Actions, action); covered != coveredByDecide(t, effect, pattern, action) {
				t.Errorf("%s %q: covers %s = %v, Decide says otherwise", effect, pattern, action, covered)
				return false
			}
		}
		if unmatched := slices.Contains(analysis.UnmatchedPatterns, pattern); unmatched != (len(analysis.Actions) == 0) {
			t.Errorf("%s %q: unmatched %v, covers %v", effect, pattern, unmatched, analysis.Actions)
			return false
		}
		return true
	}
	// Every action by its name, and the wildcards everybody writes.
	for _, effect := range []iam.Effect{iam.Allow, iam.Deny} {
		for _, pattern := range []string{"*", "s3:*", "admin:*", "s3:Get*", "admin:Get*", "admin:List*", "s3:*Object*", "s3:?etObject", "s3:Nothing"} {
			agrees(effect, pattern)
		}
		for _, action := range actions {
			agrees(effect, string(action))
			agrees(effect, strings.ToUpper(string(action)))
		}
	}
	// Patterns made from the names: each pick changes one character of the
	// name to '*' or '?', cuts the name short with a '*', changes the case
	// of a letter or adds one; the service is kept, in either case.
	property := func(deny, upper bool, base uint8, picks []uint16) bool {
		service, name, _ := strings.Cut(string(actions[int(base)%len(actions)]), ":")
		if upper {
			service = strings.ToUpper(service)
		}
		pattern := []byte(name)
		for _, pick := range picks {
			if len(pattern) == 0 {
				break
			}
			at := int(pick/5) % len(pattern)
			switch pick % 5 {
			case 0:
				pattern[at] = '*'
			case 1:
				pattern[at] = '?'
			case 2:
				pattern = append(pattern[:at:at], '*')
			case 3:
				if c := pattern[at] | 0x20; c >= 'a' && c <= 'z' {
					pattern[at] ^= 0x20
				}
			default:
				pattern = slices.Insert(pattern, at, 'x')
			}
		}
		effect := iam.Allow
		if deny {
			effect = iam.Deny
		}
		return agrees(effect, service+":"+string(pattern))
	}
	named := checked
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
	// Most of the patterns made are patterns of the language: the
	// property is not held by patterns the parser refuses.
	if checked-named < 1500 {
		t.Errorf("only %d of 3000 patterns made were patterns of the language", checked-named)
	}
}
