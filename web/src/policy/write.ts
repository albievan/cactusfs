import { MOST_POLICY_BYTES, utf8Bytes, type PolicyLayout } from '../ui/format';
import type { ConditionBlock, PolicyModel, Principal, Statement, Strings, Text } from './model';

// The writer of the builder (ADR-0032 Decision 5, PLAN 9.7): a model to the
// text that is sent. It writes every field the model holds, in the form the
// model holds it, and nothing else; `readPolicy` of what it writes is the
// model again. The fields are written in the order the policy language names
// them; the operators of a condition and their keys in the order of the
// model.

/** A JSON value to be written: the fields of an object in order. */
type Out = string | null | Out[] | { fields: [string, Out][] };

function strings(value: Strings): Out {
  return value.form === 'one' ? value.value : [...value.values];
}

function principal(value: Principal): Out {
  if (value.form === 'string') {
    return value.value;
  }
  return { fields: value.aws === undefined ? [] : [['AWS', strings(value.aws)]] };
}

function condition(blocks: ConditionBlock[]): Out {
  return {
    fields: blocks.map((block) => [block.operator, { fields: block.tests.map((test) => [test.key, strings(test.values)]) }]),
  };
}

function statement(value: Statement): Out {
  const fields: [string, Out][] = [];
  const text = (name: string, field: Text | undefined): void => {
    if (field !== undefined) {
      fields.push([name, field]);
    }
  };
  text('Sid', value.sid);
  text('Effect', value.effect);
  if (value.principal !== undefined) {
    fields.push(['Principal', principal(value.principal)]);
  }
  if (value.action !== undefined) {
    fields.push(['Action', strings(value.action)]);
  }
  if (value.resource !== undefined) {
    fields.push(['Resource', strings(value.resource)]);
  }
  if (value.condition !== undefined) {
    fields.push(['Condition', condition(value.condition)]);
  }
  return { fields };
}

function documentOf(policy: PolicyModel): Out {
  const fields: [string, Out][] = [];
  if (policy.version !== undefined) {
    fields.push(['Version', policy.version]);
  }
  if (policy.statements !== undefined) {
    fields.push(['Statement', policy.statements.map(statement)]);
  }
  return { fields };
}

/**
 * Writes a value as JSON.stringify does: with `indent` spaces a level, or
 * none at all and no white space when indent is 0. JSON.stringify itself
 * cannot be given the document: an object puts the fields whose names are
 * numbers first, and a field named "__proto__" is no field of it.
 */
function stringify(value: Out, indent: number, depth = 0): string {
  if (value === null || typeof value === 'string') {
    return JSON.stringify(value);
  }
  const items = Array.isArray(value)
    ? value.map((item) => stringify(item, indent, depth + 1))
    : value.fields.map(([name, item]) => `${JSON.stringify(name)}:${indent > 0 ? ' ' : ''}${stringify(item, indent, depth + 1)}`);
  const [open, close] = Array.isArray(value) ? ['[', ']'] : ['{', '}'];
  if (items.length === 0) {
    return `${open}${close}`;
  }
  if (indent === 0) {
    return `${open}${items.join(',')}${close}`;
  }
  const inner = '\n' + ' '.repeat(indent * (depth + 1));
  const outer = '\n' + ' '.repeat(indent * depth);
  return `${open}${inner}${items.join(`,${inner}`)}${outer}${close}`;
}

/**
 * Writes a policy: indented, as `policyLayout` lays a stored policy out,
 * unless indented it would be larger than the API takes - then compact, so
 * that a document is never refused for its spaces.
 */
export function writePolicy(policy: PolicyModel): PolicyLayout {
  const document = documentOf(policy);
  const indented = stringify(document, 2);
  if (utf8Bytes(indented) <= MOST_POLICY_BYTES) {
    return { text: indented, compact: false };
  }
  return { text: stringify(document, 0), compact: true };
}
