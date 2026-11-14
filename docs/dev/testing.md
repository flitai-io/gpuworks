# 测试指南

测试规约的唯一权威是 [`CLAUDE.md`](../../CLAUDE.md)。本文说明如何选择和运行各层测试，
不重复定义质量门槛。

控制台真实浏览器测试遵循 [浏览器 E2E 测试规范](../../test/e2e/browser-e2e-testing-standard.md)：按共享组件与
业务领域盘点，用真实请求、持久化、agent 执行及适用运行时形成证据链。`make test-ui` 与
`npm run test:coverage` 是单元/RTL 测试，不代表浏览器业务覆盖；现有 Python Playwright 入口、
fixture 边界及尚待接入的浏览器门禁见该规范。

> **L3 不得在有在线 `gpuworks-agent` 的机器上运行。** L3 夹具在同一 docker daemon 上启动测试 agent，而 agent 的孤儿清理按 `gpuworks-*` 名前缀删除不在自己期望集合里的容器，会摧毁线上 agent 的实例（2026-09-16 在 gpu3 发生，见 `todo.md` T9）。要在这类机器上跑 L3，先停掉线上 agent，跑完再启动。

## L1–L4

| 层级 | 环境 | 用途 |
|---|---|---|
| L1（simulation） | 单进程、fake/fixture | 纯逻辑、协议、错误和边界；默认离线可重复 |
| L2（multi-process） | 本机多个真实进程 | server/agent 重连、流、进程退出和恢复 |
| L3（container） | 本地 Docker/udocker/nerdctl | 容器网络、挂载、runtime CLI 与 fake engine |
| L4（cloud GPU） | 显式真机或云资源 | GPU/驱动/厂商 runtime、真实下载和推理验收 |

```bash
make test
make test-integration
make test-coverage-report
```

提交前还要对相关包运行 `go test -race`、对带显式 build tag 的测试集做 compile-only（只编译、不执行）、运行
`make vet` 和 `git diff --check`。缺少外部能力时普通测试才可 `t.Skip`；
`GPUWORKS_TEST_STRICT=1` 下关键依赖缺失应失败。

`internal/testenv` 起的 server/agent 日志默认丢弃。`GPUWORKS_TESTENV_LOG=<path>` 全量写入文件；
`GPUWORKS_TESTENV_FAILURE_LOG_DIR=<dir>` 只保留失败用例的日志：每个用例在内存里留最后 1 MiB，失败时写成
`<dir>/<用例名>.log`，通过就丢弃。GitLab 的 `core-l3` 开启后者；`core-l2`/`core-l3` 失败时还把内层 dockerd
日志、容器列表与各容器最后几行写进 `failure-evidence/`，作为 job artifact 上传（总量封顶 10 MiB，保留 14 天），
用来查本地复现不了的偶发失败。

## 负载用例（默认 suite 之外）

这些用例测量实际经过的时间（墙钟时间）和资源消耗，默认测试集（suite）不跑，`GPUWORKS_TEST_PERF=1` 显式开启。
下文的 fleet 是批量运行 agent 的测试集群；churn 指反复执行部署、拉镜像、进入 running、删除的循环。
CI 引用的两个多进程用例有 Make target；其余直接用 `go test` 跑，完整命令（含 `-tags`、`-timeout`、缩放变量）写在各测试函数的注释里：

| 入口 | 用例 | 测什么 |
|---|---|---|
| `go test`（见 `skipUnlessPerf` 注释） | `internal/integrationtest/tunnel` 的 `TestLanes_*` | 双通道吞吐与 ctrl 延迟 |
| `go test`（见测试注释） | `TestFleet1000NodesRedeployChurn` | 大规模节点接入 + 反复部署时控制面的写入压力（单进程） |
| `make test-fleet-perf-processes` | `TestFleet1000NodesRedeployChurnAcrossProcesses` | 同一负载，但 server 是独立进程、fleet 是另一个进程 |
| `go test`（见测试注释） | `TestFleetLogCenterScale` | 日志中心在 fleet 负载下的读目标（单进程） |
| `make test-fleet-log-scale-processes` | `TestFleetLogCenterScaleAcrossProcesses` | 同一日志中心负载，server 与 fleet 分进程 |
| `go test`（见测试注释） | `TestFleetGatewayLoadAcrossProcesses` | `/v1` 数据面：server 进程 + 多卡 fleet（流式 fake 引擎）+ 流式客户端群，报告 req/s、TTFT/E2E、server 核数与内存、副本分布；`GPUWORKS_GATEWAY_*` 调形态，基线见 `artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/` |

fleet 用例默认 1000 个 agent，其中一半循环执行 部署→拉镜像→running→删除。数据库写入磁盘，
以测量真实写入代价；`:memory:` 不执行 fsync（把数据同步到磁盘），测不出这部分开销。用例断言：

- 在线节点的心跳不会超过 60 s 不更新（心跳排在进度写入后面，就是本次治理的目标故障）；
- churn 结束时没有节点仍处于 offline；
- 每次部署都能进入 running；
- 每次部署的事件行 ≤ 20。

另外记录（只报数不断言）：churn 期间掉线又重连的节点数、`GET /deployments` 与事件接口的
p50/p99（第 50/99 百分位耗时）、写入静止时的 `GET /deployments` 单次耗时、数据库增长、
RSS（进程驻留内存）与 CPU。

### 两种形态，同一套驱动

从「节点全部上线」到「churn 结束」之间的全部代码都在 `internal/integrationtest/fleet`
（REST 客户端、节点存活观测、churn 循环、报表与判定），两种形态共用，只有进程拓扑不同：

