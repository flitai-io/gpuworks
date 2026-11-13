import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of [
  "window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node",
  "MutationObserver", "ShadowRoot", "getComputedStyle", "matchMedia", "requestAnimationFrame",
] as const) {
  if (dom.window[name] === undefined) continue;
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const common = JSON.parse(readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const nodes = JSON.parse(readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8"));

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  resources: { en: { common, nodes } },
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: AddNodeDrawer } = await import("../src/components/AddNodeDrawer");
const apiModule = await import("../src/api");

const platformKey = {
  public_key: "ssh-ed25519 AAAAC3NzaPlatform gpuworks@server",
  fingerprint: "SHA256:platformfp",
  created_at: "2026-10-03T00:00:00Z",
  authorize_command: "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo 'ssh-ed25519 AAAAC3NzaPlatform gpuworks@server' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys",
};
const keyStub = { restore: () => {} };
beforeEach(() => {
  const api = apiModule.api as Record<string, unknown>;
  const original = api.platformSSHKey;
  const originalNode = api.node;
  api.node = async (uuid: string) => ({ uuid, state: "connecting" });
  api.platformSSHKey = async () => platformKey;
  keyStub.restore = () => { api.platformSSHKey = original; api.node = originalNode; };
});
afterEach(() => {
  zeronMessage.destroy();
  cleanup();
  keyStub.restore();
});

const { FeedbackHost, message: zeronMessage } = await import("../src/ui-kit/feedback");

function renderDrawer(onClose = () => {}) {
  return render(
    <I18nextProvider i18n={i18n}>
      <FeedbackHost /><AddNodeDrawer open onClose={onClose} onInstalled={() => {}} />
    </I18nextProvider>,
  );
}

function stubAPI(name: "installNode" | "precheckNode" | "createNode" | "platformSSHKey" | "node", reply: unknown) {
  const api = apiModule.api as Record<string, unknown>;
  const original = api[name];
  const bodies: Record<string, unknown>[] = [];
  api[name] = async (body: Record<string, unknown>) => {
    bodies.push(body);
    return reply;
  };
  return { bodies, restore: () => { api[name] = original; } };
}

const installReport = { node_uuid: "installed-node", target: "", user: "root", host: "203.0.113.7", port: 51223, binary_name: "gpuworks-agent", install_dir: "/opt/gpuworks", server_url: "http://localhost", checks: [] };

test("connection direction labels name who starts the connection", () => {
  renderDrawer();
  assert.ok(screen.getByRole("radio", { name: "Agent-initiated connection" }));
  assert.ok(screen.getByRole("radio", { name: "Server-initiated connection" }));
});

// The management node address is what the node dials; with the server dialing
// instead, the form asks where the agent listens and, below it, where the
// server reaches it — two facts, so a mapped port is not mistaken for the
// listen port.
test("server-initiated mode asks for the listen address above the compute node address", () => {
  renderDrawer();
  assert.ok(screen.getByText("Management node address"));
  assert.ok(screen.queryByText("Compute node address") === null, "Expected the element to be absent");
  assert.ok(screen.queryByText(nodes.add.agentListenAddress) === null, "Expected the element to be absent");

  fireEvent.click(screen.getByRole("radio", { name: nodes.connection.serverDial }));
  assert.ok(screen.queryByText("Management node address") === null, "Expected the element to be absent");
  const listen = screen.getByRole("textbox", { name: nodes.add.agentListenAddress }) as HTMLInputElement;
  const address = screen.getByRole("textbox", { name: nodes.add.hostAddress }) as HTMLInputElement;
  assert.equal(listen.value, "0.0.0.0:18800");
  assert.equal(address.value, "", "the compute node address starts empty");
  assert.ok(listen.compareDocumentPosition(address) & Node.DOCUMENT_POSITION_FOLLOWING, "the listen address comes first");
  assert.ok(screen.getByText(nodes.add.target), "the SSH target is labelled as such");
});

test("the removed connection notes are gone in both SSH branches", () => {
  renderDrawer();
  for (const branch of [nodes.add.sshSupported, nodes.add.sshUnsupported]) {
    fireEvent.click(screen.getByRole("radio", { name: branch }));
    for (const mode of [nodes.connection.agentDial, nodes.connection.serverDial]) {
      fireEvent.click(screen.getByRole("radio", { name: mode }));
      assert.ok(screen.queryByText(/NAT container/i) === null, `${branch}/${mode}`);
      assert.ok(screen.queryByText(/connects to the target host's listen port/i) === null, `${branch}/${mode}`);
      assert.ok(screen.queryByRole("alert") === null, `${branch}/${mode}`);
    }
  }
});

// The install directory is an advanced setting whether or not the host has SSH.
test("the install directory sits under Advanced in both SSH branches", () => {
  renderDrawer();
  for (const branch of [nodes.add.sshSupported, nodes.add.sshUnsupported]) {
    fireEvent.click(screen.getByRole("radio", { name: branch }));
    assert.ok(screen.queryByPlaceholderText(nodes.add.installDirPlaceholder) === null, `${branch}: collapsed by default`);
    fireEvent.click(screen.getByText(nodes.add.advanced));
    assert.ok(screen.getByPlaceholderText(nodes.add.installDirPlaceholder), `${branch}: shown once expanded`);
    fireEvent.click(screen.getByText(nodes.add.advanced));
  }
});

// The platform key is the default: the form shows the command that
// authorizes it, and the request carries no secret at all.
test("the default platform key needs only the authorize command on the target", async () => {
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    assert.ok(screen.getByRole("radio", { name: nodes.ssh.auth.platformKey, checked: true }));
    await waitFor(() => assert.ok(screen.getByText(platformKey.authorize_command)));
    assert.ok(screen.getByText(nodes.ssh.platformKey.authorizeHelp.replace("{{fingerprint}}", platformKey.fingerprint)));
    const target = "ssh -p 51223 root@203.0.113.7 -L 8080:localhost:8080";
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: target } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.equal(install.bodies.length, 1));
    const body = install.bodies[0];
    assert.equal(body.target, target);
    assert.equal(body.auth_mode, "platform_key");
    assert.equal(body.private_key_pem, undefined);
    assert.equal(body.password, undefined);
    assert.equal(body.server_url, "http://localhost");
  } finally {
    install.restore();
  }
});

test("a platform key the server cannot load is reported, not hidden", async () => {
  (apiModule.api as Record<string, unknown>).platformSSHKey = async () => { throw new Error("PLATFORM_SSH_KEY_MISSING"); };
  renderDrawer();
  await waitFor(() => assert.ok(screen.getByText(/PLATFORM_SSH_KEY_MISSING/)));
});

test("a pasted private key is the one sent", async () => {
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.key }));
    assert.ok(screen.getByText(nodes.ssh.authorizesPlatformKey));
    fireEvent.change(screen.getByPlaceholderText("-----BEGIN OPENSSH PRIVATE KEY-----"), { target: { value: "  KEY  " } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.equal(install.bodies.length, 1));
    assert.equal(install.bodies[0].auth_mode, "private_key");
    assert.equal(install.bodies[0].private_key_pem, "KEY");
  } finally {
    install.restore();
  }
});

