import { describe, expect, it } from 'vitest';

import {
  addStatement,
  fillTemplate,
  namesAction,
  placeholdersOf,
  removeStatement,
  withCondition,
  withoutAction,
  withoutCondition,
  withoutValue,
  withValue,
} from './edit';
import { lineOf, locatePointer, pointerTokens } from './locate';
import type { PolicyModel } from './model';
import { readPolicy } from './read';
import { writePolicy } from './write';

const TEXT = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:Get{\\"x\\"}"],
      "Resource": "*",
      "Condition": {"IpAddress": {"a/b~c": "192.0.2.0/24"}}
    }
  ]
}`;

/** The text a pointer names. */
function at(text: string, pointer: string): string | undefined {
  const range = locatePointer(text, pointer);
  return range === undefined ? undefined : text.slice(range.start, range.end);
}

describe('where a pointer is in the text', () => {
  it.each([
    ['', TEXT],
    ['/Version', '"2012-10-17"'],
    ['/Statement/0/Effect', '"Allow"'],
    ['/Statement/0/Action', '["s3:GetObject", "s3:Get{\\"x\\"}"]'],
    ['/Statement/0/Action/0', '"s3:GetObject"'],
    ['/Statement/0/Action/1', '"s3:Get{\\"x\\"}"'],
    ['/Statement/0/Resource', '"*"'],
    ['/Statement/0/Condition/IpAddress/a~1b~0c', '"192.0.2.0/24"'],
  ])('finds %j', (pointer, text) => {
    expect(at(TEXT, pointer)).toBe(text);
  });

  it.each([
    ['a field that is not there', '/Statement/0/Sid'],
    ['an item past the end', '/Statement/1'],
    ['an index that is no index', '/Statement/01'],
    ['into a string', '/Version/0'],
    ['a pointer that does not begin with a slash', 'Version'],
  ])('finds nothing for %s', (_, pointer) => {
    expect(locatePointer(TEXT, pointer)).toBeUndefined();
  });

  it('finds a field given twice where it is first given', () => {
    expect(at('{"Effect":"Deny","Effect":"Allow"}', '/Effect')).toBe('"Deny"');
  });

  it('skips a value nested ever so deeply without a stack of its own', () => {
    const deep = `{"a":${'['.repeat(100_000)}${']'.repeat(100_000)},"b":1}`;
    expect(at(deep, '/b')).toBe('1');
  });

  it('reads the tokens of a pointer and the line of a character', () => {
    expect(pointerTokens('/a~1b/~0/0')).toEqual(['a/b', '~', '0']);
    expect(lineOf(TEXT, TEXT.indexOf('"Effect"'))).toBe(5);
    expect(lineOf(TEXT, 0)).toBe(1);
  });
});

describe('the changes the builder makes', () => {
  const read = (text: string): PolicyModel => {
    const reading = readPolicy(text, 'identity');
    if (reading.status !== 'read') {
      throw new Error(reading.reason);
    }
    return reading.policy;
  };

  it('adds a statement, with the catalogue Version to a document without one, and removes it', () => {
    const added = addStatement({ kind: 'identity' }, '2012-10-17');
    expect(JSON.parse(writePolicy(added).text)).toEqual({
      Version: '2012-10-17',
      Statement: [{ Effect: 'Allow', Action: [], Resource: [] }],
    });
    expect(addStatement({ kind: 'identity', version: null }, '2012-10-17').version).toBeNull();
    expect(removeStatement(added, 0).statements).toEqual([]);
  });

  it('keeps the form of a field it does not change, and adds to one as a list', () => {
    const policy = read('{"Statement":[{"Action":"s3:GetObject","Resource":"*"}]}');
    const action = policy.statements?.[0]?.action;
    expect(withValue(action, 's3:GetObject')).toBe(action);
    expect(withValue(action, 's3:PutObject')).toEqual({ form: 'list', values: ['s3:GetObject', 's3:PutObject'] });
    expect(withValue(undefined, 'x')).toEqual({ form: 'list', values: ['x'] });
    expect(withoutValue({ form: 'one', value: 'x' }, 0)).toEqual({ form: 'list', values: [] });
    expect(withoutValue({ form: 'list', values: ['a', null, 'b'] }, 1)).toEqual({ form: 'list', values: ['a', 'b'] });
  });

  it('names and removes an action whatever its case, as the node matches actions', () => {
    const actions = { form: 'list' as const, values: ['S3:getobject', 's3:PutObject', null] };
    expect(namesAction(actions, 's3:GetObject')).toBe(true);
    expect(namesAction(undefined, 's3:GetObject')).toBe(false);
    expect(withoutAction(actions, 's3:GetObject')).toEqual({ form: 'list', values: ['s3:PutObject', null] });
  });

  it('adds a condition to its operator and key, and removes it with what is left empty', () => {
    let blocks = withCondition(undefined, 'IpAddress', 'aws:SourceIp', '192.0.2.0/24');
    blocks = withCondition(blocks, 'IpAddress', 'aws:SourceIp', '198.51.100.0/24');
    blocks = withCondition(blocks, 'Bool', 'aws:SecureTransport', 'true');
    blocks = withCondition(blocks, 'IpAddress', 'aws:Other', 'x');
    expect(blocks).toEqual([
      {
        operator: 'IpAddress',
        tests: [
          { key: 'aws:SourceIp', values: { form: 'list', values: ['192.0.2.0/24', '198.51.100.0/24'] } },
          { key: 'aws:Other', values: { form: 'one', value: 'x' } },
        ],
      },
      { operator: 'Bool', tests: [{ key: 'aws:SecureTransport', values: { form: 'one', value: 'true' } }] },
    ]);
    const left = withoutCondition(blocks, 1, 0);
    expect(left).toHaveLength(1);
    expect(withoutCondition(withoutCondition(left ?? [], 0, 0) ?? [], 0, 0)).toBeUndefined();
  });

  it('fills a template of the catalogue in order, and only completely', () => {
    expect(fillTemplate('arn:aws:s3:::{bucket}/{key}', ['photos', 'reports/*'])).toBe('arn:aws:s3:::photos/reports/*');
    expect(fillTemplate('arn:cactusfs:admin:::user', [])).toBe('arn:cactusfs:admin:::user');
    expect(fillTemplate('arn:aws:s3:::{bucket}', [''])).toBeUndefined();
    expect(fillTemplate('arn:aws:s3:::{bucket}', [])).toBeUndefined();
    expect(fillTemplate('arn:aws:s3:::{bucket}', ['a', 'b'])).toBeUndefined();
    expect(placeholdersOf('arn:aws:s3:::{bucket}/{key}')).toBe(2);
    expect(placeholdersOf('arn:cactusfs:admin:::audit')).toBe(0);
  });
});
