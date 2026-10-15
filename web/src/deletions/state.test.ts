import { describe, expect, it } from 'vitest';

import {
  countOf,
  deletionsReducer,
  NO_DELETIONS,
  outcomeOf,
  progressLineOf,
  totalOf,
  type DeletionsAction,
  type DeletionsState,
} from './state';

// What is known of the deletions and how it is worded, by itself.

function run(actions: DeletionsAction[], from: DeletionsState = NO_DELETIONS): DeletionsState {
  return actions.reduce(deletionsReducer, from);
}

function only(state: DeletionsState) {
  const [deletion] = state.deletions;
  if (deletion === undefined) {
    throw new Error('no deletion');
  }
  return deletion;
}

describe('the deletions', () => {
  it.each([
    [0, '0'],
    [999, '999'],
    [1200, '1 200'],
    [5000, '5 000'],
    [1234567, '1 234 567'],
  ])('writes %i as %s', (n, said) => {
    expect(countOf(n)).toBe(said);
  });

  it.each([
    {
      name: 'what the usage said, while the listing is walked',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'all', expected: { count: 5000, atLeast: false } } },
        { type: 'listed', id: 'd', keys: 1000, last: false },
      ],
      total: { count: 5000, atLeast: false },
      line: 'Deleting 0 of 5 000 from b',
    },
    {
      name: '"at least" what the usage said, when its count stopped at its bound',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'all', expected: { count: 5000, atLeast: true } } },
      ],
      total: { count: 5000, atLeast: true },
      line: 'Deleting 0 of at least 5 000 from b',
    },
    {
      name: '"at least" what was found, when more were found than the usage said',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'all', expected: { count: 10, atLeast: false } } },
        { type: 'listed', id: 'd', keys: 1000, last: false },
      ],
      total: { count: 1000, atLeast: true },
      line: 'Deleting 0 of at least 1 000 from b',
    },
    {
      name: 'what was found, once the listing is walked to its end',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'all', expected: { count: 5000, atLeast: true } } },
        { type: 'listed', id: 'd', keys: 1000, last: false },
        { type: 'listed', id: 'd', keys: 200, last: true },
        { type: 'deleted', id: 'd' },
        { type: 'refused', id: 'd', reason: 'Access denied' },
      ],
      total: { count: 1200, atLeast: false },
      line: 'Deleting 2 of 1 200 from b · 1 not deleted',
    },
    {
      name: 'the keys that were ticked',
      actions: [{ type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'selected', keys: 3 } }],
      total: { count: 3, atLeast: false },
      line: 'Deleting 0 of 3 from b',
    },
  ] satisfies { name: string; actions: DeletionsAction[]; total: unknown; line: string }[])(
    'counts of how many: $name',
    ({ actions, total, line }) => {
      const deletion = only(run(actions));
      expect(totalOf(deletion)).toEqual(total);
      expect(progressLineOf(deletion)).toBe(line);
    },
  );

  it('keeps every reason once, with how many it was given for, first reason first', () => {
    const deletion = only(
      run([
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'selected', keys: 4 } },
        { type: 'refused', id: 'd', reason: 'Object locked' },
        { type: 'refused', id: 'd', reason: 'Access denied' },
        { type: 'refused', id: 'd', reason: 'Object locked' },
        { type: 'deleted', id: 'd' },
        { type: 'over', id: 'd' },
      ]),
    );
    expect(deletion.refusals).toEqual([
      { reason: 'Object locked', count: 2 },
      { reason: 'Access denied', count: 1 },
    ]);
    expect(outcomeOf(deletion)).toEqual({
      kind: 'error',
      title: 'Deleted 1 of 4 objects from b: 3 not deleted',
      detail: 'Not deleted: Object locked (2 objects); Access denied',
    });
  });

  it.each([
    {
      name: 'every object deleted',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'all' } },
        { type: 'listed', id: 'd', keys: 1, last: true },
        { type: 'deleted', id: 'd' },
      ],
      outcome: {
        kind: 'success',
        title: 'Deleted every object of b: 1 object',
        detail: 'An object stored in the bucket while the deletion ran may be left.',
      },
    },
    {
      name: 'the ticked objects deleted',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'selected', keys: 2 } },
        { type: 'deleted', id: 'd' },
        { type: 'deleted', id: 'd' },
      ],
      outcome: { kind: 'success', title: 'Deleted 2 objects from b', detail: undefined },
    },
    {
      name: 'cancelled',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'selected', keys: 10 } },
        { type: 'deleted', id: 'd' },
        { type: 'cancelled', id: 'd' },
        { type: 'skipped', id: 'd', keys: 9 },
      ],
      outcome: { kind: 'info', title: 'The deletion from b was cancelled: 1 object deleted, 9 not begun', detail: undefined },
    },
    {
      name: 'stopped by the session',
      actions: [
        { type: 'begun', deletion: { id: 'd', bucket: 'b', kind: 'selected', keys: 10 } },
        { type: 'deleted', id: 'd' },
        { type: 'stopped', id: 'd', reason: 'The session was locked.' },
        { type: 'stopped', id: 'd', reason: 'The session ended.' },
      ],
      outcome: { kind: 'error', title: 'Deleted 1 object from b, and stopped', detail: 'The session was locked.' },
    },
  ] satisfies { name: string; actions: DeletionsAction[]; outcome: unknown }[])('says how it ended: $name', ({ actions, outcome }) => {
    expect(outcomeOf(only(run(actions)))).toEqual(outcome);
  });

  it('forgets only what is over, of the bucket asked for', () => {
    const state = run([
      { type: 'begun', deletion: { id: 'a', bucket: 'one', kind: 'selected', keys: 1 } },
      { type: 'begun', deletion: { id: 'b', bucket: 'one', kind: 'selected', keys: 1 } },
      { type: 'begun', deletion: { id: 'c', bucket: 'two', kind: 'selected', keys: 1 } },
      { type: 'over', id: 'a' },
      { type: 'over', id: 'c' },
    ]);
    const after = deletionsReducer(state, { type: 'dismissed', ids: ['a', 'b', 'c'] });
    expect(after.deletions.map((d) => d.id)).toEqual(['b']);
    // Nothing to forget is no change.
    expect(deletionsReducer(after, { type: 'dismissed', ids: ['b'] })).toBe(after);
    expect(deletionsReducer(after, { type: 'deleted', id: 'nobody' })).toBe(after);
  });
});
