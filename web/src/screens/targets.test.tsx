import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { CSRF_TOKEN, json, noContent, refusal, signedIn, type Call } from '../test/fakeApi';
import { BucketNotificationsSection, RULES_SAVED } from './BucketNotifications';
import { Screens } from './Screens';
import { TargetsScreen } from './TargetsScreen';

const CREATED = '2026-11-01T00:00:00Z';
const CHANGED = '2026-11-02T00:00:00Z';

/** What the API answers a caller it does not tell, or does not let change something. */
const DENIED = () => refusal(403, 'AccessDenied', 'Access denied');

/** What the API answers a path it does not know: a node of an older release, during a rolling upgrade. */
const NO_SUCH_ENDPOINT = () => refusal(404, 'NotFound', 'No such endpoint');

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

/** The body of a request, read as JSON. */
function bodyOf(call: Call | undefined): unknown {
  return JSON.parse(call?.body ?? 'null') as unknown;
}

/** That every button styled as one has a hint: on a phone its text folds away. */
function expectEveryButtonHasATitle() {
  for (const control of screen.getAllByRole('button')) {
    if (control.className.includes('btn')) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
  }
}

const HOOKS = {
  name: 'hooks',
  kind: 'webhook',
  endpoint: 'https://hooks.example.com:8443',
  endpoint_path_set: true,
  buckets: ['photos', 'logs-*'],
  audit: false,
  auth: { mode: 'bearer' },
  secret_set: true,
  signing_key_set: true,
  references: 2,
  created_at: CREATED,
  updated_at: CHANGED,
};

const STREAM = {
  name: 'stream',
  kind: 'nats',
  endpoint: 'tls://nats.example.com:4222',
  endpoint_path_set: false,
  subject: 'cactusfs.notify',
  buckets: [],
  audit: true,
  auth: { mode: 'token' },
  secret_set: true,
  signing_key_set: false,
  references: 0,
  created_at: CREATED,
  updated_at: CREATED,
};

const HOOKS_STATUS = {
  name: 'hooks',
  state: 'retrying',
  pending: 1204,
  oldest_pending_at: CREATED,
  last_error_class: 'connect',
  lost: 3,
  rejected: 0,
};

const STREAM_STATUS = { name: 'stream', state: 'ok', pending: 0, lost: 0, rejected: 0 };

/** The secret and the path typed in the tests: neither may ever be on screen after it is sent. */
const SECRET = 'bearer-secret-of-the-test';
const PATH = '/hooks/path-token-of-the-test';
const SIGNING_KEY = 'signing-key-shown-once-0123456789abcdef';

/** What the API answers a target that is not there. */
const NO_SUCH_TARGET = () => refusal(404, 'NoSuchEntity', 'No such entity');

/** The answers of the API for these targets: the list, each one, and how each stands; a target not among them is not there. */
function listed(...targets: { name: string }[]) {
  const one = (name: string) => () => {
    const target = targets.find((listedOne) => listedOne.name === name);
    return target === undefined ? NO_SUCH_TARGET() : json(200, target);
  };
  return {
    'GET /targets': () => json(200, targets),
    'GET /targets/hooks': one('hooks'),
    'GET /targets/stream': one('stream'),
    'GET /targets/nexus': one('nexus'),
    'GET /targets/hooks/status': () => json(200, HOOKS_STATUS),
    'GET /targets/stream/status': () => json(200, STREAM_STATUS),
  };
}

const rowOf = (name: string) =>
  screen.getByRole('button', { name: `Edit target ${name}` }).closest('tr') as HTMLTableRowElement;

const cellOf = (name: string, label: string) =>
  rowOf(name).querySelector(`td[data-label="${label}"]`) as HTMLTableCellElement;

const field = (label: string | RegExp) => screen.getByLabelText(label) as HTMLInputElement;

