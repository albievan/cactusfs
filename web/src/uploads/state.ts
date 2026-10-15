// What is known about the uploads of the visit. This file holds no React and
// sends nothing: a pure reducer and what is read from its state.

/**
 * How one file stands. `waiting` and `sending` are on their way; the other
 * three are over. `cancelled` is what somebody stopped: it is no failure and
 * is not sent again.
 */
export type UploadState = 'waiting' | 'sending' | 'stored' | 'failed' | 'cancelled';

/** One file of an upload and how far it is. */
export interface Upload {
  /** Which upload this is, among all of the visit. */
  id: string;
  /** The files that were sent off together share a batch. */
  batch: string;
  bucket: string;
  key: string;
  state: UploadState;
  /** Bytes sent, of how many. */
  sent: number;
  total: number;
  /** Why it was not stored, in the words of the API. */
  reason?: string;
  /**
   * Set on a file that failed without anybody knowing what became of it:
   * the API was still working on it when the console stopped asking. It may
   * yet be stored, so nothing may say that it is not.
   */
  undecided?: boolean;
}

/** The uploads of the visit that were not dismissed, in the order they were asked for. */
export interface UploadsState {
  uploads: Upload[];
}

/** A file that is sent off. */
export interface NewUpload {
  id: string;
  batch: string;
  bucket: string;
  key: string;
  total: number;
}

/** How far a file is: bytes sent, of how many. */
export interface Progress {
  sent: number;
  total: number;
}

export type UploadsAction =
  | { type: 'added'; uploads: NewUpload[] }
  | { type: 'sending'; id: string }
  | { type: 'progress'; progress: ReadonlyMap<string, Progress> }
  | { type: 'stored'; id: string }
  | { type: 'failed'; id: string; reason: string; undecided?: boolean }
  | { type: 'cancelled'; id: string }
  | { type: 'requeued'; ids: readonly string[] }
  | { type: 'dismissed'; ids: readonly string[] };

/** No uploads yet. */
export const NO_UPLOADS: UploadsState = { uploads: [] };

/** Whether a file is still on its way. */
export function inFlight(upload: Upload): boolean {
  return upload.state === 'waiting' || upload.state === 'sending';
}

// Changes the uploads that `change` has something to say about; the state is
// the one it was when it has nothing to say about any.
function change(state: UploadsState, each: (upload: Upload) => Upload): UploadsState {
  let changed = false;
  const uploads = state.uploads.map((upload) => {
    const next = each(upload);
    changed ||= next !== upload;
    return next;
  });
  return changed ? { uploads } : state;
}

/**
 * What becomes of the uploads when something happens to one. A file only
 * goes the ways there are - waiting, sending, then stored, failed or
 * cancelled; failed, and waiting again - so that word of a file that comes
 * late, after it was cancelled say, changes nothing.
 */
export function uploadsReducer(state: UploadsState, action: UploadsAction): UploadsState {
  switch (action.type) {
    case 'added':
      return action.uploads.length === 0
        ? state
        : {
            uploads: [
              ...state.uploads,
              ...action.uploads.map((upload): Upload => ({ ...upload, state: 'waiting', sent: 0 })),
            ],
          };
    case 'sending':
      return change(state, (upload) =>
        upload.id === action.id && upload.state === 'waiting' ? { ...upload, state: 'sending', sent: 0 } : upload,
      );
    case 'progress':
      return change(state, (upload) => {
        const progress = action.progress.get(upload.id);
        if (progress === undefined || upload.state !== 'sending') {
          return upload;
        }
        return progress.sent === upload.sent && progress.total === upload.total ? upload : { ...upload, ...progress };
      });
    case 'stored':
      return change(state, (upload) =>
        upload.id === action.id && upload.state === 'sending'
          ? { ...upload, state: 'stored', sent: upload.total, reason: undefined, undecided: undefined }
          : upload,
      );
    case 'failed':
      return change(state, (upload) =>
        upload.id === action.id && upload.state === 'sending'
          ? { ...upload, state: 'failed', reason: action.reason, undecided: action.undecided === true ? true : undefined }
          : upload,
      );
    case 'cancelled':
      return change(state, (upload) =>
        upload.id === action.id && inFlight(upload) ? { ...upload, state: 'cancelled', reason: undefined } : upload,
      );
    case 'requeued': {
      const ids = new Set(action.ids);
      return change(state, (upload) =>
        ids.has(upload.id) && upload.state === 'failed'
          ? { ...upload, state: 'waiting', sent: 0, reason: undefined, undecided: undefined }
          : upload,
      );
    }
    case 'dismissed': {
      const ids = new Set(action.ids);
      const uploads = state.uploads.filter((upload) => !ids.has(upload.id) || inFlight(upload));
      return uploads.length === state.uploads.length ? state : { uploads };
    }
  }
}

