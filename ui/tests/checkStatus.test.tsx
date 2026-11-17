import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, cleanup } = await import("@testing-library/react");
const { CheckStatusBadge, checkStatusTone, worstCheckStatus } = await import("../src/checkStatus.tsx");

afterEach(cleanup);

async function mount(node: React.ReactNode, lng: "en" | "zh" = "en") {
  const { i18n } = await createI18n([], lng);
  return render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);
}

test("each of the five statuses reads differently, in English and Chinese", async () => {
  const en = ["Passed", "Warning", "Failed", "Not checked", "Not applicable"];
  const zh = ["通过", "告警", "失败", "未检查", "不适用"];
  for (const [lng, labels] of [["en", en], ["zh", zh]] as const) {
    await mount(<>{["pass", "warn", "fail", "skip", "na"].map((status) => <CheckStatusBadge key={status} status={status} />)}</>, lng);
    for (const label of labels) assert.ok(screen.getByText(label), `${lng}: ${label}`);
    cleanup();
  }
});

test("a skipped or not-applicable check is never drawn as passed", () => {
  assert.equal(checkStatusTone("pass"), "success");
  assert.equal(checkStatusTone("skip"), "neutral");
  assert.equal(checkStatusTone("na"), "neutral");
  assert.equal(checkStatusTone("warn"), "warning");
  assert.equal(checkStatusTone("fail"), "danger");
});

test("an unknown status from a newer server reads as not checked", async () => {
  await mount(<CheckStatusBadge status="ok" />);
  assert.ok(screen.getByText("Not checked"));
  assert.equal(screen.queryByText("Passed"), null);
});

test("the worst status ranks fail over warn over skip over pass", () => {
  assert.equal(worstCheckStatus([]), "na");
  assert.equal(worstCheckStatus(["pass", "na"]), "pass");
  assert.equal(worstCheckStatus(["pass", "skip"]), "skip");
  assert.equal(worstCheckStatus(["skip", "warn", "pass"]), "warn");
  assert.equal(worstCheckStatus(["warn", "fail"]), "fail");
  assert.equal(worstCheckStatus(["pass", "bogus"]), "skip");
});