- **单进程形态**（`internal/integrationtest/fleet`，默认 suite 之外）：server 与 1000 个 agent 同处
  一个测试进程。最省事，但它的 RSS/CPU/调度器是两者之和，**回答不了「控制面自己要多少」**。
  模型与 model file 由 store 直接写入（属于测量窗口之外的 setup）。
- **多进程形态**（`test/l2`，`-tags=multiproc`）：真实 `gpuworks` 二进制作为独立进程，另一个
  `test/l2/cmd/fleet-host` 进程托管 N 个 agent（真实 agentapp + 同一套 fake prober/driver）。
  节点只能走 REST（`POST /api/v1/nodes`）拿 token，模型走产品自己的「注册节点上已有目录」
  路径（`POST /api/v1/models/local` 与 `/models/{id}/register-path`），不下载一个字节。
  两个进程的 RSS 与 CPU 分别从 `/proc` 读取。

**多进程形态是权威的规模 case**：只有它能把 server 自身的开销与 1000 个 agent 的开销分开，
数字也更稳定；单进程形态保留作小规模快速试跑。两者都不进默认 CI。GitLab 里由
`l2-fleet-redeploy-churn` job 跑多进程形态，只在 `FLEET_REDEPLOY_CHURN=1` 的 schedule / web / api
流水线里出现（每晚定时任务带这个变量）；job 以 `GPUWORKS_TEST_STRICT=1` 运行，文件描述符上限不够
会失败而不是跳过，两个进程的日志压缩成一个 tar.gz（按进程拆开的 40 个日志文件曾因上传体积被网关以 413 拒绝）作为 artifact 保留 14 天。掉线重连数与接口 p99 目前只报数不断言，
看 nightly 的趋势，积累足够运行后再定阈值。

多进程形态可用预编译二进制运行，**没有 Go 工具链的机器只能这样跑**：

```bash
GPUWORKS_FLEET_SERVER_BIN=/path/gpuworks GPUWORKS_FLEET_HOST_BIN=/path/fleet-host \
GPUWORKS_FLEET_WORK_DIR=/path/work make test-fleet-perf-processes
```

`GPUWORKS_FLEET_WORK_DIR` 把本次运行的数据库、agent 状态与日志放到指定目录（失败后可以现场
排查）；`GPUWORKS_FLEET_PROCESS_LOGS=1` 另存两个进程的 stdout/stderr。在已经跑着线上服务的
机器上，用私有 network namespace（独立的网络命名空间）隔离端口（引擎端口固定从 30000 开始分配）。
下面是命令框架，`...` 要替换为本节所选的实际测试命令：

```bash
unshare -n bash -c 'ip link set lo up; ulimit -n 65536; ...'
```

### 掉线重连

掉线重连只报数不断言。以下保留该指标的故障诊断与修复背景，便于理解当前判定方式。
之前把掉线归因为「一个进程同时跑 server 和 1000 个 agent，CPU 不够」，
实测否定了这个解释：换到 20 核空闲机器上抖动依旧。传输层存在一个竞态，`internal/wsconn`
修复了其中一条触发路径，另一条路径由后文的 keepalive 间隔调整解决。原来的触发过程是：

1. go-yamux 的发送循环（send loop）只在连接写超时周期过半时，才更新写截止时间。
2. coder/websocket 的 `net.Conn` 用一个「已过期」粘滞标志实现写超时。`SetWriteDeadline`
   会清标志并重排定时器，却无法撤回已经放行的回调；旧回调晚到，就会在续期之后重新把标志置为已过期。
3. 下一帧尚未写入 socket，就直接以 `failed to write: context deadline exceeded` 失败。
   yamux 把发送循环出错视为整个会话的致命错误，server 随即执行 `deregister`，节点变为 offline。

两端的 `laneKeepAliveInterval` 恰好是
`laneConnWriteTimeout` 的一半，空闲会话的 ping/pong 因此反复落在写超时那一瞬间，命中概率很高。

`internal/wsconn` 的回归用例（`TestLiveWebSocketSurvivesAWriteAtTheDeadlineInstant`）用真实
WebSocket 复现：未包装时 16/16 连接失败且全部耗时 <5 ms（根本没写 socket），包装后为 0。

但 `wsconn` 只能兜住「回调到来时没有写在进行」的那一支。同一个回调若撞上正在进行的写，会直接
cancel 适配器的写 context 且永不更换，coder/websocket 见写 context 被 cancel 就整条关闭连接，
之后的写也全部失败——这一支无法在包装层补救。线上（mirror1）每个节点每天仍掉线 40–100 次、
断开时刻严格对齐 15 s 周期、抓包显示关闭方在发出自己的 keepalive ping/pong 后不到 1 ms 就 FIN，
就是这条路径；本地回环同样复现（17 分钟 7 次）。

根治是把两端 `laneKeepAliveInterval` 从 15 s
降到 10 s，让 keepalive 写入总是早于写超时过半点至少 5 s 续期，永远不会踩到到期瞬间；
`TestLaneYamuxConfig_MatchesPlan`（server）与 `TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline`
（agent）守住这个余量。改后本地回环 25 分钟零掉线，100 个 web terminal 并发满速刷屏亦零掉线。

剩余的抖动是主机真的被打满时丢了会话又立刻重连，因此仍然只报数。**节点掉线后没回来**是另一
回事，会断言。诊断时用 `GPUWORKS_TESTENV_LOG=<path>` 打开 server/agent 日志，用
`GPUWORKS_FLEET_DUMP_DIR=<dir>` 在心跳变陈旧或节点掉线的当下抓 goroutine/mutex/block dump
（`GPUWORKS_FLEET_DUMP_TRIGGER_MS` 调阈值）。

