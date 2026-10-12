import { File as NodeFile } from 'node:buffer';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { App } from './App';
import { inProgress, json, noContent, problem, replayed, signedIn, type FakeApi, type Handler } from './test/fakeApi';

// Uploads that go on when another screen is shown, the bar at the top of
// every screen that says how they stand, and the notices of what happened.

const CREATED = '2026-10-01T00:00:00Z';

/** The answers of one bucket: it lists what it holds, and holds what is put into it. */
function bucketRoutes(bucket: string, held: string[] = []): Record<string, Handler> {
  return {
    [`GET /buckets/${bucket}/objects`]: () =>
      json(200, {
        objects: held.map((key) => ({ key, size: 10, etag: '"e"', last_modified: CREATED })),
        common_prefixes: [],
        truncated: false,
        access_included: false,
      }),
    [`PUT /buckets/${bucket}/object`]: (call) => {
      const key = call.query.get('key') ?? '';
      held.push(key);
      return json(200, {
        bucket,
        key,
        size: call.body.length,
        etag: '"e"',
        last_modified: CREATED,
        content_type: 'text/plain',
        encryption: 'SSE-S3',
      });
    },
  };
}

/**
 * Holds every request of a method on its way to the fake API until the test
 * lets it go, by the bucket and the key it is for: `photos/a.txt`. The
 * fake's XMLHttpRequest answers through fetch, so holding fetch holds an
 * upload - which is then "being sent" for as long as the test likes. A
 * request that is held has not reached the API: it is not among its calls.
 */
