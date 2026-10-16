// A small, strict reader of XML for the parts of a DOCX or XLSX, run in the
// preview's worker (ADR-0028). A dedicated worker has no DOMParser in any
// browser, and the console would not want one: this reader knows no DTD,
// expands no entity but the five of XML and character references, fetches
// nothing and runs nothing. What it does not understand it refuses.

import { OoxmlError } from './ooxml-error';

/**
 * The attributes of an element, by name: `{namespace}local` for a prefixed
 * attribute, the local name alone for one without a prefix. Namespace
 * declarations are not among them.
 */
export type XmlAttributes = ReadonlyMap<string, string>;

/** What a handler answers to stop the reading: nothing after it is read. */
export const STOP = 'stop';

/** What a handler answers: STOP, or nothing to go on. */
export type XmlAnswer = typeof STOP | undefined;

/**
 * What is told of a document as it is read. An element's name is
 * `{namespace}local`, or the local name alone outside any namespace.
 * Answering STOP ends the reading there.
 */
export interface XmlHandler {
  open: (name: string, attributes: XmlAttributes) => XmlAnswer;
  close: (name: string) => XmlAnswer;
  text: (text: string) => XmlAnswer;
}

/** The namespace of `xml:` attributes, which needs no declaration. */
const XML_NAMESPACE = 'http://www.w3.org/XML/1998/namespace';

/**
 * The most namespaces an element may declare. A scope is copied when an
 * element declares one: without this and XML_MAX_PREFIXES a part could
 * make the reading quadratic. Office writes a few dozen.
 */
export const XML_MAX_DECLARATIONS = 64;

/** The most namespaces in scope at once (see XML_MAX_DECLARATIONS). */
export const XML_MAX_PREFIXES = 256;

/** How deep elements may be nested: deeper is no document the console shows. */
export const XML_MAX_DEPTH = 256;

/** A name of XML, as far as the parts of a DOCX or XLSX use them. */
const NAME = /[A-Za-z_À-￿][\w.\-·À-￿]*(?::[A-Za-z_À-￿][\w.\-·À-￿]*)?/y;
const SPACE = /[ \t\r\n]*/y;
const REQUIRED_SPACE = /[ \t\r\n]+/y;

/** A character XML 1.0 does not allow; a lone surrogate is one. */
const FORBIDDEN = /[^\t\n\r\u0020-\uD7FF\uE000-\uFFFD\u{10000}-\u{10FFFF}]/u;

/** The five entities of XML; there are no others. */
const ENTITIES: Readonly<Record<string, string>> = { lt: '<', gt: '>', amp: '&', quot: '"', apos: "'" };

/** Whether `code` is a character XML 1.0 allows. */
function allowed(code: number): boolean {
  return (
    code === 0x9 ||
    code === 0xa ||
    code === 0xd ||
    (code >= 0x20 && code <= 0xd7ff) ||
    (code >= 0xe000 && code <= 0xfffd) ||
    (code >= 0x10000 && code <= 0x10ffff)
  );
}

/** Text or an attribute's value with its references replaced: only the five entities and numbers. */
function unescape(raw: string): string {
  if (!raw.includes('&')) {
    return raw;
  }
  return raw.replace(/&([^;&]*);|&/g, (whole: string, body: string | undefined) => {
    if (body === undefined) {
      throw new OoxmlError('damaged', 'a lone & in XML');
    }
    const named = ENTITIES[body];
    if (named !== undefined && Object.hasOwn(ENTITIES, body)) {
      return named;
    }
    const number = /^#x([0-9a-fA-F]{1,6})$/.exec(body) ?? /^#([0-9]{1,7})$/.exec(body);
    if (number?.[1] === undefined) {
      // An entity of a DTD: there is none, and none is ever read.
      throw new OoxmlError('damaged', `an unknown reference ${whole.slice(0, 16)}`);
    }
    const code = Number.parseInt(number[1], body.startsWith('#x') ? 16 : 10);
    if (!allowed(code)) {
      throw new OoxmlError('damaged', 'a reference to a character XML does not allow');
    }
    return String.fromCodePoint(code);
  });
}

/** The scope of an element: the namespaces its prefixes stand for. */
interface Scope {
  name: string;
  qualified: string;
  prefixes: ReadonlyMap<string, string>;
}

/** Whether a part declares a DTD or an entity, anywhere, in any case: such a part is never read. */
export function declaresDtd(text: string): boolean {
  return /<!\s*(DOCTYPE|ENTITY)/i.test(text);
}

