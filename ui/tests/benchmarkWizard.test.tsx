import assert from "node:assert/strict";
import { afterEach, beforeEach, mock, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "HTMLBodyElement", "HTMLInputElement", "Document", "KeyboardEvent"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const { cleanup, render, screen, fireEvent, within, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: BenchmarkWizard } = await import("../src/pages/evaluations/BenchmarkWizard.tsx");
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: EvaluationsPage } = await import("../src/pages/EvaluationsPage.tsx");
const locale = (await import("../src/i18n/locales/en/evaluations.json")).default;
const common = (await import("../src/i18n/locales/en/common.json")).default;
const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "en", defaultNS: "evaluations", interpolation: { escapeValue: false },
	resources: { en: { evaluations: locale, common } },
});

Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "EventSource", { value: class { addEventListener() {} removeEventListener() {} close() {} }, configurable: true });

type BenchmarkMeta = import("../src/api.ts").BenchmarkMeta;
type WizardPreset = import("../src/pages/evaluations/BenchmarkWizard.tsx").WizardPreset;
const meta: BenchmarkMeta = {
	modes: [
		{ value: "saturation", label: { en: "Throughput Limit Probe" } },
		{ value: "sla", label: { en: "SLA Maximum Concurrency" } },
		{ value: "grid", label: { en: "Parameter Sweep" } },
	],
	mode_defaults: {
		saturation: { concurrency: 1, max_concurrency: 32, isl: [4096], osl: [1024], requests_per_point_multiplier: 10, min_throughput_gain: .05 },
		sla: { concurrency: 1, max_concurrency: 32, isl: [4096], osl: [1024], requests_per_point_multiplier: 10, objective: "rps" },
		grid: { concurrency: [1, 2, 4], isl: [4096], osl: [1024], requests_per_point_multiplier: 10 },
	},
	datasets: [{ value: "random", label: { en: "Fixed-Length Random" } }], dataset_profiles: {},
	agentic_presets: [],
	agentic_defaults: { preset: "agentic-80k-v1", turns: 13, subsequent_turn_input_tokens: 753, output_tokens: 220, seed: 42, concurrency: [1, 2, 4, 8], sessions_per_point: { "1": 4, "2": 8, "4": 8, "8": 16 }, sessions_per_concurrency: 2, min_sessions_per_point: 4 },
	sla_constraints: [], objectives: [], statuses: [], tokenizer_schemes: {},
	detail_cap_mb: { default: 64, min: 1, max: 512, unit: "MB" },
	load_defaults: { warmup_requests: 1, max_warmup_requests: 1000, request_timeout_s: 600, min_request_timeout_s: 10, max_request_timeout_s: 7200, burstiness: 1, point_duration_s: 60, min_point_duration_s: 5, max_point_duration_s: 3600, min_open_loop_requests: 20, max_request_rate: 10000, max_request_rates: 32, max_burstiness: 100, max_in_flight: 100000, ignore_eos: true, ignore_eos_datasets: ["random", "agentic"] },
};
const deployment = { id: 7, name: "Test service", node_uuid: "node-1", model_id: 1, served_name: "served", engine: "vllm", desired_state: "active", runtime_state: "running", sleepable: false, replicas: 1, template_deviated: false, created_at: "2026-10-21T00:00:00Z", operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions", renderer: "chat" }] } satisfies import("../src/api.ts").DeploymentListItem;

function wizard(preset?: WizardPreset, open = true) {
	return <I18nextProvider i18n={i18n}><BenchmarkWizard open={open} meta={meta} deployments={[deployment]} nodes={[]} preset={preset} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>;
}
let restoreFetch = () => {};
beforeEach(() => { mock.method(api, "deployment", async () => ({ ...deployment, instances: [{ uuid: "leader-1", deployment_id: deployment.id, node_uuid: "node-1", state: "running", sleep_level: 0, node_rank: 0, api_port: 8000 }] })); });
afterEach(() => { cleanup(); mock.restoreAll(); restoreFetch(); });

function externalServerFixture() {
	const original = globalThis.fetch;
	let allowed = false;
	const calls: { method: string; path: string; body?: Record<string, unknown> }[] = [];
	globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
		const path = new URL(String(input), "http://localhost").pathname;
		const method = init?.method || "GET";
		const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
		calls.push({ method, path, body });
		const json = (status: number, value: unknown) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
		if (path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
		if (path === "/api/v1/nodes/load-node") return json(200, { uuid: "load-node", name: "Bench node", state: "online", caps: ["bench"] });
		if (path === "/api/v1/server/outbound-policy" && method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal"], editable: true, revision: "r1" });
		if (path.endsWith("/outbound-policy/check")) return json(200, { items: [{ host: "bench.internal", status: allowed ? "allowed" : "needs_allow" }], total: 1 });
		if (path === "/api/v1/server/outbound-policy" && method === "PUT") {
			assert.deepEqual(body, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "bench.internal"], revision: "r1" });
			allowed = true;
			return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "bench.internal"], editable: true, revision: "r2" });
		}
		if (path === "/api/v1/benchmarks/models") return allowed || (body?.node_uuid && body.node_uuid !== "server") ? json(200, { models: ["served"], normalized_url: "http://bench.internal:8000" }) : json(403, { error: "outbound URL is not permitted" });
		if (path === "/api/v1/benchmarks" && method === "POST") return allowed || (body?.load_origin && body.load_origin !== "server") ? json(200, { id: 91, name: "Private run" }) : json(403, { error: "outbound URL is not permitted" });
		return json(599, { error: `unexpected ${method} ${path}` });
	}) as typeof fetch;
	restoreFetch = () => { globalThis.fetch = original; };
	const sourceRun = { id: 90, name: "External private", mode: "grid", target_kind: "external", base_url: "http://bench.internal:8000/v1", model: "served", load_origin: "server", tokenizer_ref: "engine:", dataset: { kind: "random" }, search: { concurrency: [1], isl: [128], osl: [32] } } as import("../src/api.ts").BenchmarkRun;
	return { calls, sourceRun };
}

