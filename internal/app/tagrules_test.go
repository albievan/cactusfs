package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// secretValue is a value of a tag that no refusal may repeat: a refusal
// names the rule, never a value.
const secretValue = "secret-value-7c1d"

// tags makes a list of tags from pairs of key and value.
func tags(pairs ...string) []object.Tag {
	var out []object.Tag
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, object.Tag{Key: pairs[i], Value: pairs[i+1]})
	}
	return out
}

func (f *fixture) setTagRules(ctx context.Context, t *testing.T, bucketName string, rules bucket.TagRules) {
	t.Helper()
	if err := f.buckets.SetTagRules(ctx, porttest.Root(), bucketName, rules); err != nil {
		t.Fatalf("SetTagRules(%s): %v", bucketName, err)
	}
}

// createBucketIn is createBucket with the caller's context.
func (f *fixture) createBucketIn(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

// uploadPartIn is uploadPart with the caller's context.
func (f *fixture) uploadPartIn(ctx context.Context, t *testing.T, u object.Upload, number int, data []byte) object.Part {
	t.Helper()
	part, err := f.objects.UploadPart(ctx, porttest.Root(), app.UploadPartInput{
		Bucket: u.Bucket, Key: u.Key, UploadID: u.ID, Number: number, Body: bytes.NewReader(data), Size: int64(len(data)),
		Verify: func() (string, error) { return partTag(data), nil },
	})
	if err != nil {
		t.Fatalf("UploadPart %d: %v", number, err)
	}
	return part
}

// draws are numbers drawn from a hash of a seed and of how many were drawn
// before (gosec G404 refuses math/rand), so that every run of a seed draws
// the same ones.
type draws struct {
	seed  uint64
	drawn uint64
}

// IntN returns a number in [0, n), for n of at most 65536.
func (d *draws) IntN(n int) int {
	h := sha256.Sum256(fmt.Appendf(nil, "%d/%d", d.seed, d.drawn))
	d.drawn++
	return int(binary.BigEndian.Uint16(h[:2])) % n
}

// TestBucketTagRules: the rules of a bucket are read as they were set, all
// off where there are none; rules that cannot be kept are refused and the
// rules before kept; each use case asks the action the contract names.
func TestBucketTagRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucketIn(ctx, t, "b")
	rules := bucket.TagRules{Lowercase: true, CatalogueOnly: true, Required: []bucket.RequiredTag{
		{Key: "stage", Values: []string{"draft", "final"}}, {SubBucket: "reports", Key: "owner"},
	}}

	if got, err := f.buckets.TagRules(ctx, porttest.Root(), "b"); err != nil || !got.Empty() {
		t.Fatalf("the rules of a bucket without = %+v, %v", got, err)
	}
	steps := []struct {
		name string
		do   func() error
		want error
	}{
		{"set", func() error { return f.buckets.SetTagRules(ctx, porttest.Root(), "b", rules) }, nil},
		{"rules that cannot be kept", func() error {
			return f.buckets.SetTagRules(ctx, porttest.Root(), "b", bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{{Key: "Stage"}}})
		}, domain.ErrInvalidArgument},
		{"a key no tag can have", func() error {
			return f.buckets.SetTagRules(ctx, porttest.Root(), "b", bucket.TagRules{Required: []bucket.RequiredTag{{Key: "a&b"}}})
		}, domain.ErrInvalidArgument},
		{"a missing bucket", func() error { return f.buckets.SetTagRules(ctx, porttest.Root(), "missing", rules) }, domain.ErrBucketNotFound},
		{"set by nobody", func() error { return f.buckets.SetTagRules(ctx, anonymous(), "b", bucket.TagRules{}) }, domain.ErrAccessDenied},
		{"removed by nobody", func() error { return f.buckets.DeleteTagRules(ctx, anonymous(), "b") }, domain.ErrAccessDenied},
		{"read by nobody", func() error { _, err := f.buckets.TagRules(ctx, anonymous(), "b"); return err }, domain.ErrAccessDenied},
		{"read of a missing bucket", func() error { _, err := f.buckets.TagRules(ctx, porttest.Root(), "missing"); return err }, domain.ErrBucketNotFound},
	}
	for _, step := range steps {
		if err := step.do(); !errors.Is(err, step.want) || (step.want == nil && err != nil) {
			t.Errorf("%s = %v, want %v", step.name, err, step.want)
		}
	}
	// What was refused changed nothing.
	if got, err := f.buckets.TagRules(ctx, porttest.Root(), "b"); err != nil || !reflect.DeepEqual(got, rules) {
		t.Errorf("TagRules = %+v, %v; want %+v", got, err, rules)
	}
	if err := f.buckets.DeleteTagRules(ctx, porttest.Root(), "b"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.buckets.TagRules(ctx, porttest.Root(), "b"); err != nil || !got.Empty() {
		t.Errorf("the rules after removal = %+v, %v", got, err)
	}
	// Each asks what the contract names: reading s3:GetBucketTagRules,
	// setting and removing s3:PutBucketTagRules.
	asked := map[iam.Action]int{}
	for _, req := range f.authz.Requests() {
		asked[req.Action]++
	}
	if asked[iam.ActionGetBucketTagRules] == 0 || asked[iam.ActionPutBucketTagRules] == 0 {
		t.Errorf("the actions asked: %v", asked)
	}

	// Rules this release cannot read are not taken for none.
	for document, want := range map[string]error{
		`{"version":2,"lowercase":false,"catalogue_only":false,"required":[]}`: domain.ErrUnavailable,
		`{"version":1,"lowercase":false,"unknown":true}`:                       domain.ErrIntegrity,
	} {
		if err := f.meta.SetBucketConfig(ctx, "b", bucket.ConfigTagRules, []byte(document)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.buckets.TagRules(ctx, porttest.Root(), "b"); !errors.Is(err, want) {
			t.Errorf("TagRules of %s = %v, want %v", document, err, want)
		}
		if _, err := f.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1}); !errors.Is(err, want) {
			t.Errorf("a put under %s = %v, want %v", document, err, want)
		}
		if _, err := f.buckets.SetTags(ctx, porttest.Root(), "b", tags("a", "b")); !errors.Is(err, want) {
			t.Errorf("the bucket's tags under %s = %v, want %v", document, err, want)
		}
	}
}

