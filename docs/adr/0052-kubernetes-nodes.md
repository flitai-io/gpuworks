# ADR 0052: Kubernetes 是一种节点来源加一种运行时，卡按需向 kubelet 借用

日期：2026-10-19 · 状态：已接受

## 背景

用户已有带 GPU 的 Kubernetes 集群，希望不逐台装 agent 就接入，并且 gpuworks 的工作负载在 Kubernetes 里可见、可记账。
server 里有 30 余处依赖“节点后面有一个 agent”，agent 承担探测、镜像、模型文件、健康检查、sleep、日志、终端与隧道。

## 决策

1. **agent 以 DaemonSet 运行，新增运行时 `kube`**：实例是钉在本节点的裸 Pod（`spec.nodeName`、`restartPolicy: Never`、
   host 网络、hostPath 同路径挂载、ownerReference 指向 agent 的 DaemonSet）。server 下发的期望状态不变，不知道执行载体
   是 Pod；与 Mac 的 host 运行时（ADR 0044）同构。不做 agentless 虚拟节点，不做 Operator / CRD。
2. **集群接入令牌换节点 token**：独立的 `POST /api/v1/kube/enroll`，按 `(集群, 节点名)` 幂等，首次使用时绑定集群
   （`kube-system` namespace 的 UID）；hub 握手不变。server 不持有 kubeconfig、不连 apiserver（2026-10-22 由 [ADR 0063](0063-apply-to-cloud-clusters.md) 修订：从云账号接入的集群，经用户确认后用云厂商现发的短期 kubeconfig 应用 manifest，不保存）。
3. **每张卡一个分配者**：有 device plugin 时卡归 kubelet，gpuworks 为要用的卡起只申请 1 张卡的占位 Pod。device plugin
   不接受指定设备，所以借卡时一次为全部空闲卡各起一个占位 Pod（kubelet 没有选择余地），经 kubelet PodResources 认出
   每个占位 Pod 拿到的卡，退回不需要的；最后一个实例停止超过集群的空闲时间（默认 60 秒，2026-10-20 由 600 秒改定；-1 常驻）后归还。分给其他
   Pod 的卡作为外部占用上报，沿用“卡被平台外占用则不调度”的规则，调度器不改。
4. **只依赖标准库**：apiserver 用最小 REST 客户端（in-cluster ServiceAccount），PodResources 用标准库 h2c 与手写
   protobuf 解码；镜像操作委托 `k8s.io` 命名空间的 nerdctl。不引 client-go 与 gRPC。
5. **设备经 agent 自带的 device plugin，命令进节点的挂载命名空间**（2026-10-20 补充）：实例要的设备节点沿用各厂商
   docker / nerdctl 路径已验证的注入（`--device` 列表与驱动目录挂载）。agent 向 kubelet 注册每卡一个资源
   `gpuworks.ai/accelerator-<卡号>` 与各卡共用节点的 `gpuworks.ai/accelerator-shared`，实例 Pod 申请它要的，kubelet 经 Allocate 把设备
   节点交给容器；不要 privileged，不依赖厂商容器运行时与厂商 device plugin。按卡分资源名是因为 Allocate 不带 Pod 身份，
   只有资源名能指定是哪张卡。曾试过 Pod 注解 + CDI：kubelet 不把 Pod 注解交给容器，containerd 只认 device plugin 给的
   CDI，真机上设备并未注入（PPU 实例 `Failed to infer device type`）。agent 经 hostPID 以 `nsenter -t 1 -m` 在节点挂载
   命名空间运行 nerdctl、mount 与厂商 CLI：containerd socket、`certs.d`、exec 的 FIFO、NFS 挂载、厂商 CLI 依赖的 C 库都
   在节点自己的路径与版本上（PPU SDK 需要 glibc 2.38，agent 镜像是 2.36）。NVIDIA 的 CLI 由 NVIDIA 运行时注入：集群
   条目指定 RuntimeClass 时 agent 在其下运行；不指定时（默认）agent 容器用节点的默认运行时，托管 Kubernetes 的默认运行时就是
   NVIDIA 的，注入了 `nvidia-smi`，没注入时（k3s 默认 runc）agent 在节点上运行它；实例 Pod 用集群的 `nvidia` RuntimeClass，
   没有就用默认运行时（这几句关于 agent 运行时与 `nvidia-smi` 的做法已由 2026-11-12 修订取代：agent 一律在默认运行时下运行，`nvidia-smi` 的查找见修订）。kubelet 用的 containerd 按 agent 容器的 shim 的 `-address` 认定，不按 socket 位置猜。
   一个集群条目一个厂商（已由 2026-11-12 修订取代：条目没有厂商）。device plugin 以非整卡分配（time-slicing、MIG、没有 HAMi 登记的 vNPU）时不借卡，卡报为外部占用并带原因；HAMi 与 ACK 共享 GPU 见第 7 条。
