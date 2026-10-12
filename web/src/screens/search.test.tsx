import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { fakeApi, json, problem } from '../test/fakeApi';
import { Screens } from './Screens';
import { SearchScreen } from './SearchScreen';

const STORED = '2026-10-01T00:00:00Z';

function found(bucket: string, key: string, size = 10) {
  return { kind: 'object', bucket, key, size, etag: '"e"', last_modified: STORED };
}

function page(results: unknown[], more: Record<string, unknown> = {}) {
  return { results, match: 'substring', examined: results.length, complete: true, ...more };
}

/** The refusal of a search while another of the same caller has not ended, as the API sends it. */
function stillRunning(retryAfter?: string): Response {
  const body = {
    type: 'about:blank',
    title: 'Too many requests at once',
    status: 429,
    code: 'TooManyRequests',
    detail: 'a search of yours is still running',
  };
  const headers = new Headers({ 'Content-Type': 'application/problem+json' });
  if (retryAfter !== undefined) {
    headers.set('Retry-After', retryAfter);
  }
  return new Response(JSON.stringify(body), { status: 429, headers });
}

describe('search', () => {
  it('shows what was found, each with the ways to go to it', async () => {
    const api = fakeApi({
      'GET /search': () =>
        json(
          200,
          page(
            [
              { kind: 'bucket', bucket: 'reports' },
              found('reports', '2026/q1/annual report.pdf', 2048),
              found('photos', 'top.jpg'),
            ],
            { examined: 1234 },
          ),
        ),
    });
    render(<SearchScreen query="report" />);

    const table = within(await screen.findByRole('table'));
    const [, bucketRow, objectRow, topRow] = table.getAllByRole('row');
    if (bucketRow === undefined || objectRow === undefined || topRow === undefined) {
      throw new Error('the table has fewer rows than results');
    }

    // The query is in the field again, and was sent as it stands.
    expect((screen.getByLabelText('Search for') as HTMLInputElement).value).toBe('report');
    const [asked] = api.callsTo('GET', '/search');
    expect(asked?.query.get('q')).toBe('report');
    expect(api.callsTo('GET', '/search')).toHaveLength(1);

    // A bucket: its name, and the way into it.
    expect(bucketRow.textContent).toContain('Bucket');
    expect(bucketRow.textContent).toContain('reports');
    expect(within(bucketRow).getByRole('link', { name: 'Open bucket reports' }).getAttribute('href')).toBe('#/buckets/reports');
    expect(within(bucketRow).queryByRole('link', { name: /Download/ })).toBeNull();
    expect(within(bucketRow).queryByRole('link', { name: /folder/ })).toBeNull();

    // An object: its key, size and time, its bucket, its folder and itself.
    expect(objectRow.textContent).toContain('Object');
    expect(objectRow.textContent).toContain('2026/q1/annual report.pdf');
    expect(objectRow.textContent).toContain('2.0 KiB');
    expect(within(objectRow).getByRole('link', { name: 'Open bucket reports' }).getAttribute('href')).toBe('#/buckets/reports');
    const folder = within(objectRow).getByRole('link', { name: 'Open the folder of 2026/q1/annual report.pdf' });
    expect(folder.getAttribute('href')).toBe('#/buckets/reports?prefix=2026%2Fq1%2F');
    const download = within(objectRow).getByRole('link', { name: 'Download 2026/q1/annual report.pdf' });
    expect(download.getAttribute('href')).toBe('/api/v1/buckets/reports/object?key=2026%2Fq1%2Fannual+report.pdf');
    expect(download.hasAttribute('download')).toBe(true);

    // An object at the top of its bucket has no folder to open.
    expect(within(topRow).getByRole('link', { name: 'Open bucket photos' }).getAttribute('href')).toBe('#/buckets/photos');
    expect(within(topRow).queryByRole('link', { name: /folder/ })).toBeNull();
    expect(within(topRow).getByRole('link', { name: 'Download top.jpg' })).toBeDefined();

    // Every button says what it does, and in a row its icon is the button.
    for (const control of [...screen.getAllByRole('button'), ...screen.getAllByRole('link')]) {
      if (control.className.includes('btn')) {
        expect(control.getAttribute('title'), control.textContent).toBeTruthy();
      }
    }
    expect(folder.querySelector('.btn-text')?.textContent).toBe('Open folder');
    expect(folder.querySelector('i.bi')?.getAttribute('aria-hidden')).toBe('true');

    // How much was looked at, and how the API read the query. Nothing more to ask for.
    const figures = screen.getByRole('status').textContent;
    expect(figures).toContain('3 found.');
    expect(figures).toContain('Examined 1,234 keys');
    expect(figures).toContain('as text');
    expect(screen.queryByRole('button', { name: 'Search further' })).toBeNull();
  });

  it('explains the three ways to search under the field', async () => {
    fakeApi({ 'GET /search': () => json(200, page([], { match: 'regex' })) });
    render(<SearchScreen query="rx:^a" />);

    const hint = document.getElementById('search-hint');
    expect(hint?.textContent).toContain('text');
    expect(hint?.textContent).toContain('wild*card?');
    expect(hint?.textContent).toContain('rx:^regular expression$');
    expect(screen.getByLabelText('Search for').getAttribute('aria-describedby')).toBe('search-hint');
    // What the API says of how it read the query is what is shown.
    expect((await screen.findByText(/Examined/)).textContent).toContain('as a regular expression');
  });

  it('goes on where a search stopped and adds what it finds to what is there', async () => {
    const api = fakeApi({
      'GET /search': (call) => {
        switch (call.query.get('continuation')) {
          case null:
            return json(200, page([found('a', 'one.txt')], { examined: 50000, complete: false, next_continuation: 'token-1' }));
          case 'token-1':
            return json(200, page([], { examined: 50000, complete: false, next_continuation: 'token-2' }));
          case 'token-2':
            return json(200, page([found('b', 'dir/two.txt'), found('b', 'three.txt')], { examined: 7 }));
          default:
            return problem(400, 'Invalid request', 'continuation: it is not one this API gave out');
        }
      },
    });
    render(<SearchScreen query="t" />);

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(screen.getByRole('status').textContent).toContain('1 found so far. Examined 50,000 keys');

    // A page can be empty and not the last: the button stays.
    fireEvent.click(screen.getByRole('button', { name: 'Search further' }));
    await waitFor(() => {
      expect(screen.getByRole('status').textContent).toContain('Examined 100,000 keys');
    });
    expect(screen.getByRole('status').textContent).toContain('1 found so far.');

    fireEvent.click(screen.getByRole('button', { name: 'Search further' }));
    expect(await screen.findByRole('link', { name: 'Download dir/two.txt' })).toBeDefined();

    // Appended, in the order found; the first page is still there.
    const rows = within(screen.getByRole('table')).getAllByRole('row').slice(1);
    expect(rows.map((row) => within(row).getByRole('link', { name: /^Download / }).getAttribute('aria-label'))).toEqual([
      'Download one.txt',
      'Download dir/two.txt',
      'Download three.txt',
    ]);
    expect(screen.getByRole('status').textContent).toContain('3 found. Examined 100,007 keys');
    expect(screen.queryByRole('button', { name: 'Search further' })).toBeNull();

    // Each page was asked for with the token of the one before, and the same query.
    const calls = api.callsTo('GET', '/search');
    expect(calls.map((call) => call.query.get('continuation'))).toEqual([null, 'token-1', 'token-2']);
    expect(calls.map((call) => call.query.get('q'))).toEqual(['t', 't', 't']);
  });

  it('shows why a pattern was refused, beside the field, and searches again when it is put right', async () => {
    const api = fakeApi({
      'GET /search': (call) =>
        call.query.get('q') === 'rx:('
          ? problem(400, 'Invalid request', 'regular expression: error parsing regexp: missing closing ): `(`', 'req-9')
          : json(200, page([found('a', 'one.txt')])),
    });
    window.location.hash = '#/search?q=rx%3A%28';
    render(<Screens />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Invalid request');
    expect(alert.textContent).toContain('missing closing )');
    expect(alert.textContent).toContain('req-9');
    // In the panel of the field, not somewhere else on the screen - and the screen still stands.
    expect(screen.getByRole('search', { name: 'Search buckets and objects' }).parentElement?.contains(alert)).toBe(true);
    expect((screen.getByLabelText('Search for') as HTMLInputElement).value).toBe('rx:(');
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();

    fireEvent.change(screen.getByLabelText('Search for'), { target: { value: 'one' } });
    fireEvent.click(within(screen.getByRole('search', { name: 'Search buckets and objects' })).getByRole('button', { name: 'Search' }));

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(window.location.hash).toBe('#/search?q=one');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.callsTo('GET', '/search').map((call) => call.query.get('q'))).toEqual(['rx:(', 'one']);
  });

  it('says that nothing was found', async () => {
    fakeApi({ 'GET /search': () => json(200, page([], { examined: 42, match: 'wildcard' })) });
    render(<SearchScreen query="*.nothing" />);

    // By its words: while the first page is on its way, "Loading" is the status.
    const figures = await screen.findByText(/Examined/);
    expect(figures.getAttribute('role')).toBe('status');
    expect(figures.textContent).toContain('Nothing found.');
    expect(figures.textContent).toContain('Examined 42 keys');
    expect(figures.textContent).toContain('as a wildcard');
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Search further' })).toBeNull();
  });

  it('says so when a search is still running, and sends it again on request', async () => {
    let busy = true;
    const api = fakeApi({
      'GET /search': () =>
        busy
          ? problem(429, 'Too many requests at once', 'a search of yours is still running')
          : json(200, page([found('a', 'one.txt')])),
    });
    render(<SearchScreen query="one" />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Too many requests at once');
    expect(alert.textContent).toContain('a search of yours is still running');
    expect(screen.queryByRole('table')).toBeNull();

    busy = false;
    fireEvent.click(screen.getByRole('button', { name: 'Search again' }));

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.callsTo('GET', '/search')).toHaveLength(2);
  });

  it('shows a refusal, and one that comes while searching further keeps what was found', async () => {
    let refuse = false;
    fakeApi({
      'GET /search': () =>
        refuse
          ? problem(403, 'Access denied')
          : json(200, page([found('a', 'one.txt')], { complete: false, next_continuation: 'token-1' })),
    });
    render(<SearchScreen query="one" />);

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    refuse = true;
    fireEvent.click(screen.getByRole('button', { name: 'Search further' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.getByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Search further' })).toBeDefined();
  });

  it('shows a refusal of the search itself', async () => {
    fakeApi({ 'GET /search': () => problem(403, 'Access denied') });
    render(<SearchScreen query="one" />);

    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('gives up the search it no longer waits for when another is asked for', async () => {
    const api = fakeApi({
      'GET /search': (call) => json(200, page([found('a', `${call.query.get('q') ?? ''}.txt`)])),
    });
    window.location.hash = '#/search?q=one';
    render(<Screens />);

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    const [first] = api.callsTo('GET', '/search');
    expect(first?.signal.aborted).toBe(false);

    // The query is refined: the API runs one search at a time for a caller,
    // so the one that was on its way must not be left running.
    const form = within(screen.getByRole('search', { name: 'Search buckets and objects' }));
    fireEvent.change(form.getByLabelText('Search for'), { target: { value: 'two' } });
    fireEvent.click(form.getByRole('button', { name: 'Search' }));

    expect(await screen.findByRole('link', { name: 'Download two.txt' })).toBeDefined();
    const calls = api.callsTo('GET', '/search');
    expect(calls.map((call) => call.query.get('q'))).toEqual(['one', 'two']);
    expect(calls.map((call) => call.signal.aborted)).toEqual([true, false]);

    // The same search sent again gives up the one before it too. The
    // screen of another query is another screen: its form is looked up anew.
    fireEvent.click(
      within(screen.getByRole('search', { name: 'Search buckets and objects' })).getByRole('button', { name: 'Search' }),
    );
    await waitFor(() => {
      expect(api.callsTo('GET', '/search').map((call) => call.signal.aborted)).toEqual([true, true, false]);
    });
  });

  it('gives up its searches, further pages included, when the screen goes away', async () => {
    const api = fakeApi({
      'GET /search': () => json(200, page([found('a', 'one.txt')], { complete: false, next_continuation: 'token-1' })),
    });
    const { unmount } = render(<SearchScreen query="one" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Search further' }));
    await waitFor(() => {
      expect(api.callsTo('GET', '/search')).toHaveLength(2);
    });
    expect(api.callsTo('GET', '/search').map((call) => call.signal.aborted)).toEqual([false, false]);

    unmount();

    expect(api.callsTo('GET', '/search').map((call) => call.signal.aborted)).toEqual([true, true]);
  });

  it('forgets why a further page failed when the same search is sent again', async () => {
    let refuse = false;
    const api = fakeApi({
      'GET /search': () =>
        refuse
          ? problem(403, 'Access denied')
          : json(200, page([found('a', 'one.txt')], { complete: false, next_continuation: 'token-1' })),
    });
    render(<SearchScreen query="one" />);

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    refuse = true;
    fireEvent.click(screen.getByRole('button', { name: 'Search further' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');

    // The same query, sent again from the form: a new search, and the
    // failure of the old one's further page is not left standing over it.
    refuse = false;
    const form = within(screen.getByRole('search', { name: 'Search buckets and objects' }));
    fireEvent.click(form.getByRole('button', { name: 'Search' }));

    expect(screen.queryByRole('alert')).toBeNull();
    await waitFor(() => {
      expect(api.callsTo('GET', '/search')).toHaveLength(3);
    });
    expect(api.callsTo('GET', '/search').at(-1)?.query.has('continuation')).toBe(false);
    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('asks nothing before something was typed', async () => {
    const api = fakeApi();
    window.location.hash = '#/search';
    render(<Screens />);

    expect(await screen.findByRole('heading', { name: 'Search' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Results' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.calls).toHaveLength(0);
  });
});

describe('a search that is refused because the one before it has not ended', () => {
  // The clock is the test's, and goes on by itself as well: what the testing
  // library waits for with timers of its own still comes.
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('is asked for once more after the time the API names, and nothing is said of the refusal', async () => {
    const timers = vi.spyOn(globalThis, 'setTimeout');
    const waitingFor = (milliseconds: number) => timers.mock.calls.some((call) => call[1] === milliseconds);
    let refusals = 1;
    const api = fakeApi({
      'GET /search': () => {
        if (refusals > 0) {
          refusals -= 1;
          return stillRunning('2');
        }
        return json(200, page([found('a', 'one.txt')]));
      },
    });
    render(<SearchScreen query="one" />);

    // Refused: the screen waits the two seconds it was told, and says nothing.
    await waitFor(() => {
      expect(waitingFor(2000)).toBe(true);
    });
    expect(api.callsTo('GET', '/search')).toHaveLength(1);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('table')).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    const calls = api.callsTo('GET', '/search');
    expect(calls).toHaveLength(2);
    expect(calls.map((call) => call.query.get('q'))).toEqual(['one', 'one']);
  });

  it('is asked for once more and no oftener: a second refusal is shown', async () => {
    const timers = vi.spyOn(globalThis, 'setTimeout');
    const waitingFor = (milliseconds: number) => timers.mock.calls.some((call) => call[1] === milliseconds);
    let answer: () => Response = () => stillRunning();
    const api = fakeApi({ 'GET /search': () => answer() });
    render(<SearchScreen query="one" />);

    // The API named no time: one second. The testing library waits a
    // second of its own by default, with the same setTimeout; here it is
    // given another time, so that the second seen is the screen's.
    await waitFor(
      () => {
        expect(waitingFor(1000)).toBe(true);
      },
      { timeout: 4000 },
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Too many requests at once');
    expect(alert.textContent).toContain('a search of yours is still running');
    expect(api.callsTo('GET', '/search')).toHaveLength(2);

    // Sent again by hand, and told to wait a minute: three seconds are the most.
    let refusals = 1;
    answer = () => {
      if (refusals > 0) {
        refusals -= 1;
        return stillRunning('60');
      }
      return json(200, page([found('a', 'one.txt')]));
    };
    expect(waitingFor(3000)).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Search again' }));
    await waitFor(() => {
      expect(waitingFor(3000)).toBe(true);
    });
    expect(waitingFor(60000)).toBe(false);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    expect(await screen.findByRole('link', { name: 'Download one.txt' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.callsTo('GET', '/search')).toHaveLength(4);
  });

  it('is not asked for again when it was given up during the wait', async () => {
    const timers = vi.spyOn(globalThis, 'setTimeout');
    const waitingFor = (milliseconds: number) => timers.mock.calls.some((call) => call[1] === milliseconds);
    const api = fakeApi({
      'GET /search': (call) =>
        call.query.get('q') === 'one' ? stillRunning('2') : json(200, page([found('a', 'two.txt')])),
    });
    window.location.hash = '#/search?q=one';
    render(<Screens />);

    await waitFor(() => {
      expect(waitingFor(2000)).toBe(true);
    });
    expect(api.callsTo('GET', '/search')).toHaveLength(1);

    // While the screen waits, the user asks for something else.
    const form = within(screen.getByRole('search', { name: 'Search buckets and objects' }));
    fireEvent.change(form.getByLabelText('Search for'), { target: { value: 'two' } });
    fireEvent.click(form.getByRole('button', { name: 'Search' }));
    expect(await screen.findByRole('link', { name: 'Download two.txt' })).toBeDefined();

    // The wait is over long since: the search that was given up was not
    // sent again, and nothing is shown of it - no result and no refusal.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(api.callsTo('GET', '/search').map((call) => call.query.get('q'))).toEqual(['one', 'two']);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('link', { name: 'Download one.txt' })).toBeNull();
    expect(screen.getByRole('link', { name: 'Download two.txt' })).toBeDefined();
  });

  it('is not asked for again when the screen went away during the wait', async () => {
    const timers = vi.spyOn(globalThis, 'setTimeout');
    const waitingFor = (milliseconds: number) => timers.mock.calls.some((call) => call[1] === milliseconds);
    const api = fakeApi({ 'GET /search': () => stillRunning('2') });
    const { unmount } = render(<SearchScreen query="one" />);

    await waitFor(() => {
      expect(waitingFor(2000)).toBe(true);
    });
    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });

    expect(api.callsTo('GET', '/search')).toHaveLength(1);
  });
});
