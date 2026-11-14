import { useCallback, useEffect, useId, useRef, useState } from 'react';

import { api, ApiError, unwrap, type Tag } from '../api/client';
import { useCatalogue } from '../ui/catalogue';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { reasonOf } from '../ui/reason';
import { firstOf, olderNode, onCode, refused, type Explain } from '../ui/sections';
import { MOST_OBJECT_TAGS, suggestionsOf, TAG_RULE } from '../ui/tags';
import { Said, TagListForm, TAGS_SAVED } from './TagSections';

const OBJECT_TAGS_ON_AN_OLDER_NODE = olderNode('Object tags are');

/** What is said of a delete marker, which the API answers 405 `MethodNotAllowed`: it has no tags. */
export const DELETE_MARKER_HAS_NO_TAGS = 'This version is a delete marker, which has no tags.';

/** What the console says of a failed read of the tags, without an alert: it is no failure of the details. */
function unreadable(error: unknown): string {
  if (error instanceof ApiError) {
    const words = firstOf(OBJECT_TAGS_ON_AN_OLDER_NODE, onCode('MethodNotAllowed', DELETE_MARKER_HAS_NO_TAGS))(
      error.problem,
    );
    if (words !== undefined) {
      return words;
    }
  }
  return `The tags of this object could not be read: ${reasonOf(error, 'the request failed')}`;
}

interface ObjectTagsProps {
  bucket: string;
  objectKey: string;
}

/**
 * The tags of an object - of its current version, which is what its details
 * show - as the API tells them: listed, and changed, saved or removed in an
 * editor that is opened from the list. Reading them takes a permission of
 * its own: a refusal is said in a line, and is no failure of the details. A
 * change the bucket's tag rules refuse is shown with the API's detail, which
 * names the rule; the tags the object had are kept.
 */
export function ObjectTags({ bucket, objectKey }: ObjectTagsProps) {
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/object/tags', { params: { path: { bucket }, query: { key: objectKey } }, signal })),
    [bucket, objectKey],
  );
  const tags = useLoad(load);
  const headingId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  const editButton = useRef<HTMLButtonElement>(null);
  // Where the keyboard is to go once the editor has opened or closed.
  const moveTo = useRef<'heading' | 'edit' | null>(null);
  const [editing, setEditing] = useState(false);
  const [kept, setKept] = useState<Tag[] | undefined>(undefined);
  const [said, setSaid] = useState<string | null>(null);
  // What the last change was: what its refusal is said of.
  const [last, setLast] = useState<'save' | 'remove'>('save');

  useEffect(() => {
    const where = moveTo.current;
    moveTo.current = null;
    if (where === 'heading') {
      heading.current?.focus();
    } else if (where === 'edit') {
      editButton.current?.focus();
    }
  }, [editing]);

  const open = () => {
    setSaid(null);
    moveTo.current = 'heading';
    setEditing(true);
  };
  const close = () => {
    moveTo.current = 'edit';
    setEditing(false);
  };

  let body;
  if (tags.data === undefined && tags.loading) {
    body = <Loading />;
  } else if (tags.data === undefined) {
    body = (
      <p className="form-hint object-access-note">
        {refused(tags.error) ? 'You may not see the tags of this object.' : unreadable(tags.error)}{' '}
        {refused(tags.error) || (tags.error instanceof ApiError && tags.error.problem.status === 405) ? null : (
          <button
            type="button"
            className="btn btn-outline-secondary btn-sm"
            onClick={tags.reload}
            title="Ask for the tags of this object again"
          >
            <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Read tags again</span>
          </button>
        )}
      </p>
    );
  } else {
    const shown = kept ?? tags.data.tags;
    const save = async (list: Tag[]) => {
      setLast('save');
      const answer = await unwrap(
        api.PUT('/buckets/{bucket}/object/tags', {
          params: { path: { bucket }, query: { key: objectKey } },
          body: { tags: list },
        }),
      );
      setKept(answer.tags);
      setSaid(TAGS_SAVED);
      close();
    };
    const remove = async () => {
      setLast('remove');
      await unwrap(
        api.DELETE('/buckets/{bucket}/object/tags', { params: { path: { bucket }, query: { key: objectKey } } }),
      );
      setKept([]);
      setSaid('Removed every tag of this object.');
      close();
    };
    body = editing ? (
      <ObjectTagsEditor
        objectKey={objectKey}
        initial={shown}
        explain={explainChange(bucket, objectKey, last)}
        onSave={save}
        onRemove={shown.length === 0 ? undefined : remove}
        onClose={close}
      />
    ) : (
      <>
        {shown.length === 0 ? (
          <p className="form-hint">This object has no tags.</p>
        ) : (
          <dl className="facts">
            {shown.map((tag, index) => (
              // A key is given once, but the list is the API's: the place keeps two apart.
              <div className="facts-pair" key={`${String(index)} ${tag.key}`}>
                <dt>{tag.key}</dt>
                <dd>{tag.value === '' ? <span className="bucket-facts-note">(no value)</span> : tag.value}</dd>
              </div>
            ))}
          </dl>
        )}
        <div className="grid-detail-actions">
          <button
            type="button"
            ref={editButton}
            className="btn btn-outline-secondary"
            onClick={open}
            title="Change the tags of this object"
          >
            <i className="bi bi-tags" aria-hidden="true" /> <span className="btn-text">Edit tags</span>
          </button>
        </div>
      </>
    );
  }

  return (
    <section className="object-tags" aria-labelledby={headingId}>
      <h3 id={headingId} ref={heading} tabIndex={-1}>
        Tags
      </h3>
      <Said text={said} />
      {body}
    </section>
  );
}

