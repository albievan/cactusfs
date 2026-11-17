import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
} from 'react';

import {
  api,
  ApiError,
  unwrap,
  type BucketReplicaMode,
  type BucketReplicationStatus,
  type ReplicationResync,
  type ReplicationRule,
  type ReplicationStatus,
} from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime } from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { reasonOf } from '../ui/reason';
import { firstOf, Fold, LoadFailure, olderNode, onCode, refused, Said, type Explain } from '../ui/sections';
import { CLASS_HINTS, stateClass } from './TargetsScreen';

const REPLICATION_ON_AN_OLDER_NODE = olderNode('Replication is');

/** What each status of a version means, in the words of the console; the status itself is the API's. */
export const REPLICATION_STATUS_HINTS: Record<ReplicationStatus, string> = {
  PENDING: 'A replication rule of the bucket matched this version, and the other deployment has not taken it yet.',
  COMPLETED: 'The other deployment has this version.',
  FAILED:
    'This version cannot be replicated and was given up on: it is encrypted with a key of the customer, or the other deployment refuses it. This is final.',
  REPLICA: 'This version arrived from another deployment.',
};

/** The question a resync asks first: what it does to every version of the bucket. */
export function resyncQuestion(bucket: string): string {
  return `Resync bucket ${bucket}? It walks every version of the bucket, oldest key first, asks the other deployment for each and sends those it does not have - a load on the link and on the other deployment. Asking again starts the walk from the beginning.`;
}

/** What is said once a resync was started. */
export const RESYNC_STARTED = 'Resync started. It goes on in the background; its progress is shown here.';

/** What is said of an answer 503: the leader that keeps the state could not be asked, or the change was not made yet. */
function unavailable(words: string): Explain {
  return (problem) => (problem.status === 503 ? words : undefined);
}

/** Which keys a rule is for, in words. */
function keysOf(rule: ReplicationRule): string {
  const prefix = rule.prefix ?? '';
  return prefix === '' ? 'every key' : `beginning with ${prefix}`;
}

/** Whether the API answered that the bucket has no replication configuration: 404 `NoSuchEntity`. */
function none(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 404 && error.problem.code === 'NoSuchEntity';
}

/** One name and its value in a list of facts. */
function Fact({ name, children }: { name: string; children: ReactNode }) {
  return (
    <div className="facts-pair">
      <dt>{name}</dt>
      <dd>{children}</dd>
    </div>
  );
}

/**
 * The replication of a bucket (ADR-0026), folded until it is asked for:
 * where its versions are copied to and by which rules, how far that has
 * come, and a resync of every version. The configuration is shown as the
 * API keeps it; it is set through the API. Who may see each part, and who
 * may start a resync, is the API's to say.
 */
export function BucketReplicationSection({ bucket, exact }: { bucket: string; exact?: boolean }) {
  return (
    <Fold title="Replication" what="the replication of this bucket">
      <ReplicationBody bucket={bucket} exact={exact} />
    </Fold>
  );
}

interface ReplicationBodyProps {
  bucket: string;
  /**
   * Whether the bucket's usage is exact, as its details read it: false while
   * it is being counted for the first time, when the API's `pending` is 0 and
   * says nothing yet. Undefined when the usage is not known.
   */
  exact?: boolean;
}

