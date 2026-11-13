# GPUWorks Engineering Log

This document preserves the fix history, validation scope, and original evidence for each issue. Read each entry within the limits of its status and date;
“current” and “now” in an entry refer to the situation when it was written. Historical acceptance results alone do not establish the state of later versions.

## Playground, Gateway, and GPUWorks `/v1` Data Plane Acceptance

- Status: Partially complete (PARTIAL; Playground and GPU3 acceptance completed on 2026-08-20;
  multilingual audio translation and additional cloud coverage remain to be added)
- Date discovered: 2026-08-19
- Scope: Product functionality beyond direct vLLM startup

When this issue was discovered, GPUWorks had neither a Playground nor a separately deployed gateway. The server now
exposes a JWT-protected Playground through its built-in OpenAI-compatible `/v1/*` reverse proxy.
Public `/v1` API key authentication is unchanged; the management Playground uses an internal authentication entry point and never exposes
provider keys to the browser.

For the direct acceptance record of Qwen3-ASR vLLM on GPU3, see
`artifacts/archive/2026-08-18-roadmap/acceptance/omni-gpu3-vllm-real-acceptance-2026-08-19.md`. The following items remain open until their respective product functionality
is implemented:

1. [x] Build the Playground UI to cover chat, comparison, and audio transcription.
2. [x] Use the built-in proxy boundary to cover JWT/API key authentication, SSE flushing, multipart
   preservation, response headers, and binary bodies.
3. [x] Complete real GPUWorks Server + Agent acceptance on GPU3, including
   `/v1/audio/transcriptions` through the server. See
   `artifacts/archive/2026-08-18-roadmap/acceptance/playground-real-acceptance-2026-08-20.md`.
4. [ ] Add `/v1/audio/translations` coverage where the selected backend supports it, and
   repeat the corresponding checks on RunPod and Compshare.

## Fix udocker Host-Path Loopback Mapping for CUDA Consumers

- Status: Implemented (IMPLEMENTED, 2026-08-21); automated validation and real udocker
  acceptance with four images are complete (RunPod/A40, Qwen3.5-0.8B)
- Date discovered: 2026-08-19
- Scope: `internal/agent/runtime/udocker`
- Priority: Severe for runner images that reopen
  CUDA libraries using filenames exposed by `/proc/self/maps` during module import

In this entry, guest means the inside of the container, and rootfs is the container's root filesystem. Loopback mapping means creating symbolic links inside the container
so that the host paths a program reads from `/proc/self/maps` resolve to the same library files; it does not mean network loopback.

### Symptoms

The vLLM service starts and becomes healthy, but both the API server and engine processes print the following warning when probing the optional `deep_ep`
module:

```text
Module deep_ep was found but failed to import
FileNotFoundError: [Errno 2] No such file or directory:
'/opt/gpuworks/udocker/containers/<container-id>/ROOT/usr/local/lib/python3.12/dist-packages/nvidia/nccl/lib/libnccl.so.2'
```

The same mapping defect is fatal in newer GPUStack runner import paths. For example,
torchvision/FlashInfer may try to reopen a CUDA runtime library at a path such as:

```text
/opt/gpuworks/udocker/containers/<real-id>/ROOT/usr/local/lib/python3.12/dist-packages/torchvision.libs/libcudart.<hash>.so.13
```

When this guest path does not exist, vLLM's model architecture check fails before the engine starts. This is
independent of whether the selected vLLM version supports the model architecture.

Do not interpret this warning as evidence that NCCL itself is unavailable. All of the following checks
succeed in the affected container:

```text
guest_exists True
maps_host_path_exists_in_guest False
ctypes_load ok
torch_nccl_version (2, 29, 7)
```

vLLM also successfully initializes its distributed backend:

```text
world_size=1 rank=0 ... backend=nccl
```

The verification deployment subsequently reaches the `running` state, `/health` returns HTTP 200, and `/v1/models`
returns the model being served.

### Root Cause

`deep_ep.check_nccl_so()` reads the loaded NCCL filename from `/proc/self/maps`,
then compares that file with the NCCL library it was linked against. Under proot, `/proc/self/maps`
exposes the host-side rootfs prefix:

```text
/opt/gpuworks/udocker/containers/<real-container-id>/ROOT/usr/.../libnccl.so.2
```

This host path currently cannot be resolved inside the guest, so the `filecmp.cmp()` call fails at
`os.stat()`, before it can compare two valid NCCL libraries. vLLM catches
this optional import failure and continues without DeepEP.

