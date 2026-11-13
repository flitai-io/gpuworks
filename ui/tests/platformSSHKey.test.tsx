import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of [
  "window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "SVGElement", "Element", "Node",
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

const load = (ns: string) => JSON.parse(readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const common = load("common");
const nodes = load("nodes");
const settings = load("settings");
const providers = load("providers");

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  resources: { en: { common, nodes, settings, providers } },
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { NodeSSHAccess } = await import("../src/components/NodeRecoveryControls");
const { default: PlatformSSHKeyCard } = await import("../src/pages/settings/PlatformSSHKeyCard");
const { default: ProviderInstallModal } = await import("../src/components/ProviderInstallModal");
const apiModule = await import("../src/api");
const api = apiModule.api as Record<string, unknown>;

const key = {
  public_key: "ssh-ed25519 AAAAC3NzaPlatform gpuworks@server",
  fingerprint: "SHA256:platformfp",
  created_at: "2026-10-03T00:00:00Z",
  authorize_command: "mkdir -p ~/.ssh && echo 'ssh-ed25519 AAAAC3NzaPlatform gpuworks@server' >> ~/.ssh/authorized_keys",
};

// stub replaces api methods for one test and records each call's arguments.
const restores: Array<() => void> = [];
function stub(name: string, impl: (...args: any[]) => unknown) {
  const original = api[name];
  const calls: any[][] = [];
  api[name] = async (...args: any[]) => {
    calls.push(args);
    return impl(...args);
  };
  restores.push(() => { api[name] = original; });
  return calls;
}
afterEach(() => {
  cleanup();
  while (restores.length) restores.pop()!();
});

function wrap(node: React.ReactElement) {
  return render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);
}

const sshNode = { uuid: "n1", hostname: "gpu-1", has_ssh_credential: true } as never;

test("the SSH channel editor shows the stored mode and saves the platform key without secrets", async () => {
  stub("platformSSHKey", () => key);
  stub("nodeSSH", () => ({ host: "gpu.example", port: 22, user: "root", has_password: false, has_private_key: false, auth_mode: "platform_key" }));
  const puts = stub("putNodeSSH", () => ({ host: "gpu.example", port: 22, user: "root", has_password: false, has_private_key: false, auth_mode: "platform_key" }));
  stub("node", () => sshNode);
  wrap(<NodeSSHAccess node={sshNode} onUpdated={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.edit }));
  await waitFor(() => assert.ok(screen.getByText(key.authorize_command)));
  assert.ok(screen.getByRole("radio", { name: nodes.ssh.auth.platformKey, checked: true }));
  assert.ok(screen.getByText(nodes.ssh.existingAuth.replace("{{method}}", nodes.ssh.auth.platformKey)));
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.save }));
  await waitFor(() => assert.equal(puts.length, 1));
  const [uuid, body] = puts[0];
  assert.equal(uuid, "n1");
  assert.equal(body.auth_mode, "platform_key");
  assert.equal(body.password, undefined);
  assert.equal(body.private_key_pem, undefined);
});

test("saving a password reports that the platform key replaced it", async () => {
  stub("platformSSHKey", () => key);
  stub("nodeSSH", () => { throw new Error("404"); });
  const puts = stub("putNodeSSH", () => ({
    host: "gpu-1", port: 22, user: "root", has_password: false, has_private_key: false, auth_mode: "platform_key",
    platform_key: { name: "Platform key", status: "ok", detail: "added to /root/.ssh/authorized_keys" },
  }));
  stub("node", () => sshNode);
  wrap(<NodeSSHAccess node={sshNode} onUpdated={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.edit }));
  await waitFor(() => assert.ok(screen.getByRole("radio", { name: nodes.ssh.auth.password })));
  fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.password }));
  fireEvent.change(screen.getByLabelText(nodes.ssh.password), { target: { value: "pw" } });
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.save }));
  await waitFor(() => assert.equal(puts.length, 1));
  assert.equal(puts[0][1].auth_mode, "password");
  assert.equal(puts[0][1].password, "pw");
  await waitFor(() => assert.ok(screen.getByText(nodes.ssh.message.switchedToPlatformKey)));
});