test("password and private key modes require what they name", async () => {
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.password }));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.ok(screen.getByText(nodes.ssh.passwordRequired)));
    fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.key }));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.ok(screen.getByText(nodes.ssh.privateKeyRequired)));
    assert.equal(install.bodies.length, 0);
  } finally {
    install.restore();
  }
});

test("keyboard-interactive sends its password to precheck and install without a stale private key", async () => {
  const precheck = stubAPI("precheckNode", { checks: [] });
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.key }));
    fireEvent.change(screen.getByPlaceholderText("-----BEGIN OPENSSH PRIVATE KEY-----"), { target: { value: "STALE KEY" } });
    fireEvent.click(screen.getByRole("radio", { name: "Keyboard-interactive" }));
    fireEvent.change(screen.getByLabelText(nodes.ssh.password), { target: { value: "keyboard-secret" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.startPrecheck }));
    await waitFor(() => assert.equal(precheck.bodies.length, 1));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.equal(install.bodies.length, 1));
    for (const body of [precheck.bodies[0], install.bodies[0]]) {
      assert.equal(body.auth_mode, "keyboard_interactive");
      assert.equal(body.password, "keyboard-secret");
      assert.equal(body.private_key_pem, undefined);
      assert.equal(body.target, "root@203.0.113.7");
    }
  } finally {
    precheck.restore();
    install.restore();
  }
});

test("keyboard-interactive requires a password before precheck or install", async () => {
  const precheck = stubAPI("precheckNode", { checks: [] });
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("radio", { name: "Keyboard-interactive" }));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.startPrecheck }));
    await waitFor(() => assert.ok(screen.getByText(nodes.ssh.passwordRequired)));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.ok(screen.getByText(nodes.ssh.passwordRequired)));
    assert.equal(precheck.bodies.length, 0);
    assert.equal(install.bodies.length, 0);
  } finally {
    precheck.restore();
    install.restore();
  }
});

