package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// countingIAM is an IAM store that counts the changes it is asked for, and
// that fails the resolution of the users and keys named in broken with the
// error given there.
type countingIAM struct {
	ports.IAMStore
	writes atomic.Int64
	broken map[string]error
	// resolved, when set, is called as a user is resolved: a test's hook
	// into the middle of an explanation.
	resolved func()
}

func (c *countingIAM) ResolveUser(ctx context.Context, name string) (iam.Identity, error) {
	if c.resolved != nil {
		c.resolved()
	}
	if err := c.broken[name]; err != nil {
		return iam.Identity{}, err
	}
	return c.IAMStore.ResolveUser(ctx, name)
}

func (c *countingIAM) ResolveIdentity(ctx context.Context, accessKeyID string) (iam.Identity, error) {
	if err := c.broken[accessKeyID]; err != nil {
		return iam.Identity{}, err
	}
	return c.IAMStore.ResolveIdentity(ctx, accessKeyID)
}

// The marker the store's errors carry, as a stored policy that does not
// parse quotes its document, and the subjects whose resolution fails so.
const (
	storeMarker      = "DOC-TEXT-SECRET"
	damagedUser      = "zed"
	damagedKey       = "AKIADAMAGEDKEY000000"
	unavailableUser  = "yan"
	unavailableKey   = "AKIAUNAVAILABLE00000"
	keyMarkerMessage = "stored inline policy of key " + damagedKey + ": invalid argument: policy: KEY-DOC-SECRET"
)

// brokenSubjects are the failures of resolution the fixture's store gives.
func brokenSubjects() map[string]error {
	return map[string]error{
		damagedUser:     fmt.Errorf("%w: stored policy team: invalid argument: policy: %s", domain.ErrIntegrity, storeMarker),
		damagedKey:      fmt.Errorf("%w: %s %s", domain.ErrIntegrity, keyMarkerMessage, storeMarker),
		unavailableUser: fmt.Errorf("%w: raft: no leader, %s", domain.ErrUnavailable, storeMarker),
		unavailableKey:  fmt.Errorf("bbolt: %s", storeMarker),
	}
}

func (c *countingIAM) PutUser(ctx context.Context, u iam.User, create bool, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.PutUser(ctx, u, create, record)
}

func (c *countingIAM) DeleteUser(ctx context.Context, name string, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.DeleteUser(ctx, name, record)
}

func (c *countingIAM) PutGroup(ctx context.Context, g iam.Group, create bool, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.PutGroup(ctx, g, create, record)
}

func (c *countingIAM) DeleteGroup(ctx context.Context, name string, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.DeleteGroup(ctx, name, record)
}

func (c *countingIAM) PutRole(ctx context.Context, r iam.Role, create bool, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.PutRole(ctx, r, create, record)
}

func (c *countingIAM) DeleteRole(ctx context.Context, name string, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.DeleteRole(ctx, name, record)
}

func (c *countingIAM) PutPolicy(ctx context.Context, p iam.Policy, create bool, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.PutPolicy(ctx, p, create, record)
}

func (c *countingIAM) DeletePolicy(ctx context.Context, name string, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.DeletePolicy(ctx, name, record)
}

func (c *countingIAM) CreateAPIKey(ctx context.Context, k iam.APIKey, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.CreateAPIKey(ctx, k, record)
}

func (c *countingIAM) DeleteAPIKey(ctx context.Context, accessKeyID string, record audit.Record) error {
	c.writes.Add(1)
	return c.IAMStore.DeleteAPIKey(ctx, accessKeyID, record)
}

func (c *countingIAM) TouchAPIKey(ctx context.Context, accessKeyID string, usedAt time.Time) error {
	c.writes.Add(1)
	return c.IAMStore.TouchAPIKey(ctx, accessKeyID, usedAt)
}

// countingTrail is an audit trail that counts what is appended to it. Like
// the replicated store, it takes nothing on a context that has ended.
type countingTrail struct {
	ports.AuditLog
	appended atomic.Int64
}

func (c *countingTrail) AppendAudit(ctx context.Context, record audit.Record) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	}
	c.appended.Add(1)
	return c.AuditLog.AppendAudit(ctx, record)
}

// explainFixture is the explanation use case over the IAM use cases and a
// bucket with a policy and sub-buckets (subBucketSite).
type explainFixture struct {
	*iamFixture
	identities *countingIAM
	appended   *countingTrail
	explain    *app.ExplainService
	// Callers: ops holds cluster-admin; eve may explain and nothing else;
	// fay may also read the policy of the bucket site, gus also its
	// sub-buckets' policies; rita is read-only; hal may explain on user
	// alone - a key that is not there - and on no user.
	ops, eve, fay, gus, rita, hal iam.Principal
	// Subjects: bob and carol are users; bobsBound is a key of bob's
	// with an inline policy, bobsExpiring one that expires.
	bobsBound, bobsExpiring app.Credentials
}

