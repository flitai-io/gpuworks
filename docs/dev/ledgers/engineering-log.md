# GPUWorks 工程日志

本文按问题保留修复经过、验证范围和原始证据。阅读时以各条目的状态与日期为边界；
条目中的“当前”“现在”对应记录形成时的情况，不能单凭历史验收推断后续版本的状态。

## Playground、网关与 GPUWorks `/v1` 数据面验收

- 状态：部分完成（PARTIAL，Playground 与 GPU3 验收已于 2026-08-20 完成；
  多语言翻译与额外云端覆盖仍待补充）
- 发现日期：2026-08-19
- 范围：直接 vLLM 启动之外的产品面

发现问题时，GPUWorks 尚无 Playground，也没有单独部署的网关。Server 现在
基于其内置的 OpenAI 兼容 `/v1/*` 反向代理，暴露一个 JWT 保护的 Playground。
公开的 `/v1` API key 鉴权未变；管理端 Playground 使用内部鉴权入口，绝不向
浏览器暴露 provider key。

GPU3 上 Qwen3-ASR vLLM 的直接验收记录见
`artifacts/archive/2026-08-18-roadmap/acceptance/omni-gpu3-vllm-real-acceptance-2026-08-19.md`。以下条目在对应产品面
落地前保持打开：

1. [x] 构建 Playground UI，覆盖对话、对比与音频转写。
2. [x] 使用内置代理边界，覆盖 JWT/API key 鉴权、SSE flushing、multipart
   保留、响应头与二进制 body。
3. [x] 在 GPU3 上跑通真实 GPUWorks Server + Agent 验收，包括通过 Server 的
   `/v1/audio/transcriptions`。见
   `artifacts/archive/2026-08-18-roadmap/acceptance/playground-real-acceptance-2026-08-20.md`。
4. [ ] 在所选后端支持的情况下补齐 `/v1/audio/translations` 覆盖，并在
   RunPod 与优云智算上重复相应检查。

## 修复 udocker 面向 CUDA 消费者的宿主机路径 loopback 映射

- 状态：已实现（IMPLEMENTED，2026-08-21）；自动化验证与四镜像真实 udocker
  验收已完成（RunPod/A40，Qwen3.5-0.8B）
- 发现日期：2026-08-19
- 范围：`internal/agent/runtime/udocker`
- 优先级：对于在导入模块（import）时，会使用 `/proc/self/maps` 暴露的文件名重新打开
  CUDA 库的 runner 镜像，属于严重问题

本条中的 guest 指容器内部，rootfs 是容器的根文件系统。loopback 映射指在容器内建立符号链接，
让程序通过 `/proc/self/maps` 读到的宿主机路径也能找到同一个库文件；它不是网络回环。

### 现象

vLLM 服务启动并变为健康，但 API server 与 engine 进程在探测可选的 `deep_ep`
模块时都会打印以下警告：

```text
Module deep_ep was found but failed to import
FileNotFoundError: [Errno 2] No such file or directory:
'/opt/gpuworks/udocker/containers/<container-id>/ROOT/usr/local/lib/python3.12/dist-packages/nvidia/nccl/lib/libnccl.so.2'
```

同样的映射缺陷在更新的 GPUStack runner import 路径下是致命的。例如
torchvision/FlashInfer 可能会尝试以如下路径重新打开一个 CUDA 运行时库：

```text
/opt/gpuworks/udocker/containers/<real-id>/ROOT/usr/local/lib/python3.12/dist-packages/torchvision.libs/libcudart.<hash>.so.13
```

当该 guest 路径不存在时，vLLM 的模型架构检查会在 engine 启动之前失败。这与
所选 vLLM 版本是否支持该模型架构无关。

不能把这个警告理解为 NCCL 本身不可用的证据。在受影响的容器上，以下各项均
成功：

```text
guest_exists True
maps_host_path_exists_in_guest False
ctypes_load ok
torch_nccl_version (2, 29, 7)
```

vLLM 也成功初始化了其分布式 backend：

```text
world_size=1 rank=0 ... backend=nccl
```

