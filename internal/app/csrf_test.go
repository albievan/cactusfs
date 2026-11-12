package app_test

import (
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

// TestCSRFToken holds what GET /session tells a browser tab of the CSRF
// token: the token of the session its cookie stands for now - the one
// sign-in and unlock gave - whichever tab began that session; nothing for a
// session whose token was drawn by a node of an earlier release; a refusal
// for a token that names no session. And that a change with another token
// is refused as a CSRF refusal, which is a denial.
func TestCSRFToken(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newLockFixture(t, nil)
	unlocked, err := f.sessions.Unlock(ctx, f.signed.Token, f.password, f.where)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// A session as a node of the release before stores one: its CSRF
	// token drawn at random, and not derived from its token.
	const oldToken, oldCSRF = "session-token-of-an-older-node-0123456789", "csrf-token-drawn-at-random-0123456789"
	old := ports.Session{
		TokenHash: app.HashToken(oldToken), User: "alice", CSRFHash: app.HashToken(oldCSRF),
		CreatedAt: testNow, ExpiresAt: testNow.Add(lockLifetime),
	}
	if err := f.store.PutSession(ctx, old, f.auditor.System("test:PutSession", "", nil)); err != nil {
		t.Fatal(err)
	}
	second, err := f.sessions.SignIn(ctx, "alice", f.password, f.where)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	tests := []struct {
		name    string
		token   string
		want    string
		refused bool
	}{
		{name: "a session after unlock: the token unlock gave", token: unlocked.Token, want: unlocked.CSRFToken},
		{name: "another session of the same user: its own token", token: second.Token, want: second.CSRFToken},
		{name: "a session of an older node: none", token: oldToken, want: ""},
		{name: "a session replaced by unlock: refused", token: f.signed.Token, refused: true},
		{name: "no session: refused", token: "no-such-session-token", refused: true},
		{name: "no token: refused", token: "", refused: true},
		{name: "a token too long to be one: refused", token: string(make([]byte, 200)), refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.sessions.CSRFToken(ctx, tt.token)
			if tt.refused {
				if !errors.Is(err, domain.ErrAccessDenied) || got != "" {
					t.Fatalf("CSRFToken() = %q, %v; want a denial", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("CSRFToken() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	if second.CSRFToken == unlocked.CSRFToken || len(second.CSRFToken) < 40 {
		t.Errorf("two sessions with CSRF tokens %q and %q", second.CSRFToken, unlocked.CSRFToken)
	}
	// The token one tab holds is refused for the session of another, and
	// the refusal says that it is the token.
	_, err = f.sessions.Authenticate(ctx, second.Token, unlocked.CSRFToken, true, f.where)
	if !errors.Is(err, domain.ErrCSRFTokenRefused) || !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a change with the CSRF token of another session = %v, want ErrCSRFTokenRefused", err)
	}
	// What GET /session tells is what a change is accepted with.
	told, err := f.sessions.CSRFToken(ctx, second.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Authenticate(ctx, second.Token, told, true, f.where); err != nil {
		t.Errorf("a change with the CSRF token GET /session told = %v", err)
	}
	// Reading needs no CSRF token, and is no CSRF refusal without one.
	if _, err := f.sessions.Authenticate(ctx, second.Token, "", false, f.where); err != nil {
		t.Errorf("a read without the CSRF token = %v", err)
	}
}
