import { File as NodeFile } from 'node:buffer';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { UNLOCK_TIMEOUT_MS } from './api/client';
import { App } from './App';
import { DeletionsProvider } from './deletions/DeletionsProvider';
import { DELETIONS_AT_ONCE, LISTING_PAGE } from './deletions/engine';
import { NoticesProvider } from './notices/NoticesProvider';
import { BEGAN_MEANWHILE, DeleteAll } from './screens/DeleteAll';
import { ObjectsScreen } from './screens/ObjectsScreen';
import { ActivityBar } from './ui/ActivityBar';
import { UploadsProvider } from './uploads/UploadsProvider';
import {
  CSRF_TOKEN,
  json,
  noContent,
  problem,
  refusal,
  SERVER,
  signedIn,
  tooManyAttempts,
  wrongPassword,
  type FakeApi,
  type Handler,
} from './test/fakeApi';

// A bucket's details on its page, and "Delete all" behind the password: the
// password judged by the server before anything is deleted, the deletions
// several at once and never more than DELETIONS_AT_ONCE, every page of the
// listing walked, refusals counted with their reasons, a cancel that stops
// what has not begun, and a deletion that goes on when another screen is
// shown. "Delete selected" goes through the same queue.

const CREATED = '2026-10-01T00:00:00Z';
const PASSWORD = 'correct horse battery staple';
const NEW_CSRF = 'csrf-after-the-password';

/** The keys k0001, k0002 … up to `count`, in the order the API lists them. */
function keysOf(count: number, from = 1): string[] {
  return Array.from({ length: count }, (_, i) => `k${String(from + i).padStart(4, '0')}`);
}

interface UsageAnswer {
  object_count: number;
  total_size: number;
  complete: boolean;
  exact: boolean;
  versions?: number;
  stored_bytes?: number;
  upload_bytes?: number;
}

/** A usage the metadata store keeps: exact. */
function exactUsage(count: number, size = count * 10): UsageAnswer {
  return { object_count: count, total_size: size, complete: true, exact: true, versions: count, stored_bytes: size, upload_bytes: 0 };
}

/**
 * A bucket the fake API keeps: it lists its keys in name order, a page of
 * `limit` at a time after `continuation_token` - as the API does - and
 * deletes them; a key in `locked` is refused as Object Lock refuses it.
 */
interface FakeBucket {
  name: string;
  held: Set<string>;
  locked: Set<string>;
  routes: Record<string, Handler>;
}

function fakeBucket(name: string, keys: string[], usage: () => Response = () => json(200, exactUsage(keys.length))): FakeBucket {
  const bucket: FakeBucket = {
    name,
    held: new Set(keys),
    locked: new Set(),
    routes: {
      [`GET /buckets/${name}/objects`]: (call) => {
        const limit = Number(call.query.get('limit') ?? '200');
        const after = call.query.get('continuation_token') ?? '';
        const rest = [...bucket.held].sort().filter((key) => key > after);
        const page = rest.slice(0, limit);
        const truncated = rest.length > limit;
        return json(200, {
          objects: page.map((key) => ({ key, size: 10, etag: '"e"', last_modified: CREATED })),
          common_prefixes: [],
          truncated,
          access_included: false,
          next_continuation_token: truncated ? page.at(-1) : undefined,
        });
      },
      [`DELETE /buckets/${name}/object`]: (call) => {
        const key = call.query.get('key') ?? '';
        if (bucket.locked.has(key)) {
          return problem(403, 'Access denied', 'The object is under Object Lock.');
        }
        bucket.held.delete(key);
        return noContent();
      },
      [`GET /buckets/${name}/usage`]: usage,
    },
  };
  return bucket;
}

/** The list of buckets, with the details of each as the API tells them. */
function bucketList(...names: string[]): Record<string, Handler> {
  return {
    'GET /buckets': () =>
      json(
        200,
        names.map((name) => ({
          name,
          created_at: CREATED,
          details: { access_included: false, policy_included: false },
        })),
      ),
  };
}

/**
 * The server's check of the password: right, it gives the session a new CSRF
 * token - and `accepted` is told, the moment the server has done so.
 */
function passwordCheck(answer?: () => Response, accepted?: () => void): Record<string, Handler> {
  return {
    'POST /session/unlock': (call) => {
      if (answer !== undefined) {
        return answer();
      }
      const body = JSON.parse(call.body) as { password?: unknown };
      if (body.password !== PASSWORD) {
        return wrongPassword();
      }
      accepted?.();
      return json(200, { user: 'alice', csrf_token: NEW_CSRF, expires_at: '2026-10-15T12:00:00Z', server: SERVER });
    },
  };
}

/**
 * The tokens of the session as the server holds them: the CSRF token of
 * signedIn until the password is accepted, NEW_CSRF from then on. A request
 * with the token of before is answered 401, as the API answers the token it
 * replaced.
 */
function tokens() {
  let current = CSRF_TOKEN;
  return {
    accepted: () => {
      current = NEW_CSRF;
    },
    guard:
      (handler: Handler): Handler =>
      (call) =>
        call.headers.get('X-CSRF-Token') === current ? handler(call) : refusal(401, 'InvalidCredentials', 'Not signed in'),
  };
}

/**
 * Holds the next check of the password before it reaches the API, for good:
 * it is never answered, and fails as fetch fails when the console aborts it.
 */
