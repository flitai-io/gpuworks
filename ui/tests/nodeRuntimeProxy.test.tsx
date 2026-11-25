import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["nodes", "common"], "en");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const NodePullProxy = (await import("../src/components/NodePullProxy.tsx")).default;
const { dropInContent, DOCKER_PROXY_DROPIN_PATH } = await import("../src/components/NodeRuntimeProxy.tsx");

afterEach(() => cleanup());

const rt = locale.nodes.runtimeProxy as Record<string, string>;
const fill = (template: string, values: Record<string, string>) => template.replace(/\{\{(\w+)\}\}/g, (_, key) => values[key]);
const PROXY = "http://10.0.0.8:3128";

type Proxy = import("../src/api.ts").RuntimeProxy;
type Network = import("../src/api.ts").NodePullProxy;
type Effective = { http_proxy?: string; https_proxy?: string; no_proxy?: string };

const proxyState = (over: Partial<Proxy> = {}): Proxy => ({
  http_proxy: "", https_proxy: "", no_proxy: "", configured_http_proxy: "", configured_https_proxy: "",
  written_http_proxy: "", written_https_proxy: "", written_no_proxy: "",
  live_restore: false, daemon_json_proxy: false, systemd: true, privilege: "root", applied: false,
  ...over,
});
// The entry the node uses; an empty effective config means the node matches no entry.
const networkOf = (runtime: Network["runtime"], effective: Effective = { http_proxy: PROXY, https_proxy: PROXY, no_proxy: "harbor.corp.example,localhost" }): Network => ({
  proxy: Object.keys(effective).length === 0 ? null : {
    id: 1, position: 0, http_proxy: effective.http_proxy ?? "", https_proxy: effective.https_proxy ?? "", no_proxy: effective.no_proxy ?? "",
    username: "", has_password: false, control_plane: false, note: "", node_requirements: {}, matched_nodes: 1,
  },
  runtime,
});
const dockerNetwork = (over: Partial<Proxy> = {}) => networkOf({ driver: "docker", proxy: proxyState(over) });

interface Stub {
  restarts: string[];
  fetches: number;
  current: { network: Network | Error };
  restartImpl: () => Promise<import("../src/api.ts").NodeRuntime>;
  restore: () => void;
}

function stubAPI(initial: Network | Error, role = "admin"): Stub {
  const original = { nodePullProxy: apiModule.api.nodePullProxy, restart: apiModule.api.restartNodeRuntime, me: apiModule.api.me };
  const stub: Stub = {
    restarts: [], fetches: 0, current: { network: initial },
    restartImpl: async () => ({ driver: "docker", proxy: proxyState({ applied: true, http_proxy: PROXY, written_http_proxy: PROXY }) }),
    restore: () => {
      apiModule.api.nodePullProxy = original.nodePullProxy;
      apiModule.api.restartNodeRuntime = original.restart;
      apiModule.api.me = original.me;
    },
  };
  apiModule.api.me = async () => ({ role }) as never;
  apiModule.api.nodePullProxy = async () => {
    stub.fetches++;
    if (stub.current.network instanceof Error) throw stub.current.network;
    return stub.current.network;
  };
  apiModule.api.restartNodeRuntime = async (uuid: string) => {
    stub.restarts.push(uuid);
    return stub.restartImpl();
  };
  return stub;
}

function show(statusPollMs?: number) {
  return render(<MemoryRouter><I18nextProvider i18n={i18n}><NodePullProxy nodeUUID="node-1" statusPollMs={statusPollMs} /></I18nextProvider></MemoryRouter>);
}
const loaded = () => waitFor(() => {
  assert.ok(screen.getByText(locale.nodes.pullProxy.title));
  assert.equal(dom.window.document.querySelector("[aria-busy=\"true\"]") === null, true, "still loading");
});
const driverLine = (driver: string) => fill(rt.driver, { driver });
const restartButton = () => screen.queryByRole("button", { name: rt.restart });
// A dismissed dialog may stay in the DOM while its exit motion runs; Base UI
// marks it closed meanwhile.
const dialogClosed = () => {
  const dialog = screen.queryByRole("dialog");
  return !dialog || dialog.hasAttribute("data-closed") || dialog.hasAttribute("data-ending-style");
};
const bodyText = () => dom.window.document.body.textContent || "";

