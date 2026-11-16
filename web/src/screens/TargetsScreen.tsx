import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';

import { api, ApiError, unwrap, type Target, type TargetAuthPut, type TargetPut, type TargetStatus } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime, joinNames, parseNames } from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { reasonOf } from '../ui/reason';
import { firstOf, LoadFailure, NO_SUCH_ENDPOINT, olderNode, onCode, refused, Said, type Explain } from '../ui/sections';

const TARGETS_ON_AN_OLDER_NODE = olderNode('Targets are');

type Kind = Target['kind'];
type AuthMode = TargetAuthPut['mode'];

/** The kinds of target, as the contract has them, in the words of the console. */
const KINDS: { kind: Kind; label: string }[] = [
  { kind: 'webhook', label: 'Webhook (HTTPS)' },
  { kind: 'nats', label: 'NATS JetStream' },
  { kind: 's3', label: 'CactusFS deployment (replication)' },
];

/**
 * The ways a deployment proves who it is to a target, and the kinds each is
 * for, as the contract describes them (`TargetAuth`): only those of the
 * kind chosen are offered. The API judges what is sent all the same.
 */
const AUTH_MODES: { mode: AuthMode; label: string; kinds: Kind[] }[] = [
  { mode: 'none', label: 'None', kinds: ['webhook', 'nats'] },
  { mode: 'bearer', label: 'Bearer token', kinds: ['webhook'] },
  { mode: 'token', label: 'Token', kinds: ['nats'] },
  { mode: 'basic', label: 'User and password', kinds: ['nats'] },
  { mode: 'sigv4', label: 'Access key', kinds: ['s3'] },
];

/** The ways of proving the contract offers a target of this kind, in the order of AUTH_MODES. */
function modesFor(kind: Kind): { mode: AuthMode; label: string }[] {
  return AUTH_MODES.filter((one) => one.kinds.includes(kind));
}

/** Whether a target of this kind may be sent the audit trail: a webhook or a NATS server, not another deployment. */
function takesAudit(kind: Kind): boolean {
  return kind !== 's3';
}

/** Whether a way of proving has a user or an access key ID beside its secret. */
function hasAuthId(mode: AuthMode): boolean {
  return mode === 'basic' || mode === 'sigv4';
}

/** What a receiver does with a request it cannot verify, as it is said beside the signing key and in the README. */
export const REFUSE_UNVERIFIED =
  'A request whose signature does not verify, or whose timestamp is more than five minutes from the receiver\'s clock, is refused with 401: the target is then held, and nothing is lost, until the key or the clock is put right. A receiver says 400 or 422 only of a message it will never take, which is passed.';

/** What the name of a target may be, as the contract has it (`TargetNameValue`). */
export const TARGET_NAME_RULE =
  '1 to 63 lower-case letters, digits and hyphens. It is part of the ARN the S3 API names it by: arn:cactusfs:notify:::<name>.';

/** What is said of the endpoint field. */
export const ENDPOINT_RULE =
  'The whole URL: https:// for a webhook or a deployment, tls:// for NATS; no user, query or fragment. It is written and never shown again whole - the console shows its scheme, host and port only, as its path may hold a token.';

/** What is said of the secret field: it is written and never read. */
export const SECRET_RULE =
  'Written, never shown - not here, not to you, not afterwards. Left empty, a change keeps the secret the target has while the way of proving stays the same.';

function loadTargets(signal: AbortSignal) {
  return unwrap(api.GET('/targets', { signal }));
}

function kindLabel(kind: Kind): string {
  return KINDS.find((one) => one.kind === kind)?.label ?? kind;
}

/** The signing key a definition or a change made, held until the reader says it was copied. */
interface ShownKey {
  target: string;
  key: string;
}

/**
 * The targets (ADR-0023, ADR-0025): the places outside the deployment it
 * sends notifications, the audit trail and replicas to. Each is listed with
 * its kind, the origin of its endpoint - never its path, never a secret -
 * and how delivery to it stands, as the node that leads tells it: its state,
 * what waits and the class of its last failure. Targets are defined, changed
 * and deleted here. Secrets are written and never shown; the signing key of
 * a webhook is shown once, in the answer that made it, and held in this
 * screen until the reader says it was copied. Who may do what is the API's
 * to say: a caller it does not show the targets is told so in a line, and a
 * change it refuses is shown in its words.
 */
