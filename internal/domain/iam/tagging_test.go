package iam_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestTagActionsOfTheBuiltInRoles holds the built-in roles to ADR-0018: every
// built-in role may list and read the catalogue of tags; only the
// cluster-admin changes it; the tag rules of a bucket are held as the
// bucket's other configurations are - by "s3:*", so by the cluster-admin and
// the bucket-admin and by no role that reads or writes objects only, nor by
// the auditor.
func TestTagActionsOfTheBuiltInRoles(t *testing.T) {
	t.Parallel()
	policies := iam.BuiltInPolicies()
	every := []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin, iam.RoleReadWrite, iam.RoleReadOnly, iam.RoleAuditor}
	bucketAdmins := []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin}
	catalogue := iam.Resource{Admin: "tag-definitions"}
	definition := iam.Resource{Admin: "tag-definition/stage"}
	bucket := iam.Resource{Bucket: "site"}
	tests := []struct {
		name     string
		action   iam.Action
		resource iam.Resource
		holders  []string
	}{
		{"list the catalogue", iam.ActionListTagDefinitions, catalogue, every},
		{"read a definition", iam.ActionGetTagDefinition, definition, every},
		{"define a tag", iam.ActionPutTagDefinition, definition, []string{iam.RoleClusterAdmin}},
		{"remove a definition", iam.ActionDeleteTagDefinition, definition, []string{iam.RoleClusterAdmin}},
		{"read a bucket's tag rules", iam.ActionGetBucketTagRules, bucket, bucketAdmins},
		{"set a bucket's tag rules", iam.ActionPutBucketTagRules, bucket, bucketAdmins},
		// As the bucket's other configurations: whoever holds the one
		// holds the other.
		{"read a bucket's CORS rules", iam.ActionGetBucketCORS, bucket, bucketAdmins},
		{"set a bucket's lifecycle rules", iam.ActionPutLifecycle, bucket, bucketAdmins},
		{"set a bucket's policy", iam.ActionPutBucketPolicy, bucket, bucketAdmins},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, role := range every {
				err := decide(user("u", policies[role]), tt.action, tt.resource)
				want := slices.Contains(tt.holders, role)
				if want != (err == nil) {
					t.Errorf("%s: %s on %s allowed = %v, want %v (%v)", role, tt.action, tt.resource, err == nil, want, err)
				}
				if err != nil && !errors.Is(err, domain.ErrAccessDenied) {
					t.Errorf("%s: %s refused with %v, want an ErrAccessDenied", role, tt.action, err)
				}
			}
		})
	}
}

// TestWhoElseMayHaveTheTagActions: a bucket policy may grant the tag rules
// of its bucket to a user - they are S3 actions - and never to a caller who
// has not authenticated; "s3:*" gives nothing of the catalogue, which is no
// bucket's. A locked session has none of them, and an API key none its own
// policy does not allow.
func TestWhoElseMayHaveTheTagActions(t *testing.T) {
	t.Parallel()
	site := iam.Resource{Bucket: "site"}
	grant := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
		"Action":["s3:GetBucketTagRules","s3:PutBucketTagRules"],"Resource":"arn:aws:s3:::site"}]}`, "site")
	alice := iam.Principal{AccessKeyID: "A", User: "alice"}
	for _, action := range []iam.Action{iam.ActionGetBucketTagRules, iam.ActionPutBucketTagRules} {
		if err := iam.Decide(iam.Request{Principal: alice, Action: action, Resource: site, BucketPolicy: grant}); err != nil {
			t.Errorf("a bucket policy does not grant %s: %v", action, err)
		}
		if err := iam.Decide(iam.Request{Action: action, Resource: site, BucketPolicy: grant}); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("an anonymous caller was allowed %s: %v", action, err)
		}
	}
	// The catalogue is no bucket's: whoever holds all of S3, everywhere,
	// holds the tag rules of every bucket and nothing of the catalogue -
	// which they would, were its actions S3's.
	allOfS3 := user("s", mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`))
	for action, want := range map[iam.Action]bool{
		iam.ActionGetBucketTagRules: true, iam.ActionPutBucketTagRules: true,
		iam.ActionListTagDefinitions: false, iam.ActionGetTagDefinition: false,
		iam.ActionPutTagDefinition: false, iam.ActionDeleteTagDefinition: false,
	} {
		resource := site
		if !want {
			resource = iam.Resource{Admin: "tag-definition/stage"}
		}
		if err := decide(allOfS3, action, resource); want != (err == nil) {
			t.Errorf("a user with s3:* on everything: %s allowed = %v, want %v", action, err == nil, want)
		}
	}

	admin := user("u", iam.BuiltInPolicies()[iam.RoleClusterAdmin])
	locked := admin
	locked.LockedSession = true
	bound := admin
	readOnly := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:List*","Resource":"*"}]}`)
	bound.Boundary = &readOnly
	tests := []struct {
		name      string
		principal iam.Principal
		action    iam.Action
		resource  iam.Resource
		allowed   bool
	}{
		{"a locked session reads no catalogue", locked, iam.ActionListTagDefinitions, iam.Resource{Admin: "tag-definitions"}, false},
		{"a locked session sets no tag rules", locked, iam.ActionPutBucketTagRules, site, false},
		{"a key bound to listing lists", bound, iam.ActionListTagDefinitions, iam.Resource{Admin: "tag-definitions"}, true},
		{"a key bound to listing defines nothing", bound, iam.ActionPutTagDefinition, iam.Resource{Admin: "tag-definition/stage"}, false},
		{"a key bound to listing sets no tag rules", bound, iam.ActionPutBucketTagRules, site, false},
		{"the cluster-admin defines", admin, iam.ActionPutTagDefinition, iam.Resource{Admin: "tag-definition/stage"}, true},
	}
	for _, tt := range tests {
		err := decide(tt.principal, tt.action, tt.resource)
		if tt.allowed != (err == nil) {
			t.Errorf("%s: allowed = %v, want %v (%v)", tt.name, err == nil, tt.allowed, err)
		}
	}
}