function stubClipboard() {
  const copied: string[] = [];
  Object.defineProperty(dom.window.navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { copied.push(text); } } });
  return copied;
}

test("while the configuration loads nothing about the runtime is shown, and a failed load shows the error alone", async () => {
  const stub = stubAPI(dockerNetwork());
  try {
    show();
    assert.ok(!screen.queryByText(driverLine("docker")), "no runtime line before the data arrives");
    await loaded();
    assert.ok(screen.getByText(driverLine("docker")));
  } finally {
    stub.restore();
  }
  cleanup();
  const failing = stubAPI(new Error("node unreachable"));
  try {
    show();
    await waitFor(() => assert.ok(screen.getByText(locale.nodes.pullProxy.loadFailed)));
    assert.ok(!screen.queryByText(driverLine("docker")));
    assert.ok(!restartButton());
  } finally {
    failing.restore();
  }
});

test("a docker node whose dockerd uses the platform proxy reads as applied", async () => {
  const stub = stubAPI(dockerNetwork({ applied: true, http_proxy: PROXY, written_http_proxy: PROXY, live_restore: true }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(driverLine("docker")));
    assert.ok(screen.getByText(rt.applied));
    assert.ok(screen.getByText(fill(rt.using, { proxy: PROXY })));
    assert.ok(!screen.queryByText(rt.notApplied));
    assert.ok(!restartButton(), "nothing to restart when the proxy already applies");
    assert.ok(screen.getByText(`${rt.liveRestoreOn} · ${rt.systemd} · ${rt.privilegeRoot}`));
  } finally {
    stub.restore();
  }
});

test("applied with no proxy at all says dockerd connects directly", async () => {
  const stub = stubAPI(dockerNetwork({ applied: true }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(fill(rt.using, { proxy: rt.direct })));
  } finally {
    stub.restore();
  }
});

test("with no proxy set for the node, dockerd's own proxy is shown and not judged", async () => {
  const own = "http://dev1.example:7890";
  const stub = stubAPI(networkOf({ driver: "docker", proxy: proxyState({ applied: true, http_proxy: own, https_proxy: own }) }, {}));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(fill(rt.unmanaged, { proxy: own })));
    assert.ok(!screen.queryByText(rt.applied), "nothing the platform set to call applied");
    assert.ok(!screen.queryByText(rt.notApplied));
    assert.ok(!restartButton(), "nothing to restart for");
  } finally {
    stub.restore();
  }
});

test("a proxy cleared for the node that dockerd still runs is flagged until dockerd restarts", async () => {
  const old = "http://10.255.255.1:3128";
  const stub = stubAPI(networkOf({ driver: "docker", proxy: proxyState({ applied: false, http_proxy: old, https_proxy: old }) }, {}));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(rt.notApplied), "the cleared proxy is still in effect");
    assert.ok(screen.getByText(fill(rt.stillUsing, { proxy: old })));
    assert.ok(!screen.queryByText(fill(rt.unmanaged, { proxy: old })), "not presented as dockerd's own proxy");
    await waitFor(() => assert.ok(restartButton(), "a restart applies the change"));
  } finally {
    stub.restore();
  }
});

test("dockerd still on another proxy is flagged, with a restart button for an admin only", async () => {
  const stale = dockerNetwork({ http_proxy: "http://old.example:8080", written_http_proxy: PROXY });
  const stub = stubAPI(stale);
  try {
    show();
    await loaded();
    const badge = screen.getByText(rt.notApplied).closest('[data-slot="badge"]') as HTMLElement;
    assert.equal(badge.dataset.status, "warning", "flagged as a warning");
    assert.ok(screen.getByText(fill(rt.stillUsing, { proxy: "http://old.example:8080" })));
    await waitFor(() => assert.ok(restartButton()));
  } finally {
    stub.restore();
  }
  cleanup();
  const viewer = stubAPI(stale, "viewer");
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(rt.notApplied));
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.ok(!restartButton(), "non-admins cannot restart dockerd");
  } finally {
    viewer.restore();
  }
});

test("a direct-connection dockerd that should use a proxy says so", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "", written_http_proxy: PROXY }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(fill(rt.stillUsing, { proxy: rt.direct })));
  } finally {
    stub.restore();
  }
});

