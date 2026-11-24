# SenseNova-U1.6-8B-MoT

SenseNova-U1.6-8B-MoT 是商汤 NEO-unify 架构（Qwen3-8B 骨干 + 生成塔 + 像素头，`neo_chat` 模型类型）的文生图与图像编辑模型：BF16 权重 35.07 GB。GPUWorks 的 Playground 可以直接文生图和单图编辑，`/v1/images/generations` 与 `/v1/images/edits` 两个接口都经平台网关提供。

## 权重

- Hugging Face `sensenova/SenseNova-U1.6-8B-MoT`，revision `8f582699bac7f1e5362df2a776397d0e1a963b6c`。
- ModelScope `SenseNova/SenseNova-U1.6-8B-MoT`，revision `c03718b305258bbdc3994e132158ddfbda842268`。
- 两个来源的文件按 sha256 一致（8 个 safetensors 分片 + tokenizer/config），`weight_bytes` 35065860072。

## 镜像与引擎

两个档位都用 `vLLM-Omni 0.32`（`cuda13.0-vllm-omni0.32.0-r2-linux-amd64`），`--omni` 启动；不声明 offload 档位：这个版本的 vLLM-Omni 对 SenseNova 架构（`SenseNovaU1ForCausalLM`）报 `No _layerwise_offload_blocks_attrs defined`，上游的逐层 offload 支持表里没有它。

## 档位

| 档位 | GPU | 适用 | 单卡显存峰值 |
|---|---:|---|---:|
| 在线 FP8 · 单卡 | 1 | Ampere 及以上，单卡 ≥ 22 GiB | 21.7–22.0 GiB |
| BF16 · 1–2 卡 | 1 或 2 | 单卡 ≥ 40 GiB，或双卡每卡 ≥ 22 GiB | 单卡 36.0 GiB；双卡 17–18.7 GiB/卡 |

**在线 FP8**（`--omni --quantization fp8`）把 BF16 权重在加载时量化到 FP8：Ada 及以上走 CUTLASS W8A8，Ampere（SM86，如 RTX 3090）没有 FP8 Tensor Core，退回 Marlin W8A16（FP8 权重、BF16/FP16 激活）。两种路径都在真实硬件上验证过。

**BF16**（`--omni`，不量化）单卡需要装下完整 35 GB 权重，两卡时平台按放置到的 GPU 数注入 `--tensor-parallel-size`，模型按张量并行切分到两卡。两种布局都能放下时优先选单卡。

## 实测性能

每行 50 步、并发 1、端到端中位数（含网络与网关开销）；"引擎版本" 指实际跑引擎的 vLLM-Omni 版本，与模板固定的 0.32 不同的行单独标注。

| 档位 | 卡 | 引擎版本 | 1024×1024 | 2048×2048 | 单卡显存峰值 |
|---|---|---|---:|---:|---:|
| 在线 FP8 · 单卡 | RTX 4090 48GB（本模板镜像） | 0.32.0 | 8.7–9.1 s | 38.1 s | 21.74 GiB |
| 在线 FP8 · 单卡 | RTX 4090 24GB（vast 租机） | 0.34.0 | 9.2–10.2 s | 42.0 s | 21.68 GiB |
| 在线 FP8 · 单卡 | RTX 3090 24GB（vast 租机） | 0.34.0 | 27.4–30.0 s | 120.3 s | 21.99 GiB |
| BF16 · 单卡 | RTX 4090 48GB（本模板镜像） | 0.32.0 | 12.5–13.3 s | 55.0 s | 36.04 GiB |
| BF16 · 双卡 TP2 | 2 × RTX 3090 24GB（本模板镜像，GPUWorks 平台部署，经网关实测） | 0.32.0 | 24.9 s | 100.5 s | 18.66 GiB/卡 |
| BF16 · 双卡 TP2 | 2 × RTX 3090 24GB（docker，另一次实测） | 0.34.0 | 21.4 s | 86.8 s | 17.1–18.2 GiB/卡 |
| BF16 · 双卡 TP2 | 2 × RTX 3090 24GB（runveil，跨拓扑无 NVLink） | 0.34.0 | 25.6 s | 124.9 s | 17.1–18.2 GiB/卡 |

说明：

- 只有 24–48 GB 消费卡（RTX 3090/4090）实测过；FP8 单卡两种架构（Ada 的 CUTLASS、Ampere 的 Marlin）都已在真实硬件上验证，没有更大显存卡（A100/H100/H20）的数据。
- RTX 3090（SM86，Ampere）没有 FP8 硬件支持，在线 FP8 比 4090（Ada，原生 W8A8）慢 3 倍以上（Marlin 的权重反量化开销）；需要更快的 FP8 可以直接用 BF16 + 更大显存的卡。
- 两卡 TP2 之间有无 NVLink 对延迟有影响：同一组 3090 上，NVLink 对（docker 示例）比跨 PCIe 拓扑（runveil 示例）快 1.18–1.44 倍，2048×2048 处差距更大。
- 冷启动（部署创建到健康检查通过）：权重已在共享存储上时，双卡 TP2 实测约 82 秒（含镜像拉取、但权重不需要下载）；首次下载权重另加权重大小 ÷ 下载带宽的时间。
- 编辑（`/v1/images/edits`，1024×1024 参考图）在双卡 TP2（本模板镜像）上实测 26.7 s。
- 1344×768 与 768×1344 两个长宽比已在本模板镜像的双卡 TP2 实测通过（各 24.6–24.8 s，与同尺寸的正方形档位接近），返回图片的像素尺寸与请求一致。

## 已知限制

- **没有 offload 档位**：vLLM-Omni 0.32/0.34 都不支持这个模型的逐层显存卸载，所以没有比在线 FP8 更省显存的路线；24 GB 以下的卡跑不动。
- **Ampere 在线 FP8 较慢**：RTX 3090 用 Marlin W8A16，2048×2048 单张图接近 2 分钟；显存紧张但不在意速度时可用，追求速度建议用支持 FP8 的 Ada/Hopper 卡或 BF16 + 更大显存。
- **Ampere 卡上的在线 FP8 需要较新的 server**：早于 2026-11-24 的显存估算矩阵会拒绝 Ampere 上的 FP8（`dtype_unsupported_detail`），需要升级到包含该修复的版本才能在 RTX 3090 一类的 Ampere 卡上使用这个档位；Ada 及以上不受影响。
- **首次请求较慢**：引擎启动后第一次推理要做图编译/预热，样本以外另有一次性开销，不计入上表中位数。

## 调用示例

文生图：

```bash
curl -X POST "$GPUWORKS_URL/v1/images/generations" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"model":"<served-model>","prompt":"A sunrise over mountains","size":"1024x1024"}'
```

图像编辑（`image` 为源图文件）：

```bash
curl -X POST "$GPUWORKS_URL/v1/images/edits" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -F image="@input.png" \
  -F model=<served-model> \
  -F prompt="Make the background blue"
```

`size` 支持 1024x1024、1536x1536、2048x2048 三个正方形档位，以及 1344x768、768x1344 两个 16:9 附近的长宽比（均为 32 的倍数，已在真实部署上验证可用）。
