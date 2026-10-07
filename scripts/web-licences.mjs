// Licence gate for the console: every production dependency, direct or
// transitive (optional ones included when installed), must carry a licence
// from the allow-list. Development-only packages are build tools and are not
// checked, so anything the console imports must be listed under
// "dependencies". Run through `make licences`.
//
// Reads each package's own package.json from the installed tree, so it does not
// depend on the output format of any package-manager command.
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { dirname, join } from 'node:path';

const allowed = new Set((process.argv[2] ?? '').split(',').filter(Boolean));
if (allowed.size === 0) {
  console.error('usage: node scripts/web-licences.mjs <comma-separated SPDX ids>');
  process.exit(2);
}

const readJSON = (file) => JSON.parse(readFileSync(file, 'utf8'));

// Resolves a dependency the way Node does: walk up from the dependant.
function resolve(name, from) {
  for (let dir = from; ; dir = dirname(dir)) {
    const candidate = join(dir, 'node_modules', name, 'package.json');
    if (existsSync(candidate)) return dirname(realpathSync(candidate));
    if (dir === dirname(dir)) return null;
  }
}

const root = realpathSync('web');
const seen = new Map();
const rootPkg = readJSON(join(root, 'package.json'));
const queue = Object.keys(rootPkg.dependencies ?? {}).map((name) => [name, root, false]);
if (queue.length === 0) {
  console.error('licences: FAIL - web: package.json lists no production dependency');
  process.exit(1);
}

while (queue.length > 0) {
  const [name, from, optional] = queue.pop();
  const dir = resolve(name, from);
  if (dir === null) {
    // An optional package for another platform is legitimately absent.
    if (optional) continue;
    console.error(`licences: FAIL - web: cannot find ${name} (required from ${from}); run 'make web-install'`);
    process.exit(1);
  }
  if (seen.has(dir)) continue;
  const pkg = readJSON(join(dir, 'package.json'));
  seen.set(dir, pkg);
  for (const dep of Object.keys(pkg.dependencies ?? {})) queue.push([dep, dir, false]);
  for (const dep of Object.keys(pkg.optionalDependencies ?? {})) queue.push([dep, dir, true]);
}

const violations = [];
for (const pkg of seen.values()) {
  const licence = typeof pkg.license === 'string' ? pkg.license : '';
  if (!allowed.has(licence)) violations.push(`${pkg.name}@${pkg.version}: ${licence || 'no licence declared'}`);
}

if (violations.length > 0) {
  console.error('licences: FAIL - web: production packages outside the allow-list:');
  for (const line of violations.sort()) console.error(`  ${line}`);
  process.exit(1);
}
console.log(`licences: web - ${seen.size} production packages, all allowed`);