### 单进程形态的收尾

单进程形态必须在用例结束时停止本用例的 agent，并关闭客户端连接，否则收尾会因为与被测行为无关的原因失败：

- `env.StopNodes(t)` 取消全部 agent 并等待 hub 的会话表清空。`AddNode` 的 cleanup 只取消
  context、不等待，server 关停于是和上百个正在拆解的会话抢时间，超出 testenv 的 5 s 关停预算。
- `client.CloseIdleConnections()` 关掉 churn 留下的 keep-alive 连接。用例自己开着几百条空闲
  连接时，`http.Server.Shutdown` 要等它们，实测这就是 1000 节点落盘用例约一半概率出现
  `server shutdown timeout` 的原因。

多进程形态没有这个问题：server 是独立进程，收尾就是发信号。

### 接入期间的列表响应

多进程 fleet 用例先接入半数节点并观测稳定状态，再接入剩余节点；默认即 500→1000。
每个接入阶段和稳定阶段分别并发读取节点全量列表、节点分页列表、部署列表和实例列表，输出样本数、
p95、最大耗时与失败数。每个接口的接入阶段至少 1 个成功样本，稳定阶段有界等待至少 3 个成功样本；短接入阶段只等待已发出的
请求完成，不把接入后的新请求算成接入样本。请求必须返回 HTTP 200、完整读取响应体，
且单次请求不超过 10 秒；超时、错误响应、读取失败和样本不足都不能判通过。
churn 期间的部署/事件列表同样检查错误、样本和 10 秒上限。

部署列表已使用批量关联查询，不能再把历史 N+1 描述当作当前性能根因。节点接入还会触发镜像恢复和
存储范围解析：这些读取必须限定当前节点及其相关传输/标签目标，避免每个新节点遍历整份镜像目录或
所有节点的存储覆盖。`GPUWORKS_FLEET_READS=0` 仅关闭 churn 的并发读，用于隔离 churn 代价；
多进程接入阶段的容量检查始终执行。

### 缩放与前置条件

按机器能力缩放，三个环境变量两种形态通用：

```bash
GPUWORKS_FLEET_NODES=200 GPUWORKS_FLEET_CHURN_NODES=100 GPUWORKS_FLEET_ROUNDS=1 \
GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleet1000NodesRedeployChurn$'
```

文件描述符（fd）需求约为 `6 × 节点数`；用例会尝试把 `RLIMIT_NOFILE` 软限额抬到硬限额，抬不上去时
`GPUWORKS_TEST_STRICT=1` 下失败、否则明确 Skip（不静默跑在不足的限额下）。非 Linux 主机
直接 Skip：那里的 fd 限额与 RSS 读法不同，数字无法与 CI 对比。

## 公网入口（expose）

`internal/server/expose` 的测试全部离线：假 cloudflared 通过 `PATH` 提供（`--version` 与注册
日志都是真实输出形状）。用于判断地址是否 ready 的网络健康检查由可注入的 `HealthChecker` 承担；
测试替换这一检查，验证 ready 判定，不依赖真实网络。进程相关用例跑真子进程：`connector-host`
helper 由测试二进制充当，覆盖 Stop 回收、SIGKILL 父进程后无孤儿（含启动竞态，多轮）、退出码
传递，以及「server 拿不到 hosting 锁时不托管」。上游通信数据格式（wire 形状，包括 cloudflared `/config`、
deb 索引、rpm repomirror 元数据）全部用 fixture 覆盖，fixture 用的是实测抓下来的真实报文。

**不新增依赖联网的永久测试**（用户确认）：下载、解包、安装的回归全部走合成包与本地 HTTP
fixture，不访问真实包源、不调用包管理器、不新增 Make target 或 CI job，也没有靠环境变量
开关的"特殊用例"——那仍然是一份会腐烂的联网测试。开发期的人工联网验证按需执行，记录版本、
环境与结果，但不固化成用例：

- **包源漂移**（fixture 是否还像上游）：`curl -s https://pkg.cloudflare.com/cloudflared/dists/any/main/binary-amd64/Packages | grep -E '^(Version|Filename|SHA256):'`
  与 `curl -sI https://pkg.cloudflare.com/cloudflared/rpm/x86_64/cloudflared-linux-x86_64.rpm`
  看 deb 索引字段、rpm 是否仍无版本号路径。解析器与 SHA256 校验的权威回归是 fixture 用例；
  这一步只回答"fixture 要不要重抓"。
- **端到端获取**：在一台 PATH 上没有 cloudflared 的主机上跑 `gpuworks expose up`，确认
  下载、解包、`--version` 自校验与随后的注册都正常。
- **quick / named / agent**：注册与新地址可用、公网 `/healthz`、named 的 token-only 与路由
  发现、SSE 首包与取消、真实 agent 接入与断网恢复——均为开发期人工验证，不固化成用例。

**named 的真机验收在 2026-09-22 只完成了一半**（用户随后决定暂不开放该提供方，见下）：实机确认了
连接器自动获取（`pkg.cloudflare.com` 下载 + 不可变版本路径启动）、只给 token 启动、注册成功
（`Registered tunnel connection`，边缘/协议归观测）、远端未发布路由时的 `awaiting_route` 与指引、
以及该 tunnel 只有默认 catch-all（`version:-1`、`http_status:503`）时不被误判为可用地址。

