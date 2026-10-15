package iam_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestPreviewByOwnership walks the rule of ADR-0028 as Decide has it: who
// committed the version shown may preview it when no policy speaks of it;
// a Deny in any source still refuses them, an API key's inline policy must
// still allow the action, and whoever is no user owns nothing.
func TestPreviewByOwnership(t *testing.T) {
	t.Parallel()
	doc := func(effect, action, resource string) iam.Document {
		return mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"`+effect+`","Action":"`+action+`","Resource":"`+resource+`"}]}`)
	}
	download := doc("Allow", "s3:GetObject", "*")
	assigned := doc("Allow", "admin:PreviewObject", "*")
	assignedElsewhere := doc("Allow", "admin:PreviewObject", "arn:aws:s3:::other/*")
	denied := doc("Deny", "admin:PreviewObject", "arn:aws:s3:::b/*")
	deniedAll := doc("Deny", "*", "*")
	deniedAdmin := doc("Deny", "admin:*", "*")
	deniedOther := doc("Deny", "admin:PreviewObject", "arn:aws:s3:::other/*")
	everything := doc("Allow", "*", "*")
	boundaryWithout := doc("Allow", "s3:GetObject", "*")
	boundaryWith := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","admin:PreviewObject"],"Resource":"*"}]}`)
	builtIn := iam.BuiltInPolicies()
	// A bucket policy cannot be written to name an admin action; one made
	// by hand stands for any source of a Deny there may ever be.
	bucketDeny := &iam.BucketPolicy{
		Document:   iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Deny, Actions: []string{"admin:PreviewObject"}, Resources: []string{"*"}}}},
		Principals: [][]string{{iam.PrincipalAnyone}},
	}
	public := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`, "b")
	alice := func(policies ...iam.Document) iam.Principal { return user("alice", policies...) }
	withBoundary := func(p iam.Principal, boundary iam.Document) iam.Principal {
		p.Boundary = &boundary
		return p
	}
	locked := alice(everything)
	locked.LockedSession = true

	tests := []struct {
		name      string
		principal iam.Principal
		creator   string
		bucket    *iam.BucketPolicy
		allowed   bool
		// lockedErr says the refusal is that of a locked session.
		lockedErr bool
	}{
		{"owner, policies silent", alice(download), "user/alice", nil, true, false},
		{"owner without any policy", alice(), "user/alice", nil, true, false},
		{"owner, denied the action on the object", alice(download, denied), "user/alice", nil, false, false},
		{"owner, denied everything", alice(download, deniedAll), "user/alice", nil, false, false},
		{"owner, denied all of administration", alice(download, deniedAdmin), "user/alice", nil, false, false},
		{"owner, denied elsewhere only", alice(download, deniedOther), "user/alice", nil, true, false},
		{"owner, denied by the bucket", alice(download), "user/alice", bucketDeny, false, false},
		{"owner, assigned and denied", alice(assigned, denied), "user/alice", nil, false, false},
		{"non-owner, policies silent", alice(download), "user/bob", nil, false, false},
		{"non-owner, assigned", alice(assigned), "user/bob", nil, true, false},
		{"non-owner, assigned on another bucket", alice(assignedElsewhere), "user/bob", nil, false, false},
		{"assigned, creator unknown", alice(assigned), "", nil, true, false},
		{"creator unknown, policies silent", alice(download), "", nil, false, false},
		{"creator is the user's own key", alice(download), "key/KEYALICE", nil, false, false},
		{"creator is root", alice(download), "root", nil, false, false},
		{"creator differs in case", alice(download), "user/Alice", nil, false, false},
		{"creator is the bare name", alice(download), "alice", nil, false, false},
		{"creator has a suffix", alice(download), "user/alice2", nil, false, false},
		{"creator is a node", alice(download), "node/alice", nil, false, false},
		{"owner by a key whose policy lacks the action", withBoundary(alice(download), boundaryWithout), "user/alice", nil, false, false},
		{"assignee by a key whose policy lacks the action", withBoundary(alice(assigned), boundaryWithout), "user/bob", nil, false, false},
		{"owner by a key whose policy has the action", withBoundary(alice(download), boundaryWith), "user/alice", nil, true, false},
		{"assignee by a key whose policy has the action", withBoundary(alice(assigned), boundaryWith), "user/bob", nil, true, false},
		{"owner in a locked session", locked, "user/alice", nil, false, true},
		{"anonymous, creator empty", iam.Principal{}, "", nil, false, false},
		{"anonymous, creator names nobody", iam.Principal{}, "user/", nil, false, false},
		{"anonymous, bucket open to anyone", iam.Principal{}, "user/", public, false, false},
		{"root", iam.Principal{AccessKeyID: "ROOT", Root: true}, "", nil, true, false},
		{"a node", iam.Principal{NodeID: "n1"}, "node/n1", nil, false, false},
		{"the lifecycle service", iam.Principal{Service: iam.ServiceLifecycle}, "service/lifecycle", nil, false, false},
		{"a key without a user", iam.Principal{AccessKeyID: "KEYX"}, "key/KEYX", nil, false, false},
		{"auditor, not the owner", alice(builtIn[iam.RoleAuditor]), "user/bob", nil, false, false},
		{"auditor, the owner", alice(builtIn[iam.RoleAuditor]), "user/alice", nil, true, false},
		{"cluster-admin", alice(builtIn[iam.RoleClusterAdmin]), "user/bob", nil, true, false},
		{"bucket-admin", alice(builtIn[iam.RoleBucketAdmin]), "user/bob", nil, true, false},
		{"read-write", alice(builtIn[iam.RoleReadWrite]), "user/bob", nil, true, false},
		{"read-only", alice(builtIn[iam.RoleReadOnly]), "user/bob", nil, true, false},
		{"read-only, denied", alice(builtIn[iam.RoleReadOnly], denied), "user/bob", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := iam.Decide(iam.Request{
				Principal: tt.principal, Action: iam.ActionPreviewObject, Resource: iam.Resource{Bucket: "b", Key: "k"},
				Now: policyNow, BucketPolicy: tt.bucket, ObjectCreator: tt.creator,
			})
			if (err == nil) != tt.allowed {
				t.Fatalf("allowed = %v, want %v (%v)", err == nil, tt.allowed, err)
			}
			switch {
			case err == nil:
			case tt.lockedErr && !errors.Is(err, domain.ErrSessionLocked):
				t.Errorf("refusal = %v, want %v", err, domain.ErrSessionLocked)
			case !tt.lockedErr && !errors.Is(err, domain.ErrAccessDenied):
				t.Errorf("refusal = %v, want %v", err, domain.ErrAccessDenied)
			}
		})
	}
}

// TestOwnershipGivesNothingElse: for every action but the preview, and
// policies, a key's inline policy, a bucket's policy and a creator taken at
// random, the
// decision is the same whoever the creator is - also when it names the
// caller.
func TestOwnershipGivesNothingElse(t *testing.T) {
	t.Parallel()
	var actions []iam.Action
	for _, action := range declaredActions(t) {
		if action != iam.ActionPreviewObject {
			actions = append(actions, action)
		}
	}
	effects := []iam.Effect{iam.Allow, iam.Deny}
	actionPatterns := []string{"*", "s3:*", "s3:Get*", "s3:GetObject", "admin:*", "admin:Preview*", "admin:Get*", "s3:PutObject"}
	resourcePatterns := []string{"*", "arn:aws:s3:::*", "arn:aws:s3:::b/*", "arn:aws:s3:::b", "arn:cactusfs:admin:::*"}
	resources := []iam.Resource{{Bucket: "b", Key: "k"}, {Bucket: "b"}, {}, {Admin: "user/alice"}, {Node: "n1"}}
	creators := []string{"", "user/alice", "user/bob", "root", "key/KEYALICE", "node/n1", "service/lifecycle"}
	principals := []iam.Principal{
		{AccessKeyID: "KEYALICE", User: "alice", Session: iam.Session{SourceIP: netip.MustParseAddr("10.1.2.3"), SecureTransport: true}},
		{AccessKeyID: "ROOT", Root: true},
		{},
		{NodeID: "n1"},
		{Service: iam.ServiceLifecycle},
		{User: "alice", LockedSession: true},
	}
	pick := func(n uint8, of int) int { return int(n) % of }
	document := func(picks []uint8) *iam.Document {
		doc := iam.Document{Version: iam.PolicyVersion}
		for i := 0; i+2 < len(picks) && len(doc.Statements) < 4; i += 3 {
			doc.Statements = append(doc.Statements, iam.Statement{
				Effect:    effects[pick(picks[i], len(effects))],
				Actions:   []string{actionPatterns[pick(picks[i+1], len(actionPatterns))]},
				Resources: []string{resourcePatterns[pick(picks[i+2], len(resourcePatterns))]},
			})
		}
		if len(doc.Statements) == 0 || doc.Validate() != nil {
			return nil
		}
		return &doc
	}
	// The policy of bucket b: statements about anyone or about alice, of
	// S3 actions only, as ParseBucketPolicy takes them.
	bucketActions := []string{"s3:*", "s3:Get*", "s3:GetObject", "s3:PutObject", "s3:ListBucket"}
	bucketResources := []string{"arn:aws:s3:::b", "arn:aws:s3:::b/*", "arn:aws:s3:::b/k"}
	bucketPrincipals := []any{"*", map[string]any{"AWS": "arn:aws:iam:::user/alice"}, map[string]any{"AWS": []string{"arn:aws:iam:::user/bob", "*"}}}
	bucketPolicy := func(picks []uint8) *iam.BucketPolicy {
		var statements []map[string]any
		for i := 0; i+3 < len(picks) && len(statements) < 4; i += 4 {
			statements = append(statements, map[string]any{
				"Effect":    effects[pick(picks[i], len(effects))],
				"Principal": bucketPrincipals[pick(picks[i+1], len(bucketPrincipals))],
				"Action":    bucketActions[pick(picks[i+2], len(bucketActions))],
				"Resource":  bucketResources[pick(picks[i+3], len(bucketResources))],
			})
		}
		if len(statements) == 0 {
			return nil
		}
		raw, err := json.Marshal(map[string]any{"Version": iam.PolicyVersion, "Statement": statements})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := iam.ParseBucketPolicy(raw, "b")
		if err != nil {
			t.Fatalf("ParseBucketPolicy(%s): %v", raw, err)
		}
		return &parsed
	}
	property := func(own, boundary, ofBucket []uint8, who, action, resource, creator uint8) bool {
		principal := principals[pick(who, len(principals))]
		if principal.User != "" {
			if doc := document(own); doc != nil {
				principal.Policies = []iam.Document{*doc}
			}
			principal.Boundary = document(boundary)
		}
		req := iam.Request{
			Principal: principal, Action: actions[pick(action, len(actions))],
			Resource: resources[pick(resource, len(resources))], Now: policyNow, BucketPolicy: bucketPolicy(ofBucket),
		}
		without := iam.Decide(req)
		req.ObjectCreator = creators[pick(creator, len(creators))]
		with := iam.Decide(req)
		return (without == nil) == (with == nil) &&
			errors.Is(without, domain.ErrAccessDenied) == errors.Is(with, domain.ErrAccessDenied) &&
			errors.Is(without, domain.ErrSessionLocked) == errors.Is(with, domain.ErrSessionLocked)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 10000}); err != nil {
		t.Error(err)
	}
}
