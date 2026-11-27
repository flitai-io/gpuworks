# 架构约束说明

`scripts/arch-constraints.txt` 的每一行是一条预算：`键<TAB>上限/下限<TAB>方向`。
`max` 表示实测值必须 ≤ 限额（只允许下降），`min` 表示实测值必须 ≥ 限额（只允许上升）。
`scripts/check_architecture.sh` 读取该文件，逐项实测并打印 `键 实测/限额 (方向)`。

规则来自 `artifacts/archive/2026-09-09-architecture-cleanup/plan.md` §3 P0 与 §6：

- 超限、统计命令执行失败或计数无效时，门禁失败（exit 1）。
- `max` 键实测**严格低于**限额时打印 WARNING（不失败），提示在同一个 MR 里把限额收紧到新实测值。
- 误报以**修模式为先**，不以调预算为先。每条排除项都记录在本文；改脚本时必须同步本文。
- **预算行只有三个字段**：键、限额、方向。理由、历史一律写在本文该键的小节里，不写在
  `arch-constraints.txt` 行尾——行尾注释会让改同一条预算的两个 MR 必然冲突。带第 4 个字段的行直接 FAIL。
- **放宽（`max`/`warn` 变大、`min` 变小）必须留记录**：`check_architecture.sh` 把预算文件与基线版本
  比较——基线优先取 CI 给出的合并请求基线 `$GPUWORKS_ARCH_BASELINE`，否则取
  `git merge-base HEAD origin/master`；解析不出基线（浅克隆、没有 `origin` remote、当前就在
  default branch 上）时打印 NOTE 并跳过，不算失败。被判定为放宽的键，必须在本文该键小节的
  "调整记录"里有一行（独占一行、从行首写起）：

  ```text
  - 放宽 `<键>` <基线限额>→<新限额>（GWKS-<单号>）：<原因>；回落条件：<什么落地后把限额降回去>
  ```

  例如 ``- 放宽 `api_fanout` 52→53（GWKS-123）：api 新增对 foo 包的调用；回落条件：GWKS-124 把 foo 的入口并进 bar 后降回 52``。
  脚本按键名、基线限额、新限额、`GWKS-<数字>` 和非空的"回落条件"逐项匹配，缺一项就 FAIL，并打印要补的那一行。
  新增或删除的键不算放宽；基线里已有的历史上调不追溯。收紧限额不需要记录。
- **`warn` 是软预算**：实测超过限额只打印 WARNING（附该键的替代做法提示），实测低于限额提示可收紧，
  **从不失败**。用于不宜硬拦、但需要在评审里被看见的趋势（`ui_antd_icon_import_files`、
  `ui_bundle_gzip_kb`、`app_prod_lines`）。上调 `warn` 限额同样要写上面的放宽记录。
- 需要额外前置条件才能测的键（如 `ui_bundle_gzip_kb` 需要先构建 UI），在 `measure()` 里返回
  `EXTERNAL`，预算循环只打印并跳过，由专门脚本测量。

文案长度、免责短语、`common` namespace 的 key 数、覆盖层导入和逐文件 antd 检查由 `./scripts/check_ui_architecture.sh` 执行。
GitLab 的 `core-ui`、GitHub 的 `ui` 和本地 `make ui` 共用这个入口。`./scripts/check_architecture.sh`
在有 Node 时也调用它；无 Node 时打印提示，由 UI 构建执行这五项检查。前四项预算仍由架构脚本对比基线。

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

排除：`*.test.*`；`ui/src/i18n/**`（引擎名作为展示文案出现在 locale 里是合理的）；控制台覆盖层 `ui/src/extensions/<id>/`（见 ee_ui_internal_imports）。

### ui_antd_import_files（迁移基线 349，2026-11-09）

意图：antd 与 Zeron 并存期间，引用 antd 的源文件数只降不升（迁移收尾清单见 `docs/dev/ui.md`）。计数单位是**文件**，
含 re-export、动态 import 与 `antd/es/...`、`antd/locale/...` 子路径。

```sh
grep -rlE "['\"]antd(/[^'\"]*)?['\"]" ui/src --include='*.ts' --include='*.tsx' \
  | grep -v '\.test\.' | grep -c .
```

排除：`*.test.*`；控制台覆盖层 `ui/src/extensions/<id>/`（见 ee_ui_internal_imports）。降到 0 时脚本额外提示加 `ui_raw_color` / `ui_raw_radius` 规则。

文件数只能拦"新文件引入 antd"，拦不住已有文件里继续加 antd 用法；逐文件的棘轮由下面的
`ui/antd-inventory.json` 负责。

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `ui_antd_import_files`（限额 287 时）：raised: master added antd before this ratchet existed (ModelSourceIDField, OutboundPolicyCard, the benchmark report export in benchmarkReportExport.css and EvaluationDetailPage, and the deploy diagnostics in DeployDiagnostics and DeploySubmissionError); the per-file snapshot (ui/antd-inventory.json) starts from that tree

### ui_copy_long_strings / ui_copy_caveat_strings（基线 0 / 0，2026-11-13）

意图：页面只写事实和下一步，公式、口径、免责放进 `ui-kit/HelpTip` 或文档（`docs/dev/ui.md` "Copy on the page"）。
`ui_copy_long_strings` 数中文 locale 里超过 120 字（key 以 `Help` 结尾的 HelpTip 内容放宽到 200 字）的字符串；
`ui_copy_caveat_strings` 数非 `Help` key 中带免责短语（不保证、仅供参考、不代表、未经实测、并不意味、不改变、仅用于界面演示）的字符串。只降不升。

```sh
node scripts/ui_copy_audit.mjs long     # 或 caveat；list 逐条列出
```

排除：无。超长说明改写或移进文档；"不包含模型文件"这类事实不算免责，正则不收。

### ui_copy_helper_strings / ui_copy_message_strings / ui_copy_sentences_strings / ui_copy_en_strings / ui_copy_inline_help_refs（基线 305 / 42 / 26 / 56 / 95，2026-11-21）

意图：上面两条只拦 120 字，而规范写的是一行 40 字，文案于是贴着 120 字写，`Help` 后缀的 key 又只看名字不看渲染。这五条按规范本身计数，存量记为基线，只降不升。

- `ui_copy_helper_strings`：非 `Help`、非消息 key 的中文字符串超过 40 字。
- `ui_copy_message_strings`：消息 key（路径里有 `apiError`、`warning(s)`、`error(s)`、`schedule`、`finding`、`fallback`、`blocked`、`unavailable`、`todo`、`reason`，或以 `Reason`/`Error`/`Warning` 结尾的一段）超过 80 字。
- 长度按渲染后的样子数：`$t(...)` 引用和 `{{占位符}}` 各算两个字。
- `ui_copy_sentences_strings`：非 `Help` 的中文字符串超过两句（数 `。！？`）。
- `ui_copy_en_strings`：英文 locale 超过 240 字符（`Help` key 400）。
- `ui_copy_inline_help_refs`：`.tsx` 里把 `Help` key 放进表单项 `extra`，或直接作为 `p`/`span`/`div`/antd `Text` 的文字，而不是 `HelpTip` 的内容。

