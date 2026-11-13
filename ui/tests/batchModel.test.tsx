import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, dictionary, model, page } from "./helpers/batchFixtures.ts";

// The model of a batch chosen on the platform: the select of the models the
// server's batch dictionary lists with their states, the override sent with the
// file, the check of the models the file names, and where the override shows
// afterwards.

const closeChannels = trackMessageChannels();
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { i18n, locale } = await createI18n(["batches"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { ConfigProvider } = await import("antd");
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { batchApi } = await import("../src/batchApi.ts");
const { default: BatchesPage } = await import("../src/pages/BatchesPage.tsx");
const { modelsInJSONL } = await import("../src/pages/batches/batchModels.ts");

const b = locale.batches;
const original = { list: batchApi.list, get: batchApi.get, create: batchApi.create, dictionary: batchApi.dictionary, me: api.me };

beforeEach(() => {
  localStorage.clear();
  api.me = (async () => ({ role: "admin" })) as never;
  batchApi.list = (async () => page([])) as never;
  batchApi.dictionary = async () => dictionary([
    model("qwen-chat", "running"), model("nap-model", "asleep"), model("warming", "starting"),
    model("held-model", "asleep_held"), model("offline-model", "offline"), model("parked", "stopped"), model("broken", "failed"),
  ]);
});
afterEach(() => {
  message.destroy();
  cleanup();
  Object.assign(batchApi, { list: original.list, get: original.get, create: original.create, dictionary: original.dictionary });
  api.me = original.me;
});
after(() => closeChannels());

function mountPage() {
  render(
    <I18nextProvider i18n={i18n}><FeedbackHost />
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={["/batches"]}>
          <Routes><Route path="/batches" element={<BatchesPage />} /></Routes>
        </MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

const requests = (...models: string[]) =>
  models.map((model, i) => JSON.stringify({ custom_id: `r${i}`, method: "POST", url: "/v1/chat/completions", body: { model, messages: [] } })).join("\n") + "\n";
const jsonl = (text: string, size?: number) => {
  const file = new dom.window.File([text], "reqs.jsonl", { type: "application/x-ndjson" });
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
const submitButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: b.create.submit }) as HTMLButtonElement;
const optionLabel = (name: string, state: string) => `${name} · ${state}`;

async function chooseModel(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement, label: string) {
  const trigger = within(dialog).getByRole("combobox", { name: b.create.model.label });
  trigger.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(trigger.getAttribute("aria-expanded"), "true"));
  await user.click(await screen.findByRole("option", { name: label }));
  await waitFor(() => assert.ok(!screen.queryByRole("listbox")));
}

test("modelsInJSONL lists each body.model once and skips lines that are not requests", () => {
  const text = `${requests("a", "b", "a")}not json\n\n{"custom_id":"x"}\n{"body":{"model":7}}\n`;
  assert.deepEqual(modelsInJSONL(text), ["a", "b"]);
});

test("the model select defaults to the server's ready models, including automatic wake", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.ok(within(dialog).getByRole("combobox", { name: b.create.model.label })));
  assert.ok(within(dialog).getByText(b.create.model.placeholder));
  await user.click(within(dialog).getByRole("combobox", { name: b.create.model.label }));
  const labels = (await screen.findAllByRole("option", { name: / · / })).map((node) => node.textContent);
  // Both selects - the batch's model and the sample's - list them.
  const listed = [...new Set(labels)];
  assert.deepEqual(listed, [
    optionLabel("qwen-chat", b.create.model.state.running),
    optionLabel("nap-model", b.create.model.state.asleep),
  ]);
});

