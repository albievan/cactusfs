import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { CSRF_TOKEN, json, noContent, refusal, signedIn, type Call } from '../test/fakeApi';
import { formatPolicyDocument, MOST_POLICY_BYTES, utf8Bytes } from '../ui/format';
import { BucketDetails, type BucketFacts } from './BucketDetails';
import { BucketPolicySection, SubBucketsSection } from './SubBuckets';

const CREATED = '2026-11-01T00:00:00Z';
const LATER = '2026-11-02T09:30:00Z';

/** The policy of bucket photos the tests begin with. */
const BUCKET_POLICY = {
  Version: '2012-10-17',
  Statement: [{ Effect: 'Allow', Principal: { AWS: ['user/bob'] }, Action: 's3:GetObject', Resource: 'arn:aws:s3:::photos/*' }],
};

/** The policy of sub-bucket reports the tests begin with. */
const SUB_POLICY = {
  Version: '2012-10-17',
  Statement: [
    { Effect: 'Allow', Principal: { AWS: ['user/carol'] }, Action: 's3:GetObject', Resource: 'arn:aws:s3:::photos/reports/*' },
  ],
};

/** A policy as a user types it: indented their own way, its keys in their own order. */
const TYPED = '{\n    "Statement": [],\n    "Version": "2012-10-17"\n}';

/**
 * A policy that is larger than the API takes when indented, and not when
 * compact: 140 statements, about 26 KiB indented and 16 KiB without.
 */
const LARGE_POLICY = {
  Version: '2012-10-17',
  Statement: Array.from({ length: 140 }, (_, i) => ({
    Sid: `s${String(i)}`,
    Effect: 'Allow',
    Action: ['s3:GetObject', 's3:PutObject'],
    Resource: `arn:aws:s3:::photos/reports/${String(i)}/*`,
  })),
};

/** What the API answers when it cannot answer now. */
const UNAVAILABLE = () => refusal(503, 'Unavailable', 'Temporarily unavailable');

/** What the API answers a path it does not know: a node of an older release, during a rolling upgrade. */
const NO_SUCH_ENDPOINT = () => refusal(404, 'NotFound', 'No such endpoint');

const OLDER_NODE_SUB_BUCKETS = 'Sub-buckets are not available on the node that answered (an older release); try again.';
const OLDER_NODE_POLICIES = 'Bucket policies are not available on the node that answered (an older release); try again.';

/** A refusal with its code and the detail the API gives. */
function refusedWith(status: number, code: string, title: string, detail: string): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title, status, code, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

/** A time as the console shows it. */
function when(value: string): string {
  return new Date(value).toLocaleString();
}

function sub(name: string, createdAt = CREATED) {
  return { name, created_at: createdAt };
}

/** Unfolds a part of the details: "Bucket policy" or "Sub-buckets". */
function unfold(name: 'Bucket policy' | 'Sub-buckets') {
  fireEvent.click(screen.getByRole('button', { name }));
}

