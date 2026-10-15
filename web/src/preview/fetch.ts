import { api, unwrap } from '../api/client';
import { KIND_HEADER, kindOf, TRUNCATED_HEADER, type PreviewKind } from './kinds';

/** A preview the API answered with its bytes: what they are, and the bytes still to be read. */
export interface PreviewOpened {
  ok: true;
  /** What the API says the bytes are; null when it names a kind the console does not know. */
  kind: PreviewKind | null;
  /** Whether the object has more bytes than were sent: text, CSV and JSON only. */
  truncated: boolean;
  /** How many bytes the API says it sends (`Content-Length`); null when it does not say. */
  length: number | null;
  /** The bytes, not read yet; null when there are none. */
  body: Response['body'];
}

/** A preview the API refused, or that could not be asked for. */
export interface PreviewRefused {
  ok: false;
  /** Why: an ApiError for an answer of the API, another error when there was none. */
  error: unknown;
  /** After how many seconds to ask again, where a 429 said so. */
  retryAfter: number | null;
}

/** What asking for a preview came to. */
export type PreviewAnswer = PreviewOpened | PreviewRefused;

/** The whole number a header gives - the seconds of `Retry-After`, the bytes of `Content-Length` - or null. */
function wholeNumberOf(header: string | null): number | null {
  if (header === null || !/^\d+$/.test(header.trim())) {
    return null;
  }
  return Number(header.trim());
}

/**
 * Asks the API for the preview of the current version of an object
 * (`previewObject`) with the generated client. The bytes are not read here:
 * whoever asked reads them when the kind is one it draws, and cancels them
 * when it is not, so that a PDF the console cannot show yet is not
 * downloaded for nothing. Aborting `signal` gives the request up, its body
 * included.
 *
 * Rejects only when `signal` was aborted; every other failure is an answer.
 */
export async function openPreview(bucket: string, key: string, signal: AbortSignal): Promise<PreviewAnswer> {
  const ask = () =>
    api.GET('/buckets/{bucket}/object/preview', {
      params: { path: { bucket }, query: { key } },
      parseAs: 'stream',
      signal,
    });
  let result: Awaited<ReturnType<typeof ask>>;
  try {
    result = await ask();
  } catch (error: unknown) {
    if (signal.aborted) {
      throw error;
    }
    return { ok: false, error, retryAfter: null };
  }
  const { response } = result;
  if (!response.ok) {
    const error = await unwrap(Promise.resolve(result)).then(
      () => null,
      (refusal: unknown) => refusal,
    );
    return { ok: false, error, retryAfter: wholeNumberOf(response.headers.get('Retry-After')) };
  }
  return {
    ok: true,
    kind: kindOf(response.headers.get(KIND_HEADER)),
    truncated: response.headers.get(TRUNCATED_HEADER) === 'true',
    length: wholeNumberOf(response.headers.get('Content-Length')),
    body: result.data ?? null,
  };
}

/**
 * The most bytes the console takes for one preview: the largest
 * `preview-max-size` the contract allows a node (256 MiB). A node never sends
 * more; one that says it would, or does, is not read on.
 */
export const PREVIEW_MAX_BYTES = 256 * 1024 * 1024;

/** A preview larger than PREVIEW_MAX_BYTES: not read on. */
export class PreviewTooLargeError extends Error {
  constructor() {
    super(`larger than the ${String(PREVIEW_MAX_BYTES / (1024 * 1024))} MiB the console takes`);
    this.name = 'PreviewTooLargeError';
  }
}

/**
 * Reads the bytes of a preview to the end, as one Blob - not copied into
 * another buffer on the way. More than `max` bytes is a PreviewTooLargeError,
 * and the rest is not read. A body that ends before its `Content-Length` -
 * the API cuts a GIF off at a frame larger than its screen - is an error,
 * never a shorter image. Aborting `signal` cancels the body and rejects.
 */
export async function readBlob(
  body: Response['body'],
  signal: AbortSignal,
  max: number = PREVIEW_MAX_BYTES,
): Promise<Blob> {
  if (body === null) {
    return new Blob([]);
  }
  let total = 0;
  let tooLarge = false;
  const counted = body.pipeThrough(
    new TransformStream<Uint8Array, Uint8Array>({
      transform(chunk, controller) {
        total += chunk.byteLength;
        if (total > max) {
          tooLarge = true;
          controller.error(new PreviewTooLargeError());
          return;
        }
        controller.enqueue(chunk);
      },
    }),
    { signal },
  );
  try {
    return await new Response(counted).blob();
  } catch (error: unknown) {
    throw tooLarge ? new PreviewTooLargeError() : error;
  }
}