function hold(method: string) {
  const waiting = new Map<string, () => void>();
  const free = new Set<string>();
  const answer = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === method) {
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

/** The bar. */
function activity() {
  return within(screen.getByRole('region', { name: 'Activity' }));
}

/** The line of the bar, as it is shown. */
function summary(): string {
  return document.querySelector('.activitybar-text')?.textContent ?? '';
}

/** What the bar has read out. */
function announced(): string {
  return document.querySelector('.activitybar-live')?.textContent ?? '';
}

/** The uploads that were begun for a key: held or not, they are requests of the browser. */
function xhrsFor(api: FakeApi, key: string) {
  return api.xhrs.filter((xhr) => new URL(xhr.url, 'http://localhost').searchParams.get('key') === key);
}

function openUploads() {
  fireEvent.click(activity().getByRole('button', { name: /^Uploads: / }));
  return within(activity().getByRole('group', { name: 'Uploads' }));
}

function openNotices() {
  fireEvent.click(activity().getByRole('button', { name: /^Notices: / }));
  return within(activity().getByRole('group', { name: 'Notices' }));
}

async function goTo(hash: string, heading: string) {
  window.location.hash = hash;
  await screen.findByRole('heading', { name: heading, level: 1 });
}

describe('uploads that belong to the console', () => {
  it('goes on with an upload when another screen is shown, says in the bar how it stands, and keeps how it ended', async () => {
    const api = signedIn({ ...bucketRoutes('photos'), 'GET /users': () => json(200, []) });
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    expect(summary()).toBe('No uploads in progress');
    expect(announced()).toBe('');

    chooseAndUpload(['a.txt', 'b.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(2);
    });
    // It is said that uploads began, in the line and to whoever has it read.
    await waitFor(() => {
      expect(summary()).toContain('Uploading 2 files');
    });
    expect(announced()).toBe('Uploading 2 files.');

    // Another screen: the one of the bucket, and its tiles, are gone...
    await goTo('#/users', 'Users');
    expect(screen.queryByRole('list', { name: 'Upload progress' })).toBeNull();
    // ...and the requests are not: nothing was aborted.
    expect(api.xhrs.map((xhr) => xhr.aborted)).toEqual([false, false]);
    expect(summary()).toContain('Uploading 2 files');
    expect(summary()).toContain('0 stored');
    const whole = activity().getByRole('progressbar', { name: 'All uploads' }) as HTMLProgressElement;
    expect(whole.max).toBe(1);
    expect(whole.value).toBeLessThan(1);

    // Opened, the bar shows each file with its bucket and a bar of its own.
    const toggle = activity().getByRole('button', { name: 'Uploads: 2' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    const panel = openUploads();
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(panel.getByRole('link', { name: 'photos' }).getAttribute('href')).toBe('#/buckets/photos');
    expect(panel.getByRole('progressbar', { name: 'Upload of a.txt to photos' })).toBeDefined();
    expect(panel.getByRole('progressbar', { name: 'Upload of b.txt to photos' })).toBeDefined();
    expect(panel.getByRole('button', { name: 'Cancel the upload of a.txt' })).toBeDefined();

    // One is stored while the other is still on its way.
    uploads.release('photos/a.txt');
    await waitFor(() => {
      expect(summary()).toContain('1 stored');
    });
    expect(summary()).toContain('Uploading 2 files');
    expect(announced()).toBe('Uploading 2 files.');
    expect((panel.getByRole('progressbar', { name: 'Upload of a.txt to photos' }) as HTMLProgressElement).value).toBe(1);
    expect(panel.queryByRole('button', { name: 'Cancel the upload of a.txt' })).toBeNull();

    uploads.release('photos/b.txt');
    await waitFor(() => {
      expect(summary()).toBe('2 of 2 stored');
    });
    // That they ended is read out, once, with how.
    expect(announced()).toBe('Uploads finished: 2 of 2 stored.');
    expect(activity().queryByRole('progressbar', { name: 'All uploads' })).toBeNull();
    expect(panel.getAllByText('Stored')).toHaveLength(2);
    // Not by colour alone: the sign before the line is the one of "stored".
    expect(document.querySelector('.activitybar-summary i.bi-check-circle')).not.toBeNull();

    // One notice for the batch, not one a file, with the way to the bucket.
    expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
    const notices = openNotices();
    expect(notices.getByText(/Stored 2 objects in photos/)).toBeDefined();
    expect(notices.getByRole('link', { name: 'Open photos' }).getAttribute('href')).toBe('#/buckets/photos');

    // Back in the bucket: the list was read after the upload, so the objects
    // are in it, and the tiles are the same ones, as they ended.
    window.location.hash = '#/buckets/photos';
    expect(await screen.findByLabelText('Select a.txt')).toBeDefined();
    expect(screen.getByLabelText('Select b.txt')).toBeDefined();
    const tiles = within(screen.getByRole('list', { name: 'Upload progress' }));
    expect(tiles.getAllByText('Stored')).toHaveLength(2);
    expect((tiles.getByRole('progressbar', { name: 'Upload of a.txt' }) as HTMLProgressElement).value).toBe(1);
    // Each file was sent once.
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(2);
  });

  it('reads the list of the bucket that is shown again when an upload into it ends', async () => {
    const api = signedIn(bucketRoutes('photos'));
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');

    chooseAndUpload(['a.txt', 'b.txt', 'c.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(3);
    });
    uploads.release('photos/a.txt', 'photos/b.txt', 'photos/c.txt');

    for (const name of ['a.txt', 'b.txt', 'c.txt']) {
      expect(await screen.findByLabelText(`Select ${name}`)).toBeDefined();
    }
    // The list was asked for after the last file was stored.
    const lastPut = api.calls.findLastIndex((call) => call.method === 'PUT');
    const lastList = api.calls.findLastIndex((call) => call.method === 'GET' && call.path === '/buckets/photos/objects');
    expect(lastList).toBeGreaterThan(lastPut);
  });

  it('sends no more files at once than the limit, whatever buckets they go to', async () => {
    const api = signedIn({ ...bucketRoutes('photos'), ...bucketRoutes('films') });
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');

    chooseAndUpload(['p1.txt', 'p2.txt', 'p3.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(3);
    });

    window.location.hash = '#/buckets/films';
    await screen.findByRole('heading', { name: 'films', level: 1 });
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['f1.txt', 'f2.txt', 'f3.txt']);

    // Three to one bucket and three to another: four are on their way, two wait.
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(4);
    });
    expect(xhrsFor(api, 'f1.txt')).toHaveLength(1);
    const panel = openUploads();
    await waitFor(() => {
      expect(panel.getAllByText('Waiting')).toHaveLength(2);
    });
    expect(api.xhrs).toHaveLength(4);
    // Each batch under its bucket.
    expect(panel.getByRole('link', { name: 'photos' })).toBeDefined();
    expect(panel.getByRole('link', { name: 'films' })).toBeDefined();

    // A place that comes free goes to the file that waited longest.
    uploads.release('photos/p1.txt');
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(5);
    });
    expect(xhrsFor(api, 'f2.txt')).toHaveLength(1);
    expect(xhrsFor(api, 'f3.txt')).toHaveLength(0);

    uploads.release('photos/p2.txt', 'photos/p3.txt', 'films/f1.txt', 'films/f2.txt', 'films/f3.txt');
    await waitFor(() => {
      expect(summary()).toBe('6 of 6 stored');
    });
    expect(api.xhrs).toHaveLength(6);
    // A notice a batch.
    const notices = openNotices();
    expect(notices.getByText(/Stored 3 objects in photos/)).toBeDefined();
    expect(notices.getByText(/Stored 3 objects in films/)).toBeDefined();
  });

  it(
    'says what was not stored and why, and sends it again from the bar of another screen as the same upload',
    async () => {
      let down = true;
      const stored: string[] = [];
      const api = signedIn({
        ...bucketRoutes('photos', stored),
        'GET /users': () => json(200, []),
      });
      const put = bucketRoutes('photos', stored)['PUT /buckets/photos/object'];
      api.on({
        // A node without a leader says "not now", for as long as the test has it.
        'PUT /buckets/photos/object': (call) =>
          down || put === undefined ? problem(503, 'No metadata leader', 'Try again shortly.') : put(call),
      });
      window.location.hash = '#/buckets/photos';
      render(<App />);
      await screen.findByText('There is nothing here.');

      chooseAndUpload(['c.txt']);
      await goTo('#/users', 'Users');

      // It is tried three times, a moment apart, and then given up.
      await waitFor(
        () => {
          expect(summary()).toBe('0 stored, 1 not stored');
        },
        { timeout: 5000 },
      );
      expect(announced()).toBe('Uploads finished: 0 stored, 1 not stored.');
      // Not by colour alone: another sign, and the words.
      expect(document.querySelector('.activitybar-summary i.bi-exclamation-triangle')).not.toBeNull();
      expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(3);

      const panel = openUploads();
      expect(panel.getByText('Not stored')).toBeDefined();
      expect(panel.getByText('No metadata leader: Try again shortly.')).toBeDefined();
      const notices = openNotices();
      expect(notices.getByText(/1 of 1 object was not stored in photos/)).toBeDefined();
      expect(notices.getByText('No metadata leader: Try again shortly.')).toBeDefined();

      // The node is back. What failed is sent again, from here.
      down = false;
      fireEvent.click(openUploads().getByRole('button', { name: 'Retry failed' }));
      await waitFor(() => {
        expect(summary()).toBe('1 of 1 stored');
      });
      expect(stored).toEqual(['c.txt']);
      expect(activity().queryByRole('button', { name: 'Retry failed' })).toBeNull();
      // Every attempt, the one that was asked for by hand too, was the same
      // upload to the API: it was never answered but by a server that could
      // not, so one Idempotency-Key says that nothing is stored twice.
      const attempts = api.callsTo('PUT', '/buckets/photos/object');
      expect(attempts).toHaveLength(4);
      expect(new Set(attempts.map((call) => call.headers.get('Idempotency-Key'))).size).toBe(1);
      expect(attempts[0]?.headers.get('Idempotency-Key')).toBeTruthy();
    },
    15000,
  );

  it('sends a file the API refused again as a new upload: the refusal is kept under the key it was given for', async () => {
    let allowed = false;
    const stored: string[] = [];
    const api = signedIn(bucketRoutes('photos', stored));
    const put = bucketRoutes('photos', stored)['PUT /buckets/photos/object'];
    api.on({
      'PUT /buckets/photos/object': (call) =>
        allowed && put !== undefined ? put(call) : problem(403, 'Access denied', 'Not this one.'),
    });
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');

    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(summary()).toBe('0 stored, 1 not stored');
    });
    // A refusal is not tried again by itself.
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(1);

    allowed = true;
    fireEvent.click(openUploads().getByRole('button', { name: 'Retry failed' }));
    await waitFor(() => {
      expect(summary()).toBe('1 of 1 stored');
    });
    const attempts = api.callsTo('PUT', '/buckets/photos/object');
    expect(attempts).toHaveLength(2);
    expect(attempts[1]?.headers.get('Idempotency-Key')).toBeTruthy();
    expect(attempts[1]?.headers.get('Idempotency-Key')).not.toBe(attempts[0]?.headers.get('Idempotency-Key'));
  });

  it(
    'asks again as the same upload when the API is still working on a request whose answer was lost, and stores the file once',
    async () => {
      const stored: string[] = [];
      let asked = 0;
      const api = signedIn(bucketRoutes('photos', stored));
      api.on({
        'PUT /buckets/photos/object': (call) => {
          const key = call.query.get('key') ?? '';
          const answer = () =>
            json(200, {
              bucket: 'photos',
              key,
              size: call.body.length,
              etag: '"e"',
              last_modified: CREATED,
              content_type: 'text/plain',
              encryption: 'SSE-S3',
            });
          asked += 1;
          if (asked === 1) {
            // The file arrives and is stored; the connection fails before the answer is back.
            stored.push(key);
            throw new TypeError('the connection was reset');
          }
          // The repeat comes while the first request is still being committed...
          if (asked === 2) {
            return inProgress();
          }
          // ...and the one after it is given the answer the API kept: nothing is stored again.
          return replayed(answer());
        },
      });
      window.location.hash = '#/buckets/photos';
      render(<App />);
      await screen.findByText('There is nothing here.');

      chooseAndUpload(['a.txt']);
      // A moment for the failed connection, two seconds for "still working".
      await waitFor(
        () => {
          expect(summary()).toBe('1 of 1 stored');
        },
        { timeout: 10000 },
      );

      // Three requests, and to the API one upload: one Idempotency-Key.
      const attempts = api.callsTo('PUT', '/buckets/photos/object');
      expect(attempts).toHaveLength(3);
      expect(attempts[0]?.headers.get('Idempotency-Key')).toBeTruthy();
      expect(new Set(attempts.map((call) => call.headers.get('Idempotency-Key'))).size).toBe(1);
      expect(stored).toEqual(['a.txt']);
      // One tile, stored, and nothing that was ever "not stored"...
      const tiles = within(screen.getByRole('list', { name: 'Upload progress' }));
      expect(tiles.getAllByRole('listitem')).toHaveLength(1);
      expect(tiles.getByText('Stored')).toBeDefined();
      expect(screen.queryByText('Not stored')).toBeNull();
      expect(screen.queryByRole('button', { name: 'Retry failed' })).toBeNull();
      expect(announced()).toBe('Uploads finished: 1 of 1 stored.');
      // ...and one notice.
      expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
      expect(openNotices().getByText(/Stored 1 object in photos/)).toBeDefined();
      expect(await screen.findByLabelText('Select a.txt')).toBeDefined();
    },
    20000,
  );

  it('speaks in the bar of what is on its way now, and of how that ended: not of what failed earlier in the visit', async () => {
    const stored: string[] = [];
    const api = signedIn(bucketRoutes('photos', stored));
    const put = bucketRoutes('photos', stored)['PUT /buckets/photos/object'];
    api.on({
      'PUT /buckets/photos/object': (call) =>
        call.query.get('key') === 'bad.txt' || put === undefined ? problem(403, 'Access denied') : put(call),
    });
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');

    // An upload that fails, and is left as it is: not dismissed.
    chooseAndUpload(['bad.txt']);
    uploads.release('photos/bad.txt');
    await waitFor(() => {
      expect(summary()).toBe('0 stored, 1 not stored');
    });
    expect(announced()).toBe('Uploads finished: 0 stored, 1 not stored.');

    // Another, later: while it is on its way the bar speaks of it alone.
    chooseAndUpload(['good.txt']);
    await waitFor(() => {
      expect(summary()).toContain('Uploading 1 file');
    });
    expect(summary()).toContain('0 stored');
    expect(summary()).not.toContain('not stored');
    expect(announced()).toBe('Uploading 1 file.');
    // All of the line is in its hint, for where it is cut short.
    expect(document.querySelector('.activitybar-text')?.getAttribute('title')).toBe(summary());
    // The button still counts every upload that was not dismissed.
    expect(activity().getByRole('button', { name: 'Uploads: 2' })).toBeDefined();

    uploads.release('photos/good.txt');
    // Nothing is on its way: the line is of all that was not dismissed...
    await waitFor(() => {
      expect(summary()).toBe('1 stored, 1 not stored');
    });
    expect(document.querySelector('.activitybar-summary i.bi-exclamation-triangle')).not.toBeNull();
    // ...and what is read out is how this upload ended, which was well.
    expect(announced()).toBe('Uploads finished: 1 of 1 stored.');
    expect(stored).toEqual(['good.txt']);
  });

  it('cancels a file that waits and one that is being sent: neither is sent again, and neither is a failure', async () => {
    const api = signedIn({ ...bucketRoutes('photos'), 'GET /users': () => json(200, []) });
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');

    chooseAndUpload(['f1.txt', 'f2.txt', 'f3.txt', 'f4.txt', 'f5.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(4);
    });
    await goTo('#/users', 'Users');
    const panel = openUploads();

    // The fifth waits for its turn: cancelled, it never gets one.
    fireEvent.click(panel.getByRole('button', { name: 'Cancel the upload of f5.txt' }));
    expect(panel.getByText('Cancelled')).toBeDefined();
    expect(panel.queryByRole('button', { name: 'Cancel the upload of f5.txt' })).toBeNull();

    // The first is being sent: cancelled, its request is aborted.
    expect(xhrsFor(api, 'f1.txt').map((xhr) => xhr.aborted)).toEqual([false]);
    fireEvent.click(panel.getByRole('button', { name: 'Cancel the upload of f1.txt' }));
    expect(xhrsFor(api, 'f1.txt').map((xhr) => xhr.aborted)).toEqual([true]);
    expect(panel.getAllByText('Cancelled')).toHaveLength(2);
    // The others are untouched.
    expect(api.xhrs.filter((xhr) => xhr.aborted)).toHaveLength(1);

    uploads.release('photos/f2.txt', 'photos/f3.txt', 'photos/f4.txt');
    await waitFor(() => {
      expect(summary()).toBe('3 stored, 2 cancelled');
    });
    // Cancelled is not failed: nothing is "not stored", and there is nothing to send again.
    expect(summary()).not.toContain('not stored');
    expect(panel.queryByText('Not stored')).toBeNull();
    expect(activity().queryByRole('button', { name: 'Retry failed' })).toBeNull();
    // The place the first gave up did not go to the fifth, and nothing was begun twice.
    expect(xhrsFor(api, 'f5.txt')).toHaveLength(0);
    expect(xhrsFor(api, 'f1.txt')).toHaveLength(1);
    expect(api.xhrs).toHaveLength(4);
    expect(
      api
        .callsTo('PUT', '/buckets/photos/object')
        .map((call) => call.query.get('key'))
        .sort(),
    ).toEqual(['f2.txt', 'f3.txt', 'f4.txt']);
    // It is said as it was: a note, not a failure.
    const notices = openNotices();
    expect(notices.getByText(/Stored 3 of 5 objects in photos: 2 cancelled/)).toBeDefined();
    expect(notices.getByText('Note:')).toBeDefined();
  });

  it('dismisses what is over, from the bar and from the bucket, and a notice by itself or all at once', async () => {
    signedIn({ ...bucketRoutes('photos'), ...bucketRoutes('films') });
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(summary()).toBe('1 of 1 stored');
    });

    window.location.hash = '#/buckets/films';
    await screen.findByRole('heading', { name: 'films', level: 1 });
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['b.txt']);
    await waitFor(() => {
      expect(summary()).toBe('2 of 2 stored');
    });

    // "Clear" on the screen of a bucket forgets the uploads of that bucket only.
    fireEvent.click(await screen.findByRole('button', { name: 'Clear' }));
    expect(screen.queryByRole('list', { name: 'Upload progress' })).toBeNull();
    expect(summary()).toBe('1 of 1 stored');
    expect(activity().getByRole('button', { name: 'Uploads: 1' })).toBeDefined();

    // "Dismiss finished" in the bar forgets the batch it stands with.
    const panel = openUploads();
    expect(panel.getByRole('link', { name: 'photos' })).toBeDefined();
    expect(panel.queryByRole('link', { name: 'films' })).toBeNull();
    fireEvent.click(panel.getByRole('button', { name: 'Dismiss finished' }));
    expect(summary()).toBe('No uploads in progress');
    expect(panel.getByText('There are no uploads to show.')).toBeDefined();
    expect(activity().getByRole('button', { name: 'Uploads: 0' })).toBeDefined();

    // The notices are still there: newest first, each dismissed by itself...
    const notices = openNotices();
    expect(notices.getAllByRole('listitem').map((item) => item.querySelector('.activitybar-notice-title')?.textContent)).toEqual([
      'Done: Stored 1 object in films',
      'Done: Stored 1 object in photos',
    ]);
    fireEvent.click(notices.getByRole('button', { name: 'Dismiss: Stored 1 object in films' }));
    expect(notices.queryByText(/Stored 1 object in films/)).toBeNull();
    expect(notices.getByText(/Stored 1 object in photos/)).toBeDefined();
    expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
    // ...or all at once.
    fireEvent.click(notices.getByRole('button', { name: 'Dismiss all' }));
    expect(notices.getByText('There are no notices to show.')).toBeDefined();
    expect(activity().getByRole('button', { name: 'Notices: 0' })).toBeDefined();
  });

  it('asks before the page is left only while files are on their way', async () => {
    const add = vi.spyOn(window, 'addEventListener');
    const remove = vi.spyOn(window, 'removeEventListener');
    const asking = (spy: typeof add | typeof remove) => spy.mock.calls.filter(([type]) => type === 'beforeunload');
    const api = signedIn(bucketRoutes('photos'));
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    // Nothing on its way: the page may be left without a question.
    expect(asking(add)).toHaveLength(0);

    chooseAndUpload(['a.txt', 'b.txt']);
    await waitFor(() => {
      expect(asking(add)).toHaveLength(1);
    });
    expect(asking(remove)).toHaveLength(0);
    // What was registered asks the browser to ask.
    const ask = asking(add)[0]?.[1];
    const leaving = new Event('beforeunload', { cancelable: true });
    if (typeof ask !== 'function') {
      throw new Error('no beforeunload listener was registered');
    }
    ask.call(window, leaving);
    expect(leaving.defaultPrevented).toBe(true);

    // One of two is stored: files are still on their way, and so is the question.
    uploads.release('photos/a.txt');
    await waitFor(() => {
      expect(summary()).toContain('1 stored');
    });
    expect(asking(add)).toHaveLength(1);
    expect(asking(remove)).toHaveLength(0);

    uploads.release('photos/b.txt');
    await waitFor(() => {
      expect(asking(remove)).toHaveLength(1);
    });
    expect(asking(remove)[0]?.[1]).toBe(ask);
    expect(asking(add)).toHaveLength(1);
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(2);
  });

  it('asks before signing out while files are on their way, and stops them when the answer is yes', async () => {
    const api = signedIn({ ...bucketRoutes('photos'), 'DELETE /session': () => noContent() });
    hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(1);
    });

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    // It asks, in the bar, and the keyboard is on the answer that changes nothing.
    const question = within(activity().getByRole('group', { name: 'Sign out and stop the uploads?' }));
    expect(question.getByText(/1 file is still being uploaded/)).toBeDefined();
    await waitFor(() => {
      expect(document.activeElement).toBe(question.getByRole('button', { name: 'Stay signed in' }));
    });
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);

    fireEvent.click(question.getByRole('button', { name: 'Stay signed in' }));
    expect(activity().queryByRole('group', { name: 'Sign out and stop the uploads?' })).toBeNull();
    expect(api.xhrs[0]?.aborted).toBe(false);
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(activity().getByRole('button', { name: 'Yes, sign out' }));
    expect(api.xhrs[0]?.aborted).toBe(true);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(api.callsTo('DELETE', '/session')).toHaveLength(1);
    // The file never reached the API.
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(0);
  });

  it('puts the keyboard back on "Sign out" when the answer is to stay, and takes Escape for that answer', async () => {
    const api = signedIn({ ...bucketRoutes('photos'), 'DELETE /session': () => noContent() });
    hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(1);
    });
    const signOut = screen.getByRole('button', { name: 'Sign out' });
    const asked = () => activity().queryByRole('group', { name: 'Sign out and stop the uploads?' });

    // "Stay signed in": the question goes, and the keyboard is where it was asked.
    fireEvent.click(signOut);
    await waitFor(() => {
      expect(document.activeElement).toBe(activity().getByRole('button', { name: 'Stay signed in' }));
    });
    fireEvent.click(activity().getByRole('button', { name: 'Stay signed in' }));
    expect(asked()).toBeNull();
    expect(document.activeElement).toBe(signOut);

    // Escape is the same answer.
    fireEvent.click(signOut);
    await waitFor(() => {
      expect(document.activeElement).toBe(activity().getByRole('button', { name: 'Stay signed in' }));
    });
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' });
    expect(asked()).toBeNull();
    expect(document.activeElement).toBe(signOut);

    // With a panel open too, Escape answers the question first and leaves the panel; then it closes the panel.
    openUploads();
    fireEvent.click(signOut);
    await waitFor(() => {
      expect(document.activeElement).toBe(activity().getByRole('button', { name: 'Stay signed in' }));
    });
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: 'Escape' });
    expect(asked()).toBeNull();
    expect(document.getElementById('activity-uploads')).not.toBeNull();
    expect(document.activeElement).toBe(signOut);
    fireEvent.keyDown(activity().getByRole('button', { name: 'Uploads: 1' }), { key: 'Escape' });
    expect(document.getElementById('activity-uploads')).toBeNull();

    // Nobody was signed out, and the upload goes on.
    expect(api.callsTo('DELETE', '/session')).toHaveLength(0);
    expect(api.xhrs[0]?.aborted).toBe(false);
  });

  it('gives up what is on its way when the session has expired', async () => {
    const api = signedIn({
      ...bucketRoutes('photos'),
      'PUT /buckets/photos/object': () => problem(401, 'The session has expired'),
    });
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt', 'b.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(2);
    });

    // One of them is answered 401: the sign-in screen takes over, as for any request...
    uploads.release('photos/b.txt');
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
    // ...and the other, which nobody is there to hear of any more, is given up.
    await waitFor(() => {
      expect(xhrsFor(api, 'a.txt').map((xhr) => xhr.aborted)).toEqual([true]);
    });
    expect(xhrsFor(api, 'b.txt').map((xhr) => xhr.aborted)).toEqual([false]);
  });

  it('does not take the keyboard away when uploads end and a notice arrives', async () => {
    const api = signedIn(bucketRoutes('photos'));
    const uploads = hold('PUT');
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(api.xhrs).toHaveLength(1);
    });

    const field = screen.getByLabelText('Search buckets and objects');
    field.focus();
    expect(document.activeElement).toBe(field);
    uploads.release('photos/a.txt');
    await waitFor(() => {
      expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
    });
    await screen.findByLabelText('Select a.txt');
    expect(document.activeElement).toBe(field);
  });
});

