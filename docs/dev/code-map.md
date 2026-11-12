# 代码地图

`CLAUDE.md`「架构总览」只保留高价值的协议约束和一份「看代码看不出来的约束」清单；每个包做什么的
逐包说明放在这里。想知道某个目录/包的职责，先看这一页；想知道为什么某处这样设计、什么是唯一产地、
哪里禁止什么，回 `CLAUDE.md`。

阅读下文时，几个反复出现的说法可以这样理解：

- **装配根**负责创建各模块、传入依赖，并安排启动和关停。
- **对账（reconcile）**：比较期望状态（desired state）与实际状态，并推动实际状态向期望状态收敛。
- **unit** 是可独立调度、启动、失败、重试与扩缩的一份服务副本；单机副本只有一个 rank，多机副本包含 leader 和 follower。PD 分离中的 P（prefill，预填充）与 D（decode，解码）各自按副本管理。
- **叶子包**位于依赖关系的底层，供上层复用基础能力；各包允许依赖哪些模块，以对应条目的说明为准。
- **唯一产地**指一类规则、类型或计算只在指定位置定义或实现，其他模块共用它。

节点、部署、PD 分离等领域术语的完整定义见 [`CONTEXT.md`](../../CONTEXT.md)。

## server 侧（`internal/server/`）

- `app`：装配根（config/store/bus/hub/HTTP 接线、启动与优雅关停、accesslog、admin bootstrap）。
- `api`（+`api/dto`）：REST 层，按域拆分为数十个文件，DTO 与 entity 严格分离；`auth` JWT 登录鉴权。
- `hub`：agent 会话管理、desired state 推送（含来源规则、代理、同步组成员与凭据，凭据只随 state.sync 下发）、server_dial。
- 拉取设置（ADR 0048、[ADR 0050](../adr/0050-model-pull-sources-and-proxies.md)）：规则（镜像规则与模型站点规则同表，种类由 `match` 是否带 `http(s)://` 决定）、代理、同步组、凭据带类型的表在 `store`（迁移见 `migration_pull_settings.go`），API 在 `api/pull*.go`。

  `planning.PullCandidatesFor` 是启动、期望状态重放、镜像页拉取、多机预拉取共用的唯一镜像候选入口；`planning.PullSettingsFor` 取一台节点适用的规则与代理，模型文件任务的候选（`PlanModelFileTasks`，hub 重放与 controller 推送共用）和 server 自己读元数据的候选（`ServerFetchCandidates`）也由 `planning` 一处算出；`controller/image_acquisition.go` 决定节点缺镜像时是否从同组节点复制，并在内存里记节点对的 3 小时冷却。
- `store`：GORM entity + 版本化迁移（`schema_migrations`），SQLite（glebarez 驱动，全仓统一，勿混用 modernc）。
- `controller`：每资源一个 reconcile 文件，手写 workqueue（`workq.go`，不引 client-go）。部署不分形态走同一套 unit 对账（`units.go` / `unit_launch.go`，ADR 0029），PD 专属的启动与漂移检查在 `pd*.go`。
- 多机与 PD 的自动放置（[ADR 0057](../adr/0057-units-placed-by-writing-the-pick-back.md)）：`planning/unit_choice.go` 挑节点（`ChooseReplicaNodes`、`ChoosePDUnits`，建立在 `SuggestUnitGroups` 的分组上），`planning/unit_trial.go` 是 API 与 controller 共用的单元试算；`api/deployments_auto_units.go`（创建与评估时补节点）与 `deployments_auto_scale.go`（按副本数扩缩、PD 增加单元、`PUT /placement-mode`）；`controller/unit_auto.go` 在单元要启动时只换放不下的 rank（复用换节点的候选与写回），`controller/pd_pinned.go` 给手动钉卡被占的 PD 单元写 `pinned_gpus_unavailable`；`store/units_auto.go` 是 `deployments.units_auto` 列的迁移与写入。
- `netplan`：纯叶子包（只依赖 `proto`），节点网络的唯一决策处：角色 / 类型词表、按挂载与网卡清单解析每个节点每个角色走哪张网（RDMA 网络的 HCA 与 bootstrap 推断在 `rdma.go`）、管理网兜底、多机组共享的参数网络、两节点间的存储同步路径、能否承载服务、服务转发地址。
- `network`：网络服务，核心流程询问“某角色走哪张网、哪个地址”的唯一入口；store 行 → netplan 模型的唯一转换（`convert.go`）；网络、挂载与管理网角色的校验和变更。

  网络目录经 `Catalog` 注入：`StoreCatalog` 为库里全部网络，`ManagementOnly` 不声明任何网络、一律走管理网与 agent 隧道；`app` 创建唯一实例分给 hub、controller、proxy 与 api。`nettest`（网络内部测试编排）与 `servingdial`（服务网络转发拨号）是它的周边叶子包。
