import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "HTMLBodyElement", "HTMLInputElement", "getComputedStyle"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
const closeChannels = trackMessageChannels();

const settingsLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/settings.json", import.meta.url), "utf8"));
const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { nodes: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8")), settings: settingsLocale, common: commonLocale } },
  defaultNS: "settings",
  interpolation: { escapeValue: false },
});

const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const UpdatesCard = (await import("../src/pages/settings/UpdatesCard.tsx")).default;
type UpdateStatus = import("../src/api.ts").UpdateStatus;

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  Reflect.deleteProperty(dom.window.performance, "getEntriesByType");
});
after(() => closeChannels());

const u = settingsLocale.updates;

function statusFixture(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    channel: "nightly", channel_editable: true, auto_check: true, checked_at: "2026-10-11T02:00:00Z", available: true,
    source: "https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com",
    sources: [
      { url: "https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com", ok: true, latency_ms: 60 },
      { url: "https://dl.gpuworks.ai", ok: false, latency_ms: 15000, error: "context deadline exceeded" },
    ],
    server: { current: "f62e36c0", latest: "f90e474e", built_at: "2026-10-11T01:03:15+08:00", update_available: true, installable: true },
    image_catalog: { current: "2026-10-09.f085e40b", latest: "2026-10-09.f085e40b", update_available: false, compatible: true },
    template_catalog: { current: "2026-09-14.24bd7470", latest: "2026-10-11.aabbccdd", update_available: true, compatible: true },
    ...overrides,
  };
}

function stub(answers: Array<UpdateStatus | Error>) {
  const original = {
    agentFleetSummary: apiModule.api.agentFleetSummary, updates: apiModule.api.updates, syncImages: apiModule.api.syncImages, syncInferenceTemplates: apiModule.api.syncInferenceTemplates,
    uploadOfficialInferenceCatalog: apiModule.api.uploadOfficialInferenceCatalog, importImageCatalogFile: apiModule.api.importImageCatalogFile,
    setUpdateChannel: apiModule.api.setUpdateChannel, setUpdateAutoCheck: apiModule.api.setUpdateAutoCheck,
  };
  const calls: string[] = [];
  const refreshes: boolean[] = [];
  apiModule.api.agentFleetSummary = async () => ({target: "v26.11.2", align: 0, check: 0});
  let checks = 0;
  apiModule.api.setUpdateChannel = async (channel: string) => { calls.push(`channel:${channel}`); return { channel }; };
  apiModule.api.setUpdateAutoCheck = async (enabled: boolean) => { calls.push(`autoCheck:${enabled}`); return { enabled }; };
  apiModule.api.updates = async (refresh = false) => {
    refreshes.push(refresh);
    const answer = answers[Math.min(checks, answers.length - 1)];
    checks++;
    if (answer instanceof Error) throw answer;
    return answer;
  };
  apiModule.api.syncImages = async () => { calls.push("syncImages"); return { catalog_version: "2026-10-12.11111111", created: 0, updated: 1, deleted: 0, deprecated: 0, skipped: [], series: [] }; };
  apiModule.api.syncInferenceTemplates = async () => { calls.push("syncTemplates"); return { models: 382, package_digest: "aabbccdd", version: "2026-10-11.aabbccdd" }; };
  apiModule.api.uploadOfficialInferenceCatalog = async (file: File) => { calls.push(`uploadTemplates:${file.name}`); return { models: 382, package_digest: "eeff", version: "2026-10-11.eeff0011" }; };
  apiModule.api.importImageCatalogFile = async (file: File) => { calls.push(`uploadImages:${file.name}`); return { catalog_version: "2026-10-12.22222222", created: 1, updated: 0, deleted: 0, deprecated: 0, skipped: [], series: [] }; };
  return { calls, refreshes, checks: () => checks, restore: () => Object.assign(apiModule.api, original) };
}

function renderCard() {
  return render(<I18nextProvider i18n={i18n}><UpdatesCard /></I18nextProvider>);
}

function row(name: string): HTMLElement {
  const cell = screen.getByText(name);
  const tr = cell.closest("tr");
  assert.ok(tr, `no table row for ${name}`);
  return tr as HTMLElement;
}