function holdUnlockForever() {
  let reached = false;
  const answer = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (!reached && new URL(request.url).pathname.endsWith('/session/unlock')) {
      reached = true;
      return new Promise<Response>((_, reject) => {
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
  return { reached: () => reached };
}

/**
 * Holds the answer to the next check of the password on its way back: the
 * server has carried it out, the console does not know yet. The checks
 * after it pass.
 */
function holdPasswordAnswer() {
  let first = true;
  let letGo: () => void = () => undefined;
  let reached = false;
  const answer = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (first && new URL(request.url).pathname.endsWith('/session/unlock')) {
      first = false;
      const response = await answer(request);
      reached = true;
      await new Promise<void>((resolve) => {
        letGo = resolve;
      });
      return response;
    }
    return answer(request);
  });
  return {
    /** Whether the server has carried the check out. */
    reached: () => reached,
    release: () => {
      letGo();
    },
  };
}

/** The deletions sent, whatever became of them, in the order they were sent. */
function deletesOf(api: FakeApi, bucket = 'photos'): string[] {
  return api.callsTo('DELETE', `/buckets/${bucket}/object`).map((call) => call.query.get('key') ?? '');
}

/** The pages of the listing "Delete all" walked: the ones asked for with the size of its pages. */
function walkedPages(api: FakeApi, bucket = 'photos', page = LISTING_PAGE) {
  return api.callsTo('GET', `/buckets/${bucket}/objects`).filter((call) => call.query.get('limit') === String(page));
}

/** A page of the walk small enough that a few objects make several pages. */
const SMALL_PAGE = 5;

/**
 * The page of a bucket as the console shows it - beneath its notices, its
 * uploads and its deletions, with the activity bar above it - but with the
 * walk of "Delete all" asking for pages of SMALL_PAGE: walking several pages
 * then takes a dozen objects, not thousands. That the console asks for pages
 * of LISTING_PAGE is shown in deletions/engine.test.ts.
 */
async function openBucketWithSmallPages(bucket = 'photos') {
  render(
    <NoticesProvider>
      <UploadsProvider>
        <DeletionsProvider listingPage={SMALL_PAGE}>
          <ActivityBar />
          <ObjectsScreen bucket={bucket} prefix="" passwordSession />
        </DeletionsProvider>
      </UploadsProvider>
    </NoticesProvider>,
  );
  await screen.findByRole('heading', { name: bucket, level: 1 });
}

/**
 * Holds every DELETE on its way to the fake API until the test lets it go,
 * and counts how many are on their way at once - in the browser, held or
 * not. A request that is held has not reached the API.
 */
function holdDeletes() {
  const waiting: (() => void)[] = [];
  let open = false;
  let flying = 0;
  let most = 0;
  const answer = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method !== 'DELETE' || !new URL(request.url).pathname.endsWith('/object')) {
      return answer(request);
    }
    flying += 1;
    most = Math.max(most, flying);
    try {
      if (!open) {
        await new Promise<void>((resolve) => {
          waiting.push(resolve);
        });
      }
      return await answer(request);
    } finally {
      flying -= 1;
    }
  });
  return {
    /** How many are held now. */
    held: () => waiting.length,
    /** The most that were on their way at once. */
    most: () => most,
    /** Lets the `n` that waited longest go. */
    release: (n: number) => {
      for (const resolve of waiting.splice(0, n)) {
        resolve();
      }
    },
    /** Lets every one go, and whatever comes from now on. */
    open: () => {
      open = true;
      for (const resolve of waiting.splice(0)) {
        resolve();
      }
    },
  };
}

/** Lets the promises and timers of the moment run, by the real clock. */
function settle(ms = 30): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

/** The bar. */
function activity() {
  return within(screen.getByRole('region', { name: 'Activity' }));
}

/** The line of the bar, as it is shown. */
function summary(): string {
  return document.querySelector('.activitybar-text')?.textContent ?? '';
}

/** Opens the notices of the bar, unless they are open. */
function openNotices() {
  if (activity().queryByRole('group', { name: 'Notices' }) === null) {
    fireEvent.click(activity().getByRole('button', { name: /^Notices: / }));
  }
  return within(activity().getByRole('group', { name: 'Notices' }));
}

/** The titles of the notices, newest first: the notices are opened to read them. */
function noticeTitles(): string[] {
  openNotices();
  return Array.from(document.querySelectorAll('.activitybar-notice-title')).map((title) => title.textContent);
}

/** The details of the bucket, as a name and its value each. */
function facts(): Record<string, string> {
  const region = screen.getByRole('region', { name: 'Bucket details' });
  const out: Record<string, string> = {};
  for (const term of within(region).getAllByRole('term')) {
    out[term.textContent] = term.nextElementSibling?.textContent ?? '';
  }
  return out;
}

/** Opens the question of "Delete all". */
async function askDeleteAll() {
  fireEvent.click(await screen.findByRole('button', { name: 'Delete all' }));
  return screen.getByRole('dialog', { name: /^Delete every object of / });
}

/** Types the password into the question and sends it. */
function deleteAllWith(password: string) {
  const dialog = screen.getByRole('dialog', { name: /^Delete every object of / });
  fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
  fireEvent.submit(within(dialog).getByRole('button', { name: 'Delete every object' }).closest('form') as HTMLFormElement);
}