function ReplicationBody({ bucket, exact }: ReplicationBodyProps) {
  const load = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/buckets/{bucket}/replication', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const config = useLoad(load);

  if (config.data === undefined && config.loading) {
    return <Loading />;
  }
  if (config.data === undefined && refused(config.error)) {
    // Starting a resync is an action of its own (admin:ResyncReplication),
    // which the API judges apart from reading the configuration.
    return (
      <>
        <p className="bucket-facts-note">You may not see the replication of this bucket.</p>
        <ResyncOnly bucket={bucket} />
      </>
    );
  }
  if (config.data === undefined && none(config.error)) {
    return <p className="bucket-facts-note">This bucket is not replicated: its versions are copied nowhere.</p>;
  }
  if (config.data === undefined) {
    return (
      <LoadFailure
        error={config.error}
        explain={REPLICATION_ON_AN_OLDER_NODE}
        onRetry={config.reload}
        what="the replication of this bucket"
      />
    );
  }

  const { target, destination_bucket: destination, rules } = config.data;
  return (
    <>
      <dl className="facts bucket-facts">
        <Fact name="Target">{target}</Fact>
        <Fact name="Destination bucket">{destination}</Fact>
      </dl>
      <table className="table table-hover align-middle grid">
        <thead>
          <tr>
            <th scope="col">Rule</th>
            <th scope="col">Status</th>
            <th scope="col">Priority</th>
            <th scope="col">Keys</th>
            <th scope="col">Delete markers</th>
          </tr>
        </thead>
        <tbody>
          {rules.map((rule) => (
            <tr key={rule.id}>
              <td data-label="Rule">{rule.id}</td>
              <td data-label="Status">{rule.enabled ? 'Enabled' : 'Disabled'}</td>
              <td data-label="Priority">{rule.priority}</td>
              <td data-label="Keys">{keysOf(rule)}</td>
              <td data-label="Delete markers">{rule.delete_markers ? 'copied' : 'not copied'}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <ReplicationProgress bucket={bucket} exact={exact} />
    </>
  );
}

/** What a resync record says, in words. */
function resyncWords(resync: ReplicationResync): string {
  const scanned = resync.scanned.toLocaleString('en');
  const sent = resync.sent.toLocaleString('en');
  return `${resync.state}: ${scanned} ${resync.scanned === 1 ? 'version' : 'versions'} looked at, ${sent} sent`;
}

/**
 * How far the replication of a bucket has come - what waits, what failed,
 * how its target stands and the resync, if one was asked for - and the
 * resync itself, started after a question that says what it does.
 */
interface Resync {
  action: Action;
  explain: Explain;
  said: string | null;
  /** Starts a resync; `onStarted` is told the record the API answered. */
  start: () => void;
}

/**
 * A resync of a bucket's replication, started with `POST .../resync`: what
 * the API answers is told to `onStarted`, a refusal is in `action.error`.
 * The console words the API's answer and decides nothing.
 */
function useResync(bucket: string, onStarted: (resync: ReplicationResync) => void): Resync {
  const action = useAction();
  const [said, setSaid] = useState<string | null>(null);
  const explain = firstOf(
    REPLICATION_ON_AN_OLDER_NODE,
    onCode('NoSuchEntity', `Bucket ${bucket} has no replication configuration any more: nothing was started.`),
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
    onCode(
      'InvalidArgument',
      'The target of the configuration is no longer another deployment open to this bucket: nothing was started.',
    ),
    // The console sends every change with a key of its own, so the API does
    // not answer it this; if it does, its words are shown, and no cause.
    onCode('RequestInProgress', 'The API answered that this request is still in progress. Refresh before asking again.'),
    unavailable('The resync could not be started now: there is no leader, or not every node knows replication yet. Try again.'),
  );
  const start = () => {
    void action.run(async () => {
      setSaid(null);
      const answer = await unwrap(
        api.POST('/buckets/{bucket}/replication/resync', {
          params: { path: { bucket } },
          // The request has no body, and says it is JSON all the same, as every POST of the API must.
          headers: { 'Content-Type': 'application/json' },
        }),
      );
      onStarted(answer);
      setSaid(RESYNC_STARTED);
    });
  };
  return { action, explain, said, start };
}

/** Resync alone, for a caller the API does not show the configuration: whether it may start one is the API's to say. */
function ResyncOnly({ bucket }: { bucket: string }) {
  const resync = useResync(bucket, () => undefined);
  return (
    <div>
      <Said text={resync.said === null ? null : 'Resync started. It goes on in the background.'} />
      <ConfirmButton
        label="Resync"
        question={resyncQuestion(bucket)}
        disabled={resync.action.busy}
        onConfirm={resync.start}
      />
      <ProblemNotice error={resync.action.error} explain={resync.explain} />
    </div>
  );
}

function ReplicationProgress({ bucket, exact }: { bucket: string; exact?: boolean }) {
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/replication/status', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const status = useLoad(load);
  // The resync as the API answered its start, with the status it was started
  // over: shown until the status is read again, which says how it stands.
  const [started, setStarted] = useState<{ resync: ReplicationResync; over: BucketReplicationStatus | undefined }>();
  const reload = status.reload;
  const resync = useResync(bucket, (answer) => {
    setStarted({ resync: answer, over: status.data });
    reload();
  });

  return (
    <div>
      <h4>Progress</h4>
      <Said text={resync.said} />
      <StatusFacts
        status={status.data}
        error={status.error}
        loading={status.loading}
        exact={exact}
        started={started !== undefined && started.over === status.data ? started.resync : undefined}
      />
      <div className="d-flex flex-wrap gap-2">
        <button
          type="button"
          className="btn btn-outline-secondary btn-sm"
          onClick={reload}
          title="Ask how far the replication of this bucket has come again"
        >
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Refresh</span>
        </button>
        <ConfirmButton
          label="Resync"
          question={resyncQuestion(bucket)}
          disabled={resync.action.busy}
          onConfirm={resync.start}
        />
      </div>
      <ProblemNotice error={resync.action.error} explain={resync.explain} />
    </div>
  );
}

interface StatusFactsProps {
  status: BucketReplicationStatus | undefined;
  error: unknown;
  loading: boolean;
  /** Whether the bucket's usage is exact; false while it is first counted, when `pending` says nothing yet. */
  exact?: boolean;
  /** The resync as the answer to its start gave it, before the status is read again. */
  started: ReplicationResync | undefined;
}

/** The figures of the status, or why they are not shown - in a line, never an alert. */
function StatusFacts({ status, error, loading, exact, started }: StatusFactsProps) {
  if (status === undefined) {
    let words: string;
    let hint: string | undefined;
    if (loading) {
      words = 'Asking…';
    } else if (refused(error)) {
      words = 'How far the replication of this bucket has come is not visible to you.';
    } else {
      words = 'How far the replication of this bucket has come is unknown now.';
      hint = reasonOf(error, 'The request failed');
    }
    return (
      <p className="bucket-facts-note" title={hint}>
        {words}
      </p>
    );
  }
  const errorClass = status.last_error_class;
  const resync = started ?? status.resync;
  return (
    <>
      {/* Asked again and not answered: what is shown is what was read before. */}
      {error === null || error === undefined ? null : (
        <p className="bucket-facts-note" title={reasonOf(error, 'The request failed')}>
          Could not ask again; shown as last read.
        </p>
      )}
      <dl className="facts bucket-facts">
        <Fact name="State">
          <span className={stateClass(status.target_state)}>{status.target_state}</span>
        </Fact>
        <Fact name="Last error">
          {errorClass === undefined || errorClass === '' ? (
            <span className="bucket-facts-note">none</span>
          ) : (
            <code title={CLASS_HINTS[errorClass]}>{errorClass}</code>
          )}
        </Fact>
        <Fact name="Pending">
          {exact === false ? (
            <span className="bucket-facts-note">not counted yet: the usage of this bucket is still being counted</span>
          ) : (
            <>
              {status.pending.toLocaleString('en')}{' '}
              <span className="bucket-facts-note">versions of this bucket still to be replicated</span>
            </>
          )}
        </Fact>
        {status.oldest_pending_at === undefined ? null : (
          <Fact name="Waiting since">
            {formatTime(status.oldest_pending_at)}{' '}
            <span className="bucket-facts-note">the oldest change for the target, of any bucket</span>
          </Fact>
        )}
        <Fact name="Failed">
          {status.failed.toLocaleString('en')}{' '}
          <span className="bucket-facts-note">versions given up on, for good</span>
        </Fact>
        {status.lost > 0 ? (
          <Fact name="Lost">
            {status.lost.toLocaleString('en')}{' '}
            <span className="bucket-facts-note">changes for the target, of any bucket, dropped before they were sent</span>
          </Fact>
        ) : null}
        <Fact name="Resync">
          {resync === undefined ? <span className="bucket-facts-note">none asked for</span> : resyncWords(resync)}
        </Fact>
      </dl>
      {status.resync_needed ? (
        <p className="form-hint">
          Changes for the target, of any bucket that uses it, were dropped before they were sent: the other deployment
          lacks versions until a resync has been through.
        </p>
      ) : null}
    </>
  );
}

/**
 * Whether a bucket is the replica of another deployment's (ADR-0026),
 * folded until it is asked for: the source, and promotion - the whole of
 * failing over - after the name of the bucket is typed. Who may is the
 * API's to say.
 */
export function BucketReplicaSection({ bucket }: { bucket: string }) {
  return (
    <Fold title="Replica mode" what="whether this bucket is a replica">
      <ReplicaBody bucket={bucket} />
    </Fold>
  );
}

function ReplicaBody({ bucket }: { bucket: string }) {
  const load = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/buckets/{bucket}/replica', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const mode = useLoad(load);
  const action = useAction();
  // The mode as the API answered the promotion with it.
  const [kept, setKept] = useState<BucketReplicaMode | undefined>(undefined);
  const [said, setSaid] = useState<string | null>(null);
  // After a promotion the dialog and the button that opened it are gone:
  // the keyboard goes to the line that says what the bucket is now.
  const outcomeRef = useRef<HTMLParagraphElement>(null);
  const focusOutcome = useRef(false);
  useLayoutEffect(() => {
    if (focusOutcome.current && outcomeRef.current !== null) {
      focusOutcome.current = false;
      outcomeRef.current.focus();
    }
  });

  if (mode.data === undefined && mode.loading) {
    return <Loading />;
  }
  if (mode.data === undefined && refused(mode.error)) {
    return <p className="bucket-facts-note">You may not see whether this bucket is a replica.</p>;
  }
  if (mode.data === undefined) {
    return (
      <LoadFailure
        error={mode.error}
        explain={REPLICATION_ON_AN_OLDER_NODE}
        onRetry={mode.reload}
        what="whether this bucket is a replica"
      />
    );
  }

  const shown = kept ?? mode.data;
  const explain = firstOf(
    REPLICATION_ON_AN_OLDER_NODE,
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
    // The console sends every change with a key of its own, and promotion
    // answers no conflict of its own today: if the API answers 409 all the
    // same, its words are shown, and no cause is claimed for it.
    onCode('RequestInProgress', 'The API answered that this request is still in progress. Read the mode again before asking once more.'),
    onCode('Conflict', `The API answered with a conflict: bucket ${bucket} was not promoted. Read its mode again before asking once more.`),
    unavailable(`Bucket ${bucket} was not promoted: not every node knows replication yet, or the change could not be recorded. Try again.`),
  );
  const promote = (source: string) =>
    action.run(async () => {
      setSaid(null);
      const answer = await unwrap(
        api.PUT('/buckets/{bucket}/replica', { params: { path: { bucket } }, body: { source: '' } }),
      );
      setKept(answer);
      focusOutcome.current = true;
      setSaid(
        `Promoted. Bucket ${bucket} takes writes again and nothing more from deployment ${source}; nothing was sent back.`,
      );
    });

  return (
    <>
      <Said text={said} />
      {!shown.replica || shown.source === undefined ? (
        <p ref={outcomeRef} tabIndex={-1} className="bucket-facts-note">
          This bucket is no replica: it takes writes as any bucket does.
        </p>
      ) : (
        <>
          <dl className="facts bucket-facts">
            <Fact name="Source">
              <code>{shown.source}</code> <span className="bucket-facts-note">the ID of the deployment it copies</span>
            </Fact>
          </dl>
          <p className="form-hint">
            It takes new versions from that deployment only, and refuses every other write with 409 InvalidBucketState.
          </p>
          <Promote bucket={bucket} source={shown.source} action={action} explain={explain} onPromote={promote} />
        </>
      )}
    </>
  );
}

interface PromoteProps {
  /** The bucket to promote, whose name must be typed. */
  bucket: string;
  /** The deployment it is a replica of. */
  source: string;
  action: Action;
  explain: Explain;
  /** Promotes the bucket; resolves to true when the API did. */
  onPromote: (source: string) => Promise<boolean>;
}

/**
 * "Promote": a button, and the question it opens. The question says what
 * promotion does and that it cannot be undone in effect, and asks for the
 * name of the bucket, typed: the button that promotes is enabled once what
 * is typed is that name. It confirms an intent and decides nothing - whether
 * the caller may promote the bucket is the API's to say.
 */
function Promote({ bucket, source, action, explain, onPromote }: PromoteProps) {
  const [asking, setAsking] = useState(false);
  const [typed, setTyped] = useState('');
  const field = useRef<HTMLInputElement>(null);
  const opener = useRef<HTMLButtonElement>(null);
  const titleId = useId();
  const whatId = useId();

  // The keyboard is in the field when the question is asked.
  useEffect(() => {
    if (asking) {
      field.current?.focus();
    }
  }, [asking]);

  // A refusal of an earlier question is not shown in the next, nor after it was put aside.
  const open = () => {
    action.fail(null);
    setAsking(true);
  };
  const close = () => {
    action.fail(null);
    setAsking(false);
    setTyped('');
    opener.current?.focus();
  };
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (typed !== bucket || action.busy) {
      return;
    }
    void onPromote(source).then((promoted) => {
      if (promoted) {
        // The keyboard goes where the outcome is said (ReplicaBody).
        setAsking(false);
        setTyped('');
      } else {
        // Refused: the keyboard goes back to the field, beside the API's words.
        field.current?.focus();
      }
    });
  };
  // Escape puts the question aside, unless the promotion is on its way.
  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === 'Escape' && !action.busy) {
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
        disabled={action.busy}
        onClick={open}
        title="Promote this bucket: it takes writes again and nothing more from its source. It asks for the name of the bucket first"
      >
        <i className="bi bi-box-arrow-up" aria-hidden="true" /> <span className="btn-text">Promote</span>
      </button>
      {asking ? (
        <div className="delete-all" role="dialog" aria-labelledby={titleId} aria-describedby={whatId} onKeyDown={onKeyDown}>
          <h3 id={titleId} className="delete-all-title">
            <i className="bi bi-exclamation-triangle" aria-hidden="true" /> Promote bucket {bucket}?
          </h3>
          <div id={whatId} className="delete-all-what">
            <ul>
              <li>
                It takes writes again, and nothing more from deployment <code>{source}</code>. Promotion is the whole of
                failing over: nothing is sent back.
              </li>
              <li>
                The replication of deployment {source} into this bucket fails from then on (<code>not_replica</code>)
                until it is changed there.
              </li>
              <li>
                A bucket that holds a version cannot be made a replica again: <strong>in effect this cannot be undone</strong>.
              </li>
            </ul>
          </div>
          <form className="delete-all-form" onSubmit={submit}>
            <ProblemNotice error={action.error} explain={explain} />
            <label className="field">
              <span>Type the name of the bucket, {bucket}, to promote it</span>
              <input
                ref={field}
                className="form-control"
                name="confirm_bucket"
                autoCapitalize="none"
                autoComplete="off"
                spellCheck={false}
                required
                value={typed}
                onChange={(change) => {
                  setTyped(change.target.value);
                }}
              />
            </label>
            <div className="confirm">
              <button
                type="submit"
                className="btn btn-danger"
                disabled={action.busy || typed !== bucket}
                title="Promote this bucket: it takes writes again and nothing more from its source"
              >
                <i className="bi bi-box-arrow-up" aria-hidden="true" /> <span className="btn-text">Promote bucket</span>
              </button>
              <button
                type="button"
                className="btn btn-outline-secondary"
                disabled={action.busy}
                onClick={close}
                title="Leave the bucket a replica"
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
