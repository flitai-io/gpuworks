# Kunlun P800 Qwen catalog package

This optional package adds Qwen3-0.6B and Qwen3.8-27B text inference profiles to the existing model identities. It does not replace the upstream built-in image matrix or any NVIDIA/Huawei profile. See [中文说明](README.zh-CN.md).

## Import

1. Build the small catalog archive: `python3 build/inference-templates/kunlun-p800/build.py --output /tmp/kunlun-p800-qwen.tgz`.
2. Make the reviewed private-registry image available to the selected Docker/nerdctl nodes, with registry credentials if required. The specs pin its manifest digest directly. No separate image catalog import is needed: the specs use the locked image directly.
3. In **Inference templates → Import**, upload `kunlun-p800-qwen.tgz`. Both specs are user profiles named `bf16-vllm-kunlun-p800`; existing profiles are retained.
4. Select the profile under Qwen3-0.6B or Qwen3.8-27B. Use Linux amd64, Docker/nerdctl and whole P800 PCIe/KL3 cards. Start with one card and the package defaults. Qwen3.8 is text-only here.

The template uses the reviewed private-registry K26 vLLM 0.25.1 build, with Qwen3.8 compatibility and PP/DP worker-rank/device-mapping fixes, recorded as manifest `sha256:33d94768ff9a895bdc8874cae09940aede1b0e88218e52ed52231b8f86a91beb`. The specs use the direct entrypoint `/opt/vllm025-venv/bin/vllm serve`; image-lock records the actual interpreter `/opt/vllm025-venv/bin/python`. Registry publication to the upstream runner matrix is a separate step; this package does not invent public mirror references.

Weights remain official, original BF16 files (no AWQ/GPTQ/INT4 replacement). The template requests FP16 computation: `float16`. This is a runtime cast, not quantized replacement weights. Revisions are pinned in each model file. Weight sizes and KV estimates are carried from the corresponding built-in original-weight profiles; they are not new measurements of this image.

GPUWorks single-node device injection verifies and exposes only the selected XPUs and control device. It does not require XDR, RDMA devices, IPC_LOCK or relaxed memlock. The image and local multi-card communication combination still require validation; this does not promise arbitrary image compatibility. Single-node multi-GPU also requires a real non-loopback interface on the node's management network. The agent validates it and derives `BKCL_SOCKET_IFNAME`, refusing before container creation when it is absent; the template no longer sets BKCL communication parameters.

## Parallel deployment

- TP means Tensor Parallelism; PP means Pipeline Parallelism; engine DP means Data Parallelism. For TP-only deployment, set PP=1 and DP=1. TP×PP×DP determines the total GPU requirement, while GPU count and CUDA-visible indexes in this package are per node. Two nodes × four cards with PP=DP=1 means TP8.
- Use the locked PP/DP image, not the earlier RDMA-only image: that image lacks the rank-sharded device mapping needed by DP. Engine DP also requires the server allocation fix for all replicas. Platform replicas are a separate deployment mechanism.
- Cross-card communication must work on the host before TP/PP can succeed; an image update cannot correct a host XRE/OAM identity conflict. The following network requirements apply only across nodes.
- Select a verified compute network. The agent copies its system `GLOO_SOCKET_IFNAME` to BKCL; do not put an arbitrary management NIC in the template.
- Set the deployment environment `BKCL_RDMA_NICS` to the verified RDMA **netdev** list for the selected cards on each node, for example `bond0,bond0` for two local cards when that bond is the actual RoCE netdev. This is not an HCA name such as `mlx5_0:1`. The platform intentionally cannot guess a site's bridge/bond mapping; the agent refuses a multi-node launch without this setting. Nodes with different mappings need appropriate per-node deployment configuration before use.
- Hosts need compatible native/XDR drivers and functional RDMA. The resolved host-RDMA network provides verbs devices, unlimited memlock and IPC_LOCK; the prober adds selected XPU devices, XDR and the derived BKCL environment. Multi-node launches without these resolved resources or netdevs, including finite memlock overrides, are refused. The template uses a direct inspectable vLLM entrypoint and does not pass extra GPU devices, hardcode physical indexes or load kernel modules.
- Confirm the selected cards form a working collective group and are free. Zero memory usage alone is not proof of availability. Reuse the existing Docker daemon; do not start another daemon or change docker0.

Defaults are conservative: context 2048, one sequence, 256 batched tokens, 70% memory, eager execution. They are a bring-up configuration, not a throughput recommendation. Supported local cardinalities are 1/2/4/8; model/global-TP divisibility and runtime constraints still apply.

## Validation boundary

The Go package test validates package/image import, official revisions, card predicates, local visibility, global TP, the direct entrypoint, dtype and launch refusals. On 2026-11-10, two P800 hosts passed production single-GPU evaluate/create, gateway chat, stale-device cleanup and agent-restart acceptance. Single-node multi-GPU moved past the loopback-interface failure but remains blocked by that environment's XRE OAM/XLink initialization error; cross-node operation was not tested. PP and engine DP are enabled, but package import, offline tests and the single-GPU result are not multi-GPU topology acceptance.

The general Web evaluation-default inheritance fix must also be present for untouched form defaults to keep the template dtype. It is delivered separately from hardware enablement. Before starting on older servers, explicitly choose Float16 and verify the command preview.

Both the specs and image-lock pin the reviewed manifest digest. No mutable-tag image catalog is shipped. This remains an optional private-registry package, not a public built-in runner release.

The multi-node image additionally includes Ubuntu RDMA userspace libraries (`libibverbs`, `librdmacm`, `libmlx5`) and providers. Passing GPU/XDR devices alone does not supply these libraries. Single-node device injection remains independent of XDR.
