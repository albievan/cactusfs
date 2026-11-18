import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { PolicyCatalogue, PolicyFault, PolicyValidation, StatementAnalysis } from '../api/client';
import {
  CSRF_TOKEN,
  fakeApi,
  holdRequests,
  json,
  refusal,
  signedIn,
  type Call,
  type FakeApi,
  type Handler,
} from '../test/fakeApi';
import {
  CATALOGUE_FAILED_NOTICE,
  CATALOGUE_REFUSED_NOTICE,
  NO_VALIDATION_NOTICE,
  NOT_SAVED_NOTICE,
  OLDER_NODE_NOTICE,
  VALIDATION_DELAY_MS,
} from '../ui/policyHooks';
import { ApiKeys } from './ApiKeys';
import { PoliciesScreen } from './PoliciesScreen';
import { RoleScreen } from './RoleScreen';

// The policy builder (PLAN 9.8, ADR-0032): every criterion of the row, on
// the two screens that use it - the Policies screen and the inline policy of
// a new API key.

const CREATED = '2026-10-01T00:00:00Z';

/** The console's stylesheet as it is shipped (a CSS import is left out of the tests). */
const stylesheet = readFileSync(resolve(import.meta.dirname, '../styles/console.css'), 'utf8');

/** A catalogue as the node answers it, cut down to what the tests choose from. */
const CATALOGUE: PolicyCatalogue = {
  version: '2012-10-17',
  actions: [
    action('s3:GetObject', 'Objects', 'Read an object and what is known of it.', ['arn:aws:s3:::{bucket}/{key}']),
    action('s3:PutObject', 'Objects', 'Store an object.', ['arn:aws:s3:::{bucket}/{key}']),
    action('s3:ListBucket', 'Buckets', 'List the objects of a bucket.', ['arn:aws:s3:::{bucket}']),
    action('admin:GetUser', 'Users', 'Read a user.', ['arn:cactusfs:admin:::user/{name}']),
  ],
  condition_keys: [
    {
      key: 'aws:SourceIp',
      operators: ['IpAddress', 'NotIpAddress'],
      value_kind: 'ip_prefix',
      when_unknown: 'An unknown source address is decided against the request.',
    },
    { key: 'aws:SecureTransport', operators: ['Bool'], value_kind: 'bool', when_unknown: 'Never unknown.' },
  ],
  resources: [
    { kind: 'bucket', template: 'arn:aws:s3:::{bucket}', description: 'a bucket, by its name' },
    { kind: 'object', template: 'arn:aws:s3:::{bucket}/{key}', description: 'an object of a bucket, by its key' },
    { kind: 'users', template: 'arn:cactusfs:admin:::user', description: 'the users as a whole' },
    { kind: 'user', template: 'arn:cactusfs:admin:::user/{name}', description: 'a user, by name, with its API keys' },
  ],
  limits: { max_bytes: 20480, max_statements: 64, max_values: 128, max_depth: 8 },
  bucket_policy_principals: { anyone: ['"*"', '{"AWS":"*"}', '{"AWS":["*"]}'], user_arn_prefix: 'arn:aws:iam:::user/' },
};

function action(name: string, group: string, description: string, resources: string[]) {
  return {
    name,
    group,
    description,
    resources,
    read: name.includes('Get') || name.includes('List'),
    by_name_only: false,
    in_bucket_policy: name.startsWith('s3:'),
    anonymous: false,
    in_sub_bucket_policy: false,
  };
}

/** A validation answer: valid exactly when there is no fault. */
function verdict(faults: PolicyFault[] = [], statements: StatementAnalysis[] = []): PolicyValidation {
  return { valid: faults.length === 0, faults, truncated: false, statements };
}

/** The analysis of one statement, as the node gives it. */
function analysis(index: number, actions: string[], unmatched: string[] = []): StatementAnalysis {
  return { index, effect: 'Allow', actions, anonymous_actions: [], never_anonymous: [], unmatched_patterns: unmatched };
}

/** The answers of a node with the builder: the catalogue, two buckets, and validation by `validate`. */
function builderNode(validate: Handler = () => json(200, verdict()), more: Record<string, Handler> = {}): FakeApi {
  return signedIn({
    'GET /policy-catalogue': () => json(200, CATALOGUE),
    'GET /buckets': () => json(200, [{ name: 'photos', created_at: CREATED }, { name: 'reports', created_at: CREATED }]),
    'POST /policy-validation': validate,
    'GET /policies': () => json(200, []),
    'GET /users/bob/keys': () => json(200, []),
    ...more,
  });
}

/** The document a validation request carried, as sent. */
function sentDocument(call: Call | undefined): unknown {
  return (JSON.parse(call?.body ?? 'null') as { document?: unknown } | null)?.document;
}

const JSON_LABEL = 'Policy document as JSON';

function jsonBox(label = JSON_LABEL): HTMLTextAreaElement {
  return screen.getByLabelText(label) as HTMLTextAreaElement;
}

/** The JSON of the editor, read. */
function document(label = JSON_LABEL): unknown {
  return JSON.parse(jsonBox(label).value);
}

/**
 * Asks for the builder as a user does, with "Open the policy builder" (of an
 * editor in `within` if it has one, else of the screen, whose editors share
 * one catalogue): the catalogue is asked for then, never as a screen is
 * shown or a policy focused.
 */
async function touch(within_: HTMLElement = window.document.body): Promise<void> {
  // The JSON of an editor is in the page once the screen has read what it shows.
  await waitFor(() => {
    expect(within_.querySelector('.policy-editor textarea')).not.toBeNull();
  });
  const open =
    within(within_).queryAllByRole('button', { name: 'Open the policy builder' })[0] ??
    screen.queryAllByRole('button', { name: 'Open the policy builder' })[0];
  if (open !== undefined) {
    fireEvent.click(open);
  }
}

/** The builder (of the editor in `within`, if given), once a policy was touched and the catalogue is there. */
async function builder(within_: HTMLElement = window.document.body): Promise<HTMLElement> {
  await touch(within_);
  return within(within_).findByRole('group', { name: 'Policy builder' });
}

/** The panel of a policy opened from the list. */
function panelOf(name: string): HTMLElement {
  return screen.getByRole('heading', { name: new RegExp(`^Policy ${name}`) }).closest('section') as HTMLElement;
}

