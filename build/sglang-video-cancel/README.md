# SGLang r2 video cancellation startup patch

Wan2.1's NVIDIA template keeps `gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2`.
Its `assets` bundle is mounted read-only and its `pre_start_script` installs this
backport before the original `sglang serve` command. No network or package
installation is needed during startup. Other templates do not receive the asset.
Existing deployments must be rebuilt to adopt the new template snapshot.

The cancellation protocol is backported from SGLang PR 47055, commit
`8b0f2f929f05a4eb447fe143ecd3ba54e688f33a` (Apache-2.0, see LICENSE.txt).
It cancels queued tasks and checks running tasks at denoising-step boundaries,
synchronizing tensor-parallel ranks. A merged batch stops only when all members
are cancelled. An unavailable or congested cancellation channel returns HTTP 503
and Retry-After: 1 without deleting the job. Multi-node/disaggregated execution
and final decoding/saving are outside this best-effort cancellation guarantee.

## Reproduce and test

```sh
python3 -B build/sglang-video-cancel/test_install.py
python3 -B build/sglang-video-cancel/build.py --check
```

`tests/r2-sources.tar.gz` contains the 11 unmodified files extracted from the
x86_64 r2 image, local image ID
`sha256:055618727df3f9baf4ecdb1240e73e2ee4085f318b606f8c93a8eb9a79a6c24c`.
The builder applies `video-cancel.patch` with exact context, compiles the results,
then deterministically packages the 15 changed/new files, before/after SHA256
manifest, installer and license. Generated replacement files are not maintained
separately. Run `python3 build/sglang-video-cancel/build.py` after changing inputs
and update the asset's SHA256/size in the template.

The installer validates all inputs before writing, rejects unknown source
versions or read-only/symlink targets, and replaces files atomically to break
udocker's shared image hardlinks. Repeated starts perform no writes; interrupted
starts resume only when every file matches its reviewed old or new hash. A
failure prevents SGLang from starting. Logs report success or the failing file.

【该逻辑为动态加载，需要人工核对】The installer resolves the installed `sglang`
package with `find_spec`; SGLang selects pipelines at runtime. Actual-image CPU
and GPU validation must confirm the installed paths and Wan2.1 pipeline. Hashes
reject an unverified r2 variant rather than silently running without the fix.

The 65 CPU/transport regressions require the real r2 SGLang dependencies:

```sh
python3 /opt/gpuworks/sglang-video-cancel/apply_video_cancel.py
python3 -m pytest -q /tests/test_api.py /tests/test_execution.py /tests/test_transport.py
```

Mount the unpacked asset at `/opt/gpuworks/sglang-video-cancel` and `tests/` at
`/tests`, both read-only. Validate real generation, in-flight cancellation and
subsequent generation separately; CPU tests cannot establish GPU termination.
