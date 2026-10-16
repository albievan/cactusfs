// Small DOCX and XLSX files made in the tests, with fflate: no binary
// fixture is kept or downloaded. Each is as little of a package as the
// readers need - what Word and Excel write has more parts, which the
// readers pass over.
import { strToU8, zipSync, type Zippable } from 'fflate';

/** WordprocessingML's namespace, transitional. */
export const W = 'http://schemas.openxmlformats.org/wordprocessingml/2006/main';

/** SpreadsheetML's namespace, transitional. */
export const S = 'http://schemas.openxmlformats.org/spreadsheetml/2006/main';

/** The namespace of relationship ids in parts. */
export const R = 'http://schemas.openxmlformats.org/officeDocument/2006/relationships';

/** The namespace of relationships parts. */
const PR = 'http://schemas.openxmlformats.org/package/2006/relationships';

/** The type of a relationship of OOXML, by its last word. */
export function relType(name: string): string {
  return `http://schemas.openxmlformats.org/officeDocument/2006/relationships/${name}`;
}

/** A relationships part: [id, type, target, external?]. */
export function rels(entries: readonly (readonly [string, string, string, boolean?])[]): string {
  const items = entries
    .map(
      ([id, type, target, external]) =>
        `<Relationship Id="${id}" Type="${type}" Target="${target}"${external === true ? ' TargetMode="External"' : ''}/>`,
    )
    .join('');
  return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="${PR}">${items}</Relationships>`;
}

/** The bytes of a zip of these parts: text as UTF-8, bytes as they are. Deflated unless `level` is 0. */
export function zipOf(parts: Record<string, string | Uint8Array>, level: 0 | 6 = 6): Uint8Array<ArrayBuffer> {
  const files: Zippable = {};
  for (const [name, content] of Object.entries(parts)) {
    files[name] = [typeof content === 'string' ? strToU8(content) : content, { level }];
  }
  return zipSync(files);
}

/** What a DOCX made here holds besides its body. */
export interface DocxParts {
  styles?: string;
  numbering?: string;
  /** More relationships of the document: [id, type, target, external?]. */
  relationships?: (readonly [string, string, string, boolean?])[];
  /** More parts, or parts replaced, by their names in the zip. */
  parts?: Record<string, string | Uint8Array>;
  /** Replaces the whole of word/document.xml. */
  document?: string;
}

/** A DOCX whose body is `body` (the XML inside `w:body`). */
export function docxOf(body: string, extra: DocxParts = {}): Uint8Array<ArrayBuffer> {
  const relationships: (readonly [string, string, string, boolean?])[] = [...(extra.relationships ?? [])];
  const parts: Record<string, string | Uint8Array> = {
    '[Content_Types].xml': '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
    '_rels/.rels': rels([['rId1', relType('officeDocument'), 'word/document.xml']]),
    'word/document.xml':
      extra.document ??
      `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="${W}" xmlns:r="${R}" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"><w:body>${body}</w:body></w:document>`,
  };
  if (extra.styles !== undefined) {
    parts['word/styles.xml'] = `<w:styles xmlns:w="${W}">${extra.styles}</w:styles>`;
    relationships.push(['rIdStyles', relType('styles'), 'styles.xml']);
  }
  if (extra.numbering !== undefined) {
    parts['word/numbering.xml'] = `<w:numbering xmlns:w="${W}">${extra.numbering}</w:numbering>`;
    relationships.push(['rIdNumbering', relType('numbering'), 'numbering.xml']);
  }
  parts['word/_rels/document.xml.rels'] = rels(relationships);
  // Last, so that a test may replace any part.
  Object.assign(parts, extra.parts);
  return zipOf(parts);
}

/** A paragraph of plain text runs. */
export function para(text: string, properties = ''): string {
  return `<w:p>${properties === '' ? '' : `<w:pPr>${properties}</w:pPr>`}<w:r><w:t xml:space="preserve">${text}</w:t></w:r></w:p>`;
}

/** What an XLSX made here holds. */
export interface XlsxParts {
  /** Its sheets: a name, and the XML inside `sheetData` - or a chart sheet. */
  sheets: { name: string; data?: string; state?: string; chart?: boolean }[];
  /** Shared strings, in order: each the XML inside `si`. */
  strings?: string[];
  /** The XML inside `styleSheet`. */
  styles?: string;
  /** Attributes of `workbookPr`. */
  workbookPr?: string;
  /** More parts, or parts replaced, by their names in the zip. */
  parts?: Record<string, string | Uint8Array>;
}

