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

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { ai: read("ai"), common: read("common") } },
  defaultNS: "ai",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { Drawer } = await import("antd");
const { default: AIAssistant } = await import("../src/components/AIAssistant.tsx");
const { default: AssistantPanel } = await import("../src/assistant/AssistantPanel.tsx");
const { AssistantProvider, useAssistantSubjects } = await import("../src/assistant/AssistantContext.tsx");
const { stepDetail, StepList, selfCorrectable } = await import("../src/components/AIAssistantParts.tsx");
type PageSubject = import("../src/assistant/AssistantContext.tsx").PageSubject;
const originalFetch = globalThis.fetch;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
});

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

type Route = (body: unknown, init?: RequestInit) => Promise<Response> | Response;

// fakeServer answers the assistant's endpoints the way the server does,
// recording every ask's body and Accept header.
function fakeServer(routes: { findings?: Route; fit?: Route; ask?: Route }) {
  const asked: { body: Record<string, unknown>; accept: string }[] = [];
  const facts: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (url.endsWith("/findings") || url.endsWith("/fit")) facts.push(url);
    if (url.endsWith("/findings") && routes.findings) return routes.findings(body, init);
    if (url.endsWith("/fit") && routes.fit) return routes.fit(body, init);
    if (url === "/api/v1/ai/ask" && routes.ask) {
      asked.push({ body, accept: String((init?.headers as Record<string, string>)?.Accept ?? "") });
      return routes.ask(body, init);
    }
    return json({ error: `unexpected ${url}` }, 500);
  }) as typeof fetch;
  return Object.assign(asked, { facts });
}

const frame = (event: string, data: unknown) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

