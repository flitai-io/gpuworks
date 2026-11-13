# 离线环境部署

**适用场景**：集群不能访问公网：拉不到 Docker Hub 等镜像仓，也下载不了 Hugging Face / ModelScope 上的模型。

**前置条件**：一台能上网的机器（下文叫「外网机」），下载好的文件用 U 盘、跳板机等任意方式拷进内网；管理员权限。已有的目录或节点副本不必重复导入。

## 推荐流程

### 1. 安装管理节点

按管理节点服务器的类型选发布包：Linux x86 服务器用 `gpuworks-<release_id>-linux-amd64.tar.gz`，Apple silicon Mac 用
`gpuworks-<release_id>-darwin-arm64.tar.gz`。在外网机上从发布源下载，拷到服务器上解压后执行 `./gpuworks install`，
见[安装和升级控制面](install-server.md)。server 内嵌了系统镜像目录和推理模板目录，离线也能直接用。

### 2. 添加计算节点

计算节点页「添加节点」，按向导装 agent。节点从 server 下载 agent，不需要公网，见[接入节点](connect-nodes.md)。

节点默认使用名为 `default` 的本地存储，模型放在每台节点自己的 `/opt/gpuworks/models` 下。这个盘放不下模型时，在「存储」页编辑
`default`，改成数据盘上的目录；个别节点的目录不同时，在「适用节点」里给它单独设置。见[使用存储](storages.md)。

### 3. 在外网机上下载镜像

先确定要哪些镜像：在镜像页「系统镜像」里按引擎（vLLM、SGLang 等）和加速卡找到版本，记下 tag，例如
`cuda13.0-sglang0.5.21-r2`（完整引用是 `gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2`）。

再从阿里云盘下载对应的归档，不需要装 Docker，网络不好时也比拉 Docker Hub 稳：

<https://bj20013.apps.aliyunfile.com/disk/s/pweW8krgZfw?domainId=bj20013>

- 按「引擎-框架」分目录：`vllm-cuda/`、`sglang-cuda/`、`vllm-omni-cuda/`、`vllm-cann/`、`sglang-cann/`、`vllm-omni-cann/`、
  `vllm-dtk/`、`sglang-dtk/`、`vllm-hggc/`、`sglang-hggc/`、`vllm-rocm/`。
- 文件名是 `gpuworks-runner_<tag>_linux-<架构>.tar.gz`。按计算节点的 CPU 架构选 `amd64` 或 `arm64`（昇腾服务器多为 `arm64`）；
  昇腾文件名里的 `910b`、`a3`、`310p`、`950` 是芯片型号。
