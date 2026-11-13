import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["deploy"]);
const { cleanup, render, screen } = await import("@testing-library/react");
const { Form } = await import("antd");
const { APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");
const useDeployEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const Summary = (await import("../src/pages/deploy/DeployStepSummary.tsx")).default;
const original = api.evaluate;
afterEach(() => { cleanup(); api.evaluate = original; });
after(() => dom.window.close());

const diagnostics = { checks: [{ id: "storage-access:7:3", status: "failed", message: {
  code: "STORAGE_UNAVAILABLE_ON_NODE", params: { reason: "not_mounted", storage: "nfs", observer: "agent_report" }, message: "not mounted",
} }] };

function Harness() {
  const [form] = Form.useForm();
  const evaluation = useDeployEvaluation({ form, signal: 1, buildPayload: () => ({ model_id: 42 }), translate: (key, options) => i18n.t(key, options) });
  return <Form form={form}><Summary result={evaluation.result} error={evaluation.error} failure={evaluation.failure} loading={evaluation.evaluating} /></Form>;
}

for (const transport of ["business-refusal", "http-error"] as const) {
  test(`evaluation keeps diagnostic evidence visible after ${transport}`, async () => {
    api.evaluate = async () => {
      if (transport === "http-error") throw new APIError("MODEL_SOURCE_UNAVAILABLE", {}, "source unavailable", 400, diagnostics, "r-evaluate-215");
      return { ok: false, diagnostics, request_id: "r-evaluate-215" } as never;
    };
    render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
    await screen.findByText(locale.deploy.diagnostics.reason.not_mounted);
    assert.ok(screen.getByText("Request ID: r-evaluate-215"));
    assert.ok(screen.getByText(locale.deploy.diagnostics.observer.agent_report));
    assert.equal(screen.queryByText(locale.deploy.diagnostics.status.passed), null);
  });
}
