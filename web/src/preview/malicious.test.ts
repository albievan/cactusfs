// The console's malicious-file suite (PLAN O.11): each file made to be taken
// for what it is not, opened through the preview dialog as an object of the
// API, with what must come of it - drawn with nothing run, fetched or
// followed, or refused with its sentence. Every file is made by code in
// ../test/malicious and ../test/ooxml.
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { createElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { objectDownloadUrl } from '../api/client';
import { fakeApi, type FakeApi } from '../test/fakeApi';
import {
  BILLION_LAUGHS,
  docxManyParts,
  docxRatioBomb,
  docxWithLyingImages,
  docxWithMarkup,
  docxWithPrologue,
  EXTERNAL_ENTITY,
  gifScriptPolyglot,
  MARKER,
  pdfHtmlPolyglot,
  pdfWithActions,
  svgWithScript,
  xlsxManyParts,
  xlsxOfManySheets,
  xlsxRatioBomb,
  xlsxWithHyperlinks,
  xlsxWithPrologue,
} from '../test/malicious';
import { gifOf } from '../test/ooxml';
import { answer, readOoxml } from './ooxml';
import { OoxmlError } from './ooxml-error';
import { PreviewButton } from './PreviewButton';

// pdf.js draws on a canvas, which jsdom has not got: it is a fake here that
// records what it is asked, and whose document and pages have every method
// by which pdf.js gives out a PDF's scripts, actions, links, forms and text -
// each a spy that must never be called. A real PDF is drawn in a browser
// (PLAN O.14).
const pdfjs = vi.hoisted(() => ({
  options: [] as Record<string, unknown>[],
  renders: [] as Record<string, unknown>[],
  // What must never be asked of a document or a page, by its name.
  forbidden: new Map<string, () => unknown>(),
}));

vi.mock('pdfjs-dist', () => ({
  version: '6.4.299',
  AnnotationMode: { DISABLE: 0, ENABLE: 1, ENABLE_FORMS: 2, ENABLE_STORAGE: 3 },
  getDocument: (options: Record<string, unknown>) => {
    pdfjs.options.push(options);
    const forbid = (of: string, names: string[]) =>
      Object.fromEntries(
        names.map((name) => {
          const spy = vi.fn(() => Promise.resolve(null));
          pdfjs.forbidden.set(`${of}.${name}`, spy);
          return [name, spy];
        }),
      );
    const page = {
      getViewport: ({ scale }: { scale: number }) => ({ width: 200 * scale, height: 200 * scale, scale }),
      render: (params: Record<string, unknown>) => {
        pdfjs.renders.push(params);
        return { promise: Promise.resolve(), cancel: vi.fn() };
      },
      cleanup: () => true,
      ...forbid('page', ['getAnnotations', 'getTextContent', 'getJSActions', 'getStructTree', 'streamTextContent']),
    };
    const document = {
      numPages: 1,
      getPage: () => Promise.resolve(page),
      ...forbid('document', [
        'getJSActions',
        'getOpenAction',
        'getAttachments',
        'getFieldObjects',
        'hasJSActions',
        'getDestinations',
        'getOutline',
        'saveDocument',
      ]),
    };
    return { promise: Promise.resolve(document), destroy: vi.fn(() => Promise.resolve()) };
  },
  PDFWorker: { create: () => ({ destroy: vi.fn() }) },
}));

vi.mock('pdfjs-dist/build/pdf.worker.min.mjs?url', () => ({ default: '/assets/pdf.worker.min-test.mjs' }));

/**
 * A Web Worker as the console makes one - jsdom has none. The document
 * reader's worker reads what it is sent with the worker's own `answer`,
 * from a structured clone and after a turn of the event loop, as a real one
 * would; pdf.js's worker does nothing.
 */
class FakeWorker {
  static made: FakeWorker[] = [];
  readonly name: string | undefined;
  readonly terminate = vi.fn(() => {
    this.ended = true;
  });
  ended = false;
  readonly #listeners = new Map<string, ((event: Event) => void)[]>();

  constructor(_url: string | URL, options?: WorkerOptions) {
    this.name = options?.name;
    FakeWorker.made.push(this);
  }

  addEventListener(type: string, listener: (event: Event) => void) {
    this.#listeners.set(type, [...(this.#listeners.get(type) ?? []), listener]);
  }

  postMessage(message: unknown) {
    if (this.name !== 'ooxml') {
      return;
    }
    setTimeout(() => {
      if (!this.ended) {
        const data = answer(structuredClone(message)).reply;
        for (const listener of this.#listeners.get('message') ?? []) {
          listener(new MessageEvent('message', { data }));
        }
      }
    }, 0);
  }
}

/** The workers of the document reader made so far. */
function readers(): FakeWorker[] {
  return FakeWorker.made.filter((worker) => worker.name === 'ooxml');
}

/** What pdf.js 6.4 needs of the language and Node may not have: given for the tests, taken away after. */
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

let blobs: Blob[];
let opened: ReturnType<typeof vi.fn>;

beforeEach(() => {
  pdfjs.options = [];
  pdfjs.renders = [];
  pdfjs.forbidden = new Map();
  FakeWorker.made = [];
  vi.stubGlobal('Worker', FakeWorker);
  vi.stubGlobal('devicePixelRatio', 1);
  opened = vi.fn(() => null);
  vi.stubGlobal('open', opened);
  givenBuiltIns = BUILT_INS.filter(([holder, name]) => typeof holder[name] !== 'function');
  for (const [holder, name] of givenBuiltIns) {
    Object.defineProperty(holder, name, { configurable: true, writable: true, value: () => undefined });
  }
  blobs = [];
  const create = vi.fn((blob: Blob) => {
    blobs.push(blob);
    return `blob:made-${String(blobs.length)}`;
  });
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, writable: true, value: create });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: vi.fn() });
});

