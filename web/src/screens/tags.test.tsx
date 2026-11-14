import { File as NodeFile } from 'node:buffer';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { onLocked } from '../api/client';
import { App } from '../App';
import { DeletionsProvider } from '../deletions/DeletionsProvider';
import { CSRF_TOKEN, json, noContent, refusal, signedIn, type Call, type FakeApi } from '../test/fakeApi';
import { LOCKED_WITH_TAGS } from '../uploads/engine';
import { UploadsProvider } from '../uploads/UploadsProvider';
import { DELETE_MARKER_HAS_NO_TAGS } from './ObjectTags';
import { ObjectsScreen } from './ObjectsScreen';
import { BucketTagsSection, TagRulesSection, TAGS_SAVED } from './TagSections';
import { TagsScreen } from './TagsScreen';

const CREATED = '2026-11-01T00:00:00Z';

/** What the API answers a path it does not know: a node of an older release, during a rolling upgrade. */
const NO_SUCH_ENDPOINT = () => refusal(404, 'NotFound', 'No such endpoint');

/** What the API answers a caller it does not tell. */
const DENIED = () => refusal(403, 'AccessDenied', 'Access denied');

/** A refusal with its code and the detail the API gives. */
function refusedWith(status: number, code: string, title: string, detail: string): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title, status, code, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