async function openBucket(bucket = 'photos') {
  window.location.hash = `#/buckets/${bucket}`;
  render(<App />);
  await screen.findByRole('heading', { name: bucket, level: 1 });
}

describe("a bucket's details on its page", () => {
  it('shows what the API tells of the bucket, in the units of the bucket grid', async () => {
    signedIn({
      'GET /buckets': () =>
        json(200, [
          {
            name: 'photos',
            created_at: CREATED,
            details: {
              access_included: true,
              created_by: 'user/alice',
              activity: {
                uploads: [{ at: '2026-10-14T10:00:00Z', by: 'user/bob', key: 'a.txt' }],
                downloads: [{ at: '2026-10-14T11:00:00Z', by: 'user/carol', key: 'b.txt', presigned: true }],
              },
              policy_included: true,
              policy: { statements: 2, open_to_anyone: false },
            },
          },
        ]),
      ...fakeBucket('photos', ['a.txt'], () =>
        json(200, {
          object_count: 1234,
          total_size: 5_000_000_000,
          complete: true,
          exact: true,
          versions: 1500,
          stored_bytes: 6_000_000_000,
          upload_bytes: 250_000_000,
        }),
      ).routes,
    });
    await openBucket();

    await waitFor(() => {
      expect(facts().Objects).toBe('1,234');
    });
    const shown = facts();
    expect(shown['Storage used']).toBe('5.00 GB');
    expect(shown.Versions).toBe('1,500 current and older, delete markers among them');
    expect(shown['All versions']).toBe('6.00 GB');
    expect(shown['Uploads in progress']).toBe('0.25 GB in parts of multipart uploads not completed');
    expect(shown.Created).toBe(new Date(CREATED).toLocaleString());
    expect(shown['Created by']).toBe('user/alice');
    expect(shown.Policy).toBe('2 statements');
    expect(shown['Last upload']).toContain('user/bob');
    expect(shown['Last upload']).toContain('a.txt');
    expect(shown['Last download']).toContain('user/carol');
    expect(shown['Last download']).toContain('presigned URL');
    // An exact count says nothing of being otherwise.
    expect(screen.queryByText(/not exact/)).toBeNull();
  });

  it('says "at least" and "not exact" of a count made for the request, never an exact figure', async () => {
    signedIn({
      ...bucketList('photos'),
      ...fakeBucket('photos', ['a.txt'], () =>
        json(200, { object_count: 100000, total_size: 2_500_000_000, complete: false, exact: false }),
      ).routes,
    });
    await openBucket();

    await waitFor(() => {
      expect(facts().Objects).toBe('≥ at least 100,000 (not exact)');
    });
    expect(facts()['Storage used']).toBe('≥ at least 2.50 GB (not exact)');
    // What only an exact count tells is not shown at all.
    expect(facts().Versions).toBeUndefined();
    expect(facts()['Uploads in progress']).toBeUndefined();
    // Neither who made it nor its policy: the list did not include them for this caller.
    expect(facts()['Created by']).toBeUndefined();
    expect(facts().Policy).toBeUndefined();
    expect(screen.getByText(/they are not exact, and the count stopped at its bound/)).toBeDefined();
  });

  it('says "not exact" of a count from the index that reached the end', async () => {
    signedIn({
      ...bucketList('photos'),
      ...fakeBucket('photos', ['a.txt'], () => json(200, { object_count: 42, total_size: 420, complete: true, exact: false })).routes,
    });
    await openBucket();

    await waitFor(() => {
      expect(facts().Objects).toBe('42 (not exact)');
    });
    expect(facts()['Storage used']).toBe('< 0.01 GB (not exact)');
  });

  it('says a figure the caller may not see is not visible, never 0', async () => {
    signedIn({
      'GET /buckets': () => problem(403, 'Access denied'),
      ...fakeBucket('photos', ['a.txt'], () => problem(403, 'Access denied')).routes,
    });
    await openBucket();

    await waitFor(() => {
      expect(facts().Objects).toBe('not visible to you');
    });
    expect(facts()['Storage used']).toBe('not visible to you');
    expect(facts().Created).toBe('not visible to you');
    const region = screen.getByRole('region', { name: 'Bucket details' });
    expect(region.textContent).not.toMatch(/\b0\b/);
    // A refusal of a part of the page is no failure of the page.
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('is read again when an upload into the bucket is over, and when a deletion of its objects is', async () => {
    const photos = fakeBucket('photos', ['a.txt', 'b.txt']);
    const api = signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      'PUT /buckets/photos/object': (call) =>
        json(200, {
          bucket: 'photos',
          key: call.query.get('key'),
          size: 1,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        }),
    });
    await openBucket();
    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/usage')).toHaveLength(1);
    });
    expect(api.callsTo('GET', '/buckets')).toHaveLength(1);

    const file = new NodeFile(['x'], 'c.txt', { type: 'text/plain' });
    fireEvent.change(screen.getByLabelText('File'), { target: { files: [file] } });
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);
    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/usage')).toHaveLength(2);
    });
    expect(api.callsTo('GET', '/buckets')).toHaveLength(2);

    fireEvent.click(await screen.findByLabelText('Select a.txt'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/usage')).toHaveLength(3);
    });
    expect(api.callsTo('GET', '/buckets')).toHaveLength(3);
  });
});