afterEach(() => {
  for (const [holder, name] of givenBuiltIns) {
    Reflect.deleteProperty(holder, name);
  }
  Reflect.deleteProperty(URL, 'createObjectURL');
  Reflect.deleteProperty(URL, 'revokeObjectURL');
});

const PREVIEW = 'GET /buckets/photos/object/preview';

const TYPES = {
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
} as const;

/** What the API answers a preview it allows: the kind it proved, as an attachment. */
function previewOf(kind: string, body: Uint8Array<ArrayBuffer>): Response {
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

/** Opens the preview of an object of the bucket "photos" whose preview is `bytes` of `kind`. */
async function openPreview(
  objectKey: string,
  contentType: string,
  kind: string,
  bytes: Uint8Array<ArrayBuffer>,
): Promise<{ dialog: HTMLElement; api: FakeApi }> {
  const api = fakeApi({ [PREVIEW]: () => previewOf(kind, bytes) });
  render(createElement(PreviewButton, { bucket: 'photos', objectKey, contentType }));
  fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
  const dialog = await screen.findByRole('dialog', { name: `Preview of ${objectKey}` });
  return { dialog, api };
}

/**
 * What must hold of the page after any file of the suite: nothing of it was
 * made markup that runs, embeds or navigates; the only link is the download
 * of the object; nothing was fetched but its preview; no window was opened
 * and the page was not navigated.
 */
function nothingRanOrWasFollowed(dialog: HTMLElement, api: FakeApi, objectKey: string, address: string) {
  expect(document.querySelector('script, iframe, frame, object, embed, form, input, svg, base, meta, link')).toBeNull();
  expect(document.querySelector('[onload], [onerror], [onclick], [href^="javascript:"], [src^="javascript:"]')).toBeNull();
  const links = Array.from(dialog.querySelectorAll('a'));
  expect(links.map((link) => link.getAttribute('href'))).toEqual([objectDownloadUrl('photos', objectKey)]);
  expect(api.calls.map((call) => `${call.method} ${call.path}`)).toEqual([PREVIEW]);
  expect(opened).not.toHaveBeenCalled();
  expect(window.location.href).toBe(address);
}

describe('a PDF that asks to run, launch and follow', () => {
  it('is drawn on a canvas with JavaScript, OpenAction, Launch and a URI link left unrun and unfollowed', async () => {
    const address = window.location.href;
    const pdf = pdfWithActions();
    const { dialog, api } = await openPreview('actions.pdf', 'application/pdf', 'pdf', pdf);
    await within(dialog).findByText('Page 1 of 1');
    await waitFor(() => {
      expect(pdfjs.renders).toHaveLength(1);
    });
    // The very file, as bytes: pdf.js is given no address to fetch or resolve against.
    const [options] = pdfjs.options;
    const given = new TextDecoder('latin1').decode(options?.data as Uint8Array);
    expect(given).toBe(new TextDecoder('latin1').decode(pdf));
    for (const asked of ['/OpenAction', '/S /JavaScript', '/S /Launch', '/S /URI']) {
      expect(given).toContain(asked);
    }
    expect(options).toMatchObject({ enableScripting: false, isEvalSupported: false, enableXfa: false, useWasm: false });
    for (const key of ['url', 'range', 'httpHeaders', 'withCredentials', 'docBaseUrl', 'cMapUrl', 'standardFontDataUrl', 'iccUrl']) {
      expect(options).not.toHaveProperty(key);
    }
    // Annotations are drawn as pixels: no form, no storage, no layer of links.
    expect(pdfjs.renders[0]?.annotationMode).toBe(1);
    expect(pdfjs.renders[0]).not.toHaveProperty('annotationLayer');
    expect(pdfjs.renders[0]?.canvas).toBeInstanceOf(HTMLCanvasElement);
    // Nothing that gives out the PDF's actions, links, forms or text was asked for.
    expect(pdfjs.forbidden.size).toBe(13);
    for (const [name, spy] of pdfjs.forbidden) {
      expect(spy, name).not.toHaveBeenCalled();
    }
    expect(dialog.querySelector('.annotationLayer, .textLayer, .linkAnnotation, [data-annotation-id], [data-element-id]')).toBeNull();
    expect(dialog.querySelectorAll('canvas')).toHaveLength(1);
    nothingRanOrWasFollowed(dialog, api, 'actions.pdf', address);
  });

  it('is drawn the same when it is also a page of HTML: the markup is never made a page', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('page.pdf', 'application/pdf', 'pdf', pdfHtmlPolyglot());
    await within(dialog).findByText('Page 1 of 1');
    expect(dialog.textContent).not.toContain(MARKER);
    nothingRanOrWasFollowed(dialog, api, 'page.pdf', address);
  });
});

