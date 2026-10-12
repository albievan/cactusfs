import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { fakeApi, json, problem } from '../test/fakeApi';
import { SettingsScreen } from './SettingsScreen';

const DAY = 24 * 60 * 60 * 1000;
const GIB = 1024 * 1024 * 1024;

/** A value the API must never send and the console must never show. */
const SMUGGLED_SECRET = 'smuggled-passphrase-7f3a';
/** The value of a setting whose kind this console does not know. */
const SMUGGLED_UNKNOWN = 'leaked-by-a-newer-node-91c2';

function setting(name: string, extra: object = {}) {
  return {
    name,
    env: `CACTUSFS_${name.toUpperCase().replaceAll('-', '_')}`,
    flag: name,
    group: 'Server',
    kind: 'plain',
    scope: 'node',
    source: 'default',
    set: true,
    value: `value-of-${name}`,
    ...extra,
  };
}

const SETTINGS = [
  setting('log-level', { group: 'Logging', value: 'info' }),
  setting('s3-address', { group: 'Listeners', source: 'flag', value: ':9000' }),
  setting('otlp-endpoint', { group: 'Telemetry', kind: 'url', source: 'environment', value: 'https://otel.example:4318' }),
  // What must never happen, and is not shown if it does: a secret with a value.
  setting('kms-passphrase', {
    group: 'Keys',
    kind: 'secret',
    scope: 'cluster',
    source: 'environment',
    flag: undefined,
    value: SMUGGLED_SECRET,
  }),
  setting('root-secret-file', { group: 'Keys', kind: 'secret-file', set: false, value: undefined }),
  // A node of a newer release: a kind and a source this console does not know.
  setting('future-thing', { group: 'Keys', kind: 'quantum', source: 'operator', value: SMUGGLED_UNKNOWN }),
  setting('node-zone', { group: 'Placement', set: false, value: undefined }),
];

function answeringNode(id: string, extra: object = {}) {
  return {
    node_id: id,
    answered: true,
    roles: ['gateway', 'storage'],
    labels: { zone: 'a' },
    server: { version: '0.7.0', build: '412', commit: '3300281abcde' },
    started_at: '2026-10-01T00:00:00Z',
    kms_key_id: 'cactusfs-data',
    settings: SETTINGS,
    drives: [
      {
        id: 'drive-0123456789abcdef',
        directory: '/data/1',
        label: 'cactusfs_node1-data1',
        filesystem: { type: 'ext4', device: '/dev/sdb1', mount_point: '/data/1' },
        total_bytes: 8 * GIB,
        free_bytes: 2 * GIB,
        online: true,
        erasure_set: 0,
      },
      // Only what the contract requires of a drive.
      { id: 'drive-b', directory: '/data/2', total_bytes: 4 * GIB, free_bytes: GIB, online: false },
    ],
    metadata: {
      directory: '/meta',
      filesystem: { type: 'xfs', device: '/dev/sdc1', mount_point: '/meta' },
      total_bytes: 16 * GIB,
      free_bytes: 12 * GIB,
    },
    certificates: [
      {
        use: 's3',
        subject: 'CN=s3.example',
        issuer: 'CN=Example CA',
        dns_names: ['s3.example'],
        ip_addresses: ['192.0.2.10'],
        not_before: new Date(Date.now() - 300 * DAY).toISOString(),
        not_after: new Date(Date.now() + 5 * DAY).toISOString(),
      },
      {
        use: 'node',
        subject: 'CN=old-node',
        issuer: 'CN=Example CA',
        dns_names: [],
        ip_addresses: [],
        not_before: new Date(Date.now() - 400 * DAY).toISOString(),
        not_after: new Date(Date.now() - 2 * DAY).toISOString(),
      },
      {
        // A use this console does not know is shown as it came.
        use: 'replication',
        subject: 'CN=fresh',
        issuer: 'CN=Example CA',
        dns_names: ['fresh.example'],
        ip_addresses: [],
        not_before: new Date(Date.now() - DAY).toISOString(),
        not_after: new Date(Date.now() + 300 * DAY).toISOString(),
      },
    ],
    ...extra,
  };
}

