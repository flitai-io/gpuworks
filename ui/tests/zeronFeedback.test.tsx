import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { FeedbackHost, message, confirm } = await import("../src/ui-kit/feedback/index.tsx");
const { i18n } = await createI18n([]);
afterEach(() => { cleanup(); message.destroy(); });
const mount = () => render(<I18nextProvider i18n={i18n}><FeedbackHost /></I18nextProvider>);

test("keyed messages update and can be explicitly dismissed", async () => {
  mount();
  act(() => { message.info({ content: "Working", key: "work", duration: 0 }); });
  await screen.findByText("Working");
  act(() => { message.success({ content: "Done", key: "work", duration: 0 }); });
  await screen.findByText("Done", {}, { mutationObserverOptions: { childList: true, subtree: true } });
  await waitFor(() => assert.ok(!screen.queryByText("Working")));
  act(() => { message.destroy("work"); });
  await waitFor(() => assert.ok(!screen.queryByText("Done")), { mutationObserverOptions: { childList: true, subtree: true } });
});
test("confirm blocks dismissal during submission and preserves failed context for retry", async () => {
  mount();
  let reject!: (reason: Error) => void;
  let calls = 0;
  let cancelled = false;
  act(() => { confirm({ title: "Remove resource", content: "Existing data", onCancel: () => { cancelled = true; }, onOk: () => ++calls === 1 ? new Promise((_, no) => { reject = no; }) : Promise.resolve() }); });
  fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
  assert.equal((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled, true);
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
  assert.equal(cancelled, false);
  await act(async () => reject(new Error("Server unavailable")));
  await screen.findByRole("alert");
  assert.ok(screen.getByText("Existing data"));
  fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  assert.equal(calls, 2);
});
test("confirm cancel invokes its callback without accepting", async () => {
  mount();
  let cancelled = false;
  let accepted = false;
  act(() => { confirm({ title: "Cancel action", onOk: () => { accepted = true; }, onCancel: () => { cancelled = true; } }); });
  fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  assert.equal(cancelled, true);
  assert.equal(accepted, false);
});

test("confirm honours custom labels, a disabled accept, update and destroy", async () => {
  mount();
  let handle!: ReturnType<typeof confirm>;
  act(() => { handle = confirm({ title: "Drain node", okText: "Drain", cancelText: "Not now", okButtonProps: { danger: true, disabled: true } }); });
  const drain = await screen.findByRole("button", { name: "Drain" }) as HTMLButtonElement;
  assert.equal(drain.disabled, true);
  assert.ok(screen.getByRole("button", { name: "Not now" }));
  act(() => { handle.update({ title: "Drain node-2", okButtonProps: { danger: true } }); });
  await screen.findByRole("dialog", { name: "Drain node-2" });
  assert.equal((screen.getByRole("button", { name: "Drain" }) as HTMLButtonElement).disabled, false);
  act(() => { handle.destroy(); });
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
});

test("confirm words a non-Error failure generically and ignores a second accept while pending", async () => {
  mount();
  let reject!: (reason: unknown) => void;
  let calls = 0;
  act(() => { confirm({ title: "Restart", onOk: () => { calls++; return new Promise((_, no) => { reject = no; }); } }); });
  const accept = await screen.findByRole("button", { name: "Confirm" });
  fireEvent.click(accept);
  fireEvent.click(accept);
  assert.equal(calls, 1);
  await act(async () => reject("network"));
  assert.equal((await screen.findByRole("alert")).textContent, "Request failed. Please retry.");
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
});

test("queued confirmations show one at a time, in order, and an update touches only its own", async () => {
  mount();
  let second!: ReturnType<typeof confirm>;
  act(() => { confirm({ title: "First" }); second = confirm({ title: "Second" }); });
  await screen.findByRole("dialog", { name: "First" });
  act(() => { second.update({ title: "Second, updated" }); });
  assert.ok(screen.getByRole("dialog", { name: "First" }), "the shown confirmation is not changed by another's update");
  assert.equal(screen.queryByRole("dialog", { name: "Second" }), null);
  fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  await screen.findByRole("dialog", { name: "Second, updated" });
  fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
});
