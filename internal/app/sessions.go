package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// DefaultSessionLifetime is how long a console session lasts before it
// reaches its timeout. Sessions are short-lived and nothing extends one: at
// the timeout the user signs in again - or, when objects are being uploaded
// with the session, unlocks it with their password (ADR-0027).
const DefaultSessionLifetime = 30 * time.Minute

// DefaultSessionUploadGrace is how long an upload lease lasts: a session
// that is locked ends that long, at the most, after its last upload did.
const DefaultSessionUploadGrace = 90 * time.Second

// DefaultSessionLockMax is the longest a session stays locked, counted from
// its timeout, whatever is being uploaded.
const DefaultSessionLockMax = 12 * time.Hour

// unlockAttempts is how many wrong passwords a session is unlocked with
// before it is ended.
const unlockAttempts = 5

// uploadLeasePrefix begins the key of every upload lease. No token hashes to
// a key that begins so: hashToken returns hexadecimal digits and nothing
// else.
const uploadLeasePrefix = "upload-lease:"

// passwordChangePrefix begins the key of the mark that says when the
// password of a user was last changed. Like the key of a lease it is the
// hash of no token.
const passwordChangePrefix = "password-change:"

// SessionsEndHorizon is how long the mark of a password change is kept: for
// longer than any session that was there when the password changed can
// last, locked or not - that is the longest lifetime and the longest lock a
// node accepts as its settings, which the node's own tests hold against
// this.
const SessionsEndHorizon = 48 * time.Hour

// PasswordChangeMargin is how much later than the mark of a password change
// a session may have begun and still be ended by it. The mark carries the
// clock of the gateway that changed the password, a session the clock of
// the gateway that began it; within this margin a session that began before
// the change, on a gateway whose clock is ahead, is ended all the same. The
// cost is the other direction: a session begun within the margin after the
// change, with the new password, is ended too - its user signs in again
// (ADR-0027).
const PasswordChangeMargin = 5 * time.Second

// maxMadeSessions bounds how many sessions a node remembers having begun,
// to remove their records when they have ended.
const maxMadeSessions = 1 << 16

// madeChecked bounds how many of them one pass looks at.
const madeChecked = 256

// sessionTokenSize is the size of a session token and of a CSRF token, in
// bytes of randomness.
const sessionTokenSize = 32

