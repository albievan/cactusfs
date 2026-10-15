// What is known about the deletions of the visit. This file holds no React
// and sends nothing: a pure reducer, and what is read from its state and said
// of it.

/**
 * What a deletion is of: the keys that were ticked on a page, or every
 * object of the bucket, found by walking its listing.
 */
export type DeletionKind = 'selected' | 'all';

/**
 * How many objects a deletion of every object was told to expect, as the
 * bucket's usage counted them when it began. `atLeast` is set where the
 * count stopped at its bound: the bucket holds that many or more.
 */
export interface Expected {
  count: number;
  atLeast: boolean;
}

/** Why some objects were not deleted, in the words of the API, and of how many. */
export interface Refusal {
  reason: string;
  count: number;
}

/** One deletion of several objects of a bucket, and how far it is. */
export interface Deletion {
  /** Which deletion this is, among all of the visit. */
  id: string;
  bucket: string;
  kind: DeletionKind;
  /** For `all`: what the usage of the bucket said when it began, if it said anything. */
  expected?: Expected;
  /** The keys found so far: of `selected` all of them from the start; of `all` as the listing is walked. */
  listed: number;
  /** Whether every key there is to delete has been found: the listing has no page left. */
  listedAll: boolean;
  /** Deleted, as the API answered. */
  deleted: number;
  /** Refused by the API, or failed: not deleted. */
  refused: number;
  /** Why, as the API said it, first reason first. */
  refusals: Refusal[];
  /** Not sent, because the deletion was cancelled or stopped before their turn came. */
  notBegun: number;
  /** Whether it is still on its way. */
  running: boolean;
  /** Whether somebody cancelled it. */
  cancelled: boolean;
  /** Why it stopped before it was over, where it did: the listing failed, the session was locked. */
  stopped?: string;
}

/** The deletions of the visit that were not dismissed, in the order they were asked for. */
export interface DeletionsState {
  deletions: Deletion[];
}

/** A deletion that begins. */
export interface NewDeletion {
  id: string;
  bucket: string;
  kind: DeletionKind;
  expected?: Expected;
  /** The keys of `selected`, which are known from the start. */
  keys?: number;
}

export type DeletionsAction =
  | { type: 'begun'; deletion: NewDeletion }
  | { type: 'listed'; id: string; keys: number; last: boolean }
  | { type: 'deleted'; id: string }
  | { type: 'refused'; id: string; reason: string }
  | { type: 'skipped'; id: string; keys: number }
  | { type: 'cancelled'; id: string }
  | { type: 'stopped'; id: string; reason: string }
  | { type: 'over'; id: string }
  | { type: 'dismissed'; ids: readonly string[] };

/** No deletions yet. */
export const NO_DELETIONS: DeletionsState = { deletions: [] };

// Changes the deletion of that id; the state is the one it was when there is none.
function changeOne(state: DeletionsState, id: string, each: (deletion: Deletion) => Deletion): DeletionsState {
  let changed = false;
  const deletions = state.deletions.map((deletion) => {
    if (deletion.id !== id) {
      return deletion;
    }
    const next = each(deletion);
    changed ||= next !== deletion;
    return next;
  });
  return changed ? { deletions } : state;
}

/** What becomes of the deletions when something happens to one of them. */
export function deletionsReducer(state: DeletionsState, action: DeletionsAction): DeletionsState {
  switch (action.type) {
    case 'begun': {
      const { id, bucket, kind, expected, keys } = action.deletion;
      return {
        deletions: [
          ...state.deletions,
          {
            id,
            bucket,
            kind,
            expected,
            listed: keys ?? 0,
            listedAll: keys !== undefined,
            deleted: 0,
            refused: 0,
            refusals: [],
            notBegun: 0,
            running: true,
            cancelled: false,
          },
        ],
      };
    }
    case 'listed':
      return changeOne(state, action.id, (d) => ({ ...d, listed: d.listed + action.keys, listedAll: action.last }));
    case 'deleted':
      return changeOne(state, action.id, (d) => ({ ...d, deleted: d.deleted + 1 }));
    case 'refused':
      return changeOne(state, action.id, (d) => {
        const known = d.refusals.find((refusal) => refusal.reason === action.reason);
        const refusals =
          known === undefined
            ? [...d.refusals, { reason: action.reason, count: 1 }]
            : d.refusals.map((refusal) => (refusal === known ? { ...refusal, count: refusal.count + 1 } : refusal));
        return { ...d, refused: d.refused + 1, refusals };
      });
    case 'skipped':
      return changeOne(state, action.id, (d) => ({ ...d, notBegun: d.notBegun + action.keys }));
    case 'cancelled':
      return changeOne(state, action.id, (d) => (d.running && !d.cancelled ? { ...d, cancelled: true } : d));
    case 'stopped':
      return changeOne(state, action.id, (d) => (d.stopped === undefined ? { ...d, stopped: action.reason } : d));
    case 'over':
      return changeOne(state, action.id, (d) => (d.running ? { ...d, running: false } : d));
    case 'dismissed': {
      const deletions = state.deletions.filter((d) => d.running || !action.ids.includes(d.id));
      return deletions.length === state.deletions.length ? state : { deletions };
    }
  }
}

