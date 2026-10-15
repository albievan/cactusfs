import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { filesUnder, pdfjsDistProblems } from './pdfjs-dist-check';

const VERSION = '6.4.299';

/** What a build of the console holds that may be shipped: its own files and pdf.js's two decoders. */
const ALLOWED = [
  'index.html',
  'favicon.svg',
  'cactusfs-icon.svg',
  'assets/index-DhaaVR-c.js',
  'assets/index-DwcJW6WH.css',
  'assets/pdf-CDq7pISk.js',
  'assets/pdf.worker.min-CjEcRF4W.mjs',
  'assets/pdf.worker.min--Ao2iAXA.js',
  'assets/bootstrap-icons-DJdhg9lH.woff2',
  'assets/bootstrap-icons-BHUGvtHM.woff',
  'assets/cactusfs-mark-BTSMXD6T.svg',
  'assets/pdfjs-6.4.299/jbig2_nowasm_fallback.js',
  'assets/pdfjs-6.4.299/openjpeg_nowasm_fallback.js',
];

describe('what a build of the console may ship of pdf.js', () => {
  it('is its own files and the two decoders', () => {
    expect(pdfjsDistProblems(ALLOWED, VERSION)).toEqual([]);
  });

  it.each([
    // As a bundler leaves them - flattened, hashed - and as a copy would.
    'assets/jbig2-B1x2y3.wasm',
    'assets/openjpeg.wasm',
    'assets/index-DhaaVR-c.js.map',
    'assets/FoxitSerif-Ab12.pfb',
    'assets/LiberationSans-Regular-Cd34.ttf',
    'assets/Font.otf',
    'assets/UniJIS-UTF16-H-Ef56.bcmap',
    'assets/CGATS001Compat-v2-micro-Gh78.icc',
    'assets/pdf.sandbox.min-Ij90.js',
    'assets/quickjs-eval-Kl12.js',
    'standard_fonts/FoxitDingbats.pfb',
    'cmaps/78-EUC-H.bcmap',
    'iccs/LICENSE',
    'wasm/LICENSE_JBIG2',
    // Anything else under the decoders' directory, or another version's.
    'assets/pdfjs-6.4.299/qcms_bg.js',
    'assets/pdfjs-6.4.298/jbig2_nowasm_fallback.js',
    // A file of public/ is checked as well.
    'favicon.svg.map',
  ])('refuses %s', (file) => {
    expect(pdfjsDistProblems([...ALLOWED, file], VERSION)).toEqual([`refused: ${file}`]);
  });

  it.each(['jbig2_nowasm_fallback.js', 'openjpeg_nowasm_fallback.js'])('refuses a build without %s', (name) => {
    const without = ALLOWED.filter((file) => !file.endsWith(`/${name}`));
    expect(pdfjsDistProblems(without, VERSION)).toEqual([`missing: assets/pdfjs-6.4.299/${name}`]);
  });

  it('refuses the decoders of another version than the one built', () => {
    expect(pdfjsDistProblems(ALLOWED, '6.5.0')).toEqual([
      'refused: assets/pdfjs-6.4.299/jbig2_nowasm_fallback.js',
      'refused: assets/pdfjs-6.4.299/openjpeg_nowasm_fallback.js',
      'missing: assets/pdfjs-6.5.0/jbig2_nowasm_fallback.js',
      'missing: assets/pdfjs-6.5.0/openjpeg_nowasm_fallback.js',
    ]);
  });
});

describe('the files of a build on disk', () => {
  let directory = '';

  afterEach(() => {
    rmSync(directory, { recursive: true, force: true });
  });

  it('are every file under it, by its path with slashes, and no directory', () => {
    directory = mkdtempSync(join(tmpdir(), 'cactusfs-dist-'));
    mkdirSync(join(directory, 'assets', 'pdfjs-6.4.299'), { recursive: true });
    for (const file of ALLOWED) {
      writeFileSync(join(directory, ...file.split('/')), 'x');
    }
    writeFileSync(join(directory, 'assets', 'stray.wasm'), 'x');
    const files = filesUnder(directory);
    expect(files).toEqual([...ALLOWED, 'assets/stray.wasm'].sort());
    expect(pdfjsDistProblems(files, VERSION)).toEqual(['refused: assets/stray.wasm']);
  });
});
