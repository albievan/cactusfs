import { useEffect, useRef, useState } from 'react';

import { countOf } from '../deletions/state';
import { openPdf, pageSize, PdfViewerUnavailableError, wantedScale, type PdfOpening } from './pdf';

/** What pdf.js gives for an opened document: what the preview uses of it. */
type PdfDocument = Awaited<PdfOpening['task']['promise']>;

/** A page of it. */
type PdfPage = Awaited<ReturnType<PdfDocument['getPage']>>;

/** The document as it is opened. */
type Opened =
  | { state: 'opening' }
  | { state: 'open'; opening: PdfOpening; pdf: PdfDocument; pages: number }
  | { state: 'failed'; message: string };

/** A page whose drawing has ended: drawn, or failed with what is said of it. */
interface Drawn {
  pdf: PdfDocument;
  page: number;
  failure: string | null;
}

/** What the viewer says when it stops working: the worker could not run, or no longer runs. */
const VIEWER_UNAVAILABLE = 'The PDF viewer could not be loaded. Reload the console and try again.';

/** What is said when a PDF cannot be opened: the kind of failure, never its text. */
function openingFailure(error: unknown): string {
  if (error instanceof PdfViewerUnavailableError) {
    return error.problem === 'browser'
      ? 'This browser is too old to show PDFs here. Download the PDF to open it.'
      : VIEWER_UNAVAILABLE;
  }
  const name = error instanceof Error ? error.name : '';
  if (name === 'PasswordException') {
    return 'This PDF is protected by a password, which the console does not ask for. Download it to open it.';
  }
  return 'This PDF could not be read: it is damaged, or it is not a PDF. Download it to open it elsewhere.';
}

/** Whether pdf.js gave a page's drawing up because it was asked to. */
function cancelled(error: unknown): boolean {
  return error instanceof Error && error.name === 'RenderingCancelledException';
}

interface PdfPreviewProps {
  /** The bytes of the PDF, as the dialog read them. */
  blob: Blob;
  /** The key of the object, for what is read out. */
  objectKey: string;
}

/**
 * A PDF, one page at a time, drawn on a canvas by pdf.js - loaded only now,
 * in a worker of the console's own origin. Only pixels are shown: no text,
 * annotation or form layer, so nothing in the PDF is clicked, followed or
 * run. A page is drawn when it is shown, at most PDF_MAX_PIXELS large; the
 * document, its worker and the worker's thread are given up when the
 * preview closes.
 *
 * The bytes are read out of the Blob into the one copy that is given to the
 * worker: nothing here keeps them.
 */
