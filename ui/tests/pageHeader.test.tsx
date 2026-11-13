import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  // A page's own namespace is the default; the header must still find the menu's words.
  defaultNS: "page",
  resources: { en: { page: {}, common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")) } },
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Link } = await import("react-router-dom");
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: PageHeader } = await import("../src/components/PageHeader.tsx");
const { selectedNavKey } = await import("../src/navigation.tsx");

afterEach(() => cleanup());

function at(path: string, header: React.ReactNode) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[path]}>{header}</MemoryRouter></I18nextProvider>);
}
const crumbs = () => {
  const nav = document.querySelector(".page-header-breadcrumb");
  return nav ? [...nav.querySelectorAll("li")].map((li) => li.textContent?.trim()).filter((text) => text && text !== "/") : [];
};

test("a page reached from the menu shows no breadcrumb, only its title", () => {
  at("/nodes", <PageHeader title="Compute Nodes" description="Onboarded hardware" />);
  assert.deepEqual(crumbs(), []);
  assert.ok(screen.getByRole("heading", { level: 2, name: "Compute Nodes" }));
  assert.ok(screen.getByText("Onboarded hardware"));
});

test("a detail page adds its parents after the section, as links back", () => {
  at("/monitor/d/7", <PageHeader title="qwen" parents={[<Link key="list" to="/monitor">Monitor Center</Link>]} />);
  assert.deepEqual(crumbs(), ["Monitoring & Analytics", "Monitor Center"]);
  assert.equal(screen.getByRole("link", { name: "Monitor Center" }).getAttribute("href"), "/monitor");
});

test("the overview entry omits its section even when the title text differs", () => {
  at("/dashboard", <PageHeader title="Fleet" />);
  assert.deepEqual(crumbs(), []);
});

test("a page below a menu page names the section and its parents, whatever its title", () => {
  at("/deploy", <PageHeader title="Inference" parents={[<Link key="list" to="/deployments">Inference Services</Link>]} />);
  assert.deepEqual(crumbs(), ["Inference", "Inference Services"]);
});

test("a nested route selects the menu item key, not the first path segment", () => {
  assert.equal(selectedNavKey("/playground/chat"), "/playground/chat");
  assert.equal(selectedNavKey("/playground/audio"), "/playground/chat");
  assert.equal(selectedNavKey("/playground"), "/playground/chat");
  assert.equal(selectedNavKey("/nodes"), "/nodes");
  assert.equal(selectedNavKey("/deploy"), "/deployments");
  assert.equal(selectedNavKey("/deployments/3"), "/deployments");
  assert.equal(selectedNavKey("/"), "/dashboard");
});

test("actions sit beside the title and page controls in their own row below", () => {
  const { container } = at("/monitor", <PageHeader title="Monitor Center" extra={<button type="button">Add</button>} toolbar={<input aria-label="Request ID" />} />);
  const header = container.querySelector(".page-header") as HTMLElement;
  const row = header.querySelector("header") as HTMLElement;
  assert.ok(within(row).getByRole("button", { name: "Add" }));
  const toolbar = header.querySelector(".page-header-toolbar") as HTMLElement;
  assert.equal(toolbar.previousElementSibling, row, "the toolbar follows the title row");
  assert.ok(within(toolbar).getByRole("textbox", { name: "Request ID" }));
});
