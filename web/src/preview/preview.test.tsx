import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { objectDownloadUrl, onLocked } from '../api/client';
import { DeletionsProvider } from '../deletions/DeletionsProvider';
import { ObjectsScreen } from '../screens/ObjectsScreen';
import { fakeApi, json, problem, refusal, sessionLocked, type FakeApi } from '../test/fakeApi';
import { UploadsProvider } from '../uploads/UploadsProvider';
import { PREVIEW_MAX_BYTES, PreviewTooLargeError, readBlob } from './fetch';
import { offersPreview } from './kinds';
import { PreviewButton } from './PreviewButton';

const PREVIEW = 'GET /buckets/photos/object/preview';

// The first bytes of each image kind; what follows does not matter here.
const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4]);

/** What the API answers a preview it allows: octet-stream, an attachment, and what the bytes are. */
function previewOf(kind: string, body: BodyInit | null, truncated = false): Response {
  return new Response(body, {
    status: 200,
    headers: {
      'Content-Type': 'application/octet-stream',
      'Content-Disposition': 'attachment',
      'X-CactusFS-Preview-Kind': kind,
      'X-CactusFS-Preview-Truncated': String(truncated),
    },
  });
}

/** The bytes of a text, as UTF-8. */
function utf8(text: string): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(text);
}

/** The blob addresses the dialog made and gave back; jsdom has neither function. */
interface BlobUrls {
  made: Blob[];
  create: ReturnType<typeof vi.fn>;
  revoke: ReturnType<typeof vi.fn>;
}

let urls: BlobUrls;

beforeEach(() => {
  const made: Blob[] = [];
  const create = vi.fn((blob: Blob) => {
    made.push(blob);
    return `blob:preview-${String(made.length)}`;
  });
  const revoke = vi.fn();
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, writable: true, value: create });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: revoke });
  urls = { made, create, revoke };
});

afterEach(() => {
  Reflect.deleteProperty(URL, 'createObjectURL');
  Reflect.deleteProperty(URL, 'revokeObjectURL');
});

/** The button of one object of the bucket "photos", as the details show it. */
function renderButton(objectKey = 'cat.png', contentType = 'image/png') {
  return render(<PreviewButton bucket="photos" objectKey={objectKey} contentType={contentType} />);
}

/** Opens the preview and returns its dialog. */
async function openPreview(objectKey = 'cat.png'): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
  return screen.findByRole('dialog', { name: `Preview of ${objectKey}` });
}

/** Closes the preview with its button. */
function close(dialog: HTMLElement) {
  fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
}

describe('which objects are offered a preview', () => {
  it.each([
    'image/png',
    'image/jpeg',
    'image/jpg',
    'image/gif',
    'image/webp',
    'application/pdf',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
    'text/plain',
    'text/plain; charset=utf-8',
    'TEXT/CSV',
    'application/json',
    'application/octet-stream',
    '',
  ])('offers it for %j', (type) => {
    expect(offersPreview(type)).toBe(true);
  });

  it.each([
    'image/svg+xml',
    'text/html',
    'application/xhtml+xml',
    'application/xml',
    'text/xml',
    'text/javascript',
    'application/javascript',
    'application/vnd.ms-excel',
    'application/msword',
    'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    'image/tiff',
    'video/mp4',
  ])('offers nothing for %j', (type) => {
    expect(offersPreview(type)).toBe(false);
  });

  it('asks for nothing until the button is pressed, and shows no button for a type it does not offer', () => {
    const api = fakeApi({ [PREVIEW]: () => previewOf('png', PNG) });
    renderButton('page.html', 'text/html');
    expect(screen.queryByRole('button', { name: 'Preview' })).toBeNull();
    renderButton();
    expect(screen.getByRole('button', { name: 'Preview' })).toBeTruthy();
    expect(api.calls).toHaveLength(0);
  });
});

