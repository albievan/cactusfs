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

  it('reads a plus that was typed into the address as a plus', () => {
    const route = parseHash('#/search?q=rx:^a+b$');

    expect(route.segments).toEqual(['search']);
    expect(route.query.get('q')).toBe('rx:^a+b$');
    // Encoded or not, a plus is a plus and a space is a space.
    expect(parseHash('#/search?q=a%2Bb').query.get('q')).toBe('a+b');
    expect(parseHash('#/search?q=a%20b').query.get('q')).toBe('a b');
    expect(parseHash('#/search?q=a b').query.get('q')).toBe('a b');
  });

  it.each([
    ['a plus', 'rx:^a+b$'],
    ['a percent sign', '100%'],
    ['a percent sign before two digits', '50%25 off %2B'],
    ['an ampersand', 'a&b=c'],
    ['a hash', 'issue #1 #2'],
    ['spaces', ' two  spaces '],
    ['a question mark and an equals sign', 'what?=x'],
    ['a slash', '2026/q1/'],
    ['letters beyond ASCII', 'żółć/日本語/🌵'],
  ])('builds a query with %s that parses back to what it was', (_, value) => {
    const href = routeHref(['search'], { q: value, 'na me+&': value });
    const route = parseHash(href);

    expect(route.segments).toEqual(['search']);
    expect(route.query.get('q')).toBe(value);
    expect(route.query.get('na me+&')).toBe(value);
    expect([...route.query.keys()]).toEqual(['q', 'na me+&']);
    // Nothing in the address needs the reader to guess: no bare plus, space, ampersand or hash in a value.
    expect(href.slice(href.indexOf('?') + 1)).toMatch(/^q=[^+ &#]*&na%20me%2B%26=[^+ &#]*$/);
  });

  it('builds the same address for the same place as a segment with those characters', () => {
    for (const name of ['a+b', '100%', 'a&b', 'a#b', 'a b', 'a?b', 'żółć']) {
      expect(parseHash(routeHref(['buckets', name], { prefix: name })).segments).toEqual(['buckets', name]);
      expect(parseHash(routeHref(['buckets', name], { prefix: name })).query.get('prefix')).toBe(name);
    }
  });

  it('reads a query that is not well-formed as well as it can', () => {
    const route = parseHash('#/search?q=100%&&flag&=x&q=second&bad=%E0%A4%A');

    expect(route.query.get('q')).toBe('100%');
    expect(route.query.getAll('q')).toEqual(['100%', 'second']);
    expect(route.query.get('flag')).toBe('');
    expect(route.query.get('')).toBe('x');
    expect(route.query.get('bad')).toBe('%E0%A4%A');
    expect(parseHash('#/search?').query.toString()).toBe('');
  });
});