describe('the activity bar', () => {
  it.each([
    '#/buckets',
    '#/buckets/photos',
    '#/buckets/photos?prefix=2026%2F',
    '#/search?q=a',
    '#/users',
    '#/users/alice',
    '#/groups',
    '#/roles',
    '#/roles/read-only',
    '#/policies',
    '#/cluster',
    '#/settings',
    '#/audit',
    '#/nowhere',
  ])('is at the top of the screen at %s, with the navigation and above the screen', async (hash) => {
    // Whatever the screen asks for and is not given: the bar is there all the same.
    signedIn();
    window.location.hash = hash;
    render(<App />);

    const bar = await screen.findByRole('region', { name: 'Activity' });
    expect(bar.className.split(' ')).toContain('activitybar');
    // The navigation and the bar are the two children of one element - which
    // on a phone is what is held at the top, so that the bar is under the
    // navigation and never behind it - and the screen follows that element.
    const top = bar.parentElement;
    expect(top?.className).toBe('layout-top');
    expect(top?.children).toHaveLength(2);
    expect(top?.children[0]).toBe(document.querySelector('header.sidebar'));
    expect(top?.children[1]).toBe(bar);
    expect(top?.nextElementSibling).toBe(document.querySelector('main.screen'));
    expect(top?.parentElement?.className).toContain('layout');
    // The bar is not a part of the screen: it does not go when the screen does.
    expect(document.querySelector('main.screen')?.contains(bar)).toBe(false);

    // Always there, with nothing going on too: it says so, and has its two buttons.
    expect(summary()).toBe('No uploads in progress');
    expect(bar.className.split(' ')).toContain('activitybar-is-idle');
    expect(activity().getByRole('button', { name: 'Uploads: 0' })).toBeDefined();
    expect(activity().getByRole('button', { name: 'Notices: 0' })).toBeDefined();
    // What is read out is a polite live region, and says nothing yet.
    const live = document.querySelector('.activitybar-live');
    expect(live?.getAttribute('aria-live')).toBe('polite');
    expect(live?.textContent).toBe('');
  });

  it('shares no class with the latest upload and download in the grid of buckets', async () => {
    signedIn(bucketRoutes('photos'));
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(summary()).toBe('1 of 1 stored');
    });

    const bar = screen.getByRole('region', { name: 'Activity' });
    // What the stylesheet has for "activity-line", "activity-key" and
    // "activity-earlier" is the grid's. Nothing of the bar, closed or with
    // either panel open, is called anything that begins so.
    const classes = () => [bar, ...bar.querySelectorAll('*')].flatMap((element) => [...element.classList]);
    const ofTheGrid = (name: string) => name === 'activity' || name.startsWith('activity-');
    expect(classes().filter(ofTheGrid)).toEqual([]);
    openUploads();
    expect(classes().filter(ofTheGrid)).toEqual([]);
    openNotices();
    expect(classes().filter(ofTheGrid)).toEqual([]);
    // The bar's own all begin with one word.
    expect(classes().filter((name) => name.startsWith('activitybar')).length).toBeGreaterThan(5);
    // And nothing outside the bar is called by one of the bar's names.
    for (const element of document.querySelectorAll('[class*="activitybar"]')) {
      expect(bar.contains(element)).toBe(true);
    }
  });

  it('is not there before anybody has signed in', async () => {
    signedIn({ 'GET /session': () => problem(401, 'Not signed in') });
    render(<App />);

    await screen.findByRole('heading', { name: 'Sign in' });
    expect(screen.queryByRole('region', { name: 'Activity' })).toBeNull();
  });

  it('gives every button a hint, and opens and closes its panels with the keyboard', async () => {
    signedIn({ ...bucketRoutes('photos') });
    window.location.hash = '#/buckets/photos';
    render(<App />);
    await screen.findByText('There is nothing here.');
    chooseAndUpload(['a.txt']);
    await waitFor(() => {
      expect(summary()).toBe('1 of 1 stored');
    });

    const uploadsButton = activity().getByRole('button', { name: 'Uploads: 1' });
    const noticesButton = activity().getByRole('button', { name: 'Notices: 1' });
    // Buttons are buttons: the keyboard reaches them and Enter presses them.
    expect(uploadsButton.tagName).toBe('BUTTON');
    expect(uploadsButton.getAttribute('aria-controls')).toBe('activity-uploads');
    expect(noticesButton.getAttribute('aria-controls')).toBe('activity-notices');

    fireEvent.click(uploadsButton);
    expect(document.getElementById('activity-uploads')).not.toBeNull();
    for (const control of [...activity().getAllByRole('button'), ...activity().getAllByRole('link')]) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
    // One panel at a time.
    fireEvent.click(noticesButton);
    expect(document.getElementById('activity-uploads')).toBeNull();
    expect(document.getElementById('activity-notices')).not.toBeNull();
    expect(uploadsButton.getAttribute('aria-expanded')).toBe('false');
    expect(noticesButton.getAttribute('aria-expanded')).toBe('true');
    for (const control of [...activity().getAllByRole('button'), ...activity().getAllByRole('link')]) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }

    // Escape closes what is open, and the keyboard is on the button that opened it.
    fireEvent.keyDown(activity().getByRole('button', { name: 'Dismiss all' }), { key: 'Escape' });
    expect(document.getElementById('activity-notices')).toBeNull();
    expect(noticesButton.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(noticesButton);
  });
});

