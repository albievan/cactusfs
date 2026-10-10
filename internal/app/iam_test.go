package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
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

// iamFixture is the IAM use cases over in-memory stores.
type iamFixture struct {
	store      *porttest.MemIAM
	meta       *porttest.MemMetadata
	kms        *porttest.KMS
	clock      *porttest.Clock
	hasher     *porttest.Hasher
	auditor    *app.Auditor
	authz      *app.Authorizer
	iam        *app.IAMService
	identities *app.IdentityService
	sessions   *app.SessionService
	logs       *bytes.Buffer
	root       iam.Principal
}

func newIAMFixture(t *testing.T) *iamFixture {
	t.Helper()
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	f := &iamFixture{
		store: porttest.NewMemIAM(), meta: porttest.NewMemMetadata(), kms: kms, clock: porttest.NewClock(testNow),
		hasher: &porttest.Hasher{}, logs: &bytes.Buffer{},
	}
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	f.auditor = app.NewAuditor(f.store, f.clock, logger)
	f.authz = app.NewAuthorizer(logger, f.clock, f.auditor)
	f.iam = app.NewIAMService(app.IAMServiceConfig{
		Store: f.store, Trail: f.store, Authz: f.authz, Auditor: f.auditor, KMS: kms, Hasher: f.hasher,
		Clock: f.clock, Entropy: rand.Reader,
	})
	f.identities = app.NewIdentityService(f.meta, kms, f.clock, rand.Reader).WithIAM(f.store, f.auditor)
	generated, err := f.identities.EnsureRoot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.root = iam.Principal{AccessKeyID: generated.AccessKeyID, Root: true, Session: session("10.0.0.1")}
	// The record of the bootstrap is written in the background; have it in
	// the trail before the test begins.
	f.auditor.Flush(context.Background())
	provider, err := app.NewLocalIdentityProvider(f.store, f.hasher, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.sessions = app.NewSessionService(app.SessionServiceConfig{
		Sessions: f.store, Users: f.store, Provider: provider, Auditor: f.auditor, Clock: f.clock, Entropy: rand.Reader,
	})
	return f
}

func session(ip string) iam.Session {
	return iam.Session{SourceIP: netip.MustParseAddr(ip), SecureTransport: true, RequestID: "REQ-" + ip}
}

// trail returns the audit trail, with what was queued written first.
func (f *iamFixture) trail(t *testing.T) []audit.Record {
	t.Helper()
	f.auditor.Flush(context.Background())
	records, err := f.store.ListAudit(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(audit.Record{}, records); err != nil {
		t.Fatalf("the audit trail does not verify: %v", err)
	}
	return records
}

// as authenticates with an API key, as a request would, and returns the
// principal.
func (f *iamFixture) as(t *testing.T, creds app.Credentials) iam.Principal {
	t.Helper()
	secret, principal, err := f.identities.SecretFor(context.Background(), creds.AccessKeyID)
	if err != nil {
		t.Fatalf("SecretFor(%s): %v", creds.AccessKeyID, err)
	}
	if string(secret) != creds.SecretKey {
		t.Fatalf("SecretFor(%s) returned another secret than the key was made with", creds.AccessKeyID)
	}
	principal.Session = session("10.0.0.9")
	// The signature verified: the key was used.
	f.identities.Authenticated(principal)
	return principal
}

// user makes a user with roles and an API key for it.
func (f *iamFixture) user(t *testing.T, name string, roles ...string) app.Credentials {
	t.Helper()
	ctx := context.Background()
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: name, Roles: roles}); err != nil {
		t.Fatalf("CreateUser(%s): %v", name, err)
	}
	creds, _, err := f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{Owner: name})
	if err != nil {
		t.Fatalf("CreateAPIKey(%s): %v", name, err)
	}
	return creds
}

