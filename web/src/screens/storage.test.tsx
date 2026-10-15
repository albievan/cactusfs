import { File as NodeFile } from 'node:buffer';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { App } from '../App';
import { DeletionsProvider } from '../deletions/DeletionsProvider';
import { fakeApi, json, noContent, problem, signedIn } from '../test/fakeApi';
import { UploadsProvider } from '../uploads/UploadsProvider';
import { BucketsScreen } from './BucketsScreen';
import { ObjectsScreen } from './ObjectsScreen';

const CREATED = '2026-10-01T00:00:00Z';

/**
 * Shows the object browser as the console does: beneath the uploads of the
 * console, which the screen hands its files to and reads its tiles from, and
 * beneath its deletions, which it hands what is to be deleted to.
 */
function renderObjects(screenOfABucket: ReactElement) {
  return render(
    <UploadsProvider>
      <DeletionsProvider>{screenOfABucket}</DeletionsProvider>
    </UploadsProvider>,
  );
}

function object(key: string, size = 10) {
  return { key, size, etag: '"e"', last_modified: CREATED };
}

/** What the API says of an object nobody has read and whose uploader was not recorded, with what a test adds. */
function access(more: Record<string, unknown> = {}) {
  return {
    uploaded_at: CREATED,
    download_count: 0,
    copy_count: 0,
    readers: [],
    other_readers: 0,
    other_downloads: 0,
    other_copies: 0,
    ...more,
  };
}

/** What the API says of an object of the bucket "photos", with what a test changes of it. */
function metadataOf(key: string, more: Record<string, unknown> = {}) {
  return {
    bucket: 'photos',
    key,
    size: 10,
    etag: '"0123abcd"',
    last_modified: CREATED,
    content_type: 'text/plain',
    encryption: 'SSE-S3',
    ...more,
  };
}

/** The answers behind the details of one object of the bucket "photos". */
function detailsOf(key: string) {
  return {
    'GET /buckets/photos/objects': () => json(200, { objects: [object(key)], common_prefixes: [], truncated: false, access_included: false }),
    'GET /buckets/photos/object/metadata': () => json(200, metadataOf(key)),
  };
}

/** The row of an object in the grid: the one its tick is in. */
function rowOf(name: string): HTMLTableRowElement {
  return screen.getByLabelText(`Select ${name}`).closest('tr') as HTMLTableRowElement;
}

/** The row of an object, once the grid shows it. */
async function findRow(name: string): Promise<HTMLTableRowElement> {
  return (await screen.findByLabelText(`Select ${name}`)).closest('tr') as HTMLTableRowElement;
}

/** The name of an object in its row: the button that opens its details. */
function nameButton(row: HTMLElement, name: string): HTMLElement {
  return within(row).getByRole('button', { name });
}

/** The details button among the actions of a row. */
function detailsButton(row: HTMLElement, name: string): HTMLElement {
  return within(row).getByRole('button', { name: `Details of ${name}` });
}

/** What the details of an object say of one fact: the value beside its name. */
function factOf(region: HTMLElement, name: string): string | null | undefined {
  return within(region).getByText(name, { selector: 'dt' }).nextElementSibling?.textContent;
}

/**
 * Watches for the page being scrolled by the console: with `window.scrollTo`,
 * `window.scrollBy` or `scrollIntoView` of an element. jsdom has no
 * `scrollIntoView`; one is put there for the watch, and what was there
 * before - nothing, or another - is put back by `restore`.
 */
function watchScrolling() {
  const scrollTo = vi.spyOn(window, 'scrollTo').mockImplementation(() => undefined);
  const scrollBy = vi.spyOn(window, 'scrollBy').mockImplementation(() => undefined);
  const before = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
  const scrollIntoView = vi.fn();
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, writable: true, value: scrollIntoView });
  return {
    count: () => scrollTo.mock.calls.length + scrollBy.mock.calls.length + scrollIntoView.mock.calls.length,
    restore: () => {
      if (before === undefined) {
        Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
      } else {
        Object.defineProperty(Element.prototype, 'scrollIntoView', before);
      }
    },
  };
}

