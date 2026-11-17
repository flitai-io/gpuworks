import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { BenchmarkMeta, BenchmarkRun } from "../src/api.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const zh = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/evaluations.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "zh", defaultNS: "evaluations", interpolation: { escapeValue: false }, resources: { zh: { evaluations: zh } } });

const text = (en: string, zhText: string) => ({ en, zh: zhText });
// The dictionary as GET /api/v1/benchmarks/meta serves it, trimmed to the
// entries the load step reads.
const defaults = (concurrency: number | number[], extra: Record<string, unknown> = {}) => ({ concurrency, isl: [4096], osl: [1024], requests_per_point_multiplier: 10, ...extra });
const meta = {
	modes: [{ value: "saturation", label: text("Throughput Limit Probe", "吞吐上限探测") }, { value: "sla", label: text("SLA Maximum Concurrency", "SLA 最大并发") }, { value: "grid", label: text("Parameter Sweep", "参数扫描") }],
	mode_defaults: { saturation: defaults(1, { max_concurrency: 32, min_throughput_gain: 0.05 }), sla: defaults(1, { max_concurrency: 32, objective: "rps" }), grid: defaults([1, 2, 4]) },
	datasets: [{ value: "random", label: text("Fixed-Length Random", "纯随机定长") }, { value: "agentic", label: text("Agentic", "Agentic 合成负载") }, { value: "multiturn", label: text("OASST1", "OASST1 多轮对话") }],
	dataset_profiles: { multiturn: { sample_count: 3826, max_input_tokens: 1172, max_output_tokens: 2496 } },
	sla_constraints: [
		{ key: "max_p99_ttft", label: text("P99 TTFT", "P99 TTFT"), unit: "ms", direction: "max", min: 0, max: 3600000, step: 1 },
		{ key: "min_interactivity", label: text("Interactivity (per-user output speed)", "单用户输出速度"), unit: "tok/s/user", direction: "min", min: 0, max: 100000, step: 0.1 },
		{ key: "min_session_completion", label: text("Session completion rate (0–1)", "会话完成率（0–1）"), unit: "", direction: "min", min: 0, max: 1, step: 0.01 },
	],
	objectives: [{ value: "rps", label: text("Request Throughput", "请求吞吐") }],
	statuses: [],
	tokenizer_schemes: { huggingface: "hf", modelscope: "ms", upload: "file" },
	detail_cap_mb: { default: 20, min: 1, max: 50, unit: "MB" },
	agentic_presets: [{ value: "agentic-80k-v1", label: "80K", mean_input_tokens: 80_000, first_turn_input_tokens: 74_162, peak_input_tokens: 85_838, required_context_tokens: 90_000, recommended: true }],
	agentic_defaults: { preset: "agentic-80k-v1", turns: 13, subsequent_turn_input_tokens: 753, output_tokens: 220, seed: 42, concurrency: [1, 2, 4, 8], sessions_per_point: { "1": 4, "2": 8, "4": 8, "8": 16 }, sessions_per_concurrency: 2, min_sessions_per_point: 4 },
	load_defaults: { warmup_requests: 1, max_warmup_requests: 1000, request_timeout_s: 600, min_request_timeout_s: 10, max_request_timeout_s: 7200, burstiness: 1, point_duration_s: 60, min_point_duration_s: 5, max_point_duration_s: 3600, min_open_loop_requests: 20, max_request_rate: 10000, max_request_rates: 32, max_burstiness: 100, max_in_flight: 100000, ignore_eos: true, ignore_eos_datasets: ["random", "agentic"] },
} as unknown as BenchmarkMeta;