function sse(...frames: string[]): Response {
  return new Response(frames.join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
}

// liveStream is an answer the test feeds frame by frame; aborting the request
// errors the body, as a real fetch does.
function liveStream() {
  let ctrl!: ReadableStreamDefaultController<Uint8Array>;
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({ start(c) { ctrl = c; } });
  return {
    respond: (_: unknown, init?: RequestInit) => {
      init?.signal?.addEventListener("abort", () => { try { ctrl.error(new DOMException("aborted", "AbortError")); } catch { /* closed */ } });
      return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
    },
    push: (event: string, data: unknown) => ctrl.enqueue(enc.encode(frame(event, data))),
    ping: () => ctrl.enqueue(enc.encode(": ping\n\n")),
    close: () => ctrl.close(),
  };
}

const done = { calls: 2, usage: { prompt_tokens: 3000, completion_tokens: 200 }, first_text_ms: 1800, total_ms: 6900 };
const answerFrames = [
  frame("step", { id: 0, tool: "call_api", args: '{"path":"/api/v1/logs/search","query":{"q":"KV cache","level":"error"}}', status: "running", ms: 0 }),
  frame("step", { id: 0, tool: "call_api", args: '{"path":"/api/v1/logs/search","query":{"q":"KV cache","level":"error"}}', status: "done", ms: 12 }),
  frame("text", { delta: "**Root cause**: the KV cache " }),
  frame("text", { delta: "does not fit." }),
  frame("done", done),
];

function Page({ subjects }: { subjects: PageSubject[] | null }) {
  useAssistantSubjects(subjects);
  return null;
}

function shell(children: React.ReactNode) {
  return render(<I18nextProvider i18n={i18n}><AssistantProvider>{children}<AssistantPanel /></AssistantProvider></I18nextProvider>);
}

const instanceSubject = { kind: "instance" as const, uuid: "inst-1", label: "inst-1" };

function renderInstance() {
  return shell(<AIAssistant task="instance.diagnose" subject={instanceSubject} buttonText="AI diagnose" />);
}

test("closing and reopening the assistant keeps the sibling detail and the unsent question", async () => {
  shell(<Drawer open title="Deployment detail" onClose={() => assert.fail("assistant interaction closed the detail")}>Deployment facts</Drawer>);
  fireEvent.keyDown(window, { key: "i", ctrlKey: true });
  const question = await screen.findByRole("textbox", { name: "Question" });
  fireEvent.change(question, { target: { value: "Why is this deployment waiting?" } });
  const assistant = screen.getByRole("dialog", { name: /AI Assistant/ });
  fireEvent.click(within(assistant).getByRole("button", { name: "Close" }));
  await waitFor(() => assert.equal(document.body.classList.contains("assistant-open"), false));
  assert.ok(screen.getByRole("dialog", { name: "Deployment detail" }));
  fireEvent.keyDown(window, { key: "i", ctrlKey: true });
  await waitFor(() => assert.equal(screen.getByRole("textbox", { name: "Question" }).getAttribute("disabled"), null));
  assert.equal((screen.getByRole("textbox", { name: "Question" }) as HTMLTextAreaElement).value,
    "Why is this deployment waiting?");
  assert.ok(screen.getByRole("dialog", { name: "Deployment detail" }));
});

test("unmounting an open assistant releases its global layout state without removing other body classes", async () => {
  document.body.classList.add("other-layout");
  try {
    const view = shell(null);
    fireEvent.keyDown(window, { key: "i", ctrlKey: true });
    await screen.findByRole("textbox", { name: "Question" });
    assert.ok(document.body.classList.contains("assistant-open"));
    view.unmount();
    assert.equal(document.body.classList.contains("assistant-open"), false);
    assert.ok(document.body.classList.contains("other-layout"));
  } finally {
    document.body.classList.remove("other-layout");
  }
});

test("one click runs the task in the side panel: rule finding first, then steps, reasoning and text stream in", async () => {
  const stream = liveStream();
  const asked = fakeServer({
    findings: () => json({ items: [{ code: "CUDA_OOM", evidence: [{ line: "torch.OutOfMemoryError: CUDA out of memory" }] }], total: 1 }),
    ask: stream.respond,
  });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));

  await waitFor(() => assert.ok(screen.getByText("Out of GPU memory")));
  assert.ok(screen.getByText("Why did this instance fail?"));
  assert.ok(screen.getByText("Rule match · AI verifies"));
  assert.ok(screen.getByLabelText("Matching log lines").textContent?.includes("CUDA out of memory"));
  assert.ok(screen.getByText("AI is analyzing…"));
  assert.ok(screen.getByRole("button", { name: "Stop" }));
  assert.deepEqual([...asked], [{ body: { task: "instance.diagnose", instance_uuid: "inst-1", locale: "en" }, accept: "text/event-stream" }]);

  stream.ping();
  stream.push("step", { id: 0, tool: "call_api", args: '{"path":"/api/v1/logs/search","query":{"q":"KV cache","level":"error"}}', status: "running", ms: 0 });
  await waitFor(() => assert.ok(screen.getByText("running…")));
  assert.ok(screen.getByText("Read"));
  // An API read shows which endpoint, not its raw arguments.
  assert.ok(screen.getByText("GET /api/v1/logs/search?q=KV+cache&level=error"));
  assert.ok(screen.getByText("AI is looking up more information…"));

  stream.push("step", { id: 0, tool: "call_api", args: '{"path":"/api/v1/logs/search","query":{"q":"KV cache","level":"error"}}', status: "done", ms: 12 });
  // While it thinks with no answer yet, the newest reasoning shows live.
  stream.push("reasoning", { delta: "weights 61 GiB " });
  stream.push("reasoning", { delta: "plus KV" });
  await waitFor(() => assert.equal(screen.getByRole("log", { name: "Reasoning" }).textContent, "weights 61 GiB plus KV"));
  assert.ok(screen.getByText("AI is thinking…"));

  // Once the answer starts, the reasoning folds away.
  stream.push("text", { delta: "**Root cause**: the KV cache " });
  await waitFor(() => assert.ok(screen.getByText("Root cause")));
  assert.equal(screen.queryByRole("log", { name: "Reasoning" }), null);
  assert.ok(screen.getByText("12 ms"));
  assert.ok(screen.getByText("Reasoning"));

  stream.push("text", { delta: "does not fit." });
  stream.push("done", done);
  stream.close();
  await waitFor(() => assert.ok(screen.getByText("First text 1.8s · total 6.9s · 1 tool calls · 2 model calls · 3200 tokens")));
  assert.ok(screen.getByText(/the KV cache does not fit\./));
  assert.equal(screen.queryByRole("button", { name: "Stop" }), null);
  // The next step is a click too.
  assert.ok(within(screen.getByLabelText("Suggested follow-ups")).getByRole("button", { name: "Give me the complete corrected launch arguments" }));
});

