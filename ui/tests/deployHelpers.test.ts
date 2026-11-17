import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";

import type { EvaluateResponse } from "../src/api.ts";

import {
  applyEvaluateFormValues,
  buildPayload,
  deployDraftPayload,
  deploymentPartitionDraft,
  findTemplateSpec,
  quickParamSpecs,
  visibleParamSpecs,
} from "../src/pages/deploy/deployHelpers.ts";
import { formatVRAMFitSummary } from "../src/pages/deploy/vramDisplay.ts";
import {
  mountRowsValidationError,
  reservedModelMountTargets,
} from "../src/mountValidation.ts";

test("evaluate and create projection carries node requirements, replicas, and explicit launch settings", () => {
  const { payload } = buildPayload({
    model_id: 7,
    node_uuid: "node-a",
    engine: "vllm",
    image: "private.local/serve:1",
    replicas: 3,
    nnodes: 1,
    node_labels: { pool: "inference", zone: "east" },
    vram_limit_gib: 16,
    partition_backend: "hami-core",
    command_mode: "manual",
    container_cmd: "python serve.py",
    container_env_rows: [{ key: "READY", value: "1" }],
    container_mount_rows: [{ host_path: "/srv/data", container_path: "/data" }],
  });
  assert.equal(payload.replicas, 3);
  assert.equal(payload.placement, undefined);
  assert.deepEqual(payload.node_requirements, {
    pool: ["inference"],
    zone: ["east"],
  });
  assert.equal(payload.image, "private.local/serve:1");
  assert.equal(payload.vram_limit_gib, 16);
  assert.deepEqual(payload.container_overrides, {
    env: { READY: "1" },
    extra_mounts: [
      { host_path: "/srv/data", container_path: "/data", read_only: false },
    ],
    cmd_display: "python serve.py",
    command_mode: "manual",
  });
});

test("automatic placement sends the typed TP and PP to evaluate but never a GPU pin", () => {
  const draft = { model_id: 7, node_selection_mode: "auto", node_uuid: "stale", tp: 2, pp: 2, gpu_indexes: [0, 1] };
  const automatic = deployDraftPayload(draft, []) as Record<string, unknown>;
  assert.equal(automatic.placement_mode, "auto");
  assert.equal("node_uuid" in automatic, false);
  assert.deepEqual(automatic.placement, { tp: 2, pp: 2 });
  // TP left to the scheduler: only PP travels, and nothing at all when both are unset.
  assert.deepEqual((deployDraftPayload({ ...draft, tp: undefined }, []) as Record<string, unknown>).placement, { pp: 2 });
  assert.equal("placement" in (deployDraftPayload({ model_id: 7, node_selection_mode: "auto" }, []) as Record<string, unknown>), false);
});

test("automatic partition draft is independent of a stale selected node capability", () => {
  const intent = {
    vram_limit_gib: 16,
    partition_backend: "hami-core",
    node_uuid: "old-node",
  };
  const unavailableNode = {
    uuid: "old-node",
    partition_capabilities: [
      {
        backend: "hami-core",
        display_name: "HAMi-core",
        vendor: "nvidia",
        maturity: "stable",
        status: "unavailable",
        reason: "probe failed",
      },
    ],
  } as never;
  assert.deepEqual(
    deploymentPartitionDraft(intent, unavailableNode, true),
    intent,
  );
  assert.deepEqual(
    deploymentPartitionDraft({ vram_limit_gib: 12 }, undefined, true),
    { vram_limit_gib: 12 },
  );
});

test("manual partition draft fills only a backend explicitly ready on the selected node", () => {
  const readyNode = {
    uuid: "ready-node",
    partition_capabilities: [
      {
        backend: "hami-core",
        display_name: "HAMi-core",
        vendor: "nvidia",
        maturity: "stable",
        status: "ready",
      },
    ],
  } as never;
  assert.deepEqual(
    deploymentPartitionDraft({ vram_limit_gib: 16 }, readyNode, false),
    { vram_limit_gib: 16, partition_backend: "hami-core" },
  );
  assert.deepEqual(
    deploymentPartitionDraft({ vram_limit_gib: 16 }, undefined, false),
    { vram_limit_gib: 16 },
  );
});

