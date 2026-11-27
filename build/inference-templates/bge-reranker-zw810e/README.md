# BGE reranker on ZW810E

This optional catalog package adds `bf16-vllm-ppu-zw810e-rerank-latency` under the existing `baai/bge-reranker-v2-m3` model identity. It targets short text reranking on a single 96 GiB PPU-ZW810E. Import it to evaluate it alongside the built-in eager profile.

## Build and import

From the repository root, with Python 3 and the repository's Go toolchain:

```sh
python3 build/inference-templates/bge-reranker-zw810e/build.py --output /tmp/bge-reranker-zw810e.tgz
```

Under **Inference templates → Import**, upload the archive and select **ZW810E · BF16 · Low-latency rerank**. The profile uses the HGGC vLLM 0.23.0 r2 runner and the pinned ModelScope BF16 checkpoint. It selects `PIECEWISE` with capture sizes `[96,128,192,256]` and a 1,024-token model limit. The installer checks the five affected vLLM modules against the tested runner and applies the source patch in the instance's writable layer. A restart accepts the same already-patched modules.

The patch groups four text pairs from one XLM-R rerank request into a single EngineCore submission. It is active for the single-core pooling path without LoRA. Other request shapes follow the existing per-pair submission path. The profile has a 240-second startup budget to allow for compilation and graph capture.

## Lab validation

The lab used one 96 GiB ZW810E, Linux amd64, PPU driver 1.6.1-f3016a, Docker 26.1.3, HGGC 13.0, vLLM 0.23.0 and PyTorch 2.10.0. The runner image config ID was `sha256:50452109e67a5d12439d52fab90083d9d9c0c867f6239d8d4601070ac84fd7c7`. The ModelScope revision is pinned in the catalog. Six fixed cases each used four short English or Chinese documents and 80–124 prompt tokens per request.

| Change | Result |
| --- | --- |
| Eager → tuned graph sizes | Serial p50 24.21 → 11.56 ms with shared CPU scheduling, and 24.60 → 13.29 ms with CPU affinity. All eight interleaved repeat blocks improved by at least 35%. An earlier experiment included a pair with a 7.9% gain. The runtime probe observed 450 successful graph replay calls. |
| Separate submissions → one submission for four pairs | With the same graph sizes, serial p50 10.58 → 6.93 ms and p95 14.06 → 8.84 ms. Twelve single-request probes yielded 23 → 12 engine iterations across the requests, with equal prompt-token counts. All 384 matched rankings agreed. Maximum absolute score delta was 0.000671. |
| One API waiter for result collection | p50 improved 1.5% in a separate experiment. The package uses the existing collector. |

These were separate comparisons. The graph and batching gains should be evaluated together on the intended traffic mix. Startup and first-request latency also matter. In separate stage profiles, `/health` was ready in 88–136 seconds. The first rerank after readiness took 1.19–2.26 seconds. The app does not perform an automatic rerank warmup.

## Business traffic acceptance

Use a representative peak and off-peak window. The caller can count documents and `top_n` at serialization time and record total prompt tokens when the engine reports them. For per-pair length and truncation, sample lengths after the engine's existing tokenization. Carry relative arrival times or per-second request counts, in-flight requests, completion code, and caller-side response time. Export counts and buckets without query text or document text. GPUWorks gateway records can supply request timing and outcomes for routed traffic. The current gateway usage registry does not parse rerank token usage.

Use the joint distribution of documents per request and pair lengths, keeping the peak arrival pattern in replay. Compare steady-state p95/p99, successful requests and text pairs per second at the application's latency budget, timeouts, and the first response after rollout. Use the application's approved ranking judgments for quality, such as nDCG@K or recall@K. Validate through a GPUWorks import and a staging deployment before switching business traffic.