GPUWorks already attempts to create host-path loopback symbolic links in `rootfsFixups()`,
but the generated path does not match the path exposed by `/proc/self/maps`:

```text
created:
  /home/udocker/.udocker/containers/<gpuworks-container-name>/ROOT

required:
  /opt/gpuworks/udocker/containers/<real-udocker-container-id>/ROOT
```

There are two independent mismatches:

1. The implementation hardcodes `/home/udocker/.udocker`, whereas the agent's actual `UDOCKER_DIR`
   is `/opt/gpuworks/udocker`.
2. The implementation derives link components from the named container symlink (`gpuworks-...`), whereas
   `/proc/self/maps` contains udocker's actual UUID directory.

Relevant code:

```text
internal/agent/runtime/udocker/helpers.go:rootfsFixups
internal/agent/runtime/udocker/helpers.go:containerRoot
```

Background evaluation reference: Section 6.1 of `udocker-offline/docs/udocker_evaluation.md` in the `runpod` research repository.

That evaluation requires a rootfs loopback at `$R/$UDOCKER_DIR/containers/$ID`, specifically
so that host paths read from `/proc/self/maps` resolve inside the guest.

### Impact

- Current single-GPU Qwen deployment: No functional impact; CUDA, NCCL, model loading, health checks, and
  inference service startup all succeed.
- General-purpose vLLM images that include DeepEP: Produce noisy warnings and disable DeepEP.
- DeepSeek MoE, expert-parallel deployments, or deployments that explicitly require DeepEP: May
  be materially affected; startup or optimized communication between experts may fail even when the underlying
  NCCL runtime is healthy.
- Other frameworks that resolve loaded shared libraries through `/proc/self/maps` may encounter the same
  host-prefix problem.

Do not treat suppression of this warning with `EP_SUPPRESS_NCCL_CHECK=1` as a platform-level fix. Suppression
skips a legitimate NCCL version safety check and hides the broken path mapping.

### Implemented Design

1. Resolve the selected named container's root with `filepath.EvalSymlinks` so that the canonical root
   contains the actual udocker container ID.
2. Build the guest loopback directory from the configured `d.udockerDir` instead of hardcoding
   `/home/udocker/.udocker`.
3. Create a link equivalent to:

   ```text
   <ROOT>/<UDOCKER_DIR-without-leading-slash>/containers/<real-id>/ROOT
     -> <canonical-ROOT>
   ```

4. Make creation idempotent: Validate an existing link before replacing it, and replace only that exact link;
   do not delete or rewrite paths belonging to unrelated containers.
5. Preserve the existing exact named-container selection logic used for the NVIDIA shim and `/etc/hosts`
   fixups.

The implementation also canonicalizes `d.udockerDir` itself to cover cases where the configured installation directory is
itself a symbolic link. It refuses to replace a loopback path that is not a symbolic link. The udocker
runtime supplies `FLASHINFER_DISABLE_VERSION_CHECK=1` and
`SGLANG_SKIP_SGL_KERNEL_VERSION_CHECK=1` as compatibility defaults, while allowing environment variable values for a specific
workload to override them.

### Automated Validation

New unit tests cover all of the following scenarios:

1. The named container path is a symbolic link to a UUID directory, matching the actual udocker directory structure.
2. The loopback is created under a nondefault `UDOCKER_DIR`, such as `/opt/gpuworks/udocker`,
   rather than `/home/udocker/.udocker`.
3. The loopback uses the actual UUID directory rather than the `gpuworks-...` name.
4. Repeated calls to `rootfsFixups()` are safe and preserve the correct link.
5. Existing stale or unrelated container directories are not modified.

At a minimum, run:

```bash
go test ./internal/agent/runtime/udocker
go test ./...
```

### Remote Acceptance Procedure

After deploying the fixed agent, use a newly created container; do not use an old rootfs as
evidence that provisioning has been corrected.

1. Confirm the named container and actual ID:

   ```bash
   ls -l /opt/gpuworks/udocker/containers/gpuworks-*
   ```

2. Confirm that the expected loopback exists inside the new rootfs:

   ```bash
   R=/opt/gpuworks/udocker/containers/<real-id>/ROOT
   readlink "$R/opt/gpuworks/udocker/containers/<real-id>/ROOT"
   ```

