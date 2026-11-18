import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { AccessExplanation, PolicyCatalogue } from '../api/client';
import { holdRequests, json, refusal, signedIn, type Call, type FakeApi, type Handler } from '../test/fakeApi';
import {
  AccessScreen,
  CATALOGUE_ABSENT_WORDS,
  CATALOGUE_FAILED_WORDS,
  CATALOGUE_REFUSED_WORDS,
  NO_SUCH_KEY_WORDS,
  NO_SUCH_USER_WORDS,
  NOT_AVAILABLE_WORDS,
  REFUSED_WORDS,
} from './AccessScreen';
import { Screens } from './Screens';
import { UserScreen } from './UserScreen';

// "Who may do this?" (PLAN 9.10, ADR-0032 section 3): every criterion of the
// row. The node decides, cites and hides; the screen asks once per Explain
// and shows what it is told.

const NOW = '2026-11-18T10:00:00Z';

/** The console's stylesheet as it is shipped (a CSS import is left out of the tests). */
const stylesheet = readFileSync(resolve(import.meta.dirname, '../styles/console.css'), 'utf8');

function action(name: string, group: string, description: string, resources: string[]) {
  return {
    name,
    group,
    description,
    resources,
    read: true,
    by_name_only: false,
    in_bucket_policy: name.startsWith('s3:'),
    anonymous: false,
    in_sub_bucket_policy: false,
  };
}

/** A catalogue as the node answers it, cut down to what the tests ask about. */
const CATALOGUE: PolicyCatalogue = {
  version: '2012-10-17',
  actions: [
    action('s3:GetObject', 'Objects', 'Read an object and what is known of it.', ['arn:aws:s3:::{bucket}/{key}']),
    action('s3:ListBucket', 'Buckets', 'List the objects of a bucket.', ['arn:aws:s3:::{bucket}']),
    action('s3:ListAllMyBuckets', 'Buckets', 'List the buckets of the deployment.', ['arn:aws:s3:::*']),
    action('admin:GetUser', 'Users', 'Read a user.', ['arn:cactusfs:admin:::user/{name}']),
    action('admin:ExplainAccess', 'Users', 'Ask what a subject may do.', [
      'arn:cactusfs:admin:::user/{name}',
      'arn:cactusfs:admin:::user',
      'arn:cactusfs:admin:::anonymous',
    ]),
    action('admin:PreviewObject', 'Objects', 'Preview an object.', ['arn:aws:s3:::{bucket}/{key}']),
    action('admin:PutTagDefinition', 'Tags', 'Define a tag.', ['arn:cactusfs:admin:::tag-definition/{tag_key}']),
    // A template of a form this console does not know, as a newer node might give.
    action('admin:Unknown', 'Tags', 'Something of a newer release.', ['arn:cactusfs:other:::thing/{name}']),
    // Never in a catalogue the node gives; were it there, it is not offered.
    action('s3:*', 'Objects', 'Everything.', ['arn:aws:s3:::{bucket}']),
  ],
  condition_keys: [],
  resources: [
    { kind: 'all-buckets', template: 'arn:aws:s3:::*', description: 'the buckets as a whole' },
    { kind: 'bucket', template: 'arn:aws:s3:::{bucket}', description: 'a bucket, by its name' },
    { kind: 'object', template: 'arn:aws:s3:::{bucket}/{key}', description: 'an object of a bucket, by its key' },
    { kind: 'users', template: 'arn:cactusfs:admin:::user', description: 'the users as a whole' },
    { kind: 'user', template: 'arn:cactusfs:admin:::user/{name}', description: 'a user, by name' },
    { kind: 'anonymous', template: 'arn:cactusfs:admin:::anonymous', description: 'a caller without credentials' },
    {
      kind: 'tag-definition',
      template: 'arn:cactusfs:admin:::tag-definition/{tag_key}',
      description: 'a definition of the catalogue of tags, by its tag key',
    },
  ],
  limits: { max_bytes: 20480, max_statements: 64, max_values: 128, max_depth: 8 },
  bucket_policy_principals: { anyone: ['"*"'], user_arn_prefix: 'arn:aws:iam:::user/' },
};

