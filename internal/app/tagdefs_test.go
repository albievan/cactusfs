package app_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/tagging"
)

// tagDefinitionFixture is the use cases of the catalogue over the in-memory
// stores of the IAM fixture, whose audit trail the catalogue writes to.
type tagDefinitionFixture struct {
	*iamFixture
	catalogue *app.TagDefinitionService
	buckets   *app.BucketService
}

func newTagDefinitionFixture(t *testing.T) *tagDefinitionFixture {
	t.Helper()
	f := newIAMFixture(t)
	f.meta.Audit = f.store
	return &tagDefinitionFixture{
		iamFixture: f,
		catalogue:  app.NewTagDefinitionService(app.TagDefinitionServiceConfig{Store: f.meta, Authz: f.authz, Auditor: f.auditor, Clock: f.clock}),
		buckets:    app.NewBucketService(f.meta, f.authz, f.clock),
	}
}

// TestTagDefinitionsAreAuthorized: every built-in role lists and reads the
// catalogue; only cluster-admin changes it. A refusal is audited, for the
// action and the resource the contract names, and changes nothing.
func TestTagDefinitionsAreAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTagDefinitionFixture(t)
	if _, err := f.catalogue.Create(ctx, f.root, app.TagDefinitionInput{Key: "stage", Values: []string{"draft"}}); err != nil {
		t.Fatal(err)
	}
	roles := []string{iam.RoleClusterAdmin, iam.RoleBucketAdmin, iam.RoleReadWrite, iam.RoleReadOnly, iam.RoleAuditor}
	principals := map[string]iam.Principal{}
	for _, role := range roles {
		principals[role] = f.as(t, f.user(t, "user-"+role, role))
	}
	operations := []struct {
		name     string
		action   iam.Action
		resource string
		call     func(p iam.Principal) error
		allowed  []string
	}{
		{"list", iam.ActionListTagDefinitions, "arn:cactusfs:admin:::tag-definitions", func(p iam.Principal) error {
			_, err := f.catalogue.List(ctx, p)
			return err
		}, roles},
		{"get", iam.ActionGetTagDefinition, "arn:cactusfs:admin:::tag-definition/stage", func(p iam.Principal) error {
			_, err := f.catalogue.Get(ctx, p, "stage")
			return err
		}, roles},
		{"create", iam.ActionPutTagDefinition, "arn:cactusfs:admin:::tag-definition/new", func(p iam.Principal) error {
			_, err := f.catalogue.Create(ctx, p, app.TagDefinitionInput{Key: "new"})
			return err
		}, []string{iam.RoleClusterAdmin}},
		{"put", iam.ActionPutTagDefinition, "arn:cactusfs:admin:::tag-definition/stage", func(p iam.Principal) error {
			_, err := f.catalogue.Put(ctx, p, app.TagDefinitionInput{Key: "stage", Values: []string{"draft", "final"}})
			return err
		}, []string{iam.RoleClusterAdmin}},
		{"delete", iam.ActionDeleteTagDefinition, "arn:cactusfs:admin:::tag-definition/new", func(p iam.Principal) error {
			return f.catalogue.Delete(ctx, p, "new")
		}, []string{iam.RoleClusterAdmin}},
	}
	for _, op := range operations {
		for _, role := range roles {
			err := op.call(principals[role])
			allowed := false
			for _, r := range op.allowed {
				allowed = allowed || r == role
			}
			switch {
			case allowed && err != nil:
				t.Errorf("%s by %s: %v", op.name, role, err)
			case !allowed && !errors.Is(err, domain.ErrAccessDenied):
				t.Errorf("%s by %s = %v, want ErrAccessDenied", op.name, role, err)
			}
			if allowed {
				continue
			}
			found := false
			for _, r := range f.trail(t) {
				found = found || (r.Outcome == audit.Denied && r.Action == string(op.action) && r.Resource == op.resource && r.Actor == "user/user-"+role)
			}
			if !found {
				t.Errorf("%s by %s: no denial of %s on %s in the trail", op.name, role, op.action, op.resource)
			}
		}
	}
	// What was refused changed nothing.
	if got, err := f.catalogue.Get(ctx, f.root, "stage"); err != nil || !reflect.DeepEqual(got.Values, []string{"draft", "final"}) {
		t.Errorf("Get after the refusals = %+v, %v", got, err)
	}
}

