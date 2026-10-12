package iam_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// replicateActions are the two actions by which a version is committed with
// an ID, a time and a sequence of the caller's choosing (ADR-0026).
func replicateActions() []iam.Action {
	return []iam.Action{iam.ActionReplicateObject, iam.ActionReplicateDelete}
}

// TestReplicateIsNeverGrantedByAWildcard: committing a replica is allowed
// only by a statement that names the action. No built-in role holds it, and
// neither does anybody who has "everything". A Deny covers it however it is
// written.
func TestReplicateIsNeverGrantedByAWildcard(t *testing.T) {
	t.Parallel()
	allow := func(actions ...string) iam.Document {
		return iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: actions, Resources: []string{"*"}}}}
	}
	deny := func(actions ...string) iam.Document {
		return iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Deny, Actions: actions, Resources: []string{"*"}}}}
	}
	builtIn := iam.BuiltInPolicies()
	object := iam.Resource{Bucket: "replica", Key: "k"}
	for _, action := range replicateActions() {
		name := string(action)
		tests := []struct {
			name     string
			policies []iam.Document
			want     bool
		}{
			// The five built-in roles: none of them.
			{iam.RoleClusterAdmin, []iam.Document{builtIn[iam.RoleClusterAdmin]}, false},
			{iam.RoleBucketAdmin, []iam.Document{builtIn[iam.RoleBucketAdmin]}, false},
			{iam.RoleReadWrite, []iam.Document{builtIn[iam.RoleReadWrite]}, false},
			{iam.RoleReadOnly, []iam.Document{builtIn[iam.RoleReadOnly]}, false},
			{iam.RoleAuditor, []iam.Document{builtIn[iam.RoleAuditor]}, false},
			{"all five at once", []iam.Document{
				builtIn[iam.RoleClusterAdmin], builtIn[iam.RoleBucketAdmin], builtIn[iam.RoleReadWrite], builtIn[iam.RoleReadOnly], builtIn[iam.RoleAuditor],
			}, false},
			// Wildcards: none of them.
			{"everything", []iam.Document{allow("*")}, false},
			{"all of S3", []iam.Document{allow("s3:*")}, false},
			{"all of S3 and all of administration", []iam.Document{allow("s3:*", "admin:*")}, false},
			{"both replicate actions by a star", []iam.Document{allow("s3:Replicate*")}, false},
			{"a star in the middle", []iam.Document{allow("s3:*" + strings.TrimPrefix(name, "s3:Replicate"))}, false},
			{"a question mark for the last letter", []iam.Document{allow(name[:len(name)-1] + "?")}, false},
			{"a star after the whole name", []iam.Document{allow(name + "*")}, false},
			{"s3:PutObject, which is how a client writes", []iam.Document{allow("s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion")}, false},
			{"nothing", nil, false},
			// By name, and only then.
			{"by name", []iam.Document{allow(name)}, true},
			{"by name, in another case", []iam.Document{allow(strings.ToUpper(name))}, true},
			{"by name, among wildcards", []iam.Document{allow("s3:Get*", name)}, true},
			{"by name, and denied by a wildcard", []iam.Document{allow(name), deny("s3:*")}, false},
			{"by name, and denied by everything", []iam.Document{allow(name), deny("*")}, false},
			{"by name, and denied by a pattern", []iam.Document{allow(name), deny("s3:Replicate*")}, false},
			{"by name, and denied by name", []iam.Document{allow(name), deny(name)}, false},
		}
		for _, tt := range tests {
			err := iam.Decide(iam.Request{
				Principal: iam.Principal{AccessKeyID: "K", User: "u", Policies: tt.policies}, Action: action, Resource: object, Now: policyNow,
			})
			if (err == nil) != tt.want || (err != nil && !errors.Is(err, domain.ErrAccessDenied)) {
				t.Errorf("%s, %s: allowed = %v, want %v (%v)", action, tt.name, err == nil, tt.want, err)
			}
		}
		// Naming one of the two does not give the other.
		other := iam.ActionReplicateDelete
		if action == iam.ActionReplicateDelete {
			other = iam.ActionReplicateObject
		}
		if decide(user("u", allow(name)), other, object) == nil {
			t.Errorf("%s gives %s", action, other)
		}
		// A key's inline policy must name it as well: a key never exceeds
		// its owner, and "everything" of the key's is not it either.
		named, everything := allow(name), allow("s3:*")
		for boundary, want := range map[*iam.Document]bool{&named: true, &everything: false} {
			p := user("u", named)
			p.Boundary = boundary
			if err := decide(p, action, object); (err == nil) != want {
				t.Errorf("%s with a key's own policy %v: %v", action, boundary.Statements[0].Actions, err)
			}
		}
		// The same in a case nobody writes: the rule is of the action, not
		// of its spelling.
		if decide(user("u", allow("s3:*")), iam.Action(strings.ToLower(name)), object) == nil {
			t.Errorf("%s written in lower case is granted by a wildcard", action)
		}
		// Nobody without a name, and no service, has it.
		for who, p := range map[string]iam.Principal{
			"anonymous": {}, "the lifecycle service": {Service: iam.ServiceLifecycle}, "a node": {NodeID: "n1"},
		} {
			if err := decide(p, action, object); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("%s and %s: %v", who, action, err)
			}
		}
	}
	// The same wildcards do grant what they always did: writing as a
	// client does.
	for _, policy := range []iam.Document{allow("*"), allow("s3:*"), builtIn[iam.RoleBucketAdmin]} {
		if err := decide(user("u", policy), iam.ActionPutObject, object); err != nil {
			t.Errorf("a wildcard no longer grants s3:PutObject: %v", err)
		}
	}
}