test("target vendor reaches evaluation without a selected node", () => {
  const { payload } = buildPayload({
    model_id: 1,
    target_vendor: "ascend",
    engine: "vllm",
  });
  assert.equal(payload.target_vendor, "ascend");
  assert.equal(payload.node_uuid, undefined);
});

test("flat spec identity reaches evaluate and deploy payloads without legacy options", () => {
  const { payload } = buildPayload({
    template_id: 7,
    template_spec_source: "user",
    template_spec_name: "w8a8",
    engine: "sglang",
  });
  assert.equal(payload.template_spec_source, "user");
  assert.equal(payload.template_spec_name, "w8a8");
  assert.equal(payload.extra_args, undefined);
});

test("same spec name is selected by catalog source", () => {
  const template = {
    specs: [
      { source: "upstream", name: "general", scenario: "balanced" },
      { source: "user", name: "general", scenario: "balanced" },
    ],
  } as never;
  assert.equal(
    findTemplateSpec(template, "general", undefined, "upstream")?.spec.source,
    "upstream",
  );
  assert.equal(
    findTemplateSpec(template, "general", undefined, "user")?.spec.source,
    "user",
  );
});

test("single-node inference-template topology stays explicit in the deployment payload", () => {
  const { payload, placement } = buildPayload({
    engine: "vllm",
    tp: 1,
    nnodes: 1,
  });
  assert.equal(payload.nnodes, 1);
  assert.deepEqual(placement, { tp: 1 });
});

test("empty container rows produce no overrides", () => {
  const { payload } = buildPayload({
    engine: "vllm",
    command_mode: "engine",
    container_env_rows: [{ key: "", value: "ignored" }],
    container_mount_rows: [{ host_path: "", container_path: "" }],
    container_docker_args: ["", "  "],
    container_entrypoint: "  ",
    container_cmd: "",
  });
  assert.equal(payload.container_overrides, undefined);
});

test("default engine command mode is not an inference-template override when no image baseline is present", () => {
  const { payload } = buildPayload({ engine: "vllm", command_mode: "engine" });
  assert.equal(payload.container_overrides, undefined);
});

test("KV offload and long-prefill controls reach quick_params", () => {
  const profile = {
    param_specs: [
      {
        key: "kv_offloading_size",
        flag: "--kv-offloading-size",
        label: "offload",
        type: "number",
      },
      {
        key: "long_prefill_token_threshold",
        flag: "--long-prefill-token-threshold",
        label: "prefill",
        type: "integer",
      },
    ],
  } as never;
  const { payload } = buildPayload(
    {
      model_id: 1,
      node_uuid: "node-1",
      engine: "vllm",
      kv_offloading_size: 8,
      long_prefill_token_threshold: 1024,
    },
    profile,
  );
  assert.deepEqual(payload.quick_params, {
    kv_offloading_size: 8,
    long_prefill_token_threshold: 1024,
  });
});

test("automatic batch fields stay absent from the deployment payload", () => {
  const profile = {
    param_specs: [
      {
        key: "performance_mode",
        flag: "--performance-mode",
        label: "mode",
        type: "enum",
        default: "balanced",
      },
      {
        key: "max_num_batched_tokens",
        flag: "--max-num-batched-tokens",
        label: "tokens",
        type: "integer",
      },
      {
        key: "max_num_seqs",
        flag: "--max-num-seqs",
        label: "sequences",
        type: "integer",
      },
    ],
  } as never;
  const { payload } = buildPayload(
    {
      engine: "vllm",
      performance_mode: "throughput",
      max_num_batched_tokens: undefined,
      max_num_seqs: "",
    },
    profile,
  );
  assert.deepEqual(payload.quick_params, { performance_mode: "throughput" });
  // A value that happens to equal the dictionary default is still the value on
  // screen, and no key is special-cased on its way out: the server
  // materializes defaults and compares templates on the materialized
  // configuration, so sending it changes nothing (ADR 0020 decision 1).
  assert.deepEqual(
    buildPayload({ engine: "vllm", performance_mode: "balanced" }, profile)
      .payload.quick_params,
    { performance_mode: "balanced" },
  );
});