test("waiting models require an explicit choice and hiding them clears a waiting selection", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  const toggle = within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting });
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  toggle.focus();
  await user.keyboard(" ");
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  await user.click(within(dialog).getByRole("combobox", { name: b.create.model.label }));
  const offered = (await screen.findAllByRole("option", { name: / · / })).map((node) => node.textContent);
  for (const name of ["warming", "held-model", "offline-model", "parked", "broken"]) {
    assert.ok(offered.some((label) => label?.startsWith(`${name} · `)), `${name} is offered after opt-in`);
  }
  await user.click(await screen.findByRole("option", { name: optionLabel("parked", b.create.model.state.stopped) }));
  assert.ok(await within(dialog).findByText(b.create.model.warning.stopped.replace("{{model}}", "parked")));
  await user.click(toggle);
  await waitFor(() => {
    assert.ok(!(within(dialog).queryByText(b.create.model.warning.stopped.replace("{{model}}", "parked"))), "expected the element to be absent");
    assert.ok(!(within(dialog).queryByText(optionLabel("parked", b.create.model.state.stopped), { selector: '[data-slot="select-value"]' })), "expected the element to be absent");
  });
});

test("a sample cannot silently use a waiting model and opt-out restores its ready default", async () => {
  batchApi.dictionary = async () => dictionary([model("parked", "stopped")]);
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.equal((within(dialog).getByRole("button", { name: b.create.sample.use }) as HTMLButtonElement).disabled, true));
  await user.click(within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting }));
  await waitFor(() => assert.equal((within(dialog).getByRole("button", { name: b.create.sample.use }) as HTMLButtonElement).disabled, false));
  assert.ok(within(dialog).getByText(optionLabel("parked", b.create.model.state.stopped), { selector: '[data-slot="select-value"]' }));
  await user.click(within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting }));
  await waitFor(() => assert.equal((within(dialog).getByRole("button", { name: b.create.sample.use }) as HTMLButtonElement).disabled, true));
});

test("the chosen model goes up with the file; without one the upload carries none", async () => {
  const calls: Array<string | undefined> = [];
  batchApi.create = (async (_file: File, options?: { model?: string }) => {
    const model = options?.model;
    calls.push(model);
    return batch({ id: "batch_m", status: "validating", model });
  }) as never;
  batchApi.get = (async () => batch({ id: "batch_m", status: "validating", model: calls[calls.length - 1] })) as never;
  const user = mountPage();
  let dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("qwen-chat")));
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(calls, [undefined]));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));

  const detail = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_m") });
  await user.click(within(detail).getByRole("button", { name: "Close" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("whatever")));
  await chooseModel(user, dialog, optionLabel("nap-model", b.create.model.state.asleep));
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(calls, [undefined, "nap-model"]));
});

test("a model whose sleep is held shows the warning, and the server's warning after creating is shown too", async () => {
  batchApi.create = (async (_file: File, options?: { model?: string }) =>
    batch({ id: "batch_h", status: "validating", model: options?.model, warnings: [{ code: "BATCH_MODEL_SLEEP_HELD", params: { model: options?.model } }] })) as never;
  batchApi.get = (async () => batch({ id: "batch_h", model: "held-model" })) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.click(within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting }));
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("held-model")));
  await chooseModel(user, dialog, optionLabel("held-model", b.create.model.state.asleepHeld));
  assert.ok(await within(dialog).findByText(b.create.model.warning.asleepHeld.replace("{{model}}", "held-model")));
  await user.click(submitButton(dialog));
  assert.ok(await screen.findByText(b.create.warnings.BATCH_MODEL_SLEEP_HELD.replace("{{model}}", "held-model")));
});

test("a stopped or failed model shows what becomes of its requests; a starting one does not", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.click(within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting }));
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("whatever")));
  await chooseModel(user, dialog, optionLabel("parked", b.create.model.state.stopped));
  assert.ok(await within(dialog).findByText(b.create.model.warning.stopped.replace("{{model}}", "parked")));
  await chooseModel(user, dialog, optionLabel("broken", b.create.model.state.failed));
  assert.ok(await within(dialog).findByText(b.create.model.warning.failed.replace("{{model}}", "broken")));
  await chooseModel(user, dialog, optionLabel("qwen-chat", b.create.model.state.running));
  await waitFor(() => assert.ok(!(within(dialog).queryByText(b.create.model.warning.failed.replace("{{model}}", "broken"))), "expected the element to be absent"));
});

