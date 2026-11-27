import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";
import { productMessageText } from "../src/apiError.ts";
import { runtimeFailureText } from "../src/runtimeFailure.ts";
import { instanceEventText } from "../src/pages/instanceEvents.ts";

for (const language of ["en", "zh"]) {
 test(`runtime failures localize stable facts in ${language} and retain unknown fallbacks`, async () => {
  const common = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/common.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({lng:language, resources:{[language]:{common}},defaultNS:"common",interpolation:{escapeValue:false}});
  const t=i18n.getFixedT(language,"common");
  for (const code of ["ACCELERATOR_DEVICE_SELECTION_REQUIRED","ACCELERATOR_RUNTIME_UNSUPPORTED","ACCELERATOR_RUNTIME_UNREPORTED","ACCELERATOR_DEVICE_UNAVAILABLE","ACCELERATOR_INVENTORY_UNAVAILABLE","ACCELERATOR_RESOURCE_UNAVAILABLE"]) {
   const text=productMessageText(t,{code,params:{vendor:"Kunlunxin",os:"linux",arch:"arm64",runtime:"host",required_os:"linux",required_arch:"amd64",required_runtime:"docker",resource:"memlock"},message:"old English diagnostic"});
   assert.notEqual(text,"old English diagnostic");
   assert.doesNotMatch(text,/ACCELERATOR_|\{\{/);
   assert.match(text,/Kunlunxin/);
   if(code==="ACCELERATOR_RESOURCE_UNAVAILABLE")assert.match(text,/memlock/);
   if(code==="ACCELERATOR_RUNTIME_UNSUPPORTED")assert.match(text,/arm64/);
  }
  assert.equal(productMessageText(t,{code:"FUTURE_DRIVER_ERROR",message:"future diagnostic"}),"future diagnostic");
 });
}

const instanceFailureCodes = [
  "ACCELERATOR_FEATURE_UNSUPPORTED", "ACCELERATOR_SM_LIMIT_UNSUPPORTED", "ACCELERATOR_PARTITION_CONTRACT_INVALID", "LAUNCH_FEATURES_UNKNOWN",
  "LAUNCH_FEATURES_CONTRADICTORY", "ACCELERATOR_INJECTION_UNAVAILABLE", "ACCELERATOR_CARD_MISSING", "DISTRIBUTED_NETWORK_UNAVAILABLE",
  "MODEL_PATH_UNAVAILABLE", "MODEL_PATH_MISSING", "ENGINE_PORT_REFUSED", "ENGINE_PORT_IN_USE", "IMAGE_PULL_FAILED", "CONTAINER_RUNTIME_UNAVAILABLE",
  "RUNTIME_ASSET_PREPARE_FAILED", "PRE_START_PREPARE_FAILED", "KV_DISK_PREPARE_FAILED", "MANAGED_DIR_PREPARE_FAILED", "MOUNT_VOLUME_UNSUPPORTED", "MOUNT_VOLUME_INVALID", "DGPU_PARTITION_FAILED", "CONTAINER_RUN_FAILED",
  "CONTAINER_DISAPPEARED", "CONTAINER_EXITED", "CONTAINER_OOM_KILLED", "CONTAINER_UNHEALTHY", "ENGINE_HEALTH_ENDPOINT_INVALID",
  "ENGINE_HEALTH_TIMEOUT", "ENGINE_START_STALLED", "ENGINE_START_ABORTED", "WORKER_READINESS_TIMEOUT", "ENGINE_UNRESPONSIVE", "CALLABLE_INIT_FAILED", "SLEEP_PREWARM_FAILED",
  "SLEEP_STATE_UNCERTAIN", "SERVING_PATH_UNAVAILABLE", "UNIT_RANK_GONE", "UNIT_RANK_NODE_OFFLINE", "UNIT_RANK_FAILED",
];

async function translator(language: string) {
  const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/${ns}.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({ lng: language, resources: { [language]: { common: read("common"), ai: read("ai") } }, defaultNS: "common", interpolation: { escapeValue: false } });
  return i18n.getFixedT(language, "common");
}

// The params the server sends with every instance failure (the typed fields
// of proto.RuntimeFailure), as internal/server/api/instance_runtime_failure.go
// maps them.
const wireParams = {
  vendor: "Kunlunxin", os: "", arch: "", runtime: "docker", required_os: "", required_arch: "", required_runtime: "",
  resource: "/models/qwen", feature: "engine_sleep", phase: "health_check", detail: "no such file or directory", node: "gpu-b",
  exit_code: 2, port: 30000, rank: 1,
};

for (const language of ["en", "zh"]) {
  test(`every instance runtime failure code renders in ${language} without placeholders or the fallback`, async () => {
    const t = await translator(language);
    for (const code of instanceFailureCodes) {
      const text = runtimeFailureText(t, { code, params: wireParams, message: "old English diagnostic" });
      assert.notEqual(text, "old English diagnostic", code);
      assert.doesNotMatch(text, /\{\{|[A-Z]{3,}_[A-Z_]+/, `${code}: ${text}`);
    }
  });

  test(`an exit renders its code, root-cause line and output causes in ${language}`, async () => {
    const t = await translator(language);
    const text = runtimeFailureText(t, {
      code: "CONTAINER_EXITED",
      params: {
        exit_code: 2, phase: "health_check", evidence: ["ValueError: max seq len too large"],
        causes: [{ code: "KV_CACHE_INSUFFICIENT", params: { evidence: ["x"] }, message: "kv cache insufficient" }, { code: "FUTURE_CAUSE", message: "future cause" }],
      },
      message: "container exited (code 2): ValueError: max seq len too large",
    });
    assert.match(text, /2/);
    assert.match(text, /ValueError: max seq len too large/);
    assert.match(text, language === "en" ? /Output shows: KV cache does not fit and future cause\./ : /输出显示：KV cache 放不下和future cause。/);
    // A cause this console does not know yet keeps the server's fallback.
    assert.match(text, /future cause/);
    assert.doesNotMatch(text, /\{\{/);
  });

  test(`a startup timeout shows the newest line it printed as its last output, not as an error, in ${language}`, async () => {
    const t = await translator(language);
    const evidence = ["No module named 'cutlass'", "loading", "[Gloo] Rank 0 is connected to 0 peer ranks."];
    for (const code of ["ENGINE_HEALTH_TIMEOUT", "WORKER_READINESS_TIMEOUT"]) {
      const text = runtimeFailureText(t, { code, params: { ...wireParams, evidence }, message: "health check timed out" });
      assert.match(text, language === "en" ? / Last output: \[Gloo\] Rank 0 is connected to 0 peer ranks\.$/ : /最后的输出：\[Gloo\] Rank 0 is connected to 0 peer ranks\.$/, text);
      assert.doesNotMatch(text, /Last error|最后的错误|cutlass|\{\{/, text);
      assert.equal(runtimeFailureText(t, { code, params: wireParams, message: "x" }), runtimeFailureText(t, { code, params: { ...wireParams, evidence: [] }, message: "x" }));
    }
    // A start that timed out on an error line still calls it the last error.
    for (const code of ["ENGINE_HEALTH_TIMEOUT", "WORKER_READINESS_TIMEOUT"]) {
      const named = runtimeFailureText(t, { code, params: { ...wireParams, evidence: ["RuntimeError: CUDA out of memory"] }, message: "x" });
      assert.match(named, language === "en" ? /Last error: RuntimeError: CUDA out of memory$/ : /最后的错误：RuntimeError: CUDA out of memory$/, named);
    }
    // A stalled start says what the agent saw and shows its last output.
    const stalled = runtimeFailureText(t, { code: "ENGINE_START_STALLED", params: { ...wireParams, detail: "no progress: output flat 20m0s; processes: 2 in tracing stop (worker_a, worker_b)", evidence }, message: "x" });
    assert.match(stalled, language === "en" ? /made no progress for a long time while its container kept running: no progress: output flat 20m0s; processes: 2 in tracing stop/ : /引擎启动长时间没有任何进展，容器仍在运行：no progress: output flat 20m0s/, stalled);
    assert.match(stalled, language === "en" ? / Last output: \[Gloo\]/ : /最后的输出：\[Gloo\]/, stalled);
    // An engine that gave up during its start shows the exception it raised.
    const aborted = runtimeFailureText(t, { code: "ENGINE_START_ABORTED", params: { ...wireParams, evidence: ["RuntimeError: Unsupport run graph with different stream."] }, message: "x" });
    assert.match(aborted, language === "en" ? /one of its processes failed.* Last error: RuntimeError: Unsupport run graph with different stream\.$/ : /某个进程失败.*最后的错误：RuntimeError: Unsupport run graph with different stream\.$/, aborted);
    // An exit that named no error shows the newest of its last lines, the
    // one the agent's message ends with, as output, not as an error.
    const exited = runtimeFailureText(t, { code: "CONTAINER_EXITED", params: { ...wireParams, evidence }, message: "x" });
    assert.match(exited, language === "en" ? / Last output: \[Gloo\] Rank 0 is connected to 0 peer ranks\.$/ : /最后的输出：\[Gloo\] Rank 0 is connected to 0 peer ranks\.$/, exited);
    assert.doesNotMatch(exited, /Last error|最后的错误|cutlass/, exited);
    // An exit whose evidence is its root-cause line keeps calling it an error.
    const rooted = runtimeFailureText(t, { code: "CONTAINER_EXITED", params: { ...wireParams, evidence: ["ValueError: This recipe requires TP16"] }, message: "x" });
    assert.match(rooted, language === "en" ? /Last error: ValueError: This recipe requires TP16$/ : /最后的错误：ValueError: This recipe requires TP16$/, rooted);
  });

  test(`a rank's failure on a multi-node leader names the rank in ${language}`, async () => {
    const t = await translator(language);
    const text = runtimeFailureText(t, { code: "CONTAINER_OOM_KILLED", params: { exit_code: 137, rank: 1, node: "gpu-b", oom_killed: true }, message: "x" });
    assert.match(text, /gpu-b/);
    assert.match(text, /137/);
    // A UNIT_RANK_ code names the rank itself; it is not wrapped twice.
    const gone = runtimeFailureText(t, { code: "UNIT_RANK_GONE", params: { rank: 1, node: "gpu-b" }, message: "x" });
    assert.equal(gone.split("gpu-b").length, 2, gone);
  });
}

test("an unknown future failure code keeps the server's English fallback", async () => {
  const t = await translator("zh");
  assert.equal(runtimeFailureText(t, { code: "FUTURE_FAILURE", params: {}, message: "future diagnostic" }), "future diagnostic");
});

test("a failed transition's event renders its reason from the code and older events keep their detail", async () => {
  const tCommon = await translator("en");
  const tDeployments = ((key: string, options: Record<string, unknown>) => (key === "instance.event.INSTANCE_STATUS_UPDATED" ? `reported as ${options.state}` : String(options.defaultValue ?? key))) as never;
  const coded = instanceEventText(tDeployments, tCommon as never, {
    id: 1, kind: "state", code: "INSTANCE_STATUS_UPDATED", created_at: "2026-11-14T00:00:00Z",
    params: { state: "failed", detail: "container exited (code 2) with no output", failure: { code: "CONTAINER_EXITED", params: { exit_code: 2, causes: [{ code: "ENGINE_NO_OUTPUT", message: "engine no output" }] }, message: "container exited (code 2) with no output" } },
  });
  assert.match(coded, /^reported as Failed: The container exited with code 2\./);
  assert.match(coded, /Engine exited without output/);
  const old = instanceEventText(tDeployments, tCommon as never, {
    id: 2, kind: "state", code: "INSTANCE_STATUS_UPDATED", created_at: "2026-11-14T00:00:00Z",
    params: { state: "failed", detail: "container exited (code 2) with no output" },
  });
  assert.equal(old, "reported as Failed: container exited (code 2) with no output");
});
