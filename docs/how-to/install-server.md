# 安装和升级控制面

本文说明如何安装、验证、升级和卸载 GPUWorks 控制面。

**适用场景**：首次部署控制面，或维护已有安装。首次部署从[安装](#安装)开始；已有安装需要更新版本或移除时，直接看[升级](#升级)或[卸载](#卸载)。

## 前置条件

- 安装主机为 linux-amd64，或 Apple silicon Mac（macOS 15+，安装方式见 [macOS](#macos)）。首次安装时，安装目录所在文件系统至少有 2 GiB 空闲，监听端口未被其他进程占用。
- 安装或升级时需取得发布包。主机不能联网下载时，先从其他机器下载并拷入，升级见[离线升级](#离线升级)。
- Linux 使用 systemd、Mac 使用 launchd 时需要 root 或 sudo。没有 sudo 时安装到用户目录，见 [systemd 与 nohup](#systemd-与-nohup)。容器环境先看[在容器里运行](#在容器里运行)中的持久化要求。

## 替换已有推理平台

同机替换其他推理平台，或复用其 GPU 节点时，在执行安装命令前完成以下人工交接。已有 GPUWorks 安装更新版本时，使用本文的[升级](#升级)流程。

GPUWorks 安装器会检查本次配置的监听地址能否绑定；这项检查不会识别其他推理平台、停止其服务或迁移其配置。端口可用也不代表所选 GPU 已释放。

1. **记录并备份旧环境。** 确认旧平台的控制面、节点服务、推理进程或容器、服务管理方式、监听端口、安装与数据目录。备份其配置和数据，记录当前业务入口、模型及回退时需要恢复的设置。
2. **停止要替换的服务。** 在约定的切换窗口暂停相关业务请求，按旧平台的管理方式停止旧控制面、所选节点的旧平台节点服务及其推理进程或容器，并处理对应的开机自启、容器重启策略或守护进程，确认它们不会自动拉起。按已确认的服务、进程或容器逐项操作，保留旧数据。
3. **确认端口与 GPU 资源。** 检查 GPUWorks 本次配置的监听地址和端口（默认端口为 `8800`）未被占用；确认旧推理进程已退出，所选 GPU 的资源占用符合预期。仍有占用时，先查明归属并处理，再继续安装。
4. **使用独立目录安装。** 为 GPUWorks 选择与旧平台分开的安装和数据目录，不将旧平台数据库或状态目录作为 GPUWorks 数据目录。按下文安装控制面，再[接入节点](connect-nodes.md)、登记模型、创建部署和推理令牌。新装的 server 不会预置推理令牌，`/v1` 默认要求 key，所以在创建第一个推理令牌之前它拒绝所有推理请求；确实需要匿名访问时由管理员在**设置 → 安全**显式打开 `inference_open_access`，见[OpenAI 兼容网关的认证](../reference/openai-gateway.md#认证)。旧模型文件如需复用，先确认其格式、路径和读取权限，再按模型登记流程处理；安装不会自动导入旧平台的模型记录、部署或令牌。
5. **验证后切换入口。** 完成下文的健康检查、节点在线检查和[实际推理请求验证](../tutorials/getting-started.md#6-调用并验证回答)，再更新业务使用的 API 地址、模型名称和令牌。需要回退时，先停止 GPUWorks 中相关部署及冲突服务，再按备份和记录恢复旧服务与业务入口，避免双方争用同一端口或 GPU。

## 安装

### 一键安装

在目标主机（Linux 或 Apple 芯片 Mac）的终端执行：

```bash
bash <(curl -fsSL https://dl.gpuworks.ai/get.sh)
```

脚本挑选响应最快的发布源（中国大陆通常是阿里云 OSS 或华为云 OBS），下载本机平台的发布包并按 `SHA256SUMS` 校验，
再运行 `./gpuworks install`，之后的交互提问与下文第 3、4 步相同。还没有 `release` 版本时自动改装 `nightly`。
脚本后面的参数原样传给 `gpuworks install`，例如 `bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet`；
用环境变量 `GPUWORKS_CHANNEL=nightly` 指定渠道，`GPUWORKS_SOURCE=<源地址>` 指定发布源。脚本源码见仓库 `scripts/get.sh`。

### 手动安装

需要自己核对每一步，或主机只能离线拷入发布包时，按以下步骤在 Linux 目标主机的终端执行。

1. **下载并校验发布包。**

   中国大陆可选阿里云 OSS（下例）或华为云 OBS `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`；其他地区可用 `https://dl.gpuworks.ai`，三个源提供相同发布包。

   下例安装正式发布版（`release` 渠道，各版本的变化见[发布说明](../releases/README.md)）；要装滚动预览版，把命令中的 `release` 换成 `nightly`。`latest` 返回所选渠道当前的 release id。

   ```bash
   src=https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com
   id=$(curl -fsSL $src/release/latest)
   curl -fLO $src/releases/$id/gpuworks-$id-linux-amd64.tar.gz
   curl -fLO $src/releases/$id/SHA256SUMS
   sha256sum -c SHA256SUMS --ignore-missing
   ```

   确认对应压缩包的校验结果为 `OK`，再继续。

2. **在同一终端解压并运行安装程序。**

   ```bash
   tar -xzf gpuworks-$id-linux-amd64.tar.gz
   cd gpuworks-$id-linux-amd64
   ./gpuworks install
   ```

3. **按提示填写安装信息。**

   安装程序在需要提权时询问 sudo 密码；后续问题按回车采用默认值。下面是交互提示示例，监听地址以主机实际探测结果为准：

   ```text
   Press Enter to take the default in brackets, or type your own value and press Enter.
   install directory (default: /opt/gpuworks/server):
   listen address (default: 192.0.2.10:8800):
   start the service automatically (default: yes)? [yes/no]:
   Admin password (default: random):            ← 仅全新安装
   ```

   - 监听地址默认是主机出口 IP 和端口 `8800`；只填写端口号时保留默认 IP。
   - 已有安装会沿用现值，已有 admin 账户时不再询问密码。
   - 新建或修改管理员密码须为 8–128 个 Unicode 字符、最多 512 字节，不能与用户名相同，并拒绝常见可猜口令、整段重复及序列；不强制大小写或符号组合，允许长口令短语。未指定时生成 24 字节随机值的十六进制口令。此策略不阻止已有账户继续登录。
   - 命令行已指定的参数不再提问。脚本安装可用 `--quiet`，未指定项采用默认值；详见[交互提问](../reference/install-layout.md#install-的交互提问)。

4. **记录安装摘要。**

   保存摘要中的访问地址、管理员密码和实际安装目录、数据目录，随后完成下节验证。默认安装目录为 `/opt/gpuworks/server`，自定义目录以摘要为准。

## 验证与首次登录

本节改密及后续维护命令须使用**已安装的二进制**，在实际安装目录中执行；安装器不会自动把它加入 `PATH`。安装目录和数据目录从安装摘要或该安装的 `server.yaml` 确认。离线升级的示例另行写明执行位置。

1. 在控制面主机的终端检查服务，替换 `<监听地址>` 为安装摘要中的 IP 和端口：

   ```bash
   curl -fsS http://<监听地址>/healthz
   ```

   确认请求成功，返回的 `status` 为 `ok`、`version` 为运行版本；失败时先按[排查](#排查)检查日志和端口。

2. 浏览器打开摘要中的访问地址，以 `admin` 和摘要中的密码登录，确认可以进入控制台。

3. 修改初始密码：在控制台右上角的账户菜单选择「修改密码」，输入当前密码和新密码；修改后该账户的所有会话退出，控制台同时尝试删除数据目录中的 `initial-credentials`（删除失败时 server 日志里有告警，需手动删除）。也可以在控制面主机的实际安装目录用命令修改：systemd 安装先按摘要提示执行 `newgrp gpuworks` 或重新登录，取得新组权限，再执行：

   ```bash
   ./gpuworks reset-password
   ```

   按提示输入两次新密码（不回显）；该命令从二进制同目录的 `server.yaml` 读取数据目录，也可用 `--data` 显式指定。

4. 使用新密码重新登录，确认登录成功。
5. 确认实际数据目录中的 `initial-credentials` 文件已删除：控制台改密和 `reset-password` 都会在成功后尝试删除它，删除失败时有告警，需手动删除。该文件保存明文初始凭据，删除它不会修改账户密码。凭证说明见[控制面安装布局](../reference/install-layout.md#凭证文件)。

接下来可[接入节点并部署第一个模型](../tutorials/getting-started.md)。节点需要能访问控制面；需要域名或公网入口时，见[给控制台一个公网地址](expose-public-address.md)。部署完成后先在 **API 令牌**页创建推理令牌再调用 `/v1`：新装的 server 没有预置令牌，而 `/v1` 默认要求 key，创建之前它拒绝所有推理请求；需要匿名访问时另有管理员开关，见[OpenAI 兼容网关的认证](../reference/openai-gateway.md#认证)。

## systemd 与 nohup

有 root/sudo 且 systemd 正在运行时，安装器配置 systemd 服务；否则使用 nohup 后台进程。需要主动选择后台进程时加 `--no-systemd`。

- **没有 sudo 时**：在解压后的发布包目录中安装到可写的用户目录：

  ```bash
  ./gpuworks install --dir ~/gpuworks/server --no-systemd
  ```

- **nohup 安装后重启了主机**：服务不会自动恢复。在实际安装目录中执行：

  ```bash
  ./gpuworks restart
  ```

  等待命令完成并确认 `/healthz` 恢复。root 安装的 nohup 进程须由 root 重启。

服务配置和权限见[安装布局](../reference/install-layout.md#权限模型)，守护进程行为见 [CLI 参考](../reference/cli.md#gpuworks-serve)。

### macOS

1. 在发布站选择 `gpuworks-<release_id>-darwin-arm64.tar.gz` 和对应 `SHA256SUMS`，下载到 Mac。
2. 在 Mac 终端用 `shasum -a 256` 核对压缩包的 SHA-256；与 `SHA256SUMS` 中的值一致后，再解压。
3. 进入解压目录，执行 `./gpuworks install`，按上面的提示填写，然后完成[验证与首次登录](#验证与首次登录)。

Mac 上默认安装到 `~/gpuworks/server`（安装用户的主目录），以 nohup 后台进程运行，不配置开机自启；自启提问默认 `no`。同机 agent 默认位于 `~/gpuworks`，两者互不覆盖。在实际控制面安装目录中按需执行：

```bash
./gpuworks restart            # 重启服务
./gpuworks stop               # 停服；launchd 安装会在下次开机时再次启动
```

需要随系统启动时，使用有 sudo 权限的账户执行 `./gpuworks install --autostart`（或在自启提问回答 `yes`）：安装为 launch daemon，服务以安装用户运行。重装已有 launch daemon 的安装时默认保留 launch daemon。launchd 配置见[安装布局](../reference/install-layout.md#launchd-plistmacos)。

### 在容器里运行

**先把安装目录及其配置、数据放在持久化存储上。** 控制台升级不会更新容器镜像；按旧镜像重建且未保留安装目录时，会回到旧版本。

根据容器平台选择一种启动方式：

- **可以设置启动命令**：安装完成后，把容器启动命令设为以下前台守护进程；路径以实际安装目录为准。

  ```bash
  /opt/gpuworks/server/gpuworks serve --supervise --config /opt/gpuworks/server/server.yaml
  ```

  容器重启后会重新启动服务，也支持控制台升级。**这种方式不要在容器内执行 `gpuworks restart` 或 `gpuworks upgrade`**，应使用控制台升级或重启容器；停掉容器主进程会使容器退出，使用 `docker run --init` 也存在这个影响。

- **启动命令由平台决定**：先在容器内完成安装，容器重启后用平台开机脚本或在安装目录执行 `./gpuworks restart`。开机脚本必须后台启动，不能阻塞；将 `<dir>` 替换为实际安装目录：

  ```bash
  setsid nohup <dir>/gpuworks serve --supervise --config <dir>/server.yaml >/dev/null 2>&1 &
  ```

  AutoDL 可自建 `/etc/autodl.sh`，Vast.ai 使用 `/root/onstart.sh`。RunPod 可把启动命令设为先启动 sshd、再 `exec` 上述前台守护进程；修改启动命令会重置 pod 并清空容器盘。RunPod 停止再启动也会清空容器盘，使用 `/workspace` 网络卷保存安装。

Kubernetes 请通过更换镜像升级，不使用控制台升级。

## 重复执行 install

需要修复服务或切换 systemd/nohup 时，可在发布包目录重新执行 `./gpuworks install`，指定原安装目录。它会替换二进制、重建服务并重启，保留已有配置、数据库、主密钥和 admin 账户；只有显式指定的参数会覆盖对应配置。

`--password` 不会修改已有账户密码。需要更换版本时使用下面的升级流程，重复安装不提供升级备份与回滚。

## 升级

先确认以下影响，再选择控制台、命令行或离线方式：

- 控制台和 `/v1` 网关会短暂中断，节点上的推理容器和模型不受影响。
- 升级前会自动备份，失败时回滚；回滚恢复的是升级前快照，升级期间的数据变更可能丢失。备份范围与兼容性见[升级与版本兼容](upgrade-and-compatibility.md)。
- 本次只升级控制面，已有节点的 agent 不会自动升级；节点升级见[接入、升级和卸载节点](connect-nodes.md)。

### 在控制台升级

控制面须由 systemd、launchd 或 `serve --supervise` 托管，运行的是安装目录中的二进制，且服务账户可写该目录；不满足时页面会说明原因并给出处理命令。

1. 以管理员身份打开**设置 → 通用 → 版本与更新**，查看**服务端**的当前版本。
2. 在**更新渠道**选择**正式发布版（release）**或**滚动预览版（nightly）**，点击**检查更新**，核对服务端的最新版本。
3. 点击**服务端**行的**升级**，核对确认框中的目标版本和中断提示，再点击**升级**。
4. 等待页面恢复，确认提示“服务端已升级到目标版本”，且**当前**版本与目标一致。若 5 分钟内未看到新版本应答，页面会提示异常；按[排查](#排查)确认当前版本和日志，不把等待超时直接当成升级成功或已回滚。

**服务器不能访问发布源时**，在同一行点击**上传升级包**，选择与服务器系统、架构匹配的 `gpuworks-<release_id>-<os>-<arch>.tar.gz`。**选择文件后即开始上传和升级，不会再出现在线升级的确认框。** 完成后按第 4 步核对结果。

如果页面没有可用的升级操作：

- 提示没有服务管理器时，在主机上执行页面给出的 `restart` 命令；容器使用页面给出的启动命令，具体限制见[在容器里运行](#在容器里运行)。
- 提示未安装、运行的不是安装目录中的程序或安装目录不可写时，先处理对应条件，或按[命令行方式](#在命令行升级)升级。
- 上传的包不匹配、无法运行、属于本地构建，或目标版本不支持控制台升级时，会在替换前拒绝。旧版本升级包可改用[离线升级](#离线升级)。

**自动检测新版本**默认开启，检查时会向发布方报告安装规模；关闭后改为手动点击**检查更新**，不再自动检查或报告。报告内容见[配置参考](../reference/configuration.md#镜像目录与镜像拉取)。更新渠道同时用于控制台和命令行。

### 在命令行升级

在控制面主机的**实际安装目录**执行。systemd 安装先按摘要提示取得组权限；同机有多套安装时，进入要升级的那一套目录。旧版只有 `install.json` 的安装也必须使用该目录中已安装的二进制。

1. 检查是否有新版本，并核对目标安装和版本：

   ```bash
   ./gpuworks upgrade --check
   ```

2. 确认后执行升级，等待命令完成：

   ```bash
   ./gpuworks upgrade
   ```

3. 重新访问控制台，或调用 `/healthz`，确认运行版本与目标一致。命令以非零码退出时按错误提示处理；探活失败会自动恢复旧版本，备份路径会显示在输出中。已是最新版时以 `0` 退出，不会重复升级。

使用其他来源时，按场景选一条命令，替代上述第 1、2 步；先追加 `--check` 核对，再去掉它执行：

```bash
./gpuworks upgrade --base-url https://releases.example.internal # 内网发布站
```

```bash
./gpuworks upgrade --source https://mirror.example.com/gpuworks # 自建发布通道镜像
```

需要固定版本、调整备份份数或无人值守时，查 [CLI 参数](../reference/cli.md#gpuworks-upgrade)。渠道中的版本比当前版本旧时默认不降级；确认需要降级或替换本地构建后，才使用 `--force`。自建镜像的目录与校验要求也见该参考页。

### 离线升级

1. 从可联网的机器下载匹配目标主机系统和架构的发布包、同一版本的 `SHA256SUMS`，一起拷到控制面主机。
2. 在存放发布包的目录校验，确认对应压缩包显示 `OK`，再检查包内版本。以下示例使用 Linux 默认安装目录，`<release_id>` 替换为下载包名中的版本标识；自定义安装或 Mac 请替换程序路径和包名，Mac 用 `shasum -a 256` 对照 `SHA256SUMS` 校验。

   ```bash
   sha256sum -c SHA256SUMS --ignore-missing
   /opt/gpuworks/server/gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz --check
   ```

3. 确认后执行升级，完成后按[命令行升级](#在命令行升级)第 3 步验证：

   ```bash
   /opt/gpuworks/server/gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz
   ```

无需解压；该流程不访问发布源，包损坏、架构不匹配或本地构建会在停服前报错。不要用新包中的 `./gpuworks install` 替代升级，否则没有升级备份与回滚。

新 agent 已内嵌在 server 中，仍可在节点页点击**升级 Agent**，不需要访问发布源。

## 卸载

先查看卸载清单中的检查状态、资源证据、保留项与处理建议。活动负载或关键资源无法确认时默认拒绝卸载，`--yes` 不绕过检查；需要保留业务、仅移除控制面时显式使用 `--keep-resources`，详见 [CLI 卸载检查](../reference/cli.md#gpuworks-uninstall)。数据库记录不能证明远端 GPU 已释放或云资源已停止计费。

需要先预览彻底清理范围时，在实际安装目录执行 `./gpuworks uninstall --purge --dry-run --json`。预览不会停止服务或删除文件；确认清单后再执行以下步骤。

1. 在控制面主机的实际安装目录运行：

   无 sudo、安装到用户目录时，由原安装用户执行 `./gpuworks uninstall`；下方自动化示例也去掉 `sudo`。

   ```bash
   sudo ./gpuworks uninstall
   ```

2. 按提示选择是否清理数据，检查清单后确认。systemd、nohup 和 macOS launchd 使用同一命令。

   | 选择 | 结果 |
   |---|---|
   | 保留数据（默认） | 停止服务、取消自启并移除程序；保留配置、数据库、主密钥、日志与备份，便于重装 |
   | 清理数据 | 还会删除本安装已知配置、数据、备份及本服务日志，包括配置指定的外置路径；执行前务必核对清单 |

3. 检查卸载报告。实际卸载返回码为 `0` 表示所选操作完成，`1` 表示失败；检查错误和已完成项目，处理原因后重试。涉及受保护路径、软链接、挂载点或无法确认归属时，命令会报错并拒绝相应清理；检查路径和归属后再处理。

自动化执行时，按需要选一条：

```bash
sudo ./gpuworks uninstall --yes                 # 保留数据
```

```bash
sudo ./gpuworks uninstall --purge --yes --json   # 清理数据并输出报告
```

非交互执行必须传 `--yes`（预览除外），`--quiet` 不会跳过确认。未知文件、服务账号与组等保留项见 [CLI 卸载说明](../reference/cli.md#gpuworks-uninstall)。

同一安装的 expose 公网入口也会关闭。**远端 agent、推理服务、云租机和 Cloudflare tunnel / DNS 不会随控制面卸载而回收**，仍须单独处理；云资源可能继续计费。

## 排查

表中的 `./gpuworks` 在实际安装目录执行；`<data目录>` 和 `<安装目录>` 从安装摘要或该安装的 `server.yaml` 确认。

| 现象 | 检查与处理 |
|---|---|
| 安装或升级后无法访问 | 查看 `<安装目录>/logs/gpuworks-server.log`，修复错误后重试；再次调用 `/healthz` 并核对版本。日志位置自定义时以实际配置为准 |
| `cannot listen on :8800` | 端口被其他进程占用；先用 `systemctl status gpuworks` 确认是否为旧服务，不要直接重复安装 |
| `create /opt/gpuworks/server needs root` | 使用有 sudo 权限的账户，或改为 `--dir ~/gpuworks/server --no-systemd` |
| `the service account gpuworks cannot enter /root` | 服务账户进不了安装目录的上级目录，systemd 无法启动服务。改装到默认的 `/opt/gpuworks/server` 或其他每级都可进入的目录；确需以 root 运行时加 `--service-user root` |
| 修改初始密码或忘记密码 | 能登录时在控制台右上角账户菜单选择「修改密码」；忘记密码时执行 `./gpuworks reset-password`，按提示输入两次新密码（不回显），数据目录从二进制同目录的 `server.yaml` 读取（也可用 `--data` 指定）；不要将真实密码留在命令历史或日志中。改密后重新登录验证 |
| 升级或重置密码报权限错误 | 当前终端可能还没取得 `gpuworks` 组权限，执行 `newgrp gpuworks` 或重新登录后重试；数据目录仍不可写时检查其权限 |
| 控制台提示渠道尚无版本 | 切换到已有发布的渠道，或等待该渠道发布后重新检查 |
| 发布索引签名校验失败 | 检查服务端出网代理、网关或登录拦截；暂时可用匹配的本地升级包 |
| 无法连接发布源 | 检查服务端出网与代理；无法联网时采用上传升级包或离线升级 |
| 控制台升级等待超时或失败 | 查看服务日志与实际数据目录中的 `install-history.jsonl`，再检查 `/healthz` 的运行版本；不能仅凭页面超时判断回滚。升级成功但历史里没有这一行时，服务日志会有 `console upgrade confirmed without an install history entry`：多是早期版本以 root 安装时把该文件留给了 root，以 root 执行一次 `gpuworks upgrade`（会重新把安装树交给服务账户）或 `chown gpuworks <data>/install-history.jsonl` 即可修复。回滚后仍无法启动时，按[升级与版本兼容](upgrade-and-compatibility.md)检查并恢复备份 |
| 升级后节点全部离线 | 检查**设置**页的**监听地址**与反向代理/NAT 是否一致，再检查节点版本，见[配置节点网络](configure-networks.md#监听地址与别人怎么连过来) |

## 补充说明

安装目录、权限、交互默认值和备份文件见[控制面安装布局](../reference/install-layout.md)；参数、升级源选择及旧布局兼容行为见 [CLI 参考](../reference/cli.md#gpuworks-upgrade)。下面的后台机制用于排查，不是需要手工执行的步骤。

<details>
<summary>控制台升级的校验、确认与回滚机制</summary>

在线升级与上传升级包使用同一流程：先解出新二进制并试运行 `version`，再检查 `console-upgrade-protocol` 支持；不支持确认或回滚自身启动的版本会被拒绝。

校验通过后，用 `VACUUM INTO` 生成数据库快照，连同 `master.key`、`server.yaml` 和当前二进制写入 `<dir>/backups/<release_id>-<时间>.tar.gz`，写入 `<dir>/tmp/upgrade-pending.json`，替换二进制后退出，由服务管理器启动新版本。

- 新版本在 15 分钟内健康应答时，确认升级、记录审计、删除标记，并保留最近 3 份备份。
- 连续 3 次未能启动，或持续不健康时，在后续启动中恢复数据库、主密钥、配置和旧二进制。
- 标记中的目标版本与实际启动版本不一致时（例如二进制被其他升级方式替换），删除标记，不累计启动次数，也不据此回滚。

页面只等待 5 分钟，早于服务端的确认期限，因此页面超时后仍需检查实际版本和日志。

</details>
