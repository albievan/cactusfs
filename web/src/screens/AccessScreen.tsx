import { useEffect, useId, useLayoutEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';

import {
  api,
  unwrap,
  type AccessExplanation,
  type CatalogueAction,
  type CitedStatement,
  type PolicyCatalogue,
  type Problem,
} from '../api/client';
import type { operations } from '../api/schema';
import { fillTemplate, placeholdersOf } from '../policy/edit';
import { formatTime } from '../ui/format';
import { usePolicyTools, type CatalogueState } from '../ui/policyHooks';
import { ProblemNotice } from '../ui/ProblemNotice';
import { NO_SUCH_ENDPOINT } from '../ui/sections';

// "Who may do this?" (PLAN 9.10, ADR-0032 section 3): asks the answering
// node what its authorizer decides for a subject, an action and a resource,
// and shows the answer as it is given. The node decides, cites and hides;
// nothing here reckons a decision, a citation or what may be read.

/** Whom a question is about: a user, an API key, or a caller without credentials. */
export type SubjectKind = 'user' | 'access_key' | 'anonymous';

/** What is said when the node refuses the question (403). */
export const REFUSED_WORDS =
  'You may not ask this. Asking needs the right to explain access for this subject and, for a question about a bucket, ' +
  "the right to read the bucket's policy and its sub-buckets' policies - or a policy the answer depends on could not be read. " +
  'The refusal is written to the audit trail.';

/** What is said when the user asked about is not there (404 `NoSuchEntity`). */
export const NO_SUCH_USER_WORDS = 'There is no such user.';

/** What is said when the API key asked about is not there (404 `NoSuchEntity`). */
export const NO_SUCH_KEY_WORDS = 'There is no such API key.';

/** What is said when the node has no such question (404 `NotFound`): it is of an older release. */
export const NOT_AVAILABLE_WORDS =
  '"Who may do this?" is not available on the node that answered: it is of an older release. Try again later.';

/** What is said, by the action field, when the catalogue was refused: the action is typed. */
export const CATALOGUE_REFUSED_WORDS =
  'You may not read the catalogue of actions, so type the action as a policy names it, such as s3:GetObject.';

/** What is said, by the action field, when the node has no catalogue (an older release). */
export const CATALOGUE_ABSENT_WORDS =
  'This node has no catalogue of actions: it is of an older release. Type the action as a policy names it, such as s3:GetObject.';

/** What is said, by the action field, when the catalogue could not be read. */
export const CATALOGUE_FAILED_WORDS =
  'The catalogue of actions could not be read, so type the action as a policy names it, such as s3:GetObject.';

// How the catalogue's templates begin: a resource of S3 is named by a
// bucket and a key, an administrative one by what follows the prefix
// (`user/{name}`), which is what `admin_resource` takes.
const S3_PREFIX = 'arn:aws:s3:::';
const ADMIN_PREFIX = 'arn:cactusfs:admin:::';

/** One kind of resource an action is asked on, from a template of the catalogue. */
interface ResourceKind {
  /** The template, as the catalogue gives it. */
  template: string;
  /** Its kind and description, from the catalogue's resources; the template itself where it has none. */
  label: string;
  /** Whether it is named by a bucket and a key, or by the administrative kind and name. */
  service: 's3' | 'admin';
  /** How many placeholders it has: for S3, 0 (every bucket), 1 (a bucket), 2 (a bucket and a key). */
  placeholders: number;
  /** The kind of an administrative resource (`user`), as its name field is labelled. */
  kind: string;
}

/** The kinds of resource an action is asked on, in the catalogue's order; templates of neither prefix are left out. */
function kindsOf(action: CatalogueAction | undefined, catalogue: PolicyCatalogue): ResourceKind[] {
  if (action === undefined) {
    return [];
  }
  const kinds: ResourceKind[] = [];
  for (const template of action.resources) {
    const service = template.startsWith(S3_PREFIX) ? 's3' : template.startsWith(ADMIN_PREFIX) ? 'admin' : undefined;
    if (service === undefined) {
      continue;
    }
    const known = catalogue.resources.find((resource) => resource.template === template);
    const path = template.slice(service === 's3' ? S3_PREFIX.length : ADMIN_PREFIX.length);
    kinds.push({
      template,
      label: known === undefined ? template : `${known.kind}: ${known.description}`,
      service,
      placeholders: placeholdersOf(template),
      kind: known?.kind ?? path.split('/')[0] ?? path,
    });
  }
  return kinds;
}

/** What the form holds. */
interface Form {
  subject: SubjectKind;
  user: string;
  accessKeyId: string;
  action: string;
  /** The template chosen, with the catalogue. */
  template: string;
  /** Without the catalogue: whether the resource is of S3 or administrative. */
  typedService: 's3' | 'admin';
  bucket: string;
  key: string;
  adminKind: string;
  adminName: string;
  listPrefix: string;
  sourceIp: string;
  /** Whether the request would come over TLS: '' leaves it to the node, which says what it assumed. */
  tls: '' | 'yes' | 'no';
}

/** The query of `GET /access/explain`, as the contract gives it. */
type Query = operations['explainAccess']['parameters']['query'];

/**
 * The query of the question the form asks, or why it is not one yet - a
 * field it needs is empty. What the node refuses (an action that is not in
 * its catalogue, a resource that is not of a template of the action, an
 * address that is no address) is the node's to say. `byTemplate`: the
 * resource is named by a template of the catalogue (`kind`), not by the
 * typed fields.
 */
function queryOf(form: Form, kind: ResourceKind | undefined, byTemplate: boolean, typed: boolean): Query | string {
  const query: Query = { action: form.action.trim() };
  switch (form.subject) {
    case 'user':
      if (form.user.trim() === '') {
        return 'Name the user to ask about.';
      }
      query.user = form.user.trim();
      break;
    case 'access_key':
      if (form.accessKeyId.trim() === '') {
        return 'Give the access key ID to ask about.';
      }
      query.access_key_id = form.accessKeyId.trim();
      break;
    case 'anonymous':
      query.anonymous = true;
      break;
  }
  if (query.action === '') {
    return typed ? 'Type an action.' : 'Choose an action.';
  }
  const service = byTemplate ? kind?.service : form.typedService;
  if (service === undefined) {
    return 'Choose the kind of resource.';
  }
  const bucket = form.bucket.trim();
  if (service === 's3') {
    const needsBucket = byTemplate ? (kind?.placeholders ?? 0) >= 1 : form.key !== '';
    const needsKey = byTemplate && (kind?.placeholders ?? 0) >= 2;
    if (needsBucket && bucket === '') {
      return 'Name the bucket.';
    }
    if (needsKey && form.key === '') {
      return 'Give the key of the object.';
    }
    if (bucket !== '' && (!byTemplate || needsBucket)) {
      query.bucket = bucket;
    }
    if (form.key !== '' && (!byTemplate || needsKey)) {
      query.key = form.key;
    }
    if (form.listPrefix !== '' && query.bucket !== undefined) {
      query.list_prefix = form.listPrefix;
    }
  } else {
    const name = form.adminName.trim();
    if (byTemplate && kind !== undefined) {
      const path = kind.template.slice(ADMIN_PREFIX.length);
      const named = kind.placeholders > 0;
      if (named && name === '') {
        return `Give the name of the ${kind.kind}.`;
      }
      const filled = fillTemplate(path, named ? [name] : []);
      if (filled === undefined) {
        return `Give the name of the ${kind.kind}.`;
      }
      query.admin_resource = filled;
    } else {
      const typedKind = form.adminKind.trim();
      if (typedKind === '') {
        return 'Give the kind of the administrative resource.';
      }
      query.admin_resource = name === '' ? typedKind : `${typedKind}/${name}`;
    }
  }
  if (form.sourceIp.trim() !== '') {
    query.source_ip = form.sourceIp.trim();
  }
  if (form.tls !== '') {
    query.secure_transport = form.tls === 'yes';
  }
  return query;
}

/** What is said of a refusal of the question, by its status and code; the API's own words follow. */
function explainProblem(subject: SubjectKind): (problem: Problem) => string | undefined {
  return (problem) => {
    if (problem.status === 403) {
      return REFUSED_WORDS;
    }
    if (problem.status === 404 && problem.code === NO_SUCH_ENDPOINT) {
      return NOT_AVAILABLE_WORDS;
    }
    if (problem.status === 404 && problem.code === 'NoSuchEntity') {
      return subject === 'access_key' ? NO_SUCH_KEY_WORDS : NO_SUCH_USER_WORDS;
    }
    return undefined;
  };
}

/** Why the catalogue is not there, said by the action field. */
function catalogueWords(catalogue: CatalogueState): string | undefined {
  if (catalogue.status !== 'unavailable') {
    return undefined;
  }
  switch (catalogue.why) {
    case 'refused':
      return CATALOGUE_REFUSED_WORDS;
    case 'older-node':
      return CATALOGUE_ABSENT_WORDS;
    default:
      return CATALOGUE_FAILED_WORDS;
  }
}

interface AccessScreenProps {
  /** The user to ask about, from the address (`#/access?user=bob`): a user's page links here with it. */
  user?: string;
}

/**
 * "Who may do this?": a form that asks the node what its authorizer decides
 * for a user, an API key or anyone, an action and a resource, and the
 * answer as the node gives it. Each "Explain" is one request, written to the
 * audit trail; nothing is asked as the screen is shown or as a field is
 * typed into. The catalogue of actions is read on the first change inside
 * the form, or when "Load the list of actions" is pressed - never as the
 * screen is shown or a control is focused, so a caller without
 * `admin:GetPolicyCatalogue` who only looks causes no audited refusal. A
 * caller who may not read it types the action.
 */
export function AccessScreen({ user = '' }: AccessScreenProps) {
  const id = useId();
  const tools = usePolicyTools(false);
  const catalogue = tools.catalogue.status === 'ready' ? tools.catalogue.catalogue : undefined;
  const withCatalogue = catalogue !== undefined;
  const typed = tools.catalogue.status === 'unavailable';
  const [form, setForm] = useState<Form>({
    subject: 'user',
    user,
    accessKeyId: '',
    action: '',
    template: '',
    typedService: 's3',
    bucket: '',
    key: '',
    adminKind: '',
    adminName: '',
    listPrefix: '',
    sourceIp: '',
    tls: '',
  });
  const [answer, setAnswer] = useState<AccessExplanation | undefined>(undefined);
  const [error, setError] = useState<{ cause: unknown; subject: SubjectKind } | null>(null);
  const [incomplete, setIncomplete] = useState<string | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  // The request is never given up: an authorized question is recorded by
  // the node as it is answered, and a request dropped half-way could leave
  // it unrecorded. An answer that comes after the screen went (or was made
  // anew for another user), or for a question asked since, is not shown.
  const mounted = useRef(false);
  const latest = useRef(0);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  // The action field is a list while there is, or may be, a catalogue, and
  // a text field once there is none. When it changes under the keyboard the
  // keyboard goes with it, as the field is put in the page.
  const actionFocused = useRef(false);
  const typedAction = useRef<HTMLInputElement>(null);
  const actionList = useRef<HTMLSelectElement>(null);
  useLayoutEffect(() => {
    if (typed && actionFocused.current) {
      typedAction.current?.focus();
    }
  }, [typed]);

  const actions = (catalogue?.actions ?? []).filter((one) => !one.name.includes('*') && !one.name.includes('?'));
  const chosenAction = actions.find((one) => one.name === form.action);
  const kinds = catalogue === undefined ? [] : kindsOf(chosenAction, catalogue);
  const kind = kinds.find((one) => one.template === form.template) ?? kinds[0];
  const groups = [...new Set(actions.map((one) => one.group))];
  // The resource is named by the action's templates when the catalogue gives
  // it any this console can name; an action whose templates are all of
  // another form is named with the typed fields, as without the catalogue.
  const byTemplate = withCatalogue && (chosenAction === undefined || kinds.length > 0);

  const change = (fields: Partial<Form>) => {
    setForm((previous) => ({ ...previous, ...fields }));
    setIncomplete(undefined);
  };

  const explain = (event: FormEvent) => {
    event.preventDefault();
    if (busy) {
      return;
    }
    const query = queryOf(form, kind, byTemplate, typed);
    if (typeof query === 'string') {
      setIncomplete(query);
      return;
    }
    setIncomplete(undefined);
    setAnswer(undefined);
    setError(null);
    setBusy(true);
    latest.current += 1;
    const ticket = latest.current;
    const wanted = () => mounted.current && latest.current === ticket;
    const subject = form.subject;
    unwrap(api.GET('/access/explain', { params: { query } })).then(
      (given) => {
        if (wanted()) {
          setAnswer(given);
          setBusy(false);
        }
      },
      (cause: unknown) => {
        if (wanted()) {
          setError({ cause, subject });
          setBusy(false);
        }
      },
    );
  };

  const s3 = byTemplate ? kind?.service === 's3' : form.typedService === 's3';
  const s3Fields = byTemplate ? (kind?.service === 's3' ? kind.placeholders : 0) : 2;
  const actionHint = `${id}-action-hint`;
  const kindHint = `${id}-kind-hint`;

  return (
    <>
      <h1>Who may do this?</h1>
      <p className="form-hint">
        Ask what the authorizer of this node decides for a user, an API key or anyone without credentials, and why.
        Each question is written to the audit trail.
      </p>

      <section className="card card-body panel" aria-labelledby={`${id}-question`}>
        <h2 id={`${id}-question`}>The question</h2>
        <form
          className="form-grid access-form"
          aria-labelledby={`${id}-question`}
          onSubmit={explain}
          noValidate
        >
          <fieldset className="field-full access-group">
            <legend>Who</legend>
            <div className="access-choices">
              {(
                [
                  ['user', 'A user'],
                  ['access_key', 'An API key'],
                  ['anonymous', 'Anyone without credentials'],
                ] as const
              ).map(([value, label]) => (
                <label className="check" key={value}>
                  <input
                    className="form-check-input"
                    type="radio"
                    name="subject"
                    value={value}
                    checked={form.subject === value}
                    onChange={() => {
                      change({ subject: value });
                    }}
                  />
                  <span>{label}</span>
                </label>
              ))}
            </div>
            {form.subject === 'user' ? (
              <label className="field">
                <span>User</span>
                <input
                  className="form-control"
                  name="user"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  required
                  value={form.user}
                  onChange={(event) => {
                    change({ user: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {form.subject === 'access_key' ? (
              <label className="field">
                <span>Access key ID</span>
                <input
                  className="form-control"
                  name="access_key_id"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  required
                  maxLength={128}
                  value={form.accessKeyId}
                  onChange={(event) => {
                    change({ accessKeyId: event.target.value });
                  }}
                />
              </label>
            ) : null}
          </fieldset>

          {/* Changing the action reads the catalogue; typing a user's name reads nothing. */}
          <fieldset className="field-full access-group" onChange={tools.want}>
            <legend>Does what</legend>
            {typed ? (
              <label className="field">
                <span>Action</span>
                <input
                  ref={typedAction}
                  className="form-control"
                  name="action"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  required
                  maxLength={128}
                  aria-describedby={actionHint}
                  value={form.action}
                  onChange={(event) => {
                    change({ action: event.target.value });
                  }}
                />
              </label>
            ) : (
              <label className="field">
                <span>Action</span>
                <select
                  ref={actionList}
                  className="form-select"
                  name="action"
                  required
                  aria-describedby={actionHint}
                  aria-busy={tools.catalogue.status === 'loading'}
                  value={form.action}
                  onFocus={() => {
                    actionFocused.current = true;
                  }}
                  onBlur={() => {
                    actionFocused.current = false;
                  }}
                  onChange={(event) => {
                    change({ action: event.target.value, template: '' });
                  }}
                >
                  <option value="">
                    {tools.catalogue.status === 'loading' ? 'Reading the actions…' : 'Choose an action'}
                  </option>
                  {groups.map((group) => (
                    <optgroup key={group} label={group}>
                      {actions
                        .filter((one) => one.group === group)
                        .map((one) => (
                          <option key={one.name} value={one.name}>
                            {one.name}
                          </option>
                        ))}
                    </optgroup>
                  ))}
                </select>
              </label>
            )}
            {tools.catalogue.status === 'idle' ? (
              <div>
                <button
                  type="button"
                  className="btn btn-outline-secondary btn-sm"
                  title="Read the catalogue of actions from the node now"
                  onClick={() => {
                    tools.want();
                    // The button goes once the list is asked for: the keyboard goes to the list.
                    actionList.current?.focus();
                  }}
                >
                  <i className="bi bi-list-ul" aria-hidden="true" />{' '}
                  <span className="btn-text">Load the list of actions</span>
                </button>
              </div>
            ) : null}
            <p id={actionHint} className="form-hint">
              {catalogueWords(tools.catalogue) ?? chosenAction?.description ?? ''}
            </p>
          </fieldset>

          <fieldset className="field-full access-group">
            <legend>On what</legend>
            {byTemplate ? (
              kinds.length === 0 ? (
                <p className="form-hint">Choose an action to see what it is asked on.</p>
              ) : (
                <div className="field">
                  <label className="field">
                    <span>Kind of resource</span>
                    <select
                      className="form-select"
                      name="resource_kind"
                      aria-describedby={kindHint}
                      value={kind?.template ?? ''}
                      onChange={(event) => {
                        change({ template: event.target.value });
                      }}
                    >
                      {kinds.map((one) => (
                        <option key={one.template} value={one.template}>
                          {one.label}
                        </option>
                      ))}
                    </select>
                  </label>
                  <span id={kindHint} className="form-hint access-template">
                    <code>{kind?.template}</code>
                  </span>
                </div>
              )
            ) : (
              <div className="access-choices">
                {withCatalogue ? (
                  <p className="form-hint">
                    This console cannot name what the catalogue says this action is asked on: give the resource below.
                  </p>
                ) : null}
                {(
                  [
                    ['s3', 'A bucket, or an object of a bucket'],
                    ['admin', 'An administrative resource'],
                  ] as const
                ).map(([value, label]) => (
                  <label className="check" key={value}>
                    <input
                      className="form-check-input"
                      type="radio"
                      name="resource_service"
                      value={value}
                      checked={form.typedService === value}
                      onChange={() => {
                        change({ typedService: value });
                      }}
                    />
                    <span>{label}</span>
                  </label>
                ))}
              </div>
            )}
            {s3 && s3Fields >= 1 ? (
              <label className="field">
                <span>{byTemplate ? 'Bucket' : 'Bucket (none for every bucket)'}</span>
                <input
                  className="form-control"
                  name="bucket"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  maxLength={63}
                  value={form.bucket}
                  onChange={(event) => {
                    change({ bucket: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {s3 && s3Fields >= 2 ? (
              <label className="field">
                <span>{byTemplate ? 'Key of the object' : 'Key of the object (optional)'}</span>
                <input
                  className="form-control"
                  name="key"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  maxLength={1024}
                  value={form.key}
                  onChange={(event) => {
                    change({ key: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {!s3 && !byTemplate ? (
              <label className="field">
                <span>Kind (as user, policy, cluster)</span>
                <input
                  className="form-control"
                  name="admin_kind"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  value={form.adminKind}
                  onChange={(event) => {
                    change({ adminKind: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {!s3 && (!byTemplate || (kind?.placeholders ?? 0) > 0) ? (
              <label className="field">
                <span>{byTemplate ? `Name of the ${kind?.kind ?? 'resource'}` : 'Name (optional)'}</span>
                <input
                  className="form-control"
                  name="admin_name"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  value={form.adminName}
                  onChange={(event) => {
                    change({ adminName: event.target.value });
                  }}
                />
              </label>
            ) : null}
          </fieldset>

          <details className="field-full access-more">
            <summary>From where, and how (optional)</summary>
            <div className="form-grid access-more-fields">
              {/* Offered for any resource with a bucket, and without the catalogue for any
                  action: a convenience of the form, not a rule. Which actions list is the
                  node's to say, and it refuses a prefix for one that lists nothing (400). */}
              {s3 && s3Fields >= 1 ? (
                <label className="field">
                  <span>Listing prefix (for a listing only)</span>
                  <input
                    className="form-control"
                    name="list_prefix"
                    autoCapitalize="none"
                    autoComplete="off"
                    spellCheck={false}
                    maxLength={1024}
                    value={form.listPrefix}
                    onChange={(event) => {
                      change({ listPrefix: event.target.value });
                    }}
                  />
                </label>
              ) : null}
              <label className="field">
                <span>Source address (IPv4 or IPv6; empty for unknown)</span>
                <input
                  className="form-control"
                  name="source_ip"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  inputMode="text"
                  value={form.sourceIp}
                  onChange={(event) => {
                    change({ sourceIp: event.target.value });
                  }}
                />
              </label>
              <label className="field">
                <span>Over TLS</span>
                <select
                  className="form-select"
                  name="secure_transport"
                  value={form.tls}
                  onChange={(event) => {
                    change({ tls: event.target.value as Form['tls'] });
                  }}
                >
                  <option value="">Not said: as the node assumes</option>
                  <option value="yes">Yes</option>
                  <option value="no">No</option>
                </select>
              </label>
            </div>
          </details>

          <div className="form-actions d-flex flex-wrap gap-2 align-items-center">
            <button type="submit" className="btn btn-primary" disabled={busy} title="Ask the node, once">
              <i className="bi bi-question-circle" aria-hidden="true" /> <span className="btn-text">Explain</span>
            </button>
            <span role="status">{incomplete ?? (busy ? 'Asking…' : '')}</span>
          </div>
        </form>
      </section>

      <ProblemNotice error={error?.cause ?? null} explain={explainProblem(error?.subject ?? 'user')} />

      <section className="card card-body panel access-answer" aria-labelledby={`${id}-answer`}>
        <h2 id={`${id}-answer`}>The answer</h2>
        <div aria-live="polite">
          {answer === undefined ? (
            <p className="form-hint">{busy ? 'Asking the node…' : 'Ask a question to see what the node decides.'}</p>
          ) : (
            <Answer answer={answer} />
          )}
        </div>
      </section>
    </>
  );
}

/** How a source of a decision or a statement is said; a value of a newer release is shown as it is. */
const SOURCE_WORDS: Record<string, string> = {
  none: 'no policy',
  rule: 'a rule of the authorizer',
  'identity-policy': "a policy of the subject's",
  'bucket-policy': "the bucket's policy",
  'sub-bucket-policy': "the sub-bucket's policy",
  'api-key-policy': "the key's own policy",
  owner: 'the owner of the version',
};

/** How a rule that answered is said. */
const RULE_WORDS: Record<string, string> = {
  root: 'the root identity may do everything',
  anonymous: 'a caller without credentials',
  service: 'a service of the deployment',
  node: 'another node of the deployment',
  locked_session: 'a locked session',
  no_user: 'there is no such user',
};

/** Why a subject would not authenticate. */
const NOT_AUTHENTICATED_WORDS: Record<string, string> = {
  user_disabled: 'the user is disabled (for an API key: its owner is)',
  key_expired: 'the API key has expired',
};

/** What an answer may leave out. */
const NOT_CONSIDERED_WORDS: Record<string, string> = {
  object_ownership: 'who made the version of the object (its ownership)',
};

function said(words: Record<string, string>, value: string): string {
  return words[value] ?? value;
}

/** The answer as the node gave it. */
function Answer({ answer }: { answer: AccessExplanation }) {
  const subject =
    answer.subject.kind === 'anonymous'
      ? 'anyone without credentials'
      : answer.subject.kind === 'access_key'
        ? `the API key ${answer.subject.access_key_id ?? ''}${answer.subject.user === undefined ? '' : ` of ${answer.subject.user}`}`
        : `the user ${answer.subject.user ?? ''}`;
  return (
    <>
      {!answer.authenticates ? null : answer.allowed === undefined ? (
        <p className="access-verdict">No decision was given</p>
      ) : (
        <p className={answer.allowed ? 'access-verdict access-allowed' : 'access-verdict access-denied'}>
          <i className={answer.allowed ? 'bi bi-check-circle-fill' : 'bi bi-x-circle-fill'} aria-hidden="true" />{' '}
          {answer.allowed ? 'Allowed' : 'Not allowed'}
        </p>
      )}
      {answer.authenticates ? null : (
        <p className="access-verdict access-denied">
          <i className="bi bi-slash-circle-fill" aria-hidden="true" /> Would not authenticate
          {answer.not_authenticated === undefined ? '' : `: ${said(NOT_AUTHENTICATED_WORDS, answer.not_authenticated)}`}. No
          decision is given.
        </p>
      )}
      <dl className="facts">
        <dt>Asked about</dt>
        <dd>{subject}</dd>
        <dt>Action</dt>
        <dd>
          <code>{answer.request.action}</code>
        </dd>
        <dt>Resource</dt>
        <dd>
          <code>{answer.request.resource}</code>
        </dd>
        {answer.request.list_prefix === undefined ? null : (
          <>
            <dt>Listing prefix</dt>
            <dd>
              <code>{answer.request.list_prefix}</code>
            </dd>
          </>
        )}
        {answer.authenticates ? (
          <>
            <dt>Decided by</dt>
            <dd>{decidedBy(answer)}</dd>
          </>
        ) : null}
      </dl>

      {answer.authenticates ? (
        <>
          <Citations title="Statements that had a say" statements={answer.statements ?? []} none="None." />
          {answer.key_policy === undefined ? null : (
            <section className="access-part" aria-label="The key's own policy">
              <h3>The key&apos;s own policy</h3>
              <p>{answer.key_policy.allowed ? 'It allows this.' : 'It does not allow this.'}</p>
              <Citations title="Its statements that had a say" statements={answer.key_policy.statements} none="None." />
            </section>
          )}
          {(answer.unknown_address ?? []).length === 0 ? null : (
            <Citations
              title="Decided without a source address"
              lead="These statements have a condition on aws:SourceIp, decided without an address."
              statements={answer.unknown_address ?? []}
              none="None."
            />
          )}
        </>
      ) : null}

      <section className="access-part" aria-label="What was assumed">
        <h3>What was assumed</h3>
        <dl className="facts">
          <dt>Time, the node&apos;s</dt>
          <dd>
            <time dateTime={answer.assumed.time}>{formatTime(answer.assumed.time)}</time>
          </dd>
          <dt>Source address</dt>
          <dd>{answer.assumed.source_ip === undefined ? 'Unknown' : <code>{answer.assumed.source_ip}</code>}</dd>
          <dt>Over TLS</dt>
          <dd>{answer.assumed.secure_transport ? 'Yes' : 'No'}</dd>
        </dl>
      </section>

      <section className="access-part" aria-label="What was not considered">
        <h3>What was not considered</h3>
        {answer.not_considered.length === 0 ? (
          <p>Nothing.</p>
        ) : (
          <ul>
            {answer.not_considered.map((what) => (
              <li key={what}>{said(NOT_CONSIDERED_WORDS, what)}</li>
            ))}
          </ul>
        )}
      </section>
    </>
  );
}

/** What decided, as the answer says it: no policy (default deny), a rule, or a source of statements. */
function decidedBy(answer: AccessExplanation): ReactNode {
  switch (answer.source) {
    case undefined:
      return '-';
    case 'none':
      return 'No statement: nothing allows it, so it is denied by default.';
    case 'rule':
      return `A rule of the authorizer: ${said(RULE_WORDS, answer.rule ?? '')}.`;
    default:
      return said(SOURCE_WORDS, answer.source);
  }
}

interface CitationsProps {
  title: string;
  lead?: string;
  statements: CitedStatement[];
  none: string;
}

/** Cited statements, each named as far as the caller may read it. */
function Citations({ title, lead, statements, none }: CitationsProps) {
  return (
    <section className="access-part" aria-label={title}>
      <h3>{title}</h3>
      {lead === undefined ? null : <p>{lead}</p>}
      {statements.some((statement) => statement.readable) ? (
        <p className="form-hint">Documents and statements are counted from 0, as the node gives them.</p>
      ) : null}
      {statements.length === 0 ? (
        <p>{none}</p>
      ) : (
        <ul className="access-citations">
          {statements.map((statement, index) => (
            // A list as given, without a key of its own: its place is its key.
            <li key={`${String(index)} ${statement.source} ${statement.effect}`}>
              <Citation statement={statement} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** One cited statement: its effect and source, and where it is when the caller may read it. */
function Citation({ statement }: { statement: CitedStatement }) {
  const effect = (
    <strong className={statement.effect === 'Deny' ? 'access-deny' : 'access-allow'}>{statement.effect}</strong>
  );
  if (!statement.readable) {
    return (
      <>
        {effect} in {said(SOURCE_WORDS, statement.source)}: <em>a policy you may not read</em>
      </>
    );
  }
  return (
    <>
      {effect} in {said(SOURCE_WORDS, statement.source)}
      {statement.policy === undefined ? null : (
        <>
          {' '}
          <code>{statement.policy}</code>
        </>
      )}
      {statement.document_index === undefined ? null : <>, document {statement.document_index}</>}
      {statement.statement_index === undefined ? null : <>, statement {statement.statement_index}</>}
      {statement.sid === undefined || statement.sid === '' ? null : (
        <>
          , Sid <code>{statement.sid}</code>
        </>
      )}
    </>
  );
}
