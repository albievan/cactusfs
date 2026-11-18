import { useMemo, useRef, useState, type KeyboardEvent, type ReactNode, type RefObject } from 'react';

import type {
  CatalogueAction,
  CatalogueConditionKey,
  CatalogueResource,
  PolicyCatalogue,
  PolicyFault,
  StatementAnalysis,
} from '../api/client';
import {
  addStatement,
  changeStatement,
  fillTemplate,
  namesAction,
  placeholdersOf,
  removeStatement,
  sidOf,
  withCondition,
  withoutAction,
  withoutCondition,
  withoutValue,
  withValue,
} from '../policy/edit';
import { lineOf, locatePointer, pointerTokens } from '../policy/locate';
import type { PolicyMark, PolicyModel, PolicyReading, Statement, Strings } from '../policy/model';
import { valuesOf } from '../policy/model';
import { readPolicy } from '../policy/read';
import { writePolicy } from '../policy/write';
import { NO_VALIDATION_NOTICE, type PolicyTools, type Validation } from './policyHooks';
import { reasonOf } from './reason';

// The policy builder (ADR-0032, PLAN 9.8): a policy made by choosing - its
// statements, their effect, actions, resources and conditions - beside the
// JSON it comes to. The JSON is the truth: it is what is sent, as shown. A
// change in the builder writes the JSON anew (policy/write.ts), and a change
// of the JSON is read back (policy/read.ts); a text the reader cannot take
// turns the builder off, with the reason. What the actions are, what a
// pattern covers and what is wrong with the document is the node's to say:
// the catalogue, and the validation of the document as it is typed. Without
// the catalogue - a node of an older release, a caller refused it - the JSON
// editor works alone, as before.

interface PolicyBuilderProps {
  /** Begins the ids of the editor's elements: unique on the page. */
  id: string;
  /** The label of the JSON text box. */
  label: string;
  /** The name of the JSON text box in its form. */
  name: string;
  /** The document as JSON text, as it is typed and as it is sent. */
  text: string;
  /** Takes the document as changed; without it the document is only shown. */
  onChange?: (text: string) => void;
  tools: PolicyTools;
  /** The validation of the document as typed; without it nothing is checked. */
  validation?: Validation;
  /** The faults the API gave with its refusal to store this very text, if it did. */
  refused?: readonly PolicyFault[];
  /** How many lines the JSON text box shows. */
  rows: number;
  /** Whether the document must be given. */
  required?: boolean;
  /** What is said under the JSON text box: its size, how it is laid out. */
  hint?: ReactNode;
}

/** What a field of a statement a fault or a mark is at, by its pointer. */
type Field = 'sid' | 'effect' | 'action' | 'resource' | 'condition' | 'statement';

const FIELDS: Readonly<Record<string, Field>> = {
  Sid: 'sid',
  Effect: 'effect',
  Action: 'action',
  Resource: 'resource',
  Condition: 'condition',
};

/** The field of a statement a pointer is in; 'statement' for anything else of it. */
function fieldOf(pointer: string): Field {
  const tokens = pointerTokens(pointer) ?? [];
  return (tokens[0] === 'Statement' && FIELDS[tokens[2] ?? '']) || 'statement';
}

/** One thing to show at a field: a fault the node found, or what its catalogue does not know. */
interface Note {
  kind: 'fault' | 'mark';
  pointer: string;
  statement?: number;
  reason: string;
}

function notesOf(faults: readonly PolicyFault[], marks: readonly PolicyMark[]): Note[] {
  return [
    ...faults.map((fault): Note => ({ kind: 'fault', pointer: fault.pointer, statement: fault.statement, reason: fault.reason })),
    ...marks.map((mark): Note => ({ kind: 'mark', pointer: mark.pointer, statement: mark.statement, reason: mark.reason })),
  ];
}

/** "1 fault", "3 faults". */
function counted(n: number, one: string, many: string): string {
  return `${String(n)} ${n === 1 ? one : many}`;
}

/** Where a fault is, for a person: the statement and the pointer. */
function placeOf(fault: PolicyFault): string {
  const where = fault.pointer === '' ? 'the whole document' : fault.pointer;
  return fault.statement === undefined ? `At ${where}` : `Statement ${String(fault.statement + 1)}, at ${where}`;
}

/**
 * The faults to show for the text as it is: those the API refused its save
 * with, when it was refused - a put has the last word, should it differ from
 * the validation - or else the validation's.
 */
function faultsNow(validation: Validation | undefined, refused: readonly PolicyFault[] | undefined): readonly PolicyFault[] {
  if (refused !== undefined && refused.length > 0) {
    return refused;
  }
  if (validation?.state.status === 'checked') {
    return validation.state.result.faults;
  }
  return [];
}

