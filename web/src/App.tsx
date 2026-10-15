import { useCallback, useEffect, useRef, useState } from 'react';

import {
  api,
  ApiError,
  onLocked,
  onUnauthorized,
  signOut as endSession,
  UNLOCK_ATTEMPTS,
  UNLOCK_PATH,
  unwrap,
  type Identity,
  type Session,
  type Unauthorized,
} from './api/client';
import { dropCsrfToken, readCsrfToken, storeCsrfToken } from './api/csrf';
import { Layout } from './Layout';
import { NoticesProvider } from './notices/NoticesProvider';
import { LockScreen } from './screens/LockScreen';
import { Screens } from './screens/Screens';
import { SignIn, type SessionEnded, type SessionEndReason } from './screens/SignIn';
import { askDelay, SESSION_POLL_MS } from './session/timing';
import { Loading } from './ui/Loading';
import { ProblemNotice } from './ui/ProblemNotice';
import type { UploadTotals } from './uploads/state';
import { UploadsProvider } from './uploads/UploadsProvider';

interface SignedIn {
  status: 'signed-in';
  identity: Identity;
  /**
   * Whether the API says the session is locked: it reached its timeout while
   * its user was uploading, and may store objects and nothing else until the
   * password is typed.
   */
  locked: boolean;
  /** When the API says the session reaches its timeout, or null when it does not say. */
  expiresAt: string | null;
}

type SessionState =
  | { status: 'loading' }
  // `ended` is set when the session was not signed out of but ended by itself.
  | { status: 'signed-out'; error: unknown; ended: SessionEnded | null }
  | SignedIn
  | { status: 'failed'; error: unknown };

/** The session as the API described it. */
function signedInAs(identity: Identity): SignedIn {
  return {
    status: 'signed-in',
    identity,
    locked: identity.locked === true,
    expiresAt: identity.expires_at ?? null,
  };
}

/**
 * Why a session ended, as far as the console can tell from the 401 that
 * said so: the API does not say why. A 401 to the console's own question of
 * how the session stands - at the timeout, or while it was locked - is a
 * session that timed out; one to an unlock after as many wrong passwords as
 * end a session is that; anything else is said without a reason.
 */
function whyEnded(cause: Unauthorized | undefined, askedHowItStands: boolean): SessionEndReason {
  if (cause?.path === UNLOCK_PATH && cause.wrongPasswords >= UNLOCK_ATTEMPTS - 1) {
    return 'wrong-passwords';
  }
  if (askedHowItStands && cause?.path === '/session' && cause.method === 'GET') {
    return 'timed-out';
  }
  return 'signed-out';
}

/** What is said of a session that ended, why, and how its last run of uploads stood. */
function endedWith(lastRun: UploadTotals | null, reason: SessionEndReason): SessionEnded {
  return {
    reason,
    uploads:
      lastRun === null || lastRun.files === 0
        ? null
        : { stored: lastRun.stored, notStored: lastRun.files - lastRun.stored },
  };
}

