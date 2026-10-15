import { useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';

import { ApiError, unlock, UNLOCK_ATTEMPTS, wrongPasswordCount, type BucketUsage } from '../api/client';
import type { Expected } from '../deletions/state';
import type { Loaded } from '../ui/hooks';
import { reasonOf } from '../ui/reason';

/** How many objects the bucket holds, as the question says it: from its usage, and never more sure than that. */
function howManyOf(usage: Loaded<BucketUsage>): string {
  const data = usage.data;
  if (data === undefined) {
    if (usage.loading) {
      return 'How many is still being counted.';
    }
    if (usage.error instanceof ApiError && usage.error.problem.status === 403) {
      return 'How many is not visible to you.';
    }
    return 'How many could not be counted.';
  }
  const count = data.object_count.toLocaleString('en');
  const objects = data.object_count === 1 && data.complete ? 'object' : 'objects';
  if (!data.complete) {
    return `It holds at least ${count} ${objects}: the count stopped at its bound.`;
  }
  if (!data.exact) {
    return `It holds ${count} ${objects}, as counted just now: not an exact figure.`;
  }
  return `It holds ${count} ${objects}.`;
}

/** What the progress of the deletion is told to expect, from the usage: nothing when it is not known. */
function expectedOf(usage: Loaded<BucketUsage>): Expected | undefined {
  const data = usage.data;
  return data === undefined ? undefined : { count: data.object_count, atLeast: !data.complete };
}

/** What is said when a deletion of the bucket began while the password was being checked. */
export const BEGAN_MEANWHILE =
  'A deletion of this bucket began while the password was being checked: nothing more was begun. Try again once it is over.';

/**
 * What is said when the password was not accepted, and nothing was deleted.
 * The API judged it: this only words its answer.
 */
function refusalOf(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return 'The server could not be reached. Nothing was deleted.';
  }
  switch (error.problem.status) {
    case 0:
      return 'The server could not be reached. Nothing was deleted.';
    case 401:
      return 'Your session has ended. Nothing was deleted.';
    case 403: {
      // The API ends the session at so many wrong passwords in a row: what is
      // left is said, as the count the client keeps has it. It decides nothing.
      const left = Math.max(1, UNLOCK_ATTEMPTS - wrongPasswordCount());
      return `The password is wrong. Nothing was deleted. ${String(left)} more wrong ${left === 1 ? 'password ends' : 'passwords end'} your session.`;
    }
    case 429:
      return 'Too many attempts - try again in a few minutes. Nothing was deleted.';
    default:
      return `The password could not be checked: ${reasonOf(error)}. Nothing was deleted.`;
  }
}

interface DeleteAllProps {
  /** The bucket whose every object is deleted. */
  bucket: string;
  /** How much it holds, for the question. */
  usage: Loaded<BucketUsage>;
  /** True while a deletion of the bucket is on its way. */
  disabled: boolean;
  /** Begins the deletion of every object, once the server accepted the password. */
  onConfirmed: (expected: Expected | undefined) => void;
  /** Told when the password goes off to be checked, and when its answer came: nothing else is to be deleted meanwhile. */
  onChecking?: (checking: boolean) => void;
}

/**
 * "Delete all": a button, and the question it opens. The question says what
 * will happen - every object of the bucket, how many, what becomes of the
 * versions a bucket keeps and of those under Object Lock - and asks for the
 * password of whoever is signed in.
 *
 * The password is judged by the SERVER, before the first deletion is sent:
 * it is sent with `POST /session/unlock`, which re-authenticates the session
 * that is open - the same check, throttle and audit trail as signing in -
 * and gives it new tokens. Only when it is accepted does the deletion begin;
 * a wrong password (403), too many attempts (429) and a session that ended
 * (401) are said, and nothing is deleted. Each deletion is then still a
 * request of its own, judged by the authorizer for its object.
 *
 * The password is in the field and nowhere else - not in the state of this
 * component - and the field is emptied the moment it is sent. It is in no
 * notice and no log.
 */
