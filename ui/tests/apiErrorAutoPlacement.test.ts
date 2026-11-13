import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";

import { APIError, apiErrorText } from "../src/apiError.ts";

// The product errors of automatic multi-node and PD placement read in the
// user's language with the figures the server sends.

async function textFor(lng: "en" | "zh", code: string, params: Record<string, unknown>) {
  const i18n = createInstance();
  const common = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/common.json`, import.meta.url), "utf8"));
  await i18n.init({ lng, resources: { [lng]: { common } }, defaultNS: "common", interpolation: { escapeValue: false } });
  return apiErrorText((key, options) => i18n.t(key, options), new APIError(code, params, "fallback", 409));
}

const none = { nodes: 12, unmet: 3, cordoned: 1, offline: 2, busy: 4, no_network: 1, largest: 1 };

test("nothing to place on says how many nodes each check left out", async () => {
  assert.equal(
    await textFor("en", "UNIT_PLACEMENT_NONE", none),
    "No group of nodes can take this deployment (of 12 nodes): 3 do not satisfy the requirements · 1 cordoned · 2 offline · 4 lack free GPUs · 1 without a compute network · largest group sharing a network and GPU architecture: 1",
  );
  assert.equal(
    await textFor("zh", "UNIT_PLACEMENT_NONE", none),
    "没有能放下该部署的节点组（共 12 个节点）：3 个不满足条件 · 1 个已停止调度 · 2 个离线 · 4 个空闲 GPU 不够 · 1 个没有参数网络 · 同网络同架构最多 1 个节点",
  );
});

test("a check that left no node out is not listed", async () => {
  assert.equal(
    await textFor("zh", "UNIT_PLACEMENT_NONE", { nodes: 4, unmet: 0, cordoned: 0, offline: 1, busy: 3, no_network: 0, largest: 1 }),
    "没有能放下该部署的节点组（共 4 个节点）：1 个离线 · 3 个空闲 GPU 不够 · 同网络同架构最多 1 个节点",
  );
});

test("the shape errors of automatic placement are worded, not left as codes", async () => {
  for (const lng of ["en", "zh"] as const) {
    for (const [code, params] of [
      ["AUTO_PLACEMENT_TP_REQUIRED", {}],
      ["AUTO_PLACEMENT_PD_MULTI_NODE", { role: "decode" }],
      ["PLACEMENT_MODE_SHAPE", {}],
    ] as const) {
      const text = await textFor(lng, code, params);
      assert.notEqual(text, "fallback");
      assert.ok(!text.includes("{{"), `${lng} ${code}: ${text}`);
    }
  }
  assert.ok((await textFor("en", "AUTO_PLACEMENT_PD_MULTI_NODE", { role: "decode" })).includes("decode role"));
  assert.ok((await textFor("zh", "AUTO_PLACEMENT_PD_MULTI_NODE", { role: "decode" })).includes("decode 角色"));
});
