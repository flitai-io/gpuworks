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
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")),
      deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: ServiceLogStream } = await import("../src/pages/monitor/ServiceLogStream.tsx");
import type { InstanceEvent, LogEntry, LogsPage } from "../src/api";
import { cursorPage } from "./helpers/listPage.ts";

// Real instance uuids are auth.NewUUID() - 32 lowercase hex characters, no
// dashes (internal/server/auth/token.go). The fixtures here mirror that
// shape rather than an invented dashed format, since ServiceLogStream's own
// short-id prefix parsing (REPLICA_PREFIX) assumes exactly that.
const REPLICA_A = "1111aaaa2222bbbb3333cccc4444dddd";
const REPLICA_B = "5555eeee6666ffff7777000088889999";
const SHORT_A = "1111aaaa";
const SHORT_B = "5555eeee";

const originalLogsSearch = api.logsSearch;
const originalInstanceEvents = api.instanceEvents;

afterEach(() => {
  cleanup();
  api.logsSearch = originalLogsSearch;
  api.instanceEvents = originalInstanceEvents;
});

function entry(uuid: string, timeMS: number, line: string, level = "info"): LogEntry {
  return { id: timeMS, instance: uuid, node: "node-1", level: level, source: "instance", time_ms: timeMS, line: line };
}

function stubLogs(page: LogsPage) {
  api.logsSearch = async () => page;
}

function stubEvents(byReplica: Record<string, InstanceEvent[]>) {
  api.instanceEvents = async (uuid: string) => cursorPage(byReplica[uuid] ?? []);
}

function renderStream(logInstances: string[], range: "1h" | "6h" = "1h") {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><ServiceLogStream logInstances={logInstances} range={range} /></MemoryRouter>
    </I18nextProvider>,
  );
}

function preText(container: HTMLElement): string {
  return container.querySelector("pre")?.textContent ?? "";
}

test("shows a loading skeleton until the first fetch resolves", async () => {
  let resolveFetch: (page: LogsPage) => void = () => {};
  api.logsSearch = () => new Promise((resolve) => { resolveFetch = resolve; });
  stubEvents({});
  const { container } = renderStream([REPLICA_A]);
  assert.equal(container.querySelector(".ant-skeleton") != null, true);
  assert.equal(container.querySelector("pre"), null);

  resolveFetch({ items: [entry(REPLICA_A, 1000, "line")] });
  await waitFor(() => assert.ok(container.querySelector("pre")));
  assert.equal(container.querySelector(".ant-skeleton"), null);
});

test("an empty replica set shows the empty state without fetching", async () => {
  let called = false;
  api.logsSearch = async () => { called = true; return { items: [] }; };
  render(<I18nextProvider i18n={i18n}><MemoryRouter><ServiceLogStream logInstances={[]} range="1h" /></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("No replicas to show logs for")));
  assert.equal(called, false);
});

test("each log line is prefixed with its replica's short id, colored consistently per replica, tinted by level, newest first", async () => {
  // The search API already returns newest-first (a's 2000 before b's 1000);
  // the stream must not reorder it into chronological order.
  stubLogs({ items: [entry(REPLICA_A, 2000, "line from a"), entry(REPLICA_B, 1000, "line from b", "error")] });
  stubEvents({});
  const { container } = renderStream([REPLICA_A, REPLICA_B]);
  await waitFor(() => assert.ok(preText(container).includes("line from a")));

  const spans = [...container.querySelectorAll("pre span[style*='color']")];
  const aBadge = spans.find((el) => el.textContent === `[${SHORT_A}]`);
  const bBadge = spans.find((el) => el.textContent === `[${SHORT_B}]`);
  assert.ok(aBadge, `expected a colored [${SHORT_A}] badge`);
  assert.ok(bBadge, `expected a colored [${SHORT_B}] badge`);
  assert.notEqual((aBadge as HTMLElement).style.color, (bBadge as HTMLElement).style.color);
  assert.ok(preText(container).indexOf("line from a") < preText(container).indexOf("line from b"), "newest first: a (2000) before b (1000)");

  const errorLine = [...container.querySelectorAll(".log-line > span")].find((el) => el.textContent?.includes("line from b")) as HTMLElement;
  assert.notEqual(errorLine.style.color, "", "an error-level line is tinted, not left at the default color");
});

test("system lines from each replica's own events merge into the stream, newest first", async () => {
  stubLogs({ items: [entry(REPLICA_A, 1000, "log a1"), entry(REPLICA_A, 3000, "log a2")] });
  stubEvents({
    [REPLICA_A]: [{ id: 1, kind: "state", code: "INSTANCE_STATE_CHANGED", params: { state: "running" }, created_at: new Date(2000).toISOString() }],
  });
  const { container } = renderStream([REPLICA_A]);
  await waitFor(() => assert.ok(preText(container).includes("log a2")));

  const lines = preText(container).split("\n");
  assert.equal(lines.length, 3);
  // Newest first: a2 (3000), then the event (2000), then a1 (1000).
  assert.ok(lines[0].includes("log a2"));
  assert.ok(lines[1].includes(`[${SHORT_A}]`) && lines[1].includes("State changed to Running"), `expected a system line: ${lines[1]}`);
  assert.ok(lines[2].includes("log a1"));
  const eventLine = [...container.querySelectorAll<HTMLSpanElement>("pre span")].find(el => el.textContent?.includes("State changed to Running") && el.style.background);
  assert.ok(eventLine);
  assert.equal(eventLine.style.color, "rgb(161, 210, 254)", "platform events use readable information blue");
  assert.equal(container.querySelector("pre")?.style.fontSize, "var(--font-size-body)");
});

