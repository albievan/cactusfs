import { describe, expect, it } from 'vitest';

import {
  batchesInFlight,
  batchesOf,
  inFlight,
  inScope,
  NO_UPLOADS,
  ofBatches,
  totalsOf,
  underWay,
  uploadsReducer,
  type NewUpload,
  type Upload,
  type UploadsAction,
  type UploadsState,
  type UploadState,
} from './state';

function added(id: string, more: Partial<NewUpload> = {}): NewUpload {
  return { id, batch: 'batch-1', bucket: 'photos', key: `${id}.txt`, total: 100, ...more };
}

/** The state after these things happened, one after the other. */
function after(...actions: UploadsAction[]): UploadsState {
  return actions.reduce(uploadsReducer, NO_UPLOADS);
}

function stateOf(state: UploadsState, id: string): UploadState | undefined {
  return state.uploads.find((upload) => upload.id === id)?.state;
}

function upload(id: string, state: UploadState, more: Partial<Upload> = {}): Upload {
  return { id, batch: 'batch-1', bucket: 'photos', key: `${id}.txt`, state, sent: 0, total: 100, ...more };
}

describe('uploads reducer', () => {
  it('adds files as waiting, with nothing sent, after those that were there', () => {
    const state = after(
      { type: 'added', uploads: [added('a')] },
      { type: 'added', uploads: [added('b', { batch: 'batch-2', bucket: 'films', total: 7 })] },
    );

    expect(state.uploads).toEqual([
      { id: 'a', batch: 'batch-1', bucket: 'photos', key: 'a.txt', state: 'waiting', sent: 0, total: 100 },
      { id: 'b', batch: 'batch-2', bucket: 'films', key: 'b.txt', state: 'waiting', sent: 0, total: 7 },
    ]);
  });

  it('is the same state when nothing is added', () => {
    const state = after({ type: 'added', uploads: [added('a')] });

    expect(uploadsReducer(state, { type: 'added', uploads: [] })).toBe(state);
  });

  it('takes a file from waiting to sending to stored, and stored is all of it sent', () => {
    const waiting = after({ type: 'added', uploads: [added('a'), added('b')] });
    const sending = uploadsReducer(waiting, { type: 'sending', id: 'a' });
    expect(stateOf(sending, 'a')).toBe('sending');
    expect(stateOf(sending, 'b')).toBe('waiting');

    const half = uploadsReducer(sending, { type: 'progress', progress: new Map([['a', { sent: 50, total: 100 }]]) });
    expect(half.uploads[0]).toMatchObject({ state: 'sending', sent: 50, total: 100 });

    const stored = uploadsReducer(half, { type: 'stored', id: 'a' });
    expect(stored.uploads[0]).toMatchObject({ state: 'stored', sent: 100, total: 100 });
    // The other file is the object it was: nothing about it changed.
    expect(stored.uploads[1]).toBe(waiting.uploads[1]);
  });

  it('keeps why a file was not stored, and how far it had come', () => {
    const state = after(
      { type: 'added', uploads: [added('a')] },
      { type: 'sending', id: 'a' },
      { type: 'progress', progress: new Map([['a', { sent: 30, total: 100 }]]) },
      { type: 'failed', id: 'a', reason: 'Access denied: Not this one.' },
    );

    expect(state.uploads[0]).toMatchObject({ state: 'failed', sent: 30, reason: 'Access denied: Not this one.' });
  });

  it('takes the progress of several files at once, and of those being sent only', () => {
    const state = after(
      { type: 'added', uploads: [added('a'), added('b'), added('c')] },
      { type: 'sending', id: 'a' },
      { type: 'sending', id: 'b' },
    );
    const next = uploadsReducer(state, {
      type: 'progress',
      progress: new Map([
        ['a', { sent: 10, total: 100 }],
        ['b', { sent: 20, total: 100 }],
        // Not begun: its progress is word of nothing.
        ['c', { sent: 30, total: 100 }],
        ['nobody', { sent: 40, total: 100 }],
      ]),
    });

    expect(next.uploads.map((entry) => entry.sent)).toEqual([10, 20, 0]);
  });

  it('is the same state when the progress says nothing new', () => {
    const state = after(
      { type: 'added', uploads: [added('a')] },
      { type: 'sending', id: 'a' },
      { type: 'progress', progress: new Map([['a', { sent: 10, total: 100 }]]) },
    );

    expect(uploadsReducer(state, { type: 'progress', progress: new Map([['a', { sent: 10, total: 100 }]]) })).toBe(state);
    expect(uploadsReducer(state, { type: 'progress', progress: new Map() })).toBe(state);
  });

  it('cancels a file that waits and one that is being sent, and neither is a failure', () => {
    const state = after(
      { type: 'added', uploads: [added('a'), added('b')] },
      { type: 'sending', id: 'a' },
      { type: 'cancelled', id: 'a' },
      { type: 'cancelled', id: 'b' },
    );

    expect(state.uploads.map((entry) => entry.state)).toEqual(['cancelled', 'cancelled']);
    expect(totalsOf(state.uploads).failed).toBe(0);
  });

  it('changes nothing when word of a file comes after it was cancelled', () => {
    const cancelled = after(
      { type: 'added', uploads: [added('a')] },
      { type: 'sending', id: 'a' },
      { type: 'cancelled', id: 'a' },
    );

    expect(uploadsReducer(cancelled, { type: 'stored', id: 'a' })).toBe(cancelled);
    expect(uploadsReducer(cancelled, { type: 'failed', id: 'a', reason: 'The upload was stopped' })).toBe(cancelled);
    expect(uploadsReducer(cancelled, { type: 'sending', id: 'a' })).toBe(cancelled);
    expect(uploadsReducer(cancelled, { type: 'progress', progress: new Map([['a', { sent: 9, total: 100 }]]) })).toBe(
      cancelled,
    );
  });

  it('does not cancel what is over', () => {
    const stored = after({ type: 'added', uploads: [added('a')] }, { type: 'sending', id: 'a' }, { type: 'stored', id: 'a' });

    expect(uploadsReducer(stored, { type: 'cancelled', id: 'a' })).toBe(stored);
  });

  it('does not store or fail a file that was never begun', () => {
    const waiting = after({ type: 'added', uploads: [added('a')] });

    expect(uploadsReducer(waiting, { type: 'stored', id: 'a' })).toBe(waiting);
    expect(uploadsReducer(waiting, { type: 'failed', id: 'a', reason: 'no' })).toBe(waiting);
  });

  it('sends again what failed - and only what failed - from the start and without its reason', () => {
    const state = after(
      { type: 'added', uploads: [added('a'), added('b'), added('c'), added('d')] },
      { type: 'sending', id: 'a' },
      { type: 'sending', id: 'b' },
      { type: 'sending', id: 'c' },
      { type: 'progress', progress: new Map([['a', { sent: 60, total: 100 }]]) },
      { type: 'failed', id: 'a', reason: 'No metadata leader' },
      { type: 'stored', id: 'b' },
      { type: 'cancelled', id: 'c' },
    );
    const again = uploadsReducer(state, { type: 'requeued', ids: ['a', 'b', 'c', 'd'] });

    expect(again.uploads.map((entry) => entry.state)).toEqual(['waiting', 'stored', 'cancelled', 'waiting']);
    expect(again.uploads[0]).toMatchObject({ sent: 0 });
    expect(again.uploads[0]?.reason).toBeUndefined();
  });

  it('dismisses what is over and keeps what is on its way, whatever was named', () => {
    const state = after(
      { type: 'added', uploads: [added('a'), added('b'), added('c'), added('d'), added('e')] },
      { type: 'sending', id: 'a' },
      { type: 'sending', id: 'b' },
      { type: 'sending', id: 'c' },
      { type: 'sending', id: 'd' },
      { type: 'stored', id: 'a' },
      { type: 'failed', id: 'b', reason: 'no' },
      { type: 'cancelled', id: 'c' },
    );
    const left = uploadsReducer(state, { type: 'dismissed', ids: ['a', 'b', 'c', 'd', 'e'] });

    // d is being sent and e waits: both stay.
    expect(left.uploads.map((entry) => entry.id)).toEqual(['d', 'e']);
    expect(uploadsReducer(left, { type: 'dismissed', ids: ['d', 'e', 'nobody'] })).toBe(left);
  });

  it('dismisses only what was named', () => {
    const state = after(
      { type: 'added', uploads: [added('a'), added('b')] },
      { type: 'sending', id: 'a' },
      { type: 'sending', id: 'b' },
      { type: 'stored', id: 'a' },
      { type: 'stored', id: 'b' },
    );

    expect(uploadsReducer(state, { type: 'dismissed', ids: ['b'] }).uploads.map((entry) => entry.id)).toEqual(['a']);
  });
});

