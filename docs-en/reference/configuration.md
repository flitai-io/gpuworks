# Configuration Reference

## Server Configuration File server.yaml

The server has a single configuration source: `server.yaml` in the installation directory (written by `gpuworks install`; the unit file and
nohup command contain only `gpuworks serve --config <dir>/server.yaml`). It resides in the installation directory rather than the data directory
so that restoring the database to another machine does not carry over the old machine's addresses.

**Normal installations do not configure the server through environment variables.** The operator-facing exceptions are
`GPUWORKS_ADMIN_PASSWORD`, which supplies the initial password when the admin account is first created, and `GPUWORKS_USAGE_REPORT=off`, which disables usage reporting. Older installations also have the compatibility exception described below.

The exception is
older installations that predate `server.yaml`: their units passed configuration through three Environment entries, `GPUWORKS_ADDR`, `GPUWORKS_DATA_DIR`, and
`GPUWORKS_TRUSTED_PROXIES`. New versions still read these three variables as a compatibility bridge when neither the command line nor `server.yaml` provides
the corresponding value, allowing the new binary installed by an older version's `upgrade` to start normally. Subsequently, the new binary's
`upgrade`, `install`, or `restart` transfers the settings from `install.json` into `server.yaml`, rewrites the unit,
and deletes `install.json`. The rewritten unit no longer carries these variables.

Command-line arguments can temporarily override corresponding file values for development or recovery. Overridden listen
addresses appear as read-only in the console, with the reason shown.

The service mode, service user, and version are not duplicated in the configuration file. They are determined, respectively, by whether a systemd unit points to this binary,
the unit's `User=`, and the installed binary's version output. Installations and upgrades are recorded in `<data>/install-history.jsonl`.

```yaml
listen:                    # Example addresses: management 192.0.2.10, node access 198.51.100.10, serving 203.0.113.10
  console:                 # Web console + management API
    - 192.0.2.10:8800
  agent:                   # Node access; defaults to console when omitted
    - 192.0.2.10:8800
    - 198.51.100.10:8800
  gateway:                 # Inference API /v1; defaults to console when omitted
    - 203.0.113.10:8800
data_dir: /opt/gpuworks/server/data
log:
  file: /opt/gpuworks/server/logs/gpuworks-server.log
  max_size: 10MB
  keep: 20
  level: info              # debug / info / warn / error
trusted_proxies: [127.0.0.1/32, "::1/128"]
metrics_retention: 72h
session_idle_timeout: 24h  # Management session idle timeout
session_max_lifetime: 168h # Absolute lifetime from sign-in
udocker_bundle_source: ""
ssh_proxy: ""              # socks5://host:port; proxy for SSH to nodes
update_channel: release    # release (stable) / nightly (rolling preview); follows the build when omitted
auto_check_updates: false  # Disable automatic update checks; enabled when omitted
usage_report: false        # No daily report or node counts/GPU models with automatic checks; ID and versions remain; opted in when omitted
```

