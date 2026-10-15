import createClient, { type Middleware } from 'openapi-fetch';

import { readCsrfToken, storeCsrfToken } from './csrf';
import type { components, paths } from './schema';

/** Where the management API is served, on the same origin as the console. */
export const API_BASE = '/api/v1';

type Schemas = components['schemas'];

/** Problem details (RFC 9457), the body of every refusal. */
export type Problem = Schemas['Problem'];
/** The answer to signing in. */
export type Session = Schemas['Session'];
/** Who the API says is making the requests. */
export type Identity = Schemas['Identity'];
export type ServerVersion = Schemas['ServerVersion'];
/** A bucket. */
export type Bucket = Schemas['Bucket'];
/** What the list of buckets says of a bucket beyond its name, as far as the caller may see it. */
export type BucketDetails = Schemas['BucketDetails'];
/** One upload into or download from a bucket, as the list of buckets tells it. */
export type BucketActivityEntry = Schemas['BucketActivityEntry'];
/** How much a bucket holds. */
export type BucketUsage = Schemas['BucketUsage'];
/** One page of an object listing. */
export type ObjectList = Schemas['ObjectList'];
/** What is known about one object. */
export type ObjectMetadata = Schemas['ObjectMetadata'];
/** Who uploaded an object and who downloaded it: best-effort counts. */
export type ObjectAccess = Schemas['ObjectAccess'];
/** What a listing says of who uploaded an object and how often it was downloaded. */
export type ObjectAccessSummary = Schemas['ObjectAccessSummary'];
/** One page of what a search found. */
export type SearchResults = Schemas['SearchResults'];
/** A bucket or an object that a search found. */
export type SearchResult = Schemas['SearchResult'];
/** A user. */
export type User = Schemas['User'];
/** A group. */
export type Group = Schemas['Group'];
/** A role. */
export type Role = Schemas['Role'];
/** A policy. */
export type Policy = Schemas['Policy'];
/** A policy document in the IAM policy language. */
export type PolicyDocument = Schemas['PolicyDocument'];
/** An API key, without its secret. */
export type APIKey = Schemas['APIKey'];
/** A new API key together with its secret, which is returned once. */
export type APIKeyCreated = Schemas['APIKeyCreated'];
/** The nodes and drives of the deployment. */
export type ClusterStatus = Schemas['ClusterStatus'];
/** What the background work of a node has done. */
export type MaintenanceStatus = Schemas['MaintenanceStatus'];
/** How every node of the deployment is configured, as the answering node could gather it. */
export type Settings = Schemas['Settings'];
/** What one node says of itself, or why it said nothing. */
export type NodeSettings = Schemas['NodeSettings'];
/** One setting of one node, as it may be shown. */
export type Setting = Schemas['Setting'];
/** One drive of a node: where it is and what it belongs to. */
export type DriveSettings = Schemas['DriveSettings'];
/** A directory a node stores in. */
export type SettingsVolume = Schemas['SettingsVolume'];
/** The file system behind a directory, where the node can tell. */
export type SettingsFilesystem = Schemas['SettingsFilesystem'];
/** What may be said of a certificate a node serves. */
export type SettingsCertificate = Schemas['SettingsCertificate'];
/** A setting that should be the same on every node and is not. */
export type SettingsMismatch = Schemas['SettingsMismatch'];
/** One record of the audit trail. */
export type AuditRecord = Schemas['AuditRecord'];
/** One page of the audit trail. */
export type AuditPage = Schemas['AuditPage'];

/**
 * The header the API sets on an answer it did not make now but kept: the
 * answer it gave before to a request with the same Idempotency-Key.
 */
export const REPLAYED_HEADER = 'Idempotent-Replayed';

/** A request the API refused. It carries the problem the API described. */
export class ApiError extends Error {
  /** The problem as the API reported it. */
  readonly problem: Problem;
  /**
   * Whether the API said that this is an answer it kept, given again to the
   * Idempotency-Key it was first given for. Only the upload with progress
   * tells: no other call of the console is made twice with one key.
   */
  readonly replayed: boolean;

