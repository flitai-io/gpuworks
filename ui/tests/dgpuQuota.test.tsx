import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// Changing a deployment's dGPU quota from the GPU page's dGPU tab. The
// answers are the server's own (ui/tests/fixtures/dgpu-quota and
// gpu-inventory, generated from the real handlers).

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "KeyboardEvent", "Document", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const gpus = read("gpus");
const common = read("common");
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { gpus, common } }, defaultNS: "gpus", interpolation: { escapeValue: false } });

const fixture = (path: string) => JSON.parse(fs.readFileSync(new URL(`./fixtures/${path}.json`, import.meta.url), "utf8"));
const gpusPage = fixture("gpu-inventory/gpus-page");
const dgpus = fixture("gpu-inventory/dgpus");
const deployment = fixture("dgpu-quota/deployment");
const noRoom = fixture("dgpu-quota/no-room");
const changed = fixture("dgpu-quota/changed");
const pending = fixture("dgpu-quota/pending");

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: GPUPage } = await import("../src/pages/GPUPage.tsx");

type Call = { method: string; url: string; body?: unknown };
let calls: Call[] = [];
let putAnswer: () => Response = () => Response.json(changed);
const originalFetch = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = String(input);
  const method = init?.method ?? "GET";
  calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
  if (url.startsWith("/api/v1/gpus?")) return Response.json(gpusPage);
  if (url.startsWith("/api/v1/dgpus")) return Response.json(dgpus);
  if (url.startsWith("/api/v1/metrics/query")) return Response.json({ items: [], total: 0 });
  if (method === "PUT" && /\/api\/v1\/deployments\/\d+\/dgpu$/.test(url)) return putAnswer();
  if (/\/api\/v1\/deployments\/\d+$/.test(url)) return Response.json(deployment);
  throw new Error(`Unexpected request: ${method} ${url}`);
};
afterEach(() => { cleanup(); localStorage.clear(); calls = []; putAnswer = () => Response.json(changed); });
after(() => { globalThis.fetch = originalFetch; closeChannels(); dom.window.close(); });

async function openQuota(name: string) {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/gpus"]}><GPUPage /></MemoryRouter></I18nextProvider>);
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  const row = (await screen.findByText(name)).closest("tr") as HTMLElement;
  fireEvent.click(within(row).getByRole("button", { name: gpus.action.editQuota }));
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("8.0 GiB");
  return { dialog, deploymentID: dgpus.items.find((item: { deployment_name: string }) => item.deployment_name === name).deployment_id };
}

function limitInput(dialog: HTMLElement) {
  return within(dialog).getByRole("spinbutton") as HTMLInputElement;
}

test("the dialog opens on the deployment's limit and keeps the restart rules behind the help mark", async () => {
  const { dialog } = await openQuota("qwen3-dgpu-a");
  assert.ok(within(dialog).getByText(deployment.name));
  assert.equal(limitInput(dialog).value, "8.0");
  assert.ok(within(dialog).getByText(gpus.quota.hint));
  assert.equal(within(dialog).queryByText(gpus.quota.noteHelp), null, "the restart rules are not on the page");
  await userEvent.setup().hover(within(dialog).getByRole("button", { name: common.helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, gpus.quota.noteHelp);
  // Nothing to save until the limit changes.
  assert.equal(within(dialog).getByRole("button", { name: gpus.quota.save }).hasAttribute("disabled"), true);
});

test("saving sends the new limit, says it took effect, and points to a restart for the engine", async () => {
  const { dialog, deploymentID } = await openQuota("qwen3-dgpu-a");
  fireEvent.change(limitInput(dialog), { target: { value: "12" } });
  const listed = calls.filter((call) => call.url.startsWith("/api/v1/dgpus")).length;
  fireEvent.click(within(dialog).getByRole("button", { name: gpus.quota.save }));
  await waitFor(() => assert.ok(calls.some((call) => call.method === "PUT")));
  const put = calls.find((call) => call.method === "PUT")!;
  assert.equal(put.url, `/api/v1/deployments/${deploymentID}/dgpu`);
  assert.deepEqual(put.body, { vram_limit_gib: 12 });
  const result = await screen.findByText(gpus.quota.applied_one.replace("{{limit}}", "12.0 GiB").replace("{{count}}", "1"));
  const resultDialog = result.closest("[role=dialog]") as HTMLElement;
  assert.ok(within(resultDialog).getByText(gpus.quota.restartHint));
  assert.equal(within(resultDialog).getByRole("link", { name: gpus.quota.openDeployment }).getAttribute("href"), `/deployments?deployment=${deploymentID}`);
  await waitFor(() => assert.ok(calls.filter((call) => call.url.startsWith("/api/v1/dgpus")).length > listed, "the slice list was not refreshed"));
  fireEvent.click(within(resultDialog).getByRole("button", { name: gpus.quota.done }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
});

test("a slice that keeps its limit until a restart is named with why", async () => {
  putAnswer = () => Response.json(pending);
  const { dialog } = await openQuota("qwen3-dgpu-a");
  fireEvent.change(limitInput(dialog), { target: { value: "6" } });
  fireEvent.click(within(dialog).getByRole("button", { name: gpus.quota.save }));
  assert.ok(await screen.findByText(gpus.quota.pendingTitle_one.replace("{{count}}", "1")));
  assert.ok(screen.getByText(gpus.quota.appliedDeployment.replace("{{limit}}", "6.0 GiB")));
  const item = pending.pending[0];
  assert.ok(screen.getByText(item.instance_uuid.slice(0, 8)));
  assert.ok(screen.getByText(gpus.quota.pending.in_use.replace("{{used}}", "10.0 GiB").replace("{{limit}}", "6.0 GiB"), { exact: false }));
});

test("a slice running under an older limit says what it gets at restart", async () => {
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/gpus"]}><GPUPage /></MemoryRouter></I18nextProvider>);
  fireEvent.click(await screen.findByRole("tab", { name: gpus.tab.dgpu }));
  const row = (await screen.findByText("qwen3-dgpu-b")).closest("tr") as HTMLElement;
  assert.ok(within(row).getByText(gpus.quota.pendingTag.replace("{{limit}}", "6.0 GiB")));
  const other = screen.getByText("qwen3-dgpu-a").closest("tr") as HTMLElement;
  assert.equal(within(other).queryByText(/after restart/), null);
});

test("a refusal is shown in the dialog in the reader's words and the dialog stays", async () => {
  putAnswer = () => Response.json(noRoom, { status: 409 });
  const { dialog } = await openQuota("qwen3-dgpu-a");
  fireEvent.change(limitInput(dialog), { target: { value: "24" } });
  fireEvent.click(within(dialog).getByRole("button", { name: gpus.quota.save }));
  const text = common.apiError.DGPU_QUOTA_NO_ROOM
    .replace("{{gpu}}", "0").replace("{{node}}", "quota-node").replace("{{free_gib}}", "23.5").replace("{{need_gib}}", "24.5");
  assert.ok(await within(dialog).findByText(text));
  assert.ok(screen.getByRole("dialog"));
});

test("an empty limit is not sent", async () => {
  const { dialog } = await openQuota("qwen3-dgpu-a");
  fireEvent.change(limitInput(dialog), { target: { value: "" } });
  fireEvent.click(within(dialog).getByRole("button", { name: gpus.quota.save }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(calls.filter((call) => call.method === "PUT").length, 0);
});

test("Escape closes the dialog without a change", async () => {
  const { dialog } = await openQuota("qwen3-dgpu-a");
  fireEvent.keyDown(dialog, { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(calls.filter((call) => call.method === "PUT").length, 0);
});
