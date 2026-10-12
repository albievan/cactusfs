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
  const segments = path
    .split('/')
    .filter((segment) => segment !== '')
    .map(decodeSegment);
  return { segments, query: parseQuery(mark === -1 ? '' : text.slice(mark + 1)) };
}

/**
 * Reads the query of a fragment: `name=value` pairs between `&`, each
 * percent-decoded. This is not the encoding of a form, in which `+` stands
 * for a space: somebody who types `#/search?q=rx:^a+b$` into the address
 * means the plus, and gets it. The pairs are put into a URLSearchParams one
 * by one, which takes them as they are.
 */
function parseQuery(text: string): URLSearchParams {
  const query = new URLSearchParams();
  for (const pair of text.split('&')) {
    if (pair === '') {
      continue;
    }
    const equals = pair.indexOf('=');
    const name = equals === -1 ? pair : pair.slice(0, equals);
    const value = equals === -1 ? '' : pair.slice(equals + 1);
    query.append(decodeSegment(name), decodeSegment(value));
  }
  return query;
}

/** Percent-decodes a part of the address; what is not valid percent-encoding is kept as it is. */
function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/**
 * Builds the `href` of a place in the console. Segments, names and values
 * are percent-encoded - a space is `%20` and a plus `%2B` - which is what
 * parseHash reads back.
 */
export function routeHref(segments: string[], query: Record<string, string> = {}): string {
  const path = segments.map(encodeURIComponent).join('/');
  const search = Object.entries(query)
    .map(([name, value]) => `${encodeURIComponent(name)}=${encodeURIComponent(value)}`)
    .join('&');
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
