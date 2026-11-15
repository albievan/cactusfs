import { useCallback, useId, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';

import {
  api,
  ApiError,
  unwrap,
  type Bucket,
  type BucketActivityEntry,
  type BucketQuota,
  type BucketUsage,
} from '../api/client';
import { routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import {
  formatExactBytes,
  formatGigabytes,
  formatShare,
  formatTime,
  gigabytesOf,
  isExactBytes,
  parseGigabytes,
  quotaBody,
} from '../ui/format';
import { useAction, useLoad, type Action, type Loaded } from '../ui/hooks';
import { limiter, type Limiter } from '../ui/limiter';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { firstOf, LoadFailure, olderNode, onCode, refused, Said, useFocusAfterChange, type Explain } from '../ui/sections';

/** How many buckets are counted at once: every count is a walk of a bucket's index. */
const USAGE_AT_ONCE = 4;

const QUOTAS_ON_AN_OLDER_NODE = olderNode('Bucket quotas are');

/**
 * How many columns the grid has: the name, the objects, the size, the quota,
 * when it was created and the actions, and those the API told of.
 */
function columnsOf(withAccess: boolean, withPolicy: boolean): number {
  return 6 + (withAccess ? 3 : 0) + (withPolicy ? 1 : 0);
}

// The list is asked for once, with everything the API may tell this caller
// of each bucket. What that is, is the API's to say: a part it left out is a
// part this screen does not show.
function loadBuckets() {
  return unwrap(api.GET('/buckets', { params: { query: { details: true } } }));
}

/** The buckets: list them in a grid, create one, delete an empty one. */
export function BucketsScreen() {
  const buckets = useLoad(loadBuckets);
  const action = useAction();
  const [name, setName] = useState('');
  // One queue for the screen: the rows take their turns in it.
  const [queue] = useState(() => limiter(USAGE_AT_ONCE));

  const create = (event: FormEvent) => {
    event.preventDefault();
    void action.run(
      async () => {
        await unwrap(api.POST('/buckets', { body: { name } }));
        setName('');
        buckets.reload();
      },
      {
        done: `Created bucket ${name}`,
        failed: `Bucket ${name} was not created`,
        link: { href: routeHref(['buckets', name]), label: `Open ${name}` },
      },
    );
  };

  const remove = (bucket: string) => {
    void action.run(
      async () => {
        await unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket } } }));
        buckets.reload();
      },
      { done: `Deleted bucket ${bucket}`, failed: `Bucket ${bucket} was not deleted` },
    );
  };

  // A column is there when the API told its part of at least one bucket. A
  // node of an older release says nothing of any: its list is the plain one.
  const list = buckets.data ?? [];
  const withAccess = list.some((bucket) => bucket.details?.access_included === true);
  const withPolicy = list.some((bucket) => bucket.details?.policy_included === true);
  const columns = columnsOf(withAccess, withPolicy);

  return (
    <>
      <h1>Buckets</h1>
      <ProblemNotice error={action.error ?? buckets.error} />

      <section className="card card-body panel" aria-labelledby="create-bucket">
        <h2 id="create-bucket">Create a bucket</h2>
        <form className="form-row" onSubmit={create}>
          <label className="field">
            <span>Bucket name</span>
            <input
              className="form-control"
              name="name"
              required
              autoCapitalize="none"
              spellCheck={false}
              value={name}
              onChange={(event) => {
                setName(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="btn btn-primary" disabled={action.busy} title="Create a bucket with this name">
            <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Create bucket</span>
          </button>
        </form>
      </section>

      <section className="card card-body panel" aria-labelledby="bucket-list">
        <h2 id="bucket-list">All buckets</h2>
        {buckets.loading ? <Loading /> : null}
        {buckets.data !== undefined && list.length === 0 ? <p>There are no buckets yet.</p> : null}
        {list.length > 0 ? (
          <div className="table-responsive grid-scroll grid-scroll-details">
            <table className={`table table-hover align-middle grid grid-keep${withAccess || withPolicy ? ' grid-wide' : ''}`}>
              <thead>
                <tr>
                  <th scope="col" className="grid-name">
                    Name
                  </th>
                  <th scope="col" className="grid-number">
                    Objects
                  </th>
                  <th scope="col" className="grid-number">
                    Size (GB)
                  </th>
                  <th scope="col">Quota</th>
                  <th scope="col">Created</th>
                  {withAccess ? <th scope="col">Created by</th> : null}
                  {withPolicy ? <th scope="col">Policy</th> : null}
                  {withAccess ? (
                    <>
                      <th scope="col">Last upload</th>
                      <th scope="col">Last download</th>
                    </>
                  ) : null}
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.map((bucket) => (
                  <BucketRow
                    key={bucket.name}
                    bucket={bucket}
                    withAccess={withAccess}
                    withPolicy={withPolicy}
                    columns={columns}
                    busy={action.busy}
                    queue={queue}
                    onRemove={remove}
                  />
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </section>
    </>
  );
}

interface BucketRowProps {
  bucket: Bucket;
  /** Whether the grid has the columns of who created and who used a bucket. */
  withAccess: boolean;
  /** Whether the grid has the column of the policy. */
  withPolicy: boolean;
  /** How many columns the grid has: the row of the quota spans them all. */
  columns: number;
  /** True while a change is being carried out. */
  busy: boolean;
  /** Where the row waits its turn to have its bucket counted. */
  queue: Limiter;
  onRemove: (bucket: string) => void;
}

/**
 * One bucket of the grid. How much it holds is asked for by the row, when its
 * turn comes. Its quota opens under it, in a row of its own.
 */
function BucketRow({ bucket, withAccess, withPolicy, columns, busy, queue, onRemove }: BucketRowProps) {
  const { name, details } = bucket;
  const regionId = useId();
  const [open, setOpen] = useState(false);
  const quotaButton = useRef<HTMLButtonElement>(null);
  const load = useCallback(
    (signal: AbortSignal) =>
      queue(() => unwrap(api.GET('/buckets/{bucket}/usage', { params: { path: { bucket: name } }, signal })), signal),
    [name, queue],
  );
  const usage = useLoad(load);
  // The API says of each bucket by itself what it tells this caller.
  const access = details?.access_included === true ? details : undefined;
  const policy = details?.policy_included === true ? details : undefined;

  const close = () => {
    quotaButton.current?.focus();
    setOpen(false);
  };
  // Escape closes the quota the keyboard is in - it is heard here only when
  // it is - and no other.
  const closeOnEscape = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape' && !event.defaultPrevented) {
      event.stopPropagation();
      close();
    }
  };

  return (
    <>
      <tr>
        <td data-label="Name">
          <a className="btn btn-link-row" href={routeHref(['buckets', name])} title="Open this bucket">
            <i className="bi bi-bucket" aria-hidden="true" />
            {name}
          </a>
        </td>
        <UsageCells usage={usage} />
        <td data-label="Created">{formatTime(bucket.created_at)}</td>
        {withAccess ? <td data-label="Created by">{access === undefined ? null : (access.created_by ?? 'not recorded')}</td> : null}
        {withPolicy ? (
          <td data-label="Policy">{policy === undefined ? null : <PolicySummary policy={policy.policy} />}</td>
        ) : null}
        {withAccess ? (
          <>
            <ActivityCell label="Last upload" entries={access?.activity?.uploads} />
            <ActivityCell label="Last download" entries={access?.activity?.downloads} />
          </>
        ) : null}
        <td data-label="Actions">
          <span className="d-flex flex-wrap gap-2">
            <a
              className="btn btn-outline-secondary"
              href={routeHref(['buckets', name])}
              aria-label={`Open ${name}`}
              title="Open this bucket"
            >
              <i className="bi bi-folder2-open" aria-hidden="true" /> <span className="btn-text">Open</span>
            </a>
            <button
              type="button"
              ref={quotaButton}
              className={`btn btn-outline-secondary${open ? ' active' : ''}`}
              aria-label={`Quota of ${name}`}
              aria-expanded={open}
              aria-controls={regionId}
              onClick={() => {
                setOpen(!open);
              }}
              title={open ? 'Hide the quota of this bucket' : 'Show the quota of this bucket, and change it'}
            >
              <i className="bi bi-speedometer2" aria-hidden="true" /> <span className="btn-text">Quota</span>
            </button>
            <ConfirmButton
              label="Delete"
              question={`Delete bucket ${name}?`}
              disabled={busy}
              onConfirm={() => {
                onRemove(name);
              }}
            />
          </span>
        </td>
      </tr>
      {open ? (
        <tr className="grid-detail">
          <td colSpan={columns}>
            {/* Escape is heard on the region for the controls inside it: the
                region itself is not something to press. */}
            <div
              className="grid-detail-body"
              role="region"
              id={regionId}
              aria-label={`Quota of ${name}`}
              onKeyDown={closeOnEscape}
            >
              <div className="grid-detail-group">
                <h3>Quota</h3>
                <QuotaEditor bucket={name} onChanged={usage.reload} />
              </div>
              <div className="grid-detail-end">
                <button type="button" className="btn btn-outline-secondary" onClick={close} title="Close the quota">
                  <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close quota</span>
                </button>
              </div>
            </div>
          </td>
        </tr>
      ) : null}
    </>
  );
}

/**
 * How much a bucket holds: its objects counted and their sizes added up, as
 * the API reports it. Whoever may not list the bucket is told nothing.
 */
function UsageCells({ usage }: { usage: Loaded<BucketUsage> }) {
  if (usage.data === undefined) {
    if (usage.loading) {
      return (
        <td data-label="Usage" colSpan={3} className="grid-number grid-note">
          Counting…
        </td>
      );
    }
    // Whoever may not list the bucket is told nothing; anything else is a
    // figure that could not be had just now, and can be asked for again.
    if (usage.error instanceof ApiError && usage.error.problem.status === 403) {
      return (
        <td data-label="Usage" colSpan={3} className="grid-number grid-note">
          Usage not shown
        </td>
      );
    }
    return (
      <td data-label="Usage" colSpan={3} className="grid-number grid-note">
        Usage unavailable{' '}
        <button type="button" className="btn btn-outline-secondary" onClick={usage.reload} title="Count the objects of this bucket again">
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Count again</span>
        </button>
      </td>
    );
  }
  const { object_count: count, total_size: size, complete } = usage.data;
  const objects = count.toLocaleString('en');
  return (
    <>
      <td
        data-label="Objects"
        className="grid-number"
        title={complete ? undefined : `${objects} or more: the count stopped at its bound`}
      >
        {complete ? null : <AtLeast />}
        {objects}
      </td>
      <td
        data-label="Size (GB)"
        className="grid-number"
        title={complete ? formatExactBytes(size) : `${formatExactBytes(size)} or more: the count stopped at its bound`}
      >
        {complete ? null : <AtLeast />}
        {formatGigabytes(size)}
      </td>
      <QuotaCell usage={usage.data} />
    </>
  );
}

/**
 * How much of its quota a bucket uses, as its usage tells it: a bar of what
 * counts against the quota - the versions it keeps and the parts of its
 * uploads, `stored_bytes` and `upload_bytes` - over the quota, with the
 * figures in the hint and in words for whoever has them read. "not set"
 * without a quota; "not counted yet" while the usage is not the exact count,
 * which is when the API tells neither - a bucket older than the count being
 * counted, or a deployment that does not count yet. A node of a release
 * before quotas says nothing of either: the cell is there, and empty.
 */
function QuotaCell({ usage }: { usage: BucketUsage }) {
  // `exact` is absent from the answer of a node of an older release.
  const exact: boolean | undefined = usage.exact;
  if (exact === undefined) {
    return <td data-label="Quota" />;
  }
  if (!exact) {
    return (
      <td data-label="Quota" className="grid-note" title="The usage of this bucket is not counted yet: its quota is shown once it is">
        not counted yet
      </td>
    );
  }
  const quota = usage.quota_bytes;
  if (quota === undefined) {
    return (
      <td data-label="Quota" className="grid-note">
        not set
      </td>
    );
  }
  const used = (usage.stored_bytes ?? 0) + (usage.upload_bytes ?? 0);
  const share = formatShare(used, quota);
  const figures = `${formatGigabytes(used)} of ${formatGigabytes(quota)} used (${share}): ${formatExactBytes(used)} of ${formatExactBytes(quota)}`;
  return (
    <td data-label="Quota" title={figures}>
      <span className="quota-cell">
        <meter className="quota-bar" min={0} max={quota} value={Math.min(used, quota)} aria-hidden="true" />
        <span className="quota-share" aria-hidden="true">
          {share}
        </span>
        <span className="visually-hidden">{figures}</span>
      </span>
    </td>
  );
}

interface QuotaEditorProps {
  /** The bucket whose quota it is. */
  bucket: string;
  /** Called once the quota was changed: the row reads the bucket's usage, and so its bar, again. */
  onChanged: () => void;
}

/**
 * The quota of a bucket, read when it is opened, changed in GB or removed.
 * Whether the caller may read it, set it or remove it is the API's to say:
 * the editor is shown when the API answered the read, and not when it
 * refused it; what it answers a change with is shown under the editor.
 */
export function QuotaEditor({ bucket, onChanged }: QuotaEditorProps) {
  const load = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/buckets/{bucket}/quota', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const quota = useLoad(load);
  const action = useAction();
  // The quota as the API answered the last change with it.
  const [kept, setKept] = useState<BucketQuota | undefined>(undefined);
  const [said, setSaid] = useState<string | null>(null);
  const formAreaRef = useRef<HTMLDivElement>(null);
  const focusSaveRef = useRef(false);
  useFocusAfterChange(formAreaRef, focusSaveRef);
  const again = () => {
    setKept(undefined);
    setSaid(null);
    quota.reload();
  };

  if (quota.data === undefined && quota.loading) {
    return <Loading />;
  }
  if (quota.data === undefined && refused(quota.error)) {
    // Not a failure: the API does not tell this caller, and a quota the
    // caller cannot see is not one to change from here.
    return <p className="bucket-facts-note">You may not see the quota of this bucket.</p>;
  }
  if (quota.data === undefined) {
    return (
      <LoadFailure error={quota.error} explain={QUOTAS_ON_AN_OLDER_NODE} onRetry={again} what="the quota of this bucket" />
    );
  }

  const shown = kept ?? quota.data;
  const explain = firstOf(
    QUOTAS_ON_AN_OLDER_NODE,
    onCode('AccessDenied', `You may not change the quota of bucket ${bucket}.`),
    onCode('Conflict', `The usage of bucket ${bucket} is still being counted: a quota can be set once it is. Try again later.`),
    onCode('InvalidArgument', `The quota was refused: bucket ${bucket} keeps the quota it had.`),
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
  );
  const save = async (bytes: bigint) => {
    setSaid(null);
    const answer = await unwrap(
      api.PUT('/buckets/{bucket}/quota', {
        params: { path: { bucket } },
        // The types ask for a number; what is sent is the integer as typed.
        body: { quota_bytes: Number(bytes) },
        bodySerializer: () => quotaBody(bytes),
      }),
    );
    focusSaveRef.current = true;
    setKept(answer);
    setSaid(
      answer.quota_bytes === undefined
        ? 'Saved.'
        : `Saved: bucket ${bucket} may hold ${formatGigabytes(answer.quota_bytes)} (${formatExactBytes(answer.quota_bytes)}).`,
    );
    onChanged();
  };
  const remove = async () => {
    setSaid(null);
    await unwrap(api.DELETE('/buckets/{bucket}/quota', { params: { path: { bucket } } }));
    focusSaveRef.current = true;
    setKept({ used_bytes: shown.used_bytes, exact: shown.exact });
    setSaid(`Removed the quota of bucket ${bucket}.`);
    onChanged();
  };

  return (
    <>
      <Said text={said} />
      <QuotaFacts quota={shown} />
      <div ref={formAreaRef}>
        <QuotaForm
          key={shown.quota_bytes === undefined ? '-' : String(shown.quota_bytes)}
          bucket={bucket}
          initial={shown.quota_bytes}
          action={action}
          explain={explain}
          onSave={save}
          remove={
            shown.quota_bytes === undefined ? undefined : { question: `Remove the quota of bucket ${bucket}?`, run: remove }
          }
        />
      </div>
    </>
  );
}

/**
 * What the API answered of a bucket's quota: the quota, and what counts
 * against it when that is known. A figure beyond 2^53 - 1 bytes is said to be
 * "about" itself: it is the nearest the client reads, not certainly the API's.
 */
function QuotaFacts({ quota }: { quota: BucketQuota }) {
  const { quota_bytes: most, used_bytes: used, exact } = quota;
  return (
    <>
      <p className="bucket-facts-note">
        {most === undefined ? 'This bucket has no quota.' : `Quota: ${formatGigabytes(most)} (${formatExactBytes(most)}).`}
        {exact ? ` Counted against it: ${formatGigabytes(used)} (${formatExactBytes(used)}).` : null}
      </p>
      {exact ? null : (
        <p className="bucket-facts-note">The usage of this bucket is not counted yet: a quota can be set once it is.</p>
      )}
    </>
  );
}

interface QuotaFormProps {
  /** The bucket whose quota it is. */
  bucket: string;
  /** The quota it begins with, in bytes; undefined for none. */
  initial: number | undefined;
  /** Carries the save and the removal out, and holds the error the form shows. */
  action: Action;
  explain: Explain;
  /** Sends the quota, in bytes. */
  onSave: (bytes: bigint) => Promise<void>;
  /** Removes the quota, once asked; absent when there is none to remove. */
  remove?: { question: string; run: () => Promise<void> };
}

/**
 * A quota typed in GB - 10^9 bytes, as the grid shows sizes - and sent in
 * bytes. The console reads only the number: which quotas there may be is
 * the API's to say, and what it says is shown under the field, which keeps
 * what was typed. A quota read beyond 2^53 - 1 bytes is not certainly the
 * API's to the byte: the form then begins empty, so that a Save does not send
 * back a quota a few bytes off the one there is.
 */
function QuotaForm({ bucket, initial, action, explain, onSave, remove }: QuotaFormProps) {
  const hintId = useId();
  const roughId = useId();
  const rough = initial !== undefined && !isExactBytes(initial);
  const [text, setText] = useState(() => (initial === undefined || rough ? '' : gigabytesOf(BigInt(initial))));

  const save = (event: FormEvent) => {
    event.preventDefault();
    let bytes: bigint;
    try {
      bytes = parseGigabytes(text);
    } catch (error) {
      action.fail(error);
      return;
    }
    void action.run(() => onSave(bytes));
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <label className="field">
        <span>Quota of bucket {bucket} in GB</span>
        <input
          className="form-control"
          name="quota"
          required
          inputMode="text"
          autoComplete="off"
          spellCheck={false}
          aria-describedby={rough ? `${roughId} ${hintId}` : hintId}
          value={text}
          onChange={(event) => {
            setText(event.target.value);
          }}
        />
      </label>
      {rough ? (
        <p id={roughId} className="form-hint field-full">
          {QUOTA_ROUGH_HINT}
        </p>
      ) : null}
      <p id={hintId} className="form-hint field-full">
        {QUOTA_HINT}
      </p>
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button
          type="submit"
          className="btn btn-primary"
          disabled={action.busy}
          title="Set this quota in place of the one the bucket has"
        >
          <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save quota</span>
        </button>
        {remove === undefined ? null : (
          <ConfirmButton
            label="Remove quota"
            question={remove.question}
            disabled={action.busy}
            onConfirm={() => {
              void action.run(remove.run);
            }}
          />
        )}
      </div>
    </form>
  );
}

/** What the quota editor says under its field: the unit, and what the contract says a quota counts and does. */
export const QUOTA_HINT =
  'Type digits, with "." as the decimal point. 1 GB is 1,000,000,000 bytes, as the grid shows sizes. Every version the bucket keeps and the parts of uploads not completed count against the quota; an upload that would take the bucket past it is refused. A quota below what the bucket holds is accepted: nothing is removed, and nothing more is taken until enough was deleted.';

/** What the quota editor says when it begins empty: the quota is too large for the console to show to the byte. */
export const QUOTA_ROUGH_HINT =
  'The quota of this bucket is larger than the console reads to the byte (2^53 bytes, about 9 PB), so the field begins empty: type a quota to set it in place of the one there is.';

/** Marks a figure of a count that stopped at its bound: the sign for the eye, the words for whoever has it read. */
export function AtLeast() {
  return (
    <>
      <span aria-hidden="true">≥ </span>
      <span className="visually-hidden">at least </span>
    </>
  );
}

/** What the policy of a bucket is, as the API sums it up; undefined is a bucket without one. */
export function PolicySummary({ policy }: { policy: { statements: number; open_to_anyone: boolean } | undefined }) {
  if (policy === undefined) {
    return <>none</>;
  }
  return (
    <span className="grid-badges">
      <span className="badge text-bg-light">
        {policy.statements.toLocaleString('en')} {policy.statements === 1 ? 'statement' : 'statements'}
      </span>
      {policy.open_to_anyone ? (
        <span
          className="badge text-bg-warning"
          title="A statement of this policy allows something to every caller. Its conditions and what other statements deny are not weighed here."
        >
          <i className="bi bi-globe2" aria-hidden="true" /> open to all callers
        </span>
      ) : null}
    </span>
  );
}

// How who uploaded or downloaded is worded: an upload of before uploaders
// were recorded has nobody.
function whoOf(by: string): string {
  return by === '' ? 'not recorded' : by;
}

/** One upload or download in a line of text, for a hint. */
function lineOf(entry: BucketActivityEntry): string {
  const mark = entry.presigned === true ? ' (presigned URL)' : '';
  const key = entry.key === undefined ? '' : ` · ${entry.key}`;
  return `${formatTime(entry.at)} · ${whoOf(entry.by)}${mark}${key}`;
}

/**
 * The latest uploads or downloads of a bucket: the newest in the cell, the
 * ones before it in the hint of the cell and, for whoever has no pointer to
 * hover with, behind a line that opens. `entries` is undefined for a bucket
 * the API tells nothing of.
 */
function ActivityCell({ label, entries }: { label: string; entries: BucketActivityEntry[] | undefined }) {
  if (entries === undefined) {
    return <td data-label={label} />;
  }
  const [latest, ...earlier] = entries;
  if (latest === undefined) {
    return <td data-label={label}>never</td>;
  }
  return (
    <td data-label={label} title={earlier.length === 0 ? undefined : `Before that:\n${earlier.map(lineOf).join('\n')}`}>
      <ActivityLine entry={latest} />
      {earlier.length === 0 ? null : (
        <details className="activity-earlier">
          <summary>{earlier.length} earlier</summary>
          <ul className="list-unstyled mb-0">
            {earlier.map((entry, index) => (
              <li key={`${String(index)} ${entry.at} ${entry.by} ${entry.key ?? ''}`}>
                <ActivityLine entry={entry} />
              </li>
            ))}
          </ul>
        </details>
      )}
    </td>
  );
}

/** One upload or download: when and who, and under it the object, when the API names it. */
export function ActivityLine({ entry }: { entry: BucketActivityEntry }) {
  return (
    <span className="activity-line">
      <span>
        {formatTime(entry.at)} · {whoOf(entry.by)}
      </span>
      {entry.presigned === true ? (
        <span className="badge text-bg-light" title="Made with a URL this user signed: whoever held the URL made it">
          presigned URL
        </span>
      ) : null}
      {entry.key === undefined ? null : <span className="activity-key">{entry.key}</span>}
    </span>
  );
}
