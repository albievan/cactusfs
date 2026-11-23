/**
 * Shows a date-time of the API in the reader's own locale and time zone. The
 * zero time (year 1, which the server sends for what was never set, such as
 * the creation of a built-in policy or role) is shown as missing.
 */
export function formatTime(value: string | undefined): string {
  if (value === undefined || value === '') {
    return '-';
  }
  const time = new Date(value);
  if (Number.isNaN(time.getTime())) {
    return value;
  }
  return time.getUTCFullYear() <= 1 ? '-' : time.toLocaleString();
}

const UNITS = ['bytes', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/** Shows a number of bytes in binary units. */
export function formatBytes(size: number): string {
  let value = size;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = unit === 0 || value >= 100 ? 0 : 1;
  return `${value.toFixed(digits)} ${UNITS[unit] ?? ''}`.trim();
}

const GIGABYTE = 1_000_000_000;

/**
 * Shows a number of bytes in decimal gigabytes (10^9 bytes): two decimals
 * below 100 GB, none from there up. What is too little to show so is said to
 * be less than the smallest figure there is, and nothing at all is "0 GB".
 * formatExactBytes is the hint that goes with it.
 */
export function formatGigabytes(size: number): string {
  if (size <= 0) {
    return '0 GB';
  }
  const value = size / GIGABYTE;
  if (value < 0.01) {
    return '< 0.01 GB';
  }
  // 99.999 GB is "100 GB", not "100.00 GB": the digits follow what is shown.
  const digits = Number(value.toFixed(2)) >= 100 ? 0 : 2;
  return `${value.toLocaleString('en', { minimumFractionDigits: digits, maximumFractionDigits: digits })} GB`;
}

/**
 * Whether a number of bytes the API answered with is the number it sent. The
 * generated client reads JSON numbers as JavaScript numbers, which hold every
 * whole number only up to 2^53 - 1 (about 9 PB): beyond that the API's figure
 * arrives as the nearest one they hold.
 */
export function isExactBytes(size: number): boolean {
  return Number.isSafeInteger(size);
}

/**
 * Shows a number of bytes exactly, for the hint of a rounded figure; beyond
 * 2^53 - 1, where the figure read is not certain to be the API's (see
 * isExactBytes), as "about" it.
 */
export function formatExactBytes(size: number): string {
  const text = `${size.toLocaleString('en')} ${size === 1 ? 'byte' : 'bytes'}`;
  return isExactBytes(size) ? text : `about ${text}`;
}

/** A GB as the quota editor reads and writes it: 10^9 bytes, as formatGigabytes shows sizes. */
const BYTES_PER_GB = 1_000_000_000n;

/** A number of GB as the quota editor reads it: digits, and at most nine of them after a point. */
const GIGABYTES = /^(\d+)(?:\.(\d{1,9}))?$/;

/**
 * Reads a number of decimal gigabytes (10^9 bytes) typed into the quota
 * editor and gives the bytes it is, exactly: a GB has nine decimal places of
 * bytes, so more are refused rather than rounded away. The bytes are a
 * bigint, sent as written by quotaBody: a JavaScript number would round a
 * quota beyond 2^53 bytes, and 2^62 + 1, which the API refuses, would reach
 * it as 2^62. Only the syntax is checked here: which quotas there may be is
 * for the API to decide, and 0 is sent for it to refuse. "." is the decimal
 * point; "," is refused, not read as one.
 */
export function parseGigabytes(text: string): bigint {
  const match = GIGABYTES.exec(text.trim());
  if (match === null) {
    throw new Error(
      'The quota is not a number of GB: digits, with "." as the decimal point and at most nine digits after it (1 GB is 1,000,000,000 bytes).',
    );
  }
  const whole = BigInt(match[1] ?? '0');
  const fraction = BigInt((match[2] ?? '').padEnd(9, '0'));
  return whole * BYTES_PER_GB + fraction;
}

/**
 * The JSON body of the request that sets a quota: `{"quota_bytes": <bytes>}`,
 * the bytes written as the integer they are. JSON.stringify cannot write a
 * bigint, and a number would be rounded beyond 2^53.
 */
export function quotaBody(bytes: bigint): string {
  return `{"quota_bytes":${bytes.toString()}}`;
}

/**
 * Shows a number of bytes as decimal gigabytes, exactly, as the quota editor
 * begins with it: what parseGigabytes reads back as the same bytes.
 */
export function gigabytesOf(bytes: bigint): string {
  const size = bytes < 0n ? 0n : bytes;
  const whole = (size / BYTES_PER_GB).toString();
  const fraction = (size % BYTES_PER_GB).toString().padStart(9, '0').replace(/0+$/, '');
  return fraction === '' ? whole : `${whole}.${fraction}`;
}

/**
 * How much of a quota is used, as a whole percentage rounded down - a quota
 * all but used is not shown as used up - and as much above 100 as a bucket
 * holds beyond a quota set below what it held.
 */
export function formatShare(used: number, quota: number): string {
  if (quota <= 0) {
    return '-';
  }
  // Multiplied first: 29 of 100 is 29 %, where 0.29 * 100 is 28.999….
  return `${Math.floor((used * 100) / quota).toLocaleString('en')} %`;
}

/** Splits a list of names typed with commas or spaces between them. */
export function parseNames(text: string): string[] {
  return text.split(/[\s,]+/).filter((name) => name !== '');
}

/** Shows a list of names the way parseNames reads it. */
export function joinNames(names: string[]): string {
  return names.join(', ');
}

/**
 * Reads a policy document typed as JSON. Only the syntax is checked here: what
 * the document may say is for the API to decide.
 */
export function parsePolicyDocument(text: string): Record<string, unknown> {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (cause) {
    const reason = cause instanceof Error ? cause.message : String(cause);
    throw new Error(`The policy is not valid JSON: ${reason}`, { cause });
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('The policy is not valid JSON: it must be a JSON object');
  }
  return value as Record<string, unknown>;
}

/** A policy typed as JSON, read, and the body of the request that sends it. */
export interface TypedPolicy {
  /** The document as JavaScript reads it: what the generated client's types ask for. */
  document: Record<string, unknown>;
  /**
   * The JSON body of the request: `{"<field>": <the text as typed>}`. The
   * document goes as it was typed, not as JavaScript re-encodes it: a key
   * typed twice would otherwise reach the API once, with its last value, and
   * the API - which refuses such a document, and measures it as sent - could
   * not tell.
   */
  body: string;
}

/**
 * Reads a policy typed as JSON (only its syntax: what it may say is for the
 * API to decide) and makes the body that sends it, as typed, under `field`.
 * The other fields of the body, if any, go before it, encoded as JSON
 * encodes them (a field whose value is undefined is left out); they must not
 * name `field` again.
 */
export function typedPolicy(field: string, text: string, others: Record<string, unknown> = {}): TypedPolicy {
  if (Object.hasOwn(others, field)) {
    throw new Error(`typedPolicy: the field ${field} is given twice`);
  }
  const document = parsePolicyDocument(text);
  const rest = JSON.stringify(others);
  // "{}" or '{"name":"x"}': opened again to take the policy as the last field.
  const head = rest === '{}' ? '{' : `${rest.slice(0, -1)},`;
  // The text is one JSON object, as JSON.parse just read it: it stands as the
  // value of the field as it is.
  return { document, body: `${head}${JSON.stringify(field)}:${text}}` };
}

/** Shows a policy document as indented JSON. */
export function formatPolicyDocument(document: Record<string, unknown>): string {
  return JSON.stringify(document, null, 2);
}

/**
 * The most a policy document may be, in bytes as sent: 20 KiB, the bound of
 * the API's policy parsers (`iam.MaxPolicyBytes` in
 * `internal/domain/iam/policy.go`). The contract says "20 KiB" in words only,
 * so the figure is repeated here; the API measures and refuses, the console
 * only chooses how to lay a stored document out.
 */
export const MOST_POLICY_BYTES = 20 * 1024;

/** The number of bytes a text takes in UTF-8, as it is sent. */
export function utf8Bytes(text: string): number {
  return new TextEncoder().encode(text).length;
}

/** A stored policy laid out for its editor, and whether it is laid out compact. */
export interface PolicyLayout {
  text: string;
  /** True when indented it would be larger than MOST_POLICY_BYTES, so it is shown without indentation. */
  compact: boolean;
}

/**
 * Lays a stored policy out for an editor that sends what it holds as typed:
 * indented, unless indented it would be larger than the API takes - then
 * compact, so that saving it unchanged is not refused for its spaces.
 */
export function policyLayout(document: Record<string, unknown>): PolicyLayout {
  const indented = formatPolicyDocument(document);
  if (utf8Bytes(indented) <= MOST_POLICY_BYTES) {
    return { text: indented, compact: false };
  }
  return { text: JSON.stringify(document), compact: true };
}

/** What an editor that sends a policy as typed says of its size. */
export const POLICY_SIZE_HINT = `Sent as typed: spaces and line breaks count towards the ${String(MOST_POLICY_BYTES / 1024)} KiB a policy may have.`;

/** What an editor says of a stored policy it shows compact. */
export const POLICY_COMPACT_HINT = `Shown without indentation: indented, it would be larger than ${String(MOST_POLICY_BYTES / 1024)} KiB.`;