3. From inside the guest, verify that both the normal NCCL path and the host
   path exposed by `/proc/self/maps` resolve:

   ```python
   import ctypes
   import os
   import torch

   guest = "/usr/local/lib/python3.12/dist-packages/nvidia/nccl/lib/libnccl.so.2"
   loaded = next(
       line.split()[-1]
       for line in open("/proc/self/maps")
       if "libnccl.so" in line
   )
   assert os.path.exists(guest)
   assert os.path.exists(loaded)
   ctypes.CDLL(guest)
   print(torch.cuda.nccl.version())
   ```

4. Import DeepEP without suppressing its NCCL check:

   ```bash
   python3 -c 'import deep_ep; print(deep_ep.__version__)'
   ```

5. Start vLLM and confirm that the logs no longer contain:

   ```text
   Module deep_ep was found but failed to import
   FileNotFoundError: .../libnccl.so.2
   ```

6. Confirm that the deployment reaches the `running` state, `/health` returns HTTP 200, and `/v1/models`
   returns the expected model.

7. When suitable multi-GPU hardware is available, run a TP/EP smoke test to verify NCCL
   initialization and actual DeepEP operation, rather than relying only on a single-GPU import test.

### Final Four-Image Acceptance

The real single-GPU acceptance record is in
`artifacts/archive/2026-08-18-roadmap/acceptance/udocker-four-image-real-acceptance-2026-08-21.md`. It covers
both GPUStack runner images and two upstream images, including real model loading and
an OpenAI-compatible request. Future multi-GPU TP/EP testing remains valuable, but it is
independent of the udocker path mapping fix verified here.

### Evidence From the Initial Reproduction

The initial reproduction used:

```text
Agent commit: 04a21f7
Deployment: 7 (myqwen-entrypoint-verify)
Instance: 3a5af132f5e0462dc5b1a425f6bb9240
Container ID: f1982811-4c10-34dc-a942-84b7d6abf009
Image: vllm/vllm-openai:v0.27.1
GPU: NVIDIA A40
```

These identifiers are diagnostic evidence only; acceptance tests must use a container newly created by
the subsequently fixed agent.

## Ascend Device Injection for the nerdctl Runtime Driver

- Status: Implemented (IMPLEMENTED, 2026-08-28); device injection and basic partition allocation have been
  validated on real hardware at 198.51.100.32
- Date discovered: 2026-08-28
- Scope: `internal/agent/gpu/gpu.go` (`DriverAwareInjector`, `RuntimeMount`),
  `internal/agent/gpu/ascend.go` (`RuntimeInjectionForDriver`),
  `internal/agent/engine/engine.go` (`applyDeviceInjection`)

`internal/agent/runtime/nerdctl` (added on 2026-08-28) provides GPUWorks with a
general container lifecycle driver for containerd/nerdctl hosts. `198.51.100.32`
(`huawei-ascend-node`) has only `nerdctl 1.7.6` and no Docker daemon at all, so it
previously could not be brought under management. This general driver (run/stop/rm/status/logs/exec/pull/inspect/
save/load) is implemented, unit tested, and validated against real `nerdctl` on `.32`
(pull through a proxy, run, inspect, exec, and rm, with confirmation that containers never leak into nerdctl's default
`k8s.io` namespace).

Implementation details: `gpu.RuntimeInjection` gains a `Mounts []RuntimeMount` field and an
optional `DriverAwareInjector` interface
(`RuntimeInjectionForDriver(indexes []int, driverName string)`), allowing the prober
to branch by container runtime. `engine.applyDeviceInjection` dispatches to this interface when the prober
implements it; otherwise, it falls back to the existing `RuntimeInjection(indexes)`. It now
also propagates `inj.Mounts` into `WorkloadSpec.Mounts`.

`AscendProber.RuntimeInjectionForDriver` preserves the existing
`--runtime ascend` plus environment-variables-only path for all drivers except `nerdctl`. For `nerdctl`, it instead
directly passes through unrenamed `--device /dev/davinciN` devices (one for each selected index, plus
`davinci_manager`/`devmm_svm`/`hisi_hdc`) and five read-only host
driver/DCMI library bind mounts (`/usr/local/dcmi`, `/usr/local/bin/npu-smi`,
`/usr/local/Ascend/driver/{lib64,version.info}`,
`/etc/ascend_install.info`). There is no need to mount the full CANN toolkit, because the inference image
already includes it. `ASCEND_VISIBLE_DEVICES` is omitted under nerdctl (nothing
reads it without the Docker Runtime plugin); `ASCEND_RT_VISIBLE_DEVICES`/
`NPU_VISIBLE_DEVICES` are retained because ACL/torch_npu reads them directly.

