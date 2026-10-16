import { describe, expect, it } from 'vitest';

import { seeded, xlsxOf, type XlsxParts } from '../test/ooxml';
import { readOoxml } from './ooxml';
import { OoxmlError } from './ooxml-error';
import { checkedModel } from './ooxml-check';
import {
  columnName,
  XLSX_MAX_CELL_CHARACTERS,
  XLSX_MAX_CHARACTERS,
  XLSX_MAX_COLUMNS,
  XLSX_MAX_ROWS,
  XLSX_MAX_SHEET_NAME,
  XLSX_MAX_SHEETS,
  type XlsxModel,
} from './ooxml-model';
import { cellText, columnOf, dateFormatOf, formatSerial, MAX_FORMAT_CODE, readXlsx } from './xlsx';
import { ZipArchive } from './zip';

/** The model of an XLSX. */
function modelOf(workbook: XlsxParts): XlsxModel {
  return readXlsx(new ZipArchive(xlsxOf(workbook)));
}

/** The rows of the first sheet of an XLSX. */
function rowsOf(workbook: XlsxParts): string[][] {
  return modelOf(workbook).sheets[0]?.rows ?? [];
}

/** The problem an XLSX is refused with. */
function problemOf(workbook: XlsxParts): string | null {
  try {
    readOoxml('xlsx', xlsxOf(workbook));
    return null;
  } catch (error: unknown) {
    return error instanceof OoxmlError ? error.problem : 'other';
  }
}

