# 架构约束说明

`scripts/arch-constraints.txt` 的每一行是一条预算：`键<TAB>上限/下限<TAB>方向`。
`max` 表示实测值必须 ≤ 限额（只允许下降），`min` 表示实测值必须 ≥ 限额（只允许上升）。
`scripts/check_architecture.sh` 读取该文件，逐项实测并打印 `键 实测/限额 (方向)`。

规则来自 `artifacts/archive/2026-09-09-architecture-cleanup/plan.md` §3 P0 与 §6：

- 超限即失败（exit 1）。
- `max` 键实测**严格低于**限额时打印 WARNING（不失败），提示在同一个 MR 里把限额收紧到新实测值。
- 误报以**修模式为先**，不以调预算为先。每条排除项都记录在本文；改脚本时必须同步本文。
- **上调（`max` 变大、`min` 变小）必须带理由**：`check_architecture.sh` 把本文件与基线版本
  比较——基线优先取 CI 给出的合并请求基线 `$GPUWORKS_ARCH_BASELINE`，否则取
  `git merge-base HEAD origin/master`；解析不出基线（浅克隆、没有 `origin` remote、当前就在
  default branch 上）时打印 NOTE 并跳过，不算失败。被判定为上调的行必须在行尾追加 Tab 分隔的
  `# raised: <原因>` 注释（`min` 键同一标记），否则直接 FAIL；新增或删除的键不算上调。该注释
  是第 4 个 Tab 分隔字段，不影响 `key<TAB>limit<TAB>direction` 的既有解析。
- **`warn` 是软预算**：实测超过限额只打印 WARNING（附该键的替代做法提示），实测低于限额提示可收紧，
  **从不失败**；基线比较也不检查 `warn` 键。用于不宜硬拦、但需要在评审里被看见的趋势
  （`ui_antd_icon_import_files`、`ui_bundle_gzip_kb`）。收紧或上调 `warn` 限额同样在行尾写
  `# raised: <原因>` 说明上调理由（约定，不由脚本强制）。
- 需要额外前置条件才能测的键（如 `ui_bundle_gzip_kb` 需要先构建 UI），在 `measure()` 里返回
  `EXTERNAL`，预算循环只打印并跳过，由专门脚本测量。

初值取 2026-09-09 `refactor/refactor-2` 分支上的实测结果，不是计划正文里的人工估算值。

公共 provider 主线另有不可豁免的依赖方向检查：`provider` 根包和 `controller` 不得导入具体 provider 子包；`app` 只允许导入 `provider` 根包和注册入口 `provider/all`。具体驱动的初始化、节点归属规则及后台业务必须保留在实现模块内，不通过搬到公共包绕过这条边界。

---

## 逐项说明

下表的"命令"是脚本 `measure()` 中该键的实测逻辑（此处按可直接粘贴运行的形式书写）。
所有命令都在仓库根目录执行。

### pending_removal_literal（初值 14）

意图：`"pending-removal"` 是真实的实例状态但不在 `proto.Inst*` 枚举里（计划 A3）。
收敛后应为 0。

```sh
grep -rn '"pending-removal"' internal --include='*.go' | grep -v '_test\.go' \
  | grep -v '^internal/proto/instance\.go:' | grep -c .
```

排除：`*_test.go`（测试断言字面量是合理的）；`internal/proto/instance.go`（`InstPendingRemoval`
常量的定义处，即唯一产地——预算的意思是"proto 之外没有字面量"，把定义本身计进去会让 0 永远不可达）。

### deployment_state_literal（初值 8）

意图：`Deployment.State` 无枚举，比较与写入都用裸字符串（计划 A1）。

```sh
{ grep -rnE 'dep(loyment)?\.State (==|!=) "[a-z-]+"' internal --include='*.go' | grep -v '_test\.go'
  grep -rnE 'SetDeploymentState\([^)]*"[a-z-]+"' internal --include='*.go' | grep -v '_test\.go'; } | grep -c .
```

排除：`*_test.go`。计划正文估的 9 是把两类合并后的手工计数；实测按行去重后为 8（比较 6 处 + 写入 2 处）。

### engine_literal_server（初值 21）

意图：引擎名有注册表却仍有 `"vllm"` / `"sglang"` 字面量散落（计划 A7）。计数单位是**文件数**，
不是出现次数——目标是让引擎名只在注册表所在文件出现。

```sh
grep -rlE '"vllm"|"sglang"' internal pkg cmd --include='*.go' \
  | grep -v '_test\.go' \
  | grep -v '^internal/engines/engines\.go$' \
  | grep -v '^internal/server/deploy/engine\.go$' \
  | grep -v '^pkg/enginelog/' | grep -c .
```

排除：

- `*_test.go`。
- `internal/engines/engines.go`：P1 落地后引擎名的唯一产地（纯叶子包，无 import）。
- `internal/server/deploy/engine.go`：注册表本身，其 `EngineVLLM` 等常量是对 `internal/engines` 的再导出。
- `pkg/enginelog/**`：`pkg` 不能依赖 `internal`，方言名必须自带（计划 P1 任务 4）；
  两份名单由 `internal/server/deploy/engine_names_test.go` 断言相等。

计划正文估的 20 未含 `cmd/`；实测把 `cmd/` 一并纳入后为 21。

P1 变更：`internal/server/deploy/sglang.go` 的排除项已**删除**——该文件已全部改用 `EngineSGLang`
常量，不再需要豁免；同时新增 `internal/engines/engines.go` 排除项（新的唯一产地）。

P1 收敛后实测 2，是这两个文件：

- `internal/server/imagecatalog/pullref/pullref.go`：注释里用 `"vllm"` 举例说明 Docker Hub
  仓库路径的分段规则；
- `internal/agent/runtime/udocker/udocker.go`：注释里用 `["vllm", "serve"]` 举例说明 argv 前缀。

两处都在注释中，都不是引擎名选择逻辑，也都没有加排除项——预算停在 2 而不是 0，是因为
本条计数单位是**文件**且这两个文件确实含该字符串。若将来要归零，应改写这两句注释的措辞
（例如换成占位符），而不是加排除项。

### engine_literal_ui（初值 15）

意图：前端硬编码引擎列表，应改读 server `/engines` 下发的字典（计划 A7、C5）。计数单位是**出现次数**。

```sh
grep -rnE "'vllm'|\"vllm\"|'sglang'|\"sglang\"" ui/src --include='*.ts' --include='*.tsx' \
  | grep -v '\.test\.' | grep -v '^ui/src/i18n/' | grep -c .
```

排除：`*.test.*`；`ui/src/i18n/**`（引擎名作为展示文案出现在 locale 里是合理的）。

### ui_antd_import_files（迁移基线 349，2026-11-09）

意图：antd 与 Zeron 并存期间，引用 antd 的源文件数只降不升（迁移收尾清单见 `docs/dev/ui.md`）。计数单位是**文件**，
含 re-export、动态 import 与 `antd/es/...`、`antd/locale/...` 子路径。

```sh
grep -rlE "['\"]antd(/[^'\"]*)?['\"]" ui/src --include='*.ts' --include='*.tsx' \
  | grep -v '\.test\.' | grep -c .
```

排除：`*.test.*`。降到 0 时脚本额外提示加 `ui_raw_color` / `ui_raw_radius` 规则。

文件数只能拦"新文件引入 antd"，拦不住已有文件里继续加 antd 用法；逐文件的棘轮由下面的
`ui/antd-inventory.json` 负责。

