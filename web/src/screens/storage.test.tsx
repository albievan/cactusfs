import { File as NodeFile } from 'node:buffer';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { App } from '../App';
import { fakeApi, json, noContent, problem, signedIn } from '../test/fakeApi';
import { BucketsScreen } from './BucketsScreen';
import { ObjectsScreen } from './ObjectsScreen';

const CREATED = '2026-10-01T00:00:00Z';

function object(key: string, size = 10) {
  return { key, size, etag: '"e"', last_modified: CREATED };
}

describe('buckets', () => {
  it('lists the buckets with links to their objects', async () => {
    fakeApi({ 'GET /buckets': () => json(200, [{ name: 'photos', created_at: CREATED }]) });
    render(<BucketsScreen />);

    const link = await screen.findByRole('link', { name: 'Open photos' });
    expect(link.getAttribute('href')).toBe('#/buckets/photos');
  });

  it('shows how many objects each bucket holds and how much they are together', async () => {
    const api = fakeApi({
      'GET /buckets': () =>
        json(200, [
          { name: 'photos', created_at: CREATED },
          { name: 'huge', created_at: CREATED },
          { name: 'private', created_at: CREATED },
          { name: 'later', created_at: CREATED },
        ]),
      'GET /buckets/photos/usage': () => json(200, { object_count: 1234, total_size: 5 * 1024 * 1024, complete: true }),
      'GET /buckets/huge/usage': () => json(200, { object_count: 1000000, total_size: 1024, complete: false }),
      'GET /buckets/private/usage': () => problem(403, 'Access denied'),
    });
    let down = true;
    api.on({
      'GET /buckets/later/usage': () =>
        down ? problem(503, 'No metadata leader') : json(200, { object_count: 1, total_size: 3, complete: true }),
    });
    render(<BucketsScreen />);

    const photos = within((await screen.findByRole('heading', { name: 'photos' })).closest('article') as HTMLElement);
    expect(await photos.findByText(/1,234\s+objects/)).toBeDefined();
    expect(photos.getByText('5.0 MiB')).toBeDefined();
    // A count that stopped at its bound says so.
    const huge = within(screen.getByRole('heading', { name: 'huge' }).closest('article') as HTMLElement);
    expect(await huge.findByText(/1,000,000\s+or more\s+objects/)).toBeDefined();
    // Whoever may not list a bucket is shown no figure, and no alarm.
    const hidden = within(screen.getByRole('heading', { name: 'private' }).closest('article') as HTMLElement);
    expect(await hidden.findByText('Usage not shown')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    // A figure that could not be had just now can be asked for again.
    const later = within(screen.getByRole('heading', { name: 'later' }).closest('article') as HTMLElement);
    expect(await later.findByText(/Usage unavailable/)).toBeDefined();
    down = false;
    fireEvent.click(later.getByRole('button', { name: 'Count again' }));
    expect(await later.findByText(/1\s+object$/)).toBeDefined();
  });

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
        }),
    });
    render(<ObjectsScreen bucket="photos" prefix="2026/" />);

    const folder = await screen.findByRole('link', { name: 'january/' });
    expect(folder.getAttribute('href')).toBe('#/buckets/photos?prefix=2026%2Fjanuary%2F');
    expect(screen.getByText('readme.txt')).toBeDefined();
    expect(screen.getByText('2.0 KiB')).toBeDefined();

    const [call] = api.calls;
    expect(call?.query.get('prefix')).toBe('2026/');
    expect(call?.query.get('delimiter')).toBe('/');
    expect(call?.query.has('continuation_token')).toBe(false);
  });

  it('sends no prefix at the top of the bucket', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects: [], common_prefixes: [], truncated: false }),
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

    expect(await screen.findByText('There is nothing here.')).toBeDefined();
    expect(api.calls[0]?.query.has('prefix')).toBe(false);
  });

  it('shows the way back up as breadcrumbs', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects: [], common_prefixes: [], truncated: false }),
    });
    render(<ObjectsScreen bucket="photos" prefix="2026/january/" />);

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
          ? json(200, { objects: [object('2026/inside.txt')], common_prefixes: [], truncated: false })
          : json(200, { objects: [], common_prefixes: ['2026/'], truncated: false }),
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
          ? json(200, { objects: [object('b.txt')], common_prefixes: [], truncated: false })
          : json(200, {
              objects: [object('a.txt')],
              common_prefixes: [],
              truncated: true,
              next_continuation_token: 'page-2',
            }),
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

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
        json(200, { objects: [object('2026/a b.jpg')], common_prefixes: [], truncated: false }),
    });
    render(<ObjectsScreen bucket="photos" prefix="2026/" />);

    const link = await screen.findByRole('link', { name: 'Download a b.jpg' });
    expect(link.getAttribute('href')).toBe('/api/v1/buckets/photos/object?key=2026%2Fa+b.jpg');
    expect(link.hasAttribute('download')).toBe(true);
  });

  it('shows what is known about an object', async () => {
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: [object('a.txt')], common_prefixes: [], truncated: false }),
      'GET /buckets/photos/object/metadata': () =>
        json(200, {
          bucket: 'photos',
          key: 'a.txt',
          size: 10,
          etag: '"0123abcd"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
        }),
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));

    expect(await screen.findByRole('heading', { name: 'Object details' })).toBeDefined();
    expect(screen.getByText('"0123abcd"')).toBeDefined();
    expect(screen.getByText('text/plain')).toBeDefined();
    expect(screen.getByText('SSE-S3')).toBeDefined();
    expect(api.callsTo('GET', '/buckets/photos/object/metadata')[0]?.query.get('key')).toBe('a.txt');

    fireEvent.click(screen.getByRole('button', { name: 'Close details' }));
    expect(screen.queryByRole('heading', { name: 'Object details' })).toBeNull();
  });

  it('deletes an object and lists again', async () => {
    let objects = [object('a.txt')];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false }),
      'DELETE /buckets/photos/object': () => {
        objects = [];
        return noContent();
      },
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    expect(await screen.findByText('There is nothing here.')).toBeDefined();
    expect(api.callsTo('DELETE', '/buckets/photos/object')[0]?.query.get('key')).toBe('a.txt');
  });

  it('deletes the objects that are ticked, and only those', async () => {
    let objects = [object('a.txt'), object('b.txt'), object('c.txt')];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: ['dir/'], truncated: false }),
      'DELETE /buckets/photos/object': (call) => {
        objects = objects.filter((o) => o.key !== call.query.get('key'));
        return noContent();
      },
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

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
              next_continuation_token: 'b.txt',
            })
          : json(200, { objects: [object('z.txt')], common_prefixes: [], truncated: false }),
      'DELETE /buckets/photos/object': () => noContent(),
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

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

  it('ticks every object of the page at once, and stops at a refusal', async () => {
    const objects = [object('a.txt'), object('b.txt')];
    const api = fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false }),
      'DELETE /buckets/photos/object': (call) =>
        call.query.get('key') === 'a.txt' ? noContent() : problem(403, 'Access denied', undefined, 'req-9'),
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

    fireEvent.click(await screen.findByLabelText('Select all on this page'));
    expect(screen.getByText('2 selected')).toBeDefined();
    expect((screen.getByLabelText('Select b.txt') as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Delete selected' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete selected' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(api.callsTo('DELETE', '/buckets/photos/object')).toHaveLength(2);
  });

  it('uploads several files at a time, each with a bar of its own, and begins no more after a refusal', async () => {
    const stored: string[] = [];
    const api = fakeApi({
      'GET /buckets/photos/objects': () =>
        json(200, { objects: stored.map((k) => object(k)), common_prefixes: [], truncated: false }),
      'PUT /buckets/photos/object': (call) => {
        const key = call.query.get('key') ?? '';
        if (key.endsWith('a.txt')) {
          return problem(403, 'Access denied', undefined, 'req-3');
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
    render(<ObjectsScreen bucket="photos" prefix="notes/" />);
    await screen.findByText('There is nothing here.');

    const names = ['a.txt', 'b.txt', 'c.txt', 'd.txt', 'e.txt'];
    const files = names.map((name) => new NodeFile([name], name, { type: 'text/plain' }));
    fireEvent.change(screen.getByLabelText('File'), { target: { files } });
    // Several files: the field is the prefix they go under.
    expect((screen.getByLabelText('Upload under this prefix') as HTMLInputElement).value).toBe('notes/');
    expect(screen.getByText(/5 files chosen/)).toBeDefined();
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);

    // Four are on their way at once, each with a request of its own. The
    // first is refused: the three beside it finish, the fifth is not begun.
    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(api.callsTo('PUT', '/buckets/photos/object').map((call) => call.query.get('key'))).toEqual([
      'notes/a.txt',
      'notes/b.txt',
      'notes/c.txt',
      'notes/d.txt',
    ]);
    const progress = within(screen.getByRole('list', { name: 'Upload progress' }));
    await waitFor(() => {
      expect(progress.getAllByText('Stored')).toHaveLength(3);
    });
    expect(progress.getByText('Not stored')).toBeDefined();
    expect(progress.getByText('Waiting')).toBeDefined();
    // Each file has a bar of its own: full for what was stored, empty for
    // what was never begun.
    expect((screen.getByRole('progressbar', { name: 'Upload of notes/b.txt' }) as HTMLProgressElement).value).toBe(1);
    expect((screen.getByRole('progressbar', { name: 'Upload of notes/e.txt' }) as HTMLProgressElement).value).toBe(0);
    // The request of an upload carries what every changing request does.
    const sent = api.callsTo('PUT', '/buckets/photos/object')[1];
    expect(sent?.headers.get('Idempotency-Key')).not.toBeNull();
    expect(sent?.headers.get('Content-Type')).toBe('text/plain');
    expect(sent?.body).toBe('b.txt');
    // What was stored is listed.
    await waitFor(() => {
      expect(screen.getByLabelText('Select b.txt')).toBeDefined();
    });
  });

  it('keeps the objects in a table, whatever the width of the screen', async () => {
    fakeApi({
      'GET /buckets/photos/objects': () => json(200, { objects: [object('a.txt')], common_prefixes: [], truncated: false }),
    });
    render(<ObjectsScreen bucket="photos" prefix="" />);

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
      'GET /buckets/photos/objects': () => json(200, { objects, common_prefixes: [], truncated: false }),
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
    render(<ObjectsScreen bucket="photos" prefix="notes/" />);
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
    render(<ObjectsScreen bucket="photos" prefix="" />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('No such bucket');
    expect(alert.textContent).toContain('req-7');
  });
});
