import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { APIError, apiErrorText } from "../src/apiError.ts";
import { createInstance } from "i18next";

import { placementRefusalText, scheduleText } from "../src/pages/deploy/deploySchedule.ts";

const locale = (lng: string, ns = "deploy") =>
  JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: { deploy: locale("en"), deployments: locale("en", "deployments"), common: locale("en", "common") },
    zh: { deploy: locale("zh"), deployments: locale("zh", "deployments"), common: locale("zh", "common") },
  },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});

test("structured scheduler messages translate with interpolation params", () => {
  const calls: Array<{ key: string; options: Record<string, unknown> }> = [];
  const t = ((key: string, options: Record<string, unknown>) => {
    calls.push({ key, options });
    return `tp=${options.tp}, heads=${options.head_count}`;
  }) as never;

  assert.equal(
    scheduleText(t, { code: "tp_head_count_not_divisible", params: { tp: 4, head_count: 6 } }),
    "tp=4, heads=6",
  );
  assert.deepEqual(calls, [{
    key: "schedule.tp_head_count_not_divisible",
    options: { tp: 4, head_count: 6, defaultValue: "" },
  }]);
});

test("historical scheduler strings remain unchanged", () => {
  const t = (() => "unexpected") as never;
  assert.equal(scheduleText(t, "历史调度说明"), "历史调度说明");
});

test("missing scheduler translations fall back to legacy message and then code", () => {
  const t = ((key: string) => key) as never;
  assert.equal(scheduleText(t, { code: "future_code", message: "Legacy fallback" }), "Legacy fallback");
  assert.equal(scheduleText(t, { code: "future_code" }), "future_code");
  assert.equal(scheduleText(t, {}), "");
});

// A delivery the deployment waits on - a peer transfer or an archive URL - used
// to fail with no trace the user could see. The rendered refusal has to name
// what failed and carry the ledger's own text, which is the part that says
// whether to retry or pick another source.
test("a failed image delivery names the node, the image and the reported failure", () => {
  for (const lng of ["en", "zh"]) {
    const t = i18n.getFixedT(lng, "deploy");
    const detail = "dial model sync target: 10.42.14.163:34787: connect: no route to host";
    const text = scheduleText(t, {
      code: "image_delivery_failed",
      params: { node: "gpu3", image: "gpustack/runner:v1", source: "peer", detail },
    });
    assert.match(text, /gpu3/, `${lng} refusal does not name the node`);
    assert.match(text, /gpustack\/runner:v1/, `${lng} refusal does not name the image`);
    assert.ok(text.includes(detail), `${lng} refusal drops the reported failure: ${text}`);
  }
});