- `parallelism`：叶子包，部署并行度（全局 TP、PP 与节点数）及每节点卡数换算的唯一实现；planning 从 placement 构造它，scheduler 按它定分组大小、权重分摊与头数整除。
- `nodelabel`：叶子包（只依赖 `accelerator`），节点标签的词表：保留域 `gpuworks.ai/` 与八个系统标签键（生成在 `planning.SystemLabels`）、人写标签的校验 `ValidateWritten`（节点标签与按标签加载网络共用，拒绝保留域）、部署节点条件的校验与归一 `NormalizeRequirements`（系统键只认已声明的八个，值去重排序）。store 与 planning 都依赖它，彼此不必互相依赖。
- `pdmode`：叶子包，PD 分离模式目录（引擎、角色、连接器、线协议、TP 规则、端口、传输），PD 规则的唯一产地；`planning/pd*.go` 据此校验 `placement.pd`、给出 PD 的 unit 与部署状态（实例与 unit 的对应由所有形态共用的 `planning.MatchUnits` 推导），`deploy/pd.go` 按角色注入连接器与环境，`controller/pd*.go` 提供 PD 的启动参数与漂移检查，`proxy/pd*.go` 做两阶段路由与降级。
- `healthcheck`：部署的健康检查，每分钟给开启的部署经网关发一个最小请求（`proxy.Proxy.Probe`，不记账），最近 10 次结果只在内存里；由 `app` 接线与提供部署列表。
- `pdverify`：PD 交接自检，逐个 prefill 实例经网关做一次内部交接并判定传输方式，结果写实例时间线；由 `app` 接线。
- `scheduler` / `planning` / `estimate`：调度管道（filters → GPU planner → scorers）+ schedule_explain；显存估算：`pkg/estimate` 只放模型侧硬件事实（dtype 每元素开销、多模态 tower 运行时缓存），server 侧 `estimate` 是其门面，把 model config + 参数算成 VRAM breakdown（部署表单/详情）与 scheduler 请求输入，另含 dtype 硬件矩阵；模型 config.json 解析在 `pkg/modelconfig`。休眠、让卡与定时副本数的规则在 `planning` 里各有唯一实现，API、自动化与驻留仲裁共用，客户端不重复：`sleep_level.go`（`ResolveSleepLevel` 定一次休眠落在 L1 还是 L2，`OrderSleep` 检查资格、定级别并写休眠指令，运维休眠、定时计划、自动休眠、让卡与让位唤醒都经它）、`replica_schedule.go`（定时副本数的范围与拒绝原因 `ReplicaBounds` / `ScheduledReplicasRefusal`，以及汇总成部署 DTO 的 `schedule_capabilities` 的 `ScheduleCapabilitiesOf`）、`replica_scaling.go` / `replica_scaling_units.go`（`Scaling`：多机与 PD 副本的增减，处理器和自动化的定时副本数都走它）、`yield_eligibility.go`（部署能否设出让策略的唯一判定，设置接口、创建和部署 DTO 的 `yield_policy_eligible` 共用）。
- `sleep`：sleep/wake 策略叶子包（按模型参数量与卡数估算每实例的睡眠残留、L1 主机内存余量、资格判断、唤醒准入），不读 store；调度账本和 API 只负责提供快照、持久化与错误映射。agent 在实例独占卡时实测残留（`internal/agent/engine/sleep_residue.go`），测不到则回退估算。
- `automation`：调度分析的执行者，评估器按 10 秒 tick 执行定时计划与唤醒等位，驻留仲裁在让卡到期时（闲置时间满、一轮服务结束、等待完成的上限到）由一个定时器触发，只通过 `Source` / `Actions` 接口读写，动作经与 API 相同的生命周期锁；由 `app` 接线（`automation_adapter.go`）。
- `yieldpolicy`：叶子包（仅标准库），部署的出让策略（`never` / `idle` / `timeslice` / `anytime`）及其参数的唯一产地：取值范围、默认值与归一、让卡规则 `Yields` 与让卡条件 `TermsOf`；store（列值）、网关（唤醒时请求最多等多久）、放置与驻留仲裁（谁可以让谁休眠）共用，见 [出让策略](../explanation/service-levels.md)。
- `deploy`：引擎参数组装、instance 状态机（含 `optimizing`、sleep/wake；状态枚举定义在 `internal/proto`）。
- `proxy`：`/v1` OpenAI 兼容反代 + API key；睡眠实例返回 503。PD 交接在 `pd_sequential.go` / `pd_concurrent.go`，请求的 prefill 阶段（实例、等待、prefill 实例自报的排队与 Prefill、降级原因、`pd.prefill` span）由 `pd_stage.go` 记下，进请求记录与在途表。
- `inferenceapi`：稳定的推理操作词表与 Contract（openai.chat/embeddings/rerank 等），被目录、部署快照、proxy、Playground 共享。
- `aihelper`：AI 助手的 agent 循环（流式、并行工具、步数预算）、任务种子与已知故障表；工具以提问用户的身份在进程内读本平台 API（`APIReader`，由 `api` 按请求提供，复用同一路由与鉴权）：`cluster_overview`、`search_api`（目录 `apidocs_gen.go` 由 `docs/reference/api.md` 生成并由测试守护）、`call_api`（jq、条数与长度上限、敏感字段脱敏、时间换算）、`search_metrics`；不可读的 GET 路由及原因在 `apiread.go`。`app/aiservice.go` 组装每次请求。

  已知故障规则表 `signature.go` 与种子上下文 `seed.go` 是纯函数。server 侧任务（实例诊断、模型适配、调度不上、节点健康、自由提问）的上下文组装在 `app/aitasks.go`、`app/aicontext.go`，工具实现在 `app/aitools.go`，前端常驻侧边栏在 `ui/src/assistant/`。

  估算器负责产生数值；OpenAI 兼容 LLM 在预组装上下文上跑只读工具 agent 循环。未配置 LLM 时，规则命中与适配表仍可用。

  同一套工具经 MCP 对外提供：协议在叶子包 `mcp`（JSON-RPC、版本协商、方法分发，不认识调用者与工具），`api/mcp.go` 只做开关、限流并以调用者身份运行工具（`aihelper.Tool.Run`，与助手的 agent 循环同一份调用、报错与截断）。鉴权用平台 API 令牌（`api/platform_tokens.go`）：`managementAuth` 识别会话或令牌，令牌范围由路由声明（`sessionOnly`、`readTokensRead`，见 `api/auth.go`）；校验结果在 `store` 里短时缓存，吊销即失效。