| Key | Default | Description | Override Argument |
|---|---|---|---|
| `listen.console` | Detected on first startup | List of `IP:port` addresses for the web console and management API | `--addr` (replaces the entire listen section; all functions use this single address) |
| `listen.agent` | Same as console | Persistent agent connections, installation script, and agent downloads; installation commands point here | Same as above |
| `listen.gateway` | Same as console | OpenAI-compatible `/v1` | Same as above |
| `data_dir` | `./data` | Database, master.key, metrics and log stores, inference catalog packages, and more | `--data` (the database path can also use `--db`) |
| `log.file` / `max_size` / `keep` | `<data>/logs/gpuworks-server.log`, 10MB, 20 | File and rotation settings for the service's own logs (`max_size` uses KB/MB/GB in powers of 1024); empty entries use their defaults | `--log-file`, `--log-max-size`, `--log-keep` |
| `log.level` | info | Log level: `debug` / `info` / `warn` / `error`; other values use info | `--log-level` |
| `trusted_proxies` | Empty (install writes `127.0.0.1/32, ::1/128`) | Proxies allowed to supply `X-Forwarded-For` / `-Host` / `-Proto` / `-Port`; while exposure (`gpuworks expose`) is enabled, connections from the local host are also trusted and need not be listed ([ADR 0049](../adr/0049-expose-trusts-same-host-connector.md)) | `--trusted-proxies` |
| `tls_cert_file` / `tls_key_file` | Empty | Paths to the PEM certificate chain and private key for native HTTPS; both must be set. Non-maintenance listeners use TLS 1.2 or later and require explicit listen IPs; wildcard addresses are unsupported. The local `127.0.0.1` / `::1` maintenance endpoints retain HTTP. See [Production Deployment](../how-to/deploy-production.md#原生-https) | — |
| `outbound_restrict_private_hosts` | `false` | Strict mode for the management node's outbound HTTP access to private destinations. Disabled by default: ordinary private and local loopback destinations need no allowlist. When `true`, private targets require `outbound_allow_private_hosts` authorization. Changes apply to the next request after saving; special metadata, link-local, and reserved addresses remain denied | — |
| `outbound_allow_private_hosts` | Empty | Private-network exceptions in strict mode: exact hostnames, IPs, or CIDRs. Ordinary private and local loopback targets do not need this list when strict mode is disabled. Changes apply to the next request after saving, without restarting. Wildcard domains are prohibited; special addresses remain denied. Platform administrators can change these two fields from Settings or the management API; host administrators can also edit the file directly | — |
| `storage_probe_roots` | `/opt/gpuworks/models`, `/opt/gpuworks/cache`, `/mnt/gpuworks` | No longer restricts storage paths: creating or modifying storage and probing it can use paths outside these roots. At startup the server still checks existing storage against it and logs a warning for each path (including cache directories and per-node paths) outside it. Takes effect after restart | — |
| `storage_denied_paths` | `/etc`, `/proc`, `/sys`, `/dev`, `/boot`, `/bin`, `/sbin`, `/lib`, `/lib64`, `/usr`, `/run`, `/var/run`, `/var/lib/docker`, `/var/lib/containerd` | Directories that storage paths and cache directories cannot use, matched by path segment and including subdirectories. When set, it replaces the whole built-in list; `[]` denies no directory, and omitting it keeps the built-in list. Each entry must be an absolute path that is not `/` and has no `.` or `..` segments. The path form rules (absolute, no `.`/`..`, not `/`, no symbolic links on the node) are not affected. File-only; takes effect after restart. It applies only to the paths a request submits, so a saved path does not block edits when the list changes; see [Use storage](../how-to/storages.md) | — |
| `metrics_retention` | 72h | Used **only on first startup** as the initial retention period for raw metric samples and container logs (Go duration; invalid or nonpositive values use the default 72h; constrained to 24 hours–30 days). Afterward, Data retention in Settings is authoritative (`PUT /api/v1/data-retention`, [ADR 0055](../adr/0055-long-term-statistics.md)); a mismatch produces a startup warning. Cleanup runs hourly in batches (50,000 sample rows per batch and 5,000 log rows per batch on the cutoff day) | `--metrics-retention` |
| `session_idle_timeout` | 24h | Time until a management session expires after no real activity (Go duration; invalid or nonpositive values use the default) | — |
| `session_max_lifetime` | 168h (7 days) | Absolute lifetime from login, which activity does not extend (Go duration; invalid or nonpositive values use the default) | — |
| `udocker_bundle_source` | Empty | A local udocker offline bundle on the server (directory or archive); when an SSH installation request includes `udocker_bundle` (the extraction directory on the node), the server uploads and extracts it there, and the node service's `GPUWORKS_UDOCKER_BUNDLE` points to that directory | — |
| `ssh_proxy` | Empty | SOCKS5 proxy for SSH to nodes, including agent uploads | — |
| `update_channel` | Empty (follows the build: master builds use `nightly`, `v*` tag builds use `release`) | Release channel used for update checks, official catalog updates, and upgrades: `release` (stable releases) or `nightly` (rolling previews). Switching channels under "Version and updates" in Settings writes this key and takes effect immediately; `gpuworks upgrade` also reads it when `--channel` is omitted. Versions predating release channels do not recognize this key and refuse to start: delete this line before downgrading to those versions (a downgrade with `gpuworks upgrade --archive` automatically rolls back if the old version cannot start) | `gpuworks upgrade --channel` |
| `auto_check_updates` | Empty (enabled) | Whether to check for new versions automatically: when enabled, opening the console triggers a check (and fetches publisher [notices](usage-report.md#发布方通知)); when `false`, the server does not check automatically and reads the version index and notices only when you click "Check for updates" in Settings. Automatic checks always include the installation identifier, server version, and agent version distribution to count installations and versions. Node counts and GPU models are included only when participating in the improvement program (`usage_report`); see "What update checks report" below. The toggle under "Version and updates" in Settings writes this key and takes effect immediately; enabling it removes the key. The key is written only when disabled; earlier versions do not recognize it and refuse to start, so delete this line before downgrading | — |
| `usage_report` | Empty (participating) | Whether to join the [User Experience Improvement Program](usage-report.md): sends one anonymous daily report to the publisher and includes node counts and GPU models with automatic update checks (see "What update checks report" below). When `false`, daily reports stop and update checks omit node counts and GPU models. Automatic checks still include the installation identifier and versions; to stop sending this information entirely, also disable `auto_check_updates`. The "User experience improvement program" toggle in Settings writes this key and takes effect immediately; enabling it removes the key. **For private deployments, write `usage_report: false` before the server's first startup** (a report is sent immediately on first startup); the environment variable `GPUWORKS_USAGE_REPORT=off` also forces it off. The key is written only when disabled; earlier versions do not recognize it and refuse to start, so delete this line before downgrading | — |

### Management Sessions

The management web interface uses server-persisted sessions and HttpOnly cookies. Both the idle timeout and absolute lifetime determine whether a session is valid:

- For real interactions, the frontend reports activity at most once per minute; application write requests also keep the session alive. Read-only polling and SSE heartbeats do not extend the idle timeout.
- After the browser sleeps, you can continue using the session directly as long as neither the idle timeout nor the absolute lifetime has expired.
- Changes to the two settings above take effect after restart; the new maximum lifetime is calculated from a new login.
- Changing or resetting a password revokes all login sessions for that user; logging out revokes only the submitted login credential. Platform API tokens and inference API keys follow their own lifecycles.

When a reverse proxy terminates HTTPS, configure its actual address in `trusted_proxies` and have it forward Host and Proto correctly. If the reverse proxy listens on a non-default port, also forward Host with the port (use `$http_host` in Nginx) or `X-Forwarded-Port`; see [Production Deployment](../how-to/deploy-production.md#反代配置要点控制台). Management write requests from the browser check Origin; trusted HTTPS forwarding headers also determine whether to set Secure cookies, while untrusted forwarding headers are ignored.

Login is rate-limited separately by client address and account: after 5 failures are recorded, it returns 429 `LOGIN_THROTTLED`, with `Retry-After` indicating the number of seconds before the next attempt is allowed. Subsequent failures use exponential backoff from 1 second to 15 minutes; failure records expire after 30 minutes of inactivity. A successful login clears only that account's records, not failures for other accounts sharing the client address. Unknown accounts receive the same treatment, and forged untrusted forwarding headers cannot change the source address. Both the number of records and concurrent password checks are capped. State is kept in the current server process; endpoints with multiple replicas should also configure shared rate limiting at the reverse proxy.

### Peer Relay Resource Limits

Model and image relays share the current server process budget. Configure it in `server.yaml` and restart to apply:

```yaml
peer_relay:
  rate_limit_kib_s: 65536
  burst_kib: 64
  no_progress_timeout: 2m
  confirmation_timeout: 5m
```

Rate and burst values are integer KiB (1 KiB = 1024 bytes). Omitted values or `0` select safe defaults, rather than unlimited bandwidth.
The defaults are 64 MiB/s aggregate, a 64 KiB burst, two concurrent relays per process, and one relay per participating node.
A lower task limit still applies; direct transfers retain their task limit. The server charges actual writes of the inner TLS stream,
sharing the budget across both directions. Receiving and forwarding the same bytes do not count as two copies of model progress.

Timeouts use Go durations such as `30s` or `2m`; omitted, empty, or `0` values use defaults.
The no-progress clock excludes time spent waiting for the relay's own bandwidth budget. The confirmation deadline starts after
the server has drained source output and observed EOF, while waiting for target validation and its ACK.
A source “sending finished” report alone does not start this deadline.
Negative values, fractional capacities, overflow, invalid durations, unknown keys, and duplicate keys reject startup.

When the relay is busy, tasks queue in arrival order for up to one minute at a time; a task not yet admitted retries with the selected source and keeps its place. Model and image transfer details show the observed route, stage,
encrypted bytes, and forwarding rate. Historical events enter the log center; aggregate series appear in the existing metric catalog.
Budgets and runtime counters belong to this server process; multiple management servers do not share a global bandwidth pool.

### Listen Rules

- Addresses must be `IP:port` literals (hostnames are not accepted); unknown keys in `server.yaml` cause the server to refuse to start, preventing misspelled keys from being silently ignored.
- Only the listed addresses are bound; the server does not listen on 0.0.0.0 and then filter in application code. To listen on all addresses, explicitly specify `0.0.0.0:8800` or `[::]:8800`.
  A wildcard address cannot coexist with a specific address on the same port (the kernel refuses both bindings); `127.0.0.1` and `::1` are exceptions and are absorbed by the wildcard address.
- `127.0.0.1` is always present and serves all functions on every port listed in the configuration: installation, upgrades, health checks, and agents on the same host connect through it.
  Loopback addresses other than `127.0.0.1` (such as `127.0.0.2`) are treated as ordinary addresses.
- Identical `IP:port` entries merge into a single listener that routes by path; functions not served on an address return 404:
  - Console: Web UI, `/api/v1/*` (excluding `/api/v1/agent/connect`), and playground;
  - Node access: `/api/v1/agent/connect`, `/install.sh`, and `/dl/*`;
  - Inference API: `/v1` and `/v1/*`;
  - All listeners respond to `/healthz`.
- If `listen` is empty on first startup, the server detects the host address from the default route (without a default route, it selects the first IPv4 address on an up, non-loopback, non-virtual interface that is not link-local)
  and writes it back to the file as `<IP>:8800`. If it still cannot find an address, startup fails and prompts you to specify one with `--addr` during installation.
- If binding any listed address fails, the server does not start (it does not start with only some functions available).

### Change Listen Addresses in the Console

The "Listen addresses" card in Settings shows only the console listen address by default (if this is the only address configured, changing it changes all functions). Node access and the inference API
are under "Advanced". Saving validates that addresses exist on the local host (except wildcard and loopback addresses) and that the address currently used by the browser
remains among the console listen addresses (to prevent locking yourself out). Saving rewrites only the `listen` key, preserving
other keys and comments in the file; a restart is then required to apply the changes:

- systemd installations: "Restart now" on the card makes the server exit and lets `Restart=always` bring it back; sudo is not required;
- nohup installations: run `gpuworks restart --dir <install-dir>` on the server.

If an incorrect address prevents console access, edit `server.yaml` directly and run `gpuworks restart`; local `127.0.0.1` always remains available.

"Listen addresses" specifies only which addresses the server binds on the local host. Nodes, users, and clients choose which address they actually
connect to: a domain, reverse proxy, or NAT can forward to a listen address, and the server neither tracks nor records that choice. Changing the node access listen address affects
installed agents: they keep dialing the address in their installation command. If that address no longer forwards to the new listen address, they go offline until forwarding
is fixed or they are reinstalled. See the topology explanation in [Configure Node Networks](../how-to/configure-networks.md).

### Command-Line-Only Tuning Arguments

The following arguments have no configuration file keys. Their defaults are the production values; adjust them through the command line only for testing or troubleshooting: `--proxy-max-body-bytes`
(default 256 MiB; the `/v1` request body limit; request bodies larger than 1 MiB are temporarily stored in `TMPDIR`; see
[Production Deployment](../how-to/deploy-production.md#网关容量与主机调优)), `--recovery-enabled` (true),
`--recovery-grace-period` (2m), `--recovery-initial-interval` (30s), `--recovery-backoff-multiplier` (2),
`--recovery-max-interval` (10m), `--recovery-restart-wait` (1m), `--stale-check-interval` (30s),
`--stale-after` (90s), and `--server-dial-reconnect-interval` (10s).

### Initial Administrator and Keys

- Administrator password: supplied through `gpuworks install --password`, the `GPUWORKS_ADMIN_PASSWORD` environment variable, or interactive input, written to
  `<data>/initial-credentials` (permissions below), and read when the `admin` account is created on first startup. If
  `GPUWORKS_ADMIN_PASSWORD` is set on first startup, it takes precedence over this file (suitable for deployments such as containers that do not run install); if neither exists, a random password is generated and written to
  the same file. It is read only when creating the account; subsequent changes do not affect existing accounts. Unit files never carry passwords.
- Master key: `<data>/master.key` (generated on first startup; permissions below), used to encrypt cloud provider credentials and other secrets; explicitly back it up in production.

New or changed administrator passwords must contain 8–128 Unicode characters and at most 512 bytes, and must not be the username. Common guessable values, whole-value repetitions and sequences are rejected; character-class mixtures are not required and passphrases are supported. Without an explicit password, a 24-byte random value is encoded as hexadecimal. Existing accounts can still authenticate with their current passwords.

Sensitive files default to owner-only access (`0600`). A Linux systemd installation explicitly bound to this binary and a non-root service account may use its trusted service group (`0660`); group membership grants access to platform credentials. macOS and standalone servers do not implicitly trust ordinary primary groups. Startup, install and upgrade harden existing credentials, databases and sidecars, and recognized backups; temporary database snapshots are owner-only. Failure to protect a file stops the operation: repair the reported owner/service-group permissions and retry. Generated passwords are not written to service logs.

- Model download mirrors: configure site rules under Settings → Pull Settings; there is no longer a startup argument (`--model-endpoint` remains only for in-process test fixtures: one site rule per site, pointing to that endpoint, without origin fallback, for the management node).

## Pull Proxies and Outbound Access

Configure proxies for model downloads and image pulls under Settings → Pull Settings → Proxies. The "Network" setting and node network overrides no longer exist (`network`, `network:<nodeID>`, `/settings/network`, `/nodes/{uuid}/network`; the last returns 410). Proxies are ordered entries; see [Configure Image and Model Pulls](../how-to/configure-pull-settings.md#代理) for fields, selection behavior, and `no_proxy` syntax:

| Field | Description |
|---|---|
| http_proxy / https_proxy | Proxy address |
| no_proxy | Go `NO_PROXY` semantics: `*`, IP/CIDR, domains including subdomains, `.` or `*.` prefixes matching only subdomains, and optional ports; loopback and localhost never use a proxy |
| Username / Password | Passwords are stored encrypted and are not echoed back |
| Applicable nodes | Node label selector |
| Used for management node outbound access | The management node uses this entry |

A node uses the first entry whose applicability conditions it meets; entries are not combined. If none matches, it uses the agent's own environment variables. Model download addresses (mirrors, internal proxies, and disabling origin fallback) use [source rules](../how-to/configure-pull-settings.md#站点规则模型). [Pull Settings](../how-to/configure-pull-settings.md) determines where images are pulled from; deployments do not select a source.

### Outbound HTTP Proxy Policy

Outbound URL hostnames accept only ASCII; convert internationalized domain names to Punycode first (for example, write `bücher.example` as `xn--bcher-kva.example`). Use the same form for domains in `outbound_allow_private_hosts`. Raw non-ASCII hostnames are rejected before DNS lookup; URL paths and query parameters can still contain Unicode text such as Chinese.

By default, user-configurable HTTP sources on the management node do not restrict ordinary private or local loopback destinations. Private model mirrors, registries, AI endpoints, and OTLP collectors require no additional allowlist configuration. All DNS answers are still checked before connection, and the connection is bound to a validated IP; every redirect step is validated again, and special metadata, link-local, and reserved addresses remain denied. URL credentials, IPv4 mapping, DNS rebinding, and secondary proxy resolution cannot bypass these target checks.

Platform administrators can view and change the policy under **Settings → Security & access → Outbound access policy**. **Restrict access to private hosts** is off by default. When enabled, enter one exact hostname, IP address, or CIDR per line under **Allowed private hosts**. Turning the switch off preserves the entered list. **Save** writes both fields to the active `server.yaml` and applies them to the next request or redirect without restarting.


With strict mode enabled, HTTP model directories, community repository browsing, manually entered image archive sources, AI services, collectors, credential login tests, model site rules used by the management node, and external benchmarks run from the server offer **Automatically add this host to the allowlist** beside the source address. Create and edit use the same authorization action. Selecting it alone does not save anything. On Save, Parse, Test, or the corresponding action, the platform checks the target again, appends only the selected eligible host to the existing list, and continues the original action. Unselected hosts do not change the list; changing the hostname requires selecting it again.

This permission applies to every management-node feature using that host, independently of whether the resource itself is saved successfully. Other entries are preserved; a concurrent policy edit is rejected rather than overwritten, so review the current policy before trying again. Users without administrator permission, read-only configurations, unresolved hosts, and permanently prohibited addresses cannot authorize from a form. A read-only check does not connect to the source or prove credentials, directory readability, or node download access. Direct node access and browser links do not use this management-node allowlist; node connectivity tests for source rules do not check management-node permission either.

Host administrators can also edit the file directly: set `outbound_restrict_private_hosts` to `true` and authorize necessary hosts or network ranges in `outbound_allow_private_hosts`. For example, this configuration enables strict mode:

```yaml
outbound_restrict_private_hosts: true
outbound_allow_private_hosts:
  - models.example.internal
  - 10.20.30.0/24
```

Changes to `outbound_restrict_private_hosts` and additions or revocations in `outbound_allow_private_hosts` apply to the next management-node HTTP request or redirect after the configuration is saved, without restarting the server; response bodies already being read continue transferring. Model metadata, image archives and relaying, community catalogs, release channels, AI, and telemetry share this runtime policy. Only these two outbound fields are reloaded; this does not apply startup settings such as listener addresses from the same file.

Runtime configuration reads are limited to 1 MiB. Omitting `outbound_restrict_private_hosts` from a valid configuration uses its default value, `false`. This differs from an oversized, missing, unreadable, or invalid YAML configuration file, which temporarily denies ordinary private and local loopback destinations while public access retains the existing security policy. After the file is repaired, the next request uses its valid policy again. Platform administrators can manage these fields through that Settings page or the administrator-only `GET` / `PUT /api/v1/server/outbound-policy` endpoints; host administrators can still edit the file directly. Saving preserves other configuration and comments in the file, without creating a separate database policy. If another administrator or a file edit has changed the configuration, a stale page cannot save over it; reload, review the current policy, and save again. The page is read-only when the server started without `server.yaml`; follow the displayed error if the file cannot be read or written. Special metadata, link-local, and reserved addresses remain prohibited even when strict mode is disabled or an allowlist entry matches.

HTTP/HTTPS proxies must support CONNECT to the destination port (including 80 for HTTP sources) to preserve the original Host / TLS SNI and pin the destination IP. If unsupported, access fails without falling back to an unsafe resolution path. This rule does not replace the dedicated management connection policies for cloud platforms, SSH, and registered agents.

The management node (server) uses the first proxy with "Used for management node outbound access" selected for its own outbound access: community image tag queries, model metadata and directory previews, model icon retrieval, release channel checks and downloads, catalog URL registration, image archive previews, size queries and relaying, and credential test logins. Agent downloads use the entry matched for that node.
After configuration changes, new requests use the new proxy; transfers already in progress retain their original route. Successful community tag results continue to use their cache lifetime.

A successful save means the proxy configuration has been persisted; configuration is delivered to nodes in the background. Offline nodes read the latest configuration after reconnecting.
Writing one configuration to an individual node's control stream waits at most 30 seconds; a write failure closes only that node's session, and reconnecting restores the configuration.
This limit excludes time spent waiting for the database or a concurrency slot for building global state.

- HTTP and HTTPS use their corresponding proxies; if only one is configured, both request types use it.
- When a proxy is selected, it is used without inheriting the process's `NO_PROXY` / `ALL_PROXY`; the entry's `no_proxy` applies. The agent automatically adds loopback and the server address to `no_proxy`. If the proxy is unavailable, an error is returned without automatically connecting directly.
- If no entry is selected for the management node or no entry matches a node, standard environment proxy behavior is retained, including `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`.
- An invalid configured proxy address causes requests to fail without a silent fallback; error messages do not include proxy credentials.

On nerdctl and udocker nodes, each image pull uses the proxy entry matched for that node, taking effect immediately (command-line pulls also honor `no_proxy`). Docker images are pulled by the daemon, and CLI environment variables cannot override
the daemon's proxy: the agent writes the node proxy to `/etc/systemd/system/docker.service.d/zz-gpuworks-proxy.conf` and runs `daemon-reload`,
without restarting dockerd. The "Pull proxy" card in node details shows whether it has taken effect and provides a "Restart dockerd" button (requires root or passwordless sudo).
See [Configure Image and Model Pulls](../how-to/configure-pull-settings.md#dockerd).

Proxies and source rules are separate settings. Cloud provider APIs, the AI assistant, the SSH proxy, and agent management connections retain their existing configurations
and do not switch network routes when the pull proxy changes.

The old `model_endpoint` and `default_model_source` settings have been removed; `PUT /settings/default_model_source`
returns 410. When upgrading from older versions, global URL rewrites and proxies are converted into site rules and proxy entries according to the [upgrade instructions](../how-to/configure-pull-settings.md#从旧版本升级).

## Image Catalog and Image Pulls

**Online updates to the system image catalog and inference template catalog** come from the release channel ([ADR 0040](../adr/0040-signed-multi-source-distribution.md)). Release sources are constants in the code
(Alibaba Cloud OSS `https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com`, Huawei Cloud OBS `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`, and
Cloudflare R2 `https://dl.gpuworks.ai`). The channel is determined by `update_channel` in `server.yaml` (see the table above; by default it follows the build).

The server initiates checks, rather than the browser: when you open the login page, the browser calls `POST /api/v1/updates/prefetch` (no login required; no result returned) to start a background check on the server.
After login, the page header reads `GET /api/v1/updates`; the server checks external sources at most once per hour for the same channel
(failed checks can be retried after 5 minutes). "Check for updates" in Settings checks again immediately; neither server startup nor a background timer accesses the network.

The server requests signed indexes from all three sources concurrently
(with 10 seconds to connect and 15 seconds to wait for response headers per source) and uses the first one that passes signature verification. Files are verified against the size and sha256 in the index; a failure or 30 seconds without new data triggers a switch to another source.

Requests prefer the management node's outbound proxy (the first proxy selected in Pull Settings); if none is configured, they inherit the `HTTPS_PROXY` / `HTTP_PROXY` environment variables. In environments that cannot reach any source, use "Upload manifest" and "Upload package" instead; see
[Image Catalog](image-families.md#更新目录).

**What update checks report.** Automatic checks (`auto_check_updates` enabled) include a set of URL query parameters when requesting signed indexes from the three release sources. The publisher uses object storage access logs to count installations and their version distribution. The following are the only parameters. Every automatic check includes `id`, `v`, and `agent`; `nodes`, `online`, and `gpu` are included only when participating in the User Experience Improvement Program (`usage_report` enabled):

| Parameter | Meaning |
|---|---|
| `id` | Installation identifier: the first 8 bytes, in hexadecimal, of HMAC-SHA256 over a fixed string using `master.key` as the key. This is one-way and cannot reveal master.key; it is used to count installations and count the same installation's hourly checks only once |
| `v` | Server version |
| `nodes` / `online` | Total nodes / currently online nodes |
| `agent` | Repeatable, `<agent-version>:<node-count>`; nodes that have never connected are recorded as `unknown` |
| `gpu` | Repeatable, `<GPU-model>:<card-count>`; counts only cards owned by the nodes themselves |

`agent` and `gpu` each allow at most 16 entries; excess entries are merged into `other`, and names are limited to 64 bytes. No hostnames, IPs, usernames, deployment information, or model information is included. Parameters are sent only to the three built-in release sources: command-line `--source` or self-hosted sites receive ordinary requests without parameters. Parameters accompany checks, so their frequency is the check frequency (at most once per hour for the same channel). With `usage_report` disabled, only `id`, `v`, and `agent` remain. With `auto_check_updates` disabled, no parameters are sent; manual "Check for updates" also never includes parameters. See [User Experience Improvement Program](usage-report.md) for the program's separate daily reports.

**Image pull addresses** are no longer a single setting. They are determined by [Pull Settings](../how-to/configure-pull-settings.md): under Settings → Pull Settings,
source rules (`/api/v1/pull/source-rules`, where image rules and model site rules share a table), sync groups (`/api/v1/pull/sync-groups`), proxies (`/api/v1/pull/proxies`), and credentials (`/api/v1/pull/credentials`; the old `/api/v1/pull/registry-credentials*` returns 410).
`PUT /api/v1/settings/image_registry_rewrites`, `image_registry_mirrors`, and image acceleration preset settings return 410 `SETTING_REMOVED`.
System images no longer include built-in third-party Docker Hub accelerators; official images have a copy on Docker Hub, quay.io, and Huawei Cloud SWR, ordered by reachability on each node.
See [Troubleshooting](../how-to/troubleshooting.md#镜像拉取失败) for candidate order and failure handling. Source rules apply to all images, including sources declared by inference templates for their own images (`image_sources`).

**`image_community_catalog`** (`PUT /api/v1/settings/image_community_catalog`) overrides or appends repository cards under "Community" on the Images page,
merging with built-in cards by `repo` (replacing matching names and appending new ones):

```json
{"repos":[
  {"repo":"registry.example.com/team/vllm","registry":"oci","engine":"vllm","framework":"cuda",
   "siblings":["registry.example.com/team/vllm-dev"]}
]}
```

| Field | Description |
|---|---|
| `repo` | Repository path; required and unique |
| `registry` | `docker-hub`, `quay`, or `oci`; if omitted, inferred from the host (no host means Docker Hub, `quay.io/` means quay, and others mean OCI) |
| `upstream_api` | Optional; overrides the address of the first page of the tag list; must use `https://` |
| `engine` | Required; the engine prefilled when adding an image |
| `framework` | The framework prefilled when adding an image |
| `siblings` | Other repositories listed alongside this one on the same card; each lists its own tags and inherits the card's engine and framework |

`schema_version` can be omitted (stored as `3`) or set to `3`; older version `2` documents without `registry`/`siblings` are still accepted. Other values return `COMMUNITY_CATALOG_SCHEMA_INVALID`.

## Deployment Policy

The `deployment` setting (`PUT /api/v1/settings/deployment`, administrators only) defines cluster-wide deployment limits and is stored as a JSON object.
The request body is merged into the current value, changing only the keys it contains; the merged result is validated as a whole. Out-of-range values return 400 without being saved.

| Key | Factory Default | Range | Meaning |
|---|---|---|---|
| `pending_timeout_sec` | 600 | 5–86400 | How long to wait before marking a deployment as failed when it still cannot be placed after its weights are ready (not a queue timeout) |
| `start_timeout_sec` | 1800 | 60–86400 | Floor for the automatic startup health budget; known engine deadlines can extend it up to 86400 seconds. A timeout marks this startup attempt as failed and recreates the instance according to retry rules. Edit it in the "Deployment" card in Settings; an individual deployment can override it with `start_timeout_sec` ("Start timeout" under "Runtime environment" in the deployment form) |
| `event_retention_days` | 180 | 1–3650 | Number of days to retain the event timeline |
| `event_retention_bytes` | 50 GiB | 64 MiB–1 TiB | Event timeline retention in bytes; whichever limit is reached first, bytes or days, applies |

If values saved by older versions lack new keys, those keys use factory defaults when read. If the stored value cannot be parsed or is out of range, the entire value falls back to factory defaults.

## Environment Variables

Environment variables serve only the agent and installation scripts; the server does not use them as a configuration source (except for `GPUWORKS_ADMIN_PASSWORD`, the `GPUWORKS_USAGE_REPORT=off` reporting override, and the older-installation bridge variables described above). Variables fall into five categories: variables read by the server, agent variables for operators, general variables read by other tools, test-only variables, and internal implementation details.

### Variables Read by the Server

| Variable | Default | Purpose | Source or Related Command |
|---|---|---|---|
| `GPUWORKS_ADMIN_PASSWORD` | None | Initial password when the admin account is first created; `gpuworks install` also uses it in the order `--password` > this variable > interactive input > random. Read only when creating the account | `gpuworks serve`, `gpuworks install` |
| `GPUWORKS_ADDR`, `GPUWORKS_DATA_DIR`, `GPUWORKS_TRUSTED_PROXIES` | None | Retained only for old units predating `server.yaml`: take effect only when neither the command line nor `server.yaml` provides the corresponding value; no longer used after `install` takes over | `gpuworks serve` |

### Agent Environment Variables

The following variables are read by the agent binary or the node installation script distributed with it. Variables marked "Boolean" treat `1`, `true`, `yes`, and `on` (case-insensitive) as enabled and all other values as disabled.

The agent resolves connection settings in this order: **command-line flags > environment variables > `<state>/gpuworks-agent.conf` > defaults**.
The configuration file is written by `gpuworks-agent configure`; see [Installation Layout](install-layout.md#agent-配置文件). The installation script writes settings to this file,
so the corresponding environment variables are usually no longer set on the node. The variables in the table below for the server, token, connection direction, listen address, driver, mount-root,
and udocker-bundle are only needed to temporarily override values in the file.

| Variable | Default | Purpose | Used by or related command |
|---|---|---|---|
| `GPUWORKS_SERVER` | None | Server base URL, used as the default for `--server`; overrides `SERVER` in the configuration file. Agents using `server_dial` do not use it | `agent run`, `smoke`, `upgrade` |
| `GPUWORKS_TOKEN` | None | Per-node token, used as the default for `--token`; overrides `TOKEN` in the configuration file | Same as above |
| `GPUWORKS_STATE_DIR` | `/opt/gpuworks` (if the directory exists), then on macOS `~/gpuworks` (if it contains `gpuworks-agent.conf`) and `~/.gpuworks` (if the directory exists), otherwise `<user-config-dir>/gpuworks`, then `./gpuworks-state` as the final fallback | Directory for state, logs, and models, used as the default for `--state` | `agent run`, `upgrade`, `uninstall`, `runveil`, `grant-device-access`; `install.sh` |
| `GPUWORKS_RUNTIME_DRIVER` | Empty (automatic detection: docker > nerdctl > runveil) | Force a runtime: `docker`, `udocker`, `nerdctl`, or `none`; `udocker` is runveil (the UI display name), and the name is retained for compatibility with existing nodes; an unknown value leaves the node with no runtime enabled | `agent run`, `upgrade`; `install.sh` |
| `GPUWORKS_CONNECT_MODE` | `agent_dial` | `agent_dial` (the agent connects outbound) or `server_dial` (the server connects directly to the agent's `--listen` port) | `agent run`, `upgrade`; `install.sh` |
| `GPUWORKS_SERVER_DIAL_LISTEN` | `0.0.0.0:18800` | Agent listen address in `server_dial` mode (`host:port` or `:port`); only relevant in `server_dial` mode. Existing nodes retain their recorded listen addresses when the default changes | Same as above |
| `GPUWORKS_DCGM` | `off` | Optional DCGM SM utilization probe: when set to `auto` (case-insensitive), adds one `dcgmi dmon` call per collection cycle if `dcgmi` is on PATH; other values (including `off`) leave it disabled | `agent run` (`--dcgm`) |
| `GPUWORKS_LOG_LEVEL` | `info` | Agent log level; only the exact values `debug` and `warn` are recognized; all other values (including `info` and `error`) use info. Logs are written to stderr and `<state>/agent.log` | All `gpuworks-agent` subcommands |
| `GPUWORKS_RUNTIME_MOUNT_ROOT` | `<state>` for the docker driver; `<state>/runtime-mounts` for other drivers | Root directory of runtime assets that the agent exposes as container bind-mount sources (assets, pre-start, dgpu, overlays, kv-disk); can also be supplied by `MOUNT_ROOT` in the configuration file | `agent run`; `install.sh` (`--mount-root`) writes it to the configuration file |
| `GPUWORKS_UDOCKER_LAUNCHER` | `runveil` | Container launcher for runveil nodes: by default, the agent directly invokes the embedded patched PRoot, and the node does not need Python; `udocker` (case-insensitive) restores the udocker CLI launcher (requires python3 ≥ 3.6). Both launchers use the native implementation for image pulls, imports, and creation | runveil driver |
| `GPUWORKS_UDOCKER_DISABLE_NATIVE` (boolean) | Off | When enabled, uses the udocker CLI for image pulls, imports, and creation (requires python3), solely as an explicit fallback for images unsupported by the native implementation (such as schema1 manifests); native failures do not trigger automatic fallback | runveil driver |
| `GPUWORKS_UDOCKER_BUNDLE` | `<state>/offline/udocker` (extracted from the offline bundle embedded in the agent) | Root directory of the udocker CLI offline bundle, used only when the udocker CLI is needed. When an SSH installation request includes `udocker_bundle`, the installation script (`--udocker-bundle`) writes it to `UDOCKER_BUNDLE` in the node configuration file (the bundle contents come from `udocker_bundle_source` in `server.yaml`) | runveil driver |
| `GPUWORKS_ALLOW_ONLINE_UDOCKER_INSTALL` (boolean) | Off | Allows online (pip) installation of the udocker CLI when the offline bundle is missing or unusable; by default, reports an error immediately to avoid silently changing the source | runveil driver |
| `GPUWORKS_INSECURE_REGISTRIES` | Empty | Registries that the runveil driver allows access to over HTTPS without certificate verification or plain HTTP, specified as comma-separated hosts, host:port entries, or CIDRs, equivalent to dockerd's `insecure-registries`; loopback addresses (`127.0.0.0/8`, `::1`, `localhost`) are always treated as insecure. For these registries, tries HTTPS without certificate verification first and switches to HTTP if the registry only speaks HTTP; all other registries use HTTPS with certificate verification only | runveil driver |
| `GPUWORKS_UDOCKER_DISABLE_TAR_CACHE` (boolean) | Off | When enabled, stops pre-extracting large layers (≥512 MiB) into a tar cache during image pulls (`<state>/udocker/tarcache`, capped at 48 GiB while reserving 20 GiB of free space on the node); useful for nodes with limited disk space | runveil driver |
| `GPUWORKS_UDOCKER_UNPACK_DEBUG` (boolean) | Off | When enabled, logs the unpacking time for each layer to help troubleshoot slow image creation | runveil driver |
| `GPUWORKS_PARTITION_ASSETS` | Empty | Additional search root for dGPU backend runtime assets (such as NVIDIA's `libvgpu.so` and Ascend's `libvruntime.so`), taking precedence over the default locations `<state>/dgpu-runtime`, `<state>/partition-assets`, and `/opt/gpuworks/dgpu-runtime` | dGPU injection and asset reporting; see [Enable and Troubleshoot dGPU](../how-to/dgpu.md) |
| `GPUWORKS_INSTALL_DIR` | `/opt/gpuworks`; `~/gpuworks` on macOS (or the agent directory of an earlier installation) | Installation directory for the agent binary (the default for `--install-dir`) | `install.sh` |
| `GPUWORKS_NO_SYSTEMD` | `0` | Set to `1` to force the nohup/foreground fallback without modifying systemd units | `install.sh` |

### Other General-Purpose Variables Read

| Variable | Purpose | Used by or related command |
|---|---|---|
| `OPENAI_API_KEY` | Default for `--api-key` | `gpuworks-agent bench` |
| `DOCKER_HOST`, `DOCKER_CONTEXT` | The docker driver only accepts a local Unix socket: if `DOCKER_HOST` does not use `unix://` or `DOCKER_CONTEXT` is not `default`, the docker driver is considered unavailable because bind mounts and accelerator devices would be resolved on a remote machine | Automatic runtime detection |
| `ASCEND_HOME_PATH` | Its `version.info` is checked first when detecting the Ascend CANN version | Agent GPU detection |
| `HF_TOKEN` | Access gated HuggingFace repositories | `gpuworks-catalog-model-config` |
| `INVOCATION_ID` | Set by systemd for unit processes; the server uses it to determine whether it is supervised by `Restart=always` and allow "Restart now" in the console | `gpuworks serve` |
| `LC_ALL`, `LC_CTYPE`, `LANG` | Determine whether the locale uses UTF-8, which decides whether `install` prints a block-style or plain ASCII banner | `gpuworks install` |
| `GPUWORKS_API_KEY`, `GPUWORKS_URL`, `GPUWORKS_WS_URL` | Only appear in curl/Python examples generated by the UI; you set them in your own shell, and the program itself does not read them | Inference example snippets |

### Variables for Testing and Development Only

The following prefixes are only used in tests, CI, and development scripts. They are not part of the support contract and may be renamed or removed at any time. Do not add them to production services. Each variable is explained in its corresponding test or script. For test layers and entry points, see the [Testing Guide](../dev/testing.md).

| Prefix | Purpose |
|---|---|
| `GPUWORKS_TEST_*`, `GPUWORKS_TESTENV_*` | Test fixtures and assertion switches (such as `GPUWORKS_TEST_STRICT` and `GPUWORKS_TESTENV_LOG`) |
| `GPUWORKS_FAKE_*` | Fake GPUs and recording runtimes (`GPUWORKS_FAKE_GPU`, `GPUWORKS_FAKE_GPU_*`, `GPUWORKS_FAKE_RUNTIME_*`); the agent reads these variables, which are only for process-level tests |
| `GPUWORKS_FLEET_*`, `GPUWORKS_GATEWAY_*`, `GPUWORKS_LOG_SAMPLE_SECONDS`, `GPUWORKS_LOG_LINES_PER_INSTANCE_PER_SEC` | Parameters for large-scale fleet, gateway load, and log center scale tests |
| `GPUWORKS_L3_*`, `GPUWORKS_L4_*`, `GPUWORKS_CLOUD_*`, `GPUWORKS_ASCEND_*`, `GPUWORKS_PPU_*`, `GPUWORKS_NVIDIA_*`, `GPUWORKS_DOCKER_*`, `GPUWORKS_VLLM_*`, `GPUWORKS_RUNPOD_*`, `GPUWORKS_KEEP_ON_FAILURE`, `GPUWORKS_SSH_PROXY` | Inputs such as images, hosts, accounts, and retention on failure for container-level (L3) and physical-machine/cloud (L4) tests |
| `GPUWORKS_REAL_*`, `GPUWORKS_OBSERVATION_*`, `GPUWORKS_NATIVE_LOAD_*`, `GPUWORKS_UNPACK_BENCH` | Unit tests and benchmarks that require real dependencies |
| `GPUWORKS_UPDATE_*` | Rewrite golden/fixture files when set |
| `GPUWORKS_RELEASE_*`, `GPUWORKS_DIST_DIR`, `GPUWORKS_MAX_*`, and variables in demo scripts under `scripts/` | Release packaging, compatibility checks, and demo scripts (for releases, see [Release Process](../dev/release.md)) |

### Internal Implementation Details

The following names appear in the code but are not user-configurable settings. Do not set them:

- `GPUWORKS_EXPOSE_CONNECTOR_SPEC`: When the connector service (`gpuworks expose run`) starts the public ingress connector helper process (`gpuworks expose connector-host`), it passes the startup request through an environment variable to keep credentials off the command line.
- `GPUWORKS_PARTITION_PROBE_BACKEND`, `GPUWORKS_PARTITION_PROBE_QUOTA_MIB`: Inputs injected into the probe program when the agent starts a dGPU probe container; they are not configuration settings read by the agent.
- `__GPUWORKS_AGENT_PROCESS_OK__`, `__GPUWORKS_AGENT_PROCESS_WARN__`: Marker strings in the output of verification commands after SSH installation on a cloud provider; they are not environment variables.
