import type { components } from '../api/schema';

/** What the API says an object is shown as (`X-CactusFS-Preview-Kind`). */
export type PreviewKind = components['schemas']['PreviewKind'];

/** The header of a preview that says what kind of thing its bytes are. */
export const KIND_HEADER = 'X-CactusFS-Preview-Kind';

/** The header of a preview that says whether the object has more bytes than were sent. */
export const TRUNCATED_HEADER = 'X-CactusFS-Preview-Truncated';

/**
 * How the console draws each kind the contract names. `later` is a kind the
 * console has no renderer for yet (PLAN O.10): it says so and offers the
 * download. The record names every kind, so that a kind added to the contract
 * and regenerated into the schema fails the type check until it is placed.
 */
const RENDERERS = {
  png: 'image',
  jpeg: 'image',
  gif: 'image',
  webp: 'image',
  pdf: 'pdf',
  docx: 'later',
  xlsx: 'later',
  text: 'text',
  json: 'text',
  csv: 'csv',
} as const satisfies Record<PreviewKind, 'image' | 'pdf' | 'text' | 'csv' | 'later'>;

/** How a kind is drawn. */
export type Renderer = (typeof RENDERERS)[PreviewKind];

/**
 * The type a Blob of an image kind is given before it becomes an address for
 * an `<img>`: one of four raster types and never SVG, whatever the object was
 * stored as. It is the kind the API named, not anything of the object.
 */
const IMAGE_TYPES: Partial<Record<PreviewKind, string>> = {
  png: 'image/png',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
};

/**
 * The kind a preview's header names, or null when it names none the console
 * knows - a kind a later release of the API added, or no header at all.
 */
export function kindOf(header: string | null): PreviewKind | null {
  if (header === null) {
    return null;
  }
  return Object.hasOwn(RENDERERS, header) ? (header as PreviewKind) : null;
}

/** How the console draws a kind. */
export function rendererOf(kind: PreviewKind): Renderer {
  return RENDERERS[kind];
}

/** The type of the Blob an image of this kind is shown from; null for a kind that is no image. */
export function imageTypeOf(kind: PreviewKind): string | null {
  return IMAGE_TYPES[kind] ?? null;
}

/**
 * The stored types for which "Preview" is offered: those the contract of
 * `previewObject` names a kind for, and none or `application/octet-stream`,
 * which it lets the bytes decide for images and PDF. Offering the button is
 * all this decides: whether the object is shown, as what and to whom, is the
 * API's answer to the request.
 */
const OFFERED_TYPES: ReadonlySet<string> = new Set([
  'image/png',
  'image/jpeg',
  'image/jpg',
  'image/gif',
  'image/webp',
  'application/pdf',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'text/plain',
  'text/csv',
  'application/json',
  'application/octet-stream',
  '',
]);

/** A stored content type as the API compares it: its media type, in lower case, without parameters. */
export function mediaTypeOf(contentType: string): string {
  const semicolon = contentType.indexOf(';');
  return (semicolon === -1 ? contentType : contentType.slice(0, semicolon)).trim().toLowerCase();
}

/** Whether "Preview" is offered for an object stored with this content type. */
export function offersPreview(contentType: string): boolean {
  return OFFERED_TYPES.has(mediaTypeOf(contentType));
}
