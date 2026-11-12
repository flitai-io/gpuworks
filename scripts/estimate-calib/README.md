# VRAM estimate calibration

The engine reserve constants in `pkg/estimate` are measured here, not derived.
This directory holds the harness that measures them and the raw runs behind the
current values, so a later edit can be checked against the same evidence instead
of re-guessing.

## What is measured, and against what

`vllm serve --load-format dummy` loads a model with correctly shaped weights
without reading a checkpoint, then profiles its own memory and prints the KV
cache it can afford:

```
Model loading took 1.12 GiB and 1.745059 seconds
Available KV cache memory: 38.39 GiB
GPU KV cache size: 359,392 tokens
```

That is the ground truth. Everything compared here comes from the engine's own
report, never from an inference about what the engine should have done.

Because the loader never reads weights, a model directory needs only
`config.json` and its tokenizer, so a sweep costs minutes per model rather than a
download per model. `fetch_model.sh` pulls exactly that slice from ModelScope.

vLLM 0.25.1 states its arithmetic in `v1/worker/gpu_worker.py`:

```
available_kv = requested_memory - non_kv_cache_memory - cudagraph_estimate
requested_memory = cudaMemGetInfo().total * gpu_memory_utilization
non_kv_cache_memory = weights + torch_peak_increase + non_torch_increase
```

SGLang 0.5.15 sizes its pool differently
(`model_runner_kv_cache_mixin.py:_profile_available_bytes`):

```
available_kv = mem_fraction_static * (available - pre_model_load_memory) - weights
```

which is a budget-proportional reserve, not a profiled one. The estimator models
the two engines differently for exactly this reason.

## Running it

On a GPU host with docker:

```bash
scp run_one.sh calib_run.sh fetch_model.sh <host>:/tmp/calib/
./fetch_model.sh Qwen/Qwen2.5-7B-Instruct Qwen2.5-7B-Instruct
./calib_run.sh q25-7b-u90 /models/modelscope/Qwen2.5-7B-Instruct 0 0.9 4096
python3 parse_logs.py /tmp/calib/logs /tmp/results.json
python3 report.py /tmp/results.json          # estimator vs measurement
```

### The scripts

| script | what it is |
|---|---|
| `run_one.sh` | one vLLM measurement on a real GPU |
| `calib_run.sh` | `run_one.sh` plus the janitor that clears a stale runner's container first |
| `run_sglang.sh` | the SGLang twin of `run_one.sh` |
| `batch.sh` | the matrix | 
| `batch2.sh` | the architectures `batch.sh` did not reach |
| `batch3.sh` | the recovery sweep after a killed runner left a container holding 40 GiB |
| `batch_sglang.sh` | the SGLang sweep |
| `cloud_run2.sh` | one measurement on a rented single-GPU host, where vLLM runs directly |
| `cloud_batch2.sh` | the rented-A100 sweep |
| `debug_probe.sh`, `debug_probe7b.sh` | re-run at DEBUG level, which is the only way to make vLLM print its own decomposition |
| `node_exec.py` | drives a host through the platform's web terminal, for a node with no inbound SSH |
| `parse_logs.py`, `parse_sglang.py` | startup logs to rows |
| `report.py` | the table, via `gocalc` |
| `kv_layout.py`, `gen_catalog_kv.sh` | the catalog's `kv_bytes_per_token` table (CPU only, no GPU) |
| `verify_catalog.sh`, `verify_kv_values.py` | the two independent checks on catalog facts and on the generated table |
| `validate_catalog_dummy.py` | walks every seed model, downloads its ModelScope config, and validates NVIDIA/vLLM entries with dummy load; failures and non-NVIDIA entries are recorded and the sweep continues |

For a resumable catalog sweep, run `validate_catalog_dummy.py` on the GPU host
with Docker access. Use `--dry-run` to inspect discovery first, `--resume` to
continue an interrupted sweep, and `--retry-failed` to retry recorded startup
failures. The default 4,096-token max length keeps startup practical; it does
not change the model's KV layout. The output JSON contains one row for every
seed model, including explicit skip reasons for models without an
NVIDIA/vLLM template or a ModelScope source.

`report.py` compares against the shipped estimator through `gocalc`, which calls
`internal/server/estimate.Compute` directly, so the table is the product's answer
rather than a re-implementation of it.

### Runner image

The scripts default to the platform's own runner images
(`gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`, and
`cuda13.0-sglang0.5.21-r3` for `run_sglang.sh`); set `IMAGE` (or `--image` for
`validate_catalog_dummy.py`) to measure another engine version. They start the
engine with an explicit `--entrypoint`, so an image with or without its own
entrypoint works. The recorded results below and under `results/` name the
image each was measured with.

