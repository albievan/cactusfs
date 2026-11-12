// The CSRF token of the console session. The server returns it when the user
// signs in or unlocks, and again with every GET /session, and means it to be
// read by script; the session token itself is an HttpOnly cookie this code
// never sees. sessionStorage keeps the token for this tab only and forgets it
// when the tab closes. The cookie, though, is the browser's: when another tab
// signs in or unlocks, the token kept here is no longer the session's, and
// the console takes the session's token from GET /session again (App.tsx).
const STORAGE_KEY = 'cactusfs.csrf';

/** Returns the CSRF token of the current session, or null when there is none. */
export function readCsrfToken(): string | null {
  return window.sessionStorage.getItem(STORAGE_KEY);
}

/** Keeps the CSRF token that signing in returned. */
export function storeCsrfToken(token: string): void {
  window.sessionStorage.setItem(STORAGE_KEY, token);
}

/** Forgets the CSRF token: the session is over. */
export function dropCsrfToken(): void {
  window.sessionStorage.removeItem(STORAGE_KEY);
}
