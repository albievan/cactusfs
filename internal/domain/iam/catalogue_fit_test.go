package iam_test

import (
	"slices"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestFitsTemplate: a resource fits a template when its placeholders, each
// one or more characters - without a '/' but in {key} and {tag_key}, and
// without a wildcard but in {key} - make the template its ARN; the rest of
// a template is literal.
func TestFitsTemplate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		template string
		resource iam.Resource
		want     bool
	}{
		{"arn:aws:s3:::*", iam.Resource{}, true},
		{"arn:aws:s3:::*", iam.Resource{Bucket: "site"}, false},
		{"arn:aws:s3:::{bucket}", iam.Resource{Bucket: "site"}, true},
		{"arn:aws:s3:::{bucket}", iam.Resource{Bucket: "site", Key: "a"}, false},
		// The resource of every bucket as a whole is no bucket: a
		// placeholder is filled by a name, never by a wildcard.
		{"arn:aws:s3:::{bucket}", iam.Resource{}, false},
		{"arn:aws:s3:::{bucket}", iam.Resource{Bucket: "si?e"}, false},
		{"arn:aws:s3:::{bucket}/{key}", iam.Resource{Bucket: "*", Key: "a"}, false},
		// A key is the object's as it is, wildcards included.
		{"arn:aws:s3:::{bucket}/{key}", iam.Resource{Bucket: "site", Key: "a*?"}, true},
		{"arn:aws:s3:::{bucket}/{key}", iam.Resource{Bucket: "site", Key: "*"}, true},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Admin: "user/*"}, false},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Admin: "user/al?ce"}, false},
		{"arn:cactusfs:admin:::policy", iam.Resource{Admin: "policy"}, true},
		// A tag key may hold a '/', and never a wildcard.
		{"arn:cactusfs:admin:::tag-definition/{tag_key}", iam.Resource{Admin: "tag-definition/team/x"}, true},
		{"arn:cactusfs:admin:::tag-definition/{tag_key}", iam.Resource{Admin: "tag-definition/team"}, true},
		{"arn:cactusfs:admin:::tag-definition/{tag_key}", iam.Resource{Admin: "tag-definition/team/*"}, false},
		{"arn:cactusfs:admin:::tag-definition/{tag_key}", iam.Resource{Admin: "tag-definition/"}, false},
		{"arn:aws:s3:::{bucket}/{key}", iam.Resource{Bucket: "site", Key: "a/b/c"}, true},
		{"arn:aws:s3:::{bucket}/{key}", iam.Resource{Bucket: "site", Key: "/"}, true},
		{"arn:aws:s3:::{bucket}/{key}", iam.Resource{Bucket: "site"}, false},
		{"arn:cactusfs:admin:::user", iam.Resource{Admin: "user"}, true},
		{"arn:cactusfs:admin:::user", iam.Resource{Admin: "user/alice"}, false},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Admin: "user/alice"}, true},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Admin: "user/"}, false},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Admin: "user/a/b"}, false},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Admin: "users/alice"}, false},
		{"arn:cactusfs:admin:::user/{name}", iam.Resource{Node: "n1"}, false},
		{"arn:cactusfs:admin:::anonymous", iam.Resource{Admin: "anonymous"}, true},
		{"arn:cactusfs:admin:::{name", iam.Resource{Admin: "x"}, false},
	}
	for _, tt := range tests {
		if got := iam.FitsTemplate(tt.template, tt.resource); got != tt.want {
			t.Errorf("FitsTemplate(%s, %s) = %v, want %v", tt.template, tt.resource, got, tt.want)
		}
	}
	// Every action is asked on an instance of each of its templates.
	for _, action := range iam.Catalogue() {
		for _, template := range action.Resources {
			if r := instance(t, template); !action.AskedOn(r) {
				t.Errorf("%s is not asked on %s, an instance of its template %s", action.Name, r, template)
			}
		}
	}
}

// TestLookupAction: an action is found by the name the catalogue gives it,
// and by nothing else.
func TestLookupAction(t *testing.T) {
	t.Parallel()
	for _, action := range iam.Catalogue() {
		found, ok := iam.LookupAction(action.Name)
		if !ok || found.Name != action.Name || !slices.Equal(found.Resources, action.Resources) {
			t.Errorf("LookupAction(%s) = %+v, %v", action.Name, found, ok)
		}
	}
	for _, name := range []iam.Action{"s3:*", "s3:Get*", "S3:GETOBJECT", "s3:getobject", iam.ActionNodeRPC, "auth:SignIn", ""} {
		if found, ok := iam.LookupAction(name); ok {
			t.Errorf("LookupAction(%q) = %+v, want none", name, found)
		}
	}
	read, _ := iam.LookupAction(iam.ActionGetObject)
	if !read.AskedOn(iam.Resource{Bucket: "site", Key: "k"}) || read.AskedOn(iam.Resource{Bucket: "site"}) || read.AskedOn(iam.Resource{Admin: "user/x"}) {
		t.Errorf("s3:GetObject is asked on objects, and only on them")
	}
}
