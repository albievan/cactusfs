import { useEffect, useId, useRef, useState, type FormEvent } from 'react';

import { api, unwrap, type TagDefinition } from '../api/client';
import { ConfirmButton } from '../ui/ConfirmButton';
import { formatTime } from '../ui/format';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { firstOf, LoadFailure, olderNode, onCode, refused, type Explain } from '../ui/sections';
import { joinValues, MOST_TAG_DEFINITIONS, MOST_TAG_VALUES, splitValues, TAG_KEY_LENGTH, TAG_VALUE_LENGTH } from '../ui/tags';
import { Said } from './TagSections';

const CATALOGUE_ON_AN_OLDER_NODE = olderNode('The catalogue of tags is');

/** What a key of the catalogue may be, as the contract has it (`TagDefinitionKey`). */
export const DEFINITION_KEY_RULE = `In lower case: 1 to ${String(TAG_KEY_LENGTH)} letters, digits, spaces and + - = . _ : / @.`;

/** What the values of a definition may be, as the contract has them (`TagDefinitionValue`). */
export const DEFINITION_VALUES_RULE = `Separated by commas, in lower case: at most ${String(MOST_TAG_VALUES)}, of at most ${String(TAG_VALUE_LENGTH)} characters each. None lets the tag have any value.`;

function loadDefinitions(signal: AbortSignal) {
  return unwrap(api.GET('/tag-definitions', { signal }));
}

/**
 * The deployment's catalogue of tags (ADR-0018): the tags users are offered
 * when they tag, each a key, what it is for and the values it may have. They
 * are listed, defined, changed and removed - removed after a question. Who may
 * do what is the API's to say: a caller it does not show the catalogue is
 * told so in a line, and is offered nothing else; a change it refuses is
 * shown in its words.
 */
