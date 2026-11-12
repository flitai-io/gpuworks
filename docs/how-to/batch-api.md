# 运行批处理任务：Batch API 与控制台

本文说明怎样把一大批推理请求交给平台异步执行：准备 JSONL 文件、上传、创建批处理任务、查看进度、下载结果、取消，以及怎样把它指向出让策略为随时出让的部署、只用空闲 GPU 跑。

**适用场景**：请求数量大、不要求即时返回的工作，例如给数据集批量生成回答、批量打标签、批量生成向量。需要逐字流式返回或毫秒级响应的请求不适合批处理，直接调用[OpenAI 兼容网关](../reference/openai-gateway.md)。

**前置条件**：

- 有一个运行中的部署，且你知道它对外的模型名（served model name）。批处理按每行请求里的 `model` 路由，和直接调用网关一样；也可以在创建任务时指定一个模型，让所有请求都发往它，见[指定模型](#指定模型)。
- 有推理令牌（`gw-` 开头）：在**API 令牌**页的**推理服务令牌**卡片创建，明文只显示一次。`/v1` 路由默认要求它，包括 server 一个推理令牌都没有的时候；只有在管理员于**设置 → 安全**打开「接受不带令牌的 /v1 推理请求」后，`/v1` 才接受不带令牌的请求，见[OpenAI 兼容网关的认证](../reference/openai-gateway.md#认证)。
- 在控制台里创建、取消批处理任务需要管理员（页面上对其他角色禁用创建按钮）；普通用户可以在控制台查看自己的任务。
- 接口规格（对象字段、错误码、限制）见 [REST API 全量路由](../reference/api.md#batches批处理任务)。

## 批处理任务做什么

一个批处理任务是一组来自同一个 JSONL 文件的请求。流程固定为三步：

1. 上传 JSONL 文件（`POST /v1/files`），平台边收边校验，通过后得到文件 ID。
2. 用这个文件创建批处理任务（`POST /v1/batches`），得到批处理任务 ID。
3. 任务结束后，从输出文件和错误文件取结果。

支持三个接口：`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`；一个任务里的所有请求必须用同一个接口。不支持流式（`"stream": true`）。完成时限默认 `24h`，创建时可以改成 `1h` 到 `72h` 的整小时（见[完成时限](#完成时限)）：任务创建后超过时限还没有结果的请求，按过期处理。

任务的请求走与客户端请求相同的网关路径：按 `model` 选副本、必要时唤醒休眠的部署、计入用量和请求记录，所以它们会出现在请求视图里，也会重置部署的闲置计时。

## 1. 准备输入文件

输入是 JSON Lines，文件名必须以 `.jsonl` 结尾（其他后缀上传时返回 400 `invalid_file_extension`）。每个非空行是一个请求，字段如下。

| 字段 | 要求 |
|---|---|
| `custom_id` | 非空字符串，在文件内唯一。结果按它对应回请求 |
| `method` | 固定为 `POST` |
| `url` | `/v1/chat/completions`、`/v1/completions`、`/v1/embeddings` 之一，创建任务时的 `endpoint` 必须与每一行相同 |
| `body` | 对象，其中 `model` 是非空字符串，且不能是 `"stream": true` |

示例 `requests.jsonl`（实际文件里每个请求占一行）：

```jsonl
{"custom_id":"q-001","method":"POST","url":"/v1/chat/completions","body":{"model":"qwen3-offline","messages":[{"role":"user","content":"Explain what a KV cache is in one sentence."}],"max_tokens":128}}
{"custom_id":"q-002","method":"POST","url":"/v1/chat/completions","body":{"model":"qwen3-offline","messages":[{"role":"user","content":"Translate hello into French."}],"max_tokens":32}}
```

文件上限：200 MB、50000 个请求（非空行），单行最长 16 MiB。上传时逐行校验，遇到第一处错误就拒绝，错误信息带行号，例如 `Line 2: custom_id must be a non-empty string`；被拒绝的上传不会留下任何文件。

## 2. 用 curl 跑一轮

以下命令使用 `GPUWORKS_URL` 和 `GPUWORKS_API_KEY` 两个环境变量，写法同[第一个模型的教程](../tutorials/getting-started.md#6-调用并验证回答)。

```bash
GPUWORKS_URL=http://192.0.2.10:8800
read -r -s -p '请粘贴推理令牌：' GPUWORKS_API_KEY
printf '\n'
```

### 上传文件

```bash
curl -sS "$GPUWORKS_URL/v1/files" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -F purpose=batch \
  -F file=@requests.jsonl
```

成功返回 200 和文件对象，记下 `id`：

```json
{"id":"file-3f9c2a7e1b6d4c8a90e5d721","object":"file","bytes":412,"created_at":1790000000,"filename":"requests.jsonl","purpose":"batch","status":"processed"}
```

`purpose` 只能是 `batch`。

### 创建批处理任务

```bash
curl -sS "$GPUWORKS_URL/v1/batches" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"purpose":"demo"}}'
```

返回批处理任务对象，初始状态是 `validating`：

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","object":"batch","endpoint":"/v1/chat/completions","errors":null,"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","completion_window":"24h","status":"validating","output_file_id":null,"error_file_id":null,"created_at":1790000010,"in_progress_at":null,"expires_at":1790086410,"finalizing_at":null,"completed_at":null,"failed_at":null,"expired_at":null,"cancelling_at":null,"cancelled_at":null,"request_counts":{"total":2,"completed":0,"failed":0},"metadata":{"purpose":"demo"}}
```

`metadata` 可选：最多 16 个键，键不超过 64 个字符，值不超过 512 个字符，都是字符串。

### 轮询进度

```bash
BATCH_ID=batch_5a1e9d03c7b24f6e8d2a4b19
while true; do
  resp=$(curl -sS "$GPUWORKS_URL/v1/batches/$BATCH_ID" -H "Authorization: Bearer $GPUWORKS_API_KEY")
  echo "$resp" | jq -c '{status, request_counts}'
  case $(echo "$resp" | jq -r .status) in
    completed|failed|expired|cancelled) break ;;
  esac
  sleep 10
done
```

`request_counts` 约每 5 秒落库一次，所以计数是跳变的，不是逐请求递增；仍在重试的请求既不算 `completed` 也不算 `failed`。

### 下载结果

任务结束后，`output_file_id` 指向成功请求的输出文件，`error_file_id` 指向失败请求的错误文件；对应请求不存在时为 `null`。

```bash
OUT=$(echo "$resp" | jq -r .output_file_id)
curl -sS "$GPUWORKS_URL/v1/files/$OUT/content" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" -o output.jsonl
```

文件格式见[结果文件怎么读](#结果文件怎么读)。

## 3. 用 OpenAI Python SDK

官方 SDK 只需要把 `base_url` 指到 GPUWORKS 的 `/v1`：

```python
import time
from openai import OpenAI

client = OpenAI(base_url="http://192.0.2.10:8800/v1", api_key="gw-...")

batch_file = client.files.create(file=open("requests.jsonl", "rb"), purpose="batch")

batch = client.batches.create(
    input_file_id=batch_file.id,
    endpoint="/v1/chat/completions",
    completion_window="24h",
    metadata={"purpose": "demo"},
)

while batch.status not in ("completed", "failed", "expired", "cancelled"):
    time.sleep(10)
    batch = client.batches.retrieve(batch.id)
    print(batch.status, batch.request_counts)

if batch.output_file_id:
    client.files.content(batch.output_file_id).write_to_file("output.jsonl")
if batch.error_file_id:
    client.files.content(batch.error_file_id).write_to_file("errors.jsonl")
```

取消用 `client.batches.cancel(batch.id)`，列出用 `client.batches.list(limit=20)`。

## 4. 在控制台运行

左侧菜单**推理服务**组里的**批处理任务**页，不写代码也能跑一轮。页面顶部的**通过 API 调用**可展开，按当前地址给出上面 curl 与 SDK 的调用示例；**API 令牌**页的调用方式里也提示了这组接口。

1. 点**创建批处理任务**，选择 `.jsonl` 文件，提交。任务调用的端点取自文件各行的 `url`，不用另选；各行 `url` 不同时会指出第一处不同的行。文件在上传时同样逐行校验，错误会指出行号和原因。可以在**模型**下拉框指定所有请求使用的模型（见[指定模型](#指定模型)），在**完成时限**里改任务的时限（见[完成时限](#完成时限)）。对话框里的可选项都由 server 下发（`GET /api/v1/batches/dictionary`）：示例可选的端点、完成时限的范围与默认值、文件大小与请求数上限、状态筛选的顺序、输出保留天数，以及可选的模型和各自的状态；控制台不自己推断，所以对话框里能选的和创建时 server 的检查一致。没有现成文件时，在对话框的**示例文件**里选示例类型（对话、文本补全或向量嵌入）和一个部署：可选的只有支持这个端点的模型（字典里每个模型带它支持的端点），默认是其中排在最前的那个（状态最好）；选了**模型**且它支持这个端点时，示例直接用它，点**下载示例**得到 1000 个请求（向量嵌入为 10000 个，每条输入由两段提示词拼成）的 `.jsonl` 示例（数学推理、翻译、摘要、写作、代码等多种任务，中英混合，对话和文本补全的 `max_tokens` 为 4096，足以让 GPU 忙上一阵；部署的上下文长度要放得下它加上提示词，否则请求会被推理引擎拒绝；同一个模型每次下载内容相同），或点**使用示例**直接用它创建任务。
2. 列表显示每个任务的 ID、状态、端点、模型（指定了才显示，否则“按文件每行”）、进度（已完成/总数，含失败数）、创建时间和过期时间，可用**按状态筛选**缩小范围。
3. 点一行打开详情抽屉：概要、请求计数、各阶段时间线、元数据，以及**下载输入文件**、**下载输出文件**、**下载错误文件**。失败的任务（`failed`）还显示失败原因；有输出或错误文件时，抽屉按任务的 `output_expires_at` 提示这些文件何时被删除。
4. 任务可以取消（任务对象的 `cancellable` 为 `true`，即 `validating` 或 `in_progress`）时点**取消任务**，在页内确认后生效；`finalizing` 阶段正在写结果文件，不能取消，抽屉里不显示取消按钮。

在控制台创建的任务属于当前登录用户，和用 API 令牌创建的任务一样，可以用同一用户的任意推理令牌通过 `/v1/batches` 查看。管理员能看到所有用户的任务，普通用户只看到自己的。

## 指定模型

输入文件每一行的 `body.model` 决定它发往哪个部署。如果整个文件都想交给同一个模型，创建任务时可以直接指定模型，不必改文件：

- **控制台**：创建对话框的**模型**下拉框（可选，不选就是**使用文件中每行的模型**）。
- **`/v1/batches`**：请求体多一个 `model` 字段，这是 gpuworks 的扩展，官方 SDK 里放进 `extra_body={"model": "qwen3-offline"}`；省略则各行用自己的模型。

```bash
curl -sS "$GPUWORKS_URL/v1/batches" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","endpoint":"/v1/chat/completions","completion_window":"24h","model":"qwen3-offline"}'
```

指定后，平台在发送每个请求（包括重试）之前把它的 `body.model` 改成这个名字，请求的其余内容不动；文件里每行仍然要有 `body.model`（上传校验照旧），写什么都行。任务对象里多一个 `model` 字段（控制台的列表和详情也显示），没指定时不出现。

指定的模型必须是某个部署（运行的或已停止的，删除中的不算）的对外模型名，不存在的返回 400（控制台 `BATCH_MODEL_UNKNOWN`，`/v1` 是 `model_not_found`），不会创建任务。模型的部署不支持任务的端点时（比如对话任务指定了只提供向量嵌入的模型）同样返回 400（控制台 `BATCH_MODEL_ENDPOINT`，`params` 为 `{model, endpoint}`；`/v1` 是 `model_endpoint_unsupported`），按网关判断部署是否开放这个接口的同一条规则（模型的推理接口声明）。

控制台的模型下拉默认只显示字典中 `ready=true` 的模型（运行或请求可自动唤醒）；示例模型使用相同筛选。勾选“显示需等待的模型”后，才能显式选择启动中、手动保持休眠、节点离线、失败或停止的模型，原有等待警告仍会显示。关闭该选项会清除已选的等待模型。API 与文件每行模型的异步等待规则保持适用。

下拉框里的模型名、状态和支持的端点来自 `GET /api/v1/batches/dictionary`，与创建时的检查用同一条判定。状态与推理服务页一致，按能否尽快服务排序：

| 状态 | 含义 | 任务里这个模型的请求 |
|---|---|---|
| `running` 运行 | 有副本醒着 | 正常处理 |
| `asleep` 睡眠 | 全部副本休眠，请求能唤醒（平台出让的休眠，或允许请求唤醒的手动休眠） | 网关挂起请求、唤醒部署，醒后放行，和普通请求一样 |
| `starting` 启动中 | 正在放置、拉取镜像或启动 | 重试，直到它开始服务 |
| `asleep_held` 睡眠（手动） | 被你或定时计划休眠且请求不会唤醒（部署详情 `sleep_held` 为 `true`） | 一直重试，直到有人唤醒它或任务过期 |
| `offline` 节点离线 | 副本所在节点断开 | 一直重试，直到节点恢复或任务过期 |
| `failed` 失败 | 副本启动失败 | 一直重试，直到有人修复并重新启动它或任务过期 |
| `stopped` 停止 | 部署已停止 | 一直等待，直到有人启动它或任务过期 |

后四种状态需要有人处理，任务照样创建，但创建的响应带 `warnings`（控制台用提示条显示，选中这种模型时下拉框下方也会提示）：`asleep_held` 是 `model_sleep_held`（控制台 `BATCH_MODEL_SLEEP_HELD`，`params` 为 `{model}`），其余三种是 `model_waits`（控制台 `BATCH_MODEL_WAITS`，`params` 为 `{model, state}`，`state` 是上表的状态值，由控制台翻成界面语言）。同一个对外模型名有多个部署时，按其中状态最好的那个算，支持的端点取并集。

**没有指定模型时检查文件里的模型**：创建任务时，平台读一遍已上传的输入文件，取出各行 `body.model` 的不同取值，逐个按上面的规则查找。其中有模型没有任何部署使用（删除中的不算）时，创建被拒绝：HTTP 400，不创建任务，上传的文件保留，可以直接用于下一次创建：

```json
{"error":{"message":"The input file names models that no deployment serves: A, B. Set a model to send every request to, or deploy these models first","type":"invalid_request_error","param":"input_file_id","code":"model_not_found"}}
```

`/v1` 的消息最多列出 5 个模型名，更多时以 `, ...` 结尾；控制台对应独立的错误码 `BATCH_FILE_MODELS_UNKNOWN`，`params` 为 `{models, more}`（`models` 是前几个模型名的列表，`more` 是还有几个，查找在发现几十个未知模型后停止，所以 `more` 可能是下限），名字由控制台按界面语言拼接。处理方法：创建时指定一个模型，让所有请求都发往它，或先部署文件里的这些模型。文件里的模型都有部署、但有的不支持任务的端点时，同样返回 400（`/v1` 是 `model_endpoint_unsupported`，控制台 `BATCH_FILE_MODELS_ENDPOINT`，`params` 为 `{models, more, endpoint}`）。文件里的模型有的处于需要有人处理的状态（上表后四种）时，任务照样创建，创建的响应按状态各带一条 `warnings`：保持休眠的是 `model_sleep_held`，离线、失败、停止的是 `model_waits`（`param` 为 `input_file_id`，列出这些模型；指定了模型时 `param` 为 `model`）；控制台的任务对象里对应 `BATCH_FILE_MODELS_SLEEP_HELD`（`params` 为 `{models, more}`）和 `BATCH_FILE_MODELS_WAIT`（`params` 为 `{models, more, state}`），指定了模型时是 `BATCH_MODEL_SLEEP_HELD`、`BATCH_MODEL_WAITS`。指定了模型时不检查文件里的模型（它们会被替换），只检查所指定的那个。

创建之后才停止的部署，请求等它重新启动（网关对停止的部署回 404，批处理确认这个模型的部署只是停止了，就当作需要等待，不判失败）；创建之后才删除的部署，对应的请求逐个得到 404，进入错误文件。

在控制台选好文件后，浏览器还会读取文件（不超过 50 MB 时）并列出它用到的所有模型，用字典里的模型名判断哪些没有部署。没有指定模型时，如果有的模型没有可用的部署，会出现警告并列出这些模型名，提示无法创建任务；指定了模型时，文件里的模型会被它替换，只显示一条提示。浏览器里的检查只是提前提醒，以创建时服务端的检查为准；超过 50 MB 的文件浏览器不检查。

## 完成时限

完成时限决定任务从创建起有多长时间：`expires_at` 是创建时间加完成时限，到点还没有结果的请求按过期处理（见[状态、取消与过期](#状态取消与过期)）。默认 `24h`；需要更紧或更宽松时可以改成 `1h` 到 `72h` 的整小时：

- **控制台**：创建对话框的**完成时限**（小时，1–72，默认 24），对话框里的格式提示会随它变化。
- **`/v1/batches`**：请求体的 `completion_window` 写 `"24h"`（OpenAI 的取值），或 `"1h"` 到 `"72h"`（gpuworks 的扩展；用官方 SDK 时可以用 `extra_body={"completion_window": "48h"}` 覆盖参数）。
- 其他写法（`0h`、`73h`、`1d`、`90m`、`24H`、带前导零或小数）返回 400 `invalid_completion_window`（控制台是 `BATCH_REQUEST_INVALID`，`params.field` 为 `completion_window`）。

任务对象和控制台详情都显示它的 `completion_window`；控制台里这个字段的范围和默认值取自字典的 `completion_window_hours`（`min`、`max`、`default`）。输出文件和错误文件的保留期（默认 30 天，字典的 `output_retention_days`）从任务结束时算起，和完成时限无关。

## 状态、取消与过期

| 状态 | 含义 |
|---|---|
| `validating` | 已创建，排队等执行名额；全 server 同时最多执行 32 个任务 |
| `in_progress` | 正在发送请求 |
| `finalizing` | 请求都有了结果，正在写输出文件和错误文件 |
| `completed` | 结束，所有请求都有了结果（成功或失败） |
| `failed` | 平台故障，例如输入文件消失；`errors` 说明原因 |
| `expired` | 创建后过了完成时限（默认 24 小时）；还没有结果的请求写入错误文件，错误码 `batch_expired` |
| `cancelling` | 已请求取消：不再发送新请求，已在途的请求最多再等 2 分钟 |
| `cancelled` | 取消完成，已有的结果在输出文件和错误文件里 |

取消用 `POST /v1/batches/{id}/cancel`（控制台里是详情抽屉的**取消**）。只有 `validating` 和 `in_progress` 的任务可以取消；`finalizing` 阶段的任务正在写结果文件，会按原样结束，已经结束的任务也一样，取消返回 409 `batch_not_cancellable`（控制台是 `BATCH_NOT_CANCELLABLE`）。控制台的任务对象带 `cancellable`，由 server 判定，抽屉据此显示取消按钮。

任务的 `expires_at` 是创建时间加完成时限（默认 24 小时）。server 重启不会丢任务：未结束的任务从最近一次进度检查点继续，已经有结果的请求不会重发。

## 结果文件怎么读

输出文件和错误文件都是 JSON Lines，每个 `custom_id` 一行，按完成顺序排列，不按输入顺序，要对应请求时用 `custom_id`。

输出文件（`purpose` 为 `batch_output`）每行是一个成功的请求：

```json
{"id":"batch_req_8c3d51f0a2e7","custom_id":"q-001","response":{"status_code":200,"request_id":"req-7b2f","body":{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The KV cache stores the keys and values of processed tokens so they are not recomputed."},"finish_reason":"stop"}]}},"error":null}
```

错误文件每行是一个最终失败的请求：

```json
{"id":"batch_req_1d90be44c6a3","custom_id":"q-002","response":{"status_code":400,"request_id":"req-91ac","body":{"error":{"message":"max_tokens is too large","type":"BadRequestError","code":400}}},"error":{"code":"400","message":"max_tokens is too large"}}
```

- 模型服务返回 4xx 时，`response` 保留完整应答，`error.code` 和 `error.message` 取自模型服务的错误对象；错误对象没有 `code` 时，`error.code` 依次取它的 `type`、`http_<状态码>`。
- 其他错误码：`batch_expired`（到期前没有结果）和 `response_too_large`（应答超过网关为批处理保留的大小）。
- 输出文件只在至少有一个请求成功时存在，错误文件只在至少有一个请求失败时存在。
- 输出文件和错误文件在任务结束后保留 30 天，之后删除（任务本身保留计数），读取返回 404；控制台的任务对象用 `output_expires_at` 给出删除时间，已过去的时间表示文件已删除，任务还在运行或没有写出这两个文件时不出现。输入文件一直保留，直到你删除。

删除输入文件用 `DELETE /v1/files/{id}`；仍有未结束的任务在读它时返回 409 `file_in_use`。

## 哪些失败会自动重试

平台把“请求本身不对”和“平台暂时服务不了”分开：

| 情况 | 处理 |
|---|---|
| 应答是 HTTP 200，但某个 choice 的 `finish_reason` 为 `abort`（随时出让的部署被更高等级抢卡） | 自动重试，不算失败 |
| 503（部署在休眠、挂起时间内没醒来、还没有副本、副本启动失败或节点离线）、429、408、其他 5xx、连接错误 | 自动重试，不算失败 |
| 404，且这个模型的部署只是停止了 | 自动重试（等部署启动），不算失败 |
| 其他 4xx：400 请求无效、404 模型不存在或部署不开放这个接口、401 等 | 最终失败，写入错误文件，计入 `failed` |

重试用指数退避，从 2 秒翻倍到 60 秒封顶，遵守应答里的 `Retry-After` 并加随机抖动，一直重试到任务过期。没有指定模型时，文件里有没人服务的模型会在创建时被拒绝；创建之后才删除的模型，每一行都会很快得到 404 而进入错误文件，不会拖到过期。

**为什么在重试**：任务运行时，控制台的任务详情会显示有请求在等待重试的每个模型：它此刻的状态（与上面[指定模型](#指定模型)的状态表相同）、多少个请求在等、从什么时候开始等，以及网关最近一次应答（HTTP 状态和原文）。接口上是单个任务对象（`GET /api/v1/batches/{id}`）的 `waiting` 字段，每项 `{model, state, status, detail, since, requests}`；没有请求在等时不出现，列表不带它。这份信息只在执行器内存里，server 重启后随下一次重试重新出现。

## 用量与请求记录里的批处理

批处理任务发出的请求在请求记录、用量统计和监控里的来源是「批处理」（`source=batch`），可以单独筛选，不和普通 API 调用混在一起。用 API 令牌创建的任务记在该令牌名下；在控制台创建的任务没有令牌，按创建它的用户记为「批处理（用户名）」。

## 与随时出让搭配

把批处理指向一个出让策略为**随时出让**的部署，就能只用空闲 GPU 跑批：实时模型占卡时批处理部署让出显存，卡空闲了再自动恢复。部署侧怎么设策略见[出让策略](scheduling-center.md#出让策略)，规则见[出让策略：几个部署怎样共用一批 GPU](../explanation/service-levels.md)。

1. 创建部署时打开**可休眠**，把**出让策略**设为**随时出让**（或之后在部署详情的**调度策略**里修改）。
2. 在 JSONL 的 `model` 里写这个部署的模型名，其余步骤同上。

批处理与随时出让部署的交互：

- **按需唤醒**：部署此刻在休眠或被让卡时，批处理请求和普通请求一样被网关挂起并触发唤醒，醒后放行。请求最多挂起部署的 `hold_max_sec`（随时出让默认 1 小时）；超时的请求得到 503，批处理自动重试。
- **算用户活动**：批处理请求会重置闲置计时，并出现在请求记录里，这一点不同于健康探测。部署因此不会在批处理中途因闲置而出让。
- **被抢占后重试**：更高等级的部署需要这些卡时，随时出让的部署立即让卡，在途请求被截断，客户端看到 HTTP 200 加 `finish_reason: "abort"`。批处理识别这个信号，把这些请求重新发送，直到任务过期为止，它们不计为失败。卡空出来后部署自动恢复，批处理继续。
- **一直被占卡时的结果**：如果实时模型一直忙，随时出让的部署一直拿不到卡，请求会持续重试直到任务过期，剩下的请求以 `batch_expired` 进入错误文件。需要更确定的完成时间时，给批处理换到不自动出让或按时自动出让的部署，或缩小批量。

## 范围与限制

- 文件和任务属于创建它们的 API 令牌所属的用户；同一用户的任意推理令牌都能看到，其他用户得到 404。管理员在控制台能看到所有用户的任务。
- 同时执行：每个任务 8 个并发请求，所有任务合计 64 个在途请求。
- 文件存放在 server 数据目录的 `batch-files/` 下，占用 server 磁盘；`GET /v1/files` 可以列出自己的文件，不再需要的输入文件请删除。
- 完成时限是 `1h` 到 `72h` 的整小时，默认 `24h`。

## 排查

| 现象 | 检查 |
|---|---|
| 上传 400，带 `Line N` | 按行号和错误码修正文件，例如 `duplicate_custom_id`、`streaming_not_supported`、`invalid_url` |
| 创建任务 400 `endpoint_mismatch` | `endpoint` 与文件里每一行的 `url` 必须一致 |
| 创建任务 400 `model_not_found`（控制台 `BATCH_MODEL_UNKNOWN`），`param` 为 `model` | 指定的 `model` 没有部署使用，检查对外模型名拼写和部署是否已删除 |
| 创建任务 400 `model_not_found`（控制台 `BATCH_FILE_MODELS_UNKNOWN`），`param` 为 `input_file_id` | 没有指定 `model` 时，文件里这些模型（消息里列出）没有部署使用；指定一个 `model` 接收全部请求，或先部署这些模型，文件无需重新上传 |
| 创建任务 400 `model_endpoint_unsupported`（控制台 `BATCH_MODEL_ENDPOINT` 或 `BATCH_FILE_MODELS_ENDPOINT`） | 模型不支持任务的端点，比如对话任务用了向量嵌入模型；换一个支持该端点的模型，或改用模型支持的端点 |
| 创建任务 400 `invalid_completion_window` | `completion_window` 必须是 `1h` 到 `72h` 的整小时，如 `"48h"` |
| 创建任务 400 `file_not_found` | 文件 ID 写错，或不是同一用户的 `purpose=batch` 文件 |
| 任务一直 `validating` | 全 server 已有 32 个任务在执行，排队等名额 |
| 任务停在 `in_progress`、计数不动 | 打开任务详情看“有请求在等待重试”：哪个模型、什么状态、网关最近的应答。部署失败就修复后重启，停止了就启动，手动休眠就唤醒或允许请求唤醒；在休眠或被让卡时看部署时间线和调度分析。请求在重试，不算失败 |
| 全部请求进了错误文件，状态码 404 | `model` 与部署对外的模型名不一致 |
| 读输出文件 404 | 任务已结束超过 30 天，文件已删除；或者没有成功的请求，输出文件本就不存在 |
| 401 `invalid_api_key` | 检查 `Authorization: Bearer <key>` |
