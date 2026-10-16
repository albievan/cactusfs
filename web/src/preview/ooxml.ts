// What the preview's worker does with a DOCX or XLSX (ADR-0028): reads it
// within OOXML_BOUNDS into a plain model, or says why it did not. Only the
// worker imports this module: none of the readers, nor fflate, is in the
// console's own bundle.

import { readDocx } from './docx';
import { OoxmlError } from './ooxml-error';
import type { OoxmlKind, OoxmlModel, OoxmlReply } from './ooxml-model';
import { readXlsx } from './xlsx';
import { ZipArchive, type ZipBounds } from './zip';

/** Reads the bytes of a DOCX or XLSX into its model; an OoxmlError says why it cannot be. */
export function readOoxml(kind: OoxmlKind, bytes: Uint8Array, bounds?: ZipBounds): OoxmlModel {
  const zip = new ZipArchive(bytes, bounds);
  return kind === 'docx' ? readDocx(zip) : readXlsx(zip);
}

/**
 * The answer to a message the worker was sent, and the buffers to hand over
 * with it (the images of a document). A message that is not a request, and
 * a failure that is no OoxmlError, are `damaged`: nothing of the document,
 * nor of the failure, is said.
 */
export function answer(message: unknown): { reply: OoxmlReply; transfer: ArrayBuffer[] } {
  const request = message as { kind?: unknown; bytes?: unknown } | null;
  if (
    typeof request !== 'object' ||
    request === null ||
    (request.kind !== 'docx' && request.kind !== 'xlsx') ||
    // By its tag, not instanceof: a buffer made in another realm is one too.
    Object.prototype.toString.call(request.bytes) !== '[object ArrayBuffer]'
  ) {
    return { reply: { ok: false, problem: 'damaged' }, transfer: [] };
  }
  try {
    const model = readOoxml(request.kind, new Uint8Array(request.bytes as ArrayBuffer));
    const transfer = model.kind === 'docx' ? model.images.map((image) => image.bytes.buffer) : [];
    return { reply: { ok: true, model }, transfer };
  } catch (error: unknown) {
    return { reply: { ok: false, problem: error instanceof OoxmlError ? error.problem : 'damaged' }, transfer: [] };
  }
}