- `provider` / `provision`：云厂商（aliyun/digitalocean/runpod/compshare/autodl/vast/zstack）租机与 SSH 预检查/安装接入。

  阿里云签 V3 请求、经云助手（`provider.ConnectionCloudAssistant`，一种不走 SSH 的连接方式）安装；`provision/connectivity.go` 是预检查里节点↔server 双向连通判断的唯一实现。SSH 安装（Linux 与 Mac）都是上传 agent 与 `installscript.Script` 后运行它（`provision/install_script.go`，`install_command.go` 生成 curl 与离线包两种手工安装命令），启动配置由脚本经 `gpuworks-agent configure` 记下，不在 provision 里另拼 systemd/nohup 启动命令。

  Vast、RunPod、CompShare 的驱动实现可选接口 `provider.ServerDialPortMapper`（CompShare 另实现 `provider.InstancePortMapper`，只对容器实例生效）（`provider/server_dial_ports.go`；各自的 `server_dial.go`，读实例已监听端口的 `provision.ListeningPorts`）：为 server 拨入的 agent 选平台已映射且空闲的 TCP 端口并记录公网地址，带外恢复也用它跟随被重新编号的端口。

  `provider` 的可选 `DriverFactory` 绑定应用级依赖，`InferenceNodeLifecycleProvider` 由驱动判断节点归属并提供生命周期处理。`provider/zstack/cloud/agent_install.go` 组织主机接入准备与共享 SSH 安装，`host_onboarding.go` 负责 Cloud 主机检查和初始快照；`cloud/resourceclaim.Service` 负责所选 GPU 的占用、释放与核对，占用所需的查询、变更和核对接口由该子包维护；`cloud/inventory` 保存发现、接入与占用共用的主机/GPU/容量类型，`cloud.Driver` 适配资源接口并组装服务。`cloud/apiclient` 负责远端 API 通信。部署暂不做 CPU/内存累计预留，当前不启动定时核对任务，超时或中断后的自动恢复待实现。`app` 不引用具体厂商实现。
