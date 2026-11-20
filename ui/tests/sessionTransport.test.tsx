import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React, { useState } from "react";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element", "Node", "MutationObserver"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { act, cleanup, render, screen } = await import("@testing-library/react");
const { useEvents, useEventPayload } = await import("../src/api.ts");
const { getPlaygroundCatalog, streamChat } = await import("../src/pages/playground/api.ts");
const { terminalPreflight } = await import("../src/terminal.ts");
const { notifySessionExpired, notifySessionAuthenticated, subscribeSessionChanges } = await import("../src/session.ts");

class EventSourceFixture extends dom.window.EventTarget {
  static sources: EventSourceFixture[] = [];
  static OPEN = 1;
  readyState = 0;
  onerror: (() => void) | null = null;
  closeCount = 0;
  constructor(readonly url: string) {
    super();
    EventSourceFixture.sources.push(this);
  }
  close() { this.closeCount++; }
  emit(topic: string, payload: unknown) {
    if (this.closeCount) return;
    this.dispatchEvent(new dom.window.MessageEvent(topic, { data: JSON.stringify(payload) }));
  }
  fail() { this.onerror?.(); }
}
const originalFetch = globalThis.fetch;
const originalEventSource = globalThis.EventSource;
Object.defineProperty(globalThis, "EventSource", { configurable: true, writable: true, value: EventSourceFixture });
afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  notifySessionAuthenticated();
  EventSourceFixture.sources = [];
});

function RefetchSubscriber() {
  const [count, setCount] = useState(0);
  useEvents("node", () => setCount((current) => current + 1));
  return <output aria-label="Refreshes">{count}</output>;
}
function PayloadSubscriber() {
  const [payload, setPayload] = useState("none");
  useEventPayload("node", (event) => setPayload(event.key || event.reason || "empty"));
  return <output aria-label="Node payload">{payload}</output>;
}
function Subscribers({ payload = true }: { payload?: boolean }) {
  return <><RefetchSubscriber />{payload && <PayloadSubscriber />}</>;
}
const text = (label: string) => screen.getByLabelText(label).textContent;
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

test("shared SSE subscriptions deliver topic payloads, close on expiry and restore both listeners without remounting", async () => {
  const view = render(<Subscribers />);
  assert.equal(EventSourceFixture.sources.length, 1);
  const first = EventSourceFixture.sources[0];
  assert.equal(first.url, "/api/v1/events");
  act(() => first.emit("deployment", { key: "unrelated" }));
  assert.equal(text("Refreshes"), "0");
  assert.equal(text("Node payload"), "none");
  act(() => first.emit("node", { key: "node-before-expiry" }));
  assert.equal(text("Refreshes"), "1");
  assert.equal(text("Node payload"), "node-before-expiry");
  act(() => { notifySessionExpired(); notifySessionExpired(); });
  assert.equal(first.closeCount, 1);
  act(() => first.emit("node", { key: "closed-source" }));
  assert.equal(text("Refreshes"), "1");
  assert.equal(text("Node payload"), "node-before-expiry");
  act(() => notifySessionAuthenticated());
  assert.equal(EventSourceFixture.sources.length, 2);
  const restored = EventSourceFixture.sources[1];
  assert.equal(text("Refreshes"), "2");
  assert.equal(text("Node payload"), "session-restored");
  act(() => restored.emit("node", { key: "node-after-login" }));
  assert.equal(text("Refreshes"), "3");
  assert.equal(text("Node payload"), "node-after-login");
  view.rerender(<Subscribers payload={false} />);
  assert.equal(restored.closeCount, 0);
  act(() => restored.emit("node", { key: "one-listener-remains" }));
  assert.equal(text("Refreshes"), "4");
  assert.equal(screen.queryByLabelText("Node payload"), null);
  view.unmount();
  assert.equal(first.closeCount, 1);
  assert.equal(restored.closeCount, 1);
  act(() => notifySessionAuthenticated());
  assert.equal(EventSourceFixture.sources.length, 2);
});

test("subscribers mounted after expiry wait for login before creating a source and receiving events", () => {
  notifySessionExpired();
  const view = render(<Subscribers />);
  assert.equal(EventSourceFixture.sources.length, 0);
  assert.equal(text("Refreshes"), "0");
  assert.equal(text("Node payload"), "none");
  act(() => notifySessionAuthenticated());
  assert.equal(EventSourceFixture.sources.length, 1);
  const source = EventSourceFixture.sources[0];
  assert.equal(text("Refreshes"), "1");
  assert.equal(text("Node payload"), "session-restored");
  act(() => source.emit("node", { key: "node-after-delayed-login" }));
  assert.equal(text("Refreshes"), "2");
  assert.equal(text("Node payload"), "node-after-delayed-login");
  view.unmount();
  assert.equal(source.closeCount, 1);
});

