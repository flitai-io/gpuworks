import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

import { createInstance } from "i18next";

import { nodeReplacementEventCodes } from "../src/pages/nodeReplacementEvents.ts";
import { instanceEventText } from "../src/pages/instanceEvents.ts";

const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));

async function texts(lang: "en" | "zh") {
  const i18n = createInstance();
  await i18n.init({ lng: lang, resources: { [lang]: { deployments: bundle(lang, "deployments"), common: bundle(lang, "common") } }, interpolation: { escapeValue: false } });
  const t = i18n.getFixedT(lang, "deployments") as never;
  const tc = i18n.getFixedT(lang, "common") as never;
  return (code: string, params: Record<string, unknown>) =>
    instanceEventText(t, tc, { id: 1, kind: "automation", code, params, created_at: "2026-10-25T00:00:00Z" });
}

const uuid = "0123456789abcdef";

test("every node replacement event has text in both locales", () => {
  for (const lang of ["en", "zh"]) {
    const events = bundle(lang, "deployments").instance.event;
    for (const code of nodeReplacementEventCodes) assert.ok(events[code], `${lang} has text for ${code}`);
  }
});

test("node replacement entries name the node, the wait, the replica and where it went", async () => {
  const text = await texts("en");
  assert.equal(text("NODE_REPLACED", { from: "gpu-a", to: "gpu-b", after_sec: 600, unit: 2, rank: 0 }), "Reschedule on node loss: node gpu-a stayed offline for over 10 min, replica 2 rescheduled to gpu-b");
  assert.equal(text("NODE_REPLACED", { from: "gpu-a", after_sec: 600, instances: [uuid] }), "Reschedule on node loss: node gpu-a stayed offline for over 10 min, a replica is rebuilt on another node");
  assert.equal(text("NODE_REPLACED", { from: "gpu-a", to: "gpu-b", after_sec: 7200, unit: 1, role: "prefill" }), "Reschedule on node loss: node gpu-a stayed offline for over 2 h, replica Prefill 1 rescheduled to gpu-b");
  assert.equal(text("NODE_REPLACE_REVERTED", { from: "gpu-b", to: "gpu-a", unit: 2, rank: 0 }), "Node gpu-a came back before replica 2 ran on gpu-b: the rescheduling is undone");
  assert.equal(text("NODE_REPLACE_NO_CANDIDATE", { nodes: ["gpu-a", "gpu-c"] }), "Nodes gpu-a, gpu-c stayed offline, but no other node can take their replicas");
  assert.equal(
    text("NODE_REPLACE_NO_CANDIDATE", { nodes: ["gpu-a"], ucx_netlink_hang: 1, ucx_netlink_nodes: ["gpu-c"] }),
    "Nodes gpu-a stayed offline, but no other node can take their replicas. Left out because their kernel hangs UCX, which this PD mode's NIXL uses: gpu-c. Turn IPv6 off on them to use them (see the node page).",
  );
  assert.equal(text("NODE_DRAIN_MOVED", { unit: 3 }), "replica 3 moved off a draining node");
});

test("the same entries read in Chinese, and a payload without params still renders", async () => {
  const text = await texts("zh");
  assert.equal(text("NODE_REPLACED", { from: "gpu-a", to: "gpu-b", after_sec: 600, unit: 2 }), "节点失联重调度：节点 gpu-a 离线超过 10 分钟，副本 2 已重调度到 gpu-b");
  assert.ok(text("NODE_REPLACE_NO_CANDIDATE", {}).length > 0);
});

test("an automatic re-pick names the unit, where it went, where it was and why", async () => {
  const text = await texts("en");
  assert.equal(
    text("UNIT_PLACED", { unit: 2, from: ["gpu-a"], to: ["gpu-b"], reason: "pinned_gpus_unavailable" }),
    "Automatic placement: replica 2 placed on gpu-b (was on gpu-a): the cards it was pinned to are held by other deployments",
  );
  assert.equal(
    text("UNIT_PLACED", { unit: 1, role: "decode", from: ["gpu-a", "gpu-b"], to: ["gpu-c", "gpu-d"], reason: "node_cordoned" }),
    "Automatic placement: replica Decode 1 placed on gpu-c, gpu-d (was on gpu-a, gpu-b): a node it was on stopped taking replicas",
  );
  // A reason code the client has no cause for reads as the general cause, not as the raw code.
  assert.equal(
    text("UNIT_PLACED", { unit: 3, from: ["gpu-a"], to: ["gpu-b"], reason: "something_new" }),
    "Automatic placement: replica 3 placed on gpu-b (was on gpu-a): its previous nodes could not take it",
  );
  // A first placement has no previous nodes and no reason.
  assert.equal(text("UNIT_PLACED", { unit: 1, to: ["gpu-b"] }), "Automatic placement: replica 1 placed on gpu-b");
});

test("the automatic re-pick reads in Chinese", async () => {
  const text = await texts("zh");
  assert.equal(
    text("UNIT_PLACED", { unit: 1, role: "prefill", from: ["gpu-a"], to: ["gpu-b"], reason: "pinned_gpus_unavailable" }),
    "自动放置：副本 Prefill 1 已放到 gpu-b（原在 gpu-a）：它钉住的卡被其他部署占用",
  );
});
