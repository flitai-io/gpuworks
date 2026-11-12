# ADR 0017: Use Tag-Only Image References in the Built-In Catalog and Remove Mandatory Digest Immutability

Date: 2026-09-07 · Status: Accepted (supersedes the image immutability requirements in ADR 0010 stating that “images...continue to be delivered through immutable external references” and “supply images with digests”; the rest of ADR 0010—the multi-file package structure, `assets/` restrictions, and immutable reference requirements for weights/dependencies—is unaffected)

## Context

All `image:` fields in the built-in inference catalog (`internal/server/inferencecatalog/seed/catalog/models/**/model.yaml`, about 280 files and 882 occurrences) previously used the `tag@sha256:<digest>` format, enforced by `catalog_contract_test.go`.

The problem: upstream registries (Docker Hub, quay.io, and others) do not guarantee retention of historical digests. When tags such as `:latest` are rebuilt, the manifest/blob for the old digest may be garbage-collected, leaving the digest with a **permanent 404**—a failure that should not be discovered only when the inference service is actually deployed. An actual error looks like this:

```
all image sources failed: vllm/vllm-openai:latest@sha256:2286e8533ca8b6bc777594bae30524f1426ba46ca21797524e06df6a94b06635: exit status 1
```

Image acceleration/multiple pull sources cannot fix this: the agent-side pull candidate chain (`pullRefs` in `internal/agent/engine/engine.go` and `imagePullRefsForImage` in `internal/server/store/deploy.go`) merely replaces the registry prefix for the same invalid digest, leaving the `@sha256:...` suffix unchanged. All candidate sources fail together, and no path can pull the image.

## Decision

Change all existing `image:` fields in the built-in catalog to tag-only references by removing their digest suffixes. Delete the regex validation in `catalog_contract_test.go` that requires images to include a valid digest. **The image field is no longer required to include or omit a digest.** When adding or editing a spec, both a plain tag and `tag@sha256:...` are valid for image and pass the tests. If a spec does include a digest, its behavior remains exactly as before: it pulls by that digest as usual, and a pull failure still produces a deployment-time error (`all image sources failed: ...`). The responsible party handles the failure; the catalog layer does not pre-judge whether the reference is correct. This differs from ADR 0001's hard validation that rejects digests in the manual image catalog: validation is removed here, rather than reversed into a prohibition.

Unaffected parts:

- Weight source revision immutability (HF/ModelScope commit sha, the `immutableCatalogRevision` validation in `catalog_contract_test.go`) remains unchanged;
- Content addressing of the whole catalog package through `PackageDigest` (`seed.go`) remains unchanged;
- The soft warning mechanism for caller-supplied digest images in deployment/Playground paths (`rejectsDigestImage`/`ImageDigestWarning` in `deployment/templates.go`, `WarningImageDigestUnverified` in `deployment/evaluate.go`, and post-pull local image digest verification in `agent/runtime/udocker`) is a general fallback capability, not an immutability guarantee specific to the built-in catalog. It remains to cover manually supplied arguments and externally imported catalog packages that still contain digest images.

## Trade-Offs

This gives up the guarantee that built-in catalog image references are frozen and reproducible at release time in exchange for availability: images remain pullable after upstream rebuilds a tag. The catalog itself no longer guarantees unchanged image contents; contents follow the tag's current upstream target. This is an accepted trade-off under this decision, no longer treated as a regression.
