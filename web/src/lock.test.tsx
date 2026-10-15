import { File as NodeFile } from 'node:buffer';

import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type MockInstance } from 'vitest';

import { UNLOCK_ATTEMPTS, UNLOCK_TIMEOUT_MS } from './api/client';
import { App } from './App';
import { SESSION_ASK_AFTER_MS, SESSION_POLL_MS } from './session/timing';
import { UPLOAD_RETRY_MS } from './uploads/engine';
import {
  CSRF_TOKEN,
  fakeSession,
  json,
  problem,
  refusal,
  signedIn,
  tooManyAttempts,
  type FakeApi,
  type FakeSession,
  type Handler,
} from './test/fakeApi';

// A session that locks, not ends, while objects are being uploaded: the
// console shows the lock in place of its screens, keeps the uploads going,
// and gives the screens back for the user's password. And a session that
// ends - there was nothing to upload, the uploads are over, the password was
// wrong too often - is signed out, and told why.
//
// The clock is the test's, and goes on by itself as well: what the testing
// library waits for with timers of its own still comes. The times the
// console waits - until the timeout, between two questions of a locked
// session - are stepped over. A time is stepped over only once the console
// is known to wait for it: its timers are watched, so that no step is taken
// a moment before the timer it is meant for is set.

const CREATED = '2026-10-01T00:00:00Z';
const UNLOCK = 'correct horse battery staple';
/** How long the sessions of these tests have until their timeout. */
const LIFETIME_MS = 60_000;

let timers: MockInstance<typeof setTimeout>;

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  timers = vi.spyOn(globalThis, 'setTimeout');
});
afterEach(() => {
  // In this order: what is watched is the test's clock, and is let go of
  // before that clock is.
  timers.mockRestore();
  vi.useRealTimers();
});

/**
 * How often the console has set itself to ask how the session stands in a
 * while: what it does while the session is locked, and for a session whose
 * timeout has passed. Nothing else in the console waits exactly that long.
 */
function polls(): number {
  return timers.mock.calls.filter((call) => call[1] === SESSION_POLL_MS).length;
}

/**
 * How often the console has set itself to ask at a timeout the API named:
 * nothing else waits longer than a poll but an unlock, which waits for its
 * answer exactly UNLOCK_TIMEOUT_MS.
 */
function timeouts(): number {
  return timers.mock.calls.filter(
    (call) => typeof call[1] === 'number' && call[1] > SESSION_POLL_MS && call[1] !== UNLOCK_TIMEOUT_MS,
  ).length;
}

/**
 * Holds the next request that unlocks on its way to the fake API, for as
 * long as the test likes: it has not reached the API. One whose signal is
 * aborted meanwhile fails as fetch fails then. The unlocks after it pass.
 */
function holdUnlock() {
  const answer = globalThis.fetch;
  let first = true;
  let letGo: () => void = () => undefined;
  const signals: AbortSignal[] = [];
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (first && new URL(request.url).pathname.endsWith('/session/unlock')) {
      first = false;
      signals.push(request.signal);
      await new Promise<void>((resolve, reject) => {
        letGo = resolve;
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
    signals,
    release: () => {
      letGo();
    },
  };
}

/**
 * Holds the answer to the next unlock on its way back: the API has carried
 * it out - the session's tokens are replaced - and the console does not know
 * yet. The unlocks after it pass.
 */
function holdUnlockAnswer() {
  const answer = globalThis.fetch;
  let first = true;
  let reached = false;
  let letGo: () => void = () => undefined;
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
    /** Whether the API has carried the unlock out. */
    reached: () => reached,
    release: () => {
      letGo();
    },
  };
}

/** Waits until the console has set itself to ask this often, in all. */
async function untilSet(count: () => number, times: number) {
  await waitFor(() => {
    expect(count()).toBe(times);
  });
}

/** The moment this long from now, as the API writes one. */
function fromNow(milliseconds: number): string {
  return new Date(Date.now() + milliseconds).toISOString();
}

/** Lets this much time pass. */
async function pass(milliseconds: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(milliseconds);
  });
}

/**
 * The answers of the bucket `photos` for a session: it lists what it holds -
 * unless the session is locked - and stores what is put into it, locked or
 * not, but for `refused.txt`, which the user may not store.
 */
function photos(session: FakeSession): Record<string, Handler> {
  const held: string[] = [];
  return {
    'GET /buckets/photos/objects': session.guarded(() =>
      json(200, {
        objects: held.map((key) => ({ key, size: 10, etag: '"e"', last_modified: CREATED })),
        common_prefixes: [],
        truncated: false,
        access_included: false,
      }),
    ),
    'PUT /buckets/photos/object': session.storing((call) => {
      const key = call.query.get('key') ?? '';
      if (key === 'refused.txt') {
        return problem(403, 'Access denied');
      }
      held.push(key);
      return json(200, {
        bucket: 'photos',
        key,
        size: call.body.length,
        etag: '"e"',
        last_modified: CREATED,
        content_type: 'text/plain',
        encryption: 'SSE-S3',
      });
    }),
  };
}