export function PdfPreview({ blob, objectKey }: PdfPreviewProps) {
  const [opened, setOpened] = useState<Opened>({ state: 'opening' });
  const [page, setPage] = useState(1);
  // The last page whose drawing ended; any other page shown is being drawn.
  const [drawn, setDrawn] = useState<Drawn | null>(null);
  const canvas = useRef<HTMLCanvasElement>(null);

  // Opens the document, and gives it up - its worker with it - when the
  // preview closes, whether it had opened or not.
  useEffect(() => {
    let current = true;
    let held: PdfOpening | null = null;
    const open = async () => {
      const bytes = new Uint8Array(await blob.arrayBuffer());
      if (!current) {
        return;
      }
      const opening = await openPdf(bytes);
      if (!current) {
        await opening.close();
        return;
      }
      held = opening;
      // A worker that cannot run would leave the document opening for ever.
      const pdf = await Promise.race([opening.task.promise, opening.failed]);
      if (current) {
        setPage(1);
        setOpened({ state: 'open', opening, pdf, pages: pdf.numPages });
      }
    };
    open().catch((error: unknown) => {
      if (current) {
        setOpened({ state: 'failed', message: openingFailure(error) });
      }
    });
    return () => {
      current = false;
      void held?.close().catch(() => undefined);
    };
  }, [blob]);

  // Draws the page shown, and gives its drawing up when another is shown or
  // the preview closes.
  useEffect(() => {
    const target = canvas.current;
    if (opened.state !== 'open' || target === null) {
      return;
    }
    let current = true;
    const { pdf, opening } = opened;
    let render: { cancel: () => void } | null = null;
    const draw = async () => {
      let proxy: PdfPage | null = null;
      try {
        proxy = await Promise.race([pdf.getPage(page), opening.failed]);
        if (!current) {
          return;
        }
        const base = proxy.getViewport({ scale: 1 });
        const size = pageSize(base.width, base.height, wantedScale(window.devicePixelRatio));
        if (size === null) {
          throw new Error('the page has no size');
        }
        const viewport = proxy.getViewport({ scale: size.scale });
        target.width = size.width;
        target.height = size.height;
        const task = proxy.render({
          canvas: target,
          viewport,
          // What annotations look like is drawn; nothing of them is made a control.
          annotationMode: opening.lib.AnnotationMode.ENABLE,
        });
        render = task;
        await Promise.race([task.promise, opening.failed]);
        if (current) {
          setDrawn({ pdf, page, failure: null });
        }
      } finally {
        // What pdf.js keeps of the page is let go, drawn or not.
        proxy?.cleanup();
      }
    };
    draw().catch((error: unknown) => {
      if (current && !cancelled(error)) {
        // A worker that stopped is no fault of the page.
        const failure =
          error instanceof PdfViewerUnavailableError
            ? VIEWER_UNAVAILABLE
            : `Page ${countOf(page)} could not be drawn: it is damaged.`;
        setDrawn({ pdf, page, failure });
      }
    });
    return () => {
      current = false;
      render?.cancel();
    };
  }, [opened, page]);

  if (opened.state === 'opening') {
    return (
      <p className="loading" role="status">
        <span className="spinner-border spinner-border-sm" aria-hidden="true" /> Opening the PDF…
      </p>
    );
  }
  if (opened.state === 'failed') {
    return (
      <div className="alert alert-danger notice" role="alert">
        <p className="notice-title">{opened.message}</p>
      </div>
    );
  }
  const { pages } = opened;
  const ended = drawn !== null && drawn.pdf === opened.pdf && drawn.page === page;
  const drawing = !ended ? 'drawing' : drawn.failure !== null ? 'failed' : 'drawn';
  if (pages < 1) {
    return <p>There is no page in it.</p>;
  }
  return (
    <div className="preview-pdf">
      <nav className="preview-pdf-pages" aria-label="Pages">
        <button
          type="button"
          className="btn btn-outline-secondary btn-sm"
          disabled={page <= 1}
          onClick={() => {
            setPage((n) => Math.max(1, n - 1));
          }}
          title="Show the previous page"
        >
          <i className="bi bi-chevron-left" aria-hidden="true" /> <span className="btn-text">Previous</span>
        </button>
        <span className="preview-pdf-count" aria-live="polite">
          Page {countOf(page)} of {countOf(pages)}
        </span>
        <button
          type="button"
          className="btn btn-outline-secondary btn-sm"
          disabled={page >= pages}
          onClick={() => {
            setPage((n) => Math.min(pages, n + 1));
          }}
          title="Show the next page"
        >
          <span className="btn-text">Next</span> <i className="bi bi-chevron-right" aria-hidden="true" />
        </button>
      </nav>
      {drawing === 'failed' ? (
        <div className="alert alert-danger notice" role="alert">
          <p className="notice-title">{drawn?.failure}</p>
        </div>
      ) : null}
      {drawing === 'drawing' ? (
        <p className="loading" role="status">
          <span className="spinner-border spinner-border-sm" aria-hidden="true" /> Drawing page {countOf(page)}…
        </p>
      ) : null}
      <div className="preview-pdf-frame" hidden={drawing === 'failed'}>
        {/* A canvas of its own for each page: a drawing given up never shares one with the next. */}
        <canvas
          key={page}
          ref={canvas}
          className="preview-pdf-page"
          role="img"
          aria-label={`Page ${countOf(page)} of ${objectKey}`}
        />
      </div>
    </div>
  );
}
