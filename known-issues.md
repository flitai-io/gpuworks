# 已知问题

本文件记录已确认、但决定暂不修的缺陷：原因、影响范围与绕开方式。`todo.md` 记的是打算修或需要决策的事项，
本文件记的是已经做出"暂不修"决定的事项；两处都用同一套 T 编号，迁入时保留原编号与原文，并在节首加一行搁置记录。

搁置理由通常是：影响面窄且有绕开方式、触发条件需要特定环境才能复现、或修复的代价与风险高于当前收益。
条件变化（真机可用、用户实际踩到、影响面扩大）时迁回 `todo.md`。

| 问题 | 影响 | 搁置理由 |
| T20：`Model.WeightBytes` 沿用声明值，下载后不回写实际大小 | 目录/元数据声明错误时长期停留，planning 的显存与磁盘估算一直读错误值且无告警 | 声明值只用于调度前粗筛，真正放不下由引擎启动失败兜底；目录 spec 的声明值至今未见错误 |
| T41：删除节点副本不等待节点间模型同步的接收端结束 | 删除之后被取消的同步仍可能把临时目录改名回来，磁盘上留下 server 已无记录的模型目录 | 未证实、未复现；窗口是删除与 rename 之间，后果是一份孤立目录占盘，不影响正确性 |
| T40：昇腾 vcann-rt 与分区探针拼 `LD_LIBRARY_PATH` 不去重、不丢空段 | 重复项只是冗余；空段让加载器搜当前目录 | 空段是镜像自带的，不是本仓库引入的；去掉它等于替镜像作者改写环境，需要昇腾机器实测后再决定 |
| T53：按卡推出的显存份额进了表单草稿后会粘住 | 同一表单会话里换到大卡节点，份额仍是小卡算出的值，大卡上白留显存 | 方向安全（少用不会 OOM）；修它要新增 DTO 字段区分「平台推的值」与「用户填的值」，属 `todo.md` T6 的一部分 |
| T54：`internal/server/api` 在 `-race` 下跑不完 `go test` 的 10m 默认超时 | 按 `CLAUDE.md` 的提交前检查逐字执行，必然看到一个长得像失败的红（超时后的 goroutine dump，不是 DATA RACE） | 先在规约里写明 race 检查要带 `-timeout`；给它一个带足超时的 target 属于另一件事 |
| T45：时间线折叠 SQL 只为历史事件行存在 | `instance_timeline.go` 用 `json_extract` 取旧行的 `$.state`/`$.phase`，是迁移之外唯一看进 JSON 的 SQL（`store_sql_json_lines` 为 2） | 现在无事可做：要等 2026-09-17 之前写下的事件行随保留期（默认 180 天）淘汰，约 2027-03 |
| T61：通用 `PUT /settings/{key}` 只要求登录、不限 key | `user` 角色可以写任意设置行，包括全局网络/镜像配置、`orphan_cleanup`、有 admin 专用路由的 `storage_policy`/`control_plane` 与试验场的 `security` 策略 | 平台目前只有管理员用户，`user` 角色没有实际使用者 |
| T66：平台因容器运行时无法使用加速卡而自动停止调度后，原因消失也不会自动恢复 | 装好插件、或 docker 重新配置 nvidia runtime 之后，节点仍是「停止调度」，要运维手动开启 | 你定过「不加锁定字段」：自动恢复要新增一个「停调来源」事实，才能只撤销平台自己设的停调而不碰运维的；偶发的 daemon 无应答已不再触发停调，误停的来源已去掉 |
| T67：实例按卡重排序号时的两个边角 | 绑定的卡这次没报上来时，实例保留旧序号，若另一张卡恰好前移到这个序号，账本会重复计数；卡的 ident 在 `pci:` 与 `uuid:` 之间翻转时被当成新卡，绑在它上面的实例不会被重排 | 前者要卡消失、后者要同一张卡的 BDF 时有时无，当前四家厂商的探测都总能给出 BDF，暂无触发路径 |
| T68：调度对非本平台进程占用的卡只做整卡排除，不按剩余显存放置 | 卡上只要有一个外部进程（`GPU.ForeignProcesses` 非空），整张卡就不可调度，评估给出 `gpus_held_outside_platform`；外部进程只占了一小部分显存时，剩下的显存也用不上 | 这是有意的保守：平台提供动态 GPU、显存限额共享、sleep/wake 等多种占卡方式，外部进程的占用量会随时变化，按剩余显存放置得不到稳定的保证；整卡排除不会出现先启动再 OOM |
| T51：两个持续很短的读写竞态 | 快照建好后才休眠的 prefill 被记成一次 PD 降级（严格降级策略下该请求被拒）；控制器读部署与读实例之间发生缩容时，先补起一个副本再把它排空 | 窗口分别不到 1 秒与一轮对账，后果是一次降级记录或一次白占卡的副本，不影响正确性；再出现或规模变大时再修 |
| T75：agent 下载模型逐个文件串行，每个文件 4 条连接，高带宽节点上跑不满 | `internal/agent/app/modelfiles.go` 按文件顺序调用 `fetcher.Fetch`，`Concurrency: 4`。2026-10-19 Vast H200×4（标称 3.2 Gbps）下 185 GB 用了约 70 分钟（约 45 MB/s）；同日 B200 节点上 `hf download --max-workers 32` 1 分 23 秒下完 510 GB（约 6 GB/s）。按小时计费的大卡上，下载时间就是主要成本 | 2026-10-20 决定暂不修；大模型可先在节点上用 `hf download` 预下载，再按路径登记 |
| T80：拉镜像失败时，实例消息里真正的错误行排在几十行进度条之后 | 消息约 4 KiB，开头是 `… Downloading [===>  ] …` 进度行，`Error response from daemon: …` 在最后一行；实例详情要翻过一屏进度才看到原因，只显示一行的地方（实例卡片的进度行）看不到原因 | 体验问题，原因并没有丢：完整消息里有，agent 日志里也有；改法是拼消息时去掉进度行、错误行放前面 |
| T102：udocker CLI 路径「从节点删除」镜像必然失败 | 关闭 native 拉取的 udocker 节点上镜像删不掉 | 平台的 udocker 节点都用 native（runveil），不走 CLI 路径 |
| T103：udocker CLI 路径拉取还没完成，节点镜像行就显示「已存在」 | 拉取卡住或失败时界面仍显示镜像在节点上 | 同 T102 |
| T107：用量分析按卡型筛选逐卡列出时，漏掉从仍在的节点上拔掉、而该节点已没有这种卡的卡 | 这张卡的历史只在汇总行与总计里，逐卡列表与热力图（按单卡）里没有它，两边差这一张 | 修它要在只按卡型筛选时扫这个卡型全部卡的统计行，规模大时代价高；触发要换卡且换成别的型号，节点删除的情况已覆盖 |
| T108：agent 状态目录在宿主 `/tmp` 下时，udocker 工作负载找不到宿主路径下的库 | runveil 给容器私有 `/tmp` 的绑定盖住了放在 `ROOT/tmp/...` 下的宿主路径回环链接，引擎按宿主路径打开 `libcudart` 等库时失败 | 只在状态目录装在宿主 `/tmp` 下时触发；安装脚本与各云厂商一键安装都默认 `/opt/gpuworks`，测试夹具也已改用 `/opt` |
| T109：Strata 的宿主机内存不参与准入 | 低内存节点可能通过每卡 12 GiB 显存检查后，在 setup 阶段失败或产生内存压力 | 本次仅发布目录规格；可先核对可用内存、标记合适节点并在部署时手动选择 |

