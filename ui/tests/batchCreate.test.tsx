import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, dictionary, model, page } from "./helpers/batchFixtures.ts";

// Creating a batch: the file and endpoint choice, the hint block, the upload
// state, an invalid file named by its line, and the keyboard paths.

const closeChannels = trackMessageChannels();
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
const { default: BatchesPage } = await import("../src/pages/BatchesPage.tsx");
const { default: BatchCreateModal } = await import("../src/pages/batches/BatchCreateModal.tsx");
const { batchErrorText, batchWarningText } = await import("../src/pages/batches/batchErrors.ts");
type Batch = import("../src/batchApi.ts").Batch;

// user-event does not set keyCode, which the dialogs read; the focused element receives the key.
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });

const b = locale.batches;
const c = locale.common;
const original = { list: batchApi.list, get: batchApi.get, create: batchApi.create, dictionary: batchApi.dictionary, me: api.me };

beforeEach(() => {
  localStorage.clear();
  api.me = (async () => ({ role: "admin" })) as never;
  batchApi.dictionary = async () => dictionary([model("qwen-chat", "running")]);
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

function mountPage() {
  render(
    <I18nextProvider i18n={i18n}><FeedbackHost />
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={["/batches"]}>
          <Routes><Route path="/batches" element={<BatchesPage />} /></Routes>
          <Probe />
        </MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

const jsonl = (name = "reqs.jsonl", size?: number) => {
  const file = new dom.window.File(['{"custom_id":"1"}\n'], name, { type: "application/x-ndjson" });
  if (size !== undefined) Object.defineProperty(file, "size", { value: size });
  return file;
};

async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  await waitFor(() => assert.equal((screen.getAllByRole("button", { name: b.action.create })[0] as HTMLButtonElement).disabled, false));
  await user.click(screen.getAllByRole("button", { name: b.action.create })[0]);
  const dialog = await screen.findByRole("dialog", { name: b.create.title });
  await waitFor(() => assert.ok(dialog.contains(document.activeElement), "initial dialog focus is settled"));
  return dialog;
}

// The dialog moves focus into itself shortly after it opens.
const focusIn = (dialog: HTMLElement) => waitFor(() => assert.ok(dialog.contains(document.activeElement), "focus moves into the dialog"));

const submitButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: b.create.submit }) as HTMLButtonElement;

test("creating a batch uploads the chosen file without an endpoint, closes the dialog and opens the new batch", async () => {
  const calls: Array<{ name: string; endpoint?: string }> = [];
  let created: Batch | undefined;
  batchApi.list = async () => page(created ? [created] : []);
  batchApi.get = async () => created!;
  batchApi.create = async (file, options) => {
    calls.push({ name: file.name, endpoint: options?.endpoint });
    created = batch({ id: "batch_new", status: "validating", input_filename: file.name, endpoint: "/v1/embeddings" });
    return created;
  };
  const user = mountPage();
  const dialog = await openCreate(user);
  assert.equal(submitButton(dialog).disabled, true, "nothing to create until a file is chosen");
  assert.equal(calls.length, 0);
  for (const hint of [b.create.hint.line, b.create.hint.unique, b.create.hint.sameUrl, b.create.hint.noStream]) {
    assert.ok(within(dialog).getByText(hint));
  }
  assert.ok(within(dialog).getByText("JSON Lines, at most 200 MB and 50,000 requests."));
  const fileInput = within(dialog).getByLabelText(b.create.file, { exact: false }) as HTMLInputElement;
  assert.equal(fileInput.getAttribute("accept"), ".jsonl");
  await user.upload(fileInput, jsonl());
  // The chosen file is named once, its size beside it.
  const chosen = await within(dialog).findByText((_, element) => element?.tagName === "SPAN" && /^reqs\.jsonl\S/.test(element.textContent ?? "") && element.children.length === 1);
  assert.match(chosen.textContent ?? "", /^reqs\.jsonl\d/);
  // The batch's endpoint is the file's: the dialog does not ask for one.
  assert.ok(!(within(dialog).queryByRole("combobox", { name: "Endpoint" })), "expected the element to be absent");
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(calls, [{ name: "reqs.jsonl", endpoint: undefined }]));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "/batches?detail=batch_new"));
  assert.ok(await screen.findByText(b.create.created.replace("{{id}}", "batch_new")));
  const drawer = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_new") });
  await within(drawer).findByText("reqs.jsonl");
});

