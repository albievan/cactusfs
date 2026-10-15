import { File as NodeFile } from 'node:buffer';

import { describe, expect, it, vi } from 'vitest';

import { fakeApi, json, noContent, problem, refusal, SERVER, sessionLocked, wrongPassword } from '../test/fakeApi';
import {
  API_BASE,
  api,
  ApiError,
  objectDownloadUrl,
  onLocked,
  onUnauthorized,
  REPLAYED_HEADER,
  SESSION_LOCKED,
  signOut,
  UNLOCK_ATTEMPTS,
  UNLOCK_TIMEOUT_MS,
  unlock,
  unwrap,
  uploadObject,
  uploadObjectWithProgress,
} from './client';
import { dropCsrfToken, readCsrfToken, storeCsrfToken } from './csrf';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

describe('the API client', () => {
  it('calls the API under /api/v1 on the same origin with same-origin credentials', async () => {
    const fake = fakeApi({ 'GET /buckets': () => json(200, []) });

    await unwrap(api.GET('/buckets'));

    expect(fake.calls).toHaveLength(1);
    expect(fake.calls[0]?.path).toBe('/buckets');
    expect(fake.calls[0]?.credentials).toBe('same-origin');
  });

  it('sends neither a CSRF token nor an Idempotency-Key with a GET', async () => {
    storeCsrfToken('token');
    const fake = fakeApi({ 'GET /buckets': () => json(200, []) });

    await unwrap(api.GET('/buckets'));

    expect(fake.calls[0]?.headers.has('X-CSRF-Token')).toBe(false);
    expect(fake.calls[0]?.headers.has('Idempotency-Key')).toBe(false);
  });

  it('sends the CSRF token and a fresh Idempotency-Key with every POST, PUT and DELETE', async () => {
    storeCsrfToken('token');
    const fake = fakeApi({
      'POST /buckets': () => json(201, { name: 'photos', created_at: '2026-10-01T00:00:00Z' }),
      'PUT /groups/ops': () => json(200, { name: 'ops', roles: [], created_at: '2026-10-01T00:00:00Z' }),
      'DELETE /buckets/photos': () => noContent(),
    });

    await unwrap(api.POST('/buckets', { body: { name: 'photos' } }));
    await unwrap(api.PUT('/groups/{name}', { params: { path: { name: 'ops' } }, body: { roles: [] } }));
    await unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));

    expect(fake.calls.map((call) => call.method)).toEqual(['POST', 'PUT', 'DELETE']);
    const keys = fake.calls.map((call) => call.headers.get('Idempotency-Key') ?? '');
    for (const call of fake.calls) {
      expect(call.headers.get('X-CSRF-Token')).toBe('token');
    }
    for (const key of keys) {
      expect(key).toMatch(UUID);
    }
    expect(new Set(keys).size).toBe(3);
  });

  it('sends no CSRF header when there is no session', async () => {
    const fake = fakeApi({
      'POST /session': () =>
        json(200, {
          user: 'alice',
          csrf_token: 't',
          expires_at: '2026-10-08T12:00:00Z',
          server: { version: '0.7.0', build: '412' },
        }),
    });

    await unwrap(api.POST('/session', { body: { username: 'alice', password: 'secret' } }));

    expect(fake.calls[0]?.headers.has('X-CSRF-Token')).toBe(false);
    expect(fake.calls[0]?.headers.get('Idempotency-Key')).toMatch(UUID);
  });

  it('throws the problem the API reported', async () => {
    fakeApi({
      'DELETE /buckets/photos': () => problem(409, 'Bucket not empty', 'Delete its objects first.', 'req-9'),
    });

    const failure = unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));

    await expect(failure).rejects.toBeInstanceOf(ApiError);
    await failure.catch((error: unknown) => {
      expect((error as ApiError).problem).toMatchObject({
        status: 409,
        title: 'Bucket not empty',
        detail: 'Delete its objects first.',
        request_id: 'req-9',
      });
    });
  });

  it('makes a problem of an answer that is not problem+json', async () => {
    fakeApi({ 'GET /buckets': () => new Response('<html>Bad gateway</html>', { status: 502 }) });

    await expect(unwrap(api.GET('/buckets'))).rejects.toMatchObject({
      problem: { status: 502, code: 'UnexpectedResponse' },
    });
  });

  it('tells its listeners about a 401, until they stop listening', async () => {
    fakeApi({ 'GET /session': () => problem(401, 'Not signed in') });
    const listener = vi.fn();
    const stop = onUnauthorized(listener);

    await expect(unwrap(api.GET('/session'))).rejects.toBeInstanceOf(ApiError);
    expect(listener).toHaveBeenCalledTimes(1);

    stop();
    await expect(unwrap(api.GET('/session'))).rejects.toBeInstanceOf(ApiError);
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('builds the download address of an object', () => {
    expect(objectDownloadUrl('photos', '2026/a b&c.jpg')).toBe(
      '/api/v1/buckets/photos/object?key=2026%2Fa+b%26c.jpg',
    );
  });

  it('uploads a file as it is, with its media type', async () => {
    storeCsrfToken('token');
    const fake = fakeApi({
      'PUT /buckets/photos/object': () =>
        json(200, {
          bucket: 'photos',
          key: 'notes/hello.txt',
          size: 5,
          etag: '"abc"',
          last_modified: '2026-10-01T00:00:00Z',
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        }),
    });
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;

    const stored = await uploadObject('photos', 'notes/hello.txt', file);

    expect(stored.size).toBe(5);
    const [call] = fake.calls;
    expect(call?.query.get('key')).toBe('notes/hello.txt');
    expect(call?.headers.get('Content-Type')).toBe('text/plain');
    expect(call?.headers.get('X-CSRF-Token')).toBe('token');
    expect(call?.headers.get('Idempotency-Key')).toMatch(UUID);
    expect(call?.body).toBe('hello');
  });

  it('gives an upload up when its signal is aborted, and says that it was stopped', async () => {
    const fake = fakeApi({ 'PUT /buckets/photos/object': () => json(200, {}) });
    // The answer is held: the upload is on its way for as long as the test has it.
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      await new Promise(() => undefined);
      return answer(input, init);
    });
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;
    const stop = new AbortController();

    const upload = uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined, 'the-same-key', stop.signal);
    expect(fake.xhrs).toHaveLength(1);
    expect(fake.xhrs[0]?.aborted).toBe(false);
    stop.abort();

    const refused: unknown = await upload.catch((error: unknown) => error);
    expect(refused).toBeInstanceOf(ApiError);
    expect((refused as ApiError).problem.title).toBe('The upload was stopped');
    expect((refused as ApiError).problem.status).toBe(0);
    expect(fake.xhrs[0]?.aborted).toBe(true);
    expect(fake.calls).toHaveLength(0);
  });

  it('does not begin an upload whose signal was aborted before', async () => {
    const fake = fakeApi({ 'PUT /buckets/photos/object': () => json(200, {}) });
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;
    const stop = new AbortController();
    stop.abort();

    const refused: unknown = await uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined, 'key', stop.signal).catch(
      (error: unknown) => error,
    );

    expect(refused).toBeInstanceOf(ApiError);
    expect((refused as ApiError).problem.title).toBe('The upload was stopped');
    // The request was made and never sent: nothing reached the API.
    expect(fake.xhrs.every((xhr) => !xhr.aborted)).toBe(true);
    expect(fake.calls).toHaveLength(0);
  });

  it('sends an upload that is not stopped with the key it was given, and no longer listens to its signal', async () => {
    const fake = fakeApi({ 'PUT /buckets/photos/object': () => json(200, { key: 'hello.txt' }) });
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;
    const stop = new AbortController();
    const listened = vi.spyOn(stop.signal, 'addEventListener');
    const unlistened = vi.spyOn(stop.signal, 'removeEventListener');

    await uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined, 'the-same-key', stop.signal);
    // What listened to the signal for this request was taken off it again.
    const [listener] = listened.mock.calls.filter(([type]) => type === 'abort').map(([, heard]) => heard);
    expect(listener).toBeTypeOf('function');
    expect(unlistened).toHaveBeenCalledWith('abort', listener);
    // Aborting afterwards is word of nothing: the request is not even asked to stop.
    stop.abort();

    expect(fake.calls[0]?.headers.get('Idempotency-Key')).toBe('the-same-key');
    expect(fake.xhrs[0]?.abortCalls).toBe(0);
    expect(fake.xhrs[0]?.aborted).toBe(false);
  });

  it('says of a refusal whether the API gave it now or had kept it under the Idempotency-Key', async () => {
    let kept = false;
    fakeApi({
      'PUT /buckets/photos/object': () => {
        const refusal = problem(400, 'Incomplete body');
        if (kept) {
          refusal.headers.set(REPLAYED_HEADER, 'true');
        }
        return refusal;
      },
    });
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;
    const send = () =>
      uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined, 'the-same-key').catch((error: unknown) => error);

    const first = await send();
    expect(first).toBeInstanceOf(ApiError);
    expect((first as ApiError).replayed).toBe(false);

    kept = true;
    const again = await send();
    expect(again).toBeInstanceOf(ApiError);
    expect((again as ApiError).problem.status).toBe(400);
    expect((again as ApiError).replayed).toBe(true);
    // The header is the one the API sets: adminapi/handler.go, headerReplayed.
    expect(REPLAYED_HEADER).toBe('Idempotent-Replayed');
  });
});

