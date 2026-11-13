import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import type { EvaluateResponse, ScheduleMessageLike } from "../src/api.ts";
const { cleanup, render, screen } = await import("@testing-library/react");
const { nodeRefusals } = await import("../src/pages/deploy/deployPlacementRefusal.ts");
const View = (await import("../src/pages/deploy/DeployNodeRefusals.tsx")).default;
afterEach(cleanup);
after(() => dom.window.close());
const reason = { code: "insufficient_disk", params: {
  node: "gpu-a", node_uuid: "node-a", model: "Qwen3", model_id: 42,
  storage: "weights", storage_id: 7, storage_type: "local", path: "/srv/weights/qwen3",
  required_bytes: 1073741824, available_bytes: 1048576,
} };
function result(message: ScheduleMessageLike = reason): EvaluateResponse {
  // An automatic draft can be valid while every evaluated node is refused.
  return { ok: true, candidates: [{ node_uuid: "node-a", node_name: "gpu-a", ok: false, reason: message }] } as never;
}

test("a manually selected node exposes the same existing-model action", async () => {
  const { i18n } = await createI18n(["deploy"]);
  const { Form } = await import("antd");
  const Summary = (await import("../src/pages/deploy/DeployStepSummary.tsx")).default;
  const evaluation = { ok: false, plan: { ok: false, message: reason } } as never;
  render(<I18nextProvider i18n={i18n}><Form><Summary result={evaluation} error={null} loading={false} /></Form></I18nextProvider>);
  assert.equal(screen.getByRole("link").getAttribute("href"), "/models?detail=42");
  assert.ok(screen.getByRole("alert").textContent?.includes("/srv/weights/qwen3"));
});

for (const [name, message] of [
  ["runtime filesystem", { ...reason, code: "insufficient_runtime_disk" }],
  ["cache storage", { ...reason, params: { ...reason.params, storage_type: "nfs_cache" } }],
  ["legacy unknown storage", { code: "insufficient_disk" }],
  ["untrusted model id", { ...reason, params: { ...reason.params, model_id: "42" } }],
] as Array<[string, ScheduleMessageLike]>) {
  test(`a ${name} refusal does not suggest an inapplicable model registration`, async () => {
    const { i18n } = await createI18n(["deploy"]);
    const items = nodeRefusals(i18n.getFixedT("en", "deploy"), result(message));
    const view = render(<I18nextProvider i18n={i18n}><View refusals={items} conditions={null} /></I18nextProvider>);
    assert.equal(screen.queryByRole("link") === null, true);
    assert.doesNotMatch(view.container.textContent ?? "", /\{\{|undefined|NaN/);
  });
}

test("explicit zero capacity remains visible and unknown values remain absent", async () => {
  const { i18n } = await createI18n(["deploy"]);
  const message = { code: "insufficient_disk", params: { path: "/mnt/cache", required_bytes: 1073741824, available_bytes: 0 } };
  const view = render(<I18nextProvider i18n={i18n}><View refusals={nodeRefusals(i18n.getFixedT("en", "deploy"), result(message))} conditions={null} /></I18nextProvider>);
  assert.ok(view.container.textContent?.includes("available: 0 B"));
  assert.doesNotMatch(view.container.textContent ?? "", /undefined|NaN|\{\{/);
});
for (const language of ["en", "zh"] as const) {
  test(`a disk refusal explains the target and space and links the same model's existing-path workflow (${language})`, async () => {
    const { i18n } = await createI18n(["deploy"], language);
    const items = nodeRefusals(i18n.getFixedT(language, "deploy"), result());
    const view = render(<I18nextProvider i18n={i18n}><View refusals={items} conditions={null} /></I18nextProvider>);
    assert.ok(view.container.textContent?.includes("/srv/weights/qwen3"), "the actual target directory is visible");
    assert.ok(view.container.textContent?.includes("1.0 GiB"), "the additional required space is visible");
    assert.ok(view.container.textContent?.includes("1.0 MiB"), "available space is visible");
    const link = screen.getByRole("link");
    assert.equal(link.getAttribute("href"), "/models?detail=42");
    assert.equal(link.getAttribute("target"), "_blank", "keep the unfinished deployment draft open");
    assert.match(link.textContent ?? "", language === "zh" ? /登记.*目录/ : /[Rr]egister.*directory/);
    assert.doesNotMatch(view.container.textContent ?? "", /\{\{|undefined|NaN/);
  });
}

test("a final runtime refusal cannot inherit a registration action from an older model plan", async () => {
  const { i18n } = await createI18n(["deploy"]);
  const evaluation = { ok: true, candidates: [{ node_uuid: "node-a", node_name: "gpu-a", ok: false,
    reason: { code: "insufficient_runtime_disk" }, plan: { ok: false, message: reason } }] } as never;
  render(<I18nextProvider i18n={i18n}><View refusals={nodeRefusals(i18n.getFixedT("en", "deploy"), evaluation)} conditions={null} /></I18nextProvider>);
  assert.equal(screen.queryByRole("link") === null, true, "the final refusal controls the offered action");
});

test("a legacy candidate with only a model plan still exposes the registration action", async () => {
  const { i18n } = await createI18n(["deploy"]);
  const evaluation = { ok: true, candidates: [{ node_uuid: "node-a", node_name: "gpu-a", ok: false,
    plan: { ok: false, message: reason } }] } as never;
  render(<I18nextProvider i18n={i18n}><View refusals={nodeRefusals(i18n.getFixedT("en", "deploy"), evaluation)} conditions={null} /></I18nextProvider>);
  assert.equal(screen.getByRole("link").getAttribute("href"), "/models?detail=42");
});
