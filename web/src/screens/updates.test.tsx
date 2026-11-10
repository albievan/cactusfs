import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { UNLOCK_ATTEMPTS } from '../api/client';
import { App } from '../App';
import { fakeApi, json, problem, refusal, signedIn, type Call, type Handler } from '../test/fakeApi';
import { UPGRADE_POLL_MS, UPGRADE_SILENCE_MS, UpdatesScreen } from './UpdatesScreen';

// The Updates screen: which releases there are, which of them may be
// installed and why the others may not, and installing one with the
// password typed again - judged by the server, never here. And the badge
// under the version line, which follows what the API says.

const CURRENT = { version: '0.7.0', build: '412', commit: '3300281abcde' };
const PASSWORD = 'correct horse battery staple';
/** What a source might put in a release's own text: never on screen. */
const SMUGGLED_BODY = '<img src=x onerror=alert(1)> release body text';

function view(extra: object = {}) {
  return {
    current: CURRENT,
    source_configured: true,
    channel: 'stable',
    checked_at: '2026-11-09T08:00:00Z',
    available: [],
    mode: 'binary',
    upgrade_available: true,
    instructions: [],
    ...extra,
  };
}

const RELEASES = [
  {
    version: '0.8.1',
    published: '2026-11-08T10:00:00Z',
    prerelease: false,
    notes_url: 'https://github.com/albievan/cactusfs/releases/tag/v0.8.1',
    allowed: true,
    // Not in the contract: a source's text that a node must never pass on, and if it did, is not shown.
    body: SMUGGLED_BODY,
  },
  { version: '0.8.0', published: '2026-11-01T10:00:00Z', prerelease: false, allowed: false, refusal: 'not-newest-patch' },
  { version: '0.7.1', prerelease: false, allowed: true },
  { version: '0.9.0-rc.1', prerelease: true, allowed: false, refusal: 'prerelease-on-stable' },
  { version: '0.9.0', prerelease: false, allowed: false, refusal: 'skipped-minor' },
  { version: '1.0.0', prerelease: false, allowed: false, refusal: 'major' },
  { version: '0.7.2', prerelease: false, allowed: false, refusal: 'unknown-release' },
];

const IDLE = { state: 'idle', nodes: [] };

function progress(state: string, extra: object = {}) {
  return {
    state,
    target: '0.7.1',
    started_by: 'user/alice',
    started_at: '2026-11-09T09:00:00Z',
    nodes: [{ id: 'node-1', version: '0.7.0', build: '412', state: 'upgrading' }],
    ...extra,
  };
}

/** The fake API of a node with checks on, and the releases above. */
function updatesApi(routes: Record<string, Handler> = {}) {
  return fakeApi({
    'GET /updates': () => json(200, view({ available: RELEASES, next_step: '0.8.1' })),
    'GET /upgrade': () => json(200, IDLE),
    ...routes,
  });
}

/** Lets this much time pass on the test's clock. */
async function pass(milliseconds: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(milliseconds);
  });
}

/** The row of a release in the grid. */
async function rowOf(version: string): Promise<HTMLElement> {
  const grid = await screen.findByRole('table', { name: 'Newer releases' });
  const header = within(grid).getByRole('rowheader', {
    name: (name) => name === version || name === `${version} pre-release`,
  });
  const row = header.closest('tr');
  if (row === null) {
    throw new Error(`no row for ${version}`);
  }
  return row;
}

/** Opens "Upgrade to X?", types the password and sends it. */
async function upgradeTo(version: string) {
  fireEvent.click(await screen.findByRole('button', { name: `Upgrade to ${version}` }));
  const dialog = await screen.findByRole('dialog', { name: `Upgrade to ${version}?` });
  const field = within(dialog).getByLabelText('Your password');
  fireEvent.change(field, { target: { value: PASSWORD } });
  fireEvent.click(within(dialog).getByRole('button', { name: `Upgrade to ${version}` }));
  return { dialog, field: field as HTMLInputElement };
}

/** Whether the password is anywhere in the page: its markup or the value of a field. */
function passwordInPage(): boolean {
  const fields = Array.from(document.querySelectorAll('input'));
  return document.body.innerHTML.includes(PASSWORD) || fields.some((input) => input.value === PASSWORD);
}

afterEach(() => {
  vi.useRealTimers();
});

