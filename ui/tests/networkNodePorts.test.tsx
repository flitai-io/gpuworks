import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { NetworkDetail, NodeNetworks } from "../src/networkTypes";

const { cleanup, fireEvent, render, screen, within } = await import("@testing-library/react");
const { RailsCell, BootstrapCell } = await import("../src/pages/networks/RDMAEndpoints.tsx");
const { api } = await import("../src/api.ts");
const { i18n } = await createI18n(["networks"]);
afterEach(cleanup);

test("a long list of HCA ports retains every port, its remove action, and keeps add outside the scrolling list", () => {
  const detail: NetworkDetail = JSON.parse(readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));
  const base = detail.nodes[0];
  const hcas = Array.from({ length: 64 }, (_, i) => ({
    ...base.hcas[0], name: `mlx5_${i}:1`, ip: i === 63 ? "" : `192.0.2.${i + 1}`,
  }));
  render(<I18nextProvider i18n={i18n}><RailsCell network={detail} node={{ ...base, hcas }} onChanged={() => {}} /></I18nextProvider>);
  const ports = screen.getByRole("list", { name: i18n.t("nodeTable.hcaIPs") });
  const entries = within(ports).getAllByRole("listitem");
  assert.equal(entries.length, 64);
  assert.ok(within(entries[63]).getByText("mlx5_63:1"));
  assert.ok(within(entries[63]).getByText("—"));
  assert.ok(within(entries[63]).getByRole("button", { name: "Remove mlx5_63:1" }));
  assert.equal(ports.contains(screen.getByRole("button", { name: new RegExp(i18n.t("rail.add")) })), false);
});

const inventory: NodeNetworks = {
  inventory_reported: true, networks: [], management: { interface: "", ip: "", roles_off: [], behind_nat: false }, serving_forward: null,
  interfaces: [], rdma_ports: [], accelerator_ports: [],
};

for (const bootstrap of [true, false]) {
  test(`${bootstrap ? "bootstrap" : "port"} picker can retry inventory reads and distinguishes pending and empty inventory`, async () => {
    const detail: NetworkDetail = JSON.parse(readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));
    let reads = 0;
    const original = api.nodeNetworks;
    api.nodeNetworks = async () => {
      reads++;
      if (reads === 1) throw new Error("unavailable");
      if (reads === 2) return { ...inventory, inventory_reported: false };
      if (reads === 3) return inventory;
      return { ...inventory, rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 100, netdev: "", ip: "" }] };
    };
    try {
      const Cell = bootstrap ? BootstrapCell : RailsCell;
      render(<I18nextProvider i18n={i18n}><Cell network={detail} node={detail.nodes[0]} onChanged={() => {}} /></I18nextProvider>);
      fireEvent.click(screen.getByRole("button", { name: new RegExp(i18n.t(bootstrap ? "rail.changeBootstrap" : "rail.add")) }));
      assert.equal((await screen.findByRole("alert")).textContent, i18n.t("rail.inventoryReadFailed"));
      fireEvent.click(screen.getByRole("button", { name: i18n.t("detail.retry") }));
      await screen.findByText(i18n.t("rail.inventoryPending"));
      fireEvent.click(screen.getByRole("button", { name: i18n.t("detail.retry") }));
      await screen.findByText(i18n.t(bootstrap ? "rail.noAddressedInterface" : "rail.noReportedPorts"));
      if (!bootstrap) {
        fireEvent.click(screen.getByRole("button", { name: i18n.t("detail.retry") }));
        await screen.findByText(i18n.t("rail.allPortsUsed"));
      }
      fireEvent.click(screen.getByRole("button", { name: i18n.t("actions.cancel") }));
      assert.equal(screen.queryByRole("status"), null);
    } finally {
      api.nodeNetworks = original;
    }
  });
}
