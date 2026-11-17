# ADR 0051: Agent Launch Configuration Lives in `<state>/gpuworks-agent.conf`; Every Launch Method Uses `run --state`

Date: 2026-10-19. Status: Accepted.

## Background

The agent's launch configuration (server address, node token, connection direction, listen address, runtime driver, mount root, and udocker offline bundle) was previously scattered across the command lines or environments of individual launch methods: systemd units included `--server --token --connect-mode --listen --state` in `ExecStart`; nohup's `agent-restart.sh` hardcoded a set of environment variables and arguments; launchd plists placed the token in `EnvironmentVariables`; and manual `upgrade` assembled the arguments again to restart the new process. The consequences were:

- The same configuration was written in four or five forms. Changing one option (such as the server_dial listen address) required changes in several places, and defaults were duplicated in the agent, installation script, API, and each cloud provider driver (`:19090`).
- The token appeared in `ps`, `systemctl cat`, and unit files, readable by all local users.
- Every piece of code that needed to determine “which server / token the agent on this host belongs to” (replacement warnings in the installation script, `Existing agent` in prechecks, identifying the agent to stop by token during uninstall, and choosing systemd or nohup for out-of-band recovery) parsed command lines for the various launch methods, each with its own grep logic.
- A `server_dial` agent never connects to the server but was still required to provide `--server`, creating a meaningless required field.

## Decisions

1. **Store configuration in one file only**: `gpuworks-agent.conf` in the state directory, with permissions `0600`, using one `KEY=value` per line (`SERVER`, `TOKEN`, `CONNECT_MODE`, `LISTEN`, `RUNTIME_DRIVER`, `MOUNT_ROOT`, `UDOCKER_BUNDLE`). Values are read literally, and unknown keys are ignored. Fields, validation, reading, writing, and defaults (`agentconf.DefaultListen` = `0.0.0.0:18800`) are defined once in the leaf package `internal/agentconf`, shared by the agent, the server (to validate values passed to the installation script), and the installation script.
2. **Use only one writer**: `gpuworks-agent configure --state DIR …` reads the token from standard input, writes a temporary file, and then renames it, replacing the entire configuration. The installation script, SSH wizard, and offline bundle all invoke it through the installation script.
3. **Every launch method runs only `gpuworks-agent run --state DIR`**: systemd unit `ExecStart`, nohup and `agent-restart.sh`, launchd `ProgramArguments`, and restarts after manual upgrades. The token no longer appears in any command line, unit, or plist.
4. **Value precedence** (the same for `run`, `smoke`, and `upgrade`): command-line flags > environment variables > configuration file > defaults. Flags and environment variables remain available for temporary overrides and testing; they predate the file, so they take precedence over it.
5. **A missing file is not an error**: agents installed earlier do not have this file and continue running from their flags and environment. Upgrading the binary does not change their launch method; rerunning installation migrates them to the file.
6. **A `server_dial` agent does not use a server address**: it does not include `--server`, the API does not save `server_url` for this type of node (regardless of what the console sends), and `run` needs only a token and listen address. Manual `upgrade` has no download source and rejects the operation immediately, explaining that the server must push the upgrade.
7. **Uninstall also deletes this file** because it contains a token.

## Compatibility Rules

For an extended period, readers that identify agents on a host must handle both agents with a configuration file and agents installed earlier whose arguments remain on the command line. Therefore:

- Readers accept both forms: first read the configuration file in the directory specified by command-line `--state`; if the file does not exist, fall back to command-line `--server`, `--token`, `--connect-mode`, and `--listen`. The shell side has only one implementation, `agentconf.ShellFuncs` (`gw_conf`, `gw_line_value`, `gw_unit_value`, `gw_unit_state`), inlined by the installation script, prechecks, uninstall checks, and liveness checks, instead of each parsing command lines independently. The Go side follows the same rule: flags for `run`/`smoke`/`upgrade` remain valid; `upgrade` still restarts agents without a file using the full arguments; `launchd.ReadJob` still reads old plists.
- This command-line-reading code exists only for older agents. **The condition for deleting it (the command-line branches of `agentconf.ShellFuncs`, old-plist parsing in `launchd.ReadJob`, and the full-argument restart form in `upgrade` for agents without a file) is**: the oldest agent version with which the server must still communicate is newer than the version that introduced this configuration file (the first release containing the commit “Record the agent's launch configuration in `<state>/gpuworks-agent.conf`,” as identified by the release version reported by `gpuworks-agent version`). In other words, it can be deleted only after agents from before the file's introduction are no longer supported.
- There is currently no enforced minimum version check between the server and agent (see [Upgrades and Version Compatibility](../how-to/upgrade-and-compatibility.md)), so “the oldest supported agent version” has no machine-readable value yet. Before deleting the code, first introduce and document this minimum, then meet the condition above. Until then, retain both forms. Existing older agents continue to work without reinstallation, but their tokens remain on their command lines until installation is rerun.

## Trade-offs and Consequences

- There is one more file containing a token: it has permissions `0600`, resides in the state directory alongside `identity.json`, and is removed on uninstall. In exchange, the token leaves `ps`, unit files, and plists, a net benefit.
- Changing configuration requires rerunning installation (or `configure`) and then restarting the agent; editing the unit file is no longer the method. This is intentional: there is only one write location.
- Launch configuration does not enter the database, and no copy is saved on the server: facts such as the node's `connect_mode` and `server_dial_listen` already exist in the node record. The file simply lets the node start itself without requesting them from anyone else.
- `--data-lane` and `--dcgm` are not stored in the file: the former is a troubleshooting switch, and the latter already has an environment variable. Neither is an installation-time decision.

## Rejected Alternatives

- **Keep configuration in argv and the environment, merely centralizing its spelling in one function**: this removes duplication but does not solve token exposure or the need to read another process's command line. Parsers would still need to handle three formats: units, plists, and scripts.
- **Place a sidecar file beside each binary (`<install-dir>/gpuworks-agent.conf`)**: multiple agents can run on one host (one per installation directory in nohup mode), so one configuration per installation directory is semantically reasonable. However, the agent already uses its state directory as its identity (`identity.json`, logs, models, and PID all live there, and `uninstall` and `runveil` also locate agents by state). Tying configuration to the binary creates ambiguity when two binaries use the same state directory with two configurations, and upgrades that replace the binary must also consider whether to move the file. Tying it to state matches the existing identity and cleanup boundaries.

<!-- translation-source-sha256: 8afb37ef2be3a4cccd1bd6ad64dbf5bc3b4f65dece1e7d84767a445e24c42795 -->
