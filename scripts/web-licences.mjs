// Licence gate for the console: every production dependency, direct or
// transitive (optional ones included when installed), must carry a licence
// from the allow-list and a licence or notice file. Development-only packages
// are build tools and are not checked, so anything the console imports must
// be listed under "dependencies". Run through `make licences`, after the
// console is built: the notices file the build wrote into web/dist (PLAN X.5)
// must be, byte for byte, what the build of the installed tree gives (the
// file is generated deterministically) - else it is not the console's, and
// neither is the console that would be embedded.
//
// The walk is web/notices.mjs, the one the build itself uses.
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

import {
  ALLOWED_LICENCES,
  NOTICES_FILE,
  PDFJS_FALLBACK_NOTICES,
  licenceProblems,
  noticesText,
  productionPackages,
} from '../web/notices.mjs';

const fail = (message) => {
  console.error(`licences: FAIL - web: ${message}`);
  process.exit(1);
};

const allowed = (process.argv[2] ?? '').split(',').filter(Boolean);
if (allowed.length === 0) {
  console.error('usage: node scripts/web-licences.mjs <comma-separated SPDX ids>');
  process.exit(2);
}
// One list: the gate's (scripts/licences.sh) and the build's must not drift.
if ([...allowed].sort().join(',') !== [...ALLOWED_LICENCES].sort().join(',')) {
  fail(`the allow-list given (${allowed.join(',')}) is not web/notices.mjs's (${ALLOWED_LICENCES.join(',')})`);
}

let packages;
let want;
try {
  packages = productionPackages('web');
  const violations = licenceProblems(packages, allowed);
  if (violations.length > 0) {
    console.error('licences: FAIL - web: production packages outside the allow-list:');
    for (const line of violations) console.error(`  ${line}`);
    process.exit(1);
  }
  want = noticesText('web', Object.keys(PDFJS_FALLBACK_NOTICES), allowed);
} catch (error) {
  fail(error instanceof Error ? error.message : String(error));
}

const built = join('web', 'dist', NOTICES_FILE);
if (!existsSync(built)) {
  fail(`no ${built}: the console was not built, or its build did not write the notices; run 'make web-build'`);
}
if (readFileSync(built, 'utf8') !== want) {
  fail(`${built} is not what the build of the installed tree gives; run 'make web-build'`);
}
console.log(`licences: web - ${packages.length} production packages, all allowed; ${built} is current`);
