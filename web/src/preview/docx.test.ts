import { strToU8 } from 'fflate';
import { describe, expect, it } from 'vitest';

import { docxOf, gifOf, para, picture, pngOf, R, relType, seeded, W } from '../test/ooxml';
import { acceptedImage, GIF_MAX_FRAMES, gifFramesFit, headingOfStyleName, imageKindOf, imageSizeOf, readDocx } from './docx';
import { groupBlocks } from './DocxPreview';
import { readOoxml } from './ooxml';
import { checkedModel } from './ooxml-check';
import { OoxmlError } from './ooxml-error';
import {
  DOCX_MAX_BLOCKS,
  DOCX_MAX_NODES,
  DOCX_MAX_TABLE_DEPTH,
  type DocxBlock,
  type DocxModel,
  type DocxParagraph,
} from './ooxml-model';
import { ZipArchive } from './zip';

/** The model of a DOCX. */
function modelOf(bytes: Uint8Array): DocxModel {
  return readDocx(new ZipArchive(bytes));
}

/** The text of a paragraph. */
function textOf(block: DocxBlock | undefined): string {
  if (block?.type !== 'paragraph') {
    return '';
  }
  return block.inlines.map((inline) => (inline.type === 'text' ? inline.text : inline.type === 'break' ? '\n' : '[img]')).join('');
}

/** The problem a DOCX is refused with. */
function problemOf(bytes: Uint8Array): string | null {
  try {
    readOoxml('docx', bytes);
    return null;
  } catch (error: unknown) {
    return error instanceof OoxmlError ? error.problem : 'other';
  }
}

/** A paragraph of a model, as a paragraph. */
function paragraph(block: DocxBlock | undefined): DocxParagraph {
  if (block?.type !== 'paragraph') {
    throw new Error('not a paragraph');
  }
  return block;
}

const STYLES = `
  <w:style w:type="paragraph" w:styleId="berschrift1"><w:name w:val="heading 1"/></w:style>
  <w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/></w:style>
  <w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style>
  <w:style w:type="paragraph" w:styleId="Mine"><w:name w:val="My heading"/><w:basedOn w:val="Heading2"/></w:style>
  <w:style w:type="paragraph" w:styleId="Outline"><w:name w:val="Outlined"/><w:pPr><w:outlineLvl w:val="3"/></w:pPr></w:style>
  <w:style w:type="paragraph" w:styleId="Bullets"><w:name w:val="List Bullet"/><w:pPr><w:numPr><w:numId w:val="2"/></w:numPr></w:pPr></w:style>
  <w:style w:type="character" w:styleId="Heading1Char"><w:name w:val="heading 1"/></w:style>`;

const NUMBERING = `
  <w:abstractNum w:abstractNumId="10"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>
  <w:abstractNum w:abstractNumId="20"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>
  <w:num w:numId="1"><w:abstractNumId w:val="10"/></w:num>
  <w:num w:numId="2"><w:abstractNumId w:val="20"/></w:num>`;