describe('the preview dialog', () => {
  it('shows an image from a blob address of the kind the API named, and revokes it on close', async () => {
    const api = fakeApi({ [PREVIEW]: () => previewOf('png', PNG) });
    renderButton();
    const dialog = await openPreview();
    expect(dialog.getAttribute('aria-modal')).toBe('true');

    const image = await within(dialog).findByRole('img', { name: 'The image cat.png' });
    expect(image.getAttribute('src')).toBe('blob:preview-1');
    const [call] = api.callsTo('GET', '/buckets/photos/object/preview');
    expect(call?.query.get('key')).toBe('cat.png');
    expect(call?.query.has('version_id')).toBe(false);
    expect(urls.made[0]?.type).toBe('image/png');
    expect(urls.made[0]?.size).toBe(PNG.length);
    expect(urls.revoke).not.toHaveBeenCalled();

    close(dialog);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(urls.revoke).toHaveBeenCalledWith('blob:preview-1');
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Preview' }));
  });

  it.each([
    ['png', 'image/png'],
    ['jpeg', 'image/jpeg'],
    ['gif', 'image/gif'],
    ['webp', 'image/webp'],
  ])('types the blob of a %s as %s', async (kind, type) => {
    fakeApi({ [PREVIEW]: () => previewOf(kind, PNG) });
    renderButton();
    const dialog = await openPreview();
    await within(dialog).findByRole('img');
    expect(urls.made.map((blob) => blob.type)).toEqual([type]);
  });

  it('chooses the renderer by the kind the API names, never by the stored type or the key', async () => {
    // Stored as an image, and the API says it is text: it is text.
    fakeApi({ [PREVIEW]: () => previewOf('text', utf8('<svg onload="alert(1)"></svg>')) });
    renderButton('cat.png', 'image/png');
    const dialog = await openPreview();
    const text = await within(dialog).findByLabelText('Contents of cat.png');
    expect(text.textContent).toBe('<svg onload="alert(1)"></svg>');
    expect(dialog.querySelector('svg, img')).toBeNull();
    expect(urls.create).not.toHaveBeenCalled();
  });

  it('draws no image for a kind it does not know - an SVG would be one', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('svg', utf8('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>')) });
    renderButton('cat.png', 'application/octet-stream');
    const dialog = await openPreview();
    expect(await within(dialog).findByText('This kind of file cannot be shown here yet.')).toBeTruthy();
    expect(dialog.querySelector('img, svg, script')).toBeNull();
    expect(urls.create).not.toHaveBeenCalled();
  });

  it('shows text as text, markup and all', async () => {
    const body = '<script>alert("x")</script>\n<img src=x onerror=alert(1)>\nline 3';
    fakeApi({ [PREVIEW]: () => previewOf('text', utf8(body)) });
    renderButton('notes.txt', 'text/plain');
    const dialog = await openPreview('notes.txt');
    const text = await within(dialog).findByLabelText('Contents of notes.txt');
    expect(text.tagName).toBe('PRE');
    expect(text.textContent).toBe(body);
    expect(document.querySelector('script, img')).toBeNull();
    expect(within(dialog).queryByText(/first 1 MiB/)).toBeNull();
  });

  it('shows bytes that are not UTF-8 without failing, and drops a byte order mark', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('text', new Uint8Array([0xef, 0xbb, 0xbf, 0x61, 0xff, 0x62])) });
    renderButton('notes.txt', 'text/plain');
    const dialog = await openPreview('notes.txt');
    const text = await within(dialog).findByLabelText('Contents of notes.txt');
    expect(text.textContent).toBe('a�b');
  });

  it('shows JSON as the text it is', async () => {
    const body = '{"a": 1, "big": 12345678901234567890, "html": "<b>x</b>"}';
    fakeApi({ [PREVIEW]: () => previewOf('json', utf8(body)) });
    renderButton('data.json', 'application/json');
    const dialog = await openPreview('data.json');
    const text = await within(dialog).findByLabelText('Contents of data.json');
    expect(text.textContent).toBe(body);
    expect(dialog.querySelector('b')).toBeNull();
  });

  it('says that only the first 1 MiB is shown when the API says there is more, and leaves a character it cut off out', async () => {
    // "é" is two bytes: the API's cut fell between them.
    fakeApi({ [PREVIEW]: () => previewOf('text', new Uint8Array([0x61, 0x62, 0xc3]), true) });
    renderButton('notes.txt', 'text/plain');
    const dialog = await openPreview('notes.txt');
    const text = await within(dialog).findByLabelText('Contents of notes.txt');
    expect(text.textContent).toBe('ab');
    expect(within(dialog).getByText(/The first 1 MiB is shown/)).toBeTruthy();
  });

  it('shows CSV as a table of text', async () => {
    const body = 'name,note\r\n"Smith, J","said ""hi"""\n<b>bold</b>,"two\nlines"\n';
    fakeApi({ [PREVIEW]: () => previewOf('csv', utf8(body)) });
    renderButton('people.csv', 'text/csv');
    const dialog = await openPreview('people.csv');
    const table = await within(dialog).findByRole('table');
    const rows = within(table).getAllByRole('row').map((row) => within(row).getAllByRole('cell').map((cell) => cell.textContent));
    expect(rows).toEqual([
      ['name', 'note'],
      ['Smith, J', 'said "hi"'],
      ['<b>bold</b>', 'two\nlines'],
    ]);
    expect(table.querySelector('b')).toBeNull();
    expect(within(dialog).queryByText(/Only the first/)).toBeNull();
  });

  it('shows the first 1 000 rows of a CSV and says so', async () => {
    const body = Array.from({ length: 1500 }, (_, i) => `row ${String(i + 1)},x`).join('\n');
    fakeApi({ [PREVIEW]: () => previewOf('csv', utf8(body)) });
    renderButton('big.csv', 'text/csv');
    const dialog = await openPreview('big.csv');
    const table = await within(dialog).findByRole('table');
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(1000);
    expect(rows.at(-1)?.textContent).toBe('row 1000x');
    expect(within(dialog).getByText('Only the first 1 000 rows are shown.')).toBeTruthy();
  });

  it('leaves out the last row of a CSV the API cut off', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('csv', utf8('a,b\nc,"d is cut'), true) });
    renderButton('cut.csv', 'text/csv');
    const dialog = await openPreview('cut.csv');
    const table = await within(dialog).findByRole('table');
    expect(within(table).getAllByRole('row').map((row) => row.textContent)).toEqual(['ab']);
    expect(within(dialog).getByText(/The first 1 MiB is shown/)).toBeTruthy();
    expect(within(dialog).getByText('Only the first 1 row is shown.')).toBeTruthy();
  });

  it.each(['pptx', 'heic'])('says that a kind it does not know (%s) cannot be shown here yet, and offers the download', async (kind) => {
    fakeApi({ [PREVIEW]: () => previewOf(kind, new Uint8Array([0x25, 0x50, 0x44, 0x46])) });
    renderButton(`report.${kind}`, 'application/octet-stream');
    const dialog = await openPreview(`report.${kind}`);
    expect(await within(dialog).findByText('This kind of file cannot be shown here yet.')).toBeTruthy();
    const download = within(dialog).getByRole('link', { name: 'Download' });
    expect(download.getAttribute('href')).toBe(objectDownloadUrl('photos', `report.${kind}`));
    expect(download.hasAttribute('download')).toBe(true);
    expect(urls.create).not.toHaveBeenCalled();
  });

  it.each([
    [403, 'AccessDenied', 'You neither own this object nor may preview it.'],
    [404, 'NoSuchObject', 'The object is not there any more: it was deleted, or its bucket was.'],
    [413, 'PreviewTooLarge', 'This object is too large to preview.'],
    [415, 'NotPreviewable', 'This object is not of a kind the console can show, or its type and its contents do not agree.'],
  ])('says what a %i means, offers the download and no retry', async (status, code, message) => {
    fakeApi({ [PREVIEW]: () => refusal(status, code, 'Refused') });
    renderButton();
    const dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(message);
    expect(within(dialog).getByRole('link', { name: 'Download' }).getAttribute('href')).toBe(
      objectDownloadUrl('photos', 'cat.png'),
    );
    expect(within(dialog).queryByRole('button', { name: 'Try again' })).toBeNull();
    expect(dialog.querySelector('img')).toBeNull();
  });

  it('says that an object with a customer key needs an S3 client with it, and offers no download', async () => {
    fakeApi({ [PREVIEW]: () => refusal(409, 'CustomerKeyRequired', 'Customer key required') });
    renderButton();
    const dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'This object is encrypted with a key of its own (SSE-C), which the console never holds: it can be neither previewed nor downloaded here. Download it with an S3 client that sends its customer key.',
    );
    expect(within(dialog).queryByRole('link', { name: 'Download' })).toBeNull();
    expect(within(dialog).queryByRole('button', { name: 'Try again' })).toBeNull();
  });

  it('cancels the bytes of a kind it does not draw, without reading them', async () => {
    for (const kind of ['pptx', 'heic', '']) {
      const pulled = vi.fn();
      const cancelled = vi.fn();
      const body = new ReadableStream<Uint8Array>({ pull: pulled, cancel: cancelled }, { highWaterMark: 0 });
      fakeApi({ [PREVIEW]: () => previewOf(kind, body) });
      const { unmount } = renderButton(`file.${kind}`, 'application/octet-stream');
      const dialog = await openPreview(`file.${kind}`);
      await within(dialog).findByText('This kind of file cannot be shown here yet.');
      await waitFor(() => {
        expect(cancelled).toHaveBeenCalledTimes(1);
      });
      expect(pulled).not.toHaveBeenCalled();
      close(dialog);
      unmount();
    }
  });

  it('reads nothing of an answer that says it is larger than the console takes', async () => {
    const cancelled = vi.fn();
    const body = new ReadableStream<Uint8Array>({ cancel: cancelled }, { highWaterMark: 0 });
    fakeApi({
      [PREVIEW]: () => {
        const answer = previewOf('png', body);
        answer.headers.set('Content-Length', String(PREVIEW_MAX_BYTES + 1));
        return answer;
      },
    });
    renderButton();
    const dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'This object is larger than the 256 MiB the console takes for a preview. Download it to see it.',
    );
    expect(cancelled).toHaveBeenCalledTimes(1);
    expect(urls.create).not.toHaveBeenCalled();
    expect(within(dialog).getByRole('link', { name: 'Download' })).toBeTruthy();
  });

  it('gives the bytes up when the dialog closes while they are read, and makes no address', async () => {
    const cancelled = vi.fn();
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(PNG);
      },
      cancel: cancelled,
    });
    fakeApi({ [PREVIEW]: () => previewOf('png', body) });
    renderButton();
    const dialog = await openPreview();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(within(dialog).getByRole('status').textContent).toContain('Loading');
    close(dialog);
    await waitFor(() => {
      expect(cancelled).toHaveBeenCalledTimes(1);
    });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(urls.create).not.toHaveBeenCalled();
    expect(urls.revoke).not.toHaveBeenCalled();
  });

  it('says a locked session previews nothing, and the lock is told', async () => {
    const locked = vi.fn();
    const stop = onLocked(locked);
    try {
      fakeApi({ [PREVIEW]: () => sessionLocked() });
      renderButton();
      const dialog = await openPreview();
      expect((await within(dialog).findByRole('alert')).textContent).toBe(
        'Your session is locked: nothing is previewed until it is unlocked.',
      );
      await waitFor(() => {
        expect(locked).toHaveBeenCalledTimes(1);
      });
    } finally {
      stop();
    }
  });

  it('says to try again after a 429, and does so', async () => {
    let answers = 0;
    const api = fakeApi({
      [PREVIEW]: () => {
        answers += 1;
        return answers === 1
          ? refusal(429, 'TooManyRequests', 'Too many requests', { 'Retry-After': '2' })
          : previewOf('png', PNG);
      },
    });
    renderButton();
    const dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'Too many previews are being shown at once. Try again in 2 seconds.',
    );
    fireEvent.click(within(dialog).getByRole('button', { name: 'Try again' }));
    expect(await within(dialog).findByRole('img')).toBeTruthy();
    expect(api.callsTo('GET', '/buckets/photos/object/preview')).toHaveLength(2);
  });

  it('says a 429 without Retry-After as "in a moment"', async () => {
    fakeApi({ [PREVIEW]: () => refusal(429, 'TooManyRequests', 'Too many requests') });
    renderButton();
    const dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'Too many previews are being shown at once. Try again in a moment.',
    );
  });

  it('says that the server could not be reached, and words any other answer', async () => {
    vi.stubGlobal('fetch', () => Promise.reject(new TypeError('Failed to fetch')));
    renderButton();
    let dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe('The server could not be reached. Try again.');
    close(dialog);

    fakeApi({ [PREVIEW]: () => problem(500, 'Internal error', 'the disk is on fire') });
    dialog = await openPreview();
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The preview could not be loaded: Internal error: the disk is on fire.',
    );
    expect(within(dialog).getByRole('button', { name: 'Try again' })).toBeTruthy();
  });

  it('never shows an image whose bytes ended early', async () => {
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(PNG);
        controller.error(new TypeError('terminated'));
      },
    });
    fakeApi({ [PREVIEW]: () => previewOf('gif', body) });
    renderButton('cat.gif', 'image/gif');
    const dialog = await openPreview('cat.gif');
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The preview could not be read to the end: terminated.',
    );
    expect(dialog.querySelector('img')).toBeNull();
    expect(urls.create).not.toHaveBeenCalled();
  });

  it('says so when the browser cannot read the image', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('png', PNG) });
    renderButton();
    const dialog = await openPreview();
    fireEvent.error(await within(dialog).findByRole('img'));
    expect((await within(dialog).findByRole('alert')).textContent).toBe(
      'The image could not be shown: the browser could not read it.',
    );
    expect(within(dialog).queryByRole('button', { name: 'Actual size' })).toBeNull();
  });

  it('toggles an image between fitted and its own size', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('png', PNG) });
    renderButton();
    const dialog = await openPreview();
    const image = await within(dialog).findByRole('img');
    const zoom = within(dialog).getByRole('button', { name: 'Actual size' });
    expect(zoom.getAttribute('aria-pressed')).toBe('false');
    expect(image.parentElement?.classList.contains('preview-image-actual')).toBe(false);
    fireEvent.click(zoom);
    expect(zoom.getAttribute('aria-pressed')).toBe('true');
    expect(image.parentElement?.classList.contains('preview-image-actual')).toBe(true);
    fireEvent.click(zoom);
    expect(zoom.getAttribute('aria-pressed')).toBe('false');
  });
});

