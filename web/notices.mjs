// The console's production packages, their licences and their notices (PLAN
// X.5). One walk of the installed tree, shared by the build - vite.config.ts
// writes the notices file into dist, so it ships in the binary's embedded
// console and in the image - and by the licence gate,
// scripts/web-licences.mjs, which also says when a built notices file is
// stale. Plain JavaScript with no dependency, so that Node runs it without a
// build step; its types are in notices.d.mts.
//
// Everything is read from each package's own files in the installed tree, so
// nothing depends on the output format of a package-manager command. The
// output is sorted and carries no date, path of the machine or count of time:
// the same tree gives the same bytes.
import { existsSync, readFileSync, readdirSync, realpathSync, statSync } from 'node:fs';
import { dirname, join, sep } from 'node:path';

/** The licences of CLAUDE.md section 3, as SPDX identifiers. scripts/licences.sh passes the same list. */
export const ALLOWED_LICENCES = Object.freeze(['Apache-2.0', 'BSD-2-Clause', 'BSD-3-Clause', 'ISC', 'MIT', 'MPL-2.0']);

/** The name the notices file has in dist, and the path the console serves it at. */
export const NOTICES_FILE = 'notices.txt';

/**
 * The notices of files shipped beside the bundle, not in it: pdf.js's two
 * image decoders in plain JavaScript (vite.config.ts copies them, unchanged,
 * to `assets/pdfjs-<version>/`; ADR-0028), by the name they are shipped
 * under. Each is code of others inside code of Mozilla, and both licences
 * go with it. The files are those of pdfjs-dist's `wasm/` directory.
 */
export const PDFJS_FALLBACK_NOTICES = Object.freeze({
  'jbig2_nowasm_fallback.js': Object.freeze({
    what: 'the JBIG2 and CCITT decoder',
    licences: 'BSD-3-Clause (the decoder, of PDFium), Apache-2.0 (the code of Mozilla around it)',
    files: Object.freeze(['wasm/LICENSE_JBIG2', 'wasm/LICENSE_PDFJS_JBIG2']),
  }),
  'openjpeg_nowasm_fallback.js': Object.freeze({
    what: 'the JPEG 2000 decoder',
    licences: 'BSD-2-Clause (the decoder, of OpenJPEG), BSD-2-Clause (the code of Mozilla around it)',
    files: Object.freeze(['wasm/LICENSE_OPENJPEG', 'wasm/LICENSE_PDFJS_OPENJPEG']),
  }),
});

/** A file at the top of a package that carries its licence or a notice. */
const NOTICE_NAME = /^(licen[cs]e|copying|notice|copyright)([._-][^/]*)?$/i;

const RULE = '-'.repeat(78);

/**
 * The directory of dist the decoders are shipped in, by the version of
 * pdf.js. The same as pdfjsAssetDirectory of src/preview/pdfjs-assets.ts,
 * which the console's own code imports and so cannot import this file
 * (node:fs); vite.config.ts refuses to build when the two differ.
 */
export function pdfjsNoticesDirectory(version) {
  return `assets/pdfjs-${version}/`;
}

const NODE_MODULES = '/node_modules/';
const slashed = (path) => path.split(sep).join('/').split('\\').join('/');

const readJSON = (file) => JSON.parse(readFileSync(file, 'utf8'));

// Code-point order, not the locale's: the same on every machine.
const byCodePoint = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

/** A text as it goes into the file: LF line ends, no trailing blank lines, no BOM. */
function normalised(text) {
  return text.replace(/^\uFEFF/, '').replace(/\r\n?/g, '\n').replace(/[ \t]+$/gm, '').replace(/\s+$/, '');
}

// Resolves a dependency the way Node does: walk up from the dependant.
function resolve(name, from) {
  for (let dir = from; ; dir = dirname(dir)) {
    const candidate = join(dir, 'node_modules', name, 'package.json');
    if (existsSync(candidate)) return dirname(realpathSync(candidate));
    if (dir === dirname(dir)) return null;
  }
}

/**
 * Every production package installed under root (the directory of the
 * console's package.json): its dependencies, direct or transitive, optional
 * ones included when installed. Development-only packages are build tools
 * and are not shipped. Sorted by name, then version.
 */
export function productionPackages(root) {
  const top = realpathSync(root);
  const rootPkg = readJSON(join(top, 'package.json'));
  const queue = Object.keys(rootPkg.dependencies ?? {}).map((name) => [name, top, false]);
  if (queue.length === 0) {
    throw new Error('package.json lists no production dependency');
  }
  const seen = new Map();
  while (queue.length > 0) {
    const [name, from, optional] = queue.pop();
    const dir = resolve(name, from);
    if (dir === null) {
      // An optional package for another platform is legitimately absent.
      if (optional) continue;
      throw new Error(`cannot find ${name} (required by ${from === top ? 'the console' : from}); run 'make web-install'`);
    }
    if (seen.has(dir)) continue;
    const pkg = readJSON(join(dir, 'package.json'));
    seen.set(dir, {
      name: String(pkg.name),
      version: String(pkg.version),
      licence: typeof pkg.license === 'string' ? pkg.license : '',
      dir,
    });
    for (const dep of Object.keys(pkg.dependencies ?? {})) queue.push([dep, dir, false]);
    for (const dep of Object.keys(pkg.optionalDependencies ?? {})) queue.push([dep, dir, true]);
  }
  return [...seen.values()].sort((a, b) => byCodePoint(a.name, b.name) || byCodePoint(a.version, b.version));
}

