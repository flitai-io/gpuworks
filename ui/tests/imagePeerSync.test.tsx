import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const imagesLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { images: imagesLocale } },
  defaultNS: "images",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const ImageNodeTable = (await import("../src/pages/images/ImageNodeTable.tsx")).default;
const { peerSources } = await import("../src/pages/images/ImagePageHelpers.tsx");

afterEach(() => cleanup());

function wrap(node: React.ReactElement) {
  return React.createElement(I18nextProvider, { i18n }, node);
}

// Neither node reports an address: that is how an ordinary agent_dial
// install looks, and peer sync must still be offered there.
function version(target: Record<string, unknown> = { state: "absent" }) {
  return {
    id: 9,
    series_id: 1,
    name: "runner",
    image_ref: "gpustack/runner:cuda13.0-vllm0.25.1",
    source: "custom",
    enabled: true,
    reference_count: 0,
    // Each row names its node and carries its state, as GET /images does.
    nodes: [
      { node_uuid: "src", node_name: "src-host", node_state: "online", state: "present" },
      { node_uuid: "gone", node_name: "gone-host", node_state: "offline", state: "present" },
      { node_uuid: "nogpu", node_name: "nogpu-host", node_state: "online", state: "present" },
      { node_uuid: "dst", node_name: "dst-host", node_state: "online", ...target },
    ],
  } as never;
}

test("any other connected node holding the image is a peer source, with or without a direct address", () => {
  const got = peerSources(version(), { node_uuid: "dst", state: "absent" } as never);
  assert.deepEqual(got.map((node) => node.node_uuid), ["src", "nogpu"], "every connected holder serves a copy; offline holders and the target itself are excluded");
  assert.deepEqual(peerSources(version(), { node_uuid: "src", state: "present" } as never).map((node) => node.node_uuid), ["nogpu"], "a node without the image is never a source");
});

function renderTable(target: Record<string, unknown>, probe: () => Promise<unknown>) {
  const syncs: Array<{ node: string; source: string; rate: number }> = [];
  const probes: Array<{ node: string; source: string }> = [];
  render(wrap(React.createElement(ImageNodeTable, {
    selectedVersion: version(target),
    inspecting: null,
    availablePeer: (v: never, row: never) => peerSources(v, row).length > 0,
    modeFor: () => "peer",
    setMode: () => {},
    pullVersion: () => { throw new Error("a peer copy must go through the dialog"); },
    probeImage: async (_v: never, node: string, source: string) => { probes.push({ node, source }); return probe(); },
    syncFromPeer: async (_v: never, node: string, source: string, rate: number) => { syncs.push({ node, source, rate }); },
    pullFromURL: async () => {},
    inspectVersion: () => {},
    cancelTransfer: () => {},
    removeFromNode: () => {},
  } as never)));
  return { syncs, probes };
}

const dialogText = imagesLocale.nodeTable.peerDialog;

function startButton() {
  return screen.getByRole("button", { name: dialogText.start }) as HTMLButtonElement;
}

test("peer sync is chosen, probed and rate limited in a dialog", async () => {
  const { syncs, probes } = renderTable({ state: "absent" }, async () => ({ state: "ready", transport: "direct" }));
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.peerAction }));
  await screen.findByText(dialogText.description);
  assert.equal(startButton().disabled, true, "nothing starts before a source is chosen and probed");

  // Two holders are connected, so the operator picks which one serves.
  fireEvent.mouseDown(screen.getByRole("combobox", { name: dialogText.source }));
  // The node table lists it too, so the option is the last match.
  await waitFor(() => assert.ok(screen.getAllByText("nogpu-host").length > 1));
  await user.click(screen.getAllByText("src-host").at(-1)!);
  assert.equal(startButton().disabled, true, "a chosen but unprobed source is still locked");

  await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.testTransport }));
  await screen.findByText(dialogText.probeDirect);
  assert.deepEqual(probes, [{ node: "dst", source: "src" }]);

  await user.type(screen.getByRole("spinbutton", { name: imagesLocale.nodeTable.rateLimit }), "2");
  await waitFor(() => assert.equal(startButton().disabled, false));
  await user.click(startButton());
  await waitFor(() => assert.deepEqual(syncs, [{ node: "dst", source: "src", rate: 2048 }], "the MiB/s limit is sent in KiB/s"));
});

test("a failed peer sync reopens with its source and limit, and a failed probe keeps it locked", async () => {
  const { syncs, probes } = renderTable(
    { state: "failed", source: "peer", source_node_uuid: "src", source_node_name: "src-host", last_rate_limit_kib_s: 1024 },
    async () => { throw new Error("source node cannot connect to target direct address"); },
  );
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.peerAction }));
  await screen.findByText(dialogText.description);
  assert.equal((screen.getByRole("spinbutton", { name: imagesLocale.nodeTable.rateLimit }) as HTMLInputElement).value, "1", "the last limit is prefilled");

  await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.testTransport }));
  await screen.findByText(dialogText.probeFailed);
  assert.ok(screen.getByText(/cannot connect to target direct address/), "the server's reason is shown");
  assert.deepEqual(probes, [{ node: "dst", source: "src" }], "the recorded source is preselected");
  assert.equal(startButton().disabled, true);
  await user.click(startButton());
  assert.deepEqual(syncs, []);
});


test("a relayed transport probe unlocks peer sync and identifies the path", async () => {
  const { syncs } = renderTable({ state: "failed", source: "peer", source_node_uuid: "src" }, async () => ({ state: "ready", transport: "relay" }));
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.peerAction }));
  assert.equal(startButton().disabled, true);
  await user.click(screen.getByRole("button", { name: "Test transfer connection" }));
  await screen.findByText("Connection verified via the management server relay.");
  assert.equal(startButton().disabled, false);
  await user.click(startButton());
  await waitFor(() => assert.deepEqual(syncs, [{ node: "dst", source: "src", rate: 0 }]));
});
