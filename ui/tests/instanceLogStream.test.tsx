import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { InstanceEvent } from "../src/api";
import type { EventTimelineState } from "../src/pages/useInstanceEvents";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

// A faithful EventSource stub: the product only uses onmessage/onerror (not
// addEventListener), so both must be independently exercisable to actually
// cover the two paths the log stream wires up.
const openEventSources: EventSourceStub[] = [];
class EventSourceStub {
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  url: string;
  constructor(url: string) { this.url = url; openEventSources.push(this); }
  close() { this.closed = true; }
}
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });

function latestEventSource(): EventSourceStub {
  const source = openEventSources[openEventSources.length - 1];
  if (!source) throw new Error("no EventSource was opened");
  return source;
}

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")) } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { cleanup, render, waitFor } = await import("@testing-library/react");
const { LogStream } = await import("../src/pages/InstanceLogStream.tsx");

const originalFetch = globalThis.fetch;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  openEventSources.length = 0;
});

function stubPersistedLogs(entries: Array<{ time_ms: number; line: string }>) {
  globalThis.fetch = (async () => new Response(
    JSON.stringify({ items: entries.map((e, i) => ({ id: i + 1, instance: "inst-1", node: "node-1", level: "", source: "instance", time_ms: e.time_ms, line: e.line })) }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  )) as typeof fetch;
}

function stateChangedEvent(id: number, timeMS: number, state: string): InstanceEvent {
  return { id, kind: "state", code: "INSTANCE_STATE_CHANGED", params: { state }, created_at: new Date(timeMS).toISOString() };
}

function timelineOf(events: InstanceEvent[], overrides: Partial<EventTimelineState> = {}): EventTimelineState {
  return { events, error: false, hasMore: false, reload: () => {}, ...overrides };
}

function linesOf(container: HTMLElement): string[] {
  return (container.querySelector("pre")?.textContent ?? "").split("\n");
}

test("an instance event merges into the non-live log stream in time order, tagged as a system line", async () => {
  stubPersistedLogs([{ time_ms: 3000, line: "line b" }, { time_ms: 1000, line: "line a" }]);
  const timeline = timelineOf([stateChangedEvent(1, 2000, "running")]);

  const { container } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true} timeline={timeline} /></I18nextProvider>,
  );
  await waitFor(() => assert.equal(linesOf(container).length, 3));

  const lines = linesOf(container);
  // Persisted lines carry a server-arrival ts prefix (timezone-dependent), so
  // assert its shape rather than a rendered clock value.
  assert.ok(/^\[\d\d-\d\d \d\d:\d\d:\d\d\] line a$/.test(lines[0]), `the earliest log line stays first: ${lines[0]}`);
  assert.ok(/^\[\d\d-\d\d \d\d:\d\d:\d\d\] line b$/.test(lines[2]), `the latest log line stays last: ${lines[2]}`);
  assert.ok(lines[1].includes("[System]"), "the event line carries the system tag");
  const eventLine = [...container.querySelectorAll<HTMLSpanElement>("pre span")].find(el => el.textContent?.includes("State changed to running") && el.style.background);
  assert.ok(eventLine);
  assert.equal(eventLine.style.color, "rgb(161, 210, 254)", "platform events use readable information blue");
  assert.equal(container.querySelector("pre")?.style.fontSize, "var(--font-size-body)");
  assert.ok(lines[1].includes("State changed to running"), "the event line renders the instance event's own text");
});

test("with no timeline passed, the log stream renders exactly the log lines (no system tag noise)", async () => {
  stubPersistedLogs([{ time_ms: 1000, line: "only line" }]);
  const { container } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true} /></I18nextProvider>,
  );
  await waitFor(() => assert.ok(/^\[\d\d-\d\d \d\d:\d\d:\d\d\] only line$/.test(linesOf(container).join("\n"))));
});

