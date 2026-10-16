// The files of the console's malicious-file suite (PLAN O.11), made here by
// code: none is downloaded, and none is kept as bytes without what made it.
// The DOCX and XLSX are made with the builders of ../test/ooxml; the
// suite of the server makes its own in internal/domain/preview/previewtest.
import { docxOf, para, picture, relType, xlsxOf, type XlsxParts } from './ooxml';

/** In every file here that carries script or markup: a page that holds it has shown something of the file as markup. */
export const MARKER = 'cactusfs-malicious';

/** The bytes of text whose characters are all below 256, one byte each. */
export function latin1(text: string): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(text.length);
  for (let at = 0; at < text.length; at += 1) {
    const code = text.charCodeAt(at);
    if (code > 0xff) {
      throw new RangeError(`not latin-1 at ${String(at)}`);
    }
    bytes[at] = code;
  }
  return bytes;
}

/**
 * A PDF of these objects - numbered from 1, the first the catalogue - with a
 * cross-reference table whose offsets are those of the objects, so that a
 * reader opens it. `comment`, when not empty, is a line after the header and
 * begins with "%".
 */
export function pdfOf(comment: string, objects: readonly string[]): Uint8Array<ArrayBuffer> {
  let text = '%PDF-1.7\n%âãÏÓ\n';
  if (comment !== '') {
    text += `${comment.replace(/\n/g, ' ')}\n`;
  }
  const offsets = objects.map((object, at) => {
    const offset = text.length;
    text += `${String(at + 1)} 0 obj\n${object}\nendobj\n`;
    return offset;
  });
  const xref = text.length;
  text += `xref\n0 ${String(objects.length + 1)}\n0000000000 65535 f \n`;
  for (const offset of offsets) {
    text += `${String(offset).padStart(10, '0')} 00000 n \n`;
  }
  text += `trailer\n<< /Size ${String(objects.length + 1)} /Root 1 0 R >>\nstartxref\n${String(xref)}\n%%EOF\n`;
  return latin1(text);
}

/**
 * A PDF of one page that asks to run a script when it is opened
 * (/OpenAction, and /Names /JavaScript), and whose page has a link to an
 * address (/URI) and one that launches a program (/Launch).
 */
export function pdfWithActions(): Uint8Array<ArrayBuffer> {
  return pdfOf('', [
    `<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R /Names << /JavaScript << /Names [(${MARKER}) 4 0 R] >> >> >>`,
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Annots [5 0 R 6 0 R] >>',
    `<< /Type /Action /S /JavaScript /JS (app.alert\\('${MARKER}'\\); this.submitForm\\('https://example.invalid/'\\);) >>`,
    `<< /Type /Annot /Subtype /Link /Rect [10 10 190 50] /Border [0 0 0] /A << /S /URI /URI (https://example.invalid/${MARKER}) >> >>`,
    `<< /Type /Annot /Subtype /Link /Rect [10 60 190 100] /Border [0 0 0] /A << /S /Launch /F (calc.exe) /Win << /F (cmd.exe) /P (/c echo ${MARKER}) >> >> >>`,
  ]);
}

/** A PDF whose first comment is a page of HTML with a script. */
export function pdfHtmlPolyglot(): Uint8Array<ArrayBuffer> {
  return pdfOf(`%<html><body><script>alert('${MARKER}')</script></body></html>`, [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>',
  ]);
}

/**
 * A GIF that a browser draws - a 10 799 × 10 screen, one frame - and also a
 * script: "GIF89a" is a name, the screen's width opens a comment that the
 * bytes after the trailer close, and a statement follows.
 */
export function gifScriptPolyglot(): Uint8Array<ArrayBuffer> {
  return latin1(
    'GIF89a/*' +
      '\u000a\u0000\u0000\u0000\u0000' +
      ',\u0000\u0000\u0000\u0000\u0001\u0000\u0001\u0000\u0000\u0002\u0002L\u0001\u0000' +
      ';' +
      `*/=1;alert('${MARKER}');//`,
  );
}

/** An SVG image with a script. */
export function svgWithScript(): Uint8Array<ArrayBuffer> {
  return latin1(
    `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" onload="alert('${MARKER}')"><script>alert('${MARKER}')</script></svg>`,
  );
}

/** Zeros: what deflates by about 1 000 to 1. */
const ZEROS = 8 * 1024 * 1024;

/** One more entry than a document may have (OOXML_BOUNDS.entries is 10 000). */
const TOO_MANY_ENTRIES = 10_001;

/** Empty parts, as many as said, under a directory of their own. */
function emptyParts(count: number): Record<string, string> {
  const parts: Record<string, string> = {};
  for (let at = 0; at < count; at += 1) {
    parts[`bomb/${String(at)}.xml`] = '';
  }
  return parts;
}

/** A DOCX whose document is one part of 8 MiB of zeros: a ratio of about 1 000. */
export function docxRatioBomb(): Uint8Array<ArrayBuffer> {
  return docxOf(para('x'), { parts: { 'word/document.xml': new Uint8Array(ZEROS) } });
}

