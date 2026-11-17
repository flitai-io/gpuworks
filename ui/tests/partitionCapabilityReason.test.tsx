import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: PartitionCapability } = await import("../src/components/PartitionCapability.tsx");
type Capability = import("../src/api.ts").PartitionCapability;

afterEach(() => cleanup());

const base: Capability = { backend: "ppu-dgpu", display_name: "PPU dGPU", vendor: "ppu", maturity: "release", status: "unavailable" };

async function show(lang: "en" | "zh", capability: Capability) {
  const { i18n } = await createI18n(["common"], lang);
  return render(<I18nextProvider i18n={i18n}><PartitionCapability capability={capability} /></I18nextProvider>);
}

// The reason is rendered from its code and params in the reader's language,
// never from the server's English fallback.
test("an unavailable capability reads its reason from the code", async () => {
  const capability: Capability = {
    ...base, reason: "container inference unverified: the backend requires the docker, kube runtime, the node runs udocker",
    reason_code: "PARTITION_RUNTIME_UNSUPPORTED", reason_params: { runtime: "udocker", supported_runtimes: ["docker", "kube"] },
  };
  await show("zh", capability);
  assert.ok(screen.getByText("需要 docker, kube 运行时，节点使用的是 udocker。"));
  assert.equal(screen.queryByText(capability.reason!), null, "the English fallback is not shown");
  cleanup();
  await show("en", capability);
  assert.ok(screen.getByText("Needs the docker, kube runtime; the node runs udocker."));
});

// The probe's own output is evidence beside the reason, not the reason.
test("a failed probe shows the localized reason with the probe output as evidence", async () => {
  await show("zh", {
    ...base, probe_status: "failed", reason: "dGPU probe failed: quota was 32768 MB",
    reason_code: "PARTITION_PROBE_FAILED", reason_params: { probe_detail: "quota was 32768 MB" },
  });
  assert.ok(screen.getByText("dGPU 探针失败。"));
  assert.ok(screen.getByText("quota was 32768 MB"));
});

test("missing assets are named in the reason", async () => {
  await show("en", { ...base, reason_code: "PARTITION_ASSETS_MISSING", reason_params: { missing_assets: ["hggc_quota.so", "hgml_dlsym_hook.so"] } });
  assert.ok(screen.getByText("dGPU assets are missing: hggc_quota.so, hgml_dlsym_hook.so."));
});

// A server that predates codes sends only the English reason; it is shown
// as written rather than dropped.
test("a reason without a code is shown as written", async () => {
  await show("zh", { ...base, reason: "agent has not reported current dGPU safety capability; upgrade the agent" });
  assert.ok(screen.getByText("agent has not reported current dGPU safety capability; upgrade the agent"));
});

test("a ready capability shows no reason", async () => {
  const { container } = await show("en", { ...base, status: "ready" });
  assert.ok(container.textContent?.includes("dGPU available"));
  assert.equal(container.textContent?.includes("missing"), false);
});