// TestTagRulesAreAuthorized: of the built-in roles, those holding s3:*
// read and change a bucket's tag rules; no other does.
func TestTagRulesAreAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	buckets := app.NewBucketService(f.meta, f.authz, f.clock)
	if err := buckets.Create(ctx, f.root, "site"); err != nil {
		t.Fatal(err)
	}
	for role, allowed := range map[string]bool{
		iam.RoleClusterAdmin: true, iam.RoleBucketAdmin: true, iam.RoleReadWrite: false, iam.RoleReadOnly: false, iam.RoleAuditor: false,
	} {
		p := f.as(t, f.user(t, "user-"+role, role))
		calls := map[string]func() error{
			"read":   func() error { _, err := buckets.TagRules(ctx, p, "site"); return err },
			"set":    func() error { return buckets.SetTagRules(ctx, p, "site", bucket.TagRules{Lowercase: true}) },
			"remove": func() error { return buckets.DeleteTagRules(ctx, p, "site") },
		}
		for name, call := range calls {
			err := call()
			if allowed != (err == nil) || (!allowed && !errors.Is(err, domain.ErrAccessDenied)) {
				t.Errorf("%s by %s = %v, allowed: %v", name, role, err, allowed)
			}
		}
	}
}

// TestBucketTagsKeepTheRuleOfCase: a bucket's own tags are kept in lower
// case where its rules say so, and as sent where not; two keys that become
// one, and a key given twice, are refused.
func TestBucketTagsKeepTheRuleOfCase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucketIn(ctx, t, "b")
	for _, tt := range []struct {
		name  string
		rules bucket.TagRules
		tags  []object.Tag
		want  map[string]string
		err   error
	}{
		{"no rules", bucket.TagRules{}, tags("Team", "Web"), map[string]string{"Team": "Web"}, nil},
		{"rules without the case", bucket.TagRules{Required: []bucket.RequiredTag{{Key: "stage"}}}, tags("Team", "Web"), map[string]string{"Team": "Web"}, nil},
		{"lower case", bucket.TagRules{Lowercase: true}, tags("Team", "Web", "env", "PROD"), map[string]string{"team": "web", "env": "prod"}, nil},
		{"two keys that become one", bucket.TagRules{Lowercase: true}, tags("Env", "a", "env", "b"), nil, domain.ErrInvalidTag},
		{"a key given twice", bucket.TagRules{}, tags("env", "a", "env", "a"), nil, domain.ErrInvalidTag},
		{"none", bucket.TagRules{Lowercase: true}, nil, nil, nil},
	} {
		f.setTagRules(ctx, t, "b", tt.rules)
		if _, err := f.buckets.SetTags(ctx, porttest.Root(), "b", tags("before", "1")); err != nil {
			t.Fatal(err)
		}
		kept, err := f.buckets.SetTags(ctx, porttest.Root(), "b", tt.tags)
		if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) || !maps.Equal(kept, tt.want) {
			t.Errorf("%s: SetTags = %v, %v; want %v, %v", tt.name, kept, err, tt.want, tt.err)
		}
		stored, err := f.buckets.Tags(ctx, porttest.Root(), "b")
		want := tt.want
		if tt.err != nil {
			// Refused: the tags before are kept.
			want = map[string]string{"before": "1"}
		}
		if err != nil || !maps.Equal(stored, want) {
			t.Errorf("%s: the stored tags = %v, %v; want %v", tt.name, stored, err, want)
		}
		// Neither error is the rule's: these are tags that cannot be kept.
		if errors.Is(err, domain.ErrTagRule) {
			t.Errorf("%s: %v is a broken rule", tt.name, err)
		}
	}
}

