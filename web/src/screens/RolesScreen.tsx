import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime, joinNames, parseNames } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { NamesEditor } from '../ui/NamesEditor';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadRoles() {
  return unwrap(api.GET('/roles'));
}

/** The roles: a role is a named set of policies. Built-in roles are only shown. */
export function RolesScreen() {
  const roles = useLoad(loadRoles);
  const action = useAction();
  const [name, setName] = useState('');
  const [policies, setPolicies] = useState('');
  const [editing, setEditing] = useState<string | null>(null);

  const create = (event: FormEvent) => {
    event.preventDefault();
    void action.run(async () => {
      await unwrap(api.POST('/roles', { body: { name, policies: parseNames(policies) } }));
      setName('');
      setPolicies('');
      roles.reload();
    });
  };

  const save = (role: string, newPolicies: string[]) => {
    void action.run(async () => {
      await unwrap(
        api.PUT('/roles/{name}', { params: { path: { name: role } }, body: { policies: newPolicies } }),
      );
      setEditing(null);
      roles.reload();
    });
  };

  const remove = (role: string) => {
    void action.run(async () => {
      await unwrap(api.DELETE('/roles/{name}', { params: { path: { name: role } } }));
      roles.reload();
    });
  };

  return (
    <>
      <h1>Roles</h1>
      <ProblemNotice error={action.error ?? roles.error} />

      <section className="card" aria-labelledby="create-role">
        <h2 id="create-role">Create a role</h2>
        <form className="form-row" onSubmit={create}>
          <label className="field">
            <span>Role name</span>
            <input
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
          <label className="field field-wide">
            <span>Policies (separated by commas)</span>
            <input
              name="policies"
              autoCapitalize="none"
              spellCheck={false}
              value={policies}
              onChange={(event) => {
                setPolicies(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="button button-primary" disabled={action.busy}>
            Create role
          </button>
        </form>
      </section>

      <section className="card" aria-labelledby="role-list">
        <h2 id="role-list">All roles</h2>
        {roles.loading ? <Loading /> : null}
        {roles.data !== undefined && roles.data.length === 0 ? <p>There are no roles.</p> : null}
        {roles.data !== undefined && roles.data.length > 0 ? (
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Policies</th>
                <th scope="col">Kind</th>
                <th scope="col">Created</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {roles.data.map((role) => (
                <tr key={role.name}>
                  <td data-label="Name">{role.name}</td>
                  <td data-label="Policies">
                    {editing === role.name ? (
                      <NamesEditor
                        label={`Policies of role ${role.name}`}
                        names={role.policies}
                        busy={action.busy}
                        onSave={(names) => {
                          save(role.name, names);
                        }}
                        onCancel={() => {
                          setEditing(null);
                        }}
                      />
                    ) : (
                      joinNames(role.policies) || '-'
                    )}
                  </td>
                  <td data-label="Kind">{role.built_in ? 'Built-in' : 'Custom'}</td>
                  <td data-label="Created">{formatTime(role.created_at)}</td>
                  <td data-label="Actions">
                    {role.built_in ? (
                      'Read-only'
                    ) : (
                      <span className="actions">
                        <button
                          type="button"
                          className="button"
                          aria-label={`Edit role ${role.name}`}
                          onClick={() => {
                            setEditing(role.name);
                          }}
                        >
                          Edit
                        </button>
                        <ConfirmButton
                          label="Delete"
                          question={`Delete role ${role.name}?`}
                          disabled={action.busy}
                          onConfirm={() => {
                            remove(role.name);
                          }}
                        />
                      </span>
                    )}
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