test("SSE error validates management auth once, preserves transport on network and 503, and deduplicates expiry", async () => {
  render(<Subscribers />);
  const source = EventSourceFixture.sources[0];
  let calls = 0;
  let finish: ((response: Response) => void) | undefined;
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "/api/v1/auth/me");
    assert.equal(options?.credentials, "include");
    calls++;
    if (calls === 1) throw new Error("offline");
    if (calls === 2) return new Response(null, { status: 503 });
    return new Promise((resolve) => { finish = resolve; });
  };
  const changes: string[] = [];
  const stop = subscribeSessionChanges((change) => changes.push(change));
  try {
    await act(async () => { source.fail(); await settle(); });
    assert.equal(source.closeCount, 0);
    act(() => source.emit("node", { key: "after-network-error" }));
    assert.equal(text("Node payload"), "after-network-error");
    await act(async () => { source.fail(); await settle(); });
    assert.equal(source.closeCount, 0);
    act(() => source.emit("node", { key: "after-service-error" }));
    assert.equal(text("Node payload"), "after-service-error");
    assert.deepEqual(changes, []);
    act(() => { source.fail(); source.fail(); source.fail(); });
    assert.equal(calls, 3);
    await act(async () => {
      finish!(Response.json({ code: "AUTHENTICATION_REQUIRED", error: "Authentication is required" }, { status: 401 }));
      await settle();
    });
    assert.deepEqual(changes, ["expired"]);
    assert.equal(source.closeCount, 1);
    assert.equal(text("Refreshes"), "2");
    assert.equal(text("Node payload"), "after-service-error");
  } finally { stop(); }
});

test("Playground catalog and chat preserve sessions for upstream errors and report management auth expiry", async () => {
  const changes: string[] = [];
  const stop = subscribeSessionChanges((change) => changes.push(change));
  const output: string[] = [];
  const callbacks = { onContent: (delta: string) => output.push(delta), onReasoning() {}, onUsage() {}, onDelta() {} };
  const chat = () => streamChat("tiny-model", [{ role: "user", content: "hello" }], { enabled: {}, values: {}, extraJSON: "" }, [], callbacks, new AbortController().signal);
  const paths: string[] = [];
  let payload: unknown = { error: { message: "Upstream API key is invalid", code: "invalid_api_key" } };
  let status = 401;
  globalThis.fetch = async (url, options) => {
    paths.push(String(url));
    assert.equal(options?.credentials, "include");
    return Response.json(payload, { status });
  };
  try {
    await assert.rejects(getPlaygroundCatalog(), /Upstream API key is invalid/);
    await assert.rejects(chat(), /Upstream API key is invalid/);
    assert.deepEqual(changes, []);
    assert.deepEqual(output, []);
    status = 503;
    payload = { code: "AUTHENTICATION_REQUIRED", error: "Service unavailable" };
    await assert.rejects(chat(), /Service unavailable/);
    assert.deepEqual(changes, []);
    status = 401;
    payload = { code: "AUTHENTICATION_REQUIRED", error: "Management session expired" };
    await Promise.all([
      assert.rejects(getPlaygroundCatalog(), /Management session expired/),
      assert.rejects(chat(), /Management session expired/),
    ]);
    assert.deepEqual(changes, ["expired"]);
    assert.deepEqual(paths.slice(0, 2), ["/api/v1/playground/models", "/api/v1/playground/v1/chat/completions"]);
  } finally { stop(); }
});

test("terminal preflight propagates management expiry while retaining non-authentication failures", async () => {
  const changes: string[] = [];
  const stop = subscribeSessionChanges((change) => changes.push(change));
  let status = 403;
  let payload = { code: "ADMINISTRATOR_REQUIRED", error: "Administrator permission is required" };
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "/api/v1/nodes/node%2Fone/terminal/settings");
    assert.equal(options?.credentials, "include");
    return Response.json(payload, { status });
  };
  try {
    await assert.rejects(terminalPreflight("node/one"), /Administrator permission/);
    assert.deepEqual(changes, []);
    status = 401;
    payload = { code: "AUTHENTICATION_REQUIRED", error: "Sign in again" };
    await assert.rejects(terminalPreflight("node/one"), /Sign in again/);
    assert.deepEqual(changes, ["expired"]);
  } finally { stop(); }
});

