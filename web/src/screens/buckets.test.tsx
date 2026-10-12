import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { fakeApi, json, noContent, problem } from '../test/fakeApi';
import { BucketsScreen } from './BucketsScreen';

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
    expect(within(row).getAllByRole('cell')).toHaveLength(9);
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
    expect(headings(table)).toEqual(['Name', 'Objects', 'Size (GB)', 'Created', 'Actions']);
    for (const label of ['Created by', 'Policy', 'Last upload', 'Last download']) {
      expect(table.querySelector(`[data-label="${label}"]`), label).toBeNull();
    }
    expect(table.className).toContain('grid-keep');
    expect(table.className).not.toContain('grid-wide');
    // No empty columns either: a row has the five cells of its headings.
    await within(rowOf(table, 'photos')).findByText('< 0.01 GB');
    await within(rowOf(table, 'site')).findByText('< 0.01 GB');
    for (const row of within(table).getAllByRole('row').slice(1)) {
      expect(within(row).getAllByRole('cell')).toHaveLength(5);
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
    expect(headings(table)).toEqual(['Name', 'Objects', 'Size (GB)', 'Created', 'Policy', 'Actions']);
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
    expect(headings(table)).toEqual(['Name', 'Objects', 'Size (GB)', 'Created', 'Actions']);
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