/**
 * A count as the deletions say it: digits in groups of three with a space
 * between them - "1 200", "5 000" - which reads the same in every language.
 */
export function countOf(n: number): string {
  return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ' ');
}

/** "1 object", "1 200 objects". */
export function objectsOf(n: number): string {
  return n === 1 ? '1 object' : `${countOf(n)} objects`;
}

/** How many have been answered: deleted or refused. */
export function doneOf(deletion: Deletion): number {
  return deletion.deleted + deletion.refused;
}

/**
 * Of how many, as far as it is known: every key, once the listing has been
 * walked to its end; until then what the usage of the bucket said, or - when
 * more have been found than it said, or it said nothing - what was found,
 * which is a least.
 */
export function totalOf(deletion: Deletion): { count: number; atLeast: boolean } {
  if (deletion.listedAll) {
    return { count: deletion.listed, atLeast: false };
  }
  const { expected } = deletion;
  if (expected !== undefined && expected.count >= deletion.listed) {
    return expected;
  }
  return { count: deletion.listed, atLeast: true };
}

/**
 * The line of a deletion on its way: "Deleting 1 200 of 5 000 from photos",
 * with how many were not deleted so far. `withBucket` false leaves the
 * bucket out, for a page that names none.
 */
export function progressLineOf(deletion: Deletion, withBucket = true): string {
  const total = totalOf(deletion);
  const of = `${total.atLeast ? 'at least ' : ''}${countOf(total.count)}`;
  const parts = [`Deleting ${countOf(doneOf(deletion))} of ${of}${withBucket ? ` from ${deletion.bucket}` : ''}`];
  if (deletion.refused > 0) {
    parts.push(`${countOf(deletion.refused)} not deleted`);
  }
  if (deletion.cancelled) {
    parts.push('cancelling');
  }
  return parts.join(' · ');
}

/** Why some were not deleted, in one line: each reason with how many, the first reason first. */
export function reasonsOf(deletion: Deletion): string {
  return deletion.refusals
    .map((refusal) => (refusal.count === 1 ? refusal.reason : `${refusal.reason} (${countOf(refusal.count)} objects)`))
    .join('; ');
}

/** What is said of a deletion that is over: a title, and why some were not deleted. */
export interface Outcome {
  kind: 'success' | 'error' | 'info';
  title: string;
  detail?: string;
}

/**
 * How a deletion ended, in words: what was deleted, what was not and why,
 * what was cancelled or never begun - and, of every object of a bucket, that
 * an object stored while it ran may be left.
 */
export function outcomeOf(deletion: Deletion): Outcome {
  const { bucket, deleted, refused, notBegun } = deletion;
  const details: string[] = [];
  if (deletion.stopped !== undefined) {
    details.push(deletion.stopped);
  }
  if (refused > 0) {
    details.push(`Not deleted: ${reasonsOf(deletion)}`);
  }
  if (deletion.kind === 'all') {
    details.push('An object stored in the bucket while the deletion ran may be left.');
  }
  const detail = details.length === 0 ? undefined : details.join(' ');
  const notDeleted = refused > 0 ? `, ${countOf(refused)} not deleted` : '';
  if (deletion.cancelled) {
    const left = notBegun > 0 ? `, ${countOf(notBegun)} not begun` : '';
    return {
      kind: refused > 0 ? 'error' : 'info',
      title: `The deletion from ${bucket} was cancelled: ${objectsOf(deleted)} deleted${notDeleted}${left}`,
      detail,
    };
  }
  if (deletion.stopped !== undefined) {
    return {
      kind: 'error',
      title: `Deleted ${objectsOf(deleted)} from ${bucket}${notDeleted}, and stopped`,
      detail,
    };
  }
  if (refused > 0) {
    return {
      kind: 'error',
      title: `Deleted ${countOf(deleted)} of ${objectsOf(deleted + refused)} from ${bucket}: ${countOf(refused)} not deleted`,
      detail,
    };
  }
  return {
    kind: 'success',
    title:
      deletion.kind === 'all'
        ? `Deleted every object of ${bucket}: ${objectsOf(deleted)}`
        : `Deleted ${objectsOf(deleted)} from ${bucket}`,
    detail,
  };
}
