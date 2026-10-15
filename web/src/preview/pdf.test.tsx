import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { fakeApi } from '../test/fakeApi';
import {
  fallbackUrl,
  missingBuiltIns,
  PDF_MAX_PIXELS,
  PDF_MAX_SIDE,
  PDF_OPTIONS,
  pageSize,
  wantedScale,
} from './pdf';
import { PDFJS_FALLBACKS, pdfjsAssetDirectory } from './pdfjs-assets';
import { PreviewButton } from './PreviewButton';

// pdf.js itself draws on a canvas, which jsdom has not got: it is replaced
// here by a fake that records how it was asked. A real PDF is drawn in a
// browser (PLAN O.14). The factories count how often each module is loaded:
// the console must load neither until a PDF is shown.
const pdfjs = vi.hoisted(() => ({
  loads: 0,
  workerLoads: 0,
  getDocument: (() => {
    throw new Error('getDocument is set by each test');
  }) as (options: Record<string, unknown>) => unknown,
  createWorker: (() => {
    throw new Error('createWorker is set by beforeEach');
  }) as (params: Record<string, unknown>) => unknown,
}));

vi.mock('pdfjs-dist', () => {
  pdfjs.loads += 1;
  return {
    version: '6.4.299',
    AnnotationMode: { DISABLE: 0, ENABLE: 1, ENABLE_FORMS: 2, ENABLE_STORAGE: 3 },
    getDocument: (options: Record<string, unknown>) => pdfjs.getDocument(options),
    PDFWorker: { create: (params: Record<string, unknown>) => pdfjs.createWorker(params) },
  };
});

vi.mock('pdfjs-dist/build/pdf.worker.min.mjs?url', () => {
  pdfjs.workerLoads += 1;
  return { default: '/assets/pdf.worker.min-test.mjs' };
});

/** A Web Worker as the console makes one: jsdom has none. */
class FakeWorker {
  readonly url: string;
  readonly options: WorkerOptions | undefined;
  readonly terminate = vi.fn();
  #errors: ((event: Event) => void)[] = [];

  constructor(url: string | URL, options?: WorkerOptions) {
    this.url = String(url);
    this.options = options;
    recorded.threads.push(this);
  }

  addEventListener(type: string, listener: (event: Event) => void) {
    if (type === 'error') {
      this.#errors.push(listener);
    }
  }

  /** The worker's script could not be loaded or run. */
  fail() {
    for (const listener of this.#errors.splice(0)) {
      listener(new Event('error'));
    }
  }
}

/**
 * What pdf.js 6.4 needs of the language and that Node may not have yet:
 * given for the tests, and taken away again after each.
 */
const BUILT_INS: [Record<string, unknown>, string][] = [
  [Map.prototype as unknown as Record<string, unknown>, 'getOrInsertComputed'],
  [Map.prototype as unknown as Record<string, unknown>, 'getOrInsert'],
  [Set.prototype as unknown as Record<string, unknown>, 'intersection'],
  [Math as unknown as Record<string, unknown>, 'sumPrecise'],
  [Uint8Array as unknown as Record<string, unknown>, 'fromBase64'],
  [Uint8Array.prototype as unknown as Record<string, unknown>, 'toBase64'],
  [Uint8Array.prototype as unknown as Record<string, unknown>, 'toHex'],
  [ArrayBuffer.prototype as unknown as Record<string, unknown>, 'transferToFixedLength'],
  [Promise as unknown as Record<string, unknown>, 'try'],
  [Promise as unknown as Record<string, unknown>, 'withResolvers'],
  [URL as unknown as Record<string, unknown>, 'parse'],
];
let givenBuiltIns: [Record<string, unknown>, string][] = [];

const PREVIEW = 'GET /buckets/photos/object/preview';

// The first bytes of a PDF; what follows does not matter to the fake.
const PDF_BYTES = new TextEncoder().encode('%PDF-1.7\n%fake\n');

/** A page of the fake document: its size at scale 1, and how its drawing ends. */
interface FakePage {
  width: number;
  height: number;
  /** The drawing fails with this error. */
  fails?: Error;
  /** The drawing never ends by itself. */
  hangs?: boolean;
}

