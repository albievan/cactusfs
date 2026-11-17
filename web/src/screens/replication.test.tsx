import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import { describe, expect, it } from 'vitest';

import { DeletionsProvider } from '../deletions/DeletionsProvider';
import { CSRF_TOKEN, fakeApi, json, refusal, signedIn, type Call } from '../test/fakeApi';
import { UploadsProvider } from '../uploads/UploadsProvider';
import {
  BucketReplicaSection,
  BucketReplicationSection,
  REPLICATION_STATUS_HINTS,
  RESYNC_STARTED,
  resyncQuestion,
} from './BucketReplication';
import { ObjectsScreen } from './ObjectsScreen';

const CREATED = '2026-11-01T00:00:00Z';
const SOURCE = 'site-a-0123';

/** What the API answers a caller it does not tell, or does not let change something. */
const DENIED = () => refusal(403, 'AccessDenied', 'Access denied');

/** What the API answers a path it does not know: a node of an older release, during a rolling upgrade. */
const NO_SUCH_ENDPOINT = () => refusal(404, 'NotFound', 'No such endpoint');

/** What the API answers a bucket without a replication configuration. */
const NOT_REPLICATED = () => refusal(404, 'NoSuchEntity', 'No such entity');

/** A refusal with its code and the detail the API gives. */
function refusedWith(status: number, code: string, title: string, detail: string): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title, status, code, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