### Mirror1 vLLM 0.25.1 check (2026-09-14)

The existing `gpustack/runner:cuda13.0-vllm0.25.1` image was run on mirror1's
RTX 3080 Ti with `--load-format dummy`. ModelScope supplied the config and
tokenizer files; no model weights were downloaded for these checks. The image
ID was `f9c58b163ec7`.

| model | available KV | reported tokens | effective bytes/token | catalog vLLM |
|---|---:|---:|---:|---:|
| Qwen3.5-0.8B | 7.25 GiB | 629,145 | 12,373 | 49,152 |
| Qwen3-0.6B | 8.98 GiB | 84,016 | 114,766 | 114,688 |
| LFM2.5-350M | 9.24 GiB | 806,636 | 12,300 | 32,768 |

The raw measurements and command parameters are recorded in
`results/mirror1-vllm0251-20260914.json`. The result confirms that the current
image agrees with the catalog for ordinary attention, while the catalog
overstates vLLM KV cost for these hybrid attention models.

### Hybrid engine reconciliation (2026-09-14)

The first generator counted every layer for the vLLM column and only full
attention layers for SGLang. That was based on a truncated dummy-load result:
the layer override did not prove that recurrent layers belonged in the
token-indexed KV pool. Full dummy runs on mirror1 show the opposite for the
families that use `linear_attention` or `conv` blocks: Qwen3.5-0.8B measured
12,373 bytes/token and LFM2.5-350M measured 12,300 bytes/token, both within
one percent of their full-attention-only catalog values (12,288 bytes/token).

The generator now counts recognized attention blocks for both engine columns.
The catalog has been corrected for 29 models covering Qwen3.5/Qwen3.6/Qwen3.8,
LFM2.5, Ornith, MiniCPM-V and Intern-S2 variants. Two Qwen3-Next entries,
eleven Nemotron-H entries and one Step-3.7 entry remain intentionally unchanged
until their distinct or incomplete layer layouts receive a direct runtime
measurement; they are listed as outstanding engine discrepancies rather than
being inferred from the Qwen3.5 family.

## What the measurements changed

**The reserve is a measured table plus one computed term**, in
`internal/server/estimate/reserve.go`:

    reserve = base(engine, graphs enabled, architecture) + 0.75 GiB x utilization

The base is what the engine holds back besides the weights, and three things
decide which entry applies, each of them measured below: the engine (SGLang
carves its pool out of a fraction of the card rather than a profiled peak and
holds back almost nothing), whether CUDA graphs are on, and whether the
architecture captures attention inside those graphs. The second term is the
driver's slice at the top of the card, which is the one part that is spent in
proportion to utilization.

An earlier revision derived this per engine and model from the engine's own
reporting, and the derivation was wrong in the dangerous direction; a later one
collapsed it to a single 1.5 GiB constant, which was wrong in both directions at
once. The measurements below are what replaced both.

**The budget was too large.** An engine sizes its pool against
`cudaMemGetInfo()`'s total, which on every card measured is smaller than the
total the platform reads from nvidia-smi:

| card | nvidia-smi | engine reports | shortfall |
|---|---|---|---|
| Quadro RTX 8000 | 45.00 GiB | 44.29 GiB | 0.71 |
| RTX 2080 Ti | 22.00 GiB | 21.48 GiB | 0.52 |
| A100 80GB PCIE | 80.00 GiB | 79.25 GiB | 0.75 |

This is not subtracted from the card's capacity any more; it is part of what the
1.5 GiB reserve covers. Subtracting it separately double-counted it against the
engine's own startup peak, which was already in the reserve.

**CUDA graph capture is real memory and it is not a separate term.** Turning it
off with `--enforce-eager` and changing nothing else gives back this much KV
room, which is what the capture costs:

| model | graphs on | `--enforce-eager` | graph capture |
|---|---|---|---|
| Qwen3-0.6B | 359,392 | 360,016 | 0.07 GiB |
| Qwen2.5-7B | 458,256 | 472,528 | 0.76 GiB |
| Qwen3-8B | 170,000 | 176,896 | 0.95 GiB |

vLLM logs no `CUDA graph memory profiling` line for any of these, and its own
`cudagraph_estimate` term reads zero, so the cost lands inside
`non_kv_cache_memory` - it is the capture raising the torch allocator's peak,
not a bucket reserved beside it. The capture shape is identical across all three
(default `cudagraph_capture_sizes`, 1..512, 51 buckets), so what varies is the
activations each bucket holds, not the number of buckets. It tracks how wide the
model is rather than how many parameters it has, which is why a MoE should be
cheaper than a dense model of the same size; **nothing measured here has an
`--enforce-eager` twin for a MoE, so that is mechanism, not evidence.**