describe('the targets', () => {
  it('are a section of the navigation with the broadcast icon, at #/targets', async () => {
    signedIn(listed(HOOKS));
    window.location.hash = '#/targets';
    render(<Screens />);

    expect(await screen.findByRole('heading', { name: 'Targets', level: 1 })).toBeDefined();
    expect(await screen.findByText('hooks')).toBeDefined();
  });

  it('are listed with their kind, the origin of the endpoint, the state, what waits and the class of the last error', async () => {
    const api = signedIn(listed(HOOKS, STREAM));
    render(<TargetsScreen />);

    const table = await screen.findByRole('table');
    expect(table.className).toContain('grid');
    expect(within(rowOf('hooks')).getByText('Webhook (HTTPS)')).toBeDefined();
    expect(within(rowOf('stream')).getByText('NATS JetStream')).toBeDefined();
    expect(cellOf('hooks', 'Endpoint').textContent).toBe('https://hooks.example.com:8443 with a path, not shown');
    expect(cellOf('stream', 'Endpoint').textContent).toBe('tls://nats.example.com:4222 subject cactusfs.notify');
    expect(cellOf('hooks', 'Sent').textContent).toBe('buckets photos, logs-*');
    expect(cellOf('stream', 'Sent').textContent).toBe('the audit trail');

    await waitFor(() => {
      expect(cellOf('hooks', 'State').textContent).toBe('retrying');
    });
    expect(within(cellOf('hooks', 'State')).getByText('retrying').className).toContain('state-warning');
    expect(cellOf('hooks', 'Pending').textContent).toBe('1,204 · 3 lost');
    expect(cellOf('hooks', 'Last error').textContent).toBe('connect');
    expect(within(cellOf('hooks', 'Last error')).getByText('connect').getAttribute('title')).toContain(
      'could not be connected to',
    );
    await waitFor(() => {
      expect(cellOf('stream', 'State').textContent).toBe('ok');
    });
    expect(cellOf('stream', 'Pending').textContent).toBe('0');
    expect(cellOf('stream', 'Last error').textContent).toBe('none');
    for (const cell of within(rowOf('hooks')).getAllByRole('cell')) {
      expect(cell.getAttribute('data-label')).toBeTruthy();
    }
    expect(api.callsTo('GET', '/targets/hooks/status')).toHaveLength(1);
    expect(api.callsTo('GET', '/targets/stream/status')).toHaveLength(1);
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('ask again for every status when refreshed', async () => {
    const api = signedIn(listed(HOOKS));
    render(<TargetsScreen />);
    await waitFor(() => {
      expect(cellOf('hooks', 'State').textContent).toBe('retrying');
    });

    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(api.callsTo('GET', '/targets/hooks/status')).toHaveLength(2);
    });
    expect(api.callsTo('GET', '/targets')).toHaveLength(2);
  });

  it('are hidden from a caller the API does not show them, in a line and without an alert', async () => {
    signedIn({ 'GET /targets': DENIED });
    render(<TargetsScreen />);

    expect(await screen.findByText('You may not see the targets.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Define target' })).toBeNull();
    expect(screen.queryByRole('table')).toBeNull();
  });

  it('say of a status the API does not show the caller that it is not visible, and of one it could not tell that it is unknown', async () => {
    signedIn({
      'GET /targets': () => json(200, [HOOKS, STREAM]),
      'GET /targets/hooks/status': DENIED,
      'GET /targets/stream/status': () => refusal(503, 'ServiceUnavailable', 'No leader to ask'),
    });
    render(<TargetsScreen />);

    await waitFor(() => {
      expect(cellOf('hooks', 'State').textContent).toBe('not visible to you');
    });
    await waitFor(() => {
      expect(cellOf('stream', 'State').textContent).toBe('unknown');
    });
    expect(within(cellOf('stream', 'State')).getByText('unknown').getAttribute('title')).toBe('No leader to ask');
    expect(cellOf('hooks', 'Pending').textContent).toBe('-');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('words a 404 NotFound as an older node', async () => {
    signedIn({ 'GET /targets': NO_SUCH_ENDPOINT });
    render(<TargetsScreen />);

    expect((await screen.findByRole('alert')).textContent).toContain(
      'Targets are not available on the node that answered (an older release); try again.',
    );
  });

  it('defines a webhook with a PUT, shows its signing key once, and keeps neither the secret nor the path', async () => {
    let targets: unknown[] = [];
    const api = signedIn({
      ...listed(),
      'GET /targets': () => json(200, targets),
      'PUT /targets/hooks': () => {
        targets = [HOOKS];
        return json(200, { target: HOOKS, signing_key: SIGNING_KEY });
      },
    });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');

    fireEvent.change(field('Name'), { target: { value: 'hooks' } });
    fireEvent.change(field('Endpoint'), { target: { value: `https://hooks.example.com:8443${PATH}` } });
    fireEvent.change(field('Buckets that may use it'), { target: { value: 'photos, logs-*' } });
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: 'bearer' } });
    const secret = field('Secret');
    expect(secret.type).toBe('password');
    expect(secret.autocomplete).toBe('new-password');
    fireEvent.change(secret, { target: { value: SECRET } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    await screen.findByText('Defined target hooks.');
    const [put] = api.callsTo('PUT', '/targets/hooks');
    expect(bodyOf(put)).toEqual({
      kind: 'webhook',
      endpoint: `https://hooks.example.com:8443${PATH}`,
      buckets: ['photos', 'logs-*'],
      audit: false,
      auth: { mode: 'bearer', secret: SECRET },
      rotate_signing_key: false,
    });
    expectSentAsAChange(put);

    // The signing key, once, with how a receiver verifies with it.
    const notice = screen.getByText(/Copy the signing key of target hooks now/).closest('.alert') as HTMLElement;
    expect(within(notice).getByText(SIGNING_KEY)).toBeDefined();
    expect(notice.textContent).toContain('X-Cactusfs-Timestamp');
    expect(notice.textContent).toContain('X-Cactusfs-Signature');
    // A receiver that cannot verify answers 401, which holds the target: a 400 would pass the message.
    expect(notice.textContent).toContain('is refused with 401');
    // That a key was made is announced; the key itself is in no live region.
    expect(
      screen.getAllByRole('status').some((region) => region.textContent.includes('A signing key was made for target hooks')),
    ).toBe(true);
    for (const region of document.querySelectorAll('[role="status"], [aria-live], [role="alert"]')) {
      expect(region.textContent).not.toContain(SIGNING_KEY);
    }
    // Neither the secret nor the path is anywhere in the page.
    await screen.findByRole('button', { name: 'Edit target hooks' });
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(document.body.innerHTML).not.toContain(PATH);
    expect(field('Name').value).toBe('');

    fireEvent.click(within(notice).getByRole('button', { name: 'I have copied it' }));
    expect(document.body.innerHTML).not.toContain(SIGNING_KEY);
  });

  it('defines a NATS server with its subject and a token', async () => {
    const api = signedIn({
      ...listed(),
      'PUT /targets/stream': () => json(200, { target: STREAM }),
    });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');

    fireEvent.change(field('Name'), { target: { value: 'stream' } });
    fireEvent.change(screen.getByLabelText('Kind'), { target: { value: 'nats' } });
    fireEvent.change(field('Endpoint'), { target: { value: 'tls://nats.example.com:4222' } });
    fireEvent.change(field('Subject'), { target: { value: 'cactusfs.notify' } });
    fireEvent.click(field('Send it every record of the audit trail as well, from the first'));
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: 'token' } });
    fireEvent.change(field('Secret'), { target: { value: SECRET } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    await screen.findByText('Defined target stream.');
    expect(bodyOf(api.callsTo('PUT', '/targets/stream')[0])).toEqual({
      kind: 'nats',
      endpoint: 'tls://nats.example.com:4222',
      subject: 'cactusfs.notify',
      buckets: [],
      audit: true,
      auth: { mode: 'token', secret: SECRET },
      rotate_signing_key: false,
    });
    // A NATS server is given no signing key, and none is shown.
    expect(screen.queryByText(/Copy the signing key/)).toBeNull();
  });

  it('does not define a name that is listed already: that target is edited in the list', async () => {
    const api = signedIn(listed(HOOKS));
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.change(field('Name'), { target: { value: 'hooks' } });
    fireEvent.change(field('Endpoint'), { target: { value: 'https://elsewhere.example.com/' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    expect((await screen.findByRole('alert')).textContent).toContain(
      'Target hooks is defined already: edit it in the list.',
    );
    expect(api.callsTo('PUT', '/targets/hooks')).toHaveLength(0);
  });

  it('asks the API again before defining, and does not define a name another administrator took meanwhile', async () => {
    const api = signedIn({ ...listed(), 'GET /targets/hooks': () => json(200, HOOKS) });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');

    fireEvent.change(field('Name'), { target: { value: 'hooks' } });
    fireEvent.change(field('Endpoint'), { target: { value: 'https://elsewhere.example.com/' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    expect((await screen.findByRole('alert')).textContent).toContain(
      'Target hooks is defined already: edit it in the list.',
    );
    expect(api.callsTo('GET', '/targets/hooks')).toHaveLength(1);
    expect(api.callsTo('PUT', '/targets/hooks')).toHaveLength(0);
  });

  it('defines nothing when the API cannot say whether the name is taken', async () => {
    const api = signedIn({ ...listed(), 'GET /targets/hooks': NO_SUCH_ENDPOINT });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');

    fireEvent.change(field('Name'), { target: { value: 'hooks' } });
    fireEvent.change(field('Endpoint'), { target: { value: 'https://hooks.example.com/' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    expect((await screen.findByRole('alert')).textContent).toContain('not available on the node that answered');
    expect(api.callsTo('PUT', '/targets/hooks')).toHaveLength(0);
  });

  it('offers for each kind only its ways of proving, and sends no field of another kind', async () => {
    const api = signedIn({
      ...listed(),
      'PUT /targets/nexus': () => json(200, { target: { ...STREAM, name: 'nexus', kind: 's3' } }),
    });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');
    const modes = () => [...(screen.getByLabelText('Credential') as HTMLSelectElement).options].map((option) => option.value);

    expect(modes()).toEqual(['none', 'bearer']);
    fireEvent.change(field('Name'), { target: { value: 'nexus' } });
    fireEvent.change(screen.getByLabelText('Kind'), { target: { value: 'nats' } });
    expect(modes()).toEqual(['none', 'token', 'basic']);
    fireEvent.change(field('Subject'), { target: { value: 'cactusfs.notify' } });
    fireEvent.click(field('Send it every record of the audit trail as well, from the first'));
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: 'basic' } });
    fireEvent.change(field('User'), { target: { value: 'cactus' } });

    // Another deployment: one way of proving, no audit trail, and nothing of NATS sent.
    fireEvent.change(screen.getByLabelText('Kind'), { target: { value: 's3' } });
    expect(modes()).toEqual(['sigv4']);
    expect((screen.getByLabelText('Credential') as HTMLSelectElement).value).toBe('sigv4');
    expect(screen.queryByLabelText('Subject')).toBeNull();
    expect(screen.queryByLabelText('Send it every record of the audit trail as well, from the first')).toBeNull();
    fireEvent.change(field('Endpoint'), { target: { value: 'https://other.example.com' } });
    fireEvent.change(field('Region'), { target: { value: 'eu-1' } });
    fireEvent.change(field('Access key ID'), { target: { value: 'AKIAOTHERSITE0000001' } });
    fireEvent.change(field('Secret'), { target: { value: SECRET } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    await screen.findByText('Defined target nexus.');
    expect(bodyOf(api.callsTo('PUT', '/targets/nexus')[0])).toEqual({
      kind: 's3',
      endpoint: 'https://other.example.com',
      region: 'eu-1',
      buckets: [],
      audit: false,
      auth: { mode: 'sigv4', id: 'AKIAOTHERSITE0000001', secret: SECRET },
      rotate_signing_key: false,
    });
  });

  it('changes a NATS target from a user and password to a token without its user, and to none without a secret', async () => {
    const BASIC = { ...STREAM, auth: { mode: 'basic', id: 'cactus' } };
    const api = signedIn({ ...listed(BASIC), 'PUT /targets/stream': () => json(200, { target: STREAM }) });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(screen.getByRole('button', { name: 'Edit target stream' }));
    let panel = within(screen.getByRole('heading', { name: 'Target stream' }).closest('section') as HTMLElement);
    expect((panel.getByLabelText('User') as HTMLInputElement).value).toBe('cactus');
    fireEvent.change(panel.getByLabelText('Credential'), { target: { value: 'token' } });
    expect(panel.queryByLabelText('User')).toBeNull();
    fireEvent.change(panel.getByLabelText('Secret'), { target: { value: SECRET } });
    fireEvent.click(panel.getByRole('button', { name: 'Save target' }));

    await screen.findByText('Saved target stream.');
    expect(bodyOf(api.callsTo('PUT', '/targets/stream')[0])).toEqual({
      kind: 'nats',
      subject: 'cactusfs.notify',
      buckets: [],
      audit: true,
      auth: { mode: 'token', secret: SECRET },
      rotate_signing_key: false,
    });

    // A secret typed and the way of proving then put to none: no secret is sent.
    panel = within(screen.getByRole('heading', { name: 'Target stream' }).closest('section') as HTMLElement);
    fireEvent.change(panel.getByLabelText('Secret'), { target: { value: SECRET } });
    fireEvent.change(panel.getByLabelText('Credential'), { target: { value: 'none' } });
    fireEvent.click(panel.getByRole('button', { name: 'Save target' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/targets/stream')).toHaveLength(2);
    });
    expect(bodyOf(api.callsTo('PUT', '/targets/stream')[1])).toMatchObject({ auth: { mode: 'none' } });
    expect(api.callsTo('PUT', '/targets/stream')[1]?.body).not.toContain(SECRET);
  });

  it('sends a target changed in nothing as the API shows it: its subject, certificates and audit trail kept', async () => {
    const CA = '-----BEGIN CERTIFICATE-----\nMIIBtest\n-----END CERTIFICATE-----\n';
    const WITH_CA = { ...STREAM, ca_pem: CA };
    const api = signedIn({ ...listed(WITH_CA), 'PUT /targets/stream': () => json(200, { target: WITH_CA }) });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(screen.getByRole('button', { name: 'Edit target stream' }));
    const panel = within(screen.getByRole('heading', { name: 'Target stream' }).closest('section') as HTMLElement);
    fireEvent.click(panel.getByRole('button', { name: 'Save target' }));

    await screen.findByText('Saved target stream.');
    expect(bodyOf(api.callsTo('PUT', '/targets/stream')[0])).toEqual({
      kind: 'nats',
      subject: 'cactusfs.notify',
      ca_pem: CA,
      buckets: [],
      audit: true,
      auth: { mode: 'token' },
      rotate_signing_key: false,
    });
  });

  it('shows the API refusing a definition (403) in its words, and keeps what was typed', async () => {
    signedIn({ ...listed(), 'PUT /targets/hooks': DENIED });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');

    fireEvent.change(field('Name'), { target: { value: 'hooks' } });
    fireEvent.change(field('Endpoint'), { target: { value: 'https://hooks.example.com/' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(field('Name').value).toBe('hooks');
    expect(screen.queryByText(/Copy the signing key/)).toBeNull();
  });

  it('explains a definition the API refuses as invalid', async () => {
    signedIn({
      ...listed(),
      'PUT /targets/hooks': () =>
        refusedWith(400, 'InvalidArgument', 'Invalid argument', 'the endpoint of a webhook is https://'),
    });
    render(<TargetsScreen />);
    await screen.findByText('No target is defined.');

    fireEvent.change(field('Name'), { target: { value: 'hooks' } });
    fireEvent.change(field('Endpoint'), { target: { value: 'http://hooks.example.com/' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define target' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Target hooks was refused, and nothing was defined.');
    expect(alert.textContent).toContain('The server said: Invalid argument: the endpoint of a webhook is https://');
  });

  it('changes a target with its fields as shown, the secret and the endpoint kept unless typed', async () => {
    const api = signedIn({
      ...listed(HOOKS),
      'PUT /targets/hooks': () => json(200, { target: { ...HOOKS, buckets: ['photos'] } }),
    });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(screen.getByRole('button', { name: 'Edit target hooks' }));
    const heading = screen.getByRole('heading', { name: 'Target hooks' });
    expect(document.activeElement).toBe(heading);
    const panel = within(heading.closest('section') as HTMLElement);
    // Write-only fields are empty: an answer of the API never fills them.
    const secret = panel.getByLabelText('Secret') as HTMLInputElement;
    expect(secret.value).toBe('');
    expect(secret.placeholder).toBe('set - left empty, it is kept');
    const endpoint = panel.getByLabelText('New endpoint (optional)') as HTMLInputElement;
    expect(endpoint.value).toBe('');
    expect(endpoint.placeholder).toBe('kept: https://hooks.example.com:8443 with its path');
    expect((panel.getByLabelText('Buckets that may use it') as HTMLInputElement).value).toBe('photos, logs-*');
    expect(panel.queryByLabelText('Name')).toBeNull();

    fireEvent.change(panel.getByLabelText('Buckets that may use it'), { target: { value: 'photos' } });
    fireEvent.click(panel.getByRole('button', { name: 'Save target' }));

    await screen.findByText('Saved target hooks.');
    const [put] = api.callsTo('PUT', '/targets/hooks');
    expect(bodyOf(put)).toEqual({
      kind: 'webhook',
      buckets: ['photos'],
      audit: false,
      auth: { mode: 'bearer' },
      rotate_signing_key: false,
    });
    expectSentAsAChange(put);
  });

  it('asks for a new signing key and shows it once', async () => {
    const api = signedIn({
      ...listed(HOOKS),
      'PUT /targets/hooks': () => json(200, { target: HOOKS, signing_key: SIGNING_KEY }),
    });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(screen.getByRole('button', { name: 'Edit target hooks' }));
    const panel = within(screen.getByRole('heading', { name: 'Target hooks' }).closest('section') as HTMLElement);
    fireEvent.click(panel.getByLabelText(/Make a new signing key/));
    fireEvent.click(panel.getByRole('button', { name: 'Save target' }));

    expect(await screen.findByText(SIGNING_KEY)).toBeDefined();
    expect(bodyOf(api.callsTo('PUT', '/targets/hooks')[0])).toMatchObject({ rotate_signing_key: true });
  });

  it('shows a change the API refuses (403) in its words, the target as it was', async () => {
    signedIn({ ...listed(HOOKS), 'PUT /targets/hooks': DENIED });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(screen.getByRole('button', { name: 'Edit target hooks' }));
    const panel = within(screen.getByRole('heading', { name: 'Target hooks' }).closest('section') as HTMLElement);
    fireEvent.click(panel.getByLabelText('Send it every record of the audit trail as well, from the first'));
    fireEvent.click(panel.getByRole('button', { name: 'Save target' }));

    expect((await panel.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.queryByText('Saved target hooks.')).toBeNull();
  });

  it('deletes a target only after the question, and explains one a bucket names (409 EntityInUse)', async () => {
    let answer = () =>
      refusedWith(409, 'EntityInUse', 'Entity is in use', 'Something refers to it: what refers to it must be changed or removed first.');
    let targets: unknown[] = [HOOKS];
    const api = signedIn({
      ...listed(),
      'GET /targets': () => json(200, targets),
      'DELETE /targets/hooks': () => answer(),
    });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('DELETE', '/targets/hooks')).toHaveLength(0);

    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Yes, delete' }));
    expect((await screen.findByRole('alert')).textContent).toContain(
      'Target hooks cannot be deleted: the notification rules or the replication of a bucket name it.',
    );

    answer = () => {
      targets = [];
      return noContent();
    };
    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Yes, delete' }));
    expect(await screen.findByText('Deleted target hooks. What waited to be sent to it is not sent.')).toBeDefined();
    await screen.findByText('No target is defined.');
    expectSentAsAChange(api.callsTo('DELETE', '/targets/hooks')[1]);
  });

  it('shows a deletion the API refuses (403) in its words', async () => {
    signedIn({ ...listed(HOOKS), 'DELETE /targets/hooks': DENIED });
    render(<TargetsScreen />);
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('hooks')).getByRole('button', { name: 'Yes, delete' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.getByRole('button', { name: 'Edit target hooks' })).toBeDefined();
  });
});

const RULES = [
  { id: 'uploads', target: 'hooks', events: ['put', 'complete'], prefix: 'in/', as: 'queue' },
  { id: 'removals', target: 'stream', events: ['marker', 'delete'], suffix: '.jpg', as: 'topic' },
];

describe('the notification rules of a bucket', () => {
  const unfold = () => {
    fireEvent.click(screen.getByRole('button', { name: 'Notifications' }));
  };
  const ruleRow = (id: string) => screen.getByText(id, { selector: 'td' }).closest('tr') as HTMLTableRowElement;

  it('are folded until asked for, then listed in the order the API gives them', async () => {
    const api = signedIn({
      'GET /buckets/photos/notifications': () => json(200, { rules: RULES }),
      'GET /targets': () => json(200, [HOOKS, STREAM]),
    });
    render(<BucketNotificationsSection bucket="photos" />);
    expect(api.callsTo('GET', '/buckets/photos/notifications')).toHaveLength(0);

    unfold();
    await screen.findByRole('table');
    const rows = screen.getAllByRole('row').slice(1);
    expect(rows.map((row) => row.querySelector('td')?.textContent)).toEqual(['uploads', 'removals']);
    expect(ruleRow('uploads').querySelector('td[data-label="Changes"]')?.textContent).toBe(
      'Put or copied; Multipart upload completed',
    );
    expect(ruleRow('uploads').querySelector('td[data-label="Keys"]')?.textContent).toBe('beginning with in/');
    expect(ruleRow('removals').querySelector('td[data-label="Keys"]')?.textContent).toBe('ending with .jpg');
    expect(ruleRow('removals').querySelector('td[data-label="In S3"]')?.textContent).toBe('Topic');
    // The names of the targets the caller may list are offered.
    await waitFor(() => {
      expect(field('Target').getAttribute('list')).toBeTruthy();
    });
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('say that a bucket has none', async () => {
    signedIn({ 'GET /buckets/photos/notifications': () => json(200, { rules: [] }) });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();

    expect(
      await screen.findByText('This bucket has no notification rules: no change of it is sent anywhere.'),
    ).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Remove every rule' })).toBeNull();
  });

  it('are hidden from a caller the API does not show them, in a line and without an alert', async () => {
    signedIn({ 'GET /buckets/photos/notifications': DENIED });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('You may not see the notification rules of this bucket.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Add rule' })).toBeNull();
  });

  it('take a rule added after the ones there are, sent whole with a PUT', async () => {
    const added = { id: 'thumbs', target: 'hooks', events: ['put'], suffix: '.png', as: 'queue' };
    const api = signedIn({
      'GET /buckets/photos/notifications': () => json(200, { rules: RULES }),
      'PUT /buckets/photos/notifications': () => json(200, { rules: [...RULES, added] }),
    });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();
    await screen.findByRole('table');

    fireEvent.change(field('Rule name'), { target: { value: 'thumbs' } });
    fireEvent.change(field('Target'), { target: { value: 'hooks' } });
    fireEvent.click(field(/^Multipart upload completed/));
    fireEvent.change(field('Keys ending with (optional)'), { target: { value: '.png' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add rule' }));

    expect(await screen.findByText(RULES_SAVED)).toBeDefined();
    const [put] = api.callsTo('PUT', '/buckets/photos/notifications');
    expect(bodyOf(put)).toEqual({
      rules: [
        { id: 'uploads', target: 'hooks', events: ['put', 'complete'], prefix: 'in/', as: 'queue' },
        { id: 'removals', target: 'stream', events: ['marker', 'delete'], suffix: '.jpg', as: 'topic' },
        { id: 'thumbs', target: 'hooks', events: ['put'], suffix: '.png', as: 'queue' },
      ],
    });
    expectSentAsAChange(put);
    expect(ruleRow('thumbs')).toBeDefined();
    expect(field('Rule name').value).toBe('');
  });

  it('change the rules as the API has them just before, not as they were read', async () => {
    const LATER = { id: 'later', target: 'stream', events: ['put'], as: 'queue' };
    let rules: unknown[] = RULES;
    const api = signedIn({
      'GET /buckets/photos/notifications': () => json(200, { rules }),
      'PUT /buckets/photos/notifications': (call) => json(200, JSON.parse(call.body) as unknown),
    });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();
    await screen.findByRole('table');

    // Another administrator adds a rule after the list was read.
    rules = [...RULES, LATER];
    fireEvent.click(within(ruleRow('uploads')).getByRole('button', { name: 'Remove' }));
    fireEvent.click(within(ruleRow('uploads')).getByRole('button', { name: 'Yes, remove' }));
    await screen.findByText(/^Removed rule uploads\./);
    expect(bodyOf(api.callsTo('PUT', '/buckets/photos/notifications')[0])).toEqual({
      rules: [RULES[1], LATER],
    });

    // And removes one before a rule is added here: it is not put back.
    rules = [LATER];
    fireEvent.change(field('Rule name'), { target: { value: 'thumbs' } });
    fireEvent.change(field('Target'), { target: { value: 'hooks' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add rule' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/notifications')).toHaveLength(2);
    });
    expect(bodyOf(api.callsTo('PUT', '/buckets/photos/notifications')[1])).toEqual({
      rules: [LATER, { id: 'thumbs', target: 'hooks', events: ['put', 'complete'], as: 'queue' }],
    });
    expect(api.callsTo('GET', '/buckets/photos/notifications')).toHaveLength(3);
  });

  it('explain a rule the API refuses, and keep the rules there were and what was typed', async () => {
    signedIn({
      'GET /buckets/photos/notifications': () => json(200, { rules: RULES }),
      'PUT /buckets/photos/notifications': () =>
        refusedWith(
          400,
          'InvalidArgument',
          'Invalid argument',
          'a rule names a target that is not there, is not a webhook or NATS server, or does not admit this bucket',
        ),
    });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();
    await screen.findByRole('table');

    fireEvent.change(field('Rule name'), { target: { value: 'elsewhere' } });
    fireEvent.change(field('Target'), { target: { value: 'closed' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add rule' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The rules were refused: bucket photos keeps the rules it had.');
    expect(alert.textContent).toContain('does not admit this bucket');
    expect(field('Target').value).toBe('closed');
    expect(screen.getAllByRole('row')).toHaveLength(3);
  });

  it('shows a change the API refuses (403) in its words', async () => {
    signedIn({
      'GET /buckets/photos/notifications': () => json(200, { rules: RULES }),
      'PUT /buckets/photos/notifications': DENIED,
    });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();
    await screen.findByRole('table');

    fireEvent.click(within(ruleRow('uploads')).getByRole('button', { name: 'Remove' }));
    fireEvent.click(within(ruleRow('uploads')).getByRole('button', { name: 'Yes, remove' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(ruleRow('uploads')).toBeDefined();
  });

  it('remove one rule with a PUT of the others, and every rule with a DELETE, each after the question', async () => {
    const api = signedIn({
      'GET /buckets/photos/notifications': () => json(200, { rules: RULES }),
      'PUT /buckets/photos/notifications': () => json(200, { rules: [RULES[1]] }),
      'DELETE /buckets/photos/notifications': () => noContent(),
    });
    render(<BucketNotificationsSection bucket="photos" />);
    unfold();
    await screen.findByRole('table');

    fireEvent.click(within(ruleRow('uploads')).getByRole('button', { name: 'Remove' }));
    fireEvent.click(within(ruleRow('uploads')).getByRole('button', { name: 'Yes, remove' }));
    expect(await screen.findByText(/^Removed rule uploads\./)).toBeDefined();
    expect(bodyOf(api.callsTo('PUT', '/buckets/photos/notifications')[0])).toEqual({
      rules: [{ id: 'removals', target: 'stream', events: ['marker', 'delete'], suffix: '.jpg', as: 'topic' }],
    });
    expect(screen.queryByText('uploads', { selector: 'td' })).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Remove every rule' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, remove every rule' }));
    expect(await screen.findByText('Removed every notification rule of bucket photos.')).toBeDefined();
    expectSentAsAChange(api.callsTo('DELETE', '/buckets/photos/notifications')[0]);
    expect(
      screen.getByText('This bucket has no notification rules: no change of it is sent anywhere.'),
    ).toBeDefined();
  });
});
