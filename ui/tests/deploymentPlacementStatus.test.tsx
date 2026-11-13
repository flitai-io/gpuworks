import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// A deployment whose replicas wait for a node says so in its drawer: how
// many, the controller's reason with the refusal it names rendered, the
// closest nodes; and the live candidate table, one server page at a time,
// filtered by status, with its loading, empty and error states.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
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
const locale = i18n.getResourceBundle("en", "deployments").detail.placement;

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { PlacementStatusAlert } = await import("../src/pages/DeploymentPlacementStatus.tsx");

type Page = { status?: string; offset?: number; limit?: number };
const apiAny = api as unknown as Record<string, unknown>;
const original = apiAny.deploymentCandidates;
afterEach(() => { cleanup(); apiAny.deploymentCandidates = original; });
after(() => { closeChannels(); dom.window.close(); });

function deployment(overrides: Record<string, unknown> = {}) {
  return {
    id: 7, name: "waiting", model_name: "m", model_id: 1, served_name: "m", engine: "vllm", node_uuid: "n-1",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: 3, template_deviated: false,
    created_at: "2026-10-03T00:00:00Z", node_requirements: { "gpuworks.ai/vendor": ["nvidia"], pool: ["a", "b"] },
    placement_strategy: "spread",
    placement_status: {
      unscheduled: 2, at: "2026-10-03T01:02:03Z",
      reason: { code: "pool_no_fit", params: {
        candidates: 40, tried: 8, node: "gpu-7", first: "no_free_gpu", first_reason: { code: "no_free_gpu" },
        closest: [{ node: "u-7", name: "gpu-7", reason: { code: "no_free_gpu" } }, { node: "u-9", name: "gpu-9", reason: { code: "no_free_gpu" } }],
      } },
    },
    ...overrides,
  } as never;
}

function candidates(items: Array<Record<string, unknown>>, total = items.length) {
  return {
    nodes: 1000, counts: { room: 3, busy: 895, unmet: 90, cordoned: 5, offline: 7 }, unmet_by_key: { pool: 90 },
    placement_strategy: "spread", total, offset: 0, limit: 50, items,
  };
}

function node(index: number, status = "busy") {
  return { uuid: `u-${index}`, name: `gpu-${index}`, status, rank: index, replicas: 0, free_gpus: 0, gpus: 8, model: "absent" };
}

function show(dep = deployment()) {
  return render(<I18nextProvider i18n={i18n}><PlacementStatusAlert dep={dep} /></I18nextProvider>);
}

test("nothing is shown while every replica has a node", () => {
  show(deployment({ placement_status: undefined }));
  assert.equal(screen.queryByText(/waiting for a node/), null);
});

test("the waiting count, the controller's reason with its refusal rendered, and the closest nodes", () => {
  show();
  assert.ok(screen.getByText(i18n.t("detail.placement.waiting", { count: 2 })));
  const noFreeGPU = i18n.t("deploy:schedule.no_free_gpu");
  // {{first}} is the rendered refusal, never the raw code.
  const reason = screen.getByText(new RegExp("None of the 8 best of 40 candidate nodes"));
  assert.ok(reason.textContent!.includes(noFreeGPU), reason.textContent!);
  assert.ok(!reason.textContent!.includes("no_free_gpu"), reason.textContent!);
  assert.ok(screen.getByText(locale.closest));
  assert.ok(screen.getByText("gpu-7") && screen.getByText("gpu-9"));
});

