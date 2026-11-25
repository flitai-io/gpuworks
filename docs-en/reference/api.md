# Complete REST API Routes

This document lists the HTTP endpoints for managing GPUWorks resources, including nodes, models, deployments, and settings. Management endpoints use the `/api/v1` prefix;
authentication and response conventions appear first, followed by endpoints organized by resource type. Replace `{id}`, `{uuid}`, and similar placeholders in paths with actual resource identifiers.

For your first call, see [Log In and Query the Inference Service List](api-conventions.md#示例登录并查询推理服务列表), which includes address, login, request, and response examples.

Model inference uses a separate set of endpoints: `/v1/*` is the external OpenAI-compatible gateway, and `/api/v1/playground/v1/*` is
the internal Playground proxy. See [OpenAI-Compatible Gateway](openai-gateway.md) for their calling conventions; they are not listed again here.
The OpenAI-compatible batch endpoints (`/v1/files`, `/v1/batches`) are the exception: they are listed together with the management batch routes in the [Batches](#batches批处理任务) section.

## Authentication Model

### Distinguish Credential Purposes First

Authentication first establishes who is making a request, then checks whether that identity can perform the operation. Choose credentials for your access purpose; management, node connection, and inference credentials are not interchangeable:

| Credential | Identity and usage | How to provide it |
|---|---|---|
| Management login session | A logged-in user, for management APIs and the console | Browsers use the `gpuworks_session` Cookie; programs take the `token` from the login response and send it in the `Authorization: Bearer <JWT>` request header |
| Platform API token | The token's owner, for programmatic management API access | `Authorization: Bearer gwp_…`; token scope further restricts permissions |
| Node token | A registered node, for agent connections | The hub handshake at `GET /api/v1/agent/connect` (a yamux connection is established after the WebSocket upgrade) |
| Inference API key | An inference caller, for the `/v1/*` gateway | See [OpenAI-Compatible Gateway](openai-gateway.md); not used to log in to management APIs |

The Cookie and JWT for a management login session are two credentials from the same login. Both must be associated with a session that is still valid on the server.
Missing or invalid credentials, or an expired or revoked session, return `401 AUTHENTICATION_REQUIRED`. See [Auth](#auth) below for login and expiration rules.

### The Authentication Column in Route Tables

| Label | Calling requirements |
|---|---|
| `JWT` | Any valid management identity; use a login session or a platform API token within its allowed scope |
| `JWT(admin)`, `admin` | In addition to management authentication, the current user must have the `admin` role; otherwise, returns `403 ADMINISTRATOR_REQUIRED` |
| Management session | Accepts only a Cookie or JWT obtained by logging in; does not accept platform API tokens |
| None | Does not validate management JWTs; the endpoint may still enforce constraints such as origin checks |

`JWT` is a historical abbreviation in these tables. It does not mean that only Bearer JWTs are accepted. Routes that use dedicated credentials, such as node connections, are labeled separately.

### Additional Restrictions on Platform API Tokens

Platform tokens operate with the owner's **current role**, further restricted by their own `read` or `write` scope.
For example, a regular user's `write` token cannot perform administrator-only operations. See [Platform Tokens](#platform-tokens平台-api-令牌) for creation and revocation.

| Condition | Behavior or error |
|---|---|
| Using a `read` token | The read-only scope check allows GET, HEAD, OPTIONS, and the read-only tool entry point `POST /api/v1/mcp`; other write requests return `403 PLATFORM_TOKEN_READ_ONLY`. Whether the endpoint supports the HTTP method still depends on the specific route |
| Using any platform token to access `/api/v1/platform-tokens` or `auth/activity` | Returns `403 PLATFORM_TOKEN_NOT_ALLOWED`; these operations require a login session |
| The token is unknown or expired, or its owner has been deleted | Returns `401 AUTHENTICATION_REQUIRED` |

### Public Installation and Health Check Endpoints

The following endpoints are outside `/api/v1` and publicly accessible:

| Path (all use GET) | Purpose |
|---|---|
| `/install.sh` | Agent installation script |
| `/healthz` | Liveness and load balancer health checks; the management endpoint `/api/v1/healthz` still requires authentication |
| `/dl/gpuworks-agent-<os>-<arch>` | Download the agent binary for the specified operating system and architecture |
| `/dl/gpuworks-agent.run` | Offline agent installation package |
| `/dl/gpuworks-agent.run.sha256` | Offline package checksum in `sha256sum` format |

Downloads are served from the node connection listener address. The offline package is a self-extracting sh script that includes the linux-amd64, linux-arm64, and darwin-arm64
agents and `install.sh`. It does not contain a node token. If the server has no embedded agents, the offline package returns 404.

## Error and Response Conventions

### JSON Fields and List Structure

JSON fields use snake_case, such as `node_uuid`, as described in [API Conventions](api-conventions.md).
When returning a collection of objects, elements appear in the `items` field rather than in a top-level array. `total` is the total number of matches after filtering:
without pagination, it equals the length of `items`; with pagination, it may exceed the number of items on the current page.

| List type | Response structure and meaning |
|---|---|
| Unpaginated | `{"items": [...], "total": N}`, returning all matching objects |
| Page-based pagination | Also includes `page` and `page_size`, the current page number and the requested number of items per page, respectively |
| Cursor-based pagination | Returns `{"items": [...]}`; the cursor marks the last position read, and the endpoint determines whether a total is returned |

### Reading Error Responses

Check the HTTP status first, then read `error` (the error description), `code` (a stable error code), and `params` (related values).
Ordinary errors may contain only `error`; do not assume that `code` or `params` is always present.
See [HTTP API Conventions: Errors](api-conventions.md#错误) for examples and general handling. The sections below list only endpoint-specific restrictions and errors.

### Nodes and Deployments: Pages, Filters, and Sorting

The following rules apply to `GET /api/v1/nodes` and `GET /api/v1/deployments`. Both require management authentication and have no request body.
Provide the following optional parameters in the URL query string:

| Parameter | Type | Default and usage |
|---|---|---|
| `page` | Integer | Page numbers start at 1; defaults to 1 when pagination is enabled and this parameter is omitted |
| `page_size` | Integer | 1–200 items per page; defaults to 50 when pagination is enabled |
| `q` | String | No name filtering when omitted; performs a case-insensitive substring match. See the corresponding resource below for matching fields |
| `state` | String | No state restriction when omitted; separate multiple values with commas to match any of them |
| `sort` | String | Newest first when omitted or empty; `sort=<column>` sorts in ascending order, and `sort=-<column>` sorts in descending order. See the corresponding resource below for available columns |

Including either `page` or `page_size` enables pagination, and the response contains `items`, `page`, `page_size`, and `total`.
When both parameters are omitted, all matching objects are returned for convenient scripting and programmatic access; filtering and sorting still apply.
Processing **filters all objects first, sorts them next, and then selects the current page**. Objects with equal sort values remain ordered newest first.

Invalid page numbers or page sizes return `400 LIST_PAGE_INVALID` rather than silently falling back to defaults.
Unknown filter values or sort columns return `400 LIST_FILTER_INVALID`.

`GET /api/v1/inference-templates` uses the same page validation and response structure but **always paginates**, defaulting to page 1 with 50 items per page.
It also has hardware and text filters: `gpu` is the card name, `vendor` is optional, and `cc` and `arch_id` each require `gpu`. These are caller-supplied filter values; they do not attest to hardware detected or verified by the server. `GET /api/v1/models` and `GET /api/v1/gpus` also support pagination; see their route entries for parameters.

### Real-Time Event Streams and Response Compression

`GET /api/v1/events` and `GET /api/v1/instances/{uuid}/logs` use SSE, a continuous stream of server-sent events,
with `Content-Type: text/event-stream`. Do not read them as a single JSON response.
`/api/v1/events` only indicates which resources have changed; it does not carry resource objects. After receiving a notification, clients fetch the corresponding resources again.
Every agent reports its status once a minute, which arrives as a `node` event with `reason` `status`; one stream forwards at most one such event every 5 seconds:
the first in a window goes out at once and the rest collapse into one event, with an empty `key`, at the window's end. Other node changes (going online or offline, scheduling, deletion) are never coalesced.

For regular management resource endpoints that return `Content-Type: application/json`, send `Accept-Encoding: gzip` to enable gzip compression.
Event streams, log streams, file transfers, and image transfers remain unchanged. WebSocket upgrades are unaffected.

### How Setting Changes Reach Nodes

Changes to pull credentials, networks and their attachments, node labels, storages (create, edit, node overrides), the orphan cleanup switch or the image catalog return as soon as they are saved, without waiting for the nodes: the server schedules one state update for each affected online node, each node is updated on its own without waiting for the others, changes in quick succession are sent together, and what is sent is the latest state at that time; the image catalog is sent about 250 ms after the first change, with the changes made meanwhile. A successful response means only that the change is saved. A node that cannot receive an update for now does not hold up the others, and gets the complete latest state when it reconnects. Deleting a storage still waits for the online nodes before it returns.

## Auth

| Method | Path | Description | Authentication |
|---|---|---|---|
| POST | `/api/v1/auth/login` | Log in with a username and password and create a persistent session; returns a session-bound JWT and sets an opaque Cookie | None |
| POST | `/api/v1/auth/logout` | Revoke the session associated with the submitted credential and clear the Cookie; repeated logout is idempotent | None |
| GET | `/api/v1/auth/me` | Return the current user and live permissions; does not extend the idle timeout | Management session or platform API token |
| POST | `/api/v1/auth/activity` | Report real activity and extend the idle timeout; no request body; returns 204 on success | Management session |
| POST | `/api/v1/auth/password` | Change the signed-in user's own password; on success all of that user's management sessions end | Management session |

### Login Request and Response

`POST /api/v1/auth/login` requires no existing login credentials. Send a JSON request body with `Content-Type: application/json`.
There are no path or query parameters.

| Request field | Type and requirement | Meaning and constraints |
|---|---|---|
| `username` | String, required | Username; leading and trailing whitespace is trimmed for lookup; up to 256 characters; must not contain control characters |
| `password` | String, required | The user's password, validated as provided without trimming whitespace |

The request body is limited to 1 MiB and must be a single JSON object, not multiple concatenated JSON values.
Success returns HTTP 200 and sets `gpuworks_session` through `Set-Cookie`. Example response structure:

```json
{
  "token": "<JWT>",
  "user": {"id": 1, "username": "admin", "role": "admin"}
}
```

`token` is the JWT for subsequent requests; `user` contains the user ID, username, and role (`admin` or `user`).
For a complete request example, see [Set the Address and Log In](api-conventions.md#1-设置地址并登录).

`auth/me` returns current user information and the length a new password must have, in the form `{"id":1,"username":"admin","role":"admin","local_admin":true,"password_policy":{"min_length":8,"max_length":128}}`.
`local_admin` says whether this request has the plugin host's local administrator floor (see [Installable extensions](../dev/extensions.md#local-administrator-floor)): it is `true` only for a user whose role is `admin`, who is marked a local administrator in the database, and who calls with a login session; a platform API token always reads `false`. It is for display only.
The Cookie is set to `HttpOnly` and `SameSite=Lax` and stores an opaque session credential, not a JWT.
When a request includes both Bearer and Cookie credentials, Bearer takes precedence; an invalid Bearer does not fall back to the Cookie.

An incorrect username or password returns `401 INVALID_CREDENTIALS`; overly frequent login attempts return `429 LOGIN_THROTTLED`.
Wait for the number of seconds specified by the `Retry-After` response header before retrying. For other errors, see [Common HTTP Statuses and Error Codes](api-conventions.md#常见-http-状态与错误码).

### Change Password

`POST /api/v1/auth/password` changes only the signed-in user's own password. It accepts only a login session (a platform API token returns `403 PLATFORM_TOKEN_NOT_ALLOWED`) and applies the same origin checks as login.

| Request field | Type and requirement | Meaning |
|---|---|---|
| `current_password` | String, required | The current password, verified as is |
| `new_password` | String, required | The new password; it must meet the [administrator password rules](security.md#管理员密码) |

Success returns `200 {"status":"password_changed"}` and clears the current Cookie: all of the user's management sessions, this one included, end, and the user signs in again with the new password. If `initial-credentials` in the data directory records this user, the server tries to delete it; a failure is only logged as a warning.

Both passwords are checked in the same request, and every error returns 400:

| Case | Error code | Notes |
|---|---|---|
| The current password is wrong | `CURRENT_PASSWORD_INVALID` | When the new password is also rejected, `params.new_password` holds its error code |
| The new password's length is out of range | `PASSWORD_LENGTH` | `params.min` and `params.max` are the bounds |
| The new password is a common weak password, a repetition or a sequence | `PASSWORD_GUESSABLE` | |
| The new password has control characters or is not valid UTF-8 | `PASSWORD_CHARACTERS` | |
| The new password is the current one | `PASSWORD_UNCHANGED` | Judged only when the current password is right |

Checking the current password shares the login throttle (counted per source address and username); too many attempts return `429 LOGIN_THROTTLED` with `Retry-After`.

### When Sessions Expire

By default, a session expires after 24 hours without real activity and lasts no more than 7 days from login. The Cookie expiration uses the latter absolute deadline.
Both time limits are configurable; see [Management Session Configuration](configuration.md#管理会话). The JWT in the login response has its own 24-hour validity period
and must also be associated with a valid session. An unexpired JWT alone does not mean the login is still valid.

| Situation | Result |
|---|---|
| Calling `auth/me` or passive polling | Does not extend the idle timeout |
| Performing management write requests through a login session, or calling `auth/activity` to report real activity | Extends the idle timeout but not the absolute deadline measured from login |
| Password change or reset (console or `reset-password`) | Revokes the user's management sessions and invalidates associated JWTs; does not revoke platform API tokens or API keys |
| Logout | Revokes the session associated with the submitted credential, invalidates associated JWTs, and clears the Cookie; repeated logout still succeeds |
| A JWT not bound to a session before the upgrade, or an old Cookie | Requires logging in again |

Platform API tokens authenticate independently according to their own scope and validity period; `auth/activity` does not extend them.

### Login Endpoint Errors and Origin Checks

An invalid session returns `401 AUTHENTICATION_REQUIRED`; unavailable session storage returns 503.
Login, logout, and management write requests using login sessions check the request origin; cross-origin requests return `403 MANAGEMENT_ORIGIN_REJECTED`.

## Nodes (Node Connection)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/agent/connect` | WS upgrade entry point for persistent agent connections | Node token (not a management JWT) |
| GET | `/install.sh` | curl\|sh installation script | None |
| GET | `/api/v1/healthz` | Health check (control plane) | JWT |
| GET | `/api/v1/readyz` | Business readiness check (no login required, for load balancers and the cluster entry): 200 with the fixed body `{"status":"ready"}` when ready; 503 while the business database cannot be read or the member has not opened business; the response carries no settings, identities or counts | None |
| GET | `/api/v1/peer-transfers` | Peer transfer observations for this management-server process. Optional single `transfer_id`; returns effective relay policy, active relay count, bandwidth waiting count, cumulative results, and bounded runtime observations. Missing observations return an empty list; unavailable capability returns 503, invalid filters 400. Counters measure inner TLS stream bytes, not verified file progress. | JWT |
| GET | `/healthz` | Health check (no login required, for liveness/load balancing) | None |
| GET | `/api/v1/engines?target_vendor=amd` | Engine parameter dictionary for a target vendor (uses the general profile when `target_vendor` is omitted); the AMD profile describes implementation admission scope, not validation on a specific GPU/ROCm version | JWT |
| GET | `/api/v1/accelerator-vendors` | The accelerator vendor dictionary, given by each vendor's definition and independent of the nodes in the cluster: each item has `id`, `brand`, `frameworks` (the image frameworks that belong to the vendor), `runtimes` (the runtime drivers its cards carry workloads on; empty means found and monitored only), `features` (the platform features offered on its cards), and an optional `partition_backend` (the dGPU backend). The console takes the vendors' order, names and frameworks from it | JWT |
| GET | `/api/v1/nodes` | List nodes; paginate with `page`/`page_size` and filter by `q`/`state`/`label` (see “Error and Response Conventions”); `source=` filters by where nodes came from, comma-separated values being alternatives: `provider:<external platform account id>` is the nodes that joined through that account (on one of its instances, or through a cluster connected from it), `cluster:<cluster entry uuid>` the nodes that joined through that cluster, and `manual` the nodes added by hand; an account or cluster that does not exist matches no node, and a value of no known form answers 400 `LIST_FILTER_INVALID`; a paged response also carries `sources`, the sources there are to choose from across every node (whatever the filters), each `{value,provider_name?,provider_kind?,cluster_name?,nodes}` where `value` is the filter value above, accounts first, then clusters, then `manual`, an account's `nodes` taking in its clusters' nodes; each item's `source` is the external platform the node joined through, `{resource,provider_id?,provider_name?,provider_kind?,cluster_uuid?,cluster_name?}` (`resource` is `vm`, `container`, `host`, `kubernetes` or `slurm`, or an empty string when the resource cannot be told (the account's kind connects several kinds of instance and the instance does not say which it is, or the kind is unknown), the field still being present; a cluster connected on its own has no `provider_*`; a node added by hand has no `source`); when the server cannot read where nodes came from, each item carries `source_unknown: true` instead, with no `source` (so it is not taken for a node added by hand; the field is omitted otherwise), a paged response's `sources` is an empty array, and a request with `source=` answers 500; nodes connected from a Kubernetes cluster carry the system labels `gpuworks.ai/kubernetes-cluster` and `gpuworks.ai/kubernetes-node` (see “Node Labels: Written and Generated”), and `cordon_reason` is `kube_unschedulable` while Kubernetes has them cordoned | JWT |
| GET | `/api/v1/nodes/facets` | Filter options and node count distributions; the `q`, `state`, and `label` query parameters are the same as for the node list. Returns distributions of user labels, system labels, and GPU models (including reported `arch_id`); see [Node List](#节点列表的筛选与返回字段) | JWT |
| GET | `/api/v1/dashboard/summary` | Dashboard summary: node counts by state and hardware totals, deployment counts by state, and capacity grouped by vendor and label (`label_key`; uses the most common key when omitted or when no node has that key); also current GPU metrics, card occupancy and tasks in flight, see [Dashboard summary](#dashboard-summary-response-fields) | JWT |
| POST | `/api/v1/nodes` | Create a node awaiting connection and return the installation command and token; see [Connection Parameters for Node Creation](#创建节点的连接参数) for all fields and conditions | JWT(admin) |
| GET | `/api/v1/agent/rejects` | Recent rejected connection attempts | JWT |
| GET | `/api/v1/agent/upgrade-errors` | Recent agent upgrade failures | JWT |
| POST | `/api/v1/agent-upgrades/preview` | Preview eligibility and target for all or selected nodes | JWT(admin) |
| POST | `/api/v1/agent-upgrades` | Create a run using the preview UUID snapshot, target, manifest fingerprint, and idempotency key | JWT(admin) |
| GET | `/api/v1/agent-upgrades/summary` | Read the running server’s Agent target version, group counts and the active batch ID. The eligible total `align` is the sum of `upgrade` (older Agent), `downgrade` (newer Agent) and `different_build` (unknown order). The `check` count covers online Agents currently unavailable for upgrade. Offline Agents are counted separately in `offline`. Paged node queries accept `align`, its three eligible groups, `check`, or `all` for `agent_upgrade` before pagination and return the fleet-wide `agent_fleet`, per-row `agent_eligibility`, and `agent_version_change` on eligible rows | JWT |
| GET | `/api/v1/agent-upgrades` | List paginated runs and the global blocking item | JWT |
| GET | `/api/v1/agent-upgrades/{id}` | Read run progress and counts | JWT |
| GET | `/api/v1/agent-upgrades/{id}/items` | Read paginated node outcomes, optionally filtered by state | JWT |
| POST | `/api/v1/agent-upgrades/{id}/stop` | Cancel queued nodes and keep observing the active node | JWT(admin) |
| POST | `/api/v1/agent-upgrades/{id}/continue-preview` | Preview remaining nodes from a paused run | JWT(admin) |
| POST | `/api/v1/agent-upgrades/{id}/items/{uuid}/release` | Release an unconfirmed reservation with an administrator audit note | JWT(admin) |
| POST | `/api/v1/nodes/precheck` | Check the host environment and bidirectional connectivity required for SSH connection; see [SSH Checks and Installation](#ssh-检查与安装) for address rules | JWT(admin) |
| POST | `/api/v1/nodes/install` | Install the agent through SSH; connection parameters are the same as for node creation. Use `no_systemd` to select the Linux background execution mode. See [SSH Checks and Installation](#ssh-检查与安装) for conflicts and replacement conditions | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}` | Node details (including the GPU list; also includes the token for administrators) | JWT |
| GET | `/api/v1/nodes/{uuid}/fs` | Browse the node filesystem (for registering local models) | JWT |
| POST | `/api/v1/nodes/{uuid}/doctor` | Trigger the node self-check RPC. Each of `checks[]` has `stage`, `status` (`pass`/`warn`/`fail`/`skip`/`na`) and `code` (UPPER_SNAKE); an agent finding also carries `finding` (`id`, `status`, `code`, evidence and recheck command); `summary` is the most serious status among them | JWT |
| POST | `/api/v1/nodes/{uuid}/connection-diagnostics` | Read Agent runtime observations over existing ctrl without SSH; also saved URL, optional SSH and public-entry history. Empty body or `{}`, no recovery/address changes; see [Node Connection Diagnostics](#节点连接诊断) | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/upgrade` | Trigger a remote agent upgrade; a Kubernetes node upgrades in place like any node (its agent runs from the node's state directory and declares `upgrade.kube_binary`); one that could not start from the state directory (noexec, say) does not declare it, and the upgrade marks it `kubernetes` and skips it | JWT |
| PUT | `/api/v1/nodes/{uuid}/scheduling` | Set `scheduling` to `cordoned`, `draining`, or `schedulable` and return the node; invalid values return 400 `FIELD_INVALID`. See [Node Scheduling and Accelerator Switching](#节点调度与加速卡切换) for the effects of each state | JWT |
| PUT | `/api/v1/nodes/{uuid}/gpus/scheduling` | Set whether one card takes new deployments: `{"pcie_bdf":"0000:3f:00.0","scheduling":"stopped"}` (a card without a PCI address is named by `uuid`). `stopped` disables scheduling and `schedulable` enables it. Both persist until an administrator changes the decision, across reports and server restarts, with no expiry. Health and outside use remain visible; cards without an explicit decision still follow automatic verdicts. Returns the node (with its cards); a card's `scheduling_stopped` and `scheduling_allowed` give its state and `scheduling_actions` what may be done now. Invalid values or no card named: 400 `FIELD_INVALID`; unknown node or card: 404; making a passed-through or driverless card schedulable: 409 `CARD_NOT_OWNED` | JWT |
| GET | `/api/v1/nodes/{uuid}/health-checks` | The current state of each check on each of the node's cards, as a table (read from server memory, never reaching the agent): `checks` are the columns (only checks that apply to some card, with their period in seconds), `cards[].cells[]` the cells (`status` is `pass`/`warn`/`fail`/`skip`/`na`, `code` a finding code or the code for why a check was not run, such as `CARD_CHECK_NOT_RUN_YET` before its first run; `checked_at`, `duration_ms`, `stale`), and `cards[].worst` (the most serious of `fail`, `warn`, `skip`, `pass`, `na`), `cards[].can_recheck` and `summary` (cells per status) are computed by the server. An older agent's lower-case codes and old state words are converted when the server reads them. `?pcie_bdf=` (or `uuid=`) narrows it to one card. Before the node has reported, `cards` is empty and `reports` false; offline, the last known states are given and marked `stale` | JWT |
| GET | `/api/v1/nodes/{uuid}/health-checks/detail?pcie_bdf=&check=` | The last run of one check on one card (`last`; `last_ran` too when `last` was skipped; `last_problem` when an earlier run found a problem; each run has `status`, `code`, `at`, `duration_ms`, a `probe` command to recheck it, and `evidence`), fetched from the agent over RPC; the agent answers from memory and runs nothing. Concurrent requests for the same cell make one call and its answer is reused for 5 seconds; at most 2 requests per node and 16 in all are in flight, beyond which it answers 429 (with `Retry-After`). Missing card or unknown check: 400 `FIELD_INVALID`; node offline: 409 `NODE_OFFLINE`; agent too old: 409 `CARD_CHECKS_UNSUPPORTED` (neither calls the agent) | JWT |
| GET | `/api/v1/nodes/{uuid}/health-records` | The node's failure records (a stretch in which a check warned or failed): ongoing ones first, then the newest; filtered by `?pcie_bdf=` (or `uuid=`) and `?check=`, paged with `page`/`page_size` (50 per page by default). Each has `card_ident`, the `card_index`/`card_name`/`pcie_bdf` of a card still on the node (`removed` is true for one that is gone), `check`, `status` (`warn`/`fail`), `code`, `started_at`, `ended_at` (absent while ongoing), `last_seen_at`, `duration_seconds`, `count`, `flapping`, `ongoing`, `first_evidence` and `latest_evidence`; `retention_days` is how long records are kept (30) | JWT |
| POST | `/api/v1/nodes/{uuid}/gpus/recheck` | Run the runtime check on one card now: `{"pcie_bdf":"0000:3f:00.0"}` (or `uuid`). The answer is recorded on the card like the periodic check's, so its state follows at the next report; returns the runtime check's detail as `health-checks/detail` does (`last.status` is `skip`, with `last.code` saying why, when the card is serving, used outside the platform, or the host cannot be asked right now). Errors as above | JWT |
| GET | `/api/v1/nodes/{uuid}/replicas` | Instances still running on the node and how eviction handles them: `{"scheduling", "items": [{"instance", "deployment_id", "deployment", "state", "kind"}]}`. `kind` is `pool` (moves to another node), `pinned` (the deployment is pinned only to this node and waits), or `named` (a multi-node or PD rank: a drain starts a replacement on another node first and switches over once it runs, waiting if there is no node to go to; only forced eviction removes it immediately); stopped and failed instances are excluded. While draining, rows may also include `drain_reason` (latest structured scheduling feedback), `drain_reason_at`, `drain_message` (the English message of the latest failed attempt on another node), and `drain_failure` (that attempt's structured failure reason, shaped like an instance's `runtime_failure`; absent for older attempts). These are existing observations, not a new persisted drain state; use the timestamp to distinguish historical feedback. The optional top-level `drain_offline` indicates that the agent is currently disconnected, so retirement and container removal cannot be confirmed; it clears after reconnection. | JWT |
| POST | `/api/v1/nodes/{uuid}/evict` | Force eviction: immediately remove every replica on the node without waiting for replacements (the replica's leader drains, followed by its followers). Replicas are recreated wherever placement is possible, without counting toward retries; returns `{"evicted": [leader UUID…]}` (202). Returns 409 `NODE_SCHEDULABLE` if the node is still schedulable | JWT |
| PUT | `/api/v1/nodes/{uuid}/labels` | Replace node labels (key/value; an empty object clears them). Keys must not use the platform-reserved `gpuworks.ai/` namespace or its subdomains | JWT |
| PATCH | `/api/v1/nodes/{uuid}/name` | Rename the node | JWT |
| PATCH | `/api/v1/nodes/{uuid}/accelerator` | Select the accelerator vendor managed on the node: `{"accelerator":"auto"}`; also accepts `nvidia`, `ascend`, `hygon`, `ppu`, or `amd`. On success, restarts the agent and returns the node; see [Node Scheduling and Accelerator Switching](#节点调度与加速卡切换) for restrictions | JWT |
| GET | `/api/v1/nodes/{uuid}/pull-proxy` | The proxy used by the node: `proxy` (the first proxy entry that selects it, in the same form as an item in `/pull/proxies`; `null` when no entry selects it, in which case the node agent uses its own environment) and `runtime`: the runtime type. Docker nodes also include the proxy actually used by dockerd, whether it matches the entry (`applied`; when the node has no proxy, compares systemd's currently loaded `configured_*` configuration with actual values), live-restore, whether `daemon.json` also configures a proxy, and the agent's privileges (`root` / `sudo` / `none`) | JWT |
| GET | `/api/v1/nodes/{uuid}/network` | Removed: 410 `SETTING_REMOVED` (`route` points to `/api/v1/pull/proxies`). Node proxies and site addresses are configured by node labels in pull settings (`/nodes/{uuid}/pull-proxy` identifies the entry used by the node) | JWT |
| PUT | `/api/v1/nodes/{uuid}/network` | Removed: 410 `SETTING_REMOVED`, as above | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/runtime/restart` | Restart the node's dockerd to apply the proxy, executed by the agent (requires root or passwordless sudo); failures return `RUNTIME_RESTART_FAILED`. If dockerd does not have live-restore enabled, the node's containers also stop | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/networks` | Networks resolved for each role on the node (NIC/IP/HCA/hints), the management network (address, NAT, disabled roles), reported NICs, RDMA port inventory (including send/receive rates over the last two minutes), and accelerator network port inventory (`accelerator_ports`) | JWT |
| GET | `/api/v1/nodes/{uuid}/ssh` | View the node's SSH credential configuration (without plaintext keys) | JWT |
| PUT | `/api/v1/nodes/{uuid}/ssh` | Write the node's SSH credentials (for offline recovery) | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}/ssh` | Delete the node's SSH credentials | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/recovery/probe` | Manually trigger one recovery probe for an offline node; when it does nothing, `skipped_code` (`RECOVERY_SKIPPED_*`) says why, and on failure `error_code` is the same code recorded on the node; `skipped_reason` and `error` are the English text | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/token/regenerate` | Regenerate the node token (disconnects the current connection) | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}` | By default, uninstall the agent using saved SSH credentials (Kubernetes nodes are not uninstalled: the agent belongs to the cluster's DaemonSet and re-enrolls as long as the DaemonSet still selects the node), delete the record after confirming completion, and return `{"status":"deleted","agent_uninstalled":true}`. If no SSH credentials are saved, return 409 `NODE_DELETE_SSH_REQUIRED`, retain the node, and suggest configuring SSH and retrying, or manually uninstalling the agent before using `?force=true`. If uninstallation is not confirmed, return 409 `NODE_UNINSTALL_UNCONFIRMED`, retain the node and SSH credentials, and suggest fixing SSH and retrying, or manually uninstalling before force removal. Explicitly specifying `?force=true` skips uninstallation, deletes only the record, and returns `agent_uninstalled:false`; both modes reject nodes with instances (409). A non-boolean force value or repeated parameter returns 400; a nonexistent node returns 404 | JWT |
| GET | `/api/v1/events` | SSE change event stream | JWT |

### Bulk Agent Upgrades

An administrator first posts the platform-wide scope to `/api/v1/agent-upgrades/preview`, independently of node list filters and pagination:

```json
{"scope":"all"}
```

Alternatively, submit `{"node_uuids":["node-a"]}`. The preview returns the complete `node_uuids` snapshot, `target_version`, `manifest_fingerprint`, per-node eligibility in `nodes`, eligibility `counts`, and the global `blocked_by_item`. Reuse that snapshot and target in the create request:

```json
{
  "node_uuids": ["node-a", "node-b"],
  "expected_target_version": "v0.6.0",
  "expected_manifest_fingerprint": "<preview.manifest_fingerprint>",
  "idempotency_key": "<client-generated-unique-key>"
}
```


The preview’s `target_build` and the update API’s `server.current_build` share running-build metadata: `version`, `kind` (`stable`, `prerelease`, `development`, `unknown`), and, when available, `commit`, `built_at`, `build_channel`, `published_version`, and `published_at`. Build time describes the running binary, not a remote candidate. Published fields require the version, full commit, and build time to match a verified signed index. Agent previews read only the existing update-check cache and do not make new outbound requests; local artifact previews remain available when a source is unavailable or no record matches.

Success returns 202 and a run object with `id`, `state`, and `counts`. An identical request with the same idempotency key returns the original run. A changed request, target, or artifact returns 409 and requires another preview. `GET /agent-upgrades` and `GET /agent-upgrades/{id}/items` support `page` and `page_size`; items also accept a single `state` filter. `processed` includes all terminal outcomes and does not mean `succeeded`; `unconfirmed` remains an unknown outcome that reserves the global execution slot.

Stopping cancels only queued items. To continue, call `continue-preview`, then create a new run with `source_run_id` pointing to the original run; cancellation of the original queue and creation of the new run are atomic. After verifying an unconfirmed node, an administrator posts `{"note":"处理依据"}` to `items/{uuid}/release`; success returns 204. The original result remains and verification is audited separately. Management identities can read results; bulk writes require an administrator and reject read-only platform tokens. The existing single-node route retains its permissions, adds `run_id` to its 202 response, and shares the global execution reservation. For workflow and workload impact, see [Upgrade the Agent](../how-to/upgrade-and-compatibility.md#升级-agent).

### Node List Filters and Response Fields

`GET /api/v1/nodes` and `/api/v1/nodes/facets` explicitly read the latest committed node and GPU facts and refresh the shared fleet snapshot. Changes committed through an external database connection do not wait for snapshot expiry. Uncommitted changes remain invisible, and long write transactions do not block these reads. Background monitoring and controllers keep the fleet snapshot's report window.

`q` matches node names, hostnames, and UUIDs. `state` indicates only the agent connection state: `online`, `offline`, or `connecting`;
accelerator problems do not change it. Sortable columns are `name`, `state`, `agent_version`, `gpu_count`, `cpu_cores`, `mem_total`, and `created_at`.

With pagination, node objects omit `topology` (topology) and `path_usage` (path usage); see `GET /api/v1/nodes/{uuid}` for details.
Paginated responses also include two fields unaffected by filters: `all`, the total number of nodes, and `labels`, user labels across all nodes, in the form `[{"key", "values"}]`.

Filter by labels using `label=key=value`, which can be repeated. Multiple values for the same key use OR; different keys use AND,
following the same matching rules as deployment placement conditions.
Keys starting with `gpuworks.ai/` match system labels, such as `gpuworks.ai/vendor=nvidia` and `gpuworks.ai/gpu-model=A100`;
other keys match user labels. Unknown system keys or invalid values return `400 LIST_FILTER_INVALID`.

`GET /api/v1/nodes/facets` returns **filter options and corresponding node counts** and accepts the same `q`, `state`, and `label` parameters.
Deployment forms and placement condition editors can use it directly to generate options and counts without first reading all nodes. The response includes:

| Content | Meaning |
|---|---|
| Matching node count | Total number of nodes that satisfy the current filters |
| User label values and system label values | Each value includes the number of nodes carrying it; system labels include server-provided display text and exclude the node's own `gpuworks.ai/node` label |
| GPU model | `vendor`, `name`, and `cc` describe the vendor, name, and compute capability; reported `arch_id` is included and distinct architectures are counted separately; `model` corresponds to the value of the `gpuworks.ai/gpu-model` system label |
| Count and GPU memory for each GPU model | Node count, plus `cards` (card count) and `memory_bytes` (maximum GPU memory per card) |

### Dashboard Summary Response Fields

Every count in `GET /api/v1/dashboard/summary` is computed by the server; the dashboard only displays it. Besides the node counts and capacity groups, the main fields are:

| Field | Meaning |
|---|---|
| `current_metrics.vram_used_bytes`, `current_metrics.vram_total_bytes` | VRAM used and total of the online nodes' own cards, summed only over cards with a sample of both in the last two minutes, so their ratio is the VRAM utilization; both are `null` when no card has both |
| `deployments.running`, `warming`, `sleeping`, `pending`, `other` | Deployments bucketed by effective state; each deployment is in exactly one bucket and they add up to `total`. `warming` is pulling an image or starting, `sleeping` is asleep or going to sleep as a whole, `pending` is waiting for a placement, and `other` is every deployment in none of these (stopped, failed, offline and the like) |
| `cards` | The same card state counts as the `summary` of `GET /api/v1/scheduling/overview`, plus `allocated`; `null` when the scheduling overview cannot be built, meaning unknown rather than no cards |
| `cards.allocated` | Cards held by deployments, i.e. `awake + asleep`: a sleeping deployment's cards stay reserved for its wake and count as allocated; cards held outside the platform, unhealthy or offline do not. `allocated / cards` is the allocation rate |
| `tasks.batches` | Batches the caller may see (every user's for an administrator, otherwise the caller's own): `active` are in progress or finalizing, `waiting` are validating their input; `null` when the Batch API is not served or the count failed |
| `tasks.benchmarks` | Evaluation runs pending or running, of every user (evaluation runs are not owned per user) |

### Node Connection Diagnostics

Administrators manually trigger `POST /api/v1/nodes/{uuid}/connection-diagnostics` with an empty body or `{}`. Custom URLs, SSH parameters and other fields are refused. Runtime observations use the named node's existing session; SSH uses only its saved identity and address. No recovery, address/token/installation changes or Agent restart/reinstall occur. SSH retains the existing host-key policy.

| Field | Meaning |
|---|---|
| `node_uuid`, `checked_at`, `connect_mode`, `saved_server_url` | Node, report time, direction and safe saved URL; not a process configuration readback |
| `last_seen_at`, `recovery_phase`, `recovery_attempts`, `last_probe_at`, `last_recovery_error`, optional `last_recovery_code` (the last error's code; older records have none), optional `last_reject_reason`/`last_reject_at` | Existing reporting/recovery and controlled rejection history, not recovery performed by this request; unknown rejection reasons are not echoed |
| `exposure` | `null` or `{url,state,observed_at}`: the connector's last record and original time, not a new liveness check |
| `checks` | `agent_control` describes this RPC, offline/old-agent or failure result; optional `ssh`, `configuration`, `node_http`. Each has `stage`, `outcome`, `vantage`, and only when observed: `target`, `source`, `observed_at`, `http_status` |
| `agent_report?` | A sanitized existing `DoctorReport`, retaining only whitelisted primitive `connection.*` facts. `report` is empty; Findings, typed evidence and other original content are not forwarded |
| `guidance` | Localized guidance distinguishing runtime observations, SSH snapshots, HTTP scope, local CLI and manual URL synchronization |

An online Agent advertising `node.connection_diagnostics` returns the receiving connection's own snapshot over an independent lightweight RPC: `connection.source=agent_runtime`, actual Server URL, direction, current ctrl/data, observed transport stages with durations/original times, and available proxy facts. The Server waits at most 15 seconds; the Agent enforces its own 10-second budget. Cancelling the HTTP wait does not remotely cancel execution. No new connection or full `node.doctor` is started. A switch to an old session after capability checking cannot invoke hardware diagnosis; late replaced-session replies are discarded. After an RPC attempt, SSH is not run serially afterward.

`agent_control.outcome=runtime_snapshot` establishes this ctrl round trip and sanitized snapshot, not file-transfer or inference health. Data `attached` is a connection observation; `detached` waits for reconnection without automatic ctrl fallback. `disabled`, `not_negotiated`, `pending` and `unknown` are distinct; missing facts are never filled in as successful. Online RPC does not manufacture CLI-only log history.

SSH with `vantage=control_plane` describes control-plane login. HTTP with `vantage=node` is an SSH-environment request to the saved URL's health endpoint, possibly with a different proxy environment. `configuration.source=state_file|legacy_arguments` identifies the snapshot source, not effective configuration after flags/env overrides. A `server_dial` node does not call the control-plane URL; that check is not applicable.

Offline, old-agent, cancelled, timed-out or invalid-response cases can still return HTTP 200 partial reports; inspect each `outcome`. SSH can be an available alternative; otherwise run `gpuworks-agent doctor --connectivity --state <actual-directory>` through the node console. HTTP health does not establish WS/authentication/SSE health. Address differences prompt verification, never an automatic address change.

Invalid input returns 400 `NODE_CONNECTION_DIAGNOSTICS_INPUT`; failure to read node settings returns 500 `NODE_CONNECTION_SETTINGS_UNAVAILABLE` without the DB error; an unknown node returns 404.

The report excludes SSH passwords, private keys, node tokens, complete configuration, process arguments and environment variables. This POST is not wired into the AI assistant's existing read-only GET tool; appearing in the API catalog does not mean the assistant can execute this diagnosis.

### Connection Parameters for Node Creation

`POST /api/v1/nodes` creates a node awaiting connection and returns the installation command and token. The connection direction determines address field requirements:

- **`agent_dial` (the default for `connect_mode`)**: The request body must include `server_url`, the address the agent dials; the UI prefills it with the current browser address.
  If missing, returns 400 `SERVER_URL_REQUIRED`. It must be an absolute http(s) address without credentials, query, or fragment; otherwise, returns 400 `SERVER_URL_INVALID`.
- **`server_dial`**: The agent never connects to the server, so `server_url` can be omitted and is not saved if provided; `server_url` in the response is empty.
  `install_command` returns a command for the [offline package](../how-to/connect-nodes.md#离线包): `sh gpuworks-agent.run --token '…' --connect-mode server_dial --listen '…'`.
- **Listen address**: `server_dial_listen` defaults to `0.0.0.0:18800`; it must be a valid `host:port` / `:port` with a port between 1 and 65535; otherwise, returns 400 `SERVER_DIAL_LISTEN_INVALID` (`address`).

**Enrollment limits**: the server can register node enrollment guards (for extension modules, such as a site's node cap). When a guard refuses a new node, `POST /api/v1/nodes`, `POST /api/v1/nodes/install`, `POST /api/v1/providers/{id}/install` and `POST /api/v1/providers/{id}/instances` answer 403 with the guard's `code` and `params` (`NODE_ENROLLMENT_REFUSED` when the guard gives no code) and create no node; SSH installs, cloud instance installs and instance rentals check before they start. A refused Kubernetes agent gets 403 `KUBE_ENROLL_REFUSED` from `/api/v1/kube/enroll` (`params.reason: enrollment_refused`, `params.refusal_code` the guard's code, the guard's other params alongside), which the agent treats as final; a refused Slurm agent gets 403 `SLURM_ENROLL_REFUSED` from `/api/v1/slurm/enroll` with the same `params`. A node that enrolled before is never refused when it enrolls again. An `apply` of a cluster entry no node has joined through yet checks first as well. With no guard registered these endpoints behave as before.

### SSH Checks and Installation

`POST /api/v1/nodes/precheck`, `POST /api/v1/nodes/install`, and `PUT /api/v1/nodes/{uuid}/ssh` share these SSH authentication fields:

| Field/value | Meaning |
|---|---|
| `auth_mode: "platform_key"` | Attempts only the server's platform key; no password or private key needs to be submitted |
| `auth_mode: "private_key"` | Attempts only the private key in `private_key_pem` |
| `auth_mode: "password"` | Attempts only the SSH `password` protocol, using the password in `password` |
| `auth_mode: "keyboard_interactive"` | Attempts only the SSH `keyboard-interactive` protocol, reusing `password`; supports one non-echoing password response and a bounded number of informational rounds with no questions, but not multiple questions, repeated password prompts, or MFA |
| `auth_mode` omitted or empty | Tries available credentials in order: platform key, user private key, user password, cloud instance password, and finally keyboard-interactive with the request's or stored `password`; unavailable credentials are skipped |

An explicit mode does not fall back to another protocol. If authentication methods have no overlap, the error identifies the configured methods and suggests checking the target SSH configuration or selecting keyboard-interactive. Network and host-key errors retain their existing classifications.

After successful installation or when saving credentials, the server still attempts to authorize the platform public key. Success clears the password/private key; only failed authorization retains the credential used. The `auth_mode` returned by `GET /api/v1/nodes/{uuid}/ssh` identifies the stored credential type, not the protocol selected for that request: a retained password gives `password`, never `keyboard_interactive`; empty secrets give `platform_key`, and a private key gives `private_key`. The response contains no plaintext password or private key; `has_password` / `has_private_key` indicates whether each is stored. Subsequent recovery uses the automatic order above.

Every check returned by the precheck, the SSH install (`checks`), the cloud install (`checks`, `precheck_details`, `install_details`) and saving SSH credentials (`platform_key`) has the same shape `{id, status, code?, params?, message?, evidence?}`: `id` is the lower_snake check identifier (such as `ssh_auth`, `install_dir_space`, `server_reachability`); `status` is only `pass`/`warn`/`fail`/`skip`/`na`, where `skip` means the check should have run but did not (a probe timeout, a missing prerequisite) and `na` means it does not apply to this host, and neither is a pass; `code` is the UPPER_SNAKE outcome code and `params` its interpolation parameters, which the client renders as `apiError.<code>`, `message` being only the English fallback; `evidence` is what a remote command, SSH or the cloud provider API returned, never the explanation. A `pass` without a `code` only reports what it observed (in `evidence`). The cloud install's `checks` are always `precheck` (the most serious status among the precheck items, with the ids of those needing attention in `params.checks`), `install_agent`, and `agent_dial` or `server_dial`.

`POST /api/v1/nodes/precheck` checks the host environment. Connectivity checks are returned according to the connection mode and detected results and may include:

- `server_reachability`: Whether the node can access the server.
- `server_to_agent`: Whether the server can dial `host_address` (defaults to the SSH host). If the address includes a port, that port is used; otherwise, the port from `server_dial_listen` is used (default `0.0.0.0:18800`). In `server_dial` mode, an unreachable address is `fail` rather than `warn`.
- `connectivity`: Connectivity summary. See [No Network Connectivity Between the Node and Server](../how-to/connect-nodes.md#节点与-server-网络不通) for troubleshooting.

The last two checks are not displayed when `agent_dial` can reach the server.

On both Linux and macOS, `POST /api/v1/nodes/install` uploads the agent and `install.sh`, then runs the script.
The requirements and error codes for `server_url` and `server_dial_listen` are the same as for [node creation](#创建节点的连接参数). On Linux, execution modes are:

- Register `gpuworks-agent.service` by default.
- `no_systemd: true` uses nohup, does not modify existing services or agents in other directories, and does not reject installation merely because the host is already connected.
  Only an agent already running in the installation directory causes 409 `NODE_INSTALL_EXISTING_AGENT_SAME_DIR` (`dir`); after confirming replacement, retry with `replace_existing_agent`.
  When a directory above the installation directory is closed to the dedicated runtime user `udocker` and the probe establishes that the node will run a root agent with runveil and that user, it answers 400 `NODE_INSTALL_DIR_UNTRAVERSABLE` (`dir`, `mode`, `install_dir`, `fix`) and uploads nothing; `POST /api/v1/providers/{id}/install` answers the same. The precheck request may carry `runtime_driver` so that its row matches the install.

macOS ignores `no_systemd`.

When `install_dir` is omitted, `/opt/gpuworks` is used, falling back to `/tmp/gpuworks` if the default directory is not writable. If an explicitly specified directory is not writable, installation fails without a fallback.

### Node Scheduling and Accelerator Switching

The three values for `PUT /api/v1/nodes/{uuid}/scheduling` have the following effects:

- `cordoned`: No new replicas start; existing replicas continue serving. The candidate pool lists the node separately as `cordoned`; manually placed multi-node or PD ranks assigned to it wait with the reason `node_cordoned`, while automatically placed ones move to other nodes when the replica is about to start (`UNIT_PLACED`).
- `draining`: Also migrates the replicas on this node one at a time. A replacement starts on another node first; once it is running, the original replica drains. This proceeds one replica at a time per deployment. Multi-node and PD replicas move the same way, replacement first (the replacement node has the same compute network, the same GPU architecture, and free cards; the placement is rewritten only once it runs, recorded in the timeline as `NODE_DRAIN_MOVED`), and stay in place waiting if there is no node to go to. Single-node deployments pinned to this node do not move.
- `schedulable`: Resumes scheduling; waiting replicas retry immediately.

A node's `scheduling_by` says who set its current state: `operator` (set through this endpoint) or `platform` (the platform stopped scheduling on its own); a schedulable node, or one stopped before an upgrade, has no such field.
The platform lifts only the stops it set. When `scheduling_by` is `platform`, `cordon_reason` gives the current reason: `runtime_gpu_blocked` (the runtime cannot give containers their accelerators), `card_needs_host_reboot` (a card needs a host reboot or power cycle to recover), `kube_unschedulable` (Kubernetes has the node cordoned; the platform lifts its stop once Kubernetes uncordons it), or `slurm_node_unavailable` (Slurm will not start jobs on the node, see below; the platform lifts its stop once Slurm runs jobs there again).
A node that borrows its cards from Slurm ([Connect a Slurm Cluster](../how-to/connect-slurm-nodes.md)) carries `slurm_node`, its name in Slurm. When Slurm will not start jobs on it (the placeholder jobs would queue forever) it also carries `slurm_unavailable` (`gres_missing`: slurmd registered fewer GPUs than the Slurm manager node configures, usually because there is no `gres.conf`; `invalid_registration`: the registration was refused; `down`: down or not responding; `drained`: drained or draining) and `slurm_reason` (Slurm's own Reason text, for display only), and `cordon_reason` is `slurm_node_unavailable`; it also carries `slurm_fixes`, the steps the server gives for that state, in order, each `{code, commands}`: `code` is `gres_conf` (add a line to `gres.conf` on the Slurm manager node; the command is the line the server composed from the node's card count, for example `NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]`, present only for NVIDIA cards whose count is known), `reconfigure`, `resume`, `restart_slurmd`, `resume_drained` or `check_slurmd`; a client shows its own text for each `code` and the `commands` as written. The cluster entry's "check and apply" can also add the `gres.conf` line on the Slurm manager node (see the Slurm section below). Once Slurm runs jobs there again these fields disappear with the next report and the platform lifts the stop it set.

After the platform stops scheduling, setting `schedulable` through this endpoint takes effect at once; while the reason remains, the next report stops it again.
An extension module can also hold a node out of scheduling (an extension-held cordon): the node's next report (or the extension applying it at once) sets it `cordoned` with `scheduling_by` naming the extension's source; it is never set `draining`, and replicas already there keep serving. An extension-held cordon outranks the platform's and yields to an operator's: the platform's reason clearing does not lift it; while it is held, setting `schedulable` answers 409 with the extension's `code` and `params` (`NODE_SCHEDULING_HELD` when it gives no code), and setting `cordoned` or `draining` works as usual. Once the extension lets go, the node falls back to the platform's stop while the platform's reason remains, and is schedulable otherwise.
A node's `uncordon` says what setting `schedulable` does now, as the server decides it: `allowed`; `confirm` (the platform has a reason to stop scheduling and its next report stops it again); `refused` (an extension holds the node, and `cordon_message` is the extension's `{code, params, message}`).

`PATCH /api/v1/nodes/{uuid}/runtime` sets the node's container runtime preference `{runtime: auto|docker|nerdctl|udocker}`; it is saved after the agent accepts it. The node's `runtime_choices` lists the values this node may be set to, in display order: a Mac has only `auto`, every other node all four; **a Kubernetes node has no such field** (its instances are pods of the cluster, so there is no runtime to choose), and setting one returns 409 `NODE_RUNTIME_KUBE_NODE`. An invalid value returns 400 `NODE_RUNTIME_INVALID`.

`GET /api/v1/nodes/{uuid}` carries `gpu_isolation` for a Kubernetes node whose NVIDIA instance pods are held to their own card by `CUDA_VISIBLE_DEVICES` alone (soft isolation: the pods can see every GPU device of the node): `level` (`soft`), `reason` (`cdi_names_missing`: the toolkit resolves `NVIDIA_VISIBLE_DEVICES` as CDI names and no CDI spec names the card), `toolkit` (`gpu_operator` or `host`, when the agent can tell), `cards`, and `fixes`: each has a `code` and `commands` to run in order, exactly as written, chosen by the server from `toolkit`. It comes from the agent's latest status report and is not stored: it is absent while the node is offline, strictly isolated, or runs an older agent, and the node list does not carry it.

`GET /api/v1/nodes/{uuid}` carries `ucx_netlink_hang` for a node whose kernel hangs UCX (bundled with NIXL, which PD's `vllm-nixl` and `sglang-nixl` move the KV over): `kernel` (the kernel release, omitted when it cannot be read) and `fixes`, each with a `code` (`disable_ipv6_now`, `disable_ipv6_persist`) and `commands` to run in order, as written (turn IPv6 off for the whole host; the first lasts until the next reboot, the second writes `/etc/sysctl.d/90-gpuworks-ucx.conf` to keep it). It comes from the agent's latest status report (checked again every 15 minutes) and is never stored: a node that is offline, unaffected or runs an older agent has no such field; the node list does not carry it.

`PATCH /api/v1/nodes/{uuid}/accelerator` selects which vendor's cards to manage on a host with multiple vendors. The selection is saved only after the agent accepts it and restarts in place.
The returned node's `accelerator_preference` indicates the selection; `accelerator_vendors` lists `{vendor, cards, model}` for each vendor on the host.

- Invalid value: 400 `NODE_ACCELERATOR_INVALID`; the host has no cards from that vendor: 400 `NODE_ACCELERATOR_NOT_ON_HOST`.
- The node has instances that are not stopped: 409 `NODE_ACCELERATOR_HAS_INSTANCES` (`params.count`); agent rejection or an offline agent also returns 409.
- The node does not exist: 404.
- When AMD platform capabilities or runtime requirements are not met, evaluation/creation returns a structured refusal. `ACCELERATOR_FEATURE_UNSUPPORTED` includes `vendor` and `feature`. Final AMD placement without an assigned device returns `ACCELERATOR_DEVICE_SELECTION_REQUIRED`; automatic placement does not return it before device selection is complete.

### Node Labels: Written and Generated

Nodes have two types of labels, represented by two DTO fields:

- `labels`: Written by operations staff (`PUT /api/v1/nodes/{uuid}/labels`), with custom keys and values.
- `system_labels`: Read-only labels computed by the platform from facts about the node itself. They are not stored in the database and are recomputed on every read, so card replacements, network changes,
  and network renames are reflected immediately.

Generated keys are all under the reserved `gpuworks.ai/` namespace. Written labels must not use that namespace or its subdomains
(`rack.gpuworks.ai/...`), so the key identifies whether a value is a platform fact or human-authored. Labels used to attach networks follow the same rule.
When upgrading to the version that introduces this rule, migration 119 deletes existing keys of this type from nodes (case-insensitively), along with networks
attached using those keys. Label conditions in deployments that reference these keys are left unchanged, because removing a condition relaxes the constraints. Such deployments
are not scheduled to any node until someone modifies them.

| Key | Value | Description |
|---|---|---|
| `gpuworks.ai/node` | Node UUID | `display` is the node name |
| `gpuworks.ai/vendor` | Canonical vendor ID | One per host |
| `gpuworks.ai/cpu-arch` | `amd64` / `arm64` | |
| `gpuworks.ai/gpu-model` | Normalized model name | One entry per model; nodes with mixed cards have multiple entries |
| `gpuworks.ai/gpu-arch` | Architecture generation within the vendor (`turing`, `910b`, etc.) | Omitted for unrecognized cards |
| `gpuworks.ai/compute-network` | Network ID; `management` when the management network handles the role | Omitted if the role has no network |
| `gpuworks.ai/storage-sync-network` | Same as above | |
| `gpuworks.ai/serving-network` | Same as above | |
| `gpuworks.ai/kubernetes-cluster` | Cluster UUID | Only nodes connected from a Kubernetes cluster; `display` is the cluster name, and a rename does not affect matching |
| `gpuworks.ai/kubernetes-node` | Node name in Kubernetes | Same as above; `display` is the same value |
| `gpuworks.ai/slurm-cluster` | UUID of the Slurm cluster entry | Only nodes connected from a Slurm cluster entry (a node whose agent was installed by hand and merely borrows cards from Slurm has none); `display` is the entry name, and a rename does not affect matching |
| `gpuworks.ai/slurm-node` | Node name in Slurm | Same as above; `display` is the same value |

The same key can have multiple entries (the values form a set). Each entry is `{key, value, display, display_code}`: `value` is the literal value
used for matching, `display` is display text, and `display_code` is provided only when the value is platform terminology (vendor or management network), referring to
an existing client translation. Clients prefer `display_code` and fall back to `display` if it is unavailable. Network and node values are IDs rather than names,
so renaming changes only `display` and does not affect label-based matching.

## Kubernetes (Cluster Connection)

A Kubernetes cluster joins as one DaemonSet: an administrator creates the cluster and runs the returned command with `kubectl apply` in the cluster; the agent on every selected GPU node exchanges the cluster's connect token for its own node token, and from then on behaves like an ordinary node.

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/kube/clusters` | List clusters: `{items:[{id,uuid,name,namespace,server_url,agent_image,agent_image_source,effective_agent_image,vendors,node_selector,runtime_class,image_pull_secret,containerd_socket,state_dir,api_server,engine_ports,idle_return_seconds,priority,allow_preemption,cluster_uid,stalled,revoked,state,category,resources,node_count,created_at,pool_ids,namespace_shared_with,can_apply,can_remove,operation_blocked?,apply_command?,remove_commands?,source?:{provider_id,provider_name,provider_kind,cluster_id}}], total}`; `apply_command` contains the connect token and is returned only to administrators, and **never when the AI assistant reads this endpoint**; an entry has no vendor: `vendors` counts the joined nodes by vendor, `[{vendor,nodes}]` (most nodes first; an empty `vendor` means nodes that reported no cards), each agent detecting its own node's vendor; `remove_commands` (administrators only, also for revoked entries) are the kubectl commands that remove the entry's agents from the cluster, to run in order: delete the DaemonSet (`kubectl -n <namespace> delete daemonset gpuworks-agent --ignore-not-found --wait`), the ClusterRoleBinding `gpuworks-agent-<namespace>`, the ClusterRole `gpuworks-agent-<namespace>`, the PriorityClass `gpuworks-inference-<namespace>`, and the namespace, all with `--ignore-not-found`, and none needs the connect token; `source` appears only for clusters connected from a cloud account's cluster; `namespace_shared_with` names the other entries that use the same namespace and may be of the same cluster (sorted; an entry no node has joined yet counts, entries known to be of different clusters do not); in one cluster the one applied later takes over the other's agents, and the console warns about it; `pool_ids` are the cloud node pool IDs a cloud entry uses (ascending, an empty array for a manual entry, kept after the cloud account is deleted); `cluster_uid` is empty until the first node joins; `idle_return_seconds` of -1 means borrowed cards are never returned; `agent_image_source` is the registry the entry pulls the published agent image from (`dockerhub`, `swr`, `quay`), and `effective_agent_image` is the image the manifest actually names (`agent_image` when that is set); `stalled` is computed by the server: no node has joined, the cluster is not revoked and it is older than `stalled_after_seconds` of `GET /kube/defaults`, so a client suggests troubleshooting; `state` is where the entry stands, the one value a client shows and filters by, decided in this order: `revoked` (the token is revoked), `stalled` (as above), `pending` (no node has joined yet), `active` (a node has joined); `category` is `kubernetes` and `resources` is `[{type:"kubernetes"}]`, the Kubernetes item of `categories` in `GET /providers/kinds`, which an entry connected on its own is listed under among the external platforms; `can_apply` and `can_remove` say whether the server can apply the manifest to the source cluster or delete it from there, and `operation_blocked` says why not: `no_source` (an entry made by hand, so the server has no way into the cluster), `private_api` (the cloud says the cluster's API server has no public address, so `kubectl apply` must run from inside the cluster's network; not decided while the cloud account cannot be asked, the operation itself then reports the failure), `revoked` (the token is revoked: only applying is refused, removing still works). The server keeps the cloud's cluster listing for 1 minute, so polling the list does not call the cloud API each time | JWT |
| GET | `/api/v1/kube/defaults` | The dictionary the cluster form needs: `{namespace, state_dir, engine_ports, idle_return_seconds, idle_return_keep (-1, the idle_return_seconds value that means never return), agent_image, stalled_after_seconds, agent_image_source, agent_image_sources:[{id, image}], priority, priority_max, allow_preemption}`; `priority` is the priority a create request that omits it takes (10000), from 0 to `priority_max`, and `allow_preemption` defaults to false; `agent_image` is the image in the default registry (`agent_image_source`, Docker Hub) at the current runtime revision, and `agent_image_sources` lists every registry the agent image is published to (the default first) with its image; `namespace`, `state_dir`, `engine_ports`, `idle_return_seconds` and `agent_image` are the values the server takes when a create request omits the field; a client shows them only as hints and leaves the field empty; the dictionary has no vendor (an entry has none; a RuntimeClass affects only NVIDIA instance Pods, and other vendors' devices come through the agent's own device plugin and ignore it) | JWT |
| POST | `/api/v1/kube/clusters` | Create a cluster: `{name, server_url, namespace?(gpuworks-system), agent_image?(your own image; left empty, nothing is stored and the manifest is rendered with the image at the current runtime revision in the `agent_image_source` registry), agent_image_source?(dockerhub\|swr\|quay; when omitted, Huawei Cloud SWR for a new manual entry (persisted as swr; historical blank sources retain Docker Hub semantics), and for a cloud entry by the cluster's region: swr in Alibaba Cloud's mainland China regions, dockerhub elsewhere; other values return 400), node_selector?("k=v,k2=v2"), runtime_class?(affects only NVIDIA instance Pods; empty means each node decides: use the nvidia RuntimeClass if it exists, otherwise the default runtime), image_pull_secret?(name of a docker-registry Secret in the namespace used to pull the agent image; empty means none), containerd_socket?(empty means the agent finds it itself), state_dir?(/opt/gpuworks), api_server?("https://host:port"; empty means the cluster's kubernetes Service), engine_ports?(40000-42767), idle_return_seconds?(60), priority?(10000, 0 to 1000000000: the PriorityClass value of the hold and instance Pods), allow_preemption?(false: the PriorityClass says preemptionPolicy: Never), provider_id?, provider_cluster_id?, provider_pool_ids?(give provider_id and provider_cluster_id together to connect from this cluster of a cloud account; see GET /providers/{id}/clusters; `provider_pool_ids` is then required and names the cluster's GPU node pools the entry uses, one or several, of any vendor, and `node_selector` may no longer be given; a manual entry gives none of the three and keeps the free-form `node_selector`)}`; returns 201. Unusable parameters return 400 `KUBE_CLUSTER_INVALID` (`field`, `reason`; also for a node pool that does not exist or none chosen; an entry has no vendor, so node pools may be of any vendor), a duplicate name returns 409 `KUBE_CLUSTER_NAME_TAKEN`, and a source cluster that already has an entry returns 409 `KUBE_CLUSTER_SOURCE_TAKEN` (`cluster`; a cluster has one entry, so edit its node pools to use more of them). On creation the server checks the node pools against the cloud provider, and a cloud error returns 424 | JWT(admin) |
| PUT | `/api/v1/kube/clusters/{uuid}/pools` | Replace the entry's node pools: `{provider_pool_ids}`, with the same rules as creation (at least one, all of the entry's source cluster, otherwise 400 `KUBE_CLUSTER_INVALID`; the node pools may be of any vendor); returns the updated cluster (one item of the GET list, its fields still at the top level) plus `stopped_instances` (see below; an empty array when none). The manifest is rendered from the new node pools at once, but **the DaemonSet in the cluster changes only when the manifest is applied again** (`POST …/apply` or kubectl); agents on the pools taken away exit and their nodes go offline; after 15 minutes offline, the server asks another online agent of the same entry whether the node still exists and still runs one of the entry's agent Pods, and when not, and the node has no instances, the node record is removed automatically (when the entry has no other online node nothing is asked; delete the node by hand). The body may carry `confirm_drop_instances` (boolean, default false): when taking node pools away leaves out nodes that still run instances, an unconfirmed request returns 409 `KUBE_SELECTION_DROPS_INSTANCES` (`nodes`: each with `node_uuid`, `node_name`, `kube_node`, `deployments`; `verified`: false means no online agent could be asked and the nodes listed are all those that run instances, which only may be affected), and it is saved only when resent with `true`; nothing is asked when the node pools do not change. When resent with `true`, the server first recomputes the nodes that leave and asks the instances on them that have not finished to stop (in the same transaction as the save, so a failed validation or a duplicate name stops nothing; state changed to `removing`; replicas of the same deployment on other nodes are untouched; it does not wait for the agents; when it has established that the nodes leave it also cordons them, a node already cordoned or draining keeps its state, and with `verified` false only the instances are stopped and nothing is cordoned), then saves; `stopped_instances` lists the instances asked to stop: `instance_uuid`, `deployment` (the deployment name), `node_uuid`, and is an empty array when no node leaves An entry made by hand, or one whose cloud account is deleted, returns 409 `KUBE_CLUSTER_NO_SOURCE`; for a cloud entry made earlier with a free-form node selector, editing its node pools makes it select by the pools alone (the node selector is cleared) | JWT(admin) |
| PUT | `/api/v1/kube/clusters/{uuid}` | Edit the entry: every body field is optional, omitted means keep and an empty string clears: `name, node_selector, agent_image, agent_image_source, image_pull_secret, runtime_class, containerd_socket, api_server, engine_ports, idle_return_seconds, priority, allow_preemption`, and `confirm_drop_instances` (boolean, default false, not a setting of the entry). Unknown fields return 400; `namespace`, `server_url`, and `state_dir` are fixed at creation and cannot change; a cloud entry selects nodes by node pool, and changing its `node_selector` returns 400 `KUBE_CLUSTER_INVALID` (change node pools with `PUT …/pools`). When a changed `node_selector` leaves out nodes that still run instances, the server asks one online agent of the entry to read those nodes and decide, and an unconfirmed request returns 409 `KUBE_SELECTION_DROPS_INSTANCES` (parameters as for `PUT …/pools`); it is saved only when resent with `confirm_drop_instances: true`, the server first asking the instances on the leaving nodes to stop (as for `PUT …/pools`), and nothing is asked and no instance is stopped when the selection does not change. Validation is as on creation: 400 `KUBE_CLUSTER_INVALID` (`field`, `reason`), a duplicate name returns 409 `KUBE_CLUSTER_NAME_TAKEN`, and an unknown uuid returns 404. Returns `{cluster, changed, reapply, commands, stopped_instances, warnings}`: `cluster` is one item of the GET list, `changed` is the JSON names of the fields that changed (sorted), and `reapply` true means the manifest changed and the cluster keeps running the old one until it is applied again; `commands` (administrators only, for an entry made by hand and not revoked) is one kubectl line: `kubectl delete priorityclass gpuworks-inference-<namespace> --ignore-not-found && <apply command>` (Kubernetes cannot change a PriorityClass's value or `preemptionPolicy` in place, and an earlier priority change may not have been applied yet); empty when the manifest did not change; `stopped_instances` are the instances asked to stop after confirmation (`instance_uuid`, `deployment`, `node_uuid`), an empty array when none; `warnings` is what the edit allowed but you should know (each with `code` and `params`, an empty array when none): after changing `engine_ports`, when running instances hold ports outside the new range, one `KUBE_ENGINE_PORTS_OUTSIDE` is carried (`range`: the new range; `instances`: each with `instance_uuid`, `deployment`, and the `ports` outside the range), saving succeeds as usual and those instances keep their ports. A cloud entry is applied with `POST …/apply`, and the server deletes and recreates a changed PriorityClass itself. `runtime_class`, `engine_ports`, and `idle_return_seconds` are not in the manifest: after saving, the server sends them to the entry's online agents at once (offline ones get them when they reconnect), with no apply and no agent restart, so when only these three change `reapply` is false and `commands` is empty; the new values apply to instance Pods started afterwards, instance ports allocated afterwards, and later idle-return decisions, and running instances are unchanged (including those whose ports fall outside the new range, see `warnings`) | JWT(admin) |
| GET | `/api/v1/kube/clusters/{uuid}/manifest` | The cluster's manifest (`application/yaml`) | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/revoke` | Revoke the connect token: nodes already connected are unaffected, and new nodes can no longer join | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/apply` | Apply the cluster's manifest to its source cluster: `{dry_run}` (required; if omitted, returns 400 `KUBE_CLUSTER_INVALID` and never executes by default). The server obtains a short-lived kubeconfig from the cloud account on the spot (not stored; ADR 0063) and server-side applies the objects one by one; with `dry_run`, it only has the API server check them; when a PriorityClass's value or `preemptionPolicy` changed, which Kubernetes does not allow in place, the server deletes and recreates it and the `action` of that item is `replace`. Returns `{dry_run, results:[{kind,name,namespace?,action(create\|update\|replace\|failed\|not_tried\|dependent),error?}]}`. If the cloud account cannot issue credentials or the cluster refuses, returns 424 with the same `results` in the body, plus `code: KUBE_CLUSTER_OPERATION_FAILED` and `params:{step(kubeconfig\|api_server),reason}`. An entry created by hand, or whose cloud account was deleted, returns 409 `KUBE_CLUSTER_NO_SOURCE` | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/remove` | Delete the manifest's objects from the source cluster (the DaemonSet first, then the rest in reverse order; cluster-wide objects are named for the namespace, so other entries of the cluster are untouched; `action` additionally has `delete\|absent`); request, response, and errors are the same as apply | JWT(admin) |
| DELETE | `/api/v1/kube/clusters/{uuid}` | Delete the cluster, returning 204; returns 409 `KUBE_CLUSTER_HAS_NODES` while it still has nodes | JWT(admin) |
| GET | `/kube/manifest.yaml?token=` | Fetch the manifest with the connect token, for `kubectl apply -f`; an unknown or revoked token returns 404 | Connect token |
| POST | `/api/v1/kube/enroll` | The agent exchanges `{token, cluster_uid, node_name}` for a node token: returns `{node_uuid, node_token, idle_return_seconds}`; the same node of the same cluster always gets the same node. The token binds to a cluster UID on first use, after which another cluster gets 403 `KUBE_ENROLL_REFUSED` (`params.reason: other_cluster`); an unknown or revoked token returns 403 (`params.reason: unknown_token`; an agent that has enrolled before keeps its saved identity); a Kubernetes node belongs to only one entry at a time: when another entry of the same cluster selects the same node and that entry's agent on it is online or was seen in the last 15 minutes, it returns 403 (`params.reason: node_taken`, `params.entry` the name of the entry holding the node), after which the node can move to the other entry | Connect token |

## Slurm (Cluster Connection)

A Slurm cluster joins through one health check: an administrator creates the cluster entry, runs the command it returns as root on the Slurm manager node (which puts a `HealthCheckProgram` in slurm.conf), the slurmd of every GPU compute node runs the check script, the script installs the agent on that node, and the agent trades the entry's enrollment token for a node token of its own and is an ordinary node from then on. Nodes added to the cluster later join the same way.

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/api/v1/slurm/clusters` | List entries: `{items:[{id,uuid,name,server_url,slurm_cluster_name,run_as,account_user,install_dir,script_dir,sbatch_args,idle_return_seconds,pending_timeout_seconds,revoked,state,stalled,node_count,online_count,slurm_gpu_nodes,list_as_of?,missing_count,missing_nodes,waiting?,shared_storage_hint?,category,resources,source?,can_apply,can_remove,operation_blocked?,delete_with_nodes_allowed,delete_with_nodes_blocker?,created_at,setup_command?,check_command?,remove_command?,uninstall_agents_command?}], total}`. The commands carry the enrollment token and are for administrators only; **the AI assistant never gets them from this endpoint**, and a revoked entry has no `setup_command` or `check_command` (the latter is the same command with `-s -- --check`: it only checks and changes nothing). `state` is as for Kubernetes: `revoked`, `stalled` (no node enrolled, not revoked, created more than `stalled_after_seconds` ago), `pending`, `active`. `slurm_cluster_name` is Slurm's own `ClusterName`, bound by the first node that enrolls and empty before. `node_count` is the nodes that enrolled, `online_count` those of them whose agent is online; `slurm_gpu_nodes` is the number of GPU nodes (`sinfo`) an enrolled agent last reported (0 before the first report), but never fewer than `node_count`: every node that enrolled has a GPU (agents are installed only on nodes with one) and a report can be up to a report interval old, so right after a node enrolls it may not be listed yet, and the entry never shows more nodes joined than GPU nodes; `missing_count` is how many of them have no agent online and `shared_storage_hint` suggests registering the cluster's shared filesystem as model storage, `{name,type,path,label_key,label_value}` being what the console fills the add-storage form with (`type` is `shared_dir`, applied to the entry's nodes by the `gpuworks.ai/slurm-cluster` label); it is given only for an entry made from an Alibaba Cloud E-HPC cluster that is not revoked, has joined nodes, and none of whose nodes has an enabled shared storage (`nfs`, `nfs_cache`, `shared_dir`) yet, and the directory is `/opt/gpuworks-models`, under the `/opt` E-HPC mounts from the cluster's NAS on every node; `list_as_of` (RFC 3339) appears only while the entry has enrolled nodes, none of whose agents is online, and its list is not empty: only agents report the list, so it is the one from before they went, at most as new as when an agent was last seen, which this is; `missing_nodes` the first `missing_nodes_limit` of them by name, `{name,reason,slurm_state,slurm_reason?,node_uuid?}` (`node_uuid` only for a node that enrolled and is offline; deleting it with `DELETE /api/v1/nodes/{uuid}` also takes it off the list, and the next report stores the list whole again, so a node still in the cluster is back on it): `reason` is `not_enrolled` (the node never enrolled: the health check has not run there yet, or the node has no NVIDIA GPU) or `offline` (enrolled, agent not connected); `slurm_state`/`slurm_reason` are Slurm's own words for the node. `waiting` is given only while no node has joined and the entry is not revoked, and is the server's reading of why there is none, `{reason, hosts?}`: `joining` (the setup was seen run less than two health-check intervals ago, 10 minutes: the health check reaches the nodes within one to one and a half intervals and half an interval more is allowed, so no node having contacted the server yet is normal, not a problem; the entry is not `stalled` meanwhile), `no_compute_nodes` (the cloud's cluster listing says it has no compute nodes, read live: the three `Nodes` counts of E-HPC `ListClusters` add up to 0), `server_unreachable` (the platform's last check or apply found hosts that cannot reach the entry's `server_url`; `hosts` are those, `{role, name, status, reason}`), `no_node_called` (the setup was seen run, because someone fetched the connect command's script or the platform applied it, and, that window having passed, no node has since asked the server for the token's status or enrolled) or `not_applied` (the setup was not seen run; fetching the script with the check command, `&check=1`, does not count). The last three are things the server saw itself, kept in memory only and not in the database; a restart forgets them, so for an entry created before this server process started none is claimed, and `waiting` is absent when nothing known says why. `category` is `slurm` and `resources` is `[{type:"slurm"}]`: the Slurm item of `categories` in `GET /providers/kinds`. `source` is the cloud account cluster the entry was made from, `{provider_id,provider_name,provider_kind,provider_cluster_id}`. `can_apply` true means the platform can run the setup on the Slurm manager node through the cloud account (the entry has a source and is not revoked, the account's driver can run commands on the Slurm manager node, and the cluster schedules with Slurm), and `can_remove` that it can take the setup out of the cluster again (the same, but a revoked entry also qualifies); when it cannot, `operation_blocked` is why: `no_source` (an entry made by hand), `no_provider` (the source account was deleted), `provider_unsupported` (the account's driver cannot run commands on the Slurm manager node), `cluster_unknown` (the last sync lists no node of the cluster and the account's cluster listing does not have it), `cluster_listing_pending` (the cluster listing is still being read in the background and there is no earlier one to use; the next poll has it), `cluster_listing_failed` (the cluster listing failed, the key may not list, or a region is missing, so the cluster cannot be said not to be in the account), `not_slurm_cluster` (the cluster schedules with something else) or `revoked` (the token is revoked; only applying is refused); a client shows "Check and apply" from these, disabled with the reason, and decides nothing itself; the cluster's region and scheduler come from its nodes in the last sync and, when it has none, from the cloud's cluster listing (the server reads it in the background per account, caches it for 1 minute, and a request waits at most 5 seconds before using the last listing or reporting `cluster_listing_pending`; polling the list neither calls the cloud API each time nor flickers between enabled and disabled when the API is slow; replacing the account's key or deleting the account drops it). `delete_with_nodes_allowed` says `DELETE …?with_offline_nodes=true` would succeed now; when it would not, `delete_with_nodes_blocker` is why: `online` (an agent is connected) or `instances` (a node still has instances) | JWT |
| GET | `/api/v1/slurm/defaults` | What the form needs: `{run_as, install_dir, script_dir, idle_return_seconds, idle_return_min_seconds, idle_return_max_seconds, pending_timeout_seconds, pending_timeout_min_seconds, pending_timeout_max_seconds, health_check_interval_seconds, stalled_after_seconds, report_interval_seconds, missing_nodes_limit, quick_tunnel_url}`. `report_interval_seconds` is how often an agent reports and fetches its settings, that is, the longest an edit takes to reach the nodes. `quick_tunnel_url` is the platform's own temporary public address right now (the address of the Cloudflare quick tunnel of `gpuworks expose` while it is on and up), an empty string when there is none: an entry's `server_url` must not be it (it changes when the connector is rebuilt), so the form warns about it and does not block it | JWT |
| POST | `/api/v1/slurm/clusters` | Create an entry: `{name, server_url, run_as?(root), account_user?, install_dir?(/var/lib/gpuworks), script_dir?(/opt/gpuworks-slurm), sbatch_args?, idle_return_seconds?(60), pending_timeout_seconds?(600), provider_id?, provider_cluster_id?}`; `provider_*` go together (a cloud account's HPC cluster; one entry per cluster, else 409 `SLURM_CLUSTER_SOURCE_TAKEN`). An unusable parameter is 400 `SLURM_CLUSTER_INVALID` (`params.field`, `params.reason`), a duplicate name 409 `SLURM_CLUSTER_NAME_TAKEN`. Returns 201 and one list item (with the three commands) | JWT(admin) |
| PUT | `/api/v1/slurm/clusters/{uuid}` | Edit an entry: every field is optional, omitted means unchanged, an empty string clears: `name, account_user, sbatch_args, idle_return_seconds, pending_timeout_seconds`. `server_url`, `run_as`, `install_dir` and `script_dir` are written into the scripts on the nodes and cannot be edited; sending them, or an unknown field, is 400. Returns `{cluster, changed}` with the JSON names of the changed fields (ascending). Agents pick the new settings up on their next report (at most `report_interval_seconds`); nothing is re-run on the cluster | JWT(admin) |
| GET | `/api/v1/slurm/clusters/{uuid}/setup` | The commands an administrator runs on the cluster: `{setup_command?, check_command?, remove_command, uninstall_agents_command, setup_url?, check_script_url?, default_interval_seconds}`, each as root on the Slurm manager node; a revoked entry has no `setup_command`, `check_command`, `setup_url` or `check_script_url`. Uninstalling takes two steps: run `uninstall_agents_command`, wait one health-check interval (the agent on each node uninstalls itself at its next check), then run `remove_command` (restores slurm.conf and runs `scontrol reconfigure`) | JWT(admin) |
| GET | `/api/v1/slurm/clusters/{uuid}/script` | The console's "View script": the full text of the setup script the Slurm manager node is served (`text/x-shellscript`, check script included), differing from `GET /slurm/setup.sh` only by the enrollment token, which is always shown as `REDACTED-ENROLLMENT-TOKEN` here; a revoked entry has it too, an unknown entry is 404; the AI assistant cannot read it | JWT(admin) |
| POST | `/api/v1/slurm/clusters/{uuid}/revoke` | Revoke the enrollment token: nodes that already enrolled are unaffected and new nodes cannot enroll; `remove_command` and the two scripts stay available so the entry can be uninstalled. Returns one list item | JWT(admin) |
| DELETE | `/api/v1/slurm/clusters/{uuid}` | Delete an entry, 204; 409 `SLURM_CLUSTER_HAS_NODES` while it has nodes (deleting those nodes on the Nodes page needs no SSH). After the agents were uninstalled, `?with_offline_nodes=true` deletes the entry together with its nodes (the entry is revoked first, then the nodes are deleted, then the entry, so the health check cannot enroll a node again meanwhile; if a step after the revocation fails the entry stays revoked), only when no node is online and none has instances; otherwise 409 `SLURM_CLUSTER_NODES_ONLINE` (`count`) or `SLURM_CLUSTER_NODES_HAVE_INSTANCES` (`count`) and nothing is deleted; instance rows only waiting to be removed go with their node; a `with_offline_nodes` that is not a single boolean is 400 `SLURM_CLUSTER_INVALID` | JWT(admin) |
| POST | `/api/v1/slurm/clusters/{uuid}/apply` | The platform runs the setup on the cluster's Slurm manager node (Alibaba Cloud E-HPC through Cloud Assistant; the Slurm manager node's instance ID comes from `ehpc:GetCluster`), for entries with `can_apply` true; the request body is `{"dry_run": bool}` and `dry_run` is required (missing, not a boolean or not a single JSON object answers 400, a missing one `SLURM_CLUSTER_INVALID` with `params.field` `dry_run`), so applying is never taken by omission: `true` only checks (see below) and `false` applies; repeatable (compute nodes scaled out since get their `gres.conf` line on the next run). First every node Slurm itself lists (`scontrol -o show node`) whose configured `Gres=` has `gpu:N` and that `gres.conf` has no gpu line for gets `NodeName=<n> Name=gpu File=/dev/nvidia0` (`/dev/nvidia[0-<N-1>]` for several cards, N being its `gpu:N`; typed and socket-suffixed spellings are read too; neither the nodes nor the counts depend on the instances the account synced, so a new cluster never synced and nodes whose names do not fit `<cluster>_<host>` are covered all the same); a node that already has a gpu line (including a `NodeName` expression that covers it, a `NodeName` anywhere in the line, and a general gpu line without `NodeName`) is never touched. Then the entry's setup script is fetched and run (writes the check script and the enrollment token file, distributes them to the nodes when needed, edits slurm.conf, `scontrol reconfigure`); finally nodes still drained for the missing GPUs get `state=resume`, and an administrator's own drain stays. One run at a time per Slurm manager node: a retry after the client gave up waiting may find the previous run still going on the Slurm manager node and gets 424 `SLURM_APPLY_FAILED` (`step: run`, `reason: busy`); wait a few minutes and retry. Waits up to 6.5 minutes; success returns `{status, manager_instance, steps:[{id,status}], gres:{added,present,no_gpu_gres}, distribute:{shared,ok,failed}, resumed_nodes, resume_failed_nodes, reach, cluster}`: `status` is `ok` or `partial` (finished, but something is left to do by hand, see the steps), `steps` are, in order, `gres`, `check_script`, `distribute`, `slurm_conf`, `reconfigure`, `resume` and, when hosts were asked, `reach` last, with status `pass`, `warn` or `skip`; `reach` is the per-host answer to whether the server's address (`server_url`'s `/healthz`) can be reached, `{hosts:[{role, name, status, reason?}], nodes_unlisted?, nodes_permission?}`: `role` is `controller` or `node`, `status` is `ok`, `failed` or `unknown` (could not be asked, which is no failure), and `reason` is, for `failed`, `dns`, `connect`, `timeout`, `tls`, `http`, `no_curl` or `other`, and for `unknown`, `not_asked`, `node_not_running`, `assistant_offline` or `command_failed`; the setup script asks from the Slurm manager node itself (and from the nodes it can reach over root ssh, unless the platform asks the nodes), and the platform asks each GPU compute node through Cloud Assistant (the nodes come from E-HPC `ListNodes`; only nodes with a card that are running are asked, at most 8 at a time and 90 seconds in all, while the Slurm manager node runs its script); `nodes_unlisted` is why the compute nodes could not be listed (`permission_missing`, with `nodes_permission` the missing permission such as `ehpc:ListNodes`, or `failed`); a `failed` host makes the `reach` step `warn` and `status` `partial`; `no_gpu_gres` are node names the account's sync lists with a card whose `Gres=` in Slurm has no `gpu` (nothing was added: slurmd would refuse them; nodes the sync did not list never appear here). When it cannot run, 409 `SLURM_APPLY_NOT_APPLICABLE` (`reason`: `no_source`, `no_provider`, `not_slurm_cluster`, `cluster_unknown` (the last sync lists no node of the cluster), `cluster_listing_pending`, `cluster_listing_failed` (see the entry list), `provider_unsupported`, `revoked`); other failures are 424: `SLURM_APPLY_PERMISSION_MISSING` (`permission`, e.g. `ehpc:GetCluster`, `ecs:RunCommand`), `SLURM_APPLY_MANAGER_UNKNOWN`, `SLURM_APPLY_FAILED` (`step`: `cluster`, `assistant`, `run`, `gres`, `setup`, `resume`; `reason` is a stable word from the script, such as `busy`, `server_unreachable`, or the setup precheck's `not_root`, `slurmctld_unreachable`, `conf_unreadable`; `output` is the last lines the Slurm manager node printed; with `reason` `server_unreachable` there are also `url`, the management node address the Slurm manager node could not reach (without the one-time code), and `kind`, one of `dns`, `connect`, `timeout`, `tls`, `http`, `no_curl`, `other`, taken from curl's exit status). **`dry_run: true` only checks and changes nothing**: no `gres.conf` line is added, no file written, nothing distributed, slurm.conf untouched, no `scontrol reconfigure`, no node resumed; it runs the same read-only logic, so what it reports is what the following `dry_run: false` does. Its failures and refusals have the apply's codes (a Slurm manager node that printed something other than a plan is 424 `SLURM_APPLY_FAILED` too); success returns `{dry_run: true, status, manager_instance, slurm_cluster, gres:{would_add:[{node,files,line}],present,no_gpu_gres}, script:{check_script,token_file,shared,fstype,copy_to,copy_files}, conf:{path,changed,interval,node_state,block,chained_program?,chained_state?,commented:[{line,key,value}],includes,unnamed_program?}, configless, rearm, resume_nodes, warnings, reach, cluster}`: `would_add` are the gres lines it would append (`line` is the text written to `gres.conf`); with `shared` false, `copy_to` are the nodes it would copy `copy_files` to; `conf.block` are the three settings of the block it would add, as written; `conf.changed` false means slurm.conf is already as the setup would write it; `commented` are the cluster's own `HealthCheck*` settings it would keep as comments in place (their program keeps running through the check script: `chained_program`, `chained_state`); `includes` and `unnamed_program` are the `Include` files the setup cannot see into and a program slurmctld runs that slurm.conf does not name; with `configless` false, slurm.conf has to be copied to the nodes by hand; `rearm` true means an earlier armed agent uninstall would be undone; `resume_nodes` are the nodes that would be resumed; `warnings` are codes that would each make the setup end `partial`: `conf_include`, `program_not_in_conf`, `not_configless`, `no_gpu_gres`, `server_unreachable` (a host cannot reach the server's address, `reach` says which; asked read-only as above), and `status` is `partial` with any, else `ok`. Only the `GWKS_APPLY` and `GWKS_SLURM` marker lines the script prints are read. The command Cloud Assistant gets holds only a one-time setup URL (`/slurm/setup.sh?once=<code>`; the random code lives in server memory only, is valid for ten minutes, works once, and is dropped when the apply ends unspent); the enrollment token appears only in the script body the Slurm manager node downloads (which writes it to the root-only token file), so the invocation record the cloud account keeps for 30 days holds no token; the two uninstall steps (`remove`) work the same way | JWT(admin) |
| POST | `/api/v1/slurm/clusters/{uuid}/remove` | The platform runs one of the two uninstall steps on the Slurm manager node, under the same conditions as apply (a revoked entry may be removed): `{"step":"uninstall_agents"}` makes every node's check script uninstall its agent on its next run and returns `wait_seconds` (one health-check interval) and `next_step: "remove_check"`; after that wait `{"step":"remove_check"}` restores slurm.conf and runs `scontrol reconfigure` (`status: "noop"` when there was nothing to restore). Returns `{step, status, manager_instance, wait_seconds, next_step?, cluster}`; an unknown `step` is 400 `SLURM_CLUSTER_INVALID`; failures as for apply | JWT(admin) |
| GET | `/slurm/setup.sh?token=` | The setup script the Slurm manager node runs, by enrollment token (`text/x-shellscript`; options `--remove`, `--uninstall-agents`, `--interval N`, `--check` (only checks and changes nothing, prints the same `GWKS_SLURM` markers with `status=plan` in place of `status=ok`, plus `install_mode`, `distribute_plan` and `conf_comment`, ending `step=done mode=check`; not combinable with `--remove` or `--uninstall-agents`); every step prints one `GWKS_SLURM key=value` marker line). An unknown token is 404; a revoked token still serves it (uninstalling needs it). The platform's own run on a cloud cluster's Slurm manager node uses `?once=<code>` instead (one-time, see apply); an unknown, spent or expired code is 404 | enrollment token / one-time code |
| GET | `/slurm/check.sh?token=` | The check script, by enrollment token (setup also writes it to the script directory); it carries no token: setup writes the token to a `token` file in the script directory (readable by root only) and the check script reads it; as above | enrollment token |
| GET | `/slurm/token-status?token=` | The one question the check script asks before it installs an agent: `{"status":"active"|"revoked"|"unknown"}`, nothing else; a revoked token installs no new agent (agents already enrolled keep running) | enrollment token |
| POST | `/api/v1/slurm/enroll` | The agent trades `{token, slurm_cluster_name, node_name}` for a node token: returns `{node_uuid, node_token, settings:{sbatch_args,idle_return_seconds,pending_timeout_seconds,account_user}}`. The same Slurm node of the same entry always gets the same node; the token binds `slurm_cluster_name` on first use and any other Slurm cluster is 403 `SLURM_ENROLL_REFUSED` (`params.reason: other_cluster`); an unknown or revoked token is 403 (`unknown_token`); a node another entry of the same Slurm cluster holds (seen within 15 minutes) is 403 (`node_taken`, `params.entry` names the holder); a new node an enrollment guard refuses is 403 (`enrollment_refused`, `params.refusal_code` is the guard's code; the agent does not fall back to a saved identity, asks again at most once a minute and joins once there is room; a node that already joined does not pass the guard when it enrolls again) | enrollment token |
| POST | `/api/v1/slurm/gpu-nodes` | An enrolled agent reports its Slurm cluster's nodes (`sinfo -a`, hidden partitions included): `{digest, state_digest, nodes?:[{name,listed_only?,state,reason?,gpus}]}`, with the node's own token (`Authorization: Bearer`). `digest` is a digest of the membership (node names, whether each is a GPU node, GPU counts), `state_digest` of the GPU nodes' states and reasons; a `listed_only` node has no `gpu` GRES and is kept by name alone (so that "no longer listed" means "left the cluster"; it is not counted as a GPU node). The server remembers the two digests it last stored per entry and answers with the settings alone when both repeat, without reading the database; when one differs (and the entry's list was not written within the last minute) it answers `need_nodes: true` and the agent reports again with `nodes` (an empty array means the cluster has no nodes). Only a changed membership replaces the table; when just states or reasons changed, only the changed rows are updated, and nothing is written when none did. Returns `{need_nodes, settings}`; a token that is not a Slurm node's is 401 | node token |

## Models

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/models` | List models; supports `page`/`page_size`, `q`, `source` (`huggingface/modelscope/url/local`), `operation`, and `source_id`. The last three accept comma-separated values; `source_id` uses case-insensitive whole-value matching. `sort` accepts `name/source/source_id/published_at/created_at/weight/presence`. Paginated responses return a `presence{ready_nodes,cached_nodes,ready_storages,in_flight,in_flight_percent,failed}` presence summary and a `summary` for all models; query model details for per-location information. See [List Filtering and Presence Summary](#列表筛选与就位汇总) | JWT |
| GET | `/api/v1/models/presence?ids=1,2` | Read only the requested models' presence summaries for download progress. `ids` is required: comma-separated positive integers, at most 200 entries, deduplicated. Returns an object keyed by model ID, with the same fields as list `presence`. Known models with no copies have zero counters; unknown/deleted IDs are omitted. Invalid, empty or excessive input returns 400; storage read failure returns 500. Progress events refresh current-page summaries; state/model changes still refresh the list | JWT |
| POST | `/api/v1/models` | Register a model from a public repository (HF/ModelScope) | JWT |
| POST | `/api/v1/models/preview-url` | Preview an HTTP model directory (`url`); requires an http/https URL with a hostname and no embedded credentials. Invalid URLs return 400 `MODEL_SOURCE_URL_INVALID`; valid but unreadable directories return 502 `HTTP_DIRECTORY_UNREACHABLE` | JWT |
| POST | `/api/v1/models/register-url` | Register an HTTP model directory (`source_url`, optional `source_id` and `name`); URL validation is the same as preview-url. Returns 201 on successful registration | JWT |
| POST | `/api/v1/models/local/preview` | Preview whether a local directory on a node can be registered as a model | JWT |
| POST | `/api/v1/models/local` | Register a local model on a node | JWT |
| GET | `/api/v1/models/{id}/logo` | Get the model logo image | JWT |
| GET | `/api/v1/models/{id}` | Get model details | JWT |
| PATCH | `/api/v1/models/{id}` | Change the source ID of a local / HTTP-directory model (`{"source_id"}`; `source_id_editable` in the details says whether it can be changed). A public-repository model returns 400 `MODEL_SOURCE_ID_FIXED`, a name another model already has returns 409 `MODEL_SOURCE_ID_TAKEN`, and an in-progress copy, download, or removal returns 409 `MODEL_TRANSFER_ACTIVE`. Existing copies keep their paths | JWT |
| DELETE | `/api/v1/models/{id}` | Delete a model | JWT(admin) |
| POST | `/api/v1/models/{id}/metadata/refresh` | Fetch model metadata again (config/README/logo) | JWT |
| POST | `/api/v1/models/{id}/download` | Start a public repository download: `storage_id` selects the storage (defaults to the default storage when omitted). For local storage, also provide `node_uuid` or `node_uuids`; for shared storage, one writer node downloads the model once | JWT |
| POST | `/api/v1/models/{id}/download/cancel` | Cancel a model download in a specified storage (`storage_id`; local storage also requires `node_uuid`); retain downloaded data for retry | JWT |
| POST | `/api/v1/models/{id}/copy` | Copy model files between two locations: `source`/`target` each contain `{storage_id, node_uuid?}`. Copy locally when the same node can read the source and write the target; otherwise, transfer directly between nodes. Returns 400 if the source and target are the same location; shared storage is identified by storage ID (ignoring the node). State conflicts such as offline or non-writable storage still return 409 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/cache/retry` | Retry a failed NFS Cache copy (`preparation.retryable=true`), with request body `{storage_id}`; returns 202 when accepted. Returns 409 `MODEL_CACHE_RETRY_BLOCKED` if the task has not failed, the node is disconnected, capacity is insufficient, termination of the previous task is unconfirmed, or a concurrent change occurs | JWT |
| DELETE | `/api/v1/models/{id}/storages/{storage_id}` | Delete model files from shared storage (and all node caches for NFS with caching); returns 409 `STORAGE_SHARED_BY_OTHER_CONTROL_PLANES` if another gpuworks control plane is using it. Use `?force=1` to force deletion | JWT(admin) |
| POST | `/api/v1/models/{id}/register-path` | Register an existing path on a node as local files for this model | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync` | Perform direct peer sync from an explicitly selected source node (`source_node`, optional `rate_limit_kib_s`; when no source node is provided, retry using the source node from the failed record) | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/probe` | Test the direct-connection handshake from the source node to the target node without transferring bytes | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/cancel` | Cancel peer sync | JWT |
| DELETE | `/api/v1/models/{id}/nodes/{uuid}` | Evict model files from a node (local storage or node cache; `?storage_id=` specifies the storage). Rejects nodes occupied by an active deployment. Any download in progress at that path is canceled first, and the operation waits for it to end; you can download again immediately after the response | JWT(admin) |

Automatic downloads, copies, and deployment acquisition keep different models in separate directories within the same storage and node. An empty `revision` still selects the source's default branch and remains distinct from explicit `main`. If another model owns the historical default directory, the new copy uses a `-model-<model ID>` suffix and retains it on retries. Ownership checks also recognize absolute paths reported by agents. If the suffixed directory is occupied too, writes are rejected; download, copy, and sync return 409 `MODEL_FILE_PATH_OCCUPIED` (`storage_id`, `path`, `model_id`). Existing copies are not moved automatically, and manual registration retains the caller's chosen directory. This check does not identify different storage definitions or symlinks that refer to the same physical directory.

### List Filtering and Presence Summary

Local-node and shared-storage rows in model details may include `transfer_error_code`. Final file-listing outcomes are `MODEL_DOWNLOAD_NETWORK_EXHAUSTED` (temporary network or upstream retries exhausted), `MODEL_DOWNLOAD_SOURCE_NOT_FOUND` (repository or revision missing), `MODEL_DOWNLOAD_SOURCE_ACCESS_DENIED` (401/403), `MODEL_DOWNLOAD_SOURCE_THROTTLED` (persistent rate limiting), and `MODEL_DOWNLOAD_SOURCE_REJECTED` (another definite source rejection). `message` retains the original platform and address error details; the page displays both category and detail. Older agents, unknown errors and mixed failure categories across sources may omit the field; omission does not mean success. Restarting a download clears its old download error code and assigns a new transfer ID. Late reports from the old task cannot overwrite the new one.

`GET /api/v1/models` without pagination parameters returns all models with presence details for each node and storage.
With `page` or `page_size`, it returns `{items, page, page_size, total, summary}`:

- Each row omits `readme`, `nodes`, and `storages`, and uses `presence{ready_nodes, cached_nodes, ready_storages, in_flight, in_flight_percent, failed}` to summarize presence instead; query `GET /api/v1/models/{id}` for details.
- `summary{registered, ready_files, sources, weight_gib}` covers all models and is unaffected by filters.

Use the query parameters as follows:

| Parameter | Usage |
|---|---|
| `q` | A substring of the name, source ID, or revision |
| `source` | `huggingface`, `modelscope`, `url`, or `local`; separate multiple values with commas |
| `operation` | Capability type; separate multiple values with commas. Matching any value is sufficient |
| `source_id` | Case-insensitive whole-value matching of source IDs; separate multiple values with commas. Use this to find registered models by a template's weight source |
| `sort` | `name`, `source`, `source_id`, `published_at`, `created_at`, `weight`, or `presence`; `published_at` is the creation time on the source site, falling back to the most recent update time when unavailable; `presence` sorts by the number of nodes where the model is ready |

### Register an Existing Node Path

The request for `POST /api/v1/models/{id}/register-path` is
`{"node_uuid":"…","path":"/data/existing-model","storage_id":1}`.
`path` is the host path of an existing directory on the node. Provide an absolute path; it can be outside the selected local storage root.
`storage_id` selects the storage to which the record belongs, rather than restricting the path. If omitted, it uses the storage of the model's existing copy on the node first, or the default local storage otherwise.
This operation only registers existing files; it does not copy or move directories. To import a library from shared storage, use the storage scan/import endpoints.

Returns `200 {"status":"registered"}` on success. The node checks that the directory is readable, contains non-empty files, and has no incomplete downloads;
Metadata must identify exactly one complete model entry in the directory. Formats with embedded metadata, such as GGUF, do not require `config.json`; parent directories, non-model directories, and unknown layouts return 409. `registered` means the selected model directory has been registered;
it does not guarantee that the engine can load the model. Do not treat a request as invalid because the path is outside the storage root.

An empty `path` returns 400; a missing target model returns 404. The node rejects missing or empty directories and directories containing unfinished `.part`/`.part.state` files (leftovers beside a file complete at the size `.part.state` records do not count);
the endpoint returns 409 and does not replace the existing registration. If the agent cannot return directory recognition metadata, the endpoint returns `AGENT_UPGRADE_REQUIRED`; upgrade the agent first. Errors such as unavailable RPC to the actual node may also return 409; handle them according to the error details.

## Images / Storages (Image Catalog and Storage Policies)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/images` | List image series | JWT |
| GET | `/api/v1/images/versions` | List image version rows; each row includes `present_nodes`/`total_nodes` and `reference_count`, without per-node rows (see series details for node rows) | JWT |
| GET | `/api/v1/images/engines` | Get the engine dictionary `[{"name","label"}]` for image forms, in display order: all deployment engines plus the catalog-only `vllm-omni` | JWT |
| GET | `/api/v1/images/community/repos` | Get community image repository cards (the six built-in categories merged with the `image_community_catalog` setting, without tags) | JWT |
| GET | `/api/v1/images/community/browse` | Use `?repo=` to list all upstream tags for the repository (any repository is allowed, with no semver filtering; cached in memory for 1 hour). An empty or malformed repository name returns 400; an upstream report that the repository does not exist returns 404 | JWT |
| PATCH | `/api/v1/images/versions/{id}` | Update an image version row: accepts only `enabled` and `registry_sources` (acquisition URLs can also be added to system versions). Other keys return 400 `IMAGE_VERSION_PATCH_INVALID`; the image catalog does not hold startup configuration (ADR 0031) | JWT(admin) |
| GET | `/api/v1/images/{id}` | Get image series details; each version's node rows include `node_name`, `node_state`, `image_digest_id`, and `updated_at`. Rows acquired through peer sync also include `source_node_name`. `state` only says whether the node's runtime holds the image; the outcome of the last acquisition is in `transfer_error_code` and `message`: `present` with `transfer_error_code=IMAGE_REFETCH_FAILED` means a forced re-fetch failed and the node kept the image it had, with the failure reason in `message`, kept until the next acquisition; `PEER_TRANSFER_CANCELED` means an operator cancelled the peer transfer; `PEER_DIRECT_CONNECT_FAILED` means the source node could not connect to the target and the copy was not relayed, with the dial error in `message` | JWT |
| PATCH | `/api/v1/images/{id}` | Update an image series: `enabled`; community/custom series also allow changes to `common_args`/`common_env` (system series return 400 `OFFICIAL_IMAGE_SERIES_MANAGED`) | JWT(admin) |
| DELETE | `/api/v1/images/{id}` | Delete a community/custom image version; system images return 400 `OFFICIAL_IMAGE_DELETE_FORBIDDEN`. Returns 409 `IMAGE_IN_USE` if still referenced by deployments, unfinished instances, or models (using the same reference criteria as system image deletion/deprecation) | JWT(admin) |
| DELETE | `/api/v1/image-series/{id}` | Delete a community/custom image series; returns 409 `IMAGE_IN_USE` if any version is still referenced | JWT(admin) |
| POST | `/api/v1/images/import` | Accepts two request body types: a system catalog manifest with `schema_version` (schema 2/3; other values return 400), or `{"series":[...]}` for community/custom additions (cannot set `source=official`) | JWT(admin) |
| POST | `/api/v1/images/sync` | Update the system image catalog from the release channel; ignores the request body and returns version counts and series. For errors, see [Catalog Import Results](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/images/preview-archive-url` | Parse a `docker save` archive URL before downloading: `{"url"}` → `url`, `size_bytes`, `repo_tags`, `previewable`, and optional `metadata_by_tag` with the engine, framework, their versions, and host architecture per reference | JWT(admin) |
| GET | `/api/v1/images/nodes/{uuid}/runtime-images` | List tagged node images. Optional query parameter `namespace`. Returns references, immutable identities, sizes, and source and deployment namespaces. | JWT(admin) |
| POST | `/api/v1/images/nodes/{uuid}/runtime-images/inspect` | Inspect the selected image configuration for catalog fields. Submit the same selection plus `node_uuid` as `runtime_image` alongside one custom version to `POST /api/v1/images/import`. | JWT(admin) |
| POST | `/api/v1/images/nodes/{uuid}/preview-archive-path` | Preview a local image tar on the selected node. Accepts `{"archive_path":"/absolute/file.tar"}` and returns `repo_tags` and optional `metadata_by_tag` to prefill the selected reference and its configuration. `IMAGE_ARCHIVE_PREVIEW_FAILED` includes `params.reason` for a missing file, permission refusal, non-regular file, invalid archive or timeout. | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync` | Trigger image acquisition on a node: an empty request body pulls according to [Pull Settings](../how-to/configure-pull-settings.md) (uses the same candidates as deployment startup; you cannot specify a repository); `source: url` / `url-relay` imports an HTTP archive; or use direct peer sync from `source_node` (optional `rate_limit_kib_s`, unrestricted by sync groups). Admins can load a compute-node file with `source: local` and an absolute `archive_path`. Specify `force: true` to replace a present image; active transfers keep their ownership. When the image is already present, a local import without force returns 202 with state `present` and no transfer ID. A node that cannot start loading the file returns 409 `IMAGE_ARCHIVE_LOAD_FAILED` with a classified `params.reason`. Invalid paths or mixed sources return 400 `IMAGE_ARCHIVE_PATH_INVALID`. A `source` value other than `""`, `default`, `url`, `url-relay`, or `local` returns 400 `IMAGE_SOURCE_REMOVED`; returns 409 `IMAGE_SOURCE_FORBIDDEN` if source rules leave the node with no available pull source. With `"force":true`, pulls again even if the image is already `present` on the node | JWT (admin for local files) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/probe` | Test the direct-connection handshake from `source_node` to the target node without transferring bytes | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/retry` | Retry image acquisition on a node; peer sync reuses the recorded source node and previous rate limit. A local retry requires an admin to supply `source: local` and `archive_path` again. An empty retry of a previous local import returns 400 `IMAGE_ARCHIVE_PATH_REQUIRED` | JWT (admin for local files) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/cancel` | Cancel an image transfer in progress on this node | JWT |
| DELETE | `/api/v1/images/{id}/nodes/{uuid}` | Delete this image from the node | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/inspect` | Read the image's OCI configuration (`entrypoint`/`cmd`/`env`/`volumes`) and image ID on the node; configuration is returned only in the response and is not written back to the catalog | JWT(admin) |
| GET | `/api/v1/storages` | List storage (type, path or export, node summary and problem nodes, model count, and deployments using it); for a local storage `nodes.on_shared_fs` is the number of nodes on which its directory sits on a shared filesystem, and with `?all_nodes=1` those nodes' states carry `shared_fs:{type,source?}` (such as `nfs4`, `10.0.0.5:/opt`; `type` is `network` when mountinfo did not say) | JWT |
| GET | `/api/v1/storages/{id}` | Get a single storage; `?all_nodes=1` returns the status of each node | JWT |
| POST | `/api/v1/storages` | Add storage; NFS types are test-mounted on one node first, and storage that cannot be written to is saved as disabled (read-only). Validation errors are told apart by cause, all 400: `STORAGE_PATH_INVALID` (malformed path, `params.path`), `STORAGE_PATH_DENIED` (under `storage_denied_paths`, `params.path`), `STORAGE_PATH_REQUIRED` (no directory), `STORAGE_CACHE_LIMIT_NEGATIVE` (negative cache limit); the same applies to updates, trial mounts and node overrides | admin |
| PATCH | `/api/v1/storages/{id}` | Update the name, enabled status, default status, node scope, path, mount options, cache directory and limit, or `nfs`↔`nfs_cache` type. Switching `nfs_cache` to `nfs` requires empty node caches, otherwise 409 `STORAGE_CACHE_NOT_EMPTY` (`params`: `entries`, `nodes`, `transferring`, `node_uuids`). Include `expected_updated_at` from the read response to prevent overwriting concurrent changes; a version change returns 409 `STORAGE_CONFIG_CHANGED` | admin |
| DELETE | `/api/v1/storages/{id}` | Remove a storage object (without deleting files) and remove models stored only in it from the model list; returns 409 `STORAGE_IN_USE` if deployments are bound to it or a deployment's model is stored only in it | admin |
| POST | `/api/v1/storages/probe` | Test-mount a storage definition on one node without leaving it mounted. Results name the actual `node_uuid`, `node_name`, `target` (the export or directory path actually checked) and `checked_at` (control-plane receipt and decode time); they describe only this node and this check. Older responses may omit the latter two fields. | admin |
| GET | `/api/v1/storages/{id}/control-planes` | Get other gpuworks control planes using the shared storage (heartbeats) | JWT |
| PATCH | `/api/v1/storages/{id}/node-overrides` | Atomically update cache overrides for fixed `node_uuids`; omitted `cache_path`/`cache_max_bytes` remain unchanged, null inherits, and a limit of 0 means unlimited. New overrides do not expand the node scope | admin |
| POST | `/api/v1/storages/{id}/cache/reconcile` | Submit background cache reconciliation with `{node_uuids, mode}` (`mode` is `"trim"` or `"clear"`); the 202 response returns the accepted count. See the cache status in storage details for per-node results | admin |
| GET | `/api/v1/storages/{id}/model-files` | Get model files in storage: one row per model for shared storage, and one row per node for local storage and `nfs_cache` node caches (including the last-used time and bound deployments) | JWT |
| POST | `/api/v1/storages/{id}/scan` | Scan model directories in shared storage, with pagination (`cursor`) | admin |
| POST | `/api/v1/storages/{id}/import` | Import scanned directories; writes only to the database | admin |
| DELETE | `/api/v1/storages/{id}/cache` | Evict all evictable node caches for `nfs_cache` storage; use `?node_uuid=` to restrict this to one node. Returns `removed`, `freed_bytes` and `failed` (each with `node_uuid`, `node_name`, `message`); any failure is 409 `STORAGE_CACHE_CLEAR_FAILED` (`params`: `failed` node count, `node` first node name), and an offline node with no cache of this storage is not a failure | admin |
| GET | `/api/v1/storages/{id}/files` | Browse files in storage ([ADR 0053](../adr/0053-storage-files.md)): `?path=` (relative to the storage root; empty means the root) `&cursor=&limit=` (up to 500 per page, ordered by name bytes; an empty `next_cursor` indicates the last page). Returns `{path, node_uuid, writable, items[{name, path, dir, symlink, size, mtime, writable}], next_cursor}`. Top-level `writable` indicates whether children can be created in the current directory; item-level `writable` indicates whether the item can be renamed/deleted. Symbolic links are listed but not followed. For shared storage, `node_uuid` is optional (automatically selects an online, ready node with agent support and a connected data lane, preferring writable nodes). It is required for local storage; otherwise, returns 400 `STORAGE_FILE_NODE_REQUIRED`. For `nfs_cache`, browses NFS itself, not the node cache | admin |
| GET | `/api/v1/storages/{id}/files/download` | Download a file or directory: `?node_uuid=&path=&format=zip\|tar.gz`. Files are returned unchanged (`Content-Length`, `application/octet-stream`); directories are archived while streaming (zip by default, `application/zip` or `application/gzip`, no `Content-Length`). Symbolic links are skipped and listed in `SKIPPED_SYMLINKS.txt` inside the archive. `Content-Disposition: attachment` includes the filename (directories append `.zip`/`.tar.gz`). Errors before byte transfer starts return JSON; errors afterward close the connection directly. Downloads remain available when storage is disabled | admin |
| PUT | `/api/v1/storages/{id}/files/upload` | Stream an upload in a single request: `?node_uuid=&path=&extract=tar\|tar.gz\|zip&overwrite=true\|false`, with file bytes as the request body (`application/octet-stream`). `path` is the destination file without extraction, or the destination directory with extraction. The server does not write to disk or cache the upload; `Content-Length` is the expected size (unknown when omitted). The agent first stages the upload in `.gpuworks/incoming/`, then atomically renames it to the destination after completion (including extraction) before returning `{path, bytes, files}`. All checks that do not require the agent are completed before reading the request body | admin |
| POST | `/api/v1/storages/{id}/files/mkdir` | Create a directory with `{node_uuid, path}` (including missing parents); existing directories do not cause an error. Returns `{path}` | admin |
| POST | `/api/v1/storages/{id}/files/rename` | Rename or move with `{node_uuid, from, to}`; both paths must be in writable regions, and the destination must not exist. Returns `{path}` | admin |
| DELETE | `/api/v1/storages/{id}/files` | Delete a file or directory with `?node_uuid=&path=`: first moves it to the storage's trash directory, then deletes the bytes in the background; 204 | admin |
| GET | `/api/v1/storages/settings` | Get the control plane display name and reserved free space on cache disks | JWT |
| PATCH | `/api/v1/storages/settings` | Update the preceding settings | admin |
| GET | `/api/v1/storages/usage` | Get model disk usage statistics for each node | JWT |

### Register an image already on a node

As an administrator, call `GET /api/v1/images/nodes/{uuid}/runtime-images`. In the example, `list` is the response and `selectedIndex` identifies the row chosen by the user. Take `ref` and `identity` from that row, and `runtime`, `namespace` and `target_namespace` from the top level. Both namespace fields are empty strings for Docker. For nerdctl, `namespace` is the source and `target_namespace` is the node’s deployment target.

```js
const row = list.items[selectedIndex];
const selection = {
  ref: row.ref,
  identity: row.identity,
  runtime: list.runtime,
  namespace: list.namespace,
  target_namespace: list.target_namespace
};
const inspectBody = selection;
const importBody = {
  runtime_image: { ...selection, node_uuid: nodeUUID },
  series: [{
    source: "custom", engine: "vllm", framework: "cuda",
    versions: [{ image_ref: row.ref, framework_version: "13.0" }]
  }]
};
```

Send `inspectBody` to `POST .../runtime-images/inspect` on the same node to read the metadata. Complete the catalog fields and send `importBody` to `POST /api/v1/images/import`. Use the GET request’s node UUID for `nodeUUID` and the image’s actual engine, framework and version. When a cross-namespace copy is needed, import returns the registered version and the node copies in the background. Use node distribution to monitor the transfer state and bytes copied or to cancel it.

### Storage Configuration and Cache Status

- `updated_at` serves as the edit version; `override_only: true` on a target indicates a node configuration override only, which does not participate in scope selection. Omitting it or setting it to `false` preserves explicit selection semantics.
- NFS cache nodes can include `cache{used_bytes,reclaimable_bytes,protected_bytes,limit_bytes,state,reason?,message?}`.
  `state` is `pending`, `running`, `failed`, `offline`, `blocked`, `satisfied`, or `unavailable`.
- An absent `cache` means statistics are unknown, not 0; when a new directory has not been verified, do not substitute the old directory's disk capacity.

### Storage File Operation Scope and Errors

Uploads, directory creation, renaming, and deletion are allowed only under `files/` and `.gpuworks/apps/`; these two region directories themselves cannot be deleted or renamed.

| HTTP Status | Error Code | Meaning |
|---|---|---|
| 403 | `STORAGE_FILE_REGION_READONLY` | The path is outside a writable region, or the operation attempts to modify the region directory itself |
| 400 | `STORAGE_FILE_PATH_INVALID` | The path contains `..` or backslashes, or resolves outside the storage root |
| 404 | `STORAGE_FILE_NOT_FOUND` | The path does not exist |
| 409 | `STORAGE_FILE_EXISTS` | The destination already exists |
| 409 | `STORAGE_FILE_AGENT_UNSUPPORTED` | The node agent does not support file operations; `params.node` identifies the node |
| 422 | `STORAGE_FILE_EXTRACT_FAILED` | Extraction failed |
| 503 | `STORAGE_FILE_DATA_LANE_UNAVAILABLE` | The node data lane is disconnected during upload or download; retry is allowed |
| 502 | `STORAGE_FILE_OPERATION_FAILED` | Another node-side operation failed |

May also return 409 `STORAGE_DISABLED` (write operations), `STORAGE_NOT_WRITABLE`, `NODE_OFFLINE`, or `STORAGE_UNAVAILABLE_ON_NODE`.

### Catalog Import Results

`POST /images/sync` and `POST /images/import` with `schema_version` import the system catalog and return
`{catalog_version, created, updated, deleted, deprecated, skipped:[], series:[...]}`.
Counts are per version row; `series` contains the system series after import. System versions absent from the manifest are deleted if unreferenced, or deprecated if referenced.
Community/custom imports return the affected series as `{items:[...], total}`.

Failures leave the catalog unchanged. Handle the following errors:

- All release sources are unreachable: 502 `UPDATE_SOURCES_UNAVAILABLE`; an index exists but no catalog file matches it: 502 `UPDATE_DOWNLOAD_FAILED`.
- The channel has not published a catalog: 404 `UPDATE_NOT_PUBLISHED`.
- Invalid content: 400 `IMAGE_MANIFEST_INVALID`; a name or ref conflicts with a community/custom row: 400 `IMAGE_IMPORT_CONFLICT`.

The version row's `rolling_tag`, `deprecated`, and `deprecated_since` describe rolling tags and deprecation status;
`reference_count` counts deployments, unfinished instances, and models that reference the ref.
For the format and import rules, see [Image Catalog and Inference Image Families](image-families.md).

## Inference Templates (Inference Catalog/Templates)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/inference-templates` | List inference templates | JWT |
| DELETE | `/api/v1/inference-templates?model_id=…` | Delete a custom template and all its specs by model_id. A blocking deployment or component reference returns 409 and preserves all specs. | JWT(admin) |
| GET | `/api/v1/inference-templates/index` | Get the template index (a lightweight list for selectors) | JWT |
| GET | `/api/v1/inference-templates/organizations` | List organizations/publishers represented in the templates | JWT |
| GET | `/api/v1/inference-templates/chips` | Static catalog of accelerator cards supported by templates; optional `arch_id` identifies the architecture required by a template, with other vendor fields unchanged | JWT |
| GET | `/api/v1/inference-templates/my-chips` | List accelerator cards detected in the current cluster, with `vendor`, `name`, optional `cc`, and reported `arch_id` (omitted when unavailable) | JWT |
| GET | `/api/v1/inference-templates/summary` | Get template summary statistics | JWT |
| GET | `/api/v1/inference-templates/weight-models` | Registered models for the weight sources one spec declares (`template_id`, `spec_source`, and `spec_name` are required; a missing one returns 400 `TEMPLATE_WEIGHT_MODELS_QUERY_INVALID`, an unknown spec 404): `models` are the models serving any source (repo ID matched case-insensitively, a whole repository serves a subfolder request, newest first, each once, rows shaped as in the paged list; a rank-scoped source lists only rank 0's artifact, and a known revision must equal the one the source states); `sources` lists the sources in declared order, and `reuse_model_id` is the newest same-provider model a choice of that source reuses | JWT |
| GET | `/api/v1/inference-templates/export/{digest}` | Export an executable template package by content digest | JWT(admin) |
| GET | `/api/v1/inference-templates/model/*` | Get template details by model path. `can_delete` indicates whether the caller may request whole-template deletion. References are checked at deletion time. | JWT |
| GET | `/api/v1/inference-templates/yaml?model_id=…` | Get flat YAML for the template editor: the server writes it using the same catalog types as import. Add `spec_source=user` (or `upstream`) to retain only specs from that source; `readme_*` fields are written only when all specs are listed or the template itself belongs to the user. Saving it unchanged (`import?overwrite=true`) preserves all fields | JWT |
| GET | `/api/v1/inference-templates/schema` | Get the dictionary for the flat YAML editor: field completion, hover descriptions, and constraints for initial client-side validation (authoritative validation still occurs at import); optional `spec.gpus.arch_id` must be nonblank and matches the reported architecture ID exactly, ignoring case (missing IDs do not match) | JWT |
| POST | `/api/v1/inference-templates/clone` | Clone a template into an editable user spec | JWT(admin) |
| POST | `/api/v1/inference-templates/evaluate` | Check flat YAML or a catalog package without writing data; accepts the same request as `import` and supports `?overwrite=true`. Returns 200 `{valid, errors, warnings}`; for check scope and results, see [Pre-Import Checks](#导入前检查) | JWT(admin) |
| POST | `/api/v1/inference-templates/import` | Import a template/catalog package; returns `{models, package_digest}` on success, with an additional `warnings` field when warnings exist (same as `evaluate`; the field is absent when there are no warnings) | JWT(admin) |
| POST | `/api/v1/inference-templates/sync` | “Check for updates”: fetch the official inference catalog package from the release channel and reconcile upstream sources (leaving user specs unchanged); ignores the request body. Returns `{models, package_digest, version}`; models or specs this server cannot read are skipped and stay as they are, and the response then also carries `skipped: [{model_id, spec?, reason}]` (`spec` is omitted when a whole model is skipped; see [What an Official Package Carries That This Server Cannot Read](inference-catalog-package.md)). Returns 409 `UPDATE_CATALOG_INCOMPATIBLE` if the catalog package schema is newer than this server supports; returns 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` if the package cannot be read at all: damaged, inconsistent with its manifest, or without a single model this server can read (`params.error` contains the original error, and the UI indicates that this version of the inference templates requires the latest server version). For other errors, see [Catalog Import Results](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/inference-templates/official` | Upload an official inference catalog package (request body is a `.tar.gz` catalog package, up to 32 MiB), using the same upstream reconciliation as “Check for updates,” with the same response and skip rules. Returns 400 `OFFICIAL_CATALOG_PACKAGE_REQUIRED` if the input is not a catalog package, or 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` if the package cannot be read at all | JWT(admin) |

For template package structure and import rules, see [Executable Inference Catalog Package](inference-catalog-package.md).

`GET /api/v1/inference-templates/index` retains the complete index by default. With `compatibility=detected&vendor=<known vendor>`, the server computes each profile's `detected_compatible` from cards reported by online nodes and returns the participating `hardware`. Add `node_uuid` to scope a manual node selection. An invalid mode/vendor returns 400; an unknown node returns 404. Offline nodes have no compatible cards. This field means chip-rule compatibility only; deployment evaluate still checks memory, the specific selected cards, and the complete launch.

Template details expose `detected_support` with the server-selected `preferred_engine` and `warnings` (including `alternative_available`). The UI only presents these facts. Hardware-filtered lists, details, and recommendations share the server chipmatch unsupported veto and specificity rules; the browser does not recompile name regexes or infer architecture eligibility.

### Pre-Import Checks

`POST /api/v1/inference-templates/evaluate` accepts the same flat YAML or `.gpuworks-catalog.tar.gz` catalog package as `import`, also supports `?overwrite=true`, and uses the same parsing and validation.

Returns 200 `{valid, errors, warnings}`. Each item in `errors` and `warnings` is `{code, params, error}`: `code` is stable, `error` is English text, and `params` includes `model_id` and `spec`.
When parsing or validation fails, `valid=false` and `errors` contains the same error codes that import would return; parsing errors without a dedicated code use `CATALOG_INVALID`, with the original text preserved in `params.detail`.

This endpoint does not check whether a user spec with the same name already exists (the actual import may return 409), fetch weight sizes, or write to template or catalog package storage. For warning codes, see [Inference Catalog Syntax Reference](inference-catalog-spec.md#导入前检查与警告).

## Deployments / Instances (Deployments and Instances)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/deployments` | List deployments; with `page`/`page_size`, returns paginated list columns, without them every matching deployment object; either way it filters by `q`/`state`, and an unknown `state` is 400 `LIST_FILTER_INVALID` (see “Error and Response Conventions”). `state` includes `sleeping`/`asleep`, and rows also carry `asleep_replicas` and `sleep_level`; see [Deployment Sleep State](#部署的睡眠状态stateasleep_replicassleep_level) | JWT |
| GET | `/api/v1/routing-policies` | Get server-provided multi-replica routing policies and localized descriptions | JWT |
| POST | `/api/v1/deployments/preflight` | Run preflight checks for multi-node inference deployment (rank assignment and network probing). Request `{node_uuids, port?, master_addr?, bandwidth?, seconds?}`; returns `{ok, pairs}`, where each directed node pair is a check result: `id` is `node_pair`, `status` is `pass` or `fail`, `code` is `PREFLIGHT_PAIR_REACHABLE`, `PREFLIGHT_PAIR_UNREACHABLE`, `PREFLIGHT_LISTENER_FAILED`, `PREFLIGHT_PROBE_FAILED`, or `PREFLIGHT_BANDWIDTH_NOT_MEASURED`, `params` holds `source`, `target`, and `address` (plus `port` for a listener failure), and `message` is the English fallback; also `source_uuid`, `target_uuid`, `target`, `reachable`, `rtt_ms_p50`, `rtt_ms_p99`, `gbps`, `bandwidth_tested`, `bytes_sent`, `bytes_received`, and `duration_seconds`. `evidence` and `bandwidth_evidence` are the agent's or RPC's own words, kept only as evidence. `ok` is false when any pair is `fail` | JWT |
| POST | `/api/v1/deployments` | Create a deployment (`model_source` contains only `{storage_id, provider}`: where to store the model and which provider's weights to run; it does not accept an image acquisition method. `placement.pd` specifies a PD disaggregated deployment; see below). Returns 409 `SERVED_NAME_IN_USE` if the served model name is occupied by a deployment that is not being deleted; `params` includes `served_name`, `deployment`, and `deployment_id`. When omitted, the name defaults to the model name and undergoes the same validation | JWT |
| GET | `/api/v1/deployments/{id}/candidates` | Query the candidate pool for the next replica; `status` is `room/busy/unmet/cordoned/offline`. Supports `offset` and `limit` (1–200, default 50). Returns overall `counts`, `unmet_by_key`, and one page of `items`; `room` does not guarantee that the model fits. Check the deployment's `placement_status` as well. See [Query the Candidate Pool](#候选池查询) | JWT |
| GET | `/api/v1/deployments/{id}/draft` | Read the deployment back as the request that creates it (`deployment`), with `revision` and the non-editable fields `locked`. The deploy page fills its edit and "Clone and modify" forms from it. See [Edit a Deployment](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}` | Edit a deployment: submit the modified draft with `base_revision`; `?dry_run=1` returns only the change plan. Launch configuration changes replace instances according to `apply` (`rolling`/`all`/`none`). See [Edit a Deployment](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}/requirements` | Replace the node requirements and placement strategy of a regular single-node deployment and return its details. Affects only replicas started afterward; running replicas are not migrated. See [Modify Node Requirements](#修改节点条件) | JWT |
| GET | `/api/v1/deployments/{id}` | Get deployment details. Takes no query parameters; returns the deployment object directly, or 404 if not found. Includes `state` (the one state to show, `asleep`/`sleeping` when it sleeps), `asleep_replicas` (replicas the gateway does not dispatch to because of sleep), `sleep_level` (the level an engine reports), `instances`, `node_requirements`, `node_requirement_labels`, `placement_strategy`, `ready_replicas` (two counts of the same replicas as `asleep_replicas`; a PD deployment also gives the per-role breakdown), `health`, `tokenize_enabled`, and `max_replicas`; `placement_status` gives scheduling failure reasons, `retry_exhausted` identifies replicas that have exhausted their retries, and `restart_requested_at` identifies a rolling restart. See `units` for multi-node replicas. See [Query Deployment Details](#查询部署详情) and [Deployment Sleep State](#部署的睡眠状态stateasleep_replicassleep_level) | JWT |
| PUT | `/api/v1/deployments/{id}/routing` | Change the routing policy (`least_request`, `prefix_affinity`, or `failover`) | JWT + administrator |
| PUT | `/api/v1/deployments/{id}/dgpu` | Adjust `vram_limit_gib` for a single-node dGPU deployment. Returns `live` instances where the change takes effect immediately and `pending` instances that require a restart. See [Adjust the GPU Memory Limit](#调整显存上限) | JWT |
| PUT | `/api/v1/deployments/{id}/health-check` | Enable or disable health checks for the deployment and set the threshold: `{"enabled": true, "threshold_sec": 60}` (`threshold_sec` is 1–600; omit it or use 0 to retain the current value). Enabling requires the model to provide chat, completions, embedding or System One decisions; otherwise returns 400 `FIELD_INVALID` (`params.field` is `health_check`). An out-of-range threshold returns 400 `FIELD_INVALID`. Does not restart instances; the new settings apply to the next check. Returns deployment details | JWT |
| PUT | `/api/v1/deployments/{id}/tokenize` | Enable or disable gateway forwarding of tokenization requests (`/v1/tokenize`, `/v1/detokenize`) to this deployment: `{"enabled": false}`. `enabled` is required; omitting it returns 400 `FIELD_INVALID` (`params.field` is `enabled`), and a non-boolean value returns 400. Returns 404 if the deployment does not exist. Does not restart instances; takes effect after the gateway routing table is rebuilt in response to the change event. Returns deployment details (including `tokenize_enabled`) | JWT |
| GET | `/api/v1/deployments/{id}/events` | Deployment event timeline (newest first, cursor pagination, collapsed on read) | JWT |
| POST | `/api/v1/deployments/{id}/clone` | Clone the saved deployment configuration, including node requirements and placement strategy. The snapshot remains available for cloning after the upstream catalog removes its original template or spec. The clone's served model name is the original name followed by `-` and 4 random lowercase letters or digits. The request body `{"name": ...}` sets both the deployment name and the served model name; returns 409 `SERVED_NAME_IN_USE` if the name is taken | JWT |
| POST | `/api/v1/deployments/{id}/catalog-template` | Write a running deployment as an inference template: first send the smallest request each cheap operation (chat, completions, embeddings, System One decision) takes, then build flat YAML from the launch arguments, weight commit and GPUs the deployment actually ran. Nothing is written to the catalog. See [Save as an inference template](#另存为推理模板) | JWT |
| POST | `/api/v1/deployments/{id}/stop` | Stop the deployment: delete all instances and retain the configuration | JWT |
| POST | `/api/v1/deployments/{id}/start` | Start a stopped deployment and reschedule it using the retained configuration (resetting each replica's retry count). Does nothing if already running; returns 409 `DEPLOYMENT_DELETING` if the deployment is being deleted | JWT |
| DELETE | `/api/v1/deployments/{id}` | Delete the deployment | JWT |
| POST | `/api/v1/deployments/{id}/sleep` | Put the deployment to sleep (`?level=1\|2`, default 1; `?mode=wait\|abort`, default wait; `?wake_on_request=true\|false`, whether a request can wake this sleep, defaulting to the deployment's `sleep_wakes_on_request`, any other value returns 400 `SLEEP_INVALID_WAKE_ON_REQUEST`): write the sleep intent to every active instance's desired state. The gateway immediately stops dispatching requests to them. For a held sleep (`wake_on_request` false) the deployment is marked as under a deployment-wide sleep command (instances started afterward also sleep accordingly and are held); when a request can wake it only the awake replicas are named, and the deployment as a whole is not under a command. The response is `{"status":"sleeping","level","mode","wake_on_request"}`, and the timeline's `SLEEP_ORDERED` records `wake_on_request`. `?role=prefill` puts only instances of that PD role to sleep (the response includes `role` and `instances`), without placing the whole deployment under a sleep command. Returns 400 `PD_NOT_PD` for non-PD deployments, 400 `PD_ROLE_UNKNOWN` for a role not in the deployment's mode, or 400 `SLEEP_ROLE_CANNOT_EMPTY` for a role that the mode does not allow to be empty (such as decode). With `level=1`, if the node's host memory cannot hold the weights to be offloaded (available memory minus a headroom of max(5% of total, 4 GiB)), returns 409 `SLEEP_HOST_MEMORY_INSUFFICIENT` (`params.node`, `need_gib`, `available_gib`); use L2. A node with no recent host memory observation is not refused | JWT |
| POST | `/api/v1/deployments/{id}/wake` | Wake the deployment: sleeping instances must fit back on their original GPUs; otherwise returns 409 `WAKE_INSUFFICIENT_GPU_MEMORY` (all or nothing). `?role=` wakes only instances of that PD role. `?partial=true` wakes those that fit, with response `{"status":"waking","woken":[...],"refused":[{"instances","node","gpu_indexes","shortfall_gib","occupants"}]}`; still returns 409 if none fit. The 409 parameters describe the first rejected node, and `refused` lists all rejected replicas. Any wake operation revokes the deployment-wide sleep command and clears the held sleep of the instances it wakes, and a wake that has to wait (202 `waiting`) also clears the hold of the waiting instances. A card held by a process outside the platform returns 409 `WAKE_GPU_HELD_OUTSIDE` (same parameters, `refused[].held_outside` is true). `?wait=true` wakes what fits and makes the rest wait for room; `?yield=true` (optionally `&yield_level=1\|2`, default 1, switched to 2 automatically when the occupant's host cannot hold L1) first puts the occupants to sleep and then waits; see [Waiting Wakes and Yielding Wakes](#唤醒等位与让位唤醒) for the responses | JWT |
| POST | `/api/v1/deployments/{id}/units/suggest` | Suggest nodes for one replica of a multi-node deployment. The request body `{"units":[...]}` lists the replicas already selected. Returns a suggested `unit` or the reason placement is not possible, without saving. See [Suggest Nodes for a Multi-Node Replica](#推荐多机副本节点) | JWT |
| POST | `/api/v1/deployments/{id}/units/retry` | Give a replica a fresh retry allowance: request body `{"role","index"}` (`role` is empty for regular deployments). Remove the replica's failed attempts (clean up containers and retain the timeline); the controller then recreates it without affecting other replicas. Returns 404 `UNIT_UNKNOWN` if the replica does not exist, 409 `UNIT_NOT_FAILED` if there are no failed attempts or instances still exist, or 409 `UNIT_RETRY_NOT_ACTIVE` if the deployment is not running | JWT |
| PUT | `/api/v1/deployments/{id}/replicas` | Change the replica count for a single-node deployment (when growing, `place_on` names the nodes the new replicas start on first), the replica node list for a manually placed multi-node deployment, or the replica count for an automatically placed multi-node deployment (`place_on` names the nodes of the new replicas' ranks). See [Adjust Replicas for Regular Deployments](#普通部署的副本调整) for all fields and conditions | JWT |
| PUT | `/api/v1/deployments/{id}/placement-mode` | Switch a multi-node or PD deployment between placement modes: body `{"mode":"auto"}` or `{"mode":"manual"}`. Instances are not restarted and units keep their current nodes; switching to `auto` also drops the cards PD units were pinned to. 200 returns the deployment detail (`placement_mode` has the new value). A `mode` other than these two returns 400 `FIELD_INVALID` (`params.field` is `mode`), a single-node deployment returns 400 `PLACEMENT_MODE_SHAPE`, and an unknown deployment returns 404; see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署) | JWT |
| DELETE | `/api/v1/deployments/{id}/wake-waiting` | Cancel every waiting wake of the deployment; returns `{"items":[instance UUIDs],"total":n}` | JWT |
| POST | `/api/v1/deployments/{id}/wake-on-request` | Let requests wake it: clear the held sleep of every instance of the deployment without waking any instance (the sleep intent is unchanged), and withdraw the deployment-wide sleep command; returns `{"released":[instance UUIDs]}`, an empty list when no instance was held. When something is released the timeline records `SLEEP_HOLD_RELEASED` (`instances`, `actor`). A deployment that cannot sleep returns 400 (the same code as sleep), and a missing deployment 404 | JWT |
| GET | `/api/v1/deployments/{id}/schedule` | The deployment's schedule, `{"items":[schedules],"total":n}`; see [Schedules](#定时计划) | JWT |
| POST | `/api/v1/deployments/{id}/schedule` | Create a schedule entry; 201 returns it. Validation failures return 400 `SCHEDULE_INVALID`, `SCHEDULE_TIMEZONE_UNKNOWN`, `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` (a multi-node or PD deployment placed by hand), `SLEEP_NOT_ENABLED`, or `SLEEP_ENGINE_UNSUPPORTED` | JWT |
| PUT | `/api/v1/deployments/{id}/schedule/{sid}` | Replace one entry; the body is the same as for creating it, 200; an entry that does not belong to the deployment returns 404 | JWT |
| DELETE | `/api/v1/deployments/{id}/schedule/{sid}` | Delete one entry, 204; a missing one returns 404 | JWT |
| PUT | `/api/v1/deployments/{id}/yield-policy` | Set the yield policy and its parameters. The body is `{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`; `policy` (`never` / `idle` / `timeslice` / `anytime`) is required, every other key may be omitted and then takes the policy's default, and inapplicable keys are ignored. 200 returns the stored policy, inapplicable parameters absent; setting `idle` while the cards already hold another `idle` deployment adds `idle_neighbours`. A missing `policy` (`{}`, or parameters only), an invalid value, or an out-of-range number returns 400 `YIELD_POLICY_INVALID` (`params.field`, `reason`: `required` / `invalid_value` / `out_of_range`, which also carries `minimum` and `maximum`); a policy other than `never` on a deployment that cannot sleep returns 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED`; a policy other than `never` on a multi-node or PD deployment returns 400 `YIELD_POLICY_SHAPE`; see [Yield policy](#出让策略) | JWT |
| PUT | `/api/v1/deployments/{id}/node-replacement` | Set how long a lost node is waited for before its replicas move, `{"after_sec":600}`; 0 is never, otherwise 60–604800. 200 returns the same body; out of range returns 400 `NODE_REPLACEMENT_INVALID` (`params.minimum`, `maximum`); see [Reschedule Replicas on Node Loss](../how-to/node-failover.md) | JWT |
| GET | `/api/v1/scheduling/board` | The scheduling board: nodes paged by `?page=&page_size=&q=` (each GPU's awake usage, sleep residue, free amount, occupants, and outside processes) plus fleet-wide `pending` (deployments refused by the scheduler in the last 30 minutes), `waiting_wakes`, `upcoming` (schedule actions in the next 24 hours); `q` matches a node's name, hostname or UUID; each GPU carries the `state` and `switching` the server derives; there is no `recent` any more (read the scheduling history for the latest automation events); `?node=<uuid>` answers for that one node only (a one-row `nodes` page and empty other lists; 404 for an unknown node), and `?queues=1` answers only the last three lists (an empty `nodes` page, no node is read); see [Scheduling Board](#调度看板) | JWT |
| GET | `/api/v1/scheduling/overview` | The scheduling overview: every node and the compact state of every card of the whole cluster in one response, plus the summary, for the heatmap; served from a cache (rebuilt at most every 2 seconds), with an `ETag` and a 304 for a matching `If-None-Match`; see [Scheduling Overview](#调度总览) | JWT |
| GET | `/api/v1/scheduling/history` | The scheduling history: scheduling events of the whole cluster, newest first, cursor-paged (`limit`, `before`, as for the deployment timeline), filterable by `deployment_id`, `category` (`auto` / `failure` / `manual`, comma separated), and `since` / `until` (RFC 3339); each entry carries the nodes it concerns, `nodes` (`uuid`, `name`); an entry reporting a failed attempt (`REPLICA_REBUILT`, `UNIT_RETRY_EXHAUSTED`) also carries `logs`: that attempt's instance `instance` and a window from 10 minutes before to 1 minute after the failure, `from` / `to` (Unix ms), the parameters of the logs page `/logs?instance=&from=&to=`; an invalid filter returns 400 `FIELD_INVALID` (`params.field`), an invalid cursor or page size 400 `EVENT_CURSOR_INVALID` / `EVENT_LIMIT_INVALID`; see [Scheduling History](#调度记录) | JWT |
| GET | `/api/v1/scheduling/timeline` | The residency timeline: `?node=<uuid>` (repeatable, at most 200, normally the node that was opened) `&window=15m\|1h\|6h\|24h` (default 6h), or a custom range `&since=<RFC 3339>&until=<RFC 3339>` (`until` defaults to now; at most 7 days; `until` is not after now, with a minute of clock skew allowed; not combined with `window` or `hours`). The segments in which each deployment's instance held each card over the window (`awake` / `asleep` / `switching` / `starting`), and the stats of the deployments whose yield policy is not `never` that appear; `?sharing=1&window=15m\|1h\|6h\|24h` (default 6h) without `node` answers only the stats of the cluster's deployments whose yield policy is not `never` (and of deleted ones that took part in a yield in the window) (`nodes` empty, cached for about 30 seconds); the response's `windows` lists the windows each read offers; the older `hours=6\|24` still works and means `window=6h\|24h`; a missing `node` (without `sharing`), a `sharing` that is not 1 or is used with `node`, a `window` not offered, an `hours` that is not 6 or 24, a `since` or `until` that is not an RFC 3339 time, an `until` without `since`, a `since` not before `until`, an `until` after now, a range over 7 days, or `since` / `until` used with `window` / `hours` / `sharing` returns 400 `FIELD_INVALID` (`params.field`, a message such as `window is one of 15m, 1h, 6h, 24h`), unknown nodes are skipped; see [Residency Timeline](#驻留时间线) | JWT |
| GET | `/api/v1/scheduling/activity` | The scheduling activity: `?window=15m\|1h\|6h\|24h\|7d` (default 24h) or `?since=&until=` (RFC 3339, `until` defaulting to now). Per bucket of the length the server picks (`bucket_seconds`), the scheduling events of each reason (`buckets[].counts`, in the order of `reasons`); each reason with its total over the range, `reasons[]` (`key`, `count`); the at most 20 cards that switched most over the range, `top_cards[]` (`node_uuid`, `node`, `gpu_index`, `switches`); `beyond_retention` true when the range begins before the statistics are kept; it never reads the event log and is cached for about 30 seconds; a `window` not offered, a `window` with `since` / `until`, an `until` without `since`, a malformed time, an `until` not after `since`, or a range over 1825 days returns 400 `FIELD_INVALID` (`params.field`); see [Scheduling Activity](#调度活动) | JWT |
| GET | `/api/v1/scheduling/yield-policies` | The dictionary of yield policies: the four policies from the highest rank to the lowest, each with its `defaults` and the parameters that can apply (`applies`), plus the conditions under which a parameter applies only when another has certain values (`applies_when`), the range of each numeric parameter (`bounds`, with `at_most` naming another parameter it may not exceed), `sleep_levels`, and `unfinished`, from which a client edits without keeping its own defaults; see [Yield policy](#出让策略) | JWT |
| PUT | `/api/v1/deployments/{id}/pd` | Adjust replicas for each role (`roles.<role>.units`) and `fallback` in a PD deployment. `mode`, each role's `tp`/`pp`, and `kv_load_failure` are immutable (`PD_IMMUTABLE`). Returns 400 `PD_NOT_PD` for non-PD deployments | JWT |
| POST | `/api/v1/deployments/{id}/restart` | Recreate the deployment using the current configuration and reset replica retry counts. The request body accepts an optional `strategy` (default `rolling`; `all` interrupts service). See [Restart Strategies](#重启策略) | JWT |
| POST | `/api/v1/evaluate` | Estimate GPU memory, simulate scheduling, and estimate acquisition (without creating a deployment). The response's `acquisition` includes `present` (storage that already holds the model and is readable by the target node), `targets` (storage to which nodes can save the model when acquiring it), `providers` (the template's weight sources), `image`, `nodes[]` when nodes are specified in the request, and `components[]` for the template's components once the target nodes are known (see "Evaluation Requests and Acquisition Estimates") | JWT |
| GET | `/api/v1/instances` | List instances, newest first; with `page`/`page_size`, returns one page (`{items,page,page_size,total}`, see “Error and Response Conventions”), without them every match (`{items,total}`); either way it filters by `q` (a case-insensitive substring of the instance UUID, its deployment's name or its node's name) and `state` (the instance's own state, comma separated: `pending`, `pulling`, `starting`, `optimizing`, `running`, `draining`, `removing`, `stopped`, `failed`, `pending-removal`; any other value is 400 `LIST_FILTER_INVALID`); a page past the last one, however large, returns empty `items` and the true `total` | JWT |
| GET | `/api/v1/instances/{uuid}` | Get instance details. Server-derived fields: `replica` (the replica `{role,index}` served by this instance; omitted if none), `asleep` (not receiving requests because of sleep: commanded to sleep or still asleep), `desired_sleep` (the commanded level), `sleep_held` (the instance's sleep was ordered by an operator or a schedule, so a request does not wake it; the hold is recorded per instance and a wake clears only the woken instances' holds), and `can_sleep_alone` / `can_wake_alone` (whether it can sleep or wake individually). Followers of a multi-node replica do not report their own sleep level; `sleep_level` is the level of the leader in their generation. For old rows without a recorded role, `dist_role` is inferred from headless and rank. A failed instance carries `runtime_failure` (`{code, params, message}`): `code` is the failure code (such as `CONTAINER_EXITED` or `ENGINE_HEALTH_TIMEOUT`), `params` are the reason's typed fields (`phase`, `exit_code`, `oom_killed`, `port`, `resource`, `detail`, `rank`, `node`, `evidence`, and `causes`: the known failures the engine's output showed, each `{code, params, message}`), and `message` is the English fallback; reports from older agents and older rows have no such field, only `message`. `instances[]` in deployment details includes the same fields, and PD roles include `may_be_empty`. `previous_attempt: true` marks retained failed attempts superseded by a later failure or a filled replica. While a unit class has an unfilled replica, its newest failure remains current and omits this field. The verdict is shared by instance detail, instance list rows (including filtered pages), and deployment `instances[]`; it is derived without storing new state. | JWT |
| GET | `/api/v1/instances/{uuid}/events` | Instance event timeline (newest first, cursor pagination, collapsed on read) | JWT |
| GET | `/api/v1/instances/{uuid}/launch-facts` | Three-layer projection of instance launch facts (intent/resolved/runtime observation; unknown is not replaced by a fallback; sensitive text is redacted) | JWT |
| GET | `/api/v1/instances/{uuid}/logs` | Instance container logs (SSE, through the agent logtail tunnel) | JWT |
| POST | `/api/v1/instances/{uuid}/sleep` | Put only the replica containing this instance to sleep (`?level=`, `?mode=` and `?wake_on_request=` are the same as at deployment level). Any rank of a multi-node replica represents the entire replica; intent is written to all its instances, and the leader's engine puts followers to sleep along with it. The response's `instances` lists all ranks. Other replicas in the deployment continue serving, and the deployment is not considered asleep as a whole (new instances are awake if this replica is recreated). Returns 400 if the deployment cannot sleep (same codes as at deployment level), 409 `INSTANCE_NOT_SERVING` if the instance is not serving a replica, or 409 `SLEEP_ROLE_CANNOT_EMPTY` if this is the last awake replica of a role that the PD mode does not allow to be empty | JWT |
| POST | `/api/v1/instances/{uuid}/wake` | Wake the replica containing this instance (the entire multi-node replica, as above). Checks only whether this replica fits back on its original GPUs (409 `WAKE_INSUFFICIENT_GPU_MEMORY`). Revokes the deployment-wide sleep command; other sleeping replicas remain asleep. Also accepts `?wait=true`, `?yield=true`, and `yield_level`, and can return 409 `WAKE_GPU_HELD_OUTSIDE` | JWT |
| POST | `/api/v1/instances/{uuid}/restart` | Restart the replica containing this instance (a follower represents its entire generation). By default, put the replica's leader into `draining` (the gateway stops dispatching new requests, and it retires after finishing its current requests) while starting replacement instances. `?drain=false` immediately removes the entire generation. Does not consume the replica's retry allowance. Records deployment event `REPLICA_RESTARTED{instance,drain}`. Returns 409 `DEPLOYMENT_NOT_ACTIVE` if the deployment is not active, or 409 `INSTANCE_NOT_SERVING` if the instance is not serving a replica (failed, draining, or being removed) | JWT |

The instance launch facts endpoint provides three layers of information: deployment intent (intent), the resolved launch plan (resolved), and runtime observations (runtime observation).
Missing or unavailable information remains unknown and is not replaced with information from another layer; sensitive text is redacted.

### Save as an Inference Template

The body of `POST /api/v1/deployments/{id}/catalog-template` may be omitted, or be `{"include_env": ["KEY", ...]}`: none of the deployment's own environment variables are exported by default (they often hold tokens and host paths); only the listed keys go into the spec. Any signed-in user may export; saving the result into the catalog is still `POST /inference-templates/import`, which only administrators may call.

It returns 200:

```json
{
  "model_id": "allenai/olmo-2-0425-1b-instruct",
  "spec_name": "vllm-1xnvidia-h20-d12",
  "yaml": "# Saved from GPUWorks deployment \"olmo\" (id 12).\n# Verified on 1 x NVIDIA H20.\n...",
  "verified": true,
  "saveable": true,
  "checks": [{"id": "operation_reply", "operation": "openai.chat", "status": "pass", "code": "TEMPLATE_OPERATION_ANSWERED", "params": {"operation": "openai.chat", "endpoint": "/v1/chat/completions", "http_status": 200}, "message": "openai.chat answered with HTTP 200"}],
  "notes": [{"code": "verified_on", "params": {"gpu": "NVIDIA H20", "count": 1}}],
  "excluded_env": ["HF_TOKEN"],
  "evidence": "GPUWorks template test record\n..."
}
```

- `yaml` imports as it is; the comments at its top state the same facts as `notes`. The comments are in English; the UI localizes `notes` from their `code` and `params`.
- `checks`: one check per operation; every `id` is `operation_reply` and `operation` names the operation. Chat, completions, embeddings and System One decisions each get the same smallest request the health check sends: a 2xx answer is `status: "pass"` (`TEMPLATE_OPERATION_ANSWERED`), anything else is `fail` with `code` `TEMPLATE_OPERATION_HTTP_ERROR`, `TEMPLATE_OPERATION_NO_REPLICA`, `TEMPLATE_OPERATION_TIMED_OUT` or `TEMPLATE_OPERATION_REPLY_INCOMPLETE`, and the gateway's or engine's own words only in `evidence`. Video, image, speech and other operations get no request: `status: "skip"` with `code` `TEMPLATE_OPERATION_NOT_PROBED` (`TEMPLATE_OPERATION_NO_ENDPOINT` when no endpoint is declared), and an `operation_not_tested` note. `params` carries `operation`, `endpoint?` and `http_status?`; `message` is the English fallback.
- When a tested operation fails, `verified` and `saveable` are both `false` and `yaml` is for troubleshooting only.
- `notes` codes: `verified_on`, `local_weights` (`repo_id`), `operation_passed`, `operation_failed`, `operation_not_tested`, `kv_bytes_missing` when there is no `kv_bytes_per_token`, `model_config_missing`, `placement_omitted` (`gpu_count`, `gpu_memory_mib`), `vram_limit`, `env_excluded`, `parameters_dropped` (settings no engine flag carries, such as `cpu_cores`), `container_overrides_omitted`.
- `evidence` is the plain-text test record to attach when proposing the template upstream.

When the deployment cannot be written as a template, it returns 409 `TEMPLATE_EXPORT_UNAVAILABLE` with `params.reason`: `not_running` (the deployment is not running or has no running instance), `distributed` (multi-node), `pd`, `no_gpu_facts`, `template_assets`, `components` (the source template carries packaged files or component models), `weights_not_public` (an unsupported source such as a URL; registered local models use `local`), `revision_unpinned` (the weight revision is not a commit), `revisions_differ`, `template_missing`, `model_missing`, `launch_changed` (configuration or the running instance changed), `launch_snapshot_missing` (no complete verifiable launch record), `heterogeneous_gpus` (different card models), `command_overridden` (the deployment replaced the engine command). When the exported arguments cannot rebuild the original command it returns 409 `LAUNCH_NOT_EXPORTABLE`. An unknown deployment is 404, and a body that is not valid JSON is 400. How the template is built is described in [Save a deployment as an inference template](../how-to/save-deployment-as-template.md).

Local exports use `weights.sources: [{local: <source_id>}]` to reference registered weights, without bundling files or storing a host path. The `local_weights` note carries `repo_id`. Creating a deployment without that registration returns 400 `MODEL_SOURCE_UNAVAILABLE` with `params.reason=local_model_not_registered` and creates no empty model. See [local weight reuse](../how-to/save-deployment-as-template.md).

### Deployment List Filters and Response Fields

`q` matches the deployment name, served model name, and model name. `state` corresponds to the state displayed in the UI: `running`, `starting`, `pulling`,
`pending`, `offline`, `failed`, `stopped`, `deleting`, `sleeping`, or `asleep`; the deleting state takes precedence over runtime states.
A value must be one of these; anything else returns 400 `LIST_FILTER_INVALID` rather than silently matching everything.
`node` filters by node UUID in `runtime_nodes`. Comma-separated values match any selected node and intersect with `q` and `state` before sorting and pagination. Only actual runtime locations match, not placement targets; deployments without runtime nodes do not match. Unknown UUIDs return no results. `node=` disables this filter; an empty comma-separated item returns 400 `LIST_FILTER_INVALID`. Paginated responses also include `node_options: [{uuid, name}]`: actual runtime nodes from the complete deployment snapshot, deduplicated by UUID and sorted by name then UUID, independent of current filters and pagination. With no available nodes this is `[]`.

Sortable columns are `name`, `model`, `state`, `replicas`, and `created_at`; `sort=state` sorts by this same `state`.

`state` is the one state a row should display; `runtime_state` is not, because a sleeping engine is still running and stays `running` there
while the gateway no longer dispatches requests to it. The precedence is:

- A deployment being deleted reads `deleting`, ahead of any runtime state.
- Otherwise it equals `runtime_state`, with two exceptions that apply only while `runtime_state` is `running`: it reads `asleep` when the gateway refuses every request because the replicas it needs sleep, and `sleeping` when the sleep order has been accepted but at least one engine has not reported its level yet (see [Query Deployment Details](#查询部署详情)).
- `offline` is a value of `runtime_state` itself: no replica can serve (no running, non-headless instance on an online node whose multi-node generation is complete), while running instances remain on nodes that went offline.
- While a replica serves (a running, non-headless instance whose multi-node generation has every follower running; in every role, for a PD deployment), `runtime_state` is `running`: a new replica pulling or starting during a scale-out or rolling restart does not turn it into `starting` / `pulling`. The same holds when every replica sleeps: `state` stays `asleep`. A headless follower's state never keeps a replica alive: when the leader has failed, or an awake leader has a failed follower in the same generation, and no other replica serves, it reads `failed`. A sleeping leader is judged for sleep first, as in the gateway: even while a follower is not ready, `state` stays `asleep` / `sleeping` and `ready_replicas` stays 0.
- A deployment with only some replicas asleep still serves: `state` stays `running` and `asleep_replicas` says how many sleep; such rows also match `state=running`.

Success returns HTTP 200. In the outer response, `items` is an array of deployment objects, and `total` is the total number of matches after filtering (an integer).
When pagination is enabled, integer fields `page` and `page_size` are also included. An empty result is `items: []`; requests beyond the last page also return an empty array.

With pagination, each deployment returns only the following list fields. See [Query Deployment Details](#查询部署详情) for commonly used fields and the deployment route entries for the remaining descriptions:

| Field Group | Fields |
|---|---|
| Identity and creation time | `id`, `name`, `created_at` |
| Template and model | `template_id`, `template_deviated`, `engine`, `image`, `model_id`, `model_name`, `served_name` |
| Nodes | `node_uuid`, `node_name`, `node_names`, `runtime_nodes`, `runtime_node_names` |
| State and replicas | `desired_state`, `runtime_state`, `state`, `sleepable`, `asleep_replicas`, `sleep_level`, `replicas`, `ready_replicas`, `health`, `preparation` |
| Operations | `operations` |

The node fields fall into two groups with different meanings, and neither stands in for the other: `node_uuid`, `node_name` and `node_names` are the **placement target** — the node the deployment was asked to run on (the node an operator wrote for manual placement; absent when placement is automatic) — while `runtime_nodes` is **where it runs now**, taken from the instance rows themselves, the same source the GPU inventory attributes cards from, so both pages name the same machine for the same instances.
The two legitimately differ: automatic placement picks the node later, a retried replica is relaunched on another node, and a `spread` deployment runs its replicas on several at once.
`runtime_nodes` is an array of objects, one per node: `node_uuid`, `node_name`, and `instances` (how many instances the node holds).
The order is stable and deduplicated per node, and a multi-replica deployment lists every node rather than one of them. The whole field is omitted when no instance row names a node: nothing has been placed yet, or every replica is stopped.
`runtime_node_names` names the same nodes in the same order.

`state`, `asleep_replicas`, and `sleep_level` mean and carry exactly what the fields of the same name carry in the details (see [Query Deployment Details](#查询部署详情));
reading `runtime_state` alone misses sleep, because a sleeping engine still reports `running`.
When `asleep_replicas` or `sleep_level` is 0 the whole field is omitted, as in the details.

In a paginated list, `preparation` contains information about the first model file that is not yet ready in an active deployment; it is omitted if there is none.
Complete preparation information is in `model_progress` in the details.

Without pagination, the remaining deployment configuration fields are returned, but `instances` and `node_requirement_labels` are still excluded.
Paginated lists also omit details such as launch configuration and catalog selection snapshots. To retrieve instances or full details, query `GET /api/v1/deployments/{id}`.

### Query Deployment Details

`GET /api/v1/deployments/{id}` returns a deployment's configuration and current state and requires management authentication.
The request has no query parameters or body. The path `id` is a positive integer deployment ID obtained from `items[].id` in the list.

Success returns HTTP 200 with the deployment object directly as the response body, without an outer `item` or `items`.
If the deployment is not found, returns 404 with response `{"error":"..."}`. Clients should handle the HTTP status and display the reason in `error`; this endpoint has no dedicated stable error code.
Authentication failures are handled as described in [Authentication Model](#鉴权模型) above.

The following illustrates the key fields of a regular deployment, omitting other configuration and instance fields:

```json
{
  "id": 7,
  "name": "qwen3-chat",
  "served_name": "qwen3",
  "desired_state": "active",
  "runtime_state": "running",
  "replicas": 1,
  "ready_replicas": 1
}
```

| Key Field | Type | Meaning |
|---|---|---|
| `id`, `name` | Integer, string | Deployment ID and display name in the management UI |
| `served_name` | String | The `model` name to specify in inference requests |
| `model_id`, `model_name` | Integer, string | ID and name of the associated registered model |
| `desired_state` | String | Desired state: `active` (keep running), `stopped` (stopped), or `deleting` (being deleted) |
| `runtime_state` | String | Current runtime state: `pending`, `pulling`, `starting`, `running`, `offline`, `failed`, or `stopped`. Derived from instances and the desired state only, never from sleep: a sleeping engine is still `running` |
| `state` | String | The one state to show. Takes the values of the list filter's `state`: `deleting` first for a deployment being deleted; otherwise `runtime_state`, except that it reads `asleep` (every asleep replica's engine reports its level) or `sleeping` (the order is accepted and the gateway already refuses, but at least one engine has not reported its level) when the gateway refuses every request because the replicas it needs sleep |
| `asleep_replicas` (optional) | Integer | Replicas the gateway does not dispatch to because they sleep, across every role; it and `ready_replicas` are two counts of one set of replicas, and their sum is the number of replicas the gateway can reach. Omitted when 0 |
| `sleep_level` (optional) | Integer | The deepest sleep level an engine reports (1 or 2); omitted while every replica is awake and while a sleep order is still being carried out. An absent field is not awake: ask `state` whether it sleeps |
| `replicas`, `ready_replicas` | Integer | The deployment's replica count and the number of awake replicas with all ranks running. PD deployments sum the units of each role (`replicas` is the sum of `pd.roles[].desired`, `ready_replicas` the sum of `pd.roles[].ready`), the same pair the list row gives; `asleep_replicas` never exceeds `replicas` |
| `health` | Object | Health check status and latest results; see [Health Checks](#健康检查) |
| `instances` (optional) | Array of objects | Instances in the deployment; `uuid` identifies an instance, `state` indicates its state, and `node_uuid` and `gpu_indexes` indicate where it runs |
| `runtime_nodes` (optional) | Array of objects | The nodes the instances are on now, one entry per node: `{node_uuid, node_name, instances}`, with the meaning and value the same field has on a list row. `node_uuid`, `node_name` and `node_names` are the placement target, which can differ from it |
| `model_progress` (optional) | Array of objects | Preparation status of model files; `state` is the file state, `progress` is progress from 0–1, and `message` provides related information |
| `quick_params`, `extra_args` (optional) | Object, string | Saved launch configuration and extra arguments; see [Deployment Launch Parameter Sources](deployment-launch-sources.md) |
| `pd` (optional) | Object | PD deployment mode and replica counts and states for each role; see [PD Disaggregated Deployment](#pd-分离部署placementpd) |
| `node_replacement` | Object | How long a lost node is waited for before its replicas move, `{after_sec}`; default 600, 0 means never |
| `yield_policy` | Object | The yield policy and parameters, shaped like the response in [Yield policy](#出让策略): `policy` and the parameters that apply; see [Yield policy](../explanation/service-levels.md) |
| `sleep_hold` (optional) | String | How the asleep replicas are held, derived from each instance's `sleep_held`: `all` (every one held by an operator or a schedule, a request wakes none), `some` (some held, a request wakes only the others) or `none` (all asleep by the platform's choice, a request wakes them); omitted with no replica asleep |
| `sleep_held` | Boolean | Derived: `true` when the deployment has asleep replicas and every one is held (`sleep_hold` is `all`), so requests do not wake the deployment |
| `sleep_wakes_on_request` | Boolean | Whether a request can wake an operator's sleep that does not give `wake_on_request`: `true` when the deployment is sleepable and its yield policy is not `never`, otherwise `false` (a held sleep). The UI's sleep dialog takes it as the initial value of "Wake it when a request arrives"; a yielding wake applies the same rule to the occupants |
| `wake_waiting_since` (optional) | RFC3339 time | Present when a wake of the deployment is waiting for room; the value is when the wait began |
| `wake_wait_limit_sec` | Integer | How long a waiting wake waits before giving up, in seconds, currently 1800; the console's waiting hints use it and carry no number of their own |
| `yield_policy_eligible` | Object | Whether the deployment may hold a yield policy other than `never`: `{ok}`, and when not, a `reason`: `SLEEP_NOT_ENABLED` (sleep is not turned on), `SLEEP_ENGINE_UNSUPPORTED` (the engine does not support sleep) or `YIELD_POLICY_SHAPE` (a multi-node or PD deployment). The endpoint that sets a yield policy refuses by the same check, and the console uses it to disable policies and show the reason |
| `health_checkable` | Boolean | Whether the deployment's health check can be turned on: `true` when the model serves chat, completions, embeddings or System One decisions. `PUT /deployments/{id}/health-check` refuses to turn it on by the same rule, and the console only reads this to decide whether the switch can be turned on |
| `priority_scheduling` | Boolean | Whether the deployment asks its engine to schedule by request priority; a launch applies it only where the engine's arguments allow it (see [Request Priority](../explanation/request-priority.md)) |
| `schedule_capabilities` | Object | What the deployment's schedule may order: `sleep` and `wake` (each `{ok, reason?}`, `reason` being `SLEEP_NOT_ENABLED` or `SLEEP_ENGINE_UNSUPPORTED`; a deployment whose engine cannot sleep allows neither, and the console shows no sleep controls for it), and `replicas[]`: `{role?, minimum, maximum, ok, reason?}`, one entry for a single-node deployment (no `role`) and one per role in the mode's order for a PD deployment; `minimum`/`maximum` are the range of a scheduled replica count (`minimum` is 0 for a PD role its mode allows to be empty), and when `ok` is `false`, `reason` is why it cannot be scheduled (for example `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` for a multi-node or PD deployment placed by hand). Creating a schedule entry validates by the same rules. The deployment list does not carry these three fields |

`active` means the deployment is intended to run, not that it can already serve requests; also check its runtime state, ready replicas, and health checks.
Do not uniformly treat absent optional fields as `0` or empty arrays. Use the following fields to determine placement and retry status:

- `node_requirements` is always present and is `{}` when there are no requirements. Fixed nodes are stored in `gpuworks.ai/node`; deleted nodes remain in the requirements but cannot match.
  Details additionally return `node_requirement_labels[]`: `{key,value,display,display_code?,nodes}`. Nodes and networks display names; `nodes` is the current match count. A value of 0 does not mean the requirement is invalid, only that no nodes currently match. Lists do not include these display fields.
- `placement_strategy` is `spread` or `binpack`. When a regular single-node replica has no instance yet, `placement_status{unscheduled,reason,at}` provides the waiting count and latest reason: `pool_empty`, `pool_no_fit` or `waiting_for_yield` (waiting for deployments that share its cards to give them up by yield policy; see [Use Scheduling Analysis](../how-to/scheduling-center.md#新部署怎样放到共用的卡上)).
  Reason parameters include counts by category, up to 3 closest candidates, and rejection reasons; at most one event is recorded per waiting timeout window.
- `placement_mode` appears only on multi-node and PD deployments: `auto` means the platform picks the nodes (at creation from free cards, and again when a unit starts and no longer fits), `manual` means the operator wrote them; deployments that existed before the upgrade are all `manual`. Single-node deployments do not carry this field; their placement is in the node requirements (`placement_mode` and `node_uuid` in `GET /deployments/{id}/draft`).
- `max_replicas` is the upper limit when adjusting replica counts; `restart_requested_at` appears only while a rolling restart is in progress.
- For regular deployments, `retry_exhausted{index,failed_attempts,last_failure}` identifies replicas that have exhausted their retries; use `units/retry` to retry them. For multi-node deployments, `units[]` gives each replica's leader and `followers[]`. See [Adjust Replicas for Regular Deployments](#普通部署的副本调整) for adjustment methods.

For a request example, see [Query Details of One Deployment](api-conventions.md#3-查询一个部署的详情).

For a deployment with no instance, image preparation does not count as a scheduling timeout while its latest scheduling reason remains `image_delivery_waiting` and its image is actively pulling or syncing on a preparation target. Transfers for other images or nodes do not qualify. Completion, failure, cancellation or a changed scheduling reason restores the existing state derivation. Lists and details return 500 if these image facts cannot be read, rather than presenting an unknown state as failed.

### Deployment Sleep State (`state`, `asleep_replicas`, `sleep_level`)

A sleeping deployment is indistinguishable in `runtime_state`: the engine process is still running and the gateway simply refuses to dispatch to it.
The fields that answer "is this deployment asleep right now" are `state`, `asleep_replicas`, and `sleep_level`. They mean the same and carry the same values
in deployment details and in list rows (the paginated list's `items[]`), and the `state` filter matches the same `state`.

All three are **derived at read time**, never stored: the only persisted facts are each instance's ordered sleep intent and the `sleep_level` it reports,
and the judgement is the same one the gateway itself uses to refuse requests.

- **`asleep_replicas`**: replicas the gateway does not dispatch to because of sleep, across every role (PD deployments count each role's units).
  A replica is judged by its leader: if the leader is running and has been ordered to sleep (intent alone counts) or still reports a sleep level, the replica is asleep.
  So **in the seconds after the order is placed, while the engine is still swapping memory out, the replica already counts in `asleep_replicas`**, because it really is not receiving requests.
  It and `ready_replicas` are two counts of one set of replicas: their sum is the number of replicas the gateway can **reach**; a replica it cannot reach for another reason
  (stopped, failed, its node gone, a multi-node replica missing a rank) counts in neither. A replica down for another reason never reads as asleep.
- **`state`**: the precedence above. `asleep` and `sleeping` appear only when the gateway answers **no request at all** — some role it needs has no awake replica,
  and every role refusing it refuses because of sleep (it has sleeping replicas and none awake), not because it is down. A deployment with only some replicas asleep
  still serves: `state` stays `running`, the awake replicas keep receiving requests, and `asleep_replicas` says how many the rest are; such a row neither poses as
  entirely healthy nor as entirely asleep. `deleting` outranks all of the above.
- **Where `sleeping` ends and `asleep` begins**: it is `asleep` once every asleep replica's engine reports the level it was ordered to; a single engine not there yet makes it `sleeping`.
  Changing levels (say L1 to L2) goes through awake before sleeping again, and the row reads `sleeping` during it rather than an unchanging `asleep`, so the click visibly takes effect.
- **`sleep_level`**: the deepest level an engine reports (1 or 2). It is 0 (field omitted) while every replica is awake; a sleep order not yet carried out also has no value —
  **do not read an absent field as "not asleep"**. Ask `state` and `asleep_replicas` whether it sleeps; `sleep_level` only answers how deep.
- **A deployment with nothing left is not asleep**: with no serving replica at all (instances stopped or failed) there is nothing to wake, so `state` gives the failure's or
  stopped state itself and `asleep_replicas` is 0, even if a sleep intent remains on the instances.
- **PD deployments**: `pd.roles[].asleep` is the same replicas split by role (`ready` + `asleep` is the role's units the gateway can reach).
  A role the mode runs without and that is entirely asleep (vLLM NIXL's prefill, for example) does not count as refusing service; the deployment reads
  `asleep`/`sleeping` only when a role that must exist, such as decode, is entirely asleep.

For the instance-level sleep fields (`instances[].asleep`, `instances[].sleep_level`, `instances[].desired_sleep`,
`can_sleep_alone` / `can_wake_alone`), see the [Deployments and Instances route entries](#deployments--instances部署与实例).
For the procedure, see [Sleep and Wake Operations](../how-to/sleep-wake.md).

### Query the Candidate Pool

`GET /api/v1/deployments/{id}/candidates`

Provides the deployment candidate pool's live state. From the perspective of the "next replica," each cluster node is categorized by the first check it fails (`room`: appears to have free GPUs; `busy`: GPUs occupied by awake instances; `unmet`: does not meet node requirements; `cordoned`: scheduling stopped or eviction in progress; `offline`: offline). `counts` covers all nodes and sums to `nodes`; `unmet_by_key` counts nodes by the first unmet requirement key. A node that meets the requirements but has no card that can run the launch (host runtime mismatch, or the accelerator does not support a requested feature) is also `unmet`, counted under the `accelerator` key; its item carries a structured `reason` (`code` and `params`, rendered by the client in the reader's language) instead of `detail`.

`items` contains one page of details (candidates include `rank` in replica attempt order, followed by nodes with unmet requirements and offline nodes). Filter with `?status=` and paginate with `?offset=` and `?limit=` (1–200, default 50). Invalid parameters return 400 `FIELD_INVALID`.

This endpoint does not simulate placement. Whether nodes that "appear to have free GPUs" actually have enough capacity is determined by the controller's simulation; see `placement_status` in deployment details.

### Edit a Deployment

`GET /api/v1/deployments/{id}/draft` returns `{"deployment": {...}, "revision": "...", "locked": [...]}`:

- `deployment` has the same shape as the `POST /api/v1/deployments` request. `quick_params` is the complete launch configuration the deployment stores. A manual placement of a single-node deployment is written as `placement_mode: "manual"` with `node_uuid`, and the vendor as `target_vendor`; neither appears in `node_requirements`.
- For a template deployment, `model_source.provider` is the weight source the deployment mounted, provided the template spec declares it. A spec is satisfied by any ready copy of its repository, and a model imported into a storage has the source `local`, which is not a choice the spec offers: the draft then states no `provider` and the evaluation picks the ready copy again (stating it would have the evaluation refuse it with `ACQUISITION_CANDIDATE_STALE`, and the field cannot change in an edit).
- `revision` identifies the configuration the draft was read from. Send it back as `base_revision` when editing.
- `locked` lists the request fields this deployment cannot edit: model, template, engine, vendor, name and served model name, weight source, and node count. Multi-node and PD deployments also lock the per-rank nodes, replica count, dGPU quota, and related fields (change them with [Replica Adjustment for Regular Deployments](#普通部署的副本调整) and the PD API); a PD deployment's `placement` is locked too. To change these fields, clone the deployment.

`PUT /api/v1/deployments/{id}` request body:

```json
{"deployment": {...}, "base_revision": "...", "apply": "rolling", "start": false, "wake": false}
```

`deployment` is the modified draft. A `locked` field, when stated, must match the draft (omitted means unchanged); otherwise the response is 400 `DEPLOYMENT_FIELD_NOT_EDITABLE` (`params.field`). Other fields are validated by the create rules (parameter dictionary, node requirements, health checks, the "never fits" check `DEPLOYMENT_NEVER_FITS`, and so on). While the yield policy is not `never`, `sleepable` cannot be turned off (yielding is sleeping): 400 `YIELD_POLICY_NEEDS_SLEEP` (`params.policy`); set the policy back to `never` with `PUT /deployments/{id}/yield-policy` first.

The launch configuration starts from the configuration the deployment stores and does not reapply the template: values sent back unchanged from the draft stay as they are, and only changed values are checked against the parameter dictionary's ranges. Template changes made after the deployment are not brought in by an edit, and dictionary defaults the deployment never had are not added.

`?dry_run=1` returns only the plan and writes nothing:

| Field | Description |
|---|---|
| `changes[]` | Per-field changes `{field, layer, from, to}`, named as in the request (launch parameters are `quick_params.<key>`, container parameters `container_overrides.<key>`). Environment variables are listed one by one as `container_overrides.env.<NAME>` with only `masked: true` and no values |
| `changes[].layer` | `live`: takes effect at once (replica count, routing, health check, tokenize, start timeout, node requirements and placement strategy); `restart`: read when an instance starts (launch parameters, extra arguments, image, container parameters, parallelism, sleep capability, dGPU quota), so running instances keep the old value until they are replaced |
| `restart` | Whether any change is in the `restart` layer |
| `apply_options`, `default_apply` | The apply modes available in the current state and the default; see the table below |
| `can_start` | The deployment is stopped and can be started with the edit (`start: true`) |
| `can_wake` | The deployment is under a sleep order that `apply: "all"` can withdraw at the same time (`wake: true`), so the replacement instances start awake |
| `apply_note` | Why the apply modes are fewer than the deployment's state alone allows; `partition_change` is described below |
| `revision` | The configuration the plan was judged against |

| Deployment state | `apply_options` | Default |
|---|---|---|
| Partition shape changes (dGPU turned on or off, another partition backend), `apply_note` is `partition_change` | `all`, `none` | `all` (`none` under a sleep order) |
| Running with at least two replicas, at least one serving | `rolling`, `all`, `none` | `rolling` |
| One replica, or no replica serving (starting, failed) | `all`, `none` | `all` |
| Under a sleep order | `none`, `all` | `none` |
| Stopped | `none` | `none` |

- `rolling` is the rolling restart of [Restart Strategies](#重启策略): replicas are replaced one at a time, failed attempts are retired, and retry counts are reset.
- `all` replaces every instance at once and interrupts service; failed attempts are also retired.
- `none` only writes the configuration, which takes effect the next time an instance starts.
- When the partition shape changes, every old instance must exit before the new ones start (dGPU caches and card claims are recorded per instance, so the old and new generations cannot coexist). The replicas therefore cannot be replaced one at a time, and service is interrupted until the new instances are ready. Resizing a dGPU slice alone does not change the partition shape and can roll.
- When only `live` changes are present, no instance restarts, and `apply` must be omitted or `none`.

An `apply` not in `apply_options`, `start` on a deployment that is not stopped, or `wake` without a sleep order or without `all` returns 400 `DEPLOYMENT_EDIT_APPLY_INVALID` (`params.apply`, `params.options`).

Without `dry_run`, the configuration, node requirements, and apply mode are written in one transaction, and the response is 200 `{"plan": {...}, "deployment": {...}}`, where `plan.revision` is the new revision after the edit. When nothing changes, the response returns at once without writing or recording anything.

Conflicts and limits:

- A missing `base_revision` returns 400 `FIELD_INVALID`. A `base_revision` that does not match the current configuration (the deployment changed after the draft was read), or a configuration, node requirement, or desired state that changed while writing, returns 409 `DEPLOYMENT_CHANGED`. State the controller advances on its own, such as rolling restart progress or the sleep level, is not a conflict.
- When a restart is needed, shared storage is checked as by the restart API; unreachable storage returns 409.
- A deployment being deleted returns 409 `DEPLOYMENT_DELETING`; a missing one returns 404.

The timeline records `DEPLOYMENT_EDITED` with `fields` (the changed fields, comma-separated), `count`, and `apply`.

`POST /api/v1/evaluate` accepts `deployment_id`: the evaluation is treated as an edit of that deployment, so placement does not count the GPU memory its existing instances hold, and its own instances are not suggested for sleep.

### Modify Node Requirements

`PUT /api/v1/deployments/{id}/requirements`

Replace the node requirements and placement strategy of a regular single-node deployment without redeploying: `{"node_requirements": {...}, "placement_strategy": "spread"}` (omit the strategy to retain it). Affects only replicas started afterward; running replicas are not migrated (restarting a replica places it again according to the new requirements).

The vendor is determined by the deployment image: omitting it retains the current vendor; specifying a different vendor returns 400 `NODE_REQUIREMENTS_INVALID`. Requirements are validated as during creation. The same "never fits" check as at creation applies: if no node under the new requirements can fit one replica even when idle, returns 400 `DEPLOYMENT_NEVER_FITS` and leaves the original requirements unchanged.

Multi-node and PD deployments list nodes per rank and return 400 `NODE_REQUIREMENTS_NOT_EDITABLE` (`params.shape` is `multi_node` or `pd`). Returns 409 `DEPLOYMENT_DELETING` if deletion is in progress. Success returns deployment details.

### Adjust the GPU Memory Limit

Example `PUT /api/v1/deployments/{id}/dgpu` request: `{"vram_limit_gib":12}`. Applies only to single-node dGPU deployments.
New instances use the new limit; running instances are adjusted immediately without restarting where possible. Returns 200 `{deployment,live,pending}`:

- `live`: UUIDs of instances where the change takes effect immediately.
- `pending`: `{instance_uuid,reason,used_bytes}` for instances that require a restart for the change to take effect.
  `reason` is `backend` (the backend reads it only at startup), `in_use` (usage exceeds the new limit), `starting`, `asleep`, or `no_usage`.

Call restrictions:

- Full-GPU deployments: 400 `DGPU_QUOTA_NOT_PARTITIONED`; multi-node/PD: 400 `DGPU_QUOTA_NOT_EDITABLE` (`params.shape`).
- Non-positive limit: 400 `DGPU_QUOTA_INVALID`; no node can fit the deployment with the new limit: 400 `DEPLOYMENT_NEVER_FITS`.
- When increasing the limit, the GPU still cannot accommodate it after excluding the instance's own usage: 409 `DGPU_QUOTA_NO_ROOM` (`params.node`, `gpu`, `free_gib`, `need_gib`).
- Deployment is being deleted: 409 `DEPLOYMENT_DELETING`.

The timeline records `DEPLOYMENT_DGPU_QUOTA_CHANGED`, including `from_bytes`, `to_bytes`, `live`, and `pending`.

### Suggest Nodes for a Multi-Node Replica

`POST /api/v1/deployments/{id}/units/suggest`

Suggest nodes for one more replica of a multi-node deployment. You confirm the suggestion in "Adjust Replicas" and save it with the list (this endpoint writes nothing). The request body `{"units": [...]}` lists replicas already in the editor (their leaders cannot serve as leaders again; replicas that do not yet exist in the deployment occupy these nodes according to the GPU count per rank).

Suggested nodes share the same compute network and GPU architecture, each have enough free GPUs for one rank, are schedulable and online, and meet the deployment's node requirements. Each rank is evaluated using the deployment's saved configuration; at most the best 3 groups are considered, with 3 nodes tried per rank.

Returns `{"unit": {"node_uuid", "followers"}, "network": {"id","name"}, "arch", "nodes": [{"uuid","name","free_gpus","model"}]}`. If placement is not possible, `unit` is `null`, and `reason.code` is `unit_none` (node counts excluded by category and the maximum number of nodes that can form a group) or `unit_no_fit` (the first rejected node and its reason). Non-multi-node deployments return 400 `REPLICAS_BODY_SHAPE`.

### Restart Strategies

`POST /api/v1/deployments/{id}/restart` applies to all deployment shapes. It recreates the deployment using the current network and image, resets each replica's retry count, and retires failed attempts.
Omit the request body or submit `{"strategy":"rolling"}` / `{"strategy":"all"}`:

- `rolling` (default): Replace replicas one at a time. The old replica enters `draining` and exits after finishing existing requests, while replacement instances start; the next replica is processed after service recovers.
  A regular multi-node replica reuses the leader's master port, so replacement instances must wait for the old generation to exit before starting on the same node.
  Returns `restart_requested_at` while in progress and records `DEPLOYMENT_ROLLING_RESTARTED{replicas}` in the timeline.
- `all`: Immediately stop all instances, then recreate them together, interrupting service in the meantime. Records `DEPLOYMENT_RESTARTED{instances}`.

An unknown strategy returns 400 `RESTART_STRATEGY_INVALID`; a non-active deployment returns 409 `DEPLOYMENT_NOT_ACTIVE`.

### Waiting Wakes and Yielding Wakes

`?wait=true` and `?yield=true` on `POST /api/v1/deployments/{id}/wake` and `POST /api/v1/instances/{uuid}/wake`:

- `wait`: replicas that fit wake first and the rest wait for room; the server wakes them automatically once capacity frees up, and gives up after 30 minutes (timeline `WAKE_WAIT_TIMEOUT`). The start of the wait is kept on the instance, and the deployment's `wake_waiting_since` is the earliest of them. The answer 202 `waiting` also clears the held sleep of the waiting instances (the instance's `sleep_held`): they are meant to be awake, so a request that arrives while it waits is held by the gateway for that wake (and the residency arbiter may make room for it under the yield policies) instead of getting a 503; before, the hold stayed until the wake landed.
- `yield`: first puts the awake deployments holding the needed GPUs to sleep (`yield_level` is 1 or 2, default 1; 1 is the yield policy's `auto`: when the occupant's host memory cannot hold L1 it switches to 2 automatically and is not refused for it), then waits. When the occupant's yield policy is not `never` this sleep can be woken by a request (it is not held), and an occupant that is `never` is held asleep (the same rule as `sleep_wakes_on_request`). The occupant's sleep is recorded as `YIELDED_FOR_WAKE` with `for_deployment` (whom it gave way to) and `wake_on_request`, and it counts as a yield in the scheduling history and the statistics.
- When everything fits the response is 200 `{"status":"waking","woken":[…],"waiting":[],"yielded":[],"yielded_held":[]}`; when some must wait it is 202 `{"status":"waiting",…}`, where `waiting` lists the instance UUIDs waiting and `yielded` the names of the deployments put to sleep, and `yielded_held` those of them held asleep.
- When an occupant cannot sleep (a deployment that is not sleep-capable, the last awake role of a PD deployment, or a process outside the platform) the response is 409 `WAKE_YIELD_BLOCKED` (`params.occupants` names, `woken`) and nothing changes; a `yield_level` other than 1 or 2 returns 400 `SLEEP_INVALID_LEVEL`.

A wake without these parameters behaves as before. For usage see [Sleep and Wake Operations](../how-to/sleep-wake.md#放不下时等位与让位).

### Yield Policy

`PUT /api/v1/deployments/{id}/yield-policy` sets how a deployment gets along with the other deployments that share its cards. The yield policy is the only thing to choose. For the rules see [Yield Policy: How Deployments Share a Set of GPUs](../explanation/service-levels.md), and for the steps see [Use Scheduling Analysis](../how-to/scheduling-center.md#出让策略). The body:

```json
{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}
```

Every key but `policy` may be omitted and then takes the policy's default; inapplicable keys are ignored (so is `idle_neighbours`), so a response can be sent back as a request body as it is.

| Field | Values | Applies to |
|---|---|---|
| `policy` | `never` never yield automatically, `idle` yield when idle, `timeslice` yield on a turn, `anytime` yield anytime; from the highest rank to the lowest | Always required |
| `idle_sec` | 15–604800 s: how long without a request counts as idle; once idle this long the deployment yields if another one waits for its cards; with `timeslice` it may not exceed `max_serve_sec` | `idle`, `timeslice`, `anytime` |
| `sleep_level` | `auto` (L1, falling back to L2 when host memory cannot hold the weights), `l1` (strict L1, refused where it does not fit), `l2`; applies to every sleep the platform chooses for the deployment (a yield); sleeps ordered by an operator or a schedule are unaffected | A policy other than `never` |
| `max_serve_sec` | 30–86400 s: when taking turns with another `timeslice` deployment, how long one wake may serve before giving way | `timeslice` |
| `unfinished` | `abort` cuts them off at once; `wait` lets them finish, for at most `unfinished_wait_sec` seconds, then cuts off what is left | `timeslice`, `anytime` |
| `unfinished_wait_sec` | 1–600 s | `unfinished` is `wait` |
| `hold_max_sec` | 10–3600 s: how long the gateway holds a request for the deployment to wake; on timeout 503 with `Retry-After` | A policy other than `never` |

The defaults of each policy:

| Policy | Defaults |
|---|---|
| `never` | No parameters |
| `idle` | `idle_sec` 60, `sleep_level` `auto`, `hold_max_sec` 300 |
| `timeslice` | `idle_sec` 30, `max_serve_sec` 120, `unfinished` `wait` with `unfinished_wait_sec` 30, `sleep_level` `auto`, `hold_max_sec` 600 |
| `anytime` | `idle_sec` 30, `unfinished` `abort`, `sleep_level` `auto`, `hold_max_sec` 3600 |

Success returns 200 with the stored policy, with inapplicable parameters absent. The `yield_policy` in deployment details has the same shape. Example responses:

```json
{"policy":"never"}
{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300}
{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}
{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","hold_max_sec":3600}
```

Setting `idle` while the cards already hold another `idle` deployment is still 200, and the response carries one more field, `idle_neighbours` (an array of the other deployments' names): while both `idle` deployments are busy neither yields, so one side's requests are held by the gateway until the other is idle, or until they reach `hold_max_sec` and get a 503; to make them take turns, use `timeslice`.

Errors:

| Case | Response |
|---|---|
| `{}`, or parameters without a `policy` | 400 `YIELD_POLICY_INVALID`, with `params` of `{"field":"policy","reason":"required"}` |
| An invalid value | 400 `YIELD_POLICY_INVALID`; `params.reason` is `invalid_value` and `params.field` is `policy`, `sleep_level`, or `unfinished` |
| A number out of range | 400 `YIELD_POLICY_INVALID`; `params.reason` is `out_of_range` and `params.field` is `idle_sec`, `max_serve_sec`, `unfinished_wait_sec`, or `hold_max_sec`, with `params.minimum` and `params.maximum`; when the `idle_sec` of a `timeslice` deployment exceeds `max_serve_sec`, the `maximum` of `idle_sec` is `max_serve_sec` |
| The policy is not `never` and the deployment is not sleep capable / the engine does not support sleep | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |
| A multi-node or PD deployment with a policy other than `never` | 400 `YIELD_POLICY_SHAPE` |

When creating a deployment (`POST /api/v1/deployments`), `yield_policy` takes the same request object and defaults to `never` when omitted; validation is the same.

`GET /api/v1/scheduling/yield-policies` (JWT, read-only) is the dictionary of policies, from which a client edits without keeping its own copy of the defaults or ranges:

```json
{
  "policies": [
    {"policy": "never", "defaults": {"policy": "never"}, "applies": []},
    {"policy": "idle", "defaults": {"policy": "idle", "idle_sec": 60, "sleep_level": "auto", "hold_max_sec": 300}, "applies": ["idle_sec", "sleep_level", "hold_max_sec"]},
    {"policy": "timeslice", "defaults": {"policy": "timeslice", "idle_sec": 30, "sleep_level": "auto", "max_serve_sec": 120, "unfinished": "wait", "unfinished_wait_sec": 30, "hold_max_sec": 600}, "applies": ["idle_sec", "sleep_level", "max_serve_sec", "unfinished", "unfinished_wait_sec", "hold_max_sec"]},
    {"policy": "anytime", "defaults": {"policy": "anytime", "idle_sec": 30, "sleep_level": "auto", "unfinished": "abort", "unfinished_wait_sec": 30, "hold_max_sec": 3600}, "applies": ["idle_sec", "sleep_level", "unfinished", "unfinished_wait_sec", "hold_max_sec"]}
  ],
  "applies_when": {"unfinished_wait_sec": {"unfinished": ["wait"]}},
  "bounds": {
    "hold_max_sec": {"minimum": 10, "maximum": 3600},
    "idle_sec": {"minimum": 15, "maximum": 604800, "at_most": "max_serve_sec"},
    "max_serve_sec": {"minimum": 30, "maximum": 86400},
    "unfinished_wait_sec": {"minimum": 1, "maximum": 600}
  },
  "sleep_levels": ["auto", "l1", "l2"],
  "unfinished": ["abort", "wait"]
}
```

| Field | Meaning |
|---|---|
| `policies[]` | From the highest rank to the lowest; `defaults` are the values filled in when a policy is chosen (every policy to which `unfinished` applies carries `unfinished_wait_sec`, for a client that turns waiting on); `applies` names the parameters that can apply, in the order an editor lists them |
| `applies_when` | The parameters that apply only when another parameter has certain values: `{parameter: {other parameter: [values…]}}`; currently only `unfinished_wait_sec`, when `unfinished` is `wait`. A parameter that is in `applies` and not named here always applies. The editor reads it and does not keep the rule itself |
| `bounds` | The range of each numeric parameter, in seconds; `at_most` names another parameter it may not exceed when both apply |
| `sleep_levels`, `unfinished` | The values of the two enumerated parameters |

Without a token the answer is 401, and other methods such as `POST` are 405.

The `trigger` of the timeline event `AUTO_SLEEP` is `schedule` (a schedule) or `preempt` (yielding cards, with `reason` `idle` / `rank` / `turn`); `level` is always the level the platform actually chose (1 or 2). For event codes see [Use Scheduling Analysis](../how-to/scheduling-center.md#时间线事件).

### Schedules

`/api/v1/deployments/{id}/schedule` manages a deployment's schedule. A schedule entry:

```json
{"id":3,"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T14:00:00Z"}
```

| Field | Meaning |
|---|---|
| `action` | `sleep`, `wake`, or `replicas` |
| `weekdays` | Array, 0 for Sunday through 6 for Saturday, at least one, no repeats |
| `time` | `HH:MM`, 24-hour |
| `timezone` | An IANA time zone name; an empty string uses the server's time zone |
| `sleep_level` | Required for `sleep`, 1 or 2; not accepted for other actions |
| `replicas` | Required for `replicas`, 1 to `max_replicas` (1 to 32 for a PD role, 0 allowed for a role the mode lets be empty); single-node deployments and automatically placed multi-node and PD deployments; not accepted for other actions. The response always states it for a `replicas` entry (0 included) and omits it for the others |
| `role` | The PD role a `replicas` entry scales (a role of its mode, such as `prefill` or `decode`): required for a PD deployment, not accepted for any other deployment or action (400 `SCHEDULE_INVALID`, `params.field` `role`); omitted from the response of an entry that is not PD |
| `enabled` | May be omitted in a request, default true |
| `next_at` | Response field, the next time it runs; omitted when disabled, and also when the server does not know the time zone (it is refused at creation, so this only happens when the server later loses that zone's data); the evaluator does not run such an entry either, so the two agree |

Each occurrence runs once; after a server outage only the newest missed occurrence runs, and only if it is within 12 hours; manual operations are not overridden; a new or edited entry takes effect from its next occurrence. See [Use Scheduling Analysis](../how-to/scheduling-center.md).

### Scheduling Board

`GET /api/v1/scheduling/board?page=1&page_size=20&q=` is read-only. The response (there is no `recent`; read the [scheduling history](#调度记录) for the latest automation events):

| Field | Meaning |
|---|---|
| `nodes` | A paged object (`items`, `page`, `page_size`, `total`) with nodes sorted by name; `q` matches part of the name, hostname or UUID (case-insensitively, as the node list does). Each node has `uuid`, `name`, `online`, and `gpus[]`: `index`, `name`, `memory_bytes`, `awake_bytes`, `sleeping_bytes`, `free_bytes`, `state` (the card's state, with the values of `states` in the [Scheduling Overview](#调度总览): `offline`, `unhealthy`, `held_outside`, `awake`, `asleep`, `free`, derived by the server), `held_outside`, `switching` (an instance on the card is starting, going to sleep, waking or draining), and `occupants[]` (`deployment_id`, `deployment`, `yield_policy`, `instance`, `state`: `awake` or `asleep`, `sleep_level`, `claim_bytes`, `residue_bytes`) |
| `pending` | Active deployments refused by the scheduler in the last 30 minutes: `deployment_id`, `deployment`, `reason{code,params}`, `at` |
| `waiting_wakes` | Wakes waiting for room: `deployment_id`, `deployment`, `instances`, `since` |
| `upcoming` | Schedule actions in the next 24 hours: `deployment_id`, `deployment`, `schedule_id`, `action`, `sleep_level`, `replicas` (only for a `replicas` action, 0 included), `role` (a `replicas` action of a PD role), `at` |

`?node=<uuid>` reads only that node: `nodes` is a one-row page (`page` and `page_size` 1, `total` 1; `page`, `page_size` and `q` are ignored) and `pending`, `waiting_wakes` and `upcoming` are empty arrays; an unknown node returns 404. The console reads it when a node's drawer is open, at a cost independent of the cluster size. `?queues=1` is the opposite: it answers only `pending`, `waiting_wakes` and `upcoming`, with an empty `nodes` page, and reads no node.

For timeline event codes see [Use Scheduling Analysis](../how-to/scheduling-center.md#时间线事件) and [Reschedule Replicas on Node Loss](../how-to/node-failover.md#时间线事件).

### Scheduling Overview

`GET /api/v1/scheduling/overview` is read-only and returns every card of every node of the cluster at once, each as the few small integers a heatmap needs and without the detail of its occupants (read that with `GET /scheduling/board?node=`). For 1000 nodes of 8 cards and about 3000 instances the response is about 290 KB (about 25 KB gzipped).

```json
{
  "summary": {
    "nodes": 3, "online_nodes": 2, "cards": 6,
    "awake": 2, "asleep": 1, "free": 1, "held_outside": 1, "unhealthy": 0, "offline": 1,
    "contention": {"waiting_wakes": 1, "held_requests": 3, "yields_last_hour": 1}
  },
  "states": ["offline", "unhealthy", "held_outside", "awake", "asleep", "free"],
  "models": ["L20", "H100"],
  "deployments": [{"id": 7, "name": "qwen", "yield_policy": "never"}, {"id": 9, "name": "night", "yield_policy": "idle"}],
  "nodes": [{
    "uuid": "…", "name": "gpu-a", "online": true, "labels": {"zone": "a"}, "contention": 5,
    "cards": [[0, 0, 49152, 100, 0, 3, 0, [0]], [1, 0, 49152, 25, 4, 3, 1, [2, 1]]]
  }]
}
```

| Field | Meaning |
|---|---|
| `summary` | The card counts of the whole cluster. A card is counted once, in the first bucket that holds, in this order: `offline` (its node is not online), `unhealthy` (the card is faulty or unusable because of ownership or health), `held_outside` (a process outside the platform uses it), `awake` (an instance holds memory on it), `asleep` (only sleepers' residue is on it), `free`; the six buckets add up to `cards` |
| `summary.contention` | `waiting_wakes`: deployments with a sleeping instance waiting for room to wake; `held_requests`: requests the gateway holds for a wake right now; `yields_last_hour`: sleeps in the last hour that made room for another deployment. A yield is counted one way: the residency arbiter's preemption (`AUTO_SLEEP` with `trigger` `preempt`) and an operator's yielding wake (`YIELDED_FOR_WAKE`) both count, and the timeline's `yielded` and `yielded_to` count the same events; a yielding wake records `for_deployment` too |
| `states` | The names of the card states, in the order of the disjoint buckets above: `offline`, `unhealthy`, `held_outside`, `awake`, `asleep`, `free`; a card refers to one by its index. Which state a card is in is decided by the server (the board's `gpus[].state` is the same result); a client only maps it to colour and words and does not work it out again |
| `models`, `deployments` | The dictionaries the cards refer to: GPU model names, and the deployments that occupy a card (`id`, `name`, `yield_policy`: `never`, `idle`, `timeslice`, or `anytime`), each in order of first appearance |
| `nodes[]` | Sorted by name. `labels` are the node labels an operator wrote (omitted when none); `contention` is the number of the node's sleepers waiting to wake, plus the requests held for deployments asleep on it, plus the yields of the deployments that have cards on it (omitted when 0) |
| `nodes[].cards[]` | The array `[index, model, memory_mib, awake_pct, sleeping_pct, state, switching, occupants]`: `index` is the card's device index on the node; `model` an index into `models`; `memory_mib` the memory in MiB; `awake_pct` / `sleeping_pct` the shares of the memory claimed by awake instances and kept by sleepers (0 to 100, a non-zero share is never rounded to 0, and the two never add up to more than 100); `state` an index into `states`; `switching` 1 when an instance on the card is switching (starting, going to sleep, waking, draining), otherwise 0 (it replaces the old `flags` bits); `occupants` is left out when empty (the array then has only 7 elements), otherwise a list of integers `index * 2` (`+ 1` when asleep) where the index is into `deployments` |

Only the cards the node owns and the vendor tool numbers are listed, as on the board.

The response comes from a cache in the server's memory and is not recomputed for each request: events of nodes, instances and deployments invalidate it, but it is rebuilt at most every 2 seconds (nodes report status often), and with no events every 10 seconds to pick up the requests the gateway holds and the hour of yields. A rebuild reads the nodes once, the live instances once and the deployments once, and charges all the cards in one pass with the same ledger arithmetic as the board, not node by node. The response carries an `ETag` (a hash of the content), and a request with a matching `If-None-Match` gets a 304.

### Residency Timeline

`GET /api/v1/scheduling/timeline?node=<uuid>&node=<uuid>&window=6h` is read-only. It is separate from the board because the board refreshes every few seconds while the lanes read the event log over a window; the console reads it once per opened node and chosen window, and refreshes it about every minute. With `node` the window is `15m`, `1h`, `6h` or `24h` (default `6h`), or a custom range read with `since` and `until` (RFC 3339, `until` defaulting to now, at most 7 days); a custom range is fixed, so the console reads it once and does not refresh it, while quick windows refresh every minute. `?sharing=1&window=6h` has no `node`: it answers only the stats of the cluster's deployments whose yield policy is not `never`, and of those deleted since that took part in a yield in the window (`deployments[]`, `nodes` an empty array), over `15m`, `1h`, `6h` or `24h` (default `6h`), cached for about 30 seconds; the console uses it for the "Deployments sharing cards" table. Neither read takes the stats from the event log: `sharing=1` gives each deployment's figures across the cluster, a `node` read counts only its replicas on those nodes, and the response's `scope` says which; the windows offered come in the response's `windows`, which the console shows rather than keeping a list of its own. The older `hours=6|24` still works and means `window=6h|24h`. The response:

```json
{
  "scope": "node",
  "window": {"from": "2026-10-28T06:00:00Z", "to": "2026-10-28T12:00:00Z", "hours": 6, "key": "6h", "seconds": 21600, "custom": false},
  "windows": [{"key": "15m", "seconds": 900}, {"key": "1h", "seconds": 3600}, {"key": "6h", "seconds": 21600}, {"key": "24h", "seconds": 86400}],
  "nodes": [{
    "uuid": "…", "name": "host2",
    "gpus": [{
      "index": 0, "truncated": false,
      "segments": [
        {"deployment_id": 7, "deployment": "qwen", "instance": "…", "state": "awake",
         "from": "2026-10-28T06:00:00Z", "to": "2026-10-28T09:00:00Z"},
        {"deployment_id": 7, "deployment": "qwen", "instance": "…", "state": "asleep", "level": 2,
         "from": "2026-10-28T09:01:00Z", "to": "2026-10-28T12:00:00Z"}
      ]
    }]
  }],
  "deployments": [{
    "deployment_id": 7, "deployment": "qwen", "yield_policy": "timeslice",
    "awake_seconds": 10800, "awake_share": 0.5, "present_seconds": 21600,
    "switches": 3, "yielded": 1, "yielded_to": 1, "wakes": 2,
    "held_requests": 4, "wait_p50_ms": 4200, "wait_p95_ms": 31000,
    "cards": [{"node_id": 3, "node_uuid": "5f0c…", "node": "gpu3", "gpus": [0]}]
  }],
  "switches_available": true
}
```

The `window` of a custom range is `{"from": "2026-10-28T08:30:00Z", "to": "2026-10-28T10:00:00Z", "hours": 1, "key": "custom", "seconds": 5400, "custom": true}`.

| Field | Meaning |
|---|---|
| `scope` | What the figures in `deployments[]` count: `node` for a `node` read (only the deployment's replicas on those nodes), `fleet` for `sharing=1` (all its replicas); the console shows its "these figures count only this node's replicas" hint from it and does not work it out |
| `window` | The range actually answered: `key` (one of `windows`; `custom` for a custom range), `custom` (whether it is a custom range), `seconds` (its length), `hours` (its whole hours, 0 under an hour), `from`, `to` (now for a quick window, the requested `until` for a custom range) |
| `windows` | The windows this read offers (`key`, `seconds`), in the order the console shows them: `15m`, `1h`, `6h`, `24h` for both reads; a custom range is not among them |
| `switches_available` | Whether `switches`, `yielded`, `yielded_to` and `wakes` in `deployments[]` are known. They come from the scheduling activity's memory, which keeps the last 24 hours only, so a `node` range that begins earlier answers `false`: the four are 0 without meaning zero, and the console shows a dash; the other figures are unaffected. Quick windows and `sharing=1` are always `true` |
| `nodes[].gpus[]` | The same cards as the board (`index`); `segments[]` run oldest to newest, at most 200. When a card changed hands too often to fit, the newest 200 segments are kept and the earlier time is summarized in `summary[]`: `summarized_before` is where summaries and segments meet (the summaries cover the window start up to this instant and `segments[]` begin at it); a segment that runs across it is split there, the part before going into the summaries and the part after staying a segment; `truncated` true means some earlier history is not listed segment by segment (it is always true when `summarized_before` is present). Without a summary, `summarized_before` and `summary` are absent and the card has the shape it always had; a client that does not know `summary` reads only the newest `segments[]` and `truncated`, as before. A card lists at most 200 segments plus 50 summaries |
| `summary[]` | One summarized stretch, the time from the window start to `summarized_before` cut into at most 50 equal slices (`kind` is always `summary`; segments carry no `kind`): `from`, `to`; `switches`, the times the card changed hands in the slice; `dominant_deployment_id`, `dominant_deployment`, the deployment awake longest in the slice (the lower id on a tie, absent when none was awake; kept for older clients, the console no longer reads it and draws each deployment's own `state` instead); `deployments[]`, each deployment that held the card in the slice with the seconds it spent in `awake_seconds` (serving), `asleep_seconds` (any level), `switching_seconds` and `starting_seconds`, longest awake first, and its `state`: the state it spent the most time in during the slice, in the words of a segment's `state` (`awake`/`asleep`/`switching`/`starting`), compared in unrounded milliseconds, the first of `awake`, `switching`, `starting`, `asleep` winning a tie; when `state` is `asleep` and the slice's asleep segments name a level it also carries `level` (1 or 2, as on a segment), the level with the most asleep time (the shallower on a tie; asleep time of unknown level does not compete), omitted when none is known. The server computes it and the console only colours the slice by `state`. The number of slices is the length from window start to the boundary divided by 1 minute, at most 50 and at least 1, all of equal length; a slice in which no deployment held the card is not listed. A **change of hands** is the awake deployment on the card becoming another one: following the awake segments in order of their start, each one whose deployment differs from the previous awake segment's counts once, in the slice the new holder began in; a deployment sleeping and waking again, or another instance of the same deployment taking over, is none. It counts the card's hand changes and differs from `switches` (`yielded` + `wakes`) in `deployments[]`, which count each deployment's own yields and wakes: one swap of A for B is 1 here, and one yield of A plus one wake of B there. Each deployment's seconds are rounded to whole seconds, overlapping segments of one deployment in one state count once, and different deployments' seconds overlap when they hold the card at the same time and fall short of the slice where none does, so they need not add up to the slice's length |
| `segments[]` | One instance holding the card for a stretch: `deployment_id`, `deployment`, `instance`, `state` (`awake` / `asleep` / `switching` / `starting`), `level` (1 or 2) while asleep, `from`, `to` (both inside the window); an instance on several cards appears once on each |
| `deployments[]` | Deployments that held these cards in the window and whose yield policy is not `never` (each with its `yield_policy`), and deployments deleted since that gave a card up or were given one in the window (`deployment_deleted: true`, `deployment` the name they left behind, `yield_policy` an empty string because it is not kept after deletion): `awake_seconds` (time at least one replica was awake), `present_seconds` (`fleet`: how long the deployment existed in the window, from max(window start, the earlier of its creation and its first instance) to min(now, its deletion); a deleted deployment's creation is not kept, so the earliest time its instances appear in the window is used, and the end of its last segment when its deletion time is unknown; `node`: how long at least one of its replicas held cards on those nodes in the window, the union of its segments in those nodes' lanes), `awake_share` (`awake_seconds` / `present_seconds`, 0 to 1, 0 when `present_seconds` is 0), `switches` (`yielded` + `wakes`), `yielded` (put to sleep to give a card to another deployment; the residency arbiter's preemption and an operator's yielding wake both count, the same basis as the overview's `yields_last_hour`), `yielded_to` (another deployment gave a card to it, counted by the event's `for_deployment`, also when the one that gave way has been deleted), `wakes` (the platform woke it), `held_requests` (requests the gateway held while it woke), `wait_p50_ms` / `wait_p95_ms` (the waits of those requests, approximate; omitted when no request was held or the gateway keeps no metrics), `cards` (where its replicas are now, by node: `node_id`, `node_uuid` (omitted for a node no longer known), `node` (the node's display name, empty for a node no longer known) and `gpus`; an empty array when none is placed). With `scope` `fleet` these figures (the switch counts, `awake_seconds`, `present_seconds`) are the deployment's own across the cluster, from the in-memory rollups of the [scheduling activity](#调度活动); with `node` they count only its replicas on those nodes: `awake_seconds` and `present_seconds` come from the same segments as the lanes, and `yielded`, `yielded_to` and `wakes` from the scheduling activity's in-memory counts by node (an event counts when the cards it concerns are on those nodes, and once when it concerns several of them; the last 24 hours are all in memory, and nothing new is persisted). A `node` read lists only the deployments that appeared on those nodes' cards; `held_requests`, the wait quantiles and `cards` are the same in both scopes: the gateway holds a request for a deployment, not for one replica, and `cards` lists where all its replicas are now |

The lanes of a custom range [`since`, `until`] are derived like this: the events of the window are read up to `until` only; an instance created after `until` is not there; one removed after `until` still holds its cards until `until`; and each instance's state at `since` comes from the newest events before it, as for a quick window. The read is made once per page of nodes and range, and the answer is cached in the server's memory: a quick window (15m / 1h / 6h / 24h) is cached for about 30 seconds per window and page of nodes, so several people polling the same page every minute share one read; a custom range whose `until` is more than a minute in the past is cached for about 5 minutes; a range that just ended is read every time. The cache holds at most 32 answers (when full, expired ones go first, then the oldest; one with very many elements, segments plus summaries, is not cached), and concurrent requests for the same key wait for one read. A quick window's answer can therefore be up to 30 seconds behind: `window.to` (where a segment still open ends), the cards the deployments are on now (`cards`), the yields and wakes counted so far, and the nodes and their GPUs are the values of up to 30 seconds ago, which the page, refreshing once a minute, does not tell apart. The summaries are computed from the segments of the same read, with no further store read; all reads are index ranges, with no sort or table scan. Events past the retention have been cleaned up, so in an older range only the instance rows that still exist say what those instances were doing.

The lanes are derived from instance records and the event log; the `INSTANCE_REMOVED` event written when an instance is removed (`kind` `residency`, params `node_id`, `gpus`) says which node and cards it held. For the semantics and limits see [Use Scheduling Analysis](../how-to/scheduling-center.md#驻留时间线).

### Scheduling Activity

`GET /api/v1/scheduling/activity?window=24h` is read-only: the scheduling activity stacked by reason, and the cards that switched most. `window` is `15m`, `1h`, `6h`, `24h` (the default) or `7d`, with buckets of 1 minute, 1 minute, 5 minutes, 15 minutes and 1 hour; a custom range is given by `since` and `until` (RFC 3339; `until` defaults to now, a later one counts as now, and the range is at most 1825 days), for which the server takes the shortest bucket that keeps the range within about 200 buckets (1, 5, 15, 30 minutes; 1, 3, 6, 12 hours; 1 day), never finer than the figures where the range begins: by the minute for the last 24 hours, by the hour within the hourly retention, by the day before. Answers are cached for about 30 seconds.

```json
{
  "from": "2026-10-30T12:00:00Z", "to": "2026-10-31T12:03:21Z", "bucket_seconds": 900,
  "reasons": [
    {"key": "idle_yield", "count": 120}, {"key": "rank_preemption", "count": 34},
    {"key": "timeslice_turn", "count": 410}, {"key": "operator_yield", "count": 2},
    {"key": "scheduled", "count": 8}, {"key": "placement", "count": 5}, {"key": "failure", "count": 3},
    {"key": "queued", "count": 17}
  ],
  "buckets": [{"start": "2026-10-30T12:00:00Z", "counts": [1, 0, 5, 0, 0, 1, 0, 0]}],
  "top_cards": [{"node_uuid": "5f0c…", "node": "gpu3", "gpu_index": 2, "switches": 61}],
  "beyond_retention": false
}
```

| Field | Meaning |
|---|---|
| `from`, `to`, `bucket_seconds` | The first bucket's start (aligned to the bucket, so possibly a little before the start asked for; day buckets align to the statistics time zone's midnight), the range's end, the bucket length in seconds |
| `reasons[]` | Every reason, in stacking order, with its total over the range: `idle_yield` (the residency arbiter put an idle holder to sleep for a waiter), `rank_preemption` (a higher-ranked deployment preempted a preemptible holder; a preemption that names no reason counts here too), `timeslice_turn` (equals took turns and the holder had used its time slice), `operator_yield` (an operator's yielding wake, `YIELDED_FOR_WAKE`), `scheduled` (a schedule's sleeps, wakes and replica changes), `placement` (placement and moves: the platform placed a new replica on a node, `UNIT_PLACED`, or moved one off a node an operator drains, `NODE_DRAIN_MOVED`), `failure` (replicas rebuilt, node failover moves with their reverts and the moves with nowhere to go, retry budgets exhausted, rolling restarts stalled, wake waits that timed out, automatic orders that failed), `queued` (a deployment that cannot be placed, `DEPLOYMENT_UNSCHEDULABLE`, and a wake that starts waiting, `WAKE_WAITING`). Wakes a request or a resume brought (the other half of a yield, already counted on the side that gave way, and still counted as the card's switch) and operator orders count under no reason; a move off a lost node, `NODE_REPLACED`, is a `failure` |
| `buckets[]` | One per bucket from `from`, empty buckets included; `counts` follows the order of `reasons` |
| `top_cards[]` | The at most 20 cards that switched most over the range, most first: `node_uuid`, `node` (the node's display name, empty for a node no longer known), `gpu_index`, `switches` (yields plus wakes on the card, by the cards the event's instances held at the time) |
| `beyond_retention` | The range begins before the daily statistics are kept; that part has no figures |

Where the figures come from: as each event is written, the store hands the scheduling events and the instances' state, sleep and removal events to the in-memory rollups; each event adds a few numbers to the minute it fell in (the reason's count, the cards it concerns, the deployment's yields, yields to it and wakes and the nodes they were on), and the instances' state changes keep each deployment's stretches with at least one replica awake and with a replica holding cards; nothing is written per event. The memory holds about the last 25 hours; on start the server replays the window's events once, by the `(kind, id)` and instance indexes. After each hour, the hour is written whole to stats.db's three hour tables (events per reason, switches per card, each sharing deployment's awake and present seconds and yields, yields to it and wakes); writing an hour again gives the same rows. When the server stops it writes the hour in progress. The hour and day tables are kept by the data retention setting (`GET /api/v1/data-retention`), `stats_hourly_days` / `stats_daily_days` (31 and 400 days by default), and a day's rows are rolled up from its hours once it is over. A read of the last 24 hours reads only the memory; older parts read the hour or day tables (the per-card sums by a primary key range), so reads do not slow down as the event log grows.

### 调度记录

`GET /api/v1/scheduling/history` is read-only and returns scheduling events across deployments, newest first:

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/history?category=failure,manual&since=2026-10-27T00:00:00Z&limit=50" \
  -H "Authorization: Bearer $TOKEN"
```

```json
{"items":[
  {"id":812,"created_at":"2026-10-28T09:14:03Z","deployment_id":7,"deployment":"qwen","instance":"9b1e…","kind":"lifecycle",
   "code":"REPLICA_REBUILT","category":"failure",
   "params":{"replica":1,"attempt":2,"reason":"CUDA out of memory","node":"gpu-3","node_uuids":["5f0c…"]},
   "nodes":[{"uuid":"5f0c…","name":"gpu-3"}],
   "logs":{"instance":"9b1e…","from":1792919043000,"to":1792919703000}},
  {"id":810,"created_at":"2026-10-28T09:10:41Z","deployment_id":7,"deployment":"qwen","kind":"schedule",
   "code":"REPLICAS_CHANGED","category":"manual","params":{"from":1,"to":3,"actor":"alice"},"actor":"alice"}
]}
```

| Parameter | Meaning |
|---|---|
| `limit`, `before` | As for `GET /api/v1/deployments/{id}/events`: `limit` defaults to 100 and is capped at 500; pass the `id` of the last item of the previous page as `before` for the next page |
| `deployment_id` | Only one deployment |
| `category` | `auto`, `failure`, `manual`; give several separated by commas; omitted means all |
| `since`, `until` | RFC 3339 times, inclusive |

| Response field | Meaning |
|---|---|
| `id`, `created_at` | The event id (the paging cursor) and its UTC time |
| `deployment_id`, `deployment` | The deployment and its name; for a deleted deployment the name comes from what it left behind and `deployment_deleted: true` is set; when not even a name is known, `deployment` is omitted |
| `instance` | The instance UUID when the event hangs on an instance, otherwise omitted |
| `kind`, `code` | The event kind (`schedule` / `automation`) and the event code |
| `category` | `auto`: the platform acted on its own settings or schedules; `failure`: something went wrong and the platform is dealing with it or has given up; `manual`: an operator's order |
| `params` | The event code's params, as on the deployment timeline |
| `actor` | The user who gave the order; present only on `manual` events whose request carried an identity, and also in `params.actor` |
| `nodes` | The nodes the event concerns, `[{uuid?, name}]`, from the first of these that yields any: the nodes the event's params recorded (`node_uuids`; an older event's `node` is a node UUID or name, its `nodes` are names), and the nodes the instances the event names (`instance`, `instances`, or the instance the event is on) are on now; the whole field is omitted when there are none. Only a node the server still knows carries `uuid` (the UI opens its drawer by it); for a node that no longer exists `name` is the first 8 characters of its UUID, and an older event that kept only a name gives that name. The nodes of a whole page are found in one query |
| `target` | The deployment the event acted for, `{id, name?, deleted?}`, taken from `params.for_deployment` (for example the deployment a yield gave the cards to); the whole field is omitted when there is none. A deleted deployment carries `deleted: true` with the name taken from what it left behind, and `name` is omitted when not even a name is known. The UI uses it to show a clickable yield target next to the deployment name |

The server decides in one place which event codes belong to the history and in which category:

| Category | Event codes |
|---|---|
| `auto` | `AUTO_SLEEP`, `AUTO_WAKE`, `AUTO_SCALE`, `WAKE_WAITING`, `WAKE_WAIT_TIMEOUT`, `SCHEDULE_ACTION_FAILED`, `YIELDED_FOR_WAKE`, `UNIT_PLACED`, `NODE_DRAIN_MOVED` |
| `failure` | `NODE_REPLACED`, `NODE_REPLACE_REVERTED`, `NODE_REPLACE_NO_CANDIDATE`, `REPLICA_REBUILT`, `UNIT_RETRY_EXHAUSTED`, `DEPLOYMENT_UNSCHEDULABLE`, `DEPLOYMENT_ROLLING_STALLED` |
| `manual` | `REPLICAS_CHANGED`, `REPLICA_RESTARTED`, `UNIT_RETRIED`, `DEPLOYMENT_RESTARTED`, `DEPLOYMENT_ROLLING_RESTARTED`, `SLEEP_ORDERED`, `WAKE_ORDERED` |

An order an operator gives through the API leaves an event on the deployment timeline with `params.actor`:

| Event code | When it is recorded | Main params |
|---|---|---|
| `REPLICAS_CHANGED` | `PUT /deployments/{id}/replicas` or `PUT /deployments/{id}/pd` changed the replica count (no event if the count is unchanged). Unrelated to the 409 error code of the same name | `from`, `to`, and `role` for PD |
| `SLEEP_ORDERED` | A deployment, a PD role, or one replica was ordered to sleep | `level`, `mode`, `scope` (`deployment` / `role` / `replica`), `role`, `instances` |
| `WAKE_ORDERED` | A wake started waking at least one instance; a wake in which every instance waits for room records nothing here (it has `WAKE_WAITING`) | `instances` |
| `DEPLOYMENT_RESTARTED`, `DEPLOYMENT_ROLLING_RESTARTED`, `REPLICA_RESTARTED`, `UNIT_RETRIED` | Restarts and retries | Their existing params |

The platform's own actions (the controller, the automation) carry no `actor`.

When a failed replica is rebuilt, the controller records `REPLICA_REBUILT` (deployment timeline, `kind` `schedule`), one per rebuild, so their number is bounded by the retry budget (see [Retry After a Fix](../how-to/troubleshooting.md#修复后重试)):

| Param | Meaning |
|---|---|
| `replica` | The replica number, counting from 1 |
| `role` | The role of a PD deployment |
| `attempt` | Which attempt this is |
| `reason` | The message of the previous failed attempt, cut off after 200 characters |
| `node` | The name of the node of the new attempt's leader |
| `node_uuids` | The UUID of the same node (a list) |

The events the scheduling history lists carry `node_uuids` (the list of node UUIDs the event concerns) when they are written: an event that names instances records the nodes those instances were on, and the controller's node events record the nodes they concern (moved from and to, lost, the placements before and after, where the failed attempts ran); `DEPLOYMENT_UNSCHEDULABLE` uses its existing `node` (a node UUID). Orders for the whole deployment (a scheduled sleep, a replica count, a rolling restart) do not carry it. See [Use the scheduling center](../how-to/scheduling-center.md#时间线事件) for each code.

The read goes through the `(kind, id)` index of `instance_events`: one page per event kind, merged, without scanning the whole timeline.

### Adjust Replicas for Regular Deployments

`PUT /api/v1/deployments/{id}/replicas` adjusts replicas of regular deployments. Multi-node and single-node deployments use different request bodies.

For **manually placed multi-node deployments**, submit all desired replicas: `{"units":[{"node_uuid":"<leader>","followers":[{"node_uuid":"..."}]}]}`.

- Each replica must have exactly `nnodes` ranks, each on a different known node; otherwise returns `REPLICA_UNIT_RANKS` / `REPLICA_UNIT_NODE`.
- GPUs cannot be specified; otherwise returns `REPLICA_UNIT_GPUS`.
- Leaders of different replicas cannot be on the same node; otherwise returns `REPLICA_LEADER_SHARED`. This is because leaders listen on the deployment's master port.
- The total number of GPUs required by all ranks listed on a node, regardless of which replica they belong to, cannot exceed that node's GPU count; otherwise returns `REPLICA_UNIT_GPUS_INSUFFICIENT`.
- Nodes for new replicas or replicas whose nodes have changed must also be online (`REPLICA_UNIT_NODE_OFFLINE`) and meet the deployment's node requirements (`REPLICA_UNIT_REQUIREMENTS`). Already listed replicas whose nodes and order remain unchanged are retained as-is, so offline nodes or changed labels do not block changes to the entire list.
- When the first replica's leader changes, the master address stored on the deployment is cleared; each replica rendezvouses at its own leader's parameter-network address.
- Replicas removed from the list drain and then retire. New replicas form separate groups and rendezvous at their own leaders. The response's `units` lists current replicas and their retry status.

For **automatically placed multi-node deployments** (`placement_mode` is `auto`), submit the replica count: `{"replicas":N,"place_on":["<node uuid>"],"remove":["<uuid>"]}`, without `units` (with `units`, the list is written as given, as above).

- When growing, the server picks nodes for the new replicas by the placement strategy (within a replica, one compute network and one GPU architecture, a free-card slot for one rank on each node, and a leader that does not share a node with another replica's leader) and writes them into the deployment's replica list; the rules are those of [how nodes are chosen at creation](../how-to/multi-node-inference.md#创建时怎样挑节点).
- `place_on` is optional and used only when growing: it names nodes for the new replicas' ranks in order (the first new replica's ranks 0…N-1, then the next replica's). You may give only some, and the server fills in the rest. More entries than the new replicas have ranks returns 400 `REPLICAS_PLACE_ON_TOO_MANY` (`params.count`, `maximum`); named nodes that cannot form a replica that fits return 409 `REPLICAS_PLACE_ON_REFUSED` (`params.node`, `reason` is `no_group`); when no group fits at all, 409 `UNIT_PLACEMENT_NONE`.
- `remove` is used only when shrinking; with it on a grow, the request returns 400 `REPLICAS_REMOVE_TOO_MANY` (`params.maximum` is 0).
- When shrinking, the replicas of the instances listed in `remove` go first (an instance that is not in this deployment returns 400 `REPLICAS_REMOVE_INVALID`, more than the reduction returns 400 `REPLICAS_REMOVE_TOO_MANY`); without it the most recently added go. At least one replica remains, otherwise 400 `REPLICAS_TOO_FEW`.

For **single-node deployments**, submit the replica count: `{"replicas":N,"remove":["<uuid>"]}`.

- `place_on` is optional and used only when adding replicas: a list of node UUIDs the new replicas start on first. It decides only the first placement, and the hint is kept in server memory. Naming more nodes than replicas added returns 400 `REPLICAS_PLACE_ON_TOO_MANY` (`params.count`, `maximum`); a node that cannot take a replica returns 409 `REPLICAS_PLACE_ON_REFUSED` (`params.node`, `reason`: `unknown`, `offline`, `cordoned`, `requirements`, `no_room`, with `detail` or `code` / `params` where relevant).
- `remove` is optional and specifies which instances' replicas to remove when scaling down. These replicas enter `draining` and retire after finishing requests. If omitted, the server selects replicas, choosing unready replicas first, then the most recently created.
- The count must satisfy 1 ≤ N ≤ the deployment's `max_replicas` (currently 1024); otherwise returns `REPLICAS_TOO_FEW` / `REPLICAS_TOO_MANY`.
- Returns 400 `REPLICAS_REMOVE_TOO_MANY` if `remove` contains more entries than the number of replicas being removed; returns 400 `REPLICAS_REMOVE_INVALID` if a specified instance is not serving a replica of this deployment.
- If the replica count or specified instances change during the request, for example because of concurrent scaling or an instance that has just failed: 409 `REPLICAS_CHANGED`, with no changes written.

**Error conditions shared by both requests:**

- Request body does not match the deployment shape (PD, a count for a manually placed multi-node deployment, a list for a single-node deployment, or `place_on` on a manually placed multi-node deployment): 400 `REPLICAS_BODY_SHAPE`.
- Deployment is being deleted: 409 `DEPLOYMENT_DELETING`.

### Configuration Fields for Evaluation and Creation

`POST /evaluate` and `POST /api/v1/deployments` share the same deployment configuration request; evaluation results do not reserve resources. Configuration fields are as follows:

| Field | Semantics |
|---|---|
| `quick_params` | Submit values in the types declared by the `/engines` parameter dictionary; explicit `false` differs from omission |
| `quick_params_unset` | Array of parameter keys explicitly excluded from inheritance in this request, preventing template backfill; not written to persistent configuration |
| `placement_mode` | `auto` lets the server select nodes; `manual` uses explicit nodes. Defaults to `manual` if omitted. Single-node: only request terminology, the deployment does not store this field, and manual placement is stored as node requirement `gpuworks.ai/node: [node_uuid]`; an automatically placed create may omit `node_uuid`, in which case the server first picks a node by the same rules as evaluation and then creates as usual, answering 409 `AUTOMATIC_PLACEMENT_UNAVAILABLE` when no node fits (`params.candidates` is the number of candidate nodes, `reasons` each candidate's reason code); a create that carries the `node_uuid` evaluation selected is re-checked as before and answers `AUTOMATIC_PLACEMENT_STALE` when resources changed. Multi-node and PD: with `auto` the server picks nodes for the ranks or units the request leaves out at creation (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)), the deployment is stored as automatically placed, and `placement_mode` in the detail is `auto` |
| `target_vendor` | Evaluation and creation use the same accelerator vendor; stored as node requirement `gpuworks.ai/vendor`. If omitted, creation fills it in using the destination node's vendor |
| `node_requirements` | Node requirements: `{"<key>": ["<value>", …]}`. Multiple values for one key mean "any of these"; different keys must "all match." Keys can be node labels or system labels (see "Node Labels" above). Any key in the reserved domain other than the eight system keys is rejected (a misspelled key does not silently match 0 nodes). `gpuworks.ai/vendor` can have only one value; network key values are network IDs or `management`. Multi-node and PD deployments list nodes per rank and do not accept `gpuworks.ai/node`. Errors: `NODE_REQUIREMENTS_INVALID`; selected nodes do not meet requirements: `NODE_REQUIREMENTS_UNMET` (`params.detail` identifies the key) |
| `yield_policy` | The yield policy, an object shaped like the body of `PUT /deployments/{id}/yield-policy` (`policy` plus parameters); when omitted it is `never`. A policy other than `never` requires `sleepable`, and a policy other than `never` also a single-node deployment, otherwise 400 `YIELD_POLICY_INVALID` / `SLEEP_NOT_ENABLED` / `YIELD_POLICY_SHAPE`. Evaluation computes placement from it: while a deployment that shares cards by yield policy is asleep, deployments it does not share with still count it at full usage |
| `placement_strategy` | `spread` or `binpack`, saved with the deployment; other values return `PLACEMENT_STRATEGY_INVALID`. When omitted it is `spread` for single-node and `binpack` for multi-node and PD (automatic or manual) |
| `placement.gpus`, `placement.tp`, `placement.pp` | Explicit physical GPU indexes; TP and PP are parallelism for the entire deployment (TP × PP ÷ `nnodes` GPUs per node, which must divide evenly). If TP or PP is omitted for a manually placed multi-node deployment (a TP or PP that is not a positive integer counts as omitted), the platform tries layouts against the model and the cards currently free on each node and recommends the fewest cards that fit, trying 1, 2, 4, 8 cards per node and then every card a node has, or every count per node that gives a stated TP: at equal size one pipeline stage per node is preferred, a template placement preference comes first when present, and layouts whose KV estimate cannot hold the context rank last. When no layout fits, both evaluation and creation return `DISTRIBUTED_PARALLELISM_UNAVAILABLE` (`params.node` names the refusing node; `params.reason` is a scheduler reason code or `no_layout`, with that reason's own params in `params.reason_params`; evaluation attaches it to the blank TP or PP) instead of waiting for cards. When the model weight size is unknown, the default applies: one pipeline stage per node, with TP set to the smallest GPU count among the selected nodes. When both TP and PP are given, the fixed layout is validated and may wait for cards. Effective values are written back to placement. If TP is omitted for single-node deployment, the scheduler selects a GPU count that is an integer multiple of PP (TP = GPU count ÷ PP). Automatic placement also selects nodes according to the supplied TP and PP. Evaluation returns the effective values in `parallelism` (`tp`, `pp`, `nodes`, `gpus_per_node`); `form_values.tp/pp` does not echo the degrees a manual multi-node recommendation left blank; `acquisition.nodes` in multi-node evaluation and manual placement provides acquisition estimates for each node |
| `routing_policy` | Multi-replica gateway policy: `least_request` (default), `prefix_affinity`, or `failover`; not an engine launch parameter |
| `tokenize_enabled` | Whether the gateway forwards `/v1/tokenize` and `/v1/detokenize` to this deployment (see [OpenAI-Compatible Gateway](openai-gateway.md#分词tokenize)). Enabled if omitted; `false` disables it. Independent of the engine; if the engine does not provide these endpoints, the engine itself returns an error |
| `priority_scheduling` | Whether the engine schedules by request priority (see [Request Priority](../explanation/request-priority.md)): if omitted, it is on when the effective LLM entrypoint and arguments allow it, otherwise off; an explicit `true` where they do not is refused by create and edit with 400, the code being the reason (`PRIORITY_SCHEDULING_ENGINE_UNSUPPORTED`, `PRIORITY_SCHEDULING_LAUNCH_UNSUPPORTED`, `PRIORITY_SCHEDULING_TARGET_UNVERIFIED`, `PRIORITY_SCHEDULING_PD_UNSUPPORTED` or `PRIORITY_SCHEDULING_POLICY_CONFLICT`, with `params.field` `priority_scheduling`). An edit that omits it keeps the stored value (turned off when the arguments no longer allow it); changing it rebuilds replicas |
| `health_check`, `health_check_threshold_sec` | Service health checks (see "Health Checks" below): when `health_check` is omitted, enabled if the model provides chat, completions, embedding or System One decisions, otherwise disabled. If explicitly `true` and the model provides none of them, creation returns 400 `FIELD_INVALID`. The threshold is 1–600 seconds; omission or 0 means 60. For out-of-range values, evaluation reports a field error and creation returns 400, both with code `FIELD_INVALID` |
| `start_timeout_sec` | Startup health check budget (seconds) for instances of this deployment: the maximum time after an instance starts to wait for the engine health check to pass. Omission or 0 selects an automatic budget (cluster default → the template spec's `start_timeout_sec` → raised to the engine deadline; see [Startup Wait Limit](deployment-launch-sources.md#启动等待上限)); a stated value replaces it and must be 60–86400. For out-of-range values, evaluation reports a field error and creation returns 400, both with code `FIELD_INVALID`. Saved with the deployment and returned under the same field name in deployment details when a value is set. The value is fixed when an instance is created; changes affect only instances started afterward |

`quick_params.enforce_eager` preserves three states: omission inherits the template or engine default, `false` explicitly disables it even if the template enables it, and `true` explicitly enables it. Evaluation `form_values`, repeated evaluation and creation preserve that choice. The eager flag is `--enforce-eager` for vLLM and `--disable-cuda-graph` for SGLang.

Instance and evaluation responses expose the actual budget and conflicts as `start_timeout_sec`, `start_timeout_source`, `engine_start_timeout_sec` and `start_timeout_conflict`; `start_timeout_source` is the step that decided it: `cluster`, `template`, `engine` or `deployment` (see [Startup Wait Limit](deployment-launch-sources.md#启动等待上限)), and is omitted for older instances that recorded none. `startup_estimate` provides only a rough formula estimate: when `method=weight_reference`, `estimated_seconds = weight_bytes / (reference_mib_per_sec × 1024 × 1024) + initialization_seconds`. The reference loading rate is `512 MiB/s`. The aggregate initialization allowance follows the effective engine/mode: `240 seconds` for SGLang, `300 seconds` for ordinary vLLM and `420 seconds` for vLLM with an explicit final `--omni` flag. These are project reference profiles, not paper coefficients or node measurements; no extra adjustment is inferred from runtime, GPU count or eager settings. Missing weights use `method=missing_weight`; unsupported configurations use `unsupported`. Unknown numeric estimates and reference parameters are omitted instead of reported as zero. The estimate does not change the startup budget. Optional instance `measured` fields `StartupSeconds`, `StartupTimeoutSeconds`, `ModelLoadingSeconds`, `CompilationSeconds` and `GraphCaptureSeconds` represent successful health wait, unfinished-wait lower bound and explicitly logged durations; they must not be added together.

### Health Checks

When enabled, the server sends a minimal request through the gateway every minute, selects a replica according to the gateway policy, and waits up to the threshold:
for chat, it sends a message containing `1` with `max_tokens:1`; when only embedding is available, it sends the input `1`; when only System One decisions are available, it asks one yes/no question about the state `1`.

- Checks only report results; they do not restart or remove replicas from service. No request is sent if no replica is awake and running.
- The gateway excludes checks from request counts, token counts, and TTFT/TPOT metrics. It does not save request records or update the last request time.
  The engine's own metrics may still include the once-per-minute, 1-token check; the gateway cannot subtract it from engine metrics.
- Only the 10 most recent results are retained in memory. New results appear within one minute after the server restarts.

The `health` field in deployment details, deployment lists, and the monitoring service page contains:

| Field | Meaning |
|---|---|
| `status` | `ok` (responded within the threshold), `timeout` (no response within the threshold), `failed` (returned an error, or the gateway could not connect to the replica), `off` (not enabled), `no_replica` (no replica available to check; no request sent), `pending` (enabled but not yet checked) |
| `threshold_sec` | Threshold |
| `last` | Most recent check, `{at_ms, status, latency_ms, http_status?, instance_uuid?, message?}`: `instance_uuid` identifies the replica that responded; `message` is the beginning of the error response or the reason for no response. For timeouts, `latency_ms` is recorded as the threshold. `null` if no check exists |
| `recent` | Up to 10 most recent checks, oldest first |
| `recent_mean_ms`, `recent_ok` | Mean latency and count of checks that responded within the threshold; `null` and `0` if none |

### Automatically Placed Multi-Node and PD Deployments

Create and evaluate requests for multi-node and PD deployments take `placement_mode: "auto"`, and the server picks the nodes for the ranks or units the request leaves out. With it omitted or `manual`, the request supplies the nodes.

Multi-node request (`nnodes` > 1):

```json
{
  "model_id": 3, "nnodes": 2, "placement_mode": "auto",
  "placement": {"tp": 8, "pp": 2},
  "node_uuids": ["<node uuid of rank 0, optional>"]
}
```

- `placement.tp` is required, otherwise 400 `AUTO_PLACEMENT_TP_REQUIRED`; the free cards each node needs are TP × PP ÷ `nnodes`.
- `node_uuids` may be omitted or may name only the first ranks' nodes (more than `nnodes` entries return 400 `DISTRIBUTED_NODE_COUNT`); the rest are filled in from the compute network and GPU architecture those nodes are in. Named nodes that are not in one compute network and one architecture are treated as no group fitting (409 `UNIT_PLACEMENT_NONE`).
- A multi-node deployment still has exactly one replica at creation (`replicas` is 1); scale it afterwards with `PUT /replicas` by replica count (see [Adjust Replicas for Regular Deployments](#普通部署的副本调整)).

PD request (`placement.pd`): a unit may omit `node_uuid`. Units that name a node stay where they are, and the platform fills in only the rest; units placed automatically pin no cards (the scheduler picks `gpus` at launch), and the cards that units already naming a node will use are subtracted first when nodes are picked for the others. When a unit without a node has `followers`, the request returns 400 `AUTO_PLACEMENT_PD_MULTI_NODE` (`params.role`): automatically placed units must fit one node. Adding units through `PUT /deployments/{id}/pd` works the same way, and new units may omit their nodes.

Selection rules (a multi-node request is chosen per replica, a PD request for the whole deployment; only online, schedulable nodes that meet the node requirements and have free cards are considered):

1. The nodes within one replica (the whole deployment for PD) share one compute network and one GPU architecture; when a PD deployment already has units, new units join the network and architecture those are in.
2. Between groups: RDMA networks (host RDMA, accelerator RDMA) before Ethernet, then the group with more nodes that already hold the model file.
3. Within a group: nodes with the model first, then `placement_strategy` (multi-node and PD default to `binpack` when it is omitted): `binpack` takes the node with the fewest free cards that still fit, `spread` the most, and ties go to the lower node ID. For PD under `binpack`, if one node can take every unit they all go on that node; under `spread`, prefill and decode avoid sharing a node where they can.

When no group fits, the request returns 409 `UNIT_PLACEMENT_NONE`, with `params` `nodes` (total nodes), `unmet` (node requirements not met), `cordoned`, `offline`, `busy` (not enough free cards), `no_network` (no usable compute network), `no_peer_memory` (a PD mode moves KV over RDMA and the node has not loaded the peer memory module), and `largest` (how many nodes the largest group has); when a PD mode moves KV over UCX on a network that is not host RDMA and nodes were skipped because their kernel hangs UCX (the node's `ucx_netlink_hang`), it also has `ucx_netlink_hang` (how many) and `ucx_netlink_nodes` (the first few names), and otherwise neither field. When a PD mode moves KV over RDMA and nodes were left out for the peer memory module, the answer is 409 `PD_PEER_MEMORY_NONE` instead (`params`: `mode`, `nodes`, `no_peer_memory`, `module`, `fix` with the fix command); a manual placement on such a node returns `PD_PEER_MEMORY_MISSING` (`mode`, `role`, `node`, `module`, `fix`, `state`). Evaluation does not answer these failures with an HTTP error; it returns `ok=false` with an entry in `errors[]` whose `field` is `placement_mode`, and the response carries `auto_placed: true`.

A successful evaluation carries `auto_placed: true` and previews the nodes picked now; creation picks again, so the two can differ. Creation does not submit the nodes evaluation selected, so it has none of single-node automatic placement's `AUTOMATIC_PLACEMENT_STALE`; when resources are taken after evaluation, the re-pick when the unit starts handles it. The deployment stores the nodes that were picked (in the multi-node `node_uuids` or in the PD units), and `placement_mode` in the detail is `auto`.

When a unit starts, the controller plans its nodes against the current free cards. If all nodes are online and fit, it starts. If nodes were cordoned, no longer meet the node requirements, or would be refused by scheduling (another deployment holds the cards), only those ranks move to other nodes and `UNIT_PLACED` is recorded (`params`: `unit` (from 1), `role`, `from`, `to` (node names by rank), `reason`). With nowhere to go the unit keeps its nodes and waits, trying again about every minute. Offline nodes belong to node-loss rescheduling and draining nodes to drain, regardless of the placement mode. Manually placed units do not do this; when PD pinned cards are held by other deployments the scheduling reason is `pinned_gpus_unavailable` (`params`: `node`, `gpus`, `holders`).

`PUT /deployments/{id}/placement-mode` switches between automatic and manual; existing deployments are manual.

### Evaluation Requests and Acquisition Estimates

Deployment requests do not select where images or models come from ([ADR 0048](../adr/0048-pull-settings.md)): `model_source` contains only `{storage_id, provider}`. The former is the storage where a node saves the model it acquires; the latter selects the weight provider for the running template (`huggingface` / `modelscope`). `acquisition.nodes[]` provides estimates for the nodes specified in the request (manual placement or multi-node, in rank order). These are estimates only; the node reassesses acquisition when it starts:

| Field | Meaning |
|---|---|
| `image_plan` | `present`: already on the node; `copy`: copy from the `image_copy_from` node in the sync group; `pull`: pull from `image_pull_refs`. The first `image_pull_fixed` entries are alternative addresses from source rules, tried first in order. The remaining entries are the image's own replicas (when `image_pull_ranked` is true, the node orders them by probe latency). If the rules leave the node with no available pull source, `image_pull_error` is included (`{code, params, message}`; code is usually `IMAGE_SOURCE_FORBIDDEN`, and params contains `node` and `route`) |
| `weights_plan` | `present`: already present; `copy`: copy from the `weights_copy_from` node; `storage`: read or cache from shared storage `weights_storage`; `download`: download from `weights_provider` |
| `image_cached`, `image_cache_reason`, `weights_present` | Whether the image is cached (with a reason if unknown) and whether the model is already on the node |

Single-node automatic placement does not provide per-node estimates before the scheduler selects a node; after scheduling, the selected node acquires the assets itself. Automatic placement of multi-node and PD requests is first resolved to the nodes picked now in evaluation, as if those nodes had been written by hand.

`acquisition.components[]` forecasts, for each template component other than the primary model (a separately published text encoder, a speculative-decoding draft model, and so on), how the target nodes get it; creating the deployment acts on the same plan ([ADR 0062](../adr/0062-template-components-follow-the-primary-storage.md)):

| Field | Meaning |
|---|---|
| `name`, `role`, `model`, `size_bytes` | Component name, role, model (repository ID) and the size the template declares |
| `plan` | `ready`: every target node has it; `shared_download`: the create downloads one copy into the shared storage `storage` (`storage_id`) through one node, and the target nodes read it; `follows_primary` is true when the request selected no **Save to** and this is the shared storage the primary model is on; `node_download`: each node without it downloads it into its own storage |
| `nodes`, `ready_nodes` | Number of target nodes, and of those that can read it already |

When the create starts a component write, the timeline records `DEPLOYMENT_COMPONENT_DOWNLOAD` with `component`, `model`, `bytes`, `storage`, `node` (the writer node) and `follows_primary`.

When hardware capabilities are not met, assembly returns `ACCELERATOR_FEATURE_UNSUPPORTED` (`vendor`, `feature`); final AMD placement without an assigned device returns `ACCELERATOR_DEVICE_SELECTION_REQUIRED`. Automatic placement does not return the latter before device selection is complete. Candidate hardware or runtime restrictions use structured refusal reasons; mixed-vendor nodes are evaluated against the cards matched by the template.

### Evaluation Field Errors (`errors[]`) and `ok=false` Semantics

`POST /evaluate` is an interactive endpoint:
validation failures attributable to request fields return HTTP 200 with `ok=false`, and errors appear in the response's
`errors[]` (each entry is `{field, code, params?}`; `code` is a stable ProductError code that maps one-to-one to the error code on the corresponding create
400 path, and `params` contains interpolation parameters for the client's locale). The form retains the draft and displays
errors beside the corresponding controls; `POST /api/v1/deployments` (create) continues to return 400/409. Failures that cannot be attributed to a field
(authentication, template resolution failures, server errors) retain their original status codes.

Independent request-level checks report their errors together. Request-level field errors stop later launch planning while retaining template and eligible-node facts, together with health-check facts that can be resolved. A missing model ID at the model-resolution stage is attributed to `model_id`; health-check eligibility may be absent before a model is resolved. Automatic multi-node placement failures return a `placement_mode` error before later checks run. Field path conventions:

- Quick parameters use the dictionary key directly (the form field name), such as `max_model_len` and `gpu_memory_utilization`.
- Request-level fields use their names in the request JSON: `replicas`, `nnodes`, `engine`, `target_vendor`, `extra_args`,
  `quick_params_unset`, `master_port`, `placement`, `placement.tp`, `placement.pp`, `node_requirements`,
  `placement_strategy`, `partition_backend`, `start_timeout_sec`, and `health_check_threshold_sec`.
- `node_uuids` (the multi-node list) maps to the `distributed_nodes` control in the form; when an automatic multi-node request cannot pick nodes or lacks `placement.tp`, the error is attributed to `placement_mode`.
- All additional mount rows map to `container_mount_rows` (the request carries only user-added mounts, without row numbers).
- Docker argument errors map to `container_docker_args`, and shared memory size errors map to `container_shm_gib`.
- **Errors with an empty `field` string do not belong to any control**: failures unrelated to an individual parameter, such as launch assembly or placement simulation failures
  (for example, the node does not have the specified GPU index, or no runtime image is available), use this form. Clients must merge them into page-level
  messages and must not attach them to parameter controls.

The rules cover replica shape (`REPLICAS_NEGATIVE`/`REPLICAS_TOO_MANY`/`DISTRIBUTED_REPLICA_COUNT`),
node requirements (`NODE_REQUIREMENTS_INVALID`/`NODE_REQUIREMENTS_UNMET`), placement strategy (`PLACEMENT_STRATEGY_INVALID`),
vendor (`TARGET_VENDOR_INVALID`), engine (`ENGINE_INVALID`),
extra_args parsing (`EXTRA_ARGS_INVALID`), mounts (`MOUNT_CONTAINER_PATH`/`MOUNT_HOST_PATH`/
`MOUNT_TARGET_DUPLICATE`/`MOUNT_TARGET_NESTED`/`MOUNT_VOLUME_INVALID`), JSON parameters (`QUICK_PARAM_JSON_INVALID`),
container path parameters (`QUICK_PARAM_PATH_INVALID`), Docker arguments that duplicate platform-managed settings or dedicated fields
(`DOCKER_ARG_PLATFORM_MANAGED`/`DOCKER_ARG_USE_SHM_FIELD`/`DOCKER_ARG_USE_ENV_FIELD`/
`DOCKER_ARG_USE_MOUNT_FIELD`/`DOCKER_ARG_USE_ENTRYPOINT_FIELD`), setting both shared memory size and `--ipc=host`
(`SHM_SIZE_WITH_HOST_IPC`), shared memory size below the CPU memory tier (`SHM_BELOW_KV_CPU_TIER`), parameter conflicts (`LAUNCH_CONFIG_CONFLICT`/
`LAUNCH_CONFIG_UNSET_CONFLICT`), and numeric bounds
(`FIELD_INVALID`, with English fallback text in `detail`). The response also includes the contract version `schema_version` (currently 1).

In addition to `errors[]`, the evaluation response includes the following fields as needed:

| Field | Meaning |
|---|---|
| `form_values` | Complete values for the form to display: the effective configuration (including dictionary defaults and template projections) and the `extra_args` text after recognized parameters have been moved into structured fields |
| `defaults` | Form defaults: `model_mount` (`/model`), `master_port` (29500), and `engine` (the default engine) |
| `eligible_nodes` | Each node is evaluated against the draft's node requirements (including vendor; the manually selected node itself is not a requirement, because the dropdown is used to select it), the template's GPU predicate, and online status. When `eligible=false`, `reasons[]` provides the reasons (`node_offline`, `node_requirements_unmet`, `vendor_mismatch`, `chip_mismatch`; when the node requirements themselves are invalid, every node has `node_requirements_invalid`, rather than all nodes appearing eligible). In a single-node draft with no memory limit, a node that could not fit a replica even with nothing else on it is also `eligible=false`: the scheduler's `no_fit` (with the memory needed and available) when its card count or per-card memory cannot hold the replica, `placement_rejected` (`params.detail` is the error text a create naming it returns) when the launch command cannot be resolved on the cards it gives, and `model_exceeds_cache_limit` when the model is larger than the cache limit of a storage on that node. The verdict does not depend on the placement mode or on which node the draft currently has; it reads what the node is, not what runs on it (busy cards, an offline node or a full disk never make a node ineligible this way). Nodes of one hardware shape with the same applicable storages and cache limits are judged once together; one evaluation judges at most 16 such kinds, and nodes of further kinds, or of a kind whose judging failed, stay eligible. Drafts with a memory limit, multi-node drafts and PD drafts are not judged, so `eligible=true` does not guarantee that a create succeeds. The node dropdown renders only this list |
| `gpu_candidates` | Manual single-node feedback: `{node_uuid, items}` with `index`, `available`, and a structured `reason` for each unavailable card. This reports current resource availability, not single-card model fit or permission to select. The UI continues to gate new selections on the node card's `schedulable` state. An absent field means not evaluated; `plan` still checks model fit, GPU groups and topology. |
| `weight_sources` | Weight sources declared by the template spec (`provider`/`repo_id`/`revision`/`subfolder`), plus `suggested` (server recommendation) and `registered` (a registration already exists in this cluster, identified by repo id). After selection, the client still calls the registration API |
| `preflight` | Only for `nnodes>1`: request-level preflight `{ok, checks}`. `checks` lists every check made, passing ones included: `id` is `node_count`, `node_list`, `port_range`, `node_online` (one per node, `params.node` naming it), or `shared_network`; `status` is only `pass`/`warn`/`fail`/`skip`/`na`; `code` and `params` give the result, and `message` is the English fallback; a check not run because an earlier one failed is `skip` with `code` `PREFLIGHT_BLOCKED`, and `blocked_by` names the check that stopped it; `evidence` carries the raw error only when no code explains the cause. `ok` is true when nothing is `fail`. Deep network probing remains in the separate preflight RPC (retained by the CLI); the frontend no longer orchestrates it |
| `diagnostics` | The model-acquisition checks behind the answer `{checks, truncated?}` (also in the error body when create is refused): each has `id`, `status` (only `pass`/`warn`/`fail`/`skip`/`na`; `warn` means a copy or transfer is still under way, or the cache must be filled at launch; `skip` means not checked or not observed, with `blocked_by` when an earlier check stopped it; `na` means it does not apply), and `message` (`{code, params, message}`, where `params.reason` is the specific cause and `params.observer` the source of the evidence); at most 128 checks and 64 KiB, with `truncated` true beyond that |
| `eviction` | Returns candidates when GPU memory is currently insufficient but space can be freed by putting awake deployments to sleep. Each entry includes `instance_uuid`, `deployment_id`, `deployment`, `gpu_indexes`, and `frees_gib` |

The server fills parameter defaults from the current engine dictionary, including engine-specific defaults. Parameters whose dependencies are not enabled receive no default.
Clients use `layout` (`quick`, `expert`, `hidden`; an empty string means `expert`) and `order` from `/engines` to arrange the parameter form.

`quick_params_unset` can contain only keys in the current engine dictionary. If the same request also supplies a value for that key in
`quick_params` or `extra_args`, it returns an unset conflict. The existing `null`
semantics remain unchanged; `null` cannot replace an explicit unset. Composite engine parameters can be removed from a template only when the adapter can recognize them without loss
and fully disable the corresponding configuration. Unknown connectors or mixed configurations return a conflict to avoid accidentally removing other capabilities.

Evaluation in automatic mode returns `selected_node`, per-node `candidates`, and the selected node's `plan`. When it does not fit, the `plan` and each candidate's `reason` say what the controller says: `waiting_for_yield` when it would fit once the holders yield, and `reserved_for_sleepers` when it does not fit only because sleepers keep the cards (both worked out with the draft's yield policy), instead of always "does not fit". When it would fit once the holders slept but yield policy keeps the draft from waiting for them, evaluation says so as well: `does_not_wait_for_yield` when the draft never yields, and `held_by_non_yielding` when some holder never yields, with the parameters `node`, `gpus`, `holders` and `blockers` (see [How a New Deployment Gets onto a Shared Card](../how-to/scheduling-center.md#新部署怎样放到共用的卡上)); these two reasons appear in evaluation only.

The evaluate response also has `yield_policy_eligible` (`{ok, reason?}`, shaped like the field of that name in the deployment details, with `reason` `SLEEP_NOT_ENABLED`, `SLEEP_ENGINE_UNSUPPORTED` or `YIELD_POLICY_SHAPE`), worked out from the draft's sleep capability, engine, node count and whether it is PD: the very check that create makes, which the deployment form uses to decide whether a yield policy can be chosen; it is absent until the draft's engine is known. When creating, the form sends the chosen yield policy as it is, and the server refuses it when it does not hold (`SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` / `YIELD_POLICY_SHAPE`). The evaluate response also has `health_checkable` (Boolean): whether a deployment created from this draft can have a health check, by the same rule as create and judged on the operations the deployment will get - for a template draft, the template's declared operations when the model's own are still empty; the deployment form uses it to decide whether the health check switch can be turned on, and it is absent until the model is resolved. The evaluate response also has `priority_scheduling` (`{ok, refusal?, fix_args?}`): whether this draft may turn on request priority scheduling, judged on the arguments the engine will receive, by the same rule as create; `refusal` is `{code, params}` with `code` one of the reasons listed for `priority_scheduling` above, and when the conflict comes from the inference template's arguments, `fix_args` gives the arguments that clear it once added to the custom arguments (such as `["--schedule-policy","fcfs"]`). It is absent until the draft's engine is known.
When creating the deployment, submit `selected_node.uuid` as `node_uuid`, together with `plan.plan.gpus`
and `plan.plan.tp`, and retain the original constraints, such as labels. Creation revalidates the request. If the original node, GPUs, or constraints are no longer valid,
it returns a conflict requiring reevaluation. This confirm-what-was-evaluated flow applies only to a single node and a single replica; automatic placement of multi-node and PD requests does not use it (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)). Evaluation with an explicit node and multi-replica
evaluation for legacy requests still use `plan_scope=per_replica`; this does not imply that the entire replica group can be allocated successfully.

In template details and evaluation responses, `engine_decides_param_keys` lists dictionary keys for which the current effective launch leaves defaults to the engine
instead of filling them for the user: the launch entrypoint or the engine version in the image may not recognize the flag (see the rules in
[Launch Configuration](../explanation/launch-config.md)). Only keys with dictionary defaults are listed. The form displays them as
“Auto.” You can still explicitly enable or disable them; explicit values are emitted as usual and are not rejected. The engine adapter makes this determination;
it cannot be inferred from whether a template specifies a flag. A user's entrypoint override for a new deployment takes precedence over the template entrypoint. Manual commands are not interpreted as
structured engine parameters.

**Submit parameters according to the selected engine's dictionary.** If a request explicitly supplies a value for a key in
`quick_params` but the selected engine's dictionary has no flag for that key (or the key is a
platform parameter that applies only to other engines), the value is rejected (`invalid quick_params: <key> is not supported by <engine>`).
Evaluation returns HTTP 200, `ok=false`, and a field error; deployment creation and clone configuration diff calculation return 400. The only exception is the key's **neutral default value**, which is equivalent to omitting it, such as
`performance_mode=balanced` for SGLang.

Unsupported explicit combinations include:

- SGLang: `enable_chunked_prefill` (both polarities), `rope_scaling`, multimodal JSON keys
  (`limit_mm_per_prompt`, `mm_processor_kwargs`, `media_io_kwargs`, `allowed_local_media_path`),
  `shared_kv_dir`, `kv_offload_read_threads`/`kv_offload_write_threads`, `kv_disk_limit_gib`,
  `async_scheduling=true`, and all vLLM-specific keys in the `performance_advanced` group.
- vLLM: `log_level` (except `info`; this engine's logging level is controlled through env), `enable_metrics=true`,
  `speculative_num_steps`, and `speculative_eagle_top_k`.
- callable: does not publish `param_specs` and accepts no dictionary parameters other than neutral values.

Scripts and third-party clients cannot directly reuse vLLM parameters for SGLang. `engine_decides_param_keys`
(which only removes defaults based on the entrypoint and engine version) complements this rule: the former does not reject any value; the latter rejects values the **engine** cannot express.

### PD Disaggregation Deployments (`placement.pd`)

```json
{"placement": {"pd": {
  "mode": "vllm-nixl",
  "roles": {
    "prefill": {"tp": 1, "units": [{"node_uuid": "n1", "gpus": [0]}]},
    "decode":  {"tp": 2, "pp": 1, "units": [{"node_uuid": "n2", "followers": [{"node_uuid": "n3"}]}]}
  },
  "kv_load_failure": "fail",
  "fallback": "decode"
}}}
```

- `mode`: one of the engine profile's `pd_modes` (`vllm-nixl`, `vllm-ascend-mooncake`, `vllm-mooncake`, `vllm-ppu-mooncake`, `sglang-nixl`, `sglang-mooncake`, `sglang-ppu-ipc`, `sglang-ppu-mooncake`). All unknown fields are rejected.
- Each `unit` is a replica; `gpus` can be omitted. `followers` makes the replica span multiple nodes (ranks 1…N), in which case the role must specify `tp`.
  For each rank, the GPU count is `tp × pp` divided by the number of ranks.
- Allowed values and defaults for `fallback` and `kv_load_failure` depend on the mode: for `vllm-nixl`, `vllm-ascend-mooncake`, and `vllm-mooncake`, `fallback`
  is `decode` (default) or `fail`, and `kv_load_failure` is `fail` (default) or `recompute`. For `sglang-nixl`, `sglang-mooncake`, `sglang-ppu-ipc`, `sglang-ppu-mooncake`, and
  `vllm-ppu-mooncake`, `fallback` only accepts `fail`, and there is no `kv_load_failure`. Values disallowed by the mode return `PD_FALLBACK_INVALID` /
  `PD_KV_LOAD_FAILURE_INVALID`.
- In `GET /api/v1/engines`, each engine's `pd_modes[]` provides these rules: `name`, `vendors`, `roles`, `scalable_to_zero` (roles that can scale to 0
  while running), `tp_rule`, `max_units_per_role`, `fallbacks`, `kv_load_failures` (the first entry is the default), and `unverified`.
- Specifying it together with `nnodes`, `node_uuids`, or `replicas > 1` returns `PD_SHAPE_CONFLICT`; use `placement_mode: "auto"` with units that omit `node_uuid` for automatic placement (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)). Other validation error codes start with
  `PD_` (field `placement.pd`). When the deployment is sleepable (`sleepable`) and KV uses the host RDMA network through UCX, creation and evaluation return
  `PD_SLEEP_RDMA_UNSUPPORTED` (`params`: `mode`, `network`).
  When a node of a **manually named** placement has a kernel that hangs UCX (the node's `ucx_netlink_hang` above) and the mode moves the KV over UCX (automatic placement simply skips such nodes): nothing is refused and create proceeds, while evaluate warns: `PD_UCX_NETLINK_HANG` on an Ethernet parameter network (the management network
  included; the engine will likely never finish starting until IPv6 is off), the softer `PD_UCX_NETLINK_HANG_RDMA` on a host RDMA network.
  Both carry the same `params`: `mode`, `network`, `nodes` (`[{node_uuid, node_name, kernel}]`, only the affected nodes), `names` (their names, comma-separated),
  `fixes` (`[{code, commands[]}]`, the same as the node's `fixes`) and `fix` (the first fix command).
- The `pd` field in deployment details contains `mode`, `fallback`, `kv_load_failure`, `state` (`not_ready|partial|running`), `short`,
  and `roles[]` (`role`, `tp`, `pp`, `desired`, `ready`, `may_be_empty` (the mode allows this role to be empty, meaning it can sleep independently), and `units[]`, including `followers[]`). Instances carry `role`.
  Each unit also has `failed_attempts`, `last_failure`, and `retry_exhausted` (the controller has given up rebuilding it; use the retry endpoint above to retry).
- The `POST /api/v1/evaluate` response also includes `pd.roles[].units[]`: placement, addresses, GPU memory breakdown, and commands for each replica's leader and its `followers[]`.

## Benchmarks

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/benchmarks` | List benchmark tasks | JWT |
| GET | `/api/v1/benchmarks/meta` | Benchmark dictionary (dataset/metric metadata) | JWT |
| POST | `/api/v1/benchmarks` | Create a benchmark task; the optional `priority_class` is the request priority class every request asks the gateway for (not case-sensitive), allowed only for a managed deployment measured from the server, otherwise 400 `BENCHMARK_PRIORITY_CLASS_GATEWAY`, 400 `PRIORITY_CLASS_NOT_FOUND` for a class that does not exist and 403 when the caller is not an administrator; the task object returns it (omitted when none), and the per-request detail's `priority_class` is the class the gateway ran the request at | JWT |
| POST | `/api/v1/benchmarks/probe` | Probe an external benchmark target (connectivity/authentication) | JWT |
| POST | `/api/v1/benchmarks/models` | List models available for benchmarking | JWT |
| POST | `/api/v1/benchmarks/datasets/import` | Import a benchmark dataset | JWT |
| POST | `/api/v1/benchmarks/tokenizers/import` | Import a benchmark tokenizer | JWT |
| GET | `/api/v1/benchmarks/{id}` | Benchmark task details | JWT |
| GET | `/api/v1/benchmarks/{id}/curve` | Benchmark load curve data | JWT |
| GET | `/api/v1/benchmarks/{id}/report` | Download the benchmark report | JWT |
| DELETE | `/api/v1/benchmarks/{id}` | Delete a benchmark task | JWT |
| GET | `/api/v1/benchmarks/{id}/points/{seq}/detail` | Download detailed data for a load point | JWT |
| GET | `/api/v1/benchmarks/{id}/points/{seq}/timeline` | Read a point request timeline (bounded, redacted timings and request identities) | JWT |
| POST | `/api/v1/benchmarks/{id}/cancel` | Cancel a running benchmark | JWT |

## Profiles / GPUs (Estimation Calibration and GPU Inventory)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/gpus` | Cluster GPU inventory and latest metrics, from a snapshot at most 20 seconds old; after an instance, deployment or node change (a card's health, a stopped card, a node going on or offline) it is rebuilt once the 2-second refresh floor has passed. Supports `page`/`page_size`, `q`, `node`, `vendor`, and `model`. `allocation_state` is `available/partial/allocated/unusable/unknown`, distinct from health level. `q` matches substrings of node names/UUIDs, GPU model names, GPU UUIDs, and PCIe addresses; the other filters accept comma-separated values. For `unusable` reasons, see `schedulable/health_status/ownership/foreign_processes`. `sort` is `node/model/vendor/allocation_state/occupants/utilization/memory/temperature/power/fan/bdf/numa`; paginated results include the full `overview` and `facets`. See [GPU Filters and Allocation States](#gpu-筛选与分配状态) | JWT |
| GET | `/api/v1/dgpus` | dGPU instance inventory | JWT |

### GPU Filters and Allocation States

Without pagination parameters, all results are returned. With `page` or `page_size`, the response is `{items,page,page_size,total,overview,facets}`.
`overview` and `facets{nodes[{uuid,name}],models,vendors}` cover all GPUs and are unaffected by filters.

- `q`: a substring of a node name/UUID, GPU model name, GPU UUID, or PCIe address.
- `node` (node UUID), `vendor`, `model`, and `allocation_state`: multiple values are comma-separated.
- `sort`: `node`, `model`, `vendor`, `allocation_state`, `occupants`, `utilization`, `memory`, `temperature`, `power`, `fan`, `bdf`, or `numa`; entries without readings sort first.

`allocation_state` indicates a GPU's allocation state and must not be treated as a health level:

- `available`, `partial`, `allocated`: distinguished by occupancy.
- `unusable`: accepts no tasks at all; see the GPU's `schedulable`, `health_status`, `ownership`, and `foreign_processes` for reasons.
- `unknown`: the node is offline, and readings are from its last report. `available_bytes` is 0, and the GPU is excluded from the allocatable count. Existing allocations and hardware health information are retained.

Memory is booked as `allocated_bytes` (held by awake instances), `sleeping_bytes` (the residue sleeping instances keep), `foreign_bytes` (the memory of processes outside the platform that is counted; 0 when their memory is not known) and `available_bytes` (still allocatable). When the four add up to less than `memory_total_bytes`, the difference is memory withheld for another reason: on a card of an online node it is the headroom kept beside processes outside the platform (the deployment policy's `foreign_headroom_pct` and `foreign_headroom_min_bytes`; no difference when both are 0), and the whole card when the outside processes' memory is not known or the node is offline.

`overview.health` counts GPUs by health level: `ok`, `warning`, and `error`. GPUs with warnings may still serve workloads; GPUs passed through or occupied by processes outside the platform may also have health `ok`.
States refresh with the 20-second snapshot.

## API Keys (Gateway Keys)

New inference tokens belong to the signed-in user; the request body cannot choose their owner. Non-administrators can list, change the call scope of, or revoke only their own tokens. Administrators retain management access to every token. The list carries the recorded owner in `user_id`; `0` explicitly identifies a historical token without a recorded user and is not automatically assigned to an administrator.

Historical tokens retain their existing inference scope and can read only the historical zero-user batch resources created by that same token. To replace one, use the existing creation operation for a personal token, then have an administrator explicitly revoke the old token. Reissuing does not transfer its old batches; administrators can still manage them in the console. Deployment scope restricts inference execution; batch history visibility follows [user ownership](../how-to/batch-api.md#scope-and-limits).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/apikeys` | Role-aware API key list: administrators see all keys, other users only their own. Each item includes a read-only numeric `id` for usage filters and never a secret or hash. Includes revoked keys. Revoked keys include `revoked_at` (UTC time); active keys omit this field. When `all_deployments` is true, the key can call every model service; when false, it can call only the deployments in `deployments` (`[{id,name,served_name}]`). If all of those deployments are deleted, the list is empty and the key can call nothing. `priority` is the key's request priority binding `{class, ceiling, policy}`: an empty `class` means the default class, an empty `ceiling` caps at the key's own class, and `policy` is `within_ceiling` (the default), `fixed` or `down_only`; see [Request Priority](../explanation/request-priority.md) | JWT |
| POST | `/api/v1/apikeys` | Create an API key. `name` must not be empty after trimming leading and trailing whitespace; otherwise, returns 400 `API_KEY_NAME_REQUIRED`. The plaintext value is returned only once, in the creation response. Optional `all_deployments` and `deployment_ids` limit which deployments the key can call: when `all_deployments` is omitted, an empty `deployment_ids` means all deployments and a non-empty one means only those. Contradictory values (true with IDs, or false without) return 400 `API_KEY_SCOPE_INVALID`; deployments that do not exist return 400 `API_KEY_DEPLOYMENT_UNKNOWN` (`params.ids`). An optional `priority` (shaped like the list item's) also binds the request priority; only an administrator may send it, and another user gets 403. An unknown class returns 400 `PRIORITY_CLASS_INVALID` and an unknown policy 400 `PRIORITY_POLICY_INVALID`, and no key is created | JWT |
| PUT | `/api/v1/apikeys/{prefix}/scope` | Change which deployments an active key can call, `{"all_deployments":bool,"deployment_ids":[...]}`. `all_deployments` is required; validation is the same as on creation. Returns the updated key (same shape as a list item). Takes effect from the next request; an unknown or revoked prefix returns 404 `API_KEY_NOT_FOUND` | JWT |
| PUT | `/api/v1/apikeys/{prefix}/priority` | Bind an active key's request priority, `{"class":"gold","ceiling":"","policy":"fixed"}` (shaped like the list item's `priority`). Administrators only; other users get 403. A policy outside `ceiling_policies` drops `ceiling`; a ceiling below the key's own class (the default class when it binds none) returns 400 `PRIORITY_CLASS_INVALID`. Returns the updated key. Takes effect from the next request; an unknown class returns 400 `PRIORITY_CLASS_INVALID`, an unknown policy 400 `PRIORITY_POLICY_INVALID`, and an unknown or revoked prefix 404 `API_KEY_NOT_FOUND` | JWT (administrator) |
| DELETE | `/api/v1/apikeys/{prefix}` | Revoke an API key using the listed 12-character `prefix` (the first 12 characters of the full key, not the full secret). New requests after revocation completes can no longer authenticate with that key, but the record remains to identify historical usage; the prefix is not reused. Success returns `{"status":"revoked"}`. An unknown prefix returns 404 `API_KEY_NOT_FOUND`; revoking an already revoked prefix again returns 200 without changing its original `revoked_at`. After all keys are revoked the gateway does **not** fall back to anonymous access: `/v1` still requires a key and refuses every request until another key is created or an administrator turns on the `inference_open_access` security setting (see [Gateway Authentication](openai-gateway.md#authentication)) | JWT |

## Priority Classes (Request Priority)

A priority class decides the priority the gateway gives a request; see [Request Priority](../explanation/request-priority.md).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/priority-classes` | The classes, largest value first: `items[]` (`name`, `display_name`, `description`, `value`, `builtin`, `default`, `deletable` (whether it can be deleted: not built in, not the default, and bound to no unrevoked key), `created_at`), with `min_value`/`max_value` (the value range, -100 to 100), `policies` (the policies a key may take, which the console shows as given), `default_policy` and `ceiling_policies` (the policies that have a ceiling) | JWT |
| POST | `/api/v1/priority-classes` | Create a class `{"name":"gold","display_name":"Gold","description":"","value":30}`. `name` takes lowercase letters, digits and hyphens, starts with a letter and has at most 32 characters, and `value` is required and within range; otherwise 400 `PRIORITY_CLASS_INVALID` (`params.field`). A duplicate name returns 409 `PRIORITY_CLASS_EXISTS`. Returns 201 with the new class | JWT (administrator) |
| PUT | `/api/v1/priority-classes/{name}` | Change `display_name`, `description` or `value` (omitted fields stay). Changing a built-in class's value returns 409 `PRIORITY_CLASS_BUILTIN`; an unknown class 404 `PRIORITY_CLASS_NOT_FOUND` | JWT (administrator) |
| DELETE | `/api/v1/priority-classes/{name}` | Delete a class; 204 on success. A built-in class returns 409 `PRIORITY_CLASS_BUILTIN`, the default class, one an active key is bound to (as its class or ceiling) or one a batch or evaluation that has not finished uses 409 `PRIORITY_CLASS_IN_USE`, an unknown class 404 `PRIORITY_CLASS_NOT_FOUND` | JWT (administrator) |
| PUT | `/api/v1/priority-classes/{name}/default` | Make the class the default (used by keys bound to no class, the playground and requests without a key); returns the whole list. An unknown class returns 404 `PRIORITY_CLASS_NOT_FOUND` | JWT (administrator) |

## Platform Tokens (Platform API Tokens)

Platform API tokens replace login sessions when calling management APIs and MCP. They are distinct from the gateway keys above, which are used only for `/v1`. Plaintext tokens start with `gwp_` and appear only once, in the creation response. The server stores only SHA-256 hashes. Each user can view and manage only their own tokens, and can do so only within a login session.

After trimming leading and trailing whitespace, a name must be 1–128 UTF-8 bytes (not characters); otherwise, returns 400 `PLATFORM_TOKEN_NAME_INVALID`.

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/platform-tokens` | Current user's tokens: `{items:[{id,name,prefix,scope,created_at,expires_at,last_used_at}], total}`; `last_used_at` updates at most once per minute | JWT (login session) |
| POST | `/api/v1/platform-tokens` | Create a token: `{name, scope?: read\|write, expires_in_days?: 0–3650}` (scope defaults to read; 0 days means no expiration), returns 201 `{token, item}`; invalid `scope` returns 400 `PLATFORM_TOKEN_SCOPE_INVALID` | JWT (login session) |
| DELETE | `/api/v1/platform-tokens/{id}` | Revoke (delete) your own token, effective immediately; returns 204. Returns 404 if the token does not exist or belongs to someone else | JWT (login session) |

## Plugins

The management API of installed signed plugin packages (`.gwpkg`); see [Extension Points](../dev/extensions.md#management-api). These are core routes: never feature-gated, and they work with every plugin down or removed. The AI assistant and MCP cannot read them, and every call writes an audit log record. When the server runs no plugin host, every route answers 503 `PLUGINS_UNAVAILABLE`.

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/plugins` | Installed packages: `{items:[{id, version, publisher, trust, enabled, state, message?, since?, restarts, parts, isolation, roles, permissions, approve_hash, previous?, on_demand, pending?, features, actions}], check}`, sorted by `id`; `check` is the `plugins` diagnostics check, `{id, status, message:{code, params, message}}`, absent with nothing installed; `permissions` is `[{name, risk}]`, sorted by `name`, where `risk` is `low`, `elevated` or `high` as the server classifies it ([Permissions](plugin-protocol.md#permissions)) | `JWT(admin)` |
| POST | `/api/v1/plugins/install?approve=<approval>&enable=<bool>` | The request body is the `.gwpkg` itself (`application/octet-stream`). Without `approve`, or with a wrong one, it answers 409 `PLUGIN_APPROVAL_REQUIRED` whose `params` give `id`, `version`, `approve_hash` and the `permissions` and `roles` it covers (comma-separated), plus the diff from the installed version (from nothing on a first install): `from_version`; `permissions_added`/`permissions_removed` (`permission=risk`); an `_added`/`_removed` pair each for `roles`, `capabilities` (`name@version`), `features`, `routes` (`METHOD pattern access`), `host_setup` (`type vendor_id:product_id`) and `devices`; and `ui_mode` and `isolation` (`old->new` only when changed). Every key is present, empty when there is nothing to list. It installs nothing; with the right one it returns `{plugin, previous?, unchanged}`. `enable` defaults to `false`; installing over an enabled version keeps it enabled | Management session, admin |
| POST | `/api/v1/plugins/{id}/enable` | Enable and start the server part, returning the package; a package that no longer verifies answers 409 `PLUGIN_BROKEN` | Management session, admin |
| POST | `/api/v1/plugins/{id}/disable` | Stop it and keep it from starting, returning the package; its pushed tables stay enforced and its grace runs as for a down plugin | Management session, admin |
| POST | `/api/v1/plugins/{id}/restart` | Stop the server part and start it again as a new instance, so its crash history and backoff start afresh; an on-demand package goes back to `idle` until its next call. Returns the package; a disabled package answers 409 `PLUGIN_DISABLED{id}`, one that no longer verifies or is revoked 409 `PLUGIN_BROKEN`, one without a server part 422 `PLUGIN_UNAVAILABLE` | Management session, admin |
| POST | `/api/v1/plugins/{id}/upgrade?approve=<approval>` | The request body is the new version's `.gwpkg`. Without `approve`, or with a wrong one, it answers 409 `PLUGIN_APPROVAL_REQUIRED` with the install's `params` plus the diff from the installed version: `from_version`, `permissions_added`/`_removed` (`permission=risk`), an `_added`/`_removed` pair for each of roles, capabilities, features, routes, host setup and devices, and `ui_mode` and `isolation` (`old->new` when changed); nothing changes. An unchanged approval hash needs no `approve` when the site sets `plugins.policy.auto_approve_unchanged`. Once approved it stops the old version, snapshots the data and starts the new one, and answers `{plugin, previous, auto_approved, unchanged}` once the new version has stayed up for 60 seconds; otherwise it rolls back automatically and answers 422 `PLUGIN_UPGRADE_ROLLED_BACK{id, version, previous, cause_code}` (`cause_code` is the failure's code, `PLUGIN_UPGRADE_CANCELLED` when a disable, an uninstall or the server's stop ended the gate early). It answers synchronously, in up to about 80 seconds | Management session, admin |
| POST | `/api/v1/plugins/{id}/rollback?restore_state=<bool>` | Go back to the previous version (the list's `previous`) and start it when enabled, returning the package. `restore_state=true` also restores that version's data and data marker from its snapshot | Management session, admin |
| DELETE | `/api/v1/plugins/{id}?purge=<bool>&confirm=<impact digest>` | Stop and uninstall, delete everything it pushed and reapply the holds; returns 204. `purge=true` also moves its data directory (snapshots included) to `.trash/` and deletes its data marker. `confirm` must be the `confirm` of the uninstall impact now (`GET …/impact`): without it, with a wrong one, or when the impact changed after the preview (the plugin pushed again, another version was installed), it answers 409 `PLUGIN_UNINSTALL_CONFIRM_REQUIRED` with `params` `{id, confirm, held_nodes, users_affected, caps_lifted, features_locked, banners, overrides}` (counts apart from `id` and `confirm`) and the body also carries `impact` (as `GET …/impact` answers it); nothing is removed. After an uninstall without purge the package directory and its data stay; uninstalling again with `purge=true` removes them | Management session, admin |
| GET | `/api/v1/plugins/{id}/impact` | What uninstalling it would change now, read from memory, changing nothing: `{id, version?, confirm, tables:[{kind, entries}], nodes, features, caps, banners, overrides, users_affected}`. `tables` are the tables deleted; `nodes` the ids of the nodes whose hold is released (the nodes only it holds, and the nodes held for capacity when no cap is left; when another cap stays but loosens, the nodes it would free are not counted, and `caps` names that cap); `features` the feature keys that become `locked`; `caps` the fleet caps lifted or loosened (`max_nodes_with_cards`, `max_cards`); `banners` the number of its administrator banners shown now; `overrides` the ids of the overrides removed with it; `users_affected` the users its roles or mappings cover (no table kind of schema 1 has them, so 0); `confirm` is the digest of all this together with each table's generation and digest and the package version. It also answers for a package whose package is gone while its pushed rows remain (an uninstall that failed half way), or whose data directory an uninstall kept | `JWT(admin)` |
| GET | `/api/v1/plugins/{id}/state` | Its part of pushed state, read from memory: `{id, installed, verified, now, down_since?, grace_s, clamp_at?, tables:[{kind, schema, generation, digest, entries, pushed_at, next_step?}], overrides}`. Every instant is on the plugin clock (extclock); `down_since` and `clamp_at` (when its grants narrow) appear only while it is down; `next_step` is when one of the table's timelines next takes a step; `overrides` as in the override list. `installed` is `false` for a package whose package is gone while its rows remain | `JWT(admin)` |
| GET | `/api/v1/plugins/{id}/logs` | An SSE stream: first everything kept (the last 256 supervision events, one `state` frame `{seq, at, state, message?, truncated?}` each; the last 1 MiB of stderr, one `stderr` frame `{offset, text, truncated?}`), then each new event or output as it comes. `truncated` says that something before it was dropped. The stream ends when the login ends or loses its administrator role; the assistant cannot read it | Management session, admin |
| GET | `/api/v1/plugins/catalog` | The imported package indexes joined with the installed packages: `{items:[{id, publisher?, installed?, best_version?, min_core_hint?, installable, reason?, refused:[{version, min_core_hint?, message}]}], errors:[{publisher, message}]}`, sorted by `id`. `best_version` is the newest build this server can install; `installable` says it is not installed yet; `refused` are the newer builds this server will not install (such as `PLUGIN_NEEDS_NEWER_CORE`, `PLUGIN_PACKAGE_REVOKED` or `PLUGIN_BELOW_FLOOR`), newest first; with no installable update, `reason` is the refusal of the newest of them; `errors` are the indexes that no longer verify and were left out | `JWT(admin)` |
| GET | `/api/v1/metrics/plugins` | The installed plugins' `gpuworks_plugin_*` metrics in the Prometheus text format (`text/plain; version=0.0.4`), for external Prometheus collection, without internal time-series persistence or stored-catalog entries; the metrics are in [Extension Points](../dev/extensions.md#health-banners-the-diagnostics-check-and-metrics) | `JWT(admin)` |
| GET | `/api/v1/plugins/overrides` | The overrides in force: `{items:[{id, owner, kind, entry, generation, action, value?, by_user, at, reason}]}`, sorted by `id` | `JWT(admin)` |
| POST | `/api/v1/plugins/overrides` | Body `{owner, kind, entry, action, value?, reason}`: loosen one restriction plugin `owner` pushed; returns 201 with the override. `kind` is `node_holds` (`entry` is a node id in decimal, and `action` can only be `delete`) or `fleet_capacity` (`entry` is `max_nodes_with_cards` or `max_cards`, and `action` is `delete`, or `loosen` with the raised cap in `value`); `reason` is required, at most 1024 bytes. A missing or malformed field answers 400 `FIELD_INVALID`; an override that breaks a rule answers 422 `PLUGIN_OVERRIDE_REJECTED` (`params.rule`: `not_restrict`, `no_entry`, `action`, `not_looser`; `reason` for a reason over 1024 bytes; `superseded` when the plugin pushed that entry again before the override was stored) | Management session, admin |
| DELETE | `/api/v1/plugins/overrides/{oid}` | Withdraw an override, so the restriction its owner last pushed is enforced again; returns 204, or 404 `PLUGIN_OVERRIDE_NOT_FOUND` when none exists | Management session, admin |

Fields:

- `trust`: `project` (certified by a project root) or `host` (by an issuer `server.yaml` trusts); absent when the package no longer verifies.
- `state`: when the server part cannot run, `disabled`, `installed` (no server part), `broken` (no longer verifies), `revoked` (its build or certificate is revoked) or `needs_host_setup` (enabled, but root has not applied the host steps it declares on this host, or applied other steps; `message` is `PLUGIN_HOST_SETUP_REQUIRED{id, command}`, where `command` is what to run); otherwise the process state `starting`, `running`, `degraded`, `backoff`, `crashloop`, `stopping`, `stopped` or `failed`, and `idle` for an on-demand server part that is not running. `message` is the reason's code and params (such as `PLUGIN_CRASHLOOP`, `PLUGIN_HANDSHAKE_REFUSED` or `PLUGIN_FILE_MISMATCH`; `PLUGIN_MEMORY_EXCEEDED` with `id`, `rss_mib` and `limit_mib` when a plugin without a cgroup of its own was restarted for its memory), absent when nothing needs saying. A request-body stall restarts the plugin with `PLUGIN_STALLED{id, stalled_s}`; see [Extension Points](../dev/extensions.md#supervision).
- `since`: when it entered its current process state, absent when not supervised; `restarts`: how many processes were started after the first.
- `parts`: the parts it carries, `server`, `node`, `ui`, `store`; `isolation`: `landlock` or `none`.
- `previous`: the newest unpacked version older than the current one, kept for rollback, absent when there is none; `on_demand`: the server part starts on demand; `pending`: a newer build the imported indexes offer, `{version}`, absent when none does; `features`: the feature keys the package declares, `[{key, availability}]` sorted by `key`, where `availability` is `full`, `read_only` or `locked` now.
- `actions`: the actions available now, computed by the server: `enable`, `disable`, `restart`, `uninstall`, `purge` (an uninstall with `purge=true`), `upgrade` (every installed package) and `rollback` (when there is a `previous`).
- `check`: `message` is the result's code and params; `status` is `fail` (`CHECK_PLUGINS_FAILING`: an enabled package is crashlooping, `failed`, `broken`, revoked or `needs_host_setup`), `warn` (`CHECK_PLUGINS_INCOMPATIBLE`: an enabled package needs a capability this server does not offer) or `pass` (`CHECK_PLUGINS_OK`); `message.params.ids` is the package ids, comma-separated.
- An override's `generation`: the owner's generation of the kind when the override was made; `by_user`: the user id of the administrator who made it; `value`: the raised cap of a `loosen`, absent for a `delete`. When a later push of the owner changes the entry, the override is superseded and deleted, and administrators see the banner `PLUGIN_OVERRIDE_SUPERSEDED` for 24 hours.

The codes of a failed plugin call (`PLUGIN_UNAVAILABLE`, `PLUGIN_BUSY`, `PLUGIN_TIMEOUT`, `PLUGIN_ROUTE_FAILED`) are in [Extension Points](../dev/extensions.md#calls-and-the-route-table).

The routes installed packages declare are mounted at `/api/v1/x/<id><pattern>`, authenticated by the route's `access` (`admin`, `admin_session`, `member`; `public` is not authenticated and is rate-limited per client address); see [Extension Points](../dev/extensions.md#plugin-routes). Besides the four codes above (answered 503 with `Retry-After: 5`, 503 with `Retry-After: 1`, 504 and 502): a path not in canonical form 400 `PLUGIN_ROUTE_PATH_INVALID`; a response the server will not pass on (HTML from a route that does not declare `html`, an invalid product error) 502 `PLUGIN_ROUTE_RESPONSE_INVALID{id}`; a body over the route's limit 413 `REQUEST_TOO_LARGE{max_bytes}`; a public route over its rate 429 `TOO_MANY_REQUESTS`; no matching route the core's 404 or 405.

Errors: not installed 404 `PLUGIN_NOT_INSTALLED`; approval needed, a broken package, the same version with other contents (`PLUGIN_VERSION_CONFLICT`), a declared feature key a compiled module or another installed package already provides (`PLUGIN_FEATURE_TAKEN{id, feature, owner}`, checked before approval) a package id matching a compiled module (`PLUGIN_ID_TAKEN{id}`, also checked before approval), plugin data newer than the build (`PLUGIN_STATE_TOO_NEW`), a restart of a disabled package (`PLUGIN_DISABLED`), an uninstall without the right impact digest (`PLUGIN_UNINSTALL_CONFIRM_REQUIRED`), an install of an id being upgraded (`PLUGIN_UPGRADE_IN_PROGRESS{id}`), a rollback without a previous version (`PLUGIN_NO_PREVIOUS_VERSION{id}`) or with `restore_state` and no snapshot of that version (`PLUGIN_SNAPSHOT_MISSING{id, version}`) 409; an upgrade package whose id is not the path's 422 `PLUGIN_UPGRADE_ID_MISMATCH{id, package_id}`; data that could not be snapshot before an upgrade 500 `PLUGIN_SNAPSHOT_FAILED{id}`; installing a package that declares host steps on a platform other than Linux 422 `PLUGIN_HOST_SETUP_UNSUPPORTED{id}`; a package too large 413 `PLUGIN_PACKAGE_TOO_LARGE`; any other refused package 422, with the codes in [Plugin Protocol and Wire Formats](plugin-protocol.md); `enable`, `purge` or `restore_state` not a boolean 400 `FIELD_INVALID`.

The host API that plugin processes call is not under `/api/v1`; its error codes (such as `PLUGIN_ROLE_REQUIRED`, `PLUGIN_HOST_NOT_PERMITTED` and `PLUGIN_HOST_ARGUMENT_INVALID`) are in [Host API clock, readiness, own state and derived keys](plugin-protocol.md#host-api-clock-readiness-own-state-and-derived-keys).

## Providers (Cloud Instance Rental)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/providers/kinds` | Supported cloud provider types, including each provider's credential fields (`credential_fields[].default_value` is an optional creation default for a non-sensitive field; `placeholder` is only a hint), default `server_dial` listening address, `server_dial_supported` (false for AutoDL: Pro instances expose only HTTP ports), `server_dial_auto_port` (true for Vast and RunPod: the driver automatically chooses the listening port and connection address according to the platform's port mappings), and `resources`: what the type connects, `[{type,product?}]`, where `type` is `vm`, `container`, `host`, `kubernetes` or `slurm` and `product` is the vendor's own product name (such as `ECS`, `ACK`, `E-HPC`), instances first, then Kubernetes, then Slurm last; `slurm` appears only for a type whose driver can list the account's HPC clusters (`can_list_hpc_clusters`; Alibaba Cloud's E-HPC), and `kubernetes` likewise follows `can_list_clusters`; `category` is the platform type the type's accounts are filed under (`public_cloud`, `private_cloud`), for a client to group by. The response is `{items,total,categories}`: `categories` are the platform types in the order the console shows them, each `{id,kinds,resources?,kube_cluster?}`, where `kinds` are the `kind`s filed under the type (in the order of `items`); a vendor platform type appears only when some kind is filed under it, and a kind that declares no platform type is filed under `other`; the `kubernetes` and `slurm` items always come last (`kubernetes` first): `kubernetes` has `kube_cluster` true and `resources` `[{type:"kubernetes"}]`, `slurm` has `cluster_kind` `slurm` and `resources` `[{type:"slurm"}]`, and both have `kinds` an empty array, meaning they are added as cluster entries (`POST /kube/clusters`, `POST /slurm/clusters`) rather than as a vendor's account | JWT |
| GET | `/api/v1/providers` | List configured cloud provider accounts; each item's `category` is the platform type its kind connects (`private_cloud` for ZStack Cloud, `public_cloud` for the public cloud vendors), derived from `kind` and not stored separately; `resources` is as in `/providers/kinds`; `instance_count` is the instances found by the last sync (not counting the nodes of managed Kubernetes clusters and the compute nodes of HPC clusters: once connected they are the cluster's nodes, not instances); `node_count` is the compute nodes that joined through the account (the nodes on its instances plus the nodes of its clusters, a machine counted once), the same nodes `GET /nodes?source=provider:<id>` lists; `synced_at` is when the account's instances were last listed successfully (RFC 3339), omitted for an account never synced and left unchanged by a failed sync | JWT |
| POST | `/api/v1/providers` | Add a cloud provider account | JWT |
| GET | `/api/v1/providers/{id}` | Read back for editing: saved endpoint/public_key and non-secret extra fields declared in the dictionary. Does not fill in provider defaults or return secrets. Credential read failures return 500; the list remains available | Login session (platform tokens and AI/MCP cannot read) |
| PUT | `/api/v1/providers/{id}` | Update a cloud provider account | JWT |
| DELETE | `/api/v1/providers/{id}` | Delete a cloud provider account with its instance records and cached Cloud host discovery snapshots in one transaction, rolling back on failure; the instances and clusters in the cloud are not released and the nodes that joined stay; cluster entries connected from it remain, without `source` and with `pool_ids` unchanged, and are treated as clusters connected on their own from then on (`can_apply` and `can_remove` false, `operation_blocked` `no_source`) | JWT |
| POST | `/api/v1/providers/{id}/sync` | Sync instances under this account: insert/update listed instances and remove records for instances no longer present in the cloud (retain associated nodes and set out-of-band recovery to `stopped`). The returned `count` and `instances` are as in the instance list and exclude the nodes of managed Kubernetes clusters and the compute nodes of HPC clusters. A successful sync records the current time as the account's `synced_at`. Listing failures return 424 and leave instance records and `synced_at` unchanged | JWT |
| POST | `/api/v1/providers/test` | Test credentials that are not saved yet: `{kind, endpoint?, api_key?, public_key?, private_key?, extra?}`. Check results are streamed over SSE one by one, and a failed test still answers 200; see [Provider Connection Test](#云平台连接测试). An unregistered `kind` returns 400 `PROVIDER_KIND_UNKNOWN` (`kind`) | JWT |
| POST | `/api/v1/providers/{id}/test` | Test a saved account. The body is optional; when given it is `{endpoint?, api_key?, public_key?, private_key?, extra?}`, and its non-empty fields replace the saved values for this test only (a secret left blank keeps the saved value, as when updating an account). Nothing is stored and saved secrets are never returned. The response is the same as above; see [Provider Connection Test](#云平台连接测试). An unknown account returns 404 | JWT |
| GET | `/api/v1/providers/{id}/clusters` | Managed Kubernetes clusters under this account, read live from the cloud provider and not stored: `{items:[{external_id,name,region,version,type?,type_label?,state,running,public_api,gpu_pools:[{id,name,gpu_model,gpu_count,nodes,vendor,in_entry}],entry?:{uuid,name},connectable,agent_image_source}],total}`; `connectable` says whether an entry can be made from it: the cluster is running, has GPU node pools, and has no entry yet; `type` is which of the vendor's products the cluster is (`ack_managed`, `ack_dedicated`, `ack_edge`, `ack_lingjun`, `ack_registered`, `doks`) and `type_label` its name in English and Chinese, both absent when the driver cannot tell; a cluster whose Pods run on virtual nodes (ACK Serverless, ACS) has no node an agent can run on and is not listed; `agent_image_source` is the agent image registry an entry made from it defaults to (by region); `entry` is the cluster entry already created from it, and `in_entry` says whether the node pool is already in that entry (the DaemonSet runs on its nodes). Returns 501 if the driver has no cluster capability, and 424 on a cloud provider error | JWT |
| GET | `/api/v1/providers/{id}/hpc-clusters` | HPC clusters (Alibaba Cloud E-HPC) under this account, read from the cloud and not stored. The regions are asked one by one, which takes seconds on cold connections, so the server keeps the last listing in memory (for up to 24 hours) and a request gets it at once, `listed_at` being when it was read; a listing older than 15 seconds is read again behind it (once per account at a time), waited for up to 1 second and answered when it is in by then, otherwise the old one is answered with `refreshing: true` and the console asks again 2 seconds later; with no listing yet the request waits for the read; `?fresh=1` waits for a new read (used after a sync). It sits beside the managed Kubernetes list (`/clusters`) rather than merging into it: they are different cloud products with their own permissions, fields and failures, and one failing does not affect the other. E-HPC is first asked in which regions it is offered (`ehpc:ListRegions`; when the key lacks that permission or the call fails, every region of the account is asked instead), then `ehpc:ListClusters` is read region by region on that region's own E-HPC endpoint only (every page; the answer of another region or of the central endpoint is never borrowed, because the response carries no region and a borrowed cluster would be filed under the region asked), a cluster is listed once, each region succeeding or failing on its own so one does not take the others down: a region without the E-HPC service, or where the account has not activated E-HPC (a code of the `NotActivated`/`NotOpen`/`NotEnabled` family), is skipped and has no clusters; when some regions fail (throttling, a RAM policy that does not allow that region, the network) the clusters of the others are returned with `partial`; only when every region fails does the whole listing fail. `{items:[{external_id,name,region,scheduler,scheduler_version,state,state_tone,vendor_state,running,compute_nodes?,gpu_nodes?,connectable,can_apply,unsupported_reason?,not_connectable_reason?,entry?:{uuid,name}}],total,access,permission?,code?,partial,failed_regions:[{region,code,message,permission?}]}` (by region, then ID): `access` is `ok`, `permission_missing` (no region could be read and all were refused by RAM: `permission` is the missing action `ehpc:ListClusters`, `items` is empty, not an error) or `not_activated` (no region could be read and the account has not activated E-HPC anywhere: `code` is the cloud's own word, `items` is empty); `partial` true means some regions were not read, they are in `failed_regions` (`code` and `message` are the cloud's, `permission` when RAM refused), and clusters in them may be missing from `items`: a cluster with no GPU node yet, or one not synced yet, is listed all the same; `name` is `external_id` when the cloud gave none; `scheduler` is the lower-case scheduler (`slurm`, `pbs`…) and `scheduler_version` the version the cloud reports; `state` is the cluster's state as a stable code (`uninit`, `creating`, `initing`, `pending`, `running`, `exception`, `stopping`, `stopped`, `releasing`, and `unknown` for a word the cloud does not document), which the client words in its language; `state_tone` is the colour meaning of the state (`success`, `info`, `neutral`, `danger`), `vendor_state` the cloud's own word, and `running` that it reports the cluster usable; `compute_nodes` is how many compute nodes the cloud lists in the cluster right now, in any state (creating, running, abnormal; the three `Nodes` counts of E-HPC `ListClusters` added up, from the same live read; absent when the listing carries no count: unknown, not 0), and since the cloud does not say which of them have a GPU it is not a GPU node count; `gpu_nodes` is the cluster's compute nodes with a GPU in the account's last instance sync, given only when there is no `compute_nodes` (a sync can be old, so the live count wins; also absent until a sync has listed the cluster); `connectable` says whether a Slurm entry can be made from it: the cluster runs, its scheduler is Slurm and it has no entry yet; `can_apply` says whether, once made, the platform can run the setup on the Slurm manager node through the cloud account (the driver can run commands on the Slurm manager node; the cluster's region and scheduler come from its nodes in the last sync and, with none, from the listing this response was made from, which reads the cloud once for the whole response, not once per cluster); `unsupported_reason` is why a cluster with no entry cannot be connected at all, for now only `scheduler_not_slurm` (its scheduler is not Slurm), which the client turns into its note instead of comparing schedulers itself (a cluster still being created whose scheduler the cloud has not named yet does not count); `not_connectable_reason` is why a Slurm cluster with no entry cannot be connected yet because of its state: `cluster_starting` (being created or initialized, connectable once it runs), `cluster_pending` (awaiting setup), `cluster_exception` (failed, to be dealt with in the E-HPC console first), `cluster_stopped` (stopped or stopping, to be started in the console first), `cluster_releasing` (being released) or `cluster_state_unknown`; at most one of it and `unsupported_reason` is given, the scheduler's first; `entry` is the Slurm entry already made from it. A driver without the HPC cluster capability answers 501, every region failing (other than by RAM refusal or non-activation) answers 424 | JWT |
| POST | `/api/v1/providers/{id}/instances` | Rent a new instance; cloud provider errors return 424 (501 if unsupported) | JWT |
| GET | `/api/v1/providers/{id}/instances` | List the account's own instances; the nodes of the account's managed Kubernetes clusters (which the cloud also lists as instances) are not included: they join with the cluster, and powering, releasing or resizing them returns 409 `PROVIDER_INSTANCE_IN_CLUSTER`; the compute nodes of the account's HPC clusters (Alibaba Cloud E-HPC) are not included either: they join with the Slurm cluster entry and are listed as clusters by `GET /providers/{id}/hpc-clusters`, and powering, releasing, resizing or installing on one returns 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER`; `actions` lists what the instance allows now (some of `power_on`, `power_off`, `install`, `delete`), and a client offers exactly those instead of deciding itself | JWT |
| DELETE | `/api/v1/providers/{id}/instances/{externalID}` | Release/delete an instance; remove its record after the cloud provider confirms. Cloud provider errors return 424 (501 if unsupported), leaving the instance record unchanged; a managed cluster node returns 409 `PROVIDER_INSTANCE_IN_CLUSTER` (`cluster`) and an HPC cluster's compute node 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER` (`cluster`), neither calling the cloud provider | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/power` | Power on/off; cloud provider errors return 424 (501 if unsupported); a managed cluster node returns 409 `PROVIDER_INSTANCE_IN_CLUSTER`, an HPC cluster's compute node 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER` | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/resize` | Change the instance specification; cloud provider errors return 424 (501 if unsupported); a managed cluster node returns 409 `PROVIDER_INSTANCE_IN_CLUSTER`, an HPC cluster's compute node 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER` | JWT |
| POST | `/api/v1/providers/{id}/install` | Install the agent on a rented instance; connection parameters are the same as for node creation. If the Pod needs to restart, returns 409 first and modifies it only after explicit permission. See [Cloud Instance Installation and Restart Confirmation](#云实例安装与重启确认); if the instance is a node of a managed Kubernetes cluster, returns 409 `PROVIDER_INSTANCE_IN_CLUSTER` (`cluster`); a compute node of an HPC cluster returns 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER` (`cluster`; they join with the cluster entry, not one by one) | JWT |

For cloud provider endpoints, any cloud provider API or instance SSH error returns 424 Failed Dependency, with the reason in the `{error}` body. These endpoints do not use 502/504: CDNs and tunnels such as Cloudflare replace origin 502/504 bodies with their own error pages, losing the reason.

### Provider Connection Test

`POST /api/v1/providers/test` and `POST /api/v1/providers/{id}/test` answer with SSE (`Content-Type: text/event-stream`).
A request that is itself wrong (unparseable JSON, an unregistered `kind`, an unknown account) gets an ordinary error before any event is sent. Once the stream starts, the HTTP status is always 200 and the outcome is in the events.

One `check` event is sent as each check finishes, then one `done` event:

```text
event: check
data: {"seq":4,"name":"tcp_connect","status":"failed","duration_ms":12,"code":"PROVIDER_TCP_REFUSED","hint":"tcp_refused"}

event: done
data: {"ok":false,"failed":["tcp_connect"]}
```

- `check`: `{seq, name, status, duration_ms, code?, detail?, hint?}`. `status` is `ok`, `failed` or `skipped`. `code` appears only on `failed` and is a stable error code. `hint` is the key a client picks its guidance text by. The keys of `detail` differ by check (see the table below). The cloud provider's own reply, credentials and the values of proxy variables never appear in an event; the cause of a failure is written to the server log with credential values replaced.
- `done`: `{"ok":true}`; `{"ok":false,"failed":[...]}` when checks failed, with `failed` listing the failed checks in the order they were sent; `{"ok":false,"code":"PROVIDER_TEST_UNAVAILABLE"}` when the test could not run to its end (the saved configuration cannot be read, the driver of that kind is not registered, or the whole test took longer than 2 minutes).
- A client that disconnects cancels the test, and no further events are sent.

Checks run in the order of the table below. When one fails, the checks that depend on it are `skipped` with `hint` `depends_failed`, and `detail.depends` names the check they depend on; the other checks still run.

| `name` | What it checks | `detail` when it passes | `code` when it fails (`hint`; `detail`) |
|---|---|---|---|
| `endpoint_format` | The endpoint is an http(s) address with no user name or query string; left empty, the default address of the kind is used | `scheme`, `host` | `PROVIDER_ENDPOINT_INVALID` (`endpoint_empty`, `endpoint_shape`) |
| `credentials_shape` | Every required credential field of the kind is filled in | `fields` (the number of required fields) | `PROVIDER_CREDENTIAL_MISSING` (`credential_missing`; `field`) |
| `dns_resolve` | The server can resolve the endpoint's host name | `ips`, plus `literal: true` when the endpoint is itself an IP | `PROVIDER_DNS_FAILED` (`dns`; `host`) |
| `tcp_connect` | The server can connect to the endpoint's port | `address`, `rtt_ms` | `PROVIDER_TCP_REFUSED` (`tcp_refused`), `PROVIDER_TCP_UNREACHABLE` (`tcp_unreachable`), `PROVIDER_TCP_TIMEOUT` (`tcp_timeout`) |
| `tls_handshake` | The TLS handshake and certificate verification of an https endpoint | `server_name`, `cert_days_left`; `mode: "plain_http"` for http | `PROVIDER_TLS_FAILED` (`tls`) |
| `proxy_detect` | The proxy environment variables of the server process, and whether requests to the endpoint go through a proxy | `proxy: "none"`, or `proxy: "env"`, `variables` (the names of the variables that are set, sorted) and `used` | `PROVIDER_PROXY_FAILED` (`proxy`): the proxy setting cannot be used |
| `clock_skew` | The `Date` of the endpoint's response is within 5 minutes of the server's clock | `skew_seconds`; `date_header: "absent"` or `"unparseable"` when there is no `Date` or it cannot be parsed | `PROVIDER_CLOCK_SKEW` (`clock_skew`; `skew_seconds`), `PROVIDER_HTTP_UNREACHABLE` (`http`) |
| `auth_request` | One validation request to the cloud provider with these credentials | None; `auth` and `hosts` for ZStack | `PROVIDER_AUTH_FAILED` (`auth`); `ZSTACK_*` error codes for ZStack |

When requests to the endpoint go through a proxy, what the server must reach is the proxy: `dns_resolve` and `tcp_connect` check the proxy's address instead, with `detail` `target: "proxy"` when they pass (`tcp_connect` also has `rtt_ms`) and `PROVIDER_PROXY_FAILED` (`proxy`; `target: "proxy"`) when they fail; `tls_handshake` is `skipped` with `hint` `via_proxy`; when the proxy refuses to connect to the endpoint, `clock_skew` fails with `PROVIDER_PROXY_FAILED`.

For a kind with no endpoint (Alibaba Cloud, whose driver picks addresses by region), `endpoint_format` and `dns_resolve` through `clock_skew` are `skipped` with `hint` `not_applicable`. Such a skip does not hold back later checks, and `auth_request` still runs.

ZStack Cloud has four more checks after `auth_request`, which fail with `ZSTACK_*` error codes:

| `name` | What it checks | `detail` |
|---|---|---|
| `protocol_shape` | Which generation of the GPU query protocol the Cloud speaks | `shape` (`modern`, `intermediate`, `legacy`, `gpu_query`; `unsupported` on failure), `allocation_field` (`present`, `absent`, `unobserved`), `cpu_reservation_config` (`bound`, `unbound`, `unprobed`, `error`); `missing` as well on failure |
| `api_endpoints` | Whether the read-only endpoints the product uses exist | `absent`, `denied`, `gpu_query: "pci_fallback"` (each only when it applies); `missing` or `endpoint` on failure |
| `read_permissions` | Whether the read-only queries the product actually sends are allowed | `queries: "all"`; on failure `denied` (with `hint` `zstack_permission`) or `query` |
| `resource_summary` | The number of resources read | `hosts`, `gpus`, `gpus_free` |

### Cloud Instance Installation and Restart Confirmation

`POST /api/v1/providers/{id}/install` installs the agent on a rented instance. Requirements and error codes for `server_url` are the same as for [node creation](#创建节点的连接参数).

`install_dir` defaults to `/opt/gpuworks` when omitted. An explicit value must be an absolute path beginning with `/` and must not be the root directory; `~` is not expanded. Otherwise, returns 400 `INSTALL_DIR_INVALID` (`dir`) before creating the node. If the specified directory is not writable, installation fails without fallback. The actual installation directory is recorded on the node for out-of-band restarts and reinstallation.

`no_systemd: true` makes the Linux agent use nohup without registering `gpuworks-agent.service`, following the same rules as [SSH Checks and Installation](#ssh-检查与安装).

- Vast and RunPod have `server_dial_auto_port=true`: when using `connect_mode=server_dial` with `server_dial_address` empty, an already mapped TCP port with no listener is selected automatically, and both the listening port and public connection address are saved.
  An explicit `server_dial_listen` takes precedence for port selection, but that port must already be mapped.
- AutoDL has `server_dial_supported=false` and does not support this mode.
- Alibaba Cloud Assistant does not handle `server_dial`; automatic selection uses SSH instead. When Cloud Assistant is online, it adds the platform public key to root's `authorized_keys` before installation through SSH.

When RunPod needs an exposed port or the platform public key, it first returns 409 `INSTANCE_RESTART_REQUIRED`; the Pod has not yet been modified.
`params.need` is `port`, `ssh_key`, or `ssh_key_port`; `agent_dial` may also require the public key to be added.
After confirmation, retry with `allow_instance_restart:true` to modify and restart the Pod. **All ports are renumbered**.
Installation failures return 424 `{error}`, with reasons requiring action, such as SSH login or instance authorization issues.

## Web Terminal

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/nodes/{uuid}/terminal` | Open a web terminal session (WS upgrade; host or container target) | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/terminal/settings` | Terminal capabilities and limits for this node (kill switch, concurrency limit, etc.) | JWT(admin) |
| GET | `/api/v1/terminal/sessions` | Terminal session audit records (metadata only; terminal content is not retained) | JWT(admin) |

## Networks

See [Configure Node Networks](../how-to/configure-networks.md).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/networks` | Network list (including attachments, resolved node count, and count of nodes with notices) and role/type dictionaries (`types`: `host_ethernet` / `host_rdma` / `accelerator_rdma` / `accelerator_hccs`; `type_roles`: roles each type can serve). The first entry is the management network (`id` 0, `builtin: true`) | JWT |
| POST | `/api/v1/networks` | Create a network (name, role, type, CIDR, and interface name wildcard). Accelerator RDMA can serve only the compute role; otherwise, returns `NETWORK_TYPE_ROLE_INVALID` | JWT(admin) |
| GET | `/api/v1/networks/{id}` | Network details: attachments, resolved nodes (interfaces/IPs/HCAs/hints and the nodes' user-defined `labels`), and the latest internal test. `id` 0 is the management network, whose nodes include `roles_off`; it cannot be modified, deleted, or attached (returns 404), but internal tests can be started | JWT |
| GET | `/api/v1/networks/{id}/search` | Search a network's resolved nodes (`kind=nodes`) or configured attachment targets (`kind=attachments`). Optional `q` matches a case-insensitive substring. For node searches, repeat `role` or `source` to match any selected value within each field. Role, source and keyword conditions apply together. Returns `{"ids":[...]}` with node UUIDs or attachment IDs as strings. With no keyword or filters, all IDs in the selected set are returned. | JWT |
| PATCH | `/api/v1/nodes/management-roles` | Batch-update management network roles: `{"node_uuids":[...],"allow":[...],"deny":[...]}`. Changes only the listed roles, with all nodes in one transaction; returns `{"updated":N}` (the number of nodes actually changed). Returns 404 `MANAGEMENT_ROLES_NODE_NOT_FOUND` if a node does not exist | JWT(admin) |
| PUT | `/api/v1/nodes/{uuid}/management-roles` | Set the roles that this node's management network does not serve (`{"roles_off":[...]}`); returns the node's network view. Applies only to new instances | JWT(admin) |
| PUT | `/api/v1/networks/{id}` | Modify a network | JWT(admin) |
| DELETE | `/api/v1/networks/{id}` | Delete a network and its attachments and test records (running instances are unaffected) | JWT(admin) |
| POST | `/api/v1/networks/{id}/attachments` | Attach to a node (optionally with interface/IP/HCA overrides) or a node label | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}` | Modify overrides for a node attachment | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}/label` | Update the label attachment’s `label_key` and `label_value`, preserving its ID and attachment time. Returns 400 for a direct node attachment, 404 when the attachment is missing or belongs to another network, and 409 when the network already has the requested selector | JWT(admin) |
| DELETE | `/api/v1/networks/{id}/attachments/{attachmentID}` | Remove an attachment | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests/estimate` | Use the same request body without starting a test: returns the resolved node count and node-pair count. Card tests also return `card` (`rounds`, `probes`, maximum duration `seconds`, and limit `max_seconds`). Requests that would be rejected when starting a test are rejected here with the same errors | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests` | Start an internal test (`kind`: tcp / rdma_tool / card_ping / card_matrix; rdma_tool requires a host RDMA network, while card_ping (pings between accelerator cards with matching indices) and card_matrix (a matrix of all cards) require an accelerator RDMA or accelerator HCCS network. HCCS card tests use `hccn_tool -hccs_ping`; RoCE card tests use `-ping`. Returns `NETWORK_TEST_TOO_LONG` if rounds × 3 seconds exceeds 10 minutes. `scope`: all / nodes / label; `id` 0 tests the management network and supports only tcp). Returns 202 and a run record (`node_uuids` lists the tested nodes, fixed when the test starts and available during execution even before `pairs` exists). Executed by the controller | JWT(admin) |

In network details, `attachments[].unresolved` is true when the node targeted by a direct attachment is absent from the network’s resolved nodes.

The latest internal test reports failed node pairs in `latest_test.failed_count` and partially passing pairs in `partial_count`. On each pair, `rails[].passed` gives the outcome of each rail. The pair’s `rail_count` and `rail_passed_count` give the total and passing counts (omitted when zero). Card tests count only card pairs that were actually probed. For Host RDMA, `avg_rail_gbps` is the mean of successful rail measurements. Historical runs with only an aggregate have no mean. A passing pair whose measured bandwidth is below one tenth of the fastest valid reading in the run carries `slow: true`.

## Settings / Network Presets

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/updates` | Check for updates to the server, image catalog, and template catalog. `refresh=true` forces a new check; without a forced check, `auto_check=false` reads only the previous result. Returns `update_available`; the server also includes `self_upgrade` and `self_upgrade_blocker`. Still returns 200 and `available=false` when no source is available, with `unavailable_reason` set to `unpublished/invalid/unreachable`. See [Update Check Results](#更新检查结果) | JWT(admin) |
| POST | `/api/v1/updates/prefetch` | The login page asks the server to start checking for updates in the background: requires no login, always returns an empty 204 response, and reveals no results. Does nothing if a recent check exists (a success within 1 hour or a failure within 5 minutes), a check is already running, or automatic checks are disabled. The number of outbound checks made by the server therefore stays the same regardless of call frequency | None |
| PUT | `/api/v1/updates/channel` | Switch the update channel with `{"channel":"release"\|"nightly"}`: writes `update_channel` to `server.yaml` and takes effect immediately; returns `{channel}`. Returns 409 `UPDATE_CHANNEL_READ_ONLY` if server.yaml does not exist, or 400 `UPDATE_CHANNEL_INVALID` for other values | JWT(admin) |
| PUT | `/api/v1/updates/auto-check` | Enable or disable automatic checks for new versions with `{"enabled":true\|false}`: writes `auto_check_updates` to `server.yaml` (removes the key when enabled) and takes effect immediately; returns `{enabled}`. When disabled, `prefetch` no longer checks for updates or fetches notices. Automatic checks include the installation ID, server version, and agent version distribution; node counts and GPU models are included only when `usage_report` is enabled (see [Configuration](configuration.md#镜像目录与镜像拉取)). Returns 409 `UPDATE_AUTO_CHECK_READ_ONLY` if server.yaml does not exist, or 400 `UPDATE_AUTO_CHECK_INVALID` if the value is missing or not Boolean | JWT(admin) |
| GET | `/api/v1/usage-report` | User Experience Improvement Program status `{enabled, editable, last_day?, last_attempt?: {at, day, ok, error?}, next_due_at?}`: whether you participate (`usage_report` in `server.yaml`), whether you can change participation in the console, the UTC day covered by the most recently delivered report, this process's latest delivery attempt, and the estimated time of the next report ([User Experience Improvement Program](usage-report.md)) | JWT(admin) |
| PUT | `/api/v1/usage-report/enabled` | Join or leave with `{"enabled":true\|false}`: writes `usage_report` to `server.yaml` (removes the key when joining), takes effect immediately, and returns the status. Returns 409 `USAGE_REPORT_READ_ONLY` if server.yaml does not exist, or 400 `USAGE_REPORT_INVALID` if the value is missing or not Boolean | JWT(admin) |
| GET | `/api/v1/usage-report/preview` | Raw content of the next report (`application/json`, identical to the bytes actually sent, without consuming in-process counters). Returns 404 `USAGE_REPORT_UNAVAILABLE` if the server does not participate in the improvement program | JWT(admin) |
| GET | `/api/v1/diagnostics/debug-session` | The current (or last) debug collection `{session, limits}`: `session` is `{id, started_by, started_at, until, max_bytes, deployment_ids, nodes: [{uuid, name, state, reason?}], open, ended?, ended_at?, server_log_bytes, request_bytes, requests}`, `null` before the first; `limits` are the duration and size defaults and bounds. A node's `state` is `collecting`, `skipped` (`reason` `offline` or `unsupported`) or `failed`; `ended` is `deadline`, `size_limit` or `stopped` | JWT(admin) |
| POST | `/api/v1/diagnostics/debug-session` | Start a debug collection `{duration_seconds?, max_bytes?, deployment_ids?, node_uuids?}` (omitted values take the defaults, 600 seconds and 10 GiB); returns 201 with the state. A duration outside 60–3600 seconds or a size outside 1 MiB–50 GiB returns 400 `DEBUG_SESSION_INVALID` (`params.field`, `params.max`); an unknown deployment or node 400 `DIAGNOSTICS_SCOPE_UNKNOWN`; a collection already running 409 `DEBUG_SESSION_OPEN`; a server without diagnostics 404 `DIAGNOSTICS_UNAVAILABLE` | JWT(admin) |
| DELETE | `/api/v1/diagnostics/debug-session` | Stop the debug collection and return the state; with none running, returns the last one's state | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles` | The kept diagnostics collections `{bundles, limits}`, newest first; each is `{id, requested_by, created_at, finished_at?, from, to, deployment_ids, instance_uuids, node_uuids, doctor, state, error?, size_bytes, items: [{id, path, status, code?, params?, message?, evidence?, bytes}]}` with `state` `running`, `ready` or `failed`. Each item is a check: `path` is its place in the archive and `id` the kind of content (`server_log`, `server_host_identity`, `node_agent_log`, `node`, `records`, on a control-plane cluster also `member`, `member_file`, `member_index`, `members`, and `server_member`, `server_role_events`, `server_leader_changes`, `server_cluster_status` for what is placed under `server/` when the membership cannot be read, ...); `status` is `pass` (collected whole), `warn` (cut at its size limit, `DIAGNOSTICS_ITEM_TRUNCATED`), `skip` (not read: node offline `DIAGNOSTICS_NODE_OFFLINE`, or an agent too old for the read `DIAGNOSTICS_AGENT_UNSUPPORTED`) or `fail` (`DIAGNOSTICS_ITEM_READ_FAILED`, `DIAGNOSTICS_ITEM_STAGE_FAILED`, with the read's own error text in `evidence`; a control-plane cluster member that did not give its part is `DIAGNOSTICS_MEMBER_UNAVAILABLE` with `path` `members/<member>` and an `evidence` that starts with `unreachable`, `timeout`, `refused`, `unsupported`, `busy` or `failed`; the collection is still `ready`: a collection with such an item lacks that member's part; `DIAGNOSTICS_ITEM_READ_FAILED` with `path` `members` means this member's cluster membership could not be read and no other member was asked); collections written by an older server still list | JWT(admin) |
| POST | `/api/v1/diagnostics/bundles` | Start collecting `{from?, to?, deployment_ids?, instance_uuids?, node_uuids?, doctor?}` (RFC 3339; both omitted is the last hour; `doctor` defaults to `true`); returns 202 with the collection in `state: "running"`. An invalid window or one over 7 days, or more than 64 nodes or instances, returns 400 `DIAGNOSTICS_BUNDLE_INVALID`; an unknown deployment or node 400 `DIAGNOSTICS_SCOPE_UNKNOWN`; a collection already running 409 `DIAGNOSTICS_BUNDLE_RUNNING` | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles/{id}` | One collection's state; 404 `DIAGNOSTICS_BUNDLE_NOT_FOUND` when there is none | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles/{id}/download` | Download a finished collection (`application/gzip`, `gpuworks-diagnostics-<id>.tar.gz`); 404 `DIAGNOSTICS_BUNDLE_NOT_FOUND` when it is missing or not finished | JWT(admin) |
| GET | `/api/v1/experience` | Experience mode status `{enabled, ready, port, user, password?, error?}`: whether the experience environment runs, whether its console answers, its port (the console port +10), its account, and its password while it runs; `error` says why it stopped by itself. The environment is a separate process and data directory holding only simulated data. Returns 404 `EXPERIENCE_UNAVAILABLE` on the environment's own console and on a server without a data directory | JWT(admin) |
| PUT | `/api/v1/experience/enabled` | Turn the experience environment on or off with `{"enabled":true\|false}`: turning on clears the previous data, generates a new password and starts it (no change if it already runs); turning off stops the process. Returns the status; 400 `EXPERIENCE_INVALID` if the value is missing or not Boolean, 409 `EXPERIENCE_PORT_IN_USE` (`params.port`) if another program holds the port, 409 `EXPERIENCE_FAILED` (`params.reason`) if it cannot start for another reason | JWT(admin) |
| POST | `/api/v1/experience/reset` | Clears the running environment's data and starts it again, keeping its password; 409 `EXPERIENCE_FAILED` if it is not running | JWT(admin) |
| POST | `/api/v1/experience/password` | Generates a new password for the running environment; sessions of the old one end. 409 `EXPERIENCE_FAILED` if it is not running | JWT(admin) |
| GET | `/api/v1/notices` | Header notification bell `{notices: [{id, severity, kind, title?, body?, url?, published_at, read}], unread}`: publisher-signed notices applicable to this installation (`kind` is `security` / `release` / `event`; `title`/`body` provide plain text by language). Sorted by urgency, then by time; `read` and `unread` are calculated per caller | JWT |
| POST | `/api/v1/notices/read` | Mark notices as read with `{"ids": [...]}` (1–128 IDs, each ≤64 bytes; otherwise, 400 `NOTICE_IDS_INVALID`). Records only the caller's own read status; repeated marking has no side effects. Returns 204 | JWT |
| GET | `/api/v1/console/manifest` | What the console shell shows beyond its own pages, `{edition, features, nav, banners}`. The server decides every value and the console only renders it; see [Console manifest and extension routes](#console-manifest-and-extension-routes). A community server always returns `{"edition":"community","features":[],"nav":[],"banners":[]}` | JWT |
| POST | `/api/v1/updates/server` | Download a release package, back up and replace the server, and restart. On acceptance, returns 202 `{from,to,backup}`; confirms or rolls back on the next startup. See [Server Upgrade Restrictions](#服务端升级限制) | JWT(admin) |
| GET | `/api/v1/updates/server/versions` | Pass the saved `channel` to receive verified server versions, the host platform, Git commits when available, package digests and reasons a version cannot be installed. Records retain publication order; both same-channel and cross-channel lists keep only the current version and newer builds. `latest` marks the channel head. | JWT(admin) |
| POST | `/api/v1/updates/server/upload` | Same as above, but the request body is an uploaded release archive (512 MiB limit). Returns 400 `SERVER_UPGRADE_ARCHIVE_INVALID` if it is not a usable release package (including versions predating console upgrades: they cannot confirm or roll back their own startup, so `detail` advises using `gpuworks upgrade --archive` instead) | JWT(admin) |
| GET | `/api/v1/settings` | Current values of all settings (excluding `telemetry_export` and `branding`, which are read and written only through their dedicated endpoints, and the three internal scheduling fencing keys) | JWT |
| PUT | `/api/v1/settings/{key}` | Modify a single setting; the three internal keys used for scheduling fencing, `scheduling.generation`, `scheduling.owner`, and `scheduling.confirmation`, cannot be written through this route and return 403 (other `scheduling.*` keys are unaffected); `telemetry_export`, `branding`, and `security` return 400 `SETTING_HAS_OWN_ROUTE` (`security` goes through `PATCH /api/v1/security/settings`, since a whole-object replace would silently clear switches the body does not name). Only administrators can modify `deployment` (pending scheduling timeout, instance startup timeout, and event retention; otherwise, 403 `ADMINISTRATOR_REQUIRED`). The request body overlays the current values and changes only the keys it contains. If the merged values are out of range, returns 400 `FIELD_INVALID` without writing | JWT(admin) |
| GET | `/api/v1/security/settings` | Security switches: `playground_preview_network` (whether the playground HTML preview can make network requests; defaults to `false`) and `inference_open_access` (whether `/v1` accepts requests without an API key; defaults to `false`. When on, a request without a key is served anonymously while a request that presents one is still verified; the gateway treats an unreadable setting as off), see [Gateway Authentication](openai-gateway.md#authentication); storage read failure returns 500 `SECURITY_SETTINGS_UNAVAILABLE` rather than fabricated defaults | JWT |
| PATCH | `/api/v1/security/settings` | Modify the switches above, changing only fields present in the body; failure to read the current policy returns 500 `SECURITY_SETTINGS_UNAVAILABLE` without writing; success returns the committed policy | admin |
| GET | `/api/v1/branding` | Branding: `product_name`, `console_title`, `login_description`, and `logo` (a base64 `data:` URL, PNG/JPEG/WebP/SVG). All four fields are always present; an empty string uses the built-in default (the client renders default text in the UI language). No login is required because the login page must display these before login | None |
| PATCH | `/api/v1/branding` | Modify branding, changing only fields present in the body; an empty string restores that field's default. After trimming leading and trailing whitespace, `product_name` allows up to 32 characters, `console_title` up to 64, and `login_description` up to 200 (only this field allows line breaks); control characters are prohibited. The decoded `logo` is limited to 128 KiB. Invalid input returns 400 `FIELD_INVALID` (`field`, `detail`) without writing. Returns all saved values | admin |
| GET | `/api/v1/telemetry/export` | OTLP export configuration and status for gateway traces: `enabled/endpoint/headers/sample_ratio/trace_link`; only header names are returned. `status` includes cumulative `exported/dropped/failed` since server startup, current `queued`, the latest error `last_error`, and the configuration error `setting_error`. See [Trace Export Settings and Status](#trace-导出设置与状态) | JWT |
| PUT | `/api/v1/telemetry/export` | Save the entire configuration and apply it immediately. Unlisted headers are deleted; missing or `null` header values retain their stored values. For fields and errors, see [Trace Export Settings and Status](#trace-导出设置与状态) | JWT(admin) |
| POST | `/api/v1/telemetry/export/test` | Send a test span named `gpuworks test span` using the draft settings in the request body (same as PUT; does not save and does not require `enabled`). Sends only once, without retries: 200 `{ok, trace_id, traces_url, error}`. If `ok=false`, `error` contains the Collector's response or a connection error. Invalid draft settings return the same 400 errors as PUT | JWT(admin) |
| GET | `/api/v1/pull/source-rules` | Source rules in application order: `{id, position, kind, match, endpoints[], allow_upstream, control_plane, note, node_requirements, matched_nodes}`. `kind` is derived from `match`: a value with `http://`/`https://` is a site rule `url` (a model site such as `https://huggingface.co`); otherwise, it is an image rule `image`. `control_plane` makes the control plane also use this rule to resolve addresses when fetching model metadata (available only for site rules) | JWT |
| POST | `/api/v1/pull/source-rules` | Create a source rule. `allow_upstream` is required; `control_plane` defaults to `false`. For a site rule, `match` is the site's root address (without a path), and endpoints are http(s) addresses that can include paths. Invalid rules return 400 `PULL_SETTING_INVALID` (`field`: `match`, `endpoints`, `control_plane`, `note`, `requirements`) | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/{id}` | Replace an entire rule | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/order` | Reorder: `{"ids":[...]}` lists every rule exactly once | JWT(admin) |
| DELETE | `/api/v1/pull/source-rules/{id}` | Delete a rule | JWT(admin) |
| POST | `/api/v1/pull/source-rules/test` | Test a site rule using a draft `{match,endpoints,allow_upstream,node_uuid,repo}` without saving. `repo` defaults to `Qwen/Qwen3-0.6B`; returns the status and elapsed time for each address. See [Source Rule Connectivity Test](#来源规则连通测试) | JWT(admin) |
| GET | `/api/v1/pull/sync-groups` | Sync group list: `{id, name, node_requirements, rate_limit_kib_s, matched_nodes}` | JWT |
| POST | `/api/v1/pull/sync-groups` | Create a sync group (node conditions select members; rate limiting is optional) | JWT(admin) |
| PUT | `/api/v1/pull/sync-groups/{id}` | Replace an entire sync group | JWT(admin) |
| DELETE | `/api/v1/pull/sync-groups/{id}` | Delete a sync group | JWT(admin) |
| GET | `/api/v1/pull/metadata-sources` | `?source=` takes `huggingface` or `modelscope`: addresses the console browser uses to read model metadata, consisting of candidates from site rules with “Used by the management node for model metadata” selected (in order, with the original site last unless the rule disables upstream fallback): `{source, bases[]}`. `bases` is empty if rules prohibit every address for the site. Other `source` values return 400 `FIELD_INVALID` | JWT |
| GET | `/api/v1/pull/proxies` | Proxy entries in matching order (a node uses the first entry that selects it): `{id, position, http_proxy, https_proxy, no_proxy, username, has_password, control_plane, note, node_requirements, matched_nodes}`. Addresses contain no login credentials, and passwords are never returned. `control_plane` routes the control plane's outbound requests through this proxy too (uses the first selected entry) | JWT |
| POST | `/api/v1/pull/proxies` | Create a proxy entry: `{http_proxy, https_proxy, no_proxy, username, password, control_plane, note, node_requirements}`, with at least one proxy address. Use scheme://host:port (`http`/`https`/`socks5`/`socks5h`; defaults to `http` if omitted); including login credentials in the address returns 400. `no_proxy` uses the same syntax as the NO_PROXY environment variable. Passwords are stored encrypted. Invalid input returns 400 `PULL_SETTING_INVALID`. After saving, each online node receives one update containing only the network portion | JWT(admin) |
| PUT | `/api/v1/pull/proxies/{id}` | Replace an entire proxy entry; omit `password` to retain the stored value, or supply an empty string to clear it | JWT(admin) |
| PUT | `/api/v1/pull/proxies/order` | Reorder: `{"ids":[...]}` lists every entry exactly once | JWT(admin) |
| DELETE | `/api/v1/pull/proxies/{id}` | Delete a proxy entry | JWT(admin) |
| GET | `/api/v1/pull/credentials` | Credentials (one entry per host): `{id, host, username, has_secret}`. Image registries use a username and password; model sites use only a token (`username` is empty, and the token is sent as `Authorization: Bearer` for downloads and metadata retrieval). Passwords and tokens are never returned | JWT(admin) |
| POST | `/api/v1/pull/credentials` | Create credentials (`host`, `username`, `secret`), stored encrypted. `host` is normalized to a host without a scheme or path (Docker Hub variants normalize to `docker.io`), with one entry per host | JWT(admin) |
| PUT | `/api/v1/pull/credentials/{id}` | Replace credentials; an empty `secret` retains the stored value | JWT(admin) |
| DELETE | `/api/v1/pull/credentials/{id}` | Delete credentials | JWT(admin) |
| POST | `/api/v1/pull/credentials/{id}/test` | Test credentials from the control plane (through its outbound proxy): credentials with a username log in to an image registry at `/v2/`; token-only credentials are validated with Hugging Face's `GET /api/whoami-v2`. Returns 200 `{ok, message}`; rejection returns 422 `CREDENTIAL_LOGIN_REJECTED` (`host`), and connection failure returns 502 `CREDENTIAL_LOGIN_FAILED` (`host`, `detail`) | JWT(admin) |
| GET | `/api/v1/pull/registry-credentials` | Removed: this route and its `POST`, `PUT/DELETE {id}`, and `POST {id}/test` return 410 `SETTING_REMOVED` (`route` is `/api/v1/pull/credentials`) | JWT(admin) |
| GET | `/api/v1/server/listen` | Server listen addresses: saved `console`/`agent`/`gateway` addresses, currently open `running` sockets, `restart_pending` (saved addresses require a restart to take effect), `config_path`, and `editable`/`overridden` (not editable if the server starts without server.yaml or is overridden by `--addr`) | JWT(admin) |
| GET | `/api/v1/server/outbound-policy` | Outbound private-host policy: configured `restrict_private_hosts` (false by default), `allow_private_hosts`, `editable`, and a `revision` based only on policy values; no other configuration fields are returned | JWT(admin) |
| PUT | `/api/v1/server/outbound-policy` | Atomically save the required `restrict_private_hosts`, `allow_private_hosts`, and `revision`, returning the updated policy. Changes persist to server.yaml and apply immediately. Returns 400 `SERVER_OUTBOUND_POLICY_INVALID`, 409 `SERVER_OUTBOUND_POLICY_CONFLICT` / `SERVER_OUTBOUND_POLICY_READ_ONLY`, or 503 `SERVER_OUTBOUND_POLICY_UNAVAILABLE`; unrelated settings and comments are preserved and files that cannot be edited safely are left unchanged | JWT(admin) |
| POST | `/api/v1/server/outbound-policy/check` | Read-only check of the bare hostnames or IP addresses in `{hosts:[]}`. Returns `{items:[{host,status}],total}`, with `allowed`, `needs_allow`, `blocked`, or `unresolved`. Limited to 32 entries, 253 bytes per entry, a 16 KiB body, and 10 seconds total. URLs, ports, CIDRs, and embedded credentials are rejected. Checks DNS and the current outbound policy without connecting to the upstream or writing the allowlist; it does not prove reachability. Invalid requests return 400 `SERVER_OUTBOUND_POLICY_INVALID`. DNS addresses and resolver details are not returned | JWT(admin) |

Sync-group create and rename require an exact case-sensitive unique name after trimming, up to 64 UTF-8 bytes. Duplicate names return 400 `PULL_SETTING_INVALID` with `params.field=name` and `params.reason=name_duplicate`. Validation errors carry a stable `reason` and arguments such as `max`; English `detail` remains for older clients and diagnostics. Clients localize by reason.

### Update Check Results

`GET /api/v1/updates` queries release sources concurrently and uses the first index whose signature passes verification. Results for the same channel are reused for 1 hour; `refresh=true` checks again immediately.

- `channel`, `channel_editable`: the current channel and whether switching is allowed.
- `auto_check`: the automatic check switch. When `false`, returns only the result of the last manual check; if no check has ever run, returns an empty response without `sources` or `unavailable_reason`. A forced refresh still performs a check.
- Source information: the winning source and `{url,ok,latency_ms,error}` for sources that have responded. Slow sources that have not responded are omitted; the UI does not display source details.
- `server`, `image_catalog`, `template_catalog`: each includes `current`, `latest`, and `update_available`. Templates also include `compatible`; the server also includes `installable`, `self_upgrade`, `self_upgrade_blocker`, and `install_dir` (the installation directory containing `server.yaml`; omitted when there is no such directory).

The server marks `update_available` only when the channel version is newer. The two catalogs must also differ from the current content and have a release date no earlier than the current build, to avoid marking old catalogs as updates.
When no source is available, the response is still 200 with `available=false`; `unavailable_reason` distinguishes the cases:

- `unpublished`: a source is reachable, but the channel has no index.
- `invalid`: content fails signature verification or parsing, for example, because a login gateway intercepts it.
- `unreachable`: no source responds, for example, due to network, proxy, DNS, timeout, or server errors.

### Server Upgrade Restrictions

`POST /api/v1/updates/server` downloads the release package for the current platform, validates it against the signed index, backs up and replaces the server, and restarts. Returns 202 `{from,to,backup}`; confirms or rolls back on the next startup.

Online update checks, version choices and installation requests use the same ordering rule; changing channels does not permit older versions. When both IDs are formal `vX.Y.Z` versions, compare version numbers; otherwise compare build times. Unreadable times retain the original update-check behavior: a different version is treated as an update. The database migration number in the signed index is not an online installation requirement. Uploaded packages keep their existing validation and are not subject to this online ordering restriction.

- 409 `SERVER_UPGRADE_UNAVAILABLE`: self-upgrade is unavailable; `reason` is `no_service_manager`, `not_installed`, `not_installed_binary`, `cluster_member` (a control-plane cluster member is not upgraded from the console), or `not_writable`.
- 409 `SERVER_ALREADY_AT_RELEASE`: already at the target version; 409 `SERVER_NEWER_THAN_RELEASE`: the target build is not newer than the current version.
- 404 `UPDATE_NOT_PUBLISHED`: the channel has no package for the current platform.
- 502 `UPDATE_DOWNLOAD_FAILED`: no source provides a package matching the size and SHA-256 in the index. `file` identifies the file, and `detail` gives each source's failure reason; the running service is not replaced.

### Trace Export Settings and Status

`GET /api/v1/telemetry/export`

OTLP export settings and runtime status for gateway request traces: `enabled`, `endpoint` (the Collector's OTLP/HTTP address), `headers` (lists only `{name}`; values are never returned), `sample_ratio` (sampling ratio for traces initiated by the gateway, 0..1, defaulting to 1 if never set), and `trace_link` (a link to an external tracing system containing `{trace_id}`; can be empty).

`status`: `active`, `traces_url` (the actual destination address), cumulative `exported`/`dropped` (discarded because the queue is full)/`failed` (rejected or delivery failed) since server startup, `queued`, `last_error` and `last_error_at`, `last_export_at` (Unix milliseconds, or `null` if absent), and `setting_error` (why stored settings cannot be applied, such as decryption failure).

`PUT /api/v1/telemetry/export`: save all settings and apply them immediately: `{enabled, endpoint, headers:[{name, value}], sample_ratio, trace_link}`. A header without `value` (or with `null`) retains the stored value for the same name; stored headers not listed are deleted. Omitted `sample_ratio` defaults to 1. Header values are stored encrypted.

An invalid address returns 400 `TELEMETRY_ENDPOINT_INVALID`; an invalid, duplicate, or exporter-managed header name returns 400 `TELEMETRY_HEADER_INVALID` (`name`); a new header without a value returns 400 `TELEMETRY_HEADER_VALUE_REQUIRED` (`name`); an out-of-range ratio returns 400 `TELEMETRY_SAMPLE_RATIO_INVALID`; a link that is not an http(s) address containing `{trace_id}` returns 400 `TELEMETRY_TRACE_LINK_INVALID`. Unknown fields return 400.

### Source Rule Connectivity Test

`POST /api/v1/pull/source-rules/test` accepts `{match,endpoints,allow_upstream,node_uuid,repo}` to test a draft without saving it.
Each call targets one node; when you select 1–5 nodes in the console, it makes separate calls in parallel. `repo` defaults to `Qwen/Qwen3-0.6B`.

Probes use the same proxy, `no_proxy`, and credentials as actual downloads and access each address in parallel, with a 10-second timeout per address:

- Hugging Face-compatible sites: first fetch `/api/models/<repo>/revision/main`, then read the first byte of `/<repo>/resolve/main/config.json`; gated repositories may reject access only on the file request.
- `modelscope.cn`: access `/api/v1/models/<repo>`.

Returns `{source,repo,node_uuid,node_name,outcomes:[{base,status,code,latency_ms,error}]}`.
`status` is `ok`, `auth_required` (401/403; credentials required), `http_error`, or `unreachable`.
Invalid drafts return 400 `PULL_SETTING_INVALID`; a nonexistent node returns 404, and an offline node returns 409 `NODE_OFFLINE`.
If a node cannot execute the probe, returns 502 `SOURCE_PROBE_FAILED` (`detail`), for example, because the agent version is too old.

### Removed Settings Endpoints

`PUT /api/v1/settings/image_registry_rewrites`, `image_registry_mirrors`, and image acceleration preset settings return 410 `SETTING_REMOVED`; image sources are now expressed through `/pull/source-rules` (in the table above in this section). `PUT /api/v1/settings/network` and `network:<node-id>` also return 410 `SETTING_REMOVED` (`route` is `/api/v1/pull/proxies`): proxies are now entries selected by node labels, and URL rewrites are now site rules. During upgrades, old global and per-node settings are converted automatically (global rewrites become site rules without node restrictions that apply to the control plane; node overrides become rules and proxy entries restricted to that node and placed first, preserving each node's effective proxy). Network acceleration presets (`GET /api/v1/network/presets`) have been removed.

## Metrics / Logs

### Logs, Metrics, and Event Timelines

Logs and event timelines use cursors to query earlier records, while metrics use time ranges; the `page` pagination described above does not apply:

| Endpoint | Queries and Pagination |
|---|---|
| `GET /api/v1/logs/search` | `limit` defaults to 200, with a maximum of 5000; `cursor_ts` and `cursor_id` mark the pagination position; `with_total=1` includes the total number of matches |
| `GET /api/v1/metrics/query` | `range` is the query duration and `step` is the sampling interval, both in Go duration strings, defaulting to `6h` and `1m`, respectively. Filter by labels such as `node`, `gpu`, `gpu_uuid`, `instance`, `benchmark`, and `point` |

`GET /api/v1/instances/{uuid}/events` and `GET /api/v1/deployments/{id}/events` return historical event timelines,
sorted newest first, with the response `{"items": [...]}`. `limit` defaults to 100, with a maximum of 500; for the next page, pass
`before=<last-id-on-previous-page>`. An invalid cursor returns `400 EVENT_CURSOR_INVALID`; an invalid page size returns `400 EVENT_LIMIT_INVALID`.

Timelines **merge consecutive duplicate events when read**: for consecutive records within the same instance that have identical `kind`, `code`, `state`, and `phase`,
only the latest record is returned. Large numbers of repeated progress reports in older databases therefore appear as fewer state milestones.
Records with `code` carry detailed values in `params` and no longer also return the raw `data`; historical records from before the code mechanism still include `data`. A record that enters `failed` carries the structured failure reason in `params.failure` (shaped like an instance's `runtime_failure`); `params.detail` remains the English fallback.


| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/metrics/query` | Query time-series metrics (`metric`, `range`, `step`, and `node`/`gpu`/`gpu_uuid`/`instance`/`benchmark`/`point`/`iface`/`hca`/`role` matchers). Returns `{"items": [{"labels": {…}, "points": [[unix_ms, value], …]}], "total"}`, with one series per label combination. If no data exists and the metric name is absent from the metric catalog (`GET /api/v1/monitor/metrics`), returns 400 `METRIC_UNKNOWN`; `params.similar` provides the closest registered names. A metric in the catalog with no data in the time window returns an empty result normally | JWT |
| GET | `/api/v1/logs/search` | Search/tail logs (`q`, `instance` prefix, `instances` (a comma-separated set of exact UUID matches, up to 64, mutually exclusive with `instance`), `node`, `level`, `source`, `from`/`to`, `cursor_ts`/`cursor_id`, `limit`, `with_total`; paginated in reverse arrival order. An omitted time boundary leaves that direction unrestricted; when both are omitted, queries all logs that have not been cleaned up. The list and total use the same range; by default, the page explicitly passes the last 24h). Returns `{"items": [{"id", "instance", "node", "level", "source", "time_ms", "line"}], "total", "total_capped", "truncated"}` (the last three fields appear as needed) | JWT |
| GET | `/api/v1/logs/context` | Context window around a single match (`instance`, `ts`, `id`, and `before`/`after` from 0–1000), equivalent to grep -C. Returns `{"items": [log entries], "match_index"}`; `match_index` is the anchor row's index in `items`, or `-1` when the anchor is absent from that partition (already pruned by retention, or a `ts` from a different day than the `id`), in which case `items` still holds the real rows around that position and the endpoint still returns 200. Callers must check `match_index >= 0` before indexing, or only compare against it | JWT |
| GET | `/api/v1/logs/histogram` | Counts by time bucket × level (same filters as search, including `instances`, plus `bucket` in milliseconds, 1s–24h). Returns `{"items": [{"start_ms", "counts": {"error": N, …}}], "total"}`; buckets align to UTC | JWT |
| GET | `/api/v1/monitor/overview` | Cluster snapshot for the monitoring center (`range` ∈ `15m\|1h\|6h\|24h\|168h`, default `1h`, or a custom `from`+`to` range as described below; signal rows report only facts—values, details, and recent trends, without severity; service table rows carry `placement` (see [Service Monitoring Response](#服务监控返回值)) and each deployment's `health` check, sorted by health: failed, timed out, no available replicas, checking, healthy, disabled, then by name within each category; `state_counts`, the service count per state: one entry for every state, zeros included, in the state summary's order; GPU memory bar, cluster tok/s, and capacity events) | JWT |
| GET | `/api/v1/monitor/deployments/{id}` | Service monitoring snapshot; `range` is `15m/1h/6h/24h/168h` (default `1h`), or use `from`+`to`. Includes requests, latency, replicas, `health`, `log_instances` for log queries, and metric `sources` (engine/gateway); PD also includes fallback, Prefill statistics, and `kv_transfer` under `pd`. Unknown deployments return 404; deleted deployments return 404 `MONITOR_DEPLOYMENT_DELETED` with `deployment_id`, `name`, and `served_name` in `params`. See [Service Monitoring Response](#服务监控返回值) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/requests` | Per-request view: `view=live` (default) or `history`; `limit` is 1–500 (default 50 for live), with optional `key` and `instance` filters, and history may also be filtered by `priority_class`. History also supports `range` or paired `from`+`to` (Unix milliseconds), `cursor`, and `min_ms/max_ms`; `results` accepts `completed/length/client_closed/interrupted/error`, with multiple values separated by commas. Records include `result/segments/pd/trace_id` and `priority_class` (the class the gateway ran the request at; empty for records from before priority classes and for requests in flight); paginate with `next_cursor`. See [Per-Request Queries](#逐请求查询) | JWT |
| GET | `/api/v1/monitor/requests/lookup` | Find finished requests across deployments; specify exactly one of `trace_id` (32 hexadecimal characters) or `request_id`. Optional paired `from`+`to` (Unix milliseconds, up to 24 hours), and `limit` of 1–500 (default 100); returns `requests/deployments/window`. Without a time range, locates requests by the gateway trace time or searches the last hour; requests discarded by sampling cannot be found. See [Find Requests Across Deployments](#跨部署查找请求) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/latency` | End-to-end latency, stage durations, and generation speeds for complete responses within `range`: `requests` is the count; `end_to_end` and `segments` include `mean_ms/p50_ms/p95_ms`; `speeds` includes `prefill_tok_s/thinking_tok_s/answering_tok_s/decode_tok_s`. Unaffected by log sampling. See [Latency Aggregation Rules](#延迟汇总口径) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/signals` | Series for the deployment detail metrics panel: `start` (Unix ms, required), `end` (defaults to now), and `step` (such as `30s`, optional). The window is not limited to the Monitor Center's fixed ranges; when it would hold more than 600 points, `step` is raised and the value used is in `step_ms`. Returns `{start_ms,end_ms,step_ms,sources,concurrency,kv,throughput}`; the three series groups carry `labels.metric` `running`/`waiting`, `usage_pct`, and `prompt_tok_s`/`generation_tok_s`. `running`, `prompt_tokens`, and `generation_tokens` each use the engine's values and fall back to the gateway's when the engine has no data; `sources` says which, with the same meaning as `sources` in service monitoring. Engine series carry an `instance` label; the gateway counts per deployment, not per instance, so a series taken from it has none. `waiting` and `usage_pct` can only come from the engine. Not cached. An invalid `start`, `end`, or `step` returns 400 `MONITOR_RANGE_INVALID` (`detail`); unknown or deleted deployments behave as in service monitoring | JWT |
| GET | `/api/v1/monitor/distributions` | Distribution of requests that finish within `range`: `{x, y, n}` for two grids, `input_output` (input bucket × output bucket) and `uncached_ttft` (uncached input bucket × time-to-first-token bucket); `token_buckets` gives bucket lower bounds and `ttft_bounds_ms` gives time-to-first-token bucket upper bounds. `coverage` gives the total request count, count with usage, and count reporting cache hits; `deployment` is optional and defaults to the entire cluster; unknown deployments return 404 | JWT |
| GET | `/api/v1/monitor/metrics` | Metric catalog; returns `{items:[{series,name,source,kind,unit,quantiles,labels,description}],total}`. `source` is `engine`, `gateway`, `gpu`, `dgpu`, `node`, `network`, `kv_disk`, or `benchmark`; inference histograms expand into quantile gauges (`quantiles:true`), `_sum`, and `_count`. See the [Inference Metric Glossary](inference-metrics.md) for field definitions | JWT |
| GET | `/api/v1/monitor/series` | Metric browser (`metric` required; `range` ∈ `15m\|1h\|6h\|24h\|168h`, default `1h`, or `from`+`to` (`step` defaults to approximately 240 points, in multiples of 10 seconds); `by` ∈ `node\|deployment\|instance\|gpu`; `fn` ∈ `avg\|sum\|max\|rate`, default `avg`; `step` defaults to a value derived from `range` and cannot be lower than that value; returns 400 if results exceed the series/point limits; unknown metrics return `[]`). Series with a `node` label also carry a display-only `node_name`, and series with a `deployment` label carry `deployment_name`; for a deleted node or deployment these are the names it had when deleted, with `node_deleted` / `deployment_deleted` set to `"true"`; labels neither side recognizes are left unchanged | JWT |

Custom time ranges for monitoring charts: `overview`, `deployments/{id}`, `deployments/{id}/latency`, `distributions`, and `series` all accept paired `from`+`to` values (Unix milliseconds), mutually exclusive with `range`. The interval must be between 1 minute and 30 days, and `to` cannot be in the future; otherwise, returns 400 `MONITOR_RANGE_INVALID` (`detail`). Charts, distributions, latency breakdowns, and signals are calculated for the interval ending at `to`, without caching. Current state, including service lists, replicas, and health checks, still reflects the present.

### Service Monitoring Response

`GET /api/v1/monitor/deployments/{id}` returns request reconciliation, a time-to-first-token breakdown, charts, and replica information.
`range` supports `15m`, `1h` (default), `6h`, `24h`, and `168h`; alternatively, use paired `from`+`to` values. Unknown deployments return 404; deleted deployments return 404 `MONITOR_DEPLOYMENT_DELETED` (`params`: `deployment_id`, `name`, `served_name`).

- `deployment.placement` (same shape as `placement` on each row of the monitoring center's service table): where the deployment runs, as facts only with no display text; the UI formats them per language. `nodes` are node names in node-rank order (replicas listed one by one, each replica's leader first, every node once); `nnodes` is the node count of the widest replica, 1 for single-node; `gpus_per_node` is the most GPUs any one instance holds; `tensor_parallel` appears only when the engine's launch command states a tensor-parallel degree (`--tensor-parallel-size`, `--tp`, … for vLLM and SGLang). The platform does not know a callable recipe's parallel scheme, and omits the degree when instances state different ones (for example PD roles) rather than deriving it from GPU or node counts. With no instances yet, `nodes` is `[]` and both counts are 0.
- `log_instances`: UUIDs of live replicas and replicas recently stopped within the window, up to 64; usable with `/logs/search?instances=...`.
- `sources`: indicates whether `ttft`, `e2e`, `prompt_tokens`, `generation_tokens`, `running`, `tpot`, `queue_time`, `prefill_time`, `decode_time`, and `kv_hit_rate` (prefix cache hit rate; the gateway value counts only requests that stated a cache hit count, see [Gateway Cache Hit Rate](inference-metrics.md#gateway-cache-hit-rate)) come from `engine` or `gateway`; signals without data are omitted. See [Inference Metrics](inference-metrics.md#网关指标) for definitions.
- `health`: same as the [health check](#健康检查) in deployment details.

The deployment detail metrics panel does not read this endpoint. It reads `GET /api/v1/monitor/deployments/{id}/signals` (see the table above) and picks a source per signal. When an engine reports only some metrics (for example running/waiting requests and cache usage, but no token counters), the panel's token throughput therefore comes from the gateway and agrees with `charts` here. The single-instance panel (the metrics page of instance details) shows only that instance's own engine series: the gateway does not count per instance, so the deployment-wide gateway figure is never presented as one instance's.

PD deployments also return `pd`:

- `effectiveness_pct`, `fallback_rate_pct`, `fallbacks` (counts by fallback reason), `prefill_waiting_avg`, and `avg_prompt_tokens`.
- `prefill_stage_avg_ms` and `prefill_stage_p95_ms`: mean and P95 time the gateway waits for prefill, both over the last 5 minutes; P95 comes from the request segment histogram.
- `kv_transfer.exposed`: whether the engine exports KV transfer metrics; for `vllm-ascend-mooncake` and `vllm-mooncake`, this is `false`, and all other transfer fields are `null`.
- Other `kv_transfer` fields: per-transfer `avg_ms`, `p95_ms`, and `avg_mb`; transfer speed `mb_per_s`; and counts of `failed` (decode pull failure) and `expired` (prefill KV lease expires without being pulled) within the window. Values are `null` when no data is available.

### Find Requests Across Deployments

For `GET /api/v1/monitor/requests/lookup`, specify exactly one of `trace_id` or `request_id`.
A trace ID is 32 hexadecimal characters, case-insensitive, and queries all requests in that trace; a request ID queries the corresponding request.
`limit` is 1–500, default 100; the time range is determined in this order:

1. If you provide `from` and `to`, queries use the received time. Provide both together (Unix milliseconds, no more than 24 hours apart).
2. If no range is specified and the gateway generated the trace, first search from 10 minutes before to 1 minute after the trace time; if no matches are found, search the last hour.
3. Otherwise, search the last hour.

Returns `{as_of,from_ms,to_ms,window,requests,deployments}`.
`window` identifies the window source: `requested`, `trace_time`, or `default`.
`requests` has the same structure as the per-request view, ordered by received time, newest first; `deployments` contains `{id,name,deleted}` for the associated deployments: `name` is the served model name; a deleted deployment has the name it had when deleted and `deleted: true`; old deployments deleted before names were kept have an empty name.
Normal requests discarded by sampling because they exceed the per-second log budget cannot be found. Invalid parameters return 400.

### Per-Request Queries

For `GET /api/v1/monitor/deployments/{id}/requests`, select a `view`:

- `live` (default): requests in progress, ordered by longest wait for the first token, then thinking, then answering. `counts` gives the `waiting`, `thinking`, and `answering` counts for all in-flight requests.
- `history`: finished requests returned newest first; pass `next_cursor` as `cursor` for the next page.
  Use `range` to query a period ending now, or provide paired `from` and `to` (Unix milliseconds).
  The response's `from_ms` and `to_ms` give the actual range; `range_total` is the total number of requests finished at the gateway within that range, counted in 5-minute buckets and unaffected by filters or log sampling.

Both views support `limit` (1–500, default 50 for live), `key` (a single API key), and `instance` (a single replica).
History also supports end-to-end duration filters `min_ms` and `max_ms`, time-to-first-token filters `min_ttft_ms` and `max_ttft_ms` (keeping only streamed replies that produced a first token), and comma-separated `results`:
`completed` for normal completion, `length` for length truncation, `client_closed` for client exit, `interrupted` for connection interruption, and `error` for errors.
Unknown deployments return 404; invalid parameters or cursors return 400.

Key fields in each record mean the following:

- `result` is the request outcome; `segments[{kind,ms,stage}]` gives stage durations, which sum to the end-to-end duration or elapsed in-flight duration; `stage` is the part a segment belongs to: `first_token` (before the first token, summing to `ttft_ms`), `decode` or `tail`; empty for a reply that does not stream and for a stream that ended without a token.
- `source` is `api`, `playground`, `batch` or `evaluation` (an evaluation measured from the server).
- `ttft_ms` is the time from receipt to the first generated output, reasoning included, `null` for a reply that does not stream, one still waiting, or one that ended without a token; `tpot_ms`, `prefill_tok_s` and `decode_tok_s` are the decode time per output token and the Prefill / Decode speeds (the engine's decode time over the output tokens when the engine reports it, otherwise the time from the first token to the last over the tokens after the first), `null` when a figure they need is missing or the request is in flight.
  See [Request Stages](request-timeline.md#2-阶段) for stage names, boundaries, and calculations.
- `pd` has a value only for PD requests and is `null` otherwise. It includes `prefill_instance_uuid`, `prefill_ms`, `prefill_queue_ms`, `prefill_compute_ms`, and `fallback`;
  on fallback, the prefill instance is empty, and unreported durations are `null`. With concurrent handoff, `prefill_ms` is `null`; with sequential handoff, it is also `null` while the request is still in prefill. Once forwarded to decode, this stage duration can be returned even if the request has not yet finished.
  Requests still in prefill appear in the live view with `dispatch_ms` set to `null`. See [PD Requests](request-timeline.md#6-pd-分离部署) for details.
- `trace_id` is a 32-character hexadecimal W3C trace ID: it inherits a valid `traceparent` from the caller, or is generated by the gateway otherwise. `deployment_id` identifies the deployment, and `weight` is the sampling weight.
- `received_ms` is an absolute Unix timestamp in milliseconds; all other timestamps are relative to when the gateway receives the request. Events that have not occurred or were not observed have `null` values. See [Timestamp Definitions](request-timeline.md#1-记录了哪些时间点).

### Latency Aggregation Rules

`GET /api/v1/monitor/deployments/{id}/latency` aggregates complete responses within `range` (normal completion or length truncation):

- `requests`: number of complete responses; `end_to_end{mean_ms,p50_ms,p95_ms}`: end-to-end duration, or `null` when there are no requests.
- `segments[]`: ordered by stage; each entry contains `kind`, `mean_ms`, `requests`, `p50_ms`, and `p95_ms`.
  Means include all complete responses, treating an absent stage as 0, so the stage means sum to the end-to-end mean; counts and quantiles include only requests with that stage, and quantiles use quarter-octave buckets.
- `speeds`: `prefill_tok_s`, `thinking_tok_s`, `answering_tok_s`, and `decode_tok_s`, calculated as total qualifying tokens divided by total duration, or `null` when no data is available.

Statistics are unaffected by log sampling; see [Latency and Speed Calculation Rules](request-timeline.md#4-汇总端到端延迟拆解与速度) for inclusion criteria.
Unknown deployments return 404; invalid `range` values return 400.

## Analytics (Usage Analysis)

For long-term statistics and data retention, see [ADR 0055](../adr/0055-long-term-statistics.md).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/data-retention` | Data retention settings: `{value, limits, server_time_zone}`. Fields in `value`: `metrics_hours` and `logs_hours` (raw metric samples and container logs); `request_log_days` and `request_log_max_mb` (per-request records in ux.db, cleaned up when either the age or capacity limit is reached; the current day is retained); `usage_hourly_days` and `usage_daily_days` (token usage in the main database); `stats_hourly_days` and `stats_daily_days` (service quality, GPU utilization, GPU-hours, and the scheduling activity in stats.db); the `quality`, `gpu`, and `gpu_time` switches (the scheduling activity is always counted and has none); and the statistics time zone, `time_zone` (an IANA name). `limits` gives `{min, max}` for each numeric field | JWT |
| PUT | `/api/v1/data-retention` | Replace all settings and apply them immediately: update the ux.db limits and run cleanup and daily aggregation once. The body must include every field in `value`, with no extra fields. Values outside `limits`, a daily retention period shorter than hourly retention, or an unknown time zone return 400 `FIELD_INVALID` (`field`, `detail`) without saving. Data outside shortened retention periods is deleted in background batches. Returns the same response as GET. `PUT /settings/data_retention` returns 400 `SETTING_HAS_OWN_ROUTE`; `GET /settings` excludes this setting | admin |
| GET | `/api/v1/data-retention/status` | Current data status: `stores[]` includes `kind` (`metrics`/`logs`/`requests`/`usage`/`quality`/`gpu`/`gpu_time`/`scheduling`), `file` (the containing file; `main` means the main database), `file_bytes` (file size; `null` for the main database; `quality`, `gpu`, `gpu_time`, and `scheduling` share stats.db and report the same size), `hourly_rows`/`daily_rows` (long-term statistics row counts; `null` for raw data), and `earliest_ms` (earliest queryable time). Also returns `live_since_ms` (when live accumulation began; earlier data comes from the one-time upgrade backfill), `backfill` (`usage_done`/`usage_through_ms`/`quality_done`/`quality_through_ms` for backfill progress), `retain`/`rollup` (`at_ms` and `error` for the latest cleanup and daily aggregation), and `usage_flush` (`at_ms`, `rows`, `ms`, and `error` for the latest token usage write) | JWT |

Statistics queries require JWT authentication. The following time range parameters and `range` response fields apply to `usage`, `quality`, `gpus`, and `gpu-time`. The `key-usage` endpoint reports today and the last 7 days; it does not require `from` or `to` and does not return `range`. Common parameters for the four range-based endpoints are `from` and `to` (required, Unix milliseconds), and `granularity` (`hour` or `day`). When omitted, granularity is hourly for ranges of up to 2 days and daily for longer ranges. Hourly queries support at most 7 days; daily queries support at most 1825 days. Daily ranges expand to whole days in the statistics time zone. Aggregated days use daily data; days not yet aggregated (today, or days that elapsed while the server was stopped) are calculated from hourly data.

Each range-based response includes `range`: `from_ms`, `to_ms`, `granularity`, `time_zone`, `earliest_ms` (the earliest retained bucket at this granularity), `live_since_ms`, and `buckets` (bucket start times, corresponding one-to-one with `series`). Buckets before `live_since_ms` come from the one-time upgrade backfill: sampled rows are weighted, and historical `/v1/messages` and `/v1/responses` requests have no token counts because the gateway did not recognize their usage at that time. Invalid parameters return 400 `ANALYTICS_RANGE_INVALID`, `ANALYTICS_RANGE_TOO_LONG`, or `ANALYTICS_VALUE_INVALID` (`field`, `detail`).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/analytics/usage` | Token usage. Filters: `deployment` (deployment ID), `key` (API key ID; `none` for requests without a key), and `source` (`api`, `playground`, `batch`, the requests a batch sends, or `evaluation`, an evaluation measured from the server). `totals` and `series[]` contain counts: `requests`, `no_usage` (requests without usage), `prompt_tokens` (total input), `completion_tokens`, `reasoning_tokens` (reported by `reasoning_reported` requests), and `cached_tokens` (cache hits reported by `cache_reported` requests; hit rate = `cached_tokens` ÷ `cache_reported_prompt`). `rows[]` has one row per key × user × deployment × source, with `api_key_name`/`api_key_prefix`/`api_key_revoked`/`api_key_deleted` (keys deleted before revocation began retaining records), `user_name`, `deployment_name`/`served_name`/`deployment_deleted`, `source`, and `first_ms`/`last_ms` (first and last buckets used) | JWT |
| GET | `/api/v1/analytics/quality` | Service quality; optional `deployment` filter. `totals`, `series[]`, and `rows[]` (by deployment) include result counts (`completed`, `length`, `client_closed`, `interrupted`, `error`), `ttft_p50_ms`/`ttft_p95_ms` (time to first token for complete streaming responses), `e2e_p50_ms`/`e2e_p95_ms` (end-to-end time for complete responses), and `output_tokens_per_sec`. `classes[]` splits the totals by request priority class (`priority_class` with the same figures, most requests first; an empty `priority_class` holds requests recorded before priority classes, whose class is unknown). Quantiles are reconstructed from quarter-octave buckets, with approximately 10% error; missing values are `null` | JWT |
| GET | `/api/v1/analytics/gpus` | GPU utilization. `group_by` is `model` (default), `node`, or `gpu` and determines table and heatmap rows. `metric` is `util` (default), `power`, `temp`, or `mem` and selects the heatmap value, averaged within each bucket. Filters: repeatable `label=key=value` (same matching as the node list, against current user and system labels; deleted nodes no longer match) and `model` (GPU model). `card_count` and `by_model[]{model, cards}` give the current cards covered by the filters. With at most 256 cards, `detail: true` and `cards[]` has one row per card: `node_name`, `node_deleted` (the node is deleted; `node_name` is the name it had when deleted), `gpu_index`, `gpu_uuid`, `gpu_model`, `present` (still in the inventory, including when the node is offline), `deployments` (currently using it), `util_avg`/`util_max`, `mem_used_avg_bytes`/`mem_used_max_bytes`/`mem_total_bytes`, `power_avg_watts`, and `temp_avg_celsius`/`temp_max_celsius`. Above 256 cards, `detail: false` and `cards` is empty. Cards of nodes deleted within the time range are listed too. When grouped by model or node, `groups[]` has one row per group: `key`, `label`, `node_deleted` (when grouped by node, a deleted node; its `cards` and `nodes` are 0), `cards`, `nodes`, `models[]`, `deployments[]{name, cards}`, `unassigned` (cards not allocated to a deployment), and the same statistics, taken from node/model aggregates. `heat[]` contains `{key, label, node_deleted, values[]}`; when grouping more than 100 nodes, it includes only the 50 highest and 50 lowest nodes for the selected metric, while `heat_total` gives the total row count. Grouping by card with more than 256 cards returns 400 `ANALYTICS_TOO_MANY_CARDS` | JWT |
| GET | `/api/v1/analytics/gpu-time` | GPU-hours; optional `deployment` filter. `rows[]` has one row per deployment × GPU model: `gpu_hours` (dGPU usage weighted by its memory share), `whole_gpu_hours`, `avg_instances` (average replica count up to now), `max_instances`, `prompt_tokens`/`completion_tokens` for the deployment in the same period, and `gpu_hours_per_million_output`/`gpu_hours_per_million_tokens` (`null` without token data). `series[]` gives each deployment's GPU-hours per bucket. `capacity[]` gives each GPU model's `total_gpu_hours` (platform-usable cards on online nodes), `schedulable_gpu_hours`, and `allocated_gpu_hours` | JWT |
| GET | `/api/v1/analytics/key-usage` | Counts for each API key with usage today and in the last 7 days, including today: `{items:[{prefix, today, last_7_days}], time_zone}`. Used by the API Tokens page | JWT |

## AI Helper (AI Assistant)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/ai/configs` | List AI Helper LLM connection configurations. Each row has `proxy_mode` (`management` for the management node outbound proxy, `direct`, or `custom`), `proxy_url` (`custom` only, login masked) and `image_input` (whether the model accepts images), and no key | JWT |
| GET | `/api/v1/ai/presets` | The preset dictionary: `presets[]{name, base_url, model, image_input}` in the console's order, ending with `custom`, and `proxy_modes`, the proxy choices. `image_input` only fills the switch of a new service | JWT |
| POST | `/api/v1/ai/configs` | Add an LLM connection configuration. `base_url` is filled up to `/v1`: trailing slashes and a pasted `/chat/completions` are dropped before storing, and an address that is not http(s) or has no host returns 400 `FIELD_INVALID` with `params.field` `base_url`. Without a `base_url`, a known `preset` supplies one; with neither, the configuration is still stored and the AI Helper reports it as unusable. `proxy_mode` defaults to `management`; `custom` requires a `proxy_url` (http, https, socks5 or socks5h, login allowed, stored encrypted), and a `proxy_url` with any other mode returns 400 `FIELD_INVALID`. Without `image_input`, the preset's value applies when the model also comes from the preset, otherwise `false` | JWT |
| PATCH | `/api/v1/ai/configs/{id}` | Partially update an LLM connection configuration: only the `name`/`preset`/`base_url`/`model`/`is_default`/`proxy_mode`/`proxy_url`/`image_input` in the request change. A missing `api_key` or an empty string keeps the stored key (including a legacy plaintext one); a non-empty one is encrypted and stored. `proxy_url` works the same way: empty keeps the stored proxy while the mode stays `custom`, and switching to another mode drops it. Changing only `preset` (without `model` or `image_input`) also changes the image switch to the preset's. Setting the default clears it on the others. Answers the same row the list shows (no key). 404 for a missing configuration, 400 `FIELD_INVALID` for an invalid value | JWT |
| DELETE | `/api/v1/ai/configs/{id}` | Delete an LLM connection configuration | JWT |
| POST | `/api/v1/ai/configs/test` | Try the unsaved `base_url`/`api_key`/`model` a form holds, storing nothing. A 10 second timeout, through the same outbound policy the assistant uses. Answers `{outcome, status, detail, base_url, latency_ms}`, where `outcome` separates `ok`, `connection` (DNS, dial or timeout), `blocked` (refused by the outbound policy), `auth` (401/403), `path` (a 404 that names no model: the wrong address), `model` (the endpoint does not serve that model) and `http` (any other status, or not a completion). `detail` is the endpoint's own words, shortened and with the key removed. An edit dialog sends its `id` with an empty `api_key` to reuse the stored key, only while `base_url` still matches the stored address. The call goes through the proxy `proxy_mode`/`proxy_url` name; `custom` with an empty `proxy_url` uses the stored proxy. With `check_image: true` and a successful text call, a small image is sent as well and `image` answers `accepted` (the answer names the image's color), `ignored` (answered without seeing the image, as when the upstream drops it silently), `rejected` (refused because of the image: 400/415/422 naming no model) or `unknown` (could not tell), with `image_status` and `image_detail` | JWT |
| GET | `/api/v1/ai/available` | Whether AI Helper is available (whether an LLM is configured): `{available, image_input, images?}`. When the AI service in use has "Accepts image input" on, `image_input` is `true` and `images:{per_message, per_request, max_bytes, max_side}` gives the images allowed per message and per request, the bytes of one image, and the longest side the console shrinks an image to | JWT |
| POST | `/api/v1/ai/ask` | Ask AI Helper to perform a task; supports regular responses and SSE. See [AI Ask Request and Response](#ai-提问的请求与响应) for all fields and conditions | JWT |
| GET | `/api/v1/ai/instances/{uuid}/findings` | Known engine failures of a failed instance: `{items:[{code, evidence:[{line}]}], total}`; `code` is an UPPER_SNAKE `pkg/enginelog` failure code, and the frontend renders the text. An instance with a structured failure reason gets the `causes` its agent reported, without matching logs; only instances from older agents have their logs matched. No LLM required; returns 404 if the instance does not exist | JWT |
| GET | `/api/v1/ai/models/{id}/fit` | Physical fit of the model on each accelerator card type in the online cluster; does not represent currently free resources. Returns `weight_bytes`, `dtype`, and `rows` per card type: `cards/unusable` (usable/faulty card counts), `dtype_supported`, `fast_link_cards` (maximum number of cards in a high-speed interconnect group such as NVLink), `fits`, minimum `tp`, and remaining `headroom_bytes`; no LLM required; returns 404 if the model does not exist. See [Model Fit Results](#模型适配结果) | JWT |

### Model Fit Results

`GET /api/v1/ai/models/{id}/fit` evaluates the physical capabilities of each card type in the online cluster, not currently free resources; no LLM is required, and a nonexistent model returns 404.
Returns the model's `model_id`, `weight_bytes`, `dtype`, `gpu_memory_utilization`, and `rows[]`:

- Card type and capacity: `vendor`, `name`, `memory_bytes`, `nodes`, `cards` (usable card count), `unusable` (faulty card count based on UI health levels), and `max_cards_per_node`.
- Precision and interconnect: `dtype_supported` indicates whether the card supports computing at the model's precision, with optional explanation in `dtype_note`; `fast_link_cards` is the maximum number of cards in a high-speed interconnect group within a single node, where 1 means PCIe only and 0 means unknown. Up to 8 nodes are sampled for each card type.
- Placement estimate: `fits`, `tp`, `per_gpu_bytes`, `usable_bytes`, and `headroom_bytes`.
  `tp` is the smallest power of 2 needed to fit the weights and engine reservation within a single node; if they do not fit, `fits:false` and `tp:0`.

### AI Ask Request and Response

`POST /api/v1/ai/ask` asks AI Helper to perform a task. The `task` values and target fields are:

| `task` | Target Specified in the Request |
|---|---|
| `instance.diagnose` | `instance_uuid` |
| `model.fit` | `model_id` |
| `deployment.unscheduled` | `deployment_id` |
| `node.health` | `node_uuid` |
| `ask` | Requires `question` or `images`; optionally include one of the targets above |

Specify targets by reference; the server assembles the context. `question` is optional except for the `ask` task, where it is required (or images alone are sent). For follow-up questions, include `history:[{role:user|assistant,content,images?}]`, keeping at most the latest 10 rounds.

`images` are the images sent with the question, each a `data:image/(png|jpeg|webp|gif);base64,…` URL; a user turn in `history` may carry its own `images`, an assistant turn may not. They go to the AI service as OpenAI multi-part `content` (text plus `image_url`). The limits are those of `GET /api/v1/ai/available`: 4 per message, 8 per request in total, 2 MiB each once decoded, with content matching the declared type. Images are accepted only when the AI service in use has "Accepts image input" on.

After assembling the context, the server runs an agent loop with read-only tools. The tools read this API's GET endpoints in-process as the user asking the question; see [Use AI Helper](../how-to/use-ai-helper.md). A regular response is `{answer, steps:[{tool,args,ms,error?}], calls, usage:{prompt_tokens,completion_tokens}}`.

With the `Accept: text/event-stream` request header, the response uses SSE streaming:

- `step`: `{id,tool,args,status:running|done|error,ms,error?}`, one event when each tool call starts and one when it ends.
- `text` / `reasoning`: `{delta}`.
- The final event is `done` (`{calls,usage,first_text_ms,total_ms}`) or `error` (`{error, code?}`; `code` is `AI_IMAGE_REJECTED` when the AI service refused an ask because of its images).
- When idle, sends a `: ping` comment every 10s.

Failures before the model call, such as an unconfigured LLM or a nonexistent target, still return HTTP status codes. Error conditions are:

| Condition | Response |
|---|---|
| No LLM configured | 503 `AI_ENDPOINT_NOT_CONFIGURED` |
| Invalid `task` | 400 `AI_TASK_INVALID` |
| Missing target or multiple targets provided | 400 `AI_TARGET_REQUIRED` |
| The same user already has 2 questions being processed | 429 `AI_ASK_BUSY`; `params.limit` gives the limit |
| An image is not usable: wrong format, content not matching its type, too large, too many, or on an assistant turn | 400 `AI_IMAGE_INVALID`; `params.reason` is `format`/`too_large`/`too_many`/`role`, `params.where` is `question`/`history`/`request` |
| The body exceeds every image allowed plus text | 413 |
| Images sent while the AI service in use does not have "Accepts image input" on | 409 `AI_IMAGE_INPUT_OFF`; the AI service is not called |
| The AI service refused the images (400/415/422 naming no model) | 422 `AI_IMAGE_REJECTED` |
| Instance, model, deployment, or node does not exist | 404 |
| LLM endpoint failure | 502 |

### MCP

| Method | Path | Description | Authentication |
|---|---|---|---|
| POST | `/api/v1/mcp` | MCP service (Streamable HTTP, JSON responses only, no SSE; `GET` returns 405). JSON-RPC 2.0, single messages; supports `initialize`, `ping`, `tools/list`, `tools/call`, and notifications (returning 202). Tools are the same read-only tool set as AI Helper, reading this API as the caller with the same redaction and response shaping; tool failures return a result with `isError: true`. Each user can have at most 4 concurrent tool calls. Returns 403 `MCP_DISABLED` when the service is disabled. See [Connect Through MCP](../how-to/use-mcp.md) | JWT or platform token (`read` is sufficient) |
| GET | `/api/v1/mcp/status` | MCP service status: `{enabled, path, protocol_version, tools}` | JWT |
| PUT | `/api/v1/mcp/settings` | Enable or disable the MCP service: `{enabled}`; returns the new status; disabled by default | JWT(admin) |

Deterministic estimator outputs remain fully available without an LLM configured; `ai/ask` is available only after an LLM is configured; MCP does not require an LLM.

## Playground

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/playground/models` | List deployed models callable in Playground, excluding external AI Helper models in global settings; each model has `request_priority` (when true, a request may name any request priority class with `X-GPUWorks-Priority-Class`, the default without one, and the answer names the class it ran at) and `priority_scheduling` (whether the deployment runs priority scheduling; without it the class is recorded but does not change queue order); `html_preview.allow_network` is the HTML preview network policy (from security settings); each model has `sleeping` (boolean), and a sleeping model whose sleep a request ends (the deployment is sleepable and one of its sleeping instances is not held by an operator or a schedule) also has `wakes_on_request: true` (omitted otherwise): a request to such a model is held by the gateway and let through once it wakes, while a request to any other sleeping model answers 503 until it is woken; the Playground shows its notice from this | JWT |
| GET | `/api/v1/playground/openapi.json` | OpenAPI description of the Playground proxy | JWT |
| ANY | `/api/v1/playground/v1/*` | Internal Playground proxy (protected by management JWT, not a `/v1` gateway key) | JWT |

For the boundary between Playground and the `/v1` gateway, and external model naming `external:<config-id>:<model>`, see
[`openai-gateway.md`](openai-gateway.md).

## Batches

A batch runs the requests in one JSONL file asynchronously through the gateway and writes the results to an output file and an error file. It has two groups of endpoints:

- Management endpoints `/api/v1/batches`, used by the console and authenticated like the other routes in this document.
- OpenAI-compatible endpoints `/v1/files` and `/v1/batches`, which require an inference API key by default (a server with no key still refuses them unless an administrator turns on `inference_open_access`) and are usable directly from the official SDKs.

For the procedure, the result file format, and the retry rules, see [Run Batches](../how-to/batch-api.md).

### Console Endpoints `/api/v1/batches`

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/batches` | List batches, newest first, as the paged structure `{items,page,page_size,total}`. `state` is one of the 8 statuses; separate several values with commas; an invalid value returns 400 `LIST_FILTER_INVALID`; `q` is not supported and also returns 400 `LIST_FILTER_INVALID`. Administrators see every user's batches; other users see only their own. List items do not include `input_filename` | JWT |
| GET | `/api/v1/batches/dictionary` | The dictionary for creating a batch: `priority_class` (the request priority class a batch created without one runs at, `batch`), `endpoints`, `completion_window_hours` (`min`/`max`/`default`), `max_file_bytes`, `max_file_lines`, `statuses` (in the order a list filters them), `output_retention_days`, and `models`: the served model names of the deployments not being deleted (stopped ones included) with their state and supported endpoints `{name, state, ready, endpoints}`. `ready` is computed by the server and is true only while serving or when a request can wake the model automatically. The console defaults to these choices; users can explicitly reveal models that require waiting. `state` matches the deployment list and, ordered by how soon the model can serve, is `running` (a replica is awake), `asleep` (asleep, a request wakes it), `starting` (being placed, pulling or starting), `asleep_held` (asleep by hand or by a schedule, a request does not wake it), `offline` (the replica's node is disconnected), `failed` (failed to start) or `stopped` (stopped); the list is sorted in this order and then by name. `endpoints` are the batch endpoints the model's inference API declaration opens (the gateway lets requests through by the same declaration). It is the same judgement as the model check at creation; a name several deployments share takes the state of the best of them and the union of their endpoints. The console's create dialog takes all its choices from here, and a client works none of them out itself | JWT |
| GET | `/api/v1/batches/{id}` | Batch details, including `input_filename`; a missing batch, or one that is not yours (administrators excepted), returns 404 `BATCH_NOT_FOUND` | JWT |
| GET | `/api/v1/batches/{id}/files/{kind}` | Download a batch file; `kind` is `input`, `output`, or `error`; the response is JSON Lines (`application/x-ndjson`, with an attachment filename); an invalid `kind` returns 400 `BATCH_REQUEST_INVALID`; a missing or deleted file returns 404 `BATCH_NOT_FOUND` | JWT |
| POST | `/api/v1/batches` | Create a batch: `multipart/form-data` with the field `file` (JSONL) and optionally `endpoint` (when absent, the `url` all lines share; lines with different `url`s return 400 `BATCH_FILE_INVALID` with `reason` `mixed_endpoints` and `line` the first line that differs from the first), `completion_window` (a whole number of hours from `1h` to `72h`, `24h` by default; an invalid one returns 400 `BATCH_REQUEST_INVALID` with `params.field` `completion_window`) and `model` (when set, the `body.model` of every request is replaced by it; it must be the served model name of a deployment not being deleted, otherwise 400 `BATCH_MODEL_UNKNOWN`; 400 `BATCH_MODEL_ENDPOINT` when its deployment does not support the batch's endpoint) and `priority_class` (the request priority class the batch's requests run at, not case-sensitive, the dictionary's `priority_class` when absent; one that does not exist returns 400 `PRIORITY_CLASS_NOT_FOUND`); the file is validated line by line on upload, and success returns 201 and the batch object, with `warnings` when the chosen model (without `model`, the models of the file's lines) needs someone to act: a sleep by an operator's order (a request does not wake it) is `BATCH_MODEL_SLEEP_HELD` or `BATCH_FILE_MODELS_SLEEP_HELD`, and an offline node, a failed start or a stopped deployment is `BATCH_MODEL_WAITS` or `BATCH_FILE_MODELS_WAIT`. Without `model`, when the file names models that no deployment serves, it returns 400 `BATCH_FILE_MODELS_UNKNOWN` (`params` is `{models, more}`), and when some model does not support the batch's endpoint, 400 `BATCH_FILE_MODELS_ENDPOINT` (`params` is `{models, more, endpoint}`); in both cases no batch is created. The batch belongs to the signed-in user | JWT(admin) |
| POST | `/api/v1/batches/{id}/cancel` | Cancel a batch; returns the batch object with status `cancelling`; only `validating` and `in_progress` batches can be cancelled (`cancellable` of the object is `true`); a batch that has ended, or is `finalizing` (writing its result files), returns 409 `BATCH_NOT_CANCELLABLE` | JWT(admin) |

Reading (GET) works with a read-only platform token; writing needs the administrator role (otherwise 403 `ADMINISTRATOR_REQUIRED`) and a platform token with write scope (otherwise 403 `PLATFORM_TOKEN_READ_ONLY`).

The batch object (RFC3339 times; time fields not yet reached are omitted, as are `output_file_id`, `error_file_id`, and `failure`):

| Field | Meaning |
|---|---|
| `id`, `endpoint`, `status` | Batch ID, endpoint (one of `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`), and status |
| `status` | `validating`, `in_progress`, `finalizing`, `completed`, `failed`, `expired`, `cancelling`, `cancelled` |
| `cancellable` | Whether the cancel endpoint would accept it now: `true` while `validating` or `in_progress`, decided by the server; a client shows the cancel button by it and does not infer it from the status |
| `input_file_id`, `input_filename` | Input file ID; the file name appears only in detail, create, and cancel responses |
| `output_file_id`, `error_file_id` | Output and error file IDs, present once the file exists |
| `completion_window` | The completion window chosen at creation, a whole number of hours from `1h` to `72h`, `24h` by default |
| `model` | The model chosen at creation: every request is sent to it and the line's own `body.model` is replaced; absent when none was chosen and each line uses its own |
| `request_counts` | `{total, completed, failed}`, updated about every 5 seconds; a request being retried is counted in neither |
| `metadata` | An object of string to string, `{}` when there is none |
| `failure` | `{code, message}`, present only in the `failed` status |
| `output_expires_at` | When the output and error files are deleted (the end time of the batch plus the retention, 30 days by default, in days as `output_retention_days` of the dictionary); absent while the batch is running or when it wrote neither file, and a time before now means the files are gone |
| `warnings` | Findings at creation that do not stop the batch; present only in the create response, for example `[{"code":"BATCH_MODEL_SLEEP_HELD","params":{"model":"qwen3-offline"}}]`: the chosen model's deployment sleeps by an operator's or a schedule's order, which a request does not wake; with no model chosen, the models of the file that are like that are `BATCH_FILE_MODELS_SLEEP_HELD` with `params` `{models, more}`. A model whose node is offline, whose start failed or that is stopped gives `BATCH_MODEL_WAITS` (`params` is `{model, state}`) or `BATCH_FILE_MODELS_WAIT` (`{models, more, state}`), where `state` takes the model's state value from the dictionary; the requests are retried until the model serves again or the batch expires |
| `waiting` | Present only on a single batch (`GET`) and only when requests are waiting to be retried: one item per model, `{model, state, status, detail, since, requests}`, where `state` is the model's state right now (same values as the dictionary, omitted when the deployment is gone), `status` and `detail` are the HTTP status and original text of the gateway's latest answer (`status` is 0 when no answer was received), `since` is when these requests began to wait, and `requests` how many are waiting now. It lives only in the executor's memory and reappears with the next retry after a server restart |
| `user_id`, `api_key_id` | The creator; a batch created from the console has no `api_key_id` |
| `created_at`, `expires_at` | Creation time and expiry time (creation time plus the completion window) |
| `in_progress_at`, `finalizing_at`, `completed_at`, `failed_at`, `expired_at`, `cancelling_at`, `cancelled_at` | When each stage was reached |

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","endpoint":"/v1/chat/completions","status":"in_progress","input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","input_filename":"requests.jsonl","completion_window":"24h","request_counts":{"total":1000,"completed":410,"failed":3},"metadata":{},"user_id":3,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z","in_progress_at":"2026-10-28T09:00:02Z"}
```

Product error codes:

| Error code | Status | Meaning |
|---|---|---|
| `BATCH_NOT_FOUND` | 404 | The batch or file does not exist, or does not belong to the caller |
| `BATCH_NOT_CANCELLABLE` | 409 | The batch has ended, or is `finalizing` writing its result files, and no longer accepts cancelling |
| `BATCH_FILE_INVALID` | 400 | The input file is invalid; `params` is `{line, reason, detail}`, `line` 0 means the file as a whole, and `reason` is one of the validation error codes below |
| `BATCH_FILE_TOO_LARGE` | 413 | The file exceeds the limit; `params` is `{limit_mb}` |
| `BATCH_MODEL_UNKNOWN` | 400 | The `model` chosen at creation is served by no deployment, `params` is `{model}`. A deployment being deleted or already deleted does not count |
| `BATCH_MODEL_ENDPOINT` | 400 | The deployment of the `model` chosen at creation does not support the batch's endpoint, `params` is `{model, endpoint}` |
| `BATCH_FILE_MODELS_UNKNOWN` | 400 | With no `model` chosen, some of the models the lines of the input file name are served by no deployment; `params` is `{models, more}`, where `models` is the list of the first 5 names and `more` how many further ones there are (the lookups stop after a few dozen unknown models, so it may be a lower bound), and the client joins the names in the interface language. The matching warning code is `BATCH_FILE_MODELS_SLEEP_HELD`, with `params` of the same shape |
| `BATCH_FILE_MODELS_ENDPOINT` | 400 | With no `model` chosen, the deployments of some of the models the lines of the input file name do not support the batch's endpoint; `params` is `{models, more, endpoint}` |
| `BATCH_REQUEST_INVALID` | 400 | A field of the request is invalid; `params` is `{field, reason}`, for example not multipart, `file` missing, `endpoint` unsupported, or a given `endpoint` different from the `url` in the file |

Files are stored under `batch-files/` in the server data directory.

### OpenAI-Compatible Files and Batches Endpoints

These routes are mounted under `/v1` together with the other gateway endpoints such as `/v1/chat/completions`, and authenticate with an inference API key (`Authorization: Bearer <key>`) by default, including when the server has no inference token at all; only after an administrator turns on `inference_open_access` in the security settings do they accept requests without one (see [Gateway Authentication](openai-gateway.md#authentication)). A failed authentication returns 401 with the error code `invalid_api_key`. Files and batches belong to the user that owns the API key that created them: any key of the same user can access them, and other users get 404.

| Method | Path | Description |
|---|---|---|
| POST | `/v1/files` | Upload an input file, `multipart/form-data` with the fields `file` and `purpose` (only `batch`); success returns 200 and a file object |
| GET | `/v1/files` | List files; `purpose` is `batch` or `batch_output`, `limit` defaults to and is capped at 10000, `after` is the last file ID of the previous page, and only `order=desc` (newest first) is supported |
| GET | `/v1/files/{id}` | The file object |
| GET | `/v1/files/{id}/content` | The file content, `application/jsonl` |
| DELETE | `/v1/files/{id}` | Delete a file; returns `{"id","object":"file","deleted":true}`; while a batch that has not ended still reads it, returns 409 `file_in_use` |
| POST | `/v1/batches` | Create a batch; success returns 200 and the batch object |
| GET | `/v1/batches` | List batches, newest first; `limit` defaults to 20 and is capped at 100, `after` is the last batch ID of the previous page |
| GET | `/v1/batches/{id}` | The batch object |
| POST | `/v1/batches/{id}/cancel` | Cancel a batch; returns the batch object with status `cancelling`; a batch that has ended returns 409 `batch_not_cancellable` |

A list response is `{"object":"list","data":[...],"first_id","last_id","has_more"}`; when `data` is empty, `first_id` and `last_id` are `null`.

The file object:

```json
{"id":"file-3f9c2a7e1b6d4c8a90e5d721","object":"file","bytes":412,"created_at":1790000000,"filename":"requests.jsonl","purpose":"batch","status":"processed"}
```

`id` is `file-` plus 24 hexadecimal digits; `purpose` is `batch` (input) or `batch_output` (output and error files); `created_at` is Unix seconds.

The request body for creating a batch:

| Field | Requirement |
|---|---|
| `input_file_id` | Required. A file with `purpose` `batch` that the caller can see; otherwise 400 `file_not_found` (`param` is `input_file_id`) |
| `endpoint` | Required, one of `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, and equal to the `url` of every line in the file; otherwise 400 `endpoint_mismatch` |
| `completion_window` | Required: `"24h"` (OpenAI's value) or a whole number of hours from `"1h"` to `"72h"` without leading zeros (a gpuworks extension); otherwise 400 `invalid_completion_window` (`param` is `completion_window`). `expires_at` is `created_at` plus this time |
| `metadata` | Optional, an object of string to string with at most 16 keys, keys of 1–64 characters, and values of at most 512 characters; otherwise 400 `invalid_metadata` |
| `model` | A gpuworks extension, optional. A served model name: every request is sent to it and the line's own `body.model` is replaced; omitted, each line uses its own model. It must be the served model name of a deployment not being deleted (stopped ones included), otherwise 400 `model_not_found` (`param` is `model`); 400 `model_endpoint_unsupported` when its deployment does not support the batch's endpoint. A deployment that sleeps by an operator's or a schedule's order (a request does not wake it), whose node is offline, whose start failed or that is stopped is still accepted, and the create response carries `warnings`; the requests are retried until the model serves again or the batch expires. When `model` is omitted, the platform reads the input file once at creation, and if it names a model that no deployment serves (one being deleted or already deleted does not count), it returns 400 `model_not_found` with `param` `input_file_id`; no batch is created and the file stays reusable. The error body is below, naming at most 5 models and ending with `, ...` when more: `{"error":{"message":"The input file names models that no deployment serves: A, B. Set a model to send every request to, or deploy these models first","type":"invalid_request_error","param":"input_file_id","code":"model_not_found"}}`; when some model does not support the batch's endpoint it is likewise 400 with `code` `model_endpoint_unsupported`. With `model` chosen the models in the file are not checked (an unknown `model` is still 400 `model_not_found` with `param` `model`); a model deleted after creation fails per request at run time, while the requests for a model stopped after creation wait for it to start again |

All times in the batch object are Unix seconds, and `null` when not yet reached:

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","object":"batch","endpoint":"/v1/chat/completions","errors":null,"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","completion_window":"24h","status":"in_progress","output_file_id":null,"error_file_id":null,"created_at":1790000010,"in_progress_at":1790000012,"expires_at":1790086410,"finalizing_at":null,"completed_at":null,"failed_at":null,"expired_at":null,"cancelling_at":null,"cancelled_at":null,"request_counts":{"total":1000,"completed":410,"failed":3},"metadata":null}
```

- A batch object created with a `model` has an extra `model` field (absent when none was chosen), and the create response has an extra `warnings` array `[{"code":"model_sleep_held","message":"…","param":"model"}]` (with no `model` chosen and the file's models under a held sleep, `param` is `input_file_id` and the message lists those models; a model whose node is offline, whose start failed or that is stopped is `model_waits`, one entry per state) when needed; other reads do not carry it. Both fields are gpuworks extensions that the official SDKs ignore.
- `id` is `batch_` plus 24 hexadecimal digits; `metadata` is `null` when there is none; `errors` is normally `null` and, in the `failed` status, is `{"object":"list","data":[{"code","message","param":null,"line":null}]}`.
- `status` values: `validating` (waiting for an execution slot; at most 32 batches run at once across the server), `in_progress`, `finalizing`, `completed`, `failed` (only because of a platform fault, such as the input file disappearing), `expired` (the completion window passed; requests without a result go to the error file with the code `batch_expired`), `cancelling`, `cancelled`.
- Cancellation: no new requests are sent, in-flight requests get up to 2 more minutes, and then the batch ends as `cancelled`, with the results so far kept in the output and error files.

**Input file validation**: the file is validated while it is received and the first error rejects it. The error carries `line`, its `message` starts with `Line N: `, and a rejected upload leaves no file behind.

| Item | Limit |
|---|---|
| File size | 200 MB; larger returns 413 `file_too_large` |
| Number of requests | At most 50000 non-blank lines; more returns 400 `too_many_requests` |
| Line length | 16 MiB; longer returns 400 `line_too_long` |
| Empty file | 400 `empty_file` |

Each non-blank line must be a JSON object. The validation error codes are:

| Error code | Meaning |
|---|---|
| `invalid_json_line` | The line is not a JSON object |
| `missing_custom_id` | `custom_id` is missing or not a non-empty string |
| `duplicate_custom_id` | `custom_id` is repeated in the file |
| `invalid_method` | `method` is not `POST` |
| `invalid_url` | `url` is not one of the three supported endpoints |
| `missing_body` | `body` is missing or not an object |
| `missing_model` | `body.model` is missing or not a non-empty string |
| `streaming_not_supported` | `body.stream` is `true` |

Errors of the upload request itself, all 400: `invalid_content_type`, `invalid_multipart`, `invalid_purpose` (`purpose` is not `batch`), and `missing_required_parameter` (`file` or `purpose` missing).

**Result files**: after a batch ends, `output_file_id` points to the output file of successful requests and `error_file_id` to the error file of requests that finally failed. Both are JSON Lines with `purpose` `batch_output`, one line per `custom_id`, in completion order. A line of the output file is `{"id":"batch_req_<hex>","custom_id","response":{"status_code":200,"request_id","body":{...}},"error":null}`, and a line of the error file is `{"id","custom_id","response":null|{"status_code","request_id","body"},"error":{"code","message"}}`. The output file exists only if at least one request succeeded, and the error file only if at least one request failed. Both are deleted 30 days after the batch ends and reading them afterwards returns 404 `file_not_found`; the batch keeps its counts. Input files are kept until the user deletes them.

**Which requests are retried**: when the answer is HTTP 200 but some choice has `finish_reason` `abort`, or is 503, 429, 408, any other 5xx, or a transport error, the request is retried with exponential backoff (starting at 2 seconds, doubling, capped at 60 seconds, honoring `Retry-After`, with jitter) until the batch expires and does not count as failed; any other 4xx is a final failure.

**Error format**: these routes return errors in OpenAI's format, and file validation errors carry an extra `line`:

```json
{"error":{"message":"Line 2: custom_id must be a non-empty string","type":"invalid_request_error","param":null,"code":"missing_custom_id","line":2}}
```

| Status | `code` | Scenario |
|---|---|---|
| 400 | `invalid_json`, `invalid_limit`, `invalid_order`, `invalid_endpoint`, `invalid_completion_window`, `invalid_metadata`, `endpoint_mismatch`, `file_not_found`, `invalid_purpose`, `model_not_found`, `model_endpoint_unsupported` | Invalid batch creation or list parameters; the input file validation codes are listed above |
| 401 | `invalid_api_key` | The API key is missing or invalid; `/v1` requires one by default, and a request without a key is accepted only when an administrator turned on `inference_open_access` |
| 404 | `file_not_found`, `batch_not_found`, `not_found` | The object does not exist or does not belong to the caller; unknown path |
| 405 | `method_not_allowed` | The path does not support the method |
| 409 | `file_in_use`, `batch_not_cancellable` | Deleting a file still read by a batch that has not ended; cancelling a batch that has ended |
| 413 | `file_too_large` | The upload exceeds 200 MB |

**Execution and limits**: a batch's requests take the same gateway path as a client's request (routing by `body.model`, on-demand wake of sleeping deployments, usage counting, and request records attributed to the API key and user that created the batch). Each batch runs 8 requests at a time and all batches together have at most 64 requests in flight; progress is saved about every 5 seconds, and after a server restart unfinished batches resume from the latest checkpoint without sending again the requests that already have a result.

## Out of Scope

- For data plane `/v1/*` endpoints (OpenAI-compatible chat/embeddings/rerank/audio/image/video/realtime endpoints, etc.), see
  [`openai-gateway.md`](openai-gateway.md); they are not repeated here, except `/v1/files` and `/v1/batches`, which are in [Batches](#batches批处理任务).
- Agent-side yamux subprotocols (`ctrl`/`telemetry`/`logtail`/`tunnel`/`model.read`/`model.write`/
  `image.read`/`image.write`/`terminal` and others; `proto.LaneFor` has the full list and their lanes) are not HTTP routes. They belong to the `internal/proto` control plane protocol and are outside this document's scope.

## Route and Contract Maintenance

The following conventions maintain the endpoint inventory; they are not prerequisites for calling the API:

- Management routes are registered by `internal/server/api.Router` using the chi router.
  `TestRouterInventoryMatchesCompleteContract` in `internal/server/api/router_inventory_test.go` asserts each entry in the actual route tree;
  update this test whenever you add or remove a route.
- Management authentication is checked by `managementAuth` in `internal/server/api/auth.go`; administrator operations also pass through `managementAdmin`.
  Product errors use `messages.ProductError`; 130+ stable error codes are defined in `internal/server/api/errors.go` and are not listed individually here.
- `make check-api-conventions` checks field naming and list structures at the type level; see [API Conventions](api-conventions.md).
- During production assembly, `internal/server/app` always injects non-nil
  `ProxyV1`, `ProxyModels`, and `PlaygroundV1` handlers for `/v1/*`, `/v1/models`, and `/api/v1/playground/v1/*`. Route inventory tests use zero-value `Deps{}`, so these three gateway routes
  and the SPA fallback `/*` are excluded from their assertions, but are always mounted in production.

### Console manifest and extension routes

A separately built server can mount its own management routes under `/api/v1/x/<name>/` (`api.Deps.Extensions`). The community server mounts no extension routes, and its route inventory is exactly the same as with no extensions.

- **Namespace and collisions**: an extension name must match `[a-z][a-z0-9_]{1,31}`; a route (relative to `/api/v1`) must be `/x/<name>` or start with `/x/<name>/`. The core registers no route under `/api/v1/x/`, so an extension name may equal a core path segment (such as `nodes`), and `/api/v1/nodes` stays the core route. `Router` panics at startup on a route outside its namespace or a method and path registered twice.
- **Authentication**: extension routes use the same management authentication as core routes (including the session's origin check and renewal). Each declares one of three levels: `read` (any signed-in user and platform token; a read token only for GET/HEAD/OPTIONS), `admin` (also requires an administrator, otherwise `403 ADMINISTRATOR_REQUIRED`), or `admin-session` (an administrator with a login session only; a platform token gets `403 PLATFORM_TOKEN_NOT_ALLOWED`).
- **Feature availability**: every extension route either belongs to one feature or is explicitly marked as a community route, never both. A feature's availability is `full`, `read_only`, `locked`, or `hidden`, enforced on the server: under `read_only` only reads pass (plus routes declared writable while read-only) and writes get `403 FEATURE_READ_ONLY`; under `locked` the route returns `403 FEATURE_UNAVAILABLE`; under `hidden` it returns `404 FEATURE_UNAVAILABLE`. When nothing answers for availability, the feature counts as unavailable and returns `FEATURE_PROVIDER_ABSENT`. The error's `params.feature` is the feature name.
- **Availability of the whole server**: a separately built server can set the whole server to `read_only` or `locked`. While `read_only`, reads, `/v1/*` and `/api/v1/playground/v1/*` work as usual and changes get `403 PLATFORM_READ_ONLY`; while `locked`, reads work as usual, as do signing in, health checks, the console manifest, the connection of machines that already joined, plugin package management, the server's upgrade, diagnostics bundles and the routes of the extension that set the limit, and changes and `/v1/*` get `403 PLATFORM_LOCKED`. A request under `/api/v1` is authenticated before it is refused, so one that is not signed in still gets 401; a refusal on `/v1/*` has the gateway's error shape with the same code. Installing or upgrading a plugin package is refused like a change. While limited, `GET /api/v1/console/manifest` carries `platform` (`access`, `message`, and `link` for an administrator only); the community server is never limited and its manifest has no such field.
- **AI assistant and MCP**: extension GET routes are closed to the AI assistant and MCP by default; only explicitly marked routes appear in the readable route list and can be read.
- **Route inventory**: `api.RouteInventory` lists one route per line; an extension route adds `[access=…] [action=…] [feature=…]` (`[community]` for a community route), and an extension route that neither belongs to a feature nor is marked community fails the inventory.

Fields of `GET /api/v1/console/manifest`:

| Field | Description |
|---|---|
| `edition` | Build name, for display only |
| `features[]` | Current availability of each declared feature (those compiled modules declare together with those the plugin packages installed now declare, changing as soon as a plugin is installed or uninstalled), sorted by `key`: `{key, access, presentation, link?, message?, limits}`. `access` is `full` / `read_only` / `locked` / `hidden`; `presentation` is `promote` (shown as locked with `link` when unavailable) or `hidden` (hidden entirely when unavailable, so locked is returned as `hidden`); `message` is a product message `{code, params?, message}` explaining the current availability; `limits` is `[{dim, max}]`, where a `null` `max` means unlimited |
| `nav[]` | Menu entries extensions add to the console `{id, group, label_key, icon?, route, feature?}`; entries whose feature is `hidden` are not returned |
| `banners[]` | Notices shown at the top of every page `{id, severity, message, link?}`; `severity` is `info` / `warning` / `danger` (any other value is returned as `info`), and `message` is a product message `{code, params?, message}` |

### Whether GPU capacity is known

Node GPU and `/api/v1/gpus` physical-card DTOs both return the boolean `memory_capacity_known`. It is derived from inventory capacity and not persisted. When false, `memory_total_bytes` must not render as a known zero-GB capacity. Measured zero usage remains a valid metric. Only the compatibility boundary for old APIs without the field uses the legacy zero-capacity sentinel.

An unknown `target_vendor` on `/api/v1/engines` returns 400. If custom-command features cannot be determined and the target declares launch-feature restrictions, evaluation/creation returns `LAUNCH_FEATURES_UNKNOWN` with parameter `vendor`; the scheduler uses `launch_features_unknown`. Re-evaluate with a recognized engine entrypoint and structured parameters.

<!-- translation-source-sha256: 6afe2639959adce6d0cd8a2ac72970e0be68444f550c7aed620d08806b5021cd # Complete REST API Routes -->
<!-- translation-source-sha256: e051ec0ecf8fc5027d13fd4f167a593d9a49317c02dccee13f4f188d483ff72e ## Authentication Model -->
<!-- translation-source-sha256: 27715e542090e35f7a58c1ac958b5f745de687a01d3406ed5c1d61e4b48f8e3d ### Distinguish Credential Purposes First -->
<!-- translation-source-sha256: 7eb19ad3323f85d0523f1507c21a8ca570766dab14058e8641d141e78dec6779 ### The Authentication Column in Route Tables -->
<!-- translation-source-sha256: 923ecd23932b58ea7adf21ee0a734d50b5bed8433191059e5840e1f611a737d6 ### Additional Restrictions on Platform API Tokens -->
<!-- translation-source-sha256: 4de591bd514d5217e84278a38396f09c4b265fa40e4dafc8ee48c380d4294284 ### Public Installation and Health Check Endpoints -->
<!-- translation-source-sha256: 941e8b0121ee9545d578500b430d51dede7c87a5c885722a1d588102d1d5a8bb ## Error and Response Conventions -->
<!-- translation-source-sha256: b800a03ab8d388ff374983e970c96499cbd1d6ba26bfd5a3a1f34890aed3d738 ### JSON Fields and List Structure -->
<!-- translation-source-sha256: a5858d91e1c4eba669657ee25d54358c070f8aff5f6e7048230e5a553564cfca ### Reading Error Responses -->
<!-- translation-source-sha256: bc212faa17263c00dd1605040cb5996ea0de5e568f58382b760188718e50c374 ### Nodes and Deployments: Pages, Filters, and Sorting -->
<!-- translation-source-sha256: 07bbb1333fa351e29a815227a6849f59fad7ef2bedb4deeba1dfd62b5a75ac5d ### Real-Time Event Streams and Response Compression -->
<!-- translation-source-sha256: e84017100ea5bd395dd6f4ddc000ef7b85d4ce62c29e008d613c0bf11ff9495b ### How Setting Changes Reach Nodes -->
<!-- translation-source-sha256: dd05cb84569536c26f5469fce1e4dbc875e0c445cd676077ae3a32fc2320ee29 ## Auth -->
<!-- translation-source-sha256: 16f64df1cf4b0d600ddac8d1033036cc031016cfca0375951216effed4a9a00e ### Login Request and Response -->
<!-- translation-source-sha256: 0c5e5b2fbfc149bc9864e31a84b6c0bcf78d7b74a8bfeb0e3924ff0b27a78a43 ### Change Password -->
<!-- translation-source-sha256: 78c7acae52d8a56aa6a026d187b7898535ed349c3236bcfaa5ee2d989a580c46 ### When Sessions Expire -->
<!-- translation-source-sha256: 60f52e5578eaeb214401ce94f2639e999f5f048d60dfef2ae192cdc06720c4af ### Login Endpoint Errors and Origin Checks -->
<!-- translation-source-sha256: 0db1295a2cd47ce30ef1d6c8d72fc7decd4e634b43cc01b7b3e70e55a59759fb ## Nodes (Node Connection) -->
<!-- translation-source-sha256: 35cc145ca4fd18815571610c10cab2c36e52df6b8f6dbc6ba25f1805d78e47b4 ### Bulk Agent Upgrades -->
<!-- translation-source-sha256: 98c4940bec9d9b4daed0cf57ea7d5af70d4156588fdda2c82782b5fcf39f43ce ### Node List Filters and Response Fields -->
<!-- translation-source-sha256: 0a68e7fac638afa42ac575a6cebd0ec317977c52eaca8dcbebf9022482a36c64 ### Dashboard Summary Response Fields -->
<!-- translation-source-sha256: 91c4e30202699e8b96b74000ae5a84ba54ccfead4c98283a0e6f5cfa7cc011b1 ### Node Connection Diagnostics -->
<!-- translation-source-sha256: 1e218fa4ed22e19d4acdccb182ab42acaa348646fd2a0b9c73b5c86fd49583ae ### Connection Parameters for Node Creation -->
<!-- translation-source-sha256: fce169a4cb7bee2f56727bb980cfdae24b94c38dfa145e356f59cd220e7c594a ### SSH Checks and Installation -->
<!-- translation-source-sha256: 4b4f4a4b25a2e98642ab57b1602561eb8a651333475aad10ef69e3ce4ecc9969 ### Node Scheduling and Accelerator Switching -->
<!-- translation-source-sha256: b4e11dbe33d4099c767c7382b562dcb10ba534decc2f13323aff9b76eaaa6000 ### Node Labels: Written and Generated -->
<!-- translation-source-sha256: eb70c381f0fe7b561688238a4a64e06ef8207d19abd2527daf8b05f526511238 ## Kubernetes (Cluster Connection) -->
<!-- translation-source-sha256: 8ab144ebfbe02539abc2194f2d702b2abac1a76b8a0a10c4a97990a187475ac2 ## Slurm (Cluster Connection) -->
<!-- translation-source-sha256: eb0d2ff341e8c1ae82bcc18e67d0720a647009973d4a5c9b9394f4b3c57c737d ## Models -->
<!-- translation-source-sha256: 2818637fc41db824cf8851d4bd0bc2cb1e90e978da76ce1c2cb5098d8aeece25 ### List Filtering and Presence Summary -->
<!-- translation-source-sha256: 536c79e56bf4d787dec94eef14773f9e978accf7d42427e195db79e5fa0ac045 ### Register an Existing Node Path -->
<!-- translation-source-sha256: a8ebc8f569b4695db5d7e80450a6e843689abd8b2724c44a3c54d53df4c178bc ## Images / Storages (Image Catalog and Storage Policies) -->
<!-- translation-source-sha256: 5a937a0329b1aabd619bac36afbe45ff963584a7031b8588b3c6d1c9ebabdbe0 ### Register an image already on a node -->
<!-- translation-source-sha256: 04f403145369bea2e0886af6fd3a84add59d07299152c9a53cc09ef3d598fdf0 ### Storage Configuration and Cache Status -->
<!-- translation-source-sha256: 76495aa0a302fbe279f354b4207253de032639c11e8498c2041f1b8a373a248c ### Storage File Operation Scope and Errors -->
<!-- translation-source-sha256: 28ab68c5e446f16a186a6ea1ad5e2fe940b67e51a2098b9d556108ede3a22d09 ### Catalog Import Results -->
<!-- translation-source-sha256: c162aaa85f62dfb49b9c18ed8540144b49d74ede563ca77a776b437d0b8a1125 ## Inference Templates (Inference Catalog/Templates) -->
<!-- translation-source-sha256: 4b7e28c06adfbce76189556587fc94653baf4749acae30f34cec3772bf431f37 ### Pre-Import Checks -->
<!-- translation-source-sha256: f3c254df3774c7c48ee5ebf7cc4861e55fd88aed33f8cc457ecb4190f5c41377 ## Deployments / Instances (Deployments and Instances) -->
<!-- translation-source-sha256: 853ce95b08ddeb141bb84fdd808c7961cb81e7ed7b2e3b66ca8f26a3729eb149 ### Save as an Inference Template -->
<!-- translation-source-sha256: 3338bc9ecd8619b8b4794888aa8b6581e080e20b0aa4ed8e06bfddf20796d893 ### Deployment List Filters and Response Fields -->
<!-- translation-source-sha256: d98ba052a1e0fbbb930db8d81eeba6a64d25345932a8f6be879383c529b268c9 ### Query Deployment Details -->
<!-- translation-source-sha256: e3ce46f0914b9b3c2d279244d804da397e2cb12f7ac1d7b06ef98ff210c47f52 ### Deployment Sleep State (`state`, `asleep_replicas`, `sleep_level`) -->
<!-- translation-source-sha256: df36dcd2a36134ffcf559784263272ca22672ba639f387be636a64fd8aaff8bf ### Query the Candidate Pool -->
<!-- translation-source-sha256: 1319b5d1601a10582fc3abacd3801c3c7bbb51d610511c40bbef7778e24a8d98 ### Edit a Deployment -->
<!-- translation-source-sha256: 87fa2929e5a7c7cf0fddcafe8c7b6e1f686f8af22a1f28b88c1241cf9781386f ### Modify Node Requirements -->
<!-- translation-source-sha256: 4d84620768f7334d6cfd9a5bdc9ed1360dff841208da5342b4162bdd5424339e ### Adjust the GPU Memory Limit -->
<!-- translation-source-sha256: 7b3f7296ca76c39ca77a286b7fcaa02b91d889acf33dd181f7a0d3b6d664487b ### Suggest Nodes for a Multi-Node Replica -->
<!-- translation-source-sha256: 96aee6f77e4fb26d3190164eb2988180be2d354154ca6bd5931d5600c2f817b7 ### Restart Strategies -->
<!-- translation-source-sha256: 3af911c6258a244e7c506c21b0d5a3105fa0351aa312ad925f69aacc34147cc7 ### Waiting Wakes and Yielding Wakes -->
<!-- translation-source-sha256: 0e907540cc6d8cde88f1086843caf8fbdfff962fe0c7a9b5464eaec83cf1aa9f ### Yield Policy -->
<!-- translation-source-sha256: c4ccfcdf9aba569f30fef80195fedf6bfea949ac0e85c172a65179834a222914 ### Schedules -->
<!-- translation-source-sha256: c4a7172711a0b5be1e0554d9f7cebcdbf63b233978824fec033008301170d4f9 ### Scheduling Board -->
<!-- translation-source-sha256: f41bc12a0e1a8256a187bf5ea66d0701060de0861a75853414ca52bd6a0ea72b ### Scheduling Overview -->
<!-- translation-source-sha256: dec82318d3cf41109b6e82f1baf610b359c234220e96bc4180de314cd5c464b2 ### Residency Timeline -->
<!-- translation-source-sha256: 04729495d747796f472236e8964dd72e448a89c00a2e32c948412b87b4516e63 ### Scheduling Activity -->
<!-- translation-source-sha256: 8904b35f67e5b13cc1a29b2bafeb6871cd88e1c7b72122e119b43ce71671d46a ### 调度记录 -->
<!-- translation-source-sha256: 1ae2b3bd9ab2cf28d28865c3107f97bd5e05203f266761927f774ce49d3960a1 ### Adjust Replicas for Regular Deployments -->
<!-- translation-source-sha256: 78d17e39c2b506870d03fceafef3b739ee794a40aba7b1d2aafb3fbf73d6f0dd ### Configuration Fields for Evaluation and Creation -->
<!-- translation-source-sha256: 6ddc68a02f5cb22b8e620e368c46524ef8c9626138d69e78a25e2a7a68fab526 ### Health Checks -->
<!-- translation-source-sha256: e7d8e9be1155b1cfb96a1b2f3ce361296581c5a25ffcd642194d34d4e6790bf0 ### Automatically Placed Multi-Node and PD Deployments -->
<!-- translation-source-sha256: 42d2614a5ddf9143c7f9872842403db29553e091bd5b4279675696e2e1b99419 ### Evaluation Requests and Acquisition Estimates -->
<!-- translation-source-sha256: 7dc71e6b90d4dbf5934073cc9e8db4e6604b8b5068d599dc3056ebb243c4e1c9 ### Evaluation Field Errors (`errors[]`) and `ok=false` Semantics -->
<!-- translation-source-sha256: 88cb849dfd035df08ee25bd8d0c73481757feb4d58dfb56df3c289608515e1b7 ### PD Disaggregation Deployments (`placement.pd`) -->
<!-- translation-source-sha256: 12626c939c377f3d9b6d4039f0c8e5e179d8e8daecfc32db0218d7b5dcb4a510 ## Benchmarks -->
<!-- translation-source-sha256: ecb6a1c6841684be7b5314d1fce4a0d700ecba4806a10f86a4bdc041fac42253 ## Profiles / GPUs (Estimation Calibration and GPU Inventory) -->
<!-- translation-source-sha256: 8b6ffd2276631c3b7fd5394a404f9e78f11a9def77a00e19cf643435700e77c5 ### GPU Filters and Allocation States -->
<!-- translation-source-sha256: b4dc52b015553f925e0f7f1675b1373ebdad131f022a17361e56f2bc72978e9b ## API Keys (Gateway Keys) -->
<!-- translation-source-sha256: 76e34784e2bc78ac53f7cfec6138ae0da78f55035ac27592c8a6ab471b8e30e7 ## Priority Classes (Request Priority) -->
<!-- translation-source-sha256: df002fa7f5282fe8c271ae6d7bcdf6698cf6b9d9cb5844c03c0807d98649ef7f ## Platform Tokens (Platform API Tokens) -->
<!-- translation-source-sha256: b5db8cf5bc957f862713388e0b3b5a447a3a6ef992dfd4526f4ac73e2176f851 ## Plugins -->
<!-- translation-source-sha256: aaef73cdcb77d09afd56f6b584a2602e367ab17483bffc0fbf3112f855893b33 ## Providers (Cloud Instance Rental) -->
<!-- translation-source-sha256: 943c135b4dd17cb7a5163ff1ef96f31bfe4cc8daf3b1332d2dae254d1ff3a973 ### Provider Connection Test -->
<!-- translation-source-sha256: 4a41e54a2a7691fc464f2c3bff6c62ff401cee25b2e8ede1d1511164ab86791e ### Cloud Instance Installation and Restart Confirmation -->
<!-- translation-source-sha256: dff84f5e2e143ca7701c66cd40683e1ca6428f5d803325e20c3a4e6b759a19ae ## Web Terminal -->
<!-- translation-source-sha256: 7858bdfd14670c6316f95db08d7f9198accf3451a7e655c9513ff13cf428941c ## Networks -->
<!-- translation-source-sha256: 996230365d7f304ba6d5b55ec19068b5aae5e436538dd5f988dba762660e75c0 ## Settings / Network Presets -->
<!-- translation-source-sha256: 0d6096536d010c9fb3e99a7c9823f000f224ee23f15072817338937928739331 ### Update Check Results -->
<!-- translation-source-sha256: 0e96eaf91ec4ceaa489e42f4bc76c90f1ae38793c3dc6f511af1e5a915929772 ### Server Upgrade Restrictions -->
<!-- translation-source-sha256: 407e22173bef80c6affe537b15efce4491e1e75938bc45fc92a2c73d4679d528 ### Trace Export Settings and Status -->
<!-- translation-source-sha256: 63ec2229bea726b68dd009f914f6f8e306163df675d1f05e3c10b6615073a778 ### Source Rule Connectivity Test -->
<!-- translation-source-sha256: 56f04dff8dd311893b721ecd43218c92e9c3c7678bd8f1200d8507f7a051c5ec ### Removed Settings Endpoints -->
<!-- translation-source-sha256: d8f9488df6a35ad975dcfc452f0f163de4cc794f6dc8a9c618024a5cb892edcc ## Metrics / Logs -->
<!-- translation-source-sha256: 158a9ddb434b6aa850af1555591056753a2f3638de60fcb17287a63972096f40 ### Logs, Metrics, and Event Timelines -->
<!-- translation-source-sha256: 6619b8e2688e4b0d50d64dfbbb54b3ebf0dc95800adc2ed316f59f1d5146e0ad ### Service Monitoring Response -->
<!-- translation-source-sha256: d53f3fa5bcd7abaf2ac6df9efa27cf061e9e5d9f49abf349ed222cd7baa035fa ### Find Requests Across Deployments -->
<!-- translation-source-sha256: 909217ceee4a6d4598149501677e551fbd3af0eb6decb1dec4de54d97aabda4e ### Per-Request Queries -->
<!-- translation-source-sha256: 9f324281249cb8d28d339dc165940ef0bcf0f1c535ce67068975be3f128d50b4 ### Latency Aggregation Rules -->
<!-- translation-source-sha256: e6e2d82b3e4cb53280f5f986eac09c5331e7c75c92855104a7dfa83f410ad5d9 ## Analytics (Usage Analysis) -->
<!-- translation-source-sha256: c338ca0460689aa4d9d3859cf4365d9406db09e55bc28d7972d39ea9c7aa3790 ## AI Helper (AI Assistant) -->
<!-- translation-source-sha256: cc96478af8cba8876b10d8d992d3b2a84827297cbc405f43f3c1a2305033c425 ### Model Fit Results -->
<!-- translation-source-sha256: fdb516621c011d9d860afe6dc48066899257527b959718b65b620955afe8632c ### AI Ask Request and Response -->
<!-- translation-source-sha256: 31955e4d7f2f472ef37b1ed0e8f2c2856a88ec37f7689e2b718ad566ec8beba5 ### MCP -->
<!-- translation-source-sha256: 8c9f7cff863992af234e27888ecb43b46f4a1937ccfc8bedc8d44ddca11d8aad ## Playground -->
<!-- translation-source-sha256: 792b7c7e84d325c8be066e460eeb7dc836b7fcc46a1a22fdc8c308f227b40b16 ## Batches -->
<!-- translation-source-sha256: fab893a3efb3fa86d45b0a6e875f1cebad71738dc6424310e5af4b801fa794f4 ### Console Endpoints `/api/v1/batches` -->
<!-- translation-source-sha256: d90c2a891f76775aec3f7b48248481bd52e0751724b4a75648d8f62318cd8799 ### OpenAI-Compatible Files and Batches Endpoints -->
<!-- translation-source-sha256: a896a5dab1c99b94dd8ffc62b82650f6309812d3ef0bf90b28daeb38d7cc2c25 ## Out of Scope -->
<!-- translation-source-sha256: a065b875b11936222db3d33c7671feb5120f5ed464965d91d9278a81bb51ecb0 ## Route and Contract Maintenance -->
<!-- translation-source-sha256: 1f082a1dc64217e23acc8eb01bf5b2781011ef80a37db2325a8ca6b8f758ede1 ### Console manifest and extension routes -->
<!-- translation-source-sha256: 3b39ae121cdbd7a4964c45d1deacffa4a4772f5ddd32c4530ca202ffcd247aef ### Whether GPU capacity is known -->
