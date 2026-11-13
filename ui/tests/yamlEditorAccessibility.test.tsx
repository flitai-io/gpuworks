import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createRequire } from "node:module";

// Render the actual CodeMirror editor, which the normal browser uses; the
// detail-page suite exercises its separate plain-textarea fallback.
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/", pretendToBeVisual: true });
for (const name of ["window", "document", "navigator", "Window", "HTMLElement", "Element", "Node", "Text", "Range", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: dom.window.getComputedStyle.bind(dom.window), configurable: true });
Object.defineProperty(globalThis, "requestAnimationFrame", { value: dom.window.requestAnimationFrame.bind(dom.window), configurable: true });
Object.defineProperty(globalThis, "cancelAnimationFrame", { value: dom.window.cancelAnimationFrame.bind(dom.window), configurable: true });
Object.defineProperties(dom.window.Range.prototype, {
  getClientRects: { configurable: true, value: () => [] },
  getBoundingClientRect: { configurable: true, value: () => new dom.window.DOMRect() },
});
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
// The schema dependency exposes its Node-compatible entry through CommonJS.
const { default: YamlEditor } = createRequire(import.meta.url)("../src/pages/yamlEditor.tsx") as typeof import("../src/pages/yamlEditor.tsx");
afterEach(cleanup);
after(() => dom.window.close());

for (const [source, value] of [
  ["a compact registered local weight source", "- local: company-tuned-model\n"],
  ["an expanded registered local weight source", "- local:\n    repo_id: company-tuned-model\n    revision: trained-v2\n"],
  ["a compact Hugging Face weight source", "- huggingface: org/repo\n"],
  ["a compact ModelScope weight source", "- modelscope: org/repo\n"],
] as const) {
  test(`the real YAML editor accepts ${source}`, async () => {
    const schema = JSON.parse(fs.readFileSync(new URL("./fixtures/catalog-template/weight-source-schema.json", import.meta.url), "utf8"));
    // First observe an invalid value, so the initial panel's empty state cannot
    // count as successful validation of the actual weight source below.
    const { rerender } = render(<YamlEditor value={"local: []\n"} onChange={() => {}} schema={schema} ariaLabel="Edit weight sources YAML" />);
    const editor = screen.getByRole("textbox", { name: "Edit weight sources YAML" });
    editor.focus();
    fireEvent.keyDown(editor, { key: "M", code: "KeyM", keyCode: 77, ctrlKey: true, shiftKey: true });
    const diagnostics = await screen.findByRole("listbox", { name: "Diagnostics" }, { timeout: 5000 });
    await waitFor(() => assert.notEqual(within(diagnostics).getByRole("option").textContent, "No diagnostics"), { timeout: 5000 });
    rerender(<YamlEditor value={value} onChange={() => {}} schema={schema} ariaLabel="Edit weight sources YAML" />);
    await within(diagnostics).findByRole("option", { name: "No diagnostics" }, { timeout: 5000 });
  });
}

for (const [source, value] of [
  ["a type error for an array used as a local weight source", "- local: []\n"],
  ["an error for an absolute host path used as a local weight source", "- local: /srv/models/company-tuned-model\n"],
] as const) {
  test(`the real YAML editor reports ${source}`, async () => {
    const schema = JSON.parse(fs.readFileSync(new URL("./fixtures/catalog-template/weight-source-schema.json", import.meta.url), "utf8"));
    const { rerender } = render(<YamlEditor value={"local: []\n"} onChange={() => {}} schema={schema} ariaLabel="Edit weight sources YAML" />);
    const editor = screen.getByRole("textbox", { name: "Edit weight sources YAML" });
    editor.focus();
    fireEvent.keyDown(editor, { key: "M", code: "KeyM", keyCode: 77, ctrlKey: true, shiftKey: true });
    const diagnostics = await screen.findByRole("listbox", { name: "Diagnostics" }, { timeout: 5000 });
    await waitFor(() => assert.notEqual(within(diagnostics).getByRole("option").textContent, "No diagnostics"), { timeout: 5000 });
    const rootError = within(diagnostics).getByRole("option").textContent;
    rerender(<YamlEditor value={value} onChange={() => {}} schema={schema} ariaLabel="Edit weight sources YAML" />);
    await waitFor(() => {
      const sourceError = within(diagnostics).getByRole("option").textContent;
      assert.notEqual(sourceError, rootError, "the diagnostics must describe the current source rather than the previous root value");
      assert.notEqual(sourceError, "No diagnostics", "an invalid local source must have a visible diagnostic");
    }, { timeout: 5000 });
  });
}

test("the real YAML editing surface has its localized accessible name", () => {
  const { rerender } = render(<YamlEditor value="models: []" onChange={() => {}} ariaLabel="Edit flat YAML" />);
  const editor = screen.getByRole("textbox", { name: "Edit flat YAML" });
  assert.equal(editor.getAttribute("contenteditable"), "true");
  assert.equal(editor.textContent, "models: []");
  rerender(<YamlEditor value="models: []" onChange={() => {}} ariaLabel="编辑 YAML" />);
  assert.equal(screen.getByRole("textbox", { name: "编辑 YAML" }), editor);
  assert.equal(screen.queryByRole("textbox", { name: "Edit flat YAML" }), null);
});
