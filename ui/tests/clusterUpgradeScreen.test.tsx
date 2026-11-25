import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { JSDOM } from "jsdom";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const locales = Object.fromEntries(["en", "zh"].map((lng) => [lng, JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/clusterUpgrade.json`, import.meta.url), "utf8"))]));
async function i18nFor(lng: string) {
  const i18n = createInstance();
  await i18n.use(initReactI18next).init({ lng, resources: { [lng]: { clusterUpgrade: locales[lng] } }, defaultNS: "clusterUpgrade", interpolation: { escapeValue: false } });
  return i18n;
}
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const { ConfigProvider } = await import("antd");
const { default: ClusterUpgradeBoundary } = await import("../src/components/ClusterUpgradeScreen.tsx");
const { notifyClusterUpgrade, resetClusterUpgradeForTest, CLUSTER_UPGRADE_PROGRESS_PATH } = await import("../src/clusterUpgrade.ts");
const { apiResponseError } = await import("../src/apiError.ts");
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); globalThis.fetch = originalFetch; resetClusterUpgradeForTest(); });

type Answer = { status: number; body?: unknown } | "network";
function serve(answers: Record<string, Answer[]>) {
  const calls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = String(input);
    calls.push(path);
    const queue = answers[path] ?? [];
    const next = queue.length > 1 ? queue.shift()! : queue[0];
    if (next === undefined || next === "network") throw new TypeError("network");
    return new Response(next.body === undefined ? "" : JSON.stringify(next.body), { status: next.status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return calls;
}
const doc = (state: string, businessOpen: boolean, applied = 247, known = 248, onTarget = 3) =>
  ({ state, business_open: businessOpen, schema: { applied, known }, members: { total: 3, on_target: onTarget, fresh: 3 } });

async function mount(lng = "en", reload = () => {}) {
  const i18n = await i18nFor(lng);
  return render(<I18nextProvider i18n={i18n}><ConfigProvider theme={{ token: { motion: false } }}>
    <ClusterUpgradeBoundary reload={reload}><button>Create deployment</button></ClusterUpgradeBoundary>
  </ConfigProvider></I18nextProvider>);
}

test("the console is shown untouched until an API answer carries the upgrade code", async () => {
  const calls = serve({});
  await mount();
  assert.ok(screen.getByRole("button", { name: "Create deployment" }));
  assert.equal(screen.queryByText(locales.en.title), null);
  // An ordinary 503 and another product code do not switch the console.
  await apiResponseError(new Response("member business service is not ready\n", { status: 503 }), "");
  await apiResponseError(new Response(JSON.stringify({ code: "SERVICE_UNAVAILABLE", error: "x" }), { status: 503 }), "");
  assert.ok(screen.getByRole("button", { name: "Create deployment" }));
  assert.deepEqual(calls, []);
});

test("a 503 with the upgrade code switches to the upgrade screen, which shows the server's state and counts", async () => {
  serve({ [CLUSTER_UPGRADE_PROGRESS_PATH]: [{ status: 200, body: doc("window", false) }] });
  await mount();
  await act(async () => {
    await apiResponseError(new Response(JSON.stringify({ code: "CLUSTER_UPGRADE_IN_PROGRESS", error: "the control-plane cluster is being upgraded" }), { status: 503 }), "");
  });
  const en = locales.en;
  assert.ok(await screen.findByRole("heading", { name: en.title }));
  assert.equal(screen.queryByRole("button", { name: "Create deployment" }), null);
  assert.ok(await screen.findByText(en.states.window));
  assert.ok(screen.getByText("247 / 248"));
  assert.ok(screen.getByText("3 / 3"));
  assert.ok(screen.getByRole("status"));
});

test("the screen follows the states, survives unreadable progress, and reloads once a member serves again", async () => {
  let reloads = 0;
  const calls = serve({ [CLUSTER_UPGRADE_PROGRESS_PATH]: [
    { status: 200, body: doc("waiting", false, 247, 248, 2) },
    "network",
    { status: 503 },
    { status: 200, body: doc("migrating", false) },
    { status: 200, body: doc("idle", false, 248, 248) },
    { status: 200, body: doc("idle", true, 248, 248) },
  ] });
  notifyClusterUpgrade();
  await mount("zh", () => { reloads++; });
  const zh = locales.zh;
  assert.ok(await screen.findByRole("heading", { name: zh.title }));
  assert.ok(await screen.findByText(zh.states.waiting));
  assert.ok(screen.getByText("2 / 3"));
  // Unreadable progress keeps the last counts and says so; nothing reloads.
  await waitFor(() => assert.ok(screen.getByText(zh.states.unobserved)));
  assert.ok(screen.getByText("2 / 3"));
  assert.equal(reloads, 0);
  await waitFor(() => assert.ok(screen.getByText(zh.states.migrating)));
  // Idle while this member's business is still closed is not the end.
  await waitFor(() => assert.ok(screen.getByText(zh.states.idle)));
  assert.equal(reloads, 0);
  await waitFor(() => assert.equal(reloads, 1));
  const after = calls.length;
  await new Promise((resolve) => setTimeout(resolve, 2500));
  assert.equal(calls.length, after, "polling continued after the reload");
});

test("an unknown state code is shown as the code, and a server without the document reloads when it is ready", async () => {
  let reloads = 0;
  serve({ [CLUSTER_UPGRADE_PROGRESS_PATH]: [{ status: 200, body: doc("draining", false) }, { status: 404 }], "/api/v1/readyz": [{ status: 503 }, { status: 200, body: { status: "ready" } }] });
  notifyClusterUpgrade();
  await mount("en", () => { reloads++; });
  assert.ok(await screen.findByText("draining"));
  assert.equal(reloads, 0);
  await waitFor(() => assert.equal(reloads, 1));
});