/**
 * Reads `xml` from its start to its end, telling `handler` of every element
 * and every text in it. Comments and processing instructions are passed
 * over; CDATA is text. Refused (an OoxmlError):
 *
 * - `unsafe`: a part with `<!DOCTYPE` or `<!ENTITY` anywhere, before a
 *   character of it is read;
 * - `damaged`: anything else that is not well-formed XML with namespaces -
 *   an element not closed, closed by another name, a prefix not declared, an
 *   attribute twice, a reference to an entity other than the five of XML, a
 *   character XML does not allow, text outside the root - or nested more
 *   than XML_MAX_DEPTH deep, or with more namespaces than
 *   XML_MAX_DECLARATIONS on an element or XML_MAX_PREFIXES in scope.
 */
export function readXml(xml: string, handler: XmlHandler): void {
  if (declaresDtd(xml)) {
    throw new OoxmlError('unsafe', 'a DOCTYPE or an ENTITY');
  }
  const stack: Scope[] = [];
  let prefixes: ReadonlyMap<string, string> = new Map([['xml', XML_NAMESPACE]]);
  let rootSeen = false;
  let at = 0;
  const end = xml.length;

  const fail = (what: string): never => {
    throw new OoxmlError('damaged', `${what} at ${String(at)}`);
  };
  const match = (pattern: RegExp): string | null => {
    pattern.lastIndex = at;
    const found = pattern.exec(xml);
    if (found === null) {
      return null;
    }
    at = pattern.lastIndex;
    return found[0];
  };
  const resolve = (qualified: string, scope: ReadonlyMap<string, string>, isAttribute: boolean): string => {
    const colon = qualified.indexOf(':');
    if (colon === -1) {
      const namespace = isAttribute ? undefined : scope.get('');
      return namespace === undefined || namespace === '' ? qualified : `{${namespace}}${qualified}`;
    }
    const namespace = scope.get(qualified.slice(0, colon));
    if (namespace === undefined || namespace === '') {
      return fail(`an undeclared prefix in ${qualified.slice(0, 64)}`);
    }
    return `{${namespace}}${qualified.slice(colon + 1)}`;
  };
  const checkCharacters = (text: string) => {
    if (FORBIDDEN.test(text)) {
      fail('a character XML does not allow');
    }
  };
  const tell = (answer: XmlAnswer): boolean => answer === STOP;

  while (at < end) {
    const lt = xml.indexOf('<', at);
    if (lt !== at) {
      const raw = xml.slice(at, lt === -1 ? end : lt);
      if (raw.includes(']]>')) {
        fail('"]]>" in text');
      }
      at = lt === -1 ? end : lt;
      if (stack.length === 0) {
        if (raw.trim() !== '') {
          fail('text outside the root element');
        }
        continue;
      }
      checkCharacters(raw);
      if (tell(handler.text(unescape(raw)))) {
        return;
      }
      continue;
    }
    if (xml.startsWith('<?', at)) {
      const close = xml.indexOf('?>', at + 2);
      if (close === -1) {
        fail('a processing instruction not closed');
      }
      at = close + 2;
      continue;
    }
    if (xml.startsWith('<!--', at)) {
      const close = xml.indexOf('-->', at + 4);
      if (close === -1) {
        fail('a comment not closed');
      }
      at = close + 3;
      continue;
    }
    if (xml.startsWith('<![CDATA[', at)) {
      if (stack.length === 0) {
        fail('CDATA outside the root element');
      }
      const close = xml.indexOf(']]>', at + 9);
      if (close === -1) {
        fail('CDATA not closed');
      }
      const text = xml.slice(at + 9, close);
      checkCharacters(text);
      at = close + 3;
      if (tell(handler.text(text))) {
        return;
      }
      continue;
    }
    if (xml.startsWith('<!', at)) {
      // Not a comment or CDATA: a declaration of a DTD, which is never read.
      fail('a declaration');
    }
    if (xml.startsWith('</', at)) {
      at += 2;
      const qualified = match(NAME);
      match(SPACE);
      if (qualified === null || xml[at] !== '>') {
        fail('a malformed end tag');
      }
      at += 1;
      const scope = stack.pop();
      if (scope?.qualified !== qualified) {
        fail(`an end tag ${String(qualified).slice(0, 64)} that closes nothing open`);
      }
      prefixes = stack.at(-1)?.prefixes ?? new Map([['xml', XML_NAMESPACE]]);
      if (tell(handler.close(scope?.name ?? ''))) {
        return;
      }
      continue;
    }
    // A start tag.
    at += 1;
    if (stack.length === 0 && rootSeen) {
      fail('a second root element');
    }
    const qualified = match(NAME);
    if (qualified === null) {
      fail('a malformed start tag');
    }
    const raw: [string, string][] = [];
    let empty = false;
    for (;;) {
      const spaced = match(REQUIRED_SPACE) !== null;
      if (xml[at] === '>') {
        at += 1;
        break;
      }
      if (xml.startsWith('/>', at)) {
        at += 2;
        empty = true;
        break;
      }
      const name = spaced ? match(NAME) : null;
      if (name === null) {
        fail('a malformed attribute');
      }
      match(SPACE);
      if (xml[at] !== '=') {
        fail('an attribute without a value');
      }
      at += 1;
      match(SPACE);
      const quote = xml[at];
      if (quote !== '"' && quote !== "'") {
        fail('an attribute value without quotes');
      }
      const close = xml.indexOf(quote ?? '"', at + 1);
      if (close === -1) {
        fail('an attribute value not closed');
      }
      const value = xml.slice(at + 1, close);
      if (value.includes('<')) {
        fail('"<" in an attribute value');
      }
      checkCharacters(value);
      at = close + 1;
      raw.push([name ?? '', unescape(value)]);
    }
    // The namespaces this element declares, then its names in them.
    let declared: Map<string, string> | null = null;
    let declarations = 0;
    const seen = new Set<string>();
    for (const [name, value] of raw) {
      if (seen.has(name)) {
        fail(`the attribute ${name.slice(0, 64)} twice`);
      }
      seen.add(name);
      if (name === 'xmlns' || name.startsWith('xmlns:')) {
        declarations += 1;
        if (declarations > XML_MAX_DECLARATIONS) {
          fail('too many namespace declarations on one element');
        }
        declared ??= new Map(prefixes);
        declared.set(name === 'xmlns' ? '' : name.slice(6), value);
        if (declared.size > XML_MAX_PREFIXES) {
          fail('too many namespaces in scope');
        }
      }
    }
    const scope = declared ?? prefixes;
    const attributes = new Map<string, string>();
    for (const [name, value] of raw) {
      if (name === 'xmlns' || name.startsWith('xmlns:')) {
        continue;
      }
      const resolved = resolve(name, scope, true);
      if (attributes.has(resolved)) {
        fail(`the attribute ${name.slice(0, 64)} twice`);
      }
      attributes.set(resolved, value);
    }
    const name = resolve(qualified ?? '', scope, false);
    rootSeen = true;
    if (empty) {
      if (tell(handler.open(name, attributes)) || tell(handler.close(name))) {
        return;
      }
      continue;
    }
    if (stack.length >= XML_MAX_DEPTH) {
      fail('elements nested too deep');
    }
    stack.push({ name, qualified: qualified ?? '', prefixes: scope });
    prefixes = scope;
    if (tell(handler.open(name, attributes))) {
      return;
    }
  }
  if (stack.length > 0) {
    fail('an element not closed');
  }
  if (!rootSeen) {
    fail('no root element');
  }
}

