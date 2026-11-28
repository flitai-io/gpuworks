# 扩展点：模块契约与可安装扩展

gpuworks 的 server 在核心内部经一组接口组合模块（`app.Module` 与 `app.Host`）。这套契约今天由测试与分叉使用：另一个 Go 模块可以把 server 当作库，加上自己的模块编成新的 server 二进制（下文称「单独构建的 server」）；官方构建不带任何模块。官方与第三方扩展以签名插件包的形式安装，由内置的插件宿主模块经同一套契约接入（[ADR 0071](../adr/0071-installable-extensions.md)）；插件宿主已实现的部分与尚未实现的部分见[可安装扩展](#可安装扩展)。

本页说明开源代码留下的接口、每个接口在哪个线程上被调用、必须遵守的规则，以及开源代码不做的事。不可逆的取舍见 [ADR 0070](../adr/0070-extension-tiers-and-edition-boundary.md) 与 ADR 0071；术语（扩展模块、功能键、可用性、扩展停止调度、插件包、推送表）以 `CONTEXT.md` 为准。

扩展相关的文档、示例与测试都按机制本身描述；示例里的发布者、版本名与插件统一用虚构的 `acme`，就像示例域名用 `example.com`。

## 层级

| 层级 | 运行在哪里 | 放什么 |
|---|---|---|
| T0 核心 | server、agent | 全部通用能力与每个执行点：逐请求、逐插入、逐状态上报、逐认领、逐放置的判断都在这里；社区版本身，按定义免费 |
| T1 内置模块 | server 进程内，Go | 经 `app.Module` / `app.Host` 的核心内部组合；`cli.Options.Extensions` 留给测试与分叉，不是厂商或第三方的分发途径 |
| T2 插件包的服务端部分 | server 的子进程 | 需要 I/O、状态、流程或对接的决定，把结果推成核心执行的表（安装、监管与推送状态已实现，见[可安装扩展](#可安装扩展)；其余见[尚未实现](#尚未实现)） |
| T3 插件包的节点部分 | 节点上的核心工作负载与作业 | 用 GPU 的产品，agent 里不跑插件代码（尚未实现） |
| T4 策略部分 | server 内 | 只预留，不实现 |

本页的接口都属于 T1。T2–T4 的设计见 ADR 0071。

## 组合方式

单独构建的 server（分叉或测试）有自己的 `main`，只调用一次 `cli.Main`：

```go
func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Options{
		Edition:      "acme",
		Extensions:   app.Extensions{Modules: []app.Module{audit.New(), sso.New()}, UI: console.FS()},
		Distribution: distribution.Profile{Edition: "acme", Sources: sources, Keys: keys},
		InstallHooks: []selfinstall.InstallHook{devices.Hook{}},
		Commands:     []cli.Command{statusCommand},
	}))
}
```

`cli.Options` 的零值就是 `cmd/gpuworks` 的社区版 server：

- `Edition` 写入 `version.Edition`，只用于显示：`gpuworks edition`、`/healthz`、构建信息、用量报告和控制台清单都读它。开源代码从不按它分支（`edition_branches` 预算为 0）。
- `Extensions.Modules` 交给 `serve`；`migrate`、`reset-password`、`support-bundle` 与追加子命令打开数据库时也带上模块的迁移（追加子命令用 `cli.Env.OpenMaintenanceStore`）。
- `Extensions.UI` 替换内嵌的社区版控制台，布局与 `internal/server/webui/dist` 相同；为空时服务内嵌构建。
- `Distribution` 是该构建自己的发布通道：版本检查、通知、目录更新与升级都读它，签名索引里的版本名不符即拒绝。它的版本名必须与 `Edition` 相同，否则启动即 panic。
- `InstallHooks` 在 `install`、`upgrade` 时依次执行，`uninstall` 时倒序执行，用于设备权限之类必须由 root 完成的主机步骤。核心自己的钩子（[主机身份](#管理-server-的主机身份)的 systemd drop-in）排在它们之前，所以安装时先执行、卸载时最后执行。钩子拿到的 `Layout.UnitDir` 只在 systemd 模式下非空，没有它时主机身份钩子什么也不做；控制台升级不执行钩子。

官方构建传零值。`Edition` 与 `Distribution` 的按版本名作用域只服务分叉构建，官方发布流程不再扩展它们（ADR 0071）。

## 模块与 Host

```go
type Module interface{ Name() string }                     // [a-z][a-z0-9_]{1,31}
type Migrator interface{ Migrations() store.MigrationSet } // optional
type Initializer interface{ Init(ctx context.Context, h *app.Host) error }
type Service interface{ Run(ctx context.Context) error }
```

模块名是它唯一的标识：`server.yaml` 里 `extensions.<名>` 一节、迁移命名空间、日志的 `module` 标签都用它。名字非法、重复，或迁移集的命名空间不是模块名，server 在打开数据库之前就拒绝启动。

`Init` 在 `app.New` 里、API 依赖构建完之后、路由构建之前按模块顺序执行，只执行一次。模块只能经 `Host` 接触 server：

| 方法 | 作用 |
|---|---|
| `Log()` / `Bus()` / `DataDir()` / `InstallID()` | 带 `module=<名>` 的日志、事件总线、数据目录、安装标识 |
| `Section(v)` | 严格解码 `server.yaml` 的 `extensions.<名>`；没有这一节时 `v` 保持原样 |
| `Tables()` | 本模块的表（`store.ExtensionTables`）；没有迁移集的模块为 nil |
| `DeriveKey(purpose)` | 由 master key 经 HKDF-SHA256 派生、与模块名绑定的 32 字节密钥 |
| `MountAPI(e)` | 把 `api.Extension` 的路由挂到 `/api/v1/x/<名>/` |
| `DeclareFeatures(...)` / `ProvideFeatures(src)` | 声明功能键；为编译模块声明的功能键提供功能来源（全部模块中至多一个；插件包声明的功能键由插件宿主作答，不经它） |
| `AddBannerSource(b)` | 控制台每页顶部的横幅 |
| `GuardNodeInsert(g)` | 新节点插入时的守卫 |
| `LimitPlatform(src)` | 整台 server 的可用性来源（见“可用性”）；全部模块中至多一个 |
| `AnswerMembers(fn)` / `AskMember(ctx, member, question)` | 控制面集群里，本模块回答其他成员的提问，以及向另一个成员上的本模块提问。走成员之间已有的通道（成员证书双向 TLS），所以答案确实来自被问的成员，别人也问不了；提问和回答各不超过 64 KiB。被问的成员不必是主节点。单机上 `AskMember` 返回 `ErrNotClusterMember`；集群成员在自己通往其他成员的通道建好之前返回 `ErrMemberRouteNotReady`（模块的 `Init` 先于通道，所以在 `Init` 里问一定得到它，表示“没问成”，不代表对方没回答）；成员不在线或其构建没有注册回答函数时返回 `ErrMemberUnreachable`。`AskMember` 在运行期间调用 |
| `HoldScheduling(h)` | 调度保持者，按来源名区分，来源重复即拒绝 |
| `HoldCardScheduling(h)` | 按卡的保持者（`store.CardSchedulingHolder`），按来源名区分，来源重复即拒绝；同一模块可以用同一个来源名同时保持节点和卡，见[按卡的保持者](#按卡的保持者) |
| `ReapplySchedulingHolds()` | 保持者的答案变了：请求对全部节点（及其卡）重新应用一次保持，不等各节点的下次状态上报 |
| `ReapplySchedulingHoldsFor(ids)` | 同上，但只对答案变了的那些节点（按节点 id）及其卡重新应用 |
| `NodeFleet(ctx)` | 全部节点的保持事实（`store.NodeHoldFacts`），按 id（加入顺序）排序，读池上一次查询 |
| `CardFleet(ctx)` | 全部卡的保持事实（`store.CardHoldFacts`），按节点 id、再按卡的身份排序，读池上一次查询 |
| `NodeIdentity(ctx, ids, salt)` | 指定节点的加盐硬件身份（`store.NodeIdentity`），原始序列号不出核心，见[节点身份与节点清单](#节点身份与节点清单) |
| `NodeInventory(ctx, cursor, limit)` | 按加入顺序分页的节点清单（`store.NodeInventoryPage`）：名称、加入时间、自己的卡（序号、型号、有无序列号） |
| `NodeNamesByID(ctx, ids)` | 指定节点的显示名，以 id 为键（`map[uint]string`），读池上一次查询 |
| `ClusterObservation(ctx, timeout)` | 本 server 所在控制面集群的一次观测（`app.ClusterObservation`）：成员、角色、谁在回应、能否再失去一个成员；与 `cluster-status` 报告同源。单机或不含集群运行时的构建回答 `Clustered: false`。只读，可在 `Init` 之后、服务请求时调用；每个探测以 `timeout` 为限并依次进行 |
| `HostIdentity(salt)` | 管理 server 所在机器的加盐身份（`hostidentity.Salted`），见[管理 server 的主机身份](#管理-server-的主机身份) |

`Host` 不交出 `*store.Store`，也不交出数据库句柄。`Init` 返回之后再注册会被拒绝（`ReapplySchedulingHolds`、`ReapplySchedulingHoldsFor`、`NodeFleet`、`CardFleet`、`NodeIdentity`、`NodeInventory` 、`NodeNamesByID` 与 `HostIdentity` 不是注册，任何时候都可调用）：有返回值的方法返回 `app.ErrHostClosed`，其他方法 panic。`Init` 出错时 `New` 失败并关闭数据库，同一数据目录可以立即再次启动。

扩展路由挂在 `/api/v1/x/<名>/` 下：`api.RouteSpec.Pattern` 相对 `/api/v1`，是 `/x/<名>` 或以 `/x/<名>/` 开头，且是规整的路径（没有 `.`、`..`、`//` 与结尾的 `/`）；越出自己的命名空间、路径不规整，或同一方法下两个模式只差参数名（`{id}` 与 `{name}`），都在路由构建时 panic。核心不在 `/api/v1/x/` 下注册任何路由，所以模块名可以与核心路径段同名（如 `nodes`），也不会与核心将来新增的路径冲突。

路由的访问级别（`api.RouteSpec.Access`）有四档：`AccessRead`（任意登录用户与平台令牌，读令牌只能读）、`AccessAdmin`（管理员，会话或平台令牌）、`AccessAdminSession`（管理员的登录会话）与 `AccessPublic`（不认证）。公开路由按客户端地址限流，规则与插件包的 `public` 路由相同（默认每分钟 60 次、突发 30，超出回 429 `TOO_MANY_REQUESTS` 与 `Retry-After`）；这条链路从不读取、也从不续期会话，处理函数拿不到调用方是谁；它不能对 AI 助手可读。`RouteSpec.SignIn` 标明一条公开路由属于登录流程（身份连接器的起始与回调，包括 SAML 回传结果的 POST），只有 `AccessPublic` 的路由能设，否则挂载时 panic；整台服务端受限时它照常放行（[ADR 0088](../adr/0088-extension-public-routes-under-a-platform-limit.md)）。

所有 `Init` 结束后，server 先释放本构建没有任何模块认领的扩展停止调度与卡的保持（换了构建或去掉了模块时，它们不会一直留着），有保持者（节点或卡）时再对全部节点及其卡应用一次保持，然后构建功能门（`featuregate.New`）并交给 API。

## 保持者的重新应用与节点集合

保持者的答案平时在每次状态上报时被问到，所以答案变了的节点最多要等一分钟才被停止或恢复调度。答案变了（例如快照重建后被保持的集合不同了）的模块调用 `ReapplySchedulingHolds()`，server 对全部节点再执行一次 `ApplySchedulingHolds`：

- 不阻塞、不分配内存，可以在任何 goroutine 调用，**唯独不能在钩子里**（`SchedulingHolder.Hold`、`CardSchedulingHolder.HoldCard`、插入守卫）：一次重新应用会再问一遍所有保持者，在 `Hold`（或 `HoldCard`）里请求就会让全部节点不停地重新应用。钩子里同样不请求任何其他重算。
- 请求会合并：一次重新应用等待或执行期间的任意多次请求，最多再引起一次；两次之间至少隔 2 秒，所以答案来回抖动的保持者每 2 秒最多花一次全表写事务，不会变成每个事件一次。
- 知道答案对哪些节点变了的模块调用 `ReapplySchedulingHoldsFor(ids)`：这次重新应用只读写这些节点（`ApplySchedulingHoldsFor`，`WHERE id IN`），频繁改动少数节点的保持者因此不会每次都花一遍全表事务。它与其他请求一样合并；两次重新应用之间点名的节点超过 `holdpass.MaxScoped`（512）个时改为全部节点，有人请求全部节点时也是全部节点。推送状态（插件推送的节点保持表）走这条路，只有容量上限变了才请求全部节点。
- 重新应用在模块的 `Service` 所在的同一个 `services()` 循环里执行，不占调用方的 goroutine，随 server 停止；失败时记错误日志，从 2 秒起加倍退避（最长一分钟）重试。没有注册保持者时什么也不做。

答案取决于整个节点集合的保持者（例如按加入顺序挑选节点），用下面两样了解集合，而不是在 `Hold` 里记下见过的节点：

- `NodeFleet(ctx)`：全部节点的保持事实，读池上一次查询。只用于启动时和节点集合变化之后合并执行的重算，不在每次上报或每个事件里调用，也不在钩子里调用。先订阅下面的主题再第一次读取，二者之间的变化就不会漏掉。
- 总线主题 `store.NodeFleetTopic`：节点插入（三条插入路径）、删除，以及改变了保持事实（卡数或厂商；id、UUID、加入时间与所属 Kubernetes 集群不会变；或某张卡的保持事实：卡的增减，卡的序号、厂商或归属变化）的状态上报，各发一个事件，以节点 id 为键；用 `store.NodeFleetChangeOf(ev)` 读出 `{NodeID, Kind}`（`NodeInserted`、`NodeDeleted`、`NodeFactsChanged`），不要解析 `node` 主题的原因文本。没有改变保持事实的例行上报不发事件，所以发布方的开销不变。和所有总线主题一样，跟不上的订阅者只收到每个节点最新的一个事件，所以订阅者把任何事件都当作「这个节点、因而集合变了」，合并后从 `NodeFleet` 重算，不数事件。节点 id 删除后不再复用。

## 按卡的保持者

`store.CardSchedulingHolder`（`Source()`、`HoldCard(store.CardHoldFacts) store.CardHold`）是按单张卡的保持者，规则与 `SchedulingHolder` 相同，只是对象是卡：

- 事实 `CardHoldFacts`：卡所在节点的 id、UUID、加入时间，卡的身份 `Ident`（存卡的键，`accelerator.CardIdent`；探测给不出身份时为 `index:<n>`）、序号 `Index`（厂商 CLI 不列出时为 -1，卡组变化时会重新编号，只用于显示）、厂商 `Vendor` 与归属 `Ownership`（空表示本节点可用）。
- 答复 `CardHold{Code, Fallback, Params}`：零值表示不保持；保持时以产品消息展示给运维，没有给 code 时为 `CARD_SCHEDULING_HELD`。多个保持者时先注册、先保持的说了算。
- 被保持的卡 `GPU.CanTakeWork` 为假：调度器不在它上面放任何新工作（整卡、按显存共用、与平台外进程共用都不行，手工选卡也不行），拒绝原因是 `gpus_held`（参数 `gpus`、`count`、`node`）。节点本身不停止调度，卡上已运行的实例照常服务、从不驱逐。
- 保持的效果落在卡行上：`gpus.scheduling_held_by` 记保持者的来源名（不保持时为空），按 SQL 聚合卡数的读者（`OnlineGPUGroups`）从这一列读；保持本身不落库。每次写卡的状态上报（探测成功或只读到 PCI 时）在同一写事务里对每张卡再问一遍，`ApplySchedulingHolds` / `ApplySchedulingHoldsFor`（也就是 `ReapplySchedulingHolds`、`ReapplySchedulingHoldsFor` 请求的那一次）在同一个事务里先过卡、再过节点；卡的保持变了的节点作为调度变更发布，但不计入返回的节点列表。
- 运维：保持高于管理员的启用、低于管理员的禁用。保持期间 `SetCardScheduling` 设为可调度返回 `store.ErrCardHeld`（包着保持者的 `*messages.ProductError`，API 为 409，带保持者的码），设为停止照常生效；卡的 `scheduling_actions` 不给 `schedulable`。保持结束后卡回到管理员的决定或平台的判定。
- 卡的视图（`GET /nodes/{uuid}` 与 `GET /gpus`）带 `scheduling_held_by` 与 `scheduling_hold`（保持者现在给出的消息；行上还记着、保持者已不再保持而下一次应用还没到时，为通用的 `CARD_SCHEDULING_HELD`）。
- 答案取决于全部卡的保持者从 `CardFleet(ctx)` 读全部卡的保持事实，在 `store.NodeFleetTopic` 的事件之后合并重算（卡的增减与保持事实的变化都发 `NodeFactsChanged`），不在 `HoldCard` 里记下见过的卡。
- `HoldCard` 在每次上报里对每张卡各调用一次，纯函数规则（见下）同样适用：不保持时零分配，保持时返回预先构造好的答复。`store.Store.CardHoldFor(facts)` 就是写事务里问的那一次，不读任何东西，契约测试用它证明零分配。

`Service.Run` 由 `App.Run` 的同一个 `services()` 循环启动，与健康检查、卡故障记录、用量报告、通知刷新并列，在控制器启动之后运行；`ctx` 结束时返回，返回的错误只记日志、不重启。同一个 App 可以多次 `Run`（原地重启），所以 `Run` 必须能被再次调用。

## 节点身份与节点清单

需要认出「同一台机器、同一批卡」的模块（如节点清单导出、支持包）经 `Host` 读加盐摘要，而不是原始序列号：

- **主机系统 UUID**：agent 在每次握手的 hello 里上报 `host_system_uuid`，即固件的系统 UUID（Linux 读 `/sys/class/dmi/id/product_uuid`，macOS 读 `IOPlatformUUID`），经 `proto.NormalizeHostSystemUUID` 规整（小写；占位值、全 0、全 F 与格式不对的值丢弃）。Linux 上该文件只有 root 能读，所以非 root 的 agent 不上报；旧 agent 也不上报，照常接入。server 收到后再规整一次，只在与节点行不同时写入 `nodes.host_system_uuid`，例行重连不多一次写；空值从不清掉已记下的值。它描述机器而不是节点：同一台机器上的多个逻辑节点上报同一个值。
- **卡的序列号**：卡的标识就是探测上报的 `GPUInfo.UUID`。`accelerator.CardSerial` 区分硬件自带、换槽换机也不变的序列号与探测按槽位或序号编出来的标识，只有前者参与身份：

  | 厂商 | 上报的标识 | 稳定 |
  |---|---|---|
  | NVIDIA、PPU、昆仑芯 | 厂商 CLI 给出的卡 UUID | 是 |
  | AMD | `amd-<unique_id>`（ASIC 序列号） | 是；驱动没有 `unique_id` 时为 `amd-<PCI 地址>`，否 |
  | 海光 | `GPU-<unique id>`（hy-smi 或 sysfs） | 是；没有时为 `hygon-<PCI 地址>` 或 `hygon-<序号>`，否 |
  | 昇腾 | `ascend-<序号>` | 否（npu-smi 的表格不给序列号） |
  | Apple | `APPLE-<IOPlatformUUID>` | 是，与主机系统 UUID 是同一台机器 |
  | 假探测器 | `GPU-FAKE-<n>`、`FAKE-<厂商>-<n>` | 否（测试值，每个节点都一样） |

- `NodeIdentity(ctx, ids, salt)` 对每个仍存在的指定节点给出 `{NodeID, Host, Cards, CardsWithoutSerial}`，按 id 升序：
  - `Host` 与 `Cards` 的每一项是 `store.IdentityPart{H, Tail}`。`H` 是以调用方的盐为密钥、对「种类、一个零字节、原始值」做 HMAC-SHA256 的前 16 个十六进制字符（8 字节），种类是 `host` 或 `card`；`Tail` 是原始值的最后 4 个字符，供人到机器上核对，原始值不足 8 个字符时为空，所以尾号永远不等于整个值。同一个盐每次得到相同的摘要，不同的盐得到互不相关的 `H`；`Tail` 不加盐，每个调用方看到的都是同一段原始尾号。
  - `Cards` 只含本节点可用（`accelerator.CardUsable`）且带序列号的卡，按 `H` 排序；可用但标识是编出来的卡只计入 `CardsWithoutSerial`。所以同一台机器上的两个逻辑节点共用 `Host`，各自只有自己的卡。
  - 部分答复：agent 还没上报主机 UUID 时 `Host` 是零值（`Known()` 为假）；不存在的节点（从未有过或已删除）不出现在结果里，调用方比对 id；重复的 id 只算一次。
  - `ids` 为 1 到 500 个节点，否则返回 `store.ErrNodeReadBatch`；盐为 1 到 256 字节，否则返回 `store.ErrIdentitySalt`。越界的调用被拒绝，不会被截断。
- `NodeInventory(ctx, cursor, limit)` 按 id（加入顺序）返回一页 `store.NodeInventoryPage{Nodes, Next}`：每个节点的 id、UUID、显示名（运维起的名字，否则主机名）、加入时间、`HostReported`（主机 UUID 是否已知），以及本节点可用的卡 `{Index, Model, Serial}`，不含任何原始序列号。第一页 `cursor` 为 0，之后传上一页的 `Next`；`Next` 为 0 表示已是最后一页。`limit` 为 1 到 500，否则 `store.ErrNodeReadBatch`。节点 id 不复用，所以翻页之间加入或删除的节点不会让其余节点移位或重复。
- `NodeNamesByID(ctx, ids)` 返回每个仍存在的指定节点的显示名（运维起的名字，否则主机名），以 id 为键；不存在的节点（从未有过或已删除）不在结果里，重复的 id 只算一次。只为少数节点打标签时用它，不必翻 `NodeInventory`。`ids` 为空时返回空 map、不查询；去重后超过 500 个返回 `store.ErrNodeReadBatch`，不会被截断。它是读池上一次按主键的查询。
- 规模：`NodeIdentity` 与 `NodeInventory`都是钉住的读，在读池上各两次带索引的查询（节点按主键查找或按主键范围、卡走 `gpus` 的 `(node_id, ident)` 索引），一次最多 500 个节点。按页面上显示的节点调用 `NodeIdentity`，不要在每个事件里对整个节点集合调用。身份的变化（新的主机 UUID、换卡）不在 `store.NodeFleetTopic` 上发布；需要跟上它的模块在读页面时重新读。
- 插件：已安装的插件经宿主接口读同样的两项（`POST /host/v1/node-identity`、`GET /host/v1/nodes`，线上格式见[插件协议](../reference/plugin-protocol.md#节点读取)），需要 `read:node_identity` / `read:node_inventory` 权限与 `host.node_identity@1` / `host.nodes@1` 能力。插件的盐由 `pluginhost.IdentityKey` 绑定到包 id（`"pkg\0"+id+"\0"+盐` 作为 HMAC 密钥，盐至多 219 字节），所以两个包用同一个盐得到不同的 `H`，同一个包在不同的 server 上得到相同的 `H`；不加盐的 `Tail` 对所有调用方相同，仍允许跨包的弱关联。`app.PluginReads` 把 store 的结果转成线上对象，名称与型号超过 255 字节时按字符边界截断。

### 管理 server 的主机身份

`HostIdentity(salt)` 返回 server 自己这台机器的加盐身份 `hostidentity.Salted{SystemUUID, BoardSerial, MachineID, RootFSUUID}`，每一项是 `hwident.IdentityPart`（与节点身份同一种摘要与尾部，读不到时为零值）。系统 UUID 按节点 `Host` 的同一种类（`hwident.KindHost`）做摘要，所以同一个盐下它等于运行在 server 这台机器上的节点的 `Host.H`。盐为 1 到 256 字节，否则 `store.ErrIdentitySalt`。事实每个进程只读一次（`hostidentity.Local`）。

- 来源（`internal/server/hostidentity`）：Linux 上读 `/sys/class/dmi/id/product_uuid`、`board_serial`、`/etc/machine-id`，并按 `/proc/self/mountinfo` 与 `/dev/disk/by-uuid` 找出 `/` 的文件系统 UUID；macOS 上用 `ioreg` 读 `IOPlatformUUID` 与整机序列号。空值、固件占位值、全 0 或全 F 视为未知。
- 内核只让 root 读系统 UUID 与主板序列号，而 server 以服务账户运行：核心安装钩子在 systemd 模式下（以 root 运行的命令行 `install`、`upgrade`；控制台升级不执行安装钩子，独立运行模式下钩子拿到的 `Layout.UnitDir` 为空）写 systemd drop-in，让 systemd 每次启动前以 root 执行 [`gpuworks host-identity`](../reference/cli.md#gpuworks-host-identity)，把这两项写进 `/run/gpuworks/host-identity.json`（`root:<服务组>`，0440，所在目录 root 所有、0755）。`hostidentity.Load` 只用这个文件补自己读不到的这两项，直接读到的值永远优先，`MachineID` 与 `RootFSUUID` 只来自直接读取；文件缺失、超过 4 KiB 或不合格式时什么也不补。`/run` 是内存文件系统，把数据目录拷到另一台机器不会带走原来那台的身份。
- root 启动前步骤只执行单元目录中受保护的 `host-identity-exec`，不执行服务账户可写的主程序。命令行安装/升级从正在运行的安装器映像发布副本并检查整个路径的所有者、权限和 ACL；控制台升级保留既有副本。副本写出的身份文件格式必须与后续 server 版本保持兼容。
- 插件经宿主接口 `POST /host/v1/host-identity` 读同样的事实（线上格式见[插件协议](../reference/plugin-protocol.md#主机身份)），需要 `read:host_identity` 权限与 `host.host_identity@1` 能力；盐与节点读取一样由 `pluginhost.IdentityKey` 绑定到包 id。
- 诊断信息收集（控制台与 `gpuworks support-bundle`）带上 `server/host-identity.json`：用固定的密钥 `core\0support_bundle` 加盐的同一组事实，所以同一台机器在不同的收集里摘要相同，原始值不进压缩包。

## 表与迁移

扩展迁移只服务编译进来的模块（分叉与测试）；插件包将用自己数据目录里的存储，核心为插件持久化的表是核心表，都不用这套机制（ADR 0071）。

迁移集（`store.MigrationSet`）在核心迁移之后、同一次 `store.OpenWith` 里执行：

- 每步与它的标记在同一事务里写入 `namespace_migrations`；该表只在传入迁移集时创建，社区版打开从不创建它，也不写 schema。
- 一步建立、修改或删除的表、索引、视图、触发器，名字和所属表都必须带该集的前缀；核心表从不使用保留前缀 `ee_`。
- 数据库里有本构建不认识的命名空间时只告警，不阻止启动；已知命名空间的版本比二进制新时拒绝启动；已记录的一步在二进制里缺失或改了名，同样拒绝。
- 核心的全表清理可能触及扩展表，所以扩展表一律存 UTC 时间，迁移只做加法。

`ExtensionTables.Write` 在唯一的写连接上开一个事务，`Read` 在读连接池上开一个只读事务（内存数据库没有读池，在写连接上读）。扩展只读写自己前缀的表，核心数据只经 Store 的方法、API 与下面的钩子。

## 钩子与调用线程

| 钩子 | 在哪里被调用 | 频率 |
|---|---|---|
| `NodeEnrollmentGuard` | 三条节点插入路径（`CreateNodePending`、`EnrollKubeNode`、`RegisterNode`）的写事务内 | 每次插入新节点 |
| `SchedulingHolder.Hold` | 每次状态上报与调度变更的写事务内；启动时与每次 `ApplySchedulingHolds`（含模块请求的重新应用）时对全部节点，`ApplySchedulingHoldsFor` 时对点名的节点 | 每节点每次上报 |
| `CardSchedulingHolder.HoldCard` | 每次写卡的状态上报的写事务内，对每张卡；启动时与每次 `ApplySchedulingHolds` 时对全部卡，`ApplySchedulingHoldsFor` 时对点名节点的卡 | 每张卡每次上报 |
| `featuregate.Source.Availability` | 扩展路由的每个请求、每次控制台清单请求 | 每请求 |
| `BannerSource.Banners` | 每次控制台清单请求 | 每请求 |
| `Service.Run` | 后台循环 | 常驻 |

插入守卫拒绝时，API 以 403 返回守卫的码；租机与 provider 安装路径会先做一次不占写锁的预检查，不会先租机再拒绝。保持者保持的节点停止调度、从不驱逐，运维不能恢复调度（409，带保持者的码），但仍可停止调度或驱逐；按卡的保持者保持的卡同样不接新工作、从不驱逐，运维不能把它设为可调度（409，带保持者的码），但仍可停用它。

## 纯函数规则

写连接只有一个（`store/database_open.go` 里 `SetMaxOpenConns(1)`）。在写事务里被调用的钩子（插入守卫、调度保持者、按卡的保持者）如果调用 Store、`Tables()` 或任何等待它们的东西，就会等自己持有的连接，形成死锁。所以这些钩子必须：

- 不调用 Store、`Tables()`，不做任何 I/O，不取有竞争的锁；
- O(1)：一次原子读加一次 map 查找；不保持时不分配内存；
- 从模块自己维护的快照作答，快照在别处（`Service`、事件订阅、API 写入）更新，快照变了再调用 `ReapplySchedulingHolds`；
- 拒绝时返回预先构造好的 `*messages.ProductError`。

功能来源与横幅来源在请求线程上被调用，同样从快照作答，不扫节点、部署或表。`testenv/extmodule` 里的契约测试演示了违反规则的后果：一个在守卫里写自己表的守卫，在插入事务结束前无法继续。

## 可用性

开源代码只说可用性：功能来源对每个功能键给出完整、只读、锁定或隐藏，可附限额、链接与带码的说明。

- 扩展路由（`api.RouteSpec`）要么指明 `Feature`，要么标记 `Community`，二者必居其一；路由清单（`api.RouteInventory`）打印每条扩展路由的访问级别、动作与功能，登录路由在行末加 `[sign-in]`。
- 完整：放行。只读：放行读，以及标记 `WriteWhenReadOnly` 的写（只做删除或放宽的写）；其他写返回 403 `FEATURE_READ_ONLY`。锁定：403 `FEATURE_UNAVAILABLE`。隐藏，或声明为隐藏呈现的功能被锁定：404。功能来源给了自己的说明时，返回它的码。
- 没有模块提供功能来源时，所有声明过的功能都是锁定（`FEATURE_PROVIDER_ABSENT`）。
- 社区路径从不查询功能门；正在运行的负载从不因可用性变化而中断。核心代码不以字面量点名功能键：交给功能门的键形字面量、`featuregate.Key("…")` 转换、赋给 `featuregate.Key` 常量的字面量、功能声明的 `Key: "…"`（`featuregate.Feature{Key: …}`）与路由的 `Feature: "…"`，在非测试代码里由架构预算 `feature_key_literals_in_core`（上限 0）守护。
- 扩展 GET 路由默认不给 AI 助手与 MCP 读，除非标记 `AIReadable`。

### 整台 server 的可用性

一个模块可以用 `Host.LimitPlatform` 给整台 server 定可用性（`api.PlatformLimiter`）：完整、只读或锁定。没有模块这样做时，server 从不受限；社区版没有这个来源。

- 只读：读请求和数据面（`/v1/*` 与控制台调试台的 `/api/v1/playground/v1/*`）照常，改动一律返回 403 `PLATFORM_READ_ONLY`。锁定：读请求和恢复面照常，改动和数据面返回 403 `PLATFORM_LOCKED`；数据面的拒绝用网关的错误形状（`{"error":{"message","type","code"}}`），码相同。来源给了自己的说明时，返回它的码。来源答“隐藏”按锁定处理。
- 恢复面在任何限制下都放行：登录与会话、健康检查、控制台清单与品牌、已接入机器的连接与健康检查脚本、Kubernetes 与 Slurm 的接入接口（已接入集群里重建的节点凭它取回原来的身份；新节点是一次节点插入，由设限模块的插入守卫拒绝）、已安装插件包的管理（停用、重启、回滚、卸载；安装和升级插件包会带进新代码，按改动拒绝）、server 自身的升级、诊断包，以及注册该来源的模块自己的路由（这样它的页面才能解除限制）。
- 限制在任何处理函数之前生效，按路由的模式分类而不是按路径猜。`/api/v1` 下的请求先鉴权再拒绝：未登录的调用方得到的是不受限时的 401，看不出有哪些路由、server 是否受限；数据面和发放接入材料的地址用的是守卫无法核对的凭据，对任何人都拒绝。扩展里不需要登录的路由（编译模块的 `AccessPublic`、插件包的 `public`）同样按方法分类：读照常，改动在只读和锁定时都拒绝，而且直接回 403 与限制的码，不先要求认证（公开路由本来就对所有人公开）；只有编译模块声明了 `SignIn` 的路由和登录一样在任何限制下放行，不论方法。插件包还不能声明登录路由，它要升清单 schema，随 ADR 0085 第 3 步实施（[ADR 0088](../adr/0088-extension-public-routes-under-a-platform-limit.md)）。`/api/v1` 下的路由默认按方法分为读和改动；不存任何业务状态、也不改动节点的 POST（助手问答、模型与部署的预览、未保存的云凭据与助手模型的测试、压测目标探测、待接入机器的 SSH 预检查、通知已读，共十一条）是读，逐个点名；会让节点开监听或挂载的检查（部署前的网络预检、存储探测）按改动拒绝；发放接入材料或打开交互会话的 GET 归为改动，在任何限制下都被拒绝；不改动任何东西、但要到 server 之外取数的 GET（节点文件与镜像列表、实例日志流、存储与批处理文件下载、云账号的实例与集群列表、镜像仓库标签）是读，照常放行，并在 `platform_limit.go` 里逐个点名；`/api/v1` 之外的新路由必须在 `internal/server/api/platform_limit.go` 里点名，否则在任何限制下都被拒绝。受限时放行的每一条路由都列在 `platform_limit_routes_test.go` 里：`/api/v1` 下新增的 GET 默认是读，测试会因为它不在清单里而失败，加入清单之前要确认它不改动、不启动任何东西，也不发放接入材料。
- 来源在每个请求上被调用，遵守纯函数规则：一次原子读，不做 I/O，不加锁，不分配。
- 限制只作用于新的请求：不停止、不驱逐、不重启正在运行的负载，已接入的机器保持连接。

控制台清单在受限时带 `platform`（`access`、`link`、`message`）。锁定和只读时页面都照常显示（两者都放行读请求），页面上方的提示说明什么被拒绝，并带一个打开 `link` 的按钮；`link` 指向的页面自己说明情况，不再显示提示。

控制台按 `GET /api/v1/console/manifest` 渲染（字段见 [API 参考](../reference/api.md)），自己不推导可用性：完整照常显示；只读时组件收到 `readOnly`；锁定时菜单项带锁，页面显示锁定页和服务端给的链接；隐藏或清单里没有的功能键，什么都不显示。

## 控制台叠加

构建时叠加只服务分叉构建；插件包的界面将在运行时加载（ADR 0071，尚未实现）。

单独构建的 server 的界面模块复制到 `ui/src/extensions/<id>/`（已被 git 忽略），在 `ui/src/extensions/index.ts` 注册，只能 import `ui/src/extensions/sdk.ts` 再导出的组件、hook 与 API 客户端（`ee_ui_internal_imports` 预算为 0）。构建时设置 `GPUWORKS_UI_OUT_DIR` 输出到别处；检出了界面模块却没设这个变量时，`make ui` 与 `make release-packages` 拒绝构建，以免叠加进社区版。界面模块的文案放在自己的 locale 包里合并进 `common`，用 `GPUWORKS_I18N_EXTRA_LOCALES` / `GPUWORKS_I18N_EXTRA_MESSAGES` 跑同一套 i18n 检查。

界面模块能放的入口：

- 页面（`routes`）、侧边菜单项（`navItems`，必须带图标）、设置页签（`settingsTabs`）、顶栏控件（`headerItems`）。
- 账户菜单项：`navItems` 的 `group` 写保留值 `account`，入口进右上角账户菜单，排在内置项之后、退出登录之前，按扩展 id 排序，只显示文字。账户菜单没有放锁定入口的位置，锁定与隐藏一样不显示。`account` 不能再用作新菜单分组的 id。
- 仅管理员：以上每种入口都可标 `admin: true`，按会话检查时读到的用户角色显示。非管理员看不到菜单项、页签与控件，打开页面得到不存在页；读不到用户时同样按非管理员处理。这只影响显示，接口仍由服务端路由的访问级别把关。

横幅：链接指向当前页面（不计查询与片段）的横幅，在那个页面上不显示，由页面自己说明。

横幅来源可以给一条横幅带 `AckKey`，让看到它的人确认它：控制台在这条横幅上显示「已知晓」，用过之后不再向这个用户显示带同一个键的横幅。哪些横幅可以确认、键是什么都由来源决定；同一件事需要再次提醒时（进入新的阶段、换了截止时间），来源给出新的键，横幅就重新出现。键须符合 `api.BannerAckKeyPattern`：1 到 128 个字符，只含字母、数字与 `._:-`，以字母或数字开头；键里的时间写成 UTC、以 `Z` 结尾（如 `2027-01-01T00:00:00Z`），带时区偏移的 `+` 不在字符集里。不符合时清单不报错，直接去掉它，横幅照常显示但不能确认。确认只记在浏览器里（按用户分开），server 不存储，所以换一个浏览器或清除站点数据后会再显示一次。控制台在知道登录的是谁之前（刚登录、`/auth/me` 还没回来）不显示带键的横幅，也不记录确认。叠加层页面经 SDK 的 `useBannerAck` 读写同一份记录（`ready` 为 false 时同样不提供确认），在页面上确认同一件事，页顶的横幅也随之消失。插件推送的横幅（`table.banners@1`）不带确认键。

SDK 带主版本号 `SDK_MAJOR`（当前为 1），只在删掉或改变某个导出的含义时加一；新增导出不改版本，`ui/tests/consoleExtensionsSdk.test.ts` 固定导出清单。文件收发用 SDK 的 `downloadFile`、`saveBlob` 与 `sendForm`：

- `downloadFile` 带会话请求，文件名取响应的 `Content-Disposition`，没有时用调用方给的名字。错误响应按码解成本地化的 `APIError`，不会把错误正文存成文件。
- `saveBlob` 把一个 Blob 交给浏览器下载。
- `sendForm` 发 multipart 表单，错误解码与 `sendJSON` 相同。

界面模块的测试复制到 `ui/tests/ext-<id>/`（已被 git 忽略），用 `UI_EXTRA_TESTS='tests/ext-<id>/*.test.tsx' npm test` 跑。测试不在 `ui/src/extensions/<id>/` 里，所以 import `node:test`、testing-library 不计入 import 预算；`npm run test:coverage` 的 `c8 --all --include='src/**'` 仍统计界面模块的全部源文件。

## 码与文案

扩展产生的失败与拒绝同样遵守 [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md)：UPPER_SNAKE 码、插值参数、英文 fallback。扩展的码与 en/zh 文案放在扩展自己的代码与界面模块里，不进开源的 `messages` 与 locale 文件；没有叠加界面时，控制台显示英文 fallback。

## 升级与安装

以下按版本名的检查只对分叉构建有意义：官方构建不写版本名，ADR 0071 决定第一个可用性权威作为插件运行之后从官方发布流程删除这套机制。

- 发布索引在签名内容里写明版本名；索引的版本名与构建的发布通道不符即拒绝，社区版只接受不写版本名或写 `community` 的索引。
- `upgrade` 比较已安装二进制与待安装二进制的 `gpuworks edition`，不同则拒绝，除非带 `--allow-edition-change`；控制台升级遇到版本不同返回 409 `SERVER_EDITION_MISMATCH`。
- `upgrade` 执行的是当前二进制的安装钩子；跨版本升级后按提示用新二进制再执行一次 `install`。
- 从扩展构建回到社区版时，`server.yaml` 里的 `extensions:` 节只产生告警；扩展的表原样保留；扩展停止调度在启动时释放。

## 可安装扩展

ADR 0071 决定官方与第三方扩展以签名插件包（`.gwpkg`）安装，而不是编进二进制。插件宿主已经作为内置模块接入本页的契约：

- 插件宿主是内置模块 `plugins`（`app/plugins.go`，实现在 `internal/server/pluginbridge`），经 `Host` 注册已有的钩子：节点插入守卫、调度保持者（来源 `plugins`）与横幅。已安装的包声明的功能键进功能门的动态集合，由插件推送、核心校验并持久化的表作答；编译模块声明的功能键仍由 `ProvideFeatures` 的来源作答，两者并存。编译模块不能再叫 `plugins`。
- 运行时功能声明：安装、升级、启用、停用或卸载一个包之后，动态集合整体替换为当时已安装的包声明的功能键，功能门与控制台清单立即看到，并在 `feature` 主题上通知控制台重读清单；不用重启。功能门查找先查编译模块声明的键，再查动态集合（一次原子读取，不加锁、不分配）；`GET /api/v1/console/manifest` 的 `features` 是两者合并后按键排序的列表。什么都没装时动态集合为空，清单与原来逐字节相同。
- 一个功能键只有一个归属：包声明的键已由编译模块声明，或已由另一个已安装的包声明（同一个包升级时保留自己的键）时，安装在要求批准之前就以 409 `PLUGIN_FEATURE_TAKEN{id, feature, owner}` 拒绝，`owner` 是模块名或那个包的 id，什么都不装。在这项检查之前装上的冲突包启动时照常加载：键留给原来的归属（编译模块，或 id 较小、自两者都安装起就作答的包），每处冲突记一条日志，归属不变；这样的包在下一个版本去掉冲突的键之前不能重装或升级。已安装但不再通过校验的包不占用它声明的键：它坏着的时候，别的包可以装上这个键；它恢复校验后，键归 id 较小的一方。
- 插件是 server 的子进程，经继承的 fd 用 `gwx/1` 与核心通信，从不进入热路径，也不拿 Store。
- 功能键是否受控由插件包自己声明；授权类的表只接受发布者证书指定的权威推送；插件掉线时限制继续执行，授权在有界宽限后降为只读，时间线按 extclock 照常生效。
- 本页的钩子规则（写事务里的钩子是纯函数、保持者只停止调度不驱逐、运行中的负载不中断）对插件宿主同样适用。

包清单、证书、信任更新、`gwx/1` 消息与推送表的线上格式冻结在 `pkg/extapi` 里，见[插件协议与线上格式](../reference/plugin-protocol.md)；包的读取、校验、信任与 `gpuworks plugin` 子命令见 [CLI 参考](../reference/cli.md#gpuworks-plugin)与[安装布局](../reference/install-layout.md)。

### 插件宿主模块

启动顺序（`plugins` 模块的 `Init`，在路由建好之前）：

1. 清掉中断的安装留下的目录，逐个重新校验已安装的包（这次全部重新哈希）；不再通过校验或已被吊销的包记为已安装但未校验：它推送的限制与横幅照常执行，授权被丢弃，它不会启动。
2. 用这份包清单读入全部推送表（见下文「推送状态」），在 server 接受第一个请求之前、任何插件启动之前生效。
3. 把已安装的包声明的功能键放进动态集合（与编译模块的键冲突时记日志并留给模块），注册钩子。

模块的服务（随 server 的其他后台循环运行）先启动推送状态的写入 tick、时间线与容量选择，再并行启动每个已启用、有 `resident` 服务端部分的包，不阻塞 `serve`；已启用的 `on_demand` 包交给监管、处于 `idle`，第一次调用时才启动。启动前只重新哈希变过的文件。`resident` 插件的进程状态经监管回调推送状态：`running`、`degraded` 算在线，其余都算掉线；`on_demand` 插件不推送状态，它的状态不报给推送状态，空闲停下再启动的循环也就不写掉线记录。server 停止时先停插件，再停推送状态，所以每个插件掉线的时刻都被持久化，重启不会续期宽限。

推送方身份来自启动它时校验通过的包：监管为每个进程编号（全局递增的代数），宿主接口的推送按这条连接所属的包认定，带证书授予（含 `server.yaml` 为主机信任的签发者配置的权威）、管理员批准的权限、清单的码和这个进程的代数；连接所属的包已被替换或换了版本时推送被拒（`PLUGIN_STATE_CONNECTION_REPLACED`）。核心提供首批五种表的能力 `table.<种类>@1`，包的 `requires` 据此校验，插件握手时也据此协商。

包声明了 `storage` 时，它的数据带一个版本标记 `plugin.<id>.data`（在核心的 `plugin_markers` 表里），防止回退到读不了这份数据的旧构建：

- **写入**：每次启动后插件第一次进入 `running`（握手通过）时，宿主把标记写成 `max(已存的版本, storage.state_version)`，并记下运行的插件构建版本（被拒的旧构建据此指明要装的版本）；同一次启动里之后再进入 `running` 不再读写。标记从不调低：仍在运行的旧构建不会把它改小。没有 `storage` 的包不写标记，已经卸载的包也不再写。写入失败，或插件目录正被安装、卸载占用时，只记日志，下次健康检查时重试；读不到标记时不启动，稍后自动重试启动。
- **检查**：标记比包的 `storage.state_version`（没有 `storage` 时为 0）新时，安装这个构建以 `PLUGIN_STATE_TOO_NEW` 拒绝，什么都不装；已安装的构建（例如更新的构建运行过之后又装回旧构建）每次启动前也检查一次，不启动，列表里显示 `failed` 并带 `PLUGIN_STATE_TOO_NEW`，直到装上 `state_version` 足够新的构建。降级之前可以用 `gpuworks plugin compat` 做同样的检查。
- **生命周期**：普通卸载保留数据，也保留标记，所以重装旧构建同样被拒；只有 `purge` 在把数据目录移进 `.trash/` 的同时删掉标记。

### 管理接口

`/api/v1/plugins` 是核心路由，不受功能门控，插件全部掉线、崩溃循环或被卸载时照常可用；助手与 MCP 读不到（不在助手可读的路由里，处理函数也拒绝助手的进程内读取）：

| 方法与路径 | 谁可以调用 | 作用 |
|---|---|---|
| `GET /api/v1/plugins` | 管理员（会话或令牌） | 已安装的包与各自的状态、原因、可做的动作（服务端计算），以及诊断检查 `plugins`（见下文「健康：横幅、诊断检查与指标」） |
| `GET /api/v1/metrics/plugins` | 管理员（会话或令牌） | 插件指标，Prometheus 文本格式 |
| `POST /api/v1/plugins/install?approve=<批准值>&enable=true` | 管理员登录会话 | 请求体就是 `.gwpkg`；不带或带错批准值时回 409 `PLUGIN_APPROVAL_REQUIRED`，参数里给出批准值和它涵盖的权限、角色，什么都不装 |
| `POST /api/v1/plugins/{id}/enable`、`/disable` | 管理员登录会话 | 启用并启动；停止并不再启动（推送的表照常执行，按掉线计） |
| `POST /api/v1/plugins/{id}/restart` | 管理员登录会话 | 停止并作为新实例重新启动，崩溃历史与退避从头算；`on_demand` 包回到 `idle`；已停用的包回 409 `PLUGIN_DISABLED` |
| `POST /api/v1/plugins/{id}/upgrade?approve=<批准值>` | 管理员登录会话 | 请求体是新版本的 `.gwpkg`；快照数据、切换版本并等待健康门，新版本没通过就自动回滚（见下文）；不带或带错批准值时回 409 `PLUGIN_APPROVAL_REQUIRED`，参数里带上与已安装版本的差异，什么都不变；包的 id 不是路径里的 id 时回 422 `PLUGIN_UPGRADE_ID_MISMATCH{id, package_id}` |
| `POST /api/v1/plugins/{id}/rollback?restore_state=true` | 管理员登录会话 | 回到上一版本（列表的 `previous`）；`restore_state` 同时从该版本的快照恢复数据与数据标记；没有上一版本回 409 `PLUGIN_NO_PREVIOUS_VERSION` |
| `GET /api/v1/plugins/{id}/impact` | 管理员（会话或令牌） | 卸载的影响预览：要删的表、释放保持的节点、变为 `locked` 的功能、取消或放宽的上限、横幅与覆盖，以及确认用的摘要 `confirm` |
| `DELETE /api/v1/plugins/{id}?confirm=<摘要>&purge=true` | 管理员登录会话 | 停止、卸载（`purge` 连数据与快照一起移到 `.trash/`，并删掉数据标记），在一次提交里删掉它推送的全部内容并重新应用保持；`confirm` 不是此刻影响的摘要时回 409 `PLUGIN_UNINSTALL_CONFIRM_REQUIRED`，应答带上影响，什么都不删；重试能完成一半失败的卸载 |
| `GET /api/v1/plugins/{id}/state` | 管理员（会话或令牌） | 推送状态里它的部分：各表的代数、条目数、推送时刻与下一步，掉线时刻与宽限，覆盖 |
| `GET /api/v1/plugins/{id}/logs` | 管理员登录会话 | SSE：监管事件（`state` 帧，最近 256 条）与 stderr（`stderr` 帧，最近 1 MiB），先发保留的，再跟随新的 |
| `GET /api/v1/plugins/catalog` | 管理员（会话或令牌） | 已导入的包索引与已安装的包：每个包本 server 能装的最新构建、是否可装，以及不装的更新构建与原因 |
| `GET /api/v1/plugins/overrides` | 管理员（会话或令牌） | 生效中的覆盖，按 `id` 排序 |
| `POST /api/v1/plugins/overrides` | 管理员登录会话 | 放松插件推送的一条限制：删掉一个节点保持，或删掉、调高一项集群容量上限；必须写原因 |
| `DELETE /api/v1/plugins/overrides/{oid}` | 管理员登录会话 | 撤回覆盖，插件最后推送的那条限制重新生效 |

卸载前先预览影响：`DELETE` 没带 `confirm`，或带的摘要与此刻的影响不符（预览之后插件又推送了、换了版本），都回 409，什么都不删；控制台把影响给管理员看，确认后带上它的 `confirm` 再发一次。影响只读内存，把推送状态在不含该所有者的情况下重新组合一遍再丢掉。卸载不带 `purge` 时保留数据目录与数据标记（`plugin.<id>.data`），重装不会读到旧构建写的数据之外的东西；带 `purge` 的卸载（对已卸载、只剩数据目录的包同样可以）把目录连数据与快照移到 `.trash/` 并删掉数据标记。每个包在服务端留下的东西列在 `pluginbridge.PerPackageArtifacts`（推送状态的各张表、数据标记、包目录下的文件、内存里按包 id 记的结构），契约测试 `TestUninstallLeavesNothingUnderThePackageID` 装一个用到全部的包，卸载、再清除，逐项确认都不在了；新增按包记的状态要在同一改动里加进这份清单。

升级（先停旧版本再做快照，复制时数据不会再变）：校验、批准并解包新包之后（同一版本号已解包而内容不同的，此时就以 409 `PLUGIN_VERSION_CONFLICT` 拒绝，旧版本不受影响），先写升级标记 `<id>/upgrade.json`（`from`、`to`、`snapshot`、`started`），停止正在运行的版本，把数据目录复制成快照（失败回 500 `PLUGIN_SNAPSHOT_FAILED`，旧版本重新启动，什么都不变），再让新版本成为当前版本并启动；切换之后的任何失败都按健康门失败处理并回滚。新版本必须在健康门内起来并一直在线 60 秒、期间不换进程；崩溃、握手失败、被拒或停止都立即判为失败：停止新版本，回到旧版本并从快照恢复数据与数据标记，重新启动旧版本，回 422 `PLUGIN_UPGRADE_ROLLED_BACK{id, version, previous, cause_code}`。已停用或没有服务端部分的包没有健康门；升级新增主机步骤的包停在 `needs_host_setup`，不算失败。整个过程中推送的表照常执行，包从停止旧版本起按掉线计，直到某个版本重新在线，中间的重启不重算宽限。请求同步返回（最长约 80 秒），客户端断开也会做完。server 启动时若发现残留的升级标记（升级中途被停止），在校验、加载推送状态和启动任何插件之前回到 `from` 并用快照恢复数据，以 `audit=true` 记录；新版本还没成为当前版本的，只删掉标记（恢复型回滚停在切换版本之后、记录批准之前的，补记旧版本自己的批准值）。快照写成 `.partial` 再改名，只保留最近 2 个，卸载时保留、清除时随数据一起删除。手动回滚不带 `restore_state` 时数据保持新版本留下的样子，旧版本比数据标记旧就回 409 `PLUGIN_STATE_TOO_NEW`；带上时从该版本的快照恢复（没有快照回 409 `PLUGIN_SNAPSHOT_MISSING`）。升级进行中，同一个 id 的安装回 409 `PLUGIN_UPGRADE_IN_PROGRESS`。

管理调用按包 id 加锁：一个包升级等待健康门时只挡住它自己，另一个包的停用、卸载照常返回；同一个包的停用或卸载最多等 10 秒（旧版本仍在停止或数据仍在复制时，等到健康门开始），然后结束健康门（升级随即回滚）再继续。停止旧版本和复制数据时不持有插件目录的锁，别的包的安装、启停照常进行。`gpuworks plugin upgrade` 与 `rollback` 在 server 停止时离线执行（快照照做，没有健康门，新版本随下一次启动运行），server 运行时拒绝并给出对应的 API 路由。

监管事件与 stderr 按包保留，跨进程、重启与停用启用，直到卸载；重启（`restart`）换一个新实例，崩溃历史从头算，事件不丢。

每次调用以 `audit=true` 的结构化日志记录谁对哪个包做了什么、结果如何；覆盖的调用同时记下管理员的用户 id 与用户名，以及原因。

覆盖（ADR 0071 决策 10）只能放松限制类条目，看的是推送状态里有没有这条：所有者在运行、掉线、`broken`、`revoked`，或已卸载但行还留着（卸载没做完），都能覆盖，最需要救济的正是坏掉或被吊销的所有者。规则不满足时回 422 `PLUGIN_OVERRIDE_REJECTED`，`rule` 为 `not_restrict`、`no_entry`、`action`、`not_looser`、`reason`（理由超过 1024 字节；没填理由是 400 `FIELD_INVALID`），或 `superseded`（检查之后、覆盖落库之前所有者又推送改了这一条，以它的新决定为准）；撤回不存在（包括已被取代）的覆盖回 404 `PLUGIN_OVERRIDE_NOT_FOUND`。所有者活着时同样允许覆盖：它下次推送若改了这一条，以它的新决定为准，覆盖被取代并删除，管理员在扩展时钟的 24 小时内看到横幅 `plugins.override_superseded.<覆盖 id>`（`PLUGIN_OVERRIDE_SUPERSEDED`，只存在内存里，重启后不再显示），审计日志另记一条；推送不改这一条时覆盖保留。同一条目的第二个覆盖替换第一个；卸载所有者时它的覆盖与横幅一起删除。扩展时钟被校正时同样记审计日志，并显示横幅 `plugins.clock_corrected`。列表里的状态：服务端部分不能运行时是 `disabled`、`installed`（没有服务端部分）、`broken`（不再通过校验）、`revoked` 或 `needs_host_setup`（见[主机配置](#主机配置)）；其余是监管的进程状态（`on_demand` 包空闲时是 `idle`）。字段见 [API 参考](../reference/api.md)。

### 本地管理员兜底

`users.local_admin` 标记谁在所有插件掉线、被卸载或崩溃循环时仍能恢复站点（ADR 0071 决策 11）：

- 迁移 225 给升级前已有的全部管理员设上；首次启动创建的管理员带上；离线的 `gpuworks reset-password` 给管理员重新设上（普通用户不受影响）。之后新建的用户默认没有，控制台里改自己的密码不改动它。
- 生效值在每个请求读用户时计算：`local_admin && role == admin && 登录会话`。被降级的用户立即失去兜底，不需要写库；平台 API Token 永远不带；它不签进 JWT，也从不从 JWT 读取。`GET /api/v1/auth/me` 的 `local_admin` 返回这个生效值，只用于显示。
- 恢复面是核心路由：登录、`/auth/me`、修改密码与 `/api/v1/plugins/*` 不经过功能门，也不向插件提供的来源（功能可用性、横幅）发问。契约测试 `TestContractLocalAdminDuringCrashloop` 让两个插件都进入崩溃循环（一个保持全部节点，一个把集群卡数上限推成 0），本地管理员仍能重新登录、列出、调高卡数上限并覆盖一个节点保持让该节点恢复可调度、停用并卸载每个包，每次调用在 10 秒内返回。

推送的约束、准入保持与推送角色的执行器还不存在，所以目前没有地方需要按这个标记豁免；它们出现时先查这个标记。

### 尚未实现

- 本指南尚未列出的其他宿主接口；插件路由的按动作授权与持久化审计记录（今天审计只写结构化日志）。
- 目录里的内置包。
- 高可用下只在 leader 上运行插件。
- 清单词汇里除 `table.<种类>`、`host.node_identity`、`host.nodes`、`host.host_identity`、`host.clock`、`host.state_read`、`host.derive_key`、`host.setup`（[主机配置](#主机配置)）与 `routes` 以外的能力都还不提供：其余读取面（`host.*`）、`host.notify`、`metrics.expose`、设置（`host.config`）、WebSocket 路由（`routes.websocket`），以及部件能力 `node.app`（`node:workloads`、`node:jobs` 也依赖它）与 `ui.bundle`。`requires` 它们的包以 `PLUGIN_INCOMPATIBLE` 拒绝，只在 `uses` 里的照常安装但得不到服务（[权限](../reference/plugin-protocol.md#权限)）。

### 跳板：`gpuworks plugin-exec`

server 用自己的二进制拉起每个插件：`gpuworks plugin-exec --profile <json> -- <可执行文件> serve`（`internal/server/pluginexec`，不是给人用的子命令）。跳板以将来的插件进程身份施加沙箱，然后 `execve` 插件，所以沙箱由宿主施加，插件无法撤销：

| 步骤 | Linux | macOS |
|---|---|---|
| 父进程死亡信号 | `PR_SET_PDEATHSIG(SIGKILL)`，并核对父进程仍是 server，否则不启动 | 无；SDK 靠 fd EOF 与轮询 `getppid` 退出 |
| 权限 | `PR_SET_NO_NEW_PRIVS` | — |
| 进程组 | 独立进程组，server 按组发信号 | 同左 |
| 资源 | `RLIMIT_CORE=0`、`RLIMIT_NOFILE=4096`、nice 5；`RLIMIT_NPROC` 按用户计数，插件与 server 同一用户时不设，进程数由 cgroup 的 `pids.max` 限制 | 同左（无 cgroup） |
| 描述符 | fd 3、4 必须是 socket；stdin、stdout 指向 `/dev/null`；stderr 是日志 | 同左 |
| 环境变量 | 只保留 `PATH`、`TZ`、代理与 CA 变量，另设 `HOME`=数据目录、`TMPDIR`、`LANG=C.UTF-8` 与 `GWX_*`；代理变量原样传入，代理 URL 里的用户名密码因此也交给每个插件，代理凭据应只授予插件也可以用的权限 | 同左 |
| cgroup | server 的 cgroup 已委派时建 `<服务 cgroup>/plugins/<id>`，写 `memory.max`（`mem_mib`）、`memory.high`（其 90%）、`cpu.weight=50`、`pids.max=512` 并迁入；做不到只告警，插件照常启动，由 RSS 看门狗限制内存（见[监管](#监管)） | — |
| Landlock | ABI ≥ 1：包目录可读可执行，数据与 tmp 目录全部权限，CA、`resolv.conf`、`hosts`、时区只读，清单 `devices` 里的设备节点读写；[主机配置](#主机配置)已应用时另授设备目录下节点的读写、ioctl 与列目录，以及设备发现要读的 sysfs 与 udev 路径；ABI ≥ 6 另禁止向域外进程发信号（server 与其他插件） | 无 |

server 的 cgroup 算作已委派，要求 cgroup v2、`cgroup.subtree_control` 可写，并且满足其一：systemd 在该 cgroup 上标了委派属性（`user.delegate` 或 `trusted.delegate`，systemd 251 起），或 server 以非 root 用户运行在自己的单元 `gpuworks.service` 的 cgroup 里（systemd 只在 `Delegate=yes` 时把单元的 cgroup 交给 `User=`）。root 能写任何 cgroup，桌面会话的 scope 也归其用户所有，所以光是可写不算委派。`gpuworks install` 写的单元带 `Delegate=yes`（[安装布局](../reference/install-layout.md#systemd-单元)）。已委派时，server 启动插件宿主前先把自己迁进 `<服务 cgroup>/server`：cgroup v2 只允许没有进程的 cgroup 给子 cgroup 开控制器。插件停止或卸载时删除它的 cgroup 目录，插件绕开进程组留下的进程经 `cgroup.kill` 一并结束。插件运行后，server 核对它确实在自己的 cgroup 里、且内存控制器已启用，才算有 cgroup，否则改用 RSS 看门狗。

要求 Landlock 的包（证书 `min_isolation: landlock`）在没有 Landlock 的主机上不启动：跳板以退出码 78 退出并在 stderr 写出 `PLUGIN_ISOLATION_UNAVAILABLE`。跳板自己的其他退出码：64 命令行有误，71 施加沙箱失败，127 无法执行插件。

`internal/testenv/extplugin` 里验证 Landlock 的用例（插件读不到 `master.key` 与包外路径；Landlock ABI 6 及以上还验证不能向兄弟进程发信号）在进程无法使用 Landlock 时，普通测试会跳过。`make test-plugin-landlock` 则以 `GPUWORKS_TEST_STRICT=1` 运行四个指定用例，要求每项明确报告 PASS，且不得有 SKIP；无法使用 Landlock 会失败。CI 的必跑任务 `core-plugin-sandbox` 使用带有 `gpuworks-l3` 与 `gpuworks-plugin-sandbox` 两个标签的专用 runner，在默认 seccomp 配置和非 root Go 容器中运行此门禁（普通 L3 runner 池包含不支持 Landlock 的旧内核），不改变 L2/L3 或生产插件的隔离策略。无论成功失败，任务都保存内核版本、可见的 Seccomp/NoNewPrivs 状态与完整测试日志；测试还记录 ABI 探测的原始返回值和 errno，供区分内核能力与过滤策略问题，不能仅凭“Landlock 不可用”认定具体原因。

### 监管

`internal/server/pluginhost` 为每个包运行一个监管者。拉起插件的 goroutine 在插件存活期间一直锁定在自己的 OS 线程上：Linux 的父进程死亡信号属于 fork 子进程的那个线程，而 Go 运行时会回收锁定线程的 goroutine 结束时的线程，不锁定的话别的 goroutine 结束时就可能把插件一起杀掉。

| 项 | 默认值 |
|---|---|
| `initialize` 应答期限 | 10 秒 |
| 健康探测 | 每 10 秒一次，单次 3 秒；连续 3 次未通过则杀掉重启（`PLUGIN_UNHEALTHY`） |
| 卡死检测 | 每秒检查一次 fd 3 连接：一次写入阻塞超过 10 秒，或有请求体字节等着发出、却 10 秒没有写出任何请求体数据（DATA 帧），就杀掉重启（`PLUGIN_STALLED{id, stalled_s}`），不论健康探测是否通过 |
| 崩溃重启 | 1 秒起翻倍，最长 5 分钟；连续健康运行 10 分钟后重新从 1 秒算 |
| 崩溃循环 | 10 分钟内崩溃 5 次进入 `crashloop`（`PLUGIN_CRASHLOOP`），之后每 5 分钟重试一次，无人值守的离线站点能自己恢复 |
| 停止 | `POST /gwx/v1/shutdown`，10 秒后向进程组发 SIGTERM，再 5 秒 SIGKILL |
| 内存 | 有自己 cgroup 的插件由 `memory.max` 限制；没有的由 RSS 看门狗每 10 秒汇总插件整个进程组的常驻内存（Linux 读 `/proc/<pid>/stat`，macOS 用 `ps`），超过 1.5 × `mem_mib`（默认 128 MiB）就向进程组发 SIGKILL 并按崩溃重启（`PLUGIN_MEMORY_EXCEEDED`）；读不到内存只记一条告警，不停插件。自己另起进程组或会话的子进程不计入 |
| 调用并发 | 普通调用与流式调用各一个名额池，每个池至多 `max_concurrency`（默认 8）个；其中四分之一（至少 1 个，只有 1 个名额时不留）留给管理员调用；满了立即回 `PLUGIN_BUSY`，不排队；健康探测不占名额 |
| 升级健康门 | 新版本 80 秒内起来并在线满 60 秒（`AwaitHealthy`）；`on_demand` 插件由健康门拉起一次 |
| 无截止时间的调用 | 60 秒；`on_demand` 插件的冷启动也算在这个时间里 |
| stderr | 转发到 server 日志并带 `plugin=<id>`，JSON 行保留级别；每秒 50 行、突发 500 行，超出的计数后告警；最近 1 MiB 保留供诊断 |

`resident` 插件在加入后立即启动；`on_demand` 插件在第一次调用时启动（同时到达的调用只拉起一次，都等它就绪），空闲 10 分钟后停止（停止前先在同一步里拒收新调用，赶上这一刻的调用等下一次启动，不会被拒），崩溃循环时调用立即被拒，且不能推送状态（`PLUGIN_STATE_ON_DEMAND`）；安装与重新校验时，持有 `push:` 权限或声明功能的 `on_demand` 包同样以这个码拒绝。状态只有 `running` 与 `degraded` 算在线，其余（`starting`、`backoff`、`crashloop`、`stopping`、`stopped`、`idle`、`failed`）都算掉线；这只对 `resident` 插件报给推送状态。插件以不可重试的理由拒绝握手（如 `PLUGIN_CAPABILITY_MISSING`）、应答属于别的构建（`PLUGIN_HANDSHAKE_MISMATCH`）或选了未提供、未批准的能力版本（`PLUGIN_CAPABILITY_NOT_OFFERED`）时进入 `failed`，不再重启，直到再次启动。

### 健康：横幅、诊断检查与指标

插件宿主模块每当插件的状态变化或已安装的包变化（安装、启用、停用、卸载、重新校验）时重新判断每个已安装的包，结果只保存在内存里，不新增持久化的事实。只有已启用的包参与判断：管理员停用的包不报任何问题。

- **横幅**：只给管理员看，随控制台清单下发，变化时在 `feature` 主题上通知控制台重读清单。崩溃循环的包是 `plugins.crashloop.<id>`（`PLUGIN_CRASHLOOP`，参数与它的状态相同：`id`、`crashes`、`window_s`、`retry_s`）；被吊销的包是 `plugins.revoked.<id>`（`PLUGIN_REVOKED`，参数与它的列表行相同：`id`、`version`、`replacement`，没有指定替代版本时 `replacement` 为空），两者都是 `danger`；因为需要此服务器不提供的能力而不再通过校验的包是 `plugins.incompatible.<id>`（`PLUGIN_INCOMPATIBLE`，参数与安装时的拒绝相同：`id`、`capability`、`version`、`offered`、`min_core_hint`），是 `warning`。插件恢复运行、被停用或卸载后横幅随即消失。推送状态自己的掉线横幅（`PLUGIN_DOWN`）照常存在。
- **诊断检查 `plugins`**：随 `GET /api/v1/plugins` 的 `check` 下发（什么都没装时缺省，列表与原来逐字节相同），状态取 `proto.Check*`。有已启用的包处于崩溃循环、`failed`、`broken`、被吊销或 `needs_host_setup`时为 `fail`（`CHECK_PLUGINS_FAILING{ids}`）；否则有不兼容的包时为 `warn`（`CHECK_PLUGINS_INCOMPATIBLE{ids}`）；其余为 `pass`（`CHECK_PLUGINS_OK`）。`ids` 是按 id 排序、以逗号分隔的包 id。
- **指标**：`GET /api/v1/metrics/plugins` 以 Prometheus 文本格式（0.0.4）给出，管理员用登录会话或令牌（只读令牌也可以）抓取，助手读不到。这些指标供外部采集，不写入内置时序库，也不列入监控历史指标目录；调用延迟导出为直方图 bucket、sum 和 count，不生成内部 quantile 序列。标签 `id` 只取已安装的包，卸载后它的序列随之消失：

| 指标 | 类型 | 含义 |
|---|---|---|
| `gpuworks_plugin_up{id}` | gauge | 插件现在是否在服务调用（`running` 或 `degraded`） |
| `gpuworks_plugin_restarts_total{id}` | counter | 崩溃或健康检查失败之后的重启次数；第一次启动与 `on_demand` 插件的按需启动不计 |
| `gpuworks_plugin_state{id,state}` | gauge | 每个状态一条序列，当前状态为 1，其余为 0；状态取列表里的 `state`，包括 `needs_host_setup`，包括尚未完成主机配置的 `needs_host_setup` |
| `gpuworks_plugin_calls_total{id,class,outcome}` | counter | 经宿主的调用；`class` 为 `member`、`admin`、`member_stream`、`admin_stream`，`outcome` 为 `ok`（插件给出了应答，插件自己的拒绝也算）、`unavailable`、`busy`、`timeout`、`failed`（依次对应 `PLUGIN_UNAVAILABLE`、`PLUGIN_BUSY`、`PLUGIN_TIMEOUT`、`PLUGIN_ROUTE_FAILED`） |
| `gpuworks_plugin_call_seconds{id,class}` | histogram | 从调用开始到收到应答头或被拒的时间；桶上限 0.005、0.025、0.1、0.25、1、2.5、10、30、60 秒 |
| `gpuworks_plugin_push_rows_total{id,kind}` | counter | 已提交的推送里的条目数：`availability` 的行、`node_holds` 的节点、`banners` 的横幅、`attestations` 的声明，`fleet_capacity` 每次记 1；被拒或被更新的推送取代的不计 |

计数在调用路径上只做固定位置的原子加，不分配内存；计数器跨越插件的重启、停用与启用，卸载时丢弃。

### 调用与路由表

插件宿主模块经 `Bridge.Call` 调用插件；插件路由经 `Bridge.Forward` 用它，并为接住调用的那个进程签上主体（见下文「插件路由」）。只有已安装、已启用、通过校验且有服务端部分的包会被调用；其余在启动任何进程之前就被拒：未安装回 `PLUGIN_NOT_INSTALLED`，否则回 `PLUGIN_UNAVAILABLE`，参数带包的状态（`disabled`、`broken`、`revoked`、`installed`，启动被拒时为 `failed`）。调用方给出调用的类别：管理员调用（`admin`、`admin_session` 路由）可以用保留的名额，流式调用用流式名额池。调用失败分四类，调用方据此决定能否重试：

| 码 | 何时 | 能否重试 |
|---|---|---|
| `PLUGIN_UNAVAILABLE{plugin, state}` | 请求没到插件：没在监管（`state` 为 `stopped`）、没在运行、崩溃循环，或连接在请求头写出之前就断了。`state` 只给程序用，文案不插它（推送状态的降级横幅用同一个码，只带 `plugin` 与 `feature`） | 能 |
| `PLUGIN_BUSY{id, limit}` | 调用类别能用的名额都满了；`limit` 是拒绝这次调用的上限：管理员调用为池的大小，其余调用为池的大小减去保留名额 | 稍后能 |
| `PLUGIN_TIMEOUT{id, timeout_s, state}` | 截止时间到了：`state` 为 `starting` 表示 `on_demand` 插件还在冷启动，`running` 表示插件在处理 | 视调用而定 |
| `PLUGIN_ROUTE_FAILED{id}` | 插件已经拿到请求后调用失败（崩溃、断开），请求可能已部分生效 | 不能 |

请求头写出之后才失败的写入算作已送达，宁可报「可能已部分生效」也不让调用方盲目重试。插件自己的拒绝是正常的应答。

插件停止读取请求体时，HTTP/2 的连接级流控窗口被没读的请求体占满，同一连接上其他调用的请求体也发不出去，而 SDK 在另一个 goroutine 里应答的健康探测照样通过。所以宿主按连接计量进度：请求体字节在 HTTP/2 客户端里等流控的期间，只要连接上还有 DATA 帧写出就算有进展，读得慢但一直在读的插件不受影响；请求体小到装得进窗口时没有字节在等，处理函数晚点再读也不算卡死。卡死的插件被杀掉重启（`PLUGIN_STALLED`），卡住的调用回 `PLUGIN_ROUTE_FAILED`。待发送字节数与进度时间由同一个连接内的锁协调更新，逐请求、逐次写入都不分配内存。阻塞时长使用单调经过时间，宿主墙钟校正不会消耗检测预算；暂停后恢复的请求体读取不会覆盖更新的 DATA 进度，取消会清掉该请求待发送的字节。

路由表由已安装包的清单编译：已启用且通过校验的包，它的路由照常服务；停用的包没有路由（404，也不进路由清单），即使它同时被吊销或不再通过校验；被吊销的包，以及不再通过校验、但同一版本此前校验通过过的包，路由仍在，回 503 `PLUGIN_UNAVAILABLE`，不调用插件（启动时就坏了的包在重新通过校验之前没有路由）。每次安装、启用、停用、卸载或重新校验之后整表重编并原子替换；匹配不加锁，按路径段逐级查找，字面段优先于 `{name}` 参数段，只为参数分配一次；请求一直用它开始时取到的那张表。 授权快照绑定通过校验的清单摘要：转发前若选中了另一构建，在签名和发送之前以 `PLUGIN_UNAVAILABLE` 拒绝；同一构建的进程重启仍使用新进程密钥正常调用。

### 主机配置

插件永不以 root 运行；需要 root 的主机步骤只来自清单封闭的 `host_setup` 词汇（目前只有 `usb_device`），由核心渲染（`internal/server/extpkg/hostsetup`，只依赖 `pkg/extapi`），规则见[插件协议](../reference/plugin-protocol.md#主机配置hostsetup1)。

- **谁执行。** `sudo gpuworks plugin host-setup [--id ID | --prune]`（[CLI 参考](../reference/cli.md#gpuworks-plugin)）；`gpuworks install`、`upgrade` 与 `uninstall` 的核心安装钩子在主机上有包声明主机步骤、或已有本功能写的文件时也执行：安装与升级应用全部已安装包的步骤并清掉不再需要的，卸载全部删除（设备组保留）。没有这类包的主机上钩子不加入，安装流程与之前完全一样。
- **顺序。** 建组（`plugin host-setup` 还把服务账号加进组；安装钩子不改账号，把组写进服务单元的 `SupplementaryGroups=`）→ 写规则文件 → `udevadm control --reload-rules` 与 `udevadm trigger --action=change`（usb、hidraw）→ 最后写记录。中途失败就没有记录，包继续等待，下次执行重做；什么都没变时不执行任何命令。
- **记录。** `/etc/gpuworks/plugins/<id>.applied` 属主 root、0644，内容是步骤摘要（不是批准值：只改权限的升级不需要再执行 root 步骤）。`control.json` 由服务账号拥有，所以不在那里记录；server 只认 root 所有、组与其他用户不可写的记录。
- **启动门。** 包有主机步骤而记录缺失或不一致时，插件宿主不拉起它：状态 `needs_host_setup`，`PLUGIN_HOST_SETUP_REQUIRED{id, command}`。执行命令后在该状态下提供的「启用」重新检查记录并启动它，重启 server 也一样；升级改了步骤又回到 `needs_host_setup`。记录一致时才把设备目录与读取路径写进沙箱。
- **边界。** 设备插上后才出现、名字事先不知道（`/dev/hidrawN`、`/dev/bus/usb/<总线>/<设备>`），所以沙箱授予目录 `/dev` 而不是节点；设备之间的边界是文件权限：规则只把匹配设备的节点设为组 `gpuworks-usb`、0660，其他节点保持自己的属主与模式。所以这个授予覆盖服务账号本来就能打开的 `/dev` 下每个节点，不只是匹配的 USB 设备：插件以服务账号运行，默认 0666 的节点（如 `/dev/nvidia*`、`/dev/fuse`、部分 `/dev/dri/renderD*`）以及 `/dev/shm`、`/dev/mqueue` 里已有的文件它都能打开；hidraw 节点直接在 `/dev` 下，Landlock 无法收窄到目录以下。不建符号链接，因为 Landlock 检查的是路径解析到的 inode，链接什么也不多授。`TestDeviceDirGrantsNodesCreatedLater` 证明：沙箱生效后在授予目录里新建的节点能打开，授予目录之外指向它的符号链接也能打开，兄弟目录里的节点 `EACCES`。
- **其他平台。** macOS 没有 udev 与 Landlock 设备规则，声明主机步骤的包安装时以 `PLUGIN_HOST_SETUP_UNSUPPORTED` 拒绝。
- **每台主机一个安装。** 规则文件与记录在全机路径、只按包 id 命名，`install`、`upgrade`、`uninstall` 与 `--prune` 会删掉同机另一个安装写的文件（见[安装布局](../reference/install-layout.md)）。
- **卸载。** 规则文件与记录属于 root，离线 `gpuworks plugin uninstall` 删不掉，留着时打印 `sudo gpuworks plugin host-setup --prune`。

### 插件路由

已安装的包在清单里声明的路由挂在 `/api/v1/x/<包 id>/` 下（核心提供 `routes@1`；声明了 WebSocket 路由的包需要 `routes.websocket`，核心不提供，以 `PLUGIN_INCOMPATIBLE` 拒绝）。`/api/v1/x/*` 只有一个处理函数，它读上文的路由表，所以安装、启用、停用、卸载之后立即生效，不用重启。编译进 server 的扩展的命名空间优先：它们的路由先注册，落到这个命名空间却没有匹配的请求回核心的 405 或 404，不会交给插件；包 id 与编译模块（或它挂载的扩展）同名时安装以 409 `PLUGIN_ID_TAKEN{id}` 拒绝，启动时已安装的同名包记为 `broken`（带这个码），不运行也没有路由。

每个请求按这个顺序处理，前一步拒绝就不再往下：

1. **规范路径**：用原始路径（有 `RawPath` 时用它）。含 `.`、`..` 段、空段（`//`）、反斜杠，或转义的 `/`、反斜杠、`.`、`%`（`%2F`、`%5C`、`%2E`、`%25`，大小写不限）的请求回 400 `PLUGIN_ROUTE_PATH_INVALID`，不认证、不匹配、不调用插件。没有指明包的路径（`/api/v1/x`、`/api/v1/x/`，或以 `/api/v1/x//` 开头）不属于任何插件，回核心的 404，与没有挂载时相同。匹配和转发用同一个字符串，其他转义（如 `%20`）原样交给插件。
2. **匹配**：没有这个路径回核心的 404（未登录时 401，与没有挂载时逐字节相同），有路径没有这个方法回核心的 405；插件都不会被调用。
3. **认证**：按路由的 `access` 用核心的中间件：`admin` 要管理员（会话或平台令牌，读令牌只能读）、`admin_session` 要管理员的登录会话、`member` 要任意登录用户或平台令牌（读令牌只能读）；会话的跨站写入照常拒绝，会话照常续期。`public` 不认证，按客户端地址限流：每条路由、每个地址一个令牌桶（清单的 `rate`，未声明时每分钟 60 次、突发 30），地址按登录限流同样的规则取（只信任可信代理转发的地址，IPv6 按 /64 合并），桶表是至多 1 万个地址的 LRU；超出回 429 `TOO_MANY_REQUESTS` 并带 `Retry-After`。
4. **授权**：认证之后的一个接口点，今天总是通过；以后的授权层放在这里，隐藏功能的 404 仍然先于禁止动作的 403。
5. **功能门**：路由声明了 `feature` 时与编译扩展的路由同样处理：隐藏回 404，锁定回 403，只读时只放行读与 `write_when_read_only` 的写。`ungated` 路由不经过功能门。
6. **助手与 MCP**：只能读声明了 `ai_readable` 的 GET 路由，其余回 404；助手的路由列表里只列出这些路由。
7. **请求体上限**：`Content-Length` 超过 `max_body` 时在调用插件之前回 413 `REQUEST_TOO_LARGE{max_bytes}`；没有长度的请求体读到超过上限时中止转发，同样回 413。
8. **审计**：非 GET 请求结束后（包括被请求体上限拒绝的）写一条 `audit=true` 的结构化日志：包、版本、动作、方法、路由、调用方种类与用户、状态码、请求 id、耗时。
9. **截止时间**：普通路由整个请求以 `timeout_s` 为限；`stream` 路由的 `timeout_s` 只限制等待响应头的时间，之后插件每次写出都重新计时，1 分钟没有写出就断开连接。`on_demand` 插件的冷启动算在 `timeout_s` 里。
10. **去掉入站头**：客户端发来的 `Gwx-*`、`X-Gpuworks-*`、`Authorization`、逐跳头（含 `Connection` 列出的头）、它自称的地址（`Forwarded`、`X-Forwarded-*`、`X-Real-Ip`）和 `Cookie` 都不转发；`cookies: true` 的路由只转发不是核心名字（会话 cookie 与一切 `gpuworks` 开头的名字）、也不以 `__Host-` 开头的 cookie。合法的 `traceparent` 原样转发，否则生成一个新的。
11. **签名转发**：插件宿主用接住这次调用的进程的主体密钥签上 `Gwx-Principal`、`Gwx-Principal-Mac` 与 `Gwx-Feature-Access`（主体带 `route_action` 与 `route`，有效期为 `timeout_s` 加 5 秒；过长或不可打印的请求 id 截到 128 字节或换成新的），转发到插件的 `/r<路由路径>`；`admin` 与 `admin_session` 路由按管理员调用计（可用保留名额），`stream` 路由用流式名额池。

调用失败时的应答：

| 情形 | 状态 |
|---|---|
| `PLUGIN_UNAVAILABLE`（请求没到插件，含 `broken`、`revoked` 包的路由） | 503，`Retry-After: 5` |
| `PLUGIN_BUSY` | 503，`Retry-After: 1` |
| `PLUGIN_TIMEOUT` | 504 |
| `PLUGIN_ROUTE_FAILED` | 502，不带 `Retry-After` |
| 公开路由超出速率 | 429 `TOO_MANY_REQUESTS` |
| 没有匹配的路由 | 核心的 404，不调用插件 |

插件的应答只按白名单传给客户端：

- **头**：只保留 `Content-Type`、`Content-Length`、`Content-Encoding`、`Content-Disposition`、`Cache-Control`、`ETag`、`Last-Modified`、`Retry-After`；`Location` 只在仍指向 `/api/v1/x/<id>/` 之下的规范路径时保留；其余（`Access-Control-*`、逐跳头、插件自己的 `Content-Security-Policy` 等）一律丢弃。每个应答都带 `X-Content-Type-Options: nosniff` 与 `Content-Security-Policy: sandbox; default-src 'none'`。
- **内容类型**：`text/html` 与 `application/xhtml+xml` 只允许声明了 `html: true` 的路由返回，否则回 502 `PLUGIN_ROUTE_RESPONSE_INVALID{id}`；没有内容类型时补成 `application/octet-stream`；不是单个媒体类型（如 `text/plain, text/html`，浏览器会取列表最后一项）同样回 502；其余按规范化形式发出。
- **Cookie**：只有 `public` 且 `cookies: true` 的路由能设 cookie，路径一律改成 `/api/v1/x/<id>/`、去掉 `Domain`；与核心 cookie 同名或以 `__Host-`、`__Secure-` 开头的被丢弃。过滤只针对插件的头，核心中间件已经写下的续期会话 cookie 照常发出。
- **产品错误**：带 `Gwx-Product-Error` 的应答体必须是合法的 gwx/1 错误（至多 64 KiB）且状态为 4xx 或 5xx，码必须是包清单 `codes` 列出的码或线协议自己的 `PLUGIN_WIRE_*` 码（插件不能用核心或其他包的码作答），按插件的状态、码与参数原样传给客户端；否则回 502 `PLUGIN_ROUTE_RESPONSE_INVALID`。
- **应答体**：`stream` 路由逐块转发并立即刷新，其余至多 64 MiB。响应头发出之后才失败（插件中途断开、长度不符、超过上限、流式空闲超时）时中止连接，不留下被截断的 2xx。

路由清单（`api.RouteInventory`）不为挂载本身输出任何行，每条插件路由一行：`METHOD /api/v1/x/<id><pattern> [access=<access>] [action=<动作>] [feature=<键>]`（未门控时为 `[ungated]`）`[x=<id>@<版本>]`。什么都没装时清单与没有挂载时逐字节相同；停用的包不出现，`broken`、`revoked` 的包照常出现。规模用例 `TestFleetPluginRouteLoad`（`GPUWORKS_TEST_PERF=1`）在 5 万个公开客户端地址下测每个请求的 server CPU 与内存，改动插件请求路径时要跑。

### gwx/1 连接与宿主接口

每个插件有两对 socketpair。fd 3 上 server 是 HTTP/2 客户端：先 `initialize`（server 的能力、批准的授权、已持有的表、每次拉起新生成的主体密钥），核对应答的 `id`、`version`、`manifest_sha256` 与包一致、`use` 落在提供且批准的范围内，然后探测健康、转发路由与服务调用，每个调用带 `Gwx-Deadline-Ms`。fd 4 上 server 提供宿主接口：`PUT`/`PATCH /host/v1/state/{kind}` 交给推送状态的实现（一个接口），`GET /host/v1/health`，以及读取 `POST /host/v1/node-identity`、`GET /host/v1/nodes`、`POST /host/v1/host-identity`（`pluginhost.Reads`，由 app 以 store 与本机的主机身份实现；没有 `Reads` 时回 `PLUGIN_HOST_ROUTE_UNKNOWN`，未批准的权限回 `PLUGIN_HOST_NOT_PERMITTED`，未协商的能力回 `PLUGIN_CAPABILITY_NOT_OFFERED`，越界参数回 `PLUGIN_HOST_ARGUMENT_INVALID`），以及时钟、就绪、自有状态与派生密钥（`GET /host/v1/meta`、`POST /host/v1/clock/correct`、`POST /host/v1/state/ready`、`GET /host/v1/state/{kind}`、`POST /host/v1/derive-key`，各自需要的能力、角色与权限见[插件协议](../reference/plugin-protocol.md#宿主接口的时钟就绪自有状态与派生密钥)，由 `pluginbridge` 接到推送状态、由 `app` 从主密钥派生）；每个插件每秒 20 次、突发 100 次（`PLUGIN_HOST_RATE_LIMITED`），节点清单另有每秒 2 页、突发 4 页的桶。所有者是这条连接在握手时绑定的包，推送里没有任何身份字段；握手被接受之前到达的推送等待握手结果，握手失败即回 `PLUGIN_HOST_NOT_READY`。

### 插件 SDK：`pkg/pluginsdk`

插件作者用 `pkg/pluginsdk`（与 `pkg/extapi` 一样是独立模块，只依赖标准库与 `pkg/extapi`）：`pluginsdk.Main(cfg)` 打开两个描述符、回答握手（协议与能力协商，拒绝时用 `PLUGIN_PROTOCOL_UNSUPPORTED` 或 `PLUGIN_CAPABILITY_MISSING`）、在握手被接受后运行 `Start`、回答健康、把其他调用交给插件的 `http.Handler`；收到 `shutdown`、server 一端关闭或父进程不在时退出。`PushState` 跳过 server 已持有且摘要相同的表，推送的代数取 `max(自己的计数, 持有的代数)+1`，所以存储被恢复或清空的插件不会因代数过期被拒。`Meta`、`CorrectClock`、`Ready`、`PushedState` 与 `DeriveKey` 对应上述宿主接口调用，握手没协商其能力时直接返回 `ErrNotNegotiated`。`pkg/pluginsdk/plugintest` 在进程内用真实 socketpair 扮演 server，供插件自己的测试使用；它按与 server 相同的码、理由与参数拒绝这些调用，派生密钥用固定的测试主密钥。

### 一致性检查：`pkg/pluginsdk/conformance`

插件作者在插件自己模块的测试里，对将要打进包里的二进制运行一致性检查：

```go
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Config{
		Binary:   "bin/linux_amd64/acme-backup",
		Manifest: "manifest.json",
	})
}
```

`Run` 读清单（解码失败、没有 `runtime` 或二进制不可执行时直接失败），然后多次拉起二进制：像 server 一样把两条 socketpair 作为 fd 3、4 交给它，只给 server 也会给的环境变量（`PATH`、`TZ`、`HOME`、`TMPDIR`、`LANG` 与 gwx/1 变量；`Config.Env` 只用于作者自己的测试开关），不加沙箱，对面是 `plugintest` 的假 server（`plugintest.StartProcess`）。握手授予清单里的全部权限与角色，即管理员批准、证书授予之后的样子。每条规则是一个以规则命名的子测试（`conformance.Rules()`），失败时子测试名就是被违反的规则，并附上插件的 stderr：

| 规则 | 检查 |
|---|---|
| `initialize_answers_as_the_installed_package` | `initialize` 在 10 秒内应答，`id`、`version`、`manifest_sha256` 与清单一致 |
| `uses_only_declared_capabilities` | 应答的 `use` 只含清单 `requires` 与 `uses` 里的能力，版本在提供范围内且不超过清单允许的；运行期间没有宿主调用因未声明的能力、权限或角色被拒（`PLUGIN_CAPABILITY_NOT_OFFERED`、`PLUGIN_HOST_NOT_PERMITTED`、`PLUGIN_ROLE_REQUIRED`），也没有推送未协商的表 |
| `health_answers_within_the_timeout` | 健康在 3 秒内应答 |
| `shutdown_exits_within_the_deadline` | `shutdown` 之后 10 秒内以状态 0 退出，赶在 server 发 SIGTERM 之前 |
| `unchanged_push_is_skipped` | 对已持有同摘要表的 server 重启后，不再推送这些表 |
| `changed_push_is_applied` | 对持有不同表的 server 重启后，以高于持有代数的代数推送并被接受 |
| `on_demand_package_does_not_push` | `on_demand` 的包不推送任何表 |
| `routes_serve_a_valid_principal` | 每条声明的路由收到按 server 方式签名的请求时，不以主体为由回 403 |
| `routes_refuse_a_missing_principal` | 不带主体的请求回 403 |
| `routes_refuse_a_wrong_mac` | 主体用别的密钥签名的请求回 403 |
| `routes_refuse_another_routes_action` | 主体的 `route_action` 是另一条路由的请求回 403（所有路由同一个动作时，用一个没有路由声明的动作） |

推送两条规则先对什么都不持有的 server 运行一次，记下 `PushWait`（默认 2 秒）内被接受的每种表，再据此重启两次；插件在这段时间里什么都没推，或包是 `on_demand`，两条规则跳过。没有声明路由时路由规则跳过，`resident` 的包跳过 `on_demand` 规则。路由请求的路径参数都填 `conformance`，有请求体的方法发 `{}`；公开路由用 `public` 主体，其余用已登录的平台管理员，路由的功能键签为 `full`。三个时限默认取 server 的值，`Config` 可以改。仓库里的假插件（`internal/testenv/extplugin/fakeplugin`）在 CI 里跑这套检查，证明它对正确的二进制全部通过；它的故障开关 `--skip-principal-check`、`--ignore-route-action`、`--hang health` 与多声明的 `--requires` 各自只让对应的规则失败。

### 推送状态

首批五种推送表（`availability`、`fleet_capacity`、`node_holds`、`banners`、`attestations`）在核心里的部分在 `internal/server/pushstate`，经上文的插件宿主模块接收插件的推送、在启动时读入并注册为钩子：

- **接收**：`State.Push` 接收监管按连接认定的推送方身份（包 id、发布者、证书授予、批准的权限、清单的码、进程代数）和一份推送。`availability` 的每一行只接受该键的权威（`gate: authority` 时为声明它的发布者证书写明的 `authority_id`，`gate: self` 时为声明它的包），无前缀的键只接受项目发布者的包；`fleet_capacity` 要证书授予该特权表，`attestations` 要推送方是自己命名空间的权威，其余要批准的 `push:<种类>`；通知的码必须列在包的清单里。代数不高于已持有或待提交的被拒（`PLUGIN_STATE_STALE`）；被替换的进程的连接也被拒（`PLUGIN_STATE_CONNECTION_REPLACED`）。
- **写入**：待写推送按所有者与种类合并（后到的取代先到的，先到的收到 `superseded_by`），每个 tick（至多 1 Hz）把全部所有者的改动放进一个写事务，再替换一个快照指针；写入失败则什么都不生效（`PLUGIN_STATE_NOT_SAVED`）。持久化用核心迁移建立的 `pstate_*` 带类型表：时间线是步骤行，通知是码列加带类型的参数行，没有 JSON 列；只用于显示的 `display` 不保存。按条目（功能键、节点 id、横幅 id、证明名）写差量。
- **启动**：启动时在任何插件起来之前读入全部推送表；版本标记（`pstate.<种类>`）比本构建新则以 `PLUGIN_STATE_TOO_NEW` 拒绝启动，存储的限制解不开则拒绝启动而不放松，存储的授权解不开则略过（视为缺席）。
- **执行**：功能来源、节点插入守卫、调度保持者（来源 `plugins`）与横幅各是一次原子读加查表，零分配。没有包作答的键为 `locked`，带 `FEATURE_PROVIDER_ABSENT`。四者都是接口 `pushstate.Evaluator` 的方法，`app.NewPushedStateHooks` 只经这个接口把它们适配成 `Host` 的钩子类型；`TestHooksDoNotAllocate` 逐个方法测分配，`TestEveryEvaluatorIsMeasured` 用反射读出接口的全部方法，核对每个都有用例，新增的热路径应答因此不会漏测。
- **时间**：时间线按 extclock（`max(墙钟, 高水位)`）在各自的时刻生效，插件死了也一样；高水位每分钟和关机时保存。`State.CorrectClock` 一次至多移动 366 天，回调审计并显示管理员横幅；步骤不被消耗，所以往回校正会让之后的步骤重新未生效。持有根授予的 `clock_authority` 角色的插件经宿主接口 `POST /host/v1/clock/correct` 走同一条路径（见[插件协议](../reference/plugin-protocol.md#宿主接口的时钟就绪自有状态与派生密钥)），`GET /host/v1/meta` 读墙钟、extclock 与已保存的高水位。
- **掉线**：`State.OwnerDown` 记下持久化的掉线起点（重启不续期），授权在宽限（默认 1 小时，插件可推 60 秒到 24 小时，站点只能经 `server.yaml` 的 `plugins.policy.max_down_grace` 调低，见[配置参考](../reference/configuration.md#插件的信任与站点策略)）加 2 分钟启动延长（每段掉线都加，运行中崩溃的插件也一样，以免区分启动与运行中掉线；从持久化的掉线起点算，server 或插件重启都不再叠加一次；进程调用过 `POST /host/v1/state/ready` 的，`State.Ready` 让它之后的掉线不再加启动延长，这个标记不持久化，从不改动掉线起点）之后降为 `min(当前, down_access)`，带 `PLUGIN_UNAVAILABLE`；限制永不失效。已安装但不再通过校验的包保留限制与横幅，只丢弃授权。
- **离线维护**：root 在 server 停止时执行的 `gpuworks plugin clock reset` 与 `gpuworks plugin pstate clear` 只往 `<data>/plugins/maintenance.jsonl` 追加记录（`pluginbridge.MaintenanceEntry`）；下次启动时 `pluginbridge` 在 `Load` 之后、拉起任何插件之前按顺序应用：时钟校正走 `State.CorrectClock`（同样的上限、审计与横幅，插件经 `GET /host/v1/meta` 读到新的高水位），清除走 `State.ClearTable`（一次提交里删除该所有者的一张限制表与它的覆盖，并请求一次保持的重新应用；授权类不能清除）。每条应用后写 `audit=true` 日志；失败的记录与其后的记录留到下次启动，并显示 `PLUGIN_MAINTENANCE_FAILED` 横幅（见 [CLI 参考](../reference/cli.md#维护日志)）。
- **覆盖与卸载**：`State.Override` 对限制类条目（一个节点保持，或一项容量上限）只能放松，持久化，绑定所有者的代数，同一条目的旧覆盖在同一次提交里删除；所有者再推并改了同一条目时被取代、回调并显示 24 小时的管理员横幅。`State.RemoveOverride` 在一次提交里撤回一个覆盖并请求一次保持的重新应用。`State.Uninstall` 在一次提交里删掉所有者的全部行，并请求一次保持的重新应用。
- **容量选择**：`fleet_capacity` 给上限和可选的优先保留节点 `keep`（多个所有者取并集），选哪些节点由核心决定：`keep` 里的节点在前，其余按接入顺序（`internal/server/fleetcap`，满了以后排在后面的节点一律保持），`keep` 一变也会重新选择；它在节点集合主题的变化后合并读一次节点列表（间隔至少 2 秒），插件掉线时加入的节点同样按这条规则放置。还没放置的节点只有计入它会超过上限时才保持；上限一变（包括所有者被卸载），选择都会请求一次保持的重新应用，这样的节点也随之释放。

## 开源代码不做的事

- 不解释任何功能键，不按版本名分支；核心代码不点名任何功能。
- 不在任何请求、插入、状态上报、认领或放置路径上调用进程外的扩展；不在写事务里运行 WASM 之类的扩展代码。
- 不交出 Store 或原始数据库句柄给扩展。
- 不提供 `init()` 注册或新的包级全局变量：一切经 `cli.Options` 与 `app.Config` 传入。
- 不中断正在运行的负载：功能门只作用于新的请求与写入，保持者只停止调度、不驱逐。
