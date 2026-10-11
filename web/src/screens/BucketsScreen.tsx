import { useCallback, useState, type FormEvent } from 'react';

import { api, ApiError, unwrap } from '../api/client';
import { routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatBytes, formatTime } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadBuckets() {
  return unwrap(api.GET('/buckets'));
}

/** The buckets: list them, create one, delete an empty one. */
export function BucketsScreen() {
  const buckets = useLoad(loadBuckets);
  const action = useAction();
  const [name, setName] = useState('');

  const create = (event: FormEvent) => {
    event.preventDefault();
    void action.run(async () => {
      await unwrap(api.POST('/buckets', { body: { name } }));
      setName('');
      buckets.reload();
    });
  };

  const remove = (bucket: string) => {
    void action.run(async () => {
      await unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket } } }));
      buckets.reload();
    });
  };

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
          <button type="submit" className="btn btn-primary" disabled={action.busy}>
            <i className="bi bi-plus-lg" aria-hidden="true" /> Create bucket
          </button>
        </form>
      </section>

      <section className="card card-body panel" aria-labelledby="bucket-list">
        <h2 id="bucket-list">All buckets</h2>
        {buckets.loading ? <Loading /> : null}
        {buckets.data !== undefined && buckets.data.length === 0 ? <p>There are no buckets yet.</p> : null}
        {buckets.data !== undefined && buckets.data.length > 0 ? (
          <ul className="row row-cols-1 row-cols-md-2 row-cols-xl-3 g-3 list-unstyled mb-0">
            {buckets.data.map((bucket) => (
              <li className="col" key={bucket.name}>
                <article className="tile h-100">
                  <div className="tile-head">
                    <i className="bi bi-bucket tile-icon" aria-hidden="true" />
                    <div className="tile-text">
                      <h3 className="tile-title">{bucket.name}</h3>
                      <p className="tile-meta">Created {formatTime(bucket.created_at)}</p>
                      <BucketUsage bucket={bucket.name} />
                    </div>
                  </div>
                  <div className="tile-actions">
                    <a
                      className="btn btn-primary btn-sm"
                      href={routeHref(['buckets', bucket.name])}
                      aria-label={`Open ${bucket.name}`}
                    >
                      <i className="bi bi-folder2-open" aria-hidden="true" /> Open
                    </a>
                    <ConfirmButton
                      label="Delete"
                      question={`Delete bucket ${bucket.name}?`}
                      disabled={action.busy}
                      onConfirm={() => {
                        remove(bucket.name);
                      }}
                    />
                  </div>
                </article>
              </li>
            ))}
          </ul>
        ) : null}
      </section>
    </>
  );
}

/**
 * How much a bucket holds: its objects counted and their sizes added up, as
 * the API reports it. Whoever may not list the bucket is told nothing.
 */
function BucketUsage({ bucket }: { bucket: string }) {
  const load = useCallback(
    () => unwrap(api.GET('/buckets/{bucket}/usage', { params: { path: { bucket } } })),
    [bucket],
  );
  const usage = useLoad(load);
  if (usage.data === undefined) {
    if (usage.loading) {
      return <p className="tile-figures">Counting…</p>;
    }
    // Whoever may not list the bucket is told nothing; anything else is a
    // figure that could not be had just now, and can be asked for again.
    if (usage.error instanceof ApiError && usage.error.problem.status === 403) {
      return <p className="tile-figures">Usage not shown</p>;
    }
    return (
      <p className="tile-figures">
        Usage unavailable
        <button type="button" className="btn btn-outline-secondary btn-sm" onClick={usage.reload}>
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> Count again
        </button>
      </p>
    );
  }
  const { object_count: count, total_size: size, complete } = usage.data;
  const more = complete ? '' : ' or more';
  return (
    <p className="tile-figures">
      <span className="badge text-bg-light">
        <i className="bi bi-files" aria-hidden="true" /> {count.toLocaleString('en')}
        {more} {count === 1 && complete ? 'object' : 'objects'}
      </span>
      <span className="badge text-bg-light">
        <i className="bi bi-hdd" aria-hidden="true" /> {formatBytes(size)}
        {more}
      </span>
    </p>
  );
}