test("a file asking for two endpoints is refused at the line that differs", async () => {
  batchApi.list = async () => page([]);
  batchApi.create = async () => {
    throw new APIError("BATCH_FILE_INVALID", { line: 3, reason: "mixed_endpoints", detail: "Line 3: the url differs from the first request's; a batch calls one endpoint" }, "Line 3: the url differs", 400);
  };
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl());
  await user.click(submitButton(dialog));
  const want = b.errors.fileLine.replace("{{line}}", "3").replace("{{reason}}", b.errors.fileReasons.mixed_endpoints);
  assert.ok(await within(dialog).findByText(want));
});

test("an invalid file names its line, keeps the dialog open and lets the file be replaced", async () => {
  batchApi.list = async () => page([]);
  let attempts = 0;
  batchApi.create = async () => {
    attempts += 1;
    if (attempts === 1) {
      throw new APIError("BATCH_FILE_INVALID", { line: 7, reason: "missing_custom_id", detail: "line 7: custom_id is required" }, "line 7: custom_id is required", 400);
    }
    return batch({ id: "batch_ok", input_filename: "fixed.jsonl" });
  };
  batchApi.get = async () => batch({ id: "batch_ok", input_filename: "fixed.jsonl" });
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl("bad.jsonl"));
  await user.click(submitButton(dialog));
  const alert = await within(dialog).findByText(b.errors.fileLine.replace("{{line}}", "7").replace("{{reason}}", b.errors.fileReasons.missing_custom_id));
  assert.ok(alert);
  assert.ok(within(dialog).getByText(b.errors.detail.replace("{{detail}}", "line 7: custom_id is required")));
  assert.ok(screen.getByRole("dialog", { name: b.create.title }), "the dialog stays open");
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl("fixed.jsonl"));
  assert.ok(!within(dialog).queryByText(/Line 7/), "choosing another file clears the error");
  await user.click(submitButton(dialog));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  assert.equal(attempts, 2);
});

test("a refusal because the file's models are served by nothing is shown in the dialog in the reader's language", async () => {
  batchApi.list = async () => page([]);
  batchApi.create = async () => {
    throw new APIError("BATCH_FILE_MODELS_UNKNOWN", { models: ["ghost-1", "ghost-2"], more: 0 }, "The input file names models that no active deployment serves: ghost-1, ghost-2", 400);
  };
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl("ghosts.jsonl"));
  await user.click(submitButton(dialog));
  const list = new Intl.ListFormat("en", { type: "conjunction" }).format(["ghost-1", "ghost-2"]);
  assert.ok(await within(dialog).findByText(c.apiError.BATCH_FILE_MODELS_UNKNOWN.replace("{{models, list}}", list)));
  assert.ok(screen.getByRole("dialog", { name: b.create.title }), "the dialog stays open");
});

