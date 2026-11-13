# 用 MinerU 连接远程视觉模型

GPUWorks 负责部署 MinerU 视觉模型、管理 GPU 资源并提供推理网关。MinerU
客户端负责读取 PDF、渲染页面、调用模型和生成 Markdown、JSON 与图片。
输入 PDF 和解析结果保存在运行 MinerU 的机器上。

## 部署视觉模型

1. 在**推理模板**搜索 `MinerU2.5-2509-1.2B`，选择 NVIDIA 配方。
2. 选择节点、GPU 资源和副本数，下载模板固定版本的模型权重，创建部署。
3. 等待部署状态为运行中，在实例日志确认引擎启动完成。
4. 记下部署的**对外服务名**；在 **API 令牌 → 推理服务令牌**创建调用凭据。

配方复用 `gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2`，通过模板资产加载
固定版本的 MinerU logits processor，不在启动时安装 Python 包。权重由模型管理
下载并挂载，镜像不内置另一份权重。使用 FP16 和最大 16384 token 上下文。
请按镜像要求准备 NVIDIA 驱动；模型文件大小不等于推理所需显存。
配方设置 `VLLM_USE_V2_MODEL_RUNNER=0`，使 vLLM 使用处理器对应的 V1 接口；
不要单独移除该设置，0.31 默认的 V2 接口不接受此处理器。

此组合已在 Quadro RTX 8000（SM 7.5）上完成经 GPUWorks 网关的单页 PDF
解析验证，输出包含正文与表格的 Markdown。该验证使用下面固定版本的客户端。

模型提供 `/v1/chat/completions`，采用 MinerU 专用的版面与内容识别提示词。
普通聊天成功不代表 PDF 解析验收通过；应使用下面的 MinerU 客户端验证。

## 配置解析客户端

以下示例固定使用 MinerU 2.7.6 与 mineru-vl-utils 0.2.8，使用该版本的
`vlm-http-client` 接口。MinerU 4.x 的 CLI 已变化，不应直接套用这些命令。
纯远程 VLM 客户端不需要本地 NVIDIA GPU，也不需要安装 vLLM。
不要退回 2.7.0：该版客户端会提前导入本地推理依赖，导致仅安装远程客户端时
报 `No module named torch`；后续补丁版已修复这一问题，见
[MinerU #4300](https://github.com/opendatalab/MinerU/issues/4300)。

```bash
python3.12 -m venv .venv-mineru
. .venv-mineru/bin/activate
pip install 'mineru==2.7.6' 'mineru-vl-utils==0.2.8'

export MINERU_VL_MODEL_NAME='<served-model-name>'
export MINERU_VL_API_KEY='<gw-inference-token>'
export GPUWORKS_URL='https://<gpuworks-host>'
```

`GPUWORKS_URL` 填控制面的可访问入口；以下命令按根路径部署举例，客户端会拼接
`/v1/models` 和 `/v1/chat/completions`。不要填计算节点的引擎端口或控制台的
`/api/v1/playground` 路径。模型名必须与部署一致，显式设置可避免多模型网关中选错模型。
`gwp_` 平台令牌不能替代推理服务令牌。

先核对鉴权和模型名称，再解析一份小 PDF：

```bash
curl --fail-with-body "$GPUWORKS_URL/v1/models" \
  -H "Authorization: Bearer $MINERU_VL_API_KEY"

mineru -p ./example.pdf -o ./output \
  -b vlm-http-client -u "$GPUWORKS_URL"
```

在 `output/` 下检查生成的 Markdown、JSON 和图片。PDF 页面会由客户端转成图片，
识别请求经 GPUWorks 网关路由到模型实例；输入文件和结果文件不进入平台任务管理。
`hybrid-http-client` 还可能在客户端运行辅助模型，不属于这里的纯远程 VLM 配方。

## 查看状态与定位错误

在**部署 → 实例**查看节点、运行状态和启动/运行日志；在节点或 GPU 详情查看显存
与利用率。网关请求记录和令牌用量用于核对客户端是否实际调用了所选模型，详见
[查看指标与检索日志](monitor-and-logs.md)。

| 现象 | 检查方法 |
|---|---|
| 401 | 检查 `MINERU_VL_API_KEY` 是否为有效的推理服务令牌 |
| 模型不存在或模型选择错误 | 比对 `/v1/models` 与 `MINERU_VL_MODEL_NAME`，不要依赖列表的第一项 |
| 503 | 查看实例是否仍在启动、已休眠或节点离线 |
| logits processor 导入失败 | 查看模板资产下载、摘要校验和挂载日志，确认使用配套 vLLM 镜像 |
| 请求成功但没有正确的 Markdown | 查看 MinerU 客户端日志及输出目录，核对固定的客户端版本与输入 PDF |
| 显存不足 | 调整并发、GPU 资源或上下文长度；在实例日志定位具体阶段 |

## 接口与适用范围

这条配方复用现有 [OpenAI 兼容网关](../reference/openai-gateway.md)，不新增 PDF
上传、解析任务管理或结果下载接口。平台的模型运行日志与客户端的 PDF 解析日志
分别保留在各自环境中。

参考 [MinerU 模型说明](https://huggingface.co/opendatalab/MinerU2.5-2509-1.2B)
和 [GPUStack 的远程后端接入方式](https://github.com/gpustack/community-inference-backends/tree/main/mineru)。
