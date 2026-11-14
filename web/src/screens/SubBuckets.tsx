import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';

import {
  api,
  ApiError,
  unwrap,
  type PolicyDocument,
  type Problem,
  type SubBucket,
  type SubBucketSummary,
} from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import {
  formatPolicyDocument,
  formatTime,
  POLICY_COMPACT_HINT,
  POLICY_SIZE_HINT,
  policyLayout,
  typedPolicy,
  type TypedPolicy,
} from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

/**
 * The code of the problem the API answers a path it does not know with
 * (`internal/adapters/adminapi/handler.go`, "No such endpoint"). A bucket,
 * a sub-bucket or a policy that is not there has codes of its own
 * (`NoSuchBucket`, `NoSuchSubBucket`): only a node of a release before these
 * operations - met during a rolling upgrade - answers them `NotFound`.
 */
const NO_SUCH_ENDPOINT = 'NotFound';

/** The rule of a sub-bucket's name, as the contract has it (`SubBucketNameValue`). */
export const SUB_BUCKET_NAME_RULE =
  '1 to 63 lower-case letters, digits and hyphens, beginning and ending with a letter or digit. Sub-buckets do not nest: a name has no slash.';

/** How many sub-buckets a bucket may have, as the contract says (ADR-0017). It is only said: the API counts. */
const MOST_SUB_BUCKETS = 10;

/** A problem the console says something more of, or undefined. */
type Explain = (problem: Problem) => string | undefined;

/** Whether the API refused to tell the caller this: 403. */
function refused(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 403;
}

/** What is said of a 404 `NotFound`: the node that answered is of an older release. */
function olderNode(what: string): Explain {
  return (problem) =>
    problem.status === 404 && problem.code === NO_SUCH_ENDPOINT
      ? `${what} not available on the node that answered (an older release); try again.`
      : undefined;
}

/** The first of the explanations that has something to say. */
function firstOf(...explains: Explain[]): Explain {
  return (problem) => {
    for (const explain of explains) {
      const words = explain(problem);
      if (words !== undefined) {
        return words;
      }
    }
    return undefined;
  };
}

/** What is said when the API answers a code, and the words. */
function onCode(code: string, words: string): Explain {
  return (problem) => (problem.code === code ? words : undefined);
}

const SUB_BUCKETS_ON_AN_OLDER_NODE = olderNode('Sub-buckets are');
const POLICIES_ON_AN_OLDER_NODE = olderNode('Bucket policies are');

interface FoldProps {
  /** The heading, and the name of the region. */
  title: string;
  /** What is shown, for the hint of the button: "the policy of this bucket". */
  what: string;
  children: ReactNode;
}

/**
 * A part of a bucket's details that is folded until it is asked for. What it
 * holds is made - and so asks the API - only once it is unfolded, and is
 * kept, with what was typed into it, when it is folded again.
 */
function Fold({ title, what, children }: FoldProps) {
  const headingId = useId();
  const bodyId = useId();
  const [open, setOpen] = useState(false);
  const [made, setMade] = useState(false);
  return (
    <section className="bucket-section" aria-labelledby={headingId}>
      <h3 id={headingId} className="bucket-section-heading">
        <button
          type="button"
          className="bucket-details-toggle"
          aria-expanded={open}
          aria-controls={bodyId}
          title={open ? `Hide ${what}` : `Show ${what}`}
          onClick={() => {
            setOpen(!open);
            setMade(true);
          }}
        >
          <i className={`bi ${open ? 'bi-chevron-down' : 'bi-chevron-right'}`} aria-hidden="true" />
          {title}
        </button>
      </h3>
      <div id={bodyId} className="bucket-section-body" hidden={!open}>
        {made ? children : null}
      </div>
    </section>
  );
}

interface LoadFailureProps {
  error: unknown;
  explain: Explain;
  onRetry: () => void;
  /** What the button's hint says is asked for again. */
  what: string;
}

/** Why something could not be read, in the API's words, and a way to ask again. */
function LoadFailure({ error, explain, onRetry, what }: LoadFailureProps) {
  return (
    <div className="bucket-section-failure">
      <ProblemNotice error={error} explain={explain} />
      <button type="button" className="btn btn-outline-secondary btn-sm" onClick={onRetry} title={`Ask for ${what} again`}>
        <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Try again</span>
      </button>
    </div>
  );
}