6. **生命周期跟随 Kubernetes**：节点 cordon 时平台同样 cordon；离线 15 分钟的节点经同集群在线 agent 确认已不存在且无实例时
   移除；节点自报引擎端口段，避开 NodePort 范围。升级见第 8 条（2026-11-12 修订，此前是把 DaemonSet 换到与 server 同版本的镜像）。
7. **借卡方式随节点上的分配者而定**（2026-11-10 补充）：HAMi 与 ACK 共享 GPU 不是"一份一张卡"，按下面的办法借整卡，其余切分仍拒绝并写明原因（MIG、time-slicing、没有 HAMi 登记的 vNPU 模板）。
   - HAMi（节点有 `hami.io/node-*-register` 注解）：占位 Pod 写 `schedulerName`（默认 `hami-scheduler`，环境变量 `GPUWORKS_KUBE_HAMI_SCHEDULER` 覆盖）与 `nodeSelector kubernetes.io/hostname`，不写 `nodeName`（绕过调度器的 Pod HAMi 的 device plugin 不分配）；
     申请整张并按 UUID 指定，所以要哪张借哪张，不再先借全部再退。卡的归属以 HAMi 写在占位 Pod 上的 `hami.io/vgpu-devices-allocated` 为准并核对是整卡（kubelet 的设备 ID 是 `<UUID>-<份额号>`，不能按卡号解析）。
   - ACK 共享 GPU（有 `aliyun.com/gpu-mem` 资源，或标签 `ack.node.gpu.schedule` 为 `share` / `cgpu` / `core_mem`）：占位 Pod 申请一张卡的 GiB 数；没有指定卡的办法，借到哪张读 `scheduler.framework.gpushare.allocation`，沿用"借全部空闲卡、留要的、退其余"。
   - 外部占用来自同节点其他 Pod 上的分配注解（需要 agent 读本节点所有 namespace 的 Pod）；占位 Pod 被调度器留在 Pending 时，借卡失败并带调度器的原因与占着卡的 Pod。
   - 平台借着卡时节点换了模式，新调度器的账本里没有旧占位 Pod，会把同一张卡再分出去：这些卡报为外部占用并写明原因，不再按旧模式借卡，旧占位 Pod 在卡上没有实例后删除。
   - NVIDIA 的 HAMi 2.11 与 ACK 共享 GPU 的行为在阿里云 ACK 集群（k8s 1.36）上核对过；昇腾的 HAMi 只按 HAMi 文档的形态用夹具验证过，未上真机。
8. **镜像只是 agent 的运行环境，agent 在节点的状态目录里运行和升级**（2026-11-12 补充）：
   - 镜像的 tag 是运行环境的版本（`build/agent-image/REVISION`，如 `r1`），只有基础镜像或 nerdctl 变了才升；manifest 写的就是这个 tag，与 server 版本无关。
   - 容器启动时，镜像里的 agent 把自己拷到状态目录（hostPath，Pod 重建后还在）的 `agent/gpuworks-agent` 并 exec 它；同一镜像再启动时沿用目录里的那份，换了镜像就重新拷。新拷的那份在注册前先从 server 的 `/dl/` 取一次 server 自己的 agent，与普通节点安装时装 server 的版本一致。
   - 之后的升级就是普通节点的原地升级（推送或拉取二进制、exec、注册后确认、不确认就回滚到上一份），容器重启替代 systemd 当守护者；批量升级不再跳过这样的节点（agent 以能力 `upgrade.kube_binary` 声明）。
   - 运行环境版本变化时，manifest 渲染出新 tag，重新 apply 一次 DaemonSet 就换到新镜像；agent 不再有修改自己 DaemonSet 的权限（Role 对 DaemonSet 只有 get）。状态目录挂成 noexec 时 agent 继续运行镜像自带的程序、不声明这个能力，升级跳过它并说明原因。
   - 镜像发布到 Docker Hub、quay.io 与华为云 SWR；条目记录从哪个仓库拉（`agent_image_source`），从云账号接入时按地域预选（阿里云中国内地地域选 SWR），手动接入时由用户选，填了自己的镜像则以它为准。
