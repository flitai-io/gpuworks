import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, dictionary, page } from "./helpers/batchFixtures.ts";

// The batches page: the list's loading / empty / error / success states, the
// status filter, polling that stops once everything has ended, and the detail
// drawer with its counts, timeline, downloads and the confirmed cancel.

const closeChannels = trackMessageChannels();
// jsdom reports a document that is never rendered as hidden, which pauses polling.
Object.defineProperty(dom.window.document, "hidden", { value: false, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { i18n, locale } = await createI18n(["batches"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { ConfigProvider } = await import("antd");
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { batchApi } = await import("../src/batchApi.ts");
const { formatTimeSec } = await import("../src/i18n/datetime.ts");
const { default: BatchesPage } = await import("../src/pages/BatchesPage.tsx");
type Batch = import("../src/batchApi.ts").Batch;

// user-event does not set keyCode, which the dialogs read; the focused element receives the key.
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });

const b = locale.batches;
const c = locale.common;
const original = { list: batchApi.list, get: batchApi.get, cancel: batchApi.cancel, create: batchApi.create, dictionary: batchApi.dictionary, me: api.me };

let listCalls: Array<Record<string, unknown>> = [];
let getCalls: string[] = [];

function stubAdmin(role = "admin") {
  api.me = (async () => ({ role })) as never;
}

beforeEach(() => {
  localStorage.clear();
  listCalls = [];
  getCalls = [];
  stubAdmin();
  batchApi.dictionary = async () => dictionary();
});
afterEach(() => {
  message.destroy();
  cleanup();
  Object.assign(batchApi, original);
  api.me = original.me;
});
after(() => closeChannels());

function Probe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mount(entry = "/batches", pollMs?: number) {
  render(
    <I18nextProvider i18n={i18n}><FeedbackHost />
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={[entry]}>
          <Routes>
            <Route path="/batches" element={<BatchesPage pollMs={pollMs} />} />
          </Routes>
          <Probe />
        </MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

function serve(items: Batch[]) {
  batchApi.list = async (query) => { listCalls.push({ ...query }); return page(items, { page_size: query.page_size }); };
  batchApi.get = async (id) => {
    getCalls.push(id);
    const found = items.find((item) => item.id === id);
    if (!found) throw new APIError("BATCH_NOT_FOUND", undefined, "batch not found", 404);
    return { ...found, input_filename: "reqs.jsonl" };
  };
}

async function closeDrawer(user: ReturnType<typeof userEvent.setup>) {
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
}
// An icon button's name starts with its icon's label, so match the end.
const named = (label: string) => new RegExp(`${label}$`);
const row = (id: string) => screen.getByRole("button", { name: b.action.open.replace("{{id}}", id) }).closest("tr") as HTMLElement;
// The value cell beside a description label.
const valueOf = (scope: HTMLElement, label: string) => within(scope).getByText(label).closest("dt")?.nextElementSibling?.textContent;

test("a loading state shows until the first page answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  batchApi.list = async () => { await gate; return page([batch()]); };
  mount();
  assert.ok(screen.getByRole("status"), "the table shows its loading status");
  release();
  await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_aaa111") });
  // A boolean, never the element: a failed assert.equal prints its operands, and printing a jsdom node walks the whole document.
  await waitFor(() => assert.ok(!screen.queryByRole("status")));
});

test("a failed load shows the error and retries on request", async () => {
  let calls = 0;
  batchApi.list = async () => {
    calls += 1;
    if (calls === 1) throw new Error("batches unavailable");
    return page([batch()]);
  };
  const user = mount();
  assert.ok(await screen.findByText(c.dataTable.loadFailed));
  assert.ok(screen.getByText("batches unavailable"));
  await user.click(screen.getByRole("button", { name: c.actions.retry }));
  await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_aaa111") });
  assert.ok(!screen.queryByText(c.dataTable.loadFailed));
});

test("an empty list says so and offers to create the first batch", async () => {
  serve([]);
  mount();
  assert.ok(await screen.findByText(b.empty.all));
  await waitFor(() => assert.equal((screen.getAllByRole("button", { name: b.action.create }).at(-1) as HTMLButtonElement).disabled, false));
});

test("the list shows each batch's status, endpoint, progress and times", async () => {
  serve([
    batch({ id: "batch_run", status: "in_progress", request_counts: { total: 10, completed: 4, failed: 1 } }),
    batch({ id: "batch_done", status: "completed", endpoint: "/v1/embeddings", request_counts: { total: 8, completed: 8, failed: 0 }, created_at: "2026-10-27T01:02:00Z", expires_at: "2026-10-28T01:02:00Z" }),
  ]);
  mount();
  await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_run") });
  const running = row("batch_run");
  assert.ok(within(running).getByText(b.status.in_progress));
  assert.ok(within(running).getByText("/v1/chat/completions"));
  assert.ok(within(running).getByText("5 / 10 done"));
  assert.ok(within(running).getByText("1 failed"), "the failed count is called out");
  assert.ok(within(running).getByRole("progressbar", { name: "5 of 10 requests finished, 1 failed" }));
  const done = row("batch_done");
  assert.ok(within(done).getByText(b.status.completed));
  assert.ok(within(done).getByText("/v1/embeddings"));
  assert.ok(within(done).getByText("8 / 8 done"));
  assert.ok(!within(done).queryByText(/failed$/));
  // Created and expires, in the viewer's own time zone: whichever zone the
  // test runs in, both fall on 26–28 October.
  assert.equal(within(done).getAllByText(/^2026-10-2[678] \d\d:\d\d$/).length, 2);
  assert.deepEqual(listCalls[0], { state: [], page: 1, page_size: 20 });
});

test("the status filter asks the server for those states and words an empty answer", async () => {
  serve([batch()]);
  const user = mount();
  await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_aaa111") });
  batchApi.list = async (query) => { listCalls.push({ ...query }); return page([], { page_size: query.page_size }); };
  await user.click(screen.getByRole("button", { name: `Filter by ${b.columns.status}` }));
  await user.click(await screen.findByRole("checkbox", { name: b.status.completed }));
  await user.click(await screen.findByRole("checkbox", { name: b.status.failed }));
  assert.ok(await screen.findByText(b.empty.filtered));
  assert.deepEqual(listCalls.at(-1)?.state, ["completed", "failed"]);
  await user.keyboard("{Escape}");
  const active = await screen.findByRole("group", { name: "Filtered" });
  assert.equal(within(active).getByRole("button", { name: /^Clear filter: / }).textContent, `${b.columns.status}: ${b.status.completed}, ${b.status.failed}`);
  await user.click(within(active).getByRole("button", { name: /^Clear filter: / }));
  await waitFor(() => assert.deepEqual(listCalls.at(-1)?.state, []));
});

test("the list refreshes while a batch is moving and stops once all have ended", async () => {
  let phase: "running" | "ended" = "running";
  batchApi.list = async (query) => {
    listCalls.push({ ...query });
    return page([batch({ status: phase === "running" ? "in_progress" : "completed", completed_at: phase === "ended" ? "2026-10-28T10:00:00Z" : undefined })]);
  };
  mount("/batches", 40);
  await screen.findByText(b.status.in_progress);
  await waitFor(() => assert.ok(listCalls.length >= 3, "polls while the batch runs"));
  phase = "ended";
  await screen.findByText(b.status.completed);
  await new Promise((resolve) => setTimeout(resolve, 150));
  const settled = listCalls.length;
  await new Promise((resolve) => setTimeout(resolve, 250));
  assert.equal(listCalls.length, settled, "no request once every batch on the page has ended");
});

test("a list of only ended batches never polls", async () => {
  serve([batch({ status: "failed", failed_at: "2026-10-28T10:00:00Z" })]);
  mount("/batches", 30);
  await screen.findByText(b.status.failed);
  await new Promise((resolve) => setTimeout(resolve, 200));
  assert.equal(listCalls.length, 1);
});

test("a row opens the drawer with the counts, every timestamp, metadata and the owner", async () => {
  serve([batch({
    id: "batch_full", status: "completed", request_counts: { total: 10, completed: 8, failed: 2 },
    metadata: { team: "nlp", job: "nightly" },
    finalizing_at: "2026-10-28T09:50:00Z", completed_at: "2026-10-28T10:00:00Z",
    output_file_id: "file-out", error_file_id: "file-err",
  })]);
  const user = mount();
  await user.click(await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_full") }));
  const dialog = await screen.findByRole("dialog");
  assert.equal(screen.getByTestId("loc").textContent, "/batches?detail=batch_full");
  await within(dialog).findByText("reqs.jsonl");
  const counts = within(dialog).getByRole("region", { name: b.detail.sections.counts });
  assert.equal(valueOf(counts, b.detail.total), "10");
  assert.equal(valueOf(counts, b.detail.completed), "8");
  assert.equal(valueOf(counts, b.detail.failed), "2");
  const times = within(dialog).getByRole("region", { name: b.detail.sections.times });
  for (const key of ["created_at", "in_progress_at", "finalizing_at", "completed_at", "expires_at"]) {
    assert.ok(within(times).getByText(b.detail.times[key]), key);
  }
  for (const key of ["failed_at", "expired_at", "cancelling_at", "cancelled_at"]) {
    assert.ok(!within(times).queryByText(b.detail.times[key]), `${key} was never reached`);
  }
  const metadata = within(dialog).getByRole("region", { name: b.detail.sections.metadata });
  assert.equal(valueOf(metadata, "team"), "nlp");
  assert.equal(valueOf(metadata, "job"), "nightly");
  assert.equal(valueOf(dialog, b.detail.owner), "3");
  assert.ok(within(dialog).getByText("/v1/chat/completions"));
  assert.ok(within(dialog).getByText("24h"));
  assert.ok(!within(dialog).queryByRole("button", { name: b.action.cancel }), "an ended batch cannot be cancelled");
});

test("a batch without metadata says so, and a failed batch shows why", async () => {
  serve([batch({ id: "batch_bad", status: "failed", failed_at: "2026-10-28T09:01:00Z", failure: { code: "input_file_unreadable", message: "input file could not be read" } })]);
  const user = mount("/batches?detail=batch_bad");
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("input file could not be read");
  assert.ok(within(dialog).getByText(`${b.detail.failureCode}: input_file_unreadable`));
  assert.ok(within(dialog).getByText(b.detail.noMetadata));
  await closeDrawer(user);
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "/batches"));
});

test("the detail of an unknown batch reports it and retries", async () => {
  serve([]);
  const user = mount("/batches?detail=batch_gone");
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText(b.page.detailLoadFailed);
  const before = getCalls.length;
  await user.click(within(dialog).getByRole("button", { name: b.action.retry }));
  await waitFor(() => assert.ok(getCalls.length > before));
});

test("a running batch whose requests wait says for which model, in what state, since when and why", async () => {
  const since = "2026-11-03T03:00:40Z";
  serve([
    batch({
      id: "batch_wait", status: "in_progress",
      waiting: [
        { model: "BAAI/bge-m3", state: "failed", status: 503, detail: "deployment for BAAI/bge-m3 failed to start; see the deployment list", since, requests: 8 },
        { model: "ghost", status: 0, detail: "connection reset", since, requests: 1 },
      ],
    }),
  ]);
  mount("/batches?detail=batch_wait");
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText(b.detail.waiting.title);
  const line = (model: string, state: string, count: number) =>
    i18n.t("batches:detail.waiting.line", { model, state, count, since: formatTimeSec(since) });
  assert.ok(within(dialog).getByText(line("BAAI/bge-m3", b.create.model.state.failed, 8)));
  assert.ok(within(dialog).getByText(i18n.t("batches:detail.waiting.answer", { status: 503, detail: "deployment for BAAI/bge-m3 failed to start; see the deployment list" })));
  // A model no deployment serves any more has no state; a transport failure has no status.
  assert.ok(within(dialog).getByText(line("ghost", b.detail.waiting.gone, 1)));
  assert.ok(within(dialog).getByText("connection reset"));
});

test("a batch with no request waiting shows no waiting notice", async () => {
  serve([batch({ id: "batch_fine", status: "in_progress" })]);
  mount("/batches?detail=batch_fine");
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("batch_fine", { selector: "code" });
  assert.ok(!(within(dialog).queryByText(b.detail.waiting.title)), "expected the element to be absent");
});

test("downloads are links for files that exist and disabled buttons for those that do not", async () => {
  const keptUntil = "2099-01-02T03:04:05Z";
  serve([
    batch({ id: "batch_ok", status: "completed", completed_at: new Date().toISOString(), output_file_id: "file-out", output_expires_at: keptUntil }),
  ]);
  mount("/batches?detail=batch_ok");
  const dialog = await screen.findByRole("dialog");
  const input = await within(dialog).findByRole("link", { name: named(b.detail.download.input) });
  assert.equal(input.getAttribute("href"), "/api/v1/batches/batch_ok/files/input");
  assert.ok(input.hasAttribute("download"));
  const output = within(dialog).getByRole("link", { name: named(b.detail.download.output) });
  assert.equal(output.getAttribute("href"), "/api/v1/batches/batch_ok/files/output");
  assert.ok(!within(dialog).queryByRole("link", { name: named(b.detail.download.error) }));
  const errors = within(dialog).getByRole("button", { name: named(b.detail.download.error) }) as HTMLButtonElement;
  assert.equal(errors.disabled, true);
  // The server says when the files go; the page shows its time, not a retention of its own.
  assert.ok(within(dialog).getByText(b.detail.download.hint.replace("{{time}}", formatTimeSec(keptUntil))));
});

test("output and error files past the server's output_expires_at are not offered", async () => {
  // Ended long ago but kept until the server's time: still offered (the retention is the server's to set).
  serve([
    batch({ id: "batch_kept", status: "completed", completed_at: "2020-01-01T00:00:00Z", output_file_id: "file-out", output_expires_at: "2099-01-01T00:00:00Z" }),
    batch({ id: "batch_old", status: "completed", completed_at: "2026-10-01T00:00:00Z", output_file_id: "file-out", error_file_id: "file-err", output_expires_at: "2026-10-02T00:00:00Z" }),
  ]);
  const user = mount("/batches?detail=batch_kept");
  const kept = await screen.findByRole("dialog");
  assert.ok(await within(kept).findByRole("link", { name: named(b.detail.download.output) }));
  await closeDrawer(user);
  await user.click(screen.getByRole("button", { name: b.action.open.replace("{{id}}", "batch_old") }));
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByRole("link", { name: named(b.detail.download.input) });
  assert.equal((within(dialog).getByRole("button", { name: named(b.detail.download.output) }) as HTMLButtonElement).disabled, true);
  assert.equal((within(dialog).getByRole("button", { name: named(b.detail.download.error) }) as HTMLButtonElement).disabled, true);
});

test("cancel is offered only when the server says the batch is cancellable", async () => {
  serve([
    batch({ id: "batch_fin", status: "finalizing", finalizing_at: "2026-10-28T09:10:00Z" }),
    batch({ id: "batch_flag", status: "in_progress", cancellable: false }),
  ]);
  const user = mount();
  for (const id of ["batch_fin", "batch_flag"]) {
    await user.click(await screen.findByRole("button", { name: b.action.open.replace("{{id}}", id) }));
    const dialog = await screen.findByRole("dialog");
    await within(dialog).findByRole("link", { name: named(b.detail.download.input) });
    assert.ok(!within(dialog).queryByRole("button", { name: b.action.cancel }), `${id} offers cancel`);
    await closeDrawer(user);
  }
});

test("cancel is only offered while the batch can still be stopped, and only to an admin", async () => {
  serve([batch({ id: "batch_run" }), batch({ id: "batch_stop", status: "cancelling", cancelling_at: "2026-10-28T09:10:00Z" }), batch({ id: "batch_end", status: "completed" })]);
  const user = mount();
  await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_run") });
  await user.click(screen.getByRole("button", { name: b.action.open.replace("{{id}}", "batch_run") }));
  assert.ok(await within(await screen.findByRole("dialog")).findByRole("button", { name: b.action.cancel }));
  await closeDrawer(user);
  await user.click(screen.getByRole("button", { name: b.action.open.replace("{{id}}", "batch_stop") }));
  let dialog = await screen.findByRole("dialog");
  await within(dialog).findByText(b.detail.cancelPending);
  assert.ok(!within(dialog).queryByRole("button", { name: b.action.cancel }));
  await closeDrawer(user);
  await user.click(screen.getByRole("button", { name: b.action.open.replace("{{id}}", "batch_end") }));
  dialog = await screen.findByRole("dialog");
  await within(dialog).findByRole("link", { name: named(b.detail.download.input) });
  assert.ok(!within(dialog).queryByRole("button", { name: b.action.cancel }));
});

test("a user who is not an admin cannot cancel or create", async () => {
  stubAdmin("viewer");
  serve([batch({ id: "batch_run" })]);
  mount("/batches?detail=batch_run");
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByRole("link", { name: named(b.detail.download.input) });
  assert.ok(!within(dialog).queryByRole("button", { name: b.action.cancel }));
  // The open drawer hides the page behind it from the accessibility tree.
  assert.equal((screen.getByRole("button", { name: named(b.action.create), hidden: true }) as HTMLButtonElement).disabled, true);
});

const confirmation = () => screen.findByRole("dialog", { name: b.action.cancelTitle });
const confirmGone = () => waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.action.cancelTitle })));

