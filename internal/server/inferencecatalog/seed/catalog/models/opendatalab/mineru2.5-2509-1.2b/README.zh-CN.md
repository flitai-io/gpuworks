# MinerU 远程视觉模型

此模板部署文档视觉模型，提供 OpenAI 兼容的 chat completions 服务。
PDF 解析由另外运行的 MinerU 客户端完成，输入文件及 Markdown、JSON、图片结果
保留在客户端机器上。GPUWorks 管理模型、GPU 资源、运行状态与实例日志。

选择 NVIDIA 配方，部署就绪后复制**对外服务名**，并创建**推理服务令牌**。
本配方使用 FP16；模板会离线挂载 MinerU 所需的 logits processor。
普通聊天测试不能替代 PDF 解析验证。

在客户端使用 Python 3.12 安装固定版本，然后填写 GPUWorks 根地址：

```bash
pip install 'mineru==2.7.6' 'mineru-vl-utils==0.2.8'
export MINERU_VL_MODEL_NAME='<served-model-name>'
export MINERU_VL_API_KEY='<gw-inference-token>'
mineru -p ./example.pdf -o ./output \
  -b vlm-http-client -u 'https://<gpuworks-host>'
```

不要把计算节点的引擎端口或平台管理令牌填入上面的网关配置。
MinerU 4.x 的 CLI 与本示例不同。平台不接收 PDF，也不管理解析任务或输出文件。

模型来源：[OpenDataLab MinerU2.5-2509-1.2B](https://huggingface.co/opendatalab/MinerU2.5-2509-1.2B)。
