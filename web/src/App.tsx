import { useCallback, useEffect, useState } from 'react';

import { api, ApiError, onUnauthorized, unwrap, type Identity } from './api/client';
import { dropCsrfToken, readCsrfToken, storeCsrfToken } from './api/csrf';
import { Layout } from './Layout';
import { Screens } from './screens/Screens';
import { SignIn } from './screens/SignIn';
import { Loading } from './ui/Loading';
import { ProblemNotice } from './ui/ProblemNotice';

import './styles/console.css';

type SessionState =
  | { status: 'loading' }
  | { status: 'signed-out'; error: unknown }
  | { status: 'signed-in'; identity: Identity }
  | { status: 'failed'; error: unknown };

// The CactusFS console. It holds no business or authorization logic: it asks
// the management API who is signed in, shows the sign-in screen when the answer
// is "nobody", and otherwise renders what the API returns - refusals included.
export function App() {
  const [session, setSession] = useState<SessionState>({ status: 'loading' });
  const [round, setRound] = useState(0);

  // Ask the API who this is: on load, and again after signing in.
  useEffect(() => {
    let current = true;
    unwrap(api.GET('/session')).then(
      (identity) => {
        if (!current) {
          return;
        }
        // A session without its CSRF token (a second tab, say) can read but
        // not change anything, and the API hands the token out only at
        // sign-in: so sign in again.
        setSession(
          readCsrfToken() === null ? { status: 'signed-out', error: null } : { status: 'signed-in', identity },
        );
      },
      (error: unknown) => {
        if (!current) {
          return;
        }
        const unauthorized = error instanceof ApiError && error.problem.status === 401;
        setSession(unauthorized ? { status: 'signed-out', error: null } : { status: 'failed', error });
      },
    );
    return () => {
      current = false;
    };
  }, [round]);

  // A 401 from any request means the session is gone.
  useEffect(
    () =>
      onUnauthorized(() => {
        dropCsrfToken();
        setSession((previous) =>
          previous.status === 'signed-out' ? previous : { status: 'signed-out', error: null },
        );
      }),
    [],
  );

  const signedIn = useCallback((csrfToken: string) => {
    storeCsrfToken(csrfToken);
    setSession({ status: 'loading' });
    setRound((value) => value + 1);
  }, []);

  const signOut = useCallback(() => {
    unwrap(api.DELETE('/session'))
      .then(
        () => null,
        (error: unknown) => error,
      )
      .then((error) => {
        dropCsrfToken();
        setSession({ status: 'signed-out', error });
      });
  }, []);

  switch (session.status) {
    case 'loading':
      return (
        <main className="splash">
          <Loading />
        </main>
      );
    case 'failed':
      return (
        <main className="splash">
          <h1>CactusFS Console</h1>
          <ProblemNotice error={session.error} />
          <button
            type="button"
            className="button"
            onClick={() => {
              setSession({ status: 'loading' });
              setRound((value) => value + 1);
            }}
          >
            Try again
          </button>
        </main>
      );
    case 'signed-out':
      return <SignIn onSignedIn={signedIn} signOutError={session.error} />;
    case 'signed-in':
      return (
        <Layout identity={session.identity} onSignOut={signOut}>
          <Screens />
        </Layout>
      );
  }
}
