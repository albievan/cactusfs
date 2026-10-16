import { strToU8, zipSync } from 'fflate';
import { describe, expect, it } from 'vitest';

import { seeded, zipOf } from '../test/ooxml';
import { OoxmlError } from './ooxml-error';
import { OOXML_BOUNDS } from './ooxml-model';
import { ZipArchive, type ZipBounds } from './zip';

/** The problem a zip is refused with, reading `part` (or only its directory); null when it is read. */
function refusal(bytes: Uint8Array, part: string | null, bounds?: ZipBounds): string | null {
  try {
    const zip = new ZipArchive(bytes, bounds);
    if (part !== null) {
      zip.read(part);
    }
    return null;
  } catch (error: unknown) {
    if (error instanceof OoxmlError) {
      return error.problem;
    }
    throw error;
  }
}

/** Where each central directory entry of a zip begins. */
function directoryEntries(bytes: Uint8Array): number[] {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const at: number[] = [];
  for (let i = 0; i + 4 <= bytes.length; i += 1) {
    if (view.getUint32(i, true) === 0x02014b50) {
      at.push(i);
    }
  }
  return at;
}

/** The zip with what its entries say they unpack to set to `size` - a lie. */
function withDeclaredSize(bytes: Uint8Array, size: number): Uint8Array {
  const copy = bytes.slice();
  const view = new DataView(copy.buffer);
  for (const at of directoryEntries(copy)) {
    view.setUint32(at + 24, size, true);
  }
  return copy;
}

/** Small bounds, for zips of a few kilobytes. */
const SMALL: ZipBounds = { totalBytes: 4096, partBytes: 2048, ratio: 100, entries: 10 };

