import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["deployments", "common"], "en");
const { cleanup, render, screen } = await import("@testing-library/react");
const InstanceNotes = (await import("../src/pages/InstanceNotes.tsx")).default;

afterEach(() => cleanup());

const text = locale.deployments.instanceNote as Record<string, string>;
type Warning = import("../src/api.ts").ProductWarning;
const show = (notes?: Warning[]) => render(<I18nextProvider i18n={i18n}><InstanceNotes notes={notes} /></I18nextProvider>);

test("an instance with no notes shows nothing", () => {
  show(undefined);
  assert.equal(document.body.textContent, "");
  cleanup();
  show([]);
  assert.equal(document.body.textContent, "");
});

test("a card shared with processes outside the platform is explained with its figures", () => {
  show([{ code: "INSTANCE_CARD_SHARED_OUTSIDE", params: { gpu_indexes: [0, 2], foreign_bytes: 67089334272, foreign_gib: "62.5" } }]);
  assert.ok(screen.getByText(text.INSTANCE_CARD_SHARED_OUTSIDE.replace("{{gpu_indexes}}", "0, 2").replace("{{foreign_gib}}", "62.5")));
  assert.ok(screen.getByRole("status"));
});

test("unknown memory and several notes each render; an unknown code shows its code", () => {
  show([
    { code: "INSTANCE_CARD_SHARED_OUTSIDE_UNKNOWN", params: { gpu_indexes: [1] } },
    { code: "SOME_FUTURE_NOTE", params: {} },
  ]);
  assert.ok(screen.getByText(text.INSTANCE_CARD_SHARED_OUTSIDE_UNKNOWN.replace("{{gpu_indexes}}", "1")));
  assert.ok(screen.getByText("SOME_FUTURE_NOTE"));
  assert.equal(screen.getAllByRole("status").length, 2);
});
