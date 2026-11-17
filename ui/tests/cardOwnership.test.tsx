import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["common"], "zh");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: GPUHealthTag, gpuHealthStatus, gpuOwned } = await import("../src/components/GPUHealth.tsx");
const { cardKey, cardHasDetail } = await import("../src/nodes/cardIdentity.ts");
type GPUT = import("../src/api.ts").GPU;

afterEach(() => cleanup());

const card = (over: Partial<GPUT>): GPUT => ({
  index: 0, uuid: "", name: "GA102 [GeForce RTX 3090]", compute_capability: "", arch_family: "",
  memory_total_bytes: 0, numa_node: 0, schedulable: false, ...over,
} as GPUT);

const show = (gpu: GPUT) => render(<I18nextProvider i18n={i18n}>{<GPUHealthTag gpu={gpu} />}</I18nextProvider>);

// A card given to a virtual machine is not broken. Showing it in red as a
// fault would send an operator looking for a problem that is not there.
test("a card owned elsewhere shows whose it is, not a health verdict", () => {
  show(card({ ownership: "passthrough", pcie_bdf: "0000:1b:00.0", index: -1 }));
  assert.ok(screen.getByText("已直通"));
  assert.equal(screen.queryByText("故障"), null);
  cleanup();

  show(card({ ownership: "no_driver", pcie_bdf: "0000:1a:00.0", index: -1 }));
  assert.ok(screen.getByText("未绑定驱动"));
  assert.equal(screen.queryByText("故障"), null);
});

// A card whose own driver could not bring it up is this node's card and is
// broken; it reads as a fault, with what to do about it.
test("a card its driver could not bring up reads as a fault", () => {
  show(card({
    pcie_bdf: "0000:1c:00.0", index: -1,
    health_status: "Error", health_code: "DRIVER_INIT_FAILED",
    health_message: "PCI Address: 0000:1c:00.0",
  }));
  assert.ok(screen.getByText("驱动初始化失败"));
  assert.ok(screen.getByRole("button", { name: "查看健康详情" }), "the finding explains itself on demand");
});

test("ownership decides whether health is even asked", () => {
  assert.equal(gpuOwned(card({})), true);
  assert.equal(gpuOwned(card({ ownership: "" })), true);
  assert.equal(gpuOwned(card({ ownership: "passthrough" })), false);
  // A card lent elsewhere takes no work because it is not ours, not
  // because anything ails it, so health must not read it as a fault.
  assert.equal(gpuHealthStatus(card({ ownership: "passthrough" })), "OK");
  // A fault the vendor tool actually found is still shown, whoever holds
  // the card: the hardware is faulty wherever it is plugged in.
  assert.equal(gpuHealthStatus(card({ ownership: "passthrough", health_status: "Error" })), "Error");
});

// Several cards of one node can lack a device index, so the index cannot
// tell them apart.
test("cards without a device index are still told apart", () => {
  const passthrough = card({ ownership: "passthrough", pcie_bdf: "0000:1b:00.0", index: -1 });
  const noDriver = card({ ownership: "no_driver", pcie_bdf: "0000:1a:00.0", index: -1 });
  assert.notEqual(cardKey(passthrough), cardKey(noDriver));
  assert.equal(cardKey(passthrough), "pci:0000:1b:00.0");
  // The PCI address wins over the UUID, as it does on the server.
  assert.equal(cardKey(card({ pcie_bdf: "0000:3F:00.0", uuid: "ascend-3", index: 3 })), "pci:0000:3f:00.0");
  assert.equal(cardKey(card({ uuid: "GPU-a", index: 1 })), "uuid:GPU-a");
  assert.equal(cardKey(card({ index: 2 })), "index:2");
});

// Only a card the driver answered for has metrics and a page of its own.
test("a card known only by its PCI address has no detail page", () => {
  assert.equal(cardHasDetail(card({ uuid: "GPU-a" })), true);
  assert.equal(cardHasDetail(card({ pcie_bdf: "0000:1c:00.0" })), false);
});
