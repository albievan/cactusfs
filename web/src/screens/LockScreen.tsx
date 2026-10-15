import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';

import { ApiError, unlock, type Session } from '../api/client';
import mark from '../assets/brand/cactusfs-mark.svg';
import { ActivityBar } from '../ui/ActivityBar';
import { reasonOf } from '../ui/reason';
import { useUploadList, useUploads } from '../uploads/context';
import { inFlight } from '../uploads/state';

interface LockScreenProps {
  /** Whose session it is, as the API reported it. It is shown, not asked for. */
  user: string;
  /** Called with the answer of the API once the password was accepted. */
  onUnlocked: (session: Session) => void;
  /** Ends the session. */
  onSignOut: () => void;
}

/**
 * What is said when the session was not unlocked, or null when nothing is to
 * be said: a 401 means that the session has ended, and whoever listens for
 * that shows the sign-in screen.
 */
function refusalOf(error: unknown): string | null {
  if (!(error instanceof ApiError)) {
    return 'The server could not be reached. Try again.';
  }
  switch (error.problem.status) {
    // The connection failed, or the server did not answer in time.
    case 0:
      return 'The server could not be reached. Try again.';
    case 401:
      return null;
    case 403:
      return 'The password is wrong';
    case 429:
      return 'Too many attempts - try again in a few minutes';
    default:
      return `The session was not unlocked: ${reasonOf(error)}. Try again.`;
  }
}

/**
 * The page of a locked session, in place of every screen: the session
 * reached its timeout while objects were being uploaded, the API lets it
 * store objects and nothing else, and the console shows nothing of its
 * screens until the user's password is typed. It is a dialog nobody can put
 * aside: there is nothing behind it.
 *
 * It decides nothing. That the session is locked is what the API said, and
 * the password is judged by the API: this only sends it. The password is in
 * the field and nowhere else - not in the state of this component - and is
 * taken out of the field the moment it is sent; it is in no notice and no
 * log.
 *
 * The uploads go on underneath. How they stand is said, in counts and one
 * bar, and nothing of them can be operated here or names what is uploaded:
 * the console may be unattended, which is why it is locked.
 *
 * Signing out ends the uploads with the session, so while files are on
 * their way it is asked first.
 */
