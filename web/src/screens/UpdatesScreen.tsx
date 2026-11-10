import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';

import {
  api,
  ApiError,
  checkForUpdates,
  unwrap,
  type AvailableRelease,
  type UpdateCheckError,
  type UpdatesView,
  type UpgradeMode,
  type UpgradeNode,
  type UpgradeProgress,
  type UpgradeRefusal,
  type UpgradeState,
} from '../api/client';
import { describeBuild } from '../Layout';
import { formatTime } from '../ui/format';
import { useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { useUpdatesNotice } from '../updates/context';
import { tryAgainIn, UpgradeDialog, worded } from './UpgradeDialog';

/** How often the progress of an upgrade is asked for. */
export const UPGRADE_POLL_MS = 3_000;

/**
 * How long the console waits for an answer that shows the upgrade going on
 * before it says that it has had none. It asks no more then, until told to.
 */
export const UPGRADE_SILENCE_MS = 10 * 60 * 1000;

/** The variable that turns checks for updates on. */
const SOURCE_VARIABLE = 'CACTUSFS_UPDATE_SOURCE';

function loadUpdates(signal: AbortSignal): Promise<UpdatesView> {
  return unwrap(api.GET('/updates', { signal }));
}

// Why the last check could not be read, in words. A class this console does
// not know - a newer node may name one - is shown as it came.
const CHECK_ERRORS = new Map<string, string>([
  ['unreachable', 'the update source could not be reached'],
  ['rate-limited', 'the update source limited the rate of requests; the node asks again later'],
  ['refused', 'the update source refused the request'],
  ['invalid-answer', 'the update source answered with something the node does not read'],
  ['timeout', 'the update source did not answer in time'],
]);

function checkErrorInWords(error: UpdateCheckError | undefined): string | null {
  if (error === undefined) {
    return null;
  }
  const text: string = error;
  return CHECK_ERRORS.get(text) ?? text;
}

// Why a release may not be installed from the running one, in words.
const REFUSALS = new Map<string, string>([
  ['equal', 'It is the release that runs now.'],
  ['older', 'It is older than the release that runs now: nothing is downgraded from the console.'],
  ['major', 'It is of another major version: that is not installed from the console.'],
  ['skipped-minor', 'It is more than one minor version ahead: install the newest patch of the next minor version first.'],
  ['not-newest-patch', 'A newer patch of its minor version is listed: that one is offered instead.'],
  ['unknown-release', 'It was not listed when the node last checked.'],
  ['prerelease-on-stable', 'It is a pre-release, and the channel is stable.'],
]);

function refusalInWords(refusal: UpgradeRefusal | undefined): string {
  if (refusal === undefined) {
    return 'It may not be installed from the running release.';
  }
  const text: string = refusal;
  return REFUSALS.get(text) ?? text;
}

// How this deployment installs a release, in words.
const MODES = new Map<string, string>([
  ['off', 'Off: releases are not installed from the console'],
  ['binary', 'Binary: the binary of this node is replaced, and the node restarts'],
  ['kubernetes', 'Kubernetes: the nodes are moved to the release’s image one at a time'],
]);

function modeInWords(mode: UpgradeMode): string {
  const text: string = mode;
  return MODES.get(text) ?? text;
}

// Where an upgrade stands, in words.
const STATES = new Map<string, string>([
  ['idle', 'No upgrade is running'],
  ['verifying', 'Verifying the release'],
  ['running', 'Upgrading the nodes'],
  ['restart-required', 'The binary was replaced: whoever runs the node must restart it to finish'],
  ['failed', 'The upgrade stopped'],
  ['done', 'Every node runs the new release'],
]);

function stateInWords(state: UpgradeState): string {
  const text: string = state;
  return STATES.get(text) ?? text;
}

/** Why `GET /updates` was not answered, in plain words. */
function loadProblem(error: unknown): unknown {
  if (error instanceof ApiError) {
    switch (error.problem.status) {
      case 403:
        return worded(error, 'You are not allowed to see which releases there are.');
      case 404:
        return worded(error, 'Updates are not available on the node that answered: it runs an earlier release.');
      default:
        return error;
    }
  }
  return error;
}

/** Why a check was not made, in plain words. */
function checkProblem(error: unknown): unknown {
  if (error instanceof ApiError) {
    switch (error.problem.status) {
      case 403:
        return worded(error, 'You are not allowed to check for updates.');
      case 404:
        return worded(error, 'The node that answered runs an earlier release, which cannot check for updates.');
      case 409:
        return worded(error, `Update checks are off: ${SOURCE_VARIABLE} is not set on the node that answered. Nothing was asked.`);
      case 429:
        return worded(error, `The node checked less than a minute ago - ${tryAgainIn(error)}. Nothing was asked.`);
      default:
        return worded(error, `The check could not be made: ${error.problem.title}`);
    }
  }
  return error;
}

/** Why the progress of an upgrade is no longer asked for: the session, not the restart. */
function endsWatch(error: unknown): boolean {
  if (!(error instanceof ApiError)) {
    return false;
  }
  const status = error.problem.status;
  return status === 401 || status === 403 || status === 423;
}

/** How the console follows an upgrade. */
interface Watch {
  /** What the API said last. */
  progress: UpgradeProgress | null;
  /** The release being installed, as the console learnt it. */
  target: string | null;
  /** Whether the progress is being asked for every UPGRADE_POLL_MS. */
  watching: boolean;
  /** Whether the last question had no answer: the node that answers is taken to be restarting. */
  restarting: boolean;
  /** Why the console stopped asking before the upgrade ended: no answer, or none that showed it going on. */
  gaveUp: 'silent' | 'stalled' | null;
  /** The release the node that answers was found to run, once it is the target. */
  upgradedTo: string | null;
  /** Why the progress cannot be asked for: the session's, never the restart's. */
  error: unknown;
}

const NOT_WATCHING: Watch = {
  progress: null,
  target: null,
  watching: false,
  restarting: false,
  gaveUp: null,
  upgradedTo: null,
  error: null,
};

/** Whether an upgrade in this state is over, one way or another, for now. */
function isSettled(state: UpgradeState): boolean {
  return state === 'failed' || state === 'restart-required';
}

/** Whether an upgrade in this state is under way. */
function isUnderWay(state: UpgradeState): boolean {
  return state === 'verifying' || state === 'running';
}

/**
 * Follows an upgrade: asks `GET /upgrade` every UPGRADE_POLL_MS while it is
 * under way. While the node that answers restarts, a request that fails -
 * the connection, a 502 or 503 of whatever stands in front, any answer that
 * is not the session's refusal - is taken as "restarting", not as a failure,
 * and the console goes on asking. Once the upgrade is idle or done it asks
 * `GET /updates`, and when the node runs the target says so: a binary
 * replaced by its new self starts idle, with the new version. After
 * UPGRADE_SILENCE_MS without an answer that shows the upgrade going on it
 * says so and stops asking. It stops when the screen goes away.
 */
function useUpgradeWatch(onView: (view: UpdatesView) => void) {
  const [watch, setWatch] = useState<Watch>(NOT_WATCHING);
  // When an answer last showed the upgrade going on.
  const heard = useRef(0);
  // The target, for the questions of the loop.
  const target = useRef<string | null>(null);
  const tell = useRef(onView);
  useEffect(() => {
    tell.current = onView;
  }, [onView]);

  /** Shows the progress the API gave, and follows it while it is not over. */
  const follow = useCallback((progress: UpgradeProgress, version?: string) => {
    const goal = progress.target ?? version ?? null;
    target.current = goal;
    heard.current = Date.now();
    setWatch({
      ...NOT_WATCHING,
      progress,
      target: goal,
      watching: !isSettled(progress.state) && (goal !== null || progress.state !== 'idle'),
    });
  }, []);

  /** Asks again after the console stopped asking. */
  const resume = useCallback(() => {
    heard.current = Date.now();
    setWatch((previous) => ({ ...previous, watching: true, gaveUp: null, error: null }));
  }, []);

  useEffect(() => {
    if (!watch.watching) {
      return undefined;
    }
    let stopped = false;
    const stop = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const update = (change: Partial<Watch>) => {
      setWatch((previous) => ({ ...previous, ...change }));
    };
    const silentTooLong = () => Date.now() - heard.current >= UPGRADE_SILENCE_MS;
    const again = () => {
      timer = setTimeout(() => {
        void tick();
      }, UPGRADE_POLL_MS);
    };
    const tick = async () => {
      let progress: UpgradeProgress;
      try {
        progress = await unwrap(api.GET('/upgrade', { signal: stop.signal }));
      } catch (error) {
        if (stopped) {
          return;
        }
        if (endsWatch(error)) {
          update({ watching: false, restarting: false, error });
        } else if (silentTooLong()) {
          update({ watching: false, restarting: false, gaveUp: 'silent' });
        } else {
          update({ restarting: true });
          again();
        }
        return;
      }
      if (stopped) {
        return;
      }
      const goal = progress.target ?? target.current;
      target.current = goal;
      if (isSettled(progress.state)) {
        update({ progress, target: goal, watching: false, restarting: false });
        return;
      }
      if (progress.state === 'idle' || progress.state === 'done') {
        if (goal === null) {
          update({ progress, watching: false, restarting: false });
          return;
        }
        let view: UpdatesView;
        try {
          view = await unwrap(api.GET('/updates', { signal: stop.signal }));
        } catch {
          if (stopped) {
            return;
          }
          // The node answered one question and not the next: it is still
          // coming up, or another one answers. Asked again.
          update({ progress, restarting: false });
          again();
          return;
        }
        if (stopped) {
          return;
        }
        tell.current(view);
        if (view.current.version === goal) {
          update({ progress, target: goal, watching: false, restarting: false, upgradedTo: goal });
          return;
        }
        if (progress.state === 'done') {
          heard.current = Date.now();
        } else if (silentTooLong()) {
          // Idle, and on another release than the target, for so long: the
          // node answers, and says nothing of the upgrade.
          update({ progress, watching: false, restarting: false, gaveUp: 'stalled' });
          return;
        }
        update({ progress, target: goal, restarting: false });
        again();
        return;
      }
      heard.current = Date.now();
      update({ progress, target: goal, restarting: false });
      again();
    };
    again();
    return () => {
      stopped = true;
      clearTimeout(timer);
      stop.abort();
    };
  }, [watch.watching]);

  return { watch, follow, resume };
}

interface UpdatesScreenProps {
  /**
   * Whether the session was signed in to with a password, as the API says:
   * only such a session is offered to install a release, because the
   * password is asked for again. The API decides; this chooses what is offered.
   */
  passwordSession?: boolean;
}

/**
 * Which releases there are, and installing one. It shows the build that
 * answers; whether checks for updates are on - they are off until an
 * operator sets CACTUSFS_UPDATE_SOURCE; when the node last asked and what
 * went wrong if it could not read the answer; the releases it found, which
 * of them may be installed and why the others may not; how this deployment
 * installs a release, and the commands to install it without the console.
 *
 * What the release says of itself is not shown: its notes are a link,
 * opened elsewhere and never fetched here. Whether a release may be
 * installed and whether the caller may install it is the API's to say.
 */
export function UpdatesScreen({ passwordSession = false }: UpdatesScreenProps) {
  const loaded = useLoad(loadUpdates);
  const notice = useUpdatesNotice();
  const { report } = notice;
  // What a check or the progress answered since the screen was loaded.
  const [fresh, setFresh] = useState<UpdatesView | null>(null);
  const view = fresh ?? loaded.data;
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<unknown>(null);
  const [checked, setChecked] = useState(false);
  // The release the question "Upgrade to X?" is asked of.
  const [asking, setAsking] = useState<string | null>(null);
  const opener = useRef<HTMLButtonElement | null>(null);
  const progressHeading = useRef<HTMLHeadingElement>(null);
  const shown = useRef(true);
  useEffect(() => {
    shown.current = true;
    return () => {
      shown.current = false;
    };
  }, []);

  const learnt = useCallback(
    (next: UpdatesView) => {
      setFresh(next);
      report(next);
    },
    [report],
  );
  const { watch, follow, resume } = useUpgradeWatch(learnt);

  // What was loaded is what the badge says too.
  useEffect(() => {
    if (loaded.data !== undefined) {
      report(loaded.data);
    }
  }, [loaded.data, report]);

  // An upgrade that is under way, or ended, when the screen is opened is followed.
  const askProgress = useCallback(
    (signal?: AbortSignal) => {
      unwrap(api.GET('/upgrade', { signal })).then(
        (progress) => {
          if (shown.current && progress.state !== 'idle') {
            follow(progress);
          }
        },
        () => undefined,
      );
    },
    [follow],
  );
  useEffect(() => {
    const stop = new AbortController();
    askProgress(stop.signal);
    return () => {
      stop.abort();
    };
  }, [askProgress]);

  const reload = () => {
    setFresh(null);
    setChecked(false);
    loaded.reload();
  };

  const check = () => {
    setChecking(true);
    setCheckError(null);
    setChecked(false);
    checkForUpdates().then(
      (next) => {
        if (shown.current) {
          setChecking(false);
          setChecked(true);
          learnt(next);
        }
      },
      (error: unknown) => {
        if (shown.current) {
          setChecking(false);
          setCheckError(checkProblem(error));
        }
      },
    );
  };

  // Where the keyboard goes once the dialog is gone - not before: until
  // then the page behind it is inert, and takes no focus.
  const focusAfter = useRef<'opener' | 'progress' | null>(null);
  useEffect(() => {
    if (asking !== null || focusAfter.current === null) {
      return;
    }
    const target = focusAfter.current === 'opener' ? opener.current : progressHeading.current;
    focusAfter.current = null;
    target?.focus();
  }, [asking, watch.progress]);

  const closeDialog = () => {
    focusAfter.current = 'opener';
    setAsking(null);
  };

  const started = (progress: UpgradeProgress, version: string) => {
    focusAfter.current = 'progress';
    setAsking(null);
    follow(progress, version);
  };

  const progressState = watch.progress?.state;
  const busy = watch.watching || (progressState !== undefined && isUnderWay(progressState));

  return (
    <>
      <h1>Updates</h1>
      <p>
        <button
          type="button"
          className="btn btn-outline-secondary"
          onClick={reload}
          title="Ask the node what it knows of releases again: no server outside the deployment is asked"
        >
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Reload</span>
        </button>
      </p>
      <ProblemNotice error={loadProblem(loaded.error)} />
      {loaded.loading ? <Loading /> : null}

      {view === undefined ? null : (
        <>
          <section className="card card-body panel" aria-labelledby="updates-build">
            <h2 id="updates-build">This release</h2>
            <BuildFacts view={view} />
          </section>

          <section className="card card-body panel" aria-labelledby="updates-checks">
            <h2 id="updates-checks">Checks for updates</h2>
            {view.source_configured ? (
              <>
                <dl className="facts">
                  <dt>Checks</dt>
                  <dd>On, channel {view.channel}</dd>
                  <dt>Last checked</dt>
                  <dd>{view.checked_at === undefined ? 'Not since the node started' : formatTime(view.checked_at)}</dd>
                  <dt>Last check</dt>
                  <dd>
                    {view.checked_at === undefined
                      ? '-'
                      : view.last_error === undefined
                        ? 'Read'
                        : `Could not be read: ${checkErrorInWords(view.last_error) ?? ''}`}
                  </dd>
                </dl>
                <div className="toolbar">
                  <button
                    type="button"
                    className="btn btn-outline-primary"
                    disabled={checking}
                    onClick={check}
                    title="Ask the update source for its releases now: at most once a minute"
                  >
                    <i className="bi bi-cloud-arrow-down" aria-hidden="true" />{' '}
                    <span className="btn-text">{checking ? 'Checking…' : 'Check now'}</span>
                  </button>
                </div>
                <p className="updates-status" role="status">
                  {checked ? checkedInWords(view) : null}
                </p>
                <ProblemNotice error={checkError} />
              </>
            ) : (
              <div className="updates-off">
                <p>
                  <strong>Update checks are off.</strong> This deployment asks no server about releases until an operator sets{' '}
                  <code>{SOURCE_VARIABLE}</code> (the flag <code>--update-source</code>) to the https address of the releases list
                  - the project’s own, or a mirror inside your network. The README names the address.
                </p>
              </div>
            )}
          </section>

          {view.source_configured ? (
            <ReleasesPanel
              view={view}
              offerUpgrade={passwordSession && view.upgrade_available && !busy}
              onUpgrade={(version, button) => {
                opener.current = button;
                setAsking(version);
              }}
            />
          ) : null}

          <InstallPanel view={view} passwordSession={passwordSession} />

          {watch.progress === null || (watch.progress.state === 'idle' && watch.target === null) ? null : (
            <ProgressPanel watch={watch} heading={progressHeading} onResume={resume} />
          )}
          <ProblemNotice error={watch.error} />

          {asking === null ? null : (
            <UpgradeDialog
              version={asking}
              current={view.current.version}
              mode={view.mode}
              onClose={closeDialog}
              onStarted={started}
              onMaybeRunning={() => {
                askProgress();
              }}
              onUnavailable={reload}
            />
          )}
        </>
      )}
    </>
  );
}

/** What a check found, said once it was made. */
function checkedInWords(view: UpdatesView): string {
  if (view.last_error !== undefined) {
    return `Checked: ${checkErrorInWords(view.last_error) ?? ''}.`;
  }
  const count = view.available.length;
  if (count === 0) {
    return 'Checked: there is no newer release.';
  }
  return `Checked: ${String(count)} newer ${count === 1 ? 'release is' : 'releases are'} listed.`;
}

/** The build that answers, and how this deployment installs a release. */
function BuildFacts({ view }: { view: UpdatesView }) {
  const { current } = view;
  return (
    <dl className="facts">
      <dt>Version</dt>
      <dd>{current.version}</dd>
      <dt>Build</dt>
      <dd>{describeBuild(current).line}</dd>
      <dt>Commit</dt>
      <dd>{current.commit === undefined || current.commit === '' ? '-' : <code>{current.commit}</code>}</dd>
      <dt>Installing</dt>
      <dd>{modeInWords(view.mode)}</dd>
    </dl>
  );
}

interface ReleasesPanelProps {
  view: UpdatesView;
  /** Whether "Upgrade to X" is offered for the releases that may be installed. */
  offerUpgrade: boolean;
  /** Asks the question for a release; the button is where the keyboard returns. */
  onUpgrade: (version: string, button: HTMLButtonElement) => void;
}

/** The releases the node found, and of each whether it may be installed and why not. */
function ReleasesPanel({ view, offerUpgrade, onUpgrade }: ReleasesPanelProps) {
  return (
    <section className="card card-body panel" aria-labelledby="updates-releases">
      <h2 id="updates-releases">Newer releases</h2>
      {view.available.length === 0 ? (
        <p>{view.checked_at === undefined ? 'The node has not checked yet.' : 'There is no newer release.'}</p>
      ) : (
        <div className="table-responsive grid-scroll">
          <table className="table table-hover align-middle grid grid-keep" aria-label="Newer releases">
            <thead>
              <tr>
                <th scope="col">Version</th>
                <th scope="col">Published</th>
                <th scope="col">Notes</th>
                <th scope="col">May be installed</th>
                {offerUpgrade ? <th scope="col">Install</th> : null}
              </tr>
            </thead>
            <tbody>
              {view.available.map((release) => (
                <ReleaseRow key={release.version} release={release} offerUpgrade={offerUpgrade} onUpgrade={onUpgrade} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

interface ReleaseRowProps {
  release: AvailableRelease;
  offerUpgrade: boolean;
  onUpgrade: (version: string, button: HTMLButtonElement) => void;
}

/** One release: nothing of its own text but its version, and a link to its notes. */
function ReleaseRow({ release, offerUpgrade, onUpgrade }: ReleaseRowProps) {
  const notes = release.notes_url;
  return (
    <tr>
      <th scope="row" className="updates-version">
        {release.version}
        {release.prerelease ? (
          <>
            {' '}
            <span className="badge text-bg-warning updates-prerelease">pre-release</span>
          </>
        ) : null}
      </th>
      <td>{formatTime(release.published)}</td>
      <td>
        {notes === undefined || notes === '' ? (
          '-'
        ) : (
          <a href={notes} target="_blank" rel="noopener noreferrer" title={`The notes of ${release.version}, opened in a new tab`}>
            Release notes<span className="visually-hidden"> of {release.version} (opens in a new tab)</span>
          </a>
        )}
      </td>
      <td>
        {release.allowed ? (
          <span className="state state-online">
            <i className="bi bi-check-circle" aria-hidden="true" /> may be installed
          </span>
        ) : (
          <span className="updates-refusal">
            <span className="state">
              <i className="bi bi-dash-circle" aria-hidden="true" /> not offered
            </span>{' '}
            {refusalInWords(release.refusal)}
          </span>
        )}
      </td>
      {offerUpgrade ? (
        <td>
          {release.allowed ? (
            <button
              type="button"
              className="btn btn-primary btn-sm"
              onClick={(event) => {
                onUpgrade(release.version, event.currentTarget);
              }}
              title={`Install ${release.version} on the deployment: it asks for your password first`}
            >
              <i className="bi bi-arrow-up-circle" aria-hidden="true" /> <span className="btn-text">Upgrade to {release.version}</span>
            </button>
          ) : null}
        </td>
      ) : null}
    </tr>
  );
}

/**
 * How this deployment installs a release: whether the console can, and if
 * not why; and the commands to install the next release without it, as
 * text to copy.
 */
function InstallPanel({ view, passwordSession }: { view: UpdatesView; passwordSession: boolean }) {
  const instructions = view.instructions;
  const hasStep = view.next_step !== undefined && view.next_step !== '';
  if (view.upgrade_available && instructions.length === 0 && passwordSession) {
    return null;
  }
  return (
    <section className="card card-body panel" aria-labelledby="updates-install">
      <h2 id="updates-install">Installing</h2>
      {view.upgrade_available ? (
        passwordSession ? null : (
          <p>Installing a release from the console needs a console session signed in with a password: an API key cannot.</p>
        )
      ) : (
        <p>
          <strong>Installing from the console is not available here.</strong>{' '}
          {view.unavailable_reason === undefined || view.unavailable_reason === '' ? null : view.unavailable_reason}
        </p>
      )}
      {instructions.length === 0 ? null : (
        <Instructions lines={instructions} title={hasStep ? `To install ${view.next_step ?? ''} yourself` : 'To install it yourself'} />
      )}
    </section>
  );
}

/** Lines of text to copy, with a button that copies them. */
function Instructions({ lines, title }: { lines: string[]; title: string }) {
  const [copied, setCopied] = useState<'copied' | 'failed' | null>(null);
  const text = lines.join('\n');
  const copy = () => {
    const clipboard = navigator.clipboard as Clipboard | undefined;
    if (clipboard === undefined) {
      setCopied('failed');
      return;
    }
    clipboard.writeText(text).then(
      () => {
        setCopied('copied');
      },
      () => {
        setCopied('failed');
      },
    );
  };
  return (
    <div className="updates-instructions">
      <div className="updates-instructions-head">
        <h3 id="updates-instructions-title">{title}</h3>
        <button type="button" className="btn btn-outline-secondary btn-sm" onClick={copy} title="Copy these lines">
          <i className="bi bi-clipboard" aria-hidden="true" /> <span className="btn-text">Copy</span>
        </button>
      </div>
      <pre className="updates-pre" tabIndex={0} aria-labelledby="updates-instructions-title">
        {text}
      </pre>
      <p className="updates-status" role="status">
        {copied === 'copied' ? 'Copied.' : copied === 'failed' ? 'Could not copy: select the text and copy it.' : null}
      </p>
    </div>
  );
}

interface ProgressPanelProps {
  watch: Watch;
  heading: RefObject<HTMLHeadingElement | null>;
  onResume: () => void;
}

/** How far the upgrade has come, node by node. */
function ProgressPanel({ watch, heading, onResume }: ProgressPanelProps) {
  const progress = watch.progress;
  if (progress === null) {
    return null;
  }
  const nodes = progress.nodes;
  return (
    <section className="card card-body panel" aria-labelledby="updates-progress">
      <h2 id="updates-progress" ref={heading} tabIndex={-1}>
        Upgrade
      </h2>
      <div className="updates-progress-state" role="status" aria-live="polite">
        {watch.upgradedTo !== null ? (
          <p className="alert alert-success notice">
            <i className="bi bi-check-circle-fill" aria-hidden="true" /> Upgraded to {watch.upgradedTo}.
          </p>
        ) : watch.restarting ? (
          <p className="updates-restarting">
            <span className="spinner-border spinner-border-sm" aria-hidden="true" /> Restarting… The node is not answering while
            it restarts; the console goes on asking every {String(UPGRADE_POLL_MS / 1000)} s.
          </p>
        ) : (
          <p>
            <span className={progressClass(progress.state)}>{stateInWords(progress.state)}</span>
            {watch.watching ? ` - asked again every ${String(UPGRADE_POLL_MS / 1000)} s.` : null}
          </p>
        )}
        {watch.gaveUp === 'silent' ? (
          <p className="alert alert-warning notice">
            The node has not answered for 10 minutes. The upgrade may still be going on, or the node may need an operator.
          </p>
        ) : null}
        {watch.gaveUp === 'stalled' ? (
          <p className="alert alert-warning notice">
            For 10 minutes the node has answered that no upgrade is running, and it does not run {watch.target ?? 'the release'}.
            The upgrade may not have taken place: see the audit log and the log of the node.
          </p>
        ) : null}
      </div>
      {watch.gaveUp === null ? null : (
        <p>
          <button type="button" className="btn btn-outline-secondary" onClick={onResume} title="Ask how the upgrade stands again">
            <i className="bi bi-arrow-repeat" aria-hidden="true" /> <span className="btn-text">Keep waiting</span>
          </button>
        </p>
      )}
      <dl className="facts">
        <dt>To</dt>
        <dd>{progress.target ?? watch.target ?? '-'}</dd>
        <dt>Started by</dt>
        <dd>{progress.started_by ?? '-'}</dd>
        <dt>Started at</dt>
        <dd>{formatTime(progress.started_at)}</dd>
      </dl>
      {nodes.length === 0 ? null : <NodesGrid nodes={nodes} />}
    </section>
  );
}

function progressClass(state: UpgradeState): string {
  switch (state) {
    case 'failed':
      return 'state state-offline';
    case 'done':
      return 'state state-online';
    case 'restart-required':
      return 'state state-warning';
    default:
      return 'state';
  }
}

function nodeClass(state: UpgradeNode['state']): string {
  switch (state) {
    case 'done':
      return 'state state-online';
    case 'failed':
      return 'state state-offline';
    case 'restored':
      return 'state state-warning';
    default:
      return 'state';
  }
}

/** The nodes of the upgrade, in the order they are moved. */
function NodesGrid({ nodes }: { nodes: UpgradeNode[] }) {
  return (
    <div className="table-responsive grid-scroll">
      <table className="table table-hover align-middle grid grid-keep" aria-label="Nodes of the upgrade">
        <thead>
          <tr>
            <th scope="col">Node</th>
            <th scope="col">Version</th>
            <th scope="col">Build</th>
            <th scope="col">State</th>
            <th scope="col">Reason</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((node) => (
            <tr key={node.id}>
              <th scope="row">{node.id}</th>
              <td>{node.version ?? '-'}</td>
              <td>{node.build ?? '-'}</td>
              <td>
                <span className={nodeClass(node.state)}>{node.state}</span>
              </td>
              <td>{node.reason === undefined || node.reason === '' ? '-' : node.reason}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
