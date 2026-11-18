import type {
  ConditionBlock,
  ConditionTest,
  PolicyCatalogueView,
  PolicyKept,
  PolicyKind,
  PolicyMark,
  PolicyModel,
  PolicyReading,
  Principal,
  Statement,
  Strings,
  Text,
} from './model';
import { valuesOf } from './model';

// The reader of the builder (ADR-0032 Decision 5, PLAN 9.7): a policy as
// text, to the model of it. It is strict as the API's parsers are
// (internal/domain/iam: policy.go, bucketpolicy.go, faults.go) about the form
// of a document: a field given twice, a field the kind of document does not
// have, or a value of a type the parsers do not read there, and the text is
// "kept as it is" - the builder is disabled with the reason, and the JSON
// stays the truth. What the form allows but the API refuses (an Effect that is
// neither Allow nor Deny, a resource of another bucket, a statement too many)
// is read and modelled as written: the API's validation says what is wrong
// with it. Nothing here decides who may do what.
//
// JSON.parse cannot be the reader: of a field given twice it keeps the last,
// so a text the API refuses would be read as one it takes. The text is read
// with a tokenizer of its own, as the API's parsers read it token by token.

/** A JSON value as it is written: the fields of an object in order. */
type Json =
  | { type: 'string'; value: string }
  | { type: 'number'; raw: string }
  | { type: 'boolean'; value: boolean }
  | { type: 'null' }
  | { type: 'array'; items: Json[] }
  | { type: 'object'; names: string[]; values: Json[] };

/**
 * How deeply a document may nest, as the API's parsers count it
 * (`maxPolicyDepth` in internal/domain/iam/policy.go): a value nested deeper
 * is refused there, and is not read here. A catalogue that gives its own
 * bound (`limits.max_depth`) is read by that instead: it is the bound of the
 * node that answered it.
 */
const MOST_DEPTH = 8;

/**
 * The deepest a catalogue may make the reader go, whatever it says: the
 * reader descends by recursion, and a node's answer is not to choose how
 * deep the console's stack goes.
 */
export const DEEPEST = 64;

/** Why a text is kept as it is. */
class Unreadable extends Error {
  readonly pointer: string;
  readonly offset: number | undefined;

  constructor(reason: string, pointer: string, offset?: number) {
    super(reason);
    this.pointer = pointer;
    this.offset = offset;
  }
}

/** The pointer (RFC 6901) of a field or an item of the value at pointer. */
function pointerOf(pointer: string, token: string | number): string {
  const name = typeof token === 'number' ? String(token) : token.replaceAll('~', '~0').replaceAll('/', '~1');
  return `${pointer}/${name}`;
}

const NUMBER = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;

/** The JSON escapes of one character after a backslash. */
const ESCAPES: Readonly<Record<string, string>> = {
  '"': '"',
  '\\': '\\',
  '/': '/',
  b: '\b',
  f: '\f',
  n: '\n',
  r: '\r',
  t: '\t',
};

/**
 * Reads JSON (RFC 8259) as encoding/json reads it, keeping the order of the
 * fields of an object and refusing a field given twice in one object: two
 * names are one when they are the same once their escapes are read, as the
 * API compares them.
 */
class Scanner {
  private at = 0;
  private readonly text: string;
  private readonly mostDepth: number;

  constructor(text: string, mostDepth: number = MOST_DEPTH) {
    this.text = text;
    this.mostDepth = mostDepth;
  }

  /** Reads the text as one JSON value, with nothing after it but white space. */
  document(): Json {
    const value = this.value('', 0);
    this.space();
    if (this.at < this.text.length) {
      throw this.syntax('data after the document');
    }
    return value;
  }

  private syntax(what: string): Unreadable {
    return new Unreadable(`not JSON: ${what} at character ${String(this.at)}`, '', this.at);
  }

  private unexpected(): Unreadable {
    if (this.at >= this.text.length) {
      return new Unreadable('not JSON: the text ends too soon', '', this.at);
    }
    return this.syntax(`unexpected ${JSON.stringify(this.text.charAt(this.at))}`);
  }

  private space(): void {
    for (;;) {
      const c = this.text.charCodeAt(this.at);
      if (c !== 0x20 && c !== 0x09 && c !== 0x0a && c !== 0x0d) {
        return;
      }
      this.at += 1;
    }
  }