describe('a DOCX or XLSX that would unpack to too much', () => {
  const TOO_LARGE =
    'This document is too large to preview: it unpacks to more than the console reads. Download it to open it.';

  it.each([
    ['a DOCX of one part of a ratio of 1 000', 'docx', docxRatioBomb],
    ['a DOCX of 10 001 parts', 'docx', docxManyParts],
    ['an XLSX of one sheet of a ratio of 1 000', 'xlsx', xlsxRatioBomb],
    ['an XLSX of 10 001 parts', 'xlsx', xlsxManyParts],
  ] as const)('stops %s with "too large to preview", and its worker is ended', async (_, kind, bytes) => {
    const address = window.location.href;
    const key = `bomb.${kind}`;
    const { dialog, api } = await openPreview(key, TYPES[kind], kind, bytes());
    expect((await within(dialog).findByRole('alert')).textContent).toBe(TOO_LARGE);
    expect(readers()).toHaveLength(1);
    expect(readers()[0]?.terminate).toHaveBeenCalledTimes(1);
    nothingRanOrWasFollowed(dialog, api, key, address);
  });

  it('stops many parts, each within its bounds, that unpack to more than the total', () => {
    // Five sheets of about 18 KiB each, read with a total of 64 KiB: the
    // bounds of the plan scaled down, so that the test stays small.
    const bounds = { totalBytes: 64 * 1024, partBytes: 32 * 1024, ratio: 100, entries: 10_000 };
    const workbook = xlsxOfManySheets(5, 600);
    expect(readOoxml('xlsx', xlsxOfManySheets(2, 600), bounds).kind).toBe('xlsx');
    let refused: unknown = null;
    try {
      readOoxml('xlsx', workbook, bounds);
    } catch (error: unknown) {
      refused = error;
    }
    expect(refused).toBeInstanceOf(OoxmlError);
    expect((refused as OoxmlError).problem).toBe('too-large');
    // By the bound in all, not by the size or ratio of a part.
    expect((refused as OoxmlError).message).toMatch(/in all/);
  });
});

describe('XML that declares a DTD', () => {
  const UNSAFE = 'This document declares a DOCTYPE or entities, which the console never reads. Download it to open it elsewhere.';

  it.each([
    ['billion laughs in a DOCX', 'docx', () => docxWithPrologue(BILLION_LAUGHS, 'lol9')],
    ['an external entity in a DOCX', 'docx', () => docxWithPrologue(EXTERNAL_ENTITY, 'xxe')],
    ['billion laughs in an XLSX', 'xlsx', () => xlsxWithPrologue(BILLION_LAUGHS, 'lol9')],
    ['an external entity in an XLSX', 'xlsx', () => xlsxWithPrologue(EXTERNAL_ENTITY, 'xxe')],
  ] as const)('refuses %s, and fetches nothing it names', async (_, kind, bytes) => {
    const address = window.location.href;
    const key = `entities.${kind}`;
    const { dialog, api } = await openPreview(key, TYPES[kind], kind, bytes());
    expect((await within(dialog).findByRole('alert')).textContent).toBe(UNSAFE);
    expect(readers()[0]?.terminate).toHaveBeenCalledTimes(1);
    expect(dialog.textContent).not.toContain('lol');
    nothingRanOrWasFollowed(dialog, api, key, address);
  });
});