test("shows each item against the channel without naming the sources", async () => {
  const s = stub([statusFixture()]);
  try {
    const { container } = renderCard();
    await waitFor(() => assert.ok(screen.getByText("f90e474e")));
    assert.deepEqual(s.refreshes, [false], "opening the page reuses the server's last check");
    assert.ok(!/oss-cn-shanghai|dl\.gpuworks\.ai|context deadline/.test(container.textContent ?? ""), "the page does not list distribution sources");
    assert.ok(within(row(u.server)).getByText(u.available));
    assert.ok(within(row(u.server)).getByText("sudo gpuworks upgrade"));
    assert.ok(within(row(u.images)).getByText(u.upToDate));
    assert.ok((within(row(u.images)).getByRole("button", { name: u.apply }) as HTMLButtonElement).disabled);
    assert.ok(within(row(u.templates)).getByText(u.available));
    assert.ok(!(within(row(u.templates)).getByRole("button", { name: u.apply }) as HTMLButtonElement).disabled);
  } finally {
    s.restore();
  }
});

test("updating a catalog applies it and checks again", async () => {
  const s = stub([statusFixture(), statusFixture({ template_catalog: { current: "2026-10-11.aabbccdd", latest: "2026-10-11.aabbccdd", update_available: false, compatible: true } })]);
  try {
    renderCard();
    const apply = await waitFor(() => within(row(u.templates)).getByRole("button", { name: u.apply }));
    apply.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => assert.deepEqual(s.calls, ["syncTemplates"]));
    await waitFor(() => assert.equal(s.checks(), 2));
    await waitFor(() => assert.ok(within(row(u.templates)).getByText(u.upToDate)));
  } finally {
    s.restore();
  }
});

test("uploads go to the official endpoints even when no source answers", async () => {
  const s = stub([statusFixture({
    available: false, source: undefined,
    sources: [{ url: "https://dl.gpuworks.ai", ok: false, latency_ms: 0, error: "dial tcp: i/o timeout" }],
    server: { current: "f62e36c0", update_available: false, installable: false },
    image_catalog: { current: "2026-10-09.f085e40b", update_available: false, compatible: true },
    template_catalog: { current: "2026-09-14.24bd7470", update_available: false, compatible: true },
  })]);
  try {
    const { container } = renderCard();
    await waitFor(() => assert.ok(screen.getByText(u.unavailable)));
    assert.equal(within(row(u.templates)).getAllByText(u.unknown).length, 1);
    assert.ok((within(row(u.images)).getByRole("button", { name: u.apply }) as HTMLButtonElement).disabled);
    const inputs = container.querySelectorAll<HTMLInputElement>("input[type=file]");
    assert.equal(inputs.length, 2);
    await userEvent.upload(inputs[1], new File([new Uint8Array([0x1f, 0x8b])], "templates.tar.gz", { type: "application/gzip" }));
    await waitFor(() => assert.deepEqual(s.calls, ["uploadTemplates:templates.tar.gz"]));
    await userEvent.upload(inputs[0], new File(['{"schema_version":3}'], "manifest.json", { type: "application/json" }));
    await waitFor(() => assert.deepEqual(s.calls, ["uploadTemplates:templates.tar.gz", "uploadImages:manifest.json"]));
  } finally {
    s.restore();
  }
});

test("a catalog that needs a newer server cannot be applied", async () => {
  const s = stub([statusFixture({ template_catalog: { current: "2026-09-14.24bd7470", latest: "2026-12-01.99999999", update_available: true, compatible: false } })]);
  try {
    renderCard();
    await waitFor(() => assert.ok(within(row(u.templates)).getByText(u.needsServer)));
    assert.ok((within(row(u.templates)).getByRole("button", { name: u.apply }) as HTMLButtonElement).disabled);
  } finally {
    s.restore();
  }
});

