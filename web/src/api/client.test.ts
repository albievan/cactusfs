import { File as NodeFile } from 'node:buffer';

import { describe, expect, it, vi } from 'vitest';

import { fakeApi, json, noContent, problem } from '../test/fakeApi';
import {
  api,
  ApiError,
  objectDownloadUrl,
  onUnauthorized,
  REPLAYED_HEADER,
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
