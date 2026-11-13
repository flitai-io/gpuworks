import assert from "node:assert/strict";
import test from "node:test";

import fs from "node:fs";
import { createInstance } from "i18next";

import { instanceEventStage, instanceEventText, lifecycleEventCodes } from "../src/pages/instanceEvents.ts";

const tDeployments = ((key: string, options: Record<string, unknown>) => {
  const values: Record<string, string> = {
    "instance.event.INSTANCE_CREATED": `created (${options.state})`,
    "instance.event.INSTANCE_STATE_CHANGED": `changed to ${options.state}`,
    "instance.event.INSTANCE_STATUS_UPDATED": `reported as ${options.state}`,
    "instance.event.INSTANCE_SLEEP_RESULT": `sleep ${options.result} (L${options.level})`,
    "instance.phase.image_pull": "Downloading Image",
  };
  return values[key] ?? String(options.defaultValue ?? key);
}) as never;

const tCommon = ((key: string, options: Record<string, unknown>) => (
  key === "states.running" ? "Running" : String(options.defaultValue ?? key)
)) as never;

test("structured instance events translate state and phase but preserve raw detail", () => {
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "state",
    code: "INSTANCE_STATUS_UPDATED",
    params: { state: "running", phase: "image_pull", detail: "layer 2/4" },
    created_at: "2026-08-25T00:00:00Z",
  }), "reported as Running [Downloading Image]: layer 2/4");
});

test("sleep result events show the result and effective level", () => {
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "sleep",
    code: "INSTANCE_SLEEP_RESULT",
    params: { result: "failed_rollback", target: 1, actual: 0, detail: "engine timeout" },
    created_at: "2026-08-25T00:00:00Z",
  }), "sleep failed_rollback (L0): engine timeout");
});

test("historical instance event messages remain unchanged", () => {
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "state",
    message: "state → running",
    created_at: "2026-08-25T00:00:00Z",
  }), "state → running");
  assert.equal(instanceEventText(tDeployments, tCommon, {
    kind: "state",
    code: "FUTURE_EVENT",
    message: "future fallback",
    created_at: "2026-08-25T00:00:00Z",
  }), "future fallback");
});

test("startup stages read new params and historical data", () => {
  assert.deepEqual(instanceEventStage({
    kind: "state",
    code: "INSTANCE_STATUS_UPDATED",
    params: { phase: "image_pull", detail: "layer 2/4" },
    data: JSON.stringify({ phase: "image_pull" }),
    created_at: "2026-08-25T00:00:00Z",
  }), { phase: "image_pull", message: "layer 2/4" });
  assert.deepEqual(instanceEventStage({
    kind: "state",
    message: "legacy stage",
    data: JSON.stringify({ phase: "container_start" }),
    created_at: "2026-08-25T00:00:00Z",
  }), { phase: "container_start", message: "legacy stage" });
});

test("every lifecycle event has text in both locales, and a stalled rolling restart names its replica", async () => {
  const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
  for (const lang of ["en", "zh"]) {
    const events = bundle(lang, "deployments").instance.event;
    for (const code of lifecycleEventCodes) assert.ok(events[code], `${lang} has text for ${code}`);
  }
  const i18n = createInstance();
  await i18n.init({ lng: "en", resources: { en: { deployments: bundle("en", "deployments"), common: bundle("en", "common") } }, interpolation: { escapeValue: false } });
  assert.equal(instanceEventText(i18n.getFixedT("en", "deployments") as never, i18n.getFixedT("en", "common") as never, {
    kind: "schedule",
    code: "DEPLOYMENT_ROLLING_STALLED",
    params: { role: "", replica: 2, attempts: 3 },
    created_at: "2026-09-30T00:00:00Z",
  }), "Rolling restart waits: replica 2 gave up after 3 failed attempts; retry it to continue");
  // On a PD deployment prefill and decode each count from 1, so the role says
  // which replica to retry.
  assert.equal(instanceEventText(i18n.getFixedT("en", "deployments") as never, i18n.getFixedT("en", "common") as never, {
    kind: "schedule",
    code: "DEPLOYMENT_ROLLING_STALLED",
    params: { role: "prefill", replica: 2, attempts: 3 },
    created_at: "2026-09-30T00:00:00Z",
  }), "Rolling restart waits: replica Prefill 2 gave up after 3 failed attempts; retry it to continue");
  // A rebuilt replica says which attempt it is, where it starts and why the
  // last one failed; a failure without a message drops the clause.
  const t = i18n.getFixedT("en", "deployments") as never;
  const tc = i18n.getFixedT("en", "common") as never;
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule", code: "REPLICA_REBUILT", params: { role: "decode", replica: 1, attempt: 2, node: "gpu-3", reason: "CUDA out of memory" }, created_at: "2026-09-30T00:00:00Z",
  }), "Replica Decode 1 failed and was rebuilt automatically (attempt 2, on gpu-3); it failed with: CUDA out of memory");
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule", code: "REPLICA_REBUILT", params: { replica: 3, attempt: 3, node: "gpu-3", reason: "" }, created_at: "2026-09-30T00:00:00Z",
  }), "Replica 3 failed and was rebuilt automatically (attempt 3, on gpu-3)");
});

