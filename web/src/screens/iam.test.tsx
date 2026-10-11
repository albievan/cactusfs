import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { fakeApi, json, noContent, problem } from '../test/fakeApi';
import { ApiKeys } from './ApiKeys';
import { GroupsScreen } from './GroupsScreen';
import { PoliciesScreen } from './PoliciesScreen';
import { RoleScreen } from './RoleScreen';
import { RolesScreen } from './RolesScreen';
import { UserScreen } from './UserScreen';
import { UsersScreen } from './UsersScreen';

const CREATED = '2026-10-01T00:00:00Z';

function user(overrides: object = {}) {
  return {
    name: 'bob',
    groups: ['ops'],
    roles: ['read-only'],
    disabled: false,
    has_password: true,
    created_at: CREATED,
    ...overrides,
  };
}

function typeInto(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function body(call: { body: string } | undefined): unknown {
  return JSON.parse(call?.body ?? 'null');
}

describe('users', () => {
  it('lists the users and what the API says about them', async () => {
    fakeApi({ 'GET /users': () => json(200, [user(), user({ name: 'eve', disabled: true, has_password: false })]) });
    render(<UsersScreen />);

    const bob = await screen.findByRole('link', { name: 'bob' });
    expect(bob.getAttribute('href')).toBe('#/users/bob');
    const row = within(screen.getByRole('link', { name: 'eve' }).closest('tr') as HTMLElement);
    expect(row.getByText('Disabled')).toBeDefined();
    expect(row.getByText('Not set')).toBeDefined();
  });

  it('creates a user with roles, groups and a password', async () => {
    const api = fakeApi({
      'GET /users': () => json(200, []),
      'POST /users': () => json(201, user()),
    });
    render(<UsersScreen />);
    await screen.findByText('There are no users yet.');

    typeInto('User name', 'bob');
    typeInto('Console password (optional)', 'a long enough password');
    typeInto('Roles (separated by commas)', 'read-only, auditor');
    typeInto('Groups (separated by commas)', 'ops');
    fireEvent.click(screen.getByRole('button', { name: 'Create user' }));

    await waitFor(() => {
      expect(api.callsTo('GET', '/users')).toHaveLength(2);
    });
    expect(body(api.callsTo('POST', '/users')[0])).toEqual({
      name: 'bob',
      password: 'a long enough password',
      roles: ['read-only', 'auditor'],
      groups: ['ops'],
    });
  });

  it('leaves the password out when none is typed, and leaves judging it to the API', async () => {
    const api = fakeApi({
      'GET /users': () => json(200, []),
      'POST /users': () => problem(400, 'Invalid request', 'name does not match the pattern.'),
    });
    render(<UsersScreen />);
    await screen.findByText('There are no users yet.');

    typeInto('User name', '!bob');
    fireEvent.click(screen.getByRole('button', { name: 'Create user' }));

    expect((await screen.findByRole('alert')).textContent).toContain('name does not match the pattern.');
    expect(body(api.callsTo('POST', '/users')[0])).toEqual({ name: '!bob', roles: [], groups: [] });
  });

  it('changes roles, groups, status and password of a user', async () => {
    const api = fakeApi({
      'GET /users/bob': () => json(200, user()),
      'GET /users/bob/keys': () => json(200, []),
      'PUT /users/bob': () => json(200, user({ roles: ['read-write'], groups: [], disabled: true })),
    });
    render(<UserScreen name="bob" />);

    const roles = (await screen.findByLabelText('Roles (separated by commas)')) as HTMLInputElement;
    expect(roles.value).toBe('read-only');
    typeInto('Roles (separated by commas)', 'read-write');
    typeInto('Groups (separated by commas)', '');
    typeInto('New console password (leave empty to keep the current one)', 'another long password');
    fireEvent.click(screen.getByLabelText('Disabled'));
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));

    expect(await screen.findByText('Saved.')).toBeDefined();
    expect(body(api.callsTo('PUT', '/users/bob')[0])).toEqual({
      roles: ['read-write'],
      groups: [],
      disabled: true,
      password: 'another long password',
    });
    expect(
      (screen.getByLabelText('New console password (leave empty to keep the current one)') as HTMLInputElement).value,
    ).toBe('');
  });

  it('keeps the password when the field is left empty', async () => {
    const api = fakeApi({
      'GET /users/bob': () => json(200, user()),
      'GET /users/bob/keys': () => json(200, []),
      'PUT /users/bob': () => json(200, user()),
    });
    render(<UserScreen name="bob" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Save changes' }));

    await screen.findByText('Saved.');
    expect(body(api.callsTo('PUT', '/users/bob')[0])).toEqual({
      roles: ['read-only'],
      groups: ['ops'],
      disabled: false,
    });
  });

  it('deletes a user and goes back to the list', async () => {
    const api = fakeApi({
      'GET /users/bob': () => json(200, user()),
      'GET /users/bob/keys': () => json(200, []),
      'DELETE /users/bob': () => noContent(),
    });
    render(<UserScreen name="bob" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Delete user' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete user' }));

    await waitFor(() => {
      expect(window.location.hash).toBe('#/users');
    });
    expect(api.callsTo('DELETE', '/users/bob')).toHaveLength(1);
  });
});

