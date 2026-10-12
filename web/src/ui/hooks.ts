import { useCallback, useEffect, useState } from 'react';

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

/** What useAction knows about the change it carries out. */
export interface Action {
  /** True while a change is being carried out. */
  busy: boolean;
  /** Why the last change failed, or null. */
  error: unknown;
  /** Carries out a change. Resolves to true when it succeeded. */
  run: (change: () => Promise<void>) => Promise<boolean>;
  /** Shows an error that was found before anything was sent. */
  fail: (error: unknown) => void;
}

/** Carries out changes one at a time and keeps the error of the last one. */
export function useAction(): Action {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const run = useCallback(async (change: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await change();
      return true;
    } catch (cause) {
      setError(cause);
      return false;
    } finally {
      setBusy(false);
    }
  }, []);

  return { busy, error, run, fail: setError };
}
