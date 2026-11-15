import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { CSRF_TOKEN, fakeApi, json, noContent, problem, refusal, signedIn, type Call } from '../test/fakeApi';
import { BucketsScreen, QUOTA_HINT, QUOTA_ROUGH_HINT } from './BucketsScreen';

const CREATED = '2026-10-01T00:00:00Z';
const FIRST = '2026-10-02T08:00:00Z';
const SECOND = '2026-10-03T09:30:00Z';
const THIRD = '2026-10-04T10:45:00Z';
const FOURTH = '2026-10-05T12:00:00Z';

/** A time as the console shows it. */
function when(value: string): string {
  return new Date(value).toLocaleString();
}

/** What the API says a bucket holds. */
function usage(count: number, size: number, complete = true) {
  return json(200, { object_count: count, total_size: size, complete });
}

/** A bucket of the list, with what the API tells of it beyond its name. */
function bucket(name: string, details?: Record<string, unknown>) {
  return details === undefined ? { name, created_at: CREATED } : { name, created_at: CREATED, details };
}

const NOTHING = { access_included: false, policy_included: false };

/** The row of a bucket: the one its "Open" button stands in. */
function rowOf(table: HTMLElement, name: string): HTMLTableRowElement {
  return within(table).getByRole('link', { name: `Open ${name}` }).closest('tr') as HTMLTableRowElement;
}

/** The cell of a row under a heading, or null when the row has none. */
function cellOf(row: HTMLTableRowElement, label: string): HTMLTableCellElement | null {
  return row.querySelector<HTMLTableCellElement>(`td[data-label="${label}"]`);
}

function headings(table: HTMLElement): (string | null)[] {
  return within(table)
    .getAllByRole('columnheader')
    .map((th) => th.textContent);
}

