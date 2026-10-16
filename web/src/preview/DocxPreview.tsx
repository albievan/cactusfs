import type { ReactNode } from 'react';

import { imageTypeOf } from './kinds';
import { DOCX_MAX_TABLE_DEPTH, type DocxBlock, type DocxInline, type DocxModel, type DocxParagraph } from './ooxml-model';

/** A list of a document: its items in order, numbered or not. */
export interface DocxList {
  ordered: boolean;
  items: DocxListItem[];
}

/** An item of a list: its paragraph (none for a level a document skipped) and the lists in it. */
export interface DocxListItem {
  paragraph: DocxParagraph | null;
  lists: DocxList[];
}

/** A block of a document as it is shown: a paragraph or a table, or a list of the paragraphs that are its items. */
export type DocxGroup = { type: 'block'; block: DocxBlock } | { type: 'list'; list: DocxList };

/**
 * Gathers paragraphs that are items of a list, one after another, into
 * lists in lists by their levels. A level a document skips (an item of
 * level 2 straight after one of level 0) is an item with no paragraph; a
 * list that changes from numbered to not, or back, at a level begins a new
 * list there.
 */
export function groupBlocks(blocks: readonly DocxBlock[]): DocxGroup[] {
  const groups: DocxGroup[] = [];
  // The list open at each level.
  let open: DocxList[] = [];
  const begin = (level: number, ordered: boolean) => {
    const list: DocxList = { ordered, items: [] };
    if (level === 0) {
      groups.push({ type: 'list', list });
    } else {
      const parent = open[level - 1];
      let item = parent?.items.at(-1);
      if (parent !== undefined && item === undefined) {
        item = { paragraph: null, lists: [] };
        parent.items.push(item);
      }
      item?.lists.push(list);
    }
    open = [...open.slice(0, level), list];
  };
  for (const block of blocks) {
    if (block.type !== 'paragraph' || block.list === null) {
      open = [];
      groups.push({ type: 'block', block });
      continue;
    }
    const { level, ordered } = block.list;
    open = open.slice(0, level + 1);
    while (open.length < level) {
      begin(open.length, ordered);
    }
    if (open[level]?.ordered !== ordered) {
      begin(level, ordered);
    }
    open[level]?.items.push({ paragraph: block, lists: [] });
  }
  return groups;
}

/**
 * Blob addresses of a document's images, by their place, each typed as the
 * kind its signature gave - a raster type, never SVG. Whoever makes them
 * revokes them (revokeImageUrls) when the document is no longer shown.
 */
export function makeImageUrls(model: DocxModel): (string | null)[] {
  return model.images.map((image) => {
    const type = imageTypeOf(image.kind);
    return type === null ? null : URL.createObjectURL(new Blob([image.bytes], { type }));
  });
}

/** Gives back the addresses makeImageUrls made. */
export function revokeImageUrls(urls: readonly (string | null)[]) {
  for (const url of urls) {
    if (url !== null) {
      URL.revokeObjectURL(url);
    }
  }
}

/** What a paragraph holds: text with its emphasis, line breaks and images. */
function Inline({ inline, urls }: { inline: DocxInline; urls: (string | null)[] }) {
  switch (inline.type) {
    case 'text': {
      let node: ReactNode = inline.text;
      if (inline.underline) {
        node = <u>{node}</u>;
      }
      if (inline.italic) {
        node = <em>{node}</em>;
      }
      if (inline.bold) {
        node = <strong>{node}</strong>;
      }
      return node;
    }
    case 'break':
      return <br />;
    case 'image': {
      const url = urls[inline.image] ?? null;
      return url === null ? null : <img className="preview-docx-image" src={url} alt="An image of the document" />;
    }
    case 'image-omitted':
      return <span className="preview-docx-omitted">[an image that is not shown]</span>;
  }
}

/** The inlines of a paragraph. */
function Inlines({ paragraph, urls }: { paragraph: DocxParagraph; urls: (string | null)[] }) {
  return paragraph.inlines.map((inline, at) => (
    // Inlines have no key of their own: their place is what they are.
    <Inline key={at} inline={inline} urls={urls} />
  ));
}

/** The element of a heading: the dialog's own title is the h2. */
const HEADINGS = ['h3', 'h4', 'h5', 'h6'] as const;

/** A paragraph: a heading, or a paragraph of text. */
function Paragraph({ paragraph, urls }: { paragraph: DocxParagraph; urls: (string | null)[] }) {
  if (paragraph.heading > 0) {
    const Heading = HEADINGS[Math.min(paragraph.heading, HEADINGS.length) - 1] ?? 'h6';
    return (
      <Heading className="preview-docx-heading">
        <Inlines paragraph={paragraph} urls={urls} />
      </Heading>
    );
  }
  return (
    <p className="preview-docx-paragraph">
      <Inlines paragraph={paragraph} urls={urls} />
    </p>
  );
}

/** A list, and the lists in its items. */
function List({ list, urls }: { list: DocxList; urls: (string | null)[] }) {
  const items = list.items.map((item, at) => (
    <li key={at} className={item.paragraph === null ? 'preview-docx-skipped' : undefined}>
      {item.paragraph === null ? null : <Inlines paragraph={item.paragraph} urls={urls} />}
      {item.lists.map((inner, index) => (
        <List key={index} list={inner} urls={urls} />
      ))}
    </li>
  ));
  return list.ordered ? (
    <ol className="preview-docx-list">{items}</ol>
  ) : (
    <ul className="preview-docx-list">{items}</ul>
  );
}

/** Blocks of a document - of its body or of a cell - with their lists gathered. */
function Blocks({ blocks, urls, depth }: { blocks: readonly DocxBlock[]; urls: (string | null)[]; depth: number }) {
  return groupBlocks(blocks).map((group, at) => {
    if (group.type === 'list') {
      return <List key={at} list={group.list} urls={urls} />;
    }
    const { block } = group;
    if (block.type === 'paragraph') {
      return <Paragraph key={at} paragraph={block} urls={urls} />;
    }
    if (depth >= DOCX_MAX_TABLE_DEPTH) {
      return null;
    }
    return (
      <table key={at} className="table table-sm table-bordered preview-docx-table">
        <tbody>
          {block.rows.map((row, r) => (
            <tr key={r}>
              {row.map((cell, c) => (
                <td key={c} colSpan={cell.span > 1 ? cell.span : undefined}>
                  <Blocks blocks={cell.blocks} urls={urls} depth={depth + 1} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    );
  });
}

/**
 * A DOCX as its reader modelled it: headings, paragraphs, lists, tables,
 * emphasis, line breaks and images - every word of it a text node. What is
 * not shown is said above it.
 */
export function DocxPreview({ model, urls, objectKey }: { model: DocxModel; urls: (string | null)[]; objectKey: string }) {
  return (
    <>
      <p className="preview-note" role="note">
        Layout, styles, charts and comments are not shown; nor are headers, footers, footnotes and text boxes.
      </p>
      {model.truncated ? (
        <p className="preview-note" role="note">
          Only the beginning of the document is shown. Download it to see all of it.
        </p>
      ) : null}
      {model.deepTables ? (
        <p className="preview-note" role="note">
          Tables nested more than {String(DOCX_MAX_TABLE_DEPTH)} deep are not shown.
        </p>
      ) : null}
      {model.blocks.length === 0 ? (
        <p>There is nothing in it.</p>
      ) : (
        <div className="preview-docx" tabIndex={0} role="document" aria-label={`Contents of ${objectKey}`}>
          <Blocks blocks={model.blocks} urls={urls} depth={0} />
        </div>
      )}
    </>
  );
}
