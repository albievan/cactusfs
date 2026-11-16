package app_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// replConfigFixture is the use cases of replication configuration over the
// stores of the target fixture, with the buckets photos (keeps versions),
// suspended (versions suspended), plain (never kept versions) and copies (a
// replica of another deployment); the other deployments site-b (open to
// photos, suspended, plain and copies) and site-c (open to logs only), and
// the webhook hook (open to photos).
type replConfigFixture struct {
	*targetFixture
	replication *app.ReplicationService
}

func newReplConfigFixture(t *testing.T) *replConfigFixture {
	t.Helper()
	ctx := context.Background()
	f := &replConfigFixture{targetFixture: newTargetFixture(t)}
	f.replication = app.NewReplicationService(app.ReplicationServiceConfig{
		Buckets: f.meta, Store: f.meta, Targets: f.meta, Authz: f.authz, Auditor: f.auditor,
	})
	for name, versioning := range map[string]bucket.Versioning{
		"photos": bucket.VersioningEnabled, "suspended": bucket.VersioningSuspended, "plain": "", "copies": bucket.VersioningEnabled,
	} {
		if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatal(err)
		}
		if versioning != "" {
			if err := f.meta.SetBucketVersioning(ctx, name, versioning); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.replication.SetReplicaMode(ctx, f.root, "copies", "site-z"); err != nil {
		t.Fatalf("SetReplicaMode: %v", err)
	}
	for _, in := range []app.TargetInput{
		webhook("hook"),
		siteTarget("site-b", "photos", "suspended", "plain", "copies"),
		siteTarget("site-c", "logs"),
	} {
		if _, err := f.targets.Put(ctx, f.root, in); err != nil {
			t.Fatalf("Put of target %s: %v", in.Name, err)
		}
	}
	return f
}

// siteTarget is another deployment, open to the buckets named.
func siteTarget(name string, buckets ...string) app.TargetInput {
	return app.TargetInput{
		Name: name, Kind: delivery.KindS3, Endpoint: "https://s3." + name + ".example", Region: "eu-west-1",
		Buckets: buckets, Auth: delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"}, Secret: concealedMark,
	}
}

// replRule is an enabled rule of the priority given.
func replRule(id string, priority int, prefix string) bucket.ReplicationRule {
	return bucket.ReplicationRule{ID: id, Enabled: true, Priority: priority, Prefix: prefix}
}

// replConfig is a configuration through target to the bucket "copy".
func replConfig(target string, rules ...bucket.ReplicationRule) bucket.ReplicationConfig {
	return bucket.ReplicationConfig{Target: target, Bucket: "copy", Rules: rules}
}

// storedReplication returns the document the store holds for the bucket.
func (f *replConfigFixture) storedReplication(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := f.meta.GetBucketConfig(context.Background(), name, bucket.ConfigReplication)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestReplicationConfigurationIsAuthorized: reading the configuration
// needs s3:GetReplicationConfiguration, setting and removing it
// s3:PutReplicationConfiguration, both on the bucket; of the built-in roles
// only cluster-admin and bucket-admin hold them. A refusal is audited and
// changes nothing.
func TestReplicationConfigurationIsAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplConfigFixture(t)
	first := replConfig("site-b", replRule("first", 1, "logs/"))
	if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", first); err != nil {
		t.Fatal(err)
	}
	before := f.storedReplication(t, "photos")
	roles := map[string]string{
		iam.RoleClusterAdmin: "admin-user", iam.RoleBucketAdmin: "bucket-user", iam.RoleReadWrite: "writer-user",
		iam.RoleReadOnly: "reader-user", iam.RoleAuditor: "auditor-user",
	}
	principals := map[string]iam.Principal{}
	for role, user := range roles {
		principals[role] = f.as(t, f.user(t, user, role))
	}
	for _, op := range []struct {
		name   string
		action iam.Action
		call   func(p iam.Principal) error
	}{
		{"get", iam.ActionGetReplication, func(p iam.Principal) error {
			_, err := f.replication.Configuration(ctx, p, "photos")
			return err
		}},
		{"set", iam.ActionPutReplication, func(p iam.Principal) error {
			_, err := f.replication.SetConfiguration(ctx, p, "photos", first)
			return err
		}},
		{"delete", iam.ActionPutReplication, func(p iam.Principal) error {
			return f.replication.DeleteConfiguration(ctx, p, "photos")
		}},
	} {
		for role, p := range principals {
			allowed := role == iam.RoleClusterAdmin || role == iam.RoleBucketAdmin
			err := op.call(p)
			switch {
			case allowed && err != nil:
				t.Errorf("%s by %s: %v", op.name, role, err)
			case !allowed && !errors.Is(err, domain.ErrAccessDenied):
				t.Errorf("%s by %s = %v, want ErrAccessDenied", op.name, role, err)
			}
			if allowed {
				// Put back what was there for the next caller.
				if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", first); err != nil {
					t.Fatal(err)
				}
				continue
			}
			found := false
			for _, r := range f.trail(t) {
				found = found || (r.Outcome == audit.Denied && r.Action == string(op.action) && r.Resource == "arn:aws:s3:::photos" && r.Actor == "user/"+roles[role])
			}
			if !found {
				t.Errorf("%s by %s: no denial of %s on the bucket in the trail", op.name, role, op.action)
			}
			if got := f.storedReplication(t, "photos"); !bytes.Equal(got, before) {
				t.Errorf("%s by %s changed the configuration to %s", op.name, role, got)
			}
		}
	}
}