## T20. 模型权重字节对账：下载完成后不回写实际大小

**搁置记录**：2026-10-02 决定暂不修。`Model.WeightBytes` 的消费者只有 planning 的调度前粗筛，
声明值偏小会让一个放不下的部署被调度出去、由引擎启动失败兜底，偏大只是少一个候选节点，两者都不产生错误结果；
目录 spec 的声明值至今未见错误。需要修的信号是：用户报告"明明装得下却调度不出去"，或出现声明值与实测值系统性偏差的目录来源。

**状态**：测试金字塔遗留，2026-09-24 从 plan.md §3.2 迁入；未实施。

### 现状

- `store.Model.WeightBytes`（`internal/server/store/entities_model.go:22`）只在模型注册/绑定目录档位时写入一次：`persistCatalogWeight`（`internal/server/api/deployments_evaluate.go:73-84`）只在 `model.WeightBytes` 当前为 0 时，才把目录档位声明的 `selection.Spec.WeightBytes` 回填进去，之后不再覆盖；`model_url_register.go`、`model_local_register.go`、`model_quick_register.go` 同样只在注册时各写一次估算/声明值。
- 节点侧其实有更接近真实的字节数：`store.ModelFile.SizeBytes`（`entities_model.go:48`）在下载过程中随 agent 上报的 `st.BytesTotal` 更新（`internal/server/store/deploy.go:335-336`），下载完成后落在该节点该模型的 `ModelFile` 行上；但这个值只喂给该节点的磁盘占用/驱逐核算（`internal/server/store/model_eviction.go`），从未回写到全局的 `Model.WeightBytes`。
- 结果：`Model.WeightBytes` 一旦声明错误（目录 spec 写错、HTTP 导入的目录列举估算偏差、HF/ModelScope index 与实际 shard 不一致等），会一直错下去；`internal/server/planning/planning.go:124-141` 的显存与磁盘需求估算全程读这个声明值，不会拿 `ModelFile.SizeBytes` 交叉核对，也不产生任何告警或不一致记录。

