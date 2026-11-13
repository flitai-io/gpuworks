# MiniMax-H3-Image

MiniMax-H3-Image 是基于 MiniMax-H3 DiT 的文生图与参考图编辑模型：每张图固定 6 步（CFG 蒸馏），单次请求输出一张图，编辑最多接受 9 张参考图。GPUWorks 的 Playground 可以直接文生图和单图编辑，`/v1/images/generations` 与 `/v1/images/edits` 两个接口都经平台网关提供。

## 权重与组件

模板由两个组件组成，部署时都由平台下载并挂载，不需要手工准备：

| 组件 | 来源 | 大小 | 容器内路径 |
|---|---|---:|---|
| 主模型（DiT、VAE、PDD 输出头、分词器） | `MiniMaxAI/MiniMax-H3-Image` | 68.7 GB | `/models/MiniMax-H3-Image` |
| Qwen3-VL 文本编码器 | `MiniMaxAI/MiniMax-H3` 的 `text_encoder` 目录 | 66.7 GB | `/models/MiniMax-H3` |

上游 `model_index.json` 按仓库名去 Hub 取 MiniMax-H3 的文本编码器；模板在启动前把它改成指向挂载目录，引擎全程离线（`HF_HUB_OFFLINE=1`）。只下载 `text_encoder` 目录；节点上已有同 revision 的完整 MiniMax-H3 仓库时直接复用。

Hugging Face 上 2026-11-10 之后的 revision 另外附带了一份 diffusers 格式权重（整个仓库约 204 GB），vLLM-Omni 用不到，模板固定在之前的 revision。

## 档位

镜像使用上游随模型发布的 `vllm/vllm-omni:minimax-h3-image`（vLLM 0.33.0 + vLLM-Omni），参数取自 [vLLM recipe](https://recipes.vllm.ai/MiniMaxAI/MiniMax-H3-Image)。六个档位都实测过，数字为 2048×2048、并发 1 的端到端中位数：

| 档位 | 卡数 | 适用 | 实测卡型 | 单卡显存峰值 | 文生图 | 编辑 |
|---|---:|---|---|---:|---:|---:|
| 4 卡 Ulysses | 4 | Hopper 及以上，单卡 ≥ 90 GiB | H20-3e | 83.0 GiB | 2.55 s | 7.8 s |
| 4 卡 TP | 4 | Hopper 及以上，单卡 ≥ 44 GiB | H20-3e | 35.5 GiB | 2.63 s | 7.8 s |
| 2 卡 Ulysses | 2 | Hopper 及以上，单卡 ≥ 110 GiB | H20-3e | 94.3 GiB | 4.70 s | 14.6 s |
| 2 卡 TP | 2 | Ampere 及以上，单卡 ≥ 72 GiB | A100 80GB | 61.3 GiB | 3.31 s | 9.0 s |
| 单卡 · 文本编码器 offload | 1 | Ampere 及以上，单卡 ≥ 78 GiB | A100 80GB / H20-3e | 69.9 / 71.4 GiB | 7.67 / 9.55 s | 17.6 / 27.3 s |
| 单卡 · 逐层 offload | 1 | Ampere 及以上，单卡 ≥ 22 GiB | RTX 4090 / A100 / H20-3e | 11.1–12.7 GiB | 18.3 / 19.7 / 9.53 s | 24.3 / 21.7 / 27.3 s |

编辑一列：H20 用 2720×1536 的参考图，A100 与 4090 用 2048×2048 的参考图，两者不完全可比。

怎么选：

- **门槛最低**：单卡逐层 offload。一张 24 GB 的卡就能跑，DiT 和文本编码器放在约 124 GiB 的锁页主机内存里，速度取决于主机到 GPU 的带宽（PCIe 5 的 H20 约 9.5 s，PCIe 4 的 4090、A100 约 18–20 s）。
- **80 GB 卡**：单卡文本编码器 offload（A100 7.7 s），两张卡用 2 卡 TP（A100 3.3 s）。
- **141 GB 卡（H200、H20-3e）**：4 卡 TP 和 4 卡 Ulysses 速度相近（H20 上 2.6 s 对 2.55 s），4 卡 TP 每卡显存不到一半。
- 消费级卡（RTX 4090）之间没有 NVLink，多卡布局反而更慢：4 卡 TP 加文本编码器 offload 每张 30.6 s，2 卡 Ulysses 加逐层 offload 每张 110 s，这两种布局没有放进模板。4090 想提高吞吐，按卡部署多个单卡副本（每个副本需要约 124 GiB 主机内存）。

其他说明：

- 小于 2048² 的请求在至少 2048² 的画布上生成再缩回，所以 1024² 与 2048² 耗时几乎相同。直接请求画布尺寸（2048x2048、2720x1536、1536x2720、2368x1760、2496x1664、3136x1344）可以省掉缩放。
- 引擎一次只处理一个请求，并发只增加排队时间，吞吐不随并发上升。
- 两个 offload 档位的权重放在锁页主机内存（文本编码器约 62 GiB，逐层 offload 再加 DiT 约 62 GiB），模板为它们打开了 `memlock` 不限。
- 80 GB 卡上平台按卡的 90% 计算可用显存（72 GiB），单卡文本编码器 offload 的预留（71.5 GiB）贴着这个上限。
- B200/B300 等单卡显存足够时，单卡全部常驻是上游推荐的布局，模板尚未提供。

## 请求约束

- `num_inference_steps` 只能省略或为 6，`guidance_scale` 只能省略或为 1，不支持 `negative_prompt`，`n` 为 1。
- 编辑时参考图按上传顺序称为 `Picture 1`、`Picture 2`……，提示词可以直接引用；参考图用目标画布的尺寸、并请求同一尺寸，对齐效果最好。