test("a template download failure preserves server and image updates", async () => {
  const s = stub([statusFixture({
    server: { current: "old", latest: "new", update_available: true, installable: true, self_upgrade: true },
    image_catalog: { current: "old", latest: "new", update_available: true, compatible: true },
    template_catalog: {
      current: "old", latest: "new", update_available: true, compatible: false,
      check_error: { code: "UPDATE_DOWNLOAD_FAILED", params: { file: "templates.tar.gz", detail: "HTTP 404" }, message: "fallback" },
    },
  })]);
  try {
    renderCard();
    const expected = commonLocale.apiError.UPDATE_DOWNLOAD_FAILED.replace("{{file}}", "templates.tar.gz").replace("{{detail}}", "HTTP 404");
    await waitFor(() => assert.ok(within(row(u.templates)).getByText(expected)));
    assert.equal(within(row(u.templates)).queryByText(u.needsServer), null);
    assert.ok((within(row(u.templates)).getByRole("button", { name: u.apply }) as HTMLButtonElement).disabled);
    assert.equal((within(row(u.images)).getByRole("button", { name: u.apply }) as HTMLButtonElement).disabled, false);
    assert.equal((within(row(u.server)).getByRole("button", { name: u.upgrade }) as HTMLButtonElement).disabled, false);
  } finally {
    s.restore();
  }
});

test("a failed check is shown in the reader's words", async () => {
  const s = stub([new APIError("ADMINISTRATOR_REQUIRED", {}, "administrator role required", 403)]);
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText(commonLocale.apiError.ADMINISTRATOR_REQUIRED)));
  } finally {
    s.restore();
  }
});

function stubServerUpgrade(answers: Array<UpdateStatus>, healthVersions: string[]) {
  const s = stub(answers);
  const original = { upgradeServer: apiModule.api.upgradeServer, uploadServerArchive: apiModule.api.uploadServerArchive };
  const originalFetch = globalThis.fetch;
  apiModule.api.upgradeServer = async () => { s.calls.push("upgradeServer"); return { from: "f62e36c0", to: "f90e474e", backup: "/opt/gpuworks/server/backups/b.tar.gz" }; };
  apiModule.api.uploadServerArchive = async (file: File) => { s.calls.push(`uploadServer:${file.name}`); return { from: "f62e36c0", to: "f90e474e", backup: "/opt/gpuworks/server/backups/b.tar.gz" }; };
  let probe = 0;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    if (String(input) !== "/healthz") throw new Error(`unexpected fetch ${String(input)}`);
    const version = healthVersions[Math.min(probe, healthVersions.length - 1)];
    probe++;
    return new Response(JSON.stringify({ status: "ok", version }), { status: 200 });
  }) as typeof fetch;
  return {
    ...s,
    probes: () => probe,
    restore: () => { s.restore(); Object.assign(apiModule.api, original); globalThis.fetch = originalFetch; },
  };
}

const selfUpgradable = { current: "f62e36c0", latest: "f90e474e", built_at: "2026-10-11T01:03:15+08:00", update_available: true, installable: true, self_upgrade: true };

function requireServerUpgradeReady() {
  const button = within(row(u.server)).getByRole("button", { name: u.upgrade }) as HTMLButtonElement;
  assert.ok(!screen.queryByText(u.checking), "the update check has not finished");
  assert.ok(!button.closest(".ant-spin-blur"), "the update table is still loading");
  assert.equal(button.disabled, false);
  return button;
}

function readyServerUpgrade() {
  return waitFor(requireServerUpgradeReady);
}

test("the console upgrades the server after confirming and waits for the new version", async () => {
  const after = statusFixture({ server: { ...selfUpgradable, current: "f90e474e", update_available: false } });
  const s = stubServerUpgrade([statusFixture({ server: selfUpgradable }), after], ["f90e474e"]);
  try {
    apiModule.api.agentFleetSummary = async () => ({ target: "f90e474e", align: 3, upgrade: 0, downgrade: 1, different_build: 2, check: 1, offline: 1 });
    renderCard();
    const upgrade = await readyServerUpgrade();
    await userEvent.click(upgrade);
    const dialog = await screen.findByRole("dialog");
    assert.ok(within(dialog).getAllByText("Upgrade the server to f90e474e?").length > 0);
    assert.deepEqual(s.calls, [], "nothing happens before the confirmation");
    assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden");
    await userEvent.click(within(dialog).getByRole("button", { name: u.upgrade }));
    await waitFor(() => assert.deepEqual(s.calls, ["upgradeServer"]));
    await waitFor(() => assert.ok(s.probes() >= 1));
    await waitFor(() => assert.equal(s.checks(), 2));
    await waitFor(() => assert.ok(within(row(u.server)).getByText(u.upToDate)));
    const notice = (await screen.findByRole("link", { name: u.viewNodes })).closest("tr")!;
    assert.equal(notice.previousElementSibling, row(u.server));
    assert.equal(notice.nextElementSibling, row(u.images));
    assert.match(notice.textContent ?? "", /Different builds \(2\)/);
    assert.match(notice.textContent ?? "", /Pending alignment \(1\)/);
    assert.doesNotMatch(notice.textContent ?? "", /downgrade/i);
    assert.match(notice.textContent ?? "", /Currently unavailable for upgrade \(1\)/);
    assert.doesNotMatch(notice.textContent ?? "", /offline/i);
    assert.deepEqual(within(notice).getAllByRole("link").map(link => link.getAttribute("href")), ["/nodes"]);
    within(notice).getByRole("button", { name: "Close" }).focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden"));
  } finally {
    s.restore();
  }
});