test("only explicitly chosen switch positions reach the request", () => {
  // The server states dictionary defaults at materialization, so the draft
  // sends only what the deployer touched and every explicit position travels.
  const profile = {
    param_specs: [
      {
        key: "enable_prefix_caching",
        flag: "--enable-prefix-caching",
        flag_off: "--no-enable-prefix-caching",
        label: "prefix",
        type: "boolean",
        default: true,
      },
      {
        key: "trust_remote_code",
        flag: "--trust-remote-code",
        label: "trust",
        type: "boolean",
        default: false,
      },
      {
        key: "enable_chunked_prefill",
        flag: "--enable-chunked-prefill",
        label: "chunked",
        type: "boolean",
        default: true,
      },
    ],
  } as never;
  assert.deepEqual(
    buildPayload({ engine: "vllm", enable_prefix_caching: false }, profile)
      .payload.quick_params,
    { enable_prefix_caching: false },
  );
  assert.deepEqual(
    buildPayload({ engine: "vllm", trust_remote_code: true }, profile).payload
      .quick_params,
    { trust_remote_code: true },
  );
  assert.deepEqual(
    buildPayload({ engine: "vllm", enable_chunked_prefill: true }, profile)
      .payload.quick_params,
    { enable_chunked_prefill: true },
  );
  assert.deepEqual(
    buildPayload({ engine: "vllm" }, profile).payload.quick_params,
    undefined,
    "an untouched draft sends nothing: the server owns defaults",
  );
  // A key outside the dictionary the server sent is not submittable at all.
  assert.deepEqual(
    buildPayload({ engine: "vllm", speculative_tokens: 4 }, profile).payload
      .quick_params,
    undefined,
  );
});

// The dictionary names dependencies explicitly, and the payload projection
// reads them: a companion is a depends_on row of the disk tier.
const diskTierKeys = [
  "kv_disk_offload",
  "shared_kv_dir",
  "kv_offload_read_threads",
  "kv_offload_write_threads",
  "kv_disk_limit_gib",
];
const performanceProfile = {
  param_specs: [
    "enable_prefix_caching",
    "gpu_memory_utilization",
    "kv_offloading_size",
    ...diskTierKeys,
    "speculative_algorithm",
    "speculative_draft_model",
    "speculative_tokens",
    "optimization_level",
    "async_scheduling",
    "reasoning_parser",
    "tool_call_parser",
    "enable_auto_tool_choice",
  ].map((key) => ({
    key,
    flag:
      key === "kv_disk_limit_gib"
        ? ""
        : diskTierKeys.includes(key)
          ? "--kv-transfer-config"
          : `--${key}`,
    label: key,
    type:
      key === "kv_disk_offload"
        ? "boolean"
        : key === "kv_offloading_size" || key === "kv_disk_limit_gib"
          ? "number"
          : "string",
    depends_on:
      key === "kv_disk_offload"
        ? "kv_offloading_size"
        : diskTierKeys.includes(key) && key !== "kv_disk_offload"
          ? "kv_disk_offload"
          : undefined,
  })),
} as never;

test("a disk tier that is off keeps its companions off the wire", () => {
  const { payload } = buildPayload(
    {
      engine: "vllm",
      gpu_memory_utilization: 0.92,
      kv_offloading_size: 32,
      shared_kv_dir: "/cache/kv",
      kv_offload_read_threads: 8,
      kv_offload_write_threads: 12,
      kv_disk_limit_gib: 128,
    },
    performanceProfile,
  );
  assert.deepEqual(payload.quick_params, {
    gpu_memory_utilization: 0.92,
    kv_offloading_size: 32,
  });
});

