package app_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// The bounds the tests of the lock run with: the defaults but for the
// longest a session is locked, which a test must be able to walk to.
const (
	lockLifetime = 30 * time.Minute
	lockGrace    = 90 * time.Second
	lockMax      = 20 * time.Minute
)

// sessionStore is the session store of a fixture, with what a test needs to
// know of how it was used: how many leases were written, and a way to make
// the writing of leases fail.
type sessionStore struct {
	ports.SessionStore
	leases atomic.Int64
	// failing, when set, is what the writing of a lease answers.
	failing atomic.Pointer[error]
	// wrote, when not nil, is told of every lease that was written.
	wrote chan struct{}
}

func (s *sessionStore) PutSession(ctx context.Context, session ports.Session, record audit.Record) error {
	if !strings.HasPrefix(session.TokenHash, "upload-lease:") {
		return s.SessionStore.PutSession(ctx, session, record)
	}
	if failing := s.failing.Load(); failing != nil {
		return *failing
	}
	if session.CSRFHash != "" || record.Actor != "" {
		return fmt.Errorf("a lease with a CSRF hash or an audit record: %+v, %+v", session, record)
	}
	err := s.SessionStore.PutSession(ctx, session, record)
	if err == nil {
		s.leases.Add(1)
		if s.wrote != nil {
			s.wrote <- struct{}{}
		}
	}
	return err
}

// lockFixture is the session use case with bounds of a test's choosing, a
// user "alice" who has signed in from where, and the password she did it
// with.
type lockFixture struct {
	*iamFixture
	store    *sessionStore
	sessions *app.SessionService
	signed   app.SignedIn
	where    iam.Session
	password string
}

// newLockFixture signs alice, who may read and write objects, in at testNow.
func newLockFixture(t *testing.T, tune func(*app.SessionServiceConfig)) *lockFixture {
	t.Helper()
	ctx := t.Context()
	f := &lockFixture{iamFixture: newIAMFixture(t), where: session("198.51.100.7")}
	// Not a constant: built so that no tool takes it for a credential that
	// was left in the source.
	f.password = strings.Join([]string{"correct", "horse", "battery", "staple"}, " ")
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: f.password}); err != nil {
		t.Fatal(err)
	}
	provider, err := app.NewLocalIdentityProvider(f.iamFixture.store, f.hasher, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.store = &sessionStore{SessionStore: f.iamFixture.store}
	cfg := app.SessionServiceConfig{
		Sessions: f.store, Users: f.iamFixture.store, Provider: provider, Auditor: f.auditor, Clock: f.clock, Entropy: rand.Reader,
		Lifetime: lockLifetime, UploadGrace: lockGrace, LockMax: lockMax,
	}
	if tune != nil {
		tune(&cfg)
	}
	f.sessions = app.NewSessionService(cfg)
	if f.signed, err = f.sessions.SignIn(ctx, "alice", f.password, f.where); err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	return f
}

// state says what the session of a token is now: "active", "locked" or
// "ended", as Authenticate and Status both see it.
func (f *lockFixture) state(t *testing.T, token string) string {
	t.Helper()
	ctx := t.Context()
	principal, err := f.sessions.Authenticate(ctx, token, "", false, f.where)
	status, statusErr := f.sessions.Status(ctx, token)
	switch {
	case errors.Is(err, domain.ErrAccessDenied):
		if !errors.Is(statusErr, domain.ErrAccessDenied) || errors.Is(err, domain.ErrSessionLocked) {
			t.Fatalf("Authenticate = %v, Status = %+v, %v", err, status, statusErr)
		}
		return "ended"
	case err != nil || statusErr != nil:
		t.Fatalf("Authenticate = %v, Status = %v", err, statusErr)
	case principal.LockedSession != status.Locked || principal.User != "alice" || principal.Root || principal.AccessKeyID != "":
		t.Fatalf("Authenticate = %+v where Status = %+v", principal, status)
	}
	if status.Locked {
		return "locked"
	}
	return "active"
}

// expect fails the test unless the session of the fixture is in a state.
func (f *lockFixture) expect(t *testing.T, want, when string) {
	t.Helper()
	if got := f.state(t, f.signed.Token); got != want {
		t.Fatalf("%s the session is %s, want %s", when, got, want)
	}
}

// upload begins an upload with a token and has the keeper make the pass it
// makes when it is woken - if the upload woke it.
func (f *lockFixture) upload(t *testing.T, token string) (ended func()) {
	t.Helper()
	ended = f.sessions.UploadBegun(t.Context(), token)
	f.sessions.WokenUploadLeases(t.Context())
	return ended
}