test("the SSH editor selects keyboard-interactive for a saved password and sends the newly entered password", async () => {
  stub("platformSSHKey", () => key);
  stub("nodeSSH", () => ({ host: "gpu.example", port: 22, user: "root", has_password: true, has_private_key: false, auth_mode: "password" }));
  const puts = stub("putNodeSSH", () => ({
    host: "gpu.example", port: 22, user: "root", has_password: false, has_private_key: false, auth_mode: "platform_key",
    platform_key: { name: "Platform key", status: "ok", detail: "added to /root/.ssh/authorized_keys" },
  }));
  stub("node", () => sshNode);
  wrap(<NodeSSHAccess node={sshNode} onUpdated={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.edit }));
  await waitFor(() => assert.ok(screen.getByRole("button", { name: nodes.ssh.save })));
  assert.ok(screen.getByRole("radio", { name: nodes.ssh.auth.password, checked: true }));
  assert.ok(screen.getByText(nodes.ssh.existingAuth.replace("{{method}}", nodes.ssh.auth.password)));
  assert.ok(screen.getByText(nodes.ssh.savedPasswordHelp));
  fireEvent.click(screen.getByRole("radio", { name: nodes.ssh.auth.key }));
  fireEvent.change(screen.getByPlaceholderText("-----BEGIN OPENSSH PRIVATE KEY-----"), { target: { value: "EDITOR STALE KEY" } });
  fireEvent.click(screen.getByRole("radio", { name: "Keyboard-interactive" }));
  const password = screen.getByLabelText(nodes.ssh.password) as HTMLInputElement;
  assert.equal(password.value, "", "stored passwords must never be returned to the editor");
  fireEvent.change(password, { target: { value: "new-keyboard-secret" } });
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.save }));
  await waitFor(() => assert.equal(puts.length, 1));
  assert.equal(puts[0][0], "n1");
  assert.equal(puts[0][1].auth_mode, "keyboard_interactive");
  assert.equal(puts[0][1].password, "new-keyboard-secret");
  assert.equal(puts[0][1].private_key_pem, undefined);
});

test("saving reports a gpuworks-agent service that belongs to another server", async () => {
  stub("platformSSHKey", () => key);
  stub("nodeSSH", () => ({ host: "gpu.example", port: 22, user: "root", has_password: false, has_private_key: false, auth_mode: "platform_key" }));
  const puts = stub("putNodeSSH", () => ({
    host: "gpu.example", port: 22, user: "root", has_password: false, has_private_key: false, auth_mode: "platform_key",
    probe: {
      reachable: true, agent_running: false, agent_installed: true,
      foreign_server_url: "http://192.0.2.156:8800", detail: "foreign-systemd http://192.0.2.156:8800",
    },
  }));
  stub("node", () => sshNode);
  wrap(<NodeSSHAccess node={sshNode} onUpdated={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.edit }));
  await waitFor(() => assert.ok(screen.getByText(key.authorize_command)));
  // antd toasts from earlier tests stay mounted, so compare counts.
  const savedBefore = screen.queryAllByText(nodes.ssh.message.saved).length;
  fireEvent.click(screen.getByRole("button", { name: nodes.ssh.save }));
  await waitFor(() => assert.equal(puts.length, 1));
  const warning = nodes.ssh.message.savedProbeForeignAgent.replace("{{server}}", "http://192.0.2.156:8800");
  await waitFor(() => assert.ok(screen.getByText(warning)));
  assert.equal(screen.queryAllByText(nodes.ssh.message.saved).length, savedBefore);
});

