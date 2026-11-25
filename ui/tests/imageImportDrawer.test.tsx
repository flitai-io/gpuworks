import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const imagesLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { images: imagesLocale } },
  defaultNS: "images",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { Form } = await import("antd");
const ImageImportDrawer = (await import("../src/pages/images/ImageImportDrawer.tsx")).default;

afterEach(() => cleanup());
after(() => {
  closeChannels();
  dom.window.close();
});

function Harness() {
  const [form] = Form.useForm();
  return React.createElement(ImageImportDrawer, {
    open: true,
    onClose: () => {},
    form,
    importing: false,
    importCustom: () => {},
    hasExplicitTag: () => true,
    sourceTab: "custom",
    versions: [],
    onArchiveURLMatched: () => {},
  });
}

// A custom image row records what the image is and where to get it. The form
// used to offer an "adapted model" declaration that the import request never
// sent, so whatever the user picked there was silently dropped.
test("the custom image form offers no adapted model fields", () => {
  render(React.createElement(I18nextProvider, { i18n }, React.createElement(Harness)));

  assert.ok(screen.getByText("Add Custom Image"));
  // The neighbouring fields are still offered.
  assert.ok(screen.getByText("Acquisition addresses"));
  assert.ok(document.getElementById("registry_sources"), "registry sources field");
  assert.ok(document.getElementById("series_name"), "series name field");
  assert.ok(document.getElementById("framework"), "framework field");

  // The adapted model declaration is gone: no fields, no notice, no labels.
  // Compare presence as booleans: a failing assertion on a DOM node would try
  // to print the whole jsdom graph.
  // Purpose is the server's default and series defaults are not edited here.
  for (const id of ["adapted_model_id", "adapted_model_label", "purpose", "common_args_rows", "common_env_rows"]) {
    assert.equal(document.getElementById(id) !== null, false, `unexpected field ${id}`);
  }
  for (const text of [/adapted model/i, /platform model/i, /import\.(adapted|platformModel)/]) {
    assert.equal(screen.queryByText(text) !== null, false, `unexpected text ${text}`);
  }
});

test("the image reference uses a human-readable label instead of its wire key", () => {
  render(React.createElement(I18nextProvider, { i18n }, React.createElement(Harness)));
  assert.ok(screen.getByRole("textbox", { name: "Image Reference", exact: true }));
  assert.equal(screen.queryByText("image_ref") === null, true);
  assert.equal(screen.queryByText(/registry_sources/) === null, true, "help text must use the acquisition-address label");
  assert.ok(screen.getByPlaceholderText("Leave blank to group automatically by the image repository"));
});

test("the custom image form can select Kunlun XRE without losing existing frameworks", async () => {
  render(React.createElement(I18nextProvider, { i18n }, React.createElement(Harness)));

  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Framework", exact: true }));
  for (const framework of ["CUDA", "CANN", "ROCm", "HGGC", "DTK", "Metal", "XRE"]) {
    assert.equal((await screen.findByText(framework, { exact: true })) !== null, true);
  }
  fireEvent.click(screen.getByText("XRE", { exact: true }));
  const selected = document.querySelector(".ant-select-selection-item[title='XRE']");
  assert.equal(selected?.textContent, "XRE");
});
