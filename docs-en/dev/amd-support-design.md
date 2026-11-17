# AMD support design

This document explains how AMD joins the existing discovery, planning, scheduling and runtime pipeline, including compatibility boundaries in shared interfaces. See [AMD hardware validation](amd-hardware-validation.md) for operations and [ADR 0020](../adr/0020-launch-config-single-source.md) for launch configuration ownership.

## Goals and scope

GPUWorks currently admits AMD on Linux amd64 and Docker/nerdctl/runveil for whole-device deployments, single-node or vLLM multi-node (PP and cross-node TP; RCCL uses the NCCL_* network variables). Built-in specs select gfx942 (MI300X/MI325X) and gfx1151 (AMD395) by `arch_id`. Sleep and PD (`vllm-nixl` over UCX) are implemented and validated on MI300X; data parallelism takes the same path as on NVIDIA, and memory partitioning is the `amd-dgpu` backend (libvrocm). This is not a permanent limitation of AMD hardware or ROCm. Physical unified memory does not enable Apple's memory-sharing allocation policy.

## Data flow and fact ownership

![AMD data flow and fact ownership](../assets/amd-support-data-flow.svg)

Inventory + metrics feeds API/Web to the right and card identity/capacity into planning below. Execution flows right to left along the bottom. The dashed edge supplies published probe identity for comparison with live devices during injection. Inventory health and engine Ready are different facts.

| Fact | Sole owner | Consumption |
|---|---|---|
| Vendor identity, PCI constants, platform constraints and host requirements | internal/accelerator | The table declares facts instead of selecting a second admission path by vendor. |
| PCI/UUID, DRM/render, KFD/gfx, capacity and live readings | Agent Prober | Inventory publishes identity; injection checks current devices again. DRM index is not HIP ordinal. |
| Engine option vocabulary and feature inspection | internal/engines | Inspect actual resolved argv; do not reinterpret shell text. |
| Generated command and RequestedFeatures | deploy.ResolveLaunch | Features travel in the existing ResolvedLaunch snapshot consumed by the agent. |
| Per-card eligibility, selection and resource claims | planning, scheduler, controller | Candidates and actual scheduling share a refusal factory; a node error only refuses that node. |
| Partition capability and user refusal text | partitioncap | No second FeaturePartition in accelerator. |
| Template hardware matching and preview placement | Server chipmatch / planning | Browsers display server results without duplicating regex, unsupported or specificity rules. |
| Base images and model facts | Runner matrix / seed catalog | AMD specs live in the seed catalog like every other vendor's, naming the ROCm runner the matrix publishes. |

## Discovery, caching and concurrency

AMD discovery reads sysfs/KFD without cgo or a host ROCm CLI. A PCI vendor and amdgpu binding alone do not add AMD to the census: a matching KFD compute node is required. Before automatically choosing AMD, a single bus snapshot checks for another vendor's non-VFIO card. Such a card retains its probe-failure/fallback path rather than being replaced by an AMD integrated GPU. Explicit administrator selection still uses the existing product workflow.

Healthy compute inventory requires a known architecture, unique PCI/UUID/index, KFD and render character-device mapping. Missing facts, duplicate identity, ambiguous render nodes and mismatched device numbers cannot be validated schedulable devices. Additional errors preserve earlier diagnostics; one unreadable card does not erase other readable inventory.

KFD parsing consumes only nonnegative integer fields used for identity matching and capacity decisions. Missing, malformed, duplicate or internally out-of-range consumed fields remain unusable. Other kernel metadata, such as the unsigned 64-bit `unique_id` and `debug_prop`, does not participate in admission; exceeding the signed integer range must not discard the whole card. Synthetic ABI cases cover this boundary and do not constitute new hardware validation.

Metrics reuse static topology for 15 seconds, spanning the normal ten-second collection interval. Each collection still checks live identity and capacity provenance, and reads utilization, temperature, power and memory use. Readings are discarded if identity changes during collection. Metrics cannot publish a replacement card as a new identity baseline. Launch injection always rechecks live devices, independent of the metrics cache.

A shared bounded-call helper limits outstanding sysfs operations; locks only exchange snapshots, never enclose I/O. A caller may time out, but an unfinished kernel read retains its admission slot. Uncancellable kernel I/O is an explicit limitation; retries must not create unlimited goroutines.

## Common per-card admission

All vendors use the same per-card admission path. A global model read and node-independent catalog/launch preparation occur once, only when a candidate actually participates in that evaluation/scheduling cycle. Offline, cordoned or requirement-mismatching pools do not trigger this read. Its serially owned memo stores either a pure feature snapshot or a refusal by effective hardware/configuration shape, so invalid templates are not parsed again for every node. Concrete node-local model and component paths are checked later, outside this memo. Missing files reject only that node, never the whole pool. Actual creation regenerates the real command using the same resolver. A missing global model record or invalid shared template configuration explicitly fails evaluation; card and node-file failures reject only the corresponding candidate. One serial pool scan owns the admission object and checks its global error after scanning.

Resource/quota preview may explicitly allow an image to remain unresolved. The same resolver still derives known launch facts without inventing an image or pull plan. This permission applies only to preview; real creation and previews without the permission must satisfy the image contract.

Engine adapters generate final argv. The leaf package identifies PP, DP, sleep, multi-node, PD and KV disk-tier requests; structured topology and declarations complete RequestedFeatures. This is part of the existing opaque ResolvedLaunch snapshot, not a separate database fact. The agent does not copy engine vocabulary: it validates the snapshot, host and selected-device contract, including consistency with explicit structural fields.

