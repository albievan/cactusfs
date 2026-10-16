import { readFileSync, rmSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';

import react from '@vitejs/plugin-react';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

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

// The console is embedded in the cactusfs binary and served from its own
// origin. Every asset is bundled: nothing is loaded from a CDN (air-gap safe).
export default defineConfig({
  plugins: [react(), pdfjsFallbacks()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  // The document reader's worker is made with { type: 'module' }
  // (web/src/preview/office.ts): it is built as one, too.
  worker: {
    format: 'es',
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test/setup.ts'],
  },
});
