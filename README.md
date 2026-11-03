<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/brand/gpuworks-wordmark-dark.svg">
    <img src="docs/assets/brand/gpuworks-wordmark.svg" alt="GPUWorks" width="360">
  </picture>
</p>

<p align="center">
  <b>A 40 MB MaaS platform: one cluster across public and private clouds, on any mix of accelerators.</b>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/go-1.26-00ADD8.svg" alt="Go 1.26">
  <img src="https://img.shields.io/badge/accelerators-NVIDIA%20%7C%20Ascend%20%7C%20PPU%20%7C%20Hygon%20%7C%20Apple%20Silicon-76B900.svg" alt="Accelerators">
</p>

<p align="center">
  English | <a href="README.zh-CN.md">简体中文</a>
</p>

<p align="center">
  <img src="docs/assets/screenshots/readme/tour.gif" alt="GPUWorks tour: overview, GPUs, deployment wizard, Playground" width="900">
</p>

GPUWorks is an ultra-lightweight GPU management and Model-as-a-Service (MaaS) platform. The whole control plane, web
UI included, is a 40 MB download. Join your own GPU servers, rented cloud instances or containers, or a Mac with one
command, pick a model, and GPUWorks estimates the memory it needs, picks the GPUs, starts vLLM, SGLang or any engine
an inference template defines (Strata, ninfer, …), and serves it behind one OpenAI-compatible endpoint.
NVIDIA, Huawei Ascend, T-Head PPU, Hygon DCU and Apple Silicon run side by side in the same cluster.

The pool of capacity is the core: bare-metal servers, public cloud instances and private cloud VMs today, with
Kubernetes clusters and Slurm partitions joining next. On top of it, GPUWorks serves models now and will run
fine-tuning and training jobs on the same accelerators.

## Highlights

- **Verified model × accelerator combinations deploy in one click.** The built-in catalog ships 1,224 launch specs
  for 390 models: image, engine, flags and pinned weight revisions for each GPU type. The combinations we have run
  on real NVIDIA, Ascend, PPU and Apple hardware come with launch settings that are known to work, so you pick a
  model and press deploy instead of hunting for flags. Qwen, DeepSeek, GLM, Kimi, MiniMax, Llama, Gemma, gpt-oss,
  plus embedding, rerank, speech, image and video models.
- **Heterogeneous accelerators, one scheduler.** NVIDIA, Ascend, PPU, Hygon and Apple Silicon nodes join the same
  cluster; the scheduler matches each deployment to hardware its launch spec supports.
- **Public and private cloud in one pool.** Sync instances from RunPod, CompShare, AutoDL, Vast.ai and Alibaba Cloud,
  or VMs from ZStack Cloud, install the agent in one click, and schedule them together with your own servers.
- **Ultra-lightweight.** The server and the agent are each one static binary (`CGO_ENABLED=0`); the server embeds the
  web UI (English / 中文) and the agents for every platform, and keeps its state in one SQLite file. The agent dials
  out over WebSocket, so nodes need no inbound ports (or the server dials in, for nodes that cannot reach out). In a
  drill on Alibaba Cloud, one server ran 1,003 rented GPU machines across 7 regions: over 500 replicas placed and
  running within 4 minutes, and 2,000 concurrent requests through the gateway with the server peaking at 9 CPU cores
  and 1.6 GB of memory.
- **Memory estimate and explained placement.** Before anything starts, you see how much GPU memory weights, KV
  cache and engine overhead need, which GPUs the scheduler picked and why the others were rejected.
- **Share a GPU between models.** dGPU puts a hard memory quota on each instance so several models fit on one card
  (NVIDIA via HAMi-core, Ascend via vCANN, PPU via HGGC; Hygon experimental). Sleep/wake lets vLLM models take turns
  on the same GPUs on NVIDIA, Ascend, PPU and Hygon.
- **Scale out.** Replicas across nodes, multi-node tensor/pipeline parallel, and prefill/decode disaggregation
  (vLLM + NIXL on NVIDIA, vLLM-Ascend + Mooncake on Ascend).