test("the models of a file are joined with the reader's separators, the further ones counted", async () => {
  const many = { models: ["m1", "m2", "m3", "m4", "m5"], more: 3 };
  for (const lng of ["en", "zh"] as const) {
    const { i18n: local, locale: words } = await createI18n(["batches"], lng);
    const t = local.getFixedT(lng, "batches");
    const tc = local.getFixedT(lng, "common");
    const more = words.batches.errors.moreModels.replace("{{count}}", "3");
    const list = new Intl.ListFormat(lng, { type: "conjunction" }).format([...many.models, more]);
    const refusal = batchErrorText(t, tc, new APIError("BATCH_FILE_MODELS_UNKNOWN", many, "fallback", 400));
    assert.equal(refusal.message, words.common.apiError.BATCH_FILE_MODELS_UNKNOWN.replace("{{models, list}}", list), lng);
    assert.ok(lng === "zh" ? list.includes("、") && !list.includes(", ") : list.includes(", "), `${lng} separators: ${list}`);
    const warning = batchWarningText(t, { code: "BATCH_FILE_MODELS_SLEEP_HELD", params: { models: ["napping"], more: 0 } });
    assert.equal(warning, words.batches.create.warnings.BATCH_FILE_MODELS_SLEEP_HELD.replace("{{models, list}}", "napping"), lng);
    const single = batchWarningText(t, { code: "BATCH_MODEL_SLEEP_HELD", params: { model: "napping" } });
    assert.equal(single, words.batches.create.warnings.BATCH_MODEL_SLEEP_HELD.replace("{{model}}", "napping"), lng);
    // A model that waits names its state in the reader's words.
    const stopped = batchWarningText(t, { code: "BATCH_MODEL_WAITS", params: { model: "parked", state: "stopped" } });
    assert.equal(stopped, words.batches.create.warnings.BATCH_MODEL_WAITS.replace("{{model}}", "parked").replace("{{state}}", words.batches.create.model.state.stopped), lng);
    const failed = batchWarningText(t, { code: "BATCH_FILE_MODELS_WAIT", params: { models: ["a", "b"], more: 0, state: "failed" } });
    const pair = new Intl.ListFormat(lng, { type: "conjunction" }).format(["a", "b"]);
    assert.equal(failed, words.batches.create.warnings.BATCH_FILE_MODELS_WAIT.replace("{{models, list}}", pair).replace("{{state}}", words.batches.create.model.state.failed), lng);
    // Models of another endpoint are refused, named as a list with the endpoint.
    const other = batchErrorText(t, tc, new APIError("BATCH_FILE_MODELS_ENDPOINT", { models: ["bge"], more: 0, endpoint: "/v1/chat/completions" }, "fallback", 400));
    assert.equal(other.message, words.common.apiError.BATCH_FILE_MODELS_ENDPOINT.replace("{{endpoint}}", "/v1/chat/completions").replace("{{models, list}}", "bge"), lng);
    const one = batchErrorText(t, tc, new APIError("BATCH_MODEL_ENDPOINT", { model: "bge", endpoint: "/v1/chat/completions" }, "fallback", 400));
    assert.equal(one.message, words.common.apiError.BATCH_MODEL_ENDPOINT.replace("{{model}}", "bge").replace("{{endpoint}}", "/v1/chat/completions"), lng);
  }
});

test("an error about the whole file or the request is worded without a line", async () => {
  const t = i18n.getFixedT("en", "batches");
  const tc = i18n.getFixedT("en", "common");
  const whole = batchErrorText(t, tc, new APIError("BATCH_FILE_INVALID", { line: 0, reason: "empty_file", detail: "the file has no requests" }, "the file has no requests", 400));
  assert.equal(whole.message, b.errors.fileWhole.replace("{{reason}}", b.errors.fileReasons.empty_file));
  assert.equal(whole.detail, "the file has no requests");
  const mismatch = batchErrorText(t, tc, new APIError("BATCH_REQUEST_INVALID", { field: "endpoint", reason: "endpoint_mismatch" }, "endpoint mismatch", 400));
  assert.equal(mismatch.message, b.errors.requestInvalid.replace("{{field}}", "endpoint").replace("{{reason}}", b.errors.requestReasons.endpoint_mismatch));
  const unknown = batchErrorText(t, tc, new APIError("BATCH_FILE_INVALID", { line: 3, reason: "a_new_reason", detail: "line 3: something new" }, "line 3: something new", 400));
  assert.equal(unknown.message, c.apiError.BATCH_FILE_INVALID.replace("{{detail}}", "line 3: something new"), "an unknown reason falls back to the shared text");
  const tooLarge = batchErrorText(t, tc, new APIError("BATCH_FILE_TOO_LARGE", { limit_mb: 200 }, "too large", 413));
  assert.equal(tooLarge.message, c.apiError.BATCH_FILE_TOO_LARGE.replace("{{limit_mb}}", "200"));
  const plain = batchErrorText(t, tc, new Error("network down"));
  assert.equal(plain.message, "network down");
});