test("declining the in-page confirmation sends no request; confirming cancels and the status follows", async () => {
  let status: Batch["status"] = "in_progress";
  const cancels: string[] = [];
  serve([]);
  batchApi.list = async () => page([batch({ id: "batch_run", status })]);
  batchApi.get = async (id) => { getCalls.push(id); return batch({ id, status, input_filename: "reqs.jsonl" }); };
  batchApi.cancel = async (id) => { cancels.push(id); status = "cancelling"; return batch({ id, status }); };
  const user = mount("/batches?detail=batch_run");
  const drawerDialog = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_run") });
  await user.click(await within(drawerDialog).findByRole("button", { name: b.action.cancel }));
  const first = await confirmation();
  assert.ok(within(first).getByText(b.action.cancelBody));
  await user.click(within(first).getByRole("button", { name: b.action.cancelKeep }));
  await confirmGone();
  assert.deepEqual(cancels, []);
  await user.click(within(drawerDialog).getByRole("button", { name: b.action.cancel }));
  const second = await confirmation();
  await user.click(within(second).getByRole("button", { name: b.action.cancelOk }));
  await waitFor(() => assert.deepEqual(cancels, ["batch_run"]));
  await within(drawerDialog).findByText(b.detail.cancelPending);
  assert.ok(within(drawerDialog).getAllByText(b.status.cancelling).length >= 1);
  assert.ok(!within(drawerDialog).queryByRole("button", { name: b.action.cancel }));
  await confirmGone();
});