test("a proxy in daemon.json wins: the reason is shown and no restart is offered", async () => {
  const stub = stubAPI(dockerNetwork({ daemon_json_proxy: true, http_proxy: "http://daemon.example:1" }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(rt.notApplied));
    assert.ok(screen.getByText(rt.daemonJsonWins));
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.ok(!restartButton(), "restarting cannot help while daemon.json overrides the platform");
    assert.ok(!screen.queryByText(rt.noPrivilege));
  } finally {
    stub.restore();
  }
});

test("sudo is enough to restart and is shown as such", async () => {
  const stub = stubAPI(dockerNetwork({ privilege: "sudo", live_restore: true, http_proxy: "http://old.example:8080" }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(`${rt.liveRestoreOn} · ${rt.systemd} · ${rt.privilegeSudo}`));
    await waitFor(() => assert.ok(restartButton()));
  } finally {
    stub.restore();
  }
});

test("restart confirmation for a node without live-restore warns that containers stop, then restarts once and refreshes the status", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "http://old.example:8080", live_restore: false }));
  let release: () => void = () => {};
  stub.restartImpl = () => new Promise((resolve) => {
    const done = { driver: "docker", proxy: proxyState({ applied: true, http_proxy: PROXY, written_http_proxy: PROXY }) };
    release = () => {
      stub.current.network = networkOf(done);
      resolve(done);
    };
  });
  try {
    const user = userEvent.setup({ document: dom.window.document });
    show();
    await loaded();
    await waitFor(() => assert.ok(restartButton()));
    await user.click(restartButton()!);
    const dialog = await screen.findByRole("dialog");
    assert.ok(within(dialog).getByText(rt.confirmTitle));
    assert.ok(within(dialog).getByText(rt.confirmNoLive));
    assert.ok(!within(dialog).queryByText(rt.confirmLive));
    assert.equal(stub.restarts.length, 0, "opening the dialog does not restart anything");

    await user.click(within(dialog).getByRole("button", { name: rt.confirmOk }));
    await waitFor(() => assert.equal(stub.restarts.length, 1));
    assert.deepEqual(stub.restarts, ["node-1"]);
    // Pending: the confirm button shows a spinner and the dialog cannot be dismissed.
    const cancel = within(dialog).getByRole("button", { name: locale.nodes.action.cancel }) as HTMLButtonElement;
    assert.equal(cancel.disabled, true, "cancel is disabled while the restart runs");
    assert.equal((within(dialog).getByRole("button", { name: rt.confirmOk }) as HTMLButtonElement).disabled, true, "confirm shows its loading state");
    fireEvent.keyDown(dialog, { key: "Escape", keyCode: 27 });
    assert.ok(!dialogClosed(), "Esc does not close a running restart");

    release();
    await waitFor(() => assert.ok(screen.getByText(rt.applied)));
    await waitFor(() => assert.ok(dialogClosed()));
    assert.equal(stub.restarts.length, 1);
    assert.ok(stub.fetches >= 2, "the status was read again after the restart");
    assert.ok(!restartButton());
    const status = screen.getByText(rt.applied).closest("[aria-live]") as HTMLElement;
    await waitFor(() => assert.equal(dom.window.document.activeElement === status, true, "focus moves to the status"));
  } finally {
    stub.restore();
  }
});

test("restart confirmation with live-restore on says containers keep running", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "http://old.example:8080", live_restore: true }));
  try {
    const user = userEvent.setup({ document: dom.window.document });
    show();
    await loaded();
    await waitFor(() => assert.ok(restartButton()));
    await user.click(restartButton()!);
    const dialog = await screen.findByRole("dialog");
    assert.ok(within(dialog).getByText(rt.confirmLive));
    assert.ok(!within(dialog).queryByText(rt.confirmNoLive));
  } finally {
    stub.restore();
  }
});

