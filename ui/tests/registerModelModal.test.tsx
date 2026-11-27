import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { installFleet } from "./helpers/fakeFleet.ts";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const models = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/models.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { models } },
  defaultNS: "models",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { RegisterModal } = await import("../src/pages/models/RegisterModelModal.tsx");
const { ModelSourceTag, ModelTile, sourceLabel } = await import("../src/pages/ModelsPageParts.tsx");

type Preview = import("../src/api.ts").URLModelPreview;

const directoryPreview: Preview = {
  source_url: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B",
  source_id: "Qwen/Qwen3-0.6B",
  name: "Qwen3-0.6B",
  arch: "Qwen3ForCausalLM",
  precision: "bfloat16",
  context_length: 40960,
  weight_bytes: 1503300328,
  files_count: 8,
  input_modalities: ["text"],
};

const original = { createModel: api.createModel, metadataSources: api.metadataSources, previewURLModel: api.previewURLModel, registerURLModel: api.registerURLModel, imageVersions: api.imageVersions, nodesPage: api.nodesPage, nodes: api.nodes };
// The dialogs search nodes on the server (NodePicker); the fake answers as GET /nodes?page=… does,
// and refuses a request for the whole fleet.

beforeEach(() => {
  api.imageVersions = (async () => list([])) as typeof api.imageVersions;
});
afterEach(() => {
  cleanup();
  Object.assign(api, original);
});

function openDirectoryForm() {
  render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: models.column.source }));
  fireEvent.click(screen.getByText(models.source.url));
}

async function directoryURLInput() {
  return await screen.findByRole("textbox", { name: models.register.directoryURL });
}

beforeEach(() => { installFleet(api as never, [] as never); });

test("the register dialog offers the HTTP directory source with its own single field", async () => {
  openDirectoryForm();
  const input = await directoryURLInput();
  assert.ok(input, "the directory URL is the only identity field for this source");
  assert.equal(screen.queryByText(models.register.modelDirectory), null, "a node path is not asked for");
  assert.ok(screen.getByRole("button", { name: models.register.parseDirectory }));
  // Until the directory is parsed the primary action reads as a parse, not a
  // registration.
  assert.ok(screen.getByRole("button", { name: models.register.parsePreview }));
});