describe('an XLSX read into its model', () => {
  it('reads shared strings - rich ones too, without their phonetic guides - inline strings, numbers, booleans and errors', () => {
    const rows = rowsOf({
      strings: ['<t>plain</t>', '<r><t>ri</t></r><r><rPr><b/></rPr><t>ch</t></r><rPh sb="0" eb="1"><t>FURIGANA</t></rPh>'],
      sheets: [
        {
          name: 'One',
          data:
            '<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="inlineStr"><is><t>inline</t></is></c></row>' +
            '<row r="2"><c r="A2"><v>3.25</v></c><c r="B2" t="b"><v>1</v></c><c r="C2" t="b"><v>0</v></c><c r="D2" t="e"><v>#DIV/0!</v></c><c r="E2" t="s"><v>99</v></c></row>',
        },
      ],
    });
    expect(rows).toEqual([
      ['plain', 'rich', 'inline'],
      ['3.25', 'TRUE', 'FALSE', '#DIV/0!'],
    ]);
  });

  it('shows the value saved for a formula, never the formula, and evaluates nothing', () => {
    const rows = rowsOf({
      strings: ['<t>=HYPERLINK("https://example.invalid","click")</t>'],
      sheets: [
        {
          name: 'F',
          data:
            '<row r="1"><c r="A1"><f>1+1</f><v>2</v></c><c r="B1" t="str"><f>HYPERLINK("https://example.invalid","go")</f><v>go</v></c>' +
            '<c r="C1" t="s"><v>0</v></c><c r="D1" t="inlineStr"><is><t>=HYPERLINK("javascript:alert(1)")</t></is></c><c r="E1"><f>NOW()</f></c></row>',
        },
      ],
    });
    expect(rows).toEqual([['2', 'go', '=HYPERLINK("https://example.invalid","click")', '=HYPERLINK("javascript:alert(1)")']]);
  });

  it('keeps markup in cells as text', () => {
    const rows = rowsOf({
      sheets: [{ name: 'M', data: '<row r="1"><c r="A1" t="inlineStr"><is><t>&lt;img src=x onerror=alert(1)&gt;</t></is></c></row>' }],
    });
    expect(rows).toEqual([['<img src=x onerror=alert(1)>']]);
  });

  it('places cells by their references, leaving gaps empty, and counts on where references are missing', () => {
    const rows = rowsOf({
      sheets: [
        {
          name: 'Sparse',
          data:
            '<row r="2"><c r="C2"><v>c2</v></c></row><row><c><v>a3</v></c><c><v>b3</v></c><c r="E3"><v>e3</v></c><c><v>f3</v></c></row>',
        },
      ],
    });
    expect(rows).toEqual([[], ['', '', 'c2'], ['a3', 'b3', '', '', 'e3', 'f3']]);
  });

  it('shows dates and times by built-in number formats and the workbook\'s own, in the 1900 and 1904 systems', () => {
    const styles =
      '<numFmts count="2"><numFmt numFmtId="164" formatCode="yyyy\\-mm\\-dd"/><numFmt numFmtId="165" formatCode="&quot;Day&quot;\\ 0.00"/></numFmts>' +
      '<cellXfs count="6"><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="22"/><xf numFmtId="18"/><xf numFmtId="164"/><xf numFmtId="165"/></cellXfs>';
    const data =
      '<row r="1"><c r="A1" s="1"><v>46310</v></c><c r="B1" s="2"><v>46310.5734375</v></c><c r="C1" s="3"><v>0.75</v></c>' +
      '<c r="D1" s="4"><v>1</v></c><c r="E1" s="5"><v>46310</v></c><c r="F1" s="0"><v>46310</v></c><c r="G1" s="1"><v>-1</v></c></row>';
    expect(rowsOf({ styles, sheets: [{ name: 'D', data }] })).toEqual([
      ['2026-10-15', '2026-10-15 13:45:45', '18:00:00', '1900-01-01', '46310', '46310', '-1'],
    ]);
    expect(rowsOf({ styles, workbookPr: 'date1904="1"', sheets: [{ name: 'D', data }] })[0]?.slice(0, 1)).toEqual(['2030-10-16']);
  });

  it.each([
    [14, undefined, 'date'],
    [22, undefined, 'datetime'],
    [21, undefined, 'time'],
    [46, undefined, 'time'],
    [0, undefined, null],
    [2, '0.00', null],
    [164, 'dd/mm/yyyy', 'date'],
    [165, 'h:mm AM/PM', 'time'],
    [166, 'yyyy-mm-dd hh:mm', 'datetime'],
    [167, '[Red]0.00;[Blue]-0.00', null],
    [168, '"days" 0', null],
    [169, '[h]:mm:ss', 'time'],
    [170, 'General', null],
    [171, '0.0E+00', null],
    [172, '[$-409]mmmm d, yyyy', 'date'],
  ] as const)('number format %i %j is %j', (id, code, expected) => {
    expect(dateFormatOf(id, code)).toBe(expected);
  });

  it('turns serials into dates as Excel does, the 29th of February 1900 that never was included', () => {
    expect(formatSerial(1, 'date', false)).toBe('1900-01-01');
    expect(formatSerial(59, 'date', false)).toBe('1900-02-28');
    expect(formatSerial(60, 'date', false)).toBe('1900-02-29');
    expect(formatSerial(61, 'date', false)).toBe('1900-03-01');
    expect(formatSerial(0, 'date', true)).toBe('1904-01-01');
    expect(formatSerial(2_958_465, 'date', false)).toBe('9999-12-31');
    expect(formatSerial(2_958_466, 'date', false)).toBeNull();
    expect(formatSerial(Number.NaN, 'time', false)).toBeNull();
    expect(formatSerial(0.999_999_99, 'datetime', false)).toBe('1900-01-01 00:00:00');
  });

  it(`reads no format code longer than ${String(MAX_FORMAT_CODE)} characters, and a long one quickly`, () => {
    expect(dateFormatOf(200, `${'0'.repeat(MAX_FORMAT_CODE - 4)}yyyy`)).toBe('date');
    expect(dateFormatOf(200, `${'0'.repeat(MAX_FORMAT_CODE - 3)}yyyy`)).toBeNull();
    const started = performance.now();
    expect(dateFormatOf(200, '['.repeat(1_000_000))).toBeNull();
    expect(dateFormatOf(200, '"'.repeat(MAX_FORMAT_CODE))).toBeNull();
    expect(performance.now() - started).toBeLessThan(100);
  });

  it('names columns by letters, both ways', () => {
    expect([1, 26, 27, 52, 703, 16_384].map(columnName)).toEqual(['A', 'Z', 'AA', 'AZ', 'AAA', 'XFD']);
    expect(['A1', 'z9', 'AA', 'XFD1048576', 'XFE1', '1A', ''].map(columnOf)).toEqual([1, 26, 27, 16_384, null, null, null]);
    const random = seeded(5);
    for (let i = 0; i < 500; i += 1) {
      const column = 1 + Math.floor(random() * 16_384);
      expect(columnOf(`${columnName(column)}7`)).toBe(column);
    }
  });

  it('shows a cell of a type it does not know as what was saved', () => {
    expect(cellText('x', 'saved', '', null, { strings: [], date1904: false })).toBe('saved');
    expect(cellText('d', '2026-10-15T00:00:00', '', 'date', { strings: [], date1904: false })).toBe('2026-10-15T00:00:00');
  });

  it(`shows the first ${String(XLSX_MAX_ROWS)} rows and ${String(XLSX_MAX_COLUMNS)} columns, and says there are more`, () => {
    const row = (r: number) =>
      `<row r="${String(r)}">${Array.from({ length: 102 }, (_, c) => `<c r="${columnName(c + 1)}${String(r)}"><v>${String(r * 1000 + c)}</v></c>`).join('')}</row>`;
    const data = Array.from({ length: XLSX_MAX_ROWS + 2 }, (_, r) => row(r + 1)).join('');
    const [sheet] = modelOf({ sheets: [{ name: 'Big', data }] }).sheets;
    expect(sheet?.rows).toHaveLength(XLSX_MAX_ROWS);
    expect(sheet?.rows.every((cells) => cells.length === XLSX_MAX_COLUMNS)).toBe(true);
    expect(sheet?.rows.at(-1)?.at(-1)).toBe(String(XLSX_MAX_ROWS * 1000 + 99));
    expect(sheet?.moreRows).toBe(true);
    expect(sheet?.moreColumns).toBe(true);
  });

  it(`shows the first ${String(XLSX_MAX_SHEETS)} sheets, says there are more, and tells charts and hidden sheets`, () => {
    const sheets = Array.from({ length: XLSX_MAX_SHEETS + 3 }, (_, at) => ({
      name: `S${String(at + 1)}`,
      data: `<row r="1"><c r="A1"><v>${String(at)}</v></c></row>`,
    }));
    const model = modelOf({
      sheets: [{ name: 'Chart', chart: true }, { name: 'Hidden', state: 'hidden', data: '' }, ...sheets],
    });
    expect(model.sheets).toHaveLength(XLSX_MAX_SHEETS);
    expect(model.moreSheets).toBe(true);
    expect(model.sheets.slice(0, 3).map((sheet) => [sheet.name, sheet.content, sheet.hidden, sheet.rows])).toEqual([
      ['Chart', 'chart', false, []],
      ['Hidden', 'cells', true, []],
      ['S1', 'cells', false, [['0']]],
    ]);
  });

  it(`counts a shared string each time a cell shows it: ${String(XLSX_MAX_CHARACTERS)} characters in all, ${String(XLSX_MAX_CELL_CHARACTERS)} a cell`, () => {
    // One string of 1 MB, shown by 100 000 cells: 10^11 characters if each cell were given all of it.
    const random = seeded(17);
    const long = Array.from({ length: 1_000_000 }, () => String.fromCharCode(97 + Math.floor(random() * 26))).join('');
    const data = Array.from(
      { length: XLSX_MAX_ROWS },
      (_, r) =>
        `<row r="${String(r + 1)}">${Array.from({ length: XLSX_MAX_COLUMNS }, (_, c) => `<c r="${columnName(c + 1)}${String(r + 1)}" t="s"><v>0</v></c>`).join('')}</row>`,
    ).join('');
    const bytes = xlsxOf({ strings: [`<t>${long}</t>`], sheets: [{ name: 'Bomb', data }, { name: 'After', data }] });
    expect(bytes.length).toBeLessThan(1024 * 1024);
    const model = readXlsx(new ZipArchive(bytes));
    const cells = model.sheets.flatMap((sheet) => sheet.rows.flat());
    expect(cells.reduce((sum, cell) => sum + cell.length, 0)).toBe(XLSX_MAX_CHARACTERS);
    expect(cells.every((cell) => cell.length <= XLSX_MAX_CELL_CHARACTERS)).toBe(true);
    expect(model.truncated).toBe(true);
    expect(model.sheets[1]?.rows).toEqual([]);
    expect(checkedModel(model)).not.toBeNull();
  });

  it(`shows at most ${String(XLSX_MAX_SHEET_NAME)} characters of a sheet's name`, () => {
    const model = modelOf({ sheets: [{ name: 'N'.repeat(5000), data: '' }] });
    expect(model.sheets[0]?.name).toBe('N'.repeat(XLSX_MAX_SHEET_NAME));
    expect(model.truncated).toBe(false);
  });

  it.each([
    ['in a sheet', { 'xl/worksheets/sheet1.xml': '<!DOCTYPE w [<!ENTITY a "aaaa">]><worksheet/>' }],
    ['in the shared strings', { 'xl/sharedStrings.xml': '<!ENTITY a SYSTEM "file:///etc/passwd"><sst/>' }],
    ['in the workbook', { 'xl/workbook.xml': '<!DOCTYPE workbook><workbook/>' }],
  ])('refuses a DOCTYPE or an ENTITY %s as unsafe', (_, parts) => {
    expect(problemOf({ strings: ['<t>x</t>'], sheets: [{ name: 'One', data: '' }], parts })).toBe('unsafe');
  });

  it.each([
    ['no workbook', { 'xl/workbook.xml': '<other/>' }],
    ['a sheet missing', { 'xl/worksheets/sheet1.xml': '' }],
  ])('refuses %s as damaged', (_, parts) => {
    expect(problemOf({ sheets: [{ name: 'One', data: '' }], parts })).toBe('damaged');
  });
});
