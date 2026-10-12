import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';

import { api, objectDownloadUrl, unwrap, type SearchResult, type SearchResults } from '../api/client';
import { navigate, routeHref } from '../router';
import { formatBytes, formatTime } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

const DELIMITER = '/';

/** How the API says it read the query, in words. */
const READ_AS: Record<SearchResults['match'], string> = {
  substring: 'text',
  wildcard: 'a wildcard',
  regex: 'a regular expression',
};

/** The problem code of "you are searching already, or the node is busy searching". */
const TOO_MANY_REQUESTS = 'TooManyRequests';
/** How long to wait before asking again when the API does not say, and the longest wait, in seconds. */
const RETRY_AFTER_DEFAULT = 1;
const RETRY_AFTER_MOST = 3;

/** What a search is asked: the query, and where to go on. */
interface SearchQuery {
  q: string;
  continuation?: string;
}

/** How many milliseconds an answer asks to wait before the request is made again. */
function retryAfter(response: Response): number {
  const seconds = Number(response.headers.get('Retry-After') ?? '');
  const wait = Number.isFinite(seconds) && seconds > 0 ? seconds : RETRY_AFTER_DEFAULT;
  return Math.min(wait, RETRY_AFTER_MOST) * 1000;
}

/** Waits, unless the signal says that nobody waits for the outcome any more: then it rejects. */
function pause(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const givenUp = () => {
      clearTimeout(timer);
      reject(new DOMException('The search was given up', 'AbortError'));
    };
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', givenUp);
      resolve();
    }, milliseconds);
    if (signal.aborted) {
      givenUp();
      return;
    }
    signal.addEventListener('abort', givenUp, { once: true });
  });
}

/**
 * Asks the API for one page of a search.
 *
 * The API runs one search at a time for a caller. A search that was given up
 * a moment ago - the query was refined - may not have let go of its place
 * yet, and the API then refuses the next with 429 `TooManyRequests`: for the
 * user's own previous search, which they no longer wait for. That refusal is
 * not shown: the page is asked for once more after the time the API names,
 * and what comes then is the answer, whatever it is. While waiting, the
 * signal is honoured like during the request.
 */
async function searchPage(query: SearchQuery, signal: AbortSignal): Promise<SearchResults> {
  const ask = () => api.GET('/search', { params: { query }, signal });
  let answer = await ask();
  if (answer.response.status === 429 && answer.error?.code === TOO_MANY_REQUESTS) {
    await pause(retryAfter(answer.response), signal);
    answer = await ask();
  }
  return unwrap(Promise.resolve(answer));
}

interface SearchScreenProps {
  /** What to search for, as it stands in the address; empty when nothing was asked yet. */
  query: string;
}

/**
 * Search: buckets by their name and objects by their key. What is found, how
 * the query is read and what the caller may see are the API's to decide; the
 * screen sends what was typed and shows the answer, page after page.
 */
