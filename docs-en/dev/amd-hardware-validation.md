# AMD Integration Structure and Hardware Validation

See [AMD support design](amd-support-design.md) for the implementation design.

AMD integration reuses the Prober, PCI inventory, and DriverAwareInjector interfaces used by other accelerators. The PCI roster recognizes an AMD function as a card only when bound to `amdgpu`; the agent then reads amdgpu sysfs/KFD facts and explicitly passes the selected render device. NVIDIA retains the container device-number semantics of its toolkit, and Ascend retains runtime-specific device injection.

Automatic probing preserves the existing vendor priority. Select `amd` explicitly through the node accelerator-selection API on mixed NVIDIA/AMD hosts, or AMD hosts with a stale or compatibility `nvidia-smi` installation; one agent manages the selected vendor.

## Responsibilities and support boundary

- `accelerator` declares vendor identity, host requirements, and capability constraints. Physical AMD unified memory does not use Apple's shared-memory scheduling policy; workloads are still placed as full-GPU allocations.
- `deploy.RequestedLaunchFeatures` normalizes server-side launch requirements; `planning` and `scheduler` admit a deployment using the actual template-matched candidate cards. A mixed-vendor node is not restricted as a whole just because it contains AMD.
- The agent rechecks selected-device identity and character-device numbers, then returns additions, removals, or a refusal through the shared `RuntimeInjection` contract. The engine applies the result only after validation succeeds. AMD requires a nonempty device selection; the agent rejects an empty selection, and failures do not leave partial changes.
- Sleep admission, engine declarations, and protocol execution are separate layers. AMD shares the vLLM sleep protocol (staged L2 wake) with NVIDIA, validated on MI300X.

AMD uses the shared Prober, DriverAwareInjector, and target-constraint interfaces without depending on another vendor's driver, prober, topology injector, or model templates. The shared layer contains host facts, capability declarations, and refusal contracts; AMD's own prober handles KFD/render mapping and ROCm device injection. Missing OS, architecture, or runtime facts produce runtime_unreported; known mismatches produce runtime_unsupported. Neither authorizes a launch. Template candidates are checked against the actual matching cards: a NVIDIA card cannot stand in for an AMD-only match on a mixed node, while a template allowing NVIDIA can still select that card. All vendors consume the resolved launch feature snapshot; explicitly constrained targets cannot bypass admission through an unknown command.

The implementation admission scope is Linux amd64, Docker/nerdctl/runveil, and full-GPU deployment (single host or vLLM multi-node). Validate each image, model, and target device separately; passing one combination does not establish compatibility across AMD GPUs. Data parallelism takes the same path as on NVIDIA, and GPU-memory partitioning is the `amd-dgpu` backend; validate sleep and PD (`vllm-nixl`) per image and network; validate multi-node and pipeline parallelism per network and RCCL version. System RAM is not automatically schedulable GPU memory.

UMA capacity probing is enabled only through the narrow AMD395 path constrained by explicit device facts: PCI device `0x1586`, KFD `gfx1151`, and a unique KFD node matching the DRM render minor and PCI domain/location. The single KFD public bank must be a system-memory heap whose capacity exactly matches the GTT total for the same DRM device. When these facts agree, GPUWorks reports that KFD/GTT pool capacity and uses GTT used for utilization; this is the pool the driver exposes to the GPU, not onboard VRAM, total host RAM, or current HIP free memory. Missing or conflicting AMD395 UMA facts report unknown capacity and degrade the card; a small dedicated carve-out is not treated as verified healthy capacity. Other AMD/APU devices are not classified as UMA from `local_mem_size=0` alone.

The ROCm image recipes are in [build/runner-images/rocm](../../build/runner-images/rocm/README.md). The vLLM entry of the official runner matrix is published and listed in the built-in image catalog; the SGLang entry remains an unpublished `catalog: false` candidate. Component checks and a successful image pull do not prove GPU inference, model quality, or isolation; verify the image's gfx target, digest, and model revision first.

