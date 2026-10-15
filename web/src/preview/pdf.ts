import type { DocumentInitParameters } from 'pdfjs-dist/types/src/display/api';

import { pdfjsAssetDirectory } from './pdfjs-assets';

/**
 * How a PDF is opened (ADR-0028): from the bytes the dialog has already read,
 * never from an address, a range or a stream, so that pdf.js fetches nothing
 * by itself; with no address of standard fonts, character maps or colour
 * profiles, so that none of them is fetched or shipped (a font a PDF does not
 * embed falls back to one of the system's); without WebAssembly, which the
 * console's CSP refuses (no `wasm-unsafe-eval`) - its decoders of scanned
 * and JPEG 2000 images are then the two plain-JavaScript fallbacks at
 * `wasmUrl` (openPdf); without XFA forms; and with no image inside a page
 * larger than an image the API previews (40 MP).
 *
 * `enableScripting` and `isEvalSupported` are not options of the pdf.js
 * pinned (6.4): it evaluates nothing, and scripting belongs to its viewer's
 * annotation layer, which the console does not use. They are given anyway,
 * off, so that a later release which reads them again finds them off. The
 * console's CSP refuses `eval` whatever a release does.
 */
export const PDF_OPTIONS = {
  useWasm: false,
  enableScripting: false,
  isEvalSupported: false,
  enableXfa: false,
  maxImageSize: 40_000_000,
  // Errors only: what a damaged PDF makes pdf.js warn of is not the console's to log.
  verbosity: 0,
} as const satisfies DocumentInitParameters & { enableScripting: false; isEvalSupported: false };

/** The most pixels a page is drawn with: larger pages are drawn smaller. */
export const PDF_MAX_PIXELS = 16_000_000;

/** The most pixels a side of a page's canvas has: what every browser draws. */
export const PDF_MAX_SIDE = 16_384;

/** CSS pixels to a PDF point: a page at 100 % is as large as on paper. */
const CSS_PER_POINT = 96 / 72;

/** The scale a page is drawn at before the caps: as on paper, sharp on this screen. */
export function wantedScale(devicePixelRatio: number): number {
  const ratio = Number.isFinite(devicePixelRatio) && devicePixelRatio > 0 ? devicePixelRatio : 1;
  return CSS_PER_POINT * ratio;
}

/** A page's canvas: the scale it is drawn at and its size in whole pixels. */
export interface PageSize {
  scale: number;
  width: number;
  height: number;
}

/**
 * The size a page of `width` × `height` (at scale 1) is drawn at: `wanted`,
 * or less, so that its canvas has at most PDF_MAX_PIXELS pixels and
 * PDF_MAX_SIDE a side. Null for a page with no size to draw.
 */
export function pageSize(width: number, height: number, wanted: number): PageSize | null {
  if (!(Number.isFinite(width) && Number.isFinite(height) && width > 0 && height > 0 && wanted > 0)) {
    return null;
  }
  const scale = Math.min(
    wanted,
    Math.sqrt(PDF_MAX_PIXELS / (width * height)),
    PDF_MAX_SIDE / width,
    PDF_MAX_SIDE / height,
  );
  // Rounded down, so the caps hold; never no pixel at all.
  return {
    scale,
    width: Math.max(1, Math.floor(width * scale)),
    height: Math.max(1, Math.floor(height * scale)),
  };
}

/** Why pdf.js cannot be used here. */
export type PdfViewerProblem = 'browser' | 'load';

/**
 * pdf.js cannot be used: this browser lacks what it needs (`browser`), or its
 * code or its worker could not be loaded or started (`load`).
 */
export class PdfViewerUnavailableError extends Error {
  readonly problem: PdfViewerProblem;

  constructor(problem: PdfViewerProblem, cause: unknown) {
    super(
      problem === 'browser' ? 'this browser is too old to show PDFs here' : 'the PDF viewer could not be loaded',
      { cause },
    );
    this.name = 'PdfViewerUnavailableError';
    this.problem = problem;
  }
}

/**
 * What pdf.js 6.4 calls of the language and the platform and does not bring
 * with it: a browser without one of them cannot draw a PDF, and fails
 * somewhere in the middle of one. Each is [where, its name].
 */
const BUILT_INS: readonly (readonly [(g: typeof globalThis) => unknown, string])[] = [
  [(g) => g.Map?.prototype, 'getOrInsertComputed'],
  [(g) => g.Map?.prototype, 'getOrInsert'],
  [(g) => g.Set?.prototype, 'intersection'],
  [(g) => g.Math, 'sumPrecise'],
  [(g) => g.Uint8Array, 'fromBase64'],
  [(g) => g.Uint8Array?.prototype, 'toBase64'],
  [(g) => g.Uint8Array?.prototype, 'toHex'],
  [(g) => g.ArrayBuffer?.prototype, 'transferToFixedLength'],
  [(g) => g.Promise, 'try'],
  [(g) => g.Promise, 'withResolvers'],
  [(g) => g.URL, 'parse'],
  // Iterator helpers; pdf.js also reads the global Iterator when it loads.
  [(g) => (Reflect.get(g, 'Iterator') as { prototype?: unknown } | undefined)?.prototype, 'filter'],
  [(g) => (Reflect.get(g, 'Iterator') as { prototype?: unknown } | undefined)?.prototype, 'find'],
  [(g) => (Reflect.get(g, 'Iterator') as { prototype?: unknown } | undefined)?.prototype, 'some'],
  [(g) => (Reflect.get(g, 'Iterator') as { prototype?: unknown } | undefined)?.prototype, 'toArray'],
];

