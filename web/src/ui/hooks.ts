import { useCallback, useEffect, useRef, useState } from 'react';

import { useNoticeActions } from '../notices/context';
import type { NoticeLink } from '../notices/state';
import { reasonOf } from './reason';

/** What useLoad knows about the data it fetches. */
export interface Loaded<T> {
  /** The data of the last successful load. */
  data: T | undefined;
  /** Why the last load failed, or null. */
  error: unknown;
  /** True until the first load has finished. */
  loading: boolean;
  /** Loads again. The data on screen stays until the new data arrives. */
  reload: () => void;
}

interface LoadState<T> {
  data: T | undefined;
  error: unknown;
  loading: boolean;
}

/**
 * Loads data when the component mounts and whenever `load` changes, so pass a
 * function made with useCallback.
 *
 * `load` is handed a signal that is aborted when its answer is no longer
 * wanted: the component went away, `load` changed or the data is loaded
 * again. A loader that passes the signal on to its request has the request
 * given up then, so that the API does not go on working for nobody; one that
 * takes no notice of it behaves as it always did.
 */
export function useLoad<T>(load: (signal: AbortSignal) => Promise<T>): Loaded<T> {
  const [state, setState] = useState<LoadState<T>>({ data: undefined, error: null, loading: true });
  const [round, setRound] = useState(0);

  useEffect(() => {
    let current = true;
    const abandoned = new AbortController();
    load(abandoned.signal).then(
      (data) => {
        if (current) {
          setState({ data, error: null, loading: false });
        }
      },
      (error: unknown) => {
        if (current) {
          setState((previous) => ({ data: previous.data, error, loading: false }));
        }
      },
    );
    return () => {
      current = false;
      abandoned.abort();
    };
  }, [load, round]);

  const reload = useCallback(() => {
    setRound((value) => value + 1);
  }, []);

  return { ...state, reload };
}

/** What is said, as a notice, of how a change ended. */
export interface Outcome {
  /** What is said when the change was carried out, for example "Created bucket photos". */
  done: string;
  /**
   * What is said when it was not and the screen that asked for it is no
   * longer there to show why; the reason the API gave goes with it.
   */
  failed: string;
  /** Where the notice points. */
  link?: NoticeLink;
}

/** What useAction knows about the change it carries out. */
export interface Action {
  /** True while a change is being carried out. */
  busy: boolean;
  /** Why the last change failed, or null. */
  error: unknown;
  /**
   * Carries out a change. Resolves to true when it succeeded. With an
   * outcome, how it ended is said as a notice as well: that it was done,
   * or - when the screen was left meanwhile - that it was not.
   */
  run: (change: () => Promise<void>, outcome?: Outcome) => Promise<boolean>;
  /** Shows an error that was found before anything was sent. */
  fail: (error: unknown) => void;
}

/** Carries out changes one at a time and keeps the error of the last one. */
export function useAction(): Action {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const { notify } = useNoticeActions();
  // Whether the screen that carries the change out is still shown.
  const shown = useRef(true);
  useEffect(() => {
    shown.current = true;
    return () => {
      shown.current = false;
    };
  }, []);

  const run = useCallback(
    async (change: () => Promise<void>, outcome?: Outcome) => {
      setBusy(true);
      setError(null);
      try {
        await change();
        if (outcome !== undefined) {
          notify({ kind: 'success', title: outcome.done, link: outcome.link });
        }
        return true;
      } catch (cause) {
        setError(cause);
        // The screen shows why while it is there; once it is not, the
        // notice is the only place left to say it.
        if (outcome !== undefined && !shown.current) {
          notify({
            kind: 'error',
            title: outcome.failed,
            detail: reasonOf(cause, 'The request failed'),
            link: outcome.link,
          });
        }
        return false;
      } finally {
        setBusy(false);
      }
    },
    [notify],
  );

  return { busy, error, run, fail: setError };
}