// server_dial never shows the management node address, so a value typed in
// agent mode first must not leak into the request.
// The server dials this agent, which never contacts the server: the install
// carries no server address, and the listen address the form shows.
test("server-initiated installs send no server address", async () => {
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.change(screen.getByDisplayValue(window.location.origin), { target: { value: "https://other.example" } });
    fireEvent.click(screen.getByRole("radio", { name: nodes.connection.serverDial }));
    fireEvent.change(screen.getByPlaceholderText(nodes.add.hostAddressPlaceholder), { target: { value: "203.0.113.7" } });
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.password }));
    fireEvent.change(screen.getByLabelText(nodes.ssh.password), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.equal(install.bodies.length, 1));
    const body = install.bodies[0];
    assert.equal(body.server_url, undefined);
    assert.equal(body.connect_mode, "server_dial");
    assert.equal(body.host_address, "203.0.113.7");
    assert.equal(body.server_dial_listen, "0.0.0.0:18800");
    assert.equal(body.auth_mode, "password");
    assert.equal(body.password, "pw");
  } finally {
    install.restore();
  }
});

// Plain-HTTP consoles have no navigator.clipboard; the install command is the
// first thing a new user copies, so it must go through the selection fallback.
function plainHTTPClipboard(execResult: boolean) {
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (command: string) => {
      if (command === "copy") copied.push((document.activeElement as HTMLTextAreaElement).value);
      return execResult;
    },
  });
  return {
    copied,
    restore: () => {
      delete (navigator as { clipboard?: unknown }).clipboard;
      delete (document as { execCommand?: unknown }).execCommand;
    },
  };
}

async function generateManualCommand() {
  fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshUnsupported }));
  fireEvent.click(screen.getByRole("button", { name: nodes.add.createCommand }));
  await screen.findByText(nodes.add.tokenSaved);
  return document.querySelector("pre")?.textContent ?? "";
}

test("server-initiated installs refuse an empty compute node address before calling the server", async () => {
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.click(screen.getByRole("radio", { name: nodes.connection.serverDial }));
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await screen.findByText(nodes.add.message.hostAddressRequired);
    assert.equal(install.bodies.length, 0);
  } finally {
    install.restore();
  }
});

// Without SSH a node the server dials installs from the offline package: the
// form offers it for download and shows the server's command to run it.
test("a server-initiated node without SSH gets the offline package and its command", async () => {
  const command = "sh gpuworks-agent.run --token 'tok-1' --connect-mode server_dial --listen '0.0.0.0:6006'";
  const create = stubAPI("createNode", { node: { uuid: "offline-node" }, install_command: command, token: "tok-1" });
  const originalFetch = globalThis.fetch;
  const fetched: string[] = [];
  globalThis.fetch = (async (url: string) => {
    fetched.push(url);
    return new Response("3f2a  gpuworks-agent.run\n", { status: 200 });
  }) as typeof fetch;
  const original = apiModule.api.node;
  apiModule.api.node = () => new Promise(() => {});
  try {
    renderDrawer();
    fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshUnsupported }));
    fireEvent.click(screen.getByRole("radio", { name: nodes.connection.serverDial }));
    fireEvent.change(screen.getByRole("textbox", { name: nodes.add.agentListenAddress }), { target: { value: "0.0.0.0:6006" } });
    fireEvent.change(screen.getByRole("textbox", { name: nodes.add.hostAddress }), { target: { value: "connect.westd.example:37145" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.createCommand }));
    await screen.findByText(nodes.add.tokenSaved);
    assert.equal(document.querySelector("pre")?.textContent, command);
    const body = create.bodies[0];
    assert.equal(body.server_url, undefined);
    assert.equal(body.server_dial_listen, "0.0.0.0:6006");
    assert.equal(body.host_address, "connect.westd.example:37145");
    const link = screen.getByRole("link", { name: nodes.add.offlineDownload }) as HTMLAnchorElement;
    assert.equal(link.getAttribute("href"), "/dl/gpuworks-agent.run");
    await screen.findByText("sha256: 3f2a");
    assert.deepEqual(fetched, ["/dl/gpuworks-agent.run.sha256"]);
    assert.ok(screen.getByText(nodes.add.offlineInstructions));
  } finally {
    apiModule.api.node = original;
    globalThis.fetch = originalFetch;
    create.restore();
  }
});