test("all planned performance values map through the structured payload", () => {
  const profile = performanceProfile;
  const { payload } = buildPayload(
    {
      engine: "vllm",
      enable_prefix_caching: true,
      gpu_memory_utilization: 0.92,
      kv_offloading_size: 32,
      kv_disk_offload: true,
      shared_kv_dir: "/cache/kv",
      kv_offload_read_threads: 8,
      kv_offload_write_threads: 12,
      kv_disk_limit_gib: 128,
      speculative_algorithm: "draft_model",
      speculative_draft_model: "/draft",
      speculative_tokens: 5,
      optimization_level: "O2",
      async_scheduling: true,
      reasoning_parser: "qwen3",
      tool_call_parser: "hermes",
      enable_auto_tool_choice: true,
    },
    profile,
  );
  assert.deepEqual(payload.quick_params, {
    enable_prefix_caching: true,
    gpu_memory_utilization: 0.92,
    kv_offloading_size: 32,
    speculative_tokens: 5,
    kv_disk_offload: true,
    kv_offload_read_threads: 8,
    kv_offload_write_threads: 12,
    shared_kv_dir: "/cache/kv",
    kv_disk_limit_gib: 128,
    async_scheduling: true,
    speculative_algorithm: "draft_model",
    speculative_draft_model: "/draft",
    optimization_level: "O2",
    reasoning_parser: "qwen3",
    tool_call_parser: "hermes",
    enable_auto_tool_choice: true,
  });
});

test("disabled KV tiers preserve the draft while projecting tombstones", () => {
  const values = {
    engine: "vllm",
    kv_offloading_size: 32,
    kv_disk_offload: true,
    shared_kv_dir: "/cache/custom",
    kv_offload_read_threads: 7,
    kv_offload_write_threads: 9,
    kv_disk_limit_gib: 144,
    kv_memory_tier_enabled: false,
  };
  const memoryOff = buildPayload(values, performanceProfile);
  assert.deepEqual(memoryOff.quick, {});
  assert.deepEqual(memoryOff.quickParamsUnset, [
    "kv_offloading_size",
    "kv_disk_offload",
    "shared_kv_dir",
    "kv_offload_read_threads",
    "kv_offload_write_threads",
    "kv_disk_limit_gib",
  ]);
  assert.deepEqual(
    memoryOff.payload.quick_params_unset,
    memoryOff.quickParamsUnset,
  );
  assert.equal(values.kv_offloading_size, 32);
  assert.equal(values.shared_kv_dir, "/cache/custom");

  // The disk tier's switch is the parameter itself: off states false and the
  // companions ride along as tombstones.
  const diskOff = buildPayload(
    { ...values, kv_memory_tier_enabled: true, kv_disk_offload: false },
    performanceProfile,
  );
  assert.deepEqual(diskOff.quick, { kv_offloading_size: 32 });
  assert.deepEqual(diskOff.quickParamsUnset, [
    "kv_disk_offload",
    "shared_kv_dir",
    "kv_offload_read_threads",
    "kv_offload_write_threads",
    "kv_disk_limit_gib",
  ]);

  const restored = buildPayload(
    { ...values, kv_memory_tier_enabled: true, kv_disk_offload: true },
    performanceProfile,
  );
  assert.deepEqual(restored.quick, {
    kv_offloading_size: 32,
    kv_disk_offload: true,
    shared_kv_dir: "/cache/custom",
    kv_offload_read_threads: 7,
    kv_offload_write_threads: 9,
    kv_disk_limit_gib: 144,
  });
  assert.deepEqual(restored.quickParamsUnset, []);
});

test("engine profile filters stale unsupported form values", () => {
  const sglang = {
    param_specs: [
      {
        key: "gpu_memory_utilization",
        flag: "--mem-fraction-static",
        label: "memory",
        type: "number",
      },
      {
        key: "enable_prefix_caching",
        flag: "--disable-radix-cache",
        label: "prefix",
        type: "boolean",
      },
    ],
  } as never;
  const { payload } = buildPayload(
    {
      engine: "sglang",
      performance_mode: "throughput",
      kv_offloading_size: 8,
      optimization_level: "O3",
      gpu_memory_utilization: 0.92,
      enable_prefix_caching: true,
    },
    sglang,
  );
  assert.deepEqual(payload.quick_params, {
    enable_prefix_caching: true,
    gpu_memory_utilization: 0.92,
  });
});

