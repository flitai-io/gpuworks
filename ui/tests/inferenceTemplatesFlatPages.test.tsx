import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "HTMLBodyElement", "HTMLHtmlElement", "HTMLInputElement", "HTMLTextAreaElement", "MutationObserver", "ShadowRoot", "File", "Blob"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    inferenceTemplates: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/inferenceTemplates.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
  } },
  defaultNS: "inferenceTemplates",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: InferenceTemplatesPage, loadAllInferenceTemplates } = await import("../src/pages/InferenceTemplatesPage.tsx");
const { listQueryString } = await import("../src/api.ts");
const { default: InferenceTemplateDetailPage } = await import("../src/pages/InferenceTemplateDetailPage.tsx");
const { default: TemplateWarnings } = await import("../src/components/TemplateWarnings.tsx");

const original = {
  inferenceTemplates: api.inferenceTemplates,
  inferenceTemplate: api.inferenceTemplate,
  inferenceTemplateYAML: api.inferenceTemplateYAML,
  inferenceTemplateChips: api.inferenceTemplateChips,
  detectedInferenceTemplateChips: api.detectedInferenceTemplateChips,
  importInferenceTemplates: api.importInferenceTemplates,
  cloneInferenceTemplate: api.cloneInferenceTemplate,
  deleteInferenceTemplateSpec: api.deleteInferenceTemplateSpec,
  nodes: api.nodes,
  nodesPage: api.nodesPage,
  nodeFacets: api.nodeFacets,
  evaluate: api.evaluate,
};

const h100Spec = {
  id: 1, source: "upstream", name: "h100-fp8", display_name_zh_cn: "H100 · FP8", display_name_en: "H100 · FP8 spec", scenario: "balanced", engine: "vllm", image: "vllm/image:latest", quantization: "fp8",
  weight_sources: [{ provider: "huggingface", repo_id: "deepseek-ai/model" }], gpus: { vendor: "nvidia", cc: "9.0", name: "H100" }, args: ["--kv-cache-dtype", "fp8"],
  api: { operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions", mode: "sync", request_encodings: ["json"], renderer: "chat", response: "json" }] },
};
const ascendSpec = {
  id: 2, source: "upstream", name: "ascend-w8a8", display_name_zh_cn: "910B (A2) · INT8 W8A8", display_name_en: "910B (A2) · INT8 W8A8 spec", scenario: "throughput", engine: "sglang", image: "ascend/image@sha256:abc", quantization: "w8a8",
  weight_sources: [{ provider: "modelscope", repo_id: "deepseek-ai/model" }], gpus: { vendor: "ascend", name: "910B4" }, pre_start_argv: ["apply-patches"],
};
const upstreamModel = { id: 7, model_id: "deepseek-ai/model", display_title: "DeepSeek Model", source: "upstream", specs: [h100Spec, ascendSpec] };

// What the server's editor-YAML endpoint answers: the flat catalog document
// written from the Go catalog types, including facts the UI has no field for
// (runtime_weight_bytes, pd_mode, kv_bytes_per_token) that a browser-side
// serializer once dropped on save.
function serverYAML(modelID: string) {
  return `models:
  - model_id: ${modelID}
    source: user
    display_title: Served Title
    kv_bytes_per_token:
      vllm: 49152
    specs:
      - name: h100-fp8
        engine: vllm
        image: vllm/image:latest
        weights:
          quantization: fp8
          sources:
            - huggingface: deepseek-ai/model
        runtime_weight_bytes: 12884901888
        gpus:
          vendor: nvidia
        pd_mode: vllm-nixl
`;
}

function renderList() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><InferenceTemplatesPage /></MemoryRouter></I18nextProvider>);
}

function renderDetail(path = "/inference-templates/deepseek-ai/model") {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[path]}><Routes><Route path="/inference-templates/*" element={<InferenceTemplateDetailPage />} /><Route path="/deploy" element={<div>deploy page</div>} /><Route path="/playground/*" element={<RouteLocation />} /></Routes></MemoryRouter></I18nextProvider>);
}

function RouteLocation() {
  const location = useLocation();
  return <div>{location.pathname}{location.search}</div>;
}

function renderCatalogFlow(initialEntry = "/inference-templates") {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[initialEntry]}><Routes><Route path="/inference-templates" element={<InferenceTemplatesPage />} /><Route path="/inference-templates/*" element={<InferenceTemplateDetailPage />} /></Routes></MemoryRouter></I18nextProvider>);
}

// Tests that open the editor get the server-written document of the model they show.
beforeEach(() => {
  api.inferenceTemplateYAML = (async (modelID: string) => serverYAML(modelID)) as never;
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  Object.assign(api, original);
});