// stored reports whether the record of a session is still in the store.
func (f *lockFixture) stored(t *testing.T, token string) bool {
	t.Helper()
	_, err := f.iamFixture.store.GetSession(t.Context(), app.HashToken(token))
	if err != nil && !errors.Is(err, domain.ErrEntityNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// pass moves the clock on by d in steps of a tick of the keeper, which makes
// its pass at each.
func (f *lockFixture) pass(t *testing.T, d time.Duration) {
	t.Helper()
	const tick = lockGrace / 3
	for ; d >= tick; d -= tick {
		f.clock.Advance(tick)
		f.sessions.KeepUploadLeases(t.Context())
	}
	f.clock.Advance(d)
}

// TestSessionLock: what a session is - active, locked or ended - follows
// from its record, from the upload lease and from the clock (ADR-0027).
func TestSessionLock(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		tune func(*app.SessionServiceConfig)
		run  func(t *testing.T, f *lockFixture)
	}{
		"active until the timeout, uploading or not": {run: func(t *testing.T, f *lockFixture) {
			f.expect(t, "active", "at sign-in")
			defer f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime-time.Second)
			f.expect(t, "active", "a second before the timeout")
			status, err := f.sessions.Status(t.Context(), f.signed.Token)
			if err != nil || status.Locked || !status.ExpiresAt.Equal(testNow.Add(lockLifetime)) || !status.LockEndsAt.Equal(testNow.Add(lockLifetime+lockMax)) {
				t.Errorf("Status = %+v, %v", status, err)
			}
			if !f.signed.ExpiresAt.Equal(testNow.Add(lockLifetime)) || !f.signed.CookieExpires.Equal(testNow.Add(lockLifetime+lockMax)) {
				t.Errorf("signed in until %v, the cookie until %v", f.signed.ExpiresAt, f.signed.CookieExpires)
			}
		}},
		"locked at the timeout with an upload in flight": {run: func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			defer f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime)
			f.expect(t, "locked", "at the timeout, uploading,")
			// The lock is not full authority, and not none: the authorizer
			// says which, of the principal this use case returns.
			principal, err := f.sessions.Authenticate(ctx, f.signed.Token, f.signed.CSRFToken, true, f.where)
			if err != nil || !principal.LockedSession || len(principal.Policies) == 0 {
				t.Fatalf("Authenticate = %+v, %v", principal, err)
			}
			object := iam.Resource{Bucket: "b", Key: "k"}
			if err := f.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObject, Resource: object}); err != nil {
				t.Errorf("a locked session storing an object: %v", err)
			}
			if err := f.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionGetObject, Resource: object}); !errors.Is(err, domain.ErrSessionLocked) {
				t.Errorf("a locked session reading an object: %v", err)
			}
			// The CSRF token is asked for as ever.
			if _, err := f.sessions.Authenticate(ctx, f.signed.Token, "", true, f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("a change by a locked session without the CSRF token = %v", err)
			}
			status, err := f.sessions.Status(ctx, f.signed.Token)
			if err != nil || !status.Locked || !status.ExpiresAt.Equal(testNow.Add(lockLifetime)) || !status.LockEndsAt.Equal(testNow.Add(lockLifetime+lockMax)) {
				t.Errorf("Status = %+v, %v", status, err)
			}
			// It stays locked for as long as it uploads, and is not swept.
			f.pass(t, lockMax/2)
			if removed, err := f.sessions.Sweep(ctx); err != nil || removed != 0 {
				t.Errorf("Sweep = %d, %v", removed, err)
			}
			f.expect(t, "locked", "half the cap later, still uploading,")
		}},
		"ended at the timeout without an upload": {run: func(t *testing.T, f *lockFixture) {
			f.pass(t, lockLifetime-time.Second)
			f.expect(t, "active", "a second before the timeout")
			f.clock.Advance(time.Second)
			f.expect(t, "ended", "at the timeout, with nothing in flight,")
			if f.store.leases.Load() != 0 {
				t.Errorf("%d leases were written for a session that uploaded nothing", f.store.leases.Load())
			}
		}},
		"ended at the timeout when its uploads ended long before": {run: func(t *testing.T, f *lockFixture) {
			f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime)
			f.expect(t, "ended", "at the timeout, having uploaded half an hour ago,")
		}},
		"ended a grace after its last upload at the most, two thirds of one at the least": {run: func(t *testing.T, f *lockFixture) {
			f.pass(t, lockLifetime-10*time.Second)
			ended := f.upload(t, f.signed.Token) // the lease is written now
			f.clock.Advance(10 * time.Second)
			f.expect(t, "locked", "at the timeout, uploading,")
			ended()
			// The keeper makes its passes: the first after the upload ended
			// writes the lease once more, to last a grace from the end of
			// the upload - not from that pass - and none writes after it.
			f.pass(t, lockGrace-time.Second)
			f.expect(t, "locked", "a second less than a grace after the upload")
			f.clock.Advance(time.Second)
			f.expect(t, "ended", "a grace after the upload")
			if n := f.store.leases.Load(); n != 2 {
				t.Errorf("%d leases were written, want 2", n)
			}
		}},
		"ended two thirds of a grace after its last upload when the keeper wrote no more": {run: func(t *testing.T, f *lockFixture) {
			f.pass(t, lockLifetime-lockGrace/3+time.Second)
			ended := f.upload(t, f.signed.Token)
			f.clock.Advance(lockGrace/3 - time.Second)
			f.expect(t, "locked", "at the timeout, uploading,")
			ended()
			// No pass of the keeper: the node is busy, or gone.
			f.clock.Advance(lockGrace*2/3 - time.Second)
			f.expect(t, "locked", "a second less than two thirds of a grace after the upload")
			f.clock.Advance(2 * time.Second)
			f.expect(t, "ended", "two thirds of a grace after the upload")
			// A pass that comes now writes nothing: the lease would have
			// lapsed before it was written.
			f.clock.Advance(lockGrace / 3)
			f.sessions.KeepUploadLeases(t.Context())
			if n := f.store.leases.Load(); n != 1 {
				t.Errorf("%d leases were written, want 1", n)
			}
		}},
		"ended at the cap with an upload in flight": {run: func(t *testing.T, f *lockFixture) {
			defer f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime+lockMax-time.Second)
			f.expect(t, "locked", "a second before the cap")
			f.clock.Advance(time.Second)
			f.expect(t, "ended", "at the cap, still uploading,")
			// The node that began it removes its record at the next tick;
			// the lease of the upload that goes on stays, and locks nothing.
			if !f.stored(t, f.signed.Token) {
				t.Fatal("the record is gone before a tick has passed")
			}
			f.sessions.KeepUploadLeases(t.Context())
			if f.stored(t, f.signed.Token) || f.sessions.MadeSessions() != 0 {
				t.Errorf("the record of the ended session is still there, or remembered (%d)", f.sessions.MadeSessions())
			}
			if _, err := f.iamFixture.store.GetSession(t.Context(), app.UploadLeaseKey("alice", testNow)); err != nil {
				t.Errorf("the lease of the upload in flight: %v", err)
			}
			f.expect(t, "ended", "its record removed,")
		}},
		"the upload of another session of the user does not lock this one": {run: func(t *testing.T, f *lockFixture) {
			f.clock.Advance(time.Second)
			other, err := f.sessions.SignIn(t.Context(), "alice", f.password, f.where)
			if err != nil {
				t.Fatal(err)
			}
			defer f.upload(t, other.Token)()
			f.pass(t, lockLifetime-time.Second)
			f.expect(t, "ended", "at its timeout, the other session uploading,")
			if got := f.state(t, other.Token); got != "active" {
				t.Errorf("the session that uploads is %s a second before its timeout", got)
			}
			f.clock.Advance(time.Second)
			if got := f.state(t, other.Token); got != "locked" {
				t.Errorf("the session that uploads is %s at its timeout", got)
			}
		}},
		"the user is deleted while it is locked": {run: func(t *testing.T, f *lockFixture) {
			defer f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime)
			f.expect(t, "locked", "at the timeout, uploading,")
			if err := f.iam.DeleteUser(t.Context(), f.root, "alice"); err != nil {
				t.Fatal(err)
			}
			f.expect(t, "ended", "its user deleted,")
			// The lease went with the user, and the keeper cannot bring it
			// back: there is no user to write it for.
			f.pass(t, lockGrace)
			if n := f.iamFixture.store.Sessions(); n != 0 {
				t.Errorf("%d session records outlived their user", n)
			}
		}},
		"the user is disabled while it is locked": {run: func(t *testing.T, f *lockFixture) {
			defer f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime)
			f.expect(t, "locked", "at the timeout, uploading,")
			if _, err := f.iam.UpdateUser(t.Context(), f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Disabled: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.sessions.Authenticate(t.Context(), f.signed.Token, "", false, f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("the locked session of a disabled user = %v", err)
			}
			// And no password unlocks it.
			if _, err := f.sessions.Unlock(t.Context(), f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("unlocking the session of a disabled user = %v", err)
			}
		}},
		"signing out of a locked session": {run: func(t *testing.T, f *lockFixture) {
			defer f.upload(t, f.signed.Token)()
			f.pass(t, lockLifetime)
			f.expect(t, "locked", "at the timeout, uploading,")
			if err := f.sessions.SignOut(t.Context(), f.signed.Token, f.where); err != nil {
				t.Fatal(err)
			}
			f.expect(t, "ended", "signed out,")
			// The upload goes on, and so does its lease - which is of no
			// use to anybody: there is no session it could lock.
			f.pass(t, lockGrace)
			f.expect(t, "ended", "signed out, a grace later,")
			if _, err := f.sessions.Unlock(t.Context(), f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
				t.Errorf("unlocking a session that was signed out = %v", err)
			}
			trail := f.trail(t)
			if last := trail[len(trail)-1]; last.Action != string(app.ActionSignOut) || last.Actor != "user/alice" {
				t.Errorf("the last record = %+v", last)
			}
		}},
		"sessions that never lock end as they always did": {
			tune: func(cfg *app.SessionServiceConfig) { cfg.LockMax = -1 },
			run: func(t *testing.T, f *lockFixture) {
				ctx := t.Context()
				defer f.upload(t, f.signed.Token)()
				if !f.signed.CookieExpires.Equal(f.signed.ExpiresAt) {
					t.Errorf("the cookie is kept until %v, the session ends at %v", f.signed.CookieExpires, f.signed.ExpiresAt)
				}
				status, err := f.sessions.Status(ctx, f.signed.Token)
				if err != nil || status.Locked || !status.LockEndsAt.IsZero() {
					t.Errorf("Status = %+v, %v", status, err)
				}
				f.pass(t, lockLifetime-time.Second)
				f.expect(t, "active", "a second before the timeout")
				f.clock.Advance(time.Second)
				f.expect(t, "ended", "at the timeout, uploading, where sessions never lock,")
				if f.store.leases.Load() != 0 || f.sessions.KeptUploadLeases() != 0 {
					t.Errorf("%d leases were written and %d are kept where sessions never lock", f.store.leases.Load(), f.sessions.KeptUploadLeases())
				}
				// A lease that another node wrote - one that locks - is not
				// read either.
				lease := ports.Session{TokenHash: app.UploadLeaseKey("alice", testNow), User: "alice", CreatedAt: testNow, ExpiresAt: f.clock.Now().Add(time.Hour)}
				if err := f.iamFixture.store.PutSession(ctx, lease, audit.Record{}); err != nil {
					t.Fatal(err)
				}
				f.expect(t, "ended", "with the lease of another node,")
				if _, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
					t.Errorf("unlocking an ended session = %v", err)
				}
				f.clock.Advance(time.Second)
				if removed, err := f.sessions.Sweep(ctx); err != nil || removed != 1 {
					t.Errorf("Sweep = %d, %v", removed, err)
				}
			},
		},
		"the defaults": {
			tune: func(cfg *app.SessionServiceConfig) { cfg.Lifetime, cfg.UploadGrace, cfg.LockMax = 0, 0, 0 },
			run: func(t *testing.T, f *lockFixture) {
				if !f.signed.ExpiresAt.Equal(testNow.Add(app.DefaultSessionLifetime)) ||
					!f.signed.CookieExpires.Equal(testNow.Add(app.DefaultSessionLifetime+app.DefaultSessionLockMax)) {
					t.Errorf("signed in until %v, the cookie until %v", f.signed.ExpiresAt, f.signed.CookieExpires)
				}
				ended := f.sessions.UploadBegun(t.Context(), f.signed.Token)
				f.sessions.KeepUploadLeases(t.Context())
				lease, err := f.iamFixture.store.GetSession(t.Context(), app.UploadLeaseKey("alice", testNow))
				if err != nil || !lease.ExpiresAt.Equal(testNow.Add(app.DefaultSessionUploadGrace)) {
					t.Errorf("the lease = %+v, %v", lease, err)
				}
				ended()
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, newLockFixture(t, tt.tune))
		})
	}
}

