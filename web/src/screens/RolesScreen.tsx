import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import { routeHref } from '../router';
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

      <section className="card card-body panel" aria-labelledby="create-role">
        <h2 id="create-role">Create a role</h2>
        <form className="form-row" onSubmit={create}>
          <label className="field">
            <span>Role name</span>
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
          <label className="field field-wide">
            <span>Policies (separated by commas)</span>
            <input
              className="form-control"
              name="policies"
              autoCapitalize="none"
              spellCheck={false}
              value={policies}
              onChange={(event) => {
                setPolicies(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="btn btn-primary" disabled={action.busy} title="Create this role">
            <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Create role</span>
          </button>
        </form>
      </section>

      <section className="card card-body panel" aria-labelledby="role-list">
        <h2 id="role-list">All roles</h2>
        {roles.loading ? <Loading /> : null}
        {roles.data !== undefined && roles.data.length === 0 ? <p>There are no roles.</p> : null}
        {roles.data !== undefined && roles.data.length > 0 ? (
          <table className="table table-hover align-middle grid">
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
                  <td data-label="Name">
                    <a
                      className="btn btn-link-row"
                      href={routeHref(['roles', role.name])}
                      title="Open this role: its policies, and what they allow"
                    >
                      <i className="bi bi-person-badge" aria-hidden="true" />
                      {role.name}
                    </a>
                  </td>
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
                      <span className="d-flex flex-wrap gap-2">
                        <button
                          type="button"
                          className="btn btn-outline-secondary"
                          aria-label={`Edit role ${role.name}`}
                          onClick={() => {
                            setEditing(role.name);
                          }}
                          title="Change this"
                        >
                          <i className="bi bi-pencil" aria-hidden="true" /> <span className="btn-text">Edit</span>
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
