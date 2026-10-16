// The packaging of a DOCX or XLSX (Open Packaging Conventions): which part
// is where, found through relationships. Names are resolved inside the zip
// only - nothing is ever fetched, and a relationship to anything outside the
// package is left out.

import { OoxmlError } from './ooxml-error';
import { declaresDtd, readXmlTree, type XmlElement } from './xml';
import type { ZipArchive } from './zip';

/** The namespace of relationships parts. */
const RELATIONSHIPS = 'http://schemas.openxmlformats.org/package/2006/relationships';

/** The relationship to a package's main part, in transitional and strict OOXML. */
const OFFICE_DOCUMENT = [
  'http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument',
  'http://purl.oclc.org/ooxml/officeDocument/relationships/officeDocument',
];

/** The namespaces of relationship ids (`r:id`, `r:embed`) in parts, transitional and strict. */
export const RELATIONSHIP_ID_NAMESPACES = [
  'http://schemas.openxmlformats.org/officeDocument/2006/relationships',
  'http://purl.oclc.org/ooxml/officeDocument/relationships',
];

/** A relationship of a part to another part of the package. */
export interface Relationship {
  /** Its type, a URI; compare with endsWith for the strict and transitional forms. */
  type: string;
  /** The part it leads to, resolved to a name of the zip; null when it leads outside the package. */
  target: string | null;
}

/**
 * The part a target names, relative to the part `source`: a name of the zip,
 * without a leading slash. Null for a target that is not a part of the
 * package - an address with a scheme, one that climbs out of it, one that
 * cannot be decoded.
 */
export function resolvePart(source: string, target: string): string | null {
  if (target === '' || /^[a-z][a-z0-9+.-]*:/i.test(target) || target.startsWith('//')) {
    return null;
  }
  let path: string;
  try {
    path = decodeURIComponent(target.split('#')[0] ?? '');
  } catch {
    return null;
  }
  const base = path.startsWith('/') ? [] : source.split('/').slice(0, -1);
  const segments = [...base];
  for (const segment of path.split('/')) {
    if (segment === '' || segment === '.') {
      continue;
    }
    if (segment === '..') {
      if (segments.length === 0) {
        return null;
      }
      segments.pop();
      continue;
    }
    segments.push(segment);
  }
  return segments.length === 0 ? null : segments.join('/');
}

/** The name of the relationships part of a part: `word/document.xml` → `word/_rels/document.xml.rels`. */
export function relationshipsPartOf(part: string): string {
  const slash = part.lastIndexOf('/');
  return `${part.slice(0, slash + 1)}_rels/${part.slice(slash + 1)}.rels`;
}

/**
 * Reads a part of XML whole. Its text is judged for a DOCTYPE or an entity
 * before any of it is read; null when there is no such part.
 */
export function readPartTree(zip: ZipArchive, part: string): XmlElement | null {
  const text = zip.readText(part);
  if (text === null) {
    return null;
  }
  if (declaresDtd(text)) {
    throw new OoxmlError('unsafe', `a DOCTYPE or an ENTITY in ${part}`);
  }
  return readXmlTree(text);
}

/** The relationships of a part, by id; none when it has no relationships part. */
export function relationshipsOf(zip: ZipArchive, part: string): Map<string, Relationship> {
  const relationships = new Map<string, Relationship>();
  const tree = readPartTree(zip, relationshipsPartOf(part));
  if (tree === null) {
    return relationships;
  }
  if (tree.name !== `{${RELATIONSHIPS}}Relationships`) {
    throw new OoxmlError('damaged', 'a relationships part of another kind');
  }
  for (const child of tree.children) {
    if (typeof child === 'string' || child.name !== `{${RELATIONSHIPS}}Relationship`) {
      continue;
    }
    const id = child.attributes.get('Id');
    const target = child.attributes.get('Target');
    const type = child.attributes.get('Type') ?? '';
    if (id === undefined || target === undefined) {
      continue;
    }
    const external = child.attributes.get('TargetMode') === 'External';
    relationships.set(id, { type, target: external ? null : resolvePart(part, target) });
  }
  return relationships;
}

/** The main part of the package - `word/document.xml`, `xl/workbook.xml` - as its root relationships name it. */
export function mainPartOf(zip: ZipArchive): string {
  for (const relationship of relationshipsOf(zip, '').values()) {
    if (OFFICE_DOCUMENT.includes(relationship.type) && relationship.target !== null) {
      return relationship.target;
    }
  }
  throw new OoxmlError('damaged', 'no main part');
}

/** The value of an attribute named in one of `namespaces`, or undefined. */
export function attributeIn(element: { attributes: ReadonlyMap<string, string> }, namespaces: readonly string[], local: string) {
  for (const namespace of namespaces) {
    const value = element.attributes.get(`{${namespace}}${local}`);
    if (value !== undefined) {
      return value;
    }
  }
  return undefined;
}
