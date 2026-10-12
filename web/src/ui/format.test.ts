import { describe, expect, it } from 'vitest';

import {
  formatBytes,
  formatExactBytes,
  formatGigabytes,
  formatPolicyDocument,
  formatTime,
  joinNames,
  parseNames,
  parsePolicyDocument,
} from './format';

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

  it.each([
    [0, '0 GB'],
    [1, '< 0.01 GB'],
    [5 * 1024 * 1024, '< 0.01 GB'],
    [9_999_999, '< 0.01 GB'],
    [10_000_000, '0.01 GB'],
    [250_000_000, '0.25 GB'],
    [1_000_000_000, '1.00 GB'],
    [1_234_567_890, '1.23 GB'],
    [99_990_000_000, '99.99 GB'],
    // What rounds to a hundred is shown as a hundred is.
    [99_999_000_000, '100 GB'],
    [100_000_000_000, '100 GB'],
    [123_456_000_000, '123 GB'],
    [1_234_500_000_000, '1,235 GB'],
  ])('shows %d bytes as %s in gigabytes of 10^9 bytes', (size, text) => {
    expect(formatGigabytes(size)).toBe(text);
  });

  it('shows a number of bytes exactly, for the hint of a rounded one', () => {
    expect(formatExactBytes(0)).toBe('0 bytes');
    expect(formatExactBytes(1)).toBe('1 byte');
    expect(formatExactBytes(5 * 1024 * 1024)).toBe('5,242,880 bytes');
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
