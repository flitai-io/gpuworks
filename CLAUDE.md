# gpuworks — 开发规约

轻量易用的 GPU 集群管理平台：节点接入、GPU 探测、模型部署（vLLM/SGLang）、显存估算与调度、dGPU 动态 GPU（NVIDIA HAMi-core / 昇腾 vcann-rt / PPU ppu-slicing-shim / AMD rocm-slicing-shim / 海光原生 vdev，命名见 `CONTEXT.md`）、sleep/wake、监控日志、云厂商租机、多机推理、评测与 AI 助手。当前进行中的工作与 backlog 见 `artifacts/plan.md`（生成区块）与 `artifacts/README.md`（v1/v2 原文存档 `artifacts/archive/2026-08-18-roadmap/plan-v1.md`、`artifacts/archive/2026-08-18-roadmap/plan-v2.md`）。全部文档的索引与目录约定见 `docs/README.md`；领域词汇表（L1–L4 测试分层、dGPU 后端/能力/探针等术语的权威定义）见 `CONTEXT.md`。`AGENTS.md` 是本文件的符号链接，只改本文件。

## 架构总览

两个静态单二进制：

- **server**（`cmd/gpuworks` → `internal/server`）：控制面 + Web UI + OpenAI 兼容网关。子命令 `serve` / `install` / `upgrade` / `reset-password` / `migrate`。
- **agent**（`cmd/gpuworks-agent` → `internal/agent`）：跑在每个 GPU 节点上，负责探测、容器运行、采集。子命令 `run` / `smoke` / `upgrade` / `doctor` / `grant-device-access` / `bench` / `runveil`（ps/images/exec/logs 排障）/ `uninstall`。

逐包说明见 `docs/dev/code-map.md`。

### 连接架构

agent 主动外连 `wss://server/api/v1/agent/connect` → WS upgrade → go-yamux 多路复用。默认走**双通道**（各一条 WS + 一条 yamux）：`?lane=ctrl` 控制通道（固定 256 KiB 窗口，承载 `ctrl`/`terminal`，延迟敏感）、`?lane=data` 数据通道（256 KiB→16 MiB 自适应窗口，承载 `tunnel`/`logtail`/`telemetry`/`model.read`/`model.write`/`image.read`/`image.write`，适配高带宽高延迟链路）。数据通道用 `DataHello{node_token, session_id}` 认证并绑定当前 ctrl 会话（`session_id` 由 Registered 下发，不匹配回 `reject{reason:stale_session}`），是其子资源：独立退避重连，掉线（detached）期间新数据类流在调用方 context 内等重连（上限 5s，见 `hub.dataLaneReattachWait`），超时返回 `hub.ErrDataLaneUnavailable`（proxy 映射为可重试 503），永不回退到 ctrl（ADR 0024）。`proto.LaneFor(kind)` 是 kind→lane 唯一映射。老 agent、`--data-lane=false` 走单通道（无 `lane` 参数，data profile，行为同早期单通道）；server_dial 在 agent 开启数据通道时同样由 server 拨第二条 lane（`serverdial_lanes.go`）。
yamux 流：`ctrl`（双向 JSON Lines 控制面）、`telemetry`（agent 在数据通道上打开，上送指标/日志批次；server hello 宣告 `telemetry.stream` 且该连接会有数据通道才用，否则走 ctrl；指标与日志各一条流（流头 `ref`）；数据通道 30s 未连上则先走 ctrl、连上后切到数据通道（只切一次）；日志断线时在 agent 内存有界缓冲、重连补发，server 入库对遥测流反压不丢，ADR 0064）、`logtail`、`tunnel`、`model.read`/`model.write`（模型目录 tar 收发）、`image.read`/`image.write`（docker save/load 流）、`terminal`（交互 pty，target=host/container）。
控制消息 envelope：`{"t":"...","id":...,"d":{...}}`（定义在 `internal/proto`，含 serverdial HMAC、终端帧协议、instance 状态枚举如 `optimizing`）。
指令下发 = desired state + watch；短动作走 RPC。另有 server_dial（HMAC）：server 直连 agent 的 `--listen` 端口纳管出站受限的节点（SSH 只用于安装、预检查和带外恢复）。

