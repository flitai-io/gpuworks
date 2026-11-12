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

`auth/me` returns current user information and the length a new password must have, in the form `{"id":1,"username":"admin","role":"admin","password_policy":{"min_length":8,"max_length":128}}`.
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
| The new password is the username | `PASSWORD_USERNAME` | Compared ignoring case, punctuation and leading or trailing digits |
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
| GET | `/api/v1/peer-transfers` | Peer transfer observations for this management-server process. Optional single `transfer_id`; returns effective relay policy, active relay count, bandwidth waiting count, cumulative results, and bounded runtime observations. Missing observations return an empty list; unavailable capability returns 503, invalid filters 400. Counters measure inner TLS stream bytes, not verified file progress. | JWT |
| GET | `/healthz` | Health check (no login required, for liveness/load balancing) | None |
| GET | `/api/v1/engines?target_vendor=amd` | Engine parameter dictionary for a target vendor (uses the general profile when `target_vendor` is omitted); the AMD profile describes implementation admission scope, not validation on a specific GPU/ROCm version | JWT |
| GET | `/api/v1/nodes` | List nodes; paginate with `page`/`page_size` and filter by `q`/`state`/`label` (see “Error and Response Conventions”); nodes connected from a Kubernetes cluster carry the system labels `gpuworks.ai/kubernetes-cluster` and `gpuworks.ai/kubernetes-node` (see “Node Labels: Written and Generated”), and `cordon_reason` is `kube_unschedulable` while Kubernetes has them cordoned | JWT |
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
| POST | `/api/v1/nodes/{uuid}/doctor` | Trigger the node self-check RPC | JWT |
| POST | `/api/v1/nodes/{uuid}/connection-diagnostics` | Read Agent runtime observations over existing ctrl without SSH; also saved URL, optional SSH and public-entry history. Empty body or `{}`, no recovery/address changes; see [Node Connection Diagnostics](#节点连接诊断) | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/upgrade` | Trigger a remote agent upgrade; a Kubernetes node upgrades in place like any node (its agent runs from the node's state directory and declares `upgrade.kube_binary`); one that could not start from the state directory (noexec, say) does not declare it, and the upgrade marks it `kubernetes` and skips it | JWT |
| PUT | `/api/v1/nodes/{uuid}/scheduling` | Set `scheduling` to `cordoned`, `draining`, or `schedulable` and return the node; invalid values return 400 `FIELD_INVALID`. See [Node Scheduling and Accelerator Switching](#节点调度与加速卡切换) for the effects of each state | JWT |
| PUT | `/api/v1/nodes/{uuid}/gpus/scheduling` | Set whether one card takes new deployments: `{"pcie_bdf":"0000:3f:00.0","scheduling":"stopped"}` (a card without a PCI address is named by `uuid`). `stopped` holds until lifted and the platform never lifts it; `schedulable` lifts an administrator's stop, or, when the platform judged the card unusable (a fault, or processes outside the platform), allows it until the first report at least a minute later judges it again. Returns the node (with its cards); a card's `scheduling_stopped` and `scheduling_allowed` give its state and `scheduling_actions` what may be done now. Invalid values or no card named: 400 `FIELD_INVALID`; unknown node or card: 404; making a passed-through or driverless card schedulable: 409 `CARD_NOT_OWNED` | JWT |
| GET | `/api/v1/nodes/{uuid}/health-checks` | The current state of each check on each of the node's cards, as a table (read from server memory, never reaching the agent): `checks` are the columns (only checks that apply to some card, with their period in seconds), `cards[].cells[]` the cells (`state` is `pass`/`warn`/`fail`/`skip`/`pending`/`na`, `reason` a finding code or skip reason, `checked_at`, `duration_ms`, `stale`), and `cards[].worst`, `cards[].can_recheck` and `summary` (cells per state) are computed by the server. `?pcie_bdf=` (or `uuid=`) narrows it to one card. Before the node has reported, `cards` is empty and `reports` false; offline, the last known states are given and marked `stale` | JWT |
| GET | `/api/v1/nodes/{uuid}/health-checks/detail?pcie_bdf=&check=` | The last run of one check on one card (`last`; `last_ran` too when `last` was skipped; `last_problem` when an earlier run found a problem; each run has `state`, `reason`, `at`, `duration_ms`, a `probe` command to recheck it, and `evidence`), fetched from the agent over RPC; the agent answers from memory and runs nothing. Concurrent requests for the same cell make one call and its answer is reused for 5 seconds; at most 2 requests per node and 16 in all are in flight, beyond which it answers 429 (with `Retry-After`). Missing card or unknown check: 400 `FIELD_INVALID`; node offline: 409 `NODE_OFFLINE`; agent too old: 409 `CARD_CHECKS_UNSUPPORTED` (neither calls the agent) | JWT |
| GET | `/api/v1/nodes/{uuid}/health-records` | The node's failure records (a stretch in which a check warned or failed): ongoing ones first, then the newest; filtered by `?pcie_bdf=` (or `uuid=`) and `?check=`, paged with `page`/`page_size` (50 per page by default). Each has `card_ident`, the `card_index`/`card_name`/`pcie_bdf` of a card still on the node (`removed` is true for one that is gone), `check`, `level` (`warn`/`fail`), `reason`, `started_at`, `ended_at` (absent while ongoing), `last_seen_at`, `duration_seconds`, `count`, `flapping`, `ongoing`, `first_evidence` and `latest_evidence`; `retention_days` is how long records are kept (30) | JWT |
| POST | `/api/v1/nodes/{uuid}/gpus/recheck` | Run the runtime check on one card now: `{"pcie_bdf":"0000:3f:00.0"}` (or `uuid`). The answer is recorded on the card like the periodic check's, so its state follows at the next report; returns the runtime check's detail as `health-checks/detail` does (`last.state` is `skip` when the card is serving, used outside the platform, or the host cannot be asked right now). Errors as above | JWT |
| GET | `/api/v1/nodes/{uuid}/replicas` | Instances still running on the node and how eviction handles them: `{"scheduling", "items": [{"instance", "deployment_id", "deployment", "state", "kind"}]}`. `kind` is `pool` (moves to another node), `pinned` (the deployment is pinned only to this node and waits), or `named` (a multi-node or PD rank: a drain starts a replacement on another node first and switches over once it runs, waiting if there is no node to go to; only forced eviction removes it immediately); stopped and failed instances are excluded. While draining, rows may also include `drain_reason` (latest structured scheduling feedback), `drain_reason_at`, and `drain_message` (the latest failed attempt on another node). These are existing observations, not a new persisted drain state; use the timestamp to distinguish historical feedback. The optional top-level `drain_offline` indicates that the agent is currently disconnected, so retirement and container removal cannot be confirmed; it clears after reconnection. | JWT |
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
| POST | `/api/v1/nodes/{uuid}/recovery/probe` | Manually trigger one recovery probe for an offline node | JWT(admin) |
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
| `last_seen_at`, `recovery_phase`, `recovery_attempts`, `last_probe_at`, `last_recovery_error`, optional `last_reject_reason`/`last_reject_at` | Existing reporting/recovery and controlled rejection history, not recovery performed by this request; unknown rejection reasons are not echoed |
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

`POST /api/v1/nodes/precheck` checks the host environment. Connectivity checks are returned according to the connection mode and detected results and may include:

- `Server reachability`: Whether the node can access the server.
- `Server to agent`: Whether the server can dial `host_address` (defaults to the SSH host). If the address includes a port, that port is used; otherwise, the port from `server_dial_listen` is used (default `0.0.0.0:18800`). In `server_dial` mode, failure to connect is an error, not a warning.
- `Connectivity`: Connectivity summary. See [No Network Connectivity Between the Node and Server](../how-to/connect-nodes.md#节点与-server-网络不通) for troubleshooting.

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
The platform lifts only the stops it set. When `scheduling_by` is `platform`, `cordon_reason` gives the current reason: `runtime_gpu_blocked` (the runtime cannot give containers their accelerators), `card_needs_host_reboot` (a card needs a host reboot or power cycle to recover), or `kube_unschedulable` (Kubernetes has the node cordoned; the platform lifts its stop once Kubernetes uncordons it).
After the platform stops scheduling, setting `schedulable` through this endpoint takes effect at once; while the reason remains, the next report stops it again.

`PATCH /api/v1/nodes/{uuid}/runtime` sets the node's container runtime preference `{runtime: auto|docker|nerdctl|udocker}`; it is saved after the agent accepts it. The node's `runtime_choices` lists the values this node may be set to, in display order: a Mac has only `auto`, every other node all four; **a Kubernetes node has no such field** (its instances are pods of the cluster, so there is no runtime to choose), and setting one returns 409 `NODE_RUNTIME_KUBE_NODE`. An invalid value returns 400 `NODE_RUNTIME_INVALID`.

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

The same key can have multiple entries (the values form a set). Each entry is `{key, value, display, display_code}`: `value` is the literal value
used for matching, `display` is display text, and `display_code` is provided only when the value is platform terminology (vendor or management network), referring to
an existing client translation. Clients prefer `display_code` and fall back to `display` if it is unavailable. Network and node values are IDs rather than names,
so renaming changes only `display` and does not affect label-based matching.

## Kubernetes (Cluster Connection)

A Kubernetes cluster joins as one DaemonSet: an administrator creates the cluster and runs the returned command with `kubectl apply` in the cluster; the agent on every selected GPU node exchanges the cluster's connect token for its own node token, and from then on behaves like an ordinary node.

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/kube/clusters` | List clusters: `{items:[{id,uuid,name,namespace,server_url,agent_image,agent_image_source,effective_agent_image,vendors,node_selector,runtime_class,image_pull_secret,containerd_socket,state_dir,api_server,engine_ports,idle_return_seconds,priority,allow_preemption,cluster_uid,stalled,revoked,node_count,created_at,pool_ids,namespace_shared_with,can_apply,can_remove,operation_blocked?,apply_command?,remove_commands?,source?:{provider_id,provider_name,provider_kind,cluster_id}}], total}`; `apply_command` contains the connect token and is returned only to administrators, and **never when the AI assistant reads this endpoint**; an entry has no vendor: `vendors` counts the joined nodes by vendor, `[{vendor,nodes}]` (most nodes first; an empty `vendor` means nodes that reported no cards), each agent detecting its own node's vendor; `remove_commands` (administrators only, also for revoked entries) are the kubectl commands that remove the entry's agents from the cluster, to run in order: delete the DaemonSet (`kubectl -n <namespace> delete daemonset gpuworks-agent --ignore-not-found --wait`), the ClusterRoleBinding `gpuworks-agent-<namespace>`, the ClusterRole `gpuworks-agent-<namespace>`, the PriorityClass `gpuworks-inference-<namespace>`, and the namespace, all with `--ignore-not-found`, and none needs the connect token; `source` appears only for clusters connected from a cloud account's cluster; `namespace_shared_with` names the other entries that use the same namespace and may be of the same cluster (sorted; an entry no node has joined yet counts, entries known to be of different clusters do not); in one cluster the one applied later takes over the other's agents, and the console warns about it; `pool_ids` are the cloud node pool IDs a cloud entry uses (ascending, an empty array for a manual entry, kept after the cloud account is deleted); `cluster_uid` is empty until the first node joins; `idle_return_seconds` of -1 means borrowed cards are never returned; `agent_image_source` is the registry the entry pulls the published agent image from (`dockerhub`, `swr`, `quay`), and `effective_agent_image` is the image the manifest actually names (`agent_image` when that is set); `stalled` is computed by the server: no node has joined, the cluster is not revoked and it is older than `stalled_after_seconds` of `GET /kube/defaults`, so a client suggests troubleshooting; `can_apply` and `can_remove` say whether the server can apply the manifest to the source cluster or delete it from there, and `operation_blocked` says why not: `no_source` (an entry made by hand, so the server has no way into the cluster), `private_api` (the cloud says the cluster's API server has no public address, so `kubectl apply` must run from inside the cluster's network; not decided while the cloud account cannot be asked, the operation itself then reports the failure), `revoked` (the token is revoked: only applying is refused, removing still works). The server keeps the cloud's cluster listing for 1 minute, so polling the list does not call the cloud API each time | JWT |
| GET | `/api/v1/kube/defaults` | The dictionary the cluster form needs: `{namespace, state_dir, engine_ports, idle_return_seconds, idle_return_keep (-1, the idle_return_seconds value that means never return), agent_image, stalled_after_seconds, agent_image_source, agent_image_sources:[{id, image}], priority, priority_max, allow_preemption}`; `priority` is the priority a create request that omits it takes (10000), from 0 to `priority_max`, and `allow_preemption` defaults to false; `agent_image` is the image in the default registry (`agent_image_source`, Docker Hub) at the current runtime revision, and `agent_image_sources` lists every registry the agent image is published to (the default first) with its image; `namespace`, `state_dir`, `engine_ports`, `idle_return_seconds` and `agent_image` are the values the server takes when a create request omits the field; a client shows them only as hints and leaves the field empty; the dictionary has no vendor (an entry has none; a RuntimeClass affects only NVIDIA instance Pods, and other vendors' devices come through the agent's own device plugin and ignore it) | JWT |
| POST | `/api/v1/kube/clusters` | Create a cluster: `{name, server_url, namespace?(gpuworks-system), agent_image?(your own image; left empty, nothing is stored and the manifest is rendered with the image at the current runtime revision in the `agent_image_source` registry), agent_image_source?(dockerhub\|swr\|quay; when omitted, Docker Hub for an entry made by hand, and for a cloud entry by the cluster's region: swr in Alibaba Cloud's mainland China regions, dockerhub elsewhere; other values return 400), node_selector?("k=v,k2=v2"), runtime_class?(affects only NVIDIA instance Pods; empty means each node decides: use the nvidia RuntimeClass if it exists, otherwise the default runtime), image_pull_secret?(name of a docker-registry Secret in the namespace used to pull the agent image; empty means none), containerd_socket?(empty means the agent finds it itself), state_dir?(/opt/gpuworks), api_server?("https://host:port"; empty means the cluster's kubernetes Service), engine_ports?(40000-42767), idle_return_seconds?(60), priority?(10000, 0 to 1000000000: the PriorityClass value of the hold and instance Pods), allow_preemption?(false: the PriorityClass says preemptionPolicy: Never), provider_id?, provider_cluster_id?, provider_pool_ids?(give provider_id and provider_cluster_id together to connect from this cluster of a cloud account; see GET /providers/{id}/clusters; `provider_pool_ids` is then required and names the cluster's GPU node pools the entry uses, one or several, of any vendor, and `node_selector` may no longer be given; a manual entry gives none of the three and keeps the free-form `node_selector`)}`; returns 201. Unusable parameters return 400 `KUBE_CLUSTER_INVALID` (`field`, `reason`; also for a node pool that does not exist or none chosen; an entry has no vendor, so node pools may be of any vendor), a duplicate name returns 409 `KUBE_CLUSTER_NAME_TAKEN`, and a source cluster that already has an entry returns 409 `KUBE_CLUSTER_SOURCE_TAKEN` (`cluster`; a cluster has one entry, so edit its node pools to use more of them). On creation the server checks the node pools against the cloud provider, and a cloud error returns 424 | JWT(admin) |
| PUT | `/api/v1/kube/clusters/{uuid}/pools` | Replace the entry's node pools: `{provider_pool_ids}`, with the same rules as creation (at least one, all of the entry's source cluster, otherwise 400 `KUBE_CLUSTER_INVALID`; the node pools may be of any vendor); returns the updated cluster (one item of the GET list). The manifest is rendered from the new node pools at once, but **the DaemonSet in the cluster changes only when the manifest is applied again** (`POST …/apply` or kubectl); agents on the pools taken away exit and their nodes go offline; after 15 minutes offline, the server asks another online agent of the same entry whether the node still exists and still runs one of the entry's agent Pods, and when not, and the node has no instances, the node record is removed automatically (when the entry has no other online node nothing is asked; delete the node by hand). An entry made by hand, or one whose cloud account is deleted, returns 409 `KUBE_CLUSTER_NO_SOURCE`; for a cloud entry made earlier with a free-form node selector, editing its node pools makes it select by the pools alone (the node selector is cleared) | JWT(admin) |
| PUT | `/api/v1/kube/clusters/{uuid}` | Edit the entry: every body field is optional, omitted means keep and an empty string clears: `name, node_selector, agent_image, agent_image_source, image_pull_secret, runtime_class, containerd_socket, api_server, engine_ports, idle_return_seconds, priority, allow_preemption`. Unknown fields return 400; `namespace`, `server_url`, and `state_dir` are fixed at creation and cannot change; a cloud entry selects nodes by node pool, and changing its `node_selector` returns 400 `KUBE_CLUSTER_INVALID` (change node pools with `PUT …/pools`). Validation is as on creation: 400 `KUBE_CLUSTER_INVALID` (`field`, `reason`), a duplicate name returns 409 `KUBE_CLUSTER_NAME_TAKEN`, and an unknown uuid returns 404. Returns `{cluster, changed, reapply, commands}`: `cluster` is one item of the GET list, `changed` is the JSON names of the fields that changed (sorted), and `reapply` true means the manifest changed and the cluster keeps running the old one until it is applied again; `commands` (administrators only, for an entry made by hand and not revoked) is one kubectl line: `kubectl delete priorityclass gpuworks-inference-<namespace> --ignore-not-found && <apply command>` (Kubernetes cannot change a PriorityClass's value or `preemptionPolicy` in place, and an earlier priority change may not have been applied yet); empty when the manifest did not change. A cloud entry is applied with `POST …/apply`, and the server deletes and recreates a changed PriorityClass itself. `idle_return_seconds` is also written into the manifest (the annotation `gpuworks.ai/idle-return-seconds` on the DaemonSet's Pod template), so changing it needs the manifest applied again, and the agents pick the new value up when they enroll after restarting; instance Pods are not restarted | JWT(admin) |
| GET | `/api/v1/kube/clusters/{uuid}/manifest` | The cluster's manifest (`application/yaml`) | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/revoke` | Revoke the connect token: nodes already connected are unaffected, and new nodes can no longer join | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/apply` | Apply the cluster's manifest to its source cluster: `{dry_run}` (required; if omitted, returns 400 `KUBE_CLUSTER_INVALID` and never executes by default). The server obtains a short-lived kubeconfig from the cloud account on the spot (not stored; ADR 0063) and server-side applies the objects one by one; with `dry_run`, it only has the API server check them; when a PriorityClass's value or `preemptionPolicy` changed, which Kubernetes does not allow in place, the server deletes and recreates it and the `action` of that item is `replace`. Returns `{dry_run, results:[{kind,name,namespace?,action(create\|update\|replace\|failed\|not_tried\|dependent),error?}]}`. If the cloud account cannot issue credentials or the cluster refuses, returns 424 with the same `results` in the body, plus `code: KUBE_CLUSTER_OPERATION_FAILED` and `params:{step(kubeconfig\|api_server),reason}`. An entry created by hand, or whose cloud account was deleted, returns 409 `KUBE_CLUSTER_NO_SOURCE` | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/remove` | Delete the manifest's objects from the source cluster (the DaemonSet first, then the rest in reverse order; cluster-wide objects are named for the namespace, so other entries of the cluster are untouched; `action` additionally has `delete\|absent`); request, response, and errors are the same as apply | JWT(admin) |
| DELETE | `/api/v1/kube/clusters/{uuid}` | Delete the cluster, returning 204; returns 409 `KUBE_CLUSTER_HAS_NODES` while it still has nodes | JWT(admin) |
| GET | `/kube/manifest.yaml?token=` | Fetch the manifest with the connect token, for `kubectl apply -f`; an unknown or revoked token returns 404 | Connect token |
| POST | `/api/v1/kube/enroll` | The agent exchanges `{token, cluster_uid, node_name}` for a node token: returns `{node_uuid, node_token, idle_return_seconds}`; the same node of the same cluster always gets the same node. The token binds to a cluster UID on first use, after which another cluster gets 403 `KUBE_ENROLL_REFUSED` (`params.reason: other_cluster`); an unknown or revoked token returns 403 (`params.reason: unknown_token`; an agent that has enrolled before keeps its saved identity); a Kubernetes node belongs to only one entry at a time: when another entry of the same cluster selects the same node and that entry's agent on it is online or was seen in the last 15 minutes, it returns 403 (`params.reason: node_taken`, `params.entry` the name of the entry holding the node), after which the node can move to the other entry | Connect token |

## Models

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/models` | List models; supports `page`/`page_size`, `q`, `source` (`huggingface/modelscope/url/local`), `operation`, and `source_id`. The last three accept comma-separated values; `source_id` uses case-insensitive whole-value matching. `sort` accepts `name/source/source_id/published_at/created_at/weight/presence`. Paginated responses return a `presence{ready_nodes,cached_nodes,ready_storages,in_flight,in_flight_percent,failed}` presence summary and a `summary` for all models; query model details for per-location information. See [List Filtering and Presence Summary](#列表筛选与就位汇总) | JWT |
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

### List Filtering and Presence Summary

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
| GET | `/api/v1/images/{id}` | Get image series details; each version's node rows include `node_name`, `node_state`, `image_digest_id`, and `updated_at`. Rows acquired through peer sync also include `source_node_name` | JWT |
| PATCH | `/api/v1/images/{id}` | Update an image series: `enabled`; community/custom series also allow changes to `common_args`/`common_env` (system series return 400 `OFFICIAL_IMAGE_SERIES_MANAGED`) | JWT(admin) |
| DELETE | `/api/v1/images/{id}` | Delete a community/custom image version; system images return 400 `OFFICIAL_IMAGE_DELETE_FORBIDDEN`. Returns 409 `IMAGE_IN_USE` if still referenced by deployments, unfinished instances, or models (using the same reference criteria as system image deletion/deprecation) | JWT(admin) |
| DELETE | `/api/v1/image-series/{id}` | Delete a community/custom image series; returns 409 `IMAGE_IN_USE` if any version is still referenced | JWT(admin) |
| POST | `/api/v1/images/import` | Accepts two request body types: a system catalog manifest with `schema_version` (schema 2/3; other values return 400), or `{"series":[...]}` for community/custom additions (cannot set `source=official`) | JWT(admin) |
| POST | `/api/v1/images/sync` | Update the system image catalog from the release channel; ignores the request body and returns version counts and series. For errors, see [Catalog Import Results](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/images/preview-archive-url` | Parse a `docker save` archive URL before downloading: `{"url"}` → `url`, `size_bytes`, `repo_tags`, `previewable` | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync` | Trigger image acquisition on a node: an empty request body pulls according to [Pull Settings](../how-to/configure-pull-settings.md) (uses the same candidates as deployment startup; you cannot specify a repository); `source: url` / `url-relay` imports an HTTP archive; or use direct peer sync from `source_node` (optional `rate_limit_kib_s`, unrestricted by sync groups). A `source` value other than `""`, `default`, `url`, or `url-relay` returns 400 `IMAGE_SOURCE_REMOVED`; returns 409 `IMAGE_SOURCE_FORBIDDEN` if source rules leave the node with no available pull source. With `"force":true`, pulls again even if the image is already `present` on the node | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/probe` | Test the direct-connection handshake from `source_node` to the target node without transferring bytes | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/retry` | Retry image acquisition on a node; peer sync reuses the recorded source node and previous rate limit | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/cancel` | Cancel an image transfer in progress on this node | JWT |
| DELETE | `/api/v1/images/{id}/nodes/{uuid}` | Delete this image from the node | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/inspect` | Read the image's OCI configuration (`entrypoint`/`cmd`/`env`/`volumes`) and image ID on the node; configuration is returned only in the response and is not written back to the catalog | JWT(admin) |
| GET | `/api/v1/storages` | List storage (type, path or export, node summary and problem nodes, model count, and deployments using it) | JWT |
| GET | `/api/v1/storages/{id}` | Get a single storage; `?all_nodes=1` returns the status of each node | JWT |
| POST | `/api/v1/storages` | Add storage; NFS types are test-mounted on one node first, and storage that cannot be written to is saved as disabled (read-only). Validation errors are told apart by cause, all 400: `STORAGE_PATH_INVALID` (malformed path, `params.path`), `STORAGE_PATH_DENIED` (under `storage_denied_paths`, `params.path`), `STORAGE_PATH_REQUIRED` (no directory), `STORAGE_CACHE_LIMIT_NEGATIVE` (negative cache limit); the same applies to updates, trial mounts and node overrides | admin |
| PATCH | `/api/v1/storages/{id}` | Update the name, enabled status, default status, node scope, path, mount options, cache directory and limit, or `nfs`↔`nfs_cache` type. Switching `nfs_cache` to `nfs` requires empty node caches, otherwise 409 `STORAGE_CACHE_NOT_EMPTY` (`params`: remaining entries, nodes, entries still transferring). Include `expected_updated_at` from the read response to prevent overwriting concurrent changes; a version change returns 409 `STORAGE_CONFIG_CHANGED` | admin |
| DELETE | `/api/v1/storages/{id}` | Remove a storage object (without deleting files) and remove models stored only in it from the model list; returns 409 `STORAGE_IN_USE` if deployments are bound to it or a deployment's model is stored only in it | admin |
| POST | `/api/v1/storages/probe` | Test-mount a storage definition on one node without leaving it mounted. Results name the actual `node_uuid`, `node_name`, `target` (the export or directory path actually checked) and `checked_at` (control-plane receipt and decode time); they describe only this node and this check. Older responses may omit the latter two fields. | admin |
| GET | `/api/v1/storages/{id}/control-planes` | Get other gpuworks control planes using the shared storage (heartbeats) | JWT |
| PATCH | `/api/v1/storages/{id}/node-overrides` | Atomically update cache overrides for fixed `node_uuids`; omitted `cache_path`/`cache_max_bytes` remain unchanged, null inherits, and a limit of 0 means unlimited. New overrides do not expand the node scope | admin |
| POST | `/api/v1/storages/{id}/cache/reconcile` | Submit background cache reconciliation with `{node_uuids, mode}` (`mode` is `"trim"` or `"clear"`); the 202 response returns the accepted count. See the cache status in storage details for per-node results | admin |
| GET | `/api/v1/storages/{id}/model-files` | Get model files in storage: one row per model for shared storage, and one row per node for local storage and `nfs_cache` node caches (including the last-used time and bound deployments) | JWT |
| POST | `/api/v1/storages/{id}/scan` | Scan model directories in shared storage, with pagination (`cursor`) | admin |
| POST | `/api/v1/storages/{id}/import` | Import scanned directories; writes only to the database | admin |
| DELETE | `/api/v1/storages/{id}/cache` | Evict all evictable node caches for `nfs_cache` storage; use `?node_uuid=` to restrict this to one node | admin |
| GET | `/api/v1/storages/{id}/files` | Browse files in storage ([ADR 0053](../adr/0053-storage-files.md)): `?path=` (relative to the storage root; empty means the root) `&cursor=&limit=` (up to 500 per page, ordered by name bytes; an empty `next_cursor` indicates the last page). Returns `{path, node_uuid, writable, items[{name, path, dir, symlink, size, mtime, writable}], next_cursor}`. Top-level `writable` indicates whether children can be created in the current directory; item-level `writable` indicates whether the item can be renamed/deleted. Symbolic links are listed but not followed. For shared storage, `node_uuid` is optional (automatically selects an online, ready node with agent support and a connected data lane, preferring writable nodes). It is required for local storage; otherwise, returns 400 `STORAGE_FILE_NODE_REQUIRED`. For `nfs_cache`, browses NFS itself, not the node cache | admin |
| GET | `/api/v1/storages/{id}/files/download` | Download a file or directory: `?node_uuid=&path=&format=zip\|tar.gz`. Files are returned unchanged (`Content-Length`, `application/octet-stream`); directories are archived while streaming (zip by default, `application/zip` or `application/gzip`, no `Content-Length`). Symbolic links are skipped and listed in `SKIPPED_SYMLINKS.txt` inside the archive. `Content-Disposition: attachment` includes the filename (directories append `.zip`/`.tar.gz`). Errors before byte transfer starts return JSON; errors afterward close the connection directly. Downloads remain available when storage is disabled | admin |
| PUT | `/api/v1/storages/{id}/files/upload` | Stream an upload in a single request: `?node_uuid=&path=&extract=tar\|tar.gz\|zip&overwrite=true\|false`, with file bytes as the request body (`application/octet-stream`). `path` is the destination file without extraction, or the destination directory with extraction. The server does not write to disk or cache the upload; `Content-Length` is the expected size (unknown when omitted). The agent first stages the upload in `.gpuworks/incoming/`, then atomically renames it to the destination after completion (including extraction) before returning `{path, bytes, files}`. All checks that do not require the agent are completed before reading the request body | admin |
| POST | `/api/v1/storages/{id}/files/mkdir` | Create a directory with `{node_uuid, path}` (including missing parents); existing directories do not cause an error. Returns `{path}` | admin |
| POST | `/api/v1/storages/{id}/files/rename` | Rename or move with `{node_uuid, from, to}`; both paths must be in writable regions, and the destination must not exist. Returns `{path}` | admin |
| DELETE | `/api/v1/storages/{id}/files` | Delete a file or directory with `?node_uuid=&path=`: first moves it to the storage's trash directory, then deletes the bytes in the background; 204 | admin |
| GET | `/api/v1/storages/settings` | Get the control plane display name and reserved free space on cache disks | JWT |
| PATCH | `/api/v1/storages/settings` | Update the preceding settings | admin |
| GET | `/api/v1/storages/usage` | Get model disk usage statistics for each node | JWT |

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
| GET | `/api/v1/inference-templates/index` | Get the template index (a lightweight list for selectors) | JWT |
| GET | `/api/v1/inference-templates/organizations` | List organizations/publishers represented in the templates | JWT |
| GET | `/api/v1/inference-templates/chips` | Static catalog of accelerator cards supported by templates; optional `arch_id` identifies the architecture required by a template, with other vendor fields unchanged | JWT |
| GET | `/api/v1/inference-templates/my-chips` | List accelerator cards detected in the current cluster, with `vendor`, `name`, optional `cc`, and reported `arch_id` (omitted when unavailable) | JWT |
| GET | `/api/v1/inference-templates/summary` | Get template summary statistics | JWT |
| GET | `/api/v1/inference-templates/weight-models` | Registered models for the weight sources one spec declares (`template_id`, `spec_source`, and `spec_name` are required; a missing one returns 400 `TEMPLATE_WEIGHT_MODELS_QUERY_INVALID`, an unknown spec 404): `models` are the models serving any source (repo ID matched case-insensitively, a whole repository serves a subfolder request, newest first, each once, rows shaped as in the paged list); `sources` lists the sources in declared order, and `reuse_model_id` is the newest same-provider model a choice of that source reuses | JWT |
| GET | `/api/v1/inference-templates/export/{digest}` | Export an executable template package by content digest | JWT(admin) |
| GET | `/api/v1/inference-templates/model/*` | Get template details by model path (wildcard path) | JWT |
| GET | `/api/v1/inference-templates/yaml?model_id=…` | Get flat YAML for the template editor: the server writes it using the same catalog types as import. Add `spec_source=user` (or `upstream`) to retain only specs from that source; `readme_*` fields are written only when all specs are listed or the template itself belongs to the user. Saving it unchanged (`import?overwrite=true`) preserves all fields | JWT |
| GET | `/api/v1/inference-templates/schema` | Get the dictionary for the flat YAML editor: field completion, hover descriptions, and constraints for initial client-side validation (authoritative validation still occurs at import); optional `spec.gpus.arch_id` must be nonblank and matches the reported architecture ID exactly, ignoring case (missing IDs do not match) | JWT |
| POST | `/api/v1/inference-templates/clone` | Clone a template into an editable user spec | JWT(admin) |
| POST | `/api/v1/inference-templates/evaluate` | Check flat YAML or a catalog package without writing data; accepts the same request as `import` and supports `?overwrite=true`. Returns 200 `{valid, errors, warnings}`; for check scope and results, see [Pre-Import Checks](#导入前检查) | JWT(admin) |
| POST | `/api/v1/inference-templates/import` | Import a template/catalog package; returns `{models, package_digest}` on success, with an additional `warnings` field when warnings exist (same as `evaluate`; the field is absent when there are no warnings) | JWT(admin) |
| POST | `/api/v1/inference-templates/sync` | “Check for updates”: fetch the official inference catalog package from the release channel and reconcile upstream sources (leaving user specs unchanged); ignores the request body. Returns `{models, package_digest, version}`. Returns 409 `UPDATE_CATALOG_INCOMPATIBLE` if the catalog package schema is newer than this server supports; returns 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` if package parsing or validation fails (`params.error` contains the original error, and the UI indicates that this version of the inference templates requires the latest server version). For other errors, see [Catalog Import Results](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/inference-templates/official` | Upload an official inference catalog package (request body is a `.tar.gz` catalog package, up to 32 MiB), using the same upstream reconciliation as “Check for updates.” Returns 400 `OFFICIAL_CATALOG_PACKAGE_REQUIRED` if the input is not a catalog package, or 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` if parsing or validation fails | JWT(admin) |

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
| GET | `/api/v1/deployments` | List deployments; with `page`/`page_size`, returns paginated list columns and supports filtering by `q`/`state` (see “Error and Response Conventions”). `state` includes `sleeping`/`asleep`, and rows also carry `asleep_replicas` and `sleep_level`; see [Deployment Sleep State](#部署的睡眠状态stateasleep_replicassleep_level) | JWT |
| GET | `/api/v1/routing-policies` | Get server-provided multi-replica routing policies and localized descriptions | JWT |
| POST | `/api/v1/deployments/preflight` | Run preflight checks for multi-node inference deployment (rank assignment and network probing) | JWT |
| POST | `/api/v1/deployments` | Create a deployment (`model_source` contains only `{storage_id, provider}`: where to store the model and which provider's weights to run; it does not accept an image acquisition method. `placement.pd` specifies a PD disaggregated deployment; see below). Returns 409 `SERVED_NAME_IN_USE` if the served model name is occupied by a deployment that is not being deleted; `params` includes `served_name`, `deployment`, and `deployment_id`. When omitted, the name defaults to the model name and undergoes the same validation | JWT |
| GET | `/api/v1/deployments/{id}/candidates` | Query the candidate pool for the next replica; `status` is `room/busy/unmet/cordoned/offline`. Supports `offset` and `limit` (1–200, default 50). Returns overall `counts`, `unmet_by_key`, and one page of `items`; `room` does not guarantee that the model fits. Check the deployment's `placement_status` as well. See [Query the Candidate Pool](#候选池查询) | JWT |
| GET | `/api/v1/deployments/{id}/draft` | Read the deployment back as the request that creates it (`deployment`), with `revision` and the non-editable fields `locked`. The deploy page fills its edit and "Clone and modify" forms from it. See [Edit a Deployment](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}` | Edit a deployment: submit the modified draft with `base_revision`; `?dry_run=1` returns only the change plan. Launch configuration changes replace instances according to `apply` (`rolling`/`all`/`none`). See [Edit a Deployment](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}/requirements` | Replace the node requirements and placement strategy of a regular single-node deployment and return its details. Affects only replicas started afterward; running replicas are not migrated. See [Modify Node Requirements](#修改节点条件) | JWT |
| GET | `/api/v1/deployments/{id}` | Get deployment details. Takes no query parameters; returns the deployment object directly, or 404 if not found. Includes `state` (the one state to show, `asleep`/`sleeping` when it sleeps), `asleep_replicas` (replicas the gateway does not dispatch to because of sleep), `sleep_level` (the level an engine reports), `instances`, `node_requirements`, `node_requirement_labels`, `placement_strategy`, `ready_replicas` (two counts of the same replicas as `asleep_replicas`; a PD deployment also gives the per-role breakdown), `health`, `tokenize_enabled`, and `max_replicas`; `placement_status` gives scheduling failure reasons, `retry_exhausted` identifies replicas that have exhausted their retries, and `restart_requested_at` identifies a rolling restart. See `units` for multi-node replicas. See [Query Deployment Details](#查询部署详情) and [Deployment Sleep State](#部署的睡眠状态stateasleep_replicassleep_level) | JWT |
| PUT | `/api/v1/deployments/{id}/routing` | Change the routing policy (`least_request`, `prefix_affinity`, or `failover`) | JWT + administrator |
| PUT | `/api/v1/deployments/{id}/dgpu` | Adjust `vram_limit_gib` for a single-node dGPU deployment. Returns `live` instances where the change takes effect immediately and `pending` instances that require a restart. See [Adjust the GPU Memory Limit](#调整显存上限) | JWT |
| PUT | `/api/v1/deployments/{id}/health-check` | Enable or disable health checks for the deployment and set the threshold: `{"enabled": true, "threshold_sec": 60}` (`threshold_sec` is 1–600; omit it or use 0 to retain the current value). Enabling requires the model to provide chat or embedding; otherwise returns 400 `FIELD_INVALID` (`params.field` is `health_check`). An out-of-range threshold returns 400 `FIELD_INVALID`. Does not restart instances; the new settings apply to the next check. Returns deployment details | JWT |
| PUT | `/api/v1/deployments/{id}/tokenize` | Enable or disable gateway forwarding of tokenization requests (`/v1/tokenize`, `/v1/detokenize`) to this deployment: `{"enabled": false}`. `enabled` is required; omitting it returns 400 `FIELD_INVALID` (`params.field` is `enabled`), and a non-boolean value returns 400. Returns 404 if the deployment does not exist. Does not restart instances; takes effect after the gateway routing table is rebuilt in response to the change event. Returns deployment details (including `tokenize_enabled`) | JWT |
| GET | `/api/v1/deployments/{id}/events` | Deployment event timeline (newest first, cursor pagination, collapsed on read) | JWT |
| POST | `/api/v1/deployments/{id}/clone` | Clone the deployment configuration, including node requirements and placement strategy. The clone's served model name is the original name followed by `-` and 4 random lowercase letters or digits. The request body `{"name": ...}` sets both the deployment name and the served model name; returns 409 `SERVED_NAME_IN_USE` if the name is taken | JWT |
| POST | `/api/v1/deployments/{id}/catalog-template` | Write a running deployment as an inference template: first send the smallest request each cheap operation (chat, completions, embeddings) takes, then build flat YAML from the launch arguments, weight commit and GPUs the deployment actually ran. Nothing is written to the catalog. See [Save as an inference template](#另存为推理模板) | JWT |
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
| GET | `/api/v1/instances` | List instances | JWT |
| GET | `/api/v1/instances/{uuid}` | Get instance details. Server-derived fields: `replica` (the replica `{role,index}` served by this instance; omitted if none), `asleep` (not receiving requests because of sleep: commanded to sleep or still asleep), `desired_sleep` (the commanded level), `sleep_held` (the instance's sleep was ordered by an operator or a schedule, so a request does not wake it; the hold is recorded per instance and a wake clears only the woken instances' holds), and `can_sleep_alone` / `can_wake_alone` (whether it can sleep or wake individually). Followers of a multi-node replica do not report their own sleep level; `sleep_level` is the level of the leader in their generation. For old rows without a recorded role, `dist_role` is inferred from headless and rank. `instances[]` in deployment details includes the same fields, and PD roles include `may_be_empty` | JWT |
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
  "checks": [{"operation": "openai.chat", "endpoint": "/v1/chat/completions", "tested": true, "passed": true, "http_status": 200}],
  "notes": [{"code": "verified_on", "params": {"gpu": "NVIDIA H20", "count": 1}}],
  "excluded_env": ["HF_TOKEN"],
  "evidence": "GPUWorks template test record\n..."
}
```

- `yaml` imports as it is; the comments at its top state the same facts as `notes`. The comments are in English; the UI localizes `notes` from their `code` and `params`.
- `checks`: chat, completions and embeddings each get the same smallest request the health check sends (`tested: true`); video, image, speech and other operations get no request, `tested: false`, and an `operation_not_tested` note.
- When a tested operation fails, `verified` and `saveable` are both `false` and `yaml` is for troubleshooting only.
- `notes` codes: `verified_on`, `operation_passed`, `operation_failed`, `operation_not_tested`, `kv_bytes_missing` when there is no `kv_bytes_per_token`, `model_config_missing`, `placement_omitted` (`gpu_count`, `gpu_memory_mib`), `vram_limit`, `env_excluded`, `parameters_dropped` (settings no engine flag carries, such as `cpu_cores`), `container_overrides_omitted`.
- `evidence` is the plain-text test record to attach when proposing the template upstream.

When the deployment cannot be written as a template, it returns 409 `TEMPLATE_EXPORT_UNAVAILABLE` with `params.reason`: `not_running` (the deployment is not running or has no running instance), `distributed` (multi-node), `pd`, `no_gpu_facts`, `template_assets`, `components` (the source template carries packaged files or component models), `weights_not_public` (weights from a local directory or a URL), `revision_unpinned` (the weight revision is not a commit), `revisions_differ`, `template_missing`, `model_missing`, `launch_changed` (configuration or the running instance changed), `launch_snapshot_missing` (no complete verifiable launch record), `heterogeneous_gpus` (different card models), `command_overridden` (the deployment replaced the engine command). When the exported arguments cannot rebuild the original command it returns 409 `LAUNCH_NOT_EXPORTABLE`. An unknown deployment is 404, and a body that is not valid JSON is 400. How the template is built is described in [Save a deployment as an inference template](../how-to/save-deployment-as-template.md).

### Deployment List Filters and Response Fields

`q` matches the deployment name, served model name, and model name. `state` corresponds to the state displayed in the UI: `running`, `starting`, `pulling`,
`pending`, `offline`, `failed`, `stopped`, `deleting`, `sleeping`, or `asleep`; the deleting state takes precedence over runtime states.
A value must be one of these; anything else returns 400 `LIST_FILTER_INVALID` rather than silently matching everything.
Sortable columns are `name`, `model`, `state`, `replicas`, and `created_at`; `sort=state` sorts by this same `state`.

`state` is the one state a row should display; `runtime_state` is not, because a sleeping engine is still running and stays `running` there
while the gateway no longer dispatches requests to it. The precedence is:

- A deployment being deleted reads `deleting`, ahead of any runtime state.
- Otherwise it equals `runtime_state`, with two exceptions that apply only while `runtime_state` is `running`: it reads `asleep` when the gateway refuses every request because the replicas it needs sleep, and `sleeping` when the sleep order has been accepted but at least one engine has not reported its level yet (see [Query Deployment Details](#查询部署详情)).
- `offline` is a value of `runtime_state` itself: no replica can serve (no running, non-headless instance on an online node whose multi-node generation is complete), while running instances remain on nodes that went offline.
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
| `placement.gpus`, `placement.tp`, `placement.pp` | Explicit physical GPU indexes; TP and PP are parallelism for the entire deployment (TP × PP ÷ `nnodes` GPUs per node, which must divide evenly). If TP is omitted for multi-node deployment, the default is one pipeline stage per node, with TP set to the smallest GPU count among the selected nodes; effective values are written back to placement. If TP is omitted for single-node deployment, the scheduler selects a GPU count that is an integer multiple of PP (TP = GPU count ÷ PP). Automatic placement also selects nodes according to the supplied TP and PP. Evaluation returns values in `form_values.tp/pp` and `parallelism` (`tp`, `pp`, `nodes`, `gpus_per_node`); `acquisition.nodes` in multi-node evaluation and manual placement provides acquisition estimates for each node |
| `routing_policy` | Multi-replica gateway policy: `least_request` (default), `prefix_affinity`, or `failover`; not an engine launch parameter |
| `tokenize_enabled` | Whether the gateway forwards `/v1/tokenize` and `/v1/detokenize` to this deployment (see [OpenAI-Compatible Gateway](openai-gateway.md#分词tokenize)). Enabled if omitted; `false` disables it. Independent of the engine; if the engine does not provide these endpoints, the engine itself returns an error |
| `health_check`, `health_check_threshold_sec` | Service health checks (see "Health Checks" below): when `health_check` is omitted, enabled if the model provides chat or embedding, otherwise disabled. If explicitly `true` and the model provides neither, creation returns 400 `FIELD_INVALID`. The threshold is 1–600 seconds; omission or 0 means 60. For out-of-range values, evaluation reports a field error and creation returns 400, both with code `FIELD_INVALID` |
| `start_timeout_sec` | Startup health check budget (seconds) for instances of this deployment: the maximum time after an instance starts to wait for the engine health check to pass. Omission or 0 selects an automatic budget, using the cluster default (setting `deployment.start_timeout_sec`, factory default 1800) as a floor and considering known engine deadlines; otherwise must be 60–86400. For out-of-range values, evaluation reports a field error and creation returns 400, both with code `FIELD_INVALID`. Saved with the deployment and returned under the same field name in deployment details when a value is set. The value is fixed when an instance is created; changes affect only instances started afterward |

Instance and evaluation responses expose the actual budget and conflicts as `start_timeout_sec`, `start_timeout_source`, `engine_start_timeout_sec` and `start_timeout_conflict`. `startup_estimate` provides only a rough formula estimate: when `method=weight_reference`, `estimated_seconds = weight_bytes / (reference_mib_per_sec × 1024 × 1024) + initialization_seconds`. The reference loading rate is `512 MiB/s`. The aggregate initialization allowance follows the effective engine/mode: `240 seconds` for SGLang, `300 seconds` for ordinary vLLM and `420 seconds` for vLLM with an explicit final `--omni` flag. These are project reference profiles, not paper coefficients or node measurements; no extra adjustment is inferred from runtime, GPU count or eager settings. Missing weights use `method=missing_weight`; unsupported configurations use `unsupported`. Unknown numeric estimates and reference parameters are omitted instead of reported as zero. The estimate does not change the startup budget. Optional instance `measured` fields `StartupSeconds`, `StartupTimeoutSeconds`, `ModelLoadingSeconds`, `CompilationSeconds` and `GraphCaptureSeconds` represent successful health wait, unfinished-wait lower bound and explicitly logged durations; they must not be added together.

### Health Checks

When enabled, the server sends a minimal request through the gateway every minute, selects a replica according to the gateway policy, and waits up to the threshold:
for chat, it sends a message containing `1` with `max_tokens:1`; when only embedding is available, it sends the input `1`.

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

When no group fits, the request returns 409 `UNIT_PLACEMENT_NONE`, with `params` `nodes` (total nodes), `unmet` (node requirements not met), `cordoned`, `offline`, `busy` (not enough free cards), `no_network` (no usable compute network), `no_peer_memory` (a PD mode moves KV over RDMA and the node has not loaded the peer memory module), and `largest` (how many nodes the largest group has). When a PD mode moves KV over RDMA and nodes were left out for the peer memory module, the answer is 409 `PD_PEER_MEMORY_NONE` instead (`params`: `mode`, `nodes`, `no_peer_memory`, `module`, `fix` with the fix command); a manual placement on such a node returns `PD_PEER_MEMORY_MISSING` (`mode`, `role`, `node`, `module`, `fix`, `state`). Evaluation does not answer these failures with an HTTP error; it returns `ok=false` with an entry in `errors[]` whose `field` is `placement_mode`, and the response carries `auto_placed: true`.

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
(authentication, model not found, server errors) retain their original status codes. Field path conventions:

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
`MOUNT_TARGET_DUPLICATE`/`MOUNT_TARGET_NESTED`), JSON parameters (`QUICK_PARAM_JSON_INVALID`),
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
| `eligible_nodes` | Each node is evaluated against the draft's node requirements (including vendor; the manually selected node itself is not a requirement, because the dropdown is used to select it), the template's GPU predicate, and online status. When `eligible=false`, `reasons[]` provides the reasons (`node_offline`, `node_requirements_unmet`, `vendor_mismatch`, `chip_mismatch`; when the node requirements themselves are invalid, every node has `node_requirements_invalid`, rather than all nodes appearing eligible). The node dropdown renders only this list |
| `gpu_candidates` | Manual single-node feedback: `{node_uuid, items}` with `index`, `available`, and a structured `reason` for each unavailable card. This reports current resource availability, not single-card model fit or permission to select. The UI continues to gate new selections on the node card's `schedulable` state. An absent field means not evaluated; `plan` still checks model fit, GPU groups and topology. |
| `weight_sources` | Weight sources declared by the template spec (`provider`/`repo_id`/`revision`/`subfolder`), plus `suggested` (server recommendation) and `registered` (a registration already exists in this cluster, identified by repo id). After selection, the client still calls the registration API |
| `preflight` | Only for `nnodes>1`: request-level preflight results (node count, duplicates, port range, online status, and routable addresses). Deep network probing remains in the separate preflight RPC (retained by the CLI); the frontend no longer orchestrates it |
| `eviction` | Returns candidates when GPU memory is currently insufficient but space can be freed by putting awake deployments to sleep. Each entry includes `instance_uuid`, `deployment_id`, `deployment`, `gpu_indexes`, and `frees_gib` |

The server fills parameter defaults from the current engine dictionary, including engine-specific defaults. Parameters whose dependencies are not enabled receive no default.
Clients use `layout` (`quick`, `expert`, `hidden`; an empty string means `expert`) and `order` from `/engines` to arrange the parameter form.

`quick_params_unset` can contain only keys in the current engine dictionary. If the same request also supplies a value for that key in
`quick_params` or `extra_args`, it returns an unset conflict. The existing `null`
semantics remain unchanged; `null` cannot replace an explicit unset. Composite engine parameters can be removed from a template only when the adapter can recognize them without loss
and fully disable the corresponding configuration. Unknown connectors or mixed configurations return a conflict to avoid accidentally removing other capabilities.

Evaluation in automatic mode returns `selected_node`, per-node `candidates`, and the selected node's `plan`. When it does not fit, the `plan` and each candidate's `reason` say what the controller says: `waiting_for_yield` when it would fit once the holders yield, and `reserved_for_sleepers` when it does not fit only because sleepers keep the cards (both worked out with the draft's yield policy), instead of always "does not fit". When it would fit once the holders slept but yield policy keeps the draft from waiting for them, evaluation says so as well: `does_not_wait_for_yield` when the draft never yields, and `held_by_non_yielding` when some holder never yields, with the parameters `node`, `gpus`, `holders` and `blockers` (see [How a New Deployment Gets onto a Shared Card](../how-to/scheduling-center.md#新部署怎样放到共用的卡上)); these two reasons appear in evaluation only.

The evaluate response also has `yield_policy_eligible` (`{ok, reason?}`, shaped like the field of that name in the deployment details, with `reason` `SLEEP_NOT_ENABLED`, `SLEEP_ENGINE_UNSUPPORTED` or `YIELD_POLICY_SHAPE`), worked out from the draft's sleep capability, engine, node count and whether it is PD: the very check that create makes, which the deployment form uses to decide whether a yield policy can be chosen; it is absent until the draft's engine is known. When creating, the form sends the chosen yield policy as it is, and the server refuses it when it does not hold (`SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` / `YIELD_POLICY_SHAPE`).
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

- `mode`: one of the engine profile's `pd_modes` (`vllm-nixl`, `vllm-ascend-mooncake`, `vllm-mooncake`, `vllm-ppu-mooncake`, `sglang-nixl`, `sglang-ppu-ipc`, `sglang-ppu-mooncake`). All unknown fields are rejected.
- Each `unit` is a replica; `gpus` can be omitted. `followers` makes the replica span multiple nodes (ranks 1…N), in which case the role must specify `tp`.
  For each rank, the GPU count is `tp × pp` divided by the number of ranks.
- Allowed values and defaults for `fallback` and `kv_load_failure` depend on the mode: for `vllm-nixl`, `vllm-ascend-mooncake`, and `vllm-mooncake`, `fallback`
  is `decode` (default) or `fail`, and `kv_load_failure` is `fail` (default) or `recompute`. For `sglang-nixl`, `sglang-ppu-ipc`, `sglang-ppu-mooncake`, and
  `vllm-ppu-mooncake`, `fallback` only accepts `fail`, and there is no `kv_load_failure`. Values disallowed by the mode return `PD_FALLBACK_INVALID` /
  `PD_KV_LOAD_FAILURE_INVALID`.
- In `GET /api/v1/engines`, each engine's `pd_modes[]` provides these rules: `name`, `vendors`, `roles`, `scalable_to_zero` (roles that can scale to 0
  while running), `tp_rule`, `max_units_per_role`, `fallbacks`, `kv_load_failures` (the first entry is the default), and `unverified`.
- Specifying it together with `nnodes`, `node_uuids`, or `replicas > 1` returns `PD_SHAPE_CONFLICT`; use `placement_mode: "auto"` with units that omit `node_uuid` for automatic placement (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)). Other validation error codes start with
  `PD_` (field `placement.pd`). When the deployment is sleepable (`sleepable`) and KV uses the host RDMA network through UCX, creation and evaluation return
  `PD_SLEEP_RDMA_UNSUPPORTED` (`params`: `mode`, `network`).
- The `pd` field in deployment details contains `mode`, `fallback`, `kv_load_failure`, `state` (`not_ready|partial|running`), `short`,
  and `roles[]` (`role`, `tp`, `pp`, `desired`, `ready`, `may_be_empty` (the mode allows this role to be empty, meaning it can sleep independently), and `units[]`, including `followers[]`). Instances carry `role`.
  Each unit also has `failed_attempts`, `last_failure`, and `retry_exhausted` (the controller has given up rebuilding it; use the retry endpoint above to retry).
- The `POST /api/v1/evaluate` response also includes `pd.roles[].units[]`: placement, addresses, GPU memory breakdown, and commands for each replica's leader and its `followers[]`.

## Benchmarks

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/benchmarks` | List benchmark tasks | JWT |
| GET | `/api/v1/benchmarks/meta` | Benchmark dictionary (dataset/metric metadata) | JWT |
| POST | `/api/v1/benchmarks` | Create a benchmark task | JWT |
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
| GET | `/api/v1/gpus` | Cluster GPU inventory and latest metrics, using a 20-second snapshot. Supports `page`/`page_size`, `q`, `node`, `vendor`, and `model`. `allocation_state` is `available/partial/allocated/unusable/unknown`, distinct from health level. `q` matches substrings of node names/UUIDs, GPU model names, GPU UUIDs, and PCIe addresses; the other filters accept comma-separated values. For `unusable` reasons, see `schedulable/health_status/ownership/foreign_processes`. `sort` is `node/model/vendor/allocation_state/occupants/utilization/memory/temperature/power/fan/bdf/numa`; paginated results include the full `overview` and `facets`. See [GPU Filters and Allocation States](#gpu-筛选与分配状态) | JWT |
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

`overview.health` counts GPUs by health level: `ok`, `warning`, and `error`. GPUs with warnings may still serve workloads; GPUs passed through or occupied by processes outside the platform may also have health `ok`.
States refresh with the 20-second snapshot.

## API Keys (Gateway Keys)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/apikeys` | List API keys (without plaintext values), including revoked keys. Revoked keys include `revoked_at` (UTC time); active keys omit this field. When `all_deployments` is true, the key can call every model service; when false, it can call only the deployments in `deployments` (`[{id,name,served_name}]`). If all of those deployments are deleted, the list is empty and the key can call nothing | JWT |
| POST | `/api/v1/apikeys` | Create an API key. `name` must not be empty after trimming leading and trailing whitespace; otherwise, returns 400 `API_KEY_NAME_REQUIRED`. The plaintext value is returned only once, in the creation response. Optional `all_deployments` and `deployment_ids` limit which deployments the key can call: when `all_deployments` is omitted, an empty `deployment_ids` means all deployments and a non-empty one means only those. Contradictory values (true with IDs, or false without) return 400 `API_KEY_SCOPE_INVALID`; deployments that do not exist return 400 `API_KEY_DEPLOYMENT_UNKNOWN` (`params.ids`) | JWT |
| PUT | `/api/v1/apikeys/{prefix}/scope` | Change which deployments an active key can call, `{"all_deployments":bool,"deployment_ids":[...]}`. `all_deployments` is required; validation is the same as on creation. Returns the updated key (same shape as a list item). Takes effect from the next request; an unknown or revoked prefix returns 404 `API_KEY_NOT_FOUND` | JWT |
| DELETE | `/api/v1/apikeys/{prefix}` | Revoke an API key using the listed 12-character `prefix` (the first 12 characters of the full key, not the full secret). New requests after revocation completes can no longer authenticate with that key, but the record remains to identify historical usage; the prefix is not reused. Success returns `{"status":"revoked"}`. An unknown prefix returns 404 `API_KEY_NOT_FOUND`; revoking an already revoked prefix again returns 200 without changing its original `revoked_at`. After all keys are revoked the gateway does **not** fall back to anonymous access: `/v1` still requires a key and refuses every request until another key is created or an administrator turns on the `inference_open_access` security setting (see [Gateway Authentication](openai-gateway.md#authentication)) | JWT |

## Platform Tokens (Platform API Tokens)

Platform API tokens replace login sessions when calling management APIs and MCP. They are distinct from the gateway keys above, which are used only for `/v1`. Plaintext tokens start with `gwp_` and appear only once, in the creation response. The server stores only SHA-256 hashes. Each user can view and manage only their own tokens, and can do so only within a login session.

After trimming leading and trailing whitespace, a name must be 1–128 UTF-8 bytes (not characters); otherwise, returns 400 `PLATFORM_TOKEN_NAME_INVALID`.

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/platform-tokens` | Current user's tokens: `{items:[{id,name,prefix,scope,created_at,expires_at,last_used_at}], total}`; `last_used_at` updates at most once per minute | JWT (login session) |
| POST | `/api/v1/platform-tokens` | Create a token: `{name, scope?: read\|write, expires_in_days?: 0–3650}` (scope defaults to read; 0 days means no expiration), returns 201 `{token, item}`; invalid `scope` returns 400 `PLATFORM_TOKEN_SCOPE_INVALID` | JWT (login session) |
| DELETE | `/api/v1/platform-tokens/{id}` | Revoke (delete) your own token, effective immediately; returns 204. Returns 404 if the token does not exist or belongs to someone else | JWT (login session) |

## Providers (Cloud Instance Rental)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/providers/kinds` | Supported cloud provider types, including each provider's credential fields (`credential_fields[].default_value` is an optional creation default for a non-sensitive field; `placeholder` is only a hint), default `server_dial` listening address, `server_dial_supported` (false for AutoDL: Pro instances expose only HTTP ports), and `server_dial_auto_port` (true for Vast and RunPod: the driver automatically chooses the listening port and connection address according to the platform's port mappings) | JWT |
| GET | `/api/v1/providers` | List configured cloud provider accounts; each item's `category` is the platform type its kind connects (`private_cloud` for ZStack Cloud, `public_cloud` for the public cloud vendors), derived from `kind` and not stored separately | JWT |
| POST | `/api/v1/providers` | Add a cloud provider account | JWT |
| GET | `/api/v1/providers/{id}` | Read back for editing: saved endpoint/public_key and non-secret extra fields declared in the dictionary. Does not fill in provider defaults or return secrets. Credential read failures return 500; the list remains available | Login session (platform tokens and AI/MCP cannot read) |
| PUT | `/api/v1/providers/{id}` | Update a cloud provider account | JWT |
| DELETE | `/api/v1/providers/{id}` | Delete a cloud provider account | JWT |
| POST | `/api/v1/providers/{id}/sync` | Sync instances under this account: insert/update listed instances and remove records for instances no longer present in the cloud (retain associated nodes and set out-of-band recovery to `stopped`). Listing failures return 424 and leave instance records unchanged | JWT |
| GET | `/api/v1/providers/{id}/clusters` | Managed Kubernetes clusters under this account, read live from the cloud provider and not stored: `{items:[{external_id,name,region,version,state,running,public_api,gpu_pools:[{id,name,gpu_model,gpu_count,nodes,vendor,in_entry}],entry?:{uuid,name},agent_image_source}],total}`; `agent_image_source` is the agent image registry an entry made from it defaults to (by region); `entry` is the cluster entry already created from it, and `in_entry` says whether the node pool is already in that entry (the DaemonSet runs on its nodes). Returns 501 if the driver has no cluster capability, and 424 on a cloud provider error | JWT |
| POST | `/api/v1/providers/{id}/instances` | Rent a new instance; cloud provider errors return 424 (501 if unsupported) | JWT |
| GET | `/api/v1/providers/{id}/instances` | List instances under this account; `managed_cluster` indicates the instance is a node of one of the account's managed Kubernetes clusters (it joins with the cluster and cannot be installed, powered, released or resized individually); `actions` lists what the instance allows now (some of `power_on`, `power_off`, `install`, `delete`; an empty array for a managed cluster node), and a client offers exactly those instead of deciding itself | JWT |
| DELETE | `/api/v1/providers/{id}/instances/{externalID}` | Release/delete an instance; remove its record after the cloud provider confirms. Cloud provider errors return 424 (501 if unsupported), leaving the instance record unchanged; a managed cluster node returns 409 `PROVIDER_INSTANCE_IN_CLUSTER` (`cluster`) without calling the cloud provider | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/power` | Power on/off; cloud provider errors return 424 (501 if unsupported); a managed cluster node returns 409 `PROVIDER_INSTANCE_IN_CLUSTER` | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/resize` | Change the instance specification; cloud provider errors return 424 (501 if unsupported); a managed cluster node returns 409 `PROVIDER_INSTANCE_IN_CLUSTER` | JWT |
| POST | `/api/v1/providers/{id}/install` | Install the agent on a rented instance; connection parameters are the same as for node creation. If the Pod needs to restart, returns 409 first and modifies it only after explicit permission. See [Cloud Instance Installation and Restart Confirmation](#云实例安装与重启确认); if the instance is a node of a managed Kubernetes cluster, returns 409 `PROVIDER_INSTANCE_IN_CLUSTER` (`cluster`) | JWT |

For cloud provider endpoints, any cloud provider API or instance SSH error returns 424 Failed Dependency, with the reason in the `{error}` body. These endpoints do not use 502/504: CDNs and tunnels such as Cloudflare replace origin 502/504 bodies with their own error pages, losing the reason.

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
| PATCH | `/api/v1/nodes/management-roles` | Batch-update management network roles: `{"node_uuids":[...],"allow":[...],"deny":[...]}`. Changes only the listed roles, with all nodes in one transaction; returns `{"updated":N}` (the number of nodes actually changed). Returns 404 `MANAGEMENT_ROLES_NODE_NOT_FOUND` if a node does not exist | JWT(admin) |
| PUT | `/api/v1/nodes/{uuid}/management-roles` | Set the roles that this node's management network does not serve (`{"roles_off":[...]}`); returns the node's network view. Applies only to new instances | JWT(admin) |
| PUT | `/api/v1/networks/{id}` | Modify a network | JWT(admin) |
| DELETE | `/api/v1/networks/{id}` | Delete a network and its attachments and test records (running instances are unaffected) | JWT(admin) |
| POST | `/api/v1/networks/{id}/attachments` | Attach to a node (optionally with interface/IP/HCA overrides) or a node label | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}` | Modify overrides for a node attachment | JWT(admin) |
| DELETE | `/api/v1/networks/{id}/attachments/{attachmentID}` | Remove an attachment | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests/estimate` | Use the same request body without starting a test: returns the resolved node count and node-pair count. Card tests also return `card` (`rounds`, `probes`, maximum duration `seconds`, and limit `max_seconds`). Requests that would be rejected when starting a test are rejected here with the same errors | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests` | Start an internal test (`kind`: tcp / rdma_tool / card_ping / card_matrix; rdma_tool requires a host RDMA network, while card_ping (pings between accelerator cards with matching indices) and card_matrix (a matrix of all cards) require an accelerator RDMA or accelerator HCCS network. HCCS card tests use `hccn_tool -hccs_ping`; RoCE card tests use `-ping`. Returns `NETWORK_TEST_TOO_LONG` if rounds × 3 seconds exceeds 10 minutes. `scope`: all / nodes / label; `id` 0 tests the management network and supports only tcp). Returns 202 and a run record (`node_uuids` lists the tested nodes, fixed when the test starts and available during execution even before `pairs` exists). Executed by the controller | JWT(admin) |

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
| GET | `/api/v1/diagnostics/bundles` | The kept diagnostics collections `{bundles, limits}`, newest first; each is `{id, requested_by, created_at, finished_at?, from, to, deployment_ids, instance_uuids, node_uuids, doctor, state, error?, size_bytes, items: [{name, state, reason?, bytes, truncated?}]}` with `state` `running`, `ready` or `failed` | JWT(admin) |
| POST | `/api/v1/diagnostics/bundles` | Start collecting `{from?, to?, deployment_ids?, instance_uuids?, node_uuids?, doctor?}` (RFC 3339; both omitted is the last hour; `doctor` defaults to `true`); returns 202 with the collection in `state: "running"`. An invalid window or one over 7 days, or more than 64 nodes or instances, returns 400 `DIAGNOSTICS_BUNDLE_INVALID`; an unknown deployment or node 400 `DIAGNOSTICS_SCOPE_UNKNOWN`; a collection already running 409 `DIAGNOSTICS_BUNDLE_RUNNING` | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles/{id}` | One collection's state; 404 `DIAGNOSTICS_BUNDLE_NOT_FOUND` when there is none | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles/{id}/download` | Download a finished collection (`application/gzip`, `gpuworks-diagnostics-<id>.tar.gz`); 404 `DIAGNOSTICS_BUNDLE_NOT_FOUND` when it is missing or not finished | JWT(admin) |
| GET | `/api/v1/experience` | Experience mode status `{enabled, ready, port, user, password?, error?}`: whether the experience environment runs, whether its console answers, its port (the console port +10), its account, and its password while it runs; `error` says why it stopped by itself. The environment is a separate process and data directory holding only simulated data. Returns 404 `EXPERIENCE_UNAVAILABLE` on the environment's own console and on a server without a data directory | JWT(admin) |
| PUT | `/api/v1/experience/enabled` | Turn the experience environment on or off with `{"enabled":true\|false}`: turning on clears the previous data, generates a new password and starts it (no change if it already runs); turning off stops the process. Returns the status; 400 `EXPERIENCE_INVALID` if the value is missing or not Boolean, 409 `EXPERIENCE_PORT_IN_USE` (`params.port`) if another program holds the port, 409 `EXPERIENCE_FAILED` (`params.reason`) if it cannot start for another reason | JWT(admin) |
| POST | `/api/v1/experience/reset` | Clears the running environment's data and starts it again, keeping its password; 409 `EXPERIENCE_FAILED` if it is not running | JWT(admin) |
| POST | `/api/v1/experience/password` | Generates a new password for the running environment; sessions of the old one end. 409 `EXPERIENCE_FAILED` if it is not running | JWT(admin) |
| GET | `/api/v1/notices` | Header notification bell `{notices: [{id, severity, kind, title?, body?, url?, published_at, read}], unread}`: publisher-signed notices applicable to this installation (`kind` is `security` / `release` / `event`; `title`/`body` provide plain text by language). Sorted by urgency, then by time; `read` and `unread` are calculated per caller | JWT |
| POST | `/api/v1/notices/read` | Mark notices as read with `{"ids": [...]}` (1–128 IDs, each ≤64 bytes; otherwise, 400 `NOTICE_IDS_INVALID`). Records only the caller's own read status; repeated marking has no side effects. Returns 204 | JWT |
| POST | `/api/v1/updates/server` | Download a release package, back up and replace the server, and restart. On acceptance, returns 202 `{from,to,backup}`; confirms or rolls back on the next startup. See [Server Upgrade Restrictions](#服务端升级限制) | JWT(admin) |
| POST | `/api/v1/updates/server/upload` | Same as above, but the request body is an uploaded release archive (512 MiB limit). Returns 400 `SERVER_UPGRADE_ARCHIVE_INVALID` if it is not a usable release package (including versions predating console upgrades: they cannot confirm or roll back their own startup, so `detail` advises using `gpuworks upgrade --archive` instead) | JWT(admin) |
| GET | `/api/v1/settings` | Current values of all settings (excluding `telemetry_export` and `branding`, which are read and written only through their dedicated endpoints) | JWT |
| PUT | `/api/v1/settings/{key}` | Modify a single setting; `telemetry_export`, `branding`, and `security` return 400 `SETTING_HAS_OWN_ROUTE` (`security` goes through `PATCH /api/v1/security/settings`, since a whole-object replace would silently clear switches the body does not name). Only administrators can modify `deployment` (pending scheduling timeout, instance startup timeout, and event retention; otherwise, 403 `ADMINISTRATOR_REQUIRED`). The request body overlays the current values and changes only the keys it contains. If the merged values are out of range, returns 400 `FIELD_INVALID` without writing | JWT(admin) |
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

- 409 `SERVER_UPGRADE_UNAVAILABLE`: self-upgrade is unavailable; `reason` is `no_service_manager`, `not_installed`, `not_installed_binary`, or `not_writable`.
- 409 `SERVER_ALREADY_AT_RELEASE`: already at the target version; 409 `SERVER_NEWER_THAN_RELEASE`: the current build is newer than the channel version.
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
Records with `code` carry detailed values in `params` and no longer also return the raw `data`; historical records from before the code mechanism still include `data`.


| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/metrics/query` | Query time-series metrics (`metric`, `range`, `step`, and `node`/`gpu`/`gpu_uuid`/`instance`/`benchmark`/`point`/`iface`/`hca`/`role` matchers). Returns `{"items": [{"labels": {…}, "points": [[unix_ms, value], …]}], "total"}`, with one series per label combination. If no data exists and the metric name is absent from the metric catalog (`GET /api/v1/monitor/metrics`), returns 400 `METRIC_UNKNOWN`; `params.similar` provides the closest registered names. A metric in the catalog with no data in the time window returns an empty result normally | JWT |
| GET | `/api/v1/logs/search` | Search/tail logs (`q`, `instance` prefix, `instances` (a comma-separated set of exact UUID matches, up to 64, mutually exclusive with `instance`), `node`, `level`, `source`, `from`/`to`, `cursor_ts`/`cursor_id`, `limit`, `with_total`; paginated in reverse arrival order. An omitted time boundary leaves that direction unrestricted; when both are omitted, queries all logs that have not been cleaned up. The list and total use the same range; by default, the page explicitly passes the last 24h). Returns `{"items": [{"id", "instance", "node", "level", "source", "time_ms", "line"}], "total", "total_capped", "truncated"}` (the last three fields appear as needed) | JWT |
| GET | `/api/v1/logs/context` | Context window around a single match (`instance`, `ts`, `id`, and `before`/`after` from 0–1000), equivalent to grep -C. Returns `{"items": [log entries], "match_index"}`; `match_index` is the anchor row's index in `items`, or `-1` when the anchor is absent from that partition (already pruned by retention, or a `ts` from a different day than the `id`), in which case `items` still holds the real rows around that position and the endpoint still returns 200. Callers must check `match_index >= 0` before indexing, or only compare against it | JWT |
| GET | `/api/v1/logs/histogram` | Counts by time bucket × level (same filters as search, including `instances`, plus `bucket` in milliseconds, 1s–24h). Returns `{"items": [{"start_ms", "counts": {"error": N, …}}], "total"}`; buckets align to UTC | JWT |
| GET | `/api/v1/monitor/overview` | Cluster snapshot for the monitoring center (`range` ∈ `15m\|1h\|6h\|24h\|168h`, default `1h`, or a custom `from`+`to` range as described below; signal rows report only facts—values, details, and recent trends, without severity; service table includes each deployment's `health` check, sorted by health: failed, timed out, no available replicas, checking, healthy, disabled, then by name within each category; GPU memory bar, cluster tok/s, and capacity events) | JWT |
| GET | `/api/v1/monitor/deployments/{id}` | Service monitoring snapshot; `range` is `15m/1h/6h/24h/168h` (default `1h`), or use `from`+`to`. Includes requests, latency, replicas, `health`, `log_instances` for log queries, and metric `sources` (engine/gateway); PD also includes fallback, Prefill statistics, and `kv_transfer` under `pd`. Unknown deployments return 404; deleted deployments return 404 `MONITOR_DEPLOYMENT_DELETED` with `deployment_id`, `name`, and `served_name` in `params`. See [Service Monitoring Response](#服务监控返回值) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/requests` | Per-request view: `view=live` (default) or `history`; `limit` is 1–500 (default 50 for live), with optional `key` and `instance` filters. History also supports `range` or paired `from`+`to` (Unix milliseconds), `cursor`, and `min_ms/max_ms`; `results` accepts `completed/length/client_closed/interrupted/error`, with multiple values separated by commas. Records include `result/segments/pd/trace_id`; paginate with `next_cursor`. See [Per-Request Queries](#逐请求查询) | JWT |
| GET | `/api/v1/monitor/requests/lookup` | Find finished requests across deployments; specify exactly one of `trace_id` (32 hexadecimal characters) or `request_id`. Optional paired `from`+`to` (Unix milliseconds, up to 24 hours), and `limit` of 1–500 (default 100); returns `requests/deployments/window`. Without a time range, locates requests by the gateway trace time or searches the last hour; requests discarded by sampling cannot be found. See [Find Requests Across Deployments](#跨部署查找请求) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/latency` | End-to-end latency, stage durations, and generation speeds for complete responses within `range`: `requests` is the count; `end_to_end` and `segments` include `mean_ms/p50_ms/p95_ms`; `speeds` includes `prefill_tok_s/thinking_tok_s/answering_tok_s/decode_tok_s`. Unaffected by log sampling. See [Latency Aggregation Rules](#延迟汇总口径) | JWT |
| GET | `/api/v1/monitor/distributions` | Distribution of requests that finish within `range`: `{x, y, n}` for two grids, `input_output` (input bucket × output bucket) and `uncached_ttft` (uncached input bucket × time-to-first-token bucket); `token_buckets` gives bucket lower bounds and `ttft_bounds_ms` gives time-to-first-token bucket upper bounds. `coverage` gives the total request count, count with usage, and count reporting cache hits; `deployment` is optional and defaults to the entire cluster; unknown deployments return 404 | JWT |
| GET | `/api/v1/monitor/metrics` | Metric catalog; returns `{items:[{series,name,source,kind,unit,quantiles,labels,description}],total}`. `source` is `engine`, `gateway`, `gpu`, `dgpu`, `node`, `network`, `kv_disk`, or `benchmark`; inference histograms expand into quantile gauges (`quantiles:true`), `_sum`, and `_count`. See the [Inference Metric Glossary](inference-metrics.md) for field definitions | JWT |
| GET | `/api/v1/monitor/series` | Metric browser (`metric` required; `range` ∈ `15m\|1h\|6h\|24h\|168h`, default `1h`, or `from`+`to` (`step` defaults to approximately 240 points, in multiples of 10 seconds); `by` ∈ `node\|deployment\|instance\|gpu`; `fn` ∈ `avg\|sum\|max\|rate`, default `avg`; `step` defaults to a value derived from `range` and cannot be lower than that value; returns 400 if results exceed the series/point limits; unknown metrics return `[]`). Series with a `node` label also carry a display-only `node_name`, and series with a `deployment` label carry `deployment_name`; for a deleted node or deployment these are the names it had when deleted, with `node_deleted` / `deployment_deleted` set to `"true"`; labels neither side recognizes are left unchanged | JWT |

Custom time ranges for monitoring charts: `overview`, `deployments/{id}`, `deployments/{id}/latency`, `distributions`, and `series` all accept paired `from`+`to` values (Unix milliseconds), mutually exclusive with `range`. The interval must be between 1 minute and 30 days, and `to` cannot be in the future; otherwise, returns 400 `MONITOR_RANGE_INVALID` (`detail`). Charts, distributions, latency breakdowns, and signals are calculated for the interval ending at `to`, without caching. Current state, including service lists, replicas, and health checks, still reflects the present.

### Service Monitoring Response

`GET /api/v1/monitor/deployments/{id}` returns request reconciliation, a time-to-first-token breakdown, charts, and replica information.
`range` supports `15m`, `1h` (default), `6h`, `24h`, and `168h`; alternatively, use paired `from`+`to` values. Unknown deployments return 404; deleted deployments return 404 `MONITOR_DEPLOYMENT_DELETED` (`params`: `deployment_id`, `name`, `served_name`).

- `log_instances`: UUIDs of live replicas and replicas recently stopped within the window, up to 64; usable with `/logs/search?instances=...`.
- `sources`: indicates whether `ttft`, `e2e`, `prompt_tokens`, `generation_tokens`, and `running` come from `engine` or `gateway`; signals without data are omitted. See [Inference Metrics](inference-metrics.md#网关指标) for definitions.
- `health`: same as the [health check](#健康检查) in deployment details.

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
History also supports end-to-end duration filters `min_ms` and `max_ms`, and comma-separated `results`:
`completed` for normal completion, `length` for length truncation, `client_closed` for client exit, `interrupted` for connection interruption, and `error` for errors.
Unknown deployments return 404; invalid parameters or cursors return 400.

Key fields in each record mean the following:

- `result` is the request outcome; `segments[{kind,ms}]` gives stage durations, which sum to the end-to-end duration or elapsed in-flight duration.
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
| GET | `/api/v1/analytics/usage` | Token usage. Filters: `deployment` (deployment ID), `key` (API key ID; `none` for requests without a key), and `source` (`api`, `playground`, or `batch`, the requests a batch sends). `totals` and `series[]` contain counts: `requests`, `no_usage` (requests without usage), `prompt_tokens` (total input), `completion_tokens`, `reasoning_tokens` (reported by `reasoning_reported` requests), and `cached_tokens` (cache hits reported by `cache_reported` requests; hit rate = `cached_tokens` ÷ `cache_reported_prompt`). `rows[]` has one row per key × user × deployment × source, with `api_key_name`/`api_key_prefix`/`api_key_revoked`/`api_key_deleted` (keys deleted before revocation began retaining records), `user_name`, `deployment_name`/`served_name`/`deployment_deleted`, `source`, and `first_ms`/`last_ms` (first and last buckets used) | JWT |
| GET | `/api/v1/analytics/quality` | Service quality; optional `deployment` filter. `totals`, `series[]`, and `rows[]` (by deployment) include result counts (`completed`, `length`, `client_closed`, `interrupted`, `error`), `ttft_p50_ms`/`ttft_p95_ms` (time to first token for complete streaming responses), `e2e_p50_ms`/`e2e_p95_ms` (end-to-end time for complete responses), and `output_tokens_per_sec`. Quantiles are reconstructed from quarter-octave buckets, with approximately 10% error; missing values are `null` | JWT |
| GET | `/api/v1/analytics/gpus` | GPU utilization. `group_by` is `model` (default), `node`, or `gpu` and determines table and heatmap rows. `metric` is `util` (default), `power`, `temp`, or `mem` and selects the heatmap value, averaged within each bucket. Filters: repeatable `label=key=value` (same matching as the node list, against current user and system labels; deleted nodes no longer match) and `model` (GPU model). `card_count` and `by_model[]{model, cards}` give the current cards covered by the filters. With at most 256 cards, `detail: true` and `cards[]` has one row per card: `node_name`, `node_deleted` (the node is deleted; `node_name` is the name it had when deleted), `gpu_index`, `gpu_uuid`, `gpu_model`, `present` (still in the inventory, including when the node is offline), `deployments` (currently using it), `util_avg`/`util_max`, `mem_used_avg_bytes`/`mem_used_max_bytes`/`mem_total_bytes`, `power_avg_watts`, and `temp_avg_celsius`/`temp_max_celsius`. Above 256 cards, `detail: false` and `cards` is empty. Cards of nodes deleted within the time range are listed too. When grouped by model or node, `groups[]` has one row per group: `key`, `label`, `node_deleted` (when grouped by node, a deleted node; its `cards` and `nodes` are 0), `cards`, `nodes`, `models[]`, `deployments[]{name, cards}`, `unassigned` (cards not allocated to a deployment), and the same statistics, taken from node/model aggregates. `heat[]` contains `{key, label, node_deleted, values[]}`; when grouping more than 100 nodes, it includes only the 50 highest and 50 lowest nodes for the selected metric, while `heat_total` gives the total row count. Grouping by card with more than 256 cards returns 400 `ANALYTICS_TOO_MANY_CARDS` | JWT |
| GET | `/api/v1/analytics/gpu-time` | GPU-hours; optional `deployment` filter. `rows[]` has one row per deployment × GPU model: `gpu_hours` (dGPU usage weighted by its memory share), `whole_gpu_hours`, `avg_instances` (average replica count up to now), `max_instances`, `prompt_tokens`/`completion_tokens` for the deployment in the same period, and `gpu_hours_per_million_output`/`gpu_hours_per_million_tokens` (`null` without token data). `series[]` gives each deployment's GPU-hours per bucket. `capacity[]` gives each GPU model's `total_gpu_hours` (platform-usable cards on online nodes), `schedulable_gpu_hours`, and `allocated_gpu_hours` | JWT |
| GET | `/api/v1/analytics/key-usage` | Counts for each API key with usage today and in the last 7 days, including today: `{items:[{prefix, today, last_7_days}], time_zone}`. Used by the API Tokens page | JWT |

## AI Helper (AI Assistant)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/ai/configs` | List AI Helper LLM connection configurations | JWT |
| POST | `/api/v1/ai/configs` | Add an LLM connection configuration. `base_url` is filled up to `/v1`: trailing slashes and a pasted `/chat/completions` are dropped before storing, and an address that is not http(s) or has no host returns 400 `FIELD_INVALID` with `params.field` `base_url`. Without a `base_url`, a known `preset` supplies one; with neither, the configuration is still stored and the AI Helper reports it as unusable | JWT |
| PATCH | `/api/v1/ai/configs/{id}` | Partially update an LLM connection configuration: only the `name`/`preset`/`base_url`/`model`/`is_default` in the request change. A missing `api_key` or an empty string keeps the stored key (including a legacy plaintext one); a non-empty one is encrypted and stored. Setting the default clears it on the others. Answers the same row the list shows (six fields, no key). 404 for a missing configuration, 400 `FIELD_INVALID` for an invalid value | JWT |
| DELETE | `/api/v1/ai/configs/{id}` | Delete an LLM connection configuration | JWT |
| POST | `/api/v1/ai/configs/test` | Try the unsaved `base_url`/`api_key`/`model` a form holds, storing nothing. A 10 second timeout, through the same outbound policy the assistant uses. Answers `{outcome, status, detail, base_url, latency_ms}`, where `outcome` separates `ok`, `connection` (DNS, dial or timeout), `blocked` (refused by the outbound policy), `auth` (401/403), `path` (a 404 that names no model: the wrong address), `model` (the endpoint does not serve that model) and `http` (any other status, or not a completion). `detail` is the endpoint's own words, shortened and with the key removed. An edit dialog sends its `id` with an empty `api_key` to reuse the stored key, only while `base_url` still matches the stored address | JWT |
| GET | `/api/v1/ai/available` | Whether AI Helper is available (whether an LLM is configured) | JWT |
| POST | `/api/v1/ai/ask` | Ask AI Helper to perform a task; supports regular responses and SSE. See [AI Ask Request and Response](#ai-提问的请求与响应) for all fields and conditions | JWT |
| GET | `/api/v1/ai/instances/{uuid}/findings` | Known failures matched in a failed instance's logs: `{items:[{code, evidence:[{line}]}], total}`; `code` is stable, and the frontend renders the text. No LLM required; returns 404 if the instance does not exist | JWT |
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
| `ask` | Requires `question`; optionally include one of the targets above |

Specify targets by reference; the server assembles the context. `question` is optional except for the `ask` task, where it is required. For follow-up questions, include `history:[{role:user|assistant,content}]`, keeping at most the latest 10 rounds.

After assembling the context, the server runs an agent loop with read-only tools. The tools read this API's GET endpoints in-process as the user asking the question; see [Use AI Helper](../how-to/use-ai-helper.md). A regular response is `{answer, steps:[{tool,args,ms,error?}], calls, usage:{prompt_tokens,completion_tokens}}`.

With the `Accept: text/event-stream` request header, the response uses SSE streaming:

- `step`: `{id,tool,args,status:running|done|error,ms,error?}`, one event when each tool call starts and one when it ends.
- `text` / `reasoning`: `{delta}`.
- The final event is `done` (`{calls,usage,first_text_ms,total_ms}`) or `error` (`{error}`).
- When idle, sends a `: ping` comment every 10s.

Failures before the model call, such as an unconfigured LLM or a nonexistent target, still return HTTP status codes. Error conditions are:

| Condition | Response |
|---|---|
| No LLM configured | 503 `AI_ENDPOINT_NOT_CONFIGURED` |
| Invalid `task` | 400 `AI_TASK_INVALID` |
| Missing target or multiple targets provided | 400 `AI_TARGET_REQUIRED` |
| The same user already has 2 questions being processed | 429 `AI_ASK_BUSY`; `params.limit` gives the limit |
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
| GET | `/api/v1/playground/models` | List deployed models callable in Playground, excluding external AI Helper models in global settings; `html_preview.allow_network` is the HTML preview network policy (from security settings); each model has `sleeping` (boolean), and a sleeping model whose sleep a request ends (the deployment is sleepable and one of its sleeping instances is not held by an operator or a schedule) also has `wakes_on_request: true` (omitted otherwise): a request to such a model is held by the gateway and let through once it wakes, while a request to any other sleeping model answers 503 until it is woken; the Playground shows its notice from this | JWT |
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
| GET | `/api/v1/batches/dictionary` | The dictionary for creating a batch: `endpoints`, `completion_window_hours` (`min`/`max`/`default`), `max_file_bytes`, `max_file_lines`, `statuses` (in the order a list filters them), `output_retention_days`, and `models`: the served model names of the deployments not being deleted (stopped ones included) with their state and supported endpoints `{name, state, ready, endpoints}`. `ready` is computed by the server and is true only while serving or when a request can wake the model automatically. The console defaults to these choices; users can explicitly reveal models that require waiting. `state` matches the deployment list and, ordered by how soon the model can serve, is `running` (a replica is awake), `asleep` (asleep, a request wakes it), `starting` (being placed, pulling or starting), `asleep_held` (asleep by hand or by a schedule, a request does not wake it), `offline` (the replica's node is disconnected), `failed` (failed to start) or `stopped` (stopped); the list is sorted in this order and then by name. `endpoints` are the batch endpoints the model's inference API declaration opens (the gateway lets requests through by the same declaration). It is the same judgement as the model check at creation; a name several deployments share takes the state of the best of them and the union of their endpoints. The console's create dialog takes all its choices from here, and a client works none of them out itself | JWT |
| GET | `/api/v1/batches/{id}` | Batch details, including `input_filename`; a missing batch, or one that is not yours (administrators excepted), returns 404 `BATCH_NOT_FOUND` | JWT |
| GET | `/api/v1/batches/{id}/files/{kind}` | Download a batch file; `kind` is `input`, `output`, or `error`; the response is JSON Lines (`application/x-ndjson`, with an attachment filename); an invalid `kind` returns 400 `BATCH_REQUEST_INVALID`; a missing or deleted file returns 404 `BATCH_NOT_FOUND` | JWT |
| POST | `/api/v1/batches` | Create a batch: `multipart/form-data` with the field `file` (JSONL) and optionally `endpoint` (when absent, the `url` all lines share; lines with different `url`s return 400 `BATCH_FILE_INVALID` with `reason` `mixed_endpoints` and `line` the first line that differs from the first), `completion_window` (a whole number of hours from `1h` to `72h`, `24h` by default; an invalid one returns 400 `BATCH_REQUEST_INVALID` with `params.field` `completion_window`) and `model` (when set, the `body.model` of every request is replaced by it; it must be the served model name of a deployment not being deleted, otherwise 400 `BATCH_MODEL_UNKNOWN`; 400 `BATCH_MODEL_ENDPOINT` when its deployment does not support the batch's endpoint); the file is validated line by line on upload, and success returns 201 and the batch object, with `warnings` when the chosen model (without `model`, the models of the file's lines) needs someone to act: a sleep by an operator's order (a request does not wake it) is `BATCH_MODEL_SLEEP_HELD` or `BATCH_FILE_MODELS_SLEEP_HELD`, and an offline node, a failed start or a stopped deployment is `BATCH_MODEL_WAITS` or `BATCH_FILE_MODELS_WAIT`. Without `model`, when the file names models that no deployment serves, it returns 400 `BATCH_FILE_MODELS_UNKNOWN` (`params` is `{models, more}`), and when some model does not support the batch's endpoint, 400 `BATCH_FILE_MODELS_ENDPOINT` (`params` is `{models, more, endpoint}`); in both cases no batch is created. The batch belongs to the signed-in user | JWT(admin) |
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
- Agent-side yamux subprotocols (`ctrl`/`metrics`/`logtail`/`tunnel`/`model.read`/`model.write`/
  `image.read`/`image.write`/`terminal`) are not HTTP routes. They belong to the `internal/proto` control plane protocol and are outside this document's scope.

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

### Whether GPU capacity is known

Node GPU and `/api/v1/gpus` physical-card DTOs both return the boolean `memory_capacity_known`. It is derived from inventory capacity and not persisted. When false, `memory_total_bytes` must not render as a known zero-GB capacity. Measured zero usage remains a valid metric. Only the compatibility boundary for old APIs without the field uses the legacy zero-capacity sentinel.

An unknown `target_vendor` on `/api/v1/engines` returns 400. If custom-command features cannot be determined and the target declares launch-feature restrictions, evaluation/creation returns `LAUNCH_FEATURES_UNKNOWN` with parameter `vendor`; the scheduler uses `launch_features_unknown`. Re-evaluate with a recognized engine entrypoint and structured parameters.
