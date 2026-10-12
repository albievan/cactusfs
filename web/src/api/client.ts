import createClient, { type Middleware } from 'openapi-fetch';

import { readCsrfToken } from './csrf';
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
/** A bucket. */
export type Bucket = Schemas['Bucket'];
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
/** One record of the audit trail. */
export type AuditRecord = Schemas['AuditRecord'];
/** One page of the audit trail. */
export type AuditPage = Schemas['AuditPage'];

/** A request the API refused. It carries the problem the API described. */
export class ApiError extends Error {
  /** The problem as the API reported it. */
  readonly problem: Problem;

  constructor(problem: Problem) {
    super(problem.title);
    this.name = 'ApiError';
    this.problem = problem;
  }
}

const unauthorizedListeners = new Set<() => void>();

/**
 * Calls the listener whenever the API answers 401, which means there is no
 * session (any more). Returns the function that removes the listener.
 */
export function onUnauthorized(listener: () => void): () => void {
  unauthorizedListeners.add(listener);
  return () => {
    unauthorizedListeners.delete(listener);
  };
}

const IDEMPOTENT_METHODS = new Set(['POST', 'PUT', 'DELETE']);

// Adds what the API asks of a session: the CSRF token on every request that is
// not a GET, and a fresh Idempotency-Key on every request that changes
// something. A 401 is reported to whoever listens, so that the sign-in screen
// can take over.
const sessionMiddleware: Middleware = {
  onRequest({ request }) {
    if (request.method !== 'GET') {
      const token = readCsrfToken();
      if (token !== null) {
        request.headers.set('X-CSRF-Token', token);
      }
    }
    if (IDEMPOTENT_METHODS.has(request.method)) {
      request.headers.set('Idempotency-Key', crypto.randomUUID());
    }
    return request;
  },
  onResponse({ response }) {
    if (response.status === 401) {
      for (const listener of unauthorizedListeners) {
        listener();
      }
    }
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
 * Idempotency-Key, the session cookie, and the report of a 401. A failure of
 * the connection is an ApiError of status 0.
 */
export function uploadObjectWithProgress(
  bucket: string,
  key: string,
  file: File,
  onProgress: UploadProgress,
  idempotencyKey: string = crypto.randomUUID(),
): Promise<ObjectMetadata> {
  const path: keyof paths = '/buckets/{bucket}/object';
  const query = new URLSearchParams({ key });
  const url = `${API_BASE}${path.replace('{bucket}', encodeURIComponent(bucket))}?${query.toString()}`;
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
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
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) {
        onProgress(event.loaded, event.total);
      }
    };
    const unexpected = (title: string) =>
      new ApiError({ type: 'about:blank', title, status: request.status, code: 'UnexpectedResponse' });
    request.onerror = () => {
      reject(unexpected('The upload did not reach the server'));
    };
    request.onabort = () => {
      reject(unexpected('The upload was stopped'));
    };
    request.onload = () => {
      if (request.status === 401) {
        for (const listener of unauthorizedListeners) {
          listener();
        }
      }
      let body: unknown;
      try {
        body = JSON.parse(request.responseText) as unknown;
      } catch {
        body = undefined;
      }
      if (request.status >= 200 && request.status < 300) {
        onProgress(file.size, file.size);
        resolve(body as ObjectMetadata);
      } else if (isProblem(body)) {
        reject(new ApiError(body));
      } else {
        reject(unexpected(`The server answered ${String(request.status)} ${request.statusText}`.trim()));
      }
    };
    request.send(file);
  });
}

