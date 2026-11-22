import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { PolicyCatalogue, PolicyFault, PolicyValidation, StatementAnalysis } from '../api/client';
import {
  CSRF_TOKEN,
  holdRequests,
  json,
  noContent,
  refusal,
  signedIn,
  type Call,
  type FakeApi,
  type Handler,
} from '../test/fakeApi';
import { formatPolicyDocument } from '../ui/format';
import {
  CATALOGUE_FAILED_NOTICE,
  CATALOGUE_REFUSED_NOTICE,
  CATALOGUE_WAIT_MS,
  CHANGED_WHILE_CHECKED,
  NO_VALIDATION_NOTICE,
  NOT_SAVED_NOTICE,
  OLDER_NODE_NOTICE,
  VALIDATION_DELAY_MS,
} from '../ui/policyHooks';
import { BucketDetails, type BucketFacts } from './BucketDetails';
import { BucketPolicySection, SubBucketsSection } from './SubBuckets';

// A bucket's and a sub-bucket's policy with the policy builder (PLAN 9.9,
// ADR-0032): every criterion of the row - the builder for a bucket's
// policy, principals kept as written, the warning before a policy open to
// anyone is saved, what a sub-bucket may hold, a document stored through S3
// opened and not sent again unchanged, no validation on opening, removal
// after a question, the fallbacks of 9.8, and access.

const CREATED = '2026-11-01T00:00:00Z';

/** A catalogue as the node answers it, cut down to what the tests choose from. */
const CATALOGUE: PolicyCatalogue = {
  version: '2012-10-17',
  actions: [
    action('s3:GetObject', 'Objects', 'Read an object.', ['arn:aws:s3:::{bucket}/{key}'], {
      in_bucket_policy: true,
      anonymous: true,
      in_sub_bucket_policy: true,
    }),
    action('s3:PutObject', 'Objects', 'Store an object.', ['arn:aws:s3:::{bucket}/{key}'], {
      in_bucket_policy: true,
      in_sub_bucket_policy: true,
    }),
    action('s3:ListBucket', 'Buckets', 'List the objects of a bucket.', ['arn:aws:s3:::{bucket}'], {
      in_bucket_policy: true,
      anonymous: true,
      in_sub_bucket_policy: true,
    }),
    action('s3:PutBucketTagging', 'Buckets', 'Set the tags of a bucket.', ['arn:aws:s3:::{bucket}'], {
      in_bucket_policy: true,
    }),
    // No bucket policy may allow it; one may deny it.
    action('s3:BypassGovernanceRetention', 'Retention', 'Bypass governance retention.', ['arn:aws:s3:::{bucket}/{key}'], {}),
    action('admin:GetUser', 'Users', 'Read a user.', ['arn:cactusfs:admin:::user/{name}'], {}),
  ],
  condition_keys: [
    {
      key: 'aws:SourceIp',
      operators: ['IpAddress', 'NotIpAddress'],
      value_kind: 'ip_prefix',
      when_unknown: 'An unknown source address is decided against the request.',
    },
  ],
  resources: [
    { kind: 'bucket', template: 'arn:aws:s3:::{bucket}', description: 'a bucket, by its name' },
    { kind: 'object', template: 'arn:aws:s3:::{bucket}/{key}', description: 'an object of a bucket, by its key' },
    { kind: 'user', template: 'arn:cactusfs:admin:::user/{name}', description: 'a user, by name' },
  ],
  limits: { max_bytes: 20480, max_statements: 64, max_values: 128, max_depth: 8 },
  bucket_policy_principals: { anyone: ['"*"', '{"AWS":"*"}'], user_arn_prefix: 'arn:aws:iam:::user/' },
};

function action(
  name: string,
  group: string,
  description: string,
  resources: string[],
  flags: { in_bucket_policy?: boolean; anonymous?: boolean; in_sub_bucket_policy?: boolean },
) {
  return {
    name,
    group,
    description,
    resources,
    read: name.includes('Get') || name.includes('List'),
    by_name_only: false,
    in_bucket_policy: flags.in_bucket_policy ?? false,
    anonymous: flags.anonymous ?? false,
    in_sub_bucket_policy: flags.in_sub_bucket_policy ?? false,
  };
}

/** A validation answer: valid exactly when there is no fault. */
function verdict(faults: PolicyFault[] = [], statements: StatementAnalysis[] = []): PolicyValidation {
  return { valid: faults.length === 0, faults, truncated: false, statements };
}

/** The node's analysis of one statement. */
function analysis(index: number, more: Partial<StatementAnalysis> = {}): StatementAnalysis {
  return {
    index,
    effect: 'Allow',
    actions: [],
    anonymous_actions: [],
    never_anonymous: [],
    unmatched_patterns: [],
    ...more,
  };
}

/** A statement the node says is open to anyone: GetObject for callers who have not signed in, PutObject for every user. */
const OPEN = analysis(0, {
  sid: 'Public',
  actions: ['s3:GetObject', 's3:PutObject'],
  open_to_anyone: true,
  anonymous_actions: ['s3:GetObject'],
  never_anonymous: ['s3:PutObject'],
});

/**
 * A bucket policy as S3's PutBucketPolicy stores it and GetBucketPolicy
 * answers it: each field in the form its author wrote - a Principal of `"*"`,
 * one user as a single string, an Action as a string.
 */
const STORED_THROUGH_S3 = {
  Version: '2012-10-17',
  Statement: [
    {
      Sid: 'Public',
      Effect: 'Allow',
      Principal: '*',
      Action: 's3:GetObject',
      Resource: 'arn:aws:s3:::photos/public/*',
    },
    {
      Effect: 'Allow',
      Principal: { AWS: 'arn:aws:iam:::user/alice' },
      Action: ['s3:PutObject'],
      Resource: ['arn:aws:s3:::photos/uploads/*'],
    },
  ],
};

/** The same document as an S3 client sent it: compact, its keys in another order. */
const STORED_COMPACT =
  '{"Statement":[{"Resource":"arn:aws:s3:::photos/public/*","Action":"s3:GetObject","Principal":"*","Effect":"Allow","Sid":"Public"},' +
  '{"Resource":["arn:aws:s3:::photos/uploads/*"],"Action":["s3:PutObject"],"Principal":{"AWS":"arn:aws:iam:::user/alice"},"Effect":"Allow"}],' +
  '"Version":"2012-10-17"}';

