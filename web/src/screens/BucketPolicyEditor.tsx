import { useEffect, useId, useMemo, useRef, useState, type FormEvent, type ReactNode } from 'react';

import type { PolicyDocument, PolicyFault, StatementAnalysis } from '../api/client';
import { valuesOf, type PolicyKind, type Statement } from '../policy/model';
import { canonicalPolicy, readPolicy } from '../policy/read';
import { ConfirmButton } from '../ui/ConfirmButton';
import { POLICY_COMPACT_HINT, POLICY_SIZE_HINT, policyLayout, typedPolicy, type TypedPolicy } from '../ui/format';
import type { Action } from '../ui/hooks';
import { PolicyBuilder } from '../ui/PolicyBuilder';
import {
  askAtSave,
  CHANGED_WHILE_CHECKED,
  checkingOf,
  NOT_SAVED_NOTICE,
  openToAnyone,
  usePolicyValidation,
  useRefusal,
  type PolicyTools,
  type ValidationTarget,
} from '../ui/policyHooks';
import { ProblemNotice } from '../ui/ProblemNotice';
import type { Explain } from '../ui/sections';

// The policy of a bucket or of a sub-bucket, with the policy builder beside
// its JSON (ADR-0032, PLAN 9.9). As for every policy the console edits, the
// JSON is what is sent, as shown; a document the same in meaning as the
// stored one is never sent, so that one stored through S3 stays the bytes
// S3's GetBucketPolicy answers; and the node checks the document only once
// it differs from the stored one. Before a document is sent whose
// statements the node says are open to anyone - or a bucket's policy the
// node could not check at all - the console says so, and asks.

/** Where the policy is kept. */
export type PolicyPlace = { kind: 'bucket'; bucket: string } | { kind: 'sub-bucket'; bucket: string; subBucket: string };

interface BucketPolicyEditorProps {
  place: PolicyPlace;
  /** The label of the JSON text: "Policy of bucket photos as JSON". */
  label: string;
  /** The stored document; undefined when there is none, or the caller may not read it. */
  initial: PolicyDocument | undefined;
  /** The node's catalogue, read once for the page once it is wanted. */
  tools: PolicyTools;
  /** Carries the save and the removal out, and holds the error the editor shows. */
  action: Action;
  explain: Explain;
  /** The hint of the save button. */
  saveTitle: string;
  /** Sends the document as typed, in the body of the put. */
  onSave: (typed: TypedPolicy) => Promise<void>;
  /** Removes the policy, once asked; absent when there is none to remove. */
  remove?: { question: string; run: () => Promise<void> };
  /** More buttons, after the editor's own. */
  children?: ReactNode;
}

/** A save waiting for the answer to a question: what was checked, which is what will be sent. */
type Asking = { text: string; typed: TypedPolicy } & (
  | { kind: 'open'; open: StatementAnalysis[] }
  /** The node has no validation (404): whether the policy opens the bucket to anyone is not known. */
  | { kind: 'unchecked' }
);

/** The field of the put's body the document is sent in. */
function fieldOf(place: PolicyPlace): string {
  return place.kind === 'bucket' ? 'document' : 'policy';
}

/** "bucket photos", "sub-bucket reports of photos". */
function nameOf(place: PolicyPlace): string {
  return place.kind === 'bucket' ? `bucket ${place.bucket}` : `sub-bucket ${place.subBucket} of ${place.bucket}`;
}

/**
 * Edits the policy of a bucket or a sub-bucket: the builder beside the JSON
 * when the node has a catalogue for the caller, and the JSON alone, with a
 * notice of why, when it has not.
 */