test("an external server-origin rerun authorizes its checked current host before creation", async () => {
	const { calls, sourceRun } = externalServerFixture();
	render(wizard({ sourceRun }));
	const user = userEvent.setup({ document: dom.window.document });
	await user.click(await screen.findByRole("checkbox", { name: /bench\.internal/ }));
	await user.click(screen.getByRole("button", { name: locale.wizard.next }));
	await user.click(await screen.findByRole("button", { name: locale.wizard.start }));
	await screen.findByText(locale.wizard.created);
	assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
	const create = calls.find((call) => call.path === "/api/v1/benchmarks" && call.method === "POST");
	assert.equal(create?.body?.base_url, "http://bench.internal:8000");
	assert.equal(create?.body?.load_origin, "server");
	assert.ok(calls.findIndex((call) => call.method === "PUT") < calls.indexOf(create!), "permission is saved before creating the run");
});

test("external model probing saves a host only after explicit consent", async () => {
	const { calls, sourceRun } = externalServerFixture();
	render(wizard({ sourceRun }));
	const user = userEvent.setup({ document: dom.window.document });
	const allow = await screen.findByRole("checkbox", { name: /bench\.internal/ });
	await user.click(screen.getByRole("button", { name: new RegExp(`${locale.wizard.probe}$`) }));
	await waitFor(() => assert.equal(calls.filter((call) => call.path === "/api/v1/benchmarks/models").length, 1));
	assert.equal(calls.some((call) => call.method === "PUT"), false, "an unchecked probe does not change policy");
	await user.click(allow);
	await user.click(screen.getByRole("button", { name: new RegExp(`${locale.wizard.probe}$`) }));
	await waitFor(() => assert.equal(calls.filter((call) => call.path === "/api/v1/benchmarks/models").length, 2));
	assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
	await screen.findByText(i18n.t("wizard.probeSuccess", { count: 1 }));
	const secondProbe = calls.filter((call) => call.path === "/api/v1/benchmarks/models")[1];
	assert.ok(calls.findIndex((call) => call.method === "PUT") < calls.indexOf(secondProbe));
});

test("a node-origin external rerun probes from that node without management-host authorization", async () => {
	const { calls, sourceRun } = externalServerFixture();
	const loadNode = { uuid: "load-node", name: "Bench node", hostname: "bench-node", state: "online", caps: ["bench"] } as import("../src/api.ts").Node;
	render(<I18nextProvider i18n={i18n}><BenchmarkWizard open meta={meta} deployments={[]} nodes={[loadNode]} preset={{ sourceRun: { ...sourceRun, load_origin: "load-node" } }} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
	const user = userEvent.setup({ document: dom.window.document });
	await screen.findByText("Compute node · Bench node");
	await user.click(screen.getByRole("button", { name: new RegExp(`${locale.wizard.probe}$`) }));
	await screen.findByText(locale.wizard.v1Removed);
	assert.equal(screen.queryByRole("checkbox", { name: /bench\.internal/ }), null);
	assert.equal(calls.some((call) => call.path.includes("outbound-policy")), false);
	assert.equal(calls.find((call) => call.path === "/api/v1/benchmarks/models")?.body?.node_uuid, "load-node");
});

function page(entry = "/evaluations") {
	mock.method(api, "benchmarks", async () => ({ items: [], total: 0 }));
	mock.method(api, "benchmarkMeta", async () => meta);
	mock.method(api, "deploymentsPage", async () => ({ items: [deployment], total: 1, page: 1, page_size: 50 }));
	mock.method(api, "nodesPage", async () => ({ items: [], total: 0, page: 1, page_size: 50 }));
	return render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[entry]}><EvaluationsPage /></MemoryRouter></I18nextProvider>);
}

