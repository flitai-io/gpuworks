# 接入 Kubernetes 集群

**适用场景**：已有带 GPU 的 Kubernetes 集群，希望把其中一批 GPU 节点交给 gpuworks 部署模型，而不逐台安装 agent。

公有云托管集群（阿里云 ACK、DigitalOcean DOKS、Nebius mk8s）的逐步操作见 [接入公有云托管 Kubernetes](connect-managed-kubernetes.md)。

**前置条件**：

- server 已经启动，集群节点能访问 server 地址（agent 主动外连，不需要节点入站端口）。
- 集群使用 containerd（kubeadm、k3s、RKE2、microk8s、GPU Operator 环境都是）。CRI-O（OpenShift）与 Docker 节点不支持，
  agent 启动时会拒绝并写明原因。
- 节点上装好厂商驱动：Kubernetes 上支持 NVIDIA、昇腾、海光（DCU）、PPU、AMD 与昆仑芯，一个集群条目可以同时覆盖这几种厂商的节点，
  不需要按厂商各建一个条目（见下一节）。NVIDIA 节点的 NVIDIA 容器运行时可用：或者是 containerd 的
  默认运行时（托管 Kubernetes 如 DOKS、Nebius、ACK 都是），或者有名为 `nvidia` 的 RuntimeClass（k3s 会自动创建）；
  其他厂商只要求节点上装好厂商驱动，不需要厂商的容器运行时，也不需要厂商的 device plugin。
- 要挂 NFS 存储的节点装有 `mount.nfs`（nfs-utils / nfs-common）：agent 在节点的挂载命名空间里用它挂载。
- 能在集群里执行 `kubectl apply`，并允许一个 `privileged` 的 namespace（manifest 会给 namespace 打上
  Pod Security Admission 的 `privileged` 标签；Kyverno、Gatekeeper 一类的策略需另外放行）。

## 工作方式

一个集群以一个 DaemonSet 接入。被选中的每个节点上跑一个 agent，它用集群的接入令牌换取自己的节点 token，之后与
普通节点完全一样：调度、部署、网关、日志、终端都不区分。实例落成钉在该节点上的 Pod（`restartPolicy: Never`、
host 网络、模型目录以 hostPath 同路径挂载），`kubectl get pods -n gpuworks-system` 能看到每个实例；重试与重建仍由
gpuworks 负责，Kubernetes 不会把失败的实例拉起来。

每张卡同一时刻只有一个分配者。节点上装了 device plugin（`nvidia.com/gpu` 等）时，卡归 kubelet 分配：gpuworks 要用
哪张卡，就为它起一个只申请 1 张卡的占位 Pod（`gpuworks-hold-*`），实例全部停止并超过集群的「空闲归还」时间后删除
占位 Pod，把卡还给 Kubernetes。被其他 Pod 占着的卡在 gpuworks 里显示为外部占用，不会被调度。节点没有 device
plugin 时，gpuworks 直接使用节点上的卡。集群用 HAMi、ACK 共享 GPU 或其他方式把一张卡分成多份时，借卡的办法随之不同，
见下一节。

## 与集群里的其他业务共存

gpuworks 不接管集群的 GPU，只借用要用的卡，集群里原有的业务照常运行：

- **不改动集群已有的 GPU 组件。** 沿用集群已装的 device plugin、HAMi 或 ACK 共享 GPU，不需要关闭、替换或卸载它们。
- **两边的调度互相看得见。** gpuworks 用着的卡在 Kubernetes 里由占位 Pod 占着，kube-scheduler 不会再把它分给别的 Pod；
  别的 Pod 占着的卡在 gpuworks 里显示为外部占用，gpuworks 不会在上面放实例。
- **借卡只涉及实例所在的那个节点。** 占位 Pod 钉在该节点上，其他节点的卡不受影响。普通 device plugin 与 ACK 共享 GPU
  不接受指定哪一张卡，所以借卡时 gpuworks 为该节点当时空闲的每张卡各起一个占位 Pod，认出要的那张后立即退回其余的；
  这期间该节点上新来的 GPU Pod 会短暂 Pending。HAMi 可以指定卡，要哪张借哪张，没有这个过程。
- **借到的是整卡，卡上仍可用 gpuworks 自己的 dGPU 切分。** 一张借来的卡可以切给多个实例，与普通节点一致。不能用的是集群
  自己已经切分的节点（MIG、time-slicing 等，见下文的表）。
