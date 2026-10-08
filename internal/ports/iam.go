package ports

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// IAMStore is the replicated store of users, groups, roles, policies and API
// keys. Every change is committed through Raft before the call returns, and
// carries the audit record of the change in the same entry: there is no
// change without its record and no record of a change that did not happen.
// Reads are strongly consistent.
//
// The store keeps references whole. A user can only be given groups and
// roles that exist, a role policies that exist; a group, role or policy that
// something still refers to cannot be removed (domain.ErrEntityInUse).
// Built-in roles and policies are not stored and cannot be put or removed.
//
// Put with create set adds a new entity and fails with
// domain.ErrEntityExists when the name is taken; without it, it replaces an
// existing one and fails with domain.ErrEntityNotFound when there is none.
// Get and Delete fail with domain.ErrEntityNotFound.
type IAMStore interface {
	PutUser(ctx context.Context, u iam.User, create bool, record audit.Record) error
	// DeleteUser removes a user together with the user's API keys and
	// sessions.
	DeleteUser(ctx context.Context, name string, record audit.Record) error
	GetUser(ctx context.Context, name string) (iam.User, error)
	ListUsers(ctx context.Context) ([]iam.User, error)

	PutGroup(ctx context.Context, g iam.Group, create bool, record audit.Record) error
	DeleteGroup(ctx context.Context, name string, record audit.Record) error
	GetGroup(ctx context.Context, name string) (iam.Group, error)
	ListGroups(ctx context.Context) ([]iam.Group, error)

	PutRole(ctx context.Context, r iam.Role, create bool, record audit.Record) error
	DeleteRole(ctx context.Context, name string, record audit.Record) error
	GetRole(ctx context.Context, name string) (iam.Role, error)
	ListRoles(ctx context.Context) ([]iam.Role, error)

	PutPolicy(ctx context.Context, p iam.Policy, create bool, record audit.Record) error
	DeletePolicy(ctx context.Context, name string, record audit.Record) error
	GetPolicy(ctx context.Context, name string) (iam.Policy, error)
	ListPolicies(ctx context.Context) ([]iam.Policy, error)

	// CreateAPIKey adds a key. Its owner must exist.
	CreateAPIKey(ctx context.Context, k iam.APIKey, record audit.Record) error
	// DeleteAPIKey revokes a key. From the moment the call returns no node
	// accepts it.
	DeleteAPIKey(ctx context.Context, accessKeyID string, record audit.Record) error
	// ListAPIKeys returns the keys of a user, or of everybody when owner is
	// empty. The sealed secrets are included; they are useless without the
	// KMS.
	ListAPIKeys(ctx context.Context, owner string) ([]iam.APIKey, error)
	// TouchAPIKey records that a key was used at the given time. It is not
	// audited: it says nothing that the requests themselves do not.
	TouchAPIKey(ctx context.Context, accessKeyID string, usedAt time.Time) error

	// ResolveIdentity returns, as of one moment, an API key, its owner and
	// every policy document that applies to the owner. The error wraps
	// domain.ErrCredentialNotFound when there is no such key.
	ResolveIdentity(ctx context.Context, accessKeyID string) (iam.Identity, error)
	// ResolveUser returns a user and every policy document that applies,
	// for a session that acts as the user without an API key.
	ResolveUser(ctx context.Context, name string) (iam.Identity, error)
}

// AuditLog is the replicated, append-only, hash-chained audit trail.
type AuditLog interface {
	// AppendAudit adds a record. The store numbers it and links it to the
	// one before; Sequence, PreviousHash and Hash of the argument are
	// ignored.
	AppendAudit(ctx context.Context, record audit.Record) error
	// ListAudit returns up to limit records with a sequence number greater
	// than after, in order.
	ListAudit(ctx context.Context, after uint64, limit int) ([]audit.Record, error)
}

// Session is a signed-in console user, as it is stored. The token itself is
// not stored, only its hash, so that a copy of the state signs nobody in.
type Session struct {
	// TokenHash identifies the session: the SHA-256 of its token, in hex.
	TokenHash string
	User      string
	// CSRFHash is the SHA-256, in hex, of the token a browser must send
	// back with every request that changes something.
	CSRFHash  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionStore is the replicated store of console sessions.
type SessionStore interface {
	PutSession(ctx context.Context, s Session, record audit.Record) error
	// GetSession fails with domain.ErrEntityNotFound for an unknown hash.
	GetSession(ctx context.Context, tokenHash string) (Session, error)
	// DeleteSession ends a session. Ending one that does not exist is not
	// an error.
	DeleteSession(ctx context.Context, tokenHash string, record audit.Record) error
	// DeleteExpiredSessions removes the sessions that ended before now and
	// returns how many there were.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}

// PasswordHasher turns console passwords into hashes that are slow to guess
// from, and checks a password against one.
type PasswordHasher interface {
	// Hash returns an encoded hash that carries its own parameters and salt.
	Hash(password string) (string, error)
	// Verify reports whether password is the one encoded was made from. It
	// takes as long for a wrong password as for the right one. An encoded
	// hash it cannot read is an error.
	Verify(encoded, password string) (bool, error)
}

// IdentityProvider says who somebody is from what they present at sign-in.
// The local provider checks a password; an OIDC provider will check a token.
type IdentityProvider interface {
	// Authenticate returns the name of the user the credentials belong to.
	// The error wraps domain.ErrAccessDenied when they are not accepted,
	// without saying whether the user exists.
	Authenticate(ctx context.Context, username, password string) (string, error)
}
