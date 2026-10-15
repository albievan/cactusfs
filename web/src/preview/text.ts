/** How many rows of a CSV object are shown. */
export const CSV_ROWS = 1000;

/** How many columns of a CSV row are shown. */
export const CSV_COLUMNS = 100;

/**
 * The bytes of a text preview as text. A sequence that is not UTF-8 becomes
 * U+FFFD and nothing fails; a byte order mark is dropped. When the API sent
 * only the first part of the object, a character its last bytes began and
 * did not finish is left out rather than shown as U+FFFD.
 */
export function decodeText(bytes: ArrayBuffer, truncated: boolean): string {
  return new TextDecoder('utf-8', { fatal: false }).decode(bytes, { stream: truncated });
}

/** The rows of a CSV object, as many as are shown. */
export interface CsvTable {
  /** The rows, each the text of its cells; at most CSV_ROWS of them and CSV_COLUMNS cells a row. */
  rows: string[][];
  /** Whether rows were left out: past CSV_ROWS, or a last row the text was cut off in. */
  moreRows: boolean;
  /** Whether a row had more cells than are shown. */
  moreColumns: boolean;
}

/**
 * Reads CSV as RFC 4180 has it: cells parted by commas, rows by CRLF or LF,
 * a cell in double quotes may hold commas, line breaks and doubled quotes. It
 * is read leniently - a quote inside an unquoted cell is a character of it -
 * and never fails: every cell is text, and is shown as text.
 *
 * When `truncated`, the text is the first part of the object: its last row,
 * unless it ended with a line break outside quotes, may have been cut off and
 * is left out.
 */
export function parseCsv(text: string, truncated: boolean): CsvTable {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = '';
  let quoted = false;
  // Whether the row being read has begun: a line that is empty at the end of
  // the text is no row.
  let begun = false;
  let moreColumns = false;
  let i = 0;

  const endCell = () => {
    if (row.length < CSV_COLUMNS) {
      row.push(cell);
    } else {
      moreColumns = true;
    }
    cell = '';
  };
  // Returns false once enough rows are read.
  const endRow = (): boolean => {
    endCell();
    rows.push(row);
    row = [];
    begun = false;
    return rows.length <= CSV_ROWS;
  };

  while (i < text.length) {
    const c = text[i];
    if (quoted) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          cell += '"';
          i += 2;
          continue;
        }
        quoted = false;
      } else {
        cell += c;
      }
      i += 1;
      continue;
    }
    if (c === '"' && cell === '') {
      quoted = true;
      begun = true;
    } else if (c === ',') {
      endCell();
      begun = true;
    } else if (c === '\n' || (c === '\r' && text[i + 1] === '\n')) {
      i += c === '\r' ? 2 : 1;
      if (!endRow()) {
        break;
      }
      continue;
    } else {
      cell += c;
      begun = true;
    }
    i += 1;
  }

  // A last row without a line break after it: whole if the object ended
  // there, perhaps cut off if the API sent only its first part.
  let cut = false;
  if (rows.length <= CSV_ROWS && begun) {
    if (truncated) {
      cut = true;
    } else {
      endRow();
    }
  }
  const moreRows = cut || rows.length > CSV_ROWS;
  rows.length = Math.min(rows.length, CSV_ROWS);
  return { rows, moreRows, moreColumns };
}
