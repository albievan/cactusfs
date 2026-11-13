package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// subBucketFixture is a bucket "site" (and "other") behind the authorizer as
// a node has it - reading the bucket's policy and its sub-buckets together -
// with the audit trail every change and every refusal is written to.
type subBucketFixture struct {
	*fixture
	trail   *porttest.MemIAM
	auditor *app.Auditor
	authz   *app.Authorizer
	subs    *app.SubBucketService
}

func newSubBucketFixture(t *testing.T) *subBucketFixture {
	t.Helper()
	f := newFixture(t)
	trail := porttest.NewMemIAM()
	f.meta.Audit = trail
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	auditor := app.NewAuditor(trail, f.clock, logger)
	authz := app.NewAuthorizer(logger, f.clock, auditor).WithSubBuckets(f.meta)
	for _, name := range []string{"site", "other"} {
		if err := f.meta.CreateBucket(t.Context(), bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
	}
	return &subBucketFixture{
		fixture: f, trail: trail, auditor: auditor, authz: authz,
		subs: app.NewSubBucketService(f.meta, authz, auditor, f.clock, logger),
	}
}

// records returns the audit trail, everything queued written first.
func (f *subBucketFixture) records(t *testing.T) []audit.Record {
	t.Helper()
	f.auditor.Flush(t.Context())
	records, err := f.trail.ListAudit(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// register puts sub-buckets straight into the store, without a record.
func (f *subBucketFixture) register(t *testing.T, bucketName string, policies map[string]string) {
	t.Helper()
	for name, policy := range policies {
		if err := f.meta.PutSubBucket(t.Context(), bucketName, bucket.SubBucket{Name: name, Policy: []byte(policy), CreatedAt: testNow}, audit.Record{}); err != nil {
			t.Fatal(err)
		}
	}
}

// Principals of the tests of sub-buckets.
var (
	subAdmin = iam.Principal{AccessKeyID: "A", User: "ann", Policies: []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}},
	}}}
	// subReader may list the bucket whole and do nothing else.
	subReader = iam.Principal{AccessKeyID: "R", User: "rita", Policies: []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:ListBucket"}, Resources: []string{"arn:aws:s3:::site"}}},
	}}}
	// subViewer may read sub-buckets and their policies, and change none.
	subViewer = iam.Principal{AccessKeyID: "V", User: "vic", Policies: []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:GetSubBucketPolicy", "s3:ListSubBuckets"}, Resources: []string{"arn:aws:s3:::site"}}},
	}}}
	// subBob has no policy of his own: what he may, a sub-bucket allows.
	subBob    = iam.Principal{AccessKeyID: "B", User: "bob"}
	subNobody = iam.Principal{AccessKeyID: "N", User: "nora"}
)

