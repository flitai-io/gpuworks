import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { installCanvas } from "./helpers/analyticsFixtures.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { cursorPage } from "./helpers/listPage.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
installCanvas(dom);
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class EventSourceStub { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    networks: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/networks.json", import.meta.url), "utf8")),
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: InstancePanel } = await import("../src/pages/InstancePanel.tsx");
const originalFetch = globalThis.fetch;
const originalInstanceEvents = api.instanceEvents;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  api.instanceEvents = originalInstanceEvents;
});

const base = {
  uuid: "launch-facts-ui", deployment_id: 1, node_uuid: "node-1", state: "running",
  gpu_indexes: [0], ports: [30000], api_port: 30000, image: "image", message: "", container_id: "abc123", sleep_level: 0,
} as never;

function renderPanel(inst: object = base, sleepable = false) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><InstancePanel inst={inst as never} sleepable={sleepable} /></MemoryRouter></I18nextProvider>);
}

function response(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

test("a whole-card instance shows a scheduling exception only when the selected GPUs differ from its assignment", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const inst = { ...(base as object), gpu_indexes: [1, 0], schedule: { explain: { topo_highlight: { gpus: [0, 1] } } }, env: { MODE: "local" } };
  const { unmount } = renderPanel(inst);
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts" }));
  assert.ok(screen.getByText("MODE=local"));
  assert.equal(screen.queryByText(/node adds dGPU/), null);
  assert.equal(screen.queryByText("Scheduled GPUs"), null);
  unmount();

  renderPanel({ ...inst, gpu_indexes: [0], vram_limit_bytes: 8 * 1024 ** 3 });
  assert.ok(screen.getByText("Scheduled GPUs"));
  assert.ok(screen.getByText("0, 1"));
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts" }));
  assert.ok(screen.getByText(/node adds dGPU/));
});

test("the L1 wake estimate appears for a sleepable running service but does not describe whole-card usage or deep sleep", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const inst = { ...(base as object), schedule: { breakdown: { weight_bytes: 14e9, tensor_parallel: 1 } } };
  const active = renderPanel(inst);
  assert.equal(screen.queryByText(i18n.t("instance.wakeEstimate", { seconds: 1 })), null);
  active.unmount();

  const sleepable = renderPanel(inst, true);
  assert.ok(screen.getByText(i18n.t("instance.wakeEstimate", { seconds: 1 })));
  sleepable.unmount();

  renderPanel({ ...inst, sleep_level: 2, desired_sleep: 2, asleep: true }, true);
  assert.equal(screen.queryByText(i18n.t("instance.wakeEstimate", { seconds: 1 })), null);
  assert.ok(screen.getByText(i18n.t("instance.sleepWeight", { storage: i18n.t("instance.disk"), gib: (14e9 / 1024 ** 3).toFixed(1) })));
});

test("launch facts keeps loading and transport error states distinct", async () => {
  api.instanceEvents = async () => cursorPage([]);
  let resolve: ((value: Response) => void) | undefined;
  globalThis.fetch = (() => new Promise<Response>((done) => { resolve = done; })) as typeof fetch;
  renderPanel();
  fireEvent.click(screen.getByRole("button", { name: i18n.t("instance.launchFacts.title"), expanded: false }));
  assert.ok(screen.getByLabelText("Loading launch facts"));
  resolve?.(response({}, 503));
  await waitFor(() => assert.ok(screen.getByText("Launch facts could not be loaded.")));
  assert.ok(screen.getByRole("button", { name: "Retry" }));
});

