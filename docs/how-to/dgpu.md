# 启用和排查 dGPU

本文说明怎样确认 GPU 可用的 dGPU 能力、创建带显存上限的部署，以及调整已有部署的配额。

**适用场景**：多个模型实例共用一张物理卡，或带 dGPU 显存限额的部署被拒绝，需要排查原因。

**前置条件**：

- 节点已接入并上报 GPU（见[接入节点](connect-nodes.md)）。
- NVIDIA：x86_64、glibc ≥ 2.17，宿主机驱动提供动态库（`libcuda.so.1`、`libnvidia-ml.so.1`）。
- 昇腾：宿主机的 device-share 权限满足 vcann-rt 的要求，所选推理镜像提供 glibc ≥ 2.33；能接入并整卡运行，不代表宿主机已允许 dGPU 设备共享。
- PPU：配额语义是显存限额加繁忙时的加权 QoS。20/80 配置表示繁忙时的相对吞吐权重，不是硬 SM 分区。
- AMD：x86_64、Docker、nerdctl 或 runveil，rocm-slicing-shim（libvrocm.so）做硬显存限额，不支持算力（SM/CU）上限；当前只在 MI300X（gfx942）上实测。
- 海光 Z100 / K100_AI / BW：实验性，支持 x86_64 上的 Docker 或 runveil/udocker，每实例一张卡；需安装原生 helper、宿主 DTK/HYHAL/DCGM 库并通过逐卡探针。CU 随显存比例分配，不支持独立算力配额；仅报告系列 K100 或其他未适配型号时仍只支持整卡。PRoot 不提供独立 cgroup 或内核只读挂载，不代表敌对程序的安全隔离。详见[海光 Z100 / K100_AI / BW dGPU](../reference/support-matrix.md#海光-z100--k100_ai--bw-dgpu)。
- 想先走一遍完整流程，见[一张 GPU 跑两个模型](../tutorials/one-gpu-two-models.md)；语义与边界见 [dGPU 是什么、又不是什么](../explanation/dgpu.md)。

各厂商的支持范围见[支持矩阵](../reference/support-matrix.md)。

## 确认能力并部署

1. 打开**GPU 设备**页，查看目标 GPU 详情中的 **dGPU** 能力及原因。只有状态为 `ready` 的 GPU 才能创建带配额的实例。
2. 在[部署模型](deploy-models.md)时填写 **dGPU 显存上限**，检查候选节点与显存评估结果，再提交。平台根据节点硬件选择已就绪的后端；NVIDIA 对应 HAMi-core、昇腾对应 vcann-rt、PPU 对应 ppu-slicing-shim、AMD 对应 rocm-slicing-shim、海光 Z100 / K100_AI / BW 对应原生 vdev，无需手动指定。
3. 实例启动后，回到 **GPU 设备 → dGPU**，确认切片所属部署、节点和显存上限。需要验证两个模型共卡时，按[一张 GPU 跑两个模型](../tutorials/one-gpu-two-models.md)完成调用检查。

能力以页面返回的 `status` 和 `reason` 为准：

- `ready`：运行时、dGPU 资产及该后端要求的逐卡探针条件全部满足，可以创建带配额的实例；NVIDIA 与 AMD 豁免行为探针。
- `unavailable`：任一条件不满足，`reason` 说明具体原因，例如 agent 版本过旧未上报探针、
  缺少 dGPU 资产、运行时未验证容器推理、探针尚未完成或探针失败。

| 不可用原因 | 处理 |
|---|---|
| agent 版本过旧 | [升级 agent](upgrade-and-compatibility.md)，等待重新上报能力 |
| 缺少 dGPU 资产 | 按原因中列出的资产检查节点安装及 agent 日志 |
| 探针尚未完成 | 等待探针结果，再查看该 GPU 的状态 |
| 运行时未通过或探针失败 | 查看原因与 agent 日志，处理后确认恢复为 `ready`；也可选择已就绪的节点 |

经过行为探针的后端（除 NVIDIA、AMD 以外；二者就绪只看内嵌资产，豁免探针）另有独立的
`probe_status`（`passed`/`skipped`/`failed`），用于诊断 dGPU 探针本身
的结果，不等同于上面的 `status`。

`unavailable` 时改成整卡部署不能作为 dGPU 验证通过的证据。

## 调整 dGPU 配额

此入口调整的是切片**所属部署**的配额，只支持已有 dGPU 配额的单机部署；整卡、多机和 PD 部署不能在这里调整。

1. 在 **GPU 设备 → dGPU** 找到切片，确认它所属的部署，点击**调整配额**。
2. 填写新的显存上限并保存。API 为 `PUT /api/v1/deployments/{id}/dgpu`，请求示例为 `{"vram_limit_gib": 12}`。
3. 检查立即生效与待重启的实例数。GPU 页显示**重启后 X GiB**的实例仍使用原上限，按下表处理；不要仅凭“保存成功”认定所有实例已切换。

保存后：

- 部署之后启动的实例按新上限启动。
- 支持安全在线调整的运行中实例立即改上限：调度记账按新上限计，HAMi-core 从下一次分配起按新上限限制，实例不重启。
- 引擎能不能用上新上限取决于引擎：vLLM、SGLang 在启动时按上限切 KV 缓存，到部署页**重启实例**后才会用上；按需分配显存的引擎直接用上。

以下实例保持原上限，到重启时才按新上限运行（GPU 页显示「重启后 X GiB」，返回的 `pending` 列出每个实例与原因）：

| `reason` | 情况 |
|---|---|
| `backend` | 后端需重启实例才能应用新上限（昇腾 vcann-rt、PPU ppu-slicing-shim、AMD rocm-slicing-shim、海光 Z100 / K100_AI / BW 原生 vdev） |
| `in_use` | 调低时，实例已占用的显存超过新上限——当场调低会让引擎下一次分配失败而退出 |
| `starting` / `asleep` | 调低时实例正在启动或休眠，之后需要的显存可能比现在多 |
| `no_usage` | 调低时实例没有上报显存用量 |

拒绝（原配额不变）：

| code | 含义与处理 |
|---|---|
| `DEPLOYMENT_NEVER_FITS` | 新上限下没有任何节点能跑这个部署（比如比卡还大）；与创建时的检查相同 |
| `DGPU_QUOTA_NO_ROOM` | 调高时，实例所在的卡在扣掉它自己的占用后放不下新切片（`params` 给出节点、卡号、可用与需要的 GiB）；调小上限，或先腾出这张卡 |
| `DGPU_QUOTA_NOT_PARTITIONED` | 部署使用整卡，没有 dGPU 配额 |
| `DGPU_QUOTA_NOT_EDITABLE` | 多机或 PD 部署逐 rank 放置，不在这里改 |
| `DGPU_QUOTA_INVALID` | 上限不是正数 |

部署时间线会记一条「dGPU 显存上限 A → B」及立即生效与待重启的实例数。实例因为用量顶到上限而退出时，失败原因写作
`dGPU memory limit reached on GPU N: the engine needed X GiB, its limit is Y GiB`。