### 建议

下载完成（`ModelFile.State == proto.MFPresent`）时，若该节点上报的 `SizeBytes` 与 `Model.WeightBytes` 偏差超过阈值（例如 5%），至少记一条可观测事件或日志；是否据此覆盖 `Model.WeightBytes`（存在多节点数值冲突、多来源精度不同的问题）留给实现阶段判断，但现状"完全不检测、不告警"应该改。

补充（2026-10-02 核实）：`MFPresent` 上报里的 `BytesTotal` 确实是落盘后的实测值，不是 server 下发的预期值——
下载路径由 `writeCompletionMarker` 返回的 `measured`（`internal/agent/app/modelfiles.go:175`、`:288`），
节点间同步路径由 `fileBytes`（`internal/agent/app/model_sync.go:128`）。因此真要做这个比对，数据是可用的。

### 验收条件

- 下载完成后，声明值与实际 `SizeBytes` 偏差超过阈值时有可观测记录（事件或日志），不是静默。
- 有测试覆盖声明值准确、偏小、偏大三种情形下的行为。
- planning 的显存/磁盘估算是否需要改为读实测值，或说明继续读声明值为何安全（例如声明值只用于调度前粗筛、真正加载由引擎自身失败退出兜底），在实现时给出结论并写进验收记录。

## T41. 删除节点副本不等待节点间模型同步的接收端结束

**搁置记录**：2026-10-02 决定暂不修。未证实、未复现：窗口在"删除返回"与"接收端 rename"之间，
后果是磁盘上留一份 server 已无记录的模型目录，占盘但不影响部署正确性（该目录不在任何 `ModelFile` 行上，
不会被当成已有副本使用）。修法清楚——照 `removeModelPath`（`internal/agent/app/modelfiles_rpc.go:200`）
对下载的处理，把 `n.modelSyncs` 里路径重叠的接收端一并取消并等结束，再在锁内删目录；
需要让 `rpcModelSyncStop` 从异步改为可等待。真机上观察到孤立目录、或盘空间因此告急时再做。

**来源**：2026-10-01 修复 T38（删除节点副本后立即重新下载失败）时发现，未证实、未复现。

删除副本现在会先取消并等待该路径上的下载结束再删目录，但节点间同步走的是另一条路径：`rpcModelSyncStop` 是异步的，返回时接收端可能还在跑；`receiveModelArchiveAtomic` 先解压到临时目录再 rename，所以删除之后被取消的同步理论上仍可能把目录改名回来，磁盘上留下 server 已无记录的模型目录。

### 验收条件

- 同步进行中删除副本：删除返回后接收端已结束，目录不会再出现；有 hermetic 用例覆盖。

## T40. 昇腾 vcann-rt 与分区探针拼 `LD_LIBRARY_PATH` 不去重、不丢空段

**搁置记录**：2026-10-02 决定暂不修，原记录的两个毛病性质不同，核实后都不适合当成普通缺陷直接改。

**去重**：注入的驱动路径（`/usr/local/Ascend/driver/lib64` 及其 `common`、`driver` 两个子目录）在昇腾镜像自己的
`LD_LIBRARY_PATH` 里本来就有，所以拼接后各出现两遍。但注入的在前面，优先级本就正确，按"保留首次出现"去重的结果
与现状完全一致：**语义零影响**，只是每次库查找多 stat 三个目录，以及排障时环境变量更难读。纯整理，不做没有功能影响。

**丢空段**：影响是真的——加载器把空段当作当前目录，引擎进程工作目录（或挂进去的卷）里的同名 `.so` 会优先于系统路径被加载，
既是加载顺序风险也是注入面。但 2026-10-02 核实发现**这个空段是镜像自带的，不是本仓库拼出来的**：拼接顺序是
`驱动路径 + ":" + 镜像值`，镜像值在后，而目录里在用的 5 个昇腾镜像的 `LD_LIBRARY_PATH` 全部以冒号结尾——
`quay.io/ascend/sglang` 的 v0.5.18（34 档）、v0.5.14（20 档）、main（1 档），`vllm-ascend:v0.23.0`（18 档），
`vllm-omni:v0.23.0rc1`（2 档），逐个取 registry 的 image config 确认。也就是说用户直接 `docker run` 这些镜像时，
当前目录同样在搜索路径里。

