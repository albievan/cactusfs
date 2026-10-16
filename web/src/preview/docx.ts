// A DOCX read into a plain model (ADR-0028), in the preview's worker:
// paragraphs, headings by their style, lists, tables, bold, italic and
// underline, line breaks, and the PNG, JPEG and GIF images the document
// holds. Page layout, styles, headers and footers, footnotes, comments,
// text boxes, charts, fields' codes and deleted text are not read.

import { OoxmlError } from './ooxml-error';
import {
  DOCX_MAX_BLOCKS,
  DOCX_MAX_CHARACTERS,
  DOCX_MAX_IMAGE_PIXELS,
  DOCX_MAX_IMAGES,
  DOCX_MAX_NODES,
  DOCX_MAX_TABLE_DEPTH,
  type DocxBlock,
  type DocxCell,
  type DocxImage,
  type DocxImageKind,
  type DocxInline,
  type DocxModel,
  type DocxParagraph,
  type DocxTable,
} from './ooxml-model';
import { attributeIn, mainPartOf, readPartTree, RELATIONSHIP_ID_NAMESPACES, relationshipsOf, type Relationship } from './opc';
import { childNamed, childrenNamed, declaresDtd, readXml, STOP, type XmlAnswer, type XmlAttributes, type XmlElement } from './xml';
import type { ZipArchive } from './zip';

/** WordprocessingML, transitional and strict. */
const WORD = ['http://schemas.openxmlformats.org/wordprocessingml/2006/main', 'http://purl.oclc.org/ooxml/wordprocessingml/main'];

/** DrawingML, transitional and strict: where a picture names its image. */
const DRAWING = ['http://schemas.openxmlformats.org/drawingml/2006/main', 'http://purl.oclc.org/ooxml/drawingml/main'];

/** Markup compatibility: its fallbacks repeat what the choice before them shows. */
const COMPATIBILITY = 'http://schemas.openxmlformats.org/markup-compatibility/2006';

/** The local name of an element of WordprocessingML, or null for one of another namespace. */
function wordName(name: string): string | null {
  for (const namespace of WORD) {
    if (name.startsWith(`{${namespace}}`)) {
      return name.slice(namespace.length + 2);
    }
  }
  return null;
}

/** The `w:val` of an element. */
function valOf(attributes: XmlAttributes): string | undefined {
  return attributeIn({ attributes }, WORD, 'val');
}

/** A whole number of an attribute, or null. */
function wholeNumber(value: string | undefined): number | null {
  if (value === undefined || !/^-?\d{1,9}$/.test(value.trim())) {
    return null;
  }
  return Number(value.trim());
}

/** Whether an on/off property (`w:b`, `w:i`) is on: present, and not said to be off. */
function onOff(attributes: XmlAttributes): boolean {
  const value = valOf(attributes);
  return value === undefined || !['0', 'false', 'off'].includes(value.toLowerCase());
}

/** What a paragraph style says of headings and lists. */
interface Style {
  basedOn: string | null;
  heading: number | null;
  numId: string | null;
  level: number | null;
}

/** The heading level a style's name gives - `heading 1` to `heading 9`, `Title` - or null. */
export function headingOfStyleName(name: string): number | null {
  const heading = /^heading\s*([1-9])$/i.exec(name.trim());
  if (heading?.[1] !== undefined) {
    return Number(heading[1]);
  }
  return /^title$/i.test(name.trim()) ? 1 : null;
}

/** The heading level of an outline level (0 to 8), or null: 9 and beyond are body text. */
function headingOfOutline(level: number | null): number | null {
  return level !== null && level >= 0 && level <= 8 ? level + 1 : null;
}