describe('a DOCX read into its model', () => {
  it('reads paragraphs, and headings by the names of their styles, outline levels and what they are based on', () => {
    const model = modelOf(
      docxOf(
        [
          para('Plain'),
          para('Localised', '<w:pStyle w:val="berschrift1"/>'),
          para('Second', '<w:pStyle w:val="Heading2"/>'),
          para('Title', '<w:pStyle w:val="Title"/>'),
          para('Based', '<w:pStyle w:val="Mine"/>'),
          para('Outlined', '<w:pStyle w:val="Outline"/>'),
          para('Direct', '<w:outlineLvl w:val="0"/>'),
          para('Body', '<w:outlineLvl w:val="9"/>'),
          para('Unknown style', '<w:pStyle w:val="Nothing"/>'),
        ].join(''),
        { styles: STYLES },
      ),
    );
    expect(model.blocks.map((block) => [textOf(block), paragraph(block).heading])).toEqual([
      ['Plain', 0],
      ['Localised', 1],
      ['Second', 2],
      ['Title', 1],
      ['Based', 2],
      ['Outlined', 4],
      ['Direct', 1],
      ['Body', 0],
      ['Unknown style', 0],
    ]);
    expect(model.truncated).toBe(false);
  });

  it('names headings from "heading 1" to "heading 9" and Title, nothing else', () => {
    expect(headingOfStyleName('heading 1')).toBe(1);
    expect(headingOfStyleName('Heading 9')).toBe(9);
    expect(headingOfStyleName('title')).toBe(1);
    expect(headingOfStyleName('heading 10')).toBeNull();
    expect(headingOfStyleName('Subtitle')).toBeNull();
  });

  it('reads lists, numbered or not, by their levels, directly and by style', () => {
    const item = (text: string, numId: string, level: number) =>
      para(text, `<w:numPr><w:ilvl w:val="${String(level)}"/><w:numId w:val="${numId}"/></w:numPr>`);
    const model = modelOf(
      docxOf(
        [
          item('one', '1', 0),
          item('one.a', '1', 1),
          item('two', '1', 0),
          para('by style', '<w:pStyle w:val="Bullets"/>'),
          item('no list', '0', 0),
          item('unknown list', '99', 0),
        ].join(''),
        { styles: STYLES, numbering: NUMBERING },
      ),
    );
    expect(model.blocks.map((block) => paragraph(block).list)).toEqual([
      { level: 0, ordered: true },
      { level: 1, ordered: false },
      { level: 0, ordered: true },
      { level: 0, ordered: false },
      null,
      { level: 0, ordered: false },
    ]);
  });

  it('reads bold, italic and underline - and their being turned off - tabs and line breaks', () => {
    const model = modelOf(
      docxOf(
        `<w:p>
          <w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r>
          <w:r><w:rPr><w:b w:val="0"/><w:i/></w:rPr><w:t>italic</w:t></w:r>
          <w:r><w:rPr><w:u w:val="single"/></w:rPr><w:t>under</w:t><w:tab/><w:t>lined</w:t></w:r>
          <w:r><w:rPr><w:u w:val="none"/></w:rPr><w:t xml:space="preserve"> plain </w:t><w:br/><w:t>next line</w:t></w:r>
          <w:r><w:t>joined</w:t></w:r>
        </w:p>`,
      ),
    );
    expect(paragraph(model.blocks[0]).inlines).toEqual([
      { type: 'text', text: 'bold', bold: true, italic: false, underline: false },
      { type: 'text', text: 'italic', bold: false, italic: true, underline: false },
      { type: 'text', text: 'under\tlined', bold: false, italic: false, underline: true },
      { type: 'text', text: ' plain ', bold: false, italic: false, underline: false },
      { type: 'break' },
      { type: 'text', text: 'next linejoined', bold: false, italic: false, underline: false },
    ]);
  });

  it('reads tables, cells that span columns, and tables in cells', () => {
    const cell = (inner: string, span = '') => `<w:tc>${span === '' ? '' : `<w:tcPr><w:gridSpan w:val="${span}"/></w:tcPr>`}${inner}</w:tc>`;
    const model = modelOf(
      docxOf(
        `<w:tbl><w:tr>${cell(para('a'))}${cell(para('b'))}</w:tr><w:tr>${cell(
          `<w:tbl><w:tr>${cell(para('inner'))}</w:tr></w:tbl>${para('after')}`,
          '2',
        )}</w:tr></w:tbl>${para('below')}`,
      ),
    );
    const [table, below] = model.blocks;
    expect(textOf(below)).toBe('below');
    if (table?.type !== 'table') {
      throw new Error('no table');
    }
    expect(table.rows.map((row) => row.map((c) => c.span))).toEqual([[1, 1], [2]]);
    expect(textOf(table.rows[0]?.[0]?.blocks[0])).toBe('a');
    const inner = table.rows[1]?.[0]?.blocks[0];
    expect(inner?.type).toBe('table');
    expect(textOf(table.rows[1]?.[0]?.blocks[1])).toBe('after');
  });

  it('reads no cell inside a cell of the same table, so that later tables do not nest in earlier cells', () => {
    // Before, each copy nested the next table in its cell: 5 000 deep. The text
    // in the inner cell varies, as a file's does, and is never read.
    const random = seeded(29);
    const odd = () => `<w:tbl><w:tr><w:tc><w:tc>${para(random().toString(36).slice(2))}</w:tc></w:tc></w:tr></w:tbl>`;
    const model = modelOf(docxOf(`${Array.from({ length: 5000 }, odd).join('')}${para('after')}`));
    expect(model.blocks).toHaveLength(5001);
    expect(model.deepTables).toBe(false);
    expect(model.blocks.slice(0, -1).every((block) => block.type === 'table' && block.rows[0]?.length === 1 && block.rows[0][0]?.blocks.length === 0)).toBe(true);
    expect(textOf(model.blocks.at(-1))).toBe('after');
    expect(checkedModel(model)).not.toBeNull();
    expect(() => structuredClone(model)).not.toThrow();
  });

  it(`leaves out tables nested deeper than ${String(DOCX_MAX_TABLE_DEPTH)}, and says so`, () => {
    let body = para('deepest');
    for (let i = 0; i <= DOCX_MAX_TABLE_DEPTH; i += 1) {
      body = `<w:tbl><w:tr><w:tc>${body}</w:tc></w:tr></w:tbl>`;
    }
    const model = modelOf(docxOf(body));
    expect(model.deepTables).toBe(true);
    expect(JSON.stringify(model)).not.toContain('deepest');
  });

  it('leaves out deleted text, the codes of fields, text boxes and the fallbacks of markup compatibility', () => {
    const model = modelOf(
      docxOf(
        `<w:p><w:r><w:t>kept</w:t></w:r><w:del><w:r><w:delText>deleted</w:delText></w:r></w:del>
        <w:r><w:instrText>HYPERLINK "https://example.invalid"</w:instrText></w:r>
        <w:r><mc:AlternateContent><mc:Choice Requires="wps"><w:drawing><w:txbxContent>${para('boxed')}</w:txbxContent></w:drawing></mc:Choice><mc:Fallback><w:pict>${para('fallback')}</w:pict></mc:Fallback></mc:AlternateContent></w:r>
        <w:r><w:t>, too</w:t></w:r></w:p>`,
      ),
    );
    expect(model.blocks.map(textOf)).toEqual(['kept, too']);
  });

  it('keeps markup in the text as text', () => {
    const model = modelOf(docxOf(para('&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src=x onerror=alert(2)&gt;')));
    expect(textOf(model.blocks[0])).toBe('<script>alert(1)</script><img src=x onerror=alert(2)>');
  });

  it('shows embedded PNG, JPEG and GIF images checked by their signature, and nothing else', () => {
    const jpeg = new Uint8Array([0xff, 0xd8, 0xff, 0xe0, 0, 4, 0, 0, 0xff, 0xc0, 0, 11, 8, 0, 20, 0, 30, 1, 0, 0, 0]);
    const gif = gifOf(5, 6);
    const model = modelOf(
      docxOf(
        `<w:p>${picture('r:embed', 'png')}${picture('r:embed', 'jpeg')}${picture('r:embed', 'gif')}${picture('r:embed', 'png')}</w:p>
         <w:p>${picture('r:embed', 'html')}${picture('r:embed', 'svg')}${picture('r:embed', 'huge')}${picture('r:link', 'web')}${picture('r:embed', 'external')}${picture('r:embed', 'gone')}</w:p>`,
        {
          relationships: [
            ['png', relType('image'), 'media/image1.png'],
            ['jpeg', relType('image'), 'media/photo.jpeg'],
            ['gif', relType('image'), '/word/media/anim.gif'],
            ['html', relType('image'), 'media/page.png'],
            ['svg', relType('image'), 'media/drawing.svg'],
            ['huge', relType('image'), 'media/huge.png'],
            ['web', relType('image'), 'https://example.invalid/tracker.png', true],
            ['external', relType('image'), 'https://example.invalid/b.png', true],
            ['gone', relType('image'), 'media/none.png'],
          ],
          parts: {
            'word/media/image1.png': pngOf(40, 30),
            'word/media/photo.jpeg': jpeg,
            'word/media/anim.gif': gif,
            'word/media/page.png': strToU8('<html><script>alert(1)</script></html>'),
            'word/media/drawing.svg': strToU8('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>'),
            'word/media/huge.png': pngOf(65_535, 65_535),
          },
        },
      ),
    );
    expect(model.images.map((image) => image.kind)).toEqual(['png', 'jpeg', 'gif']);
    expect(Array.from(model.images[1]?.bytes ?? [])).toEqual(Array.from(jpeg));
    expect(paragraph(model.blocks[0]).inlines).toEqual([
      { type: 'image', image: 0 },
      { type: 'image', image: 1 },
      { type: 'image', image: 2 },
      { type: 'image', image: 0 },
    ]);
    expect(paragraph(model.blocks[1]).inlines).toEqual(Array.from({ length: 6 }, () => ({ type: 'image-omitted' })));
  });

  it('judges an image by its signature and its size, as an image previewed is judged', () => {
    expect(imageKindOf(pngOf(1, 1))).toBe('png');
    expect(imageKindOf(strToU8('GIF87a'))).toBe('gif');
    expect(imageKindOf(strToU8('GIF8xa'))).toBeNull();
    expect(imageKindOf(strToU8('<svg'))).toBeNull();
    expect(imageSizeOf('png', pngOf(3, 4))).toEqual({ width: 3, height: 4 });
    expect(acceptedImage(pngOf(16_384, 2_000))).toEqual({ kind: 'png', pixels: 32_768_000 });
    expect(acceptedImage(pngOf(16_385, 1))).toBeNull();
    expect(acceptedImage(pngOf(8000, 5001))).toBeNull();
    expect(acceptedImage(pngOf(0, 10))).toBeNull();
    // A JPEG whose data begins before any frame header says no size.
    expect(acceptedImage(new Uint8Array([0xff, 0xd8, 0xff, 0xda, 0, 2]))).toBeNull();
    expect(acceptedImage(new Uint8Array([0xff, 0xd8, 0xff]))).toBeNull();
  });

  it('judges a GIF by every frame, not by its screen alone', () => {
    expect(gifFramesFit(gifOf(10, 10, [[0, 0, 10, 10], [5, 5, 5, 5]]))).toBe(true);
    // A 1×1 screen with a frame of 65 535 × 65 535: a browser would make the frame.
    expect(acceptedImage(gifOf(1, 1, [[0, 0, 65_535, 65_535]]))).toBeNull();
    expect(gifFramesFit(gifOf(10, 10, [[9, 0, 2, 1]]))).toBe(false);
    expect(gifFramesFit(gifOf(10, 10, [[0, 0, 1, 1]], false))).toBe(false);
    expect(gifFramesFit(gifOf(2, 2, Array.from({ length: GIF_MAX_FRAMES + 1 }, () => [0, 0, 1, 1] as const)))).toBe(false);
    expect(gifFramesFit(gifOf(2, 2, Array.from({ length: GIF_MAX_FRAMES }, () => [0, 0, 1, 1] as const)))).toBe(true);
    // A block that is no GIF block.
    const odd = gifOf(2, 2);
    odd[21] = 0x99;
    expect(gifFramesFit(odd)).toBe(false);
  });

  it('shows images to 100 megapixels in all, and leaves out those past it', () => {
    const model = modelOf(
      docxOf(`<w:p>${picture('r:embed', 'a')}${picture('r:embed', 'b')}${picture('r:embed', 'c')}${picture('r:embed', 'a')}</w:p>`, {
        relationships: [
          ['a', relType('image'), 'media/a.png'],
          ['b', relType('image'), 'media/b.png'],
          ['c', relType('image'), 'media/c.png'],
        ],
        // 40 MP each: two are 80 MP, a third would be 120.
        parts: { 'word/media/a.png': pngOf(8000, 5000), 'word/media/b.png': pngOf(5000, 8000), 'word/media/c.png': pngOf(8000, 5000).fill(1, 26) },
      }),
    );
    expect(model.images).toHaveLength(2);
    expect(paragraph(model.blocks[0]).inlines).toEqual([
      { type: 'image', image: 0 },
      { type: 'image', image: 1 },
      { type: 'image-omitted' },
      { type: 'image', image: 0 },
    ]);
  });

  it.each([
    [
      'line breaks',
      (random: () => number) => `<w:p>${Array.from({ length: 40_000 }, () => `<w:r><w:t>${random().toString(36).slice(2, 6)}</w:t><w:br/></w:r>`).join('')}</w:p>`,
    ],
    [
      'references to one image',
      (random: () => number) =>
        `<w:p>${Array.from({ length: 30_000 }, () => `<w:r><w:t>${random().toString(36).slice(2, 6)}</w:t></w:r>${picture('r:embed', 'img')}`).join('')}</w:p>`,
    ],
    [
      'references to images not shown',
      (random: () => number) =>
        `<w:p>${Array.from({ length: 30_000 }, () => `<w:r><w:t>${random().toString(36).slice(2, 6)}</w:t></w:r>${picture('r:link', 'web')}`).join('')}</w:p>`,
    ],
    [
      'rows and cells',
      (random: () => number) =>
        `<w:tbl>${Array.from({ length: 600 }, () => `<w:tr>${Array.from({ length: 100 }, () => `<w:tc>${random() < 0.1 ? para(random().toString(36).slice(2, 5)) : ''}</w:tc>`).join('')}</w:tr>`).join('')}</w:tbl>`,
    ],
    [
      'runs of changing emphasis',
      (random: () => number) =>
        `<w:p>${Array.from({ length: 60_000 }, (_, i) => `<w:r><w:rPr>${i % 2 === 0 ? '<w:b/>' : ''}</w:rPr><w:t>${random().toString(36).slice(2, 4)}</w:t></w:r>`).join('')}</w:p>`,
    ],
  ])(`counts %s against ${String(DOCX_MAX_NODES)} nodes, stops there, and says so`, (_, body) => {
    const bytes = docxOf(body(seeded(13)), {
      relationships: [
        ['img', relType('image'), 'media/a.png'],
        ['web', relType('image'), 'https://example.invalid/a.png', true],
      ],
      parts: { 'word/media/a.png': pngOf(2, 2) },
    });
    expect(bytes.length).toBeLessThan(1024 * 1024);
    const model = modelOf(bytes);
    expect(model.truncated).toBe(true);
    // The page takes it: within the bound it counts the same way.
    expect(checkedModel(model)).not.toBeNull();
    expect(JSON.stringify(model).length).toBeLessThan(5 * 1024 * 1024);
  });

  it(`stops at ${String(DOCX_MAX_BLOCKS)} blocks and says it did`, () => {
    // Text that differs, as a document's does: the same paragraph ten thousand times deflates past 100:1.
    const random = seeded(3);
    const body = Array.from({ length: DOCX_MAX_BLOCKS + 5 }, () => para(random().toString(36).slice(2))).join('');
    const model = modelOf(docxOf(body));
    expect(model.blocks).toHaveLength(DOCX_MAX_BLOCKS);
    expect(model.truncated).toBe(true);
  });

  it('reads a document of strict OOXML and one with other prefixes', () => {
    const strict = 'http://purl.oclc.org/ooxml/wordprocessingml/main';
    const model = modelOf(
      docxOf('', {
        document: `<x:document xmlns:x="${strict}" xmlns:r="${R}"><x:body><x:p><x:pPr><x:pStyle x:val="T"/></x:pPr><x:r><x:t>strict</x:t></x:r></x:p></x:body></x:document>`,
        styles: '<w:style w:type="paragraph" w:styleId="T"><w:name w:val="heading 3"/></w:style>',
      }),
    );
    expect(model.blocks.map((block) => [textOf(block), paragraph(block).heading])).toEqual([['strict', 3]]);
  });

  it.each([
    ['in the document', { document: `<!DOCTYPE w:document [<!ENTITY e "x">]><w:document xmlns:w="${W}"><w:body/></w:document>` }],
    ['in the styles', { styles: '', parts: { 'word/styles.xml': '<!DOCTYPE s SYSTEM "http://example.invalid/s.dtd"><w:styles/>' } }],
    ['in the relationships', { parts: { 'word/_rels/document.xml.rels': '<!DOCTYPE r><Relationships/>' } }],
  ])('refuses a DOCTYPE or an ENTITY %s as unsafe', (_, extra) => {
    expect(problemOf(docxOf(para('x'), extra))).toBe('unsafe');
  });

  it.each([
    ['a zip without a document', docxOf('', { parts: { '_rels/.rels': '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>' } })],
    ['a document that is not XML', docxOf('', { document: 'not xml' })],
    ['a document with an entity of a DTD', docxOf(para('&nbsp;'))],
    ['bytes that are no zip', strToU8('PK\u0003\u0004 and nothing more')],
  ])('refuses %s as damaged', (_, bytes) => {
    expect(problemOf(bytes)).toBe('damaged');
  });
});

