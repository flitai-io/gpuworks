# Release Notes

This section records what each GPUWorks release adds, changes, fixes, and secures, newest first.
For upgrade steps, see [Upgrades and Version Compatibility](../how-to/upgrade-and-compatibility.md).

## Versions

| Version | Release date | Type | Highlights |
|---|---|---|---|
| [v26.11.3](v26.11.3.md) | 2026-11-12 | Patch release | Hygon cards reporting only `BW` match built-in templates, Hygon cards other than card 0 work under Docker/nerdctl, deployments no longer refused when the selected model and weight source disagree |
| [v26.11.2](v26.11.2.md) | 2026-11-08 | Patch release | Private-network sources work by default, storage paths no longer limited to default directories, local models deploy with inference templates, benchmark metrics corrected for reasoning models, Go 1.26.10 |
| [v26.11.1](v26.11.1.md) | 2026-11-04 | Patch release | Default vLLM and vLLM-Omni runners move to r4, Qwen3-0.6B deploys on V100, about 20 templates fixed from A100 runs, archives ship third-party license texts |
| [v26.11.0](v26.11.0.md) | 2026-11-03 | First release | First version of the 26.11 release line |

## Version Numbers

Releases use calendar versions `vYY.M.PATCH`: `YY.M` is the year and month the release line was cut from master, and `PATCH` starts at 0.
Patch releases on a line are mostly bug fixes; a fix is merged into master first and then backported to the release branch.
The "Upgrade notes" section of each version page lists behavior changes to check. For the full rules on version numbers and publishing, see [Release and Embedded Assets](../dev/release.md#版本号).

## Downloading and Verifying

The one-line installer `get.sh` picks the fastest of `dl.gpuworks.ai`, Alibaba Cloud OSS, and Huawei Cloud OBS and checks the archive against `SHA256SUMS` before installing;
upgrades from the console read the ed25519-signed release index. For a manual download, archives live under `https://dl.gpuworks.ai/releases/<version>/`,
and `SHA256SUMS` in the same directory lists the checksum of each platform's archive. The download sources keep every release's archives permanently and the most recent 30 nightly builds.
