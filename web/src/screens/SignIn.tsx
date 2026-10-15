import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import logo from '../assets/brand/cactusfs-logo-tagline.svg';
import { useAction } from '../ui/hooks';
import { ProblemNotice } from '../ui/ProblemNotice';

/**
 * Why a session ended by itself, as far as the console can tell: it timed
 * out, the password was wrong as often as ends a session, or it is not known
 * - the API said only that the session was gone.
 */
export type SessionEndReason = 'timed-out' | 'wrong-passwords' | 'signed-out';

/** What is known of a session that ended by itself: nobody signed out of it. */
export interface SessionEnded {
  /** Why it ended, as far as is known. */
  reason: SessionEndReason;
  /**
   * How the uploads that were on their way together last stood when it
   * ended - what was not stored by then is not going to be - or null when
   * there were none.
   */
  uploads: { stored: number; notStored: number } | null;
}

interface SignInProps {
  /** Called with the CSRF token of the new session. */
  onSignedIn: (csrfToken: string) => void;
  /** Why signing out failed, when it did; null otherwise. */
  signOutError: unknown;
  /** Set when the session before this screen ended by itself; it is said then, with how its uploads stood. */
  ended?: SessionEnded | null;
}

/** What the sign-in screen says of a session that ended, by why it ended. */
const ENDED: Record<SessionEndReason, string> = {
  'timed-out': 'You were signed out: your session timed out.',
  'wrong-passwords': 'You were signed out: the password was wrong too many times.',
  'signed-out': 'You were signed out.',
};

/** "3 objects were stored, 1 was not." */
function storedAndNot(stored: number, notStored: number): string {
  const were = stored === 1 ? '1 object was stored' : `${String(stored)} objects were stored`;
  const wereNot = notStored === 1 ? '1 was not' : `${String(notStored)} were not`;
  return `${were}, ${wereNot}.`;
}

/**
 * The sign-in screen: what the store is, on the left; the form, on the right.
 * The form comes first in the document, as it does on a narrow screen and for
 * whoever has the page read to them.
 * The API sets the session token as an HttpOnly cookie; the password is sent
 * once and kept nowhere.
 *
 * When the session before it ended by itself the form says so, why when
 * that is known, and how the uploads stood: whoever comes back to the console finds the sign-in screen
 * where an upload was, and is told why.
 */
export function SignIn({ onSignedIn, signOutError, ended = null }: SignInProps) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [shown, setShown] = useState(false);
  const action = useAction();

  const submit = (event: FormEvent) => {
    event.preventDefault();
    void action.run(async () => {
      const session = await unwrap(api.POST('/session', { body: { username, password } }));
      setPassword('');
      onSignedIn(session.csrf_token);
    });
  };

  return (
    <div className="signin">
      <main className="signin-panel">
        <img className="signin-logo" src={logo} alt="CactusFS" />
        <form className="signin-form" onSubmit={submit}>
          <h1>Sign in</h1>
          {ended === null ? null : (
            <div className="signin-ended" role="status">
              <p className="signin-ended-title">{ENDED[ended.reason]}</p>
              {ended.uploads === null ? null : (
                <p className="signin-ended-uploads">{storedAndNot(ended.uploads.stored, ended.uploads.notStored)}</p>
              )}
            </div>
          )}
          <ProblemNotice error={action.error ?? signOutError} />
          <label className="field">
            <span>User name</span>
            <input
              className="form-control"
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              required
              value={username}
              onChange={(event) => {
                setUsername(event.target.value);
              }}
            />
          </label>
          <label className="field">
            <span>Password</span>
            <input
              className="form-control"
              name="password"
              type={shown ? 'text' : 'password'}
              autoComplete="current-password"
              required
              value={password}
              onChange={(event) => {
                setPassword(event.target.value);
              }}
            />
          </label>
          <label className="signin-show">
            <input
              className="form-check-input"
              type="checkbox"
              checked={shown}
              onChange={(event) => {
                setShown(event.target.checked);
              }}
            />
            <span>Show what I type</span>
          </label>
          <button type="submit" className="btn btn-primary btn-lg signin-submit" disabled={action.busy} title="Sign in to the console">
            <span className="btn-text">Sign in</span>
          </button>
        </form>
        <nav className="signin-links" aria-label="Documentation">
          <a className="btn btn-outline-secondary btn-sm" href="/docs" title="The management API, to read and to try">
            <i className="bi bi-book" aria-hidden="true" /> <span className="btn-text">API documentation</span>
          </a>
          <a className="btn btn-outline-secondary btn-sm" href="/api/v1/openapi.yaml" title="The OpenAPI description of the management API">
            <i className="bi bi-file-earmark-code" aria-hidden="true" /> <span className="btn-text">API contract</span>
          </a>
        </nav>
      </main>
      <aside className="signin-hero" aria-label="About CactusFS">
        <div className="signin-hero-text">
          <h2>Object storage that keeps what you give it</h2>
          <p>
            CactusFS is an S3-compatible object store. Every object is encrypted before it is
            stored, split into erasure-coded shards across drives and nodes, checked on every
            read and healed in the background. Versioning and Object Lock keep data out of
            reach of mistakes and of ransomware.
          </p>
          <ul className="signin-points">
            <li>Encrypted at rest, always</li>
            <li>Erasure-coded and self-healing</li>
            <li>One binary, standalone or cluster</li>
          </ul>
        </div>
        <Dunes />
      </aside>
    </div>
  );
}

// The width of one swell of the dunes. Each line repeats after it, and is
// drawn twice as wide, so that sliding the lines one swell to the left ends
// where it began and the motion has no seam.
const SWELL = 1200;
// How far apart the points of a line are; at the width the hero is shown
// the lines read as curves.
const STEP = 8;

/** One line of the dunes, its baseline at y: two waves of the swell, added. */
function duneLine(index: number): string {
  const y = 150 + index * 18;
  const lift = 90 - index * 4;
  const points: string[] = [];
  for (let x = 0; x <= 2 * SWELL; x += STEP) {
    const turn = (2 * Math.PI * x) / SWELL;
    const height = y - lift * Math.sin(turn + 0.6) - lift * 0.35 * Math.sin(2 * turn + 1.4 + index * 0.06);
    points.push(`${String(x)} ${height.toFixed(1)}`);
  }
  return `M${points.join(' L')}`;
}

/**
 * The decoration of the left side: lines of dunes, drawn here, drifting
 * slowly as sand does (no motion where the reader asked for less). Nothing
 * is fetched, and it says nothing a screen reader needs to read.
 */
function Dunes() {
  const lines = Array.from({ length: 14 }, (_, index) => index);
  return (
    <svg className="signin-dunes" viewBox="0 0 1200 420" preserveAspectRatio="none" aria-hidden="true">
      <g className="signin-dunes-drift">
        {lines.map((index) => (
          <path
            key={index}
            d={duneLine(index)}
            strokeOpacity={0.12 + index * 0.035}
            style={{ animationDelay: `${String(-(lines.length - 1 - index) * 0.45)}s` }}
          />
        ))}
      </g>
    </svg>
  );
}