/**
 * Holds every upload on its way to the fake API until the test lets it go,
 * by the bucket and the key it is for: `photos/a.txt`. The fake's
 * XMLHttpRequest answers through fetch, so holding fetch holds an upload -
 * which is then "being sent" for as long as the test likes. A request that
 * is held has not reached the API: it is not among its calls.
 */
function holdUploads() {
  const waiting = new Map<string, () => void>();
  const free = new Set<string>();
  const answer = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === 'PUT') {
      const url = new URL(request.url);
      const bucket = /\/buckets\/([^/]+)\//.exec(url.pathname)?.[1] ?? '';
      const name = `${bucket}/${url.searchParams.get('key') ?? ''}`;
      if (!free.has(name)) {
        await new Promise<void>((resolve) => {
          waiting.set(name, resolve);
        });
      }
    }
    return answer(request);
  });
  return {
    /** Lets these go, and whatever is asked for them from now on. */
    release: (...names: string[]) => {
      for (const name of names) {
        free.add(name);
        waiting.get(name)?.();
      }
    },
  };
}

function chooseAndUpload(names: string[]) {
  const files = names.map((name) => new NodeFile([`contents of ${name}`], name, { type: 'text/plain' }));
  fireEvent.change(screen.getByLabelText('File'), { target: { files } });
  // jsdom does not count a file set by a test as a value of a required
  // field, so submit the form itself rather than click its button.
  fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);
}

/** The lock, once it is shown and the console has set itself to ask how the session stands. */
async function lock() {
  const dialog = await screen.findByRole('dialog', { name: 'Session locked' });
  await waitFor(() => {
    expect(polls()).toBeGreaterThan(0);
  });
  return dialog;
}

/** What the tab keeps: every value of its sessionStorage and of its localStorage. */
function kept(): string[] {
  const values: string[] = [];
  for (const storage of [window.sessionStorage, window.localStorage]) {
    for (let index = 0; index < storage.length; index += 1) {
      values.push(storage.getItem(storage.key(index) ?? '') ?? '');
    }
  }
  return values;
}

function passwordField(): HTMLInputElement {
  return screen.getByLabelText('Password') as HTMLInputElement;
}

/** Types a password into the lock and sends it with the button. */
function unlockWith(password: string) {
  fireEvent.change(passwordField(), { target: { value: password } });
  fireEvent.click(screen.getByRole('button', { name: 'Unlock' }));
}

/** The line of the bar, as it is shown. */
function summary(): string {
  return document.querySelector('.activitybar-text')?.textContent ?? '';
}

/** How often the console asked the API how the session stands. */
function asked(api: FakeApi): number {
  return api.callsTo('GET', '/session').length;
}

/** The uploads of a key that reached the API. */
function stored(api: FakeApi, key: string) {
  return api.callsTo('PUT', '/buckets/photos/object').filter((call) => call.query.get('key') === key);
}

/** What the sign-in screen says of the session that ended, or null when it says nothing of one. */
function endedNotice(): string | null {
  return document.querySelector('.signin-ended')?.textContent ?? null;
}

interface Locked {
  api: FakeApi;
  session: FakeSession;
  uploads: ReturnType<typeof holdUploads>;
}

/**
 * A console on the screen of the bucket `photos` with these files being
 * uploaded - every one of them held - whose session the API has locked
 * meanwhile: the console went to the users, was answered 423, asked, and
 * shows the lock. The place in the console is `#/users`.
 */
async function lockedWhileUploading(names: string[]): Promise<Locked> {
  const session = fakeSession(fromNow(60 * LIFETIME_MS));
  const api = signedIn({
    ...session.routes,
    ...photos(session),
    'GET /users': session.guarded(() => json(200, [])),
  });
  const uploads = holdUploads();
  window.location.hash = '#/buckets/photos';
  render(<App />);
  await screen.findByText('There is nothing here.');

  chooseAndUpload(names);
  await waitFor(() => {
    // No more than four are sent at once: the others wait their turn.
    expect(api.xhrs).toHaveLength(Math.min(names.length, 4));
  });

  session.stands = 'locked';
  window.location.hash = '#/users';
  await lock();
  return { api, session, uploads };
}

