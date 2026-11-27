# Phi-4-mini-reasoning on ZW810E

The optional catalog package adds an experimental BF16 profile for a single PPU-ZW810E with at least 96 GiB. It uses the pinned ModelScope checkpoint and the HGGC vLLM 0.23.0-r2 runner. The profile selects async scheduling, a 4,096-token batch budget and decode-only CUDA graphs with capture sizes 1 through 4.

## Build and import

From the repository root, use the repository's Go toolchain to package it.

```sh
go run ./cmd/gpuworks-pack-catalog \
  --catalog build/inference-templates/phi-4-mini-reasoning-zw810e/catalog \
  --output /tmp/phi-4-mini-reasoning-zw810e.tgz
```

Upload the archive under **Inference templates → Import**, then select **ZW810E · BF16 · Async decode graph (experimental)**. Use a dedicated card for comparisons with the lab results. The profile limits model context to 2,048 tokens and concurrent sequences to four. The deployment reserves 22 GiB on the device, with vLLM memory utilization set to 0.22.

## Lab evidence

The isolated single-card benchmark kept BF16, model weights, runner, graph settings and input order fixed. It compared synchronous Graph with async Graph at a 4,096-token batch budget. At concurrency four, output throughput rose from 329.27 to 360.45 tokens/s and from 332.03 to 360.67 tokens/s in two opposite launch orders. The benchmark used direct streaming HTTP and synthetic prompts.

All six serial answer checks passed. In the mixed-concurrency checks, the same arithmetic prompt reached the 1,536-output-token cap in all four starts. A separate comparison of 4,096 against 2,048 showed negligible p95 TPOT change and higher p95 TTFT at 2,048. A direct comparison of this combined profile with the built-in eager deployment and a GPUWorks staging import and launch are still pending.
