# 接入 KV 卸载后端

KV 卸载后端决定实例的 KV 缓存在 GPU 之外由谁保存：引擎自带的内存层与磁盘层，或引擎之外的组件
（例如 LMCache、Mooncake Store）。本页面向要接入新后端的贡献者，说明后端模型、各部分落在哪里、
新增一个后端要改什么。设计取舍见 [ADR 0069](../adr/0069-kv-offload-backends.md)。

## 后端模型

一个后端是 `internal/server/kvbackend` 里的一行描述，按 (引擎, id) 唯一，编译进二进制：

| 字段 | 含义 |
|---|---|
| `ID`、`Engine` | 后端 id 与所属引擎；id 就是部署参数 `kv_offload_backend` 的取值 |
| `Label` | 产品名，部署表单的卡片与实例面板原样展示 |
| `Default` | 引擎自带的卸载；`kv_offload_backend` 为空时解析到它，每个引擎至多一行 |
| `External` | KV 缓存由引擎之外的组件保存，请求能力快照记 `external_kv_cache` |
| `Vendors` | 加速卡厂商白名单，空表示不限 |
| `Replaces` | 选中时不可再设置的原生参数键；每个可选行都必须列出引擎自带卸载的全部键（含没有字典行的 `kv_offload_block_size`） |
| `PDRoles` | PD 部署中保留该后端的角色，必须包含 prefill；其他角色以无卸载启动 |
| `Topology` | 节点数上限与是否允许 PP、DP |

行只放用于判断的数据，没有 `init()` 注册，也没有包级可变状态。目前只有 `vllm/native` 一行，
因此部署表单与 API 的表现与只有原生卸载时完全相同。

## 各部分落在哪里

- **选择**：参数表中的 `kv_offload_backend` 是一个选择键（`Selector`），没有 flag，也不是平台参数。
  引擎有至少一个非默认行时才发布，选项由注册表生成；没有时请求里的该键被丢弃。
- **渲染**：`internal/server/deploy/kv_render_vllm.go` 按 id 提供每个后端的 vLLM 部分（`vllmOffloadBackend`）：
  渲染函数拿到完整的 `Params`（PD 角色、厂商、计算网络、并行度），返回 connector 对象，可另带
  包在 `MultiConnector` 外层的键（`wrapper`，组装点自己的键优先）与引擎进程的环境变量（`env`，
  `vLLMEngine.Build` 统一合并）；`kv_compose.go` 的 `composeVLLMKVTransfer` 把 PD 片段与卸载片段合成
  唯一的 `--kv-transfer-config`。平台为非 PD 启动合成的这份配置不会被判为 PD，无论 connector 叫什么。
  目前只有 vLLM 有卸载适配器。
- **校验**：`kv_offload.go` 返回下列码（文案在 `ui/src/i18n/locales/{en,zh}/common.json`）：

  | 码 | 何时返回 |
  |---|---|
  | `KV_OFFLOAD_BACKEND_UNKNOWN` | 引擎没有该 id 的可选后端；评估、编辑与启动都检查 |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER` | `Replaces` 中的原生键有有效值；平台按默认写入的值不算 |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_ARG` | 外部后端的参数层写了会覆盖其配置的 flag（`KVConflictFlags`，按拼写归一）；PD 下同样检查 |
  | `KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET` | 目标厂商不在 `Vendors` 中；厂商未知（未落位）时放行，落位后再查 |
  | `KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED` | 节点数、PP 或 DP 超出 `Topology` |

  后两项在 `ValidateTargetFeatures` 中检查，两种校验模式都会经过，只收集能力时不检查。
- **PD**：`PDRoleLaunchConfig` 在角色不在 `PDRoles` 时清掉选择与原生字段；`KVOffloadPDRoles` 给出保留卸载的角色，
  decode 不在其中时评估返回警告 `PD_KV_OFFLOAD_PREFILL_ONLY`。
- **反解**：引擎钩子 `RecognizeKVBackend` 把后端非 PD 渲染结果读回为选择，`ReverseArgs` 与
  `CanUnsetTemplateFlag` 共用它，另存为模板可以往返。对象里带自己参数值的后端提供读回函数
  （`vllmOffloadBackend.recognize`），返回的值作为字典值随选择一起读回；没有读回函数的后端只认
  未设置其他值时的渲染结果。
- **前端**：引擎画像的 `kv_backends` 列出可选后端（`ProfileForTarget` 按厂商过滤），部署表单每项一张卡片，
  选中时 `projectDraft` 取消该项 `replaces` 中的键；画像不再列出的已选后端（换了目标）不发送并被取消。
  部署详情里每个实例带 `kv_offload_backend`（`{id, label}`，服务端按实例的 PD 角色裁剪），实例面板据此显示。
  `ui/src` 不写后端 id。

## 新增一个后端

1. 在 `kvbackend.Builtin()` 加一行，只填它真正用到的字段；需要新字段时与第一个用到它的行一起加。
2. 在 `vllmOffloadBackends()`（`kv_render_vllm.go`）加该 id 的渲染函数；对象里带自己参数值时一并给出读回函数。
   不需要改 `internal/engines/features.go`：平台合成的非 PD 配置不按 connector 名判 PD。
3. 把 id 加进 `scripts/check_architecture.sh` 中 `kv_backend_id_literals` 的正则。
4. 测试：`kvbackend` 的表不变量；`deploy` 中 `TestKVBackendSetRendersEverySelectableRow`（`kvBackendSetProblems`）
   检查每个可选行都有渲染函数、`Replaces` 覆盖引擎自带卸载的全部键、非 PD 启动不请求 PD。通用规则已由测试行
   `vllm/testext`（`kv_backends_helpers_test.go`）覆盖，新后端补自己的渲染结果与真机 L4 用例即可。
5. 在浏览器 E2E 台账 `test/e2e/coverage/deployment-wizard-create.json` 更新卡片的执行状态。

## 已知限制

- 混合厂商集群里调度器不按后端厂商筛节点，不支持的厂商在落位后的目标校验时被拒。
- 旧版本 server 会丢掉 `kv_offload_backend`：选了外部后端的部署在旧版本上以无卸载运行。
- 只有 vLLM 有卸载适配器：SGLang 的行（如 HiCache）要先有 SGLang 侧的渲染、冲突 flag 与读回，测试会拒绝没有适配器的行。
- 后端自己的字典键（依赖 `kv_offload_backend` 的键）尚无机制：`PDRoleLaunchConfig` 只清原生键，
  `TestPDRoleLaunchConfigDropsEveryOffloadKey` 在出现这类键时失败，提醒同时让它在不保留后端的角色上清掉。
- 渲染结果还不能携带挂载与 `/dev/shm` 需求；需要它们的第一个后端连同字段一起加。
