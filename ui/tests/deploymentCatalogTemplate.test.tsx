import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// Saving a running deployment as an inference template. The answers are the
// server's own (ui/tests/fixtures/catalog-template, generated from the real
// router, exporter and import by TestCatalogTemplateUIFixtures).

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "KeyboardEvent", "Document", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
// A browser download is an anchor click on an object URL.
const downloads: { name: string; text: Promise<string> }[] = [];
Object.defineProperty(globalThis.URL, "createObjectURL", { configurable: true, value: (blob: Blob) => { downloads.push({ name: "", text: blob.text() }); return "blob:template"; } });
Object.defineProperty(globalThis.URL, "revokeObjectURL", { configurable: true, value: () => {} });
dom.window.HTMLAnchorElement.prototype.click = function click(this: HTMLAnchorElement) { downloads[downloads.length - 1].name = this.download; };

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const deployments = read("deployments");
const common = read("common");
const text = deployments.catalogTemplate;
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { deployments, common } }, defaultNS: "deployments", interpolation: { escapeValue: false } });

const fixture = (name: string) => JSON.parse(fs.readFileSync(new URL(`./fixtures/catalog-template/${name}.json`, import.meta.url), "utf8"));
const exported = fixture("exported");
const exportedLocal = fixture("exported-local");
const exportedWithEnv = fixture("exported-with-env");
const checkFailed = fixture("check-failed");
const notRunning = fixture("not-running");
const imported = fixture("imported");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: DeploymentCatalogTemplateModal } = await import("../src/pages/DeploymentCatalogTemplateModal.tsx");

type Call = { method: string; url: string; body?: string };
let calls: Call[] = [];
let role = "admin";
let exportAnswers: Response[] = [];
const originalFetch = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = String(input);
  const method = init?.method ?? "GET";
  const body = typeof init?.body === "string" ? init.body : init?.body instanceof dom.window.Blob || init?.body instanceof Blob ? await (init.body as Blob).text() : undefined;
  calls.push({ method, url, body });
  if (url === "/api/v1/auth/me") return Response.json({ id: 1, username: role, role });
  if (method === "POST" && url === "/api/v1/deployments/1/catalog-template") return exportAnswers.shift() ?? Response.json(exported);
  if (method === "POST" && url === "/api/v1/inference-templates/import") return Response.json(imported);
  throw new Error(`Unexpected request: ${method} ${url}`);
};
afterEach(() => { cleanup(); calls = []; role = "admin"; exportAnswers = []; downloads.length = 0; });
after(() => { globalThis.fetch = originalFetch; closeChannels(); dom.window.close(); });

let closed = 0;
async function open() {
  render(<I18nextProvider i18n={i18n}><MemoryRouter><DeploymentCatalogTemplateModal deploymentID={1} onClose={() => { closed++; }} /></MemoryRouter></I18nextProvider>);
  return screen.findByRole("dialog");
}

const exports = () => calls.filter((call) => call.url.endsWith("/catalog-template"));

test("saving a local deployment explains that the template references registered weights without bundling files", async () => {
  exportAnswers = [Response.json(exportedLocal)];
  const dialog = await open();
  const yaml = await within(dialog).findByRole("textbox", { name: text.yaml }) as HTMLTextAreaElement;
  assert.equal(yaml.value, exportedLocal.yaml);
  assert.ok(within(dialog).queryByText("This template references the registered local weights company-tuned-model; it does not include the model files. Register these weights before using the template on another cluster.") !== null);
  assert.equal((within(dialog).getByRole("button", { name: new RegExp(text.save) }) as HTMLButtonElement).disabled, false);
});

test("the dialog says it is testing, then shows the checks, the notes and the YAML", async () => {
  const dialog = await open();
  await within(dialog).findByText(text.verified.replace("{{model}}", exported.model_id).replace("{{spec}}", exported.spec_name));
  assert.ok(within(dialog).getByText("openai.chat"));
  assert.ok(within(dialog).getByText(text.check.passed.replace("{{status}}", "200")));
  assert.ok(within(dialog).getByText(text.note.verified_on.replace("{{count}}", "1").replace("{{gpu}}", "NVIDIA H20")));
  assert.ok(within(dialog).getByText(text.note.env_excluded.replace("{{keys}}", "HF_TOKEN, VLLM_LOGGING_LEVEL")));
  const yaml = within(dialog).getByRole("textbox", { name: text.yaml }) as HTMLTextAreaElement;
  assert.equal(yaml.value, exported.yaml);
  assert.ok(!yaml.value.includes("hf_secret"), "an env value reached the YAML");
  // The operation check is a row of its own, not repeated as a note.
  const notes = within(dialog).getByRole("region", { name: text.notes });
  assert.equal(within(notes).queryByText(/openai\.chat/), null);
  assert.deepEqual(exports().map((call) => call.body), ["{}"]);
});

