# What dGPU Is and Is Not

dGPU (dynamic GPU) injects GPU memory budgets and the required runtime libraries into containers, constraining multiple instances on the same physical GPU to their respective memory limits.
It does not virtualize the GPU into two PCIe devices with independent failure domains, fixed bandwidth, and fixed compute capacity.

Implementations and upstreams per backend: NVIDIA uses [HAMi-core](https://github.com/Project-HAMi/HAMi-core) (`libvgpu.so`); Ascend uses vcann-rt from openEuler [ubs-virt](https://atomgit.com/openeuler/ubs-virt) (`libvruntime.so`), a software-level logical quota rather than the driver-level vNPU partitioning of Ascend `npu-smi`; PPU and AMD use ppu-slicing-shim (hooking the PPU HGGC runtime and HGML) and rocm-slicing-shim (`libvrocm.so`); Hygon uses native driver vdevs. Pinned upstream revisions and this repository's patches are listed in `build/runtime-assets/manifest.json`.

- NVIDIA uses HAMi-core: libvgpu.so and CUDA environment variables. The quota is a hard GPU memory limit. HAMi enforces its compute cap by throttling kernel launches,
  which caps ordinary kernels, but does not constrain the CUDA graphs and cuBLAS used by vLLM by default. With multiple inference instances sharing one GPU, it cannot allocate compute proportionally,
  so the platform does not expose a compute cap.
- Under long-context, high-concurrency workloads, PPU ppu-slicing-shim can turn a 20/80 configuration into busy-time throughput weights close to the target. It borrows spare capacity when the other instance
  is idle, so this is not fixed SM partitioning.
- Ascend vcann-rt/device-share depends on the combination of host permissions and runtime. The current Gitee cloud environment proves only node
  probes or full GPU chat; a cloud-side rejection cannot be reported as a dGPU pass.
- Hygon Z100 / K100_AI / BW uses native vdevs (experimental), with one GPU per instance. The memory quota also determines the CU allocation; compute cannot be adjusted independently, and inference throughput is not guaranteed to scale with the memory quota. Docker or runveil/udocker on x86_64 is supported, with the native helper, host libraries, and a passing per-GPU behavioral probe. Devices reporting only K100, or other unsupported models, remain full-GPU only.
- AMD uses rocm-slicing-shim (libvrocm.so) (experimental, user space, interposed at the HIP runtime layer). The quota is a hard GPU memory limit. ROCm enforces its compute quota in hardware
  through `HSA_CU_MASK`, below anything a preloaded library can see or verify, so the platform does not expose a compute cap for AMD either — the same
  outcome as NVIDIA's "cannot be enforced proportionally," reached by a different mechanism. Docker, nerdctl, or runveil on x86_64 is supported.
  dGPU has been tested on MI300X (gfx942) under Docker and runveil; other models and combinations remain unverified.

The server derives partition capability status/reason from hardware facts, assets, and behavioral probes; the agent does not declare
ready on its own. When the capability requirements are not met, full GPU deployment remains available, but a request with a dGPU memory limit must fail explicitly (for example, the scheduler returns
partition_unavailable), rather than silently dropping the quota. NVIDIA and AMD are exempt from behavioral probes but must still meet asset, architecture, and runtime requirements; Ascend, PPU, and Hygon must pass a per-GPU behavioral probe. For platform and environment boundaries, see the [Support Matrix](../reference/support-matrix.md).

## Quota Changes: Immediate on the Platform, Engine-Dependent in Use

HAMi reads the current limit from the instance's shared area on every allocation and every query for total GPU memory. Platform quota changes therefore take effect immediately: the instance's desired state and scheduling accounting
use the new limit, and the agent writes it to the shared area ([Adjust dGPU Quotas](../how-to/dgpu.md#调整-dgpu-配额)).

Whether the engine can use it depends on the engine:
vLLM and SGLang size the KV cache once at startup based on the limit they see, so you must restart the instance to use an increase. Engines that allocate on demand can use it immediately. Lowering the limit below memory already occupied by the engine
would cause its next allocation to fail and the engine to exit, so such instances retain their original limit until they restart with the new one.

Ascend vcann-rt, PPU ppu-slicing-shim, and AMD rocm-slicing-shim read the limit only when the container
starts, so their instances also require a restart. Hygon Z100 / K100_AI / BW also requires an instance restart, releasing the old vdev before creating one with the new memory quota and CU count.

## How Scheduling Accounts for a dGPU Instance

A dGPU instance is charged against the physical GPU as "GPU memory limit + the portion the backend allows outside the limit." Scheduling admission and
allocated GPU memory on the GPU page use the same number. NVIDIA HAMi-core changes the total GPU memory visible to the engine to the limit, so the engine's own
reservations (CUDA graphs, activations, and so on) all come from within that limit. Only the CUDA context, which HAMi cannot see, remains outside it: measurements show about
400 MiB per process, accounted as 512 MiB. Backends whose overhead outside the quota has not yet been measured (Ascend, PPU, Hygon, and AMD) still use the engine reservation for full GPU deployments, preferring
a slightly larger deduction over overselling a GPU.

If the request does not specify a backend, the selected node uses its own ready backend (preferring its vendor's backend when both that and the NVIDIA backend are available).
If one is specified, only that backend can be used. Automatic placement does not know the selected node in advance, so it never specifies a backend.
With automatic placement, the deployment form determines whether dGPU can be enabled from the backends available on candidate nodes, not from the node chosen by the scheduler:
when every GPU already has an instance, full GPU deployment cannot find a node, which is exactly when partitioning is needed most.
