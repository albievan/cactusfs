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

const UPLOAD_ICON = {
  waiting: 'bi-hourglass',
  sending: 'bi-arrow-up-circle',
  stored: 'bi-check-circle',
  failed: 'bi-x-circle',
} as const;

const UPLOAD_WORD = { waiting: 'Waiting', sending: 'Uploading…', stored: 'Stored', failed: 'Not stored' } as const;

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
  // The files chosen for upload. One file can be given any key; several go
  // under a prefix, each by its own name.
  const [files, setFiles] = useState<File[]>([]);
  const [key, setKey] = useState('');
  // What became of each file of the last upload.
  const [uploads, setUploads] = useState<{ name: string; state: 'waiting' | 'sending' | 'stored' | 'failed' }[]>([]);
  // The keys ticked for deletion, of this page.
  const [chosen, setChosen] = useState<string[]>([]);

  const several = files.length > 1;

  // Uploads the chosen files one after another, each with a request of its
  // own, as the API has it. It stops at the first refusal; what was stored
  // until then stays stored.
  const upload = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    if (files.length === 0) {
      return;
    }
    const sending = files.map((file) => ({ file, key: several ? key + file.name : key }));
    setUploads(sending.map(({ key: objectKey }) => ({ name: objectKey, state: 'waiting' })));
    const mark = (index: number, state: 'sending' | 'stored' | 'failed') => {
      setUploads((list) => list.map((entry, i) => (i === index ? { ...entry, state } : entry)));
    };
    void action
      .run(async () => {
        for (const [index, { file, key: objectKey }] of sending.entries()) {
          mark(index, 'sending');
          try {
            await uploadObject(bucket, objectKey, file);
          } catch (error) {
            mark(index, 'failed');
            throw error;
          }
          mark(index, 'stored');
        }
        form.reset();
        setFiles([]);
        setKey('');
      })
      .then(() => {
        listing.reload();
      });
  };

  const remove = (objectKey: string) => {
    void action.run(async () => {
      await unwrap(
        api.DELETE('/buckets/{bucket}/object', { params: { path: { bucket }, query: { key: objectKey } } }),
      );
      setMetadata((shown) => (shown?.key === objectKey ? null : shown));
      setChosen((left) => left.filter((k) => k !== objectKey));
      listing.reload();
    });
  };

  // Another page is another choice: what was ticked on the page left behind
  // is not carried along, where nobody can see it.
  const turnTo = (pages: string[]) => {
    setChosen([]);
    setTokens(pages);
  };

  const choose = (objectKey: string, on: boolean) => {
    setChosen((keys) => (on ? [...keys.filter((k) => k !== objectKey), objectKey] : keys.filter((k) => k !== objectKey)));
  };

  // Deletes what is ticked on the page shown - the keys the count and the
  // question name, and no others - one request an object, as the API has it.
  // It stops at the first refusal; what was deleted until then stays deleted.
  const removeChosen = (keys: string[]) => {
    void action
      .run(async () => {
        for (const objectKey of keys) {
          await unwrap(
            api.DELETE('/buckets/{bucket}/object', { params: { path: { bucket }, query: { key: objectKey } } }),
          );
          setChosen((left) => left.filter((k) => k !== objectKey));
          setMetadata((shown) => (shown?.key === objectKey ? null : shown));
        }
      })
      .then(() => {
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
  const pageKeys = page?.objects.map((object) => object.key) ?? [];
  // Only what is still on the page counts as chosen.
  const selected = chosen.filter((k) => pageKeys.includes(k));
  const allChosen = pageKeys.length > 0 && selected.length === pageKeys.length;

  return (
    <>
      <h1>{bucket}</h1>
      <Breadcrumbs bucket={bucket} prefix={prefix} />
      <ProblemNotice error={action.error ?? listing.error} />

      <section className="card card-body panel" aria-labelledby="upload-object">
        <h2 id="upload-object">Upload an object</h2>
        <form className="form-row" onSubmit={upload}>
          <label className="field">
            <span>File</span>
            <input
              className="form-control"
              name="file"
              type="file"
              multiple
              required
              onChange={(event) => {
                const chosen = Array.from(event.target.files ?? []);
                setFiles(chosen);
                setUploads([]);
                const [only] = chosen;
                setKey(chosen.length === 1 && only !== undefined ? prefix + only.name : prefix);
              }}
            />
          </label>
          <label className="field field-wide">
            <span>{several ? 'Upload under this prefix' : 'Object key'}</span>
            <input
              className="form-control"
              name="key"
              required={!several}
              spellCheck={false}
              value={key}
              onChange={(event) => {
                setKey(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="btn btn-primary" disabled={action.busy}>
            <i className="bi bi-upload" aria-hidden="true" /> Upload
          </button>
        </form>
        {several ? (
          <p className="form-hint">
            {files.length} files chosen: each is stored under the prefix by its own name. A file whose key already
            exists replaces it.
          </p>
        ) : null}
        {uploads.length > 1 ? (
          <ul className="uploads" aria-label="Upload progress">
            {uploads.map((entry) => (
              <li key={entry.name} className={`upload upload-${entry.state}`}>
                <i className={`bi ${UPLOAD_ICON[entry.state]}`} aria-hidden="true" />
                <span className="upload-name">{entry.name}</span>
                <span className="upload-state">{UPLOAD_WORD[entry.state]}</span>
              </li>
            ))}
          </ul>
        ) : null}
      </section>

      {metadata === null ? null : (
        <section className="card card-body panel" aria-labelledby="object-metadata">
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
            className="btn btn-outline-secondary"
            onClick={() => {
              setMetadata(null);
            }}
          >
            Close details
          </button>
        </section>
      )}

      <section className="card card-body panel" aria-labelledby="object-list">
        <h2 id="object-list">Objects</h2>
        {listing.loading ? <Loading /> : null}
        {empty ? <p>There is nothing here.</p> : null}
        {page !== undefined && pageKeys.length > 0 ? (
          <div className="toolbar">
            <label className="check">
              <input
                className="form-check-input"
                type="checkbox"
                checked={allChosen}
                onChange={(event) => {
                  setChosen(event.target.checked ? pageKeys : []);
                }}
              />
              <span>Select all on this page</span>
            </label>
            <span className="toolbar-count" aria-live="polite">
              {selected.length} selected
            </span>
            <ConfirmButton
              label="Delete selected"
              question={`Delete ${String(selected.length)} ${selected.length === 1 ? 'object' : 'objects'}?`}
              disabled={action.busy || selected.length === 0}
              onConfirm={() => {
                removeChosen(selected);
              }}
            />
          </div>
        ) : null}
        {page !== undefined && !empty ? (
          <table className="table table-hover align-middle grid">
            <thead>
              <tr>
                <th scope="col" className="grid-check">
                  <span className="visually-hidden">Select</span>
                </th>
                <th scope="col">Name</th>
                <th scope="col">Size</th>
                <th scope="col">Last modified</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {page.common_prefixes.map((commonPrefix) => (
                <tr key={`prefix ${commonPrefix}`}>
                  <td className="grid-check" />
                  <td data-label="Name">
                    <a className="btn btn-link-row" href={routeHref(['buckets', bucket], { prefix: commonPrefix })}>
                      <i className="bi bi-folder" aria-hidden="true" />
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
                  <tr key={`object ${object.key}`} className={selected.includes(object.key) ? 'grid-chosen' : undefined}>
                    <td className="grid-check">
                      <input
                        className="form-check-input"
                        type="checkbox"
                        aria-label={`Select ${name}`}
                        checked={selected.includes(object.key)}
                        onChange={(event) => {
                          choose(object.key, event.target.checked);
                        }}
                      />
                    </td>
                    <td data-label="Name">{name}</td>
                    <td data-label="Size">{formatBytes(object.size)}</td>
                    <td data-label="Last modified">{formatTime(object.last_modified)}</td>
                    <td data-label="Actions">
                      <span className="d-flex flex-wrap gap-2">
                        <a
                          className="btn btn-outline-secondary"
                          href={objectDownloadUrl(bucket, object.key)}
                          download
                          aria-label={`Download ${name}`}
                        >
                          <i className="bi bi-download" aria-hidden="true" /> Download
                        </a>
                        <button
                          type="button"
                          className="btn btn-outline-secondary"
                          aria-label={`Details of ${name}`}
                          disabled={action.busy}
                          onClick={() => {
                            showMetadata(object.key);
                          }}
                        >
                          <i className="bi bi-info-circle" aria-hidden="true" /> Details
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
            className="btn btn-outline-secondary"
            disabled={tokens.length === 0}
            onClick={() => {
              turnTo([]);
            }}
          >
            First page
          </button>
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={tokens.length === 0}
            onClick={() => {
              turnTo(tokens.slice(0, -1));
            }}
          >
            Previous page
          </button>
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={nextToken === undefined}
            onClick={() => {
              if (nextToken !== undefined) {
                turnTo([...tokens, nextToken]);
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
      <ol className="crumbs">
        <li>
          <a className="btn btn-outline-secondary btn-sm" href={routeHref(['buckets'])}>
            <i className="bi bi-bucket" aria-hidden="true" />
            Buckets
          </a>
        </li>
        <li>
          <a className="btn btn-outline-secondary btn-sm" href={routeHref(['buckets', bucket])}>
            {bucket}
          </a>
        </li>
        {parts.map((part, index) => {
          const upTo = parts.slice(0, index + 1).join(DELIMITER) + DELIMITER;
          return (
            <li key={upTo}>
              <a className="btn btn-outline-secondary btn-sm" href={routeHref(['buckets', bucket], { prefix: upTo })}>
                {part === '' ? DELIMITER : part}
              </a>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
