# Strata DeepSeek CUDA runner

This recipe preserves DeepSeek support on **Strata v0.2.13**, source commit `d2f096475817d4666c364c820b0ca830f751cfc5`.
It rebuilds the **0.1.59 native engine**; 0.2.13 is the application release, not the engine version.
Source and dependency pins are recorded in [source-manifest.json](source-manifest.json).
Later upstream application/setup versions no longer start DeepSeek; this runner prepares and serves the pinned model independently.
It is separate from the Qwen runner: the Qwen MTP/vision assets and old queue patch do not apply.
The image serves ISTA-DAS 2.00 bpw text requests with the upstream OpenAI-compatible API.
The six external GGUF files and one metadata GGUF bundled in the image are pinned, including sizes and SHA-256, in
[model-manifest.json](model-manifest.json). Their total is 256,816,878,701 bytes.
The metadata file contains no weight tensors and is not registered as an independent model.
The runtime never calls setup or launch and never downloads missing components.

## Build

Run large downloads and builds on the authorized build machine, in its task directory.
Use an existing authorized container runtime with an explicit endpoint. This recipe needs no additional Docker daemon.
Check free memory, storage and existing tasks first; limit the build's total memory, swap, runtime and compiler jobs.

Prepare this directory as the build context with one ordinary Git checkout:

- `metadata/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00001-of-00009.gguf`: the 5,780,589-byte metadata file from
  `unsloth/DeepSeek-V4.1-Flash-1017-GGUF`, revision `8a6ec9cf118e785a362ea94ee123c61c70553be6`,
  path `UD-Q8_K_XL/DeepSeek-V4.1-Flash-1017-UD-Q8_K_XL-00001-of-00009.gguf`.
  SHA-256: `cc15d470afcb030558bbc4d8e2ccd840182e2e95de1a95198138efe63a5fd99f`.
- `upstream/`: `https://github.com/Niko1221/Strata.git`, commit `d2f096475817d4666c364c820b0ca830f751cfc5`.

Keep its `.git` directory: the Dockerfile verifies the commit identity and rejects tracked source modifications.
GGML is already vendored in `third_party/strata-ggml`, frozen at llama.cpp commit
`3cf03257f219afbe7334045ff7c6a06ac68c627d`. Both recipes verify the pin and run upstream's
`tools/check_vendored.py` to check every file against its manifest. They use this default CMake source;
no external llama.cpp checkout or `STRATA_GGML_DIR` override is needed.
The vendored tree includes DeepSeek's CPU lane and the CUDA MMQ headers and quantization instances.
The text-only pack tool uses the Python standard library; the separate vision reader and setup's
gguf-py path, which would need the full llama.cpp repository, are not used here.
Do not put the six external weight files in the build context. The small metadata file is the only model input bundled in the image; both recipes check its size and hash. The CUDA build/runtime bases can be overridden with digest-pinned mirrors through
`CUDA_BUILD_IMAGE` and `CUDA_RUNTIME_IMAGE`; their defaults are NVIDIA CUDA 13.0.0 on Ubuntu 24.04.

```sh
docker --host unix:///var/run/docker.sock build \
  --build-arg BUILD_JOBS=4 \
  --tag quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.59-deepseek41-amd64-r4 \
  --file Dockerfile .
```

The build targets only `strata-dsv41`, with portable AVX2 CPU code and CUDA SASS for sm75, sm86 and sm89,
plus compute89 PTX. The upstream release DeepSeek package only includes sm86/sm89/sm120;
sm75 here is a source build, whose kernels include the pre-sm80 fallback. Compilation alone does not establish
successful inference on Turing or mixed GPUs. CUDA 13 requires NVIDIA driver 580.65 or newer.

For an offline reproduction where the CUDA development image cannot be fetched,
[Dockerfile.cached](Dockerfile.cached) uses the immutable r3 runner's cached compiler and Python environment.
It replaces its source tree and rebuilds the native engine and libraries from v0.2.13.
It verifies `source/strata-archive` against the required
`STRATA_ARCHIVE_SHA256` build argument before extracting it. The argument must also equal the pinned archive hash; a different archive cannot acquire the fixed source identity.
Obtain the archive from the exact Strata commit in `source-manifest.json` and retain its download/hash evidence.
The recipe records `BUILD_ID`/`SOURCE_REVISION` for archive source; it does not invent Git metadata.
It replaces all old source and the launch entrypoint, retains the existing virtual environment,
and checks its imports and package consistency without downloading dependencies.
This recipe retains the r3 image's original layers and a compiler-output layer, and must be identified separately in build evidence.
Compilation and packaging are separate steps so a packaging failure can reuse the compiled output.
For this candidate, a bounded container replayed the cached recipe's source checks,
native compilation and packaging, then committed its completed filesystem once.
The resulting image passed offline qualification; the classic Docker builder attempt did not complete.
The clean multi-stage recipe has not been built in this validation.
Both recipes explicitly bundle `libstrata_kq.so`, as upstream's CUDA package does:
the optional K-quant prompt path loads it with `dlopen`, so an ordinary linked-library dependency scan does not find it.