interface PolicyTextProps {
  /** The label of the text: "Policy of bucket photos as JSON". */
  label: string;
  /** The document it begins with; undefined for an empty text. */
  initial: PolicyDocument | undefined;
  /** The field of the request body the document is sent in. */
  field: string;
  /** Carries the save and the removal out, and holds the error the editor shows. */
  action: Action;
  explain: Explain;
  /** The hint of the save button. */
  saveTitle: string;
  /** Sends the document, as typed. */
  onSave: (typed: TypedPolicy) => Promise<void>;
  /** Removes the policy, once asked; absent when there is none to remove. */
  remove?: { question: string; run: () => Promise<void> };
  /** More buttons, after the editor's own. */
  children?: ReactNode;
}

/**
 * A policy document edited as JSON text. The console checks only that it is
 * one JSON object and sends it as typed; whether it is a policy that can be
 * kept is the API's to say, and what it says is shown under the text. Sent as
 * typed, its spaces count against the API's bound: a stored document that
 * indented would pass it is shown compact.
 */
function PolicyText({ label, initial, field, action, explain, saveTitle, onSave, remove, children }: PolicyTextProps) {
  const hintId = useId();
  const [layout] = useState(() => (initial === undefined ? { text: '', compact: false } : policyLayout(initial)));
  const [text, setText] = useState(layout.text);

  const save = (event: FormEvent) => {
    event.preventDefault();
    let typed: TypedPolicy;
    try {
      typed = typedPolicy(field, text);
    } catch (error) {
      action.fail(error);
      return;
    }
    void action.run(() => onSave(typed));
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <label className="field field-full">
        <span>{label}</span>
        <textarea
          className="form-control"
          name="policy"
          required
          rows={12}
          spellCheck={false}
          autoCapitalize="none"
          aria-describedby={hintId}
          value={text}
          onChange={(event) => {
            setText(event.target.value);
          }}
        />
      </label>
      <p id={hintId} className="form-hint field-full">
        {layout.compact ? `${POLICY_COMPACT_HINT} ` : ''}
        {POLICY_SIZE_HINT}
      </p>
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button type="submit" className="btn btn-primary" disabled={action.busy} title={saveTitle}>
          <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save policy</span>
        </button>
        {remove === undefined ? null : (
          <ConfirmButton
            label="Remove policy"
            question={remove.question}
            disabled={action.busy}
            onConfirm={() => {
              void action.run(remove.run);
            }}
          />
        )}
        {children}
      </div>
    </form>
  );
}

/** The key that makes a fresh editor whenever the document it shows changes. */
function editorKey(document: PolicyDocument | undefined): string {
  return document === undefined ? '-' : formatPolicyDocument(document);
}

/**
 * The policy of a bucket (ADR-0011), folded until it is asked for: shown as
 * indented JSON, changed, and removed after a question. What the caller may
 * read, set or remove is the API's to say.
 */
export function BucketPolicySection({ bucket }: { bucket: string }) {
  return (
    <Fold title="Bucket policy" what="the policy of this bucket">
      <BucketPolicyBody bucket={bucket} />
    </Fold>
  );
}