## Host preparation and import

1. Record GPU model, PCI BDF, OS/kernel, amdgpu/ROCm, and container runtime versions. For an APU, also record firmware, UMA/carveout settings, and available host memory. Do not fabricate a gfx version to bypass a mismatch.
2. Confirm that the runtime user can access `/dev/kfd` and the selected `/dev/dri/renderD*`. Compare `/sys/class/kfd/kfd/dev`, the PCI render node's sysfs `dev`, and the actual character device's major/minor. Device symlinks, ambiguous mappings, duplicate identities, or inventory identity changes must refuse injection.
3. Compare GPUWorks-reported sysfs VRAM with the capacity actually available to ROCm. A 128 GB system-memory report or firmware carveout setting does not prove that a 27B model, runtime, and KV cache fit.
4. Receive and verify the image archive in the target storage, load it into the Docker daemon or nerdctl namespace used by the agent, and check its tag and image ID. Importing image-catalog metadata does not transfer image layers.
5. Onboard the node through the product agent install flow, run `gpuworks-agent doctor` to inspect driver, KFD, render devices and runtime, then run `gpuworks-agent smoke` for bounded checks on that node; retain the actual commands and results. Deploy the built-in AMD spec for the card's `arch_id` and keep default engine command mode.
6. Prepare complete, revision-pinned weights and tokenizer. Check the final model mount, port, dtype, TP, and image; verify that template shm/memlock and placement preference appear in the launch plan.

## Runtime and model validation

Start only one engine at a time, retain logs, and set resource limits on the test host.

1. In the image, run a small GPU allocation and FP16/BF16 matrix operation, synchronize the GPU, and record device/gfx, available and total memory, and a result checksum.
2. Use the conservative settings for the matching template. The gfx1151 Qwen3-0.6B and Qwen3.8-27B templates use BF16, TP1, and a 4096-token context; vLLM allows one concurrent request and SGLang allows four. Qwen3-0.6B uses memory fractions of 0.25 for vLLM and 0.40 for SGLang; both Qwen3.8-27B engines use 0.75. vLLM uses eager mode, while SGLang uses Triton attention and disables CUDA graphs. Both disable AITER until its kernels are verified.
3. Require successful startup, `/health`, and a bounded text chat request. Preserve the initial failure log before changing context or memory fraction. Test image input separately after text succeeds; this does not establish video support.
4. Deploy repeatedly through GPUWorks and check that the container receives only `/dev/kfd` and the selected render node. Verify model path, port, inventory/metrics, stop, restart, and recovery. A standalone manual launch that maps the full `/dev/dri` cannot prove per-device selection by the platform.
   Health-check history may remain visible after a stop, but a deployment with zero ready replicas must not be shown as currently healthy.
5. On a multi-GPU host, verify selected-device visibility and independent instances; validate single-host TP only with a matching image and model. Mark cross-card tests not applicable on a single-APU host.

The built-in AMD specs declare an exact `gpus.arch_id` (`gfx942` or `gfx1151`). Missing or mismatched architecture facts must fail template matching even if the name contains `gfx1151`. This narrows candidate selection; it does not prove GPU kernel execution or the exact product SKU. Real-hardware acceptance is still required.

## Evidence to retain

For each engine, retain image ID/archive digest, host and driver facts, model revision, actual command and device mapping, GPU/host-memory observations, health and request results, and every skipped or failed item. A check that cannot run in a CPU-only environment because no HIP device is available remains unverified.

Software fixtures cover damaged fields, partial inventories, duplicate identities, and mismatched device numbers. Do not modify real sysfs or device nodes to manufacture a failure. Injection checks apply the assigned devices; they are not a security sandbox for administrator-supplied container configuration.

<!-- translation-source-sha256: 3220b213c1296422816f73ca65d5d998c527b63af1677d3de3df4c2804e2a053 -->
