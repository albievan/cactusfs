/** Shows a date-time of the API in the reader's own locale and time zone. */
export function formatTime(value: string | undefined): string {
  if (value === undefined || value === '') {
    return '-';
  }
  const time = new Date(value);
  return Number.isNaN(time.getTime()) ? value : time.toLocaleString();
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

/** Shows a number of bytes exactly, for the hint of a rounded figure. */
export function formatExactBytes(size: number): string {
  return `${size.toLocaleString('en')} ${size === 1 ? 'byte' : 'bytes'}`;
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