- `recovery`：独立 SSH 旁路恢复离线 agent（刻意放在 controller 外，SSH 可阻塞几十秒）。登录前先重读关联的云厂商实例（`endpoints.go`）：被平台重新编号的 SSH 端点与 server_dial 的映射地址在这里跟随，一个云账号的实例清单一轮内只读一次（30 秒缓存）。
- `installscript`：agent 安装脚本 `/install.sh` 的唯一来源（叶子包），server 提供、SSH 向导上传、离线包内嵌的是同一份。
- `offlinepkg`：离线包 `/dl/gpuworks-agent.run` 的构建与服务（自解压 sh：三个平台的 agent + `installscript.Script`，server 启动后第一次被请求时构建，存进 `<data_dir>/offline/`，不含 token）。
- `tsdb` / `logstore`：内嵌时序库 + FTS5 日志检索。
- `uxrecord` / `uxstore`：网关逐请求记录（叶子包，网关、请求存储与监控 API 共用）与它的存储：独立 `ux.db`，单写协程按 UTC 日分区写请求日志（`req_`，超出每秒预算时异常请求全留、正常请求抽样并记权重）、分布网格（`grid_`）与覆盖计数（`cnt_`，后两者按全部请求统计），保留按整日删表；请求表带 `trace_id`（不建索引，按时间窗经部署索引查找，见 `lookup.go`）。
- `tracecontext`：叶子包，W3C Trace Context：解析 `traceparent`/`tracestate`、网关新起 trace 的时间有序 trace id、span id、按 trace id 低 56 位的比例采样；网关在 `proxy/request_identity.go` 为每个转发请求确立网关 span 并透传给引擎（ADR 0046）。
- `rollup` / `statsdb`：用量分析的长期统计（[ADR 0055](../adr/0055-long-term-statistics.md)）。`rollup` 是累加器：网关结束的每个请求（Token 用量、服务质量）、指标入库的每个 GPU 样本、每分钟一次的持有 GPU 快照（卡时）在内存里按小时累加，取出的是增量，写失败还回去。`statsdb` 是数据目录下独立的 `stats.db`：服务质量、GPU 利用、卡时的按小时与按天表、日汇总、按保留期分批删除、补算进度。Token 用量在主库（`store/usage_stats.go`）。`app/opsstats.go` 把三者串起来：定时写出、按天汇总、按数据保留设置（`store/data_retention.go`）清理全部存储，升级时从 ux.db 补一次历史（`opsstats_backfill.go`），并实现 `api.Analytics`。
- `schedactivity`：调度分析的调度活动（`docs/reference/api.md` 的「调度活动」）。store 在事件写入后把调度事件与实例的状态、休眠、移除事件交给它（`store.SetEventObserver`；哪个事件算哪个原因、涉及哪些卡由 `store/scheduling_activity.go` 一处定义），它按分钟累加最近约 25 小时：各原因的次数、每张卡的切换、每个部署的让出 / 被让 / 唤醒，以及每个部署至少一个副本醒着、有副本占卡的时间段。启动时从事件日志回放一次（`store.ReplaySchedulingActivity`），每个整点后整小时写进 stats.db 的三张小时表。`Service` 实现 `api.SchedulingActivity`：调度活动接口、共卡部署统计与节点时间线的统计都读它，不读事件日志；`app/schedactivity.go` 把它接进统计循环。
- `templateexport`：把运行中的部署写成平铺推理模板（[把部署另存为推理模板](../how-to/save-deployment-as-template.md)）：启动参数经 `deploy.ExportLaunchArgs` 从引擎实际生成的命令行反推并校验能重建原命令，GPU 选择器只匹配实测的型号，权重写实例所读副本解析到的 commit，缺可靠来源的字段写成注释；实测用健康检查的最小请求。不导入 store，读部署的部分在 `app/catalog_templates.go`，经 `api.CatalogTemplates` 接给 `POST /deployments/{id}/catalog-template`。
- `traceexport`：叶子包，把网关逐请求记录（`uxrecord.Request`）事后构造成 OTLP span，以 OTLP/HTTP JSON + gzip 批量推给 Collector；有界队列、重试、状态计数、测试 span。配置只来自平台设置 `telemetry_export`（`store/telemetry_export.go`，header 值加密），由 `app/telemetry_export.go` 应用并实现 `api.TelemetryService`。
- `imagecatalog`：镜像目录与分发（含 commandmode、社区目录）。各部分职责如下：
  - `imagecatalog/manifest`：镜像域 schema 与系统目录导入规则 `ImportOfficial`。
  - `imagecatalog/pullref`：纯叶子包，拉取候选的唯一实现，store、imagecatalog 与 planning 共用。`Copies` 产镜像自己的等价副本（模板 `image_sources`、目录 `registry_sources`、原名）；`PlanCandidates` 把适用于节点的来源规则产生的替代地址排在副本之前，并处理禁止回源。
  - `imagecatalog/community`：社区仓库卡片与 tag 浏览的叶子子包。`Browser` 持有可注入的 HTTP 客户端与按仓库配置指纹缓存的 tag 列表；Docker Hub / quay / OCI 三种 tag 列表 API 在 `registry.go`。
  - `imagecatalog` 本包：内嵌种子 `catalog-seed.json`、启动同步 `SyncSeed`、手动导入 `ImportManifest` 与写库适配（`seed.go`、`manifest_sink.go`）。检查更新从发布通道读取，使用 `api.Deps.Outside` 的 `distribution.Client`，API 侧见 `api/updates.go`。
  - 与 store 的边界：store 只提供引用计数 `ImageReferenceCounts` 与缺失系统行的删除/弃用 `ReconcileMissingOfficialImages`（`store/image_references.go`），不依赖 manifest 包；系统行上「运维开关不被导入覆盖」也由 store 的 upsert 保证。
  - API 入口：目录写入与检查更新在 `api/images_catalog_import.go`，节点分发在 `api/images_nodes.go`。