/** An element of a small part, read whole: its name, attributes and what is in it. */
export interface XmlElement {
  name: string;
  attributes: XmlAttributes;
  children: (XmlElement | string)[];
}

/** The most elements a part read whole may have: a part with more is too large. */
export const XML_TREE_MAX_ELEMENTS = 200_000;

/**
 * Reads a small part - relationships, styles, numbering, a workbook - whole,
 * as a tree. More than `maxElements` elements is an OoxmlError `too-large`.
 */
export function readXmlTree(xml: string, maxElements: number = XML_TREE_MAX_ELEMENTS): XmlElement {
  const root: XmlElement = { name: '', attributes: new Map(), children: [] };
  const open: XmlElement[] = [root];
  let count = 0;
  readXml(xml, {
    open(name, attributes) {
      count += 1;
      if (count > maxElements) {
        throw new OoxmlError('too-large', 'a part with too many elements');
      }
      const element: XmlElement = { name, attributes, children: [] };
      open.at(-1)?.children.push(element);
      open.push(element);
    },
    close() {
      open.pop();
    },
    text(text) {
      open.at(-1)?.children.push(text);
    },
  });
  const first = root.children[0];
  if (first === undefined || typeof first === 'string') {
    throw new OoxmlError('damaged', 'no root element');
  }
  return first;
}

/** The elements directly in `element` with this name. */
export function childrenNamed(element: XmlElement, name: string): XmlElement[] {
  return element.children.filter((child): child is XmlElement => typeof child !== 'string' && child.name === name);
}

/** The first element directly in `element` with this name, or null. */
export function childNamed(element: XmlElement, name: string): XmlElement | null {
  return childrenNamed(element, name)[0] ?? null;
}

/** The text of an element and of everything in it. */
export function textOf(element: XmlElement): string {
  return element.children.map((child) => (typeof child === 'string' ? child : textOf(child))).join('');
}
