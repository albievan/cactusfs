import { ApiError, uploadObjectWithProgress } from '../api/client';
import type { NoticeInput } from '../notices/state';
import { routeHref } from '../router';
import { limiter } from '../ui/limiter';
import { reasonOf } from '../ui/reason';
import {
  inFlight,
  inScope,
  NO_UPLOADS,
  totalsOf,
  uploadsReducer,
  type Progress,
  type UploadsAction,
  type UploadScope,
  type UploadsState,
} from './state';

/** How many files are on their way at once, whatever buckets they go to. */
export const UPLOADS_AT_ONCE = 4;

/** How often an upload is tried when it fails for a reason that may pass. */
export const UPLOAD_ATTEMPTS = 3;
export const UPLOAD_RETRY_MS = 400;

/**
 * How an upload waits when the API says that it is still working on the
 * request it was first sent with (409 `RequestInProgress`): the answer was
 * lost, the request was not. It is asked again with the same key after 2 s,
 * 4 s, 8 s and then every 15 s - ten times, two minutes in all, which
 * outlasts a slow commit - and never as a new upload, which would store the
 * file twice.
 */
export const UPLOAD_BUSY_WAITS = 10;
export const UPLOAD_BUSY_MS = 2000;
export const UPLOAD_BUSY_MAX_MS = 15000;

/** How long the wait is before the API is asked the `nth` time (from 1) whether it is still working. */
export function busyWait(nth: number): number {
  return Math.min(UPLOAD_BUSY_MS * 2 ** (nth - 1), UPLOAD_BUSY_MAX_MS);
}

/**
 * What is said of a file the API was still working on when the console
 * stopped asking. Nobody knows that it is not stored, so this does not say so.
 */
export const STILL_WORKING =
  'The server is still working on this file: it may yet be stored. Look in the bucket, or send it again in a while.';

/** How often, at most, the progress of the files is told to the screen. */
export const PROGRESS_EVERY_MS = 100;

/** A file to upload, and the key to store it under. */
export interface UploadItem {
  file: File;
  key: string;
}

/** What is told to whoever listens to the uploads. */
export type UploadEvent =
  | {
      /** A file was stored. */
      type: 'stored';
      batch: string;
      bucket: string;
      key: string;
    }
  | {
      /** No file of a batch is on its way any more. It follows the word of the last file that was stored. */
      type: 'settled';
      batch: string;
      bucket: string;
      files: number;
      stored: number;
      failed: number;
      cancelled: number;
    };

/** What the console can do with uploads. None of it changes between renders. */
export interface UploadsActions {
  /** Sends files off to a bucket and returns the batch they make. */
  start: (bucket: string, items: UploadItem[]) => string;
  /** Sends again what was not stored, and only that: of a batch, of a bucket, or of all. */
  retryFailed: (scope?: UploadScope) => void;
  /** Stops a file that is waiting or being sent. It is then cancelled, not failed. */
  cancel: (id: string) => void;
  /** Stops every file that is on its way. */
  cancelAll: () => void;
  /** Whether any file is on its way, asked at the moment something is to be done about it. */
  busy: () => boolean;
  /** Forgets the uploads that are over: of a batch, of a bucket, or all. */
  dismissFinished: (scope?: UploadScope) => void;
  /** Tells the listener when a file is stored and when a batch is over; returns what removes it. */
  listen: (listener: (event: UploadEvent) => void) => () => void;
}

/** The uploads of the console: what can be done with them, and how they stand. */
export interface UploadsEngine extends UploadsActions {
  /** How the uploads stand. */
  getState: () => UploadsState;
  /** Calls `onChange` whenever getState has another answer; returns what removes it. */
  subscribe: (onChange: () => void) => () => void;
  /** Stops everything that is on its way, without a word: the console is going away. */
  dispose: () => void;
}

// What sending a file needs and its state does not show.
interface Job {
  file: File;
  bucket: string;
  key: string;
  batch: string;
  /** The same for every attempt at the same upload; see send. */
  idempotencyKey: string;
  /** The API answered the last attempt with a refusal, which it keeps under the key. */
  refused: boolean;
  /** A request was made with the key before: the next one with it is a repeat. */
  tried: boolean;
  stop: AbortController;
}

/**
 * Whether the API said that the first request with this Idempotency-Key has
 * not finished. It is no answer to the upload: nothing is kept under the key
 * for it, and the request it speaks of may yet store the file.
 */
function inProgress(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 409 && error.problem.code === 'RequestInProgress';
}

/** Whether a failed attempt is worth making again soon: the connection, or a server that says "not now". */
function mayPass(error: unknown): boolean {
  if (!(error instanceof ApiError)) {
    return false;
  }
  const { status } = error.problem;
  return status === 0 || status === 502 || status === 503 || status === 504;
}

/** Whether the API answered with a refusal: an answer it keeps, and gives again to the same Idempotency-Key. */
function refusal(error: unknown): boolean {
  return (
    error instanceof ApiError && error.problem.status >= 400 && error.problem.status < 500 && !inProgress(error)
  );
}

/**
 * Whether a refusal is one the API kept and gave again, rather than one it
 * made of this request: it says so in a header.
 */
