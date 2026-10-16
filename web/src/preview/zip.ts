// The zip of a DOCX or XLSX, read in the preview's worker (ADR-0028): its
// central directory by this file, a part's deflated bytes by fflate's
// streaming Inflate, fed a little at a time and counted as they come, so
// that a zip bomb is stopped by what it unpacks, not by what it says.

import { Inflate } from 'fflate';

import { OoxmlError } from './ooxml-error';
import { OOXML_BOUNDS } from './ooxml-model';

/** What the reader takes of one document; OOXML_BOUNDS unless a test says otherwise. */
export interface ZipBounds {
  totalBytes: number;
  partBytes: number;
  ratio: number;
  entries: number;
}

/** An entry of the central directory, as far as it is read. */
interface ZipEntry {
  name: string;
  flags: number;
  method: number;
  compressedSize: number;
  size: number;
  localOffset: number;
  /** Where its compressed bytes begin, after its local header. */
  dataStart: number;
}

/**
 * How many compressed bytes are given to Inflate at once. What one push can
 * unpack is bounded by it - deflate reaches about 1 032:1, so 8 MiB at most -
 * and the bounds are judged after every push.
 */
const PIECE = 8 * 1024;

const END_OF_DIRECTORY = 0x06054b50;
const DIRECTORY_ENTRY = 0x02014b50;
const LOCAL_HEADER = 0x04034b50;
const ZIP64_LOCATOR = 0x07064b50;

/** The marker of a zip that keeps its sizes elsewhere (zip64): not read. */
const ZIP64_MARK = 0xffffffff;

/**
 * A zip read from its central directory. Parts are inflated when asked for,
 * and every byte inflated is counted against the bounds: past `partBytes`
 * in a part, past `totalBytes` in all, or past `ratio` times a part's
 * compressed size, the reading stops with an OoxmlError `too-large` - a
 * count of entries past `entries` before any is read. Encrypted entries,
 * zip64, methods other than stored and deflate, and names that are the
 * same but for case (part names of OOXML are not case-sensitive) are
 * `damaged`.
 */
export class ZipArchive {
  readonly #bytes: Uint8Array;
  readonly #view: DataView;
  readonly #bounds: ZipBounds;
  readonly #entries = new Map<string, ZipEntry>();
  #inflated = 0;

  constructor(bytes: Uint8Array, bounds: ZipBounds = OOXML_BOUNDS) {
    this.#bytes = bytes;
    this.#view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    this.#bounds = bounds;
    this.#readDirectory();
  }

  /** How many bytes have been inflated so far. */
  get inflated(): number {
    return this.#inflated;
  }

  /** The names of the entries, as stored. */
  names(): string[] {
    return Array.from(this.#entries.values(), (entry) => entry.name);
  }

  /** Whether the zip has a part of this name (in any case). */
  has(name: string): boolean {
    return this.#entries.has(name.toLowerCase());
  }

  /** The bytes of a part, inflated and counted; null when there is none of this name. */
  read(name: string): Uint8Array | null {
    const entry = this.#entries.get(name.toLowerCase());
    if (entry === undefined) {
      return null;
    }
    if ((entry.flags & 1) !== 0) {
      throw new OoxmlError('damaged', 'an encrypted entry');
    }
    const data = this.#dataOf(entry);
    if (entry.method === 0) {
      if (entry.size !== entry.compressedSize) {
        throw new OoxmlError('damaged', 'a stored entry whose sizes differ');
      }
      this.#count(entry, entry.size, this.#inflated);
      this.#inflated += entry.size;
      return data;
    }
    if (entry.method !== 8) {
      throw new OoxmlError('damaged', `an entry compressed by method ${String(entry.method)}`);
    }
    // What the entry says it unpacks to is judged before a byte is
    // inflated, and what it does unpack to as it comes.
    this.#count(entry, entry.size, this.#inflated);
    return this.#inflate(entry, data);
  }

  /** Reads a part as UTF-8 text; null when there is none of this name. */
  readText(name: string): string | null {
    const bytes = this.read(name);
    if (bytes === null) {
      return null;
    }
    try {
      return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
    } catch {
      throw new OoxmlError('damaged', 'a part that is not UTF-8');
    }
  }

  /**
   * Refuses `produced` bytes of `entry` if they pass a bound, `base` bytes
   * having been inflated before the entry.
   */
  #count(entry: ZipEntry, produced: number, base: number) {
    const { partBytes, totalBytes, ratio } = this.#bounds;
    if (produced > partBytes) {
      throw new OoxmlError('too-large', 'a part larger than the bound');
    }
    if (produced > ratio * Math.max(entry.compressedSize, 1) && entry.method !== 0) {
      throw new OoxmlError('too-large', 'a part compressed past the ratio');
    }
    if (base + produced > totalBytes) {
      throw new OoxmlError('too-large', 'more than the bound in all');
    }
  }