/** The numbering properties (`w:numPr`) in a paragraph's or a style's properties. */
function numberingOf(properties: XmlElement | null): { numId: string | null; level: number | null } {
  const numPr = properties === null ? null : wordChild(properties, 'numPr');
  if (numPr === null) {
    return { numId: null, level: null };
  }
  const numId = wordChild(numPr, 'numId');
  const level = wordChild(numPr, 'ilvl');
  return {
    numId: numId === null ? null : (valOf(numId.attributes) ?? null),
    level: level === null ? null : wholeNumber(valOf(level.attributes)),
  };
}

/** The first child of WordprocessingML with this local name. */
function wordChild(element: XmlElement, local: string): XmlElement | null {
  for (const namespace of WORD) {
    const child = childNamed(element, `{${namespace}}${local}`);
    if (child !== null) {
      return child;
    }
  }
  return null;
}

/** The children of WordprocessingML with this local name. */
function wordChildren(element: XmlElement, local: string): XmlElement[] {
  return WORD.flatMap((namespace) => childrenNamed(element, `{${namespace}}${local}`));
}

/** The paragraph styles of `styles.xml`, by id. */
export function readStyles(tree: XmlElement | null): Map<string, Style> {
  const styles = new Map<string, Style>();
  if (tree === null) {
    return styles;
  }
  for (const style of wordChildren(tree, 'style')) {
    const id = attributeIn(style, WORD, 'styleId');
    if (id === undefined || (attributeIn(style, WORD, 'type') ?? 'paragraph') !== 'paragraph') {
      continue;
    }
    const name = wordChild(style, 'name');
    const basedOn = wordChild(style, 'basedOn');
    const properties = wordChild(style, 'pPr');
    const outline = properties === null ? null : wordChild(properties, 'outlineLvl');
    const numbering = numberingOf(properties);
    styles.set(id, {
      basedOn: basedOn === null ? null : (valOf(basedOn.attributes) ?? null),
      heading:
        (name === null ? null : headingOfStyleName(valOf(name.attributes) ?? '')) ??
        headingOfOutline(outline === null ? null : wholeNumber(valOf(outline.attributes))),
      numId: numbering.numId,
      level: numbering.level,
    });
  }
  return styles;
}

/** Whether a list of `numbering.xml` is numbered at a level: its numId → its level's format, not a bullet. */
export function readNumbering(tree: XmlElement | null): (numId: string, level: number) => boolean {
  const abstracts = new Map<string, Map<number, string>>();
  const nums = new Map<string, string>();
  if (tree !== null) {
    for (const abstract of wordChildren(tree, 'abstractNum')) {
      const id = attributeIn(abstract, WORD, 'abstractNumId');
      if (id === undefined) {
        continue;
      }
      const levels = new Map<number, string>();
      for (const level of wordChildren(abstract, 'lvl')) {
        const index = wholeNumber(attributeIn(level, WORD, 'ilvl'));
        const format = wordChild(level, 'numFmt');
        if (index !== null && format !== null) {
          levels.set(index, valOf(format.attributes) ?? 'decimal');
        }
      }
      abstracts.set(id, levels);
    }
    for (const num of wordChildren(tree, 'num')) {
      const id = attributeIn(num, WORD, 'numId');
      const abstract = wordChild(num, 'abstractNumId');
      const abstractId = abstract === null ? undefined : valOf(abstract.attributes);
      if (id !== undefined && abstractId !== undefined) {
        nums.set(id, abstractId);
      }
    }
  }
  return (numId, level) => {
    const abstract = nums.get(numId);
    const format = abstract === undefined ? undefined : abstracts.get(abstract)?.get(level);
    return format !== undefined && format !== 'bullet' && format !== 'none';
  };
}

/** The signature of each kind of image a document may show. */
const IMAGE_SIGNATURES: readonly [DocxImageKind, readonly number[]][] = [
  ['png', [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]],
  ['jpeg', [0xff, 0xd8, 0xff]],
  ['gif', [0x47, 0x49, 0x46, 0x38]],
];

/** The most pixels a side of an image of a document may have, as for an image previewed (O.3). */
export const IMAGE_MAX_SIDE = 16_384;

