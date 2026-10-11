import { useState } from 'react';

interface ConfirmButtonProps {
  /** What the button says, for example "Delete". */
  label: string;
  /** What is about to happen, for example "Delete bucket photos?". */
  question: string;
  /** Carries the change out once it has been confirmed. */
  onConfirm: () => void;
  /** True while a change is being carried out. */
  disabled?: boolean;
}

/** A button for a change that cannot be undone: it asks before it acts. */
export function ConfirmButton({ label, question, onConfirm, disabled = false }: ConfirmButtonProps) {
  const [asking, setAsking] = useState(false);

  if (!asking) {
    return (
      <button
        type="button"
        className="btn btn-outline-danger"
        disabled={disabled}
        onClick={() => {
          setAsking(true);
        }}
      >
        {label === 'Delete' ? <i className="bi bi-trash" aria-hidden="true" /> : null}
        {label}
      </button>
    );
  }
  return (
    <span className="confirm" role="group" aria-label={question}>
      <span className="confirm-question">{question}</span>
      <button
        type="button"
        className="btn btn-outline-danger"
        disabled={disabled}
        onClick={() => {
          setAsking(false);
          onConfirm();
        }}
      >
        Yes, {label.toLowerCase()}
      </button>
      <button
        type="button"
        className="btn btn-outline-secondary"
        onClick={() => {
          setAsking(false);
        }}
      >
        Cancel
      </button>
    </span>
  );
}