// ActionSignIn, ActionSignOut and ActionUnlockSession name sign-in, sign-out
// and the unlocking of a session in the audit trail; ActionEndSessions the
// ending of every session of a user that a change of password makes. They
// are not actions a policy grants.
const (
	ActionSignIn        iam.Action = "auth:SignIn"
	ActionSignOut       iam.Action = "auth:SignOut"
	ActionUnlockSession iam.Action = "auth:UnlockSession"
	ActionEndSessions   iam.Action = "auth:EndSessions"
	// ActionConfirmPassword names a password typed again into a session
	// for one request that asks for it (SessionService.Confirm).
	ActionConfirmPassword iam.Action = "auth:ConfirmPassword"
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

// SignedIn is what a browser is given at sign-in. The session token is
// shown once; the CSRF token derives from it and is told again to whoever
// holds it (SessionService.CSRFToken). The store keeps only their hashes.
type SignedIn struct {
	// Token identifies the session. It goes into a cookie that scripts
	// cannot read.
	Token string
	// CSRFToken must come back, in a header, with every request that
	// changes something: a page of another site can make the browser send
	// the cookie, but cannot read this.
	CSRFToken string
	User      string
	// ExpiresAt is when the session reaches its timeout.
	ExpiresAt time.Time
	// CookieExpires is until when a browser is to keep the token: the
	// latest moment the session can be locked, which is ExpiresAt where
	// sessions never lock. A browser that dropped the cookie at the
	// timeout could not unlock.
	CookieExpires time.Time
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
	// uploadGrace is how long an upload lease lasts; lockMax how long a
	// session can be locked, and zero when sessions never lock.
	uploadGrace time.Duration
	lockMax     time.Duration

	entropyMu sync.Mutex
	entropy   io.Reader

	// attempts counts failed sign-ins and unlocks per address; names
	// counts failed sign-ins per user name; known remembers from where
	// each user signed in.
	attempts attemptLimiter
	names    attemptLimiter
	known    knownAddresses
	unlocks  unlockFailures
	leases   uploadLeases
}

const (
	// signInAttempts is how many failed sign-ins one address may make in
	// signInWindow before it has to wait for the window to end.
	signInAttempts = 10
	signInWindow   = 5 * time.Minute
	// userSignInAttempts is how many failed sign-ins one user name may be
	// given, from every address together, in userSignInWindow. It is what
	// stops guessing the password of one user from many addresses.
	userSignInAttempts = 30
	userSignInWindow   = 15 * time.Minute
	// signInAddresses bounds how many addresses, user names and known
	// addresses of users are remembered, each.
	signInAddresses = 10_000
	// knownAddressHorizon is how long an address a user signed in from is
	// remembered as theirs.
	knownAddressHorizon = 30 * 24 * time.Hour
	// ipv6SignInPrefix is how much of an IPv6 address is counted as one
	// address: a /64 is what one subscriber is given, and every address in
	// it is theirs to use.
	ipv6SignInPrefix = 64
)

// signInAddress is what the limiter counts the source of a request as: an
// IPv4 address as it is, an IPv6 address by its /64, and no address as one
// address of its own.
func signInAddress(addr netip.Addr) string {
	addr = addr.Unmap()
	switch {
	case !addr.IsValid():
		return "unknown"
	case addr.Is4():
		return addr.String()
	default:
		prefix, err := addr.Prefix(ipv6SignInPrefix)
		if err != nil {
			return addr.String()
		}
		return prefix.String()
	}
}

// attemptLimiter counts failed attempts per key - an address, a user name -
// in windows of a fixed length. It lives in one node's memory: a cluster
// allows as many attempts as it has gateways.
//
// It remembers at most capacity windows. When it is full, a new key takes
// the place of a window that has ended or, if none has, of the window that
// has counted the fewest failures, the oldest of those. Both are found in
// logarithmic time - the windows are kept in two heaps, by start and by
// count - so that a caller who makes a new key at every attempt holds the
// lock for no more than that (review of 2026-11-13).
type attemptLimiter struct {
	mu     sync.Mutex
	failed map[string]*attemptWindow
	// byStart orders the windows by when they began; byCount by how many
	// failures they counted, then by when they began.
	byStart, byCount windowHeap
	// limit is how many attempts a key may make in a window.
	limit    int
	window   time.Duration
	capacity int
}

type attemptWindow struct {
	key   string
	since time.Time
	count int
	// at is the window's position in each heap: byStart, byCount.
	at [2]int
}

// newAttemptLimiter returns a limiter of limit attempts per key in window.
func newAttemptLimiter(limit int, window time.Duration) attemptLimiter {
	return newAttemptLimiterOf(limit, window, signInAddresses)
}

// newAttemptLimiterOf is newAttemptLimiter remembering capacity windows.
func newAttemptLimiterOf(limit int, window time.Duration, capacity int) attemptLimiter {
	return attemptLimiter{
		failed: map[string]*attemptWindow{}, limit: limit, window: window, capacity: capacity,
		byStart: windowHeap{order: byStartOrder}, byCount: windowHeap{order: byCountOrder},
	}
}

// reserve counts an attempt of a key that is about to be checked, and
// reports false when the key has used up its attempts.
func (l *attemptLimiter) reserve(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.failed[key]
	if ok && now.Sub(w.since) < l.window {
		if w.count >= l.limit {
			return false
		}
		w.count++
		l.byCount.fix(w)
		return true
	}
	if ok {
		l.remove(w)
	} else if len(l.failed) >= l.capacity {
		l.makeRoom(now)
	}
	w = &attemptWindow{key: key, since: now, count: 1}
	l.failed[key] = w
	l.byStart.push(w)
	l.byCount.push(w)
	return true
}

// makeRoom forgets a window that has ended or, if none has, the one window
// that has counted the fewest failures, the oldest of those. A live window
// is never forgotten wholesale: a key that has used up its attempts gets
// them back only when every other key has failed as often - which takes as
// many guesses as the window it frees allows.
func (l *attemptLimiter) makeRoom(now time.Time) {
	if oldest := l.byStart.top(); oldest != nil && now.Sub(oldest.since) >= l.window {
		l.remove(oldest)
		return
	}
	if least := l.byCount.top(); least != nil {
		l.remove(least)
	}
}

// remove forgets a window.
func (l *attemptLimiter) remove(w *attemptWindow) {
	delete(l.failed, w.key)
	l.byStart.remove(w)
	l.byCount.remove(w)
}

// forgive takes back the count of an attempt that was not a wrong guess.
func (l *attemptLimiter) forgive(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.failed[key]; ok && w.count > 0 {
		w.count--
		l.byCount.fix(w)
	}
}

// knownAddresses remembers, for each user, the addresses they signed in
// from lately. An address a user is known at is not held to the budget of
// the user name (userSignInAttempts): whoever guesses a user's password
// from elsewhere can make the user wait to sign in from a new address, not
// from the one they always use.
type knownAddresses struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func knownKey(user, address string) string { return user + "\x00" + address }

// remember notes that a user signed in from an address.
func (k *knownAddresses) remember(user, address string, now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.seen == nil {
		k.seen = map[string]time.Time{}
	}
	key := knownKey(user, address)
	if _, ok := k.seen[key]; !ok && len(k.seen) >= signInAddresses {
		// Full: forget what is past the horizon and, if nothing is, the
		// address seen the longest ago.
		var oldest string
		var at time.Time
		for known, when := range k.seen {
			if now.Sub(when) >= knownAddressHorizon {
				delete(k.seen, known)
				continue
			}
			if oldest == "" || when.Before(at) {
				oldest, at = known, when
			}
		}
		if len(k.seen) >= signInAddresses {
			delete(k.seen, oldest)
		}
	}
	k.seen[key] = now
}

// knows reports whether a user signed in from an address within the
// horizon.
func (k *knownAddresses) knows(user, address string, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	when, ok := k.seen[knownKey(user, address)]
	return ok && now.Sub(when) < knownAddressHorizon
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
	// UploadGrace is how long an upload lease lasts; zero means the
	// default.
	UploadGrace time.Duration
	// LockMax is the longest a session that reached its timeout while
	// objects were being uploaded with it stays locked. Zero means the
	// default; a negative value means that sessions never lock, and end at
	// their timeout whatever is in flight.
	LockMax time.Duration
	// Entropy must be a cryptographically secure random source.
	Entropy io.Reader
}

// NewSessionService returns a SessionService.
func NewSessionService(cfg SessionServiceConfig) *SessionService {
	lifetime := cfg.Lifetime
	if lifetime <= 0 {
		lifetime = DefaultSessionLifetime
	}
	grace := cfg.UploadGrace
	if grace <= 0 {
		grace = DefaultSessionUploadGrace
	}
	lockMax := cfg.LockMax
	switch {
	case lockMax == 0:
		lockMax = DefaultSessionLockMax
	case lockMax < 0:
		lockMax = 0
	}
	return &SessionService{
		sessions: cfg.Sessions, users: cfg.Users, provider: cfg.Provider, auditor: cfg.Auditor, clock: cfg.Clock,
		lifetime: lifetime, uploadGrace: grace, lockMax: lockMax, entropy: cfg.Entropy,
		leases:   uploadLeases{wake: make(chan struct{}, 1)},
		attempts: newAttemptLimiter(signInAttempts, signInWindow),
		names:    newAttemptLimiter(userSignInAttempts, userSignInWindow),
	}
}

// uploadLeaseKey is the key of the upload lease of a session: of the user
// and the time of sign-in, which an unlock carries over to the session's new
// record, so that an upload begun before an unlock still counts after it.
//
// It never has the form of what hashToken returns - that is 64 hexadecimal
// digits, this begins with a word and a colon - so no token, on this release
// or on one before it, is looked up under the key of a lease: a lease signs
// nobody in.
func uploadLeaseKey(user string, createdAt time.Time) string {
	sum := sha256.Sum256([]byte(user + "\n" + createdAt.UTC().Format(time.RFC3339Nano)))
	return uploadLeasePrefix + hex.EncodeToString(sum[:])
}

// passwordChangeKey is the key of the mark of a user's last change of
// password. Like the key of a lease it never has the form of what hashToken
// returns.
func passwordChangeKey(user string) string {
	sum := sha256.Sum256([]byte(user))
	return passwordChangePrefix + hex.EncodeToString(sum[:])
}

// notASession reports whether a stored record is one of the marks kept among
// the sessions - an upload lease, the mark of a password change - and no
// session.
func notASession(stored ports.Session) bool {
	return strings.HasPrefix(stored.TokenHash, uploadLeasePrefix) || strings.HasPrefix(stored.TokenHash, passwordChangePrefix)
}

// endSessionsOf ends every session of a user that exists at the time now,
// active or locked, on every node: it leaves the mark that stateOf holds
// each session of the user against. It is what a change of password does. No
// record is looked for and none is deleted - the store cannot be asked for
// the sessions of a user - so it costs one entry whatever the user has.
func endSessionsOf(ctx context.Context, sessions ports.SessionStore, user string, now time.Time) error {
	mark := ports.Session{
		TokenHash: passwordChangeKey(user), User: user, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(SessionsEndHorizon),
	}
	return sessions.PutSession(ctx, mark, audit.Record{})
}

// sessionState is what a session is at one moment.
type sessionState int

const (
	// sessionEnded: past its timeout with no upload in flight, past the
	// longest a session can be locked, or never there.
	sessionEnded sessionState = iota
	// sessionActive: before its timeout.
	sessionActive
	// sessionLocked: past its timeout, and kept because an upload lease of
	// it is live.
	sessionLocked
)

// stateOf derives what a stored session is at the time now (ADR-0027).
// Nothing stored says "locked": it follows from the record, from the upload
// lease the gateways keep while they serve the session's uploads, and from
// the clock. And nothing stored in the session says that the password of its
// user was changed since: that follows from the mark the change left.
func (s *SessionService) stateOf(ctx context.Context, stored ports.Session, now time.Time) (sessionState, error) {
	if notASession(stored) {
		// Not reachable through a token; and should a store ever hand one
		// out, a mark is no session.
		return sessionEnded, nil
	}
	state := sessionActive
	if !now.Before(stored.ExpiresAt) {
		if s.lockMax <= 0 || !now.Before(stored.ExpiresAt.Add(s.lockMax)) {
			return sessionEnded, nil
		}
		lease, err := s.sessions.GetSession(ctx, uploadLeaseKey(stored.User, stored.CreatedAt))
		if errors.Is(err, domain.ErrEntityNotFound) {
			return sessionEnded, nil
		}
		if err != nil {
			return sessionEnded, err
		}
		if lease.User != stored.User || !now.Before(lease.ExpiresAt) {
			return sessionEnded, nil
		}
		state = sessionLocked
	}
	// A session that was there when the password of its user was changed
	// has ended, locked or not: whoever holds it proved an old password.
	// A session of the very moment of the change counts as one from before,
	// and so does one of up to PasswordChangeMargin later: the two times
	// may be of two clocks. A mark that cannot be read ends the session -
	// it may be one that should. The session, its lease and the mark are
	// three reads: the port reads one record at a time (ADR-0027).
	mark, err := s.sessions.GetSession(ctx, passwordChangeKey(stored.User))
	if errors.Is(err, domain.ErrEntityNotFound) {
		return state, nil
	}
	if err != nil {
		return sessionEnded, err
	}
	if mark.User == stored.User && !stored.CreatedAt.After(mark.CreatedAt.Add(PasswordChangeMargin)) {
		return sessionEnded, nil
	}
	return state, nil
}

// cookieExpires is until when a browser is to keep the token of a session
// that reaches its timeout at expiresAt.
func (s *SessionService) cookieExpires(expiresAt time.Time) time.Time {
	return expiresAt.Add(s.lockMax)
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
//
// An address that keeps failing is made to wait: the error then wraps
// domain.ErrTooManyAttempts, and the password is not looked at.
func (s *SessionService) SignIn(ctx context.Context, username, password string, session iam.Session) (SignedIn, error) {
	now := s.clock.Now()
	from := signInAddress(session.SourceIP)
	// The attempt is counted before the password is looked at: guesses
	// made at the same moment count like guesses made one by one.
	if !s.attempts.reserve(from, now) {
		s.auditor.AuthenticationFailed(ctx, string(ActionSignIn), "", session, "too many failed attempts from this address")
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", domain.ErrTooManyAttempts)
	}
	// And against the user name, from every address together: guesses at
	// one user's password spread over many addresses are counted all the
	// same. Any name that could be a user's is counted - whether the user
	// exists is not told by this either. An address the user signed in
	// from lately is not held to it: whoever spends the budget of a name
	// makes its user wait to sign in from a new address, for as long as
	// they keep guessing, and never from the one the user always uses.
	claimed := username
	if iam.ValidateName(claimed) != nil {
		claimed = ""
	}
	counted := false
	if claimed != "" {
		counted = s.names.reserve(claimed, now)
		if !counted && !s.known.knows(claimed, from, now) {
			s.attempts.forgive(from)
			s.auditor.AuthenticationFailed(ctx, string(ActionSignIn), claimed, session, "too many failed attempts for this user name")
			return SignedIn{}, fmt.Errorf("session.SignIn: %w", domain.ErrTooManyAttempts)
		}
	}
	failed := false
	defer func() {
		if !failed {
			// Only a wrong guess counts against an address and a name.
			s.attempts.forgive(from)
			if counted {
				s.names.forgive(claimed)
			}
		}
	}()
	// The session begins when the password is about to be checked, not
	// when the check is over: a check that read the hash of a password
	// that is being changed began before that change was written, and so
	// before its mark - and the session it opens is ended by the mark.
	// Taken after the check, a slow check that straddled the change would
	// open a session of the old password dated after the change.
	begun := s.clock.Now().UTC()
	name, err := s.provider.Authenticate(ctx, username, password)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDenied) {
			failed = true
			// What was typed into the user field is kept only if it
			// could be a user name: people type passwords there.
			recorded := claimed
			if recorded == "" {
				recorded = "(not a user name)"
			}
			s.auditor.AuthenticationFailed(ctx, string(ActionSignIn), recorded, session, "credentials not accepted")
		}
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	token, err := s.newToken()
	if err != nil {
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	csrf := csrfTokenFor(token)
	// Its lifetime is counted from now: the user gets all of it.
	stored := ports.Session{
		TokenHash: hashToken(token), User: name, CSRFHash: hashToken(csrf), CreatedAt: begun, ExpiresAt: s.clock.Now().UTC().Add(s.lifetime),
	}
	principal := iam.Principal{User: name, Session: session}
	record := s.auditor.Record(principal, ActionSignIn, iam.Resource{Admin: "user/" + name}, audit.Succeeded, nil)
	if err := s.sessions.PutSession(ctx, stored, record); err != nil {
		return SignedIn{}, fmt.Errorf("session.SignIn: %w", err)
	}
	s.leases.remember(stored.TokenHash, stored.ExpiresAt, s.lockMax > 0)
	s.known.remember(name, from, now)
	return SignedIn{
		Token: token, CSRFToken: csrf, User: name, ExpiresAt: stored.ExpiresAt, CookieExpires: s.cookieExpires(stored.ExpiresAt),
	}, nil
}

// Authenticate returns the principal a session token stands for: the user,
// with every policy that applies now. The error wraps domain.ErrAccessDenied
// when the token names no session, the session has ended, or the user is
// gone or disabled.
//
// A session that has reached its timeout while objects are being uploaded
// with it has not ended: it is locked, and the principal says so
// (iam.Principal.LockedSession). What a locked session may do is the
// authorizer's to say, not this use case's and not the caller's.
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
	state, err := s.stateOf(ctx, stored, s.clock.Now())
	if err != nil {
		return iam.Principal{}, fmt.Errorf("session.Authenticate: %w", err)
	}
	if state == sessionEnded {
		return iam.Principal{}, refused
	}
	if mutating && !iam.SecretsEqual([]byte(hashToken(csrfToken)), []byte(stored.CSRFHash)) {
		s.auditor.AuthenticationFailed(ctx, "auth:CSRF", stored.User, session, "missing or wrong CSRF token")
		return iam.Principal{}, fmt.Errorf("session.Authenticate: %w", domain.ErrCSRFTokenRefused)
	}
	identity, err := s.users.ResolveUser(ctx, stored.User)
	if errors.Is(err, domain.ErrEntityNotFound) || (err == nil && identity.User.Disabled) {
		return iam.Principal{}, refused
	}
	if err != nil {
		return iam.Principal{}, fmt.Errorf("session.Authenticate: %w", err)
	}
	return iam.Principal{
		User: identity.User.Name, UserCreatedAt: identity.User.CreatedAt, Policies: identity.Policies, Session: session,
		LockedSession: state == sessionLocked,
	}, nil
}

// SessionStatus is what a session is at one moment, for its own user.
type SessionStatus struct {
	// Locked says that the session has reached its timeout and is kept
	// because objects are being uploaded with it.
	Locked bool
	// ExpiresAt is when the session reaches, or reached, its timeout.
	ExpiresAt time.Time
	// LockEndsAt is the latest moment the session can stay locked to: by
	// then it has ended whatever is being uploaded. Zero where sessions
	// never lock.
	LockEndsAt time.Time
}

// Status returns what the session of a token is now. The error wraps
// domain.ErrAccessDenied when the token names no session or the session has
// ended. It does not look at the user: whoever asks has been authenticated
// with the same token.
func (s *SessionService) Status(ctx context.Context, token string) (SessionStatus, error) {
	refused := fmt.Errorf("session.Status: %w: not signed in", domain.ErrAccessDenied)
	if token == "" || len(token) > 4*sessionTokenSize {
		return SessionStatus{}, refused
	}
	stored, err := s.sessions.GetSession(ctx, hashToken(token))
	if errors.Is(err, domain.ErrEntityNotFound) {
		return SessionStatus{}, refused
	}
	if err != nil {
		return SessionStatus{}, fmt.Errorf("session.Status: %w", err)
	}
	state, err := s.stateOf(ctx, stored, s.clock.Now())
	if err != nil {
		return SessionStatus{}, fmt.Errorf("session.Status: %w", err)
	}
	if state == sessionEnded {
		return SessionStatus{}, refused
	}
	status := SessionStatus{Locked: state == sessionLocked, ExpiresAt: stored.ExpiresAt}
	if s.lockMax > 0 {
		status.LockEndsAt = stored.ExpiresAt.Add(s.lockMax)
	}
	return status, nil
}

// unlockFailures counts the wrong passwords each session was unlocked with.
// Like the limiter of sign-in it lives in one node's memory: a cluster allows
// as many as it has gateways, times unlockAttempts.
type unlockFailures struct {
	mu     sync.Mutex
	failed map[string]int
}

// add counts a wrong password for a session and returns how many there were.
func (u *unlockFailures) add(tokenHash string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failed == nil {
		u.failed = map[string]int{}
	}
	if _, ok := u.failed[tokenHash]; !ok && len(u.failed) >= signInAddresses {
		// Full of sessions that were left with a wrong password or two.
		// The one with the fewest is forgotten, which gives it its
		// attempts back - not every session at once; the limiter of the
		// address counts on.
		victim, least := "", 0
		for hash, count := range u.failed {
			if victim == "" || count < least {
				victim, least = hash, count
			}
		}
		delete(u.failed, victim)
	}
	u.failed[tokenHash]++
	return u.failed[tokenHash]
}

// forget drops the count of a session that was unlocked or has ended.
func (u *unlockFailures) forget(tokenHash string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.failed, tokenHash)
}