function statement(n: number): HTMLElement {
  return screen.getByRole('group', { name: new RegExp(`^Statement ${String(n)}`) });
}

function typeInto(element: HTMLElement, value: string) {
  fireEvent.change(element, { target: { value } });
}

/** Opens the Policies screen of a node with the builder and adds one statement. */
async function withOneStatement(api: FakeApi = builderNode()): Promise<FakeApi> {
  render(<PoliciesScreen />);
  fireEvent.click(within(await builder()).getByRole('button', { name: 'Add a statement' }));
  return api;
}

const ONE = { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: [], Resource: [] }] };

describe('the catalogue', () => {
  it.each([
    ['a node of an older release', () => refusal(404, 'NotFound', 'Not found'), OLDER_NODE_NOTICE],
    ['a caller refused the catalogue', () => refusal(403, 'AccessDenied', 'Access denied'), CATALOGUE_REFUSED_NOTICE],
    ['a catalogue that cannot be read', () => refusal(500, 'InternalError', 'Internal error'), CATALOGUE_FAILED_NOTICE],
  ])('leaves the JSON editor alone, as before, for %s, and says why', async (_, catalogue, notice) => {
    const typed = '{"Version":"2012-10-17","Statement":[]}';
    const api = fakeApi({
      'GET /policy-catalogue': catalogue,
      'GET /policies': () => json(200, []),
      'POST /policies': () => json(201, { name: 'p', document: {}, built_in: false, created_at: CREATED }),
    });
    render(<PoliciesScreen />);

    // Shown, the screen asks nothing of the catalogue; touched, it does.
    await screen.findByLabelText(JSON_LABEL);
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    await touch();
    expect(await screen.findByText(notice)).toBeDefined();
    expect(screen.queryByRole('group', { name: 'Policy builder' })).toBeNull();
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    typeInto(jsonBox(), typed);
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));

    await waitFor(() => {
      expect(api.callsTo('POST', '/policies')).toHaveLength(1);
    });
    expect(api.callsTo('POST', '/policies')[0]?.body).toBe(`{"name":"p","document":${typed}}`);
    // Nothing is asked of a node that has no builder for this caller, not even validation.
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);
    expect(api.callsTo('GET', '/buckets')).toHaveLength(0);
  });

  it('leaves the inline policy of a new key to the JSON editor on a node of an older release', async () => {
    const typed = '{"Version":"2012-10-17","Statement":[]}';
    const api = fakeApi({
      'GET /policy-catalogue': () => refusal(404, 'NotFound', 'Not found'),
      'GET /users/bob/keys': () => json(200, []),
      'POST /users/bob/keys': () => json(201, { key: { access_key_id: 'AK', owner: 'bob', created_at: CREATED }, secret_key: 's' }),
    });
    render(<ApiKeys user="bob" />);

    await touch();
    expect(await screen.findByText(OLDER_NODE_NOTICE)).toBeDefined();
    typeInto(jsonBox('Inline policy as JSON (optional; narrows what the key may do)'), typed);
    fireEvent.click(screen.getByRole('button', { name: 'Create API key' }));

    await waitFor(() => {
      expect(api.callsTo('POST', '/users/bob/keys')[0]?.body).toBe(`{"inline_policy":${typed}}`);
    });
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);
  });

  it('offers the actions of the catalogue by group, and finds them by search', async () => {
    await withOneStatement();
    const actions = within(statement(1)).getByRole('group', { name: 'Actions of statement 1' });

    // The groups, in the catalogue's order, each saying how many of it are chosen.
    const groups = Array.from(actions.querySelectorAll('summary')).map((summary) => summary.textContent);
    expect(groups).toEqual(['Objects (0 chosen)', 'Buckets (0 chosen)', 'Users (0 chosen)']);
    const read = within(actions).getByRole('checkbox', { name: 's3:GetObject' });
    expect(read.getAttribute('aria-describedby')).not.toBeNull();
    expect(window.document.getElementById(read.getAttribute('aria-describedby') ?? '')?.textContent).toBe(
      'Read an object and what is known of it.',
    );
    fireEvent.click(read);
    expect(document()).toEqual({ ...ONE, Statement: [{ ...ONE.Statement[0], Action: ['s3:GetObject'] }] });
    expect(actions.querySelector('summary')?.textContent).toBe('Objects (1 chosen)');

    // Search: by name, description or group; only what matches is offered.
    typeInto(within(actions).getByRole('searchbox', { name: 'Search actions of statement 1' }), 'list');
    expect(within(actions).getAllByRole('checkbox').map((box) => box.id.split('-').pop())).toEqual(['s3:ListBucket']);
    fireEvent.click(within(actions).getByRole('checkbox', { name: 's3:ListBucket' }));
    typeInto(within(actions).getByRole('searchbox', { name: 'Search actions of statement 1' }), 'nothing like it');
    expect(within(actions).getByText('No action of the catalogue matches “nothing like it”.')).toBeDefined();
    expect(document()).toEqual({ ...ONE, Statement: [{ ...ONE.Statement[0], Action: ['s3:GetObject', 's3:ListBucket'] }] });

    // Unticked, an action goes - however its case was written.
    typeInto(jsonBox(), JSON.stringify({ ...ONE, Statement: [{ ...ONE.Statement[0], Action: ['S3:GETOBJECT'] }] }, null, 2));
    typeInto(within(actions).getByRole('searchbox', { name: 'Search actions of statement 1' }), '');
    const ticked = within(statement(1)).getByRole('checkbox', { name: 's3:GetObject' }) as HTMLInputElement;
    expect(ticked.checked).toBe(true);
    fireEvent.click(ticked);
    expect(document()).toEqual(ONE);
  });

  it('takes a typed pattern, and shows what it covers as the validation answers', async () => {
    const api = builderNode((call) => {
      const sent = sentDocument(call) as { Statement: { Action?: string[] }[] };
      const patterns = sent.Statement[0]?.Action ?? [];
      return json(
        200,
        verdict([], [analysis(0, patterns.includes('s3:Get*') ? ['s3:GetObject'] : [], patterns.filter((p) => p === 's3:Gte*'))]),
      );
    });
    await withOneStatement(api);
    const actions = within(statement(1)).getByRole('group', { name: 'Actions of statement 1' });
    const pattern = within(actions).getByRole('textbox', { name: 'Action pattern for statement 1' });

    typeInto(pattern, 's3:Get*');
    fireEvent.click(within(actions).getByRole('button', { name: 'Add pattern to statement 1' }));
    typeInto(pattern, 's3:Gte*');
    fireEvent.keyDown(pattern, { key: 'Enter' });

    expect(document()).toEqual({ ...ONE, Statement: [{ ...ONE.Statement[0], Action: ['s3:Get*', 's3:Gte*'] }] });
    expect((pattern as HTMLInputElement).value).toBe('');
    // What the patterns cover is the node's answer, not the console's reckoning.
    expect(await within(actions).findByText('Covers 1 action, as the node reads them')).toBeDefined();
    expect(within(actions).getByText('s3:GetObject', { selector: '.policy-covers-list code' })).toBeDefined();
    const misspelt = within(actions).getByText('s3:Gte*').closest('li') as HTMLElement;
    expect(misspelt.textContent).toContain('covers no action this node knows');
    const right = within(actions).getByText('s3:Get*').closest('li') as HTMLElement;
    expect(right.textContent).not.toContain('covers no action');

    fireEvent.click(within(actions).getByRole('button', { name: 'Remove action s3:Gte* from statement 1' }));
    expect(document()).toEqual({ ...ONE, Statement: [{ ...ONE.Statement[0], Action: ['s3:Get*'] }] });
  });

  it('chooses resources by bucket and prefix, by admin kind and name, or as a typed ARN', async () => {
    const api = await withOneStatement();
    const resources = within(statement(1)).getByRole('group', { name: 'Resources of statement 1' });
    const bucket = await within(resources).findByRole('combobox', { name: 'Bucket for statement 1' });

    // The buckets the caller may list are offered.
    await waitFor(() => {
      expect(Array.from(resources.querySelectorAll('datalist option')).map((o) => (o as HTMLOptionElement).value)).toEqual([
        'photos',
        'reports',
      ]);
    });
    expect(api.callsTo('GET', '/buckets')).toHaveLength(1);
    typeInto(bucket, 'photos');
    fireEvent.click(within(resources).getByRole('button', { name: 'Add bucket to statement 1' }));
    typeInto(within(resources).getByRole('textbox', { name: 'Prefix (optional) for statement 1' }), 'reports/2026/');
    fireEvent.click(within(resources).getByRole('button', { name: 'Add objects to statement 1' }));

    const kind = within(resources).getByRole('combobox', { name: 'Admin resource for statement 1' });
    const name = within(resources).getByRole('textbox', { name: 'Name for statement 1' }) as HTMLInputElement;
    expect(Array.from((kind as HTMLSelectElement).options).map((o) => o.value)).toEqual(['users', 'user']);
    // A kind without a name has none to give.
    expect(name.disabled).toBe(true);
    fireEvent.change(kind, { target: { value: 'user' } });
    expect(name.disabled).toBe(false);
    typeInto(name, 'bob');
    fireEvent.click(within(resources).getByRole('button', { name: 'Add admin resource to statement 1' }));

    const arn = within(resources).getByRole('textbox', { name: 'ARN for statement 1' });
    typeInto(arn, 'arn:aws:s3:::reports/*');
    fireEvent.keyDown(arn, { key: 'Enter' });

    expect(document()).toEqual({
      ...ONE,
      Statement: [
        {
          ...ONE.Statement[0],
          Resource: ['arn:aws:s3:::photos', 'arn:aws:s3:::photos/reports/2026/*', 'arn:cactusfs:admin:::user/bob', 'arn:aws:s3:::reports/*'],
        },
      ],
    });
    fireEvent.click(within(resources).getByRole('button', { name: 'Remove resource arn:aws:s3:::photos from statement 1' }));
    expect((document() as typeof ONE).Statement[0]?.Resource).toHaveLength(3);
  });

  it('chooses conditions by key, operator and value from the catalogue', async () => {
    await withOneStatement();
    const conditions = within(statement(1)).getByRole('group', { name: 'Conditions of statement 1' });
    const key = within(conditions).getByRole('combobox', { name: 'Condition key for statement 1' });
    const operator = within(conditions).getByRole('combobox', { name: 'Operator for statement 1' }) as HTMLSelectElement;

    expect(Array.from(operator.options).map((o) => o.value)).toEqual(['IpAddress', 'NotIpAddress']);
    expect(within(conditions).getByText('An unknown source address is decided against the request.')).toBeDefined();
    fireEvent.change(operator, { target: { value: 'NotIpAddress' } });
    const value = within(conditions).getByRole('textbox', { name: 'Value for statement 1' });
    expect(value.getAttribute('placeholder')).toBe('192.0.2.0/24');
    typeInto(value, '192.0.2.0/24');
    fireEvent.keyDown(value, { key: 'Enter' });

    // Another key offers its own operators, and a value of its kind.
    fireEvent.change(key, { target: { value: 'aws:SecureTransport' } });
    expect(Array.from(operator.options).map((o) => o.value)).toEqual(['Bool']);
    const bool = within(conditions).getByRole('combobox', { name: 'Value for statement 1' }) as HTMLSelectElement;
    expect(Array.from(bool.options).map((o) => o.value)).toEqual(['true', 'false']);
    fireEvent.change(bool, { target: { value: 'false' } });
    fireEvent.click(within(conditions).getByRole('button', { name: 'Add condition to statement 1' }));

    expect(document()).toEqual({
      ...ONE,
      Statement: [
        {
          ...ONE.Statement[0],
          Condition: { NotIpAddress: { 'aws:SourceIp': '192.0.2.0/24' }, Bool: { 'aws:SecureTransport': 'false' } },
        },
      ],
    });
    fireEvent.click(
      within(conditions).getByRole('button', { name: 'Remove condition NotIpAddress aws:SourceIp from statement 1' }),
    );
    fireEvent.click(within(conditions).getByRole('button', { name: 'Remove condition Bool aws:SecureTransport from statement 1' }));
    expect(document()).toEqual(ONE);
  });
});