test("the file's models are counted, and those without a deployment are warned about", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.ok(within(dialog).getByRole("combobox", { name: b.create.model.label })));
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("qwen-chat", "ghost-1", "nap-model", "ghost-2", "ghost-1")));
  assert.ok(await within(dialog).findByText("Models in the file (4): qwen-chat, ghost-1, nap-model, ghost-2"));
  const warning = await within(dialog).findByText(b.create.fileModels.unknown.replace("{{models}}", "ghost-1, ghost-2"));
  assert.ok(warning.closest('[data-status="warning"]'), "a warning, not an error");
  assert.equal((submitButton(dialog) as HTMLButtonElement).disabled, false, "the submit is not blocked");
});

test("a file whose models all have deployments raises no warning", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.ok(within(dialog).getByRole("combobox", { name: b.create.model.label })));
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("qwen-chat", "nap-model")));
  await within(dialog).findByText("Models in the file (2): qwen-chat, nap-model");
  assert.ok(!(dialog.querySelector('[data-status="warning"]')), "expected the element to be absent");
});

test("with a model chosen the file's other models are only said to be replaced, as information", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("ghost-1", "qwen-chat", "ghost-2")));
  await within(dialog).findByText(b.create.fileModels.unknown.replace("{{models}}", "ghost-1, ghost-2"));
  await chooseModel(user, dialog, optionLabel("qwen-chat", b.create.model.state.running));
  const info = await within(dialog).findByText(b.create.fileModels.replaced.replace("{{models}}", "ghost-1, ghost-2").replace("{{model}}", "qwen-chat"));
  assert.ok(info.closest('[data-status="info"]'));
  assert.ok(!(within(dialog).queryByText(/have no available deployment/)), "expected the element to be absent");
  assert.ok(!(dialog.querySelector('[data-status="warning"]')), "expected the element to be absent");
});

test("a file over 50 MB is not read: no model list, no warning, and the reader is told", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.ok(within(dialog).getByRole("combobox", { name: b.create.model.label })));
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl(requests("ghost-1"), 51 * 1024 * 1024));
  assert.ok(await within(dialog).findByText(b.create.fileModels.skipped));
  assert.ok(!(within(dialog).queryByText(/Models in the file/)), "expected the element to be absent");
  assert.ok(!(within(dialog).queryByText(/ghost-1/)), "expected the element to be absent");
  assert.ok(!(dialog.querySelector('[data-status="warning"]')), "expected the element to be absent");
});

test("the sample is built on the chosen model", async () => {
  let sent: File | null = null;
  batchApi.create = (async (file: File) => { sent = file; return batch(); }) as never;
  batchApi.get = (async () => batch()) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.ok(within(dialog).getByRole("combobox", { name: b.create.model.label })));
  await chooseModel(user, dialog, optionLabel("nap-model", b.create.model.state.asleep));
  assert.equal((within(dialog).getByRole("combobox", { name: b.create.sample.model }) as HTMLInputElement).disabled, true);
  await user.click(screen.getByRole("button", { name: b.create.sample.use }));
  await user.click(submitButton(dialog));
  await waitFor(() => assert.ok(sent));
  assert.equal(JSON.parse((await (sent as unknown as File).text()).split("\n")[0]).body.model, "nap-model");
});

test("the list and the detail drawer show the model a batch was given, and the per-line note when none", async () => {
  batchApi.list = (async () => page([
    batch({ id: "batch_with", model: "qwen-chat" }), batch({ id: "batch_without" }),
  ])) as never;
  batchApi.get = (async () => batch({ id: "batch_with", model: "qwen-chat" })) as never;
  const user = mountPage();
  const withRow = (await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_with") })).closest("tr") as HTMLElement;
  assert.ok(within(withRow).getByText("qwen-chat"));
  const withoutRow = screen.getByRole("button", { name: b.action.open.replace("{{id}}", "batch_without") }).closest("tr") as HTMLElement;
  assert.ok(within(withoutRow).getByText(b.columns.modelPerLine));
  await user.click(screen.getByRole("button", { name: b.action.open.replace("{{id}}", "batch_with") }));
  const drawer = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_with") });
  await within(drawer).findByText(b.detail.model);
  assert.ok(within(drawer).getByText("qwen-chat"));
});
