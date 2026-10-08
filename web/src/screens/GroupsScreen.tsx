import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime, joinNames, parseNames } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { NamesEditor } from '../ui/NamesEditor';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadGroups() {
  return unwrap(api.GET('/groups'));
}

/** The groups: a group gives its roles to every user in it. */
export function GroupsScreen() {
  const groups = useLoad(loadGroups);
  const action = useAction();
  const [name, setName] = useState('');
  const [roles, setRoles] = useState('');
  const [editing, setEditing] = useState<string | null>(null);

  const create = (event: FormEvent) => {
    event.preventDefault();
    void action.run(async () => {
      await unwrap(api.POST('/groups', { body: { name, roles: parseNames(roles) } }));
      setName('');
      setRoles('');
      groups.reload();
    });
  };

  const save = (group: string, newRoles: string[]) => {
    void action.run(async () => {
      await unwrap(
        api.PUT('/groups/{name}', { params: { path: { name: group } }, body: { roles: newRoles } }),
      );
      setEditing(null);
      groups.reload();
    });
  };

  const remove = (group: string) => {
    void action.run(async () => {
      await unwrap(api.DELETE('/groups/{name}', { params: { path: { name: group } } }));
      groups.reload();
    });
  };

  return (
    <>
      <h1>Groups</h1>
      <ProblemNotice error={action.error ?? groups.error} />

      <section className="card" aria-labelledby="create-group">
        <h2 id="create-group">Create a group</h2>
        <form className="form-row" onSubmit={create}>
          <label className="field">
            <span>Group name</span>
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
            <span>Roles (separated by commas)</span>
            <input
              name="roles"
              autoCapitalize="none"
              spellCheck={false}
              value={roles}
              onChange={(event) => {
                setRoles(event.target.value);
              }}
            />
          </label>
          <button type="submit" className="button button-primary" disabled={action.busy}>
            Create group
          </button>
        </form>
      </section>

      <section className="card" aria-labelledby="group-list">
        <h2 id="group-list">All groups</h2>
        {groups.loading ? <Loading /> : null}
        {groups.data !== undefined && groups.data.length === 0 ? <p>There are no groups yet.</p> : null}
        {groups.data !== undefined && groups.data.length > 0 ? (
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Roles</th>
                <th scope="col">Created</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {groups.data.map((group) => (
                <tr key={group.name}>
                  <td data-label="Name">{group.name}</td>
                  <td data-label="Roles">
                    {editing === group.name ? (
                      <NamesEditor
                        label={`Roles of group ${group.name}`}
                        names={group.roles}
                        busy={action.busy}
                        onSave={(names) => {
                          save(group.name, names);
                        }}
                        onCancel={() => {
                          setEditing(null);
                        }}
                      />
                    ) : (
                      joinNames(group.roles) || '-'
                    )}
                  </td>
                  <td data-label="Created">{formatTime(group.created_at)}</td>
                  <td data-label="Actions">
                    <span className="actions">
                      <button
                        type="button"
                        className="button"
                        aria-label={`Edit group ${group.name}`}
                        onClick={() => {
                          setEditing(group.name);
                        }}
                      >
                        Edit
                      </button>
                      <ConfirmButton
                        label="Delete"
                        question={`Delete group ${group.name}?`}
                        disabled={action.busy}
                        onConfirm={() => {
                          remove(group.name);
                        }}
                      />
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
