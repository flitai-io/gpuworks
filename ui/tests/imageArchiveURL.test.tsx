import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const imagesLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"));
const imagesZhLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/images.json", import.meta.url), "utf8"));
const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const commonZhLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { images: imagesLocale, common: commonLocale }, zh: { images: imagesZhLocale, common: commonZhLocale } },
  defaultNS: "images",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const ImageArchiveURLImport = (await import("../src/pages/images/ImageArchiveURLImport.tsx")).default;
const ImageNodeTable = (await import("../src/pages/images/ImageNodeTable.tsx")).default;
const { archiveURLDialogValue, autoSourceValue, imageSourceOptions, isArchiveURL, archiveURLHost, transferRunning } = await import("../src/pages/images/ImagePageHelpers.tsx");

afterEach(() => cleanup());
after(() => { closeChannels(); dom.window.close(); });

const ARCHIVE_URL = "https://containerimages.example/gpustack_runner--gpu3.tar";

function wrap(node: React.ReactElement) {
  return React.createElement(I18nextProvider, { i18n }, node);
}

function stubPreview(result: unknown, error?: Error) {
  const original = apiModule.api.previewImageArchiveURL;
  const calls: string[] = [];
  apiModule.api.previewImageArchiveURL = async (url: string) => {
    calls.push(url);
    if (error) throw error;
    return result as never;
  };
  return { calls, restore: () => { apiModule.api.previewImageArchiveURL = original; } };
}

function catalogVersion(overrides: Record<string, unknown> = {}) {
  return {
    id: 7,
    series_id: 3,
    name: "runner",
    image_ref: "gpustack/runner:cuda13.0-vllm0.25.1",
    source: "custom",
    enabled: true,
    reference_count: 0,
    ...overrides,
  } as never;
}

test("resolving an archive whose reference is already in the catalog reports the match", async () => {
  const stub = stubPreview({
    url: ARCHIVE_URL,
    size_bytes: 9_448_928_256,
    repo_tags: ["gpustack/runner:cuda13.0-vllm0.25.1"],
    previewable: true,
  });
  const matched: unknown[] = [];
  const prefilled: unknown[] = [];
  try {
    render(wrap(React.createElement(ImageArchiveURLImport, {
      versions: [catalogVersion()],
      onMatched: (version: unknown) => matched.push(version),
      onPrefill: (preview: unknown) => prefilled.push(preview),
    })));
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));

    await waitFor(() => assert.equal(matched.length, 1));
    assert.equal(prefilled.length, 0, "a matched reference must not also prefill a new row");
    assert.deepEqual(stub.calls, [ARCHIVE_URL]);
    // The operator sees the real tag and the real size, not the file name.
    await screen.findByText("gpustack/runner:cuda13.0-vllm0.25.1");
    await screen.findByText("8.8 GiB");
    await screen.findByText(/already in the catalog/);
  } finally {
    stub.restore();
  }
});

test("resolving an archive with no catalog match prefills a new image row", async () => {
  const stub = stubPreview({
    url: ARCHIVE_URL,
    size_bytes: 1024 * 1024,
    repo_tags: ["vendor/other:v9", "vendor/other:latest"],
    previewable: true,
  });
  const matched: unknown[] = [];
  const prefilled: Array<{ url: string; repo_tags: string[] }> = [];
  try {
    render(wrap(React.createElement(ImageArchiveURLImport, {
      versions: [catalogVersion()],
      onMatched: (version: unknown) => matched.push(version),
      onPrefill: (preview: never) => prefilled.push(preview),
    })));
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));

    await waitFor(() => assert.equal(prefilled.length, 1));
    assert.equal(matched.length, 0);
    assert.equal(prefilled[0].url, ARCHIVE_URL);
    assert.equal(prefilled[0].repo_tags[0], "vendor/other:v9");
    // Both references the archive carries are shown, and the first one is the
    // reference the form gets.
    await screen.findByText("vendor/other:v9");
    await screen.findByText("vendor/other:latest");
    await screen.findByText(/filled in with vendor\/other:v9/);
  } finally {
    stub.restore();
  }
});

