import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const en = await createI18n(["deploy"], "en");
const zh = await createI18n(["deploy"], "zh");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form } = await import("antd");
const { default: DeployAdvancedSection } = await import("../src/pages/deploy/DeployAdvancedSection.tsx");

afterEach(() => cleanup());

const mount = (i18n: typeof en.i18n) =>
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <DeployAdvancedSection advancedSpecs={[]} manualCommand={false} templateArgs={[]} />
      </Form>
    </I18nextProvider>,
  );

// The custom-args box is split like a shell command line, so a JSON value
// typed with double quotes loses them. The field's line says how it is split;
// its help mark says to single-quote such a value and shows the form that works.
test("the custom-args field tells the user to single-quote JSON values", async () => {
  mount(en.i18n);
  assert.ok(screen.getByText("Arguments outside the parameter dictionary, split like a shell command line"));
  assert.equal(screen.queryByText(/in single quotes/), null, "the quoting rule stays off the page");
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: /Advanced performance tuning/ }));
  await user.hover(await screen.findByRole("button", { name: "More information" }));
  const tip = await screen.findByRole("tooltip");
  assert.match(tip.textContent ?? "", /in single quotes/);
  assert.match(tip.textContent ?? "", /--compilation-config '\{"cudagraph_mode":"FULL_DECODE_ONLY"\}'/);
  assert.equal(screen.queryByText(/as written/), null);
});

test("the same hint is shipped in Chinese", async () => {
  mount(zh.i18n);
  assert.ok(screen.getByText(/按 shell 命令行拆分/));
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: /高级性能调优/ }));
  await user.hover(await screen.findByRole("button", { name: "说明" }));
  const tip = await screen.findByRole("tooltip");
  assert.match(tip.textContent ?? "", /单引号/);
  assert.match(tip.textContent ?? "", /--compilation-config '\{"cudagraph_mode":"FULL_DECODE_ONLY"\}'/);
});
