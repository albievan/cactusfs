import { useCallback, useState, type FormEvent } from 'react';

import { api, unwrap, type APIKeyCreated } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatPolicyDocument, formatTime, parsePolicyDocument } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

interface ApiKeysProps {
  /** The user that owns the keys. */
  user: string;
}

/**
 * The API keys of one user: list, create, revoke. The secret of a new key is
 * in the answer to its creation and nowhere else, so it is held in the state
 * of this component only until the reader says it has been copied.
 */
export function ApiKeys({ user }: ApiKeysProps) {
  const load = useCallback(
    () => unwrap(api.GET('/users/{name}/keys', { params: { path: { name: user } } })),
    [user],
  );
  const keys = useLoad(load);
  const action = useAction();
  const [description, setDescription] = useState('');
  const [expires, setExpires] = useState('');
  const [policy, setPolicy] = useState('');
  const [created, setCreated] = useState<APIKeyCreated | null>(null);

  const create = (event: FormEvent) => {
    event.preventDefault();
    let inlinePolicy: Record<string, unknown> | undefined;
    try {
      inlinePolicy = policy.trim() === '' ? undefined : parsePolicyDocument(policy);
    } catch (error) {
      action.fail(error);
      return;
    }
    void action.run(async () => {
      const answer = await unwrap(
        api.POST('/users/{name}/keys', {
          params: { path: { name: user } },
          body: {
            description: description === '' ? undefined : description,
            // The field holds local time; the API wants an instant.
            expires_at: expires === '' ? undefined : new Date(expires).toISOString(),
            inline_policy: inlinePolicy,
          },
        }),
      );
      setCreated(answer);
      setDescription('');
      setExpires('');
      setPolicy('');
      keys.reload();
    });
  };

  const revoke = (accessKeyId: string) => {
    void action.run(async () => {
      await unwrap(
        api.DELETE('/keys/{access_key_id}', { params: { path: { access_key_id: accessKeyId } } }),
      );
      keys.reload();
    });
  };

  return (
    <section className="card card-body panel" aria-labelledby="api-keys">
      <h2 id="api-keys">API keys</h2>
      <ProblemNotice error={action.error ?? keys.error} />

      {created === null ? null : (
        <NewKeyNotice
          created={created}
          onDone={() => {
            setCreated(null);
          }}
        />
      )}

      <h3>Create an API key</h3>
      <form className="form-grid" onSubmit={create}>
        <label className="field">
          <span>Description (optional)</span>
          <input
            className="form-control"
            name="description"
            value={description}
            onChange={(event) => {
              setDescription(event.target.value);
            }}
          />
        </label>
        <label className="field">
          <span>Expires (optional)</span>
          <input
            className="form-control"
            name="expires"
            type="datetime-local"
            value={expires}
            onChange={(event) => {
              setExpires(event.target.value);
            }}
          />
        </label>
        <label className="field field-full">
          <span>Inline policy as JSON (optional; narrows what the key may do)</span>
          <textarea
            className="form-control"
            name="policy"
            rows={6}
            spellCheck={false}
            value={policy}
            onChange={(event) => {
              setPolicy(event.target.value);
            }}
          />
        </label>
        <div className="form-actions d-flex flex-wrap gap-2">
          <button type="submit" className="btn btn-primary" disabled={action.busy}>
            Create API key
          </button>
        </div>
      </form>

      <h3>Keys of {user}</h3>
      {keys.loading ? <Loading /> : null}
      {keys.data !== undefined && keys.data.length === 0 ? <p>This user has no API keys.</p> : null}
      {keys.data !== undefined && keys.data.length > 0 ? (
        <table className="table table-hover align-middle grid">
          <thead>
            <tr>
              <th scope="col">Access key ID</th>
              <th scope="col">Description</th>
              <th scope="col">Created</th>
              <th scope="col">Expires</th>
              <th scope="col">Last used</th>
              <th scope="col">Inline policy</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {keys.data.map((key) => (
              <tr key={key.access_key_id}>
                <td data-label="Access key ID">
                  <code>{key.access_key_id}</code>
                </td>
                <td data-label="Description">{key.description || '-'}</td>
                <td data-label="Created">{formatTime(key.created_at)}</td>
                <td data-label="Expires">{key.expires_at === undefined ? 'Never' : formatTime(key.expires_at)}</td>
                <td data-label="Last used">
                  {key.last_used_at === undefined ? 'Never' : formatTime(key.last_used_at)}
                </td>
                <td data-label="Inline policy">
                  {key.inline_policy === undefined ? (
                    'None'
                  ) : (
                    <details>
                      <summary>Show</summary>
                      <pre className="code">{formatPolicyDocument(key.inline_policy)}</pre>
                    </details>
                  )}
                </td>
                <td data-label="Actions">
                  <ConfirmButton
                    label="Revoke"
                    question={`Revoke key ${key.access_key_id}?`}
                    disabled={action.busy}
                    onConfirm={() => {
                      revoke(key.access_key_id);
                    }}
                  />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </section>
  );
}

function NewKeyNotice({ created, onDone }: { created: APIKeyCreated; onDone: () => void }) {
  const [copied, setCopied] = useState(false);

  const copy = () => {
    void navigator.clipboard.writeText(created.secret_key).then(() => {
      setCopied(true);
    });
  };

  return (
    <div className="alert alert-success notice" role="status">
      <p className="notice-title">Copy the secret key now. It is shown this once and cannot be had again.</p>
      <dl className="facts">
        <dt>Access key ID</dt>
        <dd>
          <code>{created.key.access_key_id}</code>
        </dd>
        <dt>Secret key</dt>
        <dd>
          <code>{created.secret_key}</code>
        </dd>
      </dl>
      <div className="d-flex flex-wrap gap-2">
        <button type="button" className="btn btn-outline-secondary" onClick={copy}>
          Copy secret key
        </button>
        <button type="button" className="btn btn-primary" onClick={onDone}>
          I have copied it
        </button>
        <span role="status">{copied ? 'Copied.' : ''}</span>
      </div>
    </div>
  );
}