test("the cancel confirmation works from the keyboard: Tab moves between its buttons, Escape declines, Enter confirms", async () => {
  const cancels: string[] = [];
  serve([batch({ id: "batch_run" })]);
  batchApi.cancel = async (id) => { cancels.push(id); return batch({ id, status: "cancelling" }); };
  const user = mount("/batches?detail=batch_run");
  const drawerDialog = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_run") });
  const trigger = await within(drawerDialog).findByRole("button", { name: b.action.cancel });
  trigger.focus();
  await user.keyboard("{Enter}");
  let modal = await confirmation();
  await waitFor(() => assert.ok(modal.parentElement?.parentElement?.contains(document.activeElement), "focus moves into the confirmation"));
  escape();
  await confirmGone();
  assert.deepEqual(cancels, []);
  assert.ok(screen.getByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_run") }), "Escape closed only the confirmation");
  trigger.focus();
  await user.keyboard("{Enter}");
  modal = await confirmation();
  await waitFor(() => assert.ok(modal.parentElement?.parentElement?.contains(document.activeElement), "focus moves into the confirmation"));
  const keep = within(modal).getByRole("button", { name: b.action.cancelKeep });
  const ok = within(modal).getByRole("button", { name: b.action.cancelOk });
  keep.focus();
  await user.tab();
  assert.ok(document.activeElement === ok, "Tab moves from keeping to confirming");
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(cancels, ["batch_run"]));
});

