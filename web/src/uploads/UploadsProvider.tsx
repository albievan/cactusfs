import { useEffect, useState, useSyncExternalStore, type ReactNode } from 'react';

import { useNoticeActions } from '../notices/context';
import { UploadsContext } from './context';
import { createUploads } from './engine';

/**
 * Gives everything beneath it the uploads of the console. It stands above
 * the screens, so that an upload goes on when another screen is shown, and
 * inside the signed-in part of the console, so that uploads end with the
 * session: when it goes away - signing out, a session that has expired -
 * what is on its way is given up.
 *
 * Leaving the page is another matter. A page that is reloaded, closed or
 * sent to another address takes its requests with it, and nothing in it can
 * keep them alive: that would take a service worker, which the console does
 * not have. So while files are on their way the browser is asked to ask
 * first.
 */
export function UploadsProvider({ children }: { children: ReactNode }) {
  const { notify } = useNoticeActions();
  // One for as long as the provider is there. `notify` never changes.
  const [engine] = useState(() => createUploads(notify));

  useEffect(
    () => () => {
      engine.dispose();
    },
    [engine],
  );

  // Only whether anything is on its way: it changes when uploads begin and
  // when the last of them ends, not as they progress.
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

  return <UploadsContext.Provider value={engine}>{children}</UploadsContext.Provider>;
}
