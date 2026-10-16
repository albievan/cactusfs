import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { objectDownloadUrl } from '../api/client';
import { fakeApi } from '../test/fakeApi';
import { docxOf, para, picture, pngOf, relType, seeded, xlsxOf } from '../test/ooxml';
import { rendererOf } from './kinds';
import { readOffice, type OfficeWorker } from './office';
import { OfficeBoundary } from './OfficePreview';
import { answer } from './ooxml';
import { checkedModel } from './ooxml-check';
import {
  DOCX_MAX_BLOCKS,
  DOCX_MAX_NODES,
  DOCX_MAX_TABLE_DEPTH,
  OOXML_BOUNDS,
  OOXML_TIMEOUT_MS,
  XLSX_MAX_CHARACTERS,
  type OoxmlReply,
} from './ooxml-model';
import { PreviewButton } from './PreviewButton';

const PREVIEW = 'GET /buckets/photos/object/preview';

/** How a fake worker behaves: reads with the real reader, never answers, fails to load, or answers this. */
type Behaviour = 'read' | 'silent' | 'fail' | { reply: unknown };

/**
 * A Web Worker as the console makes one - jsdom has none. It reads what it
 * is sent with the worker's own `answer`, after a turn of the event loop
 * and from a structured clone, as a real worker would.
 */
class FakeWorker implements OfficeWorker {
  static behaviour: Behaviour = 'read';
  static made: FakeWorker[] = [];
  readonly url: string;
  readonly options: WorkerOptions | undefined;
  readonly terminate = vi.fn(() => {
    this.ended = true;
  });
  readonly sent: { message: unknown; transfer: Transferable[] }[] = [];
  ended = false;
  readonly #listeners = new Map<string, ((event: Event) => void)[]>();

  constructor(url: string | URL, options?: WorkerOptions) {
    this.url = String(url);
    this.options = options;
    FakeWorker.made.push(this);
  }

  addEventListener(type: string, listener: (event: Event) => void) {
    this.#listeners.set(type, [...(this.#listeners.get(type) ?? []), listener]);
  }

  #fire(type: string, event: Event) {
    if (!this.ended) {
      for (const listener of this.#listeners.get(type) ?? []) {
        listener(event);
      }
    }
  }

  postMessage(message: unknown, transfer: Transferable[]) {
    this.sent.push({ message, transfer });
    const behaviour = FakeWorker.behaviour;
    setTimeout(() => {
      if (behaviour === 'silent') {
        return;
      }
      if (behaviour === 'fail') {
        this.#fire('error', new Event('error'));
        return;
      }
      const data = behaviour === 'read' ? answer(structuredClone(message)).reply : behaviour.reply;
      this.#fire('message', new MessageEvent('message', { data }));
    }, 0);
  }
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

let urls: { create: ReturnType<typeof vi.fn>; revoke: ReturnType<typeof vi.fn>; made: Blob[] };

beforeEach(() => {
  FakeWorker.behaviour = 'read';
  FakeWorker.made = [];
  vi.stubGlobal('Worker', FakeWorker);
  const made: Blob[] = [];
  const create = vi.fn((blob: Blob) => {
    made.push(blob);
    return `blob:image-${String(made.length)}`;
  });
  const revoke = vi.fn();
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, writable: true, value: create });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: revoke });
  urls = { create, revoke, made };
});

afterEach(() => {
  vi.useRealTimers();
  Reflect.deleteProperty(URL, 'createObjectURL');
  Reflect.deleteProperty(URL, 'revokeObjectURL');
});

/** Opens the preview of an object of the bucket "photos" and returns its dialog. */
async function openPreview(objectKey: string, contentType: string): Promise<HTMLElement> {
  render(<PreviewButton bucket="photos" objectKey={objectKey} contentType={contentType} />);
  fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
  return screen.findByRole('dialog', { name: `Preview of ${objectKey}` });
}

const DOCX_TYPE = 'application/vnd.openxmlformats-officedocument.wordprocessingml.document';
const XLSX_TYPE = 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet';

describe('kinds of office documents', () => {
  it('draws DOCX and XLSX: they are no longer "later"', () => {
    expect(rendererOf('docx')).toBe('office');
    expect(rendererOf('xlsx')).toBe('office');
  });
});