  constructor(problem: Problem, replayed = false) {
    super(problem.title);
    this.name = 'ApiError';
    this.problem = problem;
    this.replayed = replayed;
  }
}

/**
 * What is known of a 401 the console was answered: which request it
 * answered, and how many passwords an unlock of the session was refused
 * (403) since the session was last unlocked or ended. It says why the
 * session ended only as far as that can be told from here: the API does
 * not say.
 */
export interface Unauthorized {
  /** The operation the 401 answered, as the contract names its path: `/session/unlock`, `/buckets/{bucket}/object`. */
  path: string;
  /** Its method. */
  method: string;
  /** How many passwords unlocking was answered "wrong" before. */
  wrongPasswords: number;
}

const unauthorizedListeners = new Set<(cause: Unauthorized) => void>();
const lockedListeners = new Set<() => void>();

/**
 * Calls the listener whenever the API answers 401, which means there is no
 * session (any more) - but for a 401 to a request that was sent with the
 * tokens of before an unlock and answered after it: that one says only that
 * those tokens are refused now, and is the error of whoever made the
 * request. Returns the function that removes the listener.
 */
export function onUnauthorized(listener: (cause: Unauthorized) => void): () => void {
  unauthorizedListeners.add(listener);
  return () => {
    unauthorizedListeners.delete(listener);
  };
}

/**
 * The code of the problem the API answers a locked session with, status 423:
 * the session reached its timeout while its user was uploading, and may
 * store objects, ask who it is, unlock and sign out, and nothing else.
 */
export const SESSION_LOCKED = 'SessionLocked';

/**
 * Calls the listener whenever the API answers 423 with the code
 * `SessionLocked`: the session is there, and locked. It is not a 401 and is
 * told to nobody who listens for one. Returns the function that removes the
 * listener.
 */
export function onLocked(listener: () => void): () => void {
  lockedListeners.add(listener);
  return () => {
    lockedListeners.delete(listener);
  };
}

/**
 * How many wrong passwords end a session that is being unlocked, as the API
 * has it (`unlockAttempts` in internal/app/sessions.go). The console only
 * uses it to say why a session ended.
 */
export const UNLOCK_ATTEMPTS = 5;

// How many passwords unlocking was answered "wrong" since the session was
// last unlocked or ended.
let wrongPasswords = 0;

function tellUnauthorized(path: string, method: string): void {
  const cause: Unauthorized = { path, method, wrongPasswords };
  // The session is gone: what was counted of it is of no use any more.
  wrongPasswords = 0;
  for (const listener of unauthorizedListeners) {
    listener(cause);
  }
}

// Told of a 423 only when its problem says that it is the session that is
// locked: the status alone may come to mean something else one day.
function tellIfLocked(body: unknown): void {
  if (isProblem(body) && body.code === SESSION_LOCKED) {
    for (const listener of lockedListeners) {
      listener();
    }
  }
}

/** The path of unlocking, as the contract names it. */
export const UNLOCK_PATH = '/session/unlock';

/**
 * How long an unlock is waited for. A server that has not answered by then
 * is taken as one that could not be reached: the unlock is given up (its
 * request aborted), whatever waited for it is sent, and the lock says so and
 * can be tried again. Without it, an unlock that hangs would hold every
 * upload and every request back for as long as it hangs.
 */
export const UNLOCK_TIMEOUT_MS = 20_000;

// The unlock that is on its way, if one is; it never rejects. Unlocking
// changes the session token and the CSRF token, and the ones before are
// refused from the moment the API has done it - a moment before the console
// has the new ones. A request sent in between would carry the old ones and
// be answered 401, which reads as "the session has ended" and would end the
// uploads the user just typed a password to keep. So nothing else is sent
// while an unlock is on its way: it waits here, and goes with the new tokens.
// For no longer than UNLOCK_TIMEOUT_MS; and signing out never waits.
let unlocking: Promise<void> | null = null;

// The unlock on its way, if one is: `abandoned` once the user signed out
// while it was, so that its answer, should it still come, is not kept.
let pendingUnlock: { abandoned: boolean } | null = null;