  private value(pointer: string, depth: number): Json {
    this.space();
    if (depth > this.mostDepth) {
      throw new Unreadable('nested too deeply', pointer);
    }
    const c = this.text.charAt(this.at);
    switch (c) {
      case '{':
        return this.object(pointer, depth);
      case '[':
        return this.array(pointer, depth);
      case '"':
        return { type: 'string', value: this.string() };
      case 't':
        return this.literal('true', { type: 'boolean', value: true });
      case 'f':
        return this.literal('false', { type: 'boolean', value: false });
      case 'n':
        return this.literal('null', { type: 'null' });
      default: {
        NUMBER.lastIndex = this.at;
        const match = NUMBER.exec(this.text);
        if (match === null) {
          throw this.unexpected();
        }
        this.at += match[0].length;
        return { type: 'number', raw: match[0] };
      }
    }
  }

  private literal(word: string, value: Json): Json {
    if (!this.text.startsWith(word, this.at)) {
      throw this.unexpected();
    }
    this.at += word.length;
    return value;
  }

  private expect(c: string): void {
    this.space();
    if (this.text.charAt(this.at) !== c) {
      throw this.unexpected();
    }
    this.at += 1;
  }

  private object(pointer: string, depth: number): Json {
    this.at += 1;
    const node: Json = { type: 'object', names: [], values: [] };
    const seen = new Set<string>();
    this.space();
    if (this.text.charAt(this.at) === '}') {
      this.at += 1;
      return node;
    }
    for (;;) {
      this.space();
      if (this.text.charAt(this.at) !== '"') {
        throw this.unexpected();
      }
      const name = this.string();
      const at = pointerOf(pointer, name);
      if (seen.has(name)) {
        throw new Unreadable(`the field ${JSON.stringify(name)} is given twice`, at);
      }
      seen.add(name);
      this.expect(':');
      node.names.push(name);
      node.values.push(this.value(at, depth + 1));
      this.space();
      const next = this.text.charAt(this.at);
      this.at += 1;
      if (next === '}') {
        return node;
      }
      if (next !== ',') {
        this.at -= 1;
        throw this.unexpected();
      }
    }
  }

  private array(pointer: string, depth: number): Json {
    this.at += 1;
    const node: Json = { type: 'array', items: [] };
    this.space();
    if (this.text.charAt(this.at) === ']') {
      this.at += 1;
      return node;
    }
    for (;;) {
      node.items.push(this.value(pointerOf(pointer, node.items.length), depth + 1));
      this.space();
      const next = this.text.charAt(this.at);
      this.at += 1;
      if (next === ']') {
        return node;
      }
      if (next !== ',') {
        this.at -= 1;
        throw this.unexpected();
      }
    }
  }

  /** Reads a string whose opening quote is at the current character. */
  private string(): string {
    this.at += 1;
    let out = '';
    let from = this.at;
    for (;;) {
      if (this.at >= this.text.length) {
        throw this.unexpected();
      }
      const c = this.text.charCodeAt(this.at);
      if (c === 0x22) {
        out += this.text.slice(from, this.at);
        this.at += 1;
        return out;
      }
      if (c < 0x20) {
        throw this.syntax('a control character in a string');
      }
      if (c !== 0x5c) {
        this.at += 1;
        continue;
      }
      out += this.text.slice(from, this.at);
      const escape = this.text.charAt(this.at + 1);
      const plain = ESCAPES[escape];
      if (plain !== undefined) {
        out += plain;
        this.at += 2;
      } else if (escape === 'u' && /^[0-9a-fA-F]{4}$/.test(this.text.slice(this.at + 2, this.at + 6))) {
        out += String.fromCharCode(Number.parseInt(this.text.slice(this.at + 2, this.at + 6), 16));
        this.at += 6;
      } else {
        throw this.syntax('an invalid escape in a string');
      }
      from = this.at;
    }
  }
}

/** Names a kind of JSON value for a reason, as the API's parsers do. */
function describe(node: Json): string {
  switch (node.type) {
    case 'string':
      return 'a string';
    case 'number':
      return 'a number';
    case 'boolean':
      return 'a boolean';
    case 'object':
      return 'an object';
    case 'array':
      return 'a list';
    default:
      return 'null';
  }
}

/** A value read where one string is: a string, or null. */
function textOf(node: Json, name: string, pointer: string): Text {
  if (node.type === 'string') {
    return node.value;
  }
  if (node.type === 'null') {
    return null;
  }
  throw new Unreadable(`${name}: want a string, not ${describe(node)}`, pointer);
}

/** A value read where one string or a list of them is. */
function stringsOf(node: Json, name: string, pointer: string): Strings {
  if (node.type === 'array') {
    const values = node.items.map((item, i) => {
      if (item.type !== 'string' && item.type !== 'null') {
        throw new Unreadable(`${name}: want a string or a list of strings, not a list with ${describe(item)}`, pointerOf(pointer, i));
      }
      return item.type === 'string' ? item.value : null;
    });
    return { form: 'list', values };
  }
  if (node.type === 'string' || node.type === 'null') {
    return { form: 'one', value: textOf(node, name, pointer) };
  }
  throw new Unreadable(`${name}: want a string or a list of strings, not ${describe(node)}`, pointer);
}

