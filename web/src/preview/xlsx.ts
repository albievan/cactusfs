// An XLSX read into a plain model (ADR-0028), in the preview's worker: at
// most XLSX_MAX_SHEETS sheets of XLSX_MAX_ROWS rows by XLSX_MAX_COLUMNS
// columns, every cell as text - shared strings, inline strings, and for a
// formula the value Excel saved with the file. No formula is ever
// evaluated, and its text is not read. A number whose format is a date or a
// time is shown as one; any other number as it was saved.

import { OoxmlError } from './ooxml-error';
import {
  XLSX_MAX_CELL_CHARACTERS,
  XLSX_MAX_CHARACTERS,
  XLSX_MAX_COLUMNS,
  XLSX_MAX_ROWS,
  XLSX_MAX_SHEET_NAME,
  XLSX_MAX_SHEETS,
  type XlsxModel,
  type XlsxSheet,
  type XlsxSheetContent,
} from './ooxml-model';
import { attributeIn, mainPartOf, readPartTree, RELATIONSHIP_ID_NAMESPACES, relationshipsOf } from './opc';
import { declaresDtd, readXml, STOP, type XmlAttributes, type XmlElement } from './xml';
import type { ZipArchive } from './zip';

/** SpreadsheetML, transitional and strict. */
const SHEET = ['http://schemas.openxmlformats.org/spreadsheetml/2006/main', 'http://purl.oclc.org/ooxml/spreadsheetml/main'];

/** The local name of an element of SpreadsheetML, or null for one of another namespace. */
function sheetName(name: string): string | null {
  for (const namespace of SHEET) {
    if (name.startsWith(`{${namespace}}`)) {
      return name.slice(namespace.length + 2);
    }
  }
  return null;
}

/** The elements of SpreadsheetML directly in `element` with this local name. */
function sheetChildren(element: XmlElement, local: string): XmlElement[] {
  return element.children.filter(
    (child): child is XmlElement => typeof child !== 'string' && sheetName(child.name) === local,
  );
}

/** The longest format code that is read. */
export const MAX_FORMAT_CODE = 255;

/** What a number format shows: a date, a time, both, or none of them. */
export type DateFormat = 'date' | 'time' | 'datetime' | null;

/** The built-in number formats that are dates or times (ECMA-376, 18.8.30). */
const BUILT_IN_DATES: ReadonlyMap<number, DateFormat> = new Map<number, DateFormat>([
  [14, 'date'],
  [15, 'date'],
  [16, 'date'],
  [17, 'date'],
  [18, 'time'],
  [19, 'time'],
  [20, 'time'],
  [21, 'time'],
  [22, 'datetime'],
  [45, 'time'],
  [46, 'time'],
  [47, 'time'],
]);

/**
 * Whether a number format shows a date or a time: by its built-in id, or,
 * for a format of the workbook's own, by the codes of its pattern outside
 * quoted text, escapes and brackets (`d`, `y` for a date; `h`, `s` for a
 * time; `m` is either, and decides nothing).
 */
export function dateFormatOf(id: number, code: string | undefined): DateFormat {
  const builtIn = BUILT_IN_DATES.get(id);
  if (builtIn !== undefined) {
    return builtIn;
  }
  // Excel's own formats are at most 255 characters long: a longer one is
  // none of them, and is not given to the patterns below.
  if (code === undefined || code.length > MAX_FORMAT_CODE) {
    return null;
  }
  // The first section only: that of positive numbers.
  const pattern = (code.split(';')[0] ?? '')
    .replace(/"[^"]*"/g, '')
    .replace(/\\./g, '')
    .replace(/[_*]./g, '')
    .replace(/\[(h+|m+|s+)\]/gi, '$1')
    .replace(/\[[^\]]*\]/g, '');
  const date = /[dy]/i.test(pattern);
  const time = /[hs]/i.test(pattern);
  return date && time ? 'datetime' : date ? 'date' : time ? 'time' : null;
}

/** Two digits. */
function two(value: number): string {
  return String(value).padStart(2, '0');
}

