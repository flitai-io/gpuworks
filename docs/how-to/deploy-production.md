# 生产环境部署

将已安装的 server 配置为团队或生产入口，完成 HTTPS、网关长连接和凭证备份检查。本文适用于原生 TLS 或前置反向代理，两种方式按部署环境选择。

**前置条件**：

- 已按[安装和升级控制面](install-server.md)装好 server。
- 有覆盖入口域名的 TLS 证书，并能修改 server 或反向代理的配置。使用云负载均衡时，能管理其监听、证书与超时设置。
- 安排配置生效所需的重启窗口；重启 server 会断开在途请求。
- 下文主机调优命令仅用于 Linux systemd 安装，执行修改需要 root 或 sudo；仅配置云负载均衡时不需要 server 主机的 root 权限。

## 反代与 TLS

生产入口必须使用 HTTPS。可以配置下文的原生 TLS，或前置 Nginx、Caddy、云负载均衡终止 TLS，并仅在可信代理网段配置
`server.yaml` 的 `trusted_proxies`（或 `--trusted-proxies`）。只有来自这些地址的
`X-Forwarded-*` 头会被采信：`X-Forwarded-For` 用于记录 agent 接入连接的来源地址，
`X-Forwarded-Host` / `X-Forwarded-Proto` / `X-Forwarded-Port` 用于控制台写请求的 Origin 检查和
Secure Cookie，都不用于生成任何对外地址。server 不存储也不推断自己的对外
地址：给用户和 install script 用的地址来自浏览器当前访问的地址（UI 各处据此预填，可以改）或
调用方在 API 里显式传入的 `server_url`，反代或域名场景下这个地址天然就是正确的那一个，见
[ADR 0025](../adr/0025-server-holds-no-external-address.md)。

HTTPS 响应会带 HSTS，登录和退出 Cookie 使用 `Secure`；HTTP 本地开发入口不设置这些 HTTPS 专用标记。
两种部署都返回防嵌套、MIME 嗅探、引用来源与权限策略头。全局 CSP 限制 `frame-ancestors`、`base-uri` 和
`object-src`；Playground 的沙箱预览保留自己的脚本与网络策略，避免继承全局脚本限制而失效。

### 原生 HTTPS

配置前确认以下条件：

- 同时准备证书链与私钥，服务账户能读取二者；缺少任一项或读取失败时，server 会拒绝启动。
- 使用 `listen.console` 和本机具体 IP。TLS 使用通配监听时会拒绝启动；也不要用 `--addr` 覆盖，它不会创建服务自调用所需的本机维护监听，启动时同样会被拒绝。
- 所有非维护监听（包括节点和推理入口）使用 TLS，公网明文请求不能登录。`127.0.0.1` / `::1` 的本机维护入口仍使用 HTTP，供升级、安装和服务自调用，不能映射给外部用户。

1. 在 server 主机的实际安装目录编辑 `server.yaml`，将地址和文件路径替换为实际值：

   ```yaml
   listen:
     console: [192.0.2.10:8800] # 替换为本机实际地址
   tls_cert_file: /etc/gpuworks/fullchain.pem
   tls_key_file: /etc/gpuworks/privkey.pem
   ```

2. 按原服务管理方式重启 server，检查启动日志；随后通过证书覆盖的域名访问 `https://域名:8800` 并登录。
3. 需要更新 agent 的连接配置时，使用对应 `https://` / `wss://` 地址；server 不会自动修改 agent 已保存的地址。

原生 TLS 不会替你申请证书。证书更新后，需要重启 server 才生效。

### 反代配置要点（控制台）

浏览器的控制台写请求（登录、退出、各类修改）带 `Origin` 头，server 拿它和自己看到的
「协议 + 主机 + 端口」比对，不一致就拒绝，返回 403 `MANAGEMENT_ORIGIN_REJECTED`。反代必须把浏览器
访问用的主机和端口原样转给 server。