describe('reading a document in a worker of its own', () => {
  it('makes a module worker of the console, hands it the bytes, and ends it once it has answered', async () => {
    const bytes = xlsxOf({ sheets: [{ name: 'One', data: '<row r="1"><c r="A1"><v>1</v></c></row>' }] }).buffer;
    const reading = readOffice('xlsx', bytes);
    const [worker] = FakeWorker.made;
    expect(worker?.url).toMatch(/\/ooxml\.worker\.ts\b/);
    expect(worker?.options).toEqual({ type: 'module', name: 'ooxml' });
    expect(worker?.sent[0]?.transfer).toEqual([bytes]);
    const outcome = await reading.outcome;
    expect(outcome.ok && outcome.model.kind).toBe('xlsx');
    expect(worker?.terminate).toHaveBeenCalledTimes(1);
  });

  it(`ends the worker after ${String(OOXML_TIMEOUT_MS / 1000)} seconds and says so`, async () => {
    vi.useFakeTimers();
    FakeWorker.behaviour = 'silent';
    const reading = readOffice('docx', new ArrayBuffer(8));
    vi.advanceTimersByTime(OOXML_TIMEOUT_MS - 1);
    expect(FakeWorker.made[0]?.terminate).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(FakeWorker.made[0]?.terminate).toHaveBeenCalledTimes(1);
    await expect(reading.outcome).resolves.toEqual({ ok: false, problem: 'timeout' });
  });

  it('ends the worker at once when it is closed, and once only', () => {
    FakeWorker.behaviour = 'silent';
    const reading = readOffice('docx', new ArrayBuffer(8));
    reading.close();
    reading.close();
    expect(FakeWorker.made[0]?.terminate).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['a worker that fails to load', 'fail' as const, 'load'],
    ['an answer of another shape', { reply: { ok: true, model: { kind: 'html' } } }, 'damaged'],
    ['an answer of no known problem', { reply: { ok: false, problem: 'other' } }, 'damaged'],
  ])('says %s, and ends it', async (_, behaviour, problem) => {
    FakeWorker.behaviour = behaviour;
    const reading = readOffice('docx', new ArrayBuffer(8));
    await expect(reading.outcome).resolves.toEqual({ ok: false, problem });
    expect(FakeWorker.made[0]?.terminate).toHaveBeenCalledTimes(1);
  });

  it('says that a worker that cannot be made did not load, and reads nothing on the page instead', async () => {
    const reading = readOffice('docx', new ArrayBuffer(8), () => {
      throw new Error('blocked by the CSP');
    });
    await expect(reading.outcome).resolves.toEqual({ ok: false, problem: 'load' });
  });
});

