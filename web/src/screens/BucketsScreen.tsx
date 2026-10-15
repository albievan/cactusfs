import { useCallback, useState, type FormEvent } from 'react';

import { api, ApiError, unwrap, type Bucket, type BucketActivityEntry, type BucketUsage } from '../api/client';
import { routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatExactBytes, formatGigabytes, formatTime } from '../ui/format';
import { useAction, useLoad, type Loaded } from '../ui/hooks';
import { limiter, type Limiter } from '../ui/limiter';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

/** How many buckets are counted at once: every count is a walk of a bucket's index. */
const USAGE_AT_ONCE = 4;

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
          <div className="table-responsive grid-scroll">
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
  /** True while a change is being carried out. */
  busy: boolean;
  /** Where the row waits its turn to have its bucket counted. */
  queue: Limiter;
  onRemove: (bucket: string) => void;
}

/** One bucket of the grid. How much it holds is asked for by the row, when its turn comes. */
function BucketRow({ bucket, withAccess, withPolicy, busy, queue, onRemove }: BucketRowProps) {
  const { name, details } = bucket;
  const load = useCallback(
    (signal: AbortSignal) =>
      queue(() => unwrap(api.GET('/buckets/{bucket}/usage', { params: { path: { bucket: name } }, signal })), signal),
    [name, queue],
  );
  const usage = useLoad(load);
  // The API says of each bucket by itself what it tells this caller.
  const access = details?.access_included === true ? details : undefined;
  const policy = details?.policy_included === true ? details : undefined;

  return (
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
        <td data-label="Usage" colSpan={2} className="grid-number grid-note">
          Counting…
        </td>
      );
    }
    // Whoever may not list the bucket is told nothing; anything else is a
    // figure that could not be had just now, and can be asked for again.
    if (usage.error instanceof ApiError && usage.error.problem.status === 403) {
      return (
        <td data-label="Usage" colSpan={2} className="grid-number grid-note">
          Usage not shown
        </td>
      );
    }
    return (
      <td data-label="Usage" colSpan={2} className="grid-number grid-note">
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
    </>
  );
}

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