- `modelmeta`：模型元数据。
- `internal/server/sourceurl`：纯叶子包（仅标准库），站点规则的语法、匹配与 `Plan`（叠加、去重、回源判定）的唯一实现；`store` 按种类分派校验，`planning` 与 `modelmeta` 调用。
- `nodeaddr`：节点地址校验。
- `modelcache`：叶子包，`nfs_cache` 存储节点缓存的淘汰计划（纯函数：上限、保留空间、按最近使用从旧到新、只淘汰未被引用且有共享副本兜底的缓存）；调度准入（`planning.CachePlan`）与 controller 淘汰共用。存储与模型文件的数据结构、位置查询（`store.ModelLocation`，宿主机路径的唯一产地）在 `store/storage*.go`、`model_files.go`、`model_location.go`，见 ADR 0034。
- `prepobs`：叶子包，模型准备的内存观测（有界、过期、不落库）：hub 持有 agent 复制进度帧（`Copies`，按会话作废、按节点反向索引），controller 持有缓存淘汰阶段（`Phases`）。controller 另记实际选中的准备节点（`controller/preparation_view.go`）；`planning.PrepareModelView` 把这些与持久化的模型文件状态归约成部署的准备视图，API 经 `api.ModelPreparation` 读取。失败缓存复制的重试由 `controller.RetryCacheCopy` 编排。
- `acquisition`：部署页展示用的纯函数 resolver，只列出已持有模型的存储（`present`）、节点取模型时可保存到的存储（`targets`）与模板的权重来源（`providers`），不做选择、不做 I/O；镜像和模型怎么到达节点由节点启动时按拉取设置决定，不在这里（ADR 0048）。逐节点预估（`nodes`）在 evaluate 里由 `planning` 算出。
- `inferencecatalog`：ADR 0009 扁平目录（原始模型 + 可执行 spec 列表）、`inferencecatalog/seed`（内置包）、`inferencecatalog/packages`（内容寻址包仓库，DB 只存 digest/投影）。
- `chipmatch` / `hardwarecatalog` / `placement` / `partitioncap`：spec GPU 谓词与上报硬件事实匹配、假想规划用静态加速卡目录、spec 内嵌 Starlark 拓扑 resolver（确定性、无 I/O、硬限额）、dGPU 能力推导（status/reason 文本唯一产地）。
- `messages`：语言中立 ProductError（稳定 code + 插值参数 + 英文 fallback），人类可读文本归客户端 locale。
- `webterm`：web 终端会话策略（kill switch、并发上限、空闲超时、审计）。
- `agentbin`：内嵌各平台 agent 二进制（zstd 压缩存放，`make cross-agents` 优先用 zstd CLI `-19`，没有时退回 `scripts/zstdpack`），经 `/dl/` 下发时流式解压并给出原始长度。
- `webui`：`go:embed` 内嵌前端构建产物（`internal/server/webui/dist`），文本资源在首次被请求时 gzip 一次并缓存在内存，构建产物不再附带 `.gz` 副本。
- `selfinstall` / `installcred`：`gpuworks install` / `upgrade` 的实现（复制二进制、写 service 定义、从发布通道或发布站拉发布件、带备份可回滚）、控制台升级的启动守卫、无 systemd 时的 `serve --supervise` 守护循环，与首启 bootstrap admin 凭据记录（叶子包，install 与 server 两端共用，不得反向依赖 server）。
- `distribution`：发布通道客户端（ADR 0040）：签名信封与索引 schema、同时请求所有发布源取第一个验签通过的索引、文件按 sha256 校验并换源续传；发布方通知 `notices.json` 的 schema、校验、本地匹配与拉取（`notices.go`，ADR 0054）；默认源与信任公钥是包内常量。只依赖标准库（`check-architecture` 守护），selfinstall 与 api 共用。
- `diagnostics`：远程排查。调试采集（server 与所选节点的 debug 会话、所选部署请求原文的记录，一次一个、按时长与大小自动结束）与诊断信息收集（时间范围内的日志、调试采集文件、节点诊断与脱敏的数据库快照打包下载）；数据源由 `app/diagnostics_sources.go` 提供，`gpuworks support-bundle` 走同一套收集。
- `usagereport`：用户体验改进计划（ADR 0054，字段见 `docs/reference/usage-report.md`）：叶子包，日报结构与裁剪、进程内计数器（API 路由/错误码/panic、登录、agent 连接问题，由 `api.countUsage` 中间件与 hub 写入）、SLS WebTracking 与 Cloudflare 收集端的发送、每日发送时刻，以及检查更新附带的装机规模 `FleetReport`；库侧日汇总是 `store/usage_report.go`，调度与组装在 `app/usage_report.go`，通知铃铛服务在 `app/notices.go`。
- `kubemanifest`：叶子包，Kubernetes 集群接入 manifest（Namespace、RBAC、Secret、PriorityClass、agent DaemonSet）的纯函数渲染；集群接入与 enroll 接口在 `api/kube.go`，离开集群的节点回收在 `app/kube_nodes.go`（ADR 0052）。
- `kubeapply`：叶子包，用一份 kubeconfig（只认文件里自带的证书或 token）把 `kubemanifest` 渲染的对象 server-side apply 到 API server 或删除，支持 dry run，返回逐对象结果；kubeconfig 由 `provider.ClusterProvider` 为一次操作现发（ADR 0063）。`provider/cluster.go` 是集群能力：列出云账号的托管集群与 GPU 节点池、签发 kubeconfig、应用与卸载；阿里云实现在 `provider/aliyun/ack.go`。
- `bus`：事件发布。**不用 ORM 钩子**，写操作收敛进 `store` 显式方法，事务提交后 publish。
- `experiencelaunch`：设置页「功能体验模式」开关背后的启动器：在控制台端口 +10 起停独立的 `gpuworks experience` 进程（自己的数据目录与密码），`gpuworks experience` 的命令行契约（子命令与 flag 名）只定义在这里。

## agent 侧（`internal/agent/`）