/** What the fake pdf.js was asked: documents, pages and drawings. */
interface Recorded {
  options: Record<string, unknown>[];
  threads: FakeWorker[];
  pdfWorkers: { port: unknown; verbosity: unknown; destroy: ReturnType<typeof vi.fn> }[];
  destroyed: ReturnType<typeof vi.fn>;
  pagesAsked: number[];
  renders: { page: number; canvas: HTMLCanvasElement; scale: number; annotationMode: unknown; cancel: ReturnType<typeof vi.fn> }[];
  cleanedUp: number[];
}

let recorded: Recorded;

/**
 * Makes pdf.js open a document of these pages - or fail to open with
 * `refusal`, or open only when `opened` resolves.
 */
function fakeDocument(pages: FakePage[], refusal?: Error, opened?: Promise<void>) {
  pdfjs.getDocument = (options) => {
    recorded.options.push(options);
    const document = {
      numPages: pages.length,
      getPage: (n: number) => {
        recorded.pagesAsked.push(n);
        const page = pages[n - 1];
        if (page === undefined) {
          return Promise.reject(new Error(`no page ${String(n)}`));
        }
        return Promise.resolve({
          getViewport: ({ scale }: { scale: number }) => ({
            width: page.width * scale,
            height: page.height * scale,
            scale,
          }),
          render: (params: { canvas: HTMLCanvasElement; viewport: { scale: number }; annotationMode?: unknown }) => {
            let reject: (error: Error) => void = () => undefined;
            const promise = page.hangs
              ? new Promise<void>((_, no) => {
                  reject = no;
                })
              : page.fails
                ? Promise.reject(page.fails)
                : Promise.resolve();
            const cancel = vi.fn(() => {
              const error = new Error('Rendering cancelled');
              error.name = 'RenderingCancelledException';
              reject(error);
            });
            recorded.renders.push({
              page: n,
              canvas: params.canvas,
              scale: params.viewport.scale,
              annotationMode: params.annotationMode,
              cancel,
            });
            return { promise, cancel };
          },
          cleanup: () => {
            recorded.cleanedUp.push(n);
            return true;
          },
        });
      },
    };
    const promise = (async () => {
      await opened;
      if (refusal !== undefined) {
        throw refusal;
      }
      return document;
    })();
    return { promise, destroy: recorded.destroyed };
  };
}

/** An error as pdf.js names its kinds of failure. */
function pdfError(name: string, message: string): Error {
  const error = new Error(message);
  error.name = name;
  return error;
}

/** What the API answers a preview it allows. */
function previewOf(kind: string, body: BodyInit | null): Response {
  return new Response(body, {
    status: 200,
    headers: {
      'Content-Type': 'application/octet-stream',
      'Content-Disposition': 'attachment',
      'X-CactusFS-Preview-Kind': kind,
      'X-CactusFS-Preview-Truncated': 'false',
    },
  });
}

/** Opens the preview of `report.pdf` and returns its dialog. */
async function openPdfPreview(kind = 'pdf', body: BodyInit = PDF_BYTES): Promise<HTMLElement> {
  fakeApi({ [PREVIEW]: () => previewOf(kind, body) });
  render(<PreviewButton bucket="photos" objectKey="report.pdf" contentType="application/pdf" />);
  fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
  return screen.findByRole('dialog', { name: 'Preview of report.pdf' });
}

/** Closes the preview with its button. */
function close(dialog: HTMLElement) {
  fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
}

beforeEach(() => {
  recorded = {
    options: [],
    threads: [],
    pdfWorkers: [],
    destroyed: vi.fn(() => Promise.resolve()),
    pagesAsked: [],
    renders: [],
    cleanedUp: [],
  };
  pdfjs.createWorker = (params) => {
    const worker = { port: params.port, verbosity: params.verbosity, destroy: vi.fn() };
    recorded.pdfWorkers.push(worker);
    return worker;
  };
  vi.stubGlobal('Worker', FakeWorker);
  vi.stubGlobal('devicePixelRatio', 1);
  givenBuiltIns = BUILT_INS.filter(([holder, name]) => typeof holder[name] !== 'function');
  for (const [holder, name] of givenBuiltIns) {
    Object.defineProperty(holder, name, { configurable: true, writable: true, value: () => undefined });
  }
  // jsdom has no blob addresses; the images of other tests need them, a PDF never does.
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, writable: true, value: vi.fn(() => 'blob:x') });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: vi.fn() });
});

