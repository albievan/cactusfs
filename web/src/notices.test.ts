// @vitest-environment node
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { afterEach, describe, expect, it } from 'vitest';

import {
  ALLOWED_LICENCES,
  NOTICES_FILE,
  PDFJS_FALLBACK_NOTICES,
  bundledPackageProblems,
  noticeFiles,
  noticesText,
  pdfjsNoticesDirectory,
  productionPackages,
} from '../notices.mjs';
import { PDFJS_FALLBACKS, pdfjsAssetDirectory } from './preview/pdfjs-assets';

const DECODERS = ['jbig2_nowasm_fallback.js', 'openjpeg_nowasm_fallback.js'];

let made: string[] = [];
afterEach(() => {
  for (const dir of made) rmSync(dir, { recursive: true, force: true });
  made = [];
});

type Files = Record<string, string>;

function write(root: string, files: Files) {
  for (const [path, text] of Object.entries(files)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), text);
  }
}

const manifest = (fields: Record<string, unknown>) => JSON.stringify(fields);

/**
 * An installed tree: the console depends on alpha and pdfjs-dist; alpha on
 * beta, nested under it, and optionally on gamma, which is not installed; a
 * development package, delta, is installed but never shipped.
 */
function tree(change: Files = {}): string {
  const root = mkdtempSync(join(tmpdir(), 'cactusfs-notices-'));
  made.push(root);
  write(root, {
    'package.json': manifest({
      name: 'console',
      dependencies: { alpha: '1.0.0', 'pdfjs-dist': '6.4.299' },
      devDependencies: { delta: '1.0.0' },
    }),
    'node_modules/alpha/package.json': manifest({
      name: 'alpha',
      version: '1.0.0',
      license: 'MIT',
      dependencies: { beta: '2.0.0' },
      optionalDependencies: { gamma: '3.0.0' },
    }),
    'node_modules/alpha/LICENSE': 'MIT licence of alpha\r\nline two   \r\n\r\n',
    'node_modules/alpha/README.md': 'not a notice',
    'node_modules/alpha/node_modules/beta/package.json': manifest({ name: 'beta', version: '2.0.0', license: 'Apache-2.0' }),
    'node_modules/alpha/node_modules/beta/LICENSE.txt': 'Apache licence of beta',
    'node_modules/alpha/node_modules/beta/NOTICE': 'Notice of beta',
    'node_modules/delta/package.json': manifest({ name: 'delta', version: '1.0.0', license: 'GPL-3.0-only' }),
    'node_modules/delta/LICENSE': 'GPL',
    'node_modules/pdfjs-dist/package.json': manifest({ name: 'pdfjs-dist', version: '6.4.299', license: 'Apache-2.0' }),
    'node_modules/pdfjs-dist/LICENSE': 'Apache licence of pdf.js',
    'node_modules/pdfjs-dist/wasm/LICENSE_JBIG2': 'PDFium BSD-3-Clause',
    'node_modules/pdfjs-dist/wasm/LICENSE_PDFJS_JBIG2': 'Mozilla Apache-2.0',
    'node_modules/pdfjs-dist/wasm/LICENSE_OPENJPEG': 'OpenJPEG BSD-2-Clause',
    'node_modules/pdfjs-dist/wasm/LICENSE_PDFJS_OPENJPEG': 'Mozilla BSD-2-Clause',
    ...change,
  });
  return root;
}

describe("the console's production packages", () => {
  it('are the dependencies, transitive and nested, and not the development ones', () => {
    const packages = productionPackages(tree());
    expect(packages.map((pkg) => `${pkg.name}@${pkg.version} ${pkg.licence}`)).toEqual([
      'alpha@1.0.0 MIT',
      'beta@2.0.0 Apache-2.0',
      'pdfjs-dist@6.4.299 Apache-2.0',
    ]);
  });

  it('refuse a required package that is not installed', () => {
    const root = tree();
    rmSync(join(root, 'node_modules/alpha/node_modules/beta'), { recursive: true });
    expect(() => productionPackages(root)).toThrow(/cannot find beta/);
  });

  it('carry their licence and notice files, and nothing else', () => {
    const root = tree();
    expect(noticeFiles(join(root, 'node_modules/alpha'))).toEqual(['LICENSE']);
    expect(noticeFiles(join(root, 'node_modules/alpha/node_modules/beta'))).toEqual(['LICENSE.txt', 'NOTICE']);
  });
});

