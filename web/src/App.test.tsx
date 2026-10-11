import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { App } from './App';
import { CSRF_TOKEN, fakeApi, json, noContent, problem, signedIn } from './test/fakeApi';

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
    expect(screen.getByRole('region', { name: 'About CactusFS' }).textContent).toContain('S3-compatible');
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
        signedInNow ? json(200, { kind: 'user', via: 'session', user: 'alice' }) : problem(401, 'Not signed in'),
      'POST /session': () => {
        signedInNow = true;
        return json(200, { user: 'alice', csrf_token: 'fresh-token', expires_at: '2026-10-08T12:00:00Z' });
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
    fakeApi({ 'GET /session': () => json(200, { kind: 'user', via: 'session', user: 'alice' }) });
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeDefined();
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

  it('says so when the address names no screen', async () => {
    signedIn();
    window.location.hash = '#/nowhere';
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Not found' })).toBeDefined();
  });
});