const policyReadPhotos = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]}]}`

// TestIAMLifecycle walks through what an administrator does - policy, role,
// group, user, key - and what the key can then do.
func TestIAMLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)

	if err := f.iam.PutPolicy(ctx, f.root, "photo-readers", []byte(policyReadPhotos), true); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	if err := f.iam.PutRole(ctx, f.root, iam.Role{Name: "photo-viewer", Policies: []string{"photo-readers"}}, true); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	if err := f.iam.PutGroup(ctx, f.root, iam.Group{Name: "marketing", Roles: []string{"photo-viewer"}}, true); err != nil {
		t.Fatalf("PutGroup: %v", err)
	}
	created, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "alice", Groups: []string{"marketing"}, Password: "correct horse battery"})
	if err != nil || created.Name != "alice" || !created.CreatedAt.Equal(testNow) {
		t.Fatalf("CreateUser = %+v, %v", created, err)
	}
	// The password hash never leaves the use cases.
	if created.PasswordHash != "set" {
		t.Errorf("CreateUser returned the password hash: %q", created.PasswordHash)
	}
	if got, _ := f.iam.GetUser(ctx, f.root, "alice"); got.PasswordHash != "set" {
		t.Errorf("GetUser returned the password hash: %q", got.PasswordHash)
	}
	if users, _ := f.iam.ListUsers(ctx, f.root); len(users) != 1 || users[0].PasswordHash != "set" {
		t.Errorf("ListUsers = %+v", users)
	}
	if stored, _ := f.store.GetUser(ctx, "alice"); stored.PasswordHash != "test-hash:correct horse battery" {
		t.Errorf("the stored hash = %q", stored.PasswordHash)
	}

	creds, key, err := f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{Owner: "alice", Description: "laptop"})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if iam.ValidateAccessKeyID(creds.AccessKeyID) != nil || iam.ValidateSecret(creds.SecretKey) != nil {
		t.Errorf("generated credentials are not valid")
	}
	if key.Owner != "alice" || key.Description != "laptop" || len(key.Secret.Ciphertext) != 0 {
		t.Errorf("returned key = %+v", key)
	}
	// The secret is stored sealed: nothing in the store reads as it.
	stored, _ := f.store.ListAPIKeys(ctx, "alice")
	if len(stored) != 1 || bytes.Contains(stored[0].Secret.Ciphertext, []byte(creds.SecretKey)) || stored[0].Secret.KMSKeyID != "test:1" {
		t.Fatalf("stored key = %+v", stored)
	}
	// ...and listing shows no secret in any form.
	listed, err := f.iam.ListAPIKeys(ctx, f.root, "alice")
	if err != nil || len(listed) != 1 || len(listed[0].Secret.Ciphertext)+len(listed[0].Secret.WrappedKey) != 0 {
		t.Errorf("ListAPIKeys = %+v, %v", listed, err)
	}

	alice := f.as(t, creds)
	if alice.User != "alice" || alice.Root || len(alice.Policies) != 1 {
		t.Fatalf("principal = %+v", alice)
	}
	photo := iam.Resource{Bucket: "photos", Key: "2026/a.jpg"}
	if err := f.authz.Authorize(ctx, iam.Request{Principal: alice, Action: iam.ActionGetObject, Resource: photo}); err != nil {
		t.Errorf("alice reading a photo: %v", err)
	}
	for name, req := range map[string]iam.Request{
		"writing a photo":   {Principal: alice, Action: iam.ActionPutObject, Resource: photo},
		"reading elsewhere": {Principal: alice, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "finance", Key: "q3"}},
		"managing users":    {Principal: alice, Action: iam.ActionPutUser, Resource: iam.Resource{Admin: "user/alice"}},
		"reading the audit": {Principal: alice, Action: iam.ActionGetAuditLog, Resource: iam.Resource{Admin: "audit"}},
	} {
		if err := f.authz.Authorize(ctx, req); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("alice %s: %v, want %v", name, err, domain.ErrAccessDenied)
		}
	}

	// What she may do changes with her roles, at the next request.
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Groups: []string{"marketing"}, Roles: []string{iam.RoleReadWrite}}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if err := f.authz.Authorize(ctx, iam.Request{Principal: f.as(t, creds), Action: iam.ActionPutObject, Resource: photo}); err != nil {
		t.Errorf("alice writing after being given read-write: %v", err)
	}
	if stored, _ := f.store.GetUser(ctx, "alice"); stored.PasswordHash != "test-hash:correct horse battery" {
		t.Error("an update without a password changed the password")
	}

	// Disabled: the key stops at once; enabled again, it works again.
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.identities.SecretFor(ctx, creds.AccessKeyID); !errors.Is(err, domain.ErrCredentialNotFound) {
		t.Errorf("key of a disabled user: %v, want %v", err, domain.ErrCredentialNotFound)
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Revoked: gone at once.
	if err := f.iam.DeleteAPIKey(ctx, f.root, creds.AccessKeyID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if _, _, err := f.identities.SecretFor(ctx, creds.AccessKeyID); !errors.Is(err, domain.ErrCredentialNotFound) {
		t.Errorf("revoked key: %v, want %v", err, domain.ErrCredentialNotFound)
	}
	if err := f.iam.DeleteAPIKey(ctx, f.root, creds.AccessKeyID); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Errorf("revoking a key twice = %v", err)
	}

	// Reads, and taking it all apart again.
	if g, err := f.iam.GetGroup(ctx, f.root, "marketing"); err != nil || len(g.Roles) != 1 || !g.CreatedAt.Equal(testNow) {
		t.Errorf("GetGroup = %+v, %v", g, err)
	}
	if groups, err := f.iam.ListGroups(ctx, f.root); err != nil || len(groups) != 1 {
		t.Errorf("ListGroups = %v, %v", groups, err)
	}
	if r, err := f.iam.GetRole(ctx, f.root, "photo-viewer"); err != nil || len(r.Policies) != 1 {
		t.Errorf("GetRole = %+v, %v", r, err)
	}
	if roles, err := f.iam.ListRoles(ctx, f.root); err != nil || len(roles) != 6 {
		t.Errorf("ListRoles = %d, %v; want the five built-in ones and one more", len(roles), err)
	}
	if p, err := f.iam.GetPolicy(ctx, f.root, "photo-readers"); err != nil || len(p.Document.Statements) != 1 {
		t.Errorf("GetPolicy = %+v, %v", p, err)
	}
	if policies, err := f.iam.ListPolicies(ctx, f.root); err != nil || len(policies) != 6 {
		t.Errorf("ListPolicies = %d, %v", len(policies), err)
	}
	f.clock.Advance(time.Hour)
	if err := f.iam.PutPolicy(ctx, f.root, "photo-readers", []byte(policyReadPhotos), false); err != nil {
		t.Errorf("replace policy: %v", err)
	}
	if p, _ := f.iam.GetPolicy(ctx, f.root, "photo-readers"); !p.CreatedAt.Equal(testNow) {
		t.Errorf("replacing a policy changed when it was created: %v", p.CreatedAt)
	}
	if err := f.iam.PutRole(ctx, f.root, iam.Role{Name: "photo-viewer", Policies: []string{"photo-readers"}}, false); err != nil {
		t.Errorf("replace role: %v", err)
	}
	if err := f.iam.PutGroup(ctx, f.root, iam.Group{Name: "marketing", Roles: []string{"photo-viewer"}}, false); err != nil {
		t.Errorf("replace group: %v", err)
	}
	if err := f.iam.DeleteGroup(ctx, f.root, "marketing"); err != nil {
		t.Errorf("DeleteGroup of an empty group: %v", err)
	}
	for i, step := range []error{
		f.iam.DeleteUser(ctx, f.root, "alice"),
		f.iam.DeleteRole(ctx, f.root, "photo-viewer"),
		f.iam.DeletePolicy(ctx, f.root, "photo-readers"),
	} {
		if step != nil {
			t.Errorf("teardown step %d: %v", i, step)
		}
	}
}

func TestIAMRefusesInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	f.user(t, "alice")

	_, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "bob", Password: "short"})
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a short password = %v", err)
	}
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "bob", Password: strings.Repeat("p", 1025)}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("an oversized password = %v", err)
	}
	if _, err := f.store.GetUser(ctx, "bob"); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Error("a user was created although the password was refused")
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Password: "short"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("update with a short password = %v", err)
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "nobody"}); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Errorf("update of a missing user = %v", err)
	}
	if err := f.iam.PutPolicy(ctx, f.root, "p", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","NotAction":"s3:*","Resource":"*"}]}`), true); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a policy this version cannot read = %v", err)
	}
	for name, in := range map[string]app.APIKeyInput{
		"missing owner":     {Owner: "nobody"},
		"already expired":   {Owner: "alice", ExpiresAt: testNow.Add(-time.Second)},
		"bad inline policy": {Owner: "alice", InlinePolicy: []byte(`{"Version":"1"}`)},
		"long description":  {Owner: "alice", Description: strings.Repeat("d", 300)},
	} {
		if _, _, err := f.iam.CreateAPIKey(ctx, f.root, in); err == nil {
			t.Errorf("CreateAPIKey with %s succeeded", name)
		}
	}
	if keys, _ := f.store.ListAPIKeys(ctx, ""); len(keys) != 1 {
		t.Errorf("%d keys exist after refused creations, want 1", len(keys))
	}
	if _, err := f.iam.ListAPIKeys(ctx, f.root, ""); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("ListAPIKeys without an owner = %v", err)
	}
	// Failures of the store and of the KMS surface, and leave nothing.
	f.store.FailWrites = errBoom
	if _, _, err := f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{Owner: "alice"}); !errors.Is(err, errBoom) {
		t.Errorf("CreateAPIKey with a failing store = %v", err)
	}
	for name, err := range map[string]error{
		"PutGroup":     f.iam.PutGroup(ctx, f.root, iam.Group{Name: "g"}, true),
		"PutRole":      f.iam.PutRole(ctx, f.root, iam.Role{Name: "r"}, true),
		"PutPolicy":    f.iam.PutPolicy(ctx, f.root, "p", []byte(policyReadPhotos), true),
		"DeleteUser":   f.iam.DeleteUser(ctx, f.root, "alice"),
		"DeleteGroup":  f.iam.DeleteGroup(ctx, f.root, "g"),
		"DeleteRole":   f.iam.DeleteRole(ctx, f.root, "r"),
		"DeletePolicy": f.iam.DeletePolicy(ctx, f.root, "p"),
	} {
		if !errors.Is(err, errBoom) {
			t.Errorf("%s with a failing store = %v", name, err)
		}
	}
}

