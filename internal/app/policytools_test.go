package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// policyToolsFixture is the policy tools over the IAM use cases, with the
// bucket site of subBucketSite.
type policyToolsFixture struct {
	*iamFixture
	site  *bucketSite
	tools *app.PolicyToolsService
	// admin holds cluster-admin, buckets bucket-admin, watcher auditor,
	// reader read-only; bound is admin's key with an inline policy.
	admin, buckets, watcher, reader, bound iam.Principal
}

// bucketSite is the store of the bucket site, and the use cases that put
// its policy and its sub-buckets'.
type bucketSite struct {
	buckets    *app.BucketService
	subBuckets *app.SubBucketService
}

func newPolicyToolsFixture(t *testing.T) *policyToolsFixture {
	t.Helper()
	ctx := t.Context()
	f := &policyToolsFixture{iamFixture: newIAMFixture(t)}
	meta := subBucketSite(t)
	// The stored policy of the bucket other is damaged: it is of another
	// bucket, and quotes the marker.
	damaged := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::elsewhere/` + storeMarker + `"}]}`
	if err := meta.SetBucketConfig(ctx, "other", bucket.ConfigPolicy, []byte(damaged)); err != nil {
		t.Fatal(err)
	}
	f.authz.WithBucketPolicies(meta).WithSubBuckets(meta)
	f.tools = app.NewPolicyToolsService(f.authz, f.auditor)
	f.site = &bucketSite{
		buckets:    app.NewBucketService(meta, f.authz, f.clock).WithAuditor(f.auditor),
		subBuckets: app.NewSubBucketService(meta, f.authz, f.auditor, f.clock, slog.New(slog.NewJSONHandler(f.logs, nil))),
	}
	f.admin = f.as(t, f.user(t, "ada", iam.RoleClusterAdmin))
	f.buckets = f.as(t, f.user(t, "ben", iam.RoleBucketAdmin))
	f.watcher = f.as(t, f.user(t, "aud", iam.RoleAuditor))
	f.reader = f.as(t, f.user(t, "rea", iam.RoleReadOnly))
	bound, _, err := f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{
		Owner: "ada", InlinePolicy: []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:*","Resource":"*"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.bound = f.as(t, bound)
	f.auditor.Flush(ctx)
	return f
}

// TestPolicyCatalogue: the catalogue is the domain's, to whoever holds
// admin:GetPolicyCatalogue; a caller who does not is refused, audited.
func TestPolicyCatalogue(t *testing.T) {
	t.Parallel()
	f := newPolicyToolsFixture(t)
	ctx := t.Context()
	tests := []struct {
		name   string
		caller iam.Principal
		want   error
	}{
		{"root", f.root, nil},
		{"cluster-admin", f.admin, nil},
		{"bucket-admin, by name", f.buckets, nil},
		{"auditor, by admin:Get*", f.watcher, nil},
		{"read-only", f.reader, domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(recordsOf(ctx, t, f.iamFixture))
			got, err := f.tools.Catalogue(ctx, tt.caller)
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("Catalogue = %v, want %v", err, tt.want)
			}
			records := recordsOf(ctx, t, f.iamFixture)[before:]
			if tt.want != nil {
				if len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Action != string(iam.ActionGetPolicyCatalogue) ||
					records[0].Resource != "arn:cactusfs:admin:::policy-catalogue" {
					t.Errorf("a refusal left %+v", records)
				}
				return
			}
			if len(records) != 0 {
				t.Errorf("reading the catalogue left %+v", records)
			}
			if got.Version != iam.PolicyVersion || len(got.Actions) != len(iam.Catalogue()) || got.Actions[0].Name != iam.Catalogue()[0].Name ||
				len(got.ConditionKeys) != len(iam.ConditionKeys()) || len(got.Resources) != len(iam.ResourceKinds()) ||
				got.Limits != iam.PolicyLimits() || got.BucketPolicyPrincipals.UserARNPrefix != iam.BucketPolicyPrincipals().UserARNPrefix {
				t.Errorf("the catalogue: %+v", got)
			}
		})
	}
}

