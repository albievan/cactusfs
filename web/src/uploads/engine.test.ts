import { File as NodeFile } from 'node:buffer';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { NoticeInput } from '../notices/state';
import { fakeApi, inProgress, json, problem, replayed, type FakeApi, type Handler } from '../test/fakeApi';
import {
  busyWait,
  createUploads,
  STILL_WORKING,
  UPLOAD_BUSY_MAX_MS,
  UPLOAD_BUSY_MS,
  UPLOAD_BUSY_WAITS,
  UPLOAD_RETRY_MS,
  UPLOADS_AT_ONCE,
  type UploadEvent,
  type UploadsEngine,
} from './engine';

// The sending of the uploads by itself: no screen, no React. The clock is
// the test's - only setTimeout and clearTimeout, which are all the engine
// asks of it - so that the pauses between attempts take no time and are
// stepped through one by one. The requests are real promises, as in the
// other tests, and are waited for with the real clock.

const realSetTimeout = globalThis.setTimeout;

/** Waits, by the real clock, until something is so. */
async function until(what: () => boolean, said = 'what was waited for'): Promise<void> {
  for (let turn = 0; turn < 1000; turn += 1) {
    if (what()) {
      return;
    }
    await new Promise<void>((resolve) => {
      realSetTimeout(resolve, 1);
    });
  }
  throw new Error(`${said} never came`);
}

function file(name: string): File {
  return new NodeFile([`contents of ${name}`], name, { type: 'text/plain' }) as unknown as File;
}

/** The API stores what it is sent. */
const stores: Handler = (call) =>
  json(200, {
    bucket: 'photos',
    key: call.query.get('key') ?? '',
    size: call.body.length,
    etag: '"e"',
    last_modified: '2026-10-01T00:00:00Z',
    content_type: 'text/plain',
    encryption: 'SSE-S3',
  });

/** The request never arrives, or its answer never does: to the console, a failed connection. */
const connectionFails: Handler = () => {
  throw new TypeError('the connection was reset');
};

/** Answers the requests one after the other with these, and with the last of them from then on. */
function inTurn(...answers: Handler[]): Handler {
  let next = 0;
  return (call) => {
    const answer = answers[Math.min(next, answers.length - 1)];
    next += 1;
    if (answer === undefined) {
      throw new Error('no answer was given');
    }
    return answer(call);
  };
}

/**
 * Holds every request on its way to the fake API until the test lets it go,
 * by the bucket and the key it is for: `photos/a.txt`.
 */
function hold() {
  const waiting = new Map<string, () => void>();
  const answer = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const bucket = /\/buckets\/([^/]+)\//.exec(url.pathname)?.[1] ?? '';
    await new Promise<void>((resolve) => {
      waiting.set(`${bucket}/${url.searchParams.get('key') ?? ''}`, resolve);
    });
    return answer(request);
  });
  return {
    /** The requests that are held, in the order they came. */
    held: () => [...waiting.keys()],
    release: (...names: string[]) => {
      for (const name of names) {
        const go = waiting.get(name);
        if (go === undefined) {
          throw new Error(`${name} is not held`);
        }
        waiting.delete(name);
        go();
      }
    },
  };
}

/** The engine of a test, with what it told. */
function uploadsOf() {
  const notices: NoticeInput[] = [];
  const events: UploadEvent[] = [];
  const engine = createUploads((notice) => {
    notices.push(notice);
  });
  engine.listen((event) => {
    events.push(event);
  });
  return { engine, notices, events, settled: () => events.filter((event) => event.type === 'settled') };
}

function statesOf(engine: UploadsEngine): string[] {
  return engine.getState().uploads.map((upload) => upload.state);
}

function keysOf(api: FakeApi, bucket = 'photos'): (string | null)[] {
  return api.callsTo('PUT', `/buckets/${bucket}/object`).map((call) => call.headers.get('Idempotency-Key'));
}

/**
 * Waits until the one file of a test has failed an attempt and the engine
 * pauses before the next. Two things are then waited for by the clock: that
 * pause, and the showing of the progress the attempt reported when it was
 * begun - which is set at once, and runs out only when the test moves the
 * clock.
 */
