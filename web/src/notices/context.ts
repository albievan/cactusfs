import { createContext, useContext } from 'react';

import type { Notice, NoticeInput } from './state';

/** What can be done with the notices of the visit. */
export interface NoticesActions {
  /** Says what happened. */
  notify: (notice: NoticeInput) => void;
  /** Forgets one notice. */
  dismiss: (id: number) => void;
  /** Forgets every notice. */
  dismissAll: () => void;
}

// A screen that is shown by itself - in a test, say - has nobody to tell:
// what it says is then said to no one, and nothing else is the worse for it.
const NOBODY: NoticesActions = {
  notify: () => undefined,
  dismiss: () => undefined,
  dismissAll: () => undefined,
};

export const NoticesActionsContext = createContext<NoticesActions>(NOBODY);
export const NoticesContext = createContext<Notice[]>([]);

/** Says what happened, and dismisses what was said. It never changes. */
export function useNoticeActions(): NoticesActions {
  return useContext(NoticesActionsContext);
}

/** The notices of the visit, newest first. The component renders again when they change. */
export function useNotices(): Notice[] {
  return useContext(NoticesContext);
}
