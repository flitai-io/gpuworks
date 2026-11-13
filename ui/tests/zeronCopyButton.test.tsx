import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, fireEvent, waitFor, cleanup } = await import("@testing-library/react");
const { default: CopyButton } = await import("../src/components/CopyButton.tsx");
const { i18n } = await createI18n([]);
afterEach(cleanup);

test("copy preserves the full value and offers selectable text when the browser refuses", async () => {
  const value = "batch_identifier_with_a_long_untruncated_suffix";
  let copied = "";
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { copied = text; } } });
  const view = render(<I18nextProvider i18n={i18n}><CopyButton text={value} label="Copy identifier" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("button", { name: "Copy identifier" }));
  await waitFor(() => assert.equal(copied, value));
  view.unmount();
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async () => { throw new Error("denied"); } } });
  Object.defineProperty(document, "execCommand", { configurable: true, value: () => false });
  render(<I18nextProvider i18n={i18n}><CopyButton text={value} label="Copy identifier" /></I18nextProvider>);
  fireEvent.click(screen.getByRole("button", { name: "Copy identifier" }));
  const input = await screen.findByRole("textbox", { name: "Copy manually" }) as HTMLTextAreaElement;
  assert.equal(input.value, value);
  fireEvent.focus(input);
  assert.equal(input.selectionEnd - input.selectionStart, value.length);
  fireEvent.click(screen.getByRole("button", { name: "Done" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
});
