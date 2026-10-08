package porttest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// IAM is everything the IAM contract exercises.
type IAM interface {
	ports.IAMStore
	ports.AuditLog
	ports.SessionStore
}

var contractTime = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func auditOf(action, resource string) audit.Record {
	return audit.Record{
		Time: contractTime, Actor: "root", Action: action, Resource: resource, Outcome: audit.Succeeded,
		SourceIP: "10.0.0.1", RequestID: "REQ", Details: map[string]string{"k": "v"},
	}
}

func contractPolicy(t *testing.T, document string) iam.Document {
	t.Helper()
	doc, err := iam.ParsePolicy([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// IAMContract is the behaviour every implementation of the IAM, audit and
// session stores must have. open returns a new, empty store.
func IAMContract(t *testing.T, open func(t *testing.T) IAM) {
	t.Helper()
	ctx := context.Background()
	readPhotos := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`
	denySecret := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::photos/secret/*"}]}`

	// setup makes: policies p-read and p-deny, role viewers = {p-read},
	// group team = {viewers}, user alice in team with role read-write.
	setup := func(t *testing.T, s IAM) {
		t.Helper()
		steps := []error{
			s.PutPolicy(ctx, iam.Policy{Name: "p-read", Document: contractPolicy(t, readPhotos), CreatedAt: contractTime}, true, auditOf("admin:PutPolicy", "policy/p-read")),
			s.PutPolicy(ctx, iam.Policy{Name: "p-deny", Document: contractPolicy(t, denySecret), CreatedAt: contractTime}, true, auditOf("admin:PutPolicy", "policy/p-deny")),
			s.PutRole(ctx, iam.Role{Name: "viewers", Policies: []string{"p-read"}, CreatedAt: contractTime}, true, auditOf("admin:PutRole", "role/viewers")),
			s.PutGroup(ctx, iam.Group{Name: "team", Roles: []string{"viewers"}, CreatedAt: contractTime}, true, auditOf("admin:PutGroup", "group/team")),
			s.PutUser(ctx, iam.User{Name: "alice", Groups: []string{"team"}, Roles: []string{iam.RoleReadWrite}, PasswordHash: "hash", CreatedAt: contractTime}, true, auditOf("admin:PutUser", "user/alice")),
		}
		for i, err := range steps {
			if err != nil {
				t.Fatalf("setup step %d: %v", i, err)
			}
		}
	}
	key := func(id, owner string) iam.APIKey {
		return iam.APIKey{
			AccessKeyID: id, Owner: owner, Description: "for tests", CreatedAt: contractTime,
			Secret: iam.SealedSecret{KMSKeyID: "test:1", WrappedKey: []byte("wrapped"), Nonce: []byte("nonce"), Ciphertext: []byte("sealed")},
		}
	}

	t.Run("entities are stored, replaced and listed", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		setup(t, s)

		alice, err := s.GetUser(ctx, "alice")
		if err != nil || alice.Name != "alice" || !reflect.DeepEqual(alice.Groups, []string{"team"}) || alice.PasswordHash != "hash" ||
			!alice.CreatedAt.Equal(contractTime) {
			t.Fatalf("GetUser = %+v, %v", alice, err)
		}
		alice.Disabled, alice.Roles = true, nil
		if err := s.PutUser(ctx, alice, false, auditOf("admin:PutUser", "user/alice")); err != nil {
			t.Fatalf("replace user: %v", err)
		}
		if got, _ := s.GetUser(ctx, "alice"); !got.Disabled || len(got.Roles) != 0 {
			t.Errorf("after replace: %+v", got)
		}
		if err := s.PutUser(ctx, iam.User{Name: "alice"}, true, audit.Record{}); !errors.Is(err, domain.ErrEntityExists) {
			t.Errorf("create of an existing user = %v, want %v", err, domain.ErrEntityExists)
		}
		if err := s.PutUser(ctx, iam.User{Name: "nobody"}, false, audit.Record{}); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("replace of a missing user = %v, want %v", err, domain.ErrEntityNotFound)
		}
		if err := s.PutUser(ctx, iam.User{Name: "bob", CreatedAt: contractTime}, true, audit.Record{}); err != nil {
			t.Fatal(err)
		}
		users, err := s.ListUsers(ctx)
		if err != nil || len(users) != 2 || users[0].Name != "alice" || users[1].Name != "bob" {
			t.Errorf("ListUsers = %+v, %v", users, err)
		}

		group, err := s.GetGroup(ctx, "team")
		if err != nil || !reflect.DeepEqual(group.Roles, []string{"viewers"}) {
			t.Errorf("GetGroup = %+v, %v", group, err)
		}
		if groups, err := s.ListGroups(ctx); err != nil || len(groups) != 1 {
			t.Errorf("ListGroups = %+v, %v", groups, err)
		}
		policy, err := s.GetPolicy(ctx, "p-read")
		if err != nil || !reflect.DeepEqual(policy.Document, contractPolicy(t, readPhotos)) {
			t.Errorf("GetPolicy = %+v, %v", policy, err)
		}
		for name, get := range map[string]func() error{
			"user":   func() error { _, err := s.GetUser(ctx, "x"); return err },
			"group":  func() error { _, err := s.GetGroup(ctx, "x"); return err },
			"role":   func() error { _, err := s.GetRole(ctx, "x"); return err },
			"policy": func() error { _, err := s.GetPolicy(ctx, "x"); return err },
		} {
			if err := get(); !errors.Is(err, domain.ErrEntityNotFound) {
				t.Errorf("Get of a missing %s = %v, want %v", name, err, domain.ErrEntityNotFound)
			}
		}
		for name, err := range map[string]error{
			"user":   s.PutUser(ctx, iam.User{Name: "bad name"}, true, audit.Record{}),
			"group":  s.PutGroup(ctx, iam.Group{Name: ""}, true, audit.Record{}),
			"role":   s.PutRole(ctx, iam.Role{Name: "a/b"}, true, audit.Record{}),
			"policy": s.PutPolicy(ctx, iam.Policy{Name: "p"}, true, audit.Record{}),
		} {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Put of an invalid %s = %v, want %v", name, err, domain.ErrInvalidArgument)
			}
		}
	})

	t.Run("built-in roles and policies are there and cannot be touched", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		roles, err := s.ListRoles(ctx)
		if err != nil || len(roles) != 5 {
			t.Fatalf("ListRoles on an empty store = %d roles, %v; want the five built-in ones", len(roles), err)
		}
		policies, err := s.ListPolicies(ctx)
		if err != nil || len(policies) != 5 || !policies[0].BuiltIn {
			t.Fatalf("ListPolicies on an empty store = %d, %v", len(policies), err)
		}
		role, err := s.GetRole(ctx, iam.RoleAuditor)
		if err != nil || !role.BuiltIn {
			t.Errorf("GetRole(auditor) = %+v, %v", role, err)
		}
		if policy, err := s.GetPolicy(ctx, iam.RoleReadOnly); err != nil || !policy.BuiltIn {
			t.Errorf("GetPolicy(read-only) = %+v, %v", policy, err)
		}
		doc := contractPolicy(t, readPhotos)
		for name, err := range map[string]error{
			"put role":      s.PutRole(ctx, iam.Role{Name: iam.RoleClusterAdmin}, true, audit.Record{}),
			"replace role":  s.PutRole(ctx, iam.Role{Name: iam.RoleReadOnly}, false, audit.Record{}),
			"delete role":   s.DeleteRole(ctx, iam.RoleAuditor, audit.Record{}),
			"put policy":    s.PutPolicy(ctx, iam.Policy{Name: iam.RoleReadWrite, Document: doc}, true, audit.Record{}),
			"delete policy": s.DeletePolicy(ctx, iam.RoleBucketAdmin, audit.Record{}),
		} {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s of a built-in = %v, want %v", name, err, domain.ErrInvalidArgument)
			}
		}
		// A user and a group can hold them.
		if err := s.PutGroup(ctx, iam.Group{Name: "admins", Roles: []string{iam.RoleClusterAdmin}}, true, audit.Record{}); err != nil {
			t.Errorf("group with a built-in role: %v", err)
		}
		if err := s.PutUser(ctx, iam.User{Name: "carol", Roles: []string{iam.RoleAuditor}}, true, audit.Record{}); err != nil {
			t.Errorf("user with a built-in role: %v", err)
		}
	})

	t.Run("references stay whole", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		setup(t, s)
		dangling := map[string]error{
			"user in a missing group":     s.PutUser(ctx, iam.User{Name: "bob", Groups: []string{"nope"}}, true, audit.Record{}),
			"user with a missing role":    s.PutUser(ctx, iam.User{Name: "bob", Roles: []string{"nope"}}, true, audit.Record{}),
			"group with a missing role":   s.PutGroup(ctx, iam.Group{Name: "g", Roles: []string{"nope"}}, true, audit.Record{}),
			"role with a missing policy":  s.PutRole(ctx, iam.Role{Name: "r", Policies: []string{"nope"}}, true, audit.Record{}),
			"key of a missing user":       s.CreateAPIKey(ctx, key("AKIDNOBODY", "nobody"), audit.Record{}),
			"session of a missing user":   s.PutSession(ctx, ports.Session{TokenHash: "h", User: "nobody"}, audit.Record{}),
			"delete of a missing user":    s.DeleteUser(ctx, "nobody", audit.Record{}),
			"delete of a missing group":   s.DeleteGroup(ctx, "nope", audit.Record{}),
			"delete of a missing role":    s.DeleteRole(ctx, "nope", audit.Record{}),
			"delete of a missing policy":  s.DeletePolicy(ctx, "nope", audit.Record{}),
			"delete of a missing API key": s.DeleteAPIKey(ctx, "AKIDNOPE", audit.Record{}),
		}
		for name, err := range dangling {
			if !errors.Is(err, domain.ErrEntityNotFound) {
				t.Errorf("%s = %v, want %v", name, err, domain.ErrEntityNotFound)
			}
		}
		inUse := map[string]error{
			"policy in a role":   s.DeletePolicy(ctx, "p-read", audit.Record{}),
			"role of a group":    s.DeleteRole(ctx, "viewers", audit.Record{}),
			"group with members": s.DeleteGroup(ctx, "team", audit.Record{}),
		}
		for name, err := range inUse {
			if !errors.Is(err, domain.ErrEntityInUse) {
				t.Errorf("delete of a %s = %v, want %v", name, err, domain.ErrEntityInUse)
			}
		}
		if err := s.PutRole(ctx, iam.Role{Name: "direct"}, true, audit.Record{}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutUser(ctx, iam.User{Name: "bob", Roles: []string{"direct"}}, true, audit.Record{}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRole(ctx, "direct", audit.Record{}); !errors.Is(err, domain.ErrEntityInUse) {
			t.Errorf("delete of a role a user holds = %v, want %v", err, domain.ErrEntityInUse)
		}
		// Taken apart from the outside in, everything can go.
		for i, step := range []func() error{
			func() error { return s.DeleteUser(ctx, "alice", audit.Record{}) },
			func() error { return s.DeleteUser(ctx, "bob", audit.Record{}) },
			func() error { return s.DeleteGroup(ctx, "team", audit.Record{}) },
			func() error { return s.DeleteRole(ctx, "viewers", audit.Record{}) },
			func() error { return s.DeleteRole(ctx, "direct", audit.Record{}) },
			func() error { return s.DeletePolicy(ctx, "p-read", audit.Record{}) },
			func() error { return s.DeletePolicy(ctx, "p-deny", audit.Record{}) },
		} {
			if err := step(); err != nil {
				t.Errorf("teardown step %d: %v", i, err)
			}
		}
	})

	t.Run("an identity is resolved with every policy that applies", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		setup(t, s)
		inline := contractPolicy(t, denySecret)
		k := key("AKIDALICE0000000001", "alice")
		k.InlinePolicy, k.ExpiresAt = &inline, contractTime.Add(time.Hour)
		if err := s.CreateAPIKey(ctx, k, auditOf("admin:CreateAPIKey", "user/alice")); err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
		if err := s.CreateAPIKey(ctx, k, audit.Record{}); !errors.Is(err, domain.ErrEntityExists) {
			t.Errorf("a second key with the same ID = %v, want %v", err, domain.ErrEntityExists)
		}

		identity, err := s.ResolveIdentity(ctx, k.AccessKeyID)
		if err != nil {
			t.Fatalf("ResolveIdentity: %v", err)
		}
		if identity.User.Name != "alice" || identity.Key.Owner != "alice" || !identity.Key.ExpiresAt.Equal(k.ExpiresAt) ||
			!reflect.DeepEqual(identity.Key.Secret, k.Secret) || identity.Key.InlinePolicy == nil ||
			!reflect.DeepEqual(*identity.Key.InlinePolicy, inline) || identity.Key.Description != "for tests" {
			t.Errorf("resolved key = %+v", identity.Key)
		}
		// read-write directly, p-read through the group's role.
		want := []iam.Document{contractPolicy(t, readPhotos), iam.BuiltInPolicies()[iam.RoleReadWrite]}
		if len(identity.Policies) != 2 || !containsDocs(identity.Policies, want) {
			t.Errorf("resolved policies = %+v", identity.Policies)
		}
		user, err := s.ResolveUser(ctx, "alice")
		if err != nil || !containsDocs(user.Policies, want) || user.Key.AccessKeyID != "" {
			t.Errorf("ResolveUser = %+v, %v", user, err)
		}
		if _, err := s.ResolveUser(ctx, "nobody"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("ResolveUser of nobody = %v", err)
		}
		if _, err := s.ResolveIdentity(ctx, "AKIDUNKNOWN"); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("ResolveIdentity of an unknown key = %v, want %v", err, domain.ErrCredentialNotFound)
		}

		// A change of policy is seen by the very next resolution.
		if err := s.PutRole(ctx, iam.Role{Name: "viewers", Policies: []string{"p-read", "p-deny"}}, false, audit.Record{}); err != nil {
			t.Fatal(err)
		}
		if identity, _ := s.ResolveIdentity(ctx, k.AccessKeyID); len(identity.Policies) != 3 {
			t.Errorf("after the role gained a policy: %d documents, want 3", len(identity.Policies))
		}

		// Last use: recorded, never moved backwards, silent for a key that
		// is gone.
		used := contractTime.Add(10 * time.Minute)
		if err := s.TouchAPIKey(ctx, k.AccessKeyID, used); err != nil {
			t.Fatal(err)
		}
		if err := s.TouchAPIKey(ctx, k.AccessKeyID, used.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if identity, _ := s.ResolveIdentity(ctx, k.AccessKeyID); !identity.Key.LastUsedAt.Equal(used) {
			t.Errorf("LastUsedAt = %v, want %v", identity.Key.LastUsedAt, used)
		}
		if err := s.TouchAPIKey(ctx, "AKIDGONE", used); err != nil {
			t.Errorf("TouchAPIKey of a missing key = %v", err)
		}

		if err := s.CreateAPIKey(ctx, key("AKIDALICE0000000002", "alice"), audit.Record{}); err != nil {
			t.Fatal(err)
		}
		if keys, err := s.ListAPIKeys(ctx, "alice"); err != nil || len(keys) != 2 {
			t.Errorf("ListAPIKeys(alice) = %d, %v", len(keys), err)
		}
		if keys, err := s.ListAPIKeys(ctx, "bob"); err != nil || len(keys) != 0 {
			t.Errorf("ListAPIKeys(bob) = %d, %v", len(keys), err)
		}

		// Revocation is immediate.
		if err := s.DeleteAPIKey(ctx, k.AccessKeyID, auditOf("admin:DeleteAPIKey", "user/alice")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveIdentity(ctx, k.AccessKeyID); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("a revoked key still resolves: %v", err)
		}
		// Removing a user removes the user's keys and sessions.
		if err := s.PutSession(ctx, ports.Session{TokenHash: "h1", User: "alice", ExpiresAt: contractTime.Add(time.Hour)}, audit.Record{}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteUser(ctx, "alice", auditOf("admin:DeleteUser", "user/alice")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveIdentity(ctx, "AKIDALICE0000000002"); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("a key outlived its owner: %v", err)
		}
		if _, err := s.GetSession(ctx, "h1"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("a session outlived its user: %v", err)
		}
	})

	t.Run("every change is audited with the change, and the trail is a chain", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		setup(t, s) // five audited changes
		records, err := s.ListAudit(ctx, 0, 0)
		if err != nil || len(records) != 5 {
			t.Fatalf("ListAudit = %d records, %v; want 5", len(records), err)
		}
		if err := audit.Verify(audit.Record{}, records); err != nil {
			t.Fatalf("the trail does not verify: %v", err)
		}
		first := records[0]
		if first.Sequence != 1 || first.Actor != "root" || first.Action != "admin:PutPolicy" || first.Outcome != audit.Succeeded ||
			first.SourceIP != "10.0.0.1" || first.RequestID != "REQ" || first.Details["k"] != "v" || !first.Time.Equal(contractTime) {
			t.Errorf("first record = %+v", first)
		}

		// A change that is refused leaves no record of having happened.
		if err := s.PutUser(ctx, iam.User{Name: "alice"}, true, auditOf("admin:PutUser", "user/alice")); !errors.Is(err, domain.ErrEntityExists) {
			t.Fatal(err)
		}
		if err := s.DeleteRole(ctx, "viewers", auditOf("admin:DeleteRole", "role/viewers")); !errors.Is(err, domain.ErrEntityInUse) {
			t.Fatal(err)
		}
		// A change without a record is refused when its record is invalid,
		// and then did not happen.
		bad := audit.Record{Actor: "root", Action: "x", Outcome: "perhaps"}
		if err := s.PutUser(ctx, iam.User{Name: "dave"}, true, bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a change with an invalid audit record = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if _, err := s.GetUser(ctx, "dave"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Error("a change whose audit record was refused happened anyway")
		}
		if records, _ := s.ListAudit(ctx, 0, 0); len(records) != 5 {
			t.Fatalf("%d audit records after refused changes, want 5", len(records))
		}

		// Records appended on their own join the same chain.
		denied := audit.Record{Time: contractTime, Actor: "user/alice", Action: "s3:GetObject", Resource: "arn:aws:s3:::b/k", Outcome: audit.Denied}
		for range 3 {
			if err := s.AppendAudit(ctx, denied); err != nil {
				t.Fatalf("AppendAudit: %v", err)
			}
		}
		if err := s.AppendAudit(ctx, audit.Record{Action: "x", Outcome: audit.Failed}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("AppendAudit without an actor = %v", err)
		}
		// Whatever numbers a caller puts in, the store decides them.
		forged := denied
		forged.Sequence, forged.Hash, forged.PreviousHash = 99, "f00d", "beef"
		if err := s.AppendAudit(ctx, forged); err != nil {
			t.Fatal(err)
		}
		all, err := s.ListAudit(ctx, 0, 0)
		if err != nil || len(all) != 9 || all[8].Sequence != 9 {
			t.Fatalf("ListAudit = %d records, %v", len(all), err)
		}
		if err := audit.Verify(audit.Record{}, all); err != nil {
			t.Errorf("the trail does not verify after appends: %v", err)
		}
		// Pages.
		page, err := s.ListAudit(ctx, 3, 4)
		if err != nil || len(page) != 4 || page[0].Sequence != 4 || page[3].Sequence != 7 {
			t.Errorf("ListAudit(after 3, limit 4) = %+v, %v", page, err)
		}
		if err := audit.Verify(all[2], page); err != nil {
			t.Errorf("a page does not continue the chain: %v", err)
		}
		if tail, err := s.ListAudit(ctx, 9, 10); err != nil || len(tail) != 0 {
			t.Errorf("ListAudit past the end = %d, %v", len(tail), err)
		}
		if tail, err := s.ListAudit(ctx, ^uint64(0), 10); err != nil || len(tail) != 0 {
			t.Errorf("ListAudit after the largest number = %d, %v", len(tail), err)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		setup(t, s)
		session := ports.Session{
			TokenHash: "token-hash-1", User: "alice", CSRFHash: "csrf-hash-1",
			CreatedAt: contractTime, ExpiresAt: contractTime.Add(30 * time.Minute),
		}
		if err := s.PutSession(ctx, session, auditOf("auth:SignIn", "user/alice")); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
		got, err := s.GetSession(ctx, "token-hash-1")
		if err != nil || got.User != "alice" || got.CSRFHash != "csrf-hash-1" || !got.ExpiresAt.Equal(session.ExpiresAt) {
			t.Errorf("GetSession = %+v, %v", got, err)
		}
		if _, err := s.GetSession(ctx, "other"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("GetSession of an unknown hash = %v", err)
		}
		for i := range 3 {
			old := ports.Session{TokenHash: fmt.Sprintf("old-%d", i), User: "alice", ExpiresAt: contractTime.Add(-time.Minute)}
			if err := s.PutSession(ctx, old, audit.Record{}); err != nil {
				t.Fatal(err)
			}
		}
		removed, err := s.DeleteExpiredSessions(ctx, contractTime)
		if err != nil || removed != 3 {
			t.Errorf("DeleteExpiredSessions = %d, %v; want 3", removed, err)
		}
		if removed, err := s.DeleteExpiredSessions(ctx, contractTime); err != nil || removed != 0 {
			t.Errorf("second DeleteExpiredSessions = %d, %v", removed, err)
		}
		if _, err := s.GetSession(ctx, "token-hash-1"); err != nil {
			t.Errorf("a live session was swept: %v", err)
		}
		if err := s.DeleteSession(ctx, "token-hash-1", auditOf("auth:SignOut", "user/alice")); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSession(ctx, "token-hash-1", audit.Record{}); err != nil {
			t.Errorf("ending a session that is gone = %v", err)
		}
		if _, err := s.GetSession(ctx, "token-hash-1"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("a session survived sign-out: %v", err)
		}
	})
}

// containsDocs reports whether got holds every document of want.
func containsDocs(got, want []iam.Document) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || reflect.DeepEqual(g, w)
		}
		if !found {
			return false
		}
	}
	return true
}