- **OpenAI-compatible gateway.** Chat, completions, embeddings, rerank, audio, images, video and realtime under
  `/v1`; `/v1/messages` and `/v1/responses` pass through. Least-request, prefix-affinity or failover routing, API
  keys, per-key token usage.
- **See what is going on.** GPU and engine metrics, searchable logs, per-request traces with a timeline (OTLP export),
  a web terminal into hosts and containers, and a built-in benchmark that sweeps concurrency and finds the SLA limit.
- **AI assistant and MCP.** Ask questions about the cluster in the UI, or connect Claude Code / any MCP client to the
  read-only MCP endpoint.
- **Air-gapped ready.** Import images and models from archives or an HTTP file server, and sync models between nodes.

## Quick start

**1. Install the server** on a Linux host (x86_64 or arm64) or an Apple Silicon Mac:

```bash
bash <(curl -fsSL https://dl.gpuworks.ai/get.sh)
```

The script picks the fastest download source (including mirrors in mainland China), verifies the checksum and runs
the installer, which sets up a service on port 8800 and prints the URL and the admin password. Add `--quiet` to
accept every default. To check each step yourself, follow the [manual install](docs-en/how-to/install-server.md).

**2. Add a GPU node.** Open the web UI, go to **Compute nodes → Add node**, and run the generated command on the GPU
host:

```bash
curl -fsSL http://<server>:8800/install.sh | sh -s -- --server http://<server>:8800 --token <node-token>
```

**3. Deploy a model.** Open **Deploy**, pick a model such as `Qwen/Qwen3-1.7B` and a recommended inference template,
review the configuration evaluation (memory estimate and placement), and deploy. Then create an inference token under
**API Tokens** and call the model by its public model name:

```bash
curl http://<server>:8800/v1/chat/completions \
  -H "Authorization: Bearer gw-..." -H "Content-Type: application/json" \
  -d '{"model": "qwen3-1.7b", "messages": [{"role": "user", "content": "Hello!"}]}'
```

The full walkthrough is in [Getting started](docs-en/tutorials/getting-started.md). For a Mac, see
[Run your first local model on a Mac](docs-en/tutorials/mac-local-model.md); for air-gapped sites, see
[Offline deployment](docs-en/how-to/deploy-offline.md).

### Install with an AI coding agent

Paste this into Claude Code, Codex, Cursor or a similar agent that has shell access to your server:

```text
Install the GPUWorks control plane on this host and verify it.
1. Run `bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet`. It downloads the latest build, verifies its
   checksum and installs a service on port 8800 (use sudo when asked). If I have no sudo or no systemd, append
   `--dir ~/gpuworks-server --no-systemd` instead.
2. Verify with `curl -fsS http://<listen-address>/healthz` (expect "status":"ok") and show me the install summary:
   URL, install directory, data directory and the initial admin password location.
3. Do not open firewall ports, change other services, or join GPU nodes. Tell me which address GPU nodes should
   use to reach the server; I will add nodes from the web UI.