/** The fields of an object, each with its value and pointer. */
function fieldsOf(node: Json & { type: 'object' }, pointer: string): [string, Json, string][] {
  return node.names.map((name, i) => [name, node.values[i] as Json, pointerOf(pointer, name)]);
}

function objectOf(node: Json, name: string, pointer: string): Json & { type: 'object' } {
  if (node.type !== 'object') {
    throw new Unreadable(`${name}: want an object, not ${describe(node)}`, pointer);
  }
  return node;
}

function unknownField(name: string, pointer: string): Unreadable {
  return new Unreadable(`unknown field ${JSON.stringify(name)}`, pointer);
}

function conditionOf(node: Json, pointer: string): ConditionBlock[] {
  return fieldsOf(objectOf(node, 'Condition', pointer), pointer).map(([operator, tests, at]) => ({
    operator,
    tests: fieldsOf(objectOf(tests, operator, at), at).map(
      ([key, values, where]): ConditionTest => ({ key, values: stringsOf(values, key, where) }),
    ),
  }));
}

function principalOf(node: Json, pointer: string): Principal {
  if (node.type === 'string') {
    return { form: 'string', value: node.value };
  }
  if (node.type !== 'object') {
    throw new Unreadable(`Principal: want "*" or {"AWS": ...}, not ${describe(node)}`, pointer);
  }
  const principal: Principal = { form: 'object' };
  for (const [name, value, at] of fieldsOf(node, pointer)) {
    if (name !== 'AWS') {
      throw unknownField(name, at);
    }
    principal.aws = stringsOf(value, name, at);
  }
  return principal;
}

function statementOf(node: Json, kind: PolicyKind, pointer: string): Statement {
  const statement: Statement = {};
  for (const [name, value, at] of fieldsOf(objectOf(node, 'a statement', pointer), pointer)) {
    switch (name) {
      case 'Sid':
        statement.sid = textOf(value, name, at);
        break;
      case 'Effect':
        statement.effect = textOf(value, name, at);
        break;
      case 'Action':
        statement.action = stringsOf(value, name, at);
        break;
      case 'Resource':
        statement.resource = stringsOf(value, name, at);
        break;
      case 'Condition':
        statement.condition = conditionOf(value, at);
        break;
      case 'Principal':
        if (kind === 'identity') {
          throw unknownField(name, at);
        }
        statement.principal = principalOf(value, at);
        break;
      default:
        throw unknownField(name, at);
    }
  }
  return statement;
}

function policyOf(root: Json, kind: PolicyKind): PolicyModel {
  const policy: PolicyModel = { kind };
  for (const [name, value, at] of fieldsOf(objectOf(root, 'the document', ''), '')) {
    switch (name) {
      case 'Version':
        policy.version = textOf(value, name, at);
        break;
      case 'Statement':
        if (value.type !== 'array') {
          throw new Unreadable(`Statement: want a list, not ${describe(value)}`, at);
        }
        policy.statements = value.items.map((item, i) => statementOf(item, kind, pointerOf(at, i)));
        break;
      default:
        throw unknownField(name, at);
    }
  }
  return policy;
}

/** The pointer of the i-th value of a field that may be one string or a list. */
function itemPointer(pointer: string, strings: Strings, i: number): string {
  return strings.form === 'one' ? pointer : pointerOf(pointer, i);
}

/** Whether an action pattern has a wildcard: what it covers is the API's to say. */
function isPattern(action: string): boolean {
  return action.includes('*') || action.includes('?');
}

/**
 * Marks what the catalogue does not know: a Version other than its own; an
 * action named without a wildcard that it does not list (actions are named
 * without regard to case, as the API matches them); a condition key it does
 * not list, or an operator it does not list for the key; and a principal of a
 * form it does not give. A pattern with a wildcard is not marked: which
 * actions it covers, if any, the API's validation says.
 */