test("a failed restart shows the product error as readable text and leaves the status unapplied", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "http://old.example:8080" }));
  stub.restartImpl = async () => { throw new APIError("RUNTIME_RESTART_FAILED", { detail: "docker.service failed to start" }, "restart failed", 409); };
  try {
    const user = userEvent.setup({ document: dom.window.document });
    show();
    await loaded();
    await waitFor(() => assert.ok(restartButton()));
    await user.click(restartButton()!);
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: rt.confirmOk }));
    const alert = await screen.findByRole("alert");
    assert.ok((alert.textContent || "").includes(rt.restartFailed));
    assert.ok((alert.textContent || "").includes(fill(locale.common.apiError.RUNTIME_RESTART_FAILED, { detail: "docker.service failed to start" })));
    assert.ok(screen.getByText(rt.notApplied));
    await waitFor(() => assert.ok(dialogClosed()));
    assert.equal(stub.restarts.length, 1);
    assert.ok(restartButton(), "the operator can try again");
  } finally {
    stub.restore();
  }
});

test("Esc and Cancel close the confirmation without calling the API, and Cancel has focus", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "http://old.example:8080" }));
  try {
    const user = userEvent.setup({ document: dom.window.document });
    show();
    await loaded();
    await waitFor(() => assert.ok(restartButton()));
    await user.click(restartButton()!);
    let dialog = await screen.findByRole("dialog");
    const cancel = within(dialog).getByRole("button", { name: locale.nodes.action.cancel });
    await waitFor(() => assert.equal(dom.window.document.activeElement === cancel, true, "the safe choice is focused"));

    fireEvent.keyDown(dialog, { key: "Escape", keyCode: 27 });
    await waitFor(() => assert.ok(dialogClosed()));

    await user.click(restartButton()!);
    dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: locale.nodes.action.cancel }));
    await waitFor(() => assert.ok(dialogClosed()));
    assert.equal(stub.restarts.length, 0, "dismissing never restarts dockerd");
  } finally {
    stub.restore();
  }
});

test("the restart button is keyboard reachable and Enter and Space activate it", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "http://old.example:8080" }));
  try {
    const user = userEvent.setup({ document: dom.window.document });
    show();
    await loaded();
    await waitFor(() => assert.ok(restartButton()));
    restartButton()!.focus();
    assert.equal(dom.window.document.activeElement === restartButton(), true);
    await user.keyboard("{Enter}");
    let dialog = await screen.findByRole("dialog");
    fireEvent.keyDown(dialog, { key: "Escape", keyCode: 27 });
    await waitFor(() => assert.ok(dialogClosed()));
    restartButton()!.focus();
    await user.keyboard(" ");
    dialog = await screen.findByRole("dialog");
    assert.ok(within(dialog).getByText(rt.confirmNoLive));
  } finally {
    stub.restore();
  }
});

const dropIn = (...lines: string[]) => ["[Service]", ...lines].join("\n");

test("without privileges the page shows the drop-in file and the commands, each copyable", async () => {
  const stub = stubAPI(dockerNetwork({ privilege: "none", http_proxy: "http://old.example:8080" }));
  try {
    const user = userEvent.setup({ document: dom.window.document });
    const copied = stubClipboard();
    show();
    await loaded();
    assert.ok(screen.getByText(rt.noPrivilege));
    assert.ok(!restartButton(), "the platform cannot restart without privileges");
    assert.ok(screen.getByText(`${rt.liveRestoreOff} · ${rt.systemd} · ${rt.privilegeNone}`));
    assert.ok(screen.getByText(rt.fileAuthority));
    assert.ok(screen.getByText(DOCKER_PROXY_DROPIN_PATH));

    const expected = dropIn(
      `Environment="HTTP_PROXY=${PROXY}"`,
      `Environment="HTTPS_PROXY=${PROXY}"`,
      'Environment="NO_PROXY=harbor.corp.example,localhost,127.0.0.1,::1"',
    );
    const blocks = Array.from(dom.window.document.querySelectorAll("pre")).map((pre) => pre.textContent);
    assert.deepEqual(blocks, [expected, "sudo systemctl daemon-reload", "sudo systemctl restart docker"]);

    await user.click(screen.getByRole("button", { name: rt.copyFile }));
    await user.click(screen.getByRole("button", { name: fill(rt.copyCommand, { command: "sudo systemctl daemon-reload" }) }));
    await user.click(screen.getByRole("button", { name: fill(rt.copyCommand, { command: "sudo systemctl restart docker" }) }));
    assert.deepEqual(copied, [expected, "sudo systemctl daemon-reload", "sudo systemctl restart docker"]);
  } finally {
    stub.restore();
  }
});