test("the candidate table pages and filters on the server, from the keyboard", async () => {
  const asked: Page[] = [];
  apiAny.deploymentCandidates = async (id: number, page: Page) => {
    assert.equal(id, 7);
    asked.push(page);
    if (page.status === "unmet") {
      return candidates([
        { ...node(1, "unmet"), rank: undefined, detail: "node gpu-1 does not satisfy pool ∈ {a, b}" },
        { ...node(2, "unmet"), rank: undefined, reason: { code: "accelerator_feature_unsupported", params: { vendor: "AMD", feature: "engine_sleep" } } },
      ], 90);
    }
    return candidates(Array.from({ length: 50 }, (_, i) => node((page.offset ?? 0) + i + 1)), 1000);
  };
  const user = userEvent.setup({ document: dom.window.document });
  show();
  await user.click(screen.getByRole("button", { name: locale.view }));
  const modal = (await screen.findByText(locale.candidates)).closest(".ant-modal") as HTMLElement;
  await within(modal).findByText("gpu-1");
  assert.deepEqual(asked[0], { status: undefined, offset: 0, limit: 50 });
  // The counts cover the whole cluster, not the page.
  assert.ok(within(modal).getByText(i18n.t("detail.placement.summary", { nodes: 1000, room: 3, busy: 895, unmet: 90, cordoned: 5, offline: 7 })));
  assert.ok(within(modal).getByText("gpuworks.ai/vendor=nvidia") && within(modal).getByText("pool=a | b"));

  await user.click(within(modal).getByTitle("2"));
  await within(modal).findByText("gpu-51");
  assert.deepEqual(asked.at(-1), { status: undefined, offset: 50, limit: 50 });

  // The status filter is a radio group: reach it with the keyboard.
  const unmet = within(modal).getByRole("radio", { name: new RegExp(locale.status.unmet) });
  unmet.focus();
  await user.keyboard(" ");
  await within(modal).findByText("node gpu-1 does not satisfy pool ∈ {a, b}");
  assert.deepEqual(asked.at(-1), { status: "unmet", offset: 0, limit: 50 });
  // An accelerator refusal arrives as a code and is rendered in the reader's language.
  const feature = i18n.t("common:acceleratorFeature.names.engine_sleep");
  assert.notEqual(feature, "engine_sleep");
  assert.ok(within(modal).getByText(i18n.t("deploy:schedule.accelerator_feature_unsupported", { vendor: "AMD", feature })));
  assert.equal(within(modal).queryByText(/accelerator_feature_unsupported/), null);
});

test("an empty page and a failed load say so", async () => {
  apiAny.deploymentCandidates = async () => candidates([]);
  const user = userEvent.setup({ document: dom.window.document });
  show();
  await user.click(screen.getByRole("button", { name: locale.view }));
  const modal = (await screen.findByText(locale.candidates)).closest(".ant-modal") as HTMLElement;
  await waitFor(() => assert.ok(modal.querySelector(".ant-empty")));
  cleanup();

  apiAny.deploymentCandidates = async () => { throw new APIError("FIELD_INVALID", { field: "limit", detail: "1..200" }, "limit must be between 1 and 200", 400); };
  show();
  await user.click(screen.getByRole("button", { name: locale.view }));
  await screen.findByText(new RegExp(i18n.t("detail.placement.loadFailed", { detail: "" }).trim()));
});


test("replicas preparing a shared model do not claim they are waiting for nodes", () => {
  show(deployment({ model_progress: [{ node_uuid: "n-1", state: "downloading", preparation: { phase: "downloading", total_known: true, idle_seconds: 0 } }] }));
  assert.ok(screen.getByText("Preparing model; 2 replica(s) have not started"));
  assert.ok(screen.getByText("Downloading model"));
  assert.equal(screen.queryByText(i18n.t("detail.placement.waiting", { count: 2 })), null);
  assert.equal(screen.queryByText(locale.closest), null, "old scheduler refusals must not describe an active download");
});


test("failed cache cleanup does not masquerade as active model preparation", () => {
  show(deployment({ model_progress: [{ node_uuid: "n-1", state: "failed", preparation: { phase: "eviction_failed", reason: "MODEL_CACHE_EVICTION_FAILED", total_known: false, idle_seconds: 0 } }] }));
  assert.ok(screen.getByText(i18n.t("detail.placement.waiting", { count: 2 })));
  assert.ok(screen.getByText(locale.closest));
  assert.equal(screen.queryByText("Preparing model; 2 replica(s) have not started"), null);
});