/** The built-ins pdf.js needs that `g` lacks, by name; none in a browser that can show a PDF. */
export function missingBuiltIns(g: typeof globalThis = globalThis): string[] {
  const missing: string[] = [];
  for (const [where, name] of BUILT_INS) {
    const holder = where(g) as Record<string, unknown> | undefined;
    if (typeof holder?.[name] !== 'function') {
      missing.push(name);
    }
  }
  return missing;
}

/** pdf.js, once loaded. */
export type Pdfjs = typeof import('pdfjs-dist');

/**
 * Loads pdf.js - its own chunk, apart from the console's - and the address
 * of its worker, a file of the console's own origin (worker-src 'self').
 * Nothing of it is loaded until a PDF is shown.
 */
async function loadPdfjs(): Promise<{ lib: Pdfjs; workerSrc: string }> {
  try {
    const [lib, worker] = await Promise.all([
      import('pdfjs-dist'),
      import('pdfjs-dist/build/pdf.worker.min.mjs?url'),
    ]);
    return { lib, workerSrc: worker.default };
  } catch (error: unknown) {
    throw new PdfViewerUnavailableError('load', error);
  }
}

/**
 * The address pdf.js imports its two image decoders from (PDFJS_FALLBACKS):
 * the directory of this version of pdf.js in the console's own assets.
 */
export function fallbackUrl(version: string, page: string = window.location.href): string {
  return new URL(`${import.meta.env.BASE_URL}${pdfjsAssetDirectory(version)}`, page).href;
}

/** A PDF being opened. */
export interface PdfOpening {
  /** pdf.js. */
  lib: Pdfjs;
  /** The document being opened. */
  task: ReturnType<Pdfjs['getDocument']>;
  /**
   * Rejects with a PdfViewerUnavailableError when the worker cannot run;
   * never resolves. Raced with whatever waits on the worker, which would
   * otherwise wait for ever.
   */
  failed: Promise<never>;
  /** Ends the worker's thread at once, and gives the document and the worker up. */
  close: () => Promise<void>;
}

/**
 * Begins to open a PDF from its bytes with PDF_OPTIONS, in a worker of its
 * own: a module worker of the console's origin, made here so that pdf.js
 * never falls back to parsing on the page's own thread. `data` is given to
 * the worker: it is unusable here afterwards.
 *
 * Rejects with a PdfViewerUnavailableError when the browser lacks what
 * pdf.js needs - before anything of it is loaded - or pdf.js cannot be
 * loaded.
 */
export async function openPdf(data: Uint8Array, g: typeof globalThis = globalThis): Promise<PdfOpening> {
  const missing = missingBuiltIns(g);
  if (missing.length > 0) {
    throw new PdfViewerUnavailableError('browser', new Error(`missing: ${missing.join(', ')}`));
  }
  const { lib, workerSrc } = await loadPdfjs();
  let thread: Worker;
  try {
    thread = new Worker(workerSrc, { type: 'module', name: 'pdf.js' });
  } catch (error: unknown) {
    throw new PdfViewerUnavailableError('load', error);
  }
  const failed = new Promise<never>((_, reject) => {
    thread.addEventListener(
      'error',
      (event) => {
        reject(new PdfViewerUnavailableError('load', event));
      },
      { once: true },
    );
  });
  // Heard by whoever races it; a failure nobody waits for is no error.
  failed.catch(() => undefined);
  let worker: InstanceType<Pdfjs['PDFWorker']>;
  try {
    worker = lib.PDFWorker.create({ port: thread, verbosity: PDF_OPTIONS.verbosity });
  } catch (error: unknown) {
    thread.terminate();
    throw new PdfViewerUnavailableError('load', error);
  }
  let task: PdfOpening['task'];
  try {
    task = lib.getDocument({ ...PDF_OPTIONS, wasmUrl: fallbackUrl(lib.version), data, worker });
  } catch (error: unknown) {
    worker.destroy();
    thread.terminate();
    throw new PdfViewerUnavailableError('load', error);
  }
  // The thread is ended first: pdf.js's own destroy waits on the worker's
  // answers, which a worker that never loaded, or one deep in a long
  // decode, never gives.
  const close = () => {
    thread.terminate();
    worker.destroy();
    void task.destroy().catch(() => undefined);
    return Promise.resolve();
  };
  return { lib, task, failed, close };
}