describe('API keys', () => {
  const key = { access_key_id: 'AKIDEXAMPLE', owner: 'bob', description: 'backup job', created_at: CREATED };

  it('lists the keys of a user', async () => {
    fakeApi({
      'GET /users/bob/keys': () =>
        json(200, [{ ...key, expires_at: '2027-01-01T00:00:00Z', inline_policy: { Version: '2012-10-17' } }]),
    });
    render(<ApiKeys user="bob" />);

    expect(await screen.findByText('AKIDEXAMPLE')).toBeDefined();
    expect(screen.getByText('backup job')).toBeDefined();
    expect(screen.getByText(/"Version": "2012-10-17"/)).toBeDefined();
  });

  it('shows the secret of a new key exactly once and stores it nowhere', async () => {
    const secret = 'wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY';
    const api = fakeApi({
      'GET /users/bob/keys': () => json(200, []),
      'POST /users/bob/keys': () => json(201, { key, secret_key: secret }),
    });
    render(<ApiKeys user="bob" />);
    await screen.findByText('This user has no API keys.');

    typeInto('Description (optional)', 'backup job');
    typeInto('Expires (optional)', '2027-01-01T00:00');
    typeInto(
      'Inline policy as JSON (optional; narrows what the key may do)',
      '{"Version":"2012-10-17","Statement":[]}',
    );
    api.on({ 'GET /users/bob/keys': () => json(200, [key]) });
    fireEvent.click(screen.getByRole('button', { name: 'Create API key' }));

    expect(await screen.findByText(secret)).toBeDefined();
    expect(screen.getByText(/Copy the secret key now/)).toBeDefined();
    expect(body(api.callsTo('POST', '/users/bob/keys')[0])).toEqual({
      description: 'backup job',
      expires_at: new Date('2027-01-01T00:00').toISOString(),
      inline_policy: { Version: '2012-10-17', Statement: [] },
    });
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(secret);
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(secret);

    // Listing again does not bring the secret back, and neither does anything else.
    await screen.findByRole('button', { name: 'Revoke' });
    fireEvent.click(screen.getByRole('button', { name: 'I have copied it' }));

    expect(screen.queryByText(secret)).toBeNull();
    expect(screen.queryByText(/Copy the secret key now/)).toBeNull();
    expect(document.body.innerHTML).not.toContain(secret);
  });

  it('creates a key with nothing but defaults', async () => {
    const api = fakeApi({
      'GET /users/bob/keys': () => json(200, []),
      'POST /users/bob/keys': () => json(201, { key, secret_key: 's3cr3t-once' }),
    });
    render(<ApiKeys user="bob" />);
    await screen.findByText('This user has no API keys.');

    fireEvent.click(screen.getByRole('button', { name: 'Create API key' }));

    await screen.findByText('s3cr3t-once');
    expect(body(api.callsTo('POST', '/users/bob/keys')[0])).toEqual({});
  });

  it('does not send an inline policy that is not JSON', async () => {
    const api = fakeApi({ 'GET /users/bob/keys': () => json(200, []) });
    render(<ApiKeys user="bob" />);
    await screen.findByText('This user has no API keys.');

    typeInto('Inline policy as JSON (optional; narrows what the key may do)', '{"Version":');
    fireEvent.click(screen.getByRole('button', { name: 'Create API key' }));

    expect((await screen.findByRole('alert')).textContent).toContain('The policy is not valid JSON');
    expect(api.callsTo('POST', '/users/bob/keys')).toHaveLength(0);
  });

  it('revokes a key', async () => {
    let keys = [key];
    const api = fakeApi({
      'GET /users/bob/keys': () => json(200, keys),
      'DELETE /keys/AKIDEXAMPLE': () => {
        keys = [];
        return noContent();
      },
    });
    render(<ApiKeys user="bob" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, revoke' }));

    expect(await screen.findByText('This user has no API keys.')).toBeDefined();
    expect(api.callsTo('DELETE', '/keys/AKIDEXAMPLE')).toHaveLength(1);
  });
});

describe('groups', () => {
  it('creates, changes and deletes a group', async () => {
    let groups = [{ name: 'ops', roles: ['read-only'], created_at: CREATED }];
    const api = fakeApi({
      'GET /groups': () => json(200, groups),
      'POST /groups': (call) => {
        const created = { ...(JSON.parse(call.body) as { name: string; roles: string[] }), created_at: CREATED };
        groups = [...groups, created];
        return json(201, created);
      },
      'PUT /groups/ops': (call) => {
        const changed = { name: 'ops', ...(JSON.parse(call.body) as { roles: string[] }), created_at: CREATED };
        groups = groups.map((group) => (group.name === 'ops' ? changed : group));
        return json(200, changed);
      },
      'DELETE /groups/dev': () => {
        groups = groups.filter((group) => group.name !== 'dev');
        return noContent();
      },
    });
    render(<GroupsScreen />);
    await screen.findByText('ops');

    typeInto('Group name', 'dev');
    typeInto('Roles (separated by commas)', 'read-write');
    fireEvent.click(screen.getByRole('button', { name: 'Create group' }));
    await screen.findByText('dev');
    expect(body(api.callsTo('POST', '/groups')[0])).toEqual({ name: 'dev', roles: ['read-write'] });

    fireEvent.click(screen.getByRole('button', { name: 'Edit group ops' }));
    typeInto('Roles of group ops', 'read-only, auditor');
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText('read-only, auditor');
    expect(body(api.callsTo('PUT', '/groups/ops')[0])).toEqual({ roles: ['read-only', 'auditor'] });

    const row = within(screen.getByText('dev').closest('tr') as HTMLElement);
    fireEvent.click(row.getByRole('button', { name: 'Delete' }));
    fireEvent.click(row.getByRole('button', { name: 'Yes, delete' }));
    await waitFor(() => {
      expect(screen.queryByText('dev')).toBeNull();
    });
  });
});

describe('roles', () => {
  const roles = [
    { name: 'cluster-admin', policies: ['cluster-admin'], built_in: true, created_at: CREATED },
    { name: 'uploader', policies: ['put-only'], built_in: false, created_at: CREATED },
  ];

  it('shows built-in roles read-only, as the API marks them', async () => {
    fakeApi({ 'GET /roles': () => json(200, roles) });
    render(<RolesScreen />);

    const builtIn = within((await screen.findByText('Built-in')).closest('tr') as HTMLElement);
    expect(builtIn.getByText('Read-only')).toBeDefined();
    expect(builtIn.queryByRole('button')).toBeNull();
    expect(screen.getByRole('button', { name: 'Edit role uploader' })).toBeDefined();
  });

  it('creates a role and changes its policies', async () => {
    const api = fakeApi({
      'GET /roles': () => json(200, roles),
      'POST /roles': () => json(201, { name: 'viewer', policies: ['get-only'], built_in: false, created_at: CREATED }),
      'PUT /roles/uploader': () => json(200, { ...roles[1], policies: ['put-only', 'list'] }),
    });
    render(<RolesScreen />);
    await screen.findByText('uploader');

    typeInto('Role name', 'viewer');
    typeInto('Policies (separated by commas)', 'get-only');
    fireEvent.click(screen.getByRole('button', { name: 'Create role' }));
    await waitFor(() => {
      expect(body(api.callsTo('POST', '/roles')[0])).toEqual({ name: 'viewer', policies: ['get-only'] });
    });

    fireEvent.click(screen.getByRole('button', { name: 'Edit role uploader' }));
    typeInto('Policies of role uploader', 'put-only list');
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(body(api.callsTo('PUT', '/roles/uploader')[0])).toEqual({ policies: ['put-only', 'list'] });
    });
  });
});

