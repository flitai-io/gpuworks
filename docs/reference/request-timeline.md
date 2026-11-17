# 请求时间线与阶段拆分

网关为每个转发的 `/v1` 与 Playground 请求记一条记录（[ADR 0042](../adr/0042-gateway-request-records.md)）。请求视图、请求详情、端到端延迟拆解、分布卡片和导出的 OTel span（OpenTelemetry 追踪片段，[ADR 0046](../adr/0046-gateway-request-traces.md)）都从这条记录算出。

本文描述时间点、阶段拆分与汇总规则；各处的字段名对照见[第 5 节](#5-各处的名字对照)。拆分规则只有一处实现：`internal/server/uxrecord/segments.go` 的 `SegmentsOf`（进行中的请求是 `LiveSegments`），汇总在同目录的 `latency.go`。

## 1. 记录了哪些时间点

除 `received_ms` 是 Unix 毫秒绝对时间外，下表其余时间点都是相对**网关收到请求**的毫秒数。没发生或没观察到的记为未知（API 里是 `null`，OTel 属性不写）。

| 时间点 | 记录字段 | 取值 |
|---|---|---|
| 收到 | `received_ms`（绝对时间，Unix 毫秒） | 网关入口鉴权时（`/v1` 校验 API key、Playground 入口）取的时间；之后才读请求体找模型名 |
| 交给副本 | `dispatch_ms` | 选好副本、读完并（需要时）改写请求体之后，交给转发之前。PD 顺序交接时，prefill 阶段在这之前完成；在途请求还在 prefill 阶段时为未知 |
| 首字节 | `first_byte_ms` | 收到上游响应头 |
| 首个思考 | `first_think_ms` | 第一个带非空 `reasoning_content` / `reasoning` 的流式事件 |
| 首个回答 | `first_answer_ms` | 第一个带非空回答文本（`content`）或非空 `tool_calls` 的流式事件 |
| 最后一块 | `last_chunk_ms` | 最后一个带文本的流式事件；结束块、usage 块、`[DONE]` 不算 |
| 结束 | `end_ms`（即端到端） | 回复复制完（或客户端走了、连接断了） |

流式事件的时刻是网关从上游**读到**这段字节的时刻（同一次读取里的多个事件取同一时刻），包含引擎到网关的网络，不包含网关到客户端。只有流式回复有首个思考、首个回答和最后一块；非流式回复只有交给副本、首字节和结束。

此外记录引擎在回复里**自报**的三个时长（[逐请求统计](inference-metrics.md#逐请求统计)）：`engine_queue_ms`（排队）、`engine_prefill_ms`（Prefill）、`engine_decode_ms`（Decode）。vLLM 要带 `--enable-per-request-metrics`；TensorFold、Strata 报 Prefill 与 Decode，不报排队；其他引擎多半不报。

PD 分离部署的请求另记一个 `pd` 对象（[第 6 节](#6-pd-分离部署)）：prefill 实例、网关等它的时长（`prefill_ms`）、prefill 实例自报的排队与 Prefill、降级原因。

**首个 token** 取首个思考与首个回答里较早的那个，是请求视图「首字」、TTFT 分布与 OTel `gen_ai.response.time_to_first_chunk` 的定义。

## 2. 阶段

| 阶段 | 含义 |
|---|---|
| `gateway` 网关 | 收到 → 交给副本，扣掉 PD 的 prefill 阶段：读请求体、找模型、选副本、注入 usage 请求、改写交给 decode 的请求体 |
| `pd_prefill` PD Prefill | PD 顺序交接时网关等 prefill 实例的时间：把请求（`max_tokens=1`）发给 prefill 实例到拿到它的回复（含失败后换实例重试） |
| `preprocess` 引擎预处理 | 首个 token 之前的等待里，引擎自报的排队与 Prefill 之外的部分：引擎进调度队列之前做的事（渲染模板、分词、等正在跑的那一步结束），加上首个 token 回到网关的网络时间 |
| `queue` 排队 | 引擎自报的排队 |
| `prefill` Prefill | 引擎自报的 Prefill |
| `waiting` 等首字 | 引擎没报排队时，首个 token 之前分不开的等待（排队、预处理与网络合在一起；只报 Prefill 的引擎，Prefill 单列） |
| `thinking` 思考 | 首个思考 → 首个回答（没有回答时到最后一块） |
| `answering` 回答 | 首个回答 → 最后一块 |
| `tail` 收尾 | 最后一块 → 结束：结束块、usage 块、`[DONE]`，以及网关写给客户端（客户端读得慢也在这里） |

## 3. 拆分算法

记 `end` 为端到端（`end_ms`，未知时用 `last_chunk_ms`），`dispatch` 为交给副本（不超过 `end`），`first` 为首个 token。

**一般情况：**

1. `pd_prefill = min(pd.prefill_ms, dispatch)`（不是 PD 请求、或并发交接时为 0），`gateway = dispatch − pd_prefill`，顺序是 网关 → PD Prefill。
2. 等待 `wait = first − dispatch`（没有首个 token 时到 `end`）。
3. 引擎报了 Prefill 时：
   - 取 `queue`（没报为 0）与 `prefill`。若 `queue + prefill > wait`（引擎与网关从不同时刻计时，引擎的时长可能比网关看到的等待还长），两者**按比例缩小**到正好填满 `wait`。
   - 引擎报了排队：`preprocess = wait − queue − prefill`，顺序是 网关 → 引擎预处理 → 排队 → Prefill。
   - 没报排队（TensorFold、Strata）：`waiting = wait − prefill`，顺序是 网关 → 等首字 → Prefill。
4. 引擎没报 Prefill：整段 `waiting = wait`。
5. 有首个 token 时，`last` 取 `max(first, last_chunk_ms)`：
   - 有思考：`thinking = (有回答 ? max(first_answer, first_think) : last) − first_think`；
   - 有回答：`answering = last − first_answer`；
   - `tail = end − last`。

**没有首个 token（非流式回复，或流里没有文本），且引擎报全了排队、Prefill、Decode 三项时**，交给副本之后的时间按引擎报告的时长拆分：

- `gateway` 与 `pd_prefill` 同第 1 步。
- `queue`、`prefill`、`answering = decode` 依次排列；三者之和超过 `end − dispatch` 时，同样按比例缩小。
- 剩下的时间是 `preprocess`。

时长为 0 的阶段不出现，各阶段之和等于端到端。

进行中的请求（在途请求）按同样规则拆到当前已经经过的时长。由于请求结束前没有引擎自报的数，首个 token 之前总是一整段「等首字」；还在 prefill 阶段的 PD 请求，`pd_prefill` 从阶段开始算到此刻。

## 4. 汇总：端到端延迟拆解与速度

服务页的「端到端延迟拆解」按部署、按 5 分钟档累加，只算**端到端时长大于 0 的完整回复**（结果为 `completed` 或 `length`；客户端断开、连接中断、出错的不算）：

- 每段的平均 = 该段时长之和 ÷ 完整回复数（没有这一段的回复按 0 计），所以各段平均之和等于端到端平均。
- p50 / p95 来自四分之一倍频程的分档直方图（每档取几何中点），误差约 10%；某一段的分位数只算有这一段的回复。
- 速度按**符合条件的请求的 token 总数 ÷ 对应时长总和**计算；时长换算为秒后，单位为 token/s。每项只纳入带 `usage`、且该项分子 token 数与分母时长都大于 0 的回复。缺少 `usage` 的回复仍可计入延迟统计，但不计速度。
  各项的分子、分母及额外条件如下：
  - Prefill：未命中缓存的输入 token ÷ 引擎 Prefill 时间，要求缓存命中数已知。PD 顺序交接用 prefill 实例自报的 Prefill，见[第 6 节](#6-pd-分离部署)。
  - 思考：思考 token ÷ 思考段，要求思考 token 数已知。
  - 回答：（输出 − 思考）token ÷ 回答段。usage 不带思考 token 数时，只算没有思考段的回复，此时分子为全部输出 token。
  - Decode：要求输出 token 数大于 1。引擎报告的 Decode 时间大于 0 时，用输出 token ÷ 引擎 Decode 时间；否则，仅在首 token 时刻已知、且最后一块晚于首 token 时，用（输出 − 1）÷（最后一块 − 首个 token）。
- 汇总按全部请求累加，不受请求日志抽样影响。

## 5. 各处的名字对照

| 含义 | 请求视图 / API | OTel span 属性 |
|---|---|---|
| 收到时刻、端到端 | `received_ms`、`end_ms` | span 开始时间与时长；`gen_ai.latency.e2e`（秒） |
| 交给副本、首字节、首个思考、首个回答、最后一块 | `dispatch_ms`、`first_byte_ms`、`first_think_ms`、`first_answer_ms`、`last_chunk_ms` | 事件 `gpuworks.dispatch`、`gpuworks.first_byte`、`gpuworks.first_reasoning`、`gpuworks.first_answer`、`gpuworks.last_chunk` |
| 首个 token | 取 `first_think_ms` 与 `first_answer_ms` 中较早者 | `gen_ai.response.time_to_first_chunk`（秒） |
| 引擎自报排队、Prefill、Decode（原值） | `engine_queue_ms`、`engine_prefill_ms`、`engine_decode_ms` | `gen_ai.latency.time_in_queue`、`time_in_model_prefill`、`time_in_model_decode`（秒）；Prefill + Decode 为 `time_in_model_inference` |
| 阶段（拆分后） | `segments[]`：`{kind, ms}` | 网关侧各段为 `gpuworks.phase.<kind>_ms`；`queue`、`prefill` 两段用上一行的引擎原值，只在第 3 节的按比例缩小发生时与 `segments` 不同 |
| 流式 chunk 数、最长停顿 | `events`、`max_gap_ms` | `gpuworks.stream.events`、`gpuworks.stream.max_gap_ms` |
| PD prefill 实例、降级原因 | `pd.prefill_instance_uuid`、`pd.fallback` | `gpuworks.pd.prefill_instance`、`gpuworks.pd.fallback`（父 span 与 `pd.prefill` 子 span 都有） |
| PD prefill 阶段 | `pd.prefill_ms`；`segments[]` 的 `pd_prefill` | 子 span `pd.prefill` 的时长 |
| prefill 实例自报排队、Prefill | `pd.prefill_queue_ms`、`pd.prefill_compute_ms` | `pd.prefill` 子 span 上的 `gen_ai.latency.time_in_queue`、`time_in_model_prefill`（秒） |

span 的其余属性见[导出请求 trace 到 OpenTelemetry](../how-to/export-request-traces.md#附录span-字段)，API 字段见 [REST API](api.md)。

## 6. PD 分离部署

PD 分离部署（[ADR 0028](../adr/0028-pd-disaggregated-deployment.md)）的请求先后经过两组实例，记录与拆分在上面的基础上多一段（[ADR 0047](../adr/0047-pd-request-breakdown.md)）：

| 交接方式（PD 模式） | prefill 阶段 | 记录 |
|---|---|---|
| 顺序（`vllm-nixl`、`vllm-ascend-mooncake`、`vllm-mooncake`） | 网关先把请求发给 prefill 实例、拿到 KV 参数（`vllm-mooncake` 由网关自己生成）后才交给 decode | `pd.prefill_ms` 是网关实测的等待；prefill 实例回复里的逐请求统计（与 decode 回复同一读法）记为 `prefill_queue_ms`、`prefill_compute_ms` |
| 并发（`sglang-nixl`、`sglang-ppu-ipc`、`sglang-ppu-mooncake`） | prefill 与 decode 同时收到请求，网关不等 | `prefill_ms` 未知、没有 `pd_prefill` 段，只记 prefill 实例与降级原因 |

顺序交接时，缓存命中与 Prefill 速度都使用 prefill 实例的数据：

- 请求的缓存命中（`cached_tokens`）取 prefill 实例回复里的值，客户端收到的 usage 也是这个数（见 [OpenAI 兼容网关](openai-gateway.md#用量usage)）。原因是 decode 实例把拉来的 KV 算作命中，它的数总等于整个提示。
- Prefill 速度用 prefill 实例自报的 Prefill（`prefill_compute_ms`），不用 decode 的首 token 重算。

prefill 实例不可用而由 decode 单独完成时（`fallback: decode`），`pd.fallback` 记原因（`prefill_unreachable`、`prefill_5xx`、`prefill_no_kv_params`、`prefill_no_bootstrap` 等，与 PD 卡片的降级原因同一套），`prefill_instance_uuid` 为空；`prefill_ms` 仍是网关在放弃之前等的时间。

**decode 实例自报的数在 PD 下含义不同**，界面改了名字，拆分规则不变：

- `queue` 显示为「KV 传输·排队」：vLLM 调度器在等远端 KV（`WAITING_FOR_REMOTE_KVS`）期间把请求留在等待队列里，所以 decode 自报的排队包含从 prefill 实例拉 KV 的时间。vllm-ascend 用同一个调度器，同样如此。
- `prefill` 显示为「首 token 重算」：KV 已经传过来，decode 只重算最后一个提示 token。

单个请求的 KV 传输时间没有引擎导出（vLLM 的 NIXL 连接器只有聚合指标，vllm-ascend 的 Mooncake 连接器只在 decode 日志里写一行），所以没有单独的一段；聚合的传输统计在服务页 PD 卡片（见 [PD 分离部署](../how-to/pd-disaggregation.md#运行中)）。
