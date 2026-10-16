// What the preview's worker makes of a DOCX or XLSX (ADR-0028): plain data,
// drawn by the page as React elements and text nodes. Every string here is
// text of the document and is only ever shown as text. Types, bounds and
// the names of columns only: the page imports this module, and none of the
// reader with it.

import type { OoxmlProblem } from './ooxml-error';

/**
 * What the worker unpacks of one document, at most: the bytes it inflates
 * in all and of one part, the ratio of a part's inflated bytes to its
 * compressed ones, and the entries of the zip.
 */
export const OOXML_BOUNDS = {
  totalBytes: 64 * 1024 * 1024,
  partBytes: 32 * 1024 * 1024,
  ratio: 100,
  entries: 10_000,
} as const;

/** How long the worker is given to read a document; then it is ended. */
export const OOXML_TIMEOUT_MS = 10_000;

/** The most sheets of a workbook that are shown. */
export const XLSX_MAX_SHEETS = 20;

/**
 * The most characters of a workbook that are shown, in all its cells: a
 * shared string is counted every time a cell shows it, so a short file of
 * one long string referenced by many cells cannot make a model of
 * billions of characters.
 */
export const XLSX_MAX_CHARACTERS = 1_000_000;

/** The most characters of a cell that are shown: as many as Excel holds in one. */
export const XLSX_MAX_CELL_CHARACTERS = 32_767;

/** The most characters of a sheet's name that are shown: as many as Excel allows. */
export const XLSX_MAX_SHEET_NAME = 31;

/** The most rows of a sheet that are shown. */
export const XLSX_MAX_ROWS = 1000;

/** The most columns of a sheet that are shown. */
export const XLSX_MAX_COLUMNS = 100;

/** The most blocks - paragraphs and tables, those in tables too - of a document that are shown. */
export const DOCX_MAX_BLOCKS = 10_000;

/**
 * The most nodes of a document's model: blocks, rows, cells, inlines -
 * text, line breaks and every reference to an image, shown or not. Bounds
 * what is handed to the page and drawn there, whatever the file's size.
 */
export const DOCX_MAX_NODES = 50_000;

/** The most pixels of all the images of a document together. */
export const DOCX_MAX_IMAGE_PIXELS = 100_000_000;

/** The most characters of a document that are shown. */
export const DOCX_MAX_CHARACTERS = 1_000_000;

/** The most images of a document that are shown. */
export const DOCX_MAX_IMAGES = 100;

/** How deep tables in tables are shown. */
export const DOCX_MAX_TABLE_DEPTH = 8;

/** The letters of a column (1 → `A`, 27 → `AA`, 16 384 → `XFD`). */
export function columnName(column: number): string {
  let name = '';
  for (let rest = column; rest > 0; rest = Math.floor((rest - 1) / 26)) {
    name = String.fromCharCode(65 + ((rest - 1) % 26)) + name;
  }
  return name;
}

/** The kinds of image a document may show: raster, checked by their signature. */
export type DocxImageKind = 'png' | 'jpeg' | 'gif';

/** An image of a document: its bytes, of the kind their signature says. */
export interface DocxImage {
  kind: DocxImageKind;
  bytes: Uint8Array<ArrayBuffer>;
}

/** What is in a paragraph. */
export type DocxInline =
  | { type: 'text'; text: string; bold: boolean; italic: boolean; underline: boolean }
  | { type: 'break' }
  /** An image, by its place in DocxModel.images. */
  | { type: 'image'; image: number }
  /** An image that is not shown: linked, of another kind, too large, or past DOCX_MAX_IMAGES. */
  | { type: 'image-omitted' };

/** A paragraph: a heading of a level 1 to 6, an item of a list, or neither. */
export interface DocxParagraph {
  type: 'paragraph';
  /** 0 for a paragraph that is no heading. */
  heading: number;
  /** Its place in a list - its level from 0 and whether the list is numbered - or null. */
  list: { level: number; ordered: boolean } | null;
  inlines: DocxInline[];
}

/** A cell of a table: how many columns it spans, and what is in it. */
export interface DocxCell {
  span: number;
  blocks: DocxBlock[];
}

/** A table: its rows of cells. */
export interface DocxTable {
  type: 'table';
  rows: DocxCell[][];
}

/** A paragraph or a table. */
export type DocxBlock = DocxParagraph | DocxTable;

/** What is shown of a DOCX. */
export interface DocxModel {
  kind: 'docx';
  blocks: DocxBlock[];
  images: DocxImage[];
  /** Whether it was read only to DOCX_MAX_BLOCKS, DOCX_MAX_NODES or DOCX_MAX_CHARACTERS. */
  truncated: boolean;
  /** Whether tables nested deeper than DOCX_MAX_TABLE_DEPTH were left out. */
  deepTables: boolean;
}

/** What a sheet is, as far as it is shown. */
export type XlsxSheetContent = 'cells' | 'chart' | 'other';

/** A sheet of a workbook. */
export interface XlsxSheet {
  name: string;
  hidden: boolean;
  content: XlsxSheetContent;
  /**
   * Its first XLSX_MAX_ROWS rows from row 1, each its cells from column A
   * as text, to its last cell; an empty row is an empty array.
   */
  rows: string[][];
  /** Whether it has rows past XLSX_MAX_ROWS. */
  moreRows: boolean;
  /** Whether a row has cells past XLSX_MAX_COLUMNS. */
  moreColumns: boolean;
}

/** What is shown of an XLSX. */
export interface XlsxModel {
  kind: 'xlsx';
  sheets: XlsxSheet[];
  /** Whether it has sheets past XLSX_MAX_SHEETS. */
  moreSheets: boolean;
  /**
   * Whether cells were cut short or left out: past XLSX_MAX_CELL_CHARACTERS
   * in a cell, or XLSX_MAX_CHARACTERS in all.
   */
  truncated: boolean;
}

/** What is shown of a DOCX or XLSX. */
export type OoxmlModel = DocxModel | XlsxModel;

/** The kinds the worker reads. */
export type OoxmlKind = OoxmlModel['kind'];

/** What the page asks of the worker: to read these bytes as this kind. */
export interface OoxmlRequest {
  kind: OoxmlKind;
  bytes: ArrayBuffer;
}

/** What the worker answers. */
export type OoxmlReply = { ok: true; model: OoxmlModel } | { ok: false; problem: OoxmlProblem };