function BucketPolicyBody({ bucket }: { bucket: string }) {
  const load = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/buckets/{bucket}/policy', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const policy = useLoad(load);
  const action = useAction();

  if (policy.data === undefined && policy.loading) {
    return <Loading />;
  }
  if (policy.data === undefined && !refused(policy.error)) {
    return (
      <LoadFailure
        error={policy.error}
        explain={POLICIES_ON_AN_OLDER_NODE}
        onRetry={policy.reload}
        what="the policy of this bucket"
      />
    );
  }

  const document = policy.data?.document;
  const save = async (typed: TypedPolicy) => {
    await unwrap(
      api.PUT('/buckets/{bucket}/policy', {
        params: { path: { bucket } },
        body: { document: typed.document },
        bodySerializer: () => typed.body,
      }),
    );
    policy.reload();
  };
  const remove = async () => {
    await unwrap(api.DELETE('/buckets/{bucket}/policy', { params: { path: { bucket } } }));
    policy.reload();
  };

  return (
    <>
      {policy.data === undefined ? (
        <p className="bucket-facts-note">
          You may not read the policy of this bucket. A policy saved here replaces the one it has.
        </p>
      ) : document === undefined ? (
        <p className="bucket-facts-note">
          This bucket has no policy: only the policies of the callers decide what they may do with it.
        </p>
      ) : null}
      {policy.data !== undefined && policy.error !== null ? (
        <LoadFailure
          error={policy.error}
          explain={POLICIES_ON_AN_OLDER_NODE}
          onRetry={policy.reload}
          what="the policy of this bucket"
        />
      ) : null}
      <PolicyText
        key={editorKey(document)}
        label={`Policy of bucket ${bucket} as JSON`}
        initial={document}
        field="document"
        action={action}
        explain={POLICIES_ON_AN_OLDER_NODE}
        saveTitle="Set this policy in place of the one the bucket has"
        onSave={save}
        // Offered whenever there may be a policy to remove: one the caller
        // may not read may still be one they may remove.
        remove={
          policy.data !== undefined && document === undefined
            ? undefined
            : {
                question: `Remove the policy of bucket ${bucket}?`,
                run: remove,
              }
        }
      />
    </>
  );
}

/**
 * The sub-buckets of a bucket (ADR-0017), folded until they are asked for:
 * the list, a sub-bucket's policy, creating one and deleting an empty one.
 * The API decides who may see and change what; the console shows what it
 * was told, and says so where it was refused.
 */
export function SubBucketsSection({ bucket }: { bucket: string }) {
  return (
    <Fold title="Sub-buckets" what="the sub-buckets of this bucket">
      <SubBucketsBody bucket={bucket} />
    </Fold>
  );
}

function SubBucketsBody({ bucket }: { bucket: string }) {
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/subbuckets', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const list = useLoad(load);
  const removal = useAction();
  const [open, setOpen] = useState<string | null>(null);
  // What holds the list: where the keyboard goes back to from a closed panel.
  const listRef = useRef<HTMLDivElement>(null);
  // The sub-bucket the last deletion was of: what its error is said of.
  const [deleting, setDeleting] = useState('');
  const shown = list.data;

  const remove = (name: string) => {
    setDeleting(name);
    void removal.run(async () => {
      try {
        await unwrap(api.DELETE('/buckets/{bucket}/subbuckets/{name}', { params: { path: { bucket, name } } }));
        setOpen((current) => (current === name ? null : current));
        // The row, with the button the keyboard was on, goes: the list takes it.
        listRef.current?.focus();
      } finally {
        // Deleted or not, the list says what is there now.
        list.reload();
      }
    });
  };

  // Closed, a panel hands the keyboard back to the button that opened it, or
  // to the list when that row is no longer there.
  const close = (name: string) => {
    setOpen(null);
    const opener = [...(listRef.current?.querySelectorAll<HTMLButtonElement>('button[data-sub-bucket]') ?? [])].find(
      (button) => button.dataset.subBucket === name,
    );
    (opener ?? listRef.current)?.focus();
  };

  const explainDeletion = firstOf(
    SUB_BUCKETS_ON_AN_OLDER_NODE,
    onCode(
      'SubBucketNotEmpty',
      `Sub-bucket ${deleting} is not empty: the bucket holds objects, older versions, delete markers or uploads in progress under "${deleting}/". Only an empty sub-bucket is deleted, and its keys are never deleted with it: delete them first.`,
    ),
    onCode('NoSuchSubBucket', `Sub-bucket ${deleting} is not there any more.`),
  );

  return (
    <>
      <p className="form-hint">
        A sub-bucket is a named first-level prefix of the bucket: its objects are the keys that begin with its name and
        a slash, and S3 clients address them as keys of the bucket. A bucket has at most {MOST_SUB_BUCKETS}; each may
        have a policy of its own.
      </p>
      <div
        ref={listRef}
        className="sub-bucket-list"
        role="region"
        aria-label={`Sub-buckets of bucket ${bucket}`}
        tabIndex={-1}
      >
        <SubBucketList
          list={shown}
          loading={list.loading}
          error={list.error}
          onRetry={list.reload}
          busy={removal.busy}
          onOpen={setOpen}
          onDelete={remove}
        />
      </div>
      <ProblemNotice error={removal.error} explain={explainDeletion} />
      {open === null ? null : (
        <SubBucketPanel
          key={open}
          bucket={bucket}
          name={open}
          onClose={() => {
            close(open);
          }}
          onChanged={list.reload}
        />
      )}
      <CreateSubBucket bucket={bucket} shown={shown} onCreated={list.reload} />
    </>
  );
}