for (const mode of ["saturation", "sla", "grid"] as const) {
	test(`choosing the ${mode} page card opens configuration and preserves its defaults`, async () => {
		const view = page();
		const label = meta.modes.find((item) => item.value === mode)!.label.en;
		const card = await within(view.container).findByText(label!);
		fireEvent.click(card);
		assert.equal(screen.queryAllByRole("textbox", { name: locale.wizard.name }).length, 1);
		const user = userEvent.setup({ document: dom.window.document });
		// Target validation still blocks advancing before a service is selected.
		await user.click(screen.getByRole("button", { name: locale.wizard.next }));
		await screen.findByText(locale.wizard.serviceRequired);
		await user.click(screen.getByRole("button", { name: locale.wizard.previous }));
		const next = screen.getByRole("button", { name: locale.wizard.next });
		next.focus();
		await user.keyboard("{Enter}");
		assert.equal(screen.queryAllByRole("textbox", { name: locale.wizard.name }).length, 1);
		await user.click(screen.getByRole("combobox", { name: locale.wizard.service }));
		await user.click(screen.getByText("Test service", { selector: ".ant-select-item-option-content" }));
		await user.click(screen.getByRole("button", { name: locale.wizard.next }));
		await screen.findByText(i18n.t("wizard.currentQuestion", { question: label }));
		const concurrency = screen.getByRole("textbox", { name: mode === "grid" ? locale.wizard.concurrencyList : locale.wizard.initialConcurrency }) as HTMLInputElement;
		assert.equal(concurrency.value, mode === "grid" ? "1,2,4" : "1");
		assert.equal((screen.getByRole("textbox", { name: locale.wizard.isl }) as HTMLInputElement).value, "4096");
		assert.equal((screen.getByRole("textbox", { name: locale.wizard.osl }) as HTMLInputElement).value, "1024");
	});
}

test("no preset starts with question selection", () => {
	render(wizard());
	assert.equal(screen.queryAllByRole("textbox", { name: locale.wizard.name }).length, 0);
	assert.equal(screen.getByRole("button", { name: locale.wizard.previous }).hasAttribute("disabled"), true);
	fireEvent.click(screen.getByText("Parameter Sweep", { exact: true }));
	assert.equal(screen.queryAllByRole("textbox", { name: locale.wizard.name }).length, 1);
});

test("closing and reopening with another mode returns to target configuration", () => {
	const view = render(wizard({ mode: "grid" }));
	fireEvent.click(screen.getByRole("button", { name: locale.wizard.previous }));
	view.rerender(wizard({ mode: "grid" }, false));
	view.rerender(wizard({ mode: "sla" }));
	assert.equal(screen.queryAllByRole("textbox", { name: locale.wizard.name }).length, 1);
});

test("the generic new evaluation button still starts with question selection", async () => {
	page();
	await screen.findByText("Parameter Sweep");
	fireEvent.click(screen.getByRole("button", { name: new RegExp(locale.list.new) }));
	assert.equal(screen.queryAllByRole("textbox", { name: locale.wizard.name }).length, 0);
	assert.equal(screen.getByRole("button", { name: locale.wizard.previous }).hasAttribute("disabled"), true);
});

for (const mode of ["saturation", "sla", "grid"] as const) {
	test(`a ${mode} deep link opens configuration without selecting a deployment first`, async () => {
		page(`/evaluations?create=${mode}`);
		await screen.findByRole("textbox", { name: locale.wizard.name });
	});
}

test("rerun still restores the previous target and run name", () => {
	const sourceRun = { id: 23, name: "Prior run", mode: "sla", target_kind: "instance", deployment_id: 7, dataset: { kind: "random" }, search: { concurrency: 2, isl: [1024], osl: [128] } } as import("../src/api.ts").BenchmarkRun;
	render(wizard({ sourceRun }));
	assert.equal((screen.getByRole("textbox", { name: locale.wizard.name }) as HTMLInputElement).value, "Prior run - Rerun");
	assert.equal(screen.queryAllByText("Test service").length, 1);
});