describe('the CSRF token', () => {
  it('is kept in sessionStorage and dropped on request', () => {
    expect(readCsrfToken()).toBeNull();

    storeCsrfToken('token');
    expect(readCsrfToken()).toBe('token');
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe('token');

    dropCsrfToken();
    expect(readCsrfToken()).toBeNull();
  });
});

describe('a session that is locked', () => {
  /** Listens for a locked session and for one that has ended, until `stop`. */
  function listen() {
    const locked = vi.fn();
    const ended = vi.fn();
    const stops = [onLocked(locked), onUnauthorized(ended)];
    return {
      locked,
      ended,
      stop: () => {
        for (const stop of stops) {
          stop();
        }
      },
    };
  }

  /** What unlocking answers when the password was accepted. */
  function unlockedWith(csrfToken: string): Response {
    return json(200, { user: 'alice', csrf_token: csrfToken, expires_at: '2026-10-08T12:30:00Z', server: SERVER });
  }

  it('tells its listeners about a 423 that says so, until they stop listening, and nobody who listens for a 401', async () => {
    fakeApi({ 'GET /buckets': () => sessionLocked() });
    const heard = listen();

    // Whoever asked still gets the problem as the API described it.
    await expect(unwrap(api.GET('/buckets'))).rejects.toMatchObject({
      problem: { status: 423, code: 'SessionLocked', title: 'The session is locked' },
    });
    expect(heard.locked).toHaveBeenCalledTimes(1);
    expect(heard.ended).not.toHaveBeenCalled();
    // The code is the one the API gives it: adminapi/handler.go, problemOf.
    expect(SESSION_LOCKED).toBe('SessionLocked');

    heard.stop();
    await expect(unwrap(api.GET('/buckets'))).rejects.toBeInstanceOf(ApiError);
    expect(heard.locked).toHaveBeenCalledTimes(1);
  });

  it('takes a 423 for a locked session only by its code', async () => {
    fakeApi({
      'GET /buckets': () => refusal(423, 'Locked', 'Something else is locked'),
      'GET /users': () => new Response('<html>Locked</html>', { status: 423 }),
    });
    const heard = listen();

    await expect(unwrap(api.GET('/buckets'))).rejects.toMatchObject({ problem: { status: 423, code: 'Locked' } });
    await expect(unwrap(api.GET('/users'))).rejects.toMatchObject({
      problem: { status: 423, code: 'UnexpectedResponse' },
    });

    expect(heard.locked).not.toHaveBeenCalled();
    expect(heard.ended).not.toHaveBeenCalled();
    heard.stop();
  });

  it('does not take a 401 for a locked session', async () => {
    fakeApi({ 'GET /buckets': () => problem(401, 'Not signed in') });
    const heard = listen();

    await expect(unwrap(api.GET('/buckets'))).rejects.toBeInstanceOf(ApiError);

    expect(heard.ended).toHaveBeenCalledTimes(1);
    expect(heard.locked).not.toHaveBeenCalled();
    heard.stop();
  });

  it('says "locked", not "ended", when an upload is answered 423', async () => {
    fakeApi({ 'PUT /buckets/photos/object': () => sessionLocked() });
    const heard = listen();
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;

    await expect(uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined)).rejects.toMatchObject({
      problem: { status: 423, code: 'SessionLocked' },
    });

    expect(heard.locked).toHaveBeenCalledTimes(1);
    expect(heard.ended).not.toHaveBeenCalled();
    heard.stop();
  });

  it('unlocks with the password and the CSRF token of the session, and keeps the token it is answered with', async () => {
    storeCsrfToken('old');
    const fake = fakeApi({ 'POST /session/unlock': () => unlockedWith('new') });

    const session = await unlock('correct horse battery staple');

    expect(session.csrf_token).toBe('new');
    expect(session.expires_at).toBe('2026-10-08T12:30:00Z');
    expect(readCsrfToken()).toBe('new');
    expect(fake.calls).toHaveLength(1);
    expect(JSON.parse(fake.calls[0]?.body ?? '')).toEqual({ password: 'correct horse battery staple' });
    expect(fake.calls[0]?.headers.get('X-CSRF-Token')).toBe('old');
    expect(fake.calls[0]?.headers.get('Content-Type')).toBe('application/json');
    expect(fake.calls[0]?.credentials).toBe('same-origin');
  });

  it('keeps the CSRF token it had when the password is not accepted, and says nothing of a session that ended', async () => {
    storeCsrfToken('old');
    fakeApi({ 'POST /session/unlock': () => wrongPassword() });
    const heard = listen();

    await expect(unlock('wrong')).rejects.toMatchObject({ problem: { status: 403, code: 'AccessDenied' } });

    expect(readCsrfToken()).toBe('old');
    expect(heard.ended).not.toHaveBeenCalled();
    expect(heard.locked).not.toHaveBeenCalled();
    heard.stop();
  });

  it('tells whoever listens for a 401 when unlocking finds that the session has ended', async () => {
    storeCsrfToken('old');
    fakeApi({ 'POST /session/unlock': () => problem(401, 'Not signed in') });
    const heard = listen();

    await expect(unlock('whatever')).rejects.toMatchObject({ problem: { status: 401 } });

    expect(heard.ended).toHaveBeenCalledTimes(1);
    heard.stop();
  });

  it('sends nothing else while an unlock is on its way, and then sends it with the new CSRF token', async () => {
    storeCsrfToken('old');
    const fake = fakeApi({
      'POST /session/unlock': () => unlockedWith('new'),
      'DELETE /buckets/photos': () => noContent(),
      'PUT /buckets/photos/object': () =>
        json(200, {
          bucket: 'photos',
          key: 'hello.txt',
          size: 5,
          etag: '"abc"',
          last_modified: '2026-10-01T00:00:00Z',
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        }),
    });
    // The unlock is held on its way to the API for as long as the test likes.
    let letGo: () => void = () => undefined;
    const held = new Promise<void>((resolve) => {
      letGo = resolve;
    });
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (new URL(request.url).pathname.endsWith('/session/unlock')) {
        await held;
      }
      return answer(request);
    });
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;

    const unlocking = unlock('correct horse battery staple');
    const deleting = unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));
    const uploading = uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined);

    // Neither has been sent: the tokens they would carry are about to be refused.
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 20);
    });
    expect(fake.calls).toHaveLength(0);
    expect(fake.xhrs).toHaveLength(0);

    letGo();
    await Promise.all([unlocking, deleting, uploading]);

    expect(readCsrfToken()).toBe('new');
    expect(fake.callsTo('POST', '/session/unlock')[0]?.headers.get('X-CSRF-Token')).toBe('old');
    expect(fake.callsTo('DELETE', '/buckets/photos')[0]?.headers.get('X-CSRF-Token')).toBe('new');
    expect(fake.callsTo('PUT', '/buckets/photos/object')[0]?.headers.get('X-CSRF-Token')).toBe('new');
    // The first call of the list is the unlock: the others came after it.
    expect(fake.calls[0]?.path).toBe('/session/unlock');

    // Nothing is held once the unlock is over.
    await unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));
    expect(fake.callsTo('DELETE', '/buckets/photos')).toHaveLength(2);
  });

  it('sends what waited for an unlock that was refused, with the token the session still has', async () => {
    storeCsrfToken('old');
    const fake = fakeApi({
      'POST /session/unlock': () => wrongPassword(),
      'DELETE /buckets/photos': () => noContent(),
    });

    const unlocking = unlock('wrong');
    const deleting = unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));

    await expect(unlocking).rejects.toBeInstanceOf(ApiError);
    await deleting;
    expect(fake.calls.map((call) => call.path)).toEqual(['/session/unlock', '/buckets/photos']);
    expect(fake.callsTo('DELETE', '/buckets/photos')[0]?.headers.get('X-CSRF-Token')).toBe('old');
  });

  /**
   * Holds the requests to a path on their way to the fake API until
   * `release`, after which they and whatever comes later pass. A request that
   * is held has not reached the API; one whose signal is aborted while it is
   * held fails as fetch fails then.
   */
  function hold(path: string) {
    const answer = globalThis.fetch;
    let free = false;
    let letGo: () => void = () => undefined;
    const released = new Promise<void>((resolve) => {
      letGo = resolve;
    });
    let arrived: () => void = () => undefined;
    const reached = new Promise<void>((resolve) => {
      arrived = resolve;
    });
    const signals: AbortSignal[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (!free && new URL(request.url).pathname === `${API_BASE}${path}`) {
        signals.push(request.signal);
        arrived();
        await new Promise<void>((resolve, reject) => {
          void released.then(resolve);
          request.signal.addEventListener(
            'abort',
            () => {
              reject(new DOMException('The request was aborted', 'AbortError'));
            },
            { once: true },
          );
        });
      }
      return answer(request);
    });
    return {
      /** Settles once a request to the path has come to be held. */
      reached,
      /** The signals of the requests that were held. */
      signals,
      release: () => {
        free = true;
        letGo();
      },
    };
  }

  const stored = {
    bucket: 'photos',
    key: 'hello.txt',
    size: 5,
    etag: '"abc"',
    last_modified: '2026-10-01T00:00:00Z',
    content_type: 'text/plain',
    encryption: 'SSE-S3',
  };

  it('gives up an unlock the server does not answer in time, and then sends what waited for it', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      storeCsrfToken('old');
      const fake = fakeApi({
        'DELETE /buckets/photos': () => noContent(),
        'PUT /buckets/photos/object': () => json(200, stored),
      });
      const held = hold('/session/unlock');
      const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;

      const unlocking = unlock('correct horse battery staple');
      const givenUp = expect(unlocking).rejects.toMatchObject({ problem: { status: 0, code: 'UnexpectedResponse' } });
      const deleting = unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));
      const uploading = uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined);
      await held.reached;

      // Until the time is up, nothing else is sent.
      await vi.advanceTimersByTimeAsync(UNLOCK_TIMEOUT_MS - 1);
      expect(fake.calls).toHaveLength(0);
      expect(fake.xhrs).toHaveLength(0);

      await vi.advanceTimersByTimeAsync(1);
      await givenUp;
      // The request of the unlock is aborted, not left to hang.
      expect(held.signals.map((signal) => signal.aborted)).toEqual([true]);
      // What waited goes now, with the token the session still has.
      await Promise.all([deleting, uploading]);
      expect(fake.callsTo('DELETE', '/buckets/photos')[0]?.headers.get('X-CSRF-Token')).toBe('old');
      expect(fake.callsTo('PUT', '/buckets/photos/object')[0]?.headers.get('X-CSRF-Token')).toBe('old');
      expect(readCsrfToken()).toBe('old');
      // Nothing waits any more.
      await unwrap(api.DELETE('/buckets/{bucket}', { params: { path: { bucket: 'photos' } } }));
      expect(fake.callsTo('DELETE', '/buckets/photos')).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('signs out at once while an unlock is on its way, and ends the session that unlock renews if the API carries it out', async () => {
    storeCsrfToken('old');
    const fake = fakeApi({
      'POST /session/unlock': () => unlockedWith('new'),
      'DELETE /session': () => noContent(),
    });
    const held = hold('/session/unlock');
    const heard = listen();

    const unlocking = unlock('correct horse battery staple');
    const failed = expect(unlocking).rejects.toBeInstanceOf(ApiError);
    await held.reached;

    // The sign-out does not wait for the unlock.
    await expect(signOut()).resolves.toBeNull();
    expect(fake.callsTo('DELETE', '/session')).toHaveLength(1);
    expect(fake.callsTo('DELETE', '/session')[0]?.headers.get('X-CSRF-Token')).toBe('old');

    // The API carries the unlock out after all: the session it renewed is
    // ended with the tokens of that answer, which are kept nowhere.
    held.release();
    await failed;
    await vi.waitFor(() => {
      expect(fake.callsTo('DELETE', '/session')).toHaveLength(2);
    });
    expect(fake.callsTo('DELETE', '/session')[1]?.headers.get('X-CSRF-Token')).toBe('new');
    expect(readCsrfToken()).toBe('old');
    expect(heard.ended).not.toHaveBeenCalled();
    heard.stop();
  });

  it('does not take a 401 for the end of the session when the request was sent before an unlock and answered after it', async () => {
    storeCsrfToken('old');
    fakeApi({
      'POST /session/unlock': () => unlockedWith('new'),
      'GET /buckets': () => problem(401, 'Not signed in'),
    });
    const held = hold('/buckets');
    const heard = listen();

    const listing = unwrap(api.GET('/buckets'));
    await held.reached;
    await unlock('correct horse battery staple');
    held.release();

    // Whoever asked is told that it failed; nobody is told that the session ended.
    await expect(listing).rejects.toMatchObject({ problem: { status: 401 } });
    expect(heard.ended).not.toHaveBeenCalled();

    // The same answer to a request sent after the unlock is a session that ended.
    await expect(unwrap(api.GET('/buckets'))).rejects.toMatchObject({ problem: { status: 401 } });
    expect(heard.ended).toHaveBeenCalledTimes(1);
    heard.stop();
  });

  it('makes an upload that was sent before an unlock and answered 401 after it one to try again, not the end of the session', async () => {
    storeCsrfToken('old');
    fakeApi({
      'POST /session/unlock': () => unlockedWith('new'),
      'PUT /buckets/photos/object': () => problem(401, 'Not signed in'),
    });
    const held = hold('/buckets/photos/object');
    const heard = listen();
    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' }) as unknown as File;

    const uploading = uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined);
    await held.reached;
    await unlock('correct horse battery staple');
    held.release();

    // Status 0, as a connection that failed: the engine makes it again.
    await expect(uploading).rejects.toMatchObject({ problem: { status: 0, code: 'UnexpectedResponse' } });
    expect(heard.ended).not.toHaveBeenCalled();

    // An upload sent after the unlock and answered 401 is a session that ended.
    await expect(uploadObjectWithProgress('photos', 'hello.txt', file, () => undefined)).rejects.toMatchObject({
      problem: { status: 401 },
    });
    expect(heard.ended).toHaveBeenCalledTimes(1);
    heard.stop();
  });

  it('tells, with a 401, which request it answered and how many passwords were wrong before', async () => {
    storeCsrfToken('old');
    const fake = fakeApi({ 'POST /session/unlock': () => unlockedWith('new') });
    // Counted from an unlock.
    await unlock('correct horse battery staple');
    fake.on({ 'POST /session/unlock': () => wrongPassword() });
    for (let attempt = 1; attempt < UNLOCK_ATTEMPTS; attempt += 1) {
      await expect(unlock('wrong')).rejects.toMatchObject({ problem: { status: 403 } });
    }
    fake.on({
      'POST /session/unlock': () => problem(401, 'Not signed in'),
      'GET /session': () => problem(401, 'Not signed in'),
    });
    const ended = vi.fn();
    const stop = onUnauthorized(ended);

    await expect(unlock('wrong again')).rejects.toMatchObject({ problem: { status: 401 } });
    expect(ended).toHaveBeenLastCalledWith({
      path: '/session/unlock',
      method: 'POST',
      wrongPasswords: UNLOCK_ATTEMPTS - 1,
    });
    // The session is gone, and so is what was counted of it.
    await expect(unwrap(api.GET('/session'))).rejects.toBeInstanceOf(ApiError);
    expect(ended).toHaveBeenLastCalledWith({ path: '/session', method: 'GET', wrongPasswords: 0 });
    stop();
  });
});