describe('Updates: what the node knows', () => {
  it('says that checks are off and names the variable that turns them on, and asks nothing', async () => {
    const api = updatesApi({
      'GET /updates': () => json(200, view({ source_configured: false, checked_at: undefined, mode: 'off', upgrade_available: false })),
    });
    render(<UpdatesScreen passwordSession />);

    const panel = await screen.findByRole('region', { name: 'Checks for updates' });
    expect(panel.textContent).toContain('Update checks are off');
    expect(within(panel).getByText('CACTUSFS_UPDATE_SOURCE').tagName).toBe('CODE');
    expect(screen.queryByRole('button', { name: /Check now/ })).toBeNull();
    expect(screen.queryByRole('table', { name: 'Newer releases' })).toBeNull();
    expect(api.callsTo('POST', '/updates/check')).toHaveLength(0);
  });

  it('shows the running build, and the releases with what may be installed and why not', async () => {
    updatesApi();
    render(<UpdatesScreen passwordSession />);

    const build = await screen.findByRole('region', { name: 'This release' });
    expect(build.textContent).toContain('0.7.0');
    expect(build.textContent).toContain('build 412');
    expect(build.textContent).toContain('3300281abcde');
    expect(build.textContent).toContain('Binary');

    const allowed = await rowOf('0.8.1');
    expect(allowed.textContent).toContain('may be installed');
    const link = within(allowed).getByRole('link', { name: /Release notes/ });
    expect(link.getAttribute('href')).toBe('https://github.com/albievan/cactusfs/releases/tag/v0.8.1');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');
    expect(within(allowed).getByRole('button', { name: 'Upgrade to 0.8.1' })).toBeDefined();

    // A release without notes has no link.
    const patch = await rowOf('0.7.1');
    expect(within(patch).queryByRole('link')).toBeNull();
    expect(within(patch).getByRole('button', { name: 'Upgrade to 0.7.1' })).toBeDefined();

    const reasons: [string, string][] = [
      ['0.8.0', 'A newer patch of its minor version is listed'],
      ['0.9.0-rc.1', 'pre-release, and the channel is stable'],
      ['0.9.0', 'more than one minor version ahead'],
      ['1.0.0', 'another major version'],
      ['0.7.2', 'not listed when the node last checked'],
    ];
    for (const [version, reason] of reasons) {
      const row = await rowOf(version);
      expect(row.textContent).toContain('not offered');
      expect(row.textContent).toContain(reason);
      expect(within(row).queryByRole('button')).toBeNull();
    }
    expect((await rowOf('0.9.0-rc.1')).textContent).toContain('pre-release');
    // Nothing of a release's own text is drawn.
    expect(document.body.innerHTML).not.toContain('release body text');
    expect(document.querySelector('img')).toBeNull();
  });

  it.each([
    ['equal', 'It is the release that runs now'],
    ['older', 'nothing is downgraded from the console'],
  ])('words the refusal %s', async (reason, words) => {
    updatesApi({
      'GET /updates': () => json(200, view({ available: [{ version: '0.6.0', prerelease: false, allowed: false, refusal: reason }] })),
    });
    render(<UpdatesScreen passwordSession />);
    expect((await rowOf('0.6.0')).textContent).toContain(words);
  });

  it.each([
    ['unreachable', 'the update source could not be reached'],
    ['rate-limited', 'the update source limited the rate of requests'],
    ['refused', 'the update source refused the request'],
    ['invalid-answer', 'the update source answered with something the node does not read'],
    ['timeout', 'the update source did not answer in time'],
  ])('words the last error %s plainly', async (lastError, words) => {
    updatesApi({ 'GET /updates': () => json(200, view({ last_error: lastError })) });
    render(<UpdatesScreen passwordSession />);
    const panel = await screen.findByRole('region', { name: 'Checks for updates' });
    expect(panel.textContent).toContain(`Could not be read: ${words}`);
  });

  it('offers no upgrade where installing is not available, and gives the commands to copy', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    const instructions = [
      'curl -LO https://github.com/albievan/cactusfs/releases/download/v0.8.1/cactusfs-linux-amd64',
      'sha256sum -c SHA256SUMS --ignore-missing',
    ];
    updatesApi({
      'GET /updates': () =>
        json(
          200,
          view({
            available: RELEASES,
            next_step: '0.8.1',
            mode: 'off',
            upgrade_available: false,
            unavailable_reason: 'upgrade-mode is off.',
            instructions,
          }),
        ),
    });
    render(<UpdatesScreen passwordSession />);

    await rowOf('0.8.1');
    expect(screen.queryByRole('button', { name: /Upgrade to/ })).toBeNull();
    const install = screen.getByRole('region', { name: 'Installing' });
    expect(install.textContent).toContain('Installing from the console is not available here.');
    expect(install.textContent).toContain('upgrade-mode is off.');
    const pre = install.querySelector('pre');
    expect(pre?.textContent).toBe(instructions.join('\n'));
    expect(within(install).getByText('To install 0.8.1 yourself')).toBeDefined();

    fireEvent.click(within(install).getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith(instructions.join('\n'));
    expect(await within(install).findByText('Copied.')).toBeDefined();
  });

  it('offers no upgrade to a caller that is not a password session', async () => {
    updatesApi();
    render(<UpdatesScreen />);
    await rowOf('0.8.1');
    expect(screen.queryByRole('button', { name: /Upgrade to/ })).toBeNull();
    expect(screen.getByRole('region', { name: 'Installing' }).textContent).toContain('an API key cannot');
  });

  it('says plainly why the node did not answer', async () => {
    updatesApi({ 'GET /updates': () => problem(404, 'Not found') });
    render(<UpdatesScreen passwordSession />);
    expect((await screen.findByRole('alert')).textContent).toContain('it runs an earlier release');
  });
});

