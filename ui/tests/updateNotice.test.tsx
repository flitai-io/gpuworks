import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, beforeEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "CustomEvent", "Event"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
const closeChannels = trackMessageChannels();

const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { common: commonLocale } }, defaultNS: "common", interpolation: { escapeValue: false } });

const { act, cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const noticeModule = await import("../src/components/UpdateNotice.tsx");
const UpdateNotice = noticeModule.default;
type UpdateStatus = import("../src/api.ts").UpdateStatus;

const original = apiModule.api.updates;
beforeEach(() => { try { window.localStorage.clear(); } catch { /* no storage */ } });
afterEach(() => { cleanup(); apiModule.api.updates = original; });
after(() => closeChannels());

function status(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    channel: "nightly", channel_editable: true, checked_at: "2026-10-11T02:00:00Z", available: true, sources: [],
    server: { current: "a", latest: "a", update_available: false, installable: true, self_upgrade: true },
    image_catalog: { current: "x", latest: "x", update_available: false, compatible: true },
    template_catalog: { current: "y", latest: "y", update_available: false, compatible: true },
    ...overrides,
  };
}

function Where() {
  const location = useLocation();
  return <span data-testid="where">{location.pathname + location.hash}</span>;
}

function renderNotice() {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/dashboard"]}>
        <UpdateNotice />
        <Routes><Route path="*" element={<Where />} /></Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

const label = commonLocale.header.updateAvailable;

test("an available update shows a header button that leads to Version and updates", async () => {
  let calls = 0;
  apiModule.api.updates = async () => { calls++; return status({ server: { current: "a", latest: "b", update_available: true, installable: true, self_upgrade: true } }); };
  renderNotice();
  const button = await screen.findByRole("button", { name: new RegExp(label) });
  await userEvent.click(button);
  assert.equal(screen.getByTestId("where").textContent, "/settings#updates");
  assert.equal(calls, 1);
  // A reload within the hour does not ask the server again.
  cleanup();
  renderNotice();
  await screen.findByRole("button", { name: new RegExp(label) });
  assert.equal(calls, 1);
});

test("nothing is shown when everything is current, the catalog needs a newer server, or the check fails", async () => {
  apiModule.api.updates = async () => status({ template_catalog: { current: "y", latest: "z", update_available: true, compatible: false } });
  renderNotice();
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(screen.queryByRole("button", { name: new RegExp(label) }), null);
  cleanup();
  window.localStorage.clear();
  apiModule.api.updates = async () => { throw new Error("forbidden"); };
  renderNotice();
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(screen.queryByRole("button", { name: new RegExp(label) }), null);
});

test("a different channel offers a manual version choice without triggering a server update notice", () => {
  const value = status({ server: { current: "v26.11.2", latest: "abc12345", update_available: true, installable: true,
    current_build: { version: "v26.11.2", kind: "stable", build_channel: "release" } } });
  assert.equal(noticeModule.serverUpdateState(value), "differentChannelVersion");
  assert.equal(noticeModule.hasUpdate(value), false);
  value.image_catalog.update_available = true;
  assert.equal(noticeModule.hasUpdate(value), true);
});

test("the notice follows a check made on the settings page", async () => {
  apiModule.api.updates = async () => status({ image_catalog: { current: "x", latest: "y", update_available: true, compatible: true } });
  renderNotice();
  await screen.findByRole("button", { name: new RegExp(label) });
  act(() => noticeModule.publishUpdateStatus(status()));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: new RegExp(label) }), null));
});
