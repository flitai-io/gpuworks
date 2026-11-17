# SGLang Logging Injection: Behavior and Boundaries

## What It Is and Why It Exists

SGLang's default log format contains only a timestamp and rank prefix, without a level. Actual engine errors therefore look the same as ordinary request logs, so the level filter (`level=warn`) and histogram on the platform's Logs page cannot identify them.

SGLang can read a Python `dictConfig` file through the `SGLANG_LOGGING_CONFIG_PATH` environment variable. The platform uses this capability: for eligible deployments, the SGLang adapter code (`internal/server/deploy/sglang_logging.go`) generates a logging configuration that includes levels and a pre-start script that writes it. The SGLang adapter owns this environment and script; the API, generic deployment workflow, and frontend do not reimplement them (see [ADR 0022](../adr/0022-template-owned-deployment-launch.md)).

For how to choose a log level, see [View Metrics and Search Logs · Set the Engine Log Level](../how-to/monitor-and-logs.md#设置引擎日志级别). This document explains what the platform does automatically, when it does not, and how to take control yourself.

## What Happens Automatically

When the conditions are met, the platform adds two things to the final launch plan. They always appear together:

| Item | Description |
|---|---|
| Environment variable | `SGLANG_LOGGING_CONFIG_PATH=/tmp/gpuworks-sglang-logging.json` |
| Pre-start script | Writes the configuration to the path above on every container start, before the engine command |

The script runs on every start because SGLang treats "variable set but file missing" as a fatal configuration error. The file and variable must therefore both exist.

The configuration does the following:

- Outputs logs in the format `[time process-id] level message` to container stderr, so `docker logs` and the platform's log collector still see the same output stream.
- Sets the root logger's threshold from the final `--log-level`, converted to uppercase.
- Reduces noise: `httpx` and `httpcore` output only WARNING and above; `flashinfer.jit` outputs only ERROR and above.

The deployment command preview shows the command by default. Expand environment variables and preparation steps to see `SGLANG_LOGGING_CONFIG_PATH` and the pre-start script that writes the configuration. Instance details also show the saved script and env. Copying the preview includes all launch steps.

`/tmp/gpuworks-sglang-logging.json` is a configuration file, not a log output file.

## When Injection Does Not Happen

Injection happens only when all the following conditions are met. If any condition is not met, the platform leaves the existing behavior unchanged and the engine uses its own default format. Each restriction addresses an actual failure mode:

| Condition | Why Injection Is Skipped If It Is Not Met |
|---|---|
| Command mode is `engine` (engine-managed) | Commands in `manual` and `image-entry` modes belong to the user and are not rewritten by the platform |
| The launch plan has an explicit entrypoint | The agent's wrapper script must execute the original command according to the entrypoint boundary, which is reliable only when the launch plan specifies that boundary |
| No `pre_start_argv` is present | The agent does not accept both a pre-start script and argv; preparation commands supplied by the template take precedence over a platform-added script |
| No one has set `SGLANG_LOGGING_CONFIG_PATH` | Whoever sets it owns the logging configuration |
| The final command has no `--log-file` | When using a custom logging configuration, SGLang skips its own file handler setup; injection would silently remove file logging |
| The final command has an explicit `--log-level` | The configuration threshold is derived from it; without an explicit value, no configuration is injected and the engine decides the default level (this is what selecting Unset in the form does) |
| There is no user-specified `--config`, and `--log-level` is written in full with a valid value | Abbreviated or invalid level arguments are left for SGLang to interpret, without reinterpretation |
| Launch uses a single process (`tp`/`pp`/`dp`/`ep`/`nnodes`/`moe-dp` are all 1 or unset, and are not symbolic placeholders) | Parallel launches retain upstream rank log prefixes without changing the format |

Even when the configuration is injected, raw log lines written directly by the engine or third-party libraries without a level may still have no level on the Logs page. They appear as plain text and do not match a level filter.

## How to Take Control Yourself

Plans with preparation steps, such as a template with a pre-start script, still allow manual command editing. When you enter edit mode in the command preview on the deployment page:

1. The page puts the **complete script**, including preparation steps and logging configuration, into the manual command and switches the command mode to `manual`.
2. The deployment request sets `container_overrides.command_includes_preparation` to `true`, indicating that this manual script already contains preparation steps. The server therefore does not separately run the template's preparation steps, avoiding duplicate side effects.
3. In `manual` mode, the platform does not inject logging configuration. You control whether to retain, modify, or remove the logging configuration in the script.

A few boundaries apply:

- `command_includes_preparation` is an intent flag in deployment overrides; it cannot be inferred from arbitrary shell text. Older handwritten commands do not have this flag and continue to execute the template's preparation steps as before.
- It is valid only when a manual command is present and the command mode is `manual`; otherwise, validation rejects it.
- Returning to the form from edit mode discards manual overrides and regenerates the command from the form. Automatic injection rules then apply again.

If you only want to use your own logging configuration without taking over the entire script, set `SGLANG_LOGGING_CONFIG_PATH` in the environment variables and ensure the file exists. The platform skips injection when it sees this variable.

<!-- translation-source-sha256: b3e962a8eb98a5f9352bd55244c5bc5755ab2b9528007ae0ea5ea39c9bd992f8 -->