// The rules of the enforcement table: tags in lower case, a stage that must
// be draft or final, and an owner for every object of the sub-bucket
// "reports".
var enforced = bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{
	{Key: "stage", Values: []string{"draft", "final"}}, {SubBucket: "reports", Key: "owner"},
}}

// TestTagRulesAreEnforced: every way an object comes to have tags - a put,
// a copy with the source's tags or with tags of its own and either
// metadata directive, a change of its tags, the removal of its tags, a
// multipart upload when it begins and when it completes - keeps the
// bucket's rules: tags are kept in lower case, and tags the rules do not
// allow are refused with domain.ErrTagRule, whose message names the rule
// and never a value. What is refused changes nothing.
func TestTagRulesAreEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()
	put := func(f *fixture, key string, list []object.Tag) (object.Version, error) {
		return f.objects.Put(ctx, root, app.PutInput{Bucket: "b", Key: key, Body: strings.NewReader("x"), Size: 1, Tags: list})
	}
	copyOf := func(f *fixture, key string, replaceMetadata, replaceTags bool, list []object.Tag) (object.Version, error) {
		out, err := f.objects.Copy(ctx, root, app.CopyInput{
			Source:          app.GetInput{Bucket: "src", Key: "source"},
			Destination:     app.PutInput{Bucket: "b", Key: key, Tags: list, ContentType: "text/plain"},
			ReplaceMetadata: replaceMetadata, ReplaceTags: replaceTags, Digest: tagged,
		})
		return out.Version, err
	}
	// The version of b/k that was there before the rules: tagged outside
	// them, in upper case.
	existing := app.LockInput{Bucket: "b", Key: "k"}
	upload := func(f *fixture, key string, list []object.Tag) (object.Version, error) {
		u, err := f.objects.CreateUpload(ctx, root, app.CreateUploadInput{Bucket: "b", Key: key, Tags: list})
		if err != nil {
			return object.Version{}, err
		}
		part := f.uploadPartIn(ctx, t, u, 1, []byte("x"))
		return f.objects.CompleteUpload(ctx, root, app.CompleteUploadInput{Bucket: "b", Key: key, UploadID: u.ID, Parts: listed(part), ETag: "e"})
	}
	for _, tt := range []struct {
		name string
		// sourceTags are the tags of src/source, which a copy reads.
		sourceTags []object.Tag
		do         func(f *fixture) (object.Version, error)
		// want are the tags of the version made or changed; err the
		// refusal, when it is one.
		want map[string]string
		err  error
	}{
		{"a put", nil, func(f *fixture) (object.Version, error) { return put(f, "a", tags("Stage", "Draft")) }, map[string]string{"stage": "draft"}, nil},
		{"a put without tags", nil, func(f *fixture) (object.Version, error) { return put(f, "a", nil) }, nil, domain.ErrTagRule},
		{"a put with a value not allowed", nil, func(f *fixture) (object.Version, error) { return put(f, "a", tags("stage", secretValue)) }, nil, domain.ErrTagRule},
		{"a put into the sub-bucket without its tag", nil, func(f *fixture) (object.Version, error) { return put(f, "reports/q1", tags("stage", "final")) }, nil, domain.ErrTagRule},
		{"a put into the sub-bucket with it", nil, func(f *fixture) (object.Version, error) {
			return put(f, "reports/q1", tags("stage", "final", "Owner", "Albie"))
		}, map[string]string{"stage": "final", "owner": "albie"}, nil},
		{"a put whose keys become one", nil, func(f *fixture) (object.Version, error) { return put(f, "a", tags("Stage", "draft", "stage", "draft")) }, nil, domain.ErrInvalidTag},
		{"a put with a key twice", nil, func(f *fixture) (object.Version, error) { return put(f, "a", tags("stage", "draft", "stage", "final")) }, nil, domain.ErrInvalidTag},
		{
			"a copy with the source's tags", tags("STAGE", "FINAL", "x", "Y"), func(f *fixture) (object.Version, error) { return copyOf(f, "a", false, false, nil) },
			map[string]string{"stage": "final", "x": "y"},
			nil,
		},
		{
			"a copy with the source's tags and new metadata", tags("stage", "draft"), func(f *fixture) (object.Version, error) { return copyOf(f, "a", true, false, nil) },
			map[string]string{"stage": "draft"},
			nil,
		},
		{"a copy of a source without tags", nil, func(f *fixture) (object.Version, error) { return copyOf(f, "a", false, false, nil) }, nil, domain.ErrTagRule},
		{
			"a copy of a source whose tags break the rules", tags("stage", secretValue), func(f *fixture) (object.Version, error) { return copyOf(f, "a", true, false, nil) },
			nil, domain.ErrTagRule,
		},
		{"a copy with tags of its own", tags("stage", secretValue), func(f *fixture) (object.Version, error) {
			return copyOf(f, "a", false, true, tags("Stage", "Final"))
		}, map[string]string{"stage": "final"}, nil},
		{"a copy with tags of its own and new metadata", nil, func(f *fixture) (object.Version, error) {
			return copyOf(f, "a", true, true, tags("stage", "draft"))
		}, map[string]string{"stage": "draft"}, nil},
		{"a copy with no tags of its own", tags("stage", "draft"), func(f *fixture) (object.Version, error) { return copyOf(f, "a", false, true, nil) }, nil, domain.ErrTagRule},
		{"a copy with tags of its own the rules refuse", nil, func(f *fixture) (object.Version, error) {
			return copyOf(f, "a", true, true, tags("stage", secretValue))
		}, nil, domain.ErrTagRule},
		{"setting the tags", nil, func(f *fixture) (object.Version, error) {
			return f.objects.SetTags(ctx, root, existing, tags("STAGE", "Final"))
		}, map[string]string{"stage": "final"}, nil},
		{"setting tags the rules refuse", nil, func(f *fixture) (object.Version, error) {
			return f.objects.SetTags(ctx, root, existing, tags("other", secretValue))
		}, nil, domain.ErrTagRule},
		{"removing the tags", nil, func(f *fixture) (object.Version, error) { return f.objects.SetTags(ctx, root, existing, nil) }, nil, domain.ErrTagRule},
		{"an upload", nil, func(f *fixture) (object.Version, error) { return upload(f, "a", tags("Stage", "DRAFT")) }, map[string]string{"stage": "draft"}, nil},
		{"an upload without tags", nil, func(f *fixture) (object.Version, error) { return upload(f, "a", nil) }, nil, domain.ErrTagRule},
		{"an upload into the sub-bucket without its tag", nil, func(f *fixture) (object.Version, error) {
			return upload(f, "reports/big", tags("stage", "draft"))
		}, nil, domain.ErrTagRule},
		{"an upload with a key twice", nil, func(f *fixture) (object.Version, error) {
			return upload(f, "a", tags("stage", "draft", "stage", "draft"))
		}, nil, domain.ErrInvalidTag},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucketIn(ctx, t, "b")
			f.createBucketIn(ctx, t, "src")
			if _, err := f.objects.Put(ctx, root, app.PutInput{Bucket: "src", Key: "source", Body: strings.NewReader("source"), Size: 6, Tags: tt.sourceTags}); err != nil {
				t.Fatal(err)
			}
			before, err := f.objects.Put(ctx, root, app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1, Tags: tags("Stage", "Draft")})
			if err != nil {
				t.Fatal(err)
			}
			f.setTagRules(ctx, t, "b", enforced)
			got, err := tt.do(f)
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("= %v, want %v", err, tt.err)
			}
			if err != nil {
				if strings.Contains(err.Error(), secretValue) {
					t.Errorf("the refusal tells a value: %v", err)
				}
				if errors.Is(tt.err, domain.ErrTagRule) && !strings.Contains(err.Error(), "tag") {
					t.Errorf("the refusal names no rule: %v", err)
				}
				// Nothing was stored, and the version there before
				// kept its tags.
				if v, err := f.objects.Head(ctx, root, "b", "a"); !errors.Is(err, domain.ErrObjectNotFound) {
					t.Errorf("a refused write left %+v (%v)", v, err)
				}
				if v, err := f.objects.Head(ctx, root, "b", "k"); err != nil || !maps.Equal(v.Tags, before.Tags) {
					t.Errorf("the version before = %v, %v", v.Tags, err)
				}
				return
			}
			if !maps.Equal(got.Tags, tt.want) {
				t.Errorf("the tags kept = %v, want %v", got.Tags, tt.want)
			}
			if err := enforced.Check(got.Key, got.Tags, nil); err != nil {
				t.Errorf("what was kept breaks the rules: %v", err)
			}
		})
	}
}

