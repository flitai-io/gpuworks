# runveil 与 udocker

**一句话**：runveil 是 GPUWorks 在没有 docker（或 docker 给不了容器 NVIDIA 卡）的节点上用的容器运行时。它沿用 udocker 的磁盘布局和 PRoot
执行方式，但镜像处理和启动容器都由 agent 自己完成，不需要 udocker CLI，也不需要 Python。界面上统一叫
runveil；`udocker` 这个名字只留在线上值、配置项、目录名和兼容路径里。

## 为什么会有两个名字

早期没有 docker 的节点（AutoDL、CompShare、RunPod 等云容器实例）直接调用 udocker CLI：`udocker pull`、
`udocker create`、`udocker run`。udocker 是 Python 写的，用户态的 PRoot 模拟 chroot，不需要 root 和守护进程，
正好适合这类实例。但它慢：单流下载、拉完再用 Python 重新哈希每一层、每层解包起两个 tar 子进程单核解压，
而且节点上必须有能用的 Python。

后来 agent 把这些环节逐个换成了自己的实现，留下的只有 udocker 的**磁盘布局**和**PRoot 执行语义**。
这个实现就是 runveil。节点记录、API、命令行参数里的运行时取值仍是 `udocker`，以免已有节点和调用方失效。

## 现在各部分是谁

| 环节 | 默认（runveil） | 代码 |
|---|---|---|
| 运行时选择 | 没有可用的 docker、nerdctl，或在 NVIDIA 节点上它们给不了容器加速卡（缺 NVIDIA Container Toolkit 或 Docker 未配置）时自动选中；也可用 `--driver udocker` 或 `GPUWORKS_RUNTIME_DRIVER=udocker` 强制 | `internal/agent/app/runtime_select.go` |
| 拉取、导入、导出镜像 | agent 内的 native 实现：多层并发、大层分段下载、边下边解压 | `runtime/udocker/native` |
| 创建容器 rootfs | native 解包，写成 udocker 认得的 `containers/<name>/ROOT` | `runtime/udocker/native` |
| 启动、exec、终端、删除 | agent 直接调用内嵌的 patched PRoot，按 udocker P1 语义构造参数和环境，移植了 `setup --nvidia` | `runtime/runveil` |
| 排障与手工导入 | `gpuworks-agent runveil ps / images / load / exec / logs`，直接读写磁盘状态，不经过 agent | `runveilcli` |

udocker CLI 仍内嵌在 agent 里（离线包：udocker 源码、englib 和同一个 patched PRoot），只在以下情况按需
解压使用，这时节点需要 python3：

- 镜像是以前由 udocker CLI 登记的（旧布局），native 读不了；
- 设置了 `GPUWORKS_UDOCKER_DISABLE_NATIVE`：镜像处理退回 udocker CLI，用于 native 不支持的镜像（如 schema1 清单）；
- 设置了 `GPUWORKS_UDOCKER_LAUNCHER=udocker`：启动器也退回 udocker CLI。