// Documents of each kind, valid and not.
const (
	identityValid   = `{"Version":"2012-10-17","Statement":[{"Sid":"Read","Effect":"Allow","Action":["s3:Get*","s3:GetObjet"],"Resource":"*"}]}`
	identityInvalid = `{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"s3:GetObject","Resource":"*"},{"Effect":"Allow","Action":"s3:GetObject","Resource":"nope"}]}`
	bucketValid     = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::site/*"}]}`
	bucketInvalid   = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::elsewhere/*"}]}`
	subValid        = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/team/*"}]}`
	subInvalid      = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/other/*"}]}`
)

// kindOfDocument is one of the four places a policy document is kept: how
// it is validated, how it is put, and how it is parsed.
type kindOfDocument struct {
	name     string
	validate func(f *policyToolsFixture, p iam.Principal, document string) (app.PolicyValidation, error)
	put      func(f *policyToolsFixture, p iam.Principal, document string) error
	parse    func(document string) error
	// action and resource are the authorization of both.
	action   iam.Action
	resource string
	valid    string
	invalid  string
}

func documentKinds(t *testing.T) []kindOfDocument {
	ctx := t.Context()
	return []kindOfDocument{
		{
			name: "named policy",
			validate: func(f *policyToolsFixture, p iam.Principal, document string) (app.PolicyValidation, error) {
				return f.tools.ValidateIdentity(ctx, p, app.IdentityPolicyValidation{Document: []byte(document), Name: "team"})
			},
			put: func(f *policyToolsFixture, p iam.Principal, document string) error {
				return f.iam.PutPolicy(ctx, p, "team", []byte(document), true)
			},
			parse:  func(document string) error { _, err := iam.ParsePolicy([]byte(document)); return err },
			action: iam.ActionPutPolicy, resource: "arn:cactusfs:admin:::policy/team", valid: identityValid, invalid: identityInvalid,
		},
		{
			name: "policy without a name",
			validate: func(f *policyToolsFixture, p iam.Principal, document string) (app.PolicyValidation, error) {
				return f.tools.ValidateIdentity(ctx, p, app.IdentityPolicyValidation{Document: []byte(document)})
			},
			parse:  func(document string) error { _, err := iam.ParsePolicy([]byte(document)); return err },
			action: iam.ActionPutPolicy, resource: "arn:cactusfs:admin:::policy", valid: identityValid, invalid: identityInvalid,
		},
		{
			name: "inline policy of a key",
			validate: func(f *policyToolsFixture, p iam.Principal, document string) (app.PolicyValidation, error) {
				return f.tools.ValidateIdentity(ctx, p, app.IdentityPolicyValidation{Document: []byte(document), KeyOwner: "rea"})
			},
			put: func(f *policyToolsFixture, p iam.Principal, document string) error {
				_, _, err := f.iam.CreateAPIKey(ctx, p, app.APIKeyInput{Owner: "rea", InlinePolicy: []byte(document)})
				return err
			},
			parse:  func(document string) error { _, err := iam.ParsePolicy([]byte(document)); return err },
			action: iam.ActionCreateKey, resource: "arn:cactusfs:admin:::user/rea", valid: identityValid, invalid: identityInvalid,
		},
		{
			name: "bucket policy",
			validate: func(f *policyToolsFixture, p iam.Principal, document string) (app.PolicyValidation, error) {
				return f.tools.ValidateBucket(ctx, p, "site", []byte(document))
			},
			put: func(f *policyToolsFixture, p iam.Principal, document string) error {
				return f.site.buckets.SetPolicy(ctx, p, "site", []byte(document))
			},
			parse:  func(document string) error { _, err := iam.ParseBucketPolicy([]byte(document), "site"); return err },
			action: iam.ActionPutBucketPolicy, resource: "arn:aws:s3:::site", valid: bucketValid, invalid: bucketInvalid,
		},
		{
			name: "sub-bucket policy",
			validate: func(f *policyToolsFixture, p iam.Principal, document string) (app.PolicyValidation, error) {
				return f.tools.ValidateSubBucket(ctx, p, "site", "team", []byte(document))
			},
			put: func(f *policyToolsFixture, p iam.Principal, document string) error {
				_, err := f.site.subBuckets.Put(ctx, p, "site", "team", []byte(document))
				return err
			},
			parse: func(document string) error {
				_, err := iam.ParseSubBucketPolicy([]byte(document), "site", "team")
				return err
			},
			action: iam.ActionPutSubBucket, resource: "arn:aws:s3:::site", valid: subValid, invalid: subInvalid,
		},
	}
}

// snapshot is what the stores hold that a validation could change: the
// trail, the named policies, the keys of rea, the policy of site and its
// sub-buckets.
func (f *policyToolsFixture) snapshot(ctx context.Context, t *testing.T) string {
	t.Helper()
	policies, err := f.store.ListPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := f.store.ListAPIKeys(ctx, "rea")
	if err != nil {
		t.Fatal(err)
	}
	document, err := f.site.buckets.Policy(ctx, f.root, "site")
	if err != nil {
		t.Fatal(err)
	}
	subs, err := f.site.subBuckets.List(ctx, f.root, "site")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(len(recordsOf(ctx, t, f.iamFixture)), len(policies), len(keys), string(document), subs)
}

// TestValidatePolicy: each kind of document is validated as its put is
// authorized, stores nothing, and finds the faults the parser and the put
// find - and only what the parser says of the document is told.
func TestValidatePolicy(t *testing.T) {
	t.Parallel()
	for _, kind := range documentKinds(t) {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			f := newPolicyToolsFixture(t)
			ctx := t.Context()
			tests := []struct {
				name     string
				caller   iam.Principal
				document string
				want     error
				valid    bool
			}{
				{"valid, by root", f.root, kind.valid, nil, true},
				{"valid, by cluster-admin", f.admin, kind.valid, nil, true},
				{"refused, by cluster-admin", f.admin, kind.invalid, nil, false},
				{"by a caller who may not put it", f.reader, kind.valid, domain.ErrAccessDenied, false},
				{"a refused document, by a caller who may not put it", f.reader, kind.invalid, domain.ErrAccessDenied, false},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					before := f.snapshot(ctx, t)
					trail := len(recordsOf(ctx, t, f.iamFixture))
					got, err := kind.validate(f, tt.caller, tt.document)
					if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
						t.Fatalf("validate = %+v, %v; want %v", got, err, tt.want)
					}
					records := recordsOf(ctx, t, f.iamFixture)[trail:]
					if tt.want != nil {
						if len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Action != string(kind.action) || records[0].Resource != kind.resource {
							t.Fatalf("a refusal left %+v", records)
						}
						if len(got.Faults) != 0 {
							t.Errorf("a refusal told faults: %+v", got)
						}
						return
					}
					if after := f.snapshot(ctx, t); after != before {
						t.Errorf("a validation changed the stores: %s, then %s", before, after)
					}
					if got.Valid != tt.valid || got.Valid != (len(got.Faults) == 0) || got.Faults == nil || got.Statements == nil || got.Truncated {
						t.Fatalf("validation: %+v", got)
					}
					parseErr := kind.parse(tt.document)
					if tt.valid {
						if parseErr != nil || len(got.Statements) != 1 {
							t.Errorf("a valid document: %+v, parsed %v", got, parseErr)
						}
						return
					}
					var refused *iam.PolicyError
					if !errors.As(parseErr, &refused) || !slices.Equal(got.Faults, refused.Faults()) || len(got.Statements) != 0 {
						t.Errorf("faults %+v, the parser's %v", got.Faults, parseErr)
					}
				})
			}
		})
	}
}

// TestPutsReturnTheirFaults: each put of a refused document returns the
// faults validation finds, through errors.As, with its sentinels as they
// were: an identity or inline policy wraps domain.ErrInvalidArgument only,
// a bucket's and a sub-bucket's domain.ErrInvalidPolicy.
func TestPutsReturnTheirFaults(t *testing.T) {
	t.Parallel()
	f := newPolicyToolsFixture(t)
	for _, kind := range documentKinds(t) {
		if kind.put == nil {
			continue
		}
		t.Run(kind.name, func(t *testing.T) {
			validation, err := kind.validate(f, f.admin, kind.invalid)
			if err != nil || validation.Valid {
				t.Fatalf("validate: %+v, %v", validation, err)
			}
			err = kind.put(f, f.admin, kind.invalid)
			refused, ok := app.PolicyFaults(err)
			if !ok || !slices.Equal(refused.Faults(), validation.Faults) || refused.Truncated() != validation.Truncated {
				t.Fatalf("the put returned %v, its faults %v; validation %+v", err, refused, validation.Faults)
			}
			var asFaults *iam.PolicyError
			if !errors.As(err, &asFaults) || !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("the put's error %v is no *iam.PolicyError, or no invalid argument", err)
			}
			if kind.action == iam.ActionPutPolicy || kind.action == iam.ActionCreateKey {
				if errors.Is(err, domain.ErrInvalidPolicy) {
					t.Errorf("an identity policy's refusal wraps ErrInvalidPolicy: %v", err)
				}
			} else if !errors.Is(err, domain.ErrInvalidPolicy) {
				t.Errorf("a bucket's or sub-bucket's refusal does not wrap ErrInvalidPolicy: %v", err)
			}
			// A put refused by the authorizer tells no fault.
			if err := kind.put(f, f.reader, kind.invalid); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("a put by read-only = %v", err)
			} else if _, ok := app.PolicyFaults(err); ok {
				t.Errorf("a refused put told faults: %v", err)
			}
		})
	}
}

// TestPolicyFaultsOfStoredPolicies: no faults are taken from an error that
// is about a document already stored.
func TestPolicyFaultsOfStoredPolicies(t *testing.T) {
	t.Parallel()
	_, parseErr := iam.ParsePolicy([]byte(identityInvalid))
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a put's", fmt.Errorf("iam.PutPolicy: %w", parseErr), true},
		{"a stored one's", fmt.Errorf("%w: stored: %w", domain.ErrIntegrity, parseErr), false},
		{"another error", domain.ErrInvalidArgument, false},
		{"no error", nil, false},
	}
	for _, tt := range tests {
		if _, ok := app.PolicyFaults(tt.err); ok != tt.want {
			t.Errorf("%s: PolicyFaults = %v, want %v", tt.name, ok, tt.want)
		}
	}
}

// TestValidatePolicyRequests: what is not a validation request, a key
// owner's by a bound key, and the analysis and the bound of the faults.
func TestValidatePolicyRequests(t *testing.T) {
	t.Parallel()
	f := newPolicyToolsFixture(t)
	ctx := t.Context()
	doc := []byte(identityValid)
	tests := []struct {
		name string
		run  func() (app.PolicyValidation, error)
		want error
	}{
		{"no document", func() (app.PolicyValidation, error) {
			return f.tools.ValidateIdentity(ctx, f.admin, app.IdentityPolicyValidation{Name: "x"})
		}, domain.ErrInvalidArgument},
		{"a name and a key owner", func() (app.PolicyValidation, error) {
			return f.tools.ValidateIdentity(ctx, f.admin, app.IdentityPolicyValidation{Document: doc, Name: "x", KeyOwner: "rea"})
		}, domain.ErrInvalidArgument},
		{"a name no policy has", func() (app.PolicyValidation, error) {
			return f.tools.ValidateIdentity(ctx, f.admin, app.IdentityPolicyValidation{Document: doc, Name: strings.Repeat("x", 300)})
		}, domain.ErrInvalidArgument},
		{"a key owner no user can be", func() (app.PolicyValidation, error) {
			return f.tools.ValidateIdentity(ctx, f.admin, app.IdentityPolicyValidation{Document: doc, KeyOwner: strings.Repeat("x", 300)})
		}, domain.ErrInvalidArgument},
		{"a bucket name no bucket has", func() (app.PolicyValidation, error) {
			return f.tools.ValidateBucket(ctx, f.admin, "A", []byte(bucketValid))
		}, domain.ErrInvalidArgument},
		{"a bucket policy without a document", func() (app.PolicyValidation, error) {
			return f.tools.ValidateBucket(ctx, f.admin, "site", nil)
		}, domain.ErrInvalidArgument},
		{"a sub-bucket name no sub-bucket has", func() (app.PolicyValidation, error) {
			return f.tools.ValidateSubBucket(ctx, f.admin, "site", "a/b", []byte(subValid))
		}, domain.ErrInvalidArgument},
		{"a sub-bucket in a bucket name no bucket has", func() (app.PolicyValidation, error) {
			return f.tools.ValidateSubBucket(ctx, f.admin, "A", "team", []byte(subValid))
		}, domain.ErrInvalidArgument},
		{"a sub-bucket without a policy", func() (app.PolicyValidation, error) {
			return f.tools.ValidateSubBucket(ctx, f.admin, "site", "team", nil)
		}, domain.ErrInvalidArgument},
		{"a bucket that is not there is no error", func() (app.PolicyValidation, error) {
			return f.tools.ValidateBucket(ctx, f.admin, "nowhere", []byte(strings.ReplaceAll(bucketValid, "site", "nowhere")))
		}, nil},
		{"a sub-bucket that is not there is no error", func() (app.PolicyValidation, error) {
			return f.tools.ValidateSubBucket(ctx, f.admin, "nowhere", "team", []byte(strings.ReplaceAll(subValid, "site", "nowhere")))
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.run()
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("= %+v, %v; want %v", got, err, tt.want)
			}
			if tt.want == nil && !got.Valid {
				t.Errorf("not valid: %+v", got)
			}
		})
	}

	t.Run("a key owner's policy, by a bound key", func(t *testing.T) {
		before := len(recordsOf(ctx, t, f.iamFixture))
		_, err := f.tools.ValidateIdentity(ctx, f.bound, app.IdentityPolicyValidation{Document: doc, KeyOwner: "rea"})
		if !errors.Is(err, domain.ErrAccessDenied) {
			t.Fatalf("= %v, want a refusal", err)
		}
		records := recordsOf(ctx, t, f.iamFixture)[before:]
		if len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Action != string(iam.ActionCreateKey) ||
			records[0].Resource != "arn:cactusfs:admin:::user/rea" || !strings.Contains(records[0].Details["reason"], "cannot make keys") {
			t.Errorf("the refusal left %+v", records)
		}
		// The same key may check a named policy: its owner's role allows it,
		// and so does its own policy.
		if got, err := f.tools.ValidateIdentity(ctx, f.bound, app.IdentityPolicyValidation{Document: doc, Name: "team"}); err != nil || !got.Valid {
			t.Errorf("a named policy, by a bound key: %+v, %v", got, err)
		}
		// Refused for being bound before its owner's name is looked at, as
		// CreateAPIKey refuses it.
		if _, err := f.tools.ValidateIdentity(ctx, f.bound, app.IdentityPolicyValidation{Document: doc, KeyOwner: "no such name"}); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("a bound key, an owner no user can be: %v, want a refusal", err)
		}
	})

	t.Run("a damaged stored policy of the bucket is told as damage, without its text", func(t *testing.T) {
		document := []byte(strings.ReplaceAll(bucketValid, "site", "other"))
		_, err := f.tools.ValidateBucket(ctx, f.admin, "other", document)
		if !errors.Is(err, domain.ErrIntegrity) || strings.Contains(err.Error(), storeMarker) || strings.Contains(err.Error(), "elsewhere") {
			t.Errorf("= %v, want damage without the document", err)
		}
		if _, ok := app.PolicyFaults(err); ok {
			t.Errorf("faults were taken from a stored policy: %v", err)
		}
	})

	t.Run("the analysis", func(t *testing.T) {
		got, err := f.tools.ValidateIdentity(ctx, f.admin, app.IdentityPolicyValidation{Document: doc})
		if err != nil || len(got.Statements) != 1 {
			t.Fatalf("= %+v, %v", got, err)
		}
		s := got.Statements[0]
		if s.Sid != "Read" || s.Effect != iam.Allow || !slices.Contains(s.Actions, iam.ActionGetObject) || !slices.Equal(s.UnmatchedPatterns, []string{"s3:GetObjet"}) || s.OpenToAnyone {
			t.Errorf("the analysis: %+v", s)
		}
		open, err := f.tools.ValidateBucket(ctx, f.buckets, "site", []byte(bucketValid))
		if err != nil || len(open.Statements) != 1 {
			t.Fatalf("= %+v, %v", open, err)
		}
		s = open.Statements[0]
		if !s.OpenToAnyone || !slices.Equal(s.AnonymousActions, []iam.Action{iam.ActionGetObject}) || !slices.Equal(s.NeverAnonymous, []iam.Action{iam.ActionPutObject}) {
			t.Errorf("the analysis of an open statement: %+v", s)
		}
		sub, err := f.tools.ValidateSubBucket(ctx, f.buckets, "site", "team", []byte(subValid))
		if err != nil || len(sub.Statements) != 1 || sub.Statements[0].OpenToAnyone || sub.Statements[0].Effect != iam.Deny {
			t.Errorf("the analysis of a sub-bucket's: %+v, %v", sub, err)
		}
	})

	t.Run("at most a hundred faults", func(t *testing.T) {
		bad := strings.TrimSuffix(strings.Repeat(`{"Effect":"x","Action":["a","b"],"Resource":"c"},`, iam.MaxPolicyStatements), ",")
		got, err := f.tools.ValidateIdentity(ctx, f.admin, app.IdentityPolicyValidation{Document: []byte(`{"Version":"2012-10-17","Statement":[` + bad + `]}`)})
		if err != nil || got.Valid || len(got.Faults) != iam.MaxPolicyFaults || !got.Truncated {
			t.Errorf("= %d faults, truncated %v, %v", len(got.Faults), got.Truncated, err)
		}
	})

	t.Run("a document as sent, with data after it", func(t *testing.T) {
		got, err := f.tools.ValidateBucket(ctx, f.admin, "site", append([]byte(bucketValid), '}'))
		if err != nil || got.Valid || len(got.Faults) != 1 || got.Faults[0].Offset < 0 {
			t.Errorf("= %+v, %v", got, err)
		}
		if !bytes.Contains([]byte(got.Faults[0].Reason), []byte("data after the document")) {
			t.Errorf("the fault: %+v", got.Faults[0])
		}
	})
}

// TestValidationReadsNoBucket: validation reads nothing a put's
// authorization does not: no bucket, no sub-bucket, whatever there is.
func TestValidationReadsNoBucket(t *testing.T) {
	t.Parallel()
	f := newPolicyToolsFixture(t)
	ctx := t.Context()
	// The bucket site has a sub-bucket reports, with a policy; validating
	// a policy for it is the same whether or not it is there.
	for _, name := range []string{"reports", "absent"} {
		document := strings.ReplaceAll(subValid, "/team/", "/"+name+"/")
		got, err := f.tools.ValidateSubBucket(ctx, f.buckets, "site", name, []byte(document))
		if err != nil || !got.Valid {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if _, err := f.site.subBuckets.Get(ctx, f.root, "site", "absent"); !errors.Is(err, domain.ErrSubBucketNotFound) {
		t.Errorf("a validation registered a sub-bucket: %v", err)
	}
	if subs, err := f.site.subBuckets.List(ctx, f.root, "site"); err != nil || slices.ContainsFunc(subs, func(s bucket.SubBucket) bool { return s.Name == "absent" }) {
		t.Errorf("the sub-buckets: %v, %v", subs, err)
	}
}