// TestCompletingAnUploadAfterTheRulesChanged: an upload is judged again when
// it is completed, by the rules of then and with the tags it was begun
// with, which completing does not change: an upload whose tags the rules no
// longer allow is not completed, and can still be aborted.
func TestCompletingAnUploadAfterTheRulesChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()
	for _, tt := range []struct {
		name   string
		before bucket.TagRules
		tags   []object.Tag
		after  bucket.TagRules
		err    error
	}{
		{"no change", enforced, tags("stage", "draft"), enforced, nil},
		{
			"a value no longer allowed", enforced, tags("stage", "draft"),
			bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{{Key: "stage", Values: []string{"final"}}}},
			domain.ErrTagRule,
		},
		{"a tag required since", bucket.TagRules{}, nil, enforced, domain.ErrTagRule},
		// Refused by the case alone: any stage would do.
		{"lower case since", bucket.TagRules{}, tags("stage", "Draft"), bucket.TagRules{Lowercase: true, Required: []bucket.RequiredTag{{Key: "stage"}}}, domain.ErrTagRule},
		{"lower case since, in lower case", bucket.TagRules{}, tags("stage", "draft"), bucket.TagRules{Lowercase: true}, nil},
		{"no rules since", enforced, tags("stage", "draft"), bucket.TagRules{}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucketIn(ctx, t, "b")
			f.setTagRules(ctx, t, "b", tt.before)
			u, err := f.objects.CreateUpload(ctx, root, app.CreateUploadInput{Bucket: "b", Key: "big", Tags: tt.tags})
			if err != nil {
				t.Fatal(err)
			}
			part := f.uploadPartIn(ctx, t, u, 1, []byte("x"))
			f.setTagRules(ctx, t, "b", tt.after)
			v, err := f.objects.CompleteUpload(ctx, root, app.CompleteUploadInput{Bucket: "b", Key: "big", UploadID: u.ID, Parts: listed(part), ETag: "e"})
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("CompleteUpload = %v, want %v", err, tt.err)
			}
			if err == nil {
				if !maps.Equal(v.Tags, u.Tags) {
					t.Errorf("the version's tags = %v, the upload's %v", v.Tags, u.Tags)
				}
				return
			}
			if err := f.objects.AbortUpload(ctx, root, app.UploadRef{Bucket: "b", Key: "big", UploadID: u.ID}); err != nil {
				t.Errorf("the upload that could not be completed cannot be aborted: %v", err)
			}
		})
	}
}