Two real-hardware validations have been performed on Ascend pool members: The first manually ran `nerdctl run`
with this exact set of arguments against the cached
`quay.io/ascend/sglang:v0.5.14-cann9.0.0-910b` image
(`torch.npu.is_available() == True`, `device_count() == 1`). The second used
the actual production code path: `test/l4/cmd/ascend-nerdctl-verify` directly calls
`gpu.AscendProber.RuntimeInjectionForDriver` and the real
`internal/agent/runtime/nerdctl.Driver` (Run/Exec/Remove). It is executed by
`TestAscendNerdctlDeviceInjectionEndToEnd`
(`make test-l4-ascend-nerdctl-devinject`, integrated into the `l4-ascend-pool-partition-inference`
CI job). The target machine's network link (measured on `198.51.100.31`: aarch64, nerdctl available, no docker,
no Go) sustains only about 45 KiB/s for bulk transfers (unrelated to this code; even a raw
`ssh ... cat >` pipe of just a few MB stalls like `scp`). The test therefore sends a gzip-compressed
binary through a regular SSH stdin pipe instead of using `scp`; it never
invokes `go build` on GPU nodes.

2026-09-15 revision: This helper was previously checked into the repository as a gzip asset of about 9 MiB
(`test/l4/testdata/ascend-nerdctl-verify-linux-arm64.gz`), with
`make check-ascend-nerdctl-verify-asset` in core CI using “cross-compile again + byte-for-byte
comparison” to determine whether it was stale. The rule itself was sound, but its cost outweighed its benefit: The helper links
a large set of agent packages (accelerator, proto, agent/gpu, agent/runtime,
runtime/nerdctl, agent/engine, assets, engines, catalogasset, enginelog).
Since its introduction, the dependency closure has seen 48 commits, and the asset has changed 20 times (8 of those commits were titled “regenerate
asset”). In addition, **any rebase invalidates the regeneration in that commit**, so every MR
had to carry a gate for an artifact used only by an L4 test, while history accumulated 24 blobs (about 202 MB).

Now, the L4 test cross-compiles the helper on the runner from the revision under test
(`ascendNerdctlVerifyLinuxARM64Gzip`, 1–2 seconds with the cache populated). The checked-in asset, byte-comparison
gate, and `build-asset.sh` are all removed; core CI no longer checks the asset. The L4 lane adds
`TestAscendNerdctlVerifyHelperBuildsForTheFixture` solely to prove that the helper still cross-compiles
for the fixture. The former `.gz`/toolchain reproducibility prerequisites (compare payloads, not gz files; pin
`GOTOOLCHAIN` to the version in `go.mod`) no longer apply, because they addressed
the problem of a “checked-in artifact.”

The two reproducibility prerequisites from the checked-in artifact era (compare only decompressed payloads and use `GOTOOLCHAIN` to pin the toolchain
specified by `go.mod`) are also obsolete: go1.26.6 and 1.26.7 produce different binaries from the same source, so
byte-for-byte comparison required a pinned toolchain at that time. Different gzip/zlib versions also produce different
deflate streams for the same payload (a measured difference of 1821 bytes), so only the payloads could be compared.
Now that the binary is compiled from the revision under test each time, these differences no longer affect
any decision.

The go directive in `go.mod` intentionally remains aligned with the actual version in the CI image `golang:1.26` (currently
1.26.7): When the host version is lower than the directive, Go automatically downloads the toolchain, placing about 309 MiB (69 MiB downloaded +
240 MiB unpacked) in `GOMODCACHE`. Because `GOMODCACHE` has no automatic GC, each cache generation
incurs this cost. Keeping the versions aligned makes this step a consistent no-op in CI; the cost shifts to developers
whose local Go version is older (a one-time automatic download by Go).

The throughput/latency assertions for the tunnel's dual channels are disabled by default (run only with `GPUWORKS_TEST_PERF=1`; see the
`skipUnlessPerf` comment for the command). They measure wall-clock time in an in-process network simulation, so results follow host
load rather than the code under test: The same ctrl latency test measured 22/31/50 ms on a machine without other load (the last
run fully saturated all 12 cores), but exceeded the 500 ms
limit when the full test suite and a real CI job ran concurrently on the same machine. Legacy single-channel throughput
measured 396 and 476 Mbps in two consecutive runs inside the same container. Upper and lower limits that drift with the host
are a coin toss in CI, not a regression signal. The gate disables only the measurements; deterministic assertions for channel routing, data lane
attachment and fallback, byte integrity, and reconnection continue to run unconditionally.