describe('notices of what happened', () => {
  it('says that a bucket was created and that one was deleted', async () => {
    const buckets: { name: string; created_at: string }[] = [];
    signedIn({
      'GET /buckets': () => json(200, buckets),
      'POST /buckets': (call) => {
        const bucket = { name: (JSON.parse(call.body) as { name: string }).name, created_at: CREATED };
        buckets.push(bucket);
        return json(201, bucket);
      },
      'DELETE /buckets/holiday': () => {
        buckets.length = 0;
        return noContent();
      },
    });
    render(<App />);
    await screen.findByText('Signed in as alice');
    expect(activity().getByRole('button', { name: 'Notices: 0' })).toBeDefined();

    fireEvent.change(await screen.findByLabelText('Bucket name'), { target: { value: 'holiday' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create bucket' }));
    await waitFor(() => {
      expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
    });
    const notices = openNotices();
    expect(notices.getByText(/Created bucket holiday/)).toBeDefined();
    expect(notices.getByRole('link', { name: 'Open holiday' }).getAttribute('href')).toBe('#/buckets/holiday');

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));
    await waitFor(() => {
      expect(activity().getByRole('button', { name: 'Notices: 2' })).toBeDefined();
    });
    // Newest first.
    expect(
      notices.getAllByRole('listitem').map((item) => item.querySelector('.activitybar-notice-title')?.textContent),
    ).toEqual(['Done: Deleted bucket holiday', 'Done: Created bucket holiday']);
  });

  it('says nothing was done when the API refuses, and leaves the reason to the screen that is shown', async () => {
    signedIn({
      'GET /buckets': () => json(200, []),
      'POST /buckets': () => problem(409, 'Bucket already exists', 'There is a bucket holiday.'),
    });
    render(<App />);

    fireEvent.change(await screen.findByLabelText('Bucket name'), { target: { value: 'holiday' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create bucket' }));

    // The screen is there to say why: it does, and no notice says it twice.
    expect((await screen.findByRole('alert')).textContent).toContain('Bucket already exists');
    expect(activity().getByRole('button', { name: 'Notices: 0' })).toBeDefined();
  });

  it('says how deleting an object ended when its screen was left meanwhile: the refusal, in the words of the API', async () => {
    const api = signedIn({
      ...bucketRoutes('photos', ['a.txt']),
      'DELETE /buckets/photos/object': () => problem(403, 'Access denied', 'Not yours to delete.'),
      'GET /users': () => json(200, []),
    });
    const deletes = hold('DELETE');
    window.location.hash = '#/buckets/photos';
    render(<App />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));
    await goTo('#/users', 'Users');
    expect(activity().getByRole('button', { name: 'Notices: 0' })).toBeDefined();

    deletes.release('photos/a.txt');
    await waitFor(() => {
      expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
    });
    expect(api.callsTo('DELETE', '/buckets/photos/object')).toHaveLength(1);
    const notices = openNotices();
    expect(notices.getByText(/a\.txt was not deleted from photos/)).toBeDefined();
    expect(notices.getByText('Access denied: Not yours to delete.')).toBeDefined();
    expect(notices.getByText('Not done:')).toBeDefined();
    // No alert on a screen that never asked.
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says how deleting the ticked objects ended when their screen was left meanwhile', async () => {
    const held = ['a.txt', 'b.txt', 'c.txt'];
    const api = signedIn({
      ...bucketRoutes('photos', held),
      'DELETE /buckets/photos/object': (call) => {
        held.splice(held.indexOf(call.query.get('key') ?? ''), 1);
        return noContent();
      },
      'GET /users': () => json(200, []),
    });
    const deletes = hold('DELETE');
    window.location.hash = '#/buckets/photos';
    render(<App />);

    fireEvent.click(await screen.findByLabelText('Select a.txt'));
    fireEvent.click(screen.getByLabelText('Select c.txt'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));
    await goTo('#/users', 'Users');

    // One request an object, one after the other: they go on though the screen is gone.
    deletes.release('photos/a.txt', 'photos/c.txt');
    await waitFor(() => {
      expect(activity().getByRole('button', { name: 'Notices: 1' })).toBeDefined();
    });
    expect(api.callsTo('DELETE', '/buckets/photos/object').map((call) => call.query.get('key'))).toEqual([
      'a.txt',
      'c.txt',
    ]);
    const notices = openNotices();
    expect(notices.getByText(/Deleted 2 objects from photos/)).toBeDefined();
    expect(notices.getByRole('link', { name: 'Open photos' }).getAttribute('href')).toBe('#/buckets/photos');
  });
});