```sh
node scripts/ui_copy_audit.mjs helper   # 或 message、sentences、en、inlinehelp；list 逐条列出
```

排除：无。超出的文案删减、拆成"事实 + 下一步"、移进 `HelpTip` 或文档；不靠改 key 名或拆 key 过关。

### ui_copy_clauses_strings / ui_copy_command_strings（基线 24 / 12，2026-11-21）

意图：上面几条按长度和句号计数，文案于是用"：…，…；…"把三句话串成一句，或把处理命令写进 `HelpTip` 的句子里；
页面上读不懂、命令也复制不了（`docs/dev/ui.md` "No chained clauses"、"Problems and what to do"）。只降不升。

- `ui_copy_clauses_strings`：非 `Help` 的中文字符串里 `；` 出现两次及以上，或一个 `；` 之前还有引出它的 `：`。
- `ui_copy_command_strings`：中文字符串（含 `Help` key）的句子里带用户要敲的命令：`sudo <命令>`、`gpuworks`/`gpuworks-agent` 的子命令、
  `kubectl`/`systemctl`/`journalctl`。key 以 `Command` 结尾的是命令块的内容，不计。

```sh
node scripts/ui_copy_audit.mjs clauses  # 或 command；list 逐条列出
```

排除：无。串句拆成 summary / fix 等独立 key 或改成 `标签 值 · 标签 值`；命令由组件拼成可复制块。
句子是否说清了问题和下一步，预算数不出来，由 `docs/dev/ui.md` "Reviewing copy" 的 maintainer review 把关。

### ui_common_locale_keys（初值 590，2026-11-21）

意图：`ui/src/i18n/locales/{en,zh}/common.json` 是每个功能都往里加文案的地方，2026-11-21 审计主线前一周的 185 次合并，51 次改了它，
分支在这里冲突。`common` 只放多个功能共用的词（导航、通用动作、状态、表格与表单的通用文字）；一个功能自己的文案
写进它自己的 namespace 文件（`nodes.json`、`deploy.json`、`deployments.json`、`models.json`、`settings.json`、`storage.json` 等，
全部见 `ui/src/i18n/locales/en/`），没有合适的文件就新建一个。存量记为基线，只降不升：把某个功能的 key 搬出
`common` 时，在同一个 MR 里把限额收紧到新实测值。

计数对象是 `en/common.json` 里值为字符串的叶子 key（`zh` 与 `en` 的 key 一致，由 `make i18n-check` 保证，数一份即可）。

```sh
node scripts/ui_copy_audit.mjs commonkeys
```

排除：`apiError.*`（2026-11-21 为 1022 条，连同它整份文件是 1612 条）。每个产品错误码在这里有且只有一条文案，
`scripts/i18n-check.mjs` 逐码核对缺失与多余，UI 也固定从 `common:apiError.<码>` 取文案；新增错误码必然新增一条，
不是可以放进功能 namespace 的文案，计入就等于每个新错误码都要放宽一次预算。

### ee_ui_internal_imports（初值 0，2026-11-16）

意图：控制台覆盖层模块（单独构建的控制台复制进 `ui/src/extensions/<id>/` 的代码，本仓库 git 忽略）只通过
`ui/src/extensions/sdk.ts` 使用控制台，这样控制台内部可以自由重构而不破坏覆盖层；`sdk.ts` 的导出清单由
`ui/tests/consoleExtensionsSdk.test.ts` 钉住，只增不改。本项数覆盖层里越过 SDK 的 import。

```sh
node scripts/ui_overlay_imports.mjs | grep -c .   # 脚本逐条列出 file:line: specifier，预算取行数
```

计入：`ui/src/extensions/<id>/` 下任一 `.ts/.tsx/.js/.jsx/.mjs/.cjs` 文件（含测试）的静态 import、re-export、
副作用 import、`import()` 与 `require()`，只要目标既不是 `sdk.ts`（相对路径或 `@/extensions/sdk`），也不在该覆盖层
自己的目录里。npm 包同样计入（React、i18n、路由都经 SDK 再导出），整行注释不计入。社区树里没有覆盖层，恒为 0；
单独构建的控制台在自己的 CI 里把覆盖层复制进来后跑同一个脚本。

盲区（归 review）：拼接出来的动态 import 路径、写在字符串里的 `from "x"` 文本，正则分不清。

同一组覆盖层目录被其他 UI 测量排除（`check_architecture.sh` 的 `drop_ui_overlays` 与 `ui_antd_inventory.mjs` 的
范围），这只在覆盖层从不提交时成立，所以脚本的「console overlays」一节硬性要求 `git ls-files 'ui/src/extensions/*/*'`
为空。

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

`check_ui_architecture.sh` 在 UI 预算检查后运行
`node scripts/ui_antd_inventory.mjs --check ui/antd-inventory.json`，与提交的快照逐文件比较，以下任一情况 FAIL：

- 某文件的 antd 使用点（sites）变多；
- 某文件新引入了快照里没有的 antd 名字（含 `import type`）；
- 某文件的 `@ant-design/icons` 引入数变多，或 CSS 的 `.ant-*` 规则数变多；
- 快照里没有的文件开始使用 antd / `@ant-design/icons` / `.ant-*` 选择器。

减少一律通过（打印 NOTE 提示刷新快照）。迁移 MR 在同一提交里运行
`node scripts/ui_antd_inventory.mjs --write` 刷新快照；快照每个文件一行，diff 只显示动到的文件。
该检查随 UI 构建执行，缺少 Node 或快照检查失败都会使作业失败。

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

排除：`*.test.*`；`ui/src/components/ui/`（Zeron 托管代码）；控制台覆盖层 `ui/src/extensions/<id>/`。

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

### agent_node_runtime_fields / agent_node_runtime_mutexes / agent_node_runtime_methods / agent_onrpc_cases / agent_engine_struct_maps（初值 77 / 12 / 233 / 51 / 9，2026-11-15）

意图：agent 的组合根只做接线（ADR 0066）。`NodeRuntime` 的字段、互斥锁、方法与 `OnRPC` 分发的 RPC 数只降不升：镜像、模型文件、卡健康、doctor 各自成为带锁的子系统，`NodeRuntime` 每个子系统一个字段，RPC 改为「方法 → handler」表。方法数守护行为：只把字段搬进子系统、方法仍挂在 `NodeRuntime` 上直接碰那些状态，不算拆开。`Engine` 的 map 字段只降不升：一个实例的状态按生命周期收进记录（`processFacts` 是引擎进程那一份），不再加按 UUID 的并行 map；`TestEveryEngineFieldIsClassified` 与 `TestRemovingAnInstanceLeavesNothingUnderItsUUID` 要求每个新字段分类、按实例的真的随移除清理。