An unknown command has CommandKnown=false, which does not mean no advanced features were requested. Targets with an explicit restricted launch-feature list require a recognized engine entrypoint; otherwise launch_features_unknown asks for a recognized entrypoint and structured parameters. NVIDIA, Ascend and other targets without that restriction retain custom-entrypoint behavior. Apple's deny-all feature declaration does not reject ordinary inference. Legacy desired state without a snapshot retains structural-field checks without claiming to verify unknown argv. Admission does not parse preparation hooks or dynamic shell text.

Host mismatch is a typed internal result expanded to stable message parameters at the presentation boundary. Missing facts and known incompatibility remain distinct. An unknown target_vendor returns 400. AMD has no default image because the generic default is CUDA: select a verified ROCm image or template explicitly.

## Agent lifecycle and atomic injection

The agent handles adoption of running containers before resolving persisted card identities, then applies admission only when creating a new container. New rules must not mark a still-running adopted container failed without handling it, or reject empty indexes before binding reconstructs them. Validation and launch use the same captured runtime driver.

AMD injection compares published identity, live PCI/UUID, that PCI function's drm/renderD* nodes, sysfs major/minor and character-device Rdev. It injects only /dev/kfd and selected render devices, without guessing HIP ordinals. Empty or ambiguous selection refuses creation.

Every vendor's RuntimeInjection is applied to a workload copy and committed once. Injectors needing replacement declare managed environment variables, options and device paths through typed ManagedSelection. An absent declaration means additions only, still without mutating the input. Failure leaves no partial changes. Exact cleanup preserves unrelated devices such as InfiniBand. This enforces platform assignment, not a security sandbox for administrator-defined privileged containers.

## Impact on other vendors and separation

| Module | Scope | Necessity and protection |
|---|---|---|
| Per-card admission, host results and feature snapshot | Shared by all vendors | Removes parallel node-level and AMD paths; preserves ordinary NVIDIA/Ascend commands and lifecycle with baseline/candidate comparisons. |
| Atomic injection contract | All injectors | Removes an implicit mode switch; existing device/environment/argument output is preserved without modifying the caller's map. |
| AMD discovery, KFD/GTT and ROCm injection | AMD implementation | No dependency on Kunlun, NVIDIA or Ascend backends. |
| arch_id, server template selection and selected catalog row | Shared catalog contract | Preview, creation and execution use matching results and model facts consistently; architecture errors carry required and observed values. |
| Capacity-known flag and vendor labels | Shared DTO/UI | Derived memory_capacity_known is not persisted; unknown renders as a dash while measured zero usage remains visible; brands come from the common locale dictionary. |
| AMD images and launch overlays | AMD data | Runner matrix owns base images; seed catalog owns model configuration and weights. |

This integration does not change SGLang hybrid/Mamba estimates, explicit token-limit rules, shared Qwen3.8 model facts, GPU telemetry windows/UUID matching, deployment-health derivation, CI log wrappers or default fleet GPU-read settings. Those previously bundled all-vendor changes need independent issues, baseline evidence and review. Preserving upstream behavior does not imply all related upstream defects are fixed.

## Memory, image and model contracts

AMD395 UMA reports the driver-exposed pool only with PCI 0x1586, gfx1151, unique KFD/PCI/render correspondence, and matching KFD system-memory-bank/GTT totals. Missing or conflicting facts yield unknown capacity and degraded health rather than a healthy dedicated carve-out. GTT use excludes some host consumption; total minus used is not current HIP free memory. Real pressure and OOM behavior still need dedicated validation.

AMD specs sit in the seed catalog next to other vendors' specs, sharing the model's configuration, sources and weights, and name the official ROCm runner.

The runner matrix exclusively owns ROCm base images, versions and Dockerfile selection. The vLLM entry is built, published and validated on MI325X and AMD395 hardware; the SGLang entry remains an unpublished candidate. No claims expand without actual build, pull and hardware evidence.

Tools connect only to an explicitly selected, authorized runtime; they never start a second dockerd or perform global prune. A separate socket/data-root is not network isolation. Another daemon requires a separate VM or a distinct network namespace proven before startup.

## Validation boundaries and future work

| Scenario | Required evidence |
|---|---|
| Integrated-GPU misclassification, vendor probe failure, identity replacement and blocked sysfs | Valid normal fixtures, real failure injection and bounded concurrency/cancellation tests. |
| One bad node, mixed vendors and cached shapes | Consistent API/controller refusals, remaining nodes eligible, resolution count proportional to shapes. |
| NVIDIA/Ascend/AMD Web lifecycle | Real browser, server, SQLite, agent and HTTP engine fixture; inspect requests, persistence, Ready, chat and stop. |
| Scale | Explicit AMD share in 200-node churn; identical baseline/candidate load comparing CPU-seconds, RSS and failures. |
| HIP/ROCm, model quality, long context, OOM and multi-card isolation | AMD hardware, actual images and real weights; CPU simulation cannot substitute. |

Before adding AMD models, partitioning or sleep/PD, establish device/engine and failure-path evidence, then update capability data and the backend. Current software checks do not inherit the retired AMD host's results or generalize one AMD395 to MI300 or other ASICs.

<!-- translation-source-sha256: a029a001bc4b935cc19d9236f4e2ea7520b7393312ced7982533ebee97697e4a -->
