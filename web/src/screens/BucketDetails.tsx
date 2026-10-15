import { useCallback, useEffect, useId, useState, type ReactNode } from 'react';

import { api, ApiError, unwrap, type Bucket, type BucketActivityEntry, type BucketUsage } from '../api/client';
import { useDeletions } from '../deletions/context';
import { formatExactBytes, formatGigabytes, formatTime } from '../ui/format';
import { useLoad, type Loaded } from '../ui/hooks';
import { reasonOf } from '../ui/reason';
import { useUploads } from '../uploads/context';
import { ActivityLine, AtLeast, PolicySummary } from './BucketsScreen';

/** What the API tells of one bucket, as the details of its page show it. */
export interface BucketFacts {
  /** The bucket as the list of buckets has it, with its `details`; undefined when the list does not name it. */
  bucket: Loaded<Bucket | undefined>;
  /** How much it holds. */
  usage: Loaded<BucketUsage>;
}

/**
 * Reads what the API tells of a bucket - the list of buckets with its
 * details, for this bucket, and the bucket's usage - and reads it again
 * when an upload into the bucket or a deletion of its objects is over.
 */
export function useBucketFacts(bucket: string): BucketFacts {
  // The list is the one answer that carries a bucket's details: who made it,
  // its policy, its latest uploads and downloads. What of it the caller may
  // see is the API's to say.
  const loadBucket = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets', { params: { query: { details: true } }, signal })).then((buckets) =>
        buckets.find((one) => one.name === bucket),
      ),
    [bucket],
  );
  const loadUsage = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/buckets/{bucket}/usage', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const facts = { bucket: useLoad(loadBucket), usage: useLoad(loadUsage) };
  const uploads = useUploads();
  const deletions = useDeletions();
  const reloadBucket = facts.bucket.reload;
  const reloadUsage = facts.usage.reload;

  useEffect(() => {
    const again = () => {
      reloadBucket();
      reloadUsage();
    };
    const stopUploads = uploads.listen((event) => {
      if (event.type === 'settled' && event.bucket === bucket) {
        again();
      }
    });
    const stopDeletions = deletions.listen((event) => {
      if (event.type === 'settled' && event.bucket === bucket) {
        again();
      }
    });
    return () => {
      stopUploads();
      stopDeletions();
    };
  }, [uploads, deletions, bucket, reloadBucket, reloadUsage]);

  return facts;
}

// Whether the details are folded away, remembered for this viewer only. It
// is a convenience: without storage the details are simply shown.
const FOLDED_KEY = 'cactusfs.bucketDetails.folded';

function readFolded(): boolean {
  try {
    return window.localStorage.getItem(FOLDED_KEY) === '1';
  } catch {
    return false;
  }
}

function rememberFolded(folded: boolean): void {
  try {
    window.localStorage.setItem(FOLDED_KEY, folded ? '1' : '0');
  } catch {
    // Not remembered, then.
  }
}

/** Whether the API refused to tell the caller this: 403. */
function refused(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 403;
}

/**
 * The details of a bucket at the top of its page: how much it holds, what it
 * keeps besides, when it was made and by whom, its policy and its latest
 * uploads and downloads - as far as the API tells the caller, and in its
 * words. A figure the caller may not see is said to be so, one that is not
 * exact is said to be that: neither is ever shown as 0.
 */
export function BucketDetails({ facts }: { facts: BucketFacts }) {
  const headingId = useId();
  const { usage, bucket } = facts;
  const found = bucket.data;
  const details = found?.details;
  const access = details?.access_included === true ? details : undefined;
  const policy = details?.policy_included === true ? details : undefined;
  const exact = usage.data?.exact === true ? usage.data : undefined;
  const bodyId = useId();
  const [folded, setFolded] = useState(readFolded);
  const toggle = () => {
    const next = !folded;
    rememberFolded(next);
    setFolded(next);
  };

  return (
    <section className="card card-body panel bucket-details" aria-labelledby={headingId}>
      <h2 id={headingId} className="bucket-details-heading">
        <button
          type="button"
          className="bucket-details-toggle"
          aria-expanded={!folded}
          aria-controls={bodyId}
          title={folded ? 'Show the details of this bucket' : 'Hide the details of this bucket'}
          onClick={toggle}
        >
          <i className={`bi ${folded ? 'bi-chevron-right' : 'bi-chevron-down'}`} aria-hidden="true" />
          Bucket details
        </button>
        {folded ? <FoldedSummary usage={usage} /> : null}
      </h2>
      <div id={bodyId} hidden={folded}>
        <dl className="facts bucket-facts">
          <Fact name="Objects">
            <UsageFigure usage={usage} figure={(data) => data.object_count.toLocaleString('en')} />
          </Fact>
          <Fact name="Storage used">
            <UsageFigure
              usage={usage}
              figure={(data) => formatGigabytes(data.total_size)}
              hint={(data) => formatExactBytes(data.total_size)}
            />
          </Fact>
          {exact?.versions === undefined ? null : (
            <Fact name="Versions">
              {exact.versions.toLocaleString('en')}{' '}
              <span className="bucket-facts-note">current and older, delete markers among them</span>
            </Fact>
          )}
          {exact?.stored_bytes === undefined ? null : (
            <Fact name="All versions">
              <span title={formatExactBytes(exact.stored_bytes)}>{formatGigabytes(exact.stored_bytes)}</span>
            </Fact>
          )}
          {exact?.upload_bytes === undefined ? null : (
            <Fact name="Uploads in progress">
              <span title={formatExactBytes(exact.upload_bytes)}>{formatGigabytes(exact.upload_bytes)}</span>{' '}
              <span className="bucket-facts-note">in parts of multipart uploads not completed</span>
            </Fact>
          )}
          {usage.data?.quota_bytes === undefined ? null : (
            <Fact name="Quota">
              <span title={formatExactBytes(usage.data.quota_bytes)}>{formatGigabytes(usage.data.quota_bytes)}</span>
            </Fact>
          )}
          <Fact name="Created">
            <CreatedFigure bucket={bucket} />
          </Fact>
          {access === undefined ? null : <Fact name="Created by">{access.created_by ?? 'not recorded'}</Fact>}
          {policy === undefined ? null : (
            <Fact name="Policy">
              <PolicySummary policy={policy.policy} />
            </Fact>
          )}
          {access === undefined ? null : (
            <>
              <Fact name="Last upload">
                <LatestOf entries={access.activity?.uploads} />
              </Fact>
              <Fact name="Last download">
                <LatestOf entries={access.activity?.downloads} />
              </Fact>
            </>
          )}
        </dl>
        {usage.data !== undefined && !usage.data.exact ? (
          <p className="form-hint">
            These figures were counted for this request from the index, not kept as the bucket changed: they are not
            exact
            {usage.data.complete ? '' : ', and the count stopped at its bound before it reached the end of the bucket'}.
          </p>
        ) : null}
      </div>
    </section>
  );
}

