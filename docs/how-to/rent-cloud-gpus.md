# 租用云 GPU

把阿里云、RunPod、优云智算 CompShare、AutoDL 或 Vast.ai 的 GPU 实例接入 GPUWorks，并在用完后停止或释放云资源。先在厂商控制台租机，再在本平台同步和安装 agent；支持的 REST API 创建方式也在本文列出。

**前置条件**：

- server 已运行（见[安装和升级控制面](install-server.md)），使用有云账号和节点管理权限的账户登录控制台，server 能访问所选厂商 API。
- 有对应云厂商的账号和 API 凭据（凭据字段见下表），账户里有可用余额，并已在云厂商一侧有一台 GPU 实例（或打算用下文的 API 创建）。
- 安装时实例须已运行。Agent 发起连接时，实例要能访问 `server_url`；Server 发起连接时，server 要能访问实例的 agent TCP 端口。SSH 或云助手的安装条件见[安装 agent](#安装-agent让实例出现在计算节点)。
- Agent 发起连接且 server 在 NAT 后面时，提供实例可达的稳定域名或反代地址，不用 quick 隧道地址（见[公网入口](expose-public-address.md)）。

GPUWORKS 对云实例做的事是：保存云账号凭据（加密存储）→ 同步实例清单 → 经 SSH（阿里云优先经云助手）在实例上安装
agent → 实例作为“计算节点”上线。**控制台里的“外部平台”页不提供“选机型、选区域、下单”的界面**：
租机（选机型、区域、镜像）请先在云厂商自己的控制台完成，再回到 GPUWORKS 同步并接入。通过 REST API
创建/删除实例的方式见[用 API 创建和释放实例](#用-api-创建和释放实例)。

## 添加云账号

1. 左侧菜单“算力资源”分组下打开“外部平台”页（路径 `/providers`）。
2. 点“添加外部平台”，或点页面中部“快速接入”里的某个厂商卡片（会预选该类型）。卡片下方是厂商官网链接。
3. 在“外部平台”字段选择厂商，填写“名称”（自定义，必填）和“认证信息”里的字段，点“添加”。

各类型的凭据字段（以界面显示为准，带“可选”的可以不填）：

| 类型 | 必填 | 可选 |
|---|---|---|
| 阿里云 | AccessKey ID、AccessKey Secret | RAM 角色 ARN、地域（见[阿里云](#阿里云)） |
| DigitalOcean | API Key（Personal Access Token） | API 接口地址（见 [DigitalOcean](#digitalocean)） |
| RunPod | API Key | API 接口地址 |
| 优云智算（CompShare） | 公钥、私钥（优云智算 API 密钥对） | API 接口地址 |
| AutoDL | API Key | API 接口地址、SSH 密码（之后装 agent 时作为默认 SSH 密码） |
| Vast.ai | API Key | API 接口地址 |

当前 AutoDL 接口只同步**容器实例Pro**，一键安装只提供**Agent 发起连接**。不要把这一产品限制当作所有 AutoDL 实例的端口能力；需手工配置端口时，以[厂商开放端口说明](https://www.autodl.com/docs/port/)和实例实际配置为准。

RunPod 只支持 Secure Cloud 的 Pod，不支持 Community Cloud（添加账号的表单里也有这条提示）。

“API 接口地址”留空时使用各厂商默认地址，只有走代理或私有网关时才需要改。

保存时 server 会用这些凭据调用云厂商接口做一次校验：通过才保存，状态显示“有效”；不通过则提示
“添加失败：…”，并带上云厂商返回的原因。保存后凭据加密存储，界面不再回显。之后可在列表里点“编辑”
更新凭据：**没填的字段保留原值**，类型不能改。

## 同步实例并确认机型、区域和价格

1. 在“已接入的外部平台”列表里点某个账号的“同步”（或页面上方“同步全部”一次同步所有外部平台）。列表的“外部平台”列显示厂商（如阿里云、RunPod），“平台类型”列为公有云。完成后提示“同步完成，发现 N 个实例”。
2. 点开该账号所在行左侧的展开箭头，看实例表：

   | 列 | 含义 |
   |---|---|
   | 实例名称 / 实例 ID | 云厂商侧的名称与 ID |
   | GPU 型号 / GPU 卡数 | 云厂商返回的规格；拿不到时显示“未返回” |
   | 连接方式 | 云厂商提供的入口：“直连”或“外部平台代理”（SSH，可复制 `ssh` 命令；标注“仅终端”的入口只能连接，不能用来装 agent），阿里云还有“云助手”（无需 SSH） |
   | 状态 | 运行中 / 已停止 / 未知 |
   | 费用 | 每小时价格（如 `¥1.98/时`）；云厂商没返回价格时可能显示 `¥0.00/时`，不代表免费 |
   | 区域、类型 | 区域；类型为“容器”或“虚拟机” |

3. 账号状态会随同步结果变化：同步失败会把账号标为“异常”，页面上方出现“存在异常的外部平台”提示，按提示检查凭据和云厂商 API 可用性，再重新同步。

列表每 15 秒自动刷新一次账号状态，但实例清单来自上一次“同步”的结果，想看云端最新情况请重新点“同步”。容器实例通常没有可用的 Docker 守护进程，agent 在无法使用 Docker 时使用内置 runveil；即使容器内是 root，也不等于能运行宿主机 Docker。

## 安装 agent，让实例出现在“计算节点”

先确认实例已运行，并按连接方向准备地址：

| 接入方式 | 地址与条件 |
|---|---|
| **Agent 发起连接**（默认） | **管理节点接入地址**由浏览器地址预填，须改为实例可达的稳定地址；实例不需要 agent 入站端口 |
| **Server 发起连接** | 填 **Agent 监听地址**（通常默认 `0.0.0.0:18800`，CompShare 默认 `:3389`；端口映射实例可留空自动选择，见下文）和 **计算节点接入地址**；后者使用 server 可达的 IP、域名或映射后的 `host:port`；只填主机时使用 Agent 监听端口。安全组和防火墙须放行相应 TCP 端口。此方式不填写 `server_url` |

`server_url` 必须是无用户信息、查询和片段的 `http://` 或 `https://` 绝对地址。quick 隧道地址会显示临时地址警告，不能作为长期节点地址。

1. 在实例表找到尚未接入的实例，点击**一键装 Agent**。已接入的实例显示**已接入**。
2. 在弹窗选择**连接方式**和**接入方式**，核对上表地址。**自动选择**会按厂商能力尝试安装通道：阿里云的 Agent 发起连接优先云助手，其余优先 SSH 直连，失败时尝试后续可用入口。也可指定入口；标为**仅终端**的入口不能安装。
3. 在**高级**中核对安装目录和**通过 systemd 启动**，再核对下面的凭据、端口和重启条件，点击**安装**。无需另行先创建计算节点或取得 Token。
4. 等待安装结果，展开失败或警告项。出现**agent 已安装并已连接**时点击**完成**；**已安装，有需要留意的项**也要先检查报告。出现**已启动，尚未连上**时，agent 已安装，先修复连接条件，不要重复安装。
5. 打开**计算节点**，确认该实例对应的节点在线，并核对加速卡型号、数量和运行时。失败时保留报告中的原因，修正后从弹窗**重试**；尚未成功安装的待接入节点会清理。

安装目录留空时为 `/opt/gpuworks`；自定义目录必须以 `/` 开头且不能为根目录，不展开 `~`。填写的目录不可写时安装失败，不会回退到 `/tmp/gpuworks`。

**通过 systemd 启动**默认开启：Linux 实例有 systemd 且能以 root 运行时注册 `gpuworks-agent.service`。关闭后用 nohup，开机不自启，保留主机已有服务和其他目录里的 agent。容器实例通常没有 systemd，开关均走 nohup，详见[启动方式](connect-nodes.md#启动方式与后续重启)。

### 凭据与可能的重启确认

SSH 安装默认使用平台密钥，通常不用手填密码。各厂商的准备方式不同：

| 厂商或场景 | 安装前需要确认什么 |
|---|---|
| 阿里云云助手 | 不需要 SSH；实例须能下载 server 的安装脚本和 agent。Server 发起连接时自动选择改走 SSH，细节见[阿里云](#阿里云) |
| RunPod | 平台公钥追加到账号 SSH Key，保留已有 key；Pod 只在启动时写入账号公钥，因此已运行的 Pod 不会自动获得新 key。已运行 Pod 没有该公钥、且现有凭据不能登录时，安装会请求重启确认；也可在**高级设置**提供一次可用私钥，避免通过修改 Pod 配置加公钥 |
| Vast.ai | 平台通过厂商接口向实例授权公钥 |
| CompShare、AutoDL | 尝试用接口返回的实例 root 密码登录并写入平台公钥；AutoDL 账号保存的 SSH 密码作为后备 |
| 自动认证失败 | 在**高级设置**提供该实例可用的密码或私钥，或按错误提示授权平台公钥，再重试 |

平台公钥写入成功后，后续恢复使用平台密钥；写入失败时可能保留本次有效凭据，实际结果见 `SSH authentication`、`Platform key` 等报告项。预检查提示安装时才会添加公钥时，尚未登录的其他检查会在安装阶段继续。

**RunPod 的公钥或端口变更可能触发重启确认。** 出现 `INSTANCE_RESTART_REQUIRED` 时尚未修改 Pod；确认后才修改配置、重启并继续安装。**重启会清空非持久卷，端口映射（包括 SSH）也可能重新编号**。先保存需保留的数据；不接受重启时取消，改用可登录凭据或其他接入方式。报告中的 `Platform key (pod)` 和 `Server-dial port` 可用于核对处理结果。

### Server 发起连接的厂商差异

- **阿里云**：走 SSH 上传 agent，不要求实例下载 server 的文件。预检查读取安全组，缺少入站放行规则时报告失败，部分来源放行时提示核对 server 出口地址；不自动修改安全组。agent 已启动但 server 尚连不上时，放行后等待自动连接，无需重装。
- **CompShare**：容器实例（Pod）只经平台转发的 TCP 端口到达，入口是 `…podtcp.compshare.cn` 上的另一个端口号（SSH 也是这样转发的，容器内是 23 端口）。两个地址栏都留空时，驱动先在实例已开放、没有进程监听的 TCP 端口里选（优先「Agent 监听地址」的端口，默认 3389）；一个都没有时经 `UpdateCompShareInstancePorts` 接口给实例加开一个（保留已开放的 HTTP/TCP 端口，立即生效、不重启容器），把监听端口和转发后的地址记到节点上，检查表里多一行 `Server-dial port`。平台每个实例最多开放 10 个 TCP 端口（含 SSH），满了而且都被占用时安装失败，需要在控制台「配置开放端口」里关掉一个，或改用“Agent 发起连接”。虚拟机实例有公网 IP，server 直接拨它的监听端口（默认 `:3389`），安全组要放行。
- **AutoDL**：一键安装不提供此方向。确有可用 TCP 入口时，可按厂商文档配置后，通过[添加计算节点](connect-nodes.md)手工接入。
- **Vast.ai、RunPod**：两栏都留空时，安装会从实例已映射、没有进程监听且非 SSH 的 TCP 端口中选择，记录实际监听端口和公网映射地址。预检查仅提示安装时检查，不会误测默认端口。
- **Vast.ai 没有空闲映射端口**：改用 Agent 发起连接，或选择提供备用映射端口的实例模板。不要假定任意容器端口都能从外部访问。
- **RunPod 没有空闲映射端口**：请求上述重启确认；同意后添加 TCP 端口，等待新映射和 SSH 可用后再安装（最长等 5 分钟）。等待失败时查看报告和 Pod 实际状态，再重试。

云一键安装是[接入节点](connect-nodes.md)的方式之一；同样的实例也可以用 SSH 向导或 `/install.sh` 手工装。

租来的节点自带系统标签 `gpuworks.ai/provider`（云厂商）和 `gpuworks.ai/region`（区域），可以用它们建[同步组](configure-pull-settings.md#同步组)：不同厂商或区域的云主机通常互不相通，不该互相复制模型和镜像。

## 停止和释放实例，避免继续计费

在 GPUWORKS 里能做的：

- **关机**：在实例行点“关机”并确认。只有支持电源控制的类型才有这个按钮（阿里云、优云智算、AutoDL）；“开机”同理。点了之后提示“已发送关机请求”，状态以随后的同步结果为准。阿里云使用继续计费的**普通停机**，关机不等于释放。
- **释放（销毁）**：界面上没有释放实例的按钮。RunPod、Vast.ai 不支持电源控制，也就没有关机按钮，需在厂商控制台管理这些实例的停用或销毁，或使用受支持的 REST API 释放；费用是否终止以账单为准。
- **删除云账号**（列表里的“删除”）只会删除 GPUWORKS 里的账号、凭据和实例记录，**不会**关闭或销毁云上的实例，也不会删除已接入的节点。
- 用 API 释放成功后，该实例立即从实例列表里消失；释放失败时列表不变。在云厂商控制台销毁或到期回收的实例，下一次同步成功时从列表里移除；同步失败（账号标为“异常”）不改动列表。
- 实例上已接入的节点不会随之删除：节点保留，带外恢复阶段直接标为 `stopped`（原因 `provider instance no longer exists`），不再尝试 SSH 恢复，见[排障](troubleshooting.md)；确认不再需要后手动删除节点。

计费规则（关机后是否继续收取磁盘、带宽等费用）由云厂商决定，以各家控制台的账单为准。**用完的实例请到云厂商控制台确认已销毁，并定期核对账单**。

## 用 API 创建和释放实例

如果想不进云厂商控制台，直接由 GPUWORKS 下单/释放，用 REST API（使用有相应权限的 JWT 或读写平台 API 令牌，字段见 [API 参考](../reference/api.md)）：

- `POST /api/v1/providers/{id}/instances`：创建，请求体为 `name`、`region`、`gpu_model`、`gpu_count`、`image` 和字符串键值的 `extra`，成功返回 201 和实例信息。
- `DELETE /api/v1/providers/{id}/instances/{externalID}`：释放。
- `POST /api/v1/providers/{id}/instances/{externalID}/power`：请求体 `{"action":"on"}` 或 `{"action":"off"}`。

创建之后，到“外部平台”页点“同步”，实例才会出现在列表里。各类型的要求不同：

| 类型 | 创建时的要求 |
|---|---|
| RunPod | `gpu_model` 必填（RunPod 的 GPU 型号 ID）；`gpu_count` 默认 1；`image` 默认 `runpod/pytorch:...`；`extra` 可带 `disk_gb`（默认 20）、`volume_gb`、`ports` 等 |
| 优云智算 | `gpu_model` 和 `image`（优云智算镜像 ID）必填；`gpu_count` 默认 1；`region` 缺省用厂商默认区域；`extra` 可带 `zone`、`disk_gb`（默认 100）、`cpu`、`memory_mb`、`password` 等 |
| AutoDL | `image`（镜像 UUID）必填；GPU 型号需能映射到已知规格，或在 `extra.gpu_spec_uuid` 里给出；`gpu_count` 1–4；`region` 为数据中心列表 |
| Vast.ai | `extra.offer_id` 必填（先在 Vast.ai 上搜索并选定一个报价，价格由该报价决定）；`image` 有默认值；`extra.disk_gb` 默认 20 |
| DigitalOcean | `region` 和 `extra.size`（GPU 规格 slug，如 `gpu-h100x1-80gb`）必填；`image` 缺省按规格选 DigitalOcean 的 AI/ML Ready 镜像（NVIDIA 单卡 `gpu-h100x1-base`、多卡 `gpu-h100x8-base`、AMD `gpu-amd-base`）；`extra.ssh_keys` 可带逗号分隔的账号 SSH Key ID 或指纹，平台公钥总会带上 |

这些要求来自各 provider 的实现，以接口返回的错误信息为准；创建前先确认余额和库存。释放：RunPod、Vast.ai、DigitalOcean 直接销毁；优云智算、AutoDL 会先关机再释放。`resize` 接口目前都返回 501（不支持）。

## 常见问题

**添加账号时报“添加失败：credential validation failed …”**
凭据没通过云厂商校验：检查 API Key / 公钥私钥是否复制完整（前后有空格或换行）、是否属于对应平台、账号是否被限制，以及 server 主机能否访问云厂商 API（“API 接口地址”是否需要走代理）。编辑已有账号时，没填的字段保留旧值，换了 Key 就要把新 Key 填进去。

**列表里账号显示“异常”，或点“同步”报“同步失败”**
同步会真实调用云厂商接口，失败即标“异常”。先按失败信息排查凭据是否被吊销、账户是否欠费，再点“同步”；成功后状态自动回到“有效”。

**实例表是空的（“未同步到实例，请点击上方‘同步’后重试”）**
还没同步，或云厂商账号下确实没有实例。先点“同步”。

**点“一键装 Agent”失败**

先看弹窗顶部的安装结论：

- 绿色**agent 已安装并已连接**：安装成功。
- 黄色**已安装，有需要留意的项**或**已启动，尚未连上**：继续检查报告中的告警，后者还需修复连接条件。
- 红色**安装失败**：结论后会带失败原因，按对应检查项处理。

装好后只剩**完成**按钮，关闭弹窗时实例列表会刷新。失败时表单保留，点击**重试**前可以改连接方式或在**高级**里填凭据。

**预检查或 SSH 入口失败**

`precheck` 一行只列没通过的项；全部通过时显示 `all N checks passed`。完整清单在**预检查明细**中展开。

SSH 入口不可达、用户名／密码／私钥不对，或选了**仅终端**入口时，检查凭据或换一个**SSH 连接方式**。**自动选择**会在直连失败时回退，失败详情会以告警列出每个入口的原因。

**安装目录相关失败或警告**

云一键安装默认使用 `/opt/gpuworks`，可在安装弹窗**高级**中修改；预检查与安装都会检查它背后的文件系统；七项检查的含义见[安装目录检查](connect-nodes.md#安装目录检查)。按报告中的项目处理：

- **Install directory persistence 警告**：容器实例的默认目录可能位于容器可写层，实例删除或重建后 agent 与缓存会丢失，需要重新安装。租机时给容器盘预留能容纳镜像的容量。
- **ownership / filesystem 警告**：`/workspace` 这类持久卷往往是不能改属主的网络文件系统，需结合目录权限和文件系统限制判断是否可用。
- **file names 失败**：RunPod 的 `/workspace` 不允许文件名带冒号，runveil 在那里跑不了部署，agent 需安装在容器盘上；不能仅因目录持久化就选它。
- **noexec 导致失败**：该目录不能执行程序，安装会直接失败。
- **Install directory path access 失败**：安装目录位于 `/root` 或私有主目录等专用运行用户 `udocker` 进不去的上级目录之下，且探测确定 agent 以 root 运行、会用 runveil（实例上没有可用的 docker 或 nerdctl）和专用用户，之后每次启动都会 `permission denied`，因此安装在上传 agent 前停止（400 `NODE_INSTALL_DIR_UNTRAVERSABLE`），报错给出被挡住的目录；执行 `chmod o+x <目录>` 或改用其他安装目录后重试。实例上有 docker 或 nerdctl 等无法确定会用 runveil 的情况只显示警告，安装继续。

需要把 agent 安装到兼容的持久卷或数据盘时，在安装弹窗**高级**里填写安装目录。显式填写的目录不可写时安装失败，不会回退；先处理目录问题，再重试。

**其他安装检查项**

- **SSH authentication 失败且只试了平台密钥**：实例还没有平台公钥。RunPod 上在 server 注册公钥前已启动的 Pod，从**外部平台**页安装时会询问是否重启以加入公钥。也可在**高级设置**提供一次私钥或密码，或按错误提示在实例上授权。
- **云厂商未返回 SSH 入口**：实例可能仍在启动，稍后重新点击**同步**。
- **agent_dial 警告**：agent 已启动但还没连上 server，通常是 `server_url` 对实例不可达，按下一条处理。

**安装成功，但“计算节点”页没有出现节点，或一直离线**
Agent 发起连接时，检查 `server_url` 是否对实例可达，并查看 agent 日志；地址错误时沿用该节点的身份修正连接配置。Server 发起连接时，检查 server 到实例接入地址的 TCP 连通性、端口映射及安全组，不要填写 `server_url`。接入后的被拒、掉线排查见[排障](troubleshooting.md)。

**误留了实例，产生了费用**
GPUWORKS 不会自动释放云实例，删除云账号也不会。到云厂商控制台销毁不用的实例，必要时用上文的 API 释放；销毁后在 GPUWORKS 里到“计算节点”页删除对应的离线节点。

## 相关文档

- [接入、升级和卸载节点](connect-nodes.md)
- [给控制台一个公网地址](expose-public-address.md)
- [排障](troubleshooting.md)
- [测试指南（云端 L4 用例与清理）](../dev/testing.md)
## 阿里云

**凭据**：填 AccessKey ID 和 AccessKey Secret，主账号或 RAM 子账号的 AccessKey 都可以。主账号 AccessKey 拥有账号的全部权限，**建议新建一个只有下面权限的 RAM 子账号**，用它的 AccessKey。

“地域”留空时同步账号所有地域；只在部分地域有实例时填逗号分隔的地域 ID（如 `cn-shanghai,cn-beijing`）可以减少调用。

**需要的权限**如下。下方 JSON 和 CLI 示例包含基础同步、安装及开关机权限；使用 Server 发起连接的安全组检查时，再加入 `ecs:DescribeSecurityGroupAttribute`。

| 权限 | 用途 |
|---|---|
| `ecs:DescribeRegions` | 校验凭据、枚举地域 |
| `ecs:DescribeInstances` | 同步实例；开关机前定位实例所在地域 |
| `ecs:DescribeCloudAssistantStatus` | 判断实例的云助手是否在线 |
| `ecs:RunCommand`、`ecs:DescribeInvocationResults` | 经云助手执行安装前检查和安装，并读取结果 |
| `ecs:StartInstance`、`ecs:StopInstance` | 开机、关机；不需要可以去掉 |
| `ecs:DescribeSecurityGroupAttribute` | Server 发起连接时读取入站安全组规则；不使用此检查时可不授予 |
| `cs:DescribeClustersV1`、`cs:DescribeClusterNodePools`、`ecs:DescribeInstanceTypes` | 「Kubernetes 集群」页签列出 ACK 集群及其 GPU 节点池；不接入 ACK 可以去掉 |
| `cs:DescribeClusterUserKubeconfig` | 由 GPUWORKS 把接入 manifest 应用到 ACK 集群时签发 15 分钟有效的 kubeconfig；只用 kubectl 接入可以去掉 |

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecs:DescribeRegions",
        "ecs:DescribeInstances",
        "ecs:DescribeCloudAssistantStatus",
        "ecs:RunCommand",
        "ecs:DescribeInvocationResults",
        "ecs:StartInstance",
        "ecs:StopInstance",
        "ecs:DescribeInstanceTypes",
        "cs:DescribeClustersV1",
        "cs:DescribeClusterNodePools",
        "cs:DescribeClusterUserKubeconfig"
      ],
      "Resource": "*"
    }
  ]
}
```

GPUWORKS 不在阿里云上创建或释放实例，不需要这两类权限。

**在控制台创建子账号**（界面步骤参照[阿里云 RAM 用户文档](https://help.aliyun.com/zh/ram/user-guide/create-a-ram-user)）：

1. 打开 [RAM 访问控制](https://ram.console.aliyun.com/)，「权限管理 → 权限策略 → 创建权限策略」，切到「脚本编辑」，粘贴上面的 JSON，命名为 `gpuworks-ecs` 保存。
2. 「身份管理 → 用户 → 创建用户」，填登录名（如 `gpuworks`），访问方式勾选「使用永久 AccessKey 访问」，按页面提示确认创建 AccessKey，确定后**立即保存** AccessKey ID 和 Secret（Secret 只显示这一次）。
3. 在用户列表里给这个用户「添加权限」，选「自定义策略」里的 `gpuworks-ecs`。
4. 回到 GPUWORKS，用这对 AccessKey 添加阿里云账号。

**用命令行创建**（[阿里云 CLI](https://help.aliyun.com/zh/cli/)，用有 RAM 管理权限的身份执行）：

```bash
POLICY='{"Version":"1","Statement":[{"Effect":"Allow","Action":["ecs:DescribeRegions","ecs:DescribeInstances","ecs:DescribeCloudAssistantStatus","ecs:RunCommand","ecs:DescribeInvocationResults","ecs:StartInstance","ecs:StopInstance","ecs:DescribeInstanceTypes","cs:DescribeClustersV1","cs:DescribeClusterNodePools","cs:DescribeClusterUserKubeconfig"],"Resource":"*"}]}'
aliyun ram CreatePolicy --PolicyName gpuworks-ecs --PolicyDocument "$POLICY"
aliyun ram CreateUser --UserName gpuworks
aliyun ram AttachPolicyToUser --PolicyType Custom --PolicyName gpuworks-ecs --UserName gpuworks
aliyun ram CreateAccessKey --UserName gpuworks   # 输出里的 AccessKeyId / AccessKeySecret 就是要填的凭据
```

**可选：RAM 角色**。企业把权限收在角色里、或跨账号授权时，权限策略授给角色而不是用户，用户只保留扮演角色的权限，再在“RAM 角色 ARN”里填角色 ARN（`acs:ram::<账号 ID>:role/<角色名>`）。server 用用户的 AccessKey 调 STS `AssumeRole` 换取一小时的临时凭据，之后的调用都用临时凭据签名，快到期时自动续：

```bash
ACCOUNT=$(aliyun sts GetCallerIdentity | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccountId"])')
TRUST="{\"Statement\":[{\"Action\":\"sts:AssumeRole\",\"Effect\":\"Allow\",\"Principal\":{\"RAM\":[\"acs:ram::$ACCOUNT:root\"]}}],\"Version\":\"1\"}"
aliyun ram CreateRole --RoleName gpuworks --AssumeRolePolicyDocument "$TRUST"
aliyun ram AttachPolicyToRole --PolicyType Custom --PolicyName gpuworks-ecs --RoleName gpuworks
aliyun ram AttachPolicyToUser --PolicyType System --PolicyName AliyunSTSAssumeRoleAccess --UserName gpuworks
```

这时用户本身不需要 `gpuworks-ecs`（可以 `DetachPolicyFromUser` 去掉），ECS 权限全部来自角色。

**同步**：实例表只列出账号在各地域带 GPU 的 ECS 实例（不带 GPU 的实例不显示），GPU 型号与卡数来自实例规格。运行中的实例同时查询云助手是否在线：在线的实例连接方式里出现“云助手”；有公网 IP（含 EIP）的实例还有“直连”（`root@<公网 IP>:22`）。Windows 实例不能安装 agent。

**安装通道**

使用**自动选择**时，Agent 发起连接优先经云助手安装；Server 发起连接（`server_dial`）或云助手不在线时使用 SSH。

**经云助手安装**

这条路径不需要 SSH、密码、平台密钥或公网 IP，但**实例必须能访问 `server_url`**。没有公网出口时，使用同一 VPC 或已打通网络中可达的 server 地址，见[节点与 server 网络不通](connect-nodes.md#节点与-server-网络不通)。

server 调用 `RunCommand`，让实例以 root 执行与**添加节点**相同的一行安装命令（`curl <server_url>/install.sh | sh -s -- …`），由实例下载 agent。安装前检查同样经云助手执行，并从 server 一侧检查能否反向连到实例。

**经 SSH 安装时的认证回退**

平台会尝试通过云助手把平台公钥追加到实例 root 的 `~/.ssh/authorized_keys`，已有公钥不重复添加，再用平台密钥登录：

- **云助手在线**：可以直接添加公钥，不需要手填凭据。报告中对应 `Platform key (Cloud Assistant)`。
- **云助手尚未在线**：安装最多等待 2 分钟，上线后再加公钥，报告写明等待时长。刚开机时 SSH 可能先可连，云助手约一分钟后才上报第一次心跳。
- **等待后仍未在线**：例如自定义镜像没有预装云助手，实例又没有平台密钥时，在**高级设置**提供一次密码或私钥；阿里云接口不会提供 root 密码。需要补装云助手时，按[阿里云安装说明](https://help.aliyun.com/zh/ecs/user-guide/install-the-cloud-assistant-agent)选择匹配系统的方式，并确认该方式是否需要重启。

尚无平台公钥的实例，预检查中的 `SSH` 行显示警告，说明安装时会经云助手添加；其他需要登录的检查会在取得认证后继续。安装后核对报告中的公钥写入与等待结果。

**云助手的命令记录**

云助手会记录执行命令，保留期限以阿里云设置和文档为准。命令包含节点接入 token，agent 之后持续用它连接 server；有权查看调用记录（`ecs:DescribeInvocations`）的阿里云用户能读到 token。节点删除后，该 token 随之失效。

**ACK 集群**：账号展开后的「Kubernetes 集群」页签实时列出账号下的 ACK 集群（Serverless 与 ACS 集群的 Pod 跑在虚拟节点上，不能运行 agent 的 DaemonSet，不列出）和其中的 GPU 节点池：节点池的实例规格都带同一型号的 GPU 才算，自动模式节点池（不指定规格）不列出。选一个节点池「接入」，会建一个节点选择器为 `alibabacloud.com/nodepool-id=<节点池 ID>` 的 Kubernetes 集群条目，之后可以由 GPUWORKS 应用 manifest，也可以自己执行 kubectl，见[接入公有云托管 Kubernetes](connect-managed-kubernetes.md)。ACK 集群的节点本身也是 GPU 实例，会出现在实例表里，标为「集群节点」，不能单独安装 agent。

由 GPUWORKS 应用 manifest 需要：集群的 API Server 有公网地址（创建集群时勾选「使用 EIP 暴露 API Server」，没有的显示「仅内网 API」，只能用 kubectl 接入）；用 RAM 子账号时，这个子账号还要在 ACK 控制台「授权管理」里获得该集群的管理员（或能创建 Namespace、RBAC、DaemonSet、PriorityClass 的）RBAC 权限，否则签发出的 kubeconfig 会被集群拒绝，GPUWORKS 会把集群的拒绝原因原样显示出来。

**开关机**：关机使用“普通停机”（继续计费），保留实例的 GPU、固定公网 IP 等资源；“节省停机模式”会释放这些资源，再开机时可能没有库存，GPUWORKS 不使用它。

## DigitalOcean

**凭据**：在 DigitalOcean 控制台「API → Tokens」生成一个 Personal Access Token 填到 API Key。用 Full Access 最简单；
自定义权限时要覆盖 account、droplet、size、ssh_key、kubernetes 的读取，开关机、创建、删除 Droplet 要 droplet 的写入，
登记平台公钥要 ssh_key 的写入，签发 DOKS 的 kubeconfig 要 kubernetes 的相应权限（自定义权限的组合未逐项实测，缺权限时界面会显示 DigitalOcean 返回的原因）。

**同步**：实例表只列出 GPU Droplet（规格带 GPU 的），型号、卡数和每小时价格来自规格。DigitalOcean 的 GPU 规格要账号开通，
控制台和 API 里看不到某个规格时先在控制台申请。DOKS 集群的节点也是 GPU Droplet，标为「集群节点」，不能单独安装 agent。

**安装**：走 SSH（`root@<公网 IP>:22`）。添加账号时平台公钥登记为账号的 SSH Key，之后经 GPUWORKS 创建的 Droplet 自带它，
不用填凭据；DigitalOcean 不能给已经存在的 Droplet 追加账号 Key，别处创建的 Droplet 在安装的「高级设置」里提供一次私钥或密码。
账号里已有的 SSH Key 不会被修改或删除；平台登记的 `gpuworks-platform` 在 GPUWORKS 里删除账号后仍留在 DigitalOcean，不再需要时到控制台「Settings → Security」删除。

**开关机与释放**：开机、关机对应 Droplet 的 power on / power off（关机后仍计费）；释放即删除 Droplet，数据一并删除。

**DOKS 集群**：「Kubernetes 集群」页签列出账号下的 DOKS 集群与 NVIDIA GPU 节点池（AMD 节点池不列出），接入后节点选择器为
`doks.digitalocean.com/node-pool-id=<节点池 ID>`；可以由 GPUWORKS 用 15 分钟有效的 kubeconfig 应用 manifest，也可以自己执行
kubectl，见[接入公有云托管 Kubernetes](connect-managed-kubernetes.md)。