async function paused(): Promise<void> {
  await until(() => vi.getTimerCount() === 2, 'the pause before the next attempt');
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the sending of uploads', () => {
  it('waits longer each time the API says it is still working, and no longer than the limit', () => {
    expect(busyWait(1)).toBe(UPLOAD_BUSY_MS);
    expect(busyWait(2)).toBe(2 * UPLOAD_BUSY_MS);
    expect(busyWait(3)).toBe(4 * UPLOAD_BUSY_MS);
    expect(busyWait(4)).toBe(UPLOAD_BUSY_MAX_MS);
    expect(busyWait(UPLOAD_BUSY_WAITS)).toBe(UPLOAD_BUSY_MAX_MS);
    // All of them together outlast a commit that takes a minute and a half.
    const all = Array.from({ length: UPLOAD_BUSY_WAITS }, (_, index) => busyWait(index + 1)).reduce((sum, ms) => sum + ms, 0);
    expect(all).toBeGreaterThanOrEqual(90_000);
  });

  it('makes no further attempt at a file that is cancelled in the pause between two, and waits for nothing more', async () => {
    const api = fakeApi({ 'PUT /buckets/photos/object': inTurn(connectionFails, stores) });
    const { engine, notices, settled } = uploadsOf();

    engine.start('photos', [{ file: file('a.txt'), key: 'a.txt' }]);
    await paused();
    expect(statesOf(engine)).toEqual(['sending']);
    expect(api.xhrs).toHaveLength(1);

    const [upload] = engine.getState().uploads;
    engine.cancel(upload?.id ?? '');
    // The pause is over at once, and its timer with it.
    expect(vi.getTimerCount()).toBe(1);
    expect(statesOf(engine)).toEqual(['cancelled']);
    expect(settled()).toHaveLength(1);
    expect(notices.map((notice) => notice.kind)).toEqual(['info']);

    await vi.advanceTimersByTimeAsync(60_000);
    expect(vi.getTimerCount()).toBe(0);
    // The API would have stored it: it was never asked again.
    expect(api.xhrs).toHaveLength(1);
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(1);
    expect(statesOf(engine)).toEqual(['cancelled']);
    expect(settled()).toHaveLength(1);
    expect(notices).toHaveLength(1);
  });

  it('makes no further attempt when it is disposed of in the pause between two, without a word', async () => {
    const api = fakeApi({ 'PUT /buckets/photos/object': inTurn(connectionFails, stores) });
    const { engine, notices, events } = uploadsOf();

    engine.start('photos', [{ file: file('a.txt'), key: 'a.txt' }]);
    await paused();

    engine.dispose();
    expect(vi.getTimerCount()).toBe(0);

    await vi.advanceTimersByTimeAsync(60_000);
    expect(api.xhrs).toHaveLength(1);
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(1);
    expect(notices).toEqual([]);
    expect(events).toEqual([]);
  });

  it('says once more how a batch ended when what failed in it is sent again', async () => {
    let allowed = false;
    const api = fakeApi({
      'PUT /buckets/photos/object': (call) =>
        allowed || call.query.get('key') === 'good.txt' ? stores(call) : problem(403, 'Access denied'),
    });
    const { engine, notices, settled } = uploadsOf();

    engine.start('photos', [
      { file: file('good.txt'), key: 'good.txt' },
      { file: file('bad.txt'), key: 'bad.txt' },
    ]);
    await until(() => notices.length === 1, 'the notice of the batch');
    expect(statesOf(engine)).toEqual(['stored', 'failed']);
    expect(notices[0]).toMatchObject({ kind: 'error', title: '1 of 2 objects were not stored in photos', detail: 'Access denied' });
    expect(settled()).toHaveLength(1);

    allowed = true;
    engine.retryFailed();
    // It has its turn at once: nothing else is on its way.
    expect(statesOf(engine)).toEqual(['stored', 'sending']);
    await until(() => notices.length === 2, 'the second notice of the batch');
    expect(statesOf(engine)).toEqual(['stored', 'stored']);
    expect(notices[1]).toMatchObject({ kind: 'success', title: 'Stored 2 objects in photos' });
    expect(settled()).toHaveLength(2);
    expect(settled()[1]).toMatchObject({ files: 2, stored: 2, failed: 0, cancelled: 0 });

    // Nothing failed any more: asking again sends nothing and says nothing.
    engine.retryFailed();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(3);
    expect(notices).toHaveLength(2);
    expect(settled()).toHaveLength(2);
  });

  it('forgets what is over while other files are on their way, and those go on', async () => {
    fakeApi({ 'PUT /buckets/photos/object': stores });
    const requests = hold();
    const { engine, notices } = uploadsOf();

    engine.start('photos', [
      { file: file('a.txt'), key: 'a.txt' },
      { file: file('b.txt'), key: 'b.txt' },
    ]);
    await until(() => requests.held().length === 2, 'both requests');
    requests.release('photos/a.txt');
    await until(() => statesOf(engine)[0] === 'stored', 'the first file to be stored');
    expect(statesOf(engine)).toEqual(['stored', 'sending']);
    // A file of the batch is on its way: nothing is said of the batch yet.
    expect(notices).toEqual([]);

    engine.dismissFinished();
    expect(engine.getState().uploads.map((upload) => upload.key)).toEqual(['b.txt']);
    expect(statesOf(engine)).toEqual(['sending']);
    expect(engine.busy()).toBe(true);

    requests.release('photos/b.txt');
    await until(() => notices.length === 1, 'the notice of the batch');
    expect(statesOf(engine)).toEqual(['stored']);
    expect(notices[0]?.kind).toBe('success');
    expect(engine.busy()).toBe(false);
  });

  it('sends no more files at once than the limit, counted over two buckets', async () => {
    const api = fakeApi({ 'PUT /buckets/photos/object': stores, 'PUT /buckets/films/object': stores });
    const requests = hold();
    const { engine, notices } = uploadsOf();
    const three = (prefix: string) => [1, 2, 3].map((n) => ({ file: file(`${prefix}${String(n)}.txt`), key: `${prefix}${String(n)}.txt` }));

    engine.start('photos', three('p'));
    engine.start('films', three('f'));
    // Three and three: four are being sent, two wait their turn.
    expect(api.xhrs).toHaveLength(UPLOADS_AT_ONCE);
    expect(statesOf(engine)).toEqual(['sending', 'sending', 'sending', 'sending', 'waiting', 'waiting']);
    await until(() => requests.held().length === UPLOADS_AT_ONCE, 'four requests');
    expect(requests.held().sort()).toEqual(['films/f1.txt', 'photos/p1.txt', 'photos/p2.txt', 'photos/p3.txt']);

    // A place that comes free in one bucket goes to the file that waited longest, of the other.
    requests.release('photos/p1.txt');
    await until(() => api.xhrs.length === 5, 'the fifth request');
    await until(() => requests.held().length === UPLOADS_AT_ONCE, 'four requests again');
    expect(requests.held()).toContain('films/f2.txt');
    expect(statesOf(engine)).toEqual(['stored', 'sending', 'sending', 'sending', 'sending', 'waiting']);

    requests.release('photos/p2.txt', 'photos/p3.txt', 'films/f1.txt', 'films/f2.txt');
    await until(() => requests.held().length === 1, 'the last request');
    expect(requests.held()).toEqual(['films/f3.txt']);
    requests.release('films/f3.txt');
    await until(() => notices.length === 2, 'a notice a batch');
    expect(api.xhrs).toHaveLength(6);
    expect(notices.map((notice) => notice.title)).toEqual(['Stored 3 objects in photos', 'Stored 3 objects in films']);
  });

  it('asks again with the same key while the API is still working on the first request, and takes its answer', async () => {
    // The answer of the first request is lost. The API is still committing
    // it when the console asks again, twice; then it gives the answer it kept.
    const api = fakeApi({
      'PUT /buckets/photos/object': inTurn(connectionFails, inProgress, inProgress, (call) => replayed(stores(call))),
    });
    const { engine, notices, settled } = uploadsOf();

    engine.start('photos', [{ file: file('a.txt'), key: 'a.txt' }]);
    await paused();
    // A failed connection: the next attempt is soon.
    await vi.advanceTimersByTimeAsync(UPLOAD_RETRY_MS - 1);
    expect(api.xhrs).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    await until(() => api.xhrs.length === 2, 'the second attempt');

    // "Still working": the same upload is asked after again, and given longer.
    await paused();
    expect(statesOf(engine)).toEqual(['sending']);
    await vi.advanceTimersByTimeAsync(busyWait(1) - 1);
    expect(api.xhrs).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    await until(() => api.xhrs.length === 3, 'the third attempt');

    await paused();
    await vi.advanceTimersByTimeAsync(busyWait(2) - 1);
    expect(api.xhrs).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(1);
    await until(() => api.xhrs.length === 4, 'the fourth attempt');

    await until(() => statesOf(engine)[0] === 'stored', 'the file to be stored');
    // One upload to the API, whatever was asked how often: one key.
    const keys = keysOf(api);
    expect(keys).toHaveLength(4);
    expect(new Set(keys).size).toBe(1);
    expect(keys[0]).toBeTruthy();
    // One file, one word of how its batch ended.
    expect(engine.getState().uploads).toHaveLength(1);
    expect(notices).toHaveLength(1);
    expect(notices[0]).toMatchObject({ kind: 'success', title: 'Stored 1 object in photos' });
    expect(settled()).toHaveLength(1);
  });

  it('does not say "not stored" of a file the API is still working on when it stops asking, and sends it again as the same upload', async () => {
    let working = true;
    const api = fakeApi({
      'PUT /buckets/photos/object': (call) => (working ? inProgress() : replayed(stores(call))),
    });
    const { engine, notices, settled } = uploadsOf();

    engine.start('photos', [{ file: file('a.txt'), key: 'a.txt' }]);
    for (let wait = 1; wait <= UPLOAD_BUSY_WAITS; wait += 1) {
      await paused();
      expect(statesOf(engine)).toEqual(['sending']);
      expect(api.xhrs).toHaveLength(wait);
      await vi.advanceTimersByTimeAsync(busyWait(wait));
      await until(() => api.xhrs.length === wait + 1, 'the next attempt');
    }
    await until(() => statesOf(engine)[0] === 'failed', 'the console to stop asking');

    // It is over for now, and can be sent again - but nothing says that it is not stored.
    const [upload] = engine.getState().uploads;
    expect(upload).toMatchObject({ state: 'failed', undecided: true, reason: STILL_WORKING });
    expect(STILL_WORKING).toContain('The server is still working on this file');
    expect(STILL_WORKING).not.toMatch(/not stored/i);
    expect(settled()).toHaveLength(1);
    expect(notices).toHaveLength(1);
    expect(notices[0]).toMatchObject({ kind: 'info', title: '1 of 1 object is not confirmed as stored in photos' });
    expect(notices[0]?.title).not.toMatch(/not stored/i);
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(UPLOAD_BUSY_WAITS + 1);

    // "Retry failed": the same upload still, so that the file the first
    // request stored is not stored a second time.
    working = false;
    engine.retryFailed();
    await until(() => statesOf(engine)[0] === 'stored', 'the file to be stored');
    const keys = keysOf(api);
    expect(keys).toHaveLength(UPLOAD_BUSY_WAITS + 2);
    expect(new Set(keys).size).toBe(1);
    expect(engine.getState().uploads[0]?.undecided).toBeUndefined();
    expect(engine.getState().uploads[0]?.reason).toBeUndefined();
    expect(notices).toHaveLength(2);
    expect(notices[1]).toMatchObject({ kind: 'success', title: 'Stored 1 object in photos' });
  });

  it('sends a file once more as a new upload when a repeat is answered with a refusal the API had kept', async () => {
    // The first request was cut short: the API refused it, and keeps that
    // refusal under its key. The repeat, though whole, is given it again.
    const cutShort = () => problem(400, 'Incomplete body');
    const api = fakeApi({
      'PUT /buckets/photos/object': inTurn(connectionFails, () => replayed(cutShort()), stores),
    });
    const { engine, notices } = uploadsOf();

    engine.start('photos', [{ file: file('a.txt'), key: 'a.txt' }]);
    await paused();
    await vi.advanceTimersByTimeAsync(UPLOAD_RETRY_MS);
    await until(() => statesOf(engine)[0] === 'stored', 'the file to be stored');

    const keys = keysOf(api);
    expect(keys).toHaveLength(3);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).toBeTruthy();
    expect(keys[2]).not.toBe(keys[0]);
    expect(notices).toHaveLength(1);
    expect(notices[0]?.kind).toBe('success');
  });

  it('draws a new key for a kept refusal once, and takes a refusal that was made of the request as it is', async () => {
    const cutShort = () => problem(400, 'Incomplete body');
    const api = fakeApi({
      // The new upload meets a failed connection too, and its repeat a kept refusal again.
      'PUT /buckets/photos/object': inTurn(connectionFails, () => replayed(cutShort()), connectionFails, () => replayed(cutShort())),
      // A refusal the API made now, of the first request: it is the answer.
      'PUT /buckets/films/object': () => problem(403, 'Access denied'),
    });
    const { engine, notices } = uploadsOf();

    engine.start('photos', [{ file: file('a.txt'), key: 'a.txt' }]);
    await paused();
    await vi.advanceTimersByTimeAsync(UPLOAD_RETRY_MS);
    await paused();
    await vi.advanceTimersByTimeAsync(2 * UPLOAD_RETRY_MS);
    await until(() => statesOf(engine)[0] === 'failed', 'the file to fail');

    const keys = keysOf(api);
    expect(keys).toHaveLength(4);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[0]);
    expect(keys[3]).toBe(keys[2]);
    expect(engine.getState().uploads[0]).toMatchObject({ state: 'failed', reason: 'Incomplete body' });
    expect(engine.getState().uploads[0]?.undecided).toBeUndefined();
    expect(notices[0]).toMatchObject({ kind: 'error', title: '1 of 1 object was not stored in photos' });

    engine.start('films', [{ file: file('b.txt'), key: 'b.txt' }]);
    await until(() => statesOf(engine)[1] === 'failed', 'the second file to fail');
    expect(api.callsTo('PUT', '/buckets/films/object')).toHaveLength(1);
  });
});
