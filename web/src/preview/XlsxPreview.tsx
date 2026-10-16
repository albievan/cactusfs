import { useId, useState } from 'react';

import { countOf } from '../deletions/state';
import {
  columnName,
  XLSX_MAX_CELL_CHARACTERS,
  XLSX_MAX_CHARACTERS,
  XLSX_MAX_COLUMNS,
  XLSX_MAX_ROWS,
  XLSX_MAX_SHEETS,
  type XlsxModel,
  type XlsxSheet,
} from './ooxml-model';

/** The name a sheet is shown by. */
function sheetLabel(sheet: XlsxSheet): string {
  const name = sheet.name === '' ? 'A sheet without a name' : sheet.name;
  return sheet.hidden ? `${name} (hidden)` : name;
}

/** A sheet: its cells as a table of text, with the letters of its columns and the numbers of its rows. */
function Sheet({ sheet, objectKey }: { sheet: XlsxSheet; objectKey: string }) {
  if (sheet.content === 'chart') {
    return <p>This sheet is a chart, which is not shown.</p>;
  }
  if (sheet.content === 'other') {
    return <p>This sheet is not of a kind that is shown.</p>;
  }
  const width = sheet.rows.reduce((most, row) => Math.max(most, row.length), 0);
  const columns = Array.from({ length: width }, (_, at) => columnName(at + 1));
  return (
    <>
      {sheet.moreRows ? (
        <p className="preview-note" role="note">
          Only the first {countOf(XLSX_MAX_ROWS)} rows are shown.
        </p>
      ) : null}
      {sheet.moreColumns ? (
        <p className="preview-note" role="note">
          Only the first {countOf(XLSX_MAX_COLUMNS)} columns are shown.
        </p>
      ) : null}
      {width === 0 ? (
        <p>There is nothing in it.</p>
      ) : (
        <div
          className="preview-table"
          tabIndex={0}
          role="group"
          aria-label={`Sheet ${sheetLabel(sheet)} of ${objectKey}`}
        >
          <table className="table table-sm table-bordered preview-xlsx">
            <thead>
              <tr>
                <td className="preview-xlsx-corner" />
                {columns.map((name) => (
                  <th key={name} scope="col">
                    {name}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {sheet.rows.map((row, r) => (
                // A row's place is its number.
                <tr key={r}>
                  <th scope="row">{String(r + 1)}</th>
                  {columns.map((name, c) => (
                    <td key={name}>{row[c] ?? ''}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/**
 * An XLSX as its reader modelled it: a sheet at a time, chosen by its name,
 * every cell a text node - the values saved with the workbook, no formula
 * calculated. What is not shown is said above it.
 */
export function XlsxPreview({ model, objectKey }: { model: XlsxModel; objectKey: string }) {
  const [shown, setShown] = useState(0);
  const panelId = useId();
  const sheet = model.sheets[shown] ?? model.sheets[0];
  return (
    <>
      <p className="preview-note" role="note">
        Formulas are not calculated: the values saved with the workbook are shown. Layout, styles, charts and comments
        are not shown.
      </p>
      {model.truncated ? (
        <p className="preview-note" role="note">
          Some cells are cut short or not shown: at most {countOf(XLSX_MAX_CHARACTERS)} characters of the workbook are
          shown, and {countOf(XLSX_MAX_CELL_CHARACTERS)} of a cell. Download it to see all of it.
        </p>
      ) : null}
      {model.moreSheets ? (
        <p className="preview-note" role="note">
          Only the first {String(XLSX_MAX_SHEETS)} sheets are shown.
        </p>
      ) : null}
      {sheet === undefined ? (
        <p>There is no sheet in it.</p>
      ) : (
        <>
          {model.sheets.length > 1 ? (
            <nav className="preview-xlsx-sheets" aria-label="Sheets">
              {model.sheets.map((each, at) => (
                <button
                  // Sheets may share a name in a damaged workbook: their place is theirs.
                  key={at}
                  type="button"
                  className={`btn btn-sm ${at === shown ? 'btn-primary' : 'btn-outline-secondary'}`}
                  aria-pressed={at === shown}
                  aria-controls={panelId}
                  onClick={() => {
                    setShown(at);
                  }}
                >
                  {sheetLabel(each)}
                </button>
              ))}
            </nav>
          ) : null}
          <section id={panelId} className="preview-xlsx-sheet" aria-label={`Sheet ${sheetLabel(sheet)}`}>
            <Sheet sheet={sheet} objectKey={objectKey} />
          </section>
        </>
      )}
    </>
  );
}
