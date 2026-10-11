package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// IAMService holds the use cases that manage users, groups, roles, policies
// and API keys, and that read the audit trail. Every one of them passes the
// authorizer first, and every change is written to the audit trail together
// with the change itself.
type IAMService struct {
	store   ports.IAMStore
	trail   ports.AuditLog
	authz   ports.Authorizer
	auditor *Auditor
	kms     ports.KMS
	hasher  ports.PasswordHasher
	clock   ports.Clock

	entropyMu sync.Mutex
	entropy   io.Reader
}

// IAMServiceConfig lists what an IAMService depends on.
type IAMServiceConfig struct {
	Store ports.IAMStore
	Trail ports.AuditLog
	Authz ports.Authorizer
	// Auditor builds the records of changes.
	Auditor *Auditor
	KMS     ports.KMS
	Hasher  ports.PasswordHasher
	Clock   ports.Clock
	// Entropy must be a cryptographically secure random source.
	Entropy io.Reader
}

// NewIAMService returns an IAMService.
func NewIAMService(cfg IAMServiceConfig) *IAMService {
	return &IAMService{
		store: cfg.Store, trail: cfg.Trail, authz: cfg.Authz, auditor: cfg.Auditor, kms: cfg.KMS,
		hasher: cfg.Hasher, clock: cfg.Clock, entropy: cfg.Entropy,
	}
}

// allow asks the authorizer and, when it agrees, returns the audit record of
// the action as carried out.
func (s *IAMService) allow(ctx context.Context, p iam.Principal, action iam.Action, kind, name string, details map[string]string) (audit.Record, error) {
	resource := iam.Resource{Admin: kind}
	if name != "" {
		resource.Admin = kind + "/" + name
	}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: p, Action: action, Resource: resource}); err != nil {
		return audit.Record{}, err
	}
	return s.auditor.Record(p, action, resource, audit.Succeeded, details), nil
}

// UserInput is what can be said about a user when making or changing one.
type UserInput struct {
	Name   string
	Groups []string
	Roles  []string
	// Disabled suspends the user.
	Disabled bool
	// Password, when not empty, sets the console password. It is hashed
	// before it is stored and never logged.
	Password string
}

// MinPasswordLength is the shortest console password accepted.
const MinPasswordLength = 12

// maxPasswordLength bounds the work of hashing what a client sends.
const maxPasswordLength = 1024

func (s *IAMService) hash(password string) (string, error) {
	if len(password) < MinPasswordLength || len(password) > maxPasswordLength {
		return "", fmt.Errorf("%w: a password must be %d to %d characters", domain.ErrInvalidArgument, MinPasswordLength, maxPasswordLength)
	}
	return s.hasher.Hash(password)
}

// The console user every new deployment begins with (ADR-0014, decided by
// Albie on 2026-10-11): the name and the password are the same everywhere
// and are public. Whoever runs the deployment is expected to change the
// password, or delete the user, before anybody else can reach the console.
const (
	DefaultAdminName     = "admin"
	DefaultAdminPassword = "admin"
)

