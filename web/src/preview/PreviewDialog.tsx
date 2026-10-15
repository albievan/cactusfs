import { useEffect, useId, useRef, useState, type KeyboardEvent, type MouseEvent } from 'react';
import { createPortal } from 'react-dom';

import { ApiError, objectDownloadUrl } from '../api/client';
import { countOf } from '../deletions/state';
import { Loading } from '../ui/Loading';
import { reasonOf } from '../ui/reason';
import { openPreview, PREVIEW_MAX_BYTES, PreviewTooLargeError, readBlob } from './fetch';
import { imageTypeOf, rendererOf } from './kinds';
import { PdfPreview } from './PdfPreview';
import { CSV_COLUMNS, decodeText, parseCsv, type CsvTable } from './text';

/** What the dialog shows. */
type View =
  | { state: 'loading' }
  | { state: 'image'; url: string }
  | { state: 'pdf'; blob: Blob }
  | { state: 'text'; text: string; truncated: boolean }
  | { state: 'csv'; table: CsvTable; truncated: boolean }
  // A kind the console cannot draw yet, or one it does not know.
  | { state: 'later' }
  // More bytes than the console takes for a preview.
  | { state: 'too-large' }
  | { state: 'refused'; error: unknown; retryAfter: number | null }
  // The answer began and its bytes could not be read to the end.
  | { state: 'cut-off'; error: unknown };

/** What is said of a refusal, and whether asking again may help. */
interface Refusal {
  message: string;
  retry: boolean;
}

/**
 * Words the API's answer. It decides nothing: whether the object may be
 * previewed, and as what, is what the API said.
 */
function refusalOf(error: unknown, retryAfter: number | null): Refusal {
  if (!(error instanceof ApiError) || error.problem.status === 0) {
    return { message: 'The server could not be reached. Try again.', retry: true };
  }
  switch (error.problem.status) {
    case 401:
      return { message: 'Your session has ended.', retry: false };
    case 403:
      return { message: 'You neither own this object nor may preview it.', retry: false };
    case 404:
      return { message: 'The object is not there any more: it was deleted, or its bucket was.', retry: false };
    case 409:
      return {
        message:
          'This object is encrypted with a key of its own (SSE-C), which the console never holds: it can be neither previewed nor downloaded here. Download it with an S3 client that sends its customer key.',
        retry: false,
      };
    case 413:
      return { message: 'This object is too large to preview.', retry: false };
    case 415:
      return {
        message: 'This object is not of a kind the console can show, or its type and its contents do not agree.',
        retry: false,
      };
    case 423:
      return { message: 'Your session is locked: nothing is previewed until it is unlocked.', retry: false };
    case 429:
      return {
        message:
          retryAfter === null
            ? 'Too many previews are being shown at once. Try again in a moment.'
            : `Too many previews are being shown at once. Try again in ${String(retryAfter)} ${retryAfter === 1 ? 'second' : 'seconds'}.`,
        retry: true,
      };
    default:
      return { message: `The preview could not be loaded: ${reasonOf(error, 'the request failed')}.`, retry: true };
  }
}

// What can have the keyboard inside the dialog, in order.
const FOCUSABLE = 'a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])';

interface PreviewDialogProps {
  /** The bucket the object is in. */
  bucket: string;
  /** The key of the object. */
  objectKey: string;
  /** Closes the dialog. Whoever opened it puts the keyboard back. */
  onClose: () => void;
}

/**
 * The preview of the current version of an object, in a modal dialog over
 * the whole console - the whole screen on a phone.
 *
 * The bytes are asked for with the generated client when it opens, and the
 * request - its body included - is given up when it closes. What they are
 * drawn as is chosen by `X-CactusFS-Preview-Kind` alone, never by the key or
 * the stored type: an image from a blob address typed as the raster kind the
 * API named (never SVG), revoked when the dialog closes; a PDF on a canvas,
 * a page at a time (PdfPreview); text and JSON as text; CSV as a table of
 * text. Nothing of an object is ever markup.
 *
 * Who may preview is the API's to decide: a refusal is said in words, and
 * the download is beside it whatever the answer.
 *
 * While it is open the rest of the page is inert, the keyboard goes round
 * inside it, and Escape closes it.
 */