function typeInto(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function textOf(label: string): string {
  return (screen.getByLabelText(label) as HTMLTextAreaElement).value;
}

/** That a change was sent as every change of a session is: with the CSRF token and an Idempotency-Key of its own. */
function expectSentAsAChange(call: Call | undefined) {
  expect(call?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
  expect(call?.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
}

/** The hint an editor's text is described by. */
function hintOf(label: string): string {
  const id = screen.getByLabelText(label).getAttribute('aria-describedby') ?? '';
  return document.getElementById(id)?.textContent ?? '';
}

/** That every button styled as one has a hint: on a phone its text folds away. */
function expectEveryButtonHasATitle() {
  for (const control of screen.getAllByRole('button')) {
    if (control.className.includes('btn')) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
  }
}

/**
 * Puts a scrollIntoView in jsdom, which has none, for the test to see it
 * called; restore puts back what was there.
 */
function watchScrollIntoView() {
  const before = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
  const scrollIntoView = vi.fn();
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, writable: true, value: scrollIntoView });
  return {
    scrollIntoView,
    restore: () => {
      if (before === undefined) {
        Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
      } else {
        Object.defineProperty(Element.prototype, 'scrollIntoView', before);
      }
    },
  };
}

/** The live region of a sub-bucket's panel that says what was seen of it: always in the page. */
function panelStatus(region: ReturnType<typeof within>): HTMLElement {
  const said = region.getAllByRole('status').find((element: HTMLElement) => element.classList.contains('notice-region'));
  expect(said).toBeDefined();
  return said as HTMLElement;
}

/** The row of a sub-bucket in the list. */
function rowOf(name: string): HTMLTableRowElement {
  return screen.getByRole('button', { name: `Open sub-bucket ${name}` }).closest('tr') as HTMLTableRowElement;
}

const BUCKET_POLICY_LABEL = 'Policy of bucket photos as JSON';
const NEW_SUB_NAME = 'Sub-bucket name';
const NEW_SUB_POLICY = 'Policy of the sub-bucket as JSON (optional)';

describe("the policy and the sub-buckets in a bucket's details", () => {
  it('are folded, and ask the API nothing until they are unfolded', async () => {
    const api = signedIn({
      'GET /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }),
      'GET /buckets/photos/subbuckets': () => json(200, [sub('reports')]),
    });
    const facts: BucketFacts = {
      bucket: { data: { name: 'photos', created_at: CREATED }, error: null, loading: false, reload: () => undefined },
      usage: {
        data: { object_count: 1, total_size: 3, complete: true, exact: true },
        error: null,
        loading: false,
        reload: () => undefined,
      },
    };
    render(<BucketDetails bucket="photos" facts={facts} />);

    const details = screen.getByRole('region', { name: 'Bucket details' });
    const policy = within(details).getByRole('region', { name: 'Bucket policy' });
    const subs = within(details).getByRole('region', { name: 'Sub-buckets' });
    const policyToggle = within(policy).getByRole('button', { name: 'Bucket policy' });
    const subsToggle = within(subs).getByRole('button', { name: 'Sub-buckets' });
    expect(policyToggle.getAttribute('aria-expanded')).toBe('false');
    expect(subsToggle.getAttribute('aria-expanded')).toBe('false');
    expect(api.calls.filter((call) => call.path.startsWith('/buckets/photos/'))).toHaveLength(0);

    fireEvent.click(subsToggle);
    expect(subsToggle.getAttribute('aria-expanded')).toBe('true');
    expect(await within(subs).findByText('reports')).toBeTruthy();
    expect(api.callsTo('GET', '/buckets/photos/subbuckets')).toHaveLength(1);
    expect(api.callsTo('GET', '/buckets/photos/policy')).toHaveLength(0);

    fireEvent.click(policyToggle);
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(formatPolicyDocument(BUCKET_POLICY));
    });

    // Folded again, what was read is kept: unfolded, it is not asked for again.
    fireEvent.click(subsToggle);
    const body = document.getElementById(subsToggle.getAttribute('aria-controls') ?? '');
    expect(body?.hidden).toBe(true);
    fireEvent.click(subsToggle);
    expect(body?.hidden).toBe(false);
    expect(api.callsTo('GET', '/buckets/photos/subbuckets')).toHaveLength(1);
  });
});

