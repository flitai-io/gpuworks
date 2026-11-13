import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["providers"], "en");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: ProviderInstallModal } = await import("../src/components/ProviderInstallModal.tsx");
const { default: ProviderInstancesTable } = await import("../src/components/ProviderInstancesTable.tsx");
const { api } = await import("../src/api.ts");
type ProviderInstance = import("../src/api.ts").ProviderInstance;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const assistant = { id: "cloud_assistant", type: "cloud_assistant", label: { en: "Cloud Assistant", zh: "云助手" }, host: "", port: 0, user: "", install_supported: true };
const direct = { id: "direct", type: "direct", label: { en: "Direct", zh: "直连" }, host: "47.0.0.1", port: 22, user: "root", install_supported: true };
const instance: ProviderInstance = {
  external_id: "i-1", name: "gpu-1", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0,
  region: "cn-shanghai", public_ip: "47.0.0.1", ssh_port: 22, ssh_user: "root", ssh_connections: [assistant, direct], kind: "vm", node_id: 0, actions: ["install"],
};

test("the instance list shows Cloud Assistant as a path without SSH", async () => {
  Object.assign(api, { providerInstances: async () => ({ items: [instance], total: 1 }) });
  render(<I18nextProvider i18n={i18n}><ProviderInstancesTable providerId={1} refreshKey="x" /></I18nextProvider>);
  await screen.findByText("gpu-1");
  assert.ok(screen.getByText("Cloud Assistant"));
  assert.ok(screen.getByText("no SSH needed"));
  assert.ok(screen.getByText("root@47.0.0.1:22"));
  assert.equal(screen.queryByText(/@:0/), null, "Cloud Assistant has no endpoint to print");
});

test("the install dialog offers Cloud Assistant by name and reports it was used", async () => {
  const requests: unknown[] = [];
  Object.assign(api, {
    installAgent: async (_id: number, body: unknown) => {
      requests.push(body);
      return {
        status: "installing", connect_mode: "agent_dial", ssh_connection: assistant,
        checks: [
          { name: "precheck", status: "ok", detail: "all 5 checks passed" },
          { name: "install agent", status: "ok", detail: "gpuworks: agent started via systemd." },
          { name: "agent_dial", status: "ok", detail: "agent session established" },
        ],
      };
    },
  });
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind="aliyun" inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
  assert.ok(screen.getByText(/through Alibaba Cloud Cloud Assistant/));
  assert.ok(screen.getByText("Connection"));
  fireEvent.mouseDown(document.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(document.querySelectorAll(".ant-select-item-option").length >= 3));
  const options = [...document.querySelectorAll(".ant-select-item-option")].map((el) => el.textContent);
  assert.deepEqual(options, ["Automatic", "Cloud Assistant", "Direct · root@47.0.0.1:22"]);
  fireEvent.click([...document.querySelectorAll(".ant-select-item-option")][1] as HTMLElement);

  fireEvent.click(screen.getByRole("button", { name: /^(loading)?Install$/ }));
  await waitFor(() => assert.ok(screen.getByText("Used Cloud Assistant")));
  assert.equal((requests[0] as { ssh_connection_id: string }).ssh_connection_id, "cloud_assistant");
});