const deployment = { id: 7, name: "托管部署", node_uuid: "node-1", model_id: 1, served_name: "served", engine: "vllm", desired_state: "active", runtime_state: "running", sleepable: false, replicas: 1, template_deviated: false, created_at: "2026-09-01T00:00:00Z", operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions", renderer: "chat" }] } as unknown as import("../src/api.ts").Deployment;
const nodes = [{ uuid: "node-1", name: "节点一", hostname: "node-1", state: "online", caps: ["bench", "bench.arrival"] }] as unknown as import("../src/api.ts").Node[];

const calls: Array<Record<string, any>> = [];
const apiModule = await import("../src/api.ts");
apiModule.api.deployment = async () => ({ ...deployment, instances: [{ uuid: "leader-1", deployment_id: deployment.id, node_uuid: "node-1", state: "running", sleep_level: 0, node_rank: 0, api_port: 8000 } as import("../src/api.ts").Instance] });
apiModule.api.createBenchmark = (async (body: unknown) => {
	calls.push(body as Record<string, any>);
	return { id: 1, name: "created", load_origin: "server" } as BenchmarkRun;
}) as typeof apiModule.api.createBenchmark;
apiModule.api.priorityClasses = async () => ({
	min_value: -100, max_value: 100, policies: [], default_policy: "", ceiling_policies: [],
	items: [
		{ name: "high", display_name: "", description: "", value: 20, builtin: true, default: false, deletable: false, created_at: "" },
		{ name: "normal", display_name: "", description: "", value: 0, builtin: true, default: true, deletable: false, created_at: "" },
	],
});
(await import("antd")).message.success = (() => ({}) as never) as never;

const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: BenchmarkWizard } = await import("../src/pages/evaluations/BenchmarkWizard.tsx");

afterEach(() => {
	cleanup();
	calls.length = 0;
});

async function until(predicate: () => boolean, what: string) {
	for (let attempt = 0; attempt < 100; attempt++) {
		if (predicate()) return;
		await new Promise((resolve) => setTimeout(resolve, 20));
	}
	throw new Error(`timed out waiting for ${what}`);
}

