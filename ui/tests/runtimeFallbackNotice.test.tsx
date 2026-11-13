import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://gpuworks.example/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      nodes: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { default: RuntimeFallbackNotice, runtimeFallbackCommand } = await import("../src/components/RuntimeFallbackNotice.tsx");

const { FeedbackHost, message: zeronMessage } = await import("../src/ui-kit/feedback");

afterEach(() => { cleanup(); zeronMessage.destroy(); });

const plugin = "NVIDIA Container Toolkit";
const renderNotice = (node: Parameters<typeof RuntimeFallbackNotice>[0]["node"]) =>
  render(<I18nextProvider i18n={i18n}><FeedbackHost /><RuntimeFallbackNotice node={node} /></I18nextProvider>);

test("the fix offered follows why the runtime was passed over", () => {
  const missing = runtimeFallbackCommand({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_plugin_missing" });
  assert.match(missing, /apt-get install -y nvidia-container-toolkit/);
  assert.match(missing, /nvidia-ctk runtime configure --runtime=docker/);
  const unconfigured = runtimeFallbackCommand({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_runtime_unconfigured" });
  assert.equal(unconfigured, "sudo nvidia-ctk runtime configure --runtime=docker\nsudo systemctl restart docker");
  // nerdctl consumes the toolkit directly: installing it is the whole fix.
  const nerdctl = runtimeFallbackCommand({ gpu_vendor: "nvidia", runtime_fallback_from: "nerdctl", runtime_fallback_reason: "gpu_plugin_missing" });
  assert.match(nerdctl, /nvidia-container-toolkit/);
  assert.doesNotMatch(nerdctl, /runtime configure/);
  assert.equal(runtimeFallbackCommand({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_ready" }), "");
});

test("nothing is shown on a node that passed nothing over", () => {
  const { container } = renderNotice({ gpu_vendor: "nvidia" });
  assert.equal(container.textContent, "");
  assert.equal(screen.queryByRole("alert"), null);
});

test("a missing toolkit warns that the node runs runveil and offers the install command", async () => {
  let copied = "";
  Object.defineProperty(globalThis.navigator, "clipboard", { value: { writeText: async (text: string) => { copied = text; } }, configurable: true });
  renderNotice({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_plugin_missing", runtime_fallback_plugin: plugin });
  assert.equal(screen.getByRole("alert").textContent,
    "docker is installed but NVIDIA Container Toolkit is not, so this node uses runveil. Install NVIDIA Container Toolkit; the node switches back to docker once it is idle.");
  fireEvent.click(screen.getByRole("button", { name: /Copy install command/ }));
  await screen.findByText("Plugin install command copied");
  assert.match(copied, /nvidia-container-toolkit/);
});

test("an unconfigured daemon names the configure step, with its own command", () => {
  renderNotice({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_runtime_unconfigured", runtime_fallback_plugin: plugin });
  assert.match(screen.getByRole("alert").textContent ?? "", /not configured for GPUs, so this node uses runveil/);
  assert.ok(screen.getByRole("button", { name: /Copy configure command/ }));
  assert.equal(screen.queryByRole("button", { name: /Copy install command/ }), null);
});

test("a ready runtime is news, not a warning, and offers no command", () => {
  renderNotice({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_ready", runtime_fallback_plugin: plugin });
  assert.equal(screen.queryByRole("alert"), null);
  assert.equal(screen.getByRole("status").textContent, "docker is ready and can be switched to; the node switches automatically once it is idle.");
  assert.equal(screen.queryByRole("button"), null);
});

test("a reason this client does not know is shown as its code rather than as nothing", () => {
  renderNotice({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_something_new" });
  assert.equal(screen.getByRole("alert").textContent, "docker: gpu_something_new");
});

test("a daemon that did not answer at startup warns, says the agent retries, and offers no command", () => {
  renderNotice({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "daemon_not_ready" });
  assert.equal(screen.getByRole("alert").textContent,
    "docker is installed but did not answer when the agent started (still starting or busy), so this node uses runveil for now. The agent retries every minute and switches back to docker once it answers and the node is idle.");
  assert.equal(screen.queryByRole("button"), null);
  assert.equal(runtimeFallbackCommand({ gpu_vendor: "nvidia", runtime_fallback_from: "docker", runtime_fallback_reason: "daemon_not_ready" }), "");
});
