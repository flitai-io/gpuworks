# Release Guide

## Build Artifacts

Run the following commands from the repository root. Build prerequisite artifacts such as the UI first, then generate the archives for distribution:

```bash
make build
make release-packages
```

`make build` first validates assets and builds the UI and cross-platform agents, then builds the server and the agent for the current platform into
`bin/`. The server embeds `internal/server/webui/dist` and agents for each target platform through `go:embed`.
For a release, you cannot simply rebuild the server and skip these prerequisite artifacts. To target only Linux amd64, use
`make server-linux-amd64`, which still builds the UI and the embedded agents.

`make release-packages` produces four server archives for Linux and macOS on amd64/arm64 (approximately 45 MB each), along with a shared
`SHA256SUMS`. The three agents embedded in the server are stored with zstd compression (using `-19` when the build machine has the `zstd` command; the release job first tries
`apt-get install zstd` and falls back to the Go encoder if installation fails, increasing the package size by approximately 1.5 MB). The extracted server binary is approximately 69 MB. The macOS server also embeds the Linux amd64/arm64 agents, so you can run the control plane on a development machine and
connect Linux GPU nodes. Apple silicon Mac nodes run supported engines as host processes through the `host` runtime; see [ADR 0044](../adr/0044-mac-engines-as-host-processes.md) for their capability limits.

Besides the `gpuworks` binary and `INSTALL.txt`, each archive carries `LICENSE`, `NOTICE` and `LICENSES/`: the third-party programs embedded in the binary, such as PRoot, talloc and perftest,
require their license texts to accompany every binary distribution. When you add or replace an embedded asset, update `NOTICE` and `LICENSES/` with it.

To avoid the artifact size limit for a single job (each archive is approximately 53 MB after the server embeds the macOS agent,
so two archives in one job exceed the limit), CI splits the build into four jobs by OS and architecture: `release-build-linux-amd64`,
`release-build-linux-arm64`, `release-build-macos-amd64`, and `release-build-macos-arm64`.
Each job builds only one archive. Use `GPUWORKS_RELEASE_OSES=linux|darwin` and
`GPUWORKS_RELEASE_ARCHES=amd64|arm64` to reproduce the builds locally. The release site only combines the artifacts from all four successful jobs
in the same pipeline.

Both runtime binaries must keep `CGO_ENABLED=0`. Run these checks before release:

```bash
make check-assets          # HAMi libvgpu.so glibc baseline; ib_write_bw library/symbol baseline and hashes
make runtime-assets-verify # Ascend vcann-rt, PPU ppu-slicing-shim, and partition probe manifest integrity and per-file sha256
make test
make vet
git diff --check
```

`check-assets` includes `check-libvgpu` and `check-perftest`; it does not validate Ascend, PPU, or partition probe assets.
You must run `make runtime-assets-verify` separately before release, or missing or tampered assets in these three categories go undetected.

## Release Documentation PDFs

For a formal version tag, the pipeline first builds the Chinese and English documentation from that commit,
then `release-docs-pdf` generates two user guides. They cover tutorials, how-to guides, reference, and architecture,
excluding developer documentation and ADRs. The cover identifies the version and source commit. The guides include
a clickable table of contents, page numbers, images, Mermaid diagrams, and searchable, copyable text. Links to included
articles point to their PDF chapters; links to omitted articles and downloads point to online documentation and require network access.

The outputs are `dist/docs/gpuworks-<tag>-zh.pdf`, `dist/docs/gpuworks-<tag>-en.pdf`, and
`dist/docs/SHA256SUMS`. Failed checks for Chinese fonts, images, links, content width, or PDF completeness block publication.
PDF artifacts for formal version tags are retained permanently. Once publication succeeds, all three files are available under
`releases/<tag>/docs/` on each distribution source for separate download and sharing. Documentation has its own checksum
list; the root `SHA256SUMS` continues to list only platform archives. Uploaded PDFs and checksums are read back in full
and compared before switching the distribution channel.