test("a follow-up keeps the task and subject and carries the thread", async () => {
  let n = 0;
  const asked = fakeServer({
    findings: () => json({ items: [], total: 0 }),
    ask: () => (++n === 1 ? sse(...answerFrames) : sse(frame("text", { delta: "Use --max-model-len 8192." }), frame("done", done))),
  });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByLabelText("Suggested follow-ups")));
  fireEvent.click(screen.getByRole("button", { name: "Give me the complete corrected launch arguments" }));
  await waitFor(() => assert.ok(screen.getByText("Use --max-model-len 8192.")));
  assert.deepEqual(asked[1].body, {
    task: "instance.diagnose", instance_uuid: "inst-1", locale: "en", question: "Give me the complete corrected launch arguments",
    history: [{ role: "user", content: "Why did this instance fail?" }, { role: "assistant", content: "**Root cause**: the KV cache does not fit." }],
  });
  assert.equal(asked.facts.length, 1, "a follow-up does not refetch the platform checks");

  // Typed follow-ups go the same way.
  fireEvent.change(screen.getByRole("textbox", { name: "Question" }), { target: { value: "and on gpu 2?" } });
  fireEvent.keyDown(screen.getByRole("textbox", { name: "Question" }), { key: "Enter", code: "Enter", charCode: 13 });
  await waitFor(() => assert.equal(asked.length, 3));
  assert.equal(asked[2].body.question, "and on gpu 2?");
  assert.equal((asked[2].body.history as unknown[]).length, 4);
});

test("the empty panel offers the page's tasks, attention first, and a free question about the picked subject", async () => {
  const asked = fakeServer({ ask: () => sse(frame("text", { delta: "ok" }), frame("done", done)) });
  shell(<Page subjects={[
    { subject: { kind: "deployment", id: 4, label: "qwen3" }, tasks: [] },
    { subject: { kind: "instance", uuid: "bad-1", label: "bad-1" }, tasks: ["instance.diagnose"], attention: "Failed" },
  ]} />);
  fireEvent.keyDown(window, { key: "i", metaKey: true });
  await waitFor(() => assert.ok(screen.getByText("On this page")));
  assert.ok(screen.getByText("Failed"));
  assert.ok(screen.getByRole("button", { name: "Diagnose failure" }));

  // The first subject the page shows is picked; a free question is about it.
  fireEvent.change(screen.getByRole("textbox", { name: "Question" }), { target: { value: "why slow?" } });
  fireEvent.click(screen.getByRole("button", { name: "Send" }));
  await waitFor(() => assert.equal(asked.length, 1));
  assert.deepEqual(asked[0].body, { task: "ask", deployment_id: 4, locale: "en", question: "why slow?" });

  // A new conversation goes back to the page's tasks; unpicking leaves the question about nothing.
  await waitFor(() => assert.ok(screen.getByRole("button", { name: "New conversation" })));
  fireEvent.click(screen.getByRole("button", { name: "New conversation" }));
  await waitFor(() => assert.ok(screen.getByText("On this page")));
  fireEvent.click(screen.getByText("Deployment qwen3"));
  fireEvent.change(screen.getByRole("textbox", { name: "Question" }), { target: { value: "cluster status?" } });
  fireEvent.click(screen.getByRole("button", { name: "Send" }));
  await waitFor(() => assert.equal(asked.length, 2));
  assert.deepEqual(asked[1].body, { task: "ask", locale: "en", question: "cluster status?" });
});

test("a page task button starts that task; tasks without platform checks fetch none", async () => {
  const asked = fakeServer({ ask: () => sse(frame("text", { delta: "busy" }), frame("done", done)) });
  shell(<Page subjects={[{ subject: { kind: "deployment", id: 9, label: "big" }, tasks: ["deployment.unscheduled"], attention: "2 replicas are waiting for a node" }]} />);
  fireEvent.keyDown(window, { key: "I", ctrlKey: true });
  fireEvent.click(await screen.findByRole("button", { name: "Why not placed" }));
  await waitFor(() => assert.ok(screen.getByText("Why are replicas not being placed?")));
  await waitFor(() => assert.equal(asked.length, 1));
  assert.deepEqual(asked[0].body, { task: "deployment.unscheduled", deployment_id: 9, locale: "en" });
  assert.equal(asked.facts.length, 0);
  await waitFor(() => assert.ok(screen.getByRole("button", { name: "Which node is closest to fitting it?" })));
});

test("an empty page says how to start", async () => {
  fakeServer({});
  shell(null);
  fireEvent.keyDown(window, { key: "i", metaKey: true });
  await waitFor(() => assert.ok(screen.getByText("Pick a task from a page, or ask below")));
});

test("without an AI service the platform checks still show, with a pointer to Settings", async () => {
  fakeServer({
    findings: () => json({ items: [{ code: "IMAGE_PULL_DENIED", evidence: [{ line: "pull access denied" }] }], total: 1 }),
    ask: () => json({ error: "no AI endpoint configured", code: "AI_ENDPOINT_NOT_CONFIGURED" }, 503),
  });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByText(/Configure an AI service in Settings/)));
  assert.ok(screen.getByText("Image pull failed"));
  assert.equal(screen.queryByText("AI request failed"), null);
});

