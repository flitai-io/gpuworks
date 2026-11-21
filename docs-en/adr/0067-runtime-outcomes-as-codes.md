# ADR 0067: Runtime failures and check results are codes with parameters; English text is only a fallback

Date: 2026-11-14 · Status: Accepted (extends the API layer's `messages.ProductError` to runtime outcomes and check results; follows [ADR 0032](0032-typed-columns-json-as-opaque-snapshot.md))

## Context

Requests the API refuses have long been `ProductError`s (a stable code, interpolation parameters, an English fallback) that the client renders in its locale. Problems that happen at run time reached the user as English prose instead: a failed instance had only `InstanceStatus.Message`; the agent had read the exit code, the OOM flag and the last 1000 log lines but reported one sentence, and the server guessed again with the `aihelper` regular-expression table over that sentence and the logs. dGPU capability reasons, node precheck details, card check reasons and the category of a recovery failure (guessed from substrings such as `ssh` and `timed out`) had the same shape. Gateway `/v1` errors carried only `{message, type}`, without a code.

Every feature defined its own check status words, at least four sets: `ok/warn/error`, `passed/failed/skipped`, `collected/blocked`, `warning/error`; "not checked" and "not applicable" read like "passed" in the UI. Codes were not even spelled one way: `ProductError` used `MOUNT_TARGET_DUPLICATE`, the assistant and doctor used `kv_cache_insufficient` and `disk_low`.

That prose cannot be localized, counted or matched reliably; the assistant depended on the upstream engines' log wording, which changes between vLLM and SGLang releases. New code copies the code around it, so the convention has to be enforced by types and checks rather than only written down.

## Decision

1. **The code is decided where the fact is produced; text is rendered by the client.** Every failure, refusal or check result that reaches a user (UI, API response, event, diagnostics bundle, assistant) carries a stable code and interpolation parameters; the English text is only a fallback for old clients, logs and scripts. New code must not match on the fallback or use it as UI copy.
2. **Naming.** A `code` is always UPPER_SNAKE and names *what the outcome was*; a check `id` is always lower_snake and names *which check this is*. Lower-case codes reported by older agents are upper-cased at the server's read boundary; nothing downstream sees them.
3. **Where codes live.** Codes shared by the agent and the server are defined in `internal/proto` (the agent cannot depend on the server); codes produced only inside the server are defined in `internal/server/messages`. The constant is the single source; other packages reference it instead of writing the literal. Every code gets its en/zh text (`ui/src/i18n/locales/{en,zh}`) in the same change, enforced by a check.
4. **A check status has exactly five values** (`internal/proto/check.go`):
   - `pass`: checked, nothing wrong;
   - `warn`: checked, a problem that does not block;
   - `fail`: checked, a problem that blocks;
   - `skip`: should have been checked but was not (blocked by an earlier check, a missing capability, an old agent, a timeout, no result received); it must carry a code saying why;
   - `na`: the check does not apply to this resource.
   `skip` and `na` must never render as passed. A check result has the shape `id + status + code + params`, optionally with `evidence` and a `recheck` command, modelled on `proto.DoctorFinding` and `dto.DiagnosticCheck`; features need not share one Go type, but they use only these field names and status values.
5. **Persisted parameters follow ADR 0032.** Parameters in transit may be a `map` (as in `ProductError` and `dto.ProductMessage`); a persisted failure reason must be a typed struct (such as `proto.RuntimeFailure`), and the map is built only when mapping to the API.
6. **Instance failures.** `InstanceStatus.RuntimeFailure` grows from "accelerator injection refused" into the single structured reason for an instance's runtime failure: typed fields for the code, phase, exit code, OOM flag and evidence lines. The agent must attach it when it reports `failed`; engine log classification (`pkg/enginelog`) returns codes and evidence lines, not sentences; the known-failure signature table exists only in `pkg/enginelog`, and the assistant diagnoses by the reported code first, matching logs only for reports from older agents that carry no code.
7. **Text classification only at an external boundary.** Output of external systems (container runtime CLI stderr, cloud provider API error bodies, engine logs, SSH errors) may be recognized by its text only in the adapter that calls it, and is turned into a code or a typed error right there; error text the repository itself produced is never parsed — decisions use `errors.Is` / `errors.As`.
8. **Every HTTP error has a code.** Every error response of the management API carries `code`: when the caller supplied none, the writer assigns a generic code by status. Gateway `/v1` errors are written from one code table in the OpenAI shape `{"error": {"message", "type", "code", "param"}}`, and every response carries `X-Request-ID`. The one exception is an API whose codes OpenAI itself defines: `/v1/files` and `/v1/batches` keep OpenAI's lower-case codes (such as `invalid_api_key` and `model_not_found`), because that is an external contract that clients match from OpenAI's documentation.
9. **Enforced by checks.** The related budgets in `scripts/arch-constraints.txt` (text classification of errors, instance failures without a reason, gateway errors outside the code table, codes without locale text) are all 0 and may only stay at 0.

## Trade-offs

- **Shared vocabulary vs shared type**: the facts each feature attaches to a check differ (a disk path, a card, an HTTP status); forcing one type would grow a pocket of optional fields. What is shared is the field names, the status values and the rules for codes.
- **Generic codes vs one code per call site**: a code assigned by status says little, but it makes every error response countable and localizable; call sites that need a more specific explanation keep using `ProductError`.
- **Older agents**: older agents send no `RuntimeFailure` and report lower-case codes; the server accepts both on read. This is the lasting boundary of a mixed-version fleet, not an intermediate state of the migration.

## Consequences

- Adding a runtime failure, a check or an API error has one way to be written: define the code where it is produced, report it with parameters, add its en/zh text.
- The assistant and rule cards diagnose by code and no longer drift with the upstream engines' log wording.
- Docs: the domain glossary's "failure codes and check results" (`CONTEXT.md`), [API conventions](../reference/api-conventions.md).

<!-- translation-source-sha256: 2a15a8a6e1736dfb1647c5bf9dc32dda507aedbee0318c07535b311b1c463ebc # ADR 0067: Runtime failures and check results are codes with parameters; English text is only a fallback -->
<!-- translation-source-sha256: 2be8eb2ce7eb56db7bfbdc0b2f158d01a346465f659f1d1a3db968a8c1a348f8 ## Context -->
<!-- translation-source-sha256: 3eb9abb0e5d016e7233a5d388d19409160a28c21f8a699b53f9050c06f9a48e2 ## Decision -->
<!-- translation-source-sha256: fedeb09fdddecaf4c8efa48e4aa087ecd2e4ccabc264d41aaf6e4d53814f841d ## Trade-offs -->
<!-- translation-source-sha256: 9bdb7566800b24017758583db24697a065d293264e13090693e6c96943980f4f ## Consequences -->
