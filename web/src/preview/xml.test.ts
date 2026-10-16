import { describe, expect, it } from 'vitest';

import { seeded } from '../test/ooxml';
import { OoxmlError } from './ooxml-error';
import {
  readXml,
  readXmlTree,
  STOP,
  textOf,
  XML_MAX_DECLARATIONS,
  XML_MAX_DEPTH,
  XML_MAX_PREFIXES,
  type XmlElement,
} from './xml';

/** Everything the reader tells of a document, as lines. */
function events(xml: string): string[] {
  const told: string[] = [];
  readXml(xml, {
    open(name, attributes) {
      const list = Array.from(attributes, ([key, value]) => `${key}=${value}`).join(' ');
      told.push(list === '' ? `<${name}>` : `<${name} ${list}>`);
      return undefined;
    },
    close(name) {
      told.push(`</${name}>`);
      return undefined;
    },
    text(text) {
      told.push(text);
      return undefined;
    },
  });
  return told;
}

/** The problem the reader refuses a document with; null when it reads it. */
function refusal(xml: string): string | null {
  try {
    events(xml);
    return null;
  } catch (error: unknown) {
    if (error instanceof OoxmlError) {
      return error.problem;
    }
    throw error;
  }
}

describe('the XML reader of the preview worker', () => {
  it('tells elements in their namespaces, attributes and text', () => {
    expect(
      events(
        '<?xml version="1.0" encoding="UTF-8"?>\n<!-- made by hand --><w:doc xmlns:w="urn:w" xmlns="urn:d" w:a="1" b=\'2\'><x>t</x><w:y/></w:doc>',
      ),
    ).toEqual(['<{urn:w}doc {urn:w}a=1 b=2>', '<{urn:d}x>', 't', '</{urn:d}x>', '<{urn:w}y>', '</{urn:w}y>', '</{urn:w}doc>']);
  });

  it('knows the prefix xml without a declaration, and a prefix declared again inside', () => {
    expect(events('<a xmlns:p="urn:1"><p:b xml:space="preserve"/><c xmlns:p="urn:2"><p:d/></c></a>')).toEqual([
      '<a>',
      '<{urn:1}b {http://www.w3.org/XML/1998/namespace}space=preserve>',
      '</{urn:1}b>',
      '<c>',
      '<{urn:2}d>',
      '</{urn:2}d>',
      '</c>',
      '</a>',
    ]);
  });

  it('reads the five entities and character references, and CDATA as it is', () => {
    expect(events('<a b="&lt;&#x41;&#66;&quot;">&amp;&apos;&gt;&#x1F335;<![CDATA[<b>&amp;</b>]]></a>')).toEqual([
      '<a b=<AB">',
      "&'>\u{1F335}",
      '<b>&amp;</b>',
      '</a>',
    ]);
  });

  it('stops where a handler says so', () => {
    const seen: string[] = [];
    readXml('<a><b/><c/></a>', {
      open(name) {
        seen.push(name);
        return name === 'b' ? STOP : undefined;
      },
      close: () => undefined,
      text: () => undefined,
    });
    expect(seen).toEqual(['a', 'b']);
  });

  it.each([
    ['a DOCTYPE', '<!DOCTYPE a [<!ENTITY x "y">]><a>&x;</a>'],
    ['a DOCTYPE in lower case', '<!doctype a><a/>'],
    ['an ENTITY alone', '<a><!ENTITY x SYSTEM "file:///etc/passwd"></a>'],
    ['billion laughs', '<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;">]><lolz>&lol2;</lolz>'],
    ['a DOCTYPE with space', '<! DOCTYPE a><a/>'],
  ])('refuses %s as unsafe before reading a character', (_, xml) => {
    let told = 0;
    expect(() => {
      readXml(xml, {
        open: () => {
          told += 1;
          return undefined;
        },
        close: () => undefined,
        text: () => undefined,
      });
    }).toThrow(OoxmlError);
    expect(refusal(xml)).toBe('unsafe');
    expect(told).toBe(0);
  });

  it.each([
    ['an entity of a DTD', '<a>&x;</a>'],
    ['a lone ampersand', '<a>fish & chips</a>'],
    ['a reference to NUL', '<a>&#0;</a>'],
    ['a reference past Unicode', '<a>&#x110000;</a>'],
    ['another declaration', '<a><!ATTLIST a b CDATA "c"></a>'],
    ['an element not closed', '<a><b></a>'],
    ['an end tag of another name', '<a></b>'],
    ['an end tag with nothing open', '</a>'],
    ['a prefix not declared', '<p:a/>'],
    ['an attribute twice', '<a b="1" b="2"/>'],
    ['an attribute twice by namespace', '<a xmlns:p="urn:x" xmlns:q="urn:x" p:b="1" q:b="2"/>'],
    ['an attribute without quotes', '<a b=1/>'],
    ['an attribute with "<"', '<a b="<"/>'],
    ['attributes without space between them', '<a b="1"c="2"/>'],
    ['two roots', '<a/><b/>'],
    ['text outside the root', 'x<a/>'],
    ['no root', '<?xml version="1.0"?>'],
    ['a control character', '<a>\u0001</a>'],
    ['a lone surrogate', '<a>\uD800</a>'],
    ['"]]>" in text', '<a>]]></a>'],
    ['a comment not closed', '<a><!-- </a>'],
    ['CDATA not closed', '<a><![CDATA[ </a>'],
    ['a processing instruction not closed', '<a><?pi </a>'],
    ['a start tag not closed', '<a'],
  ])('refuses %s as damaged', (_, xml) => {
    expect(refusal(xml)).toBe('damaged');
  });

  it(`refuses elements nested deeper than ${String(XML_MAX_DEPTH)}`, () => {
    const deep = (depth: number) => `${'<a>'.repeat(depth)}${'</a>'.repeat(depth)}`;
    expect(refusal(deep(XML_MAX_DEPTH))).toBeNull();
    expect(refusal(deep(XML_MAX_DEPTH + 1))).toBe('damaged');
  });

  it(`refuses more than ${String(XML_MAX_DECLARATIONS)} namespaces on an element, or ${String(XML_MAX_PREFIXES)} in scope, and is quick either way`, () => {
    const declare = (from: number, count: number) =>
      Array.from({ length: count }, (_, i) => ` xmlns:p${String(from + i)}="urn:${String(from + i)}"`).join('');
    const started = performance.now();
    expect(refusal(`<a${declare(0, 20_000)}/>`)).toBe('damaged');
    expect(refusal(`<a${declare(0, XML_MAX_DECLARATIONS)}/>`)).toBeNull();
    // Nested, each level declaring its most: the scope fills, and is refused.
    const levels = Math.ceil(XML_MAX_PREFIXES / XML_MAX_DECLARATIONS) + 1;
    const nested = Array.from({ length: levels }, (_, level) => `<a${declare(level * XML_MAX_DECLARATIONS, XML_MAX_DECLARATIONS)}>`).join('');
    expect(refusal(`${nested}${'</a>'.repeat(levels)}`)).toBe('damaged');
    // 20 000 elements, each declaring a namespace beside the root's 64: read, and soon.
    const many = `<r${declare(0, XML_MAX_DECLARATIONS)}>${Array.from({ length: 20_000 }, (_, i) => `<b xmlns:q="urn:${String(i)}"/>`).join('')}</r>`;
    expect(refusal(many)).toBeNull();
    expect(performance.now() - started).toBeLessThan(2000);
  });

  it('reads a small part whole as a tree, and refuses one with too many elements', () => {
    const tree = readXmlTree('<a x="1"><b>one</b>two<c><b>three</b></c></a>');
    expect(tree.name).toBe('a');
    expect(tree.attributes.get('x')).toBe('1');
    expect(textOf(tree)).toBe('onetwothree');
    expect(() => readXmlTree(`<a>${'<b/>'.repeat(10)}</a>`, 10)).toThrow(OoxmlError);
    expect(readXmlTree(`<a>${'<b/>'.repeat(9)}</a>`, 10).children).toHaveLength(9);
  });

  it('keeps markup in text as text', () => {
    expect(events('<a>&lt;script&gt;alert(1)&lt;/script&gt;</a>')).toEqual(['<a>', '<script>alert(1)</script>', '</a>']);
  });
});

