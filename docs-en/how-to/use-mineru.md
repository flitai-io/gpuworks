# Connect MinerU to a remote vision model

GPUWorks deploys the MinerU vision model, manages GPU resources and provides an
inference gateway. The MinerU client reads PDFs, renders pages, calls the model
and produces Markdown, JSON and images. Input PDFs and parsing results stay on
the machine running MinerU.

## Deploy the vision model

1. Search **Inference Templates** for `MinerU2.5-2509-1.2B` and choose the NVIDIA recipe.
2. Select the node, GPU resources and replicas, acquire the pinned weights and create a deployment.
3. Wait for the deployment to be running and confirm engine startup in the instance logs.
4. Note the deployment's **served model name** and create an **Inference service token** under **API Tokens**.

The recipe reuses `gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2` and loads a
pinned MinerU logits processor from a catalog asset. No Python packages are
installed at startup. Model management acquires and mounts weights separately
from the image. The recipe uses FP16 and a 16384-token maximum context.
Meet the image's NVIDIA driver requirements; weight size is not the total
inference memory requirement.
The recipe sets `VLLM_USE_V2_MODEL_RUNNER=0` to match the processor's V1
interface. Do not remove this setting independently: the default V2 interface
in 0.31 rejects this processor.

This combination has parsed a one-page PDF through the GPUWorks gateway on a
Quadro RTX 8000 (SM 7.5), producing Markdown with body text and a table. That
validation used the pinned client versions below.

The model exposes `/v1/chat/completions` for MinerU's specialized layout and
recognition prompts. A successful ordinary chat is not PDF parsing acceptance;
validate with the MinerU client below.

## Configure the parsing client

This example pins MinerU 2.7.6 and mineru-vl-utils 0.2.8 and uses their
`vlm-http-client` interface. MinerU 4.x changed its CLI; these commands are not
instructions for that version. The remote VLM client needs neither a local
NVIDIA GPU nor vLLM installed locally.
Do not use 2.7.0: that release eagerly imports local inference dependencies and
fails with `No module named torch` in a remote-client-only installation. Later
patch releases fixed this issue; see
[MinerU #4300](https://github.com/opendatalab/MinerU/issues/4300).

```bash
python3.12 -m venv .venv-mineru
. .venv-mineru/bin/activate
pip install 'mineru==2.7.6' 'mineru-vl-utils==0.2.8'

export MINERU_VL_MODEL_NAME='<served-model-name>'
export MINERU_VL_API_KEY='<gw-inference-token>'
export GPUWORKS_URL='https://<gpuworks-host>'
```

Replace the placeholders with the served model name, inference token and
reachable control-plane URL. These examples assume a root-path deployment;
the client appends `/v1/models` and `/v1/chat/completions`. Do not use a worker's
engine port or the console's `/api/v1/playground` endpoint. Set the model name
explicitly so a multi-model gateway cannot select the wrong model. A `gwp_`
platform token cannot replace the inference token.

Check authentication and the model name, then parse a small PDF:

```bash
curl --fail-with-body "$GPUWORKS_URL/v1/models" \
  -H "Authorization: Bearer $MINERU_VL_API_KEY"

mineru -p ./example.pdf -o ./output \
  -b vlm-http-client -u "$GPUWORKS_URL"
```

Inspect Markdown, JSON and images under `output/`. The client renders PDF pages
into images and sends recognition requests through the GPUWorks gateway to the
model instance. Input and output files are not platform-managed parsing tasks.
`hybrid-http-client` can additionally run auxiliary models on the client; it is
outside this remote-only VLM recipe.

## Inspect status and diagnose errors

Open **Deployments → Instances** for node, status and startup/runtime logs. GPU
and node details show memory and utilization. Gateway request records and token
usage help confirm the selected model was called; see
[Monitor metrics and logs](monitor-and-logs.md).

| Symptom | Check |
|---|---|
| 401 | Verify `MINERU_VL_API_KEY` is a valid inference service token |
| Missing or wrong model | Compare `/v1/models` with `MINERU_VL_MODEL_NAME`; do not select the first model implicitly |
| 503 | Check for a starting/sleeping instance or offline node |
| Logits processor import failure | Inspect asset acquisition, digest verification and mount logs; use the matching vLLM image |
| Successful requests without correct Markdown | Inspect client logs/output, pinned client versions and the input PDF |
| Out of GPU memory | Adjust concurrency, GPU resources or context length; locate the failing stage in instance logs |

## Interface and scope

The recipe uses the existing [OpenAI-compatible gateway](../reference/openai-gateway.md).
It adds no PDF upload, parsing task management or result download API. Model
runtime logs and client-side PDF parsing logs remain in their respective environments.

See the [MinerU model card](https://huggingface.co/opendatalab/MinerU2.5-2509-1.2B)
and [GPUStack remote backend integration](https://github.com/gpustack/community-inference-backends/tree/main/mineru).