export function PreviewDialog({ bucket, objectKey, onClose }: PreviewDialogProps) {
  const titleId = useId();
  const backdrop = useRef<HTMLDivElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const [view, setView] = useState<View>({ state: 'loading' });
  // How often it was asked for again.
  const [attempt, setAttempt] = useState(0);
  // Whether an image is shown at its own size rather than fitted.
  const [actualSize, setActualSize] = useState(false);
  const [imageFailed, setImageFailed] = useState(false);

  // Asks for the preview, reads the bytes when they are of a kind that is
  // drawn, and gives everything up when the dialog closes: the request is
  // aborted and the image's address revoked.
  useEffect(() => {
    const stop = new AbortController();
    let current = true;
    let url: string | null = null;
    const show = (next: View) => {
      if (current) {
        setView(next);
      }
    };
    const load = async () => {
      const answer = await openPreview(bucket, objectKey, stop.signal);
      if (!answer.ok) {
        show({ state: 'refused', error: answer.error, retryAfter: answer.retryAfter });
        return;
      }
      const { kind, truncated, length, body } = answer;
      if (kind === null || rendererOf(kind) === 'later') {
        // Not read: the download is offered instead.
        void body?.cancel().catch(() => undefined);
        show({ state: 'later' });
        return;
      }
      if (length !== null && length > PREVIEW_MAX_BYTES) {
        void body?.cancel().catch(() => undefined);
        show({ state: 'too-large' });
        return;
      }
      let blob: Blob;
      try {
        blob = await readBlob(body, stop.signal);
      } catch (error: unknown) {
        show(error instanceof PreviewTooLargeError ? { state: 'too-large' } : { state: 'cut-off', error });
        return;
      }
      if (!current) {
        return;
      }
      switch (rendererOf(kind)) {
        case 'image':
          // The same bytes, typed as the raster kind the API named.
          url = URL.createObjectURL(blob.slice(0, blob.size, imageTypeOf(kind) ?? 'application/octet-stream'));
          show({ state: 'image', url });
          return;
        case 'pdf':
          // Drawn by pdf.js, loaded only now, from the bytes read here.
          show({ state: 'pdf', blob });
          return;
        case 'csv': {
          const text = decodeText(await blob.arrayBuffer(), truncated);
          show({ state: 'csv', table: parseCsv(text, truncated), truncated });
          return;
        }
        default:
          show({ state: 'text', text: decodeText(await blob.arrayBuffer(), truncated), truncated });
      }
    };
    // Rejects when the dialog was closed meanwhile, and is then heard by
    // nobody; anything else is said rather than left loading.
    load().catch((error: unknown) => {
      show({ state: 'cut-off', error });
    });
    return () => {
      current = false;
      stop.abort();
      if (url !== null) {
        URL.revokeObjectURL(url);
      }
    };
  }, [bucket, objectKey, attempt]);

  // The latest onClose, for the listener on the document.
  const closing = useRef(onClose);
  useEffect(() => {
    closing.current = onClose;
  }, [onClose]);

  // While open: the keyboard is in the dialog, and the rest of the page is
  // inert - neither clicked, focused nor read out. When it closes, what was
  // made inert here is given back.
  useEffect(() => {
    const own = backdrop.current;
    const made: Element[] = [];
    if (own !== null) {
      for (const sibling of Array.from(document.body.children)) {
        if (sibling !== own && !sibling.hasAttribute('inert')) {
          sibling.setAttribute('inert', '');
          made.push(sibling);
        }
      }
    }
    // The page behind does not scroll; what it had is given back.
    const overflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    closeButton.current?.focus();
    // Whatever takes the keyboard out of the dialog gives it back.
    const keepFocus = (event: FocusEvent) => {
      const inside = dialog.current;
      if (inside !== null && event.target instanceof Node && !inside.contains(event.target)) {
        closeButton.current?.focus();
      }
    };
    document.addEventListener('focusin', keepFocus);
    // Escape with the keyboard nowhere - after a click on text in the
    // dialog, say - closes it too. Inside the dialog it is heard there first.
    const escape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape' && !event.defaultPrevented) {
        event.preventDefault();
        closing.current();
      }
    };
    document.addEventListener('keydown', escape);
    return () => {
      document.removeEventListener('focusin', keepFocus);
      document.removeEventListener('keydown', escape);
      document.body.style.overflow = overflow;
      for (const element of made) {
        element.removeAttribute('inert');
      }
    };
  }, []);

  // Whether the pointer went down on the dimmed page beside the dialog: a
  // selection begun in the dialog and let go beside it closes nothing.
  const downBeside = useRef(false);

  const again = () => {
    setView({ state: 'loading' });
    setImageFailed(false);
    setAttempt((n) => n + 1);
  };

  // Escape closes; Tab goes round the dialog's own controls.
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape') {
      // Heard by nothing behind it: the details of the row would close too.
      event.preventDefault();
      event.stopPropagation();
      onClose();
      return;
    }
    if (event.key !== 'Tab' || dialog.current === null) {
      return;
    }
    const controls = Array.from(dialog.current.querySelectorAll<HTMLElement>(FOCUSABLE));
    const first = controls[0];
    const last = controls.at(-1);
    if (first === undefined || last === undefined) {
      return;
    }
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  // A click on the dimmed page beside the dialog closes it: pressed and let
  // go there, both.
  const onBackdropMouseDown = (event: MouseEvent<HTMLDivElement>) => {
    downBeside.current = event.target === event.currentTarget;
  };
  const onBackdropClick = (event: MouseEvent<HTMLDivElement>) => {
    const beside = downBeside.current && event.target === event.currentTarget;
    downBeside.current = false;
    if (beside) {
      onClose();
    }
  };
  // An object encrypted with its customer key cannot be downloaded here either.
  const withDownload = !(
    view.state === 'refused' &&
    view.error instanceof ApiError &&
    view.error.problem.status === 409
  );

  return createPortal(
    // The keys are heard on the dialog; the backdrop only takes the click beside it.
    <div ref={backdrop} className="preview-backdrop" onMouseDown={onBackdropMouseDown} onClick={onBackdropClick}>
      <div
        ref={dialog}
        className="preview-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        // A click on its text puts the keyboard on the dialog, not on the page.
        tabIndex={-1}
        onKeyDown={onKeyDown}
      >
        <header className="preview-head">
          <h2 id={titleId} className="preview-title">
            <i className="bi bi-eye" aria-hidden="true" /> Preview of {objectKey}
          </h2>
          {view.state === 'image' && !imageFailed ? (
            <button
              type="button"
              className="btn btn-outline-secondary btn-sm"
              aria-pressed={actualSize}
              onClick={() => {
                setActualSize((on) => !on);
              }}
              title={actualSize ? 'Fit the image to the window' : 'Show the image at its own size'}
            >
              <i className={actualSize ? 'bi bi-arrows-angle-contract' : 'bi bi-arrows-angle-expand'} aria-hidden="true" />{' '}
              <span className="btn-text">Actual size</span>
            </button>
          ) : null}
        </header>
        <div className="preview-body">
          <PreviewBody
            view={view}
            objectKey={objectKey}
            actualSize={actualSize}
            imageFailed={imageFailed}
            onImageFailed={() => {
              setImageFailed(true);
            }}
            onAgain={again}
          />
        </div>
        <footer className="preview-foot">
          {withDownload ? (
            <a
              className="btn btn-outline-secondary"
              href={objectDownloadUrl(bucket, objectKey)}
              download
              title="Download this object"
            >
              <i className="bi bi-download" aria-hidden="true" /> <span className="btn-text">Download</span>
            </a>
          ) : null}
          <button ref={closeButton} type="button" className="btn btn-primary" onClick={onClose} title="Close the preview">
            <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Close</span>
          </button>
        </footer>
      </div>
    </div>,
    document.body,
  );
}