**未证**：
首次公网可达（`ready` 提升）、SSE 首包/取消、agent 接入、重启后域名不变——这些都需要用户先在自己
的 Cloudflare 账号里发布一条路由，而 named 现在不对用户开放（`--provider cloudflare-named` 会被
明确拒绝），所以这几条在放开之前无法验收，也不得写成已通过。

## 架构门禁

`make check-architecture`（已并入 `make test-unit`，CI 与 `check_time_policy.sh` 同 job）
执行两类检查：

- **架构约束**：`scripts/arch-constraints.txt` 每行是 `键<TAB>限额<TAB>方向`。`max` 表示实测值
  必须 ≤ 限额，`min` 表示必须 ≥ 限额。脚本逐项打印 `键 实测/限额 (方向)`，超限即失败。
- **依赖方向**：叶子包不依赖 store、`api` 与 `controller` 互不依赖、`pkg` 不依赖 `internal`、
  agent 不依赖 server、`proto` 无本仓库依赖。这些是结构规则，不是预算，违反即失败。

棘轮规则：`max` 键的限额**只降不升**，`min` 键的限额**只升不降**。实测值优于限额时脚本打印
WARNING（不失败），提示在同一个 MR 里把限额收紧到新实测值。任何抬高 `max` 限额或降低 `min`
限额的改动都必须在 MR 描述里解释并获得显式批准。

每条预算的意图、实测命令与排除清单见 [`scripts/arch-constraints.md`](../../scripts/arch-constraints.md)；
误报应当修模式，不应当调预算。

过程/里程碑标签（`M1`、`S0`、`G2`、`P3` 这类阶段编号）**禁止出现在生产代码**里——包括标识符、
注释和文件名，由 `process_name_refs` 预算守护。计划一旦失效，这类标签就变成读者无法核对的
误导性注释；应改写为对行为的描述。测试文件不受此限制，可以自由引用具体阶段。

真机用例统一为 `l4-<provider或资源池>-<行为>-<拓扑或外部依赖>`，Go case、Make target
与 CI job 一一对应。可验证行为使用 `node-onboarding`、`public-download`、
`deploy-chat`、`provisioning-matrix`、`one-vm-two-logical-nodes` 或
`two-vm-docker-distributed`；`nightly` 和 `manual` 只描述触发策略。

仓库当前提供 RunPod、AutoDL、Vast、CompShare、阿里云 ECS 和昇腾资源池的显式 `make test-l4-*`
target。先以 `make help`/Makefile 确认目标，再只运行所需的测试路径（lane）。

定时（`CLOUD_FULL=1`，每天 03:00 与 15:00 Asia/Shanghai 各一次）的触发策略：

- CompShare 的 VM 用例（`l4-compshare-provisioning-matrix`、`single-vm-tp2-dgpu`、
  `one-vm-two-logical-nodes`、`two-vm-docker-distributed`、`pd-deploy-chat-two-vm-docker` 和
  `modelscope-public-download`）**不进定时任务**：这个资源池几乎排不到机器，阿里云用例接替它们。
  需要时从 web/api 流水线带 `COMPSHARE_VM=1` 手动触发，或在 MR 里手动点。CompShare 的容器用例
  （`l4-compshare-server-dial-node-onboarding-container`）和离线契约
  （`core-compshare-offline-contracts`）照常运行。

云端 L4 的 CI job 都带 `environment: {name: cloud-test/<provider>/<lane>, action: prepare}`：环境作用域的
变量（`cloud-test/<provider>/*` 下的凭据）照常注入，但 `action: prepare` 不创建 deployment。
项目开着「跳过过期的部署作业」（`ci_forward_deployment_enabled`），一旦 job 创建 deployment，
同一环境里后启动的 job 会把先启动、仍在运行的那个直接判失败（日志截断，`403 Job is not running`）；
定时 master 流水线与 MR 手动点同一个 job 就会这样互相打断。新增云端 L4 job 必须同样写
`action: prepare`；互斥靠 `resource_group`，不靠环境。

## GitLab MR 流水线的拆分

MR 流水线的总时长由 `core` 阶段最慢的 job 决定，而 runner 的核数差距很大（192 核与 20 核的机器上同一个
suite 相差约 4 倍），job 落在哪台机器取决于哪里有空位。所以两个长 suite 拆成并行分片，单片落到慢机器上也
不再是关键路径：

- `core-ui-test` 用 `parallel: 3`，以 `TEST_SHARD=$CI_NODE_INDEX/$CI_NODE_TOTAL` 走与 GitHub 相同的
  `--test-shard`；host-env adapter 与 strata 队列资产两项非 UI 检查只在第 1 片跑。
- `core-l2` 用 `parallel: 3`。`make test-l2 L2_SHARD=<i>/<n>` 由 `scripts/go_test_shard.sh` 列出 suite 的
  顶层用例，按名字排序后轮流分配（第 1、4、7… 个归第 1 片），再作为 `-run` 传给 `go test`；每个用例恰好
  落在一片里。CI 带 `-v`，trace 里有每个用例的耗时，可据此检查各片是否均衡。

另一项与 runner 无关的开销是建库：`store.Open` 打开空的内存库时，每个测试进程只真实执行一次完整迁移链，
之后的空内存库回放这次的结果（见 `internal/server/store/migration_snapshot.go`）。文件库以及已有表的内存库
始终走真实迁移，升级路径不受影响。

## GitHub Actions 镜像门禁