function answer(extra: object = {}) {
  return {
    answered_by: 'node-1',
    cluster: {
      deployment_id: 'dep-1',
      metadata_level: 4,
      leader: 'node-2',
      nodes: 2,
      voters: 1,
      erasure_sets: [{ index: 0, drives: 4, data: 2, parity: 2, read_quorum: 2, write_quorum: 3 }],
    },
    nodes: [answeringNode('node-1')],
    mismatches: [],
    ...extra,
  };
}

/** The card of a node, once the settings are on screen. */
async function cardOf(nodeID: string): Promise<HTMLElement> {
  return screen.findByRole('region', { name: `Node ${nodeID}` });
}

/** What a tile of figures says under a name. */
function statOf(container: HTMLElement, name: string): string | undefined {
  return within(container).getByText(name).closest('.stat')?.querySelector('dd')?.textContent;
}

/** The names of the settings a grid lists, in its order. */
function listedIn(table: HTMLElement): string[] {
  return Array.from(table.querySelectorAll('tbody .settings-name')).map((cell) => cell.textContent);
}

/** The headings of the groups a grid lists, in its order. */
function groupsIn(table: HTMLElement): string[] {
  return Array.from(table.querySelectorAll('.grid-group')).map((row) => row.textContent);
}

function rowOf(table: HTMLElement, text: string): HTMLElement {
  return within(table).getByText(text).closest('tr') as HTMLElement;
}