/** That a change was sent as every change of a session is: with the CSRF token and an Idempotency-Key of its own. */
function expectSentAsAChange(call: Call | undefined) {
  expect(call?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
  expect(call?.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
}

/** That every button styled as one has a hint: on a phone its text folds away. */
function expectEveryButtonHasATitle() {
  for (const control of screen.getAllByRole('button')) {
    if (control.className.includes('btn')) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
  }
}

const CONFIG = {
  target: 'site-b',
  destination_bucket: 'photos-copy',
  rules: [
    { id: 'logs', enabled: true, priority: 2, prefix: 'logs/', delete_markers: true },
    { id: 'off', enabled: false, priority: 0, delete_markers: false },
  ],
};

const STATUS = {
  target: 'site-b',
  target_state: 'stalled',
  last_error_class: 'not_replica',
  pending: 1204,
  oldest_pending_at: CREATED,
  failed: 3,
  lost: 7,
  resync_needed: true,
  resync: { state: 'running', scanned: 1500, sent: 12 },
};

/** The answers for the replication of bucket "photos": its configuration and its status. */
function replicated(status: () => Response = () => json(200, STATUS)) {
  return {
    'GET /buckets/photos/replication': () => json(200, CONFIG),
    'GET /buckets/photos/replication/status': status,
  };
}

/** What the list of facts says of one: the value beside its name. */
function factOf(name: string): string | null | undefined {
  return screen.getByText(name, { selector: 'dt' }).nextElementSibling?.textContent;
}

const ruleRow = (id: string) => screen.getByText(id, { selector: 'td' }).closest('tr') as HTMLTableRowElement;

const cellOf = (id: string, label: string) =>
  ruleRow(id).querySelector(`td[data-label="${label}"]`)?.textContent;

describe('the replication of a bucket', () => {
  const unfold = () => {
    fireEvent.click(screen.getByRole('button', { name: 'Replication' }));
  };

  it('is folded until asked for, then shows the target, the destination bucket, the rules and the status', async () => {
    const api = signedIn(replicated());
    render(<BucketReplicationSection bucket="photos" />);
    expect(api.callsTo('GET', '/buckets/photos/replication')).toHaveLength(0);

    unfold();
    await screen.findByRole('table');
    expect(factOf('Target')).toBe('site-b');
    expect(factOf('Destination bucket')).toBe('photos-copy');
    const rows = screen.getAllByRole('row').slice(1);
    expect(rows.map((row) => row.querySelector('td')?.textContent)).toEqual(['logs', 'off']);
    expect(cellOf('logs', 'Status')).toBe('Enabled');
    expect(cellOf('logs', 'Priority')).toBe('2');
    expect(cellOf('logs', 'Keys')).toBe('beginning with logs/');
    expect(cellOf('logs', 'Delete markers')).toBe('copied');
    expect(cellOf('off', 'Status')).toBe('Disabled');
    expect(cellOf('off', 'Keys')).toBe('every key');
    expect(cellOf('off', 'Delete markers')).toBe('not copied');

    await screen.findByText('State', { selector: 'dt' });
    const state = screen.getByText('stalled');
    expect(state.className).toBe('state state-offline');
    const errorClass = screen.getByText('not_replica', { selector: 'code' });
    expect(errorClass.getAttribute('title')).toContain('not a replica of this deployment');
    expect(factOf('Pending')).toContain('1,204');
    expect(factOf('Failed')).toContain('3');
    expect(factOf('Lost')).toBe('7 changes for the target, of any bucket, dropped before they were sent');
    expect(factOf('Waiting since')).toBeTruthy();
    expect(factOf('Resync')).toBe('running: 1,500 versions looked at, 12 sent');
    expect(
      screen.getByText(
        'Changes for the target, of any bucket that uses it, were dropped before they were sent: the other deployment lacks versions until a resync has been through.',
      ),
    ).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('says what is not there: no error class, nothing lost, no resync asked for', async () => {
    signedIn(
      replicated(() =>
        json(200, { target: 'site-b', target_state: 'ok', pending: 0, failed: 0, lost: 0, resync_needed: false }),
      ),
    );
    render(<BucketReplicationSection bucket="photos" />);
    unfold();

    await screen.findByText('State', { selector: 'dt' });
    expect(screen.getByText('ok').className).toBe('state state-online');
    expect(factOf('Last error')).toBe('none');
    expect(factOf('Resync')).toBe('none asked for');
    expect(screen.queryByText('Lost', { selector: 'dt' })).toBeNull();
    expect(screen.queryByText('Waiting since', { selector: 'dt' })).toBeNull();
    expect(screen.queryByText(/^Changes for the target/)).toBeNull();
  });

  it('says that a bucket without a configuration is not replicated, and asks no status', async () => {
    const api = signedIn({ 'GET /buckets/photos/replication': NOT_REPLICATED });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('This bucket is not replicated: its versions are copied nowhere.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.callsTo('GET', '/buckets/photos/replication/status')).toHaveLength(0);
    expect(screen.queryByRole('button', { name: 'Resync' })).toBeNull();
  });

  it('is hidden from a caller the API does not show it (403), in a line and without an alert', async () => {
    const api = signedIn({ 'GET /buckets/photos/replication': DENIED });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('You may not see the replication of this bucket.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.callsTo('GET', '/buckets/photos/replication/status')).toHaveLength(0);
  });

  it('offers a resync under the 403 of the configuration: the API judges admin:ResyncReplication apart', async () => {
    const api = signedIn({
      'GET /buckets/photos/replication': DENIED,
      'POST /buckets/photos/replication/resync': () => json(202, { state: 'running', scanned: 0, sent: 0 }),
    });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('You may not see the replication of this bucket.');

    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    expect(screen.getByRole('group', { name: resyncQuestion('photos') }).textContent).toContain(
      'It walks every version of the bucket',
    );
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));
    expect(await screen.findByText('Resync started. It goes on in the background.')).toBeDefined();
    expectSentAsAChange(api.callsTo('POST', '/buckets/photos/replication/resync')[0]);
  });

  it('shows a resync refused (403) under the 403 of the configuration in the API\'s words', async () => {
    signedIn({ 'GET /buckets/photos/replication': DENIED, 'POST /buckets/photos/replication/resync': DENIED });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('You may not see the replication of this bucket.');

    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
  });

  it('says "not counted yet" for pending while the usage of the bucket is not exact, never 0', async () => {
    signedIn(replicated(() => json(200, { ...STATUS, pending: 0 })));
    render(<BucketReplicationSection bucket="photos" exact={false} />);
    unfold();

    await screen.findByText('State', { selector: 'dt' });
    expect(factOf('Pending')).toBe('not counted yet: the usage of this bucket is still being counted');
  });

  it('says that a refresh was not answered, and keeps what was read, with the reason in the hint', async () => {
    let status = () => json(200, STATUS);
    signedIn(replicated(() => status()));
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('State', { selector: 'dt' });
    expect(screen.queryByText('Could not ask again; shown as last read.')).toBeNull();

    status = () => refusedWith(503, 'ServiceUnavailable', 'Service unavailable', 'no leader to ask');
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    const line = await screen.findByText('Could not ask again; shown as last read.');
    expect(line.getAttribute('title')).toContain('no leader to ask');
    expect(factOf('Pending')).toContain('1,204');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows the record the start of a resync answered while the status cannot be read again', async () => {
    let status = () => json(200, STATUS);
    signedIn({
      ...replicated(() => status()),
      'POST /buckets/photos/replication/resync': () => json(202, { state: 'running', scanned: 0, sent: 0 }),
    });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('State', { selector: 'dt' });
    expect(factOf('Resync')).toBe('running: 1,500 versions looked at, 12 sent');

    // The status read after the start fails: the 202's record is all there is to show.
    status = () => refusedWith(503, 'ServiceUnavailable', 'Service unavailable', 'no leader to ask');
    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));

    expect(await screen.findByText('Could not ask again; shown as last read.')).toBeDefined();
    expect(factOf('Resync')).toBe('running: 0 versions looked at, 0 sent');
  });

  it('shows the configuration to a caller the API does not tell the status (403), and says so in a line', async () => {
    signedIn(replicated(DENIED));
    render(<BucketReplicationSection bucket="photos" />);
    unfold();

    expect(
      await screen.findByText('How far the replication of this bucket has come is not visible to you.'),
    ).toBeDefined();
    expect(factOf('Target')).toBe('site-b');
    expect(screen.queryByRole('alert')).toBeNull();
    // Whether the caller may resync is the API's to say.
    expect(screen.getByRole('button', { name: 'Resync' })).toBeDefined();
  });

  it('says a status the API could not give (503) is unknown, with its words in the hint', async () => {
    signedIn(replicated(() => refusedWith(503, 'ServiceUnavailable', 'Service unavailable', 'no leader to ask')));
    render(<BucketReplicationSection bucket="photos" />);
    unfold();

    const line = await screen.findByText('How far the replication of this bucket has come is unknown now.');
    expect(line.getAttribute('title')).toContain('no leader to ask');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('explains a node of an older release, and asks again', async () => {
    const api = signedIn({ 'GET /buckets/photos/replication': NO_SUCH_ENDPOINT });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();

    expect((await screen.findByRole('alert')).textContent).toContain(
      'Replication is not available on the node that answered (an older release); try again.',
    );
    api.on(replicated());
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('table')).toBeDefined();
  });

  it('starts a resync only after a question that says it walks every version, and reads the status again', async () => {
    let status: unknown = STATUS;
    const api = signedIn({
      ...replicated(() => json(200, status)),
      'POST /buckets/photos/replication/resync': () => json(202, { state: 'running', scanned: 0, sent: 0 }),
    });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('State', { selector: 'dt' });

    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    const question = screen.getByRole('group', { name: resyncQuestion('photos') });
    expect(question.textContent).toContain('It walks every version of the bucket');
    // Put aside, nothing is sent.
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('POST', '/buckets/photos/replication/resync')).toHaveLength(0);

    status = { ...STATUS, resync: { state: 'running', scanned: 0, sent: 0 } };
    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));

    expect(await screen.findByText(RESYNC_STARTED)).toBeDefined();
    const [post] = api.callsTo('POST', '/buckets/photos/replication/resync');
    expectSentAsAChange(post);
    expect(post?.headers.get('Content-Type')).toBe('application/json');
    expect(post?.body).toBe('');
    await waitFor(() => {
      expect(factOf('Resync')).toBe('running: 0 versions looked at, 0 sent');
    });
    expect(api.callsTo('GET', '/buckets/photos/replication/status')).toHaveLength(2);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows a resync the API refuses (403) in its words, and the status as it was', async () => {
    signedIn({ ...replicated(), 'POST /buckets/photos/replication/resync': DENIED });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('State', { selector: 'dt' });

    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.queryByText(RESYNC_STARTED)).toBeNull();
    expect(factOf('Resync')).toBe('running: 1,500 versions looked at, 12 sent');
  });

  it('explains a 409 of a resync without claiming a cause (defensive: the console reuses no key, so the API does not answer it this)', async () => {
    signedIn({
      ...replicated(),
      'POST /buckets/photos/replication/resync': () =>
        refusedWith(409, 'RequestInProgress', 'Request in progress', 'a request with this key is in progress'),
    });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('State', { selector: 'dt' });

    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The API answered that this request is still in progress. Refresh before asking again.');
    expect(alert.textContent).toContain('a request with this key is in progress');
    expect(screen.queryByText(RESYNC_STARTED)).toBeNull();
  });

  it('explains a resync whose target is no longer open to the bucket (400)', async () => {
    signedIn({
      ...replicated(),
      'POST /buckets/photos/replication/resync': () =>
        refusedWith(400, 'InvalidArgument', 'Invalid argument', 'target site-b is not open to bucket photos'),
    });
    render(<BucketReplicationSection bucket="photos" />);
    unfold();
    await screen.findByText('State', { selector: 'dt' });

    fireEvent.click(screen.getByRole('button', { name: 'Resync' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, resync' }));
    expect((await screen.findByRole('alert')).textContent).toContain(
      'The target of the configuration is no longer another deployment open to this bucket: nothing was started.',
    );
  });
});

describe('the replica mode of a bucket', () => {
  const unfold = () => {
    fireEvent.click(screen.getByRole('button', { name: 'Replica mode' }));
  };
  const typed = () => screen.getByLabelText('Type the name of the bucket, photos, to promote it') as HTMLInputElement;
  const promoteButton = () => screen.getByRole('button', { name: 'Promote bucket' }) as HTMLButtonElement;
  const REPLICA = () => json(200, { replica: true, source: SOURCE });

  it('is folded until asked for, then says that a bucket that is no replica takes writes', async () => {
    const api = signedIn({ 'GET /buckets/photos/replica': () => json(200, { replica: false }) });
    render(<BucketReplicaSection bucket="photos" />);
    expect(api.callsTo('GET', '/buckets/photos/replica')).toHaveLength(0);

    unfold();
    expect(await screen.findByText('This bucket is no replica: it takes writes as any bucket does.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Promote' })).toBeNull();
  });

  it('is hidden from a caller the API does not show it (403), in a line and without an alert', async () => {
    signedIn({ 'GET /buckets/photos/replica': DENIED });
    render(<BucketReplicaSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('You may not see whether this bucket is a replica.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Promote' })).toBeNull();
  });

  it('shows the source of a replica, and promotes it only once the name of the bucket is typed', async () => {
    const api = signedIn({
      'GET /buckets/photos/replica': REPLICA,
      'PUT /buckets/photos/replica': () => json(200, { replica: false }),
    });
    render(<BucketReplicaSection bucket="photos" />);
    unfold();

    expect(await screen.findByText(SOURCE, { selector: 'code' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    const dialog = screen.getByRole('dialog', { name: 'Promote bucket photos?' });
    expect(dialog.textContent).toContain('in effect this cannot be undone');
    expect(dialog.textContent).toContain('nothing is sent back');
    expect(document.activeElement).toBe(typed());
    expectEveryButtonHasATitle();

    // Anything but the name - another case, a part of it - promotes nothing.
    expect(promoteButton().disabled).toBe(true);
    for (const wrong of ['Photos', 'photo', 'photos ']) {
      fireEvent.change(typed(), { target: { value: wrong } });
      expect(promoteButton().disabled).toBe(true);
      fireEvent.submit(promoteButton().closest('form') as HTMLFormElement);
    }
    expect(api.callsTo('PUT', '/buckets/photos/replica')).toHaveLength(0);

    fireEvent.change(typed(), { target: { value: 'photos' } });
    expect(promoteButton().disabled).toBe(false);
    fireEvent.click(promoteButton());

    expect(
      await screen.findByText(
        `Promoted. Bucket photos takes writes again and nothing more from deployment ${SOURCE}; nothing was sent back.`,
      ),
    ).toBeDefined();
    const [put] = api.callsTo('PUT', '/buckets/photos/replica');
    expect(JSON.parse(put?.body ?? 'null')).toEqual({ source: '' });
    expectSentAsAChange(put);
    expect(screen.queryByRole('dialog')).toBeNull();
    // The dialog and its opener are gone: the keyboard is on the line that says what the bucket is now.
    const outcome = screen.getByText('This bucket is no replica: it takes writes as any bucket does.');
    expect(document.activeElement).toBe(outcome);
  });

  it('puts the question aside with Escape or Cancel, and forgets what was typed', async () => {
    const api = signedIn({ 'GET /buckets/photos/replica': REPLICA });
    render(<BucketReplicaSection bucket="photos" />);
    unfold();
    await screen.findByText(SOURCE, { selector: 'code' });

    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    fireEvent.change(typed(), { target: { value: 'photos' } });
    fireEvent.keyDown(typed(), { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Promote' }));

    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    expect(typed().value).toBe('');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(api.callsTo('PUT', '/buckets/photos/replica')).toHaveLength(0);
  });

  it('shows a promotion the API refuses (403) in its words, and keeps the bucket a replica', async () => {
    const api = signedIn({ 'GET /buckets/photos/replica': REPLICA, 'PUT /buckets/photos/replica': DENIED });
    render(<BucketReplicaSection bucket="photos" />);
    unfold();
    await screen.findByText(SOURCE, { selector: 'code' });

    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    fireEvent.change(typed(), { target: { value: 'photos' } });
    // As a click does, the keyboard goes to the button first.
    promoteButton().focus();
    fireEvent.click(promoteButton());

    const dialog = screen.getByRole('dialog', { name: 'Promote bucket photos?' });
    expect((await within(dialog).findByRole('alert')).textContent).toContain('Access denied');
    expect(api.callsTo('PUT', '/buckets/photos/replica')).toHaveLength(1);
    expect(factOf('Source')).toContain(SOURCE);
    expect(screen.queryByText(/^Promoted\./)).toBeNull();
    // The keyboard is back in the field, beside the API's words.
    await waitFor(() => {
      expect(document.activeElement).toBe(typed());
    });

    // Put aside and asked again, the question does not show the old refusal.
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('explains a 409 of a promotion without claiming a cause (defensive: promotion answers no conflict today)', async () => {
    signedIn({
      'GET /buckets/photos/replica': REPLICA,
      'PUT /buckets/photos/replica': () =>
        refusedWith(409, 'Conflict', 'State has changed', 'a detail of the API'),
    });
    render(<BucketReplicaSection bucket="photos" />);
    unfold();
    await screen.findByText(SOURCE, { selector: 'code' });

    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    fireEvent.change(typed(), { target: { value: 'photos' } });
    fireEvent.click(promoteButton());

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(
      'The API answered with a conflict: bucket photos was not promoted. Read its mode again before asking once more.',
    );
    expect(alert.textContent).toContain('a detail of the API');
    expect(screen.getByRole('dialog')).toBeDefined();
    expect(screen.queryByText(/^Promoted\./)).toBeNull();
  });

  it('explains a promotion while not every node knows replication (503)', async () => {
    signedIn({
      'GET /buckets/photos/replica': REPLICA,
      'PUT /buckets/photos/replica': () => refusedWith(503, 'ServiceUnavailable', 'Service unavailable', 'not yet'),
    });
    render(<BucketReplicaSection bucket="photos" />);
    unfold();
    await screen.findByText(SOURCE, { selector: 'code' });

    fireEvent.click(screen.getByRole('button', { name: 'Promote' }));
    fireEvent.change(typed(), { target: { value: 'photos' } });
    fireEvent.click(promoteButton());

    expect((await screen.findByRole('alert')).textContent).toContain(
      'Bucket photos was not promoted: not every node knows replication yet',
    );
  });
});

/** Shows the object browser as the console does: beneath its uploads and deletions. */
function renderObjects(screenOfABucket: ReactElement) {
  return render(
    <UploadsProvider>
      <DeletionsProvider>{screenOfABucket}</DeletionsProvider>
    </UploadsProvider>,
  );
}

describe('the replication status of a version in the object browser', () => {
  /** The answers behind the details of object a.txt of bucket photos, whose metadata says this much more. */
  function detailsOf(more: Record<string, unknown>) {
    return {
      'GET /buckets/photos/objects': () =>
        json(200, {
          objects: [{ key: 'a.txt', size: 10, etag: '"e"', last_modified: CREATED }],
          common_prefixes: [],
          truncated: false,
          access_included: false,
        }),
      'GET /buckets/photos/object/metadata': () =>
        json(200, {
          bucket: 'photos',
          key: 'a.txt',
          size: 10,
          etag: '"e"',
          last_modified: CREATED,
          content_type: 'text/plain',
          encryption: 'SSE-S3',
          ...more,
        }),
    };
  }
  const open = async () => {
    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));
    const region = screen.getByRole('region', { name: 'Details of a.txt' });
    await within(region).findByText('Encryption', { selector: 'dt' });
    return region;
  };

  it.each(['PENDING', 'COMPLETED', 'FAILED', 'REPLICA'] as const)('shows %s as the API says it, with what it means', async (status) => {
    fakeApi(detailsOf({ replication_status: status }));
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const region = await open();
    const value = within(region).getByText('Replication', { selector: 'dt' }).nextElementSibling;
    expect(value?.textContent).toBe(status);
    expect(within(value as HTMLElement).getByText(status).getAttribute('title')).toBe(REPLICATION_STATUS_HINTS[status]);
  });

  it('says nothing of replication for a version no rule is about', async () => {
    fakeApi(detailsOf({}));
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);

    const region = await open();
    expect(within(region).queryByText('Replication', { selector: 'dt' })).toBeNull();
  });
});