/** The last serial a date may have: 9999-12-31. */
const LAST_SERIAL = 2_958_465;

/**
 * A serial date of a workbook as text, in ISO 8601 form: `2026-10-15`,
 * `13:45:00` or `2026-10-15 13:45:00`. In the 1900 system serial 1 is
 * 1900-01-01 and, as in Excel, 60 is the 29th of February 1900 that never
 * was; in the 1904 system serial 0 is 1904-01-01. Null for a serial that is
 * no date (negative, past 9999, not a number).
 */
export function formatSerial(serial: number, format: Exclude<DateFormat, null>, date1904: boolean): string | null {
  if (!Number.isFinite(serial) || serial < 0 || serial > LAST_SERIAL) {
    return null;
  }
  // To the nearest second.
  const seconds = Math.round(serial * 86_400);
  const days = Math.floor(seconds / 86_400);
  const time = seconds - days * 86_400;
  const clock = `${two(Math.floor(time / 3600))}:${two(Math.floor((time % 3600) / 60))}:${two(time % 60)}`;
  if (format === 'time') {
    return clock;
  }
  let day: string;
  if (!date1904 && days === 60) {
    day = '1900-02-29';
  } else {
    const epoch = date1904 ? Date.UTC(1904, 0, 1) : days < 60 ? Date.UTC(1899, 11, 31) : Date.UTC(1899, 11, 30);
    const date = new Date(epoch + days * 86_400_000);
    day = `${String(date.getUTCFullYear()).padStart(4, '0')}-${two(date.getUTCMonth() + 1)}-${two(date.getUTCDate())}`;
  }
  return format === 'date' ? day : `${day} ${clock}`;
}

/** The column of a cell reference (`A1` → 1, `XFD7` → 16 384), or null. */
export function columnOf(reference: string): number | null {
  const letters = /^([A-Z]{1,3})\d*$/i.exec(reference.trim())?.[1];
  if (letters === undefined) {
    return null;
  }
  let column = 0;
  for (const letter of letters.toUpperCase()) {
    column = column * 26 + (letter.charCodeAt(0) - 64);
  }
  return column <= 16_384 ? column : null;
}

/** The text of a part read element by element, its DTD judged first. */
function readPart(zip: ZipArchive, part: string, handler: Parameters<typeof readXml>[1]): boolean {
  const text = zip.readText(part);
  if (text === null) {
    return false;
  }
  if (declaresDtd(text)) {
    throw new OoxmlError('unsafe', `a DOCTYPE or an ENTITY in ${part}`);
  }
  readXml(text, handler);
  return true;
}

/** The shared strings of a workbook, in order: each the text of its runs, phonetic guides left out. */
export function readSharedStrings(zip: ZipArchive, part: string | null): string[] {
  const strings: string[] = [];
  if (part === null) {
    return strings;
  }
  let current: string[] | null = null;
  let phonetic = 0;
  let inText = false;
  readPart(zip, part, {
    open(name) {
      const local = sheetName(name);
      if (local === 'si') {
        current = [];
      } else if (local === 'rPh') {
        phonetic += 1;
      } else if (local === 't') {
        inText = current !== null && phonetic === 0;
      }
    },
    close(name) {
      const local = sheetName(name);
      if (local === 'si' && current !== null) {
        strings.push(current.join(''));
        current = null;
      } else if (local === 'rPh') {
        phonetic -= 1;
      } else if (local === 't') {
        inText = false;
      }
    },
    text(text) {
      if (inText) {
        (current as string[] | null)?.push(text);
      }
    },
  });
  return strings;
}

/** The date format of each cell format (`s` of a cell) of a workbook's styles. */
export function readCellFormats(tree: XmlElement | null): DateFormat[] {
  if (tree === null) {
    return [];
  }
  const codes = new Map<number, string>();
  for (const numFmts of sheetChildren(tree, 'numFmts')) {
    for (const numFmt of sheetChildren(numFmts, 'numFmt')) {
      const id = Number(numFmt.attributes.get('numFmtId'));
      const code = numFmt.attributes.get('formatCode');
      if (Number.isInteger(id) && code !== undefined) {
        codes.set(id, code);
      }
    }
  }
  const formats: DateFormat[] = [];
  for (const cellXfs of sheetChildren(tree, 'cellXfs')) {
    for (const xf of sheetChildren(cellXfs, 'xf')) {
      const id = Number(xf.attributes.get('numFmtId') ?? '0');
      formats.push(Number.isInteger(id) ? dateFormatOf(id, codes.get(id)) : null);
    }
  }
  return formats;
}

