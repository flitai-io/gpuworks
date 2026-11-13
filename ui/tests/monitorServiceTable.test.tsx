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
let matchMediaMatches = false;
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (query: string) => ({
    matches: matchMediaMatches,
    media: query,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ServiceTable } = await import("../src/pages/monitor/ServiceTable.tsx");
import type { MonitorService } from "../src/api";

const overview = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-overview.json", import.meta.url), "utf8"));
const services = overview.services as MonitorService[];

afterEach(() => {
  cleanup();
  matchMediaMatches = false;
});

let seenPath = "";
function LocationProbe() {
  seenPath = useLocation().pathname;
  return null;
}

function renderTable(props: Partial<React.ComponentProps<typeof ServiceTable>> = {}) {
  seenPath = "";
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/monitor"]}>
        <Routes>
          <Route path="/monitor" element={<ServiceTable services={services} {...props} />} />
          <Route path="/monitor/d/:id" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

test("rows render in server-given order with no client resorting", () => {
  renderTable();
  const rows = screen.getAllByRole("row").slice(1); // drop the header row
  const names = rows.map((row) => within(row).getAllByRole("cell")[0].textContent ?? "");
  services.forEach((service, index) => {
    assert.ok(names[index].startsWith(service.name), `row ${index} should be ${service.name}, got ${names[index]}`);
  });
});

test("null metrics render as a muted dash, not 0", () => {
  renderTable();
  const startingRow = screen.getByText("Llama-3.1-70B").closest("tr")!;
  assert.ok(within(startingRow).getAllByText("—").length > 0);
  assert.equal(within(startingRow).queryByText("0"), null);
});

test("running and waiting render independently: one side null does not fabricate the other as 0", () => {
  const halfNull: MonitorService = { ...services[0], deployment_id: 9001, name: "HalfNullSvc", running: 38, waiting: null };
  matchMediaMatches = true;
  renderTable({ services: [halfNull] } as never);
  const row = screen.getByText("HalfNullSvc").closest("tr")!;
  assert.ok(row.textContent?.includes("38"), row.textContent ?? "");
  assert.ok(within(row).getAllByText("—").length > 0);
  // The old bug rendered "38 / 0"; assert that never appears.
  assert.equal(row.textContent?.includes("38 / 0"), false);
});

test("a filter narrows the visible rows to the given deployment ids", () => {
  renderTable({ filterDeploymentIds: [104] });
  assert.ok(screen.getByText("DeepSeek-V2-Lite"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
});

test("clicking a row navigates to the service monitor route (mouse-only row affordance)", async () => {
  matchMediaMatches = true; // wide layout so every column, including state, renders
  renderTable();
  const row = screen.getByText("Qwen3-32B").closest("tr") as HTMLElement;
  assert.ok(row.className.includes("gpu-clickable-row"));
  // No role/tabIndex override: the row stays a plain, non-interactive <tr> in
  // the accessibility tree - see the service-name Link test below for the
  // keyboard-reachable path to the same destination.
  assert.equal(row.getAttribute("role"), null);
  await userEvent.setup().click(row);
  assert.equal(seenPath, "/monitor/d/101");
});

test("the service name is a real link that keyboard/AT users can tab to and activate", async () => {
  renderTable();
  const link = screen.getByRole("link", { name: "Qwen2.5-7B-Instruct" });
  assert.equal(link.getAttribute("href"), "/monitor/d/102");
  link.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(seenPath, "/monitor/d/102");
});

test("below the md breakpoint only service, state, health and TTFT show", () => {
  matchMediaMatches = false; // "(min-width: 768px)" does not match -> narrow
  renderTable();
  const headerRow = screen.getAllByRole("row")[0];
  const headers = within(headerRow).getAllByRole("columnheader").map((h) => h.textContent);
  assert.deepEqual(headers, ["Service", "State", "Health", "TTFT p95"]);
});

test("at or above the md breakpoint every column shows", () => {
  matchMediaMatches = true; // "(min-width: 768px)" matches -> wide
  renderTable();
  const headerRow = screen.getAllByRole("row")[0];
  const headers = within(headerRow).getAllByRole("columnheader").map((h) => h.textContent);
  assert.ok(headers.includes("KV Cache usage"));
  assert.ok(headers.includes("Prefill tok/s"));
  assert.equal(headers.length, 11);
});

// The service column once collapsed to a one-character strip under auto
// layout. The shared list declares every width and lays them out fixed, with
// the service name leading frozen on the left.
function declaredColWidths(): number[] {
  const cols = Array.from(document.querySelectorAll("table thead th")) as HTMLElement[];
  return cols.map((col) => Number.parseFloat(col.style.width)).filter((width) => !Number.isNaN(width));
}

test("wide layout is fixed-layout at the declared widths with the service frozen first", () => {
  matchMediaMatches = true;
  renderTable();
  const table = screen.getByRole("table") as HTMLElement;
  assert.equal(table.style.tableLayout, "fixed");
  const widths = declaredColWidths();
  assert.equal(widths[0], 220, `service column width in ${widths.join(",")}`);
  assert.equal(widths[2], 150, `the health column has a declared width of its own in ${widths.join(",")}`);
  assert.equal(table.style.width, `${widths.reduce((sum, width) => sum + width, 0)}px`);
  assert.equal((screen.getAllByRole("columnheader")[0] as HTMLElement).style.left, "0px");
});

test("narrow layout is fixed-layout too, over its four columns", () => {
  matchMediaMatches = false;
  renderTable();
  assert.equal((screen.getByRole("table") as HTMLElement).style.tableLayout, "fixed");
  assert.deepEqual(declaredColWidths(), [220, 110, 150, 100]);
});

test("the health column shows each service's health check, beside its state", () => {
  matchMediaMatches = true;
  renderTable();
  const headers = within(screen.getAllByRole("row")[0]).getAllByRole("columnheader").map((h) => h.textContent);
  assert.equal(headers.indexOf("Health"), headers.indexOf("State") + 1);
  const cells = (name: string) => within(screen.getByText(name).closest("tr")!).getAllByRole("cell").map((cell) => cell.textContent);
  assert.ok(cells("Qwen3-32B").includes("Healthy · 85 ms"), String(cells("Qwen3-32B")));
  assert.ok(cells("Qwen2.5-7B-Instruct").includes("Timed out"), String(cells("Qwen2.5-7B-Instruct")));
});

test("the TTFT p95 column carries its unit and reads seconds from 1 s", () => {
  matchMediaMatches = true;
  const slow = services.map((s, i) => (i === 0 ? { ...s, ttft_p95_ms: 39000 } : s));
  renderTable({ services: slow });
  const cells = (name: string) => within(screen.getByText(name).closest("tr")!).getAllByRole("cell").map((cell) => cell.textContent);
  assert.ok(cells(slow[0].name).includes("39s"), String(cells(slow[0].name)));
  assert.ok(cells(services[1].name).includes(`${services[1].ttft_p95_ms} ms`), String(cells(services[1].name)));
});
