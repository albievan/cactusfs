import { useEffect, useState, useSyncExternalStore, type ReactNode } from 'react';

import { useNoticeActions } from '../notices/context';
import { UploadsContext } from './context';
import { createUploads } from './engine';
import { batchesInFlight, NO_RUN, ofBatches, runAfter, totalsOf, type UploadTotals } from './state';

interface UploadsProviderProps {
  /**
   * Told how the last run of uploads stands - the batches that are, or were
   * until just now, on their way together - when the provider appears and
   * whenever an upload changes. It is for whoever has to say how the uploads
   * ended once the provider is gone, with the session; pass a function that
   * does not change, and that only keeps what it is told.
   */
  tally?: (lastRun: UploadTotals) => void;
  children: ReactNode;
}

/**
 * Gives everything beneath it the uploads of the console. It stands above
 * the screens, so that an upload goes on when another screen is shown, and
 * inside the signed-in part of the console, so that uploads end with the
 * session: when it goes away - signing out, a session that has ended -
 * what is on its way is given up. A session that is locked has not ended:
 * the provider stays where it is, under the lock, and the uploads go on.
 *
 * Leaving the page is another matter. A page that is reloaded, closed or
 * sent to another address takes its requests with it, and nothing in it can
 * keep them alive: that would take a service worker, which the console does
 * not have. So while files are on their way the browser is asked to ask
 * first.
 */
export function UploadsProvider({ tally, children }: UploadsProviderProps) {
  const { notify } = useNoticeActions();
  // One for as long as the provider is there. `notify` never changes.
  const [engine] = useState(() => createUploads(notify));

  useEffect(
    () => () => {
      engine.dispose();
    },
    [engine],
  );

  useEffect(() => {
    if (tally === undefined) {
      return undefined;
    }
    let run = NO_RUN;
    const tell = () => {
      const { uploads } = engine.getState();
      run = runAfter(run, batchesInFlight(uploads));
      tally(totalsOf(ofBatches(uploads, run.batches)));
    };
    tell();
    return engine.subscribe(tell);
  }, [engine, tally]);

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