### 看代码看不出来的约束

- SQLite 全仓统一用 glebarez 驱动，勿混用 modernc。
- `controller` 手写 workqueue（`workq.go`），不引 client-go。
- `recovery` 独立 SSH 旁路恢复离线 agent，刻意放在 controller 外（SSH 可阻塞几十秒）。
- `imagecatalog/pullref` 是纯叶子包，拉取候选顺序的唯一实现，store 与 imagecatalog 共用。
- `acquisition` 只产候选、不做选择、不做 I/O；服务端校验用户所选候选是否仍有效，不替换（ADR 0018）。
- `partitioncap` 是 dGPU 能力推导 status/reason 文本的唯一产地。
- `internal/server/deployment` 是部署创建/评估与部署读模型的唯一入口（纯状态推导在 `planning`），不依赖 `net/http` 与 `api`（`make check-architecture` 守护）；api 的 handler 只做解码、调用、按 `Kind` 映射 HTTP 状态，新的部署业务逻辑写进 `deployment`。
- `messages` 产语言中立 ProductError（稳定 code + 插值参数 + 英文 fallback），人类可读文本归客户端 locale。
- `selfinstall` / `installcred` 是叶子包，install 与 server 两端共用，不得反向依赖 server。
- agent `gpu` 不用 NVML cgo，走 `nvidia-smi` + `/proc` + sysfs；`gpu.Prober` 可注入做 hermetic 测试。
- agent `runtime` driver shell out 到 CLI，不用 Go SDK；`runtime.Driver` 可注入做 hermetic 测试。
- `internal/engines` 是纯叶子包无依赖：`modelmeta` 经 `inferenceapi` 被 `deploy` 依赖、agent 又不能依赖 server，两侧只能共用这个叶子包。
- `internal/accelerator` 是 vendor 身份唯一产地，新增后端必须显式改这里。

## 构建

```bash
make build      # 完整构建：跨编译 agent（内嵌）+ UI + server + agent 到 ./bin
make ui         # 构建前端到 internal/server/webui/dist（含 i18n-check）
make test       # test-unit（先构建内嵌 UI，再跑 untagged Go suite）+ test-ui
make run-server # 本地起 server
make demo       # 起 server + agent，验证接入/心跳/重连
```

其他常用：`make vet`、`make tidy`、`make test-integration`（fake agent/runtime 的产品级测试）、`make test-coverage-report`、`make server-linux-amd64` / `release-packages`（发布构建）、`make check-assets` / `check-libvgpu`（内嵌原生资产校验）、`make fake-engine` / `fake-model`（测试资产）。云端 L4 矩阵见 `make test-l4-*` 与 `docs/dev/testing.md`。

## 代码规约

- **评测信息与报告契约**：修改评测负载、采集、聚合、存储、API、UI、CLI 或导出前，必须阅读并遵守 [评测信息与报告规范](docs/dev/benchmark-reporting-standard.md)。区分计划、观测与推导，保留会话/轮次/请求关联，明确来源、统计分母、有效样本和未知值；相关改动须验证受影响的口径，不得把尚未采集的信息写成已实现能力。
- **规模目标 1000 节点 / 3000 实例**，fleet 用例（`make test-fleet-perf-processes`、`test-fleet-log-scale-processes`、`test-fleet-benchmark-processes`，以及 `TestFleetGatewayLoadAcrossProcesses`）是它的守门人；server 进程里跑的负载（server 发压评测等）与控制面共享 CPU 和内存，同样要有带 RSS/CPU 预算的规模用例。会被逐节点、逐实例、逐事件或逐请求触发的路径（controller reconcile、部署创建/详情 API、bus 事件订阅者、状态/心跳写入、路由表、日志/指标读、评测压测的逐请求回调与进度上报）不得每次全量读节点、实例或目录，不得每次对已累积的全部历史重新汇总或排序（进度、统计按时间节流或增量维护），也不得每条事件一个写事务：钉住节点就只读这些节点，高频事件做合并或批量，派生索引按版本缓存，SQL 先看 `EXPLAIN QUERY PLAN` 有没有全表排序。改动这些路径后，用 `GPUWORKS_FLEET_NODES=200` 跑 churn 用例，对比 server CPU-seconds 与改动前（2026-10 的累积退化就是这样漏进来的，见 `artifacts/archive/2026-10-04-l2-fleet-register-timeout/`）。
- **单文件 > 600 行触发拆分**。controller/reconcile 每资源一个文件，严禁上帝文件。
- **三个枢纽不再长胖**（`controller_struct_*`、`planning_*`、`app_*` 预算守护，总单 GWKS-313）：
  - controller 里按部署、节点或实例记的内存状态，收进自带锁的具名类型并实现纯内存的 `Forget`，不在 `Controller` 上直接放 map 和锁；带 I/O、会失败的清理不算 `Forget`，留在 reconcile 里显式调用并重试。
  - `planning` 是无状态的规则包：不放包级锁和可变变量；读库走本包声明的窄读接口（`*store.Store` 原样满足），不新增 `*store.Store` 形参；新规则先写不开库的测试。
  - `app` 只做装配：新功能的业务实现放自己的包，app 只留构造与适配；不往 `Run` 里直接加协程。