// judgedSession is a session whose user has just typed the right password.
type judgedSession struct {
	stored ports.Session
	state  sessionState
	hash   string
}

// judgePassword is the path a password typed into a session takes, at an
// unlock and at a confirmation alike: the session is looked up, the address
// is counted by the limiter of sign-in, the password goes to the same
// provider, and a wrong one is audited as action and counted for the session
// - the unlockAttempts-th ends it. op begins every error. Nothing of the
// password is kept or logged. The errors are those of Unlock.
func (s *SessionService) judgePassword(ctx context.Context, op string, action iam.Action, token, password string, session iam.Session) (judgedSession, error) {
	ended := fmt.Errorf("%s: %w: not signed in", op, domain.ErrCredentialNotFound)
	if token == "" || len(token) > 4*sessionTokenSize {
		return judgedSession{}, ended
	}
	hash := hashToken(token)
	stored, err := s.sessions.GetSession(ctx, hash)
	if errors.Is(err, domain.ErrEntityNotFound) {
		return judgedSession{}, ended
	}
	if err != nil {
		return judgedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	state, err := s.stateOf(ctx, stored, s.clock.Now())
	if err != nil {
		return judgedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	if state == sessionEnded {
		return judgedSession{}, ended
	}
	from := signInAddress(session.SourceIP)
	// Counted before the password is looked at, and by the counter of
	// sign-in: an address has one budget of guesses, not two.
	if !s.attempts.reserve(from, s.clock.Now()) {
		s.auditor.AuthenticationFailed(ctx, string(action), stored.User, session, "too many failed attempts from this address")
		return judgedSession{}, fmt.Errorf("%s: %w", op, domain.ErrTooManyAttempts)
	}
	failed := false
	defer func() {
		if !failed {
			// Only a wrong guess counts against an address.
			s.attempts.forgive(from)
		}
	}()
	if _, err := s.provider.Authenticate(ctx, stored.User, password); err != nil {
		if !errors.Is(err, domain.ErrAccessDenied) {
			return judgedSession{}, fmt.Errorf("%s: %w", op, err)
		}
		failed = true
		s.auditor.AuthenticationFailed(ctx, string(action), stored.User, session, "credentials not accepted")
		if s.unlocks.add(hash) < unlockAttempts {
			return judgedSession{}, fmt.Errorf("%s: %w", op, err)
		}
		// Too many for this session: it ends. Whoever is guessing loses
		// what they held, and the user signs in again.
		principal := iam.Principal{User: stored.User, Session: session}
		record := s.auditor.Record(principal, action, iam.Resource{Admin: "user/" + stored.User}, audit.Failed,
			map[string]string{"reason": "too many wrong passwords: the session is ended"})
		if err := s.sessions.DeleteSession(ctx, hash, record); err != nil {
			return judgedSession{}, fmt.Errorf("%s: %w", op, err)
		}
		s.unlocks.forget(hash)
		s.leases.forgetMade(hash)
		return judgedSession{}, fmt.Errorf("%s: %w: not signed in any more: %w", op, domain.ErrCredentialNotFound, domain.ErrTooManyAttempts)
	}
	return judgedSession{stored: stored, state: state, hash: hash}, nil
}

// Confirm judges a password typed again into a session for one request that
// asks for it - installing a release (ADR-0029) - and returns the user the
// session belongs to when it is that user's password. It is the path of
// Unlock without what follows it: the session keeps its tokens, its
// timeout and its state, locked or not; what a locked session may do is the
// authorizer's to say, and the caller asks it first.
//
// The password goes where it goes at sign-in and at an unlock - the same
// provider, the same limiter of the caller's address, the same count of
// wrong passwords for the session - and nowhere else: it is not logged and
// not stored. Wrong passwords are audited as ActionConfirmPassword. The
// errors are those of Unlock: domain.ErrAccessDenied for a password that is
// not accepted, domain.ErrTooManyAttempts for an address that must wait,
// domain.ErrCredentialNotFound for a session that has ended, and both of the
// last two when this wrong password was the one that ended it. The right
// password gives the session its count of attempts back.
func (s *SessionService) Confirm(ctx context.Context, token, password string, session iam.Session) (string, error) {
	judged, err := s.judgePassword(ctx, "session.Confirm", ActionConfirmPassword, token, password, session)
	if err != nil {
		return "", err
	}
	s.unlocks.forget(judged.hash)
	return judged.stored.User, nil
}

// Unlock gives a session its full authority back, for another lifetime, when
// password is the password of the session's own user (ADR-0027). It is asked
// of a locked session, and accepted of one that is not: either way the
// session gets a new token and a new CSRF token, which are returned, and the
// ones it had are refused from then on - whoever else holds the old cookie
// does not gain by its owner unlocking. The time of sign-in stays, and with
// it the upload lease: an upload that began before still counts.
//
// The password goes the way a password goes at sign-in - the same provider,
// the same limiter of the caller's address - and nowhere else: it is not
// logged and not stored. A password that is not accepted wraps
// domain.ErrAccessDenied and is audited; an address that keeps failing gets
// domain.ErrTooManyAttempts and its password is not looked at. For a token
// that names no session, or one whose session has ended, the error wraps
// domain.ErrCredentialNotFound - the caller is not signed in, which is
// another thing than a wrong password. The unlockAttempts-th wrong password
// for one session ends the session: the error then wraps both
// domain.ErrCredentialNotFound and domain.ErrTooManyAttempts, and by the
// second alone it is told from a session that had ended before - which may
// be this very session, unlocked a moment ago by a request sent twice.
func (s *SessionService) Unlock(ctx context.Context, token, password string, session iam.Session) (SignedIn, error) {
	judged, err := s.judgePassword(ctx, "session.Unlock", ActionUnlockSession, token, password, session)
	if err != nil {
		return SignedIn{}, err
	}
	stored, state, hash := judged.stored, judged.state, judged.hash
	principal := iam.Principal{User: stored.User, Session: session}
	resource := iam.Resource{Admin: "user/" + stored.User}
	fresh, err := s.newToken()
	if err != nil {
		return SignedIn{}, fmt.Errorf("session.Unlock: %w", err)
	}
	csrf := csrfTokenFor(fresh)
	// The time of sign-in is kept: a password change that lands while the
	// password is checked here leaves its mark after that time, so the
	// renewed session is ended by it as the old one was.
	renewed := ports.Session{
		TokenHash: hashToken(fresh), User: stored.User, CSRFHash: hashToken(csrf),
		CreatedAt: stored.CreatedAt, ExpiresAt: s.clock.Now().UTC().Add(s.lifetime),
	}
	was := "active"
	if state == sessionLocked {
		was = "locked"
	}
	record := s.auditor.Record(principal, ActionUnlockSession, resource, audit.Succeeded, map[string]string{"session": was})
	// The new record first: one that nobody was told the token of is of no
	// use to anybody, and a session without a record would end its uploads.
	if err := s.sessions.PutSession(ctx, renewed, record); err != nil {
		return SignedIn{}, fmt.Errorf("session.Unlock: %w", err)
	}
	s.leases.remember(renewed.TokenHash, renewed.ExpiresAt, s.lockMax > 0)
	if err := s.sessions.DeleteSession(ctx, hash, audit.Record{}); err != nil {
		return SignedIn{}, fmt.Errorf("session.Unlock: %w", err)
	}
	s.unlocks.forget(hash)
	s.leases.forgetMade(hash)
	return SignedIn{
		Token: fresh, CSRFToken: csrf, User: stored.User, ExpiresAt: renewed.ExpiresAt, CookieExpires: s.cookieExpires(renewed.ExpiresAt),
	}, nil
}

// uploadLeases is what one node knows of the uploads it is serving for
// console sessions, of the leases it has written for them, and of the
// sessions it began.
type uploadLeases struct {
	mu   sync.Mutex
	kept map[string]*keptLease
	// made are the sessions this node began - at sign-in or at an unlock -
	// by the hash of their token, with when each reaches its timeout: the
	// node removes the record of each once it has ended.
	made map[string]time.Time
	// wake tells Run that a lease is to be written now, not at the tick.
	wake chan struct{}
}

// keptLease is the upload lease of one session, as this node keeps it.
type keptLease struct {
	user      string
	createdAt time.Time
	// inFlight is how many uploads of the session this node is serving.
	inFlight int
	// touched says that an upload began or ended since the lease was last
	// written; active is when one last did.
	touched bool
	active  time.Time
	// expires is until when the lease this node last wrote lasts; zero
	// when it wrote none.
	expires time.Time
}

// remember notes a session this node began.
func (l *uploadLeases) remember(tokenHash string, expiresAt time.Time, lockable bool) {
	if !lockable {
		// Where sessions never lock, the sweep removes each at its
		// timeout: there is nothing to remember it for.
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.made == nil {
		l.made = map[string]time.Time{}
	}
	if len(l.made) < maxMadeSessions {
		l.made[tokenHash] = expiresAt
	}
}

// forgetMade drops a session whose record is gone.
func (l *uploadLeases) forgetMade(tokenHash string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.made, tokenHash)
}

// UploadBegun is told that this node begins to serve an upload for the
// session of a token, and returns what is to be called - once; more calls do
// nothing - when the upload has ended, however it ended. While an upload is
// in flight Run keeps the session's upload lease in the replicated state,
// which is what makes a session that reaches its timeout locked instead of
// ended, on every node (ADR-0027).
//
// It decides nothing, writes nothing itself and waits for no lease: the
// lease is written off the request's own time. A token that names no session
// is told of to nobody, and where sessions never lock there is no lease to
// keep.
func (s *SessionService) UploadBegun(ctx context.Context, token string) (ended func()) {
	nothing := func() {}
	if s.lockMax <= 0 || token == "" || len(token) > 4*sessionTokenSize {
		return nothing
	}
	stored, err := s.sessions.GetSession(ctx, hashToken(token))
	if err != nil || notASession(stored) {
		return nothing
	}
	key := uploadLeaseKey(stored.User, stored.CreatedAt)
	now := s.clock.Now()
	l := &s.leases
	l.mu.Lock()
	if l.kept == nil {
		l.kept = map[string]*keptLease{}
	}
	lease, ok := l.kept[key]
	if !ok {
		lease = &keptLease{user: stored.User, createdAt: stored.CreatedAt}
		l.kept[key] = lease
	}
	lease.inFlight++
	lease.touched, lease.active = true, now
	soon := lease.wanted(now, s.uploadGrace)
	l.mu.Unlock()
	if soon {
		// No lease of this session is known to be there, or the one that
		// is would lapse before two more ticks have passed: it is written
		// now. A session that reaches its timeout in between has an upload
		// to show for itself. Run is told, and nobody waits for it.
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			lease.inFlight--
			lease.touched, lease.active = true, s.clock.Now()
		})
	}
}