// TestAnUploadLeaseSignsNobodyIn: the lease is a record among the sessions,
// and no credential. No token is looked up under its key; whoever sends the
// key itself, or any part of it, is refused; and a lease that lives says
// nothing of a session that is not there.
func TestAnUploadLeaseSignsNobodyIn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newLockFixture(t, nil)
	defer f.upload(t, f.signed.Token)()
	key := app.UploadLeaseKey("alice", testNow)
	lease, err := f.iamFixture.store.GetSession(ctx, key)
	if err != nil || lease.User != "alice" || lease.CSRFHash != "" || !lease.CreatedAt.Equal(testNow) || !lease.ExpiresAt.Equal(testNow.Add(lockGrace)) {
		t.Fatalf("the lease = %+v, %v", lease, err)
	}
	if f.iamFixture.store.Sessions() != 2 {
		t.Fatalf("%d session records, want the session and its lease", f.iamFixture.store.Sessions())
	}
	base := f.store.leases.Load()

	tokens := map[string]string{
		"the key of the lease":                     key,
		"the hash in the key":                      strings.TrimPrefix(key, "upload-lease:"),
		"the prefix":                               "upload-lease:",
		"the hash of the key":                      app.HashToken(key),
		"the user and the time":                    "alice\n" + testNow.Format(time.RFC3339Nano),
		"the hash of the session":                  app.HashToken(f.signed.Token),
		"the key of the mark of a password change": app.PasswordChangeKey("alice"),
		"nothing":                                  "",
	}
	for name, token := range tokens {
		for _, mutating := range []bool{false, true} {
			if _, err := f.sessions.Authenticate(ctx, token, "", mutating, f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("%s as a token (changing: %v) = %v, want a refusal", name, mutating, err)
			}
		}
		if _, err := f.sessions.Status(ctx, token); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("Status with %s as a token = %v", name, err)
		}
		if _, err := f.sessions.Unlock(ctx, token, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("Unlock with %s as a token = %v", name, err)
		}
		if err := f.sessions.SignOut(ctx, token, f.where); err != nil {
			t.Errorf("SignOut with %s as a token = %v", name, err)
		}
		// And no upload is counted for it.
		f.sessions.UploadBegun(ctx, token)()
	}
	if f.sessions.WokenUploadLeases(ctx) {
		t.Error("a token that names no session woke the keeper")
	}
	if f.store.leases.Load() != base || f.sessions.KeptUploadLeases() != 1 || f.iamFixture.store.Sessions() != 2 {
		t.Errorf("tokens that name no session wrote %d leases; %d are kept, %d records stored",
			f.store.leases.Load()-base, f.sessions.KeptUploadLeases(), f.iamFixture.store.Sessions())
	}

	// A record that has the form of a lease is no session, wherever a store
	// keeps it: not under the hash of a token either.
	stray := "stray-token"
	if err := f.iamFixture.store.PutSession(ctx, ports.Session{
		TokenHash: app.HashToken(stray), User: "alice", CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour),
	}, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Authenticate(ctx, stray, "", false, f.where); err != nil {
		t.Fatalf("a session under the hash of its token: %v", err)
	}
	misplaced := &misplacedLease{SessionStore: f.iamFixture.store, under: app.HashToken(stray), lease: lease}
	strict := app.NewSessionService(app.SessionServiceConfig{
		Sessions: misplaced, Users: f.iamFixture.store, Auditor: f.auditor, Clock: f.clock, Entropy: rand.Reader,
	})
	if _, err := strict.Authenticate(ctx, stray, "", false, f.where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a lease that a store handed out for a token = %v, want a refusal", err)
	}
	if _, err := strict.Unlock(ctx, stray, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
		t.Errorf("unlocking a lease = %v", err)
	}
	strict.UploadBegun(ctx, stray)()
	if strict.KeptUploadLeases() != 0 {
		t.Error("an upload was counted for a lease")
	}
	// Nor is the other mark kept among the sessions, that of a change of
	// password, a session.
	misplaced.lease = ports.Session{
		TokenHash: app.PasswordChangeKey("alice"), User: "alice", CreatedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour),
	}
	if _, err := strict.Authenticate(ctx, stray, "", false, f.where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("the mark of a password change that a store handed out for a token = %v, want a refusal", err)
	}
	if _, err := strict.Status(ctx, stray); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Status of the mark of a password change = %v", err)
	}

	// The session ended - signed out - and its lease lives: nobody is
	// signed in by that.
	if err := f.sessions.SignOut(ctx, f.signed.Token, f.where); err != nil {
		t.Fatal(err)
	}
	f.pass(t, lockLifetime)
	if live, err := f.iamFixture.store.GetSession(ctx, key); err != nil || !f.clock.Now().Before(live.ExpiresAt) {
		t.Fatalf("the lease = %+v, %v; want one that lives", live, err)
	}
	f.expect(t, "ended", "with a lease and no session,")
}

// misplacedLease is a store that answers the question for one token hash
// with an upload lease - which the real stores cannot.
type misplacedLease struct {
	ports.SessionStore
	under string
	lease ports.Session
}

func (m *misplacedLease) GetSession(ctx context.Context, tokenHash string) (ports.Session, error) {
	if tokenHash == m.under {
		return m.lease, nil
	}
	return m.SessionStore.GetSession(ctx, tokenHash)
}

// TestUploadLeaseKeyIsNoTokenHash: whatever the user, the time and the
// token, the key of a lease is not what a token is stored under - on this
// release, and on every one before it, that is the SHA-256 of the token in
// hexadecimal digits and nothing else.
func TestUploadLeaseKeyIsNoTokenHash(t *testing.T) {
	t.Parallel()
	tokenHash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	property := func(user, token string, seconds int64, nanos uint32) bool {
		at := time.Unix(seconds%(1<<34), int64(nanos%1e9))
		key := app.UploadLeaseKey(user, at)
		hash := app.HashToken(token)
		return tokenHash.MatchString(hash) && !tokenHash.MatchString(key) && key != hash &&
			app.HashToken(key) != key && strings.HasPrefix(key, "upload-lease:") && len(key) == len("upload-lease:")+64 &&
			// It is of the sign-in, whatever zone the time is told in.
			key == app.UploadLeaseKey(user, at.In(time.FixedZone("elsewhere", 3600)))
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
	// Another user, or another moment of sign-in, is another lease.
	if app.UploadLeaseKey("alice", testNow) == app.UploadLeaseKey("bob", testNow) ||
		app.UploadLeaseKey("alice", testNow) == app.UploadLeaseKey("alice", testNow.Add(time.Nanosecond)) {
		t.Error("two sessions share a lease")
	}
}

// TestUploadLeaseKeeper: what keeping the leases costs the replicated log -
// an entry in a third of the grace for a session that uploads, however many
// requests it makes - and that the keeper forgets, retries and stops.
func TestUploadLeaseKeeper(t *testing.T) {
	t.Parallel()
	const tick = lockGrace / 3
	tests := map[string]func(t *testing.T, f *lockFixture){
		"a thousand small uploads in a minute": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			for i := range 1000 {
				if at := time.Duration(i) * 60 * time.Millisecond; at > 0 && at%tick == 0 {
					f.sessions.KeepUploadLeases(ctx) // the tick
				}
				f.upload(t, f.signed.Token)()
				f.clock.Advance(60 * time.Millisecond)
			}
			// One when the first began, one at the tick.
			if n := f.store.leases.Load(); n != 2 {
				t.Errorf("%d leases were written in a minute of uploads, want 2: one in %v", n, tick)
			}
		},
		"one upload of an hour": func(t *testing.T, f *lockFixture) {
			defer f.upload(t, f.signed.Token)()
			f.pass(t, time.Hour)
			if n, most := f.store.leases.Load(), int64(time.Hour/tick)+1; n != most {
				t.Errorf("%d leases were written in an hour, want %d", n, most)
			}
		},
		"uploads that begin all the time wake the keeper no more than a lease needs": func(t *testing.T, f *lockFixture) {
			// An upload begins every second for ten minutes and lasts half
			// of one; the ticks come as they should.
			for i := range 600 {
				if i > 0 && time.Duration(i)*time.Second%tick == 0 {
					f.sessions.KeepUploadLeases(t.Context())
				}
				ended := f.upload(t, f.signed.Token)
				f.clock.Advance(500 * time.Millisecond)
				ended()
				f.clock.Advance(500 * time.Millisecond)
			}
			// One at each tick, and one when the first began: no upload
			// that found a lease good for two ticks more asked for another.
			if n, want := f.store.leases.Load(), int64(10*time.Minute/tick); n != want {
				t.Errorf("%d leases were written in ten minutes, want %d", n, want)
			}
		},
		"many uploads at once are one lease": func(t *testing.T, f *lockFixture) {
			var all []func()
			for range 50 {
				all = append(all, f.upload(t, f.signed.Token))
			}
			if n := f.store.leases.Load(); n != 1 || f.sessions.KeptUploadLeases() != 1 {
				t.Errorf("%d leases were written, %d are kept", n, f.sessions.KeptUploadLeases())
			}
			for _, ended := range all {
				ended()
				ended() // told twice: counted once
			}
			// Nothing is in flight: one more write, for the uploads that
			// ended since the last, and then none.
			f.pass(t, 10*lockGrace)
			if n := f.store.leases.Load(); n != 2 || f.sessions.KeptUploadLeases() != 0 {
				t.Errorf("%d leases were written, %d are still kept", n, f.sessions.KeptUploadLeases())
			}
			// What was told twice did not make the count negative: the next
			// upload is in flight until it ends.
			ended := f.upload(t, f.signed.Token)
			f.pass(t, 2*lockGrace)
			if f.sessions.KeptUploadLeases() != 1 {
				t.Error("an upload in flight is not kept a lease for")
			}
			ended()
		},
		"a lease that cannot be written is written at the next pass": func(t *testing.T, f *lockFixture) {
			unavailable := fmt.Errorf("raftmeta.PutSession: %w", domain.ErrUnavailable)
			f.store.failing.Store(&unavailable)
			defer f.upload(t, f.signed.Token)()
			f.sessions.KeepUploadLeases(t.Context())
			if f.store.leases.Load() != 0 {
				t.Fatal("a lease was written by a store that fails")
			}
			// Nothing is known to be written: the next upload asks again.
			f.sessions.UploadBegun(t.Context(), f.signed.Token)()
			if !f.sessions.WokenUploadLeases(t.Context()) {
				t.Error("an upload that began with no lease written did not wake the keeper")
			}
			f.store.failing.Store(nil)
			f.sessions.KeepUploadLeases(t.Context())
			if f.store.leases.Load() != 1 {
				t.Errorf("%d leases were written once the store answered", f.store.leases.Load())
			}
		},
		"the write after the last upload ended is tried again when it fails": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			ended := f.upload(t, f.signed.Token)
			f.clock.Advance(tick)
			f.sessions.KeepUploadLeases(ctx) // in flight: a grace from now
			f.clock.Advance(10 * time.Second)
			ended()
			endedAt := f.clock.Now()
			// The tick that would say "a grace from the end" fails...
			unavailable := fmt.Errorf("raftmeta.PutSession: %w", domain.ErrUnavailable)
			f.store.failing.Store(&unavailable)
			f.clock.Advance(tick - 10*time.Second)
			f.sessions.KeepUploadLeases(ctx)
			// ...and the next one writes it, though nothing is in flight and
			// nothing began or ended since.
			f.store.failing.Store(nil)
			f.clock.Advance(tick)
			f.sessions.KeepUploadLeases(ctx)
			lease, err := f.iamFixture.store.GetSession(ctx, app.UploadLeaseKey("alice", testNow))
			if err != nil || !lease.ExpiresAt.Equal(endedAt.Add(lockGrace)) {
				t.Errorf("the lease lasts until %v (%v), want a grace after the upload ended: %v", lease.ExpiresAt, err, endedAt.Add(lockGrace))
			}
		},
		"a store that fails for good is not asked for ever": func(t *testing.T, f *lockFixture) {
			unavailable := fmt.Errorf("raftmeta.PutSession: %w", domain.ErrUnavailable)
			f.store.failing.Store(&unavailable)
			f.upload(t, f.signed.Token)()
			f.pass(t, lockGrace)
			if f.sessions.KeptUploadLeases() != 0 {
				t.Error("the upload that ended a grace ago is still kept a lease for")
			}
		},
		"each session has its own": func(t *testing.T, f *lockFixture) {
			f.clock.Advance(time.Second)
			other, err := f.sessions.SignIn(t.Context(), "alice", f.password, f.where)
			if err != nil {
				t.Fatal(err)
			}
			defer f.upload(t, f.signed.Token)()
			defer f.upload(t, other.Token)()
			if n := f.store.leases.Load(); n != 2 || f.sessions.KeptUploadLeases() != 2 {
				t.Errorf("%d leases were written, %d are kept, want 2 and 2", n, f.sessions.KeptUploadLeases())
			}
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, newLockFixture(t, nil))
		})
	}
}