9. **占位 Pod 与实例 Pod 的优先级由条目决定，默认不抢占**（2026-11-12 补充）：PriorityClass 按命名空间命名（`gpuworks-inference-<命名空间>`，同一集群的多个条目互不覆盖），agent 从 DaemonSet 的环境变量得知名字；数值默认 10000（此前固定 100000），默认 `preemptionPolicy: Never`。HAMi 与 ACK 共享 GPU 模式下占位 Pod 经调度器，平台看到的卡与调度器的账不一致时（例如读完分配到建占位 Pod 之间有 Pod 被调度上来），原先的默认会让调度器驱逐优先级更低的普通 Pod；现在只会等待并判借卡失败。PriorityClass 的数值与抢占策略在 Kubernetes 里不可修改，改了要删掉重建。

## 取舍

- **裸 Pod 而不是 Deployment**：重试、整组失败与排空已由 unit 对账负责（ADR 0029），再交给 Kubernetes 控制器会有两个
  对账循环争抢同一批 Pod。
- **host 网络**：端口分配、loopback 健康检查、隧道、serving 转发、NCCL/RDMA 选网卡全部原样可用；代价是实例不走 Pod
  网络与 Service。
- **借卡而不是关闭 device plugin**：不要求用户改动已有的 GPU Operator，k8s 账面如实；代价是借卡瞬间空闲卡会被短暂全部
  占用（其他 Pod 只会短暂 Pending），以及 time-slicing / MIG 副本的节点不能用。
- **privileged agent**：探测、NFS 挂载传播、进入节点挂载命名空间、注册 device plugin 需要；manifest 给 namespace 打 PSA `privileged` 标签。
- **二进制在状态目录而不是镜像里**（第 8 条）：升级不碰镜像仓库、不改 DaemonSet，私有仓库与离线集群不必每个版本推一次镜像，
  入口与普通节点统一；代价是 `kubectl` 里看到的镜像 tag 不说明 agent 版本（以平台显示为准），集群新加的节点仍要从仓库拉一次运行环境镜像。
  也考虑过由 server 经 agent 连接把新版本镜像分发进各节点的 containerd，再改 DaemonSet：同样不碰仓库，但每次升级要给每个节点传一份镜像，比只传二进制重。
- **复用厂商 device plugin 而不是自带**：自带各厂商的 device plugin 就要求用户关掉厂商的；gpuworks 是集群的客人，
  沿用用户已装的插件，代价是只能按整卡借，集群自己已切分（MIG、time-slicing 等）的节点不能用；
  借到整卡后，gpuworks 自己的 dGPU 切分照常可用。

## 后果

- 新增持久化事实：`kube_clusters` 表；`nodes` 的 `kube_cluster_id`、`kube_node_name`、`kube_unschedulable`；网络清单
  快照的 `engine_port_range`。
- 新包：`internal/server/kubemanifest`、`internal/agent/runtime/kube`（含 `kubeapi`、`podresources` 两个叶子包）。
- agent 镜像（`build/agent-image`，debian-slim：NVIDIA 运行时挂进来的 `nvidia-smi`、宿主机的 `npu-smi` 都是 glibc
  程序；只 COPY，amd64 与 arm64 都不需要模拟即可构建）。发布 tag 为运行环境版本 `rN` 与 `rN-<server 版本>`。
- 新包：`internal/agent/kubeboot`（第 8 条的交接与对齐）；新增持久化事实：`kube_clusters` 的 `agent_image_source`（渲染时无从得知节点能访问哪个仓库）。
- 新增持久化事实：`kube_clusters` 的 `priority`、`allow_preemption`（第 9 条）。
- 新增持久化事实：`kube_clusters` 的 `vendor`（2026-11-12 修订后已移除）、`state_dir`、`api_server`（manifest 参数）。agent 用节点的 DNS
  （`dnsPolicy: Default`），不依赖集群 DNS；没有组件提供 Service 地址的节点（KubeEdge、OpenYurt 边缘节点）用
  `api_server` 直连 API server。

## 修订（2026-11-12）：零厂商配置

集群条目不再有厂商。此前的条目要选厂商，manifest 据此给 agent 挂载厂商专用的宿主机路径（驱动目录、厂商 CLI、设备节点）；这一修订取消该字段，由 agent 自己识别：

