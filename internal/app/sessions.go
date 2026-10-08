package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// DefaultSessionLifetime is how long a console session lasts. Sessions are
// short-lived and are not extended: when one ends, the user signs in again.
const DefaultSessionLifetime = 30 * time.Minute

// sessionTokenSize is the size of a session token and of a CSRF token, in
// bytes of randomness.
const sessionTokenSize = 32

// ActionSignIn and ActionSignOut name sign-in and sign-out in the audit
// trail. They are not actions a policy grants.
const (
	ActionSignIn  iam.Action = "auth:SignIn"
	ActionSignOut iam.Action = "auth:SignOut"
)

// LocalIdentityProvider checks console passwords against the hashes stored
// with the users.
type LocalIdentityProvider struct {
	users  ports.IAMStore
	hasher ports.PasswordHasher
	// decoy is a hash nobody's password matches. It is verified against
	// when there is no user or no password to check, so that the answer
	// takes as long either way and does not tell which names exist.
	decoy string
}

var _ ports.IdentityProvider = (*LocalIdentityProvider)(nil)

// NewLocalIdentityProvider returns the provider for local accounts. entropy
// must be a cryptographically secure random source.
func NewLocalIdentityProvider(users ports.IAMStore, hasher ports.PasswordHasher, entropy io.Reader) (*LocalIdentityProvider, error) {
	random := make([]byte, sessionTokenSize)
	if _, err := io.ReadFull(entropy, random); err != nil {
		return nil, fmt.Errorf("app.NewLocalIdentityProvider: %w", err)
	}
	decoy, err := hasher.Hash(base64.RawURLEncoding.EncodeToString(random))
	if err != nil {
		return nil, fmt.Errorf("app.NewLocalIdentityProvider: %w", err)
	}
	return &LocalIdentityProvider{users: users, hasher: hasher, decoy: decoy}, nil
}

// Authenticate implements ports.IdentityProvider.
func (p *LocalIdentityProvider) Authenticate(ctx context.Context, username, password string) (string, error) {
	refused := fmt.Errorf("%w: the user name or the password is wrong", domain.ErrAccessDenied)
	if len(password) > maxPasswordLength {
		return "", refused
	}
	user, err := p.users.GetUser(ctx, username)
	known := err == nil
	if err != nil && !errors.Is(err, domain.ErrEntityNotFound) && !errors.Is(err, domain.ErrInvalidArgument) {
		return "", fmt.Errorf("identity.Authenticate: %w", err)
	}
	encoded := p.decoy
	if known && user.PasswordHash != "" {
		encoded = user.PasswordHash
	}
	match, err := p.hasher.Verify(encoded, password)
	if err != nil {
		return "", fmt.Errorf("identity.Authenticate: %w", err)
	}
	if !match || !known || user.PasswordHash == "" || user.Disabled {
		return "", refused
	}
	return user.Name, nil
}

// SignedIn is what a browser is given at sign-in. Both tokens are shown
// once; the store keeps only their hashes.
type SignedIn struct {
	// Token identifies the session. It goes into a cookie that scripts
	// cannot read.
	Token string
	// CSRFToken must come back, in a header, with every request that
	// changes something: a page of another site can make the browser send
	// the cookie, but cannot read this.
	CSRFToken string
	User      string
	ExpiresAt time.Time
}

// SessionService signs console users in and out and answers who a session
// belongs to.
type SessionService struct {
	sessions ports.SessionStore
	users    ports.IAMStore
	provider ports.IdentityProvider
	auditor  *Auditor
	clock    ports.Clock
	lifetime time.Duration

	entropyMu sync.Mutex
	entropy   io.Reader
}

// SessionServiceConfig lists what a SessionService depends on.
type SessionServiceConfig struct {
	Sessions ports.SessionStore
	Users    ports.IAMStore
	Provider ports.IdentityProvider
	Auditor  *Auditor
	Clock    ports.Clock
	// Lifetime is how long a session lasts; zero means the default.
	Lifetime time.Duration
	// Entropy must be a cryptographically secure random source.
	Entropy io.Reader
}

// NewSessionService returns a SessionService.
func NewSessionService(cfg SessionServiceConfig) *SessionService {
	lifetime := cfg.Lifetime
	if lifetime <= 0 {
		lifetime = DefaultSessionLifetime
	}
	return &SessionService{
		sessions: cfg.Sessions, users: cfg.Users, provider: cfg.Provider, auditor: cfg.Auditor, clock: cfg.Clock,
		lifetime: lifetime, entropy: cfg.Entropy,
	}
}

