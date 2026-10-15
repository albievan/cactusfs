import { createContext, useContext, useSyncExternalStore } from 'react';

import type { DeletionsActions, DeletionsEngine } from './engine';
import type { Deletion } from './state';

export const DeletionsContext = createContext<DeletionsEngine | null>(null);

function engineOf(engine: DeletionsEngine | null): DeletionsEngine {
  if (engine === null) {
    throw new Error('CactusFS console: deletions are used outside the DeletionsProvider');
  }
  return engine;
}

/**
 * What can be done with deletions: delete keys or every object of a bucket,
 * cancel, dismiss what is over, and listen for what was deleted. It never
 * changes, and whoever uses only this is not rendered again as objects are
 * deleted.
 */
export function useDeletions(): DeletionsActions {
  return engineOf(useContext(DeletionsContext));
}

/**
 * The deletions of the visit that were not dismissed, in the order they
 * were asked for. The component renders again whenever an object of one of
 * them is answered, so it is for the parts that show progress, and not for
 * a whole screen.
 */
export function useDeletionList(): Deletion[] {
  const engine = engineOf(useContext(DeletionsContext));
  return useSyncExternalStore(engine.subscribe, engine.getState).deletions;
}

/**
 * Whether a deletion of objects of this bucket is on its way. The component
 * renders again only when that changes, not as objects are deleted.
 */
export function useDeleting(bucket: string): boolean {
  const engine = engineOf(useContext(DeletionsContext));
  return useSyncExternalStore(engine.subscribe, () =>
    engine.getState().deletions.some((deletion) => deletion.running && deletion.bucket === bucket),
  );
}
