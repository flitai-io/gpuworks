import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["monitoring", "common"]);
// After createI18n: a static import pulls react-dom in before the JSDOM
// globals exist, and react-dom decides then whether the DOM has native
// input events (tests/helpers/reactDomOrder.ts).
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: RequestLookup } = await import("../src/pages/monitor/RequestLookup.tsx");
const { default: RequestDetailDrawer } = await import("../src/pages/monitor/RequestDetailDrawer.tsx");
const { resetTraceLinkCache } = await import("../src/pages/monitor/useTraceLink.ts");
import type { MonitorRequest, MonitorRequestLookup, MonitorRequestLookupQuery } from "../src/monitorRequestsApi";

const lk = locale.monitoring.lookup as Record<string, string>;
const original = { lookup: api.monitorRequestLookup, telemetry: api.telemetryExport };
afterEach(() => {
  cleanup();
  api.monitorRequestLookup = original.lookup;
  api.telemetryExport = original.telemetry;
  resetTraceLinkCache();
});
after(() => closeChannels());

const TRACE = "4bf92f3577b34da6a3ce929d0e0e4736";

function request(over: Partial<MonitorRequest>): MonitorRequest {
  return {
    id: "r", trace_id: TRACE, deployment_id: 7, received_ms: Date.UTC(2026, 9, 15, 8, 0, 0), phase: "done", age_ms: null,
    instance_uuid: "a1b2c3d4e5f6", api_key_id: 3, api_key_name: "agent", user_id: 1, source: "api", stream: true, body_bytes: 900,
    images: 0, dispatch_ms: 2, first_byte_ms: 20, first_think_ms: null, first_answer_ms: 300, last_chunk_ms: 900, end_ms: 905,
    events: 30, think_events: 0, max_gap_ms: 40, status: 200, outcome: "ok", finish_reason: "stop", prompt_tokens: 800,
    cached_tokens: 600, completion_tokens: 40, reasoning_tokens: null, engine_queue_ms: null, engine_prefill_ms: null,
    engine_decode_ms: null, weight: 1, result: "completed", segments: [{ kind: "gateway", ms: 2 }, { kind: "waiting", ms: 298 }, { kind: "answering", ms: 605 }],
    ...over,
  };
}

function answer(over: Partial<MonitorRequestLookup>): MonitorRequestLookup {
  return { as_of: 1, from_ms: Date.UTC(2026, 9, 15, 7, 0), to_ms: Date.UTC(2026, 9, 15, 8, 0), window: "default", requests: [], deployments: [], ...over };
}

function stubLookup(answers: (q: MonitorRequestLookupQuery) => Promise<MonitorRequestLookup>) {
  const calls: MonitorRequestLookupQuery[] = [];
  api.monitorRequestLookup = async (q) => {
    calls.push(q);
    return answers(q);
  };
  api.telemetryExport = async () => {
    throw new Error("no telemetry");
  };
  return calls;
}

function renderLookup() {
  const user = userEvent.setup();
  render(
    <MemoryRouter>
      <I18nextProvider i18n={i18n}>
        <RequestLookup />
      </I18nextProvider>
    </MemoryRouter>,
  );
  return user;
}
const box = () => screen.getByRole("searchbox", { name: lk.label });

test("a trace id finds the trace's requests across deployments, each naming its deployment", async () => {
  const calls = stubLookup(async () =>
    answer({
      window: "trace_time",
      requests: [request({ id: "b", deployment_id: 9 }), request({ id: "a", deployment_id: 7 }), request({ id: "c", deployment_id: 5 })],
      deployments: [{ id: 9, name: "", deleted: true }, { id: 7, name: "qwen", deleted: false }, { id: 5, name: "org/old", deleted: true }],
    }),
  );
  const user = renderLookup();
  await user.type(box(), ` ${TRACE.toUpperCase()} {Enter}`);
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], { traceId: TRACE.toUpperCase() });
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText((locale.monitoring.lookup.title as Record<string, string>).trace));
  assert.ok(within(drawer).getByText(/around the time the trace id carries · 3 found/));
  const rows = within(within(drawer).getByRole("list", { name: lk.results })).getAllByRole("listitem");
  assert.equal(rows.length, 3);
  assert.ok(within(rows[0]).getByText(/#9/), "a deployment deleted before names were kept is named by id");
  assert.ok(within(rows[1]).getByText(/qwen/));
  assert.ok(within(rows[2]).getByText(/org\/old \(deleted\)/), "a deleted deployment is named by the name it left");
  // A result opens its details, naming the deployment with a link to its page.
  await user.click(rows[1]);
  const details = await screen.findByText("Request details");
  const panel = details.closest(".ant-drawer-content") as HTMLElement;
  const link = within(panel).getByRole("link", { name: "qwen" });
  assert.equal(link.getAttribute("href"), "/monitor/d/7");
  assert.ok(within(panel).queryByText("Deleted") === null);
  assert.ok(within(panel).getByText(/finish_reason only marks the end of generation/));
  // The gateway boundary caveat is behind the help mark, not on the page.
  assert.equal(within(panel).queryByText(/third-party gateway/), null);
  assert.ok(within(panel).getByRole("button", { name: "More information" }));
  assert.equal(within(panel).getByRole("link", { name: "Collect streaming evidence" }).getAttribute("href"), "/settings#diagnostics");
  // A deleted deployment's details name it, marked deleted, still linking to
  // its page (which says it was deleted).
  await user.click(rows[2]);
  await waitFor(() => assert.ok(within(panel).getByRole("link", { name: "org/old" })));
  assert.equal(within(panel).getByRole("link", { name: "org/old" }).getAttribute("href"), "/monitor/d/5");
  assert.ok(within(panel).getByText("Deleted"));
});

