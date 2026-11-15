import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';

import { api, unwrap, type RequiredTag, type Tag, type TagRules } from '../api/client';
import { useCatalogue } from '../ui/catalogue';
import { ConfirmButton } from '../ui/ConfirmButton';
import { useAction, useLoad, type Action } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';
import { firstOf, Fold, LoadFailure, olderNode, onCode, refused, Said, useFocusAfterChange, type Explain } from '../ui/sections';
import { TagRows } from '../ui/TagRows';
import {
  joinValues,
  MOST_BUCKET_TAGS,
  MOST_REQUIRED_TAGS,
  MOST_TAG_VALUES,
  rowsOf,
  splitValues,
  suggestionsOf,
  TAG_RULE,
  TAG_VALUE_LENGTH,
  tagsOf,
  type TagSuggestions,
} from '../ui/tags';

const BUCKET_TAGS_ON_AN_OLDER_NODE = olderNode('Bucket tags are');
const TAG_RULES_ON_AN_OLDER_NODE = olderNode('Tag rules are');

/** What is said once tags were saved: the answer is what the API keeps, which may not be what was typed. */
export const TAGS_SAVED = 'Saved. Shown as they are kept: in key order, and in lower case where the tag rules say so.';

/** A key that makes a fresh editor whenever what it shows changes. */
function keyOf(value: unknown): string {
  return JSON.stringify(value);
}

interface TagListFormProps {
  /** The tags it begins with. */
  initial: readonly Tag[];
  /** The name of the group of rows: "Tags of bucket photos". */
  label: string;
  /** What is said under the rows: the bounds of a tag. */
  hint: string;
  suggestions?: TagSuggestions;
  /** Carries the save and the removal out, and holds the error the form shows. */
  action: Action;
  explain: Explain;
  /** The hint of the save button. */
  saveTitle: string;
  /** Sends the tags, in the order they were typed. */
  onSave: (tags: Tag[]) => Promise<void>;
  /** Removes every tag, once asked; absent when there is none to remove. */
  remove?: { question: string; run: () => Promise<void> };
  /** More buttons, after the form's own. */
  children?: ReactNode;
}

/**
 * A list of tags edited as rows, and saved as a whole: what is sent is the
 * list in the order it was typed, for the API to keep or refuse. What it
 * refuses is said under the rows, which keep what was typed.
 */
export function TagListForm({
  initial,
  label,
  hint,
  suggestions,
  action,
  explain,
  saveTitle,
  onSave,
  remove,
  children,
}: TagListFormProps) {
  const hintId = useId();
  const [rows, setRows] = useState(() => rowsOf(initial));

  const save = (event: FormEvent) => {
    event.preventDefault();
    void action.run(() => onSave(tagsOf(rows)));
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <div className="field-full">
        <TagRows
          rows={rows}
          onChange={setRows}
          label={label}
          hintId={hintId}
          suggestions={suggestions}
          disabled={action.busy}
        />
      </div>
      <p id={hintId} className="form-hint field-full">
        {hint}
      </p>
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button type="submit" className="btn btn-primary" disabled={action.busy} title={saveTitle}>
          <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save tags</span>
        </button>
        {remove === undefined ? null : (
          <ConfirmButton
            label="Remove tags"
            question={remove.question}
            disabled={action.busy}
            onConfirm={() => {
              void action.run(remove.run);
            }}
          />
        )}
        {children}
      </div>
    </form>
  );
}

/**
 * The tags of a bucket (ADR-0011), folded until they are asked for: shown as
 * rows, changed, saved as a whole, and removed after a question. What the
 * API answers a save with - the tags as they are kept, lower-cased where the
 * bucket's rules say so - is what is shown after it.
 */
export function BucketTagsSection({ bucket }: { bucket: string }) {
  return (
    <Fold title="Bucket tags" what="the tags of this bucket">
      <BucketTagsBody bucket={bucket} />
    </Fold>
  );
}