describe('"Delete all"', () => {
  it('is not there for an empty bucket', async () => {
    signedIn({ ...bucketList('empty'), ...fakeBucket('empty', []).routes });
    await openBucket('empty');
    await screen.findByText('There is nothing here.');
    await waitFor(() => {
      expect(facts().Objects).toBe('0');
    });
    expect(screen.queryByRole('button', { name: 'Delete all' })).toBeNull();
  });

  it('is not offered to an API key', async () => {
    signedIn({
      'GET /session': () => json(200, { kind: 'user', via: 'api_key', access_key_id: 'AKEXAMPLE', server: SERVER }),
      ...bucketList('photos'),
      ...fakeBucket('photos', ['a.txt']).routes,
    });
    await openBucket();
    await screen.findByLabelText('Select a.txt');
    await waitFor(() => {
      expect(facts().Objects).toBe('1');
    });
    expect(screen.getByRole('button', { name: 'Delete selected' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Delete all' })).toBeNull();
  });

  it('says what will happen, in words that cannot be mistaken, and asks for the password and nothing else', async () => {
    signedIn({
      ...bucketList('photos'),
      ...fakeBucket('photos', ['a.txt'], () =>
        json(200, { object_count: 5000, total_size: 50000, complete: false, exact: false }),
      ).routes,
    });
    await openBucket();
    await waitFor(() => {
      expect(facts().Objects).toContain('5,000');
    });

    const dialog = await askDeleteAll();
    const text = dialog.textContent;
    expect(text).toContain('Every object of the bucket photos is deleted');
    expect(text).toContain('not only those on this page or under this folder');
    expect(text).toContain('It holds at least 5,000 objects');
    expect(text).toContain('If the bucket keeps versions, its versions are kept: each object gets a delete marker');
    expect(text).toContain('Versions under Object Lock are not removed');
    // The password of whoever is signed in, and no name.
    const field = within(dialog).getByLabelText('Your password') as HTMLInputElement;
    expect(field.type).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('current-password');
    expect(within(dialog).queryAllByRole('textbox')).toHaveLength(0);
    expect(document.activeElement).toBe(field);

    // Cancel leaves the bucket as it is.
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('says how many from an exact count', async () => {
    signedIn({ ...bucketList('photos'), ...fakeBucket('photos', keysOf(3)).routes });
    await openBucket();
    await waitFor(() => {
      expect(facts().Objects).toBe('3');
    });
    expect((await askDeleteAll()).textContent).toContain('It holds 3 objects.');
  });

  it.each([
    // The first wrong password of the session: four more end it.
    [
      'a wrong password',
      () => wrongPassword(),
      'The password is wrong. Nothing was deleted. 4 more wrong passwords end your session.',
    ],
    ['too many attempts', () => tooManyAttempts(), 'Too many attempts - try again in a few minutes. Nothing was deleted.'],
  ])('deletes nothing at %s, and says so', async (_, answer, said) => {
    const photos = fakeBucket('photos', keysOf(5));
    const api = signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck(answer) });
    await openBucket();
    await askDeleteAll();
    deleteAllWith('not the password');

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe(said);
    await settle();
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(1);
    expect(deletesOf(api)).toEqual([]);
    expect(walkedPages(api)).toHaveLength(0);
    expect(photos.held.size).toBe(5);
    // The field is empty, and the password is nowhere in the page.
    const field = screen.getByLabelText('Your password') as HTMLInputElement;
    expect(field.value).toBe('');
    expect(document.body.innerHTML).not.toContain('not the password');
    expect(screen.getByRole('dialog', { name: /^Delete every object of / })).toBeDefined();
  });

  it('deletes nothing when the session has ended, and the console signs out', async () => {
    const photos = fakeBucket('photos', keysOf(5));
    const api = signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      ...passwordCheck(() => refusal(401, 'InvalidCredentials', 'Not signed in')),
    });
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    expect(await screen.findByRole('heading', { name: 'Sign in', level: 1 })).toBeDefined();
    await settle();
    expect(deletesOf(api)).toEqual([]);
    expect(walkedPages(api)).toHaveLength(0);
    expect(photos.held.size).toBe(5);
    expect(document.body.innerHTML).not.toContain(PASSWORD);
  });

  it('checks the password on the server before the first deletion, then walks every page and deletes every object', async () => {
    const keys = keysOf(SMALL_PAGE * 2 + 2);
    const photos = fakeBucket('photos', keys);
    const api = signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck() });
    // Counted, not held: how many are on their way at once.
    const deletes = holdDeletes();
    deletes.open();
    await openBucketWithSmallPages();
    await waitFor(() => {
      expect(facts().Objects).toBe('12');
    });
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 12 objects');
    });
    expect(photos.held.size).toBe(0);
    expect(deletes.most()).toBeGreaterThan(1);
    expect(deletes.most()).toBeLessThanOrEqual(DELETIONS_AT_ONCE);
    // The password went first, and once.
    const unlockAt = api.calls.findIndex((call) => call.method === 'POST' && call.path === '/session/unlock');
    const firstDelete = api.calls.findIndex((call) => call.method === 'DELETE');
    expect(unlockAt).toBeGreaterThanOrEqual(0);
    expect(unlockAt).toBeLessThan(firstDelete);
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(1);
    // Every deletion is one request of its own, sent with the tokens the server gave for the password.
    expect(new Set(deletesOf(api))).toEqual(new Set(keys));
    expect(deletesOf(api)).toHaveLength(keys.length);
    expect(api.callsTo('DELETE', '/buckets/photos/object').every((call) => call.headers.get('X-CSRF-Token') === NEW_CSRF)).toBe(true);
    // Three pages, each going on from the last key of the one before.
    const pages = walkedPages(api, 'photos', SMALL_PAGE);
    expect(pages.map((call) => call.query.get('continuation_token'))).toEqual([null, keys[4], keys[9]]);
    expect(pages.every((call) => call.query.get('delimiter') === null && call.query.get('prefix') === null)).toBe(true);
  });

  it('counts a refusal with its reason and goes on with the rest', async () => {
    const keys = keysOf(10);
    const photos = fakeBucket('photos', keys);
    photos.locked.add('k0003');
    photos.locked.add('k0007');
    const api = signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck() });
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    await waitFor(() => {
      expect(noticeTitles()).toContain('Not done: Deleted 8 of 10 objects from photos: 2 not deleted');
    });
    expect(deletesOf(api)).toHaveLength(10);
    expect([...photos.held]).toEqual(['k0003', 'k0007']);
    const notices = openNotices();
    expect(
      notices.getByText(/Not deleted: Access denied: The object is under Object Lock\. \(2 objects\)/),
    ).toBeDefined();
    // The screen says it as well, while it is shown.
    expect(screen.getAllByRole('alert').some((alert) => alert.textContent.includes('2 not deleted'))).toBe(true);
  });

  it(`never has more than ${String(DELETIONS_AT_ONCE)} deletions on their way, and says how far it is in the bar`, async () => {
    const photos = fakeBucket('photos', keysOf(30));
    const api = signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck() });
    const deletes = holdDeletes();
    await openBucket();
    await waitFor(() => {
      expect(facts().Objects).toBe('30');
    });
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });
    await settle(50);
    expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    expect(summary()).toBe('Deleting 0 of 30 from photos');
    // The keyboard went from the question to the progress of what it began.
    expect(document.activeElement).toBe(
      within(screen.getByRole('region', { name: 'Objects' })).getByRole('button', { name: 'Cancel deletion' }),
    );
    // Disabled while it runs.
    expect((screen.getByRole('button', { name: 'Delete all' }) as HTMLButtonElement).disabled).toBe(true);

    // Each that is answered makes room for one more, and for no more.
    deletes.release(3);
    await waitFor(() => {
      expect(summary()).toBe('Deleting 3 of 30 from photos');
    });
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });
    for (let round = 0; round < 10; round += 1) {
      deletes.release(2);
      await settle(5);
    }
    deletes.open();
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 30 objects');
    });
    expect(deletes.most()).toBe(DELETIONS_AT_ONCE);
    expect(deletesOf(api)).toHaveLength(30);
    expect(summary()).toBe('No uploads in progress');
  });

  it('cancels: what has not begun is not sent, what was on its way is counted', async () => {
    const photos = fakeBucket('photos', keysOf(20));
    const api = signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck() });
    const deletes = holdDeletes();
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });

    fireEvent.click(activity().getByRole('button', { name: 'Cancel deletion' }));
    deletes.open();
    await waitFor(() => {
      expect(noticeTitles()).toContain(
        `Note: The deletion from photos was cancelled: ${String(DELETIONS_AT_ONCE)} objects deleted, 12 not begun`,
      );
    });
    await settle();
    expect(deletesOf(api)).toHaveLength(DELETIONS_AT_ONCE);
    expect(photos.held.size).toBe(20 - DELETIONS_AT_ONCE);
  });

  it('goes on when another screen is opened, and says how it ended', async () => {
    const photos = fakeBucket('photos', keysOf(12));
    const api = signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      ...passwordCheck(),
      'GET /users': () => json(200, []),
    });
    const deletes = holdDeletes();
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });

    window.location.hash = '#/users';
    await screen.findByRole('heading', { name: 'Users', level: 1 });
    expect(summary()).toBe('Deleting 0 of 12 from photos');

    deletes.open();
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 12 objects');
    });
    expect(deletesOf(api)).toHaveLength(12);
    expect(photos.held.size).toBe(0);
    const notices = openNotices();
    expect(notices.getByRole('link', { name: 'Open photos' }).getAttribute('href')).toBe('#/buckets/photos');
  });

  it('asks before the page is left while objects are being deleted', async () => {
    const add = vi.spyOn(window, 'addEventListener');
    const asking = () => add.mock.calls.filter(([type]) => type === 'beforeunload');
    const photos = fakeBucket('photos', keysOf(3));
    signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck() });
    const deletes = holdDeletes();
    await openBucket();
    expect(asking()).toHaveLength(0);
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(asking()).toHaveLength(1);
    });
    const ask = asking()[0]?.[1];
    if (typeof ask !== 'function') {
      throw new Error('no beforeunload listener was registered');
    }
    const leaving = new Event('beforeunload', { cancelable: true });
    ask.call(window, leaving);
    expect(leaving.defaultPrevented).toBe(true);
    deletes.open();
  });

  it('stops when the session is locked, and deletes no more', async () => {
    const photos = fakeBucket('photos', keysOf(20));
    let locked = false;
    const api = signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      ...passwordCheck(),
      'DELETE /buckets/photos/object': (call) => {
        if (locked) {
          return refusal(423, 'SessionLocked', 'The session is locked');
        }
        photos.held.delete(call.query.get('key') ?? '');
        return noContent();
      },
    });
    const deletes = holdDeletes();
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });
    deletes.release(2);
    await waitFor(() => {
      expect(photos.held.size).toBe(18);
    });
    locked = true;
    // The console asks how the session stands, and is told it is locked.
    api.on({
      'GET /session': () => json(200, { kind: 'user', via: 'session', user: 'alice', server: SERVER, locked: true }),
    });
    deletes.open();
    await screen.findByRole('dialog', { name: 'Session locked' });
    await settle(50);
    // What was on its way when the lock came was answered; nothing was sent after.
    expect(deletesOf(api).length).toBeLessThanOrEqual(2 + DELETIONS_AT_ONCE);
    expect(photos.held.size).toBe(18);
  });
});