afterEach(() => {
  for (const [holder, name] of givenBuiltIns) {
    Reflect.deleteProperty(holder, name);
  }
  Reflect.deleteProperty(URL, 'createObjectURL');
  Reflect.deleteProperty(URL, 'revokeObjectURL');
});

// First in the file: pdf.js is loaded for the first PDF, and not before.
describe('when pdf.js is loaded', () => {
  it.each([
    ['png', new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])],
    ['text', new TextEncoder().encode('%PDF-1.7 as text')],
    ['csv', new TextEncoder().encode('a,b')],
    ['docx', new Uint8Array([0x50, 0x4b, 0x03, 0x04])],
    ['unknown', PDF_BYTES],
  ])('is not loaded for a preview of kind %s, whatever its bytes', async (kind, body) => {
    fakeDocument([{ width: 612, height: 792 }]);
    const dialog = await openPdfPreview(kind, body);
    await waitFor(() => {
      expect(within(dialog).queryByRole('status')).toBeNull();
    });
    expect(pdfjs.loads).toBe(0);
    expect(pdfjs.workerLoads).toBe(0);
    expect(recorded.options).toHaveLength(0);
    expect(dialog.querySelector('canvas')).toBeNull();
    close(dialog);
  });

  it('is not loaded by a browser too old for it, which is told so', async () => {
    // One of what pdf.js 6.4 needs and does not bring.
    const native = Object.getOwnPropertyDescriptor(Math, 'sumPrecise');
    Reflect.deleteProperty(Math, 'sumPrecise');
    try {
      fakeDocument([{ width: 612, height: 792 }]);
      const dialog = await openPdfPreview();
      expect((await within(dialog).findByRole('alert')).textContent).toBe(
        'This browser is too old to show PDFs here. Download the PDF to open it.',
      );
      expect(within(dialog).getByRole('link', { name: 'Download' })).toBeTruthy();
      expect(pdfjs.loads).toBe(0);
      expect(pdfjs.workerLoads).toBe(0);
      expect(recorded.threads).toHaveLength(0);
      close(dialog);
    } finally {
      if (native !== undefined) {
        Object.defineProperty(Math, 'sumPrecise', native);
      }
    }
  });

  it('is loaded, with a worker of its own from the same origin, for a preview of kind pdf', async () => {
    fakeDocument([{ width: 612, height: 792 }]);
    const dialog = await openPdfPreview();
    await within(dialog).findByText('Page 1 of 1');
    expect(pdfjs.loads).toBe(1);
    expect(pdfjs.workerLoads).toBe(1);
    // A module worker made by the console - so pdf.js never parses on the
    // page's own thread - from a file of its own origin: a path, no host.
    expect(recorded.threads).toHaveLength(1);
    expect(recorded.threads[0]?.url).toBe('/assets/pdf.worker.min-test.mjs');
    expect(recorded.threads[0]?.options?.type).toBe('module');
    expect(recorded.pdfWorkers).toHaveLength(1);
    expect(recorded.pdfWorkers[0]?.port).toBe(recorded.threads[0]);
    expect(recorded.pdfWorkers[0]?.verbosity).toBe(0);
    expect(recorded.options[0]?.worker).toBe(recorded.pdfWorkers[0]);
    close(dialog);
  });
});

