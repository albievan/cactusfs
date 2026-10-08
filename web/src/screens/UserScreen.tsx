import { useCallback, useState, type FormEvent } from 'react';

import { api, unwrap, type User } from '../api/client';
import { navigate, routeHref } from '../router';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime, joinNames, parseNames } from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { ApiKeys } from './ApiKeys';

interface UserScreenProps {
  /** The name of the user. */
  name: string;
}

/** One user: change roles, groups, status and password, delete, and manage API keys. */
export function UserScreen({ name }: UserScreenProps) {
  const load = useCallback(() => unwrap(api.GET('/users/{name}', { params: { path: { name } } })), [name]);
  const user = useLoad(load);
  const action = useAction();

  const remove = () => {
    void action.run(async () => {
      await unwrap(api.DELETE('/users/{name}', { params: { path: { name } } }));
      navigate(routeHref(['users']));
    });
  };

  return (
    <>
      <h1>User {name}</h1>
      <p>
        <a href={routeHref(['users'])}>All users</a>
      </p>
      <ProblemNotice error={action.error ?? user.error} />

      <section className="card" aria-labelledby="edit-user">
        <h2 id="edit-user">Settings</h2>
        {user.loading ? <Loading /> : null}
        {user.data === undefined ? null : (
          <>
            <UserForm user={user.data} action={action} />
            <div className="danger-zone">
              <ConfirmButton
                label="Delete user"
                question={`Delete user ${name}?`}
                disabled={action.busy}
                onConfirm={remove}
              />
            </div>
          </>
        )}
      </section>

      <ApiKeys user={name} />
    </>
  );
}

function UserForm({ user, action }: { user: User; action: Action }) {
  const [saved, setSaved] = useState(user);
  const [roles, setRoles] = useState(joinNames(user.roles));
  const [groups, setGroups] = useState(joinNames(user.groups));
  const [disabled, setDisabled] = useState(user.disabled);
  const [password, setPassword] = useState('');
  const [done, setDone] = useState(false);

  const save = (event: FormEvent) => {
    event.preventDefault();
    setDone(false);
    void action.run(async () => {
      const changed = await unwrap(
        api.PUT('/users/{name}', {
          params: { path: { name: user.name } },
          body: {
            roles: parseNames(roles),
            groups: parseNames(groups),
            disabled,
            // An empty field leaves the password as it is.
            password: password === '' ? undefined : password,
          },
        }),
      );
      setSaved(changed);
      setRoles(joinNames(changed.roles));
      setGroups(joinNames(changed.groups));
      setDisabled(changed.disabled);
      setPassword('');
      setDone(true);
    });
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <label className="field">
        <span>Roles (separated by commas)</span>
        <input
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
          name="groups"
          spellCheck={false}
          autoCapitalize="none"
          value={groups}
          onChange={(event) => {
            setGroups(event.target.value);
          }}
        />
      </label>
      <label className="field">
        <span>New console password (leave empty to keep the current one)</span>
        <input
          name="password"
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(event) => {
            setPassword(event.target.value);
          }}
        />
      </label>
      <label className="check">
        <input
          name="disabled"
          type="checkbox"
          checked={disabled}
          onChange={(event) => {
            setDisabled(event.target.checked);
          }}
        />
        <span>Disabled</span>
      </label>
      <dl className="facts">
        <dt>Console password</dt>
        <dd>{saved.has_password ? 'Set' : 'Not set'}</dd>
        <dt>Created</dt>
        <dd>{formatTime(saved.created_at)}</dd>
      </dl>
      <div className="form-actions">
        <button type="submit" className="button button-primary" disabled={action.busy}>
          Save changes
        </button>
        <span role="status">{done ? 'Saved.' : ''}</span>
      </div>
    </form>
  );
}