test("a held update check keeps the upgrade behind the loading veil until its answer arrives", async () => {
  const status = statusFixture({ server: selfUpgradable });
  const s = stubServerUpgrade([status], ["f90e474e"]);
  let answer: (status: UpdateStatus) => void = () => {};
  try {
    renderCard();
    await readyServerUpgrade();
    apiModule.api.updates = (refresh = false) => {
      s.refreshes.push(refresh);
      return new Promise((resolve) => { answer = resolve; });
    };
    await userEvent.click(screen.getByRole("button", { name: new RegExp(u.check) }));
    await waitFor(() => assert.deepEqual(s.refreshes, [false, true]));
    assert.ok(screen.getByText(u.checking));
    const upgrade = within(row(u.server)).getByRole("button", { name: u.upgrade });
    assert.ok(upgrade.closest(".ant-spin-blur"), "the existing button is present but cannot receive a pointer");
    assert.deepEqual(s.calls, []);
    assert.throws(requireServerUpgradeReady, /the update check has not finished/);
    answer(status);
    const readyUpgrade = await readyServerUpgrade();
    assert.ok(!readyUpgrade.closest(".ant-spin-blur"));
    assert.notEqual(getComputedStyle(readyUpgrade).pointerEvents, "none");
    assert.deepEqual(s.calls, [], "checking alone does not start an upgrade");
  } finally {
    s.restore();
  }
});

test("an uploaded release goes to the server upload endpoint", async () => {
  const s = stubServerUpgrade([statusFixture({ available: false, server: { ...selfUpgradable, latest: undefined, update_available: false } })], ["f90e474e"]);
  try {
    apiModule.api.agentFleetSummary = async () => ({ target: "f90e474e", align: 0, check: 2 });
    const { container } = renderCard();
    await waitFor(() => assert.ok(within(row(u.server)).getByRole("button", { name: new RegExp(u.uploadServer) })));
    const inputs = container.querySelectorAll<HTMLInputElement>("input[type=file]");
    assert.equal(inputs.length, 3, "server, image catalog and template catalog each take an upload");
    await userEvent.upload(inputs[0], new File([new Uint8Array([0x1f, 0x8b])], "gpuworks-f90e474e-linux-amd64.tar.gz", { type: "application/gzip" }));
    await waitFor(() => assert.deepEqual(s.calls, ["uploadServer:gpuworks-f90e474e-linux-amd64.tar.gz"]));
    const notice = (await screen.findByRole("link", { name: u.viewNodes })).closest("tr")!;
    assert.match(notice.textContent ?? "", /Currently unavailable for upgrade \(2\)/);
    assert.deepEqual(within(notice).getAllByRole("link").map(link => link.getAttribute("href")), ["/nodes"]);
  } finally {
    s.restore();
  }
});

test("a server that cannot upgrade itself shows the command and why", async () => {
  const s = stub([statusFixture({ server: { ...selfUpgradable, self_upgrade: false, self_upgrade_blocker: "not_writable" } })]);
  try {
    renderCard();
    await waitFor(() => assert.ok(within(row(u.server)).getByText("sudo gpuworks upgrade")));
    assert.ok(within(row(u.server)).getByText(u.blocker.not_writable));
    assert.equal(within(row(u.server)).queryByRole("button", { name: u.upgrade }), null);
  } finally {
    s.restore();
  }
});