// failingCatalogue is a catalogue that cannot be read.
type failingCatalogue struct{ ports.TagDefinitionStore }

func (failingCatalogue) ListTagDefinitions(context.Context) ([]tagging.Definition, error) {
	return nil, fmt.Errorf("%w: no leader", domain.ErrNotLeader)
}

// TestTagsOfTheCatalogueOnly: a bucket whose rules take only tags of the
// catalogue takes those, with values the definitions allow, and refuses
// others as a broken rule; a catalogue that cannot be read - or that the
// node was not given - refuses nothing as a broken rule: the request is
// refused as one to try again (domain.ErrUnavailable).
func TestTagsOfTheCatalogueOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucketIn(ctx, t, "b")
	for _, d := range []tagging.Definition{{Key: "stage", Values: []string{"draft", "final"}}, {Key: "project"}} {
		if err := f.meta.PutTagDefinition(ctx, d, audit.Record{}); err != nil {
			t.Fatal(err)
		}
	}
	f.setTagRules(ctx, t, "b", bucket.TagRules{Lowercase: true, CatalogueOnly: true})
	reading := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.TagDefinitions = f.meta })
	failing := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.TagDefinitions = failingCatalogue{f.meta} })
	for _, tt := range []struct {
		name    string
		objects *app.ObjectService
		tags    []object.Tag
		err     error
	}{
		{"defined tags", reading, tags("Stage", "Final", "project", "anything"), nil},
		{"no tags", reading, nil, nil},
		{"a tag nobody defined", reading, tags("colour", secretValue), domain.ErrTagRule},
		{"a value the definition does not list", reading, tags("stage", secretValue), domain.ErrTagRule},
		{"no catalogue on the node", f.objects, tags("stage", "draft"), domain.ErrUnavailable},
		{"no catalogue on the node, and no tags", f.objects, nil, nil},
		{"a catalogue that cannot be read", failing, tags("stage", "draft"), domain.ErrUnavailable},
	} {
		_, err := tt.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1, Tags: tt.tags})
		if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
			t.Errorf("%s: Put = %v, want %v", tt.name, err, tt.err)
		}
		if errors.Is(tt.err, domain.ErrUnavailable) && errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: a catalogue that cannot be read is taken for a broken rule: %v", tt.name, err)
		}
		if err != nil && strings.Contains(err.Error(), secretValue) {
			t.Errorf("%s: the refusal tells a value: %v", tt.name, err)
		}
	}
}

