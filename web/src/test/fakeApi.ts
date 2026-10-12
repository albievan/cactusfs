import { vi } from 'vitest';

/** One request the console made, as the fake API saw it. */
export interface Call {
  /** The HTTP method. */
  method: string;
  /** The path below /api/v1, for example `/buckets/photos/objects`. */
  path: string;
  /** The query of the address. */
  query: URLSearchParams;
  /** The request headers. */
  headers: Headers;
  /** The request body as text; empty when there was none. */
  body: string;
  /** What the request asked for credentials (`same-origin`, …). */
  credentials: RequestCredentials;
  /** The signal of the request: aborted when the console gave the request up. */
  signal: AbortSignal;
}

/** Answers one request; the key it is registered under is `METHOD /path`. */
export type Handler = (call: Call) => Response;

/** The fake API: what it was asked, and a way to change its answers. */
export interface FakeApi {
  /** Every request so far, oldest first. */
  calls: Call[];
  /** The requests made with this method to this path. */
  callsTo: (method: string, path: string) => Call[];
  /** Adds or replaces answers. */
  on: (routes: Record<string, Handler>) => void;
}

/** A JSON answer. */
export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** An answer without a body. */
export function noContent(): Response {
  return new Response(null, { status: 204 });
}

/** A refusal, as application/problem+json. */
export function problem(status: number, title: string, detail?: string, requestId?: string): Response {
  const body = { type: 'about:blank', title, status, code: 'Test', detail, request_id: requestId };
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

/**
 * Replaces `fetch` with a fake management API. A request nobody registered an
 * answer for gets a 501 problem, so that a test cannot pass by accident.
 */
export function fakeApi(routes: Record<string, Handler> = {}): FakeApi {
  const handlers = new Map<string, Handler>(Object.entries(routes));
  const calls: Call[] = [];

  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const call: Call = {
      method: request.method,
      path: url.pathname.replace(/^\/api\/v1/, ''),
      query: url.searchParams,
      headers: request.headers,
      body: await request.text(),
      credentials: request.credentials,
      signal: request.signal,
    };
    calls.push(call);
    const handler = handlers.get(`${call.method} ${call.path}`);
    return handler === undefined
      ? problem(501, 'No answer registered', `${call.method} ${call.path}`)
      : handler(call);
  });

  // The upload with progress is made with XMLHttpRequest; here it asks the
  // same fake API, and reports its progress in two steps.
  vi.stubGlobal('XMLHttpRequest', FakeXHR);

  return {
    calls,
    callsTo: (method, path) => calls.filter((call) => call.method === method && call.path === path),
    on: (more) => {
      for (const [key, handler] of Object.entries(more)) {
        handlers.set(key, handler);
      }
    },
  };
}

/** The CSRF token of the session that signedIn starts. */
export const CSRF_TOKEN = 'csrf-token-of-the-test';

/**
 * Answers for a console that is already signed in: the CSRF token is in
 * sessionStorage and `GET /session` knows the caller.
 */
export function signedIn(routes: Record<string, Handler> = {}): FakeApi {
  window.sessionStorage.setItem('cactusfs.csrf', CSRF_TOKEN);
  return fakeApi({
    'GET /session': () => json(200, { kind: 'user', via: 'session', user: 'alice' }),
    ...routes,
  });
}

/** As much of XMLHttpRequest as an upload needs, answered by the stubbed fetch. */
class FakeXHR {
  status = 0;
  statusText = '';
  responseText = '';
  responseType = '';
  upload: { onprogress: ((event: { lengthComputable: boolean; loaded: number; total: number }) => void) | null } = {
    onprogress: null,
  };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  private method = 'GET';
  private url = '';
  private readonly headers = new Headers();

  open(method: string, url: string): void {
    this.method = method;
    this.url = url;
  }

  setRequestHeader(name: string, value: string): void {
    this.headers.set(name, value);
  }

  send(body: Blob): void {
    const total = body.size;
    this.upload.onprogress?.({ lengthComputable: true, loaded: Math.floor(total / 2), total });
    void body
      .text()
      .then((text) =>
        globalThis.fetch(
          new Request(new URL(this.url, 'http://localhost'), { method: this.method, headers: this.headers, body: text }),
        ),
      )
      .then(async (response) => {
        this.status = response.status;
        this.statusText = response.statusText;
        this.responseText = await response.text();
        this.onload?.();
      })
      .catch(() => {
        this.onerror?.();
      });
  }
}