test("a per-replica filter chip hides that replica's lines and events", async () => {
  stubLogs({ items: [entry(REPLICA_A, 1000, "line from a"), entry(REPLICA_B, 2000, "line from b")] });
  stubEvents({});
  const { container } = renderStream([REPLICA_A, REPLICA_B]);
  await waitFor(() => assert.ok(preText(container).includes("line from a")));
  assert.ok(preText(container).includes("line from b"));

  fireEvent.click(screen.getByText(SHORT_B));
  await waitFor(() => assert.equal(preText(container).includes("line from b"), false));
  assert.ok(preText(container).includes("line from a"), "the unfiltered replica's lines stay visible");

  // Re-checking the chip brings its lines back.
  fireEvent.click(screen.getByText(SHORT_B));
  await waitFor(() => assert.ok(preText(container).includes("line from b")));
});

test("a fetch failure shows an error banner with a working retry", async () => {
  let calls = 0;
  api.logsSearch = async () => { calls += 1; if (calls === 1) throw new Error("network down"); return { items: [entry(REPLICA_A, 1000, "recovered")] }; };
  stubEvents({});
  renderStream([REPLICA_A]);
  await waitFor(() => assert.ok(screen.getByText("Failed to load logs")));
  assert.ok(screen.getByText("network down"));

  // fireEvent (not a bare .click()) so the state update it triggers is
  // flushed inside act() before the next assertion runs.
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => assert.equal(screen.queryByText("Failed to load logs"), null));
  assert.equal(calls, 2);
});

test("a successful fetch with zero matching lines shows the no-logs-in-range message", async () => {
  stubLogs({ items: [] });
  stubEvents({});
  const { container } = renderStream([REPLICA_A]);
  await waitFor(() => assert.ok(preText(container).includes("No log lines in the selected range")));
});

test("the truncated banner reports the tail limit when the server returns a full 2000-line page", async () => {
  // A realistic truncated:true response: exactly LOG_TAIL_LIMIT (2000)
  // entries, matching what the server now actually sends once it honors the
  // requested limit and sets Truncated itself - not an unrealistic
  // zero-entries-but-truncated stub.
  // Newest-first, matching the real search API's own ordering convention.
  const entries = Array.from({ length: 2000 }, (_, i) => entry(REPLICA_A, 3000 - i, `line ${i}`));
  stubLogs({ items: entries, truncated: true });
  stubEvents({});
  const { container } = renderStream([REPLICA_A]);
  await waitFor(() => assert.ok(screen.getByText(/More than 2000 log lines/)));
  assert.equal(preText(container).split("\n").length, 2000);
});

test("the header link deep-links to the first replica only, scoped to the stream's fetch window", async () => {
  stubLogs({ items: [entry(REPLICA_A, 1000, "line")] });
  stubEvents({});
  const before = Date.now();
  renderStream([REPLICA_A, REPLICA_B]);
  const link = await waitFor(() => screen.getByRole("link", { name: "Open first replica in log console" })) as HTMLAnchorElement;
  const after = Date.now();

  const url = new URL(link.getAttribute("href") ?? "", "http://localhost");
  assert.equal(url.pathname, "/logs");
  assert.equal(url.searchParams.get("instance"), REPLICA_A);
  const to = Number(url.searchParams.get("to"));
  const from = Number(url.searchParams.get("from"));
  assert.ok(to >= before && to <= after, `to=${to} should fall within [${before}, ${after}]`);
  assert.equal(from, to - 3_600_000, "range=1h in this test's renderStream call");
});

test("each rendered line carries its own deep link to the log console for its own replica", async () => {
  stubLogs({ items: [entry(REPLICA_A, 1000, "line from a")] });
  stubEvents({});
  const { container } = renderStream([REPLICA_A, REPLICA_B]);
  await waitFor(() => assert.ok(screen.getByText("line from a")));

  const lineLink = screen.getByRole("link", { name: new RegExp(SHORT_A) }) as HTMLAnchorElement;
  assert.equal(lineLink.textContent, "→", "the arrow glyph must render literally, not as an unescaped HTML entity");
  const url = new URL(lineLink.getAttribute("href") ?? "", "http://localhost");
  assert.equal(url.searchParams.get("instance"), REPLICA_A);
  assert.ok(url.searchParams.get("from"));
  assert.ok(url.searchParams.get("to"));

  // Structural, not just visual: the arrow lives in a user-select:none
  // sibling of the line's text (src/styles.css hides/reveals it), not
  // interleaved inside the text itself - so selecting/copying the line's
  // text can never also select the arrow.
  const line = container.querySelector(".log-line") as HTMLElement;
  const linkWrapper = line.querySelector(".log-line-link") as HTMLElement;
  assert.ok(linkWrapper.contains(lineLink), "the link lives inside the .log-line-link wrapper");
  assert.equal(linkWrapper.previousElementSibling?.textContent?.includes("line from a"), true, "the text lives in a sibling span, not the same span as the link");
  assert.equal(line.getAttribute("tabindex"), "0", "the line itself is a keyboard stop, so the link can be reached (and revealed) via Tab without a mouse");
});

test("a system line's own deep link points at its own replica, not the first one", async () => {
  stubLogs({ items: [] });
  stubEvents({
    [REPLICA_B]: [{ id: 1, kind: "state", code: "INSTANCE_STATE_CHANGED", params: { state: "running" }, created_at: new Date(1000).toISOString() }],
  });
  renderStream([REPLICA_A, REPLICA_B]);
  await waitFor(() => assert.ok(screen.getByText(/State changed to Running/)));

  const link = screen.getByRole("link", { name: new RegExp(SHORT_B) }) as HTMLAnchorElement;
  const url = new URL(link.getAttribute("href") ?? "", "http://localhost");
  assert.equal(url.searchParams.get("instance"), REPLICA_B);
});