describe('buckets', () => {
  it('lists the buckets with links to their objects', async () => {
    fakeApi({ 'GET /buckets': () => json(200, [{ name: 'photos', created_at: CREATED }]) });
    render(<BucketsScreen />);

    const link = await screen.findByRole('link', { name: 'Open photos' });
    expect(link.getAttribute('href')).toBe('#/buckets/photos');
  });

  // How much each bucket holds, and the rest of the grid: buckets.test.tsx.

  it('says so when there are none', async () => {
    fakeApi({ 'GET /buckets': () => json(200, []) });
    render(<BucketsScreen />);

    expect(await screen.findByText('There are no buckets yet.')).toBeDefined();
  });

  it('creates a bucket and lists again', async () => {
    const buckets: { name: string; created_at: string }[] = [];
    const api = fakeApi({
      'GET /buckets': () => json(200, buckets),
      'POST /buckets': (call) => {
        const bucket = { name: (JSON.parse(call.body) as { name: string }).name, created_at: CREATED };
        buckets.push(bucket);
        return json(201, bucket);
      },
    });
    render(<BucketsScreen />);

    await screen.findByText('There are no buckets yet.');
    fireEvent.change(screen.getByLabelText('Bucket name'), { target: { value: 'photos' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create bucket' }));

    expect(await screen.findByRole('link', { name: 'Open photos' })).toBeDefined();
    expect(JSON.parse(api.callsTo('POST', '/buckets')[0]?.body ?? '')).toEqual({ name: 'photos' });
    expect((screen.getByLabelText('Bucket name') as HTMLInputElement).value).toBe('');
  });

  it('deletes a bucket only after the question is answered', async () => {
    let buckets = [{ name: 'photos', created_at: CREATED }];
    const api = fakeApi({
      'GET /buckets': () => json(200, buckets),
      'DELETE /buckets/photos': () => {
        buckets = [];
        return noContent();
      },
    });
    render(<BucketsScreen />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(screen.getByText('Delete bucket photos?')).toBeDefined();
    expect(api.callsTo('DELETE', '/buckets/photos')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    expect(await screen.findByText('There are no buckets yet.')).toBeDefined();
    expect(api.callsTo('DELETE', '/buckets/photos')).toHaveLength(1);
  });

  it('can be talked out of deleting', async () => {
    const api = fakeApi({ 'GET /buckets': () => json(200, [{ name: 'photos', created_at: CREATED }]) });
    render(<BucketsScreen />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(screen.getByRole('button', { name: 'Delete' })).toBeDefined();
    expect(api.calls.every((call) => call.method === 'GET')).toBe(true);
  });

  it('shows the problem of the API when it refuses', async () => {
    fakeApi({
      'GET /buckets': () => json(200, [{ name: 'photos', created_at: CREATED }]),
      'DELETE /buckets/photos': () =>
        problem(409, 'The bucket is not empty', 'Delete its objects first.', 'req-42'),
    });
    render(<BucketsScreen />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The bucket is not empty');
    expect(alert.textContent).toContain('Delete its objects first.');
    expect(alert.textContent).toContain('req-42');
    expect(alert.parentElement?.getAttribute('aria-live')).toBe('assertive');
    expect(screen.getByRole('link', { name: 'Open photos' })).toBeDefined();
  });

  it('shows the refusal to list, and does not hide the form because of it', async () => {
    fakeApi({ 'GET /buckets': () => problem(403, 'Access denied', 's3:ListAllMyBuckets is not allowed.') });
    render(<BucketsScreen />);

    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.getByRole('button', { name: 'Create bucket' })).toBeDefined();
  });
});

describe('object browser', () => {
  it('lists one level of a prefix, with the delimiter', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, {
          objects: [object('2026/readme.txt', 2048)],
          common_prefixes: ['2026/january/'],
          truncated: false,
          access_included: false,
        }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="2026/" />);

    const folder = await screen.findByRole('link', { name: 'january/' });
    expect(folder.getAttribute('href')).toBe('#/buckets/photos?prefix=2026%2Fjanuary%2F');
    expect(screen.getByText('readme.txt')).toBeDefined();
    expect(screen.getByText('2.0 KiB')).toBeDefined();

    const [call] = api.calls;
    expect(call?.query.get('prefix')).toBe('2026/');
    expect(call?.query.get('delimiter')).toBe('/');
    expect(call?.query.has('continuation_token')).toBe(false);
    // Who uploaded and how often it was downloaded is asked for with the
    // listing, not object by object.
    expect(call?.query.get('access')).toBe('true');
  });

  it('sends no prefix at the top of the bucket', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects: [], common_prefixes: [], truncated: false, access_included: false }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    expect(await screen.findByText('There is nothing here.')).toBeDefined();
    expect(api.calls[0]?.query.has('prefix')).toBe(false);
  });

  it('shows the way back up as breadcrumbs', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects: [], common_prefixes: [], truncated: false, access_included: false }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="2026/january/" />);

    const crumbs = within(screen.getByRole('navigation', { name: 'Prefix' }));
    expect(crumbs.getByRole('link', { name: 'Buckets' }).getAttribute('href')).toBe('#/buckets');
    expect(crumbs.getByRole('link', { name: 'photos' }).getAttribute('href')).toBe('#/buckets/photos');
    expect(crumbs.getByRole('link', { name: '2026' }).getAttribute('href')).toBe('#/buckets/photos?prefix=2026%2F');
    expect(crumbs.getByRole('link', { name: 'january' }).getAttribute('href')).toBe(
      '#/buckets/photos?prefix=2026%2Fjanuary%2F',
    );
    await screen.findByText('There is nothing here.');
  });

  it('walks into a prefix when its link is followed', async () => {
    const api = signedIn({
      'GET /buckets/photos/objects': (call) =>
        call.query.get('prefix') === '2026/'
          ? json(200, { objects: [object('2026/inside.txt')], common_prefixes: [], truncated: false, access_included: false })
          : json(200, { objects: [], common_prefixes: ['2026/'], truncated: false, access_included: false }),
    });
    window.location.hash = '#/buckets/photos';
    render(<App />);

    const folder = await screen.findByRole('link', { name: '2026/' });
    window.location.hash = folder.getAttribute('href') ?? '';

    expect(await screen.findByText('inside.txt')).toBeDefined();
    expect(api.callsTo('GET', '/buckets/photos/objects').at(-1)?.query.get('prefix')).toBe('2026/');
  });

  it('pages with the continuation token, forwards and back', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': (call) =>
        call.query.get('continuation_token') === 'page-2'
          ? json(200, { objects: [object('b.txt')], common_prefixes: [], truncated: false, access_included: false })
          : json(200, {
              objects: [object('a.txt')],
              common_prefixes: [],
              truncated: true,
              access_included: false,
              next_continuation_token: 'page-2',
            }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    await screen.findByText('a.txt');
    expect(screen.getByText('Page 1')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Previous page' }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: 'Next page' }));

    await screen.findByText('b.txt');
    expect(screen.queryByText('a.txt')).toBeNull();
    expect(screen.getByText('Page 2')).toBeDefined();
    expect(api.calls.at(-1)?.query.get('continuation_token')).toBe('page-2');
    expect((screen.getByRole('button', { name: 'Next page' }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: 'Previous page' }));

    await screen.findByText('a.txt');
    expect(api.calls.at(-1)?.query.has('continuation_token')).toBe(false);
  });

  it('downloads through a plain same-origin link', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('2026/a b.jpg')], common_prefixes: [], truncated: false, access_included: false }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="2026/" />);

    const link = await screen.findByRole('link', { name: 'Download a b.jpg' });
    expect(link.getAttribute('href')).toBe('/api/v1/buckets/photos/object?key=2026%2Fa+b.jpg');
    expect(link.hasAttribute('download')).toBe(true);
  });

  it('opens what is known about an object under its row, when its name is clicked', async () => {
    const scrolling = watchScrolling();
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt'), object('b.txt')], common_prefixes: [], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) => json(200, metadataOf(call.query.get('key') ?? '')),
      'GET /buckets/photos/object/access': () =>
        json(200, access({ uploaded_by: 'user/alice', download_count: 9, copy_count: 2 })),
    });
    try {
      renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

      const row = await findRow('a.txt');
      const name = nameButton(row, 'a.txt');
      const details = detailsButton(row, 'a.txt');
      // Closed: both say so, and there is no row under it but the next object's.
      expect(name.getAttribute('aria-expanded')).toBe('false');
      expect(details.getAttribute('aria-expanded')).toBe('false');
      expect(row.nextElementSibling).toBe(rowOf('b.txt'));

      name.focus();
      fireEvent.click(name);

      // The details are in the row directly under the row of the object, and
      // nowhere else: there is no panel above the grid any more.
      const region = screen.getByRole('region', { name: 'Details of a.txt' });
      const detailRow = region.closest('tr');
      expect(detailRow).not.toBeNull();
      expect(row.nextElementSibling).toBe(detailRow);
      expect(detailRow?.className).toBe('grid-detail');
      expect(detailRow?.nextElementSibling).toBe(rowOf('b.txt'));
      expect(screen.queryByRole('heading', { name: 'Object details' })).toBeNull();
      expect(row.closest('table')?.contains(region)).toBe(true);

      // Everything the panel showed.
      await within(region).findByText('Bucket', { selector: 'dt' });
      expect(factOf(region, 'Bucket')).toBe('photos');
      expect(factOf(region, 'Key')).toBe('a.txt');
      expect(factOf(region, 'Size')).toBe('10 bytes');
      expect(factOf(region, 'ETag')).toBe('"0123abcd"');
      expect(factOf(region, 'Last modified')).toBe(new Date(CREATED).toLocaleString());
      expect(factOf(region, 'Content type')).toBe('text/plain');
      expect(factOf(region, 'Encryption')).toBe('SSE-S3');
      await within(region).findByRole('heading', { name: 'Uploads and downloads' });
      expect(factOf(region, 'Uploaded by')).toBe('user/alice');
      expect(factOf(region, 'Uploaded')).toBe(new Date(CREATED).toLocaleString());
      expect(factOf(region, 'Downloads')).toBe('9');
      expect(factOf(region, 'Copies')).toBe('2');
      expect(factOf(region, 'Last downloaded')).toBe('never');
      expect(within(region).getByText(/counts kept as well as can be, not an audit trail/)).toBeDefined();
      expect(within(region).getByRole('button', { name: 'Close details' })).toBeDefined();

      // Both of what opens it say that it is open, and what they opened.
      expect(name.getAttribute('aria-expanded')).toBe('true');
      expect(details.getAttribute('aria-expanded')).toBe('true');
      expect(region.id).not.toBe('');
      expect(name.getAttribute('aria-controls')).toBe(region.id);
      expect(details.getAttribute('aria-controls')).toBe(region.id);
      // The id is not made of the key.
      expect(region.id).not.toContain('a.txt');
      // The other row is as it was.
      expect(nameButton(rowOf('b.txt'), 'b.txt').getAttribute('aria-expanded')).toBe('false');

      // Nothing moved: the keyboard is where it was, and the page was not scrolled.
      expect(document.activeElement).toBe(name);
      expect(scrolling.count()).toBe(0);
      expect(screen.queryByRole('alert')).toBeNull();
      const asked = api.callsTo('GET', '/buckets/photos/object/metadata');
      expect(asked).toHaveLength(1);
      expect(asked[0]?.query.get('key')).toBe('a.txt');

      // The same click closes it.
      fireEvent.click(name);
      expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
      expect(row.nextElementSibling).toBe(rowOf('b.txt'));
      expect(name.getAttribute('aria-expanded')).toBe('false');
      expect(details.getAttribute('aria-expanded')).toBe('false');
      expect(document.activeElement).toBe(name);
      expect(scrolling.count()).toBe(0);
    } finally {
      scrolling.restore();
    }
  });

  it('opens and closes them with the details button too, which says which of the two it will do', async () => {
    const api = fakeApi({ ...detailsOf('a.txt'), 'GET /buckets/photos/object/access': () => json(200, access()) });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const row = await findRow('a.txt');
    const name = nameButton(row, 'a.txt');
    const details = detailsButton(row, 'a.txt');
    expect(details.getAttribute('title')).toBe('Show details');
    expect(name.getAttribute('title')).toBe('Show details');

    details.focus();
    fireEvent.click(details);

    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    expect(row.nextElementSibling).toBe(region.closest('tr'));
    await waitFor(() => {
      expect(factOf(region, 'Content type')).toBe('text/plain');
    });
    expect(details.getAttribute('aria-expanded')).toBe('true');
    expect(name.getAttribute('aria-expanded')).toBe('true');
    expect(details.getAttribute('aria-controls')).toBe(region.id);
    expect(details.getAttribute('title')).toBe('Hide details');
    expect(name.getAttribute('title')).toBe('Hide details');
    // That it is open is not said by colour alone.
    expect(details.className).toContain('active');
    expect(document.activeElement).toBe(details);
    expect(api.callsTo('GET', '/buckets/photos/object/metadata')[0]?.query.get('key')).toBe('a.txt');

    fireEvent.click(details);

    expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
    expect(row.nextElementSibling).toBeNull();
    expect(details.getAttribute('aria-expanded')).toBe('false');
    expect(name.getAttribute('aria-expanded')).toBe('false');
    expect(details.getAttribute('title')).toBe('Show details');
    expect(document.activeElement).toBe(details);
  });

  it('closes them with their own button and with Escape, and gives the keyboard back to what opened them', async () => {
    const api = fakeApi({ ...detailsOf('a.txt'), 'GET /buckets/photos/object/access': () => json(200, access()) });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const row = await findRow('a.txt');
    const name = nameButton(row, 'a.txt');
    const details = detailsButton(row, 'a.txt');
    const region = () => screen.queryByRole('region', { name: 'Details of a.txt' });
    const closeButton = () => within(region() as HTMLElement).getByRole('button', { name: 'Close details' });

    // Opened by the name, closed by the button at its end.
    fireEvent.click(name);
    await within(region() as HTMLElement).findByText('Encryption', { selector: 'dt' });
    const close = closeButton();
    expect(close.getAttribute('title')).toBe('Close the details');
    close.focus();
    fireEvent.click(close);
    expect(region()).toBeNull();
    expect(name.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(name);

    // Opened by the details button, closed by Escape from inside.
    fireEvent.click(details);
    expect(region()).not.toBeNull();
    closeButton().focus();
    fireEvent.keyDown(closeButton(), { key: 'Escape' });
    expect(region()).toBeNull();
    expect(details.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(details);

    // Escape anywhere else closes nothing, and neither does another key inside.
    fireEvent.click(name);
    expect(region()).not.toBeNull();
    name.focus();
    fireEvent.keyDown(name, { key: 'Escape' });
    expect(region()).not.toBeNull();
    fireEvent.keyDown(closeButton(), { key: 'Enter' });
    expect(region()).not.toBeNull();

    // What was read of the object was read once, however often it was opened.
    await within(region() as HTMLElement).findByText('Encryption', { selector: 'dt' });
    expect(api.callsTo('GET', '/buckets/photos/object/metadata')).toHaveLength(1);
  });

  it('shows who uploaded an object and who downloaded it', async () => {
    const api = fakeApi({
      ...detailsOf('a.txt'),
      'GET /buckets/photos/object/access': () =>
        json(
          200,
          access({
            uploaded_by: 'user/alice',
            download_count: 9,
            copy_count: 2,
            last_downloaded_at: '2026-10-05T10:00:00Z',
            readers: [
              { who: 'anonymous', presigned: false, downloads: 1, copies: 0, last_at: '2026-10-02T10:00:00Z' },
              { who: 'user/bob', presigned: false, downloads: 3, copies: 2, last_at: '2026-10-05T10:00:00Z' },
              { who: 'user/bob', presigned: true, downloads: 2, copies: 0, last_at: '2026-10-04T10:00:00Z' },
            ],
            other_readers: 4,
            other_downloads: 3,
            other_copies: 1,
          }),
        ),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));

    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    const panel = (await within(region).findByRole('heading', { name: 'Uploads and downloads' })).closest('section');
    expect(panel).not.toBeNull();
    const facts = within(panel as HTMLElement);
    const fact = (name: string) => facts.getByText(name, { selector: 'dt' }).nextElementSibling?.textContent;
    expect(fact('Uploaded by')).toBe('user/alice');
    expect(fact('Uploaded')).toBe(new Date(CREATED).toLocaleString());
    expect(fact('Downloads')).toBe('9');
    expect(fact('Copies')).toBe('2');
    expect(fact('Last downloaded')).toBe(new Date('2026-10-05T10:00:00Z').toLocaleString());

    const table = facts.getByRole('table', { name: 'Downloaded by' });
    expect(within(table).getAllByRole('columnheader').map((th) => th.textContent)).toEqual([
      'Who',
      'Downloads',
      'Copies',
      'Last time',
    ]);
    const rows = within(table)
      .getAllByRole('row')
      .slice(1)
      .map((row) =>
        within(row)
          .getAllByRole('cell')
          .map((cell) => cell.textContent.trim()),
      );
    expect(rows).toEqual([
      ['anonymous', '1', '0', new Date('2026-10-02T10:00:00Z').toLocaleString()],
      ['user/bob', '3', '2', new Date('2026-10-05T10:00:00Z').toLocaleString()],
      ['user/bob presigned link', '2', '0', new Date('2026-10-04T10:00:00Z').toLocaleString()],
    ]);
    // What a presigned link means is said where it is shown.
    expect(within(table).getByText('presigned link').getAttribute('title')).toContain('whoever held the link');
    expect(facts.getByText('and 4 others, with 3 downloads and 1 copy between them')).toBeDefined();
    // The figures are called what they are.
    expect(facts.getByText(/counts kept as well as can be, not an audit trail/)).toBeDefined();

    const asked = api.callsTo('GET', '/buckets/photos/object/access');
    expect(asked).toHaveLength(1);
    expect(asked[0]?.query.get('key')).toBe('a.txt');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says so when the uploader was not recorded and nobody downloaded the object', async () => {
    fakeApi({ ...detailsOf('a.txt'), 'GET /buckets/photos/object/access': () => json(200, access()) });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));

    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    const panel = (await within(region).findByRole('heading', { name: 'Uploads and downloads' })).closest('section');
    const facts = within(panel as HTMLElement);
    const fact = (name: string) => facts.getByText(name, { selector: 'dt' }).nextElementSibling?.textContent;
    expect(fact('Uploaded by')).toBe('not recorded');
    expect(fact('Downloads')).toBe('0');
    expect(fact('Last downloaded')).toBe('never');
    expect(facts.queryByRole('table')).toBeNull();
    expect(facts.queryByText(/others/)).toBeNull();
  });

  it('says in one line, in the row and without an alert, that who accessed an object may not be seen', async () => {
    fakeApi({
      ...detailsOf('a.txt'),
      'GET /buckets/photos/object/access': () => problem(403, 'Access denied'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));

    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    expect(await within(region).findByText('You may not see who accessed this object.')).toBeDefined();
    // The details themselves are shown, and nothing is reported as failed.
    expect(factOf(region, 'Content type')).toBe('text/plain');
    expect(factOf(region, 'Encryption')).toBe('SSE-S3');
    expect(factOf(region, 'ETag')).toBe('"0123abcd"');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Uploads and downloads' })).toBeNull();
    expect(screen.queryByText('Uploaded by')).toBeNull();
    expect(screen.queryByText(/Access denied/)).toBeNull();
  });

  it('says why when who accessed an object cannot be loaded, and still shows the details', async () => {
    fakeApi({
      ...detailsOf('a.txt'),
      'GET /buckets/photos/object/access': () => problem(503, 'Temporarily unavailable'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));

    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    expect(
      await within(region).findByText('Who accessed this object could not be loaded: Temporarily unavailable'),
    ).toBeDefined();
    expect(factOf(region, 'Content type')).toBe('text/plain');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('keeps the details of several objects open at once, each asked for once and each with its own', async () => {
    const scrolling = watchScrolling();
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt'), object('b.txt')], common_prefixes: [], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) =>
        json(200, metadataOf(call.query.get('key') ?? '', { content_type: call.query.get('key') === 'a.txt' ? 'text/plain' : 'image/png' })),
      'GET /buckets/photos/object/access': (call) =>
        json(200, access({ uploaded_by: call.query.get('key') === 'a.txt' ? 'user/alice' : 'user/bob' })),
    });
    try {
      renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

      fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));
      const regionA = screen.getByRole('region', { name: 'Details of a.txt' });
      await within(regionA).findByRole('heading', { name: 'Uploads and downloads' });
      // The one lower down is opened by its name: the table grows downward.
      fireEvent.click(nameButton(rowOf('b.txt'), 'b.txt'));
      const regionB = screen.getByRole('region', { name: 'Details of b.txt' });
      await within(regionB).findByRole('heading', { name: 'Uploads and downloads' });

      // Both are open, each under its own row.
      expect(screen.getByRole('region', { name: 'Details of a.txt' })).toBe(regionA);
      expect(rowOf('a.txt').nextElementSibling).toBe(regionA.closest('tr'));
      expect(regionA.closest('tr')?.nextElementSibling).toBe(rowOf('b.txt'));
      expect(rowOf('b.txt').nextElementSibling).toBe(regionB.closest('tr'));
      expect(regionB.closest('tr')?.nextElementSibling).toBeNull();
      // Each says what is known of its own object.
      expect(factOf(regionA, 'Key')).toBe('a.txt');
      expect(factOf(regionA, 'Content type')).toBe('text/plain');
      expect(factOf(regionA, 'Uploaded by')).toBe('user/alice');
      expect(factOf(regionB, 'Key')).toBe('b.txt');
      expect(factOf(regionB, 'Content type')).toBe('image/png');
      expect(factOf(regionB, 'Uploaded by')).toBe('user/bob');
      // Two regions, two ids, and two headings that do not share one either.
      expect(regionA.id).not.toBe(regionB.id);
      const headings = screen.getAllByRole('heading', { name: 'Uploads and downloads' });
      expect(headings).toHaveLength(2);
      expect(new Set(headings.map((heading) => heading.id)).size).toBe(2);

      // The screen is drawn again - an object is ticked, and unticked - and
      // nothing is asked for a second time.
      fireEvent.click(screen.getByLabelText('Select a.txt'));
      fireEvent.click(screen.getByLabelText('Select a.txt'));
      expect(api.callsTo('GET', '/buckets/photos/object/metadata').map((call) => call.query.get('key'))).toEqual([
        'a.txt',
        'b.txt',
      ]);
      expect(api.callsTo('GET', '/buckets/photos/object/access').map((call) => call.query.get('key'))).toEqual([
        'a.txt',
        'b.txt',
      ]);

      // Closing one leaves the other.
      fireEvent.click(within(regionA).getByRole('button', { name: 'Close details' }));
      expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
      expect(screen.getByRole('region', { name: 'Details of b.txt' })).toBe(regionB);
      expect(factOf(regionB, 'Key')).toBe('b.txt');
      expect(scrolling.count()).toBe(0);
    } finally {
      scrolling.restore();
    }
  });

  it('lays the details across every column of the grid, with the access columns and without', async () => {
    for (const withAccess of [false, true]) {
      fakeApi({
        'GET /buckets/photos/objects': () =>
          json(200, {
            objects: [
              withAccess
                ? { ...object('a.txt'), access: { uploaded_at: CREATED, download_count: 0, copy_count: 0 } }
                : object('a.txt'),
            ],
            common_prefixes: ['dir/'],
            truncated: false,
            access_included: withAccess,
          }),
        'GET /buckets/photos/object/metadata': () => json(200, metadataOf('a.txt')),
        'GET /buckets/photos/object/access': () => json(200, access()),
      });
      const shown = renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

      const row = await findRow('a.txt');
      fireEvent.click(nameButton(row, 'a.txt'));
      const region = screen.getByRole('region', { name: 'Details of a.txt' });
      await within(region).findByText('Bucket', { selector: 'dt' });

      const table = row.closest('table') as HTMLTableElement;
      const headings = table.tHead?.rows[0]?.cells.length;
      expect(headings).toBe(withAccess ? 10 : 5);
      const detailRow = row.nextElementSibling as HTMLTableRowElement;
      expect(detailRow.cells).toHaveLength(1);
      expect(detailRow.cells[0]?.colSpan).toBe(headings);
      // The row of the object has a cell a column, as it had.
      expect(row.cells).toHaveLength(withAccess ? 10 : 5);
      // The row of the details has nothing to tick.
      expect(within(detailRow).queryByRole('checkbox')).toBeNull();

      shown.unmount();
    }
  });

  it('says in the row, and not above the page, that the details of an object could not be read, and reads them when asked again', async () => {
    let failing = true;
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt'), object('b.txt')], common_prefixes: [], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) =>
        failing && call.query.get('key') === 'a.txt'
          ? problem(503, 'Temporarily unavailable', 'No metadata leader.', 'req-5')
          : json(200, metadataOf(call.query.get('key') ?? '')),
      'GET /buckets/photos/object/access': () => json(200, access()),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(nameButton(await findRow('a.txt'), 'a.txt'));
    const regionA = screen.getByRole('region', { name: 'Details of a.txt' });

    // Why, in the words of the API, inside the row that was opened...
    const alert = await within(regionA).findByRole('alert');
    expect(alert.textContent).toContain('Temporarily unavailable');
    expect(alert.textContent).toContain('No metadata leader.');
    expect(alert.textContent).toContain('req-5');
    // ...and nowhere else: the page reports no failure of its own.
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(screen.getAllByRole('alert')[0]).toBe(alert);
    expect(within(regionA).queryByText('Bucket', { selector: 'dt' })).toBeNull();
    expect(within(regionA).queryByRole('heading', { name: 'Uploads and downloads' })).toBeNull();
    // The row stays open, and can be closed.
    expect(nameButton(rowOf('a.txt'), 'a.txt').getAttribute('aria-expanded')).toBe('true');
    expect(within(regionA).getByRole('button', { name: 'Close details' })).toBeDefined();

    // Another object is not held up by it.
    fireEvent.click(nameButton(rowOf('b.txt'), 'b.txt'));
    const regionB = screen.getByRole('region', { name: 'Details of b.txt' });
    await waitFor(() => {
      expect(factOf(regionB, 'Key')).toBe('b.txt');
    });
    expect(within(regionB).queryByRole('alert')).toBeNull();
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(screen.getAllByRole('alert')[0]).toBe(alert);

    // Asked again, it is read.
    failing = false;
    const again = within(regionA).getByRole('button', { name: 'Try again' });
    expect(again.getAttribute('title')).toBe('Ask for the details of this object again');
    fireEvent.click(again);
    await waitFor(() => {
      expect(factOf(regionA, 'Key')).toBe('a.txt');
    });
    expect(screen.queryByRole('alert')).toBeNull();
    expect(within(regionA).queryByRole('button', { name: 'Try again' })).toBeNull();
    expect(
      api.callsTo('GET', '/buckets/photos/object/metadata').map((call) => call.query.get('key')),
    ).toEqual(['a.txt', 'b.txt', 'a.txt']);
  });

  it('keeps the details open when the list is read again, and asks again only about an object that was replaced', async () => {
    let objects = [object('a.txt'), object('b.txt'), object('c.txt'), object('d.txt')];
    let etag = '"first"';
    let uploader = 'user/alice';
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) => json(200, metadataOf(call.query.get('key') ?? '', { etag })),
      'GET /buckets/photos/object/access': () => json(200, access({ uploaded_by: uploader })),
      'DELETE /buckets/photos/object': (call) => {
        objects = objects.filter((o) => o.key !== call.query.get('key'));
        return noContent();
      },
    });
    const lists = () => api.callsTo('GET', '/buckets/photos/objects').length;
    const metadataAsked = () => api.callsTo('GET', '/buckets/photos/object/metadata').length;
    const accessAsked = () => api.callsTo('GET', '/buckets/photos/object/access').length;
    // Deleting another object is what has the list read again.
    const deleteOther = async (name: string) => {
      const before = lists();
      fireEvent.click(within(rowOf(name)).getByRole('button', { name: 'Delete' }));
      fireEvent.click(within(rowOf(name)).getByRole('button', { name: 'Yes, delete' }));
      await waitFor(() => {
        expect(screen.queryByLabelText(`Select ${name}`)).toBeNull();
      });
      expect(lists()).toBe(before + 1);
    };
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(nameButton(await findRow('a.txt'), 'a.txt'));
    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    await within(region).findByRole('heading', { name: 'Uploads and downloads' });
    expect(factOf(region, 'ETag')).toBe('"first"');
    expect(metadataAsked()).toBe(1);
    expect(accessAsked()).toBe(1);

    // Read again, and the object is as it was: its details stay, and nothing is asked.
    await deleteOther('b.txt');
    expect(screen.getByRole('region', { name: 'Details of a.txt' })).toBe(region);
    expect(rowOf('a.txt').nextElementSibling).toBe(region.closest('tr'));
    expect(factOf(region, 'ETag')).toBe('"first"');
    expect(nameButton(rowOf('a.txt'), 'a.txt').getAttribute('aria-expanded')).toBe('true');
    expect(metadataAsked()).toBe(1);
    expect(accessAsked()).toBe(1);

    // Read again, and the object was replaced: its details stay open and are asked for anew.
    objects = objects.map((o) => (o.key === 'a.txt' ? { ...o, etag: '"second"' } : o));
    etag = '"second"';
    uploader = 'user/bob';
    await deleteOther('c.txt');
    await waitFor(() => {
      expect(factOf(screen.getByRole('region', { name: 'Details of a.txt' }), 'ETag')).toBe('"second"');
    });
    await waitFor(() => {
      expect(factOf(screen.getByRole('region', { name: 'Details of a.txt' }), 'Uploaded by')).toBe('user/bob');
    });
    expect(nameButton(rowOf('a.txt'), 'a.txt').getAttribute('aria-expanded')).toBe('true');
    expect(metadataAsked()).toBe(2);
    expect(accessAsked()).toBe(2);

    // Read again, and the object is gone: so are its details.
    objects = objects.filter((o) => o.key !== 'a.txt');
    await deleteOther('d.txt');
    await waitFor(() => {
      expect(screen.queryByLabelText('Select a.txt')).toBeNull();
    });
    expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
    expect(metadataAsked()).toBe(2);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('closes the details when another page is turned to', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': (call) =>
        call.query.get('continuation_token') === 'page-2'
          ? json(200, { objects: [object('b.txt')], common_prefixes: [], truncated: false, access_included: false })
          : json(200, {
              objects: [object('a.txt')],
              common_prefixes: [],
              truncated: true,
              access_included: false,
              next_continuation_token: 'page-2',
            }),
      'GET /buckets/photos/object/metadata': (call) => json(200, metadataOf(call.query.get('key') ?? '')),
      'GET /buckets/photos/object/access': () => json(200, access()),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(nameButton(await findRow('a.txt'), 'a.txt'));
    await within(screen.getByRole('region', { name: 'Details of a.txt' })).findByText('Bucket', { selector: 'dt' });

    fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
    await findRow('b.txt');
    expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
    expect(rowOf('b.txt').nextElementSibling).toBeNull();

    // Back on the page it was opened on, it is closed.
    fireEvent.click(screen.getByRole('button', { name: 'Previous page' }));
    const row = await findRow('a.txt');
    expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
    expect(row.nextElementSibling).toBeNull();
    expect(nameButton(row, 'a.txt').getAttribute('aria-expanded')).toBe('false');
    expect(detailsButton(row, 'a.txt').getAttribute('aria-expanded')).toBe('false');
    expect(api.callsTo('GET', '/buckets/photos/object/metadata')).toHaveLength(1);
  });

  it('closes the details when another prefix is walked into', async () => {
    signedIn({
      'GET /buckets/photos/objects': (call) =>
        call.query.get('prefix') === '2026/'
          ? json(200, { objects: [object('2026/inside.txt')], common_prefixes: [], truncated: false, access_included: false })
          : json(200, { objects: [object('top.txt')], common_prefixes: ['2026/'], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) => json(200, metadataOf(call.query.get('key') ?? '')),
      'GET /buckets/photos/object/access': () => json(200, access()),
    });
    window.location.hash = '#/buckets/photos';
    render(<App />);

    fireEvent.click(nameButton(await findRow('top.txt'), 'top.txt'));
    await within(screen.getByRole('region', { name: 'Details of top.txt' })).findByText('Bucket', { selector: 'dt' });

    window.location.hash = screen.getByRole('link', { name: '2026/' }).getAttribute('href') ?? '';
    await findRow('inside.txt');
    expect(screen.queryByRole('region', { name: 'Details of top.txt' })).toBeNull();

    window.location.hash = '#/buckets/photos';
    const row = await findRow('top.txt');
    expect(screen.queryByRole('region', { name: 'Details of top.txt' })).toBeNull();
    expect(nameButton(row, 'top.txt').getAttribute('aria-expanded')).toBe('false');
  });

  it('closes the details of an object that is deleted, and of no other', async () => {
    // The list is slow to forget what was deleted: that the details close is
    // the doing of the deletion, not of the object having left the list.
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt'), object('b.txt')], common_prefixes: [], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) => json(200, metadataOf(call.query.get('key') ?? '')),
      'GET /buckets/photos/object/access': () => json(200, access()),
      'DELETE /buckets/photos/object': () => noContent(),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(nameButton(await findRow('a.txt'), 'a.txt'));
    fireEvent.click(nameButton(rowOf('b.txt'), 'b.txt'));
    await within(screen.getByRole('region', { name: 'Details of a.txt' })).findByText('Bucket', { selector: 'dt' });
    await within(screen.getByRole('region', { name: 'Details of b.txt' })).findByText('Bucket', { selector: 'dt' });

    fireEvent.click(within(rowOf('a.txt')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('a.txt')).getByRole('button', { name: 'Yes, delete' }));

    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
    });
    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/objects')).toHaveLength(2);
    });
    expect(api.callsTo('DELETE', '/buckets/photos/object')[0]?.query.get('key')).toBe('a.txt');
    expect(screen.queryByRole('region', { name: 'Details of a.txt' })).toBeNull();
    expect(nameButton(rowOf('a.txt'), 'a.txt').getAttribute('aria-expanded')).toBe('false');
    const regionB = screen.getByRole('region', { name: 'Details of b.txt' });
    expect(rowOf('b.txt').nextElementSibling).toBe(regionB.closest('tr'));
    expect(factOf(regionB, 'Key')).toBe('b.txt');
  });

  it('keeps opening the details and ticking an object apart', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt'), object('b.txt')], common_prefixes: [], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': (call) => json(200, metadataOf(call.query.get('key') ?? '')),
      'GET /buckets/photos/object/access': () => json(200, access()),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    // The name opens the details and ticks nothing.
    const row = await findRow('a.txt');
    fireEvent.click(nameButton(row, 'a.txt'));
    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    await within(region).findByText('Bucket', { selector: 'dt' });
    expect((screen.getByLabelText('Select a.txt') as HTMLInputElement).checked).toBe(false);
    expect(screen.getByText('0 selected')).toBeDefined();
    expect(row.className).not.toContain('grid-chosen');

    // The tick opens nothing, and closes nothing.
    fireEvent.click(screen.getByLabelText('Select b.txt'));
    expect(screen.getByText('1 selected')).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Details of b.txt' })).toBeNull();
    expect(screen.getByRole('region', { name: 'Details of a.txt' })).toBe(region);

    // The row of the details is no object: all of the page is the two objects, and it has nothing to tick.
    fireEvent.click(screen.getByLabelText('Select all on this page'));
    expect(screen.getByText('2 selected')).toBeDefined();
    expect(within(row.closest('table') as HTMLTableElement).getAllByRole('checkbox')).toHaveLength(2);
    expect(region.closest('tr')?.className).toBe('grid-detail');
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    expect(screen.getByRole('group', { name: 'Delete 2 objects?' })).toBeDefined();
  });

  it('deletes an object and lists again', async () => {
    let objects = [object('a.txt')];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false, access_included: false }),
      'DELETE /buckets/photos/object': () => {
        objects = [];
        return noContent();
      },
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    expect(await screen.findByText('There is nothing here.')).toBeDefined();
    expect(api.callsTo('DELETE', '/buckets/photos/object')[0]?.query.get('key')).toBe('a.txt');
  });

  it('deletes the objects that are ticked, and only those', async () => {
    let objects = [object('a.txt'), object('b.txt'), object('c.txt')];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: ['dir/'], truncated: false, access_included: false }),
      'DELETE /buckets/photos/object': (call) => {
        objects = objects.filter((o) => o.key !== call.query.get('key'));
        return noContent();
      },
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    // Nothing is ticked: nothing can be deleted.
    const bulk = await screen.findByRole('button', { name: 'Delete selected' });
    expect((bulk as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText('0 selected')).toBeDefined();

    fireEvent.click(screen.getByLabelText('Select a.txt'));
    fireEvent.click(screen.getByLabelText('Select c.txt'));
    expect(screen.getByText('2 selected')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    // It asks first, and says how many.
    expect(screen.getByRole('group', { name: 'Delete 2 objects?' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));

    await waitFor(() => {
      expect(api.callsTo('DELETE', '/buckets/photos/object').map((call) => call.query.get('key'))).toEqual([
        'a.txt',
        'c.txt',
      ]);
    });
    await waitFor(() => {
      expect(screen.queryByLabelText('Select a.txt')).toBeNull();
    });
    expect(screen.getByLabelText('Select b.txt')).toBeDefined();
    expect(screen.getByText('0 selected')).toBeDefined();
  });

  it('deletes nothing that was ticked on another page', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': (call) =>
        call.query.get('continuation_token') === null
          ? json(200, {
              objects: [object('a.txt'), object('b.txt')],
              common_prefixes: [],
              truncated: true,
              access_included: false,
              next_continuation_token: 'b.txt',
            })
          : json(200, { objects: [object('z.txt')], common_prefixes: [], truncated: false, access_included: false }),
      'DELETE /buckets/photos/object': () => noContent(),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByLabelText('Select a.txt'));
    fireEvent.click(screen.getByLabelText('Select b.txt'));
    fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
    fireEvent.click(await screen.findByLabelText('Select z.txt'));
    expect(screen.getByText('1 selected')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    expect(screen.getByRole('group', { name: 'Delete 1 object?' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));

    await waitFor(() => {
      expect(api.callsTo('DELETE', '/buckets/photos/object')).toHaveLength(1);
    });
    expect(api.callsTo('DELETE', '/buckets/photos/object')[0]?.query.get('key')).toBe('z.txt');
    // Back on the first page nothing is ticked any more.
    fireEvent.click(screen.getByRole('button', { name: 'First page' }));
    expect(((await screen.findByLabelText('Select a.txt')) as HTMLInputElement).checked).toBe(false);
    expect(api.callsTo('DELETE', '/buckets/photos/object')).toHaveLength(1);
  });

  it('ticks every object of the page at once, and counts a refusal with its reason while the rest go on', async () => {
    const objects = [object('a.txt'), object('b.txt')];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false, access_included: false }),
      'DELETE /buckets/photos/object': (call) =>
        call.query.get('key') === 'a.txt' ? noContent() : problem(403, 'Access denied', undefined, 'req-9'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByLabelText('Select all on this page'));
    expect(screen.getByText('2 selected')).toBeDefined();
    expect((screen.getByLabelText('Select b.txt') as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Deleted 1 of 2 objects from photos: 1 not deleted');
    expect(alert.textContent).toContain('Access denied');
    expect(api.callsTo('DELETE', '/buckets/photos/object')).toHaveLength(2);
  });

  it('uploads several files at a time, each with a bar of its own, and tries every one of them', async () => {
    const stored: string[] = [];
    let allowA = false;
    let flaky = 2;
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: stored.map((k) => object(k)), common_prefixes: [], truncated: false, access_included: false }),
      'PUT /buckets/photos/object': (call) => {
        const key = call.query.get('key') ?? '';
        if (key.endsWith('a.txt') && !allowA) {
          return problem(403, 'Access denied', 'Not this one.', 'req-3');
        }
        // A node that is restarting says "not now" twice, then stores it.
        if (key.endsWith('c.txt') && flaky > 0) {
          flaky -= 1;
          return problem(503, 'No metadata leader');
        }
        stored.push(key);
        return json(200, {
          bucket: 'photos',
          key,
          size: call.body.length,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        });
      },
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="notes/" />);
    await screen.findByText('There is nothing here.');

    const names = ['a.txt', 'b.txt', 'c.txt', 'd.txt', 'e.txt', 'f.txt'];
    const files = names.map((name) => new NodeFile([name], name, { type: 'text/plain' }));
    fireEvent.change(screen.getByLabelText('File'), { target: { files } });
    // Several files: the field is the prefix they go under.
    expect((screen.getByLabelText('Upload under this prefix') as HTMLInputElement).value).toBe('notes/');
    expect(screen.getByText(/6 files chosen/)).toBeDefined();
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);

    // Every file is tried. One is refused, and that does not hold the others
    // up; one meets a node that says "not now" and is sent again.
    const progress = within(await screen.findByRole('list', { name: 'Upload progress' }));
    await waitFor(
      () => {
        expect(progress.getAllByText('Stored')).toHaveLength(5);
      },
      { timeout: 5000 },
    );
    expect(progress.getByText('Not stored')).toBeDefined();
    expect(screen.getByText('5 of 6 stored')).toBeDefined();
    expect(screen.getByText(/1 not stored/)).toBeDefined();
    // While files were on their way there was nothing to clear: see the test
    // of the button below. Now that none is, there is.
    await screen.findByRole('button', { name: 'Clear' });
    // Why, beside the file it is about.
    expect(progress.getByText('Access denied: Not this one.')).toBeDefined();
    const puts = () => api.callsTo('PUT', '/buckets/photos/object');
    expect(puts().filter((call) => call.query.get('key') === 'notes/a.txt')).toHaveLength(1);
    // The attempts at one file carry one Idempotency-Key: stored once, whatever happened to an answer.
    const attempts = puts().filter((call) => call.query.get('key') === 'notes/c.txt');
    expect(attempts).toHaveLength(3);
    expect(new Set(attempts.map((call) => call.headers.get('Idempotency-Key'))).size).toBe(1);
    expect(new Set(puts().map((call) => call.headers.get('Idempotency-Key'))).size).toBe(6);
    // Each file has a bar of its own, under its name.
    const bar = screen.getByRole('progressbar', { name: 'Upload of notes/b.txt' }) as HTMLProgressElement;
    expect(bar.value).toBe(1);
    expect(bar.previousElementSibling?.textContent).toContain('notes/b.txt');
    expect((screen.getByRole('progressbar', { name: 'Upload of notes/a.txt' }) as HTMLProgressElement).value).toBeLessThan(1);
    const sentB = puts().find((call) => call.query.get('key') === 'notes/b.txt');
    expect(sentB?.headers.get('Content-Type')).toBe('text/plain');
    expect(sentB?.body).toBe('b.txt');
    // What was stored is listed.
    await waitFor(() => {
      expect(screen.getByLabelText('Select b.txt')).toBeDefined();
    });

    // What failed, and only that, can be sent again.
    allowA = true;
    const before = puts().length;
    fireEvent.click(screen.getByRole('button', { name: 'Retry failed' }));
    await waitFor(() => {
      expect(screen.getByText('6 of 6 stored')).toBeDefined();
    });
    expect(puts().slice(before).map((call) => call.query.get('key'))).toEqual(['notes/a.txt']);
    expect(screen.queryByRole('button', { name: 'Retry failed' })).toBeNull();
  });

  it('clears what became of an upload when asked to, and not while files are on their way', async () => {
    const stored: string[] = [];
    // The uploads are held until the test lets them go.
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: stored.map((k) => object(k)), common_prefixes: [], truncated: false, access_included: false }),
      'PUT /buckets/photos/object': (call) => {
        const key = call.query.get('key') ?? '';
        if (key === 'bad.txt') {
          return problem(403, 'Access denied');
        }
        stored.push(key);
        return json(200, {
          bucket: 'photos',
          key,
          size: call.body.length,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        });
      },
    });
    // The fake's XMLHttpRequest answers through fetch: holding fetch for a
    // PUT holds the upload.
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (request.method === 'PUT') {
        await held;
      }
      return answer(request);
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    const files = ['good.txt', 'bad.txt'].map((name) => new NodeFile([name], name, { type: 'text/plain' }));
    fireEvent.change(screen.getByLabelText('File'), { target: { files } });
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);

    // On their way: there is a status, and no way to clear it.
    await screen.findByRole('list', { name: 'Upload progress' });
    expect(screen.getByText('0 of 2 stored')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Clear' })).toBeNull();

    release();
    const clear = await screen.findByRole('button', { name: 'Clear' });
    expect(screen.getByText('1 of 2 stored')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Retry failed' })).toBeDefined();
    expect(clear.getAttribute('title')).toBe('Clear the upload status');
    expect(clear.querySelector('.btn-text')?.textContent).toBe('Clear');
    expect(clear.querySelector('i.bi')).not.toBeNull();
    await screen.findByLabelText('Select good.txt');
    // Why a file was not stored stands in its tile, and nowhere else: the
    // page has no alert for it.
    expect(within(screen.getByRole('list', { name: 'Upload progress' })).getByText('Access denied')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    const puts = api.callsTo('PUT', '/buckets/photos/object').length;
    const lists = api.callsTo('GET', '/buckets/photos/objects').length;

    fireEvent.click(clear);

    // The summary, the tiles and the way to send again are gone...
    expect(screen.queryByRole('list', { name: 'Upload progress' })).toBeNull();
    expect(screen.queryByText('1 of 2 stored')).toBeNull();
    expect(screen.queryByText(/not stored/)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry failed' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Clear' })).toBeNull();
    // ...with what the upload was refused for, which went with its tile...
    expect(screen.queryByText('Access denied')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    // ...and nothing else is touched: the files chosen, the field, the objects.
    expect(screen.getByText(/2 files chosen/)).toBeDefined();
    expect(screen.getByLabelText('Upload under this prefix')).toBeDefined();
    expect(screen.getByLabelText('Select good.txt')).toBeDefined();
    expect(api.callsTo('PUT', '/buckets/photos/object')).toHaveLength(puts);
    expect(api.callsTo('GET', '/buckets/photos/objects')).toHaveLength(lists);
  });

  it('empties the form when its files are stored, unless other files were chosen meanwhile', async () => {
    const stored: string[] = [];
    // The uploads are held until the test lets them go.
    let release: () => void = () => undefined;
    let held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: stored.map((k) => object(k)), common_prefixes: [], truncated: false, access_included: false }),
      'PUT /buckets/photos/object': (call) => {
        const key = call.query.get('key') ?? '';
        stored.push(key);
        return json(200, metadataOf(key));
      },
    });
    const answer = globalThis.fetch;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (request.method === 'PUT') {
        await held;
      }
      return answer(request);
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');
    const choose = (name: string) => {
      fireEvent.change(screen.getByLabelText('File'), { target: { files: [new NodeFile([name], name, { type: 'text/plain' })] } });
    };
    const submit = () => {
      fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);
    };
    const keyField = () => screen.getByLabelText('Object key') as HTMLInputElement;

    choose('one.txt');
    expect(keyField().value).toBe('one.txt');
    submit();
    await screen.findByText('0 of 1 stored');

    // While it is on its way another file is chosen, for the next upload.
    choose('two.txt');
    expect(keyField().value).toBe('two.txt');
    release();
    await screen.findByText('1 of 1 stored');
    await screen.findByLabelText('Select one.txt');
    await waitFor(() => {
      expect((screen.getByRole('button', { name: 'Upload' }) as HTMLButtonElement).disabled).toBe(false);
    });
    // The first is stored, and the choice made meanwhile is still there.
    expect(keyField().value).toBe('two.txt');
    expect(api.callsTo('PUT', '/buckets/photos/object').map((call) => call.query.get('key'))).toEqual(['one.txt']);

    // It is sent as it stands, with nothing chosen again...
    held = new Promise<void>((resolve) => {
      release = resolve;
    });
    submit();
    await waitFor(() => {
      expect((screen.getByRole('button', { name: 'Upload' }) as HTMLButtonElement).disabled).toBe(true);
    });
    release();
    await screen.findByText('2 of 2 stored');
    expect(api.callsTo('PUT', '/buckets/photos/object').map((call) => call.query.get('key'))).toEqual(['one.txt', 'two.txt']);
    // ...and this time nothing else was chosen meanwhile: the form is empty again.
    await waitFor(() => {
      expect(keyField().value).toBe('');
    });
  });

  it('lists what was uploaded into an empty bucket, with nothing more asked of the user', async () => {
    const stored: string[] = [];
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: stored.map((k) => object(k)), common_prefixes: [], truncated: false, access_included: false }),
      'PUT /buckets/photos/object': (call) => {
        const key = call.query.get('key') ?? '';
        stored.push(key);
        return json(200, {
          bucket: 'photos',
          key,
          size: call.body.length,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        });
      },
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    const names = ['one.txt', 'two.txt', 'three.txt', 'four.txt'];
    const files = names.map((name) => new NodeFile([name], name, { type: 'text/plain' }));
    fireEvent.change(screen.getByLabelText('File'), { target: { files } });
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);

    await screen.findByText('4 of 4 stored');
    // Every one of them is in the grid, and the bucket is no longer called
    // empty - without a click, a reload or a change of page.
    for (const name of names) {
      expect(await screen.findByLabelText(`Select ${name}`)).toBeDefined();
    }
    expect(screen.queryByText('There is nothing here.')).toBeNull();
    // The listing was asked for again after the last file was stored.
    const lastPut = api.calls.findLastIndex((call) => call.method === 'PUT');
    const lastList = api.calls.findLastIndex((call) => call.method === 'GET' && call.path === '/buckets/photos/objects');
    expect(lastList).toBeGreaterThan(lastPut);
    expect(api.callsTo('GET', '/buckets/photos/objects')).toHaveLength(2);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('does not call a bucket empty when the list could not be read again, and reads it again when asked', async () => {
    const stored: string[] = [];
    let failing = false;
    fakeApi({
      'GET /buckets/photos/objects': () =>
        failing
          ? json(503, { type: 'about:blank', title: 'Temporarily unavailable', status: 503, code: 'Unavailable' })
          : json(200, { objects: stored.map((k) => object(k)), common_prefixes: [], truncated: false, access_included: false }),
      'PUT /buckets/photos/object': (call) => {
        const key = call.query.get('key') ?? '';
        stored.push(key);
        return json(200, {
          bucket: 'photos',
          key,
          size: call.body.length,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        });
      },
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    // The file is stored, and the list that follows cannot be read.
    failing = true;
    const file = new NodeFile(['one'], 'one.txt', { type: 'text/plain' });
    fireEvent.change(screen.getByLabelText('File'), { target: { files: [file] } });
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);
    await screen.findByText('1 of 1 stored');

    // What is shown is the list of before: it is said to be that, and the
    // bucket is not said to hold nothing.
    await screen.findByText(/The list could not be read again/);
    expect(screen.queryByText('There is nothing here.')).toBeNull();

    failing = false;
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }));
    expect(await screen.findByLabelText('Select one.txt')).toBeDefined();
    expect(screen.queryByText(/The list could not be read again/)).toBeNull();
  });

  it('shows who uploaded each object and how often it was downloaded, when the listing carries it', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, {
          objects: [
            {
              ...object('read.txt'),
              access: {
                uploaded_by: 'user/alice',
                uploaded_at: '2026-10-02T08:00:00Z',
                download_count: 1234,
                copy_count: 2,
                last_downloaded_at: '2026-10-05T10:00:00Z',
              },
            },
            // Uploaded before uploaders were recorded, and never read.
            { ...object('old.txt'), access: { uploaded_at: CREATED, download_count: 0, copy_count: 0 } },
            // One the caller may not see it of: the API says nothing.
            object('secret.txt'),
          ],
          common_prefixes: ['dir/'],
          truncated: false,
          access_included: true,
        }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const table = (await screen.findByRole('table')) as HTMLTableElement;
    expect(within(table).getAllByRole('columnheader').map((th) => th.textContent)).toEqual([
      'Select',
      'Name',
      'Size',
      'Last modified',
      'Uploaded by',
      'Uploaded',
      'Downloads',
      'Copies',
      'Last downloaded',
      'Actions',
    ]);
    // Wide: it is scrolled inside its panel, the page is not.
    expect(table.className).toContain('grid-wide');
    expect(table.className).toContain('grid-keep');
    expect(table.parentElement?.className).toContain('table-responsive');

    // The five cells after "Last modified" of the row of an object.
    const facts = (name: string) => {
      const row = within(table).getByLabelText(`Select ${name}`).closest('tr') as HTMLTableRowElement;
      const cells = within(row).getAllByRole('cell');
      expect(cells).toHaveLength(10);
      expect(cells.map((cell) => cell.getAttribute('data-label')).slice(4, 9)).toEqual([
        'Uploaded by',
        'Uploaded',
        'Downloads',
        'Copies',
        'Last downloaded',
      ]);
      return cells.slice(4, 9).map((cell) => cell.textContent);
    };
    // Worded as the details of an object word the same facts.
    expect(facts('read.txt')).toEqual([
      'user/alice',
      new Date('2026-10-02T08:00:00Z').toLocaleString(),
      '1234',
      '2',
      new Date('2026-10-05T10:00:00Z').toLocaleString(),
    ]);
    expect(facts('old.txt')).toEqual(['not recorded', new Date(CREATED).toLocaleString(), '0', '0', 'never']);
    expect(facts('secret.txt')).toEqual(['', '', '', '', '']);
    // A folder has none of it: its cells are there, and empty.
    const folder = within(table).getByRole('link', { name: 'dir/' }).closest('tr') as HTMLTableRowElement;
    const folderCells = within(folder).getAllByRole('cell');
    expect(folderCells).toHaveLength(10);
    expect(folderCells.slice(4, 9).map((cell) => cell.textContent)).toEqual(['', '', '', '', '']);
    expect(folderCells[9]?.getAttribute('data-label')).toBe('Actions');
    // The counts are figures in a column of figures.
    const downloads = within(table).getByRole('columnheader', { name: 'Downloads' });
    expect(downloads.className).toContain('grid-number');
    expect(within(table).getByRole('columnheader', { name: 'Copies' }).className).toContain('grid-number');
    expect(within(table).getByRole('columnheader', { name: 'Uploaded by' }).className).not.toContain('grid-number');

    // All of it came with the one listing: no question an object. The page's
    // other questions are of the bucket - its details and its usage - and of
    // no object.
    const ofObjects = api.calls.filter((call) => call.path !== '/buckets' && call.path !== '/buckets/photos/usage');
    expect(ofObjects).toHaveLength(1);
    expect(ofObjects[0]?.query.get('access')).toBe('true');
    expect(api.callsTo('GET', '/buckets/photos/object/access')).toHaveLength(0);
  });

  it('shows none of those columns when the listing does not carry them', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt')], common_prefixes: ['dir/'], truncated: false, access_included: false }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const table = (await screen.findByRole('table')) as HTMLTableElement;
    const headings = within(table)
      .getAllByRole('columnheader')
      .map((th) => th.textContent);
    expect(headings).toEqual(['Select', 'Name', 'Size', 'Last modified', 'Actions']);
    for (const heading of ['Uploaded by', 'Uploaded', 'Downloads', 'Copies', 'Last downloaded']) {
      expect(headings).not.toContain(heading);
      expect(table.querySelector(`[data-label="${heading}"]`)).toBeNull();
    }
    // No empty columns either: a row has the cells it always had.
    for (const row of within(table).getAllByRole('row').slice(1)) {
      expect(within(row).getAllByRole('cell')).toHaveLength(5);
    }
    expect(table.className).not.toContain('grid-wide');
    // It was asked for all the same: the API said no, and that is no failure.
    expect(api.calls[0]?.query.get('access')).toBe('true');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('gives every button a hint, and the buttons of a row their name as one', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt')], common_prefixes: ['dir/'], truncated: false, access_included: false }),
      'GET /buckets/photos/object/metadata': () => json(200, metadataOf('a.txt')),
      'GET /buckets/photos/object/access': () => json(200, access()),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByLabelText('Select a.txt');

    for (const control of [...screen.getAllByRole('button'), ...screen.getAllByRole('link')]) {
      if (control.className.includes('btn')) {
        expect(control.getAttribute('title'), control.textContent).toBeTruthy();
      }
    }
    // In a row the icon is the button: its name is still there to be read
    // out, in the element the stylesheet folds away.
    const details = screen.getByRole('button', { name: 'Details of a.txt' });
    expect(details.getAttribute('title')).toBe('Show details');
    expect(details.querySelector('.btn-text')?.textContent).toBe('Details');
    expect(details.querySelector('i.bi')).not.toBeNull();
    // The name of an object is a button too, with the same hint and a mark
    // beside it; a folder keeps its link.
    const name = nameButton(rowOf('a.txt'), 'a.txt');
    expect(name.getAttribute('title')).toBe('Show details');
    expect(name.querySelector('i.bi')).not.toBeNull();
    expect(screen.getByRole('link', { name: 'dir/' }).getAttribute('title')).toBe('Open this folder');
    const remove = screen.getByRole('button', { name: 'Delete' });
    expect(remove.getAttribute('title')).toContain('Delete a.txt?');
    expect(remove.querySelector('.btn-text')?.textContent).toBe('Delete');

    // Opened, the row has buttons of its own: they have hints as well.
    fireEvent.click(name);
    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    const close = await within(region).findByRole('button', { name: 'Close details' });
    expect(close.getAttribute('title')).toBe('Close the details');
    expect(close.querySelector('.btn-text')?.textContent).toBe('Close details');
    for (const control of [...screen.getAllByRole('button'), ...screen.getAllByRole('link')]) {
      if (control.className.includes('btn')) {
        expect(control.getAttribute('title'), control.textContent).toBeTruthy();
      }
    }
    // Nothing of the details is styled in the page itself: the policy of the server forbids it.
    expect(region.closest('tr')?.querySelector('[style]')).toBeNull();
  });

  it('keeps the objects in a table, whatever the width of the screen', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects: [object('a.txt')], common_prefixes: [], truncated: false, access_included: false }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const table = (await screen.findByRole('table')) as HTMLTableElement;
    // The class that keeps it from being turned into cards on a phone.
    expect(table.className).toContain('grid-keep');
    expect(within(table).getAllByRole('columnheader').map((th) => th.textContent)).toEqual([
      'Select',
      'Name',
      'Size',
      'Last modified',
      'Actions',
    ]);
  });

  it('uploads a file under the current prefix and lists again', async () => {
    let objects: ReturnType<typeof object>[] = [];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false, access_included: false }),
      'PUT /buckets/photos/object': (call) => {
        objects = [object(call.query.get('key') ?? '', call.body.length)];
        return json(200, {
          bucket: 'photos',
          key: call.query.get('key'),
          size: call.body.length,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        });
      },
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="notes/" />);
    await screen.findByText('There is nothing here.');

    const file = new NodeFile(['hello'], 'hello.txt', { type: 'text/plain' });
    fireEvent.change(screen.getByLabelText('File'), { target: { files: [file] } });
    expect((screen.getByLabelText('Object key') as HTMLInputElement).value).toBe('notes/hello.txt');
    // jsdom does not count a file set by a test as a value of a required
    // field, so submit the form itself rather than click its button.
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);

    expect(await screen.findByText('hello.txt')).toBeDefined();
    const [put] = api.callsTo('PUT', '/buckets/photos/object');
    expect(put?.query.get('key')).toBe('notes/hello.txt');
    expect(put?.headers.get('Content-Type')).toBe('text/plain');
    expect(put?.body).toBe('hello');
    await waitFor(() => {
      expect((screen.getByLabelText('Object key') as HTMLInputElement).value).toBe('');
    });
  });

  it('shows the problem of the API when a listing is refused', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () => problem(404, 'No such bucket', 'There is no bucket photos.', 'req-7'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('No such bucket');
    expect(alert.textContent).toContain('req-7');
  });
});