/** What a workbook gives every sheet to show its cells. */
interface Workbook {
  strings: string[];
  formats: DateFormat[];
  date1904: boolean;
  /** The characters every sheet shows, counted together against XLSX_MAX_CHARACTERS. */
  budget: { characters: number; truncated: boolean };
}

/** A cell being read. */
interface OpenCell {
  column: number;
  type: string;
  format: DateFormat;
  value: string[];
  inline: string[];
}

/** The text a cell shows, from what was saved of it. */
export function cellText(
  type: string,
  value: string,
  inline: string,
  format: DateFormat,
  workbook: Pick<Workbook, 'strings' | 'date1904'>,
): string {
  switch (type) {
    case 's': {
      const index = /^\d{1,9}$/.test(value.trim()) ? Number(value.trim()) : -1;
      return workbook.strings[index] ?? '';
    }
    case 'inlineStr':
      return inline;
    case 'b':
      return value.trim() === '1' ? 'TRUE' : value.trim() === '0' ? 'FALSE' : value;
    case 'str':
    case 'e':
    case 'd':
      return value;
    default: {
      if (format !== null && value.trim() !== '') {
        const date = formatSerial(Number(value), format, workbook.date1904);
        if (date !== null) {
          return date;
        }
      }
      return value;
    }
  }
}

/** Reads the cells of a worksheet: its first XLSX_MAX_ROWS rows, XLSX_MAX_COLUMNS columns of each. */
export function readWorksheet(zip: ZipArchive, part: string, workbook: Workbook): Pick<XlsxSheet, 'rows' | 'moreRows' | 'moreColumns'> {
  const rows: string[][] = [];
  let moreRows = false;
  let moreColumns = false;
  let nextRow = 1;
  let row: string[] | null = null;
  let nextColumn = 1;
  let cell: OpenCell | null = null;
  let into: 'value' | 'inline' | null = null;
  let phonetic = 0;
  let formula = 0;
  const { budget } = workbook;
  if (budget.characters >= XLSX_MAX_CHARACTERS) {
    // Earlier sheets showed all there is room for: this one is not read.
    budget.truncated = true;
    return { rows, moreRows, moreColumns };
  }
  const found = readPart(zip, part, {
    open(name, attributes: XmlAttributes) {
      const local = sheetName(name);
      if (formula > 0 || local === 'f') {
        formula += 1;
        return;
      }
      switch (local) {
        case 'row': {
          const r = Number(attributes.get('r') ?? nextRow);
          const index = Number.isInteger(r) && r >= nextRow ? r : nextRow;
          if (index > XLSX_MAX_ROWS) {
            moreRows = true;
            return STOP;
          }
          nextRow = index + 1;
          nextColumn = 1;
          while (rows.length < index) {
            rows.push([]);
          }
          row = rows[index - 1] ?? null;
          return;
        }
        case 'c': {
          const column = columnOf(attributes.get('r') ?? '') ?? nextColumn;
          nextColumn = column + 1;
          const style = Number(attributes.get('s') ?? '0');
          cell = {
            column,
            type: attributes.get('t') ?? 'n',
            format: (Number.isInteger(style) ? workbook.formats[style] : null) ?? null,
            value: [],
            inline: [],
          };
          return;
        }
        case 'v':
          into = cell === null ? null : 'value';
          return;
        case 'rPh':
          phonetic += 1;
          return;
        case 't':
          into = cell !== null && phonetic === 0 ? 'inline' : null;
          return;
        default:
          return;
      }
    },
    close(name) {
      const local = sheetName(name);
      if (formula > 0) {
        formula -= 1;
        return;
      }
      switch (local) {
        case 'c': {
          const open = cell as OpenCell | null;
          cell = null;
          const current = row as string[] | null;
          if (open === null || current === null) {
            return;
          }
          if (open.column > XLSX_MAX_COLUMNS) {
            moreColumns = true;
            return;
          }
          let text = cellText(open.type, open.value.join(''), open.inline.join(''), open.format, workbook);
          if (text === '') {
            return;
          }
          // Counted as the cell's text is made: a shared string as often as it is shown.
          const room = Math.min(XLSX_MAX_CELL_CHARACTERS, XLSX_MAX_CHARACTERS - budget.characters);
          if (room <= 0) {
            // A cell with text, and no room left for it: the rest is not read.
            budget.truncated = true;
            return STOP;
          }
          if (text.length > room) {
            text = text.slice(0, room);
            budget.truncated = true;
          }
          budget.characters += text.length;
          while (current.length < open.column - 1) {
            current.push('');
          }
          current[open.column - 1] = text;
          return undefined;
        }
        case 'row':
          row = null;
          return;
        case 'v':
        case 't':
          into = null;
          return;
        case 'rPh':
          phonetic -= 1;
          return;
        case 'sheetData':
          return STOP;
        default:
          return;
      }
    },
    text(text) {
      const open = cell as OpenCell | null;
      if (open === null || into === null) {
        return;
      }
      (into === 'value' ? open.value : open.inline).push(text);
    },
  });
  if (!found) {
    throw new OoxmlError('damaged', 'a worksheet missing');
  }
  // Rows past the last that has a cell are not shown.
  while (rows.length > 0 && rows.at(-1)?.length === 0) {
    rows.pop();
  }
  return { rows, moreRows, moreColumns };
}

