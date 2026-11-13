# CLI Reference

GPUWorks provides two command-line programs: `gpuworks` installs, runs, upgrades, and maintains the control plane; `gpuworks-agent`
connects GPU nodes, checks node environments, troubleshoots containers, and runs benchmarks.

This page lists flags, defaults, and key behavior by subcommand. Each command's `--help` (`-h`) output is authoritative. Flag definitions are in
`cmd/gpuworks` and `cmd/gpuworks-agent`, respectively. For environment variables, see the [Configuration Reference](configuration.md).

## Example: Restart the Control Plane

After modifying `server.yaml`, run the following command on **the machine where the control plane is installed** to apply the configuration. You need administrative privileges for that installation.
The command briefly stops the control plane service, starts it using the existing service mode, and checks that it recovers.
The `gpuworks` command must be available in the current `PATH`; otherwise, use its full path in the installation directory, such as `/opt/gpuworks/server/gpuworks restart`.

```bash
gpuworks restart
```

On success, the output looks like this, with the installation's current service mode in parentheses:

```text
gpuworks restarted (systemd)
```

To specify another installation directory, append `--dir /path/to/installation` to the command. For all options and restrictions, see
[`gpuworks restart`](#gpuworks-restart) below. Run `gpuworks restart --help` to view command help.

## Command Overview

| Binary | Subcommand | Purpose | Who Uses It and When |
|---|---|---|---|
| `gpuworks` | [`serve`](#gpuworks-serve) | Run the control plane (Web UI, management API, and OpenAI-compatible gateway) | Started by a systemd unit or nohup; run manually during development |
| | [`install`](#gpuworks-install) | Install itself as a service, write `server.yaml`, start the service, and check health | Operators, for initial deployment or installation repair |
| | [`upgrade`](#gpuworks-upgrade) | Upgrade the installed control plane from a release channel, internal release site, or local release archive, with automatic rollback on failure | Operators, when upgrading the control plane |
| | [`restart`](#gpuworks-restart) | Restart the installed control plane using its current service mode | Operators, after modifying `server.yaml` |
| | [`stop`](#gpuworks-stop) | Stop the installed control plane using its current service mode | Operators, before maintenance or shutdown |
| | [`uninstall`](#gpuworks-uninstall) | Uninstall the control plane, optionally removing configuration and data | Operators, during migration or decommissioning |
| | [`expose`](#gpuworks-expose) | Publish a public endpoint for the console | Operators, when the control plane is behind NAT |
| | [`migrate`](#gpuworks-migrate) | Run migrations and seed the built-in catalog in the specified SQLite database, then exit | Operators/developers, for offline checks or database premigration |
| | [`reset-password`](#gpuworks-reset-password) | Reset the administrator account password | Operators, when the password is forgotten |
| | `version` | Print the version | Anyone |
| | `console-upgrade-protocol` | Print the console upgrade protocol number supported by this version (currently `1`); versions predating console upgrades do not have this subcommand | Before a console upgrade, the server invokes it on the new binary to be installed |
| `gpuworks-agent` | [`configure`](#gpuworks-agent-configure) | Write startup configuration to `<state>/gpuworks-agent.conf` | Called by the installation script; used for manually connected nodes |
| | [`run`](#gpuworks-agent-run) | Connect to the server and provide node capabilities | Started by the installation script/unit; usually not run manually |
| | [`smoke`](#gpuworks-agent-smoke) | Validate a single connection handshake, then exit | Validation before agent self-upgrade; connectivity troubleshooting |
| | [`upgrade`](#gpuworks-agent-upgrade) | Download a new agent from the server, validate it, and replace the existing agent in place | Node administrators, for manual agent upgrades |
| | [`doctor`](#gpuworks-agent-doctor) | Print node environment self-check results | Troubleshooting node connection, GPU, or runtime issues |
| | [`grant-device-access`](#gpuworks-agent-grant-device-access) | Give a non-root runtime user access to accelerator device nodes | Called automatically by the installation script; used to manually grant missing permissions on rootless runveil nodes |
| | [`bench`](#gpuworks-agent-bench) | Load-test an OpenAI-compatible endpoint | Performance evaluation; the command-line equivalent of the Benchmarks page |
| | [`runveil`](#gpuworks-agent-runveil) | Inspect or enter runveil containers, import image archives | Troubleshooting on runveil nodes; importing an image archive copied onto the node |
| | [`install-service`](#gpuworks-agent-install-service) | Register this binary as a macOS launchd job | Called by the installation script on Mac |
| | [`uninstall`](#gpuworks-agent-uninstall) | Stop and remove the agent service | Decommissioning a node |
| | `version` | Print the version | Anyone |
| Node installation script | [`/install.sh`](#节点安装脚本-installsh) | The `curl \| sh` bootstrap script served by the server | Copy the installation command when adding a node |
| Other | [`gpuworks-image-manifest`](#gpuworks-image-manifest), [`gpuworks-dist`](#gpuworks-dist), [`template-probe`](#template-probe), [`gpuworks-catalog-model-config`](#gpuworks-catalog-model-config) | Catalog, image, and release channel maintenance tools | Catalog maintainers and CI, run from source |

Both `gpuworks` and `gpuworks-agent` print usage and exit with code 2 when no subcommand or an unknown subcommand is supplied. To start the agent, explicitly use `gpuworks-agent run`, even if the server and token are already set through environment variables or a configuration file. Subcommand flags must follow the subcommand. `--help` (`-h`) prints help and exits with code 0.

## gpuworks serve

Run the control plane. Without `--config`, all functionality is served on `--addr`. With `--config`, the specified `server.yaml` is read
(for keys, see the [Configuration Reference](configuration.md#server-配置文件-serveryaml)), and command-line options override the corresponding values in the file.

| Flag | Default | Description |
|---|---|---|
| `--config PATH` | Empty | Path to `server.yaml` |
| `--addr ADDR` | `:8800` | Serve all functionality on this address, replacing the entire `listen` section in the file |
| `--data DIR` | `./data` | Data directory |
| `--db PATH` | `<data>/gpuworks.db` | SQLite path |
| `--trusted-proxies CIDRS` | Empty | Comma-separated proxy IPs/CIDRs allowed to supply `X-Forwarded-For` |
| `--metrics-retention DURATION` | `72h` | Retention window for raw metrics and log lines |
| `--log-file PATH` | `<data>/logs/gpuworks-server.log` | Log file; an empty string is treated as unspecified and still writes to the default path |
| `--log-max-size SIZE` | `10MB` | Log rotation threshold (KB/MB/GB, base 1024) |
| `--log-keep N` | `20` | Number of rotated log files to retain |
| `--log-level LEVEL` | `info` | `debug` / `info` / `warn` / `error`; other values are treated as `info` |
| `--proxy-max-body-bytes BYTES` | `268435456` (256 MiB) | `/v1` request body limit; exceeding it returns 413 |
| `--recovery-enabled BOOL` | `true` | Enable out-of-band SSH recovery for offline agents |
| `--recovery-grace-period DURATION` | `2m` | Wait time after disconnection before starting SSH recovery |
| `--recovery-initial-interval DURATION` | `30s` | Initial recovery retry interval |
| `--recovery-backoff-multiplier FLOAT` | `2` | Multiplier for the recovery retry interval |
| `--recovery-max-interval DURATION` | `10m` | Maximum recovery retry interval |
| `--recovery-restart-wait DURATION` | `1m` | Wait time for the agent to reconnect after restart or reinstallation |
| `--stale-check-interval DURATION` | `30s` | Interval for checking whether online nodes have become stale |
| `--stale-after DURATION` | `90s` | Mark a node offline when no heartbeat arrives within this duration |
| `--server-dial-reconnect-interval DURATION` | `10s` | Reconnection scan interval for server_dial nodes |
| `--supervise` | `false` | Run as a supervisor instead of running the server directly: start the server using the same binary path and arguments with this flag removed, and restart it 3 seconds after it exits |

Key behavior:

- For the mapping between file keys and flags and for listening rules, see the [Configuration Reference](configuration.md). `--recovery-*`, `--stale-*`, `--proxy-max-body-bytes`, and `--server-dial-reconnect-interval` have no file keys; their defaults are the production values.
- Logs are written to both stderr and a file, with size-based rotation. Logging is configured only through flags and `server.yaml`, with no environment variables.
- The server reads the operator-facing `GPUWORKS_ADMIN_PASSWORD` variable (the initial password when admin is first created) and the `GPUWORKS_USAGE_REPORT=off` override (which disables usage reporting). There is also an internal marker, `GPUWORKS_SUPERVISED=1`, set by the supervisor for its child process (the server removes it from its own environment after reading it and does not pass it to processes it starts). `GPUWORKS_ADDR`, `GPUWORKS_DATA_DIR`, and `GPUWORKS_TRUSTED_PROXIES` are used only as compatibility bridges for legacy units; see the Configuration Reference for details.
- On `SIGUSR1`, a loopback-only pprof listener is temporarily opened (first trying `127.0.0.1:6060`, then a random port if it is occupied) and automatically closed after 10 minutes. The actual address is recorded in the log.
- `SIGINT`/`SIGTERM` trigger graceful shutdown. When the process runs under systemd (`INVOCATION_ID` is present) or is started with `--supervise`, the console's "Restart now" action and console upgrades can make it exit and be restarted by `Restart=always` or the supervisor.

### Process Behavior of `--supervise`

With `--supervise`, the parent process supervises the server child process without reading configuration or opening the database:

- It normally loops through starting the server, waiting for it to exit, and starting it again after 3 seconds. Each start uses the installation path. After a console upgrade renames the new binary to this path, the next start runs the new version, while the parent process continues running the version it originally started with.
- On SIGINT/SIGTERM, it forwards the signal to the server, waits for the server to exit, and then exits with code 0.
- On Linux, the server has a parent-death signal: when the parent is killed by SIGKILL, the server is killed as well. When the parent runs as PID 1, it reaps all orphan processes.
- When the directory referenced by `--config` is the installation directory containing this binary, the parent writes its PID to `<dir>/gpuworks.pid` and removes it on clean exit.

## gpuworks install

Install the current binary as a service: copy it to `--dir`, write `server.yaml` and the service definition, start the service, and check health. For instructions, see [Install and Upgrade the Control Plane](../how-to/install-server.md). For the directory layout, permissions, and credential file, see [Control Plane Installation Layout](install-layout.md).

| Flag | Default | Description |
|---|---|---|
| `--dir DIR` | `/opt/gpuworks/server`; `~/gpuworks/server` on macOS | Installation directory; a leading `~` in `--dir`, `--data`, and `--logs` expands to the home directory |
| `--data DIR` | `<dir>/data` | Data directory; an existing installation retains the value in its `server.yaml` |
| `--logs DIR` | `<dir>/logs` | Log directory; an existing installation retains its current value |
| `--addr IP:PORT` | Default route's outbound IP, port 8800 | Listen address (serving all functionality); specifying only `:port` means `0.0.0.0` |
| `--trusted-proxies CIDRS` | `127.0.0.1/32,::1/128` | Proxies allowed to supply `X-Forwarded-For` |
| `--password PW` | None | Administrator password; see the precedence below when omitted |
| `--service-user USER` | `gpuworks` | systemd service account; `root` means no dedicated account is created. Ignored on macOS: the launch daemon runs as the user performing the installation |
| `--log-max-size SIZE` | `10MB` | Log rotation threshold |
| `--log-keep N` | `20` | Number of rotated log files to retain |
| `--quiet` | `false` | Do not print a banner or prompt; generate a random password if none is supplied (the final summary is still printed) |
| `--autostart` | `true` on Linux, `false` on macOS | Install a systemd unit (a launch daemon on macOS) that starts at boot; an error if given together with `--no-systemd` |
| `--no-systemd` | `false` | Do not use a service manager (systemd, or launchd on macOS); start in the background with nohup instead |
| `--skip-check` | `false` | Downgrade the preflight checks that predict whether the server can run (port in use, disk space, whether the service account can enter the installation directory) to warnings and continue; checks that protect the host are unaffected |

Key behavior:

- Preflight checks: root or usable `sudo` is required (otherwise, fall back to nohup with a warning). Fail if another process occupies the target port, except when it is the current installation. The filesystem containing the installation directory must have at least 2 GiB free. In systemd mode, every existing directory above the installation, data and log directories must let `--service-user` enter it (the `x` bit, judged by owner/group/other; directories with an ACL are not refused), otherwise the install fails, for example under `/root` (`0700`). `--skip-check` downgrades these three checks to warnings; the checks that refuse recursive permission changes on broad directories such as `/`, `/var` or a home directory, or that would widen a sensitive file's ACL, cannot be skipped.
- Privilege escalation: install/upgrade/reset-password run as the operator, with each privileged step (creating directories and accounts, writing units, and `systemctl`) executed through a separate `sudo` command. `sudo` prompts for a password once and caches it; without a terminal or cached credentials, `sudo` fails immediately instead of hanging.
- systemd mode: create the `--service-user` system account (or reuse it if it exists) and add the operator to its group (the current terminal requires `newgrp` or a new login). Subsequent upgrade backups and binary replacement no longer require `sudo`, but `systemctl` and the final `chown`/`chmod` still do; reset-password does not.
- nohup mode (`--no-systemd`, no running systemd, no root/`sudo`, or no `--autostart` on macOS): ignore `--service-user` (with a warning only if it was given explicitly) and run `serve --supervise --config <dir>/server.yaml` in the background as the invoking user. The supervisor restarts the server when it exits, so console upgrades are supported. It does not automatically recover after a machine or container restart. Start it with `<dir>/gpuworks restart` and stop it with `<dir>/gpuworks stop`, or set this command as the container startup command; see [Run in a Container](../how-to/install-server.md#在容器里运行). On macOS, run `<dir>/gpuworks install --autostart` to switch to starting at boot. The installation summary prints these commands.
- macOS: runs in nohup mode by default, and the default installation directory is `~/gpuworks/server` (the home directory of the user performing the installation, or of `SUDO_USER` under `sudo`). launchd mode (with root/`sudo` and `--autostart`; kept without it when reinstalling over an installation that has a launch daemon) writes `/Library/LaunchDaemons/ai.gpuworks.server.plist` and runs `launchctl bootstrap`. The service runs as the user performing the installation, starts at boot, and is restarted by launchd after exit (supporting console upgrades), and the installation tree belongs to that user. For plist contents, see [Control Plane Installation Layout · launchd Plist](install-layout.md#launchd-plistmacos).
- Interaction: without `--quiet`, print a banner first (block characters when output is a terminal and the locale is UTF-8, otherwise plain ASCII). In a terminal, then ask for the installation directory (a leading `~` in the answer is expanded as well), listen address, whether to start automatically (yes/no; default no on macOS), and finally the administrator password. Items already supplied on the command line are not requested again. Press Enter to accept the default in parentheses (the existing installation's current value or the static default).
- Administrator password precedence: `--password` > environment variable `GPUWORKS_ADMIN_PASSWORD` (displayed and confirmed during interaction) > interactive prompt (Enter generates a random password) > random generation. Write it to `<data>/initial-credentials` (`0600` by default; `0660` for the trusted Linux platform group), not the unit file. If `gpuworks.db` already exists in the data directory, password processing is skipped; `--password` is ignored with a message to use `reset-password`.
- Running the command again repairs the installation: retain the existing `server.yaml` (overwrite values only when explicitly supplied on the command line), replace the binary, recreate the service, and restart it, preserving the contents of the database, `master.key`, and admin account while hardening sensitive file permissions.
- The server does not manage its own external address: APIs for adding nodes, cloud provider installation, and similar operations always require the caller to explicitly supply `server_url`.

## gpuworks upgrade

Fetch the current version from a [release channel](../adr/0040-signed-multi-source-distribution.md) (or use `--base-url` for an internal release site or `--archive` for a local release archive) to replace the installed control plane: stop the service, back up, replace atomically, and check health (60 seconds). On failure, automatically roll back and exit with a nonzero code.

| Flag | Default | Description |
|---|---|---|
| `--dir DIR` | The installation containing this binary (its directory contains `server.yaml` or `install.json`), otherwise the default installation directory (`/opt/gpuworks/server`; `~/gpuworks/server` on macOS) | Installation directory |
| `--data DIR` / `--logs DIR` | Directories recorded at installation | Override the data/log directories |
| `--channel NAME` | `update_channel` in `server.yaml`, or the build-time channel if absent (`nightly` for master builds, `release` for `v*` tag builds) | Channel whose release index to read: `release` (stable releases) or `nightly` (rolling previews); applies only to release channels |
| `--source URL` | Three built-in public sources | Source in release channel format (`<URL>/<channel>/index.json`); repeatable, replaces the built-in sources; used for customer-hosted mirrors |
| `--base-url URL` | Empty | Read from a release site instead (`<URL>/data/release.json` and `<URL>/downloads/`), such as one hosted on an internal network |
| `--archive FILE` | Empty | Local release archive `gpuworks-<release_id>-<os>-<arch>.tar.gz`; makes no network requests |
| `--version ID` | Empty | Expected release ID; error if it differs from the current channel/site version or the archive version, rather than silently upgrading to another version |
| `--keep N` | `3` | Number of pre-upgrade backup sets to retain |
| `--force` | `false` | Allow replacement of a local build without a release ID (`dev` / `-dirty`) |
| `--check` | `false` | Only report whether a newer version is available; make no changes |
| `--quiet` | `false` | Output only warnings and errors; no output on success |

Key behavior:

- If `--dir` has no `server.yaml` but contains an `install.json` left by a version predating `server.yaml`, first write its listen address, data/log directories, log rotation, and trusted proxies to `server.yaml`; rewrite the unit that passes configuration through environment variables to use `serve --config`; delete `install.json`; then continue. Only the installed binary can perform this step (`<dir>/gpuworks upgrade` / `restart`); another binary exits with an error without making changes. If neither file exists, the installation is considered unadopted; an error instructs you to run `install` first. If already up to date, print a message and exit with code 0.
- Without `--dir`, operate on the installation containing the binary being executed: `/opt/gpuworks1/gpuworks upgrade` upgrades `/opt/gpuworks1` without touching another installation in the default directory. A binary downloaded elsewhere still operates on the default installation directory. At startup, print `upgrading the installation at <dir> (<current-version>, <service-mode>)`.
- After stopping the service, first confirm that the listen address in `server.yaml` has been released. If it is still occupied (for example, another installation on the same machine listens on the same port), fail immediately without backing up or replacing anything, and restart the stopped service unchanged. On Linux, report the occupying process's PID and command line; if it is another gpuworks installation, suggest `--dir <its-install-dir>`. If the process cannot be found, report the version returned by `/healthz` at that address.
- Local builds (`dev` or `-dirty`) are rejected unless `--force` is supplied.
- `--source`, `--base-url`, and `--archive` are mutually exclusive. `--channel` can only be used with release channels (the default or `--source`).
- If the channel version is older than the current build (for example, when switching from nightly to release), do not downgrade; print a message and exit with code 0. Supply `--force` to install it.
- Release channels: request `<channel>/index.json` from all sources simultaneously and use the first one whose signature passes verification (ed25519, with the public key compiled into the binary; the source itself does not need to be trusted). Download the archive from that source first. On failure or no new data for 30 seconds, switch to the next source and resume using Range. Finally, verify the size and sha256 against the index. The output identifies the source used. Built-in sources are Alibaba Cloud OSS (`https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com`), Huawei Cloud OBS (`https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`), and Cloudflare R2 (`https://dl.gpuworks.ai`).
- `--base-url`: the release site's manifest is unsigned; trust comes from the site's TLS. Verify the archive against the sha256 in the manifest. This mode is for internal release sites.
- `--archive`: extract `gpuworks` from the archive to `<dir>/tmp/`, run its `version` once, and use the release ID in the output as the target version. Subsequent steps match the online path. Before stopping the service, reject archives that cannot be extracted, have the wrong architecture (check the ELF header first rather than attempting execution, so hosts with qemu binfmt installed are also protected), cannot run, or do not report a release ID (local builds). Keep the release archive unchanged. Integrity relies on gzip's built-in check; after copying the archive to the host, verify sha256 yourself against the release's `SHA256SUMS`.
- Use `<dir>/.upgrade.lock` for mutual exclusion; concurrent upgrades fail immediately. Backups include the database, `master.key`, `server.yaml`, and binary.
- Rollback restores the pre-upgrade snapshot, losing writes made during the upgrade window. The backup path is still printed after rollback. Each installation/upgrade appends an audit entry to `<data>/install-history.jsonl`.
- Only the control plane is upgraded; agents on nodes are not automatically upgraded with it.

## gpuworks restart

| Flag | Default | Description |
|---|---|---|
| `--dir DIR` | The installation containing this binary (its directory contains `server.yaml` or `install.json`), otherwise the default installation directory (`/opt/gpuworks/server`; `~/gpuworks/server` on macOS) | Installation directory |

- systemd installations use `systemctl stop` + `systemctl restart`. nohup installations first terminate the old supervisor (the server exits with it), then run `serve --supervise --config <dir>/server.yaml` in the background. The command is refused when the supervisor is PID 1 (the container startup command), because stopping it stops the container. `upgrade` and repeated `install` are also refused in this case.
- Both modes first stop the service and confirm that the listen address has been released (if still occupied, report the occupant and fail as with `upgrade`, without restarting), then start the service and wait for `/healthz` to report the installed binary's version. A response from another server at that address does not count as recovery. Use this command after editing `server.yaml` to apply the configuration.
- Installations predating `server.yaml` (with only `install.json`) are converted to `server.yaml` before restart; the installed binary must perform this operation. `install` also converts such installations first.
- launchd installations (macOS) use `launchctl bootout`, then `bootstrap` the plist again, applying plist changes.

## gpuworks uninstall

```bash
sudo gpuworks uninstall --dir /opt/gpuworks/server                 # Choose whether to remove data interactively, then confirm uninstallation
sudo gpuworks uninstall --dir /opt/gpuworks/server --yes           # Automation: retain data
sudo gpuworks uninstall --dir /opt/gpuworks/server --purge --yes    # Automation: remove data
gpuworks uninstall --dir /opt/gpuworks/server --purge --dry-run --json
```

| Option | Default | Description |
|---|---|---|
| `--dir DIR` | Same as `restart` | Target installation directory; external data and log directories in the configuration are also recognized |
| `--purge` | `false` | Delete this installation's configuration, data (including the database and `master.key`), backups, and logs; this cannot be undone |
| `--keep-resources` | `false` | Explicitly remove only the component while retaining workloads, nodes, and cloud resources; bypasses resource gates only, never path, process, or service ownership checks |
| `--yes` / `-y` | `false` | Skip interactive selection and final confirmation; execute according to explicit options |
| `--dry-run` | `false` | Print only the cleanup list without stopping services or modifying files; no confirmation required |
| `--json` | `false` | Return only a structured report on stdout; except for previews, also requires `--yes` |
| `--quiet` | `false` | Hide ordinary explanatory output; prompts and confirmation still appear, so this does not replace `--yes` |

Before uninstalling, use `--dry-run --json` to inspect the `checklist`. Each item has a stable `id`, a `status`, a summary, evidence details, and a suggested action. Statuses are `pass`, `warn`, `blocked`, or `unknown`. Completing a preview does not mean that every check passed.

The checklist distinguishes local services and autostart, listening ports, installation/data/backup/log paths and retained items, and deployment, instance, node, and cloud records in the database. Persisted records do not prove that remote processes stopped, GPUs were released, or cloud billing ended. By default, active workloads or unavailable critical resource evidence block uninstallation before services stop or files are deleted. `--yes` skips interaction, not resource checks. Resources are checked again after confirmation and before stopping the service.

To retain workloads while removing only the control plane, explicitly add `--keep-resources`. It does not stop inference instances, delete cloud resources, or replace node uninstallation. Combining it with `--purge` also removes management records and credentials, so first preserve the information needed to manage those resources independently. Passing resource checks does not replace the existing directory and service ownership protections.

- Terminal interaction retains data by default. Without explicit `--purge`, first ask whether to clean up completely, then show the list and request confirmation. Pipelines, scripts, and CI must supply `--yes` or `--dry-run`; otherwise, the command fails before making changes.
- Stop the server and confirm it has exited, disable automatic startup, and remove the associated systemd unit / launchd plist / nohup PID and program files. The associated expose public endpoint is also closed and its unit removed. Services for other installations are retained; cleanup is refused when ownership cannot be determined.
- By default, retain configuration, data, backups, logs, and the service account for reinstallation and recovery. `--purge` removes only known installation assets and retains unknown files. Keep `.upgrade.lock` and its installation directory as mutual-exclusion lock anchors to prevent concurrent upgrades/uninstalls from obtaining different lock files.
- Cleanup refuses dangerous root directories, user home directories, symlinks, mount points, and data paths that encompass the installation directory. A nonempty data directory must contain a recognizable GPUWorks database or master key. Service definitions or data whose ownership cannot be determined require separate inspection first; the command returns failure and retains files not yet removed.
- The service account, group, and home directory are not automatically deleted: the account may have existed before installation or be reused by an agent on the same machine. The report lists retained items. If the account home is the installation directory itself, known assets within it are still cleaned up according to the rules above.
- Remote agents, model services, cloud rental machines, and Cloudflare tunnels / DNS are not automatically deleted and must be handled separately; cloud resources may continue to incur charges.
- Exit code `0` indicates that the selected uninstall operation is complete; `1` indicates failure. Run the command again to retry. On failure, retain content that has not yet been safely removed and return the cleanup results completed so far. The `--json` report includes `success`, `error` (on failure), `layout`, `mode`, `service_user`, `purge`, `dry_run`, `already_absent`, `removed`, and `kept`; paths reflect the actual installation.

## gpuworks stop

| Flag | Default | Description |
|---|---|---|
| `--dir DIR` | Same as `restart` | Installation directory |

- systemd installations use `systemctl stop`, and launchd installations use `launchctl bootout`. The unit or plist remains in place, so the service starts normally after a machine reboot; `gpuworks restart` starts it immediately. nohup installations terminate the supervisor (the server exits with it), after which only `gpuworks restart` can start it again.
- Return after the server exits. Refuse execution when the supervisor is PID 1 in a container, for the same reason as `restart`.

## gpuworks expose

Publish a public endpoint for the console: one endpoint per data directory, with only the provider to select. See [Give the Console a Public Address](../how-to/expose-public-address.md).

| Subcommand | Specific Flags | Description |
|---|---|---|
| `expose up` | `--provider` (defaults to the saved selection, or `cloudflare-quick` otherwise), `--systemd`, `--origin URL` | Submit configuration, start the connector service (a background process by default; `--systemd` installs `gpuworks-expose.service`), wait for the endpoint to be ready, then print its address |
| `expose status` | `--url-only` | Print the provider, desired state, current state, and address; `--url-only` writes the address to stdout only when ready |
| `expose down` | None | Close the endpoint: stop cloudflared, after which the connector service exits (wait up to 45 seconds); remove the unit when using `--systemd` |
| `expose rm` | None | Close the endpoint, then delete local configuration and observation files |
| `expose logs` | `-f` | Print the last 200 lines of the connector log; `-f` follows the log |

Every subcommand accepts `--data DIR`.

Key behavior:

- cloudflared runs in a separate connector service (`gpuworks expose run`) and is not managed by the server: server restarts and upgrades do not affect it, and the quick address stays the same. By default, `up` starts the service as a background process, which does not automatically recover after a machine reboot. `--systemd` (requires root) installs it as `gpuworks-expose.service` with startup at boot. The service runs as the owner of the data directory and exits automatically after the endpoint is closed.
- The forwarding target is resolved in this order: `--origin` > the listen address recorded when the server starts (`<data>/expose/server-origin`) > the first console address in `server.yaml` in the installation directory.
- The data directory is resolved in this order: `--data` > `server.yaml` next to the binary > `server.yaml` of the default installation (`/opt/gpuworks/server/server.yaml`; `~/gpuworks/server/server.yaml` on macOS) > `./data`. The matching rule is printed.
- `up` waits for at most 3 minutes. A timeout or Ctrl-C only stops waiting and does not cancel the request (`down` stops it). If the configuration is unchanged and the endpoint still exists, the existing endpoint is reused without issuing a new quick address.
- `status --url-only` exits with a nonzero code unless the connector service is running, its revision matches, and the endpoint is ready. The reason is written to stderr.
- Only `cloudflare-quick` is currently available: its address is temporary and does not support SSE. `up`/`status`/`--url-only` display this notice. `--provider cloudflare-named` currently reports that it is not yet available.
- `rm` only deletes local files, retaining the connector binary cache and logs. It does not change anything on Cloudflare.
- `expose run` is the connector service itself, started by `up` or a systemd unit. The internal subcommand `expose connector-host` is the helper through which the service owns cloudflared. Neither command requires manual execution.

## gpuworks migrate

Run migrations and seed the built-in catalogs in the specified SQLite database (including system image catalog synchronization, as at server startup), then exit.

| Flag | Default | Description |
|---|---|---|
| `--data DIR` | `./data` | Data directory |
| `--db PATH` | `<data>/gpuworks.db` | SQLite path |

## gpuworks reset-password

Reset a user's password by modifying the database directly, without going through the server.

| Flag | Default | Description |
|---|---|---|
| `--password PASSWORD` | None: prompts twice in the terminal without echo | New password; required when stdin is not a terminal |
| `--user USER` | `admin` | Username |
| `--data DIR` | See below | Data directory |
| `--db PATH` | `<data>/gpuworks.db` | SQLite path |

- Without `--data`, the data directory is, in order: `data_dir` from the `server.yaml` next to the binary, `data_dir` from the default installation (`/opt/gpuworks/server/server.yaml`; `~/gpuworks/server/server.yaml` on macOS), then `./data`. The installation's database is therefore found regardless of the current working directory.
- The database file must already exist: if it does not, the command reports the path it used and where that path came from, then exits without creating an empty database.
- On success, if the `initial-credentials` beside the database records this user, the command tries to delete it, since its password no longer works; a failure only prints a warning and does not affect the reset.
- Run as the operator; `sudo` is not required. If database permissions are insufficient, the operation fails with the database's own permission error.
- Do not write passwords or keys to shell history or logs.

## gpuworks support-bundle

Collects diagnostics without the server, for when the server does not start or the console cannot be reached; the content matches the console's [Collect diagnostics](../how-to/troubleshooting.md#无法登录现场时调试采集与收集诊断信息), except that the nodes' part cannot be collected (the manifest marks them offline). Prints the archive's path when done. The archive is assembled in the directory of `--out`, not in the data directory, so with `--out` on another disk it still works when the server's disk is full.

| flag | Default | Description |
|---|---|---|
| `--data DIR` | As for `reset-password` | Data directory |
| `--db PATH` | `<data>/gpuworks.db` | SQLite path |
| `--log-file PATH` | `<data>/logs/gpuworks-server.log` | Server log file (with its rotated generations) |
| `--since DURATION` | `1h` | Collect this much recent time; ignored with `--from`/`--to` |
| `--from T`, `--to T` | None | Time range (RFC 3339); give both |
| `--deployments IDS` | None | Comma-separated deployment IDs whose records and instance logs to include |
| `--instances UUIDS` | None | Comma-separated instance UUIDs whose logs to include |
| `--out PATH` | `./gpuworks-diagnostics-<id>.tar.gz` | Archive path |

## gpuworks-agent configure

Write the node's startup configuration to `<state>/gpuworks-agent.conf` (permissions `0600`, replacing the entire file). The installation script calls this command after placing the binary. Every agent startup method (systemd units, nohup and `agent-restart.sh`, launchd jobs, and restarts after manual upgrades) executes only `gpuworks-agent run --state DIR` and reads all other configuration from this file.

The node token is read from the first line of standard input rather than passed as an argument, so it does not appear in `ps`, systemd units, or plists. For the file format and fields, see [Installation Layout](install-layout.md#agent-配置文件).

| Flag | Default | Description |
|---|---|---|
| `--state DIR` | None (required) | State directory; the file is written here |
| `--server URL` | Empty | Server base URL; must start with `http://` or `https://`. Required for `agent_dial`; neither required nor recorded for `server_dial` |
| `--connect-mode MODE` | `agent_dial` | `agent_dial` or `server_dial` |
| `--listen ADDR` | `0.0.0.0:18800` for `server_dial` | Listen address for `server_dial`: `host:port` or `:port`, with a port from 1–65535 |
| `--driver NAME` | Empty (automatic detection) | Force a runtime; accepts the same values as `run` |
| `--mount-root DIR` | Empty | Directory for bind-mount sources when running as a non-root user |
| `--udocker-bundle DIR` | Empty | Offline udocker bundle directory |

- Validate with the same rules as `run` before writing: a missing token, a missing server for `agent_dial`, an invalid address or listen port, or newlines or control characters in values cause rejection without writing the file.
- Write a temporary file in the same directory first, then rename it. An agent starting concurrently therefore reads either the old file or the new file, never a partial file.

## gpuworks-agent run

Connect to the server and provide node capabilities. There is only one standard invocation: `gpuworks-agent run --state DIR`. Configuration comes from `gpuworks-agent.conf` in that directory (see [`configure`](#gpuworks-agent-configure)).

Each configuration item is resolved in this order: **command-line flag > environment variable > configuration file > default**. A missing file is not an error: agents from earlier installations do not have this file and still run using command-line arguments or environment variables.

| Flag | Environment Variable | Description |
|---|---|---|
| `--state DIR` | `$GPUWORKS_STATE_DIR` | Directory for state, logs, and models, and the location from which the configuration file is read; see defaults below |
| `--server URL` | `$GPUWORKS_SERVER` | Server base URL; must start with `http://` or `https://`. Required for `agent_dial`; not required for `server_dial` |
| `--token TOKEN` | `$GPUWORKS_TOKEN` | Per-node connection token (obtained by creating a node pending connection in the UI); required |
| `--driver NAME` | `$GPUWORKS_RUNTIME_DRIVER`; automatic detection if empty | Force a runtime: `docker` / `udocker` / `nerdctl` / `host` / `none` |
| `--connect-mode MODE` | `$GPUWORKS_CONNECT_MODE`; defaults to `agent_dial` | `agent_dial` (outbound agent connection) or `server_dial` (server connects directly to the agent's `--listen` port) |
| `--listen ADDR` | `$GPUWORKS_SERVER_DIAL_LISTEN`; defaults to `0.0.0.0:18800` for `server_dial` | Listen address in `server_dial` mode |
| None | `$GPUWORKS_RUNTIME_MOUNT_ROOT`, `$GPUWORKS_UDOCKER_BUNDLE` | Can only come from environment variables or the configuration file's `MOUNT_ROOT` and `UDOCKER_BUNDLE`; the agent sets them for the runtime at startup |
| `--data-lane BOOL` | None | Defaults to `true`. Enable a dedicated high-throughput data channel; set to `false` behind a proxy that only supports a single connection |
| `--image-pull-max-connections N` | `$GPUWORKS_IMAGE_PULL_MAX_CONNECTIONS` | Upper limit of simultaneous connections a runveil (udocker) node opens to each registry host (to the proxy when going through one) when pulling images; `0` or unset means automatic (up to 16), `1` means a single connection, and the largest value is 32; a value out of range makes the agent refuse to start. For registries and proxies that allow only a few connections or rate-limit many; not an everyday setting. See [Limit Connections for Image Pulls](../how-to/configure-pull-settings.md#限制镜像拉取的连接数runveil-节点) |
| `--dcgm MODE` | `$GPUWORKS_DCGM`; defaults to `off` | Optional DCGM SM utilization probe: `off` / `auto` (enabled only when `dcgmi` is on PATH) |

- Default for `--state`: `$GPUWORKS_STATE_DIR`, otherwise `/opt/gpuworks` (if it exists); on macOS, then `~/gpuworks` (if it contains `gpuworks-agent.conf`) and `~/.gpuworks` (if it exists); otherwise `gpuworks` under the user's configuration directory, and finally `./gpuworks-state`.
- `--data-lane`, `--dcgm` and `--image-pull-max-connections` are not part of the configuration file and are not recorded by `configure`.
- `--driver udocker` selects runveil: `udocker` is the retained driver name (still used in node records and APIs). Its implementation uses native image handling and a launcher that directly invokes the embedded PRoot; the UI displays runveil.
- `--driver host` runs the engine as a host process for macOS (Apple silicon) nodes: Linux containers on macOS run in a virtual machine and cannot access the Metal GPU. The image is a darwin/arm64 environment bundle, which the agent starts directly after extraction.
- If no driver is specified, macOS selects host directly (even if Docker Desktop is available). Other systems detect runtimes in this order: docker (daemon available) > nerdctl > runveil. If none is available, deployments are disabled on the node. An unknown `--driver` value or a runtime unavailable on the current system (`host` on Linux or `udocker` on macOS) logs an error and leaves all runtimes disabled.
- Logs are written to both stderr and `<state>/agent.log` (rotated at 10 MiB, retaining the previous generation as `agent.log.1`). Records at warning level or above are sent to the server's log center. Only the `GPUWORKS_LOG_LEVEL` environment variable controls the log level, which filters both the file and stderr; repeated records are compressed, see [The server's and agent's own logs](../how-to/monitor-and-logs.md#server-与-agent-自身的日志).
- The merged configuration is validated with the same set of rules. A failure immediately reports an error and exits, indicating the configuration file path: a missing token; a missing server for `agent_dial`; a server URL without a scheme or containing credentials, a query string, or a fragment; or an invalid port in the listen address for `server_dial`. A `server_dial` agent only needs a token and listen address. It never connects to the server and neither requires nor uses a server address.

## gpuworks-agent smoke

Use the new binary to validate a connection handshake with the server, then exit without starting any node capabilities. Agent self-upgrade uses this command to validate the downloaded version before replacing the binary.

| Flag | Default | Description |
|---|---|---|
| `--state DIR` | Same as `run` | State directory from which to read the configuration file |
| `--server URL` | Same precedence as `run` (flag > `$GPUWORKS_SERVER` > configuration file) | Server base URL (only checked for a nonempty value; the scheme is not validated) |
| `--token TOKEN` | Same as above, using `$GPUWORKS_TOKEN` | Per-node token |

Exit codes: `0` handshake succeeded; `3` not completed within 10 seconds; `4` rejected by the server (invalid token, etc.); `1` other errors (including missing arguments).

## gpuworks-agent upgrade

Manually upgrade the agent: download the new version from the server, replace the binary in place after smoke validation succeeds, and re-execute it.

| Flag | Default | Description |
|---|---|---|
| `--state DIR` | Same as `run` | Directory from which to read the configuration file; also passed to the replacement `run` |
| `--server URL` / `--token TOKEN` | Same precedence as `run` | Required (from flags, environment variables, or the configuration file) |
| `--driver NAME`, `--connect-mode MODE`, `--listen ADDR` | Same as `run` | Passed to the replacement `run` only for older agents without a configuration file; otherwise these values are already in the file |

- Only Linux is supported. Download from `<server>/dl/gpuworks-agent-linux-<arch>` to `<bin>.new`. Only after `smoke` succeeds within 10 seconds is the old binary renamed to `<bin>.old` and replaced with the new version. A smoke failure leaves the old binary unchanged.
- A `server_dial` agent does not know the server address and cannot download from it: manual `upgrade` is rejected immediately, with a message that upgrades are pushed by the server through the "Upgrade Agent" action on the Nodes page.
- When configuration comes from a file, the replacement `run` carries only `--state <dir>`. Older agents without a configuration file carry `--server --token --connect-mode --state`, plus `--listen` for `server_dial` and `--driver` if a driver is specified. Neither case carries `--data-lane`, `--dcgm` or `--image-pull-max-connections`. The replacement process inherits the current environment variables, so you can use `GPUWORKS_DCGM` to retain the value of `--dcgm` and `GPUWORKS_IMAGE_PULL_MAX_CONNECTIONS` to retain the value of `--image-pull-max-connections`. There is no corresponding environment variable for `--data-lane`, which returns to its default of `true` after a manual upgrade.
- The entire process has a 10-minute budget; only one upgrade is allowed at a time. The replacement process must also register successfully within the confirmation deadline or it rolls back to `<bin>.old`. For agents that support pushed upgrades, the "Upgrade Agent" action on the Nodes page has the server push the binary over the connection, without using `/dl/` or `smoke`. See [Upgrades and Version Compatibility](../how-to/upgrade-and-compatibility.md).

## gpuworks-agent doctor

By default, print a node environment self-check. Use `--state DIR` to select the Agent directory:

- Host, CPU, memory, root partition capacity, and filesystem type (mount flags: network, noexec, read-only, container layer). "Diagnostics" in node details additionally includes paths such as the model, udocker, and agent directories, and reports checks for runtime initialization failures, `noexec` / read-only mounts, and data in container layers. See [Troubleshooting](../how-to/troubleshooting.md#运行时初始化失败与容器节点的文件系统).
- Availability and versions of `docker`, `python3`, and `dcgmi`.
- GPU: vendor, driver version, and each card's model/GPU memory/architecture/NUMA/health status.
- Runtime plugin detection results and whether device injection can be planned.
- NVIDIA nodes also include a manual diagnostics section: driver and Fabric Manager, CUDA runtime smoke test, inter-card peer access, and multi-GPU NCCL.

`--connectivity` selects lightweight connection checks without GPU, CUDA or NCCL self-checks:

```bash
gpuworks-agent doctor --connectivity --state /opt/gpuworks
gpuworks-agent doctor --connectivity --state /opt/gpuworks --json
```

- Substitute the actual state directory. Precedence matches `run`: explicit flags > this invocation's environment > `gpuworks-agent.conf`. `--server`, `--token` and `--connect-mode` can override it; prefer the file for tokens so credentials do not appear in process arguments.
- The total budget is at most 10 seconds, using the real WebSocket, yamux and identity handshake. DNS, connect, proxy CONNECT and TLS are reported only from actual transport callbacks. Proxy-side DNS, reused connections and other unobserved stages are not filled in as successful.
- Text and JSON identify `source=cli`, the environment of this invocation, not necessarily the running service. JSON reuses `DoctorReport` with `report` and primitive `connection.*` facts. Logs go to stderr, separately from stdout JSON.
- Exit `0` means the listed checks succeeded or a valid `server_dial` outbound check is not applicable; `3` is the total deadline, `4` an explicit server rejection, `1` another failure including invalid configuration, and `2` invalid arguments. Default environment doctor still requires reading the report, not judging hardware health from its exit code.
- `control=smoke_verified` establishes only this short handshake; `data_lane=unverified` means no data test. It starts no second formal Agent or replacement ctrl/data session and closes its own connection on completion or timeout. Smoke on an old Server can still clear an upgrade error; a new Server reserves that for formal sessions. Mixed versions are not guaranteed to cause zero writes.
- History reads only that state's `agent.log` and `agent.log.1`, at most 512 KiB total and 32 new safe events. `connection.last_failure.*` is the latest valid recorded failure with its original time, separate from this test. Optional `last_failure.server_url` and `last_failure.retry_at` retain that event's original target and planned retry time; missing values are not filled from this probe, and an old plan does not establish that the Agent is still waiting. Legacy, missing, rotated or unreadable evidence is unknown/unavailable; raw logs are not emitted. Repeated events are aggregated, so the summary does not enumerate every attempt.

A valid `server_dial` configuration requires a token but no Server URL. It does not dial the control plane from the node; check inbound listening and the reverse network. Online nodes can return runtime observations over existing ctrl from the page without SSH. Offline nodes require the local CLI through the node console.

## gpuworks-agent grant-device-access

Add a non-root runtime user to the groups that own accelerator device nodes so that runveil workloads do not need to run as root.

| Flag | Default | Description |
|---|---|---|
| `--user USER` | None (required) | Non-root runtime user |
| `--state DIR` | Same as `run` | Agent state directory (the runveil driver's root) |
| `--create-user` | `false` | First create the user with `useradd -m` if it does not exist (requires root) |

- Detect local accelerators, determine the device nodes to inject using the same rules as deployment, and identify the owning groups that the user lacks. If no devices require authorization or the user already has read/write permissions, only print an explanation and exit successfully.
- If groups must be added, run as root and execute `usermod -aG <group> <user>`. Restart the agent for the changes to take effect.
- Applies only to the runveil (`udocker`) driver. The node installation script automatically calls `grant-device-access --state <state> --user udocker --create-user` when running as non-root with available `sudo` and `--driver udocker`.

## gpuworks-agent bench

Load-test an OpenAI-compatible endpoint (the Benchmarks page uses the same built-in Go loadgen; see [Run Benchmarks and Interpret Reports](../how-to/benchmark.md)).

| Flag | Default | Description |
|---|---|---|
| `--url URL` | None (required) | Endpoint base URL |
| `--model MODEL` | None (required) | Model ID |
| `--api-key KEY` | `$OPENAI_API_KEY` | API key |
| `--concurrency N` | `1` | Concurrency (used when `--concurrency-list` is not provided) |
| `--concurrency-list LIST` | Empty | Concurrency grid, such as `1,2,4` |
| `--isl N` | `128` | Target input length (used when `--isl-list` is not provided) |
| `--isl-list LIST` | Empty | Input length grid, such as `128,512` |
| `--osl N` | `32` | Target output length (used when `--osl-list` is not provided) |
| `--osl-list LIST` | Empty | Output length grid, such as `32,128` |
| `--requests N` | `0` (automatic) | Requests per grid point in closed-loop mode; automatic is `concurrency × 2`, or 4/8/8/16 for agentic concurrency 1/2/4/8 |
| `--request-rate-list LIST` | Empty | Open-loop arrival rate grid (req/s), such as `1,2,4`; replaces the concurrency grid when provided |
| `--burstiness F` | `1` | Shape parameter of the gamma distribution for open-loop inter-arrival intervals; 1 is Poisson |
| `--max-in-flight N` | `0` | Maximum in-flight requests in open-loop mode; 0 means unlimited |
| `--point-duration S` | `60` | Arrival duration per open-loop point (seconds); request count = arrival rate × duration, with a minimum of 20 |
| `--warmup-requests N` | `1` | Warmup requests sent and discarded before the first point; 0 disables warmup |
| `--request-timeout S` | `600` | Seconds each request, warmup included, may take before it fails; 10–7200 |
| `--ignore-eos BOOL` | `true` | Include `ignore_eos` in random / agentic requests to generate up to the target output length |
| `--dataset KIND` | `random` | `random` / `multiturn` / `agentic` |
| `--preset NAME` | `agentic-80k-v1` | Agentic input preset: `agentic-{10k,20k,40k,80k,128k,256k,512k,1m}-v1` |
| `--turns N` | `13` | Number of agentic conversation turns |
| `--stream BOOL` | `true` | Use streaming responses |
| `--tokenizer REF` | Empty | Tokenizer path, URL, or `hf://` or `ms://` reference |

- Combine all entries in the concurrency, input length, and output length lists, and run every combination (the Cartesian product). When `--request-rate-list` is provided, its arrival rate list replaces the concurrency list.
  Each combination is a load point. After each point runs, print one summary line: success/failure counts, output and total token throughput, per-user output speed `1000 / TPOT p50`, TTFT p50/p99, TPOT p50, and E2E latency p50/p99.
  Concurrency and length list entries must be positive integers; arrival rates must be positive numbers.
- Execution shares the same engine as the Benchmarks page. For metric definitions, see [Run Benchmarks and Interpret Reports](../how-to/benchmark.md#指标口径).
- Providing `--tokenizer` selects `exact` mode; otherwise the mode is `estimate`. `agentic` requires a tokenizer and defaults to a concurrency grid of `1,2,4,8` when `--concurrency-list` is not provided. Input/output lengths are determined by the preset.

## gpuworks-agent runveil

Troubleshoot runveil nodes and import images by reading and writing the agent's state directory directly, without going through the agent or requiring Python.

Usage: `gpuworks-agent runveil [--state DIR] <command>`. `--state` must appear before the command and has the same default as `run` (including `GPUWORKS_STATE_DIR`).

| Command | Description |
|---|---|
| `ps [-a]` | List containers (NAME/IMAGE/STATUS/PID/CREATED); only running containers by default; `-a` (`--all`) lists all |
| `images` | List images (REPOSITORY/TAG/IMAGE ID/SIZE) |
| `load -i FILE REF` | Register a `docker save` or OCI archive (`.tar` or `.tar.gz`) as the image `REF`; `-i` may also be written `--input` and may come before or after `REF` |
| `exec NAME [CMD [ARG...]]` | Execute a command inside a container with the workload's environment variables, mounts, and GPUs; defaults to `/bin/sh -l`, with the exit code passed through |
| `logs [-f] [-n N] NAME` | Print container output; `-n` (`--tail`) selects the number of trailing lines, defaulting to all; `-f` (`--follow`) follows until the container stops |

- `NAME` can be a unique prefix of a container name. Multiple matches list the candidates and fail.
- Any user who can read the state directory can execute `ps`/`images`/`logs`, with the same results as root. `exec` must run as root or the agent's runtime user (the owner of the container spec file recorded by the agent).
- `REF` is required for `load`: give the full image reference the Images page shows (such as `gpuworks/gpuworks-runner:cuda13.0-sglang0.5.22-r1`). The platform decides whether a node has an image by this reference; the tag recorded in the archive may differ and is not registered separately. Digest references are not accepted. Run it as root or as the owner of `udocker/` in the state directory; new files are given to that owner so the runtime user can read and replace them. The agent need not be stopped: after its next image status report (about one minute) the Images page shows “Present.” On failure no image is registered and the layers already written are removed.
- SIZE in `images` is the compressed size actually stored on the node. Images registered by the old udocker CLI have no manifest, so ID and SIZE display `-`.
- If the state directory contains no runveil state, report an error prompting you to pass `--state` (if the agent's state directory is specified through `GPUWORKS_STATE_DIR` in the unit, set it here as well).
- Exit codes: `0` success; `1` runtime error; `2` usage error.

## gpuworks-agent install-service

macOS only. Register the currently running binary as the launchd job `ai.gpuworks.agent`. The state directory must already contain a valid configuration file written by [`configure`](#gpuworks-agent-configure). If the file is missing or invalid, report an error prompting you to run `configure` first.

| Flag | Default | Description |
|---|---|---|
| `--state DIR` | None (required) | State directory containing the configuration file; also the job's working directory, where `agent.stderr.log` is written |
| `--user USER` | None | User under which to run the LaunchDaemon; required when executing as root, and cannot be root |

- The job's `ProgramArguments` contain only `<binary> run --state <dir>`. The server, token, connection direction, listen address, and driver are not included in the plist; the agent reads them from the configuration file at startup.
- When executed as root, write `/Library/LaunchDaemons/ai.gpuworks.agent.plist` (system domain), remove the user's own LaunchAgent, and transfer ownership of files belonging to other accounts in the state and binary directories to that user. Otherwise, write `~/Library/LaunchAgents/ai.gpuworks.agent.plist` (`gui/<uid>` domain); report an error if the user has no login session.
- Set plist permissions to 0600. Then `bootout` the old job, wait for launchd to release it, and `bootstrap`. Exit with 0 only after `launchctl print` reports the same running PID for several consecutive seconds. Restore the original plist if launchd rejects the new job. If the agent fails to start, report an error with the last few lines of `agent.stderr.log`.

## gpuworks-agent uninstall

| Flag | Default | Description |
|---|---|---|
| `--state DIR` | On Linux, prefer the state directory from the systemd unit; without a unit, use the same default as `run`. On macOS, use the state directory in the launchd job | Agent state directory |

First use `--dry-run` (or `--dry-run --json`) to inspect installation/state ownership, services and processes, autostart, runtime workload candidates, GPU observations, and retained directories. A container name in a shared runtime does not prove that it belongs to this agent. Unavailable runtime evidence is marked unknown. Unsupported or failed GPU queries do not report an idle GPU, and an exited process-group leader does not prove that all workers exited.

| Option | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Preview checks and cleanup scope without stopping services or deleting files |
| `--json` | `false` | Output a structured checklist and result; execution requires `--yes` |
| `--yes` / `-y` | `false` | Skip interactive confirmation, not resource or ownership checks |
| `--keep-resources` | `false` | Explicitly retain workloads and remove only the agent; never bypass service/process ownership or cleanup safety checks |

By default, active workloads and critical unknowns must be addressed before confirmation and uninstallation. Critical resources are checked again after confirmation. Unknown GPU observations are shown explicitly and cannot establish that GPUs were released. Binaries, models, and logs remain; success means that the selected component was removed.

- On Linux, if the unit's state directory matches this directory, first run `disable --now gpuworks-agent.service`, then stop `run` processes with the same binary and explicit `--state` directory (including nohup), and wait for them to exit. Then delete `/etc/systemd/system/gpuworks-agent.service` and run `daemon-reload`. Finally, delete `<state>/identity.json`, the PID file, and `<state>/gpuworks-agent.conf` (which contains the token). If the unit belongs to another state directory (another agent on the same host), leave it untouched and stop only this directory's processes. Refuse to execute if the state directory cannot be read from the unit.
- On macOS, remove the LaunchDaemon and the executor's LaunchAgent (under `sudo`, the user who invoked sudo), and delete `identity.json` and `gpuworks-agent.conf` from their respective state directories. If a LaunchDaemon exists but root permissions are unavailable, report an error and exit with 1.
- Exit with a nonzero status if stopping or cleanup fails or times out. Retain binaries, models, logs, and the state directory.

## Node Installation Script install.sh

The bootstrap script served by the server at `/install.sh`. When you add a node, the UI generates an installation command in the form `curl … | sh -s -- --server … --token …`. For a comparison of connection methods, see [Connect, Upgrade, and Uninstall Nodes](../how-to/connect-nodes.md). The SSH wizard (Linux and Mac) and [offline bundles](../how-to/connect-nodes.md#离线包) also run this same script: the SSH wizard uploads it together with the agent binary and runs it with `GPUWORKS_AGENT_BINARY` pointing to the uploaded binary. There is no longer a separate SSH startup command.

| Parameter | Environment Variable | Default | Description |
|---|---|---|---|
| `--token TOKEN` | None | None (required) | Per-node token; the script contains no built-in cluster-level secrets |
| `--server URL` | None | None | Server base URL; the script contains no built-in address. Required only for `agent_dial`; neither required nor recorded for `server_dial`. If neither `GPUWORKS_AGENT_BINARY` nor `--server` is provided, no binary can be downloaded and an error is reported immediately |
| `--connect-mode MODE` | `GPUWORKS_CONNECT_MODE` | `agent_dial` | Same as `run` |
| `--listen ADDR` | `GPUWORKS_SERVER_DIAL_LISTEN` | `0.0.0.0:18800` for `server_dial` | Same as `run` |
| `--driver NAME` | `GPUWORKS_RUNTIME_DRIVER` | Empty (automatic detection) | Same as `run` |
| `--mount-root DIR` | `GPUWORKS_RUNTIME_MOUNT_ROOT` | Empty | Root directory for runtime bind-mount sources |
| `--udocker-bundle DIR` | `GPUWORKS_UDOCKER_BUNDLE` | Empty | Offline udocker bundle directory |
| `--state DIR` | `GPUWORKS_STATE_DIR` | Same as `--install-dir` | State directory, where the configuration file is written |
| `--install-dir DIR` | `GPUWORKS_INSTALL_DIR` | `/opt/gpuworks`; see below for macOS | Directory containing the agent binary |
| `--supervisor MODE` | None | `systemd` | What keeps the Linux agent running: `systemd` (register `gpuworks-agent.service` if systemd is available and the unit can be written; otherwise use nohup), `nohup` (never modify systemd units), or `keep` (retain the host's current method for this agent: systemd if this node's unit exists; systemd if neither a unit nor `agent-restart.sh` exists; otherwise nohup). Other values produce an error; ignored on macOS |
| `--no-systemd` | `GPUWORKS_NO_SYSTEMD=1` | Off | Equivalent to `--supervisor nohup`: do not register a systemd service; run in the background with nohup. Leave the host's existing `gpuworks-agent.service` and agents in other directories untouched |
| `--clean-existing` | None | Off | Use with nohup (`--no-systemd` or `--supervisor nohup`): when root/passwordless sudo is available, run `disable --now` on the existing `gpuworks-agent.service` and delete it. The UI does not generate this argument |
| `--no-sudo` | None | Off | Install with the caller's own permissions even if `sudo` is available. The SSH wizard passes this when `sudo` cannot take ownership of the selected installation directory and it falls back to a user-writable directory |

- Download from `<server>/dl/gpuworks-agent-linux-<amd64|arm64>` according to `uname -m`. Write to a temporary file first, then replace atomically, leaving the old binary usable if the download fails. If `GPUWORKS_AGENT_BINARY` points to an existing file, copy it instead (used by the SSH wizard and offline bundles; the node does not need access to the server). Unrecognized arguments are ignored.
- After placing the binary, the script uses [`gpuworks-agent configure`](#gpuworks-agent-configure) to record the server, token, connection direction, listen address, driver, mount-root, and udocker-bundle in `<state>/gpuworks-agent.conf` (the token is passed through standard input). All subsequent startup methods use `gpuworks-agent run --state <state>`.
- When running as non-root, elevate privileges only if passwordless `sudo -n` is available. With root permissions and a running systemd, write `gpuworks-agent.service` (`ExecStart=<binary> run --state <state>`, `Restart=always`, `KillMode=process`; reinstalling does not kill inference containers) and start it. Otherwise, run in the background with nohup and write the PID to `<state>/gpuworks-agent.pid`.
- When writing the systemd service, stop the agent started by a previous nohup installation and recorded in the PID file, then delete the PID file and `agent-restart.sh`.
- The nohup branch also writes `<state>/agent-restart.sh` (`0700`, rewritten on every installation). On hosts without systemd, no process supervises the agent; after a crash, use this script to restart it with `nohup <binary> run --state <state>`. Configuration is in the file, so the script contains no token and does not depend on the installation-time environment variables.
- When replacing an existing agent, the script first reads which server it belongs to. If this installation uses systemd and a unit already exists, read the configuration file in the unit's state directory (for older agents without a configuration file, read `--server` from the unit's command line). If unavailable, read the configuration file in this installation's state directory. If the server differs from this installation's `--server`, print a `WARNING` line to stderr and proceed with installation. The same comparison applies to `server_dial` installations, which have no `--server`.
- With non-root + passwordless `sudo` + `--driver udocker`, automatically run [`grant-device-access`](#gpuworks-agent-grant-device-access) after installation to authorize the `udocker` account.
- macOS: only Apple silicon and macOS 15 or later are accepted. Download `gpuworks-agent-darwin-arm64` (`GPUWORKS_AGENT_BINARY` works as above). After writing the configuration, use [`install-service`](#gpuworks-agent-install-service) to register the launchd job. If passwordless `sudo` is unavailable but a terminal is present, `sudo` prompts for a password in the terminal and installs a LaunchDaemon. If neither is available, install a LaunchAgent. If `--install-dir` is not specified, the directory is `~/gpuworks` (the home directory of the user performing the installation, or of `SUDO_USER` under `sudo`), with or without administrator rights. If an earlier installation left an agent (`gpuworks-agent.conf` or the `gpuworks-agent` binary) in `~/gpuworks`, `/opt/gpuworks`, or `~/.gpuworks`, the first directory found in that order is reused. A quoted `~` or `~/x` in `--install-dir` or `--state` expands to the installing user's home directory. The control plane on the same machine defaults to `~/gpuworks/server`, so the two do not overwrite each other.

## Other CLIs

These tools are not included in release packages. Run them from source: `go run ./cmd/<name> …`.

### gpuworks-dist

Generate and verify the release channel directory tree ([ADR 0040](../adr/0040-signed-multi-source-distribution.md)). This tool is called by `scripts/publish_dist.sh` (`make dist-publish`)
and the CI `release-publish` job. rclone handles uploads; this tool does not hold storage credentials.

| Subcommand | Description |
|---|---|
| `assemble -channel C -dist DIR -out DIR [-commit SHA] [-built-at TIME] [-release-id ID]` | Read `gpuworks-<id>-<os>-<arch>.tar.gz` from `DIR` (all packages must have the same release ID; do not read the separate `release.json` files from the two build jobs), and write a new directory tree containing these packages and the image catalog and inference template catalog packages embedded in the current checkout. Sign `<C>/index.json` with the ed25519 seed in the `GPUWORKS_DIST_SIGNING_KEY` environment variable (change the variable name with `-key-env`), and write `<C>/latest`, `<C>/manifest.json` (the same file as the image catalog), and `releases/<id>/SHA256SUMS`. Refuse to proceed if `-out` already exists |
| `release-id -dist DIR` | Print the release ID of the release packages in `DIR` (the same rules as `assemble` apply) |
| `adopt -dir DIR -release-id ID` | Verify `releases/<id>/` copied back from a source: each package must match `SHA256SUMS` and belong to `ID`. Print the build time recorded in the package's `INSTALL.txt`; exit with a nonzero status on a mismatch |
| `verify -channel C -source URL … [-release-id ID] [-index FILE] [-full]` | Read the index from each source and verify its signature, check the release ID, and send a HEAD request for each file to check its size. With `-full`, download the files and check their sha256 instead. With `-index`, use a local, unpublished signed index to verify files on each source (without reading the source's own index), for checks before switching channels. Exit with a nonzero status if any source fails |
| `newer -channel C -built-at TIME [-source URL …]` | If a source serves a version in the same channel with a build time later than `TIME`, print it and exit with status 0. Exit with status 1 if none exists, or 2 for other errors. The publish script uses this result to skip older pipelines that finish out of order |
| `prune [-keep N] [-protect ID …] < listing.json` | Read the output of `rclone lsjson -R --files-only <remote>/releases` and print version directories that can be deleted: version tag (`vYY.M.PATCH`) directories are never deleted; of the other (nightly) directories, the latest N (default 30) and protected versions are kept |

### gpuworks-image-manifest

Generate the system image catalog
manifest (schema 3) from the gpuworks-runner build matrix (`build/runner-images/matrix.yaml`). The same output serves as the server's embedded seed, `internal/server/imagecatalog/catalog-seed.json`, and is included in every release through the
[release channel](#gpuworks-dist). For the format and import rules, see [Image Catalog](image-families.md#manifest-格式).

| Flag | Default | Description |
|---|---|---|
| `-in PATH` | None (required) | Build matrix `matrix.yaml` |
| `-out PATH` | `-` (stdout) | Output manifest JSON |
| `-registry REPO` | `gpuworks/gpuworks-runner` | Canonical repository written to `image_ref` |
| `-date YYYY-MM-DD` | Current date (UTC) | Date portion of `catalog_version`; `catalog_version` = `<date>.<first-8-characters-of-content-hash>` |
| `-verify PATH` | None | Instead of generating a manifest, check that every reference in this manifest can be pulled in full; see below |

Each matrix entry produces one version, `<registry>:<id>-<revision>`. Its `registry_sources` are the same tag in the three registries:
quay.io, swr.cn-southwest-2.myhuaweicloud.com, and registry.zstack.ai. The engine, engine version, framework, and
framework version are parsed from the ID. Architectures come from the entry's `platforms` (or the matrix defaults if omitted). The tool exits with an error if an ID does not match the grammar,
if `ENGINE_VERSION`/`FRAMEWORK_VERSION` declared in the matrix do not match the ID, or if a platform combination is unsupported. Output is sorted by family name and then by
`image_ref` for reproducible diffs. Unit tests ensure that the embedded seed and the output generated from the matrix are byte-for-byte identical.

`-verify` performs a pre-release check: first parse the manifest using the server's rules, then, for each version's `image_ref` and every
`registry_sources` entry, anonymously fetch the tag and each platform manifest listed in the index (skipping `unknown/unknown` build attestations), and send a HEAD request
for each config and layer blob. Platform manifest digests in each registry must match those of the canonical reference (SWR rewrites an OCI index as a manifest
list, so a different index digest is normal; platform digests remain unchanged).

If anything cannot be fetched, does not match, or requires credentials, print `FAIL <ref>: <reason>` for each failure
and exit with status 1. This blocks two types of failures that would otherwise surface only when users pull images: a registry has deleted a platform manifest still referenced by a tag (SWR does this when another tag sharing that
platform manifest is deleted), or the catalog lists a revision that was never published or has been deleted.

Docker Hub counts manifest GET requests
against its anonymous pull quota, but does not count HEAD requests. It is therefore checked last, using only HEAD, with content retrieved by digest from other registries.

Two make targets:

| Target | Purpose |
|---|---|
| `make image-manifest` | Regenerate `internal/server/imagecatalog/catalog-seed.json`. Use `IMAGE_MATRIX=<path>` to select a different matrix and `IMAGE_MANIFEST_DATE=<YYYY-MM-DD>` to fix the date |
| `make image-manifest-verify` | Run `-verify` on the committed seed |

### template-probe

An L1/L2/L3 validation tool for the inference template catalog, with JSON output. For the complete flags of each subcommand, run `go run ./cmd/template-probe <subcommand> -h`:

| Subcommand | Purpose | Main Flags |
|---|---|---|
| `l1` | Read only model repository metadata, estimate weight size, and check it against the image capability snapshot | `--source` (`huggingface` / `modelscope`, default: `huggingface`), `--model`, `--revision`, `--endpoint`, `--token-env`, `--image-digest`, `--architectures`, `--quantizations`, `--max-context-length`, `--index-metadata-only`, `--list-weight-files-fallback`, `--timeout` (30s) |
| `l2-command` | Derive the engine startup command from the model's `config.json` | `--engine` (`vllm`/`sglang`), `--model`, `--config` (required; `-` reads stdin), `--argv-json` |
| `l3` | Deploy a template through the server and run acceptance cases, then clean up the deployment | `--server`, `--deployment-json`, `--case-json` (all three required), `--management-token-env`, `--api-key-env`, `--timeout` (45m), `--cleanup-timeout` (5m), `--poll-interval` (2s), `--keep-deployment` |
| `classify` | Classify engine failure logs into deterministic signatures | `--log` (`-` reads stdin), `--include-forward` |
| `alignment` | Compare whether full and truncated probes produce consistent results | `--full-status`, `--full-signature`, `--truncated-status`, `--truncated-signature` |
| `calibrate` | Generate calibration fixtures from hardware calibration records | `--input` (`-` reads stdin), `--output` |

Always pass tokens through environment variables whose names are specified by `--*-env`; tokens do not appear on the command line.

### gpuworks-catalog-model-config

Maintain each model's `model_config` in the inference catalog package (the subset of `config.json` required for GPU memory estimation). By default, only report differences.

| Flag | Default | Description |
|---|---|---|
| `--catalog PATH` | `internal/server/inferencecatalog/seed/catalog` | Catalog package root directory |
| `--fallbacks PATH` | `cmd/gpuworks-catalog-model-config/fallback-sources.json` | Fallback sources for each model (mirror repositories, subdirectories, or manually specified shapes) |
| `--write` | `false` | Write back to `model.yaml`; otherwise, only report |
| `--verify` | `false` | Cross-check the layer counts and attention types in existing blocks against published weight indexes |
| `--only MODEL_ID` | Empty | Limit processing to a single model |
| `--jobs N` | `8` | Number of repositories read in parallel |

- Gated repositories require `export HF_TOKEN=…`. If it is not set, the tool displays a message and treats these repositories as returning 401. The product itself fetches metadata without a token, so predefined models must include their own facts required for estimation.