// TestReplicationConfigurationSet: what is kept, what is refused - each
// with the sentinel its answer is made from - and that a refusal leaves
// what the bucket had.
func TestReplicationConfigurationSet(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		bucket string
		config bucket.ReplicationConfig
		// fail makes the store refuse the change.
		fail error
		want error
		// kept is the configuration as kept, when it is.
		kept bucket.ReplicationConfig
	}{
		"one rule": {
			bucket: "photos", config: replConfig("site-b", replRule("first", 1, "")),
			kept: replConfig("site-b", replRule("first", 1, "")),
		},
		"rules in their order, one disabled, one with markers": {
			bucket: "photos",
			config: replConfig("site-b",
				replRule("logs", 2, "logs/"),
				bucket.ReplicationRule{ID: "off", Priority: 9, Prefix: "tmp/"},
				bucket.ReplicationRule{ID: "all", Enabled: true, Markers: true},
			),
			kept: replConfig("site-b",
				replRule("logs", 2, "logs/"),
				bucket.ReplicationRule{ID: "off", Priority: 9, Prefix: "tmp/"},
				bucket.ReplicationRule{ID: "all", Enabled: true, Markers: true},
			),
		},
		"IDs filled in, past an ID that is taken": {
			bucket: "photos",
			config: replConfig("site-b", replRule("", 0, "a/"), replRule("rule-2", 0, "b/"), replRule("", 0, "c/")),
			kept:   replConfig("site-b", replRule("rule-1", 0, "a/"), replRule("rule-2", 0, "b/"), replRule("rule-3", 0, "c/")),
		},
		"a bucket that is a replica":            {bucket: "copies", config: replConfig("site-b", replRule("r", 0, "")), want: domain.ErrInvalidBucketState},
		"a bucket whose versions are suspended": {bucket: "suspended", config: replConfig("site-b", replRule("r", 0, "")), want: domain.ErrVersioningRequired},
		"a bucket that never kept versions":     {bucket: "plain", config: replConfig("site-b", replRule("r", 0, "")), want: domain.ErrVersioningRequired},
		"a bucket that is not there":            {bucket: "missing", config: replConfig("site-b", replRule("r", 0, "")), want: domain.ErrBucketNotFound},
		"a target that is not there":            {bucket: "photos", config: replConfig("nowhere", replRule("r", 0, "")), want: domain.ErrInvalidArgument},
		"a target closed to the bucket":         {bucket: "photos", config: replConfig("site-c", replRule("r", 0, "")), want: domain.ErrInvalidArgument},
		"a webhook":                             {bucket: "photos", config: replConfig("hook", replRule("r", 0, "")), want: domain.ErrInvalidArgument},
		"a target name that is none":            {bucket: "photos", config: replConfig("Site!", replRule("r", 0, "")), want: domain.ErrInvalidArgument},
		"no rules":                              {bucket: "photos", config: replConfig("site-b"), want: domain.ErrInvalidArgument},
		"no configuration at all":               {bucket: "photos", want: domain.ErrInvalidArgument},
		"an ID twice": {
			bucket: "photos", config: replConfig("site-b", replRule("r", 0, "a"), replRule("r", 1, "b")), want: domain.ErrInvalidArgument,
		},
		"an ID that is too long": {
			bucket: "photos", config: replConfig("site-b", replRule(strings.Repeat("i", 256), 0, "")), want: domain.ErrInvalidArgument,
		},
		// Two enabled rules of one priority that some key matches both
		// would be settled by the order they were put in: refused.
		"one prefix beginning another at one priority": {
			bucket: "photos", config: replConfig("site-b", replRule("logs", 1, "logs/"), replRule("deep", 1, "logs/2026/")), want: domain.ErrInvalidArgument,
		},
		"every key and some keys at one priority": {
			bucket: "photos", config: replConfig("site-b", replRule("tmp", 0, "tmp/"), replRule("all", 0, "")), want: domain.ErrInvalidArgument,
		},
		"one prefix twice at one priority": {
			bucket: "photos", config: replConfig("site-b", replRule("a", 3, "x"), replRule("b", 3, "x")), want: domain.ErrInvalidArgument,
		},
		"a tie found past the IDs filled in": {
			bucket: "photos", config: replConfig("site-b", replRule("", 0, "a/"), replRule("", 0, "a/b")), want: domain.ErrInvalidArgument,
		},
		"overlapping prefixes at different priorities": {
			bucket: "photos",
			config: replConfig("site-b", replRule("logs", 1, "logs/"), replRule("deep", 2, "logs/2026/")),
			kept:   replConfig("site-b", replRule("logs", 1, "logs/"), replRule("deep", 2, "logs/2026/")),
		},
		"overlapping at one priority, one disabled": {
			bucket: "photos",
			config: replConfig("site-b", replRule("all", 0, ""), bucket.ReplicationRule{ID: "off", Prefix: "tmp/"}),
			kept:   replConfig("site-b", replRule("all", 0, ""), bucket.ReplicationRule{ID: "off", Prefix: "tmp/"}),
		},
		"a negative priority": {bucket: "photos", config: replConfig("site-b", replRule("r", -1, "")), want: domain.ErrInvalidArgument},
		"a prefix that is too long": {
			bucket: "photos", config: replConfig("site-b", replRule("r", 0, strings.Repeat("p", 1025))), want: domain.ErrInvalidArgument,
		},
		"a destination that is no bucket name": {
			bucket: "photos", config: bucket.ReplicationConfig{Target: "site-b", Bucket: "No_Bucket", Rules: []bucket.ReplicationRule{replRule("r", 0, "")}},
			want: domain.ErrInvalidArgument,
		},
		"too many rules": {bucket: "photos", config: manyRules(bucket.MaxReplicationRules + 1), want: domain.ErrInvalidArgument},
		"as many rules as there may be": {
			bucket: "photos", config: manyRules(bucket.MaxReplicationRules), kept: manyRules(bucket.MaxReplicationRules),
		},
		"below the level of replication": {
			bucket: "photos", config: replConfig("site-b", replRule("r", 0, "")), fail: domain.ErrUnavailable, want: domain.ErrUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newReplConfigFixture(t)
			// What the bucket had stays when a change is refused.
			if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("before", 0, ""))); err != nil {
				t.Fatal(err)
			}
			before := f.storedReplication(t, "photos")
			if tt.fail != nil {
				f.meta.FailConfig = map[string]error{bucket.ConfigReplication: tt.fail}
			}
			changes := len(f.trail(t))
			kept, err := f.replication.SetConfiguration(ctx, f.root, tt.bucket, tt.config)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("SetConfiguration = %v, want %v", err, tt.want)
				}
				if got := f.storedReplication(t, "photos"); !bytes.Equal(got, before) {
					t.Errorf("a refused change left %s", got)
				}
				if len(f.trail(t)) != changes {
					t.Errorf("a refused change was recorded as one: %+v", f.trail(t)[changes:])
				}
				return
			}
			if err != nil {
				t.Fatalf("SetConfiguration: %v", err)
			}
			wantDoc, err := tt.kept.Document()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(kept, tt.kept) {
				t.Errorf("SetConfiguration returned %+v, want %+v", kept, tt.kept)
			}
			if got := f.storedReplication(t, tt.bucket); !bytes.Equal(got, wantDoc) {
				t.Errorf("stored %s, want %s", got, wantDoc)
			}
			got, err := f.replication.Configuration(ctx, f.root, tt.bucket)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, kept) {
				t.Errorf("Configuration = %+v, SetConfiguration returned %+v", got, kept)
			}
			// The change is in the trail, with what it named.
			records := f.trail(t)
			last := records[len(records)-1]
			if last.Action != string(iam.ActionPutReplication) || last.Outcome != audit.Succeeded || last.Resource != "arn:aws:s3:::"+tt.bucket ||
				last.Details["target"] != tt.kept.Target || last.Details["destination"] != tt.kept.Bucket {
				t.Errorf("the change is recorded as %+v", last)
			}
		})
	}
}