test("the install command copies on a plain-HTTP console", async () => {
  const create = stubAPI("createNode", { node: { uuid: "manual-node" }, install_command: "curl -fsSL http://localhost/install.sh | sh -s --", token: "tok-123" });
  const clipboard = plainHTTPClipboard(true);
  try {
    renderDrawer();
    const command = await generateManualCommand();
    assert.match(command, /install\.sh/);
    const button = screen.getByRole("button", { name: new RegExp(nodes.add.copyCommand) });
    button.focus();
    fireEvent.click(button);
    await screen.findByText(nodes.add.message.copied);
    assert.deepEqual(clipboard.copied, [command]);
    // assert.ok, not assert.equal: a failing equal on jsdom nodes inspects the
    // whole DOM graph and stalls the runner instead of reporting.
    assert.ok(document.activeElement === button, `focus returns to the copy button, got ${document.activeElement?.tagName}`);
    assert.ok(document.querySelector("textarea[readonly]") === null, "the hidden textarea is removed");
  } finally {
    clipboard.restore();
    create.restore();
  }
});

test("a refused copy offers the install command for manual selection", async () => {
  const create = stubAPI("createNode", { node: { uuid: "manual-node" }, install_command: "curl -fsSL http://localhost/install.sh | sh -s --", token: "tok-456" });
  const clipboard = plainHTTPClipboard(false);
  try {
    renderDrawer();
    const command = await generateManualCommand();
    // antd toasts outlive cleanup in jsdom, so compare counts across the click.
    const toastsBefore = screen.queryAllByText(nodes.add.message.copied).length;
    fireEvent.click(screen.getByRole("button", { name: new RegExp(nodes.add.copyCommand) }));
    const dialog = await screen.findByRole("dialog", { name: common.copy.manualTitle });
    assert.ok(dialog.textContent?.includes(common.copy.manualHint));
    const editor = await waitFor(() => {
      const found = dialog.querySelector("textarea");
      assert.ok(found);
      return found as HTMLTextAreaElement;
    });
    assert.equal(editor.value, command);
    assert.equal(screen.queryAllByText(nodes.add.message.copied).length, toastsBefore, "no success toast for a refused copy");
  } finally {
    clipboard.restore();
    create.restore();
  }
});

// The precheck inspects the filesystem behind the directory the install will
// use, so the wizard's install directory travels with the precheck request and
// each directory verdict is its own row.
test("the precheck sends the install directory and lists each directory verdict", async () => {
  const detail = "runveil workloads will run as the agent's own user (root) instead of the dedicated runtime user: chown: changing ownership of '/workspace/gpuworks/.gpuworks-dircheck.41': Operation not permitted";
  const precheck = stubAPI("precheckNode", {
    target: "root@203.0.113.7:51223", user: "root", host: "203.0.113.7", port: 51223,
    checks: [
      { name: "Install directory", status: "error", detail: "/workspace is mounted noexec" },
      { name: "Install directory ownership", status: "warn", detail },
      { name: "Install directory persistence", status: "ok", detail: "not on a container's writable layer" },
    ],
  });
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7:51223" } });
    fireEvent.click(screen.getByText(nodes.add.advanced));
    fireEvent.change(screen.getByPlaceholderText(nodes.add.installDirPlaceholder), { target: { value: "/workspace/gpuworks" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.startPrecheck }));
    await waitFor(() => assert.equal(precheck.bodies.length, 1));
    assert.equal(precheck.bodies[0].install_dir, "/workspace/gpuworks");
    assert.equal(precheck.bodies[0].target, "root@203.0.113.7:51223");

    await screen.findByText("Install directory ownership");
    const rows = screen.getAllByRole("row").map((row) => row.textContent ?? "");
    const rowFor = (name: string) => rows.find((text) => [nodes.add.check.ok, nodes.add.check.warn, nodes.add.check.error].some((status) => text.startsWith(name + status))) ?? "";
    assert.ok(rowFor("Install directory").includes(nodes.add.check.error + "/workspace is mounted noexec"), rows.join(" | "));
    assert.ok(rowFor("Install directory ownership").includes(nodes.add.check.warn + detail), rows.join(" | "));
    assert.ok(rowFor("Install directory persistence").includes(nodes.add.check.ok), rows.join(" | "));
    // A failed check is not "installation can still continue": a noexec
    // directory stops the install.
    await screen.findByText(nodes.add.message.precheckWithErrors);
    assert.ok(screen.queryByText(nodes.add.message.precheckWithIssues) === null, "Expected the element to be absent");
  } finally {
    precheck.restore();
  }
});

test("a failed precheck echoes a parsed target and hides an empty one", async () => {
  const refused = stubAPI("precheckNode", {
    target: "root@203.0.113.7", user: "", host: "", port: 0,
    checks: [{ name: "SSH", status: "error", detail: "connection refused" }],
  });
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.startPrecheck }));
    await screen.findByText("connection refused");
    assert.ok(screen.queryByText("@:0") === null, "Expected the element to be absent");
  } finally {
    refused.restore();
    cleanup();
  }

  const parsed = stubAPI("precheckNode", {
    target: "root@203.0.113.7", user: "root", host: "203.0.113.7", port: 22,
    checks: [{ name: "SSH", status: "error", detail: "connection refused" }],
  });
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.startPrecheck }));
    await screen.findByText("root@203.0.113.7:22");
    assert.ok(screen.queryByText("@:0") === null, "Expected the element to be absent");
  } finally {
    parsed.restore();
  }
});