async function openLoadStep(preset: import("../src/pages/evaluations/BenchmarkWizard.tsx").WizardPreset) {
	const user = userEvent.setup();
	render(<I18nextProvider i18n={i18n}><BenchmarkWizard open meta={meta} deployments={[deployment]} nodes={nodes} preset={{ deploymentID: deployment.id, ...preset }} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
	await user.click(await screen.findByRole("button", { name: /下一步/ }));
	return user;
}

async function openAdvanced(user: ReturnType<typeof userEvent.setup>) {
	await user.click(screen.getByText(zh.wizard.advanced));
}

async function start(user: ReturnType<typeof userEvent.setup>) {
	await user.click(screen.getByRole("button", { name: zh.wizard.start }));
	await until(() => calls.length === 1, "the create request");
	return calls[0];
}

test("a new run warms up once and fixes the output length by default", async () => {
	const user = await openLoadStep({ mode: "saturation" });
	await openAdvanced(user);
	// Saturation bisects on concurrency, so the arrival-rate choice is a grid
	// setting only.
	assert.equal(screen.queryByText(zh.wizard.loadMode), null);
	assert.equal((screen.getByLabelText(new RegExp(zh.wizard.warmupRequests)) as HTMLInputElement).value, "1");
	assert.equal((screen.getByRole("checkbox", { name: new RegExp(zh.wizard.ignoreEOS.replace(/[()]/g, "\\$&")) }) as HTMLInputElement).checked, true);
	// The request timeout defaults to the server's dictionary value.
	assert.equal((screen.getByLabelText(new RegExp(zh.wizard.requestTimeout)) as HTMLInputElement).value, "600");
	const body = await start(user);
	assert.equal(body.search.warmup_requests, 1);
	assert.equal(body.search.request_timeout_s, 600);
	assert.equal(body.search.request_rates, undefined);
	assert.equal(body.dataset.kind, "random");
	assert.equal(body.dataset.ignore_eos, true);
});

test("warmup and the fixed output length can be turned off", async () => {
	const user = await openLoadStep({ mode: "saturation" });
	await openAdvanced(user);
	const warmup = screen.getByLabelText(new RegExp(zh.wizard.warmupRequests));
	await user.clear(warmup);
	await user.type(warmup, "0");
	await user.click(screen.getByRole("checkbox", { name: new RegExp(zh.wizard.ignoreEOS.replace(/[()]/g, "\\$&")) }));
	const body = await start(user);
	assert.equal(body.search.warmup_requests, 0);
	assert.equal(body.dataset.ignore_eos, false);
});

test("a long-context run raises the request timeout in the advanced settings", async () => {
	const user = await openLoadStep({ mode: "saturation" });
	await openAdvanced(user);
	const timeout = screen.getByLabelText(new RegExp(zh.wizard.requestTimeout));
	await user.clear(timeout);
	await user.type(timeout, "1800");
	await user.keyboard("{Tab}");
	const body = await start(user);
	assert.equal(body.search.request_timeout_s, 1800);
});

test("a parameter scan by arrival rate sends rates instead of concurrency", async () => {
	const user = await openLoadStep({ mode: "grid" });
	await openAdvanced(user);
	const loadMode = screen.getByText(zh.wizard.loadMode).closest(".ant-form-item") as HTMLElement;
	await user.click(within(loadMode).getByText(zh.wizard.loadModeOpen));
	// The rate list replaces the concurrency list on the load step.
	assert.equal(screen.queryByLabelText(zh.wizard.concurrencyList), null, "an arrival-rate scan shows no concurrency list");
	const rates = screen.getByPlaceholderText("1, 2, 4, 8");
	await user.type(rates, "1, 2.5");
	// 1 req/s and 2.5 req/s for the default 60 s: 60 + 150 requests, two
	// one-minute points.
	assert.ok(screen.getByText("约 210 个请求，预计 2 分钟"));
	const maxInFlight = screen.getByLabelText(new RegExp(zh.wizard.maxInFlight));
	await user.clear(maxInFlight);
	await user.type(maxInFlight, "16");
	const body = await start(user);
	assert.equal(body.mode, "grid");
	assert.deepEqual(body.search.request_rates, [1, 2.5]);
	assert.equal(body.search.concurrency, undefined);
	assert.equal(body.search.burstiness, 1);
	assert.equal(body.search.max_in_flight, 16);
	assert.equal(body.search.point_duration_s, 60);
	assert.equal(body.search.warmup_requests, 1);
	assert.deepEqual(body.search.isl, [4096]);
});

test("switching back to fixed concurrency restores the concurrency list", async () => {
	const user = await openLoadStep({ mode: "grid" });
	await openAdvanced(user);
	const loadMode = screen.getByText(zh.wizard.loadMode).closest(".ant-form-item") as HTMLElement;
	await user.click(within(loadMode).getByText(zh.wizard.loadModeOpen));
	assert.equal(screen.queryByLabelText(zh.wizard.concurrencyList), null);
	await user.click(within(loadMode).getByText(zh.wizard.loadModeClosed));
	assert.equal((screen.getByLabelText(zh.wizard.concurrencyList) as HTMLInputElement).value, "1,2,4");
	const body = await start(user);
	assert.deepEqual(body.search.concurrency, [1, 2, 4]);
	assert.equal(body.search.request_rates, undefined);
});

test("an arrival-rate scan without rates is refused before it is sent", async () => {
	const user = await openLoadStep({ mode: "grid" });
	await openAdvanced(user);
	const loadMode = screen.getByText(zh.wizard.loadMode).closest(".ant-form-item") as HTMLElement;
	await user.click(within(loadMode).getByText(zh.wizard.loadModeOpen));
	await user.click(screen.getByRole("button", { name: zh.wizard.start }));
	await screen.findByText(zh.wizard.requestRatesInvalid.replace("{{count}}", "32").replace("{{max}}", "10000"));
	assert.equal(calls.length, 0);
});

test("a corpus dataset never offers or sends ignore_eos", async () => {
	const source = { id: 3, name: "多轮", mode: "saturation", target_kind: "instance", deployment_id: 7, model: "m", load_origin: "server", num_gpus: 1, dataset: { kind: "multiturn", output_tokens: 256 }, search: { concurrency: 1, max_concurrency: 8, osl: [256] }, keep_detail: false, detail_cap_mb: 20, state: "done", created_at: "2026-10-01T00:00:00Z" } as unknown as BenchmarkRun;
	const user = await openLoadStep({ sourceRun: source });
	await openAdvanced(user);
	assert.equal(screen.queryByRole("checkbox", { name: new RegExp(zh.wizard.ignoreEOS.replace(/[()]/g, "\\$&")) }), null);
	const body = await start(user);
	assert.equal(body.dataset.kind, "multiturn");
	assert.equal("ignore_eos" in body.dataset, false);
});

test("rerunning a run with retired per-user limits drops them and says so", async () => {
	const source = { id: 9, name: "旧 SLA", mode: "sla", target_kind: "instance", deployment_id: 7, model: "m", load_origin: "server", num_gpus: 1, dataset: { kind: "random", input_tokens: 512, output_tokens: 128 }, search: { concurrency: 1, max_concurrency: 8, isl: [512], osl: [128], max_p99_ttft: 900, min_per_user_output_tps: 20, max_per_user_total_tps: 400, objective: "rps" }, keep_detail: false, detail_cap_mb: 20, state: "done", created_at: "2026-09-01T00:00:00Z" } as unknown as BenchmarkRun;
	const user = await openLoadStep({ sourceRun: source });
	assert.ok(screen.getByText(zh.wizard.legacySLADropped));
	const body = await start(user);
	assert.equal(body.search.max_p99_ttft, 900);
	assert.equal(body.search.min_per_user_output_tps, undefined);
	assert.equal(body.search.max_per_user_total_tps, undefined);
	// The source predates warmup and the switch: the rerun takes today's
	// defaults.
	assert.equal(body.search.warmup_requests, 1);
	assert.equal(body.dataset.ignore_eos, true);
});

test("rerunning an arrival-rate scan restores its load settings", async () => {
	const source = { id: 11, name: "开环", mode: "grid", target_kind: "instance", deployment_id: 7, model: "m", load_origin: "server", num_gpus: 1, dataset: { kind: "random", input_tokens: 512, output_tokens: 128, ignore_eos: false }, search: { request_rates: [0.5, 4], burstiness: 0.5, max_in_flight: 32, point_duration_s: 120, isl: [512], osl: [128], warmup_requests: 3, request_timeout_s: 1200 }, keep_detail: false, detail_cap_mb: 20, state: "done", created_at: "2026-10-09T00:00:00Z" } as unknown as BenchmarkRun;
	const user = await openLoadStep({ sourceRun: source });
	assert.equal(screen.queryByText(zh.wizard.legacySLADropped), null);
	assert.equal((screen.getByPlaceholderText("1, 2, 4, 8") as HTMLInputElement).value, "0.5,4");
	const body = await start(user);
	assert.deepEqual(body.search.request_rates, [0.5, 4]);
	assert.equal(body.search.burstiness, 0.5);
	assert.equal(body.search.max_in_flight, 32);
	assert.equal(body.search.point_duration_s, 120);
	assert.equal(body.search.warmup_requests, 3);
	assert.equal(body.search.request_timeout_s, 1200);
	assert.equal(body.dataset.ignore_eos, false);
});


test("a grid scan sends the configured session completion floor without inventing a default", async () => {
 const user = await openLoadStep({ mode: "grid" });
 await openAdvanced(user);
 const field = screen.getByLabelText(/会话完成率/);
 await user.type(field, "0.95");
 const body = await start(user);
 assert.equal(body.search.min_session_completion, 0.95);
 assert.equal(body.search.max_p99_ttft, undefined);
});


test("rerunning a constrained scan as saturation does not send hidden experience limits", async () => {
 const source = { id: 3, name: "constrained", mode: "grid", target_kind: "instance", deployment_id: 7, load_origin: "server", dataset: { kind: "random" }, search: { concurrency: [1], min_session_completion: .95, max_p99_ttft: 100 } } as BenchmarkRun;
 const user = await openLoadStep({ mode: "saturation", sourceRun: source });
 const body = await start(user);
 assert.equal(body.mode, "saturation");
 assert.equal(body.search.min_session_completion, undefined);
 assert.equal(body.search.max_p99_ttft, undefined);
});

// A run measured from the server goes through the gateway, so it may ask for a
// request priority; none is asked for unless chosen.
test("a server-origin run asks for the chosen request priority, and for none by default", async () => {
	const render = async () => {
		const user = userEvent.setup();
		const view = renderWizard();
		const trigger = await screen.findByRole("combobox", { name: zh.wizard.priorityClass });
		await until(() => !trigger.closest(".ant-select")?.classList.contains("ant-select-disabled"), "the classes");
		return { user, trigger, view };
	};
	let { user, trigger } = await render();
	screen.getByText(zh.wizard.priorityClassHelp);
	assert.ok(screen.getByText(zh.wizard.priorityClassDefault.replace("{{name}}", "normal")));
	await user.click(screen.getByRole("button", { name: /下一步/ }));
	let body = await start(user);
	assert.equal("priority_class" in body, false);
	cleanup();
	calls.length = 0;
	({ user, trigger } = await render());
	await user.click(trigger);
	await user.click(await screen.findByTitle("high (20)"));
	await user.click(screen.getByRole("button", { name: /下一步/ }));
	body = await start(user);
	assert.equal(body.priority_class, "high");
});

function renderWizard() {
	return render(<I18nextProvider i18n={i18n}><BenchmarkWizard open meta={meta} deployments={[deployment]} nodes={nodes} preset={{ deploymentID: deployment.id, mode: "saturation" }} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
}

// A rerun asks again for its run's class, or for the default once that class
// is deleted; a deployment that does not schedule by class says so.
test("a rerun keeps its class while it exists and falls back to the default once deleted", async () => {
	const rerun = async (priority_class: string, scheduling: boolean) => {
		apiModule.api.deployment = async () => ({ ...deployment, priority_scheduling: scheduling, instances: [{ uuid: "leader-1", deployment_id: deployment.id, node_uuid: "node-1", state: "running", sleep_level: 0, node_rank: 0, api_port: 8000 } as import("../src/api.ts").Instance] });
		const sourceRun = { id: 9, name: "earlier", target_kind: "instance", deployment_id: deployment.id, model: "served", load_origin: "server", num_gpus: 1, mode: "saturation", priority_class, dataset: { kind: "random" }, search: {} } as unknown as BenchmarkRun;
		const user = userEvent.setup();
		render(<I18nextProvider i18n={i18n}><BenchmarkWizard open meta={meta} deployments={[deployment]} nodes={nodes} preset={{ deploymentID: deployment.id, mode: "saturation", sourceRun }} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
		await screen.findByRole("combobox", { name: zh.wizard.priorityClass });
		return user;
	};
	let user = await rerun("high", true);
	await screen.findByTitle("high (20)");
	await user.click(screen.getByRole("button", { name: /下一步/ }));
	assert.equal((await start(user)).priority_class, "high");
	cleanup();
	calls.length = 0;
	user = await rerun("gone", false);
	await screen.findByTitle(zh.wizard.priorityClassDefault.replace("{{name}}", "normal"));
	await screen.findByText(zh.wizard.priorityClassNotScheduled);
	await user.click(screen.getByRole("button", { name: /下一步/ }));
	assert.equal("priority_class" in (await start(user)), false);
});
