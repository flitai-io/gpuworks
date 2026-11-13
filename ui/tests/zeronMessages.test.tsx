import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, waitFor, cleanup, act } = await import("@testing-library/react");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { i18n } = await createI18n([]);
afterEach(() => { cleanup(); message.destroy(); });
const mount = () => render(<I18nextProvider i18n={i18n}><FeedbackHost /></I18nextProvider>);

// Its own file on purpose: a message leaves only after its exit animation. After
// an earlier test in the same process animated a toast or popup, later toasts
// stay at their initial frame (opacity 0) and never leave; helpers/dom.ts drops
// the frames a finished test left pending, which likely stalls framer-motion's
// frame loop. First in its own process, the timing below is real.
test("plain messages: a warning shows, a short duration expires, and loading stays until its dismisser runs", async () => {
  mount();
  let stopLoading!: () => void;
  act(() => {
    message.warning("Disk almost full");
    message.success("Saved", 0.05);
    stopLoading = message.loading("Uploading");
  });
  await screen.findByText("Disk almost full");
  await screen.findByText("Uploading");
  await waitFor(() => assert.ok(!screen.queryByText("Saved"), "a 50 ms message expires"), { mutationObserverOptions: { childList: true, subtree: true } });
  assert.ok(screen.getByText("Uploading"), "loading has no timeout");
  act(() => { stopLoading(); });
  await waitFor(() => assert.ok(!screen.queryByText("Uploading"), "loading dismissed"), { mutationObserverOptions: { childList: true, subtree: true } });
});