因此"丢空段"不是修 bug，而是**决定要不要替镜像作者清理搜索路径**：改了会让平台里的行为与手工 `docker run` 不一致，
若某个镜像恰好依赖工作目录找库（概率不高，但读代码排除不了，取决于镜像 `WorkingDir` 里有什么），就会从能跑变成找不到库。

### 要动这项时

- 去重可以随手做，但单独开 MR 不值得，建议搭在下一次改到这两处的工作里。
- 丢空段必须在昇腾节点上先验一次：起一个实例，比对 `LD_DEBUG=libs` 的搜索顺序与 `ldd` 结果，确认没有库依赖当前目录；
  同时决定这是只对分区注入路径生效，还是平台统一清理所有镜像的搜索路径。
- 合并规则如果要统一，`internal/agent/runtime/udocker/env.go` 的 `joinSearchPath` 已实现去重与丢空段，
  但它在 runtime 包，engine 包要用需要先挪到两边都能依赖的位置。

## T53. 按卡推出的显存份额进了表单草稿后会粘住

**搁置记录**：2026-10-03 决定暂不修。粘住的方向是安全的——留下的份额偏小，引擎不会因此 OOM，
代价是大卡上白留显存，而用户随时可以拖滑块收回。修它需要服务端新增一个 DTO 字段来标明
「这个值是平台按卡推的」，前端据此在节点变化时清掉它重算；那是「区分 auto 与显式值」的第一步，
属于 `todo.md` T6 建议 2 的范围，宜与之一并做而不是单独塞一个字段。
用户反馈换节点后份额没跟着变、或大卡上的浪费变得可观时迁回。

**来源**：2026-10-03 显存利用率按卡预留（`artifacts/archive/2026-10-02-vram-reserve-default/plan.md`）实现时实测。

`gpu_memory_utilization` 的默认值现在按目标节点的卡推导，经 evaluate 的 `form_values` 写进表单字段。
写进去之后它就是草稿里的一个具体值，下一次 evaluate 会把它当成用户显式填的值带上，服务端不再套默认值。
实测（同一个模型，只换节点）：

| 请求 | `form_values` |
|---|---|
| 12 GiB 节点、什么都没填 | 0.83 |
| 80 GiB 节点、什么都没填 | 0.9 |
| 80 GiB 节点、带着 0.83 | 0.83 ← 粘住 |

所以在一个表单会话里从 12 GiB 节点切到 80 GiB 节点，份额仍是 0.83，80 GiB 卡上白留约 13 GiB。
方向是安全的（少用不会 OOM），但用户不一定察觉。另有一个更轻的表现：第一次 evaluate 返回之前，
滑块显示的是字典里不按卡算的 0.9（`DeployParamField.tsx` 的 `value ?? spec.default`），评估回来后跳到按卡的值。

这正是 T6 建议 2 说的那件事——硬件相关的 key 应持久化 `auto`、由纯函数在用到时解析、永不落盘；
现在它一旦进了表单就变成具体值。

### 原记录的建议

服务端标明哪些 key 的值是平台推导来的（而不是用户填的），前端在节点变化时清掉这些 key 让它们重算。
这需要新增一个 DTO 字段，是「区分 auto 与显式值」的第一步，宜与 T6 一并考虑而不是单独做。

### 验收条件

- 换节点后，用户没碰过的按卡推导值重算；用户手动改过的值不被覆盖，两者各有测试。
- 表单首次渲染不显示一个随后会变的值，或明确说明它是未评估状态。

## T54. `internal/server/api` 在 `-race` 下跑不完默认超时

**搁置记录**：2026-10-03 决定暂不修。这不是竞态，是耗时：该包不带 race 实测 95s，带 race 实测 1835s
（约 31 分钟，零 DATA RACE），而 `go test` 的默认超时是 10 分钟。所以照 `CLAUDE.md`
「提交前检查：…相关包 `go test -race`」逐字执行，这个包必然在 601s 处超时并打印一大段 goroutine dump ——
看起来像失败，实际只是没跑完。加 `-timeout 40m` 就通过。

仓库对同类问题已有先例：`Makefile` 里 L2 专门有 `L2_TIMEOUT`，注释写明
"replaces `go test`'s 10m default, which this suite had outgrown"。`api` 包显然也 outgrow 了，
只是还没有对应的 target。