- **不用的卡会还回去。** 卡上最后一个实例停止并超过条目的「空闲归还」时间后，占位 Pod 被删除，卡回到 Kubernetes。

## 加速卡厂商：自动识别

集群条目没有厂商这一项。一个条目只有一个 DaemonSet，没有厂商专用的挂载，agent Pod 在默认运行时下运行（不给 agent 指定
RuntimeClass）；每个 agent 自己探测所在节点的加速卡：

- Kubernetes 上支持的厂商：NVIDIA、昇腾、海光（DCU）、PPU、AMD、昆仑芯（整卡；AMD 的 dGPU 切分在 Kubernetes 上不可用，见下文）。
- 节点没有加速卡，或者是别家的卡（上述以外的厂商暂不支持 Kubernetes）时，节点处于待命状态：agent Pod 照常运行，但不向
  server 注册，这样的节点不会出现在平台的节点页。
- 所以同一个条目可以覆盖含多个厂商节点的集群，不再需要一个厂商一个条目。节点选择器仍是可选项，用来只让平台使用其中一部分节点
  （例如只用部分 GPU 节点）。

agent 没有厂商挂载也能用到厂商工具：它以 privileged、hostPID 运行，经宿主机根目录（`/proc/1/root`）读宿主机的文件，并在宿主机的
挂载命名空间里（nsenter）运行厂商命令行工具 `npu-smi`、`hy-smi`、`ppu-smi`、`xpu-smi` 与 `nvidia-smi`，用宿主机自己的 C 库与驱动库。`xpu-smi`（昆仑芯）在 `PATH` 的常见目录与 `/usr/local/xpu/bin` 里找。AMD 不需要命令行工具：agent 读宿主机的 amdgpu sysfs 与 KFD，ROCm 版本读宿主机的 `/opt/rocm/.info/version`。
`nvidia-smi` 按这个顺序查找：宿主机根目录；`/run/nvidia/driver`（NVIDIA GPU Operator 的驱动容器）；NVIDIA 运行时放进 agent 容器的那一份
（manifest 固定设置 `NVIDIA_VISIBLE_DEVICES=all` 与 `NVIDIA_DRIVER_CAPABILITIES=compute,utility`，这两个变量只在 NVIDIA 的运行时下生效）。
设备节点是否存在，看的是宿主机实时的 `/dev`。

**NVIDIA GPU Operator 的 CDI 模式**（Operator 的默认）下，容器工具包把 `NVIDIA_VISIBLE_DEVICES` 当作 CDI 设备名读，卡号或
UUID 这样不带种类的名字会落到 `management.nvidia.com/gpu`，而那里只有 `all`，按卡号选卡的 Pod 起不来
（`unresolvable CDI devices management.nvidia.com/gpu=0`）。agent 创建实例 Pod 时经宿主机根目录读节点的 CDI 规格
（`/etc/cdi`、`/var/run/cdi`）：工具包处在 CDI 模式（有 `management.nvidia.com/gpu` 规格，且工具包配置不是 legacy 模式）时，
把选中的卡写成 NVIDIA device plugin 规格里按 UUID 命名的完整设备名（如
`k8s.device-plugin.nvidia.com/gpu=GPU-…`），Pod 只拿到这张卡；规格里找不到这张卡时，Pod 不创建，报错说明怎么处理
（让 NVIDIA 的 device plugin 以 CDI 运行，或把工具包设为 legacy 模式：`NVIDIA_CONTAINER_RUNTIME_MODE=legacy`）。

**一个 Kubernetes 节点同一时间只属于一个条目。** 同一个集群的两个条目选中了同一个节点时，后来的那个条目的 agent 注册被拒绝
（`KUBE_ENROLL_REFUSED`，原因 `node_taken`，并指出占着它的条目名），直到第一个条目在这个节点上的 agent 离线超过 15 分钟；
之后节点可以转到另一个条目（例如改了节点选择器之后）。其他拒绝原因：`unknown_token`（接入令牌未知或已吊销；已经注册过的 agent 保留自己保存的身份，
不受影响）、`other_cluster`（令牌属于另一个 Kubernetes 集群）。保存的身份记着发给它的条目，agent 只用自己条目发的那份：
几个条目共用默认的状态目录时，节点从一个条目转到另一个条目不会顶着前一个条目的身份连上来。

