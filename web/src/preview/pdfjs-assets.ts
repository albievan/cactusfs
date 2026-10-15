// What of pdfjs-dist the console ships besides its code, and where: read by
// vite.config.ts, which copies the files into dist, and by the console,
// which tells pdf.js where they are. Nothing here may depend on a browser.

/**
 * The decoders of JBIG2 and CCITT (scanned pages) and of JPEG 2000 images,
 * compiled to plain JavaScript (wasm2js): pdf.js imports them, by these
 * names, from `wasmUrl` when it does not use WebAssembly - which the
 * console's CSP refuses. Neither evaluates code or fetches anything.
 * Licences: the decoders BSD-3-Clause (PDFium, JBIG2) and BSD-2-Clause
 * (OpenJPEG), the code around them Apache-2.0 and BSD-2-Clause (Mozilla).
 */
export const PDFJS_FALLBACKS = ['jbig2_nowasm_fallback.js', 'openjpeg_nowasm_fallback.js'] as const;

/**
 * The directory of `dist` the fallbacks are in, by the version of pdf.js:
 * under `assets/`, which is cached for good, so a new version is a new
 * directory and never a stale file.
 */
export function pdfjsAssetDirectory(version: string): string {
  return `assets/pdfjs-${version}/`;
}
