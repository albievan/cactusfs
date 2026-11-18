// A policy document as the console's builder holds it (ADR-0032, PLAN 9.7):
// what the text says, field by field, in the form each field is written -
// one string or a list, "*" or {"AWS": "*"} - so that writing the model gives
// the text back (read.ts, write.ts). The model decides nothing: whether a
// document may be stored, and what it allows, is the API's to say.

/**
 * The kinds of policy document there are. An identity policy is a named
 * policy or an API key's inline policy; a bucket's and a sub-bucket's policy
 * also say whom each statement is about (Principal).
 */
export type PolicyKind = 'identity' | 'bucket' | 'sub-bucket';

/**
 * A place for one string. The API's parsers take null there as well (it reads
 * as an empty string), so a null that was written is kept, not made a string.
 */
export type Text = string | null;

/**
 * A field that may be written as one string or as a list of strings (Action,
 * Resource, a Principal's AWS, the values of a condition), in the form it is
 * written.
 */
export type Strings = { form: 'one'; value: Text } | { form: 'list'; values: Text[] };

/**
 * Whom a statement of a bucket's or a sub-bucket's policy is about: a string
 * (`"*"`), or an object whose AWS names users or `"*"` (`{"AWS": "*"}`,
 * `{"AWS": ["*", "arn:aws:iam:::user/alice"]}`). An object without AWS is
 * kept as written: the API refuses it.
 */
export type Principal = { form: 'string'; value: string } | { form: 'object'; aws?: Strings };

/** One test of a condition: a key and the values it is compared with. */
export interface ConditionTest {
  key: string;
  values: Strings;
}

/**
 * The tests of a condition with one operator, in the order written. The
 * operators of a statement are a list as well, in the order written.
 */
export interface ConditionBlock {
  operator: string;
  tests: ConditionTest[];
}

/**
 * One statement. A field that is not written is undefined; one written as
 * null is null; an empty Condition (`{}`) is an empty list. Which fields a
 * statement must have is the API's to say: the model holds what is written.
 */
export interface Statement {
  sid?: Text;
  effect?: Text;
  /** A bucket's or a sub-bucket's policy only. */
  principal?: Principal;
  action?: Strings;
  resource?: Strings;
  condition?: ConditionBlock[];
}

/** A policy document of a kind. */
export interface PolicyModel {
  kind: PolicyKind;
  version?: Text;
  statements?: Statement[];
}

/**
 * What the builder needs of the answering node's catalogue
 * (`GET /policy-catalogue`, ADR-0032 Decision 1) to tell what it knows from
 * what it does not. The catalogue's schema has more; any object with these
 * fields will do.
 */
export interface PolicyCatalogueView {
  /** The one Version, `2012-10-17`. */
  version: string;
  actions: readonly { name: string }[];
  condition_keys: readonly { key: string; operators: readonly string[] }[];
  /** The forms of a bucket policy's Principal: `anyone` as JSON text, and how a user is named. */
  bucket_policy_principals?: { anyone: readonly string[]; user_arn_prefix: string };
  /** The bounds of a document; the reader nests no deeper than `max_depth`. */
  limits?: { max_depth: number };
}

/** What a mark is about. */
export type MarkKind = 'version' | 'action' | 'condition' | 'principal';

/**
 * A part of a document the catalogue passed in does not know. It is kept as
 * written, never dropped; the builder shows it marked and leaves it to the
 * JSON.
 */
export interface PolicyMark {
  kind: MarkKind;
  /** Where it is, as an RFC 6901 pointer into the document as written (a field written as one string has no index). */
  pointer: string;
  /** The index of the statement it is in, from 0; undefined outside a statement. */
  statement?: number;
  reason: string;
}

/** A text the reader took: the model of it, and what the catalogue does not know of it. */
export interface PolicyRead {
  status: 'read';
  policy: PolicyModel;
  marks: PolicyMark[];
}

/**
 * A text the reader did not take: it is kept as it is, and the builder is
 * disabled with the reason.
 */
export interface PolicyKept {
  status: 'kept';
  reason: string;
  /** Where, as an RFC 6901 pointer; "" for the whole document. */
  pointer: string;
  /** For a text that is not JSON: the index of the character where reading stopped. */
  offset?: number;
}

/** What the reader makes of a text. */
export type PolicyReading = PolicyRead | PolicyKept;

/** The values of a field that may be one string or a list, as a list. */
export function valuesOf(strings: Strings): Text[] {
  return strings.form === 'one' ? [strings.value] : strings.values;
}
