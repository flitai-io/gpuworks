# A100 Verification Bug Status Registry

This living document is maintained alongside commits. It records the actual status on the **`feat/a100-p0-remediation` branch** of platform bugs listed in
`model-inference-validation/.../2026-09-02--gpuworks-a100-validation/evidence/bugs.md`.
A few early fixes from `feat/blueprint5-fix` have been merged into `master`; the other commits referenced in this table still exist only on `feat/a100-p0-remediation`.

In bugs.md, “fixed in the working tree” refers to the adjacent verification worktree `../gpuworks_blueprint5`, not a fix on this branch; this table uses this branch's source code as its authority.

Below, “landed” means that the fix is in this branch's codebase. `fixed` means the code has landed and has tests; see each row for the specific scope still awaiting real-hardware retesting.
Catalog means the inference catalog, and spec means an executable specification in the catalog; smoke means a minimal viability check, and harness means the driver that organizes test execution.

**Status values:** `fixed` (landed with tests), `partial` (partially landed), `planned` (has a concrete plan in `artifacts/work/2026-09-04-a100-verification/plan-a100-p0-remediation.md`), `backlog` (future fix, no plan yet), `upstream_blocked` (runner/upstream boundary), `semantic` (semantic quality issue, no platform change).

**Last checked**: 2026-09-04, commit `4b3dad9` (local implementation and layered acceptance for A/B/C/D/E; mirror1 L2/L3, gpu3 UDocker, and provisioning restart replays recorded).

## 1. Fixed (fixed / partial)

| ID | Issue | Status | Commit | Notes |
|---|---|---|---|---|
| BUG-006 | Runtime assets are root-only and cannot be mounted by the udocker user | fixed | `d9de071` | `internal/agent/engine/runtime_mounts.go` prepares mount paths for the runtime user; installer `chmod 0711` and `GPUWORKS_RUNTIME_MOUNT_ROOT` |
| BUG-016 | NVSwitch nodes are admitted to scheduling even when CUDA cannot initialize | fixed | `d381837` | `gpu.CUDARuntimeSmoke` runs during onboarding; doctor adds fabric manager / peer access / NCCL diagnostics. L4 retesting is still needed to confirm that failing nodes stay out of the schedulable pool |
| BUG-021 | Processes remain after UDocker instances stop, conflicting with replacement instances | fixed | `d381837` | `Stop` waits for exit, then sends SIGKILL after 3s; `Remove` is idempotent for not found and propagates other errors |
| BUG-030 | Gateway truncates large non-streaming responses (WAV / base64) | fixed | `d381837` | EOF in the tunnel request direction only half-closes the connection; proxy `FlushInterval: -1`; proxy_test adds an audio/wav case. L4 replay of >700KiB binary data remains pending |
| BUG-037 | Downloads lack capacity admission checks, and progress resets to zero after ENOSPC | fixed | `d381837`, `4b4e7b9`, `837abd4`, `c213965`, `d2ae578` | Check space before download; report actual bytes written on failure; both pre-check failures and actual `no space left on device` errors caused by the race between checking and writing are classified as `insufficient_disk_space`; L3 replay that actually fills a small tmpfs passes; UI retains resumable byte counts |
| BUG-044 | Recursive chown and ETXTBSY after agent restart | fixed | `1376fd1`, `4b4e7b9`, `4b3dad9` | Retain atomic replacement for ETXTBSY; migrate UDocker ownership once using root/bin + the `.gpuworks-owner` marker, removing repeated repository-wide chown; the gpu3 provisioning restart harness prepopulates 62 rootfs directories and verifies that restarts no longer add chown calls |
| BUG-005 | Offline unpack loses `/etc/alternatives` links | fixed | `dbdd1b5`, `c694f4e`, `554260c`, `e4e9edd` | The native udocker unpacker preserves symlinks/hardlinks by tar entry and correctly handles whiteouts, opaque directories, and directory↔link replacement; fall back to the CLI only if native unpacking fails. The L3 harness uses a synthetic TLS OCI registry + bundled udocker/proot to verify CUDA, c++, and python3 alternatives chains, hardlinks, whiteouts, and base reuse and cleanup; the same helper also passes in an actual gpu3 Linux run |
| BUG-015 | Planning uses root disk capacity rather than available space on the model disk | fixed | `dc36585` | `NodeSnapshot` reads `models.available_bytes` from the agent's `PathUsage`; missing or invalid values are treated as unknown (0), rather than incorrectly using `DiskTotalKB` |
| BUG-043 | No API to evict model cache by node | fixed | `4b4e7b9,3c688bc,c69b670,92c3f26,2c21493,527a532` | Add administrator `DELETE /models/{id}/nodes/{uuid}`; the agent deletes only resolved paths under the models root; the store is idempotent and rejects active deployments (including participating nodes in distributed NodeIDs); 409 uses the stable `MODEL_IN_USE` code and lists deployment IDs; L2 with real server/agent processes proves that reconnecting after eviction does not restore the cache |
| BUG-002 | Peer acquisition failure does not fall back to the public Internet | fixed | `4b4e7b9` | Automatically enter the download desired state if a public source exists; explicitly report `source_required` otherwise |
| BUG-007 | ModelScope `revision=main` results in an empty list | fixed | `4b4e7b9` | Normalize ModelScope `main` to `master` at registration |
| BUG-001 | Templates silently discard user-specified images | fixed | `4b4e7b9` | Preserve explicitly specified images and let the unified catalog/GPU compatibility check decide whether to return 400 |
| BUG-003 | Empty udocker image digest | fixed | `4b4e7b9` | Compute a stable sha256 image ID from the native manifest and return it in inspect/pull status |
| BUG-004 | Empty weight metadata in evaluate | fixed | `4b4e7b9` | Fall back to spec weight_bytes and write it back to the model record |
| BUG-028 | Component weights are excluded from planning | fixed | `4b4e7b9` | Add component_weight_bytes to the breakdown; the scheduler uses the component total |
| BUG-049 | Evaluate allows nested mounts | fixed | `4b4e7b9`, `ca44582` | Reject overlapping parent/child targets both before launch synthesis and during manifest import |
| BUG-059 | Conflicting model_id values in the template and request are silently overwritten | fixed | `4fab9cf` | Return a conflict error when an explicit model_id does not match the template's weight identity |
| BUG-014 | systemd unit lacks mount dependencies | fixed | `4b4e7b9` | Add RequiresMountsFor and an ExecStartPre executable check to the unit |
| BUG-063 | Recovery upload lacks timeouts and validation | fixed | `4b4e7b9`, `84eaa44`, `8abf200` | Staged file writes, sha256 validation, atomic replacement, and stalled-upload cleanup; L2 with real SSH transport covers missing directories, stalls, and sha mismatches; two consecutive failures in the same stage enter `recovery_blocked` |
| BUG-010 | Each instance fully unpacks the same digest again | fixed | `b87b488,d12e174` | Native unpacking caches an immutable base by manifest digest; subsequent instances use reflink/clone (macOS falls back to ordinary cp) |
| BUG-038 | Rootfs is not reclaimed for failed/deleted instances | fixed | `4b4e7b9` | Remove cleans up leftovers in the exact container directory; CleanupOrphans reconciles after state.sync and records failures |
| BUG-031 | Mistral dual layouts are double-counted as runtime weights | fixed | `b175446` | Add `runtime_weight_bytes` to spec/migration/API/snapshot; calculate GPU memory from the loader-selected layout and disk usage from the full repository with present/partial/absent increments; built-in Ministral 8B/14B use the exact consolidated byte counts from verification evidence |
| BUG-033 | Native Mistral layout is rejected by config.json validation | fixed | `b0ce7f3` | The validator accepts root-level params.json + tekken.json + consolidated.safetensors and rejects nested/mixed layouts |