export function SearchScreen({ query }: SearchScreenProps) {
  const [typed, setTyped] = useState(query);
  // The pages after the first, in the order they were asked for.
  const [further, setFurther] = useState<SearchResults[]>([]);
  const more = useAction();

  // The request for a further page that is on its way, if one is.
  const furtherRequest = useRef<AbortController | null>(null);

  // A search nobody waits for any more is given up, not left to run: the
  // API runs one search at a time for a caller, and would refuse the next
  // one for as long as this one went on.
  const loadFirst = useCallback(
    (signal: AbortSignal): Promise<SearchResults | undefined> =>
      query === '' ? Promise.resolve(undefined) : searchPage({ q: query }, signal),
    [query],
  );
  const first = useLoad(loadFirst);
  useEffect(
    () => () => {
      furtherRequest.current?.abort();
    },
    [],
  );

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const q = typed.trim();
    if (q === '') {
      return;
    }
    if (q === query) {
      // The address stays as it is: ask again, from the first page. What
      // the pages after it said, and why one of them failed, goes with them.
      furtherRequest.current?.abort();
      setFurther([]);
      more.fail(null);
      first.reload();
      return;
    }
    navigate(routeHref(['search'], { q }));
  };

  const pages = first.data === undefined ? [] : [first.data, ...further];
  const last = pages.at(-1);
  const results = pages.flatMap((page) => page.results);
  const examined = pages.reduce((sum, page) => sum + page.examined, 0);

  const searchFurther = () => {
    const continuation = last?.next_continuation;
    if (continuation === undefined) {
      return;
    }
    const request = new AbortController();
    furtherRequest.current = request;
    void more.run(async () => {
      try {
        const next = await searchPage({ q: query, continuation }, request.signal);
        if (!request.signal.aborted) {
          setFurther((before) => [...before, next]);
        }
      } catch (cause) {
        // A page that was given up did not fail: there is nothing to say.
        if (!request.signal.aborted) {
          throw cause;
        }
      }
    });
  };

  return (
    <>
      <h1>Search</h1>

      <section className="card card-body panel" aria-labelledby="search-for">
        <h2 id="search-for">Search buckets and objects</h2>
        <form className="form-row" role="search" aria-label="Search buckets and objects" onSubmit={submit}>
          <label className="field field-wide">
            <span>Search for</span>
            <input
              className="form-control"
              type="search"
              name="q"
              required
              maxLength={256}
              autoCapitalize="none"
              autoComplete="off"
              spellCheck={false}
              aria-describedby="search-hint"
              value={typed}
              onChange={(event) => {
                setTyped(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="btn btn-primary" disabled={more.busy} title="Search the buckets and objects you may list">
            <i className="bi bi-search" aria-hidden="true" /> <span className="btn-text">Search</span>
          </button>
        </form>
        <p className="search-hint" id="search-hint">
          Three ways to search: <code>text</code> finds the names that contain it, whatever their case;{' '}
          <code>wild*card?</code> is a pattern for a whole name, where <code>*</code> is any run of characters and{' '}
          <code>?</code> exactly one; <code>rx:^regular expression$</code> is a regular expression, case as written.
        </p>
        {/* Beside the field: a pattern the API could not read is said here, in its words. */}
        <ProblemNotice error={more.error ?? first.error} />
        {first.error !== null && first.error !== undefined && query !== '' ? (
          <p className="search-more">
            <button type="button" className="btn btn-outline-secondary" onClick={first.reload} title="Send this search again">
              <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Search again</span>
            </button>
          </p>
        ) : null}
      </section>

      {query === '' ? null : (
        <section className="card card-body panel" aria-labelledby="search-results">
          <h2 id="search-results">Results</h2>
          {first.loading ? <Loading /> : null}
          {last === undefined ? null : (
            <>
              <p className="search-figures" role="status">
                {results.length === 0
                  ? last.complete
                    ? 'Nothing found.'
                    : 'Nothing found so far.'
                  : `${results.length.toLocaleString('en')} found${last.complete ? '' : ' so far'}.`}{' '}
                Examined {examined.toLocaleString('en')} {examined === 1 ? 'key' : 'keys'}, reading the search as{' '}
                {READ_AS[last.match]}.
              </p>
              {results.length === 0 ? null : (
                <div className="table-responsive">
                  <table className="table table-hover align-middle grid">
                    <thead>
                      <tr>
                        <th scope="col">Kind</th>
                        <th scope="col">Bucket</th>
                        <th scope="col">Key</th>
                        <th scope="col">Size</th>
                        <th scope="col">Last modified</th>
                        <th scope="col">Actions</th>
                      </tr>
                    </thead>
                    <tbody>
                      {results.map((result) => (
                        <ResultRow key={`${result.kind}\n${result.bucket}\n${result.key ?? ''}`} result={result} />
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
              {last.complete ? null : (
                <p className="search-more">
                  <button
                    type="button"
                    className="btn btn-outline-secondary"
                    disabled={more.busy}
                    onClick={searchFurther}
                    title="Not everything was looked at: go on where this search stopped"
                  >
                    <i className="bi bi-arrow-down-circle" aria-hidden="true" /> <span className="btn-text">Search further</span>
                  </button>
                </p>
              )}
            </>
          )}
        </section>
      )}
    </>
  );
}

/** One bucket or object that was found, with the ways to go to it. */
function ResultRow({ result }: { result: SearchResult }) {
  const { bucket, key } = result;
  if (result.kind === 'bucket' || key === undefined) {
    return (
      <tr>
        <td data-label="Kind">
          <i className="bi bi-bucket search-kind" aria-hidden="true" /> <span className="visually-hidden">Bucket</span>
        </td>
        <td data-label="Bucket">{bucket}</td>
        <td data-label="Key">-</td>
        <td data-label="Size">-</td>
        <td data-label="Last modified">-</td>
        <td data-label="Actions">
          <span className="d-flex flex-wrap gap-2">
            <OpenBucket bucket={bucket} />
          </span>
        </td>
      </tr>
    );
  }
  // The folder of a key is what stands before its last delimiter.
  const folder = key.slice(0, key.lastIndexOf(DELIMITER) + 1);
  return (
    <tr>
      <td data-label="Kind">
        <i className="bi bi-file-earmark search-kind" aria-hidden="true" /> <span className="visually-hidden">Object</span>
      </td>
      <td data-label="Bucket">{bucket}</td>
      <td data-label="Key">{key}</td>
      <td data-label="Size">{result.size === undefined ? '-' : formatBytes(result.size)}</td>
      <td data-label="Last modified">{formatTime(result.last_modified)}</td>
      <td data-label="Actions">
        <span className="d-flex flex-wrap gap-2">
          <OpenBucket bucket={bucket} />
          {folder === '' ? null : (
            <a
              className="btn btn-outline-secondary"
              href={routeHref(['buckets', bucket], { prefix: folder })}
              aria-label={`Open the folder of ${key}`}
              title={`Open the folder ${folder}`}
            >
              <i className="bi bi-folder2-open" aria-hidden="true" /> <span className="btn-text">Open folder</span>
            </a>
          )}
          <a
            className="btn btn-outline-secondary"
            href={objectDownloadUrl(bucket, key)}
            download
            aria-label={`Download ${key}`}
            title="Download this object"
          >
            <i className="bi bi-download" aria-hidden="true" /> <span className="btn-text">Download</span>
          </a>
        </span>
      </td>
    </tr>
  );
}

function OpenBucket({ bucket }: { bucket: string }) {
  return (
    <a
      className="btn btn-outline-secondary"
      href={routeHref(['buckets', bucket])}
      aria-label={`Open bucket ${bucket}`}
      title="Open this bucket"
    >
      <i className="bi bi-bucket" aria-hidden="true" /> <span className="btn-text">Open bucket</span>
    </a>
  );
}