test("a blank install directory is left out of the precheck request", async () => {
  const precheck = stubAPI("precheckNode", { target: "root@203.0.113.7", user: "root", host: "203.0.113.7", port: 22, checks: [] });
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.startPrecheck }));
    await waitFor(() => assert.equal(precheck.bodies.length, 1));
    assert.equal(precheck.bodies[0].install_dir, undefined);
  } finally {
    precheck.restore();
  }
});

// The install directory says what it holds in a line, and why it must be a
// local filesystem in a tooltip, because the runveil runtime keeps images and
// container root filesystems there.
test("the install directory explains what it holds and where it may live", async () => {
  renderDrawer();
  fireEvent.click(screen.getByText(nodes.add.advanced));
  assert.ok(screen.getByText(nodes.add.installDirHelp));
  const label = screen.getByText(nodes.add.installDir).closest("label");
  const icon = label?.querySelector(".anticon-question-circle");
  assert.ok(icon, "the install directory label has no tooltip icon");
  fireEvent.mouseEnter(icon!);
  await screen.findByText(nodes.add.installDirTooltip);
});

test("installing onto a host that already runs an agent asks before replacing it", async () => {
  const { APIError } = await import("../src/apiError");
  const api = apiModule.api as Record<string, unknown>;
  const original = api.installNode;
  const bodies: Record<string, unknown>[] = [];
  api.installNode = async (body: Record<string, unknown>) => {
    bodies.push(body);
    if (!body.replace_existing_agent) {
      throw new APIError("NODE_INSTALL_EXISTING_AGENT", { server: "http://192.0.2.156:8800" }, "a gpuworks agent for another server is on the host", 409);
    }
    return installReport;
  };
  try {
    renderDrawer();
    await waitFor(() => assert.ok(screen.getByText(platformKey.authorize_command)));
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@192.0.2.210" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    // antd's confirm renders its title twice (heading and dialog label).
    await waitFor(() => assert.ok(screen.getAllByText(nodes.add.replaceAgent.title).length > 0));
    const reason = common.apiError.NODE_INSTALL_EXISTING_AGENT.replace("{{server}}", "http://192.0.2.156:8800");
    assert.ok(screen.getAllByText(`${reason} ${nodes.add.replaceAgent.content}`).length > 0);
    assert.equal(bodies.length, 1);
    assert.equal(bodies[0].replace_existing_agent, undefined);
    fireEvent.click(screen.getByRole("button", { name: nodes.add.replaceAgent.confirm }));
    await waitFor(() => assert.equal(bodies.length, 2));
    assert.equal(bodies[1].replace_existing_agent, true);
    assert.equal(bodies[1].target, "root@192.0.2.210");
  } finally {
    api.installNode = original;
  }
});

for (const manual of [false, true]) {
  test(`${manual ? "manual" : "SSH"} onboarding closes only after its own node is online`, async () => {
    const create = stubAPI("createNode", { node: { uuid: "new-node" }, install_command: "install", token: "token" });
    const install = stubAPI("installNode", { ...installReport, node_uuid: "new-node" });
    const api = apiModule.api;
    const original = api.node;
    let finish: ((value: never) => void) | undefined;
    const requests: string[] = [];
    api.node = (uuid) => { requests.push(uuid); return new Promise((resolve) => { finish = resolve; }); };
    let closes = 0;
    try {
      renderDrawer(() => { closes++; });
      if (manual) await generateManualCommand();
      else {
        fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
        fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
      }
      await waitFor(() => assert.deepEqual(requests, ["new-node"]));
      assert.equal(closes, 0, "installation response does not mean connected");
      await act(async () => { finish!({ uuid: "new-node", state: "online" } as never); });
      await waitFor(() => assert.equal(closes, 1));
    } finally { api.node = original; create.restore(); install.restore(); }
  });
}