describe('how a PDF is opened', () => {
  it('passes the bytes read, and nothing that would run, fetch or follow anything', async () => {
    fakeDocument([{ width: 612, height: 792 }]);
    const dialog = await openPdfPreview();
    await within(dialog).findByText('Page 1 of 1');
    expect(recorded.options).toHaveLength(1);
    const [options] = recorded.options;
    expect(options).toMatchObject({
      useWasm: false,
      enableScripting: false,
      isEvalSupported: false,
      enableXfa: false,
    });
    // From the bytes, as a typed array: no address, no range, no stream.
    expect(options?.data).toBeInstanceOf(Uint8Array);
    expect(Array.from(options?.data as Uint8Array)).toEqual(Array.from(PDF_BYTES));
    for (const key of [
      'url',
      'range',
      'httpHeaders',
      'withCredentials',
      'docBaseUrl',
      'cMapUrl',
      'standardFontDataUrl',
      'iccUrl',
    ]) {
      expect(options).not.toHaveProperty(key);
    }
    // The decoders of scanned and JPEG 2000 images come from the console's
    // own assets, in the directory of this version of pdf.js.
    const wasmUrl = new URL(String(options?.wasmUrl));
    expect(wasmUrl.origin).toBe(window.location.origin);
    expect(wasmUrl.pathname).toBe('/assets/pdfjs-6.4.299/');
    close(dialog);
  });

  it('declares no address of fonts, character maps or colour profiles', () => {
    expect(PDF_OPTIONS).toEqual({
      useWasm: false,
      enableScripting: false,
      isEvalSupported: false,
      enableXfa: false,
      maxImageSize: 40_000_000,
      verbosity: 0,
    });
  });

  it('finds the decoders where the build puts them', () => {
    expect(PDFJS_FALLBACKS).toEqual(['jbig2_nowasm_fallback.js', 'openjpeg_nowasm_fallback.js']);
    expect(pdfjsAssetDirectory('6.4.299')).toBe('assets/pdfjs-6.4.299/');
    // pdf.js joins the address and a name as they are: it must end in a slash.
    expect(fallbackUrl('6.4.299', 'https://console.example:9001/buckets/photos')).toBe(
      'https://console.example:9001/assets/pdfjs-6.4.299/',
    );
  });

  it('says the viewer could not be loaded when its worker cannot run, and ends its thread', async () => {
    let open: () => void = () => undefined;
    // pdf.js's destroy waits on a worker that never answers.
    recorded.destroyed = vi.fn(() => new Promise<void>(() => undefined));
    fakeDocument([{ width: 612, height: 792 }], undefined, new Promise<void>((resolve) => (open = resolve)));
    const dialog = await openPdfPreview();
    await waitFor(() => {
      expect(recorded.threads).toHaveLength(1);
    });
    recorded.threads[0]?.fail();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The PDF viewer could not be loaded. Reload the console and try again.',
    );
    close(dialog);
    expect(recorded.destroyed).toHaveBeenCalledTimes(1);
    expect(recorded.threads[0]?.terminate).toHaveBeenCalledTimes(1);
    expect(recorded.pdfWorkers[0]?.destroy).toHaveBeenCalledTimes(1);
    open();
  });

  it('says the viewer could not be loaded when pdf.js refuses to begin, and ends the worker', async () => {
    pdfjs.getDocument = (options) => {
      recorded.options.push(options);
      throw new Error('getDocument threw');
    };
    const dialog = await openPdfPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The PDF viewer could not be loaded. Reload the console and try again.',
    );
    expect(recorded.options).toHaveLength(1);
    expect(recorded.pdfWorkers[0]?.destroy).toHaveBeenCalledTimes(1);
    expect(recorded.threads[0]?.terminate).toHaveBeenCalledTimes(1);
    close(dialog);
  });
});

describe('what pdf.js needs of the browser', () => {
  it('is named, each, where it lacks', () => {
    expect(missingBuiltIns({} as typeof globalThis)).toEqual([
      'getOrInsertComputed',
      'getOrInsert',
      'intersection',
      'sumPrecise',
      'fromBase64',
      'toBase64',
      'toHex',
      'transferToFixedLength',
      'try',
      'withResolvers',
      'parse',
      'filter',
      'find',
      'some',
      'toArray',
    ]);
    // This browser, but for one: built-ins are not enumerable, so each is named.
    const lacking = {
      Map,
      Set,
      Math: Object.create(Math, { sumPrecise: { value: undefined } }) as Math,
      Uint8Array,
      ArrayBuffer,
      Promise,
      URL,
      Iterator: Reflect.get(globalThis, 'Iterator') as unknown,
    } as unknown as typeof globalThis;
    expect(missingBuiltIns(lacking)).toEqual(['sumPrecise']);
  });
});