/** An answer of the node; what a test does not say is a plain default deny for bob. */
function explanation(more: Partial<AccessExplanation> = {}): AccessExplanation {
  return {
    subject: { kind: 'user', user: 'bob' },
    request: { action: 's3:GetObject', resource: 'arn:aws:s3:::photos/2026/cat.jpg' },
    authenticates: true,
    allowed: false,
    source: 'none',
    statements: [],
    assumed: { time: NOW, secure_transport: true },
    not_considered: [],
    ...more,
  };
}

/** A refusal with a detail of its own, as the API gives a 400 its fixed text. */
function badRequest(detail: string): Response {
  const body = { type: 'about:blank', title: 'Invalid argument', status: 400, code: 'InvalidArgument', detail };
  return new Response(JSON.stringify(body), { status: 400, headers: { 'Content-Type': 'application/problem+json' } });
}

/** A node with the catalogue that answers questions with `answer`. */
function node(answer: Handler = () => json(200, explanation()), more: Record<string, Handler> = {}): FakeApi {
  return signedIn({
    'GET /policy-catalogue': () => json(200, CATALOGUE),
    'GET /access/explain': answer,
    ...more,
  });
}

function asked(api: FakeApi): Call[] {
  return api.callsTo('GET', '/access/explain');
}

function field(name: string): HTMLInputElement | HTMLSelectElement {
  return screen.getByLabelText(name) as HTMLInputElement | HTMLSelectElement;
}

function type(name: string, value: string) {
  fireEvent.change(field(name), { target: { value } });
}

/** Asks for the list of actions as a person does, with "Load the list of actions", and waits for it. */
async function use(): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: 'Load the list of actions' }));
  await waitFor(() => {
    expect(within(field('Action')).queryAllByRole('option').length).toBeGreaterThan(1);
  });
}

function explain() {
  fireEvent.click(screen.getByRole('button', { name: 'Explain' }));
}

/** The region the answer is announced in. */
function answerRegion(): HTMLElement {
  const region = screen.getByRole('heading', { name: 'The answer' }).nextElementSibling as HTMLElement;
  expect(region.getAttribute('aria-live')).toBe('polite');
  return region;
}

/** Asks about bob getting photos/2026/cat.jpg and waits for the answer. */
async function askAboutBob(): Promise<HTMLElement> {
  type('User', 'bob');
  type('Action', 's3:GetObject');
  type('Bucket', 'photos');
  type('Key of the object', '2026/cat.jpg');
  explain();
  const region = answerRegion();
  await within(region).findByRole('heading', { name: 'What was assumed' });
  return region;
}

