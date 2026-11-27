# DeepSeek-V4.1-Flash-1017（EXL3 2.8bpw，两台 DGX Spark）

jayleaton 的 TensorFold 原生（Zig）TP2 配方（[deepseek-v41-tensorfold-spark](https://github.com/jayleaton/deepseek-v41-tensorfold-spark)，提交 `13db91e`，TensorFold `f212eeab`），用 `callable` 引擎承载 DeepSeek-V4.1-Flash-1017。

- 1M 上下文、图像输入和投机解码都已开启。
- 748B 参数（552B 主体加 196B Engram 表）的模型跑在两台 DGX Spark（GB10）上，每台一个 TP rank。

## 模板提供了什么

- **镜像** `gpuworks/tensorfold-dsv41-flash17:f212eeab-r1`，quay.io 与华为 SWR 上也有同一份。镜像里包含：
  - TensorFold 引擎；
  - 运行时资产：RoPE 表、token map、签名，以及 sm_121 的 144 个 AOT 内核变体；
  - 这些资产对应的引擎配置；
  - 启动适配脚本：rank 0 运行 `tensorfold-dsv41 serve`，rank 1 运行 `tf-dsv41 follow`。

  AOT 内核集合由上下文长度、slot 数、预填充和草稿相关设置决定，所以资产和配置放在同一个镜像里发布。
- **权重**：
  - Hugging Face 上的 EXL3 2.8bpw 包，约 204 GB，两台节点相同。
  - Engram 表：按 TP2 打包、按 rank 拆分后放在 ModelScope 上，每个 rank 约 101 GB。每台节点只下载 Engram 组件里自己那份 `rank<r>/`。
- **托管目录**（重启后保留）：
  - `tf-prepared`（128 GiB）：存放 prepared 权重。
  - `tf-state`（150 GiB）：存放校准表、请求日志和会话磁盘层。
- **平台提供的启动信息**：rank、master 地址和端口、RDMA 设备与网卡、锁定内存都由部署提供；模板只加 `--ipc=host --init` 和不限 memlock。

## 部署要求

- 只能选两台 GB10 节点，每台一个 rank。适配脚本会拒绝其他节点数。
- 两台节点要在同一个「主机 RDMA」参数网络上（RoCE 口）；没有参数网络时，部署评估会说明原因。
- 每台节点要放得下：204 GB 的权重包、自己那份 101 GB 的 Engram，以及首次启动写出的约 96 GB prepared 权重。
- 两台 Spark 上的内存余量很小：128 GB 统一内存放下模型主体后，只剩几 GiB 能缓存 Engram 页。不要在这两台节点上同时运行其他耗内存的任务。

## 启动时间

- **首次启动约 13 分钟**：加载权重包，转换成本 rank 的 prepared 权重（约 96 GB，写入 `tf-prepared`），并测量草稿耗时。模板的启动超时为 3600 秒。
- **之后的启动约 7 分钟**：直接读回 prepared 权重。

## 实测性能

测试环境：两台 GB10，ConnectX-7 经 RoCE 互联，GPU 锁频 2200 MHz。

直连引擎测试（真实文本，关闭思考）：

| 测试 | 结果 |
|---|---|
| 单路 decode | 约 50 tok/s |
| 2 路 / 4 路合计 | 约 71 / 103 tok/s |
| 预填充 16K / 128K token | 约 2.3K / 2.5K tok/s |

经平台评测（走网关，保留模型默认的思考）：

- 真实文本数据集在并发 1 到 4 时，合计输出约 26–33 tok/s，首 token 时间 0.5–2 秒。
- 每个 token 的解码时间几乎随并发线性增长（每多一个请求，每步多约 31 ms），所以并发增加时合计吞吐基本不涨。

**负载内容对性能影响很大。**

- 每个 rank 的 Engram 表约 94 GiB，放不进内存，只能按需从盘上读取。
- 真实文本的查表有局部性，大多命中页缓存。
- 随机 token 的数据集几乎每次查表都要读盘：首 token 时间增加约 10 倍（6–7 秒），合计输出降到约 17–19 tok/s。

请用接近自己真实流量的数据集来评估这个部署；评测可以在同一个部署上切换数据集。

## 默认值与限制

- 推理强度默认 `high`，单个请求可以用 `reasoning_effort` 覆盖。
- 思考中的计算器（runner，由引擎替模型计算算式）**默认关闭**。
  - 打开时，引擎会把自己的标记（`<expr>…</expr>`、`Result: …`）写进可见回答。
  - 关闭方式写在镜像配置里：`TF_DSV41_RUNNER=0`、`TF_DSV41_COMPUTE_LANE_DEFAULT=0`、`TF_DSV41_COMPUTE_LANE_RUNNER=0`。这套配置的 AOT 检查没有缺失变体。
- 上下文长度、slot 数和草稿设置由镜像配置固定，因为预编译内核依赖它们。要改需要新镜像，不能在部署表单里改参数。
- 长预填充会让其他请求排队：多人同时发很长的提示时，其他人的首 token 可能要等几分钟。
- 多用户或 agentic 负载更适合四节点档位：Engram 表分到更多内存里。见 DeepSeek-V4.1-Flash-1017 的 pico TP4 档位。
