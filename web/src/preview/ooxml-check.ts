// The page's own check of what the preview's worker answers (ADR-0028):
// the worker bounds its model, and the page does not take that on trust. A
// model of another shape, or larger than the worker may make, is refused
// before anything of it is drawn.

import {
  DOCX_MAX_BLOCKS,
  DOCX_MAX_CHARACTERS,
  DOCX_MAX_IMAGES,
  DOCX_MAX_NODES,
  DOCX_MAX_TABLE_DEPTH,
  OOXML_BOUNDS,
  XLSX_MAX_CELL_CHARACTERS,
  XLSX_MAX_CHARACTERS,
  XLSX_MAX_COLUMNS,
  XLSX_MAX_ROWS,
  XLSX_MAX_SHEET_NAME,
  XLSX_MAX_SHEETS,
  type DocxModel,
  type OoxmlModel,
  type XlsxModel,
} from './ooxml-model';

/** A plain object's fields, or null for anything else. */
function fieldsOf(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
}

/** Whether a value is a Uint8Array, of whichever realm. */
function isBytes(value: unknown): boolean {
  return Object.prototype.toString.call(value) === '[object Uint8Array]';
}

/** Whether a value is a whole number from `least` to `most`. */
function wholeIn(value: unknown, least: number, most: number): boolean {
  return typeof value === 'number' && Number.isInteger(value) && value >= least && value <= most;
}

/** Whether a paragraph's place in a list is none, or a level 0 to 8 of a list numbered or not. */
function isListPlace(value: unknown): boolean {
  if (value === null) {
    return true;
  }
  const fields = fieldsOf(value);
  return fields !== null && wholeIn(fields.level, 0, 8) && typeof fields.ordered === 'boolean';
}

/**
 * Whether a DOCX model is of the shape the worker makes, with at most
 * DOCX_MAX_NODES nodes, DOCX_MAX_BLOCKS blocks, DOCX_MAX_CHARACTERS
 * characters, tables nested DOCX_MAX_TABLE_DEPTH deep, and DOCX_MAX_IMAGES
 * images of OOXML_BOUNDS.totalBytes in all, every image reference naming
 * one of them; headings 0 to 6, spans 1 to 1 000, every flag a boolean.
 * Walked without recursion, and given up at the first node past a bound.
 */
function checkDocx(model: Record<string, unknown>): boolean {
  const { blocks, images, truncated, deepTables } = model;
  if (!Array.isArray(blocks) || !Array.isArray(images) || typeof truncated !== 'boolean' || typeof deepTables !== 'boolean') {
    return false;
  }
  if (images.length > DOCX_MAX_IMAGES) {
    return false;
  }
  let bytes = 0;
  for (const image of images) {
    const fields = fieldsOf(image);
    if (fields === null || !['png', 'jpeg', 'gif'].includes(fields.kind as string) || !isBytes(fields.bytes)) {
      return false;
    }
    bytes += (fields.bytes as Uint8Array).byteLength;
  }
  if (bytes > OOXML_BOUNDS.totalBytes) {
    return false;
  }
  let nodes = 0;
  let count = 0;
  let characters = 0;
  // Lists of blocks still to be walked, with how many tables they are in.
  const pending: [unknown[], number][] = [[blocks, 0]];
  for (let next = pending.pop(); next !== undefined; next = pending.pop()) {
    const [list, depth] = next;
    for (const block of list) {
      nodes += 1;
      count += 1;
      if (nodes > DOCX_MAX_NODES || count > DOCX_MAX_BLOCKS) {
        return false;
      }
      const fields = fieldsOf(block);
      if (fields?.type === 'paragraph') {
        if (!wholeIn(fields.heading, 0, 6) || !isListPlace(fields.list) || !Array.isArray(fields.inlines)) {
          return false;
        }
        for (const inline of fields.inlines) {
          nodes += 1;
          const item = fieldsOf(inline);
          if (nodes > DOCX_MAX_NODES || item === null) {
            return false;
          }
          if (item.type === 'text') {
            if (
              typeof item.text !== 'string' ||
              typeof item.bold !== 'boolean' ||
              typeof item.italic !== 'boolean' ||
              typeof item.underline !== 'boolean'
            ) {
              return false;
            }
            characters += item.text.length;
          } else if (item.type === 'image') {
            if (typeof item.image !== 'number' || !Number.isInteger(item.image) || item.image < 0 || item.image >= images.length) {
              return false;
            }
          } else if (item.type !== 'break' && item.type !== 'image-omitted') {
            return false;
          }
        }
      } else if (fields?.type === 'table' && Array.isArray(fields.rows) && depth < DOCX_MAX_TABLE_DEPTH) {
        for (const row of fields.rows) {
          nodes += 1;
          if (nodes > DOCX_MAX_NODES || !Array.isArray(row)) {
            return false;
          }
          for (const cell of row) {
            nodes += 1;
            const each = fieldsOf(cell);
            if (nodes > DOCX_MAX_NODES || each === null || !wholeIn(each.span, 1, 1000) || !Array.isArray(each.blocks)) {
              return false;
            }
            pending.push([each.blocks, depth + 1]);
          }
        }
      } else {
        return false;
      }
    }
  }
  return characters <= DOCX_MAX_CHARACTERS;
}

/**
 * Whether an XLSX model is of the shape the worker makes: at most
 * XLSX_MAX_SHEETS sheets of XLSX_MAX_ROWS rows by XLSX_MAX_COLUMNS cells,
 * every cell text of at most XLSX_MAX_CELL_CHARACTERS, XLSX_MAX_CHARACTERS
 * characters in all, every flag a boolean.
 */
function checkXlsx(model: Record<string, unknown>): boolean {
  const { sheets, moreSheets, truncated } = model;
  if (!Array.isArray(sheets) || sheets.length > XLSX_MAX_SHEETS || typeof moreSheets !== 'boolean' || typeof truncated !== 'boolean') {
    return false;
  }
  let characters = 0;
  for (const sheet of sheets) {
    const fields = fieldsOf(sheet);
    if (
      fields === null ||
      typeof fields.name !== 'string' ||
      typeof fields.hidden !== 'boolean' ||
      typeof fields.moreRows !== 'boolean' ||
      typeof fields.moreColumns !== 'boolean' ||
      fields.name.length > XLSX_MAX_SHEET_NAME ||
      !['cells', 'chart', 'other'].includes(fields.content as string) ||
      !Array.isArray(fields.rows) ||
      fields.rows.length > XLSX_MAX_ROWS
    ) {
      return false;
    }
    for (const row of fields.rows) {
      if (!Array.isArray(row) || row.length > XLSX_MAX_COLUMNS) {
        return false;
      }
      for (const cell of row) {
        if (typeof cell !== 'string' || cell.length > XLSX_MAX_CELL_CHARACTERS) {
          return false;
        }
        characters += cell.length;
      }
      if (characters > XLSX_MAX_CHARACTERS) {
        return false;
      }
    }
  }
  return true;
}

/** The model of a worker's answer when it is one the page draws; null otherwise. */
export function checkedModel(value: unknown): OoxmlModel | null {
  const model = fieldsOf(value);
  if (model?.kind === 'docx') {
    return checkDocx(model) ? (model as unknown as DocxModel) : null;
  }
  if (model?.kind === 'xlsx') {
    return checkXlsx(model) ? (model as unknown as XlsxModel) : null;
  }
  return null;
}