test("parser controls keep custom values and send an explicit template disable", () => {
  const profile = {
    param_specs: [
      {
        key: "reasoning_parser",
        flag: "--reasoning-parser",
        type: "string",
        control: "combobox",
      },
      {
        key: "tool_call_parser",
        flag: "--tool-call-parser",
        type: "string",
        control: "combobox",
      },
    ],
  } as never;
  assert.deepEqual(
    buildPayload(
      {
        engine: "vllm",
        reasoning_parser: "custom_reasoner",
        tool_call_parser: "custom_tools",
      },
      profile,
    ).payload.quick_params,
    {
      reasoning_parser: "custom_reasoner",
      tool_call_parser: "custom_tools",
    },
  );
  // A cleared control states nothing and tombstones the key: the template's
  // choice cannot come back through a value the deployer removed.
  const cleared = buildPayload(
    { engine: "vllm", reasoning_parser: "" },
    profile,
  );
  assert.deepEqual(cleared.payload.quick_params, undefined);
  assert.deepEqual(cleared.quickParamsUnset, ["reasoning_parser"]);
});

test("cache hit stats switch submits explicit values and consumes template flags", () => {
  const profile = {
    param_specs: [
      {
        key: "enable_cache_report",
        flag: "--enable-prompt-tokens-details",
        label: "cache",
        type: "boolean",
        default: true,
      },
    ],
  } as never;
  assert.deepEqual(
    buildPayload({ engine: "vllm", enable_cache_report: false }, profile)
      .payload.quick_params,
    { enable_cache_report: false },
    "an explicit off must reach the payload so the engine flag stays off",
  );
  assert.deepEqual(
    buildPayload({ engine: "vllm", enable_cache_report: true }, profile).payload
      .quick_params,
    { enable_cache_report: true },
  );
  assert.deepEqual(
    buildPayload({ engine: "vllm", enable_cache_report: undefined }, profile)
      .payload.quick_params,
    undefined,
    "the untouched ON position is the server's default to state, not the draft's",
  );
});

test("multimodal and engine-specific parameters are shown only when supported", () => {
  // The dictionary decides which parameters are multimodal-only: the server
  // sends multimodal_only on exactly those keys (deploy/param_specs.go), and
  // nothing here recognises them by name.
  const profile = {
    param_specs: [
      {
        key: "max_num_seqs",
        flag: "--max-num-seqs",
        label: "batch",
        type: "integer",
        engines: ["vllm"],
      },
      {
        key: "limit_mm_per_prompt",
        flag: "--limit-mm-per-prompt",
        label: "media",
        type: "json",
        group: "multimodal",
        engines: ["vllm"],
        multimodal_only: true,
      },
      {
        key: "kv_offloading_size",
        flag: "--kv-offloading-size",
        label: "offload",
        type: "number",
        engines: ["vllm"],
      },
    ],
  } as never;
  assert.deepEqual(
    visibleParamSpecs(profile, "vllm", ["text"]).map((spec) => spec.key),
    ["max_num_seqs", "kv_offloading_size"],
  );
  assert.deepEqual(
    visibleParamSpecs(profile, "vllm", ["text", "image"]).map(
      (spec) => spec.key,
    ),
    ["max_num_seqs", "limit_mm_per_prompt", "kv_offloading_size"],
  );
  assert.deepEqual(visibleParamSpecs(profile, "sglang", ["text", "image"]), []);
  // A key the server does not mark stays visible for a text-only model however
  // multimodal its name reads, and a marked one is hidden whatever it is
  // called: the flag is the only input.
  const renamed = {
    param_specs: [
      {
        key: "mm_processor_kwargs",
        flag: "--mm-processor-kwargs",
        label: "processor",
        type: "json",
      },
      {
        key: "rope_scaling",
        flag: "--rope-scaling",
        label: "rope",
        type: "json",
        multimodal_only: true,
      },
    ],
  } as never;
  assert.deepEqual(
    visibleParamSpecs(renamed, "vllm", ["text"]).map((spec) => spec.key),
    ["mm_processor_kwargs"],
  );
  assert.deepEqual(
    visibleParamSpecs(renamed, "vllm", ["text", "audio"]).map(
      (spec) => spec.key,
    ),
    ["mm_processor_kwargs", "rope_scaling"],
  );
});