describe('the policy of a bucket', () => {
  it('is shown as indented JSON', async () => {
    signedIn({ 'GET /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }) });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');

    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(JSON.stringify(BUCKET_POLICY, null, 2));
    });
    expect(screen.getByRole('button', { name: 'Remove policy' })).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says that a bucket has none, and offers nothing to remove', async () => {
    signedIn({ 'GET /buckets/photos/policy': () => json(200, {}) });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');

    expect(await screen.findByText(/This bucket has no policy/)).toBeTruthy();
    expect(textOf(BUCKET_POLICY_LABEL)).toBe('');
    expect(screen.queryByRole('button', { name: 'Remove policy' })).toBeNull();
  });

  it('is saved as typed, with the CSRF token and an Idempotency-Key, and read again', async () => {
    let current: unknown = BUCKET_POLICY;
    const api = signedIn({
      'GET /buckets/photos/policy': () => json(200, { document: current }),
      'PUT /buckets/photos/policy': (call) => {
        current = (JSON.parse(call.body) as { document: unknown }).document;
        return json(200, { document: current });
      },
    });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(formatPolicyDocument(BUCKET_POLICY));
    });

    typeInto(BUCKET_POLICY_LABEL, TYPED);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/policy')).toHaveLength(2);
    });
    const [put] = api.callsTo('PUT', '/buckets/photos/policy');
    // The document goes as it was typed: the API reads it as sent.
    expect(put?.body).toBe(`{"document":${TYPED}}`);
    expect(put?.headers.get('Content-Type')).toBe('application/json');
    expectSentAsAChange(put);
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(formatPolicyDocument({ Statement: [], Version: '2012-10-17' }));
    });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows, next to the editor, why the API refused a document, and keeps what was typed', async () => {
    const detail = 'statement 1: s3:BypassGovernanceRetention cannot be allowed by a bucket policy';
    const api = signedIn({
      'GET /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }),
      'PUT /buckets/photos/policy': () => refusedWith(400, 'InvalidPolicy', 'Invalid policy', detail),
    });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).not.toBe('');
    });

    typeInto(BUCKET_POLICY_LABEL, TYPED);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Invalid policy');
    expect(alert.textContent).toContain(detail);
    // In the editor's own form, under its text.
    expect(alert.closest('form')).toBe(screen.getByLabelText(BUCKET_POLICY_LABEL).closest('form'));
    expect(textOf(BUCKET_POLICY_LABEL)).toBe(TYPED);
    expect(api.callsTo('GET', '/buckets/photos/policy')).toHaveLength(1);
  });

  it('sends nothing that is not a JSON object', async () => {
    const api = signedIn({ 'GET /buckets/photos/policy': () => json(200, {}) });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');
    await screen.findByText(/This bucket has no policy/);

    typeInto(BUCKET_POLICY_LABEL, '{"Version": ');
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    expect((await screen.findByRole('alert')).textContent).toContain('The policy is not valid JSON');
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
  });

  it('is removed only once the question is answered yes', async () => {
    let current: unknown = BUCKET_POLICY;
    const api = signedIn({
      'GET /buckets/photos/policy': () => json(200, current === undefined ? {} : { document: current }),
      'DELETE /buckets/photos/policy': () => {
        current = undefined;
        return noContent();
      },
    });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');
    fireEvent.click(await screen.findByRole('button', { name: 'Remove policy' }));

    // Asked, and nothing sent; cancelled, nothing sent either.
    const question = screen.getByRole('group', { name: 'Remove the policy of bucket photos?' });
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('DELETE', '/buckets/photos/policy')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Remove policy' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, remove policy' }));

    expect(await screen.findByText(/This bucket has no policy/)).toBeTruthy();
    const deletes = api.callsTo('DELETE', '/buckets/photos/policy');
    expect(deletes).toHaveLength(1);
    expectSentAsAChange(deletes[0]);
    expect(textOf(BUCKET_POLICY_LABEL)).toBe('');
  });

  it('says that a caller may not read it, without an alert', async () => {
    signedIn({ 'GET /buckets/photos/policy': () => refusal(403, 'AccessDenied', 'Access denied') });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');

    expect(await screen.findByText(/You may not read the policy of this bucket/)).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says in the hint of its editor that spaces count against the 20 KiB', async () => {
    signedIn({ 'GET /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }) });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(formatPolicyDocument(BUCKET_POLICY));
    });

    expect(hintOf(BUCKET_POLICY_LABEL)).toBe(
      'Sent as typed: spaces and line breaks count towards the 20 KiB a policy may have.',
    );
  });

  it('opens compact when indented it would be larger than 20 KiB, and is saved unchanged as shown', async () => {
    expect(utf8Bytes(formatPolicyDocument(LARGE_POLICY))).toBeGreaterThan(MOST_POLICY_BYTES);
    expect(utf8Bytes(JSON.stringify(LARGE_POLICY))).toBeLessThanOrEqual(MOST_POLICY_BYTES);
    const api = signedIn({
      'GET /buckets/photos/policy': () => json(200, { document: LARGE_POLICY }),
      'PUT /buckets/photos/policy': () => json(200, { document: LARGE_POLICY }),
    });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');

    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(JSON.stringify(LARGE_POLICY));
    });
    expect(hintOf(BUCKET_POLICY_LABEL)).toContain('Shown without indentation: indented, it would be larger than 20 KiB.');
    expect(hintOf(BUCKET_POLICY_LABEL)).toContain('spaces and line breaks count towards the 20 KiB');

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    const [put] = api.callsTo('PUT', '/buckets/photos/policy');
    expect(put?.body).toBe(`{"document":${JSON.stringify(LARGE_POLICY)}}`);
    expect(utf8Bytes(put?.body ?? '')).toBeLessThan(MOST_POLICY_BYTES + '{"document":}'.length);
  });

  it('shows a failed reading again above the editor, which keeps what it showed', async () => {
    const api = signedIn({
      'GET /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }),
      'PUT /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }),
    });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).toBe(formatPolicyDocument(BUCKET_POLICY));
    });

    api.on({ 'GET /buckets/photos/policy': UNAVAILABLE });
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Temporarily unavailable');
    const editor = screen.getByLabelText(BUCKET_POLICY_LABEL);
    expect(alert.compareDocumentPosition(editor) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(textOf(BUCKET_POLICY_LABEL)).toBe(formatPolicyDocument(BUCKET_POLICY));
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy();
  });
});