- **新增持久化事实必须谨慎，存储形式不是豁免**：只有确实必要、且无法从现有数据推断/推导/计算得到时才新增。新增 JSON/text 列、往已有 JSON 里加 key，与加列加表同等对待。
- **持久化默认用带类型的列/表**。JSON 列只用于整体读写的不透明快照（数据库不按其内部字段查询、排序、关联或局部更新），且必须：对应唯一一个具名 Go 类型并在列注释写明；不重复其他列已有的事实；形状变更同样要迁移或读时兼容，并带旧数据测试。禁止 `map[string]any` / `Extra` / `Meta` 式通用口袋，禁止一列多形态。
- server + agent 各为单静态二进制，`CGO_ENABLED=0`。
- **entity 与 DTO 严格分离**：store 层是 GORM entity，api 层是 dto，二者不混用。
- **事件发布不用 ORM 钩子**：写操作收敛进 `store` 显式方法，事务提交后 publish。
- 前端参数字典由 server 下发，不硬编码进前端。
- **前端不做业务判断**：可用性与资格（能否共卡、能否休眠、可选的动作）、上下限与默认值、优先级与派生状态（卡片状态、是否可取消）都由 server 计算后随 DTO 或字典接口下发，前端只负责展示、交互和本地化；同一条规则不在 TS 里再写一份，确需即时校验时也以 server 下发的数据为准。
- **测试夹具必须忠实还原真实外部 API 的行为**（含默认值、错误形态、参数校验），且后端测试必须覆盖前端/上层实际会传的默认值组合；宽松的 stub 视为测试缺陷。
- **依赖方向由 `make check-architecture` 强制**：叶子包不依赖 `store`、`api` 与 `controller` 互不依赖、`pkg` 不依赖 `internal`、agent 不依赖 server、`proto` 无本仓库依赖；另有 `scripts/arch-constraints.txt` 棘轮预算（`max` 只降不升、`min` 只升不降，上调须带 `# raised: <原因>`，由脚本对比基线强制，误报修模式不调预算，规则说明见 `scripts/arch-constraints.md`）。
- **过程/里程碑标签（`M1`、`S0`、`G2`、`P3` 之类）禁止出现在生产代码**的标识符、注释与文件名里，由 `process_name_refs` 预算守护；测试文件不受限。
- **内置镜像命名**：推理模板要用的内置（主推）镜像一律发布为 `gpuworks/gpuworks-runner:<backend><框架版本>[-<型号>]-<engine><版本>-rN`（文法见 `cmd/gpuworks-image-manifest` 的 `tagPattern`，如 `cuda13.0-sglang0.5.21-r2`、`metal15.0-vllm0.31.0-r1`），不另起仓库名；只有非主推的内置镜像可以单独命名。
- **内置推理模板的镜像必须能从公网直接拉取**：模板的 `image` 与 `image_sources` 只写 Docker Hub、quay.io、华为 SWR 这类普通用户无需内网即可拉取的地址，不写 `registry.zstack.ai` 等内网仓库（发布流程会推到那里，但不进模板；内网镜像由各站点自己配拉取源规则）。由 `seed` 的 `TestBuiltinTemplateImagesArePubliclyPullable` 守护。
- 敏感信息（云厂商 key、测试机地址等）放本地 `secrets/`（.gitignore），绝不进仓库。