/** How a number of uploads stand, counted. */
export interface UploadTotals {
  /** How many files there are. */
  files: number;
  waiting: number;
  sending: number;
  stored: number;
  failed: number;
  /** Of the failed: those of which nobody knows whether they are stored. */
  undecided: number;
  cancelled: number;
  /** Waiting and sending together: what is still on its way. */
  inFlight: number;
  /**
   * The bytes of the files that are stored or on their way, sent and in all.
   * What failed or was cancelled is in neither: it is not going to arrive.
   */
  bytesSent: number;
  bytesTotal: number;
}

/** Counts how the uploads stand. */
export function totalsOf(uploads: readonly Upload[]): UploadTotals {
  const totals: UploadTotals = {
    files: uploads.length,
    waiting: 0,
    sending: 0,
    stored: 0,
    failed: 0,
    undecided: 0,
    cancelled: 0,
    inFlight: 0,
    bytesSent: 0,
    bytesTotal: 0,
  };
  for (const upload of uploads) {
    totals[upload.state] += 1;
    if (upload.state === 'failed' && upload.undecided === true) {
      totals.undecided += 1;
    }
    if (upload.state !== 'failed' && upload.state !== 'cancelled') {
      totals.bytesSent += upload.state === 'stored' ? upload.total : Math.min(upload.sent, upload.total);
      totals.bytesTotal += upload.total;
    }
  }
  totals.inFlight = totals.waiting + totals.sending;
  return totals;
}

/** The files that were sent off together. */
export interface Batch {
  id: string;
  bucket: string;
  uploads: Upload[];
}

/** The uploads by batch, the batches in the order they were begun. */
export function batchesOf(uploads: readonly Upload[]): Batch[] {
  const batches = new Map<string, Batch>();
  for (const upload of uploads) {
    const batch = batches.get(upload.batch);
    if (batch === undefined) {
      batches.set(upload.batch, { id: upload.batch, bucket: upload.bucket, uploads: [upload] });
    } else {
      batch.uploads.push(upload);
    }
  }
  return [...batches.values()];
}

/** The batches that have a file on its way. */
export function batchesInFlight(uploads: readonly Upload[]): Set<string> {
  const batches = new Set<string>();
  for (const upload of uploads) {
    if (inFlight(upload)) {
      batches.add(upload.batch);
    }
  }
  return batches;
}

/** The uploads of these batches, in the order they were asked for. */
export function ofBatches(uploads: readonly Upload[], batches: ReadonlySet<string>): Upload[] {
  return uploads.filter((upload) => batches.has(upload.batch));
}

/**
 * What is going on now: every file of the batches that have a file on its
 * way - those of them that are over too, so that "3 of 12 stored" can be
 * said - and none of a batch that is over, however long ago it was not
 * dismissed.
 */
export function underWay(uploads: readonly Upload[]): Upload[] {
  return ofBatches(uploads, batchesInFlight(uploads));
}

/** The batches that are, or were until just now, on their way together. */
export interface Run {
  /** Whether a file of them is on its way. */
  active: boolean;
  batches: ReadonlySet<string>;
}

/** No run yet. */
export const NO_RUN: Run = { active: false, batches: new Set() };

/**
 * What the run is, now that these batches have a file on its way. A run
 * begins when files are sent off while nothing is on its way, takes in what
 * is sent off until nothing is, and is then kept - over - until the next.
 */
export function runAfter(run: Run, flying: ReadonlySet<string>): Run {
  if (flying.size === 0) {
    return run.active ? { active: false, batches: run.batches } : run;
  }
  if (!run.active) {
    return { active: true, batches: flying };
  }
  return [...flying].every((batch) => run.batches.has(batch))
    ? run
    : { active: true, batches: new Set([...run.batches, ...flying]) };
}

/** Which uploads are meant: those of a batch, those of a bucket, or - neither named - all. */
export interface UploadScope {
  batch?: string;
  bucket?: string;
}

/** Whether an upload is among those a scope means. */
export function inScope(upload: Upload, scope: UploadScope): boolean {
  return (
    (scope.batch === undefined || upload.batch === scope.batch) &&
    (scope.bucket === undefined || upload.bucket === scope.bucket)
  );
}