test("an archive that cannot be inspected shows its size and asks for the reference", async () => {
  const stub = stubPreview({ url: ARCHIVE_URL, size_bytes: 2048, repo_tags: [], previewable: false });
  const matched: unknown[] = [];
  const prefilled: unknown[] = [];
  try {
    render(wrap(React.createElement(ImageArchiveURLImport, {
      versions: [catalogVersion()],
      onMatched: (version: unknown) => matched.push(version),
      onPrefill: (preview: unknown) => prefilled.push(preview),
    })));
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));

    // Not previewable is not an error: the size is still reported, and nothing
    // is matched or prefilled behind the operator's back.
    await screen.findByText("2 KiB");
    await screen.findByText(imagesLocale.import.url.tagsUnknown);
    await screen.findByText(/cannot be inspected before it is downloaded/);
    assert.equal(matched.length, 0);
    assert.equal(prefilled.length, 0);
    assert.equal(screen.queryByText(/Error/), null);
  } finally {
    stub.restore();
  }
});

test("a failed resolve shows a safe generic message and resolves nothing", async () => {
  const stub = stubPreview(null, new Error("The image archive at containerimages.example could not be read."));
  const matched: unknown[] = [];
  try {
    render(wrap(React.createElement(ImageArchiveURLImport, {
      versions: [catalogVersion()],
      onMatched: (version: unknown) => matched.push(version),
      onPrefill: () => {},
    })));
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));

    await screen.findByText(imagesLocale.import.url.failed);
    assert.equal(matched.length, 0);
  } finally {
    stub.restore();
  }
});

test("a rejected archive preview retains the HTTP status and stable diagnostic code", async () => {
  const { APIError } = await import("../src/apiError.ts");
  const error = new APIError("IMAGE_ARCHIVE_URL_UNREACHABLE", {
    host: "archive.example",
    detail: "HEAD https://archive.example/image.tar?token=private-query: 503 Service Unavailable",
  }, "The image archive at archive.example could not be read.", 502);
  const stub = stubPreview(null, error);
  try {
    render(wrap(React.createElement(ImageArchiveURLImport, {
      versions: [], onMatched: () => {}, onPrefill: () => {},
    })));
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));
    await screen.findByText("HTTP 502 · IMAGE_ARCHIVE_URL_UNREACHABLE");
    await screen.findByText("The image archive at archive.example could not be read.");
    assert.equal(screen.queryByText(/private-query/) !== null, false,
      "raw upstream URLs can carry query credentials and must not be rendered");
    assert.equal((screen.getByLabelText(imagesLocale.import.url.label) as HTMLInputElement).value, ARCHIVE_URL);
  } finally {
    stub.restore();
  }
});

test("resolving with an empty address asks for one without calling the server", async () => {
  const stub = stubPreview({ url: "", size_bytes: 0, repo_tags: [], previewable: false });
  try {
    render(wrap(React.createElement(ImageArchiveURLImport, {
      versions: [],
      onMatched: () => {},
      onPrefill: () => {},
    })));
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));
    await screen.findByText(imagesLocale.import.url.required);
    assert.deepEqual(stub.calls, []);
  } finally {
    stub.restore();
  }
});

test("an archive URL becomes one acquisition option labelled with its host", () => {
  const t = (key: string) =>
    key === "nodeTable.sourceFromURL" ? imagesLocale.nodeTable.sourceFromURL : key === "nodeTable.sourceAuto" ? imagesLocale.nodeTable.sourceAuto : key;
  const options = imageSourceOptions(["quay.io/gpustack/runner:v1", ARCHIVE_URL], t as never);
  assert.equal(options[0].value, autoSourceValue, "the automatic pull comes first and is the default");
  assert.equal(options[0].label, imagesLocale.nodeTable.sourceAuto);
  // No registry host is ever an option: the registry reference declared on the
  // image is ignored, and only auto, the archive and the dialog action remain.
  assert.deepEqual(options.map((option) => option.value), [autoSourceValue, ARCHIVE_URL, archiveURLDialogValue]);
  const archive = options.find((option) => option.value === ARCHIVE_URL);
  assert.ok(archive, "the declared address gets an option of its own");
  assert.equal(archive.value, ARCHIVE_URL);
  assert.equal(archive.label, "containerimages.example");
  assert.equal(archive.title, ARCHIVE_URL, "the full address is the tooltip");
  assert.equal(archive.archiveUrl, ARCHIVE_URL);
  for (const option of options.filter((item) => item.value !== ARCHIVE_URL)) {
    assert.equal(isArchiveURL(option.value), false, "only the archive option may carry a URL");
  }
  // The last option is not an address at all: it is the action that asks for
  // one, so nothing downstream can treat it as something to pull.
  const action = options[options.length - 1];
  assert.equal(action.value, archiveURLDialogValue);
  assert.equal(action.opensDialog, true);
  assert.equal(action.label, imagesLocale.nodeTable.sourceFromURL);
  assert.equal(isArchiveURL(action.value), false);
  assert.equal(archiveURLHost(ARCHIVE_URL), "containerimages.example");
});