同一个 Kubernetes 集群接入两个条目时，**两个条目要用不同的命名空间**：manifest 里的对象按命名空间命名，同名命名空间下后应用的
会覆盖先应用的，前一个条目的节点就悄悄转到了后一个条目。可能属于同一集群、又用了同一命名空间的条目，列表里名称旁会标「命名空间重复」
（还没有节点接入的条目不知道是哪个集群，也算在内；已知属于不同集群的不标）。命名空间创建后不能改，删掉其中一个换个命名空间重建。

## 集群用 HAMi、ACK 共享 GPU 或其他切分时

借卡的办法按节点上跑着的东西自动选择，集群条目不需要配置：

| 节点上 | gpuworks 的做法 |
|---|---|
| 普通 device plugin（`nvidia.com/gpu` 一份一张卡） | 如上：占位 Pod 申请 1 张卡 |
| HAMi（节点有 `hami.io/node-*-register` 注解） | 占位 Pod 经 HAMi 的调度器指定并申请一整张卡，要哪张借哪张 |
| ACK 共享 GPU（节点有 `aliyun.com/gpu-mem` 资源，或 `ack.node.gpu.schedule` 标签为 `share` / `cgpu` / `core_mem`） | 占位 Pod 申请一整张卡的显存，借到哪张读调度器写在 Pod 上的分配注解 |
| MIG、time-slicing / MPS、没有 HAMi 登记的昇腾 vNPU 模板等其他切分 | 不借卡：节点的卡全部显示为外部占用，原因（MIG、time-slicing、vNPU 模板……）写在卡的占用信息里，不会在上面放实例；集群改回整卡分配后自动恢复 |

三种模式借到整卡以后，卡上的 dGPU 切分（NVIDIA HAMi-core、昇腾 vCANN）与普通节点完全一致：实例 Pod 不申请 HAMi
或 ACK 的资源，由 gpuworks 自己的 dGPU 把一张卡切给多个实例。PPU、海光与 AMD 的 dGPU 切分目前只在 Docker 等运行时上验证（PPU 仅 Docker；海光 Docker 与 udocker；AMD Docker、nerdctl 与 udocker），在 Kubernetes 节点上不可用，这些卡只做整卡部署。

**HAMi**：占位 Pod 不绑定节点，而是写 `schedulerName: hami-scheduler`、`nodeSelector: kubernetes.io/hostname=<节点>`，
申请 `nvidia.com/gpu: 1`、`nvidia.com/gpucores: 100`、`nvidia.com/gpumem-percentage: 100`，并用注解
`nvidia.com/use-gpuuuid` 指定卡的 UUID。HAMi 的 device plugin 只给经过 HAMi 调度器、带分配注解的 Pod 分配，所以借卡不能绕过调度器。
卡归谁以 HAMi 写在占位 Pod 上的 `hami.io/vgpu-devices-allocated` 为准，并核对确实是指定的那张、算力 100%、显存等于 HAMi 登记的整卡显存，
否则退回并报原因。其他 Pod 占着卡的一部分时，它们在 gpuworks 里显示为外部占用（Pod 名、显存、算力）；占位 Pod 因此一直 Pending 时，
借卡失败，错误里写调度器给的原因（例如 `CardInsufficientMemory`）和占着这张卡的 Pod。

- HAMi 调度器的名字默认是 `hami-scheduler`。集群里改过名时，给 agent 的 DaemonSet 设环境变量：
  `kubectl -n gpuworks-system set env ds/gpuworks-agent GPUWORKS_KUBE_HAMI_SCHEDULER=<调度器名>`。名字不对时占位 Pod 不会被任何调度器处理，借卡超时，错误里会提示这个变量。
- 昇腾的 HAMi 按 HAMi 文档的形态实现（申请 `huawei.com/<型号>: 1` 且不带显存，注解 `hami.io/use-<型号>-uuid`），目前只用测试夹具验证过，没有在真机上验证。
- 在阿里云 ACK 上安装 HAMi 要多处理两件事：（1）chart 生成的 kube-scheduler 用默认的 leader 租约 `kube-system/kube-scheduler`，它被 ACK 托管的调度器占着，
  HAMi 的调度器永远当不上 leader，扩展一直报 `Scheduler extender has not become leader yet`，单副本时设 `scheduler.leaderElect=false`；
  （2）Kubernetes 1.36 的 kube-scheduler 要 list / watch `resource.k8s.io` 的 `resourceclaims`、`resourceslices`、`deviceclasses`，
  chart 的 ClusterRole 没有这些只读权限，需要补上。

