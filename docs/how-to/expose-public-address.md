# 给控制台一个公网地址

在 NAT 后的开发机、GPU 节点或内网服务器上，为已运行的控制台开启临时公网入口，并在使用后关闭。本文使用 `cloudflare-quick`，不用于长期入口或节点接入。

**前置条件**：

- server 已启动，并能在本机访问控制台，见[安装和升级控制面](install-server.md)。
- 能登录 server 主机的终端，并有权读写这套安装的数据目录。下文在实际安装目录执行 `./gpuworks`；安装器不会自动将它加入 `PATH`。
- server 主机能访问公网（下载 cloudflared 并连接 Cloudflare）；quick 提供方不需要 Cloudflare 账号和域名。
- 只作临时入口：quick 地址在连接器重建时会变（server 重启不会重建它），且不支持 SSE；节点安装请填 server 自身的稳定地址。

一个数据目录只管理**一个入口**。当前开放的提供方如下：

| 提供方 | 需要准备 | 地址 | SSE（流式对话、实时日志） | 节点接入 |
|---|---|---|---|---|
| `cloudflare-quick` | 无账号、无域名 | 临时随机域名；server 重启不变，连接器重建后变化 | 不支持 | 请填 server 自身的稳定地址 |

`cloudflare-named` 尚未开放，固定入口的替代方式见[下文](#尚未开放named)。

## 最短路径：quick

1. 在 server 主机的实际安装目录执行：

   ```bash
   ./gpuworks expose up
   ```

   核对输出中的数据目录和转发地址是否属于目标安装。命令会在需要时下载 cloudflared，启动连接器，等待注册成功且公网探测通过后打印地址。

2. 在外网浏览器打开打印的地址，使用已有账号登录，确认进入的是目标控制台。入口状态应为 `ready`：

   ```bash
   ./gpuworks expose status
   ```

3. 使用结束后，按[常用命令](#常用命令)关闭入口。机器重启后，默认后台方式需要重新执行 `expose up`；Linux 需要开机自启时可选择 `--systemd`。

**quick 的限制**：连接器重建后地址会变，server 自身重启不会重建连接器。[Cloudflare Quick Tunnels](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/) 不支持 SSE，也不提供可用性保证；不要用它验证流式对话、实时日志，或写入长期配置和节点安装命令。

命令超时或按 Ctrl-C 只结束等待，**连接器仍会继续运行**。用 `status` 查看进展；要取消入口，执行 `down`。状态为 `degraded` 时按[网络与包源](#网络与包源)检查，再从外网复查。

## 尚未开放：named

`cloudflare-named` 当前不接受用户启用。需要固定域名或 SSE 时，使用[生产环境部署](deploy-production.md)中的固定入口；节点使用其可达的 server 稳定地址。

## 常用命令

按需要选择命令，不必依次全部执行。以下命令仍在 server 的实际安装目录运行：

| 操作 | 命令与结果 |
|---|---|
| 开机自启（仅 Linux systemd，需 root） | `sudo ./gpuworks expose up --systemd`；已经以后台进程运行时，先执行 `down` 再切换 |
| 查看状态 | `./gpuworks expose status`，查看状态、地址、origin 和最后错误 |
| 仅取得可用地址 | `./gpuworks expose status --url-only`，无可用地址时以非零码退出 |
| 查看连接器日志 | `./gpuworks expose logs -f` |
| 关闭入口并保留配置 | `./gpuworks expose down`；停止连接器服务，systemd 模式还会删除 unit，须用 sudo |
| 关闭并清除本地入口配置 | `./gpuworks expose rm`；删除配置、观测、隔离目录与托管 token，systemd 模式须用 sudo |

关闭后再执行 `status`，确认入口已停用，不再返回可用地址。

- `status` 只在 **连接器服务在运行 + revision 匹配 + ready** 时才给地址；锁文件存在不算在线，
  陈旧的 ready 快照也不会被当成可用地址。
- `down` 先写 disabled，再等连接器服务退出；`rm` 保留二进制缓存与日志（排查用），不会碰 Cloudflare
  上的 tunnel、路由与 DNS——那些是你的资源，`rm` 也不删除 `--token-file` 指向的文件。
- 配置未变时 `up` 复用当前入口（quick 不会白白换一个新地址）。

## data-dir 与 origin

- data-dir 解析顺序：`--data` > 二进制旁边的 `server.yaml` > 默认安装目录的 `server.yaml`
  （`/opt/gpuworks/server/server.yaml`，macOS 上 `~/gpuworks/server/server.yaml`）> 开发态 `./data`。CLI 每次都会打印它用了哪个
  （`--url-only` 除外，那时 stdout 只有地址）。
- origin 一般不用填：用 server 启动时记下的**实际监听**地址（通配地址转成 loopback，IPv6 也处理），
  没有记录时取安装目录 `server.yaml` 的第一个 console 地址；都不合适时用 `--origin`。server 改了监听
  地址后，重新执行一次 `expose up`。
- 入口启用期间，server 信任来自本机的连接转发的 `X-Forwarded-*`（连接器总在本机，不管 origin
  写的是 loopback 还是本机网卡地址），不需要为此改 `trusted_proxies`；`expose down` 后恢复只信任
  `trusted_proxies`，见 [ADR 0049](../adr/0049-expose-trusts-same-host-connector.md)。

## 网络与包源

cloudflared 优先使用进程 `PATH` 中兼容的版本，其次使用受控缓存。需要下载时：

- Linux 从 `pkg.cloudflare.com` 取得 deb 或 rpm 并校验，解包到缓存，不安装系统包。
- Mac 从 GitHub 的 cloudflared release 下载对应架构的 `.tgz`。若该地址不可达，可预先把兼容的 cloudflared 放到执行命令所用的 `PATH` 中。

出现下载失败或 `degraded` 时，先执行 `./gpuworks expose status` 和 `./gpuworks expose logs -f`，根据最后错误检查包源、DNS 和 server 的本机访问。修复后再次查看状态，并从外网登录验证。

DNS 传播延迟等原因会使公网探测暂时失败；连接器可能仍在线，探测恢复后会自动回到 `ready`。不要仅凭 `degraded` 重建入口，以免更换临时地址。

出口、包版本与当时的速率测量（2026-09-22，上海电信，含 320 KB/s 级别的 quick 吞吐）只代表
那次测量，不是对其他网络或其他提供方的承诺。

## 安全边界

入口暴露的是整个 server：UI/API 仍需登录会话，`/v1` 默认仍需 API key（只有管理员显式打开安全设置里的
`inference_open_access` 才接受不带 key 的请求，见[OpenAI 兼容网关的认证](../reference/openai-gateway.md#认证)），和在内网一样。**没有**做
路径过滤，也没有配置 Cloudflare Access。server 自身有登录失败限流；需要额外入口策略时应单独配置。团队里加节点时，
如果填的是 quick 地址，界面会提示这是临时地址。

## 连接器服务与 server 是分开的

- **server 重启、升级都不碰它**，quick 地址保持不变。server 停着的那段时间，地址返回 Cloudflare 的错误页，
  入口状态显示 `degraded`；server 回来后自动恢复为 `ready`。
- server 每次启动都会把自己实际监听的地址写到 `<data>/expose/server-origin`，`expose up` 把它作为转发目标
  写进配置；也可以用 `--origin http://HOST:PORT` 指定。
- **默认手动启动**：`./gpuworks expose up` 把连接器服务作为后台进程启动（独立 session，不在
  `gpuworks.service` 的 cgroup 里），机器重启后**不会**自动恢复，需要再执行一次 `expose up`。
- **`--systemd`（仅 Linux）**：`sudo ./gpuworks expose up --systemd` 改为安装独立的 `gpuworks-expose.service`
  （`Restart=on-failure`、开机自启）。
- 服务以数据目录属主的账号运行（安装版里就是 server 的服务账号），root 执行 `expose up` 也不会让
  cloudflared 跑成 root。
- 入口被关闭（`expose down`）后，服务停掉 cloudflared 并自行退出；`--systemd` 模式下 `down` 还会删除 unit。
