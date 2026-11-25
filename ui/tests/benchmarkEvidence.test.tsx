import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { BenchmarkPoint, BenchmarkRun, BenchmarkPointEvidence, BenchmarkRequestEvidence, BenchmarkTokenDistribution } from "../src/api.ts";

const { i18n } = await createI18n(["evaluations"]);
const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: BenchmarkEvidencePanel } = await import("../src/pages/evaluations/BenchmarkEvidencePanel.tsx");
afterEach(cleanup);

const run = { id: 3 } as BenchmarkRun;
const emptyDistribution = (): BenchmarkTokenDistribution => ({ count: 0, total: 0, min: 0, max: 0, p50: 0, p99: 0 });
function group(): BenchmarkRequestEvidence {
  const dist = emptyDistribution;
  const metric = { count: 0, mean_ms: 0, p50_ms: 0, p99_ms: 0 };
  return {
    attempted: 0, success: 0, failed: 0, canceled: 0,
    ttft: metric, tpot: metric, last_output_latency: metric, completion: metric,
    tokens: { input: dist(), output: dist(), context: dist(), server_input: dist(), server_output: dist(), local_input: dist(), local_output: dist(), reasoning: dist(), failed_output: dist(), input_sources: {}, output_sources: {} },
    cache: { reported: 0, unknown: 0, ratio_requests: 0, successful_reported: 0, input_tokens: 0, cached_tokens: 0 },
    finish_reasons: {}, error_stages: {},
  };
}
function evidence(): BenchmarkPointEvidence {
  const all = group();
  all.attempted = 40; all.success = 39; all.failed = 1;
  all.cache = { reported: 36, unknown: 4, ratio_requests: 36, successful_reported: 36, input_tokens: 1000, cached_tokens: 100 };
  all.tokens.output = { count: 39, total: 1950, min: 50, max: 50, p50: 50, p99: 50 };
  all.finish_reasons = { stop: 39 }; all.error_stages = { http: 1 };
  const first = group(); first.attempted = 4; first.success = 3; first.failed = 1;
  first.ttft = { count: 3, mean_ms: 55000, p50_ms: 54000, p99_ms: 56000 };
  first.cache.unknown = 4;
  const follow = group(); follow.attempted = 36; follow.success = 36;
  follow.ttft = { count: 36, mean_ms: 1500, p50_ms: 1000, p99_ms: 2500 };
  return {
    version: 1, plan: { planned_sessions: 4, planned_requests: 52, turns_per_session: 13, output_budget: 220, output_policy: "natural" },
    outcomes: { skipped: 12, canceled_before_start: 0, completed_sessions: 3, failed_sessions: 1, canceled_sessions: 0, retry_count: 0 },
    all, first_turn: first, followup_turns: follow, by_turn: {},
    timing: { started_at: "2026-10-20T01:00:00Z", finished_at: "2026-10-20T01:01:00Z", completed_session_latency: { count: 3, mean_ms: 50000, p50_ms: 50000, p99_ms: 60000 } },
    execution: { seed: 42, seed_derivation: "cumulative_stride_v1", cache_policy: "unknown", cache_state: "unknown", retry_policy: "none", tokenizer_source: "provided", local_token_count_method: "local_tokenizer_estimate", detail_policy: "optional", warmup_requests: 1, warmup_succeeded: 1 },
    sessions: [{ session_id: "session-4", planned_turns: 13, attempted: 1, success: 0, failed: 1, canceled: 0, skipped: 12, state: "failed", started_at: "2026-10-20T01:00:00Z", finished_at: "2026-10-20T01:00:01Z" }],
    sessions_truncated: false,
  };
}
function panel(e?: BenchmarkPointEvidence, expanded = false) {
  const point = { seq: 1, detail_path: e ? "retained.jsonl.gz" : undefined, long_tail: e ? { evidence: e } : undefined } as BenchmarkPoint;
  return <I18nextProvider i18n={i18n}><BenchmarkEvidencePanel run={run} point={point} expanded={expanded} /></I18nextProvider>;
}
// The section starts collapsed; the reader opens it from its header.
async function show(e?: BenchmarkPointEvidence) {
  const view = render(panel(e));
  await userEvent.setup().click(screen.getByRole("button", { name: /Request and session evidence/ }));
  return view;
}