**为什么暂不做**：真正的修法有两种，都不该顺手塞进别的 MR ——
给 race 检查一个 Makefile target（要决定覆盖哪些包、是否进 CI、CI 时长预算），
或者拆分这个包（它已是全仓扇出最高的包，`api_fanout` 有 38 的棘轮预算盯着）。
眼下的实际影响是"执行规约的人会看到一次假红"，代价是一次困惑而不是一个缺陷。

### 要动这项时

- 最小做法：在 `CLAUDE.md` 的提交前检查一条里写明 race 检查需带 `-timeout`，并记下这个包的实测耗时。
- 完整做法：`make test-race`（或类似）带足超时，明确包清单与 CI 触发策略。

### 验收条件

- 按规约逐字执行提交前检查，不会再出现因默认超时而产生的假红。

## T45. 时间线折叠 SQL 只为历史事件行存在

**搁置记录**：2026-10-03 决定暂不修（等时间）。折叠只对 2026-09-17（`b0a1e476`，进度改为状态、只把里程碑记成事件）之前写下的历史行起作用；事件保留期默认 180 天（`DeploymentPolicy.EventRetentionDays`），按天数约到 2027-03 才淘汰完，用户调短保留期或触发字节上限会更早。到时（或确认各环境的库里已无该日期之前的事件行）迁回 `todo.md` 并删除折叠 SQL。

**来源**：2026-10-01 store 列清理（`artifacts/archive/2026-10-01-store-typed-columns/plan.md`），ADR 0032 决策 5。

- `internal/server/store/instance_timeline.go` 折叠相邻事件时用 `json_extract(data, '$.state')` / `'$.phase'`。新行只在 state 或 phase 变化时才追加，相邻两行必不相同；折叠只对「进度还是事件」时代写下的历史行起作用。
- `instance_events.data` 的另一种形态（旧行的自由文本）同样只存在于这些历史行里。

### 验收条件

- 确认库里已无折叠能合并的相邻行（或 retention 窗口已覆盖该版本之前的全部数据）后，删除折叠 SQL 与对应测试，`scripts/arch-constraints.txt` 的 `store_sql_json_lines` 降到 0。

## T61. 通用 `PUT /settings/{key}` 只要求登录、不限 key

**搁置记录**：2026-10-08 决定暂不修。平台目前只有管理员用户，`user` 角色没有实际使用者，越权没有可利用的主体。出现非管理员用户（建账号、开放给团队成员只读使用等）时迁回 `todo.md`。

**来源**：2026-10-08 试验场 HTML 预览安全设置评审（`artifacts/archive/2026-10-08-playground-reading-tools/plan.md`）。

### 现状

- `internal/server/api/api.go` 把 `r.Put("/settings/{key}", a.putSetting)` 挂在只要求登录的组里；`putSetting`（`internal/server/api/provider.go`）直接用 URL 里的 key 调 `Store.SetSetting(store.SettingKey(key), ...)`，没有白名单。
- 只有 `terminal` 这个 key 在 handler 里检查了 `claims.Role == "admin"`（`monitor.rules` 原本也检查，2026-10-13 随诊断阈值一起删除，ADR 0043）。
- 因此 `user` 角色可以：打开 `orphan_cleanup`（让所有节点清理不在期望状态里的容器）、改全局 `network` 加速与代理、镜像改写规则、模板目录 URL；也可以绕过 admin-only 的 `PATCH /storages/settings` 直接写 `storage_policy`、`control_plane`；试验场的 `security` 策略（HTML 预览是否允许网络请求）同理。

### 修法

- 把 `PUT /settings/{key}` 改为 `managementAdmin`，只接受设置页实际会写的 key（`network`、`network:*`、`image_registry_rewrites`、`image_registry_mirrors`、`image_community_catalog`、`terminal`、`orphan_cleanup`），其余返回 400 ProductError、有专用路由的在错误里指明；删掉 `terminal`/`monitor.rules` 各自的 admin 检查。
- `provider.go` 已 594 行，改动时先把 settings handler 挪到独立文件。

### 验收条件

- `user` 角色对上述每个 key 及 `storage_policy`/`control_plane`/`security` 的 `PUT` 都是 403；admin 写白名单外 key 是 400；admin 写白名单内 key 行为不变；route inventory 与 `docs/reference/api.md` 同步。

## T66. 平台自动停止调度后不会自动恢复

