# HTTP API 规范

`/api/v1` 下的接口都遵守本文的约定。新增或修改接口时照此执行，`make check-api-conventions`
（`scripts/apiconv`）会对能从类型上读出来的部分做检查。逐个接口的说明见 [api.md](api.md)。

## 示例：登录并查询推理服务列表

GPUWorks 把推理服务的配置和运行实例归在“部署”下。查询部署列表使用 `GET /api/v1/deployments`，
返回结果也包括准备中、已停止或失败的部署；它与供推理客户端选择模型的 [`GET /v1/models`](openai-gateway.md) 不同。

以下示例在能访问控制台的机器上执行，需要 **Bash、curl 7.76 或更新版本、jq**，以及可登录控制台的账户。
命令使用登录返回的 JWT（管理会话令牌），不使用推理令牌（例如 `gw-…`）。

### 1. 设置地址并登录

将示例地址替换为实际**控制台地址**，保留协议和端口，去掉 `/dashboard` 等页面路径以及末尾 `/`。
默认安装端口为 `8800`；使用自定义监听地址或反向代理时，以浏览器访问控制台的地址为准。

```bash
API_URL='http://192.0.2.10:8800'
USERNAME='admin'
read -r -s -p 'GPUWorks 密码：' PASSWORD
printf '\n'

TOKEN=''
LOGIN_RESPONSE=$(jq -n --arg username "$USERNAME" --arg password "$PASSWORD" \
  '{username: $username, password: $password}' |
  curl --fail-with-body -sS "$API_URL/api/v1/auth/login" \
    -H 'Content-Type: application/json' --data-binary @-) &&
TOKEN=$(printf '%s' "$LOGIN_RESPONSE" | jq -er '.token')
unset PASSWORD
```