// TestSessionKeeperEndsWithItsContext: Run writes the lease of an upload
// that begins without waiting for its tick, and returns when its context is
// cancelled.
func TestSessionKeeperEndsWithItsContext(t *testing.T) {
	t.Parallel()
	f := newLockFixture(t, nil)
	f.store.wrote = make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(t.Context())
	var done sync.WaitGroup
	done.Go(func() { f.sessions.Run(ctx) })
	ended := f.sessions.UploadBegun(ctx, f.signed.Token)
	// The tick is half a minute away; the lease is there at once.
	<-f.store.wrote
	if _, err := f.iamFixture.store.GetSession(ctx, app.UploadLeaseKey("alice", testNow)); err != nil {
		t.Errorf("the lease of an upload that began: %v", err)
	}
	ended()
	cancel()
	done.Wait()
	// Told of an upload after its end, it keeps nothing running.
	f.sessions.UploadBegun(t.Context(), f.signed.Token)()
}

// TestSessionUnlock: the password of the session's own user gives a session
// its authority back and new tokens; a wrong one is counted, audited and, at
// the fifth, the end of the session (ADR-0027).
func TestSessionUnlock(t *testing.T) {
	t.Parallel()
	// lock takes the session of a fixture to its timeout with an upload in
	// flight.
	lock := func(t *testing.T, f *lockFixture) {
		t.Helper()
		t.Cleanup(f.upload(t, f.signed.Token))
		f.pass(t, lockLifetime)
		f.expect(t, "locked", "at the timeout, uploading,")
	}
	unlocks := func(t *testing.T, f *lockFixture, outcome audit.Outcome) (n int) {
		t.Helper()
		for _, r := range f.trail(t) {
			if r.Action == string(app.ActionUnlockSession) && r.Outcome == outcome {
				n++
			}
		}
		return n
	}
	tests := map[string]func(t *testing.T, f *lockFixture){
		"the right password": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			lock(t, f)
			f.clock.Advance(7 * time.Second)
			at := f.clock.Now()
			checks := f.hasher.Verifications()
			unlocked, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
			if err != nil {
				t.Fatalf("Unlock: %v", err)
			}
			if unlocked.User != "alice" || len(unlocked.Token) < 40 || len(unlocked.CSRFToken) < 40 ||
				unlocked.Token == f.signed.Token || unlocked.CSRFToken == f.signed.CSRFToken || unlocked.Token == unlocked.CSRFToken ||
				!unlocked.ExpiresAt.Equal(at.Add(lockLifetime)) || !unlocked.CookieExpires.Equal(at.Add(lockLifetime+lockMax)) {
				t.Fatalf("Unlock = %+v", unlocked)
			}
			if f.hasher.Verifications() != checks+1 {
				t.Errorf("unlocking made %d password checks, want 1", f.hasher.Verifications()-checks)
			}
			// The old token is refused, with either CSRF token; the new
			// one has full authority, with its own CSRF token only.
			f.expect(t, "ended", "unlocked, by its old token,")
			for _, csrf := range []string{f.signed.CSRFToken, unlocked.CSRFToken} {
				if _, err := f.sessions.Authenticate(ctx, f.signed.Token, csrf, true, f.where); !errors.Is(err, domain.ErrAccessDenied) {
					t.Errorf("the old token after the unlock = %v", err)
				}
			}
			if _, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
				t.Errorf("unlocking with the old token again = %v", err)
			}
			if got := f.state(t, unlocked.Token); got != "active" {
				t.Fatalf("the unlocked session is %s", got)
			}
			if _, err := f.sessions.Authenticate(ctx, unlocked.Token, f.signed.CSRFToken, true, f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("the new token with the old CSRF token = %v", err)
			}
			principal, err := f.sessions.Authenticate(ctx, unlocked.Token, unlocked.CSRFToken, true, f.where)
			if err != nil || principal.LockedSession {
				t.Fatalf("Authenticate = %+v, %v", principal, err)
			}
			if err := f.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "b", Key: "k"}}); err != nil {
				t.Errorf("an unlocked session reading an object: %v", err)
			}
			// The record keeps the time of sign-in, and neither token.
			stored, err := f.iamFixture.store.GetSession(ctx, app.HashToken(unlocked.Token))
			if err != nil || !stored.CreatedAt.Equal(testNow) || stored.CSRFHash != app.HashToken(unlocked.CSRFToken) {
				t.Errorf("the new record = %+v, %v", stored, err)
			}
			if _, err := f.iamFixture.store.GetSession(ctx, app.HashToken(f.signed.Token)); !errors.Is(err, domain.ErrEntityNotFound) {
				t.Errorf("the old record after the unlock: %v", err)
			}
			// A request sent twice: the second finds the session it names
			// ended - not signed in, which is not "too many attempts".
			if _, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) || errors.Is(err, domain.ErrTooManyAttempts) {
				t.Errorf("the same unlock sent again = %v", err)
			}
			if got := f.state(t, unlocked.Token); got != "active" {
				t.Fatalf("after the unlock was sent again the session is %s", got)
			}
			// So the upload that began under the old token - it is still
			// in flight - keeps the new session locked at its timeout.
			f.pass(t, lockLifetime)
			if got := f.state(t, unlocked.Token); got != "locked" {
				t.Errorf("the unlocked session at its next timeout, the old upload in flight, is %s", got)
			}
			if unlocks(t, f, audit.Succeeded) != 1 || unlocks(t, f, audit.Failed) != 0 {
				t.Errorf("the trail has %d unlocks and %d failed ones", unlocks(t, f, audit.Succeeded), unlocks(t, f, audit.Failed))
			}
		},
		"a wrong password": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			lock(t, f)
			refusals := map[string]string{
				"wrong":     "not her password at all",
				"empty":     "",
				"oversized": strings.Repeat("p", 2000),
				"the token": f.signed.Token,
			}
			var message string
			for name, password := range refusals {
				_, err := f.sessions.Unlock(ctx, f.signed.Token, password, f.where)
				if !errors.Is(err, domain.ErrAccessDenied) || errors.Is(err, domain.ErrCredentialNotFound) || errors.Is(err, domain.ErrSessionLocked) {
					t.Fatalf("Unlock with a password that is %s = %v, want a denial", name, err)
				}
				if message == "" {
					message = err.Error()
				}
				if err.Error() != message {
					t.Errorf("a password that is %s is refused differently: %q vs %q", name, err.Error(), message)
				}
				f.expect(t, "locked", "after a wrong password")
			}
			if n := unlocks(t, f, audit.Failed); n == 0 {
				t.Error("wrong passwords left nothing in the audit trail")
			}
			if f.sessions.UnlockFailuresCounted() != 1 {
				t.Errorf("wrong passwords are counted for %d sessions, want 1", f.sessions.UnlockFailuresCounted())
			}
			// The right one still unlocks: four were wrong, not five. And
			// nothing is counted any more for a token that is gone.
			unlocked, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
			if err != nil {
				t.Fatalf("the right password after four wrong ones: %v", err)
			}
			if f.sessions.UnlockFailuresCounted() != 0 {
				t.Errorf("after the unlock wrong passwords are still counted for %d sessions", f.sessions.UnlockFailuresCounted())
			}
			// The unlocked session has five attempts of its own.
			for i := 1; i < app.UnlockAttempts; i++ {
				if _, err := f.sessions.Unlock(ctx, unlocked.Token, fmt.Sprintf("another guess %d", i), session("203.0.113.40")); !errors.Is(err, domain.ErrAccessDenied) {
					t.Fatalf("guess %d at the unlocked session = %v", i, err)
				}
			}
			if got := f.state(t, unlocked.Token); got != "active" {
				t.Errorf("after four wrong passwords the unlocked session is %s", got)
			}
		},
		"the fifth wrong password ends the session": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			lock(t, f)
			for i := 1; i < app.UnlockAttempts; i++ {
				if _, err := f.sessions.Unlock(ctx, f.signed.Token, fmt.Sprintf("guess %d", i), f.where); !errors.Is(err, domain.ErrAccessDenied) {
					t.Fatalf("guess %d = %v", i, err)
				}
			}
			f.expect(t, "locked", "after four wrong passwords")
			_, err := f.sessions.Unlock(ctx, f.signed.Token, "guess the fifth", f.where)
			// Not signed in any more - and told from a session that had
			// ended before by naming the attempts as well.
			if !errors.Is(err, domain.ErrCredentialNotFound) || !errors.Is(err, domain.ErrTooManyAttempts) || errors.Is(err, domain.ErrAccessDenied) {
				t.Fatalf("the fifth wrong password = %v, want not signed in, for too many attempts", err)
			}
			f.expect(t, "ended", "after five wrong passwords")
			if f.sessions.UnlockFailuresCounted() != 0 || f.sessions.MadeSessions() != 0 {
				t.Errorf("the ended session is still counted for (%d) or remembered (%d)", f.sessions.UnlockFailuresCounted(), f.sessions.MadeSessions())
			}
			_, err = f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
			if !errors.Is(err, domain.ErrCredentialNotFound) || errors.Is(err, domain.ErrTooManyAttempts) {
				t.Errorf("the right password for a session that was ended = %v, want not signed in and nothing of attempts", err)
			}
			var end audit.Record
			for _, r := range f.trail(t) {
				if r.Action == string(app.ActionUnlockSession) && r.Actor == "user/alice" {
					end = r
				}
			}
			if end.Outcome != audit.Failed || !strings.Contains(end.Details["reason"], "the session is ended") || end.SourceIP != "198.51.100.7" {
				t.Errorf("the record of the end = %+v", end)
			}
			// It counted against that session only: another one of the
			// user has its own attempts, and the address is not yet made
			// to wait.
			other, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
			if err != nil {
				t.Fatalf("signing in again: %v", err)
			}
			if _, err := f.sessions.Unlock(ctx, other.Token, "one more guess", f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("a wrong password for another session = %v", err)
			}
			if got := f.state(t, other.Token); got != "active" {
				t.Errorf("the other session is %s", got)
			}
		},
		"an address that keeps failing is made to wait": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			lock(t, f)
			// Failed sign-ins and failed unlocks are one budget.
			for i := range app.SignInAttempts - 2 {
				if _, err := f.sessions.SignIn(ctx, "alice", fmt.Sprintf("guess %d", i), f.where); !errors.Is(err, domain.ErrAccessDenied) {
					t.Fatalf("guess %d = %v", i, err)
				}
			}
			for i := range 2 {
				if _, err := f.sessions.Unlock(ctx, f.signed.Token, fmt.Sprintf("unlock guess %d", i), f.where); !errors.Is(err, domain.ErrAccessDenied) {
					t.Fatalf("unlock guess %d = %v", i, err)
				}
			}
			checks := f.hasher.Verifications()
			_, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
			if !errors.Is(err, domain.ErrTooManyAttempts) || errors.Is(err, domain.ErrAccessDenied) {
				t.Fatalf("the right password after ten failures from the address = %v", err)
			}
			if f.hasher.Verifications() != checks {
				t.Error("the password of an address that must wait was looked at")
			}
			// The session is kept: being made to wait is not a guess.
			f.expect(t, "locked", "its address made to wait,")
			// From elsewhere it unlocks.
			if _, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, session("203.0.113.9")); err != nil {
				t.Errorf("unlocking from another address: %v", err)
			}
		},
		"a session that is not locked": func(t *testing.T, f *lockFixture) {
			ctx := t.Context()
			f.clock.Advance(10 * time.Minute)
			unlocked, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
			if err != nil || unlocked.Token == f.signed.Token || !unlocked.ExpiresAt.Equal(testNow.Add(10*time.Minute+lockLifetime)) {
				t.Fatalf("Unlock = %+v, %v", unlocked, err)
			}
			f.expect(t, "ended", "unlocked, by its old token,")
			if got := f.state(t, unlocked.Token); got != "active" {
				t.Errorf("the session is %s", got)
			}
			// A wrong password leaves an active session active.
			if _, err := f.sessions.Unlock(ctx, unlocked.Token, "wrong", f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("a wrong password = %v", err)
			}
			if got := f.state(t, unlocked.Token); got != "active" {
				t.Errorf("after a wrong password the session is %s", got)
			}
		},
		"a session that has ended": func(t *testing.T, f *lockFixture) {
			f.pass(t, lockLifetime)
			f.expect(t, "ended", "at the timeout")
			checks := f.hasher.Verifications()
			if _, err := f.sessions.Unlock(t.Context(), f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
				t.Errorf("unlocking an ended session = %v", err)
			}
			if f.hasher.Verifications() != checks || unlocks(t, f, audit.Failed) != 0 {
				t.Error("the password of nobody's session was looked at, or counted")
			}
		},
		"a user without a password": func(t *testing.T, f *lockFixture) {
			lock(t, f)
			// The password is taken away: what an identity provider other
			// than the local one would look like to this use case.
			user, err := f.iamFixture.store.GetUser(t.Context(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			user.PasswordHash = ""
			if err := f.iamFixture.store.PutUser(t.Context(), user, false, audit.Record{}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.sessions.Unlock(t.Context(), f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("unlocking the session of a user without a password = %v", err)
			}
			f.expect(t, "locked", "after that")
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLockFixture(t, nil)
			run(t, f)
			// Whatever happened: no password is in the trail or the log.
			for _, r := range f.trail(t) {
				if strings.Contains(fmt.Sprint(r), f.password) {
					t.Fatal("a password is in the audit trail")
				}
			}
			if strings.Contains(f.logs.String(), f.password) {
				t.Fatal("a password is in the log")
			}
		})
	}
}

