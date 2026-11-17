# ADR 0067: 运行期失败与检查结果用码加参数表达，英文原文只作 fallback

日期：2026-11-14 · 状态：已接受（推广 API 层的 `messages.ProductError` 到运行期与检查结果；遵守 [ADR 0032](0032-typed-columns-json-as-opaque-snapshot.md)）

## 背景

API 拒绝的请求早已是 `ProductError`（稳定码 + 插值参数 + 英文 fallback），由客户端按 locale 渲染。运行期出的问题却以英文散文到达用户：实例失败只有 `InstanceStatus.Message`，agent 读到了退出码、OOM 标记和最后 1000 行日志，却只回一句话；server 再用 `aihelper` 的一张正则表对这句话和日志重新猜一遍。dGPU 能力原因、节点预检查的 Detail、卡检查的 Reason、恢复失败的类别（靠 `ssh`、`timed out` 这类子串猜）都是同样的形态。网关 `/v1` 的错误只有 `{message, type}`，没有码。

检查结果各功能自己定义状态词，至少四套：`ok/warn/error`、`passed/failed/skipped`、`collected/blocked`、`warning/error`；「没检查」与「不适用」在 UI 上读起来都像「通过」。码的大小写也不统一：`ProductError` 用 `MOUNT_TARGET_DUPLICATE`，助手与 doctor 用 `kv_cache_insufficient`、`disk_low`。

这些散文不能本地化、不能计数、不能可靠匹配；助手依赖上游引擎的日志措辞，而它在 vLLM 与 SGLang 的版本之间会变。新代码会照着身边的写法继续写散文，所以约定必须由类型和检查强制，而不是只写在文档里。

## 决策

1. **码在产生事实的地方定，文本在客户端渲染。** 任何会到达用户（UI、API 响应、事件、采集包、助手）的失败、拒绝或检查结果，都带一个稳定码和插值参数；英文原文只作 fallback，给老客户端、日志和脚本读。新代码不得拿 fallback 做匹配，也不得把它当 UI 文案。
2. **命名。** `code` 一律 UPPER_SNAKE，标识「出了什么结果」；检查的 `id` 一律 lower_snake，标识「这是哪项检查」。老 agent 上报的小写码在 server 的读入边界统一转成大写，下游只见大写。
3. **码的产地。** agent 与 server 共用的码定义在 `internal/proto`（agent 不能依赖 server）；只在 server 内产生的码定义在 `internal/server/messages`。码常量是唯一产地，其他包引用常量，不写字面量。每个码在同一个改动里补齐 en/zh 文案（`ui/src/i18n/locales/{en,zh}`），由检查强制。
4. **检查状态只有五个值**（`internal/proto/check.go`）：
   - `pass`：查了，没问题；
   - `warn`：查了，有问题但不阻断；
   - `fail`：查了，有问题且阻断；
   - `skip`：应该查但没查（被前置检查挡住、缺能力、老 agent、超时、没收到结果），必须带码说明为什么没查；
   - `na`：这项检查不适用于这个资源。
   `skip` 与 `na` 不得渲染成通过。一项检查的结果形状是 `id + status + code + params`，可带证据（`evidence`）与复查命令（`recheck`），以 `proto.DoctorFinding` 与 `dto.DiagnosticCheck` 为蓝本；不要求所有功能共用一个 Go 类型，但字段名与状态值只用这一套。
5. **参数的持久化遵守 ADR 0032。** 传输中的参数可以是 `map`（与 `ProductError`、`dto.ProductMessage` 相同）；落库的失败原因必须是带类型的结构（如 `proto.RuntimeFailure`），`map` 只在 API 映射时临时拼出。
6. **实例失败。** `InstanceStatus.RuntimeFailure` 从「加速卡注入被拒」推广为实例运行期失败的唯一结构化原因：码、阶段、退出码、OOM 标记、证据行等带类型字段。agent 上报 `failed` 时必须带它；引擎日志分类（`pkg/enginelog`）返回码与证据行，不返回句子；已知失败签名表只在 `pkg/enginelog` 一处，助手先按上报的码诊断，只对没有码的老 agent 上报再对日志做匹配。
7. **按文本分类只允许在外部边界。** 外部系统的输出（容器运行时 CLI 的 stderr、云厂商 API 的错误体、引擎日志、SSH 的错误）只能在调用它的适配器里按文本识别，并当场转成码或带类型的 error；仓库自己产生的 error 文本永不被解析，判断用 `errors.Is` / `errors.As`。
8. **HTTP 错误都有码。** 控制面 API 的每个错误响应都带 `code`：调用方没给码时由写入器按状态码给通用码。网关 `/v1` 的错误由一张码表写出 OpenAI 形状的 `{"error": {"message", "type", "code", "param"}}`，每个响应带 `X-Request-ID`。唯一的例外是 OpenAI 自己定义了码的接口：`/v1/files` 与 `/v1/batches` 沿用 OpenAI 的小写码（如 `invalid_api_key`、`model_not_found`），因为那是外部契约，客户端按 OpenAI 文档匹配它们。
9. **由检查强制。** `scripts/arch-constraints.txt` 的相关预算（按文本分类错误、不带原因的实例失败、网关不经码表的错误、码缺 locale 文案）均为 0，只许保持为 0。

## 取舍

- **共享词表 vs 共享类型**：各功能的检查结果附带的事实不同（磁盘路径、卡、HTTP 状态），强行合成一个类型只会长出可选字段的口袋。统一的是字段名、状态值与码的规则。
- **通用码 vs 每处一个码**：按状态码给的通用码信息量低，但它让每个错误响应都可计数、可本地化；需要更具体说明的调用点照旧用 `ProductError`。
- **老 agent 兼容**：老 agent 不带 `RuntimeFailure`、上报小写码，server 读入时兼容；这是混合版本集群的长期边界，不是迁移的中间态。

## 后果

- 新增运行期失败、检查或 API 错误时，写法只有一种：在产地定义码常量，带参数上报，补 en/zh 文案。
- 助手与规则卡片按码诊断，不再随上游引擎的日志措辞漂移。
- 文档：[领域词汇表的「失败码与检查结果」](../../CONTEXT.md)、[API 约定](../reference/api-conventions.md)。
