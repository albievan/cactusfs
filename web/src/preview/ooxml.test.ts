import { describe, expect, it } from 'vitest';

import { docxOf, pngOf, relType, seeded, xlsxOf } from '../test/ooxml';
import { answer, readOoxml } from './ooxml';
import { OoxmlError } from './ooxml-error';
import type { OoxmlKind } from './ooxml-model';

/** Reads a document; anything thrown that is not an OoxmlError fails the test. */
function readOrRefuse(kind: OoxmlKind, bytes: Uint8Array) {
  try {
    return readOoxml(kind, bytes);
  } catch (error: unknown) {
    expect(error).toBeInstanceOf(OoxmlError);
    return null;
  }
}

/** A random tree of these elements, with these attributes and words, as XML. */
function randomXml(random: () => number, names: readonly string[], attributes: readonly string[], depth: number): string {
  const pick = <T,>(items: readonly T[]): T => items[Math.floor(random() * items.length)] as T;
  const name = pick(names);
  const attrs = random() < 0.6 ? ` ${pick(attributes)}` : '';
  if (depth === 0 || random() < 0.2) {
    return random() < 0.5 ? `<${name}${attrs}/>` : `<${name}${attrs}>${pick(['x', '1', '0', '46310', '&lt;b&gt;', ' ', ''])}</${name}>`;
  }
  let inner = '';
  for (let i = Math.floor(random() * 5); i > 0; i -= 1) {
    inner += randomXml(random, names, attributes, depth - 1);
  }
  return `<${name}${attrs}>${inner}</${name}>`;
}

const WORD_NAMES = [
  'w:p', 'w:r', 'w:t', 'w:pPr', 'w:rPr', 'w:pStyle', 'w:numPr', 'w:ilvl', 'w:numId', 'w:outlineLvl', 'w:b', 'w:i', 'w:u',
  'w:br', 'w:cr', 'w:tab', 'w:tbl', 'w:tr', 'w:tc', 'w:tcPr', 'w:gridSpan', 'w:drawing', 'a:blip', 'w:del', 'w:delText',
  'w:instrText', 'w:txbxContent', 'mc:Fallback', 'w:hyperlink', 'w:sdt', 'w:sdtContent', 'w:noBreakHyphen', 'w:pPrChange',
];
const WORD_ATTRIBUTES = ['w:val="Heading1"', 'w:val="1"', 'w:val="0"', 'w:val="-5"', 'w:val="99999999999"', 'r:embed="img"', 'r:embed="none"', 'r:link="x"', 'w:val="none"'];
const SHEET_NAMES = ['row', 'c', 'v', 'f', 'is', 't', 'r', 'rPh', 'sheetData'];
const SHEET_ATTRIBUTES = ['r="A1"', 'r="XFD9"', 'r="1001"', 'r="3"', 't="s"', 't="b"', 't="inlineStr"', 't="e"', 's="1"', 's="99"', 'r="?"'];

describe('the readers, by fuzz', () => {
  it('make a model of any well-formed body of a DOCX, or refuse it with an OoxmlError', () => {
    const random = seeded(0xf4ecdd);
    let read = 0;
    for (let round = 0; round < 300; round += 1) {
      const body = Array.from({ length: 1 + Math.floor(random() * 4) }, () => randomXml(random, WORD_NAMES, WORD_ATTRIBUTES, 5)).join('');
      const bytes = docxOf(body, {
        // A style based on itself is followed no further than 16 steps.
        styles: '<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Heading1"/></w:style>',
        numbering: '<w:num w:numId="1"><w:abstractNumId w:val="1"/></w:num>',
        relationships: [['img', relType('image'), 'media/a.png']],
        parts: { 'word/media/a.png': pngOf(2, 2) },
      });
      read += readOrRefuse('docx', bytes) === null ? 0 : 1;
    }
    // Most are read: the fuzz reaches the reader, not only its refusals.
    expect(read).toBeGreaterThan(250);
  });

  it('make a model of any well-formed sheet of an XLSX, or refuse it with an OoxmlError', () => {
    const random = seeded(0x2e6b4e);
    let read = 0;
    for (let round = 0; round < 300; round += 1) {
      const data = Array.from({ length: 1 + Math.floor(random() * 4) }, () => randomXml(random, SHEET_NAMES, SHEET_ATTRIBUTES, 4)).join('');
      const bytes = xlsxOf({
        strings: ['<t>s</t>'],
        styles: '<cellXfs><xf numFmtId="14"/><xf numFmtId="164"/></cellXfs>',
        sheets: [{ name: 'F', data }],
      });
      read += readOrRefuse('xlsx', bytes) === null ? 0 : 1;
    }
    expect(read).toBeGreaterThan(250);
  });

  it('answer a document that throws something else as damaged, saying nothing of it', () => {
    // A buffer that is detached cannot be read: whatever is thrown, the answer is a problem.
    const bytes = new ArrayBuffer(8);
    structuredClone(bytes, { transfer: [bytes] });
    expect(answer({ kind: 'docx', bytes })).toEqual({ reply: { ok: false, problem: 'damaged' }, transfer: [] });
  });
});
