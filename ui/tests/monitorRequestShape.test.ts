import assert from "node:assert/strict";
import { test } from "node:test";
import type { MonitorRequest } from "../src/monitorRequestsApi";
const {
  formatMS, formatTokens, gridView, liveEventRate, requestPhases, shareOf, tokenBucketLabel, ttftBucketLabel,
} = await import("../src/pages/monitor/requestShape.ts");

const BUCKETS = [0, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048];

function request(over: Partial<MonitorRequest>): MonitorRequest {
  return {
    id: "r", received_ms: 0, phase: "done", age_ms: null, instance_uuid: "", api_key_id: 0, api_key_name: "", user_id: 0,
    source: "api", stream: true, body_bytes: 0, images: 0, dispatch_ms: null, first_byte_ms: null, first_think_ms: null,
    first_answer_ms: null, last_chunk_ms: null, end_ms: null, events: 0, think_events: 0, max_gap_ms: null, status: 200,
    outcome: "ok", finish_reason: "stop", prompt_tokens: null, cached_tokens: null, completion_tokens: null,
    reasoning_tokens: null, engine_queue_ms: null, engine_prefill_ms: null, engine_decode_ms: null, ttft_ms: null, tpot_ms: null, prefill_tok_s: null, decode_tok_s: null, weight: 1, result: "completed",
    segments: [], ...over,
  };
}

test("token and time labels", () => {
  assert.equal(formatTokens(512), "512");
  assert.equal(formatTokens(1536), "1.5K");
  assert.equal(formatTokens(32768), "32K");
  assert.equal(formatTokens(1 << 20), "1M");
  assert.equal(formatMS(420), "420 ms");
  assert.equal(formatMS(2500), "2.5 s");
  assert.equal(formatMS(90_000), "1.5 min");
  assert.equal(tokenBucketLabel(BUCKETS, 10), "1K–2K");
  assert.equal(tokenBucketLabel(BUCKETS, 11), "≥ 2K");
  const ttft = [100, 250, 500];
  assert.equal(ttftBucketLabel(ttft, 0), "< 100 ms");
  assert.equal(ttftBucketLabel(ttft, 1), "100 ms–250 ms");
  assert.equal(ttftBucketLabel(ttft, 3), "> 500 ms");
  assert.equal(shareOf(1, 3), "33%");
  assert.equal(shareOf(1, 300), "0.3%");
  assert.equal(shareOf(1, 100000), "<0.1%");
  assert.equal(shareOf(1, 0), "0%");
  assert.equal(shareOf(0, 10), "0%");
});

test("gridView trims to the occupied buckets and sums repeated cells", () => {
  const view = gridView([{ x: 3, y: 5, n: 2 }, { x: 5, y: 4, n: 7 }, { x: 3, y: 5, n: 1 }, { x: 9, y: 9, n: 0 }]);
  assert.deepEqual(view.xs, [3, 4, 5]);
  assert.deepEqual(view.ys, [4, 5]);
  assert.equal(view.counts.get("3:5"), 3);
  assert.equal(view.total, 10);
  assert.equal(view.max, 7);
  assert.deepEqual(gridView([]).xs, []);
});

// The split itself is the server's (uxrecord.SegmentsOf, tested in Go); the
// page reads it and derives the totals.
test("phases read the server's segments: total, first token, empty segments dropped", () => {
  const done = requestPhases(request({
    first_think_ms: 300, first_answer_ms: 1300, end_ms: 3310,
    segments: [
      { kind: "gateway", ms: 2 }, { kind: "waiting", ms: 298 }, { kind: "queue", ms: 0 },
      { kind: "thinking", ms: 1000 }, { kind: "answering", ms: 2000 }, { kind: "tail", ms: 10 },
    ],
  }));
  assert.deepEqual(done.segments.map((s) => s.kind), ["gateway", "waiting", "thinking", "answering", "tail"]);
  assert.equal(done.totalMS, 3310);
  assert.equal(done.firstTokenMS, 300);
  // A reply without tokens waited up to its end.
  assert.equal(requestPhases(request({ stream: false, segments: [{ kind: "waiting", ms: 800 }] })).firstTokenMS, 800);
  // A live request that still waits has no first token yet.
  const waiting = requestPhases(request({ phase: "waiting", age_ms: 4000, segments: [{ kind: "gateway", ms: 1 }, { kind: "waiting", ms: 3999 }] }));
  assert.equal(waiting.totalMS, 4000);
  assert.equal(waiting.firstTokenMS, null);
});

test("request view URLs carry only the set filters", async () => {
  const { monitorDistributionsURL, monitorRequestsURL } = await import("../src/monitorRequestsApi.ts");
  assert.equal(monitorRequestsURL(7, { view: "live", limit: 20, key: 0 }), "/api/v1/monitor/deployments/7/requests?view=live&limit=20");
  assert.equal(
    monitorRequestsURL(7, { view: "history", range: "6h", limit: 20, key: 3, instance: "i-1", results: ["length", "error"], minMs: 1500.4, maxMs: 9000, minTtftMs: 300, maxTtftMs: 2500, cursor: "a b" }),
    "/api/v1/monitor/deployments/7/requests?view=history&range=6h&limit=20&key=3&instance=i-1&results=length%2Cerror&min_ms=1500&max_ms=9000&min_ttft_ms=300&max_ttft_ms=2500&cursor=a+b",
  );
  // An explicit range is sent only when both ends are set, and replaces the
  // preset; a picked page range is sent as from/to.
  assert.equal(
    monitorRequestsURL(7, { view: "history", range: "1800000000000~1800003600000" }),
    "/api/v1/monitor/deployments/7/requests?view=history&from=1800000000000&to=1800003600000",
  );
  assert.equal(
    monitorRequestsURL(7, { view: "history", range: "1h", from: 1000, to: 2000 }),
    "/api/v1/monitor/deployments/7/requests?view=history&from=1000&to=2000",
  );
  assert.equal(monitorRequestsURL(7, { view: "history", from: 1000 }), "/api/v1/monitor/deployments/7/requests?view=history");
  assert.equal(monitorDistributionsURL("1h"), "/api/v1/monitor/distributions?range=1h");
  assert.equal(monitorDistributionsURL("24h", 9), "/api/v1/monitor/distributions?range=24h&deployment=9");
});


test("a live stream's chunk rate counts the chunks after the first over the time since it", () => {
  assert.equal(liveEventRate(request({ phase: "answering", events: 11, first_answer_ms: 0, last_chunk_ms: 1000 })), 10);
  assert.equal(liveEventRate(request({ phase: "waiting", events: 0 })), null);
});