// TestNoStoredVersionBreaksARule is the property of the rules: whatever the
// rules, the catalogue, and the writes - puts, copies, changes and removals
// of tags, multipart uploads, with tags drawn at random in mixed case -
// every version that was stored or changed under the rules keeps them, and
// every write that was refused was refused as tags that cannot be kept.
func TestNoStoredVersionBreaksARule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()
	keys := []string{"stage", "Stage", "owner", "team", "x"}
	values := []string{"draft", "Draft", "final", "a", ""}
	objectKeys := []string{"k", "reports/q1", "reports/q2", "other/k"}
	// What the writes came to, over every seed: the property says nothing
	// unless some were kept and some refused.
	var kept, refused, checked int
	for seed := range uint64(40) {
		rng := &draws{seed: seed}
		pick := func(list []string) string { return list[rng.IntN(len(list))] }
		randomTags := func() []object.Tag {
			var out []object.Tag
			for range rng.IntN(4) {
				out = append(out, object.Tag{Key: pick(keys), Value: pick(values)})
			}
			return out
		}
		rules := bucket.TagRules{Lowercase: rng.IntN(2) == 0, CatalogueOnly: rng.IntN(3) == 0}
		for _, key := range []string{"stage", "owner"} {
			if rng.IntN(2) == 0 {
				continue
			}
			required := bucket.RequiredTag{Key: key}
			if rng.IntN(2) == 0 {
				required.SubBucket = "reports"
			}
			if rng.IntN(2) == 0 {
				required.Values = []string{"draft", "final"}
			}
			if !rules.Lowercase {
				// Keys and values of rules that do not lower-case
				// may be in any case.
				required.Values = append(required.Values, "Draft")
			}
			rules.Required = append(rules.Required, required)
		}
		f := newFixture(t)
		f.createBucketIn(ctx, t, "b")
		f.createBucketIn(ctx, t, "src")
		if err := f.meta.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		catalogue := map[string][]string{"stage": {"draft", "final"}, "team": nil}
		if rng.IntN(2) == 0 {
			catalogue["owner"] = []string{"a"}
		}
		for key, allowed := range catalogue {
			if err := f.meta.PutTagDefinition(ctx, tagging.Definition{Key: key, Values: allowed}, audit.Record{}); err != nil {
				t.Fatal(err)
			}
		}
		objects := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.TagDefinitions = f.meta })
		// Sources to copy from, outside the rules, and versions of b from
		// before them, which are left as they are.
		for i := range 3 {
			if _, err := objects.Put(ctx, root, app.PutInput{Bucket: "src", Key: fmt.Sprint(i), Body: strings.NewReader("s"), Size: 1, Tags: randomTags()}); err != nil &&
				!errors.Is(err, domain.ErrInvalidTag) {
				t.Fatal(err)
			}
		}
		for _, key := range objectKeys {
			if _, err := objects.Put(ctx, root, app.PutInput{Bucket: "b", Key: key, Body: strings.NewReader("old"), Size: 3}); err != nil {
				t.Fatal(err)
			}
		}
		f.setTagRules(ctx, t, "b", rules)
		// The versions of b that the rules were set after.
		old := map[object.ID]bool{}
		page, err := objects.ListVersions(ctx, root, "b", app.VersionListOptions{MaxKeys: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Versions {
			old[v.Version.VersionID] = true
		}

		for op := range 12 {
			key := pick(objectKeys)
			var err error
			changed := object.ID{}
			// given are the tags the write asked for - those sent, or a
			// copy's source's - and stored the version it made or
			// changed: what is kept must be given, as the rules keep it.
			var given []object.Tag
			var stored object.Version
			made := true
			switch rng.IntN(5) {
			case 0:
				given = randomTags()
				stored, err = objects.Put(ctx, root, app.PutInput{Bucket: "b", Key: key, Body: strings.NewReader("x"), Size: 1, Tags: given})
			case 1:
				replaceTags := rng.IntN(2) == 0
				given = randomTags()
				var out app.CopyOutput
				out, err = objects.Copy(ctx, root, app.CopyInput{
					Source:          app.GetInput{Bucket: "src", Key: fmt.Sprint(rng.IntN(3))},
					Destination:     app.PutInput{Bucket: "b", Key: key, Tags: given},
					ReplaceMetadata: rng.IntN(2) == 0, ReplaceTags: replaceTags, Digest: tagged,
				})
				stored = out.Version
				if !replaceTags {
					given = object.TagList(out.Source.Tags)
				}
				if errors.Is(err, domain.ErrObjectNotFound) {
					// A source whose own tags could not be kept.
					err, made = nil, false
				}
			case 2, 3:
				given = randomTags()
				if rng.IntN(3) == 0 {
					given = nil
				}
				stored, err = objects.SetTags(ctx, root, app.LockInput{Bucket: "b", Key: key}, given)
				if err == nil {
					changed = stored.VersionID
				}
			case 4:
				given = randomTags()
				var u object.Upload
				u, err = objects.CreateUpload(ctx, root, app.CreateUploadInput{Bucket: "b", Key: key, Tags: given})
				if err == nil {
					part, partErr := objects.UploadPart(ctx, root, app.UploadPartInput{
						Bucket: "b", Key: key, UploadID: u.ID, Number: 1, Body: bytes.NewReader([]byte("p")), Size: 1,
						Verify: func() (string, error) { return partTag([]byte("p")), nil },
					})
					if partErr != nil {
						t.Fatal(partErr)
					}
					stored, err = objects.CompleteUpload(ctx, root, app.CompleteUploadInput{Bucket: "b", Key: key, UploadID: u.ID, Parts: listed(part), ETag: "e"})
				}
			}
			if err != nil && !errors.Is(err, domain.ErrInvalidTag) {
				t.Fatalf("seed %d, operation %d: %v", seed, op, err)
			}
			if err != nil {
				refused++
			} else {
				kept++
			}
			if err == nil && made {
				// Nothing given is dropped or changed but as the rules
				// keep it.
				asMap, mapErr := object.TagMap(given)
				want, keepErr := rules.Keep(asMap, object.MaxObjectTags)
				if mapErr != nil || keepErr != nil || !maps.Equal(stored.Tags, want) {
					t.Errorf("seed %d, operation %d: given %v, stored %v; the rules keep %v (%v, %v)", seed, op, given, stored.Tags, want, mapErr, keepErr)
				}
			}
			if !changed.IsZero() {
				// A version from before the rules whose tags were
				// changed under them is held to them from then on.
				delete(old, changed)
			}
		}
		defined := func(key string) ([]string, bool) {
			allowed, ok := catalogue[key]
			return allowed, ok
		}
		page, err = objects.ListVersions(ctx, root, "b", app.VersionListOptions{MaxKeys: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, listedVersion := range page.Versions {
			v := listedVersion.Version
			if old[v.VersionID] || v.DeleteMarker {
				continue
			}
			checked++
			if err := rules.Check(v.Key, v.Tags, defined); err != nil {
				t.Errorf("seed %d: rules %+v: version %s of %s with tags %v: %v", seed, rules, v.VersionID, v.Key, v.Tags, err)
			}
		}
	}
	if kept < 50 || refused < 50 || checked < 50 {
		t.Errorf("%d writes kept, %d refused, %d versions checked: too few to say anything", kept, refused, checked)
	}
	t.Logf("%d writes kept, %d refused, %d versions checked", kept, refused, checked)
}

// TestTagRulesAreReadOnceARequest: a put asks the bucket for its rules once,
// and the catalogue at most once, however many tags it has.
func TestTagRulesAreReadOnceARequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucketIn(ctx, t, "b")
	for _, key := range []string{"a", "b", "c"} {
		if err := f.meta.PutTagDefinition(ctx, tagging.Definition{Key: key}, audit.Record{}); err != nil {
			t.Fatal(err)
		}
	}
	f.setTagRules(ctx, t, "b", bucket.TagRules{CatalogueOnly: true, Required: []bucket.RequiredTag{{Key: "a"}, {Key: "b"}}})
	counted := &countingConfig{BucketStore: f.meta}
	catalogue := &countingCatalogue{TagDefinitionStore: f.meta}
	objects := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Buckets, cfg.TagDefinitions = counted, catalogue })
	if _, err := objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1, Tags: tags("a", "1", "b", "2", "c", "3")}); err != nil {
		t.Fatal(err)
	}
	if counted.reads[bucket.ConfigTagRules] != 1 || catalogue.lists != 1 {
		t.Errorf("the rules were read %d times and the catalogue %d", counted.reads[bucket.ConfigTagRules], catalogue.lists)
	}
}

