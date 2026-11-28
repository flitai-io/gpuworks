# gpuworks

轻量 GPU 集群管理平台：server（控制面）+ agent（GPU 节点），模型部署与推理网关。本文件是领域词汇表。

## Language

### 测试体系

**Sim 测试（L1） — Simulation Test (L1)**:
单个 `go test` 进程内跑真实 server 与真实 agent 代码，注入 fake GPU 探测器与 fake 容器运行时；零外部依赖，`make test` 默认全跑。测试体系的重心。
_Avoid_: 集成测试、e2e（不加层级前缀的笼统叫法）

**多进程测试（L2） — Multi-process Test (L2)**:
用真实编译出的 server/agent 二进制、1 server + N agent（各自独立 state dir）跑的测试，验证真实进程边界（断线重连、升级等），GPU 仍为 fake。
_Avoid_: 双进程测试

**容器测试（L3） — Container Test (L3)**:
需要本地 docker 的测试，用 fake 模型与 fake 引擎镜像验证真实下载、镜像拉取、容器挂载与参数。

**云 GPU 测试（L4） — Cloud GPU Test (L4)**:
通过云厂商 API（runpod/compshare）临时租用真实 GPU 机器跑的测试，含真实引擎；由 go test 自包含编排（租机→接入→断言→销毁）。

### dGPU

**dGPU — dGPU**:
dynamic GPU 的缩写，产品对「GPU 切分」功能的唯一叫法：把整张物理加速卡呈现为带显存或算力限额的逻辑资源。UI 与用户文案统一用 dGPU，不再出现 HAMi、vGPU、切分、显存切分等历史叫法；实现名只出现在工程文档与代码里，写上游项目名而不是自造简称：NVIDIA 用 HAMi-core（`libvgpu.so`）；昇腾用 openEuler ubs-virt 的 vcann-rt（`libvruntime.so`，软件层逻辑限额）；PPU 用 ppu-slicing-shim（`hggc_quota.so` + `hgml_dlsym_hook.so`，拦截 PPU 的 HGGC 运行时与 HGML 管理库）；AMD 用 rocm-slicing-shim（`libvrocm.so`，HIP 运行时层拦截）；海光用驱动原生 vdev（可选 helper 经 hcu-dcgm 创建）。HGGC 是 PPU 的运行时（对应 CUDA），不是切分实现；平台不使用昇腾 vNPU（`npu-smi` 按模板的驱动层切分）。
_Avoid_: vGPU、HAMi、切分、显存切分、虚拟化（把逻辑限额误写成虚拟设备）；工程文档里的 vCANN（不是公开名称，指 vcann-rt）、「PPU HGGC 切分」（HGGC 是运行时）、vNPU（未使用）

**dGPU 后端 — dGPU Backend**:
一种把整张物理加速卡呈现为带显存或算力限额的逻辑资源的实现合同。后端描述适用硬件、成熟度、所需资产、注入合同、展示信息和实例清理要求；是否允许某次部署仍由平台准入规则决定。
_Avoid_: partitioner（容易误指一次切分动作）、把准入策略叫后端能力

**dGPU 能力 — dGPU Capability**:
某张物理加速卡在当前节点事实下可选择的 dGPU 后端集合；每项只有 ready / unavailable 两种状态，并附兼容范围或不可用原因。除豁免行为探针的后端（NVIDIA、AMD）外，ready 的唯一依据是[[dGPU 探针]]通过（豁免后端只看运行时约束与资产，由 `accelerator.SkipsBehaviorProbe` 声明）；版本/平台组合只产生兼容性警告，不产生 unavailable。它是 dGPU 可用性的唯一事实源，节点级展示只是对各卡能力的汇总。
_Avoid_: `partition_mode`（已删除的单值推断）、从厂商名、MIG 状态或健康状态反推 dGPU 能力、用版本组合表作硬门（探测缺口 × 硬门 = 假阴性）

**dGPU 探针 — dGPU Probe**:
agent 用内嵌的极小探针镜像在本节点自动执行的容器级端到端验证：走完整注入合同，断言容器内可见显存等于配额，秒级完成。通过是（未被豁免的后端）dGPU 能力 ready 的唯一依据；节点上线或 dGPU 资产变化时自动重跑，结果缓存并随节点事实上报。
_Avoid_: 加载探针（dlopen 级验证，已被容器级探针吸收）、把资产文件存在当作已验证、健康检查（那是运行中的探活）

PPU 的宿主 `HGGC Runtime Version=N/A` 不表示容器路径不可用：普通 Docker/runc 可显式注入设备，业务镜像携带 HGGC，探针只读挂载驱动侧 HGML/UKI 后实测配额。因此宿主版本只能记录，不能作为 probe skipped 或 capability unavailable 的先验。

**芯片族 — Chip Family**:
昇腾芯片的家族分档（如 910B、310P），决定 dGPU 资产的适配范围，参与资产选择与兼容警告，不作硬门。它与[[架构身份]]的架构代际取值相同、用途不同：芯片族只服务 dGPU 资产选择，不是调度与估算读的那个事实。
_Avoid_: 芯片家族、把芯片族当 ABI 硬约束

**GPU 型号名 — GPU Model Name**:
入库前经 `accelerator.NormalizeGPUName` 规范化的卡型号：去掉开头的厂商词（`NVIDIA `/`Ascend ` 等）、首尾空白去掉、中间空白合并。它是 server 侧唯一的型号写法，推理目录的名字谓词、PD 同型号检查、系统标签 `gpuworks.ai/gpu-model`、界面都读它；原始名字只留在 agent（`doctor`、日志）与已有 benchmark 快照里。
_Avoid_: 在读取方各自去前缀、按驱动版本的两种写法分支、改写历史 benchmark 快照

**架构身份 — GPU Architecture Identity**:
一张卡的架构，由 `accelerator.GPUArch` 从存储的原始事实（厂商、`arch_id`、compute capability）读时算出，含两部分：**arch id** 是最具体的身份（NVIDIA 的 `sm_XY`、昇腾 `910B4`/`910C`、海光 `K100AI`、PPU `ZW810E`），**架构代际**是厂商内的世代、小写（`turing`/`ampere`/`ada`/`hopper`/`blackwell`、`910b`/`910c`/`310p`、`k100`/`bw1000`、`beethoven`），认不出为空。只存原始事实：NVIDIA 存 compute capability（`sm_XY` 由它推导、不入库），其他厂商在 `arch_id` 存能确定的最具体身份（海光只报系列时存系列级 id，不留空）。dtype 支持表、显存预留估算、芯片匹配、调度、API DTO 的 `arch_family`/`arch_id` 与界面都调同一个函数。
_Avoid_: 把代际落库（曾经的 `gpus.arch_family` 列）、读取方各自从名字猜代际、信任 agent 上报的 `arch_family`（server 忽略该字段）

**资产清单 — Asset Report**:
agent 对某个 dGPU 后端实际观察到的资产目录和文件名快照，只陈述事实，不声明 ready。一个可执行资产包要求全部必需文件位于同一目录。
_Avoid_: readiness report（是否就绪是 server 按当前策略派生的结论）、跨目录合并文件

**实验性后端 — Experimental Backend**:
已满足公开兼容合同、允许显式选择，但验证范围和运行承诺仍受限的 dGPU 后端。实验性是成熟度，不是第三种可用状态；实验性后端同样只有 ready / unavailable。
_Avoid_: 强制启用、风险确认开关（界面提示不改变能力事实或准入结果）

**fake 模型 — Fake Model**:
以真实模型（Qwen3-4B）的 config/tokenizer/README 为基础、权重替换为重新生成的合法小 safetensors 的测试模型（如 `zstackai/qwen3-4b-fake`）。文件合法可解析，仅数值无意义。
_Avoid_: 假模型、截断模型