describe('the JSON beside the builder', () => {
  it('follows every change of the builder, written as the console writes a policy', async () => {
    await withOneStatement();
    expect(jsonBox().value).toBe(JSON.stringify(ONE, null, 2));

    fireEvent.change(within(statement(1)).getByRole('combobox', { name: 'Effect of statement 1' }), { target: { value: 'Deny' } });
    typeInto(within(statement(1)).getByRole('textbox', { name: 'Sid (optional) of statement 1' }), 'no-writes');
    expect(jsonBox().value).toBe(
      JSON.stringify({ ...ONE, Statement: [{ Sid: 'no-writes', Effect: 'Deny', Action: [], Resource: [] }] }, null, 2),
    );
    expect(statement(1).querySelector('legend')?.textContent).toBe('Statement 1: no-writes');

    fireEvent.click(screen.getByRole('button', { name: 'Add a statement' }));
    expect((document() as typeof ONE).Statement).toHaveLength(2);
    fireEvent.click(screen.getByRole('button', { name: 'Remove statement 1' }));
    expect(document()).toEqual(ONE);
  });

  it('is read back into the builder when it is changed, keeping the form of every field', async () => {
    render(<PoliciesScreen />);
    builderNode();
    await builder();
    const typed = '{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:PutObject","Resource":"*"}]}';
    typeInto(jsonBox(), typed);

    const first = statement(1);
    expect((within(first).getByRole('combobox', { name: 'Effect of statement 1' }) as HTMLSelectElement).value).toBe('Deny');
    expect(within(first).getByRole('button', { name: 'Remove action s3:PutObject from statement 1' })).toBeDefined();
    expect(within(first).getByRole('button', { name: 'Remove resource * from statement 1' })).toBeDefined();
    // Text the builder has not touched is left as typed.
    expect(jsonBox().value).toBe(typed);

    // A change in the builder writes the document anew, every field in the form it was written.
    fireEvent.change(within(first).getByRole('combobox', { name: 'Effect of statement 1' }), { target: { value: 'Allow' } });
    expect(jsonBox().value).toBe(
      JSON.stringify({ Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:PutObject', Resource: '*' }] }, null, 2),
    );
  });

  it.each([
    ['a field given twice', '{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Effect":"Allow"}]}', 'the field "Effect" is given twice (at /Statement/0/Effect)'],
    ['a text that is not JSON', '{"Version":', 'not JSON: the text ends too soon'],
    ['a field the language does not have', '{"Version":"2012-10-17","Id":"x"}', 'unknown field "Id" (at /Id)'],
  ])('turns the builder off with the reason for %s, and keeps the text as it is', async (_, typed, reason) => {
    builderNode();
    render(<PoliciesScreen />);
    await builder();
    typeInto(jsonBox(), typed);

    const pane = screen.getByRole('group', { name: 'Policy builder' }) as HTMLFieldSetElement;
    expect(pane.disabled).toBe(true);
    expect(within(pane).getByRole('status').textContent).toContain(reason);
    expect(screen.queryByRole('button', { name: 'Add a statement' })).toBeNull();
    expect(jsonBox().value).toBe(typed);

    // Put right, the builder is back.
    typeInto(jsonBox(), JSON.stringify(ONE));
    expect((screen.getByRole('group', { name: 'Policy builder' }) as HTMLFieldSetElement).disabled).toBe(false);
    expect(statement(1)).toBeDefined();
  });

  it('marks what the catalogue does not know, and keeps it', async () => {
    builderNode();
    render(<PoliciesScreen />);
    await builder();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:Teleport","Resource":"*"}]}');

    const actions = within(statement(1)).getByRole('group', { name: 'Actions of statement 1' });
    expect(within(actions).getByText(/Not in this node’s catalogue: the action "s3:Teleport" is not in the catalogue/)).toBeDefined();
    expect(within(actions).getByText('s3:Teleport')).toBeDefined();
  });
});

describe('validation as the policy is typed', () => {
  it('is debounced: a burst of typing is checked once, as typed, with the name it would be stored as', async () => {
    const api = builderNode();
    render(<PoliciesScreen />);
    await builder();
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'readers' } });

    // Each key comes before the pause is over: nothing is asked until typing stops.
    const pause = () => new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS / 2));
    typeInto(jsonBox(), '{"Version":"2012-10-17"}');
    await pause();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":{}}');
    await pause();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[]}');
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);
    expect(screen.getByText('Checking the policy…')).toBeDefined();

    await waitFor(() => {
      expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
    });
    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
    const calls = api.callsTo('POST', '/policy-validation');
    expect(calls).toHaveLength(1);
    expect(calls[0]?.body).toBe('{"name":"readers","document":{"Version":"2012-10-17","Statement":[]}}');
    // A POST of a session: it carries the CSRF token, as every POST does.
    expect(calls[0]?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    expect(await screen.findByText('The node finds no fault in the policy.')).toBeDefined();
  });

  it('gives up a check that is no longer wanted', async () => {
    const api = builderNode();
    render(<PoliciesScreen />);
    await builder();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[]}');
    await waitFor(() => {
      expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
    });
    const first = api.callsTo('POST', '/policy-validation')[0];
    typeInto(jsonBox(), '{"Version":"2012-10-17"}');
    await waitFor(() => {
      expect(api.callsTo('POST', '/policy-validation')).toHaveLength(2);
    });
    // The request of the text before is given up once the text changes, answered or not.
    expect(first?.signal.aborted).toBe(true);
    expect(api.callsTo('POST', '/policy-validation')[1]?.signal.aborted).toBe(false);
  });

  it('checks the inline policy of a new key as a key of its owner', async () => {
    const api = builderNode();
    render(<ApiKeys user="bob" />);
    await builder();
    typeInto(jsonBox('Inline policy as JSON (optional; narrows what the key may do)'), '{"Version":"2012-10-17","Statement":[]}');

    await waitFor(() => {
      expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
    });
    expect(api.callsTo('POST', '/policy-validation')[0]?.body).toBe(
      '{"key_owner":"bob","document":{"Version":"2012-10-17","Statement":[]}}',
    );
  });

  it('shows each fault at its field and in the JSON, and announces them', async () => {
    const faults: PolicyFault[] = [
      { pointer: '/Statement/0/Action/0', statement: 0, reason: 'statement 1: an action must be "s3:..." or "admin:..."' },
      { pointer: '/Statement/0/Resource', statement: 0, reason: 'statement 1: no resource' },
      { pointer: '/Version', reason: 'Version must be "2012-10-17"' },
    ];
    builderNode(() => json(200, verdict(faults)));
    render(<PoliciesScreen />);
    await builder();
    const typed = '{\n  "Version": "2011",\n  "Statement": [\n    {"Effect": "Allow", "Action": ["get"], "Resource": []}\n  ]\n}';
    typeInto(jsonBox(), typed);

    // At its field.
    const actions = within(statement(1)).getByRole('group', { name: 'Actions of statement 1' });
    expect(await within(actions).findByText(/Fault: statement 1: an action must be/)).toBeDefined();
    expect(window.document.getElementById(actions.getAttribute('aria-describedby') ?? '')?.textContent).toContain(
      'an action must be',
    );
    const resources = within(statement(1)).getByRole('group', { name: 'Resources of statement 1' });
    expect(within(resources).getByText(/Fault: statement 1: no resource/)).toBeDefined();
    expect(within(actions).queryByText(/no resource/)).toBeNull();
    expect(within(screen.getByRole('group', { name: 'Policy builder' })).getByText(/Fault: Version must be/)).toBeDefined();

    // In the JSON, with its line, and a way to it.
    const box = jsonBox();
    expect(box.getAttribute('aria-invalid')).toBe('true');
    const list = window.document.getElementById(box.getAttribute('aria-describedby')?.split(' ')[1] ?? '') as HTMLElement;
    expect(list.textContent).toContain('Statement 1, at /Statement/0/Action/0, line 4: statement 1: an action must be');
    expect(list.textContent).toContain('At /Version, line 2: Version must be');
    fireEvent.click(within(list).getByRole('button', { name: 'Show fault 1 in the JSON' }));
    expect(window.document.activeElement).toBe(box);
    expect(typed.slice(box.selectionStart, box.selectionEnd)).toBe('"get"');

    // Announced.
    const live = screen.getAllByRole('status').find((region) => region.getAttribute('aria-live') === 'polite') as HTMLElement;
    expect(live.textContent).toBe(
      '3 faults in the policy. The first: Statement 1, at /Statement/0/Action/0: statement 1: an action must be "s3:..." or "admin:..."',
    );
  });

  it('says when the node cannot check a policy, and then the save is checked when it is made', async () => {
    const api = builderNode(() => refusal(404, 'NotFound', 'Not found'), {
      'POST /policies': () => json(201, { name: 'p', document: {}, built_in: false, created_at: CREATED }),
    });
    render(<PoliciesScreen />);
    await builder();
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    typeInto(jsonBox(), JSON.stringify(ONE));

    expect(await screen.findByText(NO_VALIDATION_NOTICE)).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => {
      expect(api.callsTo('POST', '/policies')).toHaveLength(1);
    });
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
  });
});

