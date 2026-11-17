# ADR 0069: KV 卸载后端是编译进来、按引擎划分的描述行，由一个组装点合成

日期：2026-11-16 · 状态：已接受（修订 [ADR 0021](0021-platform-params-copyable-command.md) 的参数分类；沿用 [ADR 0028](0028-pd-disaggregated-deployment.md) 的 PD 角色与 [ADR 0067](0067-runtime-outcomes-as-codes.md) 的码）

## 背景

vLLM 的 KV 卸载目前只有平台原生的一种：内存层（`kv_offloading_size`）与磁盘层（`kv_disk_offload`、`shared_kv_dir` 等）。服务端没有任何类型表示"一个 KV 卸载后端"，十来处代码各自从 `LaunchConfig` 的原始字段推断层的含义：参数校验、性能警告、argv、`/dev/shm` 估算、磁盘挂载、PD decode 角色清空卸载字段、请求能力快照、模板反解与取消、部署表单和实例面板。

外部后端（LMCache、Mooncake Store，以及 SGLang 的 HiCache）正在到来。照现有形态接入，每个后端都要在 Go 与 TS 里再加一轮 `== "<id>"` 判断，且容易把会进入命令行的键标成平台参数，违反 ADR 0021 决策 2。另外 `engines.CommandFeatures` 按 `kv_role` 判定 PD：非 PD 的磁盘层也带 `kv_both`，于是被记为 PD，在不支持 PD 的厂商（昆仑）上被拒。

后端名、`kv_connector_module_path`、额外配置和环境变量都等于在 GPU 容器里执行代码，因此它们的来源必须受控。

## 决策

1. **后端是编译进二进制的描述行，按 (引擎, id) 唯一。** 描述行住在叶子包 `internal/server/kvbackend`，只依赖 `accelerator`、`engines`、`pdmode`；没有 `init()` 注册，没有包级可变状态，注册表是 `Builtin()` 构造的不可变值。行只声明数据：所属引擎、是否为该引擎的默认行（空选择解析到它，每个引擎至多一行）、厂商白名单、是否外部、互斥替代的原生字典键（`Replaces`）、在 PD 下保留它的角色（`PDRoles`，必须包含 prefill：缓存层复用的是 prefill 算出的 KV）、支持的拓扑（节点数上限、PP、DP）、展示名。渲染不在行里，而是 `deploy` 中按 id 取的普通 Go 构造函数（及可选的读回函数）。字段只在第一个需要它的行出现时才加。
2. **调用方只能选 id。** API、模板、清单与节点都只能给出一个后端 id，永远不能提供 connector、模块路径、环境变量或额外配置；值由 `encoding/json` 序列化。行永不删除：未知 id 在评估、编辑与启动时返回码，存量行照常解码，运行中的实例不受影响。
3. **唯一新增的持久化事实是 `LaunchConfig.kv_offload_backend`。** 空值表示引擎的默认行（vLLM 为 `native`）。字典里的这个键由注册表生成，且只在该引擎存在可选（非默认）行时才发布；未发布时请求里的这个键按原样丢弃，与旧版本行为一致。它在参数表里是一行带 `Selector` 标记的定义（文案走参数 locale 文件），选项按引擎从注册表生成，标签为各后端的产品名。它是 ADR 0021 之外的第三类键——**选择键**：不是引擎 flag 列的成员（不进 `--kv-transfer-config` 复合组，存量的取消集合不变），也不是平台参数（它决定命令行，由引擎适配器读取）。
4. **互斥按有效值判定，服务端拒绝而不是静默清空。** 选中外部后端时，`Replaces` 中任何原生键若有有效值（内存层大于 0、磁盘层开启、目录非空、容量上限大于 0；线程与块大小只在磁盘层开启时算数）即返回 `KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER`；没有字典行的 `kv_offload_block_size` 大于 0 也算。由字典默认写入的值不算。适配器选中后端时只渲染该行、不渲染原生层，因此每个可选行的 `Replaces` 必须覆盖引擎自带卸载的全部键（`deploy` 的 `kvBackendSetProblems` 测试强制）；由于互斥，选中后端时原生字段全为零，`/dev/shm` 与磁盘挂载的现有读取方无需感知后端，表单与实例面板也可以统一把两层显示为被替代。
5. **每个引擎一个组装点。** vLLM 的 `--kv-transfer-config` 只在 `composeVLLMKVTransfer` 合成：PD 片段来自 `pdmode`，卸载片段来自所选后端；两者同时存在时合成 `MultiConnector`，外层角色取 PD 角色，读失败策略留在顶层。后端的渲染函数拿到完整的启动参数（PD 角色、厂商、计算网络、并行度），自己决定内层角色、端点与环境变量，并可给出外层 `MultiConnector` 需要的键（例如某厂商的 connector 模块路径；组装点自己的键优先）与环境变量（由 `vLLMEngine.Build` 统一合并）。非 PD 且只有原生内存层时保持 `--kv-offloading-size` 形态。组装点只接受一个 PD 片段和一个卸载片段，两个卸载片段的组合在类型上就无法表达，因此没有对应的用户码。
6. **PD 下的角色范围由行声明。** `PDRoleLaunchConfig` 在角色不在 `PDRoles` 中时清掉选择与原生字段；原生行为 `[prefill]`，与现状一致。PD 评估的 `PD_KV_OFFLOAD_PREFILL_ONLY` 警告按所选行的 `PDRoles` 判断（decode 不保留即警告），实例读模型里的 `kv_offload_backend` 同样按实例角色裁剪。
7. **厂商与拓扑只在服务端检查一次。** `ValidateTargetFeatures`（每种校验模式都会经过）按 `Backend.Vendors` 与 (节点数, PP, DP) 返回 `KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET`、`KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED`；`ProfileForTarget` 用同一谓词决定发布哪些后端（`kv_backends`），前端只渲染。厂商未知（评估时没有节点、自动落位前）与 `accelerator.Supports` 一样视为尚未确定而放行，落位后的目标校验再检查。`FeatureExternalKVCache` 只作请求能力与调度原因（`kv_backend_unsupported_on_node`）的标识，没有第二张厂商表。外部后端在模板或用户参数层中遇到会覆盖其配置的 flag（引擎钩子 `KVConflictFlags`，按 `_`/`-`、`--x=y` 与引号归一后匹配）时，`ResolveLaunch` 返回 `KV_OFFLOAD_BACKEND_CONFLICTS_ARG`。PD 下同样检查：`PD_CONNECTOR_ARG_CONFLICT` 只匹配连字符写法，而引擎把下划线写法当作同一 flag，会把后端从 prefill 的 `MultiConnector` 中挤掉；`CheckPDArgs` 与警告 W4 对没有选后端的 PD 部署的匹配方式本次不变。
8. **PD 按 connector 判定，不按 `kv_role`。** 已知的纯卸载 connector（目前 `OffloadingConnector`）不论角色都不算 PD；`MultiConnector` 按其子项判定；其他 connector 带合法角色仍算 PD。这张表只用来判定用户自己写的配置：非 PD 启动里与平台合成结果完全相同的 `--kv-transfer-config` 只承载所选卸载后端，`ResolveRequestedFeatures` 直接判为非 PD，因此新增后端不需要改 `engines` 里的表。
9. **后端自己的配置可以读回选择。** 引擎钩子 `RecognizeKVBackend` 把某个可选外部后端的非 PD 配置读回为 `kv_offload_backend`：后端提供读回函数时由它判断并返回其自身字典键的值（随选择一起写回参数），否则要求与未设置其他值时的渲染结果完全相同；`ReverseArgs` 与 `CanUnsetTemplateFlag` 共用它，因此部署另存为模板再部署仍是同一选择；包在 PD `MultiConnector` 里的对象保持不透明，取消原生键时仍报冲突。

