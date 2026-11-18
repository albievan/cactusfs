import type { ConditionBlock, PolicyModel, Principal, Statement, Strings, Text } from './model';
import { valuesOf } from './model';

// The changes the builder makes to a policy's model (ADR-0032, PLAN 9.8):
// each gives a new model and leaves the one it was given as it was. They
// change what is written and nothing else - a field the change is not about
// keeps its form, a value that is not changed stays as written - and they
// decide nothing: whether the document may be stored, and what it allows, is
// the API's to say.

/** A new statement: an Allow with no action and no resource yet, both lists. */
export function newStatement(): Statement {
  return { effect: 'Allow', action: { form: 'list', values: [] }, resource: { form: 'list', values: [] } };
}

/** The policy with one more statement at its end; a policy without a Version is given `version`. */
export function addStatement(policy: PolicyModel, version: string): PolicyModel {
  return {
    ...policy,
    version: policy.version === undefined ? version : policy.version,
    statements: [...(policy.statements ?? []), newStatement()],
  };
}

/** The policy without its i-th statement. */
export function removeStatement(policy: PolicyModel, index: number): PolicyModel {
  return { ...policy, statements: (policy.statements ?? []).filter((_, i) => i !== index) };
}

/** The policy with its i-th statement changed. */
export function changeStatement(
  policy: PolicyModel,
  index: number,
  change: (statement: Statement) => Statement,
): PolicyModel {
  return { ...policy, statements: (policy.statements ?? []).map((statement, i) => (i === index ? change(statement) : statement)) };
}

/**
 * A field of one string or a list with one more value: a field not written
 * becomes a list of it, one string becomes a list of the two. A value that
 * is there already is not added again.
 */
export function withValue(strings: Strings | undefined, value: string): Strings {
  if (strings === undefined) {
    return { form: 'list', values: [value] };
  }
  const values = valuesOf(strings);
  if (values.includes(value)) {
    return strings;
  }
  return { form: 'list', values: [...values, value] };
}

/** A field of one string or a list without its i-th value: what is left is a list. */
export function withoutValue(strings: Strings, index: number): Strings {
  return { form: 'list', values: valuesOf(strings).filter((_, i) => i !== index) };
}

/**
 * A field without the values that are `value` written in any case, as an
 * action is named (the API matches actions without regard to case).
 */
export function withoutAction(strings: Strings, value: string): Strings {
  const lower = value.toLowerCase();
  return { form: 'list', values: valuesOf(strings).filter((item) => item?.toLowerCase() !== lower) };
}

/** Whether a field names `value`, written in any case (an action). */
export function namesAction(strings: Strings | undefined, value: string): boolean {
  const lower = value.toLowerCase();
  return strings !== undefined && valuesOf(strings).some((item) => item?.toLowerCase() === lower);
}

/**
 * The conditions of a statement with one more value: of the test of `key`
 * under `operator` if there is one, of a new test under that operator if
 * there is the operator, of a new operator otherwise.
 */
export function withCondition(
  blocks: ConditionBlock[] | undefined,
  operator: string,
  key: string,
  value: string,
): ConditionBlock[] {
  const all = blocks ?? [];
  const at = all.findIndex((block) => block.operator === operator);
  if (at < 0) {
    return [...all, { operator, tests: [{ key, values: { form: 'one', value } }] }];
  }
  return all.map((block, i) => {
    if (i !== at) {
      return block;
    }
    const test = block.tests.findIndex((item) => item.key === key);
    if (test < 0) {
      return { operator, tests: [...block.tests, { key, values: { form: 'one', value } }] };
    }
    return {
      operator,
      tests: block.tests.map((item, j) => (j === test ? { key, values: withValue(item.values, value) } : item)),
    };
  });
}

/**
 * The conditions of a statement without the test at `test` of the operator
 * at `block`; an operator left without tests goes too, and so does the
 * Condition left without operators (undefined).
 */
export function withoutCondition(blocks: ConditionBlock[], block: number, test: number): ConditionBlock[] | undefined {
  const left = blocks
    .map((item, i) => (i === block ? { ...item, tests: item.tests.filter((_, j) => j !== test) } : item))
    .filter((item, i) => i !== block || item.tests.length > 0);
  return left.length === 0 ? undefined : left;
}

/** A Sid as typed: nothing typed is no Sid. */
export function sidOf(typed: string): Text | undefined {
  return typed === '' ? undefined : typed;
}

/**
 * A resource of the catalogue's template (`arn:aws:s3:::{bucket}/{key}`) with
 * its placeholders filled in order by `values`; undefined when a value is
 * missing or empty, or there are more values than placeholders.
 */
export function fillTemplate(template: string, values: readonly string[]): string | undefined {
  let used = 0;
  let missing = false;
  const filled = template.replace(/\{[^{}]+\}/g, () => {
    const value = values[used];
    used += 1;
    if (value === undefined || value === '') {
      missing = true;
      return '';
    }
    return value;
  });
  return missing || used !== values.length ? undefined : filled;
}

/** The number of placeholders of a template. */
export function placeholdersOf(template: string): number {
  return template.match(/\{[^{}]+\}/g)?.length ?? 0;
}

/**
 * The names of a Principal, in the order written: the string itself, or the
 * values of its AWS. None when it is not written, or has no AWS.
 */
export function principalValues(principal: Principal | undefined): Text[] {
  if (principal === undefined) {
    return [];
  }
  if (principal.form === 'string') {
    return [principal.value];
  }
  return principal.aws === undefined ? [] : valuesOf(principal.aws);
}

/**
 * A Principal with one more name under AWS: one not written becomes
 * `{"AWS": [name]}`, the string `"*"` becomes `{"AWS": ["*", name]}`. A name
 * that is there already is not added again, and what is there keeps its
 * form.
 */
export function withPrincipal(principal: Principal | undefined, name: string): Principal {
  if (principal === undefined) {
    return { form: 'object', aws: { form: 'list', values: [name] } };
  }
  if (principal.form === 'string') {
    return principal.value === name
      ? principal
      : { form: 'object', aws: { form: 'list', values: [principal.value, name] } };
  }
  return { form: 'object', aws: withValue(principal.aws, name) };
}

/**
 * A Principal without its i-th name: a string Principal is then not written
 * at all (undefined); what is left under AWS is a list.
 */
export function withoutPrincipal(principal: Principal, index: number): Principal | undefined {
  if (principal.form === 'string') {
    return undefined;
  }
  return { form: 'object', aws: withoutValue(principal.aws ?? { form: 'list', values: [] }, index) };
}
