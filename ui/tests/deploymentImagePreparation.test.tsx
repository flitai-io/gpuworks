import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import type { Deployment } from "../src/api.ts";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/", pretendToBeVisual: true });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: read("deployments"), deploy: read("deploy"), common: read("common") } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { DeploymentPreparation } = await import("../src/pages/DeploymentPreparation.tsx");
const { MemoryRouter } = await import("react-router-dom");
function DeploymentImagePreparation({ dep }: { dep: Deployment }) {
  return <MemoryRouter><DeploymentPreparation dep={dep} rows={dep.model_progress} /></MemoryRouter>;
}

const originalImageVersions = api.imageVersions;
const originalImage = api.image;
afterEach(() => { cleanup(); api.imageVersions = originalImageVersions; api.image = originalImage; });
after(() => { closeChannels(); dom.window.close(); });

function deployment(overrides: Record<string, unknown> = {}) {
  return { id: 7, ...overrides } as Deployment;
}
function show(dep = imageDeployment()) {
  return render(<I18nextProvider i18n={i18n}><DeploymentImagePreparation dep={dep} /></I18nextProvider>);
}
function imageDeployment(node = "gpu-target", id = 7) {
  return deployment({ id, placement_status: { unscheduled: 1, reason: {
    code: "image_delivery_waiting", params: { image: "registry/runner:v1", node },
  } } });
}

function imageFixture(rows: Array<Record<string, unknown>>) {
  api.imageVersions = async () => ({ items: [{ id: 23, series_id: 4, image_ref: "registry/runner:v1", size_bytes: 999999 }] }) as never;
  api.image = async () => ({ id: 4, versions: [{ id: 23, nodes: rows }] }) as never;
}

const imageNode = { node_uuid: "target-uuid", node_name: "gpu-target", state: "syncing", size_bytes: 1024, transferred_bytes: 256, rate_bps: 128 };

test("an image wait shows the awaited node's measured bytes before any instance exists", async () => {
  imageFixture([{ ...imageNode, node_name: "another-node", transferred_bytes: 900 }, imageNode]);
  show(imageDeployment());
  const bar = await screen.findByRole("progressbar", { name: "Image transfer progress: gpu-target" });
  assert.equal(bar.getAttribute("aria-valuenow"), "25");
  assert.ok(screen.getByText("Syncing image"));
  assert.ok(screen.getByText("256 B / 1.0 KiB · 128 B/s"));
  assert.equal(screen.queryByText(/See transfer progress on the Images page/), null);
});

test("unknown transfer totals do not borrow catalog size or invent a percentage", async () => {
  imageFixture([{ ...imageNode, state: "pulling", size_bytes: 0 }]);
  show(imageDeployment());
  await screen.findByText("Downloading image");
  assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), null);
  assert.ok(screen.getByText("Transferred 256 B · 128 B/s"));
});

test("a syncing node without byte telemetry stays indeterminate instead of claiming zero bytes", async () => {
  for (const size_bytes of [undefined, 1024]) {
    imageFixture([{ ...imageNode, size_bytes, transferred_bytes: undefined, rate_bps: undefined }]);
    show(imageDeployment());
    await screen.findByText("Syncing image");
    const bar = screen.getByRole("progressbar");
    assert.equal(bar.getAttribute("aria-valuenow"), null);
    assert.equal(bar.getAttribute("aria-valuetext"), "Syncing image");
    assert.equal(screen.queryByText(/Transferred 0 B|0 B \/|0%/), null);
    cleanup();
  }
});

test("indeterminate sync switches to measured progress when the node reports real byte counts", async () => {
  imageFixture([imageNode]);
  let reads = 0;
  api.image = async () => ({ id: 4, versions: [{ id: 23, nodes: [++reads === 1
    ? { ...imageNode, size_bytes: undefined, transferred_bytes: undefined, rate_bps: undefined }
    : imageNode] }] }) as never;
  show(imageDeployment());
  const bar = await screen.findByRole("progressbar");
  assert.equal(bar.getAttribute("aria-valuenow"), null);
  await waitFor(() => assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "25"));
  assert.ok(screen.getByText("256 B / 1.0 KiB · 128 B/s"));
});

test("zero bytes with a known total remains a measured zero percent", async () => {
  imageFixture([{ ...imageNode, transferred_bytes: 0, rate_bps: 0 }]);
  show(imageDeployment());
  const bar = await screen.findByRole("progressbar");
  assert.equal(bar.getAttribute("aria-valuenow"), "0");
});