Local generation uses the website's Hugo and the existing browser acceptance tools, Playwright and Chrome/Chromium,
plus Noto Sans CJK fonts, Fontconfig, and Poppler tools (`pdftotext`, `pdfinfo`, `pdffonts`).
Install `requirements-e2e.txt`, then run in a checkout of the version tag:

```bash
HUGO_BIN=/path/to/hugo CHROME_PATH=/path/to/chrome make docs-pdf VERSION=v26.11.3
```

Replace the example version with the actual tag. The generator checks that the website output matches the requested source commit.
CI generates PDFs only for formal version tags. MRs and nightly builds do not generate PDFs; MRs still build the documentation site and run related checks.
Republishing the same version preserves the original documentation bytes. Historical versions without PDFs are not backfilled or replaced.

## Version Numbers

Releases use calendar versions `YY.M.PATCH`, tagged as `vYY.M.PATCH`, for example `v26.11.0`.

- `YY.M` is the year and month the release branch is cut from master, with no zero padding on the month (`26.1.0`, not `26.01.0`). The first version of a release line has `PATCH` 0;
  each later bug-fix-only patch version increments it by one.
- Each release line has one branch, `release/YY.M`, cut from a chosen commit on master. Only bug fixes go onto the branch: a fix is merged into master first, then
  applied to the release branch with `git cherry-pick -x`.
- Before tagging, add the version's release notes under `docs/releases/` (`vYY.M.PATCH.md`, with upgrade notes, additions, changes, fixes, security, and download checksums),
  add a row at the top of the version list in `docs/releases/README.md`, and update the translation under `docs-en/releases/`; merge into master first, then backport to the release branch. A `release` channel build has `scripts/package_release.sh` check that both language pages exist and the version list has the row; if any is missing, the build fails before it starts.
- Create an annotated tag on the release branch and push it (`git tag -a v26.11.0 -m "GPUWorks 26.11.0"`, `git push origin v26.11.0`).
  Only tags that match `^v[0-9]+\.[0-9]+\.[0-9]+$` create a pipeline and publish to the `release` channel; tags such as `v26.11.0-rc.1` create no pipeline.