test("anything else is looked up as a request id", async () => {
  const calls = stubLookup(async () => answer({ requests: [request({ id: "gw/req-1" })], deployments: [{ id: 7, name: "qwen", deleted: false }] }));
  const user = renderLookup();
  await user.type(box(), "gw/req-1{Enter}");
  await waitFor(() => assert.deepEqual(calls, [{ requestId: "gw/req-1" }]));
  assert.ok(await screen.findByText((locale.monitoring.lookup.title as Record<string, string>).request));
});

test("nothing found explains why and widens to the last day", async () => {
  const calls = stubLookup(async (q) => (q.from ? answer({ window: "requested", from_ms: q.from, to_ms: q.to!, requests: [request({ id: "found-later" })], deployments: [{ id: 7, name: "qwen", deleted: false }] }) : answer({})));
  const user = renderLookup();
  await user.type(box(), `${TRACE}{Enter}`);
  assert.ok(await screen.findByText(lk.none));
  assert.ok(screen.getByText(lk.noneHelp));
  await user.click(screen.getByRole("button", { name: lk.widen }));
  await waitFor(() => assert.equal(calls.length, 2));
  assert.equal(calls[1].traceId, TRACE);
  assert.equal(calls[1].to! - calls[1].from!, 24 * 60 * 60 * 1000);
  assert.ok(await screen.findByText(/1 found/));
});

test("a failed lookup says why and retries; an empty box does nothing", async () => {
  let fail = true;
  const calls = stubLookup(async () => {
    if (fail) throw new APIError("MONITOR_REQUESTS_QUERY_INVALID", undefined, "the window is longer than 24h0m0s", 400);
    return answer({ requests: [request({})], deployments: [{ id: 7, name: "qwen", deleted: false }] });
  });
  const user = renderLookup();
  await user.type(box(), "   {Enter}");
  assert.equal(calls.length, 0);
  await user.type(box(), "x{Enter}");
  assert.ok(await screen.findByText(lk.failed));
  fail = false;
  await user.click(screen.getByRole("button", { name: /Retry/ }));
  assert.ok(await screen.findByText(/1 found/));
  assert.equal(calls.length, 2);
});

for (const lng of ["en", "zh"] as const) {
  test(`${lng}: request evidence explains the gateway boundary and offers diagnostic collection`, async () => {
    const local = await createI18n(["monitoring", "common"], lng);
    api.telemetryExport = async () => { throw new Error("not configured"); };
    render(<MemoryRouter><I18nextProvider i18n={local.i18n}><RequestDetailDrawer request={request({})} onClose={() => {}} /></I18nextProvider></MemoryRouter>);
    const t = (key: string) => local.i18n.t(`requests.drawer.${key}`, { ns: "monitoring" });
    assert.ok(await screen.findByText(t("observationTitle")));
    // The boundary caveat sits behind the help mark beside the title, not on the page.
    assert.equal(screen.queryByText(t("observationScopeHelp")), null);
    await userEvent.setup().hover(screen.getByRole("button", { name: local.i18n.t("helpTip.label", { ns: "common" }) }));
    assert.ok(within(await screen.findByRole("tooltip")).getByText(t("observationScopeHelp")));
    assert.ok(screen.getByText(t("streamingEvidence")));
    assert.equal(screen.getByRole("link", { name: t("collectEvidence") }).getAttribute("href"), "/settings#diagnostics");
  });
}
