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
  /** Every upload made with XMLHttpRequest so far, oldest first. */
  xhrs: FakeXHR[];
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
 * What the API answers to an Idempotency-Key whose first request has not
 * finished: 409 with the code `RequestInProgress`, as adminapi/handler.go
 * has it.
 */
export function inProgress(): Response {
  const body = { type: 'about:blank', title: 'Request in progress', status: 409, code: 'RequestInProgress' };
  return new Response(JSON.stringify(body), {
    status: 409,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

/**
 * The same answer as the API gives it again to the Idempotency-Key it kept
 * it under: with the header that says so.
 */
export function replayed(answer: Response): Response {
  answer.headers.set('Idempotent-Replayed', 'true');
  return answer;
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
  // Each is kept, so that a test can tell whether it was aborted.
  const xhrs: FakeXHR[] = [];
  class TrackedXHR extends FakeXHR {
    constructor() {
      super();
      xhrs.push(this);
    }
  }
  vi.stubGlobal('XMLHttpRequest', TrackedXHR);

  return {
    calls,
    xhrs,
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

/** The build the fake API says it is, in the session answers. */
export const SERVER = { version: '0.7.0', build: '412', commit: '3300281abcde' };

/**
 * Answers for a console that is already signed in: the CSRF token is in
 * sessionStorage and `GET /session` knows the caller.
 */
export function signedIn(routes: Record<string, Handler> = {}): FakeApi {
  window.sessionStorage.setItem('cactusfs.csrf', CSRF_TOKEN);
  return fakeApi({
    'GET /session': () => json(200, { kind: 'user', via: 'session', user: 'alice', server: SERVER }),
    ...routes,
  });
}

/**
 * As much of XMLHttpRequest as an upload needs, answered by the stubbed
 * fetch. Aborted, it says so and takes no answer any more - as a browser's
 * does.
 */
export class FakeXHR {
  /** True once the console gave the request up. */
  aborted = false;
  /** How often the console asked to give the request up, whether or not it was over by then. */
  abortCalls = 0;
  /** The address the request was opened for. */
  url = '';
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
  onloadend: (() => void) | null = null;
  private method = 'GET';
  private ended = false;
  private readonly headers = new Headers();
  private answered = new Headers();

  open(method: string, url: string): void {
    this.method = method;
    this.url = url;
  }

  setRequestHeader(name: string, value: string): void {
    this.headers.set(name, value);
  }

  /** A header of the answer, or null: there is none, or no answer yet. */
  getResponseHeader(name: string): string | null {
    return this.answered.get(name);
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
        const text = await response.text();
        if (this.ended) {
          return;
        }
        this.status = response.status;
        this.statusText = response.statusText;
        this.responseText = text;
        this.answered = response.headers;
        this.end(this.onload);
      })
      .catch(() => {
        if (!this.ended) {
          this.end(this.onerror);
        }
      });
  }

  abort(): void {
    this.abortCalls += 1;
    if (this.ended) {
      return;
    }
    this.aborted = true;
    this.end(this.onabort);
  }

  private end(tell: (() => void) | null): void {
    this.ended = true;
    tell?.();
    this.onloadend?.();
  }
}

