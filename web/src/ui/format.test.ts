import { describe, expect, it } from 'vitest';

import { formatBytes, formatPolicyDocument, formatTime, joinNames, parseNames, parsePolicyDocument } from './format';

describe('formatting', () => {
  it.each([
    [0, '0 bytes'],
    [1023, '1023 bytes'],
    [1024, '1.0 KiB'],
    [1536, '1.5 KiB'],
    [5 * 1024 * 1024, '5.0 MiB'],
    [200 * 1024 ** 3, '200 GiB'],
  ])('shows %d bytes as %s', (size, text) => {
    expect(formatBytes(size)).toBe(text);
  });

  it('shows a missing time as a dash and an unreadable one as it is', () => {
    expect(formatTime(undefined)).toBe('-');
    expect(formatTime('not a time')).toBe('not a time');
    expect(formatTime('2026-10-01T00:00:00Z')).toBe(new Date('2026-10-01T00:00:00Z').toLocaleString());
  });

  it('splits names on commas and spaces', () => {
    expect(parseNames(' read-only, auditor  ops ,')).toEqual(['read-only', 'auditor', 'ops']);
    expect(parseNames('')).toEqual([]);
    expect(joinNames(['a', 'b'])).toBe('a, b');
  });

  it('reads a policy document and refuses what is not a JSON object', () => {
    expect(parsePolicyDocument('{"Version":"1"}')).toEqual({ Version: '1' });
    expect(() => parsePolicyDocument('{')).toThrow(/not valid JSON/);
    expect(() => parsePolicyDocument('[1]')).toThrow(/must be a JSON object/);
    expect(formatPolicyDocument({ a: 1 })).toBe('{\n  "a": 1\n}');
  });
});
