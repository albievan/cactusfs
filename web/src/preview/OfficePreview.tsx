import { Component, useEffect, useState, type ReactNode } from 'react';

import { DocxPreview, makeImageUrls, revokeImageUrls } from './DocxPreview';
import { readOffice, type OfficeProblem, type OfficeReading } from './office';
import { OOXML_TIMEOUT_MS, type OoxmlKind, type OoxmlModel } from './ooxml-model';
import { XlsxPreview } from './XlsxPreview';

/** The document as it is read. */
type Read =
  | { state: 'reading' }
  // With the blob addresses of a document's images, by their place.
  | { state: 'read'; model: OoxmlModel; urls: (string | null)[] }
  | { state: 'failed'; problem: OfficeProblem };

/** What is said when a document is not shown: the kind of failure, never its detail. */
function failure(problem: OfficeProblem, kind: OoxmlKind): string {
  switch (problem) {
    case 'too-large':
      return 'This document is too large to preview: it unpacks to more than the console reads. Download it to open it.';
    case 'unsafe':
      return 'This document declares a DOCTYPE or entities, which the console never reads. Download it to open it elsewhere.';
    case 'damaged':
      return `This document could not be read: it is damaged, or it is not ${kind === 'docx' ? 'a Word document' : 'an Excel workbook'}. Download it to open it elsewhere.`;
    case 'timeout':
      return `Reading this document took more than ${String(OOXML_TIMEOUT_MS / 1000)} seconds, and was stopped. Download it to open it.`;
    case 'load':
      return 'The document reader could not be loaded. Reload the console and try again.';
  }
}

/** Said when a document that was read cannot be drawn. */
const NOT_DRAWN = 'This document could not be shown. Download it to open it elsewhere.';

/**
 * Keeps a document that cannot be drawn from taking the dialog with it: the
 * failure is said in its place, and the dialog - its download and its
 * close - stays.
 */
export class OfficeBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  override state = { failed: false };

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }

  override render() {
    if (this.state.failed) {
      return (
        <div className="alert alert-danger notice" role="alert">
          <p className="notice-title">{NOT_DRAWN}</p>
        </div>
      );
    }
    return this.props.children;
  }
}

interface OfficePreviewProps {
  /** The bytes of the document, as the dialog read them. */
  blob: Blob;
  /** What the API said it is. */
  kind: OoxmlKind;
  /** The key of the object, for what is read out. */
  objectKey: string;
}

/**
 * A DOCX or XLSX, read in a worker of its own (readOffice) and shown as
 * React elements and text nodes. The worker is ended when it has answered,
 * after OOXML_TIMEOUT_MS, or at once when the preview closes; the blob
 * addresses of a document's images are revoked when it closes.
 */
export function OfficePreview({ blob, kind, objectKey }: OfficePreviewProps) {
  const [read, setRead] = useState<Read>({ state: 'reading' });

  useEffect(() => {
    let current = true;
    let reading: OfficeReading | null = null;
    let urls: (string | null)[] = [];
    const start = async () => {
      const bytes = await blob.arrayBuffer();
      if (!current) {
        return;
      }
      reading = readOffice(kind, bytes);
      const outcome = await reading.outcome;
      if (!current) {
        return;
      }
      if (!outcome.ok) {
        setRead({ state: 'failed', problem: outcome.problem });
        return;
      }
      urls = outcome.model.kind === 'docx' ? makeImageUrls(outcome.model) : [];
      setRead({ state: 'read', model: outcome.model, urls });
    };
    start().catch(() => {
      if (current) {
        setRead({ state: 'failed', problem: 'damaged' });
      }
    });
    return () => {
      current = false;
      (reading as OfficeReading | null)?.close();
      revokeImageUrls(urls);
    };
  }, [blob, kind]);

  switch (read.state) {
    case 'reading':
      return (
        <p className="loading" role="status">
          <span className="spinner-border spinner-border-sm" aria-hidden="true" /> Reading the document…
        </p>
      );
    case 'failed':
      return (
        <div className="alert alert-danger notice" role="alert">
          <p className="notice-title">{failure(read.problem, kind)}</p>
        </div>
      );
    case 'read':
      return (
        <OfficeBoundary>
          {read.model.kind === 'docx' ? (
            <DocxPreview model={read.model} urls={read.urls} objectKey={objectKey} />
          ) : (
            <XlsxPreview model={read.model} objectKey={objectKey} />
          )}
        </OfficeBoundary>
      );
  }
}
