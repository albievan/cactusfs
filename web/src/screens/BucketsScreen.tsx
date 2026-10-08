import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import { routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime } from '../ui/format';
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

      <section className="card" aria-labelledby="create-bucket">
        <h2 id="create-bucket">Create a bucket</h2>
        <form className="form-row" onSubmit={create}>
          <label className="field">
            <span>Bucket name</span>
            <input
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
          <button type="submit" className="button button-primary" disabled={action.busy}>
            Create bucket
          </button>
        </form>
      </section>

      <section className="card" aria-labelledby="bucket-list">
        <h2 id="bucket-list">All buckets</h2>
        {buckets.loading ? <Loading /> : null}
        {buckets.data !== undefined && buckets.data.length === 0 ? <p>There are no buckets yet.</p> : null}
        {buckets.data !== undefined && buckets.data.length > 0 ? (
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Created</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {buckets.data.map((bucket) => (
                <tr key={bucket.name}>
                  <td data-label="Name">
                    <a href={routeHref(['buckets', bucket.name])}>{bucket.name}</a>
                  </td>
                  <td data-label="Created">{formatTime(bucket.created_at)}</td>
                  <td data-label="Actions">
                    <ConfirmButton
                      label="Delete"
                      question={`Delete bucket ${bucket.name}?`}
                      disabled={action.busy}
                      onConfirm={() => {
                        remove(bucket.name);
                      }}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
      </section>
    </>
  );
}
