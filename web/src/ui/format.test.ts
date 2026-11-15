import { describe, expect, it } from 'vitest';

import {
  formatBytes,
  formatExactBytes,
  formatGigabytes,
  formatPolicyDocument,
  formatShare,
  formatTime,
  gigabytesOf,
  isExactBytes,
  joinNames,
  MOST_POLICY_BYTES,
  parseGigabytes,
  parseNames,
  parsePolicyDocument,
  policyLayout,
  quotaBody,
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
    expect(formatExactBytes(Number.MAX_SAFE_INTEGER)).toBe('9,007,199,254,740,991 bytes');
  });

  it('says "about" of a figure beyond 2^53 - 1 bytes, which the client may have read rounded', () => {
    // 2^62 - 1 as the API wrote it reads as 2^62.
    const read = Number('4611686018427387903');
    expect(isExactBytes(read)).toBe(false);
    expect(isExactBytes(Number.MAX_SAFE_INTEGER)).toBe(true);
    expect(isExactBytes(2 ** 53)).toBe(false);
    expect(formatExactBytes(read)).toMatch(/^about 4,611,686,018,427,3\d\d,\d{3} bytes$/);
    expect(formatExactBytes(2 ** 53)).toMatch(/^about /);
  });

  it.each([
    ['1', 1_000_000_000n],
    ['0', 0n],
    [' 2.5 ', 2_500_000_000n],
    ['10.73741824', 10_737_418_240n],
    ['0.000000001', 1n],
    ['1.000000000', 1_000_000_000n],
    // The greatest quota there is, 2^62 bytes, and the bytes either side of
    // it, which a JavaScript number would both make 2^62: the API, not the
    // console, refuses more.
    ['4611686018.427387904', 2n ** 62n],
    ['4611686018.427387903', 2n ** 62n - 1n],
    ['4611686018.427387905', 2n ** 62n + 1n],
    ['4611686019', 4_611_686_019_000_000_000n],
  ])('reads %j GB as %d bytes, exactly', (text, bytes) => {
    expect(parseGigabytes(text)).toBe(bytes);
  });

  it.each([
    [1n, '{"quota_bytes":1}'],
    [2n ** 62n + 1n, '{"quota_bytes":4611686018427387905}'],
    [0n, '{"quota_bytes":0}'],
  ])('writes a quota of %d bytes as the integer it is', (bytes, body) => {
    expect(quotaBody(bytes)).toBe(body);
    // What JSON reads, where it can read it exactly.
    if (bytes <= BigInt(Number.MAX_SAFE_INTEGER)) {
      expect(JSON.parse(body)).toEqual({ quota_bytes: Number(bytes) });
    }
  });

  it.each(['', 'ten', '1,5', '1.', '.5', '-1', '1e9', '0.0000000001', '1 GB', '0x10'])(
    'refuses %j as a number of GB, saying what one is',
    (text) => {
      expect(() => parseGigabytes(text)).toThrow(/not a number of GB: digits, with "\." as the decimal point.*1,000,000,000 bytes/);
    },
  );

  it.each([
    [1_000_000_000n, '1'],
    [0n, '0'],
    [1n, '0.000000001'],
    [2_500_000_000n, '2.5'],
    [10_737_418_240n, '10.73741824'],
    [123_000_000_000n, '123'],
    [2n ** 62n - 1n, '4611686018.427387903'],
  ])('writes %d bytes as %s GB, which reads back as the same bytes', (bytes, text) => {
    expect(gigabytesOf(bytes)).toBe(text);
    expect(parseGigabytes(text)).toBe(bytes);
  });

  it('reads back every quota it writes, for quotas of every size up to 2^62 bytes', () => {
    // A property, over sizes spread across every power of ten a quota may have.
    for (let digits = 0n; digits <= 18n; digits += 1n) {
      for (const lead of [1n, 3n, 4n, 9n]) {
        // 9 * 10^18 is beyond the greatest quota there is: 2^62 stands in for it.
        const bytes = [lead * 10n ** digits + digits, 2n ** 62n].reduce((a, b) => (a < b ? a : b));
        expect(parseGigabytes(gigabytesOf(bytes)), String(bytes)).toBe(bytes);
      }
    }
  });

  it.each([
    [0, 100, '0 %'],
    [29, 100, '29 %'],
    [999, 1000, '99 %'],
    [1000, 1000, '100 %'],
    [1300, 1000, '130 %'],
    [100_000, 1, '10,000,000 %'],
    [1, 0, '-'],
  ])('shows %d used of a quota of %d as %s, rounded down', (used, quota, text) => {
    expect(formatShare(used, quota)).toBe(text);
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