// TestUploadLeaseNeverLapsesUnderAnUpload: a session with an upload in
// flight has a live lease at every moment, whenever the ticks come - a
// millisecond early, late, or not at all for a while - and whatever its
// uploads did before. A lease that lapsed under an upload would end the
// session, and the console would give up the uploads this is all for.
func TestUploadLeaseNeverLapsesUnderAnUpload(t *testing.T) {
	t.Parallel()
	const tick = lockGrace / 3
	// at moves the clock of a fixture to so many seconds after sign-in.
	at := func(f *lockFixture, seconds float64) {
		target := testNow.Add(time.Duration(seconds * float64(time.Second)))
		f.clock.Advance(target.Sub(f.clock.Now()))
	}
	live := func(t *testing.T, f *lockFixture, when string) {
		t.Helper()
		lease, err := f.iamFixture.store.GetSession(t.Context(), app.UploadLeaseKey("alice", testNow))
		if err != nil || !f.clock.Now().Before(lease.ExpiresAt) {
			t.Fatalf("%s the lease is %+v (%v) at %v: an upload is in flight", when, lease, err, f.clock.Now().Sub(testNow))
		}
	}
	tests := map[string]func(t *testing.T, f *lockFixture){
		"ticks that come a millisecond early": func(t *testing.T, f *lockFixture) {
			// The timeout is at 1800. Upload A is short; a tick comes a
			// hair before a third of a grace has passed since the lease
			// was written; upload B begins and stays.
			at(f, 1700)
			endA := f.upload(t, f.signed.Token)
			at(f, 1701)
			endA()
			at(f, 1729.999)
			f.sessions.KeepUploadLeases(t.Context())
			at(f, 1760)
			f.sessions.KeepUploadLeases(t.Context())
			at(f, 1761)
			defer f.upload(t, f.signed.Token)()
			at(f, 1789.999)
			f.sessions.KeepUploadLeases(t.Context())
			for _, second := range []float64{1795, 1799.999} {
				at(f, second)
				live(t, f, "before the timeout")
				f.expect(t, "active", "before the timeout")
			}
			for second := 1800.0; second < 1900; second++ {
				at(f, second)
				if int(second)%30 == 20 {
					at(f, second-0.001) // every tick a hair early
					f.sessions.KeepUploadLeases(t.Context())
				}
				live(t, f, "past the timeout")
				f.expect(t, "locked", "past the timeout, uploading,")
			}
		},
		"an upload that begins a minute after the last one ended": func(t *testing.T, f *lockFixture) {
			// A ends at 1701. Its lease lasts to 1791. B begins at 1761,
			// stays in flight across the timeout at 1800 - and no tick
			// comes between 1761 and 1795: the keeper is woken instead.
			at(f, 1700)
			endA := f.upload(t, f.signed.Token)
			at(f, 1701)
			endA()
			at(f, 1730)
			f.sessions.KeepUploadLeases(t.Context())
			at(f, 1760)
			f.sessions.KeepUploadLeases(t.Context())
			at(f, 1761)
			endB := f.sessions.UploadBegun(t.Context(), f.signed.Token)
			defer endB()
			if !f.sessions.WokenUploadLeases(t.Context()) {
				t.Fatal("an upload that began half a minute before its session's lease lapses did not wake the keeper")
			}
			for second := 1762.0; second < 2000; second++ {
				at(f, second)
				if second >= 1795 && int(second)%30 == 0 {
					f.sessions.KeepUploadLeases(t.Context())
				}
				live(t, f, "with B in flight")
				want := "active"
				if second >= 1800 {
					want = "locked"
				}
				f.expect(t, want, "with B in flight")
			}
		},
		"ticks that are late by almost a tick": func(t *testing.T, f *lockFixture) {
			at(f, 1790)
			defer f.upload(t, f.signed.Token)()
			next := 1790.0
			for second := 1790.0; second < 2100; second++ {
				at(f, second)
				if second >= next {
					f.sessions.KeepUploadLeases(t.Context())
					next = second + 2*tick.Seconds() - 1
				}
				live(t, f, "with late ticks")
			}
			f.expect(t, "locked", "past the timeout, the ticks late,")
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, newLockFixture(t, nil))
		})
	}
}