该验证部署随后达到 `running` 状态，`/health` 返回 HTTP 200，`/v1/models`
返回了正在提供服务的模型。

### 根因

`deep_ep.check_nccl_so()` 从 `/proc/self/maps` 读取已加载的 NCCL 文件名，
然后将该文件与其链接时使用的 NCCL 库做比较。在 proot 下，`/proc/self/maps`
暴露的是宿主机侧的 rootfs 前缀：

```text
/opt/gpuworks/udocker/containers/<real-container-id>/ROOT/usr/.../libnccl.so.2
```

该宿主机路径目前在 guest 内部无法解析，因此 `filecmp.cmp()` 调用会在
`os.stat()` 阶段就失败，根本走不到比较两个有效 NCCL 库这一步。vLLM 会捕获
这个可选 import 的失败并在没有 DeepEP 的情况下继续运行。

GPUWorks 已经会在 `rootfsFixups()` 中尝试创建宿主机路径的 loopback 符号
链接，但生成的路径与 `/proc/self/maps` 暴露的路径不匹配：

```text
created:
  /home/udocker/.udocker/containers/<gpuworks-container-name>/ROOT

required:
  /opt/gpuworks/udocker/containers/<real-udocker-container-id>/ROOT
```

存在两处独立的不匹配：

1. 实现里硬编码了 `/home/udocker/.udocker`，而 Agent 实际的 `UDOCKER_DIR`
   是 `/opt/gpuworks/udocker`。
2. 实现从具名容器符号链接（`gpuworks-...`）派生链接分量，而
   `/proc/self/maps` 中包含的是 udocker 真正的 UUID 目录。

相关代码：

```text
internal/agent/runtime/udocker/helpers.go:rootfsFixups
internal/agent/runtime/udocker/helpers.go:containerRoot
```

背景评估参考：`runpod` 研究仓 `udocker-offline/docs/udocker_evaluation.md` 第 6.1 节。

该评估要求在 `$R/$UDOCKER_DIR/containers/$ID` 处建立 rootfs loopback，专门
用于让从 `/proc/self/maps` 读到的宿主机路径在 guest 内部可解析。

### 影响

- 当前单 GPU Qwen 部署：无功能性影响；CUDA、NCCL、模型加载、健康检查与
  推理服务启动均成功。
- 包含 DeepEP 的通用 vLLM 镜像：会产生噪声警告，且 DeepEP 被禁用。
- DeepSeek MoE、专家并行（expert-parallel）或显式要求 DeepEP 的部署：可能
  产生实质性影响；即使基础 NCCL 运行时是健康的，启动或优化后的专家间通信也
  可能失败。
- 其他通过 `/proc/self/maps` 解析已加载共享库的框架，可能会遇到同样的
  宿主机前缀问题。

不要把用 `EP_SUPPRESS_NCCL_CHECK=1` 抑制这个警告当作平台层面的修复。抑制
会跳过一个合理的 NCCL 版本安全检查，并掩盖被破坏的路径映射问题。

### 已实现的设计

1. 用 `filepath.EvalSymlinks` 解析所选具名容器的 root，使规范化后的 root
   包含真实的 udocker 容器 ID。
2. 基于配置的 `d.udockerDir` 构建 guest loopback 目录，而不是硬编码
   `/home/udocker/.udocker`。
3. 创建等价于以下形式的链接：

   ```text
   <ROOT>/<UDOCKER_DIR-without-leading-slash>/containers/<real-id>/ROOT
     -> <canonical-ROOT>
   ```

4. 使创建过程幂等：在替换前先校验已存在的链接，且只替换那一个精确的链接；
   不删除或重写无关容器的路径。
5. 保留现有用于 NVIDIA shim 与 `/etc/hosts` fixups 的精确具名容器选择逻辑
   不变。

该实现还会对 `d.udockerDir` 本身做规范化，覆盖配置的安装目录本身就是一个
符号链接的场景。它会拒绝替换一个非符号链接的 loopback 路径。udocker
runtime 会提供 `FLASHINFER_DISABLE_VERSION_CHECK=1` 与
`SGLANG_SKIP_SGL_KERNEL_VERSION_CHECK=1` 作为兼容性默认值，同时允许具体
workload 的环境变量值覆盖它们。