export function BucketPolicyEditor({
  place,
  label,
  initial,
  tools,
  action,
  explain,
  saveTitle,
  onSave,
  remove,
  children,
}: BucketPolicyEditorProps) {
  const id = useId();
  const [layout] = useState(() => (initial === undefined ? { text: '', compact: false } : policyLayout(initial)));
  const [text, setText] = useState(layout.text);
  const [unchanged, setUnchanged] = useState(false);
  const [saving, setSaving] = useState(false);
  const [asking, setAsking] = useState<Asking | null>(null);
  const saveButton = useRef<HTMLButtonElement>(null);
  const statusLine = useRef<HTMLParagraphElement>(null);
  // The text as it is now, for a save to see whether it was changed while it was checked.
  const latest = useRef(text);
  useEffect(() => {
    latest.current = text;
  }, [text]);
  const refusal = useRefusal();
  const field = fieldOf(place);
  const checking = checkingOf(tools.catalogue);
  // Checked only once it is changed, never as it is opened: checking asks
  // for the put's own action, which a reader may not hold - and a refusal
  // would be audited for a change nobody asked for.
  const changed = canonicalPolicy(text) !== canonicalPolicy(layout.text);
  const target: ValidationTarget =
    place.kind === 'bucket'
      ? { kind: 'bucket', bucket: place.bucket }
      : { kind: 'sub-bucket', bucket: place.bucket, subBucket: place.subBucket };
  const validation = usePolicyValidation(text, target, changed && checking === 'as-typed');
  // The faults the save-time check found, with the text it checked.
  const [checked, setChecked] = useState<{ text: string; faults: PolicyFault[] } | null>(null);

  const send = async (sent: string, typed: TypedPolicy) => {
    try {
      await onSave(typed);
    } catch (error) {
      refusal.keep(sent, error);
      throw error;
    }
  };

  const save = (event: FormEvent) => {
    event.preventDefault();
    setAsking(null);
    let typed: TypedPolicy;
    try {
      typed = typedPolicy(field, text);
    } catch (error) {
      action.fail(error);
      return;
    }
    // The same document, laid out otherwise or not at all, is not sent (ADR-0032):
    // one stored through S3 keeps the bytes it was stored with.
    const same = canonicalPolicy(text);
    if (same !== undefined && same === canonicalPolicy(layout.text)) {
      setUnchanged(true);
      return;
    }
    const sent = text;
    void action.run(async () => {
      // Without the builder (but for a node of an older release) the policy
      // is still checked, once, as it is saved: the check is authorized as
      // the put is, so a caller who may save causes no false denial, and the
      // question before a policy open to anyone is still asked.
      // How is decided by the catalogue's answer, waited for if it is not in yet.
      // A bucket's policy is checked at save on a node of an older release
      // too: its 404 is asked about rather than taken for "not open".
      const asked = await askAtSave(checkingOf(await tools.settled(), place.kind === 'bucket'), validation, sent, target);
      // What was checked is what is sent, or nothing is.
      if (latest.current !== sent) {
        throw new Error(CHANGED_WHILE_CHECKED);
      }
      if (asked.status === 'checked' && !asked.result.valid) {
        setChecked({ text: sent, faults: asked.result.faults });
        throw new Error(NOT_SAVED_NOTICE);
      }
      // A node that could not check a bucket's policy cannot say whether it
      // opens the bucket to anyone: that is asked, not taken for no. (A
      // sub-bucket's policy cannot allow anything to anyone: its put refuses it.)
      if (asked.status === 'absent' && place.kind === 'bucket') {
        setAsking({ kind: 'unchecked', text: sent, typed });
        return;
      }
      const open = asked.status === 'checked' ? openToAnyone(asked.result) : [];
      if (open.length > 0) {
        setAsking({ kind: 'open', text: sent, typed, open });
        return;
      }
      await send(sent, typed);
    });
  };

  const shownAsking = asking !== null && asking.text === text ? asking : null;
  const confirm = () => {
    if (shownAsking === null) {
      return;
    }
    setAsking(null);
    setSaving(true);
    // The question goes, and the keyboard with it: to the line that says the policy is being saved.
    statusLine.current?.focus();
    void action.run(() => send(shownAsking.text, shownAsking.typed)).finally(() => {
      setSaving(false);
    });
  };
  const cancel = () => {
    setAsking(null);
    saveButton.current?.focus();
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <PolicyBuilder
        id={id}
        scope={place}
        label={label}
        name="policy"
        required
        rows={12}
        text={text}
        onChange={(next) => {
          setText(next);
          setUnchanged(false);
          setAsking(null);
        }}
        tools={tools}
        validation={validation}
        refused={refusal.faultsFor(text) ?? (checked?.text === text ? checked.faults : undefined)}
        hint={`${layout.compact ? `${POLICY_COMPACT_HINT} ` : ''}${POLICY_SIZE_HINT}`}
      />
      <p ref={statusLine} className="form-hint field-full" role="status" tabIndex={-1}>
        {unchanged ? 'Nothing has changed, so there is nothing to save.' : saving ? 'Saving the policy…' : ''}
      </p>
      {shownAsking === null ? null : shownAsking.kind === 'open' ? (
        <SaveQuestion
          title={`This policy opens ${nameOf(place)} to ${shownAsking.open.some((statement) => statement.anonymous_actions.length > 0) ? 'anyone' : 'every user of this deployment'}`}
          confirm="Save, open to anyone"
          confirmTitle="Save the policy, open to anyone as it says"
          busy={action.busy}
          onConfirm={confirm}
          onCancel={cancel}
        >
          <OpenStatements place={place} text={shownAsking.text} open={shownAsking.open} />
          <p>
            This is what the node reads in the statements, before their conditions, which may narrow it. Save the policy
            as it is?
          </p>
        </SaveQuestion>
      ) : (
        <SaveQuestion
          title="This node could not check the policy"
          confirm="Save as it is"
          confirmTitle="Save the policy unchecked"
          busy={action.busy}
          onConfirm={confirm}
          onCancel={cancel}
        >
          <p>
            This node could not check the policy, so it cannot say whether it opens {nameOf(place)} to anyone. Save it as
            it is?
          </p>
        </SaveQuestion>
      )}
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button ref={saveButton} type="submit" className="btn btn-primary" disabled={action.busy} title={saveTitle}>
          <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save policy</span>
        </button>
        {remove === undefined ? null : (
          <ConfirmButton
            label="Remove policy"
            question={remove.question}
            disabled={action.busy}
            onConfirm={() => {
              setAsking(null);
              void action.run(remove.run);
            }}
          />
        )}
        {children}
      </div>
    </form>
  );
}

