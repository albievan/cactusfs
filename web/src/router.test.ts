import { describe, expect, it } from 'vitest';

import { parseHash, routeHref } from './router';

describe('the router', () => {
  it('splits a fragment into segments and a query', () => {
    const route = parseHash('#/buckets/photos?prefix=2026%2F');

    expect(route.segments).toEqual(['buckets', 'photos']);
    expect(route.query.get('prefix')).toBe('2026/');
  });

  it('reads an empty fragment as no segments', () => {
    expect(parseHash('').segments).toEqual([]);
    expect(parseHash('#/').segments).toEqual([]);
  });

  it('keeps a segment that is not valid percent-encoding as it is', () => {
    expect(parseHash('#/users/%E0%A4%A').segments).toEqual(['users', '%E0%A4%A']);
  });

  it('builds an address that parses back to the same place', () => {
    const href = routeHref(['users', 'a/b c'], { prefix: 'x/y z&' });
    const route = parseHash(href);

    expect(href.startsWith('#/users/a%2Fb%20c?')).toBe(true);
    expect(route.segments).toEqual(['users', 'a/b c']);
    expect(route.query.get('prefix')).toBe('x/y z&');
  });
});
