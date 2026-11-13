import "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { Instance, InstanceEvent } from "../src/api";

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", defaultNS: "deployments",
  resources: Object.fromEntries(["en", "zh"].map((locale) => [locale, {
    deployments: JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/deployments.json`, import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${locale}/common.json`, import.meta.url), "utf8")),
  }])),
  interpolation: { escapeValue: false },
});
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
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
  assert.ok(screen.getByText("No estimate (weight size unknown)"));
  assert.equal(screen.queryByText(/about \d/), null);
  assert.equal(screen.queryByRole("button", { name: "More information" }), null);
  assert.equal(screen.queryByRole("progressbar"), null);
});

test("the budget and the estimate share one line and the formula waits behind the help mark", async () => {
  show(instance({
    start_timeout_sec: 36030, start_timeout_source: "engine", engine_start_timeout_sec: 36000,
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 580, reference_mib_per_sec: 512, initialization_seconds: 300 },
  }));
  assert.ok(screen.getByText("Startup wait limit 10 h 0 min 30 s (engine configuration) · about 9 min 40 s"));
  assert.equal(screen.queryByText(/143360 MiB|reference values/), null, "the formula is not on the page");
  await userEvent.setup().hover(screen.getByRole("button", { name: "More information" }));
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, "143360 MiB ÷ 512 MiB/s + 5 min 0 s initialization\nThroughput and initialization are fixed reference values, not node measurements; the estimate does not affect the wait limit.");
  assert.equal(screen.queryByText(/No estimate|comparable starts|historical/i), null);
  assert.equal(screen.queryByText(/limit is shorter than/), null);
  assert.equal(within(screen.getByRole("region", { name: "Startup timing" })).getAllByText(/./, { selector: "p" }).length, 1, "one line, not a stack");
});

test("an explicit shorter platform budget reports the retained engine timeout conflict", () => {
  show(instance({ start_timeout_sec: 1800, start_timeout_source: "deployment", engine_start_timeout_sec: 36000, start_timeout_conflict: true }));
  assert.ok(screen.getByText("Startup wait limit 30 min 0 s (deployment setting)"));
  assert.ok(screen.getByText("The platform stops waiting before the engine's 10 h 0 min 0 s limit. Raise the deployment timeout."));
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
  assert.ok(screen.getByText("Startup wait limit 30 min 0 s (cluster setting) · about 10 min 20 s"));
  assert.ok(screen.getByText("Waited 13 min 20 s · Past the estimate, still within the limit"));
  assert.equal(screen.queryByText(/failed|timed out/i), null);
  assert.equal(screen.queryByRole("progressbar"), null);
});

test("only known measured stages are shown on one line, including an explicitly measured zero", async () => {
  show(instance({ state: "running", measured: {
    ModelWeightsGiB: 1, KVCacheGiB: 0, KVCacheTokens: 0, GraphCaptureGiB: 0,
    MaxConcurrency: 0, NumGPUBlocks: 0, Found: false,
    ModelLoadingSeconds: 4.32, CompilationSeconds: 0, StartupSeconds: 10,
  } }));
  assert.ok(screen.getByText("Ready after 10 s · Weight loading 4.3 s · Compilation 0 s"));
  assert.equal(screen.queryByText(/Graph capture/), null);
  assert.equal(screen.queryByText(/stages can overlap/), null);
  await userEvent.setup().hover(screen.getByRole("button", { name: "More information" }));
  assert.equal((await screen.findByRole("tooltip")).textContent, "Each stage is the largest value the engine log reports; stages can overlap and are not added.");
});

for (const [method, reason] of [
  ["unsupported", "No estimate for this configuration"],
  ["unrecognized", "No estimate"],
  ["empirical_loading_rate", "No estimate"],
] as const) {
  test(`${method} does not present numeric bounds as an available estimate`, () => {
    show(instance({ startup_estimate: { weight_bytes: 140 * 1024 ** 3, method, estimated_seconds: 620, reference_mib_per_sec: 256, initialization_seconds: 60 } }));
    assert.ok(screen.getByText(reason));
    assert.equal(screen.queryByText(/about \d/), null);
    assert.equal(screen.queryByRole("button", { name: "More information" }), null);
  });
}

test("a missed health-check timestamp stays unknown and does not borrow the image pull start", () => {
  show(instance({ phase: "health_check", start_timeout_sec: 1800 }), [
    { id: 1, kind: "state", params: { phase: "image_pull" }, created_at: "2026-11-11T08:00:00Z" },
  ]);
  assert.ok(screen.getByText("Wait time unknown"));
  assert.equal(screen.queryByText(/Waited/), null);
});

test("failed instances have no ongoing wait or slow-start message", () => {
  Date.now = () => Date.parse("2026-11-11T12:00:00Z");
  show(instance({ state: "failed", phase: "health_check", message: "container OOM-killed", start_timeout_sec: 1800,
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 620, reference_mib_per_sec: 256, initialization_seconds: 60 },
  }), [{ id: 1, kind: "state", params: { phase: "health_check" }, created_at: "2026-11-11T11:46:40Z" }]);
  assert.equal(screen.queryByText(/Waited|Wait time unknown/), null);
  assert.equal(screen.queryByText(/still within the limit/), null);
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
  assert.ok(screen.getByText("No estimate"));
  assert.equal(screen.queryByText(/about \d|Ready after|Weight loading|Compilation/), null);
});

test("Chinese startup text renders the budget, estimate and formula without English fallback", async () => {
  await i18n.changeLanguage("zh");
  show(instance({ start_timeout_sec: 1800, start_timeout_source: "cluster",
    startup_estimate: { weight_bytes: 140 * 1024 ** 3, method: "weight_reference", estimated_seconds: 520, reference_mib_per_sec: 512, initialization_seconds: 240 },
  }));
  assert.ok(screen.getByText("启动等待上限 30 分 0 秒（集群设置） · 预计约 8 分 40 秒"));
  assert.equal(screen.queryByText(/历史|样本|参考值/), null);
  await userEvent.setup().hover(screen.getByRole("button", { name: "说明" }));
  assert.equal((await screen.findByRole("tooltip")).textContent, "143360 MiB ÷ 512 MiB/s + 初始化 4 分 0 秒\n吞吐与初始化耗时是固定参考值，并非节点实测；估算不影响等待上限。");
});

for (const [locale, message] of [
  ["en", "The engine's 10 min 0 s limit is shorter than the estimated start. Raise it in the engine configuration."],
  ["zh", "引擎的 10 分 0 秒 上限短于预计启动时长，请调整引擎配置。"],
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
    assert.ok(screen.getByText("No estimate"));
    assert.equal(screen.queryByText(/about \d/), null);
  });
}
