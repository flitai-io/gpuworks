import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { Instance, InstanceEvent } from "../src/api";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element", "Node", "MutationObserver"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", defaultNS: "deployments",
  resources: Object.fromEntries(["en", "zh"].map((locale) => [locale, {
    deployments: JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/deployments.json`, import.meta.url), "utf8")),
  }])),
  interpolation: { escapeValue: false },
});
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: InstanceStartupStatus } = await import("../src/pages/InstanceStartupStatus.tsx");
const originalNow = Date.now;
afterEach(async () => { cleanup(); Date.now = originalNow; await i18n.changeLanguage("en"); });

function instance(extra: Partial<Instance> = {}): Instance {
  return {
    uuid: "startup-instance", deployment_id: 1, node_uuid: "startup-node", state: "starting",
    gpu_indexes: [0], ports: [30000], api_port: 30000, image: "vllm:test", message: "", sleep_level: 0,
    ...extra,
  };
}
function show(inst: Instance, events: InstanceEvent[] = []) {
  return render(<I18nextProvider i18n={i18n}><InstanceStartupStatus inst={inst} events={events} /></I18nextProvider>);
}

test("unknown weights expose an unknown startup estimate without invented progress", () => {
  show(instance({ startup_estimate: { weight_bytes: 0, method: "missing_weight" } }));
  assert.ok(screen.getByText("Startup estimate unavailable: model weight size is unknown"));
  assert.equal(screen.queryByText(/Rough startup estimate:|Reference formula:/), null);
  assert.equal(screen.queryByRole("progressbar"), null);
});

test("the instance shows its fixed engine budget separately from the explicit reference formula", () => {
  show(instance({
    start_timeout_sec: 36030, start_timeout_source: "engine", engine_start_timeout_sec: 36000,
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 580, reference_mib_per_sec: 512, initialization_seconds: 300 },
  }));
  assert.ok(screen.getByText("Startup wait limit: 10 h 0 min 30 s · engine configuration"));
  assert.ok(screen.getByText("Rough startup estimate: 9 min 40 s"));
  assert.ok(screen.getByText("Throughput and initialization are fixed reference assumptions, not node measurements or a completion guarantee. This estimate does not change the startup wait limit."));
  assert.ok(screen.getByText("Reference formula: 143360 MiB ÷ 512 MiB/s + 5 min 0 s initialization."));
  assert.equal(screen.queryByText(/Startup estimate unavailable|comparable starts|historical/i), null);
  assert.equal(screen.queryByText(/wait limit is shorter than/), null);
});

test("an explicit shorter platform budget reports the retained engine timeout conflict", () => {
  show(instance({ start_timeout_sec: 1800, start_timeout_source: "deployment", engine_start_timeout_sec: 36000, start_timeout_conflict: true }));
  assert.ok(screen.getByText("Startup wait limit: 30 min 0 s · deployment setting"));
  assert.ok(screen.getByText("The platform may stop waiting before the engine's 10 h 0 min 0 s wait limit. Review the deployment timeout."));
});

test("stored reference parameters retain their estimate and elapsed time starts at the latest health check", () => {
  const now = Date.parse("2026-11-11T12:00:00Z");
  Date.now = () => now;
  show(instance({
    phase: "health_check", start_timeout_sec: 1800, start_timeout_source: "cluster",
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 620, reference_mib_per_sec: 256, initialization_seconds: 60 },
  }), [
    { id: 2, kind: "state", code: "INSTANCE_STATUS_UPDATED", params: { phase: "health_check" }, created_at: "2026-11-11T11:46:40Z" },
    { id: 1, kind: "state", code: "INSTANCE_STATUS_UPDATED", params: { phase: "image_pull" }, created_at: "2026-11-11T08:00:00Z" },
    { id: 0, kind: "state", data: '{"phase":"health_check"}', created_at: "2026-11-11T07:00:00Z" },
  ]);
  assert.ok(screen.getByText("Rough startup estimate: 10 min 20 s"));
  assert.ok(screen.getByText("Reference formula: 143360 MiB ÷ 256 MiB/s + 1 min 0 s initialization."));
  assert.ok(screen.getByText("Waiting for engine readiness: 13 min 20 s"));
  assert.ok(screen.getByText("Startup is taking longer than the rough estimate; still waiting within the configured limit."));
  assert.equal(screen.queryByText(/failed|timed out/i), null);
  assert.equal(screen.queryByRole("progressbar"), null);
});

test("only known measured stages are shown, including an explicitly measured zero", () => {
  show(instance({ state: "running", measured: {
    ModelWeightsGiB: 1, KVCacheGiB: 0, KVCacheTokens: 0, GraphCaptureGiB: 0,
    MaxConcurrency: 0, NumGPUBlocks: 0, Found: false,
    ModelLoadingSeconds: 4.32, CompilationSeconds: 0, StartupSeconds: 10,
  } }));
  assert.ok(screen.getByText("Observed readiness wait: 10 s"));
  assert.ok(screen.getByText("Weight loading: 4.3 s"));
  assert.ok(screen.getByText("Compilation: 0 s"));
  assert.equal(screen.queryByText(/Graph capture:/), null);
  assert.ok(screen.getByText("Engine log durations use the largest reported value per stage; stages may overlap and are not added."));
});

for (const [method, reason] of [
  ["unsupported", "Startup estimate unavailable for this deployment configuration"],
  ["unrecognized", "Startup estimate unavailable"],
  ["empirical_loading_rate", "Startup estimate unavailable"],
] as const) {
  test(`${method} does not present numeric bounds as an available estimate`, () => {
    show(instance({ startup_estimate: { weight_bytes: 140 * 1024 ** 3, method, estimated_seconds: 620, reference_mib_per_sec: 256, initialization_seconds: 60 } }));
    assert.ok(screen.getByText(reason));
    assert.equal(screen.queryByText(/Rough startup estimate:|Reference formula:/), null);
  });
}

test("a missed health-check timestamp stays unknown and does not borrow the image pull start", () => {
  show(instance({ phase: "health_check", start_timeout_sec: 1800 }), [
    { id: 1, kind: "state", params: { phase: "image_pull" }, created_at: "2026-11-11T08:00:00Z" },
  ]);
  assert.ok(screen.getByText("Readiness wait duration is unavailable"));
  assert.equal(screen.queryByText(/Waiting for engine readiness:/), null);
});

test("failed instances have no ongoing wait or slow-start message", () => {
  Date.now = () => Date.parse("2026-11-11T12:00:00Z");
  show(instance({ state: "failed", phase: "health_check", message: "container OOM-killed", start_timeout_sec: 1800,
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 620, reference_mib_per_sec: 256, initialization_seconds: 60 },
  }), [{ id: 1, kind: "state", params: { phase: "health_check" }, created_at: "2026-11-11T11:46:40Z" }]);
  assert.equal(screen.queryByText(/Waiting for engine readiness:/), null);
  assert.equal(screen.queryByText(/still waiting within/), null);
});

test("old instance responses without startup facts do not fabricate a panel", () => {
  const { container } = show(instance());
  assert.equal(container.textContent, "");
});

test("invalid estimates and unknown timing values stay unreported", () => {
  show(instance({
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: Number.NaN, reference_mib_per_sec: 256, initialization_seconds: 60 },
    measured: { ModelWeightsGiB: 1, KVCacheGiB: 0, KVCacheTokens: 0, GraphCaptureGiB: 0, MaxConcurrency: 0, NumGPUBlocks: 0, Found: false, ModelLoadingSeconds: Number.NaN, CompilationSeconds: -1, StartupSeconds: Number.POSITIVE_INFINITY },
  }));
  assert.ok(screen.getByText("Startup estimate unavailable"));
  assert.equal(screen.queryByText(/Rough startup estimate:|Observed readiness wait:|Weight loading:|Compilation:/), null);
});

test("Chinese startup text renders the reference formula and recorded budget without English fallback", async () => {
  await i18n.changeLanguage("zh");
  show(instance({ start_timeout_sec: 1800, start_timeout_source: "cluster",
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 520, reference_mib_per_sec: 512, initialization_seconds: 240 },
  }));
  assert.ok(screen.getByText("启动等待上限：30 分 0 秒 · 集群设置"));
  assert.ok(screen.getByText("粗略预计启动：8 分 40 秒"));
  assert.ok(screen.getByText("参考公式：143360 MiB ÷ 512 MiB/s + 4 分 0 秒 初始化耗时。"));
  assert.equal(screen.queryByText(/历史|样本/), null);
  assert.ok(screen.getByText("吞吐与初始化耗时采用固定参考假设，未经节点实测，不保证完成时间，也不改变启动等待上限。"));
});

for (const [locale, message] of [
  ["en", "The engine's 10 min 0 s wait limit is shorter than the rough startup estimate and may expire before the platform limit. Review the engine configuration."],
  ["zh", "引擎自身的 10 分 0 秒 等待上限短于粗略启动估算，可能先于平台超时，请检查引擎配置。"],
] as const) {
  test(`${locale} warns when the retained engine deadline is below the rough startup estimate`, async () => {
    await i18n.changeLanguage(locale);
    show(instance({ start_timeout_sec: 1800, start_timeout_source: "cluster", engine_start_timeout_sec: 600,
      startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 620, reference_mib_per_sec: 256, initialization_seconds: 60 },
    }));
    assert.ok(screen.getByText(message));
    assert.equal(screen.queryByText(/failed|timed out|判定失败/i), null);
  });
}

for (const invalid of [
  { estimated_seconds: Number.POSITIVE_INFINITY },
  { estimated_seconds: -1 },
  { estimated_seconds: 0 },
  { reference_mib_per_sec: 0 },
  { reference_mib_per_sec: Number.NaN },
  { initialization_seconds: undefined },
  { initialization_seconds: -1 },
  { weight_bytes: 0 },
]) {
  test(`invalid formula inputs are not presented as an estimate: ${JSON.stringify(invalid)}`, () => {
    show(instance({ startup_estimate: {
      weight_bytes: 1024 ** 3, method: "weight_reference", estimated_seconds: 64,
      reference_mib_per_sec: 256, initialization_seconds: 60, ...invalid,
    } }));
    assert.ok(screen.getByText("Startup estimate unavailable"));
    assert.equal(screen.queryByText(/Rough startup estimate:|Reference formula:/), null);
  });
}