// manyRules is a configuration of n enabled rules, each of its own prefix.
func manyRules(n int) bucket.ReplicationConfig {
	config := replConfig("site-b")
	for i := range n {
		id := "rule-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		config.Rules = append(config.Rules, replRule(id, i, id+"/"))
	}
	return config
}

// TestReplicationConfigurationLeavesTheCallersRules: the rules given are
// not changed by the IDs filled in.
func TestReplicationConfigurationLeavesTheCallersRules(t *testing.T) {
	t.Parallel()
	f := newReplConfigFixture(t)
	given := replConfig("site-b", replRule("", 0, ""))
	if _, err := f.replication.SetConfiguration(context.Background(), f.root, "photos", given); err != nil {
		t.Fatal(err)
	}
	if given.Rules[0].ID != "" {
		t.Errorf("the caller's rule became %+v", given.Rules[0])
	}
}

// TestReplicationRefusalsSayTheSame: a target that is not there, one of
// another kind and one that does not admit the bucket are refused in the
// same words, so that a bucket's administrator learns nothing of targets
// that are not open to the bucket.
func TestReplicationRefusalsSayTheSame(t *testing.T) {
	t.Parallel()
	f := newReplConfigFixture(t)
	said := map[string]bool{}
	for _, target := range []string{"nowhere", "hook", "site-c"} {
		_, err := f.replication.SetConfiguration(context.Background(), f.root, "photos", replConfig(target, replRule("r", 0, "")))
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Fatalf("target %s = %v", target, err)
		}
		said[strings.ReplaceAll(err.Error(), target, "T")] = true
		for _, word := range []string{"example", "logs", "webhook", "s3"} {
			if strings.Contains(err.Error(), word) {
				t.Errorf("the refusal of %s tells %q: %v", target, word, err)
			}
		}
	}
	if len(said) != 1 {
		t.Errorf("the refusals differ: %v", said)
	}
}