**ACK 共享 GPU**：只有 ACK Pro 集群支持。占位 Pod 用默认调度器、`nodeSelector` 选中本节点，申请 `aliyun.com/gpu-mem: <一张卡的 GiB 数>`。
一张卡的显存是节点的 `aliyun.com/gpu-mem` 除以 `aliyun.com/gpu-count`，并与标签 `aliyun.accelerator/nvidia_mem` 核对；
各卡大小不一致、数量与探测不符时不借卡并写明原因。这种模式没有指定卡的办法，借卡的办法与普通 device plugin 一样：为每张空闲卡各起一个占位 Pod，
借到哪张读 `scheduler.framework.gpushare.allocation` 里的卡号，留下要的，退回其余的。其他 Pod 的这个注解记着它们占的卡与 GiB 数，显示为外部占用。

**agent 需要的额外权限**：HAMi 与 ACK 共享 GPU 把谁占了多少记在占用者 Pod 的注解里，agent 要读本节点上所有 namespace 的 Pod（只读，
manifest 的 ClusterRole 里的 `pods` 的 `get` / `list`）。用旧 manifest 接入的集群重新 `kubectl apply` 才有这条权限；没有时只能看到 kubelet 给了哪些 Pod 卡，
看不到各占多少，借卡与归属判断不受影响。

**节点中途换了模式**：平台借着卡的时候，集群把节点从 `nvidia.com/gpu` 改成 ACK 共享（或装上 HAMi）时，新的调度器看不见旧的占位 Pod，
会把同一张卡再分给别的 Pod。gpuworks 发现后，这些卡在 gpuworks 里显示为外部占用，原因写明「借卡时由 A 分配，现在由 B 分配，
Kubernetes 可能把卡分给别的 Pod」，不再按旧模式借卡，也不在这些卡上放新实例；已经在卡上的实例不受影响，卡上没有实例后旧的占位 Pod 会被删除。
节点改回原来的模式后卡自动恢复。

实例要的设备节点（昇腾的 `/dev/davinci*`、海光的 `/dev/kfd`、`/dev/dri/*`、PPU 的 `/dev/alixpu*`、AMD 的 `/dev/kfd` 与 `/dev/dri/renderD*`、昆仑芯的 `/dev/xpu*` 与 `/dev/xpuctrl` 等）由 agent 自带的
device plugin 交给实例：每张卡一个扩展资源 `gpuworks.ai/accelerator-<卡号>`，各卡共用的控制节点是 `gpuworks.ai/accelerator-shared`，
实例 Pod 申请它用的卡与共用资源，kubelet 只把这几个设备节点给容器，实例不需要 privileged。哪些节点属于哪张卡取自厂商
本身的设备注入，与普通节点上的 docker / nerdctl 实例一致。需要插件之外设备的实例（例如 InfiniBand 的
`/dev/infiniband`）以 privileged 运行。

借卡用的资源按厂商不同：NVIDIA `nvidia.com/gpu`、昇腾 `huawei.com/Ascend910` 等、海光 `hygon.com/dcu`、PPU `alibabacloud.com/ppu`、AMD `amd.com/gpu`、昆仑芯 `kunlunxin.com/xpu`（百度智能云 CCE 上是 `baidu.com/xpu`）。AMD 的 device plugin 用卡的 PCI 地址（如 `0000:43:00.0`）给设备命名，gpuworks 按卡的 PCI 地址（不区分大小写，忽略 `0000:` 域前缀）把它对到卡上；昆仑芯的设备 ID 与卡的对应只在名字里带有卡的 UUID 或以卡号结尾时成立（目前只用测试夹具验证，没有在带 device plugin 的真机上验证）。某张卡的设备注入校验失败（设备节点不在、卡不健康）时，agent 在日志里记下原因并不提供这张卡，节点上其余的卡照常可用。