- `app`：装配根：重连生命周期 + doctor 自检 + 后台任务（bench、下载重试、netprobe、modeldisk）；连接身份来自 `<state>/gpuworks-agent.conf`（命令行 flag 与环境变量可覆盖，优先级见 CLI 参考），server 发的 UUID 只存内存。
- `conn`：外连 + yamux 流；断线重连。
- `gpu`：多厂商探测器。AMD 通过 amdgpu sysfs/KFD 探测并按已发布身份注入所选 render 设备；单卡异常保留健康同伴。**不用 NVML cgo**，走 `nvidia-smi` + `/proc` + sysfs；ascend `npu-smi`、ppu `ppu-smi`、hygon `hy-smi`、Apple silicon `sysctl` + `ioreg`。`gpu.Prober` 可注入以做 hermetic 测试。
- `runtime`：docker / udocker / nerdctl（containerd，全命令固定 namespace；`pull_output.go` 把 nerdctl 重画的进度表换算成逐层字节行与解包行）driver，另有测试用 recording driver（shell out 到 CLI，不用 Go SDK）；`runtime.Driver` 可注入。
  - `runtime/nerdctl` 的容器身份标签：`gpuworks.image-target` 保存启动前查询到的 manifest/index digest。`Run` 只追加这一个 `--label`，保留原镜像引用。观测通过内部 `WorkloadInspection.ImageLookupRef` 按 target 查询镜像，核对返回的 target 后，将 config digest 作为容器和镜像的对外 ImageID，与 Docker 及原有镜像上报保持一致；不另外保存 config ID 标签。查询失败时不猜测 ImageID。标签缺失、无效或与不可变的 `Image` 冲突时只有镜像身份未知，容器本身照常观测，不自动补标或重建。标签可跨 agent 重启、容器创建后改 tag 保留，但不能消除 inspect 到创建之间并发改 tag 的窗口；启动前查询失败仍按原参数启动且不写标签。不要手工写入这个保留标签。
  - `kubeboot`：Kubernetes DaemonSet 的 agent 启动时从镜像交给节点状态目录里的 `agent/gpuworks-agent`（首次从镜像播种，之后沿用），新播种的一份注册前取一次 server 的 agent；此后原地升级与普通节点相同（ADR 0052 第 8 条）。叶子包。
  - `runtime/kube`：Kubernetes 节点的运行时（ADR 0052）：实例 = 钉在本节点的裸 Pod，镜像委托 `k8s.io` 命名空间的 nerdctl，日志经 apiserver，终端经 nerdctl exec；`cards.go` 是占位 Pod 借卡与空闲归还（含非整卡识别）；`devices.go` 把各卡的设备节点分到 agent 自带 device plugin 的每卡资源与共用资源；`host.go` 在节点挂载命名空间里准备 nerdctl、按 shim 认出 kubelet 的 containerd；`enroll.go` 用接入令牌换节点 token。`kube/kubeapi`（最小 REST 客户端，`kubefake` 是测试用的忠实假 apiserver）、`kube/grpcwire`（标准库上的最小 gRPC 与 protobuf 编解码）、`kube/podresources`（kubelet PodResources 客户端）与 `kube/deviceplugin`（kubelet device plugin v1beta1：注册、ListAndWatch、Allocate，kubelet 重启后重新注册）是叶子包。Kubernetes 上 agent 不挂任何厂商目录：`gpu.RunCLIsOnHost` 让全部厂商 CLI 经宿主机根在节点挂载命名空间里运行（nvidia-smi 依次找宿主机、`/run/nvidia/driver`、容器里的一份），哪些厂商能在 Kubernetes 上运行由 `accelerator.RunsOnKube` 决定，没有这些厂商卡的节点待命不接入。
  - `runtime/hostproc`：macOS 的 `host` driver（ADR 0044）。镜像是 darwin 环境包（OCI 镜像，用 `runtime/udocker/native` 的镜像库拉取与解包），引擎作为宿主进程组运行，`launch.go` 把容器形态的 spec（挂载、包内路径）翻译成宿主路径；`SupportedOn` 是它只在 macOS 可用的唯一判定。环境包由 `build/host-envs` 构建。
  - `runtime/procgroup`：不经守护进程管理的工作负载（hostproc 引擎、runveil 的 PRoot）的 pid/退出码记录、回收与整组停止，agent 重启后仍能找到它们。
  - `runtime/runveil`：udocker CLI 的薄替代（叶子包）。按 udocker P1 语义从 `UDOCKER_DIR/containers/<name>` 构造 patched PRoot 的 argv/env、生成 passwd/group、移植 `setup --nvidia`；udocker driver 默认用它启动/exec/终端/删除容器（`GPUWORKS_UDOCKER_LAUNCHER=udocker` 恢复 CLI 启动器）。udocker CLI 的供给与调用集中在 `runtime/udocker/cli.go`，runveil 下按需才供给；镜像拉取/导入/导出/创建/元数据在 `runtime/udocker/native`。`runveilcli` 是 `gpuworks-agent runveil ps/images/load/exec/logs`，直接读写 driver 的磁盘状态，不经过 agent。
