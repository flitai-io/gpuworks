# Kunlun P800 Deployment

## Current Capabilities

P800 PCIe / KL3 uses vendor `kunlunxin` and framework `xre`. Validated runtimes are Docker and nerdctl on Linux amd64; runveil is not enabled. This combination supports whole-card TP (Tensor Parallelism), PP (Pipeline Parallelism) and engine DP (Data Parallelism). The image and model must support the chosen combination; cross-node deployments also require the communication prerequisites below. PD, partitions and sleep remain unavailable. Platform replicas, engine DP and cross-node TP are distinct topologies.

Reuse the host’s existing Docker daemon. Independent sockets/data roots do not isolate host networking. Do not launch another dockerd with `--bridge=none` in the same network namespace: it can remove the existing `docker0`. Image and inference testing does not require restarting Docker; offline probes use container-level `--network none`.

Native discovery uses `xpu-smi -q -x`. It repairs only the observed closing-tag typo within process_info; other malformed XML fails. Unknown ECC produces a telemetry warning without rejecting scheduling. Known uncorrectable ECC errors make that card unschedulable; peers remain reported. PCI inventory supplies cards omitted by the CLI. Inventory reads may use the command cache, but device injection freshly verifies UUID, PCI BDF, minor and sysfs character devices. The common compute-process inventory continuously reports occupancy.

Launch admission fails closed for unknown handwritten commands: when it cannot determine whether a command requests a restricted feature, evaluation/creation returns `LAUNCH_FEATURES_UNKNOWN` and the scheduler refuses with `launch_features_unknown`; an unknown command is not treated as requesting none of those features. Kunlun requires an explicit nonempty GPU selection rather than implicitly exposing every XPU. Device injection is authoritative for the current selection: managed XPU/XDR device mappings and conflicting runtime options from templates or user parameters are removed before the selected devices are injected, while unmanaged mappings such as `/dev/infiniband` are preserved.

GPUWorks single-node device injection verifies and exposes only the selected XPUs and control device. It does not require XDR, RDMA devices, IPC_LOCK or relaxed memlock. The image and local multi-card communication combination still require validation; this does not promise arbitrary image compatibility. Single-node multi-GPU BKCL collectives cannot use `lo`: launch assembly uses the real non-loopback interface reported on the node's management network, and the agent derives `BKCL_SOCKET_IFNAME`. A multi-GPU launch without a usable interface fails before container creation; single-GPU launches are unaffected.

## Images and Model Profiles

Hardware support does not automatically supply an inference profile for every model. The repository has no default published Kunlun runner matrix entry; use the existing image and template import APIs to supply verified images and user specs.

Qwen3-0.6B and Qwen3.8-27B are available as an [importable template package](../../build/inference-templates/kunlun-p800/README.md). It adds only user profiles. The package specs pin the reviewed private-registry manifest digest, with no second mutable-tag image-catalog authority. Configure native communication as described in the package for two-node launches.

1. Prepare the same verified image on both nodes. Record immutable digests, engine version and the actual Python/entrypoint paths. CUDA-compatible fields do not establish native runtime support.
2. Register local weights with their full repository identity, such as `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`. A short directory name is not a repository identity. Existing files can be registered correctly without copying or downloading them.
3. A spec’s `weights.sources[].repo_id` must match the model’s `source_id`; set `gpus.vendor` to `kunlunxin` and match the `P800 PCIe` card name. Missing specs or mismatched identities leave the Web form without a compatible profile.
4. Use the image's verified direct entrypoint `/opt/vllm025-venv/bin/vllm serve` with explicit dtype, context and memory settings. The agent injects container-logical `XPU_VISIBLE_DEVICES`, and the template's `CUDA_VISIBLE_DEVICES=${LOCAL_VISIBLE_DEVICES}` also uses container-logical indexes. Do not mistake host indexes for container-logical indexes.
5. A single-card spec may fix TP=1. A cross-node placement resolver checks local card count but leaves global TP to the deployment: two nodes with two cards each use TP=4, PP=1, nnodes=2, not TP=2 from the resolver.

## Single-Node PP and DP

PP partitions one model by layer, passing intermediate activations between stages. DP starts multiple model replicas and distributes requests; each replica still shards its weights by TP × PP. vLLM requires TP × PP × DP GPUs in total. Increasing DP does not reduce per-replica weight memory. Platform replica count controls independent deployment units, not engine DP.

