import { describe, expect, it } from 'vitest';

import { NO_NOTICES, NOTICES_KEPT, noticesReducer, type NoticesAction, type NoticesState } from './state';

const AT = '2026-10-12T10:00:00.000Z';

function said(title: string): NoticesAction {
  return { type: 'said', notice: { kind: 'success', title }, at: AT };
}

function after(...actions: NoticesAction[]): NoticesState {
  return actions.reduce(noticesReducer, NO_NOTICES);
}

describe('notices reducer', () => {
  it('keeps what was said, newest first, each with a number of its own and when it was said', () => {
    const state = after(said('first'), {
      type: 'said',
      notice: {
        kind: 'error',
        title: 'second',
        detail: 'Access denied',
        link: { href: '#/buckets/photos', label: 'Open photos' },
      },
      at: '2026-10-12T10:00:01.000Z',
    });

    expect(state.notices).toEqual([
      {
        id: 2,
        at: '2026-10-12T10:00:01.000Z',
        kind: 'error',
        title: 'second',
        detail: 'Access denied',
        link: { href: '#/buckets/photos', label: 'Open photos' },
      },
      { id: 1, at: AT, kind: 'success', title: 'first' },
    ]);
  });

  it('keeps no more than so many: the oldest go', () => {
    const actions = Array.from({ length: NOTICES_KEPT + 3 }, (_, index) => said(`notice ${String(index + 1)}`));
    const state = after(...actions);

    expect(state.notices).toHaveLength(NOTICES_KEPT);
    expect(state.notices[0]?.title).toBe(`notice ${String(NOTICES_KEPT + 3)}`);
    expect(state.notices.at(-1)?.title).toBe('notice 4');
  });

  it('dismisses one notice and leaves the others in their order', () => {
    const state = noticesReducer(after(said('a'), said('b'), said('c')), { type: 'dismissed', id: 2 });

    expect(state.notices.map((notice) => notice.title)).toEqual(['c', 'a']);
  });

  it('is the same state when a notice that is not there is dismissed', () => {
    const state = after(said('a'));

    expect(noticesReducer(state, { type: 'dismissed', id: 99 })).toBe(state);
  });

  it('dismisses all of them at once', () => {
    const state = noticesReducer(after(said('a'), said('b')), { type: 'cleared' });

    expect(state.notices).toEqual([]);
    expect(noticesReducer(state, { type: 'cleared' })).toBe(state);
  });

  it('never gives a number twice, whatever was dismissed in between', () => {
    const state = after(said('a'), { type: 'dismissed', id: 1 }, said('b'), { type: 'cleared' }, said('c'));

    expect(state.notices.map((notice) => notice.id)).toEqual([3]);
  });
});