// The ledger does not always have a message, and a reason omits params it has
// no value for; the message must not render a placeholder in their place.
test("a failed image delivery without a reported message renders no placeholder", () => {
  for (const lng of ["en", "zh"]) {
    const t = i18n.getFixedT(lng, "deploy");
    const text = scheduleText(t, {
      code: "image_delivery_failed",
      params: { node: "gpu3", image: "gpustack/runner:v1" },
    });
    assert.match(text, /gpu3/, `${lng} refusal does not name the node`);
    assert.doesNotMatch(text, /\{\{/, `${lng} refusal rendered an unfilled placeholder: ${text}`);
  }
});

// A model stuck on a node holds every replica the pool would put there. The
// refusal names the model and the node, carries the failure when the
// acquisition reported one, and renders no placeholder when it did not.
test("a model stuck on a node names it and carries the reported failure", () => {
  for (const lng of ["en", "zh"]) {
    const t = i18n.getFixedT(lng, "deploy");
    const detail = "send peer model archive: dial model sync target: ";
    const text = scheduleText(t, {
      code: "model_unavailable",
      params: { node: "gw-acc-c", model: "Qwen/Qwen3-0.6B", state: "failed", detail },
    });
    assert.match(text, /gw-acc-c/, `${lng} refusal does not name the node`);
    assert.match(text, /Qwen\/Qwen3-0\.6B/, `${lng} refusal does not name the model`);
    assert.ok(text.includes(detail.trim()), `${lng} refusal drops the reported failure: ${text}`);

    const bare = scheduleText(t, {
      code: "model_unavailable",
      params: { node: "gw-acc-c", model: "Qwen/Qwen3-0.6B", state: "download_required" },
    });
    assert.doesNotMatch(bare, /\{\{/, `${lng} refusal rendered an unfilled placeholder: ${bare}`);
  }
});

test("a storage refusal says which storage and why, in both languages", () => {
  const notOnNode = { code: "storage_unavailable", params: { storage: "lab-nfs", storage_id: 2, reason: "storage_not_on_node" } };
  const en = scheduleText(i18n.getFixedT("en", "deploy"), notOnNode);
  assert.match(en, /Storage lab-nfs does not apply to this node/);
  assert.match(en, /storage node condition/);
  const zh = scheduleText(i18n.getFixedT("zh", "deploy"), notOnNode);
  assert.match(zh, /存储 lab-nfs 不用在这台节点上/);
  assert.match(scheduleText(i18n.getFixedT("zh", "deploy"), { ...notOnNode, params: { ...notOnNode.params, reason: "storage_unavailable" } }), /lab-nfs 在这台节点上挂载不可用/);
  // A reason without a specific text, or without a storage, keeps the general one.
  assert.match(scheduleText(i18n.getFixedT("en", "deploy"), { code: "storage_unavailable", params: { storage: "lab-nfs", reason: "shared_path_invalid" } }), /not ready on this node/);
  assert.match(scheduleText(i18n.getFixedT("en", "deploy"), { code: "storage_unavailable", params: { reason: "storage_not_on_node" } }), /not ready on this node/);
});

test("a source rule that forbids every image source names the node and the image and points to the rules", () => {
  const reason = { code: "image_source_forbidden", params: { node: "gpu-b-02", image: "docker.io/lmsysorg/sglang:v0.5.21" } };
  const en = scheduleText(i18n.getFixedT("en", "deploy"), reason);
  assert.match(en, /node gpu-b-02 forbid every source of image docker\.io\/lmsysorg\/sglang:v0\.5\.21/);
  assert.match(en, /Settings → Pull settings → Source rules/);
  const zh = scheduleText(i18n.getFixedT("zh", "deploy"), reason);
  assert.match(zh, /节点 gpu-b-02 适用的来源规则禁止了镜像 docker\.io\/lmsysorg\/sglang:v0\.5\.21/);
  assert.doesNotMatch(en + zh, /\{\{/);
});

test("a pinned PD unit waiting on cards other deployments hold names the node, the cards and the holders in both languages", () => {
  const reason = { code: "pinned_gpus_unavailable", params: { node: "gpu-a", gpus: "0, 1", holders: "chat, embed" } };
  const en = i18n.getFixedT("en", "deploy") as never;
  const zh = i18n.getFixedT("zh", "deploy") as never;
  assert.equal(
    scheduleText(en, reason),
    "GPU 0, 1 on gpu-a, which this unit is pinned to, is held by chat, embed. The unit waits for it; switch the deployment to automatic placement to let the platform use free GPUs instead.",
  );
  assert.equal(
    scheduleText(zh, reason),
    "该单元钉住的 gpu-a 上的 GPU 0, 1 正被 chat, embed 占用，单元会一直等待；把部署的放置方式改为自动，平台就会改用空闲的卡。",
  );
});

test("a replica waiting for holders to yield names each one and its terms", async () => {
  const reason = {
    code: "waiting_for_yield",
    params: {
      node: "gpu-1",
      instances: ["b-1", "s-1", "o-1"],
      holders: [
        { deployment: "batch", at_once: true },
        { deployment: "shared", at_once: false, idle_sec: 30, max_serve_sec: 120 },
        { deployment: "chat", at_once: false, idle_sec: 60 },
      ],
    },
  };
  await i18n.changeLanguage("zh");
  assert.equal(
    scheduleText(i18n.getFixedT("zh", "deploy") as never, reason),
    "等待：batch 立即让出 GPU；shared 闲置 30 秒或本轮服务满 2 分钟后让出 GPU；chat 闲置 1 分钟后让出 GPU。之后在 gpu-1 上放置副本。",
  );
  assert.equal(
    scheduleText(i18n.getFixedT("en", "deploy") as never, reason),
    "Waiting until batch gives up its GPUs now; shared gives up its GPUs after 30 s idle or 2 min of serving; chat gives up its GPUs after 1 min idle; the replica then goes on gpu-1.",
  );
  // With no holder left to put to sleep, it waits for the sleeps ordered.
  assert.equal(
    scheduleText(i18n.getFixedT("zh", "deploy") as never, { code: "waiting_for_yield", params: { node: "gpu-1", holders: [], instances: [] } }),
    "等待：已让出 GPU 的部署完成休眠。之后在 gpu-1 上放置副本。",
  );
});

test("a replica kept off a sleeper's card names the sleeper, its policy and what to do", () => {
  const reason = { code: "reserved_for_sleepers", params: { node: "gpu3", gpus: [0], sleepers: [{ deployment: "qwen3-asr", policy: "idle" }] } };
  assert.equal(
    scheduleText(i18n.getFixedT("zh", "deploy") as never, reason),
    "gpu3 的 GPU 0 为休眠中的 qwen3-asr（闲置自动出让）保留，不自动出让的部署不会放到这些卡上；请换一张卡，为本部署开启休眠并选能共卡的出让策略，或手动休眠占卡的部署。",
  );
  assert.match(scheduleText(i18n.getFixedT("en", "deploy") as never, reason), /^GPU 0 on gpu3 is kept for qwen3-asr \(Yield when idle\), asleep there/);
});

// Yield policy keeps a draft off cards awake deployments hold: the reason
// names the node, the cards, each holder with its policy, and what to change.
const heldCases: Array<{ name: string; reason: { code: string; params: Record<string, unknown> }; zh: string; en: string }> = [
  {
    name: "a never draft on a sharer's card",
    reason: { code: "does_not_wait_for_yield", params: { node: "gpu3", gpus: [0], holders: [{ deployment: "qwen3-asr", policy: "idle" }], blockers: [] } },
    zh: "gpu3 的 GPU 0 被 qwen3-asr（闲置自动出让）占用；本部署不自动出让，不会等占卡的部署让卡。为本部署开启休眠并选择能共卡的出让策略，即可等 qwen3-asr 按出让策略让卡。",
    en: "GPU 0 on gpu3 is held by qwen3-asr (Yield when idle); this deployment never yields, so it does not wait for them to give the cards up. Enable sleep for this deployment and pick a yield policy that shares cards, and it waits for qwen3-asr to give them up by yield policy.",
  },
  {
    name: "a sharing draft on a never holder's card",
    reason: { code: "held_by_non_yielding", params: { node: "gpu3", gpus: [0], holders: [], blockers: [{ deployment: "batch", policy: "never" }] } },
    zh: "gpu3 的 GPU 0 被 batch（不自动出让）占用；batch 不自动出让，不会把卡让给本部署。可以换一张卡，或为 batch 开启休眠并选择能共卡的出让策略。",
    en: "GPU 0 on gpu3 is held by batch (Never yield automatically); batch never yields and does not give its cards to this deployment. Choose another card, or enable sleep for batch and pick a yield policy that shares cards.",
  },
  {
    name: "a sharing draft over a sharer and a never holder",
    reason: {
      code: "held_by_non_yielding",
      params: {
        node: "gpu3", gpus: [0, 1],
        holders: [{ deployment: "shared", policy: "timeslice", at_once: false, idle_sec: 30, max_serve_sec: 120 }],
        blockers: [{ deployment: "batch", policy: "never" }],
      },
    },
    zh: "gpu3 的 GPU 0, 1 被 shared（按时自动出让）、batch（不自动出让）占用；batch 不自动出让，不会把卡让给本部署。可以换一张卡，或为 batch 开启休眠并选择能共卡的出让策略。 其余占卡的部署会让卡：shared 闲置 30 秒或本轮服务满 2 分钟后让出 GPU。",
    en: "GPU 0, 1 on gpu3 is held by shared (Yield on a turn), batch (Never yield automatically); batch never yields and does not give its cards to this deployment. Choose another card, or enable sleep for batch and pick a yield policy that shares cards. The other holders give way: shared gives up its GPUs after 30 s idle or 2 min of serving.",
  },
  {
    name: "a never draft over a sharer and a never holder",
    reason: {
      code: "does_not_wait_for_yield",
      params: { node: "gpu3", gpus: [0, 1], holders: [{ deployment: "shared", policy: "timeslice" }], blockers: [{ deployment: "batch", policy: "never" }] },
    },
    zh: "gpu3 的 GPU 0, 1 被 shared（按时自动出让）、batch（不自动出让）占用；本部署不自动出让，不会等占卡的部署让卡。为本部署开启休眠并选择能共卡的出让策略，即可等 shared 按出让策略让卡。 batch 不自动出让，不会让卡：还要换一张卡，或为 batch 开启休眠并选择能共卡的出让策略。",
    en: "GPU 0, 1 on gpu3 is held by shared (Yield on a turn), batch (Never yield automatically); this deployment never yields, so it does not wait for them to give the cards up. Enable sleep for this deployment and pick a yield policy that shares cards, and it waits for shared to give them up by yield policy. batch never yields and keeps its cards: also choose another card, or enable sleep for batch and pick a yield policy that shares cards.",
  },
];

for (const { name, reason, zh, en } of heldCases) {
  test(`${name} names the cards, each holder with its policy and what to change`, () => {
    const zhText = placementRefusalText(i18n.getFixedT("zh", "deploy") as never, reason);
    const enText = placementRefusalText(i18n.getFixedT("en", "deploy") as never, reason);
    assert.equal(zhText, zh);
    assert.equal(enText, en);
    // The evaluation's wording is the scheduler's own.
    assert.equal(scheduleText(i18n.getFixedT("zh", "deploy") as never, reason), zh);
    assert.doesNotMatch(zhText + enText, /\{\{|undefined/);
  });
}

test("hardware refusals share feature translations across API and placement", () => {
 for (const lng of ["en", "zh"]) {
  for (const feature of ["pipeline_parallelism", "data_parallelism", "partition", "future_feature"]) {
   const name = i18n.getFixedT(lng, "common")(`acceleratorFeature.names.${feature}`, { defaultValue: feature });
   const params = { vendor: "AMD", feature };
   const placement = scheduleText(i18n.getFixedT(lng, "deploy"), { code: "accelerator_feature_unsupported", params });
   const api = apiErrorText(i18n.getFixedT(lng, "common"), new APIError("ACCELERATOR_FEATURE_UNSUPPORTED", params, "fallback", 400));
   assert.equal(placement.includes(name), true);
   assert.equal(api.includes(name), true);
   assert.equal(placement.includes("{{"), false);
   assert.equal(api.includes("{{"), false);
  }
 }
});