describe('policies', () => {
  const document = { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:GetObject', Resource: '*' }] };
  const policies = [
    { name: 'read-only', document, built_in: true, created_at: CREATED },
    { name: 'get-only', document, built_in: false, created_at: CREATED },
  ];

  it('creates a policy from JSON text', async () => {
    const api = fakeApi({
      'GET /policies': () => json(200, []),
      'POST /policies': () => json(201, policies[1]),
    });
    render(<PoliciesScreen />);
    await screen.findByText('There are no policies.');

    typeInto('Policy name', 'get-only');
    typeInto('Policy document as JSON', JSON.stringify(document));
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));

    await waitFor(() => {
      expect(body(api.callsTo('POST', '/policies')[0])).toEqual({ name: 'get-only', document });
    });
  });

  it('checks the JSON syntax and nothing more', async () => {
    const api = fakeApi({
      'GET /policies': () => json(200, []),
      'POST /policies': () => problem(400, 'Invalid policy', 'Unknown field "Nonsense".', 'req-5'),
    });
    render(<PoliciesScreen />);
    await screen.findByText('There are no policies.');

    typeInto('Policy name', 'p');
    typeInto('Policy document as JSON', '{ not json');
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    expect((await screen.findByRole('alert')).textContent).toContain('The policy is not valid JSON');
    expect(api.callsTo('POST', '/policies')).toHaveLength(0);

    typeInto('Policy document as JSON', '{"Nonsense":true}');
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toContain('Unknown field "Nonsense".');
    });
    expect(body(api.callsTo('POST', '/policies')[0])).toEqual({ name: 'p', document: { Nonsense: true } });
  });

  it('shows a built-in policy read-only', async () => {
    fakeApi({ 'GET /policies': () => json(200, policies) });
    render(<PoliciesScreen />);

    fireEvent.click(await screen.findByRole('button', { name: 'View policy read-only' }));

    const editor = screen.getByLabelText('Policy document of read-only as JSON') as HTMLTextAreaElement;
    expect(editor.readOnly).toBe(true);
    expect(JSON.parse(editor.value)).toEqual(document);
    expect(screen.queryByRole('button', { name: 'Save policy' })).toBeNull();
  });

  it('changes and deletes a policy', async () => {
    let current = policies;
    const api = fakeApi({
      'GET /policies': () => json(200, current),
      'PUT /policies/get-only': (call) => {
        const changed = { ...policies[1], document: (JSON.parse(call.body) as { document: object }).document };
        current = [policies[0], changed] as typeof policies;
        return json(200, changed);
      },
      'DELETE /policies/get-only': () => {
        current = current.filter((policy) => policy.name !== 'get-only');
        return noContent();
      },
    });
    render(<PoliciesScreen />);

    fireEvent.click(await screen.findByRole('button', { name: 'Edit policy get-only' }));
    typeInto('Policy document of get-only as JSON', '{"Version":"2012-10-17","Statement":[]}');
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    await waitFor(() => {
      expect(body(api.callsTo('PUT', '/policies/get-only')[0])).toEqual({
        document: { Version: '2012-10-17', Statement: [] },
      });
    });

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));
    await waitFor(() => {
      expect(screen.queryByText('get-only')).toBeNull();
    });
    expect(api.callsTo('DELETE', '/policies/get-only')).toHaveLength(1);
  });
});