const BUCKET_LABEL = 'Policy of bucket photos as JSON';
const SUB_LABEL = 'Policy of sub-bucket reports as JSON';
const NEW_SUB_LABEL = 'Policy of the sub-bucket as JSON (optional)';
const BUCKET_VALIDATION = '/buckets/photos/policy/validation';
const SUB_VALIDATION = '/buckets/photos/subbuckets/reports/policy/validation';
const UNCHANGED = 'Nothing has changed, so there is nothing to save.';

interface BucketNode {
  api: FakeApi;
  /** The document the bucket has now, as the API would answer it. */
  stored: () => unknown;
}

/**
 * The answers of a node with the builder for bucket photos: its policy
 * (`initial`, none when undefined), the catalogue, and validation by
 * `validate`. A PUT keeps the document sent; a DELETE removes it.
 */
function bucketNode(
  initial: unknown,
  validate: Handler = () => json(200, verdict()),
  more: Record<string, Handler> = {},
): BucketNode {
  let current = initial;
  const api = signedIn({
    'GET /policy-catalogue': () => json(200, CATALOGUE),
    'GET /buckets/photos/policy': () => json(200, current === undefined ? {} : { document: current }),
    'PUT /buckets/photos/policy': (call) => {
      current = (JSON.parse(call.body) as { document: unknown }).document;
      return json(200, { document: current });
    },
    'DELETE /buckets/photos/policy': () => {
      current = undefined;
      return noContent();
    },
    [`POST ${BUCKET_VALIDATION}`]: validate,
    ...more,
  });
  return { api, stored: () => current };
}

/**
 * Asks for the builder of a policy, as a user does, with "Open the policy
 * builder" beside its JSON - when it is still offered: the page's editors
 * share one catalogue. The catalogue is asked for then, never as a policy
 * is shown or focused.
 */
async function touch(label: string, within_: HTMLElement = document.body): Promise<HTMLTextAreaElement> {
  const box = (await within(within_).findByLabelText(label)) as HTMLTextAreaElement;
  const editor = box.closest('.policy-editor') as HTMLElement;
  const open = within(editor).queryByRole('button', { name: 'Open the policy builder' });
  if (open !== null) {
    fireEvent.click(open);
  }
  return box;
}

/** Opens the bucket's policy, touches it, and waits for the builder. */
async function openBucketPolicy(): Promise<HTMLElement> {
  render(<BucketPolicySection bucket="photos" />);
  fireEvent.click(screen.getByRole('button', { name: 'Bucket policy' }));
  await touch(BUCKET_LABEL);
  return screen.findByRole('group', { name: 'Policy builder' });
}

function jsonBox(label = BUCKET_LABEL): HTMLTextAreaElement {
  return screen.getByLabelText(label) as HTMLTextAreaElement;
}

function documentOf(label = BUCKET_LABEL): unknown {
  return JSON.parse(jsonBox(label).value);
}

function typeInto(element: HTMLElement, value: string) {
  fireEvent.change(element, { target: { value } });
}

function statement(n: number, within_: HTMLElement = document.body): HTMLElement {
  return within(within_).getByRole('group', { name: new RegExp(`^Statement ${String(n)}`) });
}

/** The document a put of a bucket's policy carried, as sent. */
function sentPolicy(call: Call | undefined): unknown {
  return (JSON.parse(call?.body ?? 'null') as { document?: unknown } | null)?.document;
}

/** Longer than a check waits for typing to pause: whatever would be checked has been. */
function pause(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, VALIDATION_DELAY_MS * 2));
}

/** The live region of the builder: what a screen reader hears of the check. */
function liveRegion(): HTMLElement {
  return screen.getAllByRole('status').find((region) => region.getAttribute('aria-live') === 'polite') as HTMLElement;
}

/** A policy open to anyone, as typed. */
const OPEN_TEXT =
  '{"Version":"2012-10-17","Statement":[{"Sid":"Public","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::photos/*"}]}';

/** Opens the bucket's policy on a node that gives no builder, and waits for the notice saying why and the stored text. */
async function openWithoutBuilder(notice: string): Promise<void> {
  render(<BucketPolicySection bucket="photos" />);
  fireEvent.click(screen.getByRole('button', { name: 'Bucket policy' }));
  await waitFor(() => {
    expect(jsonBox().value).toBe(formatPolicyDocument(BUCKET_DOC));
  });
  await touch(BUCKET_LABEL);
  expect(await screen.findByText(notice)).toBeDefined();
  expect(screen.queryByRole('group', { name: 'Policy builder' })).toBeNull();
}