// TestEveryIAMUseCasePassesTheAuthorizer: somebody without the permission is
// refused by every one of them, nothing happens, nothing is told, and the
// refusal is in the audit trail.
func TestEveryIAMUseCasePassesTheAuthorizer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	victim := f.user(t, "victim", iam.RoleReadOnly)
	outsider := f.as(t, f.user(t, "mallory", iam.RoleBucketAdmin)) // all of S3, nothing of admin
	before := len(f.trail(t))

	attempts := map[string]func() error{
		"CreateUser": func() error {
			_, err := f.iam.CreateUser(ctx, outsider, app.UserInput{Name: "eve", Roles: []string{iam.RoleClusterAdmin}})
			return err
		},
		"UpdateUser": func() error {
			_, err := f.iam.UpdateUser(ctx, outsider, app.UserInput{Name: "mallory", Roles: []string{iam.RoleClusterAdmin}})
			return err
		},
		"DeleteUser":        func() error { return f.iam.DeleteUser(ctx, outsider, "victim") },
		"GetUser":           func() error { _, err := f.iam.GetUser(ctx, outsider, "victim"); return err },
		"GetUser of nobody": func() error { _, err := f.iam.GetUser(ctx, outsider, "nobody"); return err },
		"ListUsers":         func() error { _, err := f.iam.ListUsers(ctx, outsider); return err },
		"PutGroup": func() error {
			return f.iam.PutGroup(ctx, outsider, iam.Group{Name: "g", Roles: []string{iam.RoleClusterAdmin}}, true)
		},
		"DeleteGroup":  func() error { return f.iam.DeleteGroup(ctx, outsider, "g") },
		"GetGroup":     func() error { _, err := f.iam.GetGroup(ctx, outsider, "g"); return err },
		"ListGroups":   func() error { _, err := f.iam.ListGroups(ctx, outsider); return err },
		"PutRole":      func() error { return f.iam.PutRole(ctx, outsider, iam.Role{Name: "r"}, true) },
		"DeleteRole":   func() error { return f.iam.DeleteRole(ctx, outsider, "r") },
		"GetRole":      func() error { _, err := f.iam.GetRole(ctx, outsider, iam.RoleAuditor); return err },
		"ListRoles":    func() error { _, err := f.iam.ListRoles(ctx, outsider); return err },
		"PutPolicy":    func() error { return f.iam.PutPolicy(ctx, outsider, "p", []byte(policyReadPhotos), true) },
		"DeletePolicy": func() error { return f.iam.DeletePolicy(ctx, outsider, "p") },
		"GetPolicy":    func() error { _, err := f.iam.GetPolicy(ctx, outsider, iam.RoleAuditor); return err },
		"ListPolicies": func() error { _, err := f.iam.ListPolicies(ctx, outsider); return err },
		"CreateAPIKey for another": func() error {
			_, _, err := f.iam.CreateAPIKey(ctx, outsider, app.APIKeyInput{Owner: "victim"})
			return err
		},
		"CreateAPIKey for itself": func() error {
			_, _, err := f.iam.CreateAPIKey(ctx, outsider, app.APIKeyInput{Owner: "mallory"})
			return err
		},
		"DeleteAPIKey":           func() error { return f.iam.DeleteAPIKey(ctx, outsider, victim.AccessKeyID) },
		"DeleteAPIKey of no key": func() error { return f.iam.DeleteAPIKey(ctx, outsider, "AKIDDOESNOTEXIST") },
		"ListAPIKeys":            func() error { _, err := f.iam.ListAPIKeys(ctx, outsider, "victim"); return err },
		"AuditLog":               func() error { _, err := f.iam.AuditLog(ctx, outsider, 0, 10); return err },
	}
	for name, attempt := range attempts {
		// Whether the thing exists or not, the answer is the same: denied.
		if err := attempt(); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s by somebody without permission = %v, want %v", name, err, domain.ErrAccessDenied)
		}
	}
	if users, _ := f.store.ListUsers(ctx); len(users) != 2 {
		t.Errorf("%d users after the attempts, want 2", len(users))
	}
	if keys, _ := f.store.ListAPIKeys(ctx, ""); len(keys) != 2 {
		t.Errorf("%d keys after the attempts, want 2", len(keys))
	}
	if stored, _ := f.store.GetUser(ctx, "mallory"); len(stored.Roles) != 1 {
		t.Errorf("mallory's roles after the attempts: %v", stored.Roles)
	}

	denied := 0
	for _, record := range f.trail(t)[before:] {
		if record.Outcome != audit.Denied || record.Actor != "user/mallory" || record.SourceIP != "10.0.0.9" {
			t.Errorf("unexpected audit record: %+v", record)
		}
		denied++
	}
	if denied == 0 {
		t.Error("no refusal reached the audit trail")
	}
	if strings.Contains(f.logs.String(), victim.SecretKey) {
		t.Error("a secret is in the log")
	}

	// An auditor may look, at everything, and change nothing.
	auditor := f.as(t, f.user(t, "olga", iam.RoleAuditor))
	if _, err := f.iam.AuditLog(ctx, auditor, 0, 10); err != nil {
		t.Errorf("auditor reading the audit log: %v", err)
	}
	if _, err := f.iam.ListUsers(ctx, auditor); err != nil {
		t.Errorf("auditor listing users: %v", err)
	}
	if _, err := f.iam.ListAPIKeys(ctx, auditor, "victim"); err != nil {
		t.Errorf("auditor listing keys: %v", err)
	}
	if err := f.iam.DeleteUser(ctx, auditor, "victim"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("auditor deleting a user = %v", err)
	}
	if _, _, err := f.iam.CreateAPIKey(ctx, auditor, app.APIKeyInput{Owner: "olga"}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("auditor making itself a key = %v", err)
	}
}

