import { useCallback, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';

import { api, unwrap, type NotificationRule, type NotificationRulePut } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { firstOf, Fold, LoadFailure, olderNode, onCode, refused, Said, useFocusAfterChange, type Explain } from '../ui/sections';

const NOTIFICATIONS_ON_AN_OLDER_NODE = olderNode('Notifications are');

type Change = NotificationRule['events'][number];

/** The kinds of change a rule is for, as the contract has them, with the name S3 gives each. */
export const EVENTS: { event: Change; label: string; s3: string }[] = [
  { event: 'put', label: 'Put or copied', s3: 's3:ObjectCreated:Put' },
  { event: 'complete', label: 'Multipart upload completed', s3: 's3:ObjectCreated:CompleteMultipartUpload' },
  { event: 'marker', label: 'Deleted, leaving a delete marker', s3: 's3:ObjectRemoved:DeleteMarkerCreated' },
  { event: 'delete', label: 'A version removed for good', s3: 's3:ObjectRemoved:Delete' },
];

/** What is said once rules were saved. */
export const RULES_SAVED =
  'Saved. From now on every change a rule matches is sent to its target, in the order the changes are made; nothing of before is sent.';

function eventLabel(event: Change): string {
  return EVENTS.find((one) => one.event === event)?.label ?? event;
}

/** A rule as it is sent again: what the API answered, in the shape it takes. */
function putOf(rule: NotificationRule): NotificationRulePut {
  return {
    id: rule.id,
    target: rule.target,
    events: rule.events,
    ...(rule.prefix === undefined || rule.prefix === '' ? {} : { prefix: rule.prefix }),
    ...(rule.suffix === undefined || rule.suffix === '' ? {} : { suffix: rule.suffix }),
    as: rule.as,
  };
}

/** Which keys a rule is for, in words. */
function keysOf(rule: NotificationRule): string {
  const prefix = rule.prefix ?? '';
  const suffix = rule.suffix ?? '';
  if (prefix === '' && suffix === '') {
    return 'every key';
  }
  if (suffix === '') {
    return `beginning with ${prefix}`;
  }
  if (prefix === '') {
    return `ending with ${suffix}`;
  }
  return `beginning with ${prefix} and ending with ${suffix}`;
}

/**
 * The notification rules of a bucket (ADR-0025), folded until they are asked
 * for: which changes, of which keys, are told to which target. A rule names a
 * target, never an address; the targets are defined on the Targets screen.
 * Rules are added and removed here, each change sending the whole list - as
 * the API has it just before - for the API to keep or refuse; what it
 * answers is what is shown after.
 */
export function BucketNotificationsSection({ bucket }: { bucket: string }) {
  return (
    <Fold title="Notifications" what="the notification rules of this bucket">
      <NotificationsBody bucket={bucket} />
    </Fold>
  );
}

function NotificationsBody({ bucket }: { bucket: string }) {
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/notifications', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const rules = useLoad(load);
  const action = useAction();
  // The rules as the API answered the last change with them: as they are kept.
  const [kept, setKept] = useState<NotificationRule[] | undefined>(undefined);
  const [said, setSaid] = useState<string | null>(null);
  // A fresh form once a rule was added.
  const [round, setRound] = useState(0);
  const formAreaRef = useRef<HTMLDivElement>(null);
  const focusSaveRef = useRef(false);
  useFocusAfterChange(formAreaRef, focusSaveRef);
  const again = () => {
    setKept(undefined);
    setSaid(null);
    rules.reload();
  };

  if (rules.data === undefined && rules.loading) {
    return <Loading />;
  }
  if (rules.data === undefined && refused(rules.error)) {
    // Not a failure: rules are replaced as a whole, and a list the caller
    // cannot see is not one to change from here.
    return <p className="bucket-facts-note">You may not see the notification rules of this bucket.</p>;
  }
  if (rules.data === undefined) {
    return (
      <LoadFailure
        error={rules.error}
        explain={NOTIFICATIONS_ON_AN_OLDER_NODE}
        onRetry={again}
        what="the notification rules of this bucket"
      />
    );
  }

  const shown = kept ?? rules.data.rules;
  const explain = firstOf(
    NOTIFICATIONS_ON_AN_OLDER_NODE,
    onCode('InvalidArgument', `The rules were refused: bucket ${bucket} keeps the rules it had.`),
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
  );

  const put = async (list: NotificationRulePut[], words: string) => {
    setSaid(null);
    const answer = await unwrap(
      api.PUT('/buckets/{bucket}/notifications', { params: { path: { bucket } }, body: { rules: list } }),
    );
    focusSaveRef.current = true;
    setKept(answer.rules);
    setSaid(words);
  };
  // The rules as the API has them now: a change is made to them, not to
  // what this screen read a while ago, so that a rule another administrator
  // added or removed meanwhile is neither lost nor put back. (The contract has
  // no precondition for the PUT: a change in the moment between is not caught.)
  const current = async () =>
    (await unwrap(api.GET('/buckets/{bucket}/notifications', { params: { path: { bucket } } }))).rules;
  const add = async (rule: NotificationRulePut) => {
    const now = await current();
    await put([...now.map(putOf), rule], RULES_SAVED);
    setRound((value) => value + 1);
  };
  const drop = (id: string) => {
    void action.run(async () => {
      const now = await current();
      await put(
        now.filter((rule) => rule.id !== id).map(putOf),
        `Removed rule ${id}. What it matched before is still sent; nothing after.`,
      );
    });
  };
  const removeAll = () => {
    void action.run(async () => {
      setSaid(null);
      await unwrap(api.DELETE('/buckets/{bucket}/notifications', { params: { path: { bucket } } }));
      focusSaveRef.current = true;
      setKept([]);
      setSaid(`Removed every notification rule of bucket ${bucket}.`);
    });
  };

  return (
    <>
      {rules.error === null ? null : (
        <LoadFailure
          error={rules.error}
          explain={NOTIFICATIONS_ON_AN_OLDER_NODE}
          onRetry={again}
          what="the notification rules of this bucket"
        />
      )}
      <Said text={said} />
      {shown.length === 0 ? (
        <p className="bucket-facts-note">This bucket has no notification rules: no change of it is sent anywhere.</p>
      ) : (
        <table className="table table-hover align-middle grid">
          <thead>
            <tr>
              <th scope="col">Rule</th>
              <th scope="col">Target</th>
              <th scope="col">Changes</th>
              <th scope="col">Keys</th>
              <th scope="col">In S3</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((rule) => (
              <tr key={rule.id}>
                <td data-label="Rule">{rule.id}</td>
                <td data-label="Target">{rule.target}</td>
                <td data-label="Changes">{rule.events.map(eventLabel).join('; ')}</td>
                <td data-label="Keys">{keysOf(rule)}</td>
                <td data-label="In S3">{rule.as === 'topic' ? 'Topic' : 'Queue'}</td>
                <td data-label="Actions">
                  <ConfirmButton
                    label="Remove"
                    question={`Remove rule ${rule.id}?`}
                    disabled={action.busy}
                    onConfirm={() => {
                      drop(rule.id);
                    }}
                  />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div ref={formAreaRef}>
        <AddRule key={round} bucket={bucket} action={action} explain={explain} onAdd={add}>
          {shown.length === 0 ? null : (
            <ConfirmButton
              label="Remove every rule"
              question={`Remove every notification rule of bucket ${bucket}?`}
              disabled={action.busy}
              onConfirm={removeAll}
            />
          )}
        </AddRule>
      </div>
    </>
  );
}

function loadTargetNames(signal: AbortSignal) {
  return unwrap(api.GET('/targets', { signal })).then((targets) => targets.map((target) => target.name));
}

interface AddRuleProps {
  bucket: string;
  action: Action;
  explain: Explain;
  onAdd: (rule: NotificationRulePut) => Promise<void>;
  children?: ReactNode;
}

/**
 * A rule, typed and added to the rules the bucket has. The target is a name;
 * the names of the targets are offered when the caller may list them, and
 * which of them this bucket may use is the API's to say.
 */
function AddRule({ bucket, action, explain, onAdd, children }: AddRuleProps) {
  const targetsId = useId();
  const keysHintId = useId();
  const targets = useLoad(loadTargetNames);
  const [id, setId] = useState('');
  const [target, setTarget] = useState('');
  const [events, setEvents] = useState<Change[]>(['put', 'complete']);
  const [prefix, setPrefix] = useState('');
  const [suffix, setSuffix] = useState('');
  const [as, setAs] = useState<NotificationRule['as']>('queue');

  const toggle = (event: Change, on: boolean) => {
    setEvents((previous) =>
      on ? EVENTS.map((one) => one.event).filter((one) => one === event || previous.includes(one)) : previous.filter((one) => one !== event),
    );
  };
  const submit = (event: FormEvent) => {
    event.preventDefault();
    void action.run(() =>
      onAdd({
        id,
        target,
        events,
        ...(prefix === '' ? {} : { prefix }),
        ...(suffix === '' ? {} : { suffix }),
        as,
      }),
    );
  };

  return (
    <form className="form-grid" onSubmit={submit}>
      <label className="field">
        <span>Rule name</span>
        <input
          className="form-control"
          name="rule_id"
          required
          autoCapitalize="none"
          autoComplete="off"
          spellCheck={false}
          disabled={action.busy}
          value={id}
          onChange={(change) => {
            setId(change.target.value);
          }}
        />
      </label>
      <label className="field">
        <span>Target</span>
        <input
          className="form-control"
          name="target"
          required
          autoCapitalize="none"
          autoComplete="off"
          spellCheck={false}
          list={targets.data === undefined ? undefined : targetsId}
          disabled={action.busy}
          value={target}
          onChange={(change) => {
            setTarget(change.target.value);
          }}
        />
      </label>
      {targets.data === undefined ? null : (
        <datalist id={targetsId}>
          {targets.data.map((name) => (
            <option key={name} value={name} />
          ))}
        </datalist>
      )}
      <fieldset className="field-full">
        <legend className="form-hint">Changes it is for</legend>
        {EVENTS.map((one) => (
          <label className="check" key={one.event}>
            <input
              className="form-check-input"
              type="checkbox"
              name="events"
              value={one.event}
              checked={events.includes(one.event)}
              disabled={action.busy}
              onChange={(change) => {
                toggle(one.event, change.target.checked);
              }}
            />{' '}
            {one.label} <span className="bucket-facts-note">({one.s3})</span>
          </label>
        ))}
      </fieldset>
      <label className="field">
        <span>Keys beginning with (optional)</span>
        <input
          className="form-control"
          name="prefix"
          autoCapitalize="none"
          spellCheck={false}
          aria-describedby={keysHintId}
          disabled={action.busy}
          value={prefix}
          onChange={(change) => {
            setPrefix(change.target.value);
          }}
        />
      </label>
      <label className="field">
        <span>Keys ending with (optional)</span>
        <input
          className="form-control"
          name="suffix"
          autoCapitalize="none"
          spellCheck={false}
          aria-describedby={keysHintId}
          disabled={action.busy}
          value={suffix}
          onChange={(change) => {
            setSuffix(change.target.value);
          }}
        />
      </label>
      <p id={keysHintId} className="form-hint field-full">
        Byte for byte, case and all. Empty: every key of bucket {bucket}.
      </p>
      <label className="field">
        <span>Shown in the S3 API as</span>
        <select
          className="form-select"
          name="as"
          disabled={action.busy}
          value={as}
          onChange={(change) => {
            setAs(change.target.value === 'topic' ? 'topic' : 'queue');
          }}
        >
          <option value="queue">QueueConfiguration</option>
          <option value="topic">TopicConfiguration</option>
        </select>
      </label>
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button
          type="submit"
          className="btn btn-primary"
          disabled={action.busy}
          title="Add this rule to the rules of the bucket"
        >
          <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add rule</span>
        </button>
        {children}
      </div>
    </form>
  );
}
