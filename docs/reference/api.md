# REST API 全量路由

本文列出管理 GPUWorks 资源的 HTTP 接口，包括节点、模型、部署与设置。管理接口以 `/api/v1` 为前缀；
鉴权与响应约定统一列在开头，接口按资源类型查阅。路径中的 `{id}`、`{uuid}` 等须替换为实际资源标识。

第一次调用可先看[登录并查询推理服务列表](api-conventions.md#示例登录并查询推理服务列表)，其中包含地址、登录、请求和响应示例。

调用模型推理使用另一组接口：`/v1/*` 是对外的 OpenAI 兼容网关，`/api/v1/playground/v1/*` 是
Playground 内部代理。它们的调用规则见 [OpenAI 兼容网关](openai-gateway.md)，本文不重复列出。
批处理的 OpenAI 兼容接口（`/v1/files`、`/v1/batches`）例外：它们与管理接口的批处理路由一起列在 [Batches](#batches批处理任务) 一节。

## 鉴权模型

### 先区分凭证的用途

鉴权首先确认“谁在请求”，再检查“这个身份能否执行当前操作”。应按访问目的选择凭证；管理、节点接入和推理凭证不能相互替代：

| 凭证 | 代表谁、用在哪里 | 如何携带 |
|---|---|---|
| 管理登录会话 | 已登录的用户，用于管理 API 和控制台 | 浏览器使用 `gpuworks_session` Cookie；程序取登录响应的 `token`，放入请求头 `Authorization: Bearer <JWT>` |
| 平台 API 令牌 | 令牌所属用户，用于程序访问管理 API | `Authorization: Bearer gwp_…`；权限还受令牌范围限制 |
| 节点 token | 某个已登记的节点，用于 agent 接入 | `GET /api/v1/agent/connect` 的 hub 握手（WebSocket 升级后建立 yamux 连接） |
| 推理 API key | 推理调用方，用于 `/v1/*` 网关 | 见 [OpenAI 兼容网关](openai-gateway.md)；不用于登录管理 API |

管理登录会话的 Cookie 与 JWT 是同一次登录的两种凭证，均须关联服务端仍有效的会话。
凭证缺失、无效或会话失效时，返回 `401 AUTHENTICATION_REQUIRED`。登录与失效规则见下文 [Auth](#auth)。

### 路由表的“鉴权”列

| 标记 | 调用要求 |
|---|---|
| `JWT` | 有效的管理身份即可；可使用登录会话，或在允许的范围内使用平台 API 令牌 |
| `JWT(admin)`、`admin` | 除管理鉴权外，当前用户角色还必须是 `admin`；否则返回 `403 ADMINISTRATOR_REQUIRED` |
| 管理会话 | 只接受登录后的 Cookie 或 JWT，不接受平台 API 令牌 |
| `无` | 不校验管理 JWT；接口自身仍可能有来源检查等约束 |

`JWT` 是表中的历史简写，不表示只能通过 Bearer JWT 调用。节点接入等采用专用凭证的路由，会在表中单独标明。

### 平台 API 令牌的额外限制

平台令牌按所属用户的**当前角色**执行，再受自身 `read` 或 `write` 范围限制。
例如，普通用户的 `write` 令牌也不能执行管理员专属操作。创建与撤销方式见 [Platform Tokens](#platform-tokens平台-api-令牌)。

| 条件 | 行为或错误 |
|---|---|
| 使用 `read` 令牌 | 只读范围校验允许 GET、HEAD、OPTIONS，以及只读工具入口 `POST /api/v1/mcp`；其他写请求返回 `403 PLATFORM_TOKEN_READ_ONLY`。接口是否支持该 HTTP 方法仍按具体路由判断 |
| 使用任何平台令牌访问 `/api/v1/platform-tokens` 或 `auth/activity` | 返回 `403 PLATFORM_TOKEN_NOT_ALLOWED`；这些操作只能在登录会话中执行 |
| 令牌未知、已过期，或所属用户已删除 | 返回 `401 AUTHENTICATION_REQUIRED` |

### 无需登录的安装与探活入口

以下入口不在 `/api/v1` 下，公开访问：

| 路径（均为 GET） | 用途 |
|---|---|
| `/install.sh` | agent 安装脚本 |
| `/healthz` | 探活与负载均衡健康检查；管理面 `/api/v1/healthz` 仍需鉴权 |
| `/dl/gpuworks-agent-<os>-<arch>` | 下载指定操作系统与架构的 agent 二进制 |
| `/dl/gpuworks-agent.run` | agent 离线安装包 |
| `/dl/gpuworks-agent.run.sha256` | 离线包的校验和，使用 `sha256sum` 格式 |

下载由节点接入功能的监听地址提供。离线包是自解压 sh 脚本，包含 linux-amd64、linux-arm64、darwin-arm64
的 agent 和 `install.sh`，不含节点 token。server 没有内嵌任何 agent 时，离线包返回 404。

## 错误与响应约定

### JSON 字段与列表结构

JSON 字段按 [API 规范](api-conventions.md) 使用 snake_case，例如 `node_uuid`。
返回一组对象时，元素放在 `items` 字段内，不直接返回数组。`total` 表示筛选后的总命中数：
不分页时等于 `items` 的长度；分页时可能大于本页条数。

| 列表形式 | 响应结构与含义 |
|---|---|
| 不分页 | `{"items": [...], "total": N}`，返回全部命中对象 |
| 按页码分页 | 另有 `page` 与 `page_size`，分别表示当前页码与请求的每页条数 |
| 按游标分页 | 返回 `{"items": [...]}`；游标标记上次读到的位置，总数是否返回由接口决定 |

### 错误响应怎么读

先检查 HTTP 状态，再读 `error`（错误说明）、`code`（稳定错误码）与 `params`（相关取值）。
普通错误可能只有 `error`，不能假定都有 `code` 或 `params`。
示例与通用处理见 [HTTP API 规范：错误](api-conventions.md#错误)；下文只列接口特有的限制与错误。

### 节点与部署：页码、筛选和排序

以下规则适用于 `GET /api/v1/nodes` 和 `GET /api/v1/deployments`。两者均需管理鉴权，无请求体；
以下参数放在 URL 查询字符串中，均可省略：

| 参数 | 类型 | 默认值与用法 |
|---|---|---|
| `page` | 整数 | 页码从 1 开始；启用分页但省略本参数时取 1 |
| `page_size` | 整数 | 每页 1–200 条；启用分页时默认 50 |
| `q` | 字符串 | 省略时不按名称筛选；不区分大小写的子串匹配，具体匹配字段见下方对应资源 |
| `state` | 字符串 | 省略时不限状态；多个值以逗号分隔，满足任意一个即可 |
| `sort` | 字符串 | 省略或为空时最新优先；`sort=<列>` 升序，`sort=-<列>` 降序，可选列见下方对应资源 |

只要带 `page` 或 `page_size` 就分页，响应包含 `items`、`page`、`page_size`、`total`。
两个参数都不带时，返回全部命中的对象，便于脚本与程序化调用；筛选与排序仍然生效。
处理顺序是**先筛选全部对象，再排序，最后取当前页**，排序值相同的对象保持最新优先。

非法页码或页大小返回 `400 LIST_PAGE_INVALID`，不会静默改成默认值。
未知筛选值或排序列返回 `400 LIST_FILTER_INVALID`。

`GET /api/v1/inference-templates` 使用同一套页码校验与响应结构，但**始终分页**，默认第 1 页、每页 50 条，
另有自己的硬件与文本筛选：`gpu` 为卡名，`vendor` 可选，`cc` 和 `arch_id` 需要同时提供 `gpu`。这些参数是调用方提供的筛选条件，不代表服务器已检测或验证该硬件。`GET /api/v1/models`、`GET /api/v1/gpus` 也支持分页，参数见各自路由条目。

### 实时事件流与响应压缩

`GET /api/v1/events` 和 `GET /api/v1/instances/{uuid}/logs` 是 SSE，即服务器持续推送的事件流，
使用 `Content-Type: text/event-stream`，不能按一次性 JSON 响应读取。
其中 `/api/v1/events` 只通知哪些资源发生了变化，不携带资源对象；客户端收到通知后重新拉取相应资源。
每个 agent 每分钟上报一次状态，对应 `node` 事件的 `reason` 为 `status`；同一条连接上这类事件每 5 秒最多推送一次：
窗口内第一条立即推送，其余合并为窗口结束时的一条，`key` 为空。节点的其他变化（上下线、调度、删除等）不合并。

常规管理资源接口返回 `Content-Type: application/json` 时，请求带 `Accept-Encoding: gzip` 可启用 gzip 压缩。
事件流、日志流、文件与镜像传输保持原样，WebSocket 升级不受影响。

### 设置变更怎样下发到节点

修改拉取凭据、网络与加载、节点标签、存储（新建、编辑、节点例外）、孤儿容器清理开关或镜像目录时，保存后立即返回，不等节点收到：server 为受影响的每个在线节点安排一次状态下发，各节点各自下发、互不等待，短时间内的多次修改合并下发，下发的是当时的最新状态；镜像目录在第一次变更后约 250 ms 下发，期间的修改合在这一次里。返回成功只表示修改已保存。某个节点暂时收不到时不影响其他节点，它重新连上时会收到完整的最新状态。删除存储仍会等在线节点收到后才返回。

## Auth

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| POST | `/api/v1/auth/login` | 用户名密码登录，创建持久化会话；返回绑定会话的 JWT 并写不透明 Cookie | 无 |
| POST | `/api/v1/auth/logout` | 撤销所提交凭证对应的会话并清除 Cookie；重复退出幂等 | 无 |
| GET | `/api/v1/auth/me` | 返回当前用户与实时权限；不延长空闲期限 | 管理会话或平台 API Token |
| POST | `/api/v1/auth/activity` | 报告真实操作并延长空闲期限；无请求体，成功返回 204 | 管理会话 |
| POST | `/api/v1/auth/password` | 修改当前登录用户自己的密码；成功后该用户的全部管理会话失效 | 管理会话 |

### 登录请求与响应

`POST /api/v1/auth/login` 无需已有登录凭证。请求体使用 JSON，发送 `Content-Type: application/json`；
没有路径参数或查询参数。

| 请求字段 | 类型与要求 | 含义与限制 |
|---|---|---|
| `username` | 字符串，必填 | 用户名；查找用户时去除首尾空白，最多 256 个字符，不得包含控制字符 |
| `password` | 字符串，必填 | 该用户的密码，按原值校验，不去除空白 |

请求体上限为 1 MiB，必须是一个 JSON 对象，不能拼接多个 JSON 值。
成功返回 HTTP 200，并通过 `Set-Cookie` 写入 `gpuworks_session`。响应结构示意：

```json
{
  "token": "<JWT>",
  "user": {"id": 1, "username": "admin", "role": "admin"}
}
```

`token` 是后续请求使用的 JWT；`user` 包含用户 ID、用户名和角色（`admin` 或 `user`）。
完整请求示例见[设置地址并登录](api-conventions.md#1-设置地址并登录)。

`auth/me` 返回当前用户信息和新密码的长度要求，结构为 `{"id":1,"username":"admin","role":"admin","local_admin":true,"password_policy":{"min_length":8,"max_length":128}}`。
`local_admin` 表示当前请求是否享有插件宿主的本地管理员兜底（见[可安装扩展](../dev/extensions.md#本地管理员兜底)）：只有角色为 `admin`、库里标记为本地管理员、且用登录会话访问时才为 `true`；平台 API Token 总是 `false`。它只用于显示。
Cookie 设置为 `HttpOnly`、`SameSite=Lax`，其中保存的是不透明会话凭证，不是 JWT。
请求同时带 Bearer 与 Cookie 时优先使用 Bearer；无效 Bearer 不会回退到 Cookie。

用户名或密码错误返回 `401 INVALID_CREDENTIALS`；登录过于频繁返回 `429 LOGIN_THROTTLED`，
按响应头 `Retry-After` 指定的秒数等待后重试。其他错误见[常见 HTTP 状态与错误码](api-conventions.md#常见-http-状态与错误码)。

### 修改密码

`POST /api/v1/auth/password` 只修改当前登录用户自己的密码，只接受登录会话（平台 API 令牌返回 `403 PLATFORM_TOKEN_NOT_ALLOWED`），并做与登录相同的来源检查。

| 请求字段 | 类型与要求 | 含义 |
|---|---|---|
| `current_password` | 字符串，必填 | 当前密码，按原值校验 |
| `new_password` | 字符串，必填 | 新密码，须满足[管理员密码规则](security.md#管理员密码) |

成功返回 `200 {"status":"password_changed"}`，并清除当前 Cookie：该用户的全部管理会话（含本次）失效，需用新密码重新登录。若数据目录里的 `initial-credentials` 记录的正是该用户，server 尝试删除该文件，删除失败只记告警日志。

两个密码在同一次请求里都会被检查，错误一律返回 400：

| 情况 | 错误码 | 说明 |
|---|---|---|
| 当前密码错误 | `CURRENT_PASSWORD_INVALID` | 新密码同时不合规时，`params.new_password` 给出新密码的错误码 |
| 新密码长度不在范围内 | `PASSWORD_LENGTH` | `params.min`、`params.max` 为长度上下限 |
| 新密码是常见弱口令、整段重复或连续字符 | `PASSWORD_GUESSABLE` | |
| 新密码含控制字符或不是合法 UTF-8 | `PASSWORD_CHARACTERS` | |
| 新密码与当前密码相同 | `PASSWORD_UNCHANGED` | 只在当前密码正确时判断 |

校验当前密码与登录共用限流（按来源地址和用户名计数），过于频繁返回 `429 LOGIN_THROTTLED` 并带 `Retry-After`。

### 会话什么时候失效

会话默认在 24 小时无真实操作后失效，从登录起最长保留 7 天；Cookie 的过期时间按后一个绝对期限设置。
两项期限可配置，见[管理会话配置](configuration.md#管理会话)。登录响应中的 JWT 自身有效期为 24 小时，
且必须同时关联有效会话；不能仅凭 JWT 尚未过期认定登录仍有效。

| 情况 | 处理结果 |
|---|---|
| 调用 `auth/me` 或被动轮询 | 不延长空闲期限 |
| 通过登录会话执行管理写请求，或调用 `auth/activity` 报告真实操作 | 延长空闲期限，但不延长从登录起计算的绝对期限 |
| 修改或重置密码（控制台或 `reset-password`） | 撤销该用户的管理会话，关联的 JWT 同时失效；不撤销平台 API 令牌和 API key |
| 退出登录 | 撤销所提交凭证对应的会话，关联的 JWT 同时失效，并清除 Cookie；重复退出仍可成功 |
| 升级前未绑定会话的 JWT 或旧 Cookie | 需要重新登录 |

平台 API 令牌按自己的范围和有效期独立鉴权，不通过 `auth/activity` 延期。

### 登录接口的错误与来源检查

会话失效返回 `401 AUTHENTICATION_REQUIRED`；会话存储不可用返回 503。
登录、退出，以及使用登录会话的管理写请求会检查请求来源；跨来源请求返回 `403 MANAGEMENT_ORIGIN_REJECTED`。

## Nodes（节点接入）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/agent/connect` | agent 长连接 WS upgrade 入口 | 节点 token（非管理 JWT） |
| GET | `/install.sh` | curl\|sh 安装脚本 | 无 |
| GET | `/api/v1/healthz` | 健康检查（管理面） | JWT |
| GET | `/api/v1/readyz` | 业务就绪检查（无需登录，供负载均衡与集群入口探测）：就绪时 200，正文固定为 `{"status":"ready"}`；业务库不可读或成员尚未开放业务时 503；响应不含任何配置、身份或计数 | 无 |
| GET | `/api/v1/peer-transfers` | 当前管理节点进程的 peer 传输观测；可选单个 `transfer_id`，返回有效中继预算、活动中继数、带宽额度等待数、累计结果及有界运行观测。无对应观测返回空列表，未接入观测能力返回 503；非法筛选返回 400。计数为内层 TLS 流字节，不代表文件校验进度。 | JWT |
| GET | `/healthz` | 健康检查（无需登录，供探活/负载均衡） | 无 |
| GET | `/api/v1/engines?target_vendor=amd` | 面向指定厂商的引擎参数字典（省略 `target_vendor` 时使用通用 profile，未知厂商返回 400）；AMD profile 只表示实现准入范围，不证明特定 GPU/ROCm 版本已通过真机验收 | JWT |
| GET | `/api/v1/accelerator-vendors` | 加速卡厂商字典，由各厂商的定义给出，与集群里有哪些节点无关：每项含 `id`、`brand`、`frameworks`（归属该厂商的镜像框架）、`runtimes`（卡可承载负载的运行时驱动，空表示只发现和监控）、`features`（卡上提供的平台功能）、可选的 `partition_backend`（dGPU 后端）。控制台据此得到厂商的顺序、名称与框架归属 | JWT |
| GET | `/api/v1/nodes` | 节点列表；带 `page`/`page_size` 时分页并可按 `q`/`state`/`label` 筛选（见“错误与响应约定”）；`source=` 按来源筛选，逗号分隔的几个值任一满足即可：`provider:<外部平台账号 id>` 是经该账号接入的节点（装在它的实例上，或经由它接入的集群），`cluster:<集群条目 uuid>` 是经该集群接入的节点，`manual` 是手动添加的节点；不存在的账号或集群匹配不到任何节点，格式不对的值回 400 `LIST_FILTER_INVALID`；分页响应另带 `sources`：全部节点（不受筛选影响）有哪些来源可选，每项 `{value,provider_name?,provider_kind?,cluster_name?,nodes}`，`value` 即上面的筛选值，账号在前、集群其次、`manual` 最后，账号的 `nodes` 含它的集群的节点；每项的 `source` 是节点经由的外部平台 `{resource,provider_id?,provider_name?,provider_kind?,cluster_uuid?,cluster_name?}`（`resource` 为 `vm`、`container`、`host`、`kubernetes` 或 `slurm`，判断不出资源类型时（账号的类型能接入几种实例、而实例没有说明自己是哪种，或类型未知）为空字符串，字段仍然出现；自行接入的集群没有 `provider_*`；手动添加的节点没有 `source`）；server 读不到节点来源时，每项改带 `source_unknown: true`、没有 `source`（与手动添加的节点区分；读到时省略该字段），分页响应的 `sources` 为空数组，带 `source=` 筛选时回 500；从 Kubernetes 集群接入的节点带系统标签 `gpuworks.ai/kubernetes-cluster` 与 `gpuworks.ai/kubernetes-node`（见“节点标签：写入的与生成的”），被 Kubernetes cordon 时 `cordon_reason` 为 `kube_unschedulable` | JWT |
| GET | `/api/v1/nodes/facets` | 筛选项及节点数量分布；查询参数 `q`、`state`、`label` 同节点列表，返回用户标签、系统标签与卡型号分布（含已报告的 `arch_id`），见[节点列表](#节点列表的筛选与返回字段) | JWT |
| GET | `/api/v1/dashboard/summary` | 仪表盘汇总：节点按状态计数与硬件合计、部署按状态计数、按厂商与按标签（`label_key`，缺省或无节点带该 key 时取最常用的 key）分组的容量；另含当前 GPU 指标、卡占用、在途任务，字段见[仪表盘汇总](#仪表盘汇总的返回字段) | JWT |
| POST | `/api/v1/nodes` | 创建待接入节点，返回安装命令与 token；完整字段与条件见[创建节点的连接参数](#创建节点的连接参数) | JWT(admin) |
| GET | `/api/v1/agent/rejects` | 最近被拒绝的连接尝试 | JWT |
| GET | `/api/v1/agent/upgrade-errors` | 最近 agent 升级失败记录 | JWT |
| POST | `/api/v1/agent-upgrades/preview` | 预览全平台或指定节点的升级资格与目标版本 | JWT(admin) |
| POST | `/api/v1/agent-upgrades` | 创建升级批次，提交预览的 UUID 快照、目标和 manifest 指纹及幂等键 | JWT(admin) |
| GET | `/api/v1/agent-upgrades/summary` | 读取运行中 Server 的 Agent 目标版本、各组计数及活动批次 ID。可执行集合 `align` 为 `upgrade`（较低）、`downgrade`（较高）、`different_build`（先后未知）之和。`check` 统计暂不可升级的在线 Agent，`offline` 单独统计离线 Agent。节点分页支持以 `align`、三个可执行分组、`check` 或 `all` 作为 `agent_upgrade` 筛选，先筛选再分页，返回全局 `agent_fleet`、行级 `agent_eligibility` 及可执行行的 `agent_version_change` | JWT |
| GET | `/api/v1/agent-upgrades` | 分页列出升级批次与全局阻塞项 | JWT |
| GET | `/api/v1/agent-upgrades/{id}` | 获取批次进度和计数 | JWT |
| GET | `/api/v1/agent-upgrades/{id}/items` | 分页查询逐节点结果，可按 state 筛选 | JWT |
| POST | `/api/v1/agent-upgrades/{id}/stop` | 停止尚未开始的节点，继续观察当前节点 | JWT(admin) |
| POST | `/api/v1/agent-upgrades/{id}/continue-preview` | 重新预览暂停批次的剩余节点 | JWT(admin) |
| POST | `/api/v1/agent-upgrades/{id}/items/{uuid}/release` | 管理员记录核实结论，把“结果未知”的节点提前标记为已核实（可选；系统会在节点重新连接时自行判定，批次不再因它暂停） | JWT(admin) |
| POST | `/api/v1/nodes/precheck` | 检查 SSH 接入所需的主机环境与双向连通性；地址规则见[SSH 检查与安装](#ssh-检查与安装) | JWT(admin) |
| POST | `/api/v1/nodes/install` | 经 SSH 安装 agent；连接参数同创建节点，可用 `no_systemd` 选择 Linux 后台运行方式。冲突与替换条件见[SSH 检查与安装](#ssh-检查与安装) | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}` | 节点详情（含 GPU 列表；管理员额外含 token） | JWT |
| GET | `/api/v1/nodes/{uuid}/fs` | 节点文件系统浏览（本地模型注册用） | JWT |
| POST | `/api/v1/nodes/{uuid}/doctor` | 触发节点自检 RPC。`checks[]` 每项含 `stage`、`status`（`pass`/`warn`/`fail`/`skip`/`na`）、`code`（UPPER_SNAKE），agent 的发现另带 `finding`（`id`、`status`、`code`、依据与复核命令）；`summary` 是其中最严重的状态 | JWT |
| POST | `/api/v1/nodes/{uuid}/connection-diagnostics` | 在线经既有ctrl读取Agent运行态，无需SSH；另有保存地址、可选SSH检查及历史公网入口。空body或`{}`，不执行恢复或修改地址；见[节点连接诊断](#节点连接诊断) | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/upgrade` | 触发 agent 远程升级；Kubernetes 节点与普通节点一样原地升级（agent 运行在节点状态目录里，声明 `upgrade.kube_binary`）；没能从状态目录启动的（例如 noexec）不声明它，升级把它标为 `kubernetes` 并跳过 | JWT |
| PUT | `/api/v1/nodes/{uuid}/scheduling` | 设置 `scheduling` 为 `cordoned`、`draining` 或 `schedulable`，返回节点；非法值 400 `FIELD_INVALID`。各状态的影响见[节点调度与加速卡切换](#节点调度与加速卡切换) | JWT |
| PUT | `/api/v1/nodes/{uuid}/gpus/scheduling` | 设置一张卡是否接受新部署：`{"pcie_bdf":"0000:3f:00.0","scheduling":"stopped"}`（没有 PCI 地址的卡用 `uuid`）。`stopped` 禁用调度，`schedulable` 启用调度；两者一直有效，直到管理员手动改变，不随时间、上报或 server 重启失效。健康与平台外占用仍如实展示，未手动设置的卡仍按平台自动判定。返回节点（含卡），卡上 `scheduling_stopped`、`scheduling_allowed` 表示状态，`scheduling_actions` 给出现在可以做的操作；扩展保持的卡另带 `scheduling_held_by`（扩展的来源名）与 `scheduling_hold`（扩展给出的 `{code, params, message}`），见下面「扩展保持的卡」。非法值或没点名卡 400 `FIELD_INVALID`；节点或卡不存在 404；卡已直通或没绑定驱动时设为可调度 409 `CARD_NOT_OWNED`；扩展保持的卡设为可调度 409，`code` 与 `params` 由扩展给出（没有给 code 时为 `CARD_SCHEDULING_HELD`） | JWT |
| GET | `/api/v1/nodes/{uuid}/health-checks` | 节点每张卡每项检查的当前状态，按表格给出（只读 server 内存，不触达 agent）：`checks` 是列（只含有卡适用的检查，带周期秒数），`cards[].cells[]` 是格子（`status` 为 `pass`/`warn`/`fail`/`skip`/`na`，`code` 为发现码或未检查的原因码，如 agent 启动后还没运行过为 `CARD_CHECK_NOT_RUN_YET`；`checked_at`、`duration_ms`、`stale`），`cards[].worst`（按 `fail`、`warn`、`skip`、`pass`、`na` 取最严重的）、`cards[].can_recheck` 与 `summary`（各状态格子数）由 server 计算。老 agent 上报的小写码和旧状态词由 server 读入时换成这一套。`?pcie_bdf=`（或 `uuid=`）只看一张卡。节点还没上报时 `cards` 为空、`reports` 为 false；离线时给出最后已知状态并标 `stale`。可选 `notice` 为 server 计算的 `{code, level}` 提示，`level` 为 `info` 或 `warning`；码为 `CARD_CHECKS_OFFLINE`、`CARD_CHECKS_UNSUPPORTED`、`CARD_CHECKS_WAITING`、`CARD_CHECKS_STALE`、`CARD_CHECKS_PASSTHROUGH`、`CARD_CHECKS_NO_DRIVER`、`CARD_CHECKS_HELD_ELSEWHERE`、`CARD_CHECKS_NO_USABLE_CARDS` 或 `CARD_CHECKS_NO_CARDS`，可正常展示检查时省略。`reports` 仅表示当前快照含宿主机卡的检查，不能用于推断 Agent 能力。节点在线且已收到未过期的报告时，先解释所选卡的设备归属；全部卡不可由宿主机检查时，不因 Agent 缺少能力而提示升级。 | JWT |
| GET | `/api/v1/nodes/{uuid}/health-checks/detail?pcie_bdf=&check=` | 一张卡一项检查最近一次运行的详情（`last`；`last` 是跳过时另给 `last_ran`；更早出过问题时给 `last_problem`；每次运行含 `status`、`code`、`at`、`duration_ms`、`probe` 复核命令、`evidence`），经 RPC 向 agent 取，agent 只读内存、不跑命令。同一格并发请求只发一次，答案复用 5 秒；每节点最多 2 个、全局最多 16 个在途请求，超出 429（带 `Retry-After`）。缺卡或检查名非法 400 `FIELD_INVALID`；节点离线 409 `NODE_OFFLINE`；agent 版本过旧 409 `CARD_CHECKS_UNSUPPORTED`（两者都不发 RPC） | JWT |
| GET | `/api/v1/nodes/{uuid}/health-records` | 节点的失败记录（某项检查告警或失败的一段时间）：进行中的在前，其余新的在前；`?pcie_bdf=`（或 `uuid=`）、`?check=` 筛选，`page`/`page_size` 分页（默认每页 50）。每条含 `card_ident`、仍在节点上的卡的 `card_index`/`card_name`/`pcie_bdf`（已移除的卡 `removed` 为 true）、`check`、`status`（`warn`/`fail`）、`code`、`started_at`、`ended_at`（进行中没有）、`last_seen_at`、`duration_seconds`、`count`、`flapping`、`ongoing`、`first_evidence`、`latest_evidence`；`retention_days` 是保留天数（30） | JWT |
| POST | `/api/v1/nodes/{uuid}/gpus/recheck` | 立即对一张卡做一次运行时检查：`{"pcie_bdf":"0000:3f:00.0"}`（或 `uuid`）。结果像周期检查一样记到卡上，下一次上报卡的状态随之变化；返回同 `health-checks/detail` 的运行时检查详情（卡正在服务、被平台外进程占用或宿主暂时不能检查时 `last.status` 为 `skip`，`last.code` 说明原因）。错误同上 | JWT |
| GET | `/api/v1/nodes/{uuid}/replicas` | 节点上仍在运行的实例与驱逐对它的处理：`{"scheduling", "items": [{"instance", "deployment_id", "deployment", "state", "kind"}]}`，`kind` 为 `pool`（迁到别的节点）、`pinned`（部署只钉在这台，等待）或 `named`（多机、PD 的 rank：驱逐时先在别的节点起替代，运行后切过去，没有节点可去则等待；强制驱逐才立即撤下）；已停止、已失败的实例不列。驱逐时还可返回 `drain_reason`（最近的结构化调度反馈）、`drain_reason_at`、`drain_message`（其他节点最近一次失败尝试的英文信息）与 `drain_failure`（该尝试的结构化失败原因，形状同实例的 `runtime_failure`；旧尝试缺省）。这些是现有观测，不是新的驱逐状态；反馈时间用于辨别历史记录。 驱逐时还可返回顶层 `drain_offline`，表示当前 agent 连接已离线，退役和容器移除尚无法确认；重连后此标记清除。 | JWT |
| POST | `/api/v1/nodes/{uuid}/evict` | 强制驱逐：不等替代副本，把节点上每个副本立即撤下（所在副本的 leader 进入排空，follower 随之），副本在能放的地方重建，不计入重试次数；返回 `{"evicted": [leader UUID…]}`（202）。节点仍可调度时 409 `NODE_SCHEDULABLE` | JWT |
| PUT | `/api/v1/nodes/{uuid}/labels` | 替换节点标签（key/value；空对象清空）。键名不得使用平台保留的 `gpuworks.ai/` 命名空间及其子域 | JWT |
| PATCH | `/api/v1/nodes/{uuid}/name` | 重命名节点 | JWT |
| PATCH | `/api/v1/nodes/{uuid}/accelerator` | 选择节点管理的加速卡厂商：`{"accelerator":"auto"}`，也可选 `nvidia`、`ascend`、`hygon`、`ppu`、`amd`。成功重启 agent 并返回节点；限制见[节点调度与加速卡切换](#节点调度与加速卡切换) | JWT |
| GET | `/api/v1/nodes/{uuid}/pull-proxy` | 节点使用的代理：`proxy`（首个选中它的代理条目，形同 `/pull/proxies` 的一项；没有条目选中时为 `null`，节点 agent 用自身环境）与 `runtime`：运行时类型，docker 节点另有 dockerd 实际使用的代理、与该条目是否一致（`applied`；节点没有代理时比较 systemd 当前加载的配置 `configured_*` 与实际值）、live-restore、`daemon.json` 是否也配了代理，以及 agent 的权限（`root` / `sudo` / `none`） | JWT |
| GET | `/api/v1/nodes/{uuid}/network` | 已删除：410 `SETTING_REMOVED`（`route` 指向 `/api/v1/pull/proxies`）。节点的代理与站点地址都在拉取设置里按节点标签配置（`/nodes/{uuid}/pull-proxy` 给出节点使用哪一条） | JWT |
| PUT | `/api/v1/nodes/{uuid}/network` | 已删除：410 `SETTING_REMOVED`，同上 | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/runtime/restart` | 重启节点的 dockerd 使代理生效，由 agent 执行（需 root 或免密 sudo）；失败 `RUNTIME_RESTART_FAILED`。dockerd 未开 live-restore 时节点上的容器会随之停止 | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/networks` | 节点每个角色解析到的网络（网卡/IP/HCA/提示）、管理网（地址、NAT、关闭的角色）与上报的网卡、RDMA 端口清单（带最近两分钟的收发速率）和加速卡网口清单（`accelerator_ports`） | JWT |
| GET | `/api/v1/nodes/{uuid}/ssh` | 查看节点 SSH 凭据配置（不含明文密钥） | JWT |
| PUT | `/api/v1/nodes/{uuid}/ssh` | 写入节点 SSH 凭据（离线恢复用） | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}/ssh` | 删除节点 SSH 凭据 | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/recovery/probe` | 手动触发一次离线节点恢复探测；什么都没做时 `skipped_code`（`RECOVERY_SKIPPED_*`）说明原因，失败时 `error_code` 是记在节点上的同一个码，`skipped_reason`、`error` 是英文原文 | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/token/regenerate` | 重新生成节点 token（会踢掉当前连接） | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}` | 默认通过保存的 SSH 凭据卸载 agent（Kubernetes 节点不卸载：agent 属于集群的 DaemonSet，只要 DaemonSet 仍选中该节点就会重新接入），确认完成后删除记录，返回 `{"status":"deleted","agent_uninstalled":true}`。没有已保存的 SSH 凭据时回 409 `NODE_DELETE_SSH_REQUIRED`，保留节点并提示配置 SSH 后重试，或手动卸载后使用 `?force=true`。卸载未确认回 409 `NODE_UNINSTALL_UNCONFIRMED`，保留节点及 SSH 凭据，并提示修复 SSH 后重试或手动卸载后强制移除。显式 `?force=true` 跳过卸载，仅删除记录，返回 `agent_uninstalled:false`；两种方式均拒绝有实例的节点（409）。force 非布尔值或重复参数回 400；节点不存在回 404 | JWT |
| GET | `/api/v1/events` | SSE 变更事件流 | JWT |

### Agent 批量升级

管理员先向 `/api/v1/agent-upgrades/preview` 提交全平台范围（不受节点列表的筛选、分页影响）：

```json
{"scope":"all"}
```

也可提交 `{"node_uuids":["node-a"]}`。预览返回完整 `node_uuids` 快照、`target_version`、`manifest_fingerprint`、逐节点资格 `nodes`、资格计数 `counts` 和全局阻塞项 `blocked_by_item`。创建请求必须复用该快照及目标：

```json
{
  "node_uuids": ["node-a", "node-b"],
  "expected_target_version": "v0.6.0",
  "expected_manifest_fingerprint": "<preview.manifest_fingerprint>",
  "idempotency_key": "<client-generated-unique-key>"
}
```


预览的 `target_build` 与版本更新接口的 `server.current_build` 共用当前构建说明：包含 `version`、`kind`（`stable`、`prerelease`、`development`、`unknown`）、可用时的 `commit`、`built_at`、`build_channel`、`published_version` 与 `published_at`。构建时间属于当前运行版本，不是远端候选时间；发布字段只在已验签索引的版本号、完整 commit 和构建时间均匹配时填写。Agent 预览只读取已有发布检查缓存，不新增出网请求；更新源不可用或记录不匹配时仍可预览本地制品。

成功返回 202 和批次对象，含 `id`、`state`、`counts`。同幂等键与相同请求返回原批次；修改请求或目标/制品变化返回 409，需重新预览。`GET /agent-upgrades`、`GET /agent-upgrades/{id}/items` 支持 `page`、`page_size`；逐节点结果可用单个 `state` 筛选。`processed` 包含所有终态，不等于 `succeeded`；`unconfirmed` 是未知结果：超时后 5 分钟内保留全局执行占位，之后批次继续；连续两台结果未知时批次暂停，`pause_reason` 为 `repeated_unknown_outcome`（安装包不可用时为 `artifact_unavailable`）。有 Agent 升级占用执行名额或批次仍有排队节点时，升级 server 的接口返回 409：`SERVER_UPGRADE_WAITS_FOR_AGENT`（`params.node`、`params.minutes`、`params.run_id`）或 `SERVER_UPGRADE_WAITS_FOR_AGENT_BATCH`（`params.run_id`、`params.queued`）。

停止只取消 queued 项。继续时先调用 `continue-preview`，再次创建时增加 `source_run_id` 指向原批次，原队列的撤销与新批次创建原子完成。管理员可选择向 `items/{uuid}/release` 提交 `{"note":"处理依据"}`，提前把结果未知的节点标记为已核实，成功返回 204；原结果保留，新增核实审计。查询接口可供管理身份读取，批量写入仅管理员且拒绝只读平台令牌。旧的单节点升级路由保留原权限，返回 202 时增加 `run_id`，与批量升级共享执行占位。操作和业务影响见[升级 Agent](../how-to/upgrade-and-compatibility.md#升级-agent)。

### 节点列表的筛选与返回字段

`GET /api/v1/nodes` 与 `/api/v1/nodes/facets` 显式读取已提交的最新节点与加速卡事实，并刷新共享机群快照；外部数据库连接提交的修改无需等待快照过期。未提交的修改不可见，长写事务不阻塞这些读取。后台监控与控制器继续使用机群快照的上报窗口。

`q` 匹配节点名称、主机名、UUID。`state` 只表示 agent 的连接状态：`online`、`offline`、`connecting`，
加速卡的问题不改变它。可排序的列为 `name`、`state`、`agent_version`、`gpu_count`、`cpu_cores`、`mem_total`、`created_at`。

分页时，节点对象省略 `topology`（拓扑）和 `path_usage`（路径用量），详情见 `GET /api/v1/nodes/{uuid}`。
分页响应另带两个不受筛选影响的字段：`all` 是全部节点数；`labels` 是全体节点的用户标签，结构为 `[{"key", "values"}]`。

按标签筛选使用 `label=key=value`，可重复传入。同一个 key 的多个值之间为“或”，不同 key 之间为“且”，
与部署放置条件使用同一套匹配规则。
以 `gpuworks.ai/` 开头的 key 匹配系统标签，例如 `gpuworks.ai/vendor=nvidia`、`gpuworks.ai/gpu-model=A100`；
其他 key 匹配用户标签。不存在的系统 key 或非法取值返回 `400 LIST_FILTER_INVALID`。

`GET /api/v1/nodes/facets` 用于获取**筛选项和对应节点数量**，接受相同的 `q`、`state`、`label`。
部署表单与放置条件编辑器可直接用它生成选项与计数，无须先读取全部节点。返回内容包括：

| 内容 | 含义 |
|---|---|
| 命中节点数 | 满足当前筛选条件的节点总数 |
| 用户标签值、系统标签值 | 每个值附带携带它的节点数；系统标签带服务端显示文本，不含节点自身的 `gpuworks.ai/node` |
| 卡型号 | `vendor`、`name`、`cc` 描述厂商、名称、计算能力；已报告时含 `arch_id`，不同架构分别统计；`model` 对应系统标签 `gpuworks.ai/gpu-model` 的值 |
| 每种卡型号的数量与显存 | 节点数，以及 `cards`（卡数）、`memory_bytes`（最大单卡显存） |

### 仪表盘汇总的返回字段

`GET /api/v1/dashboard/summary` 的计数都由 server 算好，仪表盘只做展示。除节点计数和容量分组外，主要字段如下：

| 字段 | 含义 |
|---|---|
| `current_metrics.vram_used_bytes`、`current_metrics.vram_total_bytes` | 在线节点自有卡的显存已用与总量，只累加最近两分钟内两项都有采样的卡，两者之比即显存利用率；没有这样的卡时均为 `null` |
| `deployments.running`、`warming`、`sleeping`、`pending`、`other` | 部署按有效状态分桶，每个部署只落一个桶，相加等于 `total`。`warming` 为拉镜像或启动中，`sleeping` 为整体休眠或正在休眠，`pending` 为等待放置，`other` 为不属于前述各桶的部署（已停止、失败、离线等） |
| `cards` | 与 `GET /api/v1/scheduling/overview` 的 `summary` 相同的卡状态计数，另加 `allocated`；调度总览无法构建时为 `null`，表示未知而非没有卡 |
| `cards.allocated` | 部署占用的卡数，即 `awake + asleep`：休眠部署的卡仍为唤醒保留，算作已分配；平台外占用、不健康和离线的卡不算。`allocated / cards` 即分配率 |
| `tasks.batches` | 调用者可见的批处理数（管理员为全部用户，其他用户只算自己的）：`active` 为执行或收尾中，`waiting` 为校验输入中；未启用 Batch API 或计数失败时为 `null` |
| `tasks.benchmarks` | 等待或运行中的评测数，统计全部用户（评测不按用户归属） |

### 节点连接诊断

`POST /api/v1/nodes/{uuid}/connection-diagnostics` 由管理员手动触发，接受空请求或 `{}`，拒绝自定义 URL、SSH 参数与其他字段。通过指定节点的既有会话读取运行态；SSH 旁路仅使用该节点已存身份与地址。不执行恢复、不修改地址/token/安装配置，也不重启或重装 Agent；SSH 沿用现有主机密钥策略。

| 字段 | 含义 |
|---|---|
| `node_uuid`、`checked_at`、`connect_mode`、`saved_server_url` | 节点、报告时间、连接方向、安全保存地址；不是运行进程配置回读 |
| `last_seen_at`、`recovery_phase`、`recovery_attempts`、`last_probe_at`、`last_recovery_error`、可选 `last_recovery_code`（最后错误的码，旧记录没有）、可选 `last_reject_reason`/`last_reject_at` | 已有上报、恢复与受控拒绝历史，不是本次执行恢复；未知拒绝原因不回显 |
| `exposure` | `null` 或 `{url,state,observed_at}`，公网连接器最近记录及原时间；不是本次存活检查 |
| `checks` | `agent_control` 表达本次RPC/离线/旧版/失败；另有可选 `ssh`、`configuration`、`node_http`。每项有 `stage`、`outcome`、`vantage`，有证据才有 `target`、`source`、`observed_at`、`http_status` |
| `agent_report?` | 清洗后的已有 `DoctorReport`，仅白名单 `connection.*` primitive Facts；`report` 为空，不透传 Findings、typed evidence 或其他原文 |
| `guidance` | 本地化指引码，区分运行态、SSH快照、HTTP范围、本地CLI和手动同步地址 |

在线且宣告 `node.connection_diagnostics` 能力的 Agent，经独立轻量 RPC 返回接收请求的那条连接的快照，`connection.source=agent_runtime`：实际 Server URL、连接方向、当前 ctrl/data、真实传输阶段状态/耗时/原始时间及可用代理事实。Server 最多等待 15 秒，Agent 自行限制 10 秒；取消HTTP等待不等于远端执行已取消。不新建连接、不运行完整 `node.doctor`；能力检查后换成旧会话也不会误跑硬件自检，旧会话迟到结果丢弃。尝试 RPC 后不再串行执行 SSH。

`agent_control.outcome=runtime_snapshot` 表示本次控制通道往返与安全快照成功，不证明业务传输或推理健康。data `attached` 只是当时连接，`detached` 表示等待重连，不会自动切回 ctrl；`disabled`、`not_negotiated`、`pending`、`unknown` 不可混用，缺字段不补成功。在线RPC不伪造CLI-only日志历史。

SSH 的 `vantage=control_plane` 描述控制面登录节点；HTTP 的 `vantage=node` 是SSH环境访问保存URL的健康接口，可能不同于Agent代理环境。`configuration.source=state_file|legacy_arguments` 仅表示快照来源，不保证覆盖flags/env后的有效配置。`server_dial` 不主动访问控制面URL，此项不适用。

离线、旧版、取消、超时或无效响应仍可返回 HTTP 200 的部分报告，须逐项看 `outcome`。可用SSH为旁路，否则从节点控制台运行 `gpuworks-agent doctor --connectivity --state <实际目录>`。HTTP健康成功不证明WS/认证/SSE成功；地址差异只提示核对，不自动修改地址。

非法请求为400 `NODE_CONNECTION_DIAGNOSTICS_INPUT`；读取节点设置失败为500 `NODE_CONNECTION_SETTINGS_UNAVAILABLE`、不回显DB错误；未知节点404。

返回不含 SSH 密码、私钥、节点 token、完整配置、进程参数或环境变量。该 POST 接口没有接入 AI 助手现有的只读 GET 工具，不能仅因出现在接口目录中就认为助手已能执行这次诊断。

### 创建节点的连接参数

`POST /api/v1/nodes` 创建待接入节点，返回安装命令与 token。连接方向决定地址字段的要求：

- **`agent_dial`（`connect_mode` 的默认值）**：请求体必须带 `server_url`，即 agent 将拨号的地址；UI 用浏览器当前地址预填。
  缺失时返回 400 `SERVER_URL_REQUIRED`。地址必须为不含账号、查询或片段的绝对 http(s) 地址，否则返回 400 `SERVER_URL_INVALID`。
- **`server_dial`**：agent 从不连接 server，因此 `server_url` 可省略，传了也不保存；响应里的 `server_url` 为空。
  `install_command` 返回[离线包](../how-to/connect-nodes.md#离线包)的命令：`sh gpuworks-agent.run --token '…' --connect-mode server_dial --listen '…'`。
- **监听地址**：`server_dial_listen` 缺省为 `0.0.0.0:18800`；必须是有效的 `host:port` / `:port`，端口在 1–65535 之间，否则返回 400 `SERVER_DIAL_LISTEN_INVALID`（`address`）。

**接入限制**：server 上可以注册节点接入守卫（供扩展模块使用，例如站点的节点上限）。守卫拒绝新节点时，`POST /api/v1/nodes`、`POST /api/v1/nodes/install`、`POST /api/v1/providers/{id}/install` 与 `POST /api/v1/providers/{id}/instances` 回 403，`code` 与 `params` 由守卫给出（守卫没有给 code 时为 `NODE_ENROLLMENT_REFUSED`），不创建节点；SSH 安装、云实例安装与租用实例在动手之前就先检查。Kubernetes agent 被拒时 `/api/v1/kube/enroll` 回 403 `KUBE_ENROLL_REFUSED`（`params.reason: enrollment_refused`，`params.refusal_code` 是守卫的 code，守卫的其余参数一并带上），agent 视为最终结果；Slurm agent 被拒时 `/api/v1/slurm/enroll` 回 403 `SLURM_ENROLL_REFUSED`，`params` 相同。接入过的节点重新注册从不被拒。集群条目尚无节点接入时的 `apply` 同样先检查。没有注册守卫时，上述接口的行为不受影响。

### SSH 检查与安装

`POST /api/v1/nodes/precheck`、`POST /api/v1/nodes/install` 和 `PUT /api/v1/nodes/{uuid}/ssh` 共用以下 SSH 认证字段：

| 字段/取值 | 含义 |
|---|---|
| `auth_mode: "platform_key"` | 仅尝试 server 的平台密钥，无需提交密码或私钥 |
| `auth_mode: "private_key"` | 仅尝试 `private_key_pem` 中的私钥 |
| `auth_mode: "password"` | 仅尝试 SSH `password` 协议，口令放在 `password` |
| `auth_mode: "keyboard_interactive"` | 仅尝试 SSH `keyboard-interactive` 协议，复用 `password`；支持一次非回显密码应答及有限的无问题信息轮次，不支持多问题、多次口令或 MFA |
| `auth_mode` 省略或为空 | 按已有凭据依次尝试平台密钥、用户私钥、用户密码、云实例密码，最后用请求或已保存的 `password` 尝试键盘交互认证；不可用的凭据跳过 |

显式指定方式时不回退到其他协议。认证方式无交集时，错误会说明已配置的方式并提示核对目标 SSH 配置或选择键盘交互认证；网络及主机密钥错误保持原分类。

安装成功或保存凭据时仍尝试授权平台公钥，成功后清除密码/私钥，授权失败才保留所用凭据。`GET /api/v1/nodes/{uuid}/ssh` 的 `auth_mode` 表示保存的凭据类型，不保存本次选择的协议：保留口令时为 `password`，不回传 `keyboard_interactive`；空凭据为 `platform_key`，私钥为 `private_key`。响应不含明文口令或私钥，`has_password` / `has_private_key` 表示是否保存；后续恢复使用上述自动顺序。

预检查、SSH 安装（`checks`）、云主机安装（`checks`、`precheck_details`、`install_details`）和保存 SSH 凭据（`platform_key`）返回的每一项检查都是同一形状 `{id, status, code?, params?, message?, evidence?}`：`id` 为 lower_snake 的检查标识（如 `ssh_auth`、`install_dir_space`、`server_reachability`）；`status` 只取 `pass`/`warn`/`fail`/`skip`/`na`，`skip` 表示应查但没查成（探测超时、缺少前提），`na` 表示不适用于这台主机，二者都不是通过；`code` 为 UPPER_SNAKE 的结果码，`params` 为其插值参数，客户端按 `apiError.<code>` 渲染，`message` 只是英文 fallback；`evidence` 是远端命令、SSH 或云厂商 API 返回的原文，不作为说明文字。没有 `code` 的 `pass` 只报告所观察到的内容（在 `evidence` 里）。云主机安装的 `checks` 固定为 `precheck`（预检查各项中最严重的状态，`params.checks` 列出需关注项的 id）、`install_agent`，以及 `agent_dial` 或 `server_dial`。

`POST /api/v1/nodes/precheck` 检查主机环境，连通性检查按连接模式和检测结果返回，可能包含：

- `server_reachability`：节点能否访问 server。
- `server_to_agent`：server 能否拨通 `host_address`（缺省为 SSH 主机）。地址带端口时用该端口，否则取 `server_dial_listen` 的端口（默认 `0.0.0.0:18800`）；`server_dial` 模式下不通为 `fail`，而非 `warn`。
- `connectivity`：连通性总结。处理方法见[节点与 server 网络不通](../how-to/connect-nodes.md#节点与-server-网络不通)。

`agent_dial` 能连通 server 时，不显示后两项。

`POST /api/v1/nodes/install` 在 Linux 与 macOS 上均上传 agent 和 `install.sh` 后执行脚本。
`server_url` 与 `server_dial_listen` 的要求、错误码同[创建节点](#创建节点的连接参数)。Linux 的运行方式为：

- 默认注册 `gpuworks-agent.service`。
- `no_systemd: true` 使用 nohup，不改已有服务或其他目录的 agent，也不因主机已经接入而拒绝安装。
  只有安装目录内已有 agent 运行时，返回 409 `NODE_INSTALL_EXISTING_AGENT_SAME_DIR`（`dir`）；确认替换后带 `replace_existing_agent` 重试。
  安装目录的某级上级目录对专用运行用户 `udocker` 关闭、且探测确定该节点会以 root agent + runveil + 专用用户运行时，返回 400 `NODE_INSTALL_DIR_UNTRAVERSABLE`（`dir`、`mode`、`install_dir`、`fix`），不上传 agent；`POST /api/v1/providers/{id}/install` 同样返回。预检查请求可带 `runtime_driver`，使该行判断与安装一致。

macOS 忽略 `no_systemd`。

`install_dir` 省略时用 `/opt/gpuworks`，默认目录不可写时回退到 `/tmp/gpuworks`；显式填写的目录不可写时安装失败，不回退。

### 节点调度与加速卡切换

`PUT /api/v1/nodes/{uuid}/scheduling` 的三个取值影响如下：

- `cordoned`：不再启动新副本，已有副本继续服务。候选池单列为 `cordoned`；手动放置的多机或 PD rank 指定了该节点时等待，原因为 `node_cordoned`；自动放置的在副本要启动时把这些 rank 换到别的节点（`UNIT_PLACED`）。
- `draining`：同时逐个迁走这台节点上的副本；先在其他节点启动替代副本，运行后再排空原副本，每个部署一次一个。多机与 PD 副本同样先起后切（替代节点同计算网络、同 GPU 架构、卡空闲，运行后放置才改写，时间线 `NODE_DRAIN_MOVED`），没有节点可去时留在原处等待。钉在这台节点上的单机部署不迁。
- `schedulable`：恢复调度，等待中的副本立即重试。

节点的 `scheduling_by` 说明当前状态是谁设的：`operator`（经这个接口设的）或 `platform`（平台自动停止调度）；可调度或升级前就停止的节点没有这个字段。
平台只解除自己设的停止调度。`scheduling_by` 为 `platform` 时，`cordon_reason` 给出现在的原因：`runtime_gpu_blocked`（运行时给不了容器加速卡）、`card_needs_host_reboot`（有卡要重启主机或断电才能恢复）、`kube_unschedulable`（Kubernetes 已 cordon 该节点，Kubernetes 里 uncordon 后平台随之恢复）或 `slurm_node_unavailable`（Slurm 不会在这台节点上启动作业，见下；Slurm 恢复后平台随之恢复）。
借 Slurm 的卡的节点（[接入 Slurm 集群](../how-to/connect-slurm-nodes.md)）带 `slurm_node`（它在 Slurm 里的名字）。Slurm 不会在节点上启动作业时（占位作业会一直排队），另带 `slurm_unavailable`（`gres_missing`：slurmd 登记的 GPU 比 Slurm 管控节点配置的少，通常是没有 `gres.conf`；`invalid_registration`：登记被拒绝；`down`：down 或无响应；`drained`：drained 或 draining）和 `slurm_reason`（Slurm 自己的 Reason 原文，只用来显示），同时 `cordon_reason` 为 `slurm_node_unavailable`；还带 `slurm_fixes`，即 server 按该状态给出的恢复步骤，按顺序排列，每步是 `{code, commands}`：`code` 为 `gres_conf`（在 Slurm 管控节点的 `gres.conf` 里加一行，命令就是 server 按该节点的卡数拼好的那一行，例如 `NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]`；只有 NVIDIA 卡且知道卡数时才有）、`reconfigure`、`resume`、`restart_slurmd`、`resume_drained`、`check_slurmd`，客户端按 `code` 显示自己语言的说明，`commands` 原样显示；`gres.conf` 的行也可由集群条目的「检查并应用」在 Slurm 管控节点上补上（见下面 Slurm 一节）。Slurm 恢复运行作业后这些字段随下一次上报消失，平台随之解除自己设的停止调度。

平台停止调度后，经这个接口设为 `schedulable` 立即生效，原因还在时下一次上报会再次停止。
扩展模块还可以把节点保持在停止调度状态（扩展停止调度）：被保持的节点在下一次上报时（或扩展立即应用时）设为 `cordoned`，`scheduling_by` 为扩展的来源名；从不设为 `draining`，已有副本照常服务。扩展停止调度高于平台、低于管理员：平台原因消失不会解除它；保持期间设为 `schedulable` 回 409，`code` 与 `params` 由扩展给出（没有给 code 时为 `NODE_SCHEDULING_HELD`），设为 `cordoned` 或 `draining` 照常生效。扩展不再保持后，平台原因还在时回到平台停止调度，否则恢复可调度。
节点的 `uncordon` 说明此时设为 `schedulable` 的结果，由 server 判定：`allowed`；`confirm`（平台有停止调度的原因，下一次上报会再次停止）；`refused`（扩展保持中，`cordon_message` 是扩展给出的 `{code, params, message}`）。
扩展保持的卡：扩展模块还可以把单张卡保持在停止调度状态。被保持的卡在节点下一次上报时（或扩展立即应用时）不再接受新部署，卡上 `schedulable` 为假，`scheduling_held_by` 为扩展的来源名，`scheduling_hold` 是扩展给出的 `{code, params, message}`，`scheduling_actions` 只给 `stopped`；节点本身不停止调度，卡上已运行的实例照常服务。它高于管理员的启用、低于管理员的禁用：保持期间 `PUT /api/v1/nodes/{uuid}/gpus/scheduling` 设为 `schedulable` 回 409（`code` 与 `params` 由扩展给出，没有给 code 时为 `CARD_SCHEDULING_HELD`），设为 `stopped` 照常生效；保持结束后卡按管理员的决定或平台判定接受部署。调度器的拒绝原因为 `gpus_held`（参数 `gpus`、`count`、`node`）。

`PATCH /api/v1/nodes/{uuid}/runtime` 设置节点的容器运行时偏好 `{runtime: auto|docker|nerdctl|udocker}`，由 agent 接受后保存。节点的 `runtime_choices` 列出这个节点可以设置的值，按展示顺序：Mac 只有 `auto`，其他节点是全部四种；**Kubernetes 节点没有这个字段**（实例是集群里的 Pod，没有运行时可选），对它设置返回 409 `NODE_RUNTIME_KUBE_NODE`。非法值回 400 `NODE_RUNTIME_INVALID`。

`GET /api/v1/nodes/{uuid}` 对 NVIDIA 实例 Pod 只靠 `CUDA_VISIBLE_DEVICES` 限定到自己那张卡（软隔离：Pod 能看到节点的全部 GPU 设备）的 Kubernetes 节点带 `gpu_isolation`：`level`（`soft`）、`reason`（`cdi_names_missing`：工具包按 CDI 名解析 `NVIDIA_VISIBLE_DEVICES`，而没有 CDI spec 给卡命名）、`toolkit`（`gpu_operator` 或 `host`，agent 能判断时给出）、`cards`，以及 `fixes`：每项有 `code` 与按顺序原样执行的 `commands`，由 server 按 `toolkit` 选出适用的。它来自 agent 最近一次状态上报，不入库：节点离线、严格隔离或旧 agent 时没有这个字段；节点列表不带它。

`GET /api/v1/nodes/{uuid}` 对内核会让 UCX（NIXL 自带，PD 的 `vllm-nixl`、`sglang-nixl` 经它传 KV）卡住的节点带 `ucx_netlink_hang`：`kernel`（内核版本，读不到时省略）与 `fixes`——每项有 `code`（`disable_ipv6_now`、`disable_ipv6_persist`）与按顺序原样执行的 `commands`（关闭整机 IPv6，前者到重启前有效，后者写入 `/etc/sysctl.d/90-gpuworks-ucx.conf` 持久化）。它来自 agent 最近一次状态上报（每 15 分钟复查），不入库：节点离线、不受影响或旧 agent 时没有这个字段；节点列表不带它。

`PATCH /api/v1/nodes/{uuid}/accelerator` 供一机多厂商选择管理哪家的卡。agent 接受后原地重启，选择才保存。
返回节点的 `accelerator_preference` 表示选择；`accelerator_vendors` 列出主机各厂商的 `{vendor, cards, model}`。

- 非法值：400 `NODE_ACCELERATOR_INVALID`；主机没有该厂商的卡：400 `NODE_ACCELERATOR_NOT_ON_HOST`。
- 节点有未停止的实例：409 `NODE_ACCELERATOR_HAS_INSTANCES`（`params.count`）；agent 拒绝或离线也返回 409。
- 节点不存在：404。

### 节点标签：写入的与生成的

节点有两种标签，在 DTO 里是两个字段：

- `labels`：运维写的（`PUT /api/v1/nodes/{uuid}/labels`），键值自定。
- `system_labels`：平台从节点自身事实算出来的，只读。不落库，每次读取现算，所以换卡、改网络、
  重命名网络之后立刻反映出来。

生成的键都在保留命名空间 `gpuworks.ai/` 下，写入的标签不得使用该命名空间及其子域
（`rack.gpuworks.ai/...`），这样看键名就知道是平台的事实还是人写的。网络加载用的标签同此规则。
升级到引入这条规则的版本时，迁移 119 删除节点上已有的这类键（不分大小写），按这类键加载的网络
一并删除；部署里引用这类键的标签条件不改，因为删掉条件等于放宽约束——这样的部署在有人修改它之前
不会被调度到任何节点。

| 键 | 值 | 说明 |
|---|---|---|
| `gpuworks.ai/node` | 节点 UUID | `display` 是节点名 |
| `gpuworks.ai/vendor` | 规范厂商 id | 整机一个 |
| `gpuworks.ai/cpu-arch` | `amd64` / `arm64` | |
| `gpuworks.ai/gpu-model` | 规范化后的型号 | 每种型号一条，混卡节点有多条 |
| `gpuworks.ai/gpu-arch` | 厂商内的架构代际（`turing`、`910b`…） | 认不出的卡不出这条 |
| `gpuworks.ai/compute-network` | 网络 ID；管理网承担时为 `management` | 该角色无网络时不出这条 |
| `gpuworks.ai/storage-sync-network` | 同上 | |
| `gpuworks.ai/serving-network` | 同上 | |
| `gpuworks.ai/kubernetes-cluster` | 集群 UUID | 仅从 Kubernetes 集群接入的节点；`display` 是集群名，改名不影响匹配 |
| `gpuworks.ai/kubernetes-node` | Kubernetes 里的节点名 | 同上；`display` 同值 |
| `gpuworks.ai/slurm-cluster` | Slurm 集群条目的 UUID | 仅从 Slurm 集群条目接入的节点（手动装了 agent、只是向 Slurm 借卡的节点没有）；`display` 是条目名，改名不影响匹配 |
| `gpuworks.ai/slurm-node` | Slurm 里的节点名 | 同上；`display` 同值 |

同一个键可以有多条（值是集合）。每条是 `{key, value, display, display_code}`：`value` 是用于匹配的
字面值，`display` 是展示文本，`display_code` 只在值是平台词汇（厂商、管理网）时给出，指向客户端
已有的翻译；客户端优先用 `display_code`，取不到时用 `display`。网络和节点的值是 ID 而不是名字，
所以改名只改 `display`，不影响按标签做的匹配。

## Kubernetes（集群接入）

一个 Kubernetes 集群以一个 DaemonSet 接入：管理员创建集群，在集群里 `kubectl apply` 返回的命令，被选中的每个 GPU 节点上的 agent 用集群的接入令牌换取自己的节点 token，之后与普通节点一样。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/kube/clusters` | 集群列表：`{items:[{id,uuid,name,namespace,server_url,agent_image,agent_image_source,effective_agent_image,vendors,node_selector,runtime_class,image_pull_secret,containerd_socket,state_dir,api_server,engine_ports,idle_return_seconds,priority,allow_preemption,cluster_uid,stalled,revoked,state,category,resources,node_count,created_at,pool_ids,namespace_shared_with,can_apply,can_remove,operation_blocked?,apply_command?,verify_command?,remove_commands?,source?:{provider_id,provider_name,provider_kind,cluster_id}}], total}`；`apply_command` 含接入令牌，只给管理员，**AI 助手读这个接口时一律不含它**；`verify_command` 是 apply 之后逐个节点检查 agent Pod 的命令（`curl -fsSL '<server>/kube/check.sh?token=…' | bash`），同样含接入令牌，只给管理员、AI 助手读时不含，令牌吊销后不再给出；条目没有厂商：`vendors` 是已接入节点按厂商的计数 `[{vendor,nodes}]`（节点最多的在前，`vendor` 为空表示没有上报加速卡的节点），每个 agent 自己识别所在节点的厂商；`remove_commands`（只给管理员，吊销过的条目也有）是把条目的 agent 从集群里移走的 kubectl 命令，按顺序执行：删 DaemonSet（`kubectl -n <命名空间> delete daemonset gpuworks-agent --ignore-not-found --wait`）、删 ClusterRoleBinding `gpuworks-agent-<命名空间>`、删 ClusterRole `gpuworks-agent-<命名空间>`、删 PriorityClass `gpuworks-inference-<命名空间>`、删命名空间，都带 `--ignore-not-found`，不需要接入令牌；`source` 只在从云账号的集群接入时出现；`namespace_shared_with` 是用同一命名空间、又可能属于同一集群的其他条目名（升序；还没有节点接入的条目算在内，已知属于不同集群的不算），同一集群里后应用的会接管前一个的 agent，控制台据此提示；`pool_ids` 是云条目选用的节点池 ID（升序，手动条目为空数组，云账号删除后仍保留）；`cluster_uid` 在首个节点接入前为空；`idle_return_seconds` 为 -1 表示借到的卡不归还；`agent_image_source` 是条目从哪个仓库拉发布的 agent 镜像（`dockerhub`、`swr`、`quay`），`effective_agent_image` 是 manifest 实际写的镜像（`agent_image` 非空时就是它）；`stalled` 由 server 算：没有节点接入、未吊销且创建超过 `GET /kube/defaults` 的 `stalled_after_seconds`，客户端据此提示排查；`state` 是条目所处的状态，客户端显示和筛选都用它，按顺序判断：`revoked`（令牌已吊销）、`stalled`（同上）、`pending`（还没有节点接入）、`active`（已有节点接入）；`category` 为 `kubernetes`，`resources` 为 `[{type:"kubernetes"}]`，即 `GET /providers/kinds` 的 `categories` 里 Kubernetes 那一项，独立接入的条目在外部平台列表里按它归类；`can_apply`、`can_remove` 说明 server 能否把 manifest 应用到来源集群、从中删除，不能时 `operation_blocked` 给原因：`no_source`（手动建的条目，server 没有进入集群的途径）、`private_api`（云厂商说该集群的 API Server 没有公网地址，只能在集群内网里用 `kubectl apply`；云账号暂时问不到时不判定，操作本身会报告失败）、`revoked`（令牌已吊销，只拒绝应用，仍可删除）。云厂商的集群列表在 server 里缓存 1 分钟，列表轮询不会每次访问云 API | JWT |
| GET | `/api/v1/kube/defaults` | 集群表单要用的字典：`{namespace, state_dir, engine_ports, idle_return_seconds, idle_return_keep(-1，即 idle_return_seconds 里表示不归还的值), agent_image, stalled_after_seconds, agent_image_source, agent_image_sources:[{id, image}], priority, priority_max, allow_preemption, check_codes:{precheck:[], verify:[]}}`；`check_codes` 是两个检查脚本（预检查、apply 之后的检查）会输出的状态码，按客户端解释它们的顺序排列：`OK`、`IMAGE_PULL_FAILED`、`SERVER_UNREACHABLE`（只有预检查）、`PULL_SECRET_MISSING`（只有预检查）、`NO_NODE_SELECTED`、`NOT_SCHEDULED`、`POD_CREATE_FAILED`、`CONTAINER_FAILED`、`STILL_STARTING`、`DAEMONSET_MISSING`（只有 apply 之后的检查）、`APPLY_FAILED`（只有预检查）、`KUBECTL_MISSING`、`KUBECTL_FAILED`；`priority` 是创建请求省略时的优先级（10000），取值 0 到 `priority_max`，`allow_preemption` 默认 false；`agent_image` 是默认仓库（`agent_image_source`，华为云 SWR）在当前运行环境版本的镜像，`agent_image_sources` 列出发布 agent 镜像的全部仓库（默认的在前）及各自的镜像；`namespace`、`state_dir`、`engine_ports`、`idle_return_seconds`、`agent_image` 是创建请求省略对应字段时 server 采用的值，客户端只当提示展示，留空即可；字典里没有厂商（条目没有厂商，RuntimeClass 只影响 NVIDIA 的实例 Pod，其他厂商的设备经 agent 自带的 device plugin 交给实例，不受它影响） | JWT |
| POST | `/api/v1/kube/clusters` | 创建集群：`{name, server_url, namespace?(gpuworks-system), agent_image?(自己的镜像；留空则不写入条目，manifest 渲染时用 `agent_image_source` 仓库里当前运行环境版本的镜像), agent_image_source?(dockerhub\|swr\|quay；省略时新建手动条目为华为云 SWR（持久化为 swr）；已有历史空来源仍按 Docker Hub 解释，云条目按集群地域选：阿里云中国内地地域为 swr，其余 dockerhub；其他值回 400), node_selector?("k=v,k2=v2"), runtime_class?(只影响 NVIDIA 的实例 Pod；空表示各节点自己判断：有 nvidia RuntimeClass 就用，没有用默认运行时), image_pull_secret?(命名空间里 docker-registry Secret 的名称，agent 镜像用它拉取；空表示不用), containerd_socket?(空表示 agent 自己找), state_dir?(/opt/gpuworks), api_server?("https://host:port"，空表示用集群的 kubernetes Service), engine_ports?(40000-42767), idle_return_seconds?(60), priority?(10000，0 到 1000000000：占位与实例 Pod 的 PriorityClass 数值), allow_preemption?(false：PriorityClass 写 preemptionPolicy: Never), provider_id?, provider_cluster_id?, provider_pool_ids?(provider_id 与 provider_cluster_id 同给：从云账号的这个集群接入，见 GET /providers/{id}/clusters；此时 `provider_pool_ids` 必填，是条目要用的该集群的 GPU 节点池，可以多个、任意厂商，`node_selector` 不能再给；手动条目不带这三项，仍用自由格式的 `node_selector`)}`，返回 201；参数不可用回 400 `KUBE_CLUSTER_INVALID`（`field`、`reason`；节点池不存在或没有选也是它；条目没有厂商，节点池可以是任意厂商），重名回 409 `KUBE_CLUSTER_NAME_TAKEN`；同一来源集群已有条目回 409 `KUBE_CLUSTER_SOURCE_TAKEN`（`cluster`，一个集群只有一个条目，要用更多节点池改它的节点池）；创建时 server 向云厂商核对节点池，云厂商报错回 424 | JWT(admin) |
| POST | `/api/v1/kube/precheck` | 接入前的预检查脚本：`{server_url, namespace?, agent_image?, agent_image_source?, node_selector?, image_pull_secret?}`，省略的字段取新建手动条目的默认值（同 `POST /kube/clusters`），返回 `{script, file_name("gpuworks-precheck.sh"), image, server}`：`image` 是每个选中节点要拉的 agent 镜像，`server` 是它们要连的 `host:port`（URL 没写端口时 http 为 80、https 为 443）。脚本用 bash 在能用 kubectl 访问集群的机器上执行：在要接入的节点上起一个临时 DaemonSet `gpuworks-precheck`（节点选择、容忍、拉取凭据、主机网络与 DNS 同正式 agent，不要特权），每个节点输出一行“节点 状态码 详情”（状态码见 `GET /kube/defaults` 的 `check_codes.precheck`），全部通过时退出码为 0，结束时删掉它建的对象。什么都不保存，脚本里没有接入令牌；不支持按节点池选节点的云条目。参数不可用回 400 `KUBE_CLUSTER_INVALID`（`field`：`server_url`、`node_selector`、`image_pull_secret`、`agent_image_source`，其余为 `manifest`） | JWT(admin) |
| PUT | `/api/v1/kube/clusters/{uuid}/pools` | 替换条目的节点池：`{provider_pool_ids}`，规则同创建（至少一个、都属于条目的来源集群，否则 400 `KUBE_CLUSTER_INVALID`；节点池可以是任意厂商）；返回更新后的集群（同 GET 列表的一项，字段仍在顶层）外加 `stopped_instances`（见下，没有时为空数组）。manifest 随即按新节点池渲染，**集群里的 DaemonSet 要重新应用 manifest 才会变**（`POST …/apply` 或 kubectl）；去掉的节点池上的 agent 随之退出，节点离线；离线 15 分钟后，server 请同条目另一个在线 agent 确认节点是否还在、是否还运行条目的 agent Pod，都不是且节点上没有实例时自动删除节点记录（条目里没有别的在线节点时不问，手动删除）。请求体可带 `confirm_drop_instances`（布尔，默认 false）：去掉节点池会让仍有实例运行的节点退出选择时，未确认回 409 `KUBE_SELECTION_DROPS_INSTANCES`（`nodes`：每项 `node_uuid`、`node_name`、`kube_node`、`deployments`；`verified`：false 表示没有在线 agent 可问，列出的是所有还有实例的节点，只是可能受影响），带 `true` 重发才保存；节点池没变不询问。带 `true` 重发时，server 先重新计算退出的节点，请求停止这些节点上还没结束的实例（与保存在同一个事务里，校验与重名检查失败时不停任何实例；状态改为 `removing`，同一部署在其他节点上的副本不动，不等 agent 停完；server 确认节点会退出时还把它们设为停止调度，已停止调度或排空的保持原状；`verified` 为 false 时只停实例、不设停止调度），再保存；`stopped_instances` 列出被请求停止的实例：`instance_uuid`、`deployment`（部署名）、`node_uuid`，没有节点退出时为空数组手动条目或来源云账号已删除回 409 `KUBE_CLUSTER_NO_SOURCE`；更早建的、用自由格式节点选择器的云条目，改节点池后只按节点池选节点（节点选择器被清空） | JWT(admin) |
| PUT | `/api/v1/kube/clusters/{uuid}` | 修改条目：请求体字段都可选，省略表示不改，空字符串表示清空：`name, node_selector, agent_image, agent_image_source, image_pull_secret, runtime_class, containerd_socket, api_server, engine_ports, idle_return_seconds, priority, allow_preemption`，另可带 `confirm_drop_instances`（布尔，默认 false，不是条目的设置）；未知字段回 400，`namespace`、`server_url`、`state_dir` 创建后固定，不能改；云条目按节点池选节点，改动 `node_selector` 回 400 `KUBE_CLUSTER_INVALID`（改节点池用 `PUT …/pools`）。`node_selector` 改了、让仍有实例运行的节点退出选择时，server 请条目里一个在线 agent 读取这些节点判断，未确认回 409 `KUBE_SELECTION_DROPS_INSTANCES`（参数同 `PUT …/pools`），带 `confirm_drop_instances: true` 重发才保存，server 先请求停止退出节点上的实例再保存（语义同 `PUT …/pools`）；选择没变不询问、不停任何实例。校验同创建：回 400 `KUBE_CLUSTER_INVALID`（`field`、`reason`），重名回 409 `KUBE_CLUSTER_NAME_TAKEN`，uuid 不存在回 404。返回 `{cluster, changed, reapply, commands, stopped_instances, warnings}`：`cluster` 同 GET 列表的一项，`changed` 是改动了的字段的 JSON 名（升序），`reapply` 为 true 表示 manifest 变了、集群里仍跑旧的，要再应用一次才生效；`commands`（只给管理员，手动建且未吊销的条目）是一行 kubectl 命令：`kubectl delete priorityclass gpuworks-inference-<命名空间> --ignore-not-found && <apply 命令>`（Kubernetes 不能原地改 PriorityClass 的数值与 `preemptionPolicy`，之前的优先级改动也可能还没应用）；manifest 没变时为空；`stopped_instances` 是确认后被请求停止的实例（`instance_uuid`、`deployment`、`node_uuid`），没有时为空数组；`warnings` 是允许保存但需要知道的事（每项 `code`、`params`，没有时为空数组）：改 `engine_ports` 后若运行中实例占用的端口落在新范围之外，带一条 `KUBE_ENGINE_PORTS_OUTSIDE`（`range`：新范围；`instances`：每项 `instance_uuid`、`deployment`、落在范围外的 `ports`），保存照常成功、这些实例保留原端口。云条目用 `POST …/apply` 应用，server 自己先删后建变了的 PriorityClass。`runtime_class`、`engine_ports`、`idle_return_seconds` 不在 manifest 里：保存后 server 立即把它们发给条目的在线 agent（离线的重连时取到），不用重新应用、不重启 agent，所以只改这三项时 `reapply` 为 false、`commands` 为空；新值对之后起的实例 Pod、之后分配的实例端口与之后的归还判断生效，已经在跑的实例不变（含端口落在新范围之外的实例，见 `warnings`） | JWT(admin) |
| GET | `/api/v1/kube/clusters/{uuid}/manifest` | 集群的 manifest（`application/yaml`） | JWT(admin) |
| GET | `/api/v1/kube/clusters/{uuid}/rollout` | 从云账号接入的条目：server 用云账号现取的临时凭据读集群里 agent DaemonSet 与 Pod 的状态，逐个节点判定，返回 `{done, poll_after_seconds?, desired, rows:[{node?, pod?, code, detail?}]}`；`code` 取自 `GET /kube/defaults` 的 `check_codes.verify`，判定规则与 `GET /kube/check.sh` 相同（拉取失败的 `detail` 取自 kubelet 事件）；`node` 与 `pod` 都没有的行说的是整个 DaemonSet（`DAEMONSET_MISSING`、`NO_NODE_SELECTED`、`POD_CREATE_FAILED` 等）；`desired` 是 DaemonSet 要的 Pod 数，还没算出或没有 DaemonSet 时为 null；`done` 为 false 时再等可能改变结果，客户端在 `poll_after_seconds`（10）秒后再问。只读集群，不保存。手动建的条目回 409 `KUBE_CLUSTER_NO_SOURCE`；取不到凭据或集群拒绝回 424 `KUBE_CLUSTER_OPERATION_FAILED`（`step`：`kubeconfig` 或 `api_server`，`reason`） | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/revoke` | 吊销接入令牌：已接入节点不受影响，新节点不能再接入 | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/apply` | 把集群的 manifest 应用到它的来源集群：`{dry_run}`（必填，缺省回 400 `KUBE_CLUSTER_INVALID`，不会默认执行）；server 向云账号现取短期 kubeconfig（不保存，ADR 0063），逐个对象 server-side apply，`dry_run` 时只让 API server 检查；PriorityClass 的数值或 `preemptionPolicy` 变了时 Kubernetes 不允许原地更新，server 先删后建，这一项的 `action` 是 `replace`。返回 `{dry_run, results:[{kind,name,namespace?,action(create\|update\|replace\|failed\|not_tried\|dependent),error?}]}`；云账号签不出凭据或集群拒绝时回 424，体内同样带 `results`，另有 `code: KUBE_CLUSTER_OPERATION_FAILED`、`params:{step(kubeconfig\|api_server),reason}`；手动建的条目或云账号已删除回 409 `KUBE_CLUSTER_NO_SOURCE` | JWT(admin) |
| POST | `/api/v1/kube/clusters/{uuid}/remove` | 从来源集群删除 manifest 的对象（先删 DaemonSet，再按相反顺序删其余对象；集群级对象按命名空间命名，同一集群其他条目的不受影响；`action` 另有 `delete\|absent`）；请求、响应与错误同 apply | JWT(admin) |
| DELETE | `/api/v1/kube/clusters/{uuid}` | 删除集群，返回 204；仍有节点时回 409 `KUBE_CLUSTER_HAS_NODES` | JWT(admin) |
| GET | `/kube/manifest.yaml?token=` | 凭接入令牌取 manifest，供 `kubectl apply -f`；令牌未知或已吊销回 404 | 接入令牌 |
| GET | `/kube/check.sh?token=` | 凭接入令牌取 apply 之后的检查脚本（`text/x-shellscript`），供 `curl … \| bash`：只读条目命名空间里的 `gpuworks-agent` DaemonSet，每个节点输出一行“节点 状态码 详情”（状态码见 `GET /kube/defaults` 的 `check_codes.verify`）；令牌未知或已吊销回 404 | 接入令牌 |
| POST | `/api/v1/kube/enroll` | agent 用 `{token, cluster_uid, node_name}` 换节点 token：返回 `{node_uuid, node_token, idle_return_seconds}`；同一集群的同一节点总是得到同一个节点；令牌首次使用时绑定集群 UID，之后其他集群回 403 `KUBE_ENROLL_REFUSED`（`params.reason: other_cluster`），未知或已吊销回 403（`params.reason: unknown_token`；注册过的 agent 保留自己保存的身份）；一个 Kubernetes 节点同一时间只属于一个条目：同一集群的另一个条目选中同一节点，且那个条目在该节点上的 agent 在线或 15 分钟内见过时，回 403（`params.reason: node_taken`，`params.entry` 是占着节点的条目名），之后节点可以转到另一个条目 | 接入令牌 |

## Slurm（集群接入）

一个 Slurm 集群以一个健康检查接入：管理员创建集群条目，在 Slurm 管控节点上以 root 执行返回的命令（把 `HealthCheckProgram` 写进 slurm.conf），每台 GPU 计算节点上的 slurmd 运行检查脚本，脚本在本机装好 agent，agent 用条目的接入令牌换取自己的节点 token，之后与普通节点一样；以后扩容的节点同样自己接入。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/slurm/clusters` | 条目列表：`{items:[{id,uuid,name,server_url,slurm_cluster_name,run_as,account_user,install_dir,script_dir,sbatch_args,idle_return_seconds,pending_timeout_seconds,revoked,state,stalled,node_count,online_count,slurm_gpu_nodes,list_as_of?,missing_count,missing_nodes,waiting?,shared_storage_hint?,category,resources,source?,can_apply,can_remove,operation_blocked?,delete_with_nodes_allowed,delete_with_nodes_blocker?,created_at,setup_command?,check_command?,remove_command?,uninstall_agents_command?}], total}`；这些命令含接入令牌，只给管理员，**AI 助手读这个接口时一律不含它们**，吊销过的条目没有 `setup_command` 和 `check_command`（后者是同一条命令加 `-s -- --check`：只检查、什么都不改）；`state` 与 kube 相同：`revoked`、`stalled`（没有节点接入、未吊销且创建超过 `stalled_after_seconds`）、`pending`、`active`；`slurm_cluster_name` 是 Slurm 自己的 `ClusterName`，首个节点接入时绑定，之前为空；`node_count` 是已登记的节点数，`online_count` 是其中 agent 在线的节点数，`slurm_gpu_nodes` 是已登记的 agent 最近一次上报的 `sinfo` GPU 节点数（还没有上报为 0），但不少于 `node_count`：登记过的节点都有 GPU（只在有 GPU 的节点上装 agent），而上报最长晚一个上报周期，节点刚登记时它可能还没列出来，所以不会出现已接入多于 GPU 节点，`missing_count` 是其中没有在线 agent 的节点数，`shared_storage_hint` 建议把集群的共享文件系统登记为模型存储，`{name,type,path,label_key,label_value}` 是控制台预填添加存储表单的内容（`type` 为 `shared_dir`，按 `gpuworks.ai/slurm-cluster` 标签适用于条目的节点），只给从阿里云 E-HPC 集群创建、未吊销、已有节点接入、且这些节点上还没有任何启用的共享存储（`nfs`、`nfs_cache`、`shared_dir`）的条目，目录是 E-HPC 从集群 NAS 挂到每台节点的 `/opt` 下的 `/opt/gpuworks-models`；`list_as_of`（RFC 3339）只在条目有已登记节点却没有一个 agent 在线、名单又不为空时出现：名单只由 agent 上报，这时它是 agent 离开前的那份，最多新到最后一个 agent 被看到的时间，就是这个值；`missing_nodes` 是其中按名称排序的前 `missing_nodes_limit` 个 `{name,reason,slurm_state,slurm_reason?,node_uuid?}`，`node_uuid` 只给登记过且离线的节点，`DELETE /api/v1/nodes/{uuid}` 删除它时也把它从名单里去掉（下一次上报重新存整份名单，节点还在集群里就回到名单），`reason` 为 `not_enrolled`（该节点从未登记：健康检查还没在它上面跑过，或本机没有 NVIDIA GPU）或 `offline`（登记过但 agent 不在线），`slurm_state`/`slurm_reason` 是 Slurm 对该节点的原话；`waiting` 只在还没有节点接入且未吊销时给出，是 server 对「为什么还没有节点」的判断 `{reason, hosts?}`：`joining`（看到过 setup 执行，且距今不到两个健康检查周期，即 10 分钟：健康检查在一到一个半周期内到各节点，再留半个周期，所以还没有节点联系过 server 是正常的，不是问题；这段时间里条目也不算 `stalled`）、`no_compute_nodes`（云厂商的集群列表说它没有计算节点，实时读取：E-HPC 的 `ListClusters` `Nodes` 三项计数之和为 0）、`server_unreachable`（平台最近一次检查或应用发现有主机访问不到条目的 `server_url`，`hosts` 是这些主机 `{role, name, status, reason}`）、`no_node_called`（看到过 setup 执行——有人取了接入命令的脚本，或平台应用过——已过上述时间，仍没有节点向 server 问过令牌状态或登记）、`not_applied`（没看到过 setup 执行；检查命令 `&check=1` 取脚本不算）；后三项是 server 自己看到的事，只存在内存里、不落库，重启后忘记，所以对创建早于 server 本次启动的条目不下这些结论，没有可说的原因时不给 `waiting`；`category` 为 `slurm`，`resources` 为 `[{type:"slurm"}]`，即 `GET /providers/kinds` 的 `categories` 里 Slurm 那一项；`source` 是条目所属云账号的集群 `{provider_id,provider_name,provider_kind,provider_cluster_id}`；`can_apply` 为 true 表示平台能经云账号在 Slurm 管控节点上执行 setup（有来源、未吊销，账号的驱动能在 Slurm 管控节点上执行命令，集群的调度器是 Slurm），`can_remove` 表示能把 setup 撤出集群（同 `can_apply`，但吊销过的条目也可以）；不能时 `operation_blocked` 给原因：`no_source`（手动建的条目）、`no_provider`（来源账号已删除）、`provider_unsupported`（账号的驱动不能在 Slurm 管控节点上执行命令）、`cluster_unknown`（上次同步里没有该集群的节点，云账号的集群列表里也没有它）、`cluster_listing_pending`（集群列表还在后台读取，没有更早的可用，下次轮询就有）、`cluster_listing_failed`（集群列表读取失败、密钥无权列出或缺了某个地域，不能断定集群不在账号里）、`not_slurm_cluster`（集群用别的调度器）、`revoked`（令牌已吊销，只拒绝应用）；客户端据此显示「检查并应用」、置灰并给出原因，不自己判断；集群的地域和调度器来自上次同步里它的节点，没有节点时来自云厂商的集群列表（server 在后台按账号单独读取，缓存 1 分钟，一个请求最多等 5 秒，等不到用上一次的列表，没有时是 `cluster_listing_pending`；列表轮询不会每次访问云 API，也不会因为云 API 慢而在可用和置灰之间跳；更换账号密钥或删除账号时丢弃）；`delete_with_nodes_allowed` 表示 `DELETE …?with_offline_nodes=true` 现在能成功，不能时 `delete_with_nodes_blocker` 是原因：`online`（有 agent 在线）或 `instances`（有节点上还有实例） | JWT |
| GET | `/api/v1/slurm/defaults` | 表单要用的字典：`{run_as, install_dir, script_dir, idle_return_seconds, idle_return_min_seconds, idle_return_max_seconds, pending_timeout_seconds, pending_timeout_min_seconds, pending_timeout_max_seconds, health_check_interval_seconds, stalled_after_seconds, report_interval_seconds, missing_nodes_limit, quick_tunnel_url}`；`report_interval_seconds` 是 agent 上报与取设置的间隔，即编辑设置后传到各节点最长要等多久；`quick_tunnel_url` 是平台自己此刻的临时公网地址（`gpuworks expose` 的 Cloudflare quick 入口开着且在线时的地址），没有时为空串：条目的 `server_url` 不要填它（连接器重建后会变），表单据此提示，不阻止 | JWT |
| POST | `/api/v1/slurm/clusters` | 创建条目：`{name, server_url, run_as?(root), account_user?, install_dir?(/var/lib/gpuworks), script_dir?(/opt/gpuworks-slurm), sbatch_args?, idle_return_seconds?(60), pending_timeout_seconds?(600), provider_id?, provider_cluster_id?}`，`provider_*` 要同时给（云账号的 HPC 集群，同一集群只能建一个条目，否则 409 `SLURM_CLUSTER_SOURCE_TAKEN`）；参数不可用回 400 `SLURM_CLUSTER_INVALID`（`params.field`、`params.reason`），重名回 409 `SLURM_CLUSTER_NAME_TAKEN`；返回 201 与列表里的一项（含三条命令） | JWT(admin) |
| PUT | `/api/v1/slurm/clusters/{uuid}` | 修改条目：字段都可选，省略表示不改，空字符串表示清空：`name, account_user, sbatch_args, idle_return_seconds, pending_timeout_seconds`；`server_url`、`run_as`、`install_dir`、`script_dir` 已写进节点上的脚本，不能改，带这些字段或未知字段回 400；返回 `{cluster, changed}`，`changed` 是改动字段的 JSON 名（升序）。agent 在下一次上报时取到新设置（最长 `report_interval_seconds`），集群上不用重新执行任何命令 | JWT(admin) |
| GET | `/api/v1/slurm/clusters/{uuid}/setup` | 管理员在集群上执行的命令：`{setup_command?, check_command?, remove_command, uninstall_agents_command, setup_url?, check_script_url?, default_interval_seconds}`，都在 Slurm 管控节点上以 root 执行；吊销后没有 `setup_command`、`check_command`、`setup_url`、`check_script_url`。卸载分两步：先执行 `uninstall_agents_command`，等一个健康检查周期（各节点上的 agent 在下一次检查时自行卸载），再执行 `remove_command`（还原 slurm.conf 并 `scontrol reconfigure`） | JWT(admin) |
| GET | `/api/v1/slurm/clusters/{uuid}/script` | 控制台「查看脚本」：Slurm 管控节点会拿到的 setup 脚本全文（`text/x-shellscript`，内含检查脚本），与 `GET /slurm/setup.sh` 的区别只有接入令牌，令牌在这里一律显示为 `REDACTED-ENROLLMENT-TOKEN`；吊销过的条目也能取，未知条目回 404；AI 助手读不了这个接口 | JWT(admin) |
| POST | `/api/v1/slurm/clusters/{uuid}/revoke` | 吊销接入令牌：已接入节点不受影响，新节点不能再接入；`remove_command` 与两个脚本仍然可取，以便卸载；返回列表里的一项 | JWT(admin) |
| DELETE | `/api/v1/slurm/clusters/{uuid}` | 删除条目，返回 204；仍有节点时回 409 `SLURM_CLUSTER_HAS_NODES`（节点页删除这些节点不需要 SSH）。卸载了 agent 的集群用 `?with_offline_nodes=true` 连节点一起删：没有节点在线、也没有节点上有实例时才执行，否则 409 `SLURM_CLUSTER_NODES_ONLINE`（`count`）或 `SLURM_CLUSTER_NODES_HAVE_INSTANCES`（`count`），什么都不删；只等待被移除的实例行随节点一起清掉；先吊销条目再删节点、最后删条目（吊销后健康检查不能再把刚删的节点登记回来；吊销之后某一步失败，条目保持吊销）；`with_offline_nodes` 不是单个布尔值回 400 `SLURM_CLUSTER_INVALID` | JWT(admin) |
| POST | `/api/v1/slurm/clusters/{uuid}/apply` | 由平台在集群的 Slurm 管控节点上执行 setup（阿里云 E-HPC 经云助手；Slurm 管控节点的实例 ID 来自 `ehpc:GetCluster`），仅 `can_apply` 为 true 的条目；请求体 `{"dry_run": bool}`，`dry_run` 必填（缺省、不是布尔值或不是单个 JSON 对象回 400，缺省回 `SLURM_CLUSTER_INVALID`，`params.field` 为 `dry_run`），不会因为漏写而直接应用：`true` 只做检查（见下），`false` 才应用；可重复执行（扩容出的计算节点下一次会补上 `gres.conf` 行）。先给 Slurm 自己列出的节点（`scontrol -o show node`）里配置的 `Gres=` 含 `gpu:N` 而 `gres.conf` 里没有它的 gpu 行的追加 `NodeName=<n> Name=gpu File=/dev/nvidia0`（多卡为 `/dev/nvidia[0-<N-1>]`，N 是它的 `gpu:N`，带类型和插槽后缀的写法也认；节点和卡数不依赖云账号同步到的实例，所以没同步过的新集群、名字不合 `<集群>_<主机>` 的节点照样补），已有它的 gpu 行（含 `NodeName` 表达式覆盖、`NodeName` 在行内任何位置、没有 `NodeName` 的通用 gpu 行）一律不动；再取并执行条目的 setup 脚本（写检查脚本与接入令牌文件、必要时分发到各节点、改 slurm.conf、`scontrol reconfigure`）；最后把仍因缺 GPU 被 drain 的节点 `state=resume`，管理员自己设的 drain 不动。同一 Slurm 管控节点上同时只执行一次：客户端等待超时断开后重试，上一次可能还在 Slurm 管控节点上执行，这时回 424 `SLURM_APPLY_FAILED`（`step: run`，`reason: busy`），等几分钟再重试。等待至多 6.5 分钟，成功返回 `{status, manager_instance, steps:[{id,status}], gres:{added,present,no_gpu_gres}, distribute:{shared,ok,failed}, resumed_nodes, resume_failed_nodes, reach, cluster}`：`status` 为 `ok` 或 `partial`（做完了但有要手工处理的，见各步），`steps` 依次为 `gres`、`check_script`、`distribute`、`slurm_conf`、`reconfigure`、`resume`，询问过主机时最后还有 `reach`，状态 `pass`、`warn`、`skip`；`reach` 是 server 地址（`server_url` 的 `/healthz`）能否被访问的逐主机答案 `{hosts:[{role, name, status, reason?}], nodes_unlisted?, nodes_permission?}`：`role` 为 `controller` 或 `node`，`status` 为 `ok`、`failed` 或 `unknown`（没能询问，不算失败），`reason` 对 `failed` 是 `dns`、`connect`、`timeout`、`tls`、`http`、`no_curl`、`other`，对 `unknown` 是 `not_asked`、`node_not_running`、`assistant_offline`、`command_failed`；Slurm 管控节点由 setup 脚本自己问（也问经 root ssh 够得到的节点，除非平台自己问节点），GPU 计算节点由平台经云助手逐个问（节点清单来自 E-HPC 的 `ListNodes`，只问有卡且在运行的节点，最多 8 个并发、总共 90 秒，与 Slurm 管控节点上的执行同时进行），`nodes_unlisted` 是列不出计算节点的原因（`permission_missing`，此时 `nodes_permission` 是缺的权限，如 `ehpc:ListNodes`；或 `failed`），有主机 `failed` 时 `reach` 步为 `warn`、`status` 为 `partial`；`no_gpu_gres` 是云账号同步列出有卡、而 Slurm 的 `Gres=` 里没有 `gpu` 的节点名（没有追加：补了 slurmd 会拒绝这些节点；没同步到的节点没有这一项）。不能执行时 409 `SLURM_APPLY_NOT_APPLICABLE`（`reason`：`no_source`、`no_provider`、`not_slurm_cluster`、`cluster_unknown`（最近一次同步里没有该集群的节点）、`cluster_listing_pending`、`cluster_listing_failed`（见条目列表）、`provider_unsupported`、`revoked`）；其余失败 424：`SLURM_APPLY_PERMISSION_MISSING`（`permission`，如 `ehpc:GetCluster`、`ecs:RunCommand`）、`SLURM_APPLY_MANAGER_UNKNOWN`、`SLURM_APPLY_FAILED`（`step`：`cluster`、`assistant`、`run`、`gres`、`setup`、`resume`；`reason` 是脚本给的稳定词，如 `busy`、`server_unreachable`、setup 预检的 `not_root`、`slurmctld_unreachable`、`conf_unreadable`；`output` 是 Slurm 管控节点上最后几行输出；`reason` 为 `server_unreachable` 时另有 `url`（Slurm 管控节点访问不到的管理节点接入地址，不含一次性地址的随机码）和 `kind`（`dns`、`connect`、`timeout`、`tls`、`http`、`no_curl`、`other`，取自 curl 的退出码））。**`dry_run: true` 只检查，什么都不改**：不追加 `gres.conf` 行、不写文件、不分发、不改 slurm.conf、不 `scontrol reconfigure`、不 resume 节点，用的是同一份只读逻辑，所以它报告的就是随后 `dry_run: false` 要做的事；失败和拒绝的错误码同应用（Slurm 管控节点打印的不是计划同样回 424 `SLURM_APPLY_FAILED`），成功返回 `{dry_run: true, status, manager_instance, slurm_cluster, gres:{would_add:[{node,files,line}],present,no_gpu_gres}, script:{check_script,token_file,shared,fstype,copy_to,copy_files}, conf:{path,changed,interval,node_state,block,chained_program?,chained_state?,commented:[{line,key,value}],includes,unnamed_program?}, configless, rearm, resume_nodes, warnings, reach, cluster}`：`would_add` 是会追加的 gres 行（`line` 是写进 `gres.conf` 的原文），`shared` 为 false 时 `copy_to` 是会复制到的节点、`copy_files` 是文件，`conf.block` 是会加进 slurm.conf 的块里的三行设置原文，`conf.changed` 为 false 表示 slurm.conf 已经是 setup 会写成的样子，`commented` 是会按原位注释掉的集群原有 `HealthCheck*` 设置（它的程序由检查脚本接着调用，`chained_program`、`chained_state`），`includes`、`unnamed_program` 是 setup 看不到的 `Include` 文件和 slurmctld 在跑而 slurm.conf 没写的程序，`configless` 为 false 时 slurm.conf 要自己复制到各节点，`rearm` 为 true 表示会取消之前武装的 agent 卸载，`resume_nodes` 是会被 resume 的节点；`warnings` 是代码，各自会让 setup 以 `partial` 结束：`conf_include`、`program_not_in_conf`、`not_configless`、`no_gpu_gres`、`server_unreachable`（有主机访问不到 server 地址，`reach` 说是哪些，读取方式同上，只读），`status` 有警告时为 `partial`，否则 `ok`；只读脚本打印的 `GWKS_APPLY`、`GWKS_SLURM` 标记行。云助手的命令里只有一次性 setup 地址（`/slurm/setup.sh?once=<码>`，随机码只存在 server 内存里，十分钟内有效、取一次即作废，apply 结束时没用掉的也作废），接入令牌只出现在 Slurm 管控节点下载到的脚本正文里（由它写进仅 root 可读的令牌文件），所以云账号里保留 30 天的调用记录里没有令牌；卸载的两步（`remove`）同样 | JWT(admin) |
| POST | `/api/v1/slurm/clusters/{uuid}/remove` | 卸载的两步由平台在 Slurm 管控节点上执行，条件同 apply（吊销过的条目也可以）：`{"step":"uninstall_agents"}` 让各节点的检查脚本在下一次运行时卸载 agent，返回 `wait_seconds`（一个健康检查周期）与 `next_step: "remove_check"`；等够之后 `{"step":"remove_check"}` 还原 slurm.conf 并 `scontrol reconfigure`（没有可还原的返回 `status: "noop"`）。返回 `{step, status, manager_instance, wait_seconds, next_step?, cluster}`；`step` 不认识回 400 `SLURM_CLUSTER_INVALID`；失败码同 apply | JWT(admin) |
| GET | `/slurm/setup.sh?token=` | 凭接入令牌取 Slurm 管控节点上执行的 setup 脚本（`text/x-shellscript`；命令行选项 `--remove`、`--uninstall-agents`、`--interval N`、`--check`（只检查，什么都不改，打印同样的 `GWKS_SLURM` 标记，`status=plan` 代替 `status=ok`，再加 `install_mode`、`distribute_plan`、`conf_comment`，最后 `step=done mode=check`；不能和 `--remove`、`--uninstall-agents` 同用）；每步输出一行 `GWKS_SLURM key=value` 标记）；令牌未知回 404，吊销过的令牌仍可取（卸载要用）；平台自己在云集群 Slurm 管控节点上执行时改用 `?once=<码>`（一次性，见 apply），码未知、已用或过期回 404 | 接入令牌 / 一次性码 |
| GET | `/slurm/check.sh?token=` | 凭接入令牌取检查脚本（setup 也会把它写到脚本目录）；脚本里没有令牌，令牌由 setup 写到脚本目录的 `token` 文件（仅 root 可读），检查脚本读它；同上 | 接入令牌 |
| GET | `/slurm/token-status?token=` | 检查脚本在安装 agent 前问的一句话：`{"status":"active"|"revoked"|"unknown"}`，只此而已；吊销过的令牌不再装新 agent（已接入的 agent 照常运行） | 接入令牌 |
| POST | `/api/v1/slurm/enroll` | agent 用 `{token, slurm_cluster_name, node_name}` 换节点 token：返回 `{node_uuid, node_token, settings:{sbatch_args,idle_return_seconds,pending_timeout_seconds,account_user}}`；同一条目的同一 Slurm 节点总是得到同一个节点；令牌首次使用时绑定 `slurm_cluster_name`，之后其他 Slurm 集群回 403 `SLURM_ENROLL_REFUSED`（`params.reason: other_cluster`），未知或已吊销回 403（`unknown_token`），同一 Slurm 集群的另一个条目在 15 分钟内持有该节点回 403（`node_taken`，`params.entry` 是持有者），接入守卫拒绝新节点回 403（`enrollment_refused`，`params.refusal_code` 是守卫的码；agent 不退回已存身份，最多每分钟重试一次，有余量后接入；已接入的节点重新接入不经过守卫） | 接入令牌 |
| POST | `/api/v1/slurm/gpu-nodes` | 已登记的 agent 上报本 Slurm 集群的节点（`sinfo -a`，含隐藏分区）：`{digest, state_digest, nodes?:[{name,listed_only?,state,reason?,gpus}]}`，凭节点自己的 token（`Authorization: Bearer`）。`digest` 是成员（节点名、是否 GPU 节点、GPU 数）的摘要，`state_digest` 是 GPU 节点状态与原因的摘要；`listed_only` 的节点没有 `gpu` GRES，只记名字（让「不在名单里」等于「离开了集群」，也不计入 GPU 节点数）。server 记着每个条目最近存下的两个摘要，都相同则只回设置，不读库；不同时（且该条目一分钟内没写过库）回 `need_nodes: true`，agent 再带 `nodes` 上报（空数组表示集群没有节点）；成员变了才整表替换，只有状态或原因变了只改变了的那几行，没变则不写库；返回 `{need_nodes, settings}`；token 不是 Slurm 节点的回 401 | 节点 token |

## Models（模型）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/models` | 模型列表；支持 `page`/`page_size`、`q`、`source`（`huggingface/modelscope/url/local`）、`operation`、`source_id`；后三项可用逗号分隔，`source_id` 整体匹配、不区分大小写。`sort` 为 `name/source/source_id/published_at/created_at/weight/presence`。分页返回 `presence{ready_nodes,cached_nodes,ready_storages,in_flight,in_flight_percent,failed}` 就位汇总及全量 `summary`；逐位置明细查模型详情。见[列表筛选与就位汇总](#列表筛选与就位汇总) | JWT |
| GET | `/api/v1/models/presence?ids=1,2` | 只读取指定模型的就位摘要，用于下载进度更新。`ids` 必填，逗号分隔的正整数，最多 200 项，重复编号去重；返回以模型编号为键的对象，每项形状同列表的 `presence`。已知但无副本的模型各项为 0；不存在或已删除的编号省略。非法、空或超量参数返回 400，存储读取失败返回 500。普通进度事件只刷新当前页摘要，状态或模型变更继续刷新完整列表 | JWT |
| GET | `/api/v1/models/catalog` | 在线模型目录。通过 `source`（`modelscope` 或 `huggingface`）和 `path` 请求公开模型列表、README 或 ModelScope 默认分支提交。管理节点按来源规则访问，返回源站正文；Hugging Face 续页信息在 `X-Catalog-Link`。 | JWT |
| POST | `/api/v1/models` | 注册公共仓库模型（HF/ModelScope）。来源站点明确回答没有该仓库或该版本时返回 422 `MODEL_REPO_NOT_FOUND` / `MODEL_REVISION_NOT_FOUND`，不登记；请求体带 `force: true` 时仍登记。站点不可达、拒绝访问（401/403）或只有镜像站回答 404 时照常登记（201），原因写在 `metadata_notice` | JWT |
| POST | `/api/v1/models/preview-url` | 预览 HTTP 模型目录（`url`）；必须为带主机名的 http/https URL，不允许内嵌凭据。非法地址返回 400 `MODEL_SOURCE_URL_INVALID`，合法但不可读取的目录返回 502 `HTTP_DIRECTORY_UNREACHABLE` | JWT |
| POST | `/api/v1/models/register-url` | 注册 HTTP 模型目录（`source_url`，可选 `source_id`、`name`）；地址校验与 preview-url 相同，注册成功返回 201 | JWT |
| POST | `/api/v1/models/local/preview` | 预览节点本地目录是否可注册为模型 | JWT |
| POST | `/api/v1/models/local` | 注册节点本地模型 | JWT |
| GET | `/api/v1/models/{id}/logo` | 模型 logo 图片 | JWT |
| GET | `/api/v1/models/{id}` | 模型详情 | JWT |
| PATCH | `/api/v1/models/{id}` | 修改本地 / HTTP 目录模型的来源 ID（`{"source_id"}`，详情的 `source_id_editable` 表示可改）；公共仓库模型 400 `MODEL_SOURCE_ID_FIXED`，与已有模型重名 409 `MODEL_SOURCE_ID_TAKEN`，有进行中的复制、下载或删除 409 `MODEL_TRANSFER_ACTIVE`；已有副本路径不变 | JWT |
| DELETE | `/api/v1/models/{id}` | 删除模型 | JWT(admin) |
| POST | `/api/v1/models/{id}/metadata/refresh` | 重新拉取模型元数据（config/README/logo）。什么都没读到时按来源站点的回答报错：404 `MODEL_REPO_NOT_FOUND` / `MODEL_REVISION_NOT_FOUND`、403 `MODEL_REPO_ACCESS_DENIED`（`params.host`、`params.repo`、`params.status` 为站点与它的 HTTP 状态码）；站点没有给出关于仓库的回答时 502 `MODEL_METADATA_UNREACHABLE` 或 `MODEL_METADATA_FORBIDDEN` | JWT |
| POST | `/api/v1/models/{id}/download` | 发起公共仓库下载：`storage_id` 选存储（省略为默认存储），本地存储再给 `node_uuid` 或 `node_uuids`，共享存储由一台写入节点下载一次 | JWT |
| POST | `/api/v1/models/{id}/download/cancel` | 取消指定存储的模型下载（`storage_id`，本地存储另带 `node_uuid`）；保留已下载数据以供重试 | JWT |
| POST | `/api/v1/models/{id}/copy` | 在两个位置之间复制模型文件：`source`/`target` 各为 `{storage_id, node_uuid?}`；同一节点能读源写目标时本机复制，否则节点间直传；源与目标为同一位置返回 400，共享存储按存储 ID 判定（忽略节点）；离线或不可写等状态冲突仍返回 409 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/cache/retry` | 重试失败的 NFS Cache 复制（`preparation.retryable=true`），请求体 `{storage_id}`，接受返回 202。非失败任务、节点未连接、容量不足、旧任务未确认退出或并发变化返回 409 `MODEL_CACHE_RETRY_BLOCKED` | JWT |
| DELETE | `/api/v1/models/{id}/storages/{storage_id}` | 从共享存储删除模型文件（及 NFS 加缓存的各节点缓存）；别的 gpuworks 在用时 409 `STORAGE_SHARED_BY_OTHER_CONTROL_PLANES`，`?force=1` 强制 | JWT(admin) |
| POST | `/api/v1/models/{id}/register-path` | 将节点已有路径登记为该模型的本地文件 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync` | 从显式选定的源节点做节点间直传同步（`source_node`、可选 `rate_limit_kib_s`；不带源节点时沿用失败记录的源节点重试） | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/probe` | 测试源节点到目标节点的直连握手，不传字节 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/cancel` | 取消节点间同步 | JWT |
| DELETE | `/api/v1/models/{id}/nodes/{uuid}` | 驱逐节点上的模型文件（本地存储或节点缓存，`?storage_id=` 指定存储）（拒绝有活跃 deployment 占用的节点）；该路径上进行中的下载先被取消并等其结束，返回后可立即重新下载 | JWT(admin) |

自动下载、复制及部署获取在同一存储、同一节点上为不同模型保留独立目录。空 `revision` 仍表示来源的默认分支，与显式 `main` 是不同身份；若历史默认目录已被另一模型占用，新副本使用带 `-model-<模型ID>` 后缀的目录，重试保留该目录。节点回报绝对路径后仍检查占用；后缀目录也被占用时拒绝写入，下载、复制和同步接口返回 409 `MODEL_FILE_PATH_OCCUPIED`（`storage_id`、`path`、`model_id`）。既有副本不会自动搬迁，手动登记仍使用调用方指定的目录；该检查不识别不同存储定义或符号链接指向同一物理目录的情况。

### 列表筛选与就位汇总

模型详情的本地节点行与共享存储行可带 `transfer_error_code`，列文件阶段的终态分类为：`MODEL_DOWNLOAD_NETWORK_EXHAUSTED`（瞬时网络或上游错误重试耗尽）、`MODEL_DOWNLOAD_SOURCE_NOT_FOUND`（仓库或版本不存在）、`MODEL_DOWNLOAD_SOURCE_ACCESS_DENIED`（401/403）、`MODEL_DOWNLOAD_SOURCE_THROTTLED`（持续限流）、`MODEL_DOWNLOAD_SOURCE_REJECTED`（其他确定的来源拒绝）。`message` 保留原始平台和地址错误详情，页面同时展示分类与详情。旧 agent、未知错误或多个来源失败类型不一致时可省略该字段；省略不代表成功。重新发起下载会清理旧下载错误码、分配新的传输 ID，迟到的旧任务回报不能覆盖新任务。

`GET /api/v1/models` 不带分页参数时返回全部模型及逐节点、存储的就位明细。
带 `page` 或 `page_size` 时返回 `{items, page, page_size, total, summary}`：

- 每行省略 `readme`、`nodes`、`storages`，改用 `presence{ready_nodes, cached_nodes, ready_storages, in_flight, in_flight_percent, failed}` 汇总就位情况；明细查 `GET /api/v1/models/{id}`。
- `summary{registered, ready_files, sources, weight_gib}` 统计全部模型，不受筛选影响。

查询参数按下面的含义使用：

| 参数 | 用法 |
|---|---|
| `q` | 名称、来源 ID 或修订的子串 |
| `source` | `huggingface`、`modelscope`、`url`、`local`，多个值用逗号分隔 |
| `operation` | 能力类型，多个值用逗号分隔，满足任意一个即可 |
| `source_id` | 来源 ID 整体匹配，不区分大小写；多个值用逗号分隔，可按模板权重源查已注册模型 |
| `sort` | `name`、`source`、`source_id`、`published_at`、`created_at`、`weight`、`presence`；`published_at` 是源站创建时间，缺失时取最近更新；`presence` 按就位节点数排序 |

### 登记节点已有路径

`POST /api/v1/models/{id}/register-path` 请求为
`{"node_uuid":"…","path":"/data/existing-model","storage_id":1}`。
`path` 是节点已有目录的宿主机路径，调用方应提供绝对路径；允许位于所选本地存储根目录之外。
`storage_id` 选择记录归属，不是路径范围限制；省略时优先采用该模型在节点上已有副本的存储，否则取默认本地存储。
该操作只登记已有文件，不复制或搬迁目录。共享存储的库导入使用存储扫描/导入接口。

成功返回 `200 {"status":"registered"}`。节点检查目录可读、有非空文件且没有未完成下载；
目录元数据必须识别出恰好一个完整模型入口，GGUF 等自带元数据的格式不要求 `config.json`；父目录、非模型目录和未知布局返回 409。`registered` 表示调用方选定的模型目录已登记，
不保证引擎可以加载该模型。不能用“路径在存储范围外”判断请求无效。

空 `path` 返回 400；目标模型不存在返回 404；缺失、空目录或含未完成 `.part`/`.part.state` 的目录由节点拒绝（同名文件已完整、大小等于 `.part.state` 记录的残留不计），
接口返回 409，已有登记不被替换。agent 过旧、无法返回目录元数据识别结果时，返回 `AGENT_UPGRADE_REQUIRED`，需先升级 agent。真实节点 RPC 不可用等错误也可能返回 409，调用方应结合错误内容处理。

## Images / Storages（镜像目录与存储策略）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/images` | 镜像系列列表 | JWT |
| GET | `/api/v1/images/versions` | 镜像版本行列表；每行带 `present_nodes`/`total_nodes` 与 `reference_count`，不带逐节点行（节点行见系列详情） | JWT |
| GET | `/api/v1/images/engines` | 镜像表单可选的引擎字典 `[{"name","label"}]`，按展示顺序：全部部署引擎加目录专用的 `vllm-omni` | JWT |
| GET | `/api/v1/images/community/repos` | 社区镜像仓库卡片（内置六类与 `image_community_catalog` 设置合并后的结果，不含 tag） | JWT |
| GET | `/api/v1/images/community/browse` | `?repo=` 列出该仓库上游的全部 tag（任意仓库均可，不做 semver 过滤；内存缓存 1 小时）；仓库名为空或格式错误返回 400，上游报告仓库不存在返回 404 | JWT |
| PATCH | `/api/v1/images/versions/{id}` | 修改镜像版本行：只接受 `enabled` 与 `registry_sources`（系统版本也可加获取地址）；其他键回 400 `IMAGE_VERSION_PATCH_INVALID`，镜像目录不承载启动配置（ADR 0031） | JWT(admin) |
| GET | `/api/v1/images/{id}` | 镜像系列详情；各版本的节点行带 `node_name`、`node_state`、`image_digest_id`、`updated_at`，节点间同步来的行另带 `source_node_name`。`state` 只表示节点运行时里有没有该镜像；上一次获取的结局看 `transfer_error_code` 与 `message`：`present` 且 `transfer_error_code=IMAGE_REFETCH_FAILED` 表示强制重新获取失败、节点保留了原有镜像，`message` 是失败原因，保留到下一次获取；`PEER_TRANSFER_CANCELED` 表示节点间传输被操作员取消；`PEER_DIRECT_CONNECT_FAILED` 表示源节点连不上目标节点且未经中继，`message` 是连接错误 | JWT |
| PATCH | `/api/v1/images/{id}` | 修改镜像系列：`enabled`；社区/自定义系列另可改 `common_args`/`common_env`（系统系列 400 `OFFICIAL_IMAGE_SERIES_MANAGED`） | JWT(admin) |
| DELETE | `/api/v1/images/{id}` | 删除社区/自定义镜像版本；系统镜像 400 `OFFICIAL_IMAGE_DELETE_FORBIDDEN`，仍被部署、未结束的实例或模型引用时 409 `IMAGE_IN_USE`（与系统镜像删除/弃用同一引用口径） | JWT(admin) |
| DELETE | `/api/v1/image-series/{id}` | 删除社区/自定义镜像系列；任一版本仍被引用时 409 `IMAGE_IN_USE` | JWT(admin) |
| POST | `/api/v1/images/import` | 两种请求体：带 `schema_version` 的系统目录 manifest（schema 2/3，其他值 400），或社区/自定义添加用的 `{"series":[...]}`（不能写 `source=official`） | JWT(admin) |
| POST | `/api/v1/images/sync` | 从发布通道更新系统镜像目录；忽略请求体，返回版本计数与系列。当前目录是手工上传的且与渠道目录不同时，须带 `?replace_uploaded=true` 才替换。错误见[目录导入结果](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/images/preview-archive-url` | 下载前解析 `docker save` 归档 URL：`{"url"}` → `url`、`size_bytes`、`repo_tags`、`previewable`，可选的 `metadata_by_tag` 按引用返回引擎、框架、各自版本和宿主架构 | JWT(admin) |
| GET | `/api/v1/images/nodes/{uuid}/runtime-images` | 扫描带标签的节点镜像。可选查询参数 `namespace`；返回镜像引用、不可变标识、大小及来源和部署用的 namespace。 | JWT(admin) |
| POST | `/api/v1/images/nodes/{uuid}/runtime-images/inspect` | 检查所选镜像的配置并回填目录字段。将同一选择和 `node_uuid` 作为 `runtime_image` 随单个自定义版本提交到 `POST /api/v1/images/import`。 | JWT(admin) |
| POST | `/api/v1/images/nodes/{uuid}/preview-archive-path` | 在所选节点预读本地镜像 tar，输入 `{"archive_path":"/absolute/file.tar"}`，返回 `repo_tags` 和可选的 `metadata_by_tag`，用于回填所选引用及其配置。`IMAGE_ARCHIVE_PREVIEW_FAILED` 的 `params.reason` 区分文件不存在、权限不足、文件类型错误、归档无效和超时。 | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync` | 触发节点镜像获取：空请求体按[拉取设置](../how-to/configure-pull-settings.md)拉取（与部署启动同一份候选，不能指定仓库）；`source: url` / `url-relay` 从 HTTP 归档导入；管理员可用 `source: local` 和计算节点上的绝对路径 `archive_path` 加载本地文件，显式传 `force: true` 可替换已存在的镜像，同一节点的活动传输继续复用。节点已持有镜像时，普通本地导入返回 202、状态 `present` 且无传输 ID。节点加载启动失败返回 409 `IMAGE_ARCHIVE_LOAD_FAILED` 和分类原因 `params.reason`。路径或来源组合无效返回 400 `IMAGE_ARCHIVE_PATH_INVALID`；或从 `source_node` 节点间直连同步（可选 `rate_limit_kib_s`，不受同步组限制）。`source` 取 `""`、`default`、`url`、`url-relay`、`local` 以外的值回 400 `IMAGE_SOURCE_REMOVED`；来源规则让该节点无处可拉时回 409 `IMAGE_SOURCE_FORBIDDEN`；`"force":true` 时节点上已 `present` 也重新拉取 | JWT（本地文件需 admin） |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/probe` | 测试 `source_node` 到目标节点的直连握手，不传字节 | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/retry` | 重试节点镜像获取；节点间同步沿用记录的源节点与上次限速。重试本地导入需管理员重新提供 `source: local` 和 `archive_path`，空请求重试上次的本地文件返回 400 `IMAGE_ARCHIVE_PATH_REQUIRED` | JWT（本地文件需 admin） |
| POST | `/api/v1/images/{id}/nodes/{uuid}/cancel` | 取消该节点上进行中的镜像传输 | JWT |
| DELETE | `/api/v1/images/{id}/nodes/{uuid}` | 从节点删除该镜像 | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/inspect` | 读取节点上镜像的 OCI 配置（`entrypoint`/`cmd`/`env`/`volumes`）与镜像 id；配置只在响应里返回，不写回目录 | JWT(admin) |
| GET | `/api/v1/storages` | 存储列表（类型、路径或导出、节点汇总与问题节点、模型数、在用部署）；本地存储的 `nodes.on_shared_fs` 是目录落在共享文件系统上的节点数，`?all_nodes=1` 时这些节点的状态带 `shared_fs:{type,source?}`（如 `nfs4`、`10.0.0.5:/opt`；mountinfo 没说时 `type` 为 `network`） | JWT |
| GET | `/api/v1/storages/{id}` | 单个存储；`?all_nodes=1` 返回每个节点的状态 | JWT |
| POST | `/api/v1/storages` | 添加存储；NFS 类先在一台节点上试挂，写不进去的保存为停用（只读）。校验错误按原因区分，均为 400：`STORAGE_PATH_INVALID`（路径格式不对，`params.path`）、`STORAGE_PATH_DENIED`（位于 `storage_denied_paths` 下，`params.path`）、`STORAGE_PATH_REQUIRED`（缺目录）、`STORAGE_CACHE_LIMIT_NEGATIVE`（缓存上限为负）；修改、试挂与节点覆盖同样适用 | admin |
| PATCH | `/api/v1/storages/{id}` | 修改名称、启用、默认、节点范围、路径、挂载选项、缓存目录与上限、`nfs`↔`nfs_cache` 类型；`nfs_cache` 改为 `nfs` 时节点缓存须先清空，否则 409 `STORAGE_CACHE_NOT_EMPTY`（`params`：`entries`、`nodes`、`transferring`、`node_uuids`）；可带读取时的 `expected_updated_at` 防止覆盖并发修改，版本变化返回409 `STORAGE_CONFIG_CHANGED` | admin |
| DELETE | `/api/v1/storages/{id}` | 移除存储对象（不删文件），并从模型列表移除只存放在此存储上的模型；有部署绑定、或部署所用模型只存放在此存储上时 409 `STORAGE_IN_USE` | admin |
| POST | `/api/v1/storages/probe` | 在一台节点上试挂一个存储定义，不留下挂载。结果含实际 `node_uuid`、`node_name`、`target`（本次实际检查的 export 或目录 path）及 `checked_at`（控制面收到并解码结果的时间），只说明这台节点这次检查的结果；旧版本响应可缺后两字段 | admin |
| GET | `/api/v1/storages/{id}/control-planes` | 共享存储上还有哪些 gpuworks 在用（心跳） | JWT |
| PATCH | `/api/v1/storages/{id}/node-overrides` | 原子修改固定 `node_uuids` 的缓存例外；`cache_path`/`cache_max_bytes` 缺省保持、null继承，限额0表示不限；新例外不扩大节点范围 | admin |
| POST | `/api/v1/storages/{id}/cache/reconcile` | `{node_uuids,mode:"trim"或"clear"}` 提交后台缓存整理，202返回accepted数量；逐节点结果见存储详情cache状态 | admin |
| GET | `/api/v1/storages/{id}/model-files` | 存储里的模型文件：共享存储每个模型一行，本地存储与 `nfs_cache` 的节点缓存每台节点一行（含最近使用时间与绑定的部署） | JWT |
| POST | `/api/v1/storages/{id}/scan` | 扫描共享存储里的模型目录，分页（`cursor`） | admin |
| POST | `/api/v1/storages/{id}/import` | 导入扫描到的目录，只写数据库 | admin |
| DELETE | `/api/v1/storages/{id}/cache` | 淘汰 `nfs_cache` 存储所有可淘汰的节点缓存，`?node_uuid=` 限一台；返回 `removed`、`freed_bytes` 与 `failed`（每项 `node_uuid`、`node_name`、`message`）；有失败时 409 `STORAGE_CACHE_CLEAR_FAILED`（`params`：`failed` 节点数、`node` 首个节点名），离线且没有这份缓存的节点不算失败 | admin |
| GET | `/api/v1/storages/{id}/files` | 浏览存储里的文件（[ADR 0053](../adr/0053-storage-files.md)）：`?path=`（相对存储根，空为根）`&cursor=&limit=`（每页至多 500，按名字字节序，`next_cursor` 为空即最后一页）；返回 `{path, node_uuid, writable, items[{name, path, dir, symlink, size, mtime, writable}], next_cursor}`。顶层 `writable` 表示当前目录能否新建子项，条目 `writable` 表示能否改名/删除；符号链接只列出、不跟随。共享存储可省略 `node_uuid`（自动选在线、ready、agent 支持且数据通道已连接的节点，优先可写节点），本地存储必填，否则 400 `STORAGE_FILE_NODE_REQUIRED`；`nfs_cache` 浏览的是 NFS 本身，不是节点缓存 | admin |
| GET | `/api/v1/storages/{id}/files/download` | 下载文件或目录：`?node_uuid=&path=&format=zip\|tar.gz`。文件原样返回（`Content-Length`、`application/octet-stream`）；目录边打包边传（默认 zip，`application/zip` 或 `application/gzip`，无 `Content-Length`），符号链接跳过并在包内 `SKIPPED_SYMLINKS.txt` 列出；`Content-Disposition: attachment` 带文件名（目录加 `.zip`/`.tar.gz`）。开始传字节之前的错误返回 JSON，之后出错直接断开连接。存储停用时仍可下载 | admin |
| PUT | `/api/v1/storages/{id}/files/upload` | 单请求流式上传：`?node_uuid=&path=&extract=tar\|tar.gz\|zip&overwrite=true\|false`，请求体为文件字节（`application/octet-stream`）。`path` 不解压时是目标文件，解压时是解压到的目标目录。server 不落盘、不缓存，`Content-Length` 作为预期大小（缺省为未知）；agent 先写入 `.gpuworks/incoming/` 暂存，完成（含解压）后原子改名到目标再回结果，返回 `{path, bytes, files}`。所有不需要 agent 的检查都在读请求体之前完成 | admin |
| POST | `/api/v1/storages/{id}/files/mkdir` | `{node_uuid, path}` 新建目录（含缺失的上级），已存在的目录不报错；返回 `{path}` | admin |
| POST | `/api/v1/storages/{id}/files/rename` | `{node_uuid, from, to}` 改名或移动，两端都须在可写区域、目标不得存在；返回 `{path}` | admin |
| DELETE | `/api/v1/storages/{id}/files` | `?node_uuid=&path=` 删除文件或目录：先移进存储的回收目录，后台删除字节；204 | admin |
| GET | `/api/v1/storages/settings` | 控制面显示名与缓存盘保留空间 | JWT |
| PATCH | `/api/v1/storages/settings` | 修改上述设置 | admin |
| GET | `/api/v1/storages/usage` | 各节点模型盘用量统计 | JWT |

### 登记节点已有镜像

管理员先调用 `GET /api/v1/images/nodes/{uuid}/runtime-images`。`list` 为响应，`selectedIndex` 为用户选择的行。读取该行的 `ref`、`identity`，再从列表顶层读取 `runtime`、`namespace` 和 `target_namespace`。Docker 的两个 namespace 均为空字符串。nerdctl 的 `namespace` 是来源，`target_namespace` 是该节点的部署目标。

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

将 `inspectBody` 提交到同一节点的 `POST .../runtime-images/inspect` 读取元数据。补齐目录字段后，将 `importBody` 提交到 `POST /api/v1/images/import`。`nodeUUID` 与 GET 路径中的节点相同。示例中的引擎、框架和版本应使用该镜像的实际值。需要跨 namespace 复制时，导入请求返回已登记的版本，节点在后台复制。通过节点分布查看传输状态、已复制字节或取消任务。

### 存储配置与缓存状态

- `updated_at` 用作编辑版本；target 的 `override_only: true` 表示纯节点配置例外，不参与范围选择。省略或为 `false` 时保留显式选择语义。
- NFS 缓存节点可带 `cache{used_bytes,reclaimable_bytes,protected_bytes,limit_bytes,state,reason?,message?}`。
  `state` 为 `pending`、`running`、`failed`、`offline`、`blocked`、`satisfied` 或 `unavailable`。
- `cache` 缺省表示统计未知，不能当成 0；新目录未验证时，不用旧目录的磁盘容量代替。

### 存储文件操作的范围与错误

上传、新建目录、改名、删除只允许在 `files/` 与 `.gpuworks/apps/` 之下进行；这两个区域目录本身不能删除或改名。

| HTTP 状态 | 错误码 | 含义 |
|---|---|---|
| 403 | `STORAGE_FILE_REGION_READONLY` | 路径不在可写区域，或试图修改区域目录本身 |
| 400 | `STORAGE_FILE_PATH_INVALID` | 路径含 `..`、反斜杠，或解析后穿出存储根 |
| 404 | `STORAGE_FILE_NOT_FOUND` | 路径不存在 |
| 409 | `STORAGE_FILE_EXISTS` | 目标已存在 |
| 409 | `STORAGE_FILE_AGENT_UNSUPPORTED` | 节点 agent 不支持文件操作，`params.node` 指出节点 |
| 422 | `STORAGE_FILE_EXTRACT_FAILED` | 解压失败 |
| 503 | `STORAGE_FILE_DATA_LANE_UNAVAILABLE` | 上传或下载时节点数据通道未连接，可重试 |
| 502 | `STORAGE_FILE_OPERATION_FAILED` | 其他节点侧操作失败 |

还可能返回 409 `STORAGE_DISABLED`（写操作）、`STORAGE_NOT_WRITABLE`、`NODE_OFFLINE` 或 `STORAGE_UNAVAILABLE_ON_NODE`。

### 目录导入结果

`POST /images/sync` 与带 `schema_version` 的 `POST /images/import` 导入系统目录，返回
`{catalog_version, created, updated, deleted, deprecated, skipped:[], series:[...]}`。
计数按版本行；`series` 是导入后的系统系列。manifest 未列出的系统版本，无引用时删除，有引用时弃用。
社区/自定义导入返回受影响的系列 `{items:[...], total}`。

失败时不修改目录，按以下错误处理：

- 发布源均不可达：502 `UPDATE_SOURCES_UNAVAILABLE`；有索引但没有与索引一致的目录文件：502 `UPDATE_DOWNLOAD_FAILED`。
- 通道未发布目录：404 `UPDATE_NOT_PUBLISHED`。
- 内容非法：400 `IMAGE_MANIFEST_INVALID`；与社区/自定义行同名或同 ref 冲突：400 `IMAGE_IMPORT_CONFLICT`。
- 上传旧版服务的兼容目录时返回 400 `IMAGE_MANIFEST_LEGACY_PROJECTION`。响应中的 `params.path` 为 `image_catalog_v4.path`，请下载签名渠道索引中该字段指向的文件再上传。

`POST /images/sync` 遇到手工上传的目录且渠道目录与它不同、又没带 `replace_uploaded=true` 时返回 409 `IMAGE_CATALOG_UPLOADED`（`params.current`、`params.latest` 是两个目录版本），目录不变。

版本行的 `rolling_tag`、`deprecated`、`deprecated_since` 描述滚动 tag 与弃用状态；
`reference_count` 统计引用该 ref 的部署、未结束实例与模型数。
格式与导入规则见[镜像目录与推理镜像家族](image-families.md)。

## Inference Templates（推理目录/模板）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/inference-templates` | 推理模板列表。在线目录可同时传 `weight_provider`、`weight_repo`、`weight_revision`，仅返回权重来源、仓库 ID 和提交版本均匹配仓库根目录的规格。 | JWT |
| DELETE | `/api/v1/inference-templates?model_id=…` | 按 model_id 删除自定义模板及全部档位。存在部署或其他模板引用时返回 409，并保留全部档位。 | JWT(admin) |
| GET | `/api/v1/inference-templates/index` | 模板索引（轻量列表，用于选择器） | JWT |
| GET | `/api/v1/inference-templates/organizations` | 模板涉及的组织/发布方列表 | JWT |
| GET | `/api/v1/inference-templates/chips` | 模板声明支持的静态加速卡目录；可选 `arch_id` 表示模板需要的架构 ID，其他厂商字段保持原样 | JWT |
| GET | `/api/v1/inference-templates/my-chips` | 当前集群实际探测到的加速卡列表，含 `vendor`、`name`、可选 `cc` 和已报告的 `arch_id`；未报告架构时省略 `arch_id` | JWT |
| GET | `/api/v1/inference-templates/summary` | 模板统计摘要 | JWT |
| GET | `/api/v1/inference-templates/weight-models` | 某个 spec 声明的权重来源对应的已登记模型（`template_id`、`spec_source`、`spec_name` 必填，缺一 400 `TEMPLATE_WEIGHT_MODELS_QUERY_INVALID`，spec 不存在 404）：`models` 为服务任一来源的模型（按 repo id 不区分大小写匹配，完整仓库可服务子目录请求，新的在前，各一次，行格式同分页列表；按 rank 拆分的来源只列 rank 0 的制品，且 revision 已知时须与来源写的一致）；`sources` 按声明顺序列出各来源，`reuse_model_id` 是选择该来源时复用的同 provider 最新模型 | JWT |
| GET | `/api/v1/inference-templates/weight-source-probe` | 部署表单在登记前测试某个 spec 声明的一个权重来源（`template_id`、`spec_source`、`spec_name`、`provider`、`repo_id` 必填，缺一 400 `WEIGHT_SOURCE_PROBE_QUERY_INVALID`；`revision`、`subfolder` 按声明原样带上，spec 没有声明这个来源时 400 `WEIGHT_SOURCE_NOT_DECLARED`，spec 不存在 404）。由管理节点按登记时的规则读取来源，返回 `{proceed, present?, host?, reason?}`：读到仓库时 `proceed: true`、`host` 为应答的地址；否则 `proceed: false`，`reason` 的码与登记的 `metadata_notice` 相同（`MODEL_REPO_NOT_FOUND`、`MODEL_REVISION_NOT_FOUND`、`MODEL_REPO_ACCESS_DENIED`、`MODEL_METADATA_UNREACHABLE`、`MODEL_METADATA_FORBIDDEN`）。选择该来源会复用的已登记模型在节点或共享存储上已有就位文件时不访问站点，直接返回 `proceed: true, present: true` | JWT |
| GET | `/api/v1/inference-templates/export/{digest}` | 按内容摘要导出可执行模板包 | JWT(admin) |
| GET | `/api/v1/inference-templates/model/*` | 按模型路径获取模板详情。`can_delete` 表示当前调用方有权请求删除整个模板，引用检查在删除时执行。 | JWT |
| GET | `/api/v1/inference-templates/yaml?model_id=…` | 模板编辑器里的平铺 YAML：server 用导入同一套目录类型写出，可加 `spec_source=user`（或 `upstream`）只留该来源的 spec，`readme_*` 字段只在 spec 全部列出或模板本身归用户时写出；原样保存（`import?overwrite=true`）不丢字段 | JWT |
| GET | `/api/v1/inference-templates/schema` | 平铺 YAML 编辑器的字典：字段的补全、悬停说明与客户端首轮校验所需的约束（权威校验仍在导入时）；可选 `spec.gpus.arch_id` 必须非空且与已报告架构 ID 精确匹配（忽略大小写，缺失时不匹配） | JWT |
| POST | `/api/v1/inference-templates/clone` | 克隆模板为可编辑的用户 spec | JWT(admin) |
| POST | `/api/v1/inference-templates/evaluate` | 检查平铺 YAML 或目录包，不写入；请求与 `import` 相同，支持 `?overwrite=true`。返回 200 `{valid, errors, warnings}`；检查范围与结果见[导入前检查](#导入前检查) | JWT(admin) |
| POST | `/api/v1/inference-templates/import` | 导入模板/目录包；成功返回 `{models, package_digest}`，有警告时多一个 `warnings`（同 `evaluate`，无警告时不出现该字段） | JWT(admin) |
| POST | `/api/v1/inference-templates/sync` | 「检查更新」：从发布通道取官方推理模板目录包，按 upstream 来源对账（用户档位不动）；请求体忽略。返回 `{models, package_digest, version}`；本 server 读不懂的模型或 spec 被跳过并保持原样，此时多一个 `skipped: [{model_id, spec?, reason}]`（跳过整个模型时不带 `spec`，见[官方目录包里读不懂的内容](inference-catalog-package.md#官方目录包里读不懂的内容)）；目录包 schema 高于本 server 时 409 `UPDATE_CATALOG_INCOMPATIBLE`；目录包整体读不了（损坏、清单不符，或没有一个模型读得懂）时 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER`（`params.error` 带原始报错，界面提示该版本推理模板依赖最新版本服务端）；其余错误见[目录导入结果](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/inference-templates/official` | 上传官方推理模板目录包（请求体为 `.tar.gz` 目录包，上限 32 MiB），与「检查更新」走同一条 upstream 对账，返回与跳过规则相同；不是目录包时 400 `OFFICIAL_CATALOG_PACKAGE_REQUIRED`，目录包整体读不了时 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` | JWT(admin) |

模板包结构与导入规则见[可执行推理目录包](inference-catalog-package.md)。

`GET /api/v1/inference-templates/index` 默认保留完整索引。带 `compatibility=detected&vendor=<已知厂商>` 时，服务端按在线节点实际报告的卡计算每个 profile 的 `detected_compatible`，并返回参与计算的 `hardware`；可加 `node_uuid` 限定手工选择的节点。无效 mode/vendor 返回 400，未知节点返回 404；离线节点没有兼容卡。该字段仅表示芯片规则匹配，不保证显存、具体选卡或完整启动可准入，这些仍由部署 evaluate 判断。

模板详情的 `detected_support` 提供服务端选择的 `preferred_engine` 与 `warnings`（含 `alternative_available`）。前端只展示这些事实；列表硬件过滤、详情与推荐共用服务端 chipmatch 的 unsupported 否决和特异性规则，不在浏览器重新编译名称正则或推导架构资格。

### 导入前检查

`POST /api/v1/inference-templates/evaluate` 接受与 `import` 相同的平铺 YAML 或 `.gpuworks-catalog.tar.gz` 目录包，也支持 `?overwrite=true`，使用相同的解析与校验。

返回 200 `{valid, errors, warnings}`。`errors`、`warnings` 的每项为 `{code, params, error}`：`code` 稳定，`error` 为英文文案，`params` 含 `model_id` 与 `spec`。
解析或校验失败时 `valid=false`，`errors` 带 import 会返回的相同错误码；没有专属码的解析错误为 `CATALOG_INVALID`，`params.detail` 保留原文。

该接口不检查同名用户档位是否已存在（实际 import 可能返回 409），不获取权重大小，也不写入模板或目录包存储。警告码见[推理目录语法参考](inference-catalog-spec.md#导入前检查与警告)。

## Deployments / Instances（部署与实例）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/deployments` | 部署列表；带 `page`/`page_size` 时分页返回列表列，不带时返回全部命中的部署对象；分页与否都按 `q`/`state` 筛选，未知 `state` 400 `LIST_FILTER_INVALID`（见“错误与响应约定”）。`state` 取值含 `sleeping`/`asleep`，行上另有 `asleep_replicas`、`sleep_level`，见[部署的睡眠状态](#部署的睡眠状态stateasleep_replicassleep_level) | JWT |
| GET | `/api/v1/routing-policies` | 服务端下发的多副本路由策略及本地化说明 | JWT |
| POST | `/api/v1/deployments/preflight` | 多机推理部署前置检查（rank 分配、网络探测）。请求 `{node_uuids, port?, master_addr?, bandwidth?, seconds?}`；返回 `{ok, pairs}`，每个有向节点对是一项检查结果：`id` 为 `node_pair`，`status` 为 `pass` 或 `fail`，`code` 为 `PREFLIGHT_PAIR_REACHABLE`、`PREFLIGHT_PAIR_UNREACHABLE`、`PREFLIGHT_LISTENER_FAILED`、`PREFLIGHT_PROBE_FAILED` 或 `PREFLIGHT_BANDWIDTH_NOT_MEASURED`，`params` 含 `source`、`target`、`address`（监听失败另有 `port`），`message` 为英文 fallback；另有 `source_uuid`、`target_uuid`、`target`、`reachable`、`rtt_ms_p50`、`rtt_ms_p99`、`gbps`、`bandwidth_tested`、`bytes_sent`、`bytes_received`、`duration_seconds`。`evidence`、`bandwidth_evidence` 是 agent 或 RPC 的原话，只作证据。任一节点对 `fail` 时 `ok` 为 false | JWT |
| POST | `/api/v1/deployments` | 创建部署（`model_source` 只有 `{storage_id, provider}`：保存模型的存储与运行哪家权重来源，不接受镜像获取方式；`placement.pd` 为 PD 分离部署，见下）。对外模型名被未在删除的部署占用时返回 409 `SERVED_NAME_IN_USE`，`params` 包含 `served_name`、`deployment`、`deployment_id`；不填写时取模型名，同样校验 | JWT |
| GET | `/api/v1/deployments/{id}/candidates` | 查询下一个副本的候选池；`status` 为 `room/busy/unmet/cordoned/offline`，支持 `offset`、`limit`（1–200，默认 50）。返回全量 `counts`、`unmet_by_key` 与一页 `items`；`room` 不保证能放下模型，需结合部署的 `placement_status` 判断。见[候选池查询](#候选池查询) | JWT |
| GET | `/api/v1/deployments/{id}/draft` | 把部署读回成创建它的请求（`deployment`），附 `revision` 与不可编辑字段 `locked`；部署页的编辑与「克隆并修改」由此回填。见[编辑部署](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}` | 编辑部署：提交改过的 draft 与 `base_revision`，`?dry_run=1` 只返回变更计划。启动配置的变化按 `apply`（`rolling`/`all`/`none`）替换实例。见[编辑部署](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}/requirements` | 替换普通单机部署的节点条件与放置策略，返回详情；只影响之后启动的副本，运行中副本不迁移；见[修改节点条件](#修改节点条件) | JWT |
| GET | `/api/v1/deployments/{id}` | 部署详情，无查询参数；直接返回部署对象，未找到 404。含 `state`（唯一该显示的状态，睡眠时为 `asleep`/`sleeping`）、`asleep_replicas`（因睡眠不被派发的副本数）、`sleep_level`（引擎上报的级别）、`instances`、`node_requirements`、`node_requirement_labels`、`placement_strategy`、`ready_replicas`（与 `asleep_replicas` 是同一组副本的两种计数，PD 按各角色汇总）、`health`、`tokenize_enabled`、`max_replicas`；`placement_status` 给出调度失败原因，`retry_exhausted` 标识耗尽重试次数的副本，`restart_requested_at` 标识滚动重启，多机副本见 `units`。见[查询部署详情](#查询部署详情)与[部署的睡眠状态](#部署的睡眠状态stateasleep_replicassleep_level) | JWT |
| PUT | `/api/v1/deployments/{id}/routing` | 修改路由策略（`least_request`、`prefix_affinity` 或 `failover`） | JWT + 管理员 |
| PUT | `/api/v1/deployments/{id}/dgpu` | 调整单机 dGPU 部署的 `vram_limit_gib`，返回立即生效的 `live` 与待重启的 `pending` 实例；见[调整显存上限](#调整显存上限) | JWT |
| PUT | `/api/v1/deployments/{id}/health-check` | 开关部署的健康检查并设阈值：`{"enabled": true, "threshold_sec": 60}`（`threshold_sec` 1–600，省略或 0 保持当前）。开启要求模型提供 chat、completions、embedding 或 System One 决策，否则 400 `FIELD_INVALID`（`params.field` 为 `health_check`）；阈值越界 400 `FIELD_INVALID`；不重启实例，下一轮检查即按新设置。返回部署详情 | JWT |
| PUT | `/api/v1/deployments/{id}/tokenize` | 开关网关对该部署的分词转发（`/v1/tokenize`、`/v1/detokenize`）：`{"enabled": false}`。`enabled` 必填，缺省 400 `FIELD_INVALID`（`params.field` 为 `enabled`），不是布尔 400；部署不存在 404。不重启实例，网关路由表随变更事件重建后生效。返回部署详情（带 `tokenize_enabled`） | JWT |
| GET | `/api/v1/deployments/{id}/events` | 部署事件时间线（最新优先、游标分页、读取时折叠） | JWT |
| POST | `/api/v1/deployments/{id}/clone` | 克隆已保存的部署配置，包括节点条件与放置策略。即使上游目录已移除原模板或档位，也可从部署快照克隆。副本的对外模型名为原名加 `-` 和 4 位随机小写字母或数字；请求体 `{"name": ...}` 同时设定部署名与对外模型名，被占用时 409 `SERVED_NAME_IN_USE` | JWT |
| POST | `/api/v1/deployments/{id}/catalog-template` | 把运行中的部署写成推理模板：先对 chat、completions、embeddings、System One 决策等低成本操作各发一个最小请求，再按部署实际运行的启动参数、权重 commit 与 GPU 生成平铺 YAML，不写入目录。见[另存为推理模板](#另存为推理模板) | JWT |
| POST | `/api/v1/deployments/{id}/stop` | 停止部署：删除全部实例，保留配置 | JWT |
| POST | `/api/v1/deployments/{id}/start` | 启动已停止的部署，按保留的配置重新调度（每个副本的重试次数重新计）；已在运行则不做任何事；删除中的部署 409 `DEPLOYMENT_DELETING` | JWT |
| DELETE | `/api/v1/deployments/{id}` | 删除部署 | JWT |
| POST | `/api/v1/deployments/{id}/sleep` | 休眠部署（`?level=1\|2`，默认 1；`?mode=wait\|abort`，默认 wait；`?wake_on_request=true\|false`，请求能否唤醒这次休眠，默认取部署的 `sleep_wakes_on_request`，其他值 400 `SLEEP_INVALID_WAKE_ON_REQUEST`）：睡眠意图写进每个活跃实例的期望状态，网关立即不再派发给它们。保持休眠（`wake_on_request` 为 false）时部署记为"整体处于睡眠命令下"（此后新起的实例也按此睡眠并被保持）；请求能唤醒时只点名醒着的副本，部署整体不在命令下。响应 `{"status":"sleeping","level","mode","wake_on_request"}`，时间线 `SLEEP_ORDERED` 记 `wake_on_request`。`?role=prefill` 只睡 PD 部署该角色的实例（响应带 `role`、`instances`），部署整体不算在睡眠命令下；非 PD 400 `PD_NOT_PD`，不是该部署模式的角色 400 `PD_ROLE_UNKNOWN`，模式不允许为空的角色（如 decode）400 `SLEEP_ROLE_CANNOT_EMPTY`。`level=1` 时本节点主机内存放不下将卸载的权重（可用内存减去 max(总量 5%, 4 GiB) 余量）409 `SLEEP_HOST_MEMORY_INSUFFICIENT`（`params.node`、`need_gib`、`available_gib`），改用 L2；节点没有最近的主机内存观测时不拦 | JWT |
| POST | `/api/v1/deployments/{id}/wake` | 唤醒部署：已睡着的实例须放得回原卡，否则 409 `WAKE_INSUFFICIENT_GPU_MEMORY`（全有或全无）。`?role=` 只唤醒 PD 该角色的实例。`?partial=true` 唤醒放得下的那部分，响应 `{"status":"waking","woken":[...],"refused":[{"instances","node","gpu_indexes","shortfall_gib","occupants"}]}`；一个也放不下时仍 409。409 的参数描述第一个被拒的节点，`refused` 列出全部被拒的副本；任何唤醒都会撤销部署整体的睡眠命令并解除被唤醒实例的保持休眠，要等位的唤醒（202 `waiting`）同时解除等位实例的保持。卡被平台外进程占着时 409 `WAKE_GPU_HELD_OUTSIDE`（参数同上，`refused[].held_outside` 为 true）。`?wait=true` 放得下的先醒、其余等位，`?yield=true`（可加 `&yield_level=1\|2`，默认 1，占用者主机放不下 L1 时自动改 2）先让占用者休眠再等位；响应见[唤醒等位与让位唤醒](#唤醒等位与让位唤醒) | JWT |
| POST | `/api/v1/deployments/{id}/units/suggest` | 为多机部署推荐一个副本的节点；请求体 `{"units":[...]}` 为已选副本，返回推荐 `unit` 或无法放置的原因，不保存；见[推荐多机副本节点](#推荐多机副本节点) | JWT |
| POST | `/api/v1/deployments/{id}/units/retry` | 让一个副本重新获得重试次数：请求体 `{"role","index"}`（普通部署 `role` 为空），该副本的失败尝试被移除（容器清理、时间线保留），控制器随后重建它，其他副本不受影响；没有该副本 404 `UNIT_UNKNOWN`，没有失败尝试或仍有实例 409 `UNIT_NOT_FAILED`，部署非运行中 409 `UNIT_RETRY_NOT_ACTIVE` | JWT |
| PUT | `/api/v1/deployments/{id}/replicas` | 单机部署修改副本数（增加时可加 `place_on` 指定新副本首次启动的节点），手动放置的多机部署修改副本节点列表，自动放置的多机部署按副本数增减（`place_on` 为新副本各 rank 的节点）；完整字段与条件见[普通部署的副本调整](#普通部署的副本调整) | JWT |
| PUT | `/api/v1/deployments/{id}/placement-mode` | 切换多机或 PD 部署的放置方式：请求体 `{"mode":"auto"}` 或 `{"mode":"manual"}`，不重启实例，单元沿用现有节点；切到 `auto` 同时去掉 PD 单元钉住的卡；200 返回部署详情（`placement_mode` 为新值）。`mode` 不是这两个值 400 `FIELD_INVALID`（`params.field` 为 `mode`），单机部署 400 `PLACEMENT_MODE_SHAPE`，部署不存在 404；见[自动放置多机与 PD 部署](#自动放置多机与-pd-部署) | JWT |
| DELETE | `/api/v1/deployments/{id}/wake-waiting` | 撤销该部署所有等位中的唤醒，返回 `{"items":[实例 UUID],"total":n}` | JWT |
| POST | `/api/v1/deployments/{id}/wake-on-request` | 允许请求唤醒：解除该部署全部实例的保持休眠，不唤醒任何实例（休眠意图不变），并撤销部署整体的睡眠命令；返回 `{"released":[实例 UUID]}`，没有被保持的实例时为空列表。有解除时时间线记 `SLEEP_HOLD_RELEASED`（`instances`、`actor`）。部署不可睡 400（同休眠的码），部署不存在 404 | JWT |
| GET | `/api/v1/deployments/{id}/schedule` | 部署的定时计划，`{"items":[计划],"total":n}`；见[定时计划](#定时计划) | JWT |
| POST | `/api/v1/deployments/{id}/schedule` | 新建一条定时计划，201 返回该计划；校验失败 400 `SCHEDULE_INVALID` / `SCHEDULE_TIMEZONE_UNKNOWN` / `SCHEDULE_REPLICAS_MANUAL_PLACEMENT`（手动放置的多机或 PD 部署） / `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` | JWT |
| PUT | `/api/v1/deployments/{id}/schedule/{sid}` | 整条替换一条计划，请求体与新建相同，200；计划不属于该部署 404 | JWT |
| DELETE | `/api/v1/deployments/{id}/schedule/{sid}` | 删除一条计划，204；不存在 404 | JWT |
| PUT | `/api/v1/deployments/{id}/yield-policy` | 设置出让策略与参数：请求体 `{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`，`policy`（`never` / `idle` / `timeslice` / `anytime`）必填，其余都可省略，省略的取该策略的默认值，不适用的被忽略。200 返回存下的策略，不适用的参数不出现；设为 `idle` 而卡上已有另一个 `idle` 部署时多一个 `idle_neighbours`。没有 `policy`（`{}`，或只带参数）、取值不合法或越界 400 `YIELD_POLICY_INVALID`（`params.field`、`reason`：`required` / `invalid_value` / `out_of_range`，越界另带 `minimum`、`maximum`）；策略不是 `never` 而部署不可休眠 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED`；多机或 PD 部署设 `never` 以外的策略 400 `YIELD_POLICY_SHAPE`；见[出让策略](#出让策略) | JWT |
| PUT | `/api/v1/deployments/{id}/node-replacement` | 设置节点失联重调度的等待时间 `{"after_sec":600}`，0 为从不，否则 60–604800，200 返回同样的请求体；越界 400 `NODE_REPLACEMENT_INVALID`（`params.minimum`、`maximum`）；见[节点失联重调度](../how-to/node-failover.md) | JWT |
| GET | `/api/v1/scheduling/board` | 调度看板：`?page=&page_size=&q=` 分页的节点（每张卡的醒着占用、休眠残留、可用量、占用者与外部进程）加全集群的 `pending`（最近 30 分钟被调度拒绝的部署）、`waiting_wakes`、`upcoming`（未来 24 小时的计划动作）；`q` 匹配节点名称、主机名或 UUID；每张卡带服务端算出的 `state` 与 `switching`；已不再有 `recent`（最近的自动化事件看调度记录）；`?node=<uuid>` 只回答这一个节点（一行的 `nodes` 页，其余三项为空，不存在的节点 404），`?queues=1` 只回答后三项（`nodes` 为空页，不读任何节点）；见[调度看板](#调度看板) | JWT |
| GET | `/api/v1/scheduling/overview` | 调度总览：全集群每个节点、每张卡的紧凑状态与汇总，一次返回，供热力图使用；来自缓存（至多每 2 秒重建一次），带 `ETag`，`If-None-Match` 命中返回 304；见[调度总览](#调度总览) | JWT |
| GET | `/api/v1/scheduling/history` | 调度记录：全集群的调度事件，最新优先、游标分页（`limit`、`before`，同部署时间线），可按 `deployment_id`、`category`（`auto` / `failure` / `manual`，逗号分隔）、`since` / `until`（RFC 3339）筛选；每条带它涉及的节点 `nodes`（`uuid`、`name`）；报告某次尝试失败的条目（`REPLICA_REBUILT`、`UNIT_RETRY_EXHAUSTED`）另带 `logs`：该次尝试的实例 `instance` 与失败前 10 分钟到失败后 1 分钟的时间窗 `from` / `to`（Unix 毫秒），即日志页 `/logs?instance=&from=&to=` 的参数；非法筛选 400 `FIELD_INVALID`（`params.field`），非法游标或页大小 400 `EVENT_CURSOR_INVALID` / `EVENT_LIMIT_INVALID`；见[调度记录](#调度记录) | JWT |
| GET | `/api/v1/scheduling/timeline` | 驻留时间线：`?node=<uuid>`（可重复，至多 200 个，通常是被打开的节点）`&window=15m\|1h\|6h\|24h`（默认 6h），或自定义范围 `&since=<RFC 3339>&until=<RFC 3339>`（`until` 缺省为现在；至多 7 天；`until` 不晚于现在，容许一分钟的时钟偏差；不与 `window`、`hours` 同用）。每张卡窗口内各部署实例占用它的分段（`awake` / `asleep` / `switching` / `starting`），以及出现过的非常驻部署的统计；`?sharing=1&window=15m\|1h\|6h\|24h`（默认 6h）不带 `node`，只回答全集群出让策略不是 `never` 的部署（及窗口内参与过让位、已删除的部署）的统计（`nodes` 为空，约 30 秒缓存）；响应的 `windows` 列出这一读法可选的窗口；旧参数 `hours=6\|24` 仍可用，等同 `window=6h\|24h`；缺少 `node`（且没有 `sharing`）、`sharing` 不是 1 或与 `node` 同用、`window` 不在可选之列、`hours` 不是 6 / 24、`since` / `until` 不是 RFC 3339 时间、只有 `until` 没有 `since`、`since` 不早于 `until`、`until` 晚于现在、范围超过 7 天、`since` / `until` 与 `window` / `hours` / `sharing` 同用时 400 `FIELD_INVALID`（`params.field`，消息如 `window is one of 15m, 1h, 6h, 24h`），不存在的节点被略过；见[驻留时间线](#驻留时间线) | JWT |
| GET | `/api/v1/scheduling/activity` | 调度活动：`?window=15m\|1h\|6h\|24h\|7d`（默认 24h）或 `?since=&until=`（RFC 3339，`until` 缺省为现在）。按服务端选定的桶长（`bucket_seconds`）给出每桶各原因的调度事件数（`buckets[].counts` 与 `reasons` 同序）、各原因在范围内的合计 `reasons[]`（`key`、`count`）、范围内切换最频繁的至多 20 张卡 `top_cards[]`（`node_uuid`、`node`、`gpu_index`、`switches`），范围开头超出统计保留期时 `beyond_retention` 为 true；不读事件日志，约 30 秒缓存；`window` 不在可选之列、与 `since` / `until` 同用、只给 `until`、时间格式不对、`until` 不晚于 `since`、范围超过 1825 天时 400 `FIELD_INVALID`（`params.field`）；见[调度活动](#调度活动) | JWT |
| GET | `/api/v1/scheduling/yield-policies` | 出让策略字典：四个策略按等级从高到低，每个带 `defaults` 与可能适用的参数 `applies`，加只在另一个参数取某些值时才适用的条件 `applies_when`，各数值参数的范围 `bounds`（`at_most` 为不得超过的另一个参数）、`sleep_levels`、`unfinished`，客户端据此编辑，不另存默认值；见[出让策略](#出让策略) | JWT |
| PUT | `/api/v1/deployments/{id}/pd` | PD 部署调整各角色副本（`roles.<role>.units`）与 `fallback`；`mode`、角色的 `tp`/`pp`、`kv_load_failure` 不可变（`PD_IMMUTABLE`）；非 PD 部署 400 `PD_NOT_PD` | JWT |
| POST | `/api/v1/deployments/{id}/restart` | 按当前配置重建部署，重置副本重试次数；请求体可选 `strategy`（默认 `rolling`，`all` 会中断服务）；见[重启策略](#重启策略) | JWT |
| POST | `/api/v1/evaluate` | 显存估算、调度试算与获取预估（不创建部署）：响应的 `acquisition` 含 `present`（已持有模型、目标节点读得到的存储）、`targets`（节点取模型时可保存到的存储）、`providers`（模板的权重来源）、`image`，请求指定了节点时的 `nodes[]`，以及目标节点已知时模板组件的 `components[]`（见「评估请求与获取预估」） | JWT |
| GET | `/api/v1/instances` | 实例列表，新的在前；带 `page`/`page_size` 时分页返回（`{items,page,page_size,total}`，见“错误与响应约定”），不带时返回全部命中（`{items,total}`）；分页与否都可按 `q`（实例 UUID、所属部署名或节点名的子串，不区分大小写）与 `state`（实例自身状态，逗号分隔：`pending`、`pulling`、`starting`、`optimizing`、`running`、`draining`、`removing`、`stopped`、`failed`、`pending-removal`，其他值 400 `LIST_FILTER_INVALID`）筛选；页码超过末页（无论多大）返回空 `items` 与真实 `total` | JWT |
| GET | `/api/v1/instances/{uuid}` | 实例详情。服务端推导的字段：`replica`（该实例承担的副本 `{role,index}`，不承担时缺省）、`asleep`（因睡眠不接收请求：已下令睡眠或仍在睡）、`desired_sleep`（下令的级别）、`sleep_held`（该实例的休眠由运维或定时计划下达，请求不唤醒它；保持按实例记录，唤醒只解除被唤醒实例的保持）、`can_sleep_alone` / `can_wake_alone`（能否单独睡、单独醒）；多机副本的 follower 不上报自己的睡眠级别，`sleep_level` 给的是它所在代 leader 的级别；`dist_role` 对未记录角色的旧行按 headless 与 rank 推出。失败的实例带 `runtime_failure`（`{code, params, message}`）：`code` 为失败码（如 `CONTAINER_EXITED`、`ENGINE_HEALTH_TIMEOUT`），`params` 为原因的类型化字段（`phase`、`exit_code`、`oom_killed`、`port`、`resource`、`detail`、`rank`、`node`、`evidence`，以及 `causes`：引擎输出显示的已知故障，各自为 `{code, params, message}`），`message` 为英文 fallback；老 agent 上报或旧行没有该字段，只有 `message`。`notes` 是服务端读实例时推导的非阻塞提示（`[{code, params}]`，没有时缺省），如实例所在的卡与平台外进程共用（`INSTANCE_CARD_SHARED_OUTSIDE`，参数 `gpu_indexes`、`foreign_bytes`、`foreign_gib`；占用未知时为 `INSTANCE_CARD_SHARED_OUTSIDE_UNKNOWN`），不持久化。部署详情里的 `instances[]` 带同样的字段，PD 角色带 `may_be_empty`。`previous_attempt: true` 标记已被后续失败或已填充副本取代的历史失败尝试。某一 unit 类仍有未填充副本时，其最新失败保持为当前失败，省略该字段。实例详情、实例列表行（含过滤分页）与部署 `instances[]` 使用同一推导，不新增持久状态。 | JWT |
| GET | `/api/v1/instances/{uuid}/events` | 实例事件时间线（最新优先、游标分页、读取时折叠） | JWT |
| GET | `/api/v1/instances/{uuid}/launch-facts` | 实例启动事实三层投影（intent/resolved/runtime observation，unknown 不降级，敏感文本脱敏） | JWT |
| GET | `/api/v1/instances/{uuid}/logs` | 实例容器日志（SSE，经 agent logtail 隧道） | JWT |
| POST | `/api/v1/instances/{uuid}/sleep` | 只睡该实例所在的副本（`?level=`、`?mode=`、`?wake_on_request=` 同部署级）；多机副本的任一 rank 都代表整个副本，意图写在它的全部实例上，由 leader 的引擎带着 follower 一起睡，响应的 `instances` 列出全部 rank。部署其余副本照常服务，部署不算整体睡眠（该副本被重建时新实例醒着）。部署不可睡 400（同部署级的码）；实例没有在承担副本 409 `INSTANCE_NOT_SERVING`；PD 模式不允许为空的角色的最后一个醒着的副本 409 `SLEEP_ROLE_CANNOT_EMPTY` | JWT |
| POST | `/api/v1/instances/{uuid}/wake` | 唤醒该实例所在的副本（多机副本同上，按整个副本），只检查这个副本放不放得回原卡（409 `WAKE_INSUFFICIENT_GPU_MEMORY`）；部署整体的睡眠命令随之撤销，其余睡着的副本保持睡眠。同样接受 `?wait=true`、`?yield=true`、`yield_level`，并可能返回 409 `WAKE_GPU_HELD_OUTSIDE` | JWT |
| POST | `/api/v1/instances/{uuid}/restart` | 重启该实例所在的副本（follower 代表它所在的整代）：默认让副本的 leader 进入 `draining`（网关不再派发新请求，处理完手上的请求后退役），同时启动替换实例；`?drain=false` 立即移除整代；不占用副本的重试次数；记部署事件 `REPLICA_RESTARTED{instance,drain}`；部署非 active 409 `DEPLOYMENT_NOT_ACTIVE`，实例没有在承担副本（失败、排空中、移除中）409 `INSTANCE_NOT_SERVING` | JWT |

实例启动事实接口的三层信息分别是：部署意图（intent）、解析后启动方案（resolved）和运行时观测（runtime observation）。
缺失或不可用的信息保持 unknown，不以其他层的信息代替；敏感文本脱敏。

### 另存为推理模板

`POST /api/v1/deployments/{id}/catalog-template` 请求体可省略，或为 `{"include_env": ["KEY", ...]}`：部署自己的环境变量默认全部不导出（常含令牌与主机路径），只有列出的 key 写进档位。任何登录用户都可以导出；把结果存进目录仍走 `POST /inference-templates/import`，只允许管理员。

返回 200：

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

- `yaml` 可原样导入，开头的注释与 `notes` 说的是同一组事实；注释是英文，界面按 `notes` 的 `code` 与 `params` 本地化。
- `checks`：每个操作一项检查，`id` 都是 `operation_reply`，`operation` 指明哪个操作。chat、completions、embeddings、System One 决策各发一个与健康检查相同的最小请求：应答 2xx 为 `status: "pass"`（`TEMPLATE_OPERATION_ANSWERED`），否则为 `fail`，`code` 为 `TEMPLATE_OPERATION_HTTP_ERROR`、`TEMPLATE_OPERATION_NO_REPLICA`、`TEMPLATE_OPERATION_TIMED_OUT` 或 `TEMPLATE_OPERATION_REPLY_INCOMPLETE`，网关或引擎返回的原文只放在 `evidence`。视频、图片、语音等操作不发请求，`status: "skip"`，`code` 为 `TEMPLATE_OPERATION_NOT_PROBED`（没有声明接口地址时为 `TEMPLATE_OPERATION_NO_ENDPOINT`），并有一条 `operation_not_tested`。`params` 带 `operation`、`endpoint?`、`http_status?`，`message` 是英文 fallback。
- 有操作实测失败时 `verified`、`saveable` 都是 `false`，`yaml` 只供排查。
- `notes` 的 `code`：`verified_on`、`operation_passed`、`operation_failed`、`operation_not_tested`、`kv_bytes_per_token` 缺失时的 `kv_bytes_missing`、`model_config_missing`、`placement_omitted`（`gpu_count`、`gpu_memory_mib`）、`vram_limit`、`env_excluded`、`parameters_dropped`（没有引擎 flag 承载的设置，如 `cpu_cores`）、`container_overrides_omitted`。
- `evidence` 是提交上游时附带的纯文本测试记录。

写不成模板时返回 409 `TEMPLATE_EXPORT_UNAVAILABLE`，`params.reason` 为：`not_running`（部署未运行或没有运行中的实例）、`distributed`（多机）、`pd`、`no_gpu_facts`、`template_assets`、`components`（源模板带随包文件或组件模型）、`weights_not_public`（不支持的权重来源，如 URL；本地注册模型支持 `local` 来源）、`revision_unpinned`（权重版本不是 commit）、`revisions_differ`、`template_missing`、`model_missing`、`launch_changed`（配置或运行实例已变化）、`launch_snapshot_missing`（没有完整可核验的启动记录）、`heterogeneous_gpus`（绑定了不同型号的卡）、`command_overridden`（部署替换了引擎命令）。导出的参数重建不出原命令时返回 409 `LAUNCH_NOT_EXPORTABLE`。部署不存在 404，请求体不是合法 JSON 400。生成规则见[把部署另存为推理模板](../how-to/save-deployment-as-template.md)。

本地模型另存的 YAML 使用 `weights.sources: [{local: <source_id>}]`，只引用已注册权重，不打包文件或保存主机路径。`local_weights` 说明携带 `repo_id`。缺少本地注册时，创建部署返回 400 `MODEL_SOURCE_UNAVAILABLE`，`params.reason=local_model_not_registered`；不会创建空模型。详情见[本地模型复用](../how-to/save-deployment-as-template.md)。

### 部署列表的筛选与返回字段

`q` 匹配部署名称、对外模型名、模型名。`state` 对应界面显示的状态：`running`、`starting`、`pulling`、
`pending`、`offline`、`failed`、`stopped`、`deleting`、`sleeping`、`asleep`；删除中状态优先于运行时状态。
取值必须是其中之一，否则 400 `LIST_FILTER_INVALID`（不静默匹配全部）。
`node` 按 `runtime_nodes` 的节点 UUID 筛选，多个值用逗号分隔，匹配任一所选节点；与 `q`、`state` 取交集，在排序和分页之前执行。只匹配实际运行位置，不匹配放置目标；没有实际运行节点的部署不命中。未知 UUID 返回空结果。`node=` 表示不筛选；逗号分隔值中出现空项时返回 400 `LIST_FILTER_INVALID`。分页响应另含 `node_options: [{uuid, name}]`，列出完整部署快照中的实际运行节点，按名称、UUID 排序并按 UUID 去重，不受当前筛选与分页影响；无可选节点时为 `[]`。

可排序的列为 `name`、`model`、`state`、`replicas`、`created_at`；`sort=state` 按的就是下面这个 `state`。

`state` 是每行唯一该显示的状态，`runtime_state` 不是：睡着的引擎仍在运行，`runtime_state` 一直是 `running`，
而网关已经不再向它派发请求。取值的优先级是：

- 删除中的部署算 `deleting`，优先于运行时的任何状态。
- 其余情况下等于 `runtime_state`，只有两种例外，且只在 `runtime_state` 为 `running` 时才替换它：网关因所需副本全部睡着而拒绝所有请求时算 `asleep`，
  睡眠指令已被接受、但还有引擎没报出级别时算 `sleeping`（详见[查询部署详情](#查询部署详情)）。
- `offline` 是 `runtime_state` 自己的取值：没有可服务的副本（在线节点上没有非 headless、且所在多机代完整的运行中实例），
  同时有运行中实例停在已离线的节点上。
- 有副本在服务（非 headless、所在多机代的 follower 全部运行中的运行中实例；PD 部署每个角色都要有）时 `runtime_state` 是 `running`，
  扩容或滚动重启中正在拉取、启动的新副本不会把它改成 `starting` / `pulling`；副本全睡着时同理，`state` 仍是 `asleep`。
  headless follower 的状态不算副本存活：leader 失败，或醒着的 leader 同一代的 follower 失败时，没有别的副本在服务就是 `failed`。
  休眠 leader 按网关顺序先判睡眠，即使 follower 未就绪，`state` 仍是 `asleep` / `sleeping`，`ready_replicas` 仍为 0。
- 只有部分副本睡着的部署仍能服务，`state` 保持 `running`，睡着的副本数由 `asleep_replicas` 给出；
  这样的行同样能被 `state=running` 筛到。

成功返回 HTTP 200。外层响应中，`items` 为部署对象数组，`total` 为筛选后的总命中数（整数）；
启用分页时另有整数 `page`、`page_size`。空结果为 `items: []`，超过最后一页也返回空数组。

分页时，每个部署只返回下列列表字段；常用字段含义见[查询部署详情](#查询部署详情)，其余说明见部署路由条目：

| 字段组 | 字段 |
|---|---|
| 身份与创建时间 | `id`、`name`、`created_at` |
| 模板与模型 | `template_id`、`template_deviated`、`engine`、`image`、`model_id`、`model_name`、`served_name` |
| 节点 | `node_uuid`、`node_name`、`node_names`、`runtime_nodes`、`runtime_node_names` |
| 状态与副本 | `desired_state`、`runtime_state`、`state`、`sleepable`、`asleep_replicas`、`sleep_level`、`replicas`、`ready_replicas`、`health`、`preparation` |
| 操作 | `operations` |

节点字段分两类，含义不同、不能互相替代：`node_uuid`、`node_name`、`node_names` 是**放置目标**，即部署被要求运行在的节点
（手动放置时是运维写的节点，自动放置时没有这个字段）；`runtime_nodes` 是**实际运行位置**，即实例此刻所在的节点，
取自实例行本身，与 GPU 设备页的归属同源，因此两处对同一批实例报同一台机器。
两者不一致是正常的：自动放置会改挑节点，失败重试会在别的节点重新启动，`spread` 部署的副本本就分散在多台。
`runtime_nodes` 是对象数组，一台节点一项：`node_uuid`、`node_name`、`instances`（该节点上的实例数）；
顺序稳定、按节点去重，多副本时给出全部节点而不是只给一台。没有实例行指向节点时（尚未放置，或副本都已停止）整个字段省略。
`runtime_node_names` 是同序的节点名。

`state`、`asleep_replicas`、`sleep_level` 在列表行上与详情里同名字段含义和取值都相同（见[查询部署详情](#查询部署详情)）；
只看 `runtime_state` 会漏掉睡眠——睡着的引擎仍报 `running`。
`asleep_replicas` 与 `sleep_level` 为 0 时整个字段省略，与详情一致。

分页列表的 `preparation` 是 active 部署中第一个尚未准备完成的模型文件信息，没有时省略；
完整准备情况在详情的 `model_progress` 中。

不分页时返回部署对象的其余配置字段，但仍不返回 `instances` 和 `node_requirement_labels`；
分页列表还省略启动配置、目录选择快照等详情。需要实例或完整详情时查询 `GET /api/v1/deployments/{id}`。

### 查询部署详情

`GET /api/v1/deployments/{id}` 返回一个部署的配置与当前状态，需管理鉴权。
请求无查询参数或请求体；路径中的 `id` 是正整数部署 ID，从列表的 `items[].id` 取得。

成功返回 HTTP 200，响应体直接是部署对象，外层没有 `item` 或 `items`。
找不到部署时返回 404，响应为 `{"error":"..."}`。客户端应根据 HTTP 状态处理，并显示 `error` 中的原因；该接口没有专用稳定错误码。
鉴权失败按上文[鉴权模型](#鉴权模型)处理。

下面是普通部署的关键字段示意，省略其他配置和实例字段：

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

| 关键字段 | 类型 | 含义 |
|---|---|---|
| `id`、`name` | 整数、字符串 | 部署 ID 与管理界面显示名 |
| `served_name` | 字符串 | 发起推理请求时填写的 `model` 名称 |
| `model_id`、`model_name` | 整数、字符串 | 关联的已注册模型 ID 与名称 |
| `desired_state` | 字符串 | 期望状态：`active`（维持运行）、`stopped`（停止）、`deleting`（删除中） |
| `runtime_state` | 字符串 | 当前运行状态：`pending`、`pulling`、`starting`、`running`、`offline`、`failed`、`stopped`。只由实例与期望状态推出，不含睡眠：睡着的引擎仍是 `running` |
| `state` | 字符串 | 唯一该显示的状态。取值同列表筛选的 `state`：删除中优先给 `deleting`；否则等于 `runtime_state`，只有网关因所需副本全部睡着而拒绝所有请求时才改为 `asleep`（所有睡着副本的引擎都已报出级别）或 `sleeping`（指令已被接受、网关已拒绝，但还有引擎没报出级别） |
| `asleep_replicas`（可选） | 整数 | 因睡眠不被网关派发的副本数，跨全部角色；与 `ready_replicas` 是同一组副本的两种计数，两者相加是网关能触达的副本数。0 时省略 |
| `sleep_level`（可选） | 整数 | 引擎上报的最深睡眠级别（1 或 2）；全部副本清醒、或睡眠指令尚未执行时省略。缺省不等于清醒，判断睡没睡看 `state` |
| `replicas`、`ready_replicas` | 整数 | 部署的副本数、当前所有 rank 均运行且清醒的副本数。PD 部署按各角色的 unit 汇总（`replicas` 为 `pd.roles[].desired` 之和，`ready_replicas` 为 `pd.roles[].ready` 之和），与列表行给出的同一对数一致；`asleep_replicas` 永不超过 `replicas` |
| `health` | 对象 | 健康检查状态与最近结果，见[健康检查](#健康检查) |
| `instances`（可选） | 对象数组 | 部署的实例；`uuid` 标识实例，`state` 表示状态，`node_uuid` 与 `gpu_indexes` 表示运行位置 |
| `runtime_nodes`（可选） | 对象数组 | 实例当前所在节点，一台一项 `{node_uuid, node_name, instances}`；与列表行的同名字段含义、取值都相同。`node_uuid`、`node_name`、`node_names` 是放置目标，与它可能不同 |
| `model_progress`（可选） | 对象数组 | 模型文件的准备状态；`state` 是文件状态，`progress` 为 0–1 的进度，`message` 为相关说明 |
| `quick_params`、`extra_args`（可选） | 对象、字符串 | 已保存的启动配置与额外参数，见[部署启动参数来源](deployment-launch-sources.md) |
| `pd`（可选） | 对象 | PD 部署的模式、各角色副本数和状态，见 [PD 分离部署](#pd-分离部署placementpd) |
| `node_replacement` | 对象 | 节点失联重调度的等待时间 `{after_sec}`，默认 600，0 表示从不 |
| `yield_policy` | 对象 | 出让策略与参数，形状同[出让策略](#出让策略)的响应：`policy` 与适用的参数；见[出让策略](../explanation/service-levels.md) |
| `sleep_hold`（可选） | 字符串 | 由各实例的 `sleep_held` 推导的休眠副本保持情况：`all`（全部被运维或定时计划保持，请求一个也不唤醒）、`some`（部分被保持，请求只唤醒其余的）、`none`（都是平台的休眠，请求会唤醒）；没有休眠副本时省略 |
| `sleep_held` | 布尔 | 推导值：部署有休眠副本且全部被保持（`sleep_hold` 为 `all`）时为 `true`，此时请求不唤醒部署 |
| `sleep_wakes_on_request` | 布尔 | 运维的休眠不指定 `wake_on_request` 时请求能否唤醒：部署可休眠且出让策略不是 `never` 时为 `true`，否则 `false`（保持休眠）。界面的休眠确认框以它为“有请求时自动唤醒”的初值；让位唤醒对占用者用同一条规则 |
| `wake_waiting_since`（可选） | RFC3339 时间 | 该部署有唤醒在等位时出现，值为等位起点 |
| `wake_wait_limit_sec` | 整数 | 等位唤醒放弃前最多等多久（秒），目前是 1800；界面的等位提示按它写，不自带数字 |
| `yield_policy_eligible` | 对象 | 该部署能否设出让策略（`never` 以外）：`{ok}`，不行时多一个 `reason`：`SLEEP_NOT_ENABLED`（没打开可休眠）、`SLEEP_ENGINE_UNSUPPORTED`（引擎不支持休眠）或 `YIELD_POLICY_SHAPE`（多机或 PD 部署）。设置出让策略的接口就是按它拒绝的，界面据此禁用策略并显示原因 |
| `health_checkable` | 布尔 | 该部署的健康检查能否开启：模型提供 chat、completions、embeddings 或 System One 决策任一接口时为 `true`。`PUT /deployments/{id}/health-check` 按同一规则拒绝开启，界面只据此决定能否打开开关 |
| `priority_scheduling` | 布尔 | 部署是否要求引擎按请求优先级调度；实际启动时只在引擎参数允许的情况下生效（见[请求优先级](../explanation/request-priority.md)） |
| `schedule_capabilities` | 对象 | 该部署的定时计划能下什么动作：`sleep`、`wake`（各是 `{ok, reason?}`，`reason` 为 `SLEEP_NOT_ENABLED` 或 `SLEEP_ENGINE_UNSUPPORTED`，引擎不能休眠的部署两者都不行，界面也不显示休眠控件），以及 `replicas[]`：`{role?, minimum, maximum, ok, reason?}`，单机部署一项（无 `role`），PD 部署按模式的角色顺序每个角色一项；`minimum`/`maximum` 是定时副本数的范围（模式允许为空的 PD 角色 `minimum` 为 0），`ok` 为 `false` 时 `reason` 是不能定的原因（例如手动放置的多机或 PD 部署是 `SCHEDULE_REPLICAS_MANUAL_PLACEMENT`）。创建定时计划的接口按同一套规则校验。部署列表不带这三个字段 |

`active` 表示期望运行，不代表已经能够服务；还需结合运行状态、就绪副本和健康检查判断。
可选字段未返回时，不能一律按 `0` 或空数组处理。以下字段用于判断放置与重试状态：

- `node_requirements` 总是出现，无条件时为 `{}`；固定节点存于 `gpuworks.ai/node`，已删除的节点仍保留在条件中，但无法匹配。
  详情额外返回 `node_requirement_labels[]`：`{key,value,display,display_code?,nodes}`。节点与网络显示名称；`nodes` 是当前匹配数量，0 不代表条件失效，而是当前没有节点匹配。列表不带这些显示字段。
- `placement_strategy` 为 `spread` 或 `binpack`。普通单机副本尚无实例时，`placement_status{unscheduled,reason,at}` 给出等待数量与最近原因：`pool_empty`、`pool_no_fit` 或 `waiting_for_yield`（等共卡的部署按出让策略让卡，见[使用调度分析](../how-to/scheduling-center.md#新部署怎样放到共用的卡上)）。
  原因参数包含各类计数、最多 3 台最接近的候选及拒绝原因；每个等待超时窗口最多记录一次。
- `placement_mode` 只在多机与 PD 部署出现：`auto` 表示节点由平台挑选（创建时按空闲卡挑，单元启动时放不下再换），`manual` 表示节点是运维写的；升级前已有的部署都是 `manual`。单机部署不带这个字段，它的放置写在节点条件里（`GET /deployments/{id}/draft` 的 `placement_mode` 与 `node_uuid`）。
- `max_replicas` 是调整副本数的上限；`restart_requested_at` 只在滚动重启进行中出现。
- 普通部署的 `retry_exhausted{index,failed_attempts,last_failure}` 指出耗尽重试次数的副本，可用 `units/retry` 重试；多机部署的 `units[]` 给出各副本 leader 与 `followers[]`。调整方式见[普通部署的副本调整](#普通部署的副本调整)。

请求示例见[查询一个部署的详情](api-conventions.md#3-查询一个部署的详情)。

没有实例的部署，最近调度原因仍为 `image_delivery_waiting`、且该部署的镜像正在目标准备节点上拉取或同步时，等待不会被算成调度超时失败。其他镜像或其他节点的传输不影响判断；传输完成、失败或取消，或调度原因改变后，恢复原有状态推导。列表和详情读取这些镜像事实失败时返回 500，不把未知状态显示成已失败。

### 部署的睡眠状态（`state`、`asleep_replicas`、`sleep_level`）

休眠的部署在 `runtime_state` 里看不出任何区别：引擎进程还在跑，它只是被网关拒绝派发。
回答「这个部署现在睡没睡」的是 `state`、`asleep_replicas` 与 `sleep_level` 这三个字段，部署详情与
列表行（分页列表的 `items[]`）同名同义、取值相同，`state` 筛选也按同一个 `state` 匹配。

这三个字段都是**读的时候算出来的**，不落库：唯一的持久事实是每个实例被下令的睡眠意图与自己上报的
`sleep_level`，判定条件与网关本身拒绝请求的条件是同一个。

- **`asleep_replicas`**：网关因睡眠而不予派发的副本数，跨全部角色（PD 部署按各角色的 unit 计）。
  副本按其 leader 判定：leader 在运行，且已被下令睡眠（意图尚未执行也算）或仍在上报睡眠级别，这个副本就算睡着。
  所以**指令刚发出、引擎还在换出显存的时候，副本就已经计入 `asleep_replicas`**，因为它确实已经不接收请求了。
  它与 `ready_replicas` 是同一组副本的两种计数：两者相加是网关**够得到**的副本数，因别的原因够不到的副本
  （已停止、已失败、节点失联、多机副本缺 rank）两个都不计。因别的原因挂掉的副本永远不会被算成睡着。
- **`state`**：见上文优先级。`asleep` 与 `sleeping` 只在网关**一个请求都不接**时出现——
  它需要的某个角色没有任何清醒副本，而这些拒绝它的角色**全都**是因为睡着（有睡着副本、没有清醒副本），
  不是因为挂了。只睡了一部分副本的部署照常服务，`state` 保持 `running`，醒着的副本继续接收请求，
  `asleep_replicas` 说出剩下的有多少不在服务；这样的一行既不会伪装成完全健康，也不会伪装成整体睡着。
  `deleting` 先于以上全部判断。
- **`sleeping` 与 `asleep` 的界线**：所有睡着副本的引擎都报出了被下令的级别时是 `asleep`；
  只要还有一个没到位就是 `sleeping`。改级别（例如 L1 改成 L2）也要经过清醒再睡，
  期间行显示 `sleeping` 而不是原地不动的 `asleep`，用户点下去之后能看出指令在执行。
- **`sleep_level`**：引擎上报的最深级别（1 或 2）。全部副本清醒时为 0（字段省略）；
  指令已发出、引擎还没睡下去时同样没有值——**别把缺省当成「没有睡」**，
  判断睡没睡看 `state` 与 `asleep_replicas`，`sleep_level` 只回答「睡到多深」。
- **什么都不剩的部署不算睡着**：没有任何可服务副本时（实例已停止或已失败），没有东西可唤醒，
  `state` 给出失败/停止本身的状态，`asleep_replicas` 为 0，即使实例上还留着睡眠意图。
- **PD 部署**：`pd.roles[].asleep` 是同一批副本按角色拆开的结果（`ready` + `asleep` 是该角色网关够得到的 unit 数）。
  模式允许跑空、且已整体睡着的角色（如 vLLM NIXL 的 prefill）不算拒绝服务，
  只有解码这类必须存在的角色全睡时部署才读作 `asleep`/`sleeping`。

实例级的睡眠字段（`instances[].asleep`、`instances[].sleep_level`、`instances[].desired_sleep`、
`can_sleep_alone` / `can_wake_alone`）见[实例与部署路由条目](#deployments--instances部署与实例)；
操作步骤见[操作 sleep / wake](../how-to/sleep-wake.md)。

### 候选池查询

`GET /api/v1/deployments/{id}/candidates`

部署候选池的实时情况：按「下一个副本」的视角把集群每台节点归到它没通过的第一道检查（`room` 看上去有空卡、`busy` 卡被醒着的实例占用、`unmet` 不满足节点条件、`cordoned` 已停止调度或驱逐中、`offline` 离线），`counts` 覆盖全部节点、相加等于 `nodes`，`unmet_by_key` 按首个不满足的条件键计数。节点条件满足、但没有一张卡能运行这次启动（宿主运行时不符、加速卡不支持所需功能）的节点也归 `unmet`，计在 `accelerator` 键下，条目不带 `detail`，改带结构化的 `reason`（`code` 与 `params`，由客户端按语言渲染）。

`items` 是一页明细（候选按副本尝试顺序带 `rank`，其后是不满足的与离线的），`?status=` 过滤，`?offset=`、`?limit=`（1–200，默认 50）分页，参数不合法 400 `FIELD_INVALID`。

本接口不做放置试算：「看上去有空卡」的是否真放得下以控制器的试算为准，见部署详情的 `placement_status`。

### 编辑部署

`GET /api/v1/deployments/{id}/draft` 返回 `{"deployment": {...}, "revision": "...", "locked": [...]}`：

- `deployment` 与 `POST /api/v1/deployments` 的请求同形。`quick_params` 是部署保存的完整启动配置；单机部署的手动放置写成 `placement_mode: "manual"` 加 `node_uuid`，厂商写在 `target_vendor`，二者不出现在 `node_requirements` 里。
- 模板部署的 `model_source.provider` 是部署挂载的权重来源，前提是模板规格声明了它。规格可以由任何就绪的同仓库副本满足，导入到存储里的模型来源是 `local`，这不是规格提供的选择：这时 draft 不写 `provider`，评估重新选出就绪副本（写了会被评估以 `ACQUISITION_CANDIDATE_STALE` 拒绝，而这个字段在编辑里不能改）。
- `revision` 标识读出 draft 时的配置，编辑时作为 `base_revision` 送回。
- `locked` 是该部署不能编辑的请求字段：模型、模板、引擎、厂商、名称与对外模型名、权重来源、节点数；多机与 PD 部署另有按 rank 给出的节点、副本数、dGPU 配额等（用[普通部署的副本调整](#普通部署的副本调整)与 PD 接口修改），PD 部署的 `placement` 也不可编辑。要改这些字段，克隆部署。

`PUT /api/v1/deployments/{id}` 请求体：

```json
{"deployment": {...}, "base_revision": "...", "apply": "rolling", "start": false, "wake": false}
```

`deployment` 是改过的 draft。`locked` 字段写了就必须与 draft 一致（不写则保持），否则 400 `DEPLOYMENT_FIELD_NOT_EDITABLE`（`params.field`）。其余字段按创建的规则校验（参数字典、节点条件、健康检查、「永远放不下」检查 `DEPLOYMENT_NEVER_FITS` 等）。出让策略不是 `never` 时不能关掉 `sleepable`（出让就是休眠），400 `YIELD_POLICY_NEEDS_SLEEP`（`params.policy`）；先用 `PUT /deployments/{id}/yield-policy` 改回 `never`。

启动配置从部署保存的配置出发，不重新套用模板：draft 里原样送回的值保持不变，只有改过的值才按参数字典的取值范围检查；模板在部署之后的修改不会经编辑带进部署，也不会补上部署原本没有的字典默认值。

`?dry_run=1` 只返回计划，不写入：

| 字段 | 说明 |
|---|---|
| `changes[]` | 逐字段的变化 `{field, layer, from, to}`，字段名同请求（启动参数为 `quick_params.<key>`，容器参数为 `container_overrides.<key>`）。环境变量逐个列出为 `container_overrides.env.<NAME>`，只带 `masked: true`，不带取值 |
| `changes[].layer` | `live`：立即生效（副本数、路由、健康检查、分词、启动超时、节点条件与放置策略）；`restart`：实例启动时读取（启动参数、额外参数、镜像、容器参数、并行度、休眠能力、dGPU 配额），运行中的实例在被替换前保持旧值 |
| `restart` | 是否有 `restart` 层的变化 |
| `apply_options`、`default_apply` | 当前状态下可选的生效方式与默认值，见下表 |
| `can_start` | 部署已停止，可随编辑启动（`start: true`） |
| `can_wake` | 部署处于睡眠命令下，可随 `apply: "all"` 一并撤销（`wake: true`），替换出的实例醒着启动 |
| `apply_note` | 生效方式比部署状态本身允许的更少时给出原因；`partition_change` 见下 |
| `revision` | 计划所依据的配置 |

| 部署状态 | `apply_options` | 默认 |
|---|---|---|
| 改变分区形态（开关 dGPU、换分区后端），`apply_note` 为 `partition_change` | `all`、`none` | `all`（处于睡眠命令下为 `none`） |
| 运行中，至少两个副本且至少一个在服务 | `rolling`、`all`、`none` | `rolling` |
| 单副本，或没有副本在服务（启动中、失败） | `all`、`none` | `all` |
| 处于睡眠命令下 | `none`、`all` | `none` |
| 已停止 | `none` | `none` |

- `rolling` 同[重启策略](#重启策略)的滚动重启：逐个替换副本，失败的尝试退役、重试次数重置。
- `all` 立即替换全部实例，期间服务中断，失败的尝试同样退役。
- `none` 只写入配置，实例下次启动时生效。
- 改变分区形态时，旧实例要全部退出后新实例才能启动（dGPU 的缓存与卡上占用按实例记录，新旧两代不能同时存在），所以不能滚动替换，服务会中断到新实例就绪；只调整 dGPU 显存大小不算改变分区形态，可以滚动。
- 只有 `live` 层变化时不重启任何实例，`apply` 只能省略或为 `none`。

`apply` 不在 `apply_options` 中、`start` 用于未停止的部署、`wake` 不在睡眠命令下或未配合 `all`，均 400 `DEPLOYMENT_EDIT_APPLY_INVALID`（`params.apply`、`params.options`）。

不带 `dry_run` 时在一个事务里写入配置、节点条件和生效方式，返回 200 `{"plan": {...}, "deployment": {...}}`，`plan.revision` 为编辑后的新 revision。没有任何变化时直接返回，不写入、不记录。

冲突与限制：

- `base_revision` 缺省 400 `FIELD_INVALID`；与当前配置不符（部署在读出 draft 后被改过），或写入时配置、节点条件、期望状态已变化，409 `DEPLOYMENT_CHANGED`。滚动重启进度、睡眠级别等控制器自己推进的状态不算冲突。
- 需要重启时与重启接口同样检查共享存储，存储不可达 409。
- 部署删除中 409 `DEPLOYMENT_DELETING`，不存在 404。

时间线记录 `DEPLOYMENT_EDITED`，含 `fields`（改动的字段，逗号分隔）、`count` 与 `apply`。

`POST /api/v1/evaluate` 可带 `deployment_id`：评估把它当作该部署的编辑，计算放置时不计入这个部署现有实例占用的显存，也不建议休眠它自己的实例。

### 修改节点条件

`PUT /api/v1/deployments/{id}/requirements`

不重新部署，替换普通单机部署的节点条件与放置策略：`{"node_requirements": {...}, "placement_strategy": "spread"}`（策略不写则保持）。只影响此后启动的副本，运行中的副本不迁移（重启副本即按新条件重新放置）。

厂商由部署镜像决定：不写则保留，写成别的厂商 400 `NODE_REQUIREMENTS_INVALID`；条件校验同创建；与创建一样做「永远放不下」检查，新条件下没有一台节点空载时放得下一个副本则 400 `DEPLOYMENT_NEVER_FITS`，原条件不变。

多机与 PD 部署逐 rank 列节点，400 `NODE_REQUIREMENTS_NOT_EDITABLE`（`params.shape` 为 `multi_node` 或 `pd`）；删除中 409 `DEPLOYMENT_DELETING`；成功返回部署详情。

### 调整显存上限

`PUT /api/v1/deployments/{id}/dgpu` 请求示例：`{"vram_limit_gib":12}`，仅适用于单机 dGPU 部署。
新实例使用新上限；运行中的实例尽量不重启、立即调整。返回 200 `{deployment,live,pending}`：

- `live`：已立即生效的实例 UUID。
- `pending`：须重启才生效的 `{instance_uuid,reason,used_bytes}`。
  `reason` 为 `backend`（后端只在启动时读取）、`in_use`（用量超过新上限）、`starting`、`asleep` 或 `no_usage`。

调用限制：

- 整卡部署：400 `DGPU_QUOTA_NOT_PARTITIONED`；多机/PD：400 `DGPU_QUOTA_NOT_EDITABLE`（`params.shape`）。
- 上限非正：400 `DGPU_QUOTA_INVALID`；新上限下无节点能放下：400 `DEPLOYMENT_NEVER_FITS`。
- 调高时所在卡扣除自身占用后仍放不下：409 `DGPU_QUOTA_NO_ROOM`（`params.node`、`gpu`、`free_gib`、`need_gib`）。
- 部署删除中：409 `DEPLOYMENT_DELETING`。

时间线记录 `DEPLOYMENT_DGPU_QUOTA_CHANGED`，含 `from_bytes`、`to_bytes`、`live`、`pending`。

### 推荐多机副本节点

`POST /api/v1/deployments/{id}/units/suggest`

为多机部署再推荐一个副本的节点，由用户在「调整副本」里确认后随列表保存（不写任何东西）：请求体 `{"units": [...]}` 是编辑器里已列出的副本（它们的 leader 不能再当 leader；部署还没有的副本按每 rank 卡数占用这些节点）。

推荐的节点共享同一计算网络与 GPU 架构、各有一个 rank 的空闲卡、可调度且在线、满足部署节点条件，每个 rank 都按部署已保存的配置试算；最多看最好的 3 组、每 rank 试 3 台。

返回 `{"unit": {"node_uuid", "followers"}, "network": {"id","name"}, "arch", "nodes": [{"uuid","name","free_gpus","model"}]}`；放不下时 `unit` 为 `null`，`reason.code` 为 `unit_none`（各类被排除的节点数与最大可组台数）或 `unit_no_fit`（第一台被拒节点及原因）。非多机部署 400 `REPLICAS_BODY_SHAPE`。

### 重启策略

`POST /api/v1/deployments/{id}/restart` 适用于所有部署形态，按当前网络与镜像重建，重置每个副本的重试次数并退役失败尝试。
请求体可省略，或提交 `{"strategy":"rolling"}` / `{"strategy":"all"}`：

- `rolling`（默认）：逐个替换副本。旧副本进入 `draining`，完成已有请求后退出，同时启动替代实例；恢复服务后再处理下一个。
  普通多机副本复用 leader 的 master 端口，须等旧一代退出后，替代实例才能在同一节点启动。
  进行中返回 `restart_requested_at`，时间线记录 `DEPLOYMENT_ROLLING_RESTARTED{replicas}`。
- `all`：立即停止全部实例再一起重建，期间服务中断；记录 `DEPLOYMENT_RESTARTED{instances}`。

未知策略返回 400 `RESTART_STRATEGY_INVALID`；非 active 部署返回 409 `DEPLOYMENT_NOT_ACTIVE`。

### 唤醒等位与让位唤醒

`POST /api/v1/deployments/{id}/wake` 与 `POST /api/v1/instances/{uuid}/wake` 的 `?wait=true` 和 `?yield=true`：

- `wait`：放得下的先醒，其余等卡腾出来，服务端容量释放后自动唤醒，等满 30 分钟放弃（时间线 `WAKE_WAIT_TIMEOUT`）。等位起点保存在实例上，部署详情的 `wake_waiting_since` 是其中最早的一个。应答 202 `waiting` 的同时，等位实例的保持休眠（实例的 `sleep_held`）就解除了：它们是要醒的，等位期间到达的请求由网关挂起、等这次唤醒（驻留仲裁也可以按出让策略为它腾地方），而不是回 503；以前保持要等唤醒落地才解除。
- `yield`：先把占着所需卡的醒着的部署休眠（`yield_level` 为 1 或 2，默认 1；1 就是出让策略的 `auto`：占用者的主机内存放不下 L1 时自动改 2，不因此拒绝），再等位。占用者的出让策略不是 `never` 时这次休眠请求能唤醒（不保持），`never` 的保持休眠（与 `sleep_wakes_on_request` 同一条规则）。占用者的休眠记 `YIELDED_FOR_WAKE`，带 `for_deployment`（为谁让位）和 `wake_on_request`，按调度记录与统计里的「让位」计数。
- 全部放得下时 200 `{"status":"waking","woken":[…],"waiting":[],"yielded":[],"yielded_held":[]}`；有的要等时 202 `{"status":"waiting",…}`，`waiting` 是在等的实例 UUID，`yielded` 是被让位休眠的部署名，`yielded_held` 是其中被保持休眠的。
- 占用者不能休眠（部署不可休眠、PD 最后一个醒着的角色、平台外进程）时 409 `WAKE_YIELD_BLOCKED`（`params.occupants` 名字列表、`woken`），什么都不改；`yield_level` 不是 1 或 2 时 400 `SLEEP_INVALID_LEVEL`。

不带这两个参数的唤醒行为不变。用法见[操作 sleep / wake](../how-to/sleep-wake.md#放不下时等位与让位)。

### 出让策略

`PUT /api/v1/deployments/{id}/yield-policy` 设置部署怎样与共用同一批卡的其他部署相处。出让策略是唯一要选的东西。规则见[出让策略：几个部署怎样共用一批 GPU](../explanation/service-levels.md)，操作见[使用调度分析](../how-to/scheduling-center.md#出让策略)。请求体：

```json
{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}
```

除 `policy` 外的键都可省略，省略的取该策略的默认值，不适用的键被忽略（`idle_neighbours` 也是），所以响应可以原样当请求体发回。

| 字段 | 取值 | 适用于 |
|---|---|---|
| `policy` | `never` 不自动出让、`idle` 闲置自动出让、`timeslice` 按时自动出让、`anytime` 随时出让；按等级从高到低 | 总是必填 |
| `idle_sec` | 15–604800 秒：多久没有请求算闲置，闲置满这么久，有部署等它的卡就让；`timeslice` 时不得超过 `max_serve_sec` | `idle`、`timeslice`、`anytime` |
| `sleep_level` | `auto`（L1，主机内存放不下权重时退回 L2）、`l1`（严格 L1，放不下就拒绝）、`l2`；适用于平台替该部署选的每一次休眠（让卡），运维与定时计划下的休眠不受影响 | 策略不是 `never` |
| `max_serve_sec` | 30–86400 秒：与另一个 `timeslice` 部署轮流时，一次醒来最长服务多久，到了就让 | `timeslice` |
| `unfinished` | `abort` 立即中断；`wait` 等待完成，最多 `unfinished_wait_sec` 秒后中断剩下的 | `timeslice`、`anytime` |
| `unfinished_wait_sec` | 1–600 秒 | `unfinished` 为 `wait` |
| `hold_max_sec` | 10–3600 秒：网关为等该部署唤醒最多挂起一个请求多久，超时 503 并带 `Retry-After` | 策略不是 `never` |

各策略的默认值：

| 策略 | 默认值 |
|---|---|
| `never` | 无参数 |
| `idle` | `idle_sec` 60，`sleep_level` `auto`，`hold_max_sec` 300 |
| `timeslice` | `idle_sec` 30，`max_serve_sec` 120，`unfinished` `wait`、`unfinished_wait_sec` 30，`sleep_level` `auto`，`hold_max_sec` 600 |
| `anytime` | `idle_sec` 30，`unfinished` `abort`，`sleep_level` `auto`，`hold_max_sec` 3600 |

成功返回 200 和存下的策略，不适用的参数不出现。部署详情的 `yield_policy` 同样的形状。响应示例：

```json
{"policy":"never"}
{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300}
{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}
{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","hold_max_sec":3600}
```

设为 `idle`、而卡上已有另一个 `idle` 部署时仍是 200，响应多一个 `idle_neighbours`（对方的部署名数组）：两个 `idle` 部署都忙时谁也不让，一方的请求被网关挂起，直到对方闲置，或挂到 `hold_max_sec` 后得到 503；要让它们轮流，用 `timeslice`。

错误：

| 情形 | 响应 |
|---|---|
| `{}`，或只带参数不带 `policy` | 400 `YIELD_POLICY_INVALID`，`params` 为 `{"field":"policy","reason":"required"}` |
| 取值不合法 | 400 `YIELD_POLICY_INVALID`，`params.reason` 为 `invalid_value`，`params.field` 为 `policy`、`sleep_level` 或 `unfinished` |
| 数值越界 | 400 `YIELD_POLICY_INVALID`，`params.reason` 为 `out_of_range`，`params.field` 为 `idle_sec`、`max_serve_sec`、`unfinished_wait_sec` 或 `hold_max_sec`，另带 `params.minimum`、`params.maximum`；`timeslice` 的 `idle_sec` 超过 `max_serve_sec` 时，`idle_sec` 的 `maximum` 就是 `max_serve_sec` |
| 策略不是 `never`，而部署没打开可休眠 / 引擎不支持休眠 | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |
| 多机或 PD 部署设了 `never` 以外的策略 | 400 `YIELD_POLICY_SHAPE` |

创建部署（`POST /api/v1/deployments`）时 `yield_policy` 取同样的请求对象，省略按 `never`；校验同上。

`GET /api/v1/scheduling/yield-policies`（JWT，只读）是策略的字典，客户端据此编辑，不另存一份默认值或范围：

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

| 字段 | 含义 |
|---|---|
| `policies[]` | 按等级从高到低；`defaults` 是选这个策略时填入的值（凡适用 `unfinished` 的都带 `unfinished_wait_sec`，供打开等待的客户端使用）；`applies` 是可能适用的参数名，按编辑器列出的顺序 |
| `applies_when` | 只在另一个参数取某些值时才适用的参数：`{参数: {另一个参数: [取值…]}}`，目前只有 `unfinished_wait_sec` 在 `unfinished` 为 `wait` 时适用；没有出现在这里的参数，凡在 `applies` 里就一律适用。编辑器读它，不自己写这条规则 |
| `bounds` | 各数值参数的范围，单位秒；`at_most` 指出同时适用时不得超过的另一个参数 |
| `sleep_levels`、`unfinished` | 两个枚举参数的取值 |

没有令牌 401，`POST` 等其他方法 405。

时间线事件 `AUTO_SLEEP` 的 `trigger` 为 `schedule`（定时计划）或 `preempt`（让卡，`reason` 为 `idle` / `rank` / `turn`）；`level` 总是平台实际选的级别（1 或 2）。事件码见[使用调度分析](../how-to/scheduling-center.md#时间线事件)。

### 定时计划

`/api/v1/deployments/{id}/schedule` 管理一个部署的定时计划。计划对象：

```json
{"id":3,"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T14:00:00Z"}
```

| 字段 | 含义 |
|---|---|
| `action` | `sleep`、`wake` 或 `replicas` |
| `weekdays` | 数组，0 为周日到 6 为周六，至少一个且不重复 |
| `time` | `HH:MM`，24 小时制 |
| `timezone` | IANA 时区名；空字符串用 server 所在时区 |
| `sleep_level` | `sleep` 必填，1 或 2；其他动作不接受 |
| `replicas` | `replicas` 必填，1 到 `max_replicas`（PD 角色为 1 到 32，模式允许为空的角色可为 0）；用于单机部署和自动放置的多机、PD 部署，其他动作不接受。响应里 `replicas` 动作的这个字段总会给出（包括 0），其他动作省略 |
| `role` | `replicas` 动作的 PD 角色（模式里的角色，如 `prefill`、`decode`）：PD 部署必填，其他部署和其他动作不接受（400 `SCHEDULE_INVALID`，`params.field` 为 `role`）；响应里非 PD 的计划省略 |
| `enabled` | 请求里可省略，默认 true |
| `next_at` | 响应字段，下一次执行时间；停用时省略，时区 server 不认识（新建时会被拒绝，只会出现在 server 之后丢了该时区数据时）时也省略，评估器同样不执行这样的计划，两边一致 |

每个时间点只执行一次；server 停机错过的只补最新的一次且不早于 12 小时；手动操作不被覆盖；新建或修改的计划从下一个时间点起生效。语义见[使用调度分析](../how-to/scheduling-center.md)。

### 调度看板

`GET /api/v1/scheduling/board?page=1&page_size=20&q=` 只读，响应（没有 `recent`；最近的自动化事件看[调度记录](#调度记录)）：

| 字段 | 含义 |
|---|---|
| `nodes` | 分页对象（`items`、`page`、`page_size`、`total`），节点按名称排序，`q` 匹配名称、主机名或 UUID 的一部分（不分大小写，与节点列表一致）。每个节点含 `uuid`、`name`、`online`、`gpus[]`：`index`、`name`、`memory_bytes`、`awake_bytes`、`sleeping_bytes`、`free_bytes`、`state`（卡的状态，取值同[调度总览](#调度总览)的 `states`：`offline`、`unhealthy`、`held_outside`、`awake`、`asleep`、`free`，服务端算出）、`held_outside`、`switching`（卡上有实例正在启动、休眠、唤醒或排空）与 `occupants[]`（`deployment_id`、`deployment`、`yield_policy`、`instance`、`state`：`awake` / `asleep`、`sleep_level`、`claim_bytes`、`residue_bytes`） |
| `pending` | 最近 30 分钟被调度拒绝的 active 部署：`deployment_id`、`deployment`、`reason{code,params}`、`at` |
| `waiting_wakes` | 等位中的唤醒：`deployment_id`、`deployment`、`instances`、`since` |
| `upcoming` | 未来 24 小时的计划动作：`deployment_id`、`deployment`、`schedule_id`、`action`、`sleep_level`、`replicas`（`replicas` 动作才有，含 0）、`role`（PD 角色的 `replicas` 动作）、`at` |

`?node=<uuid>` 只读这一个节点：`nodes` 是只有它的一行页（`page` 与 `page_size` 为 1，`total` 为 1，忽略 `page`、`page_size`、`q`），`pending`、`waiting_wakes`、`upcoming` 为空数组；节点不存在返回 404。控制台打开一个节点的抽屉时读它，开销与集群大小无关。`?queues=1` 相反，只回答 `pending`、`waiting_wakes`、`upcoming`，`nodes` 为空页，不读任何节点。

时间线事件码见[使用调度分析](../how-to/scheduling-center.md#时间线事件)与[节点失联重调度](../how-to/node-failover.md#时间线事件)。

### 调度总览

`GET /api/v1/scheduling/overview` 只读，一次返回全集群每个节点的每张卡，每张卡只有热力图需要的几个小整数，不含占用者的明细（明细用 `GET /scheduling/board?node=` 读）。1000 个节点、每节点 8 张卡、约 3000 个实例时，响应约 290 KB（gzip 后约 25 KB）。

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

| 字段 | 含义 |
|---|---|
| `summary` | 全集群的卡数。每张卡只计入第一个成立的桶，顺序为：`offline`（节点不在线）、`unhealthy`（卡故障或因所有权、健康状态不可用）、`held_outside`（被平台外进程占用）、`awake`（有实例占着显存）、`asleep`（只有休眠者的残留）、`free`；六个桶之和等于 `cards` |
| `summary.contention` | `waiting_wakes`：有休眠实例在等位唤醒的部署数；`held_requests`：网关此刻挂起等唤醒的请求数；`yields_last_hour`：最近一小时为别的部署让位而休眠的次数。让位只有一个口径：驻留仲裁的抢占（`AUTO_SLEEP`，`trigger` 为 `preempt`）和运维的让位唤醒（`YIELDED_FOR_WAKE`）都算，时间线的 `yielded`、`yielded_to` 数的是同一批事件；让位唤醒同样记 `for_deployment` |
| `states` | 卡的状态名，按上面互斥桶的先后：`offline`、`unhealthy`、`held_outside`、`awake`、`asleep`、`free`；卡用下标引用它。一张卡属于哪个状态由服务端判定（调度看板的 `gpus[].state` 是同一份结果），客户端只负责配色和措辞，不自己再推一遍 |
| `models`、`deployments` | 卡引用的字典：GPU 型号名；占着卡的部署（`id`、`name`、`yield_policy`：`never`、`idle`、`timeslice` 或 `anytime`）。各按首次出现的顺序 |
| `nodes[]` | 按名称排序。`labels` 是运维写的节点标签（没有则省略）；`contention` 是该节点上等位的休眠实例数，加上为在该节点上休眠的部署挂起的请求数，再加上该节点上有卡的部署的让位次数（没有则省略） |
| `nodes[].cards[]` | 数组 `[index, model, memory_mib, awake_pct, sleeping_pct, state, switching, occupants]`：`index` 是卡在节点上的设备序号；`model` 是 `models` 的下标；`memory_mib` 是显存总量（MiB）；`awake_pct` / `sleeping_pct` 是醒着的实例占用、休眠者残留各占显存的百分比（0 到 100，非零不会舍入成 0，两者之和不超过 100）；`state` 是 `states` 的下标；`switching` 为 1 表示卡上有实例正在切换（启动、休眠、唤醒、排空中），否则 0（取代旧的 `flags` 位）；`occupants` 没有时整项省略（此时数组只有 7 项），否则是 `下标 × 2 + 1（休眠时）` 的整数列表，下标指 `deployments` |

只列节点自己拥有、设备序号有效的卡，与看板一致。

响应来自 server 内存里的缓存，不为每个请求重算：节点、实例、部署的事件让缓存失效，但重建最多每 2 秒一次（节点的状态上报很频繁），没有事件时每 10 秒重建一次以带上网关挂起的请求和一小时的让位数。重建时只读一次节点、一次存活实例、一次部署，再按看板同样的账本算法一次算完，不按节点逐个读。响应带 `ETag`（内容的哈希），带 `If-None-Match` 且内容没变返回 304。

### 驻留时间线

`GET /api/v1/scheduling/timeline?node=<uuid>&node=<uuid>&window=6h` 只读。它与看板分开，因为看板每隔几秒刷新，而泳道要读窗口内的事件日志；界面按被打开的节点和所选窗口读一次，约每分钟刷新。带 `node` 时窗口可选 `15m`、`1h`、`6h`、`24h`（默认 `6h`），也可以用 `since`、`until`（RFC 3339，`until` 缺省为现在，至多 7 天）读一段自定义的范围；自定义范围固定，界面只读一次、不轮询，快捷窗口每分钟刷新。`?sharing=1&window=6h` 不带 `node`：只回答全集群出让策略不是 `never` 的部署、以及窗口内参与过让位后已删除的部署的统计（`deployments[]`，`nodes` 为空数组），窗口可选 `15m`、`1h`、`6h`、`24h`（默认 `6h`），缓存约 30 秒；界面用它显示「共卡部署」表。两种读法的统计都不读事件日志：`sharing=1` 是各部署在整个集群里的值，带 `node` 只统计这些节点上的副本，响应的 `scope` 说明是哪一种；可选的窗口由响应的 `windows` 给出，界面据此显示，不另存一份。旧参数 `hours=6|24` 仍可用，等同 `window=6h|24h`。响应：

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

自定义范围的 `window` 是 `{"from": "2026-10-28T08:30:00Z", "to": "2026-10-28T10:00:00Z", "hours": 1, "key": "custom", "seconds": 5400, "custom": true}`。

| 字段 | 含义 |
|---|---|
| `scope` | `deployments[]` 里的数统计的范围：带 `node` 时为 `node`（只统计部署在这些节点上的副本），`sharing=1` 时为 `fleet`（部署的全部副本）；界面据此显示「只统计本节点上的副本」的提示，不自行推断 |
| `window` | 实际回答的范围：`key`（`windows` 之一；自定义范围为 `custom`）、`custom`（是否自定义范围）、`seconds`（长度）、`hours`（整小时数，不足一小时为 0）、`from`、`to`（快捷窗口为现在，自定义范围为请求的 `until`） |
| `windows` | 这一读法可选的窗口（`key`、`seconds`），按界面显示的顺序：两种读法都是 `15m`、`1h`、`6h`、`24h`；自定义范围不在其中 |
| `switches_available` | `deployments[]` 里的 `switches`、`yielded`、`yielded_to`、`wakes` 是否已知。这四个数来自调度活动的内存，只保留最近 24 小时，所以带 `node` 的范围早于这个时间开始时为 `false`，这四个数是 0 而不是真的 0，界面显示 —；其余数不受影响。快捷窗口和 `sharing=1` 恒为 `true` |
| `nodes[].gpus[]` | 与看板同样的卡（`index`），`segments[]` 从旧到新，至多 200 段。一张卡换手次数多到放不下时，保留最近 200 段，更早的时间汇总进 `summary[]`：`summarized_before` 是汇总与逐段记录的分界（汇总覆盖窗口起点到此刻之前，`segments[]` 从此刻起），跨过分界的段在分界处切开，之前的部分进汇总、之后的部分留作逐段记录；`truncated` 为 true 表示有更早的历史没有逐段列出（`summarized_before` 出现时恒为 true）。没有汇总时不出现 `summarized_before` 与 `summary`，这张卡的形状与以前相同；不认识 `summary` 的旧客户端只读到最近的 `segments[]` 与 `truncated`，与以前一致。每张卡至多 200 段加 50 个汇总 |
| `summary[]` | 汇总的一个时间段，把窗口起点到 `summarized_before` 平均切成至多 50 段（`kind` 恒为 `summary`；逐段记录没有 `kind`）：`from`、`to`；`switches`，这段时间里这张卡换手的次数；`dominant_deployment_id`、`dominant_deployment`，这段时间里醒着最久的部署（同样长取 id 小的，这段里没有部署醒着时省略；为兼容旧客户端保留，控制台已不读它，改画每个部署自己的 `state`）；`deployments[]`，这段里占过这张卡的部署各在 `awake_seconds`（提供服务）、`asleep_seconds`（休眠，不分级别）、`switching_seconds`、`starting_seconds` 里待了多少秒，按提供服务的时长从长到短排，并带 `state`：它在这一段里待得最久的状态，取值与逐段记录的 `state` 相同（`awake`/`asleep`/`switching`/`starting`），按未取整的毫秒比较，时长一样长时按 `awake`、`switching`、`starting`、`asleep` 的先后取第一个；`state` 为 `asleep` 且这一段里休眠的分段带有级别时再带 `level`（1 或 2，与逐段记录的 `level` 同义），取休眠时间最长的级别（一样长取浅的），不知道级别的休眠不参与比较，都不知道时省略。由服务端算出，界面只按 `state` 配色。时间段数取窗口起点到分界的长度除以 1 分钟，至多 50、至少 1，每段等长；没有任何部署占过这张卡的时间段不列出。**换手**指卡上醒着的部署变成了另一个部署：按各个醒着的分段的起点顺序，相邻两个醒着分段属于不同部署就算一次，记在新的占用者开始的那个时间段里；同一个部署休眠又唤醒、同一部署的另一个实例接着醒着都不算。它是卡的换手次数，与 `deployments[]` 里的 `switches`（`yielded` + `wakes`，按部署各自的让出与唤醒计）口径不同：A 让给 B 一次，这里记 1 次，那里 A 记一次让出、B 记一次唤醒。各部署的秒数各自舍入到整秒；同一部署同一状态的重叠分段只算一次；不同部署同时占着这张卡时它们的秒数会重叠，没有部署占着时又会少于这一段的长度，所以加起来不一定等于这一段的长度 |
| `segments[]` | 一个实例在一段时间里占着这张卡：`deployment_id`、`deployment`、`instance`、`state`（`awake` / `asleep` / `switching` / `starting`）、休眠时的 `level`（1 或 2）、`from`、`to`（都落在窗口内）；多卡实例在每张卡上各出现一次 |
| `segments[].deployment_deleted`、`summary[].deployments[].deployment_deleted` | 部署已删除时为 `true`，仍存在时省略；`deployment` 保留删除记录中的名称。无论是否进入顶层共卡统计都提供此标记，客户端据此展示「（已删除）」且不提供详情链接 |
| `deployments[]` | 窗口内占过这些卡、出让策略不是 `never` 的部署（带各自的 `yield_policy`），以及窗口内让出过卡或被让过卡、此后已删除的部署（`deployment_deleted: true`，`deployment` 是它留下的名字，`yield_policy` 为空字符串，因为删除后不保留）：`awake_seconds`（至少一个副本醒着的时间）、`present_seconds`（`fleet`：部署在窗口内存在的时长，从 max(窗口起点, 部署创建时间与首个实例时间中较早者) 到 min(现在, 删除时间)，已删除的部署不保留创建时间，按它的实例在窗口内最早出现的时间算，删除时间未知时按最后一段的结束；`node`：窗口内至少一个副本在这些节点上占着卡的时长，即这些节点泳道上它各段的并集）、`awake_share`（`awake_seconds` / `present_seconds`，0 到 1，`present_seconds` 为 0 时为 0）、`switches`（`yielded` + `wakes`）、`yielded`（为别的部署让出卡而休眠，驻留仲裁的抢占与运维的让位唤醒都算，与总览的 `yields_last_hour` 同一口径）、`yielded_to`（别的部署为它让出，按事件的 `for_deployment` 计，让出者已删除也计）、`wakes`（平台唤醒它）、`held_requests`（网关为等它唤醒而挂起的请求数）、`wait_p50_ms` / `wait_p95_ms`（这些请求的等待，近似值，没有被挂起的请求或网关不保留指标时省略）、`cards`（各副本此刻所在的节点与卡，按节点：`node_id`、`node_uuid`（节点已不存在时省略）、`node`（节点显示名，节点已不存在时为空）、`gpus`；没有放置时为空数组）。`scope` 为 `fleet` 时这些数（切换计数、`awake_seconds`、`present_seconds`）是部署自身在整个集群里的值，来自[调度活动](#调度活动)的内存汇总；为 `node` 时只统计部署在这些节点上的副本：`awake_seconds` 与 `present_seconds` 由与泳道同一份分段算出，`yielded`、`yielded_to`、`wakes` 来自调度活动内存里按节点记下的计数（事件涉及的卡在这些节点上才计，同时涉及多个所列节点的事件只计一次；最近 24 小时都在内存里，不新增持久化）。带 `node` 时只列出在这些节点的卡上出现过的部署；`held_requests`、等待分位与 `cards` 两种范围相同：网关为部署而不是为某个副本挂起请求，`cards` 列出它所有副本此刻所在的节点与卡 |

自定义范围 [`since`, `until`] 的泳道这样推导：窗口内的事件只读到 `until` 为止；`until` 之后才创建的实例不出现；`until` 之后才被移除的实例仍占着卡到 `until`；每个实例在 `since` 时的状态取 `since` 之前最近的事件，与快捷窗口相同。这一读法按节点页、范围各读一次，答案在 server 内存里缓存：快捷窗口（15m / 1h / 6h / 24h）按窗口与节点页缓存约 30 秒，几个人每分钟轮询同一页共用一次读；`until` 已经过去一分钟以上的自定义范围缓存约 5 分钟；刚结束的范围每次都读。缓存最多 32 条，满了先丢过期的、再丢最旧的，元素（段加汇总）很多的答案不缓存；同一个键的并发请求等同一次读。快捷窗口的答案因此最多落后 30 秒：`window.to`（卡上还没结束的分段在这里结束）、部署此刻所在的卡（`cards`）、已发生的让出与唤醒数、节点及其 GPU 都按 30 秒前的值，页面本来就是每分钟刷新一次。汇总由同一次读出的分段算出，不多读存储；读取都是索引范围，不排序、不扫表。保留期之外的事件已被清理，更早的范围里只剩现存实例行能说明它们当时的状态。

泳道由实例记录与事件日志推导，移除实例时记录的 `INSTANCE_REMOVED` 事件（`kind` 为 `residency`，参数 `node_id`、`gpus`）说明它占过的节点与卡。语义与限制见[使用调度分析](../how-to/scheduling-center.md#驻留时间线)。

### 调度活动

`GET /api/v1/scheduling/activity?window=24h` 只读，按原因堆叠的调度活动与切换最频繁的卡。`window` 可选 `15m`、`1h`、`6h`、`24h`（默认）、`7d`，桶长依次为 1 分钟、1 分钟、5 分钟、15 分钟、1 小时；也可以用 `since`、`until`（RFC 3339，`until` 缺省为现在，晚于现在按现在算，范围至多 1825 天）给出自定义范围，服务端取能让桶数不超过约 200 的最短桶长（1、5、15、30 分钟，1、3、6、12 小时，1 天），且不短于范围开头所在那段数据的粒度：最近 24 小时按分钟，统计按小时保留期以内按小时，更早按天。响应缓存约 30 秒。

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

| 字段 | 含义 |
|---|---|
| `from`、`to`、`bucket_seconds` | 第一个桶的起点（按桶长对齐，可能略早于请求的起点；一天的桶按统计时区的日界对齐）、范围的终点、桶长（秒） |
| `reasons[]` | 全部原因，按堆叠顺序，带范围内的合计：`idle_yield`（驻留仲裁让闲置的占卡者让卡）、`rank_preemption`（排位更高的部署抢占可抢占的占卡者；没有写明原因的抢占也算在这里）、`timeslice_turn`（同级部署轮流使用，占卡者用满了它的时间片）、`operator_yield`（运维的让位唤醒，`YIELDED_FOR_WAKE`）、`scheduled`（定时计划的休眠、唤醒与改副本数）、`placement`（放置与迁移：平台把新副本放到节点上 `UNIT_PLACED`、把副本从运维排空的节点迁出 `NODE_DRAIN_MOVED`）、`failure`（副本重建、节点失联迁移及其回退与无处可迁、重试预算用尽、滚动重启停住、唤醒等位超时、自动指令失败）、`queued`（部署放不下 `DEPLOYMENT_UNSCHEDULABLE`、唤醒开始等位 `WAKE_WAITING`）。请求或恢复触发的唤醒（让位的另一半，已在让出的一侧计过，仍计入卡的切换）与运维指令不计入任何原因；节点失联后的迁移 `NODE_REPLACED` 属于 `failure` |
| `buckets[]` | 从 `from` 起每个桶一项（没有事件的桶也在），`counts` 与 `reasons` 同序 |
| `top_cards[]` | 范围内切换最频繁的至多 20 张卡，多者在前：`node_uuid`、`node`（节点显示名，节点已不存在时为空）、`gpu_index`、`switches`（卡上的让卡次数加唤醒次数，按事件涉及的实例当时所占的卡计） |
| `beyond_retention` | 范围开头早于统计按天保留期，那一段没有数据 |

数据从哪来：事件写入时，store 把调度事件和实例的状态、休眠、移除事件交给内存汇总，每个事件只在它所在的那一分钟上加几个数（原因计数、所涉卡、部署的让出 / 被让 / 唤醒及其所在节点），实例状态的变化维护各部署至少一个副本醒着、以及有副本占卡的时间段，不为每个事件写库。内存保留最近约 25 小时；server 启动时按 `(kind, id)` 与实例索引回放一次窗口内的事件。每个整点过后，上一小时整小时写入 stats.db 的三张小时表（各原因的事件数、每张卡的切换数、每个共卡部署的醒着与存在秒数和让出 / 被让 / 唤醒），重写同一小时得到同样的行；server 停止时写入进行中的那一小时。小时表与天表按数据保留设置（`GET /api/v1/data-retention`）的 `stats_hourly_days` / `stats_daily_days` 保留（默认 31 天与 400 天），天表在一天结束后由小时表汇总。读取时最近 24 小时只读内存，更早的部分读小时表或天表（按卡汇总走主键范围），不随事件日志的增长变慢。

### 调度记录

`GET /api/v1/scheduling/history` 只读，返回跨部署的调度事件，最新优先：

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

| 参数 | 含义 |
|---|---|
| `limit`、`before` | 同 `GET /api/v1/deployments/{id}/events`：`limit` 默认 100、上限 500；下一页传上一页最后一条的 `id` 作 `before` |
| `deployment_id` | 只看一个部署 |
| `category` | `auto`、`failure`、`manual`，可用逗号给多个；省略为全部 |
| `since`、`until` | RFC 3339 时间，含边界 |

| 响应字段 | 含义 |
|---|---|
| `id`、`created_at` | 事件 id（翻页游标）与 UTC 时间 |
| `deployment_id`、`deployment` | 部署及其名称；部署已删除时名称取自留下的记录并带 `deployment_deleted: true`，连名称都没有时省略 `deployment` |
| `instance` | 事件挂在某个实例上时的实例 UUID，否则省略 |
| `kind`、`code` | 事件类型（`schedule` / `automation`）与事件码 |
| `category` | `auto`：平台按设置或计划自己动手；`failure`：出了问题，平台在应对或已放弃；`manual`：运维下的指令 |
| `params` | 事件码的参数，同部署时间线 |
| `actor` | 下指令的用户名；只有 `manual` 事件、且请求带身份时才有，也在 `params.actor` 里 |
| `nodes` | 事件涉及的节点 `[{uuid?, name}]`，按顺序取第一个有结果的：事件参数里记下的节点（`node_uuids`；较早事件的 `node` 是节点 UUID 或名称，`nodes` 是名称），事件提到的实例（`instance`、`instances`，或事件所在的实例）现在所在的节点；都没有时省略整个字段。只有服务端仍认识的节点带 `uuid`（界面据此打开节点抽屉）；节点已不存在时 `name` 是 UUID 的前 8 位，较早事件只记了名称的就用这个名称。整页的节点一次查询得出 |
| `target` | 事件为之动作的部署 `{id, name?, deleted?}`，即 `params.for_deployment`（例如让卡时让给的那个部署）；没有则省略整个字段。部署已删除时带 `deleted: true`，名称取自留下的记录，连名称都没有时省略 `name`。界面据此在部署名之外再显示一个可点击的让卡对象 |

哪些事件码属于调度记录、各属哪一类，由 server 一处决定：

| 类别 | 事件码 |
|---|---|
| `auto` | `AUTO_SLEEP`、`AUTO_WAKE`、`AUTO_SCALE`、`WAKE_WAITING`、`WAKE_WAIT_TIMEOUT`、`SCHEDULE_ACTION_FAILED`、`YIELDED_FOR_WAKE`、`UNIT_PLACED`、`NODE_DRAIN_MOVED` |
| `failure` | `NODE_REPLACED`、`NODE_REPLACE_REVERTED`、`NODE_REPLACE_NO_CANDIDATE`、`REPLICA_REBUILT`、`UNIT_RETRY_EXHAUSTED`、`DEPLOYMENT_UNSCHEDULABLE`、`DEPLOYMENT_ROLLING_STALLED` |
| `manual` | `REPLICAS_CHANGED`、`REPLICA_RESTARTED`、`UNIT_RETRIED`、`DEPLOYMENT_RESTARTED`、`DEPLOYMENT_ROLLING_RESTARTED`、`SLEEP_ORDERED`、`WAKE_ORDERED` |

运维通过接口下的指令会在部署时间线记一条事件并带 `params.actor`：

| 事件码 | 何时记 | 主要参数 |
|---|---|---|
| `REPLICAS_CHANGED` | `PUT /deployments/{id}/replicas` 或 `PUT /deployments/{id}/pd` 改变了副本数（数量没变不记）。与同名的 409 错误码无关 | `from`、`to`，PD 另有 `role` |
| `SLEEP_ORDERED` | 部署、PD 角色或单个副本被下达休眠 | `level`、`mode`、`scope`（`deployment` / `role` / `replica`）、`role`、`instances` |
| `WAKE_ORDERED` | 唤醒让至少一个实例开始唤醒；全部在等位的唤醒不记（有 `WAKE_WAITING`） | `instances` |
| `DEPLOYMENT_RESTARTED`、`DEPLOYMENT_ROLLING_RESTARTED`、`REPLICA_RESTARTED`、`UNIT_RETRIED` | 重启与重试 | 各自原有参数 |

平台自己的动作（控制器、自动化）不带 `actor`。

控制器在失败的副本重建时记 `REPLICA_REBUILT`（部署时间线，`kind` 为 `schedule`），每次重建一条，数量受重试预算限制（见[修复后重试](../how-to/troubleshooting.md#修复后重试)）：

| 参数 | 含义 |
|---|---|
| `replica` | 副本序号，从 1 起 |
| `role` | PD 部署的角色 |
| `attempt` | 这是第几次尝试 |
| `reason` | 上一次尝试失败的消息，超过 200 个字符截断 |
| `node` | 新一次尝试的 leader 所在节点名称 |
| `node_uuids` | 同一节点的 UUID（列表） |

调度记录收录的事件在写入时带 `node_uuids`（事件涉及的节点 UUID 列表）：提到实例的事件记这些实例当时所在的节点，控制器的节点事件记它涉及的节点（迁出与迁入、失联、前后两次放置、失败尝试所在）；`DEPLOYMENT_UNSCHEDULABLE` 用已有的 `node`（节点 UUID）。对整个部署下的指令（定时休眠、改副本数、滚动重启）不带。逐码说明见[使用调度分析](../how-to/scheduling-center.md#时间线事件)。

读取走 `instance_events` 的 `(kind, id)` 索引，按事件类型各取一页再合并，不扫描整张时间线。

### 普通部署的副本调整

`PUT /api/v1/deployments/{id}/replicas` 用于调整普通部署的副本；多机与单机部署使用不同的请求体。

**手动放置的多机部署**提交期望的全部副本：`{"units":[{"node_uuid":"<leader>","followers":[{"node_uuid":"..."}]}]}`。

- 每个副本恰好有 `nnodes` 个 rank，且各在不同的已知节点，否则报 `REPLICA_UNIT_RANKS` / `REPLICA_UNIT_NODE`。
- 不能指定 GPU，否则报 `REPLICA_UNIT_GPUS`。
- 不同副本的 leader 不能在同一台节点，否则报 `REPLICA_LEADER_SHARED`；原因是 leader 监听部署的 master 端口。
- 同一节点上列出的所有 rank，不论属于哪个副本，所需 GPU 总数不能超过该节点的卡数，否则报 `REPLICA_UNIT_GPUS_INSUFFICIENT`。
- 新增或换了节点的副本还须在线（`REPLICA_UNIT_NODE_OFFLINE`）并满足部署的节点条件（`REPLICA_UNIT_REQUIREMENTS`）。已列出且节点与顺序都不变的副本原样保留，不因节点掉线或标签变化而挡住整个列表的修改。
- 换了首个副本的 leader 时，部署上存的 master 地址随之清空；每个副本在自己 leader 的参数网络地址上汇合。
- 从列表去掉的副本排空后退役；新副本各自成组，在自己的 leader 上汇合。响应的 `units` 列出当前副本及其重试情况。

**自动放置的多机部署**（`placement_mode` 为 `auto`）提交副本数：`{"replicas":N,"place_on":["<node uuid>"],"remove":["<uuid>"]}`，不带 `units`（带了 `units` 则按上面的列表原样写入）。

- 增加副本时，服务端按放置策略为新副本挑节点（同一副本内同计算网络、同 GPU 架构，每台有一个 rank 的空闲卡，leader 不与其他副本的 leader 同节点），写进该部署的副本列表；规则同[创建时挑节点](../how-to/multi-node-inference.md#创建时怎样挑节点)。
- `place_on` 可选，只在增加副本时使用：按顺序给新副本的 rank 指定节点（第一个新副本的 rank 0…N-1，再是下一个的），可以只给一部分，其余由服务端补。个数多于新增副本的 rank 总数 400 `REPLICAS_PLACE_ON_TOO_MANY`（`params.count`、`maximum`）；所写节点凑不成能放下的副本 409 `REPLICAS_PLACE_ON_REFUSED`（`params.node`、`reason` 为 `no_group`）；没有任何一组放得下 409 `UNIT_PLACEMENT_NONE`。
- `remove` 只在减少副本时使用，增加副本时带上返回 400 `REPLICAS_REMOVE_TOO_MANY`（`params.maximum` 为 0）。
- 减少副本时，`remove` 里的实例所在的副本先去掉（不属于本部署 400 `REPLICAS_REMOVE_INVALID`，多于减少的个数 400 `REPLICAS_REMOVE_TOO_MANY`），不写则去掉最后加入的；至少保留 1 个副本，否则 400 `REPLICAS_TOO_FEW`。

**单机部署**提交副本数：`{"replicas":N,"remove":["<uuid>"]}`。

- `place_on` 可选，仅增加副本时使用：节点 UUID 列表，新副本先在这些节点启动，只管第一次放置，提示保存在 server 内存里。个数多于新增的副本数 400 `REPLICAS_PLACE_ON_TOO_MANY`（`params.count`、`maximum`）；某节点不可用 409 `REPLICAS_PLACE_ON_REFUSED`（`params.node`、`reason`：`unknown`、`offline`、`cordoned`、`requirements`、`no_room`，视情况带 `detail` 或 `code` / `params`）。
- `remove` 可选，指定缩容时移除哪些实例的副本。这些副本进入 `draining`，处理完请求后退役；不指定时由服务端选择，先选未就绪的，再选最新创建的。
- 数量必须满足 1 ≤ N ≤ 部署的 `max_replicas`（当前 1024），否则报 `REPLICAS_TOO_FEW` / `REPLICAS_TOO_MANY`。
- `remove` 多于减少的副本数时返回 400 `REPLICAS_REMOVE_TOO_MANY`；指定的实例没有在承担本部署的副本时返回 400 `REPLICAS_REMOVE_INVALID`。
- 副本数或指定的实例在请求期间已被改动，例如并发扩缩或实例刚失败：409 `REPLICAS_CHANGED`，不写入任何改动。

**两种请求共用的错误条件：**

- 请求体与部署形态不符（PD、手动放置的多机传数量、单机传列表、手动放置的多机带 `place_on`）：400 `REPLICAS_BODY_SHAPE`。
- 部署正在删除：409 `DEPLOYMENT_DELETING`。

### 评估与创建的配置字段

`POST /evaluate` 与 `POST /api/v1/deployments` 共用部署配置请求；评估结果不预留资源。配置字段如下：

| 字段 | 语义 |
|---|---|
| `quick_params` | 按 `/engines` 参数字典声明的类型提交值；显式 `false` 与省略不同 |
| `quick_params_unset` | 本次明确不继承的参数 key 数组，阻止模板回填；不写入持久化配置 |
| `placement_mode` | `auto` 由服务端选择节点，`manual` 使用显式节点；省略按 `manual`。单机：只是请求里的说法，部署不存这个字段，手动放置存为节点条件 `gpuworks.ai/node: [node_uuid]`；自动放置的创建可以不带 `node_uuid`，服务端先按评估同样的规则挑一台再照常创建，没有节点放得下时返回 409 `AUTOMATIC_PLACEMENT_UNAVAILABLE`（`params.candidates` 为候选节点数，`reasons` 为各候选的原因码），带了评估选中的 `node_uuid` 则照旧复核，资源变化时返回 `AUTOMATIC_PLACEMENT_STALE`。多机与 PD：`auto` 时服务端在创建时为请求没写的 rank 或单元挑节点（见[自动放置多机与 PD 部署](#自动放置多机与-pd-部署)），部署保存为自动放置，详情的 `placement_mode` 为 `auto` |
| `target_vendor` | 评估与创建使用同一加速器厂商；存为节点条件 `gpuworks.ai/vendor`。未给时创建按落点节点的厂商补上 |
| `node_requirements` | 节点条件：`{"<key>": ["<value>", …]}`，同一键的多个值是「其中之一」，不同键「同时满足」。键可以是节点标签，也可以是系统标签（见上「节点标签」）；保留域里不是这八个系统键的一律拒绝（拼错的键不会悄悄匹配 0 台），`gpuworks.ai/vendor` 只能有一个值，网络键的值是网络 ID 或 `management`。多机与 PD 部署逐 rank 列节点，不接受 `gpuworks.ai/node`。错误 `NODE_REQUIREMENTS_INVALID`；所选节点不满足 `NODE_REQUIREMENTS_UNMET`（`params.detail` 写明哪个键） |
| `yield_policy` | 出让策略，对象，形状同 `PUT /deployments/{id}/yield-policy` 的请求体（`policy` 加参数）；省略按 `never`。`never` 以外的策略要求 `sleepable`，策略不是 `never` 还要求单机部署，否则 400 `YIELD_POLICY_INVALID` / `SLEEP_NOT_ENABLED` / `YIELD_POLICY_SHAPE`。评估按它计算放置：按出让策略共卡的部署睡着时，对不与它共卡的部署仍按全量占用 |
| `placement_strategy` | `spread` 或 `binpack`，随部署保存；其它值 `PLACEMENT_STRATEGY_INVALID`。没写时单机是 `spread`，多机与 PD（不论自动还是手动）是 `binpack` |
| `placement.gpus`、`placement.tp`、`placement.pp` | 明确的物理 GPU 索引；TP、PP 是整个部署的并行度（每节点 TP × PP ÷ `nnodes` 张卡，必须整除）。多机手动放置未给 TP 或 PP 时（不是正整数的 TP、PP 按未给处理），按模型和各节点当前空闲的卡试算，每节点按 1、2、4、8 张翻倍试算，再试节点的全部卡，给了 TP 时试每一个能得出该 TP 的卡数，推荐放得下的最少卡数：同样卡数优先每节点一个流水线阶段，模板有放置偏好时偏好优先，KV 估算装不下上下文的组合排在最后；没有可行组合时评估与创建都返回 `DISTRIBUTED_PARALLELISM_UNAVAILABLE`（`params.node` 为拒绝的节点，`params.reason` 为调度器原因码或 `no_layout`，`params.reason_params` 为该原因自带的参数；评估把它挂在留空的 TP 或 PP 上），不会排队等卡；模型权重大小未知时取默认值：每节点一个流水线阶段、TP 取所选节点中最少的 GPU 数。TP、PP 都给时按固定配置校验，卡暂时不够可以排队。生效值写回 placement；单机未给 TP 时由调度器按 PP 的整数倍挑卡（TP = 卡数 ÷ PP），自动放置同样按所填 TP、PP 选节点；评估在 `parallelism`（`tp`、`pp`、`nodes`、`gpus_per_node`）中返回生效值，`form_values.tp/pp` 不回显手动多机推荐的留空项；多机评估与手动放置的 `acquisition.nodes` 逐节点给出获取预估 |
| `routing_policy` | 多副本网关策略：`least_request`（默认）、`prefix_affinity` 或 `failover`；不属于引擎启动参数 |
| `tokenize_enabled` | 网关是否把 `/v1/tokenize`、`/v1/detokenize` 转给该部署（见 [OpenAI 兼容网关](openai-gateway.md#分词tokenize)）：省略为开启，`false` 关闭；与引擎无关，引擎不提供时由引擎自己回错 |
| `priority_scheduling` | 引擎是否按请求优先级调度（见[请求优先级](../explanation/request-priority.md)）：省略时，最终有效的 LLM 入口及参数允许就开启，否则关闭；显式 `true` 而不允许时创建与编辑回 400，码为拒绝原因（`PRIORITY_SCHEDULING_ENGINE_UNSUPPORTED`、`PRIORITY_SCHEDULING_LAUNCH_UNSUPPORTED`、`PRIORITY_SCHEDULING_TARGET_UNVERIFIED`、`PRIORITY_SCHEDULING_PD_UNSUPPORTED` 或 `PRIORITY_SCHEDULING_POLICY_CONFLICT`，`params.field` 为 `priority_scheduling`）。编辑省略时保持原值（参数不再允许时关闭）；修改需要重建副本 |
| `health_check`、`health_check_threshold_sec` | 服务的健康检查（见下「健康检查」）：`health_check` 省略时，模型提供 chat、completions、embedding 或 System One 决策就开启，否则关闭；显式 `true` 而模型都不提供时创建回 400 `FIELD_INVALID`。阈值 1–600 秒，省略或 0 为 60，越界时评估给字段错误、创建回 400，码均为 `FIELD_INVALID` |
| `start_timeout_sec` | 本部署实例的启动健康检查预算（秒）：实例启动后等引擎健康检查通过的最长时间。省略或 0 表示自动预算（集群默认 → 模板档位的 `start_timeout_sec` → 引擎等待上限抬高，见[启动等待上限](deployment-launch-sources.md#启动等待上限)），填了值则取代自动预算；须在 60–86400，越界时评估给字段错误、创建回 400，码均为 `FIELD_INVALID`。随部署保存，部署详情里有值时返回同名字段；值在实例创建时定下，修改只影响之后启动的实例 |

`quick_params.enforce_eager` 保留三态：省略继承模板或引擎默认，`false` 明确关闭并覆盖模板开启值，`true` 明确开启；评估的 `form_values`、再次评估与创建保留这个选择。vLLM 的 eager 参数是 `--enforce-eager`，SGLang 是 `--disable-cuda-graph`。

实例及评估响应的 `start_timeout_sec`、`start_timeout_source`、`engine_start_timeout_sec` 和 `start_timeout_conflict` 报告实际预算及冲突；`start_timeout_source` 是决定它的那一步：`cluster`、`template`、`engine` 或 `deployment`（见[启动等待上限](deployment-launch-sources.md#启动等待上限)），旧实例未记录来源时省略。`startup_estimate` 仅提供基础公式的粗略估算：`method=weight_reference` 时，`estimated_seconds = weight_bytes / (reference_mib_per_sec × 1024 × 1024) + initialization_seconds`，参考加载速率为 `512 MiB/s`；综合初始化余量按有效引擎/模式选择：SGLang `240 秒`、普通 vLLM `300 秒`、最终命令明确含 `--omni` 的 vLLM `420 秒`。这些是项目的粗略参考档，不是论文系数或节点实测；不按运行时、GPU 数或 eager 配置推断额外加减项。缺少权重时 `method=missing_weight`，不支持的配置为 `unsupported`；未知时省略数值估算及参考参数，不返回零来表示未知。估算不调整启动预算。实例 `measured` 中可选的 `StartupSeconds`、`StartupTimeoutSeconds`、`ModelLoadingSeconds`、`CompilationSeconds`、`GraphCaptureSeconds` 分别是成功健康等待、未就绪下界及日志明确耗时，彼此不能直接相加。

### 健康检查

开启后，服务端每分钟经网关发送一个最小请求，按网关策略选副本并等待至阈值：
chat 发送内容为 `1` 的消息、`max_tokens:1`；仅有 embedding 时发送输入 `1`；仅有 System One 决策时对状态 `1` 问一个是/否问题。

- 只报告结果，不重启或摘除副本；没有醒着且运行中的副本时不发送。
- 网关不计入请求数、token、TTFT/TPOT，不保存请求记录，也不刷新最近请求时间。
  引擎自己的指标仍可能计入每分钟一次、1 个 token 的检查，网关不能替引擎扣除。
- 仅在内存保留最近 10 次结果；重启服务端后一分钟内重新产生结果。

部署详情、列表与监控服务页的 `health` 字段如下：

| 字段 | 语义 |
|---|---|
| `status` | `ok`（阈值内回复）、`timeout`（阈值内没回复）、`failed`（回复了错误，或网关连不上副本）、`off`（未开启）、`no_replica`（没有可检查的副本，未发送）、`pending`（已开启、还没检查过） |
| `threshold_sec` | 阈值 |
| `last` | 最近一次检查 `{at_ms, status, latency_ms, http_status?, instance_uuid?, message?}`：`instance_uuid` 是回复它的副本，`message` 是错误回复的开头或没有回复的原因；超时的 `latency_ms` 记为阈值；没有时为 `null` |
| `recent` | 最近至多 10 次检查，旧的在前 |
| `recent_mean_ms`、`recent_ok` | 其中阈值内回复的平均耗时与次数；没有时为 `null`、`0` |

### 自动放置多机与 PD 部署

多机和 PD 的创建、评估请求写 `placement_mode: "auto"`，由服务端为请求没写的 rank 或单元挑节点；省略或写 `manual` 时节点由请求给出。

多机请求（`nnodes` > 1）：

```json
{
  "model_id": 3, "nnodes": 2, "placement_mode": "auto",
  "placement": {"tp": 8, "pp": 2},
  "node_uuids": ["<node uuid of rank 0, optional>"]
}
```

- 必须有 `placement.tp`，否则 400 `AUTO_PLACEMENT_TP_REQUIRED`；每台节点需要的空闲卡数是 TP × PP ÷ `nnodes`。
- `node_uuids` 可省略，也可以只给前几个 rank 的节点（多于 `nnodes` 个 400 `DISTRIBUTED_NODE_COUNT`），其余在这些节点所在的计算网络、GPU 架构里补；给的节点不在同一计算网络、同一架构里时按没有一组放得下处理（409 `UNIT_PLACEMENT_NONE`）。
- 多机仍只支持 1 个副本（`replicas` 为 1），其后用 `PUT /replicas` 按副本数扩容（见[普通部署的副本调整](#普通部署的副本调整)）。

PD 请求（`placement.pd`）：单元可以不写 `node_uuid`，写了的单元留在原处，平台只补没写的；自动补的单元不钉卡（`gpus` 由调度器启动时挑）；已经写了节点的单元要用的卡，在为其他单元挑节点时先扣掉。某个角色没写节点的单元带 `followers` 时 400 `AUTO_PLACEMENT_PD_MULTI_NODE`（`params.role`）：自动放置的单元必须一台节点放得下。`PUT /deployments/{id}/pd` 增加单元同理，新单元可以不写节点。

挑选规则（多机以一个副本为单位，PD 以整个部署为单位；只在在线、可调度、满足节点条件且有空闲卡的节点里挑）：

1. 一个副本内（PD 是整个部署内）的节点同一张计算网络、同一种 GPU 架构；PD 已有单元时，新单元加入它们所在的网络和架构。
2. 组之间：RDMA 网络（主机 RDMA、加速器 RDMA）优先于以太网，再看已有模型文件的节点多的组。
3. 组内：已有模型的节点优先，再按 `placement_strategy`（没写时多机与 PD 是 `binpack`）：`binpack` 取空闲卡最少但够用的，`spread` 取最多的，最后按节点编号定。PD 在 `binpack` 下，一台节点放得下全部单元就都放这一台；`spread` 下 prefill 与 decode 尽量不在同一台。

没有一组放得下返回 409 `UNIT_PLACEMENT_NONE`，`params` 为 `nodes`（节点总数）、`unmet`（不满足节点条件）、`cordoned`、`offline`、`busy`（空闲卡不够）、`no_network`（没有可用的计算网络）、`no_peer_memory`（PD 模式经 RDMA 传 KV 而节点未加载 peer memory 模块）、`largest`（最大的一组有几台）；PD 模式经 UCX 传 KV、参数网络不是主机 RDMA、且有节点内核会让 UCX 卡住（见节点的 `ucx_netlink_hang`）而被跳过时另有 `ucx_netlink_hang`（个数）与 `ucx_netlink_nodes`（前几个节点名），否则没有这两个字段。PD 模式经 RDMA 传 KV、且有节点因未加载 peer memory 模块被排除时，改为 409 `PD_PEER_MEMORY_NONE`（`params`：`mode`、`nodes`、`no_peer_memory`、`module`、`fix` 修复命令）；手动放置到这样的节点返回 `PD_PEER_MEMORY_MISSING`（`mode`、`role`、`node`、`module`、`fix`、`state`）。评估对这些失败不返回 HTTP 错误，而是 `ok=false`、`errors[]` 里 `field` 为 `placement_mode`，响应带 `auto_placed: true`。

评估成功时响应带 `auto_placed: true`，预览按此刻挑中的节点算；创建时重新挑一次，所以两次的节点可能不同；创建不提交评估选中的节点，也就没有单机自动放置那样的 `AUTOMATIC_PLACEMENT_STALE`，资源在评估后被占走时，由单元启动时的重新挑选处理。部署保存的是挑中的节点（写在多机的 `node_uuids` 或 PD 的单元里），详情的 `placement_mode` 为 `auto`。

单元启动时，控制器按当前空闲卡试算它的节点：节点都在线且放得下就启动；节点已停止调度、不满足节点条件或会被调度拒绝（卡被其他部署占了）时，只把这些 rank 换到别的节点并记 `UNIT_PLACED`（`params`：`unit`（从 1 起）、`role`、`from`、`to`（按 rank 的节点名）、`reason`）；没有地方可去时保留节点继续等待，约每分钟重试。离线节点归换节点，被驱逐的节点归驱逐，与放置方式无关。手动放置的单元不做这件事，PD 钉住的卡被其他部署占着时调度原因是 `pinned_gpus_unavailable`（`params`：`node`、`gpus`、`holders`）。

`PUT /deployments/{id}/placement-mode` 在自动与手动之间切换；已有部署都是手动。

### 评估请求与获取预估

部署请求不选择镜像或模型从哪里来（[ADR 0048](../adr/0048-pull-settings.md)）：`model_source` 只有 `{storage_id, provider}`，前者是节点取模型时保存到的存储，后者是运行模板的哪家权重来源（`huggingface` / `modelscope`）。`acquisition.nodes[]` 对请求指定的节点（手动放置或多机，按 rank 顺序）给出预估，只是预估，节点启动时会重新判断：

| 字段 | 语义 |
|---|---|
| `image_plan` | `present` 节点已有；`copy` 同步组内的 `image_copy_from` 节点复制；`pull` 从 `image_pull_refs` 拉取：前 `image_pull_fixed` 个是来源规则的替代地址，按顺序先试，其余是镜像自己的副本（`image_pull_ranked` 为真时由节点按探测延迟决定先后）；规则让该节点无处可拉时带 `image_pull_error`（`{code, params, message}`，code 一般为 `IMAGE_SOURCE_FORBIDDEN`，params 含 `node`、`route`） |
| `weights_plan` | `present` 已有；`copy` 从 `weights_copy_from` 节点复制；`storage` 从共享存储 `weights_storage` 读取或缓存；`download` 从 `weights_provider` 下载 |
| `image_cached`、`image_cache_reason`、`weights_present` | 镜像是否已缓存（未知时带原因）与模型是否已在节点上 |

单机自动放置在调度器选出节点之前不逐节点预估，调度后由选中的节点自己获取。多机与 PD 的自动放置在评估里先按此刻挑中的节点处理，与这些节点是手动写出的一样。

`acquisition.components[]` 对模板里主模型以外的每个组件（单独发布的文本编码器、投机解码草稿模型等）给出目标节点的获取预估，创建部署按同一份规划行事（[ADR 0062](../adr/0062-template-components-follow-the-primary-storage.md)）：

| 字段 | 语义 |
|---|---|
| `name`、`role`、`model`、`size_bytes` | 组件名、角色、模型（仓库 ID）与模板声明的大小；[按 rank 拆分](inference-catalog-spec.md#按-rank-拆分的权重)的组件每个 rank 一项，`model` 带该 rank 的子目录（如 `org/repo/rank1`），`size_bytes` 是一个 rank 的大小，`nodes` 只计该 rank 的节点 |
| `plan` | `ready` 目标节点都已有；`shared_download` 创建时由一个节点下载一份到共享存储 `storage`（`storage_id`），目标节点读这一份，`follows_primary` 为真表示请求没选「保存到」、这是主模型所在的共享存储；`node_download` 缺它的节点各自下载到自己的存储 |
| `nodes`、`ready_nodes` | 目标节点数与已经读得到它的节点数 |

创建部署真正发起组件写入时，时间线记 `DEPLOYMENT_COMPONENT_DOWNLOAD`，含 `component`、`model`、`bytes`、`storage`、`node`（写入节点）、`follows_primary`。

硬件能力不满足时，装配返回 `ACCELERATOR_FEATURE_UNSUPPORTED`（`vendor`、`feature`）；AMD 最终放置未分配设备时返回 `ACCELERATOR_DEVICE_SELECTION_REQUIRED`。自动放置在选卡尚未完成时不触发后者。候选节点的硬件或运行时限制以结构化拒绝返回；混合厂商节点按模板匹配后的候选卡判断。

### 评估的字段错误（`errors[]`）与 `ok=false` 语义

`POST /evaluate` 是交互端点：
可归因到请求字段的校验失败返回 HTTP 200、`ok=false`，错误进入响应的
`errors[]`（每项 `{field, code, params?}`，`code` 是稳定 ProductError 码、与 create
对应 400 路径的错误码一一对应，`params` 为客户端 locale 插值参数）。表单保留草稿并把
错误显示在对应控件旁；`POST /api/v1/deployments`（create）保持 400/409 不变。无法归因到字段的
失败（鉴权、模板解析失败、服务端错误）仍按原状态码。

请求层的独立校验会合并报告；请求层有字段错误时不继续启动试算，响应仍提供已经确定的模板、可选节点，以及能够解析的健康检查信息。模型 ID 在模型解析阶段找不到时，归到 `model_id` 字段；尚未解析到模型时，健康检查能力可以缺省。自动多机放置失败会先返回 `placement_mode` 错误，不继续后续校验。字段路径约定：

- 快捷参数直接使用字典 key（即表单字段名），如 `max_model_len`、`gpu_memory_utilization`；
- 请求级字段使用请求 JSON 中的名称：`replicas`、`nnodes`、`engine`、`target_vendor`、`extra_args`、
  `quick_params_unset`、`master_port`、`placement`、`placement.gpus`、`placement.tp`、`placement.pp`、`node_requirements`、
  `placement_strategy`、`partition_backend`、`start_timeout_sec`、`health_check_threshold_sec`；
- `node_uuids`（多机节点列表）在表单里对应 `distributed_nodes` 控件；自动放置的多机请求在挑不出节点或缺 `placement.tp` 时，错误归到 `placement_mode`；
- 额外挂载行统一归到 `container_mount_rows`（请求只携带用户新增的挂载，不含行号）；
- docker 参数错误归到 `container_docker_args`，共享内存大小错误归到 `container_shm_gib`；
- **`field` 为空串的错误不归属任何控件**：启动装配/放置试算等与单个参数无关的失败
  （如没有可用运行时镜像）以此承载，客户端应将其合并进页面级
  提示，不得挂在参数控件旁。

涉及的规则：副本形状（`REPLICAS_NEGATIVE`/`REPLICAS_TOO_MANY`/`DISTRIBUTED_REPLICA_COUNT`）、
节点条件（`NODE_REQUIREMENTS_INVALID`/`NODE_REQUIREMENTS_UNMET`）、放置策略（`PLACEMENT_STRATEGY_INVALID`）、
指定的卡（`PLACEMENT_GPU_NOT_ON_NODE`：`placement.gpus` 写了所选节点没有的序号，`params.node`/`gpu_index`/`node_gpus`）、
厂商（`TARGET_VENDOR_INVALID`）、引擎（`ENGINE_INVALID`）、
extra_args 解析（`EXTRA_ARGS_INVALID`）、挂载（`MOUNT_CONTAINER_PATH`/`MOUNT_HOST_PATH`/
`MOUNT_TARGET_DUPLICATE`/`MOUNT_TARGET_NESTED`/`MOUNT_VOLUME_INVALID`）、json 型参数（`QUICK_PARAM_JSON_INVALID`）、
容器路径型参数（`QUICK_PARAM_PATH_INVALID`）、docker 参数与平台托管项或正式字段重复
（`DOCKER_ARG_PLATFORM_MANAGED`/`DOCKER_ARG_USE_SHM_FIELD`/`DOCKER_ARG_USE_ENV_FIELD`/
`DOCKER_ARG_USE_MOUNT_FIELD`/`DOCKER_ARG_USE_ENTRYPOINT_FIELD`）、共享内存大小与 `--ipc=host` 同时设置
（`SHM_SIZE_WITH_HOST_IPC`）、共享内存大小小于 CPU 内存层（`SHM_BELOW_KV_CPU_TIER`）、参数冲突（`LAUNCH_CONFIG_CONFLICT`/
`LAUNCH_CONFIG_UNSET_CONFLICT`）与数值界
（`FIELD_INVALID`，`detail` 为英文兜底文本）。响应另带契约版本 `schema_version`（当前 1）。

除 `errors[]` 外，评估响应还按需携带以下字段：

| 字段 | 语义 |
|---|---|
| `form_values` | 表单应显示的完整值：生效配置（含字典默认与模板投影）及已将可识别参数转入结构化字段后的 `extra_args` 文本 |
| `defaults` | 表单默认值：`model_mount`（`/model`）、`master_port`（29500）、`engine`（默认引擎） |
| `eligible_nodes` | 每个节点按本草稿的节点条件（含厂商；手动选的那台节点本身不算条件，下拉正是用来选它的）/模板 GPU 谓词/在线状态判定，`eligible=false` 时 `reasons[]` 给原因（`node_offline`、`node_requirements_unmet`、`vendor_mismatch`、`chip_mismatch`；节点条件本身无效时每个节点都是 `node_requirements_invalid`，不会全部显示为合格）；没有显存限制的单机草稿里，空载也放不下一个副本的节点同样 `eligible=false`：卡数或单卡显存放不下时原因是调度器的 `no_fit`（带需要与可用的显存），所给的卡上启动命令解析不出来时是 `placement_rejected`（`params.detail` 为创建选它时返回的错误文本），模型比该节点上存储的缓存上限还大时是 `model_exceeds_cache_limit`。这个判定与放置模式、草稿当前选了哪台节点无关，只看节点本身，不看当前占用（卡被占、离线、磁盘满都不会因此标为不合格）。同一硬件规格且适用存储与缓存上限相同的节点合并判定一次，每次评估最多判定 16 种，超出的种类不判定、保持合格；某一种判定出错时该种节点同样保持合格。带显存限制的草稿、多机与 PD 草稿不做这项判定，所以 `eligible=true` 不保证创建一定成功；节点下拉只渲染此列表 |
| `gpu_candidates` | 仅手动单机评估返回 `{node_uuid, items}`；每张卡包含 `index`、`available`，不可用时含结构化 `reason`。表示当前资源可用性，不表示单卡能承载模型，也不禁止选择资源不可用的卡；UI 仍按节点卡片的 `schedulable` 控制新增选择。字段缺失表示未评估，模型/多卡/拓扑可行性继续由 `plan` 判断。 |
| `weight_sources` | 模板 spec 声明的权重来源（`provider`/`repo_id`/`revision`/`subfolder`）+ `suggested`（服务端建议）与 `registered`（本集群已有登记行，按 repo id 身份判定）；选定后仍由客户端调注册 API |
| `preflight` | 仅 `nnodes>1`：请求级预检 `{ok, checks}`。`checks` 列出查过的每一项（通过的也列出）：`id` 为 `node_count`、`node_list`、`port_range`、`node_online`（每个节点一项，`params.node` 为节点）或 `shared_network`；`status` 只取 `pass`/`warn`/`fail`/`skip`/`na`；`code` 与 `params` 给出结果，`message` 为英文 fallback；因前面的检查失败而未执行的项为 `skip`、`code` 为 `PREFLIGHT_BLOCKED`，`blocked_by` 给出挡住它的检查 id；`evidence` 只在没有码能说明原因时带原始错误。没有 `fail` 时 `ok` 为 true。深度网络探测仍在独立预检 RPC（CLI 保留），前端不再编排 |
| `diagnostics` | 模型获取的检查说明 `{checks, truncated?}`（创建被拒时也在错误体里）：每项 `id`、`status`（只取 `pass`/`warn`/`fail`/`skip`/`na`；`warn` 为副本或传输仍在进行、启动时要填缓存；`skip` 为没查或没观测到，被前面的检查挡住时带 `blocked_by`；`na` 为不适用）、`message`（`{code, params, message}`，`params.reason` 为具体原因，`params.observer` 为证据来源），最多 128 项、64 KiB，超出时 `truncated` 为 true |
| `eviction` | 当前显存不足但可通过休眠醒着的部署腾出空间时返回候选；每项含 `instance_uuid`、`deployment_id`、`deployment`、`gpu_indexes` 和 `frees_gib` |

服务端按当前引擎字典填充参数默认值（含引擎专用默认），依赖未启用的参数不填默认。
`/engines` 的 `layout`（`quick`、`expert`、`hidden`，空串按 `expert`）与 `order` 供客户端安排参数表单。

`quick_params_unset` 只能包含当前引擎字典里的 key。若同一请求又在
`quick_params` 或 `extra_args` 为该 key 提供值，返回清除冲突；原有 `null`
语义保持不变，不能用 `null` 替代明确清除。复合引擎参数只有在适配器能无损识别、
且完整关闭对应配置时才能从模板移除；未知 connector 或混合配置返回冲突，避免误删其他能力。

自动模式评估返回 `selected_node`、逐节点 `candidates` 与选中节点的 `plan`。放不下时 `plan` 和各候选的 `reason` 说的与控制器一致：占卡者让卡后就能放下时是 `waiting_for_yield`，只因休眠者留着卡放不下时是 `reserved_for_sleepers`（按草稿的出让策略算），不再一律是放不下。占卡的部署都睡下就能放下、但出让策略让草稿等不到它们时，评估另外说明：草稿不自动出让时是 `does_not_wait_for_yield`，有占卡者不自动出让时是 `held_by_non_yielding`，参数 `node`、`gpus`、`holders`、`blockers`（见[新部署怎样放到共用的卡上](../how-to/scheduling-center.md#新部署怎样放到共用的卡上)）；这两个原因只出现在评估里。

评估响应另有 `yield_policy_eligible`（`{ok, reason?}`，形状同部署详情的同名字段，`reason` 为 `SLEEP_NOT_ENABLED`、`SLEEP_ENGINE_UNSUPPORTED` 或 `YIELD_POLICY_SHAPE`），按草稿的可休眠、引擎、节点数与是否 PD 算出，就是创建时会做的那个检查，部署表单据此决定能不能选出让策略；草稿的引擎还未确定时不出现。创建部署时表单把选定的出让策略原样发来，不满足时由服务端拒绝（`SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` / `YIELD_POLICY_SHAPE`）。评估响应另有 `health_checkable`（布尔）：按这份草稿创建出的部署能否开健康检查，规则与创建时相同，并且按部署将得到的接口判断——模板草稿里，模型的接口还是空时取模板声明的接口；部署表单据此决定健康检查开关能否打开，模型还未解析出来时不出现。评估响应另有 `priority_scheduling`（`{ok, refusal?, fix_args?}`）：按引擎最终收到的参数判断这份草稿能否开启请求优先级调度，规则与创建时相同；`refusal` 是 `{code, params}`，`code` 为上表 `priority_scheduling` 列出的拒绝原因；冲突来自推理模板的参数时，`fix_args` 给出加进自定义参数即可消除冲突的参数（如 `["--schedule-policy","fcfs"]`）。草稿的引擎还未确定时不出现。
创建时提交 `selected_node.uuid` 作为 `node_uuid`，同时提交 `plan.plan.gpus`
和 `plan.plan.tp`，并保留标签等原约束。创建会重新校验，原节点、GPU 或约束失效时
返回冲突，要求重新评估。这套「评估后提交所选节点」的确认只适用于单节点、单副本；多机与 PD 的自动放置
不走它（见[自动放置多机与 PD 部署](#自动放置多机与-pd-部署)）。显式节点与旧请求的多副本
评估仍为 `plan_scope=per_replica`，不能据此推断整个副本组都能分配成功。

模板详情及评估响应中的 `engine_decides_param_keys` 是当前有效启动不替用户填默认值、
交给引擎决定的字典 key：启动入口或镜像里的引擎版本可能不认这个 flag（规则见
[启动配置](../explanation/launch-config.md)）。只列有字典默认值的 key；表单把它们显示为
「自动」，用户仍可显式打开或关闭，显式陈述的值照常发射，不会被拒绝。判断由引擎适配器做，
不能通过模板有没有写某个 flag 推断。新部署的用户入口覆盖优先于模板入口；手工命令不解释为
结构化引擎参数。

**按所选引擎的字典提交参数。** 请求在
`quick_params` 里显式给某个 key 值，而所选引擎的字典没有这个 key 的 flag（或该 key 是
只对其它引擎生效的平台参数）时，该值会被拒绝（`invalid quick_params: <key> is not supported by <engine>`）。
评估返回 HTTP 200、`ok=false` 与字段错误；创建部署及克隆的配置差异计算返回 400。唯一例外是该 key 的**中性默认值**——它与"没写"等价，例如 SGLang 的
`performance_mode=balanced`。

不支持的显式组合包括：

- SGLang：`enable_chunked_prefill`（两种极性）、`rope_scaling`、多模态 json 键
  （`limit_mm_per_prompt`、`mm_processor_kwargs`、`media_io_kwargs`、`allowed_local_media_path`）、
  `shared_kv_dir`、`kv_offload_read_threads`/`kv_offload_write_threads`、`kv_disk_limit_gib`、
  `async_scheduling=true`，以及 `performance_advanced` 组里所有 vLLM 专属键。
- vLLM：`log_level`（除 `info`，该引擎的日志级别由 env 承担）、`enable_metrics=true`、
  `speculative_num_steps`、`speculative_eagle_top_k`。
- callable：不发布 `param_specs`，除中性值外不接受任何字典参数。

脚本或第三方客户端不能直接把 vLLM 的参数复用到 SGLang。`engine_decides_param_keys`
（按入口与引擎版本只撤默认值）与这条规则互补：前者不拒绝任何值，后者按**引擎**拒绝它表达不了的值。

### PD 分离部署（`placement.pd`）

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

- `mode`：引擎档位 `pd_modes` 中的一个（`vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake`、`vllm-ppu-mooncake`、`sglang-nixl`、`sglang-mooncake`、`sglang-ppu-ipc`、`sglang-ppu-mooncake`）；未知字段一律拒绝。
- 每个 `unit` 是一个副本；`gpus` 可省略。`followers` 使该副本跨多台节点（rank 1…N），此时角色必须写 `tp`，
  每个 rank 运行 `tp × pp ÷ rank 数` 张卡。
- `fallback` 与 `kv_load_failure` 的可选值和默认值按模式定：`vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake` 的 `fallback`
  为 `decode`（默认）或 `fail`，`kv_load_failure` 为 `fail`（默认）或 `recompute`；`sglang-nixl`、`sglang-mooncake`、`sglang-ppu-ipc`、`sglang-ppu-mooncake`、
  `vllm-ppu-mooncake` 的 `fallback` 只有 `fail`，也没有 `kv_load_failure`。模式不允许的值返回 `PD_FALLBACK_INVALID` /
  `PD_KV_LOAD_FAILURE_INVALID`。
- `GET /api/v1/engines` 每个引擎的 `pd_modes[]` 给出这些规则：`name`、`vendors`、`roles`、`scalable_to_zero`（运行中可缩到 0
  的角色）、`tp_rule`、`max_units_per_role`、`fallbacks`、`kv_load_failures`（首项为默认值）、`unverified`。
- 与 `nnodes`、`node_uuids`、`replicas > 1` 同时给出时返回 `PD_SHAPE_CONFLICT`；自动放置用 `placement_mode: "auto"` 且单元不写 `node_uuid`（见[自动放置多机与 PD 部署](#自动放置多机与-pd-部署)）；其余校验错误码以
  `PD_` 开头（字段 `placement.pd`）。可休眠（`sleepable`）且 KV 经 UCX 走主机 RDMA 网络时，创建与评估返回
  `PD_SLEEP_RDMA_UNSUPPORTED`（`params`：`mode`、`network`）。
  **手动指定**的放置里有节点的内核会让 UCX 卡住（见上文节点的 `ucx_netlink_hang`）而模式经 UCX 传 KV 时（自动放置直接跳过这些节点）：都不拒绝，创建照常，评估给警告：参数网络是以太网（含管理网）为
  `PD_UCX_NETLINK_HANG`（引擎很可能一直起不来，需先关闭 IPv6），主机 RDMA 网络上为较弱的 `PD_UCX_NETLINK_HANG_RDMA`；
  两者的 `params` 相同：`mode`、`network`、`nodes`（`[{node_uuid, node_name, kernel}]`，只含受影响的节点）、`names`（节点名，逗号分隔）、
  `fixes`（`[{code, commands[]}]`，与节点的 `fixes` 相同）、`fix`（第一条修复命令）。
- 部署详情的 `pd` 字段：`mode`、`fallback`、`kv_load_failure`、`state`（`not_ready|partial|running`）、`short`、
  `roles[]`（`role`、`tp`、`pp`、`desired`、`ready`、`may_be_empty`（模式允许该角色为空，即可以单独休眠）、`units[]`，含 `followers[]`）；实例带 `role`。
  每个 unit 另有 `failed_attempts`、`last_failure` 与 `retry_exhausted`（控制器已放弃重建，可用上面的 retry 接口重试）。
- `POST /api/v1/evaluate` 的响应另有 `pd.roles[].units[]`：每个副本的 leader（及其 `followers[]`）的放置、地址、显存拆分与命令。

## Benchmarks（评测）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/benchmarks` | 评测任务列表 | JWT |
| GET | `/api/v1/benchmarks/meta` | 评测字典（数据集/指标元信息） | JWT |
| POST | `/api/v1/benchmarks` | 创建评测任务；可选 `priority_class`：每个请求向网关请求的请求优先级类（不区分大小写），只有从 server 发压测托管部署时可以指定，否则 400 `BENCHMARK_PRIORITY_CLASS_GATEWAY`，类不存在 400 `PRIORITY_CLASS_NOT_FOUND`，非管理员指定 403；任务对象原样返回它（未指定时省略），逐请求明细的 `priority_class` 是网关实际使用的类 | JWT |
| POST | `/api/v1/benchmarks/probe` | 探测外部评测目标（连通性/鉴权） | JWT |
| POST | `/api/v1/benchmarks/models` | 列出可评测模型 | JWT |
| POST | `/api/v1/benchmarks/datasets/import` | 导入评测数据集 | JWT |
| POST | `/api/v1/benchmarks/tokenizers/import` | 导入评测 tokenizer | JWT |
| GET | `/api/v1/benchmarks/{id}` | 评测任务详情 | JWT |
| GET | `/api/v1/benchmarks/{id}/curve` | 评测负载曲线数据 | JWT |
| GET | `/api/v1/benchmarks/{id}/report` | 下载评测报告 | JWT |
| DELETE | `/api/v1/benchmarks/{id}` | 删除评测任务 | JWT |
| GET | `/api/v1/benchmarks/{id}/points/{seq}/detail` | 下载某负载点的明细数据 | JWT |
| GET | `/api/v1/benchmarks/{id}/points/{seq}/timeline` | 读取点位请求时间线（有界、脱敏的计时与请求身份） | JWT |
| POST | `/api/v1/benchmarks/{id}/cancel` | 取消运行中的评测 | JWT |

## Profiles / GPUs（估算校准与 GPU 清单）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/gpus` | 集群 GPU 清单及最新指标，至多 20 秒的快照；实例、部署或节点（卡健康、停用卡、上下线）变化后，过 2 秒刷新下限即重建。支持 `page`/`page_size`、`q`、`node`、`vendor`、`model`；`allocation_state` 为 `available/partial/allocated/unusable/unknown`，与健康等级不同。`q` 匹配节点名/UUID、卡型号、GPU UUID、PCIe 地址子串，其余筛选可用逗号分隔。`unusable` 原因见 `schedulable/health_status/ownership/foreign_processes`。`sort` 为 `node/model/vendor/allocation_state/occupants/utilization/memory/temperature/power/fan/bdf/numa`；分页含全量 `overview`、`facets`。见[GPU 筛选与分配状态](#gpu-筛选与分配状态) | JWT |
| GET | `/api/v1/dgpus` | dGPU 实例清单 | JWT |

### GPU 筛选与分配状态

不带分页参数返回全部；带 `page` 或 `page_size` 返回 `{items,page,page_size,total,overview,facets}`。
`overview` 与 `facets{nodes[{uuid,name}],models,vendors}` 覆盖全部 GPU，不受筛选影响。

- `q`：节点名/UUID、卡型号、GPU UUID、PCIe 地址的子串。
- `node`（节点 UUID）、`vendor`、`model`、`allocation_state`：多个值以逗号分隔。
- `sort`：`node`、`model`、`vendor`、`allocation_state`、`occupants`、`utilization`、`memory`、`temperature`、`power`、`fan`、`bdf`、`numa`；无读数的排在前。

`allocation_state` 表示卡的分配状态，不能当作健康等级：

- `available`、`partial`、`allocated`：按占用程度区分。
- `unusable`：完全不接任务，原因见卡的 `schedulable`、`health_status`、`ownership`、`foreign_processes`。
- `unknown`：节点未在线，读数是上次上报值；`available_bytes` 为 0，不计入可分配数量，已有分配与硬件健康信息仍保留。

显存按台账分为 `allocated_bytes`（运行中实例占用）、`sleeping_bytes`（休眠实例的残留）、`foreign_bytes`（计入的平台外进程占用，用量未知时为 0）与 `available_bytes`（还能分配）。四者之和小于 `memory_total_bytes` 时，差额是因其它原因扣留的显存：节点在线的卡上是为平台外进程预留的余量（部署策略的 `foreign_headroom_pct` 与 `foreign_headroom_min_bytes`，都为 0 时没有差额），平台外进程用量未知或节点未在线时是整卡。

`overview.health` 按 `ok`、`warning`、`error` 统计健康等级。告警卡仍可能服务；被直通或被平台外进程占用的卡也可能健康为 `ok`。
状态随 20 秒快照刷新。

## API Keys（网关密钥）

新建的推理令牌属于当前登录用户，所属用户不由请求体指定。普通用户只能列出、修改调用范围或吊销自己的令牌；管理员保留全部令牌的管理权限。列表中的 `user_id` 是记录的所属用户，`0` 明确表示未记录用户归属的历史令牌，不会自动归到管理员名下。

历史令牌可继续按原调用范围推理，并只能读取由原令牌创建的历史零用户批处理资源。需要个人令牌时，使用现有创建操作重新发放，再由管理员显式吊销旧令牌；旧批次不会随重发自动转移，管理员仍可在控制台管理它们。部署调用范围只限制推理执行；同用户批处理历史的可见性遵循[批处理的用户归属](../how-to/batch-api.md#范围与限制)。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/apikeys` | API key 列表（管理员可见全部，普通用户仅自己拥有的 key），每项含只读数字 `id`，供用量筛选关联；不含明文、secret 或 hash，含已吊销的 key：已吊销的带 `revoked_at`（UTC 时间），未吊销的不出现该字段。`all_deployments` 为 true 时可调用全部模型服务；为 false 时只能调用 `deployments`（`[{id,name,served_name}]`）中的部署，这些部署全被删除后列表为空、key 什么都调用不了。`priority` 是 key 的请求优先级绑定 `{class, ceiling, policy}`：`class` 为空表示使用默认优先级，`ceiling` 为空表示上限等于自身优先级，`policy` 为 `within_ceiling`（默认）、`fixed` 或 `down_only`，见[请求优先级](../explanation/request-priority.md) | JWT |
| POST | `/api/v1/apikeys` | 创建 API key；`name` 去除首尾空白后不能为空，否则返回 400 `API_KEY_NAME_REQUIRED`；明文仅创建响应中返回一次。可选 `all_deployments` 与 `deployment_ids` 限定可调用的部署：省略 `all_deployments` 时，`deployment_ids` 为空即全部部署、非空即仅这些部署；`all_deployments` 与 `deployment_ids` 矛盾（true 且非空，或 false 且为空）返回 400 `API_KEY_SCOPE_INVALID`，不存在的部署返回 400 `API_KEY_DEPLOYMENT_UNKNOWN`（`params.ids`）。可选 `priority`（形状同列表项）同时绑定请求优先级，只有管理员可以带，其他用户带上返回 403；优先级不存在返回 400 `PRIORITY_CLASS_INVALID`，策略未知返回 400 `PRIORITY_POLICY_INVALID`，此时不创建 key | JWT |
| PUT | `/api/v1/apikeys/{prefix}/scope` | 修改未吊销 key 的可调用范围，`{"all_deployments":bool,"deployment_ids":[...]}`，`all_deployments` 必填，校验同创建；返回更新后的 key（同列表项）。下一次请求起生效；未知或已吊销的前缀返回 404 `API_KEY_NOT_FOUND` | JWT |
| PUT | `/api/v1/apikeys/{prefix}/priority` | 绑定未吊销 key 的请求优先级，`{"class":"gold","ceiling":"","policy":"fixed"}`（形状同列表项的 `priority`）；仅管理员，其他用户 403。`ceiling_policies` 以外的策略会去掉 `ceiling`；上限低于 key 自身的优先级（未绑定时为默认优先级）400 `PRIORITY_CLASS_INVALID`。返回更新后的 key。下一次请求起生效；优先级不存在 400 `PRIORITY_CLASS_INVALID`，策略未知 400 `PRIORITY_POLICY_INVALID`，未知或已吊销的前缀 404 `API_KEY_NOT_FOUND` | JWT（管理员） |
| DELETE | `/api/v1/apikeys/{prefix}` | 按列表中的 12 字符 `prefix`（完整 key 的前 12 个字符，不是完整密钥）吊销 API key：吊销完成后的新请求不能再以该 key 认证，记录保留（名字留给它的历史用量），前缀不复用；成功返回 `{"status":"revoked"}`。未知前缀返回 404 `API_KEY_NOT_FOUND`；重复吊销已吊销的前缀仍返回 200，不改动原 `revoked_at`。所有 key 都吊销后网关**不**回到匿名访问：`/v1` 仍要求 key，会拒绝所有请求，直到再创建一个 key，或管理员打开安全设置里的 `inference_open_access`（见[OpenAI 兼容网关的认证](openai-gateway.md#认证)） | JWT |

## Priority Classes（请求优先级）

优先级类决定网关给请求的优先级，见[请求优先级](../explanation/request-priority.md)。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/priority-classes` | 优先级列表，按数值从大到小：`items[]`（`name`、`display_name`、`description`、`value`、`builtin`、`default`、`deletable`（能否删除：非内置、非默认且没有未吊销 key 绑定）、`created_at`），以及 `min_value`/`max_value`（数值范围，-100 到 100）、`policies`（key 可用的策略，界面按此展示）、`default_policy`、`ceiling_policies`（有上限的策略） | JWT |
| POST | `/api/v1/priority-classes` | 新建优先级 `{"name":"gold","display_name":"Gold","description":"","value":30}`：`name` 为小写字母、数字和连字符并以字母开头、最多 32 个字符，`value` 必填且在范围内，否则 400 `PRIORITY_CLASS_INVALID`（`params.field`）；同名 409 `PRIORITY_CLASS_EXISTS`。返回 201 与新的优先级 | JWT（管理员） |
| PUT | `/api/v1/priority-classes/{name}` | 修改 `display_name`、`description`、`value`（省略的不变）；内置优先级改数值 409 `PRIORITY_CLASS_BUILTIN`，不存在 404 `PRIORITY_CLASS_NOT_FOUND` | JWT（管理员） |
| DELETE | `/api/v1/priority-classes/{name}` | 删除优先级，成功 204；内置优先级 409 `PRIORITY_CLASS_BUILTIN`，默认优先级、仍有未吊销 key 绑定（作为优先级或上限）或有未结束的批处理、评测使用 409 `PRIORITY_CLASS_IN_USE`，不存在 404 `PRIORITY_CLASS_NOT_FOUND` | JWT（管理员） |
| PUT | `/api/v1/priority-classes/{name}/default` | 设为默认优先级（没有绑定优先级的 key、试验场、无需 key 的请求使用它），返回整个列表；不存在 404 `PRIORITY_CLASS_NOT_FOUND` | JWT（管理员） |

## Platform Tokens（平台 API 令牌）

平台 API 令牌代替登录会话调用管理 API 与 MCP，与上面的网关密钥（只用于 `/v1`）是两种东西。明文 `gwp_` 开头，只在创建响应里出现一次，服务端只存 SHA-256。每个用户只看得到、管得了自己的令牌，且只能在登录会话里操作。

名称去除首尾空白后必须为 1–128 个 UTF-8 字节（不是字符数），否则返回 400 `PLATFORM_TOKEN_NAME_INVALID`。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/platform-tokens` | 当前用户的令牌：`{items:[{id,name,prefix,scope,created_at,expires_at,last_used_at}], total}`；`last_used_at` 每分钟最多更新一次 | JWT（登录会话） |
| POST | `/api/v1/platform-tokens` | 创建令牌：`{name, scope?: read\|write（默认 read）, expires_in_days?: 0–3650（0 不过期）}`，返回 201 `{token, item}`；`scope` 非法回 400 `PLATFORM_TOKEN_SCOPE_INVALID` | JWT（登录会话） |
| DELETE | `/api/v1/platform-tokens/{id}` | 吊销（删除）自己的令牌，立即生效，返回 204；不存在或属于别人回 404 | JWT（登录会话） |

## Plugins（插件）

已安装的签名插件包（`.gwpkg`）的管理接口，见[扩展点](../dev/extensions.md#管理接口)。这些是核心路由：不受功能门控，插件全部掉线或被卸载时照常可用；AI 助手与 MCP 读不到；每次调用都记一条审计日志。server 没有运行插件宿主时，每条路由回 503 `PLUGINS_UNAVAILABLE`。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/plugins` | 已安装的包：`{items:[{id, version, publisher, trust, enabled, state, message?, since?, restarts, parts, isolation, roles, permissions, approve_hash, previous?, on_demand, pending?, features, actions}], check}`，按 `id` 排序；`check` 是诊断检查 `plugins`：`{id, status, message:{code, params, message}}`，什么都没装时缺省；`permissions` 是 `[{name, risk}]`，按 `name` 排序，`risk` 为 server 给出的 `low`、`elevated` 或 `high`（[权限](plugin-protocol.md#权限)） | `JWT(admin)` |
| POST | `/api/v1/plugins/install?approve=<批准值>&enable=<bool>` | 请求体是 `.gwpkg` 本身（`application/octet-stream`）。没带或带错 `approve` 时回 409 `PLUGIN_APPROVAL_REQUIRED`，`params` 给出 `id`、`version`、`approve_hash` 以及它涵盖的 `permissions`、`roles`（逗号分隔），另有它与当前已装版本相比的差异（首次安装时相对于空）：`from_version`，`permissions_added`/`permissions_removed`（`权限=风险等级`），`roles_*`、`capabilities_*`（`名称@版本`）、`features_*`、`routes_*`（`方法 路径 访问级别`）、`host_setup_*`（`类型 vendor_id:product_id`）、`devices_*` 各一对 `_added`/`_removed`，以及 `ui_mode`、`isolation`（变了才是 `旧->新`），每个键都在、没有内容时为空串；什么都不装；带对了返回 `{plugin, previous?, unchanged}`。`enable` 缺省为 `false`；装在已启用的版本之上时保持启用 | 管理会话，admin |
| POST | `/api/v1/plugins/{id}/enable` | 启用并启动服务端部分，返回该包；不再通过校验的包回 409 `PLUGIN_BROKEN` | 管理会话，admin |
| POST | `/api/v1/plugins/{id}/disable` | 停止并不再启动，返回该包；它推送的表照常执行，并按掉线计算宽限 | 管理会话，admin |
| POST | `/api/v1/plugins/{id}/restart` | 停止服务端部分并作为新实例重新启动，崩溃历史与退避从头算；按需启动的包回到 `idle`，等下一次调用。返回该包；已停用的包回 409 `PLUGIN_DISABLED{id}`，不再通过校验或被吊销的包回 409 `PLUGIN_BROKEN`，没有服务端部分的包回 422 `PLUGIN_UNAVAILABLE` | 管理会话，admin |
| POST | `/api/v1/plugins/{id}/upgrade?approve=<批准值>` | 请求体是新版本的 `.gwpkg`。没带或带错 `approve` 时回 409 `PLUGIN_APPROVAL_REQUIRED`，`params` 与安装相同，另带与已安装版本的差异 `from_version`、`permissions_added`/`_removed`（`权限=风险`）以及角色、能力、功能、路由、主机步骤、设备各一对 `_added`/`_removed` 和 `ui_mode`、`isolation`（变了时为 `旧->新`），什么都不变；批准值未变且站点设了 `plugins.policy.auto_approve_unchanged` 时不需要 `approve`。批准后停止旧版本、快照数据、启动新版本，新版本在线满 60 秒后返回 `{plugin, previous, auto_approved, unchanged}`；没通过时自动回滚，回 422 `PLUGIN_UPGRADE_ROLLED_BACK{id, version, previous, cause_code}`（`cause_code` 是失败的码，被停用、卸载或 server 停止提前结束时为 `PLUGIN_UPGRADE_CANCELLED`）。同步返回，最长约 80 秒 | 管理会话，admin |
| POST | `/api/v1/plugins/{id}/rollback?restore_state=<bool>` | 回到上一版本（列表的 `previous`）并在启用时启动它，返回该包。`restore_state=true` 同时从该版本的快照恢复数据与数据标记 | 管理会话，admin |
| DELETE | `/api/v1/plugins/{id}?purge=<bool>&confirm=<影响摘要>` | 停止并卸载，删掉它推送的全部内容并重新应用保持，返回 204；`purge=true` 连数据目录（含快照）一起移到 `.trash/`，并删掉数据标记。`confirm` 必须是此刻卸载影响（`GET …/impact`）的 `confirm`：没带、带错，或预览之后影响变了（插件又推送了、换了版本），都回 409 `PLUGIN_UNINSTALL_CONFIRM_REQUIRED`，`params` 为 `{id, confirm, held_nodes, users_affected, caps_lifted, features_locked, banners, overrides}`（除 `id`、`confirm` 外都是计数），应答体另带 `impact`（与 `GET …/impact` 相同），什么都不删。不带 purge 卸载后包目录与数据仍在，此时再带 `purge=true` 卸载即清除它们 | 管理会话，admin |
| GET | `/api/v1/plugins/{id}/impact` | 此刻卸载它会改变什么，只读内存、什么都不改：`{id, version?, confirm, tables:[{kind, entries}], nodes, features, caps, banners, overrides, users_affected}`。`tables` 是要删的表；`nodes` 是释放保持的节点 id（只有它保持的节点，以及没有上限剩下时为容量保持的节点；另一个上限仍在但放宽时，它会释放的节点不计入，`caps` 列出该上限）；`features` 是将变为 `locked` 的功能键；`caps` 是被取消或放宽的集群上限（`max_nodes_with_cards`、`max_cards`）；`banners` 是它此刻显示给管理员的横幅数；`overrides` 是随它删除的覆盖 id；`users_affected` 是它推送的角色或映射涉及的用户数（schema 1 没有这类表，为 0）；`confirm` 是这一切加上各表代数与摘要、包版本的摘要。包已卸载但推送行还在（卸载中途失败），或卸载时保留了数据目录，同样回答 | `JWT(admin)` |
| GET | `/api/v1/plugins/{id}/state` | 推送状态里它的部分，读内存：`{id, installed, verified, now, down_since?, grace_s, clamp_at?, tables:[{kind, schema, generation, digest, entries, pushed_at, next_step?}], overrides}`。时刻都按插件时钟（extclock）；`down_since` 与 `clamp_at`（授予在此刻收窄）只在掉线时出现；`next_step` 是该表某条时间线下一步的时刻；`overrides` 同覆盖列表。包已卸载但行还在时 `installed` 为 `false` | `JWT(admin)` |
| GET | `/api/v1/plugins/{id}/logs` | SSE 流：先发保留的全部内容（最近 256 条监管事件，每条一个 `state` 帧 `{seq, at, state, message?, truncated?}`；最近 1 MiB 的 stderr，一个 `stderr` 帧 `{offset, text, truncated?}`），之后每有新事件或输出就再发。`truncated` 表示这之前有内容已被丢弃。登录失效或失去管理员角色时流结束；助手读不到 | 管理会话，admin |
| GET | `/api/v1/plugins/catalog` | 已导入的包索引与已安装的包合并：`{items:[{id, publisher?, installed?, best_version?, min_core_hint?, installable, reason?, refused:[{version, min_core_hint?, message}]}], errors:[{publisher, message}]}`，按 `id` 排序。`best_version` 是本 server 能装的最新构建；`installable` 表示它还没装上；`refused` 是比它新但本 server 不装的构建（如 `PLUGIN_NEEDS_NEWER_CORE`、`PLUGIN_PACKAGE_REVOKED`、`PLUGIN_BELOW_FLOOR`），由新到旧；没有可装的更新时 `reason` 是其中最新一个的拒绝原因；`errors` 是不再通过校验、被略过的索引 | `JWT(admin)` |
| GET | `/api/v1/metrics/plugins` | 已安装插件的 `gpuworks_plugin_*` 指标，Prometheus 文本格式（`text/plain; version=0.0.4`），供外部 Prometheus 抓取，不写入内置时序库或历史指标目录；指标见[扩展点](../dev/extensions.md#健康横幅诊断检查与指标) | `JWT(admin)` |
| GET | `/api/v1/plugins/overrides` | 生效中的覆盖：`{items:[{id, owner, kind, entry, generation, action, value?, by_user, at, reason}]}`，按 `id` 排序 | `JWT(admin)` |
| POST | `/api/v1/plugins/overrides` | 请求体 `{owner, kind, entry, action, value?, reason}`：放松插件 `owner` 推送的一条限制，返回 201 与覆盖本身。`kind` 为 `node_holds`（`entry` 是十进制节点 id，`action` 只能是 `delete`）或 `fleet_capacity`（`entry` 是 `max_nodes_with_cards` 或 `max_cards`，`action` 为 `delete`，或带调高后上限 `value` 的 `loosen`）；`reason` 必填，最多 1024 字节。字段缺失或格式不对回 400 `FIELD_INVALID`；不满足覆盖规则回 422 `PLUGIN_OVERRIDE_REJECTED`（`params.rule`：`not_restrict`、`no_entry`、`action`、`not_looser`，`reason` 超过 1024 字节为 `reason`，提交前插件又推送改了这一条为 `superseded`） | 管理会话，admin |
| DELETE | `/api/v1/plugins/overrides/{oid}` | 撤回覆盖，所有者最后推送的限制重新生效，返回 204；不存在回 404 `PLUGIN_OVERRIDE_NOT_FOUND` | 管理会话，admin |

字段：

- `trust`：`project`（项目根认证）或 `host`（`server.yaml` 信任的签发者认证）；包不再通过校验时缺省。
- `state`：服务端部分不能运行时是 `disabled`、`installed`（没有服务端部分）、`broken`（不再通过校验）、`revoked`（构建或证书被吊销）或 `needs_host_setup`（已启用，但 root 还没在本机应用它声明的主机步骤，或应用的是别的步骤；`message` 是 `PLUGIN_HOST_SETUP_REQUIRED{id, command}`，`command` 是要执行的命令）；其余是进程状态 `starting`、`running`、`degraded`、`backoff`、`crashloop`、`stopping`、`stopped`、`failed`，按需启动的服务端部分没有运行时是 `idle`。`message` 是原因的码与参数（如 `PLUGIN_CRASHLOOP`、`PLUGIN_HANDSHAKE_REFUSED`、`PLUGIN_FILE_MISMATCH`；没有自己 cgroup 的插件因内存超限被重启时是 `PLUGIN_MEMORY_EXCEEDED`，参数 `id`、`rss_mib`、`limit_mib`），没有可说的就缺省。 插件停止读取请求体被重启时为 `PLUGIN_STALLED{id, stalled_s}`，见[扩展点](../dev/extensions.md#监管)。
- `since`：进入当前进程状态的时间，未受监管时缺省；`restarts`：第一次之后又启动了几个进程。
- `parts`：包含的部分 `server`、`node`、`ui`、`store`；`isolation`：`landlock` 或 `none`。
- `previous`：比当前版本旧的最新已解包版本，留作回滚，没有时缺省；`on_demand`：服务端部分按需启动；`pending`：已导入的索引提供的更新构建 `{version}`，没有时缺省；`features`：包声明的功能键 `[{key, availability}]`，按 `key` 排序，`availability` 为此刻的 `full`、`read_only` 或 `locked`。
- `actions`：服务端算出的当前可做的动作 `enable`、`disable`、`restart`、`uninstall`、`purge`（带 `purge=true` 的卸载）、`upgrade`（每个已安装的包）与 `rollback`（有 `previous` 时）。
- `check`：`message` 是检查结果的码与参数；`status` 为 `fail`（`CHECK_PLUGINS_FAILING`：有已启用的包处于崩溃循环、`failed`、`broken`、被吊销或 `needs_host_setup`）、`warn`（`CHECK_PLUGINS_INCOMPATIBLE`：有已启用的包需要此服务器不提供的能力）或 `pass`（`CHECK_PLUGINS_OK`）；`message.params.ids` 是以逗号分隔的包 id。
- 覆盖的 `generation`：建立覆盖时所有者该种类的代数；`by_user`：建立它的管理员的用户 id；`value`：`loosen` 调高后的上限，`delete` 时缺省。所有者之后的推送改了这一条时，覆盖被取代并删除，管理员看到 24 小时的横幅 `PLUGIN_OVERRIDE_SUPERSEDED`。

调用插件失败时的码（`PLUGIN_UNAVAILABLE`、`PLUGIN_BUSY`、`PLUGIN_TIMEOUT`、`PLUGIN_ROUTE_FAILED`）见[扩展点](../dev/extensions.md#调用与路由表)。

已安装的包声明的路由挂在 `/api/v1/x/<id><pattern>`，鉴权按路由的 `access`（`admin`、`admin_session`、`member`，`public` 不鉴权、按客户端地址限流），见[扩展点](../dev/extensions.md#插件路由)。除上面四个码（分别回 503 带 `Retry-After: 5`、503 带 `Retry-After: 1`、504、502）外：路径不是规范形式 400 `PLUGIN_ROUTE_PATH_INVALID`；插件返回了不予转发的应答（未声明 `html` 的 HTML、不合法的产品错误）502 `PLUGIN_ROUTE_RESPONSE_INVALID{id}`；请求体超过路由上限 413 `REQUEST_TOO_LARGE{max_bytes}`；公开路由超出速率 429 `TOO_MANY_REQUESTS`；没有匹配的路由是核心的 404 或 405。

错误：未安装 404 `PLUGIN_NOT_INSTALLED`；需要批准、包已损坏、同版本不同内容（`PLUGIN_VERSION_CONFLICT`）、声明的功能键已由编译模块或另一个已安装的包提供（`PLUGIN_FEATURE_TAKEN{id, feature, owner}`，在要求批准之前检查）、包 id 与编译进 server 的模块同名（`PLUGIN_ID_TAKEN{id}`，同样在要求批准之前检查）、插件数据比该构建新（`PLUGIN_STATE_TOO_NEW`）、重启已停用的包（`PLUGIN_DISABLED`）、卸载没带对影响摘要（`PLUGIN_UNINSTALL_CONFIRM_REQUIRED`）、升级进行中再安装同一 id（`PLUGIN_UPGRADE_IN_PROGRESS{id}`）、没有可回滚的上一版本（`PLUGIN_NO_PREVIOUS_VERSION{id}`）或 `restore_state` 时没有该版本的快照（`PLUGIN_SNAPSHOT_MISSING{id, version}`）409；升级的包 id 与路径不符 422 `PLUGIN_UPGRADE_ID_MISMATCH{id, package_id}`；升级前无法快照数据 500 `PLUGIN_SNAPSHOT_FAILED{id}`；在 Linux 以外的平台上安装声明了主机步骤的包 422 `PLUGIN_HOST_SETUP_UNSUPPORTED{id}`；包过大 413 `PLUGIN_PACKAGE_TOO_LARGE`；其余被拒的包 422，码见[插件协议与线上格式](plugin-protocol.md)；`enable`、`purge`、`restore_state` 不是布尔值 400 `FIELD_INVALID`。

插件进程调用的宿主接口不在 `/api/v1` 之下，它的错误码（如 `PLUGIN_ROLE_REQUIRED`、`PLUGIN_HOST_NOT_PERMITTED`、`PLUGIN_HOST_ARGUMENT_INVALID`）见[宿主接口的时钟、就绪、自有状态与派生密钥](plugin-protocol.md#宿主接口的时钟就绪自有状态与派生密钥)。

## Providers（云厂商租机）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/providers/kinds` | 支持的云厂商种类，含各自的凭据字段（`credential_fields[].default_value` 为可选的非敏感字段创建默认值，`placeholder` 仅为提示）、默认 `server_dial` 监听地址，以及 `server_dial_supported`（AutoDL 为 false：Pro 实例只开放 HTTP 端口）与 `server_dial_auto_port`（Vast、RunPod 为 true：驱动按平台的端口映射自选监听端口与接入地址），以及 `resources`：该类型能接入的资源 `[{type,product?}]`，`type` 为 `vm`（虚拟机）、`container`（容器实例）、`host`（物理机）、`kubernetes` 或 `slurm`，`product` 是厂商自己的产品名（如 `ECS`、`ACK`、`E-HPC`），实例类在前、Kubernetes 次之、Slurm 在最后；`slurm` 只有驱动能列出账号下的 HPC 集群（`can_list_hpc_clusters`）的类型才有（阿里云的 E-HPC），`kubernetes` 同理对应 `can_list_clusters`；`category` 是该类型的账号所属的平台类型（`public_cloud`、`private_cloud`），供客户端分组。响应为 `{items,total,categories}`：`categories` 是按控制台展示顺序排列的平台类型，每项 `{id,kinds,resources?,kube_cluster?}`，`kinds` 是归入该类型的 `kind`（顺序同 `items`）；厂商类平台类型只在有 kind 归入时出现，没有声明平台类型的 kind 归入 `other`；`kubernetes` 与 `slurm` 两项总在最后（`kubernetes` 在前）：`kubernetes` 的 `kube_cluster` 为 true、`resources` 为 `[{type:"kubernetes"}]`，`slurm` 的 `cluster_kind` 为 `slurm`、`resources` 为 `[{type:"slurm"}]`，两者的 `kinds` 都是空数组，表示以集群条目（`POST /kube/clusters`、`POST /slurm/clusters`）添加，而不是添加厂商账号 | JWT |
| GET | `/api/v1/providers` | 已配置的云厂商账号列表；每项的 `category` 是该类型所属的平台类型（ZStack Cloud 为 `private_cloud`，公有云厂商为 `public_cloud`），由 `kind` 推导，不单独保存；`resources` 同 `/providers/kinds`；`instance_count` 是上次同步到的实例数（不含托管 Kubernetes 集群的节点和 HPC 集群的计算节点：集群接入后它们是集群的节点，不当实例数）；`node_count` 是经该账号接入的计算节点数（它的实例上的节点加它的集群的节点，同一台机器只计一次），与 `GET /nodes?source=provider:<id>` 列出的节点同口径；`synced_at` 是上次成功列出该账号实例的时间（RFC 3339），从未同步过时省略，同步失败不改它 | JWT |
| POST | `/api/v1/providers` | 新增云厂商账号 | JWT |
| GET | `/api/v1/providers/{id}` | 编辑回读：已保存的 endpoint/public_key 及字典声明的非 secret extra；不回填厂商默认值，不返回密钥。读取凭据失败返回 500，列表仍可用 | 登录会话（平台令牌及 AI/MCP 不可读） |
| PUT | `/api/v1/providers/{id}` | 更新云厂商账号 | JWT |
| DELETE | `/api/v1/providers/{id}` | 删除云厂商账号，连同它的实例记录及该账号的 Cloud 主机发现快照缓存（同一事务，失败回滚）；不释放云上的实例和集群，已接入的节点保留；从它接入的集群条目保留，`source` 去掉、`pool_ids` 不变，此后按独立接入的集群处理（`can_apply`、`can_remove` 为 false，`operation_blocked` 为 `no_source`） | JWT |
| POST | `/api/v1/providers/{id}/sync` | 同步该账号下的实例：列出的实例写入/更新，云端已不存在的实例行移除（关联节点保留，带外恢复置 `stopped`）；返回的 `count` 与 `instances` 同实例列表，不含托管 Kubernetes 集群的节点和 HPC 集群的计算节点；成功时把当前时间记为账号的 `synced_at`；列举失败返回 424、不改动实例行和 `synced_at` | JWT |
| POST | `/api/v1/providers/test` | 测试一组尚未保存的凭据：`{kind, endpoint?, api_key?, public_key?, private_key?, extra?}`；以 SSE 逐项返回检查结果，测试失败仍是 200，见[云平台连接测试](#云平台连接测试)；`kind` 未注册返回 400 `PROVIDER_KIND_UNKNOWN`（`kind`） | JWT |
| POST | `/api/v1/providers/{id}/test` | 测试已保存的账号；请求体可省略，给出时为 `{endpoint?, api_key?, public_key?, private_key?, extra?}`，非空字段只在本次测试里覆盖已保存的值（留空的密钥沿用已保存的值，规则同更新账号），不落库、不返回已保存的密钥；响应同上，见[云平台连接测试](#云平台连接测试)；账号不存在返回 404 | JWT |
| GET | `/api/v1/providers/{id}/clusters` | 该账号下的托管 Kubernetes 集群，实时向云厂商读取、不落库：`{items:[{external_id,name,region,version,type?,type_label?,state,running,public_api,gpu_pools:[{id,name,gpu_model,gpu_count,nodes,vendor,in_entry}],entry?:{uuid,name},connectable,agent_image_source}],total}`；`connectable` 说明能否由它新建条目：集群运行中、有 GPU 节点池且还没有条目；`type` 是集群属于厂商的哪种产品（`ack_managed`、`ack_dedicated`、`ack_edge`、`ack_lingjun`、`ack_registered`、`doks`），`type_label` 是它的中英文名称，驱动分辨不出时两者都不出现；Pod 运行在虚拟节点上的集群（ACK Serverless、ACS）没有能运行 agent 的节点，不列出；`agent_image_source` 是由它建条目时默认的 agent 镜像仓库（按地域）；`entry` 是已由它建的集群条目，`in_entry` 说明该节点池是否已在条目里（DaemonSet 在它的节点上运行）；驱动没有集群能力回 501，云厂商报错回 424 | JWT |
| GET | `/api/v1/providers/{id}/hpc-clusters` | 该账号下的 HPC 集群（阿里云 E-HPC），向云厂商读取、不落库：要逐个地域询问，连接冷时要几秒，所以 server 在内存里留着上一次读到的列表（最长 24 小时），请求立刻拿到它，`listed_at` 是那次读取的时间；列表超过 15 秒时同时在后台重新读取（每个账号同一时刻只读一次），最多等 1 秒，读完就返回新的，否则返回旧的并带 `refreshing: true`，控制台过 2 秒再取一次；还没有读过时等这次读取；`?fresh=1` 等新的读取完成（同步之后用）。与托管 Kubernetes 集群的列表（`/clusters`）并列而不合并：两者是不同的云产品，权限、字段和失败各自独立，一个读不到不影响另一个。先问 E-HPC 在哪些地域有服务（`ehpc:ListRegions`；密钥没有这个权限或调用失败时，改为逐个询问账号的每个地域），再逐个地域只在它自己的 E-HPC 地址读取 `ehpc:ListClusters`（分页读完；不借用别的地域或中心地址的答案，因为响应里没有地域，借来的集群会被记到被问的地域名下），同一个集群只列一次，每个地域各自成败，一个失败不拖垮别的：没有 E-HPC 服务的地域、账号在该地域没有开通 E-HPC 的（代码属 `NotActivated`/`NotOpen`/`NotEnabled` 一类）跳过，没有集群；个别地域失败（限流、该地域的 RAM 策略不放行、网络等）时返回其余地域的集群并标 `partial`；所有地域都失败时才整体失败。`{items:[{external_id,name,region,scheduler,scheduler_version,state,state_tone,vendor_state,running,compute_nodes?,gpu_nodes?,connectable,can_apply,unsupported_reason?,not_connectable_reason?,entry?:{uuid,name}}],total,access,permission?,code?,partial,failed_regions:[{region,code,message,permission?}]}`（地域、ID 升序）：`access` 为 `ok`、`permission_missing`（没有任何地域读得动，且都是 RAM 拒绝：`permission` 是缺的动作 `ehpc:ListClusters`，`items` 为空，不是错误）或 `not_activated`（没有任何地域读得动，账号都没开通 E-HPC：`code` 是云厂商的原话，`items` 为空）；`partial` 为 true 表示有地域没读到，它们在 `failed_regions`（`code`、`message` 是云厂商的，因 RAM 拒绝时带 `permission`），这些地域里的集群可能不在 `items` 里：集群里一个 GPU 节点都还没有、或还没同步过，照样列出；`name` 云厂商没给时是 `external_id`；`scheduler` 为小写的调度器（`slurm`、`pbs`…），`scheduler_version` 是云厂商报的版本；`state` 是集群状态的稳定代码（`uninit`、`creating`、`initing`、`pending`、`running`、`exception`、`stopping`、`stopped`、`releasing`，云厂商文档以外的词是 `unknown`），文字由客户端按语言写；`state_tone` 是状态的颜色语义（`success`、`info`、`neutral`、`danger`），`vendor_state` 是云厂商的原话，`running` 为它报告可用；`compute_nodes` 是云厂商此刻列出的该集群计算节点数（任何状态：创建中、运行中、异常；E-HPC 的 `ListClusters` `Nodes` 三项计数之和，同一次实时读取，列表没给计数时没有这个字段：数量未知，不是 0），云厂商不说其中哪些带 GPU，所以它不是 GPU 节点数；`gpu_nodes` 是上次实例同步里该集群带 GPU 的计算节点数，只在没有 `compute_nodes` 时给出（同步可能是很久以前的，实时计数优先；还没有同步列出过该集群时也没有这个字段）；`connectable` 说明能否由它新建 Slurm 条目：集群运行中、调度器是 Slurm 且还没有条目；`can_apply` 说明建好后平台能否经云账号在 Slurm 管控节点上执行 setup（驱动能在 Slurm 管控节点执行命令；集群的地域和调度器来自上次同步里它的节点，没有节点时来自这次读到的列表，整个响应只读一次云厂商，不是每个集群读一次）；`unsupported_reason` 是没有条目的集群根本不能接入的原因，现在只有 `scheduler_not_slurm`（调度器不是 Slurm），客户端据此写提示，不自己比较调度器（集群还在创建、云厂商还没给出调度器时不算）；`not_connectable_reason` 是 Slurm 集群没有条目、因为状态暂时不能接入的原因，`cluster_starting`（创建或初始化中，运行后可接入）、`cluster_pending`（待配置）、`cluster_exception`（异常，要先在 E-HPC 控制台处理）、`cluster_stopped`（已停止或停止中，要先在控制台启动）、`cluster_releasing`（释放中）或 `cluster_state_unknown`，与 `unsupported_reason` 至多给一个，调度器的原因优先；`entry` 是已由它建的 Slurm 条目。驱动没有 HPC 集群能力回 501，所有地域都失败（不是 RAM 拒绝或未开通）回 424 | JWT |
| POST | `/api/v1/providers/{id}/instances` | 租用新实例；云厂商报错返回 424（不支持返回 501） | JWT |
| GET | `/api/v1/providers/{id}/instances` | 该账号自己的实例列表；账号下托管 Kubernetes 集群的节点（云厂商也把它们列为实例）不在其中，它们随集群接入，对其开关机、释放或调整规格返回 409 `PROVIDER_INSTANCE_IN_CLUSTER`；HPC 集群（阿里云 E-HPC）的计算节点同样不在其中，它们随 Slurm 集群条目接入，由 `GET /providers/{id}/hpc-clusters` 以集群列出，对其开关机、释放、调整规格或安装返回 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER`；`actions` 是实例现在允许的操作（`power_on`、`power_off`、`install`、`delete` 中的若干个），客户端按它展示、不自行推断 | JWT |
| DELETE | `/api/v1/providers/{id}/instances/{externalID}` | 释放/删除实例；云厂商确认后移除该实例行；云厂商报错返回 424（不支持返回 501），实例行不变；托管集群的节点回 409 `PROVIDER_INSTANCE_IN_CLUSTER`（`cluster`），HPC 集群的计算节点回 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER`（`cluster`），都不调用云厂商 | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/power` | 开关机；云厂商报错返回 424（不支持返回 501）；托管集群的节点回 409 `PROVIDER_INSTANCE_IN_CLUSTER`，HPC 集群的计算节点回 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER` | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/resize` | 变更实例规格；云厂商报错返回 424（不支持返回 501）；托管集群的节点回 409 `PROVIDER_INSTANCE_IN_CLUSTER`，HPC 集群的计算节点回 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER` | JWT |
| POST | `/api/v1/providers/{id}/install` | 在已租实例上安装 agent；连接参数同创建节点。需要重启 Pod 时先返回 409，显式允许后才修改；见[云实例安装与重启确认](#云实例安装与重启确认)；实例是托管 Kubernetes 集群的节点时回 409 `PROVIDER_INSTANCE_IN_CLUSTER`（`cluster`）；是 HPC 集群的计算节点时回 409 `PROVIDER_INSTANCE_IN_HPC_CLUSTER`（`cluster`；它们随集群条目接入，不逐台安装） | JWT |

云厂商相关接口里，云厂商 API 或实例 SSH 出错一律返回 424 Failed Dependency，正文 `{error}` 是原因。不用 502/504：Cloudflare 等 CDN/隧道会把源站 502/504 的正文换成它自己的错误页，原因就丢了。

### 云平台连接测试

`POST /api/v1/providers/test` 与 `POST /api/v1/providers/{id}/test` 的响应是 SSE（`Content-Type: text/event-stream`）。
请求本身有误（JSON 无法解析、`kind` 未注册、账号不存在）在开始推送前按普通错误返回；一旦开始推送，HTTP 状态恒为 200，测试结果在事件里。

每完成一项检查推送一个 `check` 事件，全部结束后推送一个 `done` 事件：

```text
event: check
data: {"seq":4,"name":"tcp_connect","status":"failed","duration_ms":12,"code":"PROVIDER_TCP_REFUSED","hint":"tcp_refused"}

event: done
data: {"ok":false,"failed":["tcp_connect"]}
```

- `check`：`{seq, name, status, duration_ms, code?, detail?, hint?}`。`status` 为 `ok`、`failed` 或 `skipped`；`code` 只在 `failed` 时出现，是稳定的错误码；`hint` 是给客户端选取说明文字的键；`detail` 的键随检查项而不同（见下表）。云厂商返回的原文、凭据与代理变量的值都不会出现在事件里，失败原因写入 server 日志（凭据值已替换）。
- `done`：`{"ok":true}`；有检查失败时为 `{"ok":false,"failed":[...]}`，`failed` 按推送顺序列出失败的检查项；测试无法跑完（已保存的配置读不出、该类型的驱动未注册，或整次测试超过 2 分钟）时为 `{"ok":false,"code":"PROVIDER_TEST_UNAVAILABLE"}`。
- 客户端断开连接即取消测试，不再推送后续事件。

检查项按下表顺序执行。一项失败时，依赖它的检查项为 `skipped`，`hint` 为 `depends_failed`，`detail.depends` 是它所依赖的那一项；其余检查项照常执行。

| `name` | 检查内容 | 通过时的 `detail` | 失败的 `code`（`hint`；`detail`） |
|---|---|---|---|
| `endpoint_format` | endpoint 是不带用户名、查询串的 http(s) 地址；留空时用该类型的默认地址 | `scheme`、`host` | `PROVIDER_ENDPOINT_INVALID`（`endpoint_empty`、`endpoint_shape`） |
| `credentials_shape` | 该类型的必填凭据字段都已填写 | `fields`（必填字段数） | `PROVIDER_CREDENTIAL_MISSING`（`credential_missing`；`field`） |
| `dns_resolve` | server 能解析 endpoint 的主机名 | `ips`，endpoint 本身是 IP 时另有 `literal: true` | `PROVIDER_DNS_FAILED`（`dns`；`host`） |
| `tcp_connect` | server 能连上 endpoint 的端口 | `address`、`rtt_ms` | `PROVIDER_TCP_REFUSED`（`tcp_refused`）、`PROVIDER_TCP_UNREACHABLE`（`tcp_unreachable`）、`PROVIDER_TCP_TIMEOUT`（`tcp_timeout`） |
| `tls_handshake` | https 的 TLS 握手与证书校验 | `server_name`、`cert_days_left`；http 为 `mode: "plain_http"` | `PROVIDER_TLS_FAILED`（`tls`） |
| `proxy_detect` | server 进程的代理环境变量，以及访问 endpoint 是否经过代理 | `proxy: "none"`，或 `proxy: "env"`、`variables`（已设置的变量名，按字典序）、`used` | `PROVIDER_PROXY_FAILED`（`proxy`）：代理设置无法使用 |
| `clock_skew` | endpoint 响应的 `Date` 与 server 时钟相差不超过 5 分钟 | `skew_seconds`；没有或无法解析 `Date` 时为 `date_header: "absent"` 或 `"unparseable"` | `PROVIDER_CLOCK_SKEW`（`clock_skew`；`skew_seconds`）、`PROVIDER_HTTP_UNREACHABLE`（`http`） |
| `auth_request` | 用这组凭据向云厂商发一次校验请求 | 无；ZStack 为 `auth`、`hosts` | `PROVIDER_AUTH_FAILED`（`auth`）；ZStack 为 `ZSTACK_*` 错误码 |

访问 endpoint 要经过代理时，server 需要连上的是代理：`dns_resolve` 与 `tcp_connect` 改为检查代理的地址，通过时 `detail` 为 `target: "proxy"`（`tcp_connect` 另有 `rtt_ms`），失败时为 `PROVIDER_PROXY_FAILED`（`proxy`；`target: "proxy"`）；`tls_handshake` 为 `skipped`、`hint` 为 `via_proxy`；代理拒绝连接 endpoint 时 `clock_skew` 以 `PROVIDER_PROXY_FAILED` 失败。

没有 endpoint 的类型（阿里云，由驱动按地域选择地址）：`endpoint_format` 与 `dns_resolve` 到 `clock_skew` 各项为 `skipped`、`hint` 为 `not_applicable`，这种跳过不影响后面的检查项，`auth_request` 照常执行。

ZStack Cloud 在 `auth_request` 之后还有四项，失败时为 `ZSTACK_*` 错误码：

| `name` | 检查内容 | `detail` |
|---|---|---|
| `protocol_shape` | Cloud 的 GPU 查询协议属于哪一代 | `shape`（`modern`、`intermediate`、`legacy`、`gpu_query`，失败时 `unsupported`）、`allocation_field`（`present`、`absent`、`unobserved`）、`cpu_reservation_config`（`bound`、`unbound`、`unprobed`、`error`）；失败时另有 `missing` |
| `api_endpoints` | 产品要用的只读接口是否存在 | `absent`、`denied`、`gpu_query: "pci_fallback"`（各自有才出现）；失败时为 `missing` 或 `endpoint` |
| `read_permissions` | 产品实际会发的只读查询是否被允许 | `queries: "all"`；失败时为 `denied`（`hint` 为 `zstack_permission`）或 `query` |
| `resource_summary` | 读到的资源数量 | `hosts`、`gpus`、`gpus_free` |

### 云实例安装与重启确认

`POST /api/v1/providers/{id}/install` 在已租实例安装 agent，`server_url` 的要求及错误码同[创建节点](#创建节点的连接参数)。

`install_dir` 省略为 `/opt/gpuworks`；显式填写时必须是以 `/` 开头、非根目录的绝对路径，不展开 `~`，否则在创建节点前返回 400 `INSTALL_DIR_INVALID`（`dir`）。填写的目录不可写时安装失败、不回退；实际安装目录会记录到节点，供带外重启、重装使用。

`no_systemd: true` 让 Linux agent 用 nohup，不注册 `gpuworks-agent.service`，规则同[SSH 检查与安装](#ssh-检查与安装)。

- Vast、RunPod 的 `server_dial_auto_port=true`：使用 `connect_mode=server_dial` 且 `server_dial_address` 留空时，自动选已映射且无人监听的 TCP 端口，并保存监听端口与公网接入地址。
  显式给 `server_dial_listen` 时优先用其端口，但该端口必须已经映射。
- AutoDL 的 `server_dial_supported=false`，不支持此模式。
- 阿里云云助手不处理 `server_dial`，自动选择时改用 SSH；云助手在线时，经 SSH 安装前会把平台公钥加入 root 的 `authorized_keys`。

RunPod 需要开放端口或补平台公钥时，先返回 409 `INSTANCE_RESTART_REQUIRED`，此时尚未修改 Pod。
`params.need` 为 `port`、`ssh_key` 或 `ssh_key_port`；`agent_dial` 也可能需要补公钥。
确认后带 `allow_instance_restart:true` 重试，才会修改并重启 Pod，**全部端口会重新编号**。
安装失败返回 424 `{error}`，内容为 SSH 登录、实例授权等需要处理的原因。

## Web Terminal

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/nodes/{uuid}/terminal` | 打开 Web 终端会话（WS upgrade；host 或 container target） | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/terminal/settings` | 该节点终端能力与限制（kill switch、并发上限等） | JWT(admin) |
| GET | `/api/v1/terminal/sessions` | 终端会话审计记录（仅元数据，不留终端内容） | JWT(admin) |

## Networks

见[配置节点网络](../how-to/configure-networks.md)。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/networks` | 网络列表（含加载关系、解析节点数、有提示的节点数）与角色/类型字典（`types`：`host_ethernet` / `host_rdma` / `accelerator_rdma` / `accelerator_hccs`；`type_roles`：每种类型可承担的角色）；第一项是管理网（`id` 0、`builtin: true`） | JWT |
| POST | `/api/v1/networks` | 新建网络（名称、角色、类型、CIDR、网卡名通配）。加速器 RDMA 和加速器 HCCS 只能承担参数角色，否则返回 `NETWORK_TYPE_ROLE_INVALID` | JWT(admin) |
| GET | `/api/v1/networks/{id}` | 网络详情：加载关系、解析到的节点（网卡/IP/HCA/提示，以及节点的用户标签 `labels`）、最近一次内部测试；`id` 0 是管理网，节点带 `roles_off`；不能修改、删除、加载（返回 404），可以发起内部测试 | JWT |
| GET | `/api/v1/networks/{id}/search` | 搜索当前网络解析到的节点（`kind=nodes`）或配置的加载对象（`kind=attachments`）。可选的 `q` 按子串匹配，忽略大小写。查询节点时，可重复传入 `role` 和 `source`。同类条件匹配任一值，角色、来源和关键词同时生效。返回 `{"ids":[...]}`，元素分别为节点 UUID 或字符串形式的加载项 ID。未设置关键词和筛选条件时返回所选范围内全部 ID。 | JWT |
| PATCH | `/api/v1/nodes/management-roles` | 批量修改管理网角色：`{"node_uuids":[...],"allow":[...],"deny":[...]}`，只改列出的角色，所有节点同一事务，返回 `{"updated":N}`（实际有变化的节点数）；节点不存在返回 404 `MANAGEMENT_ROLES_NODE_NOT_FOUND` | JWT(admin) |
| PUT | `/api/v1/nodes/{uuid}/management-roles` | 设置该节点管理网不承担的角色（`{"roles_off":[...]}`），返回该节点的网络视图；只对新实例生效 | JWT(admin) |
| PUT | `/api/v1/networks/{id}` | 修改网络 | JWT(admin) |
| DELETE | `/api/v1/networks/{id}` | 删除网络及其加载关系与测试记录（运行中的实例不受影响） | JWT(admin) |
| POST | `/api/v1/networks/{id}/attachments` | 加载到一个节点（可带网卡/IP/HCA 覆盖）或一个节点标签 | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}` | 修改节点加载的覆盖值 | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}/label` | 修改标签加载项的 `label_key` 与 `label_value`，保留加载项 ID 和加载时间。更新直接节点加载项返回 400，加载项不存在或属于其他网络返回 404，同一网络已加载该标签选择器返回 409 | JWT(admin) |
| DELETE | `/api/v1/networks/{id}/attachments/{attachmentID}` | 移除加载 | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests/estimate` | 同一个请求体，不发起测试：返回解析出的节点数与节点对数；卡测试另返回 `card`（`rounds`、`probes`、最长耗时 `seconds`、上限 `max_seconds`）。发起会被拒绝的请求在这里以同样的错误被拒绝 | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests` | 发起内部测试（`kind`：tcp / rdma_tool / card_ping / card_matrix；rdma_tool 需主机 RDMA 网络，card_ping（同号加速卡互 ping）与 card_matrix（全卡矩阵）需加速器 RDMA 或加速器 HCCS 网络；HCCS 网络使用 `hccn_tool -hccs_ping`，RoCE 网络使用 `-ping`；轮数 × 3 秒超过 10 分钟时返回 `NETWORK_TEST_TOO_LONG`；`scope`：all / nodes / label；`id` 0 测管理网，只支持 tcp），返回 202 与运行记录（`node_uuids` 是被测节点，发起时固定，运行中尚无 `pairs` 时已给出）；由 controller 执行 | JWT(admin) |

网络详情的 `attachments[].unresolved` 为 `true` 时，该直接加载项指定的节点未出现在本网络的有效节点中。

最近一次内部测试 `latest_test.failed_count` 统计结果为失败的节点对，部分通过的节点对计入 `partial_count`。每个节点对的 `rails[].passed` 表示该链路是否通过，`rail_count` 和 `rail_passed_count` 分别是链路总数和通过数（零时省略）。加速卡测试只计入实际探测的卡对。主机 RDMA 使用通过链路的实测平均带宽 `avg_rail_gbps`，仅保存汇总带宽的历史记录没有此字段。全通节点对的带宽读数低于本次测试最快有效读数的十分之一时，服务端返回 `slow: true`。

## Settings / Network Presets

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/updates` | 检查 server、镜像目录和模板目录的更新；`refresh=true` 强制重查，未强制且 `auto_check=false` 时只读上次结果。返回 `update_available`，server 另有 `self_upgrade`、`self_upgrade_blocker`。无可用源仍返回 200、`available=false`，`unavailable_reason` 为 `unpublished/invalid/unreachable`。见[更新检查结果](#更新检查结果) | JWT(admin) |
| POST | `/api/v1/updates/prefetch` | 登录页请 server 在后台开始检查更新：不需登录，总是 204 空响应、不透露任何结果；已有新鲜的检查（成功 1 小时、失败 5 分钟内）或已有检查在跑、或自动检测已关闭时什么也不做，所以无论调用多频繁，server 对外的检查次数不变 | 无 |
| PUT | `/api/v1/updates/channel` | 切换更新渠道 `{"channel":"release"\|"nightly"}`：写入 `server.yaml` 的 `update_channel` 并立即生效，返回 `{channel}`；没有 server.yaml 时 409 `UPDATE_CHANNEL_READ_ONLY`，其他值 400 `UPDATE_CHANNEL_INVALID` | JWT(admin) |
| PUT | `/api/v1/updates/auto-check` | 开关自动检测新版本 `{"enabled":true\|false}`：写入 `server.yaml` 的 `auto_check_updates`（开启时删除该键）并立即生效，返回 `{enabled}`；关闭后 `prefetch` 不再检查，也不拉取通知。自动检查附带安装标识与版本，节点数与 GPU 型号只在 `usage_report` 开启时附带（见[配置](configuration.md#镜像目录与镜像拉取)）。没有 server.yaml 时 409 `UPDATE_AUTO_CHECK_READ_ONLY`，缺少或不是布尔值时 400 `UPDATE_AUTO_CHECK_INVALID` | JWT(admin) |
| GET | `/api/v1/usage-report` | 用户体验改进计划状态 `{enabled, editable, last_day?, last_attempt?: {at, day, ok, error?}, next_due_at?}`：是否加入（`server.yaml` 的 `usage_report`）、能否在控制台修改、最近一份送达的报告覆盖的 UTC 日、本进程最近一次投递、下一份报告的预计时间（[用户体验改进计划](usage-report.md)） | JWT(admin) |
| PUT | `/api/v1/usage-report/enabled` | 加入或退出 `{"enabled":true\|false}`：写入 `server.yaml` 的 `usage_report`（加入时删除该键）并立即生效，返回状态。没有 server.yaml 时 409 `USAGE_REPORT_READ_ONLY`，缺少或不是布尔值时 400 `USAGE_REPORT_INVALID` | JWT(admin) |
| GET | `/api/v1/usage-report/preview` | 下一份报告的原文（`application/json`，与实际发送的字节相同，不取走进程内计数）；未接入改进计划的服务端 404 `USAGE_REPORT_UNAVAILABLE` | JWT(admin) |
| GET | `/api/v1/diagnostics/debug-session` | 当前调试采集（或上一次）`{session, limits}`：`session` 为 `{id, started_by, started_at, until, max_bytes, deployment_ids, nodes: [{uuid, name, state, reason?}], open, ended?, ended_at?, server_log_bytes, request_bytes, requests}`，从未开过时为 `null`；`limits` 是时长与大小的默认值和上限。节点 `state` 为 `collecting`、`skipped`（`reason` 为 `offline` 或 `unsupported`）或 `failed`；`ended` 为 `deadline`、`size_limit` 或 `stopped` | JWT(admin) |
| POST | `/api/v1/diagnostics/debug-session` | 开始调试采集 `{duration_seconds?, max_bytes?, deployment_ids?, node_uuids?}`（省略取默认值 600 秒、10 GiB），返回 201 与状态。时长不在 60–3600 秒或大小不在 1 MiB–50 GiB 时 400 `DEBUG_SESSION_INVALID`（`params.field`、`params.max`），部署或节点不存在 400 `DIAGNOSTICS_SCOPE_UNKNOWN`，已有采集 409 `DEBUG_SESSION_OPEN`，服务端未启用诊断 404 `DIAGNOSTICS_UNAVAILABLE` | JWT(admin) |
| DELETE | `/api/v1/diagnostics/debug-session` | 停止调试采集，返回状态；没有进行中的采集时返回上一次的状态 | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles` | 保留中的诊断信息收集 `{bundles, limits}`，新的在前；每项为 `{id, requested_by, created_at, finished_at?, from, to, deployment_ids, instance_uuids, node_uuids, doctor, state, error?, size_bytes, items: [{id, path, status, code?, params?, message?, evidence?, bytes}]}`，`state` 为 `running`、`ready` 或 `failed`。每个 item 是一项检查：`path` 是它在压缩包里的位置，`id` 是内容类别（`server_log`、`server_host_identity`、`node_agent_log`、`node`、`records`，控制面集群上还有 `member`、`member_file`、`member_index`、`members`，以及成员表读不出来时放在 `server/` 下的 `server_member`、`server_role_events`、`server_leader_changes`、`server_cluster_status` 等）；`status` 为 `pass`（完整收集）、`warn`（达到大小上限只保留部分，`DIAGNOSTICS_ITEM_TRUNCATED`）、`skip`（未读取：节点离线 `DIAGNOSTICS_NODE_OFFLINE`，或 agent 版本不支持 `DIAGNOSTICS_AGENT_UNSUPPORTED`）或 `fail`（`DIAGNOSTICS_ITEM_READ_FAILED`、`DIAGNOSTICS_ITEM_STAGE_FAILED`，读取的错误原文在 `evidence`；控制面集群的某个成员没交出它的部分为 `DIAGNOSTICS_MEMBER_UNAVAILABLE`，`path` 是 `members/<成员>`，`evidence` 以 `unreachable`、`timeout`、`refused`、`unsupported`、`busy` 或 `failed` 开头，收集本身仍为 `ready`：有这样一项的收集缺了那个成员的部分；`path` 为 `members` 的 `DIAGNOSTICS_ITEM_READ_FAILED` 表示本成员的集群成员表读不出来，其他成员都没有被询问）；旧版 server 写下的收集照常列出 | JWT(admin) |
| POST | `/api/v1/diagnostics/bundles` | 开始收集 `{from?, to?, deployment_ids?, instance_uuids?, node_uuids?, doctor?}`（RFC 3339；都省略为最近 1 小时；`doctor` 默认 `true`），返回 202 与 `state: "running"` 的收集。时间范围无效或超过 7 天、节点或实例超过 64 个时 400 `DIAGNOSTICS_BUNDLE_INVALID`，部署或节点不存在 400 `DIAGNOSTICS_SCOPE_UNKNOWN`，已有收集在进行 409 `DIAGNOSTICS_BUNDLE_RUNNING` | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles/{id}` | 一次收集的状态；不存在时 404 `DIAGNOSTICS_BUNDLE_NOT_FOUND` | JWT(admin) |
| GET | `/api/v1/diagnostics/bundles/{id}/download` | 下载已完成的收集（`application/gzip`，`gpuworks-diagnostics-<id>.tar.gz`）；不存在或未完成时 404 `DIAGNOSTICS_BUNDLE_NOT_FOUND` | JWT(admin) |
| GET | `/api/v1/experience` | 功能体验模式状态 `{enabled, ready, port, user, password?, error?}`：体验环境是否在运行、控制台是否已应答、端口（控制台端口 +10）、账号，运行时附密码；自行退出时 `error` 为原因。体验环境是独立进程与数据目录，只含模拟数据；体验环境自身的控制台与无数据目录的服务端 404 `EXPERIENCE_UNAVAILABLE` | JWT(admin) |
| PUT | `/api/v1/experience/enabled` | 开关体验环境 `{"enabled":true\|false}`：开启时清空上次数据、生成新密码并启动，已在运行则不变；关闭时停止进程。返回状态；缺少或不是布尔值时 400 `EXPERIENCE_INVALID`，端口被其他程序占用时 409 `EXPERIENCE_PORT_IN_USE`（`params.port`），其他启动失败 409 `EXPERIENCE_FAILED`（`params.reason`） | JWT(admin) |
| POST | `/api/v1/experience/reset` | 清空运行中体验环境的数据并重新启动，密码不变；未运行时 409 `EXPERIENCE_FAILED` | JWT(admin) |
| POST | `/api/v1/experience/password` | 为运行中的体验环境生成新密码，旧密码的会话随即失效；未运行时 409 `EXPERIENCE_FAILED` | JWT(admin) |
| GET | `/api/v1/notices` | 页头通知铃铛 `{notices: [{id, severity, kind, title?, body?, url?, published_at, read}], unread}`：发布方签名发布的、适用于本安装的通知（`kind` 为 `security` / `release` / `event`，`title`/`body` 按语言给出纯文本）；按紧急程度、再按时间排序，`read` 与 `unread` 按调用者计算 | JWT |
| POST | `/api/v1/notices/read` | 把通知标为已读 `{"ids": [...]}`（1–128 个，每个 ≤64 字节，否则 400 `NOTICE_IDS_INVALID`）；只记调用者自己的已读，重复标记无副作用，204 | JWT |
| GET | `/api/v1/console/manifest` | 控制台外壳在自身页面之外显示的内容 `{edition, features, nav, banners}`，全部由 server 决定，控制台只负责渲染，见[控制台清单与扩展路由](#控制台清单与扩展路由)。社区版服务端固定返回 `{"edition":"community","features":[],"nav":[],"banners":[]}` | JWT |
| GET | `/api/v1/updates/server/versions` | 传入当前保存的 `channel`，返回已验证的服务端版本、当前主机平台、各版本的 Git 提交号（如有）、包摘要与不可安装原因。清单按发布顺序排列，同渠道与跨渠道都只保留当前版本和更新的构建，当前构建不是该渠道发布的版本（`server.unpublished_build`）时列出渠道的全部版本；`latest` 标识渠道索引的最新构建。所选版本的数据库结构比本机数据库旧（索引里的数据库迁移编号更小）时带 `downgrades_schema: true`，见[服务端升级限制](#服务端升级限制)。 | JWT(admin) |
| GET | `/api/v1/updates/server/result` | 传入本次升级的 `attempt` 或已接受响应的 `backup`，核对下载、等待重启、失败或已确认回滚。无匹配证据时返回 `unknown`。 | JWT(admin) |
| POST | `/api/v1/updates/server` | 可传入 `{channel,version,sha256,attempt}` 精确选择包。无选择时沿用渠道最新版本。下载发布包，备份并替换服务端后重启；接受返回 202 `{from,to,backup}`，下一次启动确认或回滚；见[服务端升级限制](#服务端升级限制) | JWT(admin) |
| POST | `/api/v1/updates/server/upload` | 同上，但请求体是上传的发布压缩包（上限 512 MiB）；不是可用的发布包时 400 `SERVER_UPGRADE_ARCHIVE_INVALID`（含控制台升级之前的版本：它们不会确认或回滚自己的启动，`detail` 提示改用 `gpuworks upgrade --archive`） | JWT(admin) |
| GET | `/api/v1/settings` | 全部设置项当前值（不含 `telemetry_export` 与 `branding`，它们只经各自的专用接口读写；也不含调度围栏的三个内部键） | JWT |
| PUT | `/api/v1/settings/{key}` | 修改单个设置项；调度围栏使用的三个内部键 `scheduling.generation`、`scheduling.owner`、`scheduling.confirmation` 不能经此接口写入，回 403（其他 `scheduling.*` 键不受影响）；`telemetry_export`、`branding`、`security` 回 400 `SETTING_HAS_OWN_ROUTE`（`security` 走 `PATCH /api/v1/security/settings`，整体替换会静默清掉请求体没提到的开关）。`deployment`（待调度超时、实例启动超时、事件保留）只有管理员可改（否则 403 `ADMINISTRATOR_REQUIRED`）：请求体叠加在当前值上，只改其中出现的键，叠加后越界回 400 `FIELD_INVALID` 且不写入 | JWT(admin) |
| GET | `/api/v1/security/settings` | 安全开关：`playground_preview_network`（试验场 HTML 预览是否允许发起网络请求，默认 `false`）与 `inference_open_access`（是否接受不带 API key 的 `/v1` 请求，默认 `false`；打开后不带 key 的请求被匿名放行，带了 key 的仍照常校验，网关读不到设置按关闭处理），见[OpenAI 兼容网关的认证](openai-gateway.md#认证)；设置读取失败时接口返回 500 `SECURITY_SETTINGS_UNAVAILABLE`，不返回伪造的默认值 | JWT |
| PATCH | `/api/v1/security/settings` | 修改上述开关，只改 body 里出现的字段；读取现有策略失败时返回 500 `SECURITY_SETTINGS_UNAVAILABLE` 且不写入；成功响应为已提交的策略 | admin |
| GET | `/api/v1/branding` | 品牌外观：`product_name`、`console_title`、`login_description`、`logo`（base64 `data:` URL，PNG/JPEG/WebP/SVG），四个字段总是出现，空串表示用内置默认值（默认文案由客户端按界面语言渲染）。登录页在登录前就要显示，所以不需登录 | 无 |
| PATCH | `/api/v1/branding` | 修改品牌外观，只改 body 里出现的字段，空串恢复该项默认；文字去掉首尾空白后 `product_name` 最多 32 字、`console_title` 最多 64 字、`login_description` 最多 200 字（只有它允许换行），不得含控制字符；`logo` 解码后最多 128 KiB。不合法时 400 `FIELD_INVALID`（`field`、`detail`）且不写入；返回保存后的完整值 | admin |
| GET | `/api/v1/telemetry/export` | 网关 trace 的 OTLP 导出配置与状态：`enabled/endpoint/headers/sample_ratio/trace_link`，header 仅返回名称。`status` 含自 server 启动累计的 `exported/dropped/failed`、当前 `queued`、最近错误 `last_error` 和配置错误 `setting_error`。见[Trace 导出设置与状态](#trace-导出设置与状态) | JWT |
| PUT | `/api/v1/telemetry/export` | 整体保存配置并立即生效；未列出的 header 删除，缺少或为 `null` 的 header 值沿用已存值。字段与错误见[Trace 导出设置与状态](#trace-导出设置与状态) | JWT(admin) |
| POST | `/api/v1/telemetry/export/test` | 用请求体里的草稿设置（同 PUT，不保存、不要求 `enabled`）发一个名为 `gpuworks test span` 的测试 span，只发一次不重试：200 `{ok, trace_id, traces_url, error}`，`ok=false` 时 `error` 是 Collector 的答复或连接错误；草稿本身不合法时同 PUT 的 400 | JWT(admin) |
| GET | `/api/v1/pull/source-rules` | 来源规则列表（按生效顺序）：`{id, position, kind, match, endpoints[], allow_upstream, control_plane, note, node_requirements, matched_nodes}`；`kind` 由 `match` 推出：带 `http://`/`https://` 的是站点规则 `url`（模型站点，如 `https://huggingface.co`），否则是镜像规则 `image`；`control_plane` 为管理节点取模型元数据时也按此规则找地址（只有站点规则可设） | JWT |
| POST | `/api/v1/pull/source-rules` | 新建来源规则；`allow_upstream` 必填，`control_plane` 默认 `false`。站点规则的 `match` 是站点根地址（不带路径），端点是 http(s) 地址、可带路径；规则不合法 400 `PULL_SETTING_INVALID`（`field`：`match`、`endpoints`、`control_plane`、`note`、`requirements`） | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/{id}` | 整体替换一条规则 | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/order` | 重排：`{"ids":[...]}` 列出全部规则各一次 | JWT(admin) |
| DELETE | `/api/v1/pull/source-rules/{id}` | 删除规则 | JWT(admin) |
| POST | `/api/v1/pull/source-rules/test` | 用草稿 `{match,endpoints,allow_upstream,node_uuid,repo}` 测试站点规则，不保存；`repo` 默认 `Qwen/Qwen3-0.6B`，返回各地址的状态与耗时；见[来源规则连通测试](#来源规则连通测试) | JWT(admin) |
| GET | `/api/v1/pull/sync-groups` | 同步组列表：`{id, name, node_requirements, rate_limit_kib_s, matched_nodes}` | JWT |
| POST | `/api/v1/pull/sync-groups` | 新建同步组（节点条件选择成员，限速可选） | JWT(admin) |
| PUT | `/api/v1/pull/sync-groups/{id}` | 整体替换同步组 | JWT(admin) |
| DELETE | `/api/v1/pull/sync-groups/{id}` | 删除同步组 | JWT(admin) |
| GET | `/api/v1/pull/metadata-sources` | `?source=` 取 `huggingface` 或 `modelscope`：控制台浏览器读取模型元数据的地址，即勾选了「用于管理节点获取模型元数据」的站点规则给出的候选（按顺序，最后是原站，除非规则关闭回源）：`{source, bases[]}`；规则禁止该站点所有地址时 `bases` 为空。其他 `source` 400 `FIELD_INVALID` | JWT |
| GET | `/api/v1/pull/proxies` | 代理条目列表（按匹配顺序，节点用首个选中它的条目）：`{id, position, http_proxy, https_proxy, no_proxy, username, has_password, control_plane, note, node_requirements, matched_nodes}`；地址不含登录信息，密码从不返回；`control_plane` 为管理节点出站请求也走此代理（取首个勾选的条目） | JWT |
| POST | `/api/v1/pull/proxies` | 新建代理条目：`{http_proxy, https_proxy, no_proxy, username, password, control_plane, note, node_requirements}`，至少一个代理地址；地址写成 scheme://host:port（`http`/`https`/`socks5`/`socks5h`，省略时为 `http`），登录信息写进地址 400；`no_proxy` 与 NO_PROXY 环境变量同一写法。密码加密存储。不合法 400 `PULL_SETTING_INVALID`；保存后在线节点各收到一次只含网络部分的更新 | JWT(admin) |
| PUT | `/api/v1/pull/proxies/{id}` | 整体替换一条代理条目；不给 `password` 保持已存的值，给空串清除 | JWT(admin) |
| PUT | `/api/v1/pull/proxies/order` | 重排：`{"ids":[...]}` 列出全部条目各一次 | JWT(admin) |
| DELETE | `/api/v1/pull/proxies/{id}` | 删除代理条目 | JWT(admin) |
| GET | `/api/v1/pull/credentials` | 凭据列表（每个 host 一条）：`{id, host, username, has_secret}`；镜像仓库是用户名加密码，模型站点只有令牌（`username` 为空，下载与取元数据时作为 `Authorization: Bearer` 发送）；密码或令牌从不返回 | JWT(admin) |
| POST | `/api/v1/pull/credentials` | 新建凭据（`host`、`username`、`secret`），加密存储；`host` 规范为不带协议与路径的 host（Docker Hub 的各种写法归为 `docker.io`），每个 host 一条 | JWT(admin) |
| PUT | `/api/v1/pull/credentials/{id}` | 替换凭据；`secret` 为空保持已存的值 | JWT(admin) |
| DELETE | `/api/v1/pull/credentials/{id}` | 删除凭据 | JWT(admin) |
| POST | `/api/v1/pull/credentials/{id}/test` | 从管理节点（经它的出站代理）试用凭据：有用户名的按镜像仓库登录 `/v2/`，只有令牌的按 Hugging Face 的 `GET /api/whoami-v2` 校验；200 `{ok, message}`，被拒 422 `CREDENTIAL_LOGIN_REJECTED`（`host`），连不上 502 `CREDENTIAL_LOGIN_FAILED`（`host`、`detail`） | JWT(admin) |
| GET | `/api/v1/pull/registry-credentials` | 已删除：本路由及其 `POST`、`PUT/DELETE {id}`、`POST {id}/test` 回 410 `SETTING_REMOVED`（`route` 为 `/api/v1/pull/credentials`） | JWT(admin) |
| GET | `/api/v1/server/listen` | server 的监听地址：已保存的 `console`/`agent`/`gateway` 地址、当前实际打开的 `running` 套接字、`restart_pending`（保存的地址要重启才生效）、`config_path`，以及 `editable`/`overridden`（未用 server.yaml 启动或被 `--addr` 覆盖时不可编辑） | JWT(admin) |
| GET | `/api/v1/server/outbound-policy` | 出站私网访问策略：原始 `restrict_private_hosts`（默认 false）、`allow_private_hosts`、`editable` 与仅基于策略值的 `revision`；不返回配置文件其他字段 | JWT(admin) |
| PUT | `/api/v1/server/outbound-policy` | 原子保存全部必填的 `restrict_private_hosts`、`allow_private_hosts` 与 `revision`，返回更新后的策略；写入 server.yaml 后立即生效。400 `SERVER_OUTBOUND_POLICY_INVALID`，409 `SERVER_OUTBOUND_POLICY_CONFLICT` / `SERVER_OUTBOUND_POLICY_READ_ONLY`，503 `SERVER_OUTBOUND_POLICY_UNAVAILABLE`；保留其他配置及注释，不支持安全编辑的文件保持原样 | JWT(admin) |
| POST | `/api/v1/server/outbound-policy/check` | 只读检查 `{hosts:[]}` 中的纯主机名或 IP，返回 `{items:[{host,status}],total}`，状态为 `allowed` / `needs_allow` / `blocked` / `unresolved`。最多 32 项、每项 253 字节、请求体 16 KiB，总超时 10 秒；拒绝 URL、端口、CIDR 和嵌入凭据。只做 DNS 与当前出站策略检查，不连接上游、不写白名单，也不证明来源可达。非法请求返回 400 `SERVER_OUTBOUND_POLICY_INVALID`；不返回 DNS 地址或解析细节 | JWT(admin) |

同步组创建和改名的名称按 trim 后精确文本唯一（区分大小写，最多 64 个 UTF-8 字节）。重复名为 400 `PULL_SETTING_INVALID`，`params.field=name`、`params.reason=name_duplicate`。校验错误提供稳定 `reason` 和参数（如 `max`），英文 `detail` 保留供旧客户端和诊断；界面按 `reason` 本地化。

### 更新检查结果

`GET /api/v1/updates` 同时查询发布源，使用第一个验签通过的索引。同一渠道的结果复用 1 小时，`refresh=true` 立即重查。

- `channel`、`channel_editable`：当前渠道及是否允许切换。
- `auto_check`：自动检测开关。为 `false` 时仅返回上次手动检查的结果；从未检查则为空答复，没有 `sources` 或 `unavailable_reason`。强制刷新仍会检查。
- 来源信息：胜出源，以及已答复来源的 `{url,ok,latency_ms,error}`；尚未答复的慢源不列出，界面不展示来源详情。
- `server`、`image_catalog`、`template_catalog`：各含 `current`、`latest`、`update_available`。`image_catalog` 在当前目录是手工上传的时另有 `uploaded: true`，此时 `update_available` 恒为 `false`；渠道目录与它不同时还有 `replaces_upload: true`，表示应用渠道目录会替换上传的目录。模板另有 `compatible`；server 另有 `installable`、`self_upgrade`、`self_upgrade_blocker`、`install_dir`（`server.yaml` 所在安装目录，无目录时省略）。

server 只有渠道版本更新时才标记 `update_available`。两份目录还须与当前内容不同，且发布不早于当前构建，避免把旧目录标成更新。

`server.unpublished_build: true` 表示当前构建按该渠道构建、构建时间晚于渠道最新版本，却不在渠道的版本清单里（分支构建或本地构建）。构建时间不能说明它与渠道版本谁新：此时 `server.update_available` 为 `false`，界面显示「非渠道发布的构建」而不是「已是最新」，两份目录只要与当前内容不同就标记 `update_available`。
无可用源仍返回 200、`available=false`，由 `unavailable_reason` 区分：

- `unpublished`：源可达，但该渠道没有索引。
- `invalid`：内容无法验签或解析，例如被登录网关拦截。
- `unreachable`：无源答复，例如网络、代理、DNS、超时或服务端错误。

### 服务端升级限制

`POST /api/v1/updates/server` 下载本平台发布包，按签名索引校验，备份替换后重启。返回 202 `{from,to,backup}`，下次启动确认或回滚。

在线更新检查、候选列表与安装请求使用同一排序规则，不因跨渠道而放行旧版本。两边都是正式版本号 `vX.Y.Z` 时比较版本号，否则比较构建时间；时间无法解析时沿用原有更新检测行为，将不同版本视为可更新。签名索引中的数据库迁移编号不作为在线安装门槛；它只在候选版本上作为事实给出（`downgrades_schema`）。安装一个数据库结构比本机旧的版本时，它无法在这份数据库上启动（启动时报「数据库结构比本版本支持的更新」），连续启动 3 次仍未起来后，启动守卫恢复升级前的快照（数据库、`server.yaml`、主密钥与旧二进制）并记下这次升级失败。分支构建的数据库可能包含渠道版本没有的迁移，所以这条在从 `unpublished_build` 的构建换回渠道版本时最常见。上传发布包保持原有验证方式，不受这项在线排序限制。当前构建不是该渠道发布的版本（`server.unpublished_build`）时没有可比的顺序：候选列表列出渠道的全部版本，安装请求指明 `version` 即可安装其中任意一个，不指明版本的请求仍按下面的 409 拒绝。

- 409 `SERVER_UPGRADE_UNAVAILABLE`：不能自升级，`reason` 为 `no_service_manager`、`not_installed`、`not_installed_binary`、`cluster_member`（控制面集群的成员不在控制台升级）或 `not_writable`。
- 409 `SERVER_ALREADY_AT_RELEASE`：已是目标版本；409 `SERVER_NEWER_THAN_RELEASE`：目标构建不比当前版本更新。
- 404 `UPDATE_NOT_PUBLISHED`：渠道没有本平台的包。
- 502 `UPDATE_DOWNLOAD_FAILED`：无来源能提供与索引大小、SHA-256 一致的包。`file` 指出文件，`detail` 给出各源原因；不会替换运行中的服务。

### Trace 导出设置与状态

`GET /api/v1/telemetry/export`

网关请求 trace 的 OTLP 导出设置与运行状态：`enabled`、`endpoint`（Collector 的 OTLP/HTTP 地址）、`headers`（只列 `{name}`，值从不返回）、`sample_ratio`（网关自起 trace 的采样比例，0..1，从未设置时为 1）、`trace_link`（含 `{trace_id}` 的外部追踪系统链接，可空）。

`status`：`active`、`traces_url`（实际发往的地址）、自 server 启动以来的 `exported`/`dropped`（队列满丢弃）/`failed`（被拒或送达失败）、`queued`、`last_error` 与 `last_error_at`、`last_export_at`（Unix 毫秒，没有时 `null`）、`setting_error`（已存设置无法应用的原因，如解密失败）。

`PUT /api/v1/telemetry/export`：整体保存设置并立即生效：`{enabled, endpoint, headers:[{name, value}], sample_ratio, trace_link}`；header 不给 `value`（或为 `null`）时沿用已存的同名值，没列出的已存 header 删除；`sample_ratio` 不给为 1。header 值加密存储。

地址无效 400 `TELEMETRY_ENDPOINT_INVALID`，header 名非法、重复或是导出器自设的 400 `TELEMETRY_HEADER_INVALID`（`name`），新 header 没给值 400 `TELEMETRY_HEADER_VALUE_REQUIRED`（`name`），比例越界 400 `TELEMETRY_SAMPLE_RATIO_INVALID`，链接不是含 `{trace_id}` 的 http(s) 地址 400 `TELEMETRY_TRACE_LINK_INVALID`；未知字段 400。

### 来源规则连通测试

`POST /api/v1/pull/source-rules/test` 接收 `{match,endpoints,allow_upstream,node_uuid,repo}`，测试草稿但不保存。
一次针对一台节点；控制台选 1–5 台时分别并行调用。`repo` 默认 `Qwen/Qwen3-0.6B`。

探测沿用实际下载的代理、`no_proxy` 和凭据，并行访问各地址，每项超时 10 秒：

- Hugging Face 类站点：先取 `/api/models/<repo>/revision/main`，再读 `/<repo>/resolve/main/config.json` 的首字节；门禁仓库可能只在文件请求时拒绝。
- `modelscope.cn`：访问 `/api/v1/models/<repo>`。

返回 `{source,repo,node_uuid,node_name,outcomes:[{base,status,code,latency_ms,error}]}`。
`status` 为 `ok`、`auth_required`（401/403，需凭据）、`http_error` 或 `unreachable`。
草稿非法返回 400 `PULL_SETTING_INVALID`；节点不存在 404、离线 409 `NODE_OFFLINE`；
节点无法执行返回 502 `SOURCE_PROBE_FAILED`（`detail`），例如 agent 版本过旧。

### 已移除的设置接口

`PUT /api/v1/settings/image_registry_rewrites`、`image_registry_mirrors` 及镜像加速预设设置回 410 `SETTING_REMOVED`，镜像来源改由 `/pull/source-rules`（本节上表）表达。`PUT /api/v1/settings/network` 与 `network:<节点 id>` 同样回 410 `SETTING_REMOVED`（`route` 为 `/api/v1/pull/proxies`）：代理改为按节点标签的代理条目，URL 改写改为站点规则；升级时旧的全局与节点设置自动转换（全局改写为不限节点、用于管理节点的站点规则，节点覆盖为限定该节点、排在前面的规则与代理条目，节点生效的代理不变）。网络加速预设（`GET /api/v1/network/presets`）已删除。

## Metrics / Logs

### 日志、指标与事件时间线

日志与事件时间线使用游标查询更早的记录，指标使用时间范围，不能套用上面的 `page` 分页：

| 接口 | 查询与翻页方式 |
|---|---|
| `GET /api/v1/logs/search` | `limit` 默认 200、上限 5000；`cursor_ts` 与 `cursor_id` 标记翻页位置；`with_total=1` 时附总命中数 |
| `GET /api/v1/metrics/query` | `range` 为查询时长、`step` 为采样步长，均使用 Go duration 字符串，默认分别为 `6h`、`1m`；可用 `node`、`gpu`、`gpu_uuid`、`instance`、`benchmark`、`point` 等标签条件筛选 |

`GET /api/v1/instances/{uuid}/events` 和 `GET /api/v1/deployments/{id}/events` 返回历史事件时间线，
按最新优先排列，响应为 `{"items": [...]}`。`limit` 默认 100、上限 500；下一页传
`before=<上一页最后一条的 id>`。非法游标返回 `400 EVENT_CURSOR_INVALID`，非法页大小返回 `400 EVENT_LIMIT_INVALID`。

时间线会在**读取时合并连续重复事件**：同一实例内，`kind`、`code`、`state`、`phase` 都相同的连续记录，
只返回最新的一条。因此，旧库里大量重复的进度上报会呈现为较少的状态里程碑。
带 `code` 的记录用 `params` 承载详细值，不再重复下发 `data` 原文；早于 code 机制的历史记录仍带 `data`。进入 `failed` 的记录在 `params.failure` 带结构化失败原因（形状同实例的 `runtime_failure`），`params.detail` 仍是英文 fallback。


| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/metrics/query` | 时序指标查询（`metric`、`range`、`step`、`node`/`gpu`/`gpu_uuid`/`instance`/`benchmark`/`point`/`iface`/`hca`/`role` matcher）。返回 `{"items": [{"labels": {…}, "points": [[unix_ms, value], …]}], "total"}`，每个 label 组合一条序列。没有数据且指标名不在指标目录（`GET /api/v1/monitor/metrics`）里时回 400 `METRIC_UNKNOWN`，`params.similar` 给出最接近的已登记名；目录里有但窗口内没有数据时正常返回空 | JWT |
| GET | `/api/v1/logs/search` | 日志检索/tail（`q`、`instance` 前缀、`instances`（逗号分隔 uuid 精确匹配集合，最多 64 个，与 `instance` 互斥）、`node`、`level`、`source`、`from`/`to`、`cursor_ts`/`cursor_id`、`limit`、`with_total`；按到达序倒序分页；未指定时间边界表示该方向不限制，均未指定时查询所有尚未清理的日志；列表与总数使用同一范围，页面默认显式传最近 24h）。返回 `{"items": [{"id", "instance", "node", "level", "source", "time_ms", "line"}], "total", "total_capped", "truncated"}`（后三项按需出现） | JWT |
| GET | `/api/v1/logs/context` | 单条命中的上下文窗口（`instance`、`ts`、`id`、`before`/`after` 取值 0–1000），等价 grep -C。返回 `{"items": [日志行], "match_index"}`；`match_index` 是锚点行在 `items` 中的下标，锚点行不在该分区时（已被保留策略清理，或 `ts` 与 `id` 不同天）为 `-1`，此时 `items` 仍是该位置附近真实存在的行，接口仍返回 200。调用方必须判 `match_index >= 0` 再取值，或只用它做比较 | JWT |
| GET | `/api/v1/logs/histogram` | 时间桶 × 级别计数（同 search 过滤参数，含 `instances`，+ `bucket` 毫秒，1s–24h）。返回 `{"items": [{"start_ms", "counts": {"error": N, …}}], "total"}`；桶按 UTC 对齐 | JWT |
| GET | `/api/v1/monitor/overview` | 监控中枢集群层快照（`range` ∈ `15m\|1h\|6h\|24h\|168h`，默认 `1h`，或自选时间段 `from`+`to`（见下文）；信号行（只报事实：数值、明细与近期走势，不带严重程度）、服务表（每行带 `placement`（见[服务监控返回值](#服务监控返回值)）与该部署的 `health` 健康检查，按健康状况排序：失败、超时、无可用副本、检查中、正常、未开启，同档按名称）、`state_counts`（按服务状态计数：每种状态各一项、含 0，顺序即状态摘要的顺序）、显存条、集群 tok/s、容量事件） | JWT |
| GET | `/api/v1/monitor/deployments/{id}` | 服务监控快照；`range` 为 `15m/1h/6h/24h/168h`（默认 `1h`），也可使用 `from`+`to`。含请求、延迟、副本、`health`、供日志查询的 `log_instances`、指标来源 `sources`（engine/gateway）；PD 另有 `pd` 的降级、Prefill 统计与 `kv_transfer`。未知部署 404；已删除的部署 404 `MONITOR_DEPLOYMENT_DELETED`，`params` 含 `deployment_id`、`name`、`served_name`。见[服务监控返回值](#服务监控返回值) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/requests` | 逐请求视图：`view=live`（默认）或 `history`；`limit` 1–500（实时默认 50），可按 `key`、`instance` 筛选，历史还可按 `priority_class` 筛选。历史另支持 `range` 或成对 `from`+`to`（Unix 毫秒）、`cursor`、`min_ms/max_ms`；`results` 为 `completed/length/client_closed/interrupted/error`，多个值用逗号分隔。记录含 `result/segments/pd/trace_id`，以及 `priority_class`（网关给请求的优先级，启用前的记录和进行中的请求为空）；用 `next_cursor` 翻页。见[逐请求查询](#逐请求查询) | JWT |
| GET | `/api/v1/monitor/requests/lookup` | 跨部署查已结束请求；`trace_id`（32 位十六进制）或 `request_id` 必须二选一。可成对传 `from`+`to`（Unix 毫秒，最多 24 小时），`limit` 1–500（默认 100）；返回 `requests/deployments/window`。未传时间时按网关 trace 时间定位或查最近 1 小时；抽样丢弃的请求查不到。见[跨部署查找请求](#跨部署查找请求) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/latency` | `range` 内完整回复的端到端延迟、阶段耗时与生成速度：`requests` 为数量，`end_to_end`、`segments` 含 `mean_ms/p50_ms/p95_ms`；`speeds` 含 `prefill_tok_s/thinking_tok_s/answering_tok_s/decode_tok_s`。不受日志抽样影响。见[延迟汇总口径](#延迟汇总口径) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/signals` | 部署详情指标面板的序列：`start`（Unix 毫秒，必填）、`end`（默认现在）、`step`（如 `30s`，可省略）。窗口不限于监控中枢的固定范围；窗口内点数超过 600 时 `step` 会被调大，实际值在 `step_ms`。返回 `{start_ms,end_ms,step_ms,sources,concurrency,kv,throughput}`，三组序列的 `labels.metric` 分别为 `running`/`waiting`、`usage_pct`、`prompt_tok_s`/`generation_tok_s`；`running`、`prompt_tokens`、`generation_tokens` 各自取引擎值，引擎没有数据时取网关值，`sources` 给出各取自谁，口径与服务监控的 `sources` 相同。引擎的序列带 `instance` 标签；网关按部署统计、不分实例，取自网关的序列不带该标签。`waiting` 与 `usage_pct` 只能由引擎提供。不缓存。`start`、`end`、`step` 非法时 400 `MONITOR_RANGE_INVALID`（`detail`）；未知或已删除的部署同服务监控 | JWT |
| GET | `/api/v1/monitor/distributions` | `range` 内结束的请求分布：`input_output`（输入档 × 输出档）与 `uncached_ttft`（非缓存输入档 × 首 token 时间档）两个网格的 `{x, y, n}`，档位下界 `token_buckets` 与首 token 档上界 `ttft_bounds_ms`，`coverage` 给总请求数、带 usage 的数与报了缓存命中的数；`deployment` 可选，缺省为全集群，未知部署 404 | JWT |
| GET | `/api/v1/monitor/metrics` | 指标目录，返回 `{items:[{series,name,source,kind,unit,quantiles,labels,description}],total}`。`source` 为 `engine`、`gateway`、`gpu`、`dgpu`、`node`、`network`、`kv_disk` 或 `benchmark`；推理直方图展开为分位数 gauge（`quantiles:true`）及 `_sum`、`_count`。字段口径见[推理指标词表](inference-metrics.md) | JWT |
| GET | `/api/v1/monitor/series` | 指标浏览器（`metric` 必填，`range` ∈ `15m\|1h\|6h\|24h\|168h`，默认 `1h`，或 `from`+`to`（`step` 默认约 240 个点、取 10 秒整数倍）；`by` ∈ `node\|deployment\|instance\|gpu`，`fn` ∈ `avg\|sum\|max\|rate`，默认 `avg`；`step` 默认按 `range` 推导，且不会低于该推导值；结果超过序列/点数上限时返回 400；未知指标返回 `[]`）。带 `node` 标签的序列另带只用于显示的 `node_name`，带 `deployment` 标签的另带 `deployment_name`；节点或部署已删除时取删除时的名字，并带 `node_deleted` / `deployment_deleted` 为 `"true"`，两边都不认识的保持原样 | JWT |

监控图表的自选时间段：`overview`、`deployments/{id}`、`deployments/{id}/latency`、`distributions`、`series` 都接受 `from`+`to`（Unix 毫秒，成对给出，与 `range` 互斥），跨度 1 分钟到 30 天、`to` 不能在未来，否则 400 `MONITOR_RANGE_INVALID`（`detail`）。此时图表、分布、延迟拆解与信号按截至 `to` 的这段时间计算，不缓存；服务列表、副本、健康检查等当前状态仍是现在的。

### 服务监控返回值

`GET /api/v1/monitor/deployments/{id}` 返回请求对账、首 token 拆分、图表与副本信息。
`range` 支持 `15m`、`1h`（默认）、`6h`、`24h`、`168h`，也可使用成对的 `from`+`to`；未知部署返回 404，已删除的部署返回 404 `MONITOR_DEPLOYMENT_DELETED`（`params`：`deployment_id`、`name`、`served_name`）。

- `deployment.placement`（监控中枢服务表每行的 `placement` 同形）：部署放在哪里，只报事实、不带显示文案，由界面按语言格式化。`nodes` 是节点名，按节点 rank 排序（副本逐个列出，各副本 leader 在前，每个节点只出现一次）；`nnodes` 是最宽那个副本占的节点数，单机为 1；`gpus_per_node` 是单个实例占的最多卡数；`tensor_parallel` 仅当引擎的启动命令写明张量并行度时出现（vLLM、SGLang 的 `--tensor-parallel-size`、`--tp` 等），callable 配方的并行方式平台不知道，各实例写的度数不一致（例如 PD 各角色）时同样省略，不用卡数或节点数推算。尚无实例时 `nodes` 为 `[]`，两个计数为 0。
- `log_instances`：存活及窗口内近期停止的副本 UUID，最多 64 个，可用于 `/logs/search?instances=...`。
- `sources`：`ttft`、`e2e`、`prompt_tokens`、`generation_tokens`、`running`、`tpot`、`queue_time`、`prefill_time`、`decode_time`、`kv_hit_rate`（前缀缓存命中率，网关值只统计报了缓存命中数的请求，见[网关缓存命中率](inference-metrics.md#网关缓存命中率)）各取自 `engine` 还是 `gateway`，没有数据的信号省略。口径见[推理指标](inference-metrics.md#网关指标)。
- `health`：同部署详情的[健康检查](#健康检查)。

部署详情的指标面板不读这个接口，而是读 `GET /api/v1/monitor/deployments/{id}/signals`（见上表），逐信号取来源，因此引擎只上报部分指标（例如有运行/等待请求数和缓存使用率、没有 token 计数）时，面板的 token 吞吐来自网关，与这里的 `charts` 一致。单个实例的面板（实例详情的指标页）只显示该实例引擎自己的序列：网关不按实例统计，不会把整个部署的网关数值当作某个实例的。

PD 部署另返回 `pd`：

- `effectiveness_pct`、`fallback_rate_pct`、`fallbacks`（降级原因对应次数）、`prefill_waiting_avg`、`avg_prompt_tokens`。
- `prefill_stage_avg_ms`、`prefill_stage_p95_ms`：网关等待 prefill 的平均时间与 P95，均取近 5 分钟；P95 来自请求分段直方图。
- `kv_transfer.exposed`：引擎是否导出 KV 传输指标；`vllm-ascend-mooncake` 与 `vllm-mooncake` 为 `false`，其余传输字段均为 `null`。
- `kv_transfer` 的其他字段：每次传输的 `avg_ms`、`p95_ms`、`avg_mb`，传输速度 `mb_per_s`，窗口内的 `failed`（decode 拉取失败）和 `expired`（prefill KV 租约到期未被拉走）次数。没有数据为 `null`。

### 跨部署查找请求

`GET /api/v1/monitor/requests/lookup` 的 `trace_id`、`request_id` 必须二选一。
trace id 为 32 位十六进制、不区分大小写，查询该 trace 的全部请求；请求 ID 查询对应请求。
`limit` 为 1–500，默认 100；时间范围按以下顺序确定：

1. 给出 `from`、`to` 时，按接收时间查询，两者须成对提供（Unix 毫秒，间隔不超过 24 小时）。
2. 未指定范围且 trace 由网关生成时，先查 trace 时间前 10 分钟至后 1 分钟，未命中再查最近 1 小时。
3. 其他情况查最近 1 小时。

返回 `{as_of,from_ms,to_ms,window,requests,deployments}`。
`window` 标明窗口来源：`requested`、`trace_time` 或 `default`。
`requests` 与逐请求视图同形，按接收时间倒序；`deployments` 为所属部署的 `{id,name,deleted}`：`name` 是对外模型名；已删除的部署取删除时的名字，`deleted` 为 `true`；删除时还没有保存名字的旧部署名称为空。
超出每秒日志预算、被抽样丢弃的正常请求无法查到。参数非法返回 400。

### 逐请求查询

`GET /api/v1/monitor/deployments/{id}/requests` 通过 `view` 选择：

- `live`（默认）：进行中的请求，按等首字最久、思考中、输出中排序。`counts` 统计全部在途请求的 `waiting`、`thinking`、`answering` 数。
- `history`：已结束请求倒序返回，下一页把 `next_cursor` 作为 `cursor` 传入。
  用 `range` 查询截至现在的时段，或成对传 `from`、`to`（Unix 毫秒）。
  响应的 `from_ms`、`to_ms` 是实际范围；`range_total` 是范围内网关结束的全部请求数，按 5 分钟档统计，不受筛选和日志抽样影响。

两种视图都可用 `limit`（1–500，实时默认 50）、`key`（单个 API key）、`instance`（单个副本）。
历史还支持端到端耗时 `min_ms`、`max_ms`，首个 token 时间 `min_ttft_ms`、`max_ttft_ms`（只保留出过首个 token 的流式回复），以及逗号分隔的 `results`：
`completed` 正常完成、`length` 长度截断、`client_closed` 客户端退出、`interrupted` 连接中断、`error` 出错。
未知部署返回 404，非法参数或游标返回 400。

每条记录的关键含义如下：

- `result` 是请求结果；`segments[{kind,ms,stage}]` 是阶段耗时，合计等于端到端时间或在途时长；`stage` 是该段所属的部分：`first_token`（首个 token 之前，合计等于 `ttft_ms`）、`decode` 或 `tail`；非流式回复、以及没出过 token 就结束的流式请求为空。
- `source` 是 `api`、`playground`、`batch` 或 `evaluation`（从 server 发压的评测）。
- `ttft_ms` 是收到请求到首个生成内容（含思考）的时间，非流式、仍在等首字或没出过 token 就结束时为 `null`；`tpot_ms`、`prefill_tok_s`、`decode_tok_s` 是 Decode 阶段每个输出 token 的时间与 Prefill / Decode 速度（引擎上报了 decode 耗时就用它除以输出 token 数，否则用首个到最后一个 token 的时间除以首个之后的 token 数），缺少所需数据或请求进行中时为 `null`。
  阶段名称、边界与计算方式见[请求阶段](request-timeline.md#2-阶段)。
- `pd` 仅 PD 请求有值，其余为 `null`。包含 `prefill_instance_uuid`、`prefill_ms`、`prefill_queue_ms`、`prefill_compute_ms` 和 `fallback`；
  降级时 prefill 实例为空，未报告的时长为 `null`。并发交接的 `prefill_ms` 为 `null`；顺序交接的请求仍在 prefill 时也为 `null`，转发给 decode 后即使请求尚未结束，也可返回该阶段耗时。
  仍在 prefill 的请求可在实时视图查到，`dispatch_ms` 为 `null`。含义见[PD 请求](request-timeline.md#6-pd-分离部署)。
- `trace_id` 是 32 位十六进制 W3C trace id：继承调用方的有效 `traceparent`，否则由网关生成。`deployment_id` 标识部署，`weight` 是抽样权重。
- `received_ms` 是 Unix 毫秒绝对时间，其余时间点相对网关收到请求计时；未发生或未观察到的值为 `null`。详见[时间点定义](request-timeline.md#1-记录了哪些时间点)。

### 延迟汇总口径

`GET /api/v1/monitor/deployments/{id}/latency` 统计 `range` 内完整回复（正常完成或长度截断）：

- `requests`：完整回复数；`end_to_end{mean_ms,p50_ms,p95_ms}`：端到端耗时，无请求时为 `null`。
- `segments[]`：按阶段排序，每项含 `kind`、`mean_ms`、`requests`、`p50_ms`、`p95_ms`。
  平均值包含全部完整回复，无该阶段按 0 计，各段均值之和等于端到端均值；次数与分位数只计算有该阶段的请求，分位数采用四分之一倍频程分档。
- `speeds`：`prefill_tok_s`、`thinking_tok_s`、`answering_tok_s`、`decode_tok_s`，按符合条件的 token 总数除以时长总和，无数据为 `null`。

统计不受日志抽样影响；各项纳入条件见[延迟与速度的计算口径](request-timeline.md#4-汇总端到端延迟拆解与速度)。
未知部署返回 404，非法 `range` 返回 400。

## Analytics（用量分析）

长期统计与数据保留，见 [ADR 0055](../adr/0055-long-term-statistics.md)。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/data-retention` | 数据保留设置：`{value, limits, server_time_zone}`。`value` 的字段：`metrics_hours`、`logs_hours`（指标原始样本、容器日志）、`request_log_days`、`request_log_max_mb`（逐请求记录 ux.db，天数与容量哪个先到按哪个清理，当天不清）、`usage_hourly_days`、`usage_daily_days`（Token 用量，主库）、`stats_hourly_days`、`stats_daily_days`（服务质量、GPU 利用、卡时与调度活动，stats.db）、开关 `quality`、`gpu`、`gpu_time`（调度活动始终统计，没有开关）、统计时区 `time_zone`（IANA 名）。`limits` 是每个数值字段的 `{min, max}` | JWT |
| PUT | `/api/v1/data-retention` | 整体替换设置并立即生效（ux.db 的上限立即改，清理与按天汇总立即跑一轮）。body 必须给全 `value` 的每个字段、不能有多余字段；超出 `limits`、按天保留短于按小时、时区不存在时 400 `FIELD_INVALID`（`field`、`detail`）且不写入。改短的部分在后台分批删除。返回同 GET。`PUT /settings/data_retention` 回 400 `SETTING_HAS_OWN_ROUTE`，`GET /settings` 不含它 | admin |
| GET | `/api/v1/data-retention/status` | 各类数据现在的情况：`stores[]`（`kind` ∈ `metrics`/`logs`/`requests`/`usage`/`quality`/`gpu`/`gpu_time`/`scheduling`，`file` 所在文件（`main` 是主库），`file_bytes` 文件大小（主库为 `null`；`quality`/`gpu`/`gpu_time`/`scheduling` 同在 stats.db，报同一个数），`hourly_rows`/`daily_rows`（长期统计的行数，原始数据为 `null`），`earliest_ms` 最早还能查到的时间）、`live_since_ms`（开始实时累加的时间，之前的数据来自升级时的一次性补算）、`backfill`（`usage_done`/`usage_through_ms`/`quality_done`/`quality_through_ms`：补算进行到哪）、`retain`/`rollup`（最近一次清理、按天汇总的 `at_ms` 与 `error`）、`usage_flush`（最近一次 Token 用量写入的 `at_ms`、`rows`、`ms`、`error`） | JWT |

统计查询（JWT）。以下时间范围参数与响应中的 `range` 字段适用于 `usage`、`quality`、`gpus`、`gpu-time`。`key-usage` 固定统计今天与近 7 天，无需 `from`、`to`，也不返回 `range`。上述四个按时间范围查询的接口使用以下公共参数：`from`、`to`（Unix 毫秒，必填）、`granularity`（`hour` / `day`，不填时跨度不超过 2 天按小时、否则按天；按小时最多 7 天，按天最多 1825 天）。按天时范围扩展到统计时区的整天；已汇总的天读按天数据，没汇总的天（今天，或 server 停机时跨过的天）由它的小时数据现算。这四个接口的响应都带 `range`：`from_ms`、`to_ms`、`granularity`、`time_zone`、`earliest_ms`（这一粒度最早还有数据的桶）、`live_since_ms`（之前的桶来自升级时的一次性补算：抽样行按权重计，当时网关不识别 usage 的 `/v1/messages`、`/v1/responses` 请求没有 token）、`buckets`（各桶起点，`series` 与它一一对应）。参数不合法 400 `ANALYTICS_RANGE_INVALID` / `ANALYTICS_RANGE_TOO_LONG` / `ANALYTICS_VALUE_INVALID`（`field`、`detail`）。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/analytics/usage` | Token 用量。筛选：`deployment`（部署 id）、`key`（API key id，`none` 为没有 key 的请求）、`source`（`api` / `playground` / `batch` / `evaluation`，`batch` 是批处理任务发出的请求，`evaluation` 是从 server 发压的评测）。`totals` 与 `series[]` 是计数：`requests`、`no_usage`（没拿到 usage 的请求）、`prompt_tokens`（总输入）、`completion_tokens`、`reasoning_tokens`（`reasoning_reported` 个请求报了）、`cached_tokens`（`cache_reported` 个请求报了缓存命中，命中率 = `cached_tokens` ÷ `cache_reported_prompt`）；`rows[]` 每个「key × 用户 × 部署 × 来源」一行，带 `api_key_name`/`api_key_prefix`/`api_key_revoked`/`api_key_deleted`（吊销改为保留前就删掉的 key）、`user_name`、`deployment_name`/`served_name`/`deployment_deleted`、`source`、`first_ms`/`last_ms`（用到的第一个、最后一个桶） | JWT |
| GET | `/api/v1/analytics/quality` | 服务质量，`deployment` 可选。`totals`、`series[]`、`rows[]`（按部署）：各结果数（`completed`、`length`、`client_closed`、`interrupted`、`error`）、`ttft_p50_ms`/`ttft_p95_ms`（流式完整回复的首 token）、`e2e_p50_ms`/`e2e_p95_ms`（完整回复的端到端）、`output_tokens_per_sec`；`classes[]` 按请求优先级拆分总数（`priority_class` 加同样的数字，请求数多的在前，`priority_class` 为空的是启用优先级之前记录的请求，类未知）；分位数由四分之一倍频程分档还原，误差约 10%，没有数据为 `null` | JWT |
| GET | `/api/v1/analytics/gpus` | GPU 利用。`group_by` ∈ `model`（默认）/ `node` / `gpu`（表格与热力图的行），`metric` ∈ `util`（默认）/ `power` / `temp` / `mem`（热力图的值，桶内均值），筛选 `label=键=值`（可重复，同节点列表：按节点当前的书写标签与系统标签匹配，已删除的节点不再匹配）、`model`（卡型）。`card_count` 与 `by_model[]{model, cards}` 是筛选覆盖的当前卡数。覆盖不超过 256 张卡时 `detail: true`，`cards[]` 每张卡一行：`node_name`、`node_deleted`（节点已删除，`node_name` 是删除时的名字）、`gpu_index`、`gpu_uuid`、`gpu_model`、`present`（现在仍在库存中，节点离线也算）、`deployments`（现在占用它的部署）、`util_avg`/`util_max`、`mem_used_avg_bytes`/`mem_used_max_bytes`/`mem_total_bytes`、`power_avg_watts`、`temp_avg_celsius`/`temp_max_celsius`；超过时 `detail: false`、`cards` 为空。逐卡列出时也包括时间范围内删除的节点的卡。`groups[]`（按型号或节点时）每组一行：`key`、`label`、`node_deleted`（按节点时，已删除的节点；它的 `cards`、`nodes` 为 0）、`cards`、`nodes`、`models[]`、`deployments[]{name, cards}`、`unassigned`（无部署占用的卡数）与同上的统计值，取自按节点、按型号的统计。`heat[]`：`{key, label, node_deleted, values[]}`；按节点且超过 100 个节点时只含该指标最高与最低的各 50 个，`heat_total` 是全部行数。按卡分组而覆盖超过 256 张时 400 `ANALYTICS_TOO_MANY_CARDS` | JWT |
| GET | `/api/v1/analytics/gpu-time` | 卡时，`deployment` 可选。`rows[]` 每个「部署 × 卡型」一行：`gpu_hours`（dGPU 按显存份额折算）、`whole_gpu_hours`、`avg_instances`（到现在为止的平均副本数）、`max_instances`、该部署同一时段的 `prompt_tokens`/`completion_tokens`、`gpu_hours_per_million_output`/`gpu_hours_per_million_tokens`（没有 token 为 `null`）；`series[]` 每个部署各桶的卡时；`capacity[]` 每种卡型的 `total_gpu_hours`（在线节点上平台可用的卡）、`schedulable_gpu_hours`、`allocated_gpu_hours` | JWT |
| GET | `/api/v1/analytics/key-usage` | 每个有用量的 API key 今天与近 7 天（含今天）的计数：`{items:[{prefix, today, last_7_days}], time_zone}`，供 API 令牌页 | JWT |

## AI Helper（AI 助手）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/ai/configs` | AI 助手 LLM 接入配置列表。每行含 `proxy_mode`（`management` 管理节点出站代理 / `direct` 直连 / `custom` 自定义代理）、`proxy_url`（仅 `custom`，登录信息打码）与 `image_input`（模型是否接受图片），不含密钥 | JWT |
| GET | `/api/v1/ai/presets` | 预设字典：`presets[]{name, base_url, model, image_input}` 按控制台的顺序排列、以 `custom` 结尾，`proxy_modes` 是可选的代理方式。`image_input` 只是新建服务时开关的初值 | JWT |
| POST | `/api/v1/ai/configs` | 新增 LLM 接入配置。`base_url` 只填到 `/v1` 为止：末尾多余的 `/` 与误粘贴的 `/chat/completions` 会被裁掉后再存，非 http(s) 或没有主机名的地址 400 `FIELD_INVALID`（`params.field` 为 `base_url`）。不填 `base_url` 时若给了已知 `preset` 用预设地址，两者都没有也照存（AI 助手会报配置不可用）。`proxy_mode` 缺省为 `management`；`custom` 必须带 `proxy_url`（http、https、socks5 或 socks5h，可含登录信息，加密存储），其他方式带 `proxy_url` 400 `FIELD_INVALID`。不传 `image_input` 时取预设的值（模型也取自预设时），否则为 `false` | JWT |
| PATCH | `/api/v1/ai/configs/{id}` | 部分更新 LLM 接入配置：只改请求里出现的 `name`/`preset`/`base_url`/`model`/`is_default`/`proxy_mode`/`proxy_url`/`image_input`；`api_key` 不出现或为空字符串时保留已存的密钥（旧版明文存储的也原样保留），非空则重新加密存。`proxy_url` 同理：仍为 `custom` 时为空保留已存代理，改成其他方式时清掉已存代理。只改 `preset`（不带 `model`、`image_input`）时，图片开关随预设一起改。设为默认会同时清掉其他配置的默认。返回与列表同一行（不含密钥）。配置不存在 404，取值不合法 400 `FIELD_INVALID` | JWT |
| DELETE | `/api/v1/ai/configs/{id}` | 删除 LLM 接入配置 | JWT |
| POST | `/api/v1/ai/configs/test` | 用表单里尚未保存的 `base_url`/`api_key`/`model` 试连一次，不落库。10 秒超时，走与 AI 助手相同的出站策略。返回 `{outcome, status, detail, base_url, latency_ms}`，`outcome` 区分 `ok`、`connection`（DNS/连接/超时）、`blocked`（被出站策略拒绝）、`auth`（401/403 鉴权失败）、`path`（404 且未提到模型，地址不对）、`model`（服务端没有该模型）、`http`（其他状态或不是补全响应）。`detail` 是服务端的原话（截短、隐去密钥）。编辑弹窗带 `id` 且 `api_key` 为空时复用已存密钥，仅当 `base_url` 与已存地址一致。请求按 `proxy_mode`/`proxy_url` 走代理，`custom` 且 `proxy_url` 为空时用已存代理。带 `check_image: true` 且文字调用成功时再发一张小图，返回 `image`：`accepted`（回答认出了图片的颜色）、`ignored`（应答了但没有看到图片，如上游静默丢弃图片）、`rejected`（因图片被拒，400/415/422 且未提到模型）、`unknown`（无法判断），以及 `image_status`、`image_detail` | JWT |
| GET | `/api/v1/ai/available` | AI 助手是否可用（是否已配置 LLM）：`{available, image_input, images?}`。当前使用的 AI 服务开了「支持图片输入」时 `image_input` 为 `true`，并带 `images:{per_message, per_request, max_bytes, max_side}`（每条消息、每次请求的图片数上限，单张字节上限，控制台缩图的最长边） | JWT |
| POST | `/api/v1/ai/ask` | 让 AI 助手执行任务，支持普通响应和 SSE；完整字段与条件见[AI 提问的请求与响应](#ai-提问的请求与响应) | JWT |
| GET | `/api/v1/ai/instances/{uuid}/findings` | 失败实例的已知引擎故障：`{items:[{code, evidence:[{line}]}], total}`，`code` 为 `pkg/enginelog` 的 UPPER_SNAKE 失败码、文案由前端渲染。实例带结构化失败原因时取 agent 上报的 `causes`，不再匹配日志；老 agent 的实例才对其日志做匹配。不需要 LLM；实例不存在 404 | JWT |
| GET | `/api/v1/ai/models/{id}/fit` | 模型在在线集群各类加速卡上的物理适配，不代表当前空闲资源。返回 `weight_bytes`、`dtype` 与各卡型的 `rows`：`cards/unusable`（可用/故障卡数）、`dtype_supported`、`fast_link_cards`（NVLink 等高速互联组最大卡数）、`fits`、最小 `tp` 和余量 `headroom_bytes`；不需要 LLM，模型不存在 404。见[模型适配结果](#模型适配结果) | JWT |

### 模型适配结果

`GET /api/v1/ai/models/{id}/fit` 评估在线集群各卡型的物理能力，不代表当前空闲资源；不需要 LLM，模型不存在返回 404。
返回模型的 `model_id`、`weight_bytes`、`dtype`、`gpu_memory_utilization`，以及 `rows[]`：

- 卡型与容量：`vendor`、`name`、`memory_bytes`、`nodes`、`cards`（可用卡数）、`unusable`（按界面健康等级判定的故障卡数）、`max_cards_per_node`。
- 精度与互联：`dtype_supported` 表示该卡能否计算模型精度，`dtype_note` 为可选说明；`fast_link_cards` 是单节点内高速互联组的最大卡数，1 表示只有 PCIe、0 表示未知，每种卡型最多抽样 8 个节点。
- 放置估算：`fits`、`tp`、`per_gpu_bytes`、`usable_bytes`、`headroom_bytes`。
  `tp` 是单节点内放下权重与引擎预留所需的最小 2 的幂；放不下时 `fits:false`、`tp:0`。

### AI 提问的请求与响应

`POST /api/v1/ai/ask` 让 AI 助手执行一个任务。`task` 的取值与对象字段如下：

| `task` | 请求中指定的对象 |
|---|---|
| `instance.diagnose` | `instance_uuid` |
| `model.fit` | `model_id` |
| `deployment.unscheduled` | `deployment_id` |
| `node.health` | `node_uuid` |
| `ask` | 必须带 `question` 或 `images`，可选附带上述某一个对象 |

对象按引用指定，由 server 组装上下文；`question` 可选，但 `ask` 任务必须提供（或只发图片）。追问时带 `history:[{role:user|assistant,content,images?}]`，最多保留最近 10 轮。

`images` 是随问题发送的图片，每张为 `data:image/(png|jpeg|webp|gif);base64,…`；`history` 里的用户轮次可以带各自的 `images`，助手轮次不能带。图片按 OpenAI 的多段 `content`（文本加 `image_url`）发给 AI 服务。上限见 `GET /api/v1/ai/available`：每条消息 4 张、每次请求合计 8 张、单张解码后不超过 2 MiB，内容必须与声明的类型一致。只有当前 AI 服务开了「支持图片输入」才接受图片。

server 组装上下文后运行带只读工具的 agent 循环。工具以提问用户的身份在进程内读取本 API 的 GET 接口，见[使用 AI 助手](../how-to/use-ai-helper.md)。普通响应为 `{answer, steps:[{tool,args,ms,error?}], calls, usage:{prompt_tokens,completion_tokens}}`。

请求头带 `Accept: text/event-stream` 时改为 SSE 流式返回：

- `step`：`{id,tool,args,status:running|done|error,ms,error?}`，每次工具调用开始与结束各一条。
- `text` / `reasoning`：`{delta}`。
- 最后一条为 `done`（`{calls,usage,first_text_ms,total_ms}`）或 `error`（`{error, code?}`；带图片的提问被 AI 服务因图片拒绝时 `code` 为 `AI_IMAGE_REJECTED`）。
- 空闲时每 10s 发送一条 `: ping` 注释。

在调用模型之前就失败的情况，例如未配置 LLM 或对象不存在，仍以 HTTP 状态码返回。错误条件如下：

| 条件 | 响应 |
|---|---|
| 未配置 LLM | 503 `AI_ENDPOINT_NOT_CONFIGURED` |
| `task` 非法 | 400 `AI_TASK_INVALID` |
| 缺对象或同时给了多个对象 | 400 `AI_TARGET_REQUIRED` |
| 同一用户已有 2 个提问在处理 | 429 `AI_ASK_BUSY`，`params.limit` 给出限制 |
| 图片不可用：格式不对、内容与类型不符、过大、数量超限或放在助手轮次 | 400 `AI_IMAGE_INVALID`，`params.reason` 为 `format`/`too_large`/`too_many`/`role`，`params.where` 为 `question`/`history`/`request` |
| 请求体超过全部图片上限加文本 | 413 |
| 当前 AI 服务未开「支持图片输入」却带了图片 | 409 `AI_IMAGE_INPUT_OFF`，不会调用 AI 服务 |
| AI 服务因图片拒绝（400/415/422 且未提到模型） | 422 `AI_IMAGE_REJECTED` |
| 实例、模型、部署或节点不存在 | 404 |
| LLM 端点失败 | 502 |

### MCP

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| POST | `/api/v1/mcp` | MCP 服务（Streamable HTTP，只返回 JSON，不开 SSE；`GET` 回 405）。JSON-RPC 2.0，单条消息，支持 `initialize`、`ping`、`tools/list`、`tools/call` 与各类通知（回 202）。工具即 AI 助手的只读工具集，以调用者身份读本 API，同样脱敏、整形；工具失败以 `isError: true` 的结果返回，同一用户同时最多 4 个工具调用。服务关闭时回 403 `MCP_DISABLED`。接入方式见 [通过 MCP 接入](../how-to/use-mcp.md) | JWT 或平台令牌（`read` 即可） |
| GET | `/api/v1/mcp/status` | MCP 服务状态：`{enabled, path, protocol_version, tools}` | JWT |
| PUT | `/api/v1/mcp/settings` | 开关 MCP 服务：`{enabled}`，返回新的状态；默认关闭 | JWT(admin) |

未配置 LLM 时确定性估算器产出仍完整可用；`ai/ask` 仅在配置了 LLM 后可用；MCP 不需要 LLM。

## Playground

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/playground/models` | 可在 Playground 调用的已部署模型列表，不包含全局设置中的 AI 助手外部模型；每项模型有 `request_priority`（为 true 时请求可带 `X-GPUWorks-Priority-Class` 指定任意请求优先级类，不带时用默认类，响应头写明实际使用的类）和 `priority_scheduling`（部署是否开启优先级调度；关闭时类只记录、不影响排队）；`html_preview.allow_network` 是 HTML 预览的网络策略（来自安全设置）；每项模型有 `sleeping`（布尔），休眠中且请求能唤醒它（部署可休眠、有一个休眠实例不是运维或定时计划下的保持休眠）时另有 `wakes_on_request: true`（否则省略）：这样的模型收到请求会由网关挂起、唤醒后放行，没有它的休眠模型请求回 503，须先唤醒；试验场据此提示 | JWT |
| GET | `/api/v1/playground/openapi.json` | Playground 代理的 OpenAPI 描述 | JWT |
| ANY | `/api/v1/playground/v1/*` | Playground 内部代理（管理 JWT 保护，非 `/v1` 网关 key） | JWT |

Playground 与 `/v1` 网关的边界、外部模型命名 `external:<config-id>:<model>` 见
[`openai-gateway.md`](openai-gateway.md)。

## Batches（批处理任务）

批处理任务把一个 JSONL 文件里的请求异步交给网关执行，结果写成输出文件和错误文件。它有两组接口：

- 控制台用的管理接口 `/api/v1/batches`，鉴权同本文其他路由。
- OpenAI 兼容接口 `/v1/files` 与 `/v1/batches`，默认要求推理 API key（server 没有 key 时也照旧拒绝，除非管理员打开 `inference_open_access`），官方 SDK 可直接使用。

操作步骤、结果文件格式和重试规则见[运行批处理任务](../how-to/batch-api.md)。

### 控制台接口 `/api/v1/batches`

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/batches` | 批处理任务列表，最新优先，返回分页结构 `{items,page,page_size,total}`。`state` 为 8 种状态之一，多个值以逗号分隔；非法值返回 400 `LIST_FILTER_INVALID`；不支持 `q`，带 `q` 同样返回 400 `LIST_FILTER_INVALID`。管理员看到所有用户的任务，其他用户只看到自己的。列表项不含 `input_filename` | JWT |
| GET | `/api/v1/batches/dictionary` | 创建批处理任务可用的字典：`priority_class`（不选类的任务使用的请求优先级类，即 `batch`）、`endpoints`、`completion_window_hours`（`min`/`max`/`default`）、`max_file_bytes`、`max_file_lines`、`statuses`（列表筛选顺序）、`output_retention_days`，以及 `models`：未在删除的部署（含已停止的）的对外模型名、状态和支持的端点 `{name, state, ready, endpoints}`。`ready` 由服务端计算，仅运行或请求可自动唤醒时为 true；控制台默认显示这些模型，用户显式开启等待选项后可选择其他状态。`state` 与部署列表一致，按能否尽快服务依次为 `running`（有副本醒着）、`asleep`（休眠，请求会唤醒）、`starting`（放置、拉取或启动中）、`asleep_held`（手动或定时休眠，请求不会唤醒）、`offline`（副本所在节点断开）、`failed`（启动失败）、`stopped`（已停止），列表按这个顺序再按名称排；`endpoints` 是模型推理接口声明里开放的批处理端点（网关按同一份声明放行请求）。与创建时的模型校验同一判定，同一个名字有多个部署时状态取其中最好的、端点取并集。控制台创建对话框的选项都来自这里，客户端不自行推断 | JWT |
| GET | `/api/v1/batches/{id}` | 批处理任务详情，含 `input_filename`；不存在或不是自己的任务（管理员除外）返回 404 `BATCH_NOT_FOUND` | JWT |
| GET | `/api/v1/batches/{id}/files/{kind}` | 下载任务的文件，`kind` 为 `input`、`output`、`error`，响应是 JSON Lines（`application/x-ndjson`，带附件文件名）；`kind` 非法返回 400 `BATCH_REQUEST_INVALID`，文件不存在或已删除返回 404 `BATCH_NOT_FOUND` | JWT |
| POST | `/api/v1/batches` | 创建批处理任务：`multipart/form-data`，字段 `file`（JSONL），可选 `endpoint`（不传时取文件各行共同的 `url`；各行 `url` 不同则返回 400 `BATCH_FILE_INVALID`，`reason` 为 `mixed_endpoints`，`line` 是第一处与首行不同的行）、`completion_window`（`1h` 到 `72h` 的整小时，默认 `24h`，不合法返回 400 `BATCH_REQUEST_INVALID`，`params.field` 为 `completion_window`）和 `model`（指定后每个请求的 `body.model` 都改成它，必须是某个未在删除的部署的对外模型名，否则 400 `BATCH_MODEL_UNKNOWN`；它的部署不支持任务的端点时 400 `BATCH_MODEL_ENDPOINT`）和 `priority_class`（任务的请求使用的请求优先级类，不区分大小写，不传时用字典的 `priority_class`；不存在返回 400 `PRIORITY_CLASS_NOT_FOUND`）；上传时逐行校验，成功返回 201 和任务对象，所选模型（未指定 `model` 时是文件里各行的模型）需要有人处理时响应带 `warnings`：手动休眠（请求不会唤醒）是 `BATCH_MODEL_SLEEP_HELD` 或 `BATCH_FILE_MODELS_SLEEP_HELD`，节点离线、启动失败、已停止是 `BATCH_MODEL_WAITS` 或 `BATCH_FILE_MODELS_WAIT`。未指定 `model` 时，文件里有模型没有部署使用，返回 400 `BATCH_FILE_MODELS_UNKNOWN`（`params` 为 `{models, more}`），有模型不支持任务的端点，返回 400 `BATCH_FILE_MODELS_ENDPOINT`（`params` 为 `{models, more, endpoint}`），都不创建任务。任务属于当前登录用户 | JWT(admin) |
| POST | `/api/v1/batches/{id}/cancel` | 取消任务，返回带 `cancelling` 状态的任务对象；只有 `validating` 和 `in_progress` 的任务可以取消（对象的 `cancellable` 为 `true`），已结束的或正在 `finalizing`（写结果文件）的任务返回 409 `BATCH_NOT_CANCELLABLE` | JWT(admin) |

读取（GET）使用只读平台令牌即可；写入需要管理员角色（否则 403 `ADMINISTRATOR_REQUIRED`）和读写范围的平台令牌（否则 403 `PLATFORM_TOKEN_READ_ONLY`）。

任务对象（RFC3339 时间；到达之前的时间字段不出现，`output_file_id`、`error_file_id`、`failure` 同理）：

| 字段 | 含义 |
|---|---|
| `id`、`endpoint`、`status` | 任务 ID、接口（`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings` 之一）、状态 |
| `status` | `validating`、`in_progress`、`finalizing`、`completed`、`failed`、`expired`、`cancelling`、`cancelled` |
| `cancellable` | 取消接口此刻会不会接受：`validating` 或 `in_progress` 时为 `true`，由 server 判定，客户端据此显示取消按钮，不自己按状态推断 |
| `input_file_id`、`input_filename` | 输入文件 ID；文件名只在详情、创建和取消的响应里出现 |
| `output_file_id`、`error_file_id` | 输出、错误文件 ID，文件存在后才有 |
| `completion_window` | 创建时选的完成时限，`1h` 到 `72h` 的整小时，默认 `24h` |
| `model` | 创建时指定的模型：每个请求发往它，行里自己的 `body.model` 被替换；没有指定时不出现，各行用自己的模型 |
| `request_counts` | `{total, completed, failed}`，约每 5 秒更新一次；重试中的请求两者都不计 |
| `metadata` | 字符串到字符串的对象，没有时为 `{}` |
| `failure` | `{code, message}`，仅 `failed` 状态有 |
| `output_expires_at` | 输出文件和错误文件被删除的时间（任务结束时间加保留期，默认 30 天，保留天数见字典的 `output_retention_days`）；任务还在运行或没有写出这两个文件时不出现，早于现在表示文件已删除 |
| `warnings` | 创建时不阻止任务的提示，只在创建的响应里出现，形如 `[{"code":"BATCH_MODEL_SLEEP_HELD","params":{"model":"qwen3-offline"}}]`：指定的模型的部署被人工或定时计划休眠，请求不会唤醒它；没有指定模型时，文件里有这样的模型则是 `BATCH_FILE_MODELS_SLEEP_HELD`，`params` 为 `{models, more}`。模型节点离线、启动失败或已停止时是 `BATCH_MODEL_WAITS`（`params` 为 `{model, state}`）或 `BATCH_FILE_MODELS_WAIT`（`{models, more, state}`），`state` 取字典里模型的状态值，请求一直重试到模型恢复服务或任务过期 |
| `waiting` | 只在单个任务（`GET`）上、且有请求在等重试时出现：每个模型一项 `{model, state, status, detail, since, requests}`，`state` 是模型此刻的状态（取值同字典，没有部署了则省略），`status`、`detail` 是网关最近一次应答的 HTTP 状态和原文（没收到应答时 `status` 为 0），`since` 是这些请求开始等的时间，`requests` 是此刻在等的请求数。只在执行器内存里，server 重启后随下一次重试重新出现 |
| `user_id`、`api_key_id` | 创建者；从控制台创建时没有 `api_key_id` |
| `created_at`、`expires_at` | 创建时间与过期时间（创建时间加完成时限） |
| `in_progress_at`、`finalizing_at`、`completed_at`、`failed_at`、`expired_at`、`cancelling_at`、`cancelled_at` | 各阶段到达的时间 |

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","endpoint":"/v1/chat/completions","status":"in_progress","input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","input_filename":"requests.jsonl","completion_window":"24h","request_counts":{"total":1000,"completed":410,"failed":3},"metadata":{},"user_id":3,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z","in_progress_at":"2026-10-28T09:00:02Z"}
```

产品错误码：

| 错误码 | 状态 | 含义 |
|---|---|---|
| `BATCH_NOT_FOUND` | 404 | 任务或文件不存在，或不属于调用者 |
| `BATCH_NOT_CANCELLABLE` | 409 | 任务已结束，或正在 `finalizing` 写结果文件，不再接受取消 |
| `BATCH_FILE_INVALID` | 400 | 输入文件无效；`params` 为 `{line, reason, detail}`，`line` 为 0 表示整个文件，`reason` 是下文的校验错误码 |
| `BATCH_FILE_TOO_LARGE` | 413 | 文件超过上限；`params` 为 `{limit_mb}` |
| `BATCH_MODEL_UNKNOWN` | 400 | 创建时指定的 `model` 没有部署使用，`params` 为 `{model}`。删除中、已删除的部署不算 |
| `BATCH_MODEL_ENDPOINT` | 400 | 创建时指定的 `model` 的部署不支持任务的端点，`params` 为 `{model, endpoint}` |
| `BATCH_FILE_MODELS_UNKNOWN` | 400 | 没有指定 `model` 时，输入文件各行的模型里有的没有部署使用；`params` 为 `{models, more}`，`models` 是前 5 个模型名的列表，`more` 是还有几个（查找在发现几十个未知模型后停止，可能是下限），名字由客户端按界面语言拼接。对应的警告码是 `BATCH_FILE_MODELS_SLEEP_HELD`，`params` 同形 |
| `BATCH_FILE_MODELS_ENDPOINT` | 400 | 没有指定 `model` 时，输入文件各行的模型里有的部署不支持任务的端点；`params` 为 `{models, more, endpoint}` |
| `BATCH_REQUEST_INVALID` | 400 | 请求的某个字段无效；`params` 为 `{field, reason}`，例如不是 multipart、缺少 `file`、`endpoint` 不被支持、传了的 `endpoint` 与文件内的 `url` 不一致 |

文件保存在 server 数据目录的 `batch-files/` 下。

### OpenAI 兼容的 Files 与 Batches 接口

这些路由与 `/v1/chat/completions` 等网关接口一起挂在 `/v1` 下，默认用推理 API key（`Authorization: Bearer <key>`）鉴权，server 一个推理令牌都没有时也一样；只有管理员在安全设置里打开 `inference_open_access` 后才接受不带令牌的请求（见[OpenAI 兼容网关的认证](openai-gateway.md#认证)）。鉴权失败返回 401，错误码 `invalid_api_key`。文件和任务属于创建它们的 API key 所属用户：同一用户的任意 key 都能访问，其他用户得到 404。

| Method | Path | 说明 |
|---|---|---|
| POST | `/v1/files` | 上传输入文件，`multipart/form-data`，字段 `file` 和 `purpose`（只能是 `batch`），成功返回 200 和文件对象 |
| GET | `/v1/files` | 文件列表；`purpose` 为 `batch` 或 `batch_output`，`limit` 默认且最大 10000，`after` 为上一页最后一个文件 ID，只支持 `order=desc`（最新优先） |
| GET | `/v1/files/{id}` | 文件对象 |
| GET | `/v1/files/{id}/content` | 文件内容，`application/jsonl` |
| DELETE | `/v1/files/{id}` | 删除文件，返回 `{"id","object":"file","deleted":true}`；仍有未结束的任务在读它时返回 409 `file_in_use` |
| POST | `/v1/batches` | 创建任务，成功返回 200 和任务对象 |
| GET | `/v1/batches` | 任务列表，最新优先；`limit` 默认 20、最大 100，`after` 为上一页最后一个任务 ID |
| GET | `/v1/batches/{id}` | 任务对象 |
| POST | `/v1/batches/{id}/cancel` | 取消任务，返回状态为 `cancelling` 的任务对象；已结束的任务返回 409 `batch_not_cancellable` |

列表响应是 `{"object":"list","data":[...],"first_id","last_id","has_more"}`；`data` 为空时 `first_id`、`last_id` 为 `null`。

文件对象：

```json
{"id":"file-3f9c2a7e1b6d4c8a90e5d721","object":"file","bytes":412,"created_at":1790000000,"filename":"requests.jsonl","purpose":"batch","status":"processed"}
```

`id` 是 `file-` 加 24 位十六进制；`purpose` 为 `batch`（输入）或 `batch_output`（输出文件与错误文件）；`created_at` 是 Unix 秒。

创建任务的请求体：

| 字段 | 要求 |
|---|---|
| `input_file_id` | 必填。`purpose` 为 `batch`、调用者可见的文件，否则 400 `file_not_found`（`param` 为 `input_file_id`） |
| `endpoint` | 必填，`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings` 之一，且等于文件内每一行的 `url`，否则 400 `endpoint_mismatch` |
| `completion_window` | 必填，`"24h"`（OpenAI 的取值）或 `"1h"` 到 `"72h"` 的整小时（gpuworks 扩展，不带前导零），否则 400 `invalid_completion_window`（`param` 为 `completion_window`）。`expires_at` 是 `created_at` 加这段时间 |
| `metadata` | 可选，字符串到字符串的对象，最多 16 个键，键 1–64 个字符，值至多 512 个字符，否则 400 `invalid_metadata` |
| `model` | gpuworks 扩展，可选。对外模型名，每个请求发往它，行里自己的 `body.model` 被替换；省略则各行用自己的模型。必须是某个未在删除的部署（含已停止的）的对外模型名，否则 400 `model_not_found`（`param` 为 `model`）；它的部署不支持任务的端点时 400 `model_endpoint_unsupported`。该部署被人工或定时计划休眠（请求不会唤醒）、节点离线、启动失败或已停止时仍然接受，创建的响应带 `warnings`，请求一直重试到模型恢复服务或任务过期。省略 `model` 时，平台在创建时读一遍输入文件，其中有模型没有部署使用（删除中或已删除的不算），返回 400 `model_not_found`，`param` 为 `input_file_id`，不创建任务，文件保留可重用；错误体如下，模型名最多列 5 个，更多以 `, ...` 结尾：`{"error":{"message":"The input file names models that no deployment serves: A, B. Set a model to send every request to, or deploy these models first","type":"invalid_request_error","param":"input_file_id","code":"model_not_found"}}`；有模型不支持任务的端点时同样 400，`code` 为 `model_endpoint_unsupported`。指定了 `model` 时不检查文件里的模型（未知的 `model` 仍是 400 `model_not_found`，`param` 为 `model`）；创建之后才删除的模型在运行时逐个请求失败，才停止的模型的请求等它重新启动 |

任务对象的时间都是 Unix 秒，还没到达的为 `null`：

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","object":"batch","endpoint":"/v1/chat/completions","errors":null,"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","completion_window":"24h","status":"in_progress","output_file_id":null,"error_file_id":null,"created_at":1790000010,"in_progress_at":1790000012,"expires_at":1790086410,"finalizing_at":null,"completed_at":null,"failed_at":null,"expired_at":null,"cancelling_at":null,"cancelled_at":null,"request_counts":{"total":1000,"completed":410,"failed":3},"metadata":null}
```

- 指定了 `model` 的任务对象多一个 `model` 字段（没指定时不出现）；创建的响应在需要时多一个 `warnings` 数组 `[{"code":"model_sleep_held","message":"…","param":"model"}]`（没有指定 `model`、文件里的模型被保持休眠时，`param` 为 `input_file_id`，消息列出这些模型；节点离线、启动失败或已停止的模型是 `model_waits`，每种状态一条），其他读取不带。这两个字段是 gpuworks 扩展，官方 SDK 会忽略它们。
- `id` 是 `batch_` 加 24 位十六进制；没有 `metadata` 时为 `null`；`errors` 平时为 `null`，`failed` 状态下是 `{"object":"list","data":[{"code","message","param":null,"line":null}]}`。
- `status` 取值：`validating`（排队等执行名额，全 server 同时最多执行 32 个任务）、`in_progress`、`finalizing`、`completed`、`failed`（只因平台故障，例如输入文件消失）、`expired`（超过完成时限，没有结果的请求写入错误文件，错误码 `batch_expired`）、`cancelling`、`cancelled`。
- 取消：不再发送新请求，在途请求最多再等 2 分钟，之后任务以 `cancelled` 结束，已有结果留在输出文件和错误文件里。

**输入文件校验**：上传时边收边校验，第一处错误即拒绝，错误带 `line`，`message` 以 `Line N: ` 开头，被拒绝的上传不留下任何文件。

| 项目 | 限制 |
|---|---|
| 文件大小 | 200 MB，超过返回 413 `file_too_large` |
| 请求数 | 非空行最多 50000，超过返回 400 `too_many_requests` |
| 单行长度 | 16 MiB，超过返回 400 `line_too_long` |
| 空文件 | 400 `empty_file` |

每个非空行必须是 JSON 对象，校验失败的错误码如下：

| 错误码 | 含义 |
|---|---|
| `invalid_json_line` | 该行不是 JSON 对象 |
| `missing_custom_id` | `custom_id` 缺失或不是非空字符串 |
| `duplicate_custom_id` | `custom_id` 在文件内重复 |
| `invalid_method` | `method` 不是 `POST` |
| `invalid_url` | `url` 不是三个受支持的接口之一 |
| `missing_body` | `body` 缺失或不是对象 |
| `missing_model` | `body.model` 缺失或不是非空字符串 |
| `streaming_not_supported` | `body.stream` 为 `true` |

上传请求本身的错误：`invalid_content_type`、`invalid_multipart`、`invalid_purpose`（`purpose` 不是 `batch`）、`missing_required_parameter`（缺 `file` 或 `purpose`），都是 400。

**结果文件**：任务结束后，`output_file_id` 指向成功请求的输出文件，`error_file_id` 指向最终失败请求的错误文件，都是 `purpose` 为 `batch_output` 的 JSON Lines，每个 `custom_id` 一行、按完成顺序排列。输出文件的行是 `{"id":"batch_req_<hex>","custom_id","response":{"status_code":200,"request_id","body":{...}},"error":null}`，错误文件的行是 `{"id","custom_id","response":null|{"status_code","request_id","body"},"error":{"code","message"}}`。输出文件只在至少一个请求成功时存在，错误文件只在至少一个请求失败时存在。两者在任务结束 30 天后删除，之后读取返回 404 `file_not_found`；任务保留计数。输入文件保留到用户删除。

**哪些请求会重试**：应答为 HTTP 200 但某个 choice 的 `finish_reason` 为 `abort`、503、429、408、其他 5xx 或传输错误时，请求以指数退避（2 秒起翻倍，60 秒封顶，遵守 `Retry-After`，带抖动）重试到任务过期，不计入失败；其他 4xx 是最终失败。

**错误格式**：这些路由按 OpenAI 的格式返回错误，文件校验错误另带 `line`：

```json
{"error":{"message":"Line 2: custom_id must be a non-empty string","type":"invalid_request_error","param":null,"code":"missing_custom_id","line":2}}
```

| 状态 | `code` | 场景 |
|---|---|---|
| 400 | `invalid_json`、`invalid_limit`、`invalid_order`、`invalid_endpoint`、`invalid_completion_window`、`invalid_metadata`、`endpoint_mismatch`、`file_not_found`、`invalid_purpose`、`model_not_found`、`model_endpoint_unsupported` | 创建任务或列表参数不合法；输入文件校验的错误码见上 |
| 401 | `invalid_api_key` | 缺少或无效的 API key；`/v1` 默认要求 key，不带 key 的请求只有在管理员打开 `inference_open_access` 后才放行 |
| 404 | `file_not_found`、`batch_not_found`、`not_found` | 对象不存在或不属于调用者；未知路径 |
| 405 | `method_not_allowed` | 方法不被该路径支持 |
| 409 | `file_in_use`、`batch_not_cancellable` | 删除正被未结束任务读取的文件；取消已结束的任务 |
| 413 | `file_too_large` | 上传超过 200 MB |

**执行方式与限制**：任务的请求走与客户端请求相同的网关路径（按 `body.model` 路由、休眠部署的按需唤醒、用量计数、请求记录归到创建任务的 API key 和用户）。每个任务同时 8 个请求，所有任务合计 64 个在途请求；进度约每 5 秒保存一次，server 重启后未结束的任务从最近的检查点继续，不重发已有结果的请求。

## 未覆盖内容

- 数据面 `/v1/*`（chat/embeddings/rerank/audio/image/video/realtime 等 OpenAI 兼容 endpoint）见
  [`openai-gateway.md`](openai-gateway.md)，不在本文重复；其中 `/v1/files` 与 `/v1/batches` 见 [Batches](#batches批处理任务)。
- agent 侧 yamux 子协议（`ctrl`/`telemetry`/`logtail`/`tunnel`/`model.read`/`model.write`/
  `image.read`/`image.write`/`terminal` 等，完整列表与所属通道见 `proto.LaneFor`）不是 HTTP 路由，属于 `internal/proto` 控制面协议，不在本文范围。

## 路由与契约维护

以下约定用于维护接口清单，不是调用 API 的前置条件：

- 管理路由由 `internal/server/api.Router` 注册，使用 chi router。
  `internal/server/api/router_inventory_test.go` 中的 `TestRouterInventoryMatchesCompleteContract` 逐条断言实际路由树，
  新增或删除路由时必须同步更新该测试。
- 管理鉴权由 `internal/server/api/auth.go` 的 `managementAuth` 校验，管理员操作再经过 `managementAdmin`。
  产品错误使用 `messages.ProductError`；130+ 个稳定错误码定义在 `internal/server/api/errors*.go`，本文不逐一罗列。
- `make check-api-conventions` 从类型上检查字段命名和列表结构，详见 [API 规范](api-conventions.md)。
- `internal/server/app` 在生产装配时始终为 `/v1/*`、`/v1/models`、`/api/v1/playground/v1/*` 注入非空
  `ProxyV1`、`ProxyModels`、`PlaygroundV1` handler。路由清单测试使用零值 `Deps{}`，因此这三个网关路由
  和 SPA fallback `/*` 不在其断言列表内，但生产环境始终挂载。

### 控制台清单与扩展路由

另行构建的服务端可以在 `/api/v1/x/<名称>/` 下挂载自己的管理路由（`api.Deps.Extensions`）。社区版不挂载任何扩展路由，路由清单与没有扩展时完全相同。

- **命名空间与冲突**：扩展名须符合 `[a-z][a-z0-9_]{1,31}`；路由（相对 `/api/v1`）须为 `/x/<名称>` 或以 `/x/<名称>/` 开头。核心不在 `/api/v1/x/` 下注册任何路由，所以扩展名可以与核心路径段同名（如 `nodes`），`/api/v1/nodes` 仍是核心路由。命名空间越界、同一方法与路径重复注册，`Router` 都会在启动时直接 panic。
- **鉴权**：扩展路由与核心路由走同一套管理鉴权（含会话的来源检查与续期）。按声明分三档：`read`（任何已登录用户与平台令牌，读令牌只能 GET/HEAD/OPTIONS）、`admin`（另需管理员，否则 `403 ADMINISTRATOR_REQUIRED`）、`admin-session`（管理员且只接受登录会话，平台令牌返回 `403 PLATFORM_TOKEN_NOT_ALLOWED`）。
- **功能可用性**：每条扩展路由要么属于一个功能（feature），要么明确标为社区路由，二者必居其一。功能的可用性为 `full`、`read_only`、`locked` 或 `hidden`，在 server 端执行：`read_only` 时只放行读请求（以及声明为只读时可写的路由），写请求返回 `403 FEATURE_READ_ONLY`；`locked` 时返回 `403 FEATURE_UNAVAILABLE`；`hidden` 时返回 `404 FEATURE_UNAVAILABLE`；没有组件回答可用性时按不可用处理，返回 `FEATURE_PROVIDER_ABSENT`。错误的 `params.feature` 是功能名。
- **整台服务端的可用性**：另行构建的服务端可以把整台服务端设为 `read_only` 或 `locked`。`read_only` 时读请求、`/v1/*` 和 `/api/v1/playground/v1/*` 照常，改动返回 `403 PLATFORM_READ_ONLY`；`locked` 时读请求照常，登录、健康检查、控制台清单、已接入机器的连接、插件包管理、服务端升级、诊断包以及设置该限制的扩展自己的路由也可用，改动和 `/v1/*` 返回 `403 PLATFORM_LOCKED`。`/api/v1` 下的请求先鉴权再拒绝，未登录的请求得到的仍是 401；`/v1/*` 的拒绝是网关的错误形状，码相同。安装和升级插件包按改动拒绝。受限时 `GET /api/v1/console/manifest` 带 `platform`（`access`、`message`，以及只给管理员的 `link`）；社区版从不受限，清单里没有这个字段。
- **AI 助手与 MCP**：扩展的 GET 路由默认不对 AI 助手和 MCP 开放，只有显式标记的才会出现在可读路由列表里并能被读取。
- **路由清单**：`api.RouteInventory` 每行一条路由；扩展路由附带 `[access=…] [action=…] [feature=…]`（社区路由为 `[community]`），既不属于功能也未标为社区路由的扩展路由会使清单失败。

`GET /api/v1/console/manifest` 的字段：

| 字段 | 说明 |
|---|---|
| `edition` | 构建名称，仅用于展示 |
| `features[]` | 已声明功能（编译模块声明的与当前已安装插件包声明的合在一起，安装或卸载插件后立即变化）的当前可用性，按 `key` 排序：`{key, access, presentation, link?, message?, limits}`。`access` 为 `full` / `read_only` / `locked` / `hidden`；`presentation` 为 `promote`（不可用时显示为锁定并给出 `link`）或 `hidden`（不可用时完全隐藏，此时锁定按 `hidden` 返回）；`message` 是说明当前可用性的产品消息 `{code, params?, message}`；`limits` 为 `[{dim, max}]`，`max` 为 `null` 表示不限 |
| `nav[]` | 扩展加入控制台菜单的入口 `{id, group, label_key, icon?, route, feature?}`；所属功能为 `hidden` 的入口不返回 |
| `banners[]` | 显示在每个页面顶部的通知 `{id, severity, message, link?}`，`severity` 为 `info` / `warning` / `danger`（其他值按 `info` 返回），`message` 为产品消息 `{code, params?, message}` |

### GPU 容量是否已知

节点 GPU 与 `/api/v1/gpus` 的物理卡 DTO 均返回 `memory_capacity_known`（布尔）。它从库存容量派生，不持久化；为 false 时，`memory_total_bytes` 不应渲染成已知的 0 GB。真实测得的零占用仍是有效指标。旧 API 缺少该字段时，客户端兼容层才使用历史的容量零值约定。

自定义命令的功能无法确认且目标声明了启动功能限制时，评估/创建返回 `LAUNCH_FEATURES_UNKNOWN`（参数 `vendor`），调度拒绝使用 `launch_features_unknown`。提供可识别的引擎入口与结构化参数后重新评估。