// Which tokens are the session's now: a number that goes up whenever an
// unlock gives the session new ones. Every request is stamped with it as it
// is sent, so that a 401 to a request that carried tokens of before - sent
// before an unlock, answered after it - is told from a session that ended.
let generation = 0;
// The stamps of the requests of `api` that have been sent and not answered,
// by the id the client gives each.
const sentWith = new Map<string, number>();

const IDEMPOTENT_METHODS = new Set(['POST', 'PUT', 'DELETE']);

// What the API asks of a request made with a session: see sessionMiddleware.
function dress(request: Request): Request {
  // A token set by whoever made the request is left as it is: see signOut.
  if (request.method !== 'GET' && !request.headers.has('X-CSRF-Token')) {
    const token = readCsrfToken();
    if (token !== null) {
      request.headers.set('X-CSRF-Token', token);
    }
  }
  if (IDEMPOTENT_METHODS.has(request.method)) {
    request.headers.set('Idempotency-Key', crypto.randomUUID());
  }
  return request;
}

// Adds what the API asks of a session: the CSRF token on every request that is
// not a GET, and a fresh Idempotency-Key on every request that changes
// something. A 401 is reported to whoever listens, so that the sign-in screen
// can take over - unless the request was sent with tokens an unlock has
// replaced since; a 423 that says the session is locked to whoever listens
// for that, so that the lock can.
const sessionMiddleware: Middleware = {
  onRequest({ request, schemaPath, id }) {
    const send = () => {
      const dressed = dress(request);
      sentWith.set(id, generation);
      return dressed;
    };
    // Signing out is never held back: it must always be possible.
    const signingOut = schemaPath === '/session' && request.method === 'DELETE';
    if (unlocking === null || schemaPath === UNLOCK_PATH || signingOut) {
      return send();
    }
    // The token is read once the unlock is over: it is the new one then.
    return unlocking.then(send);
  },
  onResponse({ response, schemaPath, request, id }) {
    const sent = sentWith.get(id);
    sentWith.delete(id);
    if (response.status === 401) {
      if (sent === undefined || sent === generation) {
        tellUnauthorized(schemaPath, request.method);
      }
    } else if (response.status === 423) {
      // A copy is read, so that the answer itself is left for whoever asked.
      return response
        .clone()
        .json()
        .then(tellIfLocked, () => undefined)
        .then(() => undefined);
    }
    return undefined;
  },
  onError({ id }) {
    sentWith.delete(id);
    return undefined;
  },
};

/**
 * The client of the management API, generated from api/openapi/admin.yaml.
 * Every call the console makes goes through it.
 */
export const api = createClient<paths>({
  baseUrl: API_BASE,
  credentials: 'same-origin',
  // Looked up on every call rather than once, so that a test can replace it.
  fetch: (request) => globalThis.fetch(request),
});
api.use(sessionMiddleware);

function isProblem(value: unknown): value is Problem {
  return typeof value === 'object' && value !== null && typeof (value as Problem).title === 'string';
}

/**
 * Awaits a call made with `api` and returns its data, or throws an ApiError
 * that holds the problem the API reported. An answer that is not
 * problem+json (a proxy's error page, say) becomes a problem with the HTTP
 * status as its title.
 */
export async function unwrap<T>(
  call: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  const { data, error, response } = await call;
  if (response.ok) {
    return data as T;
  }
  if (isProblem(error)) {
    throw new ApiError(error);
  }
  throw new ApiError({
    type: 'about:blank',
    title: `The server answered ${response.status} ${response.statusText}`.trim(),
    status: response.status,
    code: 'UnexpectedResponse',
  });
}

/**
 * Unlocks the session with the password of its user. The API gives the
 * session a new token - the cookie - and a new CSRF token, and refuses the
 * ones it had: the new CSRF token is kept here, before the answer is handed
 * on, so that whatever is sent next carries it. Until then nothing else is
 * sent but a sign-out; see `unlocking`.
 *
 * A password that is not accepted is an ApiError of status 403, too many
 * attempts one of 429, and both leave the session as it was. A 401 means the
 * session has ended, and is told to whoever listens for that. An unlock not
 * answered within UNLOCK_TIMEOUT_MS is given up: an ApiError of status 0, as
 * a connection that failed is, and nothing waits for it any more. The
 * password is sent once and kept nowhere.
 */