test("the loading state is announced while the server tests the API", async () => {
  let resolve: (response: Response) => void = () => {};
  exportAnswers = [];
  const pending = new Promise<Response>((done) => { resolve = done; });
  const original = globalThis.fetch;
  globalThis.fetch = async (input, init) => (String(input).endsWith("/catalog-template") ? pending : original(input, init));
  try {
    const dialog = await open();
    assert.ok(within(dialog).getByRole("status"));
    assert.ok(within(dialog).getByText(text.testing));
    resolve(Response.json(exported));
    await within(dialog).findByRole("textbox", { name: text.yaml });
  } finally {
    globalThis.fetch = original;
  }
});

test("keeping an env var the server left out rebuilds the export with it", async () => {
  exportAnswers = [Response.json(exported), Response.json(exportedWithEnv)];
  const dialog = await open();
  const keep = await within(dialog).findByRole("checkbox", { name: "VLLM_LOGGING_LEVEL" });
  fireEvent.click(keep);
  fireEvent.click(within(dialog).getByRole("button", { name: new RegExp(text.rebuild) }));
  await waitFor(() => assert.equal(exports().length, 2));
  assert.deepEqual(JSON.parse(exports()[1].body ?? "{}"), { include_env: ["VLLM_LOGGING_LEVEL"] });
  await waitFor(() => assert.equal((within(dialog).getByRole("textbox", { name: text.yaml }) as HTMLTextAreaElement).value, exportedWithEnv.yaml));
  // The kept key stays listed and ticked, so it can be dropped again.
  assert.equal((within(dialog).getByRole("checkbox", { name: "VLLM_LOGGING_LEVEL" }) as HTMLInputElement).checked, true);
  assert.equal((within(dialog).getByRole("checkbox", { name: "HF_TOKEN" }) as HTMLInputElement).checked, false);
});

test("an administrator saves the edited YAML into the catalog", async () => {
  const dialog = await open();
  const yaml = await within(dialog).findByRole("textbox", { name: text.yaml });
  const edited = exported.yaml.replace("scenario: balanced", "scenario: latency");
  fireEvent.change(yaml, { target: { value: edited } });
  const save = await within(dialog).findByRole("button", { name: new RegExp(text.save) });
  fireEvent.click(save);
  await within(dialog).findByRole("link", { name: text.openTemplate });
  const upload = calls.find((call) => call.url === "/api/v1/inference-templates/import");
  assert.equal(upload?.body, edited);
  assert.equal(within(dialog).getByRole("link", { name: text.openTemplate }).getAttribute("href"), `/inference-templates/${exported.model_id}`);
  assert.equal((within(dialog).getByRole("button", { name: new RegExp(text.save) }) as HTMLButtonElement).disabled, true);
});

test("a user who is not an administrator can download but not save", async () => {
  role = "user";
  const dialog = await open();
  await within(dialog).findByText(text.saveNeedsAdmin);
  assert.equal(within(dialog).queryByRole("button", { name: new RegExp(text.save) }), null);
  fireEvent.click(within(dialog).getByRole("button", { name: new RegExp(text.download) }));
  await waitFor(() => assert.equal(downloads.length, 1));
  assert.equal(downloads[0].name, `allenai_olmo-2-0425-1b-instruct.${exported.spec_name}.yaml`);
  assert.equal(await downloads[0].text, exported.yaml);
});

test("a failed check is shown and the template cannot be saved", async () => {
  exportAnswers = [Response.json(checkFailed)];
  const dialog = await open();
  await within(dialog).findByText(text.notVerified);
  assert.ok(within(dialog).getByText(text.check.failed.replace("{{status}}", "500").replace("{{detail}}", "engine error")));
  assert.equal((await within(dialog).findByRole("button", { name: new RegExp(text.save) }) as HTMLButtonElement).disabled, true);
});

test("a refusal says why in the reader's language and can be retried", async () => {
  exportAnswers = [Response.json(notRunning, { status: 409 }), Response.json(exported)];
  const dialog = await open();
  await within(dialog).findByText(text.reason.not_running);
  assert.ok(within(dialog).getByText(text.failed));
  assert.equal(within(dialog).queryByRole("textbox", { name: text.yaml }), null);
  fireEvent.click(within(dialog).getByRole("button", { name: text.retry }));
  await within(dialog).findByRole("textbox", { name: text.yaml });
  assert.equal(exports().length, 2);
  fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
  assert.equal(closed > 0, true);
});

test("the test record is copied, or offered for manual copy when the browser refuses", async () => {
  const dialog = await open();
  fireEvent.click(await within(dialog).findByRole("button", { name: new RegExp(text.copyEvidence) }));
  const manual = await screen.findByText(/GPUWorks template test record/);
  assert.ok(manual);
});

test("changing language preserves edits without probing the deployment again", async () => {
  const dialog = await open();
  const yaml = await within(dialog).findByRole("textbox", { name: text.yaml });
  const edited = exported.yaml + "\n# user edit\n";
  fireEvent.change(yaml, { target: { value: edited } });
  try {
    await i18n.changeLanguage("zh");
    await waitFor(() => assert.equal((within(dialog).getByRole("textbox", { name: text.yaml }) as HTMLTextAreaElement).value, edited));
    assert.equal(exports().length, 1);
  } finally {
    await i18n.changeLanguage("en");
  }
});

