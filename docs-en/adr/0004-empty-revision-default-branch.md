# ADR 0004: An Empty Revision Follows the Platform's Default Branch Instead of Being Filled with main

Date: 2026-08-23. Status: Accepted.

## Background

When registering models, the server previously filled empty revisions with `main` regardless of the source. ModelScope conventionally uses `master` as its default branch, and its file-listing API returns HTTP 200 + `Code:200` + `Files:null` for a nonexistent revision (with no error signal), whereas its `/resolve/` endpoint tolerates `main`. This led to successful registration followed by inevitable download failure that was difficult to diagnose (the mirror1 incident; see `artifacts/archive/2026-08-23-model-download-revision/plan.md`).

## Decision

An empty revision has first-class semantics: **follow the source platform's default branch, as resolved by the platform**. The entire chain (api/store/proto/agent/pkg/modelrepo) allows an empty revision. ModelScope file-listing requests omit the `Revision` parameter; download URLs are pinned to the commit SHA in the file-listing response. The UI moves revision into advanced options.

## Alternatives and Trade-Offs

1. **Hardcode defaults by source** (modelscope→master, hf→main): this also fails for ModelScope repositories whose default branch is not master and embeds platform conventions in code.
2. **Retry with another branch name on the agent when the file list is empty**: this masks the symptom, adds a round trip, and still fails for a third branch name.
3. **Empty = platform default (adopted)**: this is the only correct semantic for all repositories. The trade-off is that registration no longer determines which version is downloaded; pinning to a commit SHA restores determinism.

## Consequences

- Existing `modelscope + revision='main'` data must be migrated to empty revisions.
- The actual downloaded commit becomes a runtime fact, reported with status and displayed in the UI's advanced information.
- Test fixtures (fake hubs) must strictly simulate the platform's actual revision behavior; otherwise, this class of problem remains invisible locally. This requirement is now in CLAUDE.md.

<!-- translation-source-sha256: 7c6bbbb35d79d97b297cf6dab71c5765c05c90a4be287e99d0efafc76c6dfd9c -->