// EnsureDefaultAdmin gives a new deployment its first console user:
// DefaultAdminName with DefaultAdminPassword and the role cluster-admin. It
// is called once, when the deployment is bootstrapped; a user of that name
// who is already there is left as it is, and so is a deployment whose
// operator has deleted the user. It reports whether the deployment now has
// the user with the default password because of this call.
//
// The password is shorter than MinPasswordLength, which holds for every
// password that is set afterwards, this user's own included.
func (s *IAMService) EnsureDefaultAdmin(ctx context.Context) (bool, error) {
	hash, err := s.hasher.Hash(DefaultAdminPassword)
	if err != nil {
		return false, fmt.Errorf("iam.EnsureDefaultAdmin: %w", err)
	}
	user := iam.User{
		Name: DefaultAdminName, Roles: []string{iam.RoleClusterAdmin}, PasswordHash: hash, CreatedAt: s.clock.Now().UTC(),
	}
	record := s.auditor.System("system:BootstrapAdmin", "user/"+DefaultAdminName, map[string]string{
		"roles": iam.RoleClusterAdmin, "password": "default",
	})
	err = s.store.PutUser(ctx, user, true, record)
	switch {
	case errors.Is(err, domain.ErrEntityExists):
		return false, nil
	case errors.Is(err, domain.ErrOutcomeUnknown):
		// The user may have been made. If it is there with the default
		// password, the caller must hear of it: a deployment must not
		// have that user without the warning that goes with it.
		stored, readErr := s.store.GetUser(ctx, DefaultAdminName)
		if readErr != nil {
			return false, fmt.Errorf("iam.EnsureDefaultAdmin: %w", err)
		}
		if ok, verifyErr := s.hasher.Verify(stored.PasswordHash, DefaultAdminPassword); verifyErr == nil && ok {
			return true, nil
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("iam.EnsureDefaultAdmin: %w", err)
	}
	return true, nil
}

// CreateUser adds a user.
func (s *IAMService) CreateUser(ctx context.Context, p iam.Principal, in UserInput) (iam.User, error) {
	record, err := s.allow(ctx, p, iam.ActionPutUser, "user", in.Name, userDetails("create", in))
	if err != nil {
		return iam.User{}, fmt.Errorf("iam.CreateUser: %w", err)
	}
	user := iam.User{Name: in.Name, Groups: in.Groups, Roles: in.Roles, Disabled: in.Disabled, CreatedAt: s.clock.Now().UTC()}
	if in.Password != "" {
		if user.PasswordHash, err = s.hash(in.Password); err != nil {
			return iam.User{}, fmt.Errorf("iam.CreateUser: %w", err)
		}
	}
	if err := s.store.PutUser(ctx, user, true, record); err != nil {
		return iam.User{}, fmt.Errorf("iam.CreateUser: %w", err)
	}
	return redact(user), nil
}

// UpdateUser replaces a user's groups, roles and state, and the password
// when one is given.
func (s *IAMService) UpdateUser(ctx context.Context, p iam.Principal, in UserInput) (iam.User, error) {
	record, err := s.allow(ctx, p, iam.ActionPutUser, "user", in.Name, userDetails("update", in))
	if err != nil {
		return iam.User{}, fmt.Errorf("iam.UpdateUser: %w", err)
	}
	user, err := s.store.GetUser(ctx, in.Name)
	if err != nil {
		return iam.User{}, fmt.Errorf("iam.UpdateUser: %w", err)
	}
	user.Groups, user.Roles, user.Disabled = in.Groups, in.Roles, in.Disabled
	if in.Password != "" {
		if user.PasswordHash, err = s.hash(in.Password); err != nil {
			return iam.User{}, fmt.Errorf("iam.UpdateUser: %w", err)
		}
	}
	if err := s.store.PutUser(ctx, user, false, record); err != nil {
		return iam.User{}, fmt.Errorf("iam.UpdateUser: %w", err)
	}
	return redact(user), nil
}

// change names a put in the audit trail.
func change(create bool) string {
	if create {
		return "create"
	}
	return "update"
}

// userDetails says, in the audit record of a change to a user, what the
// user was given: the record of a grant must show the grant.
func userDetails(kind string, in UserInput) map[string]string {
	details := map[string]string{
		"change": kind, "roles": strings.Join(in.Roles, ","), "groups": strings.Join(in.Groups, ","),
		"disabled": strconv.FormatBool(in.Disabled),
	}
	if in.Password != "" {
		details["password"] = "changed"
	}
	return details
}

// redact removes what must not leave the use cases.
func redact(u iam.User) iam.User {
	if u.PasswordHash != "" {
		u.PasswordHash = "set"
	}
	return u
}

// DeleteUser removes a user with the user's API keys and sessions.
func (s *IAMService) DeleteUser(ctx context.Context, p iam.Principal, name string) error {
	record, err := s.allow(ctx, p, iam.ActionDeleteUser, "user", name, nil)
	if err == nil {
		err = s.store.DeleteUser(ctx, name, record)
	}
	if err != nil {
		return fmt.Errorf("iam.DeleteUser: %w", err)
	}
	return nil
}

// GetUser returns a user. The password hash is never returned: PasswordHash
// only says whether one is set.
func (s *IAMService) GetUser(ctx context.Context, p iam.Principal, name string) (iam.User, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetUser, "user", name, nil); err != nil {
		return iam.User{}, fmt.Errorf("iam.GetUser: %w", err)
	}
	user, err := s.store.GetUser(ctx, name)
	if err != nil {
		return iam.User{}, fmt.Errorf("iam.GetUser: %w", err)
	}
	return redact(user), nil
}

// ListUsers returns every user, by name.
func (s *IAMService) ListUsers(ctx context.Context, p iam.Principal) ([]iam.User, error) {
	if _, err := s.allow(ctx, p, iam.ActionListUsers, "user", "", nil); err != nil {
		return nil, fmt.Errorf("iam.ListUsers: %w", err)
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("iam.ListUsers: %w", err)
	}
	for i := range users {
		users[i] = redact(users[i])
	}
	return users, nil
}