agent 用节点自己的 DNS（`dnsPolicy: Default`），与直接装在主机上的 agent 一样，不依赖集群 DNS；server 地址因此要是
节点能解析的地址，不能写集群内的 Service 名。agent 在节点的挂载命名空间里运行 nerdctl（拉取镜像、导入、终端）和 `mount`（NFS），所以 containerd 的 socket 与
镜像仓库配置（k3s 的 `registries.yaml` 生成的 `certs.d`）都按节点自己的位置使用，不需要配置。节点上同时跑着 Docker
自带的 containerd 时，agent 按自己容器的 shim 认出 kubelet 用的那一个。

## 接入

1. 在「Kubernetes」页点「接入集群」，填名称、server 地址（集群节点访问 server 用的地址），按需填节点选择器
   （例如 `nvidia.com/gpu.present=true`，留空表示所有节点）。没有厂商这一项：每个节点上的 agent 自己识别加速卡，集群里
   有多个厂商的节点时一个条目就够了；没有加速卡或厂商暂不支持的节点保持待命，不会成为平台的节点。
2. 「高级」里的选项：
   - 状态目录：agent 在节点上的目录，默认 `/opt/gpuworks`。节点上已经装着一个 gpuworks agent 时换一个目录（例如
     `/opt/gpuworks-kube`），两者互不干扰。
   - containerd socket：留空时 agent 自己找（containerd、k3s、RKE2、microk8s 的默认位置）；装在别处时填写。
   - API server 地址：只给节点上没有组件提供 Service 地址的集群用（没有 kube-proxy 或其替代品，例如 KubeEdge、
     OpenYurt 的边缘节点），写成 `https://host:port`，地址须在 API server 的证书里；留空时 agent 经集群的
     `kubernetes` Service 访问。
   - agent 镜像仓库：节点从哪里拉 agent 镜像，可选 Docker Hub（默认）、华为云 SWR、quay.io。中国内地的集群通常拉不到
     Docker Hub，选华为云 SWR。镜像的 tag 是 agent 运行环境的版本（如 `r1`），不随 server 版本变化，见下文「升级 agent」。
   - agent 镜像：填了就用你自己的镜像，不再从上面的仓库拉。节点访问不了公网时，把 `gpuworks/gpuworks-agent:r1` 推到集群能访问的
     私有仓库（或在每个节点上 `ctr -n k8s.io images import` 导入）后填这个地址；运行环境版本变化前都不用再推。离线环境的镜像归档见[离线环境部署](deploy-offline.md#kubernetes-集群的-agent-镜像)。
   - 镜像拉取凭据：agent 镜像放在需要登录的仓库（企业 Harbor、云厂商的私有仓库）时，先在命名空间里建一个
     docker-registry 类型的 Secret（`kubectl -n gpuworks-system create secret docker-registry regcred …`，namespace 不存在时
     先建），再在这里填它的名称。agent Pod 用它拉镜像，agent 起的占位 Pod 也沿用它。
   - RuntimeClass：只影响 NVIDIA 的实例 Pod，它们访问 GPU 用的 RuntimeClass；其他厂商的设备由 agent 自带的 device plugin
     交给实例，不受它影响。留空（默认）时每个节点自己判断：集群里有 `nvidia` RuntimeClass 就用它，没有就用节点的默认运行时。
     只有 RuntimeClass 叫别的名字时才需要填写。
   - 引擎端口段：默认 `40000-42767`。不要与集群的 NodePort 范围（默认 `30000-32767`）重叠：kube-proxy 会把发往本机
     地址的 NodePort 流量转走。
   - 空闲归还：借来的卡在最后一个实例停止后保留多久，默认 60 秒（每 30 秒检查一次，实际在 60 到 90 秒之间归还）；「常驻」表示一直不还，节点池整体交给 gpuworks。
   - 优先级：gpuworks 的占位 Pod 与实例 Pod 所用 PriorityClass（`gpuworks-inference-<命名空间>`）的数值，默认 10000，范围 0 到 1000000000，越大越优先；普通 Pod 是 0。它也决定节点内存、磁盘紧张时的驱逐顺序：优先级低的先被驱逐。
   - 允许抢占：默认关闭（PriorityClass 写 `preemptionPolicy: Never`），借卡放不下时占位 Pod 只会等待，调度器不会为它驱逐你的其他 Pod。开启后，HAMi 与 ACK 共享 GPU 模式下（占位 Pod 经调度器）调度器可能驱逐优先级更低的 Pod；普通 device plugin 模式的占位 Pod 与所有实例 Pod 直接绑定节点，不经调度器，不受这个开关影响。
3. 复制生成的命令在集群里执行：

   ```bash
   kubectl apply -f 'https://server.example/kube/manifest.yaml?token=…'
   ```

   被选中的节点会自动出现在节点页，带系统标签 `gpuworks.ai/kubernetes-cluster`（值是集群的 UUID，显示集群名）和 `gpuworks.ai/kubernetes-node`（Kubernetes 里的节点名），节点页可据此筛选，部署的节点条件也可以用它们限定在某个集群上；扩容出来的节点同样自动加入。创建 5 分钟后仍没有
   节点加入时，列表上的「排查」给出要执行的 `kubectl` 命令和常见原因（拉不到 agent 镜像、RuntimeClass 不存在、节点
   选择器没选中节点、agent 连不上 server）。

命令里带着集群的接入令牌，只给管理员看。吊销接入令牌后新节点不能再加入，已加入的节点不受影响。

列表里每个条目按厂商列出已加入的节点数（节点最多的厂商在前；没有上报加速卡的节点单独归为一类）。

## 修改集群条目

在「Kubernetes」页点条目的「编辑」（管理员，接口是 `PUT /api/v1/kube/clusters/{uuid}`）。可以改：名称、节点选择器、agent 镜像与镜像仓库、
镜像拉取凭据、RuntimeClass、containerd socket、API server 地址、引擎端口段、空闲归还、优先级与允许抢占，校验规则与创建时一致。
命名空间、server 地址与状态目录创建后固定，要改就删掉条目重建。从云账号接入的条目按节点池选节点，不能改节点选择器，
改它的节点池（见[接入公有云托管 Kubernetes](connect-managed-kubernetes.md)）。

保存只改 gpuworks 里的记录，manifest 随即按新设置渲染，**集群里跑的仍是旧的，直到再应用一次**：

- 手动接入的条目（没有吊销）：保存后页面给出一行 `kubectl` 命令，先删条目的 PriorityClass 再应用 manifest
  （`kubectl delete priorityclass gpuworks-inference-<命名空间> --ignore-not-found && kubectl apply -f ...`）：Kubernetes 不能原地修改
  PriorityClass 的数值与 `preemptionPolicy`，只能删了重建；之前改过优先级却没应用时，单独的 `kubectl apply` 会因此失败，
  所以每次都带上删除。写成一行是为了两步之间不留没有 PriorityClass 的空档。
- 从云账号接入的条目：点「应用到集群」，server 自己应用；PriorityClass 变了时由 server 先删后建（应用结果里这一项的动作是 `replace`）。
- 空闲归还时间也写在 manifest 里（DaemonSet 的 Pod 模板带注解 `gpuworks.ai/idle-return-seconds`），所以改了它同样要重新应用：
  agent 随之重启，在启动注册时取到新值。agent 重启不会重启实例 Pod。

**改节点选择器（云条目是节点池）**：重新应用后，DaemonSet 不再在不被选中的节点上运行，这些节点的 agent 退出，节点离线；
离线 15 分钟后，平台请同一个条目里另一个在线的 agent 确认：该节点在 Kubernetes 里是否还在、是否还运行着本条目的 agent Pod。
连续两次确认（约隔 5 分钟，避开滚动更新时 agent Pod 删了还没重建的空档）都不是、且节点上没有实例时，节点记录自动删除。条目里没有别的在线节点时没有谁可问，不自动删除，需要在节点页手动删除这个节点
（删除 Kubernetes 节点不需要 SSH）。新选中的节点自己加入。

**缩小选择范围要确认**：改节点选择器（云条目是改节点池）会让某些节点退出选择、而这些节点上还有实例在运行时，server 拒绝保存，回 409
`KUBE_SELECTION_DROPS_INSTANCES`，页面弹出确认框，列出这些节点和它们上面的部署。再次应用 manifest 时这些节点上的 agent 会被移除；
实例继续运行、占着卡，但不再受平台管理、不能经网关访问，也不会被归还。请先停止或迁走这些部署，或者点「确认」仍然保存（请求带
`confirm_drop_instances: true`）。server 不知道集群里节点的标签，所以请条目里一个在线的 agent 读取这些节点、按新的选择判断是否仍被选中；
问不到任何 agent（条目里没有在线节点，或 agent 版本太旧）时，把所有还有实例的节点都列出来，确认框注明无法核实、这些节点可能受影响
（参数 `verified` 为 false）。选择没变、只是放宽，或退出的节点上没有实例时不询问。保存本身仍只改 gpuworks 的记录，集群要等再次应用 manifest 才变。

## 卸载

条目的「卸载」给出把它的 agent 从集群里移走的 `kubectl` 命令（管理员可见，吊销过的条目也有），按顺序执行：

```bash
kubectl -n <namespace> delete daemonset gpuworks-agent --ignore-not-found --wait
kubectl delete clusterrolebinding gpuworks-agent-<namespace> --ignore-not-found
kubectl delete clusterrole gpuworks-agent-<namespace> --ignore-not-found
kubectl delete priorityclass gpuworks-inference-<namespace> --ignore-not-found
kubectl delete namespace <namespace> --ignore-not-found
```

删 DaemonSet 时实例 Pod 与占位 Pod 一起被删除，卡还给 Kubernetes。这些命令不需要接入令牌；`kubectl delete -f <manifest 地址>`
则要用令牌取 manifest，吊销令牌后就不能用了。server 够得着集群的条目（从云账号接入）由 server 代为执行（「从集群卸载」，先检查、确认后删除）。
节点都从节点页消失后才能删除条目（还有节点时拒绝，`KUBE_CLUSTER_HAS_NODES`）。

## 日常运维

| 场景 | 做法 |
|---|---|
| 维护节点 | 在 Kubernetes 里 `kubectl cordon`：gpuworks 在下一次状态上报（一分钟内）跟着停止往该节点放新副本，`uncordon` 后同样恢复；节点的调度状态显示为平台停止调度，原因为「Kubernetes 已停止向该节点调度」 |
| 升级 agent | 与普通节点一样：节点页勾选后点「批量升级 Agent」，或在节点详情里点「升级 Agent」。agent 运行在节点状态目录里的 `agent/gpuworks-agent`，升级原地替换这份程序，不改 DaemonSet、不访问镜像仓库，实例 Pod 不受影响；升级后的程序在 Pod 重建后仍在。运行环境版本（镜像 tag）很少变化；某个版本升了它时，发布说明会写明，重新 apply 一次 manifest（`kubectl apply` 或「应用到集群」）即可。`kubectl` 里看到的镜像 tag 是运行环境版本，不代表 agent 版本，以节点页为准 |
| 节点被删除或缩容 | 节点离线 15 分钟后，gpuworks 请同集群另一台在线节点确认；Kubernetes 里已没有该节点且其上没有实例时自动移除 |
| 从 gpuworks 删除节点 | 只删除记录，不卸载 agent；DaemonSet 仍选中该节点时它会重新加入，需要先调整节点选择器 |
| 修改节点选择器或节点池 | 见上文「修改集群条目」：重新应用后，不再被选中的节点离线，15 分钟后自动清理；新选中的节点自己加入 |
| 卸载 | 条目的「卸载」给出 `kubectl` 命令（见上文「卸载」）：DaemonSet、实例 Pod、占位 Pod 一起删除，卡还给 Kubernetes |

## 限制

- 只支持 containerd：镜像由 agent 经节点的 containerd（kubelet 的 `k8s.io` 命名空间）拉取与导入，不经过 kubelet，
  所以实例镜像不用 Pod 的 `imagePullSecrets`，私有仓库的凭据配在 gpuworks 的镜像来源里（集群条目的镜像拉取凭据只管
  agent 镜像）。
- 实例使用 host 网络，不创建 Service / Ingress，经 gpuworks 网关访问。
- 厂商的命令行工具（`npu-smi`、`hy-smi`、`ppu-smi`、`xpu-smi`、`nvidia-smi`）在节点的挂载命名空间里运行，用节点自己的 C 库与驱动库，所以 agent
  镜像与节点的发行版无关；查找顺序见「加速卡厂商：自动识别」。
- 只支持 NVIDIA、昇腾、海光、PPU、AMD 与昆仑芯的节点；其他厂商的节点保持待命，不成为平台节点。AMD 与昆仑芯只做整卡部署：AMD 的 dGPU 显存切分在 Kubernetes 上不可用，昆仑芯本来就没有 dGPU。
- agent 以 privileged、hostNetwork、hostPID 运行（探测、经节点挂载命名空间运行 nerdctl 与 mount 需要）。
