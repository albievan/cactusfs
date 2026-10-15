import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';

import {
  api,
  ApiError,
  objectDownloadUrl,
  unwrap,
  type ObjectAccessSummary,
  type ObjectList,
  type ObjectMetadata,
} from '../api/client';
import { useDeleting, useDeletionList, useDeletions } from '../deletions/context';
import { doneOf, outcomeOf, progressLineOf, totalOf } from '../deletions/state';
import { offersPreview } from '../preview/kinds';
import { PreviewButton } from '../preview/PreviewButton';
import { routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatBytes, formatTime } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { reasonOf } from '../ui/reason';
import { useUploadList, useUploads } from '../uploads/context';
import { UPLOADS_AT_ONCE } from '../uploads/engine';
import { totalsOf } from '../uploads/state';
import { UploadTile } from '../uploads/UploadTile';
import { BucketDetails, useBucketFacts } from './BucketDetails';
import { DeleteAll } from './DeleteAll';

const DELIMITER = '/';

interface ObjectsScreenProps {
  /** The bucket to browse. */
  bucket: string;
  /** The prefix to list; empty for the top of the bucket. */
  prefix: string;
  /**
   * Whether the session was signed in to with a password, as the API says:
   * only then is "Delete all" offered, which asks for it again.
   */
  passwordSession?: boolean;
}

/**
 * How long the list waits, while files keep being stored, before it is read
 * again: thirty-nine files that are stored one after the other are not
 * thirty-nine readings of the list.
 */
const RELOAD_EVERY_MS = 1000;

// How who uploaded an object and when it was last downloaded are worded: in
// its details and in the grid alike.
function uploaderOf(uploadedBy: string | undefined): string {
  return uploadedBy ?? 'not recorded';
}

function lastDownloadOf(lastDownloadedAt: string | undefined): string {
  return lastDownloadedAt === undefined ? 'never' : formatTime(lastDownloadedAt);
}

/** An object as a listing has it. */
type ObjectSummary = ObjectList['objects'][number];

/** A column of the grid of objects, as its heading names it. */
interface Column {
  /** What the heading says. */
  label: string;
  /** The class of the heading, where it has one. */
  className?: string;
  /** True where the heading is read out and not shown. */
  hidden?: boolean;
}

// The columns that say who uploaded an object and how often it was
// downloaded: there only when the listing carries that.
const ACCESS_COLUMNS: readonly Column[] = [
  { label: 'Uploaded by' },
  { label: 'Uploaded' },
  { label: 'Downloads', className: 'grid-number' },
  { label: 'Copies', className: 'grid-number' },
  { label: 'Last downloaded' },
];

/**
 * The columns of the grid of objects. The headings are drawn from this, and
 * the details of an object, which lie across the whole grid, span as many
 * columns as it names.
 */
function columnsOf(withAccess: boolean): readonly Column[] {
  return [
    { label: 'Select', className: 'grid-check', hidden: true },
    { label: 'Name', className: 'grid-name' },
    { label: 'Size' },
    { label: 'Last modified' },
    ...(withAccess ? ACCESS_COLUMNS : []),
    { label: 'Actions' },
  ];
}

/**
 * What tells one version of an object from the next in a listing: when it
 * changes, the object was replaced and what was known of it no longer holds.
 */
function stampOf(object: ObjectSummary): string {
  return `${object.etag} ${object.last_modified}`;
}

/** What was read of an object, and of which version of it. */
interface KnownDetails {
  /** The stamp of the listed object the details were asked for. */
  stamp: string;
  /** What the API said of it. */
  metadata: ObjectMetadata;
}

/**
 * The object browser of one bucket: walks prefixes as if they were folders,
 * pages through long listings, uploads, downloads, deletes, and shows what is
 * known about an object - in the grid, under the row of the object, and of
 * as many objects at a time as are opened.
 *
 * The uploads are the console's, not this screen's: the form hands the files
 * over, and they go on when another screen is shown. What is shown here of
 * them is how the uploads into this bucket stand - the same on coming back.
 * So are the deletions of several objects - of those ticked, and of every
 * object of the bucket - and so is what is shown of them.
 *
 * Above it all, the details of the bucket, read again when an upload into
 * it or a deletion of its objects is over.
 */