describe('the list of sub-buckets', () => {
  it('shows each by name and when it was made, in a grid that is cards on a phone', async () => {
    signedIn({ 'GET /buckets/photos/subbuckets': () => json(200, [sub('reports'), sub('scans', LATER)]) });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');

    const table = await screen.findByRole('table');
    expect(within(table).getAllByRole('columnheader').map((th) => th.textContent)).toEqual(['Name', 'Created', 'Actions']);
    // A grid that is not kept a grid: on a phone each row is a card, its
    // cells labelled with their headings.
    expect(table.className).toContain('grid');
    expect(table.className).not.toContain('grid-keep');
    for (const cell of within(table).getAllByRole('cell')) {
      expect(cell.getAttribute('data-label')).not.toBeNull();
    }
    const reports = rowOf('reports');
    expect(reports.querySelector('td[data-label="Name"]')?.textContent).toBe('reports');
    expect(reports.querySelector('td[data-label="Created"]')?.textContent).toBe(when(CREATED));
    expect(rowOf('scans').querySelector('td[data-label="Created"]')?.textContent).toBe(when(LATER));
    // The buttons say what they do on a phone too, where their text folds away.
    const open = within(reports).getByRole('button', { name: 'Open sub-bucket reports' });
    expect(open.querySelector('.btn-text')?.textContent).toBe('Policy');
    expect(open.getAttribute('title')).not.toBeNull();
    expect(screen.getByText(/A bucket has at most 10/)).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says that there is none the caller may see', async () => {
    signedIn({ 'GET /buckets/photos/subbuckets': () => json(200, []) });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');

    expect(await screen.findByText('There are no sub-buckets you may see.')).toBeTruthy();
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.getByRole('button', { name: 'Create sub-bucket' })).toBeTruthy();
  });

  it('shows a failed reading again above the list it had, and asks again', async () => {
    let listed = [sub('reports'), sub('scans')];
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, listed),
      'DELETE /buckets/photos/subbuckets/reports': () => {
        listed = [sub('scans')];
        return noContent();
      },
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByRole('table');

    // The deletion is done; reading the list after it is not.
    api.on({ 'GET /buckets/photos/subbuckets': UNAVAILABLE });
    fireEvent.click(within(rowOf('reports')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Temporarily unavailable');
    // What was read before stays, under the failure.
    const table = screen.getByRole('table');
    expect(alert.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Open sub-bucket scans' })).toBeTruthy();

    api.on({ 'GET /buckets/photos/subbuckets': () => json(200, listed) });
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'Open sub-bucket reports' })).toBeNull();
    });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says, without an alert, that a caller refused (403) may not see the sub-buckets', async () => {
    signedIn({ 'GET /buckets/photos/subbuckets': () => refusal(403, 'AccessDenied', 'Access denied') });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');

    expect(await screen.findByText('You may not see the sub-buckets of this bucket.')).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByText('Access denied')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
  });
});