test("parsing a directory shows the model it found and lets the repository id be corrected", async () => {
  let asked = "";
  let registered: unknown;
  let release: (value: Preview) => void = () => {};
  api.previewURLModel = ((url: string) => {
    asked = url;
    return new Promise<Preview>((resolve) => { release = resolve; });
  }) as typeof api.previewURLModel;
  api.registerURLModel = (async (body: unknown) => { registered = body; return { id: 3 } as never; }) as typeof api.registerURLModel;

  openDirectoryForm();
  const input = await directoryURLInput();
  fireEvent.change(input, { target: { value: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B/" } });
  fireEvent.click(screen.getByRole("button", { name: models.register.parseDirectory }));

  // Loading: the parse button reports progress and no preview is claimed yet.
  await waitFor(() => assert.ok(document.querySelector(".ant-btn-loading"), "the parse is shown as in flight"));
  assert.equal(screen.queryByText(models.register.urlParsed), null);
  assert.equal(asked, "https://hf-mirror.example.com/Qwen/Qwen3-0.6B/");

  release(directoryPreview);
  await screen.findByText(models.register.urlParsed);
  assert.ok(screen.getByText("Qwen3ForCausalLM"), "the architecture is shown");
  assert.ok(screen.getByText("bfloat16"), "the precision is shown");
  assert.ok(screen.getByText("40,960"), "the context length is shown");
  assert.ok(screen.getByText("1.4 GiB"), "the directory size is shown");
  assert.ok(screen.getByText("8"), "the file count is shown");
  assert.ok(screen.getByText("Text"), "the input modalities are shown");
  // The size is a fact about the directory, not a caveat to explain away.
  assert.equal(screen.queryByText(/approx/i), null);
  // Catalog matching is not part of this card.
  assert.equal(screen.queryByText(/catalog/i), null);

  const repositoryID = screen.getByRole("textbox", { name: models.register.modelID }) as HTMLInputElement;
  assert.equal(repositoryID.value, "Qwen/Qwen3-0.6B", "the suggested repository id is prefilled");
  fireEvent.change(repositoryID, { target: { value: "Qwen/Qwen3-0.6B-mirror" } });

  // Enter in the repository id submits the registration, without reaching for
  // the mouse.
  fireEvent.keyDown(repositoryID, { key: "Enter", code: "Enter", keyCode: 13, charCode: 13 });
  fireEvent.keyPress(repositoryID, { key: "Enter", code: "Enter", keyCode: 13, charCode: 13 });
  await waitFor(() => assert.deepEqual(registered, {
    source_url: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B",
    source_id: "Qwen/Qwen3-0.6B-mirror",
    name: "Qwen3-0.6B",
  }));
});

test("a registration without metadata says why the management node read none", async () => {
  api.previewURLModel = (async () => directoryPreview) as typeof api.previewURLModel;
  const notice = { code: "MODEL_METADATA_UNREACHABLE", params: { hosts: "hf-mirror.example.com", source: "url" }, message: "the management node reached none of hf-mirror.example.com" };
  api.registerURLModel = (async () => ({ id: 4, metadata_notice: notice }) as never) as typeof api.registerURLModel;

  openDirectoryForm();
  const input = await directoryURLInput();
  fireEvent.change(input, { target: { value: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B" } });
  fireEvent.click(screen.getByRole("button", { name: models.register.parseDirectory }));
  await screen.findByText(models.register.urlParsed);
  fireEvent.click(screen.getByRole("button", { name: models.register.confirm }));
  await screen.findByText(models.register.registered);
  // The common namespace is not loaded here, so the server's own text stands in.
  await screen.findByText(notice.message);
});

test("a directory that is not a model is reported and nothing is registered", async () => {
  let registered = 0;
  api.previewURLModel = (async () => { throw new Error("The directory contains no config.json, so it is not a model repository."); }) as typeof api.previewURLModel;
  api.registerURLModel = (async () => { registered += 1; return { id: 1 } as never; }) as typeof api.registerURLModel;

  openDirectoryForm();
  const input = await directoryURLInput();
  fireEvent.change(input, { target: { value: "https://hf-mirror.example.com/weights" } });
  fireEvent.click(screen.getByRole("button", { name: models.register.parseDirectory }));

  await screen.findByText(/no config\.json/);
  assert.equal(screen.queryByText(models.register.urlParsed), null, "no preview card is shown for a failed parse");
  assert.equal(registered, 0, "a failed parse never registers a model");
});

test("an unreachable directory is reported and the form stays on the parse step", async () => {
  api.previewURLModel = (async () => { throw new Error("The HTTP directory could not be read."); }) as typeof api.previewURLModel;

  openDirectoryForm();
  const input = await directoryURLInput();
  fireEvent.change(input, { target: { value: "https://nowhere.invalid/models" } });
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13, charCode: 13 });
  fireEvent.keyPress(input, { key: "Enter", code: "Enter", keyCode: 13, charCode: 13 });

  await screen.findByText(/could not be read/);
  assert.ok(screen.getByRole("button", { name: models.register.parsePreview }), "the primary action is still the parse");
  assert.equal(screen.queryByRole("textbox", { name: models.register.modelID }), null);
});

test("an empty directory URL is refused before any request is made", async () => {
  let asked = 0;
  api.previewURLModel = (async () => { asked += 1; return directoryPreview; }) as typeof api.previewURLModel;
  openDirectoryForm();
  await directoryURLInput();
  fireEvent.click(screen.getByRole("button", { name: models.register.parseDirectory }));
  await screen.findByText(models.register.directoryURLRequired);
  assert.equal(asked, 0);
});

test("a directory model is tagged and labeled as one", () => {
  render(<I18nextProvider i18n={i18n}><ModelSourceTag source="url" /></I18nextProvider>);
  assert.ok(screen.getByText("URL"), "the short tag names the source");
  assert.equal(sourceLabel.url, "source.url");
  assert.equal(models.source.url, "HTTP directory");
});

test("a directory model keeps the actions a node-local model cannot offer", () => {
  const base = { id: 1, source_id: "Qwen/Qwen3-0.6B", revision: "", name: "Qwen3-0.6B", arch: "", quant_label: "", weight_gib: 1.4, created_at: "2026-09-01T00:00:00Z" };
  const { rerender } = render(<I18nextProvider i18n={i18n}><ModelTile
    model={{ ...base, source: "url", source_url: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B" } as never}
    onOpen={() => {}} onDownload={() => {}} onRegisterPath={() => {}} onDelete={() => {}}
  /></I18nextProvider>);
  // The weights live on a server the platform can reach, so a node can be told
  // to fetch them — exactly as for a repository model.
  assert.ok(screen.getByRole("button", { name: models.action.downloadToNode }));

  rerender(<I18nextProvider i18n={i18n}><ModelTile
    model={{ ...base, source: "local" } as never}
    onOpen={() => {}} onDownload={() => {}} onRegisterPath={() => {}} onDelete={() => {}}
  /></I18nextProvider>);
  assert.equal(screen.queryByRole("button", { name: models.action.downloadToNode }), null, "a node-local model has nowhere to download from");
});

async function registerHuggingFace(bases: string[], fetcher: typeof fetch) {
  const sent: Array<Record<string, unknown>> = [];
  api.metadataSources = (async () => ({ source: "huggingface", bases })) as typeof api.metadataSources;
  api.createModel = (async (body: Record<string, unknown>) => { sent.push(body); return { id: 5 } as never; }) as typeof api.createModel;
  const realFetch = globalThis.fetch;
  globalThis.fetch = fetcher;
  try {
    render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
    fireEvent.mouseDown(screen.getByRole("combobox", { name: models.column.source }));
    fireEvent.click(screen.getByText("Hugging Face", { selector: ".ant-select-item-option-content" }));
    fireEvent.change(await screen.findByRole("textbox", { name: models.column.sourceID }), { target: { value: "Qwen/Qwen3-0.6B" } });
    fireEvent.click(screen.getByRole("button", { name: models.register.confirm }));
    await waitFor(() => assert.equal(sent.length, 1));
    await waitFor(() => assert.ok(screen.getAllByText(models.register.registered).length > 0));
  } finally {
    globalThis.fetch = realFetch;
  }
  return sent;
}

test("a Hugging Face registration the browser could not read is sent without the browser marker", async () => {
  const asked: string[] = [];
  const sent = await registerHuggingFace([], (async (input: string | URL | Request) => { asked.push(String(input)); return new Response("x"); }) as typeof fetch);
  assert.equal(sent.length, 1, "registration still succeeds when nobody can read the metadata");
  assert.equal("browser_metadata" in sent[0], false);
  assert.equal(sent[0].readme, undefined);
  assert.deepEqual(asked, [], "no address was given, so the browser fetched nothing");
});

test("a Hugging Face registration the browser read carries the marker, the content and the commit it saw", async () => {
  const sha = "0123456789abcdef0123456789abcdef01234567";
  const sent = await registerHuggingFace(["https://hub.corp.example"], (async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/config.json")) return new Response(`{"model_type":"qwen3"}`);
    if (url.includes("/api/models/")) return new Response(JSON.stringify({ sha, siblings: [{ rfilename: "m.safetensors", size: 7 }] }));
    return new Response("nope", { status: 404 });
  }) as typeof fetch);
  assert.equal(sent[0].browser_metadata, true);
  assert.equal(sent[0].config_raw, `{"model_type":"qwen3"}`);
  assert.equal(sent[0].weight_bytes, 7);
  assert.equal(sent[0].revision, sha);
});

test("the revision detail marks an unpinned Hugging Face model that has a path-registered copy", async () => {
  const { hasUnpinnedLocalCopy } = await import("../src/pages/models/unpinnedRevision.ts");
  const copy = (acquisition: string) => ({ node_uuid: "n1", acquisition }) as never;
  const sha = "0123456789abcdef0123456789abcdef01234567";
  assert.equal(hasUnpinnedLocalCopy({ source: "huggingface", revision: "", nodes: [copy("registered")] }), true);
  assert.equal(hasUnpinnedLocalCopy({ source: "ModelScope", revision: "master", nodes: [copy("registered")] }), true);
  assert.equal(hasUnpinnedLocalCopy({ source: "huggingface", revision: sha, nodes: [copy("registered")] }), false, "a commit is pinned");
  assert.equal(hasUnpinnedLocalCopy({ source: "huggingface", revision: "main", nodes: [copy("download")] }), false, "a downloaded copy is the site's own");
  assert.equal(hasUnpinnedLocalCopy({ source: "local", revision: "", nodes: [copy("registered")] }), false);
  assert.equal(hasUnpinnedLocalCopy({ source: "huggingface", revision: "" }), false);
});

test("a catalog pick is written only after confirmation and keeps its revision and custom name on retry", async () => {
  const sent: Array<Record<string, unknown>> = [];
  api.createModel = (async (body: Record<string, unknown>) => {
    sent.push(body);
    if (sent.length === 1) throw new Error("Temporary error");
    return {id: 1};
  }) as typeof api.createModel;
  let done = false;
  render(<I18nextProvider i18n={i18n}><RegisterModal open manualSeed={{source:"modelscope", id:"org/demo", offline:false, revision:"abc123"}} onClose={() => {}} onDone={() => {done=true;}} /></I18nextProvider>);
  const name = await screen.findByRole("textbox", {name:models.column.name});
  fireEvent.change(name, {target:{value:"My model"}});
  assert.equal(sent.length, 0);
  fireEvent.click(screen.getByRole("button", {name:models.register.confirm}));
  await screen.findByText(/Temporary error/);
  assert.equal((name as HTMLInputElement).value,"My model");
  fireEvent.click(screen.getByRole("button", {name:models.register.confirm}));
  await waitFor(() => assert.equal(done,true));
  assert.deepEqual(sent.map(m => [m.source,m.source_id,m.revision,m.name]), Array(2).fill(["modelscope","org/demo","abc123","My model"]));
});

// The server refuses a repository its site says it does not have. The dialog shows the refusal
// where the form is, sends nothing more by itself, and registers with force only when asked to.
test("a registration the source refuses stays in the dialog and is forced only on request", async () => {
  const { APIError } = await import("../src/apiError");
  const sent: Array<Record<string, unknown>> = [];
  const refusal = "modelscope.cn answered 404 for zai-org/glm-5";
  api.createModel = (async (body: Record<string, unknown>) => {
    sent.push(body);
    if (!body.force) throw new APIError("MODEL_REPO_NOT_FOUND", { source: "modelscope", repo: "zai-org/glm-5", host: "modelscope.cn", status: 404 }, refusal, 422);
    return { id: 6 } as never;
  }) as typeof api.createModel;
  let done = 0;
  render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => { done++; }} /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: models.column.source }));
  fireEvent.click(screen.getByText("ModelScope", { selector: ".ant-select-item-option-content" }));
  fireEvent.change(await screen.findByRole("textbox", { name: models.column.sourceID }), { target: { value: "zai-org/glm-5" } });
  fireEvent.click(screen.getByRole("button", { name: models.register.confirm }));

  const force = await screen.findByRole("button", { name: models.register.force });
  assert.ok(screen.getByText(refusal));
  assert.equal(sent.length, 1);
  assert.equal(done, 0, "a refused registration is not reported as done");

  // Editing the repository withdraws the refusal of the previous one.
  fireEvent.change(screen.getByRole("textbox", { name: models.column.sourceID }), { target: { value: "zai-org/glm-5x" } });
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: models.register.force }), null));
  assert.equal(force.isConnected, false);

  fireEvent.click(screen.getByRole("button", { name: models.register.confirm }));
  fireEvent.click(await screen.findByRole("button", { name: models.register.force }));
  await waitFor(() => assert.equal(done, 1));
  assert.deepEqual(sent.map((body) => [body.source_id, body.force]), [["zai-org/glm-5", undefined], ["zai-org/glm-5x", undefined], ["zai-org/glm-5x", true]]);
});
