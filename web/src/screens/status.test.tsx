import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { App } from '../App';
import { fakeApi, json, problem, signedIn } from '../test/fakeApi';
import { ClusterScreen } from './ClusterScreen';

const TIME = '2026-10-01T00:00:00Z';

function record(sequence: number, extra: object = {}) {
  return {
    sequence,
    time: TIME,
    actor: 'alice',
    action: 'admin:PutUser',
    resource: `user/u${sequence}`,
    outcome: 'allowed',
    hash: `hash-${sequence}`,
    previous_hash: `hash-${sequence - 1}`,
    ...extra,
  };
}

function records(from: number, to: number) {
  return Array.from({ length: to - from + 1 }, (_, index) => record(from + index));
}

describe('cluster', () => {
  const cluster = {
    deployment_id: 'dep-1',
    answered_by: 'node-1',
    server: { version: '0.7.0', build: '412', commit: '3300281abcde' },
    leader: 'node-2',
    nodes: [
      { id: 'node-1', roles: ['gateway', 'storage'], labels: { zone: 'a' }, leader: false },
      { id: 'node-2', roles: ['meta'], labels: {}, leader: true, metadata_version: 3 },
    ],
    erasure_sets: [
      {
        index: 0,
        drives: [
          { id: 'drive-a', node_id: 'node-1', state: 'online' },
          { id: 'drive-b', node_id: 'node-2', state: 'offline' },
        ],
      },
    ],
    node_failures_tolerated: 1,
    drives_online: 1,
    drives_total: 2,
  };
  const maintenance = {
    node_id: 'node-1',
    started_at: TIME,
    scrub: { passes: 7, objects: 120, damaged: 2, shards_rebuilt: 4, unrecoverable: 0, bytes: 2048 },
    heal: { objects: 3, shards_rebuilt: 5, unrecoverable: 1 },
    collection: { passes: 9, shards: 400, removed: 11 },
  };

  it('shows nodes, erasure sets with drive states, and maintenance', async () => {
    fakeApi({
      'GET /cluster': () => json(200, cluster),
      'GET /cluster/maintenance': () => json(200, maintenance),
    });
    render(<ClusterScreen />);

    expect(await screen.findByText('dep-1')).toBeDefined();
    expect(screen.getByText('1 of 2')).toBeDefined();
    // The build of the node that answered, beside its name.
    expect(screen.getByText('node-1 (0.7.0, build 412)')).toBeDefined();
    const offline = within(screen.getByText('drive-b').closest('tr') as HTMLElement);
    expect(offline.getByText('offline')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Erasure set 0' })).toBeDefined();

    expect(await screen.findByRole('heading', { name: 'Scrubbing' })).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Healing' })).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Collection' })).toBeDefined();
    expect(screen.getByText('2.0 KiB')).toBeDefined();
  });

  it('shows each refusal where it belongs and loads again on request', async () => {
    const api = fakeApi({
      'GET /cluster': () => json(200, cluster),
      'GET /cluster/maintenance': () => problem(403, 'Access denied', 'admin:GetClusterStatus is not allowed.'),
    });
    render(<ClusterScreen />);

    expect((await screen.findByRole('alert')).textContent).toContain('Access denied');
    expect(screen.getByText('dep-1')).toBeDefined();

    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(api.callsTo('GET', '/cluster')).toHaveLength(2);
    });
  });
});

describe('audit log', () => {
  it('pages forward with next_after and back again', async () => {
    const api = signedIn({
      'GET /audit': (call) =>
        call.query.get('after') === '50'
          ? json(200, { records: records(51, 60) })
          : json(200, { records: records(1, 50), next_after: 50 }),
    });
    window.location.hash = '#/audit';
    render(<App />);

    expect(await screen.findByText('user/u1')).toBeDefined();
    const [first] = api.callsTo('GET', '/audit');
    expect(first?.query.get('after')).toBe('0');
    expect(first?.query.get('limit')).toBe('50');
    expect((screen.getByRole('button', { name: 'Older' }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: 'Newer' }));

    expect(await screen.findByText('user/u60')).toBeDefined();
    expect(window.location.hash).toBe('#/audit?after=50');
    expect(api.callsTo('GET', '/audit').at(-1)?.query.get('after')).toBe('50');
    expect(screen.getByRole('heading', { name: 'Records after number 50' })).toBeDefined();
    expect((screen.getByRole('button', { name: 'Newer' }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: 'Older' }));

    expect(await screen.findByText('user/u1')).toBeDefined();
    expect(api.callsTo('GET', '/audit').at(-1)?.query.get('after')).toBe('0');
  });

  it('reaches the newest page by walking to the end of the trail', async () => {
    const api = signedIn({
      'GET /audit': (call) => {
        const after = Number(call.query.get('after'));
        const limit = Number(call.query.get('limit'));
        const last = Math.min(after + limit, 1230);
        return json(200, { records: records(after + 1, last), next_after: last < 1230 ? last : undefined });
      },
    });
    window.location.hash = '#/audit';
    render(<App />);
    await screen.findByText('user/u1');

    fireEvent.click(screen.getByRole('button', { name: 'Newest' }));

    expect(await screen.findByText('user/u1230')).toBeDefined();
    expect(screen.getByText('user/u1181')).toBeDefined();
    expect(screen.queryByText('user/u1180')).toBeNull();
    expect(window.location.hash).toBe('#/audit?after=1180');
    const walked = api
      .callsTo('GET', '/audit')
      .filter((call) => call.query.get('limit') === '1000')
      .map((call) => call.query.get('after'));
    expect(walked).toEqual(['0', '1000']);
  });

  it('shows the details of a record', async () => {
    signedIn({
      'GET /audit': () =>
        json(200, {
          records: [
            record(1, {
              outcome: 'denied',
              access_key_id: 'AKIDEXAMPLE',
              source_ip: '192.0.2.7',
              request_id: 'req-77',
              details: { reason: 'explicit deny' },
            }),
          ],
        }),
    });
    window.location.hash = '#/audit';
    render(<App />);

    const row = within((await screen.findByText('user/u1')).closest('tr') as HTMLElement);
    expect(row.getByText('denied')).toBeDefined();
    expect(row.getByText('AKIDEXAMPLE')).toBeDefined();
    expect(row.getByText('192.0.2.7')).toBeDefined();
    expect(row.getByText('req-77')).toBeDefined();
    expect(row.getByText('reason')).toBeDefined();
    expect(row.getByText('explicit deny')).toBeDefined();
    expect(row.getByText('hash-1')).toBeDefined();
    expect(row.getByText('hash-0')).toBeDefined();
  });

  it('shows the problem of the API when the trail may not be read', async () => {
    signedIn({ 'GET /audit': () => problem(403, 'Access denied', 'admin:GetAuditLog is not allowed.', 'req-8') });
    window.location.hash = '#/audit';
    render(<App />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('admin:GetAuditLog is not allowed.');
    expect(alert.textContent).toContain('req-8');
  });
});
