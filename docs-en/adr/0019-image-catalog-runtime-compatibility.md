# ADR 0019: Keep Only Runtime Facts in the Image Catalog and Use Compatibility for Ranking and Warnings

Date: 2026-09-14 · Status: Accepted

2026-09-19: [ADR 0022](0022-template-owned-deployment-launch.md) adjusts the default command mode and configuration source for new deployments; existing deployments retain their original semantics.

## Context

The image catalog previously stored vendor, GPU model labels, Variant, compatible models, and GPU support/exclusion lists together.
These free-text fields frequently disagree with actual node hardware, cannot cover new models, and incorrectly place model knowledge in the image layer.
Public compatibility information for drivers and CUDA, CANN, DTK, and HGGC also changes continuously, making it impossible for the catalog to enumerate every combination.

## Decision

- Image series and versions describe only framework, framework version, engine, engine version, host architecture, and runtime defaults. The vendor is derived from the framework's unique mapping; old fields remain as compatibility columns but are no longer read, written, or included in wire JSON.
- Automatic selection first enforces engine, framework, vendor, and host architecture as hard constraints. If the user specifies an engine version, normalize it and require an exact match; otherwise, sort numeric versions in descending order and select the latest enabled version in the series. For now, stable sorting resolves ambiguity within the same candidate set.
- Driver compatibility only affects candidate ranking and generates Warning. CUDA uses only the minimum driver requirement; the upper bound in the minor version table does not eliminate newer drivers. Unproven CANN, DTK, and HGGC combinations return Unknown. The system does not produce Perfect judgments.
- Framework, vendor, and driver compatibility remain advisory for explicitly selected images; a known CPU architecture or execution OS mismatch must produce Blocking, including for TrustedOverride. Catalog Metal artifacts are darwin host environment packages; other known accelerator frameworks ship Linux containers. Derive the OS from the existing framework and node runtime facts instead of adding a redundant catalog field. Unknown private images retain an unverified warning; unknown does not mean incompatible. Before creating model records or starting downloads, validate every concrete rank; automatic placement is checked again once its node is selected.
- `turing-no-bf16` applies only to the actual compute capability of NVIDIA nodes, with the condition `cc < 8.0`; it does not affect image candidate eligibility.

## Impact

Node detection and `chipmatch` are the sources of truth for GPU models, architectures, and capabilities; inference templates own model family and model capability constraints.
Catalog import, API, UI, and seed no longer generate or propagate redundant labels. Non-NVIDIA nodes do not incorrectly use the CUDA profile's default image when the catalog has no candidates.

<!-- translation-source-sha256: 5b2aacc036255ee422e83bdb8a6e7bdddfc7538399a6bca096f09b1618eabb92 # ADR 0019: Keep Only Runtime Facts in the Image Catalog and Use Compatibility for Ranking and Warnings -->
<!-- translation-source-sha256: 90cf1e4fdbcc6c728a7469cfd04b9e7c8a2bd6b9ac2c557e9d9364855d6a5811 ## Context -->
<!-- translation-source-sha256: 6aec01687b2506998b8a0a373e29da45e1bd1ba84667f366372f09b00ab74a72 ## Decision -->
<!-- translation-source-sha256: 7b9c86e6d0f5349df5b449e4d8442ea8b7d58bb3bac3c37c7cc595dde012de84 ## Impact -->