/** Reads a workbook into its model. */
export function readXlsx(zip: ZipArchive): XlsxModel {
  const main = mainPartOf(zip);
  const tree = readPartTree(zip, main);
  if (tree === null || sheetName(tree.name) !== 'workbook') {
    throw new OoxmlError('damaged', 'no workbook');
  }
  const relationships = relationshipsOf(zip, main);
  const partOf = (type: string): string | null => {
    for (const relationship of relationships.values()) {
      if (relationship.type.endsWith(`/${type}`) && relationship.target !== null) {
        return relationship.target;
      }
    }
    return null;
  };
  const properties = sheetChildren(tree, 'workbookPr')[0];
  const date1904 = ['1', 'true'].includes(properties?.attributes.get('date1904') ?? '');
  const stylesPart = partOf('styles');
  const workbook: Workbook = {
    strings: readSharedStrings(zip, partOf('sharedStrings')),
    formats: readCellFormats(stylesPart === null ? null : readPartTree(zip, stylesPart)),
    date1904,
    budget: { characters: 0, truncated: false },
  };
  const listed = sheetChildren(tree, 'sheets').flatMap((sheets) => sheetChildren(sheets, 'sheet'));
  const sheets: XlsxSheet[] = [];
  for (const sheet of listed.slice(0, XLSX_MAX_SHEETS)) {
    const name = (sheet.attributes.get('name') ?? '').slice(0, XLSX_MAX_SHEET_NAME);
    const state = sheet.attributes.get('state') ?? 'visible';
    const id = attributeIn(sheet, RELATIONSHIP_ID_NAMESPACES, 'id');
    const relationship = id === undefined ? undefined : relationships.get(id);
    let content: XlsxSheetContent = 'other';
    if (relationship?.type.endsWith('/worksheet') === true && relationship.target !== null) {
      content = 'cells';
    } else if (relationship?.type.endsWith('/chartsheet') === true) {
      content = 'chart';
    }
    const cells =
      content === 'cells' && relationship?.target != null
        ? readWorksheet(zip, relationship.target, workbook)
        : { rows: [], moreRows: false, moreColumns: false };
    sheets.push({ name, hidden: state !== 'visible', content, ...cells });
  }
  return { kind: 'xlsx', sheets, moreSheets: listed.length > XLSX_MAX_SHEETS, truncated: workbook.budget.truncated };
}