### 自动化验证

新增单元测试，覆盖以下全部场景：

1. 具名容器路径是指向 UUID 目录的符号链接，与真实 udocker 目录结构一致。
2. loopback 在非默认 `UDOCKER_DIR` 下创建，例如 `/opt/gpuworks/udocker`，
   而不是 `/home/udocker/.udocker`。
3. loopback 使用真实的 UUID 目录，而不是 `gpuworks-...` 这个名字。
4. 重复调用 `rootfsFixups()` 是安全的，并能保持链接正确。
5. 已存在的过期或无关容器目录不会被修改。

至少运行：

```bash
go test ./internal/agent/runtime/udocker
go test ./...
```

### 远程验收流程

部署修复后的 Agent 后，使用一个新创建的容器；不要把旧的 rootfs 当作
provisioning 已被修正的证据。

1. 确认具名容器与真实 ID：

   ```bash
   ls -l /opt/gpuworks/udocker/containers/gpuworks-*
   ```

2. 确认新 rootfs 内部存在预期的 loopback：

   ```bash
   R=/opt/gpuworks/udocker/containers/<real-id>/ROOT
   readlink "$R/opt/gpuworks/udocker/containers/<real-id>/ROOT"
   ```

3. 从 guest 内部验证正常的 NCCL 路径与 `/proc/self/maps` 暴露的宿主机
   路径都可以解析：

   ```python
   import ctypes
   import os
   import torch

   guest = "/usr/local/lib/python3.12/dist-packages/nvidia/nccl/lib/libnccl.so.2"
   loaded = next(
       line.split()[-1]
       for line in open("/proc/self/maps")
       if "libnccl.so" in line
   )
   assert os.path.exists(guest)
   assert os.path.exists(loaded)
   ctypes.CDLL(guest)
   print(torch.cuda.nccl.version())
   ```

4. 在不抑制其 NCCL 检查的情况下 import DeepEP：

   ```bash
   python3 -c 'import deep_ep; print(deep_ep.__version__)'
   ```

5. 启动 vLLM 并确认日志中不再出现：

   ```text
   Module deep_ep was found but failed to import
   FileNotFoundError: .../libnccl.so.2
   ```

6. 确认部署到达 `running` 状态，`/health` 返回 HTTP 200，`/v1/models`
   返回预期的模型。

7. 当有合适的多 GPU 硬件可用时，运行一次 TP/EP smoke test，验证 NCCL
   初始化与 DeepEP 的实际运作，而不是只依赖单 GPU import 测试。

### 最终四镜像验收

单 GPU 真实验收记录在
`artifacts/archive/2026-08-18-roadmap/acceptance/udocker-four-image-real-acceptance-2026-08-21.md`。它同时覆盖了
两个 GPUStack runner 镜像与两个上游镜像，包括一次真实的模型加载和
OpenAI 兼容请求。未来的多 GPU TP/EP 测试仍然有价值，但与这里验证的
udocker 路径映射修复是相互独立的两件事。

### 首次复现的证据

最初复现使用了：

```text
Agent commit: 04a21f7
Deployment: 7 (myqwen-entrypoint-verify)
Instance: 3a5af132f5e0462dc5b1a425f6bb9240
Container ID: f1982811-4c10-34dc-a942-84b7d6abf009
Image: vllm/vllm-openai:v0.27.1
GPU: NVIDIA A40
```

这些标识符仅作为诊断证据；验收测试必须使用未来修复后的 Agent 新创建的
容器。

## 面向 nerdctl runtime driver 的 Ascend 设备注入

- 状态：已实现（IMPLEMENTED，2026-08-28）；设备注入与基础切分分配已在
  198.51.100.32 上完成真机验证
- 发现日期：2026-08-28
- 范围：`internal/agent/gpu/gpu.go`（`DriverAwareInjector`、`RuntimeMount`），
  `internal/agent/gpu/ascend.go`（`RuntimeInjectionForDriver`），
  `internal/agent/engine/engine.go`（`applyDeviceInjection`）