interface SaveQuestionProps {
  title: string;
  /** What the button that saves says. */
  confirm: string;
  confirmTitle: string;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  /** What is said under the title. */
  children: ReactNode;
}

/**
 * A question before a save, in the page under the policy. It takes the
 * keyboard when it is shown, and Escape cancels it. It is not modal
 * (`aria-modal` is not set and the keyboard is not held in it), as the
 * other questions of the console (ConfirmButton) are not: the page stays
 * usable, and any change of the policy withdraws the question, since it is
 * about the text that was checked.
 */
function SaveQuestion({ title, confirm, confirmTitle, busy, onConfirm, onCancel, children }: SaveQuestionProps) {
  const titleId = useId();
  const bodyId = useId();
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    box.current?.focus();
  }, []);

  return (
    <div
      ref={box}
      className="policy-anyone-warning field-full"
      role="alertdialog"
      aria-labelledby={titleId}
      aria-describedby={bodyId}
      tabIndex={-1}
      onKeyDown={(event) => {
        if (event.key === 'Escape') {
          event.preventDefault();
          onCancel();
        }
      }}
    >
      <p id={titleId} className="notice-title">
        <i className="bi bi-exclamation-triangle" aria-hidden="true" /> {title}
      </p>
      <div id={bodyId}>{children}</div>
      <div className="d-flex flex-wrap gap-2">
        <button type="button" className="btn btn-outline-danger" disabled={busy} title={confirmTitle} onClick={onConfirm}>
          <i className="bi bi-globe" aria-hidden="true" /> <span className="btn-text">{confirm}</span>
        </button>
        <button type="button" className="btn btn-outline-secondary" title="Send nothing, and go back to the policy" onClick={onCancel}>
          <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Cancel</span>
        </button>
      </div>
    </div>
  );
}

/** "s3:GetObject", "s3:GetObject and s3:ListBucket", "a, b and c". */
function listed(names: readonly string[]): string {
  if (names.length <= 1) {
    return names.join('');
  }
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1] ?? ''}`;
}

/**
 * What anyone could do by one open statement, in the words of the node's
 * analysis of it: its `anonymous_actions`, then its `never_anonymous`.
 */
function whatAnyoneCould(analysis: StatementAnalysis): string {
  const anonymous = analysis.anonymous_actions;
  const signedIn = analysis.never_anonymous;
  const said: string[] = [];
  if (anonymous.length > 0) {
    said.push(`anyone, signed in or not, could ${listed(anonymous)}.`);
  }
  if (signedIn.length > 0) {
    said.push(
      `${anonymous.length > 0 ? 'It also names' : 'it names'} for anyone ${listed(signedIn)}, never allowed without signing in: every user of this deployment could do ${signedIn.length === 1 ? 'it' : 'them'}, and nobody who has not signed in.`,
    );
  }
  if (said.length === 0) {
    said.push('it names for anyone no action this node knows.');
  }
  return said.join(' ');
}

/** The statements of the text as written, for their resources; none when it cannot be read. */
function statementsOf(text: string, kind: PolicyKind): Statement[] {
  const reading = readPolicy(text, kind);
  return reading.status === 'read' ? (reading.policy.statements ?? []) : [];
}

/**
 * What anyone could do, statement by statement, as the node's analysis gives
 * it: the actions a caller who has not signed in could be let in to
 * (`anonymous_actions`), and those the statement names for anyone that are
 * never allowed without signing in (`never_anonymous`) - every user of the
 * deployment could do these, and nobody else. The resources are the
 * statement's own, as written.
 */
function OpenStatements({ place, text, open }: { place: PolicyPlace; text: string; open: readonly StatementAnalysis[] }) {
  const statements = useMemo(() => statementsOf(text, place.kind), [text, place.kind]);
  return (
    <ul>
      {open.map((analysis) => {
        const resources = statements[analysis.index]?.resource;
        const on = resources === undefined ? [] : valuesOf(resources).map((value) => (value === null ? 'null' : value));
        return (
          <li key={analysis.index}>
            Statement {analysis.index + 1}
            {analysis.sid === undefined || analysis.sid === '' ? '' : ` (${analysis.sid})`}
            {on.length === 0 ? '' : `, on ${listed(on)}`}: {whatAnyoneCould(analysis)}
          </li>
        );
      })}
    </ul>
  );
}
