# 控制面集群的行为与边界

本文说明三成员控制面集群在成员故障时怎样表现，以及这一版不做什么。

社区发行版不含集群运行时，也没有集群命令：集群引擎留在本仓库的 `dqlite` 构建标签后，默认构建不编入。安装命令、数据库辅助程序 `gpuworks-dqlite` 及其原生构建由带集群运行时的发行版提供，安装步骤见该发行版的文档。

## 组成

- 三个成员各运行一个 `gpuworks` server。server 是纯 Go 程序；数据库由它启动的辅助程序 `gpuworks-dqlite` 承载，三个辅助程序之间用成员证书做双向 TLS，复制同一个数据库。
- 三个成员都是投票成员，写入需要其中两个确认。
- 用户和 agent 只访问一个稳定入口（负载均衡或 keepalived VIP）。成员地址不是受支持的入口。
- 默认构建和 macOS 仍是单个 SQLite 节点，不加入集群。

## 故障时的行为

- **失去一个成员**：其余两个继续服务。入口应当依据 `GET /api/v1/readyz` 把请求转给返回 200 的成员。
- **失去多数成员**：剩下的成员拒绝管理写入，返回 503；不会缩小成员集合来继续写。
- **调度跟随数据库 leader**：只有持有数据库 leader 的成员运行调度和控制循环；leader 变化后由新的 leader 接手。没有单独的调度租约。
- **业务由数据库 leader 所在的成员提供**：其他成员收到业务请求（管理接口、控制台、`/v1` 推理、agent 接入）后，经成员之间的双向 TLS 通道原样转给它，所以经入口访问时落在哪个成员上结果都一样。`/healthz` 和 `/api/v1/readyz` 由收到请求的成员自己回答。暂时观察不到 leader 的成员对业务请求回 503，入口可以重试。
- **向不能提交写入的成员发写请求**：返回 503，没有任何内容被提交，入口可以改发给别的成员。
- **成员尚未开放业务**（刚启动、与多数失联，或材料尚未校验完成）：业务请求和 `/api/v1/readyz` 都返回 503，正文是 `member business service is not ready`。
- **成员为什么拒绝了请求**：上面这种 503，以及 leader 对未被接纳的成员转来的请求所回的 403，都带响应头 `X-Gpuworks-Refusal`，值是下表中的一个代码。它在鉴权之前给出，只含代码，不含地址和身份。带令牌的 `GET /api/v1/healthz` 在出现过拒绝之后多一个 `business_refusals` 字段，按代码给出进程启动以来的次数。两者是给运维和采集工具看的诊断信息，代码可能随版本变化，不要让客户端依赖它们。

  | 代码 | 含义 |
  |---|---|
  | `no_business_generation`、`generation_changed`、`no_business_handler` | 成员的业务处理还没装上，或在请求途中被撤下 |
  | `cluster_upgrade` | 集群升级期间业务关闭 |
  | `proof_missing`、`proof_expired`、`proof_stopped`、`proof_run_canceled`、`proof_canceled`、`proof_not_eligible` | 成员当前没有有效的资格确认 |
  | `member_revoked` | 数据库拒绝了这个成员的接纳：它已被撤销 |
  | `cluster_identity_conflict` | 数据库属于另一个集群，或它的协议版本与这个程序的不同 |
  | `sessions_not_routable`、`target_not_current` | 成员不能接收会话调用，或它的目标版本不是当前版本 |
  | `no_known_leader`、`not_the_leader`、`leader_unreachable` | 最近几秒没有观察到 leader；转来的请求到了不是 leader 的成员；转给 leader 失败 |
  | `forward_peer_unidentified`、`forward_not_ready`、`forward_peer_undecided`、`forward_peer_not_admitted` | leader 一侧：转发方没有成员证书；成员通道未就绪；对转发方的资格得不到答复；转发方未被接纳（403） |
  | `proof_headroom`、`proof_epoch_changed`、`proof_owner_changed`、`renewal_unavailable`、`request_canceled`、`request_deadline` | 只出现在 `/api/v1/readyz`：资格确认即将到期或在探测途中变化，探测被取消或超时 |
  | `other` | 表中没有的原因 |
