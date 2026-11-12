# 安全默认值清单

## 管理会话与 API 令牌

管理 API 使用持久化会话：浏览器保存 HttpOnly 不透明 Cookie，Bearer JWT 绑定同一会话。每次鉴权都会查询会话和当前用户权限，数据库只保存随机凭证的 SHA256。

会话失效后的处理范围如下：

- 修改密码与撤销全部管理会话在同一事务中完成；退出登录时，撤销当前所提交凭证对应的会话。
- 升级前未绑定会话的凭证需要重新登录。
- SSE 在转发消息前复查会话。SSE 和终端还通过撤销事件及每秒检查关闭失效连接；终端数据帧不逐帧查库。
- 独立进程执行 `reset-password` 后，也会在下次检查时关闭连接。
- 已完成鉴权并执行的请求不追溯回滚。

各入口接受的凭证及其用途如下：

- `/v1` 默认要求独立 API key：不带 key 或 key 无效、已吊销都返回 401，库里没有任何 key 时也一样。无 key 访问只能由管理员显式打开安全设置里的 `inference_open_access`（默认关闭）；打开后不带 key 的请求被匿名接受，带了 key 的请求仍照常校验，设置读取失败按关闭处理。详见[OpenAI 兼容网关](openai-gateway.md#认证)。
- per-node token（每个节点独立的令牌）只用于该节点连接和 bootstrap（初始接入），不在公共 `/install.sh` 中注入。
- 管理 API 与 MCP 也接受平台 API 令牌（`gwp_`）：只存 SHA-256，按所属用户的当前角色执行，再叠加令牌范围（默认只读）。可设有效期、可吊销；任何令牌都不能管理令牌。MCP 默认关闭，由管理员开启。

## 管理员密码

新建或修改管理员密码（安装、首次启动、控制台修改密码、`reset-password`）都使用同一套规则：

- 8–128 个 Unicode 字符，最多 512 字节；不要求大小写、数字或符号的组合，可用中文或由几个词组成的短语。
- 拒绝常见弱口令（含前后加数字或符号的变形，如 `Password123`）、整段重复和连续字符（如 `12345678`、`qwertyui`）。这份名单很短，不是已泄露口令库。
- 不能与用户名相同（忽略大小写、标点与首尾数字）。
- 不能包含控制字符。已有密码不受新规则影响，照常登录。

修改与找回：

- 能登录时，在控制台右上角账户菜单选择「修改密码」，只能改自己的密码，需输入当前密码；当前密码的校验与登录共用限流。
- 修改后该用户的全部管理会话失效，平台 API 令牌和 API key 不受影响；`initial-credentials` 记录的是该用户时，server 尝试删除它（`reset-password` 同样处理）。
- 忘记密码时由管理员在服务器上执行 `gpuworks reset-password`，不带 `--password` 时在终端里不回显地输入两次，避免密码进入命令历史。
- 功能体验模式的控制台不能修改密码，其密码由主控制台「设置 › 通用 › 功能体验模式」管理。

## 凭证加密与安装文件

AI 服务/云厂商凭证（AI/provider）使用 server 的 AES-GCM secretbox 加密；主密钥（master secret）只从 data 目录的 `master.key`（首次启动生成）读取。

### agent 安装

- install script 下载 agent 到临时文件后原子替换；systemd unit 和 state 目录按最小权限创建，`identity.json` 只在卸载时删除。
- agent 的启动配置（含节点 token）只记在 `<state>/gpuworks-agent.conf`，权限 0600，经 `gpuworks-agent configure` 从标准输入写入。
- systemd 单元、nohup 的 `agent-restart.sh`、launchd plist 里都只有 `run --state <目录>`，token 不出现在 `ps`、`systemctl cat` 或 plist 里。卸载时该配置文件随 agent 一起删除。
- 离线包 `/dl/gpuworks-agent.run` 是公开下载（与其他 `/dl/` 文件一样），不含 token；token 只在运行它的命令行里给出。

### 控制面安装与升级

- Linux systemd 模式下，`gpuworks install` 默认以专用系统用户 `gpuworks` 运行单元；只有指定 `--service-user root` 才显式退回 root。
- 首启密码只经 `<data>/initial-credentials`（默认 `0600`，可信 Linux 平台组为 `0660`）传递。单元文件是全局可读的，因此绝不写入密码。
- `gpuworks upgrade` 先校验发布包的 sha256 再原子替换；探活失败时回滚到升级前快照。
- server 不存储也不推断自己的对外地址。节点安装命令里的地址由调用方（浏览器、API 调用方、`--server`）显式给出，见 [ADR 0025](../adr/0025-server-holds-no-external-address.md)。
- 安装与升级都会写审计到 `<data>/install-history.jsonl`（默认 `0600`；可信 Linux 平台组共享安装为 `0660`）。

## udocker/runveil 的目录权限

root agent 以专用用户（`udocker`）运行 udocker/runveil 时，state 目录里既有该用户要执行的 patched PRoot 与 runtime-mounts，也有 agent 私有的 identity 与日志。

agent 每次使用 runtime 前会检查专用用户能否“穿越”（x）state 目录，即能否经过该目录访问有权限的文件。**已经可穿越时保持原样**，不再进入下面的权限补授检查。

仅在尚不可穿越、需要补权限时，agent 才检查 state 目录的归属与可写权限：目录不属于 agent，或可被组或他人写入时，只报错、不修改。检查通过后，只给 state 目录本身补穿越权限，不授予列目录或写入权限：

| state 目录的组权限情况 | agent 的处理 |
|---|---|
| 组没有任何权限，或已是专用用户主组 | 改属专用用户主组并加 g+x；agent 自建的目录从 0700 变为 0710 |
| 组已有自己的权限，且不是专用用户主组 | 不动组、只加 o+x；0750 变为 0751 |

上层目录通常属于运维，只检查不修改。AutoDL 容器有一个例外：root agent 为非 root 的 runtime 用户准备路径时，若配置路径或符号链接解析后的路径位于 `/root` 下，且 `/root` 属于 root、不能被组或其他用户写入，agent 只给 `/root` 补所需的穿越权限（g+x 或 o+x）；不授予列目录或写入权限，也不修改其他上层目录。其余不可穿越的上层目录仍只记录 warning：镜像管理与 runveil 排障命令照常可用，启动 workload 会失败；准备 runtime-mounts 时则报错。

私有文件保持自身的 0600。

## 网络连接与终端访问

- 只有显式配置的 IP/CIDR 才信任 `X-Forwarded-For`，用于记录 agent 接入连接的来源地址；TLS 通常由受信任反代终止。
- server_dial 使用节点 token 派生的 HMAC challenge（认证挑战）；临时 preflight/peer 端口使用一次性 token 与 TTL（有效期），完成后关闭。
- 服务网络转发在节点有服务网络时启用，agent 在服务 IP 的 `19443` 端口监听：
  - 使用 TLS 1.3，密钥由节点 token 经 HMAC 派生；网关按公钥固定校验，不依赖 CA。
  - 每个连接必须先出示 `HMAC(节点 token, 实例 UUID)`，只能转到本节点正在运行的实例的回环端口。
  - 令牌不落库、不在界面显示；推理引擎端口仍只发布在 `127.0.0.1`。
  - 重新生成节点 token 会同时轮换 TLS 密钥和全部实例令牌。
- 网关的 Playground 入口对 API contract（接口契约）外的路径返回 404；公网 `/v1` 不做这项检查，由引擎处理，见 [OpenAI 兼容网关](openai-gateway.md#支持的操作)。睡眠/不可用实例返回 503；日志会脱敏外部命令和凭证。
- web terminal（Web 终端）受 kill switch（总开关）、并发上限、空闲超时和审计策略约束。

安全模型中的取舍（反代 TLS 依赖、token 在目标进程可见性、信任边界）见[安全模型说明](../explanation/security-model.md)。生产部署还应备份数据库和 master key，并轮换 admin 密码/API key。