describe('what is read from the uploads', () => {
  it('says which files are on their way', () => {
    expect(inFlight(upload('a', 'waiting'))).toBe(true);
    expect(inFlight(upload('a', 'sending'))).toBe(true);
    expect(inFlight(upload('a', 'stored'))).toBe(false);
    expect(inFlight(upload('a', 'failed'))).toBe(false);
    expect(inFlight(upload('a', 'cancelled'))).toBe(false);
  });

  it('counts how they stand, and the bytes of what is stored or on its way', () => {
    const totals = totalsOf([
      upload('a', 'waiting', { total: 100 }),
      upload('b', 'sending', { sent: 40, total: 200 }),
      // Stored is all of it, whatever was last reported.
      upload('c', 'stored', { sent: 10, total: 300 }),
      // Neither of these is going to arrive: their bytes are in neither sum.
      upload('d', 'failed', { sent: 50, total: 1000 }),
      upload('e', 'cancelled', { sent: 60, total: 2000 }),
    ]);

    expect(totals).toEqual({
      files: 5,
      waiting: 1,
      sending: 1,
      stored: 1,
      failed: 1,
      undecided: 0,
      cancelled: 1,
      inFlight: 2,
      bytesSent: 340,
      bytesTotal: 600,
    });
  });

  it('counts among the failed those of which nobody knows whether they are stored', () => {
    const totals = totalsOf([
      upload('a', 'failed', { undecided: true }),
      upload('b', 'failed'),
      // Only a failed file can be undecided: the mark means nothing on another.
      upload('c', 'stored', { undecided: true }),
    ]);

    expect(totals.failed).toBe(2);
    expect(totals.undecided).toBe(1);
  });

  it('marks a file the API was still working on as undecided, and forgets the mark when it is sent again or stored', () => {
    const sending = after({ type: 'added', uploads: [added('a'), added('b')] }, { type: 'sending', id: 'a' }, { type: 'sending', id: 'b' });
    const failed = uploadsReducer(
      uploadsReducer(sending, { type: 'failed', id: 'a', reason: 'still working', undecided: true }),
      { type: 'failed', id: 'b', reason: 'refused' },
    );
    expect(failed.uploads.map((entry) => entry.undecided)).toEqual([true, undefined]);

    const again = uploadsReducer(failed, { type: 'requeued', ids: ['a', 'b'] });
    expect(again.uploads.map((entry) => [entry.state, entry.undecided, entry.reason])).toEqual([
      ['waiting', undefined, undefined],
      ['waiting', undefined, undefined],
    ]);
  });

  it('requeues and dismisses many files by their ids, and only those', () => {
    const ids = Array.from({ length: 50 }, (_, index) => `f${String(index)}`);
    let state = after({ type: 'added', uploads: ids.map((id) => added(id)) });
    for (const id of ids) {
      state = uploadsReducer(state, { type: 'sending', id });
      state = uploadsReducer(state, { type: 'failed', id, reason: 'no' });
    }
    const even = ids.filter((_, index) => index % 2 === 0);

    const again = uploadsReducer(state, { type: 'requeued', ids: even });
    expect(again.uploads.filter((entry) => entry.state === 'waiting').map((entry) => entry.id)).toEqual(even);
    // What waits again is on its way and is not dismissed; what failed and is named, is.
    const left = uploadsReducer(again, { type: 'dismissed', ids });
    expect(left.uploads.map((entry) => entry.id)).toEqual(even);
  });

  it('says which batches have a file on its way', () => {
    const uploads = [
      upload('a', 'stored', { batch: 'one' }),
      upload('b', 'failed', { batch: 'one' }),
      upload('c', 'stored', { batch: 'two' }),
      upload('d', 'sending', { batch: 'two' }),
      upload('e', 'waiting', { batch: 'three' }),
      upload('f', 'cancelled', { batch: 'four' }),
    ];

    expect([...batchesInFlight(uploads)]).toEqual(['two', 'three']);
    expect([...batchesInFlight([])]).toEqual([]);
    expect(ofBatches(uploads, new Set(['one', 'four'])).map((entry) => entry.id)).toEqual(['a', 'b', 'f']);
    expect(ofBatches(uploads, new Set())).toEqual([]);
  });

  it('counts as under way every file of a batch that has one on its way, and none of a batch that is over', () => {
    const uploads = [
      // Over, and not dismissed: an old failure and what was stored with it.
      upload('a', 'stored', { batch: 'old' }),
      upload('b', 'failed', { batch: 'old' }),
      // On its way: one stored already, one being sent.
      upload('c', 'stored', { batch: 'now', total: 10 }),
      upload('d', 'sending', { batch: 'now', sent: 5, total: 10 }),
    ];

    expect(underWay(uploads).map((entry) => entry.id)).toEqual(['c', 'd']);
    // The line of the bar is made of these: one batch, and no failure of another.
    expect(totalsOf(underWay(uploads))).toMatchObject({ files: 2, stored: 1, failed: 0, inFlight: 1, bytesSent: 15, bytesTotal: 20 });
    // Nothing on its way: nothing is under way, and what is not dismissed is what there is to say.
    const over = uploads.map((entry) => (entry.id === 'd' ? { ...entry, state: 'stored' as const } : entry));
    expect(underWay(over)).toEqual([]);
    expect(totalsOf(over)).toMatchObject({ files: 4, stored: 3, failed: 1, inFlight: 0 });
    // A later run that went well is told as it went: its batch alone has no failure.
    expect(totalsOf(ofBatches(over, new Set(['now'])))).toMatchObject({ files: 2, stored: 2, failed: 0 });
  });

  it('counts nothing as nothing', () => {
    expect(totalsOf([])).toMatchObject({ files: 0, inFlight: 0, bytesSent: 0, bytesTotal: 0 });
  });

  it('never counts more sent than there is to send', () => {
    expect(totalsOf([upload('a', 'sending', { sent: 150, total: 100 })]).bytesSent).toBe(100);
  });

  it('groups the files by batch, the batches in the order they were begun', () => {
    const batches = batchesOf([
      upload('a', 'stored', { batch: 'one', bucket: 'photos' }),
      upload('b', 'sending', { batch: 'two', bucket: 'films' }),
      upload('c', 'waiting', { batch: 'one', bucket: 'photos' }),
    ]);

    expect(batches.map((batch) => [batch.id, batch.bucket, batch.uploads.map((entry) => entry.id)])).toEqual([
      ['one', 'photos', ['a', 'c']],
      ['two', 'films', ['b']],
    ]);
  });

  it('tells whether a file is among those meant: of a batch, of a bucket, of both, or any', () => {
    const file = upload('a', 'failed', { batch: 'one', bucket: 'photos' });

    expect(inScope(file, {})).toBe(true);
    expect(inScope(file, { batch: 'one' })).toBe(true);
    expect(inScope(file, { batch: 'two' })).toBe(false);
    expect(inScope(file, { bucket: 'photos' })).toBe(true);
    expect(inScope(file, { bucket: 'films' })).toBe(false);
    expect(inScope(file, { batch: 'one', bucket: 'films' })).toBe(false);
  });
});