**搁置记录**：2026-10-15 决定暂不修（MR !404 评审第 1 题）。修它需要新增一个「停调来源」的持久化事实，而「运行时无法使用加速卡时把调度开关设为停止调度、不加锁定字段、接受之后可能再被覆盖」是此前定下的取舍。同一次评审已去掉主要的误停来源：docker daemon 偶发无应答不再报 `gpu_runtime_unconfigured`，agent 沿用 daemon 上一次真正的回答。运维反馈「修好了还得手动开」成为负担时迁回 `todo.md`。

### 现状

- `store.UpdateNodeStatus` 收到 `runtime_gpu_blocked` 且节点可调度时，把 `scheduling` 写成 `cordoned`，并发布 `NodeChangeScheduling` 事件、写一条 Warn 日志。
- 原因消失后，`runtime_gpu_blocked` 清空，`scheduling` 不变：平台无法区分这个停调是自己设的还是运维设的。

### 修法

- 节点上记录平台设停调时的原因码（或一个布尔来源），原因消失且来源是平台时撤销；运维手动操作清除该来源。需要迁移与旧数据测试。

## T67. 实例按卡重排序号时的两个边角

**搁置记录**：2026-10-15 决定暂不修（MR !404 评审第 31 题）。两种情况都需要硬件或探测出现当前未观察到的变化。

### 现状

- `realignInstanceGPUs`：实例绑定的卡在这次上报里缺席时，实例保留旧序号并打 Warn。若同一次上报里另一张卡前移到这个序号，账本按序号计数，会把两个实例算到同一张卡上。
- `accelerator.CardIdent` 优先用 BDF、没有才用 UUID。同一张卡若一次上报带 BDF、另一次不带，ident 在 `pci:` 与 `uuid:` 之间翻转，`reconcileGPUs` 把它当成新卡，不置 `renumbered`，绑定它的实例不会被重排。

### 修法

- 重排时对未匹配的绑定再按 UUID 匹配一次；绑定的卡缺席时清掉实例的旧序号并告警，而不是保留可能被别的卡占用的序号。

## T68. 外部进程占用的卡只做整卡排除

**搁置记录**：2026-10-17 由 `todo.md` 的 T54 迁入并改号（T54 在本文件已被 `-race` 超时一项占用），决定不修：平台还提供动态 GPU、显存限额共享、sleep/wake 等多种占卡方式，整卡排除是这些功能共存时最稳妥的口径。原条目描述的「评估显示放得下，启动后 vLLM 自检失败、连续 3 次退出」已不会发生：该问题出现时（2026-10-03，gpu3 上两套控制面共用两张卡）还没有外部占用检测。

### 现状

- `store.GPU.HeldOutsidePlatform()` 为 `ForeignProcesses != ""`，`CanTakeWork()` 因此为假；`scheduler/filter.go` 在没有可调度卡时优先报 `gpus_held_outside_platform`。
- 不看外部进程占了多少显存，也就没有「外部占用后用显存上限仍可共享」这一档。

### 修法（若以后要做）

- 放置时把卡上已用显存减去本平台实例的占用，作为外部占用扣掉；评估与启动用同一口径；拒绝原因给出占用量。单测要覆盖「外部占用使整卡放不下」与「外部占用后限额共享仍可放」。
- 前提是外部占用量要稳定可信；两套控制面共用一张卡这类场景下占用会随对方实例起停波动。

## T51. 两个持续很短的读写竞态

**搁置记录**：2026-10-17 由 `todo.md` 迁入，决定暂不修：两个窗口都极短，后果自愈（降级只记一次、多出的副本下一轮对账被排空），没有人实际撞到过。再出现或规模变大时迁回。

**来源**：2026-10-02 MR !238 评审。

- **PD 快照滞后**：`pd_route.go` 的 `wantsPrefill` 只知道路由快照建立时已经睡着的 prefill。快照建好后才下令休眠的 prefill 仍在 `r.prefill` 里，
  `pickPrefill` 经 `awake` 把它排除后返回 `pdFallbackNoPrefill`，记成一次降级；`fallback=fail` 时该请求被拒，而不是交给 decode。
  持续到事件驱动的快照重建，通常不到 1 秒。修法：`leaseUntil` 因休眠排除了全部 prefill、且模式允许 prefill 为空时，像 `wantsPrefill` 一样跳过 prefill 阶段。
- **对账读取不在一个读事务里**：`reconcileDeployment` 先读部署（`replicas=3`），再读实例；其间 API 提交缩容（`replicas=2`，实例 X 进入 `draining`）时，
  控制器看到一个空位并补起一个新副本，下一轮发现多余再把它排空，白占一次卡。修法：部署与实例在同一个读事务里读，或 `CreateInstances` 在事务内复核部署的 `replicas`。