```

## Supported hardware

| Accelerator | Whole-GPU deploy | dGPU (shared GPU) | Sleep/wake | Multi-node | PD disaggregation |
|---|---|---|---|---|---|
| **NVIDIA** (CUDA) | ✅ Docker, containerd or runveil (built-in rootless runtime) | ✅ memory quota at MiB granularity | ✅ vLLM | ✅ | ✅ vLLM + NIXL |
| **Huawei Ascend** (CANN) | ✅ Docker, containerd or runveil (built-in rootless runtime) | ✅ memory quota at MiB granularity | ✅ vLLM-Ascend | ✅ | ✅ vLLM-Ascend + Mooncake |
| **T-Head PPU** (HGGC) | ✅ Docker, containerd or runveil (built-in rootless runtime) | ✅ memory quota at MiB granularity | ✅ vLLM | — | — |
| **Hygon DCU** (DTK) | ✅ Docker, containerd or runveil (built-in rootless runtime) | 🧪 memory quota at MiB granularity | ✅ vLLM | — | — |
| **Apple Silicon** (M1–M5, macOS 15+) | ✅ vllm-metal, MLX, llama.cpp as native processes | — | — | — | — |

✅ verified on real hardware · 🧪 experimental · — not supported or not validated. Sleep/wake beyond NVIDIA is
verified on Ascend 910C, PPU ZW810E and Hygon K100; the tested versions and limits are in
[choosing features](docs-en/explanation/choosing-features.md#sleepwake-models-take-turns-on-one-gpu).
GPUs are detected with the vendor CLIs (`nvidia-smi`, `npu-smi`, `ppu-smi`, `hy-smi`), so there is no NVML or cgo
dependency. Per-combination status lives in the [support matrix](docs-en/reference/support-matrix.md) and the
[Ascend matrix](docs-en/reference/ascend-support-matrix.md).

**Engines:** vLLM (incl. vLLM-Ascend and vLLM-Omni), SGLang, and on Mac vllm-metal, MLX-VLM, MLX-Audio and
llama.cpp. Built-in images are published as `gpuworks/gpuworks-runner` on Docker Hub, Quay and Huawei SWR
([image families](docs-en/reference/image-families.md)); you can also bring your own image.

## Model catalog

The built-in catalog pins every model to fixed Hugging Face and ModelScope revisions and ships launch specs per
engine and accelerator. The deploy page recommends the specs that match the model and the target accelerator.

| Type | Examples |
|---|---|
| Chat / reasoning / code | Qwen3 / 3.5 / 3.6 / 3.8, Qwen3-Coder, DeepSeek R1 / V3.2 / V4, GLM-4.5 – 5.3, Kimi K2 – K3, MiniMax M2 – M3, Llama 3.x / 4, Gemma 3 / 4, gpt-oss, Mistral Large 3, ERNIE 4.5, Step-3.5, MiMo, Seed-OSS, Nemotron |
| Vision-language / OCR | Qwen3-VL, Qwen3-Omni, GLM-4.xV, Kimi-VL, DeepSeek-OCR, PaddleOCR-VL, GLM-OCR |
| Embedding / rerank | Qwen3-Embedding, Qwen3-Reranker, bge-m3, bge-reranker-v2-m3, Jina v5, EmbeddingGemma |
| Speech | Qwen3-ASR, Whisper large-v3, Qwen3-TTS, GLM-TTS, Voxtral, FishAudio S2-Pro |
| Image / video | Qwen-Image, GLM-Image, Stable Diffusion 3.5, Wan 2.1 / 2.2, LTX-2.5, Cosmos3 |

Catalog totals: 390 models from 76 organizations, 1,224 launch specs (775 NVIDIA, 196 PPU, 173 Ascend, 42 Hygon,
9 Apple). Models outside the catalog can be registered from Hugging Face, ModelScope, a local directory, NFS or an
HTTP file server and deployed with your own arguments.

## Screenshots

| | |
|---|---|
| ![GPUs](docs/assets/screenshots/readme/gpus.webp) **GPUs** across nodes and vendors | ![Templates](docs/assets/screenshots/readme/templates.webp) **Model catalog** with launch specs per vendor and engine |
| ![Deployment](docs/assets/screenshots/readme/deployment-detail.webp) **Deployment** replicas, status and metrics | ![Playground](docs/assets/screenshots/readme/playground.webp) **Playground** against any deployment |
| ![Monitor](docs/assets/screenshots/readme/monitor.webp) **Monitoring**: GPU, engine and gateway metrics | ![Benchmark](docs/assets/screenshots/readme/benchmark.webp) **Benchmark** report: throughput, TTFT, latency |

## Architecture

```mermaid
flowchart LR
  user["Browser / CLI / MCP client"] -->|"Web UI, REST API"| server
  app["Apps (OpenAI SDK)"] -->|"/v1/* + API key"| server
  subgraph server["gpuworks server (one binary)"]
    direction TB
    gw["OpenAI gateway"]
    ctl["Controller + scheduler<br/>memory estimate"]
    db[("SQLite<br/>metrics · logs · traces")]
  end
  subgraph node1["GPU node"]
    a1["gpuworks-agent"] --> e1["vLLM / SGLang<br/>Docker · nerdctl · runveil"]
  end
  subgraph node2["Mac"]
    a2["gpuworks-agent"] --> e2["vllm-metal / MLX / llama.cpp"]
  end
  a1 -->|"outbound WebSocket + yamux<br/>ctrl lane / data lane"| server
  a2 -->|"outbound WebSocket"| server