interface SubBucketListProps {
  list: SubBucketSummary[] | undefined;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  busy: boolean;
  onOpen: (name: string) => void;
  onDelete: (name: string) => void;
}

/** The list of sub-buckets, or what is said in its place. */
function SubBucketList({ list, loading, error, onRetry, busy, onOpen, onDelete }: SubBucketListProps) {
  if (list === undefined) {
    if (loading) {
      return <Loading />;
    }
    if (refused(error)) {
      // Not a failure: the API shows a caller who can see nothing of the
      // bucket none of its sub-buckets, and does not say whether it is there.
      return <p className="bucket-facts-note">You may not see the sub-buckets of this bucket.</p>;
    }
    return (
      <LoadFailure
        error={error}
        explain={SUB_BUCKETS_ON_AN_OLDER_NODE}
        onRetry={onRetry}
        what="the sub-buckets of this bucket"
      />
    );
  }
  const failed =
    error === null ? null : (
      <LoadFailure
        error={error}
        explain={SUB_BUCKETS_ON_AN_OLDER_NODE}
        onRetry={onRetry}
        what="the sub-buckets of this bucket"
      />
    );
  if (list.length === 0) {
    return (
      <>
        {failed}
        <p>There are no sub-buckets you may see.</p>
      </>
    );
  }
  return (
    <>
      {failed}
      <table className="table table-hover align-middle grid">
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Created</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          {list.map((sub) => (
            <tr key={sub.name}>
              <td data-label="Name">{sub.name}</td>
              <td data-label="Created">{formatTime(sub.created_at)}</td>
              <td data-label="Actions">
                <span className="d-flex flex-wrap gap-2">
                  <button
                    type="button"
                    className="btn btn-outline-secondary"
                    aria-label={`Open sub-bucket ${sub.name}`}
                    data-sub-bucket={sub.name}
                    title="Show and change the policy of this sub-bucket"
                    onClick={() => {
                      onOpen(sub.name);
                    }}
                  >
                    <i className="bi bi-shield-lock" aria-hidden="true" /> <span className="btn-text">Policy</span>
                  </button>
                  <ConfirmButton
                    label="Delete"
                    question={`Delete sub-bucket ${sub.name}? Its keys are not deleted with it.`}
                    disabled={busy}
                    onConfirm={() => {
                      onDelete(sub.name);
                    }}
                  />
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

interface SubBucketPanelProps {
  bucket: string;
  name: string;
  onClose: () => void;
  /** Called once the sub-bucket changed, so that the list is read again. */
  onChanged: () => void;
}

/**
 * One sub-bucket: when it was made, and its policy, shown, changed and
 * removed. Opened, it takes the keyboard to its heading and is scrolled into
 * view.
 */
function SubBucketPanel({ bucket, name, onClose, onChanged }: SubBucketPanelProps) {
  const headingId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  // The sub-bucket a Save or a Remove made anew, when it had been deleted
  // since it was shown: a PUT creates a sub-bucket that is not there.
  const [recreated, setRecreated] = useState<SubBucket | null>(null);
  useEffect(() => {
    const shownHeading = heading.current;
    if (shownHeading === null) {
      return;
    }
    shownHeading.focus();
    // Not every engine has it (jsdom has not); focus alone scrolls in a browser.
    if (typeof shownHeading.scrollIntoView === 'function') {
      shownHeading.scrollIntoView({ block: 'nearest' });
    }
  }, []);
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/subbuckets/{name}', { params: { path: { bucket, name } }, signal })),
    [bucket, name],
  );
  const sub = useLoad(load);
  const action = useAction();
  const explain = firstOf(
    SUB_BUCKETS_ON_AN_OLDER_NODE,
    onCode('NoSuchSubBucket', `Sub-bucket ${name} is not there any more.`),
  );
  const close = (
    <button type="button" className="btn btn-outline-secondary" onClick={onClose} title="Close this sub-bucket">
      <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close</span>
    </button>
  );

  let body: ReactNode;
  if (sub.data === undefined && sub.loading) {
    body = <Loading />;
  } else if (sub.data === undefined && !refused(sub.error)) {
    body = (
      <>
        <LoadFailure error={sub.error} explain={explain} onRetry={sub.reload} what={`sub-bucket ${name}`} />
        <div className="d-flex flex-wrap gap-2">{close}</div>
      </>
    );
  } else {
    const policy = sub.data?.policy;
    const shownCreated = sub.data?.created_at;
    // A PUT replaces what the sub-bucket is: with a policy it sets it, and
    // without one it removes the one it had (the contract's SubBucketPut).
    // It also creates a sub-bucket that is not there: one deleted since it
    // was shown comes back. Its time of creation in the answer then differs
    // from the one shown - as it does when someone else deleted and created
    // it meanwhile, or when the server could not read it back (it answers
    // the time of the request) - so what is said is what was seen, not who
    // caused it. Until the contract has a precondition (open for Albie), it is
    // said rather than prevented; one the caller may not read has no time
    // shown, and nothing can be told of it.
    const put = async (raw: string, document: PolicyDocument | undefined) => {
      setRecreated(null);
      const answer = await unwrap(
        api.PUT('/buckets/{bucket}/subbuckets/{name}', {
          params: { path: { bucket, name } },
          body: document === undefined ? {} : { policy: document },
          bodySerializer: () => raw,
        }),
      );
      if (shownCreated !== undefined && answer.created_at !== shownCreated) {
        setRecreated(answer);
      }
      sub.reload();
      onChanged();
    };
    body = (
      <>
        {sub.data === undefined ? (
          <p className="bucket-facts-note">
            You may not read the policy of this sub-bucket. A policy saved here replaces the one it has.
          </p>
        ) : (
          <p className="bucket-facts-note">
            Created {formatTime(sub.data.created_at)}.{' '}
            {policy === undefined
              ? 'It has no policy of its own: the policy of the bucket and those of the callers decide.'
              : 'Its policy is weighed with that of the bucket and those of the callers: a deny in any refuses.'}
          </p>
        )}
        {sub.data !== undefined && sub.error !== null ? (
          <LoadFailure error={sub.error} explain={explain} onRetry={sub.reload} what={`sub-bucket ${name}`} />
        ) : null}
        <PolicyText
          key={editorKey(policy)}
          label={`Policy of sub-bucket ${name} as JSON`}
          initial={policy}
          field="policy"
          action={action}
          explain={explain}
          saveTitle="Set this policy in place of the one the sub-bucket has"
          onSave={(typed) => put(typed.body, typed.document)}
          remove={
            sub.data !== undefined && policy === undefined
              ? undefined
              : {
                  question: `Remove the policy of sub-bucket ${name}?`,
                  run: () => put('{}', undefined),
                }
          }
        >
          {close}
        </PolicyText>
      </>
    );
  }

  return (
    <section className="bucket-subsection" aria-labelledby={headingId}>
      <h4 id={headingId} ref={heading} tabIndex={-1}>
        Sub-bucket {name}
      </h4>
      {/* In the page before there is anything to say, so that it is announced when there is. */}
      <div className="notice-region" role="status">
        {recreated === null ? null : <Recreated bucket={bucket} sub={recreated} />}
      </div>
      {body}
    </section>
  );
}

/** Says that a sub-bucket was found created anew after it was shown, and what that means for the keys under it. */
function Recreated({ bucket, sub }: { bucket: string; sub: SubBucket }) {
  return (
    <div className="alert alert-warning notice">
      <p className="notice-title">
        {`Sub-bucket ${sub.name} was created again (${formatTime(sub.created_at)}) after it was shown here; it may have been deleted meanwhile.`}
      </p>
      <p>
        The keys of bucket {bucket} under &quot;{sub.name}/&quot; are its objects, and{' '}
        {sub.policy === undefined
          ? 'it has no policy of its own.'
          : 'the policy just saved is weighed for them with that of the bucket.'}
      </p>
    </div>
  );
}

interface CreateSubBucketProps {
  bucket: string;
  /** The sub-buckets the list shows, when it was read. */
  shown: SubBucketSummary[] | undefined;
  onCreated: () => void;
}

/** Creates a sub-bucket, with a policy of its own or without. */
function CreateSubBucket({ bucket, shown, onCreated }: CreateSubBucketProps) {
  const headingId = useId();
  const ruleId = useId();
  const sizeId = useId();
  const action = useAction();
  const [name, setName] = useState('');
  const [text, setText] = useState('');
  // The name the last creation was of: what its error is said of.
  const [creating, setCreating] = useState('');

  const create = (event: FormEvent) => {
    event.preventDefault();
    // What the address cannot carry is not sent: an empty name, "." or ".."
    // would make another path of it. Everything else is the API's to judge.
    if (name === '' || name === '.' || name === '..') {
      action.fail(new Error(`Name the sub-bucket: ${SUB_BUCKET_NAME_RULE}`));
      return;
    }
    // A PUT to a sub-bucket that is there sets its policy in place of the one
    // it has: creating is not the place for that.
    if (shown?.some((sub) => sub.name === name) === true) {
      action.fail(new Error(`Sub-bucket ${name} is there already: open it to change its policy.`));
      return;
    }
    let typed: TypedPolicy | undefined;
    if (text.trim() !== '') {
      try {
        typed = typedPolicy('policy', text);
      } catch (error) {
        action.fail(error);
        return;
      }
    }
    setCreating(name);
    void action.run(async () => {
      await unwrap(
        api.PUT('/buckets/{bucket}/subbuckets/{name}', {
          params: { path: { bucket, name } },
          body: typed === undefined ? {} : { policy: typed.document },
          bodySerializer: () => (typed === undefined ? '{}' : typed.body),
        }),
      );
      setName('');
      setText('');
      onCreated();
    });
  };

  const explain = firstOf(
    SUB_BUCKETS_ON_AN_OLDER_NODE,
    onCode(
      'TooManySubBuckets',
      `A bucket has at most ${String(MOST_SUB_BUCKETS)} sub-buckets, and ${bucket} has as many already: delete one before creating ${creating}.`,
    ),
    onCode('InvalidArgument', `${creating} is not a name a sub-bucket can have: ${SUB_BUCKET_NAME_RULE}`),
    onCode('InvalidPolicy', `The policy of ${creating} was refused, and nothing was created.`),
  );

  return (
    <section className="bucket-subsection" aria-labelledby={headingId}>
      <h4 id={headingId}>Create a sub-bucket</h4>
      <form className="form-grid" onSubmit={create}>
        <label className="field">
          <span>Sub-bucket name</span>
          <input
            className="form-control"
            name="name"
            required
            maxLength={63}
            autoCapitalize="none"
            autoComplete="off"
            spellCheck={false}
            aria-describedby={ruleId}
            value={name}
            onChange={(event) => {
              setName(event.target.value);
            }}
          />
        </label>
        <p id={ruleId} className="form-hint field-full">
          {SUB_BUCKET_NAME_RULE}
        </p>
        <label className="field field-full">
          <span>Policy of the sub-bucket as JSON (optional)</span>
          <textarea
            className="form-control"
            name="policy"
            rows={8}
            spellCheck={false}
            autoCapitalize="none"
            aria-describedby={sizeId}
            value={text}
            onChange={(event) => {
              setText(event.target.value);
            }}
          />
        </label>
        <p id={sizeId} className="form-hint field-full">
          {POLICY_SIZE_HINT}
        </p>
        <div className="field-full">
          <ProblemNotice error={action.error} explain={explain} />
        </div>
        <div className="form-actions d-flex flex-wrap gap-2">
          <button type="submit" className="btn btn-primary" disabled={action.busy} title="Create this sub-bucket">
            <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Create sub-bucket</span>
          </button>
        </div>
      </form>
    </section>
  );
}