test("flat catalog exposes loading, success, keyboard navigation, and chip filtering", async () => {
  let resolvePage!: (value: never) => void;
  api.inferenceTemplates = (() => new Promise((resolve) => { resolvePage = resolve as never; })) as never;
  api.detectedInferenceTemplateChips = (async () => list([{ vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" }])) as never;
  api.inferenceTemplateChips = (async () => list([{ vendor: "nvidia", name: "NVIDIA H100", cc: 9, category: "Data Center", architecture: "Hopper" }])) as never;
  const { container } = renderList();
  assert.ok(container.querySelector(".ant-spin-spinning"));
  resolvePage({ items: [upstreamModel], page: 1, page_size: 200, total: 1 } as never);
  assert.equal((await screen.findByText("DeepSeek Model")).textContent, "DeepSeek Model");
  const open = screen.getByRole("button", { name: "DeepSeek Model" });
  open.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(dom.window.location.pathname, "/", "MemoryRouter navigation remains isolated from the browser URL");
});

test("catalog starts unfiltered, keeps organization logos and cross-organization weight artifacts, then filters on an explicit chip click", async () => {
  const qwen = {
    id: 8, model_id: "qwen/qwen-model", display_title: "Qwen Model", source: "upstream",
    specs: [{ ...ascendSpec, id: 3, weight_sources: [{ provider: "huggingface", repo_id: "quant-factory/Qwen-GPTQ" }], quantization: "gptq" }],
  };
  localStorage.setItem("gpuworks.inferenceTemplates.selectedChip", "nvidia\u0000NVIDIA H100\u00009.0");
  api.inferenceTemplates = (async (query) => query.extra?.gpu
    ? { items: [{ ...upstreamModel, specs: [h100Spec] }], page: 1, page_size: 200, total: 1 }
    : { items: [upstreamModel, qwen], page: 1, page_size: 200, total: 2 }) as never;
  api.detectedInferenceTemplateChips = (async () => list([{ vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" }])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  const { container } = renderList();

  assert.equal((await screen.findByText("DeepSeek Model")).textContent, "DeepSeek Model");
  assert.equal(screen.queryByText(/Selected chip:/) === null, true, "a previous-session chip must not filter the initial catalog");
  const qwenOrganization = screen.getByRole("button", { name: /Qwen/ });
  await userEvent.setup().click(qwenOrganization);
  assert.equal((await screen.findByText("Qwen Model")).textContent, "Qwen Model");
  assert.equal(screen.getByText("GPTQ · quant-factory/Qwen-GPTQ").textContent, "GPTQ · quant-factory/Qwen-GPTQ");
  assert.ok(container.querySelector('img[src="/model-org-icons/qwen.webp"]'), "the reviewed organization logo is rendered");
  for (const logo of container.querySelectorAll<HTMLElement>(".ant-avatar-image")) {
    assert.equal(logo.style.background, "", "a published logo draws without a tinted disc behind it");
    assert.ok(logo.classList.contains("template-organization-logo"));
  }

  await userEvent.setup().click(screen.getByRole("button", { name: "H100" }));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: /Qwen/ }) === null, true));
  assert.equal((await screen.findByText("DeepSeek Model")).textContent, "DeepSeek Model");
});

test("my chips use short GPU names and reserve compute capability for an unknown vGPU model", async () => {
  api.inferenceTemplates = (async () => ({ items: [upstreamModel], page: 1, page_size: 200, total: 1 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([
    { vendor: "nvidia", name: "NVIDIA GeForce RTX 2080 Ti", cc: "7.5" },
    { vendor: "nvidia", name: "Quadro RTX 8000", cc: "7.5" },
    { vendor: "nvidia", name: "NVIDIA A40", cc: "8.6" },
    { vendor: "nvidia", name: "NVIDIA vGPU-32GB", cc: "8.9" },
  ])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  renderList();

  await screen.findByText("DeepSeek Model");
  assert.ok(screen.getByRole("button", { name: "RTX 2080 Ti" }));
  assert.ok(screen.getByRole("button", { name: "RTX 8000" }));
  assert.ok(screen.getByRole("button", { name: "A40" }));
  assert.ok(screen.getByRole("button", { name: "vGPU 32GB · CC 8.9" }));
  assert.equal(screen.queryByRole("button", { name: /RTX 2080 Ti.*CC/ }) === null, true);
});

test("operation counts intersect with the selected chip and engine", async () => {
  const multimodal = { ...upstreamModel, specs: [{ ...h100Spec, api: { operations: [{ type: "openai.image.generate", endpoint: "/v1/images", mode: "sync", request_encodings: ["json"], renderer: "image", response: "image" }] } }] };
  const text = { ...upstreamModel, id: 8, model_id: "qwen/text", display_title: "Text Model", specs: [ascendSpec] };
  const vector = { ...upstreamModel, id: 9, model_id: "jinaai/embed", display_title: "Embed Model", specs: [{ ...h100Spec, api: { operations: [{ type: "openai.embeddings", endpoint: "/v1/embeddings", mode: "sync", request_encodings: ["json"], renderer: "vector", response: "json" }] } }] };
  api.inferenceTemplates = (async (query) => query.extra?.gpu
    ? { items: [multimodal, vector], page: 1, page_size: 200, total: 2 }
    : { items: [multimodal, text, vector], page: 1, page_size: 200, total: 3 }) as never;
  api.detectedInferenceTemplateChips = (async () => list([
    { vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" },
    { vendor: "ascend", name: "Ascend 910B4" },
  ])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  renderList();

  await screen.findByText("DeepSeek Model");
  const operationButton = (label: string) => screen.getByText(label).closest("button") as HTMLButtonElement;
  assert.equal(operationButton("Chat").textContent, "Chat0");
  assert.equal(operationButton("Image generation").textContent, "Image generation1");
  assert.equal(operationButton("Embeddings").textContent, "Embeddings1");
  await userEvent.setup().click(screen.getByRole("button", { name: "H100" }));
  await waitFor(() => assert.equal(operationButton("Chat").textContent, "Chat0"));
  assert.equal(operationButton("Image generation").textContent, "Image generation1");
  assert.equal(operationButton("Embeddings").textContent, "Embeddings1");
  await userEvent.setup().click(screen.getByRole("tab", { name: "SGLang" }));
  await waitFor(() => assert.equal(operationButton("Chat").textContent, "Chat0"));
  assert.equal(operationButton("Embeddings").textContent, "Embeddings0", "chip and engine must match the same spec");
  await userEvent.setup().click(screen.getByRole("tab", { name: "vLLM" }));
  await userEvent.setup().click(screen.getByRole("button", { name: "H100" }));
  await userEvent.setup().click(operationButton("Image generation"));
  await waitFor(() => assert.ok(screen.getByText("1 original models")));
});

test("catalog loader fetches every API page beyond the 200-model page limit", async () => {
  const calls: string[] = [];
  const models = await loadAllInferenceTemplates(async (query) => {
    calls.push(listQueryString(query));
    const page = query.page;
    return { items: [{ ...upstreamModel, id: page, model_id: `org/model-${page}` }], page, page_size: 200, total: 401 };
  });
  assert.deepEqual(calls, ["page=1&page_size=200", "page=2&page_size=200", "page=3&page_size=200"]);
  assert.deepEqual(models.map((model) => model.model_id), ["org/model-1", "org/model-2", "org/model-3"]);
});

test("returning from any model detail restores the catalog organization and filters", async () => {
  const paddle = {
    ...upstreamModel,
    id: 12,
    model_id: "paddlepaddle/paddleocr-vl-1.6",
    display_title: "PaddleOCR VL 1.6",
    input_modalities: ["image"],
    specs: [h100Spec],
  };
  api.inferenceTemplates = (async () => ({ items: [upstreamModel, paddle], page: 1, page_size: 200, total: 2 })) as never;
  api.inferenceTemplate = (async (id) => {
    assert.equal(id, paddle.model_id);
    return paddle;
  }) as never;
  api.detectedInferenceTemplateChips = (async () => list([{ vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" }])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  const { container } = renderCatalogFlow();

  await screen.findByText("DeepSeek Model");
  await userEvent.setup().click(screen.getByRole("button", { name: /PaddlePaddle/ }));
  await userEvent.setup().click(screen.getByRole("button", { name: "H100" }));
  await userEvent.setup().click(screen.getByRole("tab", { name: "vLLM" }));
  await userEvent.setup().click(screen.getByText("Chat"));
  const search = screen.getByPlaceholderText("Search models or weight repositories");
  await userEvent.setup().type(search, "Paddle");
  await userEvent.setup().click(screen.getByRole("button", { name: "PaddleOCR VL 1.6" }));

  assert.ok((await screen.findAllByText("paddlepaddle/paddleocr-vl-1.6")).length >= 1);
  await userEvent.setup().click(screen.getByRole("button", { name: /Back to List/ }));
  await screen.findByText("PaddleOCR VL 1.6");
  assert.match(container.querySelector(".template-organization-sidebar button.is-active")?.textContent || "", /PaddlePaddle/);
  assert.equal((screen.getByPlaceholderText("Search models or weight repositories") as HTMLInputElement).value, "Paddle");
  assert.equal(screen.getByRole("button", { name: "H100" }).getAttribute("aria-pressed"), "true");
  assert.ok(screen.getByRole("tab", { name: "vLLM" }).getAttribute("aria-selected") === "true");
  assert.equal(screen.getByText("Chat").closest("button")?.getAttribute("aria-pressed"), "true");
});

test("returning without a search term keeps the requested organization while the catalog loads", async () => {
  const arcee = { ...upstreamModel, id: 13, model_id: "arcee-ai/first", display_title: "Arcee First", specs: [h100Spec] };
  const baai = { ...upstreamModel, id: 14, model_id: "baai/bge-reranker-v2-m3", display_title: "BGE Reranker", specs: [h100Spec] };
  let resolveCatalog!: (value: never) => void;
  api.inferenceTemplates = (() => new Promise((resolve) => { resolveCatalog = resolve as never; })) as never;
  api.inferenceTemplate = (async () => baai) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  const { container } = renderCatalogFlow("/inference-templates/baai/bge-reranker-v2-m3?from=organization%3Dbaai%26engine%3Dvllm");

  await screen.findByText("BGE Reranker");
  // The breadcrumb returns to the same filtered list as the back button.
  const crumb = container.querySelector(".page-header-breadcrumb a") as HTMLAnchorElement;
  assert.equal(crumb.getAttribute("href"), "/inference-templates?organization=baai&engine=vllm");
  await userEvent.setup().click(screen.getByRole("button", { name: /Back to List/ }));
  resolveCatalog({ items: [arcee, baai], page: 1, page_size: 200, total: 2 } as never);

  await screen.findByText("BGE Reranker");
  assert.match(container.querySelector(".template-organization-sidebar button.is-active")?.textContent || "", /BAAI/);
  assert.ok(screen.getByRole("tab", { name: "vLLM" }).getAttribute("aria-selected") === "true");
  assert.equal(screen.queryByPlaceholderText("Search models or weight repositories")?.getAttribute("value") || "", "");
});

test("models inside one organization are ordered by creation date descending", async () => {
  const older = { ...upstreamModel, id: 8, model_id: "deepseek-ai/older", display_title: "Older Model", release_date: "2024-01-02" };
  const newest = { ...upstreamModel, id: 9, model_id: "deepseek-ai/newest", display_title: "Newest Model", release_date: "2026-06-03" };
  const sameDay = { ...upstreamModel, id: 10, model_id: "deepseek-ai/a-same-day", display_title: "Same Day Model", release_date: "2026-06-03" };
  api.inferenceTemplates = (async () => ({ items: [older, newest, sameDay], page: 1, page_size: 200, total: 3 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  const { container } = renderList();

  await screen.findByText("Newest Model");
  assert.deepEqual(
    [...container.querySelectorAll(".template-model-title")].map((item) => item.textContent),
    ["Same Day Model", "Newest Model", "Older Model"],
  );
});

test("flat catalog renders empty and error states, and retry is keyboard operable", async () => {
  let calls = 0;
  api.inferenceTemplates = (async () => { calls++; if (calls === 1) throw new Error("catalog offline"); return { items: [], page: 1, page_size: 200, total: 0 }; }) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  renderList();
  assert.ok((await screen.findByText(/catalog offline/)).textContent);
  const retry = screen.getByRole("button", { name: "Retry" });
  retry.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal((await screen.findByText("No models match the filters")).textContent, "No models match the filters");
  assert.equal(calls, 2);
});

test("YAML import prompts overwrite only for same-named spec conflicts and otherwise surfaces the error", async () => {
  let overwrite: boolean | undefined;
  let calls = 0;
  api.inferenceTemplates = (async () => ({ items: [], page: 1, page_size: 200, total: 0 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  api.importInferenceTemplates = (async (_file, value) => {
    calls++; overwrite = value;
    if (!value) throw new Error("inference catalog user spec already exists: org/model general");
    return { models: 1 };
  }) as never;
  const { container } = renderList();
  await screen.findByText("No models match the filters");
  const input = container.querySelector('input[type="file"]') as HTMLInputElement;
  await userEvent.setup().upload(input, new File(["models: []"], "catalog.yaml", { type: "application/yaml" }));
  const overwriteButton = await screen.findByRole("button", { name: "Overwrite" });
  assert.ok(screen.getByText(/org\/model general/), "the confirm dialog keeps the conflict detail");
  await userEvent.setup().click(overwriteButton);
  await waitFor(() => assert.equal(overwrite, true));

  // A parse error must surface as an error message, never as an overwrite prompt.
  calls = 0; overwrite = undefined;
  api.importInferenceTemplates = (async () => { calls++; throw new Error("parse inference catalog YAML: yaml: line 3: bad"); }) as never;
  await userEvent.setup().upload(input, new File(["models: []"], "catalog.yaml", { type: "application/yaml" }));
  await waitFor(() => assert.equal(calls, 1));
  assert.equal(overwrite, undefined, "no second overwrite attempt for a parse failure");
  assert.ok(await screen.findByText(/yaml: line 3: bad/));
});

// A template with adapter assets or documentation only imports as a
// .gpuworks-catalog.tar.gz package (the server refuses its bare YAML), so the
// same button that takes YAML must offer and send a package.
test("template import accepts a catalog package next to YAML and sends it as gzip", async () => {
  api.inferenceTemplates = (async () => ({ items: [], page: 1, page_size: 200, total: 0 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  const uploaded: string[] = [];
  api.importInferenceTemplates = (async (file: File) => { uploaded.push(file.name); return { models: 2 }; }) as never;
  const { container } = renderList();
  await screen.findByText("No models match the filters");
  const button = within(container).getByText("Import Templates").closest("button") as HTMLButtonElement;
  const input = container.querySelector('input[type="file"]') as HTMLInputElement;
  // userEvent applies the input's accept list, as the browser's file chooser does.
  const user = userEvent.setup();
  await user.upload(input, new File([new Uint8Array([0x1f, 0x8b, 0x08])], "glm.gpuworks-catalog.tar.gz", { type: "application/gzip" }));
  await waitFor(() => assert.deepEqual(uploaded, ["glm.gpuworks-catalog.tar.gz"]));
  await user.upload(input, new File(["models: []"], "catalog.yaml", { type: "" }));
  await waitFor(() => assert.deepEqual(uploaded, ["glm.gpuworks-catalog.tar.gz", "catalog.yaml"]));
  await user.upload(input, new File(["x"], "notes.txt", { type: "text/plain" }));
  assert.equal(uploaded.length, 2, "a file outside the catalog formats is not offered");
  assert.ok((await screen.findAllByText("Templates imported")).length > 0);
  await user.hover(button);
  assert.ok(await screen.findByText(/\.gpuworks-catalog\.tar\.gz package/));

  const requests: Array<{ url: string; type: string | null }> = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    requests.push({ url, type: new Headers(init?.headers).get("Content-Type") });
    return new Response(JSON.stringify({ models: 1 }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    await original.importInferenceTemplates(new File([new Uint8Array([0x1f, 0x8b])], "pkg.tgz"), true);
    await original.importInferenceTemplates(new File(["models: []"], "catalog.yml"));
  } finally {
    globalThis.fetch = originalFetch;
  }
  assert.deepEqual(requests, [
    { url: "/api/v1/inference-templates/import?overwrite=true", type: "application/gzip" },
    { url: "/api/v1/inference-templates/import", type: "application/yaml" },
  ]);
});

test("the editor text is requested by model_id and spec source query, and read as text", async () => {
  const requests: string[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (url: string) => {
    requests.push(url);
    if (url.includes("missing")) return new Response(JSON.stringify({ error: "not found" }), { status: 404, headers: { "Content-Type": "application/json" } });
    return new Response("models:\n  - model_id: acme/m\n", { status: 200, headers: { "Content-Type": "application/yaml; charset=utf-8" } });
  }) as typeof fetch;
  try {
    assert.equal(await original.inferenceTemplateYAML("acme/m"), "models:\n  - model_id: acme/m\n");
    await original.inferenceTemplateYAML("acme/m x", "user");
    await assert.rejects(original.inferenceTemplateYAML("acme/missing"), /not found/);
  } finally {
    globalThis.fetch = originalFetch;
  }
  assert.deepEqual(requests, [
    "/api/v1/inference-templates/yaml?model_id=acme%2Fm",
    "/api/v1/inference-templates/yaml?model_id=acme%2Fm+x&spec_source=user",
    "/api/v1/inference-templates/yaml?model_id=acme%2Fmissing",
  ]);
});

// stubFleet answers the detail's node lookups as the server does: online
// nodes' card models from GET /nodes/facets, and the nodes holding one model
// from GET /nodes?label=gpuworks.ai/gpu-model=…. Listing every node fails.
type FleetGPU = { vendor: string; name: string; cc: string; model: string; arch_id?: string };
type FleetNode = { uuid: string; state: string; gpu_vendor?: string; gpus: FleetGPU[] };
function stubFleet(fleet: FleetNode[]) {
  const requestedPages: number[] = [];
  api.nodes = (async () => { throw new Error("the detail fetched every node"); }) as never;
  api.nodeFacets = (async (filter: { state?: string[] }) => {
    const nodes = fleet.filter((node) => !filter.state?.length || filter.state.includes(node.state));
    const gpus = new Map<string, FleetGPU & { nodes: number; cards: number; memory_bytes: number }>();
    for (const node of nodes) {
      const seen = new Set<string>();
      for (const gpu of node.gpus) {
        const key = `${gpu.vendor}/${gpu.name}/${gpu.cc}/${gpu.arch_id || ""}`;
        const group = gpus.get(key) || { ...gpu, nodes: 0, cards: 0, memory_bytes: 0 };
        if (!seen.has(key)) group.nodes++;
        group.cards++;
        seen.add(key);
        gpus.set(key, group);
      }
    }
    return { total: nodes.length, labels: [], system_labels: [], gpus: [...gpus.values()] };
  }) as never;
  api.nodesPage = (async (query: { page?: number; page_size?: number; state?: string[]; label?: string[] }) => {
    const model = query.label?.find((pair) => pair.startsWith("gpuworks.ai/gpu-model="))?.split("=")[1];
    const vendor = query.label?.find((pair) => pair.startsWith("gpuworks.ai/vendor="))?.split("=")[1];
    const matched = fleet.filter((node) =>
      (!query.state?.length || query.state.includes(node.state)) &&
      (!model || node.gpus.some((gpu) => gpu.model === model)) &&
      (!vendor || (node.gpu_vendor || node.gpus[0]?.vendor) === vendor));
    const page = query.page || 1;
    const pageSize = query.page_size || 50;
    requestedPages.push(page);
    const items = matched.slice((page - 1) * pageSize, page * pageSize).map((node) => ({
      ...node,
      gpu_vendor: node.gpu_vendor || node.gpus[0]?.vendor || "",
      // Facets expose cc; the node inventory's wire field is compute_capability.
      gpus: node.gpus.map((gpu, index) => ({
        index, vendor: gpu.vendor, name: gpu.name, compute_capability: gpu.cc, arch_id: gpu.arch_id,
      })),
    }));
    return { items, page, page_size: pageSize, total: matched.length, all: fleet.length, labels: [] };
  }) as never;
  return requestedPages;
}

test("detail prioritizes vLLM, groups specs by GPU vendor, and keyboard-opens a command drawer", async () => {
  api.inferenceTemplate = (async () => upstreamModel) as never;
  api.detectedInferenceTemplateChips = (async () => list([{ vendor: "ascend", name: "Ascend 910B4" }])) as never;
  stubFleet([{ uuid: "ascend", state: "online", gpus: [{ vendor: "ascend", name: "Ascend 910B4", cc: "", model: "910B4" }] }]);
  api.evaluate = (async () => ({ ok: true, command: ["vllm", "serve", "/model"], argv: [], image: "vllm/image" })) as never;
  renderDetail();
  assert.equal((await screen.findByText("H100 · FP8 spec")).textContent, "H100 · FP8 spec");
  assert.ok(screen.getByText("NVIDIA · 1 specs"));
  assert.equal(screen.queryByText("910B (A2) · INT8 W8A8 spec") === null, true);
  const details = screen.getByRole("button", { name: "Details" });
  details.focus();
  await userEvent.setup().keyboard("{Enter}");
  await waitFor(() => assert.match(document.querySelector(".template-command-preview")?.textContent || "", /vllm/));
  await userEvent.setup().click(screen.getByText("SGLang · sglang"));
  assert.equal((await screen.findByText("910B (A2) · INT8 W8A8 spec")).textContent, "910B (A2) · INT8 W8A8 spec");
  assert.ok(screen.getByText("Ascend · 1 specs"));
});

test("the spec drawer lists the template's image sources in declared order, and only when it declares some", async () => {
  const sources = ["swr.example.com/org/image@sha256:abc", "ghcr.nju.edu.cn/org/image:v1"];
  const withSources = { ...upstreamModel, specs: [{ ...h100Spec, image_sources: sources }, ascendSpec] };
  api.inferenceTemplate = (async () => withSources) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  stubFleet([]);
  api.evaluate = (async () => ({ ok: true, command: ["vllm", "serve", "/model"], argv: [], image: "vllm/image:latest" })) as never;
  renderDetail();
  await screen.findAllByText("H100 · FP8 spec");
  await userEvent.setup().click(screen.getByRole("button", { name: "Details" }));
  assert.ok(await screen.findByText("Image sources"));
  const shown = sources.map((source) => screen.getByText(source));
  assert.equal(shown[0].compareDocumentPosition(shown[1]) & dom.window.Node.DOCUMENT_POSITION_FOLLOWING, dom.window.Node.DOCUMENT_POSITION_FOLLOWING, "declared order is kept");

  cleanup();
  api.inferenceTemplate = (async () => upstreamModel) as never;
  renderDetail();
  await screen.findAllByText("H100 · FP8 spec");
  await userEvent.setup().click(screen.getByRole("button", { name: "Details" }));
  await screen.findByText("Weight sources");
  assert.equal(screen.queryByText("Image sources") === null, true, "a spec without sources has no such row");
  assert.ok(screen.getByText("NVIDIA · CC 9.0 · H100"), "the GPU row shows the predicate the spec really matches, not its name");
});

test("detail previews an explicit spec without an online matching node", async () => {
  let evaluatedNode: string | undefined;
  api.inferenceTemplate = (async () => upstreamModel) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  stubFleet([]);
  api.evaluate = (async (request) => {
    evaluatedNode = request.node_uuid;
    return { ok: true, command: ["vllm", "serve", "/model", "--tensor-parallel-size", "$TP_SIZE"], argv: [], image: "vllm/image:latest" };
  }) as never;
  renderDetail();
  await screen.findAllByText("H100 · FP8 spec");
  await userEvent.setup().click(screen.getByRole("button", { name: "Details" }));
  assert.ok((await screen.findByText(/\$TP_SIZE/)).textContent);
  assert.equal(evaluatedNode, undefined);
  assert.equal(screen.queryByText(/matching online GPU node/i) === null, true);
});

test("detail delegates preview node selection to the server instead of inspecting fleet cards", async () => {
  let request: Record<string, unknown> | undefined;
  const amdSpec = { ...h100Spec, name: "amd-gfx1151", quantization: "bf16", gpus: { vendor: "amd", name: "(?i)Radeon 8060S", arch_id: "gfx1151" } };
  api.inferenceTemplate = (async () => ({ ...upstreamModel, specs: [amdSpec] })) as never;
  api.nodeFacets = (async () => { throw new Error("client must not choose a card"); }) as never;
  api.nodesPage = (async () => { throw new Error("client must not choose a node"); }) as never;
  api.evaluate = (async (value) => {
    request = value as Record<string, unknown>;
    return { ok: true, selected_node: { uuid: "mixed-gfx1151" }, command: ["vllm", "server-selected"], argv: [], image: "vllm/image" };
  }) as never;
  renderDetail();
  await userEvent.setup().click(await screen.findByRole("button", { name: "Details" }));
  await screen.findByText(/server-selected/);
  assert.equal(request?.placement_mode, "auto");
  assert.equal(request?.template_spec_name, "amd-gfx1151");
  assert.equal(request?.node_uuid, undefined);
  assert.equal(request?.node_uuids, undefined);
});

test("detail usage opens the declared operation in Playground with template and spec identity", async () => {
  const videoSpec = {
    ...h100Spec,
    name: "video-diffusion",
    api: {
      health_endpoint: "/health",
      operations: [{
        type: "openai.video.generate", endpoint: "/v1/videos", mode: "async", request_encodings: ["json"], renderer: "video", response: "video",
        async: { id_field: "id", status_field: "status", success_statuses: ["completed"], failure_statuses: ["failed"], poll_method: "GET", poll_endpoint: "{endpoint}/{id}", content_method: "GET", content_endpoint: "{endpoint}/{id}/content", cancel_method: "DELETE", cancel_endpoint: "{endpoint}/{id}" },
        example: { request: { method: "POST", endpoint: "/v1/videos", content_type: "application/json", body: { model: "<served-model>", prompt: "hello" } }, curl: "curl video", python: "requests.post('video')" },
      }],
    },
  };
  api.inferenceTemplate = (async () => ({ ...upstreamModel, specs: [videoSpec] })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  stubFleet([]);
  api.evaluate = (async () => ({ ok: true, command: ["python", "video.py"], argv: [], image: "video/image" })) as never;
  renderDetail();

  await screen.findByText("H100 · FP8 spec");
  await userEvent.setup().click(screen.getByRole("button", { name: "Details" }));
  await userEvent.setup().click(await screen.findByText("Usage"));
  assert.ok(await screen.findByText("/v1/videos"));
  assert.ok(screen.getByText("curl video"));
  await userEvent.setup().click(screen.getByRole("button", { name: "Open in Playground" }));
  assert.equal((await screen.findByText("/playground/video?template_id=7&spec_source=upstream&spec=video-diffusion&operation=openai.video.generate")).textContent, "/playground/video?template_id=7&spec_source=upstream&spec=video-diffusion&operation=openai.video.generate");
});

test("mixed original model editor exposes only user-owned specs", async () => {
  const userSpec = { ...h100Spec, id: 3, source: "user", name: "my-tuned" };
  api.inferenceTemplate = (async () => ({ ...upstreamModel, specs: [h100Spec, userSpec] })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  const requests: Array<[string, string | undefined]> = [];
  api.inferenceTemplateYAML = (async (modelID: string, specSource?: string) => {
    requests.push([modelID, specSource]);
    return serverYAML(modelID).replace("name: h100-fp8", "name: my-tuned");
  }) as never;
  renderDetail();
  await screen.findAllByText("H100 · FP8 spec");
  assert.deepEqual(requests, [], "the detail page does not fetch the editor text until the editor opens");
  await userEvent.setup().click(screen.getByRole("button", { name: /Edit$/ }));
  const editor = await screen.findByRole("textbox", { name: "Edit flat YAML" });
  assert.match((editor as HTMLTextAreaElement).value, /name: my-tuned/);
  assert.doesNotMatch((editor as HTMLTextAreaElement).value, /name: h100-fp8/);
  assert.deepEqual(requests, [["deepseek-ai/model", "user"]], "an upstream template is edited through its user specs");
});

test("editor shows the server's YAML and saves it unchanged, so nothing the UI cannot model is lost", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [{ ...h100Spec, source: "user" }] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  const requests: Array<[string, string | undefined]> = [];
  const document = serverYAML("custom/model");
  api.inferenceTemplateYAML = (async (modelID: string, specSource?: string) => { requests.push([modelID, specSource]); return document; }) as never;
  const uploads: Array<{ name: string; type: string; text: string; overwrite?: boolean }> = [];
  api.importInferenceTemplates = (async (file: File, overwrite?: boolean) => { uploads.push({ name: file.name, type: file.type, text: await file.text(), overwrite }); return { models: 1 }; }) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  const editor = (await screen.findByRole("textbox", { name: "Edit flat YAML" })) as HTMLTextAreaElement;
  assert.equal(editor.value, document);
  assert.deepEqual(requests, [["custom/model", undefined]], "a user template is edited whole");
  await userEvent.setup().click(screen.getByRole("button", { name: /Save/ }));
  await waitFor(() => assert.equal(uploads.length, 1));
  assert.deepEqual(uploads[0], { name: "custom.yaml", type: "application/yaml", text: document, overwrite: true });
  assert.match(uploads[0].text, /runtime_weight_bytes: 12884901888/);
  assert.match(uploads[0].text, /pd_mode: vllm-nixl/);
  assert.match(uploads[0].text, /kv_bytes_per_token:/);
});

test("editor reports a failed YAML read and retries it", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [{ ...h100Spec, source: "user" }] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  let calls = 0;
  api.inferenceTemplateYAML = (async () => {
    calls++;
    if (calls === 1) throw new Error("catalog offline");
    return serverYAML("custom/model");
  }) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  assert.match((await screen.findByText(/Failed to load the template YAML: catalog offline/)).textContent || "", /catalog offline/);
  assert.equal(screen.queryByRole("textbox", { name: "Edit flat YAML" }) === null, true, "no editor to type into while the text is unknown");
  assert.equal((screen.getByRole("button", { name: /Save/ }) as HTMLButtonElement).disabled, true, "nothing can be saved over a template whose text was never read");
  await userEvent.setup().click(screen.getByRole("button", { name: "Retry" }));
  const editor = (await screen.findByRole("textbox", { name: "Edit flat YAML" })) as HTMLTextAreaElement;
  assert.equal(editor.value, serverYAML("custom/model"));
  assert.equal(calls, 2);
});

test("custom template editor saves flat YAML", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [h100Spec] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateYAML = (async () => serverYAML("custom/model")) as never;
  let saved = "";
  api.importInferenceTemplates = (async (file) => { saved = await file.text(); return { models: 1 }; }) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  const editor = await screen.findByRole("textbox", { name: "Edit flat YAML" });
  assert.match((editor as HTMLTextAreaElement).value, /model_id: custom\/model/);
  const user = userEvent.setup();
  await user.clear(editor);
  await user.type(editor, "models: empty");
  assert.ok((editor as HTMLTextAreaElement).value.endsWith("models: empty"));
  await userEvent.setup().click(screen.getByRole("button", { name: /Save/ }));
  await waitFor(() => assert.ok(saved.endsWith("models: empty")));
});

test("editor keeps save failures visible with a jumpable line number", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [h100Spec] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.importInferenceTemplates = (async () => {
    throw new Error("parse inference catalog YAML: yaml: unmarshal errors:\n  line 6: field enginex not found in type inferencecatalog.InferenceSpec");
  }) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  const editor = (await screen.findByRole("textbox", { name: "Edit flat YAML" })) as HTMLTextAreaElement;
  const user = userEvent.setup();
  await user.type(editor, "x");
  await userEvent.setup().click(screen.getByRole("button", { name: /Save/ }));

  const panel = await screen.findByRole("alert");
  assert.match(panel.textContent || "", /Save failed/);
  assert.match(panel.textContent || "", /field enginex not found/);
  assert.doesNotMatch(panel.textContent || "", /inferencecatalog\.InferenceSpec/, "Go type noise stays out of the user-facing panel");
  const jump = screen.getByRole("button", { name: "Line 6" });
  await userEvent.setup().click(jump);
  const line = editor.value.split("\n")[5] || "";
  assert.ok(line.includes(editor.value.slice(editor.selectionStart, editor.selectionEnd)), "the jump selects the reported line");
});

test("editor confirms before discarding unsaved changes and reset restores the saved YAML", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [{ ...h100Spec, source: "user" }] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  const editor = (await screen.findByRole("textbox", { name: "Edit flat YAML" })) as HTMLTextAreaElement;
  const baseline = editor.value;
  const user = userEvent.setup();
  await user.type(editor, "# draft");
  assert.notEqual(editor.value, baseline);

  await user.click(screen.getByRole("button", { name: "Close" }));
  // antd Modal.confirm renders the title twice (modal title + confirm title)
  assert.ok((await screen.findAllByText("Discard unsaved changes?")).length >= 1, "closing with edits asks before discarding");
  // Take the OK path only: canceling a static confirm leaves jsdom without
  // motion events, and its never-finishing close keeps the process alive.
  await user.click(screen.getByRole("button", { name: "Discard changes" }));
  // The dismissed confirm keeps its (doubled) title nodes mounted under
  // jsdom, so compare against the lingering count instead of zero.
  const lingering = screen.queryAllByText("Discard unsaved changes?").length;

  await user.click(await screen.findByRole("button", { name: /Edit$/ }));
  const reopened = (await screen.findByRole("textbox", { name: "Edit flat YAML" })) as HTMLTextAreaElement;
  assert.notEqual(reopened.value, baseline, "reopen keeps the draft for a second look");
  await user.click(screen.getByRole("button", { name: /Reset/ }));
  await waitFor(() => assert.equal((screen.getByRole("textbox", { name: "Edit flat YAML" }) as HTMLTextAreaElement).value, baseline));

  await user.click(screen.getByRole("button", { name: "Close" }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(screen.queryAllByText("Discard unsaved changes?").length, lingering, "closing without edits must not ask to discard");
});

test("editor saves an unchanged model_id without any overwrite confirmation", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [{ ...h100Spec, source: "user" }] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  const imports: string[] = [];
  api.importInferenceTemplates = (async (file) => { imports.push(await file.text()); return { models: 1 }; }) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  const editor = (await screen.findByRole("textbox", { name: "Edit flat YAML" })) as HTMLTextAreaElement;
  assert.ok(editor.value.includes('model_id: custom/model'));
  // jsdom cannot reselect text in this portal textarea, so the retargeted-id
  // confirm is covered by the yamlModelID unit tests plus the dirty-close
  // Modal plumbing; here the unchanged identity must save straight through.
  await userEvent.setup().type(editor, "# ");
  await userEvent.setup().click(screen.getByRole("button", { name: /Save/ }));
  await waitFor(() => assert.equal(imports.length, 1));
  assert.match(imports[0], /custom\/model/);
  assert.equal(screen.queryAllByText("model_id changed").length, 0, "no confirm for the loaded identity");
});

test("user specs expose an in-place delete that removes the spec or the whole template", async () => {
  const userSpec = { ...h100Spec, id: 3, source: "user", name: "my-tuned" };
  let serving = { ...upstreamModel, specs: [h100Spec, userSpec] };
  api.inferenceTemplate = (async () => serving) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  const deleted: Array<[string, string]> = [];
  api.deleteInferenceTemplateSpec = (async (modelID, spec) => {
    deleted.push([modelID, spec]);
    serving = { ...upstreamModel, specs: [h100Spec] };
    return { deleted: true, model_deleted: false };
  }) as never;
  renderDetail();
  await screen.findAllByText("H100 · FP8 spec");
  await userEvent.setup().click(await screen.findByRole("button", { name: "Delete user spec my-tuned" }));
  await userEvent.setup().click(await screen.findByRole("button", { name: "Delete", exact: true }));
  await waitFor(() => assert.deepEqual(deleted, [["deepseek-ai/model", "my-tuned"]]));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: "Delete user spec my-tuned" }) === null, true), "the row disappears after the catalog reloads");

  // deleting the last user spec of a user-only template removes the template and returns to the list.
  // The first page must be gone before the second renders: left mounted, its
  // "H100 · FP8 spec" text satisfies the wait below while the second page
  // is still loading, and the delete button is looked up before it exists.
  cleanup();
  let modelOnly = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [userSpec] };
  api.inferenceTemplate = (async () => modelOnly) as never;
  api.deleteInferenceTemplateSpec = (async () => { modelOnly = { ...modelOnly, specs: [] }; return { deleted: true, model_deleted: true }; }) as never;
  api.inferenceTemplates = (async () => ({ items: [], page: 1, page_size: 200, total: 0 })) as never;
  renderDetail("/inference-templates/custom/model");
  await screen.findAllByText("H100 · FP8 spec");
  await userEvent.setup().click(await screen.findByRole("button", { name: "Delete user spec my-tuned" }));
  await userEvent.setup().click(await screen.findByRole("button", { name: "Delete", exact: true }));
  await waitFor(() => assert.ok(screen.getByText("Last user spec deleted; template custom/model removed")));
});

test("clone opens the editor without a remount via edit=1", async () => {
  const copy = { ...upstreamModel, source: "user", model_id: "deepseek-ai/model-copy", specs: [{ ...h100Spec, source: "user" }] };
  api.inferenceTemplate = (async (id) => (id === copy.model_id ? copy : upstreamModel)) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.cloneInferenceTemplate = (async () => copy) as never;
  renderCatalogFlow("/inference-templates/deepseek-ai/model");
  await screen.findAllByText("H100 · FP8 spec");
  await userEvent.setup().click(screen.getByRole("button", { name: /Clone/ }));
  const editor = await screen.findByRole("textbox", { name: "Edit flat YAML" });
  assert.match((editor as HTMLTextAreaElement).value, /model_id: deepseek-ai\/model-copy/);
});

// The warnings an import returns for a template that was accepted (callable
// spec without api.metrics): shown as one non-blocking alert, text keyed by
// code with the params interpolated, English fallback for a code the console
// does not know.
const metricsWarning = {
  code: "CATALOG_CALLABLE_METRICS_UNDECLARED",
  error: "Callable spec serve of acme/adapter declares no api.metrics.",
  params: { model_id: "acme/adapter", spec: "serve" },
};
const futureWarning = { code: "CATALOG_SOMETHING_NEW", error: "A check this console has no text for yet.", params: { spec: "serve" } };

test("template warnings render localized text per code with params, and fall back to the server's English", async () => {
  const { container } = render(<I18nextProvider i18n={i18n}><TemplateWarnings warnings={[metricsWarning, futureWarning]} /></I18nextProvider>);
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent || "", /Imported, with 2 things to check/);
  const items = within(alert).getAllByRole("listitem").map((item) => item.textContent);
  assert.match(items[0] || "", /Spec "serve" of acme\/adapter runs the callable engine but declares no api\.metrics/);
  assert.equal(items[1], "A check this console has no text for yet.", "an unknown code shows the server's English text");
  cleanup();
  const empty = render(<I18nextProvider i18n={i18n}><TemplateWarnings warnings={[]} /></I18nextProvider>);
  assert.equal(empty.container.innerHTML, "", "no warnings render nothing");
  assert.equal(container.innerHTML, "");
});

test("template warnings read in Chinese with the same params", async () => {
  const zh = createInstance();
  await zh.use(initReactI18next).init({
    lng: "zh", fallbackLng: "zh",
    resources: { zh: { inferenceTemplates: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/inferenceTemplates.json", import.meta.url), "utf8")) } },
    defaultNS: "inferenceTemplates", interpolation: { escapeValue: false },
  });
  render(<I18nextProvider i18n={zh}><TemplateWarnings warnings={[metricsWarning, { code: "CATALOG_HEALTH_ENDPOINT_UNDECLARED", error: "x", params: { model_id: "acme/adapter", spec: "serve" } }]} /></I18nextProvider>);
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent || "", /已导入，有 2 项需要确认/);
  const items = within(alert).getAllByRole("listitem").map((item) => item.textContent || "");
  assert.match(items[0], /acme\/adapter 的档位“serve”使用 callable 引擎但没有声明 api\.metrics/);
  assert.match(items[1], /没有声明 api\.health_endpoint/);
});

test("importing a template shows its warnings after the import settles and the alert can be dismissed by keyboard", async () => {
  api.inferenceTemplates = (async () => ({ items: [], page: 1, page_size: 200, total: 0 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  let finish!: (value: unknown) => void;
  api.importInferenceTemplates = (() => new Promise((resolve) => { finish = resolve; })) as never;
  const { container } = renderList();
  await screen.findByText("No models match the filters");
  const input = container.querySelector('input[type="file"]') as HTMLInputElement;
  await userEvent.setup().upload(input, new File(["models: []"], "catalog.yaml", { type: "application/yaml" }));
  await waitFor(() => assert.equal(typeof finish, "function"));
  assert.equal(screen.queryByText(/things? to check/) === null, true, "nothing to show while the import is in flight");
  finish({ models: 1, package_digest: "", warnings: [metricsWarning] });
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent || "", /Imported, with 1 thing to check/);
  assert.match(alert.textContent || "", /declares no api\.metrics/);
  const close = within(alert).getByRole("button", { name: /close/i });
  close.focus();
  await userEvent.setup().keyboard("{Enter}");
  await waitFor(() => assert.equal(screen.queryByText(/thing to check/) === null, true));
});

test("importing a clean template, or one the server rejects, shows no warnings alert", async () => {
  api.inferenceTemplates = (async () => ({ items: [], page: 1, page_size: 200, total: 0 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  api.importInferenceTemplates = (async () => ({ models: 1, package_digest: "" })) as never;
  const { container } = renderList();
  await screen.findByText("No models match the filters");
  const input = container.querySelector('input[type="file"]') as HTMLInputElement;
  await userEvent.setup().upload(input, new File(["models: []"], "catalog.yaml", { type: "application/yaml" }));
  assert.ok((await screen.findAllByText("Templates imported")).length > 0);
  assert.equal(screen.queryByText(/to check/) === null, true);
  api.importInferenceTemplates = (async () => { throw new Error("model acme/x spec s has invalid thing"); }) as never;
  await userEvent.setup().upload(input, new File(["models: []"], "catalog.yaml", { type: "application/yaml" }));
  assert.ok(await screen.findByText(/invalid thing/));
  assert.equal(screen.queryByText(/to check/) === null, true);
});

test("saving a template in the editor shows the warnings the import returned", async () => {
  const custom = { ...upstreamModel, source: "user", model_id: "custom/model", specs: [{ ...h100Spec, source: "user" }] };
  api.inferenceTemplate = (async () => custom) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.importInferenceTemplates = (async () => ({ models: 1, warnings: [metricsWarning] })) as never;
  renderDetail("/inference-templates/custom/model?edit=1");
  await screen.findByRole("textbox", { name: "Edit flat YAML" });
  await userEvent.setup().click(screen.getByRole("button", { name: /Save/ }));
  const alert = await screen.findByText(/Imported, with 1 thing to check/);
  assert.match(alert.closest("[role=alert]")?.textContent || "", /Spec "serve" of acme\/adapter/);
});


test("a hypothetical AMD chip retains its architecture selector", async () => {
  api.inferenceTemplates = (async () => ({ items: [], page: 1, page_size: 200, total: 0 })) as never;
  api.detectedInferenceTemplateChips = (async () => list([])) as never;
  api.inferenceTemplateChips = (async () => list([{ vendor: "amd", name: "AMD Radeon 8060S", category: "workstation-consumer", architecture: "RDNA 3.5", arch_id: "gfx1151" }])) as never;
  render(<I18nextProvider i18n={i18n}><MemoryRouter><InferenceTemplatesPage /></MemoryRouter></I18nextProvider>);
  fireEvent.click(await screen.findByRole("button", { name: /Add Chip/ }));
  const dialog = await screen.findByRole("dialog", { name: "Add a hypothetical GPU" });
  fireEvent.mouseDown(within(dialog).getByRole("combobox"));
  await waitFor(() => assert.equal(document.querySelectorAll(".ant-select-item-option").length > 0, true));
  const option = [...document.querySelectorAll(".ant-select-item-option")].find((el) => el.textContent?.includes("gfx1151"));
  assert.equal(option !== undefined, true);
  fireEvent.click(option!);
  fireEvent.click(within(dialog).getByRole("button", { name: "Add", exact: true }));
  const { loadHypotheticalChips } = await import("../src/pages/InferenceTemplatesPage.tsx");
  await waitFor(() => assert.equal(loadHypotheticalChips().some((chip) => chip.vendor === "amd" && chip.arch_id === "gfx1151"), true));
});


test("chip filtering sends architecture to the server and accepts Go-only predicate syntax", async () => {
  let requested: Record<string, string[]> | undefined;
  const model = { ...upstreamModel, display_title: "Go regex model", specs: [{ ...h100Spec, gpus: { vendor: "amd", name: "(?i)radeon", arch_id: "gfx1151" } }] };
  api.inferenceTemplates = (async (query) => {
    requested = query.extra;
    return { items: [model], page: 1, page_size: 200, total: 1 };
  }) as never;
  api.detectedInferenceTemplateChips = (async () => list([{ vendor: "amd", name: "Radeon 8060S", arch_id: "gfx1151" }])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  renderList();
  await screen.findByText("Go regex model");
  await userEvent.setup().click(screen.getByRole("button", { name: "Radeon 8060S · gfx1151" }));
  await waitFor(() => assert.deepEqual(requested, { gpu: ["Radeon 8060S"], vendor: ["amd"], arch_id: ["gfx1151"] }));
  assert.equal((await screen.findByText("Go regex model")).textContent, "Go regex model");
});

test("chip query failure does not show unfiltered models as compatible", async () => {
  api.inferenceTemplates = (async (query) => {
    if (query.extra?.gpu) throw new Error("hardware query failed");
    return { items: [upstreamModel], page: 1, page_size: 200, total: 1 };
  }) as never;
  api.detectedInferenceTemplateChips = (async () => list([{ vendor: "nvidia", name: "H100", cc: "9.0" }])) as never;
  api.inferenceTemplateChips = (async () => list([])) as never;
  renderList();
  await screen.findByText("DeepSeek Model");
  await userEvent.setup().click(screen.getByRole("button", { name: "H100" }));
  await screen.findByText(/hardware query failed/);
  assert.equal(screen.queryByText("DeepSeek Model") === null, true);
});

test("detail displays server support warnings and preferred engine without re-evaluating predicates", async () => {
  api.inferenceTemplate = (async () => ({ ...upstreamModel,
    detected_support: { preferred_engine: "sglang", warnings: [{ reason: "server unsupported rule", quantization: "fp8", alternative_available: true }] },
  })) as never;
  api.detectedInferenceTemplateChips = (async () => { throw new Error("detail must use server support projection"); }) as never;
  renderDetail();
  assert.equal((await screen.findByText("910B (A2) · INT8 W8A8 spec")).textContent, "910B (A2) · INT8 W8A8 spec");
  assert.ok(screen.getByText("server unsupported rule"));
  assert.ok(screen.getByText(i18n.t("flat.alternativeAvailable")));
});
