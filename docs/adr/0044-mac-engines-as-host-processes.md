# ADR 0044: Mac 节点的引擎以宿主机进程运行，镜像是 darwin 环境包

日期：2026-10-14 · 状态：已接受

依据：[Apple Silicon 推理调研](../../artifacts/research/2026-10-13-apple-silicon-inference/README.md)、[Mac 节点计划](../../artifacts/work/2026-10-13-apple-silicon-nodes/plan.md)（含真机 spike 记录）。

## 背景

Apple silicon Mac 的 GPU 只有 macOS 宿主机上的进程能经 Metal 使用。macOS 上所有 Linux 容器（Docker Desktop、OrbStack、apple/container）都跑在虚拟机里，虚拟机里没有 Metal；唯一能用 GPU 的路径（Podman + krunkit，Vulkan 转 MoltenVK）在 M2 Max 上把 llama.cpp 的解码速度减半。agent 现有的三个运行时（docker、nerdctl、runveil）都以 Linux 镜像为单位：runveil 的执行核心是 Linux ptrace 上的 PRoot，镜像里是 Linux ELF。

## 决策

1. **新运行时 `host`**（`internal/agent/runtime/hostproc`）：引擎是 agent 用户的普通进程组，在 macOS 上优先于 Docker Desktop 被选中（`--driver host` 可强制）。进程托管（pid、退出码、组停止、日志）与 runveil 共用 `runtime/procgroup`。
2. **镜像是环境包**：darwin/arm64 的 OCI 镜像，内容是一个可搬迁的根（python-build-standalone、装进其 site-packages 的引擎、`/bin` 下的入口），由 `build/host-envs` 在 Linux 上交叉构建，按 `gpuworks-runner:metal<最低 macOS>-<引擎><版本>-rN` 发布（`CLAUDE.md`「内置镜像命名」）。拉取、离线导入、镜像源与目录沿用现有机制（runveil 的原生拉取，按 OS/架构选 manifest）。
3. **按容器语义翻译路径**：每个 manifest 解包一次，供它的所有实例共享；解包时把包内的绝对符号链接与 venv 的 `pyvenv.cfg` 重定根；启动时把 argv、环境变量、`PATH`、工作目录里的挂载目标（`/model`）换成宿主机源路径，包内路径换成解包位置；`--served-model-name` 的值是名称不是路径，原样保留（未设别名的部署以 `/model` 为名，网关按这个名字请求）；不宣告 `launch.name_verbatim` 的旧 agent 会翻译这个值，服务端对这类节点的未设别名启动不带该参数、也不期望挂载路径名，部署预览同样如此。服务端与 agent 都升级之前已放置在 Mac 节点上的未设别名实例，保存的启动参数不会自动重建，需滚动重启或重新部署一次才会带上名称，否则网关按 `/model` 请求仍得到 404。服务端照旧下发容器形状的启动参数，不知道节点是否有容器。
4. **实例隔离**：每个实例独立的 `HOME`、短路径 `TMPDIR`（`/tmp/gpuworks-host-<uid>/…`，Unix socket 路径不超过 macOS 的 104 字节）、按环境包共享的缓存；不继承 agent 的 `PATH`、`DYLD_*` 与 Python 变量；运行期间持有 `caffeinate -i` 防空闲睡眠。引擎只绑 127.0.0.1，网关经 hub 隧道访问。
5. **厂商 `apple`、框架 `metal`**：GPU 报 1 张统一内存卡，显存为 Metal 允许 GPU 常驻的工作集（`iogpu.wired_limit_mb`，未设时内存 ≤32 GiB 取 2/3，以上取 3/4）；平台不修改该值。driver 版本即 macOS 版本，镜像兼容性按最低 macOS 判断。
6. **平台功能按厂商声明**（`accelerator.Supports`）：Apple 节点不提供引擎休眠（vllm-metal 的 `/sleep` 返回 200 但不释放内存）、多机与 KV 磁盘层，调度器按需要的功能过滤节点。
7. **按内存共享卡**（`accelerator.SharedByMemory`）：一台 Mac 同时跑多个实例；每个实例按准入时的份额记账（vLLM/SGLang 取 `gpu_memory_utilization × 工作集` 与启动需求中较大者，callable 取启动需求），份额记在实例期望状态的 `memory_claim_bytes` 里。

## 取舍

- **宿主机进程 vs 容器**：放弃容器的隔离（文件系统、网络、cgroup），换来不损失 GPU 性能；隔离靠实例目录、环境变量清理与只绑回环。个人电脑上用户自己的进程与引擎共享内存，平台无法替用户约束。
- **自建环境包 vs 依赖用户安装（brew、官方脚本）**：多一条构建线、每个包数百 MB，换来版本钉死、M1–M5 同一个包、国内镜像源可用、运行时不联网。
- **路径翻译 vs 让服务端为 Mac 生成宿主机路径**：翻译集中在 agent 的一个包里，服务端、模板与预览不分叉；代价是翻译只覆盖整 token、`--flag=value`、`KEY=value`、冒号路径表和脚本里以单词出现的挂载目标。
- **份额持久化**：新增 `memory_claim_bytes` 一个事实，而不是每次快照按估算重算——重算既随模板与估算漂移，也违反逐实例全量计算的规模约束。

## 后果

- vLLM 沿用 `vllm` 引擎与参数字典（vllm-metal 接受 `vllm serve` 的命令行），macOS 上 gloo 的回环网卡由 quirk 改为 `lo0`。不支持的能力在 Mac 节点上由调度器给出带厂商名的拒绝原因。
- mlx-vlm、mlx-audio 等按请求选模型的服务走 callable 模板，包内适配层（`gpuworks_adapters`）把实例钉在 `--model` 上，因为网关会把请求的 `model` 改成部署服务名。
- 新的 darwin 平台差异（安装用 launchd、`sysinfo` 走 sysctl、终端的 pty 与进程回收走 darwin ioctl 和 `kern.proc`）都在 agent 侧；实例终端进入的是实例的环境（环境包的 PATH、实例的 HOME 与工作目录），不是容器；server 的 darwin 构建与 agent 同机即"单机一体"，安装流程与 Linux 节点一致。