export function TargetsScreen() {
  const targets = useLoad(loadTargets);
  const removal = useAction();
  const [open, setOpen] = useState<string | null>(null);
  const [said, setSaid] = useState<string | null>(null);
  const [shownKey, setShownKey] = useState<ShownKey | null>(null);
  // The target the last removal was of: what its error is said of.
  const [removing, setRemoving] = useState('');
  // Asked again, the status of every target is asked again too.
  const [round, setRound] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);

  if (targets.data === undefined && targets.loading) {
    return (
      <>
        <h1>Targets</h1>
        <Loading />
      </>
    );
  }
  if (targets.data === undefined && refused(targets.error)) {
    return (
      <>
        <h1>Targets</h1>
        <p className="bucket-facts-note">You may not see the targets.</p>
      </>
    );
  }

  const refresh = () => {
    setRound((value) => value + 1);
    targets.reload();
  };

  const saved = (name: string, signingKey: string | undefined, created: boolean) => {
    setSaid(created ? `Defined target ${name}.` : `Saved target ${name}.`);
    if (signingKey !== undefined && signingKey !== '') {
      setShownKey({ target: name, key: signingKey });
    }
    refresh();
  };

  const remove = (name: string) => {
    setRemoving(name);
    setSaid(null);
    void removal.run(async () => {
      try {
        await unwrap(api.DELETE('/targets/{name}', { params: { path: { name } } }));
        setOpen((shown) => (shown === name ? null : shown));
        setShownKey((shown) => (shown?.target === name ? null : shown));
        setSaid(`Deleted target ${name}. What waited to be sent to it is not sent.`);
        listRef.current?.focus();
      } finally {
        targets.reload();
      }
    });
  };

  // Closed, the panel hands the keyboard back to the button that opened it,
  // or to the list when that row is no longer there.
  const close = (name: string) => {
    setOpen(null);
    const opener = [...(listRef.current?.querySelectorAll<HTMLButtonElement>('button[data-target]') ?? [])].find(
      (button) => button.dataset.target === name,
    );
    (opener ?? listRef.current)?.focus();
  };

  const explainRemoval = firstOf(
    TARGETS_ON_AN_OLDER_NODE,
    onCode(
      'EntityInUse',
      `Target ${removing} cannot be deleted: the notification rules or the replication of a bucket name it. Take it out of them first.`,
    ),
    onCode('NoSuchEntity', `Target ${removing} is not there any more.`),
  );
  const list = targets.data;
  const opened = list?.find((target) => target.name === open);

  return (
    <>
      <h1>Targets</h1>
      <p className="form-hint">
        The places outside the deployment that it sends to: webhooks and NATS JetStream servers for the notifications of
        buckets and for the audit trail, and other deployments for replication. A bucket&apos;s notification rules name a
        target, never an address. What is sent leaves the deployment: object keys, sizes, entity tags, version IDs, the
        names of who made a change and when - and, to a target that is sent the audit trail, every record of it. Object
        contents, metadata, tags and secrets never.
      </p>
      <Said text={said} />
      {/* What is announced is that a key was made; the key itself is in no live region. */}
      <div className="notice-region" role="status">
        {shownKey === null ? null : (
          <p className="visually-hidden">A signing key was made for target {shownKey.target}. It is shown below, once.</p>
        )}
      </div>
      {shownKey === null ? null : (
        <SigningKeyNotice
          shown={shownKey}
          onDone={() => {
            setShownKey(null);
          }}
        />
      )}

      <section className="card card-body panel" aria-labelledby="targets-list">
        <h2 id="targets-list">Targets</h2>
        <div className="d-flex flex-wrap gap-2">
          <button
            type="button"
            className="btn btn-outline-secondary btn-sm"
            onClick={refresh}
            title="Ask again for the targets and how delivery to each stands"
          >
            <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Refresh</span>
          </button>
        </div>
        <div ref={listRef} role="region" aria-label="List of targets" tabIndex={-1}>
          {list === undefined ? null : list.length === 0 ? (
            <p>No target is defined.</p>
          ) : (
            <table className="table table-hover align-middle grid">
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Endpoint</th>
                  <th scope="col">Sent</th>
                  <th scope="col">State</th>
                  <th scope="col">Pending</th>
                  <th scope="col">Last error</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.map((target) => (
                  <tr key={target.name}>
                    <td data-label="Name">{target.name}</td>
                    <td data-label="Kind">{kindLabel(target.kind)}</td>
                    <td data-label="Endpoint">
                      <EndpointOf target={target} />
                    </td>
                    <td data-label="Sent">
                      <SentOf target={target} />
                    </td>
                    <StatusCells name={target.name} round={round} />
                    <td data-label="Actions">
                      <span className="d-flex flex-wrap gap-2">
                        <button
                          type="button"
                          className="btn btn-outline-secondary"
                          aria-label={`Edit target ${target.name}`}
                          data-target={target.name}
                          title="Change this target: its secrets are kept unless new ones are typed"
                          onClick={() => {
                            setSaid(null);
                            setOpen(target.name);
                          }}
                        >
                          <i className="bi bi-pencil" aria-hidden="true" /> <span className="btn-text">Edit</span>
                        </button>
                        <ConfirmButton
                          label="Delete"
                          question={`Delete target ${target.name}? What waits to be sent to it is not sent.`}
                          disabled={removal.busy}
                          onConfirm={() => {
                            remove(target.name);
                          }}
                        />
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
        {targets.error === null ? null : (
          <LoadFailure error={targets.error} explain={TARGETS_ON_AN_OLDER_NODE} onRetry={refresh} what="the targets" />
        )}
        <ProblemNotice error={removal.error} explain={explainRemoval} />
      </section>

      {opened === undefined ? null : (
        <TargetPanel
          // A fresh form for every target, and after every save.
          key={JSON.stringify(opened)}
          target={opened}
          onSaved={(signingKey) => {
            saved(opened.name, signingKey, false);
          }}
          onClose={() => {
            close(opened.name);
          }}
        />
      )}

      <DefineTarget
        taken={list?.map((target) => target.name) ?? []}
        onDefined={(name, signingKey) => {
          saved(name, signingKey, true);
        }}
      />
    </>
  );
}

/** The origin of a target's endpoint, as the API shows it, and whether there is a path it does not. */
function EndpointOf({ target }: { target: Target }) {
  return (
    <>
      <code>{target.endpoint}</code>
      {target.endpoint_path_set ? <span className="bucket-facts-note"> with a path, not shown</span> : null}
      {target.subject === undefined || target.subject === '' ? null : (
        <span className="bucket-facts-note">
          {' '}
          subject <code>{target.subject}</code>
        </span>
      )}
    </>
  );
}

/** What the target is sent: the buckets whose rules may name it, and the audit trail. */
function SentOf({ target }: { target: Target }) {
  const buckets =
    target.buckets.length === 0 ? null : (
      <span title="The buckets whose rules may name this target">buckets {joinNames(target.buckets)}</span>
    );
  if (buckets === null && !target.audit) {
    return <span className="bucket-facts-note">nothing yet</span>;
  }
  return (
    <>
      {buckets}
      {buckets !== null && target.audit ? '; ' : null}
      {target.audit ? 'the audit trail' : null}
    </>
  );
}

/** What a class of failure means, in the words of the console; the class itself is the API's. */
const CLASS_HINTS: Record<string, string> = {
  auth: 'The target does not take the credential (a 401 or 403, a token refused): nothing is passed until it is put right.',
  rejected: 'The target refused one message, which was passed: the others go on.',
  connect: 'The target could not be connected to: is the address right, and does the network let the nodes reach it?',
  timeout: 'The target did not answer in time.',
  tls: 'The handshake failed, or the certificate does not chain to what the target says it must.',
  redirect: 'The target answered with a redirect, which is never followed.',
  server: 'The target answered with a 5xx, a 408 or a 429, or with no status at all.',
  egress_refused: 'The deployment does not connect to that address (--egress-allow).',
  no_stream: 'No JetStream stream takes the subject: the stream is the operator of the NATS server to make.',
  internal: 'A failure on this side: the credential could not be opened, or an error without a class.',
};

/** The class of a state, for its look. */
function stateClass(state: string): string {
  switch (state) {
    case 'ok':
      return 'state state-online';
    case 'retrying':
      return 'state state-warning';
    case 'stalled':
      return 'state state-offline';
    default:
      return 'state';
  }
}

/**
 * How delivery to a target stands, as three cells of its row: the state, what
 * waits (with what was lost or refused, when anything was), and the class of
 * the last failure. Asked of the API for each target, and again when the
 * list is.
 */
function StatusCells({ name, round }: { name: string; round: number }) {
  const load = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/targets/{name}/status', { params: { path: { name } }, signal })),
    [name],
  );
  const status = useLoad(load);
  // A new round asks again; what was told stays on screen until the answer.
  const reload = status.reload;
  const asked = useRef(round);
  useEffect(() => {
    if (asked.current !== round) {
      asked.current = round;
      reload();
    }
  }, [round, reload]);
  const data: TargetStatus | undefined = status.data;

  if (data === undefined) {
    let words: string;
    let hint: string | undefined;
    if (status.loading) {
      words = 'asking…';
    } else if (refused(status.error)) {
      words = 'not visible to you';
    } else {
      words = 'unknown';
      hint = reasonOf(status.error, 'The request failed');
    }
    return (
      <>
        <td data-label="State">
          <span className="bucket-facts-note" title={hint}>
            {words}
          </span>
        </td>
        <td data-label="Pending">-</td>
        <td data-label="Last error">-</td>
      </>
    );
  }
  const errorClass = data.last_error_class;
  return (
    <>
      <td data-label="State">
        <span className={stateClass(data.state)}>{data.state}</span>
      </td>
      <td data-label="Pending">
        <span title={data.oldest_pending_at === undefined ? undefined : `The oldest since ${formatTime(data.oldest_pending_at)}`}>
          {data.pending.toLocaleString('en')}
        </span>
        {data.lost > 0 ? <span className="bucket-facts-note"> · {data.lost.toLocaleString('en')} lost</span> : null}
        {data.rejected > 0 ? (
          <span className="bucket-facts-note"> · {data.rejected.toLocaleString('en')} rejected</span>
        ) : null}
      </td>
      <td data-label="Last error">
        {errorClass === undefined || errorClass === '' ? (
          <span className="bucket-facts-note">none</span>
        ) : (
          <code title={CLASS_HINTS[errorClass]}>{errorClass}</code>
        )}
      </td>
    </>
  );
}

/** The signing key a definition or a change made: shown this once, copied, then put away. */
function SigningKeyNotice({ shown, onDone }: { shown: ShownKey; onDone: () => void }) {
  const [copied, setCopied] = useState<'copied' | 'failed' | null>(null);

  // A page that is not a secure context has no clipboard, and a browser may
  // refuse to write to it: either way the key is shown, and is selected by hand.
  const copy = () => {
    const clipboard = navigator.clipboard as Clipboard | undefined;
    if (clipboard === undefined) {
      setCopied('failed');
      return;
    }
    clipboard.writeText(shown.key).then(
      () => {
        setCopied('copied');
      },
      () => {
        setCopied('failed');
      },
    );
  };

  return (
    <div className="alert alert-success notice">
      <p className="notice-title">
        Copy the signing key of target {shown.target} now. It is shown this once and cannot be had again - only replaced.
      </p>
      <p>
        The receiver verifies every request with it: HMAC-SHA256, under this key, of the header{' '}
        <code>X-Cactusfs-Timestamp</code>, a full stop and the body as it arrived, compared in constant time with the
        hexadecimal after <code>v1=</code> in <code>X-Cactusfs-Signature</code>. {REFUSE_UNVERIFIED}
      </p>
      <dl className="facts">
        <dt>Signing key</dt>
        <dd>
          <code>{shown.key}</code>
        </dd>
      </dl>
      <div className="d-flex flex-wrap gap-2">
        <button type="button" className="btn btn-outline-secondary" onClick={copy} title="Copy the signing key to the clipboard">
          <i className="bi bi-clipboard" aria-hidden="true" /> Copy signing key
        </button>
        <button
          type="button"
          className="btn btn-primary"
          onClick={onDone}
          title="Hide the signing key: it cannot be shown again"
        >
          <i className="bi bi-check-lg" aria-hidden="true" /> I have copied it
        </button>
        <span role="status">
          {copied === 'copied' ? 'Copied.' : copied === 'failed' ? 'Could not copy: select the text and copy it.' : ''}
        </span>
      </div>
    </div>
  );
}

/** What the fields of a target hold, as they are typed. */
interface TargetFields {
  kind: Kind;
  endpoint: string;
  subject: string;
  region: string;
  caPem: string;
  buckets: string;
  audit: boolean;
  mode: AuthMode;
  authId: string;
  secret: string;
  rotate: boolean;
}

function fieldsOf(target: Target | undefined): TargetFields {
  return {
    kind: target?.kind ?? 'webhook',
    endpoint: '',
    subject: target?.subject ?? '',
    region: target?.region ?? '',
    caPem: target?.ca_pem ?? '',
    buckets: target === undefined ? '' : joinNames(target.buckets),
    audit: target?.audit ?? false,
    mode: target?.auth.mode ?? 'none',
    authId: target?.auth.id ?? '',
    secret: '',
    rotate: false,
  };
}

/**
 * The body of a definition or a change, as typed, with only the fields that
 * apply to the kind and the way of proving chosen - what a field that is not
 * shown holds is never sent. The endpoint and the secret are sent only when
 * typed - left out, a change keeps them - and everything else that applies
 * is sent whole, for the API to keep or refuse.
 */
function bodyOf(fields: TargetFields): TargetPut {
  const auth: TargetAuthPut = {
    mode: fields.mode,
    ...(hasAuthId(fields.mode) && fields.authId !== '' ? { id: fields.authId } : {}),
    ...(fields.mode !== 'none' && fields.secret !== '' ? { secret: fields.secret } : {}),
  };
  return {
    kind: fields.kind,
    ...(fields.endpoint === '' ? {} : { endpoint: fields.endpoint }),
    ...(fields.kind === 'nats' && fields.subject !== '' ? { subject: fields.subject } : {}),
    ...(fields.kind === 's3' && fields.region !== '' ? { region: fields.region } : {}),
    ...(fields.caPem.trim() === '' ? {} : { ca_pem: fields.caPem }),
    buckets: parseNames(fields.buckets),
    audit: takesAudit(fields.kind) && fields.audit,
    auth,
    rotate_signing_key: fields.kind === 'webhook' && fields.rotate,
  };
}

interface TargetFormProps {
  /** The target that is changed; undefined for one that is defined. */
  target?: Target;
  action: Action;
  explain: Explain;
  /** The name field, for a target that is defined. */
  name?: { value: string; onChange: (name: string) => void };
  /** What the save button says and its hint. */
  save: { label: string; title: string };
  /** Sends the body; the form keeps what was typed until it resolves true. */
  onSubmit: (body: TargetPut) => Promise<boolean>;
  /** More buttons, after the save button. */
  children?: ReactNode;
}

/** The fields of a target, typed and sent whole. Secret fields are written and never filled from an answer. */
function TargetForm({ target, action, explain, name, save, onSubmit, children }: TargetFormProps) {
  const nameHintId = useId();
  const endpointHintId = useId();
  const bucketsHintId = useId();
  const secretHintId = useId();
  const [fields, setFields] = useState(() => fieldsOf(target));
  const set = (change: Partial<TargetFields>) => {
    setFields((previous) => ({ ...previous, ...change }));
  };
  const keepsSecret = target !== undefined && target.secret_set && fields.mode === target.auth.mode;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    void onSubmit(bodyOf(fields)).then((done) => {
      if (done) {
        // The secret is not kept a moment longer than it is needed.
        set({ secret: '', rotate: false, endpoint: '' });
      }
    });
  };

  return (
    <form className="form-grid" onSubmit={submit} autoComplete="off">
      {name === undefined ? null : (
        <>
          <label className="field">
            <span>Name</span>
            <input
              className="form-control"
              name="name"
              required
              autoCapitalize="none"
              autoComplete="off"
              spellCheck={false}
              aria-describedby={nameHintId}
              disabled={action.busy}
              value={name.value}
              onChange={(event) => {
                name.onChange(event.target.value);
              }}
            />
          </label>
          <label className="field">
            <span>Kind</span>
            <select
              className="form-select"
              name="kind"
              disabled={action.busy}
              value={fields.kind}
              onChange={(event) => {
                const kind = event.target.value as Kind;
                // A way of proving the new kind has not is put back to its first.
                const modes = modesFor(kind);
                const mode = modes.some((one) => one.mode === fields.mode) ? fields.mode : (modes[0]?.mode ?? 'none');
                set({ kind, mode });
              }}
            >
              {KINDS.map((one) => (
                <option key={one.kind} value={one.kind}>
                  {one.label}
                </option>
              ))}
            </select>
          </label>
          <p id={nameHintId} className="form-hint field-full">
            {TARGET_NAME_RULE} The kind cannot be changed afterwards.
          </p>
        </>
      )}
      <label className="field field-full">
        <span>{target === undefined ? 'Endpoint' : 'New endpoint (optional)'}</span>
        <input
          className="form-control"
          name="endpoint"
          required={target === undefined}
          inputMode="url"
          autoCapitalize="none"
          autoComplete="off"
          spellCheck={false}
          placeholder={
            target === undefined
              ? fields.kind === 'nats'
                ? 'tls://nats.example.com:4222'
                : 'https://hooks.example.com/cactusfs'
              : `kept: ${target.endpoint}${target.endpoint_path_set ? ' with its path' : ''}`
          }
          aria-describedby={endpointHintId}
          disabled={action.busy}
          value={fields.endpoint}
          onChange={(event) => {
            set({ endpoint: event.target.value });
          }}
        />
      </label>
      <p id={endpointHintId} className="form-hint field-full">
        {ENDPOINT_RULE}
        {target === undefined ? '' : ' Left empty, the endpoint is kept, path and all; to change any part of it, type the whole URL.'}
      </p>
      {fields.kind === 'nats' ? (
        <label className="field">
          <span>Subject</span>
          <input
            className="form-control"
            name="subject"
            autoCapitalize="none"
            spellCheck={false}
            disabled={action.busy}
            value={fields.subject}
            onChange={(event) => {
              set({ subject: event.target.value });
            }}
          />
        </label>
      ) : null}
      {fields.kind === 's3' ? (
        <label className="field">
          <span>Region</span>
          <input
            className="form-control"
            name="region"
            autoCapitalize="none"
            spellCheck={false}
            disabled={action.busy}
            value={fields.region}
            onChange={(event) => {
              set({ region: event.target.value });
            }}
          />
        </label>
      ) : null}
      <label className="field field-full">
        <span>Buckets that may use it</span>
        <input
          className="form-control"
          name="buckets"
          autoCapitalize="none"
          spellCheck={false}
          placeholder="none"
          aria-describedby={bucketsHintId}
          disabled={action.busy}
          value={fields.buckets}
          onChange={(event) => {
            set({ buckets: event.target.value });
          }}
        />
      </label>
      <p id={bucketsHintId} className="form-hint field-full">
        Names or patterns with *, separated by commas: the buckets whose notification rules may name this target. * is
        every bucket; none, no bucket - a target that is sent the audit trail only.
      </p>
      {takesAudit(fields.kind) ? (
        <label className="check field-full">
          <input
            className="form-check-input"
            type="checkbox"
            name="audit"
            checked={fields.audit}
            disabled={action.busy}
            onChange={(event) => {
              set({ audit: event.target.checked });
            }}
          />{' '}
          Send it every record of the audit trail as well, from the first
        </label>
      ) : null}
      <label className="field field-full">
        <span>CA certificates (PEM, optional)</span>
        <textarea
          className="form-control"
          name="ca_pem"
          rows={4}
          spellCheck={false}
          placeholder="Empty: the system's certificate authorities"
          disabled={action.busy}
          value={fields.caPem}
          onChange={(event) => {
            set({ caPem: event.target.value });
          }}
        />
      </label>
      <label className="field">
        <span>Credential</span>
        <select
          className="form-select"
          name="auth_mode"
          disabled={action.busy}
          value={fields.mode}
          onChange={(event) => {
            set({ mode: event.target.value as AuthMode });
          }}
        >
          {modesFor(fields.kind).map((one) => (
            <option key={one.mode} value={one.mode}>
              {one.label}
            </option>
          ))}
        </select>
      </label>
      {hasAuthId(fields.mode) ? (
        <label className="field">
          <span>{fields.mode === 'basic' ? 'User' : 'Access key ID'}</span>
          <input
            className="form-control"
            name="auth_id"
            autoCapitalize="none"
            autoComplete="off"
            spellCheck={false}
            disabled={action.busy}
            value={fields.authId}
            onChange={(event) => {
              set({ authId: event.target.value });
            }}
          />
        </label>
      ) : null}
      {fields.mode === 'none' ? null : (
        <>
          <label className="field">
            <span>Secret</span>
            <input
              className="form-control"
              name="secret"
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              placeholder={keepsSecret ? 'set - left empty, it is kept' : ''}
              aria-describedby={secretHintId}
              disabled={action.busy}
              value={fields.secret}
              onChange={(event) => {
                set({ secret: event.target.value });
              }}
            />
          </label>
          <p id={secretHintId} className="form-hint field-full">
            {SECRET_RULE}
          </p>
        </>
      )}
      {target !== undefined && target.kind === 'webhook' ? (
        <label className="check field-full">
          <input
            className="form-check-input"
            type="checkbox"
            name="rotate_signing_key"
            checked={fields.rotate}
            disabled={action.busy}
            onChange={(event) => {
              set({ rotate: event.target.checked });
            }}
          />{' '}
          Make a new signing key: it is shown once, and the old one stops being used with the next request
        </label>
      ) : null}
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button type="submit" className="btn btn-primary" disabled={action.busy} title={save.title}>
          <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">{save.label}</span>
        </button>
        {children}
      </div>
    </form>
  );
}

/** What a refusal of a definition or a change is said to be. */
function explainPut(name: string, defining: boolean): Explain {
  return firstOf(
    TARGETS_ON_AN_OLDER_NODE,
    onCode(
      'InvalidArgument',
      defining ? `Target ${name} was refused, and nothing was defined.` : `The change was refused: target ${name} is as it was.`,
    ),
    onCode('TooManyRequests', 'The server could not take the request now. Nothing was changed: send it again in a moment.'),
  );
}

interface TargetPanelProps {
  target: Target;
  /** Called with the signing key the change made, if it made one. */
  onSaved: (signingKey: string | undefined) => void;
  onClose: () => void;
}

/**
 * One target, changed: its fields as the API shows them, the secret and the
 * path of the endpoint left empty - they are never shown - and kept unless
 * typed again. Opened, it takes the keyboard to its heading and is scrolled
 * into view.
 */
function TargetPanel({ target, onSaved, onClose }: TargetPanelProps) {
  const headingId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  const action = useAction();
  useEffect(() => {
    const shown = heading.current;
    if (shown === null) {
      return;
    }
    shown.focus();
    // Not every engine has it (jsdom has not); focus alone scrolls in a browser.
    if (typeof shown.scrollIntoView === 'function') {
      shown.scrollIntoView({ block: 'nearest' });
    }
  }, []);

  const submit = (body: TargetPut) => {
    let signingKey: string | undefined;
    return action
      .run(async () => {
        const answer = await unwrap(api.PUT('/targets/{name}', { params: { path: { name: target.name } }, body }));
        signingKey = answer.signing_key;
      })
      .then((done) => {
        if (done) {
          onSaved(signingKey);
        }
        return done;
      });
  };

  return (
    <section className="card card-body panel" aria-labelledby={headingId}>
      <h2 id={headingId} ref={heading} tabIndex={-1}>
        Target {target.name}
      </h2>
      <p className="form-hint">
        {kindLabel(target.kind)}, defined {formatTime(target.created_at)}, changed {formatTime(target.updated_at)}.{' '}
        {target.secret_set ? 'It has a secret. ' : ''}
        {target.signing_key_set ? 'It has a signing key. ' : ''}
        {target.references === 1
          ? 'One bucket configuration names it.'
          : `${target.references.toLocaleString('en')} bucket configurations name it.`}
      </p>
      <TargetForm
        target={target}
        action={action}
        explain={explainPut(target.name, false)}
        save={{ label: 'Save target', title: 'Save this target: a secret left empty is kept' }}
        onSubmit={submit}
      >
        <button type="button" className="btn btn-outline-secondary" onClick={onClose} title="Close this target">
          <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close</span>
        </button>
      </TargetForm>
    </section>
  );
}

/**
 * Whether the API has a target of this name now: a 404 that says there is no
 * such target is "no"; a 404 of an older node, and any other failure, is
 * thrown, and nothing is defined.
 */
async function exists(name: string): Promise<boolean> {
  try {
    await unwrap(api.GET('/targets/{name}', { params: { path: { name } } }));
    return true;
  } catch (error) {
    if (error instanceof ApiError && error.problem.status === 404 && error.problem.code !== NO_SUCH_ENDPOINT) {
      return false;
    }
    throw error;
  }
}

interface DefineTargetProps {
  /** The names of the targets listed: a name among them is not defined again from here. */
  taken: string[];
  /** Called with the name and the signing key the definition made, if it made one. */
  onDefined: (name: string, signingKey: string | undefined) => void;
}

/** Defines a target: a name, a kind, an endpoint, who may use it and how the deployment proves who it is. */
function DefineTarget({ taken, onDefined }: DefineTargetProps) {
  const headingId = useId();
  const action = useAction();
  const [name, setName] = useState('');
  // The name the last definition was of: what its error is said of.
  const [defining, setDefining] = useState('');
  // A fresh form once a target was defined.
  const [round, setRound] = useState(0);

  const submit = (body: TargetPut) => {
    setDefining(name);
    // A PUT of a name that is taken changes that target: it is changed from
    // its own panel, where what it has is shown.
    const takenAlready = () => new Error(`Target ${name} is defined already: edit it in the list.`);
    if (taken.includes(name)) {
      action.fail(takenAlready());
      return Promise.resolve(false);
    }
    let signingKey: string | undefined;
    const defined = name;
    return action
      .run(async () => {
        // The list may be older than another administrator's definition: the
        // API is asked again just before. (The contract has no precondition
        // for a PUT, so a definition in the moment between is not caught.)
        if (await exists(defined)) {
          throw takenAlready();
        }
        const answer = await unwrap(api.PUT('/targets/{name}', { params: { path: { name: defined } }, body }));
        signingKey = answer.signing_key;
      })
      .then((done) => {
        if (done) {
          setName('');
          setRound((value) => value + 1);
          onDefined(defined, signingKey);
        }
        return done;
      });
  };

  return (
    <section className="card card-body panel" aria-labelledby={headingId}>
      <h2 id={headingId}>Define a target</h2>
      <p className="form-hint">
        Nothing is connected to when a target is defined: whether it can be reached is told by its state above. In
        Kubernetes the chart&apos;s NetworkPolicy must let the nodes reach its address (networkPolicy.egressTo).
      </p>
      <TargetForm
        key={round}
        action={action}
        explain={explainPut(defining, true)}
        name={{ value: name, onChange: setName }}
        save={{ label: 'Define target', title: 'Define this target: a webhook is given a signing key, shown once' }}
        onSubmit={submit}
      />
    </section>
  );
}