- 归档里已经是规范镜像引用，导入后不需要再打 tag。网盘上通常只有各系列的当前版本，其他版本见[用 Docker 自己导出镜像](#用-docker-自己导出镜像)。

### 4. 在外网机上下载模型

用官方 CLI 下载完整目录（含 `config.json`、tokenizer 与全部权重分片）：

```bash
# Hugging Face（国内可加 HF_ENDPOINT=https://hf-mirror.com）
huggingface-cli download Qwen/Qwen3-8B --local-dir Qwen3-8B
# ModelScope
modelscope download --model Qwen/Qwen3-8B --local_dir Qwen3-8B
```

`huggingface-cli` 下载的目录里有 `.cache/huggingface/`，原样保留，导入后模型能固定到对应的 commit。

### 5. 导入镜像和模型

建议直接拷到一台计算节点上：

**镜像**：拷到节点后导入节点的容器运行时：

```bash
# Docker 节点
docker load -i gpuworks-runner_<tag>_linux-amd64.tar.gz
# nerdctl 节点（昇腾常见）：先解压，且必须带 gpuworks 命名空间，否则平台看不到
gunzip -c gpuworks-runner_<tag>_linux-arm64.tar.gz | nerdctl -n gpuworks load
```

约一分钟后，镜像页对应节点显示「已存在」。udocker（runveil）节点用 agent 自带的命令导入，`<image-ref>` 填镜像页上显示的完整镜像引用：

```bash
sudo gpuworks-agent runveil load -i gpuworks-runner_<tag>_linux-amd64.tar.gz <image-ref>
```

不方便登录节点时，也可以[经管理节点中转](#经管理节点中转导入镜像)。

**模型**：把模型目录拷到节点的本地存储目录下，例如 `/opt/gpuworks/models/Qwen/Qwen3-8B`，然后在模型页「注册模型」→ 来源选
「计算节点本地」→ 选节点、填这个目录 → 预览后注册。平台从 `config.json` 读出架构、精度和上下文长度。本地存储不支持批量扫描，
每个模型登记一次；目录里有 `.part` 等未下载完的文件时登记会失败。

**其他节点不用再拷**，在界面上从已有的节点复制：

- 部署时自动复制：同一[同步组](configure-pull-settings.md#同步组)里有节点已经有这个镜像或模型时，副本启动时自动从它复制。新装的平台默认所有节点在一个组。
- 提前手动复制：模型页对应行「同步到其他计算节点」；镜像详情页节点行的「获取方式」选「从其他节点同步…」。见[节点间同步](peer-sync-models.md)。

节点间复制要求两台节点之间网络直通。导入或同步完成后，检查目标节点的镜像状态为 `present`、模型副本可用，不要只看传输进度达到 100%。

### 6. 部署并验证

照常[部署模型](deploy-models.md)，选择与模型、节点架构匹配的镜像和模板；节点上已有的镜像和模型直接使用，不会重复下载。
等待实例就绪后，使用该部署的调用示例，经实际推理接口发送一次请求并确认返回结果；仅访问 `/v1` 前缀不能判断推理成功。

## 特殊情况

### 模型放在 NFS 上

模型多、节点多时，可以把模型按 `<组织>/<仓库名>/` 放到 NFS 上，所有节点共用一份，不用逐台拷贝：

1. 「存储」页「添加存储」，类型选「NFS」，填导出地址并「试挂」。
2. 存储卡片上点「扫描导入」，勾选模型「导入所选」，一次登记一批。

见[导入已有的模型库](storages.md#导入已有的模型库)。

### 用 Docker 自己导出镜像

要的版本不在阿里云盘上，或外网机能顺畅访问镜像仓时，可以在外网机上用 Docker（或 nerdctl）自己拉取导出：

```bash
REF=gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2
docker pull --platform linux/amd64 "$REF"      # arm64 节点改成 linux/arm64
docker save "$REF" | gzip > gpuworks-runner_cuda13.0-sglang0.5.21-r2_linux-amd64.tar.gz
```

在国内访问 Docker Hub 慢时，可以从 SWR 等镜像副本拉取，拉完用 `docker tag` 打回上面的规范引用再 `docker save`。平台按规范引用认镜像，
归档里只有副本地址的 tag 时，导入后平台认不出来。

### 经管理节点中转导入镜像

udocker（runveil）节点，或不方便登录节点时，可以在控制台里导入。节点须保持与 server 连接；即使节点不能访问归档来源，也可由 server 中转。

开始前，确认 server 能读取归档。普通内网来源及下面的本机回环地址默认无需白名单；仅当开启严格模式时才需要白名单；平台管理员可在**设置 → 安全与访问 → 出站访问策略**管理，或由主机管理员直接编辑 `server.yaml` 的 `outbound_restrict_private_hosts` 和 `outbound_allow_private_hosts` 授权来源。两个字段保存后下一次请求生效，无需重启 server，见[HTTP 导入前提](import-from-http.md)。

1. 把归档放到管理节点能访问的 HTTP 地址。最简单的是拷到管理节点上，就地起一个临时文件服务：

   ```bash
   cd /data/offline-images && python3 -m http.server 8090
   ```

2. 镜像页打开该镜像，在节点表要导入的那一行，「获取方式」选「从 URL 导入…」，填
   `http://127.0.0.1:8090/<文件名>` →「解析」，核对归档里的镜像引用 → 勾选「管理节点中转」→「拉取」。

server 边下载边推给节点，不落盘。中转不可续传，中断后从头开始；几十 GB 的镜像、链路又不稳时，优先拷到节点上导入。
Docker 与 udocker 节点可以直接用 `.tar.gz`；nerdctl 节点请先解压成 `.tar` 再放上去。细节见[从 HTTP 文件服务器导入](import-from-http.md#节点获取方式直连或管理节点中转)。

### 有内网镜像仓

节点多、长期离线、镜像经常更新时，可以把镜像同步进内网 registry（Harbor 等），一次配置后部署就和在线时一样自动拉取：

1. 同步镜像时保持原来的仓库路径，例如 `registry.corp.example/gpuworks/gpuworks-runner:<tag>`。
2. 设置 → 拉取设置 → 来源规则，加一条 `gpuworks/` → `registry.corp.example/gpuworks/`，并关闭「允许回源」，节点就不会再去公网拉。
   仓库要登录时配[凭据](configure-pull-settings.md#凭据)，规则写法见[来源规则](configure-pull-settings.md#来源规则)。

### Kubernetes 集群的 agent 镜像

以 DaemonSet 接入的 Kubernetes 集群，节点要先拉到 agent 镜像。阿里云盘的 `gpuworks-agent/` 目录里有
`gpuworks-agent_r1_linux-<架构>.tar.gz`（`r1` 是 agent 运行环境的版本，不随 server 版本变化），任选一种导入：

- 推到内网镜像仓，在集群条目「高级」的「agent 镜像」里填这个地址（仓库要登录时再填「镜像拉取凭据」）：

  ```bash
  docker load -i gpuworks-agent_r1_linux-amd64.tar.gz
  docker tag gpuworks/gpuworks-agent:r1 registry.corp.example/gpuworks/gpuworks-agent:r1
  docker push registry.corp.example/gpuworks/gpuworks-agent:r1
  ```

- 或在每个节点上导入 kubelet 用的 containerd，集群条目保持默认：manifest 里的 `gpuworks/gpuworks-agent:r1` 在节点上已经有，就不会去拉。
  之后新加入集群的节点也要先导入。

  ```bash
  gunzip -c gpuworks-agent_r1_linux-amd64.tar.gz | ctr -n k8s.io images import -
  ```

agent 程序本身由 server 下发和升级，平时升级 server 不用再导入；只有运行环境版本变化时（发布说明会写明）才要换新的归档。

### 模型放在内网 HTTP 目录

模型目录已在 HTTP 文件服务器上时，按[从 HTTP 目录注册模型](import-from-http.md#从-http-目录注册模型)登记并下载。server 需要读取目录进行解析，目标节点也必须能直接访问该目录；内网地址按该指南配置出站授权。
模型目录下载没有管理节点中转。节点不能直接读取目录时，改用上面的本地复制、NFS 或节点间同步。

### 节点不能出公网

常见于云上没有公网 IP、也没有 NAT 网关的实例（例如阿里云 VPC 里的 ECS）：节点能连上 server，但拉不到镜像、下载不了模型。
同样按上面的流程导入；在云上还可以用同一 VPC 内的服务代替公网：

- 镜像：云厂商镜像服务的 VPC 地址，例如阿里云 ACR 的 `registry-vpc.<地域>.aliyuncs.com`，按[有内网镜像仓](#有内网镜像仓)配置。
- 模型：同 VPC 的 NAS 按 [NFS](#模型放在-nfs-上) 使用；或用 `ossutil` 经 OSS 内网地址 `oss-<地域>-internal.aliyuncs.com`
  下载到节点后登记（`ossutil` 安装包本身要先拷到节点）。

### 让缺镜像或模型的部署立刻报原因

整个集群都没有公网时，按[完全离线：两条「禁止」规则](configure-pull-settings.md#完全离线两条禁止规则)给 Hugging Face 与 ModelScope
各加一条站点规则（无替代地址、关闭回源）。只有下载路径、又被这两条规则禁止的 HF / ModelScope 模型会立即给出 `weights_fetch_forbidden` 原因，不用等公网下载超时。
这些站点规则不限制 registry、HTTP 目录或其他外部访问。
镜像对应的做法是关闭「允许回源」的来源规则。

### 更新镜像目录和推理模板

想用比内嵌更新的系统镜像目录与推理模板目录时：在外网机上从任一发布源（如 `https://dl.gpuworks.ai`）下载 `<channel>/manifest.json`
与 `<channel>/index.json` 里列出的 `catalog/templates/<sha256>.tar.gz`，在设置页「版本与更新」点「上传 manifest」「上传目录包」，
规则见[镜像目录](../reference/image-families.md#更新目录)。离线环境里「检查更新」会因为发布源不可达返回 502 `UPDATE_SOURCES_UNAVAILABLE`，不改库。

## 离线边界

server 不保存、也不接受镜像 tar 包直接上传；控制台中的管理节点中转从 HTTP 来源读取归档，再流式推给节点。
配置离线来源不等于操作系统已断网；需要验证无公网访问时，应额外施加网络隔离或检查网络流量。

## 相关

- 模型目录或镜像归档已经放在内网 HTTP 文件服务器上：[从 HTTP 文件服务器导入模型与镜像](import-from-http.md)
- 下载、缓存与「本地已有」的判定：[下载、缓存和节点间同步模型](download-and-sync-models.md)