test("cancelling a batch that just ended shows the translated conflict", async () => {
  serve([batch({ id: "batch_run" })]);
  batchApi.cancel = async () => { throw new APIError("BATCH_NOT_CANCELLABLE", undefined, "batch already finished", 409); };
  const user = mount("/batches?detail=batch_run");
  const dialog = await screen.findByRole("dialog");
  await user.click(await within(dialog).findByRole("button", { name: b.action.cancel }));
  await user.click(within(await confirmation()).getByRole("button", { name: b.action.cancelOk }));
  const alert = await within(dialog).findByRole("alert");
  assert.equal(alert.textContent?.includes(b.detail.cancelFailed.replace("{{detail}}", c.apiError.BATCH_NOT_CANCELLABLE)), true);
});

test("the open drawer follows a running batch to its end", async () => {
  let status: Batch["status"] = "in_progress";
  let counts = { total: 10, completed: 4, failed: 1 };
  serve([]);
  batchApi.list = async () => page([batch({ id: "batch_run", status })]);
  batchApi.get = async (id) => { getCalls.push(id); return batch({ id, status, request_counts: counts, completed_at: status === "completed" ? "2026-10-28T10:00:00Z" : undefined }); };
  mount("/batches?detail=batch_run", 40);
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("5 / 10 done");
  status = "completed";
  counts = { total: 10, completed: 9, failed: 1 };
  await within(dialog).findByText("10 / 10 done");
  await within(dialog).findByText(b.detail.times.completed_at);
  await new Promise((resolve) => setTimeout(resolve, 100));
  const settled = getCalls.length;
  await new Promise((resolve) => setTimeout(resolve, 200));
  assert.equal(getCalls.length, settled, "the drawer stops refreshing once the batch ended");
});
