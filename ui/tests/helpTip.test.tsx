import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, waitFor, cleanup } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { HelpTip } = await import("../src/ui-kit/HelpTip.tsx");

afterEach(cleanup);

async function mount(node: React.ReactNode, lng: "en" | "zh" = "en") {
  const { i18n } = await createI18n([], lng);
  return render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);
}

test("the explanation stays off the page until the user asks for it", async () => {
  await mount(<p>Estimated start 5 min <HelpTip content="Weight size ÷ reference rate + init time." /></p>);
  assert.equal(screen.queryByText("Weight size ÷ reference rate + init time."), null);
  assert.ok(screen.getByText("Estimated start 5 min", { exact: false }));
});

test("keyboard focus opens the explanation and Escape closes it", async () => {
  await mount(<HelpTip content="Weight size ÷ reference rate + init time." />);
  const user = userEvent.setup();
  await user.tab();
  const trigger = screen.getByRole("button", { name: "More information" });
  assert.equal(document.activeElement, trigger);
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, "Weight size ÷ reference rate + init time.");
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("tooltip"), null));
});

test("hover opens it too, and a caller label names the trigger", async () => {
  await mount(<HelpTip label="How the estimate is made" content="Reference rate, not measured." />);
  const user = userEvent.setup();
  await user.hover(screen.getByRole("button", { name: "How the estimate is made" }));
  assert.equal((await screen.findByRole("tooltip")).textContent, "Reference rate, not measured.");
});

test("the default trigger name is localized", async () => {
  await mount(<HelpTip content="x" />, "zh");
  assert.ok(screen.getByRole("button", { name: "说明" }));
});

test("the trigger is a plain button so it never submits a form", async () => {
  await mount(<form><HelpTip content="x" /></form>);
  assert.equal(screen.getByRole("button").getAttribute("type"), "button");
});