describe('saving', () => {
  it('sends nothing validation refuses, and sends the document once it has no fault', async () => {
    let faults: PolicyFault[] = [{ pointer: '/Statement/0/Action', statement: 0, reason: 'statement 1: no action' }];
    const api = builderNode(() => json(200, verdict(faults)), {
      'POST /policies': () => json(201, { name: 'p', document: ONE, built_in: false, created_at: CREATED }),
    });
    render(<PoliciesScreen />);
    fireEvent.click(within(await builder()).getByRole('button', { name: 'Add a statement' }));
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });

    // Saved at once, before the pause: the document is checked first, and not sent.
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    expect((await screen.findByRole('alert')).textContent).toContain(NOT_SAVED_NOTICE);
    expect(api.callsTo('POST', '/policies')).toHaveLength(0);
    expect(within(statement(1)).getByText(/Fault: statement 1: no action/)).toBeDefined();

    faults = [];
    fireEvent.click(within(statement(1)).getByRole('checkbox', { name: 's3:GetObject' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => {
      expect(api.callsTo('POST', '/policies')).toHaveLength(1);
    });
    const sent = JSON.stringify({ ...ONE, Statement: [{ ...ONE.Statement[0], Action: ['s3:GetObject'] }] }, null, 2);
    expect(api.callsTo('POST', '/policies')[0]?.body).toBe(`{"name":"p","document":${sent}}`);
    // What was sent is what the node found no fault in, byte for byte.
    const checked = api.callsTo('POST', '/policy-validation').at(-1)?.body;
    expect(checked).toBe(`{"name":"p","document":${sent}}`);
  });

  it('sends nothing when the policy cannot be checked', async () => {
    const api = builderNode(() => refusal(503, 'ServiceUnavailable', 'Service unavailable'));
    render(<PoliciesScreen />);
    await builder();
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    typeInto(jsonBox(), JSON.stringify(ONE));
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Service unavailable');
    expect(api.callsTo('POST', '/policies')).toHaveLength(0);
  });

  it('shows the faults of a refused save at their fields', async () => {
    const faults: PolicyFault[] = [{ pointer: '/Statement/0/Effect', statement: 0, reason: 'statement 1: Effect must be Allow or Deny' }];
    builderNode(() => json(200, verdict()), {
      'POST /policies': () =>
        new Response(
          JSON.stringify({ type: 'about:blank', title: 'Invalid argument', status: 400, code: 'InvalidArgument', detail: faults[0]?.reason, faults }),
          { status: 400, headers: { 'Content-Type': 'application/problem+json' } },
        ),
    });
    render(<PoliciesScreen />);
    await builder();
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    typeInto(jsonBox(), JSON.stringify(ONE));
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Effect must be Allow or Deny');
    expect(within(statement(1)).getByText(/Fault: statement 1: Effect must be Allow or Deny/)).toBeDefined();
    expect(within(statement(1)).getByRole('combobox', { name: 'Effect of statement 1' }).getAttribute('aria-invalid')).toBe('true');
  });

  it('does not create a key whose inline policy validation refuses, and creates one without a policy unchecked', async () => {
    const api = builderNode(() => json(200, verdict([{ pointer: '/Statement', reason: 'no statement' }])), {
      'POST /users/bob/keys': () => json(201, { key: { access_key_id: 'AK', owner: 'bob', created_at: CREATED }, secret_key: 's3cr3t' }),
    });
    render(<ApiKeys user="bob" />);
    await builder();
    const label = 'Inline policy as JSON (optional; narrows what the key may do)';
    typeInto(jsonBox(label), '{"Version":"2012-10-17","Statement":[]}');
    fireEvent.click(screen.getByRole('button', { name: 'Create API key' }));

    expect((await screen.findByRole('alert')).textContent).toContain(NOT_SAVED_NOTICE);
    expect(api.callsTo('POST', '/users/bob/keys')).toHaveLength(0);

    typeInto(jsonBox(label), '');
    const before = api.callsTo('POST', '/policy-validation').length;
    fireEvent.click(screen.getByRole('button', { name: 'Create API key' }));
    expect(await screen.findByText('s3cr3t')).toBeDefined();
    expect(api.callsTo('POST', '/users/bob/keys')[0]?.body).toBe('{}');
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(before);
  });

  it('never sends a policy that is unchanged, however it is laid out', async () => {
    const stored = { name: 'get-only', document: ONE, built_in: false, created_at: CREATED };
    const api = builderNode(() => json(200, verdict()), { 'GET /policies': () => json(200, [stored]) });
    render(<PoliciesScreen />);
    fireEvent.click(await screen.findByRole('button', { name: 'Edit policy get-only' }));
    const label = 'Policy document of get-only as JSON';
    await builder(panelOf('get-only'));

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText('Nothing has changed, so there is nothing to save.')).toBeDefined();
    typeInto(jsonBox(label), JSON.stringify(ONE));
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText('Nothing has changed, so there is nothing to save.')).toBeDefined();
    expect(api.callsTo('PUT', '/policies/get-only')).toHaveLength(0);
  });

  it.each([
    ['the Policies screen', () => <PoliciesScreen />, JSON_LABEL],
    ['the API keys of a user', () => <ApiKeys user="bob" />, 'Inline policy as JSON (optional; narrows what the key may do)'],
    ['a role', () => <RoleScreen name="uploader" />, 'Policy document of put-only as JSON'],
  ])('asks nothing of the catalogue when %s is only viewed, and asks once when a policy is first edited', async (_, screenOf, label) => {
    const api = builderNode(() => json(200, verdict()), {
      'GET /roles/uploader': () => json(200, { name: 'uploader', policies: ['put-only'], built_in: false, created_at: CREATED }),
      'GET /policies/put-only': () => json(200, { name: 'put-only', document: ONE, built_in: false, created_at: CREATED }),
    });
    render(screenOf());
    const box = await screen.findByLabelText(label);
    // Viewing is not touching: a caller without admin:GetPolicyCatalogue is refused nothing, and nothing is audited.
    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    expect(screen.queryByRole('group', { name: 'Policy builder' })).toBeNull();

    typeInto(box, JSON.stringify(ONE));
    typeInto(box, JSON.stringify({ ...ONE, Statement: [] }));
    expect(await screen.findByRole('group', { name: 'Policy builder' })).toBeDefined();
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
  });

  it('asks nothing of the catalogue when an editor or a read-only built-in policy is focused', async () => {
    const builtIn = {
      name: 'read-only',
      document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:Get*'], Resource: '*' }] },
      built_in: true,
      created_at: CREATED,
    };
    const api = builderNode(() => json(200, verdict()), { 'GET /policies': () => json(200, [builtIn]) });
    render(<PoliciesScreen />);
    fireEvent.click(await screen.findByRole('button', { name: 'View policy read-only' }));
    const readOnly = jsonBox('Policy document of read-only as JSON');
    expect(readOnly.readOnly).toBe(true);
    // A reader tabbing through: nothing is asked, and a read-only policy offers no builder to open.
    fireEvent.focus(readOnly);
    fireEvent.focus(jsonBox());
    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    expect(within(panelOf('read-only')).queryByRole('button', { name: 'Open the policy builder' })).toBeNull();
    // Typing into a read-only box changes nothing, and asks nothing.
    fireEvent.change(readOnly, { target: { value: '{}' } });
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);

    // The first edit of an editable policy asks, once.
    typeInto(jsonBox(), JSON.stringify(ONE));
    expect(await screen.findAllByRole('group', { name: 'Policy builder' })).toHaveLength(2);
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
  });

  it('checks a save made while the catalogue is still loading, once, with the bytes it sends', async () => {
    let answerCatalogue: (() => void) | undefined;
    const api = builderNode(() => json(200, verdict()), {
      'POST /policies': () => json(201, { name: 'p', document: {}, built_in: false, created_at: CREATED }),
    });
    holdRequests('/policy-catalogue', (release) => {
      answerCatalogue = release;
    });
    render(<PoliciesScreen />);
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    const typed = JSON.stringify(ONE);
    typeInto(jsonBox(), typed);
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    // The save waits for the catalogue: nothing is sent unchecked.
    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS));
    expect(api.callsTo('POST', '/policies')).toHaveLength(0);
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);

    answerCatalogue?.();
    await waitFor(() => {
      expect(api.callsTo('POST', '/policies')).toHaveLength(1);
    });
    const checks = api.callsTo('POST', '/policy-validation');
    expect(checks).toHaveLength(1);
    expect(checks[0]?.body).toBe(api.callsTo('POST', '/policies')[0]?.body);
  });

  it('edits the policies of a role with the builder, one catalogue for them all', async () => {
    const stored = { name: 'put-only', document: ONE, built_in: false, created_at: CREATED };
    const api = builderNode(() => json(200, verdict()), {
      'GET /roles/uploader': () => json(200, { name: 'uploader', policies: ['put-only'], built_in: false, created_at: CREATED }),
      'GET /policies/put-only': () => json(200, stored),
      'PUT /policies/put-only': () => json(200, stored),
    });
    render(<RoleScreen name="uploader" />);
    const box = await screen.findByLabelText('Policy document of put-only as JSON');
    await builder(box.closest('section') as HTMLElement);

    fireEvent.click(within(statement(1)).getByRole('checkbox', { name: 's3:PutObject' }));
    await waitFor(() => {
      expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
    });
    expect((JSON.parse(api.callsTo('POST', '/policy-validation')[0]?.body ?? '{}') as { name?: string }).name).toBe('put-only');
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/policies/put-only')).toHaveLength(1);
    });
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
  });

  it('shows a built-in policy in the builder read-only, and checks nothing', async () => {
    const policy = {
      name: 'read-only',
      document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:Get*', 's3:List*'], Resource: '*' }] },
      built_in: true,
      created_at: CREATED,
    };
    const api = builderNode(() => json(200, verdict()), { 'GET /policies': () => json(200, [policy]) });
    render(<PoliciesScreen />);
    fireEvent.click(await screen.findByRole('button', { name: 'View policy read-only' }));
    const pane = await builder(panelOf('read-only'));

    expect(within(pane).getByText('s3:Get*')).toBeDefined();
    const effect = within(pane).getByRole('textbox', { name: 'Effect of statement 1' }) as HTMLInputElement;
    expect(effect.value).toBe('Allow');
    expect(effect.readOnly).toBe(true);
    expect(within(pane).queryAllByRole('button')).toHaveLength(0);
    expect(within(pane).queryAllByRole('checkbox')).toHaveLength(0);
    expect(within(pane).queryAllByRole('combobox')).toHaveLength(0);
    expect(jsonBox('Policy document of read-only as JSON').readOnly).toBe(true);
    expect(screen.queryByRole('button', { name: 'Save policy' })).toBeNull();
    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);
  });
});