The older adapter-only incremental recipes are not used for this release: they would retain a native engine built from a different source revision.

## Mount and launch contract

GPUWorks supplies the callable arguments `--model PATH --host HOST --port PORT`.
`--model` identifies the directory containing the first IQ2_XXS shard. The other five external component directories are named by:

| Environment variable | Component |
| --- | --- |
| `STRATA_DSV41_SHARD2_DIR` | IQ2_XXS shard 2 |
| `STRATA_DSV41_SHARD3_DIR` | IQ2_XXS shard 3 |
| `STRATA_DSV41_SHARD4_DIR` | IQ2_XXS shard 4 |
| `STRATA_DSV41_ENGRAM_DIR` | IQ4_NL Engram |
| `STRATA_DSV41_DSPARK_DIR` | Q8_0 DSpark |

The pinned metadata is read from `/opt/gpuworks/strata-deepseek/metadata/` by default and its full SHA-256 is checked at each start.
An explicit `STRATA_DSV41_ENGRAM_META_DIR` directory override remains supported and must contain that same pinned file.
It requires no platform model record or auxiliary mount. Each of the six external mounts can be read-only. The entrypoint finds exactly one file with the manifest's filename beneath each directory,
checks its size and GGUF magic, and rejects missing, ambiguous or incomplete inputs before preparing state.
Full SHA-256 verification of the six large external files belongs to acquisition, so container restarts do not reread 257 GB just to validate downloads.

The entrypoint creates a small symlink farm and fresh pack under `/strata-data` (override `--data-dir`).
It exports indexes and tokenizer data using upstream's plain ISTA-DAS recipe and shipped hot-expert profile,
then checks the result with `strata-dsv41 --pack-info`. It does not rewrite or copy weights.
Preparing the pack inside the container records its actual mounted paths. Do not reuse a host-generated pack with other paths.
Preparation failure removes only that attempt's newly created state; a completed start retains its pack and logs for diagnosis.

The server runs with `--offline` and vision off. Defaults: 8K context, one request at a time, DSpark auto.
Options: `--context 8192..1048576`, `--parallel 1..8`, `--mmap-experts auto|on|off`, `--cpu-threads N`, `--ssd-cache GB`,
`--max-queue N` and `--queue-timeout 0.1..600`. `--ssd-cache` names upstream's RAM expert-store budget.
`--mmap-experts` is omitted from the native command by default, preserving upstream's automatic placement.
Explicit modes pass through unchanged: `on` keeps CPU experts backed by the model files, with caching still managed
by the engine; `off` copies as many as fit in the RAM available after GPU loading, retaining upstream's 8 GiB margin.
If they do not all fit, upstream copies the hottest subset, leaves the rest in place and reports that fallback.
`auto` explicitly requests upstream's automatic placement. The native engine may also adjust its prompt chunk,
buffers and expert placement for the selected mode.
The assigned `CUDA_VISIBLE_DEVICES` passes through unchanged; no config GPU list overwrites it.
Mixed-card tensor parallelism may be reduced by the quantization's block alignment; the engine can still split layers.

For a directly exposed backend, `--api-key-env NAME` reads its key from the named environment variable and writes
the runtime config with mode 0600. GPUWorks uses its existing authenticated gateway and internal agent access;
this adapter adds no new authentication protocol. Publish test ports on host loopback.
The default bind address is `127.0.0.1`; the platform explicitly supplies its container bind address.

The entrypoint replaces itself with the server. The API binds only after the engine reports `READY`;
the startup timeout is 3600 seconds. `/health` and `/v1/models` are available once serving.
The server provides streaming chat, reasoning, DSML tool calls and native metrics without the old queue asset.

## Small offline checks

```sh
python3 -m unittest discover -s build/runner-images/strata-deepseek -p 'test_*.py' -v
```

Run new cases serially under an OS memory/swap/runtime limit. These fixtures use tiny files and real short child processes,
cover bundled metadata defaults/hash failures and missing/ambiguous/truncated external components, preserve GPU assignment and read-only weight contents,
inspect the generated offline text configuration, and verify SIGTERM reaps a running pack child and removes partial state.
They do not validate GPU kernels or model output.