## 取舍

- **编译进来 vs 运行时可配置**：新增后端或上游改名都需要发版。接受这个代价，因为后端描述本质上是在容器里执行的代码，不能由 API 或节点提供。
- **数据行 + Go 构造函数 vs 描述 DSL**：每个后端的配置形状不同，用 DSL 表达只会长出一个可选字段的口袋；行只放用于判断的数据，形状留在可测试的 Go 函数里。
- **先落只含原生行的接缝**：外部后端的通用机制（互斥、角色、门禁、反解、表单卡片）此时只由测试行驱动，用户面与旧版本逐字节一致；第一个外部后端只需加一行、一个构造函数和测试。
- **PD 判定改变行为**：昆仑上的 vLLM 非 PD 磁盘层不再被拒，而这一组合尚未在真机上验证；用户手写的未知卸载 connector 在加入列表前仍按 PD 处理，这是安全方向。

## 后果

- 新增 KV 卸载后端：在 `kvbackend` 加一行、在 `deploy` 加一个构造函数（需要时加读回函数）并补测试；目前只有 vLLM 有卸载适配器，其他引擎的行会被测试拒绝，直到该引擎有自己的渲染与读回；不得在其他 Go 或 `ui/src` 中写后端 id 字面量（`scripts/arch-constraints.txt` 的 `kv_backend_id_literals` 预算，基线 0）。
- 降级：旧版本 server 用普通 JSON 解码会丢掉 `kv_offload_backend`，选了外部后端的部署在旧版本上重启会以无卸载运行，编辑或草稿往返会删除该选择。这是记录在案的行为，由测试钉住。
- 混合厂商集群里调度器不按后端厂商白名单筛节点，不支持的厂商在落位后校验时被拒。
- 术语见 [领域词汇表](../../CONTEXT.md)。
