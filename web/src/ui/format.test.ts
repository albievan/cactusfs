import { describe, expect, it } from 'vitest';

import {
  formatBytes,
  formatExactBytes,
  formatGigabytes,
  formatPolicyDocument,
  formatTime,
  joinNames,
  MOST_POLICY_BYTES,
  parseNames,
  parsePolicyDocument,
  policyLayout,
  typedPolicy,
  utf8Bytes,
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

  it('sends a typed policy as it was typed, a key given twice included, and nothing that is not a JSON object', () => {
    const text = '{\n  "Version": "2012-10-17",\n  "Version": "x"\n}';
    const typed = typedPolicy('policy', text);
    // JavaScript keeps the last of the two; the body keeps both, for the API to refuse.
    expect(typed.document).toEqual({ Version: 'x' });
    expect(typed.body).toBe(`{"policy":${text}}`);
    expect(typed.body.match(/"Version"/g)).toHaveLength(2);
    expect(() => JSON.parse(typed.body) as unknown).not.toThrow();
    expect(typedPolicy('document', ' {} ').body).toBe('{"document": {} }');
    expect(() => typedPolicy('policy', '')).toThrow(/not valid JSON/);
    expect(() => typedPolicy('policy', '{} {}')).toThrow(/not valid JSON/);
    expect(() => typedPolicy('policy', '"x"')).toThrow(/must be a JSON object/);
  });

  it('puts the other fields of a body before the policy, as JSON encodes them', () => {
    const text = '{"Effect":"Deny","Effect":"Allow"}';
    const typed = typedPolicy('document', text, { name: 'p "1"' });
    expect(typed.body).toBe(`{"name":"p \\"1\\"","document":${text}}`);
    expect(JSON.parse(typed.body)).toEqual({ name: 'p "1"', document: { Effect: 'Allow' } });
    // A field without a value is left out, as the generated client leaves it.
    expect(typedPolicy('inline_policy', '{}', { description: undefined, expires_at: 'x' }).body).toBe(
      '{"expires_at":"x","inline_policy":{}}',
    );
    expect(typedPolicy('inline_policy', '{}', { description: undefined }).body).toBe('{"inline_policy":{}}');
    expect(() => typedPolicy('document', '{}', { document: {} })).toThrow(/given twice/);
  });

  it('lays a stored policy out indented, or compact when indented it would be larger than the API takes', () => {
    const small = { Version: '2012-10-17', Statement: [] };
    expect(policyLayout(small)).toEqual({ text: formatPolicyDocument(small), compact: false });

    const large = {
      Version: '2012-10-17',
      Statement: Array.from({ length: 140 }, (_, i) => ({
        Sid: `s${String(i)}`,
        Effect: 'Allow',
        Action: ['s3:GetObject', 's3:PutObject'],
        Resource: `arn:aws:s3:::photos/reports/${String(i)}/*`,
      })),
    };
    expect(utf8Bytes(formatPolicyDocument(large))).toBeGreaterThan(MOST_POLICY_BYTES);
    const laid = policyLayout(large);
    expect(laid).toEqual({ text: JSON.stringify(large), compact: true });
    expect(utf8Bytes(laid.text)).toBeLessThanOrEqual(MOST_POLICY_BYTES);
    // Bytes, not characters: what is sent is UTF-8.
    expect(utf8Bytes('é')).toBe(2);
    expect(MOST_POLICY_BYTES).toBe(20480);
  });
});
