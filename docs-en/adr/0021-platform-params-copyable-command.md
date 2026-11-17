# ADR 0021: Separate Platform Parameters From Engine Parameters—Displayed Commands Must Run When Copied

Date: 2026-09-16 · Status: Accepted (supplements ADR 0020 decision 1; other decisions remain unchanged)

## Context

Disk KV offloading needs a "disk cache capacity limit" parameter, consumed by the agent's cleanup goroutine; vLLM has no corresponding flag. The parameter dictionary (`deploy/param_specs.go`) currently exposes only keys with engine flags: `engineParams` skips rows without flags, and `Engines` is derived from the columns containing flags. This creates a tempting shortcut: assign the disk layer's composite flag `--kv-transfer-config` to this key, use the "same flag means companion keys" inference to place it automatically among disk-layer fields, and then quietly omit it from JSON in the emission function.

The result makes the flag column untruthful: `/engines` would claim that a key is carried by `--kv-transfer-config`, although the engine never receives it. The "same flag" inference would have two meanings from then on, and the next platform parameter would either copy this exception or require a proper mechanism later. More fundamentally, this undermines a product promise: you can copy the command shown on the deployment page, paste it onto a machine, and run it. It cannot contain anything vLLM does not recognize.

## Decision

1. **Flag columns represent only real engine flags.** A dictionary row's `VLLMFlag`/`SGLangFlag` and their disable forms always mean "the flag that carries this key on the engine command line." Do not assign a flag that the engine does not accept for grouping, interdependent behavior, or form layout. Every member key of a composite flag (such as `--kv-transfer-config`) must actually be written into that flag's value.
2. **Platform parameters are first-class dictionary members, but never enter the command.** Dictionary rows explicitly declare "consumed by the platform" and list applicable engines. `engineParams` exposes them, and `Materialize` includes them in `LaunchConfig`, but argv assembly, `ReverseArgs`, command previews, and template deviation comparison never see them. The API and form handle them like ordinary parameters (explicit value > dictionary default; for parameters marked `Automatic`, the server derives values from model facts rather than inventing numbers).
3. **The displayed command is the executable command.** The launch command in command previews and instance details is token-for-token identical to the argv the container actually receives. Tests enforce this: neither the key name nor the value of any platform parameter may appear in preview text.
4. **Platform parameters are not coupled to engine parameters.** Each platform parameter stands independently: if you enter a capacity limit without enabling the disk layer, the value is saved normally, without errors or side effects. The platform consumes it only when the relevant engine parameter actually takes effect (the directory is created and monitored only when `shared_kv_dir` exists). The form does not introduce "show B only when A is enabled" coupling, avoiding another copy of key relationships.

## Consequences

- Benefits: every flag in `/engines` is trustworthy; the promise that copied command previews run is testable; adding a platform parameter requires only one dictionary row and one marker.
- Costs and constraints: `ParamDefinition` gains a platform marker and an explicit engine list (new structure is necessary because neither "this key is not sent to the engine" nor "which engines it applies to" can be derived from existing data). The frontend can no longer group platform parameters under a layer based on "the same flag"; platform parameters render normally according to their `Group`.
- First example: `kv_disk_limit_gib` (disk KV cache capacity limit, in GiB), marked `Automatic`. When unset, it is twice the GPU memory occupied by model weights; you can enter a value manually.

<!-- translation-source-sha256: 828eff4fa613639a87ac7001f33232e7d5077ab5fe2bbae0e316f8ce56eb2dfd -->
