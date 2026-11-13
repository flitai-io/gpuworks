// Shared jsdom + i18n setup for component tests. Import it first (statically)
// and import React Testing Library and the components dynamically afterwards,
// so the globals exist before antd reads them.
import fs from "node:fs";
import { afterEach } from "node:test";
import { JSDOM } from "jsdom";
import { createInstance } from "i18next";
import { initReactI18next } from "react-i18next";

export const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
const names = [
  "window", "document", "navigator", "HTMLElement", "HTMLHtmlElement", "HTMLBodyElement", "HTMLInputElement", "HTMLTextAreaElement",
  "HTMLButtonElement", "HTMLSelectElement", "SVGElement", "Element", "Node", "NodeFilter", "MutationObserver", "ShadowRoot",
] as const;
for (const name of names) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// React's legacy input-event polyfill reaches for IE's attachEvent when an input gains focus under jsdom.
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
// rc-overflow creates a MessageChannel per update and never closes the ports, which would pin the node
// event loop; timer-based delivery leaves no live handle behind.
class ShimPort {
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  postMessage(data: unknown) { setTimeout(() => this.onmessage?.({ data }), 0); }
  close() {}
}
class ShimMessageChannel { port1 = new ShimPort(); port2 = new ShimPort(); }
Object.defineProperty(globalThis, "MessageChannel", { value: ShimMessageChannel, configurable: true });

// jsdom has no requestAnimationFrame, so zrender (ECharts) falls back to
// setTimeout(16). A progressive layer (the analytics heat map) paints at most
// 15 ms per frame and schedules the next one; on a loaded machine a paint is
// still pending when the test ends, cleanup disposes the chart, and the
// pending frame then reads the disposed painter ("Cannot read properties of
// null (reading 'layerStack')"), which node:test reports against the test
// that just finished. Frames belong to the test that scheduled them: once it
// ends they are dropped. Installed before ECharts is imported, since zrender
// binds requestAnimationFrame when its module loads.
let frameGeneration = 0;
const pendingFrames = new Map<number, ReturnType<typeof setTimeout>>();
let nextFrame = 0;
Object.defineProperties(dom.window, {
  requestAnimationFrame: {
    configurable: true,
    value: (callback: (time: number) => void) => {
      const id = ++nextFrame;
      const generation = frameGeneration;
      pendingFrames.set(id, setTimeout(() => {
        pendingFrames.delete(id);
        if (generation === frameGeneration) callback(performance.now());
      }, 16));
      return id;
    },
  },
  cancelAnimationFrame: {
    configurable: true,
    value: (id: number) => {
      clearTimeout(pendingFrames.get(id));
      pendingFrames.delete(id);
    },
  },
});
// Zeron's popups (Base UI, framer-motion) call the bare global, not window's.
for (const name of ["requestAnimationFrame", "cancelAnimationFrame"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
/** Frames scheduled and not yet run (or dropped); a test can wait for 0. */
export const pendingFrameCount = () => pendingFrames.size;
afterEach(() => {
  frameGeneration += 1;
});

/** Loads the real English locale files of the given namespaces (common is always included). */
export async function createI18n(namespaces: string[], lng: "en" | "zh" = "en") {
  const all = [...new Set(["common", ...namespaces])];
  const resources: Record<string, Record<string, unknown>> = { [lng]: {} };
  for (const ns of all) {
    resources[lng][ns] = JSON.parse(fs.readFileSync(new URL(`../../src/i18n/locales/${lng}/${ns}.json`, import.meta.url), "utf8"));
  }
  const i18n = createInstance();
  await i18n.use(initReactI18next).init({
    lng, fallbackLng: lng, resources, ns: all, defaultNS: namespaces[0] ?? "common", interpolation: { escapeValue: false },
  });
  return { i18n, locale: resources[lng] as Record<string, Record<string, any>> };
}
