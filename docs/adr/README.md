# 架构决策记录（ADR）

编号按创建时间递增，一经分配不再复用。部分被后续 ADR 取代的记录保留原文，在正文顶部标注取代关系；所述机制已从代码中删除、结论已由后续 ADR 承载的记录移到 `artifacts/archive/`，本索引保留编号并链接到原文，状态记为「已移出」。新增 ADR 取当前最大编号 +1，文件名 `NNNN-<kebab-slug>.md`，首行标题 `# ADR NNNN: <决策>`。

| 编号 | 决策 | 日期 | 状态 |
|---|---|---|---|
| [0001](0001-image-digest-tag-pull-verify-after.md) | 镜像校验采用「tag 拉取 + 事后比对」而非 digest 不可变引用 | 2026-08-22 | 校验部分已全部废止（image ID 三态 2026-08-23 移除，离线包 blob 比对 2026-10-08 随离线导入删除）；ref 强制带 tag 仍有效 |
| [0002](../../artifacts/archive/2026-09-25-retired-adrs/0002-runspec-field-edit-snapshot-store.md) | RunSpec 覆盖采用「字段级编辑体验 + 实例整体快照存储」 | 2026-08-22 | 已移出（RunSpec 已删除，原则见 0020、0022） |
| [0003](0003-benchmark-engine-embedded-no-containers.md) | 压测引擎内置于二进制，不依赖外部工具与容器 | 2026-08-23 | 有效 |
| [0004](0004-empty-revision-default-branch.md) | 空 revision 表示跟随平台默认分支，不回填 main | 2026-08-23 | 有效 |
| [0005](../../artifacts/archive/2026-09-25-retired-adrs/0005-blueprint-entity-and-catalog.md) | 蓝图为一等实体，两级结构（蓝图×档位），manifest 同构分发 | 2026-08-24 | 已移出（被 0007、0009 取代） |
| [0006](0006-out-of-band-recovery-ssh-credentials.md) | 保存加密 SSH 凭证以支持带外恢复 | 2026-08-24 | 有效；认证材料部分由 0035 修订 |
| [0007](../../artifacts/archive/2026-09-25-retired-adrs/0007-inference-template-schema-v2.md) | 推理模板 schema v2：更名、镜像下沉芯片层、参数字典化反解、证据分级 | 2026-08-28 | 已移出（被 0009、0020 取代） |
| [0008](../../artifacts/archive/2026-09-25-retired-adrs/0008-chip-class-catalog-gating.md) | 芯片类与目录约定式量化门控，匹配从点名改为分班 | 2026-08-30 | 已移出（被 0009 取代） |
| [0009](0009-flat-spec-catalog.md) | 扁平档位目录取代五层证据模型 | 2026-09-01 | 有效；args 列的内部拆分见 0020 |
| [0010](0010-executable-inference-catalog-package.md) | 推理目录采用模型同目录的可执行多文件包 | 2026-09-01 | 镜像不可变条款被 0017 取代；ImageDefaults 层被 0031 删除 |
| [0011](0011-inference-spec-source-identity.md) | 同一原始模型的档位按来源隔离身份 | 2026-09-01 | 有效 |
| [0012](0012-public-url-resolution-order.md) | PublicURL 解析优先级 | 2026-08-23 | 被 0025 取代 |
| [0013](0013-image-command-mode.md) | 容器命令拼装按镜像来源分「命令模式」三档 | 2026-08-24 | 三种模式仍有效，manual 为 0020 的唯一例外通道；按镜像来源给默认值已被 0022、0031 取代 |
| [0014](0014-partition-capability-framework.md) | 切分能力 ready 等于容器级探针通过 | 2026-08-25 | 有效 |
| [0015](0015-playground-request-composition.md) | 试验场请求四层组合（协议定义×目录声明×引擎发现×用户覆盖）与专家模式 | 2026-09-05 | 已接受 |
| [0016](../../artifacts/archive/2026-09-25-retired-adrs/0016-model-kinds-operation-input-projection.md) | 模型分类为 operation 与输入模态的投影 | 2026-09-06 | 已移出（kinds 机制已删除） |
| [0017](0017-embedded-catalog-image-tag-only.md) | 内置目录镜像引用改为纯 tag，不再要求 digest 不可变引用 | 2026-09-07 | 已接受 |
| [0018](0018-explicit-deployment-acquisition.md) | 部署来源显式化，服务端只校验不替换用户选择 | 2026-09-07 | 已接受；部署显式获取方式由 0048 取代 |
| [0019](0019-image-catalog-runtime-compatibility.md) | 镜像目录只维护运行时事实，兼容性采用排序告警 | 2026-09-14 | 已接受 |
| [0020](0020-launch-config-single-source.md) | 启动配置单一事实源：一套字典化参数，命令模式是唯一特殊通道 | 2026-09-15 | 已接受；2026-11-02 修订：字典解不出的模板参数透传并告警，不再拒绝导入 |
| [0021](0021-platform-params-copyable-command.md) | 平台参数与引擎参数分离，展示的命令必须复制即用 | 2026-09-16 | 已接受 |
| [0022](0022-template-owned-deployment-launch.md) | 部署启动配置归模板与显式选择，镜像不再提供可编辑默认层 | 2026-09-19 | 已接受；存量兼容与 `launch_contract` 标记被 0031 取代 |
| [0023](0023-inference-routing.md) | 多副本推理网关默认负载均衡，连接级失败重试 | 2026-09-23 | 已接受 |
| [0024](0024-data-lane-no-ctrl-fallback.md) | 数据面 lane 断开时不回退控制面，等待重连或返回可重试 503 | 2026-09-24 | 已接受；取代 `artifacts/archive/2026-09-08-tunnel-bandwidth/plan.md` §3.2 |
| [0025](0025-server-holds-no-external-address.md) | server 不管理自己的对外地址，地址由拿到它的一方给出 | 2026-09-27 | 已接受；取代 0012 |
| [0026](0026-network-module-boundary.md) | 网络决策集中在纯叶子包与一个服务，网络目录经接口注入 | 2026-09-27 | 已接受 |
| [0027](0027-rdma-network-inference.md) | RDMA 网络由推断加节点固定代替选择器，测试工具优先宿主机、其次内置 | 2026-09-28 | 已接受；修订 0030 决策 1 |
| [0028](0028-pd-disaggregated-deployment.md) | PD 分离部署是一个部署加实例角色，两阶段由现有网关编排 | 2026-09-29 | 已接受 |
| [0029](0029-deployments-reconcile-as-units.md) | 每个部署都按 unit 对账：角色 → unit → rank，一套重试、整组失败与排空 | 2026-09-29 | 已接受 |
| [0030](0030-network-objects.md) | 节点网络是可附加的对象，每节点每角色的解析结果按需计算 | 2026-09-25 | 已接受；原编号 0024，与 0024 数据面 lane 撞号，2026-09-30 改为 0030 |
| [0031](0031-image-catalog-carries-no-launch-config.md) | 镜像目录不承载启动配置，所有部署按同一套启动语义解释 | 2026-10-01 | 已接受；取代 0022 存量兼容部分 |
| [0032](0032-typed-columns-json-as-opaque-snapshot.md) | 持久化事实默认用带类型的列，JSON 列只做整体读写的不透明快照 | 2026-10-01 | 已接受 |
| [0033](0033-replica-level-lifecycle.md) | 生命周期操作落到副本：排空状态、默认滚动重启、多个多机副本、睡眠意图在实例上 | 2026-09-30 | 已接受 |
| [0034](0034-model-files-belong-to-storages.md) | 模型文件属于存储，共享存储上的模型文件只记一行 | 2026-10-02 | 已接受；修改 0018 的获取候选 |
| [0035](0035-platform-ssh-key.md) | server 持有一对平台 SSH 密钥，带外恢复优先用它，不再保存节点密码 | 2026-10-03 | 已接受；修改 0006 的认证材料 |
| [0036](0036-callable-command-owned-by-template.md) | callable 的命令行归模板，平台事实以模板变量给出 | 2026-10-08 | 已接受 |
| [0037](0037-playground-html-preview-isolation.md) | 试验场 HTML 预览在不透明源沙箱里运行，网络默认禁止、由管理员开关放开 | 2026-10-08 | 已接受 |
| [0038](0038-template-image-sources.md) | 推理模板声明镜像来源，系统改写规则只作用于系统镜像 | 2026-10-08 | 已接受；补充 0018 的镜像候选来源；决策 5 由 0048 取代 |
| [0039](0039-inference-metric-sources.md) | 推理指标的来源分层，观察配置由 server 唯一解析、运行中原地更新 | 2026-10-10 | 已接受 |
| [0040](0040-signed-multi-source-distribution.md) | 服务端升级与官方目录走签名索引的多源发布通道，官方目录最后一次导入生效 | 2026-10-11 | 已接受 |
| [0041](0041-platform-api-tokens-and-mcp.md) | 平台 API 令牌与 MCP 服务共用一种凭证 | 2026-10-11 | 已接受 |
| [0042](0042-gateway-request-records.md) | 网关为流式请求向 vLLM / SGLang 要 usage 并剥离多出的块，逐请求记录写独立的 ux.db | 2026-10-12 | 已接受；修订推理指标计划 D4 |
| [0043](0043-health-by-probe-not-rules.md) | 服务健康由探测请求判断，监控不再用规则与阈值下结论 | 2026-10-13 | 已接受 |
| [0044](0044-mac-engines-as-host-processes.md) | Mac 节点的引擎以宿主机进程运行，镜像是 darwin 环境包 | 2026-10-14 | 已接受 |
| [0045](0045-expose-connector-service.md) | 公网入口的连接器跑在独立的连接器服务里，不随 server 启停 | 2026-10-14 | 已接受 |
| [0046](0046-gateway-request-traces.md) | 网关为每个请求建立 W3C trace context，trace id 写进请求记录，逐请求记录按 OTLP 导出 | 2026-10-15 | 已接受；决策 3 由 0047 修订 |
| [0047](0047-pd-request-breakdown.md) | PD 请求的 prefill 阶段由网关实测并单列，KV 传输只给聚合统计 | 2026-10-15 | 已接受；修订 0046 决策 3 |
| [0048](0048-pull-settings.md) | 镜像从哪里拉由节点的拉取设置决定，部署不再选来源 | 2026-10-16 | 已接受；取代 0018 的部署显式获取方式与 0038 决策 5 |
| [0049](0049-expose-trusts-same-host-connector.md) | 公网入口启用期间，server 信任来自本机的转发头 | 2026-10-17 | 已接受；补充 0045 |
| [0050](0050-model-pull-sources-and-proxies.md) | 来源规则同时覆盖模型站点，代理按标签配置，凭据按 host 统一 | 2026-10-17 | 已接受；取代 0048「后果」中模型下载规则留待后续的一条 |
| [0051](0051-agent-launch-configuration-file.md) | agent 的启动配置记在 `<state>/gpuworks-agent.conf`，所有启动方式都只是 `run --state` | 2026-10-19 | 已接受 |
| [0052](0052-kubernetes-nodes.md) | Kubernetes 是一种节点来源加一种运行时，卡按需向 kubelet 借用 | 2026-10-19 | 已接受 |
| [0053](0053-storage-files.md) | 整个存储可浏览，只有 `files/` 与 `.gpuworks/apps/` 可写；上传先落暂存、成功后才回报，下载边打包边传 | 2026-10-21 | 已接受 |
| [0054](0054-usage-report-and-notices.md) | 用户体验改进计划默认加入，日报发往托管日志服务；发布方通知走签名发布通道 | 2026-10-22 | 已接受；修订 0040 补充的开关取舍 |
| [0055](0055-long-term-statistics.md) | 长期统计与原始观测分开存储：一对象一行、在入库路径上累加、增量写入；Token 用量放主库，其余放 stats.db | 2026-10-22 | 已接受 |
| [0056](0056-image-pull-connection-budget.md) | runveil 镜像拉取的连接数由按仓库 host 的预算自适应，慢连接按同伴相对判断 | 2026-10-27 | 已接受 |
| [0057](0057-units-placed-by-writing-the-pick-back.md) | 多机与 PD 的单元由平台自动放置，做法是把挑选结果写回单元 | 2026-10-28 | 已接受；补充 0028、0029 的放置方式 |
| [0058](0058-scheduling-center-acts-through-operator-orders.md) | 调度中心只执行运维事先下达的指令，休眠让出到每实例残留，节点失联重调度按运维改放置的方式进行 | 2026-10-25 | 已接受；补充 0033；决策 1 的“不抢占、不挂起请求”由 0059 修改 |
| [0059](0059-service-levels-and-the-residency-arbiter.md) | 按服务等级共用 GPU：网关挂起请求等唤醒，驻留仲裁按一条让卡规则抢占 | 2026-10-28 | 已接受；修改 0058；2026-10-29 修订：出让策略取代服务等级与闲置休眠，再修订：出让策略是唯一的选择，预设与自动休眠取消；2026-10-29 修订：新部署作为等卡方 |
| [0060](0060-card-health-from-hardware-facts.md) | 卡的健康只由硬件事实判定；事件类错误要有恢复的证据，人工干预优先 | 2026-11-01 | 已接受；补充 0043 |
| [0061](0061-spec-display-names-written-by-hand.md) | 推理档位的显示名由人写在目录里，不再由前端拼出 | 2026-11-02 | 已接受；修改 0009 |
| [0062](0062-template-components-follow-the-primary-storage.md) | 模板组件跟随主模型落到它所在的共享存储 | 2026-11-11 | 已接受；补充 0034 |
| [0063](0063-apply-to-cloud-clusters.md) | 从云账号接入的托管集群，经用户确认后由 server 用现发的短期 kubeconfig 应用接入 manifest，不保存凭据 | 2026-10-22 | 已接受 |

说明：0012–0014 原分别与 0004、0004、0006 撞号，2026-09-03 整理时按创建顺序重新编号；历史提交信息与 MR 描述中的旧编号不再准确。原 0024（节点网络对象）同理，因与数据面 lane 的 0024 撞号，2026-09-30 改为 0030；此前引用「ADR 0024」指网络对象的文字已随之更新，`CLAUDE.md` 与 `todo.md` 中的「ADR 0024」指数据面 lane。