`USERNAME` 填控制台用户名。登录成功返回 `token` 和 `user`，命令将 `token` 存入变量供后续请求使用。
登录失败时，执行 `printf '%s\n' "$LOGIN_RESPONSE"` 查看 `error`、`code`（见[错误](#错误)），处理后再查询。

### 2. 查询部署列表

登录成功后，在同一终端中执行：

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/deployments?page=1&page_size=20" \
  -H "Authorization: Bearer $TOKEN"
```

成功响应为 HTTP `200`。下面是关键字段示意，实际响应还包含模型、节点、健康状态等字段：

```json
{
  "items": [
    {
      "id": 7,
      "name": "qwen3-chat",
      "served_name": "qwen3",
      "desired_state": "active",
      "runtime_state": "running",
      "replicas": 1,
      "ready_replicas": 1
    }
  ],
  "page": 1,
  "page_size": 20,
  "total": 1
}
```

| 字段 | 如何读取 |
|---|---|
| `items` | 本页部署记录；`[]` 表示当前页没有记录 |
| `page` / `page_size` / `total` | 当前页、每页条数、匹配的部署总数。`page × page_size < total` 时还有下一页 |
| `id` | 部署 ID，用于查询 `GET /api/v1/deployments/{id}` 等管理接口 |
| `name` / `served_name` | `name` 是管理界面显示名；`served_name` 是发起推理请求时填写的 `model` 名称 |
| `desired_state` / `runtime_state` | 分别是期望状态和当前运行状态；判断能否服务时还要结合就绪副本，不能仅凭 `active` |
| `replicas` / `ready_replicas` | 部署的副本数和就绪副本数。预填充/解码分离（PD）部署按各角色的 unit 汇总，与列表行同名同值；详情里另有 `pd.roles` 给出按角色拆开的同一组数；不能把缺省当作 `0` |

`page` 从 `1` 开始，`page_size` 为 `1`–`200`。不带分页参数时返回全部匹配部署，单条记录也比分页列表更完整。
空列表同样返回 HTTP `200`；请求失败时返回错误对象，不能把它当作空列表处理。

### 3. 查询一个部署的详情

将 `7` 替换为上一步 `items` 中实际的 `id`：

```bash
curl --fail-with-body -sS "$API_URL/api/v1/deployments/7" \
  -H "Authorization: Bearer $TOKEN"
```

详情响应直接是部署对象，不套在 `items` 中，包含该部署的配置和实例信息。需要调用模型时，使用
`served_name` 并按 [OpenAI 兼容网关](openai-gateway.md) 的鉴权与请求格式调用。

## 字段命名

- JSON 字段名一律 snake_case：`node_uuid`、`memory_total_bytes`、`time_ms`。Go 结构体字段必须带
  `json` tag，不能依赖字段名的默认编码（那样会得到 `TimeMS`、`Entries`）。
- map 的键如果是数据（标签名、环境变量名、指标 label），不受此限。
- 带单位的数值把单位写进字段名：`_bytes`、`_gib`、`_ms`、`_seconds`、`_pct`。
- 时间点用 `_at`（RFC 3339 字符串）或 `_ms`（Unix 毫秒）结尾。同一个接口内不混用两种写法表示同一类时间。

## 集合

返回一组同类对象的接口，响应是一个对象，元素放在 `items` 里，**不返回裸数组**：

```json
{"items": [{"uuid": "…", "name": "gpu3"}], "total": 1}
```

- `total` 是命中总数。不分页时等于 `items` 的长度；分页时是筛选后的全部命中数，不只是这一页。
- 分页（`page`/`page_size`）另带 `page`、`page_size`，对应 `dto.ListPage`。不分页对应 `dto.List`。
- 游标分页（事件时间线、日志检索）不一定能给出 `total`，可以只给 `items`，以及翻页所需的游标字段或说明。
- 和整个集合相关、但不属于某个元素的信息（汇总、筛选选项）作为 `items` 的兄弟字段，例如
  `GET /api/v1/gpus` 的 `overview`、`GET /api/v1/nodes?page=1` 的 `labels`。
- 空集合是 `"items": []`，不是 `null`，也不省略。
- 筛选与排序参数不论是否分页都生效，**不得被静默忽略**：接口不支持的筛选值回 400（`LIST_FILTER_INVALID`），
  而不是返回未筛选的全集。
- 判断标准是「这个接口返回的主体是不是一组同类对象」。对象自身属性里的数组（一条曲线的 `points`、
  一个节点的 `gpus`）是对象的一部分，不受此限。

## 不透明字段

响应中类型为 `any`、`map[string]any` 或 `json.RawMessage` 的字段，检查脚本看不到里面的字段名。
这类字段只用于原样透出的整体快照（例如部署的 `catalog_selection_snapshot`），并且快照本身也要遵守 snake_case。
新接口优先定义具名 DTO。现有数量由 `scripts/arch-constraints.txt` 的 `api_opaque_response_fields` 预算约束，只降不升。

## 错误

管理 API 的每个错误响应都是 JSON 对象，包含 `error` 与稳定的 `code`，以及可选的 `params`。
例如登录时用户名或密码错误，返回 HTTP `401`：

```json
{"error": "invalid username or password", "code": "INVALID_CREDENTIALS"}
```

| 字段 | 用途 |
|---|---|
| `error` | 可直接查看的英文错误说明；不应靠匹配这段文字编写处理分支 |
| `code` | 稳定的错误标识，供程序区分原因；每个错误都有 |
| `params` | 错误涉及的字段、取值等上下文；客户端可与 `code` 一起用于本地化提示，未提供时省略 |

服务端没有为某个错误给出具体原因时，按 HTTP 状态填一个**通用码**：

| HTTP 状态 | 通用 `code` | HTTP 状态 | 通用 `code` |
|---|---|---|---|
| `400` | `BAD_REQUEST` | `422` | `UNPROCESSABLE` |
| `401` | `UNAUTHORIZED` | `424` | `FAILED_DEPENDENCY` |
| `403` | `FORBIDDEN` | `429` | `TOO_MANY_REQUESTS` |
| `404` | `NOT_FOUND` | `500` | `INTERNAL_ERROR` |
| `405` | `METHOD_NOT_ALLOWED` | `501` | `NOT_IMPLEMENTED` |
| `408` | `REQUEST_TIMEOUT` | `502` | `BAD_GATEWAY` |
| `409` | `CONFLICT` | `503` | `SERVICE_UNAVAILABLE` |
| `410` | `GONE` | `504` | `GATEWAY_TIMEOUT` |
| `413` | `REQUEST_TOO_LARGE` | `507` | `INSUFFICIENT_STORAGE` |
| `415` | `UNSUPPORTED_MEDIA_TYPE` | `416` | `RANGE_NOT_SATISFIABLE` |

表外的 4xx 用 `BAD_REQUEST`，其余用 `INTERNAL_ERROR`。通用码只说明类别，具体情况在 `error` 里，例如：

```json
{"error": "deployment 7 not found", "code": "NOT_FOUND"}
```

控制台把通用码显示为本地化的类别加上 `error` 原文（如「未找到：deployment 7 not found」），具体码按各自的文案显示。

### 常见 HTTP 状态与错误码

以下列出管理 API 中常见的情况。HTTP 状态表示错误类别，`code` 表示具体原因；二者不是一一对应。

| HTTP 状态 | 常见 `code` | 含义与处理 |
|---|---|---|
| `400` | `FIELD_INVALID` | 请求字段或 JSON 请求体不符合要求，检查 `params.field` 和 `error` |
| `400` | `LIST_PAGE_INVALID` / `LIST_FILTER_INVALID` | 分页参数、筛选值或排序字段不合法，按接口支持的范围修改 |
| `401` | `INVALID_CREDENTIALS` | 登录的用户名或密码错误，核对凭据 |
| `401` | `AUTHENTICATION_REQUIRED` | 管理凭据缺失、无效或已过期，重新登录或更换有效平台令牌 |
| `403` | `ADMINISTRATOR_REQUIRED` | 操作需要管理员角色，使用有权限的账户 |
| `403` | `PLATFORM_TOKEN_READ_ONLY` / `PLATFORM_TOKEN_NOT_ALLOWED` | 只读平台令牌不能写入，或此接口只允许登录会话；按接口要求更换凭据 |
| `403` | `MANAGEMENT_ORIGIN_REJECTED` | 管理请求的来源校验失败，检查浏览器入口与反向代理配置 |
| `404` / `405` | 具体码，或通用码 `NOT_FOUND` / `METHOD_NOT_ALLOWED` | 资源或路由不存在 / HTTP 方法不支持，核对地址、资源 ID 和方法 |
| `409` | 由具体操作决定 | 资源当前状态或并发变化使操作不能执行，先读错误详情并重新查询资源状态 |
| `413` | 使用通用 JSON 解码的接口返回 `FIELD_INVALID` | 请求体超过接口限制；通用管理 JSON 请求体上限为 1 MiB，上传等接口以各自约定为准 |
| `429` | `LOGIN_THROTTLED` | 登录尝试过于频繁，按响应头 `Retry-After` 指定的秒数等待后重试 |
| `500` / `503` | 由失败原因决定，没有具体原因时为 `INTERNAL_ERROR` / `SERVICE_UNAVAILABLE` | 服务内部出错 / 当前无法提供服务，保留错误内容并检查控制面日志；`AUTH_NOT_CONFIGURED` 表示管理鉴权未配置（`503`） |

客户端先检查 HTTP 状态，再读取 `code` 和 `params`；遇到通用码或不认识的码时，仍须保留并显示 `error`。
上述约定针对管理 API，推理网关的错误格式见 [OpenAI 兼容网关](openai-gateway.md)。错误码定义见
`internal/server/api/errors.go` 与 `internal/server/messages`（通用码在 `messages/http.go`），具体接口还可能返回业务错误码。

## 检查

`make check-api-conventions` 对 `internal/server/api` 做类型检查，找出每个 `writeJSON` / `writeSSE`
调用写出的值，并报告：

- `envelope`：写出的是切片或数组，应改为 `dto.List` / `dto.ListPage`；
- `snake_case`：可达的结构体字段、或原地写出的 map 字面量的键，编码后不是 snake_case。

它包含在 `make test-unit` 中。