test("an installed server nothing would restart is told how to run it behind its supervisor", async () => {
  const s = stub([statusFixture({ server: { ...selfUpgradable, self_upgrade: false, self_upgrade_blocker: "no_service_manager", install_dir: "/opt/gpuworks/server" } })]);
  try {
    renderCard();
    const r = () => row(u.server);
    await waitFor(() => assert.ok(within(r()).getByText(u.blocker.no_service_manager)));
    assert.ok(within(r()).getByText("/opt/gpuworks/server/gpuworks restart --dir /opt/gpuworks/server"));
    assert.ok(within(r()).getByText("/opt/gpuworks/server/gpuworks serve --supervise --config /opt/gpuworks/server/server.yaml"));
    assert.equal(within(r()).queryByText("sudo gpuworks upgrade"), null, "the CLI upgrade is not what fixes it");
    assert.equal(within(r()).queryByRole("button", { name: u.upgrade }), null);
  } finally {
    s.restore();
  }
});

test("a server started without an installation keeps the CLI command", async () => {
  const s = stub([statusFixture({ server: { ...selfUpgradable, self_upgrade: false, self_upgrade_blocker: "no_service_manager" } })]);
  try {
    renderCard();
    await waitFor(() => assert.ok(within(row(u.server)).getByText("sudo gpuworks upgrade")));
    assert.ok(within(row(u.server)).getByText(u.blocker.no_service_manager));
  } finally {
    s.restore();
  }
});

test("switching the channel shows it at once, checks it, and says when it is empty", async () => {
  const release = statusFixture({ channel: "release", available: false, unavailable_reason: "unpublished", server: { current: "f62e36c0", update_available: false, installable: false, self_upgrade: false } });
  const s = stub([statusFixture()]);
  let answer: (status: UpdateStatus) => void = () => {};
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText(u.channelNightlyHelp)));
    // The check of the new channel is held until the test answers it.
    apiModule.api.updates = (refresh = false) => { s.refreshes.push(refresh); return new Promise((resolve) => { answer = resolve; }); };
    await userEvent.click(screen.getByRole("combobox"));
    await userEvent.click(await screen.findByRole("option", { name: u.channelRelease }));
    // Before any answer: the new channel, its help and "checking" are shown.
    await waitFor(() => assert.ok(screen.getByText(u.channelReleaseHelp)));
    assert.ok(screen.getByText(u.checking));
    assert.deepEqual(s.calls, ["channel:release"]);
    await waitFor(() => assert.deepEqual(s.refreshes, [false, true]));
    answer(release);
    await waitFor(() => assert.ok(screen.getByText(u.unpublished.replace("{{channel}}", u.channelRelease))));
    assert.ok(screen.getByText(u.unpublishedHelp));
    assert.equal(screen.queryByText(u.unavailable), null, "an empty channel is not a network problem");
    assert.equal(screen.queryByText(u.checking), null);
  } finally {
    s.restore();
  }
});

test("a channel that cannot be saved goes back to the one in use", async () => {
  const s = stub([statusFixture()]);
  apiModule.api.setUpdateChannel = async () => { throw new Error("server.yaml is read-only"); };
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText(u.channelNightlyHelp)));
    await userEvent.click(screen.getByRole("combobox"));
    await userEvent.click(await screen.findByRole("option", { name: u.channelRelease }));
    await waitFor(() => assert.ok(screen.getByText(u.channelNightlyHelp)));
    assert.equal(screen.queryByText(u.checking), null);
    assert.deepEqual(s.refreshes, [false], "nothing is checked for a channel that was not saved");
  } finally {
    s.restore();
  }
});

test("an unreadable channel says whether the network or the answer is at fault", async () => {
  for (const [reason, title, help] of [
    ["unreachable", u.unavailable, u.unavailableHelp],
    ["invalid", u.invalidIndex, u.invalidIndexHelp],
    [undefined, u.unavailable, u.unavailableHelp], // a server that does not name a reason
  ] as const) {
    const s = stub([statusFixture({ available: false, unavailable_reason: reason })]);
    try {
      const view = renderCard();
      await waitFor(() => assert.ok(screen.getByText(title)));
      assert.ok(screen.getByText(help));
      for (const other of [u.unavailable, u.invalidIndex]) {
        if (other !== title) assert.equal(screen.queryByText(other), null, `${String(reason)} also shows ${other}`);
      }
      view.unmount();
    } finally {
      s.restore();
    }
  }
});