test("a storage outage on the timeline names the storage, the node and the localized state", async () => {
  const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({ lng: "zh", resources: { zh: { deployments: bundle("zh", "deployments"), common: bundle("zh", "common") } }, interpolation: { escapeValue: false } });
  const t = i18n.getFixedT("zh", "deployments") as never;
  const tc = i18n.getFixedT("zh", "common") as never;
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule",
    code: "STORAGE_UNAVAILABLE",
    params: { storage: "nfs-test", node: "gpu3", state: "unresponsive", reason: "statfs: filesystem did not respond" },
    created_at: "2026-10-02T17:30:00Z",
  }), "存储 nfs-test 在节点 gpu3 上不可用（无响应）：运行中的实例继续用显存里的权重服务，正在加载的实例会等它恢复");
  assert.equal(instanceEventText(t, tc, {
    kind: "schedule",
    code: "STORAGE_RECOVERED",
    params: { storage: "nfs-test", node: "gpu3" },
    created_at: "2026-10-02T17:34:00Z",
  }), "存储 nfs-test 在节点 gpu3 上已恢复");
});

// A create that downloads a template component into a shared storage leaves a
// timeline entry naming the component, the storage and the writer node.
test("a component downloaded into a shared storage is named on the timeline", async () => {
  const i18n = createInstance();
  await i18n.init({
    lng: "zh",
    resources: {
      zh: { deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/deployments.json", import.meta.url), "utf8")) },
      en: { deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")) },
    },
    interpolation: { escapeValue: false },
  });
  assert.ok(lifecycleEventCodes.includes("DEPLOYMENT_COMPONENT_DOWNLOAD"));
  const event = {
    kind: "lifecycle",
    code: "DEPLOYMENT_COMPONENT_DOWNLOAD",
    params: { component: "text-encoder", model: "MiniMax/MiniMax-H3", bytes: 66_726_510_529, storage: "models-nfs", node: "h20-1", follows_primary: true },
    created_at: "2026-11-11T08:00:00Z",
  };
  const tc = i18n.getFixedT("zh", "common") as never;
  assert.equal(instanceEventText(i18n.getFixedT("zh", "deployments") as never, tc, event),
    "组件 text-encoder（MiniMax/MiniMax-H3，62 GiB）开始下载到共享存储「models-nfs」，由 h20-1 写入一次，各节点共用这一份");
  assert.equal(instanceEventText(i18n.getFixedT("en", "deployments") as never, tc, event),
    'Started downloading component text-encoder (MiniMax/MiniMax-H3, 62 GiB) into the shared storage "models-nfs", written once by h20-1 and shared by the nodes');
});

test("an operator's order is worded with its scope and names the user who gave it", async () => {
  const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({ lng: "en", resources: { en: { deployments: bundle("en", "deployments"), common: bundle("en", "common") } }, interpolation: { escapeValue: false } });
  const t = i18n.getFixedT("en", "deployments") as never;
  const tc = i18n.getFixedT("en", "common") as never;
  const at = "2026-10-28T00:00:00Z";
  const words = (code: string, params: Record<string, unknown>, withActor?: boolean) => instanceEventText(t, tc, { id: 1, kind: "schedule", code, params, created_at: at }, { withActor });
  assert.equal(words("REPLICAS_CHANGED", { from: 1, to: 3, actor: "alice" }), "Replicas 1 → 3 (by alice)");
  assert.equal(words("REPLICAS_CHANGED", { from: 1, to: 3, actor: "alice" }, false), "Replicas 1 → 3");
  assert.equal(words("REPLICAS_CHANGED", { role: "decode", from: 2, to: 1 }), "Replicas 2 → 1 (Decode)");
  assert.equal(words("SLEEP_ORDERED", { level: 2, mode: "wait", scope: "deployment", actor: "bob" }), "Sleep ordered (L2): the whole deployment (by bob)");
  assert.equal(words("SLEEP_ORDERED", { level: 1, mode: "abort", scope: "role", role: "prefill" }), "Sleep ordered (L1): the Prefill role; in-flight requests aborted");
  assert.equal(words("SLEEP_ORDERED", { level: 1, mode: "wait", scope: "replica", instances: ["abcdef0123456"] }), "Sleep ordered (L1): the replica of instance abcdef01");
  assert.equal(words("WAKE_ORDERED", { instances: ["abcdef0123456", "12345678aaaa"] }), "Wake ordered: instances abcdef01, 12345678");
  // The existing restarts name their actor too.
  assert.match(words("DEPLOYMENT_RESTARTED", { instances: 2, actor: "alice" }), / \(by alice\)$/);
  for (const lang of ["en", "zh"]) {
    const events = bundle(lang, "deployments").instance.event;
    for (const code of ["REPLICAS_CHANGED", "SLEEP_ORDERED", "WAKE_ORDERED"]) assert.ok(events[code], `${lang} has text for ${code}`);
  }
});