test("quick parameter view uses template keys and falls back to the engine whitelist", () => {
  const profile = {
    quick_form_keys: ["max_model_len", "gpu_memory_utilization"],
    param_specs: [
      {
        key: "max_model_len",
        flag: "--max-model-len",
        label: "context",
        type: "integer",
        engines: ["vllm"],
      },
      {
        key: "gpu_memory_utilization",
        flag: "--gpu-memory-utilization",
        label: "memory",
        type: "number",
        engines: ["vllm"],
      },
      {
        key: "limit_mm_per_prompt",
        flag: "--limit-mm-per-prompt",
        label: "media",
        type: "json",
        engines: ["vllm"],
        multimodal_only: true,
      },
    ],
  } as never;
  assert.deepEqual(
    quickParamSpecs(profile, "vllm", ["text"]).map((spec) => spec.key),
    ["max_model_len", "gpu_memory_utilization"],
  );
  assert.deepEqual(
    quickParamSpecs(
      profile,
      "vllm",
      ["text", "image"],
      ["limit_mm_per_prompt", "max_model_len"],
    ).map((spec) => spec.key),
    ["limit_mm_per_prompt", "max_model_len"],
  );
  assert.deepEqual(
    quickParamSpecs(
      profile,
      "vllm",
      ["text"],
      ["limit_mm_per_prompt", "max_model_len"],
    ).map((spec) => spec.key),
    ["max_model_len"],
  );
});

test("untouched engine display defaults are not submitted as user overrides", () => {
  const profile = {
    param_specs: [
      {
        key: "gpu_memory_utilization",
        flag: "--gpu-memory-utilization",
        label: "memory",
        type: "number",
        default: 0.92,
      },
      {
        key: "dtype",
        flag: "--dtype",
        label: "dtype",
        type: "enum",
        default: "auto",
      },
    ],
  } as never;
  assert.equal(
    buildPayload({ engine: "vllm" }, profile).payload.quick_params,
    undefined,
  );
});

// What the form states is the whole override layer: the image contributes no
// baseline to subtract (ADR 0031), so every stated row travels as written.
test("container overrides carry every stated row", () => {
  const { payload } = buildPayload({
    engine: "vllm",
    command_mode: "manual",
    container_env_rows: [{ key: "CHANGED", value: "2" }],
    container_mount_rows: [
      { container_path: "/base", host_path: "/base" },
      { container_path: "/extra", volume: "extra" },
    ],
    container_docker_args: ["--ipc=host", "--cap-add=SYS_NICE"],
    container_model_mount_path: "/models",
    container_entrypoint: '["/entry"]',
    container_cmd: "vllm serve /models",
    command_includes_preparation: true,
  });
  assert.deepEqual(payload.container_overrides, {
    model_mount_path: "/models",
    env: { CHANGED: "2" },
    extra_mounts: [
      { container_path: "/base", host_path: "/base", read_only: false },
      { container_path: "/extra", volume: "extra", read_only: false },
    ],
    docker_args: ["--ipc=host", "--cap-add=SYS_NICE"],
    entrypoint_display: '["/entry"]',
    cmd_display: "vllm serve /models",
    command_includes_preparation: true,
    command_mode: "manual",
  });
});

// A mount row may name a registered model. It travels as a numeric model_id,
// always read-only and without a host path or volume; a model row whose model
// is not picked yet is still being filled in and is not sent.
test("a model mount row travels as model_id only", () => {
  const { payload } = buildPayload({
    engine: "vllm",
    container_mount_rows: [
      { model_id: 42, host_path: "", container_path: "/draft", purpose: "models", read_only: false },
      { model_id: null, container_path: "/pending", purpose: "models" },
      { host_path: "/srv/data", container_path: "/data", model_id: undefined },
    ],
  });
  assert.deepEqual(payload.container_overrides, {
    extra_mounts: [
      { model_id: 42, container_path: "/draft", purpose: "models", read_only: true },
      { host_path: "/srv/data", container_path: "/data", read_only: false },
    ],
  });
});

