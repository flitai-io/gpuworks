import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}

const locales = Object.fromEntries((["en", "zh"] as const).map((lng) => [
  lng, { deploy: JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/deploy.json`, import.meta.url), "utf8")) },
]));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", resources: locales, defaultNS: "deploy", interpolation: { escapeValue: false },
});

const { cleanup, render, screen, within } = await import("@testing-library/react");
const DeployComponentAcquisition = (await import("../src/pages/deploy/DeployComponentAcquisition.tsx")).default;
type AcquisitionResult = import("../src/api").AcquisitionResult;

afterEach(() => cleanup());
after(() => dom.window.close());

const GB = 66_726_510_529;
const encoder = { name: "text-encoder", role: "auxiliary", model: "MiniMax/MiniMax-H3", size_bytes: GB };

function show(acquisition: AcquisitionResult | undefined) {
  return render(
    <I18nextProvider i18n={i18n}>
      <DeployComponentAcquisition acquisition={acquisition} />
    </I18nextProvider>,
  );
}

function item(): HTMLElement {
  return within(screen.getByRole("list", { name: "Component weights" })).getByRole("listitem");
}

// The case that writes into shared storage without anyone choosing it: the
// form names the storage, says it is written once, what it costs, and how to
// avoid it, before the person deploys.
test("a component that follows the primary into a shared storage says where, how much and how to avoid it", () => {
  show({ components: [{ ...encoder, plan: "shared_download", nodes: 2, ready_nodes: 0, storage_id: 3, storage: "models-nfs", follows_primary: true }] });
  const entry = item();
  within(entry).getByText("text-encoder · MiniMax/MiniMax-H3 · 62.1 GiB");
  within(entry).getByText(/Will be downloaded into the shared storage "models-nfs" \(where the primary model is\), written once by one node and shared by the 2 target nodes/);
  within(entry).getByText(/Uses 62.1 GiB of "models-nfs"; deleting the deployment does not delete this copy/);
  // The form asks no Save to here (the primary already sits on the storage),
  // so the hint points only at the node requirements.
  within(entry).getByText("To keep it out of this storage, limit the storage in the node requirements");
  assert.equal(within(entry).getByRole("status").dataset.status, "info");
});

test("the avoid hint names Save to only when the form asks it, and one node reads as one", () => {
  const targets = [
    { key: "storage:1", label: "default", reason: "", recommended: true, storage_id: 1 },
    { key: "storage:2", label: "scratch", reason: "", recommended: false, storage_id: 2 },
  ];
  show({ targets, components: [{ ...encoder, plan: "shared_download", nodes: 1, ready_nodes: 0, storage: "models-nfs", follows_primary: true }] });
  within(item()).getByText(/written once by one node; the target node reads that copy/);
  within(item()).getByText("To keep it out of this storage, limit the storage in the node requirements, or choose a local storage under Save to");
});

test("a shared storage the person chose is stated without the avoid hint", () => {
  show({ components: [{ ...encoder, plan: "shared_download", nodes: 1, ready_nodes: 0, storage: "models-nfs" }] });
  within(item()).getByText(/Will be downloaded into the chosen shared storage "models-nfs"/);
  assert.equal(within(item()).queryByText(/limit the storage/), null);
  assert.equal(within(item()).getByRole("status").dataset.status, "neutral");
});

test("nodes that download the component themselves are counted with the total size", () => {
  show({ components: [{ ...encoder, plan: "node_download", nodes: 3, ready_nodes: 1 }] });
  within(item()).getByText("2 nodes each download it into their local storage (124.3 GiB in total); the first start waits for the download");
  within(item()).getByText("1 node already has it and reads it directly");
});

test("a component every node has needs no download", () => {
  show({ components: [{ ...encoder, plan: "ready", nodes: 2, ready_nodes: 2 }] });
  within(item()).getByText("All 2 target nodes already have it; nothing to download");
  cleanup();
  show({ components: [{ ...encoder, plan: "ready", nodes: 1, ready_nodes: 1 }] });
  within(item()).getByText("The target node already has it; nothing to download");
  assert.equal(within(item()).getByRole("status").dataset.status, "default");
});

test("no components, or no forecast yet, renders nothing", () => {
  const { container } = show({ nodes: [] });
  assert.equal(container.innerHTML, "");
  cleanup();
  assert.equal(show(undefined).container.innerHTML, "");
});

test("the Chinese text reads naturally for the shared case", async () => {
  await i18n.changeLanguage("zh");
  try {
    show({ components: [{ ...encoder, plan: "shared_download", nodes: 2, ready_nodes: 1, storage: "models-nfs", follows_primary: true }] });
    within(screen.getByRole("list", { name: "组件权重" })).getByText(/将下载到共享存储「models-nfs」（主模型所在的存储），由一个节点写入一次，2 个目标节点共用这一份/);
    screen.getByText("其中 1 个节点已有，直接读取");
  } finally {
    await i18n.changeLanguage("en");
  }
});