describe('the pages of a PDF', () => {
  it('draws one page at a time, when it is shown, on a canvas of its own', async () => {
    fakeDocument([
      { width: 612, height: 792 },
      { width: 792, height: 612 },
      { width: 612, height: 792 },
    ]);
    const dialog = await openPdfPreview();
    await within(dialog).findByText('Page 1 of 3');
    const previous = within(dialog).getByRole('button', { name: 'Previous' });
    const next = within(dialog).getByRole('button', { name: 'Next' });
    expect(previous.hasAttribute('disabled')).toBe(true);
    expect(next.hasAttribute('disabled')).toBe(false);
    await waitFor(() => {
      expect(recorded.cleanedUp).toEqual([1]);
    });
    expect(recorded.pagesAsked).toEqual([1]);
    const first = within(dialog).getByRole('img', { name: 'Page 1 of report.pdf' });
    expect(first.tagName).toBe('CANVAS');
    // As on paper, at 96 CSS pixels an inch.
    expect([first.getAttribute('width'), first.getAttribute('height')]).toEqual(['816', '1056']);
    expect(recorded.renders[0]?.canvas).toBe(first);
    // Annotations are drawn as they look; none is made a control.
    expect(recorded.renders[0]?.annotationMode).toBe(1);

    fireEvent.click(next);
    await within(dialog).findByText('Page 2 of 3');
    await waitFor(() => {
      expect(recorded.cleanedUp).toEqual([1, 2]);
    });
    const second = within(dialog).getByRole('img', { name: 'Page 2 of report.pdf' });
    expect(second).not.toBe(first);
    expect([second.getAttribute('width'), second.getAttribute('height')]).toEqual(['1056', '816']);
    expect(recorded.pagesAsked).toEqual([1, 2]);

    fireEvent.click(next);
    await within(dialog).findByText('Page 3 of 3');
    expect(next.hasAttribute('disabled')).toBe(true);
    fireEvent.click(previous);
    await within(dialog).findByText('Page 2 of 3');
    await waitFor(() => {
      expect(recorded.pagesAsked).toEqual([1, 2, 3, 2]);
    });
    // Nothing of the PDF is a link, a field or text in the page.
    expect(dialog.querySelector('a[href]:not([download]), input, textarea, select, iframe, object, embed')).toBeNull();
    close(dialog);
  });

  it('gives up the drawing of a page that is no longer shown', async () => {
    fakeDocument([
      { width: 612, height: 792, hangs: true },
      { width: 612, height: 792 },
    ]);
    const dialog = await openPdfPreview();
    await within(dialog).findByText('Page 1 of 2');
    await waitFor(() => {
      expect(recorded.renders).toHaveLength(1);
    });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await within(dialog).findByText('Page 2 of 2');
    expect(recorded.renders[0]?.cancel).toHaveBeenCalledTimes(1);
    // A drawing given up is not a failure; what pdf.js kept of both pages is let go.
    await waitFor(() => {
      expect([...recorded.cleanedUp].sort()).toEqual([1, 2]);
    });
    expect(within(dialog).queryByRole('alert')).toBeNull();
    close(dialog);
  });

  it('says the viewer stopped, not that the page is damaged, when the worker stops while drawing it', async () => {
    fakeDocument([{ width: 612, height: 792, hangs: true }]);
    const dialog = await openPdfPreview();
    await waitFor(() => {
      expect(recorded.renders).toHaveLength(1);
    });
    recorded.threads[0]?.fail();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The PDF viewer could not be loaded. Reload the console and try again.',
    );
    close(dialog);
  });

  it('draws a large page smaller, at most 16 MP', async () => {
    // 100 inches square: 9 600 × 9 600 CSS pixels as on paper.
    fakeDocument([{ width: 7200, height: 7200 }]);
    const dialog = await openPdfPreview();
    const page = await within(dialog).findByRole('img', { name: 'Page 1 of report.pdf' });
    await waitFor(() => {
      expect(recorded.renders).toHaveLength(1);
    });
    const width = Number(page.getAttribute('width'));
    const height = Number(page.getAttribute('height'));
    expect(width * height).toBeLessThanOrEqual(PDF_MAX_PIXELS);
    expect(width).toBe(4000);
    expect(recorded.renders[0]?.scale).toBeCloseTo(4000 / 7200, 9);
    close(dialog);
  });

  it('says a page that cannot be drawn, and shows the others', async () => {
    fakeDocument([
      { width: 612, height: 792, fails: pdfError('UnknownErrorException', 'bad content stream') },
      { width: 612, height: 792 },
    ]);
    const dialog = await openPdfPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe('Page 1 could not be drawn: it is damaged.');
    expect(within(dialog).queryByText(/bad content stream/)).toBeNull();
    // What pdf.js kept of the page is let go, drawn or not.
    expect(recorded.cleanedUp).toEqual([1]);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await within(dialog).findByText('Page 2 of 2');
    await waitFor(() => {
      expect(within(dialog).queryByRole('alert')).toBeNull();
    });
    close(dialog);
  });

  it('says a page of no size cannot be drawn', async () => {
    fakeDocument([{ width: 0, height: 792 }]);
    const dialog = await openPdfPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe('Page 1 could not be drawn: it is damaged.');
    expect(recorded.renders).toHaveLength(0);
    close(dialog);
  });

  it('says a document without a page has none', async () => {
    fakeDocument([]);
    const dialog = await openPdfPreview();
    expect(await within(dialog).findByText('There is no page in it.')).toBeTruthy();
    expect(dialog.querySelector('canvas')).toBeNull();
    close(dialog);
  });
});