// wanted reports whether the lease is to be written without waiting for the
// tick: none was written, or the one that was lasts less than two thirds of
// a grace from now - two ticks, of which one may be late.
func (k *keptLease) wanted(now time.Time, grace time.Duration) bool {
	return k.expires.IsZero() || k.expires.Sub(now) < grace*2/3
}

// Run keeps the upload leases of the sessions this node serves uploads for,
// until ctx is cancelled. Every third of a grace - at every tick, whenever
// that tick comes - it writes the lease of each session that has an upload
// in flight, or had one begin or end since the tick before; and when an
// upload begins for a session whose lease is not there or about to lapse, it
// writes that lease at once. For a session that uploads that is one entry of
// the replicated log in a third of a grace from each node it uploads
// through, at times one more - and none for a request.
//
// A lease lasts one grace from when it was written while an upload is in
// flight, and one grace from the end of the last upload when none is. So a
// session that is locked ends one grace after its last upload did, and
// sooner only if the lease could not be written once more after that upload
// ended: then when the lease written while it was in flight lapses, which is
// between two thirds of a grace and a whole one after the upload ended for
// as long as ticks come and writes succeed.
//
// The same ticks remove the records of the sessions this node began, once
// each has ended: see removeEnded.
func (s *SessionService) Run(ctx context.Context) {
	// A grace is seconds at the least; the bound is for a ticker's sake,
	// which takes no interval of nothing.
	ticker := time.NewTicker(max(s.uploadGrace/3, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.keepLeases(ctx, true)
		case <-s.leases.wake:
			s.keepLeases(ctx, false)
		}
	}
}