for (const state of ["connecting", "offline", "other-node", "error"] as const) {
  test(`onboarding keeps its waiting state for ${state} and retries until its own node connects`, async () => {
    const create = stubAPI("createNode", { node: { uuid: "new-node" }, install_command: "install", token: "token" });
    const api = apiModule.api;
    const original = api.node;
    let ready = false;
    let closes = 0;
    const requests: string[] = [];
    api.node = async (uuid) => {
      requests.push(uuid);
      if (ready) return { uuid, state: "online" } as never;
      if (state === "error") throw new Error("temporary failure");
      return { uuid: state === "other-node" ? "old-node" : uuid, state: state === "other-node" ? "online" : state } as never;
    };
    try {
      renderDrawer(() => { closes++; });
      await generateManualCommand();
      await screen.findByText(nodes.add.waitingForConnection);
      assert.equal(closes, 0);
      assert.deepEqual(requests, ["new-node"]);
      ready = true;
      await waitFor(() => assert.equal(closes, 1), { timeout: 5000 });
      assert.ok(requests.length >= 2);
      assert.ok(requests.every((uuid) => uuid === "new-node"));
    } finally { api.node = original; create.restore(); }
  });
}

for (const pending of ["installation", "connection"] as const) {
  test(`closing and reopening discards a late ${pending} response`, async () => {
    const api = apiModule.api;
    const installOriginal = api.installNode;
    const nodeOriginal = api.node;
    let finish: (value: never) => void = () => {};
    let closes = 0;
    let installStarted = false;
    let installRequests = 0;
    api.installNode = async () => { installRequests++; installStarted = true; return pending === "installation"
      ? await new Promise((resolve) => { finish = resolve; })
      : { ...installReport, node_uuid: "new-node" } as never; };
    api.node = () => new Promise((resolve) => { finish = resolve; });
    function Wizard() {
      const [open, setOpen] = React.useState(true);
      return <><button onClick={() => setOpen(true)}>Reopen wizard</button><FeedbackHost /><AddNodeDrawer open={open} onClose={() => { closes++; setOpen(false); }} onInstalled={() => {}} /></>;
    }
    try {
      render(<I18nextProvider i18n={i18n}><Wizard /></I18nextProvider>);
      fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
      fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
      if (pending === "connection") await screen.findByText(nodes.add.waitingForConnection);
      else await waitFor(() => assert.ok(installStarted));
      fireEvent.click(screen.getByRole("button", { name: "Close" }));
      await waitFor(() => assert.ok(!screen.queryByPlaceholderText("root@192.0.2.10:22")));
      fireEvent.click(screen.getByRole("button", { name: "Reopen wizard" }));
      if (pending === "installation") {
        fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
        await act(async () => { fireEvent.click(screen.getByRole("button", { name: new RegExp(nodes.add.installAgent) })); });
        assert.equal(installRequests, 1, "reopening cannot overlap the unresolved installation");
        fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "" } });
      }
      await act(async () => { finish(pending === "connection" ? { uuid: "new-node", state: "online" } as never : { ...installReport, node_uuid: "new-node" } as never); });
      assert.equal(closes, 1, "the late response must not close the new wizard");
      assert.ok(screen.queryByText(nodes.add.waitingForConnection) === null, "Expected the element to be absent");
      assert.equal((screen.getByPlaceholderText("root@192.0.2.10:22") as HTMLInputElement).value, "");
    } finally { api.installNode = installOriginal; api.node = nodeOriginal; }
  });
}

test("nonfatal runtime capability errors still observe the installed node", async () => {
  // installDirNamesCheck emits an error without Docker but does not stop installation.
  const detail = "/workspace does not allow ':' in file names, which container images need (image layers are named sha256:<hex>, and Debian-based images hold /var/lib/dpkg/info/<package>:amd64.list), so the runveil runtime used on nodes without docker cannot run deployments from here; choose an install directory on a local filesystem (on a container instance such as RunPod, a path on the container disk sized for the images)";
  const install = stubAPI("installNode", { ...installReport, checks: [
    { name: "Install directory file names", status: "error", detail },
    { name: "Start agent", status: "ok", detail: "agent started" },
  ] });
  const original = apiModule.api.node;
  let finish: (value: never) => void = () => {};
  const requests: string[] = [];
  apiModule.api.node = (uuid) => { requests.push(uuid); return new Promise((resolve) => { finish = resolve; }); };
  let closes = 0;
  try {
    renderDrawer(() => { closes++; });
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await screen.findByText(detail);
    await screen.findByText(nodes.add.waitingForConnection);
    assert.deepEqual(requests, [installReport.node_uuid]);
    assert.equal(closes, 0);
    assert.equal((screen.getByRole("button", { name: new RegExp(nodes.add.installAgent) }) as HTMLButtonElement).disabled, true);
    await act(async () => { finish({ uuid: installReport.node_uuid, state: "online" } as never); });
    await waitFor(() => assert.equal(closes, 1));
  } finally { install.restore(); apiModule.api.node = original; }
});