/** An XLSX of these sheets. */
export function xlsxOf(workbook: XlsxParts): Uint8Array<ArrayBuffer> {
  const relationships: [string, string, string][] = [];
  const parts: Record<string, string | Uint8Array> = {
    '[Content_Types].xml': '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
    '_rels/.rels': rels([['rId1', relType('officeDocument'), 'xl/workbook.xml']]),
  };
  const sheets = workbook.sheets
    .map((sheet, at) => {
      const id = `rIdSheet${String(at + 1)}`;
      if (sheet.chart === true) {
        relationships.push([id, relType('chartsheet'), `chartsheets/sheet${String(at + 1)}.xml`]);
      } else {
        relationships.push([id, relType('worksheet'), `worksheets/sheet${String(at + 1)}.xml`]);
        parts[`xl/worksheets/sheet${String(at + 1)}.xml`] =
          `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="${S}" xmlns:r="${R}"><sheetData>${sheet.data ?? ''}</sheetData></worksheet>`;
      }
      return `<sheet name="${sheet.name}" sheetId="${String(at + 1)}"${sheet.state === undefined ? '' : ` state="${sheet.state}"`} r:id="${id}"/>`;
    })
    .join('');
  parts['xl/workbook.xml'] =
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="${S}" xmlns:r="${R}"><workbookPr ${workbook.workbookPr ?? ''}/><sheets>${sheets}</sheets></workbook>`;
  if (workbook.strings !== undefined) {
    relationships.push(['rIdStrings', relType('sharedStrings'), 'sharedStrings.xml']);
    parts['xl/sharedStrings.xml'] =
      `<sst xmlns="${S}" count="${String(workbook.strings.length)}">${workbook.strings.map((s) => `<si>${s}</si>`).join('')}</sst>`;
  }
  if (workbook.styles !== undefined) {
    relationships.push(['rIdStyles', relType('styles'), 'styles.xml']);
    parts['xl/styles.xml'] = `<styleSheet xmlns="${S}">${workbook.styles}</styleSheet>`;
  }
  parts['xl/_rels/workbook.xml.rels'] = rels(relationships);
  Object.assign(parts, workbook.parts);
  return zipOf(parts);
}

/** The first bytes of a PNG of this size: its signature and IHDR. Enough for the readers; no browser draws it. */
export function pngOf(width: number, height: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(33);
  bytes.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52]);
  const view = new DataView(bytes.buffer);
  view.setUint32(16, width);
  view.setUint32(20, height);
  bytes.set([8, 6, 0, 0, 0], 24);
  return bytes;
}

/**
 * A GIF of this screen whose frames are these (left, top, width, height),
 * each with one block of data, ending with its trailer unless `trailer` is
 * false. Enough for the readers; no browser draws it.
 */
export function gifOf(
  width: number,
  height: number,
  frames: readonly (readonly [number, number, number, number])[] = [[0, 0, width, height]],
  trailer = true,
): Uint8Array<ArrayBuffer> {
  const u16 = (value: number) => [value & 0xff, value >> 8];
  const bytes = [0x47, 0x49, 0x46, 0x38, 0x39, 0x61, ...u16(width), ...u16(height), 0, 0, 0];
  // A graphic control extension, as an animation has.
  bytes.push(0x21, 0xf9, 4, 0, 0, 0, 0, 0);
  for (const [left, top, w, h] of frames) {
    bytes.push(0x2c, ...u16(left), ...u16(top), ...u16(w), ...u16(h), 0, 2, 2, 0x4c, 0x01, 0);
  }
  if (trailer) {
    bytes.push(0x3b);
  }
  return new Uint8Array(bytes);
}

/** A picture in a run, naming its image by a relationship id. */
export function picture(attribute: 'r:embed' | 'r:link', id: string): string {
  return `<w:r><w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"><a:graphic><a:graphicData><pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:blipFill><a:blip ${attribute}="${id}"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r>`;
}

/** A seeded generator of numbers in [0, 1), so that a failure is repeated. */
export function seeded(seed: number): () => number {
  let state = seed;
  return () => {
    state = (state * 1_103_515_245 + 12_345) % 2 ** 31;
    return state / 2 ** 31;
  };
}
