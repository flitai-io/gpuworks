# ADR 0013: Three Command Modes for Container Command Assembly Based on Image Source

Date: 2026-08-23 · Status: Accepted (M20/M21 acceptance grill Q17/Q25). 2026-09-15: Under ADR 0020, `manual` mode is the only path that bypasses the parameter dictionary—the command text is the source of truth, estimation does not apply, and placement inputs still come from the form; engine parameters in `engine` and `image-entry` modes all come from a single launch configuration.

2026-09-19: [ADR 0022](0022-template-owned-deployment-launch.md) adjusts the default command mode and configuration source for new deployments; existing deployments retain their original semantics.

## Context

M19 narrows image entrypoint/cmd to display-only information (the engine builder's complete command takes over unconditionally, `controller.go:540-566`), preventing an entrypoint populated by inspect from unexpectedly overriding the engine command. Acceptance testing reveals that this blanket approach does not work for community/custom images: the platform cannot guarantee the contents of external images. Community images such as `vllm/vllm-openai` normally use the image's built-in entrypoint as the launcher and cmd as its arguments, while model-specific images (such as glm-4.6-sm120) may hardcode the entire command. These three image types need three assembly semantics, and image entrypoints vary too widely to infer the correct one automatically.

## Decision

Add a `command_mode` field to the image RunSpec, with three options. The source determines the default, which you can change:

1. `engine` (engine takes over; default for system images): use the builder's complete command and ignore the image entrypoint—the contents of system images are controlled and predictable;
2. `image-entry` (image entry + engine arguments; default for community images): leave the entrypoint unchanged and have the builder produce only arguments to append as cmd. The engine Profile defines which token begins the arguments (whether to include the `serve` subcommand) according to the conventions of that engine's community image;
3. `manual` (fully custom; available for custom images): inject no engine arguments; run the image entrypoint+cmd unchanged or use a command written by the user.

Instance-level entrypoint/cmd overrides remain available and are independent of the mode. The deployment page's “Final execution preview” uses the same assembly code as the dispatched InstanceDesiredState to show the actual result of each mode.

## Consequences

- (+) Community/custom images work out of the box, without being incorrectly overridden by engine commands;
- (+) Experts can select manual for special images and configure their arguments so beginners can deploy them directly;
- (−) Engine Profiles must maintain the community image argument conventions for each engine and track changes to upstream conventions;
- (−) In image-entry mode, compatibility between engine arguments and an unknown entrypoint cannot be guaranteed statically; preview and post-deployment logs provide the fallback for troubleshooting.

Rejected alternatives: retain unconditional engine takeover (incorrect semantics for community images); automatically detect entrypoint structure (too varied to be reliable).

<!-- translation-source-sha256: 638c04b4e137640c81bc2687b2067162c17d3bdb672dea1516f982a5eb567610 -->