describe('markup and links in the text of a document', () => {
  it('shows markup inside a DOCX as text, and its hyperlinks as text without a link', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('markup.docx', TYPES.docx, 'docx', docxWithMarkup());
    const body = await within(dialog).findByText(`<script>alert('${MARKER}')</script>`);
    expect(body.tagName).toBe('P');
    expect(within(dialog).getByText(`<img src=x onerror="alert('${MARKER}')">`)).toBeTruthy();
    expect(within(dialog).getByText(`click ${MARKER}`).closest('a')).toBeNull();
    expect(within(dialog).getByText(`run ${MARKER}`).closest('a')).toBeNull();
    expect(dialog.querySelector('img')).toBeNull();
    nothingRanOrWasFollowed(dialog, api, 'markup.docx', address);
  });

  it('shows an XLSX cell that begins "=HYPERLINK(" as text, with no link, and a formula as its saved value', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('links.xlsx', TYPES.xlsx, 'xlsx', xlsxWithHyperlinks());
    const sheet = await within(dialog).findByRole('group', { name: 'Sheet Links of links.xlsx' });
    const rows = within(sheet).getAllByRole('row');
    expect(rows.map((row) => Array.from(row.children, (cell) => cell.textContent))).toEqual([
      ['', 'A', 'B', 'C'],
      ['1', `=HYPERLINK("https://example.invalid/${MARKER}","click")`, '=HYPERLINK("javascript:alert(1)","run")', 'go'],
    ]);
    expect(sheet.querySelector('a')).toBeNull();
    nothingRanOrWasFollowed(dialog, api, 'links.xlsx', address);
  });
});

describe('an image that lies about what it is', () => {
  it('draws an object named and stored as PNG as the GIF the API proved it is, from a blob typed image/gif', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('cat.png', 'image/png', 'gif', gifOf(4, 4));
    await within(dialog).findByRole('img');
    expect(blobs.map((blob) => blob.type)).toEqual(['image/gif']);
    nothingRanOrWasFollowed(dialog, api, 'cat.png', address);
  });

  it('types the bytes as the raster kind the API named, never SVG, whatever they hold', async () => {
    const address = window.location.href;
    // A server that lied: SVG with a script, said to be a PNG.
    const { dialog, api } = await openPreview('cat.png', 'image/png', 'png', svgWithScript());
    await within(dialog).findByRole('img');
    expect(blobs.map((blob) => blob.type)).toEqual(['image/png']);
    nothingRanOrWasFollowed(dialog, api, 'cat.png', address);
  });

  it('draws a GIF that is also a script as a GIF', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('cat.gif', 'image/gif', 'gif', gifScriptPolyglot());
    await within(dialog).findByRole('img');
    expect(blobs.map((blob) => blob.type)).toEqual(['image/gif']);
    expect(dialog.textContent).not.toContain(MARKER);
    nothingRanOrWasFollowed(dialog, api, 'cat.gif', address);
  });

  it('draws nothing of a kind it does not know: SVG', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('cat.png', 'application/octet-stream', 'svg', svgWithScript());
    expect(await within(dialog).findByText('This kind of file cannot be shown here yet.')).toBeTruthy();
    expect(blobs).toHaveLength(0);
    nothingRanOrWasFollowed(dialog, api, 'cat.png', address);
  });

  it('shows a DOCX image by its signature, not its name: SVG named .png is not shown, a GIF named .png is a GIF', async () => {
    const address = window.location.href;
    const { dialog, api } = await openPreview('images.docx', TYPES.docx, 'docx', docxWithLyingImages(gifOf(5, 6)));
    expect(await within(dialog).findByText('[an image that is not shown]')).toBeTruthy();
    expect(within(dialog).getAllByRole('img', { name: 'An image of the document' })).toHaveLength(1);
    expect(blobs.map((blob) => blob.type)).toEqual(['image/gif']);
    nothingRanOrWasFollowed(dialog, api, 'images.docx', address);
  });
});