describe('"Delete selected"', () => {
  it(`goes through the same queue: never more than ${String(DELETIONS_AT_ONCE)} at once, with "Delete all" of another bucket`, async () => {
    const photos = fakeBucket('photos', keysOf(20));
    const docs = fakeBucket('docs', keysOf(6, 100));
    const api = signedIn({ ...bucketList('photos', 'docs'), ...photos.routes, ...docs.routes, ...passwordCheck() });
    const deletes = holdDeletes();
    await openBucket('photos');
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });

    // Another bucket: its ticked objects wait their turn behind those of photos.
    window.location.hash = '#/buckets/docs';
    await screen.findByRole('heading', { name: 'docs', level: 1 });
    fireEvent.click(await screen.findByLabelText('Select all on this page'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
    await settle(50);
    expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    expect(deletesOf(api, 'docs')).toEqual([]);
    expect(summary()).toBe('Deleting 0 of 20 from photos · Deleting 0 of 6 from docs');

    for (let round = 0; round < 20; round += 1) {
      deletes.release(1);
      await settle(5);
    }
    deletes.open();
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted 6 objects from docs');
    });
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 20 objects');
    });
    expect(deletes.most()).toBe(DELETIONS_AT_ONCE);
    expect(docs.held.size).toBe(0);
    expect(photos.held.size).toBe(0);
  });

  it(`sends at most ${String(DELETIONS_AT_ONCE)} at once of a page of ticked objects`, async () => {
    const photos = fakeBucket('photos', keysOf(15));
    const api = signedIn({ ...bucketList('photos'), ...photos.routes });
    const deletes = holdDeletes();
    await openBucket();
    fireEvent.click(await screen.findByLabelText('Select all on this page'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });
    await settle(50);
    expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    expect(summary()).toBe('Deleting 0 of 15 from photos');
    deletes.open();
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted 15 objects from photos');
    });
    expect(deletes.most()).toBe(DELETIONS_AT_ONCE);
    expect(deletesOf(api)).toHaveLength(15);
    // No password is asked for, nor checked.
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(0);
  });
});

