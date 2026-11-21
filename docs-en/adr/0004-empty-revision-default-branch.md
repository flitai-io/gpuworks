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

<!-- translation-source-sha256: c0431d63bc71f62510487c44dff9172013b8e89e4dbb1a1862c34c868a645693 # ADR 0004: An Empty Revision Follows the Platform's Default Branch Instead of Being Filled with main -->
<!-- translation-source-sha256: 60e328e834ce18e11c4d15293c9a3414d23f94bc3520b5539bcd48f4efeb002f ## Background -->
<!-- translation-source-sha256: e6abe2ec5f47acd177c01e3b1237009e20a7fcbcd51f15c290f53fe59e224157 ## Decision -->
<!-- translation-source-sha256: 8d63f5710b75bdf06c4d189141d998f727b2de74986866e47d6845c393a537e4 ## Alternatives and Trade-Offs -->
<!-- translation-source-sha256: 6871d1b3d41851d52c3553561a747c756f31adf89315d4e29bbb68f1bf961fbd ## Consequences -->