```sh
# scripts/structcount 解析非测试文件的语法树，打印一个数
go run ./scripts/structcount -dir internal/agent/app    -type NodeRuntime -measure fields    # 70
go run ./scripts/structcount -dir internal/agent/app    -type NodeRuntime -measure mutexes   # 9
go run ./scripts/structcount -dir internal/agent/app    -type NodeRuntime -measure methods   # 218
go run ./scripts/structcount -dir internal/agent/app    -type NodeRuntime -measure rpcs      # 43
go run ./scripts/structcount -dir internal/agent/engine -type Engine      -measure maps      # 9
```

口径（`scripts/structcount/main_test.go` 的夹具逐条覆盖）：一行多个名字按名字计；指针锁、`sync.Map`、包内声明的 map 类型都计；匿名 struct 里的字段、锁与 map 照样计（包一层不算离开）；具名类型的字段只计这一个字段（移进具名子系统类型才算离开）；方法按接收者计，值与指针都算；`rpcs` 是包内所有 switch 的 case 里出现的不同 `proto.RPC*` 常量数，`OnRPC` 转交给辅助 switch（`kubeRPC`）的也计。

初值是 R-23 第一个 MR（GWKS-321）之后的值：`Engine` 的 12 张按 UUID 的 map 中，描述引擎进程的四张（睡眠级别、预热、L1 主机缓冲、卡上进程）收成 `processes map[string]*processFacts`，12 → 9。

77 / 12 / 233 / 51 → 70 / 9 / 218 / 43（2026-11-25，GWKS-503）：镜像成为第一个有自己类型的所有者 `nodeImages`，带走 8 个字段、3 把锁；镜像的 17 个 RPC 走它自己的两张表。同一次改动扩大了统计范围：`methods` 把以该类型为参数的函数也算作方法，不论它挂在谁身上（当时 4 个：`storageUsers`、`startSlurm`、`Agent.runServerDial`、`Agent.handlers`）；`rpcs` 把 `==` / `!=` 比较、值类型提到该类型的 map 字面量的键、以该类型为接收者或参数的函数里任何 map 字面量的键也算进去，另一个具名类型的、不接该类型的方法里的分发不算。按旧口径方法数是 214。

### agent_image_deps_fields（初值 9，2026-11-25）

意图：`nodeImages` 向 `NodeRuntime` 要的东西只降不升（ADR 0066 决策 4：依赖收窄）。依赖一多，子系统就是换了名字的 `NodeRuntime`。

```sh
go run ./scripts/structcount -dir internal/agent/app -type imageDeps -measure fields    # 9
```

### store_concrete_importers（初值 17）

意图：`*store.Store` 具体类型贯穿全服务端，无窄接口（计划 B6）。

```sh
go list -e -f '{{.ImportPath}}{{"\t"}}{{join .Imports " "}}' ./... \
  | grep -v '\.test$' \
  | grep -v '^gpuworks\.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures[[:space:]]' \
  | awk -F'\t' '$2 ~ /gpuworks\/internal\/server\/store( |$)/ {print $1}' | sort -u | grep -c .
```

排除：`.test` 伪包（`go list` 为带测试的包生成的合成包）；`internal/integrationtest/gpuallocation/internal/fixtures`，
它是分配测试共用的建库夹具，写成普通 `.go` 文件只为让多个测试包导入，不是生产依赖。只排除这一个包，
新的生产包或测试辅助包导入 store 仍然计数。
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

2026-11-24：推理网关不再持有 store（转发面拆分第 1 步）。`internal/server/proxy` 原先直接持有 `*store.Store`、
订阅事件总线并引用 hub 的错误；现在只持有 `proxy/plane` 里的接口（路由行快照、请求路径上的查找、回写、少数需要控制面的调用），
由 `proxy/storeplane` 用本进程的 store 与总线实现。句柄从 proxy 移进了 `storeplane`，行类型（`store.Deployment` 等）暂时仍是
store 的实体，所以 `plane` 也导入 store：包级导入数 24→26，而 proxy 对句柄的依赖由新增的 `proxy_store_handle_refs`、
`proxy_bus_hub_imports`、`proxy_adapter_refs` 三项守在 0。

- 放宽 `store_concrete_importers` 24→26（GWKS-460）：网关的 store 句柄移进新包 `proxy/storeplane`，接口包 `proxy/plane` 因行类型仍是 store 实体而导入 store；回落条件：转发面拆分第 2 步给快照定义自己的行类型后，`proxy` 与 `proxy/plane` 不再导入 store，降回 24

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `store_concrete_importers`（限额 24 时）：raised: the Slurm entry use cases (internal/server/slurmcluster) moved out of the HTTP handlers: the entry views read the store in grouped queries and report ingestion replaces an entry's node list, over the store's own Slurm rows (SlurmCluster, SlurmGPUNode, the counts and blockers), behind narrow interfaces; earlier: the deployment use cases (internal/server/deployment) create and evaluate a deployment and keep its read model (row lookups, runtime state) over the store, moved out of the HTTP handlers so the automation, batches and MCP can call them; api still imports store for the handlers not yet moved; earlier: the scheduling activity (internal/server/schedactivity) counts the events the store hands over (store.ObservedEvent, store.ResidencyMachine) and reads the replay and the deployments it names from the store; earlier: Cloud resource ownership moved into provider/zstack/cloud/resourceclaim; cloud retains host onboarding, while resourceclaim owns node/GPU reads and lease lifecycle state; internal/server/analytics answers the usage analytics queries over the usage tables, whose rows are store's own typed entities (UsageCounts, UsageGroup, the deployment and user name lookups) - a narrow interface would need a parallel set of row types for the same tables

### proxy_store_handle_refs / proxy_bus_hub_imports / proxy_adapter_refs（初值 0，2026-11-24）

意图：推理网关只经 `internal/server/proxy/plane` 的接口取数据与回写，不持有 `*store.Store`、不订阅事件总线、不导入 hub。
这样网关所依据的数据以后可以由别的成员提供，或者放在进程边界另一侧（GWKS-460 转发面拆分）。本进程的实现在
`internal/server/proxy/storeplane`，不在统计范围内。

```sh
grep -nE '\*store\.Store|store\.Open(With)?\(|\.Bus\b' internal/server/proxy/*.go internal/server/proxy/plane/*.go \
  | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c .
grep -nE '"gpuworks\.ai/gpuworks/internal/server/(bus|hub)"' internal/server/proxy/*.go internal/server/proxy/plane/*.go \
  | grep -v '_test\.go' | grep -c .
grep -nE '"gpuworks\.ai/gpuworks/internal/server/proxy/storeplane"|storeplane\.' internal/server/proxy/*.go internal/server/proxy/plane/*.go \
  | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c .
```

`proxy_adapter_refs`：适配包 `storeplane.Source` 内嵌 `*store.Store`，网关的生产代码一旦导入它，一次类型断言就拿回句柄，
前两项却数不到；所以网关的非测试文件不得导入或引用 `storeplane`，接线只在 `app`。