// TestReplicationConfigurationDelete: a configuration is removed, a bucket
// without one is left as it is, and each removal is audited; what the
// store refuses is passed on, and changes nothing.
func TestReplicationConfigurationDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplConfigFixture(t)
	if _, err := f.replication.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("r", 0, ""))); err != nil {
		t.Fatal(err)
	}
	f.meta.FailConfig = map[string]error{bucket.ConfigReplication: domain.ErrUnavailable}
	if err := f.replication.DeleteConfiguration(ctx, f.root, "photos"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("DeleteConfiguration below the level of replication = %v", err)
	}
	if len(f.storedReplication(t, "photos")) == 0 {
		t.Error("a refused removal removed the configuration")
	}
	f.meta.FailConfig = nil
	for range 2 {
		if err := f.replication.DeleteConfiguration(ctx, f.root, "photos"); err != nil {
			t.Fatalf("DeleteConfiguration: %v", err)
		}
		if got, err := f.replication.Configuration(ctx, f.root, "photos"); err != nil || !got.Empty() {
			t.Errorf("Configuration after the removal = %+v, %v", got, err)
		}
	}
	if err := f.replication.DeleteConfiguration(ctx, f.root, "missing"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("DeleteConfiguration of a bucket that is not there = %v", err)
	}
	removals := 0
	for _, r := range f.trail(t) {
		if r.Action == string(iam.ActionPutReplication) && r.Outcome == audit.Succeeded && r.Details["replication"] == "removed" {
			removals++
		}
	}
	if removals != 2 {
		t.Errorf("%d removals in the trail, want 2", removals)
	}
	// Once removed, the bucket may be made a replica.
	if _, err := f.replication.SetReplicaMode(ctx, f.root, "photos", "site-z"); err != nil {
		t.Errorf("SetReplicaMode after the removal = %v", err)
	}
}

