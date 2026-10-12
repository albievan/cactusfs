// What the console did and how it ended, kept for the visit. This file is the
// whole of what is known about notices and holds no React: a pure reducer.

/** How a notice reads: something was done, something was not, or something to know. */
export type NoticeKind = 'success' | 'error' | 'info';

/** Where a notice points: a place in the console. */
export interface NoticeLink {
  /** The address, as routeHref builds it. */
  href: string;
  /** What the link says. */
  label: string;
}

/** What is said when something happened. */
export interface NoticeInput {
  kind: NoticeKind;
  /** What happened, in one line. */
  title: string;
  /** More of it: as a rule, what the API answered. */
  detail?: string;
  link?: NoticeLink;
}

/** A notice that is kept. */
export interface Notice extends NoticeInput {
  /** Its number in the visit: later ones are larger. */
  id: number;
  /** When it was said, as an ISO date-time. */
  at: string;
}

/** The notices of the visit, newest first. */
export interface NoticesState {
  notices: Notice[];
  /** The number the next notice gets. */
  next: number;
}

export type NoticesAction =
  | { type: 'said'; notice: NoticeInput; at: string }
  | { type: 'dismissed'; id: number }
  | { type: 'cleared' };

/** How many notices are kept: the oldest go when there are more. */
export const NOTICES_KEPT = 50;

/** No notices yet. */
export const NO_NOTICES: NoticesState = { notices: [], next: 1 };

/** What becomes of the notices when something is said or dismissed. */
export function noticesReducer(state: NoticesState, action: NoticesAction): NoticesState {
  switch (action.type) {
    case 'said':
      return {
        notices: [{ ...action.notice, id: state.next, at: action.at }, ...state.notices].slice(0, NOTICES_KEPT),
        next: state.next + 1,
      };
    case 'dismissed': {
      const notices = state.notices.filter((notice) => notice.id !== action.id);
      return notices.length === state.notices.length ? state : { ...state, notices };
    }
    case 'cleared':
      return state.notices.length === 0 ? state : { ...state, notices: [] };
  }
}