The earlier claim that `.32` does not support partitioning was incorrect: nerdctl lacks the Ascend Docker
Runtime plugin, but GPUWorks can pass in the raw device nodes and mounts itself, and vcann-rt still uses
the host's device-share capability. `TestAscendNerdctlPartitionCapabilityProbeEndToEnd`
now calls the production `Engine.BuildPartitionProbeWorkload` path, loads the embedded 2.5 MiB
`gpuworks/partition-probe:v1` image through the real nerdctl driver, and
asserts that `dsmi_get_hbm_info` reports exactly 4096 MiB. It does not depend on any serving image and
does not load Python, a model, vLLM, or SGLang.

An intermediate diagnosis mistakenly treated the CANN8.5 and CANN9 asset paths as different ABIs. Binary
comparison disproved this assumption: The SHA256/BuildID values of the two arm64 files are identical, as are those of the two
amd64 files. They are the same vcann-rt build copied into different
build-matrix directories; the version number is provenance/verification metadata, not a runtime selection key. Image
version metadata remains only an advisory warning. Node capability comes from this small behavioral probe; future
image-specific probes must actually execute in the target image's userspace before the product can claim that
image has passed partitioning validation.

The committed helper supports `GPUWORKS_KEEP_ON_FAILURE=1`: On failure, the final container and its
exact partition state directory are retained and printed for debugging; successful runs clean up after themselves. See
`docs/ascend-support-matrix.md`.

## Platform-API Partitioned Inference on the Owned Ascend Resource Pool

- Status: Implemented (IMPLEMENTED) and validated on real hardware from mirror1 (2026-08-28)
- Scope: `test/l4/partition_ascend_pool_platform_test.go`,
  `test-l4-ascend-pool-partition-inference` in `Makefile`, and
  `l4-ascend-pool-partition-inference` in `.gitlab-ci.yml`

`.31` and `.32` are two test hosts (fixtures); the unavailability of one no longer causes the entire group to fail together. CI treats
them as a pool selected by capability: Unreachable or unsuitable members are recorded, and the
first member with working SSH, an idle configured card, a local model, and an application image
is used. A current, self-contained arm64 agent is uploaded to a unique
`/tmp/gpuworks-ascend-pool-*` directory and started on a unique loopback port;
the test never overwrites or restarts the host's existing `gpuworks-agent` service.

The arm64 agent from the current committed code is built in `mirror1:~/workspaces/gpuworks_build` (where the CI job
already runs), gzip-compressed in memory, and made available to the selected node for download through a random, short-lived private
HTTP URL. This avoids both remote compilation and the SSH file transfer path measured at about 5 KiB/s.
The agent's `server_dial` listener uses a random private port with token/HMAC authentication,
because the owned nodes' sshd disables TCP forwarding by administrative policy.

`TestAscendPoolPlatformPartitionInferenceAndWholeGPU` registers a real local model through
`POST /api/v1/models/local`, creates two 8 GiB replicas on the same physical card through
`POST /api/v1/deployments`, waits for two
real vLLM containers to become ready, calls `/v1/chat/completions`, and verifies that over-quota
ACL allocations are rejected in both containers created by the platform. It then stops the partitioned deployment through the API,
creates a whole-GPU deployment through the API, and completes a second real chat. The earlier
direct-Docker L4 test remains a useful diagnostic tool, but no longer serves as the basis for
CI conclusions about platform path coverage.

Real run results: The nerdctl member passed actual device injection; the platform test observed that card 7
was busy, automatically selected idle card 1, started two 8 GiB vLLM replicas, completed a chat, and confirmed that both
containers correctly rejected over-quota allocations. It then stopped them and completed the whole-GPU deployment/chat. The run took
about 10 minutes and verified precise cleanup of both the temporary agent directory and the two containers.

## Extend Gitee CI to Cover Full Server+Agent Deployment and Chat (Whole-GPU, Unpartitioned)

- Status: Complete (DONE, 2026-08-28)
- Scope: `test/l4/partition_gitee_deploy_chat_test.go`;
  `l4-gitee-ascend-deploy-chat` in `.gitlab-ci.yml`

