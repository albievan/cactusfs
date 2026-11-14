import type { RequiredTag, Tag, TagDefinition } from '../api/client';

// Tags as the console edits them: rows of a key and a value, in the order
// they were typed. Nothing here decides whether a tag may be kept - that is
// the API's (ADR-0018) - it only reads what was typed and lays it out.

/** One row of an editor of tags: a key and a value, as typed. */
export interface TagRow {
  /** Which row this is, for React; never sent. */
  id: string;
  key: string;
  value: string;
}

/** How many tags a bucket may have, as the contract says. It is only said: the API counts. */
export const MOST_BUCKET_TAGS = 50;

/** How many tags a version of an object may have, as the contract says. */
export const MOST_OBJECT_TAGS = 10;

/** How many tags a bucket's rules may require, as the contract says. */
export const MOST_REQUIRED_TAGS = 10;

/** How many values a required tag or a definition may allow, as the contract says. */
export const MOST_TAG_VALUES = 50;

/** How many definitions the catalogue may hold, as the contract says. */
export const MOST_TAG_DEFINITIONS = 200;

/** The longest key, in characters (`TagKey`). */
export const TAG_KEY_LENGTH = 128;

/** The longest value, in characters (`TagValue`). */
export const TAG_VALUE_LENGTH = 256;

/** What a key and a value may be, as the contract has it (`TagKey`, `TagValue`). */
export const TAG_RULE = `A key is 1 to ${String(TAG_KEY_LENGTH)} characters and a value at most ${String(TAG_VALUE_LENGTH)}, of letters, digits, spaces and + - = . _ : / @; a key is given once.`;

/** A new, empty row. */
export function emptyRow(key = ''): TagRow {
  return { id: crypto.randomUUID(), key, value: '' };
}

/** The rows of an editor that begins with these tags, in their order. */
export function rowsOf(tags: readonly Tag[]): TagRow[] {
  return tags.map((tag) => ({ id: crypto.randomUUID(), key: tag.key, value: tag.value }));
}

/**
 * The tags the rows say, in the order they were typed. A row left wholly
 * empty is a line nobody filled in, and is not sent; every other row is
 * sent as typed - a key given twice, or an empty key, included - for the
 * API to judge.
 */
export function tagsOf(rows: readonly TagRow[]): Tag[] {
  return rows.filter((row) => row.key !== '' || row.value !== '').map((row) => ({ key: row.key, value: row.value }));
}

/**
 * The tags as `X-CactusFS-Tagging` (and S3's `x-amz-tagging`) carry them: a
 * URL query string, `stage=draft&team=finance`, each key and value
 * percent-encoded, in the order given. Empty for no tags.
 */
export function taggingOf(tags: readonly Tag[]): string {
  const encode = (text: string) => encodeURIComponent(wellFormed(text));
  return tags.map((tag) => `${encode(tag.key)}=${encode(tag.value)}`).join('&');
}

/**
 * The text with every lone surrogate replaced by U+FFFD, as
 * `String.prototype.toWellFormed` does (ES2024, beyond the console's lib):
 * encodeURIComponent throws on one, and pasted text may hold one. U+FFFD is
 * no character a tag may have, so the API refuses it and says why.
 */
export function wellFormed(text: string): string {
  return text.replace(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g, '\uFFFD');
}

/**
 * Reads values typed with commas between them. A value cannot hold a comma
 * (`TagValue`), so a comma always parts two; the spaces around each are not
 * part of it, and nothing between two commas is no value.
 */
export function splitValues(text: string): string[] {
  return text
    .split(',')
    .map((value) => value.trim())
    .filter((value) => value !== '');
}

/** Shows values the way splitValues reads them. */
export function joinValues(values: readonly string[] | undefined): string {
  return (values ?? []).join(', ');
}

/**
 * Whether a tag the rules require is one for an object of this key: one for
 * the whole bucket, or one for the sub-bucket the key is under. It only
 * chooses which rows an upload begins with; the API judges the upload.
 */
export function requiredFor(rule: RequiredTag, key: string): boolean {
  return rule.sub_bucket === undefined || key.startsWith(`${rule.sub_bucket}/`);
}

/**
 * The rows an upload's tags have once the tags the rules require of it are
 * there: those rows, with the keys it lacks added, empty, at the end. A key
 * typed in another case is not lacking: a bucket that lower-cases its tags
 * keeps `Stage` as `stage`, and a second row would give the key twice.
 */
export function withRequired(rows: readonly TagRow[], required: readonly RequiredTag[], key: string): TagRow[] {
  const keys = new Set(rows.map((row) => row.key.toLowerCase()));
  const added: TagRow[] = [];
  for (const rule of required) {
    const lower = rule.key.toLowerCase();
    if (requiredFor(rule, key) && !keys.has(lower)) {
      keys.add(lower);
      added.push(emptyRow(rule.key));
    }
  }
  return added.length === 0 ? [...rows] : [...rows, ...added];
}

/**
 * What a tag may be offered as, while it is typed: the keys of the catalogue
 * and of the rules, and for a key the values they name. Suggestions only -
 * any key and value may be typed, and the API judges them.
 */
export interface TagSuggestions {
  keys: string[];
  valuesOf: (key: string) => string[];
}

/** The suggestions of a catalogue and of required tags, either of which may be unknown. */
export function suggestionsOf(
  definitions: readonly TagDefinition[] | undefined,
  required: readonly RequiredTag[] | undefined = undefined,
): TagSuggestions {
  const values = new Map<string, string[]>();
  const add = (key: string, more: readonly string[] | undefined) => {
    const known = values.get(key) ?? [];
    for (const value of more ?? []) {
      if (!known.includes(value)) {
        known.push(value);
      }
    }
    values.set(key, known);
  };
  for (const rule of required ?? []) {
    add(rule.key, rule.values);
  }
  for (const definition of definitions ?? []) {
    add(definition.key, definition.values);
  }
  return { keys: [...values.keys()], valuesOf: (key) => values.get(key) ?? [] };
}
