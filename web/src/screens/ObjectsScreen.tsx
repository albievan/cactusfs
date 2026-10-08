import { useCallback, useState, type FormEvent } from 'react';

import { api, objectDownloadUrl, unwrap, uploadObject, type ObjectMetadata } from '../api/client';
import { routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatBytes, formatTime } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

const DELIMITER = '/';

interface ObjectsScreenProps {
  /** The bucket to browse. */
  bucket: string;
  /** The prefix to list; empty for the top of the bucket. */
  prefix: string;
}

/**
 * The object browser of one bucket: walks prefixes as if they were folders,
 * pages through long listings, uploads, downloads, deletes, and shows what is
 * known about an object.
 */
export function ObjectsScreen({ bucket, prefix }: ObjectsScreenProps) {
  // The continuation tokens of the pages before this one and of this one; the
  // first page has none.
  const [tokens, setTokens] = useState<string[]>([]);
  const token = tokens.at(-1);

  const load = useCallback(
    () =>
      unwrap(
        api.GET('/buckets/{bucket}/objects', {
          params: {
            path: { bucket },
            query: {
              prefix: prefix === '' ? undefined : prefix,
              delimiter: DELIMITER,
              continuation_token: token,
            },
          },
        }),
      ),
    [bucket, prefix, token],
  );
  const listing = useLoad(load);
  const action = useAction();
  const [metadata, setMetadata] = useState<ObjectMetadata | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [key, setKey] = useState('');

  const upload = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    if (file === null) {
      return;
    }
    void action.run(async () => {
      await uploadObject(bucket, key, file);
      form.reset();
      setFile(null);
      setKey('');
      listing.reload();
    });
  };

  const remove = (objectKey: string) => {
    void action.run(async () => {
      await unwrap(
        api.DELETE('/buckets/{bucket}/object', { params: { path: { bucket }, query: { key: objectKey } } }),
      );
      setMetadata((shown) => (shown?.key === objectKey ? null : shown));
      listing.reload();
    });
  };

  const showMetadata = (objectKey: string) => {
    void action.run(async () => {
      setMetadata(
        await unwrap(
          api.GET('/buckets/{bucket}/object/metadata', {
            params: { path: { bucket }, query: { key: objectKey } },
          }),
        ),
      );
    });
  };

  const page = listing.data;
  const nextToken = page?.truncated === true ? page.next_continuation_token : undefined;
  const empty = page !== undefined && page.objects.length === 0 && page.common_prefixes.length === 0;

  return (
    <>
      <h1>{bucket}</h1>
      <Breadcrumbs bucket={bucket} prefix={prefix} />
      <ProblemNotice error={action.error ?? listing.error} />

      <section className="card" aria-labelledby="upload-object">
        <h2 id="upload-object">Upload an object</h2>
        <form className="form-row" onSubmit={upload}>
          <label className="field">
            <span>File</span>
            <input
              name="file"
              type="file"
              required
              onChange={(event) => {
                const chosen = event.target.files?.[0] ?? null;
                setFile(chosen);
                setKey(chosen === null ? '' : prefix + chosen.name);
              }}
            />
          </label>
          <label className="field field-wide">
            <span>Object key</span>
            <input
              name="key"
              required
              spellCheck={false}
              value={key}
              onChange={(event) => {
                setKey(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="button button-primary" disabled={action.busy}>
            Upload
          </button>
        </form>
      </section>

      {metadata === null ? null : (
        <section className="card" aria-labelledby="object-metadata">
          <h2 id="object-metadata">Object details</h2>
          <dl className="facts">
            <dt>Bucket</dt>
            <dd>{metadata.bucket}</dd>
            <dt>Key</dt>
            <dd>{metadata.key}</dd>
            <dt>Size</dt>
            <dd>{metadata.size} bytes</dd>
            <dt>ETag</dt>
            <dd>
              <code>{metadata.etag}</code>
            </dd>
            <dt>Last modified</dt>
            <dd>{formatTime(metadata.last_modified)}</dd>
            <dt>Content type</dt>
            <dd>{metadata.content_type}</dd>
            <dt>Encryption</dt>
            <dd>{metadata.encryption}</dd>
          </dl>
          <button
            type="button"
            className="button"
            onClick={() => {
              setMetadata(null);
            }}
          >
            Close details
          </button>
        </section>
      )}

      <section className="card" aria-labelledby="object-list">
        <h2 id="object-list">Objects</h2>
        {listing.loading ? <Loading /> : null}
        {empty ? <p>There is nothing here.</p> : null}
        {page !== undefined && !empty ? (
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Size</th>
                <th scope="col">Last modified</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {page.common_prefixes.map((commonPrefix) => (
                <tr key={`prefix ${commonPrefix}`}>
                  <td data-label="Name">
                    <a href={routeHref(['buckets', bucket], { prefix: commonPrefix })}>
                      {commonPrefix.slice(prefix.length)}
                    </a>
                  </td>
                  <td data-label="Size">-</td>
                  <td data-label="Last modified">-</td>
                  <td data-label="Actions">-</td>
                </tr>
              ))}
              {page.objects.map((object) => {
                const name = object.key.slice(prefix.length);
                return (
                  <tr key={`object ${object.key}`}>
                    <td data-label="Name">{name}</td>
                    <td data-label="Size">{formatBytes(object.size)}</td>
                    <td data-label="Last modified">{formatTime(object.last_modified)}</td>
                    <td data-label="Actions">
                      <span className="actions">
                        <a
                          className="button"
                          href={objectDownloadUrl(bucket, object.key)}
                          download
                          aria-label={`Download ${name}`}
                        >
                          Download
                        </a>
                        <button
                          type="button"
                          className="button"
                          aria-label={`Details of ${name}`}
                          disabled={action.busy}
                          onClick={() => {
                            showMetadata(object.key);
                          }}
                        >
                          Details
                        </button>
                        <ConfirmButton
                          label="Delete"
                          question={`Delete ${name}?`}
                          disabled={action.busy}
                          onConfirm={() => {
                            remove(object.key);
                          }}
                        />
                      </span>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        ) : null}
        <div className="pager">
          <button
            type="button"
            className="button"
            disabled={tokens.length === 0}
            onClick={() => {
              setTokens([]);
            }}
          >
            First page
          </button>
          <button
            type="button"
            className="button"
            disabled={tokens.length === 0}
            onClick={() => {
              setTokens(tokens.slice(0, -1));
            }}
          >
            Previous page
          </button>
          <button
            type="button"
            className="button"
            disabled={nextToken === undefined}
            onClick={() => {
              if (nextToken !== undefined) {
                setTokens([...tokens, nextToken]);
              }
            }}
          >
            Next page
          </button>
          <span>Page {tokens.length + 1}</span>
        </div>
      </section>
    </>
  );
}

function Breadcrumbs({ bucket, prefix }: { bucket: string; prefix: string }) {
  const parts = prefix.split(DELIMITER);
  if (parts.at(-1) === '') {
    parts.pop();
  }
  return (
    <nav aria-label="Prefix">
      <ol className="breadcrumbs">
        <li>
          <a href={routeHref(['buckets'])}>Buckets</a>
        </li>
        <li>
          <a href={routeHref(['buckets', bucket])}>{bucket}</a>
        </li>
        {parts.map((part, index) => {
          const upTo = parts.slice(0, index + 1).join(DELIMITER) + DELIMITER;
          return (
            <li key={upTo}>
              <a href={routeHref(['buckets', bucket], { prefix: upTo })}>{part === '' ? DELIMITER : part}</a>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