test("the section starts collapsed, opens from the keyboard, and a report capture shows it without interaction", async () => {
  const view = render(panel(evidence()));
  const header = screen.getByRole("button", { name: /Request and session evidence/ });
  assert.equal(header.getAttribute("aria-expanded"), "false");
  assert.equal(screen.queryByText("97.5%"), null);
  assert.equal(screen.queryByText("First turns"), null);
  // The download stays reachable while collapsed, and using it does not toggle the section.
  await userEvent.setup().click(screen.getByRole("link", { name: "Download request JSONL" }));
  assert.equal(header.getAttribute("aria-expanded"), "false");
  header.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(header.getAttribute("aria-expanded"), "true");
  assert.ok(screen.getByText("97.5%"));
  view.unmount();
  render(panel(evidence(), true));
  assert.ok(screen.getByText("97.5%"));
  cleanup();
  render(panel());
  assert.equal(screen.queryByText(/Historical identities and cache states cannot be reconstructed/), null);
});

test("the report distinguishes request success from complete sessions and explains skipped rounds", async () => {
  await show(evidence());
  assert.ok(screen.getByText("52"));
  assert.ok(screen.getByText("97.5%"));
  assert.ok(screen.getByText("75%"));
  assert.ok(screen.getByText("3 / 4"));
  assert.ok(screen.getByText("12"));
  const first = screen.getAllByText("First turns")[0].closest("tr")!;
  const follow = screen.getAllByText("Follow-up turns")[0].closest("tr")!;
  assert.ok(within(first).getByText("54,000 / 56,000 ms · n=3"));
  assert.ok(within(follow).getByText("1,000 / 2,500 ms · n=36"));
  assert.ok(screen.getByRole("link", { name: "Download request JSONL" }).getAttribute("href")?.endsWith("/3/points/1/detail"));
  const user = userEvent.setup();
  const sessions = screen.getByRole("button", { name: /Session records/ });
  sessions.focus();
  await user.keyboard("{Enter}");
  assert.ok(await screen.findByText("session-4"));
  assert.ok(screen.getByText("0 / 13"));
});

test("cache coverage and weighted usage are separate and an unreported group stays unknown", async () => {
  await show(evidence());
  const rows = screen.getAllByText("All requests");
  const cache = rows[1].closest("tr")!;
  assert.ok(within(cache).getByText("36 / 40"));
  assert.ok(within(cache).getByText("90%"));
  assert.ok(within(cache).getByText("10%"));
  const firstCache = screen.getAllByText("First turns")[1].closest("tr")!;
  assert.ok(within(firstCache).getByText("Unknown"));
  assert.ok(screen.getByText("Natural finish"));
  assert.ok(screen.getByText("220"));
  assert.ok(screen.getByText(/Local counts are estimates/));
});

test("legacy and future evidence versions cannot manufacture a zero completion rate", async () => {
  const view = await show();
  assert.ok(screen.getByText(/Historical identities and cache states cannot be reconstructed/));
  assert.equal(screen.queryByText("0%"), null);
  view.unmount();
  await show({ ...evidence(), version: 99 });
  assert.ok(screen.getByText(/no supported session or round evidence/));
  assert.equal(screen.queryByText("75%"), null);
});

test("first visible content is shown beside TTFT and reasoning-only requests are counted, not hidden", async () => {
  const e = evidence();
  e.followup_turns.first_content = { count: 30, mean_ms: 4100, p50_ms: 4000, p99_ms: 6000 };
  e.followup_turns.no_content = 6;
  await show(e);
  assert.ok(screen.getByRole("columnheader", { name: "First content" }));
  const follow = screen.getAllByText("Follow-up turns")[0].closest("tr")!;
  assert.ok(within(follow).getByText("4,000 / 6,000 ms · n=30 · 6 without content"));
  // Evidence recorded before first content was summarized has neither field.
  const first = screen.getAllByText("First turns")[0].closest("tr")!;
  assert.equal(within(first).getAllByText("Unknown").length >= 1, true);
});