/** What the live region says of the validation, so that a screen reader hears what changed. */
function announcement(validation: Validation | undefined, faults: readonly PolicyFault[]): string {
  const state = validation?.state;
  switch (state?.status) {
    case 'checking':
      return 'Checking the policy…';
    case 'failed':
      return `The policy could not be checked: ${reasonOf(state.error, 'the request failed')}.`;
    case 'checked': {
      if (state.result.valid) {
        return 'The node finds no fault in the policy.';
      }
      break;
    }
    default:
      if (faults.length === 0) {
        return '';
      }
  }
  const first = faults[0];
  const more = validation?.state.status === 'checked' && validation.state.result.truncated ? ' or more' : '';
  return `${counted(faults.length, 'fault', 'faults')}${more} in the policy.${first === undefined ? '' : ` The first: ${placeOf(first)}: ${first.reason}`}`;
}

/** The document as the builder reads it: an empty text is a document of nothing yet. */
function readingOf(text: string, catalogue: PolicyCatalogue | undefined): PolicyReading {
  if (text.trim() === '') {
    return { status: 'read', policy: { kind: 'identity' }, marks: [] };
  }
  return readPolicy(text, 'identity', catalogue);
}

/**
 * A policy document edited as JSON and, when the answering node has a
 * catalogue for the caller, with the builder beside it. Built for a named
 * policy and an API key's inline policy: identity policies.
 */
export function PolicyBuilder({
  id,
  label,
  name,
  text,
  onChange,
  tools,
  validation,
  refused,
  rows,
  required,
  hint,
}: PolicyBuilderProps) {
  const json = useRef<HTMLTextAreaElement>(null);
  const catalogue = tools.catalogue.status === 'ready' ? tools.catalogue.catalogue : undefined;
  const reading = useMemo(() => readingOf(text, catalogue), [text, catalogue]);
  const faults = faultsNow(validation, refused);
  const hintId = `${id}-size`;
  const faultsId = `${id}-faults`;
  const describedBy = [hint === undefined ? undefined : hintId, faults.length > 0 ? faultsId : undefined]
    .filter((part) => part !== undefined)
    .join(' ');
  const said = announcement(validation, faults);

  return (
    <div className="policy-editor field-full">
      {tools.catalogue.status === 'unavailable' ? (
        <p className="policy-notice" role="status">
          <i className="bi bi-info-circle" aria-hidden="true" /> {tools.catalogue.notice}
        </p>
      ) : null}
      {catalogue !== undefined && validation?.state.status === 'absent' ? (
        <p className="policy-notice" role="status">
          <i className="bi bi-info-circle" aria-hidden="true" /> {NO_VALIDATION_NOTICE}
        </p>
      ) : null}
      <div className={catalogue === undefined ? 'policy-alone' : 'policy-builder'}>
        {catalogue === undefined ? null : (
          <BuilderPane
            id={id}
            reading={reading}
            catalogue={catalogue}
            buckets={tools.buckets}
            faults={faults}
            analysis={validation?.state.status === 'checked' ? validation.state.result.statements : undefined}
            onChange={
              onChange === undefined
                ? undefined
                : (policy) => {
                    onChange(writePolicy(policy).text);
                  }
            }
          />
        )}
        <div className="policy-json">
          <label className="field">
            <span>{label}</span>
            <textarea
              ref={json}
              className="form-control"
              name={name}
              required={required}
              rows={rows}
              spellCheck={false}
              readOnly={onChange === undefined}
              aria-describedby={describedBy === '' ? undefined : describedBy}
              aria-invalid={faults.length > 0 ? true : undefined}
              value={text}
              onChange={(event) => {
                onChange?.(event.target.value);
              }}
            />
          </label>
          {hint === undefined ? null : (
            <p id={hintId} className="form-hint">
              {hint}
            </p>
          )}
          {faults.length === 0 ? null : (
            <JsonFaults
              id={faultsId}
              text={text}
              faults={faults}
              truncated={validation?.state.status === 'checked' && validation.state.result.truncated}
              json={json}
            />
          )}
        </div>
      </div>
      <p className="policy-check" role="status" aria-live="polite">
        {said}
      </p>
    </div>
  );
}