type countingConfig struct {
	ports.BucketStore
	reads map[string]int
}

func (c *countingConfig) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	if c.reads == nil {
		c.reads = map[string]int{}
	}
	c.reads[kind]++
	return c.BucketStore.GetBucketConfig(ctx, name, kind)
}

type countingCatalogue struct {
	ports.TagDefinitionStore
	lists int
}

func (c *countingCatalogue) ListTagDefinitions(ctx context.Context) ([]tagging.Definition, error) {
	c.lists++
	return c.TagDefinitionStore.ListTagDefinitions(ctx)
}

// TestTagsGivenAskToTag: whoever gives tags with a new object - a put, or a
// multipart upload when it begins - must be allowed s3:PutObjectTagging as
// well as s3:PutObject; without tags, s3:PutObject is enough. The refusal
// is the authorizer's, for that action, and nothing is begun.
func TestTagsGivenAskToTag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := allowing("s3:PutObject")
	tagger := allowing("s3:PutObject", "s3:PutObjectTagging")
	for _, tt := range []struct {
		name string
		p    iam.Principal
		do   func(f *fixture, p iam.Principal) error
		want error
	}{
		{"an upload with tags, by a writer", writer, func(f *fixture, p iam.Principal) error {
			_, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "b", Key: "big", Tags: tags("stage", "draft")})
			return err
		}, domain.ErrAccessDenied},
		{"an upload without tags, by a writer", writer, func(f *fixture, p iam.Principal) error {
			_, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "b", Key: "big"})
			return err
		}, nil},
		{"an upload with tags, by one who may tag", tagger, func(f *fixture, p iam.Principal) error {
			_, err := f.objects.CreateUpload(ctx, p, app.CreateUploadInput{Bucket: "b", Key: "big", Tags: tags("stage", "draft")})
			return err
		}, nil},
		{"a put with tags, by a writer", writer, func(f *fixture, p iam.Principal) error {
			_, err := f.objects.Put(ctx, p, app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1, Tags: tags("stage", "draft")})
			return err
		}, domain.ErrAccessDenied},
		{"a put without tags, by a writer", writer, func(f *fixture, p iam.Principal) error {
			_, err := f.objects.Put(ctx, p, app.PutInput{Bucket: "b", Key: "k", Body: strings.NewReader("x"), Size: 1})
			return err
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.createBucketIn(ctx, t, "b")
			err := tt.do(f, tt.p)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("= %v, want %v", err, tt.want)
			}
			asked := false
			for _, req := range f.authz.Requests() {
				asked = asked || req.Action == iam.ActionPutObjectTagging
			}
			if gave := strings.Contains(tt.name, "with tags"); asked != gave {
				t.Errorf("s3:PutObjectTagging asked: %v; tags given: %v", asked, gave)
			}
			if err != nil {
				uploads, listErr := f.objects.ListUploads(ctx, porttest.Root(), app.ListUploadsInput{Bucket: "b", Max: 100})
				if listErr != nil || len(uploads.Uploads) != 0 {
					t.Errorf("a refused upload was begun: %+v, %v", uploads, listErr)
				}
			}
		})
	}
}