test("a rebuild hides stale actions and a late old deployment response cannot replace the new one", async () => {
  let resolveOld: (value: Response) => void = () => {};
  const old = new Promise<Response>((resolve) => { resolveOld = resolve; });
  const original = globalThis.fetch;
  let oldCalls = 0;
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/deployments/1/catalog-template") && ++oldCalls > 1) return old;
    if (String(input).endsWith("/deployments/2/catalog-template")) return Response.json({ ...exported, model_id: "new/model", yaml: "models: []\n# new deployment\n" });
    return original(input, init);
  };
  const element = (id: number) => <I18nextProvider i18n={i18n}><MemoryRouter><DeploymentCatalogTemplateModal deploymentID={id} onClose={() => {}} /></MemoryRouter></I18nextProvider>;
  try {
    const rendered = render(element(1));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(await within(dialog).findByRole("button", { name: new RegExp(text.rebuild) }));
    await within(dialog).findByRole("status");
    assert.equal(within(dialog).queryByRole("button", { name: new RegExp(text.save) }), null);
    assert.equal(within(dialog).queryByRole("button", { name: new RegExp(text.download) }), null);
    rendered.rerender(element(2));
    const yaml = await within(dialog).findByRole("textbox", { name: text.yaml });
    assert.equal((yaml as HTMLTextAreaElement).value, "models: []\n# new deployment\n");
    resolveOld(Response.json(exported));
    await old;
    await waitFor(() => assert.equal((yaml as HTMLTextAreaElement).value, "models: []\n# new deployment\n"));
  } finally {
    resolveOld(Response.json(exported));
    globalThis.fetch = original;
  }
});

test("only the YAML actually saved is marked saved, and edited model identities link to the catalog", async () => {
  const original = globalThis.fetch;
  let resolveImport: (value: Response) => void = () => {};
  const pending = new Promise<Response>((resolve) => { resolveImport = resolve; });
  globalThis.fetch = async (input, init) => String(input).endsWith("/inference-templates/import") ? pending : original(input, init);
  try {
    const dialog = await open();
    const editor = await within(dialog).findByRole("textbox", { name: text.yaml });
    const renamed = exported.yaml.replace(exported.model_id, "new/model");
    fireEvent.change(editor, { target: { value: renamed } });
    fireEvent.click(within(dialog).getByRole("button", { name: new RegExp(text.save) }));
    const editedDuringSave = renamed + "\n# not yet saved\n";
    fireEvent.change(editor, { target: { value: editedDuringSave } });
    resolveImport(Response.json(imported));
    await waitFor(() => assert.equal((within(dialog).getByRole("button", { name: new RegExp(text.save) }) as HTMLButtonElement).disabled, false));
    assert.equal(within(dialog).queryByRole("link", { name: text.openTemplate }), null);
    fireEvent.change(editor, { target: { value: renamed } });
    const link = await within(dialog).findByRole("link", { name: text.openTemplate });
    assert.equal(link.getAttribute("href"), "/inference-templates");
    assert.equal((within(dialog).getByRole("button", { name: new RegExp(text.save) }) as HTMLButtonElement).disabled, true);
    fireEvent.change(editor, { target: { value: editedDuringSave } });
    assert.equal((within(dialog).getByRole("button", { name: new RegExp(text.save) }) as HTMLButtonElement).disabled, false);
  } finally {
    resolveImport(Response.json(imported));
    globalThis.fetch = original;
  }
});

test("changing deployment while an import is pending clears the old saving state", async () => {
  const original = globalThis.fetch;
  let resolveImport: (value: Response) => void = () => {};
  const pending = new Promise<Response>((resolve) => { resolveImport = resolve; });
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/inference-templates/import")) return pending;
    if (String(input).endsWith("/deployments/2/catalog-template")) return Response.json({ ...exported, model_id: "new/model" });
    return original(input, init);
  };
  const element = (id: number) => <I18nextProvider i18n={i18n}><MemoryRouter><DeploymentCatalogTemplateModal deploymentID={id} onClose={() => {}} /></MemoryRouter></I18nextProvider>;
  try {
    const rendered = render(element(1));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(await within(dialog).findByRole("button", { name: new RegExp(text.save) }));
    rendered.rerender(element(2));
    await within(dialog).findByRole("textbox", { name: text.yaml });
    await waitFor(() => assert.equal(within(dialog).getByRole("button", { name: new RegExp(text.save) }).querySelector(".ant-btn-loading-icon"), null));
    resolveImport(Response.json(imported));
    await pending;
    assert.equal(within(dialog).queryByRole("link", { name: text.openTemplate }), null);
  } finally {
    resolveImport(Response.json(imported));
    globalThis.fetch = original;
  }
});