test("the node table fetches a declared archive through the dialog, relay included", async () => {
  const stub = stubPreview({
    url: ARCHIVE_URL,
    size_bytes: 1024 * 1024,
    repo_tags: ["gpustack/runner:v1"],
    previewable: true,
  });
  const version = catalogVersion({
    image_ref: "gpustack/runner:v1",
    registry_sources: [ARCHIVE_URL],
    nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "absent" }],
  }) as never as Record<string, unknown>;
  let mode = ARCHIVE_URL;
  const pulls: Array<{ url: string; relay: boolean }> = [];
  const retries: Array<{ retry?: boolean }> = [];
  try {
    render(wrap(React.createElement(ImageNodeTable, {
      selectedVersion: version as never,
      inspecting: null,
      availablePeer: () => false,
      modeFor: () => mode,
      setMode: (_v: never, _uuid: string, next: string) => { mode = next; },
      pullVersion: (_v: never, _uuid: string, retry?: boolean) => retries.push({ retry }),
      pullFromURL: async (_v: never, _uuid: string, url: string, relay: boolean) => { pulls.push({ url, relay }); },
      inspectVersion: () => {},
      cancelTransfer: () => {},
      removeFromNode: () => {},
    })));

    const user = userEvent.setup({ document: dom.window.document });
    // The row button does not transfer anything by itself: a URL is fetched
    // through the dialog, which resolves the address first.
    await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.pullFromURL }));
    assert.deepEqual(pulls, [], "opening the dialog must not start a transfer");
    assert.deepEqual(retries, []);
    await screen.findByText("1.0 MiB");
    assert.deepEqual(stub.calls, [ARCHIVE_URL], "the declared address is resolved on open");

    // The relay choice lives in the dialog, and starts cleared: the node
    // downloads the archive itself unless the operator says otherwise.
    const relay = screen.getByRole("checkbox", { name: imagesLocale.nodeTable.relay }) as HTMLInputElement;
    assert.equal(relay.checked, false);
    await user.click(relay);
    await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.pull }));
    await waitFor(() => assert.deepEqual(pulls, [{ url: ARCHIVE_URL, relay: true }]));
  } finally {
    stub.restore();
  }
});

test("an archive carrying another image is refused in the dialog", async () => {
  const stub = stubPreview({
    url: ARCHIVE_URL,
    size_bytes: 2048,
    repo_tags: ["vendor/other:v9"],
    previewable: true,
  });
  const version = catalogVersion({
    image_ref: "gpustack/runner:cann8.5-910b-vllm0.18.0",
    registry_sources: [],
    nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "absent" }],
  }) as never as Record<string, unknown>;
  const pulls: Array<{ url: string }> = [];
  try {
    render(wrap(React.createElement(ImageNodeTable, {
      selectedVersion: version as never,
      inspecting: null,
      availablePeer: () => false,
      modeFor: () => archiveURLDialogValue,
      setMode: () => {},
      pullVersion: () => {},
      pullFromURL: async (_v: never, _uuid: string, url: string) => { pulls.push({ url }); },
      inspectVersion: () => {},
      cancelTransfer: () => {},
      removeFromNode: () => {},
    })));

    const user = userEvent.setup({ document: dom.window.document });
    await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.pullFromURL }));
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));

    await screen.findByText(imagesLocale.nodeTable.archiveDialog.mismatch);
    assert.ok(screen.getAllByText(/vendor\/other:v9/).length > 0, "the archive's real reference is shown");
    const pull = screen.getByRole("button", { name: imagesLocale.nodeTable.pull }) as HTMLButtonElement;
    assert.equal(pull.disabled, true, "a mismatched archive must not be pullable");
    await user.click(pull);
    assert.deepEqual(pulls, []);
  } finally {
    stub.restore();
  }
});