describe('creating a sub-bucket', () => {
  it('sends its policy as typed, with the CSRF token and an Idempotency-Key, and lists it', async () => {
    let listed = [] as ReturnType<typeof sub>[];
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, listed),
      'PUT /buckets/photos/subbuckets/reports': (call) => {
        listed = [sub('reports')];
        return json(200, { ...sub('reports'), policy: (JSON.parse(call.body) as { policy: unknown }).policy });
      },
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByText('There are no sub-buckets you may see.');
    // The rule of a name is said where it is typed.
    const name = screen.getByLabelText(NEW_SUB_NAME);
    const rule = document.getElementById(name.getAttribute('aria-describedby') ?? '');
    expect(rule?.textContent).toContain('1 to 63 lower-case letters, digits and hyphens');

    typeInto(NEW_SUB_NAME, 'reports');
    typeInto(NEW_SUB_POLICY, TYPED);
    fireEvent.click(screen.getByRole('button', { name: 'Create sub-bucket' }));

    expect(await screen.findByRole('button', { name: 'Open sub-bucket reports' })).toBeTruthy();
    const puts = api.callsTo('PUT', '/buckets/photos/subbuckets/reports');
    expect(puts).toHaveLength(1);
    expect(puts[0]?.body).toBe(`{"policy":${TYPED}}`);
    expectSentAsAChange(puts[0]);
    expect((screen.getByLabelText(NEW_SUB_NAME) as HTMLInputElement).value).toBe('');
    expect(textOf(NEW_SUB_POLICY)).toBe('');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('sends no policy when none is typed', async () => {
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, []),
      'PUT /buckets/photos/subbuckets/scans': () => json(200, sub('scans')),
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByText('There are no sub-buckets you may see.');

    typeInto(NEW_SUB_NAME, 'scans');
    fireEvent.click(screen.getByRole('button', { name: 'Create sub-bucket' }));

    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/subbuckets')).toHaveLength(2);
    });
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')[0]?.body).toBe('{}');
  });

  it.each([
    {
      status: 409,
      code: 'TooManySubBuckets',
      title: 'Too many sub-buckets',
      detail: 'the bucket has 10 sub-buckets',
      said: 'A bucket has at most 10 sub-buckets, and photos has as many already: delete one before creating reports.',
    },
    {
      status: 400,
      code: 'InvalidArgument',
      title: 'Invalid argument',
      detail: 'not a sub-bucket name',
      said: 'reports is not a name a sub-bucket can have: 1 to 63 lower-case letters',
    },
    {
      status: 400,
      code: 'InvalidPolicy',
      title: 'Invalid policy',
      detail: 'statement 1: resource arn:aws:s3:::photos/* is not of the sub-bucket reports',
      said: 'The policy of reports was refused, and nothing was created.',
    },
  ])('says why the API refused it: $status $code', async ({ status, code, title, detail, said }) => {
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, []),
      'PUT /buckets/photos/subbuckets/reports': () => refusedWith(status, code, title, detail),
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByText('There are no sub-buckets you may see.');

    typeInto(NEW_SUB_NAME, 'reports');
    typeInto(NEW_SUB_POLICY, TYPED);
    fireEvent.click(screen.getByRole('button', { name: 'Create sub-bucket' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(said);
    // The API's own words too.
    expect(alert.textContent).toContain(detail);
    expect(alert.closest('form')).toBe(screen.getByLabelText(NEW_SUB_NAME).closest('form'));
    // What was typed is kept, and nothing is listed.
    expect((screen.getByLabelText(NEW_SUB_NAME) as HTMLInputElement).value).toBe('reports');
    expect(textOf(NEW_SUB_POLICY)).toBe(TYPED);
    expect(api.callsTo('GET', '/buckets/photos/subbuckets')).toHaveLength(1);
  });

  it('does not send a name that is in the list already: that would replace its policy', async () => {
    const api = signedIn({ 'GET /buckets/photos/subbuckets': () => json(200, [sub('reports')]) });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByRole('table');

    typeInto(NEW_SUB_NAME, 'reports');
    fireEvent.click(screen.getByRole('button', { name: 'Create sub-bucket' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Sub-bucket reports is there already');
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(0);
  });

  it('does not send a name the address cannot carry', async () => {
    const api = signedIn({ 'GET /buckets/photos/subbuckets': () => json(200, []) });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByText('There are no sub-buckets you may see.');

    typeInto(NEW_SUB_NAME, '..');
    fireEvent.click(screen.getByRole('button', { name: 'Create sub-bucket' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Name the sub-bucket');
    expect(api.calls.filter((call) => call.method === 'PUT')).toHaveLength(0);
  });
});

describe("a sub-bucket's policy", () => {
  /** The API of bucket photos with sub-bucket reports, whose policy the test changes. */
  function withReports() {
    let policy: unknown = SUB_POLICY;
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, [sub('reports')]),
      'GET /buckets/photos/subbuckets/reports': () =>
        json(200, policy === undefined ? sub('reports') : { ...sub('reports'), policy }),
      'PUT /buckets/photos/subbuckets/reports': (call) => {
        policy = (JSON.parse(call.body) as { policy?: unknown }).policy;
        return json(200, policy === undefined ? sub('reports') : { ...sub('reports'), policy });
      },
    });
    return api;
  }

  const LABEL = 'Policy of sub-bucket reports as JSON';

  async function openReports() {
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = await screen.findByRole('region', { name: 'Sub-bucket reports' });
    await waitFor(() => {
      expect(textOf(LABEL)).not.toBe('');
    });
    return within(region);
  }

  it('is shown, and saved as typed', async () => {
    const api = withReports();
    const region = await openReports();
    expect(textOf(LABEL)).toBe(formatPolicyDocument(SUB_POLICY));
    expect(region.getByText(/^Created /).textContent).toContain(`Created ${when(CREATED)}.`);

    typeInto(LABEL, TYPED);
    fireEvent.click(region.getByRole('button', { name: 'Save policy' }));

    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/subbuckets/reports')).toHaveLength(2);
    });
    const [put] = api.callsTo('PUT', '/buckets/photos/subbuckets/reports');
    expect(put?.body).toBe(`{"policy":${TYPED}}`);
    expectSentAsAChange(put);
    // Its time of creation is the one shown: nothing was made anew.
    expect(region.queryByText(/was created again/)).toBeNull();
    expect(panelStatus(region).textContent).toBe('');
  });

  it('shows why the API refused a document, next to the editor', async () => {
    const detail = 'statement 1: a sub-bucket policy cannot deny s3:ListBucket';
    const api = withReports();
    api.on({ 'PUT /buckets/photos/subbuckets/reports': () => refusedWith(400, 'InvalidPolicy', 'Invalid policy', detail) });
    const region = await openReports();

    typeInto(LABEL, TYPED);
    fireEvent.click(region.getByRole('button', { name: 'Save policy' }));

    const alert = await region.findByRole('alert');
    expect(alert.textContent).toContain(detail);
    expect(alert.closest('form')).toBe(screen.getByLabelText(LABEL).closest('form'));
    expect(textOf(LABEL)).toBe(TYPED);
  });

  it('is removed, once asked, by a PUT without a policy', async () => {
    const api = withReports();
    const region = await openReports();

    fireEvent.click(region.getByRole('button', { name: 'Remove policy' }));
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(0);
    expect(region.getByRole('group', { name: 'Remove the policy of sub-bucket reports?' })).toBeTruthy();
    fireEvent.click(region.getByRole('button', { name: 'Yes, remove policy' }));

    expect(await region.findByText(/It has no policy of its own/)).toBeTruthy();
    const [put] = api.callsTo('PUT', '/buckets/photos/subbuckets/reports');
    expect(put?.body).toBe('{}');
    expectSentAsAChange(put);
    expect(textOf(LABEL)).toBe('');
    expect(region.queryByRole('button', { name: 'Remove policy' })).toBeNull();
  });

  it('closes', async () => {
    withReports();
    const region = await openReports();
    fireEvent.click(region.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('region', { name: 'Sub-bucket reports' })).toBeNull();
  });

  it.each([
    {
      change: 'a Save',
      act: (region: ReturnType<typeof within>) => {
        typeInto(LABEL, TYPED);
        fireEvent.click(region.getByRole('button', { name: 'Save policy' }));
      },
      sent: `{"policy":${TYPED}}`,
      policySaid: 'the policy just saved is weighed for them with that of the bucket.',
    },
    {
      change: 'a Remove',
      act: (region: ReturnType<typeof within>) => {
        fireEvent.click(region.getByRole('button', { name: 'Remove policy' }));
        fireEvent.click(region.getByRole('button', { name: 'Yes, remove policy' }));
      },
      sent: '{}',
      policySaid: 'it has no policy of its own.',
    },
  ])('says plainly, after $change, that the sub-bucket was created again since it was shown', async ({ act, sent, policySaid }) => {
    const api = withReports();
    // Someone else deletes reports; the PUT of this panel creates it anew.
    api.on({
      'PUT /buckets/photos/subbuckets/reports': (call) => {
        const policy = (JSON.parse(call.body) as { policy?: unknown }).policy;
        const made = policy === undefined ? sub('reports', LATER) : { ...sub('reports', LATER), policy };
        api.on({ 'GET /buckets/photos/subbuckets/reports': () => json(200, made) });
        return json(200, made);
      },
    });
    const region = await openReports();

    // The live region is there, empty, before there is anything to say.
    const said = panelStatus(region);
    expect(said.textContent).toBe('');

    act(region);

    await waitFor(() => {
      expect(said.textContent).toContain(
        `Sub-bucket reports was created again (${when(LATER)}) after it was shown here; it may have been deleted meanwhile.`,
      );
    });
    // Said as seen: not as caused by this change, and nothing is advised as if it were.
    expect(said.textContent).not.toContain('this change');
    expect(said.textContent).not.toContain('Delete it');
    expect(said.textContent).toContain('The keys of bucket photos under "reports/" are its objects');
    expect(said.textContent).toContain(policySaid);
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')[0]?.body).toBe(sent);
    // The panel shows the sub-bucket as it is now.
    await waitFor(() => {
      expect(region.getByText(/^Created /).textContent).toContain(`Created ${when(LATER)}.`);
    });
  });

  it('says that a sub-bucket that is gone is not there any more (404 NoSuchSubBucket)', async () => {
    withReports().on({
      'GET /buckets/photos/subbuckets/reports': () =>
        refusedWith(404, 'NoSuchSubBucket', 'No such sub-bucket', 'there is no sub-bucket reports'),
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = within(await screen.findByRole('region', { name: 'Sub-bucket reports' }));

    const alert = await region.findByRole('alert');
    expect(alert.textContent).toContain('Sub-bucket reports is not there any more.');
    expect(alert.textContent).toContain('there is no sub-bucket reports');
    expect(alert.textContent).not.toContain('older release');
    expect(region.getByRole('button', { name: 'Try again' })).toBeTruthy();
    expect(region.getByRole('button', { name: 'Close' })).toBeTruthy();
    expect(region.queryByLabelText(LABEL)).toBeNull();
  });

  it('says, without an alert, that a caller may not read it (403), and still offers the editor and Remove', async () => {
    const api = withReports();
    api.on({ 'GET /buckets/photos/subbuckets/reports': () => refusal(403, 'AccessDenied', 'Access denied') });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = within(await screen.findByRole('region', { name: 'Sub-bucket reports' }));

    expect(await region.findByText(/You may not read the policy of this sub-bucket/)).toBeTruthy();
    expect(region.queryByRole('alert')).toBeNull();
    expect(region.queryByText('Access denied')).toBeNull();
    expect(textOf(LABEL)).toBe('');
    expect(region.getByRole('button', { name: 'Save policy' })).toBeTruthy();
    expect(region.getByRole('button', { name: 'Remove policy' })).toBeTruthy();
    expect(region.queryByRole('button', { name: 'Try again' })).toBeNull();

    // With no time shown, nothing can be told of a re-creation: none is said.
    fireEvent.click(region.getByRole('button', { name: 'Remove policy' }));
    fireEvent.click(region.getByRole('button', { name: 'Yes, remove policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(1);
    });
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')[0]?.body).toBe('{}');
    // Read again once the answer was looked at.
    await waitFor(() => {
      expect(api.callsTo('GET', '/buckets/photos/subbuckets/reports')).toHaveLength(2);
    });
    expect(panelStatus(region).textContent).toBe('');
  });

  it('takes the keyboard to its heading when opened, and back to its row when closed', async () => {
    const scrolling = watchScrollIntoView();
    try {
      withReports();
      render(<SubBucketsSection bucket="photos" />);
      unfold('Sub-buckets');
      const opener = await screen.findByRole('button', { name: 'Open sub-bucket reports' });
      fireEvent.click(opener);
      const region = within(await screen.findByRole('region', { name: 'Sub-bucket reports' }));
      const heading = region.getByRole('heading', { name: 'Sub-bucket reports' });

      await waitFor(() => {
        expect(document.activeElement).toBe(heading);
      });
      expect(heading.getAttribute('tabindex')).toBe('-1');
      expect(scrolling.scrollIntoView).toHaveBeenCalled();

      fireEvent.click(region.getByRole('button', { name: 'Close' }));
      expect(screen.queryByRole('region', { name: 'Sub-bucket reports' })).toBeNull();
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Open sub-bucket reports' }));
    } finally {
      scrolling.restore();
    }
  });

  it('gives the keyboard to the list when closed after its row has gone', async () => {
    const api = withReports();
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = within(await screen.findByRole('region', { name: 'Sub-bucket reports' }));
    await waitFor(() => {
      expect(textOf(LABEL)).not.toBe('');
    });

    // The list read after the save no longer has reports: someone else deleted it.
    api.on({ 'GET /buckets/photos/subbuckets': () => json(200, [sub('scans')]) });
    fireEvent.click(region.getByRole('button', { name: 'Save policy' }));
    await screen.findByRole('button', { name: 'Open sub-bucket scans' });
    expect(screen.queryByRole('button', { name: 'Open sub-bucket reports' })).toBeNull();

    fireEvent.click(region.getByRole('button', { name: 'Close' }));
    const list = screen.getByRole('region', { name: 'Sub-buckets of bucket photos' });
    expect(list.contains(screen.getByRole('table'))).toBe(true);
    expect(document.activeElement).toBe(list);
  });

  it('gives the keyboard to the named list when the sub-bucket whose panel is open is deleted', async () => {
    const api = withReports();
    api.on({
      'DELETE /buckets/photos/subbuckets/reports': () => {
        api.on({ 'GET /buckets/photos/subbuckets': () => json(200, [sub('scans')]) });
        return noContent();
      },
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    await screen.findByRole('region', { name: 'Sub-bucket reports' });

    fireEvent.click(within(rowOf('reports')).getByRole('button', { name: 'Delete' }));
    const yes = screen.getByRole('button', { name: 'Yes, delete' });
    yes.focus();
    fireEvent.click(yes);

    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'Sub-bucket reports' })).toBeNull();
    });
    await screen.findByRole('button', { name: 'Open sub-bucket scans' });
    const list = screen.getByRole('region', { name: 'Sub-buckets of bucket photos' });
    expect(document.activeElement).toBe(list);
    expect(document.activeElement).not.toBe(document.body);
  });
});

describe('the buttons of the opened sections', () => {
  it('each have a hint, questions included', async () => {
    signedIn({
      'GET /buckets/photos/policy': () => json(200, { document: BUCKET_POLICY }),
      'GET /buckets/photos/subbuckets': () => json(200, [sub('reports')]),
      'GET /buckets/photos/subbuckets/reports': () => json(200, { ...sub('reports'), policy: SUB_POLICY }),
    });
    render(
      <>
        <BucketPolicySection bucket="photos" />
        <SubBucketsSection bucket="photos" />
      </>,
    );
    unfold('Bucket policy');
    unfold('Sub-buckets');
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = within(await screen.findByRole('region', { name: 'Sub-bucket reports' }));
    await waitFor(() => {
      expect(textOf('Policy of sub-bucket reports as JSON')).not.toBe('');
    });
    await waitFor(() => {
      expect(textOf(BUCKET_POLICY_LABEL)).not.toBe('');
    });
    expectEveryButtonHasATitle();

    // Asked, the questions' own buttons have hints too.
    fireEvent.click(region.getByRole('button', { name: 'Remove policy' }));
    fireEvent.click(within(rowOf('reports')).getByRole('button', { name: 'Delete' }));
    expect(screen.getAllByRole('group')).toHaveLength(2);
    expectEveryButtonHasATitle();
  });
});

describe('deleting a sub-bucket', () => {
  it('asks first, then deletes it and lists what is left', async () => {
    let listed = [sub('reports'), sub('scans')];
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, listed),
      'DELETE /buckets/photos/subbuckets/reports': () => {
        listed = [sub('scans')];
        return noContent();
      },
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('reports')).getByRole('button', { name: 'Delete' }));
    expect(api.callsTo('DELETE', '/buckets/photos/subbuckets/reports')).toHaveLength(0);
    expect(
      screen.getByRole('group', { name: 'Delete sub-bucket reports? Its keys are not deleted with it.' }),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'Open sub-bucket reports' })).toBeNull();
    });
    expect(screen.getByRole('button', { name: 'Open sub-bucket scans' })).toBeTruthy();
    const deletes = api.callsTo('DELETE', '/buckets/photos/subbuckets/reports');
    expect(deletes).toHaveLength(1);
    expectSentAsAChange(deletes[0]);
  });

  it('explains that one that is not empty is not deleted (409 SubBucketNotEmpty)', async () => {
    signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, [sub('reports')]),
      'DELETE /buckets/photos/subbuckets/reports': () =>
        refusedWith(409, 'SubBucketNotEmpty', 'Sub-bucket not empty', 'the sub-bucket is not empty'),
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('reports')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Sub-bucket reports is not empty');
    expect(alert.textContent).toContain('"reports/"');
    expect(alert.textContent).toContain('delete them first');
    expect(alert.textContent).toContain('the sub-bucket is not empty');
    expect(screen.getByRole('button', { name: 'Open sub-bucket reports' })).toBeTruthy();
  });
});