1. 在 Nginx 的 `http` 配置中定义后端，并配置下面的 TLS `server`；已有 TLS 虚拟主机时，把对应 `location` 合入现有块，避免重复创建同名入口。下例 `192.0.2.10:8800` 替换为 Nginx 能访问的控制台监听地址：

```nginx
upstream gpuworks {
    server 192.0.2.10:8800;
}

server {
    listen 443 ssl;
    server_name gpuworks.example.com;             # 替换为实际域名
    ssl_certificate /etc/gpuworks/fullchain.pem;
    ssl_certificate_key /etc/gpuworks/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    location / {
        proxy_pass http://gpuworks;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;              # 带端口；$host 不带端口
        proxy_set_header X-Forwarded-Host $http_host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Upgrade $http_upgrade;        # agent 接入、Web 终端走 WebSocket
        proxy_set_header Connection "upgrade";
        proxy_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

2. 在 GPUWorks 的 `server.yaml` 中把 Nginx 连接 server 时的源地址加入 `trusted_proxies`。同机 Nginx 可填其实际使用的回环地址；远端 Nginx 填其源 IP 或受控网段，不要填用户地址、域名解析地址或 `0.0.0.0/0`。
3. 将明文 80 入口重定向到 HTTPS，并限制外部直接访问反代后的 HTTP 端口。需要推理入口时，先补齐下节 `/v1/` 配置，再应用和验证。

保留 `$http_host` 可原样转发浏览器使用的端口，特别是 `https://<地址>:8808` 这类非默认端口。可信代理还会使用 `X-Forwarded-Proto`、`X-Forwarded-Port` 还原外部入口；配置错误时，登录或修改操作可能报 `MANAGEMENT_ORIGIN_REJECTED`。

### 反代配置要点（/v1 网关）

推理请求经过反代时，应关闭响应缓冲并允许长请求。将下面的 `location /v1/` 放入**上一节同一个 TLS `server` 块**，与 `location /` 并列；它复用 `upstream gpuworks`。若推理网关使用独立监听地址，则另建 upstream 指向该监听，替换这里的 `proxy_pass`：

```nginx
location /v1/ {
    proxy_pass http://gpuworks;          # upstream 指向 server
    proxy_http_version 1.1;
    proxy_set_header Connection "";      # 与 upstream 复用长连接
    proxy_buffering off;                 # 否则 SSE 被攒批，TTFT 变成整段响应时间
    proxy_request_buffering off;         # 大请求体直接转发，不在 Nginx 再落一次盘
    client_max_body_size 256m;           # 与 --proxy-max-body-bytes 对齐（Nginx 默认 1m）
    proxy_read_timeout 3600s;            # 两次读之间的间隔；非流式长生成要等到整段完成
    proxy_send_timeout 3600s;
}
```