- `engine` / `collector` / `dgpucache` / `sysinfo`：引擎生命周期（含 per-backend dGPU 注入器 `partition_{nvidia,ascend,ppu}.go`，按 backend ID 注册分发；进入 running 后的容器存活检查 `liveness.go`）、指标采集、libvgpu 共享区 cache 解析（dGPU 利用率上报）、系统信息（linux build tag + darwin stub）。
- `imagepull`：按 server 下发的整条候选依次拉取（`image.pull.candidates` 能力），下载阶段 3 分钟无新增字节判定卡住并换下一个（只认带层 key 的字节计数；只按粗步长显示字节的层按 `StallMinRate` 放宽时限；解包阶段不判；下载持续低于 `CrawlMinRate` 满 `CrawlWindow` 且后面还有候选时同样换下一个，最后一个候选与粗步长层不判），driver 读不懂运行时的进度输出时发 `runtime.PullLineUnreadable`，这次拉取不再判卡住；driver 自造的进度行一律用 `runtime.PullLine*` 构造（`pull_lines.go`，与解析器的契约测试见 `imagepull/pull_lines_contract_test.go`），单候选含解包最多 60 分钟，拉到后在本地 tag 回原名；镜像仓库凭据写临时 `DOCKER_CONFIG`，拉完删除。
- `connbudget`：叶子包（仅标准库），连接预算（[ADR 0056](../adr/0056-image-pull-connection-budget.md)）。`Hosts` 按上游地址（仓库 host、代理）各给一个 `Pool`，一次下载是 `Session`，每个请求先取 `Lease`；session 按自己的吞吐自适应连接数（从起点加倍、收益不足退回），429/503、拒绝连接与连续重置时减半，遵守 `Retry-After`（上游级暂停，所有 session 共同等待）；`Lease.Observe` 按同一 session 内兄弟连接的中位数与自身峰值判断「爬行连接」，每窗口最多换一半。当前只有 `runtime/udocker/native` 的镜像层下载使用（`budget.go`），模型下载可复用。
- `modelfetch`：按 server 下发的候选逐个下载模型文件（ADR 0050）：任何错误状态与连接错误使地址冷却 30 分钟（与镜像共用）、5 分钟无数据为卡住且连续 2 次换下一个、有后续候选的地址最多重试 6 次（最后一个 16 次）、版本固定为 commit 时换地址续传；站点凭据只在 https 同 host 发送。
- `registryprobe`：探测仓库 `https://<host>/v2/`（5 秒超时、缓存 30 分钟，走拉取同一代理），对镜像自己的副本按可达性和延迟排序，并记失败 / 卡住端点的 30 分钟冷却。
- `dockerproxy`：docker 节点的代理管理，把节点代理写进 `docker.service.d/zz-gpuworks-proxy.conf` 并 `daemon-reload`（不重启），上报 dockerd 实际代理、权限与是否生效。
- `probe`：行为级 dGPU 探测（与真实实例相同的注入 builder，quota 容器探测）。
- `terminal`：pty 终端服务（host pty linux build tag / container exec），走 proto 终端帧协议。
- `storagemount`：叶子包，让一个存储在本节点可用并上报状态：NFS 挂载（可注入的 `mount` 命令，不经 shell）、mountinfo 解析、挂载选项白名单、按存储单飞的有时限文件系统调用（挂死的 NFS 报 `unresponsive` 而不堆积）、试挂、控制面心跳文件。
- `modelscan`：叶子包，"已完成的模型目录"校验的唯一产地（`ValidateModelDir`：可读、有非空文件、无未完成下载，不认任何布局——布局归推理模板所选引擎；完成标记名），以及共享存储的扫描（发现期按 config.json/Mistral 布局启发式识别、身份判定、未下载完痕迹、跳过原因、分页）。
- `download` / `modelsync`：可续传多连接限速 HTTP 下载（HF/ModelScope）与节点间认证直传（server 只协调端点与一次性凭据，不中转模型字节）。
- `netinfo` / `perftest`：网卡清单（sysfs，只读不起进程）、引擎端口范围内的主机 TCP listener（netlink sock_diag，内核只回 LISTEN socket，供调度避让）与 RDMA 带宽测试：`perftest` 运行 `ib_write_bw`，先用宿主机的，没有时解出 `assets` 内置的一份（`locate.go`），按 rail 返回带原因码的结果。
- `hostsim` / `simulate`：体验环境的模拟节点。`hostsim` 是叶子包，是「代表另一台机器的节点」的唯一策略：替代真实主机上报的主机信息、主机指标与引擎端口占用，拒绝会碰到真实主机的一切（终端、节点目录外的文件、非本节点引擎的端口、存储、镜像与模型传输、升级、评测发压、模型下载）；每个 RPC 与流类型都在 `rules.go` 显式分类，proto 新增一种而未分类时测试失败、运行时默认拒绝。真实节点的 `*hostsim.Host` 为 nil，所有方法原样放行，主线路径各只有一次调用、不感知模拟。`simulate` 提供模拟的卡、运行时（不起容器，向体验环境的引擎宿主认领端口）与该机器的 `hostsim.Host`，只由 `GPUWORKS_SIMULATED_NODE` 且父进程为写入它的体验环境时启用。

## 共享库（`pkg/` 与 `internal/` 顶层）

`pkg/`：

- `estimate`（显存估算用的模型侧硬件事实，见上）、
- `modelconfig`（HF 布局 config.json → KV 估算用归一化结构，不绑定 HF/ModelScope 任一提供方）、
- `topoparse`（NVIDIA/昇腾拓扑解析）、
- `enginelog`（引擎日志方言解析与校准，内置 vllm/sglang dialect；原 vllmlog 已并入）、
- `safetensors`（解析 safetensors 元数据，计算模型权重大小）、
- `modelrepo`（HF/ModelScope 仓库列举解析，server+agent 共享，支持 `BaseURL` 与带凭据的请求）、
- `httpproxy`（代理选择与 `no_proxy` 的 Go `NO_PROXY` 语义，server+agent 共用）、
- `bench`（评测字典与执行：dataset/loadgen/metrics/report/search；`search.Document` 是 Run 的 search 列的具名类型）、
- `catalogasset`（目录随带运行时资产校验解包）、
- `localization`（BCP 47 wire 文本类型）。

`internal/` 顶层共享：

