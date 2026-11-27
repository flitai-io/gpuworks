# DeepSeek-V4.1-Flash-1017 (EXL3 2.8bpw, two DGX Spark)

jayleaton's TensorFold native (Zig) TP2 recipe ([deepseek-v41-tensorfold-spark](https://github.com/jayleaton/deepseek-v41-tensorfold-spark), commit `13db91e`, TensorFold `f212eeab`) for DeepSeek-V4.1-Flash-1017, hosted on the `callable` engine. The 1M-token context, image input and speculative decoding are all on; the 748B-parameter model (552B backbone plus 196B Engram tables) runs on two DGX Spark (GB10) nodes, one TP rank per node.

## What the template provides

- **Image** `gpuworks/tensorfold-dsv41-flash17:f212eeab-r1`, also on quay.io and SWR. It carries the TensorFold engine, the runtime assets (RoPE table, token map, signatures and 144 AOT kernel variants for sm_121), the engine profile those assets were compiled for, and a launch adapter: rank 0 runs `tensorfold-dsv41 serve`, rank 1 runs `tf-dsv41 follow`. Assets and profile ship together because the AOT set depends on the context, slot, prefill and drafting settings.
- **Weights**: the EXL3 2.8bpw pack from Hugging Face (about 204 GB, the same on both nodes) plus the Engram tables, packed for TP2 and split per rank on ModelScope (about 101 GB per rank). Each node downloads only its own `rank<r>/` of the Engram component.
- **Managed directories**: `tf-prepared` (128 GiB) holds the prepared weight forms and `tf-state` (150 GiB) holds calibration tables, the request log and the session disk tier. Both are kept across restarts.
- **Platform facts**: rank, master address and port, the RDMA devices and interface, and locked memory come from the deployment; the template adds `--ipc=host --init` and unlimited memlock.

## Deployment requirements

- Select exactly two GB10 nodes, one rank per node. The adapter refuses any other node count.
- Both nodes must sit on the same Host RDMA parameter network (RoCE ports); without one, the deploy evaluation says why.
- Each node needs room for the 204 GB pack, its 101 GB Engram part and the roughly 96 GB of prepared forms written on the first start.
- The engine's memory margin on two Sparks is thin (the 128 GB of unified memory holds the backbone and leaves only a few GiB for caching Engram pages). Do not run other memory-hungry workloads on the nodes.

## Start-up time

- **First start about 13 minutes**: load the pack, convert it into this rank's prepared forms (about 96 GB written to `tf-prepared`) and time the draft windows. The template's start timeout is 3600 s.
- **Later starts about 7 minutes**: the prepared forms are read back directly.

## Measured performance

On two GB10 nodes with ConnectX-7 over RoCE, GPUs locked at 2200 MHz.

Direct engine measurements, real text, thinking off:

| Measurement | Result |
|---|---|
| Single-stream decode | about 50 tok/s |
| 2 / 4 streams, aggregate | about 71 / 103 tok/s |
| Prefill, 16K / 128K tokens | about 2.3K / 2.5K tok/s |

Through the platform benchmark (gateway, model-default thinking on), with real-text datasets the aggregate output is about 26 to 33 tok/s at concurrency 1 to 4 and TTFT is 0.5 to 2 s. Decode time per token grows nearly linearly with concurrency (each extra request adds about 31 ms to a step), so aggregate throughput barely rises with concurrency.

**The workload matters a lot.** Each rank's Engram table is about 94 GiB and cannot stay in memory, so it is read from disk on demand. Real text has locality and mostly hits the page cache. Random-token datasets touch the disk on almost every lookup: TTFT grows about tenfold (6 to 7 s) and aggregate output drops to about 17 to 19 tok/s. Judge this deployment with a dataset close to your own traffic; the benchmark can switch datasets on the same deployment.

## Defaults and limits

- Reasoning effort defaults to `high`; a request overrides it with `reasoning_effort`.
- The in-thinking calculator ("runner", where the engine computes arithmetic for the model) is **off**. With it on, the engine writes its cell markup (`<expr>...</expr>`, `Result: ...`) into the visible answer. It is switched off in the image's profile (`TF_DSV41_RUNNER=0`, `TF_DSV41_COMPUTE_LANE_DEFAULT=0`, `TF_DSV41_COMPUTE_LANE_RUNNER=0`); the AOT set was checked with that profile and no variant is missing.
- Context, slot count and drafting are fixed by the image's profile because the pre-compiled kernels depend on them. Changing them needs a new image, not a deploy-form argument.
- Long prefills queue other requests: with several users sending very long prompts, the first token of the others can wait minutes.
- For many users or agentic workloads, a four-node profile spreads the Engram tables over more memory and is the better fit; see the DeepSeek-V4.1-Flash-1017 pico TP4 profiles.