describe('Updates: check now', () => {
  it('asks the source through the node and shows what it found', async () => {
    let found = false;
    const api = updatesApi({
      'GET /updates': () => json(200, view()),
      'POST /updates/check': () => {
        found = true;
        return json(200, view({ available: RELEASES, next_step: '0.8.1', checked_at: '2026-11-09T09:30:00Z' }));
      },
    });
    render(<UpdatesScreen passwordSession />);

    expect(await screen.findByText('There is no newer release.')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Check now' }));
    expect(await screen.findByText('Checked: 7 newer releases are listed.')).toBeDefined();
    expect(found).toBe(true);
    await rowOf('0.8.1');

    const [call] = api.callsTo('POST', '/updates/check');
    expect(call?.headers.get('Content-Type')).toBe('application/json');
    expect(call?.body).toBe('');
  });

  it('says that checks are off when the node answers 409', async () => {
    updatesApi({ 'POST /updates/check': () => refusal(409, 'UpdatesOff', 'Updates are off') });
    render(<UpdatesScreen passwordSession />);
    fireEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Update checks are off: CACTUSFS_UPDATE_SOURCE is not set');
    expect(alert.textContent).toContain('Nothing was asked.');
  });

  it('says when to try again when the node checked less than a minute ago', async () => {
    updatesApi({
      'POST /updates/check': () => refusal(429, 'TooManyRequests', 'Too many requests', { 'Retry-After': '42' }),
    });
    render(<UpdatesScreen passwordSession />);
    fireEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    expect((await screen.findByRole('alert')).textContent).toContain('try again in 42 s');
  });
});

describe('Updates: upgrade', () => {
  it('asks for the password in a modal dialog that keeps the keyboard and closes with Escape', async () => {
    const api = updatesApi();
    render(<UpdatesScreen passwordSession />);

    const opener = await screen.findByRole('button', { name: 'Upgrade to 0.8.1' });
    fireEvent.click(opener);
    const dialog = await screen.findByRole('dialog', { name: 'Upgrade to 0.8.1?' });
    expect(dialog.getAttribute('aria-modal')).toBe('true');
    const field = within(dialog).getByLabelText('Your password');
    expect(field.getAttribute('type')).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('current-password');
    await waitFor(() => {
      expect(document.activeElement).toBe(field);
    });
    // What will happen is said.
    expect(dialog.textContent).toContain('from 0.7.0 to 0.8.1');
    expect(dialog.textContent).toContain('The node answering you restarts');
    expect(dialog.textContent).toContain('Going back is not possible once 0.8.1 has raised the metadata schema');
    // The rest of the page is out of reach.
    expect(opener.closest('[inert]')).not.toBeNull();

    // The keyboard goes round the dialog.
    const cancel = within(dialog).getByRole('button', { name: 'Cancel' });
    cancel.focus();
    fireEvent.keyDown(cancel, { key: 'Tab' });
    expect(document.activeElement).toBe(field);

    fireEvent.keyDown(field, { key: 'Escape' });
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
    await waitFor(() => {
      expect(document.activeElement).toBe(opener);
    });
    expect(opener.closest('[inert]')).toBeNull();
    expect(api.callsTo('POST', '/upgrade')).toHaveLength(0);
  });

  it('sends the version and the password, and follows the upgrade that began', async () => {
    const api = updatesApi({
      'POST /upgrade': () => json(202, progress('verifying')),
      'GET /upgrade': () => json(200, IDLE),
    });
    render(<UpdatesScreen passwordSession />);

    const { field } = await upgradeTo('0.7.1');
    // Out of the field the moment it was sent.
    expect(field.value).toBe('');
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
    const panel = await screen.findByRole('region', { name: 'Upgrade' });
    expect(panel.textContent).toContain('Verifying the release');
    expect(panel.textContent).toContain('user/alice');
    const nodes = within(panel).getByRole('table', { name: 'Nodes of the upgrade' });
    expect(within(nodes).getByRole('rowheader', { name: 'node-1' })).toBeDefined();
    expect(nodes.textContent).toContain('upgrading');
    await waitFor(() => {
      expect(document.activeElement).toBe(within(panel).getByRole('heading', { name: 'Upgrade' }));
    });
    // Nothing more is offered while it runs.
    expect(screen.queryByRole('button', { name: /Upgrade to/ })).toBeNull();

    const [call] = api.callsTo('POST', '/upgrade');
    expect(JSON.parse(call?.body ?? '{}')).toEqual({ version: '0.7.1', password: PASSWORD });
    expect(passwordInPage()).toBe(false);
  });

  it.each([
    [403, 'AccessDenied', {}, 'The password was not accepted, or you are not allowed to install releases.'],
    [401, 'Unauthenticated', {}, 'Your session has ended'],
    [423, 'SessionLocked', {}, 'Your session is locked'],
    [409, 'UpgradeRunning', {}, 'An upgrade is running already'],
    [409, 'UpgradeNotAllowed', {}, 'This release may not be installed from the running one'],
    [409, 'UpdatesOff', {}, 'Update checks are off on the node that answered'],
    [412, 'ReleaseNotVerified', {}, 'The release could not be verified; nothing was installed.'],
    [429, 'TooManyAttempts', { 'Retry-After': '300' }, 'Too many attempts - try again in 300 s.'],
    [501, 'UpgradeUnavailable', {}, 'Installing from the console is not available on this deployment'],
  ])('words a %i %s, keeps the dialog and forgets the password', async (status, code, headers, words) => {
    const api = updatesApi({ 'POST /upgrade': () => refusal(status, code, 'Refused', headers) });
    render(<UpdatesScreen passwordSession />);

    const { dialog, field } = await upgradeTo('0.8.1');
    const alert = await within(dialog).findByRole('alert');
    expect(alert.textContent).toContain(words);
    expect(alert.textContent).toMatch(/nothing (was|more was) (installed|started)/i);
    expect(field.value).toBe('');
    expect(passwordInPage()).toBe(false);
    expect(api.callsTo('POST', '/upgrade')).toHaveLength(1);
    // Nothing is retried, and nothing else asks for the password.
    expect(api.callsTo('POST', '/session/unlock')).toHaveLength(0);
  });

  it('asks for the progress when an upgrade is running already', async () => {
    let running = false;
    updatesApi({
      'POST /upgrade': () => {
        running = true;
        return refusal(409, 'UpgradeRunning', 'Upgrade running');
      },
      'GET /upgrade': () => json(200, running ? progress('running') : IDLE),
    });
    render(<UpdatesScreen passwordSession />);
    await upgradeTo('0.8.1');
    const panel = await screen.findByRole('region', { name: 'Upgrade' });
    expect(panel.textContent).toContain('Upgrading the nodes');
  });

  it('follows an upgrade that was under way when the screen was opened', async () => {
    updatesApi({ 'GET /upgrade': () => json(200, progress('failed', { nodes: [{ id: 'node-2', state: 'restored', reason: 'not ready in time' }] })) });
    render(<UpdatesScreen passwordSession />);
    const panel = await screen.findByRole('region', { name: 'Upgrade' });
    expect(panel.textContent).toContain('The upgrade stopped');
    expect(panel.textContent).toContain('not ready in time');
    expect(panel.textContent).toContain('restored');
  });
});

describe('Updates: through the restart', () => {
  /** Answers that change as the test says, and count what was asked. */
  function restartingApi() {
    const state = { upgrade: 'before' as 'before' | 'started' | 'down' | 'gateway' | 'back', version: '0.7.0' };
    const api = updatesApi({
      'POST /upgrade': () => {
        state.upgrade = 'started';
        return json(202, progress('running'));
      },
      'GET /upgrade': () => {
        switch (state.upgrade) {
          case 'before':
            return json(200, IDLE);
          case 'down':
            throw new TypeError('Failed to fetch');
          case 'gateway':
            return problem(503, 'Service Unavailable');
          case 'back':
            return json(200, IDLE);
          default:
            return json(200, progress('running'));
        }
      },
      'GET /updates': () => json(200, view({ current: { ...CURRENT, version: state.version }, available: RELEASES })),
    });
    return { api, state };
  }

  it('says "restarting" while the node does not answer, and "Upgraded to X" once it runs X', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { api, state } = restartingApi();
    render(<UpdatesScreen passwordSession />);
    await upgradeTo('0.7.1');
    const panel = await screen.findByRole('region', { name: 'Upgrade' });
    expect(panel.textContent).toContain('Upgrading the nodes');

    state.upgrade = 'down';
    await pass(UPGRADE_POLL_MS);
    await waitFor(() => {
      expect(panel.textContent).toContain('Restarting…');
    });
    expect(screen.queryByRole('alert')).toBeNull();

    state.upgrade = 'gateway';
    await pass(UPGRADE_POLL_MS);
    expect(panel.textContent).toContain('Restarting…');
    expect(screen.queryByRole('alert')).toBeNull();

    // The new process starts idle, with the new version.
    state.upgrade = 'back';
    state.version = '0.7.1';
    await pass(UPGRADE_POLL_MS);
    await waitFor(() => {
      expect(panel.textContent).toContain('Upgraded to 0.7.1.');
    });
    expect(screen.getByRole('region', { name: 'This release' }).textContent).toContain('0.7.1');

    // And asks no more.
    const asked = api.callsTo('GET', '/upgrade').length;
    await pass(UPGRADE_POLL_MS * 3);
    expect(api.callsTo('GET', '/upgrade')).toHaveLength(asked);
  });

  it('keeps asking while an idle node still runs the old release', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { api, state } = restartingApi();
    render(<UpdatesScreen passwordSession />);
    await upgradeTo('0.7.1');
    await screen.findByRole('region', { name: 'Upgrade' });

    state.upgrade = 'back';
    await pass(UPGRADE_POLL_MS);
    await waitFor(() => {
      expect(api.callsTo('GET', '/updates').length).toBeGreaterThan(1);
    });
    expect(screen.queryByText(/Upgraded to/)).toBeNull();
    const asked = api.callsTo('GET', '/upgrade').length;
    await pass(UPGRADE_POLL_MS);
    expect(api.callsTo('GET', '/upgrade').length).toBeGreaterThan(asked);
  });

  it('says so after ten minutes without an answer, and can wait again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { api, state } = restartingApi();
    render(<UpdatesScreen passwordSession />);
    await upgradeTo('0.7.1');
    const panel = await screen.findByRole('region', { name: 'Upgrade' });

    state.upgrade = 'gateway';
    await pass(UPGRADE_SILENCE_MS + UPGRADE_POLL_MS);
    await waitFor(() => {
      expect(panel.textContent).toContain('The node has not answered for 10 minutes.');
    });
    const asked = api.callsTo('GET', '/upgrade').length;
    await pass(UPGRADE_POLL_MS * 3);
    expect(api.callsTo('GET', '/upgrade')).toHaveLength(asked);

    state.upgrade = 'started';
    fireEvent.click(within(panel).getByRole('button', { name: 'Keep waiting' }));
    await pass(UPGRADE_POLL_MS);
    await waitFor(() => {
      expect(api.callsTo('GET', '/upgrade').length).toBeGreaterThan(asked);
    });
    expect(panel.textContent).not.toContain('has not answered');
  });

  it('stops asking when the screen goes away', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { api } = restartingApi();
    const { unmount } = render(<UpdatesScreen passwordSession />);
    await upgradeTo('0.7.1');
    await screen.findByRole('region', { name: 'Upgrade' });
    await pass(UPGRADE_POLL_MS);
    unmount();
    const asked = api.callsTo('GET', '/upgrade').length;
    await pass(UPGRADE_POLL_MS * 5);
    expect(api.callsTo('GET', '/upgrade')).toHaveLength(asked);
  });
});

