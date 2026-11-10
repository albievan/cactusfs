import { useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';
import { createPortal } from 'react-dom';

import { ApiError, startUpgrade, type UpgradeMode, type UpgradeProgress } from '../api/client';
import { ProblemNotice } from '../ui/ProblemNotice';
import { reasonOf } from '../ui/reason';

// What the keyboard goes round, inside the dialog.
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), [tabindex]:not([tabindex="-1"])';

/** "try again in N s", from what the API said of when. */
export function tryAgainIn(error: ApiError): string {
  return error.retryAfter === undefined ? 'try again in a minute' : `try again in ${String(error.retryAfter)} s`;
}

/**
 * The same problem, with a title in plain words: what the API said in its
 * own words stays as the detail, and its request ID with it. An error that
 * is not the API's becomes one with the words as its message.
 */
export function worded(error: unknown, title: string): ApiError | Error {
  if (error instanceof ApiError) {
    return new ApiError({ ...error.problem, title }, error.replayed, error.staleTokens, error.retryAfter);
  }
  return new Error(title, { cause: error });
}

/**
 * Why an upgrade was not started, in plain words. The API judged it - the
 * password, whether the caller may, whether the release may be installed -
 * and this only words its answer. Every one of them says that nothing was
 * installed, because nothing was.
 */
export function upgradeProblem(error: unknown): ApiError | Error {
  if (!(error instanceof ApiError)) {
    return worded(
      error,
      'The request did not reach the server, or its answer was lost. Whether the upgrade began is shown on the screen once the server answers.',
    );
  }
  const { status, code } = error.problem;
  switch (status) {
    case 401:
      return worded(error, 'Your session has ended - after too many wrong passwords, or for another reason. Nothing was installed.');
    case 403:
      return worded(error, 'The password was not accepted, or you are not allowed to install releases. Nothing was installed.');
    case 404:
      return worded(error, 'The node that answered runs an earlier release, which cannot install updates. Nothing was installed.');
    case 409:
      if (code === 'UpgradeRunning') {
        return worded(error, 'An upgrade is running already: nothing more was started. Its progress is shown on the screen.');
      }
      if (code === 'UpdatesOff') {
        return worded(
          error,
          'Update checks are off on the node that answered (CACTUSFS_UPDATE_SOURCE is not set). Nothing was installed.',
        );
      }
      if (code === 'UpgradeNotAllowed') {
        return worded(
          error,
          'This release may not be installed from the running one, or the node did not find it when it last checked: check for updates again. Nothing was installed.',
        );
      }
      return error;
    case 412:
      return worded(error, 'The release could not be verified; nothing was installed.');
    case 423:
      return worded(error, 'Your session is locked: unlock it first. Nothing was installed.');
    case 429:
      return worded(error, `Too many attempts - ${tryAgainIn(error)}. Nothing was installed.`);
    case 501:
      return worded(
        error,
        'Installing from the console is not available on this deployment: the screen says why, and how to install it instead. Nothing was installed.',
      );
    default:
      return worded(error, `The upgrade could not be started: ${reasonOf(error, 'the request failed')}. Nothing was installed.`);
  }
}

interface UpgradeDialogProps {
  /** The release to install. */
  version: string;
  /** The release the answering node runs. */
  current: string;
  /** How this deployment installs a release. */
  mode: UpgradeMode;
  /** Puts the question aside; nothing was sent, or what was sent was refused. */
  onClose: () => void;
  /** The upgrade has begun: the API answered 202 with its progress. */
  onStarted: (progress: UpgradeProgress, version: string) => void;
  /** An upgrade may be running - one ran already, or the answer was lost: its progress is asked for. */
  onMaybeRunning: () => void;
  /** Installing is not available here: what the node knows is asked for again, to say why. */
  onUnavailable: () => void;
}

/**
 * "Upgrade to X?": a modal dialog that says what will happen and asks for
 * the password of whoever is signed in, sent with the request that installs
 * the release (`POST /upgrade {version, password}`). There is no separate
 * unlock: the SERVER judges the password in that request, and whether the
 * caller may install, and whether the release may be installed.
 *
 * The password is in the field and nowhere else - not in the state of a
 * component - and the field is emptied the moment it is sent. It is in no
 * notice and no log.
 *
 * While it is open the rest of the page is inert, the keyboard stays in the
 * dialog, and Escape puts it aside - unless the password is on its way.
 */
