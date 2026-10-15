import { resolve } from 'node:path';

import { ESLint } from 'eslint';
import { describe, expect, it } from 'vitest';

// The console's own lint configuration, as `pnpm lint` runs it.
const eslint = new ESLint({ cwd: resolve(import.meta.dirname, '../..') });

/** The messages of the rules that refuse markup, for code in a file of that name. */
async function refusalsOf(code: string, filePath: string): Promise<string[]> {
  const [result] = await eslint.lintText(code, { filePath });
  // Code that does not parse would have no refusal to find either.
  expect(result?.fatalErrorCount).toBe(0);
  return (result?.messages ?? [])
    .filter((message) => message.ruleId === 'no-restricted-syntax' || message.ruleId === 'no-restricted-properties')
    .map((message) => message.message);
}

const SOURCE = 'src/preview/Sample.tsx';
const TEST = 'src/preview/Sample.test.tsx';

describe('the rule that nothing is inserted as markup (ADR-0028)', () => {
  // Each case names what it shows in words, not by the name of a sink: a
  // string that is exactly such a name is refused here as anywhere.
  it.each([
    ['the React attribute', 'export const A = () => <div dangerouslySetInnerHTML={{ __html: "x" }} />;'],
    ['the attribute in a JSX spread', 'export const A = (h: string) => <div {...{ dangerouslySetInnerHTML: { __html: h } }} />;'],
    ['the attribute through createElement', 'import { createElement } from "react"; export const a = (x: { __html: string }) => createElement("div", { dangerouslySetInnerHTML: x });'],
    ['the iframe attribute', 'export const A = () => <iframe title="t" srcDoc="x" />;'],
    ['the iframe attribute in a JSX spread', 'export const A = (srcdoc: string) => <iframe title="t" {...{ srcdoc }} />;'],
    ['the iframe property', 'export function a(f: HTMLIFrameElement) { f.srcdoc = "x"; }'],
    ['the iframe attribute set by name', 'export function a(f: Element) { f.setAttribute("srcdoc", "x"); }'],
    ['writing the inner markup', 'export function a(e: Element) { e.innerHTML = "x"; }'],
    ['reading the inner markup', 'export function a(e: Element) { return e.innerHTML; }'],
    ['the inner markup by a string', 'export function a(e: Element) { e["innerHTML"] = "x"; }'],
    ['the inner markup by a template', 'export function a(e: Element) { e[`innerHTML`] = "x"; }'],
    ['the inner markup through Object.assign', 'export function a(e: Element, s: string) { Object.assign(e, { innerHTML: s }); }'],
    ['the inner markup through Reflect.set', 'export function a(e: Element, s: string) { Reflect.set(e, "innerHTML", s); }'],
    ['the outer markup', 'export function a(e: Element) { e.outerHTML = "x"; }'],
    ['inserting markup', 'export function a(e: Element) { e.insertAdjacentHTML("beforeend", "x"); }'],
    ['setting markup unsafely', 'export function a(e: Element, s: string) { e.setHTMLUnsafe(s); }'],
    ['parsing markup unsafely', 'export const a = (s: string) => Document.parseHTMLUnsafe(s);'],
    ['a contextual fragment', 'export const a = (r: Range, s: string) => r.createContextualFragment(s);'],
    ['document.write', 'export function a() { document.write("x"); }'],
    ['document.writeln', 'export function a() { document.writeln("x"); }'],
    ['document.write by a string', 'export function a() { document["write"]("x"); }'],
    ['window.document.write', 'export function a() { window.document.write("x"); }'],
    ['an element\'s ownerDocument.write', 'export function a(e: Element) { e.ownerDocument.writeln("x"); }'],
    ['the document under another name', 'export function a() { const d = document; d.write("x"); }'],
    ['window.document under another name', 'export function a() { const d = window.document; d.write("x"); }'],
  ])('refuses %s in the console', async (_, code) => {
    expect(await refusalsOf(code, SOURCE)).toHaveLength(1);
  });

  it.each([
    ['text nodes', 'export function a(e: Element) { e.textContent = "x"; }'],
    ['another attribute', 'export function a(e: Element) { e.setAttribute("title", "x"); }'],
    ['a sink name inside a longer string', 'export const a = "the innerHTML of a node";'],
    ['writing to a stream', 'export const a = (w: WritableStreamDefaultWriter) => w.write(new Uint8Array(1));'],
    ['an element of the document', 'export const a = () => document.getElementById("root");'],
  ])('leaves %s alone', async (_, code) => {
    expect(await refusalsOf(code, SOURCE)).toEqual([]);
  });

  it('lets a test read the page as markup, and not write it so', async () => {
    expect(await refusalsOf('export const seen = document.body.innerHTML;', TEST)).toEqual([]);
    expect(await refusalsOf('document.body.innerHTML = "<b>x</b>";', TEST)).toHaveLength(1);
    expect(await refusalsOf('document.body.insertAdjacentHTML("beforeend", "x");', TEST)).toHaveLength(1);
    expect(await refusalsOf('document.write("x");', TEST)).toHaveLength(1);
    expect(await refusalsOf('Object.assign(document.body, { innerHTML: "x" });', TEST)).toHaveLength(1);
  });
});
