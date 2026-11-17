# ADR 0011: Isolate Spec Identity by Source Within the Same Original Model

Date: 2026-09-01 · Status: Accepted

Built-in catalog upgrades and user extensions must coexist under the same original model without overwriting specs that share a name. Spec identity is therefore fixed as `(original_model_id, source, name)`, where source can only be `upstream` or `user`. Built-in reconciliation updates or deletes only upstream specs; user specs and their content-addressed asset packages remain unchanged. Deployments, previews, and the Playground all save and pass the spec source. When specs have the same name across sources and the caller does not specify a source, return an ambiguity error instead of relying on database ordering. While an upstream entry exists, the built-in catalog continues to own the original model's canonical metadata and description; user extensions only add user specs. The entry becomes user-owned only if the built-in catalog removes that original model while user specs still exist.

This choice avoids two coarser boundaries: isolation by `model_id` would force users to invent a new original model just to add a set of weights, while replacing an entire engine group would delete other user specs for that engine when updating one spec. Explicit selection of a same-named spec requires one additional source field, but this provides predictable upgrade behavior and precise asset ownership.

<!-- translation-source-sha256: ea3f915a46d5ac496deb471539cfce05a86a3c6c05fedb93105724fb7795cca1 -->
