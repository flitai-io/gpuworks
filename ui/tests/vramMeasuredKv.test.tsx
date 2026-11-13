// The deployment detail page is the only place a user can see what an instance
// actually got, so the engine's own KV pool size has to be legible there and not
// buried in the bar's GiB segments. These cases pin the reported figure, the
// line that carries it, and that it stays absent rather than showing a
// confident zero when the engine never reported one.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
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
  resources: { en: { deploy: { vram: {
    title: "VRAM Allocation",
    measured: "Measured",
    actualTotal: "Actual measured usage: {{value}} GiB",
    estimated: "Estimated",
    measuredCalibration: "Measured Calibration",
    measuredKvSummary: "KV pool {{value}} GiB · {{tokens}} tokens reported by the engine at startup",
    kvPoolSummary: "KV pool {{value}} GiB · estimated {{tokens}} tokens",
    kvPoolUnavailable: "KV pool {{value}} GiB · token capacity unavailable ({{reason}})",
    kvPoolNotApplicable: "KV pool {{value}} GiB · this model keeps no KV cache",
    context: "Context {{value}}",
    maxConcurrency: "Estimated maximum concurrency: {{value}}",
    utilization: "{{value}}% used",
    confidence: "{{value}}",
    partitionCapacity: "{{value}} GiB partition",
    fitFits: "fits",
    fitNoFit: "does not fit",
    segment: JSON.parse(readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8")).vram.segment,
    kvReason: {
      kv_no_measurement: "no measured KV cost for this model",
      kv_startup_exceeds_capacity: "it does not fit even before the KV cache",
      kv_not_applicable: "this model keeps no KV cache",
      unknown: "reason not reported",
    },
    grade: { measured: "measured", rough: "rough", notApplicable: "n/a" },
  } } } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: VRAMBreakdown } = await import("../src/components/VRAMBreakdown.tsx");

const GIB = 1024 ** 3;

const breakdown = {
  grade: "exact",
  kv_bytes_per_token: 114688,
  per_gpu_bytes: 2 * GIB,
  startup_fixed_bytes: 2 * GIB,
  total_bytes: 2 * GIB,
  actual_total_bytes: 2 * GIB,
  weight_bytes: GIB,
  kv_pool_bytes: 20 * GIB,
  estimated_kv_tokens: 187245,
  fits: true,
  tensor_parallel: 1,
  max_model_len: 4096,
  source: "estimate",
  reserve_bytes: 50 * 1024 ** 2,
  mm_encoder_bytes: 0,
} as never;

const measured = {
  ModelWeightsGiB: 1.12,
  KVCacheGiB: 38.39,
  KVCacheTokens: 359392,
  GraphCaptureGiB: 0,
  MaxConcurrency: 87.74,
  NumGPUBlocks: 22462,
  Found: true,
} as never;

function renderBreakdown(breakdownValue: unknown, measuredValue?: unknown) {
  render(
    React.createElement(I18nextProvider, { i18n },
      React.createElement(VRAMBreakdown, { breakdown: breakdownValue as never, measured: measuredValue as never })),
  );
}

function draw(measuredValue: unknown) {
  renderBreakdown(breakdown, measuredValue);
}

afterEach(() => cleanup());

test("renders the KV pool the engine reported, with its GiB figure", () => {
  draw(measured);
  // toLocaleString groups thousands, which is what makes a six-digit pool readable.
  assert.ok(screen.getByText(/KV pool 38\.4 GiB · 359,392 tokens reported by the engine at startup/),
    "measured KV line missing or unformatted");
  assert.ok(screen.getByText("Measured"), "measured bar label missing");
});

test("a measured instance still describes its VRAM usage below the bar when the engine did not report KV tokens", () => {
  renderBreakdown({ ...breakdown, source: "measured", capacity_bytes: 128 * GIB }, {
    ...measured, ModelWeightsGiB: 76.9, KVCacheGiB: 0, KVCacheTokens: 0,
  });
  const usage = screen.getByText(/Actual measured usage: 76.9 \/ 128.0 GiB/);
  assert.match(usage.textContent ?? "", /60% used/);
  assert.ok(screen.getByTitle("Weights 76.90 GiB").compareDocumentPosition(usage) & Node.DOCUMENT_POSITION_FOLLOWING);
  assert.equal(screen.queryByText(/reported by the engine/), null);
});

test("each measured allocation is named and sized against the same GPU capacity", () => {
  renderBreakdown({ ...breakdown, source: "measured", capacity_bytes: 20 * GIB }, {
    ...measured, ModelWeightsGiB: 12, KVCacheGiB: 4, GraphCaptureGiB: 1,
  });
  assert.equal(screen.getByTitle("Weights 12.00 GiB").style.width, "60%");
  assert.equal(screen.getByTitle("KV cache 4.00 GiB").style.width, "20%");
  assert.equal(screen.getByTitle("CUDA graph capture 1.00 GiB").style.width, "5%");
  assert.ok(screen.getByText(/Actual measured usage: 17.0 \/ 20.0 GiB/));
});

test("renders the estimate when no measurement is available", () => {
  draw({ ...(measured as Record<string, unknown>), Found: false });
  assert.equal(screen.queryByText(/reported by the engine/), null,
    "an instance with no measurement must not claim one");
  assert.ok(screen.getByText(/estimated 187,245 tokens/), "estimated KV line missing");
});

test("a computed concurrency of zero is shown, and an uncomputed one stays absent", () => {
  renderBreakdown({ ...breakdown, fits: false, max_concurrency: 0 });
  assert.ok(screen.getByText("Estimated maximum concurrency: 0"));
  cleanup();
  renderBreakdown(breakdown);
  assert.equal(screen.queryByText(/maximum concurrency/), null);
});

test("an engine that reported no token count is not rendered as zero", () => {
  draw({ ...(measured as Record<string, unknown>), KVCacheTokens: 0 });
  assert.equal(screen.queryByText(/reported by the engine/), null,
    "a zero count is silence, not a measured empty pool");
});

// The server names why a capacity is missing as a stable code, so the sentence
// comes from the reader's locale. The bug this guards: a server sentence
// interpolated into a translated frame, which rendered a Chinese page with
// "暂无法估算 token 容量（no measured KV footprint for this model）".
test("a missing KV capacity states its reason in the reader's language", () => {
  renderBreakdown({ ...breakdown, estimated_kv_tokens: 0, kv_estimate_reason: "kv_no_measurement" });
  const line = screen.getByText(/token capacity unavailable/);
  assert.match(line.textContent ?? "", /no measured KV cost for this model/);
  assert.doesNotMatch(line.textContent ?? "", /kv_no_measurement/);
});

test("a reason the client does not know falls back to its own words, not the server's", () => {
  renderBreakdown({ ...breakdown, estimated_kv_tokens: 0, kv_estimate_reason: "kv_something_new" });
  const line = screen.getByText(/token capacity unavailable/);
  assert.match(line.textContent ?? "", /reason not reported/);
  assert.doesNotMatch(line.textContent ?? "", /kv_something_new/);
});