// keepLeases is one pass of Run: at a tick it writes every lease there is
// something to write for, woken it writes the leases that cannot wait for
// the tick. The whole pass has the time of one tick: a write that takes
// longer is of no use any more.
func (s *SessionService) keepLeases(ctx context.Context, tick bool) {
	ctx, cancel := context.WithTimeout(ctx, max(s.uploadGrace/3, time.Millisecond))
	defer cancel()
	l := &s.leases
	l.mu.Lock()
	now := s.clock.Now()
	var due []string
	for key, lease := range l.kept {
		if (lease.inFlight > 0 || lease.touched) && (tick || lease.wanted(now, s.uploadGrace)) {
			due = append(due, key)
		}
	}
	l.mu.Unlock()

	for _, key := range due {
		l.mu.Lock()
		lease, ok := l.kept[key]
		if !ok {
			l.mu.Unlock()
			continue
		}
		// How long the lease lasts is said when it is written, not when
		// the pass began: a grace from now while an upload is in flight,
		// and a grace from when the last one ended when none is - a
		// session is not kept longer for the keeper being late.
		from := s.clock.Now()
		if lease.inFlight <= 0 {
			from = lease.active
		}
		written := ports.Session{TokenHash: key, User: lease.user, CreatedAt: lease.createdAt, ExpiresAt: from.UTC().Add(s.uploadGrace)}
		// What begins or ends from here on is not in this write.
		lease.touched = false
		l.mu.Unlock()
		if !s.clock.Now().Before(written.ExpiresAt) {
			// It would have lapsed before it was written.
			continue
		}
		err := s.sessions.PutSession(ctx, written, audit.Record{})
		l.mu.Lock()
		switch {
		case err == nil, errors.Is(err, domain.ErrEntityNotFound):
			// Written - or the user is gone, and with the user every
			// session there was a lease to keep for.
			lease.expires = written.ExpiresAt
		default:
			// Not written: there is something to write at the next tick.
			lease.touched = true
		}
		l.mu.Unlock()
	}

	l.mu.Lock()
	now = s.clock.Now()
	for key, lease := range l.kept {
		if lease.inFlight <= 0 && now.Sub(lease.active) >= s.uploadGrace {
			// Whatever was written for it has lapsed, or could not be
			// written for as long as it would have lasted.
			delete(l.kept, key)
		}
	}
	l.mu.Unlock()
	if tick {
		// After the leases: a session with an upload in flight on this
		// node is judged with its lease written.
		s.removeEnded(ctx)
	}
}