// The CactusFS console. It holds no business or authorization logic: it asks
// the management API who is signed in, shows the sign-in screen when the answer
// is "nobody", and otherwise renders what the API returns - refusals included.
//
// A session that reaches its timeout while its user is uploading is locked by
// the API, not ended: everything but storing objects is answered 423. The
// console then shows the lock in place of its screens and keeps the uploads
// going. Whether the session is locked is never decided here: the API says so
// when it is asked who this is, and the timer below only chooses when to ask.
export function App() {
  const [session, setSession] = useState<SessionState>({ status: 'loading' });
  const [round, setRound] = useState(0);
  // Whether the console is shown: an answer that comes after it went away is
  // told to nobody.
  const shown = useRef(true);
  // Whether the API is being asked how the session stands, so that it is not
  // asked twice at once; and which state of the session an answer belongs
  // to, so that one that was asked for before the session was unlocked or
  // ended does not undo that.
  const asking = useRef(false);
  const era = useRef(0);
  // How the last run of uploads stands, kept here because it has to be said
  // after the uploads went away with the session.
  const lastRun = useRef<UploadTotals | null>(null);

  useEffect(() => {
    shown.current = true;
    return () => {
      shown.current = false;
    };
  }, []);

  // The session is over and nobody signed out of it. Nothing is said of that
  // unless somebody was signed in: an answer of "nobody" to the first
  // question, or to a sign-in that was refused, is not a session that ended.
  const end = useCallback((cause?: Unauthorized) => {
    dropCsrfToken();
    const ended = endedWith(lastRun.current, whyEnded(cause, asking.current));
    lastRun.current = null;
    era.current += 1;
    setSession((previous) =>
      previous.status === 'signed-out'
        ? previous
        : { status: 'signed-out', error: null, ended: previous.status === 'signed-in' ? ended : null },
    );
  }, []);

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
          readCsrfToken() === null ? { status: 'signed-out', error: null, ended: null } : signedInAs(identity),
        );
      },
      (error: unknown) => {
        if (!current) {
          return;
        }
        if (error instanceof ApiError && error.problem.status === 401) {
          // Whoever listens for a 401 has been told already; this is for the
          // case that nobody did.
          setSession((previous) =>
            previous.status === 'signed-out' ? previous : { status: 'signed-out', error: null, ended: null },
          );
        } else {
          setSession({ status: 'failed', error });
        }
      },
    );
    return () => {
      current = false;
    };
  }, [round]);

  // A 401 from any request means the session is gone.
  useEffect(() => onUnauthorized(end), [end]);

  // Asks the API how the session stands, while somebody is signed in: at the
  // timeout, when a request was answered "locked", and every so often while
  // it is locked. A 401 is heard by `end`. Any other failure leaves the
  // session as it was and has it asked after again.
  const ask = useCallback(() => {
    if (asking.current) {
      return;
    }
    asking.current = true;
    const asked = era.current;
    unwrap(api.GET('/session')).then(
      (identity) => {
        asking.current = false;
        if (shown.current && asked === era.current) {
          setSession((previous) => (previous.status === 'signed-in' ? signedInAs(identity) : previous));
        }
      },
      () => {
        asking.current = false;
        if (shown.current && asked === era.current) {
          // The same session, anew: that sets the timer below again.
          setSession((previous) => (previous.status === 'signed-in' ? { ...previous } : previous));
        }
      },
    );
  }, []);

  // A 423 that says "the session is locked": ask, and show what the API says.
  useEffect(() => onLocked(ask), [ask]);

  // When to ask: a moment after the timeout the API named, and every so
  // often while the session is locked - it ends a while after its last
  // upload, and that is learnt by asking. Every answer makes the state anew
  // and so sets this timer again.
  useEffect(() => {
    if (session.status !== 'signed-in') {
      return undefined;
    }
    const delay = session.locked ? SESSION_POLL_MS : askDelay(session.expiresAt, Date.now());
    if (delay === null) {
      return undefined;
    }
    const timer = setTimeout(ask, delay);
    return () => {
      clearTimeout(timer);
    };
  }, [session, ask]);

  const signedIn = useCallback((csrfToken: string) => {
    storeCsrfToken(csrfToken);
    lastRun.current = null;
    setSession({ status: 'loading' });
    setRound((value) => value + 1);
  }, []);

  // The password was accepted. The new CSRF token is kept already - the
  // client does that before it hands the answer on - and the session has a
  // new timeout. The place in the console is in the address and was never
  // touched: its screen is shown again.
  const unlocked = useCallback((fresh: Session) => {
    era.current += 1;
    setSession((previous) =>
      previous.status === 'signed-in'
        ? {
            status: 'signed-in',
            identity: { ...previous.identity, locked: false, expires_at: fresh.expires_at },
            locked: false,
            expiresAt: fresh.expires_at,
          }
        : previous,
    );
  }, []);

  // Never held back by an unlock on its way: see the client's signOut.
  const signOut = useCallback(() => {
    void endSession().then((error) => {
      dropCsrfToken();
      lastRun.current = null;
      era.current += 1;
      if (shown.current) {
        setSession({ status: 'signed-out', error, ended: null });
      }
    });
  }, []);

  const tally = useCallback((totals: UploadTotals) => {
    lastRun.current = totals;
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
            className="btn btn-outline-primary"
            onClick={() => {
              setSession({ status: 'loading' });
              setRound((value) => value + 1);
            }}
            title="Ask the server again"
          >
            <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Try again</span>
          </button>
        </main>
      );
    case 'signed-out':
      return <SignIn onSignedIn={signedIn} signOutError={session.error} ended={session.ended} />;
    case 'signed-in':
      // Uploads and notices are the console's, not a screen's: they stand
      // above the screens, so that an upload goes on whichever is shown, and
      // within the signed-in part, so that they end with the session. They
      // stand above the lock as well: a locked session has not ended, and
      // its uploads go on. Nothing of a screen is there while it is locked.
      return (
        <NoticesProvider>
          <UploadsProvider tally={tally}>
            {session.locked ? (
              <LockScreen
                user={session.identity.user ?? session.identity.kind}
                onUnlocked={unlocked}
                onSignOut={signOut}
              />
            ) : (
              <Layout identity={session.identity} onSignOut={signOut}>
                <Screens />
              </Layout>
            )}
          </UploadsProvider>
        </NoticesProvider>
      );
  }
}
