# ADR 0065: Official Inference Catalog Packages Are Read Tolerantly per Model and Spec, and What Cannot Be Read Stays as It Is

Date: 2026-11-15 · Status: Accepted (supplements [ADR 0040](0040-signed-multi-source-distribution.md) and [ADR 0010](0010-executable-inference-catalog-package.md))

## Background

The built-in inference template catalog is published to the distribution channel with every release (ADR 0040), and "check for updates" on every server reads the same one. The catalog is written for the newest server, while a server parses `model.yaml` strictly, field by field: one model using a field an older server does not know gets the whole package refused, and that server receives no template update at all.

The package format version (`schema_version`) was meant to catch this, but it depends on someone remembering to raise it. `gpus.arch_id` (2026-11-03) and `tokenizer_ref` in the API contract (2026-11-10) entered the catalog without a new version, so a released v26.11.x reading such a package only reports a parse error.

## Decision

1. **Official catalog packages are read tolerantly.** When "check for updates" or an official package upload reads a package, a spec this build cannot parse or validate is skipped; a model whose own fields cannot be read, or whose specs each pass alone but not together, is skipped whole. Everything else is applied as usual. A skip follows component references: a spec that references a skipped model is skipped too, and a model without specs that no spec references any more is not kept.
2. **The package itself stays strict.** The file layout, path safety, the model and spec counts in the manifest (the counts the package states, not the ones this build could read) and the asset digests of the specs that are kept are checked as before, and any mismatch refuses the whole package. A `model.yaml` that is not a mapping, an unreadable manifest and a package format newer than this build also refuse the whole package.
3. **The package is stored as published.** What is skipped is not removed from the package, so its digest remains the one the channel publishes and the "is there an update" comparison is unaffected; after a server upgrade the same package reads in full.
4. **Existing records of what is skipped stay as they are.** An upstream model or spec already in the database that the new package states in a form this server skipped is neither rewritten nor deleted by the update, and keeps pointing at the package it came from; models and specs the new package really dropped are still deleted. The catalog in force is the package that wrote specs most recently.
5. **The administrator is told.** The result of applying lists every skipped model or spec with its reason, and the settings page keeps it on screen.
6. **A user's own import stays strict.** "Import templates", YAML or package, still reports an unknown field and where it is: there an unknown field is usually a mistake, and skipping it silently would drop a setting unnoticed.
7. **The package format version becomes 3 and from now on moves only for layout changes.** 3 marks the catalogs that carry `gpus.arch_id` and `tokenizer_ref`, so a server that knows only 2 shows "upgrade the server first" instead of a parse error. Every server that knows 3 reads tolerantly, so a new field or rule in `model.yaml` no longer needs a new version; it is raised when the file layout or the manifest changes in a way an older server cannot skip.

## Result

A template written for a newer server affects only itself: an older server still receives every other update, and a spec already in use does not disappear because of an update.

## Costs and What Is Not Done

- After a partial update the upstream records in the database may come from two packages. The older package the kept records reference is not cleaned up until the server is upgraded and replays its embedded catalog, or a package is applied in full again.
- Skipped new models and specs are not visible on the older server; the result of applying is the only place that shows they exist.
- Released servers without this decision (v26.11.0–v26.11.3) do not benefit: a version-3 catalog tells them to upgrade the server first. No downgraded compatibility package is published for them.
- The reason of a skip is the parser's or validator's own error text and is not translated.

<!-- translation-source-sha256: c6e1d14d55bd15bfb8d66e7c13073a4fb56cad5bf071c916d67038fd016fa083 # ADR 0065: Official Inference Catalog Packages Are Read Tolerantly per Model and Spec, and What Cannot Be Read Stays as It Is -->
<!-- translation-source-sha256: 63e15eae1cef7f7f2ab3431a08a91d66f571ca2c43579f3b84f8b31877fb3237 ## Background -->
<!-- translation-source-sha256: 6f246d0868f13d958b973a1059fb4af4ed604ea0f0ae85143da5149c5df548dc ## Decision -->
<!-- translation-source-sha256: dc1b119b785f452c7e55f8d467f73b17f35e2702d77a7eb10397cd396d3aeb96 ## Result -->
<!-- translation-source-sha256: f35571f15dec542c23378fa4dc6fb721f2e4d43b63b75c831c7c7a5ea878fa75 ## Costs and What Is Not Done -->