describe('deleting a sub-bucket that is gone', () => {
  it('says that it is not there any more (404 NoSuchSubBucket), and lists what is there', async () => {
    let listed = [sub('reports')];
    const api = signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, listed),
      'DELETE /buckets/photos/subbuckets/reports': () => {
        listed = [];
        return refusedWith(404, 'NoSuchSubBucket', 'No such sub-bucket', 'there is no sub-bucket reports');
      },
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('reports')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, delete' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Sub-bucket reports is not there any more.');
    expect(alert.textContent).toContain('there is no sub-bucket reports');
    expect(alert.textContent).not.toContain('older release');
    expect(await screen.findByText('There are no sub-buckets you may see.')).toBeTruthy();
    expect(api.callsTo('GET', '/buckets/photos/subbuckets')).toHaveLength(2);
  });
});

describe('a node of an older release (404 NotFound)', () => {
  it('is said of the list of sub-buckets, which is asked for again', async () => {
    const api = signedIn({ 'GET /buckets/photos/subbuckets': NO_SUCH_ENDPOINT });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(OLDER_NODE_SUB_BUCKETS);
    expect(alert.textContent).toContain('No such endpoint');

    // Asked again, the request may reach a node that has the route.
    api.on({ 'GET /buckets/photos/subbuckets': () => json(200, [sub('reports')]) });
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('button', { name: 'Open sub-bucket reports' })).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('is said of the policy of a bucket', async () => {
    signedIn({ 'GET /buckets/photos/policy': NO_SUCH_ENDPOINT });
    render(<BucketPolicySection bucket="photos" />);
    unfold('Bucket policy');

    expect((await screen.findByRole('alert')).textContent).toContain(OLDER_NODE_POLICIES);
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy();
  });

  it('is said of a change, too', async () => {
    signedIn({
      'GET /buckets/photos/subbuckets': () => json(200, []),
      'PUT /buckets/photos/subbuckets/reports': NO_SUCH_ENDPOINT,
    });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');
    await screen.findByText('There are no sub-buckets you may see.');

    typeInto(NEW_SUB_NAME, 'reports');
    fireEvent.click(screen.getByRole('button', { name: 'Create sub-bucket' }));

    expect((await screen.findByRole('alert')).textContent).toContain(OLDER_NODE_SUB_BUCKETS);
  });

  it('is not said of a bucket that is not there (404 NoSuchBucket)', async () => {
    signedIn({ 'GET /buckets/photos/subbuckets': () => refusal(404, 'NoSuchBucket', 'No such bucket') });
    render(<SubBucketsSection bucket="photos" />);
    unfold('Sub-buckets');

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('No such bucket');
    expect(alert.textContent).not.toContain('older release');
  });
});
