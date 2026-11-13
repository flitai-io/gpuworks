# MinerU remote vision model

This template deploys a document vision model with an OpenAI-compatible chat
completions endpoint. A separately running MinerU client parses PDFs and keeps
the inputs, Markdown, JSON and image outputs on its own machine. GPUWorks
manages the model, GPU resources, runtime status and instance logs.

Choose the NVIDIA recipe. Once ready, copy its **served model name** and create
an **inference service token**. The recipe uses FP16 and mounts the required
MinerU logits processor from an offline template asset. Ordinary chat is not
a substitute for PDF parsing validation.

Install these versions in a Python 3.12 client environment and set the GPUWorks root URL:

```bash
pip install 'mineru==2.7.6' 'mineru-vl-utils==0.2.8'
export MINERU_VL_MODEL_NAME='<served-model-name>'
export MINERU_VL_API_KEY='<gw-inference-token>'
mineru -p ./example.pdf -o ./output \
  -b vlm-http-client -u 'https://<gpuworks-host>'
```

Do not substitute a worker's engine port or a platform management token for
the gateway configuration above. MinerU 4.x has a different CLI. GPUWorks does
not accept PDFs or manage parsing tasks and output files.

Model source: [OpenDataLab MinerU2.5-2509-1.2B](https://huggingface.co/opendatalab/MinerU2.5-2509-1.2B).
