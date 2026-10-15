import js from '@eslint/js';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

// What would put text from elsewhere into the page as markup (ADR-0028): the
// console draws what it is given as React elements and text nodes, never as
// HTML. The names of the sinks are refused wherever the code can name them:
// as a property (`el.innerHTML`), a key of an object (`{ innerHTML: s }`,
// `createElement('div', { dangerouslySetInnerHTML })`, a JSX spread), a JSX
// attribute, and any string or plain template that is exactly such a name
// (`el['innerHTML']`, `Reflect.set(el, 'innerHTML', s)`,
// `el.setAttribute('srcdoc', s)`). What no rule of syntax can see: a name made
// at run time (`el['inner' + 'HTML']`), or a sink reached through a value the
// code did not name - those are for review.
const SINKS =
  /^(innerHTML|outerHTML|insertAdjacentHTML|src[dD]oc|setHTMLUnsafe|parseHTMLUnsafe|createContextualFragment|dangerouslySetInnerHTML)$/;
const WRITE = /^write(ln)?$/;
const MARKUP_MESSAGE =
  'Nothing is ever inserted as markup (ADR-0028): build React elements and text nodes instead.';
const ALIAS_MESSAGE =
  'The document is not given another name: the rule that refuses document.write (ADR-0028) could not follow it.';

/** The sink names as keys, attributes, strings and templates: refused in tests too. */
const SINK_NAMES = [
  { selector: `JSXAttribute[name.name=${String(SINKS)}]`, message: MARKUP_MESSAGE },
  { selector: `Property[key.name=${String(SINKS)}]`, message: MARKUP_MESSAGE },
  { selector: `Literal[value=${String(SINKS)}]`, message: MARKUP_MESSAGE },
  {
    selector: `TemplateLiteral[expressions.length=0] > TemplateElement[value.cooked=${String(SINKS)}]`,
    message: MARKUP_MESSAGE,
  },
];

/**
 * `document.write` and `writeln` however the document is reached by name -
 * `document.write` itself is refused by no-restricted-properties - and the
 * document never under another name, so that it cannot be reached otherwise.
 */
const DOCUMENT_WRITES = [
  {
    selector: `MemberExpression[property.name=${String(WRITE)}][object.type='MemberExpression'][object.property.name=/^(document|ownerDocument)$/]`,
    message: MARKUP_MESSAGE,
  },
  {
    selector: `MemberExpression[property.name=${String(WRITE)}][object.name='ownerDocument']`,
    message: MARKUP_MESSAGE,
  },
  {
    // `document['write']` itself is no-restricted-properties'.
    selector: `MemberExpression[computed=true][property.value=${String(WRITE)}][object.name='ownerDocument']`,
    message: MARKUP_MESSAGE,
  },
  {
    selector: `MemberExpression[computed=true][property.value=${String(WRITE)}][object.property.name=/^(document|ownerDocument)$/]`,
    message: MARKUP_MESSAGE,
  },
  { selector: "VariableDeclarator[init.type='Identifier'][init.name='document']", message: ALIAS_MESSAGE },
  {
    selector: "VariableDeclarator[init.type='MemberExpression'][init.property.name=/^(document|ownerDocument)$/]",
    message: ALIAS_MESSAGE,
  },
  { selector: "AssignmentExpression[right.type='Identifier'][right.name='document']", message: ALIAS_MESSAGE },
];

/** Every use of a markup sink, in the console's own code. */
const NO_MARKUP = [
  ...SINK_NAMES,
  ...DOCUMENT_WRITES,
  { selector: `MemberExpression[computed=false][property.name=${String(SINKS)}]`, message: MARKUP_MESSAGE },
];

/**
 * In tests the page may be read as markup - to show that a secret is nowhere
 * in it - but nothing is written into it so.
 */
const NO_MARKUP_WRITES = [
  ...SINK_NAMES,
  ...DOCUMENT_WRITES,
  {
    selector: `AssignmentExpression > MemberExpression.left[computed=false][property.name=${String(SINKS)}]`,
    message: MARKUP_MESSAGE,
  },
  {
    selector: `CallExpression > MemberExpression.callee[computed=false][property.name=${String(SINKS)}]`,
    message: MARKUP_MESSAGE,
  },
];

const NO_DOCUMENT_WRITE = [
  { object: 'document', property: 'write', message: MARKUP_MESSAGE },
  { object: 'document', property: 'writeln', message: MARKUP_MESSAGE },
];

export default tseslint.config(
  // src/api/schema.d.ts is generated from api/openapi/admin.yaml (pnpm gen:api).
  { ignores: ['dist', 'coverage', 'node_modules', 'src/api/schema.d.ts'] },
  js.configs.recommended,
  ...tseslint.configs.strict,
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2023,
      globals: globals.browser,
    },
    plugins: { 'react-hooks': reactHooks },
    rules: {
      ...reactHooks.configs.recommended.rules,
    },
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-syntax': ['error', ...NO_MARKUP],
      'no-restricted-properties': ['error', ...NO_DOCUMENT_WRITE],
    },
  },
  {
    files: ['src/**/*.test.{ts,tsx}', 'src/test/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-syntax': ['error', ...NO_MARKUP_WRITES],
    },
  },
  {
    files: ['*.config.{js,ts}'],
    languageOptions: { globals: globals.node },
  },
);