describe('deleting, and what else goes on', () => {
  it('sends again, with the new tokens, a deletion of another job answered 401 because the password replaced its tokens', async () => {
    const photos = fakeBucket('photos', keysOf(4));
    const docs = fakeBucket('docs', keysOf(20, 100));
    const session = tokens();
    const api = signedIn({
      ...bucketList('photos', 'docs'),
      ...photos.routes,
      ...docs.routes,
      'DELETE /buckets/docs/object': session.guard(docs.routes['DELETE /buckets/docs/object'] as Handler),
      'DELETE /buckets/photos/object': session.guard(photos.routes['DELETE /buckets/photos/object'] as Handler),
      ...passwordCheck(undefined, session.accepted),
    });
    const deletes = holdDeletes();
    await openBucket('docs');
    fireEvent.click(await screen.findByLabelText('Select all on this page'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });

    // Those eight carry the tokens of before; the password replaces them.
    window.location.hash = '#/buckets/photos';
    await screen.findByRole('heading', { name: 'photos', level: 1 });
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(api.callsTo('POST', '/session/unlock')).toHaveLength(1);
    });
    await settle(50);
    deletes.open();

    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted 20 objects from docs');
    });
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 4 objects');
    });
    expect(docs.held.size).toBe(0);
    // Still signed in: a 401 to replaced tokens is not the end of the session.
    expect(screen.queryByRole('heading', { name: 'Sign in', level: 1 })).toBeNull();
    // The eight refused were sent once more, with the new token, and no others twice.
    const sent = api.callsTo('DELETE', '/buckets/docs/object');
    expect(sent).toHaveLength(20 + DELETIONS_AT_ONCE);
    expect(sent.filter((call) => call.headers.get('X-CSRF-Token') === CSRF_TOKEN)).toHaveLength(DELETIONS_AT_ONCE);
  });

  it('does the same when the 401 comes before the console has heard that the password was accepted', async () => {
    const photos = fakeBucket('photos', keysOf(2));
    const docs = fakeBucket('docs', keysOf(10, 100));
    const session = tokens();
    signedIn({
      ...bucketList('photos', 'docs'),
      ...photos.routes,
      ...docs.routes,
      'DELETE /buckets/docs/object': session.guard(docs.routes['DELETE /buckets/docs/object'] as Handler),
      'DELETE /buckets/photos/object': session.guard(photos.routes['DELETE /buckets/photos/object'] as Handler),
      ...passwordCheck(undefined, session.accepted),
    });
    const deletes = holdDeletes();
    const check = holdPasswordAnswer();
    await openBucket('docs');
    fireEvent.click(await screen.findByLabelText('Select all on this page'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });
    window.location.hash = '#/buckets/photos';
    await screen.findByRole('heading', { name: 'photos', level: 1 });
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(check.reached()).toBe(true);
    });

    // The server has new tokens; the console not yet. The eight are answered 401 now.
    deletes.open();
    await settle(50);
    expect(screen.queryByRole('heading', { name: 'Sign in', level: 1 })).toBeNull();
    check.release();

    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted 10 objects from docs');
    });
    expect(docs.held.size).toBe(0);
    expect(screen.queryByRole('heading', { name: 'Sign in', level: 1 })).toBeNull();
  });

  it('deletes nothing when the screen is left while the password is being checked', async () => {
    const photos = fakeBucket('photos', keysOf(5));
    const api = signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      ...passwordCheck(),
      'GET /users': () => json(200, []),
    });
    const check = holdPasswordAnswer();
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(check.reached()).toBe(true);
    });
    // Nothing else is deleted from here while it is checked.
    expect((screen.getByRole('button', { name: 'Delete selected' }) as HTMLButtonElement).disabled).toBe(true);

    window.location.hash = '#/users';
    await screen.findByRole('heading', { name: 'Users', level: 1 });
    check.release();
    await settle(100);
    expect(deletesOf(api)).toEqual([]);
    expect(walkedPages(api)).toHaveLength(0);
    expect(photos.held.size).toBe(5);
    expect(summary()).toBe('No uploads in progress');
  });

  it('deletes what was listed, and stops, when a later page of the listing cannot be read', async () => {
    const keys = keysOf(SMALL_PAGE + 3);
    const photos = fakeBucket('photos', keys);
    const list = photos.routes['GET /buckets/photos/objects'] as Handler;
    signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      'GET /buckets/photos/objects': (call) =>
        call.query.get('continuation_token') === null ? list(call) : problem(503, 'No metadata leader'),
      ...passwordCheck(),
    });
    await openBucketWithSmallPages();
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    await waitFor(() => {
      expect(noticeTitles()).toContain('Not done: Deleted 5 objects from photos, and stopped');
    });
    expect(photos.held.size).toBe(3);
    expect(openNotices().getByText(/The objects could not be listed: No metadata leader\./)).toBeDefined();
    expect(noticeTitles().some((title) => title.includes('every object'))).toBe(false);
  });

  it.each([
    ['says there is more and not from where', undefined],
    ['says to go on from where it began', 'same'],
  ])('stops, never "every object", when a page %s', async (_, token) => {
    const photos = fakeBucket('photos', keysOf(5));
    signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      'GET /buckets/photos/objects': (call) =>
        json(200, {
          objects: [...photos.held].sort().map((key) => ({ key, size: 10, etag: '"e"', last_modified: CREATED })),
          common_prefixes: [],
          truncated: true,
          access_included: false,
          // The token it was asked with: none, the first time.
          next_continuation_token: token === undefined ? undefined : (call.query.get('continuation_token') ?? undefined),
        }),
      ...passwordCheck(),
    });
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    await waitFor(() => {
      expect(noticeTitles()).toContain('Not done: Deleted 5 objects from photos, and stopped');
    });
    expect(openNotices().getByText(/The listing could not go on\./)).toBeDefined();
    expect(noticeTitles().some((title) => title.includes('every object'))).toBe(false);
  });

  it('sends no more once the session has ended partway through, and shows the sign-in screen', async () => {
    const photos = fakeBucket('photos', keysOf(20));
    let answered = 0;
    const del = photos.routes['DELETE /buckets/photos/object'] as Handler;
    const api = signedIn({
      ...bucketList('photos'),
      ...photos.routes,
      'DELETE /buckets/photos/object': (call) => {
        answered += 1;
        return answered <= 3 ? del(call) : refusal(401, 'InvalidCredentials', 'Not signed in');
      },
      ...passwordCheck(),
    });
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);

    expect(await screen.findByRole('heading', { name: 'Sign in', level: 1 })).toBeDefined();
    await settle(100);
    expect(photos.held.size).toBe(17);
    expect(deletesOf(api).length).toBeLessThan(20);
    // A 401 that is the end of the session is never sent again: every key went once at most.
    expect(new Set(deletesOf(api)).size).toBe(deletesOf(api).length);
    expect(answered).toBe(deletesOf(api).length);
  });

  it.each([
    ['is refused', false],
    ['is not answered in time', true],
  ])('signs out on a 401 that came while the password was checked, once the check %s', async (_, hangs) => {
    if (hangs) {
      vi.useFakeTimers({ shouldAdvanceTime: true });
    }
    try {
      const photos = fakeBucket('photos', keysOf(2));
      const docs = fakeBucket('docs', keysOf(1, 100));
      const api = signedIn({
        ...bucketList('photos', 'docs'),
        ...photos.routes,
        ...docs.routes,
        // The session has ended at the API: whatever is asked is answered 401.
        'DELETE /buckets/docs/object': () => refusal(401, 'InvalidCredentials', 'Not signed in'),
        ...passwordCheck(() => wrongPassword()),
      });
      const deletes = holdDeletes();
      // The check is held: on its way back (refused), or before it reaches the API (never answered).
      const check: { reached: () => boolean; release?: () => void } = hangs ? holdUnlockForever() : holdPasswordAnswer();
      await openBucket('docs');
      fireEvent.click(await screen.findByLabelText('Select all on this page'));
      fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
      fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
      await waitFor(() => {
        expect(deletes.held()).toBe(1);
      });
      window.location.hash = '#/buckets/photos';
      await screen.findByRole('heading', { name: 'photos', level: 1 });
      await askDeleteAll();
      deleteAllWith(PASSWORD);
      await waitFor(() => {
        expect(check.reached()).toBe(true);
      });

      // Answered 401 while the check is on its way: not judged yet.
      deletes.open();
      await settle(50);
      expect(screen.queryByRole('heading', { name: 'Sign in', level: 1 })).toBeNull();

      if (hangs) {
        await vi.advanceTimersByTimeAsync(UNLOCK_TIMEOUT_MS);
      } else {
        check.release?.();
      }
      expect(await screen.findByRole('heading', { name: 'Sign in', level: 1 })).toBeDefined();
      // Not sent again: it was the end of the session.
      expect(deletesOf(api, 'docs')).toHaveLength(1);
      expect(deletesOf(api)).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('sends again a page of the listing answered 401 because another password check replaced its tokens', async () => {
    const photos = fakeBucket('photos', keysOf(5));
    const docs = fakeBucket('docs', keysOf(3, 100));
    const list = photos.routes['GET /buckets/photos/objects'] as Handler;
    let refuseOnce = false;
    const api = signedIn({
      ...bucketList('photos', 'docs'),
      ...photos.routes,
      ...docs.routes,
      'GET /buckets/photos/objects': (call) => {
        if (refuseOnce && call.query.get('limit') === String(LISTING_PAGE)) {
          refuseOnce = false;
          return refusal(401, 'InvalidCredentials', 'Not signed in');
        }
        return list(call);
      },
      ...passwordCheck(),
    });
    // The first page "Delete all" of photos asks for is held on its way.
    let letGo: () => void = () => undefined;
    let heldPage = false;
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      const url = new URL(request.url);
      if (!heldPage && url.pathname.endsWith('/buckets/photos/objects') && url.searchParams.get('limit') === String(LISTING_PAGE)) {
        heldPage = true;
        await new Promise<void>((resolve) => {
          letGo = resolve;
        });
      }
      return answer(request);
    });
    await openBucket('photos');
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(heldPage).toBe(true);
    });

    // Another bucket, another check of the password: the page held went with the tokens of before.
    window.location.hash = '#/buckets/docs';
    await screen.findByRole('heading', { name: 'docs', level: 1 });
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of docs: 3 objects');
    });

    refuseOnce = true;
    letGo();
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 5 objects');
    });
    expect(walkedPages(api).map((call) => call.query.get('continuation_token'))).toEqual([null, null]);
    expect(photos.held.size).toBe(0);
    expect(screen.queryByRole('heading', { name: 'Sign in', level: 1 })).toBeNull();
  });

  it('asks before signing out while objects are being deleted', async () => {
    const photos = fakeBucket('photos', keysOf(10));
    const api = signedIn({ ...bucketList('photos'), ...photos.routes, ...passwordCheck() });
    const deletes = holdDeletes();
    await openBucket();
    await askDeleteAll();
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(deletes.held()).toBe(DELETIONS_AT_ONCE);
    });

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    const question = within(activity().getByRole('group', { name: 'Sign out and stop what is on its way?' }));
    expect(question.getByText('Objects are still being deleted. Sign out and stop the deletion?')).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);
    fireEvent.click(question.getByRole('button', { name: 'Stay signed in' }));
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);
    deletes.open();
    await waitFor(() => {
      expect(noticeTitles()).toContain('Done: Deleted every object of photos: 10 objects');
    });
  });

  it('begins nothing when a deletion of the bucket began while the password was checked, and says why', async () => {
    fakeApiForDialog();
    const check = holdPasswordAnswer();
    const confirmed = vi.fn();
    const usage = { data: undefined, error: null, loading: true, reload: () => undefined };
    const { rerender } = render(
      <DeletionsProvider>
        <DeleteAll bucket="photos" usage={usage} disabled={false} onConfirmed={confirmed} />
      </DeletionsProvider>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Delete all' }));
    deleteAllWith(PASSWORD);
    await waitFor(() => {
      expect(check.reached()).toBe(true);
    });
    rerender(
      <DeletionsProvider>
        <DeleteAll bucket="photos" usage={usage} disabled onConfirmed={confirmed} />
      </DeletionsProvider>,
    );
    check.release();
    expect((await screen.findByRole('alert')).textContent).toBe(BEGAN_MEANWHILE);
    expect(confirmed).not.toHaveBeenCalled();
  });
});

/** The fake API the dialog by itself needs: the check of the password. */
function fakeApiForDialog() {
  return signedIn(passwordCheck());
}