describe('the zip of a document', () => {
  it('has the bounds of the plan', () => {
    expect(OOXML_BOUNDS).toEqual({ totalBytes: 64 * 1024 * 1024, partBytes: 32 * 1024 * 1024, ratio: 100, entries: 10_000 });
  });

  it('reads stored and deflated parts, by any case of their names, and counts what it inflates', () => {
    const text = 'a part of a document '.repeat(20);
    const zip = new ZipArchive(zipOf({ 'word/Document.xml': text, 'stored.bin': new Uint8Array([1, 2, 3]) }, 6));
    expect(zip.names().sort()).toEqual(['stored.bin', 'word/Document.xml']);
    expect(zip.has('WORD/document.XML')).toBe(true);
    expect(zip.readText('word/document.xml')).toBe(text);
    expect(zip.inflated).toBe(text.length);
    expect(Array.from(zip.read('stored.bin') ?? [])).toEqual([1, 2, 3]);
    expect(zip.inflated).toBe(text.length + 3);
    expect(zip.read('missing')).toBeNull();
    const stored = new ZipArchive(zipOf({ 'a.xml': text }, 0));
    expect(stored.readText('a.xml')).toBe(text);
  });

  it('stops a zip bomb by its ratio, as it says - with the bounds of the plan', () => {
    // 4 MiB of zeros deflate to about 4 KiB: a ratio of about 1 000.
    const bomb = zipOf({ 'word/document.xml': new Uint8Array(4 * 1024 * 1024) });
    expect(refusal(bomb, null)).toBeNull();
    expect(refusal(bomb, 'word/document.xml')).toBe('too-large');
  });

  it('stops a zip bomb by its ratio as it unpacks, whatever its entry says', () => {
    const bomb = withDeclaredSize(zipOf({ 'word/document.xml': new Uint8Array(4 * 1024 * 1024) }), 10);
    const zip = new ZipArchive(bomb);
    expect(() => zip.read('word/document.xml')).toThrow(/ratio/);
    // What was inflated before it stopped is bounded by a few pushes.
    expect(zip.inflated).toBeLessThan(16 * 1024 * 1024);
  });

  it('stops a part larger than a part may be, as it says and as it unpacks', () => {
    const random = seeded(7);
    // Text that deflates by less than 100:1.
    const words = Array.from({ length: 600 }, () => String(Math.floor(random() * 1e6))).join(' ');
    expect(words.length).toBeGreaterThan(SMALL.partBytes);
    const zip = zipOf({ 'a.xml': words });
    expect(refusal(zip, 'a.xml', SMALL)).toBe('too-large');
    expect(refusal(withDeclaredSize(zip, 100), 'a.xml', SMALL)).toBe('too-large');
    expect(refusal(zip, 'a.xml', { ...SMALL, partBytes: words.length, totalBytes: words.length })).toBeNull();
  });

  it('stops a stored part of more than 32 MiB with the bounds of the plan, before it is read', () => {
    const zip = zipOf({ 'big.bin': new Uint8Array(OOXML_BOUNDS.partBytes + 1) }, 0);
    expect(refusal(zip, 'big.bin')).toBe('too-large');
  });

  it('stops when what it inflated in all passes the bound', () => {
    const random = seeded(11);
    const part = () => Array.from({ length: 250 }, () => String(Math.floor(random() * 1e6))).join(' ');
    const archive = new ZipArchive(zipOf({ 'a.xml': part(), 'b.xml': part(), 'c.xml': part() }), SMALL);
    archive.read('a.xml');
    expect(() => archive.read('b.xml')).not.toThrow();
    expect(() => archive.read('c.xml')).toThrow(OoxmlError);
  });

  it('refuses a zip of more entries than the bound before reading any - with the bounds of the plan', () => {
    const many: Record<string, string> = {};
    for (let i = 0; i <= OOXML_BOUNDS.entries; i += 1) {
      many[`e${String(i)}`] = '';
    }
    expect(refusal(zipOf(many, 0), null)).toBe('too-large');
    delete many.e0;
    expect(refusal(zipOf(many, 0), null)).toBeNull();
  });

  it('refuses a part whose deflated data ends early, or that unpacks to another size than it says', () => {
    const zip = zipOf({ 'a.xml': 'some text, some text, some text' });
    expect(refusal(withDeclaredSize(zip, 5), 'a.xml')).toBe('damaged');
    expect(refusal(withDeclaredSize(zip, 500), 'a.xml')).toBe('damaged');
  });

  it.each([
    ['nothing', new Uint8Array()],
    ['no zip', strToU8('<html>not a zip</html>')],
    ['a zip cut short', zipOf({ 'a.xml': 'text' }).slice(0, 40)],
  ])('refuses %s as damaged', (_, bytes) => {
    expect(refusal(bytes, 'a.xml')).toBe('damaged');
  });

  it('refuses an encrypted entry, a method it does not read, zip64 and names twice', () => {
    const zip = zipOf({ 'a.xml': 'text' }, 0);
    const [entry] = directoryEntries(zip);
    const encrypted = zip.slice();
    new DataView(encrypted.buffer).setUint16((entry ?? 0) + 8, 1, true);
    expect(refusal(encrypted, 'a.xml')).toBe('damaged');
    const bzip2 = zip.slice();
    new DataView(bzip2.buffer).setUint16((entry ?? 0) + 10, 12, true);
    expect(refusal(bzip2, 'a.xml')).toBe('damaged');
    const zip64 = zip.slice();
    new DataView(zip64.buffer).setUint32((entry ?? 0) + 20, 0xffffffff, true);
    expect(refusal(zip64, null)).toBe('damaged');
    const twice = zipSync({ 'A.xml': strToU8('one'), 'a.xml': strToU8('two') });
    expect(refusal(twice, null)).toBe('damaged');
  });

  it('refuses a local header that is not its entry\'s: another name, method or encryption', () => {
    const zip = zipOf({ 'a.xml': 'text text text' });
    const view = (bytes: Uint8Array) => new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const renamed = zip.slice();
    renamed[30] = 'b'.charCodeAt(0);
    expect(refusal(renamed, null)).toBe('damaged');
    const method = zip.slice();
    view(method).setUint16(8, 0, true);
    expect(refusal(method, null)).toBe('damaged');
    const encrypted = zip.slice();
    view(encrypted).setUint16(6, 1, true);
    expect(refusal(encrypted, null)).toBe('damaged');
    expect(refusal(zip, 'a.xml')).toBeNull();
  });

  it('refuses entries that overlap, and data that runs into the central directory', () => {
    const zip = zipOf({ 'a.xml': 'first part', 'b.xml': 'second part' }, 0);
    const [first, second] = directoryEntries(zip);
    const overlapping = zip.slice();
    // The first entry said to be longer: its data runs over the second's header.
    new DataView(overlapping.buffer).setUint32((first ?? 0) + 20, 12, true);
    new DataView(overlapping.buffer).setUint32((first ?? 0) + 24, 12, true);
    expect(refusal(overlapping, null)).toBe('damaged');
    const into = zip.slice();
    new DataView(into.buffer).setUint32((second ?? 0) + 20, 200, true);
    expect(refusal(into, null)).toBe('damaged');
    // Two entries of one local header.
    const shared = zipOf({ 'a.xml': 'x', 'a.xmm': 'y' }, 0);
    const [, other] = directoryEntries(shared);
    new DataView(shared.buffer).setUint32((other ?? 0) + 42, 0, true);
    expect(refusal(shared, null)).toBe('damaged');
  });

  it('reads the end of the directory whose comment reaches the end of the zip, and no other', () => {
    const zip = zipOf({ 'a.xml': 'text' });
    const commented = new Uint8Array(zip.length + 5);
    commented.set(zip);
    commented.set(strToU8('hello'), zip.length);
    new DataView(commented.buffer).setUint16(zip.length - 2, 5, true);
    expect(refusal(commented, 'a.xml')).toBeNull();
    // Bytes after the record that no comment accounts for.
    const trailing = new Uint8Array(zip.length + 3);
    trailing.set(zip);
    expect(refusal(trailing, 'a.xml')).toBe('damaged');
  });

  it('either reads a damaged zip or refuses it with an OoxmlError - never anything else', () => {
    const random = seeded(0x163b2c);
    const original = zipOf({ 'word/document.xml': '<w:document>text</w:document>'.repeat(30), 'b.bin': new Uint8Array(64) });
    for (let round = 0; round < 1500; round += 1) {
      const bytes = original.slice();
      for (let edits = 1 + Math.floor(random() * 6); edits > 0; edits -= 1) {
        bytes[Math.floor(random() * bytes.length)] = Math.floor(random() * 256);
      }
      const cut = random() < 0.2 ? bytes.slice(0, Math.floor(random() * bytes.length)) : bytes;
      try {
        const zip = new ZipArchive(cut);
        for (const name of zip.names()) {
          zip.read(name);
        }
      } catch (error: unknown) {
        expect(error).toBeInstanceOf(OoxmlError);
      }
    }
  });
});