/** Escapes text for XML. */
function escape(text: string): string {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/** A random tree of elements, attributes and text, and its XML. */
function randomTree(random: () => number, depth: number): { element: XmlElement; xml: string } {
  const pick = <T,>(items: readonly T[]): T => items[Math.floor(random() * items.length)] as T;
  const words = ['a', 'b', 'x1', 'é', 'ü-ß', '<&>"', ' ', '\u{1F335}', 'text with space', ']]', "'"];
  const name = pick(['a', 'b', 'c', 'row', 'w.t', 'x_y']);
  const attributes = new Map<string, string>();
  let xml = `<${name}`;
  for (let i = Math.floor(random() * 3); i > 0; i -= 1) {
    const key = pick(['k', 'v', 'id', 'r']);
    if (!attributes.has(key)) {
      const value = pick(words) + pick(words);
      attributes.set(key, value);
      xml += ` ${key}="${escape(value)}"`;
    }
  }
  xml += '>';
  const children: (XmlElement | string)[] = [];
  for (let i = depth > 0 ? Math.floor(random() * 4) : 0; i > 0; i -= 1) {
    if (random() < 0.5) {
      const text = pick(words);
      const last = children.at(-1);
      if (typeof last === 'string') {
        children[children.length - 1] = last + text;
      } else {
        children.push(text);
      }
      xml += escape(text);
    } else {
      const child = randomTree(random, depth - 1);
      children.push(child.element);
      xml += child.xml;
    }
  }
  xml += `</${name}>`;
  return { element: { name, attributes, children }, xml };
}

describe('the XML reader, by property and fuzz', () => {
  it('reads back every random tree it is given as XML', () => {
    const random = seeded(0x2e6b4e);
    for (let round = 0; round < 500; round += 1) {
      const { element, xml } = randomTree(random, 4);
      expect(readXmlTree(xml)).toEqual(element);
    }
  });

  it('either reads a damaged document or refuses it with an OoxmlError - never anything else, never for ever', () => {
    const random = seeded(0xd9c39a);
    const pieces = ['<', '>', '/', '&', ';', '"', "'", '=', '!', '?', '[', ']', '-', ':', 'a', 'xmlns', '#x', ' ', '\u0000', '\uD800'];
    for (let round = 0; round < 2000; round += 1) {
      const { xml } = randomTree(random, 3);
      const chars = Array.from(xml);
      for (let edits = 1 + Math.floor(random() * 4); edits > 0; edits -= 1) {
        const at = Math.floor(random() * (chars.length + 1));
        const choice = random();
        if (choice < 0.4) {
          chars.splice(at, 1);
        } else if (choice < 0.8) {
          chars.splice(at, 0, pieces[Math.floor(random() * pieces.length)] ?? '');
        } else {
          chars.splice(at, chars.length);
        }
      }
      const mutated = chars.join('');
      try {
        readXml(mutated, { open: () => undefined, close: () => undefined, text: () => undefined });
      } catch (error: unknown) {
        expect(error).toBeInstanceOf(OoxmlError);
      }
    }
  });
});