const (
	reportsPolicy = `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::site"},
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"}]}`
	privatePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/private/*"}]}`
)

// wantRecord finds the one record of an action and outcome on the bucket,
// and returns it.
func wantRecord(t *testing.T, records []audit.Record, action iam.Action, outcome audit.Outcome) audit.Record {
	t.Helper()
	var found []audit.Record
	for _, r := range records {
		if r.Action == string(action) && r.Outcome == outcome {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d records of %s %s among %+v, want 1", len(found), action, outcome, records)
	}
	return found[0]
}

// TestSubBucketServicePut: a sub-bucket is registered or its policy set by
// whoever holds s3:PutSubBucket on the bucket, and the change is audited in
// the entry that makes it. A policy that is not confined to the sub-bucket is
// refused with domain.ErrInvalidPolicy and nothing is changed; the eleventh
// sub-bucket is refused; a refusal of the authorizer is audited.
func TestSubBucketServicePut(t *testing.T) {
	t.Parallel()
	ten := func(t *testing.T, f *subBucketFixture) {
		policies := map[string]string{}
		for i := range bucket.MaxSubBuckets {
			policies[fmt.Sprintf("s%02d", i)] = ""
		}
		f.register(t, "site", policies)
	}
	tests := map[string]struct {
		setup     func(t *testing.T, f *subBucketFixture)
		principal iam.Principal
		bucket    string
		name      string
		policy    string
		want      error
		// notWant is an error the error must not wrap.
		notWant error
		// stored is the policy the sub-bucket has afterwards; absent says
		// it is not there.
		stored string
		absent bool
		// details are those of the record of the change; none when the
		// change is not made.
		details map[string]string
		denied  bool
	}{
		"with a policy": {
			principal: subAdmin, name: "reports", policy: reportsPolicy, stored: reportsPolicy,
			details: map[string]string{"sub_bucket": "reports", "policy": "set", "statements": "2"},
		},
		"without a policy": {
			principal: subAdmin, name: "reports", details: map[string]string{"sub_bucket": "reports", "policy": "none"},
		},
		"by root": {
			principal: porttest.Root(), name: "private", policy: privatePolicy, stored: privatePolicy,
			details: map[string]string{"sub_bucket": "private", "policy": "set", "statements": "1"},
		},
		"the policy is replaced": {
			setup: func(t *testing.T, f *subBucketFixture) {
				f.register(t, "site", map[string]string{"reports": privateFor("reports")})
			},
			principal: subAdmin, name: "reports", policy: reportsPolicy, stored: reportsPolicy,
			details: map[string]string{"sub_bucket": "reports", "policy": "set", "statements": "2"},
		},
		"the policy is removed": {
			setup: func(t *testing.T, f *subBucketFixture) {
				f.register(t, "site", map[string]string{"reports": reportsPolicy})
			},
			principal: subAdmin, name: "reports", details: map[string]string{"sub_bucket": "reports", "policy": "none"},
		},
		"a policy of another sub-bucket's keys": {
			principal: subAdmin, name: "reports", policy: privatePolicy, want: domain.ErrInvalidPolicy, absent: true,
		},
		"a policy of the whole bucket's keys": {
			principal: subAdmin, name: "reports", want: domain.ErrInvalidPolicy, absent: true,
			policy: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`,
		},
		"a policy that lets anybody in": {
			principal: subAdmin, name: "reports", want: domain.ErrInvalidPolicy, absent: true,
			policy: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"}]}`,
		},
		"a policy that bypasses governance": {
			principal: subAdmin, name: "reports", want: domain.ErrInvalidPolicy, absent: true,
			policy: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:BypassGovernanceRetention","Resource":"arn:aws:s3:::site/reports/*"}]}`,
		},
		"a policy that is not JSON": {
			principal: subAdmin, name: "reports", policy: "{not json", want: domain.ErrInvalidPolicy, absent: true,
		},
		"an invalid policy leaves the one it had": {
			setup: func(t *testing.T, f *subBucketFixture) {
				f.register(t, "site", map[string]string{"reports": reportsPolicy})
			},
			principal: subAdmin, name: "reports", policy: privatePolicy, want: domain.ErrInvalidPolicy, stored: reportsPolicy,
		},
		"a name a sub-bucket cannot have": {
			principal: subAdmin, name: "Reports/2026", want: domain.ErrInvalidArgument, notWant: domain.ErrInvalidPolicy, absent: true,
		},
		"a name a sub-bucket cannot have, with a policy": {
			principal: subAdmin, name: "-reports", policy: reportsPolicy, want: domain.ErrInvalidArgument, notWant: domain.ErrInvalidPolicy, absent: true,
		},
		"no such bucket": {principal: subAdmin, bucket: "absent", name: "reports", want: domain.ErrBucketNotFound},
		"the eleventh is refused": {
			setup: ten, principal: subAdmin, name: "reports", want: domain.ErrTooManySubBuckets, absent: true,
		},
		"the tenth's policy can still be set": {
			setup: ten, principal: subAdmin, name: "s09", policy: privateFor("s09"), stored: privateFor("s09"),
			details: map[string]string{"sub_bucket": "s09", "policy": "set", "statements": "1"},
		},
		"refused without s3:PutSubBucket": {
			principal: subViewer, name: "reports", policy: reportsPolicy, want: domain.ErrAccessDenied, absent: true, denied: true,
		},
		"a sub-bucket's policy cannot grant it": {
			setup: func(t *testing.T, f *subBucketFixture) {
				f.register(t, "site", map[string]string{"reports": reportsPolicy})
			},
			principal: subBob, name: "reports", want: domain.ErrAccessDenied, stored: reportsPolicy, denied: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSubBucketFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			bucketName := tt.bucket
			if bucketName == "" {
				bucketName = "site"
			}
			before := f.clock.Now()
			f.clock.Advance(time.Hour)
			got, err := f.subs.Put(ctx, tt.principal, bucketName, tt.name, []byte(tt.policy))
			if tt.want != nil {
				if !errors.Is(err, tt.want) || (tt.notWant != nil && errors.Is(err, tt.notWant)) {
					t.Fatalf("Put = %v, want %v (and not %v)", err, tt.want, tt.notWant)
				}
			} else {
				if err != nil {
					t.Fatalf("Put: %v", err)
				}
				if got.Name != tt.name || string(got.Policy) != tt.stored {
					t.Errorf("Put returned %+v", got)
				}
				// A sub-bucket that was there keeps its age.
				wantCreated := f.clock.Now()
				if tt.setup != nil {
					wantCreated = before
				}
				if !got.CreatedAt.Equal(wantCreated) {
					t.Errorf("created at %v, want %v", got.CreatedAt, wantCreated)
				}
			}
			stored, err := f.meta.GetSubBucket(ctx, "site", tt.name)
			switch {
			case tt.absent:
				if !errors.Is(err, domain.ErrSubBucketNotFound) {
					t.Errorf("after: %+v, %v; want none", stored, err)
				}
			case bucketName == "site" && (err != nil || string(stored.Policy) != tt.stored):
				t.Errorf("after: %+v, %v; want the policy %q", stored, err, tt.stored)
			}
			records := f.records(t)
			if tt.details != nil {
				r := wantRecord(t, records, iam.ActionPutSubBucket, audit.Succeeded)
				if r.Resource != "arn:aws:s3:::site" || r.Actor != actorName(tt.principal) || !maps.Equal(r.Details, tt.details) {
					t.Errorf("the record of the change = %+v, want the details %v", r, tt.details)
				}
				if strings.Contains(fmt.Sprint(r), "arn:aws:iam:::user/bob") {
					t.Errorf("the record holds the policy: %+v", r)
				}
			}
			if tt.denied {
				wantRecord(t, records, iam.ActionPutSubBucket, audit.Denied)
			}
			if tt.details == nil && !tt.denied && len(records) != 0 {
				t.Errorf("a change that was not made left %+v", records)
			}
		})
	}
}

// privateFor is a policy of the sub-bucket name that denies everybody its
// keys.
func privateFor(name string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/` + name + `/*"}]}`
}

// actorName is how the audit trail names the principals of these tests.
func actorName(p iam.Principal) string {
	if p.Root {
		return "root"
	}
	return "user/" + p.User
}

// TestSubBucketServiceDelete: only an empty sub-bucket is deleted - one with
// no object, no delete marker and no upload in progress under its name -
// and a key of a sub-bucket of a name that begins alike does not count. The
// deletion is audited, a refusal of the authorizer too.
func TestSubBucketServiceDelete(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup     func(t *testing.T, f *subBucketFixture)
		principal iam.Principal
		name      string
		want      error
		denied    bool
	}{
		"an empty one": {principal: subAdmin, name: "reports"},
		"by root":      {principal: porttest.Root(), name: "reports"},
		"keys beside it do not count": {
			setup: func(t *testing.T, f *subBucketFixture) {
				f.put(t, "site", "reports", []byte("a key named as the sub-bucket"))
				f.put(t, "site", "reports-x/a.csv", []byte("a key of another"))
				f.put(t, "other", "reports/a.csv", []byte("a key of another bucket"))
			},
			principal: subAdmin, name: "reports",
		},
		"an object under it": {
			setup:     func(t *testing.T, f *subBucketFixture) { f.put(t, "site", "reports/2026/q1.csv", []byte("figures")) },
			principal: subAdmin, name: "reports", want: domain.ErrSubBucketNotEmpty,
		},
		"a delete marker under it": {
			setup: func(t *testing.T, f *subBucketFixture) {
				if err := f.buckets.SetVersioning(t.Context(), porttest.Root(), "site", bucket.VersioningEnabled); err != nil {
					t.Fatal(err)
				}
				f.put(t, "site", "reports/q1.csv", []byte("figures"))
				if _, err := f.objects.Delete(t.Context(), porttest.Root(), app.DeleteInput{Bucket: "site", Key: "reports/q1.csv"}); err != nil {
					t.Fatal(err)
				}
			},
			principal: subAdmin, name: "reports", want: domain.ErrSubBucketNotEmpty,
		},
		"an upload in progress under it": {
			setup: func(t *testing.T, f *subBucketFixture) {
				if _, err := f.objects.CreateUpload(t.Context(), porttest.Root(), app.CreateUploadInput{Bucket: "site", Key: "reports/big.bin"}); err != nil {
					t.Fatal(err)
				}
			},
			principal: subAdmin, name: "reports", want: domain.ErrSubBucketNotEmpty,
		},
		"no such sub-bucket": {principal: subAdmin, name: "absent", want: domain.ErrSubBucketNotFound},
		"refused without s3:DeleteSubBucket": {
			principal: subViewer, name: "reports", want: domain.ErrAccessDenied, denied: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSubBucketFixture(t)
			f.register(t, "site", map[string]string{"reports": reportsPolicy})
			if tt.setup != nil {
				tt.setup(t, f)
			}
			err := f.subs.Delete(ctx, tt.principal, "site", tt.name)
			if (tt.want == nil) != (err == nil) || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("Delete = %v, want %v", err, tt.want)
			}
			_, err = f.meta.GetSubBucket(ctx, "site", "reports")
			if gone := errors.Is(err, domain.ErrSubBucketNotFound); gone != (tt.want == nil && tt.name == "reports") {
				t.Errorf("after: the sub-bucket is gone: %v (%v)", gone, err)
			}
			records := f.records(t)
			switch {
			case tt.want == nil:
				r := wantRecord(t, records, iam.ActionDeleteSubBucket, audit.Succeeded)
				if r.Details["sub_bucket"] != "reports" || r.Actor != actorName(tt.principal) {
					t.Errorf("the record of the deletion = %+v", r)
				}
			case tt.denied:
				wantRecord(t, records, iam.ActionDeleteSubBucket, audit.Denied)
			default:
				// A deletion the store refused is not recorded as made.
				for _, r := range records {
					if r.Outcome == audit.Succeeded {
						t.Errorf("a deletion that was refused left %+v", r)
					}
				}
			}
		})
	}
}

// TestSubBucketServiceList: whoever holds s3:ListSubBuckets is shown every
// sub-bucket; anybody else those whose keys they may list - by their own
// policies, the bucket's or the sub-bucket's - and one they may not list is
// invisible, without an error and without an audit record. A caller who can
// see nothing of the bucket is refused, audited, and not told whether the
// bucket is there.
func TestSubBucketServiceList(t *testing.T) {
	t.Parallel()
	all := []string{"broken", "open", "private", "reports"}
	tests := map[string]struct {
		principal iam.Principal
		bucket    string
		// none is a bucket without sub-buckets.
		none   bool
		want   []string
		err    error
		denied bool
	}{
		"root":                       {principal: porttest.Root(), want: all},
		"with s3:ListSubBuckets":     {principal: subViewer, want: all},
		"with s3:*":                  {principal: subAdmin, want: all},
		"who may list the bucket":    {principal: subReader, want: []string{"open", "private", "reports"}},
		"by the sub-bucket's policy": {principal: subBob, want: []string{"reports"}},
		"who may list nothing":       {principal: subNobody, err: domain.ErrAccessDenied, denied: true},
		"who may list the bucket, which has none": {
			principal: subReader, none: true, want: []string{},
		},
		"root, of a bucket that has none":                   {principal: porttest.Root(), none: true, want: []string{}},
		"with s3:ListSubBuckets, of a bucket that has none": {principal: subViewer, none: true, want: []string{}},
		"who may list nothing, of a bucket that has none": {
			principal: subNobody, none: true, err: domain.ErrAccessDenied, denied: true,
		},
		"no such bucket": {principal: subAdmin, bucket: "absent", err: domain.ErrBucketNotFound},
		"no such bucket, for one who may see none": {
			principal: subNobody, bucket: "absent", err: domain.ErrAccessDenied, denied: true,
		},
		"no such bucket, for one who may list it": {
			principal: iam.Principal{AccessKeyID: "R2", User: "rob", Policies: []iam.Document{{
				Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:ListBucket"}, Resources: []string{"*"}}},
			}}},
			bucket: "absent", err: domain.ErrBucketNotFound,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSubBucketFixture(t)
			if !tt.none {
				f.register(t, "site", map[string]string{
					"reports": reportsPolicy, "private": privatePolicy, "open": "",
					// A stored policy that does not parse: whoever cannot
					// list every sub-bucket is not shown this one.
					"broken": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/elsewhere/*"}]}`,
				})
			}
			bucketName := tt.bucket
			if bucketName == "" {
				bucketName = "site"
			}
			got, err := f.subs.List(ctx, tt.principal, bucketName)
			if (tt.err == nil) != (err == nil) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("List = %v, %v; want %v", got, err, tt.err)
			}
			if tt.err == nil {
				if got == nil {
					t.Error("List = nil, want a list, empty or not")
				}
				names := []string{}
				for _, sub := range got {
					names = append(names, sub.Name)
					// A listing shows no policy: that is Get's, behind
					// s3:GetSubBucketPolicy.
					if sub.Policy != nil || !sub.CreatedAt.Equal(testNow) {
						t.Errorf("List shows %s with the policy %q, created %v", sub.Name, sub.Policy, sub.CreatedAt)
					}
				}
				if !slices.Equal(names, tt.want) {
					t.Errorf("List = %v, want %v", names, tt.want)
				}
			}
			records := f.records(t)
			if tt.denied {
				r := wantRecord(t, records, iam.ActionListSubBuckets, audit.Denied)
				if r.Resource != "arn:aws:s3:::"+bucketName {
					t.Errorf("the record of the refusal = %+v", r)
				}
			}
			if !tt.denied && len(records) != 0 {
				t.Errorf("filtering left audit records: %+v", records)
			}
		})
	}
}

// TestSubBucketServiceGet: a sub-bucket and its policy are read by whoever
// holds s3:GetSubBucketPolicy on the bucket; a refusal is audited.
func TestSubBucketServiceGet(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		principal iam.Principal
		bucket    string
		name      string
		policy    string
		err       error
		denied    bool
	}{
		"with a policy":                  {principal: subViewer, name: "reports", policy: reportsPolicy},
		"without a policy":               {principal: subAdmin, name: "open"},
		"by root":                        {principal: porttest.Root(), name: "private", policy: privatePolicy},
		"no such sub-bucket":             {principal: subAdmin, name: "absent", err: domain.ErrSubBucketNotFound},
		"no such bucket":                 {principal: subAdmin, bucket: "absent", name: "reports", err: domain.ErrBucketNotFound},
		"refused, though it may list it": {principal: subReader, name: "reports", err: domain.ErrAccessDenied, denied: true},
		"refused, though a sub-bucket lets it read": {principal: subBob, name: "reports", err: domain.ErrAccessDenied, denied: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newSubBucketFixture(t)
			f.register(t, "site", map[string]string{"reports": reportsPolicy, "private": privatePolicy, "open": ""})
			bucketName := tt.bucket
			if bucketName == "" {
				bucketName = "site"
			}
			got, err := f.subs.Get(ctx, tt.principal, bucketName, tt.name)
			if (tt.err == nil) != (err == nil) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("Get = %+v, %v; want %v", got, err, tt.err)
			}
			if tt.err == nil && (got.Name != tt.name || string(got.Policy) != tt.policy || !got.CreatedAt.Equal(testNow)) {
				t.Errorf("Get = %+v", got)
			}
			policy, err := f.subs.GetPolicy(ctx, tt.principal, bucketName, tt.name)
			if (tt.err == nil) != (err == nil) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("GetPolicy = %q, %v; want %v", policy, err, tt.err)
			}
			if string(policy) != tt.policy || (tt.policy == "" && policy != nil) {
				t.Errorf("GetPolicy = %q, want %q", policy, tt.policy)
			}
			records := f.records(t)
			if tt.denied {
				// Get and GetPolicy are refused alike: the first refusal is
				// a record, its repeat is counted into another.
				if !slices.ContainsFunc(records, func(r audit.Record) bool {
					return r.Action == string(iam.ActionGetSubBucketPolicy) && r.Outcome == audit.Denied && r.Actor == actorName(tt.principal)
				}) {
					t.Errorf("no record of the refusal among %+v", records)
				}
			} else if len(records) != 0 {
				t.Errorf("a read left audit records: %+v", records)
			}
		})
	}
}

// unreadableAfterPut is a store of sub-buckets from which a sub-bucket
// cannot be read back: what Put returns is then what it put.
type unreadableAfterPut struct {
	*porttest.MemMetadata
}

func (unreadableAfterPut) GetSubBucket(context.Context, string, string) (bucket.SubBucket, error) {
	return bucket.SubBucket{}, domain.ErrUnavailable
}

// overwrittenAfterPut is a store in which another writer puts another policy
// on the sub-bucket right after each put, before it is read back.
type overwrittenAfterPut struct {
	*porttest.MemMetadata
}

func (o overwrittenAfterPut) GetSubBucket(ctx context.Context, bucketName, name string) (bucket.SubBucket, error) {
	sub, err := o.MemMetadata.GetSubBucket(ctx, bucketName, name)
	sub.Policy = []byte(privateFor(name))
	sub.CreatedAt = testNow.Add(-24 * time.Hour)
	return sub, err
}

// changedMind is an authorizer whose answer changes between a call's
// filtering and its refusal: Allowed says no, Authorize yes - as when a
// policy that allows the caller is put in between.
type changedMind struct{}

func (changedMind) Allowed(context.Context, iam.Request) (bool, error) { return false, nil }
func (changedMind) Authorize(context.Context, iam.Request) error       { return nil }

// TestSubBucketServiceListWhenTheAnswerChanges: a caller found to see
// nothing, whom the authorizer then allows after all, is given what the call
// found - the bucket is not there, or nothing of it is visible - and not a
// refusal.
func TestSubBucketServiceListWhenTheAnswerChanges(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		bucket string
		want   error
	}{
		"no such bucket":      {bucket: "absent", want: domain.ErrBucketNotFound},
		"nothing of it shown": {bucket: "site"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSubBucketFixture(t)
			f.register(t, "site", map[string]string{"reports": reportsPolicy})
			svc := app.NewSubBucketService(f.meta, changedMind{}, f.auditor, f.clock, slog.New(slog.DiscardHandler))
			got, err := svc.List(t.Context(), subNobody, tt.bucket)
			if (tt.want == nil) != (err == nil) || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("List = %v, %v; want %v", got, err, tt.want)
			}
			if tt.want == nil && (got == nil || len(got) != 0) {
				t.Errorf("List = %#v, want an empty list", got)
			}
		})
	}
}

// TestSubBucketServiceWithoutAuditTrail: a service that cannot record a
// change makes none; one whose store cannot read the change back reports
// what it made.
func TestSubBucketServiceWithoutAuditTrail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSubBucketFixture(t)
	f.register(t, "site", map[string]string{"open": ""})
	unaudited := app.NewSubBucketService(f.meta, f.authz, nil, f.clock, slog.New(slog.DiscardHandler))
	if _, err := unaudited.Put(ctx, subAdmin, "site", "reports", nil); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Put without an auditor = %v, want refused", err)
	}
	if err := unaudited.Delete(ctx, subAdmin, "site", "open"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Delete without an auditor = %v, want refused", err)
	}
	if subs, err := f.meta.ListSubBuckets(ctx, "site"); err != nil || len(subs) != 1 || subs[0].Name != "open" {
		t.Errorf("after: %+v, %v; want nothing changed", subs, err)
	}

	logs := &syncBuffer{}
	blind := app.NewSubBucketService(unreadableAfterPut{f.meta}, f.authz, f.auditor, f.clock, slog.New(slog.NewJSONHandler(logs, nil)))
	got, err := blind.Put(ctx, subAdmin, "site", "reports", []byte(reportsPolicy))
	if err != nil || got.Name != "reports" || string(got.Policy) != reportsPolicy || !got.CreatedAt.Equal(f.clock.Now()) {
		t.Errorf("Put = %+v, %v", got, err)
	}
	// The failure is logged; the policy is not.
	if !strings.Contains(logs.String(), "cannot be read back") || !strings.Contains(logs.String(), `"sub_bucket":"reports"`) ||
		strings.Contains(logs.String(), "arn:aws:iam") {
		t.Errorf("the log: %s", logs.String())
	}
}

// TestSubBucketServicePutReturnsItsOwnPolicy: when another writer has put
// another policy between this call's change and its reading back, the call
// returns its own policy with the time the sub-bucket was created - never
// the other writer's document.
func TestSubBucketServicePutReturnsItsOwnPolicy(t *testing.T) {
	t.Parallel()
	f := newSubBucketFixture(t)
	raced := app.NewSubBucketService(overwrittenAfterPut{f.meta}, f.authz, f.auditor, f.clock, slog.New(slog.DiscardHandler))
	got, err := raced.Put(t.Context(), subAdmin, "site", "reports", []byte(reportsPolicy))
	if err != nil || got.Name != "reports" || string(got.Policy) != reportsPolicy || !got.CreatedAt.Equal(testNow.Add(-24*time.Hour)) {
		t.Errorf("Put = %+v, %v; want its own policy and the stored time", got, err)
	}
	// Without a policy it returns none, and not the other writer's.
	got, err = raced.Put(t.Context(), subAdmin, "site", "reports", nil)
	if err != nil || got.Policy != nil {
		t.Errorf("Put without a policy = %+v, %v", got, err)
	}
}