// removeEnded deletes the records of the sessions this node began that have
// ended, a tick after their timeout: those that are neither active nor
// locked then. Sweep keeps a record until no session could be locked any
// more, because the store cannot be asked which sessions are - so without
// this every session that ever was would be kept that long. A session a node
// began and forgot - it was restarted - is left to Sweep. A locked one is
// looked at again at every tick, and removed when its lock has ended.
//
// An ended session is ended for good: it cannot upload, so no lease of it is
// written again. Its lease, when that has lapsed, goes with it.
func (s *SessionService) removeEnded(ctx context.Context) {
	now := s.clock.Now()
	l := &s.leases
	l.mu.Lock()
	var past []string
	for hash, expiresAt := range l.made {
		if len(past) >= madeChecked {
			break
		}
		if now.Sub(expiresAt) >= s.uploadGrace/3 {
			past = append(past, hash)
		}
	}
	l.mu.Unlock()
	for _, hash := range past {
		stored, err := s.sessions.GetSession(ctx, hash)
		if errors.Is(err, domain.ErrEntityNotFound) {
			l.forgetMade(hash)
			continue
		}
		if err != nil {
			continue
		}
		state, err := s.stateOf(ctx, stored, s.clock.Now())
		if err != nil || state != sessionEnded {
			continue
		}
		if err := s.sessions.DeleteSession(ctx, hash, audit.Record{}); err != nil {
			continue
		}
		l.forgetMade(hash)
		s.unlocks.forget(hash)
		key := uploadLeaseKey(stored.User, stored.CreatedAt)
		if lease, err := s.sessions.GetSession(ctx, key); err == nil && !s.clock.Now().Before(lease.ExpiresAt) {
			// Not an error to report: the sweep removes what stays.
			_ = s.sessions.DeleteSession(ctx, key, audit.Record{})
		}
	}
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
	s.unlocks.forget(hash)
	s.leases.forgetMade(hash)
	return nil
}

// Sweep removes the sessions that have ended. A node calls it now and then;
// an ended session is refused whether or not it has been swept.
//
// A session past its timeout may be locked and not ended, for as long as
// sessions can be locked, and the store cannot be asked which are: only
// records older than that are removed here. The node that began a session
// removes its record sooner, once it has ended (removeEnded); what is left
// to this sweep are the sessions of nodes that were restarted, and the marks
// kept among the sessions - upload leases, password changes - which are
// judged by their own times and mean nothing once those have passed.
func (s *SessionService) Sweep(ctx context.Context) (int, error) {
	removed, err := s.sessions.DeleteExpiredSessions(ctx, s.clock.Now().Add(-s.lockMax))
	if err != nil {
		return 0, fmt.Errorf("session.Sweep: %w", err)
	}
	return removed, nil
}