test("VRAM summary uses backend fit and capacity without recomputing segments", () => {
  const summary = formatVRAMFitSummary({
    per_gpu_bytes: 12.7 * 1024 ** 3,
    capacity_bytes: 10.8 * 1024 ** 3,
    fits: false,
  } as never);
  assert.equal(summary.tone, "danger");
  assert.equal(
    summary.text,
    "Insufficient VRAM for startup: about 12.7 GiB required, exceeding the 10.8 GiB available limit.",
  );
});

test("VRAM summary names a context that does not fit when startup memory does", () => {
  const summary = formatVRAMFitSummary({
    per_gpu_bytes: 6 * 1024 ** 3,
    capacity_bytes: 21.6 * 1024 ** 3,
    fits: false,
    max_model_len: 262144,
    max_concurrency: 0,
  } as never);
  assert.equal(summary.tone, "danger");
  assert.match(summary.text, /maximum context/);
  assert.doesNotMatch(summary.text, /Insufficient VRAM for startup/);
});

test("the front end no longer judges performance rules of its own", async () => {
  // ADR 0020: R1-R6 and W1-W10 were a copy of internal/server/deploy/params.go.
  // The evaluation response is the only source of those errors and warnings
  // now, so the helper module must not export a second judge.
  const helpers =
    (await import("../src/pages/deploy/deployHelpers.ts")) as Record<
      string,
      unknown
    >;
  assert.equal(helpers.performanceValidation, undefined);
  const source = await import("node:fs").then((fs) =>
    fs.readFileSync(
      new URL("../src/pages/deploy/deployHelpers.ts", import.meta.url),
      "utf8",
    ),
  );
  for (const code of ["R1", "R2", "R3", "R4", "R5", "R6", "W1", "W5", "W10"]) {
    assert.equal(
      source.includes(`"${code}"`),
      false,
      `${code} must come from the server, not from this module`,
    );
  }
});

test("the display instruction clears stale keys and preserves disabled tier drafts", () => {
  const read = () => values;
  let values: Record<string, unknown> = {
    engine: "vllm",
    max_model_len: 8192,
    kv_cache_dtype: "fp8",
    kv_offloading_size: 32,
    kv_memory_tier_enabled: false,
  };
  const form = {
    setFieldsValue: (updates: Record<string, unknown>) => {
      values = { ...values, ...updates };
    },
    getFieldsValue: () => values,
  };
  // The next response stops carrying the precision the previous one applied,
  // so the field is cleared rather than left behind on the new draft. The
  // instruction rides on the captured template evaluation, so its shape is
  // the server's own.
  const templateFixture = JSON.parse(
    fs.readFileSync(
      new URL("./fixtures/evaluate/template_vllm.json", import.meta.url),
      "utf8",
    ),
  ) as EvaluateResponse;
  const instruction: Record<string, unknown> = {
    ...templateFixture.form_values,
    max_model_len: 4096,
    extra_args: "--foo",
  };
  // The next spec says nothing about precision: the key is absent, not empty.
  delete instruction.kv_cache_dtype;
  applyEvaluateFormValues(
    form,
    { ...templateFixture, form_values: instruction },
    ["max_model_len", "kv_cache_dtype", "kv_offloading_size"],
  );
  assert.equal(read().kv_cache_dtype, undefined);
  assert.equal(read().max_model_len, 4096);
  assert.equal(read().extra_args, "--foo");
  // A disabled memory tier keeps its preserved size: the instruction cannot
  // switch the tier back on by writing the value.
  assert.equal(read().kv_offloading_size, 32);
  applyEvaluateFormValues(
    form,
    { ...templateFixture, form_values: { kv_offloading_size: 64 } },
    [],
  );
  assert.equal(
    read().kv_offloading_size,
    32,
    "the tier card position governs the key, not the response",
  );
  assert.deepEqual(applyEvaluateFormValues(form, null, []), [], "no answer defers no field");
});