describe('the dialog as a dialog', () => {
  /** A preview the API takes its time over: the request is kept, to see it given up. */
  function slowPreview(): { request: () => Request | undefined } {
    let seen: Request | undefined;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      seen = request;
      return new Promise<Response>((_, reject) => {
        request.signal.addEventListener('abort', () => {
          reject(new DOMException('The operation was aborted.', 'AbortError'));
        });
      });
    });
    return { request: () => seen };
  }

  it('has the keyboard on Close, is labelled, and makes the rest of the page inert', async () => {
    slowPreview();
    const { container } = renderButton();
    const dialog = await openPreview();
    expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: 'Close' }));
    expect(container.hasAttribute('inert')).toBe(true);
    close(dialog);
    expect(container.hasAttribute('inert')).toBe(false);
  });

  it('closes with Escape, gives the request up and puts the keyboard back on Preview', async () => {
    const slow = slowPreview();
    renderButton();
    const dialog = await openPreview();
    await waitFor(() => {
      expect(slow.request()).toBeDefined();
    });
    expect(within(dialog).getByRole('status').textContent).toContain('Loading');

    fireEvent.keyDown(within(dialog).getByRole('button', { name: 'Close' }), { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(slow.request()?.signal.aborted).toBe(true);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Preview' }));
  });

  it('keeps the keyboard inside: Tab and Shift+Tab go round', async () => {
    fakeApi({ [PREVIEW]: () => refusal(403, 'AccessDenied', 'Access denied') });
    renderButton();
    const dialog = await openPreview();
    await within(dialog).findByRole('alert');
    const download = within(dialog).getByRole('link', { name: 'Download' });
    const closeButton = within(dialog).getByRole('button', { name: 'Close' });

    closeButton.focus();
    fireEvent.keyDown(closeButton, { key: 'Tab' });
    expect(document.activeElement).toBe(download);
    fireEvent.keyDown(download, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(closeButton);
  });

  it('takes the keyboard back when something outside the dialog gets it', async () => {
    slowPreview();
    render(
      <>
        <button type="button">Outside</button>
        <PreviewButton bucket="photos" objectKey="cat.png" contentType="image/png" />
      </>,
    );
    const dialog = await openPreview();
    screen.getByRole('button', { name: 'Outside' }).focus();
    expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: 'Close' }));
  });

  it('closes on a click beside it, and not on a click in it nor on a selection let go beside it', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('png', PNG) });
    renderButton();
    const dialog = await openPreview();
    const backdrop = dialog.parentElement as HTMLElement;
    const image = await within(dialog).findByRole('img');
    fireEvent.mouseDown(image);
    fireEvent.click(image);
    expect(screen.getByRole('dialog')).toBe(dialog);
    // Pressed in the dialog, let go beside it: the browser clicks the backdrop.
    fireEvent.mouseDown(image);
    fireEvent.click(backdrop);
    expect(screen.getByRole('dialog')).toBe(dialog);
    fireEvent.mouseDown(backdrop);
    fireEvent.click(backdrop);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(urls.revoke).toHaveBeenCalledWith('blob:preview-1');
  });

  it('closes with Escape when a click on its text left the keyboard nowhere', async () => {
    fakeApi({ [PREVIEW]: () => previewOf('text', utf8('some text')) });
    renderButton('notes.txt', 'text/plain');
    const dialog = await openPreview('notes.txt');
    await within(dialog).findByLabelText('Contents of notes.txt');
    expect(dialog.getAttribute('tabindex')).toBe('-1');
    (document.activeElement as HTMLElement).blur();
    expect(document.activeElement).toBe(document.body);
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Preview' }));
  });

  it('gives back only the inertness it gave, and the page its own scrolling', async () => {
    slowPreview();
    const already = document.createElement('div');
    already.setAttribute('inert', '');
    document.body.append(already);
    document.body.style.overflow = 'scroll';
    try {
      const { container } = renderButton();
      const dialog = await openPreview();
      expect(container.hasAttribute('inert')).toBe(true);
      expect(document.body.style.overflow).toBe('hidden');
      close(dialog);
      expect(container.hasAttribute('inert')).toBe(false);
      expect(already.hasAttribute('inert')).toBe(true);
      expect(document.body.style.overflow).toBe('scroll');
    } finally {
      already.remove();
      document.body.style.overflow = '';
    }
  });
});

