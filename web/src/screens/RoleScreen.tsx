import { useCallback, useState } from 'react';

import { api, ApiError, unwrap } from '../api/client';
import { navigate, routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime, joinNames } from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { NamesEditor } from '../ui/NamesEditor';
import { ProblemNotice } from '../ui/ProblemNotice';
import { PolicyEditor } from './PoliciesScreen';

interface RoleScreenProps {
  /** The name of the role. */
  name: string;
}

/**
 * One role: the policies it is made of, each with its document, which can be
 * changed here unless it is built in. A role that is not built in can be given
 * other policies, and deleted.
 */
export function RoleScreen({ name }: RoleScreenProps) {
  const loadRole = useCallback(() => unwrap(api.GET('/roles/{name}', { params: { path: { name } } })), [name]);
  const role = useLoad(loadRole);
  const action = useAction();
  const [editing, setEditing] = useState(false);

  const save = (names: string[]) => {
    void action.run(async () => {
      await unwrap(api.PUT('/roles/{name}', { params: { path: { name } }, body: { policies: names } }));
      setEditing(false);
      role.reload();
    });
  };

  const remove = () => {
    void action.run(async () => {
      await unwrap(api.DELETE('/roles/{name}', { params: { path: { name } } }));
      navigate(routeHref(['roles']));
    });
  };

  const builtIn = role.data?.built_in === true;
  const attached = role.data?.policies ?? [];

  return (
    <>
      <h1>Role {name}</h1>
      <p>
        <a className="btn btn-outline-secondary btn-sm" href={routeHref(['roles'])}>
          <i className="bi bi-arrow-left" aria-hidden="true" /> All roles
        </a>
      </p>
      <ProblemNotice error={action.error ?? role.error} />

      <section className="card card-body panel" aria-labelledby="role-settings">
        <h2 id="role-settings">Settings</h2>
        {role.loading ? <Loading /> : null}
        {role.data === undefined ? null : (
          <>
            <dl className="facts">
              <dt>Kind</dt>
              <dd>{builtIn ? 'Built-in (read-only)' : 'Custom'}</dd>
              <dt>Created</dt>
              <dd>{formatTime(role.data.created_at)}</dd>
              <dt>Policies</dt>
              <dd>
                {editing ? (
                  <NamesEditor
                    label={`Policies of role ${name}`}
                    names={attached}
                    busy={action.busy}
                    onSave={save}
                    onCancel={() => {
                      setEditing(false);
                    }}
                  />
                ) : (
                  joinNames(attached) || '-'
                )}
              </dd>
            </dl>
            {builtIn ? (
              <p className="text-secondary mb-0">
                A built-in role and its policy cannot be changed or deleted. Make a role of your own with the policies
                you want.
              </p>
            ) : (
              <div className="d-flex flex-wrap gap-2">
                <button
                  type="button"
                  className="btn btn-outline-secondary"
                  disabled={editing}
                  onClick={() => {
                    setEditing(true);
                  }}
                >
                  <i className="bi bi-pencil" aria-hidden="true" /> Change policies
                </button>
                <ConfirmButton
                  label="Delete"
                  question={`Delete role ${name}?`}
                  disabled={action.busy}
                  onConfirm={remove}
                />
              </div>
            )}
          </>
        )}
      </section>

      {attached.map((policyName) => (
        <RolePolicy key={policyName} name={policyName} action={action} />
      ))}
    </>
  );
}

/**
 * One policy of the role, fetched by its name: whoever may read this policy
 * sees it, whatever else they may not list.
 */
function RolePolicy({ name, action }: { name: string; action: Action }) {
  const load = useCallback(() => unwrap(api.GET('/policies/{name}', { params: { path: { name } } })), [name]);
  const policy = useLoad(load);
  // Counts the saves, so that after one the editor shows what was stored.
  const [saved, setSaved] = useState(0);

  if (policy.data === undefined) {
    const missing = policy.error instanceof ApiError && policy.error.problem.status === 404;
    return (
      <section className="card card-body panel">
        <h2>Policy {name}</h2>
        {policy.loading ? <Loading /> : null}
        {policy.loading ? null : missing ? (
          <p className="mb-0">This policy does not exist: the role names it, and it gives nothing.</p>
        ) : (
          <ProblemNotice error={policy.error} />
        )}
      </section>
    );
  }
  return (
    <>
      {policy.data.built_in ? null : (
        <p className="text-secondary policy-shared">
          <i className="bi bi-info-circle" aria-hidden="true" /> Changing policy {name} changes it for every role that
          has it, not only this one.
        </p>
      )}
      <PolicyEditor
        key={`${name} ${String(saved)}`}
        policy={policy.data}
        action={action}
        onSaved={() => {
          setSaved((count) => count + 1);
          policy.reload();
        }}
      />
    </>
  );
}