`max_num_batched_tokens` moves the same total. At 16384 instead of the 2048
default, with graphs on in both runs, the engine holds back 0.12 GiB more on
Qwen3-0.6B, 1.03 GiB more on Qwen2.5-7B and 0.32 GiB more on Qwen3-8B. That is
the capture plus a larger profiling peak together; the two are not separated
here, and it is the reason `vllm serve` on Hopper - where the default is 8192,
not 2048 - is still an open question for a 1.5 GiB reserve.

**A sliding window that is not in force no longer routes a model to the sliding
adapter.** Every Qwen2.5 config publishes `sliding_window` next to
`use_sliding_window: false`. The adapter this used to select was deleted along
with the rest of the KV derivation, so the reading is now inert; the parse is
kept because the catalog stores the config verbatim.

## Fit at the defaults

`vllm serve` defaults to `max_num_batched_tokens = 2048` on every card measured
(only >=70 GiB non-A100 parts get 8192), which is what the 1.5 GiB reserve
assumes. Against that default the estimator lands within 0.5% of the engine's own
KV token count across every dense model measured, on both Turing and Ampere:

| model | measured tokens | estimated | delta |
|---|---|---|---|
| Qwen3-0.6B (RTX 8000) | 359,392 | 360,167 | +0.2% |
| Qwen2.5-7B (RTX 8000) | 458,256 | 460,151 | +0.4% |
| Qwen3-8B (RTX 8000) | 170,000 | 170,762 | +0.4% |
| Llama-3.1-8B (RTX 8000) | 194,896 | 195,800 | +0.5% |
| Mistral-7B-v0.3 (RTX 8000) | 213,120 | 213,974 | +0.4% |
| Qwen3-0.6B (A100) | 653,424 | 655,079 | +0.3% |
| Qwen2.5-7B (A100) | 1,046,336 | 1,049,975 | +0.3% |
| Qwen3-8B (A100) | 398,704 | 400,138 | +0.4% |
| Qwen3-0.6B (SGLang, 2080 Ti) | 168,244 | 167,331 | -0.5% |

## Known residuals

These are measured, not theoretical, and each is a direction the constants do
*not* cover. `results/table.txt` is the full run.

- **MLA attention under-reserves by ~0.45 GiB.** DeepSeek-V2-Lite measures a
  0.89 GiB peak against the 0.39 GiB its projection predicts, so the estimate
  lands 5.4% high on KV tokens. The compressed-latent attention allocates
  workspace the projection does not describe.
- **A low `gpu_memory_utilization` shrinks the real reserve.** At 0.5 and 0.7 the
  measured reserve of a 7B/8B falls to ~0.30 GiB while the constants still claim
  ~1.1, so the estimate lands 5-9% *low*. The deploy form's slider starts at
  0.70, where the shortfall is 5.6%.
- **`--max-num-batched-tokens` above the default grows the reserve.** At 16384 a
  7B measures 2.13 GiB against 1.10 claimed, so the estimate lands 4.8% high.
  The platform leaves this parameter on Automatic, so the default path is the
  one calibrated.
- **`--enforce-eager` removes the peak entirely** (measured 0.28-0.33 GiB), so
  the estimate lands ~3% low.
- **Models whose `config.json` omits the attention shape** (gemma-3-4b-it) still
  yield no KV estimate; that is a config-extraction gap, separate from the
  constants here. The reserve itself does not go to zero for them: a config that
  names a width but no vocabulary is reserved as if the vocabulary were 150k,
  which over-reserves rather than under. A config with no width at all reserves
  only the context floor, and those reach the estimator through the catalog's
  observed per-device contract instead.
- **Tied embeddings are charged twice.** The artifact on disk stores
  `embed_tokens` and `lm_head` separately, but a `tie_word_embeddings` model
  keeps one copy resident: Qwen3-0.6B loads 1.12 GiB from a 1.40 GiB artifact.
  The estimator uses the artifact size, so it over-reserves by one embedding
  matrix on tied models. Not fixed here; the runs above pass the engine's
  measured weight figure so this is excluded from the table.

## Sweep coverage

RTX 8000 (45 GiB, sm_75) and RTX 2080 Ti (22 GiB, sm_75) on gpu3, plus one
rented A100 80GB PCIE. Architectures: dense GQA (Qwen2.5/Qwen3/Llama/Mistral),
MLA (DeepSeek-V2-Lite), sliding-window hybrid (gpt-oss-20b, gemma-3), MoE.
Axes: `gpu_memory_utilization` 0.5/0.7/0.9, `max_model_len` 4096/32768,
`max_num_batched_tokens` 2048/8192/16384, `--enforce-eager` on/off, tensor
parallel 1.

