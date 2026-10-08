import { File as NodeFile } from 'node:buffer';

import { describe, expect, it, vi } from 'vitest';

import { fakeApi, json, noContent, problem } from '../test/fakeApi';
import { api, ApiError, objectDownloadUrl, onUnauthorized, unwrap, uploadObject } from './client';
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
      'POST /session': () => json(200, { user: 'alice', csrf_token: 't', expires_at: '2026-10-08T12:00:00Z' }),
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
