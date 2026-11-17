# User Experience Improvement Program

Participating installations send the publisher one anonymous report each day describing their environment, what they are used for, which tasks ran that day, and where errors occurred. The publisher uses this information to prioritize hardware and engine support and error fixes. Participation is enabled by default. This page lists **every field** in the report; the report contains nothing beyond this page (tests in `internal/server/usagereport` ensure the two stay consistent).

## Toggle

- The “User experience improvement program” toggle under Settings → General → “Version and updates” writes `usage_report` to `server.yaml` and takes effect immediately ([Configuration](configuration.md#server-配置文件-serveryaml)).
- For **private deployments**, manually set `usage_report: false` before the server starts for the first time.
- After opting out: no daily reports are sent. Automatic update checks no longer include node counts or GPU models, but still include the installation identifier and server and agent versions to count installations (see [Configuration](configuration.md#镜像目录与镜像拉取)). To stop sending this information as well, disable automatic version checks. In-process counters for the opt-out period are discarded immediately. Opting back in sends a report immediately. Update checks and [publisher notices](#发布方通知) are unaffected.
- Hover over the exclamation mark beside the toggle for a brief explanation. You can retrieve the raw content of the next report with `GET /api/v1/usage-report/preview`; it is byte-for-byte identical to what is actually sent.

## Delivery

- **Only the server sends reports**; the agent is unchanged and makes no outbound connections. One HTTPS POST is sent each day. The uncompressed report is no larger than 128 KiB (if it exceeds this limit, each distribution is trimmed again and the report is marked `truncated`).
- **Timing**: a report is sent immediately on first startup and when participation is re-enabled (covering the previous UTC day). Afterward, one report is sent daily for the previous UTC day, after 00:30 UTC the following day, staggered within a 0–6-hour window using a hash of the installation identifier. Days missed while the server is down are not backfilled; only the most recent day is sent.
- **Scope**: only production installations started with `server.yaml` send reports. A server without `server.yaml` (run directly for testing or development) never sends reports. The environment variable `GPUWORKS_USAGE_REPORT=off` also forces reporting off (both CI and l2 tests set it).
- **Destinations**: two collectors: an Alibaba Cloud Simple Log Service (SLS) WebTracking Logstore in Shanghai (`gpuworks-usage.cn-shanghai.log.aliyuncs.com`, deflate compression), and a collector on Cloudflare (`usage.gpuworks.ai`, gzip compression). The addresses are compiled into the binary and cannot be configured.
- **Routing**: before sending, the server simultaneously sends each collector an empty GET request to measure response time (they return only 4xx responses, write no data, and have a 5-second timeout). The report goes to the first to respond; SLS takes precedence, and Cloudflare moves ahead only if it is more than 50 ms faster. If delivery fails, the server tries the other collector. The measured milliseconds and final choice are recorded in the report's `delivery`. Normally, only one report reaches one collector each day.
- **Failure**: no retries, no alerts, and no effect on any operations; that day's in-process counters are included in the next day's report. An offline environment cannot send reports and experiences no side effects. The Settings page displays the result of the latest delivery.
- **New persisted data**: only one setting, `usage_report_last_day` (the day covered by the most recently delivered report).

## Never Sent

Hostnames, private IP and MAC addresses, node or deployment names, usernames, labels, local paths, private image registry addresses, cloud accounts and regions, original error messages and error parameters, logs, prompts and responses, and your own model names (non-public repositories and local models are always recorded as `custom`). Version numbers retain only the numeric portion (kernel `6.8.0-138-generic` is recorded as `6.8`) to avoid exposing custom host build names.

## Fields

A distribution (the “Distribution” type in the table below) is a `{"name": count}` object. The top 16 entries by count are retained, and the rest are merged into `other`. Names are limited to 64 bytes, and empty names are recorded as `unknown`. `usage.routes` retains the top 64 entries; `errors.events` and `errors.api_errors` retain the top 32.

| Field | Type | Meaning |
|---|---|---|
| `schema` | Integer | Report format version, currently 1 |
| `install_id` | String | Installation identifier: derived one way from `master.key` (HMAC); the key cannot be recovered from it. Used only for deduplication; the same as the `id` included in update checks |
| `day` | String | UTC day covered by the report (`YYYY-MM-DD`) |
| `truncated` | Boolean | `true` if the report exceeds the size limit and is trimmed again; otherwise omitted |
| `delivery.endpoint` | String | Collector selected after response-time probing: `sls` or `collector` |
| `delivery.probe_ms` | Object | Each collector's response time to the probe request in milliseconds; `-1` means no response |
| `server.version` | String | Server version |
| `server.channel` | String | Followed release channel (`nightly` / `release`) |
| `server.os` / `server.arch` | String | Server operating system and CPU architecture |
| `server.schema_version` | Integer | Database migration version |
| `environment.nodes` / `environment.online_nodes` | Integer | Total nodes / online nodes (at report generation time) |
| `environment.agent_versions` | Distribution | Number of nodes running each agent version |
| `environment.os` | Distribution | Node operating systems (`linux`, `darwin`) |
| `environment.kernels` | Distribution | Node kernel versions (major.minor) |
| `environment.arch` | Distribution | Node CPU architectures |
| `environment.vendors` | Distribution | Node accelerator vendors (`nvidia`, `ascend`, `ppu`, `amd`, `hygon`, `apple`) |
| `environment.frameworks` | Distribution | Compute frameworks and major.minor versions, such as `cuda 12.4`, `cann 8.0` |
| `environment.drivers` | Distribution | Vendors and driver major versions, such as `nvidia 550` |
| `environment.runtimes` | Distribution | Container runtimes (`docker`, `nerdctl`, `udocker`, `host`) |
| `environment.connect_modes` | Distribution | Connection modes (`agent_dial`, `server_dial`) |
| `environment.onboarding` | Distribution | Onboarding methods: `ssh` (installed through SSH), `cloud` (cloud provider instance), `agent` (script or manual installation) |
| `environment.gpu_models` | Distribution | Cards owned by the nodes themselves, counted by model |
| `environment.card_ownership` | Distribution | Cards unavailable to the platform and the reasons (`passthrough`, `no_driver`, `held_elsewhere`) |
| `environment.card_health` | Distribution | Cards with health issues, counted by health code |
| `environment.providers` | Distribution | Types of configured cloud providers (excluding accounts and regions) |
| `environment.storages` | Distribution | Storage types (`local`, `nfs`, `nfs_cache`, `shared_dir`) |
| `usage.deployments` | Integer | Total deployments |
| `usage.deployment_engines` | Distribution | Number of deployments for each inference engine |
| `usage.deployment_states` | Distribution | Desired deployment states (`active`, `stopped`, `deleting`) |
| `usage.multi_node` / `usage.pd` / `usage.sleepable` | Integer | Counts of multi-node deployments, PD disaggregated deployments, and deployments that support sleep |
| `usage.dgpu` | Distribution | Deployments using dGPU, counted by backend |
| `usage.template_sources` | Distribution | Sources of the templates used by deployments: `upstream` (built-in), `user`, `none` |
| `usage.templates` | Distribution | Built-in templates in use (the template's model ID) |
| `usage.models` | Distribution | Deployed models: public repositories are recorded as `huggingface:<repository>` / `modelscope:<repository>`, and all others as `custom`. A repository is considered “public” only when this can be established: the deployment uses a built-in template, or the platform has no access credentials configured for that model hosting site (all downloads are anonymous) |
| `usage.instance_states` | Distribution | Instance states |
| `usage.gateway_requests` | Distribution | Requests through the gateway that day, counted by the deployment's engine |
| `usage.logins` | Distribution | Management sign-ins that day: `ok` / `failed` |
| `usage.routes` | Distribution | Management API calls that day, counted by “method route template” (such as `POST /nodes/{uuid}/upgrade`), excluding path parameter values |
| `tasks.deployments_created` | Distribution | Deployments created that day, counted by engine |
| `tasks.instances_started` | Integer | Instances created that day |
| `tasks.instances_ready` / `tasks.instances_failed` | Integer | Of those instances, the number ready / failed by report generation time |
| `tasks.ready_seconds` | Distribution | Time-to-ready buckets: `<30s`, `30s-2m`, `2m-10m`, `10m-30m`, `30m-2h`, `>2h` |
| `tasks.model_files` | Distribution | Model files created that day, counted by “acquisition method status” (such as `download present`, `peer failed`) |
| `tasks.model_bytes` | Distribution | Size buckets for model files downloaded that day: `<1G`, `1G-10G`, `10G-100G`, `100G-500G`, `>500G` |
| `tasks.benchmarks` | Distribution | Benchmarks created that day, counted by “search mode dataset type status” |
| `errors.events` | Distribution | Deployment timeline events that day, counted by event code (such as `UNIT_RETRY_EXHAUSTED`, `DEPLOYMENT_UNSCHEDULABLE`) |
| `errors.api_errors` | Distribution | Product errors returned by the management API that day, counted by error code |
| `errors.panics` | Distribution | Panics recovered that day, counted by the innermost platform function name where they occur (excluding files, line numbers, and arguments) |
| `errors.images_failed` / `errors.model_files_failed` | Integer | Image replicas / model files in a failed state at report generation time |
| `errors.protocol` | Distribution | Agent connection issues that day, counted by reason (`proto_mismatch`, `bad_token`, `stale_session`, `data_lane_unavailable`) |

## Publisher Notices

The bell in the page header lists notices the publisher sends to this installation: security advisories, important releases, and community events.

- Notices are a signed file, `notices.json`, in the release channel, signed with the same key as the version index ([ADR 0040](../adr/0040-signed-multi-source-distribution.md)). They are fetched with update checks (at most once per hour per channel; when `auto_check_updates` is enabled, they are also fetched in the background every 6 hours). Files that fail signature verification are always rejected.
- The server determines applicability using local facts (channel, server build time, and accelerator vendors in the cluster). **This assessment sends no information to the publisher** and is independent of participation in the improvement program.
- Notices contain only plain text and one https link; they cannot trigger any action. Applicable security notices also generate a warn entry in the server log, allowing environments where no one signs in to the console to detect them through log alerts.
- Read status is recorded per user. When the publisher withdraws a notice, its read records are deleted on the next fetch.

For design tradeoffs, see [ADR 0054](../adr/0054-usage-report-and-notices.md).

<!-- translation-source-sha256: ee3391351bad92ba79f30b8941f771de244963cb47d94765fdce32d677f3de12 -->