test("a folded milestone's phase-cost summary is appended to its system line", async () => {
  stubPersistedLogs([]);
  const milestone: InstanceEvent = {
    id: 9, kind: "state", code: "INSTANCE_STATUS_UPDATED",
    params: {
      state: "starting", phase: "container_start",
      prev_phase: "image_pull", prev_duration_ms: 42_000, prev_bytes_done: 1_073_741_824, prev_rate_bps: 26_214_400,
    },
    created_at: new Date(1000).toISOString(),
  };
  const { container } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true} timeline={timelineOf([milestone])} /></I18nextProvider>,
  );
  await waitFor(() => assert.ok(linesOf(container)[0]?.includes("[System]")));
  assert.ok(
    linesOf(container)[0].includes("Downloading Image took 42 s · 1.0 GiB at 25 MiB/s"),
    `expected the phase summary in: ${linesOf(container)[0]}`,
  );
});

test("startup output appears once in recent logs and never leaks into a selected historical window", async () => {
  stubPersistedLogs([{ time_ms: 1000, line: "stored line" }]);
  const renderLogs = (line: string, history: boolean) => (
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true}
      runtimeLine={line} range={history ? { kind: "custom", startMS: 0, endMS: 2000 } : null} /></I18nextProvider>
  );
  const view = render(renderLogs("stored line", false));
  await waitFor(() => assert.match(linesOf(view.container)[0], /^\[.+\] stored line$/));
  assert.equal(linesOf(view.container).length, 1);
  view.rerender(renderLogs("extracting layer", false));
  assert.equal(linesOf(view.container).at(-1), "extracting layer");
  view.rerender(renderLogs("extracting layer", true));
  await waitFor(() => assert.match(linesOf(view.container)[0], /stored line/));
  assert.equal(linesOf(view.container).length, 1);
});

test("an events-fetch error renders a muted retry line instead of silently dropping history", async () => {
  stubPersistedLogs([{ time_ms: 1000, line: "line" }]);
  let reloaded = false;
  const timeline = timelineOf([], { error: true, reload: () => { reloaded = true; } });
  const { container, getByRole } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true} timeline={timeline} /></I18nextProvider>,
  );
  await waitFor(() => assert.ok(container.textContent?.includes("Event history unavailable")));
  getByRole("button", { name: "Retry" }).click();
  assert.ok(reloaded, "the retry action must call the timeline's reload");
});

test("hasMore renders a muted 'earlier events not shown' line", async () => {
  stubPersistedLogs([{ time_ms: 1000, line: "line" }]);
  const timeline = timelineOf([], { hasMore: true });
  const { container } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={false} follow={true} timeline={timeline} /></I18nextProvider>,
  );
  await waitFor(() => assert.ok(container.textContent?.includes("Earlier events are not shown")));
});

// --- Live mode: one clock (the receive clock); SSE onmessage/onerror paths ---

test("live: onmessage appends a receive-clock-stamped line, and onerror is a harmless no-op", async () => {
  const { container } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={true} follow={true} /></I18nextProvider>,
  );
  const source = latestEventSource();
  assert.ok(source.url.includes("/api/v1/instances/inst-1/logs"));

  source.onmessage?.({ data: "hello live" } as MessageEvent);
  await waitFor(() => assert.equal(linesOf(container).join("\n"), "hello live"));

  // onerror must not throw and must not clear what is already shown; the
  // product relies on EventSource's own auto-reconnect.
  assert.doesNotThrow(() => source.onerror?.());
  assert.equal(linesOf(container).join("\n"), "hello live");
});

test("live: a backlog of lines plus pre-stream milestones render as one history block, then the backlog", async () => {
  const before = Date.now();
  const history = [
    stateChangedEvent(2, before - 2000, "starting"),
    stateChangedEvent(1, before - 5000, "pulling"),
  ];
  const timeline = timelineOf(history);
  const { container } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={true} follow={true} timeline={timeline} /></I18nextProvider>,
  );
  const source = latestEventSource();
  // The docker --tail backlog: several lines delivered right after connect.
  source.onmessage?.({ data: "backlog 1" } as MessageEvent);
  source.onmessage?.({ data: "backlog 2" } as MessageEvent);

  await waitFor(() => assert.equal(linesOf(container).length, 4));
  const lines = linesOf(container);
  assert.ok(lines[0].includes("[System]") && lines[0].includes("State changed to pulling"), `unexpected first line: ${lines[0]}`);
  assert.ok(lines[1].includes("[System]") && lines[1].includes("State changed to starting"), "the older milestone (pulling) sorts before the newer one (starting)");
  assert.equal(lines[2], "backlog 1", "history sorts entirely before the backlog");
  assert.equal(lines[3], "backlog 2");
});