describe('the "update available" badge', () => {
  function badge() {
    return screen.queryByRole('link', { name: /update available/ });
  }

  /** Signed in, on the buckets screen, with GET /updates answered so. */
  async function shown(answer: Handler): Promise<Call[]> {
    const api = signedIn({ 'GET /buckets': () => json(200, []), 'GET /updates': answer });
    render(<App />);
    await screen.findByRole('heading', { name: 'Buckets' });
    await waitFor(() => {
      expect(api.callsTo('GET', '/updates')).toHaveLength(1);
    });
    return api.calls;
  }

  it('is under the version line when a release may be installed, and leads to Updates', async () => {
    await shown(() => json(200, view({ available: RELEASES, next_step: '0.8.1' })));
    const link = await screen.findByRole('link', { name: /update available/ });
    expect(link.getAttribute('href')).toBe('#/updates');
    expect(link.previousElementSibling?.className).toBe('sidebar-version');
    expect(screen.getByRole('link', { name: 'Updates' }).getAttribute('href')).toBe('#/updates');
  });

  it.each([
    ['nothing may be installed', () => json(200, view({ available: RELEASES.filter((release) => !release.allowed) }))],
    ['checks are off', () => json(200, view({ source_configured: false }))],
    ['the caller may not see updates (403)', () => problem(403, 'Access denied')],
    ['the node cannot say (501)', () => problem(501, 'Not implemented')],
    ['the node is of an earlier release (404)', () => problem(404, 'Not found')],
  ])('is not there when %s, and nothing is said', async (_, answer) => {
    await shown(answer);
    // Given the answer time to be drawn.
    await act(async () => {
      await Promise.resolve();
    });
    expect(badge()).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('follows a check made on the Updates screen', async () => {
    window.location.hash = '#/updates';
    const api = signedIn({
      'GET /updates': () => json(200, view()),
      'GET /upgrade': () => json(200, IDLE),
      'POST /updates/check': () => json(200, view({ available: RELEASES, next_step: '0.8.1' })),
    });
    render(<App />);
    fireEvent.click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByRole('link', { name: /update available/ })).toBeDefined();
    // Asked once by the console and once by the screen; the check told the badge.
    expect(api.callsTo('GET', '/updates')).toHaveLength(2);
  });
});

describe('Updates: a session ended by wrong passwords', () => {
  /**
   * The console on the Updates screen, whose `POST /upgrade` refuses the
   * password `wrong` times (403) and then answers 401: the API ended the
   * session. It counts those passwords with the unlocks' and answers
   * `Unauthenticated` either way: the console's count says why.
   */
  async function refusedThenEnded(wrong: number) {
    window.location.hash = '#/updates';
    let refused = 0;
    let ended = false;
    signedIn({
      'GET /session': () =>
        ended ? problem(401, 'Not signed in') : json(200, { kind: 'user', via: 'session', user: 'alice', server: CURRENT }),
      'GET /updates': () => json(200, view({ available: RELEASES, next_step: '0.8.1' })),
      'GET /upgrade': () => json(200, IDLE),
      'POST /upgrade': () => {
        if (refused < wrong) {
          refused += 1;
          return refusal(403, 'AccessDenied', 'Access denied');
        }
        ended = true;
        return refusal(401, 'Unauthenticated', 'Authentication required');
      },
    });
    render(<App />);
    const { dialog } = await upgradeTo('0.8.1');
    for (let attempt = 1; attempt <= wrong; attempt += 1) {
      await waitFor(() => {
        expect(refused).toBe(attempt);
      });
      const field = await within(dialog).findByLabelText('Your password');
      await waitFor(() => {
        expect((within(dialog).getByRole('button', { name: 'Upgrade to 0.8.1' }) as HTMLButtonElement).disabled).toBe(false);
      });
      fireEvent.change(field, { target: { value: PASSWORD } });
      fireEvent.click(within(dialog).getByRole('button', { name: 'Upgrade to 0.8.1' }));
    }
    await screen.findByRole('heading', { name: 'Sign in' });
  }

  it('says that the password was wrong too many times, as after unlocking', async () => {
    await refusedThenEnded(UNLOCK_ATTEMPTS - 1);
    expect(await screen.findByText('You were signed out: the password was wrong too many times.')).toBeDefined();
    expect(passwordInPage()).toBe(false);
  });

  it('gives no such reason when fewer passwords were wrong', async () => {
    await refusedThenEnded(1);
    expect(await screen.findByText('You were signed out.')).toBeDefined();
    expect(screen.queryByText(/wrong too many times/)).toBeNull();
  });
});
