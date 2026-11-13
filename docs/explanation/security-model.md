# 安全模型与信任边界

## 控制面与数据面

服务端持久化会话保护管理 API：网页使用 HttpOnly Cookie，非浏览器客户端可使用绑定会话的 JWT。
鉴权检查会话有效性与当前用户权限；改密原子撤销全部管理会话，退出撤销当前凭证，空闲和绝对期限
由服务端强制执行。前端只报告真实交互并处理重新登录，不管理 refresh token；后台轮询不保活。

平台 API Token 继续按所属用户权限、令牌范围与独立有效期保护管理 API 和 MCP，不参与登录会话续期或改密撤销。

API key 保护 /v1 数据面，可限定只调用指定的模型服务（部署）：范围外的模型返回 403，`/v1/models` 也只列出范围内的模型，批处理请求按 key 当前的范围执行。默认每个 `/v1` 请求都必须带 key，库里没有 key 时也不放行；唯一的例外是管理员显式打开的安全开关 `inference_open_access`（默认关闭），打开后不带 key 的请求以匿名身份通过、带了 key 的仍照常校验，开关读不到时按关闭处理，所以一次失败的设置读取不会打开网关。per-node token 只证明某个 agent 的连接身份；
server_dial 通过 HMAC challenge 防止未持 token 的拨入。

preflight、peer sync 和模型/镜像流使用
短时一次性凭据，server 协调但不承载模型字节。

## 反代 TLS 与转发头

server 原生 HTTP，TLS 通常由反代终止。只有配置在 `server.yaml` 的 `trusted_proxies`
（或 `--trusted-proxies`）中的来源才可提供 X-Forwarded-For，用作节点接入连接的来源
地址（日志与节点详情里的“来源地址”）；否则按 socket 的对端地址判断，避免任意客户端伪造来源。

server 不再存储或推断自己的对外
地址（见 [ADR 0025](../adr/0025-server-holds-no-external-address.md)），`trusted_proxies`
因此也不影响任何地址生成。生产应把 /install.sh、/dl/ 和管理 API 放在适当的访问控制后。

## 凭证可见性

master secret 解锁 AES-GCM 加密的 provider/AI key 与平台 SSH 私钥（server 登录节点所用，
节点授权其公钥即可，库里不再保存节点密码，见 [ADR 0035](../adr/0035-platform-ssh-key.md)）；能读取 server data 目录或 server 进程内存的
管理员仍处于信任边界内。

agent token 必须传给 agent 进程，它记在 state 目录的 `gpuworks-agent.conf`（权限 0600）里，不在 agent 的命令行上；但同一节点上的 root 或具备进程
内存、环境查看权限的用户仍可能拿到它；节点隔离和最小权限不能由平台单独替代。

## 试验场 HTML 预览

试验场里 HTML 代码块的预览运行模型输出，按不可信代码对待：iframe 是不透明源 sandbox（不加
`allow-same-origin`），拿不到控制台的会话；默认还用 CSP 禁止它从查看者浏览器发起网络请求，
管理员可在「设置 → 安全」放开网络，但 sandbox 不变。残留风险（自我导航、WebRTC、经图片地址
外泄内容）见 [ADR 0037](../adr/0037-playground-html-preview-isolation.md)。

## 终端与实例

web terminal 受并发、空闲和 kill switch 策略，并写审计；实例 contract 限定其公开的 OpenAI
路径。睡眠实例返回 503 而不是把请求发送到过期端口。日志和外部命令做脱敏，但发布验收仍需
记录 digest、命令和环境摘要，以便复现而不泄露 secret。

