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
const { cleanup, render, screen, within, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ServiceTable } = await import("../src/pages/monitor/ServiceTable.tsx");
import type { MonitorService, MonitorStateCount } from "../src/api";

const overview = JSON.parse(fs.readFileSync(new URL("./fixtures/monitor-overview.json", import.meta.url), "utf8"));
const services = overview.services as MonitorService[];
const stateCounts = overview.state_counts as MonitorStateCount[];

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
          <Route path="/monitor" element={<ServiceTable services={services} stateCounts={stateCounts} {...props} />} />
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
  renderTable({ signalFilter: { label: "Ready replicas", deploymentIds: [104] } });
  assert.ok(screen.getByText("DeepSeek-V2-Lite"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
});

test("a signal narrowing is a toolbar filter chip beside the column filters, and its clear goes to the owner", async () => {
  let cleared = 0;
  renderTable({ signalFilter: { label: "Ready replicas", deploymentIds: [103, 107] }, onClearSignalFilter: () => { cleared++; } });
  const toolbar = screen.getByRole("heading", { name: "Inference Services" }).closest(".resource-table-toolbar") as HTMLElement;
  const chip = within(within(toolbar).getByRole("group", { name: "Filtered" })).getByRole("button", { name: "Clear filter: Signal Ready replicas" });
  assert.equal(chip.textContent, "Signal: Ready replicas");
  await userEvent.setup().click(chip);
  assert.equal(cleared, 1);
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
  assert.deepEqual(headers, ["Service name", "State", "Health", "TTFT p95"]);
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

const pressed = (name: string) => screen.getByRole("button", { name, exact: true }).getAttribute("aria-pressed");

test("state count shortcuts and the column filter share one selection", async () => {
  renderTable();
  const user = userEvent.setup();
  assert.equal(pressed("All 11"), "true", "nothing picked lights All");
  await user.click(screen.getByRole("button", { name: "Starting 1" }));
  await waitFor(() => assert.equal(pressed("Starting 1"), "true"));
  assert.equal(pressed("All 11"), "false");
  // The pressed shortcut says what is filtered: no second "State: Starting" chip.
  assert.equal(screen.queryByRole("button", { name: /^Clear filter: State/ }), null);
  assert.equal(screen.queryByRole("group", { name: "Filtered" }), null);
  assert.ok(screen.getByText("Llama-3.1-70B"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
  await user.click(screen.getByRole("button", { name: "All 11" }));
  await waitFor(() => assert.equal(pressed("Starting 1"), "false"));
  assert.equal(pressed("All 11"), "true");
  assert.ok(screen.getByText("Qwen3-32B"));
  await user.click(screen.getByRole("button", { name: "Filter by State" }));
  await user.click(await screen.findByRole("checkbox", { name: /Starting/ }));
  await waitFor(() => assert.equal(pressed("Starting 1"), "true"));
  assert.equal(screen.queryByRole("button", { name: /^Clear filter: State/ }), null);
});

test("a state picked from the shortcut shows checked in the column filter, and clicking the pressed shortcut again clears it", async () => {
  renderTable();
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Failed 1" }));
  await user.click(screen.getByRole("button", { name: "Filter by State" }));
  const failed = await screen.findByRole("checkbox", { name: /Failed/ });
  assert.equal(failed.getAttribute("aria-checked") ?? String((failed as HTMLInputElement).checked), "true");
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: "Failed 1" }));
  await waitFor(() => assert.equal(pressed("All 11"), "true"));
  assert.ok(screen.getByText("Qwen3-32B"));
});

test("several states picked in the column light no shortcut and keep the toolbar chip", async () => {
  renderTable();
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Filter by State" }));
  await user.click(await screen.findByRole("checkbox", { name: /Starting/ }));
  await user.click(await screen.findByRole("checkbox", { name: /Failed/ }));
  await user.keyboard("{Escape}");
  await waitFor(() => assert.ok(screen.getByRole("button", { name: "Clear filter: State Starting, Failed" })));
  for (const name of ["All 11", "Running 8", "Starting 1", "Asleep 1", "Failed 1"]) assert.equal(pressed(name), "false", name);
  assert.ok(screen.getByText("Llama-3.1-70B"));
  assert.ok(screen.getByText("ChatGLM3-6B"));
  assert.equal(screen.queryByText("Qwen3-32B"), null);
});

test("the summary shows the server's counts and order, not a count of the rows it was given", () => {
  // Deliberately disagrees with services[]: the counts are the server's.
  renderTable({ stateCounts: [
    { state: "failed", count: 4 }, { state: "running", count: 30 }, { state: "starting", count: 0 },
    { state: "asleep", count: 0 }, { state: "offline", count: 2 }, { state: "stopped", count: 0 },
  ] });
  const summary = screen.getByRole("group", { name: "Service state" });
  assert.deepEqual(within(summary).getAllByRole("button").map((button) => button.textContent), ["All 36", "Failed 4", "Running 30", "Offline 2"]);
});


test("offline services use warning status in both the summary and table while failed services stay danger", () => {
  renderTable({ services: [
    { ...services[0], deployment_id: 9001, name: "Offline service", state: "offline" },
    { ...services[0], deployment_id: 9002, name: "Failed service", state: "failed" },
  ], stateCounts: [
    { state: "running", count: 0 }, { state: "starting", count: 0 }, { state: "asleep", count: 0 },
    { state: "offline", count: 1 }, { state: "failed", count: 1 }, { state: "stopped", count: 0 },
  ] });
  assert.ok(screen.getByRole("heading", { name: "Inference Services" }));
  assert.equal(screen.getByRole("link", { name: "Offline service" }).getAttribute("href"), "/monitor/d/9001");
  const offline = screen.getByText("Offline service").closest("tr")!;
  const failed = screen.getByText("Failed service").closest("tr")!;
  assert.equal(within(offline).getByText("Offline", { exact: true }).closest('[data-slot="badge"]')?.getAttribute("data-status"), "warning");
  assert.equal(within(failed).getByText("Failed", { exact: true }).closest('[data-slot="badge"]')?.getAttribute("data-status"), "danger");
  const summary = screen.getByRole("group", { name: "Service state" });
  assert.equal(within(summary).getByRole("button", { name: "Offline 1" }).querySelector('[data-slot="badge"]')?.getAttribute("data-status"), "warning");
});
