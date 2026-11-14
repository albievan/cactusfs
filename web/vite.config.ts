import { readFileSync, rmSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import react from '@vitejs/plugin-react';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

import {
  NOTICES_FILE,
  PDFJS_FALLBACK_NOTICES,
  bundledPackageProblems,
  noticesText,
  pdfjsNoticesDirectory,
  productionPackages,
} from './notices.mjs';
import { PDFJS_FALLBACKS, pdfjsAssetDirectory } from './src/preview/pdfjs-assets.ts';
import { filesUnder, pdfjsDistProblems } from './src/preview/pdfjs-dist-check.ts';

/**
 * Ships pdf.js's two image decoders in plain JavaScript (PDFJS_FALLBACKS)
 * under the names pdf.js imports them by - in dist, and from the dev
 * server - and nothing else of pdfjs-dist's data: no standard fonts,
 * character maps, colour profiles, WebAssembly or source maps (ADR-0028).
 * What the build would write is checked before it is written, and what is
 * in the output directory - public/ included - after: a build that fails
 * the check leaves no output directory, so that no later
 * `go build -tags console` embeds it.
 */
function pdfjsFallbacks(): Plugin {
  const require = createRequire(import.meta.url);
  const root = dirname(require.resolve('pdfjs-dist/package.json'));
  const { version } = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')) as { version: string };
  const directory = pdfjsAssetDirectory(version);
  const read = (name: string) => readFileSync(join(root, 'wasm', name));
  const refuse = (problems: string[]) => `pdf.js in dist (ADR-0028): ${problems.join('; ')}`;
  let outDir = '';
  let building = false;
  // A build that fails the check leaves nothing to embed.
  const discard = () => {
    if (outDir !== '') {
      rmSync(outDir, { recursive: true, force: true });
    }
  };
  return {
    name: 'cactusfs-pdfjs-fallbacks',
    configResolved(config) {
      outDir = resolve(config.root, config.build.outDir);
      building = config.command === 'build';
    },
    buildStart() {
      if (!building) {
        return;
      }
      for (const name of PDFJS_FALLBACKS) {
        this.emitFile({ type: 'asset', fileName: `${directory}${name}`, source: read(name) });
      }
    },
    configureServer(server) {
      server.middlewares.use((request, response, next) => {
        const path = (request.url ?? '').split('?')[0] ?? '';
        const name = PDFJS_FALLBACKS.find((file) => path === `/${directory}${file}`);
        if (name === undefined) {
          next();
          return;
        }
        response.setHeader('Content-Type', 'text/javascript; charset=utf-8');
        response.end(read(name));
      });
    },
    generateBundle(_, bundle) {
      // Before the bundle is written.
      const problems = pdfjsDistProblems(Object.keys(bundle), version);
      if (problems.length > 0) {
        discard();
        this.error(refuse(problems));
      }
    },
    writeBundle(options) {
      // What is on disk, public/ included.
      if (options.dir !== undefined) {
        outDir = options.dir;
      }
      const problems = pdfjsDistProblems(filesUnder(outDir), version);
      if (problems.length > 0) {
        discard();
        this.error(refuse(problems));
      }
    },
  };
}

/** The directory of this file, which is the console's root, and where its build goes. */
const WEB = dirname(fileURLToPath(import.meta.url));
const OUT_DIR = 'dist';

/** A build that fails a check of the notices leaves nothing to embed. */
function refuseNotices(message: string): string {
  rmSync(resolve(WEB, OUT_DIR), { recursive: true, force: true });
  return `the console's notices (PLAN X.5): ${message}`;
}

/**
 * Writes the notices file (PLAN X.5) into dist as `notices.txt`: every
 * production package of the installed tree with its version, licence and the
 * text of its licence and notice files, then the notices of the pdf.js
 * decoders shipped beside the bundle. The walk is notices.mjs, the one the
 * licence gate checks the file against. A licence outside the allow-list, a
 * package without a licence or notice file, or a shipped decoder whose
 * notices are not named fails the build, and its output directory is removed.
 */
function consoleNotices(): Plugin {
  let building = false;
  return {
    name: 'cactusfs-notices',
    configResolved(config) {
      building = config.command === 'build';
    },
    buildStart() {
      if (!building) {
        return;
      }
      // The decoders shipped and the decoders whose notices are named are one
      // list, and the notices name the directory they are shipped in.
      const named = Object.keys(PDFJS_FALLBACK_NOTICES).sort();
      const shipped = [...PDFJS_FALLBACKS].sort();
      if (named.join() !== shipped.join()) {
        this.error(refuseNotices(`the notices name ${named.join(', ')} and the build ships ${shipped.join(', ')}`));
      }
      for (const version of ['0.0.0', '6.4.299']) {
        if (pdfjsNoticesDirectory(version) !== pdfjsAssetDirectory(version)) {
          this.error(refuseNotices('notices.mjs and pdfjs-assets.ts name different directories for the decoders'));
        }
      }
      let text: string;
      try {
        text = noticesText(WEB, PDFJS_FALLBACKS);
      } catch (error) {
        this.error(refuseNotices(error instanceof Error ? error.message : String(error)));
      }
      this.emitFile({ type: 'asset', fileName: NOTICES_FILE, source: text });
    },
  };
}

/**
 * Holds the notices file to what is bundled: every module read from under a
 * node_modules directory must belong to a production package - the ones the
 * file lists - and Vite's module preload polyfill must not be in it. One
 * instance for the page's bundle and one for each worker's (worker.plugins).
 */
function bundledPackages(): Plugin {
  return {
    name: 'cactusfs-bundled-packages',
    apply: 'build',
    generateBundle() {
      let problems: string[];
      try {
        problems = bundledPackageProblems(this.getModuleIds(), productionPackages(WEB));
      } catch (error) {
        this.error(refuseNotices(error instanceof Error ? error.message : String(error)));
      }
      if (problems.length > 0) {
        this.error(refuseNotices(problems.join('; ')));
      }
    },
  };
}

// The console is embedded in the cactusfs binary and served from its own
// origin. Every asset is bundled: nothing is loaded from a CDN (air-gap safe).
export default defineConfig({
  plugins: [react(), pdfjsFallbacks(), consoleNotices(), bundledPackages()],
  build: {
    outDir: OUT_DIR,
    emptyOutDir: true,
    sourcemap: false,
    // No polyfill of <link rel="modulepreload">: it is code of Vite, a
    // development package, whose notices are not in notices.txt. The target
    // is Vite's default, Baseline Widely Available (Chrome and Edge 111+,
    // Firefox 114+, Safari 16.4+); of those only Safari 16.4 lacks
    // modulepreload, and there a module is fetched when it is imported
    // instead of ahead - slower, not broken.
    modulePreload: { polyfill: false },
  },
  // The document reader's worker is made with { type: 'module' }
  // (web/src/preview/office.ts): it is built as one, too.
  worker: {
    format: 'es',
    plugins: () => [bundledPackages()],
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test/setup.ts'],
  },
});
