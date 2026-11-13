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

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { monitoring: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8")) } },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: SignalTile, formatSignalValue } = await import("../src/pages/monitor/SignalTile.tsx");
const { colors } = await import("../src/theme/tokens.ts");
const { formatLatency, formatLatencyText, latencyUnit } = await import("../src/pages/monitor/signalFormat.ts");
import type { MonitorSignal } from "../src/api";

afterEach(() => cleanup());

function renderTile(
  signal: MonitorSignal,
  onClick?: () => void,
  unit?: string,
  labels?: { valueLabel?: string; denominatorLabel?: string },
) {
  return render(
    <I18nextProvider i18n={i18n}>
      <SignalTile
        signal={signal}
        label="Ready replicas"
        unit={unit}
        onClick={onClick}
        valueLabel={labels?.valueLabel}
        denominatorLabel={labels?.denominatorLabel}
      />
    </I18nextProvider>,
  );
}

test("a collected signal renders its value, denominator and aria-label", () => {
  const signal: MonitorSignal = { key: "ready_replicas", collected: true, value: 9, denominator: 11 };
  renderTile(signal);
  assert.equal(screen.getByText("9 / 11").textContent, "9 / 11");
  assert.ok(screen.getByRole("button", { name: "Ready replicas: 9 / 11" }));
});

test("collected:false renders the muted not-collected state, never a fabricated 0", () => {
  const signal: MonitorSignal = { key: "error_rate", collected: false };
  renderTile(signal);
  assert.equal(screen.getByText("Not collected").textContent, "Not collected");
  assert.equal(screen.queryByText("0"), null);
  assert.ok(screen.getByRole("button", { name: "Ready replicas: Not collected" }));
});

test("collected:false is aria-disabled and ignores clicks - there is nothing to filter by", async () => {
  let clicks = 0;
  const signal: MonitorSignal = { key: "error_rate", collected: false };
  renderTile(signal, () => clicks++);
  const button = screen.getByRole("button");
  assert.equal(button.getAttribute("aria-disabled"), "true");
  await userEvent.setup().click(button);
  assert.equal(clicks, 0);
});

test("a collected signal has no aria-disabled attribute", () => {
  renderTile({ key: "waiting", collected: true, value: 14 });
  assert.equal(screen.getByRole("button").hasAttribute("aria-disabled"), false);
});

test("token_rate's value row shows the bare number pair - both numbers stay visible at 8 columns instead of being ellipsized", () => {
  // At 8 columns (>=1200px) the tile's inner width is only ~81-160px;
  // "Input 8420 / Output 5130 tok/s" measures 215-248px and would always
  // clip. The value row drops the labels/unit and shows just the numbers.
  const signal: MonitorSignal = { key: "token_rate", collected: true, value: 8420, denominator: 5130 };
  renderTile(signal, undefined, "tok/s", { valueLabel: "Input", denominatorLabel: "Output" });
  const valueRow = screen.getByTestId("signal-value-row");
  assert.equal(valueRow.textContent, "8420 / 5130");
  assert.ok(valueRow.textContent?.includes("8420"));
  assert.ok(valueRow.textContent?.includes("5130"));
  assert.equal(valueRow.textContent?.includes("tok/s"), false);
});

test("token_rate's detail row carries the Input/Output labels and the unit that the value row no longer shows", () => {
  const signal: MonitorSignal = { key: "token_rate", collected: true, value: 8420, denominator: 5130 };
  renderTile(signal, undefined, "tok/s", { valueLabel: "Input", denominatorLabel: "Output" });
  assert.equal(screen.getByTestId("signal-detail-row").textContent, "Input / Output tok/s");
});

test("token_rate's aria-label keeps the full Input/Output/unit wording even though the visible value row is compact", () => {
  const signal: MonitorSignal = { key: "token_rate", collected: true, value: 8420, denominator: 5130 };
  renderTile(signal, undefined, "tok/s", { valueLabel: "Input", denominatorLabel: "Output" });
  assert.ok(screen.getByRole("button", { name: "Ready replicas: Input 8420 / Output 5130 tok/s" }));
});

test("the token tile gets the compact-number modifier class for the 8-column layout", () => {
  const signal: MonitorSignal = { key: "token_rate", collected: true, value: 0, denominator: 0 };
  renderTile(signal, undefined, "tok/s", { valueLabel: "Input", denominatorLabel: "Output" });
  assert.ok(screen.getByRole("button").className.includes("monitor-signal-tile--token"));
});

test("a token_rate signal rendered without value/denominator labels renders the same bare pair but gets no detail caption or compact class", () => {
  const signal: MonitorSignal = { key: "token_rate", collected: true, value: 100, denominator: 50 };
  renderTile(signal, undefined, "tok/s");
  assert.equal(screen.getByTestId("signal-value-row").textContent, "100 / 50tok/s");
  assert.equal(screen.getByTestId("signal-detail-row").textContent, "");
  assert.equal(screen.getByRole("button").className.includes("monitor-signal-tile--token"), false);
});

test("every tile renders the label, value, detail and sparkline rows regardless of state", () => {
  const states: MonitorSignal[] = [
    { key: "waiting", collected: true, value: 3 },
    { key: "waiting", collected: false },
    { key: "waiting", collected: true, value: 0, detail: { no_recent_requests: true } },
  ];
  for (const signal of states) {
    const { unmount } = renderTile(signal);
    assert.ok(screen.getByTestId("signal-value-row"), `value row missing for ${JSON.stringify(signal)}`);
    assert.ok(screen.getByTestId("signal-detail-row"), `detail row missing for ${JSON.stringify(signal)}`);
    assert.ok(screen.getByTestId("signal-spark-slot"), `spark slot missing for ${JSON.stringify(signal)}`);
    // The sparkline slot always holds an <svg>, even with no spark data -
    // it must never collapse to an empty, height-0 slot.
    assert.ok(screen.getByTestId("signal-spark-slot").querySelector("svg"));
    unmount();
  }
});

test("detail.no_recent_requests is an annotation, not a value suppressor: waiting=42 still shows 42, with the sentence in the detail row", () => {
  // An engine driven directly on its own port can have a real, nonzero
  // waiting/preemptions/request_rate/token_rate/error_rate value while the
  // proxy in front of it has seen no traffic in the last 5 minutes - the
  // number must stay visible, not be replaced by the sentence.
  const signal: MonitorSignal = { key: "waiting", collected: true, value: 42, detail: { no_recent_requests: true } };
  renderTile(signal);
  assert.equal(screen.getByTestId("signal-value-row").textContent, "42");
  assert.equal(screen.getByTestId("signal-detail-row").textContent, "No recent requests");
  assert.equal(screen.queryByText("Not collected"), null);
  assert.ok(screen.getByRole("button", { name: "Ready replicas: 42 (No recent requests)" }));
});

test("no_recent_requests stays collected: it remains clickable", async () => {
  let clicks = 0;
  const signal: MonitorSignal = { key: "waiting", collected: true, value: 42, detail: { no_recent_requests: true } };
  renderTile(signal, () => clicks++);
  const button = screen.getByRole("button");
  assert.equal(button.hasAttribute("aria-disabled"), false);
  await userEvent.setup().click(button);
  assert.equal(clicks, 1);
});

test("ttft/tpot are the one exception: the server omits value/denominator on no_recent_requests, so the sentence replaces the value row and the detail row stays empty", () => {
  const signal: MonitorSignal = { key: "ttft", collected: true, detail: { no_recent_requests: true, upper_bound: true } };
  renderTile(signal);
  assert.equal(screen.getByTestId("signal-value-row").textContent, "No recent requests");
  assert.equal(screen.getByTestId("signal-detail-row").textContent, "");
  assert.equal(screen.queryByText("0"), null);
  assert.ok(screen.getByRole("button", { name: "Ready replicas: No recent requests" }));
});

test("formatSignalValue shows at most one decimal and drops a trailing .0", () => {
  assert.equal(formatSignalValue(9), "9");
  assert.equal(formatSignalValue(2.9), "2.9");
  assert.equal(formatSignalValue(8420), "8420");
  // Floating-point noise that rounds back to a whole number never leaves a
  // trailing ".0" behind.
  assert.equal(formatSignalValue(3.0000000000000004), "3");
  assert.equal(formatSignalValue(0.05), "0.1");
});

test("a tile passes no verdict: no status dot; only fewer replicas ready than wanted is marked", () => {
  const { container, unmount } = renderTile({ key: "waiting", collected: true, value: 3 });
  assert.equal(container.querySelector('span[aria-hidden="true"]'), null, "no status dot");
  unmount();
  const short = renderTile({ key: "ready_replicas", collected: true, value: 9, denominator: 11 });
  const number = short.container.querySelector(".monitor-signal-tile__value-number") as HTMLElement;
  // The warning text colour comes from the theme, so the check follows it.
  const expected = document.createElement("span");
  expected.style.color = colors.warningText;
  assert.equal(number.style.color, expected.style.color);
  assert.notEqual(number.style.color, "");
  short.unmount();
  const full = renderTile({ key: "ready_replicas", collected: true, value: 11, denominator: 11 });
  assert.equal((full.container.querySelector(".monitor-signal-tile__value-number") as HTMLElement).style.color, "");
  full.unmount();
});

test("clicking the tile invokes onClick", async () => {
  let clicks = 0;
  renderTile({ key: "waiting", collected: true, value: 14 }, () => clicks++);
  await userEvent.setup().click(screen.getByRole("button"));
  assert.equal(clicks, 1);
});

test("the tile is keyboard operable via Enter and Space", async () => {
  let clicks = 0;
  renderTile({ key: "waiting", collected: true, value: 14 }, () => clicks++);
  const button = screen.getByRole("button");
  button.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(clicks, 1);
  await userEvent.setup().keyboard("[Space]");
  assert.equal(clicks, 2);
});

test("request_rate shows the rate and the in-flight concurrency as a bare pair, with no units to crowd the tile", () => {
  const signal: MonitorSignal = { key: "request_rate", collected: true, value: 5, denominator: 20 };
  renderTile(signal);
  assert.equal(screen.getByTestId("signal-value-row").textContent, "5 / 20");
  assert.ok(screen.getByRole("button", { name: "Ready replicas: 5 / 20" }));
});

test("request_rate without an in-flight gauge shows the rate alone", () => {
  const signal: MonitorSignal = { key: "request_rate", collected: true, value: 5 };
  renderTile(signal);
  assert.equal(screen.getByTestId("signal-value-row").textContent, "5");
});

test("latencies switch to seconds from 1 s, and a pair shares the unit of its larger value", () => {
  assert.equal(latencyUnit(999), "ms");
  assert.equal(latencyUnit(1000), "s");
  assert.equal(latencyUnit(800, 1200), "s");
  assert.equal(latencyUnit(null, undefined), "ms");
  assert.equal(formatLatency(39000, "s"), "39");
  assert.equal(formatLatency(800, "s"), "0.8");
  assert.equal(formatLatency(13.34, "ms"), "13.3");
  assert.equal(formatLatencyText(39000), "39s");
  assert.equal(formatLatencyText(1530), "1.5s");
  assert.equal(formatLatencyText(345.6), "345.6 ms");
});

test("a latency pair in seconds renders without five-digit millisecond counts", () => {
  const signal: MonitorSignal = { key: "ttft", collected: true, value: 38000, denominator: 39500 };
  render(
    <I18nextProvider i18n={i18n}>
      <SignalTile signal={signal} label="TTFT p95 / p99" unit="s" formatValue={(v) => formatLatency(v, "s")} />
    </I18nextProvider>,
  );
  assert.equal(screen.getByTestId("signal-value-row").textContent, "38 / 39.5s");
  assert.ok(screen.getByRole("button", { name: "TTFT p95 / p99: 38 / 39.5 s" }));
});
