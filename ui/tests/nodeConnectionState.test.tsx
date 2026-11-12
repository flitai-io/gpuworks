import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["nodes"], "zh");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { NodeConnectionBadge } = await import("../src/components/NodeConnectionBadge.tsx");
type NodeT = import("../src/api.ts").Node;

afterEach(() => cleanup());

const show = (node: Partial<NodeT>) =>
  render(<I18nextProvider i18n={i18n}><NodeConnectionBadge node={node as NodeT} /></I18nextProvider>);

// The server folds an in-flight recovery into the state, so the node
// arrives as connecting; the phase is detail beside it, not a second
// status. The UI does not re-decide which phases mean that.
test("a node being restored reads as connecting with the phase as a note", () => {
  const phases: [string, string][] = [
    ["probing", "探测中"], ["artifact_check", "检查 Agent 安装包"], ["uploading", "上传 Agent"], ["verifying", "校验 Agent"],
    ["replacing", "替换 Agent"], ["restarting", "重启中"], ["reinstalling", "重装中"], ["reconnecting", "等待 Agent 重连"],
  ];
  for (const [phase, text] of phases) {
    const { container } = show({ state: "connecting", recovery_phase: phase });
    assert.ok(container.textContent?.includes("连接中"), `phase ${phase} reads as connecting`);
    assert.equal(screen.queryByText("在线"), null);
    assert.equal(screen.queryByText("离线"), null);
    assert.match(screen.getByRole("img").getAttribute("aria-label") ?? "", new RegExp(text), `phase ${phase} says what it is doing`);
    cleanup();
  }
});

// Once recovery has failed or been given up the node arrives offline; the
// reason stays as a note rather than becoming a state.
test("a recovery that gave up reads as offline with the reason kept", () => {
  for (const [phase, text] of [["unreachable", "不可达"], ["probe_failed", "恢复探测失败"], ["recovery_blocked", "恢复受阻"], ["stopped", "实例已销毁"]]) {
    const { container } = show({ state: "offline", recovery_phase: phase });
    assert.ok(container.textContent?.includes("离线"), `phase ${phase} reads as offline`);
    assert.equal(screen.queryByText("连接中"), null);
    assert.match(screen.getByRole("img").getAttribute("aria-label") ?? "", new RegExp(text));
    cleanup();
  }
  const { container } = show({ state: "offline" });
  assert.ok(container.textContent?.includes("离线"));
  assert.equal(screen.queryByRole("img"), null, "an offline node with no recovery has nothing to add");
});

test("a connected node is online whatever its cards are doing", () => {
  const { container } = show({ state: "online", recovery_phase: "unreachable" });
  assert.ok(container.textContent?.includes("在线"));
  assert.equal(screen.queryByRole("img"), null, "a stale recovery phase adds no warning to a connected node");
});

test("the badge shows the state, and the recovery detail only on demand", async () => {
  show({ state: "connecting", recovery_phase: "reinstalling", last_recovery_error: "ssh: handshake failed" });
  assert.ok(screen.getByText("连接中"), "a node being restored reads as connecting");
  assert.equal(screen.queryByText("重装中"), null, "the phase is not a second status beside it");
  const mark = screen.getByRole("img");
  assert.match(mark.getAttribute("aria-label") ?? "", /重装中[\s\S]*ssh: handshake failed/);
  await userEvent.setup().tab();
  await waitFor(() => assert.match(screen.getByRole("tooltip").textContent ?? "", /重装中/));
});

test("an online node carries no warning mark", () => {
  const { container } = show({ state: "online" });
  assert.ok(container.textContent?.includes("在线"));
  assert.equal(screen.queryByRole("img"), null);
});

test("keyboard focus reveals the saved report and recovery history only on demand", async () => {
  show({
    state: "connecting", recovery_phase: "probing", last_recovery_error: "ssh: handshake failed",
    last_seen_at: new Date(2026, 10, 12, 8, 15).toISOString(),
    last_probe_at: new Date(2026, 10, 12, 9, 20).toISOString(), recovery_attempts: 3,
  });
  assert.equal(screen.queryByText("最后成功上报"), null);
  await userEvent.setup().tab();
  const history = await screen.findByRole("tooltip");
  assert.equal(document.activeElement?.getAttribute("aria-describedby"), history.id);
  assert.match(history.textContent ?? "", /最后成功上报[\s\S]*2026-11-12 08:15/);
  assert.match(history.textContent ?? "", /最近恢复探测[\s\S]*2026-11-12 09:20/);
  assert.match(history.textContent ?? "", /已尝试恢复 3 次/);
  assert.match(history.textContent ?? "", /ssh: handshake failed/);
});

test("hovering a healthy node keeps unobserved times unknown and omitted zero attempts as zero", async () => {
  // Node DTO omits recovery_attempts when it is zero.
  for (const recovery_attempts of [undefined, 0]) {
    show({ state: "online", recovery_attempts });
    assert.equal(screen.queryByRole("img"), null);
    fireEvent.mouseOver(screen.getByLabelText(/^在线 · /));
    const history = await screen.findByRole("tooltip");
    assert.equal((history.textContent?.match(/未观测/g) ?? []).length, 2);
    assert.match(history.textContent ?? "", /已尝试恢复 0 次/);
    cleanup();
  }
});