/** What is said of a refused change of an object's tags, before the API's own words. */
function explainChange(bucket: string, objectKey: string, last: 'save' | 'remove'): Explain {
  const gone = `${objectKey} is not there any more.`;
  return firstOf(
    OBJECT_TAGS_ON_AN_OLDER_NODE,
    onCode(
      'TagRuleViolated',
      last === 'remove'
        ? `The tag rules of bucket ${bucket} require a tag of this object: its tags are kept.`
        : `The tag rules of bucket ${bucket} do not allow these tags: the object keeps the tags it had.`,
    ),
    onCode('InvalidArgument', 'The tags were refused: the object keeps the tags it had.'),
    onCode('MethodNotAllowed', DELETE_MARKER_HAS_NO_TAGS),
    onCode('NoSuchObject', gone),
    onCode('NoSuchVersion', gone),
    onCode('NoSuchBucket', `Bucket ${bucket} is not there any more.`),
  );
}

interface ObjectTagsEditorProps {
  objectKey: string;
  initial: Tag[];
  explain: Explain;
  onSave: (tags: Tag[]) => Promise<void>;
  onRemove?: () => Promise<void>;
  onClose: () => void;
}

/** The editor of an object's tags; the catalogue is offered once it is open. */
function ObjectTagsEditor({ objectKey, initial, explain, onSave, onRemove, onClose }: ObjectTagsEditorProps) {
  const catalogue = useCatalogue();
  // The editor's own: opened again, it does not show why the last attempt failed.
  const action = useAction();
  return (
    <TagListForm
      initial={initial}
      label={`Tags of ${objectKey}`}
      hint={`An object has at most ${String(MOST_OBJECT_TAGS)} tags. ${TAG_RULE} Saved, the list replaces the tags the object has; an empty list removes them. The bucket's tag rules may require some.`}
      suggestions={suggestionsOf(catalogue)}
      action={action}
      explain={explain}
      saveTitle="Set these tags in place of the ones the object has"
      onSave={onSave}
      remove={onRemove === undefined ? undefined : { question: `Remove every tag of ${objectKey}?`, run: onRemove }}
    >
      <button type="button" className="btn btn-outline-secondary" onClick={onClose} title="Close the editor of the tags">
        <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close</span>
      </button>
    </TagListForm>
  );
}