**fake 引擎 — Fake Engine**:
仓库内维护的静态编译小程序，模拟 vLLM/SGLang 的 CLI 参数接收与 HTTP 接口（/health、/v1/*、sleep/wake），行为可经环境变量注入（慢启动、启动即崩等）；打包为极小容器镜像供 L3 使用。
_Avoid_: fake vllm 镜像（当指代程序本身时）

### 模型来源与下载

**版本指定 — Revision**:
模型来源仓库的版本指定。空 = 跟随来源平台的默认分支（由平台解析，HF 通常 main、ModelScope 通常 master 但均非绝对），是常态；非空 = 用户显式钉住的分支/tag/commit，属高级选项。
_Avoid_: 把 `main` 当跨平台默认值回填

**commit 钉住 — SHA Pinning**:
下载开始时把列文件响应中的 commit sha 作为本次下载全部文件的统一 revision，保证整个模型目录原子地对应同一 commit。
_Avoid_: 逐文件各自解析默认分支

**获取 — Acquisition**:
权重和镜像到达节点的过程，由节点在副本启动时按[[拉取设置]]自己决定：本地已有则直接用，同步组内有在线节点持有则复制，否则按候选拉取或下载。部署不选择来源，只带 `model_source: {storage_id, provider}`（模型保存到哪个存储、运行哪家权重）；evaluate 对请求指定的节点给出获取预估（`present`/`copy`/`pull`…），预估不是选择。
_Avoid_: 下载方式（漏掉“本地已有则不下载”这一支）、把获取说成部署的选项

**节点间同步 — Peer Sync**:
从已有完整模型或镜像的在线节点直传到目标节点。server 只协调端点和一次性凭据，不中转字节。有两种触发：副本启动时的自动复制，只发生在同一[[同步组]]内，失败则目标自己拉取或下载；在模型页或镜像页手动发起，显式选定的源节点是唯一候选、不受同步组限制，不可用时失败而非静默改用其他节点。
_Avoid_: 对等同步、节点同步（与多机推理的节点通信混淆）

**拉取设置 — Pull Settings**:
管理员对「节点从哪里拉镜像、能从谁那里复制」的配置，由[[来源规则]]、[[同步组]]和仓库凭据组成，所有拉取路径（启动、期望状态重放、扩副本、多机预拉取、镜像页「拉取到节点」）共用同一份候选（ADR 0048）。
_Avoid_: 把它当部署参数

**来源规则 — Pull Source Rule**:
匹配一个仓库 host 或引用前缀，给出有序替代地址，可关闭回源、可按节点标签限定范围。语义同 containerd `hosts.toml`：先试替代地址，最后回到原地址（除非关闭回源），拉到后在本地 tag 回原名；只在 agent 侧「拉另一个地址」实现，不改宿主机运行时配置，所以只作用于平台发起的拉取。对所有镜像生效，包括模板声明的来源。
_Avoid_: 镜像站、加速地址（规则可以指向任何仓库）、改写规则（节点网络的 `rewrites` 只重定向下载 URL）

**同步组 — Sync Group**:
一组节点（由节点条件选择）。节点只自动从同组节点复制镜像与模型；默认有一个包含全部节点的组，不属于任何组的节点不自动复制，多组取并集、限速取最严。同步组表达「连得上但不该走」，连不通靠快速失败加 3 小时冷却处理。
_Avoid_: 网络分区（它不描述网络拓扑）

**模型 HTTP 目录来源 — Model HTTP Directory Source**:
**模型**的一类来源（`source=url`），与容器镜像无关：模型权重由一台普通 HTTP 文件服务器的目录列表页提供，目录地址存在 `Model.SourceURL`，`source_id` 仍是仓库 ID（默认取地址末两段）。它没有 revision，凡需要版本固定的地方都退化为“指向不可变目录”的运维约定；列表页给出的大小只是显示值，只有下载响应与完成标记里的实测字节数算准。
_Avoid_: 把 autoindex 目录当 Hugging Face API 兼容镜像、用列表页大小做磁盘准入、把 `source_id` 当成 URL

**存储 — Storage**:
模型文件所在的地方，是一个对象而不是节点属性。类型决定权威的模型文件在哪、节点怎么读到它：本地存储（每节点各自的目录）、NFS（agent 挂载，所有节点共用一份）、NFS 加本地缓存（权威一份在 NFS，节点先缓存到本地再启动）、已挂好的共享目录。停用即只读。见 ADR 0034。
_Avoid_: 存储策略（旧称，那时它只是一个下载开关）、模型目录（指节点上的一个具体路径）

**模型文件 — Model File**:
一个模型在某个存储里的那份文件，对应 `model_files` 的一行，按 (模型, 存储, 节点) 唯一：本地存储每节点一行，共享存储只有一行（节点为 0），NFS 加本地缓存的节点缓存是同一存储下带节点的行。
_Avoid_: 副本（那是部署里的服务副本）、模型拷贝

**位置查询 — Model Location**:
"模型 M 在节点 N 上从存储 S 读哪个宿主机路径、现在能不能读"的唯一回答（`store.ModelLocation`）。共享存储的宿主机路径由节点自己的挂载点加存储内相对路径拼成。
_Avoid_: 直接读 `ModelFile.Path` 当挂载源

**写入节点 — Writer Node**:
往共享存储写入一个模型文件（下载或复制）时执行写入的那台节点；先写到 `.gpuworks/incoming/<控制面 ID>/` 再改名到最终路径。

**控制面心跳 — Control Plane Heartbeat**:
每套 gpuworks 在共享存储的 `.gpuworks/control-planes/` 下写的心跳文件，用来提醒"还有别的 gpuworks 在用这个存储"；只是提醒，不是锁。

### 镜像

**镜像系列 / 镜像版本 — Image Series / Image Version**:
两级目录模型。系列 = 一个发行流（如 vLLM、SGLang Ascend、`lmsysorg/sglang-rocm`），版本 = 系列内一个具体可拉取的 ref（含 tag）。三类来源共用此两级模型。
_Avoid_: 镜像（不区分层级的笼统叫法，需说清指系列还是版本）

**三类来源 — Source Types**:
系统镜像（wire/库枚举 `official`，由[[镜像目录]]定义、元数据只读、不可删除）/ 社区镜像（`community`，用户从社区仓库挑 tag 添加）/ 自定义镜像（`custom`，用户填任意 ref）。同一模型上的标记差异，不是三种实体。界面一律叫「系统镜像」，枚举值 `official` 只留在 wire 与数据库。
_Avoid_: 三种镜像表、在界面或文档正文里把系统镜像叫「官方镜像」

**镜像体积 — Image Size**:
目录版本的 `size_bytes` 是旧 manifest 声明的归档体积；节点的 `SizeBytes` 仍承载传输归档的预期/完成字节。两者都不能推导本地运行时体积。`ImageNode.RuntimeSizeBytes` 是可空的独立原始观测，目前来自 Docker inspect 的 `Size`：包含镜像所有层，containerd image store 下包含本地内容和解包快照。页面汇总仅累加 present 副本最近上报的已知值，同时列出未知副本数；共享层按镜像重复计入，不代表唯一磁盘占用或可回收空间。nil 为未知，0 为实测零；旧 agent、不支持的运行时、成功但无体积的上报均为未知，完整探测失败保留上一次观测。
_Avoid_: 用归档体积乘副本数冒充本地体积、用未知值填零、把逐镜像体积之和称为磁盘使用量

**镜像目录 — Image Catalog**:
定义系统镜像全集的 manifest 文件（schema 4，兼容导入 2、3），由 gpuworks-runner 构建矩阵生成。内嵌种子（server 换了构建后的首次启动重放）、「检查更新」（从[[发布通道]]取）、「上传 manifest」三条入口走同一导入规则；导入即对系统行全量对账，最后一次导入生效（ADR 0040）。`catalog_version` 只作展示与等值比较，不做新旧比较。目录中的 `arch` 表示实际支持的架构，`auto_select` 只限制自动选用范围（省略表示全部支持架构，`manual` 表示仅手选）。`registry_digest` 是发布校验时预期的 registry 顶层 manifest 摘要。
_Avoid_: 镜像列表(那是查询结果,不是权威定义)、离线镜像包（已删除，目录只是一个 JSON）

**推理模板目录 — Inference Catalog**:
定义原始模型及其全部可部署档位的多文件目录包；数据库只是该目录的投影。

**规范引用 / 有效引用 — Canonical Ref / Effective Ref**:
canonical = 目录记录与 presence 记账用的唯一引用(强制带 tag,拒 digest);effective = 经[[来源规则]]或镜像自己的副本得到的、实际去拉的地址,拉完 `docker tag` 回 canonical。实例 spec 的镜像若是 digest 引用(`repo@sha256:…`,模板常见),运行时无法给 digest 引用打 tag,所以服务端给出的拉取候选(含来源规则的替代地址、已在本地的候选副本)都被接受,不 tag,直接以拉到的候选引用启动容器;候选由服务端(模板声明、来源规则)负责,节点不核对其 digest 是否与 spec 一致。
_Avoid_: 镜像地址(不区分二者)

**模板镜像来源 — Template Image Source**:
推理模板 spec 的 `image_sources` 列出的、同一个模板镜像可以拉取的更多 registry 引用，和 `weights.sources` 同构：作者声明，是镜像自己的等价副本，由节点探测可达性排序，部署者不选。只在部署用的镜像等于 spec 的 `image` 时存在，只语法校验、不要求钉同一个 digest；[[来源规则]]对它同样生效，目录 `registry_sources` 只属于解析得到[[镜像目录]]行的系统镜像（ADR 0038、0048）。
_Avoid_: 把模板来源登记进镜像目录来换取国内可达、把它叫成「镜像站」(镜像站是[[来源规则]]里管理员写的替代地址)

**容器镜像归档地址 — Container Image Archive URL**:
**容器镜像**的获取地址，与模型下载无关：[[镜像版本]]「获取地址」里的一类条目，是一个 `http(s)://` 地址，指向 `docker save` 归档。是不是归档地址只由 `pullref.IsArchiveURL` 判定；它只会被下载并 load，永远不进拉取命令。归档里烘焙的引用与 canonical 不同时，节点把 canonical 补打上去，真实引用记进节点行的消息。
_Avoid_: 把归档地址当 registry 引用、靠文件名推断镜像引用

**容器镜像管理节点中转 — Container Image Management Relay**:
**容器镜像**的一条获取路径，与模型的[[节点间同步]]不是同一件事：归档由管理节点下载并流式推送给目标节点，服务端不落盘。用于节点自身访问不到归档地址的场合；传输不可续传，也没有跨节点去重。直连（节点自己下载归档）是另一条路径，两者的节点行来源标签不同。
_Avoid_: 把它与[[节点间同步]]混为一谈（后者传的是节点上已有的模型，不是从源地址取的镜像）

**镜像内置配置 — Image Built-in Configuration**:
镜像制品自带的 ENTRYPOINT、CMD、ENV 与声明卷，是运行事实：容器运行时直接使用，平台只在节点检查或实例启动事实里展示，不另存、不当作部署配置。镜像目录不承载任何启动配置（ADR 0031）。
_Avoid_: 镜像默认值 / Image Defaults(已删除的目录可编辑层)、RunSpec(旧称)、把 inspect 结果写回目录

**环境包 — Environment Package**:
Mac 节点用的镜像：darwin/arm64 的 OCI 镜像，内容是一个可搬迁的根（独立 Python、装进其中的引擎、`/bin` 入口），由 host 运行时解包后以宿主机进程运行。它在镜像目录、拉取、归档导入与 canonical 记账上与容器镜像完全相同，按 `gpuworks-runner:metal…` 发布（ADR 0044）。
_Avoid_: 把它叫 venv、conda 环境；要求用户 brew/pip 安装引擎

**容器覆盖 — Container Overrides**:
用户在一次部署中显式设置或清除的容器字段：环境变量的设置与删除、挂载、docker 参数、`/dev/shm` 大小、模型挂载路径、入口、命令与[[命令模式]]。它是部署自定义的唯一一层，是[[解析后启动方案]]的输入，不是独立完整的运行定义；与平台托管项重复的 docker 参数被拒绝而不是被覆盖。
_Avoid_: RunSpec Overrides(旧称)、把覆盖记录当作可独立执行的配置、把 `/dev/shm` 大小理解成内存预留

**启动超时 — Start Timeout**:
实例启动后 agent 等引擎健康检查第一次通过的最长时间（`start_timeout_sec`），超时即判这次启动失败、按重试规则重建。部署可以自设，否则取集群默认（设置 `deployment.start_timeout_sec`）；值在实例创建时写进它的期望状态，之后修改只影响新启动的实例。它属于部署本身，不是[[容器覆盖]]，虽然表单把它放在「运行环境」里。
_Avoid_: 健康检查超时（那是单次探测请求的超时）、把它当作运行中的存活探测、待调度超时（`pending_timeout_sec`，那是还没放下时的上限）

**解析后启动方案 — Resolved Launch**:
将[[档位]]、用户显式配置、引擎适配和已确定的放置事实合并得到的启动配置，包括命令、环境、资产、挂载和 API 契约。它表达准备怎样启动，不代表已经观察到的镜像内置内容或进程状态；未绑定的运行值保持待确定。
_Avoid_: RuntimeSpec、Result、Assembly(同一启动方案的重复中间表示)、在预览或 controller 中再次拼命令

**实例期望状态 — Instance Desired State**:
一个具体实例交给 agent 持续对账的期望状态,由一份完整[[解析后启动方案]]加上实例身份、目标 GPU、端口、desired state 和分布式角色组成。数据库中的实例 spec 只是这份协议对象的序列化快照,不是另一种运行定义。
_Avoid_: 把 Instance Spec 当镜像运行基线、把数据库 JSON 字段理解成独立 Spec、用 Instance Spec 反向维护目录事实

**命令模式 — Command Mode**:
一次部署对启动命令解释方式的选择：引擎接管、镜像入口加引擎参数、手工命令。默认引擎接管，其他方式只能由部署显式选择，不随镜像来源变化。
_Avoid_: entrypoint 覆盖(那是实例级对 entrypoint 值的覆盖,与模式正交)

**默认环境变量 / 常用候选 — Default Environment Variables / Common Candidates**:
默认环境变量是部署所继承的环境配置，来自[[档位]]与引擎适配。常用参数和环境变量候选只是可供用户选择的提示，不选不生效；成套运行经验由[[推理模板]]表达。
_Avoid_: 把常用候选叫默认参数(候选不生效)、把候选当作专家赋能小白的主通道(那是推理模板)

**推理模板 — Inference Template**:
一个[[原始模型]]在目录中的条目:模型元数据(参数量/发布日/上下文)+ 一组扁平[[档位]] + 不支持声明。同模型多引擎在同一条目内按引擎分组展示,≥2 引擎才显示切换。见 ADR 0009(取代 ADR 0005/0007 的五层结构)。
_Avoid_: 蓝图/Blueprint(旧名,已全面更名)、裸称「模板」(仅推理语境内可简称)、配方(中文场景不用此词)、五层矩阵/权重版本×芯片×档位三级(旧模型,ADR 0009 已废)

**内置推理模板目录 — Built-in Inference Catalog**:
GPUWorks 发布的原始模型规范元数据与 upstream [[档位]]权威全集:随二进制内嵌一份,[[发布通道]]上另有一份可在线更新的同格式目录包;两者谁最后导入谁生效(ADR 0040),任一时刻数据库里只有一份 upstream 目录。外部 recipe、真机记录、候选文件与审计报告只提供导入依据和来源证明,不构成另一份可独立生效的目录。
_Avoid_: 把生成候选、审计产物、数据库投影或前端映射当作并列信息源

**目录维护工具 — Catalog Authoring Tooling**:
可由人工、LLM 或仓库外脚本辅助修改[[内置推理模板目录]]，但不属于 GPUWorks 产品、构建依赖或运行时。产品测试直接验证最终目录的可加载、可预览和可部署行为，不要求目录可由某个生成器反向复现。
_Avoid_: 把生成命令当产品子命令、要求最终目录与某份候选文件或生成器输出保持双向同步

**可执行推理目录包 — Executable Inference Catalog Package**:
[[内置推理模板目录]]的可分发形态,把每个[[原始模型]]的结构化定义、轻量运行资产与人读说明收拢在同一模型目录中;数据库只是它的投影,镜像与权重仍由不可变引用独立分发。
_Avoid_: 把目录包叫镜像包、把数据库投影或单个 YAML 当成另一份权威目录

**运行组件 — Runtime Component**:
一个[[档位]]启动时需要独立下载和挂载的模型角色,例如 primary、speculative_draft 或 verifier;同一原始模型可以作为一个部署的草稿组件,但不会因此并入目标原始模型。
_Avoid_: 把草稿模型当目标模型的权重版本、从参数字符串或仓库名猜组件关系

**运行包装器 — Runtime Wrapper**:
随[[可执行推理目录包]]交付的轻量服务层,把一次性模型调用转换为可探活、可代理的长驻推理服务;底层框架和系统依赖属于镜像,不属于运行包装器。
_Avoid_: Serve Adapter(易与管理面协议适配混淆)、把 Python 环境或框架依赖塞进包装器

**推理 API 契约 — Inference API Contract**:
[[档位]]部署后对外提供的结构化操作集合,声明 endpoint、请求形态及同步/流式/异步方式,供代理、使用说明和 Playground 共同消费。
_Avoid_: 只在 README 中描述调用方式、从模型架构或 endpoint 字符串猜完整请求协议

**原始模型 — Original Model**:
推理模板跨引擎聚合与去重使用的内部稳定身份,统一小写,通常为带组织名的仓库式 ID(如 `deepseek-ai/deepseek-v4`)。用户主标题使用保留正确品牌大小写的 `display_title`(如 `DeepSeek V4`);实际下载和启动的具体仓库由[[档位]]的权重下载源表达并保留上游大小写。schema 不再另设含义模糊且重复的 `family_key`。
_Avoid_: 逻辑模型/Logical Model(旧称)、模型家族(会被理解为 Qwen3/DeepSeek-V3 等更宽泛系列)、base model(通常指微调前基座模型)、把内部小写 ID 当用户标题

**草稿模型 — Draft Model**:
推测解码中负责提出候选 token、由目标模型校验的独立模型身份;即使名称与目标模型相近,也不是目标模型的量化或精度版本。只有上游启动合同显式给出外部 draft checkpoint 时,目录才建立 `speculative_draft` 组件。融合式 checkpoint 同时包含目标权重和 draft module,仍是独立[[原始模型]],但不是第二个运行组件;例如 `DeepSeek-V4-Flash-DSpark` 由自身融合权重启动,不并入 `DeepSeek-V4-Flash`,也不伪造外部 draft mount。
_Avoid_: 把 draft 理解为未完成/未发布状态、把草稿模型当目标模型的权重变体

**模型组织 — Model Organization**:
原始模型的规范发布命名空间,通常是 `model_id` 的首段；界面可使用经复核的品牌展示名,但不改变内部身份。它不等于某个[[权重]]下载源仓库的再发布组织。
_Avoid_: provider(平台中专指云资源提供方)、权重仓组织(量化再发布仓可能不同)

**权重 — Weights**:
[[档位]]的一个字段:规范化量化标签(bf16/fp8/int4/w8a8…)+ 多下载源列表(huggingface/modelscope 各自 repo id,顺序即优先级,部署按可达性与用户偏好选源)。同一[[原始模型]]可有多个权重制品,且量化再发布仓可以属于其他组织;它们是不同档位,不改变[[模型组织]]。权重不再是独立层级(旧「权重版本」实体已废,ADR 0009);昇腾 w8a8 与官方 BF16 是不同档位。量化对硬件的门槛由档位的 gpus 谓词表达(fp8 档位写 `cc: ">=8.9"`),门槛数字在目录数据里、不在代码里。

另支持 `local` 来源，引用已注册本地模型的逻辑身份，沿用已有副本、存储和同步组规则；不代表公网下载、不存主机路径，也不随模板打包权重。

_Avoid_: 权重版本(旧实体名)、变体标签(旧 variant_label 自由文本)、把量化门槛写死在代码里(门槛随引擎内核演进,是目录事实不是常量)、把运行形态塞进量化标签(那属于档位 name/scenario)

同一原始模型可以有面向不同任务的权重制品。例如 `jinaai/jina-embeddings-v5-text-small` 的 classification、clustering、retrieval 与 text-matching 是一个原始模型支持的多种任务,不是四个原始模型。

档位的权重制品大小只表示本档位实际需要下载和落盘的字节数；它既不是来源仓库全部文件的大小，也不是运行时 GPU 显存需求。普通界面只需分别展示“本次下载量”和“GPU 要求”；来源仓库总大小仅在能够准确取得时放入高级来源详情，不得用一个含糊的模型大小替代这些含义。

**仓库稀疏物化 — Sparse Repository Materialization**:
权重来源可以只物化仓库内一个相对目录以避免下载无关制品，但必须保留该目录在仓库中的相对路径；引擎仍看到正常仓库根并自行解释内部布局。它是通用的下载与缓存事实，参与制品身份、缓存隔离和完整性校验，与模型类型及运行引擎无关；未声明时物化完整仓库。
同一来源与同一 revision 下，经过完整性校验的完整仓库可以满足任意稀疏物化请求；稀疏物化只能满足其路径集合的子集，不能反向冒充完整仓库。运行中的制品视图保持不可变，不能靠向同一目录补文件静默改变身份。
_Avoid_: 为某个模型硬编码目录名、把所选子目录改造成仓库根、把稀疏物化当运行时补丁、让启动参数感知下载缓存的内部层级

**档位资源合同 — Spec Resource Contract**:
少数无法由默认估算表达的档位可以声明候选 GPU 必须满足的运行时资源要求，并基于实际分配结果产生启动变量；未声明的绝大多数档位继续使用平台默认估算和自动并行度。资源合同只影响准入与变量，不选择镜像、权重、API 或执行任意宿主动作。
_Avoid_: 每个档位都要求手写资源逻辑、在产品代码中按模型 ID 或具体 GPU 型号分支、用资源合同替代镜像依赖和静态目录事实

**芯片兼容性 — Chip Compatibility**:
模型能否在某种芯片上运行的结论,只回答模型与芯片类型的兼容关系,与卡数、节点数及当前集群资源无关。由[[档位]]的 GPU 适配谓词与模型级不支持声明共同表达;所需卡数由部署时估算推导,实际资源能否满足由部署评估和调度判断。
_Avoid_: 把上游整机硬件档当芯片类型、因当前卡数不足判定不兼容、把“兼容”解释为“当前集群一定能运行”

**GPU 适配谓词 — GPU Filter**:
[[档位]]上内联的适用范围声明:`{vendor, cc?, name?}`——vendor 必填;cc 表达式(`>=x`/`<x`/`x..y`)仅 NVIDIA 求值(agent 采集的 compute capability,卡自报,无需名单);name 为不区分大小写正则(国产卡代际表达,如 `910[bc]`)。开区间谓词的档位即「通用配置」——此词仅指这类档位,不作其他用途。NVIDIA recipe 的测试卡不是谓词边界;目录生产需剥离测试卡调优并归纳安全下界,至少保证 H20(CC 9.0),能够证明 A100 可运行时优先写 `>=8.0`。界面标签从谓词现算(如「NVIDIA · CC≥8.9」)。
_Avoid_: 芯片类/Chip Class(旧具名类注册表与 `<vendor>_sm<NN>_plus` 三段文法,ADR 0009 已废,谓词内联不再具名)、把场景合并行标成「通用配置」(该词只属于开区间谓词档位)

**国产代际兼容继承 — Domestic Generation Compatibility Inheritance**:
目录生产时对真机 PASS recipe 执行的有向兼容规则:K100/K100AI PASS 默认同样适用于整个 BW 系列(只报系列的 `BW` 与 BW100、BW150、BW1000、BW1100 等);910B/910B4 PASS 默认同样适用于 910C/950DT/950PR。继承结果写入档位的[[GPU 适配谓词]],不在 chipmatch 中隐式猜测;同一精确配置在后继卡上的明确 FAIL 优先,中止该卡继承。

**厂商目录镜像必需 — Vendor Catalog Image Required**:
`accelerator.RequiresVendorCatalogImage(vendor) bool` 是镜像解析的唯一判定依据，表示该 vendor 的镜像不能安全继承 CUDA/ROCm 引擎默认镜像，必须在镜像目录中有对应条目。当前命中范围：ascend、ppu、kunlunxin（`IsDomestic` 子集）、hygon（DTK 镜像与 ROCm 引擎默认镜像不兼容）以及 apple（使用 macOS 环境包，不继承 Linux 引擎镜像）。`IsDomestic` 是更窄的子集（ascend/ppu/kunlunxin），用于 dGPU 后端与原生资产选择；两者含义不同，不可互换。所有镜像目录键、hardwarecatalog 的 vendor 字段、agent 探测器的 `Vendor()` 返回值都必须在 `accelerator.KnownVendors()` 范围内，由测试机械化断言。
_Avoid_: 在镜像解析路径中自行判断"是否国产"、用 `IsDomestic` 代替 `RequiresVendorCatalogImage` 做镜像目录必需判断、在 deploy/imagecatalog/hub 中重新实现 vendor 规范化逻辑

_Avoid_: 将 K100 继承扩大到 K500、将非 PASS 文档声明当成继承源、在匹配器代码中写死代际关系

**匹配特异度 — Match Specificity**:
一张卡同时命中同一模型多条[[档位]]时的裁决序:不支持声明一票否决 → 谓词越窄越特异(具体名字匹配 > 窄算力区间 > 开区间)。平手视为目录缺陷:兜底取目录序,并在 matched_by 标 ambiguous 供排查。部署结果记录 matched_by(命中了哪条、为何),随部署快照可回查。
_Avoid_: 按数据库返回顺序取第一条(那是缺陷兜底不是裁决规则)、给档位手工维护优先级整数(靠谓词宽度自派生)、比证据等级(证据已外置,ADR 0009)

**档位 — Spec**:
推理模板下的一条可执行交付配置,目录唯一真值单元:[[档位来源]]与 name 共同构成模型内稳定身份,其余事实包括[[场景]]、引擎、镜像、[[权重]]、[[GPU 适配谓词]]、args、env、pre_start。**不含 tp/nodes**——并行度是部署时从分到的卡数推导的运行时事实,特殊拓扑由 args 显式表达。「一模型多档位」即多条 spec,特殊卡特殊处理 = 更窄谓词的又一条 spec。部署页默认推荐档,可切换。
_Avoid_: 把档位当独立推理模板、Profile(旧五层实体名)、在目录里写 tp/nodes 字段(拓扑单一来源在部署时)、无参数的占位档位(没有完整参数就不入目录)

**档位来源 — Spec Source**:
档位所有权只有 upstream(随内置目录发布、只读)与 user(用户导入或克隆、升级对账不触碰)两值；同一[[原始模型]]下允许同名档位跨来源并存，选择时必须携带来源消除歧义。
_Avoid_: 用模型来源替代档位来源、按数据库顺序选择同名档位、把 Hugging Face/ModelScope 权重下载源称为档位来源

**模板展示类型 — Template Display Type**:
档位面向用户的用途分类,只有 general(通用)、compatible(兼容)、optimized(优化)三种语义;与独立的 GPU 目标组合后由语言包生成标题,例如 NVIDIA + general 显示「NVIDIA 通用模板」/「NVIDIA General Template」。优化类型必须有来源明确的专属参数、镜像、内核或环境证据,不能从档位 name、GPU 型号或 CC 下限猜测。
_Avoid_: 为每个厂商复制一组展示类型枚举、把中文/英文文案写入 seed、从 `spec.name` 解析展示名

**场景 — Scenario**:
低延迟 / 均衡 / 高吞吐(必要时含生产/PD 分离)标签,对应上游 orientation,是档位的一等维度:同一模型下不同场景是不同 args 的独立档位,展示永远并列、不合并。默认推荐规则:均衡 > 低延迟 > 首条。
_Avoid_: 性能模式(那是 vLLM 的 flag 名)、压测工况档(SGLang 文档里同名的假档位——同一命令仅压测参数不同,不导入)、把多场景合并行称作「通用配置」(已废除的展示形态)

**GPUWorks 已验证 — GPUWorks Verified**:
已废弃(ADR 0009,暂缓项):产品内不再有验证徽标与证据分级展示。真机验证事实由独立研究基础设施账本承担,其 PASS 结论经目录管线转化为档位;可信度叙事在目录生产线质量,不在产品标注。
_Avoid_: 在产品内标注已验证/证据等级(机制已删)

**兼容性参考 — Compatibility Reference**:
已废弃(ADR 0009):仅有兼容声明、缺完整可执行参数的条目不入目录、不展示。目录里只有可部署档位与不支持声明两态;此类信息留在研究目录,待产出完整配置后一步到位成为档位。
_Avoid_: 无参数的占位条目(没有参数展示了用户也用不了)、readiness 三分类(已删)

**自定义模板 — Custom Template**:
用户克隆内置推理模板，或向一个[[原始模型]]导入 user [[档位来源]]配置后形成的可编辑内容；它全局可见(产品无租户)，不随上游更新、不做上游 diff 提示。
_Avoid_: fork(界面用语统一 clone)、自定义蓝图(旧名)

**本集群已验证 — Cluster Verified**:
已废弃(ADR 0009,暂缓项):local_verified 字段与标记机制已删,需要时再加。
_Avoid_: 与 gpuworks 已验证混用(两者均已废弃)

**偏离 — Deviated**:
从推理模板部署时用户对推理模板预设的修改产生的状态标记,按字段级 diff 计算,仅提示不拦截。优先级恒为:用户表单 > 推理模板预设 > 引擎默认。
_Avoid_: 降级(偏离不是错误)、拦截

**快捷模式 / 专家模式 — Quick Mode / Expert Mode**:
同一部署草稿的常用配置视图与完整配置视图；切换视图不改变配置。手工命令是独立的启动方式，不等同于专家视图。
_Avoid_: 简单模式(旧称)、快捷部署/标准部署(旧双 tab 结构)

**GPU 硬件事实 — GPU Hardware Facts**:
GPU 型号、架构和 compute capability 只来自节点探测与 `chipmatch`，不再由镜像版本维护
supported/unsupported 自由文本标签。镜像目录描述 framework、引擎及架构等制品事实；新部署的参数、环境与准备步骤由推理模板和本次显式配置确定。
驱动与 framework 的兼容矩阵只用于候选排序和 Warning，不作为穷举式硬拦截。
_Avoid_: 在镜像上填写 GPU 型号兼容标签、把目录经验当成节点硬件事实

**厂商 / 框架 — Vendor / Framework**:
框架(cuda/rocm/cann/hggc/dtk/xre)是系列级单值事实属性(一系列一框架,不混排);厂商(nvidia/amd/ascend/ppu/hygon/kunlunxin)由 framework→vendor 映射派生,仅用于展示(logo)与过滤,不独立存储在镜像上。
_Avoid_: 把厂商当镜像的独立必填字段、一个系列混多框架

**校验 — Verification**:
镜像目录当前没有任何镜像校验：image ID 三态校验（verified/unverified/mismatch）、离线包 blob sha256 比对与 Docker Hub 存在性校验都已移除。manifest 可选的 `sha256` 只是正文完整性哈希；归档 URL 的「解析」是下载前读出真实引用，不叫校验。
_Avoid_: 校验三态、「已校验/未校验」标记(已移除的机制)

**浏览 / 采用 — Browse / Adopt**:
社区镜像 tag 列表是临时浏览数据(内置社区仓库或任意仓库的上游全部 tag,内存缓存 1 小时、不落库);用户点「添加」才生成 DB 条目;已添加条目上的用户自定义永不被刷新覆盖,上游消失只在浏览结果里标注。
_Avoid_: 同步(会误导为双向或覆盖式)

**滚动 tag — Rolling Tag**:
`latest`、`main`、`master`、`release`、`nightly`、`dev`、`edge`(整个 tag、不区分大小写)这类内容随上游变化的 tag。自动选镜像从不选它;节点上可「重新拉取」(force pull)取得上游当前内容,节点行的 digest 显示实际拉到的是哪一份。
_Avoid_: 把滚动 tag 当成不可变版本做自动推荐

**废弃 — Deprecated**:
目录对账的软状态:新导入的 manifest 里已没有、但仍被引用的系统版本标为弃用并记起始 `catalog_version`;没有引用的同类版本直接删除。「引用」= `deployments.image`、未结束实例的 `instances.image`、`models.image_id` 任一命中。已跑实例不受影响,自动选镜像不再选它,显式选择不拦截。
_Avoid_: 下架、把「弃用」与「删除」混用(两者由有无引用决定)

**就位 — Presence**:
某镜像版本在某节点 docker 本地库中的存在状态(absent/pulling/present/syncing/failed),由目录驱动 agent 批量回报。
_Avoid_: 已下载(不含 peer 分发/归档 load 等来源语义)

### 推理指标

**平台指标词表 — Inference Metric Vocabulary**:
平台推理指标的唯一定义（`internal/inferencemetrics`）：短名（`ttft_seconds`）、类型（counter / gauge / histogram）、单位，以及派生指标。存储名是 `gpuworks_infer_<短名>`；模板、映射规则和文档都用短名。
_Avoid_: 在新代码里直接拼写 `gpuworks_infer_*` 字符串、把存储名写进模板

**指标来源 — Metric Source**:
平台指标的出处：引擎自己的 Prometheus 端点（经[[观察配置]]映射），或网关对经过它的请求的统计。同一指标两者都有时用引擎值。
_Avoid_: 数据源（与监控数据源、模型来源混淆）

**观察配置 — Instance Observation**:
server 为一个实例解析出的「怎么采集它的引擎指标」：端点、映射规则或自动识别的候选规则集（`proto.InstanceObservation`）。由引擎内置映射和模板 `api.metrics` 唯一解析（`deploy.ResolveObservation`），取模板当前声明；是实例期望状态里唯一允许在运行中原地更新的部分，更新不重启实例（ADR 0039）。
_Avoid_: 指标配置、采集配置（不区分谁解析、何时生效）

**信号 — Signal**:
面板展示的一个可观测量（运行请求数、KV 使用率、TTFT……），由若干[[指标来源]]按优先级提供；面板只认信号，不认具体序列名。
_Avoid_: 指标（指存储的序列时用「平台指标」）

### 评测与压测

**评测 — Evaluation**:
对模型服务各类测量的顶层伞形概念，UI 入口名。当前仅含性能压测一种类型，为将来的精度评测预留位置。
_Avoid_: 把"评测"直接当"压测"的同义词

**压测 — Benchmark**:
评测的一种类型：对 OpenAI 兼容端点施加受控负载并测量性能指标（吞吐、TTFT、TPOT 等）。API/entity 命名用 benchmark。
_Avoid_: EvalScope（被替代的旧外部工具）、PerfTest、StressTest

**发压点 — Load Origin**:
实际发出压测请求的位置——某个 agent 节点或 server 自身，用户可选。决定测量结果中掺入多少网络路径开销。
_Avoid_: 压测客户端（不区分位置语义）

**运行 — Run**:
一次压测提交：目标端点、发压点、数据集与参数矩阵的一次完整执行，含一个或多个点位。
_Avoid_: 任务/Job（旧容器式管道的概念，已删除）

**点位 — Point**:
Run 参数矩阵中的一个参数组合（并发×输入长度×输出长度等）对应的一轮独立压测，是结果聚合、存储与对比的最小单位。
_Avoid_: case、轮次

**样本 — Sample**:
一个点位内发压的最小工作单元。合成定长数据集下一个样本即一次请求；多轮对话与[[Agentic 合成负载]]下一个样本是一整段会话，由同一并发通道按轮次一问一答顺序发完才算一条。
_Avoid_: 请求（二者在多轮场景下不等价）

**Agentic 合成负载 — Synthetic Agentic Workload**:
按确定的首轮长度、轮间增量、输出长度和轮数生成的多轮顺序负载，用于测量长上下文、前缀复用和并发会话性能；它不衡量 Agent 任务或工具调用的正确性，也不代表生产流量。
_Avoid_: Agent 轨迹、真实 Agent 流量、工具调用评测

**会话并发 — Session Concurrency**:
一个[[点位]]内同时活跃的多轮会话数；同一会话内各轮有顺序依赖，不等同于某一时刻正在处理的 HTTP 请求数。
_Avoid_: 请求并发（指 Agentic 多轮负载时）

**平均输入长度 — Mean Input Length**:
一段多轮会话各轮完整 prompt token 数的算术平均值，是[[Agentic 合成负载]]的长度档位；它不是首轮长度或峰值长度。
_Avoid_: 输入长度（未说明平均/首轮/峰值）、平均首轮输入

**所需上下文长度 — Required Context Length**:
目标服务为完成一段合成会话至少要支持的上下文容量，覆盖峰值 prompt、本轮输出、chat template 和安全余量；它通常大于峰值输入。
_Avoid_: 最大输入、峰值输入、平均输入

**设计前缀复用率 — Designed Prefix Reuse Rate**:
由[[Agentic 合成负载]]的消息长度和轮间前缀关系在运行前计算出的可复用输入 token 占比，计算范围包含首轮输入；它描述负载提供的复用机会，不是引擎实际命中率，首轮身份也不说明缓存状态。
_Avoid_: 缓存命中率、实际缓存率

**实测缓存命中率 — Observed Cache Hit Rate**:
端点实际上报的 cached input tokens 占已上报 prompt tokens 的比例，必须同时说明上报覆盖率；端点未上报时该值为未知，不是零。
_Avoid_: 设计缓存率、未上报时写 0%

**单用户输出速度 — Interactivity**:
单个流式请求开始解码后每秒收到的 token 数，= 1000 / [[点位]]的 TPOT P50（ms），单位 tok/s/user；不含 TTFT，对应 InferenceMAX 的 interactivity、AIPerf 的 output token throughput per user。它与输出吞吐除以并发得到的容量份额（含 TTFT、请求间隙与收尾）是两个不同指标。
_Avoid_: 每用户输出吞吐、并发均摊吞吐、交互速度

**请求成功率 — Request Success Rate**:
成功的逻辑请求数占实际发起的逻辑请求数的比例；重试尝试单独计数，未发起的后续轮次不进入分母。
_Avoid_: 会话完成率、把跳过请求算作已发起请求

**会话完成率 — Session Completion Rate**:
完整成功执行全部规定轮次的会话数占计划会话数的比例；存在最终失败、取消的逻辑请求或缺少规定轮次的会话不算完整完成，尝试失败后重试成功不因此判为会话中断。
_Avoid_: 请求成功率、用 Run 的 done 状态表示会话完整完成

**缓存上报覆盖率 — Cache Report Coverage**:
有明确缓存字段上报的请求数占所声明请求集合的比例，必须同时给出分子、分母和统计范围；无上报表示未知，不等于未命中。
_Avoid_: 缓存命中率、未声明成功请求或全部发起请求分母的百分比

**整体处理速度 — Endpoint Token Throughput**:
整个被测端点每秒成功处理的输入与输出 token 总数，单位 tok/s，界面短标签为“整体速度”；它不按 GPU 数归一化。
_Avoid_: 每 GPU 吞吐、输出吞吐、GPU 速度

**每并发样本数 — Samples per Channel**:
一个点位内每条并发通道依次跑多少条[[样本]]，点位总量 = 并发 × 该值。随并发缩放使各点位统计功效一致。
_Avoid_: 每点请求数（名实不符：既非请求数，也非点位固定值）

**精确模式 / 估算模式 — Exact Mode / Estimation Mode**:
合成定长输入的两档：有可用 tokenizer（纯 Go 加载 tokenizer.json）时按 tokenizer vocabulary 生成；否则用随机字符并按 usage 自校准逼近目标长度，结果标注长度为近似值。[[Agentic 合成负载]]为保证长上下文形状可复现，只允许精确模式。
_Avoid_: 把两档混为一谈（结果可比性不同）

**自动 Tokenizer 来源 — Automatic Tokenizer Source**:
托管实例上创建 Run 时让平台决定用哪个 tokenizer：优先取发压点那台节点上该模型已有的 `tokenizer.json`（发压点是 server 时经数据通道取回），没有才回落到模型仓库引用；两者都没有则退回[[精确模式 / 估算模式|估算模式]]，Agentic 负载因不允许估算而在创建期直接拒绝。它是对「按模型身份猜仓库」的替代——猜错会静默用错 tokenizer。外部端点没有可解析的目标机，只有显式的仓库或上传两种来源。
_Avoid_: 自动下载（它可能根本不下载）、默认 tokenizer（默认指选择策略，不是某个文件）

**过程曲线 — Run Curve**:
一次 Run 执行期间逐点位的指标时序（吞吐、TTFT、TPOT、延迟等），是「跑的过程中发生了什么」的记录，与[[点位]]聚合结果（「这个参数组合的最终成绩」）互补。随 Run 持久化，终态后仍可回看。
_Avoid_: 实时指标（那只是运行中的观察视角，不含回看语义）、监控指标（平台级 GPU/节点时序，另一套体系）

**被测对象快照 — Target Snapshot**:
创建 Run 时对「测的到底是什么」的一次性完整记录：目标端点与模型、部署与实例引用、GPU 型号与数量、引擎启动命令与环境变量（敏感值打码）。与部署后续变更及删除脱钩，是跨 Run 对比的前提。同[[运行契约]]的快照思路一致。
_Avoid_: 目标（那只是其中的端点/部署指向，不含配置全貌）、运行参数（那是压测自身的参数矩阵，与被测对象无关）

**判定 — Verdict**:
Run 终态时对测量结果的客观解读，测量事实的一部分：饱和点在哪、是否被上限截断、SLA 最大达标并发等。与「建议」严格区分——建议（下一步该怎么改参）是 UI 层从判定推导的展示策略，不属于测量事实。
_Avoid_: 结论（UI 组件"结论卡"可用此词，域模型不用）、建议（那是判定的下游推导）

**停止原因 — Stop Reason**:
自适应搜索（饱和/SLA 模式）终止的机理：增益不足、触达并发上限、错误率超限等。判定"真饱和 vs 被截断"的直接依据，由发压执行方随终态进度上报。
_Avoid_: 从点位序列反推（启发式不可靠）

### 节点接入与 agent 管理

**API 令牌 — API Token**:
GPUWorks OpenAI 兼容网关签发的访问凭证，用于调用 `/v1` 下的推理接口。与上游 AI/provider 配置中的 API Key 是两种不同凭证。
_Avoid_: API Key（当指 GPUWorks 网关自身签发的凭证时）、节点 token（agent 身份凭证）

**节点 token — Node Token**:
每节点唯一的长期凭证，token 即身份——server 凭它识别连接属于哪个节点。无 TTL，踢机器靠删节点。
_Avoid_: bootstrap token（已退役的全局共享凭证）、api key（网关侧概念）

**pending 节点 — Pending Node**:
在 UI 铸出 token 时创建、尚未有 agent 成功连上的节点记录。使“脚本跑了但没连上”从无声失败变成一行可见记录。状态值现在是 `connecting`（连接中），不再有单独的 `pending` 状态；agent 连上后变 `online`。
_Avoid_: 未注册节点、临时节点

**注册确认消息 — Registered**:
每次连接建立时 server 下发给 agent 的身份告知消息（含节点 uuid）。agent 仅在内存持有 uuid，磁盘上不存任何身份状态。
_Avoid_: identity.json（已删除的旧机制）

**拒绝消息 — Reject**:
server 在关闭连接之前主动发出的拒绝消息，带原因码（如 bad_token）。agent 收到后打 ERROR 并转入 5 分钟长退避重试，不退出进程。
_Avoid_: 踢下线（那是同 uuid 新会话替换旧会话）

**smoke 握手 — Smoke Handshake**:
只验证“能否与 server 建立最基础连接”的一次性握手：走完真实 WS + 多路复用后发 `Hello{Smoke:true}`，server 校验 token 但不注册会话、不踢现有连接，回 ack 即关。是升级换二进制前的冒烟测试手段。
_Avoid_: 健康检查（那是运行中的存活探测）、ping

**原地升级 — In-place Upgrade**:
agent 升级机制：下载新二进制 → smoke 握手通过 → 原子替换文件 → `exec` 原地换进程映像（PID 不变、进程从不退出），从而不触发 systemd 重启、不杀正在跑的推理。
_Avoid_: 重启升级、滚动升级

**Kubernetes 集群 — Kubernetes Cluster**:
以一个 DaemonSet 接入的一组 GPU 节点，记录接入令牌与 manifest 参数（`kube_clusters`）。集群里的每个节点仍是普通节点。条目有两种来历：从云账号的托管集群接入的，归在账号之下，GPUWorks 可以用云账号现取的临时凭据代为应用 manifest；不经云账号、由管理员自己在集群里执行 `kubectl apply` 接入的（如自建的 k3s，以及没有云账号可接入的其他云上的托管集群），自己算一个[[外部平台]]，在列表里单独占一行（「厂商」列为「—」，「平台类型」列写 Kubernetes）。「快速接入」的卡片和「添加外部平台」弹窗的页签都叫“Kubernetes 集群”，指后一种接入方式。需要和从云账号接入的集群区分时，写“独立接入的 Kubernetes 集群”。删除云账号后，从它接入的条目改为独立接入的条目，节点照常工作，GPUWorks 不再能代为应用或从集群卸载。
_Avoid_: 集群分组（标签的事）、k8s 节点池、本地 Kubernetes 集群（“本地”容易被理解为集群所在的位置）、其他 Kubernetes 集群、自建集群（云上的托管集群也可能独立接入）

**接入令牌 — Enrollment Token**:
集群级凭证，只用来让 DaemonSet 的 agent 换取各自的节点 token（`POST /kube/enroll`），首次使用时绑定集群。吊销后新节点不能再加入，已加入的节点照常。
_Avoid_: bootstrap token、节点 token

**kube 运行时 — Kube Runtime**:
agent 的运行时之一（`--driver=kube`），把实例落成钉在本节点的裸 Pod；镜像在节点 containerd 的 `k8s.io` 命名空间。
_Avoid_: k8s 驱动、Operator

**占位 Pod / 借卡 — Hold Pod / Card Borrowing**:
有 device plugin 的节点上，gpuworks 为要用的卡起一个只申请 1 张卡的占位 Pod（`gpuworks-hold-*`），使该卡在 Kubernetes 账面上归平台；实例全部停止超过集群的「空闲归还」时间后删除，称为归还。
_Avoid_: 预留 Pod、独占

**带外恢复 — Out-of-band Recovery**:
agent 控制面连接失联后，server 经独立的 SSH 旁路判定节点与 agent 状态并尝试修复的机制；它与 agent 自身重连、server_dial 拨号并列，是最后一层手段。
_Avoid_: 失联恢复（「失联」只是 offline 的口语泛称）、节点自愈（没有体现旁路通道）

**带外探测 — Out-of-band Probe**:
带外恢复中的一次 SSH 探测动作，用于判定节点 SSH 可达性与 agent 进程存活状态。
_Avoid_: 健康检查（指运行中存活探测）、心跳

**agent 重启 — Agent Restart**:
带外恢复中经 SSH 重新拉起 agent 进程；进程身份会变化，但正在运行的推理容器应被新进程接管。
_Avoid_: 原地升级（后者替换进程映像且 PID 不变）

**带外重装 — Out-of-band Reinstall**:
agent 重启无效或安装环境丢失时，经 SSH 重新安装 agent 并沿用原节点身份的恢复动作。
_Avoid_: 重新纳管（会创建新的节点身份）

**AgentVersion / 可升级 — Agent Version / Upgrade Available**:
节点连接时自报并落库的 agent 版本；“最新版”即 server 自身版本（agent 内嵌于 server，同次构建），计算节点按当前连接、版本、升级能力和本地包情况展示待升级（较低正式版）、待对齐（较高正式版）、构建不同（先后未知）或暂不可升级。离线 Agent 单独展示离线前版本，重连后重新分类。预览确认后按 server 目标版本执行。
_Avoid_: manifest 版本（不存在独立版本清单）

**name / hostname — Name / Hostname**:
name 是用户可改的展示名，仅在首次连接且为空时用 hostname 回填一次，之后永不自动同步；hostname 是独立字段，随连接持续更新。
_Avoid_: 用二者互指

**最近来源地址 — Last Remote Address**:
server 实际观测到的 agent 来源地址（非 agent 自报），NAT 场景端口每次重连会变。
_Avoid_: 节点地址（那是 计算节点接入地址（HostAddress）/ ServerDialAddress 等寻址配置）

**SSH 连接方式 — SSH Connection Method**:
云实例提供的一种 SSH 访问路径；同一实例可以同时提供直连、平台代理等多种方式。
_Avoid_: SSH over TCP、SSH 地址（无法表达一个实例存在多种路径）

**管理节点接入地址 — Management Node Address**:
agent 获取安装资源并主动连接的控制面基址。
_Avoid_: 控制面接入地址（旧称）、server 外部地址、监听地址、SSH 地址

**agent 接入地址 — Server-dial Address**:
server_dial 模式下，server 视角可拨通的 agent `host:port`。
_Avoid_: agent 外部地址、节点 IP、agent 监听地址

**agent 监听地址 — Agent Listen Address**:
server_dial 模式下 agent 等待 server 拨入的内部绑定地址。
_Avoid_: agent 监听端口、agent 接入地址、节点端口

**节点互联地址 — Node Interconnect Address**:
多机推理和节点间模型同步使用的节点互达地址，与 SSH 和 server_dial 寻址无关。
_Avoid_: agent 互联地址、SSH 地址、agent 接入地址

**外部平台 — External Platform**:
GPUWorks 之外、把机器交给 GPUWorks 纳管的平台：公有云厂商的账号、ZStack Cloud 专有云环境，以及不经云账号、自行执行 `kubectl apply` 接入的 Kubernetes 集群。控制台在“外部平台”页把它们列在一起，列表的「厂商」列写云厂商（独立接入的 Kubernetes 集群为「—」），「平台类型」列写公有云、专有云或 Kubernetes；「添加外部平台」弹窗里对应的三个页签是公有云、专有云和[[Kubernetes 集群]]；从云账号接入的托管集群归在它的账号之下，不单独算一个外部平台。界面上单说“平台”分不清指谁：指这些对象写“外部平台”，指 GPUWorks 自己的密钥写“GPUWorks 平台密钥”。
_Avoid_: 第三方平台（ZStack Cloud 与客户自己的私有云不是第三方）、接入平台、算力来源

**平台资源 — Platform Resource**:
一个外部平台交给 GPUWorks、能变成计算节点的东西属于哪一类：虚拟机（`vm`）、容器实例（`container`）、物理机（`host`）或 Kubernetes 集群（`kubernetes`）。前三类是实例，逐台安装 agent；集群以一个 DaemonSet 接入，选中的 GPU 节点自动成为计算节点。一个平台可以有几类（阿里云有虚拟机和 Kubernetes）；厂商对它有自己的产品名时，界面上的标签写产品名（“ECS”“ACK”），没有时写类型名。由 server 的平台字典给出。
_Avoid_: 资源类型（单说容易与 GPU、存储等混淆）、实例类型（那是云厂商的机型规格）

**节点来源 — Node Source**:
一个计算节点经哪个[[外部平台]]或集群接入：某个云账号的实例，或某个[[Kubernetes 集群]]（云账号下的集群或独立接入的集群）的节点；手动添加（安装命令或 SSH 向导）的节点为“手动添加”；server 读不到来源时显示“来源读取失败”，不当作手动添加。「计算节点」页的「来源」列显示它，可按它筛选，点开回到外部平台页里对应的那一行；接口里是节点的 `source`。
_Avoid_: 模型来源、来源规则（那是拉取镜像与模型的地址规则）

### 控制面安装与升级

**初始密码 — Initial Password**:
`gpuworks install` 交给服务首启的 admin 密码，记录在 `<data>/initial-credentials`（0664）。存在的唯一理由是单元文件全局可读、不能承载密码；服务创建 admin 账户时读取，改密后应删除该文件。
_Avoid_: 默认密码、首启凭证（密码本身叫初始密码，凭证文件只负责把它交接给服务）

**服务端配置 — Server Config**:
`<dir>/server.yaml`，server 启动前就要知道、且跟着机器走的配置：各功能的监听地址、数据目录、日志、可信代理等。install 写入、server 读取，控制台只改监听地址一节；命令行参数可临时覆盖。服务模式、服务用户、版本号不存，从单元文件和二进制推导；环境变量不是配置来源。
_Avoid_: 安装清单（`install.json` 已删除，只在把更早的安装转到 server.yaml 时读一次）、发布清单（发布站的 release.json）、设置（在界面上改、存数据库的业务设置）

**监听功能 — Listen Function**:
server 对外提供的三类入口：控制台（Web UI 与管理 API）、节点接入（agent 长连接、安装脚本、agent 下载）、推理 API（`/v1`）。每类在 server.yaml 里绑定自己的 `IP:端口`，不写的继承控制台；请求只在承载其功能的地址上应答，127.0.0.1 承载全部。
_Avoid_: 端口（同一端口可在不同 IP 上承载不同功能）、服务网络（server 连节点的方向，不是监听）

**控制面升级 — Control-plane Upgrade**:
停服 → 备份 → 原子替换二进制 → 重启 → 探活、失败自动回滚的升级路径；进程会重启，因此与 agent 的原地升级不是同一机制。
_Avoid_: 原地升级（agent 的机制，PID 不变）、重装（数据与 master key 不动）

**发布通道 — Distribution Channel**:
发布方交付 server 版本、[[镜像目录]]与[[内置推理模板目录]]的唯一途径：一棵逐字节相同的发布树放在多个[[发布源]]上，入口是各渠道的签名[[发布索引]]：`nightly`（滚动预览版，每次 master 构建）与 `release`（正式发布版，每个 `v*` tag）。安装选哪个渠道记在 `server.yaml` 的 `update_channel`，不写时跟随构建（ADR 0040）。
_Avoid_: 发布站（内网浏览/手工下载用的 `gpuworks.zstack.ai`，不再是升级来源）、上游地址（单一地址的旧说法）

**发布源 — Distribution Source**:
放着同一棵发布树的一个基础 URL（阿里云 OSS、华为云 OBS 默认域名，Cloudflare R2 `dl.gpuworks.ai`，或客户自建镜像）。客户端同时请求所有源的索引，取第一个验签通过的；文件失败换源续传。源只负责传输，不需要可信。
_Avoid_: 镜像站（与容器镜像混淆）、CDN（源不一定在 CDN 后面）

**发布索引 — Distribution Index**:
`<channel>/index.json`：ed25519 签名信封里的 JSON，列出该渠道当前的 server 版本（各平台压缩包）、镜像目录与推理模板目录包，每个文件带相对路径、size 与 sha256。公钥编进二进制。
_Avoid_: release.json（内网发布站的旧清单）、manifest（那是镜像目录本身）

**控制台升级 — Console Upgrade**:
在设置页「版本与更新」由运行中的服务端升级自己：备份（运行中的数据库用 `VACUUM INTO` 拍下）→ 原子替换二进制 → 退出由 systemd 拉起；新版本启动时按升级标记确认，或在用完启动次数后还原备份换回旧版本；标记只对它写明的目标版本有效。只接受支持控制台升级协议的发布包，只在 systemd 托管、运行安装目录里的二进制且目录可写时提供（ADR 0040）。
_Avoid_: 原地升级（agent 的机制）、在线升级（容易与目录的在线更新混淆）

**升级备份 — Pre-upgrade Backup**:
upgrade 停服后打包的 `<dir>/backups/<release_id>-<时间戳>.tar.gz`：数据库（含 WAL/SHM）、`master.key`、`server.yaml` 与旧二进制。默认保留最近 3 组，回滚和手工还原都用它。
_Avoid_: 数据备份（只含数据、不含旧二进制，无法直接顶回一次安装）、升级快照

**控制面集群 — Control-Plane Cluster**:
三个成员复制同一个数据库的控制面，写入需要两个成员确认；用户和 agent 只经一个稳定入口访问。默认构建与 macOS 是单个 SQLite 节点，不属于集群。
_Avoid_: HA 模式、高可用版（它不是一个版本，也不是功能键）

**成员 — Member**:
控制面集群里的一台主机：一个 `gpuworks` server 加它启动的数据库辅助程序。不是 GPU 节点（Node）。
_Avoid_: 节点、副本（节点专指运行 agent 的 GPU 主机）

**数据库辅助程序 — Database Helper**:
`gpuworks-dqlite`，承载成员的数据库节点的子进程，唯一使用 cgo 的制品；由 server 启动，随 server 退出。它不在本仓库构建，由带集群运行时的构建随 server 一起提供；server 只启动协议版本与自己一致的辅助程序。
_Avoid_: dqlite 服务、数据库服务

**传输策略 — Transport Policy**:
成员数据库连接允许与吊销的成员密钥清单，带修订号，落在成员数据目录里；辅助程序在每次握手时按它校验对端。

**业务代际 — Business Generation**:
成员在自己的数据库连接上装配出来的一次业务服务（API、网关、控制循环）。成员失去多数或材料证明失效时撤回当前代际，条件恢复后装配新的一代；成员进程和数据库连接不随之重启。

**材料证明 — Material Proof**:
成员开放业务前对集群材料（凭据密钥、成员身份、目录包与运行时资产、离线镜像、评测输入与明细、模型 logo、SSH 主机密钥等数据库之外的文件）做的一次多数校验；有有效期，过期且无法续期时成员关闭业务。

### 节点网络

**网络 — Network**:
用户声明的一套节点间网络（一套 fabric），带角色、类型与默认选择器，加载到节点或节点标签后解析到每个节点的网卡、IP 与 HCA。互通以"同一个网络对象"为准。
_Avoid_: 网段（只是选择器的一部分）、VPC、子网

**存储同步网络 / 参数网络 / 服务网络 — Storage-sync / Compute / Serving Network**:
网络的三种角色：节点间同步模型和镜像；多机推理的集合通信与 PD 分离 KV 传输；网关访问推理实例。一个网络可同时承担多个角色。
_Avoid_: 数据网络（与 agent 数据通道 data lane 混淆）、业务网络；英文 Parameter network（旧称；代码与取值为 `compute`）

**加载 — Attachment**:
网络与一个节点或一个节点标签的绑定；加载到节点时可覆盖网卡、IP、HCA。直接加载到节点的优先于经标签加载的，同优先级先加载的生效。英文仍叫 Attach。
_Avoid_: 附加（旧称）、绑定（易与端口绑定混淆）、挂载

**HCA（rail） — HCA**:
主机 RDMA 网络上节点用来传数据的一个 RDMA 端口，写作 `设备:端口`（如 `mlx5_0:1`）；加速器 RDMA 网络上是一张加速卡，写作 `npu3`。内部测试里两个节点的 HCA 一一配对跑 ib_write_bw。默认由网卡清单推断（主机 RDMA：已激活、IB 优先、RoCE 需网卡有 IPv4、只留速率最高的一组；加速器 RDMA：链路 UP 且配了地址的卡）；节点上可以增删，增删后该节点固定为手动列表。
_Avoid_: 网口、RDMA 网卡（HCA 是端口，一块网卡可有多个）

**HCA IP — HCA IP**:
HCA 所在网卡的 IPv4 地址。RoCE v2 靠它生成 GID，没有它 RoCE 端口不可用；InfiniBand 端口不需要，显示为"—"。

**bootstrap 地址 — Bootstrap Address**:
主机 RDMA 与加速器 RDMA 网络上节点用于握手的 TCP 地址：NCCL 带外通道、HCCL 建通信域、Gloo、vLLM master 地址、ib_write_bw 交换连接参数，数据不经过它。可以不在 RDMA 网卡上。主机 RDMA 默认取第一条 HCA 所在网卡的地址，没有 IPoIB 的 InfiniBand 取管理网地址；加速器 RDMA 默认取管理网地址，因为主机进程绑不了加速卡网口的地址。可以用网络的 bootstrap 网段或节点固定值改变。
_Avoid_: 管理地址（管理网是另一回事）、控制面地址

**自动 / 手动 — Auto / Manual**:
RDMA 网络上一个节点的 HCA 或 bootstrap 是按网卡清单推断的（自动，不落库、随上报变化），还是写在节点加载项里固定的（手动）。"恢复自动"清除固定值。

**管理网 — Management Network**:
内置网络，每个节点在上面的地址是 agent 连接 server 所用的本机地址（回环或经代理时取默认路由网卡），自动上报、不能删改。某个角色没有独立网络时由它承担：服务走 agent 隧道，存储同步与参数同步走该地址。每个节点可分别关闭它承担的角色，默认全开。
_Avoid_: 默认网络、直连地址（已删除的 `direct_address`）

**平台密钥 — Platform SSH Key**:
server 自己持有的一对 SSH 密钥（私钥加密存库），SSH 纳管、预检查、带外恢复和云厂商安装默认用它登录；节点授权其公钥一次即可，节点 SSH 凭据只记地址时即表示用平台密钥。
_Avoid_: server 默认私钥（已删除的读 `~/.ssh` 回退）、部署密钥

**计算节点接入地址 — Compute Node Address**:
server 主动连接一台计算节点所用的地址，只用于 SSH 纳管与 server_dial 拨号，与节点间流量无关。
_Avoid_: 主机接入地址（旧称）、直连地址、节点地址（节点间流量用管理网地址）

**网卡清单 — Network Inventory**:
agent 从 sysfs 读取并随节点状态上报的网卡（地址、速率、MTU）与 RDMA 端口（link layer、速率、对应网卡），以及昇腾节点上用 `hccn_tool` 读取的加速卡网口（卡号、地址、链路状态、速率）。
_Avoid_: 网络配置（那是 server 下发的设置）

**卡连通性测试 — Card Reachability Test**:
加速器 RDMA 网络的[[内部测试]]方式：节点用厂商工具从一张加速卡的网口 ping 另一个节点的加速卡网口。"同号卡"只测两端同名的卡；"全卡矩阵"测一个节点的每张卡到另一个节点的每张卡。结果按节点对归为形状：全互通、仅同号卡、部分可达、全不通。
_Avoid_: NPU ping、卡间带宽（只测可达性）

**内部测试 — Internal Network Test**:
在一个网络上对选定节点两两做认证握手 RTT 与 TCP 吞吐测试，由 controller 执行、结果按网络保留。
_Avoid_: 预检（部署前的多机预检是另一个入口）、压测（那是推理评测）

### 公网入口

**公网入口 — Exposure**:
控制台对外发布的那一个地址，以及它的期望配置与观测。一个 data-dir 一个入口，连接器由 server 托管、随 server 退出而关闭；CLI 只提交配置与读取观测，不自己持有进程。
_Avoid_: 公网地址（只指地址本身，不含托管与状态）、反向代理（那是把请求转给别处的另一件事）、隧道（本仓指 server–agent 的双通道传输）

**公网入口提供方 — Exposure Provider**:
一种发布公网入口的实现：当前对用户开放的是 `cloudflare-quick`（免账号临时地址、不支持 SSE）；`cloudflare-named`（token + 已发布路由、固定域名、支持 SSE）已实现并有离线用例，但因首次使用需要用户自备 Cloudflare 域名与已发布路由而暂不开放。两者共用 cloudflared 获取与进程托管；与云资源 provider（租机）不是一套概念，也不是多入口并存。
_Avoid_: 厂商（提供方不要求与厂商一一对应）、隧道类型（会与 server–agent 隧道混淆）、profile（本功能不做多 profile）

### PD 分离部署

**PD 部署 — PD Deployment**:
由 prefill 与 decode 两个角色组组成的一个部署（ADR 0028）。客户端仍访问同一个服务名，两阶段交接由网关完成。
_Avoid_: 两个部署加关联、把 P/D 当成多机的 rank

**角色 — Role**:
实例在 PD 部署里的职责，`prefill` 或 `decode`，写在 `instances.role`。与多机推理的 leader / follower（`dist_role`）是两个独立维度。
_Avoid_: rank、副本类型

**PD 模式 — PD Mode**:
一条完整配方：引擎、KV 连接器、线协议与配对规则，例如 `vllm-nixl`。由 `internal/server/pdmode` 声明，用户只选这一项。
_Avoid_: 连接器（只是模式的一部分）、传输后端

**线协议 — Wire Protocol**:
网关完成两阶段交接所用的请求字段与调用顺序（`vllm-nixl` 是先 P 后 D 的顺序两阶段）。与 KV 走 RDMA 还是 TCP 无关。
_Avoid_: 传输协议

**降级 — Fallback**:
P 不可用、连接失败、返回 5xx 或没有返回 KV 参数时，网关把原请求交给 D 本地完成。按原因计数；部署可以用 `fallback=fail` 改为返回可重试的 503。
_Avoid_: 重试（重试是在同角色的其他实例之间重放）

**PD 有效性 — PD Effectiveness**:
D 收到的 prompt token 中经 KV 传输得到的占比（vLLM `prompt_tokens_by_source{source="external_kv_transfer"}`）。持续下降说明分离没有真的发生。
_Avoid_: 用 P 侧的传输计数判断（NIXL 是 D 拉取，P 侧计数一直为 0）

**KV 卸载后端 — KV Offload Backend**:
在 GPU 之外保存实例 KV 缓存的组件：引擎自带的内存层与磁盘层（默认，`native`），或引擎之外的组件。部署用 `kv_offload_backend` 选 id，描述行由 `internal/server/kvbackend` 声明（ADR 0069）；PD 下只在行声明的角色上保留。
_Avoid_: KV 连接器（只是后端渲染出的配置）、缓存层（指引擎自带的内存层或磁盘层）

### 部署放置与资源

**副本 — Replica**:
部署里被独立调度、启动、失败、重试与扩缩的一份服务能力：角色（PD 之外为空）加它的一组 rank。单机副本只有一个 rank，多机副本是一个 leader 加若干 follower；PD 的每个 P、每个 D 各是一个副本。代码与线协议里叫 unit（ADR 0029）。
_Avoid_: 实例单元、组、用"实例"指代副本

**实例 — Instance**:
某个副本某一代里的一个 rank，对应一个容器从创建到清理的全过程；UUID 不跨代。失败的 leader 实例会保留，作为该副本用掉的一次尝试。
_Avoid_: 实例行、容器（容器可能尚未创建或已被移除，实例仍在）

**代 — Generation**:
一个副本的一次启动：leader 与 followers 共享 `(dist_group, dist_attempt)`，一起创建、一起失败。失败的 leader 实例就是副本用掉的一次重试。
_Avoid_: restart count（已删除）、attempt 单指数字

**按内存共享卡 — Shared by Memory**:
一类加速卡（当前只有 Apple 统一内存）的调度语义：一张卡可以同时承载多个实例，每个实例按准入时的内存份额（`memory_claim_bytes`）记账，卡满即止。份额在准入时确定并随实例保存，之后不按模板或估算重算（ADR 0044）。
_Avoid_: 把它当 dGPU 切分、按卡独占计数

**卡准入闸 — Card Gate**:
调度器里在某个条件下扣下一张本可使用的卡的规则（`scheduler.cardGate`），按表序决定先报哪个拒绝原因；可同意的闸在请求的同意（手工选卡）下放行；运维对一张卡启用调度后，这张卡本身可调度，不再经过闸。闸不改卡的能力事实（如 `ForeignShareable`）。
_Avoid_: 把同意折进能力位、在 `takesWorkFor` 里加特例

**卡偏好档 — Card Preference**:
放置在有得选时避开的一类卡（`scheduler.cardPreferences`），不是准入规则：先试不含这类卡的方案，放不下才逐档放宽，并在方案里说明用了哪些卡。表序即优先级：先放宽告警卡，最后放宽与平台外进程共用的卡；只有标为 `poolWide` 的让步才让池放置跨节点继续找。
_Avoid_: 用打分罚分表达偏好、在调度器里复制「先试 X 再试全部」的分支

**实例提示 — Instance Note**:
服务端读实例时按规则表推导的非阻塞事实（`deployment.InstanceNotes`，码 + 参数，如 `INSTANCE_CARD_SHARED_OUTSIDE`），随实例 DTO 的 `notes` 下发，用来解释用户看到的现象（如推理变慢）。不持久化、不按实例保存状态：原因消失或实例删除后不留任何东西。
_Avoid_: 运行失败（`runtime_failure` 是持久化的失败原因）、把提示写进实例行

**用户标签 — Node Label**:
运维写在节点上的键值（`Node.Labels`），也是按标签加载网络时匹配的对象。键不得落在保留域 `gpuworks.ai/` 及其子域里。
_Avoid_: 用标签代替芯片探测事实

**系统标签 — System Label**:
平台从节点事实现算的标签，键是保留域下固定的八个（节点、厂商、CPU 架构、GPU 型号与架构、三种网络角色），不落库；一个键可以有多个值（混卡节点、多个网络）。
_Avoid_: 把系统标签写进节点标签、为它建表

**节点条件 — Node Requirement**:
部署声明的候选节点范围：每个键给出允许的值，同键多值为「其中之一」，不同键「同时满足」；键可以是用户标签或系统标签。手动指定节点是 `gpuworks.ai/node` 条件的特例。条件只划定候选池，节点仍需满足资源和兼容性检查。
_Avoid_: 节点标签约束（旧称，只覆盖用户标签）、无匹配时自动扩大节点范围

**停止调度 — Cordon**:
运维对节点的声明（`Node.Scheduling` 为 `cordoned` 或 `draining`）：不再往这台节点启动新副本——候选池把它单列为「已停止调度」，手动放置的多机、PD rank 指定了它也等待（自动放置的在副本要启动时换节点，见[[自动放置]]）；已有副本照常服务。恢复调度后等待中的副本立即重试。
_Avoid_: 用删标签、下线节点代替停止调度

**扩展停止调度 — Extension-Held Cordon**:
扩展模块注册的调度保持者（`store.SchedulingHolder`，以来源名区分，多个时先注册的先说了算）把节点保持在停止调度：节点设为 `cordoned`、`scheduling_by` 记为它的来源名，从不驱逐，已有副本照常服务。它高于平台停止调度、低于运维：平台原因消失不解除它，运维不能恢复调度（409，带扩展的 code）但可以停止调度或驱逐；保持结束后回到平台停止调度或可调度。保持本身不落库，每次上报按当前事实重新询问；没有注册保持者的 server 在启动时释放遗留的扩展停止调度。
_Avoid_: 用运维停止调度代替（运维可以随手解除）、驱逐被保持的节点

**扩展保持的卡 — Extension-Held Card**:
扩展模块注册的按卡保持者（`store.CardSchedulingHolder`，以来源名区分，多个时先注册的先说了算）把单张卡保持在停止调度：卡不接新工作（`GPU.CanTakeWork` 为假，`scheduling_held_by` 记为它的来源名），节点本身照常可调度，卡上已运行的实例照常服务、从不驱逐。它高于管理员的启用、低于管理员的禁用：运维不能把被保持的卡设为可调度（409，带扩展的 code），但可以禁用它；保持结束后回到管理员的决定或平台的判定。保持本身不落库，每次写卡的上报与每次重新应用按当前事实重新询问；没有注册该来源的 server 在启动时释放遗留的卡保持。
_Avoid_: 用管理员禁用调度代替（管理员可以随手启用）、驱逐被保持的卡上的实例

**驱逐 — Node Drain**:
停止调度，并把这台节点上的副本迁走，每个部署一次迁一个，服务副本数不低于驱逐前。候选池放来的副本先在别的节点起好替代副本、替代副本运行后才进入排空并退役；多机与 PD 的副本同样先在其他节点起替代（同计算网络、同 GPU 架构、卡空闲），运行后放置才改写过去、旧副本再排空。没有节点可去时副本留在原处等待，继续服务。钉在这台节点上的单机副本不迁；强制驱逐不等替代，立即撤下全部副本（不计入重试次数）。
_Avoid_: 排空（单个实例停收新请求、处理完在途请求后退役，是驱逐中的一步）

**自动放置 — Automatic Placement**:
多机与 PD 部署的放置方式之一（`placement_mode` 为 `auto`，存为 `deployments.units_auto`），与单机的「自动」同名但机制不同：平台在创建时按空闲卡为请求没写的 rank 或单元挑节点，一个多机副本、一个 PD 部署只在一张计算网络、一种 GPU 架构里挑，RDMA 优先，已有模型的节点优先，再按放置策略（多机与 PD 默认集中，PD 集中时一台放得下就全放一台）；挑中的节点**写回单元**（ADR 0057），单元因此始终写着节点，匹配、网络解析、节点失联重调度、驱逐照常工作，写着的节点也是之后「优先沿用」的依据。单元要启动时，控制器按当前空闲卡试算，节点都在线却放不下（卡被占、停止调度、不满足条件）就只换放不下的 rank（`UNIT_PLACED`），没有地方可去则保留节点继续等待；卡不写回，启动时由调度器挑（PD 自动单元不钉 `gpus`）。已有部署与默认值都是手动放置：节点与卡按运维所写钉死，卡被其他部署占着时 `pinned_gpus_unavailable` 点名占卡者。自动放置的 PD 单元必须一台节点放得下。
_Avoid_: 与单机的自动选节点混为一谈（单机每次启动都由候选池重选，不写回；多机与 PD 写回）、用它指代节点失联重调度（那是离线触发）

**节点失联重调度 — Node Replacement**:
一台节点离线超过部署的重调度等待（默认 10 分钟，每个部署可调，0 为从不）即视为失联，其上的副本被重调度到别的节点（界面与文档称「节点失联重调度」，API 与代码仍叫 node replacement）：单机副本由候选池在别处补位；多机与 PD 副本只换失联的那个 rank，选同计算网络（管理网兜底也可）、同厂商同架构、卡空闲且试算放得下的节点，优先已有模型的；凑不成组才整组重选。重调度按运维改副本列表的同一套校验改写放置，不消耗重试次数；新一代跑起来之前原节点回来就撤回，跑起来之后不再切回。撤回记录只在内存里，server 重启后保留已做的替换。钉在原节点上的单机副本不换。
_Avoid_: 故障转移（副本先失败、扣重试次数再重建；重调度不是失败重试）、驱逐（驱逐是运维主动的维护动作）

**资源分配权 — Allocation Claim**:
平台已为运行单元保留的资源使用权，决定其他部署还能否被准入。它不等于瞬时物理占用。已确认休眠的实例，分配权缩到它的休眠残留，卡上其余显存可以让给别的部署；唤醒时须重新过准入，卡被占满就不再放得回（409 `WAKE_INSUFFICIENT_GPU_MEMORY`），被平台外进程占着则拒绝（`WAKE_GPU_HELD_OUTSIDE`）。唤醒中的实例按完整显存计，直到它回报已醒。
_Avoid_: 用监控空闲量代替分配权、认为普通休眠保留原有分配权、认为休眠后一定能原样唤醒

**休眠残留 — Sleep Residue**:
休眠实例仍占在每张卡上的显存（CUDA 上下文、分配器池、CUDA graph 池、多卡时的通信缓冲），是休眠实例的分配权。按实例记：实例独占这几张卡、卡上没有平台外进程时，agent 在休眠成功约 1 秒后读卡的已用显存作为实测值；否则由 server 按模型参数量分档、CUDA graph 池、卡数估算（0.75–4 GiB，模型大小未知按 1.5 GiB）。没有实测值时用估算，绝不当 0；实测值保留到该实例下一次休眠重新测量。
_Avoid_: 把常量 1.5 GiB 当残留、把未测量记成 0、在界面上区分实测与估算

**物理占用 — Physical Footprint**:
工作负载在某个观察时刻实际占用的 GPU、主存或磁盘资源。观察的范围、来源和有效时间属于事实的一部分；缺少观察时保持未知。休眠残留的实测值是一次物理占用观察，估算值不是。
_Avoid_: 把分配配额标为实测占用、把整卡观测全部归给一个共卡实例

**过渡峰值 — Transition Peak**:
启动、加载、唤醒或切换运行状态期间可能同时需要的资源上界。需要对应引擎行为或测量依据；稳态预算不能直接充当过渡峰值。
_Avoid_: 用稳态显存估算冒充加载或唤醒峰值、将未知写成零

**定时计划 — Lifecycle Schedule**:
运维给一个部署预先写好的若干条动作：在周几的某个时刻（时区默认 server 所在时区，每条可改）休眠 L1/L2、唤醒，或把单机部署（或自动放置的多机部署）的副本数设为 N。每个时间点只执行一次；server 停机错过的，启动时只补最新的一次，且不得超过 12 小时；手动操作不会被反复覆盖，新建或修改的计划从下一个时间点起生效。执行经过与 API 相同的生命周期锁和同样的准入。
_Avoid_: 把它当调度器的自动扩缩（计划只执行运维事先写下的动作，不看负载）、定时任务（cron）

**唤醒等位 — Wake Waiting**:
唤醒放不下时的另一条路：放得下的先醒，其余记下起点（`wake_waiting_since`）等卡腾出来，由 server 在容量释放后自动唤醒；等满 30 分钟放弃并写时间线，随时可撤销。起点落库，server 重启不丢。
_Avoid_: 排队抢占（等位只拿现成的空位，不让任何人让出）、挂起请求

**让位唤醒 — Yielding Wake**:
带 `yield` 的唤醒：先让占着所需卡的醒着的部署休眠（默认 L1，对方主机放不下 L1 时自动改 L2：`yield_level=1` 就是休眠级别 `auto`，与出让策略的 `auto` 同一规则），再把唤醒放进等位，对方睡下后自动完成。被让位的部署记 `YIELDED_FOR_WAKE`，并记下为哪个部署让位（`for_deployment`），统计里它和[[让卡]]算同一种让出。占用者里有不可休眠的、PD 里最后一个醒着的角色或平台外进程时整体拒绝（409 `WAKE_YIELD_BLOCKED`），什么都不改。
_Avoid_: 抢占（占用者是被运维点名休眠，不是被调度器强制驱走）

**出让策略 — Yield Policy**:
部署上决定它会不会、什么时候把卡让给别的部署的设置，也就是谁可以抢谁，是部署共用 GPU 时唯一要选的东西，让卡规则只看这一项。四个值按等级从高到低：不自动出让（`never`，常驻服务）；闲置自动出让（`idle`，在线服务：别的部署在等且自己闲置满[[闲置时间]]才让，忙着从不让）；按时自动出让（`timeslice`，多个模型分时共用：闲置满闲置时间就让，更高等级的来了立即让，与同为按时自动出让的部署轮流，每轮最长服务[[最长服务时间]]）；随时出让（`anytime`，批处理与离线任务：闲置满闲置时间就让，更高等级的来了立即让，同为随时出让的之间只在闲置时互让）。策略之外是只在适用时才出现的参数：闲置时间、休眠级别（`auto`：先 L1，主机内存放不下权重时退回 L2；`l1`：严格 L1，放不下就拒绝；`l2`）、最长服务时间、出让时未完成请求的处理（`abort` / `wait`，最多等 600 秒）、[[请求最长挂起时间]]，默认值由服务端的策略字典给出。不自动出让以外的策略只用于可休眠的单机部署；两个闲置自动出让的部署可以共用一张卡，但两边都忙时谁也不让，设置时会警告。部署在无人等卡时一直醒着，不会自己休眠。见 ADR 0059（2026-10-29 再修订）。
_Avoid_: 服务等级、预设、常驻/在线/分时/离线（旧的四档预设，已取消，现在只作为各策略适合的场景）、无抢占时自动休眠 / 闲置休眠（已取消）、优先级（策略还决定空闲让卡、轮流与未完成请求的处理，不只是先后）、把不自动出让理解为“不能休眠”（运维和定时计划仍可让它休眠）

**闲置时间 — Idle Time**:
部署多久没有用户请求（没有进行中或挂起的请求；平台自发的健康检查不算）算闲置，`idle_sec`，15 秒到 7 天。除不自动出让外的三个出让策略都用它：闲置满这么久，有别的部署等它的卡就让。按时自动出让时不得超过[[最长服务时间]]。
_Avoid_: 用探测请求刷新闲置计时、把它当成自动休眠的时长（部署无人等卡时不会自己休眠）

**最长服务时间 — Longest Serving Time**:
按时自动出让的部署与另一个按时自动出让的部署轮流时，一次醒来最长服务多久，`max_serve_sec`，30 秒到 24 小时，到了就让给等着的那个。取代旧的“最短提供服务时间”，不再随实测切换时间拉长。
_Avoid_: 最短提供服务时间（旧称，含义相反）

**请求最长挂起时间 — Longest Request Hold**:
网关为等部署唤醒最多替一个请求挂起多久，`hold_max_sec`，10 秒到 1 小时，超时回 503 并带 `Retry-After`。
_Avoid_: 请求超时（它不限制推理本身的时长）

**驻留仲裁 — Residency Arbiter**:
自动化评估器里决定共卡部署此刻谁醒着的一步：唤醒有请求在等的部署，以及卡已没人占的待恢复部署（待恢复不让任何占卡者让卡），有请求的部署放不下时按让卡规则判断占卡者是否必须让卡，要么全让、要么都不让；让卡在到期的那一刻（闲置时间满、一轮服务结束、等待完成的上限到）由一个定时器触发，不等周期性评估。
_Avoid_: 调度器（它不放置新实例，只决定已放置实例的醒与睡）

**让卡 — Yield (Residency)**:
驻留仲裁为另一个部署让占卡的部署休眠（L1，主机放不下时 L2；不保持）。让卡规则只有一条：占卡者的出让策略不是不自动出让，并且闲置满它的闲置时间、或可被抢（按时自动出让、随时出让）且等卡者等级更高、或与等卡者同为按时自动出让且这一次醒来已服务满最长服务时间。统计「让出」的口径也只有一个：让卡（`AUTO_SLEEP`，`trigger` 为 `preempt`）和运维的[[让位唤醒]]（`YIELDED_FOR_WAKE`）让占用者休眠，都算一次，总览、时间线和调度记录数的是同一批，都记下为哪个部署让卡（`for_deployment`）。
_Avoid_: 把让位唤醒当成另一种统计（它由运维点名、是一次性动作，但算同一种让出）、驱逐

**保持休眠 — Held Sleep**:
请求不唤醒的休眠，网关仍回 503：定时计划下的休眠，运维的部署、副本、角色休眠里选了保持的（`wake_on_request=false`；不选时按出让策略，不自动出让的部署保持、其他由请求唤醒），以及让位唤醒点名的不自动出让的占用者。运维可以对已保持的部署“允许请求唤醒”（解除保持、不唤醒）。保持是每个实例的事实（`instances.sleep_held`）：只保持这次休眠点名的实例，唤醒只解除它唤醒或留下等位的实例的保持，平台自己做的休眠（让卡）既不设也不解除保持，请求会唤醒它。所以一个部署可以一部分实例被保持、一部分请求能唤醒；部署层面的说法（网关挂起还是回 503、`sleep_hold`）都由各实例推导。要等位的唤醒在应答 202 `waiting` 时就解除等位实例的保持：请求由网关挂起等这次唤醒，而不是回 503。保持休眠不为自己留卡：请求唤醒不了它，它的卡对任何新部署开放；只有请求能唤醒的休眠实例（`Deployment.WakesOnRequest(instance)`：部署可休眠且该实例不是保持休眠）才让共卡的休眠部署对不共卡的部署留着卡，新部署因此放不下时，调度原因是 `reserved_for_sleepers`。
_Avoid_: 把所有休眠都当成可被请求唤醒、认为手动休眠的部署仍保留卡

**调度记录 — Scheduling History**:
调度分析里跨部署、最新优先的调度事件列表：什么时间、什么原因、发生了什么（副本自动重建、定时休眠、手动扩缩容……）。它不是新的存储，而是部署时间线里属于调度的那些事件码，各归入 `auto`（平台按设置或计划自己动手）、`failure`（出了问题，平台在应对或已放弃）、`manual`（运维下的指令，记下达指令的用户）三类，事件码到类别的映射只在 server 一处。
_Avoid_: 另建一张调度日志表、把实例状态变化或配置修改也算进来

**同 NUMA 约束 — Same-NUMA Constraint**:
要求一个运行单元在同一节点上使用的 GPU 属于同一个已知 NUMA 域。它是放置条件，不是评分偏好；不同节点的相同 NUMA 编号不表示节点间拓扑接近。
_Avoid_: 把未知 NUMA 当成 0、用 NVLink 连通性替代同 NUMA 条件

**托管目录 — Managed Directory**:
推理模板以 spec 的 `dirs` 声明的可写主机目录（ADR 0077）。部署运行到的每个节点上各有一份，归在该部署的目录组（`<部署 id>-<创建秒>`）下，跨实例重建保留，部署删除后由 server 通知节点删除；调度按声明的 `limit_bytes` 加磁盘保留空间准入。它存状态，平台不修剪；KV 磁盘层是同一机制里按节点保留、会被修剪的缓存。
_Avoid_: 缓存目录（KV 磁盘层才是会被修剪的缓存）、把它当部署的 `extra_mounts`（主机路径由节点决定，不由运维填写）

**RDMA 运行传输 — RDMA Runtime Transport**:
具体运行单元在当前运行代次实际采用的 RDMA 数据传输。节点设备具备能力、两个节点预检连通和工作负载实际采用该传输是不同事实。
_Avoid_: 用 TCP 吞吐或环境变量设置证明 RDMA 正在生效

### 扩展与构建

**扩展模块 — Extension Module**:
单独构建的 server 编进二进制的一段 Go 代码（`app.Module`），经 `cli.Options.Extensions` 交给 server：可带自己的迁移（只建以 `ee_<名>_` 开头的表，记在 `namespace_migrations`）、在 `Init` 里经 `app.Host` 注册路由（挂在 `/api/v1/x/<名>/` 下）、功能、横幅、节点接入守卫与调度保持者，以及随 server 运行的后台服务。社区版与官方构建不含任何扩展模块，零值即今天的 server；这套契约留给测试、分叉与内置的插件宿主模块 `plugins`（ADR 0070、ADR 0071、`docs/dev/extensions.md`）。
_Avoid_: 插件（那是[[插件包]]，签名安装、作为 server 的子进程运行）、应用（应用是装在节点上的容器负载）、按版本命名的模块（模块按能力命名）

**功能键 — Feature Key**:
扩展路由、扩展菜单项与扩展注册的守卫用来指明「属于哪项可选能力」的不透明名字（`featuregate.Key`，如 `audit` 或 `iam.custom_roles`）。开源代码不解释任何功能键，社区路径从不查询；功能键只在扩展模块声明后才存在。
_Avoid_: 商品项、SKU（谁决定、为什么决定是功能来源的事）

**可用性 — Feature Availability**:
功能来源（`featuregate.Source`）对一个[[功能键]]给出的当前答复：完整（full）、只读（read_only）、锁定（locked，界面显示但不能用）或隐藏（hidden），可附限额、链接与带码的说明。server 在扩展路由上按它放行或拒绝（只读放行读与声明为放宽的写；锁定 403；隐藏 404），控制台按 `GET /api/v1/console/manifest` 渲染，自己不推导。正在运行的负载从不因可用性变化而中断。
_Avoid_: 功能来源内部的状态词（谁决定、为什么是功能来源的事，开源代码只说可用性）

**节点身份 — Node Identity**:
认出「同一台机器、同一批卡」的加盐摘要，由核心算出、原始值不出核心（`app.Host.NodeIdentity`）：主机部分摘的是 agent 在 hello 里上报的主机系统 UUID（固件的 DMI/SMBIOS `product_uuid` 或 macOS 的 `IOPlatformUUID`，存于 `nodes.host_system_uuid`），卡部分摘的是本节点可用的卡中带硬件序列号的那些（`accelerator.CardSerial`；昇腾按序号编出的标识不算）。每项是以调用方的盐为密钥的 HMAC-SHA256 前 8 字节，外加原始值末 4 位供人核对。它描述硬件而不是节点：同一台机器上的两个逻辑节点主机部分相同、卡各归各；节点 UUID 才是节点在平台里的身份（`docs/dev/extensions.md`）。
_Avoid_: 节点 UUID、节点 token（那是平台发的身份，换硬件不变）、机器指纹（只有主机系统 UUID 与卡序列号，不采集别的机器特征）

**插件包 — Plugin Package**:
ADR 0071；服务端进程部分已实现，节点组件、界面包与存储迁移尚未实现。官方与第三方扩展的唯一安装形式：签名的 `.gwpkg`，最多带服务端进程（server 的子进程，经继承的 fd 用 `gwx/1` 与核心通信）、节点组件（作为核心工作负载运行）、界面包与自带存储迁移四个可选部分。插件不进热路径，只把决定推成[[推送表]]；只运行能链到受信任根或主机信任发布者的包。
_Avoid_: 扩展模块（那是编进二进制的 Go 代码）、插件二进制（包不只是可执行文件）、动态库插件（server 是 `CGO_ENABLED=0` 的静态二进制，不加载动态库）

**发布者证书 — Publisher Certificate**:
ADR 0071；本构建还没有编进项目根密钥。编进核心的项目根密钥（或主机 root 在 `server.yaml` 里信任的发布者）签给一个发布者的证书，写明它能签哪些包名、能声明哪些功能命名空间、哪一个包是该命名空间的[[权威]]、能声明哪些特权角色与特权表、能否提供同源界面、最低隔离级别。证书的 `not_after` 只限制签发窗口，窗口内签出的包永不过期。
_Avoid_: 站点或客户的凭证（证书只说发布者能做什么）

**推送表 — Pushed Table**:
ADR 0071；首批五种（可用性、节点容量、节点保持、横幅、证明）已实现。插件交给核心的带类型的决定（如可用性、节点容量上限、节点保持、横幅）。核心校验、按 tick 合并进一个写事务持久化，再原子替换快照，热路径上的执行器只读快照。分四类：限制（按交集合并，永不失效）、授权（只接受[[权威]]推送，掉线超过[[宽限]]即降级）、显示（横幅与品牌，从不参与执行）、不透明（只存储与转交）。核心重启后、插件起来之前就按最后的推送执行。
_Avoid_: 插件钩子、回调（插件从不在请求、插入、上报或放置路径上被调用）

**权威 — Authority**:
ADR 0071。发布者证书的 `authority_id` 指定的那一个插件包，唯一能为该发布者命名空间里声明为 `gate: authority` 的功能键推送可用性；身份按它所在的 fd 认定，消息里没有可伪造的字段。一个命名空间至多一个已安装的权威；没有权威时这些键为锁定（`FEATURE_PROVIDER_ABSENT`）。
_Avoid_: 功能来源（那是 `featuregate.Source` 接口，插件宿主实现它）

**宽限 — Down Grace**:
ADR 0071；站点调低宽限的设置与插件路由尚未实现。插件掉线（停用也算）后，它推的授权类表仍按原样生效的时长：由插件随表推送，核心限定在 60 秒到 24 小时、未给时 1 小时，站点只能调低；掉线起点持久化，重启不续期。宽限过后授权降为只读（`PLUGIN_UNAVAILABLE`）；限制类的表不受宽限影响，永不失效；插件自己的路由在掉线时立即回 503。
_Avoid_: 租约（宽限总以降级结束，插件不能延长）、到期宽限（那是推送表时间线自己的步骤）

**限制覆盖 — Restriction Override**:
ADR 0071；核心的覆盖已实现，管理路由与 `gpuworks plugin pstate clear` 尚未实现。插件死了而它推的某条限制必须解除时，管理员对一条限制类条目做的放松（解除一个保持、调高一个配额、补一次额度）：只能放松，持久化、审计；所有者再次推送同一条目即被取代。离线时用 `gpuworks plugin pstate clear` 释放整张表。
_Avoid_: 解锁、强制授权（覆盖只作用于限制类，从不产生授权）

**extclock — Extension Clock**:
ADR 0071；离线重置与 `clock_authority` 的纠正入口尚未实现。[[推送表]]时间线所用的单调时钟：`max(墙钟, 高水位)`，高水位每分钟与关机时持久化，运行中按单调时钟前进，所以回拨墙钟不能让已过的步骤复活。时钟被拨快时只能由 root 在 server 停止时重置，或由持有 `clock_authority` 角色的包提交已验证的纠正（单次不超过 366 天，审计）。
_Avoid_: 可信时间（节点时钟互证、硬件钥匙时钟等由插件负责，核心只保证单调）

**扩展等级 — Extension Level**:
ADR 0084。一种可扩展的东西开放到哪一步：L0 散落（知识分布在多处分支里）、L1 单一接缝（一处定义、编译期显式登记，接缝外不出现它的名字）、L2 数据开放（核心实现行为，有哪些、参数是什么来自签名目录或[[推送表]]）、L3 代码开放（第三方代码在进程外运行，交出的结果仍由核心执行）。L1 是每种能力的默认要求，L2、L3 按 ADR 0084 的判定准则开放。
_Avoid_: 「做成插件」（不说是哪一级，含义差了一个数量级）、全部开放（开放即冻结契约）

**表种类 — Table Kind**:
ADR 0071、ADR 0084（注册表尚未实施）。[[推送表]]的种类名（如 `availability`、`node_holds`），决定行的 schema、类别（限制、授权、显示、不透明）、合并规则与执行它的钩子；只随核心版本增加。
_Avoid_: 表名（表种类是契约，持久化的表是核心实现）

**身份连接器 — Identity Connector**:
ADR 0085；编译模块可以实现跳转式连接器，插件包与密码式连接器尚未实施。与站点身份源（OIDC、SAML、LDAP、办公平台的 OAuth2）对话的插件包或扩展模块：跳转式的在身份源校验后请核心签发一次性登录票据，密码式的在登录时被核心同步调用一次校验密码。账号、会话与停用都在核心；连接器读不到用户表，也不设置会话 Cookie。
_Avoid_: SSO 插件（连接器也做目录同步与停用）、身份提供方（那是站点自己的系统）

**外部身份 — External Identity**:
ADR 0085。账号在某个身份源里的身份，`user_identities` 的一行 `(source, subject)`：source 是拥有它的扩展（插件包或编译模块）的来源名，`[a-z][a-z0-9_]{1,31}`，按扩展独占；subject 是身份源里不变的 id，不是用户名或邮箱。一对只链接一个账号。核心只按这一对找账号，从不按用户名或邮箱自动链接到已有账号；把已有账号链接到外部身份只能由管理员显式操作（`/api/v1/users/{id}/identities`），链接不创建账号，也不改角色。
_Avoid_: 第三方账号、绑定账号（链接只由核心建立）、按登录名匹配

**停用账号 — Disabled Account**:
ADR 0085。`users.disabled_at` 不为空的账号：全部管理会话立即删除，登录、平台令牌与推理 API key 一律以 `USER_DISABLED` 拒绝；数据与 key 保留，重新启用后照常可用，已提交的批处理照常完成。只由管理员或身份连接器的明确停用产生，连接器掉线不会停用任何人；最后一个启用的本地管理员（`users.local_admin` 为真的管理员）不能停用（`LAST_LOCAL_ADMIN`）。
_Avoid_: 停用用户（同一概念，统一叫停用账号）、删除账号、锁定账号（锁定指登录限流）

**登录票据 — Sign-in Ticket**:
ADR 0085。[[身份连接器]]在身份源校验过一个人之后，经 `Accounts().SignIn` 从核心换到的一次性凭证：60 秒内有效、只能用一次、库里只存哈希（`sign_in_tickets`），绑定在发起登录的那个浏览器的[[浏览器绑定]]上。浏览器拿它到 `/api/v1/auth/complete`，核心兑换后建立会话。
_Avoid_: 令牌（平台令牌与 API key 是长期凭证）、授权码（那是身份源发给连接器的）

**浏览器绑定 — Browser Binding**:
ADR 0085。经身份连接器登录时，核心在 `/api/v1/auth/sign-in/start` 连同来源名写进浏览器一次性 Cookie 的随机值；连接器只拿到由来源与它派生、反推不出它的句柄，[[登录票据]]绑定在句柄上，所以票据只能在开始登录的那个浏览器里、为开始时的那个来源兑换（挡住登录 CSRF 与跨来源的票据）。
_Avoid_: state（那是连接器与身份源之间的参数）、来源检查（兑换不看 `Origin` 与 `Sec-Fetch-Site`）

### 失败码与检查结果

**失败码 — Failure Code**:
运行期失败、API 拒绝或检查结果的稳定标识，UPPER_SNAKE，配插值参数；英文原文只作 fallback，人类可读文本由客户端按 locale 渲染。agent 与 server 共用的码定义在 `internal/proto`，只在 server 内产生的定义在 `internal/server/messages`，每个码都有 en/zh 文案（ADR 0067）。
_Avoid_: 把英文消息当作契约、按消息文本匹配分支、小写或驼峰的码、在调用点写码的字面量

**检查 id — Check ID**:
标识「这是哪项检查」，lower_snake（如 `ssh_reachable`、`disk_free`）；与标识「结果是什么」的失败码分开。
_Avoid_: 用失败码当检查 id、用显示名当检查 id

**检查状态 — Check Status**:
一项检查的结果，只有五个值（`proto.Check*`）：`pass`（查了，没问题）、`warn`（查了，有问题但不阻断）、`fail`（查了，有问题且阻断）、`skip`（应该查但没查：被前置检查挡住、缺能力、老 agent、超时、没收到结果，必须带码说明原因）、`na`（不适用于这个资源）。`skip` 与 `na` 不是通过。
_Avoid_: `ok`、`error`、`warning`、`passed`、`failed`、`skipped`、`blocked`、`unsupported` 等各功能自造的状态词；把「没查」或「不适用」显示成通过

**运行期失败原因 — Runtime Failure**:
实例进入 `failed` 时 agent 上报的结构化原因（`proto.RuntimeFailure`）：失败码、阶段、退出码、OOM 标记、证据行等带类型字段，随 `InstanceStatus.Message` 一起落库（`instances.runtime_failure`）。引擎日志的已知失败签名只在 `pkg/enginelog` 一处，助手按码诊断。
_Avoid_: 只上报一句 `err.Error()`、server 再用正则从消息里猜原因

**外部边界分类 — Boundary Classification**:
外部系统的输出（容器运行时 CLI 的 stderr、云厂商 API 错误体、引擎日志、SSH 错误）只能在调用它的适配器里按文本识别，并当场转成码或带类型的 error。仓库自己产生的 error 文本永不被解析，判断用 `errors.Is` / `errors.As`。
_Avoid_: 在下游用 `strings.Contains(err.Error(), ...)` 判断错误类别

### 批处理

**批处理任务 — Batch**:
用户上传一个 JSONL 请求文件后创建的异步任务：文件里的每个请求经网关（与客户端请求相同的路由、按需唤醒、用量与请求记录）执行，结果写入输出文件与错误文件，24 小时内未得到结果的请求按过期处理。有 OpenAI 兼容的 `/v1/batches` 与控制台「批处理任务」页两个入口；控制台创建对话框的选项（端点、完成时限范围、文件上限、可选的模型及其状态）由 server 的 `GET /api/v1/batches/dictionary` 下发，是否可取消（`cancellable`）也由 server 判定，控制台不自己推断。平台暂时服务不了的请求（503、429、5xx、被随时出让的部署让卡截断的 `abort`）重试到任务过期、不计失败，其余 4xx 为最终失败。见 `docs/how-to/batch-api.md`。
_Avoid_: 评测（那是[[评测]]，用负载压测并产出报告）、随时出让的部署（那是[[出让策略]]里的一个值，批处理可以指向它，但两者独立）、一次请求里的 batch 参数（引擎自己的批量大小）

### 推理网关

**令牌信息 — Key Information**:
网关对一个 API 令牌的自我说明，由 `GET /v1/key` 用该令牌鉴权后返回：令牌绑定的优先级类（不带请求头时用的类、请求头所能用到的全部类、更高的类按哪个类处理）、本 server 支持的网关能力（`features`，只增不减），以及令牌有权调用的每个模型有多少运行中的副本按优先级调度。供放在 GPUWorks 前面的网关在转发之前判断该怎么带头。它不是推理流量：不计入网关请求数、不留请求记录，从网关的路由表与优先级类快照回答，不读部署表。见 `docs/reference/openai-gateway.md` 与 ADR 0083。
_Avoid_: 令牌详情（那是管理面的 API 令牌页）、在上游网关里重写 GPUWorks 的优先级策略（按 `reachable` 与 `ceiling` 解析即可）

**运行位 — Run Slots**:
一个副本同时运行的请求数上限，取实例启动参数里明确写出的并发序列上限（vLLM `--max-num-seqs`、SGLang `--max-running-requests`，参数 `max_num_seqs`）；没写、值不是正整数、或数据并行规模不为 1 时未知。GPUWorks 不猜引擎的默认值，运行位未知的副本不参与[[繁忙婉拒]]。见 `docs/reference/openai-gateway.md`。
_Avoid_: 并发上限（那是管理面对请求数的静态限制）、引擎默认值（vLLM 自己的默认不算明确写出）

**繁忙婉拒 — Busy Rejection**:
调用方用请求头 `X-GPUWorks-If-Busy: reject` 声明「忙就别排队」时，GPUWorks 判断当前接不了就立即回 429 `MODEL_BUSY`（带 `Retry-After: 1`），由调用方改投别处；不带声明时行为不变。是否接得了由网关在选副本、占位的同一时刻判断，两个请求不会同时拿到最后一个空位。第一版的口径是：每个可选副本上不低于本请求优先级的在途请求数都已达到它的[[运行位]]；运行位未知的副本、PD 分离部署、多机部署、休眠或唤醒中的部署、连接失败后换副本的重发都不婉拒。多个 server 时各自只数自己转发的在途请求，结果只会少婉拒、不会误拒。[[令牌信息]]的 `models[].busy_reject` 说明哪些模型会婉拒。见 `docs/reference/openai-gateway.md`、`docs/explanation/request-priority.md`（上游网关接入）与 ADR 0083。
_Avoid_: 限流（婉拒不是对调用方的限额，不带声明的请求照常排队）、过载保护、在调用方按口径自己推算（只认状态码与 `error.code`）