- **成员为什么关闭了业务**：成员关闭业务接纳时，在日志里留一行 `closed business admission`，其中 `caller` 是下表中的一个原因，`first_of_period` 说明它是不是这一段关闭期的第一次。同一段关闭期里每个原因只记一行；成员已经关闭之后，资格确认自己的失败不再记这一行，而是记在 `candidate material proof failed while business admission is closed` 里。恢复时记一行 `candidate material proof succeeded after business admission closed`，其中 `close_callers` 是这段时间里出现过的原因个数。`caller` 的值只会是表里的词，不含函数名和行号；按旧形式（例如 `caller=app.(*materialEligibility).Reprove:422`）匹配日志的采集脚本需要改成匹配这些词。与拒绝代码一样，它们是诊断信息，可能随版本变化。

  | `caller` | 含义 |
  |---|---|
  | `proof_failed` | 一次资格确认得到了否定结论，或者太久得不到答复 |
  | `proof_expired` | 上一次资格确认的有效期用完了 |
  | `proof_canceled`、`proof_context_ended`、`proof_run_canceled` | 进行中的资格确认被取消；它的上下文因有效期以外的原因结束；确认还没开始，后台任务就结束了 |
  | `proof_stopped`、`member_stopping` | 资格确认的后台任务被停止；成员正在停止自己的业务 |
  | `transport_refresh_refused`、`writer_refused` | 成员间连接的资格刷新得到了否定结论；数据库拒绝了一次写入，成员已不被接纳 |
  | `member_absent`、`own_origin_changed`、`origin_registered` | 成员不在观察到的成员名单里；成员的登记地址与已登记的不一致；有成员登记了变化后的地址 |
  | `material_probe_rejected`、`material_startup_rejected`、`material_check_rejected`、`material_pass_rejected` | 关键文件的检查得到了否定结论：对已发布清单的探测、启动后的第一轮同步、对已安装文件的定期检查、一轮同步 |
  | `material_received`、`material_installed` | 接收到其他成员送来的关键文件变化；同步改动了本成员已安装的关键文件 |
  | `other` | 表中没有的原因 |
