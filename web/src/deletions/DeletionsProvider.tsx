import { useEffect, useState, useSyncExternalStore, type ReactNode } from 'react';

import { useNoticeActions } from '../notices/context';
import { DeletionsContext } from './context';
import { createDeletions } from './engine';

interface DeletionsProviderProps {
  /** The size of a page of the walk of "Delete all": see DeletionsOptions. The console passes none. */
  listingPage?: number;
  children: ReactNode;
}

/**
 * Gives everything beneath it the deletions of the console. It stands where
 * the uploads do - above the screens, so that a deletion goes on when another
 * screen is shown, and inside the signed-in part, so that what has not begun
 * ends with the session.
 *
 * A page that is reloaded, closed or sent to another address takes its
 * requests with it: while objects are being deleted the browser is asked to
 * ask first, as it is while files are uploaded.
 */
export function DeletionsProvider({ listingPage, children }: DeletionsProviderProps) {
  const { notify } = useNoticeActions();
  // One for as long as the provider is there. `notify` never changes.
  const [engine] = useState(() => createDeletions(notify, { listingPage }));

  useEffect(
    () => () => {
      engine.dispose();
    },
    [engine],
  );

  // Only whether anything is on its way: it changes when a deletion begins
  // and when the last one ends, not as objects are deleted.
  const busy = useSyncExternalStore(engine.subscribe, engine.busy);
  useEffect(() => {
    if (!busy) {
      return undefined;
    }
    const ask = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      // What browsers older than the standard go by.
      event.returnValue = true;
    };
    window.addEventListener('beforeunload', ask);
    return () => {
      window.removeEventListener('beforeunload', ask);
    };
  }, [busy]);

  return <DeletionsContext.Provider value={engine}>{children}</DeletionsContext.Provider>;
}
