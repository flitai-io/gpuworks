# gpuworks-runner self-built image build repo

The matrix is defined in [matrix.yaml](matrix.yaml); for plans and evidence see the private artifacts repo, [artifacts/work/2026-10-03-runner-images/plan.md](../../artifacts/work/2026-10-03-runner-images/plan.md).
Some vLLM patches under patches/ carry third-party attribution; see [NOTICE](NOTICE). The Apache License 2.0 text is in [licenses/Apache-2.0.txt](licenses/Apache-2.0.txt).

## Layout

- `matrix.yaml` — target matrix (vendor × framework tier × engine × architecture × base × requires × revision)
- `cuda/` `cann/` `dtk/` `hggc/` — per-backend `Dockerfile.{vllm,sglang}` (ARG-driven; the base is injected by the matrix)
  There is also `cuda/Dockerfile.vllm-v100`: a dedicated line for V100 / V100S (sm_70) that installs the 1Cat-vLLM release wheel
  and does not use the upstream vllm image as its base (the upstream prebuilt kernels do not include sm_70); its matrix entry is marked `catalog: false`, so it is only built, verified and published,
  and never enters the system image catalog; L4 test cases reference it explicitly by card model.
  If the build machine cannot reach GitHub, put the release wheel in a local HTTP directory (`<url>/v1.5.1/1cat_vllm-1.5.1-cp312-cp312-linux_x86_64.whl`),
  and use the build machine's local copy of the matrix to point that entry's `ONECAT_RELEASE_URL` at it; the SHA-256 check still applies as usual
- `shared/` — scripts shared by build and verification
  - `assert_requires.sh` — the single implementation of "what requires means": used both at the end of the build and by verify
  - `apply_patches.sh` — strict patching (dry-run first, no fuzz, no leftover .rej/.orig)
  - `check_clean_history.sh` — checks the image config/history for credential-shaped strings
  - `check_vendor_base.sh` — checks the base for traces of the company's internal re-packaging (ZStack/AIOS markers);
    **must run before `prepare-base.sh` squashes the history**, since afterwards there is nothing left to find
  - `stage_base.py` — "registers" an upstream base into the local mirror, with zero transfer for layers that already exist (see below)
- `overlays/` — model overlays and offline audit scripts (installed into `/opt/gpuworks/overlays/` for specs to reference)
- `patches/vllm/`, `patches/vllm_omni/`, `patches/vllm_dtk/` — source patches for vLLM (cuda tier), vllm-omni, and the Hygon dtk vllm tier
  (per-file descriptions for both are in "patches/ and overlays/ file-by-file notes" below)
- `build.sh` — build driver ｜ `verify.sh` — finished-image verification ｜ `push.sh` — copy between registries
  ｜ `publish.py` — publish to the three public registries (see "Publishing" below)
- `prepare-base.sh` — squashes the history of a vendor base (credential scrubbing, done only at the base layer)
- `base-mirrors.example` — sample of the build machine's local base mapping (see below)

## Key conventions

1. **Tags are immutable**. Each entry has a `revision` (r1, r2, ...) and produces
   `<id>-<revision>` as the authoritative tag, plus a bare `<id>` as a human-friendly alias.
   The inference catalog (seed catalog) only references revisioned tags. When a recipe changes, bump the revision; never overwrite in place —
   a node that already has an image with the same name will not re-pull it, so overwriting in place means the change never takes effect.
2. **The matrix `base` is always a reproducible upstream source**. When the vendor registry is unreachable or only provides a tar,
   use the build machine's local `base-mirrors.local` (not checked into the repo) to map it to a mirror explicitly:

   ```bash
   cp base-mirrors.example base-mirrors.local   # then fill in according to the actual local mirror
   ```

3. **Upstream bases do not need to be downloaded again**. From gpu3, the various docker.io mirrors measured only 0.1~0.6 MB/s
   (daocloud and 1ms.run were both tried); pulling a 10 GB cuda base takes more than 4 hours. But as long as the local
   registry already holds a finished image built from the same base, **every layer of the base already exists there as a blob**
   (image layers are content-addressed; the first N layers of the upstream base are exactly the first N layers of the finished image). `shared/stage_base.py` uses cross-repository
   blob mount to link them into the target repository and only adds a config of about 40 KB, with digests verified one by one against upstream:

   ```bash
   export https_proxy=... no_proxy=<local mirror>   # small objects go through the proxy to fetch from docker.io
   python3 shared/stage_base.py vllm/vllm-openai:v0.30.0 \
       <mirror>/vllm/vllm-openai:v0.30.0 --insecure --mount-from gpuworks-runner
   ```

   If any layer is neither in the target repository nor mountable, it errors out — only then do you need a real skopeo pull.
