// Writes an OpenAPI document that is kept as YAML in its JSON form, which the
// binary embeds and serves at /api/v1/openapi.json. Run through `make gen`;
// the YAML parser is a development dependency of web/.
//
//   node scripts/openapi-json.mjs api/openapi/admin.yaml api/openapi/admin.json
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';

// Resolve packages from web/node_modules, wherever this is run from.
const require = createRequire(new URL('../web/package.json', import.meta.url));
const { parse } = require('yaml');

const [source, target] = process.argv.slice(2);
if (!source || !target) {
  console.error('usage: node scripts/openapi-json.mjs <document.yaml> <document.json>');
  process.exit(2);
}
const doc = parse(readFileSync(source, 'utf8'));
if (!doc || typeof doc !== 'object' || !doc.openapi || !doc.paths) {
  console.error(`${source}: not an OpenAPI document`);
  process.exit(1);
}
writeFileSync(target, JSON.stringify(doc, null, 2) + '\n');