### ui_copy_long_strings / ui_copy_caveat_strings（基线 0 / 0，2026-11-13）

意图：页面只写事实和下一步，公式、口径、免责放进 `ui-kit/HelpTip` 或文档（`docs/dev/ui.md` "Copy on the page"）。
`ui_copy_long_strings` 数中文 locale 里超过 120 字（key 以 `Help` 结尾的 HelpTip 内容放宽到 200 字）的字符串；
`ui_copy_caveat_strings` 数非 `Help` key 中带免责短语（不保证、仅供参考、不代表、未经实测、并不意味、不改变、仅用于界面演示）的字符串。只降不升。

```sh
node scripts/ui_copy_audit.mjs long     # 或 caveat；list 逐条列出
```

排除：无。超长说明改写或移进文档；"不包含模型文件"这类事实不算免责，正则不收。

### ui_untested_shared_files（基线 0，2026-11-11）

意图：每个页面都会复用的 UI 构件各有自己的单元测试，改它的人改得到对应测试，新加的构件不会没有测试就进来。范围：
`ui/src/ui-kit/**`、`ui/src/app/**`、`ui/src/components/detail/**`、`ui/src/components/resourceTable/**`，
以及 `ui/src/components/PageHeader.tsx`、`EmptyState.tsx`。`ui/tests/` 下某个测试直接 import 它，或经真实宿主驱动它（侧栏经 App、列筛选经 ResourceTable）并在注释写明其 `ui/src/...` 路径，才算有测试；
只被页面测试顺带渲染不算。超出时脚本列出缺测试的文件。

```sh
# 见 check_architecture.sh 的 untested_shared_ui：对范围内每个 .ts/.tsx，
grep -rqE "['\"](\.\./)+<去掉 ui/ 与扩展名的路径>(\.tsx?)?['\"]|ui/<同一路径>\.tsx?" ui/tests --include='*.ts' --include='*.tsx'
```

排除：无。不再使用的构件直接删掉，不为它补测试。

### antd 逐文件棘轮（`ui/antd-inventory.json`，硬检查，非预算行）

`check_architecture.sh` 在预算循环后运行
`node scripts/ui_antd_inventory.mjs --check ui/antd-inventory.json`，与提交的快照逐文件比较，以下任一情况 FAIL：

- 某文件的 antd 使用点（sites）变多；
- 某文件新引入了快照里没有的 antd 名字（含 `import type`）；
- 某文件的 `@ant-design/icons` 引入数变多，或 CSS 的 `.ant-*` 规则数变多；
- 快照里没有的文件开始使用 antd / `@ant-design/icons` / `.ant-*` 选择器。

减少一律通过（打印 NOTE 提示刷新快照）。迁移 MR 在同一提交里运行
`node scripts/ui_antd_inventory.mjs --write` 刷新快照；快照每个文件一行，diff 只显示动到的文件。
机器上没有 `node` 时打印 NOTE 并跳过（CI 的 `core-test` 跑在 golang 镜像里，需在有 node 的 job 里执行）。

计量口径（`scripts/ui_antd_inventory.mjs`，只用 Node 内置模块）：

- 范围：`ui/src` 下 `*.ts(x)`、`*.js(x)`、`*.css`；排除 Zeron 托管代码 `src/components/ui/`、`src/lib/`、
  `src/hooks/use-*`，以及 `*.test.*` 与 `src/i18n/`。
- 使用点 sites = JSX `<Name` / `<Name.Sub` + 静态调用 `name.fn(`（`message.success(`、`Modal.confirm(`、
  `Form.useForm(`），按 import 的本地别名匹配；`const { Text } = Typography` 解构出的名字记到 `Typography`。
- 只识别行首注释（`//`、`/* */`、`{/* */}`），行尾注释里的 `<Name` 会被计入；作为值传递
  （`component={Table}`）、仅用于类型位置的名字不计 sites，但计入 imports/types，新增照样被拦。
- `@ant-design/icons` 按引入的名字数计；CSS 按选择器含 `.ant-` 的规则块计（`@media` 内的规则同样计）。

另有页面归属（`--json` 的 `attribution`、`--affected <file...>`、`--base <ref>`），供浏览器检查按改动文件
挑页面重查：`App.tsx` 的 `<Route path element={<X`> 解析出路由元素模块，按 `navigation.tsx` 菜单项
（含 `also`）的首段路径归到菜单 key；某文件归属每个能经 import 图（静态、re-export、`import()`、CSS `@import`）
传递到达它的页面。从 `main.tsx` / `App.tsx` 外壳（不走 App 到路由元素的边）能到达的文件记为 `shared`，
改动时输出 `["*"]`；`styles.css`、`app.css`、`theme/`、`main/App/navigation.tsx` 与 UI 构建配置同样输出 `["*"]`。
已知缺口：`App.tsx` 全局引入的页面 CSS（如 `pages/batches/batches.css`）因此记为 `shared`；
不被任何页面引用的文件（死代码）输出空；不在 import 图里的现存非 md 文件（如静态资源）保守输出 `["*"]`。

### ui_antd_icon_import_files（软预算 warn，初值 103，2026-11-09）

意图：外壳与新页面的图标统一走 `ui/src/ui-kit/consoleIcons.ts`（HugeIcons），`@ant-design/icons` 的引用
只减不增。软预算：超出只告警（提示改用 `consoleIcons.ts`），不失败；新文件引入 antd 图标由上面的逐文件棘轮硬拦。

```sh
grep -rlE "['\"]@ant-design/icons(/[^'\"]*)?['\"]" ui/src --include='*.ts' --include='*.tsx' \
  | grep -v '\.test\.' | grep -v '^ui/src/components/ui/' | grep -c .
```

排除：`*.test.*`；`ui/src/components/ui/`（Zeron 托管代码）。

### ui_bundle_gzip_kb（软预算 warn，初值 2450，2026-11-09）

意图：内嵌 Web UI 的发布体积可见。由 `scripts/check_ui_bundle_size.sh` 在 `make ui` 末尾测量
（`check_architecture.sh` 不构建 UI，跳过该键）：

```sh
tar -C internal/server/webui/dist -czf - . | wc -c   # 字节，向上取整到 KB
```

与 `scripts/package_release.sh` 打包方式一致。超出只告警并列出最大的 5 个资产（原始 / gzip KB），从不失败。
初值为 2026-11-09 实测 2400 KB（macOS bsdtar，含其 10 KiB 块填充；GNU tar 略小）向上取到下一个 50 KB。

### setting_key_literal（初值 27）

意图：设置键无常量，`GetSetting("…")` / `SetSetting("…")` 字面量散落多包（计划 A9）。

```sh
grep -rnE '(Get|Set)Setting\("' internal --include='*.go' \
  | grep -v '_test\.go' | grep -v '^internal/server/store/settings_keys\.go:' | grep -c .
```

排除：`*_test.go`；`internal/server/store/settings_keys.go`（P1 新建的常量文件，是唯一产地）。

P1 收敛后为 0：`GetSetting` / `SetSetting` / `DeleteSetting` 三者的形参类型都改成了
`store.SettingKey`，字面量在编译期就过不去。唯一允许未定型键进入 store 的位置是
`/settings/{key}` 通用路由（`api/provider.go`），那里显式写 `store.SettingKey(key)` 做边界转换，
不匹配 `(Get|Set)Setting\("` 因而不计数——这是有意的：路径参数本来就来自 HTTP，不可能是常量。

