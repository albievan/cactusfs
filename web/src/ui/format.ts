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