test("live: a milestone arriving mid-stream lands after the newest live line despite server clock skew", async () => {
  // A baseline history event is present from the very first render, so the
  // stream's "first merge decides history" identity check is unambiguous:
  // the milestone introduced later is definitely new, definitely live.
  const baseline = stateChangedEvent(1, Date.now() - 60_000, "pulling");
  const { container, rerender } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={true} follow={true} timeline={timelineOf([baseline])} /></I18nextProvider>,
  );
  const source = latestEventSource();
  source.onmessage?.({ data: "line 1" } as MessageEvent);
  await waitFor(() => assert.ok(linesOf(container).some((line) => line === "line 1")));
  source.onmessage?.({ data: "line 2" } as MessageEvent);
  await waitFor(() => assert.ok(linesOf(container).some((line) => line === "line 2")));

  // The milestone is observed live (its own created_at is far in the future -
  // a +30s-skewed server clock), well after the stream opened. timeline.events
  // always carries the full accumulated list, so the baseline stays present.
  const skewed = stateChangedEvent(9, Date.now() + 30_000, "running");
  rerender(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={true} follow={true} timeline={timelineOf([baseline, skewed])} /></I18nextProvider>,
  );

  await waitFor(() => {
    const lines = linesOf(container);
    assert.equal(lines.length, 4);
    assert.ok(lines[0].includes("[System]"), "the baseline history event renders first");
    assert.equal(lines[1], "line 1");
    assert.equal(lines[2], "line 2");
    assert.ok(lines[3].includes("[System]"), "the skewed milestone must land after both live lines, not above them");
  });
});

test("live: a milestone arriving mid-stream lands after the newest live line despite a lagging server clock", async () => {
  const baseline = stateChangedEvent(1, Date.now() - 60_000, "pulling");
  const { container, rerender } = render(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={true} follow={true} timeline={timelineOf([baseline])} /></I18nextProvider>,
  );
  const source = latestEventSource();
  source.onmessage?.({ data: "line 1" } as MessageEvent);
  await waitFor(() => assert.ok(linesOf(container).some((line) => line === "line 1")));

  // The server's clock lags by 30s: created_at claims a time before the
  // baseline history event above, which a naive comparison would misfile.
  const lagging = stateChangedEvent(8, Date.now() - 90_000, "running");
  rerender(
    <I18nextProvider i18n={i18n}><LogStream uuid="inst-1" live={true} follow={true} timeline={timelineOf([baseline, lagging])} /></I18nextProvider>,
  );

  await waitFor(() => {
    const lines = linesOf(container);
    assert.equal(lines.length, 3);
    assert.ok(lines[0].includes("[System]"));
    assert.equal(lines[1], "line 1");
    assert.ok(lines[2].includes("[System]"), "the lagging milestone must land after the live line, not be misfiled as history above it");
  });
});

test("the truncated banner clears once a historical range is cleared back to a live tail, not just when truncated itself resets", async () => {
  // A realistic truncated:true response: exactly LOG_TAIL_LIMIT (2000)
  // entries, newest-first, not an empty page - the store only ever sets
  // Truncated when it actually filled the requested limit.
  const entries = Array.from({ length: 2000 }, (_, i) => ({ id: 2000 - i, instance: "inst-1", node: "node-1", level: "", source: "instance", time_ms: 3000 - i, line: `line ${i}` }));
  globalThis.fetch = (async () => new Response(
    JSON.stringify({ items: entries, truncated: true }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  )) as typeof fetch;

  const view = render(
    <I18nextProvider i18n={i18n}>
      <LogStream uuid="inst-1" live={true} follow={true} range={{ kind: "quick", key: "1h" }} />
    </I18nextProvider>,
  );
  await waitFor(() => assert.ok(view.container.textContent?.includes("More than")));

  // Clearing the range (range: undefined) on a still-live instance switches
  // the view back to the live tail (historical becomes false); the
  // historical-path effect that set truncated=true never fires again to
  // reset it, so the banner must be derived from historical too, not
  // truncated alone.
  view.rerender(
    <I18nextProvider i18n={i18n}>
      <LogStream uuid="inst-1" live={true} follow={true} />
    </I18nextProvider>,
  );
  await waitFor(() => assert.equal(view.container.textContent?.includes("More than"), false));
});
