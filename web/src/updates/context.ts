import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';

import { api, unwrap, type UpdatesView } from '../api/client';

/** What the console knows of releases for its "update available" badge. */
export interface UpdatesNotice {
  /**
   * Whether the API, when last asked, listed a release that may be installed
   * from the running one. False until it answered, and whenever it would not
   * or could not say.
   */
  available: boolean;
  /** Tells what `GET /updates` or a check answered: the badge follows it. */
  report: (view: UpdatesView) => void;
}

// A screen shown by itself - in a test, say - has no badge to tell.
const NOBODY: UpdatesNotice = { available: false, report: () => undefined };

export const UpdatesNoticeContext = createContext<UpdatesNotice>(NOBODY);

/** The badge's state, for a screen that learns something about releases. */
export function useUpdatesNotice(): UpdatesNotice {
  return useContext(UpdatesNoticeContext);
}

/**
 * Whether the view offers a release to install: the API names one
 * (`next_step`), or says of one that it is allowed. Whether this deployment
 * can install it from the console (`upgrade_available`) is another matter,
 * and does not make a release appear.
 */
export function offersRelease(view: UpdatesView): boolean {
  return (view.next_step !== undefined && view.next_step !== '') || view.available.some((release) => release.allowed);
}

/**
 * Asks `GET /updates` once, when the signed-in console is shown, and keeps
 * whether a release is offered. Any refusal - 401, 403, a 501 or a 404 of a
 * node of an earlier release - hides the badge and says nothing: the badge
 * is a hint, and the Updates screen says why when it is opened. What a
 * screen reports later replaces what was asked.
 */
export function useAskedUpdates(): UpdatesNotice {
  const [available, setAvailable] = useState(false);
  // Whether a screen has reported since: an answer to the first question
  // that comes after it is older than what was reported, and is not kept.
  const reported = useRef(false);

  useEffect(() => {
    let current = true;
    const stop = new AbortController();
    unwrap(api.GET('/updates', { signal: stop.signal })).then(
      (view) => {
        if (current && !reported.current) {
          setAvailable(offersRelease(view));
        }
      },
      () => {
        if (current && !reported.current) {
          setAvailable(false);
        }
      },
    );
    return () => {
      current = false;
      stop.abort();
    };
  }, []);

  const report = useCallback((view: UpdatesView) => {
    reported.current = true;
    setAvailable(offersRelease(view));
  }, []);

  return useMemo(() => ({ available, report }), [available, report]);
}