/** The faults of the document beside its JSON, each with where it is and a way to it. */
function JsonFaults({
  id,
  text,
  faults,
  truncated,
  json,
}: {
  id: string;
  text: string;
  faults: readonly PolicyFault[];
  truncated: boolean;
  json: RefObject<HTMLTextAreaElement | null>;
}) {
  const ranges = useMemo(() => faults.map((fault) => locatePointer(text, fault.pointer)), [text, faults]);
  return (
    <div id={id} className="policy-json-faults">
      <p className="policy-field-title">Faults in the JSON</p>
      <ul className="policy-notes">
        {faults.map((fault, i) => {
          const range = ranges[i];
          return (
            <li key={`${String(i)}${fault.pointer}`} className="policy-fault">
              <span>
                {placeOf(fault)}
                {range === undefined ? '' : `, line ${String(lineOf(text, range.start))}`}: {fault.reason}
              </span>
              {range === undefined ? null : (
                <button
                  type="button"
                  className="btn btn-link btn-sm"
                  aria-label={`Show fault ${String(i + 1)} in the JSON`}
                  title="Select where this fault is in the JSON"
                  onClick={() => {
                    const box = json.current;
                    if (box !== null) {
                      box.focus();
                      box.setSelectionRange(range.start, range.end);
                    }
                  }}
                >
                  Show in the JSON
                </button>
              )}
            </li>
          );
        })}
      </ul>
      {truncated ? <p className="form-hint">The node stopped at 100 faults; there are more.</p> : null}
    </div>
  );
}

interface PaneProps {
  id: string;
  reading: PolicyReading;
  catalogue: PolicyCatalogue;
  buckets: string[] | undefined;
  faults: readonly PolicyFault[];
  analysis: readonly StatementAnalysis[] | undefined;
  /** Takes the changed model; without it the policy is only shown. */
  onChange?: (policy: PolicyModel) => void;
}

/** The builder: the statements of the document, or why the JSON cannot be shown so. */
function BuilderPane({ id, reading, catalogue, buckets, faults, analysis, onChange }: PaneProps) {
  const [added, setAdded] = useState<number | null>(null);
  // Counts removals: the statements are known by their place, so after one
  // is removed every statement is drawn anew, and what was being typed into
  // the pickers of one does not pass to the statement that takes its place.
  const [removals, setRemovals] = useState(0);
  const add = useRef<HTMLButtonElement>(null);
  const keptId = `${id}-kept`;

  if (reading.status === 'kept') {
    const where = reading.pointer === '' ? '' : ` (at ${reading.pointer})`;
    return (
      <fieldset className="policy-pane" disabled aria-describedby={keptId}>
        <legend className="policy-pane-title">Policy builder</legend>
        <p id={keptId} className="policy-kept" role="status">
          <i className="bi bi-exclamation-triangle" aria-hidden="true" /> The builder is off until the JSON is put
          right: {reading.reason}
          {where}.
        </p>
      </fieldset>
    );
  }

  const policy = reading.policy;
  const statements = policy.statements ?? [];
  const notes = notesOf(faults, reading.marks);
  const documentNotes = notes.filter((note) => note.statement === undefined);

  return (
    <fieldset
      className="policy-pane"
      onKeyDown={(event) => {
        // Enter in a field of the builder never sends the form the builder is in.
        if (event.key === 'Enter' && event.target instanceof HTMLInputElement) {
          event.preventDefault();
        }
      }}
    >
      <legend className="policy-pane-title">Policy builder</legend>
      <p className="policy-version">
        Version <code>{policy.version === undefined ? 'not given' : String(policy.version)}</code>
      </p>
      <Notes notes={documentNotes} id={`${id}-document-notes`} />
      {statements.length === 0 ? <p className="policy-empty">There are no statements yet.</p> : null}
      {statements.map((statement, i) => (
        <StatementEditor
          // A statement is known by its place (the JSON gives it no other name) and the removals before.
          key={`${String(removals)}-${String(i)}`}
          id={`${id}-s${String(i)}`}
          index={i}
          statement={statement}
          catalogue={catalogue}
          buckets={buckets}
          notes={notes.filter((note) => note.statement === i)}
          analysis={analysis?.find((item) => item.index === i)}
          focusOnShow={added === i}
          onChange={
            onChange === undefined
              ? undefined
              : (change) => {
                  onChange(changeStatement(policy, i, change));
                }
          }
          onRemove={
            onChange === undefined
              ? undefined
              : () => {
                  onChange(removeStatement(policy, i));
                  setAdded(null);
                  setRemovals((n) => n + 1);
                  add.current?.focus();
                }
          }
        />
      ))}
      {onChange === undefined ? null : (
        <button
          ref={add}
          type="button"
          className="btn btn-outline-secondary"
          title="Add a statement to the end of the policy"
          onClick={() => {
            onChange(addStatement(policy, catalogue.version));
            setAdded(statements.length);
          }}
        >
          <i className="bi bi-plus-lg" aria-hidden="true" /> Add a statement
        </button>
      )}
    </fieldset>
  );
}