// machineClock is the clock of the machine, for the one test that must see what
// a ticker really does.
type machineClock struct{}

func (machineClock) Now() time.Time { return time.Now() }

// TestUploadLeaseCadenceOnARealClock: Run, on the clock and the ticker of
// the machine, writes the lease of an upload in flight at every tick - a
// third of a grace - and not at every second one, which a comparison of the
// time since the last write with the tick itself comes to whenever a tick is
// a microsecond early.
func TestUploadLeaseCadenceOnARealClock(t *testing.T) {
	t.Parallel()
	const (
		grace = 900 * time.Millisecond
		tick  = grace / 3
		// What a tick may be late by on a machine that is busy with the
		// other tests. A skipped tick is a whole tick more.
		late  = 200 * time.Millisecond
		ticks = 10
	)
	f := newLockFixture(t, func(cfg *app.SessionServiceConfig) { cfg.Clock, cfg.UploadGrace = machineClock{}, grace })
	f.store.wrote = make(chan struct{}, 4*ticks)
	ctx, cancel := context.WithCancel(t.Context())
	var done sync.WaitGroup
	done.Go(func() { f.sessions.Run(ctx) })
	defer done.Wait()
	defer cancel()
	defer f.sessions.UploadBegun(ctx, f.signed.Token)()

	var last time.Time
	for n := range ticks {
		select {
		case <-f.store.wrote:
		case <-time.After(grace):
			t.Fatalf("no lease was written for %v after the %d before: the lease of an upload in flight has lapsed", grace, n)
		}
		now := time.Now()
		if gap := now.Sub(last); n > 0 && gap > tick+late {
			t.Errorf("write %d came %v after the one before; a tick is %v", n, gap, tick)
		}
		last = now
	}
}

// TestALeaseOfAnotherUserLocksNothing: the lease is found by its key, which
// is of a user and a moment of sign-in - and it is the lease of a session
// only if it names that session's user as well.
func TestALeaseOfAnotherUserLocksNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newLockFixture(t, nil)
	f.user(t, "bob", iam.RoleReadWrite)
	f.pass(t, lockLifetime)
	f.expect(t, "ended", "at the timeout, nothing uploading,")
	lease := ports.Session{
		TokenHash: app.UploadLeaseKey("alice", testNow), User: "bob", CreatedAt: testNow, ExpiresAt: f.clock.Now().Add(time.Hour),
	}
	if err := f.iamFixture.store.PutSession(ctx, lease, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "ended", "with a lease under its key that names another user,")
	// The same record with its own user's name is its lease.
	lease.User = "alice"
	if err := f.iamFixture.store.PutSession(ctx, lease, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "locked", "with its lease,")
}

// TestAPasswordChangeEndsTheSessions: when the password of a user is
// changed, every session of the user that is there ends - active or locked,
// with an upload in flight or not - and no password brings one back. The
// user signs in again, with the new password. A change that is not of the
// password ends nothing.
func TestAPasswordChangeEndsTheSessions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newLockFixture(t, nil)
	changed := strings.Join([]string{"another", "long", "password"}, " ")
	// Two sessions: one will be locked, one is active. And another user's.
	if _, err := f.iam.CreateUser(ctx, f.root, app.UserInput{Name: "bob", Roles: []string{iam.RoleReadWrite}, Password: f.password}); err != nil {
		t.Fatal(err)
	}
	defer f.upload(t, f.signed.Token)()
	f.pass(t, lockLifetime-time.Minute)
	active, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.sessions.SignIn(ctx, "bob", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	f.pass(t, time.Minute)
	f.expect(t, "locked", "at the timeout, uploading,")

	// A mark under alice's key that names another user - which no node
	// writes - ends nothing of hers.
	if err := f.iamFixture.store.PutSession(ctx, ports.Session{
		TokenHash: app.PasswordChangeKey("alice"), User: "bob", CreatedAt: f.clock.Now(), ExpiresAt: f.clock.Now().Add(time.Hour),
	}, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "locked", "with a mark that names another user,")
	// Roles are changed, the password is not: nothing ends.
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadOnly}}); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "locked", "its user's roles changed,")
	if got := f.state(t, active.Token); got != "active" {
		t.Fatalf("the active session is %s after a change of roles", got)
	}

	f.clock.Advance(time.Second)
	// A session begun at the very moment of the change - the clock of this
	// test stands still - counts as one from before it.
	sameMoment, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: changed}); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "ended", "the password of its user changed, the locked")
	if got := f.state(t, sameMoment.Token); got != "ended" {
		t.Errorf("the session begun at the moment of the change is %s", got)
	}
	if got := f.state(t, active.Token); got != "ended" {
		t.Errorf("the active session is %s after the password changed", got)
	}
	if _, err := f.sessions.Authenticate(ctx, bob.Token, "", false, f.where); err != nil {
		t.Errorf("the session of another user after alice's password changed: %v", err)
	}
	for name, password := range map[string]string{"the old": f.password, "the new": changed} {
		for which, token := range map[string]string{"locked": f.signed.Token, "active": active.Token} {
			if _, err := f.sessions.Unlock(ctx, token, password, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
				t.Errorf("unlocking the %s session with %s password = %v, want not signed in", which, name, err)
			}
		}
	}
	// The upload goes on and the keeper keeps its lease: the session stays
	// ended, and no upload is counted for its token any more.
	f.pass(t, lockGrace)
	f.expect(t, "ended", "a grace later,")
	// Nobody else's session is touched: bob's, begun before the change,
	// unlocks with bob's password, and the one he begins now is a session
	// like any other.
	if _, err := f.sessions.Unlock(ctx, bob.Token, f.password, f.where); err != nil {
		t.Errorf("unlocking bob's session after alice's password changed: %v", err)
	}
	again, err := f.sessions.SignIn(ctx, "bob", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Authenticate(ctx, again.Token, "", false, f.where); err != nil {
		t.Errorf("a session of another user after alice's password changed: %v", err)
	}

	// The old password signs nobody in; the new one does, and that session
	// is one from after the change.
	if _, err := f.sessions.SignIn(ctx, "alice", f.password, f.where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("signing in with the old password = %v", err)
	}
	fresh, err := f.sessions.SignIn(ctx, "alice", changed, f.where)
	if err != nil {
		t.Fatalf("signing in with the new password: %v", err)
	}
	if got := f.state(t, fresh.Token); got != "active" {
		t.Errorf("the session begun after the change is %s", got)
	}
	// It locks and unlocks like any other.
	defer f.upload(t, fresh.Token)()
	f.pass(t, lockLifetime)
	if got := f.state(t, fresh.Token); got != "locked" {
		t.Fatalf("the session begun after the change is %s at its timeout, uploading", got)
	}
	if _, err := f.sessions.Unlock(ctx, fresh.Token, changed, f.where); err != nil {
		t.Errorf("unlocking it with the new password: %v", err)
	}
	// The mark is a record among the sessions and no session: the sweep
	// removes it when no session from before it can be left.
	mark, err := f.iamFixture.store.GetSession(ctx, app.PasswordChangeKey("alice"))
	if err != nil || mark.User != "alice" || mark.CSRFHash != "" || !mark.ExpiresAt.Equal(mark.CreatedAt.Add(app.SessionsEndHorizon)) {
		t.Fatalf("the mark of the change = %+v, %v", mark, err)
	}
	if _, err := f.sessions.Authenticate(ctx, app.PasswordChangeKey("alice"), "", false, f.where); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("the key of the mark as a token = %v", err)
	}
	f.clock.Advance(app.SessionsEndHorizon + lockMax + time.Second)
	if _, err := f.sessions.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.iamFixture.store.GetSession(ctx, app.PasswordChangeKey("alice")); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Errorf("the mark after its time: %v", err)
	}
	// A password that cannot be set changes nothing and ends nothing.
	last, err := f.sessions.SignIn(ctx, "alice", changed, f.where)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.iam.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: "short"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("a password too short = %v", err)
	}
	if got := f.state(t, last.Token); got != "active" {
		t.Errorf("after a change of password that was refused the session is %s", got)
	}
}