/** What the heading of folded details still says: how much the bucket holds. */
function FoldedSummary({ usage }: { usage: Loaded<BucketUsage> }) {
  const data = usage.data;
  if (data === undefined) {
    if (usage.error === null || usage.error === undefined) {
      return <span className="bucket-details-summary">Counting…</span>;
    }
    return (
      <span className="bucket-details-summary">{refused(usage.error) ? 'not visible to you' : 'not counted'}</span>
    );
  }
  const objects = data.object_count === 1 ? '1 object' : `${data.object_count.toLocaleString('en')} objects`;
  return (
    <span className="bucket-details-summary">
      {objects} · {formatGigabytes(data.total_size)}
      {data.exact ? '' : ' (not exact)'}
    </span>
  );
}

/** One name and its value in the list of facts. */
function Fact({ name, children }: { name: string; children: ReactNode }) {
  return (
    <div className="facts-pair">
      <dt>{name}</dt>
      <dd>{children}</dd>
    </div>
  );
}

interface UsageFigureProps {
  usage: Loaded<BucketUsage>;
  /** The figure, as it is shown. */
  figure: (usage: BucketUsage) => string;
  /** The figure exactly, for the hint, where it is rounded. */
  hint?: (usage: BucketUsage) => string;
}

/**
 * A figure of the bucket's usage: "at least" where the count stopped at its
 * bound, "not exact" where it was counted for this request, "not visible to
 * you" where the API refused, and never a 0 that nobody counted.
 */
function UsageFigure({ usage, figure, hint }: UsageFigureProps) {
  if (usage.data === undefined) {
    if (usage.loading) {
      return <span className="bucket-facts-note">Counting…</span>;
    }
    if (refused(usage.error)) {
      return <span className="bucket-facts-note">not visible to you</span>;
    }
    return (
      <span className="bucket-facts-note">
        could not be counted: {reasonOf(usage.error, 'the request failed')}{' '}
        <button type="button" className="btn btn-outline-secondary btn-sm" onClick={usage.reload} title="Count the objects of this bucket again">
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Count again</span>
        </button>
      </span>
    );
  }
  const data = usage.data;
  const exactly = hint?.(data);
  return (
    <>
      <span title={exactly}>
        {data.complete ? null : <AtLeast />}
        {figure(data)}
      </span>
      {data.exact ? null : <span className="bucket-facts-note"> (not exact)</span>}
    </>
  );
}

/** When the bucket was made, as the list of buckets says; or why that is not shown. */
function CreatedFigure({ bucket }: { bucket: Loaded<Bucket | undefined> }) {
  if (bucket.data !== undefined) {
    return <>{formatTime(bucket.data.created_at)}</>;
  }
  if (bucket.loading) {
    return <span className="bucket-facts-note">Loading…</span>;
  }
  if (bucket.error === null) {
    // The list was read and does not name the bucket: it is not there.
    return <span className="bucket-facts-note">not in the list of buckets</span>;
  }
  return (
    <span className="bucket-facts-note">
      {refused(bucket.error) ? 'not visible to you' : `could not be read: ${reasonOf(bucket.error, 'the request failed')}`}
    </span>
  );
}

/**
 * The latest upload or download: when, who and of what. When the API names
 * none, there was none the caller may see: it leaves out a row of an object
 * the caller may not see, and does not say so.
 */
function LatestOf({ entries }: { entries: BucketActivityEntry[] | undefined }) {
  const latest = entries?.[0];
  if (latest === undefined) {
    return <>none you may see</>;
  }
  return <ActivityLine entry={latest} />;
}
