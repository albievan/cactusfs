import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { App } from './App';
import { describeBuild } from './Layout';
import { CSRF_TOKEN, fakeApi, json, noContent, problem, SERVER, signedIn } from './test/fakeApi';

function typeInto(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

describe('session', () => {
  it('shows the sign-in screen with the tagline logo when nobody is signed in', async () => {
    fakeApi({ 'GET /session': () => problem(401, 'Not signed in') });
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(screen.getByAltText('CactusFS').getAttribute('src')).toContain('cactusfs-logo-tagline.svg');
  });

  it('says what the store is, links to the documentation it serves, and can show the password', async () => {
    fakeApi({ 'GET /session': () => problem(401, 'Not signed in') });
    render(<App />);

    await screen.findByRole('heading', { name: 'Sign in' });
    expect(screen.getByRole('complementary', { name: 'About CactusFS' }).textContent).toContain('S3-compatible');
    // Nothing leaves the deployment: both links are served by it.
    expect(screen.getByRole('link', { name: 'API documentation' }).getAttribute('href')).toBe('/docs');
    expect(screen.getByRole('link', { name: 'API contract' }).getAttribute('href')).toBe('/api/v1/openapi.yaml');

    const password = screen.getByLabelText('Password');
    expect(password.getAttribute('type')).toBe('password');
    fireEvent.click(screen.getByLabelText('Show what I type'));
    expect(password.getAttribute('type')).toBe('text');
    fireEvent.click(screen.getByLabelText('Show what I type'));
    expect(password.getAttribute('type')).toBe('password');
  });

  it('signs in, keeps the CSRF token for the tab, and shows the console', async () => {
    let signedInNow = false;
    const api = fakeApi({
      'GET /session': () =>
        signedInNow
          ? json(200, { kind: 'user', via: 'session', user: 'alice', server: SERVER })
          : problem(401, 'Not signed in'),
      'POST /session': () => {
        signedInNow = true;
        return json(200, {
          user: 'alice',
          csrf_token: 'fresh-token',
          expires_at: '2026-10-08T12:00:00Z',
          server: SERVER,
        });
      },
      'GET /buckets': () => json(200, []),
    });
    render(<App />);

    await screen.findByRole('heading', { name: 'Sign in' });
    typeInto('User name', 'alice');
    typeInto('Password', 'correct horse battery staple');
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));

    expect(await screen.findByText('Signed in as alice')).toBeDefined();
    expect(await screen.findByRole('heading', { name: 'Buckets' })).toBeDefined();
    expect(screen.getByAltText('CactusFS').getAttribute('src')).toContain('cactusfs-logo-reversed.svg');

    const [signIn] = api.callsTo('POST', '/session');
    expect(JSON.parse(signIn?.body ?? '')).toEqual({
      username: 'alice',
      password: 'correct horse battery staple',
    });
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBe('fresh-token');
  });

  it('shows why signing in was refused', async () => {
    fakeApi({
      'GET /session': () => problem(401, 'Not signed in'),
      'POST /session': () =>
        problem(401, 'Sign-in failed', 'The user name or the password is wrong.', 'req-123'),
    });
    render(<App />);

    await screen.findByRole('heading', { name: 'Sign in' });
    typeInto('User name', 'alice');
    typeInto('Password', 'wrong');
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Sign-in failed');
    expect(alert.textContent).toContain('The user name or the password is wrong.');
    expect(alert.textContent).toContain('req-123');
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
  });

  it('goes back to the sign-in screen when any request is answered 401', async () => {
    signedIn({ 'GET /buckets': () => problem(401, 'The session has expired') });
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
  });

  it('asks to sign in again when the session has no CSRF token in this tab', async () => {
    fakeApi({ 'GET /session': () => json(200, { kind: 'user', via: 'session', user: 'alice', server: SERVER }) });
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
  });

  it('has the sections down the left as links with icons, and folds them to icons alone', async () => {
    signedIn({ 'GET /buckets': () => json(200, []) });
    render(<App />);

    const sections = within(await screen.findByRole('navigation', { name: 'Sections' }));
    const links = sections.getAllByRole('link');
    expect(links.map((link) => link.textContent.trim())).toEqual([
      'Buckets',
      'Users',
      'Groups',
      'Roles',
      'Policies',
      'Cluster',
      'Audit log',
    ]);
    for (const link of links) {
      expect(link.querySelector('i.bi')?.getAttribute('aria-hidden')).toBe('true');
    }
    expect(sections.getByRole('link', { name: 'Buckets' }).getAttribute('aria-current')).toBe('page');

    // Folded, the names are still there for whoever has the page read, the
    // mark stands in for the logo, and the choice is remembered.
    const fold = screen.getByRole('button', { name: 'Show icons only' });
    expect(fold.getAttribute('aria-pressed')).toBe('false');
    fireEvent.click(fold);
    expect(fold.getAttribute('aria-pressed')).toBe('true');
    expect(sections.getByRole('link', { name: 'Users' }).getAttribute('title')).toBe('Users');
    expect(screen.getByAltText('CactusFS').getAttribute('src')).toContain('cactusfs-mark.svg');
    expect(window.localStorage.getItem('cactusfs.nav.folded')).toBe('1');

    fireEvent.click(fold);
    expect(fold.getAttribute('aria-pressed')).toBe('false');
    expect(window.localStorage.getItem('cactusfs.nav.folded')).toBe('0');
  });

  it('says the version and the build under the logo, and the commit in the hint', async () => {
    signedIn({ 'GET /buckets': () => json(200, []) });
    render(<App />);

    const line = await screen.findByText('v0.7.0 \u00b7 build 412');
    expect(line.getAttribute('title')).toBe('Version 0.7.0, build 412, commit 3300281abcde');
    expect(line.className).toBe('sidebar-version');
    // Right after the logo, inside the navigation's head.
    const logo = screen.getByAltText('CactusFS');
    expect(logo.closest('.sidebar-head')).toBe(line.parentElement);
    expect(logo.closest('a')?.nextElementSibling).toBe(line);
  });

  it('calls a build without a number a local build', async () => {
    signedIn({
      'GET /session': () =>
        json(200, { kind: 'user', via: 'session', user: 'alice', server: { version: '0.7.0', build: '0' } }),
      'GET /buckets': () => json(200, []),
    });
    render(<App />);

    const line = await screen.findByText('v0.7.0 \u00b7 local build');
    expect(line.getAttribute('title')).toBe('Version 0.7.0, local build');
  });

  it('describes a build as the API reported it', () => {
    expect(describeBuild({ version: '0.7.0', build: '412', commit: '3300281abcde-dirty' })).toEqual({
      line: 'v0.7.0 \u00b7 build 412',
      hint: 'Version 0.7.0, build 412, commit 3300281abcde-dirty',
    });
    expect(describeBuild({ version: '1.0.0', build: '0', commit: 'abcdef123456' })).toEqual({
      line: 'v1.0.0 \u00b7 local build',
      hint: 'Version 1.0.0, local build, commit abcdef123456',
    });
    expect(describeBuild({ version: '1.0.0', build: '7' })).toEqual({
      line: 'v1.0.0 \u00b7 build 7',
      hint: 'Version 1.0.0, build 7',
    });
  });

  it('shows no version line when a node that does not say its build answers', async () => {
    signedIn({
      'GET /session': () => json(200, { kind: 'user', via: 'session', user: 'alice' }),
      'GET /buckets': () => json(200, []),
    });
    render(<App />);

    expect(await screen.findByText('Signed in as alice')).toBeDefined();
    expect(document.querySelector('.sidebar-version')).toBeNull();
  });

  it('does not say the version before anybody has signed in', async () => {
    fakeApi({ 'GET /session': () => problem(401, 'Not signed in') });
    render(<App />);

    await screen.findByRole('heading', { name: 'Sign in' });
    expect(document.querySelector('.sidebar-version')).toBeNull();
    expect(document.body.textContent).not.toMatch(/v\d+\.\d+\.\d+|local build|build \d/);
  });

  it('signs out with the CSRF token and forgets it', async () => {
    const api = signedIn({
      'GET /buckets': () => json(200, []),
      'DELETE /session': () => noContent(),
    });
    render(<App />);

    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }));

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
    const [signOut] = api.callsTo('DELETE', '/session');
    expect(signOut?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    expect(window.sessionStorage.getItem('cactusfs.csrf')).toBeNull();
  });

  it('says so when the API cannot be reached, and tries again on request', async () => {
    const api = fakeApi({ 'GET /session': () => problem(503, 'No metadata leader', 'Try again shortly.') });
    render(<App />);

    expect((await screen.findByRole('alert')).textContent).toContain('No metadata leader');

    api.on({ 'GET /session': () => problem(401, 'Not signed in') });
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
  });
});