// TestAKeyNeverExceedsItsOwner: a key with an inline policy can do what both
// its owner's policies and its own allow - never more than the owner, even
// when its own policy says "everything", and never more than its own, even
// when its owner is an administrator.
func TestAKeyNeverExceedsItsOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "reader", Roles: []string{iam.RoleReadOnly}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "admin", Roles: []string{iam.RoleClusterAdmin}}); err != nil {
		t.Fatal(err)
	}
	everything := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)
	onlyOneBucket := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::backups/*"}]}`)

	wide, _, err := f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{Owner: "reader", InlinePolicy: everything})
	if err != nil {
		t.Fatal(err)
	}
	narrow, key, err := f.iam.CreateAPIKey(ctx, f.root, app.APIKeyInput{Owner: "admin", InlinePolicy: onlyOneBucket, ExpiresAt: testNow.Add(time.Hour)})
	if err != nil || key.InlinePolicy == nil || !key.ExpiresAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("CreateAPIKey with a policy and an expiry = %+v, %v", key, err)
	}
	object := func(bucket string) iam.Resource { return iam.Resource{Bucket: bucket, Key: "k"} }
	tests := []struct {
		name    string
		creds   app.Credentials
		action  iam.Action
		res     iam.Resource
		allowed bool
	}{
		{"a reader's key, however wide its own policy, reads", wide, iam.ActionGetObject, object("any"), true},
		{"...and does not write", wide, iam.ActionPutObject, object("any"), false},
		{"...and does not administer", wide, iam.ActionPutUser, iam.Resource{Admin: "user/reader"}, false},
		{"an administrator's narrowed key reads its bucket", narrow, iam.ActionGetObject, object("backups"), true},
		{"...and no other", narrow, iam.ActionGetObject, object("finance"), false},
		{"...and does not write", narrow, iam.ActionPutObject, object("backups"), false},
		{"...and does not administer", narrow, iam.ActionDeleteUser, iam.Resource{Admin: "user/reader"}, false},
	}
	for _, tt := range tests {
		err := f.authz.Authorize(ctx, iam.Request{Principal: f.as(t, tt.creds), Action: tt.action, Resource: tt.res})
		if tt.allowed != (err == nil) {
			t.Errorf("%s: allowed = %v (%v)", tt.name, err == nil, err)
		}
	}
	// A narrowed key cannot make itself a wider one.
	if _, _, err := f.iam.CreateAPIKey(ctx, f.as(t, narrow), app.APIKeyInput{Owner: "admin"}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a narrowed key making a key for its owner = %v, want %v", err, domain.ErrAccessDenied)
	}

	// The key expires.
	f.clock.Advance(59 * time.Minute)
	if _, _, err := f.identities.SecretFor(ctx, narrow.AccessKeyID); err != nil {
		t.Errorf("the key a minute before it expires: %v", err)
	}
	f.clock.Advance(time.Minute)
	if _, _, err := f.identities.SecretFor(ctx, narrow.AccessKeyID); !errors.Is(err, domain.ErrCredentialNotFound) {
		t.Errorf("the key at its expiry: %v, want %v", err, domain.ErrCredentialNotFound)
	}
	// Unknown and malformed keys are simply not found; root still is.
	for _, id := range []string{"AKIDUNKNOWN000000000", "", "not a key id", strings.Repeat("A", 500)} {
		if _, _, err := f.identities.SecretFor(ctx, id); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("SecretFor(%q) = %v, want %v", id, err, domain.ErrCredentialNotFound)
		}
	}
	if _, p, err := f.identities.SecretFor(ctx, f.root.AccessKeyID); err != nil || !p.Root || p.User != "" {
		t.Errorf("root: %+v, %v", p, err)
	}
	// A key sealed under another KMS key cannot be used here.
	other, err := porttest.NewKMS("other:1")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := app.NewIdentityService(f.meta, other, f.clock, rand.Reader).WithIAM(f.store, f.auditor)
	if _, _, err := elsewhere.SecretFor(ctx, wide.AccessKeyID); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("a key sealed under another KMS key = %v, want %v", err, domain.ErrKeyUnavailable)
	}
}

// TestKeyUseIsRecordedSparingly: when a key was last used is written to the
// replicated state at most once in a while, and never on the request's time.
func TestKeyUseIsRecordedSparingly(t *testing.T) {
	t.Parallel()
	f := newIAMFixture(t)
	creds := f.user(t, "alice", iam.RoleReadOnly)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.identities.Run(ctx); close(done) }()

	lastUsed := func() time.Time {
		identity, err := f.store.ResolveIdentity(context.Background(), creds.AccessKeyID)
		if err != nil {
			t.Fatal(err)
		}
		return identity.Key.LastUsedAt
	}
	for range 50 {
		f.as(t, creds)
	}
	deadline := time.Now().Add(5 * time.Second)
	for lastUsed().IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !lastUsed().Equal(testNow) {
		t.Fatalf("LastUsedAt = %v, want %v", lastUsed(), testNow)
	}
	// A use a minute later is not worth a write; one after the interval is.
	f.clock.Advance(time.Minute)
	f.as(t, creds)
	time.Sleep(50 * time.Millisecond)
	if !lastUsed().Equal(testNow) {
		t.Errorf("a use a minute later was written: %v", lastUsed())
	}
	f.clock.Advance(5 * time.Minute)
	f.as(t, creds)
	deadline = time.Now().Add(5 * time.Second)
	for lastUsed().Equal(testNow) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !lastUsed().Equal(testNow.Add(6 * time.Minute)) {
		t.Errorf("LastUsedAt = %v, want %v", lastUsed(), testNow.Add(6*time.Minute))
	}
	cancel()
	<-done
}

// TestAuditTrail: every change is in the trail with who, what, on what and
// from where; refusals are too, with repeats counted and not written one by
// one; and a trail that was tampered with is not served as if it were whole.
func TestAuditTrail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	creds := f.user(t, "alice", iam.RoleReadOnly) // two changes: user, key
	alice := f.as(t, creds)

	records := f.trail(t)
	if len(records) != 3 {
		t.Fatalf("%d records after bootstrapping, creating a user and a key, want 3", len(records))
	}
	if boot := records[0]; boot.Actor != "system" || boot.Action != "system:BootstrapRoot" || boot.Details["generated"] != "true" ||
		boot.Details["access_key_id"] != f.root.AccessKeyID {
		t.Errorf("record of the bootstrap = %+v", boot)
	}
	user, key := records[1], records[2]
	if user.Details["roles"] != iam.RoleReadOnly || user.Details["change"] != "create" || user.Details["disabled"] != "false" {
		t.Errorf("the record of the user does not say what the user was given: %+v", user.Details)
	}
	if user.Actor != "root" || user.Action != "admin:PutUser" || user.Resource != "arn:cactusfs:admin:::user/alice" ||
		user.Outcome != audit.Succeeded || user.SourceIP != "10.0.0.1" || user.RequestID != "REQ-10.0.0.1" || !user.Time.Equal(testNow) {
		t.Errorf("record of the user = %+v", user)
	}
	if key.Action != "admin:CreateAPIKey" || key.Details["access_key_id"] != creds.AccessKeyID {
		t.Errorf("record of the key = %+v", key)
	}
	for _, record := range records {
		for _, value := range record.Details {
			if strings.Contains(value, creds.SecretKey) {
				t.Fatal("a secret is in the audit trail")
			}
		}
	}

	// A hundred identical refusals: one record at once, one more that says
	// how many followed.
	for range 100 {
		_ = f.authz.Authorize(ctx, iam.Request{Principal: alice, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "b", Key: "k"}})
	}
	// Another refusal is a record of its own.
	_ = f.authz.Authorize(ctx, iam.Request{Principal: alice, Action: iam.ActionDeleteBucket, Resource: iam.Resource{Bucket: "b"}})
	// Failed authentications, from two addresses.
	for range 30 {
		f.identities.AuthenticationFailed(ctx, "AKIDGUESS", session("203.0.113.5"), "SignatureDoesNotMatch")
	}
	f.identities.AuthenticationFailed(ctx, strings.Repeat("A", 5000), session("203.0.113.6"), strings.Repeat("r", 5000))

	records = f.trail(t)[3:]
	var puts, repeats, deletes, failures int
	for _, r := range records {
		switch {
		case r.Action == "s3:PutObject" && r.Details["repeated"] == "":
			puts++
			if r.Outcome != audit.Denied || r.Actor != "user/alice" || r.AccessKeyID != creds.AccessKeyID || r.Resource != "arn:aws:s3:::b/k" {
				t.Errorf("denial record = %+v", r)
			}
		case r.Action == "s3:PutObject":
			repeats++
			if r.Details["repeated"] != "99" {
				t.Errorf("repeat record says %q, want 99", r.Details["repeated"])
			}
		case r.Action == "s3:DeleteBucket":
			deletes++
		case r.Action == "auth:Authenticate":
			failures++
			if r.Outcome != audit.Failed || r.Actor != "anonymous" || len(r.AccessKeyID) > 512 || len(r.Details["reason"]) > 512 {
				t.Errorf("authentication failure record = %+v", r)
			}
		}
	}
	if puts != 1 || repeats != 1 || deletes != 1 || failures != 3 {
		t.Errorf("records: %d denials, %d repeat summaries, %d other denials, %d authentication failures; want 1, 1, 1, 3",
			puts, repeats, deletes, failures)
	}
	if len(records) != 6 {
		t.Errorf("%d records for 132 refusals, want 6", len(records))
	}

	// Served through the use case, verified. Then somebody edits the state.
	page, err := f.iam.AuditLog(ctx, f.root, 3, 3)
	if err != nil || len(page) != 3 || page[0].Sequence != 4 {
		t.Fatalf("AuditLog(after 3, limit 3) = %d records, %v", len(page), err)
	}
	f.store.Tamper(5, func(r *audit.Record) { r.Actor = "user/somebody-else" })
	if _, err := f.iam.AuditLog(ctx, f.root, 0, 100); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("AuditLog over a tampered record = %v, want %v", err, domain.ErrIntegrity)
	}
	if _, err := f.iam.AuditLog(ctx, f.root, 3, 3); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("a page that contains the tampered record = %v, want %v", err, domain.ErrIntegrity)
	}
	if _, err := f.iam.AuditLog(ctx, f.root, 0, 3); err != nil {
		t.Errorf("a page before the tampered record: %v", err)
	}
}

// TestAuditorNeverHoldsARequestUp: with the trail unreachable and records
// pouring in, the auditor returns at once, its queue stays bounded, what does
// not fit is counted and logged, and what can be written later is.
func TestAuditorNeverHoldsARequestUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	f.auditor.Flush(ctx)
	f.store.FailWrites = errBoom

	begun := time.Now()
	for i := range 5000 {
		f.auditor.Succeeded(ctx, f.auditor.System("system:Test", fmt.Sprintf("thing-%d", i), nil))
	}
	if took := time.Since(begun); took > 5*time.Second {
		t.Errorf("5000 records took %s to hand over", took)
	}
	if f.auditor.Dropped() == 0 {
		t.Error("nothing was dropped although the queue cannot hold 5000 records")
	}
	if !strings.Contains(f.logs.String(), "audit record dropped") {
		t.Error("dropped records were not reported in the log")
	}
	// The trail cannot take them: each failure is logged, with the record.
	f.auditor.Flush(ctx)
	if !strings.Contains(f.logs.String(), "audit record could not be written") {
		t.Error("a record that could not be written was not reported in the log")
	}
	// The trail is back: what arrives now is written.
	f.store.FailWrites = nil
	before, _ := f.store.ListAudit(ctx, 0, 0)
	f.auditor.Denied(ctx, iam.Request{Principal: iam.Principal{AccessKeyID: "K", User: "late"}, Action: iam.ActionGetObject}, "no")
	done := make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	go func() { f.auditor.Run(runCtx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if records, _ := f.store.ListAudit(ctx, 0, 0); len(records) > len(before) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if records, _ := f.store.ListAudit(ctx, 0, 0); len(records) != len(before)+1 {
		t.Errorf("%d records once the trail was back, want one more than %d", len(records), len(before))
	}
}

// accounted adds up what the trail says about refusals: one per record, plus
// the repeats and the suppressed ones that records count.
func accounted(records []audit.Record) (total, written int) {
	for _, r := range records {
		if r.Outcome != audit.Denied && r.Outcome != audit.Failed {
			continue
		}
		written++
		switch {
		case r.Details["repeated"] != "":
			n, _ := strconv.Atoi(r.Details["repeated"])
			total += n
		case r.Details["suppressed"] != "":
			n, _ := strconv.Atoi(r.Details["suppressed"])
			total += n
		default:
			total++
		}
	}
	return total, written
}

// TestRefusalsCannotFloodTheTrail: whoever is refused decides almost
// everything about the refusal - the key they claim, the object they name,
// how often and how fast. None of it buys more than a handful of records in
// the replicated log, and none of it makes a refusal go uncounted.
func TestRefusalsCannotFloodTheTrail(t *testing.T) {
	t.Parallel()

	t.Run("a different key claimed every time", func(t *testing.T) {
		t.Parallel()
		f := newIAMFixture(t)
		base := len(f.trail(t))
		for i := range 3000 {
			f.identities.AuthenticationFailed(t.Context(), fmt.Sprintf("AKIDINVENTED%06d", i), session("203.0.113.5"), "InvalidAccessKeyId")
		}
		total, written := accounted(f.trail(t)[base:])
		if total != 3000 || written != 2 {
			t.Errorf("3000 failures with invented keys: %d accounted for in %d records; want 3000 in 2", total, written)
		}
	})

	t.Run("a different object named every time", func(t *testing.T) {
		t.Parallel()
		f := newIAMFixture(t)
		alice := f.as(t, f.user(t, "alice", iam.RoleReadOnly))
		base := len(f.trail(t))
		for i := range 3000 {
			_ = f.authz.Authorize(t.Context(), iam.Request{Principal: alice, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "b", Key: fmt.Sprint(i)}})
		}
		total, written := accounted(f.trail(t)[base:])
		if total != 3000 || written != 2 {
			t.Errorf("3000 denials on different objects: %d accounted for in %d records; want 3000 in 2", total, written)
		}
	})

	t.Run("from a different address every time", func(t *testing.T) {
		t.Parallel()
		f := newIAMFixture(t)
		base := len(f.trail(t))
		for i := range 3000 {
			f.identities.AuthenticationFailed(t.Context(), "AKID", session(fmt.Sprintf("198.51.%d.%d", i/250, i%250)), "SignatureDoesNotMatch")
		}
		total, written := accounted(f.trail(t)[base:])
		// A budget of kinds per window, then one record for the rest.
		if total != 3000 || written > 70 {
			t.Errorf("3000 failures from 3000 addresses: %d accounted for in %d records; want 3000 in at most 70", total, written)
		}
	})

	t.Run("over a long time", func(t *testing.T) {
		t.Parallel()
		f := newIAMFixture(t)
		base := len(f.trail(t))
		// A steady stream that crosses the end of a window again and
		// again, with nothing summarising in between: no count may be
		// lost at the boundary.
		for round := range 5 {
			for range 1000 {
				f.identities.AuthenticationFailed(t.Context(), "AKID", session("203.0.113.9"), "SignatureDoesNotMatch")
			}
			f.clock.Advance(11*time.Second + time.Duration(round)*time.Second)
		}
		total, written := accounted(f.trail(t)[base:])
		if total != 5000 {
			t.Errorf("5000 failures across five windows: %d accounted for (in %d records)", total, written)
		}
		if written > 10 {
			t.Errorf("%d records for five windows of one kind of failure, want at most 10", written)
		}
	})

	t.Run("with names too long for a record", func(t *testing.T) {
		t.Parallel()
		f := newIAMFixture(t)
		alice := f.as(t, f.user(t, "alice", iam.RoleReadOnly))
		base := len(f.trail(t))
		long := strings.Repeat("k", 1024)
		_ = f.authz.Authorize(t.Context(), iam.Request{Principal: alice, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "b", Key: long}})
		_ = f.authz.Authorize(t.Context(), iam.Request{Principal: alice, Action: iam.ActionDeleteObject, Resource: iam.Resource{Bucket: "b", Key: long + "x"}})
		if _, err := f.iam.GetUser(t.Context(), alice, strings.Repeat("u", 5000)); !errors.Is(err, domain.ErrAccessDenied) {
			t.Fatal(err)
		}
		f.identities.AuthenticationFailed(t.Context(), strings.Repeat("A", 5000), session("203.0.113.6"), strings.Repeat("r", 5000))
		records := f.trail(t)[base:]
		if len(records) != 4 {
			t.Fatalf("%d records for four refusals with oversized names, want 4: none may be lost for being long", len(records))
		}
		for _, r := range records {
			if err := r.Validate(); err != nil {
				t.Errorf("a stored record does not validate: %v", err)
			}
		}
		// Cut, but still telling two long names apart.
		if !strings.Contains(records[0].Resource, "sha256:") || records[0].Resource == records[1].Resource {
			t.Errorf("long resources are not kept apart: %q", records[0].Resource[len(records[0].Resource)-80:])
		}
		if strings.Contains(f.logs.String(), "could not be written") {
			t.Error("a record was refused by the trail")
		}
	})
}

// TestTheAuthorizerUsesItsOwnClock: the time a condition is tested against
// is this node's, whatever a caller put into the request.
func TestTheAuthorizerUsesItsOwnClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*",
		"Condition":{"DateLessThan":{"aws:CurrentTime":"2026-10-07T13:00:00Z"}}}]}`
	if err := f.iam.PutPolicy(ctx, f.root, "until-one", []byte(policy), true); err != nil {
		t.Fatal(err)
	}
	if err := f.iam.PutRole(ctx, f.root, iam.Role{Name: "until-one", Policies: []string{"until-one"}}, true); err != nil {
		t.Fatal(err)
	}
	alice := f.as(t, f.user(t, "alice", "until-one"))
	request := iam.Request{Principal: alice, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "b", Key: "k"}}
	if err := f.authz.Authorize(ctx, request); err != nil {
		t.Fatalf("at noon: %v", err)
	}
	f.clock.Advance(2 * time.Hour)
	request.Now = testNow // the caller says it is still noon
	if err := f.authz.Authorize(ctx, request); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("at two, with a request that claims noon = %v, want %v", err, domain.ErrAccessDenied)
	}
}

// TestKeyPermissionsAreAboutTheOwner: who may make and revoke keys is decided
// per user whose keys they are. A helpdesk role for one user's keys is good
// for that user's and nobody else's.
func TestKeyPermissionsAreAboutTheOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
		"Action":["admin:CreateAPIKey","admin:DeleteAPIKey","admin:ListAPIKeys"],"Resource":"arn:cactusfs:admin:::user/alice"}]}`
	if err := f.iam.PutPolicy(ctx, f.root, "alice-keys", []byte(policy), true); err != nil {
		t.Fatal(err)
	}
	if err := f.iam.PutRole(ctx, f.root, iam.Role{Name: "alice-keys", Policies: []string{"alice-keys"}}, true); err != nil {
		t.Fatal(err)
	}
	aliceKey := f.user(t, "alice", iam.RoleReadOnly)
	bobKey := f.user(t, "bob", iam.RoleClusterAdmin)
	helpdesk := f.as(t, f.user(t, "helpdesk", "alice-keys"))

	made, _, err := f.iam.CreateAPIKey(ctx, helpdesk, app.APIKeyInput{Owner: "alice"})
	if err != nil {
		t.Fatalf("making a key for alice: %v", err)
	}
	if keys, err := f.iam.ListAPIKeys(ctx, helpdesk, "alice"); err != nil || len(keys) != 2 {
		t.Errorf("listing alice's keys = %d, %v", len(keys), err)
	}
	if err := f.iam.DeleteAPIKey(ctx, helpdesk, aliceKey.AccessKeyID); err != nil {
		t.Errorf("revoking a key of alice: %v", err)
	}
	// Bob is an administrator: a key of his would be a key to everything.
	if _, _, err := f.iam.CreateAPIKey(ctx, helpdesk, app.APIKeyInput{Owner: "bob"}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("making a key for bob = %v, want %v", err, domain.ErrAccessDenied)
	}
	if _, err := f.iam.ListAPIKeys(ctx, helpdesk, "bob"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("listing bob's keys = %v", err)
	}
	err = f.iam.DeleteAPIKey(ctx, helpdesk, bobKey.AccessKeyID)
	if !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("revoking a key of bob = %v, want %v", err, domain.ErrAccessDenied)
	}
	// The refusal does not say whose key it is, or that it exists.
	missing := f.iam.DeleteAPIKey(ctx, helpdesk, "AKIDTHATDOESNOTEXIST")
	if err == nil || missing == nil || err.Error() != missing.Error() || strings.Contains(err.Error(), "bob") {
		t.Errorf("refusals for a key of bob and for no key differ, or name the owner: %q vs %q", err, missing)
	}
	if _, _, err := f.identities.SecretFor(ctx, bobKey.AccessKeyID); err != nil {
		t.Errorf("bob's key after the attempts: %v", err)
	}
	// The key made for alice is alice's: read-only, not a helpdesk key.
	if err := f.authz.Authorize(ctx, iam.Request{Principal: f.as(t, made), Action: iam.ActionCreateKey, Resource: iam.Resource{Admin: "user/alice"}}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("the key made for alice can make keys: %v", err)
	}
}

// TestSessions: a console user signs in with a password and gets a session
// that ends by itself; requests that change something need the CSRF token;
// nothing tells an attacker which user names exist.
func TestSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	const password = "correct horse battery staple"
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadOnly}, Password: password}); err != nil {
		t.Fatal(err)
	}
	f.user(t, "keyonly", iam.RoleReadOnly) // no password: cannot sign in
	where := session("198.51.100.7")

	signed, err := f.sessions.SignIn(ctx, "alice", password, where)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if signed.User != "alice" || len(signed.Token) < 40 || len(signed.CSRFToken) < 40 || signed.Token == signed.CSRFToken ||
		!signed.ExpiresAt.Equal(testNow.Add(app.DefaultSessionLifetime)) {
		t.Fatalf("SignIn = %+v", signed)
	}
	// Neither token is stored.
	if f.store.Sessions() != 1 {
		t.Fatalf("%d sessions stored", f.store.Sessions())
	}
	if _, err := f.store.GetSession(ctx, signed.Token); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Error("the session is stored under its token, not under a hash of it")
	}
	another, err := f.sessions.SignIn(ctx, "alice", password, where)
	if err != nil || another.Token == signed.Token || another.CSRFToken == signed.CSRFToken {
		t.Errorf("a second sign-in reused a token: %v", err)
	}

	// Reading needs the session; changing needs the CSRF token as well.
	principal, err := f.sessions.Authenticate(ctx, signed.Token, "", false, where)
	if err != nil || principal.User != "alice" || principal.Root || principal.AccessKeyID != "" || len(principal.Policies) != 1 ||
		principal.Session != where {
		t.Fatalf("Authenticate = %+v, %v", principal, err)
	}
	if err := f.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "b", Key: "k"}}); err != nil {
		t.Errorf("a signed-in reader reading: %v", err)
	}
	if _, err := f.sessions.Authenticate(ctx, signed.Token, signed.CSRFToken, true, where); err != nil {
		t.Errorf("a change with the CSRF token: %v", err)
	}
	for name, csrf := range map[string]string{"none": "", "wrong": "nonsense", "another session's": another.CSRFToken, "the session token": signed.Token} {
		if _, err := f.sessions.Authenticate(ctx, signed.Token, csrf, true, where); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("a change with CSRF token %s = %v, want %v", name, err, domain.ErrAccessDenied)
		}
	}
	for name, token := range map[string]string{"none": "", "unknown": "nonsense", "the CSRF token": signed.CSRFToken, "oversized": strings.Repeat("t", 1000)} {
		if _, err := f.sessions.Authenticate(ctx, token, signed.CSRFToken, false, where); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("session token %s = %v, want %v", name, err, domain.ErrAccessDenied)
		}
	}

	// Refused sign-ins all look alike, and each costs one password check -
	// also when there is no such user or no password to check against.
	refused := map[string][2]string{
		"wrong password":          {"alice", "wrong password entirely"},
		"unknown user":            {"nobody", password},
		"user without a password": {"keyonly", password},
		"empty password":          {"alice", ""},
		"invalid user name":       {"not a name", password},
		"oversized password":      {"alice", strings.Repeat("p", 2000)},
	}
	var message string
	for name, attempt := range refused {
		before := f.hasher.Verifications()
		_, err := f.sessions.SignIn(ctx, attempt[0], attempt[1], where)
		if !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("sign-in with %s = %v, want %v", name, err, domain.ErrAccessDenied)
			continue
		}
		if message == "" {
			message = err.Error()
		}
		if err.Error() != message {
			t.Errorf("sign-in with %s is refused differently: %q vs %q", name, err.Error(), message)
		}
		if name != "oversized password" && f.hasher.Verifications() != before+1 {
			t.Errorf("sign-in with %s made %d password checks, want 1", name, f.hasher.Verifications()-before)
		}
	}
	if f.store.Sessions() != 2 {
		t.Errorf("%d sessions after refused sign-ins, want 2", f.store.Sessions())
	}

	// A disabled user is signed out everywhere and cannot sign in.
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadOnly}, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Authenticate(ctx, signed.Token, "", false, where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("session of a disabled user = %v", err)
	}
	if _, err := f.sessions.SignIn(ctx, "alice", password, where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("sign-in of a disabled user = %v", err)
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}}); err != nil {
		t.Fatal(err)
	}
	// Roles changed meanwhile: the session has the new ones at once.
	principal, err = f.sessions.Authenticate(ctx, signed.Token, "", false, where)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "b", Key: "k"}}); err != nil {
		t.Errorf("the session does not have the user's new role: %v", err)
	}

	// Sign-out ends that session and no other.
	if err := f.sessions.SignOut(ctx, signed.Token, where); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if err := f.sessions.SignOut(ctx, signed.Token, where); err != nil {
		t.Errorf("signing out twice = %v", err)
	}
	if _, err := f.sessions.Authenticate(ctx, signed.Token, "", false, where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a session after sign-out = %v", err)
	}
	if _, err := f.sessions.Authenticate(ctx, another.Token, "", false, where); err != nil {
		t.Errorf("the other session after the first signed out: %v", err)
	}

	// A session ends by itself, swept or not.
	f.clock.Advance(app.DefaultSessionLifetime)
	if _, err := f.sessions.Authenticate(ctx, another.Token, "", false, where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a session at the end of its lifetime = %v", err)
	}
	f.clock.Advance(time.Second)
	if removed, err := f.sessions.Sweep(ctx); err != nil || removed != 1 || f.store.Sessions() != 0 {
		t.Errorf("Sweep = %d, %v; %d sessions left", removed, err, f.store.Sessions())
	}
	// Deleting the user ends the user's sessions.
	again, err := f.sessions.SignIn(ctx, "alice", password, where)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.iam.DeleteUser(ctx, f.root, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Authenticate(ctx, again.Token, "", false, where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a session of a deleted user = %v", err)
	}

	// The trail has the sign-ins, the sign-out, the refusals - and no
	// password.
	var signIns, signOuts, failed, csrf int
	for _, r := range f.trail(t) {
		switch {
		case r.Action == "auth:SignIn" && r.Outcome == audit.Succeeded:
			signIns++
			if r.Actor != "user/alice" || r.SourceIP != "198.51.100.7" {
				t.Errorf("sign-in record = %+v", r)
			}
		case r.Action == "auth:SignOut":
			signOuts++
		case r.Action == "auth:SignIn" && r.Outcome == audit.Failed:
			failed++
		case r.Action == "auth:CSRF":
			csrf++
		}
		if strings.Contains(fmt.Sprint(r), password) {
			t.Fatal("a password is in the audit trail")
		}
	}
	if signIns != 3 || signOuts != 1 || failed == 0 || csrf == 0 {
		t.Errorf("trail: %d sign-ins, %d sign-outs, %d failed sign-ins, %d CSRF refusals", signIns, signOuts, failed, csrf)
	}
	if strings.Contains(f.logs.String(), password) {
		t.Error("a password is in the log")
	}
}

// TestOtherAdministrativeChangesAreAudited: buckets made and removed, and
// what a node does by itself when a deployment begins, are in the trail too.
func TestOtherAdministrativeChangesAreAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	buckets := app.NewBucketService(f.meta, f.authz, f.clock).WithAuditor(f.auditor)
	alice := f.as(t, f.user(t, "alice", iam.RoleBucketAdmin))
	base := len(f.trail(t))

	if err := buckets.Create(ctx, alice, "reports"); err != nil {
		t.Fatal(err)
	}
	if err := buckets.Create(ctx, alice, "reports"); !errors.Is(err, domain.ErrBucketAlreadyExists) {
		t.Fatal(err)
	}
	if err := buckets.Delete(ctx, alice, "reports"); err != nil {
		t.Fatal(err)
	}
	if err := buckets.Delete(ctx, alice, "reports"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Fatal(err)
	}
	records := f.trail(t)[base:]
	if len(records) != 2 {
		t.Fatalf("%d records for a bucket made and removed (and two attempts that changed nothing), want 2", len(records))
	}
	for i, action := range []string{"s3:CreateBucket", "s3:DeleteBucket"} {
		r := records[i]
		if r.Action != action || r.Actor != "user/alice" || r.Resource != "arn:aws:s3:::reports" || r.Outcome != audit.Succeeded || r.SourceIP != "10.0.0.9" {
			t.Errorf("record %d = %+v", i, r)
		}
	}

	// A new deployment's topology, and a drive replaced, are recorded as
	// the node's own doing.
	drives := newDrives("d0", "d1", "d2", "d3")
	opts := topologyOptions(true)
	opts.NodeID, opts.Auditor = "node-a", f.auditor
	if _, err := app.EnsureTopology(ctx, f.meta, attachedDrives(drives...), opts); err != nil {
		t.Fatal(err)
	}
	replaced := attachedDrives(drives[0], drives[1], porttest.NewMemShardStore("fresh"), drives[3])
	if _, err := app.EnsureTopology(ctx, f.meta, replaced, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := app.EnsureTopology(ctx, f.meta, replaced, opts); err != nil {
		t.Fatal(err)
	}
	system := f.trail(t)[base+2:]
	if len(system) != 2 || system[0].Action != "system:CreateTopology" || system[0].Actor != "system" || system[0].Details["drives"] != "4" ||
		system[1].Action != "system:ReplaceDrive" || system[1].Details["missing_drive"] != "d2" || system[1].Details["new_drive"] != "fresh" {
		t.Errorf("records of the topology = %+v", system)
	}
}

// TestKnowingAKeyIsNotUsingIt: a key counts as used when a request's
// signature verified with it, not when somebody merely names it.
func TestKnowingAKeyIsNotUsingIt(t *testing.T) {
	t.Parallel()
	f := newIAMFixture(t)
	creds := f.user(t, "alice", iam.RoleReadOnly)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.identities.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// What an attacker with the key's ID and no secret causes: the lookup.
	for range 20 {
		if _, _, err := f.identities.SecretFor(ctx, creds.AccessKeyID); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	identity, err := f.store.ResolveIdentity(ctx, creds.AccessKeyID)
	if err != nil || !identity.Key.LastUsedAt.IsZero() {
		t.Errorf("a key that only had its ID presented is marked as used at %v (%v)", identity.Key.LastUsedAt, err)
	}
	// The root key and console sessions are not API keys of users.
	f.identities.Authenticated(f.root)
	f.identities.Authenticated(iam.Principal{User: "alice"})
}

// TestBucketConfigurationChangesAreAudited: a policy, cross-origin rules and
// tags set on a bucket or taken off it are in the trail, and a policy with
// whether it opens anything to everyone.
func TestBucketConfigurationChangesAreAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	buckets := app.NewBucketService(f.meta, f.authz, f.clock).WithAuditor(f.auditor)
	alice := f.as(t, f.user(t, "alice", iam.RoleBucketAdmin))
	if err := buckets.Create(ctx, alice, "site"); err != nil {
		t.Fatal(err)
	}
	base := len(f.trail(t))
	open := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`)
	closed := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`)
	rules := &bucket.CORS{Rules: []bucket.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}}}
	steps := []struct {
		do     func() error
		action string
		detail [2]string
	}{
		{func() error { return buckets.SetPolicy(ctx, alice, "site", open) }, "s3:PutBucketPolicy", [2]string{"open_to_anyone", "true"}},
		{func() error { return buckets.SetPolicy(ctx, alice, "site", closed) }, "s3:PutBucketPolicy", [2]string{"open_to_anyone", "false"}},
		{func() error { return buckets.SetPolicy(ctx, alice, "site", nil) }, "s3:DeleteBucketPolicy", [2]string{"policy", "removed"}},
		{func() error { return buckets.SetCORS(ctx, alice, "site", rules) }, "s3:PutBucketCORS", [2]string{"rules", "1"}},
		{func() error { return buckets.SetCORS(ctx, alice, "site", nil) }, "s3:PutBucketCORS", [2]string{"cors", "removed"}},
		{func() error { return buckets.SetTags(ctx, alice, "site", map[string]string{"team": "web"}) }, "s3:PutBucketTagging", [2]string{"tags", "1"}},
		{func() error { return buckets.SetTags(ctx, alice, "site", nil) }, "s3:PutBucketTagging", [2]string{"tags", "0"}},
		{func() error {
			return buckets.SetLifecycle(ctx, alice, "site", &bucket.Lifecycle{Rules: []bucket.LifecycleRule{{Enabled: true, ExpirationDays: 1}}})
		}, "s3:PutLifecycleConfiguration", [2]string{"rules", "1"}},
		{func() error { return buckets.SetLifecycle(ctx, alice, "site", nil) }, "s3:PutLifecycleConfiguration", [2]string{"lifecycle", "removed"}},
	}
	for i, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// What changed nothing is not recorded as a change.
	if err := buckets.SetPolicy(ctx, alice, "site", []byte("not a policy")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatal(err)
	}
	records := f.trail(t)[base:]
	if len(records) != len(steps) {
		t.Fatalf("%d records for %d changes", len(records), len(steps))
	}
	for i, step := range steps {
		r := records[i]
		if r.Action != step.action || r.Actor != "user/alice" || r.Resource != "arn:aws:s3:::site" || r.Outcome != audit.Succeeded ||
			r.Details[step.detail[0]] != step.detail[1] {
			t.Errorf("record %d = %+v, want %s with %s=%s", i, r, step.action, step.detail[0], step.detail[1])
		}
	}
}