/** The faults and marks at one place of the builder. */
function Notes({ notes, id }: { notes: readonly Note[]; id?: string }) {
  if (notes.length === 0) {
    return null;
  }
  return (
    <ul id={id} className="policy-notes">
      {notes.map((note, i) => (
        <li key={`${String(i)}${note.pointer}`} className={note.kind === 'fault' ? 'policy-fault' : 'policy-mark'}>
          <i className={note.kind === 'fault' ? 'bi bi-x-circle' : 'bi bi-info-circle'} aria-hidden="true" />{' '}
          {note.kind === 'fault' ? 'Fault: ' : 'Not in this node’s catalogue: '}
          {note.reason}
        </li>
      ))}
    </ul>
  );
}

/** A value of a field as it is shown: null as the word. */
function shown(value: string | null): string {
  return value === null ? 'null' : value;
}

interface StatementProps {
  id: string;
  index: number;
  statement: Statement;
  catalogue: PolicyCatalogue;
  buckets: string[] | undefined;
  notes: readonly Note[];
  analysis: StatementAnalysis | undefined;
  /** Whether the statement was just added, so that it takes the focus. */
  focusOnShow: boolean;
  onChange?: (change: (statement: Statement) => Statement) => void;
  onRemove?: () => void;
}

function StatementEditor({ id, index, statement, catalogue, buckets, notes, analysis, focusOnShow, onChange, onRemove }: StatementProps) {
  const n = String(index + 1);
  const at = (field: Field) => notes.filter((note) => fieldOf(note.pointer) === field);
  const effect = statement.effect;
  const notesId = (field: Field) => (at(field).length === 0 ? undefined : `${id}-${field}-notes`);
  const sid = statement.sid;

  return (
    <fieldset className="policy-statement">
      <legend className="policy-statement-title">
        Statement {n}
        {typeof sid === 'string' && sid !== '' ? `: ${sid}` : ''}
      </legend>
      <Notes notes={at('statement')} />
      <div className="policy-row">
        <label className="field">
          <span>Effect</span>
          {onChange === undefined ? (
            <input
              className="form-control"
              aria-label={`Effect of statement ${n}`}
              readOnly
              value={effect === undefined ? 'Not given' : shown(effect)}
            />
          ) : (
            <select
              className="form-select"
              aria-label={`Effect of statement ${n}`}
              aria-describedby={notesId('effect')}
              aria-invalid={at('effect').some((note) => note.kind === 'fault') ? true : undefined}
              // The statement just added takes the focus, so that the keyboard is where the work is.
              autoFocus={focusOnShow}
              value={effect ?? ''}
              onChange={(event) => {
                const value = event.target.value;
                onChange((s) => ({ ...s, effect: value }));
              }}
            >
              {effect === 'Allow' || effect === 'Deny' ? null : (
                <option value={effect ?? ''}>{effect === undefined ? 'Not given' : `As written: ${shown(effect)}`}</option>
              )}
              <option value="Allow">Allow</option>
              <option value="Deny">Deny</option>
            </select>
          )}
        </label>
        <label className="field">
          <span>Sid (optional)</span>
          <input
            className="form-control"
            aria-label={`Sid (optional) of statement ${n}`}
            aria-describedby={notesId('sid')}
            readOnly={onChange === undefined}
            autoCapitalize="none"
            spellCheck={false}
            value={sid ?? ''}
            onChange={(event) => {
              const value = event.target.value;
              onChange?.((s) => ({ ...s, sid: sidOf(value) }));
            }}
          />
        </label>
      </div>
      <Notes notes={at('effect')} id={notesId('effect')} />
      <Notes notes={at('sid')} id={notesId('sid')} />

      <ActionsField
        id={`${id}-actions`}
        n={n}
        actions={statement.action}
        catalogue={catalogue.actions}
        notes={at('action')}
        analysis={analysis}
        onChange={
          onChange === undefined
            ? undefined
            : (action) => {
                onChange((s) => ({ ...s, action }));
              }
        }
      />
      <ResourcesField
        id={`${id}-resources`}
        n={n}
        resources={statement.resource}
        templates={catalogue.resources}
        buckets={buckets}
        notes={at('resource')}
        onChange={
          onChange === undefined
            ? undefined
            : (resource) => {
                onChange((s) => ({ ...s, resource }));
              }
        }
      />
      <ConditionsField
        id={`${id}-conditions`}
        n={n}
        statement={statement}
        keys={catalogue.condition_keys}
        notes={at('condition')}
        onChange={onChange}
      />
      {onRemove === undefined ? null : (
        <div className="policy-statement-actions">
          <button
            type="button"
            className="btn btn-outline-danger btn-sm"
            aria-label={`Remove statement ${n}`}
            title="Remove this statement from the policy"
            onClick={onRemove}
          >
            <i className="bi bi-trash" aria-hidden="true" /> <span className="btn-text">Remove statement</span>
          </button>
        </div>
      )}
    </fieldset>
  );
}