export function UpgradeDialog({ version, current, mode, onClose, onStarted, onMaybeRunning, onUnavailable }: UpgradeDialogProps) {
  const [sending, setSending] = useState(false);
  const sendingNow = useRef(false);
  const [problem, setProblem] = useState<unknown>(null);
  const backdrop = useRef<HTMLDivElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const field = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const whatId = useId();
  // Whether this is still shown when the answer comes.
  const shown = useRef(true);
  useEffect(() => {
    shown.current = true;
    return () => {
      shown.current = false;
    };
  }, []);

  // The latest onClose and whether it may be used, for the listeners on the document.
  const closing = useRef(onClose);
  useEffect(() => {
    closing.current = onClose;
  }, [onClose]);

  // While open: the keyboard is in the password field, and the rest of the
  // page is inert - neither clicked, focused nor read out. When it closes,
  // what was made inert here is given back.
  useEffect(() => {
    const own = backdrop.current;
    const made: Element[] = [];
    if (own !== null) {
      for (const sibling of Array.from(document.body.children)) {
        if (sibling !== own && !sibling.hasAttribute('inert')) {
          sibling.setAttribute('inert', '');
          made.push(sibling);
        }
      }
    }
    const overflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    field.current?.focus();
    // Whatever takes the keyboard out of the dialog gives it back.
    const keepFocus = (event: FocusEvent) => {
      const inside = dialog.current;
      if (inside !== null && event.target instanceof Node && !inside.contains(event.target)) {
        field.current?.focus();
      }
    };
    document.addEventListener('focusin', keepFocus);
    // Escape with the keyboard nowhere closes it too, unless the password is on its way.
    const escape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape' && !event.defaultPrevented && !sendingNow.current) {
        event.preventDefault();
        closing.current();
      }
    };
    document.addEventListener('keydown', escape);
    return () => {
      document.removeEventListener('focusin', keepFocus);
      document.removeEventListener('keydown', escape);
      document.body.style.overflow = overflow;
      for (const element of made) {
        element.removeAttribute('inert');
      }
    };
  }, []);

  // Escape closes; Tab goes round the dialog's own controls.
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      if (!sendingNow.current) {
        onClose();
      }
      return;
    }
    if (event.key !== 'Tab' || dialog.current === null) {
      return;
    }
    const controls = Array.from(dialog.current.querySelectorAll<HTMLElement>(FOCUSABLE));
    const first = controls[0];
    const last = controls.at(-1);
    if (first === undefined || last === undefined) {
      return;
    }
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const input = field.current;
    if (input === null || sendingNow.current || input.value === '') {
      return;
    }
    // Out of the field as it is sent: from here on it is only in the request.
    const password = input.value;
    input.value = '';
    sendingNow.current = true;
    setSending(true);
    setProblem(null);
    startUpgrade(version, password).then(
      (progress) => {
        sendingNow.current = false;
        if (shown.current) {
          onStarted(progress, version);
        }
      },
      (error: unknown) => {
        sendingNow.current = false;
        if (!shown.current) {
          return;
        }
        setSending(false);
        setProblem(upgradeProblem(error));
        if (!(error instanceof ApiError) || (error.problem.status === 409 && error.problem.code === 'UpgradeRunning')) {
          onMaybeRunning();
        } else if (error.problem.status === 501) {
          onUnavailable();
        }
        field.current?.focus();
      },
    );
  };

  return createPortal(
    <div ref={backdrop} className="upgrade-backdrop">
      <div
        ref={dialog}
        className="upgrade-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={whatId}
        onKeyDown={onKeyDown}
      >
        <header className="upgrade-head">
          <h2 id={titleId} className="upgrade-title">
            <i className="bi bi-arrow-up-circle" aria-hidden="true" /> Upgrade to {version}?
          </h2>
        </header>
        <form className="upgrade-form" onSubmit={submit}>
          <div className="upgrade-body">
            <div id={whatId} className="upgrade-what">
              <p>
                <strong>
                  The deployment is moved from {current} to {version}.
                </strong>
              </p>
              <ul>
                {mode === 'binary' ? (
                  <li>
                    This deployment is one node: its binary is replaced by that of {version}, and the node restarts with it.
                    The binary it runs now is kept beside it.
                  </li>
                ) : (
                  <li>
                    The nodes are moved one at a time, the node answering you last. A node that does not come up is put back,
                    and the upgrade stops there.
                  </li>
                )}
                <li>
                  The node answering you restarts. The console then says that it is restarting and goes on asking how the upgrade
                  stands until it answers again.
                </li>
                <li>
                  Before anything is installed the release is verified on the node, against the signature of its release. If it
                  does not verify, nothing is installed.
                </li>
                <li>
                  Going back is not possible once {version} has raised the metadata schema: {current} cannot read it any more. It
                  is not offered by the console in any case.
                </li>
              </ul>
            </div>
            <ProblemNotice error={problem} />
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
          </div>
          <footer className="upgrade-foot">
            <button
              type="submit"
              className="btn btn-primary"
              disabled={sending}
              title={`Send your password with the request to install ${version}: the server checks both`}
            >
              <i className="bi bi-arrow-up-circle" aria-hidden="true" /> <span className="btn-text">Upgrade to {version}</span>
            </button>
            <button
              type="button"
              className="btn btn-outline-secondary"
              disabled={sending}
              onClick={onClose}
              title="Install nothing"
            >
              <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Cancel</span>
            </button>
          </footer>
        </form>
      </div>
    </div>,
    document.body,
  );
}