/** The packages whose declared licence is not on the list, as "name@version: licence". */
export function licenceProblems(packages, allowed = ALLOWED_LICENCES) {
  const ok = new Set(allowed);
  return packages
    .filter((pkg) => !ok.has(pkg.licence))
    .map((pkg) => `${pkg.name}@${pkg.version}: ${pkg.licence || 'no licence declared'}`)
    .sort(byCodePoint);
}

/**
 * What a bundle holds of packages that are not production packages: for each
 * module the bundler read (moduleIds, as `this.getModuleIds()` gives them)
 * from under a node_modules directory, the package it belongs to must be one
 * of packages - else its notices are not in the file. The console's own
 * modules are not under node_modules and are not looked at; virtual modules
 * (`\0...`) are the bundler's own, except Vite's module preload polyfill,
 * which is code of Vite (a development package) and is refused by name.
 * Sorted, one line a package.
 */
export function bundledPackageProblems(moduleIds, packages) {
  const shipped = new Set(packages.map((pkg) => slashed(pkg.dir)));
  const problems = new Set();
  for (const raw of moduleIds) {
    if (raw.startsWith('\u0000')) {
      if (raw.includes('modulepreload-polyfill')) {
        problems.add(`Vite's module preload polyfill is in the bundle: set build.modulePreload.polyfill to false`);
      }
      continue;
    }
    const id = slashed(raw.split('?')[0] ?? raw);
    const at = id.lastIndexOf(NODE_MODULES);
    if (at < 0) continue;
    const parts = id.slice(at + NODE_MODULES.length).split('/');
    const name = parts[0]?.startsWith('@') ? parts.slice(0, 2).join('/') : (parts[0] ?? '');
    let root = id.slice(0, at + NODE_MODULES.length) + name;
    try {
      root = slashed(realpathSync(root));
    } catch {
      // Not on disk under that name: compared as the bundler gave it.
    }
    if (!shipped.has(root)) {
      problems.add(`${name} is in the bundle but is not a production package (${root})`);
    }
  }
  return [...problems].sort(byCodePoint);
}

/** The files at the top of a package's directory that carry its licence or a notice, sorted. */
export function noticeFiles(dir) {
  return readdirSync(dir)
    .filter((name) => NOTICE_NAME.test(name) && statSync(join(dir, name)).isFile())
    .sort(byCodePoint);
}

function section(heading, licence, texts) {
  const lines = [RULE, heading, licence, RULE];
  for (const [name, text] of texts) {
    lines.push('', `${name}:`, '', text);
  }
  lines.push('');
  return lines.join('\n');
}

/**
 * The notices file: every production package's name, version, licence and
 * the text of each of its licence and notice files, then the notices of the
 * files shipped beside the bundle (fallbacks: the names vite.config.ts ships,
 * each of which must be a key of PDFJS_FALLBACK_NOTICES). Throws, naming
 * every problem, when a licence is not on the list, when a package has no
 * licence or notice file, or when a notice of a shipped file is missing.
 */
export function noticesText(root, fallbacks, allowed = ALLOWED_LICENCES) {
  const packages = productionPackages(root);
  const problems = licenceProblems(packages, allowed).map((line) => `licence outside the allow-list: ${line}`);
  const sections = [];
  for (const pkg of packages) {
    const files = noticeFiles(pkg.dir);
    if (files.length === 0) {
      problems.push(`no licence or notice file in ${pkg.name}@${pkg.version}`);
      continue;
    }
    sections.push(
      section(
        `${pkg.name} ${pkg.version}`,
        `Licence: ${pkg.licence}`,
        files.map((name) => [name, normalised(readFileSync(join(pkg.dir, name), 'utf8'))]),
      ),
    );
  }

  const pdfjs = packages.find((pkg) => pkg.name === 'pdfjs-dist');
  const shipped = [];
  for (const name of [...fallbacks].sort(byCodePoint)) {
    const entry = PDFJS_FALLBACK_NOTICES[name];
    if (entry === undefined) {
      problems.push(`no notices named for the shipped file ${name} (PDFJS_FALLBACK_NOTICES)`);
      continue;
    }
    if (pdfjs === undefined) {
      problems.push(`${name} is shipped but pdfjs-dist is not a production package`);
      continue;
    }
    const texts = [];
    for (const file of entry.files) {
      const path = join(pdfjs.dir, file);
      if (!existsSync(path)) {
        problems.push(`pdfjs-dist@${pdfjs.version} has no ${file}, the notice of ${name}`);
        continue;
      }
      texts.push([file, normalised(readFileSync(path, 'utf8'))]);
    }
    shipped.push(
      section(
        `${pdfjsNoticesDirectory(pdfjs.version)}${name}: ${entry.what} of pdfjs-dist ${pdfjs.version}`,
        `Licences: ${entry.licences}`,
        texts,
      ),
    );
  }

  if (problems.length > 0) {
    throw new Error(problems.sort(byCodePoint).join('; '));
  }

  const head = [
    'Notices of the CactusFS console',
    '',
    'The CactusFS console - the web application the cactusfs binary serves - is built',
    'with the packages below, which keep their own licences. CactusFS itself is',
    'dedicated to the public domain: see LICENSE.md in its source.',
    '',
    'This file is generated when the console is built, from the installed production',
    `packages of web/: ${packages.length} packages, then ${shipped.length} files shipped beside the bundle.`,
    '',
    '',
  ].join('\n');
  const tail = shipped.length === 0 ? '' : `\nFiles shipped beside the bundle\n\n${shipped.join('\n')}`;
  return `${head}Packages\n\n${sections.join('\n')}${tail}`;
}