Not covered: ROCm, Ascend and PPU.

### Hopper holds back far more

Measured on a rented H100 PCIe (`results/hopper-h100pcie-vllm.json`), same model,
same dummy loader, two utilizations - which is what makes the card's own total
cancellable:

```
available_kv(u) = T x u - weights - reserve
available_kv(0.9) - available_kv(0.5) = 0.4 x T
```

| util | tokens | available KV |
|---|---|---|
| 0.5 | 146,496 | 20.12 GiB |
| 0.9 | 377,120 | 51.78 GiB |

Solving gives **T = 79.18 GiB** with no reference to any spec sheet, and the
reserve is **4.20 GiB at both utilizations**. The solve cross-checks: the card's
published total minus the driver shortfall measured on other NVIDIA parts is
78.90-79.13 GiB, within 0.05-0.28 GiB of the solved figure.

| card | reserve at util 0.9 | at util 0.5 |
|---|---|---|
| RTX 8000 | 1.10 GiB | 1.10 GiB |
| A100 80GB | 1.31 GiB | - |
| **H100 PCIe** | **4.20 GiB** | **4.20 GiB** |
| **B200** | **3.64 GiB** | **3.64 GiB** |

Blackwell was solved the same way, from its own two points
(`results/blackwell-b200-vllm.json`): a total of 178.32 GiB, and 3.63 GiB at
both utilizations. It lands in Hopper's bracket rather than above it, so the two
share one entry in the table - the 4.3 GiB there over-covers a B200 by 0.66 GiB,
which is inside the run-to-run variation anyway. The solved total is worth noting
on its own: 191.5 GB for a part sold as 180 GB is the same reporting convention
H100 shows at 85.5 GB for 80, and it is why the total is solved rather than
taken from a spec sheet.

The two columns are the same on purpose. The engine's holdback does not move
with utilization - measured, not assumed: see the note on run-to-run variation
below.

A single constant cannot cover this: 1.5 GiB under-reserves Hopper by about
2.7 GiB while over-reserving SGLang by about a GiB.

Turning the graphs off on the same card isolates the cause. Qwen3-8B at util
0.9, H100 PCIe, `--enforce-eager` changing nothing else:

| | tokens | reserve |
|---|---|---|
| default | 377,120 | 4.63 GiB |
| `--enforce-eager` | 395,344 | 2.12 GiB |

**CUDA graphs cost 2.50 GiB of it.** The rest, 1.66 GiB, is the engine's
non-graph holdback (1.24 GiB after the driver shortfall), against 0.33 GiB on
Turing - so Hopper is larger in both terms, and the second one is most likely
the attention backend's persistent workspace (vLLM's own docstring names
"buffers for some attention backends" as a non-torch cost).

Why Hopper and not the others: vLLM's capture mode is `FULL_AND_PIECEWISE`
everywhere, but the mode name is misleading. On the RTX 8000 the DEBUG log shows
**27 captures, all PIECEWISE and none FULL** - piecewise graphs cover everything
except attention, leaving attention to run eagerly beside the graph. A FULL graph
captures attention too, and there is one per capture size. Ampere lands in the
same bracket as Turing (1.30 vs 1.25 GiB), so the split looks like Hopper and
newer versus the rest, though that inference rests on magnitudes rather than on
an A100 DEBUG log.

Two explanations this rules out:

- **Not `max_num_batched_tokens`.** `vllm/config/compilation.py` caps
  `max_cudagraph_capture_size` at 512 by default, so the capture ladder is the
  same on every architecture. Raising mnbt from 2048 to 16384 on the RTX 8000
  moved the reserve by 0.32 GiB.
- **Not a larger driver shortfall.** Hopper's solved shortfall, 0.47 GiB, is
  smaller than the RTX 8000's 0.71.

And the reserve does **not** move with utilization. Two DEBUG runs on the RTX
8000, Qwen2.5-7B, report `Total non KV cache memory: 15.39GiB` at util 0.9 and
at util 0.5, digit for digit. An earlier revision of this file reported a
utilization dependence; that was two runs of the same configuration disagreeing
by 0.77 GiB, and the difference was the torch.compile cache - the DEBUG run had
no warm `/root/.cache/vllm`. **The engine's own holdback varies by ~0.8 GiB with
the compile-cache state**, which is the floor on how precisely a constant can
match it, and a fresh container is the cold case.

The reserve also moves with utilization on Turing (1.10 -> 0.33 GiB) but not on
Hopper (4.20 -> 4.20). A single constant cannot match both; over-reserving is
the safe direction, since the alternative is a deployment the platform admits
and the engine then cannot start.
