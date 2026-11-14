import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { list } from "./helpers/listPage.ts";
import { account } from "./helpers/platformFixtures.ts";
import type { ProviderInstance, ProviderKindInfo } from "../src/api.ts";

const { i18n } = await createI18n(["providers"], "en");
const { cleanup, render, screen } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { default: ProviderResources } = await import("../src/pages/platforms/ProviderResources.tsx");
const { api } = await import("../src/api.ts");
const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const instance: ProviderInstance = {
  external_id: "host-a", name: "Test host", gpu_model: "A100", gpu_count: 1,
  power: "running", price_per_hr: 0, region: "zone-a", public_ip: "", ssh_port: 22,
  ssh_user: "root", ssh_connections: [], kind: "vm", node_id: 1, actions: [],
};

for (const pricing of [false, true, undefined]) {
  test(`pricing capability ${pricing} controls the price column even when the price is zero`, async () => {
    api.providerInstances = async () => list([instance]);
    const kindInfo: ProviderKindInfo | undefined = pricing === undefined ? undefined : {
      kind: pricing ? "runpod" : "zstack", label: "Test provider", category: "public_cloud", resources: [{ type: "vm" }], credential_fields: [], server_dial_supported: false,
      capabilities: {
        can_list_instances: true, can_power_control: false, can_install_agent: false,
        can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: pricing,
      },
    };
    // An account's detail (ui/src/pages/platforms/ProviderResources.tsx) of a kind without managed Kubernetes.
    render(<I18nextProvider i18n={i18n}><MemoryRouter><ProviderResources provider={account(1, "Test cloud", "testcloud", { resources: [{ type: "vm" }] })} kindInfo={kindInfo} refreshKey="initial" entries={{}} onEntriesChanged={() => {}} admin={false} /></MemoryRouter></I18nextProvider>);
    await screen.findByText("Test host");
    assert.equal(screen.queryAllByRole("columnheader", { name: "Price" }).length > 0, pricing === true);
    if (pricing) assert.ok(screen.getByText("¥0.00/hr"));
    assert.ok(screen.getByText("A100 ×1"));
  });
}