test("finishing transfers and completed image rows stop showing speed", async () => {
  for (const state of ["pulling", "syncing", "present"]) {
    imageFixture([{ ...imageNode, state, transferred_bytes: 1024 }]);
    show(imageDeployment());
    await screen.findByText(state === "present" ? "1.0 KiB" : "1.0 KiB / 1.0 KiB");
    assert.equal(screen.queryByText(/B\/s/), null);
    if (state === "present") {
      assert.ok(screen.getByText("Image ready"));
      assert.equal(screen.queryByRole("progressbar"), null);
    }
    cleanup();
  }
});

test("failed and canceled transfers retain the reason without a running progress bar", async () => {
  for (const state of ["failed", "canceled"]) {
    imageFixture([{ ...imageNode, state: "failed", transfer_error_code: state === "canceled" ? "PEER_TRANSFER_CANCELED" : undefined, message: "connection interrupted" }]);
    show(imageDeployment());
    await screen.findByText(state === "failed" ? "Image transfer failed" : "Image transfer canceled");
    assert.ok(screen.getByText(state === "failed" ? "connection interrupted" : i18n.t("common:apiError.PEER_TRANSFER_CANCELED")));
    assert.equal(screen.queryByRole("progressbar"), null);
    assert.equal(screen.queryByText(/B\/s/), null);
    cleanup();
  }
});

test("missing or ambiguous node records never show another node's transfer", async () => {
  for (const rows of [[], [{ ...imageNode, node_name: "other" }], [imageNode, { ...imageNode, node_uuid: "duplicate-name" }]]) {
    imageFixture(rows);
    show(imageDeployment());
    await screen.findByText("Waiting for progress");
    assert.equal(screen.queryByRole("progressbar"), null);
    cleanup();
  }
});

test("an unavailable image read offers reload and recovers", async () => {
  imageFixture([imageNode]);
  const readImage = api.image;
  api.image = async () => { throw new Error("image read failed"); };
  show(imageDeployment());
  await screen.findByText("Progress unavailable");
  assert.equal(screen.queryByRole("progressbar"), null);
  api.image = readImage;
  await userEvent.setup({ document: dom.window.document }).click(screen.getByRole("button", { name: "Reload" }));
  await screen.findByRole("progressbar");
});

test("image progress refreshes while waiting and disappears when placement completes", async () => {
  imageFixture([imageNode]);
  let reads = 0;
  api.image = async () => ({ id: 4, versions: [{ id: 23, nodes: [{ ...imageNode, transferred_bytes: ++reads === 1 ? 256 : 512 }] }] }) as never;
  const view = show(imageDeployment());
  await screen.findByRole("progressbar");
  await waitFor(() => assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "50"));
  view.rerender(<I18nextProvider i18n={i18n}><DeploymentImagePreparation dep={deployment({ placement_status: undefined })} /></I18nextProvider>);
  assert.equal(screen.queryByRole("progressbar"), null);
});

test("a late image response cannot overwrite a newly selected deployment", async () => {
  imageFixture([imageNode]);
  let resolveOld: ((value: unknown) => void) | undefined;
  api.image = () => new Promise(resolve => { resolveOld = resolve as typeof resolveOld; });
  const view = show(imageDeployment());
  await waitFor(() => assert.ok(resolveOld));
  const next = { ...imageNode, node_name: "new-target", transferred_bytes: 768 };
  api.image = async () => ({ id: 4, versions: [{ id: 23, nodes: [next] }] }) as never;
  view.rerender(<I18nextProvider i18n={i18n}><DeploymentImagePreparation dep={imageDeployment("new-target", 8)} /></I18nextProvider>);
  await screen.findByRole("progressbar", { name: "Image transfer progress: new-target" });
  resolveOld!({ id: 4, versions: [{ id: 23, nodes: [imageNode] }] });
  await waitFor(() => assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "75"));
  assert.equal(screen.queryByText("Target node: gpu-target"), null);
});


test("model and image preparation share one resource section, even without an instance", async () => {
  imageFixture([imageNode]);
  const dep = { ...imageDeployment(), model_progress: [{ node_uuid: "target-uuid", node_name: "gpu-target", model_name: "Qwen/model", state: "downloading", size_bytes: 1024, downloaded: 512, rate_bps: 0, preparation: { phase: "downloading", total_known: true } }] } as never;
  show(dep);
  await screen.findByText("Syncing image");
  assert.equal(screen.getAllByRole("heading", { name: "Resource preparation" }).length, 1);
  const section = screen.getByRole("heading", { name: "Resource preparation" }).closest("section")!;
  assert.ok(section.textContent!.includes("Qwen/model"));
  assert.ok(section.textContent!.includes("registry/runner:v1"));
  assert.equal(screen.queryByRole("heading", { name: "Model preparation" }), null);
});

