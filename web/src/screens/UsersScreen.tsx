import { useState, type FormEvent } from 'react';

import { api, unwrap } from '../api/client';
import { routeHref } from '../router';
import { formatTime, joinNames, parseNames } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadUsers() {
  return unwrap(api.GET('/users'));
}

/** The users: list them and create one. A user is changed on its own screen. */
export function UsersScreen() {
  const users = useLoad(loadUsers);
  const action = useAction();
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [roles, setRoles] = useState('');
  const [groups, setGroups] = useState('');

  const create = (event: FormEvent) => {
    event.preventDefault();
    void action.run(async () => {
      await unwrap(
        api.POST('/users', {
          body: {
            name,
            roles: parseNames(roles),
            groups: parseNames(groups),
            // Without a password the user cannot sign in to the console.
            password: password === '' ? undefined : password,
          },
        }),
      );
      setName('');
      setPassword('');
      setRoles('');
      setGroups('');
      users.reload();
    });
  };

  return (
    <>
      <h1>Users</h1>
      <ProblemNotice error={action.error ?? users.error} />

      <section className="card card-body panel" aria-labelledby="create-user">
        <h2 id="create-user">Create a user</h2>
        <form className="form-grid" onSubmit={create}>
          <label className="field">
            <span>User name</span>
            <input
              className="form-control"
              name="name"
              required
              autoCapitalize="none"
              spellCheck={false}
              autoComplete="off"
              value={name}
              onChange={(event) => {
                setName(event.target.value);
              }}
            />
          </label>
          <label className="field">
            <span>Console password (optional)</span>
            <input
              className="form-control"
              name="password"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(event) => {
                setPassword(event.target.value);
              }}
            />
          </label>
          <label className="field">
            <span>Roles (separated by commas)</span>
            <input
              className="form-control"
              name="roles"
              spellCheck={false}
              autoCapitalize="none"
              value={roles}
              onChange={(event) => {
                setRoles(event.target.value);
              }}
            />
          </label>
          <label className="field">
            <span>Groups (separated by commas)</span>
            <input
              className="form-control"
              name="groups"
              spellCheck={false}
              autoCapitalize="none"
              value={groups}
              onChange={(event) => {
                setGroups(event.target.value);
              }}
            />
          </label>
          <div className="form-actions d-flex flex-wrap gap-2">
            <button type="submit" className="btn btn-primary" disabled={action.busy} title="Create this user">
              <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Create user</span>
            </button>
          </div>
        </form>
      </section>

      <section className="card card-body panel" aria-labelledby="user-list">
        <h2 id="user-list">All users</h2>
        {users.loading ? <Loading /> : null}
        {users.data !== undefined && users.data.length === 0 ? <p>There are no users yet.</p> : null}
        {users.data !== undefined && users.data.length > 0 ? (
          <table className="table table-hover align-middle grid">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Roles</th>
                <th scope="col">Groups</th>
                <th scope="col">Status</th>
                <th scope="col">Console password</th>
                <th scope="col">Created</th>
              </tr>
            </thead>
            <tbody>
              {users.data.map((user) => (
                <tr key={user.name}>
                  <td data-label="Name">
                    <a
                      className="btn btn-link-row"
                      href={routeHref(['users', user.name])}
                      title="Open this user: roles, groups, password and API keys"
                    >
                      <i className="bi bi-person" aria-hidden="true" />
                      {user.name}
                    </a>
                  </td>
                  <td data-label="Roles">{joinNames(user.roles) || '-'}</td>
                  <td data-label="Groups">{joinNames(user.groups) || '-'}</td>
                  <td data-label="Status">{user.disabled ? 'Disabled' : 'Enabled'}</td>
                  <td data-label="Console password">{user.has_password ? 'Set' : 'Not set'}</td>
                  <td data-label="Created">{formatTime(user.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
      </section>
    </>
  );
}
