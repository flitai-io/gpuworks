# ADR 0024: 数据面 lane 断开时不回退控制面

日期：2026-09-24 · 状态：已接受 · 取代 `artifacts/archive/2026-09-08-tunnel-bandwidth/plan.md` §3.2「data lane 单独掉线」一条

双通道隧道（ADR 前身：`artifacts/archive/2026-09-08-tunnel-bandwidth/plan.md`）原计划在 data lane 掉线期间，把 tunnel/logtail/model.\*/image.\* 这些数据面 stream kind 回退到 ctrl session，「正在飞行的 stream 失败（proxy 返回 502，与今天断线行为一致）」。这个回退违背了双通道设计本身的隔离目的：agent 数据面独立退避重连，上限 30 秒，这段窗口内推理代理、日志跟随、镜像/模型传输的全部流量会挤上 ctrl session，拖慢心跳、状态上报与 web 终端，把一次数据面故障放大成控制面故障，最坏情况下让心跳超时把在线节点误判离线。更细的问题是探测竞态：TCP 已经断开但 yamux 尚未通过 keepalive/读循环察觉（`IsClosed()` 仍报 `false`）时，新流按旧逻辑仍会尝试开在这个「看起来健康」的死 session 上，写头失败后直接向上传播成 502，而不是任何形式的重试。

现在把 `Session` 的 data lane 状态显式分为三态：`none`（从未 attach 过——老 agent、`--data-lane=false`、`server_dial`，单通道是它们的正常、永久形态）、`attached`（当前健康）、`detached`（attach 过，当前掉线）。`none` 态维持原计划的行为，数据面 kind 继续走 ctrl，因为这类节点本来就只有一条物理连接。`attached` 态直接用 data lane。`detached` 态不再回退 ctrl：调用方在自己的 context 内等待 data lane 重新 attach，等待上限是一个固定常量（`hub.dataLaneReattachWait`，5 秒），够覆盖 agent「attach 成功后掉线，第一次重连立即进行、不过退避」这一常见情形的一次往返；等不到，返回类型化哨兵 `hub.ErrDataLaneUnavailable`，调用方按连接级失败处理。

探测竞态的收敛**只在确认会话真的死亡时才生效**：`OpenStream` 失败且该 session 此刻 `IsClosed()` 为真（go-yamux v5 在把由关闭引发的错误返回给调用方之前，一定先设置内部 shutdown 标志再关闭 `shutdownCh`，所以调用返回后立即查 `IsClosed()` 不会漏判也不会误判），或者流已经开出但写头失败（yamux 的发送路径是 fire-and-forget，写失败是这条路径上唯一拿得到的信号，因此无条件按死亡处理）——这两种情况才标记为 detached 并 `Close()` 该 session，再走等待重连-重试，全程不落到 ctrl 上。`OpenStream` 因 ctx 取消/超时、资源作用域限制、流号耗尽等原因失败时，session 本身仍然健康，直接把错误原样返回给调用方，不 detach、不重试、不 close——早期实现曾对任何 `OpenStream`/写头失败都无条件 detach，这会把一次请求取消放大成节点级数据面不可用（等待的 lane 明明还在，却被误判为已断，直到它真的断开才会被替换），已改正。

流已经开出、请求还没拿到响应时 data lane 死亡（2026-10-09 补充）同样按「这个节点的数据通道不可用」报告：数据通道上开出的流（`hub.laneStream`）在读写出错且所属 yamux 会话此刻 `IsClosed()` 为真时，把错误包成 `ErrDataLaneUnavailable`，并立即把这条 lane detach。判断是确定的：go-yamux v5 在用 `stream reset: connection closed` 强制关闭会话内所有流之前已经置好 shutdown 标志；agent 单独 reset 某个流（拨引擎失败、引擎断开）时会话仍然存活，错误原样保留，proxy 照旧回 502。于是 proxy 在响应头之前遇到这类失败，与开流时 lane 已断一样回可重试的 503（带 `Retry-After`），多副本时由连接级重试换副本。响应头已经发出之后的断开改不了状态码，客户端看到的是被截断的响应。

代价是明确的：detached 窗口内，单副本部署收到的请求会等最多 5 秒再拿到一个可重试的 503（带 `Retry-After`），而不是立即失败的 502；多副本部署由推理网关的连接级重试（ADR 0023）换到另一个副本兜底，用户侧通常感觉不到。这个代价被认为远小于把数据面故障放大成控制面故障的风险，尤其是控制面延迟一旦失控可能连带触发节点误判离线、正在运行的实例被判定失联等更严重的连锁后果。

不做的事：不新增持久化数据结构，不改协议 wire（`DataHello`/`SessionID`/`stale_session` 拒绝逻辑不变）；`file.large_read`（`OpenLargeFileRead`）维持它更严格的既有语义——要求已经 `attached`，`none` 与 `detached` 都直接拒绝，不等待，因为这个 stream 的契约本来就是「没有 lane 就没有这次抓取，调用方立刻走另一条路径」，多等 5 秒只会拖慢那条回退路径。

后续：agent 上送的指标与日志批次也按本规则路由，见 [ADR 0064](0064-telemetry-stream-on-the-data-lane.md)。另，`server_dial` 节点在 agent 开启数据通道时同样由 server 拨入第二条通道，并非永久处于 `none` 态。