describe('the lists of a document as they are shown', () => {
  const item = (text: string, level: number, ordered: boolean): DocxParagraph => ({
    type: 'paragraph',
    heading: 0,
    list: { level, ordered },
    inlines: [{ type: 'text', text, bold: false, italic: false, underline: false }],
  });
  const plain: DocxParagraph = { type: 'paragraph', heading: 0, list: null, inlines: [] };

  it('gathers items into lists in lists, and ends a list at a paragraph that is no item', () => {
    const groups = groupBlocks([item('1', 0, true), item('1.a', 1, false), item('1.b', 1, false), item('2', 0, true), plain, item('again', 0, true)]);
    expect(groups.map((group) => group.type)).toEqual(['list', 'block', 'list']);
    const [first] = groups;
    if (first?.type !== 'list') {
      throw new Error('no list');
    }
    expect(first.list.ordered).toBe(true);
    expect(first.list.items.map((each) => textOf(each.paragraph ?? undefined))).toEqual(['1', '2']);
    expect(first.list.items[0]?.lists[0]?.items.map((each) => textOf(each.paragraph ?? undefined))).toEqual(['1.a', '1.b']);
  });

  it('fills a level a document skips with an item of no paragraph, and begins a new list where numbering changes', () => {
    const groups = groupBlocks([item('deep', 2, false), item('bullet', 0, false), item('number', 0, true)]);
    expect(groups).toHaveLength(2);
    const [skipped, numbered] = groups;
    if (skipped?.type !== 'list' || numbered?.type !== 'list') {
      throw new Error('no list');
    }
    expect(skipped.list.items.map((each) => textOf(each.paragraph ?? undefined))).toEqual(['', 'bullet']);
    expect(numbered.list.ordered).toBe(true);
    expect(skipped.list.items[0]?.paragraph).toBeNull();
    expect(skipped.list.items[0]?.lists[0]?.items[0]?.paragraph).toBeNull();
    expect(textOf(skipped.list.items[0]?.lists[0]?.items[0]?.lists[0]?.items[0]?.paragraph ?? undefined)).toBe('deep');
  });
});
