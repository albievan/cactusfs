// Where a part of a policy document is in its text (ADR-0032, PLAN 9.8): the
// API says where a fault is as an RFC 6901 pointer into the document as sent,
// and the builder shows it in the JSON. This finds the characters a pointer
// names; it reads the text only as far as it must, and decides nothing.

/** A part of a text: from the character at `start` to the one before `end`. */
export interface TextRange {
  start: number;
  end: number;
}

/** The tokens of an RFC 6901 pointer: `""` is none, `/a~1b/0` is `a/b` and `0`. */
export function pointerTokens(pointer: string): string[] | undefined {
  if (pointer === '') {
    return [];
  }
  if (!pointer.startsWith('/')) {
    return undefined;
  }
  return pointer
    .slice(1)
    .split('/')
    .map((token) => token.replaceAll('~1', '/').replaceAll('~0', '~'));
}

function space(text: string, at: number): number {
  let i = at;
  while (i < text.length && ' \t\n\r'.includes(text.charAt(i))) {
    i += 1;
  }
  return i;
}

/** The end of the string whose opening quote is at `at`: the character after its closing quote. */
function stringEnd(text: string, at: number): number {
  let i = at + 1;
  while (i < text.length) {
    const c = text.charAt(i);
    if (c === '\\') {
      i += 2;
      continue;
    }
    i += 1;
    if (c === '"') {
      return i;
    }
  }
  return text.length;
}

/**
 * The end of the value that begins at `at`. Objects and lists are skipped by
 * counting brackets outside strings, not by descending into them, so that a
 * text nested ever so deeply costs no stack.
 */
function valueEnd(text: string, at: number): number {
  const first = text.charAt(at);
  if (first === '"') {
    return stringEnd(text, at);
  }
  if (first === '{' || first === '[') {
    let depth = 0;
    let i = at;
    while (i < text.length) {
      const c = text.charAt(i);
      if (c === '"') {
        i = stringEnd(text, i);
        continue;
      }
      if (c === '{' || c === '[') {
        depth += 1;
      } else if (c === '}' || c === ']') {
        depth -= 1;
        if (depth === 0) {
          return i + 1;
        }
      }
      i += 1;
    }
    return text.length;
  }
  let i = at;
  while (i < text.length && !',]} \t\n\r'.includes(text.charAt(i))) {
    i += 1;
  }
  return i;
}

/** The name of an object's field, read as JSON reads it; undefined for one that does not read. */
function nameAt(text: string, at: number, end: number): string | undefined {
  try {
    const name: unknown = JSON.parse(text.slice(at, end));
    return typeof name === 'string' ? name : undefined;
  } catch {
    return undefined;
  }
}

/** The member of the object at `at` named `token`: where its value begins. */
function fieldAt(text: string, at: number, token: string): number | undefined {
  let i = at + 1;
  for (;;) {
    i = space(text, i);
    if (text.charAt(i) !== '"') {
      return undefined;
    }
    const end = stringEnd(text, i);
    const name = nameAt(text, i, end);
    i = space(text, end);
    if (text.charAt(i) !== ':') {
      return undefined;
    }
    i = space(text, i + 1);
    // A field given twice is found where it is first given.
    if (name === token) {
      return i;
    }
    i = space(text, valueEnd(text, i));
    if (text.charAt(i) !== ',') {
      return undefined;
    }
    i += 1;
  }
}

/** The item of the list at `at` with the index `token`: where it begins. */
function itemAt(text: string, at: number, token: string): number | undefined {
  if (!/^(?:0|[1-9]\d{0,5})$/.test(token)) {
    return undefined;
  }
  const index = Number(token);
  let i = space(text, at + 1);
  if (text.charAt(i) === ']') {
    return undefined;
  }
  for (let n = 0; ; n += 1) {
    if (n === index) {
      return i;
    }
    i = space(text, valueEnd(text, i));
    if (text.charAt(i) !== ',') {
      return undefined;
    }
    i = space(text, i + 1);
  }
}

/**
 * Where the value a pointer names is in a JSON text: undefined when the
 * pointer names nothing there, or the text is not JSON as far as it is read.
 */
export function locatePointer(text: string, pointer: string): TextRange | undefined {
  const tokens = pointerTokens(pointer);
  if (tokens === undefined) {
    return undefined;
  }
  let at = space(text, 0);
  for (const token of tokens) {
    const c = text.charAt(at);
    const next = c === '{' ? fieldAt(text, at, token) : c === '[' ? itemAt(text, at, token) : undefined;
    if (next === undefined) {
      return undefined;
    }
    at = next;
  }
  if (at >= text.length) {
    return undefined;
  }
  return { start: at, end: valueEnd(text, at) };
}

/** The line, from 1, a character of a text is on. */
export function lineOf(text: string, at: number): number {
  let line = 1;
  for (let i = 0; i < at && i < text.length; i += 1) {
    if (text.charCodeAt(i) === 0x0a) {
      line += 1;
    }
  }
  return line;
}
