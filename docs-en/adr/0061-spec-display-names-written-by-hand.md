# ADR 0061: Inference spec display names are written by hand in the catalog, no longer assembled by the frontend

Date: 2026-11-02 · Status: Accepted (changes 0009's "the UI computes labels from predicates")

## Background

The inference template detail page used to build each spec's name in the frontend as "{chip target} {general|compatible|optimized} spec": the chip target came from the spec's `gpus.display_target` (when missing, the frontend guessed it from the model name with regular expressions), and the kind from `display_profile`. Both keys were written by hand in YAML with nothing tying them to what `gpus` actually matches. The built-in catalog had 194 groups of specs of one model and engine sharing one display name (122 models); in 149 of them even the quantization and scenario tags were the same, so users had to open each spec to tell them apart. Some specs labeled "NVIDIA" matched only Hopper or Blackwell cards. "General / compatible / optimized" was never defined and was used inconsistently.

What really tells sibling specs apart is the chip generation, the quantization and weight publisher, a memory threshold, a fixed card count, the shape (speculative decoding, Engram offload, multi-node) and the image version. Which of these belong in a name differs from spec to spec.

## Decision

1. Remove the spec-level `display_profile` and `gpus.display_target`. Imports still read both keys so older templates keep working, but ignore them and raise the `CATALOG_DISPLAY_PROFILE_RETIRED` warning; they are neither stored nor exported again.
2. Specs gain hand-written `display_name_zh_cn` and `display_name_en`, one column each. The server sends them as is; the frontend picks the one for its language, falls back to the other, and shows the spec's `name` when there is neither. It derives no names.
3. A name reads "cards · quantization [· key limit or shape]"; see [Inference Catalog Specification · Display Names](../reference/inference-catalog-spec.md#display-names). Catalog validation keeps the Chinese and English names each unique within a model and engine, on one line and within a length limit; every built-in spec carries both names (guarded by a seed test).
4. The detail drawer's "Applicable GPUs" shows the real `gpus` predicate; the name is only a summary.

## Trade-offs

- **Written by hand vs generated from fields**: generation can only join a fixed set of dimensions, so names either grow too long or miss the real difference (for example, two specs that differ only in context length). Writing by hand costs two more lines per new spec and can drift from the predicate; the uniqueness check catches clashes, and drift is caught in review by comparing the chip names in the name with the predicate.
- **Two columns vs one per-language map**: the UI has only Chinese and English, like the model-level `readme_zh_cn` / `readme_en`; typed columns are simpler than a JSON map and follow [ADR 0032](0032-typed-columns-json-as-opaque-snapshot.md).

## Consequences

- Existing user specs have no names and show their spec `name` until edited. Built-in specs get their names with the catalog upgrade.
- ADR 0009's "the named classes' Chinese display names disappear, and the UI has to compute labels from predicates" no longer holds: display names are back in catalog data, but written on each spec rather than on a named chip class.

<!-- translation-source-sha256: de968f307c9612bed76046bd9412d02ed763e98d6ae709b6cf5929aa42aa059d # ADR 0061: Inference spec display names are written by hand in the catalog, no longer assembled by the frontend -->
<!-- translation-source-sha256: f0f3cecfc5622e2cbbddff480717a49152ad005a37cbb8e4fb62d49017f7c454 ## Background -->
<!-- translation-source-sha256: 68668f999c96f6d84ee55ad3f3de6b1856cf377e98f275509b5d3908943d0359 ## Decision -->
<!-- translation-source-sha256: bd63e2c14a972718f4e3c73674ca24701183a8804c52ba4a2a162281cec2d098 ## Trade-offs -->
<!-- translation-source-sha256: 43135cf0e87fbedff724b1a3fc4bb567e18eca0ad50d0c4b0b16b53de4132d4b ## Consequences -->
