import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import logo from '../assets/brand/cactusfs-logo-tagline.svg';
import { useAction } from '../ui/hooks';
import { ProblemNotice } from '../ui/ProblemNotice';

interface SignInProps {
  /** Called with the CSRF token of the new session. */
  onSignedIn: (csrfToken: string) => void;
  /** Why signing out failed, when it did; null otherwise. */
  signOutError: unknown;
}

/**
 * The sign-in screen: what the store is, on the left; the form, on the right.
 * The form comes first in the document, as it does on a narrow screen and for
 * whoever has the page read to them.
 * The API sets the session token as an HttpOnly cookie; the password is sent
 * once and kept nowhere.
 */
export function SignIn({ onSignedIn, signOutError }: SignInProps) {
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

/**
 * The decoration of the left side: lines of dunes, drawn here. Nothing is
 * fetched, and it says nothing a screen reader needs to read.
 */
function Dunes() {
  const lines = Array.from({ length: 14 }, (_, index) => index);
  return (
    <svg className="signin-dunes" viewBox="0 0 1200 420" preserveAspectRatio="none" aria-hidden="true">
      {lines.map((index) => {
        const y = 150 + index * 18;
        const lift = 110 - index * 6;
        const d =
          `M0 ${String(y + 40)} ` +
          `C 200 ${String(y - lift)}, 380 ${String(y + lift * 0.9)}, 600 ${String(y)} ` +
          `S 980 ${String(y - lift * 0.7)}, 1200 ${String(y + 20)}`;
        return <path key={index} d={d} strokeOpacity={0.12 + index * 0.035} />;
      })}
    </svg>
  );
}