`internal/agent/runtime/nerdctl`（2026-08-28 新增）为 GPUWorks 提供了一个
面向 containerd/nerdctl 主机的通用容器生命周期 driver —— `198.51.100.32`
（`huawei-ascend-node`）上只有 `nerdctl 1.7.6`，完全没有 Docker daemon，此前
无法纳管。这个通用 driver（run/stop/rm/status/logs/exec/pull/inspect/
save/load）已实现、已做单测，并在 `.32` 上针对真实 `nerdctl` 完成验证
（经代理 pull、run、inspect、exec、rm，并确认容器从未泄漏进 nerdctl 默认的
`k8s.io` namespace）。

具体实现：`gpu.RuntimeInjection` 新增了 `Mounts []RuntimeMount` 字段，并新增
一个可选的 `DriverAwareInjector` 接口
（`RuntimeInjectionForDriver(indexes []int, driverName string)`），让 prober
可以按容器 runtime 分支处理。`engine.applyDeviceInjection` 在 prober 实现了
该接口时会分发给它，否则回退到现有的 `RuntimeInjection(indexes)`，并且现在
会把 `inj.Mounts` 传播进 `WorkloadSpec.Mounts`。

`AscendProber.RuntimeInjectionForDriver` 让除 `nerdctl` 之外的所有 driver
都维持现有的 `--runtime ascend` + 仅 env 的路径不变。对于 `nerdctl`，它改为
直接透传未改名的 `--device /dev/davinciN`（每个选中的 index 一个，外加
`davinci_manager`/`devmm_svm`/`hisi_hdc`），以及五个只读的宿主机
driver/DCMI 库 bind mount（`/usr/local/dcmi`、`/usr/local/bin/npu-smi`、
`/usr/local/Ascend/driver/{lib64,version.info}`、
`/etc/ascend_install.info`）—— 不需要挂载完整的 CANN toolkit，推理镜像自身
已经内置了它。nerdctl 下省略了 `ASCEND_VISIBLE_DEVICES`（没有 Docker
Runtime 插件时不会有任何东西读它）；保留了 `ASCEND_RT_VISIBLE_DEVICES`/
`NPU_VISIBLE_DEVICES`，因为 ACL/torch_npu 会直接读取它们。

已在昇腾池成员上做过两次真机验证：第一次是手动用这组精确参数针对
缓存的 `quay.io/ascend/sglang:v0.5.14-cann9.0.0-910b` 镜像执行 `nerdctl run`
（`torch.npu.is_available() == True`、`device_count() == 1`），第二次是走
真正的生产代码路径 —— `test/l4/cmd/ascend-nerdctl-verify` 直接调用
`gpu.AscendProber.RuntimeInjectionForDriver` 与真实的
`internal/agent/runtime/nerdctl.Driver`（Run/Exec/Remove）。它由
`TestAscendNerdctlDeviceInjectionEndToEnd` 执行
（`make test-l4-ascend-nerdctl-devinject`，接入 `l4-ascend-pool-partition-inference`
这个 CI job）。目标机（实测 `198.51.100.31`：aarch64、有 nerdctl、无 docker、
无 Go）的网络链路批量传输只能维持约 45 KiB/s（与本代码无关 —— 即使是几 MB
的原始 `ssh ... cat >` 管道也会像 `scp` 一样卡住），因此测试通过普通 SSH
stdin 管道发送一个 gzip 压缩的二进制文件，而不是用 `scp`；GPU 节点上从不
调用 `go build`。

2026-09-15 修订：该 helper 过去以约 9 MiB 的 gzip 资产签入仓库
（`test/l4/testdata/ascend-nerdctl-verify-linux-arm64.gz`），并靠
`make check-ascend-nerdctl-verify-asset` 在 core CI 里"重新交叉编译 + 逐字节
比较"来判断是否过期。这条规则本身没错，但代价与收益不匹配：helper 链接了
agent 的一大片包（accelerator、proto、agent/gpu、agent/runtime、
runtime/nerdctl、agent/engine、assets、engines、catalogasset、enginelog），
自引入以来闭包里 48 个提交、资产被改过 20 次（其中 8 次的标题就是"重新生成
资产"），而且**任何 rebase 都会让提交里那次重新生成失效**，于是每个 MR 都要
为一个只被 L4 用例使用的产物背一条门禁，历史里还堆了 24 个 blob（约 202 MB）。