export function LockScreen({ user, onUnlocked, onSignOut }: LockScreenProps) {
  const uploads = useUploads();
  const field = useRef<HTMLInputElement>(null);
  const signOutButton = useRef<HTMLButtonElement>(null);
  const stayButton = useRef<HTMLButtonElement>(null);
  // Whether the password is on its way: in the state for the button, and in
  // a ref for the form, which may be sent twice before it is drawn again.
  const [busy, setBusy] = useState(false);
  const sending = useRef(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  // Whether the question "sign out and stop the uploads?" is being asked.
  const [leaving, setLeaving] = useState(false);
  // Whether this page is still shown when an answer comes.
  const shown = useRef(true);

  // The keyboard is in the password field when the lock appears.
  useEffect(() => {
    shown.current = true;
    field.current?.focus();
    return () => {
      shown.current = false;
    };
  }, []);

  // The question was asked: the keyboard goes to the answer that changes
  // nothing. Answered, it is back on the button that asked.
  const asked = useRef(false);
  useEffect(() => {
    if (leaving) {
      asked.current = true;
      stayButton.current?.focus();
    } else if (asked.current) {
      asked.current = false;
      signOutButton.current?.focus();
    }
  }, [leaving]);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const input = field.current;
    if (input === null || sending.current || input.value === '') {
      return;
    }
    // Out of the field as it is sent: from here on it is only in the request.
    const password = input.value;
    input.value = '';
    sending.current = true;
    setBusy(true);
    setRefusal(null);
    unlock(password).then(
      (session) => {
        sending.current = false;
        // Whoever shows this page is told whether or not it is still shown.
        onUnlocked(session);
        if (shown.current) {
          setBusy(false);
        }
      },
      (error: unknown) => {
        sending.current = false;
        if (!shown.current) {
          return;
        }
        setBusy(false);
        setRefusal(refusalOf(error));
        // Still locked: the field is empty and has the keyboard again.
        field.current?.focus();
      },
    );
  };

  // Signing out with nothing on its way is done at once.
  const signOut = () => {
    if (uploads.busy()) {
      setLeaving(true);
    } else {
      onSignOut();
    }
  };

  const stay = () => {
    setLeaving(false);
  };

  const leave = () => {
    setLeaving(false);
    uploads.cancelAll();
    onSignOut();
  };

  // Escape answers the question with the answer that changes nothing. It
  // does nothing else: the lock cannot be put aside.
  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === 'Escape' && leaving) {
      stay();
    }
  };

  return (
    <main className="lockscreen" onKeyDown={onKeyDown}>
      <div
        className="lockscreen-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="lockscreen-title"
        aria-describedby="lockscreen-why"
      >
        <img className="lockscreen-mark" src={mark} alt="CactusFS" />
        <h1 id="lockscreen-title" className="lockscreen-title">
          <i className="bi bi-lock" aria-hidden="true" /> Session locked
        </h1>
        <p className="lockscreen-user">
          <i className="bi bi-person-circle" aria-hidden="true" />
          <span>
            Signed in as <strong>{user}</strong>
          </span>
        </p>
        <Why />
        <ActivityBar readOnly />
        <form className="lockscreen-form" onSubmit={submit}>
          <div className="notice-region" aria-live="assertive">
            {refusal === null ? null : (
              <div className="alert alert-danger notice" role="alert">
                <p className="notice-title">{refusal}</p>
              </div>
            )}
          </div>
          <label className="field">
            <span>Password</span>
            <input
              ref={field}
              className="form-control"
              name="password"
              type="password"
              autoComplete="current-password"
              required
            />
          </label>
          <button
            type="submit"
            className="btn btn-primary btn-lg lockscreen-submit"
            disabled={busy}
            title="Unlock the console with your password: the uploads go on meanwhile"
          >
            <i className="bi bi-unlock" aria-hidden="true" /> <span className="btn-text">Unlock</span>
          </button>
        </form>
        <div className="lockscreen-leave">
          {leaving ? (
            <div className="confirm" role="group" aria-label="Sign out and stop the uploads?">
              <span className="confirm-question">
                Objects are still being uploaded. Sign out and stop the uploads?
              </span>
              <button
                type="button"
                className="btn btn-outline-danger btn-sm"
                onClick={leave}
                title="Stop the uploads and end this session: what was not stored yet is not stored"
              >
                <i className="bi bi-box-arrow-right" aria-hidden="true" /> <span className="btn-text">Yes, sign out</span>
              </button>
              <button
                ref={stayButton}
                type="button"
                className="btn btn-outline-secondary btn-sm"
                onClick={stay}
                title="Stay signed in: the uploads go on"
              >
                <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Stay signed in</span>
              </button>
            </div>
          ) : (
            <>
              <button
                ref={signOutButton}
                type="button"
                className="btn btn-outline-secondary btn-sm"
                onClick={signOut}
                aria-describedby="lockscreen-leave-hint"
                title="End this session. The uploads are stopped: what was not stored yet is not stored"
              >
                <i className="bi bi-box-arrow-right" aria-hidden="true" /> <span className="btn-text">Sign out</span>
              </button>
              <span id="lockscreen-leave-hint" className="lockscreen-hint">
                Signing out stops the uploads.
              </span>
            </>
          )}
        </div>
      </div>
    </main>
  );
}

/**
 * Why the console is locked, and what becomes of the uploads. It is a part
 * of its own because it follows the uploads: the rest of the page is not
 * drawn again as they progress.
 *
 * A page that was loaded again has no uploads - they went with the page
 * before - and does not say that they continue: the session then ends a
 * while after the last of them did, unless it is unlocked.
 */
function Why() {
  const uploading = useUploadList().some(inFlight);
  return (
    <p id="lockscreen-why" className="lockscreen-why">
      Your session timed out while objects were being uploaded. Type your password to go on.{' '}
      {uploading
        ? 'The uploads continue.'
        : 'Nothing is being uploaded from this page: the session ends soon unless it is unlocked.'}
    </p>
  );
}
