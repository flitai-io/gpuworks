# ADR 0066: agent 的状态按所有者与生命周期成组，组合根只做接线

日期：2026-11-15 · 状态：已接受

## 背景

`internal/agent/app` 一个包有 89 个文件、15.2k 行（非测试）；`NodeRuntime` 有 77 个字段（含匿名 struct 内的）、12 把互斥锁、233 个方法，`OnRPC` 连同它转交的辅助 switch 分发 51 个 RPC。镜像、模型文件、卡健康、doctor 都挂在这个结构体上、共用它的锁，任一功能的改动都要改它，子系统不能单独测试或替换。

`internal/agent/engine` 把一个实例的状态放在 12 张按 UUID 索引的并行 map 里，每条退出路径各删一部分。描述引擎进程的四张（睡眠级别、预热、L1 主机缓冲、卡上进程）只在容器自己退出时清除，server 正常移除或停止实例时不清，容器死掉后被下一次同步直接替换时也不清。实测（GWKS-321）：同一 UUID 的新容器被上报持有旧进程的 L1 主机缓冲，server 因此对它跳过主机内存检查；新进程不再预热；期望睡眠的实例换了新进程后，agent 把旧进程的睡眠当成新进程的，新引擎醒着占满显存，server 却以为它在睡。

server 侧的 controller 已经按同样的思路整理过：每类记忆一个自带锁的类型（GWKS-304），每个字段由测试要求写明是否按部署清理，并由另一个测试真的写入、清理、断言为空（GWKS-303）。本 ADR 把它定为 agent 的写法。

## 决策

1. **按所有者与生命周期成组。** 同一个键（实例 UUID、模型文件、镜像引用……）、在同一时刻失效的状态放进一条记录（`struct`），由一张 `map[key]*record` 持有，不再为每个事实各开一张并行 map。一个实例的状态按生命周期分记录：期望的那一代（desired）、一次对账尝试（in-flight、pending、cancel）、一个引擎进程（`processFacts`）。
2. **记录的边界就是它描述的东西的边界。** 引擎进程的记录在创建容器的地方从空开始，在确认容器已删除的地方（`reclaimInstance`）与切换运行时时整条丢弃；不在各条退出路径上各删一部分。从空开始的那一端兜住了所有不经 `reclaimInstance` 的替换（容器死后被同步直接替换、切换运行时）。新的事实加进它所属的记录，就随记录一起清理。写入记录的代码在该 UUID 的对账尝试之内（inFlight 串行化）；移除等尝试结束后才丢弃，所以丢弃后不会被旧尝试写回。
3. **每个字段必须分类，按实例的清理由测试证明。** `TestEveryEngineFieldIsClassified` 列出 `Engine` 的每个字段：按实例的写明 server 移除实例时是否丢弃，其余写明是依赖、配置、锁还是全局计数；按字符串索引的 map 只能归为按实例。`TestRemovingAnInstanceLeavesNothingUnderItsUUID` 让一个睡眠中的实例在各张表里都留下条目，移除后断言每个标为丢弃的字段都不再持有它。这与 controller 的 `TestEveryControllerFieldIsClassified` 加 `TestForgetDeploymentLeavesNoMemory` 是同一写法。新增字段在分类之前、或标为丢弃却没清理时测试失败。
4. **子系统自带状态与锁，依赖收窄。** 镜像、模型文件、卡健康、doctor 各成一个包或类型，持有自己的字段与锁，依赖只给它需要的接口（driver、reporter、storage guard 等），零值或构造函数即可单独测试。`NodeRuntime` 是组合根：每个子系统一个字段，加上连接与生命周期的接线。
5. **共享状态归组合根，锁不跨子系统。** 几个子系统都要读的配置（网络配置 `netcfg`、模型目录、registry 认证与 puller）由组合根持有，以只读的 getter 或快照交给子系统，子系统不持有别人的配置锁。子系统之间不互相取锁：要查另一个子系统的表（peer relay 同时查模型同步与镜像同步），调用对方暴露的窄方法，由对方在自己的锁内回答。任何代码都不同时持有两个子系统的锁。
6. **RPC 是表，不是 switch。** `OnRPC` 改为「方法名 → handler」表，handler 由各子系统提供；`NodeRuntime` 只负责查表与公共的错误处理。
7. **棘轮守护。** `scripts/arch-constraints.txt` 的 `agent_node_runtime_fields`、`agent_node_runtime_mutexes`、`agent_node_runtime_methods`、`agent_onrpc_cases`、`agent_engine_struct_maps` 只降不升（初值 77 / 12 / 233 / 51 / 9），放宽必须写 `# raised:` 说明理由。测量用 `scripts/structcount` 解析语法树：一行多个名字按名字计，指针锁、`sync.Map` 与包内的 map 类型都计，匿名 struct 里的照样计（包一层不算离开），`OnRPC` 转交给辅助 switch 的 RPC 也计；只有移进具名子系统类型的才算离开。方法数守护的是行为：字段搬走了，方法还挂在 `NodeRuntime` 上直接碰子系统的状态，不算拆开。

## 结果

- 第一步（GWKS-321）：引擎进程的四张 map 收成 `processes map[string]*processFacts`，创建容器时从空开始，在 `reclaimInstance` 与切换运行时整条丢弃；`Engine` 的 map 字段 12 → 9。同一 UUID 的新容器不再继承旧进程的事实：`TestANewContainerUnderTheSameUUIDKnowsNothingOfTheOldProcess`（server 移除、停止）与 `TestAReplacedEngineProcessIsPutToSleepAgain`（容器死后被同步替换、切换运行时，新进程真的被重新送去睡眠）。
- 后续按子系统逐个抽出：镜像（peer relay 改为经镜像子系统的窄方法读它的同步表）→ 模型文件 → 卡健康与 doctor → RPC 表，每一步单独合并，棘轮随之收紧。

## 代价与不做的事

- `runtimeEpoch` 按 UUID 保留到 agent 进程结束，是有意的：移除时改为一个全局递增的新值，复用的 UUID 永远对不上旧的尝试。代价是每个见过的 UUID 一个 `uint64`。
- 期望那一代与对账尝试的几张 map（`desired`、`inFlight`、`inFlightCancel`、`pending`、`cleanupRequired`）这一步不合并：它们由 reconcile 的串行化协议共同维护，合并要连同那段协议一起改，留给单独的变更。
- `NodeRuntime` 这一步没有字段分类测试：它的字段多是子系统状态，随子系统抽出整体离开，由棘轮守护；按实例清理的测试先只在 `Engine` 上要求。
- 子系统拆分不改变 agent 与 server 之间的协议与行为。
