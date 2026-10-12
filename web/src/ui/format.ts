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

/** Shows a policy document as indented JSON. */
export function formatPolicyDocument(document: Record<string, unknown>): string {
  return JSON.stringify(document, null, 2);
}