/** The most pixels an image of a document may have, as for an image previewed (O.3). */
export const IMAGE_MAX_PIXELS = 40_000_000;

/** The kind of image `bytes` are by their signature - never by their name or stated type - or null. */
export function imageKindOf(bytes: Uint8Array): DocxImageKind | null {
  for (const [kind, signature] of IMAGE_SIGNATURES) {
    if (bytes.length >= signature.length && signature.every((byte, at) => bytes[at] === byte)) {
      if (kind === 'gif' && !(bytes[4] === 0x37 || bytes[4] === 0x39)) {
        return null;
      }
      return kind;
    }
  }
  return null;
}

/** The size of an image from its header - PNG's IHDR, GIF's screen, JPEG's frame header - or null. */
export function imageSizeOf(kind: DocxImageKind, bytes: Uint8Array): { width: number; height: number } | null {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  switch (kind) {
    case 'png':
      if (bytes.length < 24 || String.fromCharCode(...bytes.subarray(12, 16)) !== 'IHDR') {
        return null;
      }
      return { width: view.getUint32(16), height: view.getUint32(20) };
    case 'gif':
      return bytes.length < 10 ? null : { width: view.getUint16(6, true), height: view.getUint16(8, true) };
    case 'jpeg': {
      let at = 2;
      while (at + 4 <= bytes.length) {
        if (bytes[at] !== 0xff) {
          return null;
        }
        const marker = bytes[at + 1] ?? 0;
        if (marker === 0xff) {
          at += 1;
          continue;
        }
        if (marker === 0xd8 || marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) {
          at += 2;
          continue;
        }
        if (marker === 0xd9 || marker === 0xda) {
          // The image's data began, or it ended, before a frame header.
          return null;
        }
        const length = view.getUint16(at + 2);
        const frame = marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc;
        if (frame) {
          return at + 9 <= bytes.length ? { width: view.getUint16(at + 7), height: view.getUint16(at + 5) } : null;
        }
        if (length < 2) {
          return null;
        }
        at += 2 + length;
      }
      return null;
    }
  }
}

/** The most frames a GIF of a document may have. */
export const GIF_MAX_FRAMES = 1000;

/** Skips the data sub-blocks of a GIF from `at`; where they end, or -1 when the bytes end first. */
function afterSubBlocks(bytes: Uint8Array, start: number): number {
  let at = start;
  for (;;) {
    const length = bytes[at];
    if (length === undefined) {
      return -1;
    }
    at += 1;
    if (length === 0) {
      return at;
    }
    at += length;
  }
}

/**
 * Whether every frame of a GIF lies inside its screen, to its trailer, with
 * at most GIF_MAX_FRAMES frames: a browser makes a frame as large as it
 * says, whatever the screen. A block that is no GIF block, or bytes that end
 * before the trailer, are refused - as the API judges a GIF it previews.
 */
