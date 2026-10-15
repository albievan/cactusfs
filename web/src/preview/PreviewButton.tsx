import { useEffect, useRef, useState } from 'react';

import { offersPreview } from './kinds';
import { PreviewDialog } from './PreviewDialog';

interface PreviewButtonProps {
  /** The bucket the object is in. */
  bucket: string;
  /** The key of the object. */
  objectKey: string;
  /** The type the object was stored with, as the API told it. */
  contentType: string;
}

/**
 * "Preview": offered for an object whose stored type is one the API may
 * preview, and nothing for another. It opens the preview dialog; when that
 * closes, the keyboard is back on the button. Whether the object is shown is
 * the API's answer, not this button's.
 */
export function PreviewButton({ bucket, objectKey, contentType }: PreviewButtonProps) {
  const [open, setOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  // Whether the dialog was open and closed: the keyboard goes back here once
  // it is gone and the page is no longer inert.
  const opened = useRef(false);
  useEffect(() => {
    if (open) {
      opened.current = true;
    } else if (opened.current) {
      opened.current = false;
      button.current?.focus();
    }
  }, [open]);

  if (!offersPreview(contentType)) {
    return null;
  }
  return (
    <>
      <button
        ref={button}
        type="button"
        className="btn btn-outline-secondary"
        aria-haspopup="dialog"
        onClick={() => {
          setOpen(true);
        }}
        title="Show what this object holds, here in the console"
      >
        <i className="bi bi-eye" aria-hidden="true" /> <span className="btn-text">Preview</span>
      </button>
      {open ? (
        <PreviewDialog
          bucket={bucket}
          objectKey={objectKey}
          onClose={() => {
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}