test("an unverifiable archive still pulls, with the reason on screen", async () => {
  const stub = stubPreview({ url: ARCHIVE_URL, size_bytes: 2048, repo_tags: [], previewable: false });
  const version = catalogVersion({
    image_ref: "gpustack/runner:cann8.5-910b-vllm0.18.0",
    nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "absent" }],
  }) as never as Record<string, unknown>;
  const pulls: Array<{ url: string; relay: boolean }> = [];
  try {
    render(wrap(React.createElement(ImageNodeTable, {
      selectedVersion: version as never,
      inspecting: null,
      availablePeer: () => false,
      modeFor: () => archiveURLDialogValue,
      setMode: () => {},
      pullVersion: () => {},
      pullFromURL: async (_v: never, _uuid: string, url: string, relay: boolean) => { pulls.push({ url, relay }); },
      inspectVersion: () => {},
      cancelTransfer: () => {},
      removeFromNode: () => {},
    })));

    const user = userEvent.setup({ document: dom.window.document });
    await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.pullFromURL }));
    await user.type(screen.getByLabelText(imagesLocale.import.url.label), ARCHIVE_URL);
    await user.click(screen.getByRole("button", { name: imagesLocale.import.url.resolve }));

    await screen.findByText(imagesLocale.nodeTable.archiveDialog.unverified);
    await user.click(screen.getByRole("button", { name: imagesLocale.nodeTable.pull }));
    await waitFor(() => assert.deepEqual(pulls, [{ url: ARCHIVE_URL, relay: false }]));
  } finally {
    stub.restore();
  }
});

test("a present node acquired from a URL is labelled URL, and a relayed one URL · relayed", async () => {
  const version = catalogVersion({
    registry_sources: [ARCHIVE_URL],
    nodes: [
      { node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "present", source: "url" },
      { node_uuid: "node-2", node_name: "gpu-2", node_state: "online", state: "present", source: "url-relay" },
    ],
  }) as never as Record<string, unknown>;
  render(wrap(React.createElement(ImageNodeTable, {
    selectedVersion: version as never,
    inspecting: null,
    availablePeer: () => false,
    modeFor: () => ARCHIVE_URL,
    setMode: () => {},
    pullVersion: () => {},
    pullFromURL: async () => {},
    cancelTransfer: () => {},
    removeFromNode: () => {},
    inspectVersion: () => {},
  })));
  await screen.findByText(imagesLocale.nodeTable.sourceURL);
  await screen.findByText(imagesLocale.nodeTable.sourceURLRelay);
  // A row that already holds the image shows no picker, no relay box, and no
  // way to start a URL fetch.
  assert.equal(screen.queryByRole("checkbox", { name: imagesLocale.nodeTable.relay }), null);
  assert.equal(screen.queryByRole("button", { name: imagesLocale.nodeTable.pullFromURL }), null);
  });

test("the sync request body names the url source only for an archive address", () => {
  assert.deepEqual(apiModule.imageFetchBody(undefined, undefined), {});
  // A registry can no longer be named: whatever a stale caller passes, the
  // body stays empty and the platform decides.
  assert.deepEqual(apiModule.imageFetchBody("quay"), {});
  assert.deepEqual(apiModule.imageFetchBody("huaweicloud"), {});
  assert.deepEqual(apiModule.imageFetchBody("direct"), {});
  assert.deepEqual(apiModule.imageFetchBody("url", { archive_url: ARCHIVE_URL, relay: false }), {
    source: "url",
    archive_url: ARCHIVE_URL,
    relay: false,
  });
  assert.deepEqual(apiModule.imageFetchBody("url", { archive_url: ARCHIVE_URL, relay: true }), {
    source: "url",
    archive_url: ARCHIVE_URL,
    relay: true,
  });
  // A forced re-pull names no source — the server picks it — so the body is
  // exactly {force:true}; without force the historical shapes above stay byte
  // for byte.
  assert.deepEqual(apiModule.imageFetchBody(undefined, { force: true }), { force: true });
  assert.equal(JSON.stringify(apiModule.imageFetchBody(undefined, { force: true })), '{"force":true}');
  assert.deepEqual(apiModule.imageFetchBody("quay", { force: false }), {});
});

