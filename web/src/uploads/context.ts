import { createContext, useContext, useSyncExternalStore } from 'react';

import type { UploadsActions, UploadsEngine } from './engine';
import type { Upload } from './state';

export const UploadsContext = createContext<UploadsEngine | null>(null);

function engineOf(engine: UploadsEngine | null): UploadsEngine {
  if (engine === null) {
    throw new Error('CactusFS console: uploads are used outside the UploadsProvider');
  }
  return engine;
}

/**
 * What can be done with uploads: send files off, stop them, send again what
 * failed, dismiss what is over, and listen for what was stored. It never
 * changes, and whoever uses only this is not rendered again as files
 * progress.
 */
export function useUploads(): UploadsActions {
  return engineOf(useContext(UploadsContext));
}

/**
 * The uploads of the visit that were not dismissed, in the order they were
 * asked for. The component renders again when one of them changes - a few
 * times a second while files are sent - so it is for the parts that show
 * progress, and not for a whole screen.
 */
export function useUploadList(): Upload[] {
  const engine = engineOf(useContext(UploadsContext));
  return useSyncExternalStore(engine.subscribe, engine.getState).uploads;
}
