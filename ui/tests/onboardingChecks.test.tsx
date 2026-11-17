import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, cleanup, fireEvent } = await import("@testing-library/react");
const { default: OnboardingChecks, countOnboardingChecks, onboardingCheckText } = await import("../src/components/OnboardingChecks.tsx");

afterEach(cleanup);

// What the server sends for a precheck (provision.CheckResult on the wire).
const checks = [
  { id: "os_arch", status: "pass", evidence: "Linux x86_64 6.8.0" },
  { id: "install_dir_space", status: "warn", code: "CHECK_INSTALL_DIR_SPACE_LOW", params: { available: "12.0 GiB", total: "40.0 GiB", minimum: "50.0 GiB" }, message: "12.0 GiB free of 40.0 GiB" },
  { id: "ssh", status: "fail", code: "CHECK_SSH_CONNECT_FAILED", params: { target: "203.0.113.7" }, message: "the server could not open an SSH session", evidence: "dial tcp 203.0.113.7:22: connect: connection refused" },
  { id: "install_dir_ownership", status: "skip", code: "CHECK_INSTALL_DIR_CHOWN_UNTESTED", params: { dir: "/opt" }, message: "not checked" },
  { id: "install_dir_traversal", status: "na", code: "CHECK_NOT_NEEDED_ON_MAC", message: "not needed on a Mac" },
] as const;

async function mount(lng: "en" | "zh") {
  const { i18n } = await createI18n([], lng);
  return render(
    <I18nextProvider i18n={i18n}>
      <OnboardingChecks checks={[...checks]} labels={{ item: "Check", status: "Status", detail: "Details" }} />
    </I18nextProvider>,
  );
}

test("every row reads in English from its code, with its status distinct", async () => {
  await mount("en");
  for (const text of [
    "OS / architecture", "Linux x86_64 6.8.0",
    "Install directory free space", "12.0 GiB free of 40.0 GiB; engine images and model weights need about 50.0 GiB or more.",
    "The server could not open an SSH session to 203.0.113.7.",
    "No file could be created in /opt with privileges, so this was not checked.",
    "Not needed on a Mac: engines run as the agent's own user.",
    "Passed", "Warning", "Failed", "Not checked", "Not applicable",
  ]) assert.ok(screen.getByText(text), text);
  // Only the one real pass reads as passed.
  assert.equal(screen.getAllByText("Passed").length, 1);
  // The SSH library's own words are evidence, behind a disclosure.
  const summary = screen.getByText("Output");
  fireEvent.click(summary);
  assert.ok(screen.getByText("dial tcp 203.0.113.7:22: connect: connection refused"));
});

test("every row reads in Chinese from its code", async () => {
  await mount("zh");
  for (const text of [
    "操作系统 / 架构", "安装目录可用空间", "可用 12.0 GiB，共 40.0 GiB；引擎镜像与模型权重约需 50.0 GiB 以上。",
    "server 无法与 203.0.113.7 建立 SSH 会话。", "通过", "告警", "失败", "未检查", "不适用", "输出",
  ]) assert.ok(screen.getByText(text), text);
  assert.equal(screen.queryByText("the server could not open an SSH session"), null, "the English fallback is not shown when the code is known");
});

test("counts leave na out and count an unknown status as skip", () => {
  assert.deepEqual(countOnboardingChecks([...checks, { id: "x", status: "later" as never }]), { pass: 1, warn: 1, fail: 1, skip: 2 });
});

test("an unknown code falls back to the server's English", async () => {
  const { i18n } = await createI18n([], "zh");
  const t = i18n.getFixedT("zh", "common") as never;
  assert.equal(onboardingCheckText(t, { id: "x", status: "warn", code: "CHECK_FROM_A_NEWER_SERVER", message: "a newer fallback" }), "a newer fallback");
  assert.equal(onboardingCheckText(t, { id: "x", status: "pass", evidence: "observed" }), "observed");
});