/** Adds what is typed when Enter is pressed in a field, rather than sending the form. */
function onEnter(add: () => void): (event: KeyboardEvent<HTMLInputElement>) => void {
  return (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      add();
    }
  };
}

/** The values of a field as chips, each with a button that removes it. */
function Chips({
  values,
  what,
  n,
  flagged,
  onRemove,
}: {
  values: Strings | undefined;
  /** What a value is, for the names of the buttons: "action", "resource". */
  what: string;
  n: string;
  /** What is said of a value beside it, if anything. */
  flagged?: (value: string | null) => string | undefined;
  onRemove?: (index: number) => void;
}) {
  const list = values === undefined ? [] : valuesOf(values);
  if (list.length === 0) {
    return <p className="policy-empty">None yet.</p>;
  }
  return (
    <ul className="policy-chips">
      {list.map((value, i) => {
        const flag = flagged?.(value);
        return (
          <li key={`${String(i)}${shown(value)}`} className={flag === undefined ? 'policy-chip' : 'policy-chip policy-chip-flagged'}>
            <code>{shown(value)}</code>
            {flag === undefined ? null : <span className="policy-chip-flag">{flag}</span>}
            {onRemove === undefined ? null : (
              <button
                type="button"
                className="btn btn-link btn-sm policy-chip-remove"
                aria-label={`Remove ${what} ${shown(value)} from statement ${n}`}
                title={`Remove this ${what}`}
                onClick={() => {
                  onRemove(i);
                }}
              >
                <i className="bi bi-x-lg" aria-hidden="true" />
              </button>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/** The actions of the catalogue by their group, in the order the catalogue gives them. */
function groupsOf(actions: readonly CatalogueAction[]): [string, CatalogueAction[]][] {
  const groups = new Map<string, CatalogueAction[]>();
  for (const action of actions) {
    const group = groups.get(action.group);
    if (group === undefined) {
      groups.set(action.group, [action]);
    } else {
      group.push(action);
    }
  }
  return [...groups];
}

interface ActionsProps {
  id: string;
  n: string;
  actions: Strings | undefined;
  catalogue: readonly CatalogueAction[];
  notes: readonly Note[];
  analysis: StatementAnalysis | undefined;
  onChange?: (actions: Strings) => void;
}

function ActionsField({ id, n, actions, catalogue, notes, analysis, onChange }: ActionsProps) {
  const [search, setSearch] = useState('');
  const [pattern, setPattern] = useState('');
  const titleId = `${id}-title`;
  const notesId = notes.length === 0 ? undefined : `${id}-notes`;
  const unmatched = new Set(analysis?.unmatched_patterns ?? []);
  const groups = useMemo(() => groupsOf(catalogue), [catalogue]);
  const term = search.trim().toLowerCase();
  const found =
    term === ''
      ? []
      : catalogue.filter(
          (action) =>
            action.name.toLowerCase().includes(term) ||
            action.description.toLowerCase().includes(term) ||
            action.group.toLowerCase().includes(term),
        );

  const toggle = (action: string, on: boolean) => {
    if (onChange === undefined) {
      return;
    }
    onChange(on ? withValue(actions, action) : withoutAction(actions ?? { form: 'list', values: [] }, action));
  };
  const addPattern = () => {
    const typed = pattern.trim();
    if (typed === '' || onChange === undefined) {
      return;
    }
    onChange(withValue(actions, typed));
    setPattern('');
  };
  const checks = (list: readonly CatalogueAction[]) => (
    <ul className="policy-action-list">
      {list.map((action) => {
        const box = `${id}-${action.name}`;
        return (
          <li key={action.name}>
            <input
              id={box}
              type="checkbox"
              className="form-check-input"
              aria-describedby={`${box}-what`}
              checked={namesAction(actions, action.name)}
              onChange={(event) => {
                toggle(action.name, event.target.checked);
              }}
            />{' '}
            <label htmlFor={box}>
              <code>{action.name}</code>
            </label>{' '}
            <span id={`${box}-what`} className="policy-action-what">
              {action.description}
            </span>
          </li>
        );
      })}
    </ul>
  );

  return (
    <div className="policy-field" role="group" aria-labelledby={titleId} aria-describedby={notesId}>
      <p id={titleId} className="policy-field-title">
        Actions of statement {n}
      </p>
      <Chips
        values={actions}
        what="action"
        n={n}
        flagged={(value) => (value !== null && unmatched.has(value) ? 'covers no action this node knows' : undefined)}
        onRemove={
          onChange === undefined || actions === undefined
            ? undefined
            : (i) => {
                onChange(withoutValue(actions, i));
              }
        }
      />
      <Notes notes={notes} id={notesId} />
      {analysis === undefined ? null : (
        <details className="policy-covers">
          <summary>
            Covers {counted(analysis.actions.length, 'action', 'actions')}, as the node reads them
          </summary>
          {analysis.actions.length === 0 ? null : (
            <ul className="policy-covers-list">
              {analysis.actions.map((action) => (
                <li key={action}>
                  <code>{action}</code>
                </li>
              ))}
            </ul>
          )}
        </details>
      )}
      {onChange === undefined ? null : (
        <div className="policy-picker">
          <label className="field">
            <span>Search actions</span>
            <input
              type="search"
              className="form-control"
              aria-label={`Search actions of statement ${n}`}
              autoCapitalize="none"
              spellCheck={false}
              value={search}
              onKeyDown={(event) => {
                if (event.key === 'Enter') {
                  event.preventDefault();
                }
              }}
              onChange={(event) => {
                setSearch(event.target.value);
              }}
            />
          </label>
          {term === '' ? (
            groups.map(([group, list]) => (
              <details key={group} className="policy-group">
                <summary>
                  {group} ({counted(list.filter((action) => namesAction(actions, action.name)).length, 'chosen', 'chosen')})
                </summary>
                {checks(list)}
              </details>
            ))
          ) : found.length === 0 ? (
            <p className="policy-empty">No action of the catalogue matches “{search.trim()}”.</p>
          ) : (
            checks(found)
          )}
          <div className="policy-add">
            <label className="field">
              <span>Action pattern</span>
              <input
                className="form-control"
                aria-label={`Action pattern for statement ${n}`}
                placeholder="s3:Get*"
                autoCapitalize="none"
                spellCheck={false}
                value={pattern}
                onKeyDown={onEnter(addPattern)}
                onChange={(event) => {
                  setPattern(event.target.value);
                }}
              />
            </label>
            <button
              type="button"
              className="btn btn-outline-secondary"
              aria-label={`Add pattern to statement ${n}`}
              title="Add the action or pattern as typed: what it covers is the node's to say"
              disabled={pattern.trim() === ''}
              onClick={addPattern}
            >
              <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add pattern</span>
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

const S3_PREFIX = 'arn:aws:s3:::';
const ADMIN_PREFIX = 'arn:cactusfs:admin:::';

interface ResourcesProps {
  id: string;
  n: string;
  resources: Strings | undefined;
  templates: readonly CatalogueResource[];
  buckets: string[] | undefined;
  notes: readonly Note[];
  onChange?: (resources: Strings) => void;
}

function ResourcesField({ id, n, resources, templates, buckets, notes, onChange }: ResourcesProps) {
  const [bucket, setBucket] = useState('');
  const [prefix, setPrefix] = useState('');
  const admin = templates.filter((resource) => resource.template.startsWith(ADMIN_PREFIX));
  const [kind, setKind] = useState(admin[0]?.kind ?? '');
  const [adminName, setAdminName] = useState('');
  const [arn, setArn] = useState('');
  const titleId = `${id}-title`;
  const listId = `${id}-buckets`;
  const notesId = notes.length === 0 ? undefined : `${id}-notes`;
  const bucketTemplate = templates.find((resource) => resource.kind === 'bucket' && resource.template.startsWith(S3_PREFIX));
  const objectTemplate = templates.find((resource) => resource.kind === 'object' && resource.template.startsWith(S3_PREFIX));
  const chosen = admin.find((resource) => resource.kind === kind);
  const named = chosen !== undefined && placeholdersOf(chosen.template) > 0;

  const add = (resource: string | undefined) => {
    if (resource === undefined || onChange === undefined) {
      return false;
    }
    onChange(withValue(resources, resource));
    return true;
  };
  const addBucket = () => {
    if (bucketTemplate !== undefined && add(fillTemplate(bucketTemplate.template, [bucket.trim()]))) {
      setPrefix('');
    }
  };
  const addObjects = () => {
    if (objectTemplate !== undefined && add(fillTemplate(objectTemplate.template, [bucket.trim(), `${prefix}*`]))) {
      setPrefix('');
    }
  };
  const addAdmin = () => {
    if (chosen !== undefined && add(fillTemplate(chosen.template, named ? [adminName.trim()] : []))) {
      setAdminName('');
    }
  };
  const addArn = () => {
    if (add(arn.trim() === '' ? undefined : arn.trim())) {
      setArn('');
    }
  };

  return (
    <div className="policy-field" role="group" aria-labelledby={titleId} aria-describedby={notesId}>
      <p id={titleId} className="policy-field-title">
        Resources of statement {n}
      </p>
      <Chips
        values={resources}
        what="resource"
        n={n}
        onRemove={
          onChange === undefined || resources === undefined
            ? undefined
            : (i) => {
                onChange(withoutValue(resources, i));
              }
        }
      />
      <Notes notes={notes} id={notesId} />
      {onChange === undefined ? null : (
        <div className="policy-picker">
          {bucketTemplate === undefined && objectTemplate === undefined ? null : (
            <div className="policy-add">
              <label className="field">
                <span>Bucket</span>
                <input
                  className="form-control"
                  aria-label={`Bucket for statement ${n}`}
                  list={buckets === undefined ? undefined : listId}
                  autoCapitalize="none"
                  spellCheck={false}
                  value={bucket}
                  onKeyDown={onEnter(addObjects)}
                  onChange={(event) => {
                    setBucket(event.target.value);
                  }}
                />
              </label>
              {buckets === undefined ? null : (
                <datalist id={listId}>
                  {buckets.map((name) => (
                    <option key={name} value={name} />
                  ))}
                </datalist>
              )}
              <label className="field">
                <span>Prefix (optional)</span>
                <input
                  className="form-control"
                  aria-label={`Prefix (optional) for statement ${n}`}
                  autoCapitalize="none"
                  spellCheck={false}
                  value={prefix}
                  onKeyDown={onEnter(addObjects)}
                  onChange={(event) => {
                    setPrefix(event.target.value);
                  }}
                />
              </label>
              {bucketTemplate === undefined ? null : (
                <button
                  type="button"
                  className="btn btn-outline-secondary"
                  aria-label={`Add bucket to statement ${n}`}
                  title={`Add the bucket itself: ${bucketTemplate.template}`}
                  disabled={bucket.trim() === ''}
                  onClick={addBucket}
                >
                  <i className="bi bi-bucket" aria-hidden="true" /> <span className="btn-text">Add bucket</span>
                </button>
              )}
              {objectTemplate === undefined ? null : (
                <button
                  type="button"
                  className="btn btn-outline-secondary"
                  aria-label={`Add objects to statement ${n}`}
                  title="Add the objects of the bucket whose keys begin with the prefix"
                  disabled={bucket.trim() === ''}
                  onClick={addObjects}
                >
                  <i className="bi bi-files" aria-hidden="true" /> <span className="btn-text">Add objects</span>
                </button>
              )}
            </div>
          )}
          {admin.length === 0 ? null : (
            <div className="policy-add">
              <label className="field">
                <span>Admin resource</span>
                <select
                  className="form-select"
                  aria-label={`Admin resource for statement ${n}`}
                  aria-describedby={chosen === undefined ? undefined : `${id}-kind-what`}
                  value={kind}
                  onChange={(event) => {
                    setKind(event.target.value);
                  }}
                >
                  {admin.map((resource) => (
                    <option key={resource.kind} value={resource.kind}>
                      {resource.kind}
                    </option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span>Name</span>
                <input
                  className="form-control"
                  aria-label={`Name for statement ${n}`}
                  disabled={!named}
                  autoCapitalize="none"
                  spellCheck={false}
                  value={named ? adminName : ''}
                  onKeyDown={onEnter(addAdmin)}
                  onChange={(event) => {
                    setAdminName(event.target.value);
                  }}
                />
              </label>
              <button
                type="button"
                className="btn btn-outline-secondary"
                aria-label={`Add admin resource to statement ${n}`}
                title="Add the administrative resource"
                disabled={chosen === undefined || (named && adminName.trim() === '')}
                onClick={addAdmin}
              >
                <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add admin resource</span>
              </button>
              {chosen === undefined ? null : (
                <p id={`${id}-kind-what`} className="form-hint policy-add-hint">
                  <code>{chosen.template}</code>: {chosen.description}
                </p>
              )}
            </div>
          )}
          <div className="policy-add">
            <label className="field">
              <span>ARN</span>
              <input
                className="form-control"
                aria-label={`ARN for statement ${n}`}
                placeholder="arn:aws:s3:::photos/*"
                autoCapitalize="none"
                spellCheck={false}
                value={arn}
                onKeyDown={onEnter(addArn)}
                onChange={(event) => {
                  setArn(event.target.value);
                }}
              />
            </label>
            <button
              type="button"
              className="btn btn-outline-secondary"
              aria-label={`Add ARN to statement ${n}`}
              title="Add the resource as typed"
              disabled={arn.trim() === ''}
              onClick={addArn}
            >
              <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add ARN</span>
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

/** What a value of a kind looks like, to show in an empty field. */
const EXAMPLES: Readonly<Record<CatalogueConditionKey['value_kind'], string>> = {
  ip_prefix: '192.0.2.0/24',
  bool: 'true',
  date_time: '2027-01-01T00:00:00Z',
  string: 'alice',
};

interface ConditionsProps {
  id: string;
  n: string;
  statement: Statement;
  keys: readonly CatalogueConditionKey[];
  notes: readonly Note[];
  onChange?: (change: (statement: Statement) => Statement) => void;
}

function ConditionsField({ id, n, statement, keys, notes, onChange }: ConditionsProps) {
  const [key, setKey] = useState(keys[0]?.key ?? '');
  const chosen = keys.find((item) => item.key === key);
  const [operator, setOperator] = useState(chosen?.operators[0] ?? '');
  const [value, setValue] = useState('');
  const titleId = `${id}-title`;
  const whatId = `${id}-unknown`;
  const notesId = notes.length === 0 ? undefined : `${id}-notes`;
  const blocks = statement.condition ?? [];
  const operators = chosen?.operators ?? [];
  const op = operators.includes(operator) ? operator : (operators[0] ?? '');
  const typed = chosen?.value_kind === 'bool' && value === '' ? 'true' : value;

  const add = () => {
    if (onChange === undefined || chosen === undefined || op === '' || typed.trim() === '') {
      return;
    }
    onChange((s) => ({ ...s, condition: withCondition(s.condition, op, chosen.key, typed.trim()) }));
    setValue('');
  };

  return (
    <div className="policy-field" role="group" aria-labelledby={titleId} aria-describedby={notesId}>
      <p id={titleId} className="policy-field-title">
        Conditions of statement {n}
      </p>
      {blocks.length === 0 ? <p className="policy-empty">None: the statement holds whatever the request.</p> : null}
      {blocks.length === 0 ? null : (
        <ul className="policy-chips">
          {blocks.flatMap((block, b) =>
            block.tests.map((test, t) => {
              const values = valuesOf(test.values).map(shown).join(', ');
              return (
                <li key={`${String(b)}-${String(t)}`} className="policy-chip">
                  <code>{block.operator}</code> <code>{test.key}</code> <code>{values}</code>
                  {onChange === undefined ? null : (
                    <button
                      type="button"
                      className="btn btn-link btn-sm policy-chip-remove"
                      aria-label={`Remove condition ${block.operator} ${test.key} from statement ${n}`}
                      title="Remove this condition"
                      onClick={() => {
                        onChange((s) => ({ ...s, condition: withoutCondition(s.condition ?? [], b, t) }));
                      }}
                    >
                      <i className="bi bi-x-lg" aria-hidden="true" />
                    </button>
                  )}
                </li>
              );
            }),
          )}
        </ul>
      )}
      <Notes notes={notes} id={notesId} />
      {onChange === undefined || keys.length === 0 ? null : (
        <div className="policy-picker">
          <div className="policy-add">
            <label className="field">
              <span>Condition key</span>
              <select
                className="form-select"
                aria-label={`Condition key for statement ${n}`}
                aria-describedby={chosen === undefined ? undefined : whatId}
                value={key}
                onChange={(event) => {
                  setKey(event.target.value);
                  setValue('');
                }}
              >
                {keys.map((item) => (
                  <option key={item.key} value={item.key}>
                    {item.key}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>Operator</span>
              <select
                className="form-select"
                aria-label={`Operator for statement ${n}`}
                value={op}
                onChange={(event) => {
                  setOperator(event.target.value);
                }}
              >
                {operators.map((item) => (
                  <option key={item} value={item}>
                    {item}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>Value</span>
              {chosen?.value_kind === 'bool' ? (
                <select
                  className="form-select"
                  aria-label={`Value for statement ${n}`}
                  value={typed}
                  onChange={(event) => {
                    setValue(event.target.value);
                  }}
                >
                  <option value="true">true</option>
                  <option value="false">false</option>
                </select>
              ) : (
                <input
                  className="form-control"
                  aria-label={`Value for statement ${n}`}
                  placeholder={chosen === undefined ? undefined : EXAMPLES[chosen.value_kind]}
                  autoCapitalize="none"
                  spellCheck={false}
                  value={value}
                  onKeyDown={onEnter(add)}
                  onChange={(event) => {
                    setValue(event.target.value);
                  }}
                />
              )}
            </label>
            <button
              type="button"
              className="btn btn-outline-secondary"
              aria-label={`Add condition to statement ${n}`}
              title="Add the condition: the statement holds only when it does"
              disabled={chosen === undefined || typed.trim() === ''}
              onClick={add}
            >
              <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add condition</span>
            </button>
            {chosen === undefined ? null : (
              <p id={whatId} className="form-hint policy-add-hint">
                {chosen.when_unknown}
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
