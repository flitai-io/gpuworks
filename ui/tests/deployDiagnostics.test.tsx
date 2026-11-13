import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import type { DiagnosticReport } from "../src/diagnosticChecks.ts";

const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const View = (await import("../src/pages/deploy/DeployDiagnostics.tsx")).default;
afterEach(cleanup);
after(() => dom.window.close());

const report: DiagnosticReport = { checks: [
  { id: "storage-access:7:3", status: "failed", message: { code: "STORAGE_UNAVAILABLE_ON_NODE", params: { node: "gpu-a", storage: "shared-nfs", storage_id: 3, reason: "not_mounted", observer: "agent_report" }, message: "mount failed" } },
  { id: "model-record:7:3", status: "failed", message: { code: "MODEL_SOURCE_UNAVAILABLE", params: { model: "local-model", model_id: 42, reason: "model_missing_in_storage", observer: "control_plane" }, message: "no complete model record" } },
  { id: "path-read:7:3", status: "blocked", blocked_by: ["storage-access:7:3"], message: { code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "storage_access_required", observer: "control_plane" }, message: "path not checked" } },
  { id: "transfer:7", status: "pending", message: { code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "copy_in_progress", observer: "control_plane" }, message: "copy pending" } },
  { id: "peer:7", status: "unknown", message: { code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "peer_available", observer: "control_plane" }, message: "peer not probed" } },
] };

for (const language of ["en", "zh"] as const) {
  test(`diagnostics retain independent failures, blocked steps and observation scope (${language})`, async () => {
    const { i18n, locale } = await createI18n(["deploy"], language);
    const text = locale.deploy.diagnostics;
    render(<I18nextProvider i18n={i18n}><View report={report} requestId="r215" /></I18nextProvider>);
    const details = document.querySelector("details")!;
    assert.equal(details.open, true, "failures must be visible without opening another layer");
    const items = screen.getAllByRole("listitem");
    assert.equal(items.length, 5);
    assert.ok(within(items[0]).getByText(text.reason.not_mounted));
    assert.ok(within(items[1]).getByText(text.reason.model_missing_in_storage));
    assert.ok(within(items[2]).getByText(text.status.blocked));
    assert.ok(within(items[2]).getByText(text.reason.storage_access_required));
    assert.ok(within(items[2]).getByText(text.blockedBy.replace("{{checks}}", text.reason.not_mounted)));
    assert.ok(within(items[3]).getByText(text.status.pending));
    assert.ok(within(items[4]).getByText(text.status.unknown));
    assert.ok(screen.getByText(text.observer.agent_report));
    assert.ok(screen.getByText(text.request.replace("{{id}}", "r215")));
    assert.equal(screen.getByRole("link", { name: text.storageAction }).getAttribute("href"), "/storage?storage=3");
    assert.equal(screen.getByRole("link", { name: text.modelAction }).getAttribute("href"), "/models?detail=42");
    const summary = details.querySelector("summary")!;
    summary.focus();
    assert.equal(document.activeElement, summary);
    await userEvent.setup().click(summary);
    assert.equal(details.open, false);
    await userEvent.setup().click(summary);
    assert.equal(details.open, true);
  });
}

test("future diagnostic codes and statuses remain useful without a false pass", async () => {
  const { i18n, locale } = await createI18n(["deploy"], "zh");
  render(<I18nextProvider i18n={i18n}><View report={{ truncated: true, checks: [
    { id: "future", status: "future_state", message: { code: "FUTURE_CODE", message: "A future safe explanation" } },
    { id: "future-reason", status: "unknown", message: { code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "future_reason" }, message: "A specific cause this client does not know yet" } },
  ] }} /></I18nextProvider>);
  assert.ok(screen.getByText("A future safe explanation"));
  assert.ok(screen.getByText("A specific cause this client does not know yet"));
  assert.equal(screen.getAllByText(locale.deploy.diagnostics.status.unknown).length, 2);
  assert.ok(screen.getByText(locale.deploy.diagnostics.truncated));
  assert.equal(screen.queryByText(locale.deploy.diagnostics.status.passed), null);
});

test("legacy responses do not create an empty diagnostic panel", async () => {
  const { i18n } = await createI18n(["deploy"]);
  const view = render(<I18nextProvider i18n={i18n}><View /></I18nextProvider>);
  assert.equal(view.container.textContent, "");
});