## 文档与计划

- `docs/` 只描述当前行为；不可逆的架构取舍写 `docs/adr/`；术语以 `CONTEXT.md` 为准。
- **`artifacts/` 是独立的私有仓库** `gpuworks-artifacts`（与主仓库同一 git 主机、同一命名空间；GitHub 镜像 `flitai-io/gpuworks-artifacts`），检出在主 checkout 的 `artifacts/` 下，主仓库 `.gitignore` 忽略它、绝不跟踪其中文件（`make check-artifacts` 拦截）。主 checkout 里执行 `git clone "$(git remote get-url origin | sed 's/gpuworks\.git$/gpuworks-artifacts.git/')" artifacts`；各 worktree 用 `git -C ../gpuworks/artifacts worktree add -b <分支> "$PWD/artifacts" origin/master` 共享对象。计划、证据的改动提交到 artifacts 仓库，代码改动提交到主仓库，两边分别推送。没检出 artifacts 时 `make test` 照常通过，只跳过索引与链接检查——所以 worktree 里开工前先确认 `artifacts/` 存在，缺了就按上面的命令检出，否则读不到计划与已定决策，新决策也会漏记。`scripts/githooks/post-checkout` 在 `git worktree add` 时自动以 detached 方式检出（不用软链：多个 worktree 共用一个检出会互相切分支；主 checkout 没有 artifacts 时静默跳过），由 `make githooks` 设置 `core.hooksPath` 启用，`make test` 经 `check-artifacts` 会顺带执行；detached 检出里提交前先 `git switch -c work/<主题>`。
- 主仓库将要公开：新写的代码注释、`docs/`、脚本与 CI 不再引用 `artifacts/` 路径（外部读者打不开），设计依据写进 `docs/` 或 ADR；存量引用随开源计划清理。
- 新工作在 `artifacts/work/<YYYY-MM-DD>-<主题>/` 开目录，入口 `plan.md`（或 `README.md`）带 YAML frontmatter；计划、设计稿、验收记录、证据都放同一目录，正文只链接证据，不内联过程记录。
- 同一计划线只有一版时叫 `plan.md`；出现第二版起统一改为 `plan-v1.md`、`plan-v2.md`… 连续编号，不再保留裸 `plan.md`。
- 关闭一个主题：把 outcome 写进 frontmatter、改 `status`，整个目录从 `work/` 原名搬进 `archive/`；独立完成的调研放 `research/<YYYY-MM-DD>-<主题>/`。
- **计划文档的状态只写在文件自身的 YAML frontmatter 里**（`title`/`status`/`created`/`updated`/`summary`/可选一句话 `outcome`，`status: superseded` 必须带 `superseded_by`，`status: dropped` 必须带 `outcome`）；`artifacts/README.md`（work/research/archive 三张表）、`artifacts/.ignore` 与 `artifacts/plan.md` 的生成区块都由此派生，不手改。新增计划或改动状态后跑 `go run ./scripts/artifactsindex -write` 重新生成；`make check-artifacts` 守护一致性、每个主题目录的入口文件与状态约束，以及 `artifacts/...` 路径引用不失效（artifacts 内的失效引用报错，主仓库指向 artifacts 的失效引用只告警）。
- **`docs/` 内链由 `make check-doc-links` 守护**（`scripts/doclinks`）：相对 Markdown 链接及 `#锚点` 必须指向真实文件/标题，`docs/` 下不用 `[[wiki]]` 引用（`CONTEXT.md` 除外）；图片放 `docs/assets/`，用相对路径引用；指向私有 `artifacts/` 的链接不在检查范围内。
- **改了 `docs/` 下已有英文译本的页面，同一提交里要同步 `docs-en/` 同路径的译文**，并把该中文页新的 SHA-256 写进 `docs-site/translations.json`（标题结构与代码块须与中文一致）；`make check-translations` 守护（`make test` 包含，CI 的 docs-build 同样检查）。译本是否存在看 `docs-en/` 里有没有同路径文件。
- `artifacts/.ignore` 列出全部 superseded/dropped 文档，`rg` 默认跳过；需要搜到它们时用 `rg --no-ignore`。
- `todo.md` 只记真实缺陷、风险与待决策项（T 编号）；计划自带的验收流程不进 todo，未做完的写进该计划的 `outcome`。已解决的 T 项按月归档到 `artifacts/archive/todo-resolved/<YYYY-MM>.md`；已确认但决定暂不修的 T 项迁入根目录 `known-issues.md`，保留原编号与原文并加一行搁置记录，条件变化时迁回 `todo.md`。
- 代码注释里引用设计依据，指向 `docs/` 或 ADR；docs 还没覆盖到的细节先补进 docs，不指向 `artifacts/`。