// policyEverything allows carol everything of S3, with a Sid no record may
// show.
const policyEverything = `{"Version":"2012-10-17","Statement":[{"Sid":"SidSecretMarker","Effect":"Allow","Action":"s3:*","Resource":"*"}]}`

func newExplainFixture(t *testing.T) *explainFixture {
	t.Helper()
	ctx := t.Context()
	f := &explainFixture{iamFixture: newIAMFixture(t)}
	site := subBucketSite(t)
	f.authz.WithBucketPolicies(site).WithSubBuckets(site)
	f.identities = &countingIAM{IAMStore: f.store, broken: brokenSubjects()}
	f.appended = &countingTrail{AuditLog: f.store}
	f.explain = app.NewExplainService(app.ExplainServiceConfig{
		Store: f.identities, Trail: f.appended, Authz: f.authz, Explainer: app.NewExplainer(f.authz),
		Auditor: f.auditor, Clock: f.clock, Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	policies := map[string]string{
		"explain-only":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:ExplainAccess","Resource":["arn:cactusfs:admin:::user/*","arn:cactusfs:admin:::user","arn:cactusfs:admin:::anonymous"]}]}`,
		"read-site-policy": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetBucketPolicy","Resource":"arn:aws:s3:::site"}]}`,
		"read-site-subs":   `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetSubBucketPolicy","Resource":"arn:aws:s3:::site"}]}`,
		"everything":       policyEverything,
		"explain-nobody":   `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:ExplainAccess","Resource":"arn:cactusfs:admin:::user"}]}`,
	}
	for name, document := range policies {
		must(f.iam.PutPolicy(ctx, f.root, name, []byte(document), true))
	}
	for role, names := range map[string][]string{
		"explainer": {"explain-only"}, "explainer-bucket": {"explain-only", "read-site-policy"},
		"explainer-full": {"explain-only", "read-site-policy", "read-site-subs"}, "all": {"everything"},
		"explainer-nobody": {"explain-nobody"},
	} {
		must(f.iam.PutRole(ctx, f.root, iam.Role{Name: role, Policies: names}, true))
	}
	f.ops = f.as(t, f.user(t, "ops", iam.RoleClusterAdmin))
	f.eve = f.as(t, f.user(t, "eve", "explainer"))
	f.fay = f.as(t, f.user(t, "fay", "explainer-bucket"))
	f.gus = f.as(t, f.user(t, "gus", "explainer-full"))
	f.rita = f.as(t, f.user(t, "rita", iam.RoleReadOnly))
	f.hal = f.as(t, f.user(t, "hal", "explainer-nobody"))
	f.user(t, "bob")
	f.user(t, "carol", "all")
	_, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "dan", Roles: []string{"all"}, Disabled: true})
	must(err)
	f.bobsBound, _, err = f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{
		Owner: "bob", InlinePolicy: []byte(`{"Version":"2012-10-17","Statement":[{"Sid":"KeySid","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`),
	})
	must(err)
	f.bobsExpiring, _, err = f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{Owner: "bob", ExpiresAt: testNow.Add(time.Hour)})
	must(err)
	f.auditor.Flush(ctx)
	return f
}

// recordsOf returns the audit trail, with what was queued written first,
// verified against its hash chain.
func recordsOf(ctx context.Context, t *testing.T, f *iamFixture) []audit.Record {
	t.Helper()
	f.auditor.Flush(ctx)
	records, err := f.store.ListAudit(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(audit.Record{}, records); err != nil {
		t.Fatalf("the audit trail does not verify: %v", err)
	}
	return records
}

// explanationRecords are the records of answers among records: those of
// admin:ExplainAccess that are no refusal.
func explanationRecords(records []audit.Record) []audit.Record {
	var answers []audit.Record
	for _, record := range records {
		if record.Action == string(iam.ActionExplainAccess) && record.Outcome != audit.Denied {
			answers = append(answers, record)
		}
	}
	return answers
}

// denials are the refusals among records.
func denials(records []audit.Record) []audit.Record {
	var refused []audit.Record
	for _, record := range records {
		if record.Outcome == audit.Denied {
			refused = append(refused, record)
		}
	}
	return refused
}

// recordDetails are the only details the record of an answer may hold:
// never a document, a statement or a Sid.
var recordDetails = []string{
	"subject", "access_key_id", "action", "resource", "list_prefix", "answer", "allowed", "source", "rule", "not_authenticated",
}

func objectAt(bucketName, key string) app.ExplainRequest {
	return app.ExplainRequest{Bucket: bucketName, Key: key}
}

// TestExplain: the answers of the explanation use case, each recorded once,
// before it is given, and none that the caller may not be told.
func TestExplain(t *testing.T) {
	t.Parallel()
	f := newExplainFixture(t)
	ctx := t.Context()
	about := func(subject app.ExplainRequest, action iam.Action, r app.ExplainRequest) app.ExplainRequest {
		subject.Action, subject.Bucket, subject.Key, subject.AdminResource, subject.ListPrefix = action, r.Bucket, r.Key, r.AdminResource, r.ListPrefix
		subject.SourceIP, subject.SecureTransport = r.SourceIP, r.SecureTransport
		return subject
	}
	bob, carol := app.ExplainRequest{User: "bob"}, app.ExplainRequest{User: "carol"}
	anyone := app.ExplainRequest{Anonymous: true}
	key := func(creds app.Credentials) app.ExplainRequest {
		return app.ExplainRequest{AccessKeyID: creds.AccessKeyID}
	}
	insecure := false

	tests := []struct {
		name   string
		caller iam.Principal
		ask    app.ExplainRequest
		// want is the error, or nil for an answer.
		want error
		// answer is what is checked of an answer.
		check func(t *testing.T, got app.AccessExplanation)
		// recorded is the answer the record says; "" for a request that
		// is refused, which leaves a denial and no record of an answer.
		recorded string
		// deniedOn is the resource a refusal is audited on.
		deniedOn string
	}{
		{
			name: "refused without admin:ExplainAccess", caller: f.rita, ask: about(bob, iam.ActionGetObject, objectAt("site", "index.html")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "refused about a key, said nothing of it", caller: f.rita, ask: about(key(f.bobsBound), iam.ActionGetObject, objectAt("site", "index.html")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "refused about a key that is not there, as about one of nobody", caller: f.rita, ask: about(app.ExplainRequest{AccessKeyID: "AKIANOSUCHKEY0000000"}, iam.ActionGetObject, objectAt("site", "index.html")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user",
		},
		{
			name: "probing: an s3: action on a bucket without s3:GetBucketPolicy", caller: f.eve, ask: about(bob, iam.ActionGetObject, objectAt("site", "index.html")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "probing: a key in a sub-bucket without s3:GetSubBucketPolicy", caller: f.fay, ask: about(bob, iam.ActionPutObject, objectAt("site", "reports/q1.csv")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "probing: a key that could be in a sub-bucket that is not registered", caller: f.fay, ask: about(bob, iam.ActionPutObject, objectAt("site", "nothing-here/q1.csv")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "probing: a listing prefix in a sub-bucket", caller: f.fay, ask: about(bob, iam.ActionListBucket, app.ExplainRequest{Bucket: "site", ListPrefix: "reports/"}),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "a key in no sub-bucket needs only the bucket's policy", caller: f.fay, ask: about(anyone, iam.ActionGetObject, objectAt("site", "index.html")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if !got.Allowed || got.Source != iam.SourceBucket || len(got.Statements) != 1 || !got.Statements[0].Readable {
					t.Errorf("anonymous on a public bucket: %+v", got)
				}
			},
		},
		{
			name: "a damaged policy is answered as a refusal", caller: f.ops, ask: about(bob, iam.ActionPutObject, objectAt("site", "broken/q1.csv")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "anonymous on a public bucket", caller: f.ops, ask: about(anyone, iam.ActionGetObject, objectAt("site", "index.html")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				want := app.Citation{Source: iam.SourceBucket, Effect: iam.Allow, Readable: true}
				if !got.Allowed || got.Source != iam.SourceBucket || !slices.Equal(got.Statements, []app.Citation{want}) || got.Subject.Kind != app.SubjectAnonymous {
					t.Errorf("anonymous on a public bucket: %+v", got)
				}
				if got.Resource != "arn:aws:s3:::site/index.html" || !got.Assumed.SecureTransport || got.Assumed.SourceIP.IsValid() || !got.Assumed.Time.Equal(testNow) {
					t.Errorf("what was assumed: %+v, resource %s", got.Assumed, got.Resource)
				}
			},
		},
		{
			name: "anonymous on a private sub-bucket of a public bucket", caller: f.ops, ask: about(anyone, iam.ActionGetObject, objectAt("site", "private/salaries.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Allowed || got.Source != iam.SourceSubBucket {
					t.Errorf("anonymous on a private sub-bucket: %+v", got)
				}
			},
		},
		{
			name: "a sub-bucket's policy", caller: f.gus, ask: about(bob, iam.ActionPutObject, objectAt("site", "reports/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				want := []app.Citation{{Source: iam.SourceSubBucket, Effect: iam.Allow, Readable: true}}
				if !got.Allowed || got.Source != iam.SourceSubBucket || !slices.Equal(got.Statements, want) || got.KeyPolicy != nil {
					t.Errorf("bob in reports: %+v", got)
				}
			},
		},
		{
			name: "a listing in a sub-bucket", caller: f.gus, ask: about(bob, iam.ActionListBucket, app.ExplainRequest{Bucket: "site", ListPrefix: "reports/"}),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if !got.Allowed || got.Source != iam.SourceSubBucket || got.ListPrefix != "reports/" {
					t.Errorf("bob lists reports/: %+v", got)
				}
			},
		},
		{
			name: "a policy the caller may not read is not named", caller: f.gus, ask: about(carol, iam.ActionDeleteObject, objectAt("site", "open/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				want := []app.Citation{{Source: iam.SourceIdentity, Effect: iam.Allow}}
				if !got.Allowed || got.Source != iam.SourceIdentity || !slices.Equal(got.Statements, want) {
					t.Errorf("carol, seen by gus: %+v", got)
				}
			},
		},
		{
			name: "a policy the caller may read is named", caller: f.ops, ask: about(carol, iam.ActionDeleteObject, objectAt("site", "open/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				want := []app.Citation{{Source: iam.SourceIdentity, Effect: iam.Allow, Readable: true, Policy: "everything", Sid: "SidSecretMarker"}}
				if !got.Allowed || !slices.Equal(got.Statements, want) {
					t.Errorf("carol, seen by ops: %+v", got)
				}
			},
		},
		{
			name: "the sub-bucket's deny over the caller's allow, the deny named", caller: f.gus, ask: about(carol, iam.ActionDeleteObject, objectAt("site", "reports/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Allowed || got.Source != iam.SourceSubBucket {
					t.Errorf("carol in reports: %+v", got)
				}
				for _, c := range got.Statements {
					if (c.Source == iam.SourceSubBucket) != c.Readable || c.Sid != "" && !c.Readable {
						t.Errorf("a citation shown as it may not be: %+v", c)
					}
				}
			},
		},
		{
			name: "a key bound by its inline policy", caller: f.gus, ask: about(key(f.bobsBound), iam.ActionPutObject, objectAt("site", "reports/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Allowed || got.Source != iam.SourceKeyBound || got.KeyPolicy == nil || got.KeyPolicy.Allowed || got.Subject.User != "bob" {
					t.Errorf("bob's bound key, beyond its bound: %+v", got)
				}
			},
		},
		{
			name: "a key's inline policy, hidden without admin:ListAPIKeys", caller: f.gus, ask: about(key(f.bobsBound), iam.ActionGetObject, objectAt("site", "reports/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				want := []app.Citation{{Source: iam.SourceKeyBound, Effect: iam.Allow}}
				if !got.Allowed || got.KeyPolicy == nil || !got.KeyPolicy.Allowed || !slices.Equal(got.KeyPolicy.Statements, want) {
					t.Errorf("bob's bound key, seen by gus: %+v", got)
				}
			},
		},
		{
			name: "a key's inline policy, shown with admin:ListAPIKeys", caller: f.ops, ask: about(key(f.bobsBound), iam.ActionGetObject, objectAt("site", "reports/q1.csv")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				want := []app.Citation{{Source: iam.SourceKeyBound, Effect: iam.Allow, Readable: true, Sid: "KeySid"}}
				if got.KeyPolicy == nil || !slices.Equal(got.KeyPolicy.Statements, want) {
					t.Errorf("bob's bound key, seen by ops: %+v", got)
				}
			},
		},
		{
			name: "an admin action needs no policy of a bucket", caller: f.eve, ask: about(carol, iam.ActionListUsers, app.ExplainRequest{AdminResource: "user"}),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Allowed || got.Source != iam.SourceNone || got.Resource != "arn:cactusfs:admin:::user" || len(got.NotConsidered) != 0 {
					t.Errorf("carol lists users: %+v", got)
				}
			},
		},
		{
			name: "ownership is not considered for a preview", caller: f.ops, ask: about(carol, iam.ActionPreviewObject, objectAt("other", "a.png")),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if !slices.Equal(got.NotConsidered, []string{app.NotConsideredObjectOwnership}) || got.Source == iam.SourceOwner {
					t.Errorf("a preview: %+v", got)
				}
			},
		},
		{
			name: "what the caller names is assumed", caller: f.ops, ask: about(bob, iam.ActionGetObject, app.ExplainRequest{Bucket: "other", Key: "x", SourceIP: "::ffff:10.1.2.3", SecureTransport: &insecure}),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Assumed.SourceIP != netip.MustParseAddr("10.1.2.3") || got.Assumed.SecureTransport {
					t.Errorf("assumed: %+v", got.Assumed)
				}
			},
		},
		{
			name: "a disabled user would not authenticate", caller: f.ops, ask: about(app.ExplainRequest{User: "dan"}, iam.ActionGetObject, objectAt("other", "x")),
			recorded: "not_authenticated",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Authenticates || got.NotAuthenticated != app.NotAuthenticatedUserDisabled || got.Allowed || got.Source != "" || got.Statements != nil {
					t.Errorf("dan: %+v", got)
				}
			},
		},
		{
			name: "a user who is not there, after authorization", caller: f.ops, ask: about(app.ExplainRequest{User: "nobody"}, iam.ActionGetObject, objectAt("other", "x")),
			want: domain.ErrEntityNotFound, recorded: "no_such_entity",
		},
		{
			name: "a key that is not there, after authorization", caller: f.ops, ask: about(app.ExplainRequest{AccessKeyID: "AKIANOSUCHKEY0000000"}, iam.ActionGetObject, objectAt("other", "x")),
			want: domain.ErrEntityNotFound, recorded: "no_such_entity",
		},
		{
			name: "a user whose policy cannot be read is refused, as any refusal", caller: f.ops, ask: about(app.ExplainRequest{User: damagedUser}, iam.ActionGetObject, objectAt("other", "x")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/" + damagedUser,
		},
		{
			name: "a key whose policy cannot be read is refused, as any refusal", caller: f.ops, ask: about(app.ExplainRequest{AccessKeyID: damagedKey}, iam.ActionGetObject, objectAt("other", "x")),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user",
		},
		{
			name: "a key whose policy cannot be read is not asked about as a key of nobody", caller: f.hal, ask: about(app.ExplainRequest{AccessKeyID: damagedKey}, iam.ActionListUsers, app.ExplainRequest{AdminResource: "user"}),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user",
		},
		{
			name: "a key that is not there, to a caller who may ask about one", caller: f.hal, ask: about(app.ExplainRequest{AccessKeyID: "AKIANOSUCHKEY0000000"}, iam.ActionListUsers, app.ExplainRequest{AdminResource: "user"}),
			want: domain.ErrEntityNotFound, recorded: "no_such_entity",
		},
		{
			name: "a key of a user, to a caller who may ask about no user", caller: f.hal, ask: about(key(f.bobsBound), iam.ActionListUsers, app.ExplainRequest{AdminResource: "user"}),
			want: domain.ErrAccessDenied, deniedOn: "arn:cactusfs:admin:::user/bob",
		},
		{
			name: "a store that cannot resolve a user now", caller: f.ops, ask: about(app.ExplainRequest{User: unavailableUser}, iam.ActionGetObject, objectAt("other", "x")),
			want: domain.ErrUnavailable,
		},
		{
			name: "a store that cannot resolve a key now", caller: f.ops, ask: about(app.ExplainRequest{AccessKeyID: unavailableKey}, iam.ActionGetObject, objectAt("other", "x")),
			want: domain.ErrUnavailable,
		},
		{
			name: "an action asked on a bucket, without one", caller: f.ops, ask: about(bob, iam.ActionPutBucketPolicy, app.ExplainRequest{}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a bucket that is a wildcard", caller: f.ops, ask: about(bob, iam.ActionGetObject, objectAt("*", "x")),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a user that is a wildcard", caller: f.ops, ask: about(bob, iam.ActionGetUser, app.ExplainRequest{AdminResource: "user/*"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a user's name no user has", caller: f.ops, ask: about(bob, iam.ActionGetUser, app.ExplainRequest{AdminResource: "user/a b"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a target's name no target has", caller: f.ops, ask: about(bob, iam.ActionGetTarget, app.ExplainRequest{AdminResource: "target/Hook"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a kind that has no names", caller: f.ops, ask: about(bob, iam.ActionGetAuditLog, app.ExplainRequest{AdminResource: "audit/x"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a tag key with a slash", caller: f.ops, ask: about(carol, iam.ActionGetTagDefinition, app.ExplainRequest{AdminResource: "tag-definition/team/x"}),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Allowed || got.Resource != "arn:cactusfs:admin:::tag-definition/team/x" {
					t.Errorf("carol reads the definition of team/x: %+v", got)
				}
			},
		},
		{
			name: "a tag key with a wildcard", caller: f.ops, ask: about(carol, iam.ActionGetTagDefinition, app.ExplainRequest{AdminResource: "tag-definition/team/*"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a named administrative resource", caller: f.ops, ask: about(carol, iam.ActionGetTagDefinition, app.ExplainRequest{AdminResource: "tag-definition/team"}),
			recorded: "decision",
			check: func(t *testing.T, got app.AccessExplanation) {
				if got.Allowed || got.Resource != "arn:cactusfs:admin:::tag-definition/team" {
					t.Errorf("carol reads a tag definition: %+v", got)
				}
			},
		},
		{
			name: "an action with a wildcard", caller: f.ops, ask: about(bob, "s3:Get*", objectAt("other", "x")),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "an action that is not in the catalogue", caller: f.ops, ask: about(bob, "cluster:NodeRPC", app.ExplainRequest{}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a resource of no template of the action", caller: f.ops, ask: about(bob, iam.ActionGetObject, app.ExplainRequest{Bucket: "other"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a listing prefix of an action that lists nothing", caller: f.ops, ask: about(bob, iam.ActionGetObject, app.ExplainRequest{Bucket: "other", Key: "x", ListPrefix: "a/"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "a source address that is none", caller: f.ops, ask: about(bob, iam.ActionGetObject, app.ExplainRequest{Bucket: "other", Key: "x", SourceIP: "fe80::1%eth0"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
		{
			name: "both an administrative resource and a bucket", caller: f.ops, ask: about(bob, iam.ActionGetUser, app.ExplainRequest{Bucket: "other", AdminResource: "user/bob"}),
			want: domain.ErrInvalidArgument, recorded: "invalid_argument",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, writes, appended, logged := len(recordsOf(ctx, t, f.iamFixture)), f.identities.writes.Load(), f.appended.appended.Load(), len(f.logs.String())
			got, err := f.explain.Explain(ctx, tt.caller, tt.ask)
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("Explain = %+v, %v; want %v", got, err, tt.want)
			}
			if err == nil && tt.check != nil {
				tt.check(t, got)
			}
			if err != nil && !errors.Is(err, domain.ErrAccessDenied) && got.Subject.Kind != "" {
				t.Errorf("an error with an answer: %+v", got)
			}
			if f.identities.writes.Load() != writes {
				t.Error("the explanation wrote to the IAM store")
			}
			records := recordsOf(ctx, t, f.iamFixture)[before:]
			answers, refused := explanationRecords(records), denials(records)
			switch {
			case errors.Is(tt.want, domain.ErrUnavailable):
				if len(records) != 0 || f.appended.appended.Load() != appended {
					t.Fatalf("an explanation that could not be made left %+v", records)
				}
			case tt.recorded == "":
				if len(answers) != 0 || len(refused) != 1 || refused[0].Action != string(iam.ActionExplainAccess) || refused[0].Resource != tt.deniedOn {
					t.Fatalf("a refusal left the records %+v", records)
				}
				if f.appended.appended.Load() != appended {
					t.Error("a refusal was appended synchronously, as an answer is")
				}
			default:
				if len(answers) != 1 || len(refused) != 0 || answers[0].Details["answer"] != tt.recorded || f.appended.appended.Load() != appended+1 {
					t.Fatalf("an answer left the records %+v, %d appended", records, f.appended.appended.Load()-appended)
				}
				record := answers[0]
				wantAction := string(tt.ask.Action)
				if _, ok := iam.LookupAction(tt.ask.Action); !ok {
					wantAction = ""
				}
				if record.Actor != actorName(tt.caller) || record.Details["action"] != wantAction {
					t.Errorf("the record: %+v", record)
				}
				if tt.recorded == "decision" && (record.Details["allowed"] != fmt.Sprint(got.Allowed) || record.Details["source"] != string(got.Source) || record.Details["resource"] != got.Resource) {
					t.Errorf("the record of a decision: %+v, the answer %+v", record.Details, got)
				}
				for name, value := range record.Details {
					if !slices.Contains(recordDetails, name) || strings.Contains(value, "Sid") || strings.Contains(value, "Statement") || strings.Contains(value, "{") {
						t.Errorf("the record holds %s=%q", name, value)
					}
				}
			}
			for _, record := range records {
				if strings.Contains(fmt.Sprint(record), "SidSecretMarker") || strings.Contains(fmt.Sprint(record), "elsewhere-sentinel") {
					t.Errorf("a record holds a statement: %+v", record)
				}
			}
			for _, record := range records {
				for _, marker := range []string{storeMarker, "KEY-DOC-SECRET", damagedKey} {
					if strings.Contains(fmt.Sprint(record.Details), marker) {
						t.Errorf("a record holds %q: %+v", marker, record)
					}
				}
			}
			if out := f.logs.String()[logged:]; strings.Contains(out, "elsewhere-sentinel") || strings.Contains(out, "SidSecretMarker") ||
				strings.Contains(out, storeMarker) || strings.Contains(out, "KEY-DOC-SECRET") || strings.Contains(out, damagedKey) {
				t.Errorf("the log holds a statement: %s", out)
			}
			if err != nil {
				for _, text := range []string{"bob", "carol", "arn:", string(tt.ask.Action), "elsewhere-sentinel", storeMarker, "KEY-DOC-SECRET", damagedKey, damagedUser, unavailableKey} {
					if text != "" && strings.Contains(err.Error(), text) {
						t.Errorf("the error %q names %q", err, text)
					}
				}
			}
		})
	}
}

// TestExplainDamagedPolicy: a policy that cannot be read is logged with the
// names of the bucket and the sub-bucket, and nothing of what it says.
func TestExplainDamagedPolicy(t *testing.T) {
	t.Parallel()
	f := newExplainFixture(t)
	logged := len(f.logs.String())
	_, err := f.explain.Explain(t.Context(), f.ops, app.ExplainRequest{User: "bob", Action: iam.ActionGetObject, Bucket: "site", Key: "broken/q1.csv"})
	if !errors.Is(err, domain.ErrAccessDenied) || errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("Explain = %v, want a refusal that is not told as damage", err)
	}
	out := f.logs.String()[logged:]
	if !strings.Contains(out, "a policy an explanation depends on cannot be read") || !strings.Contains(out, `"sub_bucket":"broken"`) || !strings.Contains(out, `"bucket":"site"`) {
		t.Errorf("the log: %s", out)
	}
	if strings.Contains(out, "elsewhere-sentinel") || strings.Contains(out, "does not parse") {
		t.Errorf("the log holds what the policy or the parser says: %s", out)
	}
	// A root caller is not bound by the bucket's policies, so the damage
	// is met by the explainer: answered the same way.
	_, err = f.explain.Explain(t.Context(), f.root, app.ExplainRequest{Anonymous: true, Action: iam.ActionGetObject, Bucket: "site", Key: "broken/q1.csv"})
	if !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Explain by root = %v, want a refusal", err)
	}
}

// TestExplainExpiredKey: a key whose expiry has passed would not
// authenticate, and neither would a key of a disabled owner.
func TestExplainExpiredKey(t *testing.T) {
	t.Parallel()
	f := newExplainFixture(t)
	ctx := t.Context()
	ask := app.ExplainRequest{AccessKeyID: f.bobsExpiring.AccessKeyID, Action: iam.ActionGetObject, Bucket: "other", Key: "x"}
	got, err := f.explain.Explain(ctx, f.ops, ask)
	if err != nil || !got.Authenticates {
		t.Fatalf("before it expires: %+v, %v", got, err)
	}
	f.clock.Advance(2 * time.Hour)
	got, err = f.explain.Explain(ctx, f.ops, ask)
	if err != nil || got.Authenticates || got.NotAuthenticated != app.NotAuthenticatedKeyExpired {
		t.Fatalf("after it expired: %+v, %v", got, err)
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "bob", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	got, err = f.explain.Explain(ctx, f.ops, app.ExplainRequest{AccessKeyID: f.bobsBound.AccessKeyID, Action: iam.ActionGetObject, Bucket: "other", Key: "x"})
	if err != nil || got.Authenticates || got.NotAuthenticated != app.NotAuthenticatedUserDisabled {
		t.Fatalf("a key of a disabled owner: %+v, %v", got, err)
	}
}

// TestExplainNeedsItsRecord: an answer whose record cannot be appended is
// not given - unavailable, or of an outcome not known.
func TestExplainNeedsItsRecord(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		fail error
		want error
	}{
		{"the trail cannot be written", errors.New("no leader"), domain.ErrUnavailable},
		{"the trail cannot say", fmt.Errorf("%w: lost the leader", domain.ErrOutcomeUnknown), domain.ErrOutcomeUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newExplainFixture(t)
			f.store.FailWrites = tt.fail
			for _, ask := range []app.ExplainRequest{
				{Anonymous: true, Action: iam.ActionGetObject, Bucket: "site", Key: "index.html"},
				{User: "nobody", Action: iam.ActionGetObject, Bucket: "site", Key: "index.html"},
				{User: "dan", Action: iam.ActionGetObject, Bucket: "site", Key: "index.html"},
				{User: "bob", Action: "s3:*"},
			} {
				got, err := f.explain.Explain(t.Context(), f.ops, ask)
				if !errors.Is(err, tt.want) || got.Subject.Kind != "" {
					t.Errorf("Explain(%+v) = %+v, %v; want %v and no answer", ask, got, err, tt.want)
				}
				if strings.Contains(err.Error(), "no leader") || strings.Contains(err.Error(), "lost the leader") {
					t.Errorf("the error tells the store's: %v", err)
				}
			}
		})
	}
}

// TestAnExplanationIsRecordedWhenItsCallerGoes: a caller that goes once its
// question is authorized - a closed connection ends the request's context -
// does not take the record of the question with it: the answer is not
// given, but the trail has it.
func TestAnExplanationIsRecordedWhenItsCallerGoes(t *testing.T) {
	t.Parallel()
	f := newExplainFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.identities.resolved = cancel
	before := f.appended.appended.Load()
	_, _ = f.explain.Explain(ctx, f.ops, app.ExplainRequest{User: "bob", Action: iam.ActionGetObject, Bucket: "site", Key: "index.html"})
	if got := f.appended.appended.Load() - before; got != 1 {
		t.Fatalf("%d records appended for a question whose caller went; want 1", got)
	}
}

// TestExplainBeforeAuthorization: a request that names no subject is
// refused before anything is asked or recorded; a locked session is told
// that it is locked.
func TestExplainBeforeAuthorization(t *testing.T) {
	t.Parallel()
	f := newExplainFixture(t)
	ctx := t.Context()
	locked := f.ops
	locked.LockedSession = true
	tests := []struct {
		name   string
		caller iam.Principal
		ask    app.ExplainRequest
		want   error
	}{
		{"no subject", f.ops, app.ExplainRequest{Action: iam.ActionGetObject}, domain.ErrInvalidArgument},
		{"two subjects", f.ops, app.ExplainRequest{User: "bob", Anonymous: true, Action: iam.ActionGetObject}, domain.ErrInvalidArgument},
		{"a name no user has", f.ops, app.ExplainRequest{User: strings.Repeat("x", 300), Action: iam.ActionGetObject}, domain.ErrInvalidArgument},
		{"an access key ID too long", f.ops, app.ExplainRequest{AccessKeyID: strings.Repeat("A", 129), Action: iam.ActionGetObject}, domain.ErrInvalidArgument},
		{"a locked session", locked, app.ExplainRequest{User: "bob", Action: iam.ActionGetObject, Bucket: "other", Key: "x"}, domain.ErrSessionLocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appended := f.appended.appended.Load()
			if _, err := f.explain.Explain(ctx, tt.caller, tt.ask); !errors.Is(err, tt.want) {
				t.Errorf("Explain = %v, want %v", err, tt.want)
			}
			if f.appended.appended.Load() != appended {
				t.Error("a request refused before authorization was recorded as an answer")
			}
		})
	}
}

// TestExplainAsksTheExplainer: the request handed to the explainer is the
// one asked about, at the node's time, with the subject as authentication
// would make it; an explainer that cannot decide gives no answer, and says
// nothing of why.
func TestExplainAsksTheExplainer(t *testing.T) {
	t.Parallel()
	f := newExplainFixture(t)
	ctx := t.Context()
	explainer := &porttest.Explainer{}
	service := app.NewExplainService(app.ExplainServiceConfig{
		Store: f.store, Trail: f.store, Authz: f.authz, Explainer: explainer, Auditor: f.auditor, Clock: f.clock,
		Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	if _, err := service.Explain(ctx, f.ops, app.ExplainRequest{AccessKeyID: f.bobsBound.AccessKeyID, Action: iam.ActionGetObject, Bucket: "other", Key: "x", SourceIP: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	requests := explainer.Requests()
	if len(requests) != 1 {
		t.Fatalf("the explainer was asked %d times", len(requests))
	}
	got := requests[0]
	if got.Principal.User != "bob" || got.Principal.AccessKeyID != f.bobsBound.AccessKeyID || got.Principal.Boundary == nil || got.Principal.LockedSession ||
		got.Principal.Session.SourceIP != netip.MustParseAddr("10.0.0.5") || !got.Principal.Session.SecureTransport ||
		got.Action != iam.ActionGetObject || got.Resource != (iam.Resource{Bucket: "other", Key: "x"}) || !got.Now.Equal(testNow) || got.ObjectCreator != "" {
		t.Errorf("the explainer was asked %+v", got)
	}
	explainer.Fail = fmt.Errorf("%w: the store is away: bob s3:GetObject", domain.ErrUnavailable)
	_, err := service.Explain(ctx, f.ops, app.ExplainRequest{User: "bob", Action: iam.ActionGetObject, Bucket: "other", Key: "x"})
	if !errors.Is(err, domain.ErrUnavailable) || strings.Contains(err.Error(), "store is away") || strings.Contains(err.Error(), "bob") {
		t.Errorf("an explainer that cannot decide: %v", err)
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := service.Explain(ended, f.ops, app.ExplainRequest{User: "bob", Action: iam.ActionGetObject, Bucket: "other", Key: "x"}); err == nil {
		t.Error("an explanation of a request that has ended was given")
	}
}
