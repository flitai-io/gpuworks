import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test, { type TestContext } from "node:test";
// @ts-expect-error Browser harness module has no TypeScript declaration.
import { observePageNetwork, settle } from "./browser/page-states.mjs";

async function fixture(t: TestContext) {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: 1 });
  const attachments: string[] = [];
  const session = Object.assign(new EventEmitter(), {
    send: async (method: string, params?: { targetId?: string }) => {
      if (method === "Target.attachToTarget") attachments.push(params!.targetId!);
    },
  });
  const page = Object.assign(new EventEmitter(), {
    context: () => ({ newCDPSession: async () => session }),
    frames: () => ["main"],
    workers: () => [],
    waitForTimeout: async (ms: number) => { t.mock.timers.tick(ms); },
    evaluate: async (_fn: unknown, [quiet, left]: number[]) => { t.mock.timers.tick(Math.min(quiet, left)); },
  });
  await observePageNetwork(page);
  const url = "https://example.test/same.js";
  return {
    page, attachments,
    navigate: (loaderId: string) => session.emit("Page.frameNavigated", { frame: { loaderId } }),
    start: (id: string, type = "Script", loaderId = "doc") => {
      const request = { resourceType: () => type.toLowerCase(), url: () => url };
      session.emit("Network.requestWillBeSent", { requestId: id, type, loaderId, request: { url } });
      page.emit("request", request);
      return {
        headers: (mime: string) => page.emit("response", { request: () => request, headers: () => ({ "content-type": mime }) }),
        end: (failed = false) => {
          session.emit(failed ? "Network.loadingFailed" : "Network.loadingFinished", { requestId: id });
          page.emit(failed ? "requestfailed" : "requestfinished", request);
        },
      };
    },
    discover: (targetId: string) => session.emit("Target.targetCreated", { targetInfo: { type: "shared_worker", targetId } }),
    loaded: (targetId: string) => session.emit("Target.receivedMessageFromTarget", {
      targetId, sessionId: targetId, message: '{"method":"Inspector.workerScriptLoaded"}',
    }),
    elapsed: async () => {
      const start = Date.now();
      await settle(page);
      return Date.now() - start;
    },
  };
}

for (const order of ["target first", "request first"]) {
  test(`worker load requires the matching CDP ID, then a full quiet window (${order})`, async t => {
    const f = await fixture(t);
    f.navigate("doc");
    if (order === "target first") f.discover("worker");
    f.start("worker", "Script", "");
    if (order === "request first") f.discover("worker");
    f.loaded("another-worker");
    setTimeout(() => f.loaded("worker"), 1600);
    assert.deepEqual(f.attachments, ["worker"]);
    // Network quiet (500), DOM quiet (300), and the final wait (500).
    assert.equal(await f.elapsed(), 2900);
  });
}

test("an ordinary slow script with the worker's URL must finish before settling", async t => {
  const f = await fixture(t);
  f.start("worker");
  const script = f.start("ordinary-script");
  f.discover("worker");
  f.loaded("worker");
  setTimeout(() => script.end(), 3200);
  assert.equal(await f.elapsed(), 4500);

  const nextScript = f.start("next-script");
  setTimeout(() => nextScript.end(true), 2000);
  assert.equal(await f.elapsed(), 3300, "worker completion was reused");
});

test("an unrelated shared worker does not release an unanswered page script", async t => {
  const f = await fixture(t);
  f.start("page-script");
  f.discover("worker-on-another-page");
  f.loaded("worker-on-another-page");
  assert.deepEqual(f.attachments, []);
  assert.equal(await f.elapsed(), 8500);
});

test("a fetch with the worker's URL remains pending until its body completes", async t => {
  const f = await fixture(t);
  f.start("worker");
  f.discover("worker");
  f.loaded("worker");
  const api = f.start("api", "Fetch");
  f.discover("api");
  f.loaded("api");
  api.headers("application/json");
  setTimeout(() => api.end(), 3200);
  assert.deepEqual(f.attachments, ["worker"]);
  assert.equal(await f.elapsed(), 4500);
});

test("a fetch carrying an event stream still permits the page to settle", async t => {
  const f = await fixture(t);
  const stream = f.start("live-updates", "Fetch");
  stream.headers("text/event-stream");
  assert.equal(await f.elapsed(), 1300);
});

test("a worker from the previous document cannot complete a new request", async t => {
  const f = await fixture(t);
  f.navigate("login");
  const previous = f.start("old-worker", "Script", "");
  f.discover("old-worker");
  const current = f.start("new-script", "Script", "images");
  f.navigate("images");
  f.loaded("old-worker");
  previous.end(true);
  setTimeout(() => current.end(), 3200);
  assert.equal(await f.elapsed(), 4500);
});

for (const target of ["worker", "frameattached"]) {
  test(`a page with another ${target} keeps its Playwright requests pending`, async t => {
    const f = await fixture(t);
    f.page.emit(target);
    f.start("worker");
    f.discover("worker");
    f.loaded("worker");
    assert.equal(await f.elapsed(), 8500);
  });
}

test("when the page reports script completion the worker cannot release a later script", async t => {
  const f = await fixture(t);
  const worker = f.start("worker");
  f.discover("worker");
  worker.end();
  f.loaded("worker");
  const ordinary = f.start("later");
  setTimeout(() => ordinary.end(), 3200);
  assert.equal(await f.elapsed(), 4500);
});