after(() => { globalThis.EventSource = originalEventSource; });

test("a new login switches active worker subscriptions without an intervening expiry, and worker failure keeps topics live", async () => {
  const { openChangeStream } = await import("../src/events/changeStream.ts");
  class WorkerFixture {
    static all: WorkerFixture[] = [];
    onerror: (() => void) | null = null;
    sent: { topics?: string[]; close?: boolean }[] = [];
    closed = false;
    port = {
      onmessage: null as ((event: { data: unknown }) => void) | null,
      postMessage: (msg: { topics?: string[]; close?: boolean }) => this.sent.push(msg),
      close: () => { this.closed = true; },
    };
    constructor(_url: URL, readonly options: { name: string }) { WorkerFixture.all.push(this); }
  }
  const original = Object.getOwnPropertyDescriptor(globalThis, "SharedWorker");
  Object.defineProperty(globalThis, "SharedWorker", { configurable: true, value: WorkerFixture });
  let errors = 0;
  const stream = openChangeStream(() => { errors++; });
  try {
    const seen: string[] = [];
    stream.addEventListener("node", event => seen.push((event as MessageEvent<string>).data));
    stream.addEventListener("deployment", event => seen.push((event as MessageEvent<string>).data));
    const first = WorkerFixture.all[0];
    assert.deepEqual(first.sent.at(-1), { topics: ["node", "deployment"] });
    first.port.onmessage?.({ data: { topic: "node", data: "before" } });
    notifySessionAuthenticated();
    const current = WorkerFixture.all.at(-1)!;
    assert.ok(first.closed);
    assert.notEqual(current.options.name, first.options.name);
    assert.deepEqual(current.sent.at(-1), { topics: ["node", "deployment"] });
    first.port.onmessage?.({ data: { topic: "node", data: "stale" } });
    current.port.onmessage?.({ data: { topic: "deployment", data: "after" } });
    current.port.onmessage?.({ data: { error: true } });
    assert.equal(errors, 1);
    current.onerror?.();
    EventSourceFixture.sources.at(-1)!.emit("node", { key: "native" });
    assert.deepEqual(seen, ["before", "after", '{"key":"native"}']);
  } finally {
    stream.close();
    if (original) Object.defineProperty(globalThis, "SharedWorker", original);
    else Reflect.deleteProperty(globalThis, "SharedWorker");
  }
});

test("two console tabs share one server connection and receive only their subscribed changes", async () => {
  const worker = new dom.window.EventTarget();
  const original = Object.getOwnPropertyDescriptor(globalThis, "self");
  Object.defineProperty(globalThis, "self", { configurable: true, value: worker });
  try {
    await import("../src/events/sharedWorker.ts");
    function connectTab() {
      const messages: unknown[] = [];
      const port = Object.assign(new dom.window.EventTarget(), {
        postMessage(message: unknown) { messages.push(message); },
        onmessage: null as null | ((event: { data: { topics?: string[]; close?: boolean } }) => void),
        close() {},
      });
      worker.dispatchEvent(Object.assign(new dom.window.Event("connect"), { ports: [port] }));
      assert.ok(port.onmessage);
      return { messages, subscribe: (...topics: string[]) => port.onmessage!({ data: { topics } }),
        close: () => port.onmessage!({ data: { close: true } }) };
    }
    const list = connectTab();
    list.subscribe("network");
    const source = EventSourceFixture.sources[0];
    source.readyState = EventSourceFixture.OPEN;
    const detail = connectTab();
    detail.subscribe("network", "node");
    assert.equal(EventSourceFixture.sources.length, 1);
    assert.deepEqual(detail.messages, [{ topic: "node", data: '{"reason":"connected"}' }]);
    source.emit("network", { id: 3 });
    assert.deepEqual(list.messages, [{ topic: "network", data: '{"id":3}' }]);
    assert.deepEqual(detail.messages.slice(1), list.messages);
    detail.subscribe("node");
    source.emit("network", { id: 4 });
    assert.equal(detail.messages.length, 2);
    list.close();
    assert.equal(source.closeCount, 0);
    source.emit("node", { id: 6 });
    assert.deepEqual(detail.messages.at(-1), { topic: "node", data: '{"id":6}' });
    assert.equal(list.messages.length, 2);
    detail.close();
    assert.equal(source.closeCount, 1);
  } finally {
    if (original) Object.defineProperty(globalThis, "self", original);
    else Reflect.deleteProperty(globalThis, "self");
  }
});