`TestGiteeAscendPartitionCapability` previously asserted only the partition capability probe result (structurally
limited on Gitee; see
`artifacts/archive/2026-08-28-gitee-ascend-verification/device-share-limitation-2026-08-28.md`).
`TestGiteeAscendFullServerAgentDeployChatEndToEnd` and
`TestGiteeAscendARM64ContainerInContainerDeployChatEndToEnd`
(`test/l4/partition_gitee_deploy_chat_test.go`) are added to cover
the rest of the Huawei/Ascend compatibility surface on Gitee: renting real x86_64/CANN9 and arm64/CANN9 instances,
bringing a real server+agent online, deploying real whole-GPU vLLM through the real `POST /api/v1/deployments` API
(unpartitioned; confirmed never to call
`applyAscendPartition`/`EnsureDeviceShare`, so it does not trigger the device-share
limitation), and a real `/v1/chat/completions` round trip. The image comes from
Huawei's own SWR registry
(`swr.cn-south-1.myhuaweicloud.com/gpustack/runner:cann9.0-910b-vllm0.20.2`,
accessible through Gitee instances' public network egress); the model is `Qwen/Qwen3-0.6B` from ModelScope.

Both full tests run a CANN9 application image inside their respective outer rented instances. The arm64 test
deliberately rents a valid CANN9/910B1 outer image: Gitee's catalog API currently lists an
arm64/CANN8.5 outer image, but Rent rejects that combination with `镜像和主机不匹配` (“The image and host do not match”),
so treating it as a rentable fixture would create false coverage.

Running a CANN8.5 application image inside the
PRoot boundary of a CANN9 outer environment was also tested: The container starts, but CANN FE cannot
resolve the realpath of `aarch64-linux/lib64/../data/platform_config`, and torch_npu fails with
`Get ACL JitCompile default value failed` before loading model weights. This matches
the path-resolution failure in the upstream CANN8.5 container itself and remains an explicit compatibility gap,
not a passing CI result.

The capability probe and full deployment use separate jobs with clear
semantics: `l4-gitee-ascend-partition-capability` and `l4-gitee-ascend-deploy-chat`.
The latter remains an explicitly low-frequency path. Both full deployment tests explicitly assert
`runtime_driver=udocker`, proving that the Huawei SWR application image is the inner container rather than
the outer rented image. Partitioning validation runs on the owned Ascend resource pool
(`l4-ascend-pool-partition-inference`), which is not subject to the device-share limitation.

Real-hardware validation completed on 2026-08-28 (about 21 minutes for x86_64 and 29 minutes for arm64/910B1;
zero leaked cloud resources after cleanup). Three root causes were discovered and fixed during this process, all real bugs
independent of the test itself:

- Multi-architecture udocker prefetch previously always selected `linux/amd64`; it now selects
  `linux/<runtime.GOARCH>`, and the registry-stub test asserts the child manifests for both amd64 and arm64.
  The real ARM CANN9 run downloaded the expected 26 ARM layers
  (6,873,397,151 bytes), created the container, initialized CANN/NPU, reached a healthy state, and
  completed a chat through the platform gateway.

- `rewriteGzipAsTar` in `internal/agent/runtime/udocker/optimize.go` previously
  used a single-pass `gzip.Reader` that stopped at the first member's EOF. A Docker/OCI layer
  can consist of multiple gzip streams concatenated in the same blob (as was the case for the SWR image's 4.1 GB
  layer). This silently produced a truncated plain tar, causing
  `udocker create` to fail with "Error: while extracting image layer". The fix
  loops through `gzip.Reader.Reset` until the underlying file is exhausted.
- Gitee single-GPU instances have only a 30 GB system disk; an image of more than 7 GB plus its unpacked
  layer cache exhausts it. Gitee instances always include a 60 GB
  data disk mounted at `/data`, so a `DataDiskPath` field is added to `cloudprovider.Instance`
  (set to `/data` on Gitee) and passed through `installCloudAgent` to
  `provision.SSHInstallRequest.InstallDir`. This places the agent's entire state directory
  (models and udocker image layers) on the data disk instead of the system disk.

Another fix in the same batch: `Gitee.get()` previously had no retry mechanism, so a single transient GET failure
in the ownership verification call inside `Destroy()` aborted the entire cleanup process, leaving a
real billable instance running (encountered once during this session's validation runs and handled manually).
`get()` now retries up to 3 times with linear backoff.