排除：`*_test.go`（测试用真实 store 搭夹具）与注释行。只数 `proxy` 与 `proxy/plane` 两个目录本身，不含 `storeplane`。

### proxy_store_type_files（初值 15，2026-11-24）

意图：网关里还把 store 的实体当行类型用的文件数（`store.Deployment`、`store.Instance`、`store.APIKeyIdentity` 等；
`planning` 与 `deploy` 的函数以这些类型为参数）。只降不升；第 2 步给快照定义自己的行类型后归零。

```sh
grep -lE '"gpuworks\.ai/gpuworks/internal/server/store"' internal/server/proxy/*.go internal/server/proxy/plane/*.go \
  | grep -v '_test\.go' | grep -c .
```

### store_db_accessor_callers（初值 0，2026-11-14）

意图：store 之外的生产代码不拿原始 `*gorm.DB`。`(*store.Store).DB()` 只留给测试；
其他包经 `Store` 的方法读写，ZStack Cloud 的主机与租约台账经 `(*store.Store).CloudLedger()`，
单独构建的服务器所加模块的表经 `(*store.Store).ExtensionTables(namespace)`（只限该命名空间前缀的表）。

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

GWKS-306：模型传输的 API 操作移出 Controller。`SyncModelToNode`、`SyncModelToNodeWithOptions`、`ProbeModelSync`、
`CancelModelSync`、`CopyModelFile` 五个导出方法改为 `Controller.Transfers()` 返回的 `ModelTransfers`（Sync / Probe / Cancel /
Copy），api 只依赖一个 `ModelTransfers` 接口（`Deps.SyncModel`、`Deps.CancelModelSync` 与 `PeerSyncHooks` 里三个模型钩子合为
`Deps.ModelTransfers`）；传输运行表 `syncMu`/`modelSyncs` 收进 `modelSyncRuns`。收紧为 `controller_struct_fields` 34、
`controller_struct_mutexes` 0、`controller_struct_maps` 0、`controller_exported_methods` 15、`api_deps_fields` 63，
`app_new_lines` 304、`app_prod_lines` 10046。这只是第一步：`ModelTransfers` 指回 Controller，传输的实现与运行表仍在
Controller 上，收窄的是 api 的依赖面与 Controller 的导出面，不是把传输逻辑搬出 Controller。

GWKS-307：指标与日志的采集管线（队列、单写者、deployment 标签回填、直方图分位数折算、TTL 缓存淘汰）从 app 搬进叶子包
`internal/server/obsingest`（约 1,140 行），app 只留约 150 行的适配：把 store 读成 API、自动化与 AI 助手要的形状。
`app_prod_lines` 10046 → 8973；依赖方向检查新增「obsingest 只依赖 proto、selflog、logstore、tsdb、uxstore」。

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

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `controller_struct_fields`（限额 34 时）：lowered by GWKS-306 (the transfer run table became one field, Controller.syncs); earlier raised: the server's sleep and wake order lock and the placement hints moved from package globals in planning (lifecycleMu, placementHints) into lifecycle.Orders, built by the controller and handed to the API and the automation (GWKS-305); the controller holds it (Controller.orders)
- `controller_exported_methods`（限额 15 时）：lowered by GWKS-306 (the five model transfer methods became Controller.Transfers); earlier raised: the server's sleep and wake order lock and the placement hints moved from package globals in planning (lifecycleMu, placementHints) into lifecycle.Orders, built by the controller and handed to the API and the automation (GWKS-305); Controller.Orders hands it to the app
- `planning_concrete_store_refs`（限额 74 时）：raised: the controller plans unit suggestions on the fleet node snapshot (LoadUnitSuggestFactsFromSnapshot, UnitSuggestFactsOn), merged with r32-controller-nodes before this budget landed; GWKS-309 moves these reads behind a narrow interface
- `app_new_lines`（限额 301 时）：lowered: New closes its store on every error path through one deferred close instead of seven, and runs the extension modules through one startExtensions call (app/extension_start.go); earlier lowered by GWKS-306 (one Deps.ModelTransfers in place of five transfer hooks); earlier raised: the server's sleep and wake order lock and the placement hints moved from package globals in planning (lifecycleMu, placementHints) into lifecycle.Orders, built by the controller and handed to the API and the automation (GWKS-305); New hands it to the API (Deps.Orders); earlier: New builds the shared fleet node snapshot and the deployment and instance list snapshot (r32-fleet-nodes, r32-list-states), merged before this budget landed
- `app_run_lines`（限额 191 时）：lowered: the health checks, card records, usage report, notices and extension services start through one services() loop (app/services.go)
- `app_run_goroutines`（限额 20 时）：lowered: one services() loop starts four loops and the extension services
- `app_prod_lines`（限额 8973 时）：lowered by GWKS-307 (the metrics and log ingestion pipeline moved to the leaf package obsingest); raised: the app wires coded failures (ADR 0067): the assistant reads an instance's runtime failure before its logs, a template export maps its checks to the check shape, and a path a socket does not serve answers in its function's error shape (gateway code table, API error writer); earlier: lowered by GWKS-306 (one Deps.ModelTransfers in place of five transfer hooks); earlier raised: the server's sleep and wake order lock and the placement hints moved from package globals in planning (lifecycleMu, placementHints) into lifecycle.Orders, built by the controller and handed to the API and the automation (GWKS-305); app hands it to the API and the automation; earlier: the fleet node and list snapshots are wired in app (r32-fleet-nodes, r32-list-states, r32-controller-nodes), merged before this budget landed

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

2026-11-17：Slurm 集群接入。`api_fanout` 49→50：集群的 setup 与检查脚本由纯叶子包 `internal/server/slurmboot` 渲染
（参数进、shell 文本出，不依赖本仓库任何包，`check_architecture.sh` 守护），api 直接在 `/slurm/setup.sh`、
`/slurm/check.sh` 返回它们；脚本不经 Deps 注入，`api_deps_fields` 不变。