// TestTagDefinitionService: what each use case of the catalogue does, and
// what it refuses: a key defined already is not created again, a change
// keeps the time a tag was first defined, a definition a rule names is not
// deleted, and every change is in the trail.
func TestTagDefinitionService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTagDefinitionFixture(t)
	if err := f.buckets.Create(ctx, f.root, "photos"); err != nil {
		t.Fatal(err)
	}
	if err := f.buckets.SetTagRules(ctx, f.root, "photos", bucket.TagRules{Required: []bucket.RequiredTag{{Key: "owner"}}}); err != nil {
		t.Fatal(err)
	}
	first := f.clock.Now().UTC()
	base := len(f.trail(t))
	steps := []struct {
		name string
		do   func() (tagging.Definition, error)
		// want is the definition answered; err the refusal.
		want tagging.Definition
		err  error
	}{
		{"create", func() (tagging.Definition, error) {
			return f.catalogue.Create(ctx, f.root, app.TagDefinitionInput{Key: "stage", Description: "Where it is", Values: []string{"draft", "final"}})
		}, tagging.Definition{Key: "stage", Description: "Where it is", Values: []string{"draft", "final"}, CreatedAt: first}, nil},
		{"create without values", func() (tagging.Definition, error) {
			return f.catalogue.Create(ctx, f.root, app.TagDefinitionInput{Key: "owner", Values: []string{}})
		}, tagging.Definition{Key: "owner", CreatedAt: first}, nil},
		{"create what is defined", func() (tagging.Definition, error) {
			f.clock.Advance(time.Hour)
			return f.catalogue.Create(ctx, f.root, app.TagDefinitionInput{Key: "stage"})
		}, tagging.Definition{}, domain.ErrEntityExists},
		{"put what is defined", func() (tagging.Definition, error) {
			return f.catalogue.Put(ctx, f.root, app.TagDefinitionInput{Key: "stage", Description: "Stage", Values: []string{"final"}})
		}, tagging.Definition{Key: "stage", Description: "Stage", Values: []string{"final"}, CreatedAt: first}, nil},
		{"put what is new", func() (tagging.Definition, error) {
			return f.catalogue.Put(ctx, f.root, app.TagDefinitionInput{Key: "team"})
		}, tagging.Definition{Key: "team", CreatedAt: first.Add(time.Hour)}, nil},
		{"a key in upper case", func() (tagging.Definition, error) {
			return f.catalogue.Create(ctx, f.root, app.TagDefinitionInput{Key: "Stage"})
		}, tagging.Definition{}, domain.ErrInvalidArgument},
		{"a value no tag can have", func() (tagging.Definition, error) {
			return f.catalogue.Put(ctx, f.root, app.TagDefinitionInput{Key: "stage", Values: []string{"a&b"}})
		}, tagging.Definition{}, domain.ErrInvalidArgument},
		{
			"get", func() (tagging.Definition, error) { return f.catalogue.Get(ctx, f.root, "stage") },
			tagging.Definition{Key: "stage", Description: "Stage", Values: []string{"final"}, CreatedAt: first},
			nil,
		},
		{
			"get what is not defined", func() (tagging.Definition, error) { return f.catalogue.Get(ctx, f.root, "missing") },
			tagging.Definition{},
			domain.ErrEntityNotFound,
		},
		{"delete what a rule names", func() (tagging.Definition, error) {
			return tagging.Definition{}, f.catalogue.Delete(ctx, f.root, "owner")
		}, tagging.Definition{}, domain.ErrEntityInUse},
		{"delete what is not defined", func() (tagging.Definition, error) {
			return tagging.Definition{}, f.catalogue.Delete(ctx, f.root, "missing")
		}, tagging.Definition{}, domain.ErrEntityNotFound},
		{
			"delete", func() (tagging.Definition, error) {
				return tagging.Definition{}, f.catalogue.Delete(ctx, f.root, "team")
			},
			tagging.Definition{},
			nil,
		},
	}
	for _, step := range steps {
		got, err := step.do()
		if !errors.Is(err, step.err) || (step.err == nil && err != nil) || !reflect.DeepEqual(got, step.want) {
			t.Errorf("%s = %+v, %v; want %+v, %v", step.name, got, err, step.want, step.err)
		}
		if errors.Is(err, domain.ErrEntityInUse) && (errors.Is(err, domain.ErrConflict) || strings.Contains(err.Error(), "photos")) {
			t.Errorf("%s: the refusal is the store's, or names the bucket: %v", step.name, err)
		}
	}
	listed, err := f.catalogue.List(ctx, f.root)
	if err != nil || len(listed) != 2 || listed[0].Key != "owner" || listed[1].Key != "stage" {
		t.Errorf("List = %+v, %v", listed, err)
	}

	// The changes made, and only those, are in the trail.
	var changes []string
	for _, r := range f.trail(t)[base:] {
		if r.Outcome == audit.Succeeded && r.Actor == "root" {
			changes = append(changes, r.Action+" "+r.Resource+" "+r.Details["created"])
		}
	}
	want := []string{
		"admin:PutTagDefinition arn:cactusfs:admin:::tag-definition/stage true",
		"admin:PutTagDefinition arn:cactusfs:admin:::tag-definition/owner true",
		"admin:PutTagDefinition arn:cactusfs:admin:::tag-definition/stage ",
		"admin:PutTagDefinition arn:cactusfs:admin:::tag-definition/team ",
		"admin:DeleteTagDefinition arn:cactusfs:admin:::tag-definition/team ",
	}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("the changes in the trail:\n%s\nwant\n%s", strings.Join(changes, "\n"), strings.Join(want, "\n"))
	}
}

// TestTagDefinitionsWhenTheStoreCannot: the store's refusal is the answer -
// domain.ErrUnavailable while a member does not know the catalogue - and a
// definition that cannot be kept is refused before the store is asked.
func TestTagDefinitionsWhenTheStoreCannot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTagDefinitionFixture(t)
	f.meta.FailWrites = domain.ErrUnavailable
	if _, err := f.catalogue.Create(ctx, f.root, app.TagDefinitionInput{Key: "stage"}); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Create = %v, want %v", err, domain.ErrUnavailable)
	}
	if err := f.catalogue.Delete(ctx, f.root, "stage"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Delete = %v, want %v", err, domain.ErrUnavailable)
	}
	if _, err := f.catalogue.Put(ctx, f.root, app.TagDefinitionInput{Key: strings.Repeat("k", 129)}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Put of a key too long = %v, want %v", err, domain.ErrInvalidArgument)
	}
}
