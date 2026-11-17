# DeepSeek-V4.1-Flash-1017 (Strata)

This template provides **ISTA-DAS 2.00 bpw (GSQ-RCO IQ2_XXS)** through the separate `strata-dsv41` engine. Experts span VRAM, RAM and SSD; Engram rows are read from RAM or SSD. Input is text only, with vision disabled.

`quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.59-deepseek41-amd64-r4` is an unpublished candidate based on **Strata application release v0.2.13**, source commit `d2f096475817d4666c364c820b0ca830f751cfc5`, with the **0.1.59 native `strata-dsv41` engine**. The application release and native engine have separate version numbers. Later upstream application/setup versions no longer start DeepSeek; this runner prepares and serves the pinned inputs independently. Its current GPU selector covers only RTX 8000 and RTX 3080; other GPUs and quantizations are not registered. The entrypoint is separate from existing Qwen Strata r5 deployments.

The community document uses the DeepSeek-V4.1-Flash family name. Its pinned [ISTA-DAS 2.00 bpw recipe](https://github.com/Niko1221/Strata/blob/d2f096475817d4666c364c820b0ca830f751cfc5/setup.py) selects `DeepSeek-V4.1-Flash-1017-GSQ-RCO-GGUF`, matching this template.

## Validation scope

The r4 image was rebuilt from the pinned v0.2.13 source. Its local image ID is `sha256:e1baa913b829ac6187834c0963bed66bbf2bffdd0585a2c9eedf8a5588cc7408`; this is not a registry manifest digest. It remains unpublished. GPUWorks server, agent and UI for this validation were built from `c495fb3986e1d0aa534e9828f8fb06933ceb6750`, based on upstream `06272566648bebad8d2bda8750ecd37431b912c9`. The current patch is rebased onto upstream `7f87d8033ad8976cb25ce55b91143997c31a8f88`. The hardware results below belong to the original tested commit, not a repeated Web validation of the new upstream. The image entrypoint and native engine are unchanged.

The 2026-11-17 single-GPU tests use an RTX 3080 with 20 GiB VRAM, Xeon Silver 4208 (8 physical cores / 16 threads), and 123 GiB host RAM. The backbone and Engram are on separate HDDs. Settings are 8K context, one conversation, the default 8 CPU threads, all 6 routed experts, an 84 GiB container limit with no swap, and a 12-CPU quota. Each mode starts with a fresh pack and the original upstream expert profile. DSpark auto does not load a draft model on this configuration.

With default automatic expert placement, two arithmetic turns returned 42 and 50 with complete usage and stream termination. Three subsequent prompts completed with a 64-token ceiling, temperature 0, thinking disabled, a 180-second deadline and no retries:

| Request | Input / output tokens | TTFT seconds | Engine decode seconds | Engine tokens/s |
|---|---:|---:|---:|---:|
| 1 | 19 / 64 | 30.91 | 111.028 | 0.58 |
| 2 | 18 / 64 | 25.87 | 103.857 | 0.62 |
| 3 | 15 / 64 | 13.25 | 112.986 | 0.57 |

**Default-mode direct API workflow: PASS. TTFT ≤5 seconds: FAIL. Sustained generation ≥10 tokens/s: FAIL.** Container memory peaked at 72.14 GiB, with zero OOM events. The three pilot resource-observation windows charged approximately 8.5–11.5 GB of backbone HDD reads to the container; slow expert reads remain material on this configuration.

A separate `--mmap-experts off` run copied 80.8 GB of hot CPU experts and peaked at 83.56 GiB, with zero OOM events. Both arithmetic replies completed correctly, but an optional GPU observation timeout caused the test harness to abort before the three performance requests. That original FAIL is retained; sustained throughput for this mode is **NOT_RUN**. The stopped container and its unreferenced anonymous volume were subsequently confirmed removed. The observation error is corrected in the next test harness without changing inference or resource-limit checks.

The `--ssd-cache 0` comparison did not improve this shared-host trial. Its arithmetic TTFT was 135.75 / 79.05 seconds; two performance requests hit the 180-second deadline, and the third was cancelled when this regressing run was stopped. There is no completed-pilot throughput result for this mode. Memory reached the 84 GiB limit with 532 max events, but zero OOM events and no swap; cleanup passed. The cgroup-accounted file-cache size changed substantially during the run; the cause was not established. This setting is not recommended from these results.

A dual-GPU Web run used an RTX 3080 20 GiB and RTX 8000 48 GiB with explicit `--mmap-experts off` and the resource limits above. Six component registrations, built-in profile deployment, two arithmetic turns (42 and 50), history reload and streaming usage passed. Three pilots each generated 64 tokens: TTFT was 3.35 / 2.52 / 2.40 seconds and native decode was 4.43 / 4.76 / 4.64 tokens/s. All 9,041 CPU experts were copied to RAM (77.1 GB); periodic inference health checks were disabled for this deployment. Container memory peaked at the 84 GiB limit with 3,686 memory.max events, zero OOM and no swap; this limit incurred reclaim pressure. Web workflow and scoped cleanup passed. TTFT met the target; generation did not reach 10 tokens/s. This dual-GPU result is not a single-3080 speedup comparison. Long context, concurrency, cancellation recovery, public image pulls and weight acquisition on an empty node remain untested.

The host is shared with other work. Loading overlapped compilation, OS caches were not flushed, and preceding requests affect cache state. Loading times are not cold-start comparisons, and these short runs do not establish a hardware throughput ceiling.

### Historical r3 validation

The earlier r3 image used source `13eedf552417a6a791a7d13c6597e24300ee3a09` and native engine 0.1.59.1. Its 2026-11-13 Web workflow passed on **RTX 8000 plus RTX 3080**, with all 77.1 GB of remaining CPU experts copied to RAM. Three short requests reached 4.5–4.9 engine tokens/s and 2.21–3.37 seconds TTFT; the 10 tokens/s target still failed. That different image, two-GPU placement and 128-token protocol do not validate r4 or provide a single-3080 version-speedup comparison.

## Weights and storage

GPUWorks acquires six pinned weight files and mounts their directories read-only. The image includes the fixed 5,780,589-byte Engram metadata GGUF and checks its pinned SHA-256 before use. This zero-tensor file is not a complete model and must not be registered as one. The image entrypoint creates writable indexes and a run configuration. Preparation does not copy the complete weights, but indexes, logs and engine caches still need additional space. Keep the external inputs on local NVMe: slow reads of uncached experts and Engram rows reduce speed.

| File group | Bytes |
|---|---:|
| Four IQ2_XXS shards | 138,238,893,056 |
| Shared Engram IQ4_NL | 110,595,290,816 |
| DSpark Q8_0 | 7,976,914,240 |
| External weights subtotal | 256,811,098,112 |
| Engram metadata included in image | 5,780,589 |
| Total input bytes | 256,816,878,701 |

The model and Engram come from the [pinned ISTA-DASLab revision](https://huggingface.co/ISTA-DASLab/DeepSeek-V4.1-Flash-1017-GSQ-RCO-GGUF/tree/cb02422c900df489cea5833e8a92171224059b3c); DSpark and the image's metadata come from the [pinned Unsloth revision](https://huggingface.co/unsloth/DeepSeek-V4.1-Flash-1017-GGUF/tree/8a6ec9cf118e785a362ea94ee123c61c70553be6). ISTA stores several quantizations in a flat repository, so the template selects individual files. Its primary `weight_bytes` describes only shard one; storage planning adds the other five external components to obtain the external subtotal above. Original safetensors cannot replace these GGUF inputs.

To adopt existing local weights, keep the complete four-shard backbone, Engram and DSpark in three separate directories. A directory mixing the backbone and Engram contains two recognized entries and is rejected by local registration. Use the six template artifacts, identified by repository, revision and exact file path, then choose **Register existing path** for each artifact. All four backbone artifact rows may refer to the same complete four-shard directory; associate Engram and DSpark with their own directories. The six components retain separate read-only mounts. Repeated local registrations with the same source ID do not create separate artifact identities and can replace the recorded path.

Each assigned GPU needs at least 12 GiB VRAM, enforced as an admission floor. Host RAM is not part of that admission check: upstream marks configurations below approximately 40 GB RAM as untested and potentially slow, with more expert reads from SSD when RAM is limited. This is not a measured peak for this template. Leave room for the OS, context and other processes, and select suitable nodes as described in [known issue T109](https://github.com/flitai-io/gpuworks/blob/master/known-issues.md#t109-strata-的宿主机内存不参与准入).

## Deployment and runtime parameters

The first start includes input checks and index preparation. Set the deployment's startup timeout explicitly, for example to **7200 seconds**, and adjust it for the storage in use. GPUWorks defaults to an outer startup budget of 1800 seconds; the engine's separate READY wait is 3600 seconds. A longer timeout does not provide additional RAM or disk capacity.

The entrypoint prepares the pinned inputs and starts the service directly; it does not run setup to download missing weights. A missing component fails explicitly. GPUWorks sets `CUDA_VISIBLE_DEVICES`, and the engine uses those assigned cards. Do not override it through extra environment variables.

| Parameter | Default | Meaning |
|---|---|---|
| `--context N` | 8192 | Serving context; input plus output must fit |
| `--parallel N` | 1 | Concurrent conversations; more consume additional VRAM |
| `--mmap-experts MODE` | Omitted; engine auto | CPU expert placement: `auto`, `on` or `off` |
| `--cpu-threads N` | Engine default | Threads for CPU expert computation |
| `--ssd-cache N` | Engine automatic | RAM expert-store budget in decimal GB; 0 disables this store |
| `--max-queue N`, `--queue-timeout N` | Server default | Queue limits, validated by the image entrypoint |

Append these options through deployment custom arguments. The model's native context limit is 1,048,576. The template starts at 8K; longer contexts have not thereby been validated with this image. Longer contexts use VRAM otherwise available to the expert cache. No measured KV bytes/token value is declared, so the platform does not invent a token capacity.

The template omits `--mmap-experts` by default, preserving upstream's automatic placement. Explicit `auto` requests that behavior; `on` keeps CPU expert reads backed by the model files, with caching still managed by the engine. `off` copies as many experts as fit in RAM after GPU loading, retaining upstream's 8 GiB margin. If they do not all fit, the engine copies the hottest subset and leaves the rest backed by the model files. The engine may also adjust prompt chunks, buffers and expert placement for the selected mode, so comparisons do not isolate the copy mechanism alone. This engine budget does not add GPUWorks host-RAM admission or reserve memory. Check available node memory and container limits before selecting `off`.

`--ssd-cache 0` disables the engine RAM expert store, not the OS page cache. With `--mmap-experts auto` in this trial, CPU experts remained file-backed; native hot-page preloading and lookahead can still run. Combining this option with `--mmap-experts off` still allows copying the hottest experts to RAM. It does not alter quantization or the routed expert count. It is a different cache policy, not an SSD filesystem size setting.

## API and observation

Text requests use streaming `POST /v1/chat/completions`; health is exposed at `/health`. `/metrics/prometheus` supplies running/waiting request counts, processed prompt tokens, generated tokens and DSpark draft counters. Prompt tokens count newly processed tokens; the engine records cache reuse separately. `strata:requests_total` includes finished requests such as cancellations and is not mapped to successful requests.

The final reply's `timings.prompt_ms` and `timings.predicted_ms` map to engine prefill and decode seconds. No engine queue duration is declared. This template does not use the queue patch restricted to the old Qwen 0.1.38 server.

The GGUF mounts do not contain an independent standard tokenizer file. Benchmark fallback names the official model repository's pinned `tokenizer.json`. Check engine usage and client tokenizer counts separately; declaring the source does not establish measured parity.

See the [Strata v0.2.13 DeepSeek documentation](https://github.com/Niko1221/Strata/blob/d2f096475817d4666c364c820b0ca830f751cfc5/docs/DEEPSEEK41.md) for the pinned engine and preparation flow.