// TestNoBucketPolicyGrantsReplicate: whoever may write a bucket's policy -
// the bucket-admin, by "s3:*" - cannot give themselves, or anybody, the
// right to commit replicas into it.
func TestNoBucketPolicyGrantsReplicate(t *testing.T) {
	t.Parallel()
	const bucketName = "replica"
	document := func(effect, action string) string {
		return `{"Version":"2012-10-17","Statement":[{"Effect":"` + effect + `","Principal":"*","Action":"` + action + `","Resource":"arn:aws:s3:::replica/*"}]}`
	}
	object := iam.Resource{Bucket: bucketName, Key: "k"}
	for _, action := range replicateActions() {
		name := string(action)
		// A policy that names it is refused.
		for _, spelling := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			if _, err := iam.ParseBucketPolicy([]byte(document("Allow", spelling)), bucketName); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a bucket policy that allows %s: %v", spelling, err)
			}
		}
		// A policy that covers it with a wildcard is kept, and does not
		// grant it - to a user without it, to the bucket-admin, to nobody.
		for _, pattern := range []string{"s3:*", "s3:Replicate*", "s3:R*"} {
			policy, err := iam.ParseBucketPolicy([]byte(document("Allow", pattern)), bucketName)
			if err != nil {
				t.Fatalf("a bucket policy with %s: %v", pattern, err)
			}
			for who, p := range map[string]iam.Principal{
				"a user without policies": user("u"),
				"the bucket-admin":        user("b", iam.BuiltInPolicies()[iam.RoleBucketAdmin]),
				"the cluster-admin":       user("c", iam.BuiltInPolicies()[iam.RoleClusterAdmin]),
				"anonymous":               {},
			} {
				err := iam.Decide(iam.Request{Principal: p, Action: action, Resource: object, BucketPolicy: &policy, Now: policyNow})
				if !errors.Is(err, domain.ErrAccessDenied) {
					t.Errorf("%s is given %s by a bucket policy with %s: %v", who, action, pattern, err)
				}
			}
		}
		// A policy that was stored before the rule - made here without the
		// parser - grants nothing by naming it either.
		stored := iam.BucketPolicy{
			Document: iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{
				Effect: iam.Allow, Actions: []string{name}, Resources: []string{"arn:aws:s3:::replica/*"},
			}}},
			Principals: [][]string{{iam.PrincipalAnyone}},
		}
		if err := iam.Decide(iam.Request{Principal: user("u"), Action: action, Resource: object, BucketPolicy: &stored, Now: policyNow}); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("a stored bucket policy that names %s grants it: %v", action, err)
		}
		// A Deny of a bucket policy does cover it, for one who holds it.
		holder := user("u", iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{name}, Resources: []string{"*"}}}})
		for _, pattern := range []string{"s3:*", name} {
			deny, err := iam.ParseBucketPolicy([]byte(document("Deny", pattern)), bucketName)
			if err != nil {
				t.Fatalf("a bucket policy that denies %s: %v", pattern, err)
			}
			if err := iam.Decide(iam.Request{Principal: holder, Action: action, Resource: object, BucketPolicy: &deny, Now: policyNow}); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("a bucket policy that denies %s does not keep %s from its holder: %v", pattern, action, err)
			}
		}
		if err := decide(holder, action, object); err != nil {
			t.Errorf("the holder of %s is refused without a bucket policy: %v", action, err)
		}
	}
}

// TestRootHoldsTheActionsNoWildcardGrants pins what the root identity is to
// the actions that are granted by name only: it holds them, all three alike,
// as it holds every other action - it has no policy that could name them. A
// replica action is to root what s3:BypassGovernanceRetention is. What stops
// root is the state, not the authorizer: a bucket that is no replica of the
// source takes no replica from anybody (ADR-0026).
func TestRootHoldsTheActionsNoWildcardGrants(t *testing.T) {
	t.Parallel()
	everything := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"*"}, Resources: []string{"*"}}}}
	object := iam.Resource{Bucket: "replica", Key: "k"}
	tests := []struct {
		name      string
		principal iam.Principal
		want      bool
	}{
		{"root", iam.Principal{AccessKeyID: "ROOT", Root: true}, true},
		// Whoever only says so is not root.
		{"root without a key", iam.Principal{Root: true}, false},
		{"root and a user at once", iam.Principal{AccessKeyID: "ROOT", Root: true, User: "u", Policies: []iam.Document{everything}}, false},
		// And a user with everything is not root either.
		{"a user with everything", iam.Principal{AccessKeyID: "AKID", User: "u", Policies: []iam.Document{everything}}, false},
		{"a user with the role cluster-admin", iam.Principal{AccessKeyID: "AKID", User: "u", Policies: []iam.Document{iam.BuiltInPolicies()[iam.RoleClusterAdmin]}}, false},
	}
	for _, action := range append(replicateActions(), iam.ActionOverrideGovernance) {
		for _, tt := range tests {
			err := iam.Decide(iam.Request{Principal: tt.principal, Action: action, Resource: object})
			if (err == nil) != tt.want || (err != nil && !errors.Is(err, domain.ErrAccessDenied)) {
				t.Errorf("%s, %s: Decide = %v, allowed wanted: %v", tt.name, action, err, tt.want)
			}
		}
	}
}