describe('asking', () => {
  it('asks nothing as the screen is shown, not even the catalogue', async () => {
    const api = node();
    render(<AccessScreen />);

    expect(screen.getByRole('heading', { name: 'Who may do this?' })).toBeDefined();
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 50));
    });
    expect(asked(api)).toHaveLength(0);
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
  });

  it('sends nothing as every control is focused, as when the form is tabbed through', async () => {
    const api = node();
    render(<AccessScreen />);
    const form = screen.getByRole('form', { name: 'The question' });
    const controls = [...form.querySelectorAll<HTMLElement>('input, select, button, summary')];
    expect(controls.length).toBeGreaterThan(5);
    for (const control of controls) {
      act(() => {
        control.focus();
      });
    }
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 50));
    });
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    expect(asked(api)).toHaveLength(0);
  });

  it('reads nothing as the subject and the resource are filled in', async () => {
    const api = node();
    render(<AccessScreen />);
    type('User', 'bob');
    fireEvent.click(screen.getByRole('radio', { name: 'Anyone without credentials' }));
    fireEvent.click(screen.getByRole('radio', { name: 'A user' }));
    type('User', 'carol');
    type('Bucket (none for every bucket)', 'photos');
    fireEvent.click(screen.getByRole('radio', { name: 'An administrative resource' }));
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 50));
    });
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    expect(asked(api)).toHaveLength(0);
    expect(screen.getByRole('button', { name: 'Load the list of actions' })).toBeDefined();
  });

  it('reads the catalogue on the first change of the action, once', async () => {
    const api = node();
    render(<AccessScreen />);
    fireEvent.change(field('Action'), { target: { value: '' } });
    await waitFor(() => {
      expect(within(field('Action')).queryAllByRole('option').length).toBeGreaterThan(1);
    });
    type('Action', 's3:GetObject');
    type('Action', 'admin:GetUser');
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Load the list of actions' })).toBeNull();
  });

  it('reads the catalogue once when "Load the list of actions" is pressed, and puts the keyboard on the list', async () => {
    const api = node();
    render(<AccessScreen />);
    fireEvent.click(screen.getByRole('button', { name: 'Load the list of actions' }));
    expect(window.document.activeElement).toBe(field('Action'));
    await waitFor(() => {
      expect(within(field('Action')).queryAllByRole('option').length).toBeGreaterThan(1);
    });
    type('User', 'bob');
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
    expect(asked(api)).toHaveLength(0);
  });

  it('reads the catalogue once, and asks nothing while the form is filled in', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:GetObject');
    type('Bucket', 'photos');
    type('Key of the object', '2026/cat.jpg');
    act(() => {
      field('Bucket').focus();
    });
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 400));
    });

    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
    expect(asked(api)).toHaveLength(0);
  });

  it('asks once per Explain, with the question as the contract has it', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    await askAboutBob();

    expect(asked(api)).toHaveLength(1);
    const query = asked(api)[0]?.query;
    expect(Object.fromEntries(query ?? [])).toEqual({
      user: 'bob',
      action: 's3:GetObject',
      bucket: 'photos',
      key: '2026/cat.jpg',
    });

    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(2);
    });
    await within(answerRegion()).findByText('Not allowed');
    expect(asked(api)).toHaveLength(2);
  });

  it('asks once for an Explain pressed again while the node has not answered', async () => {
    const api = node();
    let release = () => {};
    holdRequests('/access/explain', (go) => {
      release = go;
    });
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:ListAllMyBuckets');
    explain();
    expect(await screen.findByText('Asking…')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Explain' }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(screen.getByRole('button', { name: 'Explain' }).closest('form') as HTMLFormElement);
    release();

    await within(answerRegion()).findByText('Not allowed');
    expect(asked(api)).toHaveLength(1);
    expect(asked(api)[0]?.query.get('bucket')).toBeNull();
  });

  it('never gives a question up as the screen goes, and shows nothing of its late answer', async () => {
    const api = node();
    const inner = globalThis.fetch;
    const signals: AbortSignal[] = [];
    const releases: (() => void)[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      if (new URL(request.url).pathname === '/api/v1/access/explain') {
        signals.push(request.signal);
        await new Promise<void>((resolve) => {
          releases.push(resolve);
        });
      }
      return inner(request);
    });

    // Left: the screen goes while the node answers.
    const left = render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:ListAllMyBuckets');
    explain();
    await waitFor(() => {
      expect(signals).toHaveLength(1);
    });
    left.unmount();
    expect(signals[0]?.aborted).toBe(false);
    releases[0]?.();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 50));
    });
    expect(screen.queryByRole('heading', { name: 'Who may do this?' })).toBeNull();
    expect(screen.queryByText('Not allowed')).toBeNull();

    // Made anew for another user: the answer to the question of before is not shown on the new form.
    window.location.hash = '#/access?user=bob';
    render(<Screens />);
    await use();
    type('Action', 's3:ListAllMyBuckets');
    explain();
    await waitFor(() => {
      expect(signals).toHaveLength(2);
    });
    act(() => {
      window.location.hash = '#/access?user=carol';
    });
    await waitFor(() => {
      expect(field('User').value).toBe('carol');
    });
    expect(signals[1]?.aborted).toBe(false);
    releases[1]?.();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(2);
    });
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 50));
    });
    expect(within(answerRegion()).queryByText('Not allowed')).toBeNull();
    expect(within(answerRegion()).getByText('Ask a question to see what the node decides.')).toBeDefined();
  });

  it('sends nothing for a question that is not one yet, and says what is missing', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    explain();
    expect(screen.getByText('Name the user to ask about.')).toBeDefined();
    type('User', 'bob');
    explain();
    expect(screen.getByText('Choose an action.')).toBeDefined();
    type('Action', 's3:GetObject');
    explain();
    expect(screen.getByText('Name the bucket.')).toBeDefined();
    type('Bucket', 'photos');
    explain();
    expect(screen.getByText('Give the key of the object.')).toBeDefined();

    expect(asked(api)).toHaveLength(0);
  });

  it('asks about an API key and about anyone', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    fireEvent.click(screen.getByRole('radio', { name: 'An API key' }));
    type('Access key ID', 'AKEXAMPLE');
    type('Action', 's3:ListAllMyBuckets');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    expect(Object.fromEntries(asked(api)[0]?.query ?? [])).toEqual({ access_key_id: 'AKEXAMPLE', action: 's3:ListAllMyBuckets' });

    fireEvent.click(screen.getByRole('radio', { name: 'Anyone without credentials' }));
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(2);
    });
    expect(Object.fromEntries(asked(api)[1]?.query ?? [])).toEqual({ anonymous: 'true', action: 's3:ListAllMyBuckets' });
  });

  it('sends the listing prefix, the source address and TLS when they are given', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:ListBucket');
    type('Bucket', 'photos');
    type('Listing prefix (for a listing only)', 'team/');
    type('Source address (IPv4 or IPv6; empty for unknown)', '192.0.2.7');
    type('Over TLS', 'no');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    expect(Object.fromEntries(asked(api)[0]?.query ?? [])).toEqual({
      user: 'bob',
      action: 's3:ListBucket',
      bucket: 'photos',
      list_prefix: 'team/',
      source_ip: '192.0.2.7',
      secure_transport: 'false',
    });
  });
});

