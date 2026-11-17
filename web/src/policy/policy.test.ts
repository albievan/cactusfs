import { describe, expect, it } from 'vitest';

import { MOST_POLICY_BYTES, formatPolicyDocument, utf8Bytes } from '../ui/format';
import type { PolicyCatalogueView, PolicyKind, PolicyModel, PolicyReading } from './model';
import { canonicalPolicy, readPolicy } from './read';
import corpusText from './testdata/corpus.json?raw';
import { writePolicy } from './write';

/** A generator of numbers that is the same on every run, so that a failure can be had again. */
function seeded(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * The canonical text of a JSON document, made with JSON.parse and nothing of
 * the reader's: the law is held against this, not against the reader's own
 * canonicalPolicy. Keys sorted, no white space, every value in its form.
 */
function oracle(text: string): string {
  const write = (value: unknown): string => {
    if (Array.isArray(value)) {
      return `[${value.map(write).join(',')}]`;
    }
    if (value !== null && typeof value === 'object') {
      const record = value as Record<string, unknown>;
      return `{${Object.keys(record)
        .sort()
        .map((key) => `${JSON.stringify(key)}:${write(record[key])}`)
        .join(',')}}`;
    }
    return JSON.stringify(value);
  };
  return write(JSON.parse(text));
}

function read(text: string, kind: PolicyKind = 'identity', catalogue?: PolicyCatalogueView): PolicyReading {
  return readPolicy(text, kind, catalogue);
}

function model(text: string, kind: PolicyKind = 'identity'): PolicyModel {
  const reading = read(text, kind);
  if (reading.status !== 'read') {
    throw new Error(`kept as it is: ${reading.reason} at ${reading.pointer}`);
  }
  return reading.policy;
}

/**
 * The law (ADR-0032 Decision 5): a text the reader takes, written again from
 * its model, is the same document - and read again, the same model.
 */
function holdsTheLaw(text: string, kind: PolicyKind): void {
  const policy = model(text, kind);
  const written = writePolicy(policy).text;
  expect(oracle(written)).toBe(oracle(text));
  expect(canonicalPolicy(written)).toBe(canonicalPolicy(text));
  expect(canonicalPolicy(text)).toBe(oracle(text));
  expect(model(written, kind)).toEqual(policy);
}

const allow = '"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"';
const identity = (statement: string): string => `{"Version":"2012-10-17","Statement":[{${statement}}]}`;
const bucket = (statement: string): string => identity(`${allow},"Principal":${statement}`);

describe('the reader', () => {
  it('models every field in the form it is written', () => {
    expect(
      model(
        identity(
          `"Sid":null,"Effect":"Deny","Action":["s3:*"],"Resource":"*","Condition":{"StringLike":{"aws:username":["a*",null]},"Bool":{"aws:SecureTransport":"true"}}`,
        ),
      ),
    ).toEqual({
      kind: 'identity',
      version: '2012-10-17',
      statements: [
        {
          sid: null,
          effect: 'Deny',
          action: { form: 'list', values: ['s3:*'] },
          resource: { form: 'one', value: '*' },
          condition: [
            { operator: 'StringLike', tests: [{ key: 'aws:username', values: { form: 'list', values: ['a*', null] } }] },
            { operator: 'Bool', tests: [{ key: 'aws:SecureTransport', values: { form: 'one', value: 'true' } }] },
          ],
        },
      ],
    });
  });

  it('keeps every form of a Principal', () => {
    const principals = [
      ['"*"', { form: 'string', value: '*' }],
      ['{"AWS":"*"}', { form: 'object', aws: { form: 'one', value: '*' } }],
      ['{"AWS":["*"]}', { form: 'object', aws: { form: 'list', values: ['*'] } }],
      ['{"AWS":["arn:aws:iam:::user/alice","*"]}', { form: 'object', aws: { form: 'list', values: ['arn:aws:iam:::user/alice', '*'] } }],
      ['{}', { form: 'object' }],
      ['"someone"', { form: 'string', value: 'someone' }],
    ] as const;
    for (const [written, principal] of principals) {
      for (const kind of ['bucket', 'sub-bucket'] as const) {
        expect(model(bucket(written), kind).statements?.[0]?.principal).toEqual(principal);
        holdsTheLaw(bucket(written), kind);
      }
    }
  });

  it('models what it is not given as not there, and what is empty as empty', () => {
    expect(model('{}')).toEqual({ kind: 'identity' });
    expect(model('{"Statement":[]}')).toEqual({ kind: 'identity', statements: [] });
    expect(model(identity('"Condition":{}'))).toEqual({ kind: 'identity', version: '2012-10-17', statements: [{ condition: [] }] });
    expect(model(identity('"Condition":{"Bool":{}}')).statements?.[0]?.condition).toEqual([{ operator: 'Bool', tests: [] }]);
    expect(model(identity('"Action":[]')).statements?.[0]?.action).toEqual({ form: 'list', values: [] });
    for (const text of ['{}', '{"Statement":[]}', identity('"Condition":{}'), identity('"Condition":{"Bool":{}}'), identity('"Action":[]')]) {
      holdsTheLaw(text, 'identity');
    }
  });

  it('reads what the API refuses but the form allows, as written', () => {
    // Not Allow or Deny, another Version, a resource of no kind: the API's
    // validation says what is wrong; the builder shows it as it is.
    const text = '{"Version":"2008-10-17","Statement":[{"Effect":"Maybe","Action":"s4:Nothing","Resource":"photos"}]}';
    expect(model(text).statements?.[0]?.effect).toBe('Maybe');
    holdsTheLaw(text, 'identity');
  });

  it('reads escapes as the API does', () => {
    const text = '{"Version":"2012-10-17","Statement":[{"Eff\\u0065ct":"Allow","Action":"s3:Get\\u004fbject","Resource":"arn:aws:s3:::a\\/b\\ud83d\\ude00\\t"}]}';
    const statement = model(text).statements?.[0];
    expect(statement?.effect).toBe('Allow');
    expect(statement?.action).toEqual({ form: 'one', value: 's3:GetObject' });
    expect(statement?.resource).toEqual({ form: 'one', value: 'arn:aws:s3:::a/b😀\t' });
    holdsTheLaw(text, 'identity');
  });

  it.each<[string, string, PolicyKind, string, string]>([
    ['a field given twice', identity(`${allow},"Effect":"Deny"`), 'identity', 'the field "Effect" is given twice', '/Statement/0/Effect'],
    ['a field given twice, once escaped', identity(`${allow},"Eff\\u0065ct":"Deny"`), 'identity', 'the field "Effect" is given twice', '/Statement/0/Effect'],
    ['a document field given twice', '{"Statement":[],"Version":"2012-10-17","Statement":[]}', 'identity', 'the field "Statement" is given twice', '/Statement'],
    ['an operator given twice', identity(`${allow},"Condition":{"Bool":{"aws:SecureTransport":"true"},"Bool":{}}`), 'identity', 'the field "Bool" is given twice', '/Statement/0/Condition/Bool'],
    ['a key given twice', identity(`${allow},"Condition":{"Bool":{"a/b":"true","a/b":"false"}}`), 'identity', 'the field "a/b" is given twice', '/Statement/0/Condition/Bool/a~1b'],
    ['AWS given twice', bucket('{"AWS":"*","AWS":"*"}'), 'bucket', 'the field "AWS" is given twice', '/Statement/0/Principal/AWS'],
    ['a field twice in an unknown field', '{"Id":{"a":1,"a":2}}', 'identity', 'the field "a" is given twice', '/Id/a'],
    ['an unknown field of the document', '{"Version":"2012-10-17","Id":"x","Statement":[]}', 'identity', 'unknown field "Id"', '/Id'],
    ['a field of the wrong case', identity('"effect":"Allow"'), 'identity', 'unknown field "effect"', '/Statement/0/effect'],
    ['NotAction', identity('"NotAction":"s3:*"'), 'identity', 'unknown field "NotAction"', '/Statement/0/NotAction'],
    ['a Principal in an identity policy', bucket('"*"'), 'identity', 'unknown field "Principal"', '/Statement/0/Principal'],
    ['a principal of another kind', bucket('{"Service":"x"}'), 'bucket', 'unknown field "Service"', '/Statement/0/Principal/Service'],
    ['a document that is a list', '[]', 'identity', 'the document: want an object, not a list', ''],
    ['a document that is null', 'null', 'bucket', 'the document: want an object, not null', ''],
    ['a Version that is a number', '{"Version":2012}', 'identity', 'Version: want a string, not a number', '/Version'],
    ['a Statement that is an object', '{"Statement":{}}', 'identity', 'Statement: want a list, not an object', '/Statement'],
    ['a Statement that is null', '{"Statement":null}', 'identity', 'Statement: want a list, not null', '/Statement'],
    ['a statement that is a string', '{"Statement":["x"]}', 'identity', 'a statement: want an object, not a string', '/Statement/0'],
    ['a statement that is null', '{"Statement":[null]}', 'identity', 'a statement: want an object, not null', '/Statement/0'],
    ['an Effect that is a boolean', identity('"Effect":true'), 'identity', 'Effect: want a string, not a boolean', '/Statement/0/Effect'],
    ['a Sid that is a list', identity('"Sid":["a"]'), 'identity', 'Sid: want a string, not a list', '/Statement/0/Sid'],
    ['an Action that is an object', identity('"Action":{}'), 'identity', 'Action: want a string or a list of strings, not an object', '/Statement/0/Action'],
    ['an Action with a number', identity('"Action":["s3:*",1]'), 'identity', 'Action: want a string or a list of strings, not a list with a number', '/Statement/0/Action/1'],
    ['a Resource of lists', identity('"Resource":[["*"]]'), 'identity', 'Resource: want a string or a list of strings, not a list with a list', '/Statement/0/Resource/0'],
    ['a Condition that is null', identity('"Condition":null'), 'identity', 'Condition: want an object, not null', '/Statement/0/Condition'],
    ['a Condition that is a list', identity('"Condition":[]'), 'bucket', 'Condition: want an object, not a list', '/Statement/0/Condition'],
    ['an operator that is null', identity('"Condition":{"Bool":null}'), 'identity', 'Bool: want an object, not null', '/Statement/0/Condition/Bool'],
    ['values that are a number', identity('"Condition":{"Bool":{"aws:SecureTransport":true}}'), 'identity', 'aws:SecureTransport: want a string or a list of strings, not a boolean', '/Statement/0/Condition/Bool/aws:SecureTransport'],
    ['a Principal that is null', bucket('null'), 'bucket', 'Principal: want "*" or {"AWS": ...}, not null', '/Statement/0/Principal'],
    ['a Principal that is a list', bucket('["*"]'), 'sub-bucket', 'Principal: want "*" or {"AWS": ...}, not a list', '/Statement/0/Principal'],
    ['an AWS that is an object', bucket('{"AWS":{}}'), 'bucket', 'AWS: want a string or a list of strings, not an object', '/Statement/0/Principal/AWS'],
    ['data after the document', '{} {}', 'identity', 'not JSON: data after the document at character 3', ''],
    // The API's parsers take this one today (PLAN 9.7 review); it is not JSON, and is kept.
    ['a stray brace after the document', `${identity(allow)}}`, 'identity', 'not JSON: data after the document at character 114', ''],
    ['a stray bracket after the document', `${bucket('"*"')}]`, 'bucket', 'not JSON: data after the document at character 130', ''],
    ['an empty text', '', 'identity', 'not JSON: the text ends too soon', ''],
    ['a comma too many', '{"Version":"2012-10-17",}', 'identity', 'not JSON: unexpected "}" at character 24', ''],
    ['a comment', '{} // a comment', 'identity', 'not JSON: data after the document at character 3', ''],
    ['a single quote', "{'Version':1}", 'identity', 'not JSON: unexpected "\'" at character 1', ''],
    ['a line break in a string', '{"Version":"a\nb"}', 'identity', 'not JSON: a control character in a string at character 13', ''],
    ['an invalid escape', '{"Version":"\\x41"}', 'identity', 'not JSON: an invalid escape in a string at character 12', ''],
    ['a short \\u escape', '{"Version":"\\u004"}', 'identity', 'not JSON: an invalid escape in a string at character 12', ''],
    ['a byte-order mark', '﻿{}', 'identity', 'not JSON: unexpected "﻿" at character 0', ''],
    ['a nesting too deep', '{"Id":[[[[[[[[[]]]]]]]]]}', 'identity', 'nested too deeply', '/Id/0/0/0/0/0/0/0/0'],
  ])('keeps as it is %s', (_, text, kind, reason, pointer) => {
    const reading = read(text, kind);
    expect(reading).toMatchObject({ status: 'kept', reason, pointer });
  });

  it('says where a text that is not JSON stops being JSON', () => {
    expect(read('{"Version" "x"}')).toEqual({ status: 'kept', reason: 'not JSON: unexpected "\\"" at character 11', pointer: '', offset: 11 });
  });

  it('reads JSON numbers and literals of every form, to say they are not of a policy', () => {
    for (const value of ['0', '-1.5e+3', '1E2', 'true', 'false']) {
      expect(read(`{"Version":${value}}`)).toMatchObject({ status: 'kept', pointer: '/Version' });
    }
    for (const value of ['01', '1.', '.5', '+1', '-', 'tru', 'nul', 'NaN']) {
      expect(read(`{"Version":${value}}`)).toMatchObject({ status: 'kept', pointer: '' });
    }
  });

  it('reads a bucket policy as a sub-bucket policy, and an identity policy as either', () => {
    holdsTheLaw(bucket('"*"'), 'sub-bucket');
    holdsTheLaw(identity(allow), 'bucket');
    expect(model(identity(allow), 'sub-bucket').kind).toBe('sub-bucket');
  });
});

const catalogue: PolicyCatalogueView = {
  version: '2012-10-17',
  actions: [{ name: 's3:GetObject' }, { name: 's3:ListBucket' }, { name: 'admin:ListUsers' }],
  condition_keys: [
    { key: 'aws:SourceIp', operators: ['IpAddress', 'NotIpAddress'] },
    { key: 'aws:username', operators: ['StringEquals', 'StringLike'] },
  ],
  bucket_policy_principals: { anyone: ['"*"', '{"AWS":"*"}'], user_arn_prefix: 'arn:aws:iam:::user/' },
};

describe('the marks of what the catalogue does not know', () => {
  it('mark an action, a condition and a principal it does not list, where each is', () => {
    const text =
      '{"Version":"2012-10-17","Statement":[' +
      '{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/alice","*","arn:aws:iam::123:root"]},"Action":["S3:getobject","s3:Get*","s3:Teleport","*"],"Resource":"*"},' +
      '{"Effect":"Deny","Principal":"x","Action":"s3:Teleport","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8","aws:Referer":"x"},"Bool":{"aws:username":"x"}}}]}';
    const reading = read(text, 'bucket', catalogue);
    expect(reading.status).toBe('read');
    if (reading.status !== 'read') {
      return;
    }
    expect(reading.marks.map((mark) => [mark.kind, mark.pointer, mark.statement])).toEqual([
      ['action', '/Statement/0/Action/2', 0],
      ['principal', '/Statement/0/Principal/AWS/2', 0],
      ['action', '/Statement/1/Action', 1],
      ['condition', '/Statement/1/Condition/IpAddress/aws:Referer', 1],
      ['condition', '/Statement/1/Condition/Bool/aws:username', 1],
      ['principal', '/Statement/1/Principal', 1],
    ]);
    expect(reading.marks[0]?.reason).toBe('the action "s3:Teleport" is not in the catalogue');
    // Marked, and kept: the document is the same.
    expect(oracle(writePolicy(reading.policy).text)).toBe(oracle(text));
  });

  it('mark a Version other than the catalogue’s, and nothing without a catalogue', () => {
    const text = '{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"s3:Teleport","Resource":"*"}]}';
    const marked = read(text, 'identity', catalogue);
    expect(marked.status === 'read' && marked.marks.map((mark) => mark.pointer)).toEqual(['/Version', '/Statement/0/Action']);
    expect(read(text)).toMatchObject({ status: 'read', marks: [] });
  });

  it('compare the forms of anyone as documents, not as bytes', () => {
    const spaced: PolicyCatalogueView = {
      ...catalogue,
      bucket_policy_principals: { anyone: [' "*" ', '{ "AWS" : "\\u002a" }'], user_arn_prefix: 'arn:aws:iam:::user/' },
    };
    expect(read(bucket('"*"'), 'bucket', spaced)).toMatchObject({ status: 'read', marks: [] });
    expect(read(bucket('{"AWS":["*"]}'), 'bucket', spaced)).toMatchObject({ status: 'read', marks: [] });
  });

  it('leave a pattern with a wildcard to the API', () => {
    const reading = read(identity('"Action":["s3:Tele*","admin:?","*"]'), 'identity', catalogue);
    expect(reading).toMatchObject({ status: 'read', marks: [] });
  });
});

describe('the writer', () => {
  it('lays a document out as policyLayout does', () => {
    const text = identity(`"Sid":"a",${allow},"Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8"]}}`);
    const written = writePolicy(model(text));
    expect(written).toEqual({ text: formatPolicyDocument(JSON.parse(text) as Record<string, unknown>), compact: false });
  });

  it('writes the fields in the order of the language, and the conditions as read', () => {
    const text = '{"Statement":[{"Condition":{"__proto__":{"2":"x","1":"y"}},"Resource":[],"Effect":null}],"Version":"v"}';
    expect(writePolicy(model(text)).text).toBe(
      '{\n  "Version": "v",\n  "Statement": [\n    {\n      "Effect": null,\n      "Resource": [],\n      "Condition": {\n        "__proto__": {\n          "2": "x",\n          "1": "y"\n        }\n      }\n    }\n  ]\n}',
    );
    holdsTheLaw(text, 'identity');
  });

  it('goes compact above 20 KiB, and not before', () => {
    const statement = (i: number): string => `{"Sid":"s${String(i)}",${allow}}`;
    let count = 1;
    const documentOf = (n: number): string => `{"Version":"2012-10-17","Statement":[${Array.from({ length: n }, (_, i) => statement(i)).join(',')}]}`;
    while (utf8Bytes(formatPolicyDocument(JSON.parse(documentOf(count + 1)) as Record<string, unknown>)) <= MOST_POLICY_BYTES) {
      count += 1;
    }
    const under = writePolicy(model(documentOf(count)));
    expect(under.compact).toBe(false);
    expect(utf8Bytes(under.text)).toBeLessThanOrEqual(MOST_POLICY_BYTES);
    const over = writePolicy(model(documentOf(count + 1)));
    expect(over.compact).toBe(true);
    expect(over.text).toBe(JSON.stringify(JSON.parse(documentOf(count + 1))));
    holdsTheLaw(documentOf(count + 1), 'identity');
  });

  it('counts the bytes of a document beyond ASCII as sent', () => {
    // Each "é" is one character and two bytes: indented, this is over the bound by bytes only.
    const sid = 'é'.repeat(MOST_POLICY_BYTES / 2);
    const written = writePolicy({ kind: 'identity', statements: [{ sid }] });
    expect(written.compact).toBe(true);
    expect(written.text).toBe(`{"Statement":[{"Sid":"${sid}"}]}`);
  });
});

interface CorpusEntry {
  kind: PolicyKind;
  origin: string;
  text: string;
}

describe('the corpus the API’s parsers wrote', () => {
  const corpus = JSON.parse(corpusText) as { entries: CorpusEntry[] };

  it('holds documents of every kind, the fuzz seeds among them', () => {
    for (const kind of ['identity', 'bucket', 'sub-bucket'] as const) {
      expect(corpus.entries.filter((entry) => entry.kind === kind).length).toBeGreaterThanOrEqual(100);
    }
    expect(corpus.entries.some((entry) => entry.origin.startsWith('Fuzz'))).toBe(true);
  });

  it('is read whole, and every document written back is the same document', () => {
    for (const entry of corpus.entries) {
      const reading = read(entry.text, entry.kind);
      expect(reading, entry.origin).toMatchObject({ status: 'read' });
      holdsTheLaw(entry.text, entry.kind);
    }
  });
});

/** A JSON value the generator writes: an object is its fields, in order. */
type Node = string | number | boolean | null | Node[] | { fields: [string, Node][] };

const isObject = (node: Node): node is { fields: [string, Node][] } => node !== null && typeof node === 'object' && !Array.isArray(node);

/** What the reader must make of a document: read it, keep it, or either - but never change it. */
type Expected = 'read' | 'kept' | 'either';

const ALPHABET = [...'abcXYZ019 +-=._:/@&%?#*"\\\t', 'é', 'ß', 'Ж', '日', '😀', ' ', '\u0000', '~'];

/** Policy documents of the three kinds, in every form and layout, and mutations of them. */
class Generator {
  private readonly random: () => number;

  constructor(seed: number) {
    this.random = seeded(seed);
  }

  n(n: number): number {
    return Math.floor(this.random() * n);
  }

  pick<T>(...values: T[]): T {
    return values[this.n(values.length)] as T;
  }

  text(): string {
    return Array.from({ length: this.n(6) }, () => this.pick(...ALPHABET)).join('');
  }

  slot(pool: string[]): string | null {
    const roll = this.n(12);
    return roll === 0 ? null : roll === 1 ? this.text() : this.pick(...pool);
  }

  oneOrMany(pool: string[]): Node {
    if (this.n(2) === 0) {
      return this.slot(pool);
    }
    return Array.from({ length: this.n(4) }, () => this.slot(pool));
  }

  principal(): Node {
    switch (this.n(5)) {
      case 0:
        return '*';
      case 1:
        return { fields: [['AWS', '*']] };
      case 2:
        return { fields: [['AWS', this.oneOrMany(['*', 'arn:aws:iam:::user/alice', 'arn:aws:iam:::user/bob'])]] };
      case 3:
        return this.text();
      default:
        return { fields: [] };
    }
  }

  condition(): Node {
    const operators = ['IpAddress', 'NotIpAddress', 'Bool', 'DateGreaterThan', 'StringEquals', 'StringLike', 'NotAnOperator', '__proto__', '1'];
    const keys = ['aws:SourceIp', 'aws:SecureTransport', 'aws:CurrentTime', 'aws:username', 'aws:Referer', 'constructor', '0'];
    const blocks: [string, Node][] = [];
    for (const operator of new Set(Array.from({ length: this.n(3) }, () => this.pick(...operators)))) {
      const tests: [string, Node][] = [...new Set(Array.from({ length: this.n(3) }, () => this.pick(...keys)))].map((key) => [
        key,
        this.oneOrMany(['10.0.0.0/8', 'true', '2026-01-01T00:00:00Z', 'alice', 'a*']),
      ]);
      blocks.push([operator, { fields: tests }]);
    }
    return { fields: blocks };
  }

  statement(kind: PolicyKind): Node {
    const fields: [string, Node][] = [];
    const maybe = (name: string, value: () => Node): void => {
      if (this.n(6) !== 0) {
        fields.push([name, value()]);
      }
    };
    if (this.n(2) === 0) {
      fields.push(['Sid', this.slot(['', 's1', 'Read "public" files'])]);
    }
    maybe('Effect', () => this.slot(['Allow', 'Deny']));
    if (kind !== 'identity') {
      maybe('Principal', () => this.principal());
    }
    maybe('Action', () => this.oneOrMany(['*', 's3:*', 's3:GetObject', 's3:Get*', 'admin:ListUsers', 's3:ListBucket']));
    maybe('Resource', () => this.oneOrMany(['*', 'arn:aws:s3:::site', 'arn:aws:s3:::site/*', 'arn:cactusfs:admin:::user/*']));
    if (this.n(2) === 0) {
      fields.push(['Condition', this.condition()]);
    }
    return { fields: this.shuffle(fields) };
  }

  document(kind: PolicyKind): Node {
    const fields: [string, Node][] = [];
    if (this.n(8) !== 0) {
      fields.push(['Version', this.slot(['2012-10-17'])]);
    }
    if (this.n(12) !== 0) {
      fields.push(['Statement', Array.from({ length: this.n(4) }, () => this.statement(kind))]);
    }
    return { fields: this.shuffle(fields) };
  }

  shuffle<T>(values: T[]): T[] {
    if (this.n(3) === 0) {
      for (let i = values.length - 1; i > 0; i -= 1) {
        const j = this.n(i + 1);
        [values[i], values[j]] = [values[j] as T, values[i] as T];
      }
    }
    return values;
  }

  /** Every object of a document, and its place. */
  objects(node: Node, out: { fields: [string, Node][] }[] = []): { fields: [string, Node][] }[] {
    if (Array.isArray(node)) {
      node.forEach((item) => this.objects(item, out));
    } else if (isObject(node)) {
      out.push(node);
      node.fields.forEach(([, value]) => this.objects(value, out));
    }
    return out;
  }

  /** Changes a document so that the reader must keep it as it is, or so that it must still read it. */
  mutate(document: Node, kind: PolicyKind): [Node, PolicyKind, Expected] {
    const objects = this.objects(document);
    const target = this.pick(...objects);
    const strings = (o: { fields: [string, Node][] }): number[] =>
      o.fields.flatMap(([name, value], i) => (['Sid', 'Effect', 'Version'].includes(name) && (typeof value === 'string' || value === null) ? [i] : []));
    switch (this.n(10)) {
      case 0: {
        // A field given twice, maybe with an escape: kept.
        const field = target.fields[this.n(target.fields.length)];
        if (field === undefined) {
          return [document, kind, 'read'];
        }
        target.fields.splice(this.n(target.fields.length + 1), 0, [field[0], this.pick<Node>(field[1], null, 'x')]);
        return [document, kind, 'kept'];
      }
      case 1: {
        // A field no document of the kind has, in the document, a statement or a Principal: kept.
        const statements = this.objects(document).filter((o) => o.fields.some(([name]) => name === 'Effect' || name === 'Action'));
        const where = this.pick(document as { fields: [string, Node][] }, ...statements);
        where.fields.splice(this.n(where.fields.length + 1), 0, [this.pick('Id', 'NotAction', 'NotResource', 'NotPrincipal', 'effect', 'Statements'), 'x']);
        return [document, kind, 'kept'];
      }
      case 2: {
        // A value of a type no field of a policy has where a string is: kept.
        const at = strings(target);
        if (at.length === 0) {
          return [document, kind, 'read'];
        }
        const field = target.fields[this.pick(...at)] as [string, Node];
        field[1] = this.pick<Node>(1, true, false, [], { fields: [] }, -0.5);
        return [document, kind, 'kept'];
      }
      case 3:
        // A statement list that is not one, or a statement that is not an object: kept.
        return [{ fields: [['Statement', this.pick<Node>(null, 'x', { fields: [] }, [null], ['x'], [[]])]] }, kind, 'kept'];
      case 4:
        // A Principal in an identity policy: kept. An identity policy read as another kind: read.
        if (kind === 'identity') {
          return [document, this.pick<PolicyKind>('bucket', 'sub-bucket'), 'read'];
        }
        return [document, 'identity', objects.some((o) => o.fields.some(([name]) => name === 'Principal')) ? 'kept' : 'read'];
      case 5: {
        // A field taken away: read.
        if (target.fields.length > 0) {
          target.fields.splice(this.n(target.fields.length), 1);
        }
        return [document, kind, 'read'];
      }
      default:
        return [document, kind, 'read'];
    }
  }

  /** Writes a node with one of four layouts, and escapes that need not be there. */
  write(node: Node, style: number, depth = 0): string {
    const space = (inner: number): string => {
      switch (style) {
        case 1:
          return '\n' + '  '.repeat(inner);
        case 2:
          return '\r\n' + '\t'.repeat(inner);
        case 3:
          return this.pick('', ' ', '\t', '\n', '\r\n ');
        default:
          return '';
      }
    };
    if (typeof node === 'string') {
      return this.string(node, style === 3);
    }
    if (typeof node === 'number' || typeof node === 'boolean' || node === null) {
      return JSON.stringify(node);
    }
    const items = Array.isArray(node)
      ? node.map((item) => this.write(item, style, depth + 1))
      : node.fields.map(([name, value]) => `${this.string(name, style === 3)}${style === 3 ? space(0) : ''}:${style === 0 ? '' : ' '}${this.write(value, style, depth + 1)}`);
    const [open, close] = Array.isArray(node) ? ['[', ']'] : ['{', '}'];
    if (items.length === 0) {
      return `${open}${style === 3 ? space(0) : ''}${close}`;
    }
    return `${open}${items.map((item) => `${space(depth + 1)}${item}`).join(',')}${space(depth)}${close}`;
  }

  string(text: string, escape: boolean): string {
    let out = '"';
    for (const unit of Array.from(text, (c) => c).flatMap((c) => (c.length > 1 && escape && this.n(2) === 0 ? [...c.split('')] : [c]))) {
      const code = unit.charCodeAt(0);
      if (unit === '"' || unit === '\\') {
        out += `\\${unit}`;
      } else if (code < 0x20 || (escape && this.n(5) === 0)) {
        out += unit.length > 1 ? unit : `\\u${code.toString(16).padStart(4, '0')}`;
      } else if (unit === '/' && escape) {
        out += '\\/';
      } else {
        out += unit;
      }
    }
    return `${out}"`;
  }

  /** Changes a text a character at a time: what is still read must still be the same document. */
  garble(text: string): string {
    const at = this.n(text.length + 1);
    switch (this.n(4)) {
      case 0:
        return text.slice(0, at) + text.slice(at + 1);
      case 1:
        return text.slice(0, at) + this.pick(...'{}[],:" \\nul0-e'.split('')) + text.slice(at);
      case 2:
        return text.slice(0, at);
      default:
        return `${text}${this.pick('x', ' {}', ',', ' ', '\n')}`;
    }
  }
}

describe('documents of every kind and their mutations', () => {
  it('are each read and written back the same, or kept as they are: never changed', () => {
    const g = new Generator(97);
    const seen = { read: 0, kept: 0, garbled: 0 };
    for (let i = 0; i < 10_000; i += 1) {
      const kind = (['identity', 'bucket', 'sub-bucket'] as const)[i % 3] as PolicyKind;
      const [document, readAs, expected] = g.n(4) === 0 ? [g.document(kind), kind, 'read' as Expected] : g.mutate(g.document(kind), kind);
      let text = g.write(document, g.n(4));
      let want = expected;
      if (g.n(8) === 0) {
        text = g.garble(text);
        want = 'either';
        seen.garbled += 1;
      }
      const reading = read(text, readAs);
      const context = `document ${String(i)} as ${readAs}: ${text}`;
      if (want !== 'either') {
        expect(reading.status, context).toBe(want);
      }
      let parsed = true;
      try {
        JSON.parse(text);
      } catch {
        parsed = false;
      }
      if (reading.status === 'kept') {
        seen.kept += 1;
        expect(reading.reason, context).not.toBe('');
        continue;
      }
      seen.read += 1;
      expect(parsed, context).toBe(true);
      const written = writePolicy(reading.policy).text;
      expect(oracle(written), context).toBe(oracle(text));
      expect(readPolicy(written, readAs), context).toEqual({ status: 'read', policy: reading.policy, marks: [] });
    }
    // Both ways were taken, many times.
    expect(seen.read).toBeGreaterThan(2_000);
    expect(seen.kept).toBeGreaterThan(2_000);
    expect(seen.garbled).toBeGreaterThan(500);
  });

  it('refuse what JSON.parse refuses', () => {
    const g = new Generator(1117);
    for (let i = 0; i < 2_000; i += 1) {
      const text = g.garble(g.garble(g.write(g.document('bucket'), g.n(4))));
      let parsed = true;
      try {
        JSON.parse(text);
      } catch {
        parsed = false;
      }
      if (!parsed) {
        expect(read(text, 'bucket').status, text).toBe('kept');
        expect(canonicalPolicy(text), text).toBeUndefined();
      }
    }
  });
});
