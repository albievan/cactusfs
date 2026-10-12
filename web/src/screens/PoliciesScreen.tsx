import { useState, type FormEvent } from 'react';

import { api, unwrap, type Policy } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatPolicyDocument, formatTime, parsePolicyDocument } from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadPolicies() {
  return unwrap(api.GET('/policies'));
}

/**
 * The policies. A policy document is edited as JSON text; the console checks
 * that it is JSON and leaves the rest to the API. Built-in policies are only
 * shown.
 */
export function PoliciesScreen() {
  const policies = useLoad(loadPolicies);
  const action = useAction();
  const [name, setName] = useState('');
  const [document, setDocument] = useState('');
  const [open, setOpen] = useState<string | null>(null);

  const create = (event: FormEvent) => {
    event.preventDefault();
    let parsed: Record<string, unknown>;
    try {
      parsed = parsePolicyDocument(document);
    } catch (error) {
      action.fail(error);
      return;
    }
    void action.run(async () => {
      await unwrap(api.POST('/policies', { body: { name, document: parsed } }));
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
          <label className="field field-full">
            <span>Policy document as JSON</span>
            <textarea
              className="form-control"
              name="document"
              required
              rows={10}
              spellCheck={false}
              value={document}
              onChange={(event) => {
                setDocument(event.target.value);
              }}
            />
          </label>
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
  onSaved: () => void;
  /** Closes the editor; without it the editor has no Close button. */
  onClose?: () => void;
}

/** Shows a policy document and, unless the policy is built in, lets it be changed. */
export function PolicyEditor({ policy, action, onSaved, onClose }: PolicyEditorProps) {
  const heading = `edit-policy-${policy.name}`;
  const [text, setText] = useState(formatPolicyDocument(policy.document));

  const save = (event: FormEvent) => {
    event.preventDefault();
    let parsed: Record<string, unknown>;
    try {
      parsed = parsePolicyDocument(text);
    } catch (error) {
      action.fail(error);
      return;
    }
    void action.run(async () => {
      await unwrap(
        api.PUT('/policies/{name}', { params: { path: { name: policy.name } }, body: { document: parsed } }),
      );
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
        <label className="field field-full">
          <span>Policy document of {policy.name} as JSON</span>
          <textarea
            className="form-control"
            name="document"
            required
            rows={14}
            spellCheck={false}
            readOnly={policy.built_in}
            value={text}
            onChange={(event) => {
              setText(event.target.value);
            }}
          />
        </label>
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