配置完成后，在 Nginx 主机执行 `sudo nginx -t`，确认成功再执行 `sudo nginx -s reload`。修改了 `server.yaml` 的可信代理或 TLS 配置时，还需按原服务方式重启 GPUWorks；随后完成[入口验证](#入口验证)。

server 自身对 HTTP 连接不设读写超时（agent WebSocket 与 SSE 都是长连接），长流只会被中间的
反代、负载均衡或客户端 SDK 的超时切断。流式请求每个 token 都会重置 `proxy_read_timeout`；
**非流式**请求在生成结束前没有任何字节，数万 token 的生成需要把超时设到大于最长生成时间，
更推荐长生成一律用 `stream: true`。

## 网关容量与主机调优

`/v1` 数据面全部经过 server 进程：鉴权、选副本、经 agent 隧道转发请求体与流式响应。
容量按下面三项估算（数据来自 [400 实例压测](../../artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/README.md)，
fake 引擎、同机回环；真实环境需重新测量）：

| 负载特征 | server CPU | 参考点 |
|---|---|---|
| 流式输出 | 约 35 µs / token（每个 SSE delta） | 4000 路并发、17 万 token/s 时 5.9 核 |
| 请求体上传 | 约 24 核 / (GB/s) | 2000 路并发、每请求 2.4 MB、约 380 MB/s 时 9.3 核 |
| 在途连接 | 约 0.2 MB 内存 / 流 | 4000 路长流时 RSS 约 850 MB |

例：400 个实例、每实例 16 路并发、每路 50 token/s，约 32 万 token/s，需要约 11 个核；
在此之上再按请求体带宽加核。网卡需要承载请求体的**两倍**流量（客户端进、转发到节点出）。

先按当前并发和观测结果决定是否调优，不必照抄全部参数。以下命令在 **Linux server 主机**执行，systemd unit 名称示例为 `gpuworks`；自定义名称请替换。

### 读取当前配置

服务须已启动。读取监听队列和服务进程的实际文件描述符限制，确认 `MainPID` 为非零值：

```bash
sysctl net.core.somaxconn
pid=$(systemctl show gpuworks --property MainPID --value)
cat /proc/$pid/limits
```

### 按负载选择修改项

**监听队列溢出**

突发建连压测出现监听队列溢出时，可将 `net.core.somaxconn` 调到 4096 或以上。当前值较小（例如 128）时，数千客户端同时建连会丢弃 SYN，1 秒后重传，表现为一批请求慢 1 秒；可用 `nstat TcpExtListenOverflows` 查看。

在 `/etc/sysctl.d/` 配置中设置 `net.core.somaxconn`，执行 `sudo sysctl --system`，再读取确认。server 在启动时读取监听队列长度，最后还需按下节重启并复测。

**文件描述符不足**

server 启动时会把软限制提到硬限制（Go 运行时行为）。硬限制至少为「并发客户端数 × 2 + 节点数 × 4」，再留余量；每个 `/v1` 请求占用一个客户端连接，大请求体还占用一个临时文件。

`gpuworks install` 生成的 unit 没有设置 `LimitNOFILE`，沿用 systemd 默认值。手工部署或默认值偏小时，执行 `sudo systemctl edit gpuworks` 设置，例如：

```ini
[Service]
LimitNOFILE=65536
```

`65536` 只是示例，按上述并发公式确定。

**请求体临时盘空间不足**

超过 1 MiB 或超出进程内 256 MiB 预算的请求体写入系统临时目录，请求结束后删除。目录由 Go 的 `os.TempDir()` 决定，对应 `TMPDIR` 环境变量，默认 `/tmp`。

同时在途的大请求会占用「并发数 × 请求体大小」的空间；XFS 对打开中的文件还有预分配，实测约放大到 2.5 倍。长上下文业务应把 `TMPDIR` 指向空间充足的数据盘，避免使用系统盘。

先准备服务账户可写、容量充足的目录，再通过 `sudo systemctl edit gpuworks` 设置。将示例路径替换为实际目录：

```ini
[Service]
Environment="TMPDIR=/srv/gpuworks-tmp"
```

### 使修改生效并复测

1. 完成所需修改后，执行 `sudo systemctl daemon-reload`，在维护窗口重启服务。
2. 重复上面的读取命令，取得新进程 PID 并检查 `sysctl` 与 `/proc/$pid/limits`，确认限制已生效；修改了临时目录时，确认目录可写。
3. 复测原来的负载，观察监听溢出、内存和磁盘占用，确认对应问题已改善。

API key：网关对校验通过的 key 在内存缓存 5 分钟。通过管理 API 吊销 key 立即失效；只有绕过
server 直接改数据库时才要等缓存过期。每个调用方使用独立的 key，便于吊销和按 `last_used`
（分钟粒度）排查。

`/v1` 默认要求 API key，生产环境保持这个默认。不带 key 的访问是管理员显式打开的例外：**设置 → 安全**里的
「接受不带令牌的 /v1 推理请求」（`inference_open_access`，默认关闭）。打开后任何能访问推理监听地址的人都能
匿名调用已部署模型、占用它们的 GPU，所以只在确实需要（例如完全受控的内网试用）时打开，并在打开的同一变更里
确认 `listen.gateway` 的暴露范围。不要在反代或防火墙层补一个「无 key 就放行」的规则来替代这个开关。规则与
401 形态见[OpenAI 兼容网关的认证](../reference/openai-gateway.md#认证)。

长请求与升级：server 重启或升级会断开全部在途的流，已经开始的响应不会重试；节点 agent
断线会断开该节点上的全部流。长生成业务的升级应安排在低峰，或先在调用方做好断流重试。
正在等休眠部署唤醒的请求在正常停止时立即得到 503 与 `Retry-After: 10`，进程被强杀时连接断开；
调用方对这两种情况重试，退避总长覆盖一次重启（通常 10 秒内），重发的请求会重新等唤醒。

网关指标（在监控中心与 `/api/v1/metrics/query` 可查）：`gpuworks_proxy_requests_total`、
`gpuworks_proxy_request_duration_ms_*`、`gpuworks_proxy_requests_inflight`，以及副本分布
`gpuworks_proxy_routed_total{deployment,instance}` 与重试 `gpuworks_proxy_retries_total`。

## 凭证与备份

保护实际 data 目录中的 `master.key` 与数据库，备份时保留二者；单独保存数据库不能恢复其中的加密凭据。敏感文件默认仅所有者可读写（`0600`）。明确绑定本安装二进制和非 root 服务账户的 Linux systemd 安装可使用可信服务组（`0660`）；该组成员等价于拥有平台凭据访问权。macOS 与独立运行默认不向普通用户组授权。启动、安装和升级会收紧存量凭据、数据库及其 sidecar、已识别备份；临时数据库快照仅所有者可读。无法保护文件时拒绝继续，按错误路径修复所有者与服务组后重试。生成口令不写入服务日志。

首次登录后按[安装篇的改密步骤](install-server.md#验证与首次登录)更换初始密码，验证新密码后删除 `<data>/initial-credentials`。推理 API key 按调用方分别创建，需要撤销时从管理接口或控制台吊销旧 key。吊销最后一个 key 不会让网关回到无鉴权状态：`/v1` 会拒绝所有请求，直到再创建一个 key，或管理员显式打开 `inference_open_access`。升级备份范围及恢复限制见[升级与版本兼容](upgrade-and-compatibility.md)。

## 运行与升级

使用独立 service 用户和持久化 data、模型盘；限制管理 API 与 `/dl/` 的网络访问。Linux systemd 安装的
`gpuworks install` 使用专用系统用户 `gpuworks`，并创建 `<dir>/data`、
日志与备份目录齐备。升级走 `gpuworks upgrade`（停服 → 备份数据库/master.key/旧二进制 → 原子
替换 → 探活，失败自动回滚到快照）；手工部署或需要自定义产物时，按[发布指南](../dev/release.md)
原子替换并保留回滚二进制。数据库迁移只有前滚、没有 down 迁移，所以升级前一定要有备份：`gpuworks upgrade` 会自动备份，手工替换要按升级指南备份数据库与 `master.key`；手工路径也没有自动探活与回滚。步骤和回滚方式见[升级与版本兼容](upgrade-and-compatibility.md)。

完整的安装、凭证、目录布局和回滚语义见[安装和升级控制面](install-server.md)。

## 入口验证

配置生效后，从实际使用入口的客户端检查：

1. 访问 HTTPS 控制台并登录，确认没有 Origin 拒绝或证书错误。
2. 在**计算节点**中确认原本在线的节点恢复在线；代理了节点入口时，确认长连接没有反复断开。
3. 配置了推理入口时，用已部署且支持流式输出的模型调用示例，经新 HTTPS `/v1` 入口发送一次 `stream: true` 请求，确认持续收到输出并正常结束。模型类型、模型名和 API key 使用该部署的实际值；不以页面能打开代替推理验证。

失败时先检查 server 与反代日志，分别核对可信代理地址、转发头、上游监听和超时设置；修正后重复对应检查。