describe('what the page takes of the worker', () => {
  const docx = (blocks: unknown, images: unknown[] = []) => ({ kind: 'docx', blocks, images, truncated: false, deepTables: false });
  const sheet = (rows: unknown, name = 'S') => ({ name, hidden: false, content: 'cells', rows, moreRows: false, moreColumns: false });
  const xlsx = (sheets: unknown[]) => ({ kind: 'xlsx', sheets, moreSheets: false, truncated: false });
  const text = (value: string) => ({ type: 'text', text: value, bold: false, italic: false, underline: false });
  const paragraph = (inlines: unknown[]) => ({ type: 'paragraph', heading: 0, list: null, inlines });

  it('takes models of the shape and size the worker makes', () => {
    expect(checkedModel(docx([paragraph([text('a'), { type: 'break' }, { type: 'image', image: 0 }])], [{ kind: 'png', bytes: new Uint8Array(1) }]))).not.toBeNull();
    expect(checkedModel(docx([{ type: 'table', rows: [[{ span: 1, blocks: [paragraph([])] }]] }]))).not.toBeNull();
    expect(checkedModel(xlsx([sheet([['a', ''], []])]))).not.toBeNull();
    expect(checkedModel(docx([{ ...paragraph([]), heading: 6, list: { level: 8, ordered: false } }]))).not.toBeNull();
    expect(checkedModel(docx([{ type: 'table', rows: [[{ span: 1000, blocks: [] }]] }]))).not.toBeNull();
    const nested = Array.from({ length: DOCX_MAX_TABLE_DEPTH - 1 }).reduce<unknown>(
      (inner) => ({ type: 'table', rows: [[{ span: 1, blocks: [inner] }]] }),
      { type: 'table', rows: [] },
    );
    expect(checkedModel(docx([nested]))).not.toBeNull();
  });

  it.each([
    ['no model', null],
    ['a kind it does not draw', { kind: 'html' }],
    ['blocks that are no list', docx('x')],
    ['a block of no known type', docx([{ type: 'script' }])],
    ['an inline of no known type', docx([paragraph([{ type: 'html', html: '<b>' }])])],
    ['text that is no string', docx([paragraph([{ ...text(''), text: {} }])])],
    ['an image that is not there', docx([paragraph([{ type: 'image', image: 1 }])], [{ kind: 'png', bytes: new Uint8Array(1) }])],
    ['an image of another kind', docx([], [{ kind: 'svg', bytes: new Uint8Array(1) }])],
    ['an image whose bytes are no bytes', docx([], [{ kind: 'png', bytes: [1, 2] }])],
    ['more nodes than the worker makes', docx([paragraph(Array.from({ length: DOCX_MAX_NODES }, () => ({ type: 'break' })))])],
    ['tables nested past the bound of nodes', docx([{ type: 'table', rows: Array.from({ length: DOCX_MAX_NODES }, () => []) }])],
    ['a cell that is no text', xlsx([sheet([[{}]])])],
    ['a sheet name longer than Excel allows', xlsx([sheet([], 'N'.repeat(32))])],
    ['more characters than the worker shows', xlsx([sheet(Array.from({ length: 11 }, () => ['x'.repeat(XLSX_MAX_CHARACTERS / 10)]))])],
    ['rows past the bound', xlsx([sheet(Array.from({ length: 1001 }, () => []))])],
    ['a cell longer than Excel holds', xlsx([sheet([['x'.repeat(32_768)]])])],
    ['a sheet whose flags are no booleans', xlsx([{ ...sheet([]), hidden: 'yes' }])],
    ['a sheet whose more rows is no boolean', xlsx([{ ...sheet([]), moreRows: 1 }])],
    ['a sheet whose more columns is no boolean', xlsx([{ ...sheet([]), moreColumns: null }])],
    ['a list place of another shape', docx([{ ...paragraph([]), list: { level: 1, ordered: 'yes' } }])],
    ['a list level past 8', docx([{ ...paragraph([]), list: { level: 9, ordered: true } }])],
    ['a list level that is no whole number', docx([{ ...paragraph([]), list: { level: 0.5, ordered: true } }])],
    ['a heading past 6', docx([{ ...paragraph([]), heading: 7 }])],
    ['a heading below 0', docx([{ ...paragraph([]), heading: -1 }])],
    ['text whose emphasis is no boolean', docx([paragraph([{ ...text('a'), bold: 'yes' }])])],
    ['a span of 0', docx([{ type: 'table', rows: [[{ span: 0, blocks: [] }]] }])],
    ['a span past 1 000', docx([{ type: 'table', rows: [[{ span: 1001, blocks: [] }]] }])],
    ['a span that is no whole number', docx([{ type: 'table', rows: [[{ span: 1.5, blocks: [] }]] }])],
    ['more blocks than the worker shows', docx(Array.from({ length: DOCX_MAX_BLOCKS + 1 }, () => paragraph([])))],
    ['images of more bytes than the worker unpacks', docx([], [{ kind: 'png', bytes: new Uint8Array(OOXML_BOUNDS.totalBytes + 1) }])],
    [
      'tables nested deeper than the worker reads',
      docx(
        [Array.from({ length: DOCX_MAX_TABLE_DEPTH }).reduce<unknown>((inner) => ({ type: 'table', rows: [[{ span: 1, blocks: [inner] }]] }), { type: 'table', rows: [] })],
      ),
    ],
  ])('refuses %s', (_, model) => {
    expect(checkedModel(model)).toBeNull();
  });

  it('says that a reply of the wrong shape could not be read, and keeps the dialog', async () => {
    FakeWorker.behaviour = { reply: { ok: true, model: { kind: 'docx', blocks: [{ type: 'paragraph', heading: 0, inlines: 'x' }], images: [], truncated: false, deepTables: false } } };
    fakeApi({ [PREVIEW]: () => previewOf('docx', new Uint8Array([0x50, 0x4b, 3, 4])) });
    const dialog = await openPreview('odd.docx', DOCX_TYPE);
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'This document could not be read: it is damaged, or it is not a Word document. Download it to open it elsewhere.',
    );
    expect(within(dialog).getByRole('button', { name: 'Close' })).toBeTruthy();
  });

  it('says that a document that cannot be drawn was not shown, in its place', () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const Broken = () => {
      throw new Error('cannot draw');
    };
    render(
      <div>
        <OfficeBoundary>
          <Broken />
        </OfficeBoundary>
        <button type="button">Close</button>
      </div>,
    );
    expect(screen.getByRole('alert').textContent).toBe('This document could not be shown. Download it to open it elsewhere.');
    expect(screen.getByRole('button', { name: 'Close' })).toBeTruthy();
  });
});

