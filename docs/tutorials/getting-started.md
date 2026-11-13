# 从零启动并部署第一个模型

**适用场景**：首次安装 GPUWorks，接入一台 GPU 节点并部署对话模型。

**前置条件**：下表所列的 Linux 主机、权限与网络；本教程不用于升级已有环境。

**目标**：通过 OpenAI 兼容接口调用 `Qwen/Qwen3-1.7B`，获得模型回答。

## 开始之前

如果这些主机已运行其他推理平台，或准备复用旧平台的 GPU，先按[替换已有推理平台](../how-to/install-server.md#替换已有推理平台)完成备份、停服以及端口和 GPU 占用检查，再开始本教程。安装命令不会自动接管旧平台。

| 你需要 | 说明 |
|---|---|
| 控制面主机 | Linux x86_64 + systemd；具备 Bash、curl、tar、sha256sum 和 sudo 权限。浏览器与 GPU 节点可访问其 8800 端口；控制面提供资源管理、Web 控制台和推理入口 |
| GPU 节点 | 实验专用的 Linux x86_64 + systemd 主机，未安装其他 GPUWorks agent；具备 curl、root 或免密 sudo，以及存放模型和镜像的本地磁盘 |
| GPU 与驱动 | 示例使用 GeForce RTX 4090（48 GiB），NVIDIA 驱动 ≥ 580，`nvidia-smi` 可识别 GPU；其他型号需确认与推荐模板兼容 |
| 运行环境 | 有可用 Docker 时使用 Docker，否则 agent 使用内置 runveil。驱动、GPU 与推理镜像须兼容，部署前检查配置评估 |
| 网络 | 控制面主机能下载 GPUWorks 发布包；控制面和节点能访问 ModelScope，节点能访问推理镜像仓库。离线场景改用[离线环境部署](../how-to/deploy-offline.md) |

控制面和 GPU 节点可以同机，命令仍按下文指定的位置执行。模型、镜像的下载时间取决于网络与磁盘速度。
Mac 本地试用见[在 Mac 上运行第一个本地模型](mac-local-model.md)；Linux 无 sudo 或非 systemd 环境见[安装和升级控制面](../how-to/install-server.md)。

下文以 `host3` 为节点显示名称，无需修改操作系统主机名；模型、部署及令牌名称在对应步骤填写。

## 1. 启动 server

正式部署用安装命令（写 systemd 单元、后台常驻、默认目录 `/opt/gpuworks/server`）：

```bash
./gpuworks install --addr 192.0.2.10:8800
```

本文用 `192.0.2.10` 代表 server 主机的地址（文档专用的示例地址），操作时换成你自己主机的 IP。

发布包的下载和校验步骤见[安装和升级控制面](../how-to/install-server.md)。只想先看一眼，也可以前台跑一个临时实例：

```bash
./gpuworks serve --addr :8800 --data ./data
```

**你应该看到**：命令最后打印安装摘要，包含访问地址和 admin 初始密码；`curl http://192.0.2.10:8800/healthz` 返回 `{"status":"ok",...}`。

首次启动会创建 admin 账户，密码记录在数据目录的 `initial-credentials`（默认 `0600`，可信 Linux 平台组为 `0660`），安装摘要里也会打印一次。
浏览器打开 `http://192.0.2.10:8800`，用 admin 登录，改密后删除该文件。界面默认跟随浏览器语言，
右上角可以切换为中文。

## 2. 接入节点

左侧菜单进入“计算节点”，点击右上角“添加节点”。本教程用最通用的 curl 方式：把“目标机器是否支持 SSH”选为
“不支持 SSH”，“节点名称”填 `host3`，“连接方向”保持“Agent 发起连接”，“管理节点接入地址”预填的是浏览器当前访问的地址（也可以改成
域名或反代地址），然后点击“创建节点并生成命令”。

![添加节点](../assets/screenshots/tutorial-add-node.png)

复制生成的命令，在 GPU 节点上执行（命令里已经带了这个节点专属的 token）：

```bash
curl -fsSL http://192.0.2.10:8800/install.sh | sh -s -- \
  --server http://192.0.2.10:8800 --token <node-token>
```

默认 agent 主动外连 server，节点不需要开放入站端口。若节点无法出站连到 server、但 server 能直连节点的监听端口，改用
[server_dial 接入指南](../how-to/connect-nodes.md)。

**你应该看到**：回到“计算节点”页，几秒内状态变为“在线”，并显示 GPU 型号与数量、驱动版本、内存和 CPU；
点击行尾“详情”可以看到运行时（docker / runveil）、Agent 版本和网络地址等。

![计算节点](../assets/screenshots/nodes.png)

如果节点一直没有上线，见[排障指南](../how-to/troubleshooting.md)的“节点一直不上线”。

## 3. 注册并下载模型

### 注册模型

1. 打开左侧**模型**，点击**注册模型**。
2. **来源**选择 **ModelScope**，**来源 ID**填写 `Qwen/Qwen3-1.7B`，**名称**填写 `dgpu-demo-a-ms`。
   **高级选项 → 版本指定**留空，使用默认分支。
3. 点击**确认注册**，在模型列表中核对名称与来源 ID。

![注册模型表单：核对模型来源、来源 ID 和登记名称](../assets/screenshots/tutorial-register-model.png)

图：来源选 ModelScope，名称填 `dgpu-demo-a-ms`。

注册仅登记信息，权重需另行下载。`dgpu-demo-a-ms` 是名称示例，不会开启 dGPU；后续部署可复用此模型和 vLLM 镜像。

### 下载到节点

1. 在 `dgpu-demo-a-ms` 的模型卡片上点击**下载到…**；下载图标的提示为**从模型源下载到存储**。
2. **存储**选择节点使用的本地存储（仅有一个可写本地存储时自动选定，不显示此字段）。
3. 在**计算节点**中仅选择 `host3`，点击**下载**。
4. 点击模型名称，在详情的**模型文件**中找到 `host3`，等待下载完成、状态变为**可用**后再继续。
   **已开始下载**仅表示任务已提交。

![模型文件页：核对目标节点、文件状态和下载进度](../assets/screenshots/tutorial-model-ready.png)

图：确认 host3 的模型文件为“可用”。

## 4. 创建推理令牌

1. 打开左侧 **API 令牌**，在**推理服务令牌**区域点击**创建推理令牌**。
2. 在**创建 API 令牌**弹窗中，名称填写 `tutorial-qwen3-17b`，点击**创建**。
3. 在**请立即保存 API 令牌**弹窗中点击**复制 API 令牌**，安全保存完整值，再点击**我已保存**。

**完成依据**：列表出现 `tutorial-qwen3-17b`，你已保存以 `gw-` 开头的完整令牌。列表仅显示前缀，不能用于调用。
新装的 server 不预置任何令牌，所以这一步必须自己建：`/v1` 默认要求 key，在创建第一个令牌之前它拒绝所有推理请求。
`gwp_` 开头的是**平台 API 令牌**，用于管理接口，不用于本教程。

## 5. 部署模型

1. 打开左侧**推理服务**，点击**创建部署**，使用**快捷部署**。**模型**选择 `dgpu-demo-a-ms`，**目标芯片**选择 NVIDIA。
2. **推荐推理模板**选择 **Qwen3-1.7B → general · vllm · balanced**，镜像应为 `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`。
   若弹出**使用已登记的模型？**，选择带 **MS**、**1 个节点**标记的 `dgpu-demo-a-ms`，点击**使用此模型并部署**，返回后核对所选模型。
3. **部署名称**、**对外模型名**均填 `tutorial-qwen3-17b`；后者用于请求中的 `model`。

![部署模型表单：核对模型、目标芯片、推理模板和部署名称](../assets/screenshots/tutorial-deploy-model.png)

图：部署名称和对外模型名保持一致。

4. 在**节点**区域选择**单机**和**手动指定**，选择 `host3` 和空闲的 **GPU 0**，副本数保持 `1`。
   **dGPU 显存（GiB）**留空（使用整卡），关闭**可休眠**；**最大上下文**设为 `2048`，**显存使用率**设为 `0.90`，其余使用模板值。

![节点选择表单：核对目标节点、GPU 和右侧配置评估](../assets/screenshots/tutorial-deploy-node.png)

图：选中 host3 的 GPU 0，并检查右侧配置评估。

5. 等待**配置评估**完成，核对节点、GPU、镜像和显存，确认没有阻止部署的错误。
6. 点击**部署**，在**推理服务**打开 `tutorial-qwen3-17b`，查看模型准备、镜像拉取和实例启动进度。

**完成依据**：部署及实例均为**运行中**。首次启动需下载镜像、加载模型，请等待完成。
下一步用真实请求验证推理接口。

![推理服务：tutorial-qwen3-17b 运行中，健康正常，副本数为 1/1](../assets/screenshots/tutorial-deployment-ready.png)

图：确认部署“运行中”、健康检查正常，副本数为 `1/1`。

## 6. 调用并验证回答

在能访问控制面的机器上打开 **Bash 终端**，替换 URL 后执行以下命令，按提示粘贴第 4 步的完整推理令牌（输入不回显）。

```bash
GPUWORKS_URL=http://192.0.2.10:8800
read -r -s -p '请粘贴推理令牌：' GPUWORKS_API_KEY
printf '\n'
curl -sS -w '\nHTTP %{http_code}\n' "$GPUWORKS_URL/v1/chat/completions" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"tutorial-qwen3-17b","messages":[{"role":"user","content":"请用一句话介绍你自己。"}],"max_tokens":256,"chat_template_kwargs":{"enable_thinking":false}}'
unset GPUWORKS_API_KEY
```

请求关闭 Qwen3 思考模式，便于检查简短回答。**最终验证**须同时满足：

- 命令末尾显示 `HTTP 200`。
- 返回 JSON 的 `choices[0].message.content` 有回答，内容不必与示例一致。

仅有“运行中”状态或 HTTP 错误响应不能算完成。

也可在**试验场 → 对话**选择 `tutorial-qwen3-17b`，输入“请用一句话介绍你自己。”，点击**发送**查看回答。

![试验场：tutorial-qwen3-17b 已返回实际回答](../assets/screenshots/tutorial-model-answer.png)

图：模型名称与部署时的对外模型名一致，下方显示本次提问的回答。

## 下一步

- 想在一张卡上同时跑两个模型：[一张 GPU 跑两个模型](one-gpu-two-models.md)
- 弄清部署表单里每一项的含义：[部署模型与解释调度结果](../how-to/deploy-models.md)
- 了解整个控制台：[控制台页面导览](../reference/web-ui-tour.md)
- 选择合适的特性组合：[怎么选：dGPU、sleep/wake、多机、副本、PD](../explanation/choosing-features.md)
- 上生产之前：[生产环境部署](../how-to/deploy-production.md)