describe('the catalogue', () => {
  it('offers the actions of the catalogue, by group, and no wildcard', async () => {
    node();
    render(<AccessScreen />);
    await use();
    const select = field('Action');
    const names = within(select)
      .getAllByRole('option')
      .map((option) => (option as HTMLOptionElement).value)
      .filter((value) => value !== '');
    expect(names).toEqual(expect.arrayContaining(['s3:GetObject', 'admin:GetUser', 'admin:ExplainAccess']));
    expect(names).not.toContain('s3:*');
    expect([...select.querySelectorAll('optgroup')].map((group) => group.label)).toEqual(['Objects', 'Buckets', 'Users', 'Tags']);

    type('Action', 'admin:GetUser');
    expect(screen.getByText('Read a user.')).toBeDefined();
  });

  it('offers the kinds of resource of the action, from its templates, and names the resource by them', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    type('User', 'carol');
    type('Action', 'admin:ExplainAccess');

    const kinds = within(field('Kind of resource'))
      .getAllByRole('option')
      .map((option) => option.textContent);
    expect(kinds).toEqual(['user: a user, by name', 'users: the users as a whole', 'anonymous: a caller without credentials']);
    expect(screen.queryByLabelText('Bucket')).toBeNull();
    type('Name of the user', 'bob');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    expect(asked(api)[0]?.query.get('admin_resource')).toBe('user/bob');

    type('Kind of resource', 'arn:cactusfs:admin:::anonymous');
    expect(screen.queryByLabelText('Name of the user')).toBeNull();
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(2);
    });
    expect(asked(api)[1]?.query.get('admin_resource')).toBe('anonymous');
    expect(asked(api)[1]?.query.get('bucket')).toBeNull();
  });

  it('names a tag definition by its tag key, slash and all', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 'admin:PutTagDefinition');
    type('Name of the tag-definition', 'team/x');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    expect(asked(api)[0]?.query.get('admin_resource')).toBe('tag-definition/team/x');
  });

  it('lets the resource be typed for an action whose templates this console cannot name', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 'admin:Unknown');
    expect(screen.queryByLabelText('Kind of resource')).toBeNull();
    expect(screen.getByText(/This console cannot name what the catalogue says this action is asked on/)).toBeDefined();
    fireEvent.click(screen.getByRole('radio', { name: 'An administrative resource' }));
    type('Kind (as user, policy, cluster)', 'thing');
    type('Name (optional)', 'one');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    expect(Object.fromEntries(asked(api)[0]?.query ?? [])).toEqual({
      user: 'bob',
      action: 'admin:Unknown',
      admin_resource: 'thing/one',
    });
  });

  it.each([
    ['refused (403)', () => refusal(403, 'AccessDenied', 'Access denied'), CATALOGUE_REFUSED_WORDS],
    ['absent on an older node (404)', () => refusal(404, 'NotFound', 'Not found'), CATALOGUE_ABSENT_WORDS],
    ['failing (500)', () => refusal(500, 'InternalError', 'Internal error'), CATALOGUE_FAILED_WORDS],
  ])('lets the action be typed when the catalogue is %s', async (_, catalogue, words) => {
    const api = node(undefined, { 'GET /policy-catalogue': catalogue });
    render(<AccessScreen />);
    fireEvent.click(screen.getByRole('button', { name: 'Load the list of actions' }));
    expect(await screen.findByText(words)).toBeDefined();
    // The keyboard was on the list of actions; it is on the field that took its place.
    await waitFor(() => {
      expect(field('Action').tagName).toBe('INPUT');
      expect(window.document.activeElement).toBe(field('Action'));
    });

    type('User', 'bob');
    type('Action', 's3:GetObject');
    type('Bucket (none for every bucket)', 'photos');
    type('Key of the object (optional)', 'a.txt');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(1);
    });
    expect(Object.fromEntries(asked(api)[0]?.query ?? [])).toEqual({
      user: 'bob',
      action: 's3:GetObject',
      bucket: 'photos',
      key: 'a.txt',
    });

    fireEvent.click(screen.getByRole('radio', { name: 'An administrative resource' }));
    type('Kind (as user, policy, cluster)', 'user');
    type('Name (optional)', 'carol');
    type('Action', 'admin:GetUser');
    explain();
    await waitFor(() => {
      expect(asked(api)).toHaveLength(2);
    });
    expect(Object.fromEntries(asked(api)[1]?.query ?? [])).toEqual({
      user: 'bob',
      action: 'admin:GetUser',
      admin_resource: 'user/carol',
    });
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
  });
});