function keptRefusal(error: unknown): boolean {
  return refusal(error) && error instanceof ApiError && error.replayed;
}

// Waits, and no longer than until the signal is aborted.
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener('abort', done, { once: true });
  });
}

function objects(count: number): string {
  return count === 1 ? '1 object' : `${String(count)} objects`;
}

/**
 * Makes the uploads of one console: the state, and the sending. It belongs
 * to the console and to no screen, so that an upload goes on whatever screen
 * is shown.
 *
 * Files are sent a few at a time - UPLOADS_AT_ONCE, counted over all buckets
 * - each with a request of its own, as the API has it. Every file is tried:
 * one that fails does not hold the others up. An attempt that fails for a
 * reason that may pass - the connection, a node that is restarting, no
 * leader for a moment - is made again, with the same Idempotency-Key, so
 * that nothing is stored twice. When the API then says that it is still
 * working on the first request with that key, the file is asked for again,
 * with the same key, at longer intervals: see UPLOAD_BUSY_WAITS.
 *
 * `notify` is told once what became of a batch, not once a file.
 */
export function createUploads(notify: (notice: NoticeInput) => void): UploadsEngine {
  let state = NO_UPLOADS;
  const jobs = new Map<string, Job>();
  const subscribers = new Set<() => void>();
  const listeners = new Set<(event: UploadEvent) => void>();
  const turn = limiter(UPLOADS_AT_ONCE);
  // The progress that was reported and not yet shown, and the timer that
  // will show it: a file reports many times a second, and every one of them
  // told at once would have the screen drawn again as often.
  const reported = new Map<string, Progress>();
  let showing: ReturnType<typeof setTimeout> | undefined;

  const apply = (action: UploadsAction) => {
    const next = uploadsReducer(state, action);
    if (next !== state) {
      state = next;
      for (const onChange of subscribers) {
        onChange();
      }
    }
  };

  const tell = (event: UploadEvent) => {
    for (const listener of listeners) {
      listener(event);
    }
  };

  const showProgress = () => {
    showing = undefined;
    if (reported.size > 0) {
      const progress = new Map(reported);
      reported.clear();
      apply({ type: 'progress', progress });
    }
  };

  const report = (id: string, sent: number, total: number) => {
    reported.set(id, { sent, total });
    showing ??= setTimeout(showProgress, PROGRESS_EVERY_MS);
  };

  // Called whenever a file is over: says so once no file of its batch is on
  // its way any more.
  const settle = (batch: string) => {
    const uploads = state.uploads.filter((upload) => upload.batch === batch);
    const [first] = uploads;
    const totals = totalsOf(uploads);
    if (first === undefined || totals.inFlight > 0) {
      return;
    }
    const { bucket } = first;
    const link = { href: routeHref(['buckets', bucket]), label: `Open ${bucket}` };
    // What failed without anybody knowing what became of it is not called
    // "not stored": the API may yet store it.
    const refused = totals.failed - totals.undecided;
    if (refused > 0) {
      const title = `${String(refused)} of ${objects(totals.files)} ${totals.files === 1 ? 'was' : 'were'} not stored in ${bucket}`;
      notify({
        kind: 'error',
        title:
          totals.undecided === 0
            ? title
            : `${title}, and ${String(totals.undecided)} ${totals.undecided === 1 ? 'is' : 'are'} not confirmed as stored`,
        // Why, as the API said it of the first of them.
        detail: uploads.find((upload) => upload.state === 'failed' && upload.undecided !== true)?.reason,
        link,
      });
    } else if (totals.undecided > 0) {
      notify({
        kind: 'info',
        title: `${String(totals.undecided)} of ${objects(totals.files)} ${totals.undecided === 1 ? 'is' : 'are'} not confirmed as stored in ${bucket}`,
        detail: STILL_WORKING,
        link,
      });
    } else if (totals.cancelled > 0) {
      notify({
        kind: 'info',
        title:
          totals.stored === 0
            ? `The upload of ${objects(totals.files)} to ${bucket} was cancelled`
            : `Stored ${String(totals.stored)} of ${objects(totals.files)} in ${bucket}: ${String(totals.cancelled)} cancelled`,
        link,
      });
    } else {
      notify({ kind: 'success', title: `Stored ${objects(totals.files)} in ${bucket}`, link });
    }
    tell({
      type: 'settled',
      batch,
      bucket,
      files: totals.files,
      stored: totals.stored,
      failed: totals.failed,
      cancelled: totals.cancelled,
    });
  };

  // Sends one file, when its turn has come.
  const send = async (id: string, job: Job) => {
    apply({ type: 'sending', id });
    // How often it failed for a reason that may pass, how often the API said
    // that it was still working, and whether a new key was drawn for a
    // refusal that was only a kept one.
    let failures = 0;
    let busy = 0;
    let renewed = false;
    for (;;) {
      const repeat = job.tried;
      job.tried = true;
      try {
        await uploadObjectWithProgress(
          job.bucket,
          job.key,
          job.file,
          (sent, total) => {
            report(id, sent, total);
          },
          job.idempotencyKey,
          job.stop.signal,
        );
      } catch (error) {
        if (job.stop.signal.aborted) {
          // Cancelled: whoever cancelled it has said so already.
          return;
        }
        const working = inProgress(error);
        if (working && busy < UPLOAD_BUSY_WAITS) {
          // The first request with this key is still running: it is asked
          // after again, as the same upload, and given longer each time.
          busy += 1;
          await pause(busyWait(busy), job.stop.signal);
          if (job.stop.signal.aborted) {
            return;
          }
          continue;
        }
        if (!working && failures < UPLOAD_ATTEMPTS - 1 && mayPass(error)) {
          failures += 1;
          await pause(UPLOAD_RETRY_MS * failures, job.stop.signal);
          if (job.stop.signal.aborted) {
            return;
          }
          continue;
        }
        if (repeat && !renewed && keptRefusal(error)) {
          // A repeat was answered with a refusal the API had kept: the one
          // it gave to the attempt before, whose body was cut short, say.
          // It is no word on the file as it is sent now, and the key would
          // have it given again for a day. So the file is sent once more as
          // a new upload - once: what that one is answered counts.
          renewed = true;
          job.idempotencyKey = crypto.randomUUID();
          job.tried = false;
          continue;
        }
        // The file is kept: what failed can be sent again. A file the API
        // is still working on is sent again as the same upload.
        job.refused = refusal(error);
        apply(
          working
            ? { type: 'failed', id, reason: STILL_WORKING, undecided: true }
            : { type: 'failed', id, reason: reasonOf(error) },
        );
        settle(job.batch);
        return;
      }
      if (job.stop.signal.aborted) {
        return;
      }
      // Stored: the file itself is let go of, so that it does not stay in
      // memory for as long as its tile is shown.
      jobs.delete(id);
      reported.delete(id);
      apply({ type: 'stored', id });
      tell({ type: 'stored', batch: job.batch, bucket: job.bucket, key: job.key });
      settle(job.batch);
      return;
    }
  };

  const queue = (id: string, job: Job) => {
    // A file that is cancelled while it waits is not begun: the queue then
    // rejects its turn, which nobody needs to hear of.
    turn(() => send(id, job), job.stop.signal).catch(() => undefined);
  };

  const start = (bucket: string, items: UploadItem[]): string => {
    const batch = crypto.randomUUID();
    const added = items.map(({ file, key }) => {
      const id = crypto.randomUUID();
      const job: Job = {
        file,
        bucket,
        key,
        batch,
        idempotencyKey: crypto.randomUUID(),
        refused: false,
        tried: false,
        stop: new AbortController(),
      };
      jobs.set(id, job);
      return { id, job };
    });
    apply({
      type: 'added',
      uploads: added.map(({ id, job }) => ({ id, batch, bucket, key: job.key, total: job.file.size })),
    });
    for (const { id, job } of added) {
      queue(id, job);
    }
    return batch;
  };

  const retryFailed = (scope: UploadScope = {}) => {
    const again: { id: string; job: Job }[] = [];
    for (const upload of state.uploads) {
      const job = jobs.get(upload.id);
      if (upload.state === 'failed' && inScope(upload, scope) && job !== undefined) {
        // The key says "this is the same upload": an attempt that was
        // stored, though its answer was lost, is then not stored twice. A
        // refusal is another matter. The API keeps an answer it gave - a
        // refusal too - and gives it again to the key it was given for: a
        // file that was refused would be refused for a day, whatever was
        // put right in between. So what was refused is sent as a new
        // upload, and what was never answered, or by a server that failed
        // or was still working on it, as the same one.
        if (job.refused) {
          job.idempotencyKey = crypto.randomUUID();
          job.refused = false;
          job.tried = false;
        }
        again.push({ id: upload.id, job });
      }
    }
    apply({ type: 'requeued', ids: again.map(({ id }) => id) });
    for (const { id, job } of again) {
      queue(id, job);
    }
  };

  const cancel = (id: string) => {
    const upload = state.uploads.find((entry) => entry.id === id);
    const job = jobs.get(id);
    if (upload === undefined || job === undefined || !inFlight(upload)) {
      return;
    }
    // Aborting gives the request up, or the turn the file waits for. What
    // was cancelled is not sent again, so its file is let go of.
    job.stop.abort();
    jobs.delete(id);
    reported.delete(id);
    apply({ type: 'cancelled', id });
    settle(upload.batch);
  };

  const cancelAll = () => {
    for (const upload of state.uploads.filter(inFlight)) {
      cancel(upload.id);
    }
  };

  const dismissFinished = (scope: UploadScope = {}) => {
    const ids = state.uploads.filter((upload) => !inFlight(upload) && inScope(upload, scope)).map((upload) => upload.id);
    for (const id of ids) {
      jobs.delete(id);
    }
    apply({ type: 'dismissed', ids });
  };

  return {
    start,
    retryFailed,
    cancel,
    cancelAll,
    dismissFinished,
    busy: () => state.uploads.some(inFlight),
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
      reported.clear();
      clearTimeout(showing);
      showing = undefined;
    },
  };
}
