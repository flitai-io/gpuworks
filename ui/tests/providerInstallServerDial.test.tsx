import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["providers"], "en");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: ProviderInstallModal } = await import("../src/components/ProviderInstallModal.tsx");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { api } = await import("../src/api.ts");
type ProviderInstance = import("../src/api.ts").ProviderInstance;
type KindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const direct = { id: "direct", type: "direct", label: { en: "Direct", zh: "直连" }, host: "47.0.0.1", port: 22, user: "root", install_supported: true };
const instance: ProviderInstance = {
  external_id: "i-1", name: "gpu-1", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0,
  region: "cn-shanghai", public_ip: "47.0.0.1", ssh_port: 22, ssh_user: "root", ssh_connections: [direct], kind: "vm", node_id: 0,
};

function mount(props: { providerKind: string; serverDialSupported?: boolean }) {
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind={props.providerKind} serverDialSupported={props.serverDialSupported} inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
}

// The listen address comes before the address the server dials, and the
// install names no server: the agent never contacts it.
test("a server-initiated install asks where the agent listens, then where it is reached", async () => {
  const requests: Record<string, unknown>[] = [];
  Object.assign(api, {
    installAgent: async (_id: number, body: Record<string, unknown>) => {
      requests.push(body);
      return { status: "installing", connect_mode: "server_dial", checks: [] };
    },
  });
  mount({ providerKind: "aliyun" });
  fireEvent.click(screen.getByRole("radio", { name: "Server-initiated connection" }));
  const listen = screen.getByRole("textbox", { name: "Agent listen address" }) as HTMLInputElement;
  const address = screen.getByRole("textbox", { name: "Compute node address" }) as HTMLInputElement;
  assert.equal(listen.value, "0.0.0.0:18800");
  assert.equal(address.value, "47.0.0.1", "the instance's public IP is offered");
  assert.ok(listen.compareDocumentPosition(address) & Node.DOCUMENT_POSITION_FOLLOWING, "the listen address comes first");
  assert.equal(screen.queryByText("Management node address"), null);

  fireEvent.change(address, { target: { value: "47.0.0.1:28800" } });
  fireEvent.click(screen.getByRole("button", { name: /^(loading)?Install$/ }));
  await waitFor(() => assert.equal(requests.length, 1));
  assert.equal(requests[0].server_url, undefined);
  assert.equal(requests[0].server_dial_listen, "0.0.0.0:18800");
  assert.equal(requests[0].server_dial_address, "47.0.0.1:28800");
});

// AutoDL Pro instances open HTTP ports only: the one-click install offers no
// server-initiated connection and points to AutoDL's port documentation.
test("AutoDL offers only agent-initiated installs and links its port documentation", () => {
  mount({ providerKind: "autodl", serverDialSupported: false });
  assert.equal(screen.queryByRole("radio", { name: "Server-initiated connection" }), null);
  const docs = screen.getByRole("link", { name: /AutoDL documentation/ }) as HTMLAnchorElement;
  assert.equal(docs.getAttribute("href"), "https://www.autodl.com/docs/port/");
  assert.ok(screen.getByText("Management node address"));
});

const capabilities = {
  can_list_instances: true, can_power_control: false, can_install_agent: true,
  can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: false,
};

test("adding an AutoDL key says only Pro container instances are listed", () => {
  const kinds: KindInfo[] = [
    { kind: "autodl", label: { en: "AutoDL" }, capabilities, server_dial_supported: false, credential_fields: [{ name: "api_key", label: { en: "API Key" }, secret: true, required: true }] },
    { kind: "vast", label: { en: "Vast.ai" }, capabilities, server_dial_supported: true, credential_fields: [{ name: "api_key", label: { en: "API Key" }, secret: true, required: true }] },
  ];
  const open = (kind: string) => render(
    <I18nextProvider i18n={i18n}>
      <ProviderConfigModal open defaultKind={kind} kindInfos={kinds} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} />
    </I18nextProvider>,
  );
  open("autodl");
  assert.ok(screen.getByText(/lists only Pro container instances/));
  cleanup();
  open("vast");
  assert.equal(screen.queryByText(/lists only Pro container instances/), null);
});