test("the settings card shows the key, imports one and regenerates after confirming", async () => {
  stub("platformSSHKey", () => key);
  const imports = stub("importPlatformSSHKey", () => ({ ...key, fingerprint: "SHA256:imported" }));
  const rotations = stub("rotatePlatformSSHKey", () => ({ ...key, fingerprint: "SHA256:rotated" }));
  wrap(<PlatformSSHKeyCard />);
  await waitFor(() => assert.ok(screen.getByText(key.public_key)));
  assert.ok(screen.getByText(key.fingerprint));
  assert.ok(screen.getByText(key.authorize_command));

  fireEvent.click(screen.getByRole("button", { name: settings.sshKey.import }));
  const textarea = await waitFor(() => screen.getByLabelText(settings.sshKey.privateKey));
  fireEvent.change(textarea, { target: { value: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----" } });
  const importButtons = screen.getAllByRole("button", { name: settings.sshKey.import });
  fireEvent.click(importButtons[importButtons.length - 1]);
  await waitFor(() => assert.equal(imports.length, 1));
  assert.match(imports[0][0], /BEGIN OPENSSH PRIVATE KEY/);
  await waitFor(() => assert.ok(screen.getByText("SHA256:imported")));

  fireEvent.click(screen.getByRole("button", { name: settings.sshKey.rotate }));
  assert.equal(rotations.length, 0, "regenerating waits for the confirmation");
  await waitFor(() => assert.ok(screen.getByText(settings.sshKey.rotateWarning)));
  fireEvent.click(screen.getByRole("button", { name: "OK" }));
  await waitFor(() => assert.equal(rotations.length, 1));
  await waitFor(() => assert.ok(screen.getByText("SHA256:rotated")));
});

test("the settings card reports a key it cannot load", async () => {
  stub("platformSSHKey", () => { throw new Error("boom"); });
  wrap(<PlatformSSHKeyCard />);
  await waitFor(() => assert.ok(screen.getByText(/Could not load the platform SSH key/)));
});

const instance = { external_id: "pod-1", name: "pod-1", public_ip: "203.0.113.30", ssh_connections: [] } as never;

for (const kind of ["runpod", "vast", "compshare", "autodl"] as const) {
  test(`one-click install on ${kind} needs no credential`, async () => {
    const installs = stub("installAgent", () => ({ checks: [{ name: "SSH authentication", status: "ok" }] }));
    wrap(<ProviderInstallModal providerId={7} providerKind={kind} inst={instance} onClose={() => {}} onDone={() => {}} />);
    assert.ok(screen.getByText(providers.install.autoAuth[kind]));
    fireEvent.click(screen.getByRole("button", { name: providers.install.install }));
    await waitFor(() => assert.equal(installs.length, 1));
    const [id, body] = installs[0];
    assert.equal(id, 7);
    assert.equal(body.external_id, "pod-1");
    assert.equal(body.ssh_key_pem, undefined);
    assert.equal(body.password, undefined);
  });
}

test("credentials given under Advanced are still sent", async () => {
  const installs = stub("installAgent", () => ({ checks: [] }));
  wrap(<ProviderInstallModal providerId={7} providerKind="runpod" inst={instance} onClose={() => {}} onDone={() => {}} />);
  fireEvent.click(screen.getByText(providers.install.advanced));
  fireEvent.change(screen.getByLabelText(providers.install.privateKey), { target: { value: "KEY" } });
  fireEvent.change(screen.getByLabelText(providers.install.password), { target: { value: "pw" } });
  fireEvent.click(screen.getByRole("button", { name: providers.install.install }));
  await waitFor(() => assert.equal(installs.length, 1));
  assert.equal(installs[0][1].ssh_key_pem, "KEY");
  assert.equal(installs[0][1].password, "pw");
});

test("the install details behind the summary can be opened", async () => {
  stub("installAgent", () => ({
    checks: [{ name: "precheck", status: "ok", detail: "fine" }],
    install_details: [
      { name: "Platform key (provider)", status: "ok", detail: "added to the RunPod account SSH keys" },
      { name: "SSH authentication", status: "ok", detail: "platform key SHA256:x accepted" },
    ],
  }));
  wrap(<ProviderInstallModal providerId={7} providerKind="runpod" inst={instance} onClose={() => {}} onDone={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: providers.install.install }));
  await waitFor(() => assert.ok(screen.getByText(providers.install.details)));
  fireEvent.click(screen.getByText(providers.install.details));
  await waitFor(() => assert.ok(screen.getByText("platform key SHA256:x accepted")));
  assert.ok(screen.getByText("added to the RunPod account SSH keys"));
});