// TestEndedSessionsAreRemoved: the node that began a session removes its
// record a tick after it has ended - not while it is locked - with its lease
// once that has lapsed. Without that, every record would stay for as long as
// a session can be locked. What a node began and forgot is left to the
// sweep.
func TestEndedSessionsAreRemoved(t *testing.T) {
	t.Parallel()
	const tick = lockGrace / 3
	ctx := t.Context()
	f := newLockFixture(t, nil)
	f.clock.Advance(time.Second)
	uploading, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	if f.sessions.MadeSessions() != 2 {
		t.Fatalf("the node remembers %d sessions, want 2", f.sessions.MadeSessions())
	}
	ended := f.upload(t, uploading.Token)
	// At the timeout of the first nothing is removed yet; a tick later it
	// is, and the one that uploads - locked - is not.
	f.pass(t, lockLifetime)
	if !f.stored(t, f.signed.Token) {
		t.Fatal("a session was removed at its timeout, before any node with a slower clock had seen it end")
	}
	f.pass(t, tick+time.Second)
	f.sessions.KeepUploadLeases(ctx)
	if f.stored(t, f.signed.Token) {
		t.Error("the record of a session that ended a tick ago is still there")
	}
	if got := f.state(t, uploading.Token); got != "locked" || !f.stored(t, uploading.Token) {
		t.Fatalf("the session that uploads is %s", got)
	}
	// Its upload ends: a grace later it has ended, a tick after that its
	// record is gone, and its lease with it.
	ended()
	f.pass(t, lockGrace+2*tick)
	if f.stored(t, uploading.Token) || f.sessions.MadeSessions() != 0 {
		t.Errorf("the record of the session whose lock ended is still there, or remembered (%d)", f.sessions.MadeSessions())
	}
	if n := f.iamFixture.store.Sessions(); n != 0 {
		t.Errorf("%d session records are left, want none - no session and no lapsed lease", n)
	}

	// A node that was restarted remembers nothing: what it began stays
	// until no session could be locked any more, and then the sweep has it.
	forgotten, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	restarted := app.NewSessionService(app.SessionServiceConfig{
		Sessions: f.store, Users: f.iamFixture.store, Auditor: f.auditor, Clock: f.clock, Entropy: rand.Reader,
		Lifetime: lockLifetime, UploadGrace: lockGrace, LockMax: lockMax,
	})
	f.clock.Advance(lockLifetime + lockGrace)
	restarted.KeepUploadLeases(ctx)
	if !f.stored(t, forgotten.Token) {
		t.Fatal("a node removed the record of a session it does not remember")
	}
	if removed, err := restarted.Sweep(ctx); err != nil || removed != 0 {
		t.Errorf("Sweep before the cap = %d, %v", removed, err)
	}
	f.clock.Advance(lockMax)
	if removed, err := restarted.Sweep(ctx); err != nil || removed != 1 || f.stored(t, forgotten.Token) {
		t.Errorf("Sweep after the cap = %d, %v", removed, err)
	}
	// Signing out and unlocking leave nothing to remember either.
	out, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	before := f.sessions.MadeSessions()
	if err := f.sessions.SignOut(ctx, out.Token, f.where); err != nil {
		t.Fatal(err)
	}
	if got := f.sessions.MadeSessions(); got != before-1 {
		t.Errorf("after a sign-out the node remembers %d sessions, want %d", got, before-1)
	}
	// An unlock makes the session anew: the node forgets the record it
	// deleted, remembers the new one, and removes that once it has ended.
	kept, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := f.sessions.Unlock(ctx, kept.Token, f.password, f.where)
	if err != nil {
		t.Fatal(err)
	}
	if f.stored(t, kept.Token) || !f.stored(t, renewed.Token) {
		t.Fatal("an unlock left the record of before, or made none")
	}
	f.pass(t, lockLifetime+tick+time.Second)
	f.sessions.KeepUploadLeases(ctx)
	if f.stored(t, renewed.Token) {
		t.Error("the record of a session an unlock renewed is still there a tick after it ended")
	}
}

// straddlingCheck checks a password with the provider it wraps and then,
// before it answers, lets something happen once: a change of the password
// lands while a check that read the old hash is still on its way - an
// Argon2 hash takes its time, and so does a Raft commit.
type straddlingCheck struct {
	ports.IdentityProvider
	during atomic.Pointer[func()]
}

func (p *straddlingCheck) Authenticate(ctx context.Context, username, password string) (string, error) {
	name, err := p.IdentityProvider.Authenticate(ctx, username, password)
	if during := p.during.Swap(nil); during != nil {
		(*during)()
	}
	return name, err
}

// brokenMarks is a session store whose marks of a password change cannot
// be read, or cannot be written; everything else it does as the store it
// wraps.
type brokenMarks struct {
	ports.SessionStore
	read, write error
}

func (s brokenMarks) GetSession(ctx context.Context, tokenHash string) (ports.Session, error) {
	if s.read != nil && strings.HasPrefix(tokenHash, "password-change:") {
		return ports.Session{}, s.read
	}
	return s.SessionStore.GetSession(ctx, tokenHash)
}

func (s brokenMarks) PutSession(ctx context.Context, session ports.Session, record audit.Record) error {
	if s.write != nil && strings.HasPrefix(session.TokenHash, "password-change:") {
		return s.write
	}
	return s.SessionStore.PutSession(ctx, session, record)
}

// hookedUsers is a store of users that lets something happen just before a
// user is written.
type hookedUsers struct {
	ports.IAMStore
	before func()
}

func (s hookedUsers) PutUser(ctx context.Context, user iam.User, create bool, record audit.Record) error {
	if s.before != nil {
		s.before()
	}
	return s.IAMStore.PutUser(ctx, user, create, record)
}