describe('what a bundle holds of packages', () => {
  it('is production packages only, nested and through a query', () => {
    const root = tree();
    const packages = productionPackages(root);
    const ids = [
      join(root, 'src/main.tsx'),
      join(root, 'node_modules/alpha/index.js'),
      join(root, 'node_modules/alpha/node_modules/beta/lib/beta.js'),
      `${join(root, 'node_modules/pdfjs-dist/build/pdf.worker.min.mjs')}?url`,
      '\0rolldown/runtime.js',
      '\0vite/preload-helper.js',
    ];
    expect(bundledPackageProblems(ids, packages)).toEqual([]);
  });

  it('refuses a development package, scoped or not, once each', () => {
    const root = tree({
      'node_modules/@scope/epsilon/package.json': manifest({ name: '@scope/epsilon', version: '1.0.0', license: 'MIT' }),
    });
    const ids = [
      join(root, 'node_modules/delta/index.js'),
      join(root, 'node_modules/delta/other.js'),
      join(root, 'node_modules/@scope/epsilon/index.js'),
    ];
    const problems = bundledPackageProblems(ids, productionPackages(root));
    expect(problems).toHaveLength(2);
    expect(problems[0]).toMatch(/^@scope\/epsilon is in the bundle but is not a production package/);
    expect(problems[1]).toMatch(/^delta is in the bundle but is not a production package/);
  });

  it("refuses Vite's module preload polyfill", () => {
    expect(bundledPackageProblems(['\0vite/modulepreload-polyfill.js'], productionPackages(tree()))).toEqual([
      "Vite's module preload polyfill is in the bundle: set build.modulePreload.polyfill to false",
    ]);
  });
});

