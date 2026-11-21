# ADR 0038: Inference Templates Declare Image Sources; System Rewrite Rules Apply Only to System Images

Date: 2026-10-08 · Status: Accepted (supplements the image candidate sources in ADR 0018; digest handling for template images revises the "accept only candidates with the same digest" rule from 79e97549)

> 2026-10-16: Decision 5, "System rewrite rules apply only to system images," is superseded by [ADR 0048](0048-pull-settings.md): source rules apply by repository or prefix to all images, including template-declared sources. Template `image_sources` remain equivalent image copies and precede catalog sources.

## Context

The platform has a "system image" mechanism: `registry_sources` on image catalog rows, the quay / SWR / registry.zstack.ai
rewrite rules in `image_registry_rewrites`, and the candidate order produced by `pullref`. These mechanisms assume an image resolves to a row in the image catalog—that is, an image published and maintained by gpuworks itself.

Most inference template images are not in the image catalog (as already noted in the context for ADR 0018, this reduces candidates to the canonical reference alone). Templates published by third-party authors, such as those pinned to a digest at
`ghcr.io/…@sha256:…`, often cannot be pulled from networks in mainland China, even though the author has a ready alternative: a byte-for-byte identical public copy or a ghcr mirror. Previously, these addresses could only be used through:

- Global rewrite rules: these apply to all deployments, and the rule table cannot express "a particular image in a particular template."
- Registering the image in the image catalog: this turns a template-specific fact into a platform catalog fact and requires accepting the catalog's "tags required, digests rejected" constraint.
- Manually entering an image override at deployment: the deployment is marked as deviating from the template, and the override must be entered every time.

At the same time, the agent accepts only candidates with the same digest for digest-pinned images (79e97549). This makes the node judge source content on behalf of the server: if an author-declared source is a tag reference or a copy under another
digest, the node rejects it before pulling.

## Decision

1. **Templates can declare multiple sources for their images.** The spec gains `image_sources: [ref, …]`, structurally equivalent to `weights.sources`: authors list sources in preference order, and deployers select one in the existing
   image source selector under "Acquisition → Public download" on the deployment page. `image` remains required and continues to identify the template's image and provide the comparison baseline for "deviates from template."
2. **Accept what the template declares.** Validate syntax only: after trimming leading and trailing whitespace, references must be nonempty, contain no whitespace or `://`, be no longer than 512 characters, and have no duplicates (compared after registry reference parsing; see `pullref.RefKey`). Sources do not have to pin the same digest as `image`;
   a source may use a tag even when `image` pins a digest. Reachability is not checked. Entries identical to `image` are discarded during normalization because `image` is already a candidate.
3. **Candidate order: template sources (in declared order) → the image catalog row's `registry_sources` (only if the image resolves to a catalog row) → `image` itself, deduplicated; archive addresses remain last.**
   The default chain in `ImagePullPlanFor` (when no explicit selection is made) follows the same order: template sources first, followed by the catalog chain (if a catalog row exists) or `image` itself. An explicit selection still replaces the entire chain. Both the deployment page's candidates and the default chain are
   assembled by `pullref.DeclaredFirst` (pull candidate ordering still has a single implementation in `pullref`).
4. **Template sources apply only when the deployment image is the spec's `image`.** If the request overrides the image, it is a different image: template sources are neither displayed nor accepted, and are not recorded in the snapshot.
5. **System rewrite rules apply only to system images.** `image_registry_rewrites`, catalog `registry_sources`, and the factory rules in `pullref` continue to apply only to images that resolve to catalog rows.
   Template sources are used unchanged, without rewriting or reordering. The node's own `network.rewrites` remain node configuration and are still applied to the final candidates.
6. **Selecting a template source does not count as deviating from the template.** `image_pull` does not participate in deviation checks; deviations arise only from request changes to engine, scenario, image, spec, env, or arguments.
7. **Server candidates are authoritative; nodes no longer verify content.** For a digest-pinned `spec.Image`, the agent accepts any candidate provided by the server (including addresses rewritten by node network rules):
   an already-local candidate is accepted directly, and a successfully pulled candidate is used directly as the launch reference, without `docker tag` (the runtime rejects a digest as a tag target). The behavior of tag-referenced `spec.Image` remains unchanged.
   Whether a candidate is "the same image" is the author's and deployer's responsibility; the node does not judge it again.
8. **Persistence:** `inference_specs.image_sources` (text, JSON `[]string`, empty means none; migration 133). Deployments record `image_sources` in their catalog selection snapshots (only when the image equals the spec's
   `image`) and reconstruct candidates from the snapshot on restart. Old snapshots lack this key and are read as having no sources.

## Tradeoffs

- The platform no longer "guarantees that source content matches `image`." Checking digest equality would require pulling and comparing every source and still would not cover tag-referenced sources. If a source points to different content,
  the author could put it in `image` itself anyway. This is the template author's responsibility, the same tradeoff as not comparing weight bytes across `weights.sources`.
- The "cached" status is still queried only by the canonical name of `image`. After a digest-pinned image is pulled from another source, it exists on the node only under that source's name, so the deployment page's "Already available locally" option does not match it.
  On a subsequent deployment, as long as the server's candidates include that source, the agent finds the copy in its local check and does not pull it again. However, the cache status displayed on the deployment page and scheduling disk estimates do not reflect this.
- Template sources extend only public Registry candidates and do not change peer sync or archive downloads: these two paths depend on an image catalog row and have no candidates when a template image lacks one.
- Sources identical to `image` are discarded instead of rejected, so authors cannot repeat `image` to put it ahead of other sources; `image` always follows the declared sources.

## Consequences

- Template authors can declare copies reachable from mainland China in a package. You can select one on the deployment page, without changing global rules or registering the image in the image catalog.
- The agent no longer rejects candidates before pulling because their digest differs from the spec. Older agents retain their previous behavior until upgraded (skipping candidates with different digests).
- Each spec in the template DTO (`GET /inference-templates*`) gains an optional `image_sources`. Evaluation's `acquisition.image.providers` puts template sources first,
  with `reason` set to `image source declared by the inference template`.
- Sources are retained with the spec when exporting or cloning a template; the deployment page requires no new controls.

<!-- translation-source-sha256: 8e21c096c0b0fa3de5562bb56809c858f9de1c5d80e8c92673a36b22e42758fa # ADR 0038: Inference Templates Declare Image Sources; System Rewrite Rules Apply Only to System Images -->
<!-- translation-source-sha256: 80a8dd3072c1ededbf4059cf0ef535637c4796cb03883fb9908332f41e243255 ## Context -->
<!-- translation-source-sha256: 86c0ba88784203f4243b16e8e2b97436db30c586cb31e62e5346e7708cdcd80a ## Decision -->
<!-- translation-source-sha256: ff8fdddd5143147dac28651077dca2c49f660ea3653a09f1aa0af133a21182cb ## Tradeoffs -->
<!-- translation-source-sha256: 6aceb07b775c27f0e57848dfa992e10d41ba52670b5ca72b5bff2f5da675dab2 ## Consequences -->