export function DeleteAll({ bucket, usage, disabled, onConfirmed, onChecking }: DeleteAllProps) {
  const [asking, setAsking] = useState(false);
  const [checking, setChecking] = useState(false);
  const sending = useRef(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const field = useRef<HTMLInputElement>(null);
  const opener = useRef<HTMLButtonElement>(null);
  const titleId = useId();
  const whatId = useId();
  // Whether this is still shown when the answer comes: a deletion the user
  // left the screen of before the password was accepted is not begun.
  const shown = useRef(true);
  // Whether a deletion of the bucket is on its way now, for the answer to
  // the password, which comes after the render that set it.
  const busyNow = useRef(disabled);
  useEffect(() => {
    busyNow.current = disabled;
  }, [disabled]);
  // Whoever is told of the check is told it is over when this goes away
  // while the password is on its way: its answer then begins nothing.
  const tellChecking = useRef(onChecking);
  useEffect(() => {
    tellChecking.current = onChecking;
  }, [onChecking]);
  useEffect(() => {
    shown.current = true;
    return () => {
      shown.current = false;
      if (sending.current) {
        tellChecking.current?.(false);
      }
    };
  }, []);

  // The keyboard is in the password field when the question is asked.
  useEffect(() => {
    if (asking) {
      field.current?.focus();
    }
  }, [asking]);

  const close = () => {
    setAsking(false);
    setRefusal(null);
    opener.current?.focus();
  };

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
    setChecking(true);
    onChecking?.(true);
    setRefusal(null);
    const expected = expectedOf(usage);
    unlock(password).then(
      () => {
        sending.current = false;
        if (!shown.current) {
          return;
        }
        setChecking(false);
        onChecking?.(false);
        // Another deletion of the bucket began meanwhile - from another
        // screen of this console, say: this one is not begun beside it.
        if (busyNow.current) {
          setRefusal(BEGAN_MEANWHILE);
          field.current?.focus();
          return;
        }
        setAsking(false);
        // Whoever begins it puts the keyboard on its progress.
        onConfirmed(expected);
      },
      (error: unknown) => {
        sending.current = false;
        if (!shown.current) {
          return;
        }
        setChecking(false);
        onChecking?.(false);
        setRefusal(refusalOf(error));
        field.current?.focus();
      },
    );
  };

  // Escape puts the question aside, unless the password is on its way.
  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === 'Escape' && !checking) {
      event.stopPropagation();
      close();
    }
  };

  return (
    <>
      <button
        ref={opener}
        type="button"
        className="btn btn-outline-danger"
        disabled={disabled}
        onClick={() => {
          setAsking(true);
        }}
        title="Delete every object of this bucket, not only those on this page: it asks for your password first"
      >
        <i className="bi bi-trash3" aria-hidden="true" /> <span className="btn-text">Delete all</span>
      </button>
      {asking ? (
        <div
          className="delete-all"
          role="dialog"
          aria-labelledby={titleId}
          aria-describedby={whatId}
          onKeyDown={onKeyDown}
        >
          <h3 id={titleId} className="delete-all-title">
            <i className="bi bi-exclamation-triangle" aria-hidden="true" /> Delete every object of {bucket}?
          </h3>
          <div id={whatId} className="delete-all-what">
            <p>
              <strong>Every object of the bucket {bucket} is deleted</strong> - not only those on this page or under
              this folder. {howManyOf(usage)}
            </p>
            <ul>
              <li>
                If the bucket keeps versions, its versions are kept: each object gets a delete marker and its older
                versions stay. Otherwise the objects are gone, and this cannot be undone.
              </li>
              <li>Versions under Object Lock are not removed: such an object is counted as not deleted, and the rest go on.</li>
              <li>An object stored while the deletion runs may be left.</li>
              <li>The deletion goes on when you open another screen; reloading or closing the page stops it.</li>
            </ul>
          </div>
          <form className="delete-all-form" onSubmit={submit}>
            <div className="notice-region" aria-live="assertive">
              {refusal === null ? null : (
                <div className="alert alert-danger notice" role="alert">
                  <p className="notice-title">{refusal}</p>
                </div>
              )}
            </div>
            <label className="field">
              <span>Your password</span>
              <input
                ref={field}
                className="form-control"
                name="password"
                type="password"
                autoComplete="current-password"
                required
              />
            </label>
            <div className="confirm">
              <button
                type="submit"
                className="btn btn-danger"
                disabled={checking || disabled}
                title="Check the password on the server, then delete every object of this bucket"
              >
                <i className="bi bi-trash3" aria-hidden="true" /> <span className="btn-text">Delete every object</span>
              </button>
              <button
                type="button"
                className="btn btn-outline-secondary"
                disabled={checking}
                onClick={close}
                title="Leave the bucket as it is"
              >
                <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Cancel</span>
              </button>
            </div>
          </form>
        </div>
      ) : null}
    </>
  );
}
