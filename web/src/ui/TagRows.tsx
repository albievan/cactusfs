import { useEffect, useId, useRef } from 'react';

import { emptyRow, type TagRow, type TagSuggestions } from './tags';

interface TagRowsProps {
  /** The rows, in the order they were typed. */
  rows: TagRow[];
  onChange: (rows: TagRow[]) => void;
  /** The name of the group, for whoever has the page read: "Tags of bucket photos". */
  label: string;
  /** The id of the hint the group is described by: the bounds of a tag, and more. */
  hintId?: string;
  /** What may be offered as keys and values; nothing is offered without it. */
  suggestions?: TagSuggestions;
  /** True while a change is being carried out. */
  disabled?: boolean;
}

/**
 * Tags edited as rows of a key and a value: added, changed and removed, in
 * the order they are typed. It checks nothing - an empty key, a key given
 * twice and one tag too many are sent as typed, and the API says what it
 * refuses. The keyboard goes to the key of a row that is added, and to
 * "Add a tag" when a row is removed. On a phone a row is a key above its
 * value, and its button a finger can hit.
 */
export function TagRows({ rows, onChange, label, hintId, suggestions, disabled = false }: TagRowsProps) {
  const base = useId();
  const group = useRef<HTMLDivElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  // The row whose key is to have the keyboard once it is drawn.
  const focusRow = useRef<string | null>(null);

  useEffect(() => {
    const id = focusRow.current;
    if (id === null) {
      return;
    }
    const input = [...(group.current?.querySelectorAll<HTMLInputElement>('input[data-tag-key]') ?? [])].find(
      (element) => element.dataset.tagKey === id,
    );
    if (input !== undefined) {
      focusRow.current = null;
      input.focus();
    }
  });

  const change = (id: string, part: 'key' | 'value', text: string) => {
    onChange(rows.map((row) => (row.id === id ? { ...row, [part]: text } : row)));
  };

  const add = () => {
    const row = emptyRow();
    onChange([...rows, row]);
    focusRow.current = row.id;
  };

  const remove = (id: string) => {
    onChange(rows.filter((row) => row.id !== id));
    addButton.current?.focus();
  };

  const keysId = `${base}-keys`;
  return (
    <div className="tag-rows" role="group" aria-label={label} aria-describedby={hintId} ref={group}>
      {suggestions === undefined || suggestions.keys.length === 0 ? null : (
        <datalist id={keysId}>
          {suggestions.keys.map((key) => (
            <option key={key} value={key} />
          ))}
        </datalist>
      )}
      {rows.length === 0 ? <p className="tag-rows-empty">No tags.</p> : null}
      {rows.map((row, index) => {
        const n = String(index + 1);
        const values = suggestions?.valuesOf(row.key) ?? [];
        const valuesId = `${base}-values-${n}`;
        return (
          <div className="tag-row" key={row.id}>
            <label className="field">
              <span>Key of tag {n}</span>
              <input
                className="form-control"
                data-tag-key={row.id}
                autoCapitalize="none"
                autoComplete="off"
                spellCheck={false}
                list={suggestions === undefined || suggestions.keys.length === 0 ? undefined : keysId}
                disabled={disabled}
                value={row.key}
                onChange={(event) => {
                  change(row.id, 'key', event.target.value);
                }}
              />
            </label>
            <label className="field">
              <span>Value of tag {n}</span>
              <input
                className="form-control"
                autoCapitalize="none"
                autoComplete="off"
                spellCheck={false}
                list={values.length === 0 ? undefined : valuesId}
                disabled={disabled}
                value={row.value}
                onChange={(event) => {
                  change(row.id, 'value', event.target.value);
                }}
              />
            </label>
            {values.length === 0 ? null : (
              <datalist id={valuesId}>
                {values.map((value) => (
                  <option key={value} value={value} />
                ))}
              </datalist>
            )}
            <button
              type="button"
              className="btn btn-outline-secondary tag-row-remove"
              aria-label={`Remove tag ${n}`}
              title="Take this tag out of the list: nothing is sent until the list is"
              disabled={disabled}
              onClick={() => {
                remove(row.id);
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
          title="Add a row for another tag"
          disabled={disabled}
          onClick={add}
        >
          <i className="bi bi-plus-lg" aria-hidden="true" /> <span className="btn-text">Add a tag</span>
        </button>
      </div>
    </div>
  );
}