## 测试环境

本地开发用 `make run-server` / `make demo`。真机与远端测试环境（含云厂商 key）的地址、SSH 方式与部署/回滚流程见 **`secrets/test-environments.md`**（gitignored，不入仓库）。`secrets/` 只在主 checkout 下完整；各开发目录都是主 checkout 的 git worktree、与它同级（如 `../gpuworks_xxx`），worktree 里通常只有 `*.example`，需要真实配置时读 `../gpuworks/secrets/`（即主 checkout 的 secrets）。

本地测试 server 端口按顺序尝试 8800、8802、8804、8806，以此类推；本地测试密码使用 `gpuworks-test-password-2026`。

跑测试（尤其是真机/云主机 L4 用例）遇到失败 case，尽量别让失败现场立刻被 teardown/重试抹掉，方便先手动登录复现环境调试、看真实进程日志，再改代码、最后统一清理。云厂商 L4 测试可设 `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` 跳过失败清理，实例留到自然寿命（`cloudTestLifetime`，当前 2 小时）到期或手动 `make test-l4-cleanup-leaked` / `cleanup-leaked --run-id <run-id>` 再清；昇腾 nerdctl 设备注入验证用的是 `GPUWORKS_KEEP_ON_FAILURE=1`；非云端场景（本地容器/进程）同理，先 kill 掉相关 agent/server 进程冻结状态，别急着删容器。

## 测试与验收规约

测试必须证明行为，而不是只证明函数被调用。新增或修改功能时遵循以下要求：