test("a file over the size limit is refused before any upload", async () => {
  batchApi.list = async () => page([]);
  let uploads = 0;
  batchApi.create = async () => { uploads += 1; return batch(); };
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl("huge.jsonl", 201 * 1024 * 1024));
  assert.ok(await within(dialog).findByText(b.create.tooLarge.replace("{{limit}}", "200")));
  assert.equal(submitButton(dialog).disabled, true);
  assert.equal(uploads, 0);
});

test("while uploading, the dialog is locked; a server error then unlocks it", async () => {
  batchApi.list = async () => page([]);
  let fail!: () => void;
  batchApi.create = () => new Promise<Batch>((_resolve, reject) => {
    fail = () => reject(new APIError("BATCH_REQUEST_INVALID", { field: "endpoint", reason: "endpoint_mismatch" }, "endpoint mismatch", 400));
  });
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl());
  await user.click(submitButton(dialog));
  await within(dialog).findByText(b.create.submitting);
  assert.equal((within(dialog).getByLabelText(b.create.file, { exact: false }) as HTMLInputElement).disabled, true);
  assert.equal((within(dialog).getByRole("button", { name: b.create.cancel }) as HTMLButtonElement).disabled, true);
  escape();
  assert.ok(screen.getByRole("dialog", { name: b.create.title }), "Escape does not abandon an upload in flight");
  fail();
  await within(dialog).findByText(b.errors.requestInvalid.replace("{{field}}", "endpoint").replace("{{reason}}", b.errors.requestReasons.endpoint_mismatch));
  assert.equal(submitButton(dialog).disabled, false);
});

test("the dialog is operable from the keyboard: Tab reaches the controls, Enter submits, Escape closes", async () => {
  batchApi.list = async () => page([]);
  const sent: string[] = [];
  batchApi.create = async (file) => { sent.push(file.name); return batch({ id: "batch_k" }); };
  batchApi.get = async () => batch({ id: "batch_k" });
  const user = mountPage();
  let dialog = await openCreate(user);
  await focusIn(dialog);
  escape();
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  dialog = await openCreate(user);
  const fileInput = within(dialog).getByLabelText(b.create.file, { exact: false });
  fileInput.focus();
  await user.upload(fileInput, jsonl("kb.jsonl"));
  await user.tab();
  assert.ok(document.activeElement === within(dialog).getByRole("button", { name: c.actions.remove }));
  await user.tab();
  assert.ok(document.activeElement === within(dialog).getByRole("spinbutton", { name: b.create.window.label }));
  const submit = submitButton(dialog);
  submit.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(sent, ["kb.jsonl"]));
});

