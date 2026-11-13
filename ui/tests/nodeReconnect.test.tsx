import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["nodes", "common"], "zh");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { NodeReconnectButton } = await import("../src/components/NodeReconnectButton.tsx");
const { api } = await import("../src/api.ts");
const { message } = await import("antd");
type NodeT = import("../src/api.ts").Node;

afterEach(() => cleanup());

const node = (over: Partial<NodeT>): NodeT => ({
  uuid: "n1", scheduling: "schedulable", state: "offline", hostname: "gpu-a",
  os: "linux", kernel: "5.15", arch: "amd64", cpu_model: "EPYC", cpu_cores: 64,
  mem_total_gib: 512, disk_total_gib: 1024, gpu_count: 2, gpus: [],
  has_ssh_credential: false, created_at: "2026-10-14T00:00:00Z", labels: {},
  ...over,
} as NodeT);

const show = (n: NodeT) =>
  render(<I18nextProvider i18n={i18n}><NodeReconnectButton node={n} onUpdated={() => {}} /></I18nextProvider>);

test("a connected node offers nothing to reconnect", () => {
  const { container } = show(node({ state: "online" }));
  assert.equal(container.textContent, "");
});

// A node the server is already restoring reads as connecting: there is
// nothing to bring back that the platform is not already bringing back.
test("a node being connected offers no reconnect button", () => {
  const { container } = show(node({ state: "connecting", recovery_phase: "reinstalling", has_ssh_credential: true }));
  assert.equal(container.textContent, "");
  assert.equal(screen.queryByRole("button"), null);
});

// The instance behind the node was destroyed: neither SSH nor the install
// command has anything to reach, so the button is there but disabled, with
// the reason on hover.
test("a node whose instance was destroyed cannot be reconnected, and says why", async () => {
  const user = userEvent.setup();
  for (const has_ssh_credential of [true, false]) {
    let probed = false;
    const origProbe = api.probeNodeRecovery;
    api.probeNodeRecovery = (async () => { probed = true; return {} as never; }) as never;
    try {
      show(node({ recovery_phase: "stopped", has_ssh_credential }));
      const button = screen.getByRole("button", { name: /重\s*连/ }) as HTMLButtonElement;
      assert.ok(button.disabled, "the button is disabled");
      await user.hover(button.parentElement as HTMLElement);
      await screen.findByText(/实例已销毁，没有可以重连的节点/);
      await user.click(button);
      assert.equal(probed, false, "a destroyed instance is never probed");
      assert.equal(screen.queryByRole("dialog"), null, "no install command is offered for a destroyed instance");
    } finally {
      api.probeNodeRecovery = origProbe;
      cleanup();
    }
  }
});

// With an SSH credential the platform does it itself.
test("reconnect over SSH runs the recovery probe", async () => {
  const user = userEvent.setup();
  const calls: string[] = [];
  const origProbe = api.probeNodeRecovery;
  const origNode = api.node;
  api.probeNodeRecovery = (async (uuid: string) => { calls.push(uuid); return { node_uuid: uuid, action: "probe", recovered: true, probe: {} }; }) as never;
  api.node = (async () => node({ has_ssh_credential: true })) as never;
  try {
    show(node({ has_ssh_credential: true }));
    await user.click(screen.getByRole("button", { name: /重\s*连/ }));
    await waitFor(() => assert.deepEqual(calls, ["n1"]));
  } finally {
    api.probeNodeRecovery = origProbe;
    api.node = origNode;
  }
});

test("a failed recovery probe leaves the reconnect action available", async (t) => {
  const user = userEvent.setup();
  const originalProbe = api.probeNodeRecovery;
  const originalNode = api.node;
  const failed = node({ state: "offline", recovery_phase: "probe_failed", has_ssh_credential: true, last_recovery_error: "connection refused" });
  let updated: NodeT | undefined;
  const success = t.mock.method(message, "success", () => {});
  api.probeNodeRecovery = (async () => ({ node_uuid: failed.uuid, action: "reconnect", recovered: false, phase: "probe_failed", error: "connection refused", probe: {} })) as never;
  api.node = (async () => failed) as never;
  try {
    const view = render(<I18nextProvider i18n={i18n}><NodeReconnectButton node={failed} onUpdated={(n) => { updated = n; }} /></I18nextProvider>);
    await user.click(screen.getByRole("button", { name: /重\s*连/ }));
    await waitFor(() => assert.equal(updated?.state, "offline"));
    view.rerender(<I18nextProvider i18n={i18n}><NodeReconnectButton node={updated!} onUpdated={() => {}} /></I18nextProvider>);
    assert.ok(screen.getByRole("button", { name: /重\s*连/ }));
    await screen.findByText("connection refused");
    assert.equal(success.mock.callCount(), 0, "failed recovery must not report success");
  } finally {
    api.probeNodeRecovery = originalProbe;
    api.node = originalNode;
  }
});

// Without one there is no way in but the node itself, so the button hands
// over the install command instead of being a dead end.
test("reconnect without an SSH credential hands over the install command", async () => {
  const user = userEvent.setup();
  let probed = false;
  const origProbe = api.probeNodeRecovery;
  const origNode = api.node;
  api.probeNodeRecovery = (async () => { probed = true; return {} as never; }) as never;
  api.node = (async () => node({ install_command: "curl -fsSL 'https://gw/install.sh' | sh -s -- --token 'abc'" })) as never;
  try {
    show(node({}));
    await user.click(screen.getByRole("button", { name: /重\s*连/ }));
    await screen.findByText(/curl -fsSL 'https:\/\/gw\/install.sh'/);
    assert.equal(probed, false, "a node with no credential is never probed over SSH");
  } finally {
    api.probeNodeRecovery = origProbe;
    api.node = origNode;
  }
});

test("a node with no recorded install command says so instead of showing an empty box", async () => {
  const user = userEvent.setup();
  const origNode = api.node;
  api.node = (async () => node({})) as never;
  try {
    show(node({}));
    await user.click(screen.getByRole("button", { name: /重\s*连/ }));
    await screen.findByText(/该节点没有安装命令/);
  } finally {
    api.node = origNode;
  }
});