describe('navigation', () => {
  it('marks the section of the current screen and follows the address', async () => {
    signedIn({
      'GET /buckets': () => json(200, []),
      'GET /groups': () => json(200, [{ name: 'ops', roles: ['read-only'], created_at: '2026-10-01T00:00:00Z' }]),
    });
    render(<App />);

    const buckets = await screen.findByRole('link', { name: 'Buckets' });
    expect(buckets.getAttribute('aria-current')).toBe('page');

    window.location.hash = '#/groups';

    expect(await screen.findByRole('heading', { name: 'Groups' })).toBeDefined();
    expect(await screen.findByText('ops')).toBeDefined();
    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'Groups' }).getAttribute('aria-current')).toBe('page');
    });
  });

  it('has a search field in the navigation that goes to the search screen with what was typed', async () => {
    const api = signedIn({
      'GET /buckets': () => json(200, []),
      'GET /search': () => json(200, { results: [], match: 'substring', examined: 0, complete: true }),
    });
    render(<App />);

    const search = within(await screen.findByRole('search', { name: 'Search' }));
    const field = search.getByLabelText('Search buckets and objects');
    const button = search.getByRole('button', { name: 'Search' });
    expect(button.getAttribute('title')).toContain('rx:');
    expect(button.querySelector('i.bi')?.getAttribute('aria-hidden')).toBe('true');

    // Nothing typed, nothing asked.
    fireEvent.click(button);
    fireEvent.change(field, { target: { value: '   ' } });
    fireEvent.click(button);
    expect(window.location.hash).toBe('');

    fireEvent.change(field, { target: { value: ' rx:^2026/a&b #1$ ' } });
    fireEvent.click(button);

    // What was typed is in the address, encoded, and reaches the API as typed.
    await waitFor(() => {
      expect(window.location.hash).toBe('#/search?q=rx%3A%5E2026%2Fa%26b%20%231%24');
    });
    expect(await screen.findByRole('heading', { name: 'Search' })).toBeDefined();
    await waitFor(() => {
      expect(api.callsTo('GET', '/search')).toHaveLength(1);
    });
    const [asked] = api.callsTo('GET', '/search');
    expect(asked?.query.get('q')).toBe('rx:^2026/a&b #1$');
    expect(asked?.query.has('continuation')).toBe(false);
    expect(await screen.findByText(/Nothing found\./)).toBeDefined();
    // No section is the current one on the search screen.
    expect(screen.queryByRole('link', { current: 'page' })).toBeNull();
  });

  it('shows in the field of the navigation the search that is on screen, wherever it was asked for', async () => {
    const api = signedIn({
      'GET /buckets': () => json(200, []),
      'GET /search': () => json(200, { results: [], match: 'substring', examined: 0, complete: true }),
    });
    // An address typed by hand: the plus is a plus.
    window.location.hash = '#/search?q=rx:^a+b$';
    render(<App />);

    const field = within(await screen.findByRole('search', { name: 'Search' })).getByLabelText(
      'Search buckets and objects',
    ) as HTMLInputElement;
    expect(field.value).toBe('rx:^a+b$');
    await waitFor(() => {
      expect(api.callsTo('GET', '/search').map((call) => call.query.get('q'))).toEqual(['rx:^a+b$']);
    });

    // The form of the search screen asks for something else: the field of the navigation follows.
    const form = within(screen.getByRole('search', { name: 'Search buckets and objects' }));
    fireEvent.change(form.getByLabelText('Search for'), { target: { value: 'second one' } });
    fireEvent.click(form.getByRole('button', { name: 'Search' }));
    await waitFor(() => {
      expect(window.location.hash).toBe('#/search?q=second%20one');
    });
    await waitFor(() => {
      expect(field.value).toBe('second one');
    });

    // So does it when the address changes under it, as with the back button.
    window.location.hash = '#/search?q=third';
    await waitFor(() => {
      expect(field.value).toBe('third');
    });

    // Elsewhere the field keeps what it holds, typed or not.
    fireEvent.change(field, { target: { value: 'typed and not sent' } });
    window.location.hash = '#/buckets';
    expect(await screen.findByRole('heading', { name: 'Buckets' })).toBeDefined();
    expect(field.value).toBe('typed and not sent');
  });

  it('shows a magnifier when the navigation is folded, which unfolds it and puts the cursor in the field', async () => {
    signedIn({ 'GET /buckets': () => json(200, []) });
    render(<App />);

    const fold = await screen.findByRole('button', { name: 'Show icons only' });
    fireEvent.click(fold);
    expect(fold.getAttribute('aria-pressed')).toBe('true');

    const magnifier = screen.getByRole('button', { name: 'Show the search field' });
    expect(magnifier.getAttribute('title')).toBe('Search buckets and objects');
    fireEvent.click(magnifier);

    expect(fold.getAttribute('aria-pressed')).toBe('false');
    expect(window.localStorage.getItem('cactusfs.nav.folded')).toBe('0');
    await waitFor(() => {
      expect(document.activeElement).toBe(screen.getByLabelText('Search buckets and objects'));
    });
  });

  it('says so when the address names no screen', async () => {
    signedIn();
    window.location.hash = '#/nowhere';
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Not found' })).toBeDefined();
  });
});