describe('one role', () => {
  const policies = [
    {
      name: 'put-only',
      built_in: false,
      created_at: CREATED,
      document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:PutObject', Resource: '*' }] },
    },
    {
      name: 'cluster-admin',
      built_in: true,
      created_at: CREATED,
      document: { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: ['s3:*', 'admin:*'], Resource: '*' }] },
    },
  ];

  it('is opened from the list of roles by a button that carries its name', async () => {
    fakeApi({
      'GET /roles': () => json(200, [{ name: 'uploader', policies: ['put-only'], built_in: false, created_at: CREATED }]),
    });
    render(<RolesScreen />);

    const open = await screen.findByRole('link', { name: 'uploader' });
    expect(open.getAttribute('href')).toBe('#/roles/uploader');
    expect(open.className).toContain('btn');
  });

  it('shows the policy of the role and saves a change to it', async () => {
    const api = fakeApi({
      'GET /roles/uploader': () =>
        json(200, { name: 'uploader', policies: ['put-only'], built_in: false, created_at: CREATED }),
      'GET /policies/put-only': () => json(200, policies[0]),
      'PUT /policies/put-only': () => json(200, policies[0]),
    });
    render(<RoleScreen name="uploader" />);

    const document = (await screen.findByLabelText('Policy document of put-only as JSON')) as HTMLTextAreaElement;
    expect(document.value).toContain('s3:PutObject');
    expect(document.readOnly).toBe(false);
    // The policy is asked for by its name: the list of all policies is not needed.
    expect(api.callsTo('GET', '/policies')).toHaveLength(0);
    // A policy can belong to several roles, and the screen says so.
    expect(screen.getByText(/changes it for every role that\s+has it/)).toBeDefined();

    const changed = { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:GetObject', Resource: '*' }] };
    fireEvent.change(document, { target: { value: JSON.stringify(changed) } });
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(body(api.callsTo('PUT', '/policies/put-only')[0])).toEqual({ document: changed });
    });
  });

  it('gives the role other policies, and deletes it', async () => {
    const api = fakeApi({
      'GET /roles/uploader': () =>
        json(200, { name: 'uploader', policies: ['put-only'], built_in: false, created_at: CREATED }),
      'GET /roles': () => json(200, []),
      'GET /policies/put-only': () => json(200, policies[0]),
      'GET /policies/list': () => problem(404, 'No such policy'),
      'PUT /roles/uploader': () =>
        json(200, { name: 'uploader', policies: ['put-only', 'list'], built_in: false, created_at: CREATED }),
      'DELETE /roles/uploader': () => noContent(),
    });
    render(<RoleScreen name="uploader" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Change policies' }));
    typeInto('Policies of role uploader', 'put-only list');
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(body(api.callsTo('PUT', '/roles/uploader')[0])).toEqual({ policies: ['put-only', 'list'] });
    });

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));
    await waitFor(() => {
      expect(api.callsTo('DELETE', '/roles/uploader')).toHaveLength(1);
    });
    await waitFor(() => {
      expect(window.location.hash).toBe('#/roles');
    });
  });

  it('shows a built-in role and its policy, and offers no change', async () => {
    fakeApi({
      'GET /roles/cluster-admin': () =>
        json(200, { name: 'cluster-admin', policies: ['cluster-admin'], built_in: true, created_at: CREATED }),
      'GET /policies/cluster-admin': () => json(200, policies[1]),
    });
    render(<RoleScreen name="cluster-admin" />);

    const document = (await screen.findByLabelText(
      'Policy document of cluster-admin as JSON',
    )) as HTMLTextAreaElement;
    expect(document.readOnly).toBe(true);
    expect(screen.getByText(/cannot be changed or deleted/)).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Save policy' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Change policies' })).toBeNull();
  });

  it('says which policy of the role cannot be read, and shows the others', async () => {
    fakeApi({
      'GET /roles/mixed': () =>
        json(200, { name: 'mixed', policies: ['put-only', 'secret', 'gone'], built_in: false, created_at: CREATED }),
      'GET /policies/put-only': () => json(200, policies[0]),
      'GET /policies/secret': () => problem(403, 'Access denied'),
      'GET /policies/gone': () => problem(404, 'No such policy'),
    });
    render(<RoleScreen name="mixed" />);

    expect(await screen.findByLabelText('Policy document of put-only as JSON')).toBeDefined();
    const secret = (await screen.findByRole('heading', { name: 'Policy secret' })).closest('section') as HTMLElement;
    expect((await within(secret).findByRole('alert')).textContent).toContain('Access denied');
    const gone = screen.getByRole('heading', { name: 'Policy gone' }).closest('section') as HTMLElement;
    expect(await within(gone).findByText(/does not exist/)).toBeDefined();
  });
});