test("adding a RunPod key says only Secure Cloud pods are supported", () => {
  const kinds: KindInfo[] = [
    { kind: "runpod", label: { en: "RunPod" }, capabilities, server_dial_supported: true, credential_fields: [{ name: "api_key", label: { en: "API Key" }, secret: true, required: true }] },
    { kind: "vast", label: { en: "Vast.ai" }, capabilities, server_dial_supported: true, credential_fields: [{ name: "api_key", label: { en: "API Key" }, secret: true, required: true }] },
  ];
  const open = (kind: string) => render(
    <I18nextProvider i18n={i18n}>
      <ProviderConfigModal open defaultKind={kind} kindInfos={kinds} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} />
    </I18nextProvider>,
  );
  open("runpod");
  assert.ok(screen.getByText("Only Secure Cloud pods are supported; Community Cloud pods are not."));
  cleanup();
  open("vast");
  assert.equal(screen.queryByText(/Only Secure Cloud pods/), null);
});

// Vast and RunPod map container ports: left empty, the server picks a free
// mapped port and its address.
test("a port-mapping provider leaves the listen and compute node addresses to the server", async () => {
  const requests: Record<string, unknown>[] = [];
  Object.assign(api, {
    installAgent: async (_id: number, body: Record<string, unknown>) => {
      requests.push(body);
      return { status: "installing", connect_mode: "server_dial", checks: [] };
    },
  });
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind="vast" serverDialAutoPort inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
  fireEvent.click(screen.getByRole("radio", { name: "Server-initiated connection" }));
  const listen = screen.getByRole("textbox", { name: "Agent listen address" }) as HTMLInputElement;
  const address = screen.getByRole("textbox", { name: "Compute node address" }) as HTMLInputElement;
  assert.equal(listen.value, "");
  assert.equal(address.value, "");
  assert.equal(listen.placeholder, "Automatic");
  assert.ok(screen.getByText(/a TCP port the instance maps and nothing holds is used/));
  fireEvent.click(screen.getByRole("button", { name: /^(loading)?Install$/ }));
  await waitFor(() => assert.equal(requests.length, 1));
  assert.equal(requests[0].server_dial_listen, undefined);
  assert.equal(requests[0].server_dial_address, undefined);
  assert.equal(requests[0].allow_instance_restart, undefined);
});

// RunPod opens a port only by restarting the pod: the server refuses until
// the operator agrees, and the dialog asks, saying why, then installs again.
test("a RunPod pod without a spare port is restarted only after the operator agrees", async () => {
  const { APIError } = await import("../src/apiError.ts");
  const requests: Record<string, unknown>[] = [];
  Object.assign(api, {
    installAgent: async (_id: number, body: Record<string, unknown>) => {
      requests.push(body);
      if (!body.allow_instance_restart) {
        throw new APIError("INSTANCE_RESTART_REQUIRED", undefined, "the change the install needs restarts the instance", 409);
      }
      return { status: "installing", connect_mode: "server_dial", checks: [] };
    },
  });
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind="runpod" serverDialAutoPort inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
  fireEvent.click(screen.getByRole("radio", { name: "Server-initiated connection" }));
  fireEvent.click(screen.getByRole("button", { name: /^(loading)?Install$/ }));
  await screen.findByText(/restarts its container and clears what is not on a volume/);
  assert.equal(requests.length, 1);
  fireEvent.click(screen.getByRole("button", { name: "Confirm and restart the pod" }));
  await waitFor(() => assert.equal(requests.length, 2));
  assert.equal(requests[1].allow_instance_restart, true);
});

// A pod started before the platform key was in the account: even an
// agent-initiated install needs the key added, which restarts the pod, so
// the dialog asks, saying it is for the key.
test("a RunPod pod without the platform key is restarted only after the operator agrees", async () => {
  const { APIError } = await import("../src/apiError.ts");
  const requests: Record<string, unknown>[] = [];
  Object.assign(api, {
    installAgent: async (_id: number, body: Record<string, unknown>) => {
      requests.push(body);
      if (!body.allow_instance_restart) {
        throw new APIError("INSTANCE_RESTART_REQUIRED", { need: "ssh_key" }, "the change the install needs restarts the instance", 409);
      }
      return { status: "installing", connect_mode: "agent_dial", checks: [] };
    },
  });
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind="runpod" serverDialAutoPort inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: /^(loading)?Install$/ }));
  await screen.findByText(/started before the GPUWorks platform key was added to the RunPod account/);
  fireEvent.click(screen.getByRole("button", { name: "Confirm and restart the pod" }));
  await waitFor(() => assert.equal(requests.length, 2));
  assert.equal(requests[0].connect_mode, "agent_dial");
  assert.equal(requests[1].allow_instance_restart, true);
});