export function unlock(password: string): Promise<Session> {
  const attempt = { abandoned: false };
  pendingUnlock = attempt;
  const stop = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      stop.abort();
      reject(
        new ApiError({
          type: 'about:blank',
          title: 'The server did not answer in time',
          status: 0,
          code: 'UnexpectedResponse',
        }),
      );
    }, UNLOCK_TIMEOUT_MS);
  });
  const answered = unwrap(api.POST(UNLOCK_PATH, { body: { password }, signal: stop.signal })).then(
    (session) => {
      if (attempt.abandoned) {
        // The user signed out while this was on its way, and the API
        // carried it out all the same: the session lives on under the
        // tokens of this answer, which the sign-out did not carry. It is
        // ended with them, and they are kept nowhere.
        void unwrap(api.DELETE('/session', { headers: { 'X-CSRF-Token': session.csrf_token } })).catch(
          () => undefined,
        );
        throw new ApiError({ type: 'about:blank', title: 'Signed out', status: 401, code: 'SignedOut' });
      }
      storeCsrfToken(session.csrf_token);
      generation += 1;
      wrongPasswords = 0;
      return session;
    },
    (error: unknown) => {
      if (error instanceof ApiError && error.problem.status === 403) {
        wrongPasswords += 1;
      }
      throw error;
    },
  );
  // An answer that comes after the unlock was given up is handled above,
  // and its failure is nobody's to hear.
  answered.catch(() => undefined);
  const call = Promise.race([answered, late]).finally(() => {
    clearTimeout(timer);
    if (pendingUnlock === attempt) {
      pendingUnlock = null;
    }
  });
  const over = call.then(
    () => undefined,
    () => undefined,
  );
  unlocking = over;
  void over.then(() => {
    if (unlocking === over) {
      unlocking = null;
    }
  });
  return call;
}

/**
 * Ends the session: `DELETE /session`, sent at once. It never waits for an
 * unlock that is on its way - signing out must always be possible - and so
 * may carry the tokens that unlock is about to replace: the API may then
 * carry the unlock out after it, and the session would live on under the new
 * ones. So that unlock is abandoned: should it succeed, its tokens are not
 * kept and the session it renewed is ended in turn.
 *
 * Resolves to null when the API signed the session out, and to why not when
 * it did not; it never rejects.
 */
export function signOut(): Promise<unknown> {
  if (pendingUnlock !== null) {
    pendingUnlock.abandoned = true;
  }
  return unwrap(api.DELETE('/session')).then(
    () => null,
    (error: unknown) => error,
  );
}

/**
 * The address an object is downloaded from. It is a plain same-origin link:
 * the session cookie authenticates it and the browser streams it to disk.
 */
export function objectDownloadUrl(bucket: string, key: string): string {
  const path: keyof paths = '/buckets/{bucket}/object';
  const query = new URLSearchParams({ key });
  return `${API_BASE}${path.replace('{bucket}', encodeURIComponent(bucket))}?${query.toString()}`;
}

/**
 * Uploads a file as an object. The file is handed to fetch as it is, so the
 * browser streams it from disk and sets Content-Length itself.
 */
export function uploadObject(bucket: string, key: string, file: File): Promise<ObjectMetadata> {
  return unwrap(
    api.PUT('/buckets/{bucket}/object', {
      params: {
        path: { bucket },
        query: { key },
        header: { 'Content-Type': file.type === '' ? 'application/octet-stream' : file.type },
      },
      // The generated type of a binary body is `string`; a File is what fetch
      // wants, and it must not be turned into JSON on the way.
      body: file as unknown as string,
      bodySerializer: (body) => body,
    }),
  );
}

/** What an upload in progress reports: bytes sent, of how many. */
export type UploadProgress = (sent: number, total: number) => void;