function marksOf(policy: PolicyModel, catalogue: PolicyCatalogueView): PolicyMark[] {
  const marks: PolicyMark[] = [];
  if (typeof policy.version === 'string' && policy.version !== catalogue.version) {
    marks.push({ kind: 'version', pointer: '/Version', reason: `the Version ${JSON.stringify(policy.version)} is not ${JSON.stringify(catalogue.version)}` });
  }
  const actions = new Set(catalogue.actions.map((action) => action.name.toLowerCase()));
  const keys = new Map(catalogue.condition_keys.map((key) => [key.key, key.operators]));
  const principals = catalogue.bucket_policy_principals;
  // The forms of anyone are JSON text: compared as documents, not as bytes.
  const anyone = new Set((principals?.anyone ?? []).map((form) => canonicalPolicy(form)));
  (policy.statements ?? []).forEach((statement, i) => {
    const at = pointerOf('/Statement', i);
    const mark = (kind: PolicyMark['kind'], pointer: string, reason: string): void => {
      marks.push({ kind, pointer, statement: i, reason });
    };
    if (statement.action !== undefined) {
      const action = statement.action;
      valuesOf(action).forEach((name, j) => {
        if (name !== null && !isPattern(name) && !actions.has(name.toLowerCase())) {
          mark('action', itemPointer(pointerOf(at, 'Action'), action, j), `the action ${JSON.stringify(name)} is not in the catalogue`);
        }
      });
    }
    for (const block of statement.condition ?? []) {
      for (const test of block.tests) {
        const where = pointerOf(pointerOf(pointerOf(at, 'Condition'), block.operator), test.key);
        const operators = keys.get(test.key);
        if (operators === undefined) {
          mark('condition', where, `the condition key ${JSON.stringify(test.key)} is not in the catalogue`);
        } else if (!operators.includes(block.operator)) {
          mark('condition', where, `the operator ${JSON.stringify(block.operator)} is not one the catalogue gives for ${test.key}`);
        }
      }
    }
    const principal = statement.principal;
    if (principals === undefined || principal === undefined) {
      return;
    }
    const where = pointerOf(at, 'Principal');
    if (principal.form === 'string') {
      if (!anyone.has(canonicalPolicy(JSON.stringify(principal.value)))) {
        mark('principal', where, `the Principal ${JSON.stringify(principal.value)} is not a form the catalogue gives`);
      }
      return;
    }
    const aws = principal.aws;
    if (aws === undefined) {
      return;
    }
    valuesOf(aws).forEach((name, j) => {
      const isAnyone = anyone.has(canonicalPolicy(JSON.stringify({ AWS: name })));
      if (name !== null && !isAnyone && !name.startsWith(principals.user_arn_prefix)) {
        mark('principal', itemPointer(pointerOf(where, 'AWS'), aws, j), `the principal ${JSON.stringify(name)} is neither anyone nor a user`);
      }
    });
  });
  return marks;
}

function kept(cause: unknown): PolicyKept {
  if (!(cause instanceof Unreadable)) {
    throw cause;
  }
  const reading: PolicyKept = { status: 'kept', reason: cause.message, pointer: cause.pointer };
  if (cause.offset !== undefined) {
    reading.offset = cause.offset;
  }
  return reading;
}

/**
 * Reads a policy document of a kind into its model, or keeps it as it is with
 * the reason. With a catalogue, what it does not know is marked.
 * `writePolicy` of the model gives back the text, laid out anew: the two are
 * one document (`canonicalPolicy` of each is the same).
 */
export function readPolicy(text: string, kind: PolicyKind, catalogue?: PolicyCatalogueView): PolicyReading {
  let policy: PolicyModel;
  try {
    const depth = Math.min(catalogue?.limits?.max_depth ?? MOST_DEPTH, DEEPEST);
    policy = policyOf(new Scanner(text, depth).document(), kind);
  } catch (cause) {
    return kept(cause);
  }
  return { status: 'read', policy, marks: catalogue === undefined ? [] : marksOf(policy, catalogue) };
}

function canonical(node: Json): string {
  switch (node.type) {
    case 'string':
      return JSON.stringify(node.value);
    case 'number':
      return node.raw;
    case 'boolean':
      return String(node.value);
    case 'null':
      return 'null';
    case 'array':
      return `[${node.items.map(canonical).join(',')}]`;
    case 'object': {
      const order = node.names.map((_, i) => i).sort((a, b) => {
        const left = node.names[a] as string;
        const right = node.names[b] as string;
        return left < right ? -1 : left > right ? 1 : 0;
      });
      return `{${order.map((i) => `${JSON.stringify(node.names[i])}:${canonical(node.values[i] as Json)}`).join(',')}}`;
    }
  }
}

/**
 * The canonical text of a JSON document: the fields of every object in
 * sorted order, no white space, every value in the form it is written (a
 * string or a list, `"*"` or `{"AWS": "*"}`); strings written as
 * JSON.stringify writes them, and a number as written. Two texts the reader
 * takes are one document when their canonical texts are the same - so a
 * document the builder has not changed is never sent. Undefined for a text
 * that is not JSON, gives a field twice, or nests more deeply than a policy
 * may.
 */
export function canonicalPolicy(text: string): string | undefined {
  try {
    return canonical(new Scanner(text).document());
  } catch (cause) {
    if (cause instanceof Unreadable) {
      return undefined;
    }
    throw cause;
  }
}
