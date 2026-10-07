// Validates an AsyncAPI document against the official AsyncAPI 3.0.0 JSON
// Schema, and checks what the schema does not: that every local $ref resolves
// and that channel addresses follow the naming rule of the project. Run
// through `make contracts`; the packages it imports are development
// dependencies of web/, the repository's one Node package.
//
//   node scripts/asyncapi-validate.mjs api/asyncapi/events.yaml
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

// Resolve packages from web/node_modules, wherever this is run from.
const require = createRequire(new URL('../web/package.json', import.meta.url));
const Ajv = require('ajv');
const addFormats = require('ajv-formats');
const { parse } = require('yaml');

const file = process.argv[2];
if (!file) {
  console.error('usage: node scripts/asyncapi-validate.mjs <document.yaml>');
  process.exit(2);
}

const problems = [];
let doc;
try {
  doc = parse(readFileSync(file, 'utf8'));
} catch (err) {
  console.error(`asyncapi: FAIL - ${file}: ${err.message}`);
  process.exit(1);
}
if (doc === null || typeof doc !== 'object' || doc.asyncapi !== '3.0.0') {
  console.error(`asyncapi: FAIL - ${file}: not an AsyncAPI 3.0.0 document`);
  process.exit(1);
}

const schema = require('@asyncapi/specs/schemas/3.0.0-without-$id.json');
const ajv = new (Ajv.default ?? Ajv)({ allErrors: true, strict: false });
(addFormats.default ?? addFormats)(ajv);
const validate = ajv.compile(schema);
if (!validate(doc)) {
  for (const e of validate.errors ?? []) problems.push(`${e.instancePath || '/'}: ${e.message}`);
}

// Every local reference must lead somewhere.
const lookup = (ref) =>
  ref
    .slice(2)
    .split('/')
    .reduce((node, key) => node?.[key.replace(/~1/g, '/').replace(/~0/g, '~')], doc);
const walk = (node, path) => {
  if (Array.isArray(node)) {
    node.forEach((value, i) => walk(value, `${path}/${i}`));
  } else if (node !== null && typeof node === 'object') {
    if (typeof node.$ref === 'string' && (!node.$ref.startsWith('#/') || lookup(node.$ref) === undefined)) {
      problems.push(`${path}: $ref ${node.$ref} does not resolve`);
    }
    for (const [key, value] of Object.entries(node)) walk(value, `${path}/${key}`);
  }
};
walk(doc, '');

// cactusfs.<aggregate>.<event>.v<major>, and one channel per name.
const addresses = new Set();
for (const [name, channel] of Object.entries(doc.channels ?? {})) {
  const address = channel?.address;
  if (typeof address !== 'string' || !/^cactusfs\.[a-z]+\.[a-z]+(-[a-z]+)*\.v[1-9][0-9]*$/.test(address)) {
    problems.push(`/channels/${name}: address ${JSON.stringify(address)} is not cactusfs.<aggregate>.<event>.v<major>`);
  }
  if (addresses.has(address)) problems.push(`/channels/${name}: address ${address} is used twice`);
  addresses.add(address);
}
if (addresses.size === 0) problems.push('/channels: the document defines no channel');

if (problems.length > 0) {
  for (const problem of problems) console.error(`${file}: ${problem}`);
  console.error(`asyncapi: FAIL - ${file}`);
  process.exit(1);
}
console.log(`asyncapi: ${file} is valid AsyncAPI 3.0.0 (${addresses.size} channels)`);