describe('the worker itself', () => {
  it('answers a request with the model, handing over the images, and anything else as damaged', async () => {
    const posted: [unknown, Transferable[]][] = [];
    vi.stubGlobal('postMessage', (message: unknown, transfer: Transferable[]) => {
      posted.push([message, transfer]);
    });
    await import('./ooxml.worker');
    const bytes = docxOf(`<w:p>${picture('r:embed', 'img')}</w:p>`, {
      relationships: [['img', relType('image'), 'media/a.png']],
      parts: { 'word/media/a.png': pngOf(2, 2) },
    });
    window.dispatchEvent(new MessageEvent('message', { data: { kind: 'docx', bytes: bytes.buffer } }));
    window.dispatchEvent(new MessageEvent('message', { data: 'not a request' }));
    expect(posted).toHaveLength(2);
    const [reply, transfer] = posted[0] ?? [];
    const model = (reply as OoxmlReply & { ok: true }).model;
    expect(model.kind === 'docx' && model.images.map((image) => image.bytes.buffer)).toEqual(transfer);
    expect(posted[1]).toEqual([{ ok: false, problem: 'damaged' }, []]);
  });

  it.each([null, 7, { kind: 'pdf', bytes: new ArrayBuffer(1) }, { kind: 'docx', bytes: 'text' }])(
    'answers %j as damaged',
    (message) => {
      expect(answer(message)).toEqual({ reply: { ok: false, problem: 'damaged' }, transfer: [] });
    },
  );
});