// The reported bug: the copy failed with a permission error, and the screen
// explained the missing replicas with the GPU assessment instead. A failed
// preparation is the known cause, so the node refusal is not the answer.
test("a failed model preparation explains the missing replicas, not the GPU verdict", () => {
  show(deployment({ model_progress: [{ node_uuid: "n-1", state: "failed", message: "copy .msc: open /cache/weights/.msc: permission denied", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0, retryable: true } }] }));
  assert.ok(screen.getByText(i18n.t("detail.placement.preparationFailed", { count: 2 })));
  assert.ok(screen.getByText(i18n.t("preparation.reason.MODEL_PREPARATION_FAILED")));
  // The agent's own words, as it reported them.
  assert.ok(screen.getByText("copy .msc: open /cache/weights/.msc: permission denied"));
  assert.ok(screen.getByText(i18n.t("preparation.noAutoRetry")));
  // The GPU assessment is neither the headline nor the explanation, and the
  // stale closest-node list is gone with it.
  const noFreeGPU = i18n.t("deploy:schedule.no_free_gpu");
  assert.equal(screen.queryByText(new RegExp(noFreeGPU)), null);
  assert.equal(document.body.textContent!.includes("no_free_gpu"), false);
  assert.equal(screen.queryByText(locale.closest), null);
  assert.equal(screen.queryByText(i18n.t("detail.placement.waiting", { count: 2 })), null);
  // A failure is a warning, never an info notice.
  assert.ok(document.querySelector(".ant-alert-error"), "a failed preparation is an error-styled alert");
});

// The other half: with no preparation problem at all the GPU refusal is the
// real reason and stays exactly as it was.
test("a genuine no-free-GPU refusal still shows without a preparation failure", () => {
  show(deployment());
  const reason = screen.getByText(new RegExp("None of the 8 best of 40 candidate nodes"));
  assert.ok(reason.textContent!.includes(i18n.t("deploy:schedule.no_free_gpu")), reason.textContent!);
  assert.equal(screen.queryByText(locale.preparationFailed.replace("{{count}}", "2")), null);
});

// A copy still running keeps its own wording; the failure wording is not
// applied to work in progress.
test("active preparation and failed preparation say different things", () => {
  show(deployment({ model_progress: [{ node_uuid: "n-1", state: "copying", preparation: { phase: "copying", total_known: true, idle_seconds: 0 } }] }));
  assert.ok(screen.getByText(i18n.t("detail.placement.preparingModel", { count: 2 })));
  cleanup();
  show(deployment({ model_progress: [{ node_uuid: "n-1", state: "failed", preparation: { phase: "waiting", reason: "MODEL_PREPARATION_FAILED", total_known: false, idle_seconds: 0 } }] }));
  assert.ok(screen.getByText(i18n.t("detail.placement.preparationFailed", { count: 2 })));
  assert.equal(screen.queryByText(i18n.t("detail.placement.preparingModel", { count: 2 })), null);
});


test("a disk-space block remains the placement cause while another model is preparing", () => {
  const detail = "insufficient disk space at /cache/models: available=1048576 required=2147483648";
  show(deployment({ model_progress: [
    { node_uuid: "n-2", state: "downloading", preparation: { phase: "downloading", total_known: true, idle_seconds: 0 } },
    { node_uuid: "n-1", state: "insufficient_disk_space", message: detail, preparation: { phase: "waiting", reason: "MODEL_CACHE_DISK_FULL", total_known: false, idle_seconds: 0 } },
  ] }));
  assert.ok(screen.getByText("Insufficient model disk space; 2 replica(s) have not started"));
  assert.ok(screen.getByText("Insufficient model disk space"));
  assert.ok(screen.getByText(detail));
  assert.equal(screen.queryByText(i18n.t("detail.placement.preparingModel", { count: 2 })), null);
  assert.equal(screen.queryByText(i18n.t("detail.placement.waiting", { count: 2 })), null);
  assert.equal(screen.queryByText(locale.closest), null, "the old scheduler refusal must not hide disk exhaustion");
  assert.equal(screen.queryByText(i18n.t("preparation.noAutoRetry")), null, "disk admission must not invent copy retry policy");
});