// TestAPasswordChangeRaces: a change of password ends every session that a
// check of the old password opened or kept, however the check and the
// change interleave and whichever clocks dated them - within
// app.PasswordChangeMargin - and when the change cannot be completed it
// says so, and refuses rather than lets through (ADR-0027).
func TestAPasswordChangeRaces(t *testing.T) {
	t.Parallel()
	changed := strings.Join([]string{"another", "long", "password"}, " ")
	change := func(t *testing.T, f *lockFixture) {
		t.Helper()
		if _, err := f.iam.UpdateUser(t.Context(), f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: changed}); err != nil {
			t.Error(err)
		}
	}
	// racing has the next check of a password straddle a change of it: the
	// change is written longer after the check began than the margin, and
	// the check answers longer after that again - no margin covers it.
	racing := func(t *testing.T) (*lockFixture, *straddlingCheck) {
		t.Helper()
		var provider *straddlingCheck
		f := newLockFixture(t, func(c *app.SessionServiceConfig) {
			provider = &straddlingCheck{IdentityProvider: c.Provider}
			c.Provider = provider
		})
		during := func() {
			f.clock.Advance(2 * app.PasswordChangeMargin)
			change(t, f)
			f.clock.Advance(2 * app.PasswordChangeMargin)
		}
		provider.during.Store(&during)
		return f, provider
	}

	t.Run("a sign-in with the old password whose check straddles the change is ended", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f, _ := racing(t)
		old, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
		if err != nil {
			t.Fatalf("the check read the old hash and said yes: %v", err)
		}
		if got := f.state(t, old.Token); got != "ended" {
			t.Errorf("the session the old password opened while it was being changed is %s", got)
		}
		f.expect(t, "ended", "the password changed, the session from before")
		if _, err := f.sessions.Unlock(ctx, old.Token, changed, f.where); !errors.Is(err, domain.ErrCredentialNotFound) {
			t.Errorf("unlocking it with the new password = %v, want not signed in", err)
		}
	})

	t.Run("an unlock with the old password whose check straddles the change leaves no session", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f, provider := racing(t)
		// Not this time: first the session locks.
		during := provider.during.Swap(nil)
		defer f.upload(t, f.signed.Token)()
		f.pass(t, lockLifetime)
		f.expect(t, "locked", "at the timeout, uploading,")
		provider.during.Store(during)
		renewed, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
		if err != nil {
			t.Fatalf("the check read the old hash and said yes: %v", err)
		}
		// It keeps the time of the sign-in, which is before the mark.
		if got := f.state(t, renewed.Token); got != "ended" {
			t.Errorf("the session the old password unlocked while it was being changed is %s", got)
		}
		f.expect(t, "ended", "unlocked, the token of before")
	})

	// The mark carries the clock of the gateway that changed the password,
	// a session the clock of the one that began it. Here the session is the
	// fixture's, begun at testNow, and the mark is dated `before` earlier.
	for _, clocks := range []struct {
		name   string
		before time.Duration
		want   string
	}{
		{"a session from before the change", -time.Second, "ended"},
		{"a session of the very moment of the change", 0, "ended"},
		{"a session from before the change, begun through a gateway 3s ahead", 3 * time.Second, "ended"},
		{"a session dated the margin after the change", app.PasswordChangeMargin, "ended"},
		{"a session dated just past the margin after the change: a gateway further ahead than the margin is not covered", app.PasswordChangeMargin + time.Millisecond, "active"},
		{"a session begun a minute after the change", time.Minute, "active"},
	} {
		t.Run("clocks: "+clocks.name, func(t *testing.T) {
			t.Parallel()
			f := newLockFixture(t, nil)
			at := testNow.Add(-clocks.before)
			mark := ports.Session{TokenHash: app.PasswordChangeKey("alice"), User: "alice", CreatedAt: at, ExpiresAt: at.Add(app.SessionsEndHorizon)}
			if err := f.iamFixture.store.PutSession(t.Context(), mark, audit.Record{}); err != nil {
				t.Fatal(err)
			}
			f.expect(t, clocks.want, "the mark dated "+clocks.before.String()+" before the session,")
		})
	}

	t.Run("a sign-in with the new password within the margin after the change is ended: the price of the margin", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newLockFixture(t, nil)
		f.clock.Advance(time.Minute)
		change(t, f)
		f.clock.Advance(app.PasswordChangeMargin / 2)
		soon, err := f.sessions.SignIn(ctx, "alice", changed, f.where)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.state(t, soon.Token); got != "ended" {
			t.Errorf("a session begun %v after the change is %s", app.PasswordChangeMargin/2, got)
		}
		f.clock.Advance(app.PasswordChangeMargin)
		later, err := f.sessions.SignIn(ctx, "alice", changed, f.where)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.state(t, later.Token); got != "active" {
			t.Errorf("a session begun past the margin after the change is %s", got)
		}
	})

	t.Run("the mark is written after the password", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newLockFixture(t, nil)
		var straddling app.SignedIn
		users := hookedUsers{IAMStore: f.iamFixture.store}
		users.before = func() {
			// Between the check of the caller and the write of the hash,
			// longer than the margin: the old password still signs in.
			f.clock.Advance(2 * app.PasswordChangeMargin)
			var err error
			if straddling, err = f.sessions.SignIn(ctx, "alice", f.password, f.where); err != nil {
				t.Errorf("signing in with the password not yet changed: %v", err)
			}
		}
		svc := app.NewIAMService(app.IAMServiceConfig{
			Store: users, Trail: f.iamFixture.store, Authz: f.authz, Auditor: f.auditor, KMS: f.kms, Hasher: f.hasher,
			Clock: f.clock, Entropy: rand.Reader, Sessions: f.iamFixture.store,
		})
		if _, err := svc.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: changed}); err != nil {
			t.Fatal(err)
		}
		if got := f.state(t, straddling.Token); got != "ended" {
			t.Errorf("a session the old password opened before the new one was written is %s", got)
		}
	})

	t.Run("a mark that cannot be read refuses the session, active or locked", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		unavailable := fmt.Errorf("raftmeta.GetSession: %w", domain.ErrUnavailable)
		f := newLockFixture(t, func(c *app.SessionServiceConfig) {
			c.Sessions = brokenMarks{SessionStore: c.Sessions, read: unavailable}
		})
		refused := func(when string) {
			t.Helper()
			if p, err := f.sessions.Authenticate(ctx, f.signed.Token, "", false, f.where); !errors.Is(err, domain.ErrUnavailable) || p.User != "" {
				t.Errorf("%s Authenticate = %+v, %v; want refused for a mark that cannot be read", when, p, err)
			}
			if status, err := f.sessions.Status(ctx, f.signed.Token); !errors.Is(err, domain.ErrUnavailable) || status.Locked {
				t.Errorf("%s Status = %+v, %v", when, status, err)
			}
		}
		refused("active,")
		defer f.upload(t, f.signed.Token)()
		f.pass(t, lockLifetime)
		refused("locked,")
		if _, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where); !errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("Unlock = %v", err)
		}
	})

	t.Run("a mark that cannot be written: the caller is told and the audit trail says so", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newLockFixture(t, nil)
		unavailable := fmt.Errorf("raftmeta.PutSession: %w", domain.ErrUnavailable)
		svc := app.NewIAMService(app.IAMServiceConfig{
			Store: f.iamFixture.store, Trail: f.iamFixture.store, Authz: f.authz, Auditor: f.auditor, KMS: f.kms, Hasher: f.hasher,
			Clock: f.clock, Entropy: rand.Reader, Sessions: brokenMarks{SessionStore: f.iamFixture.store, write: unavailable},
		})
		before := len(f.trail(t))
		_, err := svc.UpdateUser(ctx, f.root, app.UserInput{Name: "alice", Roles: []string{iam.RoleReadWrite}, Password: changed})
		if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "the sessions of the user are not ended") {
			t.Fatalf("UpdateUser = %v", err)
		}
		var said []audit.Record
		for _, r := range f.trail(t)[before:] {
			if r.Action == string(app.ActionEndSessions) {
				said = append(said, r)
			}
			if strings.Contains(fmt.Sprint(r), changed) {
				t.Errorf("a password is in the audit trail: %+v", r)
			}
		}
		if len(said) != 1 || said[0].Outcome != audit.Failed || said[0].Resource != "arn:cactusfs:admin:::user/alice" || said[0].Actor != "root" ||
			said[0].Details["reason"] != "the password is changed, but the sessions of the user are not ended" {
			t.Fatalf("the audit trail says of the sessions that were not ended: %+v", said)
		}
		// What it says is so: the password is changed, the session lives on.
		f.expect(t, "active", "the mark not written,")
		if _, err := f.sessions.SignIn(ctx, "alice", changed, f.where); err != nil {
			t.Errorf("signing in with the new password: %v", err)
		}
		// The same request again ends them.
		f.clock.Advance(time.Minute)
		change(t, f)
		f.expect(t, "ended", "the change made again,")
	})
}

// stuckLeases is a session store whose writes of a lease never end of
// themselves: only when their context does.
type stuckLeases struct {
	ports.SessionStore
}

func (s stuckLeases) PutSession(ctx context.Context, session ports.Session, record audit.Record) error {
	if strings.HasPrefix(session.TokenHash, "upload-lease:") {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.SessionStore.PutSession(ctx, session, record)
}

// TestUploadLeaseKeeperPassHasADeadline: a pass of the keeper does not wait
// for a write of the store longer than a tick - a write that takes longer is
// of no use any more, and the next tick and every other session's lease
// would wait behind it.
func TestUploadLeaseKeeperPassHasADeadline(t *testing.T) {
	t.Parallel()
	f := newLockFixture(t, func(c *app.SessionServiceConfig) {
		c.UploadGrace = 30 * time.Millisecond
		c.Sessions = stuckLeases{SessionStore: c.Sessions}
	})
	defer f.sessions.UploadBegun(t.Context(), f.signed.Token)()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.sessions.KeepUploadLeases(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pass of the keeper still waits for a write that does not end: it has no deadline")
	}
}