describe('the preview of a DOCX', () => {
  const docx = () =>
    docxOf(
      [
        para('Report', '<w:pStyle w:val="Heading1"/>'),
        '<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>Bold</w:t></w:r><w:r><w:t xml:space="preserve"> and </w:t></w:r><w:r><w:rPr><w:i/></w:rPr><w:t>italic</w:t></w:r><w:r><w:br/><w:t>second line</w:t></w:r></w:p>',
        para('first', '<w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>'),
        para('second', '<w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>'),
        '<w:tbl><w:tr><w:tc><w:p><w:r><w:t>cell</w:t></w:r></w:p></w:tc></w:tr></w:tbl>',
        para('&lt;script&gt;alert(1)&lt;/script&gt;&lt;b&gt;not bold&lt;/b&gt;'),
        `<w:p>${picture('r:embed', 'img')}</w:p>`,
      ].join(''),
      {
        styles: '<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/></w:style>',
        numbering:
          '<w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum><w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num>',
        relationships: [['img', relType('image'), 'media/image1.png']],
        parts: { 'word/media/image1.png': pngOf(40, 30) },
      },
    );

  it('shows headings, emphasis, lists, tables and images as elements, and markup as text', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('docx', docx()) });
    const dialog = await openPreview('report.docx', DOCX_TYPE);
    const document = await within(dialog).findByRole('document', { name: 'Contents of report.docx' });
    expect(within(document).getByRole('heading', { level: 3 }).textContent).toBe('Report');
    expect(document.querySelector('strong')?.textContent).toBe('Bold');
    expect(document.querySelector('em')?.textContent).toBe('italic');
    expect(document.querySelectorAll('br')).toHaveLength(1);
    expect(Array.from(document.querySelectorAll('ol > li')).map((li) => li.textContent)).toEqual(['first', 'second']);
    expect(within(document).getByRole('table').textContent).toBe('cell');
    // Markup in the text is text: no element was made of it.
    expect(within(document).getByText('<script>alert(1)</script><b>not bold</b>')).toBeTruthy();
    expect(document.querySelector('script')).toBeNull();
    expect(document.querySelectorAll('b')).toHaveLength(0);
    const image = within(document).getByRole('img', { name: 'An image of the document' });
    expect(image.getAttribute('src')).toBe('blob:image-1');
    expect(urls.made[0]?.type).toBe('image/png');
    expect(within(dialog).getByText(/Layout, styles, charts and comments are not shown/)).toBeTruthy();
    expect(FakeWorker.made[0]?.terminate).toHaveBeenCalledTimes(1);
  });

  it('gives back the addresses of its images when the dialog closes', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('docx', docx()) });
    const dialog = await openPreview('report.docx', DOCX_TYPE);
    await within(dialog).findByRole('img', { name: 'An image of the document' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(urls.revoke).toHaveBeenCalledWith('blob:image-1');
  });

  it('ends the worker at once when the dialog closes while it reads', async () => {
    FakeWorker.behaviour = 'silent';
    fakeApi({ [PREVIEW]: () => previewOf('docx', docx()) });
    const dialog = await openPreview('report.docx', DOCX_TYPE);
    await within(dialog).findByText('Reading the document…');
    await waitFor(() => {
      expect(FakeWorker.made).toHaveLength(1);
    });
    expect(FakeWorker.made[0]?.terminate).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(FakeWorker.made[0]?.terminate).toHaveBeenCalledTimes(1);
  });

  it('says that reading took too long, and ends the worker', async () => {
    // Timers that run as time passes, and that can be overtaken.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    FakeWorker.behaviour = 'silent';
    fakeApi({ [PREVIEW]: () => previewOf('docx', docx()) });
    const dialog = await openPreview('report.docx', DOCX_TYPE);
    await waitFor(() => {
      expect(FakeWorker.made).toHaveLength(1);
    });
    const [worker] = FakeWorker.made;
    vi.advanceTimersByTime(OOXML_TIMEOUT_MS - 100);
    expect(worker?.terminate).not.toHaveBeenCalled();
    vi.advanceTimersByTime(100);
    expect(worker?.terminate).toHaveBeenCalledTimes(1);
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'Reading this document took more than 10 seconds, and was stopped. Download it to open it.',
    );
    expect(within(dialog).getByRole('link', { name: 'Download' }).getAttribute('href')).toBe(
      objectDownloadUrl('photos', 'report.docx'),
    );
  });

  it.each([
    [
      'a zip bomb',
      () => docxOf(para('x'), { parts: { 'word/document.xml': new Uint8Array(8 * 1024 * 1024) } }),
      'This document is too large to preview: it unpacks to more than the console reads. Download it to open it.',
    ],
    [
      'a DOCTYPE',
      () => docxOf('', { document: '<!DOCTYPE d [<!ENTITY lol "lol">]><d>&lol;</d>' }),
      'This document declares a DOCTYPE or entities, which the console never reads. Download it to open it elsewhere.',
    ],
    [
      'bytes that are no document',
      () => new TextEncoder().encode('PK\u0003\u0004 nothing'),
      'This document could not be read: it is damaged, or it is not a Word document. Download it to open it elsewhere.',
    ],
  ])('says why %s is not shown', async (_, bytes, message) => {
    fakeApi({ [PREVIEW]: () => previewOf('docx', bytes()) });
    const dialog = await openPreview('bad.docx', DOCX_TYPE);
    expect((await within(dialog).findByRole('alert')).textContent).toBe(message);
  });

  it('says that the reader could not be loaded', async () => {
    FakeWorker.behaviour = 'fail';
    fakeApi({ [PREVIEW]: () => previewOf('docx', docx()) });
    const dialog = await openPreview('report.docx', DOCX_TYPE);
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The document reader could not be loaded. Reload the console and try again.',
    );
  });
});