`.github/workflows/ci.yml` 在 GitHub 托管 runner 上跑与 GitLab core 阶段相同的检查：UI 构建与测试、
untagged Go suite 与各 check 脚本、`go vet`、`make lint`、CompShare 离线契约、严格模式的 L2/L3。权威流水线仍是 GitLab；
GitHub 镜像只收到 master，所以这条 workflow 由 PR、手动触发与每晚 03:07（Asia/Shanghai，紧随 GitLab 的
nightly）在 master 上运行，用来与 GitLab 同一提交的结果对比。它不使用任何 secret 或自托管 runner。
另外三条 workflow 对应 GitLab 的其余非 L4 job：`docs.yml`（文档站构建，改到文档的 PR 触发，从公开的
`gpuworks_website` 仓库拉 OINK 主题）、`ui-check.yml`（UI 巡检，改到 UI 的 PR 与每晚触发，报告作为 artifact 上传）、
`release.yml`（发布包，见[发布指南](release.md#github-actions-发布)）。L4 仍只在 GitLab 跑。

脚本只读与 CI 无关的变量名：`GPUWORKS_BUILD_COMMIT`（构建提交）、`GPUWORKS_BUILT_AT`（同一次运行所有 job
共用的构建时间）、`GPUWORKS_ARCH_BASELINE`（`check_architecture.sh` 比较预算文件用的合并请求基线），由
`.gitlab-ci.yml` 与 GitHub workflow 各自从自己的内置变量赋值；不设时脚本退回本地的 `git` 与当前时间。
`check_ci_yaml.sh` 对两边的 CI 定义都做 folded 标量注释检查，stage 顺序检查只针对 `.gitlab-ci.yml`。

- L2/L3 与 GitLab 的 `.docker-core` 同形：每个 job 一个 DinD daemon，暴露在专用的 `gpuworks-e2e` socket
  上，suite 跑在 golang 容器里。直接用 runner VM 的 Docker 不行：VM 有 systemd，console upgrade 用例会把它
  当成服务管理器；真实终端用例只接受专用 daemon。fixture 基础镜像在 suite 前预拉进 DinD。
- UI suite 在 2 核 runner 上一次只跑一个测试文件，不拆要一个多小时；按 4 片并行，`npm run test:coverage`
  读可选的 `TEST_SHARD`（如 `2/4`）传给 `--test-shard`，不设时与原命令相同。
- untagged Go suite 按包分 3 片：`scripts/go_package_shard.sh <i>/<n> ./...` 把 `go list` 的包按导入路径排序后
  轮流分配，最重的 `server/api` 与两个 integrationtest 包恰好落在不同片里。L2 与 GitLab 的 `core-l2` 一样按
  `L2_SHARD` 分 3 片。check 脚本、`go vet`、`make lint`、CompShare 离线契约与 `check-tagged-build` 合在一个
  `checks` job 里：组织的托管 runner 有并发 job 上限，每项只跑几十秒到几分钟，单开 job 只会多占 runner。
- 满盘 L2 用例需要一个专门的 tmpfs：suite 所在容器不能建 user namespace，所以和 GitLab 一样由 `docker run
  --tmpfs` 挂好并经 `GPUWORKS_TEST_FULL_DISK_TMPFS` 传入。

## 成本和缓存

- provider 常规用例只验证租机、SSH、agent 上线与 GPU inventory。RunPod、CompShare、AutoDL、
  Vast、ZStack 的 API 字段、区域约束和清理方式由各 provider 实现决定，用例按各自实现写，
  不假设统一行为。
- 公网下载只由 `test-l4-runpod-huggingface-public-download` 和
  `test-l4-compshare-modelscope-public-download` 等显式低频 `public-download` case 验证，
  避免每次租机都重复拉镜像和模型。
- AutoDL Pro、Gitee 昇腾与 CompShare 容器库存有限，夜间高峰（15:00 Asia/Shanghai）常整体售罄。三者都只在“全部候选
  都报告无库存”时等待并重试：AutoDL 为每个候选规格都返回「暂无库存」，Gitee 为每个 profile 的创建都返回
  「没有找到符合条件的可用主机」（它没有库存接口，只有创建才能探知），CompShare 容器为每个卡型（4090 / 4090_48G /
  5090 等）在所有容器可用区的创建都被供给类错误拒绝（容量不足、`RetCode 226604` 资源不足、`8433` 服务抖动、`230`
  机型未配置到该可用区、`226641` / `226603` 镜像不支持）。重试每 5 分钟整轮重走一遍，总等待默认 30 分钟，
  由 `AUTODL_STOCK_WAIT` / `GITEE_STOCK_WAIT` / `COMPSHARE_STOCK_WAIT` 覆盖（Go 时长如 `45m`，`0` 关闭）。等待期间没有创建任何实例，
  其它错误（余额不足、鉴权、价格超限，或有卡型因 SSH 端点未出现等非供给原因被跳过）不重试；AutoDL 指定了具体卡型时不换卡也不等待。
  等待时间叠加在用例的 setup 预算之上（CompShare 容器的租机发生在 setup 之前，同样额外加上），所以
  `test-l4-autodl-node-onboarding`、`test-l4-gitee-ascend-partition-capability` 与
  `test-l4-compshare-server-dial-node-onboarding-container` 的 `go test -timeout` 与 CI `timeout` 都已各加 30 分钟。
  AutoDL 每小时价格上限默认 10 元（`AUTODL_MAX_HOURLY_PRICE`）。
- CompShare 可通过 `COMPSHARE_GOLDEN_IMAGE_ID` 复用预热 VM 模板，通过
  `COMPSHARE_DOCKER_IMAGE_TAR` 指定 VM 内可见的镜像归档，通过
  `COMPSHARE_PRELOADED_MODEL_PATH` 注册每台节点已存在的完整模型目录。
- CompShare 的 V100 / V100S（Volta，sm_70）不能用通用的 vLLM 镜像：上游预编译内核不含 sm_70，
  引擎会以 `no kernel image is available for execution on the device` 退出。`l4-compshare-single-vm-tp2-dgpu`
  遇到 V100S 时改用 1Cat-vLLM 构建的 `cuda12.8-v100-vllm1cat1.5.1`（`build/runner-images`，矩阵项标
  `catalog: false`，不进系统镜像目录），并断言实例确实以该镜像启动；`COMPSHARE_V100_TEST_IMAGE` 可换镜像，
  `COMPSHARE_V100_DOCKER_IMAGE_TAR` 指定 VM 内的镜像归档（没有则由平台在部署启动窗口内拉取）。
  Pascal（P40，sm_61）没有任何镜像能跑，该用例不再租它。镜像须先构建并发布（`build.sh` 然后 `publish.py`）。
  `COMPSHARE_GPU_MODELS`（逗号分隔，如 `V100S`）替换租机的卡型候选顺序，用来把一次运行对准某个卡型。
- `prepare-compshare-golden-image` 创建并验证新模板，但不会删除旧模板或修改 CI 变量。
  模板里同时存放同一版 NVIDIA 驱动（当前 580.95.05）的开源与闭源两套内核模块，开机时
  `gpuworks-nvidia-kmod.service` 先加载开源版（RTX 5090 等 Blackwell 卡只能用它），没有初始化出 GPU
  时换成闭源版（V100S、P40 这类没有 GSP 的卡只能用它），选中的是哪套写在 `/run/gpuworks-nvidia-kmod`。

  模板里的 runner 是 `gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2`，模型是 Qwen3-0.6B（`/opt/gpuworks/test-fixtures/models/qwen3-0.6b`，
  对应 `COMPSHARE_PRELOADED_MODEL_PATH`）；不用 `cuda13.0` 镜像，是因为它不带 Turing 内核，跑不了最便宜的 2080 Ti。
  源模板的驱动不是这一版时，工具先换掉用户态驱动：runner 镜像要求驱动不低于其 CUDA 版本（toolkit 按 `NVIDIA_REQUIRE_CUDA`
  拒绝启动），而靠镜像自带的前向兼容库补差在 GeForce 卡上不可用（`Error 804`），580 是仍支持 Pascal、Volta 的最后一个分支。
  源模板里没有该 runner 时，工具按 `-pull-sources`（默认华为云 SWR、quay.io、Docker Hub）
  拉取；`-remove-images` 列出的旧 runner 会从 Docker 存储里删掉，免得留在新模板里。验证时每个卡型都在 runner 里做一次真实的
  CUDA 调用，证明驱动带得动 runner 的 CUDA 运行时。工具还把 Docker 切到 cgroupfs cgroup 驱动（与阿里云模板相同）：新模板首次开机时
  systemd 会 reload 两次，systemd 驱动下这会收回正在运行的容器的 GPU 设备权限，容器里随即报 `No CUDA GPUs are available`。
  验证失败时加 `-keep-failed-validation` 保留验证机（登录凭据写在当前目录的 `<run-id>.ssh`，查完用 `cleanup-leaked -run-id` 删除），
  `-revalidate-image <id>` 只对已有模板重做单机验证。

  工具做完模板后按 `-validate-gpus` 逐个卡型开机验证，也可用 `-validate-image <id>` 只验证已有模板；
  没货或被 CompShare 拒绝的卡型只报告不判失败。工具也会在镜像缺 nvidia-container-toolkit 时装上与华北模板相同的版本并配置 Docker 运行时
  （上海那条模板线原本没有，容器里拿不到 GPU）。

  自定义模板按地域隔离，本账号的接口不支持跨地域复制，
  各地域要分别以该地域已有的模板为源制作（`-region`/`-zone`/`-source-image`，上海只有 2080Ti×4
  时用 `-gpu-model 2080Ti -gpu-count 4`），结果写入 `COMPSHARE_GOLDEN_IMAGE_ID` 与
  `COMPSHARE_ZONE_IMAGE_CN_SH2`。

  CompShare 不允许自定义模板用于 A800、H20
  （`RetCode 226603`），与驱动无关；模板名超过 42 个字符会在建模板时被拒（`RetCode 230`）。

失败时优先冻结现场并查看进程/容器日志：先保留失败实例、记下本次运行的 run-id 和 SSH/agent 日志，
完成排查后再做 scoped cleanup，即限定本次运行范围的清理。云用例可按实现支持设置
`GPUWORKS_CLOUD_KEEP_ON_FAILURE=1`，之后只用本次 scoped run-id 调用
`test-l4-cleanup-leaked` 或相应清理入口；禁止无范围清理整个账号。云端 L4 结束后要按 run-id
确认没有实例、转发或临时文件泄漏。产品侧的租机操作见[租用云 GPU](../how-to/rent-cloud-gpus.md)。

### 阿里云 ECS

阿里云用例覆盖 CompShare 的全部 VM 场景：`l4-aliyun-provisioning-matrix`（agent_dial 接入、直连 server_dial 接入、
库存形状、双卡规格）、`l4-aliyun-single-vm-tp2-dgpu`、`l4-aliyun-one-vm-two-logical-nodes`、
`l4-aliyun-two-vm-docker-distributed`、`l4-aliyun-pd-deploy-chat-two-vm-docker`、
`l4-aliyun-two-vm-udocker-distributed`与低频的 `l4-aliyun-modelscope-public-download`（`CLOUD_DOWNLOAD=1` 才跑）。
它们租 ECS GPU 实例，并与 CompShare 共用同一套场景实现（`vmBackend`：租机、选卡型、Docker 与镜像、模型从哪来由 provider 提供）。

- **抢占式优先，没货回落按量**：先用 `SpotAsPriceGo` 下抢占式实例；库存快照与创建之间被别人买走、
  或被拒绝（`OperationDenied.NoStock`、`QuotaExceed.*`、`InvalidResourceType.NotSupported` 等）时，同一用例内
  改下按量付费（PostPaid）实例，仍无货再换下一个可用区或下一种卡型。余额不足、AccessKey 无效、镜像不存在这类账号或
  夹具问题不会被吞掉，直接失败。每次尝试及原因写入用例日志（`rental attempt:`），实例的计费方式、可用区和规格
  也会记录。`ALIYUN_NO_SPOT=1` / `ALIYUN_NO_ON_DEMAND=1` 可去掉一种计费方式。
- **候选来自实时库存**：`DescribeAvailableResource` 按地域分别查抢占式与按量库存；卡型按 T4、A10、L20 由便宜到贵，
  GPU 张数精确匹配（`internal/testenv/cloudprovider/aliyun_shapes.go`）；跨卡型也是抢占式优先：先试最佳放置是抢占式的卡型，
  只有按量库存的卡型（如 2×A10）排在后面。V100 不在其中：cuda12.8 runner 的 PyTorch 没有 sm_70 内核。
  2×T4（gn6i）不用于张量并行用例：实测 vLLM 0.17.1 在其上 TP=2 的 CUDA graph 捕获报 `CUDA error: out of memory`，
  agent 注入 `CUDA_VISIBLE_DEVICES=0,1` 时 worker 还初始化不了 NVML；该规格仍可用于接入与规格类用例。
- **双机用例同可用区**：第一台买下后，同一卡型与张数的后续租机固定在同一地域、可用区和规格（先试同样的计费方式，被拒再换另一种），
  两台才在同一 VPC 内用私网互通。普通安全组内的实例默认互通，不需要自引用规则（接口会拒绝）。
- **网络长期保留**：每个地域一套 VPC `gpuworks-test-infra`（172.30.0.0/16）、按可用区的 vSwitch 与安全组，
  缺什么建什么，之后复用；安全组放行 22 与 agent 端口 18800（来源 `ALIYUN_INGRESS_CIDR`，默认任意）。
  这套网络不属于任何一次运行，run 级清理不会删，也不会碰账号里已有的 VPC。
- **产品路径**：agent 经产品自己的阿里云 provider driver 安装。`DirectServerDialOnboarding` 走 `server_dial`（ECS 有公网 IP，
  server 直接拨 agent；产品对 ECS 默认拨私网地址，所以用例显式给出公网 `host_address`），另外三个用例走 SSH 反向转发的
  `agent_dial`，此时 Cloud Assistant 在线，agent 由 Cloud Assistant 以 root 安装，不开 SSH 会话，也就不写入平台 SSH key
  （用例对这条路径不要求 `Platform key`）。若 runner 直连云主机地址被网络设备劫持（本地曾对个别公网 IP 的任意端口回
  `HTTP 403`），设 `GPUWORKS_SSH_PROXY=socks5://host:port`，用例的 SSH 会走该代理（`server_dial` 的 TCP 不走代理）。产品 driver 仍不创建、不释放实例；
  创建与释放只在测试用的 ECS 客户端里（`aliyun.RPC` 复用 driver 的 V3 签名、重试与 STS）。
- **清理**：实例名为 `gpuworks-test-<run-id>` 并带标签 `gpuworks-test-run`，`DeleteInstance` 带 `Force`，
  等实例从列表里消失才算释放；系统盘随实例释放，`cleanup-leaked --run-id` 还会按标签找并删除残留云盘。
  泄漏扫描逐个地域列实例，范围是 `ALIYUN_REGIONS`，没设就取有黄金镜像（`ALIYUN_IMAGE_<REGION>`）的地域，这些地域列不出来
  就报错；两者都没有时扫账号可见的全部地域，并跳过 key 列不出的地域（会在 stderr 说明）。
  抢占式实例被回收时，失败用例的日志会出现 `SPOT INTERRUPTION`，提示失败多半是回收的后果，重跑即可。

- **新机首个容器与重试**：新开机的 A10 虚机上，第一个 vLLM 容器偶发启动失败（`torch._C._cuda_init()` 报
  `No CUDA GPUs are available`，也见过 `NVMLError_Unknown`），平台重试同样的容器就成功了；失败那一行会留在部署的实例列表里。
  所以多机用例在 `EnsureDocker` 里先用 agent 同样的方式（`--gpus "device=N"`）在每张卡上跑一次 CUDA 预热并重试，
  日志里记 `GPUs ready for containers after N warm-up tries`；等待部署运行时只数存活的 rank（失败/停止/移除中的行不算），
  每个失败的尝试会立刻把引擎日志写进用例日志。此前 PD 用例因为把失败行也算进 rank 数，部署明明已经 2/2 运行也一直等到超时。

- **udocker 用例**：这个用例要求三件事。agent 状态不能放在 `/tmp` 下（所以阿里云 backend 的 `HostedRoot` 是 `/opt`）：udocker 把一个私有目录绑到 guest 的 `/tmp`，
  盖住了 `ensureHostRootLoopback` 在 `ROOT/tmp/...` 下放的回环链接，引擎按 `/proc/self/maps` 里的宿主路径 dlopen `libcudart` 就找不到（实测文件在 ROOT 里，guest 里解析不到）。
  root 登录的 agent 以专用用户 `udocker` 跑工作负载，所以 `$root` 要 `chmod 711`，否则 PRoot 报 `fork/exec .../proot-x86_64: permission denied`。
  udocker 有自己的镜像存储，黄金镜像里 Docker 存的 runner 镜像它用不上；该用例不去 registry 拉（每个 rank 拉一遍要 25 到 40 分钟，偶尔拉到末尾换源重来），
  而是走平台的「从 URL 导入」：每台 VM 上 `docker save` 出未压缩的归档（约 14 GiB，约 4 分钟），用一个支持 HEAD 与 Range 的小文件服务（`python3` 脚本，只绑私网地址 18881，只服务这一个文件）发布，
  普通私网默认可访问；夹具预置的 `outbound_allow_private_hosts` 在开启 `outbound_restrict_private_hosts: true` 的严格模式时用于授权两台 VM。用例再像运维一样调管理 API：`POST /images/preview-archive-url` 解析归档、
  把两个归档 URL 登记到 runner 镜像的获取地址（目录没有该镜像时按控制台的自定义镜像表单新建）、对每个节点 `POST /images/{id}/nodes/{uuid}/sync`（`source=url`）让节点从**自己所在 VM** 直连下载并 load，
  等到节点镜像行为 `present` 才创建部署，部署之后还会确认镜像行仍是 `present`、来源仍是 `url`。归档与文件服务在导入完成后立即删除/停止。
  用例日志记录每个 rank 的 `save`、`download`、`load` 与合计耗时（下载与 load 的分界取自镜像行的消息，精度为轮询间隔）；2026-10-27 实测 save 约 4 分钟，下载约 1 分钟、load 约 15 秒，整个用例约 11 分钟。
  系统盘仍是 300 GiB：导入期间同一块盘上同时有 Docker 存储、待发布的归档、节点下载的归档和 udocker 层（两份归档在 load 后即删），之后还要解包容器。
  整个用例用按量实例而不是抢占式（`GPUSpec.OnDemand`），避免中途被回收。
  失败时 `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` 现在对多机用例也生效：失败的 VM 会保留并写出凭据文件。

黄金镜像由 `make prepare-aliyun-golden-image` 制作：以阿里云公共镜像
`ubuntu_22_04_x64_100G_with_gpu_driver_and_cuda_alibase_*`（驱动与 CUDA 已装）起一台 T4 实例，
装 Docker 29 与 NVIDIA container toolkit（Docker CE 源依次试 `mirrors.aliyun.com`、清华、中科大、华为云，
某个源正在同步、索引校验不过就换下一个；续跑前先删掉上次写下的 Docker 源）、拉取 vLLM runner 镜像（优先华为云 SWR 镜像，
再 quay.io、Docker Hub，最终标签 `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`）、下载 Qwen3-0.6B 到
`/opt/gpuworks/test-fixtures/models/qwen3-0.6b` 并写校验和，停掉 Docker、同步后 `CreateImage`，再 `CopyImage`
到 `-regions`（默认华东1、华北2、华南1、华南3）并在源地域开一台新机验证（先验证再复制，坏镜像不会白白复制到四个地域）。镜像约 50 GB（runner 镜像占大头），
150 GiB 系统盘的首个整盘快照约 90 分钟，跨地域复制再约一小时，整个过程要预留两到三小时。自定义镜像按地域隔离，
所以每个地域一个 ID，输出的 `ALIYUN_IMAGE_CN_HANGZHOU=` 等行写进 `secrets/cloud-providers.env`
或 CI 环境变量；用例只在有镜像的地域租机。工具不删旧镜像、不改 CI 变量；
`GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` 时失败的源机保留并写出 0600 凭据文件，
可用 `-resume-credentials` 接着做。
模型与 runner 都可以换：`-model-repo`/`-model-rev` 指定 ModelScope 模型，`-model-path` 指定镜像内路径，
`-docker-image`/`-pull-sources` 指定 runner，默认镜像名随模型生成（`gpuworks-golden-<模型>-<UTC 时间>`）；
`-source-regions` 限定源机与验证机的租用地域（默认同 `-regions`），复制到海外等地域时用它把源机留在有库存的大陆地域。
L4 用例按默认的 Qwen3-0.6B 注册模型；单机用例在 A10、L20 等卡上部署 `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`，在 T4 上部署 `gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2`（用 `-extra-image` 一并装进镜像），多机用例各卡都用后者。换了模型或 runner 的镜像不能直接给 L4 用。镜像里已有 Docker 与模型，用例只验证、不安装、不下载
（缺了就是镜像坏了，直接失败）。

CI 需要的变量（作用域 `cloud-test/aliyun/*`）：`ALIYUN_ACCESS_KEY_ID`、`ALIYUN_ACCESS_KEY_SECRET`
（建议只授 ECS 与 VPC 权限的 RAM 用户）、每个地域的 `ALIYUN_IMAGE_<REGION>`；可选 `ALIYUN_REGIONS`、
`ALIYUN_INGRESS_CIDR`、`ALIYUN_NO_SPOT`、`ALIYUN_NO_ON_DEMAND`。

## 证据

测试方法写在本文；一次性运行日志、截图、镜像 digest 和环境事实写到
`artifacts/work/<YYYY-MM-DD>-<主题>/<主题>-<YYYY-MM-DD>.md`（收口后随主题搬进
`artifacts/archive/`），现行文档只链接证据。