function BucketTagsBody({ bucket }: { bucket: string }) {
  const load = useCallback(
    (signal: AbortSignal) => unwrap(api.GET('/buckets/{bucket}/tags', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const tags = useLoad(load);
  const catalogue = useCatalogue();
  const action = useAction();
  // The tags as the API answered the last change with them: as they are kept.
  const [kept, setKept] = useState<Tag[] | undefined>(undefined);
  const [said, setSaid] = useState<string | null>(null);
  const formAreaRef = useRef<HTMLDivElement>(null);
  const focusSaveRef = useRef(false);
  useFocusAfterChange(formAreaRef, focusSaveRef);
  const again = () => {
    setKept(undefined);
    setSaid(null);
    tags.reload();
  };

  if (tags.data === undefined && tags.loading) {
    return <Loading />;
  }
  if (tags.data === undefined && refused(tags.error)) {
    // Not a failure: tags are replaced as a whole, and a list the caller
    // cannot see is not one to replace from here.
    return <p className="bucket-facts-note">You may not see the tags of this bucket.</p>;
  }
  if (tags.data === undefined) {
    return (
      <LoadFailure
        error={tags.error}
        explain={BUCKET_TAGS_ON_AN_OLDER_NODE}
        onRetry={again}
        what="the tags of this bucket"
      />
    );
  }

  const shown = kept ?? tags.data.tags;
  const explain = firstOf(
    BUCKET_TAGS_ON_AN_OLDER_NODE,
    onCode('InvalidArgument', `The tags were refused: bucket ${bucket} keeps the tags it had.`),
    onCode('InvalidTag', `The tags were refused: bucket ${bucket} keeps the tags it had.`),
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
  );
  const save = async (list: Tag[]) => {
    setSaid(null);
    const answer = await unwrap(
      api.PUT('/buckets/{bucket}/tags', { params: { path: { bucket } }, body: { tags: list } }),
    );
    focusSaveRef.current = true;
    setKept(answer.tags);
    setSaid(TAGS_SAVED);
  };
  const remove = async () => {
    setSaid(null);
    await unwrap(api.DELETE('/buckets/{bucket}/tags', { params: { path: { bucket } } }));
    focusSaveRef.current = true;
    setKept([]);
    setSaid(`Removed every tag of bucket ${bucket}.`);
  };

  return (
    <>
      {tags.error === null ? null : (
        <LoadFailure
          error={tags.error}
          explain={BUCKET_TAGS_ON_AN_OLDER_NODE}
          onRetry={again}
          what="the tags of this bucket"
        />
      )}
      <Said text={said} />
      {shown.length === 0 ? <p className="bucket-facts-note">This bucket has no tags.</p> : null}
      <div ref={formAreaRef}>
        <TagListForm
          key={keyOf(shown)}
          initial={shown}
          label={`Tags of bucket ${bucket}`}
          hint={`A bucket has at most ${String(MOST_BUCKET_TAGS)} tags. ${TAG_RULE} Saved, the list replaces the tags the bucket has; an empty list removes them.`}
          suggestions={suggestionsOf(catalogue)}
          action={action}
          explain={explain}
          saveTitle="Set these tags in place of the ones the bucket has"
          onSave={save}
          remove={
            shown.length === 0
              ? undefined
              : { question: `Remove every tag of bucket ${bucket}?`, run: remove }
          }
        />
      </div>
    </>
  );
}

/** A required tag as its row in the editor of rules: values as typed, with commas between them. */
interface RequiredRow {
  id: string;
  key: string;
  values: string;
  subBucket: string;
}

function requiredRowsOf(required: readonly RequiredTag[]): RequiredRow[] {
  return required.map((rule) => ({
    id: crypto.randomUUID(),
    key: rule.key,
    values: joinValues(rule.values),
    subBucket: rule.sub_bucket ?? '',
  }));
}

/**
 * The required tags the rows say, in their order. A row left wholly empty is
 * not sent; every other is, as typed, for the API to judge. No values is any
 * value, and no sub-bucket the whole bucket.
 */
function requiredOf(rows: readonly RequiredRow[]): RequiredTag[] {
  return rows
    .filter((row) => row.key !== '' || row.values.trim() !== '' || row.subBucket !== '')
    .map((row) => {
      const values = splitValues(row.values);
      return {
        key: row.key,
        ...(values.length === 0 ? {} : { values }),
        ...(row.subBucket === '' ? {} : { sub_bucket: row.subBucket }),
      };
    });
}

/** Rules that ask nothing: what a bucket without rules is answered with. */
const NO_RULES: TagRules = { lowercase: false, catalogue_only: false, required: [] };

/**
 * The tag rules of a bucket (ADR-0018), folded until they are asked for:
 * whether tags are kept in lower case, whether only the catalogue's tags may
 * be given, and which tags every object must carry. Saved as a whole and
 * removed after a question. A caller the API does not tell the rules is told
 * so, in a line, and nothing more.
 */
export function TagRulesSection({ bucket }: { bucket: string }) {
  return (
    <Fold title="Tag rules" what="the tag rules of this bucket">
      <TagRulesBody bucket={bucket} />
    </Fold>
  );
}

function TagRulesBody({ bucket }: { bucket: string }) {
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/tag-rules', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const rules = useLoad(load);
  const catalogue = useCatalogue();
  const action = useAction();
  const [kept, setKept] = useState<TagRules | undefined>(undefined);
  const [said, setSaid] = useState<string | null>(null);
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
    // Not a failure: rules are replaced as a whole, and rules the caller
    // cannot see are not ones to replace from here.
    return <p className="bucket-facts-note">You may not see the tag rules of this bucket.</p>;
  }
  if (rules.data === undefined) {
    return (
      <LoadFailure
        error={rules.error}
        explain={TAG_RULES_ON_AN_OLDER_NODE}
        onRetry={again}
        what="the tag rules of this bucket"
      />
    );
  }

  const shown = kept ?? rules.data;
  const none = !shown.lowercase && !shown.catalogue_only && shown.required.length === 0;
  const explain = firstOf(
    TAG_RULES_ON_AN_OLDER_NODE,
    onCode('InvalidArgument', `The rules were refused: bucket ${bucket} keeps the rules it had.`),
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
  );
  const save = async (next: TagRules) => {
    setSaid(null);
    const answer = await unwrap(
      api.PUT('/buckets/{bucket}/tag-rules', { params: { path: { bucket } }, body: next }),
    );
    focusSaveRef.current = true;
    setKept(answer);
    setSaid('Saved. Objects stored before keep the tags they have.');
  };
  const remove = async () => {
    setSaid(null);
    await unwrap(api.DELETE('/buckets/{bucket}/tag-rules', { params: { path: { bucket } } }));
    focusSaveRef.current = true;
    setKept(NO_RULES);
    setSaid(`Removed the tag rules of bucket ${bucket}.`);
  };

  return (
    <>
      {rules.error === null ? null : (
        <LoadFailure
          error={rules.error}
          explain={TAG_RULES_ON_AN_OLDER_NODE}
          onRetry={again}
          what="the tag rules of this bucket"
        />
      )}
      <Said text={said} />
      {none ? (
        <p className="bucket-facts-note">
          This bucket has no tag rules: tags are kept in the case they are sent in, any tag may be given, and none is
          required.
        </p>
      ) : null}
      <div ref={formAreaRef}>
        <TagRulesForm
          key={keyOf(shown)}
          bucket={bucket}
          initial={shown}
          suggestions={suggestionsOf(catalogue)}
          action={action}
          explain={explain}
          onSave={save}
          remove={none ? undefined : remove}
        />
      </div>
    </>
  );
}

interface TagRulesFormProps {
  bucket: string;
  initial: TagRules;
  suggestions: TagSuggestions;
  action: Action;
  explain: Explain;
  onSave: (rules: TagRules) => Promise<void>;
  /** Removes the rules, once asked; absent when there are none. */
  remove?: () => Promise<void>;
}

/** The rules of a bucket's tags, edited and sent whole: every field is given, as the contract asks. */
function TagRulesForm({ bucket, initial, suggestions, action, explain, onSave, remove }: TagRulesFormProps) {
  const hintId = useId();
  const keysId = useId();
  const group = useRef<HTMLDivElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  const focusRow = useRef<string | null>(null);
  const [lowercase, setLowercase] = useState(initial.lowercase);
  const [catalogueOnly, setCatalogueOnly] = useState(initial.catalogue_only);
  const [rows, setRows] = useState(() => requiredRowsOf(initial.required));

  // The keyboard goes to the key of a row that was added, once it is drawn.
  useEffect(() => {
    const id = focusRow.current;
    if (id === null) {
      return;
    }
    const input = [...(group.current?.querySelectorAll<HTMLInputElement>('input[data-required-key]') ?? [])].find(
      (element) => element.dataset.requiredKey === id,
    );
    if (input !== undefined) {
      focusRow.current = null;
      input.focus();
    }
  });

  const change = (id: string, part: 'key' | 'values' | 'subBucket', text: string) => {
    setRows(rows.map((row) => (row.id === id ? { ...row, [part]: text } : row)));
  };
  const add = () => {
    const row: RequiredRow = { id: crypto.randomUUID(), key: '', values: '', subBucket: '' };
    setRows([...rows, row]);
    focusRow.current = row.id;
  };
  const drop = (id: string) => {
    setRows(rows.filter((row) => row.id !== id));
    addButton.current?.focus();
  };
  const save = (event: FormEvent) => {
    event.preventDefault();
    void action.run(() =>
      onSave({ lowercase, catalogue_only: catalogueOnly, required: requiredOf(rows) }),
    );
  };

  return (
    <form className="form-grid" onSubmit={save}>
      <label className="check field-full tag-rule-check">
        <input
          className="form-check-input"
          type="checkbox"
          name="lowercase"
          checked={lowercase}
          disabled={action.busy}
          onChange={(event) => {
            setLowercase(event.target.checked);
          }}
        />{' '}
        Keep tags in lower case, whatever case they are sent in
      </label>
      <label className="check field-full tag-rule-check">
        <input
          className="form-check-input"
          type="checkbox"
          name="catalogue_only"
          checked={catalogueOnly}
          disabled={action.busy}
          onChange={(event) => {
            setCatalogueOnly(event.target.checked);
          }}
        />{' '}
        Allow only the tags of the catalogue, with the values it allows
      </label>
      <div
        className="field-full tag-rows"
        role="group"
        aria-label={`Required tags of bucket ${bucket}`}
        aria-describedby={hintId}
        ref={group}
      >
        <h4>Required tags</h4>
        {suggestions.keys.length === 0 ? null : (
          <datalist id={keysId}>
            {suggestions.keys.map((key) => (
              <option key={key} value={key} />
            ))}
          </datalist>
        )}
        {rows.length === 0 ? <p className="tag-rows-empty">No tag is required.</p> : null}
        {rows.map((row, index) => {
          const n = String(index + 1);
          return (
            <div className="tag-row tag-row-required" key={row.id}>
              <label className="field">
                <span>Key of required tag {n}</span>
                <input
                  className="form-control"
                  data-required-key={row.id}
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  list={suggestions.keys.length === 0 ? undefined : keysId}
                  disabled={action.busy}
                  value={row.key}
                  onChange={(event) => {
                    change(row.id, 'key', event.target.value);
                  }}
                />
              </label>
              <label className="field">
                <span>Allowed values of required tag {n}</span>
                <input
                  className="form-control"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="any value"
                  disabled={action.busy}
                  value={row.values}
                  onChange={(event) => {
                    change(row.id, 'values', event.target.value);
                  }}
                />
              </label>
              <label className="field">
                <span>Sub-bucket of required tag {n}</span>
                <input
                  className="form-control"
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="the whole bucket"
                  disabled={action.busy}
                  value={row.subBucket}
                  onChange={(event) => {
                    change(row.id, 'subBucket', event.target.value);
                  }}
                />
              </label>
              <button
                type="button"
                className="btn btn-outline-secondary tag-row-remove"
                aria-label={`Remove required tag ${n}`}
                title="Take this tag out of the required ones: nothing is sent until the rules are saved"
                disabled={action.busy}
                onClick={() => {
                  drop(row.id);
                }}
              >
                <i className="bi bi-dash-lg" aria-hidden="true" /> <span className="btn-text">Remove</span>
              </button>
            </div>
          );
        })}
        <div>
          <button
            type="button"
            ref={addButton}
            className="btn btn-outline-secondary"
            title="Add a row for another required tag"
            disabled={action.busy}
            onClick={add}
          >
            <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add a required tag</span>
          </button>
        </div>
      </div>
      <p id={hintId} className="form-hint field-full">
        At most {MOST_REQUIRED_TAGS} required tags. {TAG_RULE} Allowed values are separated by commas, at most{' '}
        {MOST_TAG_VALUES} of at most {TAG_VALUE_LENGTH} characters each; none allows any value. A tag required in a
        sub-bucket is required only of the objects under it. With lower case on, write the keys and values of required
        tags in lower case. Rules hold for objects stored from now on; objects stored before keep their tags.
      </p>
      <div className="field-full">
        <ProblemNotice error={action.error} explain={explain} />
      </div>
      <div className="form-actions d-flex flex-wrap gap-2">
        <button
          type="submit"
          className="btn btn-primary"
          disabled={action.busy}
          title="Set these rules in place of the ones the bucket has"
        >
          <i className="bi bi-check-lg" aria-hidden="true" /> <span className="btn-text">Save rules</span>
        </button>
        {remove === undefined ? null : (
          <ConfirmButton
            label="Remove rules"
            question={`Remove the tag rules of bucket ${bucket}? Tags already kept are not changed.`}
            disabled={action.busy}
            onConfirm={() => {
              void action.run(remove);
            }}
          />
        )}
      </div>
    </form>
  );
}
