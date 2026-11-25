import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
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
  resources: { en: { settings: settingsLocale, common: commonLocale } },
  defaultNS: "settings",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const ServerListenCard = (await import("../src/pages/settings/ServerListenCard.tsx")).default;

afterEach(() => cleanup());
after(() => closeChannels());

const l = settingsLocale.listen;

function listenFixture(overrides: Partial<import("../src/api.ts").ServerListen> = {}): import("../src/api.ts").ServerListen {
  return {
    console: ["192.0.2.156:8800"], agent: [], gateway: [],
    running: [{ addr: "192.0.2.156:8800", functions: ["console", "agent", "gateway"] }, { addr: "127.0.0.1:8800", functions: ["console", "agent", "gateway"] }],
    restart_pending: false, config_path: "/opt/gpuworks/server/server.yaml", editable: true, overridden: false,
    restart_mode: "self",
    ...overrides,
  };
}

function stub(initial: import("../src/api.ts").ServerListen | Error) {
  const original = { serverListen: apiModule.api.serverListen, updateServerListen: apiModule.api.updateServerListen, restartServer: apiModule.api.restartServer };
  const updates: unknown[] = [];
  let restarts = 0;
  let saveError: Error | null = null;
  apiModule.api.serverListen = async () => {
    if (initial instanceof Error) throw initial;
    return initial;
  };
  apiModule.api.updateServerListen = async (body) => {
    updates.push(body);
    if (saveError) throw saveError;
    return listenFixture({ ...body, restart_pending: true });
  };
  apiModule.api.restartServer = async () => {
    restarts++;
    return { status: "restarting" };
  };
  return {
    updates,
    restarts: () => restarts,
    failSaveWith: (err: Error) => { saveError = err; },
    restore: () => Object.assign(apiModule.api, original),
  };
}

function renderCard() {
  return render(<I18nextProvider i18n={i18n}><ServerListenCard /></I18nextProvider>);
}

test("shows the console address by default and what the server is bound to", async () => {
  const s = stub(listenFixture());
  try {
    renderCard();
    const input = await screen.findByLabelText(l.console);
    await waitFor(() => assert.equal((input as HTMLInputElement).value, "192.0.2.156:8800"));
    // Advanced addresses stay folded away.
    assert.equal(screen.queryByLabelText(l.gateway), null);
    assert.ok(screen.getByText("127.0.0.1:8800"));
    assert.equal(screen.getAllByText(l.function.gateway).length, 2);
    assert.equal(screen.queryByText(l.pending), null);
  } finally {
    s.restore();
  }
});

test("saves the addresses of every function and offers a restart", async () => {
  const s = stub(listenFixture());
  try {
    renderCard();
    const input = await screen.findByLabelText(l.console);
    await waitFor(() => assert.equal((input as HTMLInputElement).value, "192.0.2.156:8800"));
    await userEvent.click(screen.getByText(l.advanced));
    await userEvent.type(await screen.findByLabelText(l.gateway), "10.20.0.5:8800, 10.20.0.5:443");
    await userEvent.click(screen.getByRole("button", { name: settingsLocale.common.save }));
    await waitFor(() => assert.equal(s.updates.length, 1));
    assert.deepEqual(s.updates[0], { console: ["192.0.2.156:8800"], agent: [], gateway: ["10.20.0.5:8800", "10.20.0.5:443"] });
    await waitFor(() => assert.ok(screen.getByText(l.pending)));
    assert.ok(screen.getByRole("button", { name: l.restartNow }));
  } finally {
    s.restore();
  }
});

test("requires a console address and shows a rejected save in the reader's words", async () => {
  const s = stub(listenFixture());
  try {
    renderCard();
    const input = await screen.findByLabelText(l.console);
    await waitFor(() => assert.equal((input as HTMLInputElement).value, "192.0.2.156:8800"));
    await userEvent.clear(input);
    await userEvent.click(screen.getByRole("button", { name: settingsLocale.common.save }));
    await waitFor(() => assert.ok(screen.getByText(l.consoleRequired)));
    assert.equal(s.updates.length, 0);

    s.failSaveWith(new APIError("SERVER_LISTEN_LOCKOUT", { address: "192.0.2.156:8800" }, "lockout", 400));
    await userEvent.type(input, "10.20.0.5:8800");
    await userEvent.click(screen.getByRole("button", { name: settingsLocale.common.save }));
    await waitFor(() => assert.ok(screen.getByText(/You are connected through 192\.0\.2\.156:8800/)));
  } finally {
    s.restore();
  }
});

test("a pending change without a service manager shows the command to run", async () => {
  const s = stub(listenFixture({ restart_pending: true, restart_mode: "command", restart_command: "gpuworks restart --dir /opt/gpuworks/server" }));
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText(/Run on its host: gpuworks restart --dir \/opt\/gpuworks\/server/)));
    assert.equal(screen.queryByRole("button", { name: l.restartNow }), null);
  } finally {
    s.restore();
  }
});

test("an overridden or file-less server is read-only and a load failure is shown", async () => {
  const s = stub(listenFixture({ editable: false, overridden: true }));
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText(l.overridden)));
    assert.ok((screen.getByRole("button", { name: settingsLocale.common.save }) as HTMLButtonElement).disabled);
  } finally {
    s.restore();
  }
  cleanup();
  const failing = stub(new Error("listen unavailable"));
  try {
    renderCard();
    await waitFor(() => assert.ok(screen.getByText("listen unavailable")));
  } finally {
    failing.restore();
  }
});

test("restart now asks the server to restart and reloads once it answers again", async () => {
  const s = stub(listenFixture({ restart_pending: true }));
  const originalFetch = globalThis.fetch;
  let probes = 0;
  globalThis.fetch = (async () => { probes++; return new Response("{}", { status: 200 }); }) as typeof fetch;
  try {
    renderCard();
    const button = await screen.findByRole("button", { name: l.restartNow });
    button.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => assert.equal(s.restarts(), 1));
    await waitFor(() => assert.ok(probes >= 1));
  } finally {
    globalThis.fetch = originalFetch;
    s.restore();
  }
});
