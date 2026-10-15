// The check of what the console's build ships of pdfjs-dist (ADR-0028),
// for web/vite.config.ts and its tests. It runs in Node, at build time: the
// console never imports it.
import { readdirSync, statSync } from 'node:fs';
import { join, sep } from 'node:path';

import { PDFJS_FALLBACKS, pdfjsAssetDirectory } from './pdfjs-assets.ts';

/**
 * Files pdfjs-dist has that the console never ships: WebAssembly, source
 * maps, standard fonts (`.pfb`, `.ttf`, `.otf`), character maps (`.bcmap`),
 * colour profiles (`.icc`) - by extension, as a bundler that flattens
 * directories leaves them.
 */
const REFUSED_EXTENSIONS = /\.(wasm|map|pfb|ttf|otf|bcmap|icc)$/i;

/** pdf.js's scripting sandbox and its JavaScript engine, by name. */
const REFUSED_NAMES = /(^|\/)[^/]*(pdf\.sandbox|quickjs)[^/]*$/i;

/** pdfjs-dist's own directories of data, should a copy keep them. */
const REFUSED_DIRECTORIES = /(^|\/)(standard_fonts|cmaps|iccs|wasm)\//;

/**
 * What is wrong with the files a build of the console holds (paths relative
 * to its output, with `/`): every file it must not ship, and every one of
 * pdf.js's two image decoders it lacks. Empty when the build may be shipped.
 */
export function pdfjsDistProblems(files: readonly string[], version: string): string[] {
  const directory = pdfjsAssetDirectory(version);
  const fallbacks = new Set(PDFJS_FALLBACKS.map((name) => `${directory}${name}`));
  const problems: string[] = [];
  for (const file of files) {
    if (fallbacks.has(file)) {
      continue;
    }
    if (
      REFUSED_EXTENSIONS.test(file) ||
      REFUSED_NAMES.test(file) ||
      REFUSED_DIRECTORIES.test(file) ||
      file.startsWith('assets/pdfjs-')
    ) {
      problems.push(`refused: ${file}`);
    }
  }
  for (const fallback of fallbacks) {
    if (!files.includes(fallback)) {
      problems.push(`missing: ${fallback}`);
    }
  }
  return problems;
}

/** Every file under `directory`, as a path relative to it with `/`. */
export function filesUnder(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: 'utf8' })
    .filter((path) => statSync(join(directory, path)).isFile())
    .map((path) => path.split(sep).join('/'))
    .sort();
}