describe('settings', () => {
  it('asks for the settings once and shows what holds for the deployment', async () => {
    const api = fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);

    const cluster = await screen.findByRole('region', { name: 'Cluster' });
    expect(within(cluster).getByText('dep-1')).toBeDefined();
    expect(statOf(cluster, 'Metadata schema level')).toBe('4');
    expect(statOf(cluster, 'Metadata leader')).toBe('node-2');
    expect(statOf(cluster, 'Nodes')).toBe('2');
    expect(statOf(cluster, 'Voters')).toBe('1');
    expect(statOf(cluster, 'Answered by')).toBe('node-1');

    const sets = within(cluster).getByRole('table', { name: 'Erasure sets' });
    const [, first] = within(sets).getAllByRole('row');
    expect(within(first as HTMLElement).getAllByRole('cell').map((cell) => cell.textContent)).toEqual([
      '0',
      '4',
      '2',
      '2',
      '2',
      '3',
    ]);

    expect(api.callsTo('GET', '/settings')).toHaveLength(1);
    expect(api.calls).toHaveLength(1);
  });

  it('shows the node that answered with its drives, its metadata volume and its certificates', async () => {
    fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);

    const card = within(await cardOf('node-1'));
    expect(card.getByText('answered')).toBeDefined();
    expect(card.getByText('v0.7.0 · build 412')).toBeDefined();
    expect(card.getByText('gateway')).toBeDefined();
    expect(card.getByText('storage')).toBeDefined();
    expect(card.getByText('zone=a')).toBeDefined();
    expect(card.getByText('cactusfs-data')).toBeDefined();

    const drives = card.getByRole('table', { name: 'Drives of node-1' });
    const first = within(rowOf(drives, '/data/1'));
    // The ID is cut short; all of it is in the hint.
    expect(first.getByText('drive-01…').getAttribute('title')).toBe('drive-0123456789abcdef');
    expect(first.getByText('cactusfs_node1-data1')).toBeDefined();
    expect(first.getByText('ext4 · /dev/sdb1 · /data/1')).toBeDefined();
    expect(first.getByText('8.0 GiB')).toBeDefined();
    expect(first.getByText('2.0 GiB')).toBeDefined();
    expect(first.getByText('online')).toBeDefined();

    // A drive of which only what must be told is told: no label, no file system, no erasure set.
    const second = rowOf(drives, '/data/2');
    expect(within(second).getByText('offline')).toBeDefined();
    expect(within(second).getByText('drive-b')).toBeDefined();
    expect(within(second).getAllByText('-')).toHaveLength(3);

    expect(card.getByText('/meta')).toBeDefined();
    expect(card.getByText('xfs · /dev/sdc1 · /meta')).toBeDefined();
    expect(card.getByText('16.0 GiB')).toBeDefined();
    expect(card.getByText('12.0 GiB')).toBeDefined();

    const certificates = card.getByRole('table', { name: 'Certificates of node-1' });
    const soon = within(rowOf(certificates, 'CN=s3.example'));
    expect(soon.getByText('s3.example, 192.0.2.10')).toBeDefined();
    expect(soon.getByText('expires soon')).toBeDefined();
    expect(soon.queryByText('expired')).toBeNull();
    const old = within(rowOf(certificates, 'CN=old-node'));
    expect(old.getByText('expired')).toBeDefined();
    expect(old.queryByText('expires soon')).toBeNull();
    const fresh = within(rowOf(certificates, 'CN=fresh'));
    expect(fresh.getByText('replication')).toBeDefined();
    expect(fresh.queryByText('expired')).toBeNull();
    expect(fresh.queryByText('expires soon')).toBeNull();
  });

  it('lists the settings under their groups with value, source, variable and flag', async () => {
    fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);

    const table = within(await cardOf('node-1')).getByRole('table', { name: 'Settings of node-1' });
    // Groups and settings in the order the API gave them.
    expect(groupsIn(table)).toEqual(['Logging', 'Listeners', 'Telemetry', 'Keys', 'Placement']);
    expect(listedIn(table)).toEqual([
      'log-level',
      's3-address',
      'otlp-endpoint',
      'kms-passphrase',
      'root-secret-file',
      'future-thing',
      'node-zone',
    ]);

    const address = within(rowOf(table, 's3-address'));
    expect(address.getByText(':9000')).toBeDefined();
    expect(address.getByText('flag')).toBeDefined();
    expect(address.getByText('CACTUSFS_S3_ADDRESS')).toBeDefined();
    expect(address.getByText('--s3-address')).toBeDefined();

    expect(within(rowOf(table, 'otlp-endpoint')).getByText('https://otel.example:4318')).toBeDefined();
    // A setting that has no value says so.
    expect(within(rowOf(table, 'node-zone')).getByText('not set')).toBeDefined();
    // A secret has no flag.
    expect(within(rowOf(table, 'kms-passphrase')).getByText('-')).toBeDefined();
  });

  it('shows a secret as set or not set, and never a value the answer should not have held', async () => {
    fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);

    const table = within(await cardOf('node-1')).getByRole('table', { name: 'Settings of node-1' });
    expect(within(rowOf(table, 'kms-passphrase')).getByText('set')).toBeDefined();
    expect(within(rowOf(table, 'root-secret-file')).getByText('not set')).toBeDefined();

    expect(document.body.textContent).not.toContain(SMUGGLED_SECRET);
    // Not in a hint or any other attribute either.
    expect(document.body.innerHTML).not.toContain(SMUGGLED_SECRET);
  });

  it('reads a kind it does not know as a secret, and shows a source it does not know as it came', async () => {
    fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);

    const table = within(await cardOf('node-1')).getByRole('table', { name: 'Settings of node-1' });
    const row = within(rowOf(table, 'future-thing'));
    expect(row.getByText('set')).toBeDefined();
    expect(row.getByText('operator')).toBeDefined();

    expect(document.body.textContent).not.toContain(SMUGGLED_UNKNOWN);
    expect(document.body.innerHTML).not.toContain(SMUGGLED_UNKNOWN);
  });

  it('says in words why a node did not answer, and shows nothing else of it', async () => {
    fakeApi({
      'GET /settings': () =>
        json(
          200,
          answer({
            nodes: [
              answeringNode('node-1'),
              { node_id: 'node-2', answered: false, reason: 'unreachable' },
              { node_id: 'node-3', answered: false, reason: 'timeout' },
              { node_id: 'node-4', answered: false, reason: 'unsupported' },
              { node_id: 'node-5', answered: false, reason: 'failed' },
              // A reason of a newer release, and a node that gave none.
              { node_id: 'node-6', answered: false, reason: 'quarantined' },
              { node_id: 'node-7', answered: false },
            ],
          }),
        ),
    });
    render(<SettingsScreen />);

    const words: Record<string, string> = {
      'node-2': 'did not answer: could not be reached',
      'node-3': 'did not answer: did not answer in time',
      'node-4': 'did not answer: runs an older release that cannot say',
      'node-5': 'did not answer: could not read its settings',
      'node-6': 'did not answer: quarantined',
      'node-7': 'did not answer: no reason was given',
    };
    for (const [nodeID, said] of Object.entries(words)) {
      const card = await cardOf(nodeID);
      expect(within(card).getByText(said)).toBeDefined();
      expect(within(card).queryAllByRole('table')).toHaveLength(0);
      expect(card.querySelector('details')).toBeNull();
      expect(card.querySelector('dl')).toBeNull();
    }

    // The cards are in the order of the answer.
    const cards = screen.getAllByRole('region').filter((region) => region.getAttribute('aria-label')?.startsWith('Node '));
    expect(cards.map((card) => card.getAttribute('aria-label'))).toEqual([
      'Node node-1',
      'Node node-2',
      'Node node-3',
      'Node node-4',
      'Node node-5',
      'Node node-6',
      'Node node-7',
    ]);
  });

  it('warns of the settings in which the nodes differ, and only when there are some', async () => {
    const api = fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);

    await screen.findByRole('region', { name: 'Cluster' });
    expect(screen.queryByRole('heading', { name: 'Mismatches' })).toBeNull();
    expect(screen.queryByRole('table', { name: 'Settings that differ between nodes' })).toBeNull();

    api.on({
      'GET /settings': () =>
        json(
          200,
          answer({
            mismatches: [
              {
                setting: 'erasure-parity',
                values: [
                  { node_id: 'node-1', value: '2' },
                  { node_id: 'node-2', value: '3' },
                ],
              },
              {
                setting: 'kms-key-id',
                values: [
                  { node_id: 'node-1', value: 'cactusfs-data' },
                  { node_id: 'node-2', value: '' },
                ],
              },
            ],
          }),
        ),
    });
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }));

    const panel = await screen.findByRole('region', { name: 'Mismatches' });
    expect(panel.textContent).toContain('Warning:');
    expect(panel.textContent).toContain('These settings must be the same on every node and are not:');

    const table = within(panel).getByRole('table', { name: 'Settings that differ between nodes' });
    expect(listedIn(table)).toEqual(['erasure-parity', 'kms-key-id']);
    // One row a node, under the heading row.
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(4);
    const [parityOne, parityTwo, keyOne, keyTwo] = rows as [HTMLElement, HTMLElement, HTMLElement, HTMLElement];
    expect(within(parityOne).getByText('node-1')).toBeDefined();
    expect(within(parityOne).getByText('2')).toBeDefined();
    expect(within(parityTwo).getByText('node-2')).toBeDefined();
    expect(within(parityTwo).getByText('3')).toBeDefined();
    expect(within(keyOne).getByText('cactusfs-data')).toBeDefined();
    expect(within(keyTwo).getByText('node-2')).toBeDefined();
    expect(within(keyTwo).getByText('not set')).toBeDefined();
  });

  it('opens the card of the answering node alone, and opens and closes all of them on request', async () => {
    fakeApi({
      'GET /settings': () => json(200, answer({ nodes: [answeringNode('node-1'), answeringNode('node-2')] })),
    });
    render(<SettingsScreen />);

    const first = await cardOf('node-1');
    const second = await cardOf('node-2');
    expect((first.querySelector('details') as HTMLDetailsElement).open).toBe(true);
    expect((second.querySelector('details') as HTMLDetailsElement).open).toBe(false);
    expect(within(first).getByRole('table', { name: 'Settings of node-1' })).toBeDefined();
    expect(within(second).queryAllByRole('table')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Expand all' }));

    expect((second.querySelector('details') as HTMLDetailsElement).open).toBe(true);
    expect(within(second).getByRole('table', { name: 'Settings of node-2' })).toBeDefined();
    expect(within(second).getByRole('table', { name: 'Drives of node-2' })).toBeDefined();

    fireEvent.click(screen.getByRole('button', { name: 'Collapse all' }));

    expect((first.querySelector('details') as HTMLDetailsElement).open).toBe(false);
    expect((second.querySelector('details') as HTMLDetailsElement).open).toBe(false);
    expect(screen.queryAllByRole('table', { name: /^Settings of / })).toHaveLength(0);
    expect(screen.getByRole('button', { name: 'Expand all' })).toBeDefined();
  });

  it('narrows the settings of every node by name, by variable and by flag, and to those that were overridden', async () => {
    fakeApi({
      'GET /settings': () => json(200, answer({ nodes: [answeringNode('node-1'), answeringNode('node-2')] })),
    });
    render(<SettingsScreen />);
    await cardOf('node-1');
    fireEvent.click(screen.getByRole('button', { name: 'Expand all' }));

    const tables = () => [
      screen.getByRole('table', { name: 'Settings of node-1' }),
      screen.getByRole('table', { name: 'Settings of node-2' }),
    ];
    const filter = screen.getByLabelText('Filter settings');

    // By name, whatever the case.
    fireEvent.change(filter, { target: { value: 'LOG-lev' } });
    for (const table of tables()) {
      expect(listedIn(table)).toEqual(['log-level']);
      expect(groupsIn(table)).toEqual(['Logging']);
    }

    // By the name of the environment variable: no name has an underscore.
    fireEvent.change(filter, { target: { value: 'cactusfs_s3' } });
    for (const table of tables()) {
      expect(listedIn(table)).toEqual(['s3-address']);
    }

    // By the flag as it is typed on a command line.
    fireEvent.change(filter, { target: { value: '--otlp' } });
    for (const table of tables()) {
      expect(listedIn(table)).toEqual(['otlp-endpoint']);
    }

    // Nothing matches: the grid gives way to a line that says so.
    fireEvent.change(filter, { target: { value: 'no-such-setting' } });
    expect(screen.queryAllByRole('table', { name: /^Settings of / })).toHaveLength(0);
    expect(screen.getAllByText('No setting of this node matches the filter.')).toHaveLength(2);
    // The drives are not what is filtered.
    expect(screen.getByRole('table', { name: 'Drives of node-1' })).toBeDefined();

    fireEvent.change(filter, { target: { value: '' } });
    for (const table of tables()) {
      expect(listedIn(table)).toHaveLength(SETTINGS.length);
    }

    // Only what did not come from the default - a source nobody knows is not the default.
    fireEvent.click(screen.getByLabelText('Show only overridden'));
    for (const table of tables()) {
      expect(listedIn(table)).toEqual(['s3-address', 'otlp-endpoint', 'kms-passphrase', 'future-thing']);
    }

    // Both at once.
    fireEvent.change(filter, { target: { value: 'kms' } });
    for (const table of tables()) {
      expect(listedIn(table)).toEqual(['kms-passphrase']);
    }
  });

  it('shows the problem of the API when the settings may not be read', async () => {
    fakeApi({ 'GET /settings': () => problem(403, 'Access denied', 'admin:GetSettings is not allowed.', 'req-9') });
    render(<SettingsScreen />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Access denied');
    expect(alert.textContent).toContain('admin:GetSettings is not allowed.');
    expect(alert.textContent).toContain('req-9');
    expect(screen.queryByRole('region', { name: 'Cluster' })).toBeNull();
    expect(screen.queryByLabelText('Filter settings')).toBeNull();
  });

  it('asks again on request', async () => {
    const api = fakeApi({ 'GET /settings': () => json(200, answer()) });
    render(<SettingsScreen />);
    await screen.findByRole('region', { name: 'Cluster' });
    expect(api.callsTo('GET', '/settings')).toHaveLength(1);

    api.on({
      'GET /settings': () => json(200, answer({ cluster: { ...answer().cluster, deployment_id: 'dep-2' } })),
    });
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }));

    await waitFor(() => {
      expect(api.callsTo('GET', '/settings')).toHaveLength(2);
    });
    expect(await screen.findByText('dep-2')).toBeDefined();
  });

  it('does not fail for a deployment that has nothing to tell', async () => {
    fakeApi({
      'GET /settings': () =>
        json(200, {
          answered_by: 'node-1',
          cluster: { deployment_id: '', metadata_level: 0, nodes: 0, voters: 0, erasure_sets: [] },
          nodes: [],
          mismatches: [],
        }),
    });
    render(<SettingsScreen />);

    const cluster = await screen.findByRole('region', { name: 'Cluster' });
    expect(statOf(cluster, 'Metadata leader')).toBe('None');
    expect(statOf(cluster, 'Deployment ID')).toBe('-');
    expect(within(cluster).getByText('There are no erasure sets.')).toBeDefined();
    expect(screen.getByText('No node is recorded.')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Expand all' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('does not fail for nodes, settings, drives and certificates of which only what is required is told', async () => {
    fakeApi({
      'GET /settings': () =>
        json(200, {
          answered_by: 'node-1',
          cluster: { deployment_id: 'dep-1', metadata_level: 4, nodes: 2, voters: 1, erasure_sets: [] },
          nodes: [
            // A node that answered and said nothing: a gateway in plaintext development mode, say.
            { node_id: 'node-1', answered: true },
            {
              node_id: 'node-2',
              answered: true,
              settings: [{ name: 'bare', kind: 'plain', scope: 'node', source: 'default', set: true }],
              drives: [{ id: 'd', directory: '/d', total_bytes: 0, free_bytes: 0, online: true }],
              metadata: { directory: '/m', total_bytes: 0, free_bytes: 0 },
              certificates: [
                {
                  use: 's3',
                  subject: 'CN=bare',
                  issuer: 'CN=bare CA',
                  dns_names: [],
                  ip_addresses: [],
                  not_before: 'not a date',
                  not_after: 'not a date',
                },
              ],
            },
          ],
          mismatches: [],
        }),
    });
    render(<SettingsScreen />);

    const first = within(await cardOf('node-1'));
    expect(first.getByText('answered')).toBeDefined();
    expect(first.getByText('This node has no drives.')).toBeDefined();
    expect(first.getByText('This node serves no certificate.')).toBeDefined();
    expect(first.getByText('This node listed no settings.')).toBeDefined();
    expect(first.queryAllByRole('table')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Expand all' }));

    const second = within(await cardOf('node-2'));
    const table = second.getByRole('table', { name: 'Settings of node-2' });
    // A setting without a group stands under a heading of its own.
    expect(groupsIn(table)).toEqual(['Other']);
    expect(within(rowOf(table, 'bare')).getByText('set')).toBeDefined();
    expect(within(rowOf(table, 'bare')).getAllByText('-')).toHaveLength(2);
    expect(second.getByRole('table', { name: 'Drives of node-2' })).toBeDefined();
    expect(second.getByText('/m')).toBeDefined();
    const certificate = rowOf(second.getByRole('table', { name: 'Certificates of node-2' }), 'CN=bare');
    expect(within(certificate).queryByText('expired')).toBeNull();
    expect(within(certificate).queryByText('expires soon')).toBeNull();
  });
});