### 验收条件

- 两种竞态各有可重复的单测（注入快照滞后、在两次读取之间提交缩容），断言不降级、不多起副本。

## T75. 模型下载跑不满高带宽节点

**搁置记录**：2026-10-20 决定暂不修。需要时绕开：先在节点上用多并发的 `hf download` 下到本地目录，再在平台里按路径登记该模型。

**来源**：2026-10-19 前沿模型模板真机验收（`artifacts/work/2026-10-19-frontier-model-templates/validation.md`）。

- agent 逐个文件下载，每个文件 4 条 range 连接；多分片大模型的总速度被单文件吞吐卡住。
- 同类云节点上，平台下载 185 GB 用了约 70 分钟；改用 32 并发的 `hf download` 时，510 GB 只要 1 分 23 秒。

### 建议

- 文件级有界并发，并发数随文件数、节点带宽与剩余磁盘调整；单文件分段数可配置。
- 保持现有进度上报、断点续传与校验语义；拉取代理与站点规则同样适用。

### 验收条件

- 万兆级节点上下载数百 GB 的多分片模型时，吞吐接近链路带宽或源站上限，而不是单文件上限。

## T80. 拉镜像失败时错误行排在进度条之后

**搁置记录**：2026-10-20 按体验问题搁置：原因在完整消息与 agent 日志里都看得到，只是排在进度行之后。

**来源**：2026-10-20 在阿里云 L20 上验收 PLaMo 2 Translate（`artifacts/archive/2026-10-20-openai-completions/`）。

- agent 把 `docker pull` 输出的最后 4 KiB 拼进实例消息（`internal/agent/runtime/docker/docker.go`），多层并行下载时这 4 KiB 几乎全是进度行，真正的错误行（`Error response from daemon: …`）在最末尾。消息不会被截断，原因还在，只是被进度行淹没。
- 最初记录时以为原因被截掉了，核实后不是：截断 500 字节的 `boundedPullMessage` 只用于镜像页的拉取记录，部署实例消息不经过它。

### 建议

- 生成消息时去掉进度行，优先保留错误行。

### 验收条件

- 网络不通、鉴权失败、磁盘满三种失败，实例消息的第一行就是对应的错误原因。

## T102. udocker CLI 路径「从节点删除」镜像必然失败

**搁置记录**：2026-10-21 由 `todo.md` 迁入，决定暂不修：只影响关闭 native 拉取的 udocker CLI 路径（`GPUWORKS_UDOCKER_DISABLE_NATIVE=1`），平台的 udocker 节点都用 native（runveil）。

**来源**：2026-10-20 nerdctl 拉取修复的评审复验（`artifacts/archive/2026-10-20-t73-nerdctl-pull-progress/evidence/real-machine-review-2026-10-20.md`）。

- 只影响关闭 native 拉取的 udocker 节点（`GPUWORKS_UDOCKER_DISABLE_NATIVE=1`）。`udocker.Driver.RemoveImage` 走 CLI 时只传仓库名（`imageRepository`），实际执行的是 `udocker rmi registry.zstack.ai/library/golang`，没带 tag。udocker 按 `latest` 去找，报 `Error: deleting image`，接口返回 409，镜像留在节点上。

### 建议

- 传 `repo:tag`（digest 引用按 `udockerCLIImage` 换成 CLI 实际拉取的 `repo:tag`）。

### 验收条件

- CLI 路径拉取的镜像能从节点删除；测试用忠实的 udocker CLI 夹具断言 `rmi` 的参数带 tag。

## T103. udocker CLI 路径拉取还没完成，节点镜像行就显示「已存在」

**搁置记录**：2026-10-21 由 `todo.md` 迁入，决定暂不修：只影响关闭 native 拉取的 udocker CLI 路径（`GPUWORKS_UDOCKER_DISABLE_NATIVE=1`），平台的 udocker 节点都用 native（runveil）。

**来源**：同 T102。

- udocker 1.3 先写镜像清单（`repos/<仓库>/<tag>/manifest`），再下载各层。复验中 CLI 拉取在层下载开始时被暂停，18 秒后节点镜像行变成 `present`（变化前 agent 连续执行了几十次 `udocker images`，推测是盘点把只有清单的镜像当成已存在，未进一步核实）；3 分钟后 agent 判卡住、拉取失败，镜像行仍是 `present`。
- 只影响关闭 native 拉取的 udocker 节点。