test("an HTTP 400 startup failure permits retry without observing a node", async () => {
  const original = globalThis.fetch;
  let attempts = 0;
  globalThis.fetch = async () => { attempts++; return new Response(JSON.stringify({ error: "start agent: exit status 1" }), { status: 400 }); };
  const node = stubAPI("node", { uuid: "new-node", state: "online" });
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await screen.findByText(nodes.add.message.installFailed.replace("{{detail}}", "start agent: exit status 1"));
    assert.equal(node.bodies.length, 0);
    assert.ok(screen.queryByText(nodes.add.waitingForConnection) === null, "Expected the element to be absent");
    fireEvent.click(screen.getByRole("button", { name: new RegExp(nodes.add.installAgent) }));
    await waitFor(() => assert.equal(attempts, 2));
  } finally { globalThis.fetch = original; node.restore(); }
});

test("switching onboarding mode discards the previous node's late registration", async () => {
  const create = stubAPI("createNode", { node: { uuid: "new-node" }, install_command: "install", token: "token" });
  const original = apiModule.api.node;
  let finish: (value: never) => void = () => {};
  let closes = 0;
  apiModule.api.node = () => new Promise((resolve) => { finish = resolve; });
  try {
    renderDrawer(() => { closes++; });
    await generateManualCommand();
    await screen.findByText(nodes.add.waitingForConnection);
    fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshSupported }));
    await act(async () => { finish({ uuid: "new-node", state: "online" } as never); });
    assert.equal(closes, 0);
    assert.ok(screen.queryByText(nodes.add.waitingForConnection) === null, "Expected the element to be absent");
    assert.equal((screen.getByRole("button", { name: nodes.add.installAgent }) as HTMLButtonElement).disabled, false);
  } finally { apiModule.api.node = original; create.restore(); }
});

for (const change of ["name", "credentials", "mode", "direction", "systemd"] as const) {
  test(`editing ${change} cannot overlap a pending SSH installation`, async () => {
    const original = apiModule.api.installNode;
    const requests: unknown[] = [];
    let finish: (value: never) => void = () => {};
    apiModule.api.installNode = (body) => { requests.push(body); return new Promise((resolve) => { finish = resolve; }); };
    try {
      renderDrawer();
      fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
      fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
      await waitFor(() => assert.equal(requests.length, 1));
      if (change === "name") fireEvent.change(screen.getByPlaceholderText(nodes.add.nodeNamePlaceholder), { target: { value: "edited" } });
      else if (change === "credentials") {
        fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.password }));
        fireEvent.change(screen.getByLabelText(nodes.ssh.password), { target: { value: "pw" } });
      } else if (change === "systemd") {
        fireEvent.click(screen.getByText(nodes.add.advanced));
        fireEvent.click(screen.getByRole("switch", { name: nodes.add.useSystemd }));
      } else if (change === "direction") {
        fireEvent.click(screen.getByRole("radio", { name: nodes.connection.serverDial }));
        fireEvent.click(screen.getByRole("radio", { name: nodes.connection.agentDial }));
      } else {
        fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshUnsupported }));
        fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshSupported }));
      }
      const install = screen.getByRole("button", { name: new RegExp(nodes.add.installAgent) });
      await act(async () => { fireEvent.click(install); });
      assert.equal(requests.length, 1, "editing does not cancel the remote installation");
      await act(async () => { finish(installReport as never); });
      assert.ok(screen.queryByText(nodes.add.waitingForConnection) === null, "stale response does not start observing a changed form");
      fireEvent.click(install);
      await waitFor(() => assert.equal(requests.length, 2));
      await act(async () => { finish(installReport as never); });
    } finally { apiModule.api.installNode = original; }
  });
}

// systemd is on unless switched off under Advanced; switching it off reaches
// the SSH install as no_systemd and the manual command as --no-systemd.
test("starting via systemd is on by default and can be switched off", async () => {
  const install = stubAPI("installNode", installReport);
  try {
    renderDrawer();
    fireEvent.click(screen.getByText(nodes.add.advanced));
    const toggle = screen.getByRole("switch", { name: nodes.add.useSystemd });
    assert.equal(toggle.getAttribute("aria-checked"), "true");
    assert.ok(screen.queryByText(nodes.add.useSystemdOffHelp) === null, "Expected the element to be absent");
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@203.0.113.7" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.equal(install.bodies.length, 1));
    assert.equal(install.bodies[0].no_systemd, undefined);

    // The switch works from the keyboard: the left arrow turns it off.
    fireEvent.keyDown(toggle, { key: "ArrowLeft", keyCode: 37 });
    assert.equal(toggle.getAttribute("aria-checked"), "false");
    assert.ok(screen.getByText(nodes.add.useSystemdOffHelp));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    await waitFor(() => assert.equal(install.bodies.length, 2));
    assert.equal(install.bodies[1].no_systemd, true);
  } finally {
    install.restore();
  }
});

