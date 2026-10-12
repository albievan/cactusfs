import { useMemo, useReducer, type ReactNode } from 'react';

import { NoticesActionsContext, NoticesContext, type NoticesActions } from './context';
import { NO_NOTICES, noticesReducer } from './state';

/**
 * Keeps the notices of the visit for everything beneath it. They are in
 * memory only, and are not written to localStorage or anywhere else: a
 * notice may name a bucket or a key, and ends with the session it was said
 * in.
 */
export function NoticesProvider({ children }: { children: ReactNode }) {
  const [state, dispatch] = useReducer(noticesReducer, NO_NOTICES);

  // What says and dismisses never changes: whoever only says something is
  // not rendered again when something is said.
  const actions = useMemo<NoticesActions>(
    () => ({
      notify: (notice) => {
        dispatch({ type: 'said', notice, at: new Date().toISOString() });
      },
      dismiss: (id) => {
        dispatch({ type: 'dismissed', id });
      },
      dismissAll: () => {
        dispatch({ type: 'cleared' });
      },
    }),
    [],
  );

  return (
    <NoticesActionsContext.Provider value={actions}>
      <NoticesContext.Provider value={state.notices}>{children}</NoticesContext.Provider>
    </NoticesActionsContext.Provider>
  );
}