interface PreviewBodyProps {
  view: View;
  objectKey: string;
  actualSize: boolean;
  imageFailed: boolean;
  onImageFailed: () => void;
  onAgain: () => void;
}

/** What the dialog shows of the object, or why it shows nothing. */
function PreviewBody({ view, objectKey, actualSize, imageFailed, onImageFailed, onAgain }: PreviewBodyProps) {
  switch (view.state) {
    case 'loading':
      return <Loading />;
    case 'image':
      if (imageFailed) {
        return <PreviewProblem message="The image could not be shown: the browser could not read it." />;
      }
      return (
        // Scrolled by the keyboard at its own size: it can have it.
        <div
          className={`preview-image-frame${actualSize ? ' preview-image-actual' : ''}`}
          tabIndex={actualSize ? 0 : -1}
          role="group"
          aria-label="Image"
        >
          <img className="preview-image" src={view.url} alt={`The image ${objectKey}`} onError={onImageFailed} />
        </div>
      );
    case 'text':
      return (
        <>
          {view.truncated ? <TruncatedNote /> : null}
          <pre className="preview-text" tabIndex={0} aria-label={`Contents of ${objectKey}`}>
            {view.text}
          </pre>
        </>
      );
    case 'pdf':
      return <PdfPreview blob={view.blob} objectKey={objectKey} />;
    case 'csv':
      return <CsvPreview table={view.table} truncated={view.truncated} objectKey={objectKey} />;
    case 'later':
      return (
        <div className="preview-later">
          <p className="preview-later-title">
            <i className="bi bi-file-earmark" aria-hidden="true" /> This kind of file cannot be shown here yet.
          </p>
          <p>Download it to open it.</p>
        </div>
      );
    case 'too-large':
      return (
        <PreviewProblem
          message={`This object is larger than the ${String(PREVIEW_MAX_BYTES / (1024 * 1024))} MiB the console takes for a preview. Download it to see it.`}
        />
      );
    case 'refused': {
      const refusal = refusalOf(view.error, view.retryAfter);
      return <PreviewProblem message={refusal.message} onAgain={refusal.retry ? onAgain : undefined} />;
    }
    case 'cut-off':
      return (
        <PreviewProblem
          message={`The preview could not be read to the end: ${reasonOf(view.error, 'the transfer was cut off')}.`}
          onAgain={onAgain}
        />
      );
  }
}