describe('the answer', () => {
  it('shows an allowance by a statement of a named policy, as cited', async () => {
    node(() =>
      json(
        200,
        explanation({
          allowed: true,
          source: 'identity-policy',
          statements: [
            {
              source: 'identity-policy',
              effect: 'Allow',
              readable: true,
              policy: 'photo-readers',
              document_index: 0,
              statement_index: 2,
              sid: 'ReadPhotos',
            },
          ],
        }),
      ),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();

    expect(within(region).getByText('Allowed')).toBeDefined();
    expect(within(region).getByText("a policy of the subject's", { selector: 'dd' })).toBeDefined();
    const cited = within(within(region).getByRole('region', { name: 'Statements that had a say' })).getByRole('listitem');
    expect(cited.textContent).toBe("Allow in a policy of the subject's photo-readers, document 0, statement 2, Sid ReadPhotos");
    expect(within(region).getByText('arn:aws:s3:::photos/2026/cat.jpg')).toBeDefined();
  });

  it('shows a statement in a policy the caller may not read as such, and nothing of where it is', async () => {
    node(() =>
      json(
        200,
        explanation({
          allowed: false,
          source: 'bucket-policy',
          statements: [
            { source: 'identity-policy', effect: 'Allow', readable: true, policy: 'photo-readers', document_index: 0, statement_index: 0 },
            { source: 'bucket-policy', effect: 'Deny', readable: false },
          ],
        }),
      ),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();

    const items = within(within(region).getByRole('region', { name: 'Statements that had a say' })).getAllByRole('listitem');
    expect(items.map((item) => item.textContent)).toEqual([
      "Allow in a policy of the subject's photo-readers, document 0, statement 0",
      "Deny in the bucket's policy: a policy you may not read",
    ]);
    expect(within(region).getByText('Not allowed')).toBeDefined();
  });

  it('says that no decision was given when the answer has none', async () => {
    const given = explanation();
    delete given.allowed;
    node(() => json(200, given));
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    expect(within(region).getByText('No decision was given')).toBeDefined();
    expect(within(region).queryByText('Allowed')).toBeNull();
    expect(within(region).queryByText('Not allowed')).toBeNull();
  });

  it('names a key without its owner when the answer does not give one', async () => {
    node(() => json(200, explanation({ subject: { kind: 'access_key', access_key_id: 'AKEXAMPLE' } })));
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    expect(within(region).getByText('the API key AKEXAMPLE')).toBeDefined();
  });

  it('shows a default deny: no statement had a say', async () => {
    node();
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();

    expect(within(region).getByText('Not allowed')).toBeDefined();
    expect(within(region).getByText('No statement: nothing allows it, so it is denied by default.')).toBeDefined();
    expect(within(within(region).getByRole('region', { name: 'Statements that had a say' })).getByText('None.')).toBeDefined();
  });

  it('shows a decision by a rule of the authorizer', async () => {
    node(() => json(200, explanation({ allowed: false, source: 'rule', rule: 'no_user' })));
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    expect(within(region).getByText('A rule of the authorizer: there is no such user.')).toBeDefined();
  });

  it("shows the verdict of the key's own policy", async () => {
    node(() =>
      json(
        200,
        explanation({
          subject: { kind: 'access_key', user: 'bob', access_key_id: 'AKEXAMPLE' },
          allowed: false,
          source: 'api-key-policy',
          key_policy: { allowed: false, statements: [{ source: 'api-key-policy', effect: 'Deny', readable: false }] },
        }),
      ),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();

    expect(within(region).getByText('the API key AKEXAMPLE of bob')).toBeDefined();
    const key = within(region).getByRole('region', { name: "The key's own policy" });
    expect(within(key).getByText('It does not allow this.')).toBeDefined();
    expect(within(key).getByRole('listitem').textContent).toBe("Deny in the key's own policy: a policy you may not read");
  });

  it('shows what was assumed: TLS as the node assumed it, and an unknown address with what it decided', async () => {
    const api = node(() =>
      json(
        200,
        explanation({
          unknown_address: [{ source: 'identity-policy', effect: 'Allow', readable: true, policy: 'office', document_index: 0, statement_index: 1 }],
        }),
      ),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();

    // TLS was not said, so it was not sent: the node says what it assumed.
    expect(asked(api)[0]?.query.has('secure_transport')).toBe(false);
    const assumed = within(region).getByRole('region', { name: 'What was assumed' });
    const facts = [...assumed.querySelectorAll('dt')].map((dt) => [dt.textContent, dt.nextElementSibling?.textContent]);
    expect(facts).toEqual([
      ["Time, the node's", new Date(NOW).toLocaleString()],
      ['Source address', 'Unknown'],
      ['Over TLS', 'Yes'],
    ]);
    expect(assumed.querySelector('time')?.getAttribute('dateTime')).toBe(NOW);
    const unknown = within(region).getByRole('region', { name: 'Decided without a source address' });
    expect(within(unknown).getByRole('listitem').textContent).toBe("Allow in a policy of the subject's office, document 0, statement 1");
  });

  it('shows the address and TLS the node took', async () => {
    node(() => json(200, explanation({ assumed: { time: NOW, source_ip: '192.0.2.7', secure_transport: false } })));
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    const assumed = within(region).getByRole('region', { name: 'What was assumed' });
    expect(within(assumed).getByText('192.0.2.7')).toBeDefined();
    expect(within(assumed).getByText('No')).toBeDefined();
  });

  it('shows what was not considered', async () => {
    node(() =>
      json(200, explanation({ request: { action: 'admin:PreviewObject', resource: 'arn:aws:s3:::photos/a' }, not_considered: ['object_ownership'] })),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    const left = within(region).getByRole('region', { name: 'What was not considered' });
    expect(within(left).getByRole('listitem').textContent).toBe('who made the version of the object (its ownership)');
  });

  it('says that nothing was left out when nothing was', async () => {
    node();
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    expect(within(within(region).getByRole('region', { name: 'What was not considered' })).getByText('Nothing.')).toBeDefined();
  });

  it('shows a subject that would not authenticate, and no decision', async () => {
    node(() =>
      json(200, {
        subject: { kind: 'user', user: 'bob' },
        request: { action: 's3:GetObject', resource: 'arn:aws:s3:::photos/2026/cat.jpg' },
        authenticates: false,
        not_authenticated: 'user_disabled',
        assumed: { time: NOW, secure_transport: true },
        not_considered: [],
      }),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    expect(region.textContent).toContain('Would not authenticate: the user is disabled (for an API key: its owner is). No decision is given.');
    expect(within(region).queryByText('Allowed')).toBeNull();
    expect(within(region).queryByText('Not allowed')).toBeNull();
    expect(within(region).queryByRole('region', { name: 'Statements that had a say' })).toBeNull();
  });

  it('says only that the subject would not authenticate when the answer gives no reason', async () => {
    node(() =>
      json(200, {
        subject: { kind: 'user', user: 'bob' },
        request: { action: 's3:GetObject', resource: 'arn:aws:s3:::photos/2026/cat.jpg' },
        authenticates: false,
        assumed: { time: NOW, secure_transport: true },
        not_considered: [],
      }),
    );
    render(<AccessScreen />);
    await use();
    const region = await askAboutBob();
    expect(region.textContent).toContain('Would not authenticate. No decision is given.');
  });
});

describe('refusals', () => {
  it('says plainly that the question was refused (403), and shows no answer', async () => {
    node(() => refusal(403, 'AccessDenied', 'Access denied'));
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:GetObject');
    type('Bucket', 'photos');
    type('Key of the object', 'a');
    explain();

    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(REFUSED_WORDS)).toBeDefined();
    expect(within(alert).getByText('The server said: Access denied')).toBeDefined();
    expect(within(answerRegion()).queryByText('Not allowed')).toBeNull();
  });

  it.each([
    ['user', NO_SUCH_USER_WORDS],
    ['access_key', NO_SUCH_KEY_WORDS],
  ])('says that the %s is not there (404 NoSuchEntity)', async (subject, words) => {
    node(() => refusal(404, 'NoSuchEntity', 'No such entity'));
    render(<AccessScreen />);
    await use();
    if (subject === 'access_key') {
      fireEvent.click(screen.getByRole('radio', { name: 'An API key' }));
      type('Access key ID', 'AKGONE');
    } else {
      type('User', 'gone');
    }
    type('Action', 's3:ListAllMyBuckets');
    explain();
    expect(within(await screen.findByRole('alert')).getByText(words)).toBeDefined();
  });

  it('shows the fixed text of a 400 as the node gives it', async () => {
    node(() => badRequest('explain.Explain: the source address is no IP address without a zone'));
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:ListAllMyBuckets');
    type('Source address (IPv4 or IPv6; empty for unknown)', 'not-an-address');
    explain();
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText('Invalid argument')).toBeDefined();
    expect(within(alert).getByText('explain.Explain: the source address is no IP address without a zone')).toBeDefined();
  });

  it('says that the question is not available on a node of an older release (404 NotFound)', async () => {
    node(() => refusal(404, 'NotFound', 'No such endpoint'));
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:ListAllMyBuckets');
    explain();
    expect(within(await screen.findByRole('alert')).getByText(NOT_AVAILABLE_WORDS)).toBeDefined();
  });

  it('clears a refusal when the next question is answered', async () => {
    let first = true;
    node(() => {
      const answer = first ? refusal(403, 'AccessDenied', 'Access denied') : json(200, explanation());
      first = false;
      return answer;
    });
    render(<AccessScreen />);
    await use();
    type('User', 'bob');
    type('Action', 's3:ListAllMyBuckets');
    explain();
    await screen.findByRole('alert');
    explain();
    await within(answerRegion()).findByText('Not allowed');
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('the way there', () => {
  it("links from a user's page to the screen with the user filled in, and asks nothing on arrival", async () => {
    const api = node(undefined, {
      'GET /users/bob': () =>
        json(200, { name: 'bob', roles: [], groups: [], disabled: false, has_password: false, created_at: NOW }),
      'GET /users/bob/keys': () => json(200, []),
    });
    render(<UserScreen name="bob" />);
    const link = await screen.findByRole('link', { name: 'What may bob do?' });
    expect(link.getAttribute('href')).toBe('#/access?user=bob');
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);

    window.location.hash = link.getAttribute('href') ?? '';
    render(<Screens />);
    expect(await screen.findByRole('heading', { name: 'Who may do this?' })).toBeDefined();
    expect((screen.getByRole('radio', { name: 'A user' }) as HTMLInputElement).checked).toBe(true);
    expect(field('User').value).toBe('bob');
    await act(async () => {
      await new Promise((settle) => setTimeout(settle, 50));
    });
    expect(asked(api)).toHaveLength(0);
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
  });

  it('has the screen at #/access without a user', async () => {
    window.location.hash = '#/access';
    node();
    render(<Screens />);
    expect(await screen.findByRole('heading', { name: 'Who may do this?' })).toBeDefined();
    expect(field('User').value).toBe('');
  });
});

describe('accessibility', () => {
  it('names every control, and sends the question on the submit of its form (what Enter in a field does)', async () => {
    const api = node();
    render(<AccessScreen />);
    await use();
    for (const name of [
      'User',
      'Action',
      'Source address (IPv4 or IPv6; empty for unknown)',
      'Over TLS',
    ]) {
      expect(field(name)).toBeDefined();
    }
    for (const name of ['A user', 'An API key', 'Anyone without credentials']) {
      expect(screen.getByRole('radio', { name })).toBeDefined();
    }
    expect(screen.getByRole('group', { name: 'Who' })).toBeDefined();
    expect(screen.getByRole('group', { name: 'Does what' })).toBeDefined();
    expect(screen.getByRole('group', { name: 'On what' })).toBeDefined();
    expect(field('Action').getAttribute('aria-describedby')).not.toBeNull();

    type('User', 'bob');
    type('Action', 's3:ListAllMyBuckets');
    // Enter in a field submits its form; jsdom has no implicit submission, so the submit is sent as Enter would send it.
    fireEvent.submit(field('User').closest('form') as HTMLFormElement);
    await within(answerRegion()).findByText('Not allowed');
    expect(asked(api)).toHaveLength(1);
  });

  it('lays the question out in one column below 576 px', () => {
    const at = stylesheet.indexOf('@media (max-width: 575.98px) {\n  .access-form');
    expect(at).toBeGreaterThanOrEqual(0);
    const phone = stylesheet.slice(at, stylesheet.indexOf('\n}\n', at));
    expect(phone).toMatch(/\.access-form,\s*\.access-group,\s*\.access-more-fields\s*\{\s*grid-template-columns: minmax\(0, 1fr\);/);
    expect(phone).toContain('min-height: 44px');
  });
});