现在改为：由 L4 用例在 runner 上从被测 revision 交叉编译
（`ascendNerdctlVerifyLinuxARM64Gzip`，缓存后 1–2 秒），签入资产、字节比较
门禁与 `build-asset.sh` 一并删除；core CI 不再校验，L4 lane 新增
`TestAscendNerdctlVerifyHelperBuildsForTheFixture` 只证明它仍能为 fixture
交叉编译。曾经的 `.gz`/工具链可复现性前提（比载荷不比 gz、`GOTOOLCHAIN` 钉住
`go.mod` 的版本）随之失效 —— 它们当初解决的是"签入产物"的问题。

签入产物时代的两个可复现前提（只比解压后的载荷、用 `GOTOOLCHAIN` 钉住 `go.mod`
声明的工具链）也随之作废：go1.26.6 与 1.26.7 从同一份源码产出的二进制不同，所以
那时的逐字节比较必须先钉住工具链，而不同 gzip/zlib 版本对同一载荷也会产出不同
的 deflate 流（实测相差 1821 字节），所以只能比载荷。现在二进制每次从被测
revision 编译，这些差异不再参与任何判断。

`go.mod` 的 go 指令仍刻意与 CI 镜像 `golang:1.26` 的实际版本保持一致（当前
1.26.7）：Go 在宿主版本低于指令时会自动拉取工具链，约 309 MiB（69 MiB 下载 +
240 MiB 解压）会落进 `GOMODCACHE`，而 `GOMODCACHE` 没有自动 GC，每代缓存都要
付一次。保持对齐就是让 CI 侧这一步恒为空操作；代价转移到本地 Go 版本较旧的
开发者身上（Go 会自动拉取，一次性）。

隧道双通道的吞吐/延迟断言默认关闭（`GPUWORKS_TEST_PERF=1` 才跑，命令见
`skipUnlessPerf` 注释）。它们量的是进程内网络仿真的墙钟时间，结果跟着宿主
机负载走而不是被测代码：同一个 ctrl 延迟用例在空载机器上测得 22/31/50 ms（最后
一次是 12 核全打满），但全量测试与真实 CI job 在同一台机器上并发时冲破了 500 ms
上限；legacy 单通道吞吐在同一容器内连续两次跑出 396 与 476 Mbps。会随宿主漂移的
上下限在 CI 里是抛硬币，不是回归信号。门控只关掉测量本身，通道路由、data lane
挂载与回退、字节完整性、重连这些确定性断言照常无条件运行。

此前认为 `.32` 不支持切分的说法是错误的：nerdctl 没有 Ascend Docker
Runtime 插件，但 GPUWorks 可以自己传入原始设备节点/挂载，vcann-rt 依然会使用
宿主机的 device-share 能力。`TestAscendNerdctlPartitionCapabilityProbeEndToEnd`
现在会调用生产环境的 `Engine.BuildPartitionProbeWorkload` 路径，通过真实的
nerdctl driver 加载已内置的 2.5 MiB `gpuworks/partition-probe:v1` 镜像，并
断言 `dsmi_get_hbm_info` 精确报告 4096 MiB。它不依赖任何 serving 镜像，也
不加载 Python、模型、vLLM 或 SGLang。

一次中间诊断曾错误地把 CANN8.5 与 CANN9 的资产路径当作不同的 ABI。二进制
比对证明这个假设是错的：两个 arm64 文件的 SHA256/BuildID 完全相同，两个
amd64 文件也是如此。它们是同一个 vcann-rt 构建，只是被复制进了不同的
build-matrix 目录；版本号是溯源/校验元数据，不是运行时选择的 key。镜像
版本元数据仍然只是一个提示性警告。节点能力来自这个小型行为探针；未来针对
具体镜像的探针必须真正在目标镜像的用户态里执行，产品才能宣称该镜像已通过
切分验证。