  #inflate(entry: ZipEntry, data: Uint8Array): Uint8Array {
    const chunks: Uint8Array[] = [];
    let produced = 0;
    const start = this.#inflated;
    const inflate = new Inflate((chunk) => {
      produced += chunk.length;
      this.#inflated = start + produced;
      this.#count(entry, produced, start);
      if (produced > entry.size) {
        throw new OoxmlError('damaged', 'a part larger than its entry says');
      }
      chunks.push(chunk);
    });
    try {
      if (data.length === 0) {
        inflate.push(data, true);
      }
      for (let at = 0; at < data.length; at += PIECE) {
        inflate.push(data.subarray(at, at + PIECE), at + PIECE >= data.length);
      }
    } catch (error: unknown) {
      if (error instanceof OoxmlError) {
        throw error;
      }
      throw new OoxmlError('damaged', 'a part whose deflated data is broken');
    }
    if (produced !== entry.size) {
      throw new OoxmlError('damaged', 'a part whose size is not what its entry says');
    }
    const out = new Uint8Array(produced);
    let at = 0;
    for (const chunk of chunks) {
      out.set(chunk, at);
      at += chunk.length;
    }
    return out;
  }

  /** The compressed bytes of an entry, after its local header. */
  #dataOf(entry: ZipEntry): Uint8Array {
    return this.#bytes.subarray(entry.dataStart, entry.dataStart + entry.compressedSize);
  }

  /**
   * Where the compressed bytes of an entry begin: after its local header,
   * which must agree with the central directory on its name, its method and
   * whether it is encrypted, and the bytes must end before the directory.
   */
  #dataStartOf(at: number, entry: Omit<ZipEntry, 'dataStart'>, nameBytes: Uint8Array, directory: number): number {
    const view = this.#view;
    if (at + 30 > directory || view.getUint32(at, true) !== LOCAL_HEADER) {
      throw new OoxmlError('damaged', 'an entry without its local header');
    }
    const nameLength = view.getUint16(at + 26, true);
    const start = at + 30 + nameLength + view.getUint16(at + 28, true);
    const local = this.#bytes.subarray(at + 30, at + 30 + nameLength);
    if (
      view.getUint16(at + 8, true) !== entry.method ||
      (view.getUint16(at + 6, true) & 1) !== (entry.flags & 1) ||
      local.length !== nameBytes.length ||
      local.some((byte, index) => byte !== nameBytes[index])
    ) {
      throw new OoxmlError('damaged', 'a local header that is not its entry\'s');
    }
    if (start + entry.compressedSize > directory) {
      throw new OoxmlError('damaged', 'an entry that runs into the central directory');
    }
    return start;
  }

  #readDirectory() {
    const bytes = this.#bytes;
    const view = this.#view;
    // The end of the central directory: its last 22 bytes, and a comment
    // of at most 65 535 after them.
    // The record whose comment reaches exactly to the end of the bytes: a
    // signature inside a comment is no record.
    let end = -1;
    for (let at = bytes.length - 22; at >= 0 && at >= bytes.length - 22 - 0xffff; at -= 1) {
      if (view.getUint32(at, true) === END_OF_DIRECTORY && at + 22 + view.getUint16(at + 20, true) === bytes.length) {
        end = at;
        break;
      }
    }
    if (end === -1) {
      throw new OoxmlError('damaged', 'no end of a central directory');
    }
    if (end >= 20 && view.getUint32(end - 20, true) === ZIP64_LOCATOR) {
      throw new OoxmlError('damaged', 'a zip64 archive');
    }
    const count = view.getUint16(end + 10, true);
    const size = view.getUint32(end + 12, true);
    const offset = view.getUint32(end + 16, true);
    if (count === 0xffff || offset === ZIP64_MARK || size === ZIP64_MARK) {
      throw new OoxmlError('damaged', 'a zip64 archive');
    }
    if (count > this.#bounds.entries) {
      throw new OoxmlError('too-large', 'more entries than the bound');
    }
    if (offset + size > end) {
      throw new OoxmlError('damaged', 'a central directory past its end');
    }
    const decoder = new TextDecoder('utf-8', { fatal: false });
    let at = offset;
    for (let index = 0; index < count; index += 1) {
      if (at + 46 > end || view.getUint32(at, true) !== DIRECTORY_ENTRY) {
        throw new OoxmlError('damaged', 'a central directory entry missing');
      }
      const nameLength = view.getUint16(at + 28, true);
      const next = at + 46 + nameLength + view.getUint16(at + 30, true) + view.getUint16(at + 32, true);
      if (next > end) {
        throw new OoxmlError('damaged', 'a central directory entry past its end');
      }
      const nameBytes = bytes.subarray(at + 46, at + 46 + nameLength);
      const listed = {
        name: decoder.decode(nameBytes),
        flags: view.getUint16(at + 8, true),
        method: view.getUint16(at + 10, true),
        compressedSize: view.getUint32(at + 20, true),
        size: view.getUint32(at + 24, true),
        localOffset: view.getUint32(at + 42, true),
      };
      if (listed.compressedSize === ZIP64_MARK || listed.size === ZIP64_MARK || listed.localOffset === ZIP64_MARK) {
        throw new OoxmlError('damaged', 'a zip64 entry');
      }
      const entry: ZipEntry = { ...listed, dataStart: this.#dataStartOf(listed.localOffset, listed, nameBytes, offset) };
      const key = entry.name.toLowerCase();
      if (this.#entries.has(key)) {
        throw new OoxmlError('damaged', 'two entries of the same name');
      }
      this.#entries.set(key, entry);
      at = next;
    }
    // No two entries share a byte: each, header and data, ends before the next begins.
    const spans = Array.from(this.#entries.values(), (entry) => [entry.localOffset, entry.dataStart + entry.compressedSize] as const);
    spans.sort((a, b) => a[0] - b[0]);
    for (let index = 1; index < spans.length; index += 1) {
      if ((spans[index]?.[0] ?? 0) < (spans[index - 1]?.[1] ?? 0)) {
        throw new OoxmlError('damaged', 'entries that overlap');
      }
    }
  }
}