test("a failure before the stream starts is shown with a working retry", async () => {
  let calls = 0;
  fakeServer({
    findings: () => json({ items: [], total: 0 }),
    ask: () => (++calls === 1 ? json({ error: "llm http 429: rate limited" }, 502) : sse(...answerFrames)),
  });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByText("AI request failed")));
  assert.ok(screen.getByText("llm http 429: rate limited"));
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => assert.ok(screen.getByText(/First text/)));
});

test("an error event mid-answer keeps what arrived and offers a retry", async () => {
  fakeServer({
    findings: () => json({ items: [], total: 0 }),
    ask: () => sse(frame("text", { delta: "Partial diagnosis" }), frame("error", { error: "llm stream ended before the answer finished" })),
  });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByText("llm stream ended before the answer finished")));
  assert.ok(screen.getByText("Partial diagnosis"));
  assert.ok(screen.getByRole("button", { name: "Retry" }));
});

test("a stream that closes without done or error is not passed off as an answer", async () => {
  fakeServer({ findings: () => json({ items: [], total: 0 }), ask: () => sse(frame("text", { delta: "Half" })) });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByText(/The connection closed before the answer finished/)));
  assert.ok(screen.getByText("Half"));
  assert.equal(screen.queryByText(/First text/), null);
});

test("stop keeps the text that arrived and the platform checks", async () => {
  const stream = liveStream();
  fakeServer({ findings: () => json({ items: [{ code: "PORT_IN_USE", evidence: [{ line: "address already in use" }] }], total: 1 }), ask: stream.respond });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByText("Port already in use")));
  stream.push("text", { delta: "So far: port 8000" });
  await waitFor(() => assert.ok(screen.getByText("So far: port 8000")));
  fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  await waitFor(() => assert.ok(screen.getByText(/^Stopped\./)));
  assert.ok(screen.getByText("So far: port 8000"));
  assert.ok(screen.getByText("Port already in use"));
  assert.equal(screen.queryByText(/The connection closed/), null);
});

test("findings failing to load does not hide the answer", async () => {
  fakeServer({ findings: () => json({ error: "disk" }, 500), ask: () => sse(...answerFrames) });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  await waitFor(() => assert.ok(screen.getByText(/First text/)));
  assert.ok(screen.getByText("disk"));
});

test("hardware fit states the physical facts: dtype support, interconnect and broken cards", async () => {
  const asked = fakeServer({
    fit: () => json({
      model_id: 7, weight_bytes: 61 * 2 ** 30, dtype: "bfloat16", gpu_memory_utilization: 0.9,
      rows: [
        { vendor: "nvidia", name: "H100 80GB", memory_bytes: 80 * 2 ** 30, nodes: 1, cards: 7, unusable: 1, max_cards_per_node: 7, dtype_supported: true, fast_link_cards: 4, fits: true, tp: 1, per_gpu_bytes: 66 * 2 ** 30, usable_bytes: 72 * 2 ** 30, headroom_bytes: 6 * 2 ** 30 },
        { vendor: "nvidia", name: "Quadro RTX 8000", memory_bytes: 48 * 2 ** 30, nodes: 1, cards: 1, unusable: 0, max_cards_per_node: 1, dtype_supported: false, dtype_note: "turing does not support bfloat16 (use float16)", fast_link_cards: 1, fits: false, tp: 0, per_gpu_bytes: 66 * 2 ** 30, usable_bytes: 43 * 2 ** 30, headroom_bytes: -23 * 2 ** 30 },
        { vendor: "nvidia", name: "L4", memory_bytes: 24 * 2 ** 30, nodes: 2, cards: 3, unusable: 0, max_cards_per_node: 2, dtype_supported: true, fast_link_cards: 0, fits: false, tp: 0, per_gpu_bytes: 33 * 2 ** 30, usable_bytes: 21.6 * 2 ** 30, headroom_bytes: -11.4 * 2 ** 30 },
      ],
    }),
    ask: () => sse(frame("text", { delta: "H100 fits." }), frame("done", { ...done, calls: 1 })),
  });
  shell(<AIAssistant task="model.fit" subject={{ kind: "model", id: 7, label: "Qwen3-32B" }} buttonText="" buttonType="link" />);
  fireEvent.click(screen.getByRole("button", { name: "Check hardware fit" }));
  await waitFor(() => assert.ok(screen.getByText("Fit on the online fleet")));
  const table = screen.getByRole("table");
  assert.ok(within(table).getByText("Yes · TP 1"));
  assert.ok(within(table).getByText("1 broken card not counted"));
  assert.ok(within(table).getByText("Fast link ×4"));
  assert.ok(within(table).getByText("PCIe only"));
  assert.ok(within(table).getByText("Cannot compute bfloat16"));
  assert.ok(within(table).getByText("66.0 GiB / 72.0 GiB"));
  assert.ok(within(table).getByText("×3 · 2 nodes"));
  await waitFor(() => assert.ok(screen.getByText(/answered from the context the platform gathered/)));
  assert.deepEqual(asked.map((a) => a.body), [{ task: "model.fit", model_id: 7, locale: "en" }]);
});

