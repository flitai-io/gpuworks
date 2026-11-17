import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["nodes"], "zh");
const { cleanup, render, screen } = await import("@testing-library/react");
const { NodeConnectionBadge } = await import("../src/components/NodeConnectionBadge.tsx");
const { recoveryErrorText, recoverySkippedText } = await import("../src/nodes/recoveryError.ts");
type NodeT = import("../src/api.ts").Node;

afterEach(() => cleanup());

const show = (node: Partial<NodeT>) =>
  render(<I18nextProvider i18n={i18n}><NodeConnectionBadge node={node as NodeT} /></I18nextProvider>);

// A coded recovery failure reads as a sentence in the reader's language, with
// the recorded SSH error kept beneath it as evidence.
test("a coded recovery error reads localized with its evidence", () => {
  show({ state: "offline", recovery_phase: "probe_failed", last_recovery_code: "SSH_AUTH_FAILED", last_recovery_error: "ssh: unable to authenticate (tried: password)" });
  const label = screen.getByRole("img").getAttribute("aria-label") ?? "";
  assert.match(label, /节点拒绝了 SSH 凭据。\nssh: unable to authenticate \(tried: password\)/);
});

// A node recorded before codes keeps its text, shown as written.
test("a recovery error from before codes is shown as written", () => {
  show({ state: "offline", recovery_phase: "unreachable", last_recovery_error: "network: ssh dial: connection refused" });
  const label = screen.getByRole("img").getAttribute("aria-label") ?? "";
  assert.match(label, /network: ssh dial: connection refused/);
  assert.doesNotMatch(label, /无法连接/);
});

test("every recovery code has a sentence and a skipped round says why", () => {
  const t = i18n.getFixedT("zh", "nodes");
  for (const code of ["SSH_UNREACHABLE", "SSH_TIMEOUT", "SSH_CLOSED_BEFORE_IDENT", "SSH_HANDSHAKE_FAILED", "SSH_HOST_KEY_MISMATCH", "SSH_CREDENTIAL_INVALID",
    "SSH_SESSION_FAILED", "SSH_COMMAND_FAILED", "SSH_CONNECTION_LOST", "AGENT_INSTALL_BINARY_UNAVAILABLE", "AGENT_INSTALL_UPLOAD_STALLED",
    "AGENT_INSTALL_FOREIGN_SERVER", "AGENT_INSTALL_BINARY_MISSING", "AGENT_INSTALL_DIR_NOT_WRITABLE", "AGENT_INSTALL_START_FAILED",
    "RECOVERY_AGENT_NOT_CONNECTED", "RECOVERY_NO_RECONNECT", "RECOVERY_PROVIDER_INSTANCE_GONE", "RECOVERY_STEP_FAILED"]) {
    const text = recoveryErrorText(t, code, "evidence");
    assert.ok(text.endsWith("\nevidence") && !text.startsWith("evidence"), `${code} = ${text}`);
  }
  assert.equal(recoverySkippedText(t, "RECOVERY_SKIPPED_NO_SSH_CREDENTIAL", "ssh credential is not configured"), "节点没有 SSH 凭据。");
  assert.equal(recoverySkippedText(t, undefined, "ssh credential is not configured"), "ssh credential is not configured");
  // An unknown code from a newer server falls back to the recorded text.
  assert.equal(recoveryErrorText(t, "SSH_SOMETHING_NEW", "raw"), "raw");
});
