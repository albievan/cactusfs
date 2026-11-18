import { useState, type FormEvent } from 'react';

import { api, unwrap, type Policy } from '../api/client';
import { canonicalPolicy } from '../policy/read';
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
import { PolicyBuilder } from '../ui/PolicyBuilder';
import {
  requireValidAtSave,
  usePolicyTools,
  usePolicyValidation,
  useRefusal,
  type PolicyTools,
} from '../ui/policyHooks';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadPolicies() {
  return unwrap(api.GET('/policies'));
}

/**
 * The policies. A policy document is made with the policy builder beside its
 * JSON, or typed as JSON; the JSON is what is sent, as typed
 * (`typedPolicy`): a key typed twice reaches the API, which refuses it,
 * rather than JavaScript keeping its last value. While the builder is there
 * the node checks the document as it is typed, and a document it finds a
 * fault in is not sent. Built-in policies are only shown.
 */
export function PoliciesScreen() {
  const policies = useLoad(loadPolicies);
  const action = useAction();
  const tools = usePolicyTools();
  const [name, setName] = useState('');
  const [document, setDocument] = useState('');
  const [open, setOpen] = useState<string | null>(null);
  const refusal = useRefusal();
  // An empty document is not checked: the form is only being opened.
  const validation = usePolicyValidation(
    document,
    { name: name === '' ? undefined : name },
    tools.catalogue.status === 'ready',
  );

  const create = (event: FormEvent) => {
    event.preventDefault();
    let typed: TypedPolicy;
    try {
      typed = typedPolicy('document', document, { name });
    } catch (error) {
      action.fail(error);
      return;
    }
    const named = name;
    // The text as it is sent (a copy: the lint of ADR-0028 refuses another name for `document`).
    const sent = `${document}`;
    void action.run(async () => {
      // Checked as it is sent, once the catalogue has answered: a save made while it loads is checked too.
      await requireValidAtSave(tools, validation, sent, { name: named === '' ? undefined : named });
      try {
        await unwrap(
          api.POST('/policies', { body: { name, document: typed.document }, bodySerializer: () => typed.body }),
        );
      } catch (error) {
        refusal.keep(document, error);
        throw error;
      }
      setName('');
      setDocument('');
      policies.reload();
    });
  };

  const remove = (policy: string) => {
    void action.run(async () => {
      await unwrap(api.DELETE('/policies/{name}', { params: { path: { name: policy } } }));
      setOpen((shown) => (shown === policy ? null : shown));
      policies.reload();
    });
  };

  const opened = policies.data?.find((policy) => policy.name === open);

  return (
    <>
      <h1>Policies</h1>
      <ProblemNotice error={action.error ?? policies.error} />

      <section className="card card-body panel" aria-labelledby="create-policy">
        <h2 id="create-policy">Create a policy</h2>
        <form className="form-grid" onSubmit={create}>
          <label className="field">
            <span>Policy name</span>
            <input
              className="form-control"
              name="name"
              required
              autoCapitalize="none"
              spellCheck={false}
              value={name}
              onChange={(event) => {
                setName(event.target.value);
              }}
            />
          </label>
          <PolicyBuilder
            id="create-policy"
            label="Policy document as JSON"
            name="document"
            required
            rows={10}
            text={document}
            onChange={setDocument}
            tools={tools}
            validation={validation}
            refused={refusal.faultsFor(document)}
            hint={POLICY_SIZE_HINT}
          />
          <div className="form-actions d-flex flex-wrap gap-2">
            <button type="submit" className="btn btn-primary" disabled={action.busy} title="Create this policy">
              <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Create policy</span>
            </button>
          </div>
        </form>
      </section>

      {opened === undefined ? null : (
        <PolicyEditor
          // A fresh editor for every policy, and after every save.
          key={`${opened.name}\n${formatPolicyDocument(opened.document)}`}
          policy={opened}
          action={action}
          tools={tools}
          onSaved={policies.reload}
          onClose={() => {
            setOpen(null);
          }}
        />
      )}

      <section className="card card-body panel" aria-labelledby="policy-list">
        <h2 id="policy-list">All policies</h2>
        {policies.loading ? <Loading /> : null}
        {policies.data !== undefined && policies.data.length === 0 ? <p>There are no policies.</p> : null}
        {policies.data !== undefined && policies.data.length > 0 ? (
          <table className="table table-hover align-middle grid">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Kind</th>
                <th scope="col">Created</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {policies.data.map((policy) => (
                <tr key={policy.name}>
                  <td data-label="Name">{policy.name}</td>
                  <td data-label="Kind">{policy.built_in ? 'Built-in' : 'Custom'}</td>
                  <td data-label="Created">{formatTime(policy.created_at)}</td>
                  <td data-label="Actions">
                    <span className="d-flex flex-wrap gap-2">
                      <button
                        type="button"
                        className="btn btn-outline-secondary"
                        aria-label={`${policy.built_in ? 'View' : 'Edit'} policy ${policy.name}`}
                        title={policy.built_in ? 'Show this policy document' : 'Change this policy document'}
                        onClick={() => {
                          setOpen(policy.name);
                        }}
                      >
                        <i className={policy.built_in ? 'bi bi-eye' : 'bi bi-pencil'} aria-hidden="true" />{' '}
                        <span className="btn-text">{policy.built_in ? 'View' : 'Edit'}</span>
                      </button>
                      {policy.built_in ? null : (
                        <ConfirmButton
                          label="Delete"
                          question={`Delete policy ${policy.name}?`}
                          disabled={action.busy}
                          onConfirm={() => {
                            remove(policy.name);
                          }}
                        />
                      )}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
      </section>
    </>
  );
}

interface PolicyEditorProps {
  policy: Policy;
  action: Action;
  /** The node's catalogue and buckets, read once for the screen. */
  tools: PolicyTools;
  onSaved: () => void;
  /** Closes the editor; without it the editor has no Close button. */
  onClose?: () => void;
}

/**
 * Shows a policy document and, unless the policy is built in, lets it be
 * changed, with the policy builder beside its JSON. The document is sent as
 * typed, and only when it says something other than the stored one; a
 * stored one that indented would be larger than the API takes is shown
 * compact. While the builder is there, a document the node finds a fault in
 * is not sent.
 */
export function PolicyEditor({ policy, action, tools, onSaved, onClose }: PolicyEditorProps) {
  const heading = `edit-policy-${policy.name}`;
  const [layout] = useState(() => policyLayout(policy.document));
  const [text, setText] = useState(layout.text);
  const [unchanged, setUnchanged] = useState(false);
  const refusal = useRefusal();
  // The stored document is not checked as it is opened: only once it is
  // changed. Checking it asks for admin:PutPolicy, which a reader may not
  // hold - and a refusal would be audited for a change nobody asked for.
  const changed = canonicalPolicy(text) !== canonicalPolicy(layout.text);
  const validation = usePolicyValidation(
    text,
    { name: policy.name },
    !policy.built_in && changed && tools.catalogue.status === 'ready',
  );

  const save = (event: FormEvent) => {
    event.preventDefault();
    if (policy.built_in) {
      return;
    }
    let typed: TypedPolicy;
    try {
      typed = typedPolicy('document', text);
    } catch (error) {
      action.fail(error);
      return;
    }
    // The same document, laid out otherwise or not at all, is not sent (ADR-0032).
    const same = canonicalPolicy(text);
    if (same !== undefined && same === canonicalPolicy(layout.text)) {
      setUnchanged(true);
      return;
    }
    const checked = text;
    void action.run(async () => {
      // Checked as it is sent, once the catalogue has answered: a save made while it loads is checked too.
      await requireValidAtSave(tools, validation, checked, { name: policy.name });
      try {
        await unwrap(
          api.PUT('/policies/{name}', {
            params: { path: { name: policy.name } },
            body: { document: typed.document },
            bodySerializer: () => typed.body,
          }),
        );
      } catch (error) {
        refusal.keep(text, error);
        throw error;
      }
      onSaved();
    });
  };

  return (
    <section className="card card-body panel" aria-labelledby={heading}>
      <h2 id={heading}>
        Policy {policy.name}
        {policy.built_in ? ' (built-in, read-only)' : ''}
      </h2>
      <form className="form-grid" onSubmit={save}>
        <PolicyBuilder
          id={heading}
          label={`Policy document of ${policy.name} as JSON`}
          name="document"
          required
          rows={14}
          text={text}
          onChange={
            policy.built_in
              ? undefined
              : (changed) => {
                  setText(changed);
                  setUnchanged(false);
                }
          }
          tools={tools}
          validation={policy.built_in ? undefined : validation}
          refused={refusal.faultsFor(text)}
          hint={policy.built_in ? undefined : `${layout.compact ? `${POLICY_COMPACT_HINT} ` : ''}${POLICY_SIZE_HINT}`}
        />
        <p className="form-hint field-full" role="status">
          {unchanged ? 'Nothing has changed, so there is nothing to save.' : ''}
        </p>
        <div className="form-actions d-flex flex-wrap gap-2">
          {policy.built_in ? null : (
            <button type="submit" className="btn btn-primary" disabled={action.busy} title="Save this policy document">
              <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save policy</span>
            </button>
          )}
          {onClose === undefined ? null : (
            <button type="button" className="btn btn-outline-secondary" onClick={onClose} title="Close this">
              <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close</span>
            </button>
          )}
        </div>
      </form>
    </section>
  );
}
