import { api, ApiError, SESSION_LOCKED, unwrap } from '../api/client';
import type { NoticeInput } from '../notices/state';
import { routeHref } from '../router';
import { limiter } from '../ui/limiter';
import { reasonOf } from '../ui/reason';
import { deletionsReducer, NO_DELETIONS, outcomeOf, type DeletionsAction, type DeletionsState, type Expected } from './state';

/**
 * How many objects are being deleted at once, whatever deletions and buckets
 * they are of: one queue for "Delete selected" and "Delete all" together, and
 * none shared with the uploads.
 */
export const DELETIONS_AT_ONCE = 8;

/** How many keys one page of the listing that "Delete all" walks holds: the most the API gives. */
export const LISTING_PAGE = 1000;

/** What a deletions engine can be made with. The console passes none of it. */
export interface DeletionsOptions {
  /**
   * How many keys a page of the walk asks for: LISTING_PAGE unless given.
   * Only the tests give it, to walk several pages of a few objects.
   */
  listingPage?: number;
}

/** What is told to whoever listens to the deletions. */
export type DeletionEvent =
  | {
      /** An object was deleted. */
      type: 'deleted';
      id: string;
      bucket: string;
      key: string;
    }
  | {
      /** A deletion is over: everything it sent has been answered. */
      type: 'settled';
      id: string;
      bucket: string;
      deleted: number;
      refused: number;
    };

/** What the console can do with deletions. None of it changes between renders. */
export interface DeletionsActions {
  /** Deletes these keys of a bucket, as many at once as there are turns; returns the deletion. */
  deleteKeys: (bucket: string, keys: readonly string[]) => string;
  /**
   * Deletes every object of a bucket: the listing is walked page by page
   * and each key deleted as it comes. `expected` is what the bucket's usage
   * said, for the progress. The password, where one is asked for, is
   * checked before this is called: this sends deletions and nothing else.
   */
  deleteAll: (bucket: string, expected?: Expected) => string;
  /** Stops a deletion: what has not begun is not sent; what is on its way is answered and counted. */
  cancel: (id: string) => void;
  /** Stops every deletion that is on its way. */
  cancelAll: () => void;
  /** Whether a deletion is on its way, asked at the moment something is to be done about it. */
  busy: () => boolean;
  /** Forgets the deletions that are over, of a bucket or all. */
  dismissFinished: (bucket?: string) => void;
  /** Tells the listener when an object is deleted and when a deletion is over; returns what removes it. */
  listen: (listener: (event: DeletionEvent) => void) => () => void;
}

/** The deletions of the console: what can be done with them, and how they stand. */
export interface DeletionsEngine extends DeletionsActions {
  /** How the deletions stand. */
  getState: () => DeletionsState;
  /** Calls `onChange` whenever getState has another answer; returns what removes it. */
  subscribe: (onChange: () => void) => () => void;
  /** Stops everything that has not begun, without a word: the console is going away. */
  dispose: () => void;
}

// What running a deletion needs and its state does not show.
interface Job {
  id: string;
  bucket: string;
  /** Aborted when the deletion is cancelled or stopped: what waits for its turn is not begun. */
  stop: AbortController;
}

/**
 * Makes a request, and once more should its answer be a 401 to tokens an
 * unlock replaced while it was on its way (`ApiError.staleTokens`): such a
 * request never reached the authorizer, and sent again it carries the new
 * tokens. Another unlock - "Delete all" of another bucket - may be what
 * replaced them. Once only: a second such answer is an answer.
 */
async function withFreshTokens<T>(request: () => Promise<T>): Promise<T> {
  try {
    return await request();
  } catch (error) {
    if (error instanceof ApiError && error.staleTokens) {
      return request();
    }
    throw error;
  }
}

/**
 * Why an object was not deleted whose request was answered 401 twice, both
 * times to tokens a password had replaced while it was on its way: it never
 * reached the authorizer. The session did not end.
 */
export const REPLACED_TOKENS =
  'Not sent with the current tokens: the password was checked again twice while it was on its way. Delete it again.';

/**
 * Why the deletion as a whole cannot go on, or null when the error is of the
 * one object: a session that has ended (401) or is locked (423) refuses every
 * request that follows, and sending them all to be refused says nothing more.
 */