export function ObjectsScreen({ bucket, prefix, passwordSession = false }: ObjectsScreenProps) {
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
              // Who uploaded each object and how often it was downloaded,
              // with the listing: the API says whether the caller may see it.
              access: true,
            },
          },
        }),
      ),
    [bucket, prefix, token],
  );
  const listing = useLoad(load);
  const action = useAction();
  // The keys of the objects whose details are open, of this page. Several can
  // be: one compares objects by reading them side by side.
  const [open, setOpen] = useState<string[]>([]);
  // What was read of the objects of this page, kept for as long as the page
  // is shown: an object is asked about once, and again only when the listing
  // says that it was replaced.
  const [known, setKnown] = useState<ReadonlyMap<string, KnownDetails>>(() => new Map());
  // The ids of the regions the details are in start with this. They are made
  // of the place of the row, never of the key: a key holds any character.
  const detailsId = useId();
  const uploads = useUploads();
  const deletions = useDeletions();
  const deleting = useDeleting(bucket);
  const facts = useBucketFacts(bucket);
  // Whether the password of "Delete all" is being checked: nothing else is
  // deleted meanwhile from here.
  const [checkingPassword, setCheckingPassword] = useState(false);
  // The deletion "Delete all" just began: the keyboard goes to its progress.
  const [focusDeletion, setFocusDeletion] = useState<string | null>(null);
  const focused = useCallback(() => {
    setFocusDeletion(null);
  }, []);
  const uploadForm = useRef<HTMLFormElement>(null);
  // The files chosen for upload. One file can be given any key; several go
  // under a prefix, each by its own name.
  const [files, setFiles] = useState<File[]>([]);
  const [key, setKey] = useState('');
  // The batch this form sent off and that is still on its way: until it is
  // over the form sends nothing more, so that the same files are not sent
  // twice by a second click.
  const submitted = useRef<string | null>(null);
  // The files that batch was made of, and the files that are chosen now: the
  // form is emptied when the batch is stored only if they are still the same.
  const submittedFiles = useRef<File[] | null>(null);
  const chosenFiles = useRef<File[]>(files);
  const [sending, setSending] = useState(false);
  // The keys ticked for deletion, of this page.
  const [chosen, setChosen] = useState<string[]>([]);

  const several = files.length > 1;
  const { reload } = listing;

  // What the uploads tell this screen. A file stored under the prefix that
  // is shown belongs in the list, so the list is read again: when the batch
  // the file is of is over, and while files keep being stored a moment after
  // the first of them, once for all that were stored in it. A batch of which
  // a file was cancelled is read after too: the API may have stored the file
  // before it heard that it was given up. And when the batch this form sent
  // off is over, the form is free again - and empty, if every file of it was
  // stored and no other files were chosen meanwhile.
  useEffect(() => {
    // Whether something was stored that the list does not show yet.
    let behind = false;
    let later: ReturnType<typeof setTimeout> | undefined;
    const catchUp = () => {
      clearTimeout(later);
      later = undefined;
      if (behind) {
        behind = false;
        reload();
      }
    };
    const stopListening = uploads.listen((event) => {
      if (event.type === 'stored') {
        if (event.bucket === bucket && event.key.startsWith(prefix)) {
          behind = true;
          later ??= setTimeout(catchUp, RELOAD_EVERY_MS);
        }
        return;
      }
      if (event.bucket === bucket) {
        if (event.cancelled > 0) {
          behind = true;
        }
        catchUp();
      }
      if (event.batch === submitted.current) {
        const sent = submittedFiles.current;
        submitted.current = null;
        submittedFiles.current = null;
        setSending(false);
        // Files chosen while these were on their way are another choice:
        // it stays, with its key, for the next upload.
        if (event.stored === event.files && chosenFiles.current === sent) {
          uploadForm.current?.reset();
          chosenFiles.current = [];
          setFiles([]);
          setKey('');
        }
      }
    });
    return () => {
      stopListening();
      clearTimeout(later);
    };
  }, [uploads, bucket, prefix, reload]);

  // What the deletions tell this screen. An object that was deleted is no
  // longer ticked and its details close; the list is read again a moment
  // after objects under the prefix shown were deleted - once for all that
  // were deleted in that moment - and when a deletion of the bucket is over.
  useEffect(() => {
    let behind = false;
    let later: ReturnType<typeof setTimeout> | undefined;
    const catchUp = () => {
      clearTimeout(later);
      later = undefined;
      if (behind) {
        behind = false;
        reload();
      }
    };
    const stopListening = deletions.listen((event) => {
      if (event.bucket !== bucket) {
        return;
      }
      if (event.type === 'deleted') {
        setChosen((left) => left.filter((k) => k !== event.key));
        setOpen((keys) => keys.filter((k) => k !== event.key));
        setKnown((old) => new Map([...old].filter(([k]) => k !== event.key)));
        if (event.key.startsWith(prefix)) {
          behind = true;
          later ??= setTimeout(catchUp, RELOAD_EVERY_MS);
        }
        return;
      }
      behind = true;
      catchUp();
    });
    return () => {
      stopListening();
      clearTimeout(later);
    };
  }, [deletions, bucket, prefix, reload]);

  // Hands the files over to the console's uploads: they are sent a few at a
  // time, and go on whatever screen is shown.
  const upload = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (files.length === 0 || submitted.current !== null) {
      return;
    }
    submittedFiles.current = files;
    submitted.current = uploads.start(
      bucket,
      files.map((file) => ({ file, key: several ? key + file.name : key })),
    );
    setSending(true);
  };

  // An object that was deleted has no details: its row closes.
  const forget = (objectKey: string) => {
    setOpen((keys) => keys.filter((k) => k !== objectKey));
    setKnown((old) => new Map([...old].filter(([k]) => k !== objectKey)));
  };

  const remove = (objectKey: string) => {
    void action.run(
      async () => {
        await unwrap(
          api.DELETE('/buckets/{bucket}/object', { params: { path: { bucket }, query: { key: objectKey } } }),
        );
        forget(objectKey);
        setChosen((left) => left.filter((k) => k !== objectKey));
        listing.reload();
      },
      { done: `Deleted ${objectKey} from ${bucket}`, failed: `${objectKey} was not deleted from ${bucket}` },
    );
  };

  // Another page is another choice: what was ticked on the page left behind
  // is not carried along, where nobody can see it. Nor are the details that
  // were open on it.
  const turnTo = (pages: string[]) => {
    setChosen([]);
    setOpen([]);
    setKnown(new Map());
    setTokens(pages);
  };

  const choose = (objectKey: string, on: boolean) => {
    setChosen((keys) => (on ? [...keys.filter((k) => k !== objectKey), objectKey] : keys.filter((k) => k !== objectKey)));
  };

  // Deletes what is ticked on the page shown - the keys the count and the
  // question name, and no others - one request an object, as the API has it,
  // several at once through the console's deletions. A refusal is counted
  // with its reason and the others go on. It goes on when another screen is
  // shown, and how it ended is said as a notice, here or not.
  const removeChosen = (keys: string[]) => {
    deletions.deleteKeys(bucket, keys);
  };

  const toggleDetails = useCallback((objectKey: string) => {
    setOpen((keys) => (keys.includes(objectKey) ? keys.filter((k) => k !== objectKey) : [...keys, objectKey]));
  }, []);

  const closeDetails = useCallback((objectKey: string) => {
    setOpen((keys) => keys.filter((k) => k !== objectKey));
  }, []);

  const remember = useCallback((objectKey: string, stamp: string, metadata: ObjectMetadata) => {
    setKnown((old) => new Map(old).set(objectKey, { stamp, metadata }));
  }, []);

  const page = listing.data;
  // When the list was read again: the details of an object that is still in
  // it stay open, those of one that is gone close - and stay closed should a
  // key of that name come back.
  const [listed, setListed] = useState(page);
  if (page !== listed) {
    setListed(page);
    const there = new Set(page?.objects.map((object) => object.key) ?? []);
    setOpen((keys) => keys.filter((k) => there.has(k)));
    setKnown((old) => new Map([...old].filter(([k]) => there.has(k))));
  }
  const nextToken = page?.truncated === true ? page.next_continuation_token : undefined;
  const empty = page !== undefined && page.objects.length === 0 && page.common_prefixes.length === 0;
  // Whether the bucket holds nothing to delete, as far as is known: its usage
  // counted no object to the end, or the top of it lists nothing.
  const bucketEmpty =
    (facts.usage.data?.complete === true && facts.usage.data.object_count === 0) || (prefix === '' && empty);
  const withDeleteAll = passwordSession && page !== undefined && !bucketEmpty;
  const pageKeys = page?.objects.map((object) => object.key) ?? [];
  // Only what is still on the page counts as chosen.
  const selected = chosen.filter((k) => pageKeys.includes(k));
  const allChosen = pageKeys.length > 0 && selected.length === pageKeys.length;
  // Whether the listing says who uploaded its objects and how often they were
  // downloaded. Whether the caller may see that is for the API to decide:
  // without it the columns are not there at all.
  const withAccess = page?.access_included === true;
  const columns = columnsOf(withAccess);

  return (
    <>
      <h1>{bucket}</h1>
      <Breadcrumbs bucket={bucket} prefix={prefix} />
      <ProblemNotice error={action.error ?? listing.error} />
      <BucketDetails facts={facts} />

      <section className="card card-body panel" aria-labelledby="upload-object">
        <h2 id="upload-object">Upload an object</h2>
        <form className="form-row" ref={uploadForm} onSubmit={upload}>
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
                chosenFiles.current = chosen;
                setFiles(chosen);
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
          <button type="submit" className="btn btn-primary" disabled={sending} title="Upload the chosen files to this bucket">
            <i className="bi bi-upload" aria-hidden="true" /> <span className="btn-text">Upload</span>
          </button>
        </form>
        {several ? (
          <p className="form-hint">
            {files.length} files chosen: each is stored under the prefix by its own name, {UPLOADS_AT_ONCE} at a time. A
            file whose key already exists replaces it.
          </p>
        ) : null}
        <p className="form-hint">
          An upload goes on when you open another screen of the console, and the bar at the top says how it stands.
          Reloading or closing the page stops it.
        </p>
        <BucketUploads bucket={bucket} />
      </section>

      <section className="card card-body panel" aria-labelledby="object-list">
        <h2 id="object-list">Objects</h2>
        {listing.loading ? <Loading /> : null}
        {listing.error !== null && page !== undefined ? (
          <p className="list-stale" role="status">
            The list could not be read again: what is shown is the last one that could.{' '}
            <button type="button" className="btn btn-outline-secondary btn-sm" onClick={listing.reload} title="Read the list of objects again">
              <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Reload</span>
            </button>
          </p>
        ) : null}
        {empty && listing.error === null ? <p>There is nothing here.</p> : null}
        <BucketDeletions
          bucket={bucket}
          focusId={focusDeletion}
          onFocused={focused}
        />
        {page !== undefined && (pageKeys.length > 0 || withDeleteAll) ? (
          <div className="toolbar">
            {pageKeys.length > 0 ? (
              <>
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
                  disabled={action.busy || deleting || checkingPassword || selected.length === 0}
                  onConfirm={() => {
                    removeChosen(selected);
                  }}
                />
              </>
            ) : (
              <span className="toolbar-count" />
            )}
            {withDeleteAll ? (
              <DeleteAll
                bucket={bucket}
                usage={facts.usage}
                disabled={deleting}
                onChecking={setCheckingPassword}
                onConfirmed={(expected) => {
                  setFocusDeletion(deletions.deleteAll(bucket, expected));
                }}
              />
            ) : null}
          </div>
        ) : null}
        {page !== undefined && !empty ? (
          <div className="table-responsive grid-scroll grid-scroll-details">
            <table className={`table table-hover align-middle grid grid-keep${withAccess ? ' grid-wide' : ''}`}>
              <thead>
                <tr>
                  {columns.map((column) => (
                    <th key={column.label} scope="col" className={column.className}>
                      {column.hidden === true ? <span className="visually-hidden">{column.label}</span> : column.label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {page.common_prefixes.map((commonPrefix) => (
                  <tr key={`prefix ${commonPrefix}`}>
                    <td className="grid-check" />
                    <td data-label="Name">
                      <a
                        className="btn btn-link-row"
                        href={routeHref(['buckets', bucket], { prefix: commonPrefix })}
                        title="Open this folder"
                      >
                        <i className="bi bi-folder" aria-hidden="true" />
                        {commonPrefix.slice(prefix.length)}
                      </a>
                    </td>
                    <td data-label="Size">-</td>
                    <td data-label="Last modified">-</td>
                    {withAccess ? <AccessCells access={undefined} /> : null}
                    <td data-label="Actions">-</td>
                  </tr>
                ))}
                {page.objects.map((object, index) => (
                  <ObjectRow
                    key={`object ${object.key}`}
                    bucket={bucket}
                    object={object}
                    name={object.key.slice(prefix.length)}
                    regionId={`${detailsId}-details-${String(index)}`}
                    columns={columns.length}
                    withAccess={withAccess}
                    chosen={selected.includes(object.key)}
                    open={open.includes(object.key)}
                    known={known.get(object.key)}
                    busy={action.busy}
                    onChoose={choose}
                    onToggle={toggleDetails}
                    onClose={closeDetails}
                    onKnown={remember}
                    onRemove={remove}
                  />
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
        <div className="pager">
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={tokens.length === 0}
            onClick={() => {
              turnTo([]);
            }}
            title="Go to the first page"
          >
            <i className="bi bi-chevron-double-left" aria-hidden="true" /> <span className="btn-text">First page</span>
          </button>
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={tokens.length === 0}
            onClick={() => {
              turnTo(tokens.slice(0, -1));
            }}
            title="Go to the page before"
          >
            <i className="bi bi-chevron-left" aria-hidden="true" /> <span className="btn-text">Previous page</span>
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
            title="Go to the next page"
          >
            <i className="bi bi-chevron-right" aria-hidden="true" /> <span className="btn-text">Next page</span>
          </button>
          <span>Page {tokens.length + 1}</span>
        </div>
      </section>
    </>
  );
}

/**
 * How the uploads into one bucket stand, whichever screen they were sent off
 * from and whatever prefix is shown: a tile a file, with what was stored and
 * what was not counted above them. It is a part of its own, so that the
 * progress of the files draws this again and not the whole screen.
 */
function BucketUploads({ bucket }: { bucket: string }) {
  const uploads = useUploads();
  const all = useUploadList();
  const mine = all.filter((upload) => upload.bucket === bucket);
  if (mine.length === 0) {
    return null;
  }
  const totals = totalsOf(mine);
  return (
    <>
      <p className="uploads-summary" role="status">
        <strong>
          {totals.stored} of {totals.files} stored
        </strong>
        {totals.failed > totals.undecided ? (
          <span className="uploads-failed"> · {totals.failed - totals.undecided} not stored</span>
        ) : null}
        {totals.undecided > 0 ? <span className="uploads-failed"> · {totals.undecided} not confirmed</span> : null}
        {totals.cancelled > 0 ? <span className="uploads-cancelled"> · {totals.cancelled} cancelled</span> : null}
        {totals.failed > 0 && totals.inFlight === 0 ? (
          <button
            type="button"
            className="btn btn-outline-danger btn-sm"
            onClick={() => {
              uploads.retryFailed({ bucket });
            }}
            title="Send again the files that were not stored"
          >
            <i className="bi bi-arrow-repeat" aria-hidden="true" /> <span className="btn-text">Retry failed</span>
          </button>
        ) : null}
        {totals.inFlight > 0 ? null : (
          <button
            type="button"
            className="btn btn-outline-secondary btn-sm"
            onClick={() => {
              uploads.dismissFinished({ bucket });
            }}
            title="Clear the upload status"
          >
            <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Clear</span>
          </button>
        )}
      </p>
      <ul className="uploads" aria-label="Upload progress">
        {mine.map((upload) => (
          <UploadTile key={upload.id} upload={upload} barLabel={`Upload of ${upload.key}`} onCancel={uploads.cancel} />
        ))}
      </ul>
    </>
  );
}

/**
 * How the deletions of objects of one bucket stand, whichever screen they
 * were begun from: while one is on its way, how many of how many with a bar
 * and a button that cancels what has not begun; once it is over, what was
 * deleted, what was not and why, until that is cleared. It is a part of its
 * own, so that the progress draws this again and not the whole screen.
 */
interface BucketDeletionsProps {
  bucket: string;
  /** A deletion whose "Cancel deletion" is to have the keyboard, once it is shown. */
  focusId: string | null;
  /** Told once the keyboard was put there. */
  onFocused: () => void;
}

function BucketDeletions({ bucket, focusId, onFocused }: BucketDeletionsProps) {
  const deletions = useDeletions();
  const mine = useDeletionList().filter((deletion) => deletion.bucket === bucket);
  const cancelButtons = useRef(new Map<string, HTMLButtonElement>());
  useEffect(() => {
    if (focusId !== null) {
      cancelButtons.current.get(focusId)?.focus();
      onFocused();
    }
  }, [focusId, onFocused]);
  if (mine.length === 0) {
    return null;
  }
  const running = mine.filter((deletion) => deletion.running);
  const over = mine.filter((deletion) => !deletion.running);
  return (
    <div className="deletions">
      {/* What is read out: that objects are being deleted, and none of the
          figures that change as they are - those are for the eye. */}
      <p className="visually-hidden" aria-live="polite">
        {running.length > 0 ? `Deleting objects from ${bucket}.` : ''}
      </p>
      {running.map((deletion) => {
        const total = totalOf(deletion);
        return (
          <p key={deletion.id} className="uploads-summary">
            <strong>{progressLineOf(deletion, false)}</strong>
            <progress
              className="upload-bar deletions-bar"
              aria-label={`Deletion from ${bucket}`}
              value={total.count === 0 ? 0 : Math.min(1, doneOf(deletion) / total.count)}
              max={1}
            />
            <button
              ref={(button) => {
                if (button === null) {
                  cancelButtons.current.delete(deletion.id);
                } else {
                  cancelButtons.current.set(deletion.id, button);
                }
              }}
              type="button"
              className="btn btn-outline-danger btn-sm"
              disabled={deletion.cancelled}
              onClick={() => {
                deletions.cancel(deletion.id);
              }}
              title="Stop deleting: what has not begun is not sent, what is on its way is answered and counted"
            >
              <i className="bi bi-stop-circle" aria-hidden="true" /> <span className="btn-text">Cancel deletion</span>
            </button>
          </p>
        );
      })}
      {over.map((deletion) => {
        const outcome = outcomeOf(deletion);
        return (
          <div
            key={deletion.id}
            className={`alert notice ${outcome.kind === 'error' ? 'alert-danger' : outcome.kind === 'success' ? 'alert-success' : 'alert-info'}`}
            role={outcome.kind === 'error' ? 'alert' : 'status'}
          >
            <p className="notice-title">{outcome.title}</p>
            {outcome.detail === undefined ? null : <p>{outcome.detail}</p>}
          </div>
        );
      })}
      {over.length > 0 ? (
        <button
          type="button"
          className="btn btn-outline-secondary btn-sm"
          onClick={() => {
            deletions.dismissFinished(bucket);
          }}
          title="Clear what is said of the deletions that are over"
        >
          <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Clear</span>
        </button>
      ) : null}
    </div>
  );
}

interface ObjectRowProps {
  /** The bucket the object is in. */
  bucket: string;
  /** The object, as the listing has it. */
  object: ObjectSummary;
  /** Its key below the prefix that is shown. */
  name: string;
  /** The id of the region its details are in when they are open. */
  regionId: string;
  /** How many columns the grid has: the details lie across all of them. */
  columns: number;
  /** Whether the grid says who uploaded its objects and how often they were downloaded. */
  withAccess: boolean;
  /** Whether the object is ticked. */
  chosen: boolean;
  /** Whether its details are open. */
  open: boolean;
  /** What was read of it before, if anything. */
  known: KnownDetails | undefined;
  /** True while a change is being carried out. */
  busy: boolean;
  /** Ticks the object, or not. */
  onChoose: (objectKey: string, on: boolean) => void;
  /** Opens its details, or closes them. */
  onToggle: (objectKey: string) => void;
  /** Closes its details. */
  onClose: (objectKey: string) => void;
  /** Keeps what was read of it. */
  onKnown: (objectKey: string, stamp: string, metadata: ObjectMetadata) => void;
  /** Deletes it. */
  onRemove: (objectKey: string) => void;
}

/**
 * The row of an object, and under it - when it is opened by the name or by
 * the details button - a row with everything that is known of the object,
 * across the whole grid. Opening it moves nothing: the page is not scrolled
 * and the keyboard stays on what opened it. Closing it from inside, with its
 * button or with Escape, puts the keyboard back there.
 */
function ObjectRow({
  bucket,
  object,
  name,
  regionId,
  columns,
  withAccess,
  chosen,
  open,
  known,
  busy,
  onChoose,
  onToggle,
  onClose,
  onKnown,
  onRemove,
}: ObjectRowProps) {
  const nameButton = useRef<HTMLButtonElement>(null);
  const detailsButton = useRef<HTMLButtonElement>(null);
  // Which of the two opened the details: where the keyboard goes back to.
  const opener = useRef<'name' | 'details'>('name');
  const hint = open ? 'Hide details' : 'Show details';

  const toggle = (by: 'name' | 'details') => {
    opener.current = by;
    onToggle(object.key);
  };

  const close = () => {
    (opener.current === 'details' ? detailsButton : nameButton).current?.focus();
    onClose(object.key);
  };

  // Escape closes the details the keyboard is in - it is heard here only
  // when it is - and no others.
  const closeOnEscape = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape' && !event.defaultPrevented) {
      event.stopPropagation();
      close();
    }
  };

  return (
    <>
      <tr className={chosen ? 'grid-chosen' : undefined}>
        <td className="grid-check">
          <input
            className="form-check-input"
            type="checkbox"
            aria-label={`Select ${name}`}
            checked={chosen}
            onChange={(event) => {
              onChoose(object.key, event.target.checked);
            }}
          />
        </td>
        <td data-label="Name">
          <button
            type="button"
            ref={nameButton}
            className="grid-name-toggle"
            aria-expanded={open}
            aria-controls={regionId}
            onClick={() => {
              toggle('name');
            }}
            title={hint}
          >
            <i className="bi bi-chevron-right grid-name-mark" aria-hidden="true" />
            {name === '' ? object.key : name}
          </button>
        </td>
        <td data-label="Size">{formatBytes(object.size)}</td>
        <td data-label="Last modified">{formatTime(object.last_modified)}</td>
        {withAccess ? <AccessCells access={object.access} /> : null}
        <td data-label="Actions">
          <span className="d-flex flex-wrap gap-2">
            <a
              className="btn btn-outline-secondary"
              href={objectDownloadUrl(bucket, object.key)}
              download
              aria-label={`Download ${name}`}
              title="Download this object"
            >
              <i className="bi bi-download" aria-hidden="true" /> <span className="btn-text">Download</span>
            </a>
            <button
              type="button"
              ref={detailsButton}
              className={`btn btn-outline-secondary${open ? ' active' : ''}`}
              aria-label={`Details of ${name}`}
              aria-expanded={open}
              aria-controls={regionId}
              onClick={() => {
                toggle('details');
              }}
              title={hint}
            >
              <i className={open ? 'bi bi-info-circle-fill' : 'bi bi-info-circle'} aria-hidden="true" />{' '}
              <span className="btn-text">Details</span>
            </button>
            <ConfirmButton
              label="Delete"
              question={`Delete ${name}?`}
              disabled={busy}
              onConfirm={() => {
                onRemove(object.key);
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
              aria-label={`Details of ${object.key}`}
              onKeyDown={closeOnEscape}
            >
              <ObjectDetails bucket={bucket} object={object} known={known} onKnown={onKnown} />
              <div className="grid-detail-end">
                <button type="button" className="btn btn-outline-secondary" onClick={close} title="Close the details">
                  <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close details</span>
                </button>
              </div>
            </div>
          </td>
        </tr>
      ) : null}
    </>
  );
}

interface ObjectDetailsProps {
  /** The bucket the object is in. */
  bucket: string;
  /** The object, as the listing has it. */
  object: ObjectSummary;
  /** What was read of it before, if anything. */
  known: KnownDetails | undefined;
  /** Keeps what is read of it. */
  onKnown: (objectKey: string, stamp: string, metadata: ObjectMetadata) => void;
}

/**
 * Everything that is known of one object: what it is, and who uploaded and
 * downloaded it. It is asked for when the details are first opened and when
 * the listing says the object was replaced, and not otherwise. That it is on
 * its way, and that it could not be had, is said here, in the row: the
 * details of one object failing are no failure of the screen, nor of the
 * details of another. Where the stored type is one the API may preview,
 * "Preview" is offered beside what is known.
 */
function ObjectDetails({ bucket, object, known, onKnown }: ObjectDetailsProps) {
  const objectKey = object.key;
  const stamp = stampOf(object);
  const fresh = known?.stamp === stamp;
  // How often it was asked for again after a failure.
  const [attempt, setAttempt] = useState(0);
  const [failure, setFailure] = useState<{ stamp: string; attempt: number; error: unknown } | null>(null);

  useEffect(() => {
    if (fresh) {
      return undefined;
    }
    let current = true;
    const abandoned = new AbortController();
    unwrap(
      api.GET('/buckets/{bucket}/object/metadata', {
        params: { path: { bucket }, query: { key: objectKey } },
        signal: abandoned.signal,
      }),
    ).then(
      (metadata) => {
        if (current) {
          onKnown(objectKey, stamp, metadata);
        }
      },
      (error: unknown) => {
        if (current) {
          setFailure({ stamp, attempt, error });
        }
      },
    );
    return () => {
      current = false;
      abandoned.abort();
    };
  }, [bucket, objectKey, stamp, attempt, fresh, onKnown]);

  if (known !== undefined && fresh) {
    const { metadata } = known;
    return (
      <>
        <div className="grid-detail-group">
          <h3>Object</h3>
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
          {/* Offered from the type the API told; whether it is shown is the API's answer. */}
          {offersPreview(metadata.content_type) ? (
            <div className="grid-detail-actions">
              <PreviewButton bucket={metadata.bucket} objectKey={metadata.key} contentType={metadata.content_type} />
            </div>
          ) : null}
        </div>
        <div className="grid-detail-group">
          {/* Asked for again when the object was replaced: another upload has another uploader. */}
          <ObjectAccessFacts key={stamp} bucket={metadata.bucket} objectKey={metadata.key} />
        </div>
      </>
    );
  }
  if (failure !== null && failure.stamp === stamp && failure.attempt === attempt) {
    return (
      <div className="grid-detail-group grid-detail-problem">
        <ProblemNotice error={failure.error} />
        <button
          type="button"
          className="btn btn-outline-secondary"
          onClick={() => {
            setAttempt((n) => n + 1);
          }}
          title="Ask for the details of this object again"
        >
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Try again</span>
        </button>
      </div>
    );
  }
  return (
    <div className="grid-detail-group">
      <Loading />
    </div>
  );
}

/**
 * The cells of a row of the grid that say who uploaded an object and how
 * often it was downloaded, worded as the details of the object word them.
 * They are empty for a folder, and for an object the API says nothing of.
 */
function AccessCells({ access }: { access: ObjectAccessSummary | undefined }) {
  if (access === undefined) {
    return (
      <>
        <td data-label="Uploaded by" />
        <td data-label="Uploaded" />
        <td data-label="Downloads" className="grid-number" />
        <td data-label="Copies" className="grid-number" />
        <td data-label="Last downloaded" />
      </>
    );
  }
  return (
    <>
      <td data-label="Uploaded by">{uploaderOf(access.uploaded_by)}</td>
      <td data-label="Uploaded">{formatTime(access.uploaded_at)}</td>
      <td data-label="Downloads" className="grid-number">
        {access.download_count}
      </td>
      <td data-label="Copies" className="grid-number">
        {access.copy_count}
      </td>
      <td data-label="Last downloaded">{lastDownloadOf(access.last_downloaded_at)}</td>
    </>
  );
}

/**
 * Who uploaded an object and who downloaded it, as the API tells it. Seeing
 * it takes a permission of its own: a refusal is said in one line, and is no
 * failure of the details above it.
 */
function ObjectAccessFacts({ bucket, objectKey }: { bucket: string; objectKey: string }) {
  const load = useCallback(
    () =>
      unwrap(
        api.GET('/buckets/{bucket}/object/access', {
          params: { path: { bucket }, query: { key: objectKey } },
        }),
      ),
    [bucket, objectKey],
  );
  const access = useLoad(load);
  // Several objects can be open at once: each heading has an id of its own.
  const headingId = useId();

  if (access.loading) {
    return <Loading />;
  }
  const record = access.data;
  if (access.error !== null || record === undefined) {
    const refused = access.error instanceof ApiError && access.error.problem.status === 403;
    return (
      <p className="form-hint object-access-note">
        {refused
          ? 'You may not see who accessed this object.'
          : `Who accessed this object could not be loaded: ${reasonOf(access.error, 'the request failed')}`}
      </p>
    );
  }
  return (
    <section className="object-access" aria-labelledby={headingId}>
      <h3 id={headingId}>Uploads and downloads</h3>
      <dl className="facts">
        <dt>Uploaded by</dt>
        <dd>{uploaderOf(record.uploaded_by)}</dd>
        <dt>Uploaded</dt>
        <dd>{formatTime(record.uploaded_at)}</dd>
        <dt>Downloads</dt>
        <dd>{record.download_count}</dd>
        <dt>Copies</dt>
        <dd>{record.copy_count}</dd>
        <dt>Last downloaded</dt>
        <dd>{lastDownloadOf(record.last_downloaded_at)}</dd>
      </dl>
      {record.readers.length > 0 ? (
        <div className="table-responsive">
          <table className="table table-hover align-middle grid">
            <caption className="object-access-caption">Downloaded by</caption>
            <thead>
              <tr>
                <th scope="col">Who</th>
                <th scope="col">Downloads</th>
                <th scope="col">Copies</th>
                <th scope="col">Last time</th>
              </tr>
            </thead>
            <tbody>
              {record.readers.map((reader) => (
                <tr key={`${reader.who} ${String(reader.presigned)}`}>
                  <td data-label="Who">
                    {reader.who}{' '}
                    {reader.presigned ? (
                      <span
                        className="badge text-bg-light"
                        title="Downloaded with a link this reader signed: whoever held the link, not necessarily the signer"
                      >
                        presigned link
                      </span>
                    ) : null}
                  </td>
                  <td data-label="Downloads">{reader.downloads}</td>
                  <td data-label="Copies">{reader.copies}</td>
                  <td data-label="Last time">{formatTime(reader.last_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {record.other_readers > 0 ? (
        <p className="form-hint">
          and {record.other_readers} {record.other_readers === 1 ? 'other' : 'others'}, with {record.other_downloads}{' '}
          {record.other_downloads === 1 ? 'download' : 'downloads'} and {record.other_copies}{' '}
          {record.other_copies === 1 ? 'copy' : 'copies'} between them
        </p>
      ) : null}
      <p className="form-hint">
        These figures are counts kept as well as can be, not an audit trail: a download may be recorded up to half a
        minute or so after it was made, and a node that fails loses what it had counted and not yet recorded.
      </p>
    </section>
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
          <a className="btn btn-outline-secondary btn-sm" href={routeHref(['buckets'])} title="Back to all buckets">
            <i className="bi bi-bucket" aria-hidden="true" />
            Buckets
          </a>
        </li>
        <li>
          <a
            className="btn btn-outline-secondary btn-sm"
            href={routeHref(['buckets', bucket])}
            title="The top of this bucket"
          >
            {bucket}
          </a>
        </li>
        {parts.map((part, index) => {
          const upTo = parts.slice(0, index + 1).join(DELIMITER) + DELIMITER;
          return (
            <li key={upTo}>
              <a
                className="btn btn-outline-secondary btn-sm"
                href={routeHref(['buckets', bucket], { prefix: upTo })}
                title="Go to this folder"
              >
                {part === '' ? DELIMITER : part}
              </a>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