describe('the notices file', () => {
  it("names each package's name, version and licence, with the text of each of its notices", () => {
    const text = noticesText(tree(), DECODERS);
    expect(text).toContain('alpha 1.0.0\nLicence: MIT\n');
    expect(text).toContain('LICENSE:\n\nMIT licence of alpha\nline two\n');
    expect(text).toContain('beta 2.0.0\nLicence: Apache-2.0\n');
    expect(text).toContain('LICENSE.txt:\n\nApache licence of beta\n');
    expect(text).toContain('NOTICE:\n\nNotice of beta\n');
    expect(text).toContain('pdfjs-dist 6.4.299\nLicence: Apache-2.0\n');
    expect(text).not.toContain('delta');
    expect(text).not.toContain('not a notice');
    expect(text).not.toContain('\r');
  });

  it('then the notices of the decoders shipped beside the bundle, both licences of each', () => {
    const text = noticesText(tree(), DECODERS);
    const jbig2 = text.indexOf('assets/pdfjs-6.4.299/jbig2_nowasm_fallback.js');
    const openjpeg = text.indexOf('assets/pdfjs-6.4.299/openjpeg_nowasm_fallback.js');
    expect(jbig2).toBeGreaterThan(text.indexOf('pdfjs-dist 6.4.299\n'));
    expect(openjpeg).toBeGreaterThan(jbig2);
    for (const notice of ['PDFium BSD-3-Clause', 'Mozilla Apache-2.0', 'OpenJPEG BSD-2-Clause', 'Mozilla BSD-2-Clause']) {
      expect(text).toContain(notice);
    }
  });

  it('is the same bytes for the same tree, sorted, with no path of the machine', () => {
    const root = tree();
    const text = noticesText(root, DECODERS);
    expect(noticesText(root, [...DECODERS].reverse())).toBe(text);
    expect(noticesText(tree(), DECODERS)).toBe(text);
    expect(text).not.toContain(root);
    expect(text.indexOf('alpha 1.0.0')).toBeLessThan(text.indexOf('beta 2.0.0'));
    expect(text.endsWith('\n')).toBe(true);
  });

  it.each<[string, Files, RegExp]>([
    [
      'a licence outside the allow-list',
      { 'node_modules/alpha/node_modules/beta/package.json': manifest({ name: 'beta', version: '2.0.0', license: 'AGPL-3.0-only' }) },
      /licence outside the allow-list: beta@2\.0\.0: AGPL-3\.0-only/,
    ],
    [
      'no licence declared',
      { 'node_modules/alpha/package.json': manifest({ name: 'alpha', version: '1.0.0' }) },
      /alpha@1\.0\.0: no licence declared/,
    ],
    [
      'a licence that is not a plain identifier',
      { 'node_modules/alpha/package.json': manifest({ name: 'alpha', version: '1.0.0', license: { type: 'MIT' } }) },
      /alpha@1\.0\.0: no licence declared/,
    ],
  ])('refuses %s', (_, change, message) => {
    expect(() => noticesText(tree(change), DECODERS)).toThrow(message);
  });

  it('refuses a package without a licence or notice file', () => {
    const root = tree();
    rmSync(join(root, 'node_modules/alpha/LICENSE'));
    expect(() => noticesText(root, DECODERS)).toThrow(/no licence or notice file in alpha@1\.0\.0/);
  });

  it('refuses a shipped decoder whose notice is missing', () => {
    const root = tree();
    rmSync(join(root, 'node_modules/pdfjs-dist/wasm/LICENSE_PDFJS_OPENJPEG'));
    expect(() => noticesText(root, DECODERS)).toThrow(
      /pdfjs-dist@6\.4\.299 has no wasm\/LICENSE_PDFJS_OPENJPEG, the notice of openjpeg_nowasm_fallback\.js/,
    );
  });

  it('refuses a shipped file whose notices are not named', () => {
    expect(() => noticesText(tree(), [...DECODERS, 'qcms_nowasm_fallback.js'])).toThrow(
      /no notices named for the shipped file qcms_nowasm_fallback\.js/,
    );
  });

  it('names every problem at once', () => {
    const root = tree({
      'node_modules/alpha/package.json': manifest({
        name: 'alpha',
        version: '1.0.0',
        license: 'GPL-2.0',
        dependencies: { beta: '2.0.0' },
      }),
    });
    rmSync(join(root, 'node_modules/alpha/node_modules/beta/LICENSE.txt'));
    rmSync(join(root, 'node_modules/alpha/node_modules/beta/NOTICE'));
    expect(() => noticesText(root, DECODERS)).toThrow(
      'licence outside the allow-list: alpha@1.0.0: GPL-2.0; no licence or notice file in beta@2.0.0',
    );
  });
});

describe("the console's own tree", () => {
  const web = fileURLToPath(new URL('..', import.meta.url));

  it('names the notices of exactly the decoders the build ships, in the directory they are shipped in', () => {
    expect(Object.keys(PDFJS_FALLBACK_NOTICES).sort()).toEqual([...PDFJS_FALLBACKS].sort());
    for (const version of ['6.4.299', '7.0.0-rc.1']) {
      expect(pdfjsNoticesDirectory(version)).toBe(pdfjsAssetDirectory(version));
    }
  });

  it('gives a notices file that lists the packages the console is built with', () => {
    const text = noticesText(web, PDFJS_FALLBACKS);
    const pdfjs = productionPackages(web).find((pkg) => pkg.name === 'pdfjs-dist');
    for (const name of PDFJS_FALLBACKS) {
      expect(text).toContain(`\n${pdfjsAssetDirectory(pdfjs?.version ?? '')}${name}: `);
    }
    for (const name of ['react', 'react-dom', 'bootstrap', 'bootstrap-icons', 'openapi-fetch', 'pdfjs-dist', 'fflate']) {
      expect(text).toMatch(new RegExp(`^${name} \\S+\\nLicence: \\S+\\n`, 'm'));
    }
    for (const file of ['LICENSE_JBIG2', 'LICENSE_PDFJS_JBIG2', 'LICENSE_OPENJPEG', 'LICENSE_PDFJS_OPENJPEG']) {
      expect(text).toContain(`wasm/${file}:`);
    }
    expect(NOTICES_FILE).toBe('notices.txt');
    expect(ALLOWED_LICENCES).toEqual(['Apache-2.0', 'BSD-2-Clause', 'BSD-3-Clause', 'ISC', 'MIT', 'MPL-2.0']);
  });
});
