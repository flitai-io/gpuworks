import { createI18n, dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";

const context = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 600, height: 280, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return context;
  },
  set: () => true,
  apply: () => context,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => context as CanvasRenderingContext2D;
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "EventSource", { value: class { addEventListener() {} removeEventListener() {} close() {} }, configurable: true });

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { I18nextProvider } = await import("react-i18next");
const { getInstanceByDom } = await import("echarts/core");
const { api } = await import("../src/api.ts");
const { default: EvaluationDetailPage } = await import("../src/pages/evaluations/EvaluationDetailPage.tsx");
const { i18n } = await createI18n(["evaluations", "common"]);
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });
after(() => dom.window.close());

test("evaluation curve leaves unknown measurements empty while drawing observed zero", async () => {
  api.benchmark = async () => ({
    id: 7, name: "Historical metrics", target_kind: "external", model: "model", mode: "grid", state: "done",
    created_at: "2026-10-03T00:00:00Z", dataset: { kind: "random" }, search: {},
  }) as never;
  api.benchmarkMeta = async () => ({ modes: [], statuses: [], datasets: [], dataset_profiles: {}, agentic_presets: [], agentic_defaults: { preset: "agentic", turns: 13, concurrency: [1], sessions_per_point: {}, sessions_per_concurrency: 2, min_sessions_per_point: 4, subsequent_turn_input_tokens: 753, output_tokens: 220 }, load_defaults: { point_duration_s: 60, ignore_eos_datasets: [] }, mode_defaults: {}, detail_cap_mb: { default: 20 }, objectives: [], sla_constraints: [], tokenizer_schemes: {} }) as never;
  api.benchmarkCurve = async () => ({ points: [{ seq: 1, concurrency: 1, samples: [
    { t: 1000, output_tput: null, ttft_p50: null },
    { t: 2000 },
    { t: 3000, output_tput: 0, ttft_p50: 0 },
    { t: 4000, output_tput: 5, ttft_p50: 50 },
  ] }] }) as never;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/evaluations/7"]}>
    <Routes><Route path="/evaluations/:id" element={<EvaluationDetailPage />} /></Routes>
  </MemoryRouter></I18nextProvider>);
  await screen.findByText("Historical metrics");
  const chart = await screen.findByRole("img", { name: i18n.t("common:timeSeries.ariaLabel") });
  const line = () => {
    const option = getInstanceByDom(chart as HTMLElement)!.getOption();
    return (option.series as Array<{ data: Array<[number, number | null]>; connectNulls: boolean }>)[0];
  };
  await waitFor(() => assert.equal(line()?.data.length, 4));
  const values = () => line().data.map((point) => point[1]);
  assert.ok(values()[0] == null || Number.isNaN(values()[0]), "null measurement must not draw zero");
  assert.ok(values()[1] == null || Number.isNaN(values()[1]), "omitted measurement must not draw zero");
  assert.deepEqual(values().slice(2), [0, 5]);
  assert.equal(line().connectNulls, false, "missing observations leave a line gap");
  const user = (await import("@testing-library/user-event")).default.setup({ document: dom.window.document });
  screen.getByRole("radio", { name: "TTFT" }).focus();
  await user.keyboard(" ");
  await waitFor(() => assert.deepEqual(values().slice(2), [0, 50]));
  assert.ok(values()[0] == null || Number.isNaN(values()[0]));
});
