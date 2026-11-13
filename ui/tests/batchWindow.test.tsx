import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, dictionary, page } from "./helpers/batchFixtures.ts";

// The completion window of a new batch: a number of hours from 1 to 72,
// 24 by default, sent as completion_window, with the hint following it.

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

const b = locale.batches;
const original = { list: batchApi.list, get: batchApi.get, create: batchApi.create, dictionary: batchApi.dictionary, me: api.me };

beforeEach(() => {
  localStorage.clear();
  api.me = (async () => ({ role: "admin" })) as never;
  batchApi.dictionary = async () => dictionary();
  batchApi.list = (async () => page([])) as never;
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

const jsonl = () => new dom.window.File(['{"custom_id":"1"}\n'], "reqs.jsonl", { type: "application/x-ndjson" });
async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  await waitFor(() => assert.equal((screen.getAllByRole("button", { name: b.action.create })[0] as HTMLButtonElement).disabled, false));
  await user.click(screen.getAllByRole("button", { name: b.action.create })[0]);
  const dialog = await screen.findByRole("dialog", { name: b.create.title });
  await waitFor(() => assert.ok(dialog.contains(document.activeElement), "initial dialog focus is settled"));
  return dialog;
}
const submitButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: b.create.submit }) as HTMLButtonElement;
const windowInput = (dialog: HTMLElement) => within(dialog).getByRole("spinbutton", { name: b.create.window.label }) as HTMLInputElement;

test("the window defaults to 24 hours, which is what is sent", async () => {
  const sent: Array<number | undefined> = [];
  batchApi.create = (async (_file: File, options?: { completionWindowHours?: number }) => {
    sent.push(options?.completionWindowHours);
    return batch({ id: "batch_w", completion_window: "24h" });
  }) as never;
  batchApi.get = (async () => batch({ id: "batch_w" })) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  assert.equal(windowInput(dialog).value, "24");
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl());
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(sent, [24]));
});

test("a chosen window is sent in hours and the hint follows it", async () => {
  const sent: Array<number | undefined> = [];
  batchApi.create = (async (_file: File, options?: { completionWindowHours?: number }) => {
    sent.push(options?.completionWindowHours);
    return batch({ id: "batch_w", completion_window: "72h" });
  }) as never;
  batchApi.get = (async () => batch({ id: "batch_w", completion_window: "72h" })) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  const input = windowInput(dialog);
  await user.clear(input);
  await user.type(input, "72");
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl());
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(sent, [72]));
});

test("a window outside 1 to 72 hours or an empty one cannot be submitted", async () => {
  let uploads = 0;
  batchApi.create = (async () => { uploads += 1; return batch(); }) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file, { exact: false }), jsonl());
  assert.equal(submitButton(dialog).disabled, false);
  const input = windowInput(dialog);
  const help = b.create.window.help.replace("{{min}}", "1").replace("{{max}}", "72").replace("{{default}}", "24");
  assert.ok(within(dialog).getByText(help));
  await user.clear(input);
  assert.equal(submitButton(dialog).disabled, true, "empty");
  assert.ok(within(dialog).getByRole("alert"), "the help turns to an error");
  // The field clamps what is typed to its bounds on blur; the stored value stays valid.
  await user.type(input, "100");
  await user.tab();
  await waitFor(() => assert.equal(submitButton(dialog).disabled, false));
  assert.equal(input.value, "72");
  await user.clear(input);
  await user.type(input, "0");
  await user.tab();
  await waitFor(() => assert.equal(input.value, "1"));
  assert.equal(uploads, 0);
});

test("the detail drawer shows the window the batch was given", async () => {
  batchApi.list = (async () => page([batch({ id: "batch_72", completion_window: "72h" })])) as never;
  batchApi.get = (async () => batch({ id: "batch_72", completion_window: "72h" })) as never;
  const user = mountPage();
  await user.click(await screen.findByRole("button", { name: b.action.open.replace("{{id}}", "batch_72") }));
  const drawer = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_72") });
  const row = (await within(drawer).findByText(b.detail.completionWindow)).closest("dt")?.parentElement as HTMLElement;
  assert.ok(within(row).getByText("72h"));
});
