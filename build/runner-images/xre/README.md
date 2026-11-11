# Kunlun P800 release contract

The matrix records the digest-pinned XRE runner as a **prebuilt artifact**. This is an existing
GPUWorks image, not a vendor base for a new Dockerfile. `build.sh` reports and
skips it instead of silently rebuilding from an internally repackaged base.
The source is pinned in `matrix.yaml`; changing its layers requires a new
revision and new hardware acceptance.

The release is `xre5.0.21.46-vllm0.25.1-r1` (Linux amd64). The selected
artifact has manifest digest
`sha256:33d94768ff9a895bdc8874cae09940aede1b0e88218e52ed52231b8f86a91beb`
and config digest
`sha256:cf75e94dc5d9c9cfc5ff263008035af4ad3d23bb262c42d711d5c030d18685ec`.
The runtime entrypoint is `/opt/vllm025-venv/bin/vllm`, using its paired Python.
Do not infer XRE or the engine interpreter from the legacy image's Torch
metadata or a CUDA compatibility field.

## Release and verification contract

1. Verify the source manifest and config against the pinned artifact. Retain
   its vendor-base provenance and third-party notices when preparing a new
   revision; a matching tag alone is not a provenance record.
2. Confirm the native XRE/BKCL version, engine import, RDMA userspace libraries
   and PP/DP fixes using the fixed interpreter. Inspect image history for
   credentials; keep credentials out of publication logs.
3. Publish the exact artifact under the immutable revision tag in Docker Hub,
   Quay and SWR. Do not overwrite a revision that already points elsewhere.
   All three refs generated into `catalog-seed.json` must really exist.
4. Run `make image-manifest-verify`; every listed platform and blob must be
   readable anonymously. Authentication used to push is not evidence of
   anonymous pull availability.
5. Use a fresh GPUWorks database to verify System → vLLM · XRE and the two
   Qwen P800 profiles without any imports. Register original local weights,
   evaluate/create, confirm the selected devices and FP16 computation, and
   obtain nonempty Web and HTTP/SSE replies. Recheck existing user profiles
   survive a seed refresh. A locally tagged cache is useful for this test but
   does not satisfy the public-pull gate.

Publication can reuse `publish.py` with the existing public repository as
`--src`, the existing artifact tag as the source, and
`--dest-tag xre5.0.21.46-vllm0.25.1-r1`. Verify that the source tag still resolves
to the matrix digest immediately before copying. Credentials are supplied by
file/stdin, never checked into the repository. Use the digest directly when
copying with skopeo (`copy --all --preserve-digests`).

The current immutable revision is published in the three catalog registries.
Anonymous manifest/blob checks and P800 acceptance were completed for that
revision; reachability and access policy must still be checked at each site.
Repeat the gates above for each new revision; publication credentials never
prove anonymous download access.

The three matrix scripts deliberately distinguish imported artifacts:

- `build.sh` skips `prebuilt`; it must not rebuild or relabel these layers.
- `verify.sh --all` reports and skips `prebuilt`. The imported artifact contains a legacy
  `/opt/gpuworks/image.json` (`framework: torch`) and assertion script, but these
  describe its inherited stack, not the fixed `/opt/vllm025-venv` interpreter
  and native XRE contract used by this release. Verify this release against its digest and the gates above instead.
  Passing an explicit image to `verify.sh` still applies that stricter contract.
- `push.sh --tags-from matrix` reports and skips only `prebuilt`.
  It continues distributing the separately built macOS `host-env` packages.
  It does not assume this tag exists in the default internal source registry.
  Copy a prebuilt release explicitly with `skopeo copy --all --preserve-digests`,
  using the digest-pinned `artifact` as the source and the immutable release tag
  as the destination.

`make test-runner-image-scripts` covers matrix selection and copy orchestration
without a registry, container daemon or PyYAML installation. It runs from
`test-unit-checked` and the CI Python-script test lane. The fixture's JSON-only
YAML adapter is limited to the test; the manifest generator checks production
matrix syntax.

These skips are reported omissions, not hardware or public-pull verification.