- `v*` must be a protected tag in GitLab: the signing private key and the distribution source credentials are protected variables, so a pipeline for an unprotected tag cannot read them and the publish job fails.
- Once pushed, a tag is never moved or recreated: a release id maps to exactly one set of bytes (see step 1 of the publishing process below). If something is wrong, release the next patch version.
- When a server decides whether an update exists, it compares version numbers if both sides are version tags, and otherwise (a nightly's short commit hash, a local build) compares build times
  ([ADR 0040](../adr/0040-signed-multi-source-distribution.md)). The `release` channel follows only the newest release line: when the channel already names a higher version
  (for example, pushing `v26.11.2` after `v26.12.0` was released), `publish_dist.sh` refuses to publish.

## Distribution Channel

Server versions, the system image catalog, and the built-in inference template catalog are delivered to installed servers through the same distribution channel ([ADR 0040](../adr/0040-signed-multi-source-distribution.md)).
The three distribution sources host byte-for-byte identical directory trees:

| Source | Address | Purpose |
|---|---|---|
| Alibaba Cloud OSS Shanghai `gpuworks-dl` | `https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com` | Mainland China (default domain; no ICP filing required) |
| Huawei Cloud OBS Shanghai 1 `gpuworks-dl` | `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com` | Mainland China (default domain; no ICP filing required) |
| Cloudflare R2 `gpuworks-dl` | `https://dl.gpuworks.ai` | Other regions |

All three sources use the same directory structure. `<channel>` represents the release channel, `<release_id>` identifies a release,
and `<sha256>` is the content digest of the corresponding catalog file:

| Path | Contents and Purpose |
|---|---|
| `<channel>/index.json` | An ed25519 signature envelope containing the signed release index; the body lists server archives for each platform, the image catalog, and the inference template catalog package, with a path, size, and sha256 for each file |
| `<channel>/latest` | The release id as plain text, used to locate the version for the initial download |
| `<channel>/manifest.json` | The same file as the image catalog in the channel's index, at a fixed address for downloading by hand and uploading in offline environments |
| `releases/<release_id>/` | Server archives and `SHA256SUMS` |
| `catalog/images/<sha256>.json` | The image catalog identified by its content digest |
| `catalog/templates/<sha256>.tar.gz` | The inference template catalog package identified by its content digest |
| Root-level `get.sh` | One-line installer (source: `scripts/get.sh`): picks the fastest source, downloads the archive for the local platform, verifies it against `SHA256SUMS`, and runs `gpuworks install` |

The `nightly` channel is every master build, with the short commit hash as the release id; `release` is every version tag, with the tag as the release id (see [Version Numbers](#version-numbers)).
Binaries follow the channel injected at build time (`make CHANNEL=…`, defaulting to `nightly`; `scripts/package_release.sh` reads `GPUWORKS_RELEASE_CHANNEL`,
and the `release` channel accepts only release ids of the form `vYY.M.PATCH`).

CI's `release-publish` runs `scripts/publish_dist.sh <channel> dist` after all four build jobs on master or on a version tag succeed.
It runs in a separate `publish` stage because GitLab 14.0 allows needs to reference only jobs in earlier stages.
`make check-ci-yaml` checks this constraint. The script follows this process:

1. A release id always maps to exactly one set of bytes: Files under `releases/` use long-lived immutable caching, so the CDN retains the first copy it sees. Rebuilding the same commit
   (in a new pipeline or a manual rerun) produces different package bytes because the build time is included in the package. If a source already has `releases/<id>/`, the script copies it back.
   After `gpuworks-dist adopt` verifies it against `SHA256SUMS`, the script publishes that already-published package with its build time and discards the newly built package;
2. If any source already serves a build in the same channel with a later creation time, the pipelines have finished out of order, and this run publishes nothing; the `release` channel is also compared by version number,
   and the run exits with an error when a source's `release/latest` names a higher version;
3. `gpuworks-dist assemble` generates the directory tree from these archives and the two catalogs embedded in the current checkout, then signs the index;
4. rclone pushes immutable files to the three sources with `--immutable`: If an object with the same name has different contents on a source, it exits with an error and never overwrites it (that source requires manual intervention; see below);
5. Before switching, `gpuworks-dist verify -index` uses the new index to check the size of each file at all three public addresses, including the CDN. If any size differs, it stops and the channel continues to point to the previous version;
6. It saves the current index, `latest`, and `manifest.json` from each source, switches to the new version, then reads them back from all three public addresses to verify them. If the switch or readback fails, it restores each source's previous pointers and exits;
7. Upload `scripts/get.sh` to the root of every source as `get.sh` with `Cache-Control: no-cache`.
8. Version tag (`vYY.M.PATCH`) directories are kept on every source permanently; each source keeps only the most recent 30 nightly directories (`GPUWORKS_DIST_KEEP`), plus the versions referenced by `latest` in the two channels.

If a source's `releases/<id>/` does not match its own `SHA256SUMS` (for example, it was overwritten before this rule existed), publishing fails at step 4 without switching versions. To resolve this, determine which source
has the correct copy, delete the incorrect copy or replace it with the correct one, clear the CDN cache for `releases/<id>/`, then rerun publishing.

Credentials and the signing private key are protected, masked CI variables: `GPUWORKS_DIST_SIGNING_KEY` (a base64-encoded 32-byte ed25519 seed), `GPUWORKS_DIST_SIGNING_KEY_ID`,
`GPUWORKS_DIST_OSS_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY`, `GPUWORKS_DIST_OBS_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY`, and
`GPUWORKS_DIST_R2_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY` / `GPUWORKS_DIST_R2_ENDPOINT`. Each of the three sets of storage credentials belongs to a dedicated identity that can only read and write
objects in `gpuworks-dl` (Alibaba Cloud RAM user and Huawei Cloud IAM user `gpuworks-dist-ci`, with policy `gpuworks-dist-publish`,
and R2 account token `gpuworks-dist-ci`). These identities cannot create buckets, so both publishing scripts set `no_check_bucket` for rclone.

To publish manually from a local machine, use `make dist-publish CHANNEL=nightly`,
with credentials in `secrets/dist-publish.env`. To publish a release, run `make release-packages CHANNEL=release GPUWORKS_RELEASE_ID=v26.11.0`, then
`make dist-publish CHANNEL=release`.

Two cache rules are configured in the Cloudflare zone `gpuworks.ai` for `dl.gpuworks.ai`: Never cache `*/index.json`, `*/latest`,
or `*/manifest.json`; never cache 404 responses (otherwise, the edge caches the 404 from the pre-publish `newer` check for the default four hours, making a new channel unreadable after its first release).
Immutable files use long-lived caching according to the origin's `Cache-Control: immutable`. `/notices.json` (publisher notices; see below) and `/get.sh` must also bypass caching and must be added to the same rule.

Publisher notices ([ADR 0054](../adr/0054-usage-report-and-notices.md)): Edit `build/notices/notices.json` (the unsigned body; see
`internal/server/distribution/notices.go` for schema and validation: each notice has `id`, `severity`, `kind`, a `title` in at least English, optional `body` and https `url`, and required `expires_at`;
use `when` to restrict the channel, server build time range, and accelerator vendor), then run `scripts/publish_notices.sh`. It validates the notices using the server's rules, signs them with the release key,
uploads them to the root of all three sources with `Cache-Control: no-cache`, then reads them back from each public address for verification. Notices are independent of builds and can be published separately at any time. To withdraw a notice, delete its entry and publish again.

The collection endpoint for daily improvement program reports ([ADR 0054](../adr/0054-usage-report-and-notices.md)) is Alibaba Cloud SLS Shanghai project `gpuworks-usage`, logstore `reports`
(WebTracking enabled; `appendMeta` enabled to record the source IP as `__tag__:__client_ip__`; 180-day retention; analytics indexes on `schema`/`install_id`/`day`/`version`/`report`/`__tag__:__client_ip__`; a 16 KiB limit for `report`).
Extract fields with SQL such as `json_extract_scalar(report, '$.usage.routes["GET /nodes"]')` and `ip_to_province("__tag__:__client_ip__")`. The fallback is Cloudflare Worker `gpuworks-usage`
(custom domain `usage.gpuworks.ai`, code in `scripts/usage-collector/`, tests via `node --test scripts/usage-collector/worker.test.mjs`).
Reports are stored in the private R2 bucket `gpuworks-usage` at `reports/<day>/<install_id>.json`, with the source IP, country, and region in object metadata `client_ip`/`country`/`region`. WebTracking actually accepts only four top-level keys: `__topic__`/`__source__`/`__logs__`/`__tags__`,
contrary to the `project` and `logstoreName` keys listed in its API documentation.

**Signing key rotation**: Generate a new key, add the new public key alongside the old one in `trustedKeys` in `internal/server/distribution/defaults.go`, and publish a version.
Wait for installed servers to upgrade before switching to the new private key for signing, then remove the old public key in the following version.

When creating buckets, note that both object storage providers enable bucket-level “Block public access” by default. Disable it and set the bucket to public read.

### GitHub Actions Releases

`.github/workflows/release.yml` implements the same flow on GitHub, with the same triggers and channel
rules: every push to master builds `nightly` (the release id is the 8-character short commit), a
`vYY.M.PATCH` tag builds `release`, and any other tag fails.

1. The `meta` job fixes the release id, the channel and one build time for the whole run
   (`GPUWORKS_BUILT_AT`, the counterpart of GitLab's pipeline creation time);
2. The gates call `ci.yml` in `release_gates` mode, which runs only the UI build, unit, vet, lint and the
   CompShare offline contracts, matching the needs of GitLab's `.release-build`; the UI suite and L2/L3 are
   not waited for;
3. Four build jobs package one archive each by OS and architecture, embedding the UI the gates built;
4. A version tag also calls `docs.yml` to build the docs site, then prints the handbooks in both languages;
5. The `publish` job merges the four outputs (plus the PDFs for a tag) and runs `scripts/publish_dist.sh`;
6. A version tag also gets a GitHub release carrying the four archives, a `SHA256SUMS` covering all of them
   and the PDFs, with notes from `docs-en/releases/<tag>.md`.

GitLab remains the only publisher to the distribution sources. The `publish` job runs only when the
repository variable `GPUWORKS_DIST_PUBLISH` is `true`; at the switch, turn off GitLab's `release-publish`
first and then set the variable, so the two never publish the same channel at once. The publish
credentials and the signing key are secrets of the `distribution` environment (named as the CI variables
above), whose deployment rules admit only master and `v*` tags; a tag ruleset must also restrict pushing
`v*` tags to maintainers, the counterpart of GitLab's protected tags. The docs site build fetches the OINK
theme from the public `gpuworks_website` repository and needs no secret.

### Kubernetes Agent Image

The image a Kubernetes cluster's agent DaemonSet runs (`build/agent-image/Dockerfile`, see [ADR 0052](../adr/0052-kubernetes-nodes.md)) is published by CI's
`release-agent-image`. It sits in the `publish` stage with `release-publish` and runs after both linux build jobs succeed:
`make agent-image-publish` (`scripts/agent_image.sh publish`) builds amd64 and arm64 in one go and pushes them to
`docker.io/gpuworks/gpuworks-agent`, `quay.io/gpuworks/gpuworks-agent`, and `swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent`.
The tag is the runtime revision (`build/agent-image/REVISION`, such as `r1`), which is what the manifest names when a cluster entry leaves its agent image empty, plus `r1-<server version>` to tell the builds apart.
The image is only the agent's runtime: the agent in it only seeds a node, which then switches to the server's own agent and upgrades in place, so a nightly overwriting `r1` does not affect connected clusters.
Raise the runtime revision only when the base image or nerdctl changes (edit `REVISION` and `kubemanifest.AgentImageRevision`; a test keeps them equal), and say in the release notes that Kubernetes clusters apply the manifest once more after the server upgrade; also upload `gpuworks-agent_<rN>_linux-<arch>.tar.gz` for both architectures to `gpuworks-agent/` of the Aliyun Drive offline share (`build/runner-images/pds-upload.py`, as for runner images; see `build/runner-images/README.md`).

- The agent in the image is built with the same `LDFLAGS` as the embedded agents; nerdctl comes from its upstream release, is verified against the sha256 pinned in the script, and is cached on the runner host.
  The runners time out on GitHub, so CI's `NERDCTL_URL_BASE` lists two GitHub mirrors and then GitHub itself; the sha256 decides what is used.
- buildx's docker-container builder does not inherit the host docker's proxy: the script hands the daemon's http/https/no proxy to the builder, and the buildx client (which fetches registry tokens for buildkit) uses the same proxy.
- Push credentials are the protected File variable `GPUWORKS_AGENT_IMAGE_DOCKER_CONFIG` (a docker `config.json` with credentials for the three registries); without it the job fails.
- Provenance and SBOM are off: SWR refuses an OCI index that carries attestations.
- To build one architecture locally: `make agent-image ARCH=amd64|arm64 AGENT_IMAGE=<name>`.

## Embedded Native Assets

Embedded native assets include the runtime libraries, tools, and probe images listed below. They are not ordinary Go source code,
so rebuilding only the server does not incorporate updates to them. Validate or rebuild each through its corresponding entry point:

| Asset | File | Platform | Validation/Rebuild |
|---|---|---|---|
| NVIDIA HAMi | `internal/assets/bin/libvgpu.so` | One copy, compiled against the oldest supported glibc baseline | `make check-libvgpu` (`scripts/check_libvgpu_compat.sh`) |
| RDMA bandwidth test tool | `internal/assets/bin/ib_write_bw-{x86_64,arm64}` | x86_64 + arm64; uses the host's RDMA userspace libraries at runtime | `make check-perftest` (`scripts/check_perftest_compat.sh`); see `build/runtime-assets/perftest/README.md` for build instructions |
| Ascend vcann-rt | `internal/assets/bin/runtime-assets/ascend/linux-{amd64,arm64}/libvruntime.so` | One copy each for amd64 + arm64 | `make runtime-assets-verify`; rebuild with `make runtime-assets` |
| PPU ppu-slicing-shim | `internal/assets/bin/runtime-assets/ppu/linux-amd64/hggc-13.0/{hggc_quota.so,hgml_dlsym_hook.so}` | Currently amd64 only | Same as above |
| partition probe | `internal/assets/bin/runtime-assets/probe/partition-probe-linux-{amd64,arm64}.tar.gz` | amd64 + arm64 | Same as above; rebuild the images separately with `make runtime-assets-probe` |
| udocker PRoot | `internal/assets/bin/proot-{x86_64,arm64}-static-nofd` | x86_64 + arm64 | Covered by neither `check-assets` nor `runtime-assets-verify`; verify manually against `internal/assets/bin/manifest.sha256`. `build/runtime-assets/proot/reproduce-embedded-x86_64.sh` rebuilds the x86_64 binary byte for byte; `build/runtime-assets/proot/README.md` records the source of each binary |

The manifest and hashes for the Ascend and PPU runtime libraries are centralized in `internal/assets/bin/runtime-assets/manifest.json`. Their backend ids
are `ascend-dgpu` and `ppu-dgpu`, respectively (defined in `internal/accelerator/backend.go`). Partition probe hashes
are in the `.tar.gz.sha256` files beside each archive. `make runtime-assets-verify` checks the runtime library manifest, probe hashes, and image archive structure together.
When replacing any asset,
retain records of its source, architecture, hash, and license, run the corresponding asset checks, and confirm that the release package actually contains the new file.
Each vendor's release assets support only the declared platform combinations (PPU currently has no arm64 build). For user-facing limitations, see the
[dGPU Support Matrix](../reference/support-matrix.md).

## Database and Catalog Packages

The server runs versioned migrations at startup. Before release, verify migration markers (which record migration versions), idempotency on repeated execution, and transaction rollback.
The built-in inference catalog is a content-addressed package, meaning it is identified by its content digest. Release builds must apply the same schema, path, and digest checks to the package. The system image catalog's
embedded seed is generated from the gpuworks-runner build matrix by `make image-manifest` and enters the [distribution channel](#发布通道) with every release; see the
[CLI Reference](../reference/cli.md#gpuworks-image-manifest). Unit tests fail if the embedded seed differs from the matrix.

## Upgrade and Rollback

- Server: `gpuworks upgrade` automatically stops the service, creates a backup (database + `master.key` + old binary), performs an atomic replacement,
  checks health, and rolls back on failure. For the manual path, back up the data directory and `master.key`, then replace atomically and restart. Before rolling back through either path,
  confirm whether the database migrations are backward compatible.
- Agent: Upgrade from the nodes page or rerun `/install.sh`. Downloads go to a temporary file first and replace the executable atomically after success. A failed connection smoke test
  must not overwrite the current executable.
- Record the release version, build commit, platform, embedded asset digests, and catalog package digest. Store one-time acceptance results in
  `artifacts/work/<YYYY-MM-DD>-<topic>/` (move them with the topic into `artifacts/archive/` after completion).