describe('a PDF that cannot be opened', () => {
  it.each([
    [
      'a damaged PDF',
      pdfError('InvalidPDFException', 'Invalid PDF structure.'),
      'This PDF could not be read: it is damaged, or it is not a PDF. Download it to open it elsewhere.',
    ],
    [
      'any other failure',
      pdfError('UnknownErrorException', '<script>alert(1)</script>'),
      'This PDF could not be read: it is damaged, or it is not a PDF. Download it to open it elsewhere.',
    ],
    [
      'a PDF with a password',
      pdfError('PasswordException', 'No password given'),
      'This PDF is protected by a password, which the console does not ask for. Download it to open it.',
    ],
  ])('says so for %s, never in its own words, and offers the download', async (_, refusal, message) => {
    fakeDocument([], refusal);
    const dialog = await openPdfPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(message);
    expect(within(dialog).getByRole('link', { name: 'Download' })).toBeTruthy();
    expect(dialog.querySelector('canvas, script')).toBeNull();
    close(dialog);
    expect(recorded.destroyed).toHaveBeenCalledTimes(1);
  });
});

describe('closing a PDF', () => {
  it('destroys the document, and its worker with it', async () => {
    fakeDocument([{ width: 612, height: 792 }]);
    const dialog = await openPdfPreview();
    await within(dialog).findByText('Page 1 of 1');
    expect(recorded.destroyed).not.toHaveBeenCalled();
    close(dialog);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(recorded.destroyed).toHaveBeenCalledTimes(1);
    // The worker is the console's own: the console ends its thread at once.
    expect(recorded.threads[0]?.terminate).toHaveBeenCalledTimes(1);
    expect(recorded.pdfWorkers[0]?.destroy).toHaveBeenCalledTimes(1);
  });

  it('ends the worker at once, even when pdf.js never finishes destroying the document', async () => {
    recorded.destroyed = vi.fn(() => new Promise<void>(() => undefined));
    fakeDocument([{ width: 612, height: 792, hangs: true }]);
    const dialog = await openPdfPreview();
    await waitFor(() => {
      expect(recorded.renders).toHaveLength(1);
    });
    close(dialog);
    expect(recorded.destroyed).toHaveBeenCalledTimes(1);
    expect(recorded.threads[0]?.terminate).toHaveBeenCalledTimes(1);
    expect(recorded.pdfWorkers[0]?.destroy).toHaveBeenCalledTimes(1);
  });

  it('destroys a document still being opened, and shows nothing of it', async () => {
    let open: () => void = () => undefined;
    recorded.destroyed = vi.fn(() => new Promise<void>(() => undefined));
    fakeDocument([{ width: 612, height: 792 }], undefined, new Promise<void>((resolve) => (open = resolve)));
    const dialog = await openPdfPreview();
    await within(dialog).findByText('Opening the PDF…');
    await waitFor(() => {
      expect(recorded.options).toHaveLength(1);
    });
    close(dialog);
    expect(recorded.destroyed).toHaveBeenCalledTimes(1);
    expect(recorded.threads[0]?.terminate).toHaveBeenCalledTimes(1);
    open();
    await Promise.resolve();
    expect(recorded.pagesAsked).toEqual([]);
  });

  it('gives up the drawing of the page shown', async () => {
    fakeDocument([{ width: 612, height: 792, hangs: true }]);
    const dialog = await openPdfPreview();
    await waitFor(() => {
      expect(recorded.renders).toHaveLength(1);
    });
    close(dialog);
    expect(recorded.renders[0]?.cancel).toHaveBeenCalledTimes(1);
    expect(recorded.destroyed).toHaveBeenCalledTimes(1);
  });
});