test("the drop-in content skips empty variables, dedupes NO_PROXY and always keeps the loopback entries", () => {
  assert.equal(
    dropInContent({ http_proxy: PROXY, no_proxy: " localhost , 10.0.0.0/8,127.0.0.1,10.0.0.0/8 " }),
    dropIn(`Environment="HTTP_PROXY=${PROXY}"`, 'Environment="NO_PROXY=localhost,10.0.0.0/8,127.0.0.1,::1"'),
  );
  assert.equal(
    dropInContent({ https_proxy: "http://s.example:1" }),
    dropIn('Environment="HTTPS_PROXY=http://s.example:1"', 'Environment="NO_PROXY=localhost,127.0.0.1,::1"'),
  );
});

test("the manual steps follow the node and disappear once dockerd reports the proxy as applied", async () => {
  const stub = stubAPI(dockerNetwork({ privilege: "none", http_proxy: "http://old.example:8080" }));
  try {
    show(30);
    await loaded();
    assert.ok(screen.getByText(rt.noPrivilege));
    await waitFor(() => assert.ok(stub.fetches >= 3));
    stub.current.network = dockerNetwork({ privilege: "none", applied: true, http_proxy: PROXY, written_http_proxy: PROXY });
    await waitFor(() => assert.ok(screen.getByText(rt.applied)));
    assert.ok(!screen.queryByText(rt.noPrivilege));
    assert.equal(dom.window.document.querySelectorAll("pre").length, 0);
    const settled = stub.fetches;
    await new Promise((resolve) => setTimeout(resolve, 120));
    assert.ok(stub.fetches <= settled + 1, "polling stops once nothing is left to wait for");
  } finally {
    stub.restore();
  }
});

test("the refresh button reads the status on demand", async () => {
  const stub = stubAPI(dockerNetwork({ privilege: "none", http_proxy: "http://old.example:8080" }));
  try {
    const user = userEvent.setup({ document: dom.window.document });
    show();
    await loaded();
    const before = stub.fetches;
    stub.current.network = dockerNetwork({ privilege: "none", applied: true, http_proxy: PROXY, written_http_proxy: PROXY });
    await user.click(screen.getByRole("button", { name: rt.refresh }));
    await waitFor(() => assert.ok(screen.getByText(rt.applied)));
    assert.equal(stub.fetches, before + 1);
  } finally {
    stub.restore();
  }
});

test("a dockerd that systemd does not manage is read-only with an explanation", async () => {
  const stub = stubAPI(dockerNetwork({ systemd: false, http_proxy: "http://old.example:8080" }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(rt.noSystemdNote));
    assert.ok(screen.getByText(`${rt.liveRestoreOff} · ${rt.noSystemd} · ${rt.privilegeRoot}`));
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.ok(!restartButton());
    assert.ok(!screen.queryByText(rt.noPrivilege));
  } finally {
    stub.restore();
  }
});

test("a runtime error reported by the agent is shown", async () => {
  const stub = stubAPI(dockerNetwork({ http_proxy: "http://old.example:8080", error: "systemctl show docker: exit status 1" }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText("systemctl show docker: exit status 1"));
  } finally {
    stub.restore();
  }
});

test("nerdctl and udocker nodes apply the proxy per pull and need no restart", async () => {
  for (const driver of ["nerdctl", "udocker"]) {
    const stub = stubAPI(networkOf({ driver }));
    try {
      show();
      await loaded();
      assert.ok(screen.getByText(driverLine(driver)));
      assert.ok(screen.getByText(rt.perPull));
      assert.ok(!restartButton());
    } finally {
      stub.restore();
      cleanup();
    }
  }
});

test("a docker node without proxy data, such as an offline one, says the state is unknown", async () => {
  const stub = stubAPI(networkOf({ driver: "docker" }));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(driverLine("docker")));
    assert.ok(screen.getByText(rt.unknownOffline));
    assert.ok(!screen.queryByText(rt.perPull));
    assert.ok(!restartButton());
  } finally {
    stub.restore();
  }
});

test("a response without runtime information still renders the card", async () => {
  const stub = stubAPI(networkOf(undefined));
  try {
    show();
    await loaded();
    assert.ok(screen.getByText(rt.unknownOffline));
    assert.ok(screen.getByText("harbor.corp.example,localhost"));
    assert.equal(bodyText().includes(rt.restart), false);
  } finally {
    stub.restore();
  }
});