2026-11-12：部署用例（用例层拆分的第一组）。创建与评估搬进 `internal/server/deployment`，api 改为依赖它（+1），不再直接依赖
`acquisition` 与 `placement`（−2），`api_fanout` 49→48。

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `api_fanout`（限额 52 时）：raised: api calls the Slurm entry use cases in internal/server/slurmcluster (views, report ingestion, one-time codes, what was seen of a setup, with one Forget per entry) instead of keeping them in its handlers; earlier: master and the Slurm branch each added one import while apart (the feature gate below, and the Slurm leaf internal/server/slurmboot), so the merge is one above both; earlier: Slurm clusters (like Kubernetes ones) join through scripts rendered by a pure leaf package, internal/server/slurmboot (parameters in, shell text out, imports nothing from this repository, guarded by check_architecture.sh), which api serves directly at /slurm/setup.sh and /slurm/check.sh; the scripts carry the entry settings and are not part of Deps, so api_deps_fields stays; earlier: extension routes answer to the feature gate (internal/server/featuregate, a leaf whose only dependency is messages): api applies it in the extension route wrapper and reads it for GET /console/manifest; earlier: the server's sleep and wake order lock and the placement hints moved from package globals in planning (lifecycleMu, placementHints) into lifecycle.Orders, built by the controller and handed to the API and the automation (GWKS-305); api imports the leaf lifecycle (no store import) for it; earlier: Kubernetes clusters (ADR 0052) render their join manifest in the leaf package internal/server/kubemanifest (no store import; parameters in, YAML out), which api serves directly; earlier: diagnostics: api starts and stops debug sessions and diagnostics collections and maps their records (internal/server/diagnostics, which holds the sessions, the request recorder and the collection builder; no store import), the one new import; earlier: card health checks: api reads each card's check states from the in-memory tracker and builds the health tab's table from them (internal/server/cardhealth), which keeps them out of the store because every status report brings them again; earlier: yield policies: api validates a deployment's yield policy and parameters with the yieldpolicy leaf (then named servicelevel; the policy model, no store import), the one new import (the service level constants it used to take from store are gone); earlier: the user experience improvement program (docs/reference/usage-report.md), api counts its calls, product errors and panics into the leaf usagereport package; and the scheduling center: api reads a deployment's schedule the way the automation does (internal/server/automation, a leaf with no store import: the next occurrence of each order and the automation's event codes), so the one new import is that leaf (44 was master's own count)

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

2026-11-21：云平台连接测试（`docs/reference/api.md#云平台连接测试`）44→45：`POST /providers/test` 与
`POST /providers/{id}/test` 以 SSE 逐项返回检查结果，`testconn.Check.Detail` 的键随检查项而不同
（`dns_resolve` 是 `ips`，`clock_skew` 是 `skew_seconds`，ZStack 的 `protocol_shape` 是 `shape` 等），与
`dto.SchedulingHistoryEntry.Params` 随事件码而不同是同一回事；每项一个具名类型只会把检查项自己的词表再写一遍。
控制台按键名插值，所以每个检查项的 `detail` 键都有字面 wire JSON 断言（`provider/testconn`、
`provider/zstack/cloud` 与 `api` 的连接测试用例），键名改动会让这些用例失败。

排除：带 `MarshalJSON`/`MarshalText` 的类型自己决定编码，不计入。初值来自 API 规范统一时的实测：
统一前 42，把节点拓扑、benchmark `long_tail`、agent 拒绝记录、终端会话改为具名 DTO 后为 40。

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `api_opaque_response_fields`（限额 45 时）：raised: the provider connection test streams its checks over SSE (testconn.Check.Detail), whose detail keys differ by check exactly as dto.SchedulingHistoryEntry.Params differ by event code; a named type per check would duplicate the checks' own vocabulary; earlier: GET /deployments/{id}/draft answers the create request itself (dto.DeployRequest), whose open maps now also appear in a response; and GET /scheduling/history lists the timeline events across deployments (dto.SchedulingHistoryEntry.Params), whose params differ by event code exactly as dto.InstanceEvent.Params do; a named type per code would duplicate the timeline's own event vocabulary (43 was master's own count)

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

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `api_deps_fields`（限额 64 时）：raised: Deps.Extensions, everything a separately built server hands the API in one bundle (api.ExtensionDeps: the routes it mounts inside /api/v1, the feature gate they answer to, the console banners and the edition the manifest names), so later extension hooks add fields inside the bundle rather than here; a Router option was weighed, but Router's variadic is auth.SessionPolicy and every caller would change; earlier: lowered by GWKS-306 (SyncModel and CancelModelSync became Deps.ModelTransfers); earlier raised: the server's sleep and wake order lock and the placement hints moved from package globals in planning (lifecycleMu, placementHints) into lifecycle.Orders, built by the controller and handed to the API and the automation (GWKS-305); Deps.Orders; earlier: Deps.Lists, the server's one snapshot of every deployment and instance (deployment.Lists, R-32) that the deployment and instance lists read instead of every row per request; nil reads them per request, so handler tests need no change. Folding it into Deps.Preparation was weighed and rejected: nine api call sites and the tests' Deps{Preparation: ...} literals would change for no gain; earlier: Deps.NodeDoctor composes live Agent health checks and disconnected-node access evidence through one diagnostic service, adding one field to the corrected master baseline of 61; earlier: a realignment, not new fields - the measurement read api.go, where Deps no longer lives, and returned 0; 61 is the count at 8ac050aa0 read from dependencies.go, and the ratchet resumes from there; earlier: Deps.CatalogTemplates, saving a running deployment as an inference template (it reads the deployment, tests its operations through the gateway and builds the template in package templateexport), one named interface so api imports neither templateexport nor the gateway; earlier: Deps.Diagnostics, one named interface for the debug sessions and diagnostics collections (files under the data directory, no store row), rather than methods on Server or Monitor, which are about something else; earlier: Deps.CardHealth, each card's check states in memory, their failure records and the agent reads behind them (the last run of a check), one named interface for what the cardhealth tracker holds; earlier: Deps.SchedulingActivity, the in-memory rollups of the scheduling activity (the last 24 hours, counted as events are written) that the activity route, the sharing stats and the node timelines read instead of the event log; earlier: Deps.WakeHeld, the gateway's held requests by deployment, an in-memory fact of the proxy that the scheduling overview reads and that has no store row (a plain func rather than another method on an unrelated interface); Deps.Networks, the one network service every component shares; Deps.Telemetry, the request trace export setting and its running exporter (ADR 0046), a named interface of its own rather than methods on an unrelated one; Deps.Preparation, one named interface for model preparation where it is produced (the hub's copy progress, the controller's placements, evictions and cache retry), so none of those in-memory facts sit in the store; Deps.UsageReport, the improvement program's daily report (status, switch, preview, the counters the API adds to), and Deps.Notices, the notice bell fed from the signed distribution channel, two named interfaces of their own rather than methods on Server or Outside, which are about something else; Deps.Analytics, usage analytics (the data retention setting, its status, and the long-term statistics queries), one named interface for what lives in the app's statistics service rather than methods on Monitor, whose snapshots are the recent raw data; Deps.Experience, the switch for the experience environment beside the server (status, on/off, reset, password), one named interface for a separate process the settings page controls, rather than methods on Server, which is about this server's own listeners and restart

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

### agent_runtime_name_comparisons（初值 7）

意图：设备后端（`internal/agent/gpu`）与 engine 里拿运行时驱动的名字做比较。容器在某个运行时里看得到哪些设备，由
`runtime.Target.View` 回答；按名字比较的分支在新增运行时时要逐个找出来改。目标 0。

```sh
grep -rnE '(==|!=) *("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host))|("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host)) *(==|!=)|case .*("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host))|(EqualFold|Contains)\(.*("(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host))' \
  internal/agent/gpu internal/agent/engine --include='*.go' | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。同时匹配字符串字面量与三组名字常量（`proto.RuntimeDriver*`、`runtime.Driver*`、`accelerator.Runtime*`），写法包括 `==`、`!=`（名字在哪一边都算）、`case`（一行里任何位置）、`EqualFold` 与 `Contains`。剩下的在 dGPU 注入器（`engine/partition_hygon.go`、
`partition_ascend.go`）与 `engine/engine.go`、`engine/runtime_mounts.go`。

### agent_container_option_literals（初值 1）

意图：设备后端与 engine 里手写容器命令行选项（`--device`、`--gpus`、`--runtime`、`--group-add`、`--tmpfs`）。设备
后端授予的是 `runtime.DeviceGrant`，要顶替 workload 自带的哪些设置写在它的 `Replaces` 里，由各运行时驱动渲染；读
workload 自带选项用 `runtime.ParseOwnOptions`。目标 0。

```sh
grep -rnE '"--(device|gpus|runtime|group-add|tmpfs)"' internal/agent/gpu internal/agent/engine --include='*.go' \
  | grep -v '_test\.go' | grep -c .
```

排除：`*_test.go`。剩下的一处是海光 dGPU 后端的 `--tmpfs`（`engine/partition_hygon.go`）。

### vendor_name_files_outside_definitions（初值 39）

意图：在厂商定义（`internal/accelerator`）与厂商驱动（`internal/agent/gpu`）之外按名字判断厂商的生产文件数。厂商之间
的差别写在各自的定义里，别处问定义（`accelerator.Supports`、`RuntimeMismatchFor`、`RequiresDeviceSelection` 等），不比
较厂商名。新增厂商不应让这个数上升。

```sh
names=$(sed -nE 's/^[[:space:]]*Vendor[A-Za-z0-9]+[[:space:]]*=[[:space:]]*"([a-z0-9-]+)"/\1/p' internal/accelerator/vendor.go | paste -sd'|' -)
grep -rlE "accelerator\.Vendor[A-Z][A-Za-z0-9]*([^A-Za-z0-9(]|\$)|(==|!=) *\"($names)\"|\"($names)\" *(==|!=)|case .*\"($names)\"|(EqualFold|Contains|HasPrefix)\(.*\"($names)\"" internal cmd pkg --include='*.go' \
  | grep -v '_test\.go' | grep -v '^internal/accelerator/' | grep -v '^internal/agent/gpu/' | grep -c .
```

排除：`*_test.go`、`internal/accelerator/`、`internal/agent/gpu/`。匹配两种写法：厂商常量（`accelerator.VendorNVIDIA`
这样的名字，不含 `VendorBrand(`、`VendorKey(` 这类只做查询的函数调用），以及与厂商名字符串的比较（`==`、`!=`、`case`、`EqualFold`、`Contains`、`HasPrefix`）；厂商名取自
`internal/accelerator/vendor.go` 的常量（小写字母、数字与连字符），新增厂商不用改这条命令。它数的是文件，一个文件里比较几次都算一个。
kube 驱动里比较 RuntimeClass 名字的 `rc == "nvidia"` 也在数内：那是写在定义之外的厂商知识。
不匹配别的大小写的产品字样（例如解析拓扑标签时的 `"NVIDIA"`、`"NPU"`）：那是在读外部文本，不是按厂商名分支。

### udocker_vendor_inference_funcs（初值 6）

意图：udocker 驱动里判断一个 workload 是哪家厂商的函数（`isNVIDIASpec`、`isPPUSpec` 这一类）。授予带厂商名时它们读
`DeviceGrant.Vendor`；不带时（旧 agent 的落盘记录、没有选卡的 workload）从环境变量名和设备路径推断，这部分不写厂商名，上一条
预算看不见。新厂商由授予里的名字认出，不应再加一个这样的函数。目标：不增加。

```sh
grep -rhE '^func is[A-Z][A-Za-z0-9]*Spec\(' internal/agent/runtime/udocker --include='*.go' --exclude='*_test.go' | grep -c .
```

排除：`*_test.go`。

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

### store_text_columns（初值 74，2026-10-01 → 70（镜像目录四个适配声明列删除，迁移 115）→ 72（计量扩到注释声明 JSON 的定长列）→ 73（`inference_specs.image_sources`，迁移 133）→ 75（`gpus.foreign_processes`，迁移 146）→ 76（`nodes.accelerator_vendors`，迁移 156）→ 81（`inference_specs.pd_roles` 与 `manual_command`，迁移 220）→ 82（`inference_specs.docker_args`，迁移 220））

意图：持久化默认用带类型的列/表，JSON 只做整体读写的不透明快照（ADR 0032）。能装 JSON 的列是把 schema
藏进 Go 代码的入口，所以按行数设上限。新增这样的列必须上调本项，并在放宽记录里写明
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

调整记录（原 `scripts/arch-constraints.txt` 行尾注释，原文照录；新的放宽按文首规则写 `- 放宽` 行）：

- `store_text_columns`（限额 83 时）：raised: inference_specs.dirs is the typed []deploy.ManagedDir (inferencecatalog.InferenceSpec.Dirs, ADR 0077) of writable directories a template spec declares; read and written whole with the rest of the spec projection and copied into the deployment snapshot, never queried, sorted, joined or updated in part, the same opaque-snapshot shape as components on that row. Earlier: inference_specs.docker_args is the typed []string (inferencecatalog.InferenceSpec.DockerArgs) of extra docker run options a template spec's engine needs; it is read and written whole with the rest of the spec projection, never queried, sorted, joined or updated in part, the same opaque-snapshot shape as args and entrypoint on the same row, where a typed child table would only add a join to every template read. Earlier: inference_specs.manual_command is a shell script a template owns whole, free text like pre_start_script on the same row, never parsed by the database; inference_specs.pd_roles is the typed inferencecatalog.PDRoleShapes layout of a PD-only spec (a few integers per role), read and written whole with the rest of the spec projection to seed the deploy form, never queried, sorted, joined or updated in part, the same opaque-snapshot shape as placement and components on that row, where a typed child table would only add a join to every template read. Earlier: instances.runtime_failure is the typed proto.RuntimeFailure snapshot of a rejected pre-container launch. The observed host facts cannot be reconstructed after a node changes; it is written/read whole with Message, never queried or joined. RuntimeObservation requires a created container and cannot own this failure. Earlier: card_health_episodes.first_evidence and latest_evidence: the evidence a failing card check read, as free text in the card health message's form (accelerator.FormatHealthEvidence, bounded to 1 KiB), not JSON; vendors name different fields, so there is no fixed set of typed columns for it; earlier: nodes.accelerator_vendors (proto.AcceleratorPresence list), every vendor whose cards are on the host's PCI bus as the agent last reported it, so the operator can choose which the node manages; it is replaced whole on each report and read whole to draw the choice, never queried, sorted or joined, and a typed table would cost a write per vendor on every status report of a 1000-node fleet that nearly always has one; gpus.foreign_processes, the compute processes somebody outside the platform is running on a card (proto.CardProcess), reported whole by the agent and read whole to show who holds it; the verdict asks only whether it is empty (GPU.HeldOutsidePlatform), the database never queries into it, and a typed table would add a join to every card read of a fleet that mostly has none; inference_specs.image_sources (ADR 0038), the ordered list of further pull references a template declares for its image; it is read and written whole with the rest of the spec projection and never queried, sorted, joined or updated in part, so a typed table would only add a join to every template read; same opaque-snapshot shape as args and entrypoint on the same row; CloudHost.Snapshot preserves the complete installation-time CloudResource inventory, read and written as one opaque snapshot rather than queried by its fields

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

### edition_branches（初值 0，2026-11-16）

意图：开源代码只**显示**版本类型（edition），不按它分支。不同版本在行为上的差异由单独构建的服务端通过
扩展点提供，开源树里不出现「如果是某个版本就……」的判断。

```sh
find internal pkg cmd -type f -name '*.go' ! -name '*_test.go' | sort \
  | xargs grep -nE 'version\.Edition[[:space:]]*(==|!=)|(==|!=)[[:space:]]*version\.Edition|switch[[:space:]]+version\.Edition' \
  | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | grep -c .
```

脚本里这段是 `edition_branch_hits()`。匹配：与 `version.Edition` 的 `==` / `!=` 比较（两种操作数顺序）与
`switch version.Edition`（一个 switch 计一处，其下每个 `case` 都在内）。整行注释不计入。

排除：`*_test.go`（测试可以断言版本字符串）。初值 0：引入时 `version.Edition` 尚不存在。

盲区（归 review）：先把 `version.Edition` 赋给局部变量再比较、或经函数包装后比较，正则看不到；
评审时看到按版本类型取值的分支，一律改成扩展点。

---

### kv_backend_id_literals（初值 0，2026-11-16）

意图：KV 卸载后端（[ADR 0069](../docs/adr/0069-kv-offload-backends.md)）只是 `internal/server/kvbackend` 的一行
加适配器里的一个渲染函数；后端 id 若以字面量出现在别处（Go 的 `== "lmcache"`、TS 的卡片判断），就是把
本应由注册表下发的规则又写了一遍。计数单位是**行数**。

```sh
{
  grep -rnE '"(lmcache|mooncake_store|hicache)"' internal pkg cmd --include='*.go' \
    | grep -v '_test\.go' | grep -v '^internal/server/kvbackend/' \
    | grep -v '^internal/server/deploy/kv_render_'
  grep -rnE "['\"](lmcache|mooncake_store|hicache)['\"]" ui/src --include='*.ts' --include='*.tsx' \
    | grep -v '\.test\.'
} | grep -c .
```

排除：测试文件；`internal/server/kvbackend/`（后端目录本身）；`internal/server/deploy/kv_render_*.go`
（各后端的引擎配置渲染）。名单里是已规划的三个后端 id，新增后端时把它的 id 加进正则。

---

### feature_key_literals_in_core（初值 0，2026-11-18）

意图：功能键属于声明它的包（安装包清单里的 `features`，或单独构建的 server 编进去的模块声明），
功能门本身不解释任何键（`internal/server/featuregate` 包注释）。核心代码一旦用字面量点名某个键
（`Require("acme.backup", …)`），社区代码路径就开始依赖一个它并不拥有的功能，这正是“社区代码路径
从不查询功能门”要防的。计数单位是**行数**。

```sh
key='"[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){0,2}"'
{ grep -rnE "(featuregate\.Key\(|\.(Require|Admit|Access|Declared|Availability)\()${key}|featuregate\.Key[[:space:]]*=[[:space:]]*\"|Feature:[[:space:]]*${key}" \
    internal cmd --include='*.go' | grep -v '_test\.go:'
  grep -rlE '"gpuworks\.ai/gpuworks/internal/server/featuregate"' internal cmd --include='*.go' | grep -v '_test\.go$' \
    | xargs -r grep -nHE "(^|[^A-Za-z0-9_])Key:[[:space:]]*${key}"; } | grep -c .
```

覆盖五种写法：把键形字面量直接交给功能门的方法、`featuregate.Key("…")` 转换、给
`featuregate.Key` 常量赋字面量、路由或控制台入口的 `Feature: "…"`，以及导入了 `featuregate` 的文件里
功能声明的 `Key: "…"`（`featuregate.Feature{Key: …}`，字面量常单独成行，所以按文件而不按同一行匹配）。
`Feature: "…"` 不限定所属结构体（多行字面量里看不到结构体名）：别的结构体的 `Feature` 字段若写了键形字面量也会计入，
到时把该处改为常量或在此登记排除。排除：测试文件（功能门与
推送状态的测试要构造键）。看不到的：先赋给普通 `string` 变量再转换的键；这种写法在评审里拦。

### published_tags_outside_grammar（初值 121，2026-11-25）

意图：内置镜像的 tag 一律是 `<backend><框架版本>[-<型号>]-<engine><版本>-rN`，文法只有一份
（`cmd/gpuworks-image-manifest` 的 `releaseTagPattern`）。镜像目录里已经有 121 个不合文法的存量 tag
（裸 `<id>`、`-<arch>-slotN`、`-rN-<arch>` 等），它们已经在用户的目录里，不能改名也不能删。生成器对写进
目录的 tag 逐条过文法：不合文法的条目必须在 `build/runner-images/matrix.yaml` 的 `legacy_tags` 里写
`legacy_reason`，否则 `make image-manifest` 失败；合文法的条目写了 `legacy_reason` 同样失败。所以声明的
条数就是例外的条数。这条预算让例外只减不增：新 tag 取合规的名字，不是再加一条声明。计数单位是
**`legacy_reason` 出现的次数**。

```sh
grep -v '^[[:space:]]*#' build/runner-images/matrix.yaml | grep -o 'legacy_reason' | grep -c .
```

按出现次数而不按行首的键来数，YAML 换一种写法（流式 `{…}`、加引号的键）也照样计入。排除：整行注释。
会多计的：`note` 等其他字段的正文里提到 `legacy_reason` 这个词；改正文，不调预算。看不到的：用 YAML
锚点与合并键（`<<: *x`）把一条声明复用到多个条目；矩阵里不用这种写法，评审里拦。发布脚本
（`build/runner-images/tag_check.py`）读的是同一份声明，不另有名单。

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
- `internal/server/kvbackend` 是叶子包，本仓库内只许 import `internal/accelerator`、`internal/engines`
  与 `internal/server/pdmode`（它只是一张后端描述表，渲染与门禁在 `internal/server/deploy`）；
- `internal/server/distribution`（发布通道客户端，CLI 升级与 api 共用）不得 import 本仓库任何包，
  免得 `selfinstall` 经由它依赖到 server 其余部分（ADR 0040）。

**公开线格式模块**：`pkg/extapi`（插件与 server 之间的线格式）是自带 `go.mod` 的嵌套模块，根目录的
`go list ./...` 进不去，所以脚本的「public wire module」一节单独硬性检查（违反即失败，不是预算）：

- 在 `pkg/extapi` 里 `go list -e -deps -test ./...` 列出的非标准库包只有它自己（含测试）：只用标准库；
- `pkg/extapi/go.mod` 没有 `require`/`replace`，`go` 行与根 `go.mod` 相同；
- 根 `go.mod` 有 `replace gpuworks.ai/gpuworks/pkg/extapi => ./pkg/extapi`；
- 黄金文件清单 `pkg/extapi/testdata/golden/SHA256SUMS` 只增不改：基线提交（与预算比较同一个基线）里的每一行
  必须原样留在当前清单里；解析不出基线时打印 NOTE 跳过。已发布的黄金文件改了或删了，就是改了冻结的线格式，
  应当新增版本目录。

`make test-extapi`（`make test-unit`、两条 CI 流水线都会调用）负责它的 gofmt、vet 与测试；`make vet` 也
vet 这个模块。

**当前豁免**：`inferenceapi` 与 `partitioncap` 今天确实 import 了 store（计划 B7、B6），
计划在 store 边界阶段切断。脚本用 `LEAF_STORE_WAIVED` 按包名豁免并打印 `WAIVED`，
这样规则仍能拦住**新**的叶子包伸手够 store。豁免是双向的：某个包不再 import store 时，
脚本会**失败**并要求把它从豁免名单里删掉，避免豁免长期挂着变成死条目。

`internal/proto` 那条只匹配 import 列表，绝不匹配包自身的 import path——否则
`./internal/proto` 会因为自己的路径里含 `gpuworks/` 而永远"违规"。

### ADR 0067 guards（初值均为 0）

意图：失败、拒绝与检查结果以「码 + 参数」表达，不以英文散文表达，也不让下游再去解析散文
（[ADR 0067](../docs/adr/0067-runtime-outcomes-as-codes.md)）。五个键都只许保持 0；实测命令即
`scripts/check_architecture.sh` 中同名的 `*_hits` 函数，非测试 Go 文件。另有一条不走预算、由
`make i18n-check` 直接拦截的规则：`internal/proto`、`internal/server/messages` 中的每个 UPPER_SNAKE
常量，以及别处名为 `Error…`/`Warning…`/`Failure…`/`Reason…`/`Code…` 的 UPPER_SNAKE 常量，都必须在
`ui/src/i18n/locales/{en,zh}` 中有文案。

#### error_text_classification

按错误文本判断类别：`strings.Contains/HasPrefix/Cut…` 作用于 `err`/`msg`/`message`/`detail`/`reason`/
`stderr`/`output`，`err.Error() ==`，`switch x.Error()`，以及对 `.Error()` 的正则匹配。仓库自己产生的
error 一律用 `errors.Is/As`。排除的是**调用外部系统的适配器**，每个文件只在那里按文本识别并当场转成
哨兵或码：

| 文件 | 包装的外部系统 |
|---|---|
| `internal/agent/modelfetch/retry.go` | Go net/http 的 HTTP/2、TLS 错误（类型未导出） |
| `internal/agent/runtime/docker/cli_answers.go` | docker CLI 的输出 |
| `internal/agent/slurm/node_state.go` | `scontrol show node` 的 Reason 文本（Slurm 没有给「GPU 数少于配置」的结构化码），当场转成 `proto.SlurmNode*` |
| `internal/server/logstore/query.go` | glebarez SQLite 驱动的 FTS5 语法错误（只给通用 SQLITE_ERROR） |
| `internal/server/uxstore/query.go` | 同一驱动的 "no such table" |
| `internal/server/provider/compshare/api_error.go` | CompShare（UCloud）API 的错误消息 |
| `internal/server/provider/vast/vast.go` | vast.ai 绑定 SSH 公钥的 "already associated" |
| `internal/server/provision/ssh_failure.go` | x/crypto/ssh 未导出类型的认证耗尽错误 |
| `internal/server/provision/install_command.go`、`probe.go` | 远端主机上安装/探测命令的标准输出 |
| `internal/server/inferencecatalog/package_tolerant.go` | gopkg.in/yaml 的解码错误：只取第一行作为跳过原因，不据此分类 |
| `internal/server/hub/benchmark_failure_code.go` | 老 agent 的评测失败消息（混合版本集群的长期读入边界） |
| `internal/server/store/instance_runtime_failure.go` | 老 agent 端口拒绝的消息前缀（同上） |
| `internal/testenv/cloudprovider/aliyun_api.go`、`aliyun_infra.go` | 阿里云 ECS 的锁定原因与错误码（测试 harness） |
| `internal/integrationtest/fleet/gateway.go` | 去掉网络错误里的端口以归并报告行，不做分类（测试 harness） |

不匹配正则、但同样只在适配器里识别文本的还有 `runtime/nerdctl/cli_answers.go`、`runtime/pull_failure.go`、
`expose/stream.go`、`storagemount/classify.go`、`autodl/api_answer.go`、`hub/rpc_remote_error.go`。

#### instance_failed_without_reason

不经结构化原因就把实例置为 `failed`：`State: proto.InstFailed`、`SetInstanceState(…InstFailed)`、
`reportState(…InstFailed)`、`Update("state", InstFailed)`，以及按消息前缀判断端口拒绝。agent 侧唯一入口是
`internal/agent/engine/runtime_failure.go` 的 `reportFailed`（排除），server 侧唯一入口是
`store.SetInstanceFailed`（以参数传 `proto.InstFailed`，模式不匹配）。

#### gateway_error_bypass

绕过网关码表写错误：`proxy` 包里的 `writeErr(`、`"gpuworks_proxy"` 类型字面量、API/网关/hub/batch 里的
`http.Error(`、server 里手写的 `{"error":` JSON。排除：`api/security_headers.go`（非 `/api` 路径的纯文本
分支）、两个写入器本身（`messages/http.go`、`api/responses.go`）、`api/inference_templates_official.go`
（参数键恰好名为 `error`）。

#### legacy_check_vocabulary

检查结果的旧词与字面量：`DoctorFinding`/`CardCheck` 字面量里写死的状态/码/id 字符串、`DoctorLevel`、
`accelerator.Check*`、卡检查的 `"pending"`、provision/provider 的 `ok|error|passed|failed|skipped`、
`InstallCheck` 副本、部署诊断的 `passed|failed|blocked|unknown|pending|not_applicable`、节点诊断的
`info|incomplete|completed|unavailable`。排除：`internal/proto/check_compat.go`，老 agent 旧词的唯一读入边界。

#### console_unlinked_code_packages

`make i18n-check` 不要求其码有核心 locale 文案的包必须不被 `cmd/gpuworks`、`cmd/gpuworks-agent` 链接。
当前包括 `internal/templateprobe`（目录维护者 CLI 的英文诊断）与 `internal/testenv/extplugin/fakeplugin`
（测试专用的第三方插件程序，故意发出合成的插件自有通知）。计数是这两个二进制依赖图里出现这些包的次数。
一旦控制台要展示这些码，就从 i18n-check 的排除表里删掉它并补文案，而不是放宽这条预算。
