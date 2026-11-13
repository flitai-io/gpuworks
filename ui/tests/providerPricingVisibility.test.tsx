import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { list } from "./helpers/listPage.ts";
import type { ProviderInstance, ProviderKindInfo } from "../src/api.ts";

const { i18n } = await createI18n(["providers"], "en");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: ProviderInstancesTable } = await import("../src/components/ProviderInstancesTable.tsx");
const { api } = await import("../src/api.ts");
const original = api.providerInstances;
afterEach(() => { cleanup(); api.providerInstances = original; });

const instance: ProviderInstance = {
  external_id: "host-a", name: "Test host", gpu_model: "A100", gpu_count: 1,
  power: "running", price_per_hr: 0, region: "zone-a", public_ip: "", ssh_port: 22,
  ssh_user: "root", ssh_connections: [], kind: "vm", node_id: 1, actions: [],
};

for (const pricing of [false, true, undefined]) {
  test(`pricing capability ${pricing} controls the price column even when the price is zero`, async () => {
    api.providerInstances = async () => list([instance]);
    const kindInfo: ProviderKindInfo | undefined = pricing === undefined ? undefined : {
      kind: pricing ? "runpod" : "zstack", label: "Test provider", credential_fields: [], server_dial_supported: false,
      capabilities: {
        can_list_instances: true, can_power_control: false, can_install_agent: false,
        can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: pricing,
      },
    };
    render(<I18nextProvider i18n={i18n}><ProviderInstancesTable providerId={1} kindInfo={kindInfo} refreshKey="initial" /></I18nextProvider>);
    await screen.findByText("Test host");
    assert.equal(screen.queryAllByRole("columnheader", { name: "Price", exact: true }).length > 0, pricing === true);
    if (pricing) assert.ok(screen.getByText("¥0.00/hr"));
    assert.ok(screen.getByText("A100"));
  });
}
