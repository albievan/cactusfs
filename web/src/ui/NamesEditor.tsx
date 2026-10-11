import { useState, type FormEvent } from 'react';

import { joinNames, parseNames } from './format';

interface NamesEditorProps {
  /** What the names are, for example "Roles of group ops". */
  label: string;
  /** The names as they are now. */
  names: string[];
  /** True while a change is being carried out. */
  busy: boolean;
  /** Saves the new names. */
  onSave: (names: string[]) => void;
  /** Leaves the names as they are. */
  onCancel: () => void;
}

/** Edits a list of names (roles, policies) typed with commas between them. */
export function NamesEditor({ label, names, busy, onSave, onCancel }: NamesEditorProps) {
  const [text, setText] = useState(joinNames(names));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    onSave(parseNames(text));
  };

  return (
    <form className="form-row" onSubmit={submit}>
      <label className="field field-wide">
        <span>{label}</span>
        <input
          className="form-control"
          spellCheck={false}
          autoCapitalize="none"
          value={text}
          onChange={(event) => {
            setText(event.target.value);
          }}
        />
      </label>
      <button type="submit" className="btn btn-primary" disabled={busy}>
        <i className="bi bi-check-lg" aria-hidden="true" /> Save
      </button>
      <button type="button" className="btn btn-outline-secondary" onClick={onCancel}>
        Cancel
      </button>
    </form>
  );
}