describe('the grid of buckets', () => {
  it('is a table that scrolls inside its panel, with the columns in their order', async () => {
    const api = fakeApi({
      'GET /buckets': () =>
        json(200, [
          bucket('photos', {
            access_included: true,
            created_by: 'user/alice',
            activity: { uploads: [], downloads: [] },
            policy_included: true,
          }),
        ]),
      'GET /buckets/photos/usage': () => usage(1, 3),
    });
    render(<BucketsScreen />);

    const table = await screen.findByRole('table');
    expect(headings(table)).toEqual([
      'Name',
      'Objects',
      'Size (GB)',
      'Quota',
      'Created',
      'Created by',
      'Policy',
      'Last upload',
      'Last download',
      'Actions',
    ]);
    // A table on a phone too, wide, and scrolled inside its panel: the page
    // is not.
    expect(table.className).toContain('grid');
    expect(table.className).toContain('grid-keep');
    expect(table.className).toContain('grid-wide');
    expect(table.parentElement?.className).toContain('table-responsive');
    expect(table.parentElement?.className).toContain('grid-scroll');
    // The figures stand in columns of figures.
    expect(within(table).getByRole('columnheader', { name: 'Objects' }).className).toContain('grid-number');
    expect(within(table).getByRole('columnheader', { name: 'Size (GB)' }).className).toContain('grid-number');
    expect(within(table).getByRole('columnheader', { name: 'Created' }).className).not.toContain('grid-number');
    // No tiles any more.
    expect(document.querySelector('.tile')).toBeNull();
    expect(screen.queryByRole('article')).toBeNull();

    // The list was asked for once, with everything the caller may be told.
    expect(api.callsTo('GET', '/buckets')).toHaveLength(1);
    expect(api.callsTo('GET', '/buckets')[0]?.query.get('details')).toBe('true');

    const row = rowOf(table, 'photos');
    expect(within(row).getByRole('link', { name: 'photos' }).getAttribute('href')).toBe('#/buckets/photos');
    expect(cellOf(row, 'Created')?.textContent).toBe(when(CREATED));
    await within(row).findByText('< 0.01 GB');
    expect(within(row).getAllByRole('cell')).toHaveLength(10);
  });

  it('counts each bucket when its row is shown, and shows the size in GB with the bytes as a hint', async () => {
    const api = fakeApi({
      'GET /buckets': () =>
        json(200, [
          bucket('photos', NOTHING),
          bucket('small', NOTHING),
          bucket('empty', NOTHING),
          bucket('huge', NOTHING),
          bucket('private', NOTHING),
          bucket('later', NOTHING),
        ]),
      'GET /buckets/photos/usage': () => usage(1234, 5 * 1024 ** 3),
      'GET /buckets/small/usage': () => usage(1, 3),
      'GET /buckets/empty/usage': () => usage(0, 0),
      'GET /buckets/huge/usage': () => usage(1000000, 250_000_000_000, false),
      'GET /buckets/private/usage': () => problem(403, 'Access denied'),
    });
    let down = true;
    api.on({ 'GET /buckets/later/usage': () => (down ? problem(503, 'No metadata leader') : usage(2, 1_500_000_000)) });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');

    const photos = rowOf(table, 'photos');
    await within(photos).findByText('1,234');
    expect(cellOf(photos, 'Objects')?.className).toContain('grid-number');
    const size = cellOf(photos, 'Size (GB)');
    expect(size?.textContent).toBe('5.37 GB');
    expect(size?.className).toContain('grid-number');
    expect(size?.getAttribute('title')).toBe('5,368,709,120 bytes');

    // Too little to show in GB is said to be less than what can be shown.
    const small = rowOf(table, 'small');
    await within(small).findByText('< 0.01 GB');
    expect(cellOf(small, 'Size (GB)')?.getAttribute('title')).toBe('3 bytes');
    expect(cellOf(small, 'Objects')?.textContent).toBe('1');
    // And nothing at all is nothing.
    const empty = rowOf(table, 'empty');
    await within(empty).findByText('0 GB');
    expect(cellOf(empty, 'Objects')?.textContent).toBe('0');

    // A count that stopped at its bound says so, in both figures.
    const huge = rowOf(table, 'huge');
    await within(huge).findByText('1,000,000');
    expect(cellOf(huge, 'Objects')?.textContent).toBe('≥ at least 1,000,000');
    expect(cellOf(huge, 'Objects')?.getAttribute('title')).toContain('1,000,000 or more');
    expect(cellOf(huge, 'Size (GB)')?.textContent).toBe('≥ at least 250 GB');
    expect(cellOf(huge, 'Size (GB)')?.getAttribute('title')).toContain('250,000,000,000 bytes or more');

    // Whoever may not list a bucket is shown no figure, and no alarm.
    const hidden = rowOf(table, 'private');
    expect(await within(hidden).findByText('Usage not shown')).toBeDefined();
    expect(cellOf(hidden, 'Objects')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();

    // A figure that could not be had just now can be asked for again.
    const later = rowOf(table, 'later');
    expect(await within(later).findByText(/Usage unavailable/)).toBeDefined();
    const again = within(later).getByRole('button', { name: 'Count again' });
    expect(again.getAttribute('title')).toBe('Count the objects of this bucket again');
    down = false;
    fireEvent.click(again);
    await within(later).findByText('1.50 GB');
    expect(cellOf(later, 'Objects')?.textContent).toBe('2');

    // Every bucket was asked about by itself, once - and "later" once more.
    for (const name of ['photos', 'small', 'empty', 'huge', 'private']) {
      expect(api.callsTo('GET', `/buckets/${name}/usage`), name).toHaveLength(1);
    }
    expect(api.callsTo('GET', '/buckets/later/usage')).toHaveLength(2);
    expect(api.callsTo('GET', '/buckets')).toHaveLength(1);
  });

  it('shows who created a bucket, what its policy is and who used it last', async () => {
    fakeApi({
      'GET /buckets': () =>
        json(200, [
          // Of before creators were kept, without a policy, and never used.
          bucket('archive', {
            access_included: true,
            activity: { uploads: [], downloads: [] },
            policy_included: true,
          }),
          // The caller may not list it: the API names no object.
          bucket('named', {
            access_included: true,
            created_by: 'root',
            activity: { uploads: [{ at: FIRST, by: 'user/carol' }], downloads: [] },
            policy_included: true,
            policy: { statements: 1, open_to_anyone: false },
          }),
          bucket('photos', {
            access_included: true,
            created_by: 'user/alice',
            activity: {
              uploads: [
                { at: THIRD, by: 'user/alice', key: '2026/a.jpg' },
                { at: SECOND, by: 'root', key: 'b.jpg' },
                // Uploaded before uploaders were recorded.
                { at: FIRST, by: '', key: 'old.jpg' },
              ],
              downloads: [
                { at: FOURTH, by: 'user/bob', key: '2026/a.jpg', presigned: true },
                { at: SECOND, by: 'anonymous', key: 'b.jpg' },
              ],
            },
            policy_included: true,
            policy: { statements: 2, open_to_anyone: true },
          }),
          // One the API tells this caller nothing of.
          bucket('secret', NOTHING),
        ]),
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');

    const photos = rowOf(table, 'photos');
    expect(cellOf(photos, 'Created by')?.textContent).toBe('user/alice');
    // The policy: how many statements, and - in words, not by a colour
    // alone - that it allows something to every caller.
    const policy = cellOf(photos, 'Policy') as HTMLTableCellElement;
    expect(within(policy).getByText('2 statements').className).toContain('badge');
    expect(within(policy).getByText('open to all callers').className).toContain('badge');
    // No screen edits the policy of a bucket: the cell leads nowhere.
    expect(within(policy).queryByRole('link')).toBeNull();

    // The newest upload: when, who and which object.
    const upload = cellOf(photos, 'Last upload') as HTMLTableCellElement;
    const newest = upload.querySelector('.activity-line') as HTMLElement;
    expect(newest.textContent).toContain(`${when(THIRD)} · user/alice`);
    expect(newest.textContent).toContain('2026/a.jpg');
    // The two before it: in the hint of the cell, and behind a line that
    // opens without a pointer.
    expect(upload.getAttribute('title')).toBe(
      `Before that:\n${when(SECOND)} · root · b.jpg\n${when(FIRST)} · not recorded · old.jpg`,
    );
    const earlier = upload.querySelector('details') as HTMLDetailsElement;
    expect(earlier.querySelector('summary')?.textContent).toBe('2 earlier');
    // (Whether a test's document hides what a closed line holds is its own affair.)
    const lines = within(earlier)
      .getAllByRole('listitem', { hidden: true })
      .map((item) => item.textContent);
    expect(lines).toHaveLength(2);
    expect(lines[0]).toContain(`${when(SECOND)} · root`);
    expect(lines[0]).toContain('b.jpg');
    expect(lines[1]).toContain(`${when(FIRST)} · not recorded`);
    expect(lines[1]).toContain('old.jpg');

    // The newest download was made with a URL somebody signed: it says so.
    const download = cellOf(photos, 'Last download') as HTMLTableCellElement;
    const read = download.querySelector('.activity-line') as HTMLElement;
    expect(read.textContent).toContain(`${when(FOURTH)} · user/bob`);
    expect(within(read).getByText('presigned URL')).toBeDefined();
    expect(read.textContent).toContain('2026/a.jpg');
    expect(download.getAttribute('title')).toBe(`Before that:\n${when(SECOND)} · anonymous · b.jpg`);
    expect(download.querySelector('summary')?.textContent).toBe('1 earlier');
    expect(within(download.querySelector('details') as HTMLElement).queryByText('presigned URL')).toBeNull();

    // A bucket nobody used, of before creators were kept, without a policy.
    const archive = rowOf(table, 'archive');
    expect(cellOf(archive, 'Created by')?.textContent).toBe('not recorded');
    expect(cellOf(archive, 'Policy')?.textContent).toBe('none');
    expect(cellOf(archive, 'Last upload')?.textContent).toBe('never');
    expect(cellOf(archive, 'Last download')?.textContent).toBe('never');
    expect(archive.querySelector('details')).toBeNull();

    // A policy that names users only, and an upload whose object is not named.
    const named = rowOf(table, 'named');
    expect(cellOf(named, 'Created by')?.textContent).toBe('root');
    expect(cellOf(named, 'Policy')?.textContent).toBe('1 statement');
    expect(cellOf(named, 'Last upload')?.textContent).toBe(`${when(FIRST)} · user/carol`);
    expect(cellOf(named, 'Last upload')?.getAttribute('title')).toBeNull();
    expect(named.querySelector('.activity-key')).toBeNull();
    expect(cellOf(named, 'Last download')?.textContent).toBe('never');

    // What the API left out stays out: the cells are there, and empty.
    const secret = rowOf(table, 'secret');
    for (const label of ['Created by', 'Policy', 'Last upload', 'Last download']) {
      expect(cellOf(secret, label)?.textContent, label).toBe('');
    }
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows none of those columns when the API tells them of no bucket', async () => {
    const api = fakeApi({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING), bucket('site', NOTHING)]),
      'GET /buckets/photos/usage': () => usage(1, 3),
      'GET /buckets/site/usage': () => usage(2, 4),
    });
    render(<BucketsScreen />);

    const table = await screen.findByRole('table');
    expect(headings(table)).toEqual(['Name', 'Objects', 'Size (GB)', 'Quota', 'Created', 'Actions']);
    for (const label of ['Created by', 'Policy', 'Last upload', 'Last download']) {
      expect(table.querySelector(`[data-label="${label}"]`), label).toBeNull();
    }
    expect(table.className).toContain('grid-keep');
    expect(table.className).not.toContain('grid-wide');
    // No empty columns either: a row has the six cells of its headings.
    await within(rowOf(table, 'photos')).findByText('< 0.01 GB');
    await within(rowOf(table, 'site')).findByText('< 0.01 GB');
    for (const row of within(table).getAllByRole('row').slice(1)) {
      expect(within(row).getAllByRole('cell')).toHaveLength(6);
    }
    // It was asked for all the same: the API said no, and that is no failure.
    expect(api.callsTo('GET', '/buckets')[0]?.query.get('details')).toBe('true');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows the policy alone to who may see no more, and who uses the buckets alone likewise', async () => {
    let answer: unknown = [
      bucket('photos', { access_included: false, policy_included: true, policy: { statements: 3, open_to_anyone: false } }),
      bucket('site', { access_included: false, policy_included: false }),
    ];
    fakeApi({ 'GET /buckets': () => json(200, answer) });
    const first = render(<BucketsScreen />);

    let table = await screen.findByRole('table');
    expect(headings(table)).toEqual(['Name', 'Objects', 'Size (GB)', 'Quota', 'Created', 'Policy', 'Actions']);
    expect(cellOf(rowOf(table, 'photos'), 'Policy')?.textContent).toBe('3 statements');
    expect(cellOf(rowOf(table, 'site'), 'Policy')?.textContent).toBe('');
    first.unmount();

    answer = [
      bucket('photos', { access_included: true, created_by: 'root', activity: { uploads: [], downloads: [] }, policy_included: false }),
    ];
    render(<BucketsScreen />);
    table = await screen.findByRole('table');
    expect(headings(table)).toEqual([
      'Name',
      'Objects',
      'Size (GB)',
      'Quota',
      'Created',
      'Created by',
      'Last upload',
      'Last download',
      'Actions',
    ]);
    expect(cellOf(rowOf(table, 'photos'), 'Created by')?.textContent).toBe('root');
  });

  it('shows the plain list of a node that says nothing of details', async () => {
    // During a rolling upgrade the console of this release may be answered
    // by a node of the one before.
    fakeApi({
      'GET /buckets': () => json(200, [bucket('photos'), bucket('site')]),
      'GET /buckets/photos/usage': () => usage(10, 2_000_000_000),
    });
    render(<BucketsScreen />);

    const table = await screen.findByRole('table');
    expect(headings(table)).toEqual(['Name', 'Objects', 'Size (GB)', 'Quota', 'Created', 'Actions']);
    const photos = rowOf(table, 'photos');
    await within(photos).findByText('2.00 GB');
    expect(cellOf(photos, 'Objects')?.textContent).toBe('10');
    expect(screen.getByRole('link', { name: 'Open site' }).getAttribute('href')).toBe('#/buckets/site');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('does not fail for details that say less than they should', async () => {
    // Included, and then not given: shown as not told, never as an error.
    fakeApi({
      'GET /buckets': () => json(200, [bucket('photos', { access_included: true, policy_included: true })]),
    });
    render(<BucketsScreen />);

    const table = await screen.findByRole('table');
    const photos = rowOf(table, 'photos');
    expect(cellOf(photos, 'Created by')?.textContent).toBe('not recorded');
    expect(cellOf(photos, 'Policy')?.textContent).toBe('none');
    expect(cellOf(photos, 'Last upload')?.textContent).toBe('');
    expect(cellOf(photos, 'Last download')?.textContent).toBe('');
  });

  it('gives the buttons of a row an icon, their name to be read out and a hint', async () => {
    fakeApi({ 'GET /buckets': () => json(200, [bucket('photos', NOTHING)]) });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');
    const row = rowOf(table, 'photos');

    const open = within(row).getByRole('link', { name: 'Open photos' });
    expect(open.getAttribute('href')).toBe('#/buckets/photos');
    expect(open.getAttribute('title')).toBe('Open this bucket');
    expect(open.querySelector('.btn-text')?.textContent).toBe('Open');
    expect(open.querySelector('i.bi')).not.toBeNull();
    const remove = within(row).getByRole('button', { name: 'Delete' });
    expect(remove.getAttribute('title')).toContain('Delete bucket photos?');
    expect(remove.querySelector('.btn-text')?.textContent).toBe('Delete');
    expect(remove.querySelector('i.bi')).not.toBeNull();
    expect(remove.closest('td')?.getAttribute('data-label')).toBe('Actions');
    for (const control of [...within(table).getAllByRole('button'), ...within(table).getAllByRole('link')]) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
  });

  it('deletes a bucket from its row only after the question is answered', async () => {
    let buckets = [bucket('photos', NOTHING), bucket('site', NOTHING)];
    const api = fakeApi({
      'GET /buckets': () => json(200, buckets),
      'DELETE /buckets/site': () => {
        buckets = [bucket('photos', NOTHING)];
        return noContent();
      },
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');

    fireEvent.click(within(rowOf(table, 'site')).getByRole('button', { name: 'Delete' }));
    expect(screen.getByText('Delete bucket site?')).toBeDefined();
    expect(api.callsTo('DELETE', '/buckets/site')).toHaveLength(0);
    // Talked out of it: nothing is sent.
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(api.calls.every((call) => call.method === 'GET')).toBe(true);

    fireEvent.click(within(rowOf(table, 'site')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'Open site' })).toBeNull();
    });
    expect(api.callsTo('DELETE', '/buckets/site')).toHaveLength(1);
    expect(api.callsTo('DELETE', '/buckets/photos')).toHaveLength(0);
    expect(screen.getByRole('link', { name: 'Open photos' })).toBeDefined();
    // The list was read again, with its details.
    const lists = api.callsTo('GET', '/buckets');
    expect(lists).toHaveLength(2);
    expect(lists[1]?.query.get('details')).toBe('true');
  });
});

/** What the API says a bucket holds where it keeps the exact count: with the quota, when it has one. */
function counted(stored: number, uploads: number, quota?: number) {
  return json(200, {
    object_count: 3,
    total_size: stored,
    complete: true,
    exact: true,
    versions: 4,
    stored_bytes: stored,
    upload_bytes: uploads,
    quota_bytes: quota,
  });
}

/** What the API answers the read of a bucket's quota. */
function quotaOf(used: number, quota?: number, exact = true) {
  return json(200, { quota_bytes: quota, used_bytes: exact ? used : 0, exact });
}

/** The quota of the bucket of a row, opened from the row. */
async function openQuota(name: string): Promise<HTMLElement> {
  const table = await screen.findByRole('table');
  fireEvent.click(within(rowOf(table, name)).getByRole('button', { name: `Quota of ${name}` }));
  return screen.getByRole('region', { name: `Quota of ${name}` });
}

/** The body of a request, read as JSON. */
function bodyOf(call: Call | undefined): unknown {
  return JSON.parse(call?.body ?? 'null');
}

/** That a change was sent as every change of a session is: with the CSRF token and an Idempotency-Key of its own. */
function expectSentAsAChange(call: Call | undefined) {
  expect(call?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
  expect(call?.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
}

/** The field of the quota editor of bucket photos. */
const FIELD = 'Quota of bucket photos in GB';

describe('the quota of a bucket', () => {
  it('shows the quota as a bar of what counts against it over the quota, "not set" without one and "not counted yet" while usage is not exact', async () => {
    fakeApi({
      'GET /buckets': () =>
        json(200, [
          bucket('photos', NOTHING),
          bucket('open', NOTHING),
          bucket('older', NOTHING),
          bucket('before', NOTHING),
          bucket('over', NOTHING),
        ]),
      // Versions of 6 GB and parts of 1.5 GB, against 10 GB.
      'GET /buckets/photos/usage': () => counted(6_000_000_000, 1_500_000_000, 10_000_000_000),
      'GET /buckets/open/usage': () => counted(2_000_000_000, 0),
      // Not counted yet: made before the count and being counted, or on a
      // deployment that does not count yet - the API says only that.
      'GET /buckets/older/usage': () => json(200, { object_count: 7, total_size: 70, complete: true, exact: false }),
      // A node of a release before quotas says nothing of exactness.
      'GET /buckets/before/usage': () => usage(7, 70),
      // A quota set below what the bucket held.
      'GET /buckets/over/usage': () => counted(13_000_000_000, 0, 10_000_000_000),
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');
    expect(within(table).getByRole('columnheader', { name: 'Quota' }).className).not.toContain('grid-number');

    const photos = rowOf(table, 'photos');
    await within(photos).findByText('75 %');
    const cell = cellOf(photos, 'Quota') as HTMLTableCellElement;
    const figures = '7.50 GB of 10.00 GB used (75 %): 7,500,000,000 bytes of 10,000,000,000 bytes';
    // The figures are in the hint, and read out in words: the bar and the
    // share are for the eye.
    expect(cell.getAttribute('title')).toBe(figures);
    expect(within(cell).getByText(figures).className).toBe('visually-hidden');
    const bar = cell.querySelector('meter') as HTMLMeterElement;
    expect(bar.className).toBe('quota-bar');
    expect(bar.getAttribute('min')).toBe('0');
    expect(bar.getAttribute('max')).toBe('10000000000');
    expect(bar.getAttribute('value')).toBe('7500000000');
    expect(bar.getAttribute('aria-hidden')).toBe('true');
    // The size column is still the current versions alone.
    expect(cellOf(photos, 'Size (GB)')?.textContent).toBe('6.00 GB');

    const open = rowOf(table, 'open');
    await within(open).findByText('2.00 GB');
    expect(cellOf(open, 'Quota')?.textContent).toBe('not set');
    expect(cellOf(open, 'Quota')?.className).toContain('grid-note');
    expect(cellOf(open, 'Quota')?.querySelector('meter')).toBeNull();

    const older = rowOf(table, 'older');
    await within(older).findByText('not counted yet');
    expect(cellOf(older, 'Quota')?.className).toContain('grid-note');
    expect(cellOf(older, 'Quota')?.getAttribute('title')).toBe(
      'The usage of this bucket is not counted yet: its quota is shown once it is',
    );

    const before = rowOf(table, 'before');
    await within(before).findByText('7');
    expect(cellOf(before, 'Quota')?.textContent).toBe('');

    // Beyond the quota: the bar is full, the share says by how much.
    const over = rowOf(table, 'over');
    await within(over).findByText('130 %');
    const full = cellOf(over, 'Quota')?.querySelector('meter') as HTMLMeterElement;
    expect(full.getAttribute('value')).toBe('10000000000');
    expect(cellOf(over, 'Quota')?.getAttribute('title')).toBe(
      '13.00 GB of 10.00 GB used (130 %): 13,000,000,000 bytes of 10,000,000,000 bytes',
    );

    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says nothing of a quota where the usage is not shown, in the cell that says so', async () => {
    fakeApi({
      'GET /buckets': () => json(200, [bucket('private', NOTHING)]),
      'GET /buckets/private/usage': () => problem(403, 'Access denied'),
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');
    const row = rowOf(table, 'private');
    const note = await within(row).findByText('Usage not shown');
    // One cell across the objects, the size and the quota.
    expect(note.closest('td')?.getAttribute('colspan')).toBe('3');
    expect(cellOf(row, 'Quota')).toBeNull();
  });

  it('opens from the row, sets a quota in GB with the CSRF token and an Idempotency-Key, and shows what the API kept', async () => {
    let quota: number | undefined;
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING), bucket('site', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(1_200_000_000, 0, quota),
      'GET /buckets/site/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () => quotaOf(1_200_000_000, quota),
      'PUT /buckets/photos/quota': (call) => {
        quota = (JSON.parse(call.body) as { quota_bytes: number }).quota_bytes;
        return quotaOf(1_200_000_000, quota);
      },
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');
    await within(rowOf(table, 'photos')).findByText('not set');

    const button = within(rowOf(table, 'photos')).getByRole('button', { name: 'Quota of photos' });
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(button.getAttribute('title')).toBe('Show the quota of this bucket, and change it');
    expect(button.querySelector('i.bi')).not.toBeNull();
    // Nothing is read before it is asked for.
    expect(api.callsTo('GET', '/buckets/photos/quota')).toHaveLength(0);

    const region = await openQuota('photos');
    expect(button.getAttribute('aria-expanded')).toBe('true');
    expect(button.getAttribute('aria-controls')).toBe(region.id);
    // In a row of its own under the bucket's, across the whole grid.
    const detail = region.closest('tr') as HTMLTableRowElement;
    expect(detail.className).toBe('grid-detail');
    expect(detail.previousElementSibling).toBe(rowOf(table, 'photos'));
    expect(detail.querySelector('td')?.getAttribute('colspan')).toBe(String(headings(table).length));

    await within(region).findByText(/This bucket has no quota\./);
    expect(within(region).getByText(/Counted against it: 1\.20 GB \(1,200,000,000 bytes\)\./)).toBeDefined();
    const field = within(region).getByLabelText(FIELD) as HTMLInputElement;
    expect(field.value).toBe('');
    // A keyboard with ".": a decimal keypad of a comma locale has only ",".
    expect(field.getAttribute('inputmode')).toBe('text');
    expect(document.getElementById(field.getAttribute('aria-describedby') ?? '')?.textContent).toBe(QUOTA_HINT);
    // There is no quota to remove.
    expect(within(region).queryByRole('button', { name: 'Remove quota' })).toBeNull();
    // The other bucket's quota stays closed.
    expect(screen.queryByRole('region', { name: 'Quota of site' })).toBeNull();

    fireEvent.change(field, { target: { value: '2.5' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));

    await within(region).findByText('Saved: bucket photos may hold 2.50 GB (2,500,000,000 bytes).');
    const puts = api.callsTo('PUT', '/buckets/photos/quota');
    expect(puts).toHaveLength(1);
    expect(puts[0]?.body).toBe('{"quota_bytes":2500000000}');
    expect(puts[0]?.headers.get('Content-Type')).toBe('application/json');
    expectSentAsAChange(puts[0]);
    // What the API kept is what is shown, and the keyboard is on Save again.
    expect(within(region).getByText(/Quota: 2\.50 GB \(2,500,000,000 bytes\)\./)).toBeDefined();
    expect((within(region).getByLabelText(FIELD) as HTMLInputElement).value).toBe('2.5');
    expect(document.activeElement).toBe(within(region).getByRole('button', { name: 'Save quota' }));
    expect(within(region).getByRole('button', { name: 'Remove quota' })).toBeDefined();

    // The row reads the usage again, and its bar shows the new quota.
    const photos = rowOf(table, 'photos');
    await within(photos).findByText('48 %');
    expect(api.callsTo('GET', '/buckets/photos/usage')).toHaveLength(2);
    expect(api.callsTo('GET', '/buckets/site/usage')).toHaveLength(1);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('begins with the quota there is, exactly, and removes it only after the question is answered', async () => {
    let quota: number | undefined = 10_737_418_240;
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0, quota),
      'GET /buckets/photos/quota': () => quotaOf(0, quota),
      'DELETE /buckets/photos/quota': () => {
        quota = undefined;
        return noContent();
      },
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');

    await within(region).findByText(/Quota: 10\.74 GB \(10,737,418,240 bytes\)\./);
    // 10 GiB, in GB as the API keeps it: saved unchanged, it is the same quota.
    expect((within(region).getByLabelText(FIELD) as HTMLInputElement).value).toBe('10.73741824');

    fireEvent.click(within(region).getByRole('button', { name: 'Remove quota' }));
    expect(within(region).getByText('Remove the quota of bucket photos?')).toBeDefined();
    // Talked out of it: nothing is sent.
    fireEvent.click(within(region).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('DELETE', '/buckets/photos/quota')).toHaveLength(0);

    fireEvent.click(within(region).getByRole('button', { name: 'Remove quota' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Yes, remove quota' }));

    await within(region).findByText('Removed the quota of bucket photos.');
    const deletes = api.callsTo('DELETE', '/buckets/photos/quota');
    expect(deletes).toHaveLength(1);
    expectSentAsAChange(deletes[0]);
    expect(within(region).getByText(/This bucket has no quota\./)).toBeDefined();
    expect((within(region).getByLabelText(FIELD) as HTMLInputElement).value).toBe('');
    expect(within(region).queryByRole('button', { name: 'Remove quota' })).toBeNull();
    expect(document.activeElement).toBe(within(region).getByRole('button', { name: 'Save quota' }));
    // The row reads the usage again: there is no quota any more.
    await within(rowOf(screen.getByRole('table'), 'photos')).findByText('not set');
  });

  it('shows no editor to whoever may not read the quota, and no alarm', async () => {
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0, 5_000_000_000),
      'GET /buckets/photos/quota': () => refusal(403, 'AccessDenied', 'Access denied'),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');

    // The bar is the usage's to show: whoever may read the usage is told the quota.
    await within(rowOf(screen.getByRole('table'), 'photos')).findByText('0 %');
    expect(await within(region).findByText('You may not see the quota of this bucket.')).toBeDefined();
    expect(within(region).queryByRole('textbox')).toBeNull();
    expect(within(region).queryByRole('button', { name: 'Save quota' })).toBeNull();
    expect(within(region).queryByRole('button', { name: 'Remove quota' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.calls.every((call) => call.method === 'GET')).toBe(true);
  });

  it('says why a change was refused through the notice - 403 and 409 - and keeps what was typed', async () => {
    let answer = () => refusal(403, 'AccessDenied', 'Access denied');
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () => quotaOf(0),
      'PUT /buckets/photos/quota': () => answer(),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');
    const field = (await within(region).findByLabelText(FIELD)) as HTMLInputElement;
    fireEvent.change(field, { target: { value: '100' } });

    // A caller without admin:PutBucketQuota: the API says no, and is quoted.
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    const denied = await within(region).findByRole('alert');
    expect(denied.textContent).toContain('You may not change the quota of bucket photos.');
    expect(denied.textContent).toContain('The server said: Access denied');
    expect(field.value).toBe('100');
    expect(bodyOf(api.callsTo('PUT', '/buckets/photos/quota')[0])).toEqual({ quota_bytes: 100_000_000_000 });

    // A bucket whose usage is still being counted.
    answer = () => refusal(409, 'Conflict', 'Conflict');
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    await waitFor(() => {
      expect(within(region).getByRole('alert').textContent).toContain(
        'The usage of bucket photos is still being counted: a quota can be set once it is. Try again later.',
      );
    });
    expect(within(region).getByRole('alert').textContent).toContain('The server said: Conflict');
    expect(field.value).toBe('100');

    // A quota the API does not take: 0 is sent, for the API to refuse.
    answer = () => refusal(400, 'InvalidArgument', 'Invalid argument');
    fireEvent.change(field, { target: { value: '0' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    await waitFor(() => {
      expect(within(region).getByRole('alert').textContent).toContain(
        'The quota was refused: bucket photos keeps the quota it had.',
      );
    });
    expect(api.callsTo('PUT', '/buckets/photos/quota')).toHaveLength(3);
    expect(bodyOf(api.callsTo('PUT', '/buckets/photos/quota')[2])).toEqual({ quota_bytes: 0 });
  });

  it('says while the usage is counted that a quota waits for it, and leaves the refusal to the API', async () => {
    signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => json(200, { object_count: 1, total_size: 1, complete: true, exact: false }),
      'GET /buckets/photos/quota': () => quotaOf(0, undefined, false),
      'PUT /buckets/photos/quota': () => refusal(409, 'Conflict', 'Conflict'),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');

    await within(region).findByText('The usage of this bucket is not counted yet: a quota can be set once it is.');
    // What counts against it is not known yet: no figure is made up.
    expect(within(region).queryByText(/Counted against it/)).toBeNull();
    // The editor is there all the same: the API is the one that refuses.
    fireEvent.change(within(region).getByLabelText(FIELD), { target: { value: '1' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    expect((await within(region).findByRole('alert')).textContent).toContain('still being counted');
  });

  // A fresh screen for each: an alert left by the input before would match.
  it.each(['1,5', '2 GB', '0.0000000001'])('refuses %j as a number of GB before anything is sent', async (typed) => {
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () => quotaOf(0),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');
    const field = await within(region).findByLabelText(FIELD);
    expect(within(region).queryByRole('alert')).toBeNull();

    fireEvent.change(field, { target: { value: typed } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    expect((await within(region).findByRole('alert')).textContent).toContain(
      'The quota is not a number of GB: digits, with "." as the decimal point',
    );
    expect(api.callsTo('PUT', '/buckets/photos/quota')).toHaveLength(0);
  });

  it('sends a quota beyond 2^53 bytes as the integer typed, the greatest there is and one more', async () => {
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () => quotaOf(0),
      'PUT /buckets/photos/quota': () => refusal(400, 'InvalidArgument', 'Invalid argument'),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');
    const field = await within(region).findByLabelText(FIELD);

    // 2^62 + 1 bytes: as a JavaScript number it would be 2^62, which the API takes.
    fireEvent.change(field, { target: { value: '4611686018.427387905' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    await within(region).findByRole('alert');
    // 2^62 - 1 bytes, which a number would also send as 2^62.
    fireEvent.change(field, { target: { value: '4611686018.427387903' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/quota')).toHaveLength(2);
    });
    const puts = api.callsTo('PUT', '/buckets/photos/quota');
    expect(puts[0]?.body).toBe('{"quota_bytes":4611686018427387905}');
    expect(puts[1]?.body).toBe('{"quota_bytes":4611686018427387903}');
  });

  it('says a quota beyond 2^53 bytes is about what is shown, and begins empty so as not to send it back changed', async () => {
    // 2^62 - 1 as the API writes it; the client reads it as 2^62.
    const stored = '4611686018427387903';
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () =>
        new Response(`{"quota_bytes":${stored},"used_bytes":0,"exact":true}`, {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');

    await within(region).findByText(/Quota: 4,611,686,018 GB \(about 4,611,686,018,427,388,000 bytes\)\./);
    const field = within(region).getByLabelText(FIELD) as HTMLInputElement;
    expect(field.value).toBe('');
    const described = (field.getAttribute('aria-describedby') ?? '').split(' ');
    expect(described.map((id) => document.getElementById(id)?.textContent)).toEqual([QUOTA_ROUGH_HINT, QUOTA_HINT]);
    // Saved untouched, nothing is sent: the field is required.
    fireEvent.click(within(region).getByRole('button', { name: 'Save quota' }));
    expect(api.callsTo('PUT', '/buckets/photos/quota')).toHaveLength(0);
    // It can still be removed.
    expect(within(region).getByRole('button', { name: 'Remove quota' })).toBeDefined();
  });

  it('says a quota that could not be read, and reads it again when asked', async () => {
    let down = true;
    const api = signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () => (down ? refusal(404, 'NotFound', 'No such endpoint') : quotaOf(0, 1_000_000_000)),
    });
    render(<BucketsScreen />);
    const region = await openQuota('photos');

    // A node of an older release, met during a rolling upgrade.
    const failure = await within(region).findByRole('alert');
    expect(failure.textContent).toContain('Bucket quotas are not available on the node that answered (an older release)');
    expect(within(region).queryByRole('textbox')).toBeNull();
    down = false;
    fireEvent.click(within(region).getByRole('button', { name: 'Try again' }));
    await within(region).findByText(/Quota: 1\.00 GB/);
    expect(api.callsTo('GET', '/buckets/photos/quota')).toHaveLength(2);
    expect(within(region).queryByRole('alert')).toBeNull();
  });

  it('closes with Escape or its button, and gives the keyboard back to the button of the row', async () => {
    signedIn({
      'GET /buckets': () => json(200, [bucket('photos', NOTHING)]),
      'GET /buckets/photos/usage': () => counted(0, 0),
      'GET /buckets/photos/quota': () => quotaOf(0),
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');
    const button = within(rowOf(table, 'photos')).getByRole('button', { name: 'Quota of photos' });

    let region = await openQuota('photos');
    const field = await within(region).findByLabelText(FIELD);
    fireEvent.keyDown(field, { key: 'Escape' });
    expect(screen.queryByRole('region', { name: 'Quota of photos' })).toBeNull();
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(button);

    region = await openQuota('photos');
    const close = within(region).getByRole('button', { name: 'Close quota' });
    expect(close.getAttribute('title')).toBe('Close the quota');
    fireEvent.click(close);
    expect(screen.queryByRole('region', { name: 'Quota of photos' })).toBeNull();
    expect(document.activeElement).toBe(button);

    // The button of the row opens and closes it too.
    fireEvent.click(button);
    expect(screen.getByRole('region', { name: 'Quota of photos' })).toBeDefined();
    expect(button.getAttribute('title')).toBe('Hide the quota of this bucket');
    fireEvent.click(button);
    expect(screen.queryByRole('region', { name: 'Quota of photos' })).toBeNull();
  });

  it('keeps the grid a grid on a phone, the quota in it, and its editor as wide as what is seen', async () => {
    fakeApi({
      'GET /buckets': () =>
        json(200, [
          bucket('photos', {
            access_included: true,
            created_by: 'root',
            activity: { uploads: [], downloads: [] },
            policy_included: true,
          }),
        ]),
      'GET /buckets/photos/usage': () => counted(1, 0, 2),
      'GET /buckets/photos/quota': () => quotaOf(1, 2),
    });
    render(<BucketsScreen />);
    const table = await screen.findByRole('table');
    // Kept a table and scrolled sideways inside its panel (console.css,
    // max-width 47.99rem); the details of a row are as wide as what is seen.
    expect(table.className).toContain('grid-keep');
    expect(table.parentElement?.className).toContain('grid-scroll');
    expect(table.parentElement?.className).toContain('grid-scroll-details');
    const photos = rowOf(table, 'photos');
    await within(photos).findByText('50 %');
    expect(cellOf(photos, 'Quota')?.querySelector('.quota-cell')).not.toBeNull();
    // The button of the row is an icon, with its name to be read out and a hint.
    const button = within(photos).getByRole('button', { name: 'Quota of photos' });
    expect(button.querySelector('.btn-text')?.textContent).toBe('Quota');
    expect(button.closest('td')?.getAttribute('data-label')).toBe('Actions');

    const region = await openQuota('photos');
    expect(region.className).toBe('grid-detail-body');
    // Across every column there is: ten with all the details.
    expect(headings(table)).toHaveLength(10);
    expect(region.closest('td')?.getAttribute('colspan')).toBe('10');
    await within(region).findByLabelText(FIELD);
  });
});