// PutGroup adds a group, or replaces its roles.
func (s *IAMService) PutGroup(ctx context.Context, p iam.Principal, g iam.Group, create bool) error {
	record, err := s.allow(ctx, p, iam.ActionPutGroup, "group", g.Name,
		map[string]string{"change": change(create), "roles": strings.Join(g.Roles, ",")})
	if err != nil {
		return fmt.Errorf("iam.PutGroup: %w", err)
	}
	if create {
		g.CreatedAt = s.clock.Now().UTC()
	} else if existing, err := s.store.GetGroup(ctx, g.Name); err == nil {
		g.CreatedAt = existing.CreatedAt
	}
	if err := s.store.PutGroup(ctx, g, create, record); err != nil {
		return fmt.Errorf("iam.PutGroup: %w", err)
	}
	return nil
}

// DeleteGroup removes a group that has no members.
func (s *IAMService) DeleteGroup(ctx context.Context, p iam.Principal, name string) error {
	record, err := s.allow(ctx, p, iam.ActionDeleteGroup, "group", name, nil)
	if err == nil {
		err = s.store.DeleteGroup(ctx, name, record)
	}
	if err != nil {
		return fmt.Errorf("iam.DeleteGroup: %w", err)
	}
	return nil
}

// GetGroup returns a group.
func (s *IAMService) GetGroup(ctx context.Context, p iam.Principal, name string) (iam.Group, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetGroup, "group", name, nil); err != nil {
		return iam.Group{}, fmt.Errorf("iam.GetGroup: %w", err)
	}
	group, err := s.store.GetGroup(ctx, name)
	if err != nil {
		return iam.Group{}, fmt.Errorf("iam.GetGroup: %w", err)
	}
	return group, nil
}

// ListGroups returns every group, by name.
func (s *IAMService) ListGroups(ctx context.Context, p iam.Principal) ([]iam.Group, error) {
	if _, err := s.allow(ctx, p, iam.ActionListGroups, "group", "", nil); err != nil {
		return nil, fmt.Errorf("iam.ListGroups: %w", err)
	}
	groups, err := s.store.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("iam.ListGroups: %w", err)
	}
	return groups, nil
}

// PutRole adds a role, or replaces its policies. Built-in roles cannot be
// put.
func (s *IAMService) PutRole(ctx context.Context, p iam.Principal, r iam.Role, create bool) error {
	record, err := s.allow(ctx, p, iam.ActionPutRole, "role", r.Name,
		map[string]string{"change": change(create), "policies": strings.Join(r.Policies, ",")})
	if err != nil {
		return fmt.Errorf("iam.PutRole: %w", err)
	}
	if create {
		r.CreatedAt = s.clock.Now().UTC()
	} else if existing, err := s.store.GetRole(ctx, r.Name); err == nil {
		r.CreatedAt = existing.CreatedAt
	}
	if err := s.store.PutRole(ctx, r, create, record); err != nil {
		return fmt.Errorf("iam.PutRole: %w", err)
	}
	return nil
}

// DeleteRole removes a role nobody holds.
func (s *IAMService) DeleteRole(ctx context.Context, p iam.Principal, name string) error {
	record, err := s.allow(ctx, p, iam.ActionDeleteRole, "role", name, nil)
	if err == nil {
		err = s.store.DeleteRole(ctx, name, record)
	}
	if err != nil {
		return fmt.Errorf("iam.DeleteRole: %w", err)
	}
	return nil
}

// GetRole returns a role, built-in ones included.
func (s *IAMService) GetRole(ctx context.Context, p iam.Principal, name string) (iam.Role, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetRole, "role", name, nil); err != nil {
		return iam.Role{}, fmt.Errorf("iam.GetRole: %w", err)
	}
	role, err := s.store.GetRole(ctx, name)
	if err != nil {
		return iam.Role{}, fmt.Errorf("iam.GetRole: %w", err)
	}
	return role, nil
}

// ListRoles returns every role: the built-in ones first.
func (s *IAMService) ListRoles(ctx context.Context, p iam.Principal) ([]iam.Role, error) {
	if _, err := s.allow(ctx, p, iam.ActionListRoles, "role", "", nil); err != nil {
		return nil, fmt.Errorf("iam.ListRoles: %w", err)
	}
	roles, err := s.store.ListRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("iam.ListRoles: %w", err)
	}
	return roles, nil
}

