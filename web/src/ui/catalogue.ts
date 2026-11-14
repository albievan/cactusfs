import { api, unwrap, type TagDefinition } from '../api/client';
import { useLoad } from './hooks';

function loadCatalogue(signal: AbortSignal): Promise<TagDefinition[]> {
  return unwrap(api.GET('/tag-definitions', { signal }));
}

/**
 * The deployment's catalogue of tags (ADR-0018), to offer its keys and values
 * while tags are typed; undefined until it is read, and when it cannot be.
 * It is a suggestion and never a condition: an editor works without it, and
 * a caller who may not list the catalogue is told nothing of it here.
 */
export function useCatalogue(): TagDefinition[] | undefined {
  return useLoad(loadCatalogue).data;
}