export function gifFramesFit(bytes: Uint8Array): boolean {
  if (bytes.length < 13) {
    return false;
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const screenWidth = view.getUint16(6, true);
  const screenHeight = view.getUint16(8, true);
  const packed = bytes[10] ?? 0;
  let at = 13 + ((packed & 0x80) !== 0 ? 3 * 2 ** ((packed & 7) + 1) : 0);
  let frames = 0;
  while (at < bytes.length) {
    const block = bytes[at];
    if (block === 0x3b) {
      return true;
    }
    if (block === 0x21) {
      at = afterSubBlocks(bytes, at + 2);
    } else if (block === 0x2c) {
      if (at + 10 > bytes.length) {
        return false;
      }
      frames += 1;
      const left = view.getUint16(at + 1, true);
      const top = view.getUint16(at + 3, true);
      const width = view.getUint16(at + 5, true);
      const height = view.getUint16(at + 7, true);
      if (frames > GIF_MAX_FRAMES || left + width > screenWidth || top + height > screenHeight) {
        return false;
      }
      const local = bytes[at + 9] ?? 0;
      // The descriptor, its colour table, the LZW code size; then the data.
      at = afterSubBlocks(bytes, at + 10 + ((local & 0x80) !== 0 ? 3 * 2 ** ((local & 7) + 1) : 0) + 1);
    } else {
      return false;
    }
    if (at === -1) {
      return false;
    }
  }
  return false;
}

/**
 * Whether an image of a document is shown, and how many pixels it has: of
 * a kind by its signature, no larger than an image previewed may be, and -
 * a GIF - with every frame inside its screen.
 */
export function acceptedImage(bytes: Uint8Array): { kind: DocxImageKind; pixels: number } | null {
  const kind = imageKindOf(bytes);
  const size = kind === null ? null : imageSizeOf(kind, bytes);
  if (kind === null || size === null) {
    return null;
  }
  const { width, height } = size;
  if (width < 1 || height < 1 || width > IMAGE_MAX_SIDE || height > IMAGE_MAX_SIDE || width * height > IMAGE_MAX_PIXELS) {
    return null;
  }
  if (kind === 'gif' && !gifFramesFit(bytes)) {
    return null;
  }
  return { kind, pixels: width * height };
}

/** Elements whose content is never read: deleted or moved-away text, fields' codes, text boxes, fallbacks, section properties, legacy drawings. */
const SKIPPED = new Set(['del', 'moveFrom', 'delText', 'instrText', 'txbxContent', 'sectPr', 'pict', 'object', 'sym']);

/** A table being read. */
interface OpenTable {
  table: DocxTable;
  row: DocxCell[] | null;
  cell: DocxCell | null;
}

/** A paragraph being read, and what its properties say until they are closed. */
interface OpenParagraph {
  paragraph: DocxParagraph;
  style: string | null;
  outline: number | null;
  numId: string | null;
  level: number | null;
}

/**
 * Reads the main part of a DOCX into its model, element by element, with
 * the styles and numbering it names; images are read from the zip as they
 * are met. Stops at DOCX_MAX_BLOCKS blocks or DOCX_MAX_CHARACTERS
 * characters, saying it did.
 */
export function readDocx(zip: ZipArchive): DocxModel {
  const main = mainPartOf(zip);
  const relationships = relationshipsOf(zip, main);
  const partOf = (type: string): string | null => {
    for (const relationship of relationships.values()) {
      if (relationship.type.endsWith(`/${type}`) && relationship.target !== null) {
        return relationship.target;
      }
    }
    return null;
  };
  const stylesPart = partOf('styles');
  const numberingPart = partOf('numbering');
  const styles = readStyles(stylesPart === null ? null : readPartTree(zip, stylesPart));
  const numbered = readNumbering(numberingPart === null ? null : readPartTree(zip, numberingPart));
  const text = zip.readText(main);
  if (text === null) {
    throw new OoxmlError('damaged', 'no main part');
  }
  if (declaresDtd(text)) {
    throw new OoxmlError('unsafe', 'a DOCTYPE or an ENTITY in the document');
  }
  const builder = new DocxBuilder(zip, styles, numbered, relationships);
  readXml(text, builder);
  return builder.model();
}

/** Follows a style and those it is based on (at most 16) for the first answer `pick` gives. */
function inherited<T>(styles: Map<string, Style>, id: string | null, pick: (style: Style) => T | null): T | null {
  let current = id;
  for (let hops = 0; current !== null && hops < 16; hops += 1) {
    const style = styles.get(current);
    if (style === undefined) {
      return null;
    }
    const value = pick(style);
    if (value !== null) {
      return value;
    }
    current = style.basedOn;
  }
  return null;
}

/** The reader of `word/document.xml`: an XmlHandler that builds the model. */
class DocxBuilder {
  readonly #zip: ZipArchive;
  readonly #styles: Map<string, Style>;
  readonly #numbered: (numId: string, level: number) => boolean;
  readonly #relationships: Map<string, Relationship>;
  readonly #names: (string | null)[] = [];
  readonly #blocks: DocxBlock[] = [];
  readonly #containers: DocxBlock[][] = [this.#blocks];
  readonly #tables: OpenTable[] = [];
  readonly #images: DocxImage[] = [];
  readonly #imageByPart = new Map<string, number>();
  #skip = 0;
  #paragraph: OpenParagraph | null = null;
  #run: { bold: boolean; italic: boolean; underline: boolean } | null = null;
  #inText = false;
  #drawing = 0;
  #count = 0;
  #nodes = 0;
  #pixels = 0;
  #characters = 0;
  #truncated = false;
  #deepTables = false;

  constructor(
    zip: ZipArchive,
    styles: Map<string, Style>,
    numbered: (numId: string, level: number) => boolean,
    relationships: Map<string, Relationship>,
  ) {
    this.#zip = zip;
    this.#styles = styles;
    this.#numbered = numbered;
    this.#relationships = relationships;
  }

  model(): DocxModel {
    if (this.#paragraph !== null) {
      this.#settle(this.#paragraph);
    }
    return {
      kind: 'docx',
      blocks: this.#blocks,
      images: this.#images,
      truncated: this.#truncated,
      deepTables: this.#deepTables,
    };
  }

  /** The local name of the element `up` levels above the one being opened (1 = its parent). */
  #above(up: number): string | null {
    return this.#names.at(-up) ?? null;
  }

  /** Counts a node of the model; false, and the reading truncated, when there are as many as are shown. */
  #node(): boolean {
    this.#nodes += 1;
    if (this.#nodes > DOCX_MAX_NODES) {
      this.#truncated = true;
      return false;
    }
    return true;
  }

  /** Counts a block, which is a node too; false when there are as many as are shown. */
  #block(): boolean {
    this.#count += 1;
    if (this.#count > DOCX_MAX_BLOCKS) {
      this.#truncated = true;
      return false;
    }
    return this.#node();
  }

  readonly open = (name: string, attributes: XmlAttributes): XmlAnswer => {
    const local = wordName(name);
    if (this.#skip > 0) {
      this.#skip += 1;
      this.#names.push(local);
      return;
    }
    if ((local !== null && SKIPPED.has(local)) || name === `{${COMPATIBILITY}}Fallback`) {
      this.#skip = 1;
      this.#names.push(local);
      return;
    }
    const answer = local === null ? this.#openOther(name, attributes) : this.#openWord(local, attributes);
    this.#names.push(local);
    return answer;
  };

  #openOther(name: string, attributes: XmlAttributes): XmlAnswer {
    if (this.#drawing > 0 && DRAWING.some((namespace) => name === `{${namespace}}blip`)) {
      return this.#image(
        attributeIn({ attributes }, RELATIONSHIP_ID_NAMESPACES, 'embed'),
        attributeIn({ attributes }, RELATIONSHIP_ID_NAMESPACES, 'link'),
      );
    }
    return undefined;
  }

  #openWord(local: string, attributes: XmlAttributes): XmlAnswer {
    const parent = this.#above(1);
    switch (local) {
      case 'tbl': {
        if (this.#tables.length >= DOCX_MAX_TABLE_DEPTH) {
          this.#deepTables = true;
          this.#skip = 1;
          return;
        }
        if (!this.#block()) {
          return STOP;
        }
        const table: DocxTable = { type: 'table', rows: [] };
        this.#containers.at(-1)?.push(table);
        this.#tables.push({ table, row: null, cell: null });
        return;
      }
      case 'tr': {
        const open = this.#tables.at(-1);
        if (open !== undefined) {
          if (!this.#node()) {
            return STOP;
          }
          open.row = [];
          open.table.rows.push(open.row);
        }
        return;
      }
      case 'tc': {
        const open = this.#tables.at(-1);
        if (open?.cell != null) {
          // A cell in a cell of the same table is no cell: it is not read,
          // so that every cell opened is the one its end closes.
          this.#skip = 1;
          return;
        }
        if (open?.row != null) {
          if (!this.#node()) {
            return STOP;
          }
          open.cell = { span: 1, blocks: [] };
          open.row.push(open.cell);
          this.#containers.push(open.cell.blocks);
        }
        return;
      }
      case 'gridSpan': {
        const cell = this.#tables.at(-1)?.cell;
        const span = wholeNumber(valOf(attributes));
        if (parent === 'tcPr' && cell != null && span !== null) {
          cell.span = Math.min(Math.max(span, 1), 1000);
        }
        return;
      }
      case 'p': {
        if (this.#paragraph !== null) {
          // A paragraph in a paragraph is read as part of it.
          return;
        }
        if (!this.#block()) {
          return STOP;
        }
        const paragraph: DocxParagraph = { type: 'paragraph', heading: 0, list: null, inlines: [] };
        this.#containers.at(-1)?.push(paragraph);
        this.#paragraph = { paragraph, style: null, outline: null, numId: null, level: null };
        return;
      }
      case 'pStyle':
        if (parent === 'pPr' && this.#above(2) === 'p' && this.#paragraph !== null) {
          this.#paragraph.style = valOf(attributes) ?? null;
        }
        return;
      case 'outlineLvl':
        if (parent === 'pPr' && this.#above(2) === 'p' && this.#paragraph !== null) {
          this.#paragraph.outline = wholeNumber(valOf(attributes));
        }
        return;
      case 'numId':
        if (parent === 'numPr' && this.#above(2) === 'pPr' && this.#above(3) === 'p' && this.#paragraph !== null) {
          this.#paragraph.numId = valOf(attributes) ?? null;
        }
        return;
      case 'ilvl':
        if (parent === 'numPr' && this.#above(2) === 'pPr' && this.#above(3) === 'p' && this.#paragraph !== null) {
          this.#paragraph.level = wholeNumber(valOf(attributes));
        }
        return;
      case 'r':
        this.#run = { bold: false, italic: false, underline: false };
        return;
      case 'b':
      case 'i':
      case 'u':
        if (parent === 'rPr' && this.#above(2) === 'r' && this.#run !== null) {
          if (local === 'b') {
            this.#run.bold = onOff(attributes);
          } else if (local === 'i') {
            this.#run.italic = onOff(attributes);
          } else {
            this.#run.underline = (valOf(attributes) ?? 'single') !== 'none';
          }
        }
        return;
      case 't':
        this.#inText = parent === 'r';
        return;
      case 'tab':
        if (parent === 'r') {
          return this.#text('\t');
        }
        return;
      case 'br':
      case 'cr':
        if (parent === 'r' && this.#paragraph !== null) {
          if (!this.#node()) {
            return STOP;
          }
          this.#paragraph.paragraph.inlines.push({ type: 'break' });
        }
        return;
      case 'noBreakHyphen':
        if (parent === 'r') {
          return this.#text('-');
        }
        return;
      case 'drawing':
        this.#drawing += 1;
        return;
      default:
        return;
    }
  }

  readonly close = (): XmlAnswer => {
    const local = this.#names.pop() ?? null;
    if (this.#skip > 0) {
      this.#skip -= 1;
      return;
    }
    switch (local) {
      case 'tbl':
        this.#tables.pop();
        return;
      case 'tr': {
        const open = this.#tables.at(-1);
        if (open !== undefined) {
          open.row = null;
        }
        return;
      }
      case 'tc': {
        const open = this.#tables.at(-1);
        if (open?.cell != null) {
          open.cell = null;
          this.#containers.pop();
        }
        return;
      }
      case 'pPr':
        if (this.#above(1) === 'p' && this.#paragraph !== null) {
          this.#settle(this.#paragraph);
        }
        return;
      case 'p':
        if (this.#paragraph !== null && !this.#names.includes('p')) {
          this.#settle(this.#paragraph);
          this.#paragraph = null;
        }
        return;
      case 'r':
        this.#run = null;
        return;
      case 't':
        this.#inText = false;
        return;
      case 'drawing':
        this.#drawing -= 1;
        return;
      default:
        return;
    }
  };

  readonly text = (text: string): XmlAnswer => {
    if (this.#skip > 0 || !this.#inText) {
      return;
    }
    return this.#text(text);
  };

  /** Adds text to the run being read, with its emphasis. */
  #text(text: string): XmlAnswer {
    const run = this.#run;
    const paragraph = this.#paragraph?.paragraph;
    if (run === null || paragraph === undefined || text === '') {
      return;
    }
    let shown = text;
    if (this.#characters + shown.length > DOCX_MAX_CHARACTERS) {
      shown = shown.slice(0, DOCX_MAX_CHARACTERS - this.#characters);
      this.#truncated = true;
    }
    this.#characters += shown.length;
    const last = paragraph.inlines.at(-1);
    if (
      last?.type === 'text' &&
      last.bold === run.bold &&
      last.italic === run.italic &&
      last.underline === run.underline
    ) {
      last.text += shown;
    } else if (shown !== '') {
      if (!this.#node()) {
        return STOP;
      }
      paragraph.inlines.push({ type: 'text', text: shown, ...run });
    }
    return this.#truncated ? STOP : undefined;
  }

  /** What a paragraph's properties and its style say: heading, or item of a list. */
  #settle(open: OpenParagraph) {
    const styles = this.#styles;
    const heading =
      headingOfOutline(open.outline) ?? inherited(styles, open.style, (style) => style.heading) ?? 0;
    open.paragraph.heading = Math.min(heading, 6);
    const numId = open.numId ?? inherited(styles, open.style, (style) => style.numId);
    if (heading === 0 && numId !== null && numId !== '0') {
      const level = Math.min(Math.max(open.level ?? inherited(styles, open.style, (style) => style.level) ?? 0, 0), 8);
      open.paragraph.list = { level, ordered: this.#numbered(numId, level) };
    } else {
      open.paragraph.list = null;
    }
  }

  /**
   * An image a picture names: shown when it is in the package, of a kind
   * shown, not too large, and within the pixels of all the document's
   * images (DOCX_MAX_IMAGE_PIXELS). Every reference is a node, whether the
   * image is shown or not.
   */
  #image(embed: string | undefined, link: string | undefined): XmlAnswer {
    const paragraph = this.#paragraph?.paragraph;
    if (paragraph === undefined || (embed === undefined && link === undefined)) {
      return undefined;
    }
    if (!this.#node()) {
      return STOP;
    }
    const omitted: DocxInline = { type: 'image-omitted' };
    const part = embed === undefined ? null : (this.#relationships.get(embed)?.target ?? null);
    if (part === null) {
      // Linked, not embedded: never fetched.
      paragraph.inlines.push(omitted);
      return undefined;
    }
    const known = this.#imageByPart.get(part);
    if (known !== undefined) {
      paragraph.inlines.push(known === -1 ? omitted : { type: 'image', image: known });
      return undefined;
    }
    if (this.#images.length >= DOCX_MAX_IMAGES) {
      paragraph.inlines.push(omitted);
      return undefined;
    }
    const bytes = this.#zip.read(part);
    const accepted = bytes === null ? null : acceptedImage(bytes);
    if (bytes === null || accepted === null || this.#pixels + accepted.pixels > DOCX_MAX_IMAGE_PIXELS) {
      this.#imageByPart.set(part, -1);
      paragraph.inlines.push(omitted);
      return undefined;
    }
    this.#pixels += accepted.pixels;
    // A copy of its own, so that it can be handed to the page.
    this.#images.push({ kind: accepted.kind, bytes: bytes.slice() });
    this.#imageByPart.set(part, this.#images.length - 1);
    paragraph.inlines.push({ type: 'image', image: this.#images.length - 1 });
    return undefined;
  }
}