describe('a session that reaches its timeout while objects are being uploaded', () => {
  it('shows the lock at the timeout the API named, once the API says that the session is locked', async () => {
    const session = fakeSession(fromNow(LIFETIME_MS));
    const api = signedIn({ ...session.routes, ...photos(session) });
    const uploads = holdUploads();
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    expect(screen.getByRole('heading', { name: 'photos', level: 1 })).toBeDefined();

    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(1);
    });
    expect(asked(api)).toBe(1);
    await untilSet(timeouts, 1);
    expect(polls()).toBe(0);

    // Before the timeout nothing is asked and nothing is locked.
    await pass(LIFETIME_MS / 2);
    expect(asked(api)).toBe(1);
    expect(screen.queryByRole('dialog')).toBeNull();

    // At the timeout the API locks the session: an upload is on its way.
    session.stands = 'locked';
    await pass(LIFETIME_MS / 2 + SESSION_ASK_AFTER_MS);

    const dialog = await lock();
    expect(asked(api)).toBe(2);
    expect(dialog.getAttribute('aria-modal')).toBe('true');
    expect(dialog.textContent).toContain('Your session timed out while objects were being uploaded.');
    expect(dialog.textContent).toContain('Type your password to go on.');
    expect(dialog.textContent).toContain('The uploads continue.');
    // Whose session it is is said, not asked for.
    expect(dialog.textContent).toContain('Signed in as alice');
    expect(document.querySelector('input[name="username"]')).toBeNull();
    expect(document.querySelectorAll('input')).toHaveLength(1);

    // The keyboard is in the password field, which is one.
    const field = passwordField();
    expect(field.getAttribute('type')).toBe('password');
    expect(field.getAttribute('name')).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('current-password');
    expect(document.activeElement).toBe(field);

    // Nothing of the screen that was shown is in the page, nor of the frame.
    expect(screen.queryByRole('heading', { name: 'photos' })).toBeNull();
    expect(screen.queryByText('There is nothing here.')).toBeNull();
    expect(screen.queryByLabelText('File')).toBeNull();
    expect(screen.queryByRole('navigation')).toBeNull();
    expect(screen.queryByRole('search')).toBeNull();
    expect(document.querySelector('.layout')).toBeNull();
    expect(document.querySelector('.screen')).toBeNull();
    expect(document.body.textContent).not.toContain('photos');

    // The upload was not given up, and the place in the console is kept.
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    expect(api.xhrs.map((xhr) => xhr.abortCalls)).toEqual([0]);
    expect(window.location.hash).toBe('#/buckets/photos');
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe(CSRF_TOKEN);

    // The upload ends while the console is locked, and is stored.
    uploads.release('photos/a.txt');
    await waitFor(() => {
      expect(summary()).toBe('1 of 1 stored');
    });
    expect(stored(api, 'a.txt')).toHaveLength(1);
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
  });

  it('shows the lock when a request is answered 423, and not the sign-in screen', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);

    // The 423 was not taken for a 401: the session and its upload are there.
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe(CSRF_TOKEN);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    // The API was asked, and it is its answer that locked the console.
    expect(api.callsTo('GET', '/users')).toHaveLength(1);
    expect(asked(api)).toBe(2);
    // Nothing of the screen that was refused, nor of the one before it.
    expect(screen.queryByRole('heading', { name: 'Users' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('navigation')).toBeNull();
    expect(document.body.textContent).not.toContain('The session is locked');
    expect(window.location.hash).toBe('#/users');
  });

  it('goes on with the files that wait their turn while it is locked, and aborts none', async () => {
    const { api, uploads } = await lockedWhileUploading(['f1.txt', 'f2.txt', 'f3.txt', 'f4.txt', 'f5.txt']);
    expect(api.xhrs).toHaveLength(4);
    expect(stored(api, 'f5.txt')).toHaveLength(0);

    // One is over: the fifth, which waited, is sent - locked as the console is.
    uploads.release('photos/f1.txt');
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(5);
    });
    expect(new URL(api.xhrs[4]?.url ?? '', 'http://localhost').searchParams.get('key')).toBe('f5.txt');
    uploads.release('photos/f5.txt');
    await waitFor(() => {
      expect(stored(api, 'f5.txt')).toHaveLength(1);
    });
    // It carried what an upload carries: the CSRF token of the session.
    expect(stored(api, 'f5.txt')[0]?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    await waitFor(() => {
      expect(summary()).toContain('2 stored');
    });

    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false, false, false, false, false]);
    expect(api.xhrs.map((xhr) => xhr.abortCalls)).toEqual([0, 0, 0, 0, 0]);
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    // Nothing but the uploads was asked of the API while it was locked.
    expect(api.callsTo('GET', '/users')).toHaveLength(1);
  });

  it('says how the uploads stand, with nothing of them to operate and no name of what is uploaded', async () => {
    const { uploads } = await lockedWhileUploading(['a.txt', 'refused.txt', 'c.txt']);
    // One of them is refused: outside the lock that would offer "Retry failed".
    uploads.release('photos/refused.txt');
    await waitFor(() => {
      expect(summary()).toContain('1 not stored');
    });

    const dialog = within(await lock());
    const bar = within(dialog.getByRole('region', { name: 'Activity' }));
    expect(summary()).toContain('Uploading 3 files');
    expect(summary()).toContain('0 stored');
    const whole = bar.getByRole('progressbar', { name: 'All uploads' }) as HTMLProgressElement;
    expect(whole.max).toBe(1);
    expect(whole.value).toBeLessThan(1);
    // It is still read out, politely.
    const live = document.querySelector('.activitybar-live');
    expect(live?.getAttribute('aria-live')).toBe('polite');
    expect(live?.textContent).toBe('Uploading 3 files. 1 not stored.');

    // The only buttons of the page are the two of the lock.
    expect(screen.getAllByRole('button').map((button) => button.textContent.trim())).toEqual(['Unlock', 'Sign out']);
    expect(bar.queryAllByRole('button')).toHaveLength(0);
    expect(bar.queryAllByRole('link')).toHaveLength(0);
    expect(screen.queryByRole('button', { name: /Cancel/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /Retry/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /Dismiss/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /^Uploads: / })).toBeNull();
    expect(screen.queryByRole('button', { name: /^Notices: / })).toBeNull();
    expect(screen.queryAllByRole('link')).toHaveLength(0);
    expect(screen.queryByLabelText('File')).toBeNull();
    // No bar of a single file, and nothing that names a bucket or an object.
    expect(screen.getAllByRole('progressbar')).toHaveLength(1);
    expect(document.querySelector('.upload')).toBeNull();
    expect(document.body.textContent).not.toContain('a.txt');
    expect(document.body.textContent).not.toContain('refused.txt');
    expect(document.body.textContent).not.toContain('photos');
    // Why the file was refused is not said here either.
    expect(document.body.textContent).not.toContain('Access denied');
  });

  it('unlocks with the password: the screen that was shown is back and what is sent next carries the new CSRF token', async () => {
    const { api, session, uploads } = await lockedWhileUploading(['f1.txt', 'f2.txt', 'f3.txt', 'f4.txt', 'f5.txt']);
    session.expiresAtOnceUnlocked = fromNow(LIFETIME_MS);
    // One timer was set for the timeout the session had when it was loaded.
    expect(timeouts()).toBe(1);

    unlockWith(UNLOCK);
    // While the password is on its way the button cannot be pressed again,
    // and the password is in the field no more.
    expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(true);
    expect(passwordField().value).toBe('');

    // The screen of the place the console was at, read anew.
    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(window.location.hash).toBe('#/users');
    expect(screen.getByRole('navigation', { name: 'Sections' })).toBeDefined();
    await waitFor(() => {
      expect(api.callsTo('GET', '/users')).toHaveLength(2);
    });

    // The password went to the API once, with the token the session had,
    // and without a user name; the token it answered with is the one kept.
    const unlocks = api.callsTo('POST', '/session/unlock');
    expect(unlocks).toHaveLength(1);
    expect(JSON.parse(unlocks[0]?.body ?? '')).toEqual({ password: UNLOCK });
    expect(unlocks[0]?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    expect(unlocks[0]?.headers.get('Content-Type')).toBe('application/json');
    expect(unlocks[0]?.credentials).toBe('same-origin');
    expect(session.csrfToken).toBe('csrf-after-unlock-1');
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe('csrf-after-unlock-1');
    // The password is nowhere in the page, and in no storage.
    expect(document.body.innerHTML).not.toContain(UNLOCK);
    expect(kept()).toEqual(['csrf-after-unlock-1']);

    // The uploads were never given up...
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false, false, false, false]);
    // ...and the file that waited is sent with the new token; the one sent
    // before the lock carried the one of before.
    uploads.release('photos/f1.txt');
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(5);
    });
    uploads.release('photos/f5.txt');
    await waitFor(() => {
      expect(stored(api, 'f5.txt')).toHaveLength(1);
    });
    expect(stored(api, 'f5.txt')[0]?.headers.get('X-CSRF-Token')).toBe('csrf-after-unlock-1');
    expect(stored(api, 'f1.txt')[0]?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);

    // The bar can be operated again.
    const bar = within(screen.getByRole('region', { name: 'Activity' }));
    expect(bar.getByRole('button', { name: 'Uploads: 5' })).toBeDefined();

    // The session has a new timeout, and the console asks at that one.
    await untilSet(timeouts, 2);
    const before = asked(api);
    await pass(LIFETIME_MS / 2);
    expect(asked(api)).toBe(before);
    await pass(LIFETIME_MS / 2 + SESSION_ASK_AFTER_MS);
    await waitFor(() => {
      expect(asked(api)).toBe(before + 1);
    });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
  });

  it('sends the password when the form is sent from the keyboard', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);

    // Enter in the field sends its form.
    const field = passwordField();
    fireEvent.change(field, { target: { value: UNLOCK } });
    fireEvent.submit(field.closest('form') as HTMLFormElement);

    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(1);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
  });

  it('sends nothing when no password was typed', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);

    fireEvent.submit(passwordField().closest('form') as HTMLFormElement);

    expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(false);
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(0);
  });

  it('says that the password is wrong, and stays locked with the uploads going', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);

    unlockWith('not the password');

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('The password is wrong');
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    // The field is empty and has the keyboard again; the button can be pressed.
    expect(passwordField().value).toBe('');
    expect(document.activeElement).toBe(passwordField());
    expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(false);
    // A 403 is not a session that ended: the token and the upload are there.
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe(CSRF_TOKEN);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    // What was typed is nowhere in the page.
    expect(document.body.innerHTML).not.toContain('not the password');

    // The right one then unlocks.
    unlockWith(UNLOCK);
    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(2);
  });

  it('says so when there were too many attempts, and stays locked', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    api.on({ 'POST /session/unlock': () => tooManyAttempts() });

    unlockWith(UNLOCK);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('Too many attempts - try again in a few minutes');
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    expect(passwordField().value).toBe('');
    expect(document.activeElement).toBe(passwordField());
    expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe(CSRF_TOKEN);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    expect(document.body.innerHTML).not.toContain(UNLOCK);
  });

  it('says so when the server cannot be reached, and can be tried again', async () => {
    const { api, session } = await lockedWhileUploading(['a.txt']);
    const unlock = session.routes['POST /session/unlock'] as Handler;
    api.on({
      'POST /session/unlock': () => {
        throw new TypeError('the connection was reset');
      },
    });

    unlockWith(UNLOCK);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('The server could not be reached. Try again.');
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    expect(passwordField().value).toBe('');
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    expect(document.body.innerHTML).not.toContain(UNLOCK);

    api.on({ 'POST /session/unlock': unlock });
    unlockWith(UNLOCK);
    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
  });

  it('gives up an unlock the server does not answer in time: says so, keeps the uploads, and can be tried again', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    const held = holdUnlock();

    unlockWith(UNLOCK);
    expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(true);
    await waitFor(() => {
      expect(held.signals).toHaveLength(1);
    });
    await pass(UNLOCK_TIMEOUT_MS);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('The server could not be reached. Try again.');
    expect(held.signals[0]?.aborted).toBe(true);
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(false);
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe(CSRF_TOKEN);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(0);

    unlockWith(UNLOCK);
    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(1);
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe('csrf-after-unlock-1');
  });

  it('signs out while an unlock hangs, without waiting for it', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    // The unlock is held on its way and never answered.
    const held = holdUnlock();
    unlockWith(UNLOCK);
    await waitFor(() => {
      expect(held.signals).toHaveLength(1);
    });

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }));

    // No time passes: the sign-out did not wait for the unlock.
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(1);
    expect(api.callsTo('DELETE', '/session')[0]?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([true]);
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
    expect(endedNotice()).toBeNull();

    // The unlock is given up in its time, aborted, and keeps nothing.
    await pass(UNLOCK_TIMEOUT_MS);
    expect(held.signals[0]?.aborted).toBe(true);
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(0);
    expect(api.callsTo('DELETE', '/session')).toHaveLength(1);
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeDefined();
  });

  it('signs out at once while an unlock hangs, even when the sign-out itself is answered 401', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    api.on({ 'DELETE /session': () => refusal(401, 'InvalidCredentials', 'Not signed in') });
    const held = holdUnlock();
    unlockWith(UNLOCK);
    await waitFor(() => {
      expect(held.signals).toHaveLength(1);
    });

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }));

    // No time passes: neither the sign-out nor its 401 waits for the unlock.
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(1);
    expect(held.signals[0]?.aborted).toBe(false);
    // The unlock is given up in its time, and keeps nothing.
    await pass(UNLOCK_TIMEOUT_MS);
    expect(held.signals[0]?.aborted).toBe(true);
  });

  it('is not signed out by a 401 to an upload answered while the unlock that replaced its token is still on its way', async () => {
    const { api, session, uploads } = await lockedWhileUploading(['f1.txt']);
    api.on({
      'PUT /buckets/photos/object': (call) =>
        call.headers.get('X-CSRF-Token') === session.csrfToken
          ? json(200, {
              bucket: 'photos',
              key: call.query.get('key') ?? '',
              size: call.body.length,
              etag: '"e"',
              last_modified: CREATED,
              content_type: 'text/plain',
              encryption: 'SSE-S3',
            })
          : refusal(401, 'InvalidCredentials', 'Not signed in'),
    });
    const check = holdUnlockAnswer();

    unlockWith(UNLOCK);
    await waitFor(() => {
      expect(check.reached()).toBe(true);
    });
    // The API has the new token; the console not yet. The upload is answered 401 now.
    uploads.release('photos/f1.txt');
    await waitFor(() => {
      expect(stored(api, 'f1.txt')).toHaveLength(1);
    });
    await pass(50);
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();

    check.release();
    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    await pass(UPLOAD_RETRY_MS);
    await waitFor(() => {
      expect(stored(api, 'f1.txt')).toHaveLength(2);
    });
    expect(stored(api, 'f1.txt').map((call) => call.headers.get('X-CSRF-Token'))).toEqual([
      CSRF_TOKEN,
      'csrf-after-unlock-1',
    ]);
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
  });

  it('keeps nothing of an unlock answered after it was given up: the session it renewed is ended with its own token', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    const check = holdUnlockAnswer();
    unlockWith(UNLOCK);
    await waitFor(() => {
      expect(check.reached()).toBe(true);
    });
    // The API replaced the session's token: the one the console holds is refused.
    api.on({ 'GET /session': () => refusal(401, 'InvalidCredentials', 'Not signed in') });
    await pass(UNLOCK_TIMEOUT_MS);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);

    // The answer comes after all: 200, with new tokens. They are not kept,
    // and the session they renewed is ended with them.
    check.release();
    await waitFor(() => {
      expect(api.callsTo('DELETE', '/session')).toHaveLength(1);
    });
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeDefined();
    const ends = api.callsTo('DELETE', '/session');
    expect(ends).toHaveLength(1);
    expect(ends[0]?.headers.get('X-CSRF-Token')).toBe('csrf-after-unlock-1');
    expect(kept()).not.toContain('csrf-after-unlock-1');
  });

  it('is not signed out by a 401 to an upload sent before the unlock and answered after it: the upload is sent again, with the new token', async () => {
    const { api, session, uploads } = await lockedWhileUploading(['f1.txt']);
    // The API refuses the token the session had before it was unlocked.
    api.on({
      'PUT /buckets/photos/object': (call) =>
        call.headers.get('X-CSRF-Token') === session.csrfToken
          ? json(200, {
              bucket: 'photos',
              key: call.query.get('key') ?? '',
              size: call.body.length,
              etag: '"e"',
              last_modified: CREATED,
              content_type: 'text/plain',
              encryption: 'SSE-S3',
            })
          : refusal(401, 'InvalidCredentials', 'Not signed in'),
    });

    unlockWith(UNLOCK);
    expect(await screen.findByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    // The upload sent before the unlock is answered now: 401, for the token of before.
    uploads.release('photos/f1.txt');
    await waitFor(() => {
      expect(stored(api, 'f1.txt')).toHaveLength(1);
    });
    await pass(UPLOAD_RETRY_MS);

    // Tried again with the new token, and stored; nobody was signed out.
    await waitFor(() => {
      expect(stored(api, 'f1.txt')).toHaveLength(2);
    });
    expect(stored(api, 'f1.txt').map((call) => call.headers.get('X-CSRF-Token'))).toEqual([
      CSRF_TOKEN,
      'csrf-after-unlock-1',
    ]);
    // As the same upload: with the same Idempotency-Key.
    const keys = stored(api, 'f1.txt').map((call) => call.headers.get('Idempotency-Key'));
    expect(keys[0]).not.toBeNull();
    expect(keys[1]).toBe(keys[0]);
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(screen.getByRole('heading', { name: 'Users', level: 1 })).toBeDefined();
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe('csrf-after-unlock-1');
  });

  it('asks before it signs out while files are on their way, and then gives them up', async () => {
    const { api } = await lockedWhileUploading(['a.txt', 'b.txt']);
    // It is said beside the button what signing out does.
    const signOut = screen.getByRole('button', { name: 'Sign out' });
    const hint = document.getElementById(signOut.getAttribute('aria-describedby') ?? '');
    expect(hint?.textContent).toBe('Signing out stops the uploads.');

    fireEvent.click(signOut);

    // Asked first: nothing is given up and nobody is signed out.
    const question = within(screen.getByRole('group', { name: 'Sign out and stop the uploads?' }));
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false, false]);
    // The keyboard is on the answer that changes nothing.
    await waitFor(() => {
      expect(document.activeElement).toBe(question.getByRole('button', { name: 'Stay signed in' }));
    });

    // "Stay": the question is gone, the lock and the uploads are not.
    fireEvent.click(question.getByRole('button', { name: 'Stay signed in' }));
    expect(screen.queryByRole('group', { name: 'Sign out and stop the uploads?' })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false, false]);

    // Escape is "stay" as well.
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.keyDown(screen.getByRole('button', { name: 'Stay signed in' }), { key: 'Escape' });
    expect(screen.queryByRole('group', { name: 'Sign out and stop the uploads?' })).toBeNull();
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false, false]);

    // "Yes": the uploads are given up and the session is ended.
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }));

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([true, true]);
    const signOuts = api.callsTo('DELETE', '/session');
    expect(signOuts).toHaveLength(1);
    expect(signOuts[0]?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
    // Whoever signs out is not told that the session timed out.
    expect(endedNotice()).toBeNull();
  });
});