test("the dialog can be opened and closed without creating anything", async () => {
  batchApi.list = async () => page([]);
  let uploads = 0;
  batchApi.create = async () => { uploads += 1; return batch(); };
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.click(within(dialog).getByRole("button", { name: b.create.cancel }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  assert.equal(uploads, 0);
});

test("the modal component reports nothing while closed", () => {
  render(
    <I18nextProvider i18n={i18n}><FeedbackHost />
      <BatchCreateModal open={false} onClose={() => {}} onCreated={() => {}} />
    </I18nextProvider>,
  );
  assert.ok(!screen.queryByRole("dialog"));
});

// The dialog is built from the server's dictionary: it says so while it is
// read, offers a retry when it cannot be read, and then shows what it says.
test("the dialog reads the batch dictionary: loading, a failed read with a retry, then the server's choices", async () => {
  // The page reads the dictionary too, for its status filter; the dialog's reads are what is scripted.
  let reachable = false;
  let answer: (value: ReturnType<typeof dictionary>) => void = () => {};
  batchApi.list = async () => page([]);
  batchApi.dictionary = () => (reachable ? new Promise((resolve) => { answer = resolve; }) : Promise.reject(new Error("server unreachable")));
  const user = mountPage();
  const dialog = await openCreate(user);
  const failure = await within(dialog).findByText(b.create.dictionary.failed);
  assert.ok(failure.closest('[role="alert"]'));
  assert.ok(within(dialog).getByText("server unreachable"));
  assert.equal(submitButton(dialog).disabled, true, "nothing to create with before the dictionary is read");
  assert.ok(!(within(dialog).queryByLabelText(b.create.file, { exact: false })), "expected the element to be absent");

  reachable = true;
  await user.click(within(dialog).getByRole("button", { name: b.create.dictionary.retry }));
  const loading = await within(dialog).findByRole("status");
  assert.ok(within(loading).getByText(b.create.dictionary.loading));
  assert.equal(submitButton(dialog).disabled, true);

  answer(dictionary([model("only-model", "asleep", ["/v1/embeddings"])], {
    endpoints: ["/v1/embeddings"], completion_window_hours: { min: 2, max: 12, default: 6 },
    max_file_bytes: 10 * 1024 * 1024, max_file_lines: 1000, output_retention_days: 7,
  }));
  const fileInput = await within(dialog).findByLabelText(b.create.file, { exact: false });
  assert.ok(!within(dialog).queryByText(b.create.dictionary.loading));
  // The server's endpoints for the sample, window bounds and default, limits and retention.
  assert.ok(within(dialog).getByText(b.create.sample.endpoints.embeddings.replace("{{endpoint}}", "/v1/embeddings"), { selector: '[data-slot="select-value"]' }));
  assert.equal((within(dialog).getByLabelText(b.create.window.label, { exact: false }) as HTMLInputElement).value, "6");
  assert.ok(within(dialog).getByText(i18n.t("batches:create.window.help", { min: 2, max: 12, default: 6 })));
  assert.ok(within(dialog).getByText(i18n.t("batches:create.hint.format", { size: 10, requests: 1000 })));
  assert.ok(within(dialog).getByText(i18n.t("batches:create.hint.retention", { days: 7 })));
  // The models are the server's, with its state; the sample uses the first.
  assert.ok(within(dialog).getByText(`only-model · ${b.create.model.state.asleep}`, { selector: '[data-slot="select-value"]' }));
  await user.upload(fileInput, jsonl("big.jsonl", 11 * 1024 * 1024));
  assert.ok(await within(dialog).findByText(i18n.t("batches:create.tooLarge", { limit: 10 })));
  assert.equal(submitButton(dialog).disabled, true, "a file over the server's limit is not sent");
});

test("a dictionary read again on every opening shows the models' states of now", async () => {
  let state = "running";
  batchApi.list = async () => page([]);
  batchApi.dictionary = async () => dictionary([model("m", state)]);
  const user = mountPage();
  let dialog = await openCreate(user);
  await within(dialog).findByText(`m · ${b.create.model.state.running}`, { selector: '[data-slot="select-value"]' });
  await user.click(within(dialog).getByRole("button", { name: b.create.cancel }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  state = "asleep_held";
  dialog = await openCreate(user);
  await waitFor(() => assert.equal((within(dialog).getByRole("button", { name: b.create.sample.use }) as HTMLButtonElement).disabled, true));
  assert.ok(!(within(dialog).queryByText(`m · ${b.create.model.state.asleepHeld}`, { selector: '[data-slot="select-value"]' })), "expected the element to be absent");
  await user.click(within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting }));
  await within(dialog).findByText(`m · ${b.create.model.state.asleepHeld}`, { selector: '[data-slot="select-value"]' });
});