test("the check button asks the channel again and a read-only channel cannot be switched", async () => {
  const s = stub([statusFixture({ channel_editable: false })]);
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText("f90e474e")));
    assert.ok((screen.getByRole("combobox", { name: u.channel }) as HTMLButtonElement).disabled, "the channel select is disabled");
    await userEvent.click(screen.getByRole("button", { name: new RegExp(u.check) }));
    await waitFor(() => assert.deepEqual(s.refreshes, [false, true]));
  } finally {
    s.restore();
  }
});

test("the automatic check switch says what is sent, saves, and checks again", async () => {
  const s = stub([statusFixture(), statusFixture({ auto_check: false, available: false, sources: [] })]);
  try {
    renderCard();
    const toggle = await waitFor(() => screen.getByRole("switch", { name: u.autoCheck }) as HTMLButtonElement);
    assert.equal(toggle.getAttribute("aria-checked"), "true");
    assert.ok(screen.getByText(u.autoCheckOnHelp), "while on, the page lists what the check tells the publisher");
    toggle.focus();
    await userEvent.keyboard(" ");
    await waitFor(() => assert.deepEqual(s.calls, ["autoCheck:false"]));
    await waitFor(() => assert.equal(s.checks(), 2));
    await waitFor(() => assert.ok(screen.getByText(u.autoCheckOffHelp)));
    assert.equal(screen.getByRole("switch", { name: u.autoCheck }).getAttribute("aria-checked"), "false");
    // Nothing has been checked: the page says so instead of calling the channel unreachable.
    assert.ok(screen.getByText(u.notChecked));
    assert.equal(screen.queryByText(u.unavailable), null);
    assert.equal(screen.queryByText(/^Checked /), null);
  } finally {
    s.restore();
  }
});

test("a switch that cannot be saved keeps its position and a server without server.yaml cannot flip it", async () => {
  const s = stub([statusFixture()]);
  const original = apiModule.api.setUpdateAutoCheck;
  try {
    apiModule.api.setUpdateAutoCheck = async () => { throw new APIError("UPDATE_AUTO_CHECK_READ_ONLY", {}, "the server was started without server.yaml", 409); };
    renderCard();
    const toggle = await waitFor(() => screen.getByRole("switch", { name: u.autoCheck }) as HTMLButtonElement);
    await userEvent.click(toggle);
    await waitFor(() => assert.equal(s.checks(), 1));
    assert.equal(screen.getByRole("switch", { name: u.autoCheck }).getAttribute("aria-checked"), "true");
  } finally {
    apiModule.api.setUpdateAutoCheck = original;
    s.restore();
  }
  cleanup();
  const readOnly = stub([statusFixture({ channel_editable: false })]);
  try {
    renderCard();
    const toggle = await waitFor(() => screen.getByRole("switch", { name: u.autoCheck }) as HTMLButtonElement);
    assert.ok(toggle.disabled);
  } finally {
    readOnly.restore();
  }
});

test("the running build keeps its own label when the selected update channel offers another release", async () => {
  const value = statusFixture();
  value.channel = "release";
  value.server.latest = "v99.0.0";
  value.server.built_at = "2099-01-01T00:00:00Z";
  value.server.current_build = { version: value.server.current, kind: "development", build_channel: "nightly", built_at: "2026-10-01T00:00:00Z" };
  const backend = stub([value]);
  try {
    renderCard();
    const summary = await screen.findByRole("region", { name: u.currentBuild });
    assert.ok(within(summary).getByText(commonLocale.buildInfo.kind.development));
    assert.ok(within(summary).getByText(value.server.current));
    assert.equal(within(summary).queryByText(/v99\.0\.0/), null);
    assert.equal(within(summary).queryByText(/2099/), null);
    assert.equal(within(summary).queryByText(commonLocale.buildInfo.kind.stable), null);
  } finally { backend.restore(); }
});

test("opening settings after a CLI upgrade never queries the fleet, even with an offline node", async () => {
  const s = stub([statusFixture()]);
  let requests = 0;
  try {
    apiModule.api.agentFleetSummary = async () => { requests++; return { target: "f90e474e", align: 1, check: 1, offline: 1 }; };
    renderCard();
    await screen.findByText(u.server);
    assert.equal(requests, 0);
    assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden");
  } finally { s.restore(); }
});