/** A DOCX of more parts than a document may have. */
export function docxManyParts(): Uint8Array<ArrayBuffer> {
  return docxOf(para('x'), { parts: emptyParts(TOO_MANY_ENTRIES) });
}

/** An XLSX whose one sheet is 8 MiB of zeros. */
export function xlsxRatioBomb(): Uint8Array<ArrayBuffer> {
  return xlsxOf({ sheets: [{ name: 'One' }], parts: { 'xl/worksheets/sheet1.xml': new Uint8Array(ZEROS) } });
}

/** An XLSX of more parts than a document may have. */
export function xlsxManyParts(): Uint8Array<ArrayBuffer> {
  return xlsxOf({ sheets: [{ name: 'One', data: '' }], parts: emptyParts(TOO_MANY_ENTRIES) });
}

/**
 * An XLSX of `sheets` sheets, each of `cells` numbers that deflate by much
 * less than 100 to 1: every part within the bounds, all of them together
 * past any total smaller than their sum.
 */
export function xlsxOfManySheets(sheets: number, cells: number): Uint8Array<ArrayBuffer> {
  let seed = 7;
  const next = () => {
    seed = (seed * 1_103_515_245 + 12_345) % 2 ** 31;
    return seed;
  };
  const workbook: XlsxParts = { sheets: [] };
  for (let sheet = 0; sheet < sheets; sheet += 1) {
    const row = Array.from({ length: cells }, (_, at) => `<c r="A${String(at + 1)}"><v>${String(next())}</v></c>`).join('');
    workbook.sheets.push({ name: `S${String(sheet + 1)}`, data: `<row r="1">${row}</row>` });
  }
  return xlsxOf(workbook);
}

/** Billion laughs: entities of ten entities, nine deep. */
export const BILLION_LAUGHS =
  '<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol">' +
  Array.from(
    { length: 9 },
    (_, level) => `<!ENTITY lol${String(level + 1)} "${`&lol${level === 0 ? '' : String(level)};`.repeat(10)}">`,
  ).join('') +
  ']>';

/** An external entity that names a file of the machine. */
export const EXTERNAL_ENTITY = '<?xml version="1.0"?><!DOCTYPE x [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>';

/** A DOCX whose document part begins with `prologue` and uses its entity. */
export function docxWithPrologue(prologue: string, entity: string): Uint8Array<ArrayBuffer> {
  return docxOf('', {
    document: `${prologue}<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>&${entity};</w:t></w:r></w:p></w:body></w:document>`,
  });
}

/** An XLSX whose shared strings begin with `prologue` and use its entity. */
export function xlsxWithPrologue(prologue: string, entity: string): Uint8Array<ArrayBuffer> {
  return xlsxOf({
    strings: ['<t>x</t>'],
    sheets: [{ name: 'One', data: '<row r="1"><c r="A1" t="s"><v>0</v></c></row>' }],
    parts: {
      'xl/sharedStrings.xml': `${prologue}<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>&${entity};</t></si></sst>`,
    },
  });
}

/** A DOCX whose text is markup: escaped, in CDATA, and a hyperlink to an address and to script. */
export function docxWithMarkup(): Uint8Array<ArrayBuffer> {
  return docxOf(
    para(`&lt;script&gt;alert('${MARKER}')&lt;/script&gt;`) +
      `<w:p><w:r><w:t><![CDATA[<img src=x onerror="alert('${MARKER}')">]]></w:t></w:r></w:p>` +
      `<w:p><w:hyperlink r:id="link"><w:r><w:t>click ${MARKER}</w:t></w:r></w:hyperlink></w:p>` +
      `<w:p><w:hyperlink r:id="script"><w:r><w:t>run ${MARKER}</w:t></w:r></w:hyperlink></w:p>`,
    {
      relationships: [
        ['link', relType('hyperlink'), 'https://example.invalid/', true],
        ['script', relType('hyperlink'), 'javascript:alert(1)', true],
      ],
    },
  );
}

/** An XLSX whose cells begin `=HYPERLINK(`: as shared and inline strings, and a formula with its saved value. */
export function xlsxWithHyperlinks(): Uint8Array<ArrayBuffer> {
  return xlsxOf({
    strings: [`<t>=HYPERLINK("https://example.invalid/${MARKER}","click")</t>`],
    sheets: [
      {
        name: 'Links',
        data:
          '<row r="1"><c r="A1" t="s"><v>0</v></c>' +
          '<c r="B1" t="inlineStr"><is><t>=HYPERLINK("javascript:alert(1)","run")</t></is></c>' +
          '<c r="C1" t="str"><f>HYPERLINK("https://example.invalid/","go")</f><v>go</v></c></row>',
      },
    ],
  });
}

/** A DOCX whose images lie by their names: SVG in a .png, a GIF in a .png. */
export function docxWithLyingImages(gif: Uint8Array): Uint8Array<ArrayBuffer> {
  return docxOf(`<w:p>${picture('r:embed', 'svg')}${picture('r:embed', 'gif')}</w:p>`, {
    relationships: [
      ['svg', relType('image'), 'media/image1.png'],
      ['gif', relType('image'), 'media/image2.png'],
    ],
    parts: { 'word/media/image1.png': svgWithScript(), 'word/media/image2.png': gif },
  });
}