// hashToken returns what is stored in place of a token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *SessionService) newToken() (string, error) {
	random := make([]byte, sessionTokenSize)
	s.entropyMu.Lock()
	_, err := io.ReadFull(s.entropy, random)
	s.entropyMu.Unlock()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// SignIn checks a user's credentials and starts a session. A refusal wraps
// domain.ErrAccessDenied, says nothing about why, and is audited.
func (s *SessionService) SignIn(ctx context.Context, username, password string, session iam.Session) (SignedIn, error) {
	name, err := s.provider.Authenticate(ctx, username, password)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDenied) {
			// What was typed into the user field is kept only if it
			// could be a user name: people type passwords there.
			claimed := username
			if iam.ValidateName(claimed) != nil {
				claimed = "(not a user name)"
			}
			s.auditor.AuthenticationFailed(ctx, string(ActionSignIn), claimed, session, "credentials not accepted")
		}
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	token, err := s.newToken()
	if err != nil {
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	csrf, err := s.newToken()
	if err != nil {
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	now := s.clock.Now().UTC()
	stored := ports.Session{
		TokenHash: hashToken(token), User: name, CSRFHash: hashToken(csrf), CreatedAt: now, ExpiresAt: now.Add(s.lifetime),
	}
	principal := iam.Principal{User: name, Session: session}
	record := s.auditor.Record(principal, ActionSignIn, iam.Resource{Admin: "user/" + name}, audit.Succeeded, nil)
	if err := s.sessions.PutSession(ctx, stored, record); err != nil {
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	return SignedIn{Token: token, CSRFToken: csrf, User: name, ExpiresAt: stored.ExpiresAt}, nil
}

// Authenticate returns the principal a session token stands for: the user,
// with every policy that applies now. The error wraps domain.ErrAccessDenied
// when the token names no session, the session has ended, or the user is
// gone or disabled.
//
// A request that changes something must also pass csrfToken, the token the
// browser was given at sign-in; pass mutating as false for a request that
// only reads.
func (s *SessionService) Authenticate(ctx context.Context, token, csrfToken string, mutating bool, session iam.Session) (iam.Principal, error) {
	refused := fmt.Errorf("session.Authenticate: %w: not signed in", domain.ErrAccessDenied)
	if token == "" || len(token) > 4*sessionTokenSize {
		return iam.Principal{}, refused
	}
	stored, err := s.sessions.GetSession(ctx, hashToken(token))
	if errors.Is(err, domain.ErrEntityNotFound) {
		return iam.Principal{}, refused
	}
	if err != nil {
		return iam.Principal{}, fmt.Errorf("session.Authenticate: %w", err)
	}
	if !s.clock.Now().Before(stored.ExpiresAt) {
		return iam.Principal{}, refused
	}
	if mutating && !iam.SecretsEqual([]byte(hashToken(csrfToken)), []byte(stored.CSRFHash)) {
		s.auditor.AuthenticationFailed(ctx, "auth:CSRF", stored.User, session, "missing or wrong CSRF token")
		return iam.Principal{}, fmt.Errorf("session.Authenticate: %w: the CSRF token is missing or wrong", domain.ErrAccessDenied)
	}
	identity, err := s.users.ResolveUser(ctx, stored.User)
	if errors.Is(err, domain.ErrEntityNotFound) || (err == nil && identity.User.Disabled) {
		return iam.Principal{}, refused
	}
	if err != nil {
		return iam.Principal{}, fmt.Errorf("session.Authenticate: %w", err)
	}
	return iam.Principal{User: identity.User.Name, Policies: identity.Policies, Session: session}, nil
}

// SignOut ends a session. Ending one that does not exist is not an error.
func (s *SessionService) SignOut(ctx context.Context, token string, session iam.Session) error {
	hash := hashToken(token)
	stored, err := s.sessions.GetSession(ctx, hash)
	if errors.Is(err, domain.ErrEntityNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session.SignOut: %w", err)
	}
	principal := iam.Principal{User: stored.User, Session: session}
	record := s.auditor.Record(principal, ActionSignOut, iam.Resource{Admin: "user/" + stored.User}, audit.Succeeded, nil)
	if err := s.sessions.DeleteSession(ctx, hash, record); err != nil {
		return fmt.Errorf("session.SignOut: %w", err)
	}
	return nil
}

// Sweep removes the sessions that have ended. A node calls it now and then;
// an ended session is refused whether or not it has been swept.
func (s *SessionService) Sweep(ctx context.Context) (int, error) {
	removed, err := s.sessions.DeleteExpiredSessions(ctx, s.clock.Now())
	if err != nil {
		return 0, fmt.Errorf("session.Sweep: %w", err)
	}
	return removed, nil
}