// TestReplicationConfigurationRead: a bucket without a configuration has
// none; one of a later release is unavailable, one that cannot be read an
// integrity failure; a bucket that is not there is not found.
func TestReplicationConfigurationRead(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		document []byte
		bucket   string
		want     error
	}{
		"none":                {bucket: "photos"},
		"of a later release":  {bucket: "photos", document: []byte(`{"v":2,"target":"site-b"}`), want: domain.ErrUnavailable},
		"that cannot be read": {bucket: "photos", document: []byte(`{"v":1,"target":"site-b"`), want: domain.ErrIntegrity},
		"of no bucket":        {bucket: "missing", want: domain.ErrBucketNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader := app.NewReplicationService(app.ReplicationServiceConfig{
				Buckets: documentBuckets{document: tt.document}, Authz: allowAll{},
			})
			got, err := reader.Configuration(context.Background(), iam.Principal{Root: true}, tt.bucket)
			switch {
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Errorf("Configuration = %v, want %v", err, tt.want)
			case tt.want == nil && (err != nil || !got.Empty()):
				t.Errorf("Configuration = %+v, %v; want none", got, err)
			}
		})
	}
}

// documentBuckets is a store of one bucket, photos, whose replication
// configuration is the document given.
type documentBuckets struct {
	ports.BucketStore
	document []byte
}

func (d documentBuckets) GetBucketConfig(_ context.Context, name, _ string) ([]byte, error) {
	if name != "photos" {
		return nil, domain.ErrBucketNotFound
	}
	return d.document, nil
}

// allowAll is an authorizer that allows everything.
type allowAll struct{ ports.Authorizer }

func (allowAll) Authorize(context.Context, iam.Request) error { return nil }

// TestReplicationConfigurationWithoutWhatItNeeds: without an audit trail
// or the targets, nothing is changed and the caller is told to try later.
func TestReplicationConfigurationWithoutWhatItNeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplConfigFixture(t)
	for name, cfg := range map[string]app.ReplicationServiceConfig{
		"no audit trail": {Buckets: f.meta, Store: f.meta, Targets: f.meta, Authz: f.authz},
		"no targets":     {Buckets: f.meta, Store: f.meta, Authz: f.authz, Auditor: f.auditor},
	} {
		blind := app.NewReplicationService(cfg)
		if _, err := blind.SetConfiguration(ctx, f.root, "photos", replConfig("site-b", replRule("r", 0, ""))); !errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("%s: SetConfiguration = %v, want ErrUnavailable", name, err)
		}
		if len(f.storedReplication(t, "photos")) != 0 {
			t.Errorf("%s: a configuration was stored", name)
		}
	}
	blind := app.NewReplicationService(app.ReplicationServiceConfig{Buckets: f.meta, Store: f.meta, Targets: f.meta, Authz: f.authz})
	if err := blind.DeleteConfiguration(ctx, f.root, "photos"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("DeleteConfiguration without an audit trail = %v, want ErrUnavailable", err)
	}
}

// TestReplicationTargetLookupFails: a target store that fails is passed
// on as it failed, not taken for a target that is not there.
func TestReplicationTargetLookupFails(t *testing.T) {
	t.Parallel()
	f := newReplConfigFixture(t)
	failing := app.NewReplicationService(app.ReplicationServiceConfig{
		Buckets: f.meta, Store: f.meta, Targets: failingTargets{f.meta}, Authz: f.authz, Auditor: f.auditor,
	})
	_, err := failing.SetConfiguration(context.Background(), f.root, "photos", replConfig("site-b", replRule("r", 0, "")))
	if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetConfiguration with a target store that fails = %v", err)
	}
}