function stopsAll(error: unknown): string | null {
  // A 401 to tokens an unlock replaced is of that request, not of the
  // session: one that came again after it was sent once more is that
  // object's failure (see REPLACED_TOKENS), and the rest go on.
  if (!(error instanceof ApiError) || error.staleTokens) {
    return null;
  }
  if (error.problem.status === 401) {
    return 'The session ended.';
  }
  if (error.problem.status === 423 && error.problem.code === SESSION_LOCKED) {
    return 'The session was locked.';
  }
  return null;
}

/**
 * Makes the deletions of one console: the state, and the sending. It belongs
 * to the console and to no screen, so that a deletion goes on whatever
 * screen is shown.
 *
 * Every object is one `DELETE` through the generated client, as the API has
 * it: the authorizer judges each for its own object, and nothing here
 * decides what may be deleted. At most DELETIONS_AT_ONCE are on their way,
 * counted over every deletion of every bucket. A refusal - Object Lock, no
 * right to it - is counted with its reason and the others go on; a session
 * that ended or was locked stops the rest. A 401 to tokens an unlock replaced
 * meanwhile is neither: that one request is sent again with the new tokens. A failure is not sent again: in a
 * bucket that keeps versions a second `DELETE` would be a second delete
 * marker, and running the deletion again does what a retry would.
 *
 * `notify` is told once what became of a deletion, not once an object.
 */