test("the display instruction skips the controls being typed in and returns them", () => {
  const values: Record<string, unknown> = { cpu_offload_gb: null, gpu_memory_utilization: 0.8, dtype: "auto" };
  const form = {
    setFieldsValue: (next: Record<string, unknown>) => Object.assign(values, next),
    getFieldsValue: () => ({ ...values }),
  };
  const deferred = applyEvaluateFormValues(
    form,
    { form_values: { cpu_offload_gb: 4, gpu_memory_utilization: 0.9 } } as never,
    ["dtype"],
    (key) => key === "cpu_offload_gb" || key === "dtype",
  );
  assert.deepEqual(deferred.sort(), ["cpu_offload_gb", "dtype"]);
  assert.equal(values.cpu_offload_gb, null, "a focused control keeps what the deployer left in it");
  assert.equal(values.dtype, "auto", "a stale key is not cleared under the deployer's cursor either");
  assert.equal(values.gpu_memory_utilization, 0.9, "other fields still take the answer");
});

test("a shared memory size is sent in bytes and an empty or zero size is not sent", () => {
  assert.deepEqual(
    buildPayload({ engine: "vllm", container_shm_gib: 0.5 }).payload
      .container_overrides,
    { shm_bytes: 512 * 2 ** 20 },
  );
  for (const value of [undefined, null, "", 0]) {
    assert.equal(
      buildPayload({ engine: "vllm", container_shm_gib: value }).payload
        .container_overrides,
      undefined,
      `container_shm_gib=${String(value)}`,
    );
  }
});

test("removed variables travel as env_unset, trimmed and de-duplicated", () => {
  assert.deepEqual(
    buildPayload({
      engine: "vllm",
      container_env_unset: [" HF_HUB_OFFLINE ", "HF_HUB_OFFLINE", "", "TRANSFORMERS_OFFLINE"],
      container_env_rows: [{ key: "HF_ENDPOINT", value: "https://hf-mirror.com" }],
    }).payload.container_overrides,
    {
      env: { HF_ENDPOINT: "https://hf-mirror.com" },
      env_unset: ["HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"],
    },
  );
});

test("the yield policy chosen is stated, with its parameters, whenever it left never: the server judges whether the draft may hold it", () => {
  const base = { model_id: 7, engine: "vllm", sleepable: true, nnodes: 1 };
  const policy = (values: Record<string, unknown>) => (buildPayload({ ...base, ...values }).payload as Record<string, unknown>).yield_policy;
  const idle = { policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 };
  const timeslice = { policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600 };
  assert.deepEqual(policy({ yield_policy: idle }), idle);
  assert.deepEqual(policy({ yield_policy: timeslice }), timeslice);
  assert.deepEqual(policy({ yield_policy: { policy: "anytime", unfinished: "abort" } }), { policy: "anytime", unfinished: "abort" });
  assert.equal(policy({ yield_policy: { policy: "never" } }), undefined, "never is what an absent policy means");
  assert.equal(policy({}), undefined);
  // Only the known parameters go out.
  assert.deepEqual(policy({ yield_policy: { ...idle, auto_sleep: true, preset: "online", min_serve_sec: 3 } }), idle);
  // A policy left in the form after the draft became one the server refuses is still sent: the create refuses it with the reason the form shows.
  assert.deepEqual(policy({ yield_policy: idle, sleepable: false }), idle);
  assert.deepEqual(policy({ yield_policy: idle, nnodes: 2 }), idle);
  assert.deepEqual(policy({ yield_policy: idle, node_selection_mode: "pd", pd_mode: "pd_disagg" }), idle);
  // The old preset form is no longer a request field.
  assert.equal(policy({ yield_preset: "online" }), undefined);
  assert.equal((buildPayload({ ...base, yield_policy: idle }).payload as Record<string, unknown>).service_level, undefined);
});
