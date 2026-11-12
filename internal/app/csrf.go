package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// csrfLabel is what the CSRF token of a session is the HMAC of, keyed with
// the session token. A new version of the derivation gets a new label.
const csrfLabel = "cactusfs/console/csrf-token/v1"

// csrfTokenFor returns the CSRF token of the session whose token is
// sessionToken: HMAC-SHA256 of csrfLabel, keyed with the session token.
//
// It is derived rather than drawn so that it can be told again to whoever
// holds the session's cookie (SessionService.CSRFToken): the session cookie
// is the browser's, shared by every tab, while the CSRF token is held by
// each tab, and a tab whose token belongs to a session that another tab
// replaced must be able to learn the token of the session there is now.
// Knowing it tells nothing of the session token - an HMAC is not undone -
// and a page of another site can neither read it nor compute it.
func csrfTokenFor(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	mac.Write([]byte(csrfLabel))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// CSRFToken returns the CSRF token of the session whose token is token, to
// the browser that holds the session's cookie: GET /session gives it, so
// that every tab of that browser holds the token of the session the
// cookie stands for now. The error wraps domain.ErrAccessDenied when the
// token names no session.
//
// A session begun by a node of an earlier release has a CSRF token that was
// drawn, not derived, and is known to the tab that began it alone: for it
// the token is "" and no error. Such a session lasts a lifetime at most.
func (s *SessionService) CSRFToken(ctx context.Context, token string) (string, error) {
	if token == "" || len(token) > 4*sessionTokenSize {
		return "", fmt.Errorf("session.CSRFToken: %w: not signed in", domain.ErrAccessDenied)
	}
	stored, err := s.sessions.GetSession(ctx, hashToken(token))
	if errors.Is(err, domain.ErrEntityNotFound) {
		return "", fmt.Errorf("session.CSRFToken: %w: not signed in", domain.ErrAccessDenied)
	}
	if err != nil {
		return "", fmt.Errorf("session.CSRFToken: %w", err)
	}
	csrf := csrfTokenFor(token)
	if !iam.SecretsEqual([]byte(hashToken(csrf)), []byte(stored.CSRFHash)) {
		return "", nil
	}
	return csrf, nil
}