### 建议

- 盘点时只认层文件齐全（大小与清单一致）的镜像，或在本节点有进行中的拉取时不按盘点改写该镜像的状态。

### 验收条件

- CLI 拉取卡住或失败后，节点镜像行不是 `present`；测试夹具还原「清单先于层落盘」。

## T107. 用量分析按卡型筛选逐卡列出时，漏掉拔掉的旧型号卡

**搁置记录**：2026-10-24 确认、决定暂不修：只在「节点还在、拔掉的卡是某型号、节点上已没有这种型号的卡」时出现，代价与收益不相称。

- GPU 利用页按卡型筛选、覆盖不超过 256 张卡时逐张卡列出。为了不读整个集群的卡统计，逐卡查询只读「现在有这种卡的节点」和「时间范围内删除的节点」。
- 节点还在、但其上的 A100 被拔掉、现在只剩 H100 时，按 A100 筛选不会读这个节点，旧 A100 卡不在逐卡列表与按单卡的热力图里。汇总行（按卡型、按节点）读的是汇总统计，仍算它，于是逐卡合计与汇总差这一张卡。
- 节点被删除的情况不受影响（按删除时间补读）。

### 建议

- 只按卡型筛选时，逐卡查询改为按卡型直接定位卡统计行（需要卡型列上的索引，并评估 1000 节点 × 8 卡、400 天按天时的查询代价），或者在统计入库时为「本时间范围内出现过的节点 × 卡型」维护索引。

### 验收条件

- 节点上换掉某型号的全部卡后，按旧型号筛选的逐卡列表含旧卡，逐卡合计等于汇总；fleet 规模用例下查询耗时不退化。

## T108. agent 状态目录在宿主 `/tmp` 下时，udocker 工作负载找不到宿主路径下的库

**搁置记录**：2026-10-26 确认、决定暂不修：只在状态目录装在宿主 `/tmp` 下时触发，默认安装与云厂商一键安装都不会这样放。

**来源**：2026-10-26 阿里云 L4 `TestAliyunTwoVMUdockerDistributedInference` 真机排查。托管控制面夹具原先把 agent 状态放在 `/tmp/gpuworks-hosted-control-<run>/agent-rankN`，引擎报 `libcudart.so.12.8.90: No such file`。

- runveil/udocker 给容器绑定一个私有目录作为 guest 的 `/tmp`（`workload_tmp.go`）。
- `ensureHostRootLoopback` 在容器 `ROOT` 下按宿主绝对路径建回环链接，状态目录在宿主 `/tmp` 下时，链接就落在 `ROOT/tmp/...`，被上面的绑定盖住。
- 引擎按 `/proc/self/maps` 里的宿主路径打开库时解析不到。文件在容器 `ROOT` 里存在，只是 guest 看不到。
- 生产安装的状态目录默认是 `/opt/gpuworks`，不受影响；只要有人把状态目录装在 `/tmp` 下就会触发。测试夹具已改为在阿里云上用 `/opt`。

### 建议

- 二选一：安装与 agent 启动时拒绝（或警告）位于 `/tmp` 下的状态目录；或让回环链接避开被私有绑定覆盖的目录。

### 验收条件

- 状态目录位于宿主 `/tmp` 下时，要么安装前明确报错，要么 udocker 工作负载能正常打开宿主路径下的库；有对应的离线测试。


## T109. Strata 的宿主机内存不参与准入

**搁置记录**：2026-11-12 review 确认暂不在本次 Strata 目录发布中扩展内存准入逻辑。当前可通过人工核对资源并限制部署节点规避；若需要自动放置到混合内存节点，应另行设计宿主机内存准入与记账。

**现象与原因**：Strata 的 placement 只要求每张分配的 GPU 至少有 12 GiB 显存，不检查宿主机可用内存，也不预留容器内存。固定版本 setup 对 IQ2_XS、Q2_0、IQ3_XXS、IQ3_S 的内存建议分别为 48、48、60、62 GB；这些是准备阶段的建议，不是实测峰值或 GPUWorks 准入条件。低内存节点可能被选中，然后在 setup 阶段失败，或在准备、加载和运行时产生内存压力。

**规避方式**：部署前核对节点当前可用内存与容器内存上限，为上下文、准备过程及其他进程留出余量。给满足所选量化要求的节点加标签，部署时据此筛选并手动选择节点，避免自动放置到低内存节点。标签仅帮助运维识别合适节点，不会触发内存预留，也不能防止其他任务消耗内存。PLE RAM 模式还需额外检查内存和 memlock 条件。
