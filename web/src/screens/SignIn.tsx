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
 * The sign-in screen. The API sets the session token as an HttpOnly cookie;
 * the password is sent once and kept nowhere.
 */
export function SignIn({ onSignedIn, signOutError }: SignInProps) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
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
    <main className="signin">
      <img className="signin-logo" src={logo} alt="CactusFS" />
      <form className="card signin-form" onSubmit={submit}>
        <h1>Sign in</h1>
        <ProblemNotice error={action.error ?? signOutError} />
        <label className="field">
          <span>User name</span>
          <input
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
            name="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(event) => {
              setPassword(event.target.value);
            }}
          />
        </label>
        <button type="submit" className="button button-primary" disabled={action.busy}>
          Sign in
        </button>
      </form>
    </main>
  );
}
