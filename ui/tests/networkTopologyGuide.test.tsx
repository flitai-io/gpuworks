import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { MemoryRouter } from "react-router-dom";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const load = (lng: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/networks.json`, import.meta.url), "utf8"));
const en = load("en");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { networks: en } },
  defaultNS: "networks",
  interpolation: { escapeValue: false },
});

const { act, cleanup, fireEvent, render, screen, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default;
const NetworkTopologyGuide = (await import("../src/pages/networks/topology/NetworkTopologyGuide.tsx")).default;

afterEach(() => cleanup());

const tp = en.topology;
const renderGuide = () => render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/networks/guide"]}><NetworkTopologyGuide /></MemoryRouter></I18nextProvider>);
const bodyRows = () => screen.getAllByRole("row").filter((row) => row.closest("tbody"));

test("the simple topology lists the three settings it draws", () => {
  renderGuide();
  assert.ok(screen.getByRole("group", { name: tp.diagram.simpleLabel }));
  assert.ok(screen.getByText(tp.simple.heading));
  const rows = bodyRows();
  assert.equal(rows.length, 3);
  assert.ok(within(rows[0]).getByText(tp.simple.rows["1"].name));
  assert.ok(within(rows[0]).getByText(tp.kind.listen));
  assert.ok(within(rows[0]).getByText(tp.simple.rows["1"].not), "the listen address says it does not decide the address others use");
  const legend = screen.getByLabelText(tp.legend.title);
  for (const key of ["external", "management", "serving", "storageSync", "compute", "hostAccess"]) {
    assert.ok(within(legend).getByText(tp.legend[key]));
  }
});

test("switching to the complex topology shows all eight settings", async () => {
  const user = userEvent.setup();
  renderGuide();
  await user.click(screen.getByText(tp.complex.tab));
  assert.ok(screen.getByRole("group", { name: tp.diagram.complexLabel }));
  const rows = bodyRows();
  assert.equal(rows.length, 8);
  assert.ok(within(rows[2]).getByText(tp.complex.rows["3"].name));
  assert.match(tp.complex.rows["3"].not, /--server/);
  assert.ok(within(rows[2]).getByText(tp.complex.rows["3"].not));
  assert.ok(within(rows[7]).getByText(tp.kind.optional));
});

test("pointing at a marker highlights its row and fades the other lines", async () => {
  const user = userEvent.setup();
  renderGuide();
  await user.click(screen.getByText(tp.complex.tab));
  const marker = screen.getByRole("button", { name: "Setting 5" });
  fireEvent.mouseEnter(marker);
  assert.equal(marker.getAttribute("aria-pressed"), "true");
  const rows = bodyRows();
  assert.ok(rows[4].className.includes("net-topo-lit"));
  assert.ok(!rows[0].className.includes("net-topo-lit"));
  const svg = screen.getByRole("group", { name: tp.diagram.complexLabel });
  const serving = [...svg.querySelectorAll('[data-ref="f5"]')];
  const others = [...svg.querySelectorAll("[data-ref]")].filter((el) => el.getAttribute("data-ref") !== "f5");
  assert.ok(serving.length > 0 && serving.every((el) => !el.classList.contains("dimmed")));
  assert.ok(others.length > 0 && others.every((el) => el.classList.contains("dimmed")));
  const markers = screen.getAllByRole("button", { name: /^Setting \d$/ });
  assert.equal(markers.length, 8);
  for (const m of markers) {
    assert.equal(m.classList.contains("dimmed"), m !== marker, `${m.getAttribute("aria-label")} fades unless it is the one pointed at`);
  }
  fireEvent.mouseLeave(marker);
  assert.equal(svg.querySelectorAll(".dimmed").length, 0);
});

test("rows and markers are reachable from the keyboard", async () => {
  const user = userEvent.setup();
  renderGuide();
  const svg = screen.getByRole("group", { name: tp.diagram.simpleLabel });
  const marker1 = screen.getByRole("button", { name: "Setting 1" });
  assert.equal(marker1.getAttribute("tabindex"), "0");
  act(() => marker1.focus());
  assert.ok(bodyRows()[0].className.includes("net-topo-lit"));
  act(() => marker1.blur());
  assert.ok(bodyRows().every((row) => row.getAttribute("tabindex") === "0"));
  act(() => bodyRows()[2].focus());
  assert.ok(bodyRows()[2].className.includes("net-topo-lit"));
  assert.equal(screen.getByRole("button", { name: "Setting 3" }).getAttribute("aria-pressed"), "true");
  assert.ok([...svg.querySelectorAll('[data-ref="s1"]')].every((el) => el.classList.contains("dimmed")));
  act(() => bodyRows()[2].blur());
  assert.equal(svg.querySelectorAll(".dimmed").length, 0);
  // Switching layouts clears the highlight.
  await user.click(screen.getByText(tp.complex.tab));
  assert.equal(screen.getByRole("group", { name: tp.diagram.complexLabel }).querySelectorAll(".dimmed").length, 0);
});

test("both locales translate every topology string", () => {
  const zh = load("zh").topology;
  const keys = (obj: Record<string, unknown>, prefix = ""): string[] =>
    Object.entries(obj).flatMap(([k, v]) => (typeof v === "object" && v ? keys(v as Record<string, unknown>, `${prefix}${k}.`) : [`${prefix}${k}`]));
  assert.deepEqual(keys(zh).sort(), keys(tp).sort());
});
