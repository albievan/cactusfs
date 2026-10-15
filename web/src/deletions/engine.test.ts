import { afterEach, describe, expect, it, vi } from 'vitest';

import { unlock } from '../api/client';
import type { NoticeInput } from '../notices/state';
import { CSRF_TOKEN, fakeApi, json, noContent, refusal, SERVER } from '../test/fakeApi';
import { createDeletions, DELETIONS_AT_ONCE, LISTING_PAGE, REPLACED_TOKENS, type DeletionsEngine } from './engine';

// The deletions by themselves: no screen, no React. The requests are real
// promises through the client, as in the other tests.

/** Waits, by the real clock, until something is so. */
async function until(what: () => boolean, said = 'what was waited for'): Promise<void> {
  for (let turn = 0; turn < 1000; turn += 1) {
    if (what()) {
      return;
    }
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 1);
    });
  }
  throw new Error(`${said} never came`);
}

let engine: DeletionsEngine | null = null;
afterEach(() => {
  engine?.dispose();
  engine = null;
});

describe('the deletions engine', () => {
  it('sends a deletion once more, and no more, when the tokens it went with were replaced while it was on its way', async () => {
    window.sessionStorage.setItem('cactusfs.csrf', CSRF_TOKEN);
    // The server's tokens: each accepted password replaces them.
    let current = CSRF_TOKEN;
    let unlocks = 0;
    const api = fakeApi({
      'POST /session/unlock': () => {
        unlocks += 1;
        current = `csrf-${String(unlocks)}`;
        return json(200, { user: 'alice', csrf_token: current, expires_at: '2026-10-15T12:00:00Z', server: SERVER });
      },
      'DELETE /buckets/b/object': (call) =>
        call.headers.get('X-CSRF-Token') === current ? noContent() : refusal(401, 'InvalidCredentials', 'Not signed in'),
    });
    // Every DELETE is held on its way until the test lets it go.
    const waiting: (() => void)[] = [];
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (request.method === 'DELETE') {
        await new Promise<void>((resolve) => {
          waiting.push(resolve);
        });
      }
      return answer(request);
    });
    const notices: NoticeInput[] = [];
    engine = createDeletions((notice) => notices.push(notice));

    engine.deleteKeys('b', ['k']);
    await until(() => waiting.length === 1, 'the deletion');
    await unlock('the password');
    // Answered 401 to the tokens the password replaced: sent once more.
    waiting.shift()?.();
    await until(() => waiting.length === 1, 'the deletion sent again');
    await unlock('the password');
    // The same again: it is not sent a third time.
    waiting.shift()?.();
    await until(() => notices.length === 1, 'the outcome');

    expect(api.callsTo('DELETE', '/buckets/b/object')).toHaveLength(2);
    expect(waiting).toHaveLength(0);
    expect(notices[0]).toMatchObject({ kind: 'error', title: 'Deleted 0 of 1 object from b: 1 not deleted' });
    expect(notices[0]?.detail).toContain(REPLACED_TOKENS);
    // The session did not end, and nothing says it did.
    expect(notices[0]?.detail).not.toMatch(/session ended/i);
    expect(engine.getState().deletions[0]?.stopped).toBeUndefined();
  });

  it(`walks the listing in pages of ${String(LISTING_PAGE)} unless told otherwise, beyond the first, never more than ${String(DELETIONS_AT_ONCE)} at once`, async () => {
    window.sessionStorage.setItem('cactusfs.csrf', CSRF_TOKEN);
    const keys = Array.from({ length: LISTING_PAGE + 5 }, (_, i) => `k${String(i + 1).padStart(5, '0')}`);
    const held = new Set(keys);
    const api = fakeApi({
      'GET /buckets/b/objects': (call) => {
        const limit = Number(call.query.get('limit') ?? '200');
        const after = call.query.get('continuation_token') ?? '';
        const rest = [...held].sort().filter((key) => key > after);
        const page = rest.slice(0, limit);
        const truncated = rest.length > limit;
        return json(200, {
          objects: page.map((key) => ({ key, size: 1, etag: '"e"', last_modified: '2026-10-01T00:00:00Z' })),
          common_prefixes: [],
          truncated,
          access_included: false,
          next_continuation_token: truncated ? page.at(-1) : undefined,
        });
      },
      'DELETE /buckets/b/object': (call) => {
        held.delete(call.query.get('key') ?? '');
        return noContent();
      },
    });
    // How many DELETEs are on their way at once, in the browser.
    let flying = 0;
    let most = 0;
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (request.method !== 'DELETE') {
        return answer(request);
      }
      flying += 1;
      most = Math.max(most, flying);
      try {
        // A turn of the event loop on the way, as a network would take.
        await new Promise<void>((resolve) => {
          setTimeout(resolve, 0);
        });
        return await answer(request);
      } finally {
        flying -= 1;
      }
    });
    const notices: NoticeInput[] = [];
    engine = createDeletions((notice) => notices.push(notice));

    engine.deleteAll('b', { count: keys.length, atLeast: false });
    await until(() => notices.length === 1, 'the outcome');

    expect(notices[0]).toMatchObject({ kind: 'success', title: 'Deleted every object of b: 1 005 objects' });
    expect(held.size).toBe(0);
    expect(api.callsTo('DELETE', '/buckets/b/object')).toHaveLength(keys.length);
    const pages = api.callsTo('GET', '/buckets/b/objects');
    expect(pages.map((call) => call.query.get('limit'))).toEqual([String(LISTING_PAGE), String(LISTING_PAGE)]);
    expect(pages.map((call) => call.query.get('continuation_token'))).toEqual([null, keys[LISTING_PAGE - 1]]);
    expect(most).toBe(DELETIONS_AT_ONCE);
  });
});