/**
 * Uploads a file as an object and reports how far it is. fetch cannot say how
 * much of a request body has been sent, so this one call is made with
 * XMLHttpRequest - to the same operation of the same contract, with what the
 * session middleware adds to every other call: the CSRF token, an
 * Idempotency-Key, the session cookie, the report of a 401 and of a locked
 * session, and the wait for an unlock that is on its way. A failure of the
 * connection is an ApiError of status 0.
 *
 * So is a 401 to an upload that was sent with the tokens of before an unlock
 * and answered after it: the API refused those tokens, not the session, and
 * the upload engine makes such an attempt again - with the same
 * Idempotency-Key, which nothing was stored under, and the new tokens - as
 * it does one whose connection failed. It is not told as a 401.
 *
 * An upload whose `signal` is aborted is given up: the request is aborted and
 * the promise rejects with an ApiError of status 0, "The upload was stopped".
 * Whoever aborted tells that from a failure by looking at the signal.
 */
export function uploadObjectWithProgress(
  bucket: string,
  key: string,
  file: File,
  onProgress: UploadProgress,
  idempotencyKey: string = crypto.randomUUID(),
  signal?: AbortSignal,
): Promise<ObjectMetadata> {
  const path: keyof paths = '/buckets/{bucket}/object';
  const query = new URLSearchParams({ key });
  const url = `${API_BASE}${path.replace('{bucket}', encodeURIComponent(bucket))}?${query.toString()}`;
  // The request is made at once, as it always was - unless an unlock is on
  // its way: then once that is over, with the tokens it left.
  const send = () => sendObject(url, file, onProgress, idempotencyKey, signal);
  return unlocking === null ? send() : unlocking.then(send);
}

// Makes the request of uploadObjectWithProgress, now.
function sendObject(
  url: string,
  file: File,
  onProgress: UploadProgress,
  idempotencyKey: string,
  signal: AbortSignal | undefined,
): Promise<ObjectMetadata> {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    const unexpected = (title: string) =>
      new ApiError({ type: 'about:blank', title, status: request.status, code: 'UnexpectedResponse' });
    if (signal?.aborted === true) {
      reject(unexpected('The upload was stopped'));
      return;
    }
    const stop = () => {
      request.abort();
    };
    signal?.addEventListener('abort', stop, { once: true });
    // However the request ends, nobody listens to the signal for it any more.
    request.onloadend = () => {
      signal?.removeEventListener('abort', stop);
    };
    request.open('PUT', url);
    request.responseType = 'text';
    request.setRequestHeader('Content-Type', file.type === '' ? 'application/octet-stream' : file.type);
    // The same key for every attempt at the same upload: an attempt that
    // was stored, though its answer was lost, is then not stored twice.
    request.setRequestHeader('Idempotency-Key', idempotencyKey);
    const token = readCsrfToken();
    if (token !== null) {
      request.setRequestHeader('X-CSRF-Token', token);
    }
    // Which tokens it carries: see `generation`.
    const sent = generation;
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) {
        onProgress(event.loaded, event.total);
      }
    };
    request.onerror = () => {
      reject(unexpected('The upload did not reach the server'));
    };
    request.onabort = () => {
      reject(unexpected('The upload was stopped'));
    };
    request.onload = () => {
      if (request.status === 401 && sent !== generation) {
        reject(
          new ApiError({
            type: 'about:blank',
            title: 'The upload was sent before the session was unlocked',
            status: 0,
            code: 'UnexpectedResponse',
          }),
        );
        return;
      }
      if (request.status === 401) {
        tellUnauthorized('/buckets/{bucket}/object', 'PUT');
      }
      let body: unknown;
      try {
        body = JSON.parse(request.responseText) as unknown;
      } catch {
        body = undefined;
      }
      // An upload is what a locked session may do, so this is not expected:
      // if it comes all the same, it says "locked" and not "ended".
      if (request.status === 423) {
        tellIfLocked(body);
      }
      if (request.status >= 200 && request.status < 300) {
        onProgress(file.size, file.size);
        resolve(body as ObjectMetadata);
      } else if (isProblem(body)) {
        // The console and the API are one origin: every header of the answer
        // can be read.
        reject(new ApiError(body, request.getResponseHeader(REPLAYED_HEADER) === 'true'));
      } else {
        reject(unexpected(`The server answered ${String(request.status)} ${request.statusText}`.trim()));
      }
    };
    request.send(file);
  });
}

