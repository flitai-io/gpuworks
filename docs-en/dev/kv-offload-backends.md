# Adding a KV offload backend

A KV offload backend decides what keeps an instance's KV cache beyond the GPU: the engine's own memory and disk tiers, or a component outside the engine (LMCache or Mooncake Store, for example). This page is for contributors adding a backend. It describes the backend model, where each part lives and what adding a backend changes. The design trade-offs are in [ADR 0069](../adr/0069-kv-offload-backends.md).

## Backend model

A backend is one descriptor row in `internal/server/kvbackend`, unique per (engine, id) and compiled into the binary:

| Field | Meaning |
|---|---|
| `ID`, `Engine` | The backend id and its engine; the id is the value of the deployment parameter `kv_offload_backend` |
| `Label` | The product name, shown as is on the deploy form's card and the instance panel |
| `Default` | The engine's own offload; an empty `kv_offload_backend` resolves to it, at most one per engine |
| `External` | A component outside the engine keeps the KV cache; the requested-feature snapshot records `external_kv_cache` |
| `Vendors` | Accelerator vendor allow-list; empty means every vendor |
| `Replaces` | Native parameter keys that cannot be set while the backend is selected; every selectable row must list every key of the engine's own offload, including `kv_offload_block_size`, which has no dictionary row |
| `PDRoles` | PD roles that keep the backend, always including prefill; other roles launch without offload |
| `Topology` | The node-count cap and whether PP and DP are allowed |

A row holds only the data decisions are made on. There is no `init()` registration and no mutable package state. Today the built-in table carries `vllm/native` (the default) and `vllm/lmcache` (the first external backend).

## Where each part lives

- **Selection**: `kv_offload_backend` in the parameter table is a selector key (`Selector`) with no flag, and it is not a platform parameter. It is published only when the engine has at least one non-default row, with options generated from the registry; otherwise the key is dropped from requests.
- **Rendering**: `internal/server/deploy/kv_render_vllm.go` gives each backend's vLLM half (`vllmOffloadBackend`) by id. Its renderer receives the whole `Params` (PD role, vendor, compute network, parallelism) and returns the connector object. It may also return keys for the `MultiConnector` that wraps it (`wrapper`; the composer's own keys win) and environment for the engine process (`env`, merged once in `vLLMEngine.Build`). `composeVLLMKVTransfer` in `kv_compose.go` composes the PD part and the offload part into the one `--kv-transfer-config`. The config the platform composes for a non-PD launch is never classified as PD, whatever its connector is called. Only vLLM has an offload adapter today.
- **Validation**: `kv_offload.go` returns these codes (text in `ui/src/i18n/locales/{en,zh}/common.json`):

  | Code | When |
  |---|---|
  | `KV_OFFLOAD_BACKEND_UNKNOWN` | The engine has no selectable backend with that id; checked on evaluate, edit and launch |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER` | A native key in `Replaces` has an effective value; values the platform writes by default do not count |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_ARG` | An argument layer of an external backend states a flag that would replace its configuration (`KVConflictFlags`, matched across spellings); checked under PD too |
  | `KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET` | The target vendor is not in `Vendors`; an unknown vendor (not placed yet) is allowed and checked after placement |
  | `KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED` | The node count, PP or DP is outside `Topology` |

  The last two are checked in `ValidateTargetFeatures`, which both validating modes reach; collecting capabilities alone does not check them.
- **PD**: `PDRoleLaunchConfig` clears the selection and the native fields when the role is not in `PDRoles`. `KVOffloadPDRoles` gives the roles that keep offload; when decode is not among them, evaluate returns the warning `PD_KV_OFFLOAD_PREFILL_ONLY`.
- **Reverse parsing**: the engine hook `RecognizeKVBackend` reads a backend's non-PD rendering back into the selection. `ReverseArgs` and `CanUnsetTemplateFlag` share it, so saving as a template round-trips. A backend whose object carries its own values provides a reader (`vllmOffloadBackend.recognize`); the values it returns read back as dictionary values beside the selection. A backend without a reader is recognized only by its rendering with nothing else stated.
- **Frontend**: the engine profile's `kv_backends` lists the selectable backends (`ProfileForTarget` filters them by vendor). The deploy form shows one card per entry, and when one is selected `projectDraft` unsets the keys in its `replaces`; a selected backend the profile no longer lists (the target changed) is not sent and is unset. In a deployment's read, each instance carries `kv_offload_backend` (`{id, label}`, scoped by the server to the instance's PD role), which the instance panel shows. `ui/src` names no backend id.

## Adding a backend

1. Add a row in `kvbackend.Builtin()`, filling only the fields it really uses. A new field arrives together with the first row that uses it.
2. Add a renderer for the id in `vllmOffloadBackends()` (`kv_render_vllm.go`), with a reader when its object carries its own values. `internal/engines/features.go` needs no change: the non-PD config the platform composes is not classified by connector name.
3. Add the id to the `kv_backend_id_literals` pattern in `scripts/check_architecture.sh`.
4. Tests: the table invariants in `kvbackend`; `TestKVBackendSetRendersEverySelectableRow` in `deploy` (`kvBackendSetProblems`) checks that every selectable row has a renderer, that `Replaces` covers every key of the engine's own offload, and that a non-PD launch requests no PD. The generic rules are already covered by the test row `vllm/testext` (`kv_backends_helpers_test.go`), so a new backend adds tests for its own rendering and a real-hardware L4 case.
5. Update the cards' execution status in the browser E2E ledger `test/e2e/coverage/deployment-wizard-create.json`.

## Known limits

- In a mixed-vendor cluster the scheduler does not filter nodes by backend vendor; an unsupported vendor is refused at target validation after placement.
- An older server drops `kv_offload_backend`: a deployment that selected an external backend runs without offload there.
- Only vLLM has an offload adapter. A SGLang row (HiCache, say) first needs SGLang-side rendering, conflict flags and a reader; the tests refuse a row with no adapter.
- A backend's own dictionary keys (keys that depend on `kv_offload_backend`) have no mechanism yet. `PDRoleLaunchConfig` clears only the native keys, and `TestPDRoleLaunchConfigDropsEveryOffloadKey` fails once such a key appears, as a reminder to clear it on roles that do not keep the backend.
- A rendering cannot carry mounts or `/dev/shm` needs yet; the first backend that needs them adds the fields.

<!-- translation-source-sha256: ae122e25f6fbc24143739cc9c7fb1fbf8c37dfaeaaca33252774d1fa27bd2a9f # Adding a KV offload backend -->
<!-- translation-source-sha256: 58722d102565c48fc9bb6266cbac9129dad315cf2f320a79a43d91f5d83172be ## Backend model -->
<!-- translation-source-sha256: e9442f7d6c36882599e79b7b00aad83cdf035647d00b347348bff75197c2de1f ## Where each part lives -->
<!-- translation-source-sha256: 689c687cf253e1c603096cb12b8fba44c9f011f358f64c44d34b5825d767b056 ## Adding a backend -->
<!-- translation-source-sha256: 2d262c2c25a3463c389b80a5e5ca391a6c3c60a86b6e6edfde51fc5a5e9199ab ## Known limits -->