describe('the preview of an XLSX', () => {
  const xlsx = () =>
    xlsxOf({
      strings: ['<t>Name</t>', '<t>=HYPERLINK("https://example.invalid","click me")</t>'],
      styles: '<cellXfs><xf numFmtId="0"/><xf numFmtId="14"/></cellXfs>',
      sheets: [
        {
          name: 'People',
          data:
            '<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" s="1"><v>46310</v></c></row>' +
            '<row r="2"><c r="A2" t="s"><v>1</v></c><c r="C2"><f>SUM(1,2)</f><v>3</v></c></row>',
        },
        { name: 'Notes', data: '<row r="1"><c r="A1" t="inlineStr"><is><t>&lt;img src=x onerror=alert(1)&gt;</t></is></c></row>' },
        { name: 'Chart', chart: true },
      ],
    });

  it('shows a sheet at a time as a table of text, with its columns and rows named', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('xlsx', xlsx()) });
    const dialog = await openPreview('people.xlsx', XLSX_TYPE);
    const sheet = await within(dialog).findByRole('group', { name: 'Sheet People of people.xlsx' });
    const rows = within(sheet).getAllByRole('row');
    expect(rows.map((row) => Array.from(row.children, (cell) => cell.textContent))).toEqual([
      ['', 'A', 'B', 'C'],
      ['1', 'Name', '2026-10-15', ''],
      ['2', '=HYPERLINK("https://example.invalid","click me")', '', '3'],
    ]);
    // A formula's text is shown as text: nothing is linked or run.
    expect(sheet.querySelector('a')).toBeNull();
    expect(within(dialog).getByText(/Formulas are not calculated/)).toBeTruthy();
    expect(within(dialog).getByRole('button', { name: 'People' }).getAttribute('aria-pressed')).toBe('true');

    fireEvent.click(within(dialog).getByRole('button', { name: 'Notes' }));
    const notes = within(dialog).getByRole('group', { name: 'Sheet Notes of people.xlsx' });
    expect(within(notes).getByText('<img src=x onerror=alert(1)>')).toBeTruthy();
    expect(notes.querySelector('img')).toBeNull();

    fireEvent.click(within(dialog).getByRole('button', { name: 'Chart' }));
    expect(within(dialog).getByText('This sheet is a chart, which is not shown.')).toBeTruthy();
  });

  it('says that cells were cut short or left out', async () => {
    // Varied, so that it deflates by less than 100:1.
    const random = seeded(23);
    const long = Array.from({ length: 40_000 }, () => String.fromCharCode(97 + Math.floor(random() * 26))).join('');
    fakeApi({
      [PREVIEW]: () =>
        previewOf('xlsx', xlsxOf({ strings: [`<t>${long}</t>`], sheets: [{ name: 'Long', data: '<row r="1"><c r="A1" t="s"><v>0</v></c></row>' }] })),
    });
    const dialog = await openPreview('long.xlsx', XLSX_TYPE);
    expect(await within(dialog).findByText(/Some cells are cut short or not shown: at most 1 000 000 characters/)).toBeTruthy();
    expect(within(dialog).getByRole('group', { name: 'Sheet Long of long.xlsx' }).textContent).toContain(long.slice(0, 32_767));
    expect(within(dialog).getByRole('group', { name: 'Sheet Long of long.xlsx' }).textContent).not.toContain(long.slice(0, 32_768));
  });

  it('says that rows past the first 1 000 are not shown', async () => {
    const data = Array.from({ length: 1001 }, (_, r) => `<row r="${String(r + 1)}"><c><v>${String(r)}</v></c></row>`).join('');
    fakeApi({ [PREVIEW]: () => previewOf('xlsx', xlsxOf({ sheets: [{ name: 'Long', data }] })) });
    const dialog = await openPreview('long.xlsx', XLSX_TYPE);
    expect(await within(dialog).findByText('Only the first 1 000 rows are shown.')).toBeTruthy();
    expect(within(dialog).queryByRole('button', { name: 'Long' })).toBeNull();
  });
});