4. **If declared, it must be present**. For a component listed in `requires` (omni/mooncake/diffusion/
   audit/hcu_patches), an assertion failure at the end of the build means no image is produced; there are also "if declared, it must be absent" checks:
   for the Ascend omni tier, `omni_no_mooncake` (the pip package `mooncake-transfer-engine-npu` must not be present,
   see "Ascend omni tier" below) and `omni_patched` (the vllm_omni patches have landed); `verify.sh` re-verifies against the
   image's own `/opt/gpuworks/image.json`. There are no more `|| echo WARN` style tiers
   where the name says one thing and the contents say another.
5. **Patches are applied strictly**. A context mismatch fails the build (see the top of `shared/apply_patches.sh`
   for the reason: `--fuzz` can make an append-only patch land in the middle of a function body while returning 0).
   The cuda vllm tiers all use `EXPECT_ALL=1`; a patch that genuinely does not apply to some tier's base must be declared explicitly by the matrix's
   `PATCHES_NOT_APPLICABLE` (not quietly let through via `EXPECT_ALL=0`).
6. **Build-time configuration is not left behind for runtime**. The pip/apt mirrors are `ARG`s and are restored after the build; they are not written into the
   image ENV.
7. **The history of a vendor base may contain credentials**. The image history of vendor vanilla bases (asllm, sourcefind dtk)
   carries plaintext internal-network credentials, and the history is distributed with the image to every registry. For tiers marked
   `base_requires_clean_history: true` in `matrix.yaml`, first squash the base's history with `prepare-base.sh`,
   then point `base-mirrors.local` at the cleaned ref; before building, `build.sh` validates the
   effective base and rejects it outright if it is not clean, and `verify.sh` re-checks the finished image.
   **Squash at the base layer, not on every final image**: measured, the two hggc tiers share all of the asllm layers
   (sum 50.4 GB / union 25.4 GB), and squashing each one would break that 50% deduplication into two independent single-layer copies.

## Build machine setup (one-time)

```bash
docker buildx create --name gpuworks-builder --driver docker-container --use
docker run --privileged --rm tonistiigi/binfmt --install arm64   # cross-architecture
docker login "<target registry host>"                             # push credentials
cp base-mirrors.example base-mirrors.local && vi base-mirrors.local
```

## patches/ and overlays/ file-by-file notes

Patches are applied strictly by `shared/apply_patches.sh` (dry-run + `-F0`, no `.rej`/`.orig` left behind);
the manifests are written into the image as `/opt/gpuworks/patches-vllm.json` and `/opt/gpuworks/patches-omni.json`,
and `verify.sh` and the `omni_patched` check in `assert_requires.sh` re-verify them.

