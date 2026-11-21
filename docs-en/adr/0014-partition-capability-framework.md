# ADR 0014: dGPU Capability Is Ready Only After a Container-Level Probe Passes

Date: 2026-08-24 · Status: Accepted (see `plan-partition-rebase.md` for the framework scope and `plan-partition-probe.md` for the probe decision)

## Context

The old design stores a single `partition_mode` value on both nodes and GPUs, with vendor, MIG, agent detection results, and frontend logic independently inferring whether dGPU is available. The seven-axis framework and asset reporting eliminate most parallel facts, but the driver-only 910B4 incident on mirror1 shows that version tables and file existence alone still produce false negatives: without the CANN toolkit on the host, the version is empty, and the server rejects the node before checking assets; the agent's asset directory filtering then excludes candidates by version again. These two preliminary gates conflict with real deployments that provide CANN inside the container.

Comparison with gpustack-operator confirms that the version combination table describes which combinations have libraries built for them, rather than proving runtime availability. Live tests on mirror1 show that the minimal probe image is about 2.5MB and takes about 2.0–2.1 seconds end to end, while accurately observing 4096MiB/2048MiB quotas through `dsmi_get_hbm_info`. The cost of replacing preliminary inference with actual execution is therefore acceptable.

Alternatives: A) continue using the version combination table as a hard gate; B) perform only a `dlopen` loading check; C) have the server infer availability after the agent reports assets; D) have the agent run a one-shot container probe using the actual injection contract, and let the server derive ready only after the probe passes.

## Decision

Choose D:

1. **The set of dGPU capabilities on each physical GPU is the single source of truth**. Node capabilities only aggregate GPU capabilities; scheduling, API validation, and frontend display read the same set, without maintaining parallel mode fields. Capability entries have only `ready` and `unavailable` states, and one rule entry point generates unavailability reasons.
2. **The agent reports both asset facts and behavioral probe results**. The asset report still does not carry ready; `PartitionProbes` records the backend, passed/failed/skipped, the stderr tail, and the detection time. The server evaluates in this order: vendor match → container runtime → complete assets → probe passed → ready. If an older agent does not report probes, explicitly prompt for an upgrade.
3. **Capability claims and execution share code**. Through `Engine.BuildPartitionProbeWorkload`, probes call the actual device injection and `applyAscendPartition`/`applyPPUPartition`, then read the dsmi/HGML quota view inside an embedded offline image. There is no separate injection contract for probes.
4. **Version, CPU architecture, chip family, and glibc are compatibility hints, not hard gates**. An empty host version does not block availability; asset candidates are scored by version, but complete candidates are never excluded. Deployment separately warns about risks based on the selected image's CANN version.
5. **dGPU backends use a seven-axis contract**: stable ID, vendor/architecture match, maturity, required assets, injection contract, display metadata, and instance cleanup requirements. The registry explicitly lists NVIDIA, Ascend, and PPU backends, avoiding implicit initialization-based registration.
   The initial PPU execution path is fixed to Docker/runc, with GPUWorks explicitly injecting `/dev/alixpu*`. HGGC 13 comes from the workload image selected by an exact catalog match. No dedicated PPU container runtime is required on the host, and the udocker path, which has not yet passed real-hardware gates, is not marked ready. The tiny probe image does not bundle vendor SDKs; it mounts the host driver-side `libhgml.so`/`libuki.so` read-only to perform behavioral assertions. An empty host HGGC version cannot substitute for the probe result.
6. **Admission deliberately stays outside the backend abstraction**. Scheduling, API validation, and image resolution remain responsible for GPU health, available resources, exact image matching, deployment form, and product policy. Health is independent of capability, so a GPU in Warning state can report ready but is not scheduled.
7. **NVIDIA retains its existing behavior in this phase**. Embedded HAMi assets being present is sufficient for ready, with the backend-level probe framework reserved for future use. CUDA behavioral assertions require a separate later decision and do not block the probe changes for domestic accelerators.

## Consequences

- Positive: ready is proven by actual container execution on the target node. Driver-only nodes, empty host versions, and unknown chip families no longer produce false negatives due to preliminary tables; failures retain readable probe details.
- Positive: heterogeneous GPUs on the same node, multiple candidate backends for one vendor, and unavailability reasons can all be expressed accurately; experimental describes maturity rather than a switch that bypasses admission.
- Negative/constraint: when an agent first comes online or its assets change, it runs a one-shot container for about 2 seconds; device-share changes host driver state and must be idempotent and provide repair commands on failure.
- Negative/constraint: the probe proves the execution path between the embedded probe image and the current node. The selected inference image may still fail due to a different CANN/glibc version, so deployment evaluation must retain image-level warnings.
- Migration cost: add `nodes.partition_probes`; capability is unavailable when older agents do not report probes and recovers automatically after upgrade.

## Rejected Alternatives

- Extend the single-value enum: it cannot express multiple backends per GPU, reasons, or maturity, and continues to create inference in multiple places.
- Continue using version hard gates: driver-only nodes cannot supply a host CANN version, and even matching versions do not prove that the container injection chain works.
- Use only `dlopen`: it proves only that a library loads, not the end-to-end semantics of dsmi/HGML quota views, device mounts, environment variables, and driver flags.
- Put health and resource admission in the backend: the backend would depend on live scheduling state and could not serve as a stable, reusable execution contract.
- Replace the explicit registry with plugin self-registration: this hides initialization order and the supported scope, making exhaustive contract testing and security audits harder.

<!-- translation-source-sha256: c039cc5d79528f7bc982ff891518240918b5486a0a7fd68c640570120b581eb8 # ADR 0014: dGPU Capability Is Ready Only After a Container-Level Probe Passes -->
<!-- translation-source-sha256: 1b15d1132a2a1804fd27519ff125bdc05b53bea304c5717e8ffa29bd29602423 ## Context -->
<!-- translation-source-sha256: 67c3752b8c4803fc5aa6a392e00c047181d1a4d0952d6dd99152caad616a65dd ## Decision -->
<!-- translation-source-sha256: 1c41f1d562e21de78a4314533283f845f474439eed6bb9983b774627a0fe647b ## Consequences -->
<!-- translation-source-sha256: 3af9642853e0d47589338a8f1851c84edd47183ff00253d6d28aeafb95a9600a ## Rejected Alternatives -->