test("launch facts renders unknown reasons and current relation from the server projection", async () => {
  api.instanceEvents = async () => cursorPage([]);
  const facts = {
    intent: { status: "unknown" }, resolved: { status: "unknown" },
    runtime_observation_relation: { status: "current" },
    runtime_observation_received_at: "2026-09-20T00:00:00Z",
    runtime_observation: {
      runtime: "docker",
      container: { status: "observed", container_id: "abc123", image_id: "sha256:image", entrypoint: [], cmd: [], args: [], env: {}, mounts: [], devices: [] },
      image: { status: "unknown", reason: "not_collected", entrypoint: [], cmd: [], env: {}, volumes: [] },
      process: { status: "unknown", reason: "non_local_pid" },
    },
  };
  globalThis.fetch = (async (input) => {
    const url = String(input);
    return url.includes("/metrics") ? response([]) : response(facts);
  }) as typeof fetch;
  renderPanel();
  fireEvent.click(screen.getByRole("button", { name: i18n.t("instance.launchFacts.title"), expanded: false }));
  await waitFor(() => assert.ok(screen.getByText("Observation relation:")));
  assert.ok(screen.getByText("Current"));
  screen.getByText("Observed image").click();
  await waitFor(() => assert.ok(screen.getByText("This layer has not been collected.")));
  screen.getByText("Observed process").click();
  await waitFor(() => assert.ok(screen.getByText("The process was not proven to be local to this runtime.")));
});

test("a multi-node rank shows the compute network it started on", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const management = { name: "management", management: true, type: "host_ethernet", interface: "eth0", ip: "172.25.0.11" };
  const { unmount } = renderPanel({ ...(base as object), schedule: { network: { compute: management } } });
  assert.ok(screen.getByText("Compute network"));
  assert.ok(screen.getByRole("link", { name: "Management network" }));
  assert.ok(screen.getByText("Fallback: the group shares no dedicated network"));
  assert.ok(screen.getByText("eth0 · 172.25.0.11"));
  unmount();

  const rdma = { name: "ib-a", management: false, type: "host_rdma", interface: "bond0", ip: "10.20.1.11", link_layer: "infiniband" };
  renderPanel({ ...(base as object), schedule: { network: { compute: rdma } } });
  assert.ok(screen.getByText("ib-a"));
  assert.ok(screen.getByText("InfiniBand"));
  assert.equal(screen.queryByText("Fallback: the group shares no dedicated network"), null);
});

test("a single-node instance has no compute network row", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  renderPanel();
  assert.equal(screen.queryByText("Compute network"), null);
});

test("the runtime section states the /dev/shm size, or the host's under --ipc=host", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response([])) as typeof fetch;
  const { unmount } = renderPanel({ ...(base as object), shm_bytes: 32 * (1 << 30), docker_args: ["--ulimit", "nofile=65536:65536"] });
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts", expanded: false }));
  await waitFor(() => assert.ok(screen.getByText("/dev/shm 32 GiB")));
  assert.equal(screen.queryByText("Host /dev/shm (--ipc=host)"), null);
  unmount();

  renderPanel({ ...(base as object), docker_args: ["--ipc", "host"] });
  fireEvent.click(screen.getByRole("button", { name: "Environment & Mounts", expanded: false }));
  await waitFor(() => assert.ok(screen.getByText("Host /dev/shm (--ipc=host)")));
  assert.equal(screen.queryByText(/^\/dev\/shm \d/), null);
});

test("the panel names the model files the instance reads and the storage each is in", async () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response({})) as typeof fetch;
  renderPanel({
    ...(base as object),
    model_files: [
      { model_id: 7, storage_id: 2, storage_name: "lab-nfs", storage_type: "nfs", path: "/mnt/gpuworks/lab-nfs/Qwen/Qwen2.5-0.5B" },
      { model_id: 8, path: "/opt/gpuworks/models/draft" },
    ],
  });
  assert.ok(screen.getByText("Model files"));
  assert.ok(screen.getByRole("link", { name: "lab-nfs" }));
  assert.ok(screen.getByText("/mnt/gpuworks/lab-nfs/Qwen/Qwen2.5-0.5B"));
  // A file whose storage was removed since still shows where it was read.
  assert.ok(screen.getByText("Storage removed"));
  assert.ok(screen.getByText("/opt/gpuworks/models/draft"));
});

test("an instance from before model files were recorded shows no model files row", () => {
  api.instanceEvents = async () => cursorPage([]);
  globalThis.fetch = (async () => response({})) as typeof fetch;
  renderPanel();
  assert.equal(screen.queryByText("Model files"), null);
});