// IdempotencyContract is the behaviour every implementation of the
// idempotency store must have. open returns a new, empty store.
func IdempotencyContract(t *testing.T, open func(t *testing.T) ports.IdempotencyStore) {
	t.Helper()
	ctx := context.Background()
	s := open(t)
	sealed := iam.SealedSecret{KMSKeyID: "test:1", WrappedKey: []byte("wrapped"), Nonce: []byte("nonce"), Ciphertext: []byte("sealed answer")}
	begun := ports.IdempotencyRecord{ID: "id-1", Fingerprint: "fp-1", CreatedAt: contractTime}

	if _, err := s.GetIdempotent(ctx, "id-1"); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Fatalf("GetIdempotent of nothing = %v", err)
	}
	if err := s.BeginIdempotent(ctx, begun); err != nil {
		t.Fatalf("BeginIdempotent: %v", err)
	}
	// Only one request begins a key.
	if err := s.BeginIdempotent(ctx, ports.IdempotencyRecord{ID: "id-1", Fingerprint: "fp-2", CreatedAt: contractTime.Add(time.Second)}); !errors.Is(err, domain.ErrEntityExists) {
		t.Fatalf("second BeginIdempotent = %v, want %v", err, domain.ErrEntityExists)
	}
	got, err := s.GetIdempotent(ctx, "id-1")
	if err != nil || got.Done || got.Fingerprint != "fp-1" || !got.CreatedAt.Equal(contractTime) {
		t.Fatalf("a record that has begun = %+v, %v", got, err)
	}

	// An answer for a record somebody else began later is not taken.
	stranger := begun
	stranger.CreatedAt = contractTime.Add(time.Hour)
	stranger.Status, stranger.Body = 500, sealed
	if err := s.CompleteIdempotent(ctx, stranger); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetIdempotent(ctx, "id-1"); got.Done {
		t.Fatal("a record was completed by a request that did not begin it")
	}

	done := begun
	done.Status, done.ContentType, done.Body = 201, "application/json", sealed
	if err := s.CompleteIdempotent(ctx, done); err != nil {
		t.Fatalf("CompleteIdempotent: %v", err)
	}
	got, err = s.GetIdempotent(ctx, "id-1")
	if err != nil || !got.Done || got.Status != 201 || got.ContentType != "application/json" ||
		string(got.Body.Ciphertext) != "sealed answer" || got.Body.KMSKeyID != "test:1" || string(got.Body.WrappedKey) != "wrapped" {
		t.Fatalf("a completed record = %+v, %v", got, err)
	}
	// The first answer stands.
	again := done
	again.Status = 409
	if err := s.CompleteIdempotent(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetIdempotent(ctx, "id-1"); got.Status != 201 {
		t.Fatalf("a completed record was completed again: status %d", got.Status)
	}
	// Completing what is not there makes nothing.
	if err := s.CompleteIdempotent(ctx, ports.IdempotencyRecord{ID: "id-9", Fingerprint: "fp", CreatedAt: contractTime, Status: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdempotent(ctx, "id-9"); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Fatalf("completing an unknown record made one: %v", err)
	}

	// Old records are forgotten, newer ones kept.
	if err := s.BeginIdempotent(ctx, ports.IdempotencyRecord{ID: "id-2", Fingerprint: "fp", CreatedAt: contractTime.Add(48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteIdempotentBefore(ctx, contractTime.Add(24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("DeleteIdempotentBefore = %d, %v; want 1", n, err)
	}
	if n, err := s.DeleteIdempotentBefore(ctx, contractTime.Add(24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("second DeleteIdempotentBefore = %d, %v; want 0", n, err)
	}
	if _, err := s.GetIdempotent(ctx, "id-1"); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Fatalf("an old record after the sweep: %v", err)
	}
	// A delete meant for a record begun at another time removes nothing:
	// that record is another request's.
	if err := s.DeleteIdempotent(ctx, "id-2", contractTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdempotent(ctx, "id-2"); err != nil {
		t.Fatalf("a delete meant for an earlier record removed a later one: %v", err)
	}
	if err := s.DeleteIdempotent(ctx, "id-2", contractTime.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteIdempotent(ctx, "id-2", contractTime.Add(48*time.Hour)); err != nil {
		t.Fatalf("deleting a record twice: %v", err)
	}
	if _, err := s.GetIdempotent(ctx, "id-2"); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Fatalf("a deleted record: %v", err)
	}
}
