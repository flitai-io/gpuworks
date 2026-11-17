import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { installCanvas } from "./helpers/analyticsFixtures.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { cursorPage } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class EventSourceStub { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });

const read = (language: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: Object.fromEntries(["en", "zh"].map((language) => [language, Object.fromEntries(["deployments", "common", "ai", "networks"].map((ns) => [ns, read(language, ns)]))])),
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstancePanel } = await import("../src/pages/InstancePanel.tsx");
const originalFetch = globalThis.fetch;
const originalInstanceEvents = api.instanceEvents;

afterEach(async () => {
  cleanup();
  globalThis.fetch = originalFetch;
  api.instanceEvents = originalInstanceEvents;
  await i18n.changeLanguage("en");
});

// What GET /instances/{uuid} sends for an instance whose engine exited at
// startup (internal/server/api/instance_runtime_failure.go).
const failed = {
  uuid: "failed-instance", deployment_id: 1, node_uuid: "node-1", state: "failed", phase: "health_check",
  gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image", container_id: "", sleep_level: 0,
  message: "container exited (code 2): ValueError: max seq len too large",
  runtime_failure: {
    code: "CONTAINER_EXITED",
    params: { exit_code: 2, phase: "health_check", evidence: ["ValueError: max seq len too large"], causes: [{ code: "KV_CACHE_INSUFFICIENT", params: {}, message: "kv cache insufficient" }], vendor: "", detail: "" },
    message: "container exited (code 2): ValueError: max seq len too large",
  },
};

function renderPanel(inst: object) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst as never} /></MemoryRouter></I18nextProvider>);
}

test("a failed instance shows its reason from the code in the reader's language", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => new Response("[]")) as typeof fetch;
  const english = renderPanel(failed);
  assert.ok(screen.getByText("The container exited with code 2. Output shows: KV cache does not fit. Last error: ValueError: max seq len too large"));
  assert.equal(screen.queryByText(failed.message), null);
  english.unmount();

  await i18n.changeLanguage("zh");
  renderPanel(failed);
  assert.ok(screen.getByText("容器已退出，退出码 2。输出显示：KV cache 放不下。最后的错误：ValueError: max seq len too large"));
});

test("a failed instance from an older agent shows its message", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => new Response("[]")) as typeof fetch;
  renderPanel({ ...failed, runtime_failure: undefined, message: "container exited (code 2) with no output" });
  assert.ok(screen.getByText("container exited (code 2) with no output"));
});

test("the startup timeline's failed stage shows the event's coded reason", async () => {
  api.instanceEvents = async () => cursorPage([
    { id: 2, kind: "state", code: "INSTANCE_STATUS_UPDATED", created_at: "2026-11-14T00:00:02Z",
      params: { state: "failed", phase: "", prev_state: "starting", prev_phase: "health_check", detail: "health check timed out: from=node",
        failure: { code: "ENGINE_HEALTH_TIMEOUT", params: { phase: "health_check" }, message: "health check timed out: from=node" } } },
    { id: 1, kind: "state", code: "INSTANCE_STATUS_UPDATED", created_at: "2026-11-14T00:00:01Z", params: { state: "starting", phase: "health_check", detail: "waiting for engine health" } },
  ]);
  globalThis.fetch = (async () => new Response("[]")) as typeof fetch;
  renderPanel({ ...failed, runtime_failure: { code: "ENGINE_HEALTH_TIMEOUT", params: { phase: "health_check" }, message: "health check timed out: from=node" }, message: "health check timed out: from=node" });
  const rows = await screen.findAllByText("The engine did not become healthy before the start timeout.");
  assert.ok(rows.length >= 1);
  assert.equal(screen.queryByText("health check timed out: from=node"), null);
});
