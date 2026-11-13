import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import type { PeerTransferSnapshot } from "../src/peerTransferTypes.ts";

const { i18n, locale } = await createI18n(["common"]);
const copy = locale.common.peerTransfer;
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { PeerTransferDetails } = await import("../src/components/peerTransfer/PeerTransferDetails.tsx");
const original = api.peerTransfers;
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); api.peerTransfers = original; globalThis.fetch = originalFetch; });
const wire = `{
  "observed_at":"2026-11-11T04:00:00Z","scope":"server_process",
  "policy":{"rate_limit_bps":67108864,"burst_bytes":65536,"max_active":2,"max_per_node":1,"no_progress_timeout_seconds":120,"confirmation_timeout_seconds":300},
  "active_relay_count":1,"rate_waiting_count":0,
  "totals":{"started":1,"peer_confirmed":0,"failed":0,"canceled":0,"busy":0,"probe_started":0,"probe_ok":0,"probe_failed":0,"source_to_target_bytes":1048576,"target_to_source_bytes":128,"rate_wait_ms":1500},
  "active":[{"transfer_id":"a/b","kind":"model","operation":"copy","source_node_uuid":"source","target_node_uuid":"target","transport":"relay","phase":"transferring","fallback_reason":"direct_dial_failed","source_to_target_bytes":1048576,"target_to_source_bytes":128,"effective_rate_limit_bps":33554432,"server_rate_bps":8388608,"rate_window_seconds":5,"rate_wait_ms":1500}]
}`;
const snapshot = (): PeerTransferSnapshot => JSON.parse(wire);
function show() { return render(<I18nextProvider i18n={i18n}><PeerTransferDetails transferId="a/b" /></I18nextProvider>); }

test("peer observation client reads the snapshot route and encodes its attempt filter", async () => {
  const seen: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push(String(input)); assert.ok(!init?.method || init.method === "GET");
    return new Response(wire, { headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  assert.deepEqual(await api.peerTransfers("a/b"), snapshot());
  await api.peerTransfers();
  assert.deepEqual(seen, ["/api/v1/peer-transfers?transfer_id=a%2Fb", "/api/v1/peer-transfers"]);
});

test("loading becomes an observed relay with separate forwarding accounting", async () => {
  let resolve!: (value: PeerTransferSnapshot) => void;
  api.peerTransfers = () => new Promise((done) => { resolve = done; });
  show(); assert.ok(screen.getByText(copy.loading));
  resolve(snapshot()); await screen.findByText(copy.relay);
  for (const value of [copy.phases.transferring, copy.reasons.direct_dial_failed, "8.0 MiB/s", "1.0 MiB", "128 B", "32.0 MiB/s", copy.history]) assert.ok(screen.getByText(value));
  // What the server counters measure is behind the help mark beside the budget.
  assert.equal(screen.queryByText(copy.accountingHelp), null);
  await userEvent.setup().hover(screen.getByRole("button", { name: locale.common.helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, copy.accountingHelp);
});

test("missing evidence remains unknown and does not invent zero progress", async () => {
  api.peerTransfers = async () => ({ ...snapshot(), active: [] });
  show(); await screen.findByText(copy.unknown);
  for (const value of [copy.relay, "0 B/s", copy.forwarded]) assert.equal(screen.queryByText(value), null);
});

test("direct transport does not show server traffic as model progress", async () => {
  const data = snapshot(); data.active[0] = { ...data.active[0], transport: "direct", phase: "confirming", source_to_target_bytes: 0, target_to_source_bytes: 0 };
  api.peerTransfers = async () => data;
  show(); await screen.findByText(copy.direct); assert.ok(screen.getByText(copy.phases.confirming));
  for (const value of [copy.forwarded, "0 B/s"]) assert.equal(screen.queryByText(value), null);
});

test("capacity wait does not claim an established relay", async () => {
  const data = snapshot(); data.active[0] = { ...data.active[0], transport: undefined, phase: "waiting_capacity", reason: "capacity" };
  api.peerTransfers = async () => data;
  show(); await screen.findByText(copy.phases.waiting_capacity);
  assert.ok(screen.getByText(copy.unknownTransport)); assert.ok(screen.getByText(copy.reasons.capacity));
  assert.equal(screen.queryByText(copy.relay), null);
});

test("keyboard refresh reports unavailable evidence without retaining a stale path", async () => {
  let calls = 0;
  api.peerTransfers = async () => { if (++calls === 1) return snapshot(); throw new Error("offline"); };
  show(); await screen.findByText(copy.relay);
  screen.getByRole("button", { name: copy.refresh }).focus();
  await userEvent.setup({ document }).keyboard("{Enter}");
  await screen.findByText(copy.unavailable);
  assert.equal(screen.queryByText(copy.relay), null); assert.equal(calls, 2);
});

test("persisted busy wait stays visible between attempts without inventing a transport", async () => {
 api.peerTransfers = async () => ({ ...snapshot(), active: [] });
 render(<I18nextProvider i18n={i18n}><PeerTransferDetails transferId="a/b" transferErrorCode="PEER_RELAY_BUSY" /></I18nextProvider>);
 await screen.findByText(copy.phaseWaiting);
 assert.equal(screen.queryByText(copy.unknown), null);
 assert.equal(screen.queryByText(copy.relay), null);
 assert.equal(screen.queryByText(copy.forwarded), null);
});