这些开关的含义见[配置参考](../reference/configuration.md)，命令行见 [CLI 参考](../reference/cli.md#gpuworks-agent-runveil)。

## 磁盘上的样子

runveil 和 udocker CLI 读写同一棵目录，都在 agent 的 state 目录（安装目录）下：

```text
<state>/udocker/            UDOCKER_DIR
  layers/sha256:<hex>       镜像层与配置 blob
  repos/<仓库>/<tag>/        tag 登记：manifest，以及指向 layers 的符号链接
  containers/<name>/ROOT    解包后的容器根文件系统
  base/  prepared/          按 manifest 缓存的 rootfs，加快下次创建
  tarcache/                 大层预先解压成的 tar，加快创建
  bin/proot-<arch>          patched PRoot
<state>/udocker-state/      运行中容器的 pid、日志与启动规格
```

因此安装目录所在的文件系统要能存放镜像内容：文件名允许冒号（镜像层名、Debian 系镜像里的
`/var/lib/dpkg/info/<包>:amd64.list`），最好允许改属主，速度取决于这块存储。详见
[为什么 udocker 节点的安装目录不能放在网络卷上](../how-to/connect-nodes.md#为什么-udockerrunveil节点的安装目录不能放在网络卷上)。

## 容器里会看到宿主路径

PRoot 只翻译路径类系统调用，`/proc/self/maps` 由内核生成，里面登记的已加载库是**宿主路径**，
例如 `<state>/udocker/containers/<name>/ROOT/usr/local/lib/python3.10/dist-packages/torch/lib/libtorch_hip.so`。
有些运行库会按这个路径重新打开自己：HIP 在第一次启动 kernel 时据此读出 fat binary 里的代码对象，
FlashInfer、DeepEP、vLLM 等 CUDA 组件用它定位 `libcudart`。在容器里，这个宿主路径会被当作容器内路径再翻译一次，
文件不存在，于是海光 DCU 上第一个 kernel 报 `HIP error: invalid device function`，CUDA 上则是找不到库。

所以创建容器时，agent 会在 ROOT 里放一个回环符号链接，让宿主路径在容器里也能打开（`$ROOT` 是 ROOT 的宿主绝对路径）：

```text
${ROOT}${ROOT}  ->  ${ROOT}
```

它在启动阶段的「应用 GPU 运行时兼容修补」一步建立（`ensureHostRootLoopback`）。经 agent 和
`gpuworks-agent runveil exec` 启动的进程都已具备。**脱离 agent、手工用 PRoot 跑同一个 rootfs 时**，要自己补上这一项，
最简单的是把 rootfs 绑定到它自己的宿主路径：

```bash
proot -r "$ROOT" -b "$ROOT:$ROOT" ...
```

缺了它，`torch.cuda.get_arch_list()`、设备属性这类不经代码对象的查询仍然正常，只有真正启动 kernel 时才失败，容易误判成驱动或架构不兼容。

## 运行身份

udocker 拒绝以 root 运行，runveil 沿用这个约定：root 运行的 agent 会创建专用账号 `udocker`，把运行时目录交给它，
工作负载以它的身份运行。下面两种情况改为以 agent 自己的用户运行，原因写在节点诊断的「运行身份」里：

- 运行时目录所在的文件系统拒绝改属主（NFS 常见）；
- 在 udocker CLI 启动器下，专用账号跑不了 Python（例如 Python 只在 root 不可进入的 `/root/conda` 下）。

非 root 的 agent 直接以自己运行；加速卡设备节点的权限用 `gpuworks-agent grant-device-access` 补。

root 运行的 agent 通过补充组让专用账号访问设备节点。宿主机上只有 root 能访问的设备节点（`root:root` 且组和其他用户无任何权限）没有可用的属组，agent 每次启动工作负载前会把它们交给系统组 `gpuworks-runtime` 并加上组读写，工作负载以该组身份访问；其他节点保持宿主机原样。

## 线程很多的工作负载

PRoot 是单线程的 ptrace 跟踪器：容器里所有线程的每一次被跟踪的系统调用（路径类系统调用，seccomp 加速后 futex、mmap、
ioctl 不陷入）都由这一个进程处理。一个 8 卡张量并行的 SGLang 实例在 8×H200 的实例上会跑出约 3100–3700 个线程（每个 rank
360–400 个），跟踪器长期占满一个核，出现过这些现象：

- 147 个线程在 `t`（tracing-stop）里停了十几分钟，每个 rank 的主线程和全部 `pt_nccl_watchdg`、`pt_nccl_heartbt`
  线程都在其中；同时约 90 个更新的线程反复循环，它们的 tid 都比被饿住的线程大。
- 各 rank 的初始化被串行化，后一个 rank 比前一个多花约 70 秒；权重加载阶段卡住。

线程数不是某一个引擎独有：SGLang 会按主机核数给 scheduler 开线程；vLLM 自己只调 OpenMP 线程数，下表其余变量控制的
线程池不管（240 核主机上每个 TP worker 约 157 个线程），8 个 rank 的初始化同样被串行化。
有效的处理都是限制线程池。

**默认环境变量**：部署落在 runveil 节点（运行时 `udocker`）上、引擎是 SGLang 或 vLLM 时，
服务端在启动环境的系统层加入下面这组变量（`internal/server/deploy/runveil_env.go`）。它们和其他
系统层变量一样出现在部署表单的命令/环境变量预览里，与实际下发的一致：

| 变量 | 默认值 |
|---|---|
| `OMP_NUM_THREADS`、`MKL_NUM_THREADS`、`OPENBLAS_NUM_THREADS`、`NUMEXPR_NUM_THREADS`、`TORCH_NUM_THREADS` | `4` |

- 这组变量是在 Vast 8×H200 上一起验证的（DeepSeek-V4.1-Flash-1017，SGLang TP8）：每个 rank 的线程从约 390 降到 70–100，
  权重约 2 分钟加载完。没有逐个拆开验证，所以按验证过的组合原样下发，不按推测删减。
- 模板或部署自己写了同名变量时以它们为准；不想要某个变量时，在部署表单的「删除的环境变量」里填它的名字。系统层的值会盖过镜像
  自带的同名变量，要保留镜像的值也是删掉它。
- vLLM：在 runveil 上的 8×RTX 5090（240 核）运行 Qwen3-32B TP8，每个 worker 的线程从 157 降到 61，8 个 rank 的
  分布式初始化从约 145 秒降到约 57 秒，启动到就绪快约 2.3 分钟，吞吐、TTFT、TPOT 差异在测量波动内
  （[对比数据](../../artifacts/work/2026-10-20-runveil-many-threads/vllm-thread-cap-cuda-ab-2026-10-22.md)）。
  vLLM 在 `OMP_NUM_THREADS` 未设置时会自己在服务阶段把 torch 线程降到 1，设成 4 后它保持 4 并打一条警告；
  这次测量里看不出性能差别，减少线程的也不是这一项（只设另外四项结果相同），但仍按验证过的组合整体下发。
  910C 上同一 Qwen3.8-27B TP2 配方的 worker 从 732/730 个线程降到 65/64 个（镜像预置了 `OMP_NUM_THREADS=1`）。
- docker、nerdctl 节点不加：没有 PRoot，线程多不会拖慢系统调用。
- 对引擎性能：在 8×H100 上原生运行 Qwen3-32B TP8，设与不设这组变量的吞吐、TTFT、TPOT 差异在测量波动内，线程数从约
  1900 降到约 700（[对比数据](../../artifacts/work/2026-10-20-runveil-many-threads/plan.md#线程上限对引擎性能的影响2026-10-20vast-8h100-sxm)）。
  CPU 上工作多的配方（如主机内存里的 Engram 表）没有对照数据。

**没有加 `TORCH_NCCL_ENABLE_MONITORING=0`。** 读 PyTorch 源码（v2.4、v2.6、v2.8、main 的 `ProcessGroupNCCL.cpp`）：这个开关
在所有版本里都**不会**让 `pt_nccl_heartbt` 线程不创建，线程照常启动，开关只决定监控线程发现 watchdog 卡死后是否
杀掉进程。

关掉它不减少线程，只是让卡死的作业不再被自动终止，改变了故障行为，所以不作为默认值。要用的话在部署的环境变量里自己写。

**没有采用的做法：改 PRoot 的事件循环。** 上游每轮只 `waitpid(-1, __WALL)` 取一个事件；Linux 把新被跟踪的线程加到列表头部，
所以跟踪器忙不过来时，反复拿到的总是最新的忙线程，旧线程饿死，`get_tracee()` 还要线性扫描全部线程。

试过一个补丁（每轮用
`WNOHANG` 取尽待处理事件、先被跟踪的先处理、pid 哈希表），本地压力测试里主线程从 0 次提升到约 27 次/秒，真机上初始化也快了
很多，但 SGLang 权重加载阶段每次都卡住，所以**没有内嵌**。

真机对比和对原因的推测见
[计划](../../artifacts/work/2026-10-20-runveil-many-threads/plan.md)；压力测试脚本留在
[PRoot 构建说明](../../build/runtime-assets/proot/README.md)，改事件循环时可以直接用。

另一个相关的修复：PRoot 进程以前继承 agent 的当前目录。agent 以 root 在 `/root`（权限 700）下启动（例如 SSH 安装后用 nohup
起的 agent），PRoot 又降权为 `udocker` 用户时，PRoot 删除自己的临时目录后切不回 `/root`，会打出
`proot error: can't chdir to '/root': Permission denied`。

这条报错本身无害（退出码不变），但它常在 PRoot 退出时最后打出，
容器失败时会被当成失败原因显示，盖住引擎真正的错误。

现在 agent 启动的所有 PRoot 和 udocker 进程，以及容器终端、
`gpuworks-agent runveil exec`，工作目录一律是 `/`；容器内的工作目录由 PRoot 的 `-w` 决定，不受影响。