describe('a session that ends', () => {
  it('is signed out when it ends while locked, and told what was stored and what was not', async () => {
    const { api, session, uploads } = await lockedWhileUploading(['a.txt', 'b.txt', 'refused.txt']);
    // The uploads end while the console is locked: two stored, one refused.
    uploads.release('photos/a.txt', 'photos/b.txt', 'photos/refused.txt');
    await waitFor(() => {
      expect(summary()).toBe('2 stored, 1 not stored');
    });
    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    const before = asked(api);

    // Nobody unlocks. A while after its last upload the session has ended:
    // the console learns it when it next asks how the session stands.
    session.stands = 'ended';
    await pass(SESSION_POLL_MS);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(asked(api)).toBe(before + 1);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('status').textContent).toBe(
      'You were signed out: your session timed out.2 objects were stored, 1 was not.',
    );
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
  });

  it('asks a locked session every so often how it stands, and stays locked while the API says so', async () => {
    const { api, uploads } = await lockedWhileUploading(['a.txt']);
    const before = asked(api);
    expect(polls()).toBe(1);

    await pass(SESSION_POLL_MS);
    await waitFor(() => {
      expect(asked(api)).toBe(before + 1);
    });
    // The answer - still locked - has the console wait once more.
    await untilSet(polls, 2);
    await pass(SESSION_POLL_MS);
    await waitFor(() => {
      expect(asked(api)).toBe(before + 2);
    });
    await untilSet(polls, 3);

    expect(screen.getByRole('dialog', { name: 'Session locked' })).toBeDefined();
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false]);
    // The upload is still its own: it ends, stored, under the lock.
    uploads.release('photos/a.txt');
    await waitFor(() => {
      expect(summary()).toBe('1 of 1 stored');
    });
  });

  it('is signed out when the password was wrong too often: the API then says that the session has ended', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    for (let attempt = 1; attempt < UNLOCK_ATTEMPTS; attempt += 1) {
      unlockWith('not the password');
      await waitFor(() => {
        expect(api.callsTo('POST', '/session/unlock')).toHaveLength(attempt);
      });
      await waitFor(() => {
        expect((screen.getByRole('button', { name: 'Unlock' }) as HTMLButtonElement).disabled).toBe(false);
      });
    }
    // The fifth wrong one ends the session.
    api.on({ 'POST /session/unlock': () => refusal(401, 'InvalidCredentials', 'Not signed in') });

    unlockWith('wrong for the fifth time');

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(endedNotice()).toBe(
      'You were signed out: the password was wrong too many times.0 objects were stored, 1 was not.',
    );
    // The uploads ended with the session.
    await waitFor(() => {
      expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([true]);
    });
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
    expect(document.body.innerHTML).not.toContain('wrong for the fifth time');
  });

  it('says only that it was signed out when an unlock finds the session ended without wrong passwords before', async () => {
    const { api } = await lockedWhileUploading(['a.txt']);
    // It ended meanwhile - its password was changed, say.
    api.on({ 'POST /session/unlock': () => refusal(401, 'InvalidCredentials', 'Not signed in') });

    unlockWith(UNLOCK);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(endedNotice()).toBe('You were signed out.0 objects were stored, 1 was not.');
  });

  it('says only that it was signed out when a screen is answered 401', async () => {
    const session = fakeSession(fromNow(LIFETIME_MS));
    signedIn({
      ...session.routes,
      'GET /buckets': session.guarded(() => json(200, [])),
      'GET /users': session.guarded(() => json(200, [])),
    });
    window.location.hash = '#/buckets';
    render(<App />);
    expect(await screen.findByRole('heading', { name: 'Buckets', level: 1 })).toBeDefined();

    // Not at the timeout, and not to a question of how the session stands.
    session.stands = 'ended';
    window.location.hash = '#/users';

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(endedNotice()).toBe('You were signed out.');
  });

  it('shows the lock to a page that is loaded again while locked, and signs it out when the session ends', async () => {
    // The page before took its uploads with it: this one has none.
    const session = fakeSession(fromNow(-LIFETIME_MS));
    session.stands = 'locked';
    const api = signedIn({ ...session.routes, 'GET /buckets': session.guarded(() => json(200, [])) });
    render(<App />);

    const dialog = await lock();
    expect(asked(api)).toBe(1);
    // No screen was shown first, and nothing of one was asked of the API.
    expect(api.callsTo('GET', '/buckets')).toHaveLength(0);
    expect(screen.queryByRole('navigation')).toBeNull();
    expect(document.activeElement).toBe(passwordField());
    // It does not say that uploads continue: there are none in this page.
    expect(dialog.textContent).toContain('Your session timed out while objects were being uploaded.');
    expect(dialog.textContent).not.toContain('The uploads continue.');
    expect(dialog.textContent).toContain('Nothing is being uploaded from this page');
    expect(summary()).toBe('No uploads in progress');

    // The grace after the last upload passes and nobody unlocked.
    session.stands = 'ended';
    await pass(SESSION_POLL_MS);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(asked(api)).toBe(2);
    // There were no uploads to count.
    expect(endedNotice()).toBe('You were signed out: your session timed out.');
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
  });

  it('can be unlocked by a page that was loaded again while locked', async () => {
    const session = fakeSession(fromNow(-LIFETIME_MS), fromNow(60 * LIFETIME_MS));
    session.stands = 'locked';
    const api = signedIn({ ...session.routes, 'GET /buckets': session.guarded(() => json(200, [])) });
    window.location.hash = '#/buckets';
    render(<App />);
    await lock();
    expect(api.callsTo('GET', '/buckets')).toHaveLength(0);

    unlockWith(UNLOCK);

    expect(await screen.findByRole('heading', { name: 'Buckets', level: 1 })).toBeDefined();
    // Only now is anything of a screen asked of the API.
    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets')).toHaveLength(1);
    });
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe('csrf-after-unlock-1');
  });

  it('signs out of the lock at once, without asking, when nothing is on its way', async () => {
    const session = fakeSession(fromNow(-LIFETIME_MS));
    session.stands = 'locked';
    const api = signedIn({ ...session.routes });
    render(<App />);
    await lock();

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));

    expect(screen.queryByRole('group', { name: 'Sign out and stop the uploads?' })).toBeNull();
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(1);
    expect(endedNotice()).toBeNull();
  });

  it('is signed out at the timeout when nothing was being uploaded, and is never shown the lock', async () => {
    const session = fakeSession(fromNow(LIFETIME_MS));
    const api = signedIn({ ...session.routes, 'GET /buckets': session.guarded(() => json(200, [])) });
    render(<App />);
    expect(await screen.findByRole('heading', { name: 'Buckets', level: 1 })).toBeDefined();
    expect(asked(api)).toBe(1);
    await untilSet(timeouts, 1);

    // Nothing is on its way: at the timeout the API ends the session.
    session.stands = 'ended';
    await pass(LIFETIME_MS + SESSION_ASK_AFTER_MS);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(asked(api)).toBe(2);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(endedNotice()).toBe('You were signed out: your session timed out.');
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
  });

  it('does not lock by its own clock: a session the API calls open at the timeout keeps its screen', async () => {
    const session = fakeSession(fromNow(LIFETIME_MS));
    const api = signedIn({ ...session.routes, 'GET /buckets': session.guarded(() => json(200, [])) });
    render(<App />);
    expect(await screen.findByRole('heading', { name: 'Buckets', level: 1 })).toBeDefined();
    await untilSet(timeouts, 1);

    // The clock of the console says the timeout has come; the API - whose
    // clock it is - says the session is open, with the same timeout.
    await pass(LIFETIME_MS + SESSION_ASK_AFTER_MS);
    await waitFor(() => {
      expect(asked(api)).toBe(2);
    });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(screen.getByRole('heading', { name: 'Buckets', level: 1 })).toBeDefined();

    // It is not asked again at once, but as often as a locked session is...
    await untilSet(polls, 1);
    await pass(SESSION_POLL_MS / 3);
    expect(asked(api)).toBe(2);
    // ...and then the API says that the session has ended.
    session.stands = 'ended';
    await pass(SESSION_POLL_MS);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(asked(api)).toBe(3);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(endedNotice()).toBe('You were signed out: your session timed out.');
  });

  it('says nothing of a session that ended to whoever was never signed in, or signed out', async () => {
    const session = fakeSession(fromNow(LIFETIME_MS));
    session.stands = 'ended';
    signedIn({ ...session.routes });
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(endedNotice()).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
  });
});
