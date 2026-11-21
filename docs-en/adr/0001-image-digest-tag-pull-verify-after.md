# ADR 0001: Verify Images with Tag-Based Pulls and Post-Pull Comparison Instead of Immutable Digest References

Date: 2026-08-22 · Status: Partially superseded (2026-08-23, M20 acceptance grill): three-state image ID verification (`expected_image_id`/VerifyState) was removed because generating baselines required pulling every image, imposed high operational costs, and had unclear user value; `blob_sha256` verification for offline imports was retained. On 2026-10-08, narrowing the image catalog removed offline tar package imports, so `blob_sha256` comparison also ceased to take effect (`artifacts/work/2026-10-08-image-catalog-simplify/plan-v2.md`). All verification portions of this ADR are now superseded. The decision to require a tag in refs and avoid digest references remains in effect. The original grill session is archived at `~/.grill_sessions/gpuworks-images-m19.json`.

## Background

The system image catalog needs to verify that an image on a node is the image specified by the catalog. The industry-standard approach is to pull an immutable `image@sha256:<digest>` reference. However, since M16.5, this repository has required tags in canonical refs and explicitly rejected digests (validation in `imagecatalog/source.go` and `api/images.go`), because tags underpin the entire mechanism for UI display, mirror prefix rewriting, and gpustack runner tags that encode versions (`cuda12.8-vllm0.17.1`).

Two other facts constrain the design:
1. Registry manifest digests are not stable when images are replicated between sources (such as quay ↔ Alibaba Cloud): some replication tools reconstruct the multi-arch manifest list, changing its digest.
2. Images loaded offline with `docker load` do not expose the registry manifest digest locally; only the image ID (config digest) is available.

## Decision

Keep the tag requirement for refs. Use separate, optional baseline columns for verification, and compare them after pulling or loading the image:

- `expected_image_id`: provided by the catalog; the agent obtains the image ID with `docker inspect` and compares it, covering both online pulls and offline imports.
- `blob_sha256`: the hash of the offline image tar, compared when the import is written to disk.

The result has three states: verified / unverified / mismatch. A mismatch displays a red warning but does not block deployment.

## Consequences

- (+) Verification semantics are consistent across multiple sources and offline scenarios, unaffected by digest drift during replication.
- (+) The change is small; tag, mirror, and display mechanisms remain unchanged.
- (−) Verification is not enforced during the pull, so there is theoretically a window in which an incorrect image is discovered only after it has been pulled. Verification on import and checking the state at deployment mitigate this.
- (−) Image ID verification depends on the catalog generation tool producing correct baselines; the tool must produce them by building from the same source as the image or actually pulling the image.

Rejected alternatives: immutable `@sha256:` references (overturn the tag system and suffer from digest drift across sources); recalculating before every deployment (repeated overhead with no new information).

<!-- translation-source-sha256: dc3d16279ead4526fe8db9f13b3683e8496536c66dfeb7d80d0ac8f4b7362549 # ADR 0001: Verify Images with Tag-Based Pulls and Post-Pull Comparison Instead of Immutable Digest References -->
<!-- translation-source-sha256: 7788aba67a663535907d6dc4edb350fe4f9938f5df08253c18c41871085a3e3b ## Background -->
<!-- translation-source-sha256: 7b3136655cbd8173639d7ea07efc327084c354a86a70f4558b833e505cba668d ## Decision -->
<!-- translation-source-sha256: cef06057273f6ace73507caa9fb1b6c1bc8bd62195bc1852679f0bc9f8d4cab2 ## Consequences -->