describe('what is asked of the node', () => {
  it('checks no stored document as an editor opens, nor one of a role, nor an empty key policy', async () => {
    const stored = { name: 'get-only', document: ONE, built_in: false, created_at: CREATED };
    const api = builderNode(() => json(200, verdict()), {
      'GET /policies': () => json(200, [stored]),
      'GET /roles/uploader': () =>
        json(200, { name: 'uploader', policies: ['get-only', 'put-only'], built_in: false, created_at: CREATED }),
      'GET /policies/get-only': () => json(200, stored),
      'GET /policies/put-only': () => json(200, { ...stored, name: 'put-only' }),
    });
    render(<PoliciesScreen />);
    fireEvent.click(await screen.findByRole('button', { name: 'Edit policy get-only' }));
    await builder(panelOf('get-only'));
    render(<RoleScreen name="uploader" />);
    await screen.findByLabelText('Policy document of put-only as JSON');
    render(<ApiKeys user="bob" />);
    await screen.findByText('This user has no API keys.');

    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);
    for (const live of screen.getAllByRole('status').filter((region) => region.getAttribute('aria-live') === 'polite')) {
      expect(live.textContent).toBe('');
    }

    // Changed, and changed back: checked once, then nothing more to check.
    // The editor of the Policies screen, the first of the two that show get-only.
    const box = screen.getAllByLabelText('Policy document of get-only as JSON')[0] as HTMLTextAreaElement;
    const before = box.value;
    typeInto(box, JSON.stringify({ ...ONE, Statement: [] }));
    await waitFor(() => {
      expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
    });
    typeInto(box, before);
    await new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(1);
  });

  it.each([
    ['refused', () => refusal(403, 'AccessDenied', 'Access denied')],
    ['failing', () => refusal(500, 'InternalError', 'Internal error')],
  ])('works without bucket suggestions when listing the buckets is %s', async (_, buckets) => {
    builderNode(() => json(200, verdict()), { 'GET /buckets': buckets });
    render(<PoliciesScreen />);
    fireEvent.click(within(await builder()).getByRole('button', { name: 'Add a statement' }));
    const resources = within(statement(1)).getByRole('group', { name: 'Resources of statement 1' });

    const bucket = within(resources).getByRole('textbox', { name: 'Bucket for statement 1' });
    expect(bucket.getAttribute('list')).toBeNull();
    expect(resources.querySelector('datalist')).toBeNull();
    typeInto(bucket, 'photos');
    fireEvent.click(within(resources).getByRole('button', { name: 'Add bucket to statement 1' }));
    expect((document() as typeof ONE).Statement[0]?.Resource).toEqual(['arn:aws:s3:::photos']);
  });

  it('shows the faults of a refused save at the editor that sent it, and at no other', async () => {
    const faults: PolicyFault[] = [{ pointer: '/Statement/0/Action', statement: 0, reason: 'statement 1: no action' }];
    const stored = { name: 'get-only', document: ONE, built_in: false, created_at: CREATED };
    builderNode(() => json(200, verdict()), {
      'GET /policies': () => json(200, [stored]),
      'POST /policies': () =>
        new Response(JSON.stringify({ type: 'about:blank', title: 'Invalid argument', status: 400, code: 'InvalidArgument', faults }), {
          status: 400,
          headers: { 'Content-Type': 'application/problem+json' },
        }),
    });
    render(<PoliciesScreen />);
    fireEvent.click(await screen.findByRole('button', { name: 'Edit policy get-only' }));
    const editor = panelOf('get-only');
    await builder(editor);
    const create = screen.getByRole('heading', { name: 'Create a policy' }).closest('section') as HTMLElement;

    // The same text in both editors: only the one that sent it was refused.
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    typeInto(jsonBox(), jsonBox('Policy document of get-only as JSON').value);
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));

    expect(await within(create).findByText(/Fault: statement 1: no action/)).toBeDefined();
    expect(within(editor).queryByText(/no action/)).toBeNull();
  });

  it('clears what was being typed into a statement when one is removed', async () => {
    await withOneStatement();
    fireEvent.click(screen.getByRole('button', { name: 'Add a statement' }));
    typeInto(within(statement(2)).getByRole('textbox', { name: 'Action pattern for statement 2' }), 's3:Put*');
    typeInto(within(statement(1)).getByRole('textbox', { name: 'Action pattern for statement 1' }), 's3:Get*');

    fireEvent.click(screen.getByRole('button', { name: 'Remove statement 1' }));
    const pattern = within(statement(1)).getByRole('textbox', { name: 'Action pattern for statement 1' }) as HTMLInputElement;
    expect(pattern.value).toBe('');
  });

  it('never sends the form on Enter in a field of the builder', async () => {
    const api = await withOneStatement(
      builderNode(() => json(200, verdict()), {
        'POST /policies': () => json(201, { name: 'p', document: {}, built_in: false, created_at: CREATED }),
      }),
    );
    fireEvent.change(screen.getByLabelText('Policy name'), { target: { value: 'p' } });
    const inputs = Array.from(screen.getByRole('group', { name: 'Policy builder' }).querySelectorAll('input')).filter(
      (input) => input.type !== 'checkbox',
    );
    // Sid, search, pattern, bucket, prefix, name (disabled until a kind with a name), ARN, value.
    expect(inputs.map((input) => input.getAttribute('aria-label'))).toContain('Sid (optional) of statement 1');
    expect(inputs.length).toBeGreaterThanOrEqual(8);
    for (const input of inputs) {
      // false: the default - sending the form - was prevented.
      expect(fireEvent.keyDown(input, { key: 'Enter' }), input.getAttribute('aria-label') ?? '').toBe(false);
    }
    expect(api.callsTo('POST', '/policies')).toHaveLength(0);
  });
});