// A transfer that looks stuck has to be stoppable from the row it is stuck on.
test("a running transfer offers a cancel, and a finished one does not", async () => {
  const version = catalogVersion({
    registry_sources: [ARCHIVE_URL],
    nodes: [
      { node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "syncing", source: "url" },
      { node_uuid: "node-2", node_name: "gpu-2", node_state: "online", state: "present", source: "url" },
    ],
  }) as never as Record<string, unknown>;
  const cancelled: Array<{ node: string }> = [];
  render(wrap(React.createElement(ImageNodeTable, {
    selectedVersion: version as never,
    inspecting: null,
    availablePeer: () => false,
    modeFor: () => ARCHIVE_URL,
    setMode: () => {},
    pullVersion: () => {},
    pullFromURL: async () => {},
    inspectVersion: () => {},
    cancelTransfer: (_v: never, nodeUUID: string) => { cancelled.push({ node: nodeUUID }); },
    removeFromNode: () => {},
  })));

  const user = userEvent.setup({ document: dom.window.document });
  const buttons = await screen.findAllByRole("button", { name: imagesLocale.nodeTable.cancel });
  assert.equal(buttons.length, 1, "only the transfer that is running can be cancelled");
  await user.click(buttons[0]);
  assert.deepEqual(cancelled, [{ node: "node-1" }]);
});

// After a failure the button names what the next click does, not that something
// failed: fetching from a URL opens the dialog again, so it stays the URL action.
test("a failed URL row keeps the URL action instead of offering a retry label", async () => {
  const version = catalogVersion({
    registry_sources: [ARCHIVE_URL],
    nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "failed", source: "url", message: "cancelled" }],
  }) as never as Record<string, unknown>;
  render(wrap(React.createElement(ImageNodeTable, {
    selectedVersion: version as never,
    inspecting: null,
    availablePeer: () => false,
    modeFor: () => ARCHIVE_URL,
    setMode: () => {},
    pullVersion: () => {},
    pullFromURL: async () => {},
    inspectVersion: () => {},
    cancelTransfer: () => {},
    removeFromNode: () => {},
  })));
  await screen.findByRole("button", { name: imagesLocale.nodeTable.pullFromURL });
  assert.equal(
    screen.queryByRole("button", { name: imagesLocale.common.retry }),
    null,
    "a URL fetch never says retry: it opens the dialog again",
  );
});

for (const state of ["present", "absent", "failed"]) {
  test(`a ${state} row has no transfer progress or stale byte counts`, async () => {
    const version = catalogVersion({ nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state,
      size_bytes: 0, transferred_bytes: 521_666_560, rate_bps: 1_048_576 }] });
    render(wrap(React.createElement(ImageNodeTable, {
      selectedVersion: version, inspecting: null, availablePeer: () => false,
      modeFor: () => "auto", setMode: () => {}, pullVersion: () => {},
      pullFromURL: async () => {}, inspectVersion: () => {}, cancelTransfer: () => {}, removeFromNode: () => {},
    })));
    assert.ok(!screen.queryByRole("progressbar"), "an idle row must not show transfer progress");
    assert.ok(!screen.queryByText(/0 B \/ 0 B|497\.5 MiB|1\.0 MiB\/s/), "an idle row must not show stale transfer bytes or speed");
  });
}