1. **条目没有厂商。** 创建请求没有 `vendor`，条目 DTO 没有 `vendor`（改为 `vendors:[{vendor, nodes}]`，已接入节点按厂商的计数，没有上报加速卡的节点 `vendor` 为空），表单字典没有厂商列表；`kube_clusters.vendor` 列删除。一个条目一个 DaemonSet，没有厂商专用挂载，agent Pod 在默认运行时下运行（不再有 agent 的 RuntimeClass）。RuntimeClass 设置只影响 NVIDIA 的实例 Pod；其他厂商的设备经 agent 自带的 device plugin 交给实例，不受它影响。
2. **agent 经宿主机根目录和宿主机挂载命名空间用厂商工具，不靠挂载。** agent 以 privileged、hostPID 运行，经 `/proc/1/root` 读宿主机的文件，用 nsenter 在宿主机的挂载命名空间里运行 `npu-smi`、`hy-smi`、`ppu-smi` 与 `nvidia-smi`，用宿主机自己的 C 库与驱动库。`nvidia-smi` 的查找顺序：宿主机根目录，`/run/nvidia/driver`（NVIDIA GPU Operator 的驱动容器），最后是 NVIDIA 运行时放进 agent 容器的那份（manifest 固定设 `NVIDIA_VISIBLE_DEVICES=all`、`NVIDIA_DRIVER_CAPABILITIES=compute,utility`，只在 NVIDIA 运行时下生效）。设备节点检查看宿主机实时的 `/dev`。
3. **不支持的节点待命。** Kubernetes 上支持 NVIDIA、昇腾、海光与 PPU。没有加速卡或是别家的卡（AMD、昆仑芯等）的节点上，agent Pod 照常运行但不注册，所以不会成为平台节点。选择器仍可选，只用来收窄平台使用的节点。
4. **一个节点只属于一个条目。** 同一个集群的两个条目选中同一个节点时，后来者注册被拒绝（403 `KUBE_ENROLL_REFUSED`，`reason: node_taken`，`entry` 为占着节点的条目名），直到先到者在该节点的 agent 离线超过 15 分钟，之后节点可以转到另一个条目。这条规则让改节点选择器可以安全地把节点从一个条目挪到另一个条目，也避免两个 DaemonSet 的 agent 争同一个节点的卡与状态目录。
5. **条目可编辑、可卸载。** `PUT /kube/clusters/{uuid}` 改除命名空间、server 地址与状态目录之外的设置；manifest 变了而集群里还是旧的，响应的 `reapply` 与（手动条目的）`commands` 说明怎么追上。RuntimeClass、引擎端口段与空闲归还不写进 manifest：server 在节点连上时、条目被修改后把它们发给在线的 agent（`kube.settings` 控制消息，引擎端口段随节点的网络配置），agent 原地生效，所以只改这三项时 `reapply` 为 false。manifest 只带 agent 启动所需的东西。卸载给出不依赖接入令牌的 `kubectl` 命令（`remove_commands`），云条目由 server 执行。改节点选择器后不再被选中的节点离线，15 分钟后由同条目另一个在线 agent 确认并自动删除记录。

原因：同一个集群常有多个厂商的节点，一个条目一个厂商迫使用户建多个条目并各配选择器与命名空间；厂商专用挂载（驱动库目录、CLI 路径、设备节点）随节点的发行版与驱动安装方式变化，错配时 agent 起得来却探测不到卡，是接入失败最常见的来源；让 agent 在宿主机上直接运行宿主机自己的工具，减少了这一类配置与失败面。

取舍：agent 对宿主机的依赖更重（hostPID、读宿主机根），但它本来就是 privileged；`nvidia-smi` 在容器里没有注入时多一层查找。

后果：持久化事实减少 `kube_clusters.vendor`（迁移删除该列）；`KUBE_CLUSTER_MIXED_VENDORS` 错误与云条目节点池须同一厂商的规则（ADR 0063）同时取消；节点页对 Kubernetes 节点只提供 Kubernetes 上支持的厂商切换。


## 修订（2026-11-12）：AMD 与昆仑芯

Kubernetes 上的厂商由 `accelerator.RunsOnKube` 决定，加入 AMD 与昆仑芯（整卡）。第 3 条的“待命”因此只对这几家之外的卡成立。做法沿用上一修订：设备节点经宿主机根目录（`lstat`、glob）检查宿主机实时的 `/dev`，`xpu-smi` 在宿主机的 `PATH` 目录与 `/usr/local/xpu/bin` 里找，AMD 的 ROCm 版本文件读宿主机根；昆仑芯的注入在驱动为 `kube` 时不写 `--runtime runc`（Pod 没有这个选项，它会变成不存在的 RuntimeClass `runc`）。AMD 的 device plugin 用卡的 PCI 地址给设备命名，所以 kube 运行时除 UUID 外也按 PCI 地址把设备 ID 对到卡。AMD 的 dGPU 切分不开放给 Kubernetes（它的运行时约束仍只有 Docker、nerdctl 与 udocker）。某张卡的设备注入失败时，该卡不提供，不再让整个节点的设备计划失败。