export function TagsScreen() {
  const definitions = useLoad(loadDefinitions);
  const removal = useAction();
  const [open, setOpen] = useState<string | null>(null);
  const [said, setSaid] = useState<string | null>(null);
  // The definition the last removal was of: what its error is said of.
  const [removing, setRemoving] = useState('');
  const listRef = useRef<HTMLDivElement>(null);

  if (definitions.data === undefined && definitions.loading) {
    return (
      <>
        <h1>Tags</h1>
        <Loading />
      </>
    );
  }
  if (definitions.data === undefined && refused(definitions.error)) {
    return (
      <>
        <h1>Tags</h1>
        <p className="bucket-facts-note">You may not see the catalogue of tags.</p>
      </>
    );
  }

  const remove = (key: string) => {
    setRemoving(key);
    setSaid(null);
    void removal.run(async () => {
      try {
        await unwrap(api.DELETE('/tag-definitions/{key}', { params: { path: { key } } }));
        setOpen((shown) => (shown === key ? null : shown));
        setSaid(`Removed tag ${key} from the catalogue. Tags already kept are not changed.`);
        listRef.current?.focus();
      } finally {
        definitions.reload();
      }
    });
  };

  // Closed, the panel hands the keyboard back to the button that opened it,
  // or to the list when that row is no longer there.
  const close = (key: string) => {
    setOpen(null);
    const opener = [...(listRef.current?.querySelectorAll<HTMLButtonElement>('button[data-definition]') ?? [])].find(
      (button) => button.dataset.definition === key,
    );
    (opener ?? listRef.current)?.focus();
  };

  const explainRemoval = firstOf(
    CATALOGUE_ON_AN_OLDER_NODE,
    onCode(
      'EntityInUse',
      `Tag ${removing} cannot be removed: the tag rules of a bucket require it. Take it out of those rules first. The server does not say which bucket: you may not be allowed to know of it.`,
    ),
    onCode('NoSuchEntity', `Tag ${removing} is not in the catalogue any more.`),
  );
  const list = definitions.data;
  const opened = list?.find((definition) => definition.key === open);

  return (
    <>
      <h1>Tags</h1>
      <p className="form-hint">
        The catalogue of tags: the tags users are offered when they tag buckets and objects, and when they write the tag
        rules of a bucket. Tags outside it stay allowed, but in a bucket whose rules allow only the catalogue&apos;s. It
        holds at most {MOST_TAG_DEFINITIONS} definitions.
      </p>
      <Said text={said} />

      <section className="card card-body panel" aria-labelledby="tag-definitions">
        <h2 id="tag-definitions">Catalogue</h2>
        <div ref={listRef} role="region" aria-label="Definitions of tags" tabIndex={-1}>
          {list === undefined ? null : list.length === 0 ? (
            <p>The catalogue defines no tags.</p>
          ) : (
            <table className="table table-hover align-middle grid">
              <thead>
                <tr>
                  <th scope="col">Key</th>
                  <th scope="col">Description</th>
                  <th scope="col">Values</th>
                  <th scope="col">Defined</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.map((definition) => (
                  <tr key={definition.key}>
                    <td data-label="Key">{definition.key}</td>
                    <td data-label="Description">{definition.description}</td>
                    <td data-label="Values">
                      {definition.values.length === 0 ? (
                        <span className="bucket-facts-note">any value</span>
                      ) : (
                        joinValues(definition.values)
                      )}
                    </td>
                    <td data-label="Defined">{formatTime(definition.created_at)}</td>
                    <td data-label="Actions">
                      <span className="d-flex flex-wrap gap-2">
                        <button
                          type="button"
                          className="btn btn-outline-secondary"
                          aria-label={`Edit tag ${definition.key}`}
                          data-definition={definition.key}
                          title="Change what this tag is for and the values it may have"
                          onClick={() => {
                            setSaid(null);
                            setOpen(definition.key);
                          }}
                        >
                          <i className="bi bi-pencil" aria-hidden="true" /> <span className="btn-text">Edit</span>
                        </button>
                        <ConfirmButton
                          label="Delete"
                          question={`Remove tag ${definition.key} from the catalogue? Tags already kept are not changed.`}
                          disabled={removal.busy}
                          onConfirm={() => {
                            remove(definition.key);
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
        {definitions.error === null ? null : (
          <LoadFailure
            error={definitions.error}
            explain={CATALOGUE_ON_AN_OLDER_NODE}
            onRetry={definitions.reload}
            what="the catalogue of tags"
          />
        )}
        <ProblemNotice error={removal.error} explain={explainRemoval} />
      </section>

      {opened === undefined ? null : (
        <DefinitionPanel
          // A fresh editor for every definition, and after every save.
          key={JSON.stringify(opened)}
          definition={opened}
          onSaved={() => {
            setSaid(`Saved the definition of tag ${opened.key}. Tags already kept and tag rules are not changed.`);
            definitions.reload();
          }}
          onClose={() => {
            close(opened.key);
          }}
        />
      )}

      <CreateDefinition onCreated={definitions.reload} />
    </>
  );
}

interface DefinitionFieldsProps {
  description: string;
  onDescription: (text: string) => void;
  values: string;
  onValues: (text: string) => void;
  action: Action;
  valuesHintId: string;
}

/** The description and the values of a definition, as they are typed. */
function DefinitionFields({ description, onDescription, values, onValues, action, valuesHintId }: DefinitionFieldsProps) {
  return (
    <>
      <label className="field field-full">
        <span>What the tag is for</span>
        <input
          className="form-control"
          name="description"
          disabled={action.busy}
          value={description}
          onChange={(event) => {
            onDescription(event.target.value);
          }}
        />
      </label>
      <label className="field field-full">
        <span>Values it may have</span>
        <input
          className="form-control"
          name="values"
          autoCapitalize="none"
          autoComplete="off"
          spellCheck={false}
          placeholder="any value"
          aria-describedby={valuesHintId}
          disabled={action.busy}
          value={values}
          onChange={(event) => {
            onValues(event.target.value);
          }}
        />
      </label>
      <p id={valuesHintId} className="form-hint field-full">
        {DEFINITION_VALUES_RULE}
      </p>
    </>
  );
}

interface DefinitionPanelProps {
  definition: TagDefinition;
  onSaved: () => void;
  onClose: () => void;
}

/**
 * One definition, changed: what it is for and the values it may have, sent
 * whole. Opened, it takes the keyboard to its heading and is scrolled into
 * view.
 */
function DefinitionPanel({ definition, onSaved, onClose }: DefinitionPanelProps) {
  const headingId = useId();
  const valuesHintId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  const action = useAction();
  const [description, setDescription] = useState(definition.description);
  const [values, setValues] = useState(joinValues(definition.values));
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

  const save = (event: FormEvent) => {
    event.preventDefault();
    void action.run(async () => {
      await unwrap(
        api.PUT('/tag-definitions/{key}', {
          params: { path: { key: definition.key } },
          body: { description, values: splitValues(values) },
        }),
      );
      onSaved();
    });
  };
  const explain = firstOf(
    CATALOGUE_ON_AN_OLDER_NODE,
    onCode('InvalidArgument', `The definition of ${definition.key} was refused: the catalogue keeps the one it had.`),
  );

  return (
    <section className="card card-body panel" aria-labelledby={headingId}>
      <h2 id={headingId} ref={heading} tabIndex={-1}>
        Tag {definition.key}
      </h2>
      <p className="form-hint">
        Defined {formatTime(definition.created_at)}. Changing it changes no tag already kept, and no tag rule.
      </p>
      <form className="form-grid" onSubmit={save}>
        <DefinitionFields
          description={description}
          onDescription={setDescription}
          values={values}
          onValues={setValues}
          action={action}
          valuesHintId={valuesHintId}
        />
        <div className="field-full">
          <ProblemNotice error={action.error} explain={explain} />
        </div>
        <div className="form-actions d-flex flex-wrap gap-2">
          <button type="submit" className="btn btn-primary" disabled={action.busy} title="Save this definition">
            <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save definition</span>
          </button>
          <button type="button" className="btn btn-outline-secondary" onClick={onClose} title="Close this definition">
            <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close</span>
          </button>
        </div>
      </form>
    </section>
  );
}

/** Defines a tag: a key, what it is for, and the values it may have, if only some. */
function CreateDefinition({ onCreated }: { onCreated: () => void }) {
  const headingId = useId();
  const keyHintId = useId();
  const valuesHintId = useId();
  const action = useAction();
  const [key, setKey] = useState('');
  const [description, setDescription] = useState('');
  const [values, setValues] = useState('');
  // The key the last definition was of: what its error is said of.
  const [creating, setCreating] = useState('');

  const create = (event: FormEvent) => {
    event.preventDefault();
    setCreating(key);
    const chosen = splitValues(values);
    void action.run(async () => {
      await unwrap(
        api.POST('/tag-definitions', {
          body: {
            key,
            ...(description === '' ? {} : { description }),
            ...(chosen.length === 0 ? {} : { values: chosen }),
          },
        }),
      );
      setKey('');
      setDescription('');
      setValues('');
      onCreated();
    });
  };

  const explain: Explain = firstOf(
    CATALOGUE_ON_AN_OLDER_NODE,
    onCode('EntityAlreadyExists', `Tag ${creating} is defined already: edit it in the catalogue.`),
    onCode('InvalidArgument', `The definition of ${creating} was refused, and nothing was defined.`),
  );

  return (
    <section className="card card-body panel" aria-labelledby={headingId}>
      <h2 id={headingId}>Define a tag</h2>
      <form className="form-grid" onSubmit={create}>
        <label className="field">
          <span>Key</span>
          <input
            className="form-control"
            name="key"
            required
            autoCapitalize="none"
            autoComplete="off"
            spellCheck={false}
            aria-describedby={keyHintId}
            disabled={action.busy}
            value={key}
            onChange={(event) => {
              setKey(event.target.value);
            }}
          />
        </label>
        <p id={keyHintId} className="form-hint field-full">
          {DEFINITION_KEY_RULE}
        </p>
        <DefinitionFields
          description={description}
          onDescription={setDescription}
          values={values}
          onValues={setValues}
          action={action}
          valuesHintId={valuesHintId}
        />
        <div className="field-full">
          <ProblemNotice error={action.error} explain={explain} />
        </div>
        <div className="form-actions d-flex flex-wrap gap-2">
          <button type="submit" className="btn btn-primary" disabled={action.busy} title="Add this tag to the catalogue">
            <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Define tag</span>
          </button>
        </div>
      </form>
    </section>
  );
}