export function createDeletions(
  notify: (notice: NoticeInput) => void,
  { listingPage = LISTING_PAGE }: DeletionsOptions = {},
): DeletionsEngine {
  let state = NO_DELETIONS;
  const jobs = new Map<string, Job>();
  const subscribers = new Set<() => void>();
  const listeners = new Set<(event: DeletionEvent) => void>();
  const turn = limiter(DELETIONS_AT_ONCE);

  const apply = (action: DeletionsAction) => {
    const next = deletionsReducer(state, action);
    if (next !== state) {
      state = next;
      for (const onChange of subscribers) {
        onChange();
      }
    }
  };

  const tell = (event: DeletionEvent) => {
    for (const listener of listeners) {
      listener(event);
    }
  };

  const halt = (job: Job, reason: string) => {
    apply({ type: 'stopped', id: job.id, reason });
    job.stop.abort();
  };

  // Deletes one object, when its turn has come. It never rejects.
  const deleteOne = async (job: Job, key: string) => {
    try {
      await withFreshTokens(() =>
        unwrap(api.DELETE('/buckets/{bucket}/object', { params: { path: { bucket: job.bucket }, query: { key } } })),
      );
    } catch (error) {
      const why = stopsAll(error);
      if (why === null) {
        apply({
          type: 'refused',
          id: job.id,
          reason:
            error instanceof ApiError && error.staleTokens ? REPLACED_TOKENS : reasonOf(error, 'The request failed'),
        });
      } else {
        // The object is one of those the session stopped: not deleted, and
        // not counted as refused for a reason of its own.
        apply({ type: 'skipped', id: job.id, keys: 1 });
        halt(job, why);
      }
      return;
    }
    apply({ type: 'deleted', id: job.id });
    tell({ type: 'deleted', id: job.id, bucket: job.bucket, key });
  };

  // Hands the keys to the queue; resolves when every one of them has been
  // answered or was not begun.
  const queueKeys = (job: Job, keys: readonly string[]): Promise<void> =>
    Promise.all(
      keys.map((key) =>
        turn(() => deleteOne(job, key), job.stop.signal).catch(() => {
          // Its turn came after the deletion was cancelled or stopped.
          apply({ type: 'skipped', id: job.id, keys: 1 });
        }),
      ),
    ).then(() => undefined);

  const settle = (job: Job) => {
    apply({ type: 'over', id: job.id });
    jobs.delete(job.id);
    const deletion = state.deletions.find((d) => d.id === job.id);
    if (deletion === undefined) {
      return;
    }
    const outcome = outcomeOf(deletion);
    notify({
      ...outcome,
      link: { href: routeHref(['buckets', job.bucket]), label: `Open ${job.bucket}` },
    });
    tell({ type: 'settled', id: job.id, bucket: job.bucket, deleted: deletion.deleted, refused: deletion.refused });
  };

  // Walks the listing of the whole bucket - no prefix, no folders - a page
  // at a time, and deletes the keys of a page while the next is read: at
  // most two pages are held. A page goes on from the last key of the page
  // before, so a key that was deleted meanwhile moves nothing, and one that
  // was refused is not found again. A key stored meanwhile behind where the
  // walk is may be left: the outcome says so.
  const walk = async (job: Job) => {
    let token: string | undefined;
    let previous: Promise<void> = Promise.resolve();
    for (;;) {
      if (job.stop.signal.aborted) {
        break;
      }
      let page;
      try {
        page = await withFreshTokens(() =>
          unwrap(
            api.GET('/buckets/{bucket}/objects', {
              params: { path: { bucket: job.bucket }, query: { continuation_token: token, limit: listingPage } },
            }),
          ),
        );
      } catch (error) {
        if (!job.stop.signal.aborted) {
          // The session is gone or locked: nothing more is sent. Otherwise
          // the keys listed already are still deleted, and the walk stops.
          const why = stopsAll(error);
          if (why === null) {
            const reason =
              error instanceof ApiError && error.staleTokens
                ? 'the password was checked again twice while the listing was on its way'
                : reasonOf(error, 'the request failed');
            apply({ type: 'stopped', id: job.id, reason: `The objects could not be listed: ${reason}.` });
          } else {
            halt(job, why);
          }
        }
        break;
      }
      if (job.stop.signal.aborted) {
        break;
      }
      const next = page.truncated ? page.next_continuation_token : undefined;
      // A page that says there is more and does not say from where - or
      // says from where it began - cannot be gone on from: the walk would
      // end early, or never. What it holds is deleted; the walk stops, and
      // says so: it is never "every object".
      const stuck = page.truncated && (next === undefined || next === token);
      const keys = page.objects.map((object) => object.key);
      apply({ type: 'listed', id: job.id, keys: keys.length, last: !page.truncated });
      const these = queueKeys(job, keys);
      await previous;
      previous = these;
      if (stuck) {
        apply({ type: 'stopped', id: job.id, reason: 'The listing could not go on.' });
        break;
      }
      if (next === undefined) {
        break;
      }
      token = next;
    }
    await previous;
  };

  const begin = (bucket: string): Job => {
    const job: Job = { id: crypto.randomUUID(), bucket, stop: new AbortController() };
    jobs.set(job.id, job);
    return job;
  };

  const deleteKeys = (bucket: string, keys: readonly string[]): string => {
    const job = begin(bucket);
    apply({ type: 'begun', deletion: { id: job.id, bucket, kind: 'selected', keys: keys.length } });
    void queueKeys(job, keys).then(() => {
      settle(job);
    });
    return job.id;
  };

  const deleteAll = (bucket: string, expected?: Expected): string => {
    const job = begin(bucket);
    apply({ type: 'begun', deletion: { id: job.id, bucket, kind: 'all', expected } });
    void walk(job).then(() => {
      settle(job);
    });
    return job.id;
  };

  const cancel = (id: string) => {
    const job = jobs.get(id);
    if (job === undefined || job.stop.signal.aborted) {
      return;
    }
    apply({ type: 'cancelled', id });
    job.stop.abort();
  };

  return {
    deleteKeys,
    deleteAll,
    cancel,
    cancelAll: () => {
      for (const id of [...jobs.keys()]) {
        cancel(id);
      }
    },
    busy: () => state.deletions.some((d) => d.running),
    dismissFinished: (bucket) => {
      apply({
        type: 'dismissed',
        ids: state.deletions.filter((d) => !d.running && (bucket === undefined || d.bucket === bucket)).map((d) => d.id),
      });
    },
    listen: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    getState: () => state,
    subscribe: (onChange) => {
      subscribers.add(onChange);
      return () => {
        subscribers.delete(onChange);
      };
    },
    dispose: () => {
      for (const job of jobs.values()) {
        job.stop.abort();
      }
      jobs.clear();
    },
  };
}