- **测试分层**：默认 `go test ./...` 保持完全离线、可重复；云厂商 API、SSH、GPU CLI 和容器运行时用忠实的 HTTP/SSH/CLI fixture 验证请求参数、默认值、错误形态和清理行为。真实云端只放在显式 L4 target/nightly/manual pipeline。
- **覆盖重点**：`internal/agent/runtime/nerdctl`、`conn`、`collector`、`app` 的正常、超时、断线、取消和错误传播路径必须有单测；关键包以 70% statement coverage 为目标，低于目标时必须在变更说明中解释未覆盖分支。覆盖率报告要同时包含默认包和显式 build-tag 包，不能用标签过滤掩盖缺口。
- **禁止假测试**：fake driver/stub 不得对所有方法无条件返回成功；至少注入失败、空响应、部分响应和清理失败，并断言上层错误、重试和资源释放。`t.Skip` 仅用于明确缺少外部能力；`GPUWORKS_TEST_STRICT=1` 下关键依赖缺失必须失败。
- **迁移与存储**：每个迁移要验证 schema 结果、版本 marker、重复执行幂等；故意注入中途错误，确认 schema 与 marker 同事务回滚。涉及 down/回滚的迁移必须覆盖回滚后的再次启动。
- **API 契约**：路由新增/修改要更新全量 route inventory，至少覆盖鉴权、成功、参数校验、404/405/错误响应；DTO 必须用字面 wire JSON、精确 key/value 和 round-trip 检查字段名、默认值、可选字段及敏感字段，不能只测 handler 内部结构体。
- **UI 可靠性**：React 组件使用 React Testing Library（RTL）按用户可见文本、角色和交互渲染验证；同时覆盖 loading/empty/error/success 和键盘操作。RTL 不替代浏览器 E2E；UI 覆盖率必须用 `c8 --all` 统计全源码，禁止只统计被测试 import 的文件。
- **浏览器 E2E**：遵循 `test/e2e/browser-e2e-testing-standard.md`，按共享组件、字段和业务主线登记真实请求、持久化、agent 执行与适用运行时证据；禁止浏览器 mock 管理 API、固定 sleep、未知场景默认通过或把 recording runtime 当真实推理。未实现与未执行的门禁必须明确记录，不能用 RTL 覆盖率替代。
- **云端成本与稳定性**：模板预装 fake-engine 与 tiny model 时，测试先检查并复用本地 tar/模型目录，避免重复 pull/download。普通 provider case 将 `ModelFile` 预置为 `MFPresent`，只在低频 RunPod/HuggingFace、CompShare/ModelScope case 验证一次真实下载（使用单个小 shard）。Gitee Ascend 完整 deploy+chat 只允许 nightly 或手工 pipeline。
- **矩阵去重**：同一断言只保留一个权威 case。单 VM 双逻辑节点验证同机 GPU/容器隔离；跨 VM Docker 验证网络/NCCL；跨 VM udocker 仅显式低频回归，不能把语义不同的场景合并成一个测试。
- **测试命名**：真实环境测试统一命名为 `l4-<provider或资源池>-<行为>-<拓扑或外部依赖>`；Go case、Make target、GitLab job 必须一一对应。名称直接写 `node-onboarding`、`public-download`、`deploy-chat`、`provisioning-matrix`、`one-vm-two-logical-nodes` 或 `two-vm-docker-distributed` 等可验证行为，禁止使用含义模糊的 `basic`、`full`、`suite`、`provider`、`nnode2`。nightly/manual 只属于 CI 触发策略，不进入 case 名称。
- **Make 目标只给多步流程和被引用的入口**：多于一条命令（先构建再跑、串多个检查）或被 CI/其他目标引用时才加 Make 目标；只是包一层 `go test -run`（外加环境变量/超时/tag）的单用例不加目标，把完整 `go test` 命令写进该测试函数的注释。例外：L4 用例仍按下文「测试命名」的一一对应规则保留 Make target。
- **完整测试走 Makefile，不要只跑 `go test`**。`go test ./...` 只覆盖 untagged 包，会漏掉 lint、架构约束、artifacts 一致性、时间策略、i18n 与前端，以及带 build tag 的 suite。完整的一轮：

  ```bash
  make test        # test-unit（ui + check-time-policy + check-architecture
                   #  + check-ci-yaml + check-artifacts + check-doc-links
                   #  + check-translations + go test ./...）
                   # + test-ui
  make test-l2     # -tags=multiproc ./test/l2/...
  make test-l3     # -tags=docker，容器级用例
  ```

  `make test` 本身**不包含** l2/l3，必须显式再跑。改动涉及调度、部署、agent 运行时或容器行为时，l2/l3 不是可选项。
- **带外部依赖的 suite 要看清是真跑了还是跳过了**。`test-l3` 的容器用例在没有 docker daemon 时整组 `t.Skip`，`make` 仍然退出 0 —— 把这种结果当成"l3 通过"是假绿。跑完用 `go test -tags=docker -v ./test/l3/ | grep -c '^--- SKIP'` 确认跳过数，并在变更说明里写明哪些用例没真跑。
- **提交前检查**：完整的一轮（见上）、相关包 `go test -race`、`go vet` 和 `git diff --check`。`-race` 下 server 大包远超 `go test` 默认 10 分钟超时（实测 store ≈17 分钟、controller ≈16 分钟，api 见 `known-issues.md` T54），一律加 `-timeout 40m`；默认超时触发的 panic 只说明没跑完，既不算失败也不算通过；云端测试结束必须按 scoped run-id 确认实例/转发/临时文件无泄漏，绝不执行无范围的全账号清理。