/** Why nothing is shown, and a way to ask again where it may help. */
function PreviewProblem({ message, onAgain }: { message: string; onAgain?: () => void }) {
  return (
    <div className="preview-problem">
      <div className="alert alert-danger notice" role="alert">
        <p className="notice-title">{message}</p>
      </div>
      {onAgain === undefined ? null : (
        <button type="button" className="btn btn-outline-secondary" onClick={onAgain} title="Ask for the preview again">
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Try again</span>
        </button>
      )}
    </div>
  );
}

/** Said above a text that is only the first part of the object. */
function TruncatedNote() {
  return (
    <p className="preview-note" role="note">
      <i className="bi bi-scissors" aria-hidden="true" /> The first 1 MiB is shown: the object is longer. Download it to
      see all of it.
    </p>
  );
}

/** A CSV object as a table: every cell is text. */
function CsvPreview({ table, truncated, objectKey }: { table: CsvTable; truncated: boolean; objectKey: string }) {
  const { rows, moreRows, moreColumns } = table;
  return (
    <>
      {truncated ? <TruncatedNote /> : null}
      {moreRows ? (
        <p className="preview-note" role="note">
          Only the first {countOf(rows.length)} {rows.length === 1 ? 'row is' : 'rows are'} shown.
        </p>
      ) : null}
      {moreColumns ? (
        <p className="preview-note" role="note">
          Only the first {countOf(CSV_COLUMNS)} columns of a row are shown.
        </p>
      ) : null}
      {rows.length === 0 ? (
        <p>There is nothing in it.</p>
      ) : (
        <div className="preview-table" tabIndex={0} role="group" aria-label={`Contents of ${objectKey}`}>
          <table className="table table-sm table-bordered preview-csv">
            <tbody>
              {rows.map((row, r) => (
                // Rows have no key of their own: their place is what they are.
                <tr key={r}>
                  {row.map((cell, c) => (
                    <td key={c}>{cell}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