已提交的 helper 支持 `GPUWORKS_KEEP_ON_FAILURE=1`：失败时最终容器及其
精确的切分状态目录会被保留并打印出来供调试；成功的运行会自行清理。见
`docs/ascend-support-matrix.md`。

## Ascend 自有资源池 platform-API 切分推理

- 状态：已实现（IMPLEMENTED）并已从 mirror1 完成真机验证（2026-08-28）
- 范围：`test/l4/partition_ascend_pool_platform_test.go`、`Makefile` 的
  `test-l4-ascend-pool-partition-inference`、`.gitlab-ci.yml` 的
  `l4-ascend-pool-partition-inference`

`.31` 与 `.32` 是两台供测试使用的主机（fixture）；其中一台不可用，不再导致整组测试一起失败（fail-together）。CI 把
它们当作一个按能力选择的资源池：不可达或不合适的成员会被记录下来，然后
使用第一个同时满足 SSH 可用、有空闲配置好的卡、有本地模型和应用镜像的
成员。一个当前自包含的 arm64 agent 会被上传到唯一的
`/tmp/gpuworks-ascend-pool-*` 目录，并在一个唯一的 loopback 端口上启动；
测试从不覆盖或重启宿主机上已有的 `gpuworks-agent` 服务。

已提交的当前 arm64 agent 在 `mirror1:~/workspaces/gpuworks_build`（CI job
本来就在那里运行）构建，在内存中做 gzip 压缩，然后通过一个随机的短期私有
HTTP URL 提供给所选节点下载。这样同时避免了远程编译和实测约 5 KiB/s 的
SSH 文件传输路径。agent 的 `server_dial` listener 使用带 token/HMAC 鉴权的
随机私有端口，因为自有节点的 sshd 在管理策略上禁用了 TCP forwarding。

`TestAscendPoolPlatformPartitionInferenceAndWholeGPU` 通过
`POST /api/v1/models/local` 注册真实本地模型，通过
`POST /api/v1/deployments` 在同一张物理卡上创建两个 8 GiB 副本，等待两个
真实 vLLM 容器就绪，调用 `/v1/chat/completions`，并验证在两个由 platform
创建的容器中超配额的 ACL 分配都会被拒绝。随后通过 API 停止该切分部署，
再通过 API 创建一个整卡（whole-GPU）部署，并完成第二次真实对话。较早的
直接 Docker L4 测试仍然是有用的诊断手段，但不再作为 platform 链路覆盖的
CI 结论依据。

真实运行结果：nerdctl 成员通过了真实的设备注入；platform 测试观察到卡 7
繁忙，自动选择了空闲的卡 1，拉起了两个 8 GiB vLLM 副本，完成了对话，两个
容器均正确拒绝了超配额分配，随后停止它们并完成了整卡部署/对话。运行耗时
约 10 分钟，并验证了临时 agent 目录与两个容器都被精确清理。

## 将 Gitee CI 扩展为完整的 server+agent 部署+对话覆盖（整卡、非切分）

- 状态：已完成（DONE，2026-08-28）
- 范围：`test/l4/partition_gitee_deploy_chat_test.go`；`.gitlab-ci.yml` 的
  `l4-gitee-ascend-deploy-chat`

`TestGiteeAscendPartitionCapability` 此前只断言了切分能力探针的结果（在
Gitee 上结构性受限 —— 见
`artifacts/archive/2026-08-28-gitee-ascend-verification/device-share-limitation-2026-08-28.md`）。
新增了 `TestGiteeAscendFullServerAgentDeployChatEndToEnd` 和
`TestGiteeAscendARM64ContainerInContainerDeployChatEndToEnd`
（`test/l4/partition_gitee_deploy_chat_test.go`），用于在 Gitee 上覆盖
Huawei/Ascend 兼容性面的其余部分：真实 x86_64/CANN9 与 arm64/CANN9 实例
租用、真实 server+agent 上线、通过真实 `POST /api/v1/deployments` API 的
真实整卡（非切分 —— 已确认它从不调用
`applyAscendPartition`/`EnsureDeviceShare`，因此不会触发 device-share
限制）vLLM 部署，以及一次真实的 `/v1/chat/completions` 往返。镜像使用
Huawei 自己的 SWR registry
（`swr.cn-south-1.myhuaweicloud.com/gpustack/runner:cann9.0-910b-vllm0.20.2`，
可从 Gitee 实例的公网出口访问）；模型是 ModelScope 的 `Qwen/Qwen3-0.6B`。