/** That a change was sent as every change of a session is: with the CSRF token and an Idempotency-Key of its own. */
function expectSentAsAChange(call: Call | undefined) {
  expect(call?.headers.get('X-CSRF-Token')).toBe(CSRF_TOKEN);
  expect(call?.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
}

/** The body of a request, read as JSON. */
function bodyOf(call: Call | undefined): unknown {
  return JSON.parse(call?.body ?? 'null') as unknown;
}

/** That every button styled as one has a hint: on a phone its text folds away. */
function expectEveryButtonHasATitle() {
  for (const control of screen.getAllByRole('button')) {
    if (control.className.includes('btn')) {
      expect(control.getAttribute('title'), control.textContent).toBeTruthy();
    }
  }
}

/** The text of the field labelled so, in a group of rows. */
function valueIn(group: HTMLElement, label: string): string {
  return (within(group).getByLabelText(label) as HTMLInputElement).value;
}

function typeIn(group: HTMLElement, label: string, value: string) {
  fireEvent.change(within(group).getByLabelText(label), { target: { value } });
}

/** The keys and values of the rows of a group, in order. */
function rowsIn(group: HTMLElement): string[][] {
  const keys = within(group).queryAllByLabelText(/^Key of tag \d+$/) as HTMLInputElement[];
  const values = within(group).queryAllByLabelText(/^Value of tag \d+$/) as HTMLInputElement[];
  return keys.map((key, index) => [key.value, values[index]?.value ?? '']);
}

/** The text of the hint a group of rows is described by. */
function hintOf(element: HTMLElement): string {
  return document.getElementById(element.getAttribute('aria-describedby') ?? '')?.textContent ?? '';
}

function tag(key: string, value: string) {
  return { key, value };
}

const CATALOGUE = [
  { key: 'stage', description: 'Stage of work', values: ['draft', 'final'], created_at: CREATED },
  { key: 'owner', description: '', values: [], created_at: CREATED },
];

describe('the tags of a bucket', () => {
  const unfold = () => {
    fireEvent.click(screen.getByRole('button', { name: 'Bucket tags' }));
  };
  const group = () => screen.getByRole('group', { name: 'Tags of bucket photos' });

  it('are folded until asked for, then shown as rows in the order the API gives them', async () => {
    const api = signedIn({
      'GET /buckets/photos/tags': () => json(200, { tags: [tag('stage', 'draft'), tag('team', 'finance')] }),
      'GET /tag-definitions': () => json(200, CATALOGUE),
    });
    render(<BucketTagsSection bucket="photos" />);
    expect(api.callsTo('GET', '/buckets/photos/tags')).toHaveLength(0);

    unfold();
    await screen.findByRole('group', { name: 'Tags of bucket photos' });
    expect(rowsIn(group())).toEqual([
      ['stage', 'draft'],
      ['team', 'finance'],
    ]);
    // The bounds of a tag are said, beside the rows.
    expect(hintOf(group())).toContain('at most 50 tags');
    expect(hintOf(group())).toContain('1 to 128 characters');
    expect(hintOf(group())).toContain('at most 256');
    expect(screen.getByRole('button', { name: 'Remove tags' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('offers the keys and values of the catalogue while a tag is typed', async () => {
    signedIn({
      'GET /buckets/photos/tags': () => json(200, { tags: [] }),
      'GET /tag-definitions': () => json(200, CATALOGUE),
    });
    render(<BucketTagsSection bucket="photos" />);
    unfold();
    fireEvent.click(await screen.findByRole('button', { name: 'Add a tag' }));
    const key = within(group()).getByLabelText('Key of tag 1');
    await waitFor(() => {
      expect(key.getAttribute('list')).toBeTruthy();
    });
    const keys = document.getElementById(key.getAttribute('list') ?? '');
    expect([...(keys?.querySelectorAll('option') ?? [])].map((option) => option.value)).toEqual(['stage', 'owner']);

    typeIn(group(), 'Key of tag 1', 'stage');
    const value = within(group()).getByLabelText('Value of tag 1');
    const values = document.getElementById(value.getAttribute('list') ?? '');
    expect([...(values?.querySelectorAll('option') ?? [])].map((option) => option.value)).toEqual(['draft', 'final']);
  });

  it('says that a bucket has none, and offers nothing to remove', async () => {
    signedIn({ 'GET /buckets/photos/tags': () => json(200, { tags: [] }) });
    render(<BucketTagsSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('This bucket has no tags.')).toBeDefined();
    expect(within(group()).getByText('No tags.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Remove tags' })).toBeNull();
  });

  it('says in a line, without an alert, that the caller may not see them, and offers no editor', async () => {
    signedIn({ 'GET /buckets/photos/tags': DENIED });
    render(<BucketTagsSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('You may not see the tags of this bucket.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('group', { name: 'Tags of bucket photos' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Save tags' })).toBeNull();
  });

  it('words a 404 NotFound as an older node, and asks again', async () => {
    let older = true;
    const api = signedIn({
      'GET /buckets/photos/tags': () => (older ? NO_SUCH_ENDPOINT() : json(200, { tags: [tag('a', '1')] })),
    });
    render(<BucketTagsSection bucket="photos" />);
    unfold();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(
      'Bucket tags are not available on the node that answered (an older release); try again.',
    );
    older = false;
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByRole('group', { name: 'Tags of bucket photos' });
    expect(rowsIn(group())).toEqual([['a', '1']]);
    expect(api.callsTo('GET', '/buckets/photos/tags')).toHaveLength(2);
  });

  it('are saved as the list in the order typed, and then shown as the API keeps them', async () => {
    const api = signedIn({
      'GET /buckets/photos/tags': () => json(200, { tags: [] }),
      // The bucket lower-cases its tags: the answer is them as kept, in key order.
      'PUT /buckets/photos/tags': () => json(200, { tags: [tag('alpha', 'two'), tag('zeta', 'one')] }),
    });
    render(<BucketTagsSection bucket="photos" />);
    unfold();

    const add = await screen.findByRole('button', { name: 'Add a tag' });
    fireEvent.click(add);
    // The keyboard goes to the key of the row that was added.
    expect(document.activeElement).toBe(within(group()).getByLabelText('Key of tag 1'));
    fireEvent.click(add);
    expect(document.activeElement).toBe(within(group()).getByLabelText('Key of tag 2'));
    typeIn(group(), 'Key of tag 1', 'Zeta');
    typeIn(group(), 'Value of tag 1', 'One');
    typeIn(group(), 'Key of tag 2', 'alpha');
    typeIn(group(), 'Value of tag 2', 'two');
    fireEvent.click(screen.getByRole('button', { name: 'Save tags' }));

    await waitFor(() => {
      expect(rowsIn(group())).toEqual([
        ['alpha', 'two'],
        ['zeta', 'one'],
      ]);
    });
    const [put] = api.callsTo('PUT', '/buckets/photos/tags');
    expect(bodyOf(put)).toEqual({ tags: [tag('Zeta', 'One'), tag('alpha', 'two')] });
    expectSentAsAChange(put);
    expect(screen.getByText(TAGS_SAVED).closest('[role="status"]')).not.toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    // The form was made anew with what is kept: the keyboard is on its Save button, not lost.
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Save tags' }));
  });

  it('sends a key given twice, and shows why the API refused it, keeping what was typed', async () => {
    const api = signedIn({
      'GET /buckets/photos/tags': () => json(200, { tags: [] }),
      'PUT /buckets/photos/tags': () =>
        refusedWith(400, 'InvalidArgument', 'Invalid argument', 'the tag key "a" is given twice'),
    });
    render(<BucketTagsSection bucket="photos" />);
    unfold();

    const add = await screen.findByRole('button', { name: 'Add a tag' });
    fireEvent.click(add);
    fireEvent.click(add);
    typeIn(group(), 'Key of tag 1', 'a');
    typeIn(group(), 'Value of tag 1', '1');
    typeIn(group(), 'Key of tag 2', 'a');
    typeIn(group(), 'Value of tag 2', '2');
    fireEvent.click(screen.getByRole('button', { name: 'Save tags' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The tags were refused: bucket photos keeps the tags it had.');
    expect(alert.textContent).toContain('The server said: Invalid argument: the tag key "a" is given twice');
    expect(bodyOf(api.callsTo('PUT', '/buckets/photos/tags')[0])).toEqual({ tags: [tag('a', '1'), tag('a', '2')] });
    expect(rowsIn(group())).toEqual([
      ['a', '1'],
      ['a', '2'],
    ]);
  });

  it('takes a row out without sending anything, and gives the keyboard to "Add a tag"', async () => {
    const api = signedIn({ 'GET /buckets/photos/tags': () => json(200, { tags: [tag('a', '1'), tag('b', '2')] }) });
    render(<BucketTagsSection bucket="photos" />);
    unfold();
    await screen.findByRole('group', { name: 'Tags of bucket photos' });

    fireEvent.click(within(group()).getByRole('button', { name: 'Remove tag 1' }));
    expect(rowsIn(group())).toEqual([['b', '2']]);
    expect(document.activeElement).toBe(within(group()).getByRole('button', { name: 'Add a tag' }));
    expect(api.calls.every((call) => call.method === 'GET')).toBe(true);
  });

  it('are removed only after the question, with a DELETE', async () => {
    const api = signedIn({
      'GET /buckets/photos/tags': () => json(200, { tags: [tag('a', '1')] }),
      'DELETE /buckets/photos/tags': () => noContent(),
    });
    render(<BucketTagsSection bucket="photos" />);
    unfold();

    fireEvent.click(await screen.findByRole('button', { name: 'Remove tags' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('DELETE', '/buckets/photos/tags')).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Remove tags' }));
    expect(screen.getByRole('group', { name: 'Remove every tag of bucket photos?' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Yes, remove tags' }));

    expect(await screen.findByText('This bucket has no tags.')).toBeDefined();
    expect(screen.getByText('Removed every tag of bucket photos.')).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Save tags' }));
    expectSentAsAChange(api.callsTo('DELETE', '/buckets/photos/tags')[0]);
    expect(api.callsTo('PUT', '/buckets/photos/tags')).toHaveLength(0);
  });
});

describe('the tag rules of a bucket', () => {
  const unfold = () => {
    fireEvent.click(screen.getByRole('button', { name: 'Tag rules' }));
  };
  const required = () => screen.getByRole('group', { name: 'Required tags of bucket photos' });
  const RULES = {
    lowercase: true,
    catalogue_only: false,
    required: [
      { key: 'stage', values: ['draft', 'final'] },
      { key: 'period', sub_bucket: 'reports' },
    ],
  };

  it('are folded until asked for, then shown: lower case, the catalogue, the required tags and their bounds', async () => {
    const api = signedIn({ 'GET /buckets/photos/tag-rules': () => json(200, RULES) });
    render(<TagRulesSection bucket="photos" />);
    expect(api.callsTo('GET', '/buckets/photos/tag-rules')).toHaveLength(0);
    unfold();

    await screen.findByRole('group', { name: 'Required tags of bucket photos' });
    expect((screen.getByLabelText('Keep tags in lower case, whatever case they are sent in') as HTMLInputElement).checked).toBe(true);
    expect(
      (screen.getByLabelText('Allow only the tags of the catalogue, with the values it allows') as HTMLInputElement).checked,
    ).toBe(false);
    expect(valueIn(required(), 'Key of required tag 1')).toBe('stage');
    expect(valueIn(required(), 'Allowed values of required tag 1')).toBe('draft, final');
    expect(valueIn(required(), 'Sub-bucket of required tag 1')).toBe('');
    expect(valueIn(required(), 'Key of required tag 2')).toBe('period');
    expect(valueIn(required(), 'Allowed values of required tag 2')).toBe('');
    expect(valueIn(required(), 'Sub-bucket of required tag 2')).toBe('reports');
    const hint = hintOf(required());
    expect(hint).toContain('At most 10 required tags');
    expect(hint).toContain('1 to 128 characters');
    expect(hint).toContain('at most 50 of at most 256 characters each');
    expect(screen.getByRole('button', { name: 'Remove rules' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('says that a bucket has none', async () => {
    signedIn({
      'GET /buckets/photos/tag-rules': () => json(200, { lowercase: false, catalogue_only: false, required: [] }),
    });
    render(<TagRulesSection bucket="photos" />);
    unfold();

    expect(await screen.findByText(/^This bucket has no tag rules/)).toBeDefined();
    expect(within(required()).getByText('No tag is required.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Remove rules' })).toBeNull();
  });

  it('says in a line, without an alert, that the caller may not see them, and offers nothing else', async () => {
    signedIn({ 'GET /buckets/photos/tag-rules': DENIED });
    render(<TagRulesSection bucket="photos" />);
    unfold();

    expect(await screen.findByText('You may not see the tag rules of this bucket.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Save rules' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
  });

  it('words a 404 NotFound as an older node', async () => {
    signedIn({ 'GET /buckets/photos/tag-rules': NO_SUCH_ENDPOINT });
    render(<TagRulesSection bucket="photos" />);
    unfold();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(
      'Tag rules are not available on the node that answered (an older release); try again.',
    );
    expect(screen.getByRole('button', { name: 'Try again' })).toBeDefined();
  });

  it('are saved whole, the required tags in the order typed, and then shown as the API answered', async () => {
    const saved = {
      lowercase: true,
      catalogue_only: false,
      required: [
        { key: 'stage', values: ['draft', 'final'] },
        { key: 'team', sub_bucket: 'reports' },
      ],
    };
    const api = signedIn({
      'GET /buckets/photos/tag-rules': () => json(200, { lowercase: false, catalogue_only: false, required: [] }),
      'PUT /buckets/photos/tag-rules': () => json(200, saved),
    });
    render(<TagRulesSection bucket="photos" />);
    unfold();

    const add = await screen.findByRole('button', { name: 'Add a required tag' });
    fireEvent.click(screen.getByLabelText('Keep tags in lower case, whatever case they are sent in'));
    fireEvent.click(add);
    expect(document.activeElement).toBe(within(required()).getByLabelText('Key of required tag 1'));
    fireEvent.click(add);
    typeIn(required(), 'Key of required tag 1', 'stage');
    typeIn(required(), 'Allowed values of required tag 1', ' draft, final ,');
    typeIn(required(), 'Key of required tag 2', 'team');
    typeIn(required(), 'Sub-bucket of required tag 2', 'reports');
    fireEvent.click(screen.getByRole('button', { name: 'Save rules' }));

    expect(await screen.findByText('Saved. Objects stored before keep the tags they have.')).toBeDefined();
    const [put] = api.callsTo('PUT', '/buckets/photos/tag-rules');
    expect(bodyOf(put)).toEqual(saved);
    expectSentAsAChange(put);
    expect(valueIn(required(), 'Allowed values of required tag 1')).toBe('draft, final');
    expect(screen.getByRole('button', { name: 'Remove rules' })).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Save rules' }));
  });

  it('shows why the API refused rules, keeping what was typed', async () => {
    signedIn({
      'GET /buckets/photos/tag-rules': () => json(200, { lowercase: true, catalogue_only: false, required: [] }),
      'PUT /buckets/photos/tag-rules': () =>
        refusedWith(400, 'InvalidArgument', 'Invalid argument', 'required tag 1: the key must be in lower case'),
    });
    render(<TagRulesSection bucket="photos" />);
    unfold();

    fireEvent.click(await screen.findByRole('button', { name: 'Add a required tag' }));
    typeIn(required(), 'Key of required tag 1', 'Stage');
    fireEvent.click(screen.getByRole('button', { name: 'Save rules' }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The rules were refused: bucket photos keeps the rules it had.');
    expect(alert.textContent).toContain('The server said: Invalid argument: required tag 1: the key must be in lower case');
    expect(valueIn(required(), 'Key of required tag 1')).toBe('Stage');
  });

  it('are removed only after the question, with a DELETE', async () => {
    const api = signedIn({
      'GET /buckets/photos/tag-rules': () => json(200, RULES),
      'DELETE /buckets/photos/tag-rules': () => noContent(),
    });
    render(<TagRulesSection bucket="photos" />);
    unfold();

    fireEvent.click(await screen.findByRole('button', { name: 'Remove rules' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, remove rules' }));

    expect(await screen.findByText(/^This bucket has no tag rules/)).toBeDefined();
    expect(screen.getByText('Removed the tag rules of bucket photos.')).toBeDefined();
    expectSentAsAChange(api.callsTo('DELETE', '/buckets/photos/tag-rules')[0]);
    expect((screen.getByLabelText('Keep tags in lower case, whatever case they are sent in') as HTMLInputElement).checked).toBe(false);
  });
});

/**
 * Shows the object browser as the console does: beneath the uploads and the
 * deletions of the console.
 */
function renderObjects(screenOfABucket: ReactElement) {
  return render(
    <UploadsProvider>
      <DeletionsProvider>{screenOfABucket}</DeletionsProvider>
    </UploadsProvider>,
  );
}

const LISTING = {
  objects: [{ key: 'a.txt', size: 3, etag: '"e"', last_modified: CREATED }],
  common_prefixes: [],
  truncated: false,
  access_included: false,
};

const METADATA = {
  bucket: 'photos',
  key: 'a.txt',
  size: 3,
  etag: '"e"',
  last_modified: CREATED,
  content_type: 'application/octet-stream',
  encryption: 'SSE-S3',
};

describe('the tags of an object, in its details', () => {
  /** Opens the details of a.txt and returns the region they are in. */
  async function openDetails(): Promise<HTMLElement> {
    fireEvent.click(await screen.findByRole('button', { name: 'Details of a.txt' }));
    return screen.getByRole('region', { name: 'Details of a.txt' });
  }

  function objectRoutes(more: Record<string, (call: Call) => Response>): FakeApi {
    return signedIn({
      'GET /buckets/photos/objects': () => json(200, LISTING),
      'GET /buckets/photos/object/metadata': () => json(200, METADATA),
      ...more,
    });
  }

  it('are shown, those of the current version, asked for by key', async () => {
    const api = objectRoutes({
      'GET /buckets/photos/object/tags': () =>
        json(200, { version_id: 'null', tags: [tag('stage', 'draft'), tag('team', '')] }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    expect(await within(region).findByText('stage')).toBeDefined();
    expect(within(region).getByText('draft')).toBeDefined();
    expect(within(region).getByText('(no value)')).toBeDefined();
    const [get] = api.callsTo('GET', '/buckets/photos/object/tags');
    expect(get?.query.get('key')).toBe('a.txt');
    expect(get?.query.has('version_id')).toBe(false);
    expect(within(region).queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('says that an object has none', async () => {
    objectRoutes({ 'GET /buckets/photos/object/tags': () => json(200, { version_id: 'null', tags: [] }) });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    expect(await within(region).findByText('This object has no tags.')).toBeDefined();
  });

  it('says in a line, without an alert, that the caller may not see them', async () => {
    objectRoutes({ 'GET /buckets/photos/object/tags': DENIED });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    expect(await within(region).findByText('You may not see the tags of this object.')).toBeDefined();
    expect(within(region).queryByRole('alert')).toBeNull();
    expect(within(region).queryByRole('button', { name: 'Edit tags' })).toBeNull();
  });

  it('says that a delete marker has none (405)', async () => {
    objectRoutes({
      'GET /buckets/photos/object/tags': () => refusal(405, 'MethodNotAllowed', 'Not possible for this resource'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    expect(await within(region).findByText(DELETE_MARKER_HAS_NO_TAGS, { exact: false })).toBeDefined();
    expect(within(region).queryByRole('alert')).toBeNull();
  });

  it('words a 404 NotFound as an older node, and asks again', async () => {
    let older = true;
    objectRoutes({
      'GET /buckets/photos/object/tags': () =>
        older ? NO_SUCH_ENDPOINT() : json(200, { version_id: 'null', tags: [tag('a', '1')] }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    expect(
      await within(region).findByText(
        'Object tags are not available on the node that answered (an older release); try again.',
        { exact: false },
      ),
    ).toBeDefined();
    older = false;
    // Named for what it asks again: the details have a "Try again" of their own.
    fireEvent.click(within(region).getByRole('button', { name: 'Read tags again' }));
    expect(await within(region).findByText('a')).toBeDefined();
  });

  it('are edited and saved in the order typed, then shown as kept, the keyboard taken to the editor and back', async () => {
    const api = objectRoutes({
      'GET /buckets/photos/object/tags': () => json(200, { version_id: 'null', tags: [tag('stage', 'draft')] }),
      'PUT /buckets/photos/object/tags': () =>
        json(200, { version_id: 'null', tags: [tag('owner', 'ana'), tag('stage', 'final')] }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    fireEvent.click(await within(region).findByRole('button', { name: 'Edit tags' }));
    expect(document.activeElement).toBe(within(region).getByRole('heading', { name: 'Tags' }));
    const group = within(region).getByRole('group', { name: 'Tags of a.txt' });
    typeIn(group, 'Value of tag 1', 'Final');
    fireEvent.click(within(group).getByRole('button', { name: 'Add a tag' }));
    typeIn(group, 'Key of tag 2', 'Owner');
    typeIn(group, 'Value of tag 2', 'Ana');
    fireEvent.click(within(region).getByRole('button', { name: 'Save tags' }));

    expect(await within(region).findByText('ana')).toBeDefined();
    expect(within(region).getByText('final')).toBeDefined();
    expect(within(region).getByText(TAGS_SAVED)).toBeDefined();
    const [put] = api.callsTo('PUT', '/buckets/photos/object/tags');
    expect(put?.query.get('key')).toBe('a.txt');
    expect(bodyOf(put)).toEqual({ tags: [tag('stage', 'Final'), tag('Owner', 'Ana')] });
    expectSentAsAChange(put);
    expect(document.activeElement).toBe(within(region).getByRole('button', { name: 'Edit tags' }));
  });

  it('shows the detail of a tag rule the API says they break, and keeps the editor open', async () => {
    objectRoutes({
      'GET /buckets/photos/object/tags': () => json(200, { version_id: 'null', tags: [tag('stage', 'draft')] }),
      'PUT /buckets/photos/object/tags': () =>
        refusedWith(400, 'TagRuleViolated', "Against the bucket's tag rules", 'required tag "stage" has a value it may not have'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    fireEvent.click(await within(region).findByRole('button', { name: 'Edit tags' }));
    const group = within(region).getByRole('group', { name: 'Tags of a.txt' });
    typeIn(group, 'Value of tag 1', 'gone');
    fireEvent.click(within(region).getByRole('button', { name: 'Save tags' }));

    const alert = await within(region).findByRole('alert');
    expect(alert.textContent).toContain(
      'The tag rules of bucket photos do not allow these tags: the object keeps the tags it had.',
    );
    expect(alert.textContent).toContain(
      `The server said: Against the bucket's tag rules: required tag "stage" has a value it may not have`,
    );
    expect(valueIn(group, 'Value of tag 1')).toBe('gone');

    // Closed and opened again, the editor begins afresh: the refusal of before is not shown.
    fireEvent.click(within(region).getByRole('button', { name: 'Close' }));
    expect(document.activeElement).toBe(within(region).getByRole('button', { name: 'Edit tags' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Edit tags' }));
    expect(valueIn(within(region).getByRole('group', { name: 'Tags of a.txt' }), 'Value of tag 1')).toBe('draft');
    expect(within(region).queryByRole('alert')).toBeNull();
  });

  it('explains a delete marker and an object that is gone when saving', async () => {
    let answer = () => refusal(405, 'MethodNotAllowed', 'Not possible for this resource');
    objectRoutes({
      'GET /buckets/photos/object/tags': () => json(200, { version_id: 'null', tags: [] }),
      'PUT /buckets/photos/object/tags': () => answer(),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    fireEvent.click(await within(region).findByRole('button', { name: 'Edit tags' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Save tags' }));
    expect((await within(region).findByRole('alert')).textContent).toContain(DELETE_MARKER_HAS_NO_TAGS);

    answer = () => refusal(404, 'NoSuchObject', 'No such object');
    fireEvent.click(within(region).getByRole('button', { name: 'Save tags' }));
    await waitFor(() => {
      expect(within(region).getByRole('alert').textContent).toContain('a.txt is not there any more.');
    });
  });

  it('are removed after the question; a rule that requires one is explained', async () => {
    let refuse = true;
    const api = objectRoutes({
      'GET /buckets/photos/object/tags': () => json(200, { version_id: 'null', tags: [tag('stage', 'draft')] }),
      'DELETE /buckets/photos/object/tags': () =>
        refuse
          ? refusedWith(400, 'TagRuleViolated', "Against the bucket's tag rules", 'required tag "stage" is missing')
          : noContent(),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    const region = await openDetails();

    fireEvent.click(await within(region).findByRole('button', { name: 'Edit tags' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Remove tags' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Yes, remove tags' }));
    expect((await within(region).findByRole('alert')).textContent).toContain(
      'The tag rules of bucket photos require a tag of this object: its tags are kept.',
    );

    refuse = false;
    fireEvent.click(within(region).getByRole('button', { name: 'Remove tags' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Yes, remove tags' }));
    expect(await within(region).findByText('This object has no tags.')).toBeDefined();
    const deletes = api.callsTo('DELETE', '/buckets/photos/object/tags');
    expect(deletes).toHaveLength(2);
    expect(deletes[1]?.query.get('key')).toBe('a.txt');
    expectSentAsAChange(deletes[1]);
  });
});

describe('the tags of an upload', () => {
  const group = () => screen.getByRole('group', { name: 'Tags of the upload' });
  const RULES = {
    lowercase: true,
    catalogue_only: true,
    required: [
      { key: 'stage', values: ['draft', 'final'] },
      { key: 'team' },
      { key: 'period', sub_bucket: 'reports' },
    ],
  };

  function uploadRoutes(more: Record<string, (call: Call) => Response>): FakeApi {
    return signedIn({
      'GET /buckets/photos/objects': () => json(200, { ...LISTING, objects: [] }),
      'PUT /buckets/photos/object': (call) => json(200, { ...METADATA, key: call.query.get('key') }),
      ...more,
    });
  }

  function choose(name: string) {
    fireEvent.change(screen.getByLabelText('File'), {
      target: { files: [new NodeFile([name], name, { type: 'text/plain' })] },
    });
  }

  function upload() {
    fireEvent.submit(screen.getByRole('button', { name: 'Upload' }).closest('form') as HTMLFormElement);
  }

  it('asks for the rules only once files are chosen, says what they require, and begins with the keys required', async () => {
    const api = uploadRoutes({ 'GET /buckets/photos/tag-rules': () => json(200, RULES) });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');
    expect(api.callsTo('GET', '/buckets/photos/tag-rules')).toHaveLength(0);
    expect(rowsIn(group())).toEqual([]);

    choose('a.txt');
    await waitFor(() => {
      expect(rowsIn(group())).toEqual([
        ['stage', ''],
        ['team', ''],
      ]);
    });
    const said = screen.getAllByRole('listitem').map((item) => item.textContent);
    expect(said).toContain('stage: one of draft, final');
    expect(said).toContain('team: any value');
    expect(said).toContain('period: any value (only under reports/)');
    expect(screen.getByText('Only the tags of the catalogue may be given, with the values it allows.')).toBeDefined();
    expect(screen.getByText('Tags are kept in lower case.')).toBeDefined();
    expect(api.callsTo('GET', '/buckets/photos/tag-rules')).toHaveLength(1);
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('sends the tags as X-CactusFS-Tagging, a URL-encoded query string in the order typed', async () => {
    const api = uploadRoutes({ 'GET /buckets/photos/tag-rules': () => json(200, RULES) });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    choose('a.txt');
    await waitFor(() => {
      expect(rowsIn(group())).toHaveLength(2);
    });
    typeIn(group(), 'Value of tag 1', 'draft');
    typeIn(group(), 'Value of tag 2', 'a+b=c/d e');
    fireEvent.click(within(group()).getByRole('button', { name: 'Add a tag' }));
    typeIn(group(), 'Key of tag 3', 'Owner');
    typeIn(group(), 'Value of tag 3', 'Ana@x:y');
    upload();

    expect(await screen.findByText('1 of 1 stored')).toBeDefined();
    const [put] = api.callsTo('PUT', '/buckets/photos/object');
    const header = put?.headers.get('X-CactusFS-Tagging') ?? '';
    expect(header).toBe('stage=draft&team=a%2Bb%3Dc%2Fd%20e&Owner=Ana%40x%3Ay');
    expect([...new URLSearchParams(header)]).toEqual([
      ['stage', 'draft'],
      ['team', 'a+b=c/d e'],
      ['Owner', 'Ana@x:y'],
    ]);
    expect(put?.query.get('key')).toBe('a.txt');
  });

  it('sends no header when there are no tags', async () => {
    const api = uploadRoutes({
      'GET /buckets/photos/tag-rules': () => json(200, { lowercase: false, catalogue_only: false, required: [] }),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    choose('a.txt');
    expect(await screen.findByText('This bucket requires no tags.')).toBeDefined();
    upload();

    expect(await screen.findByText('1 of 1 stored')).toBeDefined();
    expect(api.callsTo('PUT', '/buckets/photos/object')[0]?.headers.get('X-CactusFS-Tagging')).toBeNull();
  });

  it('says that the server will tell when the caller may not read the rules (403), without an alert', async () => {
    uploadRoutes({ 'GET /buckets/photos/tag-rules': DENIED });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    choose('a.txt');
    expect(
      await screen.findByText(
        "You may not see the tag rules of this bucket: the server will say whether the upload's tags keep to them.",
      ),
    ).toBeDefined();
    expect(rowsIn(group())).toEqual([]);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says the same when the rules could not be read', async () => {
    uploadRoutes({ 'GET /buckets/photos/tag-rules': NO_SUCH_ENDPOINT });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    choose('a.txt');
    expect(
      await screen.findByText(
        "The tag rules of this bucket could not be read: the server will say whether the upload's tags keep to them.",
      ),
    ).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows beside the file the detail of a tag rule the server refused it for', async () => {
    uploadRoutes({
      'GET /buckets/photos/tag-rules': () => json(200, RULES),
      'PUT /buckets/photos/object': () =>
        refusedWith(400, 'TagRuleViolated', "Against the bucket's tag rules", 'required tag "team" is missing'),
    });
    renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
    await screen.findByText('There is nothing here.');

    choose('a.txt');
    await waitFor(() => {
      expect(rowsIn(group())).toHaveLength(2);
    });
    typeIn(group(), 'Value of tag 1', 'draft');
    upload();

    const progress = within(await screen.findByRole('list', { name: 'Upload progress' }));
    expect(
      await progress.findByText(`Against the bucket's tag rules: required tag "team" is missing`),
    ).toBeDefined();
    expect(progress.getByText('Not stored')).toBeDefined();
  });

  it('says of a file with tags that a locked session was refused, and tells the lock', async () => {
    const locked = vi.fn();
    const stop = onLocked(locked);
    try {
      uploadRoutes({
        'GET /buckets/photos/tag-rules': () => json(200, { lowercase: false, catalogue_only: false, required: [] }),
        'PUT /buckets/photos/object': () => refusal(423, 'SessionLocked', 'The session is locked'),
      });
      renderObjects(<ObjectsScreen bucket="photos" prefix="" />);
      await screen.findByText('There is nothing here.');

      choose('a.txt');
      await screen.findByText('This bucket requires no tags.');
      fireEvent.click(within(group()).getByRole('button', { name: 'Add a tag' }));
      typeIn(group(), 'Key of tag 1', 'stage');
      typeIn(group(), 'Value of tag 1', 'draft');
      upload();

      const progress = within(await screen.findByRole('list', { name: 'Upload progress' }));
      expect(await progress.findByText(LOCKED_WITH_TAGS)).toBeDefined();
      expect(locked).toHaveBeenCalled();
    } finally {
      stop();
    }
  });
});

describe('the catalogue of tags', () => {
  const rowOf = (key: string) =>
    screen.getByRole('button', { name: `Edit tag ${key}` }).closest('tr') as HTMLTableRowElement;

  it('lists the definitions as a grid that folds into cards on a phone', async () => {
    signedIn({ 'GET /tag-definitions': () => json(200, CATALOGUE) });
    render(<TagsScreen />);

    expect(await screen.findByRole('heading', { name: 'Tags', level: 1 })).toBeDefined();
    const table = await screen.findByRole('table');
    expect(table.className).toContain('grid');
    expect(table.className).not.toContain('grid-keep');
    expect(within(rowOf('stage')).getByText('Stage of work')).toBeDefined();
    expect(within(rowOf('stage')).getByText('draft, final')).toBeDefined();
    expect(within(rowOf('owner')).getByText('any value')).toBeDefined();
    for (const cell of within(rowOf('stage')).getAllByRole('cell')) {
      expect(cell.getAttribute('data-label')).toBeTruthy();
    }
    expect(screen.queryByRole('alert')).toBeNull();
    expectEveryButtonHasATitle();
  });

  it('says when it is empty', async () => {
    signedIn({ 'GET /tag-definitions': () => json(200, []) });
    render(<TagsScreen />);

    expect(await screen.findByText('The catalogue defines no tags.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Define tag' })).toBeDefined();
  });

  it('is hidden from a caller the API does not show it, in a line and without an alert', async () => {
    signedIn({ 'GET /tag-definitions': DENIED });
    render(<TagsScreen />);

    expect(await screen.findByText('You may not see the catalogue of tags.')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Define tag' })).toBeNull();
    expect(screen.queryByRole('table')).toBeNull();
  });

  it('words a 404 NotFound as an older node', async () => {
    signedIn({ 'GET /tag-definitions': NO_SUCH_ENDPOINT });
    render(<TagsScreen />);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain(
      'The catalogue of tags is not available on the node that answered (an older release); try again.',
    );
  });

  it('defines a tag with a POST, the values in the order typed, and reads the list again', async () => {
    const api = signedIn({
      'GET /tag-definitions': () => json(200, []),
      'POST /tag-definitions': () => json(201, CATALOGUE[0]),
    });
    render(<TagsScreen />);
    await screen.findByText('The catalogue defines no tags.');

    fireEvent.change(screen.getByLabelText('Key'), { target: { value: 'stage' } });
    fireEvent.change(screen.getAllByLabelText('What the tag is for')[0] as HTMLElement, { target: { value: 'Stage of work' } });
    fireEvent.change(screen.getAllByLabelText('Values it may have')[0] as HTMLElement, { target: { value: 'final, draft' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define tag' }));

    await waitFor(() => {
      expect(api.callsTo('GET', '/tag-definitions')).toHaveLength(2);
    });
    const [post] = api.callsTo('POST', '/tag-definitions');
    expect(bodyOf(post)).toEqual({ key: 'stage', description: 'Stage of work', values: ['final', 'draft'] });
    expectSentAsAChange(post);
    expect((screen.getByLabelText('Key') as HTMLInputElement).value).toBe('');
  });

  it('defines a tag of any value without values', async () => {
    const api = signedIn({
      'GET /tag-definitions': () => json(200, []),
      'POST /tag-definitions': () => json(201, CATALOGUE[1]),
    });
    render(<TagsScreen />);
    await screen.findByText('The catalogue defines no tags.');

    fireEvent.change(screen.getByLabelText('Key'), { target: { value: 'owner' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define tag' }));

    await waitFor(() => {
      expect(api.callsTo('POST', '/tag-definitions')).toHaveLength(1);
    });
    expect(bodyOf(api.callsTo('POST', '/tag-definitions')[0])).toEqual({ key: 'owner' });
  });

  it('explains a key that is defined already, and one the API refuses', async () => {
    let answer = () => refusal(409, 'EntityAlreadyExists', 'Entity already exists');
    signedIn({ 'GET /tag-definitions': () => json(200, CATALOGUE), 'POST /tag-definitions': () => answer() });
    render(<TagsScreen />);
    await screen.findByRole('table');

    fireEvent.change(screen.getByLabelText('Key'), { target: { value: 'stage' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define tag' }));
    expect((await screen.findByRole('alert')).textContent).toContain(
      'Tag stage is defined already: edit it in the catalogue.',
    );

    answer = () => refusedWith(400, 'InvalidArgument', 'Invalid argument', 'a key of the catalogue is in lower case');
    fireEvent.change(screen.getByLabelText('Key'), { target: { value: 'Stage' } });
    fireEvent.click(screen.getByRole('button', { name: 'Define tag' }));
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toContain(
        'The definition of Stage was refused, and nothing was defined.',
      );
    });
    expect(screen.getByRole('alert').textContent).toContain(
      'The server said: Invalid argument: a key of the catalogue is in lower case',
    );
    expect((screen.getByLabelText('Key') as HTMLInputElement).value).toBe('Stage');
  });

  it('changes a definition with a PUT, the keyboard taken to it and back', async () => {
    const api = signedIn({
      'GET /tag-definitions': () => json(200, CATALOGUE),
      'PUT /tag-definitions/stage': () => json(200, { ...CATALOGUE[0], values: ['draft', 'final', 'archived'] }),
    });
    render(<TagsScreen />);
    await screen.findByRole('table');

    const edit = screen.getByRole('button', { name: 'Edit tag stage' });
    fireEvent.click(edit);
    const heading = screen.getByRole('heading', { name: 'Tag stage' });
    expect(document.activeElement).toBe(heading);
    const panel = heading.closest('section') as HTMLElement;
    expect((within(panel).getByLabelText('Values it may have') as HTMLInputElement).value).toBe('draft, final');
    fireEvent.change(within(panel).getByLabelText('Values it may have'), {
      target: { value: 'draft, final, archived' },
    });
    fireEvent.click(within(panel).getByRole('button', { name: 'Save definition' }));

    await waitFor(() => {
      expect(api.callsTo('PUT', '/tag-definitions/stage')).toHaveLength(1);
    });
    const [put] = api.callsTo('PUT', '/tag-definitions/stage');
    expect(bodyOf(put)).toEqual({ description: 'Stage of work', values: ['draft', 'final', 'archived'] });
    expectSentAsAChange(put);
    const saved = await screen.findByText(
      'Saved the definition of tag stage. Tags already kept and tag rules are not changed.',
    );
    expect(saved.closest('[role="status"]')).not.toBeNull();

    fireEvent.click(within(screen.getByRole('heading', { name: 'Tag stage' }).closest('section') as HTMLElement).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('heading', { name: 'Tag stage' })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Edit tag stage' }));
  });

  it('removes a definition only after the question', async () => {
    let listed = CATALOGUE;
    const api = signedIn({
      'GET /tag-definitions': () => json(200, listed),
      'DELETE /tag-definitions/stage': () => {
        listed = CATALOGUE.slice(1);
        return noContent();
      },
    });
    render(<TagsScreen />);
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Cancel' }));
    expect(api.callsTo('DELETE', '/tag-definitions/stage')).toHaveLength(0);

    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Yes, delete' }));

    expect(await screen.findByText('Removed tag stage from the catalogue. Tags already kept are not changed.')).toBeDefined();
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'Edit tag stage' })).toBeNull();
    });
    expectSentAsAChange(api.callsTo('DELETE', '/tag-definitions/stage')[0]);
  });

  it('explains a definition a rule names (409 EntityInUse) and one that is gone (404 NoSuchEntity)', async () => {
    let answer = () =>
      refusedWith(409, 'EntityInUse', 'Entity is in use', 'Something refers to it: what refers to it must be changed or removed first.');
    signedIn({ 'GET /tag-definitions': () => json(200, CATALOGUE), 'DELETE /tag-definitions/stage': () => answer() });
    render(<TagsScreen />);
    await screen.findByRole('table');

    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Yes, delete' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Tag stage cannot be removed: the tag rules of a bucket require it.');
    expect(alert.textContent).toContain('The server said: Entity is in use');

    answer = () => refusal(404, 'NoSuchEntity', 'No such entity');
    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(rowOf('stage')).getByRole('button', { name: 'Yes, delete' }));
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toContain('Tag stage is not in the catalogue any more.');
    });
  });

  it('is a section of the navigation, with an icon, that opens the screen', async () => {
    signedIn({
      'GET /buckets': () => json(200, []),
      'GET /tag-definitions': () => json(200, CATALOGUE),
    });
    render(<App />);

    const sections = within(await screen.findByRole('navigation', { name: 'Sections' }));
    const link = sections.getByRole('link', { name: 'Tags' });
    expect(link.getAttribute('href')).toBe('#/tags');
    expect(link.querySelector('i.bi-tags')?.getAttribute('aria-hidden')).toBe('true');

    window.location.hash = '#/tags';
    expect(await screen.findByRole('heading', { name: 'Tags', level: 1 })).toBeDefined();
    expect(await screen.findByText('Stage of work')).toBeDefined();
  });
});