function reloadAttempt(deadline: number, phase: "restored" | "restarting" = "restored") {
  Object.defineProperty(dom.window.performance, "getEntriesByType", { configurable: true, value: () => [{ type: "reload", name: dom.window.location.href }] });
  dom.window.sessionStorage.setItem("gpuworks.console-upgrade", JSON.stringify({
    url: dom.window.location.href, session: null, attempt: { from: "old", to: "f90e474e", phase, deadline },
  }));
}

test("refresh preserves the deadline and cannot revive an expired follow-up", async () => {
  const s = stub([statusFixture()]);
  try {
    apiModule.api.agentFleetSummary = async () => ({ target: "f90e474e", align: 1, check: 0 });
    reloadAttempt(Date.now() + 900);
    const view = renderCard();
    await screen.findByRole("link", { name: u.viewNodes });
    await waitFor(() => assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden"), { timeout: 2500 });
    view.unmount();
    renderCard();
    await screen.findByText(u.server);
    assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden");
  } finally { s.restore(); }
});

test("offline Agents stay silent until a reconnect reports an actionable difference", async () => {
  const s = stub([statusFixture()]);
  let connected = false;
  let requests = 0;
  try {
    apiModule.api.agentFleetSummary = async () => {
      requests++;
      return { target: "f90e474e", align: Number(connected), upgrade: Number(connected), check: 0, offline: Number(!connected) };
    };
    reloadAttempt(Date.now() + 60000);
    renderCard();
    await waitFor(() => assert.ok(requests > 0));
    assert.equal(screen.queryByRole("link", { name: u.viewNodes }), null);
    connected = true;
    Object.defineProperty(document, "hidden", { configurable: true, value: false });
    await act(() => { document.dispatchEvent(new dom.window.Event("visibilitychange")); });
    await screen.findByRole("link", { name: u.viewNodes });
  } finally { Reflect.deleteProperty(document, "hidden"); s.restore(); }
});

test("a failed fleet read retries locally, and a confirmed empty result ends the notice", async () => {
  const s = stub([statusFixture()]);
  let calls = 0;
  try {
    apiModule.api.agentFleetSummary = async () => {
      calls++;
      if (calls === 1) throw new Error("temporarily unavailable");
      return { target: "f90e474e", align: 0, check: calls === 2 ? 1 : 0 };
    };
    reloadAttempt(Date.now() + 60000);
    renderCard();
    await screen.findByText(u.server);
    assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden");
    Object.defineProperty(document, "hidden", { configurable: true, value: false });
    const tick = () => act(() => { document.dispatchEvent(new dom.window.Event("visibilitychange")); });
    await tick();
    await screen.findByRole("link", { name: u.viewNodes });
    await tick();
    await waitFor(() => assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden"));
    await tick();
    assert.equal(calls, 3, "a new node after the terminal empty result belongs on the node list");
  } finally { Reflect.deleteProperty(document, "hidden"); s.restore(); }
});

test("a restart which returns the old release never opens the follow-up", async () => {
  const s = stubServerUpgrade([statusFixture()], ["old"]);
  let calls = 0;
  try {
    apiModule.api.agentFleetSummary = async () => { calls++; return { target: "old", align: 1, check: 0 }; };
    reloadAttempt(Date.now() + 650, "restarting");
    renderCard();
    await waitFor(() => assert.ok(s.probes() > 0));
    await waitFor(() => assert.ok(screen.getByText(u.serverNotBack.replace("{{version}}", "f90e474e"))), { timeout: 2500 });
    assert.equal(calls, 0);
    assert.ok(!screen.queryByRole("link", { name: u.viewNodes }), "the upgrade notice is hidden");
  } finally { s.restore(); }
});

test("logging in again cannot restore the previous session's upgrade", async () => {
  const s = stub([statusFixture()]);
  let requests = 0;
  try {
    reloadAttempt(Date.now() + 60000);
    dom.window.localStorage.setItem("gpuworks.session", JSON.stringify({ type: "authenticated", nonce: "next-login" }));
    apiModule.api.agentFleetSummary = async () => { requests++; return { target: "f90e474e", align: 1, check: 0 }; };
    renderCard();
    await screen.findByText(u.server);
    assert.equal(requests, 0);
    assert.ok(!screen.queryByRole("link", { name: u.viewNodes }));
  } finally { dom.window.localStorage.removeItem("gpuworks.session"); s.restore(); }
});