- `proto`（控制面协议）、
- `engines`（引擎名唯一产地，纯叶子无依赖：`VLLM`/`SGLang`/`Callable`，`Callable` 承载目录自带的 Serve Adapter、无日志方言与参数字典，需要方言/字典时用 `DialectNames()` 而非 `Names()`；`deploy` 再导出为 `EngineVLLM`/`EngineSGLang`/`DefaultEngine`，因为 `modelmeta` 经 `inferenceapi` 被 `deploy` 依赖、agent 又不能依赖 server，两侧只能共用这个叶子包）、
- `logging`（server+agent 共用的按大小轮转文件日志，`--log-file`/`--log-max-size`/`--log-keep`）、
- `logthrottle`（server+agent 共用的相同日志限频：同一类记录每 30 秒限量写入，其余计数后补一条汇总）、
- `debuglog`（限时限量的调试会话：会话期间把 debug 及以上的记录另写一个文件，进程本身的日志级别不变）、
- `accelerator`（dGPU 后端注册表 + **vendor 身份唯一产地**：`VendorNVIDIA/AMD/Ascend/PPU/Hygon` 常量、`NormalizeVendor`/`VendorKey`/`FrameworkVendor`/`InferVendor`/`KnownVendors`/`RequiresVendorCatalogImage`/`IsDomestic`，以及加速卡自带网口的能力 `CardNetworkOf`（rail 命名、集合通信库要的环境变量、容器要的主机文件）、加速卡健康词汇（`health.go`：正常/告警/故障三级、稳定的发现码、取证行 `FormatHealthEvidence`/`ParseHealthEvidence`，`health_guide.go`：按厂商+码+BDF 派生取证/复核/处理命令 `HealthGuideFor`），server+agent 共享，新增后端必须显式改这里）、
- `assets`（内嵌原生资产：libvgpu、partition probe、ib_write_bw 等）、
- `commandlog`（外部命令脱敏日志）、
- `templateprobe`（推理模板 L1/L2/L3 校验核心）、
- `testenv` / `integrationtest`（进程内集成测试 harness）、
- `benchrun`（一次评测 Run 的执行：预热、按 search 文档逐点闭环/开环发压、把点位结果编成 `status.benchmark` 协议；server 发压、agent 发压与 `gpuworks-agent bench` 共用，两侧只提供状态、警告与逐请求明细的去处）、
- `platformmetrics`（平台自身序列的词表：GPU、dGPU、节点、网络、KV 缓存盘、评测与网关运营指标的名字、单位、label 与含义，与推理词表 `inferencemetrics` 并列，共同构成 `GET /api/v1/monitor/metrics` 的指标目录；纯叶子，agent 与 server 共用；`TestEveryMetricLiteralIsRegistered` 扫描 agent、server 与 UI 源码，要求出现的每个指标名都已登记）、
- `agentconf`（agent 启动配置的唯一定义，叶子包：`<state>/gpuworks-agent.conf` 的字段、校验、读写、默认监听地址 `DefaultListen`，以及供所有脚本读取主机上 agent 配置的 shell 函数 `ShellFuncs`，同时识别较早安装的命令行形式；agent 的 `configure`/`run`/`smoke`/`upgrade` 与 server 的安装、预检查、卸载、带外恢复共用，见 [ADR 0051](../adr/0051-agent-launch-configuration-file.md)）、
- `launchd`（Mac agent 的 launchd 任务唯一产地：label、plist 位置、plist 编解码，以及 `install-service`/`uninstall` 用的安装与移除序列；纯叶子，agent 安装、server 查找已有 agent 共用）、
- `version`（server 与 agent 共用的构建版本、提交、构建时间与发布渠道）、
- `experience`（`gpuworks experience` 进程：一个带 guard 的控制台、模拟节点 fleet、为所有模拟实例应答的引擎宿主、离线 HF 仓库、种子数据与持续的轻量流量；guard 按路由器自己的路由模式判断请求，写默认拒绝，读默认放行但沿用 `aihelper` 不可读的路由一律拒绝）、
- `parentwatch`（父进程消失即结束子进程的工作，体验环境与其模拟节点共用）。

## 辅助 cmd

- `cmd/gpuworks-dist`：发布通道目录树的生成与核对（`assemble` 签名索引、`verify` 逐源读回、`newer` 防乱序、`prune` 挑旧版本），由 `scripts/publish_dist.sh` / `make dist-publish` 与 CI `release-publish` 调用；上传用 rclone，本工具不持有存储凭据。
- `cmd/gpuworks-image-manifest`：从 gpuworks-runner 构建矩阵生成系统镜像目录 manifest（即内嵌种子 `imagecatalog/catalog-seed.json`，随发布进入发布通道），由 `make image-manifest` / `make image-manifest-verify` 调用；每个版本的 `registry_sources` 由 pullref 出厂改写规则推出，镜像仓清单只在 `pullref` 定义。
- `cmd/template-probe`：推理模板目录 L1/L2/L3 校验 CLI（`l1|l2-command|l3|classify|alignment|calibrate`），面向目录维护者与 CI。
- `cmd/gpuworks-catalog-model-config`：从 spec 声明的仓库拉真实 config.json，回填推理目录包的 `model_config`（估算子集），使部署期不需再取远端；gated 仓库需 `HF_TOKEN`，读不到的走 `fallback-sources.json`。默认只报告，`-write` 才落盘。

## 测试资产（`test/`）

- `l2/`（`-tags=multiproc`，真实 server/agent 二进制多进程）、
- `l3/`（`-tags=docker`，容器级）、
- `l4/`（`-tags=cloudgpu,<provider>`，云/真机；`l4/cmd/` 含 `cleanup-leaked`、`prepare-compshare-golden`、`prepare-aliyun-golden`、`ascend-nerdctl-verify`）、
- `fake-engine/` / `fake-model/`（fake 引擎镜像与 tiny 模型）、
- `ocifixture`（确定性合成 OCI 镜像，覆盖硬链接/软链接/whiteout/opaque dir/大 layer 等 Dockerfile 表达不了的边界）、
- `runtime-wrapper-*`（runtime 包装镜像）、
- `e2e/`（Playwright Python 脚本，真 Chrome 跑 Playground/推理目录验收，只属于显式 gpu3/L4 job，不进离线 suite）。

## 前端（`ui/`）

React + Vite，构建产物输出到 `internal/server/webui/dist` 被 server `go:embed`。参数字典由 server `/engines` 下发，**不硬编码进前端**。i18n：`ui/src/i18n/`（en/zh locales）+ `ui/i18n/`（glossary、review 状态），`make i18n-check` 是 `make ui` 的前置。