describe("a bucket's policy with the builder", () => {
  it('opens a document stored through S3 in the builder, every principal in the form written, and checks nothing', async () => {
    const { api } = bucketNode(STORED_THROUGH_S3);
    await openBucketPolicy();

    const first = statement(1);
    expect(first.querySelector('legend')?.textContent).toBe('Statement 1: Public');
    const principals = within(first).getByRole('group', { name: 'Principals of statement 1' });
    expect(within(principals).getByText('*').closest('li')?.textContent).toContain('anyone');
    const second = within(statement(2)).getByRole('group', { name: 'Principals of statement 2' });
    expect(within(second).getByText('arn:aws:iam:::user/alice').closest('li')?.textContent).toContain('user alice');
    expect(jsonBox().value).toBe(formatPolicyDocument(STORED_THROUGH_S3));

    // Opening is not a change: nothing is checked (a refusal would be audited for nothing).
    await pause();
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
    // The policy names this bucket only: no bucket is listed to offer.
    expect(api.callsTo('GET', '/buckets')).toHaveLength(0);
  });

  it('does not send a document stored through S3 again when it is saved unchanged in meaning, however it is laid out', async () => {
    const { api, stored } = bucketNode(STORED_THROUGH_S3);
    await openBucketPolicy();

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText(UNCHANGED)).toBeDefined();

    // Laid out as the S3 client sent it - compact, in another order - it is the same document.
    typeInto(jsonBox(), STORED_COMPACT);
    expect(screen.queryByText(UNCHANGED)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText(UNCHANGED)).toBeDefined();

    await pause();
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);
    // So what S3's GetBucketPolicy answers is still the document as stored.
    expect(stored()).toBe(STORED_THROUGH_S3);
  });

  it('keeps every principal in the form written when the builder changes another field', async () => {
    const { api } = bucketNode(STORED_THROUGH_S3);
    await openBucketPolicy();

    // The user's statement becomes a Deny; nothing else of the document changes.
    fireEvent.change(within(statement(2)).getByRole('combobox', { name: 'Effect of statement 2' }), {
      target: { value: 'Deny' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    const sent = sentPolicy(api.callsTo('PUT', '/buckets/photos/policy')[0]) as typeof STORED_THROUGH_S3;
    expect(sent).toEqual({
      ...STORED_THROUGH_S3,
      Statement: [STORED_THROUGH_S3.Statement[0], { ...STORED_THROUGH_S3.Statement[1], Effect: 'Deny' }],
    });
    // "*" is still a string, and the one user still a single string under AWS.
    expect(sent.Statement[0]?.Principal).toBe('*');
    expect(sent.Statement[1]?.Principal).toEqual({ AWS: 'arn:aws:iam:::user/alice' });
  });

  it('makes a policy for a bucket that has none: users or anyone, the actions a bucket policy may allow, and this bucket', async () => {
    const { api } = bucketNode(undefined, () => json(200, verdict([], [analysis(0)])));
    const pane = await openBucketPolicy();
    expect(screen.getByText(/This bucket has no policy/)).toBeDefined();
    // Nothing to remove.
    expect(screen.queryByRole('button', { name: 'Remove policy' })).toBeNull();

    fireEvent.click(within(pane).getByRole('button', { name: 'Add a statement' }));
    const first = statement(1);
    const principals = within(first).getByRole('group', { name: 'Principals of statement 1' });
    typeInto(within(principals).getByRole('textbox', { name: 'User name for statement 1' }), 'alice');
    fireEvent.click(within(principals).getByRole('button', { name: 'Add user to statement 1' }));
    const anyone = within(principals).getByRole('button', { name: 'Add anyone to statement 1' }) as HTMLButtonElement;
    fireEvent.click(anyone);
    // Anyone is there once: it is not offered again.
    expect(anyone.disabled).toBe(true);

    // Only the actions a bucket policy may allow are offered (in_bucket_policy).
    const actions = within(first).getByRole('group', { name: 'Actions of statement 1' });
    const offered = within(actions)
      .getAllByRole('checkbox')
      .map((box) => box.id.slice(box.id.lastIndexOf('-') + 1));
    expect(offered).toEqual(['s3:GetObject', 's3:PutObject', 's3:ListBucket', 's3:PutBucketTagging']);
    fireEvent.click(within(actions).getByRole('checkbox', { name: 's3:GetObject' }));

    // The resources are this bucket's: no bucket to type, no administrative resource.
    const resources = within(first).getByRole('group', { name: 'Resources of statement 1' });
    expect(within(resources).queryByRole('combobox', { name: 'Bucket for statement 1' })).toBeNull();
    expect(within(resources).queryByRole('combobox', { name: 'Admin resource for statement 1' })).toBeNull();
    typeInto(within(resources).getByRole('textbox', { name: 'Prefix (optional) for statement 1' }), 'public/');
    fireEvent.click(within(resources).getByRole('button', { name: 'Add objects to statement 1' }));
    fireEvent.click(within(resources).getByRole('button', { name: 'Add bucket to statement 1' }));

    expect(documentOf()).toEqual({
      Version: '2012-10-17',
      Statement: [
        {
          Effect: 'Allow',
          Principal: { AWS: ['arn:aws:iam:::user/alice', '*'] },
          Action: ['s3:GetObject'],
          Resource: ['arn:aws:s3:::photos/public/*', 'arn:aws:s3:::photos'],
        },
      ],
    });

    // A principal is removed by its own button.
    fireEvent.click(within(principals).getByRole('button', { name: 'Remove principal * from statement 1' }));
    expect((documentOf() as { Statement: { Principal: unknown }[] }).Statement[0]?.Principal).toEqual({
      AWS: ['arn:aws:iam:::user/alice'],
    });
    expect(anyone.disabled).toBe(false);

    // Checked where a bucket's policy is checked, as typed, in the body of its put.
    await waitFor(() => {
      expect(api.callsTo('POST', BUCKET_VALIDATION).length).toBeGreaterThan(0);
    });
    const checks = api.callsTo('POST', BUCKET_VALIDATION);
    const last = checks[checks.length - 1];
    expect(last?.body).toBe(`{"document":${jsonBox().value}}`);
    expect(last?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    // Not open to anyone, as the node says: no question was asked.
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(api.callsTo('PUT', '/buckets/photos/policy')[0]?.body).toBe(last?.body);
  });

  it('says what the node says anyone could do before a policy open to anyone is saved, and saves it once confirmed', async () => {
    const { api } = bucketNode(undefined, () => json(200, verdict([], [OPEN])));
    await openBucketPolicy();
    const typed = JSON.stringify(
      {
        Version: '2012-10-17',
        Statement: [
          {
            Sid: 'Public',
            Effect: 'Allow',
            Principal: '*',
            Action: ['s3:GetObject', 's3:PutObject'],
            Resource: 'arn:aws:s3:::photos/public/*',
          },
        ],
      },
      null,
      2,
    );
    typeInto(jsonBox(), typed);
    // As typed, the node's reading is shown beside the principals.
    expect(await screen.findByText('The node reads this statement as open to anyone.')).toBeDefined();

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    const warning = await screen.findByRole('alertdialog', { name: 'This policy opens bucket photos to anyone' });
    // It names what anyone could do, as the node's analysis gives it, and where.
    expect(warning.textContent).toContain(
      'Statement 1 (Public), on arn:aws:s3:::photos/public/*: anyone, signed in or not, could s3:GetObject.',
    );
    // And what it names for anyone that is never anonymous: every user, and nobody who has not signed in.
    expect(warning.textContent).toContain(
      'It also names for anyone s3:PutObject, never allowed without signing in: every user of this deployment could do it, and nobody who has not signed in.',
    );
    expect(document.getElementById(warning.getAttribute('aria-describedby') ?? '')?.textContent).toContain(
      'before their conditions',
    );
    // It takes the keyboard, so that it is read first.
    expect(document.activeElement).toBe(warning);
    // Nothing is sent until it is answered.
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);

    fireEvent.click(within(warning).getByRole('button', { name: 'Save, open to anyone' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    const put = api.callsTo('PUT', '/buckets/photos/policy')[0];
    // What is sent is byte for byte what the node checked.
    const checks = api.callsTo('POST', BUCKET_VALIDATION);
    expect(put?.body).toBe(`{"document":${typed}}`);
    expect(checks[checks.length - 1]?.body).toBe(put?.body);
    expect(put?.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('sends nothing when the warning is cancelled, and gives the keyboard back to Save', async () => {
    const { api } = bucketNode(undefined, () => json(200, verdict([], [OPEN])));
    await openBucketPolicy();
    typeInto(
      jsonBox(),
      '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}',
    );

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    const warning = await screen.findByRole('alertdialog');
    fireEvent.click(within(warning).getByRole('button', { name: 'Cancel' }));

    expect(screen.queryByRole('alertdialog')).toBeNull();
    // The question may be shown before the run that asked it has ended, while
    // Save is still disabled: the keyboard goes to Save once it can take it.
    await waitFor(() => {
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Save policy' }));
    });
    await pause();
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);

    // Asked again, and the document changed meanwhile: the question was about the text it named.
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    await screen.findByRole('alertdialog');
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[]}');
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
  });

  it('names for a statement with no action the node knows that it names none, as the node says', async () => {
    bucketNode(undefined, () => json(200, verdict([], [analysis(0, { open_to_anyone: true, unmatched_patterns: ['s3:Teleport'] })])));
    await openBucketPolicy();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:Teleport","Resource":"*"}]}');
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    const warning = await screen.findByRole('alertdialog');
    expect(warning.textContent).toContain('Statement 1, on *: it names for anyone no action this node knows.');
  });

  it('sends nothing the node finds a fault in, and announces the faults', async () => {
    const faults: PolicyFault[] = [
      { pointer: '/Statement/0/Principal', statement: 0, reason: 'bucket policy: statement 0: no Principal' },
    ];
    const { api } = bucketNode(undefined, () => json(200, verdict(faults)));
    await openBucketPolicy();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}');

    const principals = within(statement(1)).getByRole('group', { name: 'Principals of statement 1' });
    expect(await within(principals).findByText(/Fault: bucket policy: statement 0: no Principal/)).toBeDefined();
    expect(liveRegion().textContent).toBe(
      '1 fault in the policy. The first: Statement 1, at /Statement/0/Principal: bucket policy: statement 0: no Principal',
    );
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText(NOT_SAVED_NOTICE)).toBeDefined();
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('shows the faults of a refused save at their fields', async () => {
    const faults: PolicyFault[] = [{ pointer: '/Statement/0/Action', statement: 0, reason: 'bucket policy: statement 0: refused' }];
    const { api } = bucketNode(undefined, () => json(200, verdict()), {
      'PUT /buckets/photos/policy': () =>
        new Response(
          JSON.stringify({ type: 'about:blank', title: 'Invalid policy', status: 400, code: 'InvalidPolicy', detail: 'refused', faults }),
          { status: 400, headers: { 'Content-Type': 'application/problem+json' } },
        ),
    });
    await openBucketPolicy();
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}]}');
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    const actions = within(statement(1)).getByRole('group', { name: 'Actions of statement 1' });
    expect(await within(actions).findByText(/Fault: bucket policy: statement 0: refused/)).toBeDefined();
  });

  it('removes the policy only once the question is answered yes', async () => {
    const { api, stored } = bucketNode(STORED_THROUGH_S3);
    await openBucketPolicy();

    fireEvent.click(screen.getByRole('button', { name: 'Remove policy' }));
    const question = screen.getByRole('group', { name: 'Remove the policy of bucket photos?' });
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('DELETE', '/buckets/photos/policy')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Remove policy' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, remove policy' }));
    await waitFor(() => {
      expect(api.callsTo('DELETE', '/buckets/photos/policy')).toHaveLength(1);
    });
    expect(stored()).toBeUndefined();
    expect(await screen.findByText(/This bucket has no policy/)).toBeDefined();
    expect(jsonBox().value).toBe('');
    // Removing is not a document: nothing was checked.
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);
  });

  it('asks, on a node of an older release, before saving a policy that node could not check', async () => {
    const { api } = bucketNode(BUCKET_DOC, () => refusal(404, 'NotFound', 'Not found'), {
      'GET /policy-catalogue': () => refusal(404, 'NotFound', 'Not found'),
    });
    await openWithoutBuilder(OLDER_NODE_NOTICE);
    typeInto(jsonBox(), OPEN_TEXT);
    await pause();
    // Nothing is checked as it is typed; the save asks the node once, and its 404 is asked about.
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);
    await expectUncheckedQuestion(api);
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(2);
    expect(api.callsTo('GET', '/buckets')).toHaveLength(0);
  });

  it('warns, on a node whose catalogue answers 404 but whose validation answers, as the validation says', async () => {
    const { api } = bucketNode(BUCKET_DOC, () => json(200, verdict([], [OPEN])), {
      'GET /policy-catalogue': () => refusal(404, 'NotFound', 'Not found'),
    });
    await openWithoutBuilder(OLDER_NODE_NOTICE);
    typeInto(jsonBox(), OPEN_TEXT);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByRole('alertdialog', { name: 'This policy opens bucket photos to anyone' })).toBeDefined();
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
  });

  it('does not wait for a catalogue that never answers: after the wait it saves as without the builder, checked', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { api } = bucketNode(BUCKET_DOC, () => json(200, verdict([], [OPEN])));
      holdRequests('/policy-catalogue', () => undefined);
      render(<BucketPolicySection bucket="photos" />);
      fireEvent.click(screen.getByRole('button', { name: 'Bucket policy' }));
      await waitFor(() => {
        expect(jsonBox().value).toBe(formatPolicyDocument(BUCKET_DOC));
      });
      typeInto(jsonBox(), OPEN_TEXT);
      fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
      expect((screen.getByRole('button', { name: 'Save policy' }) as HTMLButtonElement).disabled).toBe(true);
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);

      vi.advanceTimersByTime(CATALOGUE_WAIT_MS);
      // Taken as failed: the notice says so, the save is checked once, and the node's answer is asked about.
      expect(await screen.findByText(CATALOGUE_FAILED_NOTICE)).toBeDefined();
      expect(await screen.findByRole('alertdialog', { name: 'This policy opens bucket photos to anyone' })).toBeDefined();
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(1);
      expect((screen.getByRole('button', { name: 'Save policy' }) as HTMLButtonElement).disabled).toBe(false);
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });

  describe.each([
    ['a caller refused the catalogue (403)', () => refusal(403, 'AccessDenied', 'Access denied'), CATALOGUE_REFUSED_NOTICE],
    ['a catalogue that cannot be read', () => refusal(500, 'InternalError', 'Internal error'), CATALOGUE_FAILED_NOTICE],
  ])('without the builder for %s', (_, catalogue, notice) => {
    it('checks nothing as it is opened or typed, and warns before a policy open to anyone is saved', async () => {
      const { api } = bucketNode(BUCKET_DOC, () => json(200, verdict([], [OPEN])), { 'GET /policy-catalogue': catalogue });
      await openWithoutBuilder(notice);
      await pause();
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);
      typeInto(jsonBox(), OPEN_TEXT);
      await pause();
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);

      // Saved, it is checked once, in the body of the put; the node says it is open, and the console asks.
      fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
      const warning = await screen.findByRole('alertdialog', { name: 'This policy opens bucket photos to anyone' });
      expect(warning.textContent).toContain('anyone, signed in or not, could s3:GetObject.');
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(1);
      expect(api.callsTo('POST', BUCKET_VALIDATION)[0]?.body).toBe(`{"document":${OPEN_TEXT}}`);

      // Cancelled, nothing is sent.
      fireEvent.click(within(warning).getByRole('button', { name: 'Cancel' }));
      await pause();
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);

      // Confirmed, the bytes that were checked are sent.
      fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
      fireEvent.click(
        within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Save, open to anyone' }),
      );
      await waitFor(() => {
        expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
      });
      const checks = api.callsTo('POST', BUCKET_VALIDATION);
      expect(api.callsTo('PUT', '/buckets/photos/policy')[0]?.body).toBe(checks[checks.length - 1]?.body);
      expect(api.callsTo('GET', '/buckets')).toHaveLength(0);
    });

    it('sends nothing the save-time check finds a fault in, and shows the faults', async () => {
      const faults: PolicyFault[] = [{ pointer: '/Statement/0/Action', statement: 0, reason: 'bucket policy: statement 0: bad' }];
      const { api } = bucketNode(BUCKET_DOC, () => json(200, verdict(faults)), { 'GET /policy-catalogue': catalogue });
      await openWithoutBuilder(notice);
      typeInto(jsonBox(), OPEN_TEXT);
      fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));

      expect(await screen.findByText(NOT_SAVED_NOTICE)).toBeDefined();
      expect(screen.getByText(/Statement 1, at \/Statement\/0\/Action, line 1: bucket policy: statement 0: bad/)).toBeDefined();
      expect(liveRegion().textContent).toContain('1 fault in the policy.');
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
    });

    it('asks before saving a policy the node could not check (validation 404): cancel sends nothing, confirm sends it', async () => {
      const { api } = bucketNode(BUCKET_DOC, () => refusal(404, 'NotFound', 'Not found'), { 'GET /policy-catalogue': catalogue });
      await openWithoutBuilder(notice);
      typeInto(jsonBox(), OPEN_TEXT);
      await expectUncheckedQuestion(api);
    });
  });

  /**
   * That a save the node could not check (404) is asked about in fixed
   * words, that Cancel sends nothing and that confirming sends the text.
   */
  async function expectUncheckedQuestion(api: FakeApi): Promise<void> {
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    let question = await screen.findByRole('alertdialog', { name: 'This node could not check the policy' });
    expect(question.textContent).toContain(
      'This node could not check the policy, so it cannot say whether it opens bucket photos to anyone. Save it as it is?',
    );
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    await pause();
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    question = await screen.findByRole('alertdialog', { name: 'This node could not check the policy' });
    fireEvent.click(within(question).getByRole('button', { name: 'Save as it is' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    expect(api.callsTo('PUT', '/buckets/photos/policy')[0]?.body).toBe(`{"document":${OPEN_TEXT}}`);
  }

  it('asks before saving a policy the node could not check, with the builder, and checks the next text again', async () => {
    let answers = 0;
    const { api } = bucketNode(undefined, () => {
      answers += 1;
      // A node of an older release answers the first check; one of this release the rest.
      return answers === 1 ? refusal(404, 'NotFound', 'Not found') : json(200, verdict([], [analysis(0)]));
    });
    await openBucketPolicy();
    typeInto(jsonBox(), OPEN_TEXT);
    await waitFor(() => {
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(1);
    });
    expect(await screen.findByText(NO_VALIDATION_NOTICE)).toBeDefined();
    // The answer was for that text: the same text is not asked again, and the save asks.
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    const question = await screen.findByRole('alertdialog', { name: 'This node could not check the policy' });
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(1);

    // Another text is checked again: one node of an older release does not turn validation off.
    const next = OPEN_TEXT.replace('"Sid":"Public"', '"Sid":"Public2"');
    typeInto(jsonBox(), next);
    await waitFor(() => {
      expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(2);
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(api.callsTo('PUT', '/buckets/photos/policy')[0]?.body).toBe(`{"document":${next}}`);
  });

  it('asks about a policy the node could not check, with the builder, and sends it once confirmed', async () => {
    const { api } = bucketNode(undefined, () => refusal(404, 'NotFound', 'Not found'));
    await openBucketPolicy();
    typeInto(jsonBox(), OPEN_TEXT);
    await expectUncheckedQuestion(api);
  });

  it('sends nothing when the node fails to check the policy as it is saved (5xx)', async () => {
    const { api } = bucketNode(undefined, () => refusal(503, 'Unavailable', 'Temporarily unavailable'));
    await openBucketPolicy();
    typeInto(jsonBox(), OPEN_TEXT);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Temporarily unavailable');
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('sends nothing when the policy is changed while it is being checked', async () => {
    const { api } = bucketNode(BUCKET_DOC, () => json(200, verdict()), {
      'GET /policy-catalogue': () => refusal(403, 'AccessDenied', 'Access denied'),
    });
    await openWithoutBuilder(CATALOGUE_REFUSED_NOTICE);
    typeInto(jsonBox(), OPEN_TEXT);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    // The check is in flight: the text changes under it.
    typeInto(jsonBox(), '{"Version":"2012-10-17","Statement":[]}');
    expect(await screen.findByText(CHANGED_WHILE_CHECKED)).toBeDefined();
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(1);
    expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(0);
  });

  it('says "every user" when no open statement lets anyone in without signing in', async () => {
    bucketNode(undefined, () =>
      json(200, verdict([], [analysis(0, { open_to_anyone: true, actions: ['s3:PutObject'], never_anonymous: ['s3:PutObject'] })])),
    );
    await openBucketPolicy();
    typeInto(jsonBox(), OPEN_TEXT);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(
      await screen.findByRole('alertdialog', { name: 'This policy opens bucket photos to every user of this deployment' }),
    ).toBeDefined();
  });

  it('cancels the question on Escape, and gives the keyboard to the status line once confirmed', async () => {
    const { api } = bucketNode(undefined, () => json(200, verdict([], [OPEN])));
    await openBucketPolicy();
    typeInto(jsonBox(), OPEN_TEXT);
    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    fireEvent.keyDown(await screen.findByRole('alertdialog'), { key: 'Escape' });
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Save policy' }));

    fireEvent.click(screen.getByRole('button', { name: 'Save policy' }));
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Save, open to anyone' }));
    const status = document.activeElement as HTMLElement;
    expect(status.getAttribute('role')).toBe('status');
    expect(status.textContent).toBe('Saving the policy…');
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/policy')).toHaveLength(1);
    });
  });

  it('offers every s3: action to a Deny, and to an Allow only what a bucket policy may allow', async () => {
    bucketNode(undefined);
    const pane = await openBucketPolicy();
    fireEvent.click(within(pane).getByRole('button', { name: 'Add a statement' }));
    const offered = () =>
      within(within(statement(1)).getByRole('group', { name: 'Actions of statement 1' }))
        .getAllByRole('checkbox')
        .map((box) => box.id.slice(box.id.lastIndexOf('-') + 1));
    expect(offered()).not.toContain('admin:GetUser');
    fireEvent.change(within(statement(1)).getByRole('combobox', { name: 'Effect of statement 1' }), { target: { value: 'Deny' } });
    expect(offered()).toEqual(['s3:GetObject', 's3:PutObject', 's3:ListBucket', 's3:PutBucketTagging', 's3:BypassGovernanceRetention']);
  });

  it('names every control of a principal for a screen reader', async () => {
    bucketNode(STORED_THROUGH_S3);
    const pane = await openBucketPolicy();
    const principals = within(pane).getByRole('group', { name: 'Principals of statement 1' });
    for (const role of ['button', 'textbox']) {
      const all = within(principals).getAllByRole(role);
      expect(all.length).toBeGreaterThan(0);
      expect(within(principals).getAllByRole(role, { name: /\S/ })).toHaveLength(all.length);
    }
    for (const control of within(principals).getAllByRole('button')) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
    // Enter in the user name adds the user, and does not send the form.
    const user = within(principals).getByRole('textbox', { name: 'User name for statement 1' });
    typeInto(user, 'bob');
    fireEvent.keyDown(user, { key: 'Enter' });
    expect(within(principals).getByText('arn:aws:iam:::user/bob')).toBeDefined();
  });
});

/** A small bucket policy for the fallbacks. */
const BUCKET_DOC = {
  Version: '2012-10-17',
  Statement: [{ Effect: 'Allow', Principal: { AWS: ['arn:aws:iam:::user/bob'] }, Action: 's3:GetObject', Resource: 'arn:aws:s3:::photos/*' }],
};

/** The policy of sub-bucket reports the tests begin with. */
const SUB_POLICY = {
  Version: '2012-10-17',
  Statement: [
    {
      Effect: 'Allow',
      Principal: { AWS: 'arn:aws:iam:::user/carol' },
      Action: 's3:GetObject',
      Resource: 'arn:aws:s3:::photos/reports/*',
    },
  ],
};

/** A node with bucket photos and its sub-bucket reports, whose policy the test changes. */
function subNode(validate: Handler = () => json(200, verdict()), more: Record<string, Handler> = {}): FakeApi {
  let policy: unknown = SUB_POLICY;
  return signedIn({
    'GET /policy-catalogue': () => json(200, CATALOGUE),
    'GET /buckets/photos/subbuckets': () => json(200, [{ name: 'reports', created_at: CREATED }]),
    'GET /buckets/photos/subbuckets/reports': () =>
      json(200, policy === undefined ? { name: 'reports', created_at: CREATED } : { name: 'reports', created_at: CREATED, policy }),
    'PUT /buckets/photos/subbuckets/reports': (call) => {
      policy = (JSON.parse(call.body) as { policy?: unknown }).policy;
      return json(200, policy === undefined ? { name: 'reports', created_at: CREATED } : { name: 'reports', created_at: CREATED, policy });
    },
    [`POST ${SUB_VALIDATION}`]: validate,
    ...more,
  });
}

/** Opens sub-bucket reports and waits for its builder. */
async function openReports(): Promise<HTMLElement> {
  render(<SubBucketsSection bucket="photos" />);
  fireEvent.click(screen.getByRole('button', { name: 'Sub-buckets' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
  const region = await screen.findByRole('region', { name: 'Sub-bucket reports' });
  await waitFor(() => {
    expect(jsonBox(SUB_LABEL).value).not.toBe('');
  });
  await touch(SUB_LABEL, region);
  await within(region).findByRole('group', { name: 'Policy builder' });
  return region;
}

describe("the catalogue of a bucket's page", () => {
  it('is not asked for while the policies are only viewed, and is asked once for the page when one is first touched', async () => {
    const api = subNode(() => json(200, verdict()), {
      'GET /buckets/photos/policy': () => json(200, { document: BUCKET_DOC }),
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
    fireEvent.click(screen.getByRole('button', { name: 'Bucket policy' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sub-buckets' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = await screen.findByRole('region', { name: 'Sub-bucket reports' });
    await waitFor(() => {
      expect(jsonBox(SUB_LABEL).value).toBe(formatPolicyDocument(SUB_POLICY));
      expect(jsonBox().value).toBe(formatPolicyDocument(BUCKET_DOC));
    });

    // Viewing every policy of the page asks nothing of the catalogue: a
    // caller without admin:GetPolicyCatalogue (read-write, read-only) is
    // refused nothing, and nothing is audited.
    // Nor does focusing them: a reader tabbing through is refused nothing.
    fireEvent.focus(jsonBox());
    fireEvent.focus(jsonBox(SUB_LABEL));
    await pause();
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    expect(screen.queryByRole('group', { name: 'Policy builder' })).toBeNull();

    // Touched, it is read once, for every editor of the page.
    await touch(BUCKET_LABEL);
    expect(await within(region).findByRole('group', { name: 'Policy builder' })).toBeDefined();
    await touch(SUB_LABEL, region);
    await touch(NEW_SUB_LABEL);
    expect(screen.getAllByRole('group', { name: 'Policy builder' })).toHaveLength(3);
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
  });

  it('is asked for by the first edit of a policy, too', async () => {
    const { api } = bucketNode(BUCKET_DOC);
    render(<BucketPolicySection bucket="photos" />);
    fireEvent.click(screen.getByRole('button', { name: 'Bucket policy' }));
    await waitFor(() => {
      expect(jsonBox().value).toBe(formatPolicyDocument(BUCKET_DOC));
    });
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(0);
    typeInto(jsonBox(), OPEN_TEXT);
    expect(await screen.findByRole('group', { name: 'Policy builder' })).toBeDefined();
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
  });
});

describe("a sub-bucket's policy with the builder", () => {
  it('opens in the builder, checks nothing, and offers only what a sub-bucket may hold', async () => {
    const api = subNode();
    const region = await openReports();
    await pause();
    expect(api.callsTo('POST', SUB_VALIDATION)).toHaveLength(0);
    // The catalogue is read once for the section, and no bucket is listed.
    expect(api.callsTo('GET', '/policy-catalogue')).toHaveLength(1);
    expect(api.callsTo('GET', '/buckets')).toHaveLength(0);

    const first = statement(1, region);
    const principals = within(first).getByRole('group', { name: 'Principals of statement 1' });
    expect(within(principals).getByText('arn:aws:iam:::user/carol').closest('li')?.textContent).toContain('user carol');
    // A sub-bucket's policy never allows anything to anyone: not offered to an Allow.
    expect(within(principals).queryByRole('button', { name: 'Add anyone to statement 1' })).toBeNull();

    // Only the actions the catalogue says a sub-bucket's policy may name, by their full names.
    const actions = within(first).getByRole('group', { name: 'Actions of statement 1' });
    const offered = within(actions)
      .getAllByRole('checkbox')
      .map((box) => box.id.slice(box.id.lastIndexOf('-') + 1));
    expect(offered).toEqual(['s3:GetObject', 's3:PutObject', 's3:ListBucket']);
    expect(within(actions).queryByRole('textbox', { name: 'Action pattern for statement 1' })).toBeNull();

    // Its keys, or the bucket for listing: nothing else is offered.
    const resources = within(first).getByRole('group', { name: 'Resources of statement 1' });
    expect(within(resources).queryByRole('combobox', { name: 'Bucket for statement 1' })).toBeNull();
    expect(within(resources).queryByRole('combobox', { name: 'Admin resource for statement 1' })).toBeNull();
    expect(within(resources).getByText('Prefix after reports/ (optional)')).toBeDefined();
    typeInto(within(resources).getByRole('textbox', { name: 'Prefix (optional) for statement 1' }), '2026/');
    fireEvent.click(within(resources).getByRole('button', { name: 'Add objects to statement 1' }));
    fireEvent.click(within(resources).getByRole('button', { name: 'Add bucket to statement 1' }));
    expect((documentOf(SUB_LABEL) as typeof SUB_POLICY).Statement[0]?.Resource).toEqual([
      'arn:aws:s3:::photos/reports/*',
      'arn:aws:s3:::photos/reports/2026/*',
      'arn:aws:s3:::photos',
    ]);
  });

  it('offers anyone to a Deny only', async () => {
    subNode();
    const region = await openReports();
    fireEvent.change(within(statement(1, region)).getByRole('combobox', { name: 'Effect of statement 1' }), {
      target: { value: 'Deny' },
    });
    const principals = within(statement(1, region)).getByRole('group', { name: 'Principals of statement 1' });
    fireEvent.click(within(principals).getByRole('button', { name: 'Add anyone to statement 1' }));
    expect((documentOf(SUB_LABEL) as { Statement: { Principal: unknown }[] }).Statement[0]?.Principal).toEqual({
      AWS: ['arn:aws:iam:::user/carol', '*'],
    });
  });

  it('is checked as the policy of the sub-bucket once changed, and sent as checked', async () => {
    const api = subNode();
    const region = await openReports();
    fireEvent.click(within(statement(1, region)).getByRole('checkbox', { name: 's3:PutObject' }));
    expect((documentOf(SUB_LABEL) as { Statement: { Action: unknown }[] }).Statement[0]?.Action).toEqual([
      's3:GetObject',
      's3:PutObject',
    ]);

    await waitFor(() => {
      expect(api.callsTo('POST', SUB_VALIDATION).length).toBeGreaterThan(0);
    });
    const checks = api.callsTo('POST', SUB_VALIDATION);
    expect(checks[checks.length - 1]?.body).toBe(`{"policy":${jsonBox(SUB_LABEL).value}}`);
    expect(api.callsTo('POST', '/policy-validation')).toHaveLength(0);
    expect(api.callsTo('POST', BUCKET_VALIDATION)).toHaveLength(0);

    fireEvent.click(within(region).getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(1);
    });
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')[0]?.body).toBe(checks[checks.length - 1]?.body);
  });

  it('is not sent unchanged in meaning', async () => {
    const api = subNode();
    const region = await openReports();
    typeInto(jsonBox(SUB_LABEL), JSON.stringify(SUB_POLICY));
    fireEvent.click(within(region).getByRole('button', { name: 'Save policy' }));
    expect(await within(region).findByText(UNCHANGED)).toBeDefined();
    await pause();
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(0);
    expect(api.callsTo('POST', SUB_VALIDATION)).toHaveLength(0);
  });

  it('is removed only once the question is answered yes, by a PUT without a policy', async () => {
    const api = subNode();
    const region = await openReports();
    fireEvent.click(within(region).getByRole('button', { name: 'Remove policy' }));
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(0);
    fireEvent.click(within(region).getByRole('button', { name: 'Yes, remove policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(1);
    });
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')[0]?.body).toBe('{}');
    expect(api.callsTo('POST', SUB_VALIDATION)).toHaveLength(0);
  });

  it('is made with the builder when a sub-bucket is created, and checked for the name given', async () => {
    const api = subNode(() => json(200, verdict()), {
      'GET /buckets/photos/subbuckets': () => json(200, []),
      'POST /buckets/photos/subbuckets/scans/policy/validation': () => json(200, verdict()),
      'PUT /buckets/photos/subbuckets/scans': () => json(200, { name: 'scans', created_at: CREATED }),
    });
    render(<SubBucketsSection bucket="photos" />);
    fireEvent.click(screen.getByRole('button', { name: 'Sub-buckets' }));
    const create = (await screen.findByRole('heading', { name: 'Create a sub-bucket' })).closest('section') as HTMLElement;
    await touch(NEW_SUB_LABEL, create);
    const pane = await within(create).findByRole('group', { name: 'Policy builder' });

    // Not named yet: there are no keys to offer.
    fireEvent.click(within(pane).getByRole('button', { name: 'Add a statement' }));
    const resources = within(statement(1, create)).getByRole('group', { name: 'Resources of statement 1' });
    expect((within(resources).getByRole('button', { name: 'Add objects to statement 1' }) as HTMLButtonElement).disabled).toBe(true);

    typeInto(within(create).getByLabelText('Sub-bucket name'), 'scans');
    typeInto(within(statement(1, create)).getByRole('textbox', { name: 'User name for statement 1' }), 'dave');
    fireEvent.click(within(statement(1, create)).getByRole('button', { name: 'Add user to statement 1' }));
    fireEvent.click(within(statement(1, create)).getByRole('checkbox', { name: 's3:GetObject' }));
    fireEvent.click(within(resources).getByRole('button', { name: 'Add objects to statement 1' }));
    expect(documentOf(NEW_SUB_LABEL)).toEqual({
      Version: '2012-10-17',
      Statement: [
        {
          Effect: 'Allow',
          Principal: { AWS: ['arn:aws:iam:::user/dave'] },
          Action: ['s3:GetObject'],
          Resource: ['arn:aws:s3:::photos/scans/*'],
        },
      ],
    });
    const text = jsonBox(NEW_SUB_LABEL).value;

    await waitFor(() => {
      expect(api.callsTo('POST', '/buckets/photos/subbuckets/scans/policy/validation').length).toBeGreaterThan(0);
    });
    fireEvent.click(within(create).getByRole('button', { name: 'Create sub-bucket' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')).toHaveLength(1);
    });
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')[0]?.body).toBe(`{"policy":${text}}`);
  });

  it('creates nothing whose policy the node finds a fault in', async () => {
    const faults: PolicyFault[] = [{ pointer: '/Statement/0/Action', statement: 0, reason: 'sub-bucket policy: statement 0: no' }];
    const api = subNode(() => json(200, verdict()), {
      'GET /buckets/photos/subbuckets': () => json(200, []),
      'POST /buckets/photos/subbuckets/scans/policy/validation': () => json(200, verdict(faults)),
      'PUT /buckets/photos/subbuckets/scans': () => json(200, { name: 'scans', created_at: CREATED }),
    });
    render(<SubBucketsSection bucket="photos" />);
    fireEvent.click(screen.getByRole('button', { name: 'Sub-buckets' }));
    const create = (await screen.findByRole('heading', { name: 'Create a sub-bucket' })).closest('section') as HTMLElement;
    await touch(NEW_SUB_LABEL, create);
    await within(create).findByRole('group', { name: 'Policy builder' });
    typeInto(within(create).getByLabelText('Sub-bucket name'), 'scans');
    typeInto(jsonBox(NEW_SUB_LABEL), '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}');

    fireEvent.click(within(create).getByRole('button', { name: 'Create sub-bucket' }));
    expect(await within(create).findByText(NOT_SAVED_NOTICE)).toBeDefined();
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')).toHaveLength(0);
  });

  it('is saved without a question when the node cannot check it (404): a sub-bucket policy can open nothing to anyone', async () => {
    const api = subNode(() => refusal(404, 'NotFound', 'Not found'));
    const region = await openReports();
    typeInto(jsonBox(SUB_LABEL), '{"Version":"2012-10-17","Statement":[]}');
    fireEvent.click(within(region).getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(1);
    });
    expect(api.callsTo('POST', SUB_VALIDATION).length).toBeGreaterThan(0);
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it.each([
    ['refused (403)', () => refusal(403, 'AccessDenied', 'Access denied'), CATALOGUE_REFUSED_NOTICE],
    ['failing', () => refusal(500, 'InternalError', 'Internal error'), CATALOGUE_FAILED_NOTICE],
  ])('creates a sub-bucket with the catalogue %s by checking its policy once, as it is created', async (_, catalogue, notice) => {
    const typed = '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}';
    let faulty = true;
    const api = subNode(() => json(200, verdict()), {
      'GET /policy-catalogue': catalogue,
      'GET /buckets/photos/subbuckets': () => json(200, []),
      'POST /buckets/photos/subbuckets/scans/policy/validation': () =>
        json(200, faulty ? verdict([{ pointer: '/Statement/0/Action', statement: 0, reason: 'sub-bucket policy: statement 0: no' }]) : verdict()),
      'PUT /buckets/photos/subbuckets/scans': () => json(200, { name: 'scans', created_at: CREATED }),
    });
    render(<SubBucketsSection bucket="photos" />);
    fireEvent.click(screen.getByRole('button', { name: 'Sub-buckets' }));
    const create = (await screen.findByRole('heading', { name: 'Create a sub-bucket' })).closest('section') as HTMLElement;
    await touch(NEW_SUB_LABEL, create);
    expect(await within(create).findByText(notice)).toBeDefined();
    typeInto(within(create).getByLabelText('Sub-bucket name'), 'scans');
    typeInto(jsonBox(NEW_SUB_LABEL), typed);
    await pause();
    // Nothing is checked as it is typed without the builder.
    expect(api.callsTo('POST', '/buckets/photos/subbuckets/scans/policy/validation')).toHaveLength(0);

    fireEvent.click(within(create).getByRole('button', { name: 'Create sub-bucket' }));
    expect(await within(create).findByText(NOT_SAVED_NOTICE)).toBeDefined();
    // At the JSON, and announced.
    expect(within(create).getByText(/Statement 1, at \/Statement\/0\/Action, line 1: sub-bucket policy: statement 0: no/)).toBeDefined();
    expect(liveRegion().textContent).toContain('1 fault in the policy.');
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')).toHaveLength(0);

    faulty = false;
    fireEvent.click(within(create).getByRole('button', { name: 'Create sub-bucket' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')).toHaveLength(1);
    });
    const checks = api.callsTo('POST', '/buckets/photos/subbuckets/scans/policy/validation');
    expect(checks).toHaveLength(2);
    expect(api.callsTo('PUT', '/buckets/photos/subbuckets/scans')[0]?.body).toBe(checks[1]?.body);
  });

  it('leaves the JSON editor alone on a node without the builder, with a notice, and checks nothing', async () => {
    const api = subNode(() => json(200, verdict()), { 'GET /policy-catalogue': () => refusal(404, 'NotFound', 'Not found') });
    render(<SubBucketsSection bucket="photos" />);
    fireEvent.click(screen.getByRole('button', { name: 'Sub-buckets' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Open sub-bucket reports' }));
    const region = await screen.findByRole('region', { name: 'Sub-bucket reports' });
    await touch(SUB_LABEL, region);
    expect(await within(region).findByText(OLDER_NODE_NOTICE)).toBeDefined();
    expect(within(region).queryByRole('group', { name: 'Policy builder' })).toBeNull();
    await waitFor(() => {
      expect(jsonBox(SUB_LABEL).value).toBe(formatPolicyDocument(SUB_POLICY));
    });
    typeInto(jsonBox(SUB_LABEL), '{"Version":"2012-10-17","Statement":[]}');
    fireEvent.click(within(region).getByRole('button', { name: 'Save policy' }));
    await waitFor(() => {
      expect(api.callsTo('PUT', '/buckets/photos/subbuckets/reports')).toHaveLength(1);
    });
    expect(api.callsTo('POST', SUB_VALIDATION)).toHaveLength(0);
  });
});