describe('the preview in the object browser', () => {
  function browse(api: FakeApi) {
    api.on({
      'GET /buckets/photos/objects': () =>
        json(200, {
          objects: [
            { key: 'cat.png', size: 12, etag: '"e"', last_modified: '2026-10-01T00:00:00Z' },
            { key: 'page.html', size: 12, etag: '"f"', last_modified: '2026-10-01T00:00:00Z' },
          ],
          common_prefixes: [],
          truncated: false,
          access_included: false,
        }),
      'GET /buckets/photos/object/metadata': (call) => {
        const key = call.query.get('key') ?? '';
        return json(200, {
          bucket: 'photos',
          key,
          size: 12,
          etag: '"e"',
          last_modified: '2026-10-01T00:00:00Z',
          content_type: key === 'cat.png' ? 'image/png' : 'text/html',
          encryption: 'SSE-S3',
        });
      },
      [PREVIEW]: () => previewOf('png', PNG),
    });
    return render(
      <UploadsProvider>
        <DeletionsProvider>
          <ObjectsScreen bucket="photos" prefix="" />
        </DeletionsProvider>
      </UploadsProvider>,
    );
  }

  it('offers Preview in the opened row by the stored type, and Escape closes the preview, not the details', async () => {
    const api = fakeApi();
    browse(api);
    fireEvent.click(await screen.findByRole('button', { name: 'Details of page.html' }));
    const html = screen.getByRole('region', { name: 'Details of page.html' });
    await within(html).findByText('text/html');
    expect(within(html).queryByRole('button', { name: 'Preview' })).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Details of cat.png' }));
    const region = screen.getByRole('region', { name: 'Details of cat.png' });
    fireEvent.click(await within(region).findByRole('button', { name: 'Preview' }));
    const dialog = await screen.findByRole('dialog', { name: 'Preview of cat.png' });
    await within(dialog).findByRole('img');

    fireEvent.keyDown(dialog, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('region', { name: 'Details of cat.png' })).toBe(region);
    expect(document.activeElement).toBe(within(region).getByRole('button', { name: 'Preview' }));
    expect(urls.revoke).toHaveBeenCalledWith('blob:preview-1');
    expect(api.callsTo('GET', '/buckets/photos/object/preview')).toHaveLength(1);
  });
});

describe('reading the bytes of a preview', () => {
  /** A body of these chunks. */
  function bodyOf(...chunks: Uint8Array<ArrayBuffer>[]): ReadableStream<Uint8Array<ArrayBuffer>> {
    return new ReadableStream<Uint8Array<ArrayBuffer>>({
      start(controller) {
        for (const chunk of chunks) {
          controller.enqueue(chunk);
        }
        controller.close();
      },
    });
  }

  it('reads them into one Blob', async () => {
    const blob = await readBlob(bodyOf(utf8('ab'), utf8('cd')), new AbortController().signal, 4);
    expect(await blob.text()).toBe('abcd');
  });

  it('stops at more than it takes, whatever the answer said', async () => {
    await expect(readBlob(bodyOf(utf8('ab'), utf8('cde')), new AbortController().signal, 4)).rejects.toBeInstanceOf(
      PreviewTooLargeError,
    );
  });

  it('takes no more than the largest preview the contract allows', () => {
    expect(PREVIEW_MAX_BYTES).toBe(256 * 1024 * 1024);
  });
});