## 2. Planned (planned; See `artifacts/work/2026-09-04-a100-verification/plan-a100-p0-remediation.md` for the Plan)

| Chain | ID | Issue | Planned Steps |
|---|---|---|---|

## 3. Backlog (backlog)

### 3.1 Catalog Parameters / API Contracts (Minimal Fix Evidence Exists; Land in Batches; Corresponds to “M4: Batch Landing of Proven Catalog Fixes (P1)” in `artifacts/work/2026-09-04-a100-verification/plan-nvidia-a100-verification-fixes.md`)

BUG-008, 013, 018, 019, 022, 023, 024, 025, 026, 035, 038 (the dtype portion), 039, 046, 047, 050, 051, 053. These share errors in template parameters, example requests, or API modes, and candidate fixes have passed on real hardware. Replay the minimal contract once with the current runner before landing.

### 3.2 Catalog Sources and Hub Resolution

BUG-011 (missing fallback Hub source), BUG-012 (source repository can no longer be resolved).

### 3.3 Platform Capabilities (Corresponds to “M5.1 Platform Capabilities GPUWorks Must Fix” in `artifacts/work/2026-09-04-a100-verification/plan-nvidia-a100-verification-fixes.md`)

- BUG-036: Offline UDocker TP2 aborts in `ncclCommInitRank`; needs NCCL smoke checks and failure log retention.
- BUG-041: Explicit deploy-config is lost in the Omni runner wrapper.
- BUG-048: Spec-level capability cannot narrow model-level kinds.
- BUG-060: Diffusers specs with zero weight bytes pass planning.
- BUG-062: BF16 candidates actually run as FP8, violating the metadata contract.

### 3.4 Joint Component / Runner Delivery (Corresponds to “M5.2 Joint Catalog + Runner/Component Delivery” in `artifacts/work/2026-09-04-a100-verification/plan-nvidia-a100-verification-fixes.md`)

BUG-020, 027, 029, 032, 034, 042, 045, 052, 054, 055. These require cooperation from image, component, or checkpoint owners and are not purely platform changes.

## 4. Issues Not Fixed by the Platform

| ID | Category | Disposition |
|---|---|---|
| BUG-009 | semantic | PaddleOCR-VL-1.5 endpoint is correct but semantics are wrong; runtime PASS, semantic WARN |
| BUG-017 | Awaiting replay | Qwen3 FP8 SGLang A100 Marlin gate; replay with the current runner before deciding whether to relax it |
| BUG-040 | upstream_blocked | SGLang GME multimodal embedding crashes |
| BUG-056 | semantic | Qwen3-VL-30B nondeterministic vision / video hallucination |
| BUG-057 / 058 | upstream_blocked | Qwen3-4B FP8 exits with code 1 in both SGLang and vLLM; no further retries with the same versions |
| BUG-061 / 064 | upstream_blocked | vLLM 0.28.0 Gemma4 loader / HunyuanOCR CUDA-graph signature |

## 5. Maintenance Rules

- To close a BUG, move its row to §1, fill in the commit hash, and update the original bugs.md entry with that hash and one of `platform fix / catalog fix / runner upgrade / upstream_blocked / semantic`.
- Add newly discovered BUGs to §3 first, then move them to §2 once planned.
- Every `partial` entry must state “done / not done”; the word partial alone is insufficient.

<!-- translation-source-sha256: 94e3d3f09b94a8b75546862bb292d9ea1c432f043c9b2c882c96f2f712d215b6 -->