test("an empty fleet says so instead of an empty table", async () => {
  const stream = liveStream();
  fakeServer({ fit: () => json({ model_id: 7, weight_bytes: 1, gpu_memory_utilization: 0.9, rows: [] }), ask: stream.respond });
  shell(<AIAssistant task="model.fit" subject={{ kind: "model", id: 7, label: "m" }} />);
  fireEvent.click(screen.getByRole("button", { name: "Check hardware fit" }));
  await waitFor(() => assert.ok(screen.getByText("No online node has an accelerator to size this model against.")));
});

test("a long chain of thought shows only its newest part", async () => {
  const stream = liveStream();
  fakeServer({ findings: () => json({ items: [], total: 0 }), ask: stream.respond });
  renderInstance();
  fireEvent.click(screen.getByRole("button", { name: "AI diagnose" }));
  stream.push("reasoning", { delta: "OLDEST " + "x".repeat(2000) + " NEWEST" });
  await waitFor(() => assert.ok(screen.getByRole("log", { name: "Reasoning" })));
  const shown = screen.getByRole("log", { name: "Reasoning" }).textContent ?? "";
  assert.ok(shown.startsWith("…") && shown.endsWith("NEWEST") && !shown.includes("OLDEST") && shown.length <= 601, shown.length.toString());
});

test("stepDetail names what each tool read", () => {
  assert.equal(stepDetail("call_api", '{"path":"/api/v1/nodes","jq":".items[] | {name, state}"}'), "GET /api/v1/nodes · jq: .items[] | {name, state}");
  assert.equal(stepDetail("call_api", '{"path":"/api/v1/metrics/query","query":{"metric":"gpuworks_gpu_utilization","range":"1h"}}'), "GET /api/v1/metrics/query?metric=gpuworks_gpu_utilization&range=1h");
  assert.equal(stepDetail("search_api", '{"query":"node disk"}'), "node disk");
  assert.equal(stepDetail("search_metrics", '{"query":"rdma"}'), "rdma");
  assert.equal(stepDetail("cluster_overview", "{}"), "");
  assert.equal(stepDetail("compare_with_healthy_replica", '{"instance_uuid":"x"}'), "x");
  assert.equal(stepDetail("get_gpu_topology", '{"node_uuid":"ppu-1"}'), "ppu-1");
  // Arguments the model sent malformed are shown as sent.
  assert.equal(stepDetail("call_api", "{not json"), "{not json");
});

// A mistake the model fixes on its own next step (a wrong jq field) reads as
// a note, its full text on hover; a platform failure stays an error.
test("the step list tells a self-correctable mistake from a failure", () => {
  const jqError = 'jq: cannot divide: null and number (1073741824). The answer has no field "memory_bytes" (did you mean "memory_total_bytes"?).';
  render(
    <I18nextProvider i18n={i18n}>
      <StepList
        steps={[
          { id: 0, tool: "call_api", args: '{"path":"/api/v1/gpus","jq":".items[] | {m: .memory_bytes}"}', status: "error", ms: 2, error: jqError },
          { id: 1, tool: "call_api", args: '{"path":"/api/v1/nodes"}', status: "error", ms: 3, error: "HTTP 500: database is locked" },
        ]}
      />
    </I18nextProvider>,
  );
  const note = screen.getByText("query needs a fix - the model got the actual structure");
  assert.equal(note.getAttribute("title"), jqError);
  assert.ok(!note.className.includes("danger"));
  assert.ok(screen.getByText("HTTP 500: database is locked").className.includes("danger"));
  assert.equal(screen.queryByText(jqError), null);
});

test("selfCorrectable covers only the model's own mistakes", () => {
  for (const e of ['jq: x', "arguments are not valid JSON for this tool (x)", "no GET endpoint /api/v1/x; find one with search_api", 'path must start with /api/v1/, got "/v2"', 'no node is named "gpu9"']) {
    assert.ok(selfCorrectable(e), e);
  }
  for (const e of ["HTTP 403: forbidden", "/api/v1/apikeys is not readable by the assistant (API keys)", "context deadline exceeded"]) {
    assert.ok(!selfCorrectable(e), e);
  }
});