- **存储短暂卡顿**：成员凭自己最近一次确认过的成员资格接纳请求。确认由后台每隔几秒续期，单个请求不等待它，所以几秒的存储卡顿不会因为授权拒绝或拖慢请求。续期得不到答复（存储卡顿、超时、找不到数据库 leader）时成员继续接纳请求，距上一次成功确认开始满 60 秒仍未续上才关闭业务。向其他成员同步关键文件的一轮如果超时、找不到数据库 leader，或者一时得不到多数成员的应答（例如一个成员停机、另一个暂时拒绝），同样不关闭成员；文件与已校验的内容不一致、缺失或读不出才关闭。
- **成员被撤销或失联之后**：
  - 写入：已被撤销的成员即使仍能连到数据库 leader，写入也在提交前被拒绝，随后它关闭自己的业务；与多数失联的成员本来就提交不了。检查本身失败（写入权被占用、找不到 leader、连接中断、超时）不算拒绝，原样返回给调用方。决定成员资格本身的写入、向空数据库载入备份或导入数据、以及 server 启动时自己的工作不经过这项检查。
  - 读请求和推理请求：数据库答复该成员已被撤销时，成员立即关闭；后台的资格确认被拒绝之后，它上面的请求得到 503，拒绝代码是 `member_revoked`（先由被拒绝的写入关闭、或进程刚重启时，在下一次确认被拒绝之前的几秒里代码是 `proof_missing`）；后台的资格确认在上一次成功后最多 4 秒开始下一次，所以没有写入的成员通常也在撤销后几秒内关闭。只有针对本成员自己的拒绝才立即关闭它：别的成员被撤销、集群处于不提供服务的维护阶段，都不会让它立即关闭。这一版没有撤销或移除单个成员的命令：撤销只发生在从备份恢复时，恢复出来的是只有一个成员的集群，其余成员重新加入，所以不会出现“已被撤销、却仍在数据库成员集合里”的成员。续期一直得不到答复时，最长到上一次成功确认开始后的 60 秒；这段时间里它按最后看到的数据应答。日志里怎么看，见[排障指南](../how-to/troubleshooting.md#控制面集群的成员拒绝请求)。
  - 成员之间的连接：凭 15 秒内确认过的成员名单接纳；确认不了时最长沿用到 60 秒，与成员自己接纳请求的上限相同；因此存储卡顿期间被撤销的成员，最长 60 秒内仍会被其他成员接受。提交撤销的那个成员立即丢弃名单。已经建立的连接只在建立时核对一次。

## 边界

下面这些是这一版的限制。

- **进行中的流不续传**。故障切换时，已经打开的流式回答、镜像或模型传输随持有它的成员断开；客户端需要重新发起。请求被转发之后发生的失败不会被重试。
- **leader 变化时 agent 保持连接，进行中的推理请求中断**。agent 的连接留在接受它的成员上，由该成员把它接到 leader；leader 换到另一个成员后，这个成员改接到新的 leader，agent 只会再收到一次完整的期望状态。这段时间（实测约一秒到数秒）节点可能短暂显示为连接中，经过该节点的推理请求返回 503，等待回答的调用和已打开的流（日志跟随、终端、传输）中断。接受 agent 连接的那个成员本身停止时，agent 重新连接。运行中的实例不受影响。全部业务流量经过一个成员，集群提供的是可用性，不增加容量。
- **成员密钥只在从备份恢复时被吊销**。这一版没有在运行中吊销单个成员的命令。恢复要求所有成员停机，恢复出来的那个成员从启动起就拒绝被吊销密钥的数据库连接；运行中的成员不会在运行期把吊销加进自己的数据库连接策略。被吊销的成员自己，写入立即被拒绝，业务在它下一次资格确认时关闭（几秒）。
- **没有恢复时间保证**。故障后恢复写入需要一次 leader 选举和成员重新开放业务，耗时取决于存储的同步写延迟和网络。慢盘上会明显变长；这里不给出任何保证的秒数。安装时确定的[存储档位](#存储档位)决定选举等待时间，它列出的是恢复目标，不是保证。
- **成员不能用单机的命令升级、重启或停止**。`gpuworks upgrade`、`gpuworks restart`、`gpuworks stop` 和控制台里的升级只作用于一台 server，不认识成员自己的服务单元、它的数据库辅助程序和其他成员；在集群成员的安装目录上它们会直接拒绝，不做任何改动。集群用提供集群的发行版的 `gpuworks cluster upgrade` 升级；成员的启停通过它自己的服务单元进行。
- **少于三个成员不受支持，也不要手工移除成员**。集群只剩两个成员时，数据库辅助程序只保留一个投票成员，另一个成员不再接收数据，再有一台故障集群就停止服务。成员所在的主机故障后，把它留在成员集合里：三个投票成员剩下两个，仍能提交写入。这一版没有更换成员的操作步骤。成员离开、失去投票权，或有投票成员连不上时，集群状态报告显示 `Overall: degraded` 并写明原因，不再显示 `ready`；集群还在组建时不算：最近一次准入成员后的五分钟内，且从未吊销过成员。
- **备份是手动的，恢复要停掉整个集群**。集群不自动备份。一个成员可以在集群照常服务时把数据库的完整镜像、主密钥、集群身份和数据库引用的文件写成一个归档；恢复时全部成员停止，由取备份的那个成员用归档重建数据目录，另外两个成员重新加入。归档之后的写入和登录会话在恢复后不存在。单机的 `gpuworks upgrade` 备份不适用于集群：复制库不接受它用的快照方式。
- **首次启动慢**。新数据库上的迁移逐个提交并同步到多数成员。
- **只支持 Linux amd64 成员**；辅助程序是唯一使用 cgo 的制品。

## 存储档位

集群的每次提交都要等多数成员完成一次同步写。数据库在一段时间联系不上 leader 后重新选举，这段时间由存储档位决定，三个成员必须相同。档位在安装时确定，由首节点记录在 `<数据目录>/cluster-enrollment/config.json` 的 `storage_tier` 里（`standard` 不写该键），其余成员加入时从集群取得。

| 档位 | follower 发起选举前的等待 | 适用存储 | 杀掉 leader 进程后的恢复目标 |
|---|---|---|---|
| `standard` | 150–300 ms | 同步写不出现 150 ms 以上停顿 | 10 秒 |
| `slow-storage` | 0.75–1.5 s | 同步写有数百毫秒到数秒的停顿（如共享的网络存储） | 6 秒 |

恢复目标来自小样本实测，不是对其他存储的保证。三个成员的存储同时停顿数秒时，两个档位都会有请求失败。

档位名之后可以写它运行的数据库延迟，例如 `slow-storage@100ms`：整数毫秒，10 到 200，一种时间只有一种写法：延迟等于某个档位的原值（`standard` 10 ms、`slow-storage` 50 ms）时就是那个档位，其他延迟一律写在 `slow-storage` 上（`standard@50ms` 记录为 `slow-storage`）。只有本版本及之后的程序认识带延迟的写法，更早的 server 读到它会拒绝启动。数据库的其余时间都从延迟得出：心跳间隔是延迟的 1.5 倍，选举超时是 15 倍（leader 这么久收不到多数应答就让位，follower 等一到两个选举超时后发起选举）。跟着变的还有成员资格授权的重试时间（两个选举超时加一次提交，不少于 2.2 秒，不超过 4.7 秒；延迟约 140 ms 以上时一次选举可能比它长）和恢复目标（加上两个多出来的选举超时）。延迟超过 50 ms 时按比例加长的还有两个原本固定的时限：业务层把 leader 观察当作有效的 3 秒（100 ms 时 6 秒，200 ms 时 12 秒），和成员之间连接无确认即断开的 3 秒（同样 6 秒、12 秒）；否则一次正常的选举就会让每个成员拒绝业务，或在让位的同一时刻断开连接。不随延迟变化的是成员资格授权本身的 5 秒时限（重试时间的上限 4.7 秒由它而来）和依赖它的材料资格证明窗口。成员的 server 启动时读记录，所以改记录后要重启该成员才生效；三个成员应当记录相同的值，逐个重启期间允许并存的条件是较慢一方的三个心跳间隔不超过较快一方的选举超时。企业版的 `gpuworks cluster-storage-tier` 命令显示和更换这个记录。

## 数据库 leader 为什么换了：角色记录

数据库 leader 每换一次，在途的业务请求、终端和日志跟随会被切断，agent 的会话要向新的 leader 重新登记。每个成员的 server 日志（集群安装的成员是 `<安装目录>/logs/server.log`；[收集诊断信息](../how-to/troubleshooting.md#控制面集群诊断包里有三个成员的记录)把三个成员的都带走，放在 `members/<成员>/` 下，角色记录另抽出为 `role-events.log`）都记下本成员的数据库进程在 Raft 里做了什么、为什么，一直开着，不需要调试构建：

- `cluster database role event`：数据库进程的一条角色记录。`observed_at` 是事情发生的时间（数据库进程的时钟，纳秒），日志行自己的时间晚不到一秒。
- `cluster database leader changed`：本成员认到了新的 leader。写明原来的 leader（`old_leader`）、新的 leader（`new_leader`，成员 id）、任期（`term`）、本成员是不是新的 leader（`leads`）、本成员看到的原因（`reason`）和本成员没有 leader 的时长（`without_leader_ms`，按数据库进程的单调时钟量，不受校时影响）。
- `cluster database leader kept after a campaign`：本成员发起过选举，但随后跟随的还是原来的 leader（`leader`），任期没有变。它不是 leader 变化，查 leader 变化时只看上一条。同一个成员丢掉 leader 又重新当选是一次变化：上一条里 `old_leader` 与 `new_leader` 相同，`old_term` 与 `term` 不同。
- `business follows a new database leader`：本成员的业务层开始把请求发给另一个 leader。`agent_sessions_closed` 是它为了让 agent 会话向新的 leader 登记而关掉的会话数。

`event` 的取值：

| `event` | 谁写 | 含义与要看的字段 |
|---|---|---|
| `state` | 每个成员 | 角色变化，`old` → `new`（`FOLLOWER`、`CANDIDATE`、`LEADER`，`UNAVAILABLE` 是启动前和停止后）；`id`、`address` 是本成员 |
| `election_start` | 发起选举的成员 | `reason=election_timeout`：这么久没听到 leader（`since_leader_contact_ms`，不小于选举超时 `timeout_ms`）；`reason=transfer_requested`：原 leader（`from`）要求它接手 |
| `election_retry` | 候选成员 | 一轮选举没拿到多数票，再来一轮 |
| `pre_vote_won`、`election_won` | 候选成员 | 预投票通过；当选，`votes`/`voters` 是得票与投票成员数，`term` 是新任期 |
| `vote_reply` | 被请求投票的成员 | `candidate` 是谁，`granted=1/0`，不给票时 `reason` 是 `has_leader`（它还听得到 leader）、`local_term_higher`、`log_or_earlier_vote`、`installing_snapshot`；`disrupt_leader=1` 表示这是一次交接 |
| `step_down` | 原 leader | `reason=no_majority_contact`：一个选举超时内没有收到多数成员的应答，`peer_ack_age_ms` 是每个其他成员 `id:角色:上次应答距今毫秒`（`-1` 为从未应答）；`reason=higher_term`：`from` 那个成员已经进入更高的任期 |
| `term_bump` | 跟随的成员 | 从 `from` 听到更高的任期 |
| `following` | 跟随的成员 | 开始跟随 `leader`（`leader_address`）；同一个 leader 在新任期重新当选时也写 |
| `transfer_start`、`transfer_end`、`transfer_timeout` | 原 leader | 把 leader 交给 `to`；`result=leadership_left` 是交出去了，`still_leader` 是没交成 |
| `membership_request`、`configuration` | leader；每个成员 | 成员集合或角色的变更请求（`op=add/assign/remove`）；收到的新成员表 `members=id:角色,…` |
| `snapshot_take`、`snapshot_install` | 每个成员 | 做了一次快照、装入了 leader 发来的快照；`took_ms` 是用时，`status` 是 `ok`、`failed`（带 `code`）或 `unfinished`。不到 250 毫秒的每 10 分钟只写一条 |
| `slow_step` | 每个成员 | 数据库进程主循环里的一步用了 `took_ms` 毫秒（达到 100 毫秒才写）：`step` 是 `recv`（处理一条消息）、`tick`、`apply`（应用已提交的日志，`entries` 条）、`fsm_apply`（其中一条）、`snapshot_begin`、`snapshot_restore`。这一步进行时本成员既不发心跳也不应答 leader |
| `loop_late` | 每个成员 | 数据库进程的主循环晚了 `late_ms` 才轮到（进程没被调度，或循环里有一步太久）；达到 250 毫秒才写 |
| `slow_log_write` | 每个成员 | 一次 Raft 日志落盘用了 `ms` 毫秒；达到 500 毫秒才写 |
| `peer_connect_failed`、`peer_send_failed` | 每个成员 | 到另一个成员（`peer`、`peer_address`）的连接失败（第 `attempts` 次）、发送失败 |
| `peer_connected`、`peer_accepted`、`peer_disconnected` | 每个成员 | 到另一个成员的连接建立（试了 `attempts` 次，用时 `took_ms`）、接受了它连过来的连接、连接断开（`dir=out` 是本成员连出去的，`dir=in` 是对方连进来的） |
| `peer_io` | 原 leader；发起选举的成员 | 每条记录最多两个成员，成员多时连写几条；数据库进程记住最先见到的八个成员。紧跟 `step_down reason=no_majority_contact`（`trigger=step_down`）和 `election_start reason=election_timeout`（`trigger=election_start`）：每个其他成员 `id:send=…:sent=…:recv=…:out=<状态>/<毫秒>:in=<条数>/<毫秒>`。`send` 是上一条给它的消息交给连接距今多少毫秒，`sent` 是上一次写完成距今，`recv` 是上一次从它读到消息距今（`-1` 为从未）；`out` 是连出去的连接的状态（`up`、`trying`、`down`）和进入该状态多久，`in` 是它连进来的连接数和上次变化多久 |
| `tcp` | 每个成员 | 内核对数据库进程持有的每条 TCP 连接的记录，按对端地址和方向汇总：除了成员之间的连接，本成员自己的 server 和其他成员的 server 连到数据库端口的连接也算在内，所以会有一条对端是本成员自己地址的 `/in`。`peers=<地址>/<out 或 in>;…`：连接数 `n`，上次发出数据、收到数据、收到确认距今的毫秒（`sent`、`recv`、`ack`），重传定时器 `rto`（毫秒）与已退避的次数 `backoff`，未确认的包数 `unacked`，往返时间 `rtt_us`；`max_rto`、`max_backoff`、`max_unacked`、`retrans`（重传包数）是上一条 `tcp` 记录以来的最大值和累计。角色变化之后必写一条（`trigger=role_change`）；平时只在有连接正在重传，或重传定时器达到半个选举超时（不低于 400 毫秒，内核的最小值是 200 毫秒）时写，每 10 秒至多一条（`trigger=interval`） |
| `health` | 每个成员 | 一段时间内量到的最大延迟：主循环晚到（`loop_late_max_ms`）、日志落盘（`log_write_max_ms`）、某个跟随成员两次应答的间隔（`ack_gap_max_ms`，`ack_gap_peer_<id>_max_ms`）、leader 两条消息的间隔（`leader_gap_max_ms`）；`*_over=a/b/c/d` 是达到 100、250、750、3000 毫秒的次数。角色变化之后必写一条（`trigger=role_change`），平时每分钟只在超过选举超时的一部分时写（`trigger=interval`） |
| `app` | 每个成员 | 数据库进程自己的告警（调整角色、加入集群） |
| `proxy_closed` | 每个成员 | 一条经本成员到它自己数据库节点的连接没有道别就断了；健康的集群每秒有好几条，所以每 10 分钟只写一条并带略去的条数 |
| `dropped` | 每个成员 | 有记录没写出来：`reason=reader_behind` 是 server 没来得及读，`library_queue_full` 是数据库进程内的队列满了 |

重复出现的情况（`loop_late`、`slow_log_write`、`slow_step`、同一个成员的连接失败、同一条 `app` 告警）每 10 秒只写一条，`suppressed` 是其间略去的条数。一个与其他成员失联的成员每个选举超时发起一次选举：`election_start`、`election_retry`、`pre_vote_won`、对同一个候选成员的 `vote_reply`、候选与跟随之间的 `state`、角色变化后的 `health`，每一种先照常写 6 条，之后同样每 10 秒一条；安静满一分钟后重新计。server 另有自己的上限：一个数据库进程一分钟最多记 240 条，超出的只计数，下一分钟写一行 `cluster database role events suppressed suppressed=…`。`step_down`、`election_won`、`following`、`dropped`、进入或离开 `LEADER` 的 `state`、紧跟让位、选举和角色变化的 `peer_io` 与 `tcp`，以及 leader 变化的汇总不占这个上限，另有自己的上限：每分钟 1200 条，每一种的第一条必写，超出的同样只计数，下一分钟写一行带 `kind=leadership` 的 `cluster database role events suppressed`。连接的建立与断开（`peer_connected`、`peer_accepted`、`peer_disconnected`，按成员和方向分别计）先照常写 6 条，之后每 10 秒一条。数据库进程每 0.2 秒把攒下的记录交出一次，所以相邻几行在日志里的先后可能与发生的先后差零点几秒，要排先后时按 `observed_at`。数据库进程把记录放进内存里的定长队列，另一个线程写出；写不出去时丢记录并计数，Raft 的主循环不等它。

**怎么读一次 leader 变化**：先在三个成员的日志里找同一时刻的 `cluster database leader changed`，`reason` 说的是这个成员自己看到的；说 `not seen by this member` 时到另外两个成员的日志里看。然后按下面对照：

| 看到的记录 | 结论 |
|---|---|
| 原 leader：`step_down reason=no_majority_contact`，`peer_ack_age_ms` 里两个成员都超过选举超时（下面三行按紧跟它的 `peer_io`、`tcp` 细分） | 原 leader 一个选举超时内没收到应答。再看它此前的 `health`、`loop_late`、`slow_log_write`：`loop_late` 大是它自己没被调度；都正常则是网络或另外两个成员的问题，到它们的日志里看同一时刻的 `loop_late`、`slow_log_write` |
| 其后的 `peer_io trigger=step_down`：两个成员 `send`、`sent` 都小（不到一个心跳间隔的几倍），`out=up`，`recv` 超过选举超时 | 发出去了，没有回来。看同一时刻的 `tcp`：`backoff` 大于 0、`max_rto` 接近或超过选举超时、`unacked` 不为 0，是包在网络上丢了、内核在重传，重传间隔比选举超时长；`tcp` 正常则对端收到了但没有应答，到对端的日志里看 `loop_late`、`slow_step`、`slow_log_write` |
| 其后的 `peer_io`：`send` 或 `sent` 本身就超过选举超时 | 本成员没有发：`send` 大是主循环没有产生心跳（看 `loop_late`、`slow_step`），`send` 小而 `sent` 大是写卡在连接上 |
| 其后的 `peer_io`：`out=trying` 或 `out=down`，此前有 `peer_disconnected`、`peer_connect_failed` | 连接正在重建。`peer_connected` 的 `took_ms`、`attempts` 是重建用了多久 |
| 某个跟随成员：`election_start reason=election_timeout`，原 leader 没有 `step_down`，而是随后 `step_down reason=higher_term from=<那个成员>` | 那个跟随成员这么久没听到 leader 就发起了选举。看它的 `since_leader_contact_ms` 和此前的 `health`（`leader_gap_max_ms`）、`loop_late`：自己的循环晚到，说明是它这一侧的问题；循环正常而 leader 的消息间隔大，是 leader 那一侧或两者之间的网络 |
| 原 leader：`transfer_start`，目标成员：`election_start reason=transfer_requested` | 有人要求交接（升级命令，或成员正常停止） |
| 某个成员的数据库进程刚启动：`following` 之后不到一秒出现 `slow_step step=apply entries=<N> took_ms=<几千>`，随后 `loop_late` | 重启的成员在第一次听到 leader 时，把上一次快照之后的全部日志（最多约 1024 条）在主循环里一次应用完，这几秒里它不应答。用时随条数增长，与数据库总大小无关。此时若另一个成员也不在，leader 会因为收不到多数应答而让位 |
| 原 leader 的日志在那一刻中断，重新开始于 `state old=UNAVAILABLE` | 原 leader 的进程退出或被重启 |

例子（三个成员，原 leader 的数据库进程被冻结 2 秒）。跟随成员的 `health` 里自己的循环正常（`loop_late_max_ms=23`），原 leader 恢复后的第一条记录是 `loop_late late_ms=1990`：问题在原 leader 自己这一侧。

```text
# member 13029695054750815211 (10.240.4.6), a follower: it stopped hearing the leader and won the election
time=2026-11-23T22:50:56.000Z level=WARN msg="cluster database role event" observed_at=2026-11-23T22:50:55.994851832Z event=election_start reason=election_timeout term=2 leader=12341872649424584096 since_leader_contact_ms=1257 timeout_ms=1193
time=2026-11-23T22:50:56.000Z level=INFO msg="cluster database role event" observed_at=2026-11-23T22:50:55.995559204Z event=pre_vote_won term=2 next_term=3
time=2026-11-23T22:50:56.000Z level=WARN msg="cluster database role event" observed_at=2026-11-23T22:50:56.000035853Z event=health trigger=role_change election_timeout_ms=750 loop_late_max_ms=23 loop_late_n=565 loop_late_over=0/0/0/0 log_write_max_ms=32 log_write_n=330 log_write_over=0/0/0/0 leader_gap_max_ms=164 leader_gap_n=651 leader_gap_over=83/0/0/0 leader_gap_peer_12341872649424584096_max_ms=164
time=2026-11-23T22:50:56.203Z level=INFO msg="cluster database role event" observed_at=2026-11-23T22:50:56.019313298Z event=election_won term=3 votes=2 voters=3
time=2026-11-23T22:50:56.203Z level=INFO msg="cluster database leader changed" old_leader=12341872649424584096 new_leader=13029695054750815211 term=3 this_member=13029695054750815211 leads=true reason="this member started an election: election_timeout since_leader_contact_ms=1257 timeout_ms=1193" observed_at=2026-11-23T22:50:56.019320067Z without_leader_ms=24
time=2026-11-23T22:50:56.819Z level=INFO msg="business follows a new database leader" new_leader=13029695054750815211 this_member=13029695054750815211 leads=true learned="leader observation" agent_sessions_closed=0 old_leader=12341872649424584096 since_last_observation_ms=2381

# member 12341872649424584096 (10.240.4.7), the leader before: its own loop was 1990 ms late, then it met the higher term
time=2026-11-23T22:50:56.828Z level=WARN msg="cluster database role event" observed_at=2026-11-23T22:50:56.751267272Z event=loop_late late_ms=1990 period_ms=75
time=2026-11-23T22:50:56.829Z level=WARN msg="cluster database role event" observed_at=2026-11-23T22:50:56.754016735Z event=step_down reason=higher_term was=leader term=2 new_term=3 from=3297041220608546238 from_address=10.240.4.5:9061
time=2026-11-23T22:50:56.829Z level=INFO msg="cluster database role event" observed_at=2026-11-23T22:50:56.765822683Z event=following leader=13029695054750815211 leader_address=10.240.4.6:9061 term=3
time=2026-11-23T22:50:56.829Z level=INFO msg="cluster database leader changed" old_leader=12341872649424584096 new_leader=13029695054750815211 term=3 this_member=12341872649424584096 leads=false reason="this member stepped down: higher_term from=3297041220608546238 new_term=3" observed_at=2026-11-23T22:50:56.765822683Z without_leader_ms=11
```

## 数据库辅助程序的协议版本

server 启动辅助程序时读它的一行就绪声明，其中 `protocol` 是辅助程序实现的协议版本。server 只接受自己支持的版本；版本不同或声明里没有版本时拒绝启动数据库，错误信息写出两边的版本。辅助程序与 server 要成对安装。

## 升级集群的表结构

带新迁移的版本不能逐台滚动上线：三个成员共用一个数据库，迁移一执行，表结构对三台同时生效。集群用一个短的维护窗口完成这一步。

- **换上新程序的成员先等待**。新版本的 server 在旧表结构上能启动：它继续提供数据库副本，业务保持关闭，并在数据库里记下自己的迁移列表。其余成员照常提供业务，所以逐台替换时，直到最后一台被替换，业务都还在。比数据库表结构旧的程序仍然拒绝启动；缺少数据库里已有的某个扩展模块的程序也一样。
- **三步由数据库主成员执行**，每一步都要求三个成员报告的是同一份迁移列表：
  1. 开始：维护阶段从 `serving` 进到 `upgrading`。从这一刻起任何成员都不再接受业务写入，包括还在运行旧程序的成员。
  2. 迁移：核心的迁移和各扩展模块自己的迁移中未执行的部分依次执行一次，每个迁移与它的标记在同一个事务里提交。新版本多出一个模块时，它的表也在这一步建立。中途换主或失败后，在新的主成员上重做这一步，已提交的迁移会被跳过。
  3. 完成：维护阶段回到 `serving`，成员不需要重启，各自重新打开业务。
- **窗口内的回答**：每个成员对业务请求（包括 `/api/v1/readyz`）回 503，产品码 `CLUSTER_UPGRADE_IN_PROGRESS`，带 `Retry-After`。`/healthz` 不变。
- **窗口内看进度**：每个成员在业务监听上回答 `GET /api/v1/cluster/upgrade-progress`，业务打开或关闭时都答，不需要登录（窗口内无法校验登录会话，会话记在业务库里）。内容只有状态码（`idle`、`waiting`、`window`、`migrating`、`unobserved`）、本成员业务是否已打开、迁移的已应用数与目标数、成员总数与已就位数，不含地址、版本和错误原文；同一秒内的请求共用一次读取。控制台收到带上述产品码的 503 后切到「控制面正在升级」页并轮询这份进度，成员恢复服务后自动重新加载。窗口内用浏览器新打开控制台，成员回一张自带全部内容的进度页，状态码仍是 503。
- **中止**只在还没有迁移被执行时可用，维护阶段回到 `serving`；这时成员要换回原来的程序。迁移执行之后没有退路，只能从升级前的备份恢复，所以升级前必须有备份。
- **替换主成员之前先交出主成员身份**。同一个监听器上还有 `lead` 步骤：它发给哪个成员，哪个成员就向当前的数据库主成员要来主成员身份，并在自己成为主成员之后才回答。原来的主成员随后可以停止，其余成员不必等一次选举。只有维护阶段是 `serving`、并且该成员的程序与数据库表结构一致时才接受，因为主成员就是提供业务的成员；交接没有在 10 秒内完成时回答里带原因，主成员仍是原来那个。交接期间正在取的集群备份不受损：备份在读取前后各确认一次主成员；主成员变了、读取中断或一时没有主成员，就稍等后重读，三次都不成才报错，不会留下半份归档。`status` 的回答另带该成员自己持有的 agent 连接数（`agent_connections`），成员说不准时不带这个字段：agent 的连接留在接受它的成员上，停止该成员会让这些 agent 重连一次，升级命令据此决定替换顺序。
- **这些步骤由成员自己的注册监听器提供**，调用方要出示集群成员的证书，也就是要在成员主机上以安装账户执行。业务关闭时管理会话不可用，所以它们不走管理接口。
- **以单成员方式运行的成员只回答状态**。只有一个成员的集群，它的成员一直这样运行；创建集群的那个成员在其他成员加入之后、自己下一次启动之前也是这样。它照常回答其他成员和集群命令向每个成员问的问题（状态、成员发现、仲裁事实、它在诊断采集里的那一份）；`begin`、`migrate`、`finish`、`abort`、`lead` 在它这里被拒绝，回答里带 `refused_code: "single_member"` 和理由，集群不变。集群里已有其他成员时，这个成员重启一次后即可参与升级；只有一个成员的集群在新程序启动时应用迁移，`gpuworks cluster upgrade` 不需要它执行任何步骤。

目前的限制：

- 成员每次启动都报告自己的程序，等待升级期间每隔几秒重报一次。某个成员在窗口内被换成另一个版本的程序并启动后，它的报告随之改变，后面的步骤会被拒绝。不带这项功能的旧版本不会报告，这种情况察觉不到；迁移完成之后它会因为表结构比它新而拒绝启动。
- 成员停机时它最后一次报告仍然有效，只要多数成员在最近 30 秒内重报过，步骤就继续，停机的成员回来后再追上。
- 逐台替换程序并依次调用这些步骤的是提供集群的发行版的 `gpuworks cluster upgrade`；这个仓库的构建没有这条命令。控制台入口和入口层的进度展示还没有。
