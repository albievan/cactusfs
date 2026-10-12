import { memo } from 'react';

import { inFlight, type Upload, type UploadState } from './state';

const UPLOAD_ICON: Record<UploadState, string> = {
  waiting: 'bi-hourglass',
  sending: 'bi-arrow-up-circle',
  stored: 'bi-check-circle',
  failed: 'bi-x-circle',
  cancelled: 'bi-slash-circle',
};

const UPLOAD_WORD: Record<UploadState, string> = {
  waiting: 'Waiting',
  sending: 'Uploading…',
  stored: 'Stored',
  failed: 'Not stored',
  cancelled: 'Cancelled',
};

/** What stands on the tile of a file nobody knows the end of: not that it is not stored. */
const UNDECIDED_WORD = 'Not confirmed';

interface UploadTileProps {
  upload: Upload;
  /** What the bar is called, for whoever has it read: it names the file. */
  barLabel: string;
  /** Stops the upload. */
  onCancel: (id: string) => void;
}

/**
 * One file of an upload: its key and how it stands on one line - in a sign
 * and in a word, not by colour alone - its bar beneath, why it was not
 * stored when it was not, and while it is on its way a button that stops it.
 *
 * It is drawn again only when its file changes: the others of a long list
 * are left as they are while one of them progresses.
 */
export const UploadTile = memo(function UploadTile({ upload, barLabel, onCancel }: UploadTileProps) {
  const undecided = upload.state === 'failed' && upload.undecided === true;
  return (
    <li className={undecided ? 'upload upload-failed upload-undecided' : `upload upload-${upload.state}`}>
      <div className="upload-head">
        <i className={`bi ${undecided ? 'bi-question-circle' : UPLOAD_ICON[upload.state]}`} aria-hidden="true" />
        <span className="upload-name">{upload.key}</span>
        <span className="upload-state">
          {upload.state === 'sending' && upload.total > 0
            ? `${String(Math.floor((upload.sent / upload.total) * 100))} %`
            : undecided
              ? UNDECIDED_WORD
              : UPLOAD_WORD[upload.state]}
        </span>
        {inFlight(upload) ? (
          <button
            type="button"
            className="btn btn-outline-secondary btn-sm upload-cancel"
            aria-label={`Cancel the upload of ${upload.key}`}
            onClick={() => {
              onCancel(upload.id);
            }}
            title="Stop this upload. The file may not be stored: if the server had all of it already, it is"
          >
            <i className="bi bi-x-lg" aria-hidden="true" />
          </button>
        ) : null}
      </div>
      <progress
        className="upload-bar"
        aria-label={barLabel}
        value={upload.state === 'stored' ? 1 : upload.total > 0 ? Math.min(upload.sent / upload.total, 1) : 0}
        max={1}
      />
      {upload.reason === undefined ? null : <p className="upload-reason">{upload.reason}</p>}
    </li>
  );
});
