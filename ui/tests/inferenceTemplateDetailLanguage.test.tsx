import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "HTMLBodyElement", "HTMLHtmlElement", "HTMLInputElement", "HTMLTextAreaElement", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} }) });
Object.defineProperty(globalThis, "ResizeObserver", { configurable: true, value: class { observe() {} unobserve() {} disconnect() {} } });
const { cleanup, render, screen, within, fireEvent } = await import("@testing-library/react");
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: Detail } = await import("../src/pages/InferenceTemplateDetailPage.tsx");
const original = { inferenceTemplate: api.inferenceTemplate, detectedInferenceTemplateChips: api.detectedInferenceTemplateChips, nodeFacets: api.nodeFacets, evaluate: api.evaluate };
afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => dom.window.close());

for (const locale of ["en", "zh"]) {
  test(`local template weight sources describe registered weights in ${locale}`, async () => {
    const i18n = createInstance();
    const catalog = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/inferenceTemplates.json`, import.meta.url), "utf8"));
    const common = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/common.json`, import.meta.url), "utf8"));
    await i18n.use(initReactI18next).init({ lng: locale, resources: { [locale]: { inferenceTemplates: catalog, common } }, interpolation: { escapeValue: false } });
    api.inferenceTemplate = (async () => ({ id: 1, model_id: "e2e/local", source: "user", display_title: "Local fixture", specs: [{ id: 1, source: "user", name: "local-spec", engine: "vllm", image: "fixture:test", weight_sources: [{ provider: "local", repo_id: "org/private-model" }], gpus: { vendor: "nvidia" } }] })) as never;
    api.detectedInferenceTemplateChips = (async () => ({ items: [], total: 0 })) as never;
    api.nodeFacets = (async () => ({ total: 0, labels: [], system_labels: [], gpus: [] })) as never;
    api.evaluate = (async (request) => {
      assert.equal(request.template_id, 1);
      assert.equal(request.template_spec_name, "local-spec");
      return { ok: true, command: ["vllm", "serve", "/model"], argv: [], image: "fixture:test" };
    }) as typeof api.evaluate;
    render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/inference-templates/e2e/local"]}><Routes><Route path="/inference-templates/*" element={<Detail />} /></Routes></MemoryRouter></I18nextProvider>);
    await screen.findByRole("heading", { name: "Local fixture" });
    fireEvent.click(screen.getByRole("button", { name: locale === "zh" ? /详\s*情/ : catalog.flat.details }));
    await screen.findByText(locale === "en" ? "Registered local weights" : "已注册的本地权重");
    assert.ok(screen.getByText("org/private-model"));
  });

  test(`template detail metadata labels and source are localized in ${locale}`, async () => {
    const i18n = createInstance();
    const catalog = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/inferenceTemplates.json`, import.meta.url), "utf8"));
    const common = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/common.json`, import.meta.url), "utf8"));
    await i18n.use(initReactI18next).init({ lng: locale, resources: { [locale]: { inferenceTemplates: catalog, common } }, interpolation: { escapeValue: false } });
    api.inferenceTemplate = (async () => ({ id: 1, model_id: "e2e/language", source: "user", display_title: "Language fixture", input_modalities: ["text"], specs: [{ id: 1, source: "user", name: "language-spec", engine: "vllm", image: "fixture:test", weight_sources: [], gpus: { vendor: "nvidia" } }] })) as never;
    api.detectedInferenceTemplateChips = (async () => ({ items: [], total: 0 })) as never;
    api.nodeFacets = (async () => ({ total: 0, labels: [], system_labels: [], gpus: [] })) as never;
    render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/inference-templates/e2e/language"]}><Routes><Route path="/inference-templates/*" element={<Detail />} /></Routes></MemoryRouter></I18nextProvider>);
    await screen.findByRole("heading", { name: "Language fixture" });
    assert.ok(screen.getByText(locale === "en" ? "Operations" : "推理能力", { exact: true }));
    assert.ok(screen.getByText(locale === "en" ? "Input modalities" : "输入模态", { exact: true }));
    const sourceRow = screen.getByText(catalog.detail.source, { exact: true }).closest("tr")!;
    assert.ok(within(sourceRow).getByText(catalog.flat.userSource, { exact: true }));
    assert.equal(screen.queryByText("user", { exact: true }), null);
    fireEvent.click(screen.getByRole("button", { name: catalog.editor.deleteSpec.replace("{{name}}", "language-spec") }));
    const confirm = await screen.findByRole("tooltip");
    assert.ok(within(confirm).getByRole("button", { name: locale === "en" ? "Cancel" : /取\s*消/ }));
    assert.equal(within(confirm).queryByText("editor.cancel"), null);
  });
}