// PutPolicy adds a policy, or replaces its document. document is the policy
// in the IAM policy language; it is parsed strictly.
func (s *IAMService) PutPolicy(ctx context.Context, p iam.Principal, name string, document []byte, create bool) error {
	// The record names the document by its hash: which policy text was put
	// can be proven later without the trail holding every version of it.
	digest := sha256.Sum256(document)
	record, err := s.allow(ctx, p, iam.ActionPutPolicy, "policy", name,
		map[string]string{"change": change(create), "document_sha256": hex.EncodeToString(digest[:])})
	if err != nil {
		return fmt.Errorf("iam.PutPolicy: %w", err)
	}
	doc, err := iam.ParsePolicy(document)
	if err != nil {
		return fmt.Errorf("iam.PutPolicy: %w", err)
	}
	policy := iam.Policy{Name: name, Document: doc, CreatedAt: s.clock.Now().UTC()}
	if !create {
		if existing, err := s.store.GetPolicy(ctx, name); err == nil {
			policy.CreatedAt = existing.CreatedAt
		}
	}
	if err := s.store.PutPolicy(ctx, policy, create, record); err != nil {
		return fmt.Errorf("iam.PutPolicy: %w", err)
	}
	return nil
}

// DeletePolicy removes a policy no role contains.
func (s *IAMService) DeletePolicy(ctx context.Context, p iam.Principal, name string) error {
	record, err := s.allow(ctx, p, iam.ActionDeletePolicy, "policy", name, nil)
	if err == nil {
		err = s.store.DeletePolicy(ctx, name, record)
	}
	if err != nil {
		return fmt.Errorf("iam.DeletePolicy: %w", err)
	}
	return nil
}

// GetPolicy returns a policy, built-in ones included.
func (s *IAMService) GetPolicy(ctx context.Context, p iam.Principal, name string) (iam.Policy, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetPolicy, "policy", name, nil); err != nil {
		return iam.Policy{}, fmt.Errorf("iam.GetPolicy: %w", err)
	}
	policy, err := s.store.GetPolicy(ctx, name)
	if err != nil {
		return iam.Policy{}, fmt.Errorf("iam.GetPolicy: %w", err)
	}
	return policy, nil
}

// ListPolicies returns every policy: the built-in ones first.
func (s *IAMService) ListPolicies(ctx context.Context, p iam.Principal) ([]iam.Policy, error) {
	if _, err := s.allow(ctx, p, iam.ActionListPolicies, "policy", "", nil); err != nil {
		return nil, fmt.Errorf("iam.ListPolicies: %w", err)
	}
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("iam.ListPolicies: %w", err)
	}
	return policies, nil
}

// APIKeyInput describes an API key to make.
type APIKeyInput struct {
	// Owner is the user the key acts as.
	Owner       string
	Description string
	// InlinePolicy, when not empty, is a policy document that narrows what
	// the key may do.
	InlinePolicy []byte
	// ExpiresAt, when not zero, is when the key stops working.
	ExpiresAt time.Time
}

// CreateAPIKey makes an API key for a user and returns its credentials. This
// is the only time the secret can be had: it is stored sealed through the
// KMS and is never shown, returned or logged again.
//
// The authorizer is asked whether the caller may make keys for that user
// (admin:CreateAPIKey on user/<owner>). Whatever the answer, the key can do
// no more than its owner.
func (s *IAMService) CreateAPIKey(ctx context.Context, p iam.Principal, in APIKeyInput) (Credentials, iam.APIKey, error) {
	// A key that is narrowed by a policy of its own makes no keys: the new
	// key would not carry the narrowing, and would be a way around it.
	if p.Boundary != nil {
		const reason = "an API key with a policy of its own cannot make keys"
		s.auditor.Denied(ctx, iam.Request{Principal: p, Action: iam.ActionCreateKey, Resource: iam.Resource{Admin: "user/" + in.Owner}}, reason)
		return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w: %s", domain.ErrAccessDenied, reason)
	}
	record, err := s.allow(ctx, p, iam.ActionCreateKey, "user", in.Owner, nil)
	if err != nil {
		return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w", err)
	}
	now := s.clock.Now().UTC()
	key := iam.APIKey{Owner: in.Owner, Description: in.Description, ExpiresAt: in.ExpiresAt, CreatedAt: now}
	if !in.ExpiresAt.IsZero() && !in.ExpiresAt.After(now) {
		return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w: the key would have expired already", domain.ErrInvalidArgument)
	}
	if len(in.InlinePolicy) > 0 {
		doc, err := iam.ParsePolicy(in.InlinePolicy)
		if err != nil {
			return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w", err)
		}
		key.InlinePolicy = &doc
	}

	s.entropyMu.Lock()
	id, err := iam.GenerateAccessKeyID(s.entropy)
	var secret string
	if err == nil {
		secret, err = iam.GenerateSecret(s.entropy)
	}
	s.entropyMu.Unlock()
	if err != nil {
		return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w", err)
	}
	key.AccessKeyID = id
	if key.Secret, err = s.seal(ctx, id, secret); err != nil {
		return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w", err)
	}
	record.Details = map[string]string{
		"access_key_id": id, "inline_policy": strconv.FormatBool(key.InlinePolicy != nil),
		"expires": strconv.FormatBool(!key.ExpiresAt.IsZero()),
	}
	if err := s.store.CreateAPIKey(ctx, key, record); err != nil {
		return Credentials{}, iam.APIKey{}, fmt.Errorf("iam.CreateAPIKey: %w", err)
	}
	key.Secret = iam.SealedSecret{}
	return Credentials{AccessKeyID: id, SecretKey: secret}, key, nil
}