describe('access', () => {
  /** Puts the focus on a control as the Tab key would, and says it took it. */
  function reach(element: HTMLElement): HTMLElement {
    expect(element.tabIndex).toBeGreaterThanOrEqual(0);
    element.focus();
    expect(window.document.activeElement).toBe(element);
    return element;
  }

  /** What Enter or Space does to a focused native button: a click. */
  function press(element: HTMLElement) {
    reach(element);
    fireEvent.click(element);
  }

  it('makes a policy with the keyboard alone', async () => {
    const api = builderNode(() => json(200, verdict()), {
      'POST /policies': () => json(201, { name: 'p', document: {}, built_in: false, created_at: CREATED }),
    });
    render(<PoliciesScreen />);
    typeInto(reach(screen.getByLabelText('Policy name')), 'p');
    press(within(await builder()).getByRole('button', { name: 'Add a statement' }));

    // The new statement takes the focus.
    const effect = within(statement(1)).getByRole('combobox', { name: 'Effect of statement 1' });
    expect(window.document.activeElement).toBe(effect);
    fireEvent.change(effect, { target: { value: 'Deny' } });

    // Enter in a field adds what is typed there, and does not send the form.
    const pattern = reach(within(statement(1)).getByRole('textbox', { name: 'Action pattern for statement 1' }));
    typeInto(pattern, 's3:Put*');
    fireEvent.keyDown(pattern, { key: 'Enter' });
    const search = reach(within(statement(1)).getByRole('searchbox', { name: 'Search actions of statement 1' }));
    typeInto(search, 'GetObject');
    fireEvent.keyDown(search, { key: 'Enter' });
    press(within(statement(1)).getByRole('checkbox', { name: 's3:GetObject' }));
    const bucket = reach(await within(statement(1)).findByRole('combobox', { name: 'Bucket for statement 1' }));
    typeInto(bucket, 'photos');
    fireEvent.keyDown(bucket, { key: 'Enter' });
    const value = reach(within(statement(1)).getByRole('textbox', { name: 'Value for statement 1' }));
    typeInto(value, '192.0.2.0/24');
    fireEvent.keyDown(value, { key: 'Enter' });
    expect(api.callsTo('POST', '/policies')).toHaveLength(0);

    // Every group of actions opens from the keyboard: a summary is focusable.
    for (const summary of Array.from(statement(1).querySelectorAll('summary'))) {
      reach(summary as HTMLElement);
    }
    press(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => {
      expect(api.callsTo('POST', '/policies')).toHaveLength(1);
    });
    expect((JSON.parse(api.callsTo('POST', '/policies')[0]?.body ?? '{}') as { document: unknown }).document).toEqual({
      Version: '2012-10-17',
      Statement: [
        {
          Effect: 'Deny',
          Action: ['s3:Put*', 's3:GetObject'],
          Resource: ['arn:aws:s3:::photos/*'],
          Condition: { IpAddress: { 'aws:SourceIp': '192.0.2.0/24' } },
        },
      ],
    });
  });

  it('takes the focus to the button that adds a statement when one is removed', async () => {
    await withOneStatement();
    press(screen.getByRole('button', { name: 'Remove statement 1' }));
    expect(window.document.activeElement).toBe(screen.getByRole('button', { name: 'Add a statement' }));
  });

  it('names every control for a screen reader', async () => {
    await withOneStatement();
    fireEvent.click(screen.getByRole('button', { name: 'Add a statement' }));
    const editor = screen.getByRole('group', { name: 'Policy builder' }).closest('.policy-editor') as HTMLElement;
    for (const role of ['button', 'textbox', 'searchbox', 'combobox', 'checkbox']) {
      const all = within(editor).getAllByRole(role);
      expect(all.length).toBeGreaterThan(0);
      expect(within(editor).getAllByRole(role, { name: /\S/ })).toHaveLength(all.length);
    }
    // Two statements, and no name said twice.
    const names = within(editor)
      .getAllByRole('button')
      .map((button) => button.getAttribute('aria-label') ?? button.textContent);
    expect(new Set(names).size).toBe(names.length);
  });

  it('lays the builder and the JSON side by side on a wide screen, and one above the other below 576 px', async () => {
    await withOneStatement();
    const layout = screen.getByRole('group', { name: 'Policy builder' }).parentElement as HTMLElement;
    expect(layout.className).toBe('policy-builder');
    expect(layout.lastElementChild?.className).toBe('policy-json');

    const rule = (block: string, selector: string): string | undefined =>
      new RegExp(`(?:^|[,}\\s])${selector.replace('.', '\\.')}[^{]*\\{([^}]*)\\}`).exec(block)?.[1];
    const media = (query: string): string => {
      const at = stylesheet.indexOf(`@media (${query}) {\n  .policy-builder`);
      expect(at).toBeGreaterThanOrEqual(0);
      const end = stylesheet.indexOf('\n}\n', at);
      return stylesheet.slice(at, end);
    };
    // Outside any media query the builder is one column; side by side only on a wide screen.
    const plain = stylesheet.slice(stylesheet.indexOf('/* The policy builder'));
    expect(rule(plain, '.policy-alone')).toContain('grid-template-columns: minmax(0, 1fr);');
    expect(rule(media('min-width: 62rem'), '.policy-builder')).toContain('grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);');
    const phone = media('max-width: 575.98px');
    expect(phone).toMatch(/\.policy-builder,\s*\.policy-row,\s*\.policy-add\s*\{\s*grid-template-columns: minmax\(0, 1fr\);/);
    expect(phone).toContain('min-height: 44px');
  });
});