describe('the size a page is drawn at', () => {
  it('is as on paper, sharp on the screen', () => {
    expect(wantedScale(1)).toBeCloseTo(4 / 3, 12);
    expect(wantedScale(2)).toBeCloseTo(8 / 3, 12);
    for (const ratio of [0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(wantedScale(ratio)).toBeCloseTo(4 / 3, 12);
    }
  });

  it.each([
    // A4 and Letter at scale 1 and 2.
    [595, 842, 4 / 3],
    [612, 792, 8 / 3],
    // Very large, very long, very wide, very small.
    [14_400, 14_400, 4 / 3],
    [200, 200_000, 4 / 3],
    [200_000, 1, 8 / 3],
    [0.01, 0.01, 4 / 3],
    [1, 1, 1_000_000],
  ])('keeps a page of %f × %f at scale %f within 16 MP and 16 384 px a side', (width, height, wanted) => {
    const size = pageSize(width, height, wanted);
    expect(size).not.toBeNull();
    if (size === null) {
      return;
    }
    expect(size.scale).toBeLessThanOrEqual(wanted);
    expect(size.width * size.height).toBeLessThanOrEqual(PDF_MAX_PIXELS);
    expect(size.width).toBeLessThanOrEqual(PDF_MAX_SIDE);
    expect(size.height).toBeLessThanOrEqual(PDF_MAX_SIDE);
    expect(size.width).toBeGreaterThanOrEqual(1);
    expect(size.height).toBeGreaterThanOrEqual(1);
  });

  it('is what was wanted when that is within the caps', () => {
    expect(pageSize(612, 792, 4 / 3)).toEqual({ scale: 4 / 3, width: 816, height: 1056 });
  });

  it.each([
    [0, 792],
    [612, 0],
    [-612, 792],
    [Number.NaN, 792],
    [612, Number.POSITIVE_INFINITY],
  ])('is none for a page of %f × %f', (width, height) => {
    expect(pageSize(width, height, 4 / 3)).toBeNull();
  });

  it('holds for any page and any scale (property)', () => {
    // A small generator of its own, seeded, so that a failure is repeated.
    let seed = 0x2e6b4e;
    const random = () => {
      seed = (seed * 1_103_515_245 + 12_345) % 2 ** 31;
      return seed / 2 ** 31;
    };
    for (let i = 0; i < 2_000; i += 1) {
      const width = 10 ** (random() * 10 - 3);
      const height = 10 ** (random() * 10 - 3);
      const wanted = 10 ** (random() * 4 - 1);
      const size = pageSize(width, height, wanted);
      expect(size).not.toBeNull();
      if (size === null) {
        return;
      }
      expect(size.width * size.height).toBeLessThanOrEqual(PDF_MAX_PIXELS);
      expect(Math.max(size.width, size.height)).toBeLessThanOrEqual(PDF_MAX_SIDE);
      expect(Math.min(size.width, size.height)).toBeGreaterThanOrEqual(1);
      expect(size.scale).toBeLessThanOrEqual(wanted);
    }
  });
});