// seal encrypts a secret under a fresh data key from the KMS, bound to the
// access key ID.
func (s *IAMService) seal(ctx context.Context, accessKeyID, secret string) (iam.SealedSecret, error) {
	aad := []byte(accessKeyID)
	key, err := s.kms.GenerateDataKey(ctx, aad)
	if err != nil {
		return iam.SealedSecret{}, err
	}
	defer clear(key.Plaintext)
	s.entropyMu.Lock()
	sealed, err := iam.Seal(key.Plaintext, s.entropy, []byte(secret), aad)
	s.entropyMu.Unlock()
	if err != nil {
		return iam.SealedSecret{}, err
	}
	sealed.KMSKeyID = s.kms.KeyID()
	sealed.WrappedKey = key.Wrapped
	return sealed, nil
}

// DeleteAPIKey revokes an API key. From the moment it returns, no node
// accepts the key.
func (s *IAMService) DeleteAPIKey(ctx context.Context, p iam.Principal, accessKeyID string) error {
	identity, err := s.store.ResolveIdentity(ctx, accessKeyID)
	owner := identity.Key.Owner
	if err != nil {
		// Whether the key exists is not told to somebody who may not ask:
		// the authorizer is asked about a key of nobody.
		owner = ""
	}
	record, authErr := s.allow(ctx, p, iam.ActionDeleteKey, "user", owner, map[string]string{"access_key_id": bound(accessKeyID)})
	if authErr != nil {
		// The refusal says nothing about the key: not whether it exists,
		// not whose it is.
		return fmt.Errorf("iam.DeleteAPIKey: %w", domain.ErrAccessDenied)
	}
	if err != nil {
		return fmt.Errorf("iam.DeleteAPIKey: %w: API key %s", domain.ErrEntityNotFound, bound(accessKeyID))
	}
	if err := s.store.DeleteAPIKey(ctx, accessKeyID, record); err != nil {
		return fmt.Errorf("iam.DeleteAPIKey: %w", err)
	}
	return nil
}

// ListAPIKeys returns the keys of a user. Secrets are not part of the
// answer in any form.
func (s *IAMService) ListAPIKeys(ctx context.Context, p iam.Principal, owner string) ([]iam.APIKey, error) {
	if _, err := s.allow(ctx, p, iam.ActionListKeys, "user", owner, nil); err != nil {
		return nil, fmt.Errorf("iam.ListAPIKeys: %w", err)
	}
	if owner == "" {
		return nil, fmt.Errorf("iam.ListAPIKeys: %w: name the user whose keys to list", domain.ErrInvalidArgument)
	}
	keys, err := s.store.ListAPIKeys(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("iam.ListAPIKeys: %w", err)
	}
	for i := range keys {
		keys[i].Secret = iam.SealedSecret{}
	}
	return keys, nil
}

// AuditLog returns up to limit records of the audit trail after the given
// sequence number, verified against the hash chain. The error wraps
// domain.ErrIntegrity when the trail has been tampered with.
func (s *IAMService) AuditLog(ctx context.Context, p iam.Principal, after uint64, limit int) ([]audit.Record, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetAuditLog, "audit", "", nil); err != nil {
		return nil, fmt.Errorf("iam.AuditLog: %w", err)
	}
	records, err := ReadAudit(ctx, s.trail, after, limit)
	if err != nil {
		return nil, fmt.Errorf("iam.AuditLog: %w", err)
	}
	return records, nil
}