for (const state of ["pulling", "syncing"]) {
  for (const total of [undefined, 0, 1_048_576]) {
    test(`a ${state} row retains live progress with ${total === undefined ? "missing" : total ? "known" : "zero"} total`, async () => {
      const version = catalogVersion({ size_bytes: 39_727_986_688, nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state,
        size_bytes: total, transferred_bytes: 524_288, rate_bps: 1_048_576 }] });
      render(wrap(React.createElement(ImageNodeTable, {
        selectedVersion: version, inspecting: null, availablePeer: () => false,
        modeFor: () => "auto", setMode: () => {}, pullVersion: () => {},
        pullFromURL: async () => {}, inspectVersion: () => {}, cancelTransfer: () => {}, removeFromNode: () => {},
      })));
      if (total) {
        assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "50");
      } else {
        assert.ok(!screen.queryByRole("progressbar"), "unknown total must not claim zero percent complete");
        assert.ok(!screen.queryByText("0%"));
      }
      assert.ok(screen.getByText(total ? "512 KiB / 1.0 MiB · 1.0 MiB/s" : "512 KiB · 1.0 MiB/s"));
    });
  }
}

test("polling speeds up only while something is moving", () => {
  const idle = catalogVersion({ nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "present" }] }) as never;
  const moving = catalogVersion({ nodes: [{ node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "syncing" }] }) as never;
  assert.equal(transferRunning([idle]), false);
  assert.equal(transferRunning([moving]), true);
  assert.equal(transferRunning([]), false);
  assert.equal(transferRunning(undefined), false);
});

// Deleting an image is a separate action from fetching one: a row that holds
// the image offers it, a row that does not offers nothing to delete, and the
// click is confirmed before anything reaches a node.
for (const language of ["en", "zh"] as const) {
  test(`node image deletion names its target and impact before cancel or confirm (${language})`, async () => {
    await i18n.changeLanguage(language);
    const locale = language === "en" ? imagesLocale : imagesZhLocale;
    const version = catalogVersion({
      registry_sources: [ARCHIVE_URL],
      nodes: [
        { node_uuid: "node-1", node_name: "gpu-1", node_state: "online", state: "present", source: "url" },
        { node_uuid: "node-2", node_name: "gpu-2", node_state: "online", state: "absent" },
      ],
    }) as never as Record<string, unknown>;
    const removed: Array<{ node: string }> = [];
    render(wrap(React.createElement(ImageNodeTable, {
      selectedVersion: version as never,
      inspecting: null,
      availablePeer: () => false,
      modeFor: () => ARCHIVE_URL,
      setMode: () => {},
      pullVersion: () => {},
      pullFromURL: async () => {},
      inspectVersion: () => {},
      cancelTransfer: () => {},
      removeFromNode: (_v: never, nodeUUID: string) => { removed.push({ node: nodeUUID }); },
    })));

    const user = userEvent.setup({ document: dom.window.document });
    // Deleting a held image is an action of the row's expanded detail.
    for (const expand of await screen.findAllByRole("button", { name: locale.nodeTable.expand })) await user.click(expand);
    const buttons = await screen.findAllByRole("button", { name: locale.nodeTable.remove });
    assert.equal(buttons.length, 1, "only the node that holds the image can delete it");
    await user.click(buttons[0]);
    await screen.findByText(language === "en"
      ? "Delete “gpustack/runner:cuda13.0-vllm0.25.1” from node “gpu-1”?"
      : "从节点“gpu-1”删除镜像“gpustack/runner:cuda13.0-vllm0.25.1”？");
    await screen.findByText(language === "en"
      ? "Only this node's local copy will be removed. The catalog version and copies on other nodes will remain."
      : "仅删除该节点上的本地镜像副本，保留镜像目录中的版本及其他节点上的副本。");
    // Nothing happens until the confirmation is accepted: deleting an image from a
    // node is not undoable from the UI.
    assert.deepEqual(removed, []);
    await user.click(screen.getByRole("button", { name: language === "zh" ? /^取\s*消$/ : locale.common.cancel }));
    assert.deepEqual(removed, [], "cancelling must not remove any node image");
    await user.click(buttons[0]);
    await user.click(await screen.findByRole("button", { name: language === "zh" ? /^删\s*除$/ : locale.nodeTable.removeConfirmOk, exact: true }));
    await waitFor(() => assert.deepEqual(removed, [{ node: "node-1" }]));
    await i18n.changeLanguage("en");
  });
}