```

- The **server** is the control plane: REST API and web UI, the OpenAI-compatible gateway, a controller that
  reconciles desired state, the scheduler, and storage for metrics, logs and request traces.
- An **agent** runs on each GPU node. It probes the accelerators, runs engines through Docker, nerdctl, the built-in
  rootless runtime (runveil) or as host processes on macOS, and reports metrics and logs.
- Agents connect out to the server and multiplex everything over two lanes: `ctrl` for commands, metrics and
  terminals, `data` for inference traffic, logs and model/image transfer. Inference requests reach the engine
  through that tunnel, so nodes never expose ports.

More in [Architecture](docs-en/explanation/architecture.md) and the [ADRs](docs-en/adr/README.md).

## Documentation

| I want to… | Go to |
|---|---|
| Get it running | [Getting started](docs-en/tutorials/getting-started.md) · [Install the server](docs-en/how-to/install-server.md) · [Connect nodes](docs-en/how-to/connect-nodes.md) |
| Fit more on my GPUs | [Choosing features](docs-en/explanation/choosing-features.md) · [dGPU](docs-en/how-to/dgpu.md) · [Sleep/wake](docs-en/how-to/sleep-wake.md) · [Two models on one GPU](docs-en/tutorials/one-gpu-two-models.md) |
| Serve bigger models or more traffic | [Multi-node inference](docs-en/how-to/multi-node-inference.md) · [PD disaggregation](docs-en/how-to/pd-disaggregation.md) · [Scale replicas](docs-en/tutorials/scale-model-replicas.md) |
| Measure and debug | [Benchmark](docs-en/how-to/benchmark.md) · [Monitoring and logs](docs-en/how-to/monitor-and-logs.md) · [Trace a slow request](docs-en/tutorials/trace-slow-request.md) · [Troubleshooting](docs-en/how-to/troubleshooting.md) |
| Integrate | [OpenAI gateway](docs-en/reference/openai-gateway.md) · [REST API](docs-en/reference/api.md) · [MCP](docs-en/how-to/use-mcp.md) · [CLI](docs-en/reference/cli.md) |
| Run it in production | [Production checklist](docs-en/how-to/deploy-production.md) · [Offline](docs-en/how-to/deploy-offline.md) · [Upgrades](docs-en/how-to/upgrade-and-compatibility.md) · [Security](docs-en/reference/security.md) |

Everything else: [documentation index](docs-en/README.md).

[Documentation Website](https://gpuworks.readthedocs.io/en/)

## Build from source

Requires Go 1.26 and Node.js.

```bash
make build      # web UI + server + agents into ./bin
make demo       # throwaway server + a GPU-less agent: onboarding, heartbeat, reconnect
make test       # static checks + Go unit tests + UI tests
make test-l2    # multi-process tests with real binaries and fake GPUs
```

See the [developer docs](docs-en/dev/README.md) and [testing guide](docs-en/dev/testing.md). How to file a
report, send a patch, or get a hardware combination into the support matrix is in
[CONTRIBUTING.md](CONTRIBUTING.md); contributor conventions are in [CLAUDE.md](CLAUDE.md) (Chinese) and
domain terms in [CONTEXT.md](CONTEXT.md). Security issues go to [SECURITY.md](SECURITY.md) — never a
public issue.

## Help us improve the product

Production installations send one anonymous usage report per day (hardware and engine mix, task counts, error
categories; no hostnames, model inputs or outputs, or any other sensitive information). Turn it off in
**Settings → General** or with `usage_report: false` in `server.yaml`. Every field is listed in
[User Experience Improvement Program](docs-en/reference/usage-report.md).

## License

GPUWorks is licensed under the [Apache License 2.0](LICENSE). Bundled third-party components keep their own
licenses; see [NOTICE](NOTICE).
