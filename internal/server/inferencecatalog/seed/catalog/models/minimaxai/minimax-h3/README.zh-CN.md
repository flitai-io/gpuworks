# MiniMax H3

MiniMax H3 是音视频生成模型。GPUWorks 可从 Playground 提交异步视频任务，并持续查询任务状态直至下载结果。

## 昇腾档位

当前昇腾档位经过 4 张 910B4 32 GiB 真机验证。模板会自动选择模型仓库中的 `FL2VA` 目录，保留仓库原有目录结构，并按 TP4、SP1 启动 SGLang Diffusion。用户不需要手工填写这些参数。

建议从 512 像素短边开始生成。实测 896×512、约 4 秒视频可完成，单卡峰值显存约 29,424 MiB。

当前档位不支持 910C/A3。上游 MiniMax H3 的 SGLang diffusion full-loop denoise 仍硬依赖 CUDA，替换 attention 后端后也会进入 CUDA JIT；把上下文改为 NPU 不能消除该限制。只有上游提供可用的 NPU denoise 路径并通过真机验证后，才能新增 910C 档位。

## NVIDIA 档位

NVIDIA 档位按 vLLM-Omni 官方 MiniMax H3 recipe 配置，每档固定卡数：

| 档位 | GPU 数 | 适用的卡 | 拓扑 | 步数 | 镜像 |
|---|---:|---|---|---:|---|
| 通用 | 1 | Ampere 及以上 | DiT 与文本编码器逐层 offload（容量路径，较慢） | 50 | GPUWorks vLLM-Omni 0.30 runner |
| Blackwell | 4 | Blackwell | Ulysses 4，不 offload | 50 | 同上 |
| RTX 4090 | 2 | RTX 4090 | TP2 + 分布式逐层 offload，常驻 12 个 DiT 块 | 50 | 同上 |
| RTX 5090 | 2 | RTX 5090 | TP2 + 分布式逐层 offload，常驻 20 个 DiT 块 | 50 | 同上 |
| RTX PRO 5000 | 4 | RTX PRO 5000 | TP2 × Ulysses 2，不 offload | 50 | 同上 |
| Turbo 4 步 · 2 卡 | 2 | Ampere 及以上、单卡 ≥ 24 GB（3090、4090、5090、A100、L40S 等） | TP2 + 分布式逐层 offload，32 GB 以上的卡常驻 20 个 DiT 块、24 GB 的卡常驻 12 个，加载 LightX2V Turbo LoRA | 4 | `vllm/vllm-omni:minimax-h3-image` |
| Turbo 4 步 · 单卡 | 1 | Ampere 及以上、单卡 ≥ 32 GB（5090、A100、L40S、RTX 6000 Ada 等） | 单卡分布式逐层 offload，常驻 12 个 DiT 块，加载 Turbo LoRA | 4 | 同上 |
| FastH3 Omni NVFP4 · 单卡 | 1 | Blackwell、单卡 ≥ 32 GB（5090、RTX PRO 6000、B200、DGX Spark 等） | FastVideo 的 NVFP4 蒸馏版，DiT 与文本编码器逐层 offload | 8 | 同上 |

### 怎么选

以下数字为单个请求的端到端耗时，输出带音轨的 H.264：

| 档位 | 实测的卡 | 1344×768 · 5 s | 832×480 · 15 s | 1344×768 · 15 s |
|---|---|---:|---:|---:|
| RTX 5090 · 2 卡（50 步） | 2 × RTX 5090 | 约 13 分钟（每步约 15 s） | — | 30 分钟以上 |
| Turbo 4 步 · 2 卡 | 2 × RTX 5090 | 68 s | — | 274 s |
| Turbo 4 步 · 2 卡 | 2 × RTX 3090 | 184 s | 225 s | 24 GB 显存不足（768p · 10 s 可以，507 s） |
| Turbo 4 步 · 单卡 | 1 × RTX 5090 | 113 s | 108 s | 32 GB 显存不足 |
| FastH3 Omni NVFP4 · 单卡 | 1 × RTX 5090 | 52 s | 68 s | 32 GB 显存不足 |

- **慢在步数。** 50 步的 2 卡档每步约 15 s，把 DiT 量化成 FP8 全部常驻显存后每步仍是 15 s：瓶颈是注意力计算，逐层 offload 的传输已经与计算重叠。蒸馏到 4/8 步是数量级的提速，画质与 50 步不完全相同。消费级卡上把一个实例拆到更多卡上并不更快：4 卡 TP4 比 2 卡还慢。
- **一台机器能跑几个实例看主机内存，不看卡数。** 2 卡 Turbo 档锁页约 190 GiB 主机内存，单卡 Turbo 档约 150 GiB，FastH3 NVFP4 约 65 GiB。512 GiB 内存的 8 卡机上同时起两个 2 卡 offload 实例，后起的那个加载时会挤爆内存、把先起的实例杀掉。这类机器建议只跑一个 2 卡 offload 实例；Blackwell 卡的其余卡给 FastH3 NVFP4 单卡实例（4 个单卡实例并发时每个请求 54–74 s）。
- **Turbo 档**用 [LightX2V MiniMax-H3 Turbo](https://huggingface.co/lightx2v/Minimax-h3-Turbo) 的 FL2VA 4 步 v1.3 LoRA，请求不指定步数时默认 4 步、guidance 1，可以文生视频，也可以给一张首帧参考图。按 768p 训练，提供 1344×768、832×480 及其竖屏尺寸，输出分辨率跟 `size` 走。时长须在 4–15 s 之间。只给文字时引擎要求带 `aspect_ratio`，试验场按模板默认值 `16:9` 发送，直接调 API 时要自己带上。
- **FastH3 Omni NVFP4** 是 [FastVideo](https://huggingface.co/FastVideo/FastH3-Omni-NVFP4-QAD) 用量化感知蒸馏得到的 Ref2VA 版本，固定 8 步，**必须给参考图**（不支持纯文生视频）；NVFP4 内核只有 Blackwell 有。模板另外下载 MiniMax-H3 的 `Ref2VA/video_vae`、`Ref2VA/audio_vae` 与 8 层 NVFP4 视频解码器，启动前在容器内搭好离线的 Hugging Face 缓存目录。
- Turbo 与 FastH3 档位使用上游随 MiniMax-H3-Image 发布的 `vllm/vllm-omni:minimax-h3-image`（vLLM 0.33.0 + vLLM-Omni）；GPUWorks 的 0.30 runner 与上游 v0.32.0 都还不认 FastH3 Omni 的 checkpoint。
- offload 档位把大部分权重放在锁页主机内存，模板为它们打开了 `memlock` 不限；无 Docker 的容器节点（udocker/runveil）不支持该设置，只能用宿主进程现有的上限，而且大块结果在进程间传递很慢，长视频可能超时。

## 已知边界

- 当前档位仅接受 4 张单卡至少 32 GiB 的 910B4；其他卡数和型号不从一次真机结果外推。
- 768 像素短边在最终 VAE 解码阶段可能显存不足。
- 8 卡 TP8 会在权重分片前产生较高主机内存压力，当前不作为可用档位发布。
- 模板只下载并缓存运行所需的 `FL2VA` 仓库目录；如果节点已经缓存同 revision 的完整仓库，GPUWorks 会直接复用完整仓库。