两个完整用例都在各自的外层租用实例内运行一个 CANN9 应用镜像。arm64 用例
有意租用了合法的 CANN9/910B1 外层镜像：Gitee 的 catalog API 目前列出了一个
arm64/CANN8.5 的外层镜像，但 Rent 会以 `镜像和主机不匹配` 拒绝这个组合，
因此把它当作可租用的 fixture 会造成虚假覆盖。

同时也测试了在 CANN9 外层的
PRoot 边界内运行 CANN8.5 应用镜像的情形：容器能够启动，但 CANN FE 无法对
`aarch64-linux/lib64/../data/platform_config` 做 realpath，torch_npu 会在
模型权重加载之前就以 `Get ACL JitCompile default value failed` 失败。这与
上游 CANN8.5 容器本身的路径解析失败问题一致，仍是一个明确的兼容性缺口，
而不是一个能通过 CI 的结论。

能力探针与完整部署使用了各自独立、语义清晰的
job：`l4-gitee-ascend-partition-capability` 与 `l4-gitee-ascend-deploy-chat`。
后者仍然是一条明确的低频路径。两个完整部署用例都显式断言
`runtime_driver=udocker`，证明 Huawei SWR 的应用镜像是内层容器，而不是
外层租用镜像。切分相关的验证运行在自有 Ascend 资源池上
（`l4-ascend-pool-partition-inference`），不受 device-share 限制。

已于 2026-08-28 完成真机验证（x86_64 约 21 分钟；arm64/910B1 约 29 分钟；
清理后零云资源泄漏）。在此过程中发现并修复了三个根因，全部是与本测试本身
无关的真实 bug：

- 多架构 udocker prefetch 此前总是选择 `linux/amd64`；现在会选择
  `linux/<runtime.GOARCH>`，registry-stub 测试同时断言 amd64 与 arm64 的
  子 manifest。ARM CANN9 真实运行下载了预期的 26 层 ARM layer
  （6,873,397,151 字节），创建了容器，初始化了 CANN/NPU，达到健康状态，并
  完成了一次经由 platform 网关的对话。

- `internal/agent/runtime/udocker/optimize.go` 里的 `rewriteGzipAsTar` 此前
  使用单遍 `gzip.Reader`，会在第一个成员的 EOF 处停止。Docker/OCI 的 layer
  可能是同一个 blob 内多个拼接在一起的 gzip 流（SWR 镜像那层 4.1 GB 的
  layer 就是这种情况），因此这会悄悄产生一个被截断的普通 tar，导致
  `udocker create` 以 "Error: while extracting image layer" 失败。修复方式
  是通过 `gzip.Reader.Reset` 循环处理，直到底层文件被读尽为止。
- Gitee 的单 GPU 实例只有 30 GB 系统盘；7 GB 以上的镜像加上其解压后的
  layer 缓存会把它耗尽。Gitee 实例总是自带一块挂载在 `/data` 的 60 GB
  数据盘，因此给 `cloudprovider.Instance` 新增了 `DataDiskPath` 字段
  （Gitee 上设为 `/data`），并通过 `installCloudAgent` 传入
  `provision.SSHInstallRequest.InstallDir`，使 agent 的整个状态目录
  （models、udocker image layer）落在数据盘而不是系统盘上。

同一批次里还修复了：`Gitee.get()` 此前没有重试机制，导致 `Destroy()` 里
所有权校验调用只要出现一次瞬时 GET 失败，整个清理流程就会中止，留下一个
真实计费中的实例继续运行（在本次会话的验证运行中命中过一次，已手动处理）。
`get()` 现在会以线性退避最多重试 3 次。
