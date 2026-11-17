import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "zh", defaultNS: "evaluations", interpolation: { escapeValue: false },
	resources: { zh: { evaluations: {
		common: { retry: "重试" },
		detail: {
			curveLoading: "正在读取过程曲线…", curveLoadFailed: "过程曲线读取失败",
			curveRefreshFailed: "最新曲线刷新失败，当前显示上次成功读取的数据",
			curveNoRequests: "本次 Run 在正式采样开始前已失败，因此没有运行过程曲线",
			curveLegacyMissing: "该历史 Run 未记录过程曲线",
		},
	} } },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: BenchmarkCurveNotice } = await import("../src/pages/evaluations/BenchmarkCurveNotice.tsx");

afterEach(() => cleanup());

test("a Run that failed before sending requests is not labeled as lost historical data", () => {
	render(<I18nextProvider i18n={i18n}><BenchmarkCurveNotice active={false} loading={false} hasResponse hasSamples={false} runState="failed" completed={0} runError="agentic workload requires an exact tokenizer" onRetry={() => {}} /></I18nextProvider>);
	assert.ok(screen.getByText(/正式采样开始前已失败/));
	assert.ok(screen.getByText("agentic workload requires an exact tokenizer"));
	assert.equal(screen.queryByText(/历史 Run/), null);
});

test("loading and endpoint failures are distinct from an empty historical curve", () => {
	const { rerender } = render(<I18nextProvider i18n={i18n}><BenchmarkCurveNotice active={false} loading hasResponse={false} hasSamples={false} completed={0} onRetry={() => {}} /></I18nextProvider>);
	assert.ok(screen.getByRole("status"));
	assert.ok(screen.getByText("正在读取过程曲线…"));
	renderError(rerender);
	assert.ok(screen.getByText("过程曲线读取失败"));
	assert.equal(screen.queryByText(/历史 Run/), null);
});

function renderError(rerender: (ui: React.ReactNode) => void) {
	rerender(<I18nextProvider i18n={i18n}><BenchmarkCurveNotice active={false} loading={false} hasResponse={false} hasSamples={false} completed={0} error={new Error("network down")} onRetry={() => {}} /></I18nextProvider>);
}

test("warmup recovery shows readable progress without claiming measured requests", () => {
 i18n.addResource("zh", "evaluations", "detail.warmupRecovery", "正在预热，尚未开始正式采样");
 const {rerender}=render(<I18nextProvider i18n={i18n}><BenchmarkCurveNotice active warmup loading hasResponse={false} hasSamples={false} completed={0} runError="HTTP 503, stage http: engine restarting; retrying in 5s" onRetry={()=>{}} /></I18nextProvider>);
 assert.ok(screen.getByText("正在预热，尚未开始正式采样"));
 assert.ok(screen.getByText(/engine restarting; retrying/));
 assert.equal(screen.queryByText("正在读取过程曲线…"),null);
 rerender(<I18nextProvider i18n={i18n}><BenchmarkCurveNotice active={false} warmup loading={false} hasResponse hasSamples={false} completed={0} runState="failed" runError="warmup recovery exhausted: engine restarting" onRetry={()=>{}} /></I18nextProvider>);
 assert.ok(screen.getByText("warmup recovery exhausted: engine restarting"));
 assert.equal(screen.queryByText("正在预热，尚未开始正式采样"),null);
});

test("a coded failure is rendered from its code in the reader's language, not from the English message", async () => {
	const zh = (await import("../src/i18n/locales/zh/evaluations.json", { with: { type: "json" } })).default as { benchmarkError: Record<string, string> };
	i18n.addResource("zh", "evaluations", "benchmarkError.BENCHMARK_DATASET_LOAD_FAILED", zh.benchmarkError.BENCHMARK_DATASET_LOAD_FAILED);
	const failure = { code: "BENCHMARK_DATASET_LOAD_FAILED", params: { dataset: "multiturn" }, message: "load the built-in multiturn dataset: unexpected EOF" };
	render(<I18nextProvider i18n={i18n}><BenchmarkCurveNotice active={false} loading={false} hasResponse hasSamples={false} runState="failed" completed={0} runError={failure.message} runFailure={failure} onRetry={() => {}} /></I18nextProvider>);
	assert.ok(screen.getByText("内置数据集 multiturn 加载失败，评测在发出请求前已终止。"));
	assert.equal(screen.queryByText(/unexpected EOF/), null);
});