In Web, first select two free whole GPUs. PP2 uses TP=1, PP=2 and DP=1. DP2 uses TP=1, PP=1 and DP=2 (the data-parallel size in advanced parameters). Confirm evaluation allocates two GPUs and the command preview carries those degrees before creating. Neither single-node mode requires XDR. Multi-node mode retains XDR and compute-network checks even with DP.

The DP image must handle rank-sharded device mappings correctly: when `assigned_physical_gpu_ids` is set, it must not apply the DP offset to the local index again. Otherwise the second rank looks up index 1 in a one-device mapping and fails. Use an exact image containing this fix; enabling the platform capability does not repair the image. Single-node entrypoints must not inherit the build host's interface name. The agent validates and injects the BKCL interface uniformly; the template no longer sets BKCL communication parameters.

## Two-Node Communication Prerequisites

- Both nodes need working XDR compatible with their kernels, and this is a hard prerequisite for multi-node: `/dev/xdrdrv` must be a real character device, and a rank whose host lacks it fails during device injection with `accelerator runtime unavailable: Kunlun multi-node runtime requires XDR`, taking the whole multi-node unit with it. Such a node still comes online, is probed and is scheduled, and single-node work on it is unaffected, so an unmet prerequisite shows up only in multi-node. Confirm `modinfo xdr` answers and `/dev/xdrdrv` exists on every node before looking elsewhere. The agent only checks and exposes devices; it does not install, compile or load drivers, or alter KASLR/boot entries.
- Configure a reachable compute network and verify the inventory’s mapping among RDMA ports, interfaces and IPs. Inventory neither changes networking nor establishes measured bandwidth.
- The common network layer supplies `/dev/infiniband`, unlimited memlock, `IPC_LOCK` and `GLOO_SOCKET_IFNAME` for a host RDMA compute network. The Kunlun prober injects the selected XPUs, control device and XDR, derives the BKCL XDR/verbs switches and `BKCL_SOCKET_IFNAME`, and does not enumerate all HCAs or duplicate network arguments. The deployment environment must also provide `BKCL_RDMA_NICS`, the selected cards' RDMA netdev list. XDR must be a real character device. Multi-node launches explicitly refuse missing resolved resources or netdevs and finite memlock limits.
- `IPC_LOCK` and memlock relaxation serve communication-memory registration. Templates and host mounts use the common administrator trust model; do not add other GPUs or device directories.

Images must supply the corresponding RDMA userspace libraries and verified communication parameters. The platform does not introduce global defaults that bypass driver checks or modify kernel boot arguments. Check the supplier’s supported kernel, driver and image combinations before use.

## Validation and Boundaries

Acceptance on 2026-11-10 covered only production single-GPU evaluate/create on two P800 hosts, gateway chat, stale-device cleanup and agent restart; all of these checks passed. Single-node multi-GPU moved past the loopback-interface failure but remains blocked by that environment's XRE OAM/XLink initialization error. Cross-node operation was not tested. PP and engine DP are enabled capabilities, not evidence that these multi-GPU topologies have completed hardware acceptance.

On 2026-11-11, single-GPU evaluate/create, health and chat passed with the final `33d947…` image on both Docker and nerdctl. Its Kunlun vLLM platform rejects `--enable-sleep-mode` before loading the model, so sleep/wake remains unavailable. With NIXL added experimentally, the UCX agent initialized but identified P800 KV VRAM as host memory and returned `NIXL_ERR_BACKEND` during registration, so PD is also unavailable. Opening GPUWorks gates cannot bypass these engine limitations. Runveil completed device selection and model loading, but the current image lacks the Kunlun XTDK/`CLANG_PATH` toolchain required by JITC, so inference does not complete with the default artifacts and runveil remains disabled.

For real-hardware acceptance, verify each node on one card first, then collective communication and two-node deployment. In Web, choose the correct profile, two nodes and TP=4/PP=1; check each rank’s command and model/image acquisition source, then health, ordinary HTTP, SSE DONE and visible response text. A handshake or HTTP 200 alone does not prove a valid text response.

See the [vendor support matrix](../reference/support-matrix.md) for related boundaries.

The hardware catalog displays `Kunlunxin P800 PCIe` and stores vendor `kunlunxin`; `NormalizeGPUName` removes the brand before spec matching, so the reviewed `^P800 PCIe$` predicate still matches. Hardware identity feeds capability selection as runtime and model combinations are validated. Partition availability comes from the dynamic partition capability check; there is no static Partition feature axis. The engine Profile still defines engine PD modes. Vendor restrictions are defined only in the accelerator capability table; `ProfileForTarget` derives the modes available on that target from it.