function modelRow(ready = false) {
  return { node_uuid: "target-uuid", node_name: "gpu-target", model_id: 1, model_name: "Qwen/model", storage_id: 1,
    state: ready ? "present" : "downloading", size_bytes: 2048, downloaded: ready ? 2048 : 512, rate_bps: 128,
    preparation: { phase: ready ? "ready" : "downloading", total_known: true, idle_seconds: 0 } };
}
function update(view: ReturnType<typeof show>, dep: Deployment) {
  view.rerender(<I18nextProvider i18n={i18n}><DeploymentImagePreparation dep={dep} /></I18nextProvider>);
}

test("sequential preparation keeps the completed model and introduces the image only when its task starts", async () => {
  imageFixture([imageNode]);
  const initial = deployment({ state: "pending", instances: [], model_progress: [modelRow()] });
  const view = show(initial);
  assert.ok(screen.getByText("Downloading model"));
  assert.equal(screen.queryByLabelText("Image preparation progress"), null);
  update(view, { ...initial, model_progress: [modelRow(true)] } as Deployment);
  assert.ok(screen.getByText("Model ready"));
  assert.ok(screen.getByText("2.0 KiB"));
  assert.equal(screen.queryByRole("progressbar"), null);
  assert.equal(screen.queryByText(/B\/s/), null);
  assert.equal(screen.queryByLabelText("Image preparation progress"), null);
  update(view, { ...initial, ...imageDeployment(), model_progress: [modelRow(true)] } as Deployment);
  await screen.findByText("Syncing image");
  assert.ok(screen.getByText("Model ready"));
  assert.equal(screen.getAllByRole("progressbar").length, 1);
  assert.equal(document.querySelectorAll('[data-status="done"]').length, 1);
});

test("parallel preparation removes only the completed model's progress and speed", async () => {
  imageFixture([imageNode]);
  const dep = { ...imageDeployment(), model_progress: [modelRow()] } as Deployment;
  const view = show(dep);
  await screen.findByText("Syncing image");
  assert.equal(screen.getAllByRole("progressbar").length, 2);
  update(view, { ...dep, model_progress: [{ ...modelRow(true), preparation: modelRow().preparation, message: "old download output" }] } as Deployment);
  assert.ok(screen.getByText("Model ready"));
  assert.equal(screen.getAllByRole("progressbar").length, 1);
  assert.equal(screen.queryByText("old download output"), null);
  assert.equal(screen.queryByText("Downloading model"), null);
});

test("an observed image remains ready while the model finishes, then both disappear when the instance starts", async () => {
  imageFixture([{ ...imageNode, state: "present", transferred_bytes: 1024 }]);
  const initial = { ...imageDeployment(), state: "pending", instances: [], model_progress: [] } as Deployment;
  const view = show(initial);
  await screen.findByText("Image ready");
  assert.equal(screen.queryByText("Qwen/model"), null);
  const modelOnlyWait = { ...initial, placement_status: undefined, model_progress: [modelRow()] } as Deployment;
  update(view, modelOnlyWait);
  assert.ok(screen.getByText("Image ready"));
  assert.ok(screen.getByText("Downloading model"));
  assert.equal(screen.getAllByRole("progressbar").length, 1);
  update(view, { ...modelOnlyWait, model_progress: [modelRow(true)] } as Deployment);
  assert.ok(screen.getByText("Image ready"));
  assert.ok(screen.getByText("Model ready"));
  assert.equal(screen.queryByRole("progressbar"), null);
  assert.equal(screen.queryByText(/B\/s/), null);
  update(view, { ...modelOnlyWait, state: "starting", model_progress: [modelRow(true)], instances: [{ uuid: "new-instance", state: "starting" }] } as Deployment);
  assert.equal(screen.queryByRole("heading", { name: "Resource preparation" }), null);
});

test("completed image history does not leak to another deployment or target node", async () => {
  imageFixture([{ ...imageNode, state: "present" }]);
  const view = show({ ...imageDeployment(), model_progress: [modelRow()] } as Deployment);
  await screen.findByText("Image ready");
  update(view, deployment({ id: 8, state: "pending", model_progress: [modelRow()] }));
  assert.equal(screen.queryByLabelText("Image preparation progress"), null);
  update(view, { ...imageDeployment(), model_progress: [modelRow()] } as Deployment);
  await screen.findByText("Image ready");
  update(view, deployment({ node_uuid: "new-target", state: "pending", model_progress: [modelRow()] }));
  assert.equal(screen.queryByLabelText("Image preparation progress"), null);
});

test("completed shared model rows remain deduplicated while an image is being prepared", async () => {
  imageFixture([imageNode]);
  const ready = { ...modelRow(true), storage_type: "nfs", transfer_id: "same-transfer" };
  show({ ...imageDeployment(), model_progress: [ready, { ...ready, node_uuid: "second-node", node_name: "second-node" }] } as Deployment);
  await screen.findByText("Syncing image");
  assert.equal(screen.getAllByText("Model ready").length, 1);
});