| Patch | Purpose | Applies to |
| --- | --- | --- |
| `patches/vllm/001_wrong_dp_ray.patch` | Adds the missing `_next_port` module-level variable to `get_open_port` in `vllm/utils/network_utils.py` (under DP + Ray, getting a port raises `NameError`) | cuda vllm tiers. **No longer applies on vllm 0.31, but only because upstream reflowed the `get_open_port` docstring** (vllm-project/vllm#52136); the port issue itself is not fixed upstream. The `cuda12.9-vllm0.31.0` tier currently declares it explicitly with `PATCHES_NOT_APPLICABLE` in the matrix args, and the manifest records `001_wrong_dp_ray.patch:declared-not-applicable`; the patch must be rebased onto 0.31 at the next revision. A stale declaration (the patch actually applies) also fails the build |
| `patches/vllm/002_mooncake_prom_metrics.patch` | Adds Prometheus metrics export to the mooncake KV connector (upstream only has internal statistics, which platform monitoring cannot read) | cuda vllm tiers |
| `patches/vllm_dtk/001_mooncake_report_failed_pulls.patch` | In the Hygon vllm 0.21 base, vLLM's MooncakeConnector records requests whose pull failed as completed just like successful ones, and does not implement `get_block_ids_with_load_errors`: decode answers anyway with KV it never received (200 + garbage), `kv_load_failure_policy` has no effect, and the failed blocks even remain in the prefix cache. Following NIXL's approach, the patch records the local blocks of failed requests and reports them to the scheduler (measured on K100: `fail` returns 500, `recompute` recomputes the correct result) | dtk vllm 0.21 tier (from r2, matrix `DTK_VLLM_PATCHES=1`); after patching, rerun `vllm-hcu-apply-patches` to confirm the change is still in place |
| `patches/vllm_dtk/002_mooncake_hybrid_gdn.patch` | PD for GDN/Mamba hybrid models (Qwen3.5/3.6/3.8) with the Hygon vllm 0.21 MooncakeConnector. Unpatched, the connector handles full attention only: registering a Mamba layer's state fails an assertion, the transfer topology probes the GDN backend's KV cache shape (`NotImplementedError`), and the forced HND layout makes a hybrid model's attention compute garbage even without a transfer. With the hybrid KV cache manager every KV cache tensor is pages of one size shared by one layer of each group, so the patch registers each tensor once and copies whole pages by block id (equal TP and PP size 1 only; other pairs fail the request), keeps the backend's default layout for hybrid models, drops the null placeholder blocks of Mamba groups, and, as the in-tree NIXL connector does, has prefill compute N-1 prompt tokens while decode recomputes the last one. Requires `--no-disable-hybrid-kv-cache-manager` (the connector refuses to start a hybrid model without it). Measured on K100 (Qwen3.5-0.8B, hygon/Qwen3.8-27B-Channel-INT8-w8a8): output identical to a non-PD engine, N-1 tokens counted as `external_kv_transfer` | dtk vllm 0.21 tier (from r3), applied after 001 |
| `patches/vllm_dtk/003_scheduler_hma_invalid_blocks.patch` | vLLM 0.21's scheduler unpacks exactly one block-id list per request when a connector reports failed blocks, so a failed pull on a model with several KV cache groups kills the EngineCore (`too many values to unpack`). The groups share one block pool, so the reported ids still identify requests; an affected request recomputes from its first token (a recurrent state has no valid prefix to keep). Measured on K100: `fail` returns 500 and the engine keeps serving, `recompute` returns the correct text | dtk vllm 0.21 tier (from r3) |
| `patches/vllm_omni/001_omni_bypass_non_omni_loads.diff` | vllm-omni unconditionally replaces vllm's internal types such as `EngineCoreOutput` at import time; this patch adds an `--omni` check so that vllm's behavior is unchanged when that flag is absent, letting the omni image also run ordinary text workloads | all omni tiers |
| `patches/vllm_omni/002_ming_audio_dsp_npu_cpu_fallback.diff` | Ming-omni-tts's `audio_dsp.py` fails in two places on Ascend: `torch.hann_window` fails in `aclnnInplaceCos` for int64, and `F.fold` reports 507035 on the aivec vector core. The patch performs both steps on the CPU and moves the result back to the device (the tensors are just one audio chunk, so the overhead is negligible) | Ascend omni tiers (from r3) |
| `patches/vllm_omni/003_moss_realtime_ref_encoder_device.diff` | MOSS-TTS-Realtime's reference-audio encoding runs in the API server process: the codec has been placed on the code2wav stage's GPU while the waveform stays on the CPU, and `batch_encode` builds its input on the device of the first waveform, so the request fails with a device mismatch. The patch first moves the waveform to the device the codec lives on (see item 4 of "Model-specific dependencies and vllm-omni defects" below) | cuda omni tiers (from r4) and the 0.30 Ascend omni tier; the 0.23 Ascend omni tier does not have this class and skips it via an `OMNI_PATCHES_NOT_APPLICABLE` declaration |

| Overlay | Purpose |
| --- | --- |
| `overlays/audit/stack-audit.py` | **Offline component audit gate**: aggregates engine/dependency versions and `pip check` results into `/opt/gpuworks/components.json`; any non-empty errors fails the build. Derived from a colleague's image (see `NOTICE`); from r2 it adds `GPUWORKS_PIP_CHECK_BASELINE` — only `pip check` lines that "a clean base already had" are let through, and anything new beyond the baseline counts as introduced by us; there is also `GPUWORKS_PIP_CHECK_NODEPS` — it only lets through the `pip check` lines of packages declared per tier to be installed with `--no-deps` (such as voxcpm) |
| `overlays/audit/omni_diffusion_check.py` | Offline content check for the omni/diffusion stack (modules locatable, versions match the declaration); not equivalent to GPU acceptance |
| `overlays/audit/qwen3_tts_check.py`, `qwen3_tts_prepare.py` | Offline check for the small Qwen3-TTS tier, and "deriving a single-device config from upstream assets" |
| `overlays/audit/patch_sdxl.py` | Applies a bounded SDXL VAE fix to the pinned-version omni source; errors on source drift rather than silently publishing an unpatched image |
| `overlays/gpuworks-{diffusers,transformers,vision_remote}-runtime.tar.gz` | Adapters (`gpuworks_serve.py` + one adapter each) referenced by the Hygon callable tier in the inference catalog; unpacked and used by the spec |
| `overlays/vllm-omni-h3-48298030.tar.gz` | vllm-omni source overlay for MiniMax-H3 (the upstream image predates the merge of the H3 pipeline); content-addressed, mounted read-only by the spec after SHA-256 verification |
| `dtk/files/vllm-hcu-apply-patches` | Tool that the Hygon `pre_start_argv` depends on (extracted from the vendor `custom:*` images, see `dtk/files/README.md`); built into the dtk tier at build time |

## Reproducing a tier (from scratch to finished image)

```bash
# 0) One-time: see "Build machine setup" below; vendor/unreachable bases additionally need base-mirrors.local
cp base-mirrors.example base-mirrors.local        # fill in according to the local mirror

# 1) See how this tier would be built (touches nothing)
./build.sh --list
./build.sh --only cann9.1-910b-vllm-omni0.30.0 --dry-run

# 2) If the upstream base is not in the local mirror: register it first (existing layers transfer nothing; on failure fall back to a real skopeo pull)
python3 shared/stage_base.py quay.io/ascend/vllm-omni:v0.30.0     <mirror>/ascend/vllm-omni:v0.30.0 --insecure --mount-from gpuworks-runner

# 3) Vendor base (base_requires_clean_history: true in the matrix): check for internal re-packaging traces first, then squash the history
bash shared/check_vendor_base.sh <base-ref>       # must run before prepare-base.sh
./prepare-base.sh <base-ref> <clean-ref>          # then point base-mirrors.local at clean-ref

# 4) Build and push to the internal registry (multi-arch); assert_requires.sh runs at the end of the build, no image is produced if it fails
./build.sh --only cann9.1-910b-vllm-omni0.30.0

# 5) Re-verify the finished image (against the image's own /opt/gpuworks/image.json)
./verify.sh registry.zstack.ai/gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3

# 6) After it passes on real hardware, publish to the public registries and point the bare tag at it
./publish.py cann9.1-910b-vllm-omni0.30.0-r3 swr --dry-run
```

For what to look for in real-hardware verification and how far it has been taken, see [`artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md`](../../artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md).

## Common commands

```bash
./build.sh --list                                   # view the matrix (with revisions and requires)
./build.sh --only cuda13.0-vllm0.30.0 --dry-run     # only see how it would be built
./build.sh --only dtk26.04-vllm0.18.1 --no-push     # build into the local daemon
./verify.sh registry.zstack.ai/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2
./build.sh --revision-only r2                       # build and push all r2 tiers
./build.sh --only hggc13.0-vllm0.23.0 --squash      # vendor base needs its history squashed
./verify.sh --all                                   # re-verify all revisions in the registry
./push.sh --dest quay.io/gpuworks/gpuworks-runner    # copy to another registry
```

The `REGISTRY` environment variable determines the push target; the default is `registry.zstack.ai/gpuworks/gpuworks-runner`.

## Design trade-offs

1. **Slimmer tooling layer**: no build toolchain (cmake/gfortran/openmpi, etc.); everything uses prebuilt wheels,
   so images are smaller and builds are faster. gpuworks' use cases do not need a heavy toolchain.
2. **Merge strategy**: sglang+diffusion go into one image (the diffusion extras are purely additive; they do not squeeze the core
   dependencies or change engine behavior at import time); **vllm and omni are separate tiers**, because
   `vllm-omni` pushes `transformers` down to `<5.15` and pins `accelerate` to `1.12.0`,
   and merging them would put that upper bound on every workload in the image, while the nvidia vllm specs
   in the inference catalog are 517 ordinary : 27 omni. The omni tier is additionally published as `<framework tier>-vllm-omni<version>`, consistent with the Ascend line's naming;
   both tiers share the same base, so the large layers are fully deduplicated within the same repository, and this extra tag costs only about 280 MB.
   There is no longer a diffusion/tts variant split.
3. **The Ascend line does not install PyPI's vllm-omni**: it was shown to crash the engine's registry-check subprocess
   (`artifacts/work/2026-10-03-runner-images/evidence/r1-verify.md` §2.2/§2.4); Ascend omni workloads use the `vllm-omni` tier
   (whose base ships a native NPU build).
   **Two treatments in the omni tier (from r3)** — measured by r2 on .32 (8×910B4, driver 26.1.1, real containers)
   (evidence `artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md` §6h):
   - **Remove the `mooncake-transfer-engine-npu` that ships in the base**. It is the root cause of the `corrupted size vs. prev_size` heap corruption at exit during `import vllm_omni` /
     `vllm --help` (triggered when `mooncake/*.so`, `libcann_hixl.so`, `libllm_datadist.so` and `libibverbs` are loaded in the same process
     as vllm_omni; `import mooncake` alone does not crash), and it also makes vLLM's model-architecture introspection subprocess SIGABRT, so Ming-omni-tts and
     Qwen3-TTS cannot start. `quay.io/ascend/vllm-omni` has carried it since v0.24.0; v0.23.0rc1 does not. The omni tier therefore
     sets `MOONCAKE` to empty, drops `mooncake` from requires, and adds `omni_no_mooncake`.
   - **NPU patch for Ming-omni-tts** (`patches/vllm_omni/002`): in `audio_dsp.py`, `hann_window`
     (int64 `aclnnInplaceCos` fails) and the two `F.fold` calls (aivec vector core 507035) are computed on the CPU instead.
   - Note that omni is an editable install in the base (`/vllm-workspace/vllm-omni`); the patch target is located via
     `find_spec('vllm_omni')`, not in site-packages.
   - Recommended baselines: fleets with the old driver (26.0.rc1) use `cann9.0-910b-vllm-omni0.23.0rc1` (the upstream original has no mooncake and
     uses CANN 9.0.0); for HDK ≥ 26.1.1 use r3 of 0.30.0. Ming in v0.25.0 has a separate `lm_head` defect; do not use it.
4. **Engine executables carry their own env source** (Ascend line): as soon as a gpuworks spec declares
   an `entrypoint`, it overrides the image ENTRYPOINT, and the chain that does `source set_env.sh` in the base breaks.
   Putting the source inside `vllm`/`sglang` themselves makes both cases work.
5. **The cu129 vllm tier explicitly replaces the torchcodec in the base** (`cuda12.9-vllm0.31.0`, evidence
   `artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md` §6e, §6e.1). The `torchcodec 0.17.0` in upstream `vllm/vllm-openai:v0.31.0-cu129`
   is a **CUDA 13 build** (`libtorchcodec_core*.so` depends on `libnvrtc.so.13`, while the image only has
   `.12`), so `import torchcodec` fails outright with `OSError`; and the CLI stage of `vllm serve` does
   `from torchcodec.decoders import AudioDecoder`, so it **cannot start on any hardware**. Cause: upstream installs dependencies with
   `uv --extra-index-url https://download.pytorch.org/whl/cu129`; the cu129 index's torchcodec only goes up to
   0.16.0+cu129, while PyPI's 0.17.0 (CUDA 13 by default on Linux) has a higher version and gets selected.
   **Fix**: after the other pip steps, `cuda/Dockerfile.vllm` installs
   `torchcodec==0.16.0+cu129` with `--no-deps` from the official cu129 index (torchcodec 0.14~0.17 all only require `torch>=2.11`, and vllm requires `>=0.14`),
   driven by the matrix's `TORCHCODEC_SPEC` / `TORCHCODEC_INDEX_URL`; other tiers do not pass them and do not run this step.
   **Per-tier exception to pip_guard**: `torchcodec` is still on the guard list, so this is not a global relaxation; the Dockerfile must declare it explicitly with
   `pip_guard.py replaced <name> <version> <reason>`; the declaration asserts that "the installed version equals the declared version and differs from the base"
   and writes `{from, to, reason}` into `/opt/gpuworks/base-replaced.json` in the image; `check` only lets through
   exactly the change "base -> declared version", and any other change (including the same package becoming another version) still fails; the build log prints
   `DECLARED REPLACEMENT torchcodec: 0.17.0 -> 0.16.0+cu129`. **It is not a stub module**: it is the official cu129 wheel, and audio decoding takes the real path.
   **Gate**: `engine_import` in `assert_requires.sh` asserts `import vllm.multimodal.media.audio` for vllm tiers, and
   asserts `import torchcodec` when torchcodec is installed (both can be decided offline without a GPU); if this defect reappears the build fails.
   A comparison for the `-cu129-ubuntu2404` variant is in evidence doc §6e.1.
6. **ENTRYPOINT is otherwise left as upstream has it**: when gpuworks deploys, it injects argv according to the spec.
7. **Clean a vendor base's history before using it**: the image
   history of vendor vanilla bases (asllm, sourcefind dtk) carries internal-network credentials, and the history is distributed with the image. Use `prepare-base.sh` to squash once at the **base**
   layer (not on every final image — measured, that would break the shared layers; the two hggc tiers have
   sum 50.4 GB / union 25.4 GB), and `build.sh` validates this before building.
8. **The callable runtime is not baked into the image**: the inference catalog's spec already pins the same asset by sha256 and
   the agent mounts it read-only at the same path, so the copy inside the image would always be covered up.
9. **The INT8 spec of the Hygon dtk0.21 tier must turn off vllm-hcu's custom quantized GEMM** (`dtk26.04-vllm0.21.0`, measured on real K100 hardware).
   This tier's base is the Hygon vanilla, which ships the `vllm-hcu` platform plugin, and the plugin enables `VLLM_HCU_USE_CUSTOM_QUANTIZATION_GEMM` by default;
   the compressed-tensors W8A8 INT8 of Qwen3-4B-Thinking-2507 outputs nothing but `!` on it, while the vendor `custom:vllm0210-*` images
   (which do not have this plugin) work correctly. The INT8 spec in the inference catalog therefore carries `VLLM_HCU_USE_CUSTOM_QUANTIZATION_GEMM: '0'`;
   BF16 is not affected. The image itself does not turn this switch off globally, so as not to affect other quantized workloads.

## Known TODOs

- `dtk/files/vllm-hcu-apply-patches` comes from the vendor `custom:*` images (see `dtk/files/README.md`).
- The cuda13.4 tier will be added once upstream ships a stable release; cuda12.8 no longer has an upstream base and cuda13.3 does not exist, so neither will be added.
- The three `cann9.1-*-vllm-omni0.30.0` tiers at r3: 910b has been verified on real hardware on .32 (driver 26.1.1); a3 (no A3 hardware) and
  950 (the container environment is gone) await re-verification, see `artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md` §6h.

## Model-specific dependencies and vllm-omni defects (2026-11 A100 template verification)

In the full NVIDIA template verification on A100, four models failed to start under the runner or failed requests. The first, second and fourth are fixed in the image
(omni tier r4, default vllm tier r4); the third only changes the template.

1. **VoxCPM2 is missing `voxcpm`** (installed in the omni tier from r4). When vllm-omni 0.30's VoxCPM2 talker is constructed, it runs
   `VoxCPM.from_pretrained(model, load_denoiser=False, optimize=False)`, which looks up `VLLM_OMNI_VOXCPM_CODE_PATH`,
   `../VoxCPM/src`, and an installed `voxcpm`, in that order; in r3 none of the three exist, so it reports `No module named 'voxcpm'`.
   The upstream `vllm/vllm-omni` image does not ship this package either.
   - **Install `voxcpm==2.0.3` with `--no-deps`** (matrix `VOXCPM_SPEC`). The dependencies it declares on PyPI are heavy, including `gradio<7`,
     `funasr`, `modelscope`, `datasets<4`, `spaces`, `argbind`, `matplotlib`, and unbounded `torch`/`transformers`;
     installing by dependencies would be blocked by pip_guard and would also hit omni's `transformers<5.15` upper bound. The inference path actually imports
     `voxcpm.core`, `voxcpm.model.*` and `voxcpm.modules.*`, which only use torch, torchaudio, einops, pydantic, tqdm,
     transformers, librosa, huggingface_hub and safetensors, all of which the omni tier already has; the denoiser (modelscope) and text normalization
     (wetext, inflect) are lazily loaded when `load_denoiser=False` and normalization is not enabled by default.
   - The declared dependencies that are not installed make `pip check` report them missing. The Dockerfile records this package in `/opt/gpuworks/pip-nodeps.json`, and the audit gate
     (`GPUWORKS_PIP_CHECK_NODEPS`) only lets through the `pip check` lines for "this package at this version", recording them in `pip_check_nodeps` of `components.json`;
     if it is declared but not installed at that version, that fails too.
   - The `voxcpm` requires check goes through vllm-omni's own `import_voxcpm2_core()` and confirms that it can actually be found.
   - Measured (gpu3, Quadro RTX 8000): in r4 the talker is constructed successfully (`Loaded VoxCPM2Model`, `Loaded VoxCPM2 (dtype=torch.bfloat16)`).
     Producing audio could not be verified on this card: on Turing (cc 7.5) vLLM rejects bf16, and switching to fp16 mixes with voxcpm's native bf16 modules
     (`mat1 and mat2 must have the same dtype`). A full deploy+speech must be verified on Ampere or newer cards.
2. **ERNIE-4.5-VL is missing `decord` and `moviepy`** (decord is installed in the default vllm tier from r3, moviepy from r4; both
   ERNIE-4.5-VL specs use this tier). The decord error comes from the HF dynamic module loaded with
   `--trust-remote-code`: transformers' `check_imports` requires `import decord` to work.
   - **Install `decord2==3.4.0`, not PyPI's `decord`** (matrix `DECORD_SPEC`). The last `decord` release, 0.6.0, was published in
     2021 and only has an x86_64 manylinux2010 wheel, so it cannot be installed on arm64; `decord2` has the same import name, `decord`, and provides
     cp312 x86_64 and aarch64 manylinux_2_28 wheels, with ffmpeg bundled in `decord2.libs`; its only hard dependency is numpy,
     and it does not touch the torch stack. It is purely additive and does not change engine behavior at import time, so it goes into the default tier rather than a separate one.
   - The `decord` requires check uses ffmpeg to produce a 1-second synthetic video and asserts that 5 frames can be decoded with the correct shape.
   - Measured: running transformers' `check_imports` on the three remote-code files of the real model directory, r2's
     `processing_ernie4_5_vl.py` reports the same `requires ... decord` as on A100, and in r3 all three files pass.
   - **moviepy** (matrix `MOVIEPY_SPEC: moviepy==2.2.1`): in r3's real deployment on A100, it got past decord and then reported
     `No module named 'moviepy'` — the same dynamic module imports it only at runtime, which `check_imports` cannot see. It is installed with its dependencies, and the `moviepy`
     requires check asserts that it can be imported and that a `ColorClip` can be constructed. r4 passed deploy+chat on A100.
   - **Cost: pillow drops from the base's 12.3.0 to 11.3.0** (moviepy 2.2.1 requires `pillow<12`), and imageio, imageio-ffmpeg
     and proglog are added as well. pillow is not on the pip_guard list; the multimodal models on the default tier all go through it for image preprocessing,
     and r4 spot-checked a vision chat model with image input. Watch this upper bound when upgrading moviepy or the base.
3. **GLM-TTS reports "not a registered Omni pipeline": the image is fine, it is the template that needs changing**. vllm-omni 0.30 already ships
   a complete GLM-TTS implementation (`deploy/glm_tts.yaml`, `model_executor/models/glm_tts`, the TTS adapter). Pipeline auto-detection
   looks, in order, at `model_type` and `architectures` in the root `config.json`, then `model_index.json`, and finally matches the basename of the model path
   against registered names; the GLM-TTS repo has no `config.json` in its root (the LLM config is in `llm/config.json`), and gpuworks
   mounts the model at `/model`, so the basename `model` does not match `glm_tts`. The upstream example works because the path name contains `GLM-TTS`.
   `--hf-config-path` does not help: pipeline detection reads the model directory, not this argument.
   - Template fix: pre_start_script writes an overlay deploy yaml (`base_config:` pointing at the image's `glm_tts.yaml`, plus
     `pipeline: glm_tts`), and `--deploy-config` points at it. The `pipeline` field takes priority over auto-detection, and `base_config` merges
     stages/connectors. Verified offline in the omni image with only GLM-TTS's config and tokenizer files mounted read-only at `/model`:
     the bundled yaml reproduces the original error, and the overlay resolves to the two stages `glm_tts` → `glm_tts_dit`; stage 0's model config
     needs neither a root `config.json` nor `--hf-config-path` (vllm-omni injects `glm_tts` by `model_arch` and resolves to
     `GLMTTSConfig`, taking the tokenizer from `vq32k-phoneme-tokenizer`). Real-hardware deploy+speech is still to be verified.
   - The PPU spec likewise uses the bundled `glm_tts.yaml` and likewise mounts at `/model`, so it very likely has the same problem; this has not been verified.
   - If another omni model has "no `config.json` in the repo root" in the future, likewise specify `pipeline:` explicitly first.
4. **A MOSS-TTS-Realtime request fails with `mat2 is on cuda:0, different from other tensors on cpu`: a vllm-omni defect,
   patched with `patches/vllm_omni/003` from omni tier r4**. The service starts (both stages are healthy); the failure occurs at the step where the API server
   process encodes the reference audio. vllm-omni moves the Realtime codec (`MOSS-Audio-Tokenizer`) to the GPU where the code2wav stage
   lives (upstream #7724), but `MossRealtimeReferenceEncoder._encode_prepared` hands the CPU waveform as-is to
   `codec.batch_encode`, and the codec's remote code builds the input tensor on `wav_list[0].device`, so the weights are on cuda:0 and the input is on the CPU.
   This code is unchanged in 0.30.0, 0.32.0 and upstream main (2026-11-02), so upgrading the omni version does not fix it. The non-Realtime MOSS variants
   go through the processor's `encode_audios_from_wav`, which handles devices itself, so they are unaffected: in the same verification round MOSS-TTS, TTSD,
   SoundEffect and VoiceGenerator all produced WAVs. The patch moves each waveform to the codec's device before calling `batch_encode`;
   once upstream fixes it, it will be detected as already-present. The 0.23 Ascend omni tier does not have this class, and the matrix declares it not applicable explicitly with
   `OMNI_PATCHES_NOT_APPLICABLE`.
   - Measured (gpu3, Quadro RTX 8000, fp16 overlay, 3-second reference audio): r3 returns 500 with the same device mismatch as on A100;
     r4 returns 200 and a 3.04-second, 24 kHz WAV.
   - **Template caveat**: vllm-omni distinguishes MOSS variants by model path name (`realtime`, `local`, `ttsd`, `soundeffect`,
     `voicegenerator`; treated as `tts` when none match), not by config. With the model mounted at `/model`, Realtime is treated as `tts`, and the request fails with
     `'MossTTSRealtimeProcessor' object has no attribute 'model_config'`. A Realtime template must mount the model at a path whose name contains
     `realtime` (the A100 verification used `/models/MOSS-TTS-Realtime`), mount the codec in a separate directory, and point to it with
     `--hf-overrides '{"codec_model_name_or_path": "<codec directory>"}'`; otherwise under `HF_HUB_OFFLINE=1` it goes looking on HF
     for `OpenMOSS-Team/MOSS-Audio-Tokenizer`. The same applies to the other MOSS variants.
   - **On 24 GiB cards the talker stage's memory must be reduced**: the codec exists twice on the card — about 7.7 GiB in the code2wav stage, and about 7.2 GiB for the copy the API server uses to
     encode reference audio (not in any stage's budget). The image's bundled deploy config gives the talker stage 0.60, which OOMs on the first speech request on an RTX 4090 D;
     the global `--gpu-memory-utilization` only reaches part of stage 0, which is not enough. The template uses the placement
     resolver to set variables by per-card memory: < 22 GiB is rejected, 22–40 GiB has pre_start write a deploy overlay that only overrides stage 0 to 0.25,
     and ≥ 40 GiB keeps 0.60. On the 4090 D, 0.35 still OOMs and 0.25 produces a WAV.

## Publishing (internal registry -> public)

`publish.py` publishes `registry.zstack.ai/gpuworks/gpuworks-runner:<tag>` to Docker Hub / quay / Huawei SWR.
It uses only the standard library and is Python 3.6 compatible (runs directly on gpu3). An image = all of the upstream base's layers + a few hundred MB of thin layers,
so the key to publishing is **not re-uploading the base layers**:

| Target | Cross-repository mount (measured) | Conclusion |
| --- | --- | --- |
| Docker Hub | 201, works across namespaces (source `vllm/vllm-openai`) | for cuda tiers whose base is on Docker Hub, only the thin layers are uploaded |
| quay.io | 201, works across namespaces (source `ascend/vllm-ascend`) | for cann tiers whose base is on quay, only the thin layers are uploaded; the cuda tiers' base is on Docker Hub and cannot be mounted, so a full upload is needed |
| SWR | only blobs already in SWR can be mounted (same namespace 201); external base layers 202; a blob's HEAD is account-global | layers already pushed are skipped automatically, the first time needs a full upload; SWR rewrites the OCI index into a manifest list (the index digest changes, the per-platform manifest digests do not) |

```bash
# Report only: N blobs, of which M can be mounted, K need uploading, estimated upload size (no writes at all)
./publish.py cuda13.0-sglang0.5.21-r2 dockerhub --dry-run

# Real publish (credentials are only read in-process; --env-file - reads from stdin, so nothing need touch disk)
ssh gpu3 'cd /var/tmp/s7-publish && https_proxy=http://dev1.mytrix.me:7890 \
  no_proxy=registry.zstack.ai,192.0.2.156,192.0.2.15 \
  python3 publish.py cuda13.0-sglang0.5.21-r2 swr --dest-tag zz-pilot-... --env-file -' < secrets/registries.env
```

Flow: read the source index -> decide per layer (already at target / mountable / needs upload) -> mount -> `skopeo copy --all
--preserve-digests` (skopeo skips blobs the target already has) -> verify (index/per-platform manifest digests and all blobs).
skopeo credentials go through a 0600 temporary authfile that is deleted when done; logs redact credentials. SWR is automatically added to `no_proxy` (direct connection within China;
measured, going through the proxy was neither faster nor slower). Exit codes: 0 success, 2 arguments/configuration, 3 source read failure, 4 destination authentication failure, 5 skopeo
still failing after retries, 6 post-publish verification failure. `--dest-tag` defaults to the same tag as the source; a formal `-r2` tag must only be published after it passes real-hardware verification.

## Sync to Aliyun Drive (offline archive, mandatory after publishing)

The offline deployment doc ([`docs/how-to/deploy-offline.md`](../../docs/how-to/deploy-offline.md)) lets users download image archives from a shared Aliyun Drive directory. So **every time a runner tier is added or a revision is upgraded, after the public publish, the archive must be synced to the drive**, otherwise offline users will not get the new tier. The drive only holds the current version of each series.

- Location: `/gpuworks-images/<engine>-<framework>/` on enterprise drive 101; the directory names are the ones listed in the doc, such as `vllm-cuda/` and `sglang-dtk/`. File names are `gpuworks-runner_<tag>_linux-<arch>.tar.gz`; for multi-arch tiers, upload one copy per architecture.
- Export: on the build machine, export from the internal registry; the reference inside the archive must be in canonical form `gpuworks/gpuworks-runner:<tag>`, so users need not re-tag after importing:

  ```bash
  skopeo copy --override-arch amd64 \
    docker://registry.zstack.ai/gpuworks/gpuworks-runner:<tag> \
    docker-archive:/var/tmp/gpuworks-runner_<tag>_linux-amd64.tar:gpuworks/gpuworks-runner:<tag>
  pigz -9 /var/tmp/gpuworks-runner_<tag>_linux-amd64.tar
  ```

- Upload: use `pds-upload.py`. The credentials (`PDS_DOMAIN_ID`, `PDS_API_KEY`) are in the maintainer's local secrets. This key **can only upload and rename, not delete**, and a wrong upload cannot be undone, so: run `--dry-run` first to check the target directory and file name; the script refuses to overwrite existing files; do not upload test files. Chunks upload 16 in parallel (a single stream is under 1 MiB/s; 8 GB takes about 10 minutes); PDS requires every chunk of a parallel upload to carry a prefix SHA-1 state, which the script computes via libcrypto, so the build machine needs `libcrypto.so`. A mid-transfer failure leaves an invisible, undeletable incomplete session on the drive, so first verify the archive with `gzip -t` on the build machine and make sure the network is stable before uploading.

  ```bash
  env $(grep -v '^#' pds-ent-apikey.env | xargs) ./pds-upload.py gpuworks-runner_<tag>_linux-amd64.tar.gz vllm-dtk --dry-run
  env $(grep -v '^#' pds-ent-apikey.env | xargs) ./pds-upload.py gpuworks-runner_<tag>_linux-amd64.tar.gz vllm-dtk
  ```

- Old-version archives cannot be deleted; someone has to delete or move them manually on the drive's web page, and the commit message must note this.
- The Kubernetes agent image (`build/agent-image`) goes to `gpuworks-agent/` the same way, as `gpuworks-agent_<revision>_linux-<arch>.tar.gz` for amd64 and arm64, whenever `build/agent-image/REVISION` changes; export the published `gpuworks/gpuworks-agent:<revision>` with the reference kept in that canonical form.
