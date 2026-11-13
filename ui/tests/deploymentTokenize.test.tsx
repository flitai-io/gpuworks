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
dom.window.HTMLElement.prototype.setPointerCapture = () => {};
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const common = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { common } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const { default: TokenizeField } = await import("../src/pages/deploy/TokenizeField.tsx");
const { TokenizeEditor } = await import("../src/pages/DeploymentTokenizeEditor.tsx");
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
import type { FormInstance } from "antd";
import type { Deployment } from "../src/api";

const original = api.setDeploymentTokenize;
afterEach(() => {
  cleanup();
  api.setDeploymentTokenize = original;
});
after(() => {
  closeChannels();
  dom.window.close();
});

const note = "vLLM and SGLang serve tokenize; another engine may not, and then the request gets the engine's own error";

test("the deploy form shows tokenize on for any engine, sends only what the operator states, and says not every engine serves it", async () => {
  let form: FormInstance | undefined;
  function Host({ engine }: { engine: string }) {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={{ engine }}>
        <Form.Item name="engine" noStyle><input type="hidden" /></Form.Item>
        <TokenizeField />
      </Form>
    );
  }
  const view = render(<I18nextProvider i18n={i18n}><Host engine="vllm" /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  const box = screen.getByRole("checkbox", { name: "Expose tokenize" }) as HTMLInputElement;
  assert.equal(box.checked, true, "unstated, tokenize is on, as the server will make it");
  assert.equal("tokenize_enabled" in buildPayload(form!.getFieldsValue(true)).payload, false, "the default is the server's");
  screen.getByText(note);

  await user.click(box);
  assert.equal(box.checked, false);
  assert.equal(buildPayload(form!.getFieldsValue(true)).payload.tokenize_enabled, false);

  // The keyboard turns it back on.
  box.focus();
  await user.keyboard(" ");
  assert.equal(box.checked, true);
  assert.equal(buildPayload(form!.getFieldsValue(true)).payload.tokenize_enabled, true);

  // Another engine keeps the box, with a note.
  view.unmount();
  render(<I18nextProvider i18n={i18n}><Host engine="llamacpp" /></I18nextProvider>);
  const other = screen.getByRole("checkbox", { name: "Expose tokenize" }) as HTMLInputElement;
  assert.equal(other.checked, true);
  assert.equal(other.disabled, false);
  screen.getByText(note);

  fireEvent.mouseEnter(screen.getByRole("img", { name: "About the tokenize API" }));
  await waitFor(() => screen.getByText(/forwards POST \/v1\/tokenize and \/v1\/detokenize/));
});

test("the detail turns tokenize off and on, shows it is busy while saving, and says why a change was refused", async () => {
  const calls: Array<[number, boolean]> = [];
  let release: (() => void) | undefined;
  let refuse = false;
  api.setDeploymentTokenize = async (id, enabled) => {
    calls.push([id, enabled]);
    await new Promise<void>((resolve) => { release = resolve; });
    if (refuse) throw new Error("deployment not found");
    return {} as Deployment;
  };
  let changed = 0;
  const dep = { id: 9, engine: "vllm", tokenize_enabled: true } as unknown as Deployment;
  const view = render(<I18nextProvider i18n={i18n}><TokenizeEditor dep={dep} onChanged={() => changed++} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  const toggle = screen.getByRole("switch", { name: "Expose tokenize" });
  assert.ok(screen.getByText("Exposed").compareDocumentPosition(toggle) & Node.DOCUMENT_POSITION_FOLLOWING);
  assert.ok(view.container.querySelector('[data-status="success"] [data-slot="badge-marker"]'));
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  await user.click(toggle);
  await waitFor(() => assert.deepEqual(calls, [[9, false]]));
  await waitFor(() => assert.equal(screen.getByRole("switch", { name: "Expose tokenize" }).getAttribute("aria-disabled"), "true", "busy while saving"));
  release!();
  await waitFor(() => assert.equal(changed, 1));

  // The detail reloads with the new state; the keyboard turns it back on.
  view.rerender(<I18nextProvider i18n={i18n}><TokenizeEditor dep={{ ...dep, tokenize_enabled: false }} onChanged={() => changed++} /></I18nextProvider>);
  const off = screen.getByRole("switch", { name: "Expose tokenize" });
  assert.ok(screen.getByText("Not exposed").compareDocumentPosition(off) & Node.DOCUMENT_POSITION_FOLLOWING);
  assert.ok(view.container.querySelector('[data-status="neutral"] [data-slot="badge-marker"]'));
  assert.equal(off.getAttribute("aria-checked"), "false");
  off.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(calls[1], [9, true]));
  release!();
  await waitFor(() => assert.equal(changed, 2));

  refuse = true;
  await user.click(screen.getByRole("switch", { name: "Expose tokenize" }));
  await waitFor(() => assert.deepEqual(calls[2], [9, true]));
  release!();
  await waitFor(() => assert.ok(dom.window.document.body.textContent?.includes("deployment not found")));
  assert.equal(changed, 2, "a refused change does not reload as if it went through");
});