### vendor_normalize_funcs（初值 2）

意图：vendor 规范化有两份实现（计划 A4）。目标 1。

```sh
grep -rnE 'func (normalizeVendor|NormalizeVendor)\(' internal pkg cmd --include='*.go' \
  | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。

### infer_vendor_callers（基线 4，目标 0，当前 0）

意图：`deploy.InferVendor` 已迁入 `internal/accelerator/infer.go`（`accelerator.InferVendor`），`deploy.InferVendor` 函数定义已删除。
该预算现在度量的是 `deploy.InferVendor(` 调用点的残留数量——目标仍是 0；函数本身仍然存在于 accelerator，调用点统一以 `accelerator.InferVendor(` 形式，不计入此预算。

```sh
grep -rn 'deploy\.InferVendor(' internal pkg cmd --include='*.go' | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。原 4 处调用（`api/deployments_evaluate.go`、`api/deployments_node_helpers.go`、`api/deployments_chipmatch.go`、`controller/schedule.go`）已全部改调 `accelerator.InferVendor`。

### store_list_nodes_callers / store_list_deployments_callers / store_list_instances_callers（初值 23 / 10 / 5，2026-11-14；ListNodes → 11，节点快照；→ 9，控制器对账；ListDeployments → 9、ListInstances → 4，列表快照；→ 6 / 1，状态记忆）

意图：`store.ListNodes()`（每个节点的整行加全部卡）、`ListDeployments()`、`ListInstances()`（整表整行）不再是各层默认的读法。只要节点的标识、状态、标签或一个计数的读用已有窄读（`NodeIdentities`、`NodeUUIDsByID`、`NodesForAgentUpgrade`、`CountNodes`）；需要全机群整行的热路径（列表、仪表盘、对账）由机群快照统一读一次（架构评审 R-32）。预算只降不升；新增调用点先问能否用窄读或快照。

```sh
grep -rnE '\.ListNodes\(\)' internal pkg cmd --include='*.go' | grep -v '_test\.go' | grep -v '^internal/testenv/' | grep -c .
grep -rnE '\.ListDeployments\(\)' internal pkg cmd --include='*.go' | grep -v '_test\.go' | grep -v '^internal/testenv/' | grep -c .
grep -rnE '\.ListInstances\(\)' internal pkg cmd --include='*.go' | grep -v '_test\.go' | grep -v '^internal/testenv/' | grep -c .
```

排除：`*_test.go` 与 `internal/testenv/`（进程内测试夹具）。`\(\)` 只匹配无参调用，云厂商驱动的 `ListInstances(ctx, …)` 不计入。初值 `ListNodes` 39 → 23：`/healthz` 改用 `CountNodes`；放置选择（`LoadUnitSuggestFacts` 的调用方）不再为构造回退的在线判断先读一遍；存储、镜像、节点同步、镜像引用推送、agent 升级明细、部署运行时状态推导改用窄读。`ListNodes` 23 → 11：节点列表、筛选项、仪表盘、存储用量与节点探测、监控清单、`/gpus`、调度中心概览与看板、部署候选池、用量分析、拉取规则计数、网络状态、`NodesForStorage` 改读 `store.FleetNodes()`（节点快照，`store/fleet_nodes.go` 内部的 2 处调用计入）。`ListDeployments` 10 → 9、`ListInstances` 5 → 4：部署与实例列表改读 `deployment.Lists`（列表快照）；按请求读的回退也收进 `deployment.ReadListRows`，两条路径共用 `list_snapshot.go` 里的一处整表读。`ListDeployments` 9 → 6、`ListInstances` 4 → 1：仪表盘、监控清单与批处理模型列表改读同一份快照及其 `ListRows.States()`（每份快照只算一次全部部署的状态）。剩下的 `ListDeployments`：快照内部、启动时的启动配置规整与缓存拷贝认领、下载依赖索引、试验场、PD 校验；`ListInstances` 只剩快照内部。`ListNodes` 11 → 9：控制器的池副本放置、节点失联替换（`anotherNodeMayRun`、`planning.LoadUnitSuggestFactsFromSnapshot`）改读 `FleetNodes()`；控制器每批状态上报入队前先 `RefreshFleetNodes()`。运维者的交互式选择（部署评估 `evaluateEligibleNodes`、`rankEligible`、`CandidateNodes`，扩缩、建议与自动放置经 `planning.LoadUnitSuggestFacts`）保留直读：它们要按 agent 刚发来的上报回答，快照的 2 s 上报窗口会让刚报的磁盘不足在评估里晚 2 s 出现（`TestExistingModelDirectoryCanBeRegisteredAfterDiskRefusal`）。剩下的：快照内部 2 处、上述 4 处交互式选择、诊断包、网络测试、agent 升级预览。

### store_concrete_importers（初值 17）

意图：`*store.Store` 具体类型贯穿全服务端，无窄接口（计划 B6）。

```sh
go list -e -f '{{.ImportPath}}{{"\t"}}{{join .Imports " "}}' ./... \
  | grep -v '\.test$' \
  | awk -F'\t' '$2 ~ /gpuworks\/internal\/server\/store( |$)/ {print $1}' | sort -u | grep -c .
```

排除：`.test` 伪包（`go list` 为带测试的包生成的合成包）。
计划正文列了 16 个包；实测 17，多出的是 `scripts/seeddemo`（同属 module，计划正文漏计）。

`go list` 说明：本仓库缺少 `internal/server/webui/dist` 时 `go list` 会因 `go:embed all:dist`
匹配不到文件而报错，因此脚本一律用 `go list -e`（保留可用的包信息，只把出错的包降级）。
`internal/server/webui` 自身不 import 任何一方包，降级不影响任何一项实测。这样 CI 与本地
都无需先构建前端即可跑门禁，也不需要临时造占位文件。

2026-09-29：ZStack Cloud 资源服务（`internal/server/provider/zstack/cloud`）负责持久化主机资源快照与资源租约，
新增对 `store` 的依赖；`store_concrete_importers` 上限由 18 调整为 19。

2026-10-17：资源占用职责独立为 `provider/zstack/cloud/resourceclaim`。父包继续负责主机接入与快照保存，
子包读取节点/GPU 并管理资源占用；实际持久化职责未扩大，但包级直接导入数从 19 增加到 20，
相应调整上限。公共 provider/controller 的接口和依赖不变。

2026-10-31：调度活动（`docs/reference/api.md` 的「调度活动」）。新包 `internal/server/schedactivity` 按 store 交出的事件
（`store.ObservedEvent`，事件写入后由 `store.SetEventObserver` 回调）在内存里累加最近 24 小时，并从 store 回放与读取部署名，
`store_concrete_importers` 21→22；`api_deps_fields` 56→57（`Deps.SchedulingActivity`，调度活动路由、共卡部署统计与节点时间线
共读的这份内存汇总，没有对应的 store 行）。

2026-11-12：部署用例（用例层拆分的第一组）。新包 `internal/server/deployment` 承接部署的创建、评估与读模型（行查找、运行时状态
推导、模型准备观测），不依赖 `net/http` 与 `api`（依赖方向检查守护）；`api` 的 handler 只做解码、调用、映射。该包按 store 写入
与读取，`store_concrete_importers` 22→23；`api` 仍为其余 handler 依赖 store，故没有随之减少。

### store_db_accessor_callers（初值 0，2026-11-14）

意图：store 之外的生产代码不拿原始 `*gorm.DB`。`(*store.Store).DB()` 只留给测试；
其他包经 `Store` 的方法读写，ZStack Cloud 的主机与租约台账经 `(*store.Store).CloudLedger()`。

```sh
grep -rnE '\.DB\(\)' internal cmd pkg scripts test --include='*.go' \
  | grep -v '_test\.go' | grep -v '^internal/server/store/' | grep -c .
```

排除：`*_test.go` 与 `internal/server/store/` 自身（store 内部对 `*gorm.DB` 取连接池的 `gdb.DB()` 不算）。
模式按文本匹配任何 `.DB()`，所以 store 之外直接持有 gorm 再取 `*sql.DB` 也会计入——那同样是绕过 store，计入是有意的。
建立时实测 0：ZStack Cloud 的三处调用改走 `CloudLedger()`，`scripts/seeddemo` 改为按节点接入的真实路径写入
（`RegisterNode` + `UpdateNodeStatus`，拓扑以 `nvidia-smi topo -m` 文本给出）。

### Controller、planning、app 三个枢纽（2026-11-14，初值取当日主线 0a0b5fa6c 实测）

意图：这三个包是每个功能都要改的地方（架构评审 R-20）。拆分工作见总单 GWKS-313（子单 GWKS-303 至 GWKS-312）；
这组预算先把现状钉住，拆分落地时在同一个 MR 里把对应限额收紧到新实测值。

| 键 | 初值 | 方向 | 守护什么 |
|---|---|---|---|
| `controller_struct_fields` | 39 | max | `Controller` 结构体的字段行数 |
| `controller_struct_mutexes` | 5 | max | 直接放在 `Controller` 上的锁 |
| `controller_struct_maps` | 6 | max | 直接放在 `Controller` 上的 map |
| `controller_exported_methods` | 18 | max | `*Controller` 的导出方法（API 操作挂在 Controller 上） |
| `planning_concrete_store_refs` | 72 | max | planning 生产代码里含 `*store.Store` 的非注释行 |
| `planning_sync_files` | 2 | max | planning 中 import `sync` / `sync/atomic` 的生产文件（包级锁） |
| `app_new_lines` | 300 | max | `app.New` 的行数 |
| `app_run_lines` | 192 | max | `(*App).Run` 的行数 |
| `app_run_goroutines` | 23 | max | `(*App).Run` 里直接起的协程 |
| `app_prod_lines` | 10012 | warn | app 生产代码总行数 |

2026-11-14 合入后校正：GWKS-304（!1154）把 `Controller` 上的 4 组锁和 map 收进自带锁的类型，三项收紧为 34 / 1 / 1；
预算 MR 的 CI 跑完到合入之间，r32-fleet-nodes、r32-list-states、r32-controller-nodes 先合入，`app.New` 增加共享的节点快照与
部署/实例列表快照（300 → 307），planning 新增两个以节点快照为参数的入口（72 → 74，GWKS-309 会把这些读改走窄接口），
`app_prod_lines` 随之到 10043。

GWKS-305：sleep/wake 的顺序锁与放置提示从 planning 的包级全局变量移进叶子包 `internal/server/lifecycle` 的 `Orders`，
由 controller 建、经 `Controller.Orders` 交给 app，再交给 API（`Deps.Orders`）与自动化。状态本身没有增加，只是从隐藏的
全局变量变成显式传递：`planning_sync_files` 2 → 0；`controller_struct_fields` 34 → 35、`controller_exported_methods`
18 → 19、`api_deps_fields` 63 → 64、`api_fanout` 48 → 49、`app_new_lines` 307 → 308，各带 `# raised:` 理由。

结构体与函数的五项由 `decl_count 目录 声明模式 行模式 [body]` 测量：在该包的非测试文件里找到匹配声明模式的顶层声明，
从它数到第一个顶格的 `}`，只数匹配行模式的行（行模式为空时数全部行；`body` 表示不数声明行本身）：

```sh
decl_count internal/server/controller '^type Controller struct' '^[[:blank:]][*A-Za-z_]' body   # fields
decl_count internal/server/controller '^type Controller struct' \
  '^[[:blank:]](\*?sync\.(RW)?Mutex|[A-Za-z_][A-Za-z0-9_, ]*[[:blank:]]+sync\.(RW)?Mutex)' body # mutexes
decl_count internal/server/controller '^type Controller struct' \
  '^[[:blank:]][A-Za-z_][A-Za-z0-9_, ]*[[:blank:]]+map\[' body                                    # maps
decl_count internal/server/app '^func New\(' ''                                                   # app_new_lines
decl_count internal/server/app '^func \(a \*App\) Run\(' ''                                       # app_run_lines
decl_count internal/server/app '^func \(a \*App\) Run\(' '^[[:blank:]]+(goBG\(|bg\.start\(|go[[:blank:]])' # app_run_goroutines
```

其余四项：

```sh
grep -rnE '^func \(c \*Controller\) [A-Z]' internal/server/controller --include='*.go' | grep -v '_test\.go' | grep -c .
grep -rnE '\*store\.Store' internal/server/planning --include='*.go' | grep -v '_test\.go' \
  | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c .
grep -rlE '^[[:space:]]*"sync(/atomic)?"|^import "sync(/atomic)?"' internal/server/planning --include='*.go' \
  | grep -v '_test\.go' | grep -c .
find internal/server/app -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' -exec cat {} + | grep -c ''
```

- 声明在包内换文件仍照常测量；整个声明找不到（改名、搬出包）时 `decl_count` 输出 `NOTFOUND`，预算循环直接失败——
  搬走声明的 MR 必须把测量一起搬走，不能让预算静默变成 0。
- 模式经环境变量传给 awk（`awk -v` 会吃掉反斜杠转义），并用 `[[:blank:]]` 而不是 `\t`（busybox awk 在动态正则里
  不展开 `\t`）。gawk、mawk、busybox awk 实测结果一致。字段、map、锁的行模式要求恰好一级缩进，嵌套结构体的成员不计。
- 已知口径：多名字段（`a, b int`）算一行；`sync.Map`、具名 map 类型、`atomic.*` 不计入锁或 map；
  有多个文件声明同一个名字时（build tag 变体）只量文件名排序的第一个；
  `app_run_goroutines` 只数行首的 `goBG(`、`bg.start(`、`go `，再别名、`wg.Go`、写在同一行的 `if … { go … }` 数不到；它只看 `Run` 本身，`Run` 之外起的协程（如 `automation_adapter.go:56`）由 GWKS-308 收进组件表。
  把 `Run` 的一段搬进辅助方法会同时降低 `app_run_lines` 与 `app_run_goroutines`，这两项防的是 `Run` 变长，不是协程总数。
- 状态收进自带锁的具名类型后（`nodeFailover`、`imageWaits` 等），字段仍计入 `controller_struct_fields`，锁和 map
  不再计入——这正是要的方向。
- 不用 planning 中 import store 的文件数（57）：其中 30 个只用实体类型，压它就要另建一套平行的行类型，
  `store_concrete_importers` 的 `# raised` 已否掉这条路；用 `*store.Store` 引用代替。包级可变变量（如 `placementHints`
  的 map）没有单独的计量，原有的两处由 `planning_sync_files` 覆盖；GWKS-305 把它们移进叶子包 `lifecycle` 后该项为 0。
- 不量 `forgetDeployment` 的行数：缺陷正是它太短（漏清 5 份状态）。改用 `controller_struct_maps` 加 GWKS-303 的完整性测试。
- `app_prod_lines` 是软预算：搬出功能服务会降它，新的装配代码合理地会涨它，只要在评审里被看见。

### api_fanout（初值 36）

意图：`api` 包扇出全仓最高（计划 B1）。计数为 `api` 直接 import 的**本仓库**包数。

```sh
go list -e -f '{{join .Imports "\n"}}' ./internal/server/api \
  | grep '^gpuworks\.ai/gpuworks/' | sort -u | grep -c .
```

排除：标准库与第三方 import。

2026-09-27：网络模块化（`artifacts/archive/2026-09-27-network-module/plan.md`）把网络解析从
`store` 搬到纯叶子包 `internal/server/netplan` 与服务包 `internal/server/network`。三项预算各
抬 1：`store_concrete_importers` 17→18（`network` 服务要读节点与网络行，它承载的解析原先是
`*store.Store` 的方法，调用方仍因其他用途依赖 store，没有随之减少）；`api_fanout` 36→37（api 改为
依赖 `network` 与 `netplan` +2，`nodeaddr` 收进 `planning.PeerHost` −1）；`api_deps_fields`
49→50（`Deps.Networks`，全服务端共享的唯一网络服务实例）。

2026-10-11：发布通道（[ADR 0040](../docs/adr/0040-signed-multi-source-distribution.md)）。`api_fanout` 38→39：检查更新与官方目录更新要读
签名索引，api 直接依赖叶子包 `internal/server/distribution`（它不依赖本仓库任何包，见下文依赖方向检查）。`api_deps_fields`
不变：渠道客户端放进原来的 `Deps.ImageCatalog`（改名 `Deps.Outside`，社区仓库浏览器与渠道客户端同属「从外部读取」），
界面升级 server 用的是已有 `Deps.Server` 接口上的方法。

2026-10-22：用户体验改进计划（`docs/reference/usage-report.md`）。`api_fanout` 43→44：api 把调用、产品错误码与 panic
计入叶子包 `internal/server/usagereport`，更新检查附带的装机规模参数（原 `distribution.Report`）也搬进该包，`distribution`
因此仍不依赖本仓库任何包。

2026-11-10：Kubernetes 集群接入（[ADR 0052](../docs/adr/0052-kubernetes-nodes.md)）。`api_fanout` 48→49：集群的接入 manifest
由纯叶子包 `internal/server/kubemanifest` 渲染（不依赖 store，参数进、YAML 出），api 直接依赖它返回 manifest；`api_deps_fields`
不变（manifest 不经 Deps 注入）。

2026-11-12：部署用例（用例层拆分的第一组）。创建与评估搬进 `internal/server/deployment`，api 改为依赖它（+1），不再直接依赖
`acquisition` 与 `placement`（−2），`api_fanout` 49→48。

### api_opaque_response_fields（初值 40，2026-10-11）

意图：API 响应里类型为 `any`、`map[string]any`、`json.RawMessage` 的字段，`scripts/apiconv` 看不到其中的字段名，
无法检查 snake_case 与集合信封（`docs/reference/api-conventions.md`）。只降不升：新接口用具名 DTO。

```sh
CGO_ENABLED=0 go run ./scripts/apiconv -opaque      # 加 -v 列出字段
```

2026-10-26：编辑部署（`docs/reference/api.md#编辑部署`）40→43：`GET /deployments/{id}/draft` 把部署读回成创建请求
`dto.DeployRequest` 本身，创建请求里原有的三个开放字段（按服务端参数字典取键的 `quick_params`、`placement`、
`container_overrides`）因此也出现在响应里。它们是同一个请求契约，不是新加的不透明字段；编辑计划里逐字段的变化
（`DeploymentChange.from/to`）用字符串而不是 `any`。

排除：带 `MarshalJSON`/`MarshalText` 的类型自己决定编码，不计入。初值来自 API 规范统一时的实测：
统一前 42，把节点拓扑、benchmark `long_tail`、agent 拒绝记录、终端会话改为具名 DTO 后为 40。

### api_deps_fields（初值 48，2026-09-18 → 49（Monitor），2026-09-19 → 51（Logs 三回调））

意图：`api.Deps` 是 48 字段的上帝结构体（计划 B1），应分组为具名接口。

```sh
awk '/^type Deps struct/ {inside=1; next}
     inside && /^}/ {exit}
     inside && /^\t[A-Z]/ {n++}
     END {print n+0}' internal/server/api/dependencies.go
```

排除：注释行与空行（只计一级缩进的导出字段）。

2026-11-12：`Deps` 早已从 `api.go` 移到 `dependencies.go`，计数仍读 `api.go`，实测一直为 0，预算形同虚设。计数改读
`dependencies.go`，实测 61（8ac050aa0 自身的字段数），预算 60→61 是对齐实测的一次性调整，不是新增字段；棘轮从 61 起继续只降不升。

2026-09-18：`artifacts/archive/2026-09-19-monitor-center/plan.md`（监控中枢）第一期给
`api.Deps` 加了一个 `Monitor MonitorService` 字段，服务
`/monitor/overview`、`/monitor/deployments/{id}`、`/monitor/series` 三个路由。
本可以像 `MetricsQuery`/`LogsSearch` 那样加三个独立回调，但那样是 48→51；改成
一个持有三方法接口的字段，只把预算从 48 抬到 49——`api_deps_fields` 数的是
`Deps` 结构体的字段数，不是接口方法数，`MonitorService` 接口本身的方法数量
不计入这项预算。`MonitorService` 的方法只用 `dto` 包类型和标准库类型
（`context.Context`、`time.Duration`），刻意不引用
`internal/server/monitor`：`api_fanout` 当时已经在 36/36 的上限，
`internal/server/api` 引入 `monitor` 包会让它变 37，而这次改动的授权范围只
允许抬 `api_deps_fields`。实际的 `monitor.ClusterSnapshot`/`ServiceSnapshot`
→ dto 转换发生在新叶子包 `internal/server/monitorapi`（`MonitorService` 的
具体实现，只依赖 `monitor`、`api/dto`、`proto` 与标准库（`monitor/rules` 已随 ADR 0043 删除），
不依赖 `store` 也不依赖 `internal/server/api`），不在 `internal/server/api`；
`internal/server/app` 只保留 store → `monitor.Inventory`/`monitorapi.EventRef`
的适配代码，并把 `monitorapi.New(...)` 的结果接到 `api.Deps.Monitor`。
`api.ErrMonitorDeploymentNotFound`/`ErrMonitorSeriesTooLarge` 这两个哨兵错误
仍然定义在 `internal/server/api`，由 `app` 注入给 `monitorapi.Deps`（避免
`monitorapi` 反过来依赖 `api`）；`internal/server/api/monitor.go` 与
`internal/server/monitorapi/service.go` 的文档注释里有同样的说明。

2026-09-19：日志中心改造（artifacts/archive/2026-09-19-log-center/plan.md commit 4）在
`api.Deps` 上把单个 `LogsSearch` 回调扩为 `LogsSearch` / `LogsContext` /
`LogsHistogram` 三个，净增 2 个字段。这是刻意的接口扩面（新端点
/logs/context、/logs/histogram），不是依赖失控；与 Monitor 的单字段设计
叠加后，49→51 为新实测值。若 Monitor 也拆成三个独立回调，两处改动叠加会是
48→53；`MonitorService` 收敛成一个字段，是 51 与 53 之间的差额。

2026-09-22：评测改为「tokenizer 自动来源」时新增了一条 server 开流拉取节点单文件
的通道（`file.large_read`，见 `internal/server/api/benchmark_tokenizer.go`，
用于把目标机上已有的 tokenizer.json 取回 server 本地盘）。按本项预算的既定方向
（「应分组为具名接口」），没有新增 `Deps` 字段，而是把 `OpenLogtail` 与新的
`OpenLargeFileRead` 合成一个 `Deps.NodeStreams NodeStreams` 接口字段：字段数仍是
51，`api_fanout` 不变（接口只用 `net.Conn` 等标准库类型），新增流类型从此只增加
接口方法而不是结构体字段。调用点只有 `api/logs.go` 与 `internal/server/app` 的接线，
`hub.Hub` 直接满足该接口（方法名与 hub 方法同名，无适配层）。

2026-10-15：请求级 trace 导出（[ADR 0046](../docs/adr/0046-gateway-request-traces.md)）49→50：`Deps.Telemetry`
（`api.TelemetryService`，三个方法：读设置与状态、保存并应用、发测试 span）。它是数据库里的平台设置加运行中的导出器，
既不属于 `server.yaml` 的 `Deps.Server`，也不属于监控读路径的 `Deps.Monitor`，所以单独成一个具名接口；
`api` 不 import `traceexport`（`api_fanout` 不变），实现与校验在 `app`。

2026-10-18：NFS Cache 准备进度把高频复制观测、实际选中的准备节点与缓存淘汰阶段放回各自的生产者（hub、controller，`internal/server/prepobs` 叶子包提供有界过期表），不再放进 store；API 经一个具名接口字段 `Deps.Preparation`（`api.ModelPreparation`，现为 `deployment.ModelPreparation`：三项观测加缓存复制重试）读取，50→51。拆成四个回调会是 50→54。


2026-10-22：51→53。`Deps.UsageReport`（改进计划的日报：状态、开关、预览与 API 写入的进程内计数）与 `Deps.Notices`
（页头通知铃铛，内容来自签名发布通道）各是一个具名接口，不挂在含义不同的 `Deps.Server` 或 `Deps.Outside` 上。

2026-11-04：另存为推理模板（`docs/how-to/save-deployment-as-template.md`）59→60：`Deps.CatalogTemplates`（`api.CatalogTemplates`，一个方法）。
读部署、经网关实测操作、生成模板都在 `internal/server/app` 与 `internal/server/templateexport`，api 只依赖这个具名接口，
因此 `api_fanout` 与 `store_concrete_importers` 不变（`templateexport` 不导入 store，读 store 的部分放在 app）；
响应里的 notes 用已有的 `messages.Warning`，`api_opaque_response_fields` 不变。
### api_goroutines（初值 5）

意图：后台 job 起在 HTTP 层（计划 B4）。

```sh
grep -rnE '^[[:space:]]+go (a\.|func)' internal/server/api --include='*.go' | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。计划保留 `logs.go` / `terminal.go` 的流式转发协程，目标 ≤ 2。

### store_domain_imports（初值 9）

意图：store 因替 hub 装配 `DesiredState` 而引入领域包（计划 B5）。目标 0。

```sh
grep -rnE '"gpuworks\.ai/gpuworks/(pkg/bench|pkg/topoparse|pkg/modelrepo|internal/server/imagecatalog/manifest|internal/server/monitor)' \
  internal/server/store --include='*.go' | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。计划正文估 6；实测非测试文件共 9 处 import（`pkg/bench` 及其子包 3、
`pkg/modelrepo` 2、`pkg/topoparse` 1、`imagecatalog/manifest` 3）。`internal/server/monitor`
是监控中枢诊断阈值设置（`monitor.rules`）review 后新增的排除项，目标仍是 0——store 不得导入
`internal/server/monitor`（该设置已随 ADR 0043 删除，排除项保留）。

### agent_direct_exec（初值 5）

意图：agent 绕过 `runtime.Driver` 直接 `exec`（计划 B9）。目标 ≤ 1（doctor 的可注入 runner）。

```sh
grep -rn 'exec\.Command' internal/agent/app --include='*.go' | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。注意匹配的是 `exec.Command` 前缀，`exec.CommandContext` 一并计入。

### bare_time_now_ctrl_hub（初值 14）

意图：controller / hub 时钟不可注入（计划 C1）。目标 0。

```sh
grep -rn 'time\.Now()' internal/server/controller internal/server/hub --include='*.go' \
  | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。store 的 `time.Now().UTC()` 是写库时间戳，明确不在本预算范围内，
由 `scripts/check_time_policy.sh` 单独守护（计划 §4）。

### files_over_600（当前 13）

意图：CLAUDE.md 规定单文件 > 600 行触发拆分（计划 D1）。目标 0。

```sh
find internal pkg cmd -type f -name '*.go' ! -name '*_test.go' \
  | while read -r f; do wc -l < "$f"; done | awk '$1 > 600 {n++} END {print n+0}'
```

排除：`*_test.go`。计划 §4 声明 `internal/testenv` 与 `templateprobe` 的超长文件另行安排，
但它们是**非测试**文件，仍计入本预算（`internal/testenv/testenv.go` 755 行）；收敛时可以
在计划里显式记为取舍，但不从计数里悄悄抠掉。

### milestone_named_files（初值 7）

意图：按里程碑命名的文件（计划 A12）。目标 0。

```sh
find internal pkg cmd -type f -name '*.go' ! -name '*_test.go' \
  | while read -r f; do
      basename "$f" | grep -qE '(^|[^0-9a-z])[msgp][0-9]{1,2}([^0-9a-z]|$)' && echo "$f"
    done | grep -c .
```

P0 时的命中：`internal/proto/m1.go`、`m5.go`、`internal/server/store/entities_m1.go`、
`entities_m3.go`、`entities_m6.go`、`entities_m16.go`、`entities_m18.go`。P1 已全部按内容改名，
当前 0（改名清单见计划 §3 P1 的状态段）。

### process_name_refs（初值 77）

**这是计划正文之外新增的一条预算。** 意图：过程/里程碑标签属于项目管理词汇，不是领域词汇。
生产代码里的 `M1`、`S0`、`G2`、`P3` 这类标签会随计划失效而变成误导性注释——读者无从判断
"M11 之前的过渡桥"里的 M11 是否已经发生。它们应当被替换为对**行为**的描述，或删除。
测试文件可以自由使用（测试常需要引用具体阶段的回归场景），因此不计入。

计数单位是**命中行数**：生产源码里含标签的每一行，加上文件名含标签的每个文件（记为
`<file>:0`）。范围是 `internal/`、`pkg/`、`cmd/` 的非测试 Go 文件，以及 `ui/src` 下
非 `*.test.*` 的 `.ts` / `.tsx` / `.css`。

核心模式（每行先经过下述"已知词汇消解"后再匹配）：

```
(^|[^0-9A-Za-z_])[MSGP][0-9]{1,2}(\.[0-9])?([^0-9A-Za-z_]|$)
```

文件名模式（小写）：

```
(^|[^0-9a-z])[msgp][0-9]{1,2}([^0-9a-z]|$)
```

模式设计要点：

- 两侧用 `[^0-9A-Za-z_]` 而非依赖 `\b`。`\b` 在 `M300`、`sm90`、`P40` 这类更长的
  token 内部也会成立（`M300` 中 `M3` 后面跟 `0`，`\b` 不在此处成立，但 `sm90` 的 `m9`
  两侧都是字母/数字，显式否定字符类才能可靠排除）。显式否定类同时排除了下划线，
  于是 `entities_m16` 这类**标识符内**的标签在文件名检查里仍会命中，而
  `ttft_p50` 里的 `p50` 因紧邻 `_` 与数字不会误伤（另有专门排除，见下）。
- 正文只匹配大写 `[MSGP]`，文件名只匹配小写 `[msgp]`。Go 与 UI 的实际写法是
  注释里大写（`(M18)`）、文件名里小写（`m1.go`），分开处理可显著降低噪声。
- 小数位 `(\.[0-9])?` 用于捕获 `M16.5` 这种带小数的阶段号。

排除清单（false positive），全部在 `neutralize_known_tokens()` 中实现——做法是先把已知
的合法 token 替换成占位符，再跑主模式，这样同一行里若另有真标签仍会被抓到：

1. **百分位**：`P50` / `P75` / `P90` / `P95` / `P99` 及其小写 `p50` 等。
   出现在 `pkg/bench/metrics`、`api/preflight.go`、`api/benchmark_curve.go`、
   `agent/app/netprobe.go`、`ui/src/pages/evaluations/**` 的指标名与 JSON 键里。
   这是性能统计词汇，与阶段无关。
2. **SVG path 数据**：形如 `d="M12 2.5a9.5 …"` 的属性值。SVG 路径命令字母恰好包含
   `M`（moveto）后跟坐标。命中于 `ui/src/components/VendorLogo.tsx`。
   排除方式是整体替换 `d="[Mm][0-9]…"` 属性值。
3. **base64 数据 URI**：`data:image/png;base64,…` 的载荷是任意字母数字串，必然偶然命中。
   命中于 `ui/src/components/vendorAssets.ts`。
4. **udocker 的 P1 执行模式**（路径限定 `internal/agent/runtime/udocker/**` 与复刻该模式的 `internal/agent/runtime/runveil/**`）：
   udocker 的 PRoot 执行模式官方名称就叫 `P1`（`udocker setup --execmode=P1`），
   是外部工具词汇。仅在该目录下排除，其他目录的 `P1` 仍视为阶段标签。
5. **Tesla P40 显卡型号**（路径限定 `internal/testenv/cloudprovider/**`，2026-09-25 新增）：
   `P40` 是 compshare 容量 API 要求的 GpuType 原样拼法（`case "P40"`，代码不可改写），
   属外部硬件目录词汇。仅在该包（云厂商目录的唯一映射地）排除，做法与第 4 条相同。
6. **Apple silicon 芯片代际**（路径限定 `internal/accelerator/identity.go`、`internal/agent/gpu/apple*.go`、
   `internal/server/hardwarecatalog/catalog.go`，2026-10-14 新增）：Apple 把芯片代际命名为 `M1`…`M6`
   （`Apple M4 Pro`），统一内存名单、探测器解析的 SoC 名和硬件目录都须原样拼写。属外部硬件词汇，
   仅在建模 Apple 硬件的这几个文件排除，做法与第 5 条相同。

**硬件型号的处理**：`M300`、`P40`、`P100`、`M40`、`M60`、`T4`、`A100`、`sm90` 等加速卡与
架构名不需要额外排除项——`[MSGP][0-9]{1,2}` 后必须紧跟非字母数字，三位数的 `M300` /
`P100` 天然不匹配；`T4`、`A100` 的首字母不在 `[MSGP]` 里；`sm90` 的 `m9` 左侧是字母 `s`，
被否定字符类挡掉。当前仓库的 `internal/server/hardwarecatalog` 与 `internal/server/estimate`
下确实一条都不命中，已实测确认。若将来引入 `P4`、`M60` 这类两位数以内的 NVIDIA 老型号，
应按第 4 条的做法加**路径限定**的排除项（限定到硬件目录），而不是放宽主模式。
第 5 条正是该预案的首次落地：`P40` 作为 compshare 目录拼法进入 `cloudprovider` 时，
未调预算、未改正则，只加了路径限定的排除项。

P0 时 77 条命中的分布：`internal/server/store` 19、`internal/proto` 16、`internal/server/api` 12、
`internal/agent` 9、`internal/server/hub` 5、`ui/src` 3、`internal/server/deploy` 3、
`internal/server/controller` 3、`internal/server/scheduler` 2、`internal/server/app` 2、
`internal/templateprobe` 1、`internal/server/proxy` 1、`internal/server/modelmeta` 1。
其中 7 条是文件名命中，与 `milestone_named_files` 有意重叠：后者只数文件，前者数所有引用，
两者一起归零才算这类问题真正消失。

**P1 已归零（当前 0）。** 70 条正文命中全部改写为对行为的描述（例如
`// Hub integration (M1).` → `// Hub integration.`；
`// v1 ships a docker driver (M1); udocker (M3) implements the same interface.` →
`// The docker, udocker and nerdctl drivers all implement the same interface.`），
7 条文件名命中随文件改名消失。**没有新增任何排除项，也没有放宽正则**——
`neutralize_known_tokens()` 与 P0 时逐字相同，本文的排除清单（百分位、SVG path、base64、
udocker 的 P1 执行模式）四条不变。

### dto_test_lines_min（初值 81，方向 min）

意图：`dto` 包 1012 行源码只有 81 行测试，CLAUDE.md 的"字面 wire JSON round-trip"要求未兑现
（计划 C2）。这是唯一的下限项，只允许上升；计划 P5 的目标是 ≥ 600。

```sh
cat internal/server/api/dto/*_test.go | grep -c ''
```

用 `grep -c ''` 而非 `wc -l`，这样最后一行即使没有换行符也计入。

### store_text_columns（初值 74，2026-10-01 → 70（镜像目录四个适配声明列删除，迁移 115）→ 72（计量扩到注释声明 JSON 的定长列）→ 73（`inference_specs.image_sources`，迁移 133）→ 75（`gpus.foreign_processes`，迁移 146）→ 76（`nodes.accelerator_vendors`，迁移 156））

意图：持久化默认用带类型的列/表，JSON 只做整体读写的不透明快照（ADR 0032）。能装 JSON 的列是把 schema
藏进 Go 代码的入口，所以按行数设上限。新增这样的列必须上调本项，并在 `# raised:` 里写明
为什么不能是带类型的列。

计数对象是 store 非测试文件里带 `gorm:"` 标签的结构体字段行，满足任一条件即计入：

- 标签里有 `type:text`；
- 字段的注释声明了 JSON：同行 `//` 注释里，或紧挨着的上一行注释里，出现单词 `JSON`（区分大小写，
  前后不是字母）。这一条把 `size:` 定长却装 JSON 的列算进来。

```sh
find internal/server/store -type f -name '*.go' ! -name '*_test.go' | sort \
  | xargs awk 'FNR == 1 { prev = "" }
      { own = ""; if (index($0, "//") > 0) own = substr($0, index($0, "//"))
        above = (prev ~ /^[[:space:]]*\/\//) ? prev : ""
        field = ($0 ~ /^[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]+[^\/]*`[^`]*gorm:"/)
        json = (own ~ /(^|[^A-Za-z])JSON([^A-Za-z]|$)/ || above ~ /(^|[^A-Za-z])JSON([^A-Za-z]|$)/)
        if (field && ($0 ~ /type:text/ || json)) print ((own != "" || above != "") ? "documented" : "undocumented") "\t" FILENAME ":" FNR
        prev = $0 }' \
  | grep -c .
```

脚本里这段 awk 是 `store_json_column_lines()`，`store_text_columns_undocumented` 读同一份列表。

ZStack Cloud 接入增加 `CloudHost.Snapshot`，上限从 73 调整到 74：该列整体保存安装时的
`provider/zstack/cloud/inventory.CloudResource` 发现快照，不按内部字段执行 SQL 查询；其用途是保留外部资源
的原始快照，当前资源可用性仍通过 Cloud API 实时查询。字段注释声明归属类型。

排除：`*_test.go`。迁移文件里的历史形状结构体照常计入：它们同样是表上真实存在过的列。

2026-10-17 从 75 到 76 是新增列 `nodes.accelerator_vendors`（`Node.AcceleratorVendors`，`type:text`，
`// JSON []proto.AcceleratorPresence`）：agent 最近一次上报的、主机 PCI 总线上每个厂商的卡数与型号（含节点正在管理的
那家），供操作员在一机多厂商的主机上选择节点管哪家的卡。每次上报整体替换、读时整体读出画下拉框，数据库从不按其内容
查询、排序或关联；拆成带类型的表，1000 节点里几乎每台都只有一家，却要每次状态上报多写一张表。

2026-10-08 从 72 到 73 是新增列 `inference_specs.image_sources`（`InferenceSpecRow.ImageSources`，`type:text`，
`// JSON []string`，ADR 0038）：模板为自己的镜像声明的更多拉取地址，有序、整体读写，和同一行的 `args`、`entrypoint`
一样只是 spec 投影的一部分，数据库从不按其内容查询、排序、关联或局部更新，拆成带类型的表只会让每次读模板多一次
关联。归属类型是 `inferencecatalog.InferenceSpec.ImageSources`。

2026-10-01 从 70 到 72 不是新增列，是计量变宽后数到的两个已有定长列：`instances.gpu_indexes`
（`Instance.GPUIndexes`，`size:64`，`// JSON []int`）与 `instances.ports`（`Instance.Ports`，`size:128`，
`// JSON []int`），都在 `internal/server/store/entities_deployment.go`。本键是本 MR 新增的，基线里没有它，
所以不需要 `# raised:`。

盲区（归 review）：

- 定长字符串列装 JSON、注释却没写 `JSON`（例如只写「列表」或干脆没有注释）——脚本无从知道它装的是 JSON。
- 注释写在字段上方两行以外、或用 `/* */` 写的，不计入。
- 其他能装 JSON 的写法：`type:` 取 `text` 以外的值（`TEXT`、`json`、`longtext`），或 GORM 的
  `serializer:json`。目前 store 里一处都没有。

### store_text_columns_undocumented（初值 0）

意图：每个计入 `store_text_columns` 的列都要在注释里写明唯一的归属 Go 类型（`包.类型`，store 不能
import 的包只写在注释里），或写明是自由文本 / 密文。脚本只能检查「有注释」，注释内容是否写对归 review。

```sh
# 与 store_text_columns 同一段 awk（store_json_column_lines），只数没有注释的行：
... | grep -c '^undocumented'
```

排除：`*_test.go`。同行带 `//` 注释，或上一行是注释行（多行注释块写在字段上方的写法），都算有注释。
因注释声明 JSON 而计入的定长列天然有注释，所以实际能让本项变红的只有没有注释的 `type:text` 列；
两项读同一份列表，是为了「什么算一列」只有一个定义。

### store_sql_json_lines（初值 2）

意图：SQL 不按 JSON 列的内部字段查询、排序、关联或局部更新；需要看进 JSON 时在领域包里用归属类型
解码（ADR 0032）。计数单位是**行数**：已豁免的文件里再多写一行这样的 SQL 同样会超限。

```sh
find internal/server/store -type f -name '*.go' ! -name '*_test.go' | sort \
  | grep -v '/migration[^/]*\.go$' \
  | xargs grep -nE "json_[a-z_]+[[:space:]]*\(|instr[[:space:]]*\(|->>?[[:space:]]*('|\?|[0-9])" \
  | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c .
```

脚本里这段是 `store_sql_json_hits()`。匹配三类：

- SQLite JSON 函数调用：`json_` 开头的函数名后跟 `(`，覆盖 `json_extract`、`json_valid`、`json_type`、
  `json_each`、`json_tree`、`json_set`、`json_insert`、`json_array_length` 等全部 JSON1 函数；
- `instr(`：对 JSON 文本做子串匹配的老写法；
- `->` / `->>` 运算符，后跟引号路径（`data->'$.state'`）、绑定参数（`->>?`）或数字下标。箭头后跟别的
  不算：`internal/server/store/benchmark.go` 的错误文案 `"%q -> %q"` 就是这样排除的。

**注释行不计入**：整行以 `//` 开头的行不能执行查询，计入只会逼人删解释性注释；同一行代码后面带的
注释照常计入。一行里有几处调用都只算一行。

排除：`*_test.go`；`migration*.go`（迁移按定义要改写已有数据，改写 JSON 列的内部形状只能在 SQL 里做）。

初值 2 是 `internal/server/store/instance_timeline.go` 里 `eventFoldKey` 的两行：

- 第 32 行 `CASE WHEN json_valid(data)`；
- 第 33 行 `THEN COALESCE(json_extract(data,'$.state'),'') || char(31) || COALESCE(json_extract(data,'$.phase'),'')`。

时间线折叠用它们取历史事件行的 `$.state` / `$.phase`，只对 retention 尚未淘汰的旧行起作用，淘汰后连同
折叠 SQL 一起删除（`todo.md`），本项降到 0。

盲区（归 review）：对 JSON 列做 `LIKE '%"key":"v"%'` 的文本匹配与普通的 `LIKE` 查询在正则上无法区分，
本项不数它；评审时看到对 JSON 列的 `LIKE` / `=` / `GLOB` 比较，按 ADR 0032 第 4 条改成在领域包里解码。

---

## 依赖方向检查

`check_architecture.sh` 除预算外还执行计划 §5.3 的依赖方向检查。这些**不是预算**，
是结构规则，违反即失败：

- 叶子包 `inferenceapi`、`messages`、`placement`、`acquisition`、`chipmatch`、`partitioncap`
  不得 import `internal/server/store`；
- `api` 与 `controller` 互不 import；
- `pkg/**` 不得 import `internal/**`；
- `internal/agent/**` 不得 import `internal/server/**`；
- `internal/proto` 不得 import 本仓库任何包；
- `internal/server/distribution`（发布通道客户端，CLI 升级与 api 共用）不得 import 本仓库任何包，
  免得 `selfinstall` 经由它依赖到 server 其余部分（ADR 0040）。

**当前豁免**：`inferenceapi` 与 `partitioncap` 今天确实 import 了 store（计划 B7、B6），
计划在 store 边界阶段切断。脚本用 `LEAF_STORE_WAIVED` 按包名豁免并打印 `WAIVED`，
这样规则仍能拦住**新**的叶子包伸手够 store。豁免是双向的：某个包不再 import store 时，
脚本会**失败**并要求把它从豁免名单里删掉，避免豁免长期挂着变成死条目。

`internal/proto` 那条只匹配 import 列表，绝不匹配包自身的 import path——否则
`./internal/proto` 会因为自己的路径里含 `gpuworks/` 而永远"违规"。
