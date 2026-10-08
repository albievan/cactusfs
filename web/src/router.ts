import { useMemo, useSyncExternalStore } from 'react';

// A very small router. The place in the console is kept in the fragment of the
// address (`#/buckets/photos?prefix=2026/`), so the server only ever serves one
// page and the back button works.

/** A place in the console: the path segments and the query of the fragment. */
export interface Route {
  /** The decoded segments of the path, without empty ones. */
  segments: string[];
  /** The query that follows the path. */
  query: URLSearchParams;
}

/** Splits a fragment such as `#/buckets/photos?prefix=a/` into a route. */
export function parseHash(hash: string): Route {
  const text = hash.startsWith('#') ? hash.slice(1) : hash;
  const mark = text.indexOf('?');
  const path = mark === -1 ? text : text.slice(0, mark);
  const query = new URLSearchParams(mark === -1 ? '' : text.slice(mark + 1));
  const segments = path
    .split('/')
    .filter((segment) => segment !== '')
    .map(decodeSegment);
  return { segments, query };
}

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/** Builds the `href` of a place in the console. */
export function routeHref(segments: string[], query: Record<string, string> = {}): string {
  const path = segments.map(encodeURIComponent).join('/');
  const search = new URLSearchParams(query).toString();
  return search === '' ? `#/${path}` : `#/${path}?${search}`;
}

/** Goes to a place in the console. */
export function navigate(href: string): void {
  window.location.hash = href;
}

function subscribe(onChange: () => void): () => void {
  window.addEventListener('hashchange', onChange);
  return () => {
    window.removeEventListener('hashchange', onChange);
  };
}

function currentHash(): string {
  return window.location.hash;
}

/** The current place in the console. The component renders again when it changes. */
export function useRoute(): Route {
  const hash = useSyncExternalStore(subscribe, currentHash);
  return useMemo(() => parseHash(hash), [hash]);
}