test("the manual install command carries --no-systemd only when systemd is off", async () => {
  const create = stubAPI("createNode", { node: { uuid: "manual-node" }, install_command: "curl -fsSL http://localhost/install.sh | sh -s --", token: "tok-789" });
  const original = apiModule.api.node;
  apiModule.api.node = () => new Promise(() => {});
  try {
    renderDrawer();
    fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshUnsupported }));
    fireEvent.click(screen.getByText(nodes.add.advanced));
    fireEvent.click(screen.getByRole("switch", { name: nodes.add.useSystemd }));
    fireEvent.click(screen.getByRole("button", { name: nodes.add.createCommand }));
    await screen.findByText(nodes.add.tokenSaved);
    assert.match(document.querySelector("pre")?.textContent ?? "", / --no-systemd$/);

    // Switching back discards the command minted for the other choice.
    fireEvent.click(screen.getByRole("switch", { name: nodes.add.useSystemd }));
    assert.ok(screen.queryByText(nodes.add.tokenSaved) === null, "Expected the element to be absent");
    fireEvent.click(screen.getByRole("button", { name: nodes.add.createCommand }));
    await screen.findByText(nodes.add.tokenSaved);
    assert.doesNotMatch(document.querySelector("pre")?.textContent ?? "", /--no-systemd/);
  } finally {
    apiModule.api.node = original;
    create.restore();
  }
});

// Without systemd only the agent in the same directory is replaced, which the
// error says; the confirm does not speak of replacing the service.
test("a nohup install into an agent's directory asks without mentioning the service", async () => {
  const { APIError } = await import("../src/apiError");
  const api = apiModule.api as Record<string, unknown>;
  const original = api.installNode;
  const bodies: Record<string, unknown>[] = [];
  api.installNode = async (body: Record<string, unknown>) => {
    bodies.push(body);
    if (!body.replace_existing_agent) {
      throw new APIError("NODE_INSTALL_EXISTING_AGENT_SAME_DIR", { dir: "/opt/gpuworks" }, "a gpuworks agent already runs from /opt/gpuworks; installing there replaces it", 409);
    }
    return installReport;
  };
  try {
    renderDrawer();
    fireEvent.click(screen.getByText(nodes.add.advanced));
    fireEvent.click(screen.getByRole("switch", { name: nodes.add.useSystemd }));
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@192.0.2.210" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    const reason = common.apiError.NODE_INSTALL_EXISTING_AGENT_SAME_DIR.replace("{{dir}}", "/opt/gpuworks");
    await waitFor(() => assert.ok(screen.getAllByText(reason).length > 0));
    // An earlier test's confirm may still be leaving; read the newest one.
    const dialogs = document.querySelectorAll(".ant-modal-confirm");
    const confirm = dialogs[dialogs.length - 1]?.textContent ?? "";
    assert.ok(confirm.includes(reason), `newest confirm: ${confirm}`);
    assert.ok(!confirm.includes(nodes.add.replaceAgent.content), `the confirm speaks of the service: ${confirm}`);
    fireEvent.click(screen.getByRole("button", { name: nodes.add.replaceAgent.confirm }));
    await waitFor(() => assert.equal(bodies.length, 2));
    assert.equal(bodies[1].replace_existing_agent, true);
    assert.equal(bodies[1].no_systemd, true);
  } finally {
    api.installNode = original;
  }
});

// A closed ancestor of the install directory stops the install: the toast
// gives the reason in the reader's language, with the repair command.
test("an install stopped by a closed ancestor of the install directory says why and how to fix it", async () => {
  const { APIError } = await import("../src/apiError");
  const api = apiModule.api as Record<string, unknown>;
  const original = api.installNode;
  api.installNode = async () => {
    throw new APIError("NODE_INSTALL_DIR_UNTRAVERSABLE", { dir: "/root", mode: "700", install_dir: "/root/gpuworks", fix: "chmod o+x /root" },
      "the dedicated runtime user cannot reach the install directory: Install directory path access: /root (mode 700, owner 0:0) cannot be searched", 400);
  };
  try {
    renderDrawer();
    fireEvent.change(screen.getByPlaceholderText("root@192.0.2.10:22"), { target: { value: "root@192.0.2.210" } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.installAgent }));
    const reason = common.apiError.NODE_INSTALL_DIR_UNTRAVERSABLE.replace("{{dir}}", "/root").replace("{{mode}}", "700").replace("{{fix}}", "chmod o+x /root");
    await screen.findByText(nodes.add.message.installFailed.replace("{{detail}}", reason));
  } finally {
    api.installNode = original;
  }
});
