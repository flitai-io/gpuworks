# GPUWorks Documentation

These documents describe current code behavior. One-time plans, acceptance records, and research materials belong in
[`artifacts/`](../artifacts/) (`work/` for ongoing work, `archive/` for completed work, and `research/` for standalone research;
see the index in [`artifacts/README.md`](../artifacts/README.md)). Chinese originals are stored in `docs/`, and English translations are stored in
`docs-en/` at matching relative paths. Command syntax, HTTP paths, and API fields remain unchanged; explanatory comments, prompts, and descriptive placeholders are translated. English translations are added in batches; the English site home page shows current coverage.

## Where to Start

| If You Are… | Read First |
|---|---|
| New to GPUWorks | [Install GPUWorks and Deploy Your First Model](tutorials/getting-started.md), then [Console Page Tour](reference/web-ui-tour.md) |
| Trying it locally on a Mac | [Run Your First Local Model on a Mac](tutorials/mac-local-model.md) |
| Trying it locally on Windows WSL2 | [Run GPUWorks on Windows WSL2 with Docker](how-to/wsl2-with-docker.md) |
| Going to production | [Install and Upgrade the Control Plane](how-to/install-server.md) → [Production Deployment](how-to/deploy-production.md) → [Upgrades and Version Compatibility](how-to/upgrade-and-compatibility.md) |
| Upgrading to a new version | [Release Notes](releases/README.md) → [Upgrades and Version Compatibility](how-to/upgrade-and-compatibility.md) |
| Having a problem | [Troubleshooting](how-to/troubleshooting.md) |
| Choosing an approach | [Choosing Between dGPU, Sleep/Wake, Multi-Node Inference, Replicas, and PD](explanation/choosing-features.md), [Typical Deployment Topologies](explanation/topologies.md) |
| Integrating with the API | [OpenAI-Compatible Gateway](reference/openai-gateway.md), [Complete REST API Routes](reference/api.md) |
| Contributing to development | [Developer Documentation Overview](dev/README.md) |

## User Documentation

### Tutorials

- [Install GPUWorks and Deploy Your First Model](tutorials/getting-started.md)
- [Run Two Models on One GPU: dGPU and Sleep/Wake](tutorials/one-gpu-two-models.md)
- [Run Your First Local Model on a Mac](tutorials/mac-local-model.md)
- [Scale a Model Service from One Replica to Two](tutorials/scale-model-replicas.md)
- [Deploy a PD Service and Observe the Request Path](tutorials/first-pd-deployment.md)
- [Run a Performance Benchmark and Understand the Report](tutorials/benchmark-concurrency.md)
- [Locate a Request with a Slow Response](tutorials/trace-slow-request.md)

### How-To Guides

**Installation and Onboarding**

- [Install and Upgrade the Control Plane](how-to/install-server.md)
- [Run GPUWorks on Windows WSL2 with Docker](how-to/wsl2-with-docker.md)
- [Upgrades and Version Compatibility](how-to/upgrade-and-compatibility.md)
- [Connect Nodes and Upgrade or Uninstall the Agent](how-to/connect-nodes.md)
- [Rent Cloud GPUs](how-to/rent-cloud-gpus.md)
- [Onboard ZStack Cloud](how-to/onboard-zstack-cloud.md)
- [Connect a Kubernetes Cluster](how-to/connect-kubernetes.md)
- [Connect Managed Kubernetes from a Public Cloud (Alibaba Cloud ACK, DigitalOcean, Nebius)](how-to/connect-managed-kubernetes.md)
- [Give the Console a Public Address](how-to/expose-public-address.md)
- [Production Deployment](how-to/deploy-production.md)
- [Configure Node Networks](how-to/configure-networks.md)

**Models and Images**

- [Use Storage: Local Directories, NFS, and NFS with Local Cache](how-to/storages.md)
- [Download, Cache, and Sync Models Between Nodes](how-to/download-and-sync-models.md)
- [Sync Models and Images Between Nodes](how-to/peer-sync-models.md)
- [Configure Image and Model Pulls: Source Rules, Proxies, Sync Groups, and Credentials](how-to/configure-pull-settings.md)
- [Import Models and Images from an HTTP File Server](how-to/import-from-http.md)
- [Deploy in an Offline Environment](how-to/deploy-offline.md)

**Deployment and Inference**

- [Deploy Models, Select GPUs, and Interpret Scheduling Results](how-to/deploy-models.md)
- [Deploy Models on Kunlun P800](how-to/kunlun-p800.md)
- [Multi-Node Inference](how-to/multi-node-inference.md)
- [Connect MinerU to a Remote Vision Model](how-to/use-mineru.md)
- [PD Disaggregation Deployment](how-to/pd-disaggregation.md)
- [Enable and Troubleshoot dGPU](how-to/dgpu.md)
- [Put Instances to Sleep and Wake Them](how-to/sleep-wake.md)
- [Use Scheduling Analysis: Schedules, Yield Policies, and Waiting Wakes](how-to/scheduling-center.md)
- [Reschedule Replicas on Node Loss](how-to/node-failover.md)
- [Run Batches: the Batch API and the Console](how-to/batch-api.md)

**Operations and Benchmarking**

- [Troubleshooting](how-to/troubleshooting.md)
- [View Metrics and Search Logs](how-to/monitor-and-logs.md)
- [View Usage Analytics](how-to/analyze-operations.md)
- [Export Request Traces to OpenTelemetry](how-to/export-request-traces.md)
- [Declare Engine Metrics for Inference Templates](how-to/declare-engine-metrics.md)
- [Save a Deployment as an Inference Template](how-to/save-deployment-as-template.md)
- [Run Benchmarks and Interpret Reports](how-to/benchmark.md)
- [Use the AI Assistant](how-to/use-ai-helper.md)
- [Connect Through MCP](how-to/use-mcp.md)

## Lookup Documentation

### Reference

- [Console Page Tour](reference/web-ui-tour.md)
- [Complete REST API Routes](reference/api.md)
- [OpenAI-Compatible Gateway](reference/openai-gateway.md)
- [CLI and Installation Scripts](reference/cli.md)
- [Configuration and Environment Variables](reference/configuration.md)
- [Control Plane Installation Layout](reference/install-layout.md) (including agent configuration files on nodes)
- [Network Monitoring Metrics](reference/network-metrics.md)
- [Inference Metrics Glossary](reference/inference-metrics.md)
- [Request Timeline and Stage Breakdown](reference/request-timeline.md)
- [GPU, Detection, Deployment, and dGPU Support Matrix](reference/support-matrix.md)
- [Ascend CANN Combination Matrix](reference/ascend-support-matrix.md)
- [Image Catalog and Inference Image Families](reference/image-families.md)
- [Sources of Deployment Launch Parameters](reference/deployment-launch-sources.md)
- [Executable Inference Catalog Package](reference/inference-catalog-package.md)
- [Inference Catalog model.yaml Syntax Reference](reference/inference-catalog-spec.md)
- [Security Defaults Checklist](reference/security.md)
- [User Glossary](reference/glossary.md)

## Why It Is Designed This Way

- [Architecture Overview: Server, Agent, and yamux](explanation/architecture.md)
- [runveil and udocker: Container Runtimes for Nodes Without Docker](explanation/runveil-and-udocker.md)
- [Typical Deployment Topologies](explanation/topologies.md)
- [Choosing Between dGPU, Sleep/Wake, Multi-Node Inference, Replicas, and PD](explanation/choosing-features.md)
- [Scheduling and GPU Memory Estimation](explanation/scheduling-and-estimation.md)
- [Yield Policy: How Deployments Share a Set of GPUs](explanation/service-levels.md)
- [Deployment Launch Configuration Semantics](explanation/launch-config.md)
- [SGLang Log Injection: Behavior and Boundaries](explanation/sglang-logging.md)
- [dGPU Semantics and Boundaries](explanation/dgpu.md)
- [Security Model and Trust Boundaries](explanation/security-model.md)
- [Architecture Decision Records (ADRs)](adr/README.md)

## Release Notes

- [Release notes for each version](releases/README.md): new features, behavior changes, bug fixes, and upgrade notes, newest first

## Developer Documentation

- [Developer Documentation Overview](dev/README.md)
- [Code Map: Package Responsibilities](dev/code-map.md)
- [Local Setup](dev/setup.md)
- [Testing Guide and Cloud Test Tiers](dev/testing.md)
- [AMD Integration and Hardware Validation](dev/amd-hardware-validation.md)
- [Benchmark Information and Reporting Standards](dev/benchmark-reporting-standard.md)
- [Observability Limits of Per-Request Engine Queue Time](dev/engine-queue-observation.md)
- [Browser E2E Testing Standards](../test/e2e/browser-e2e-testing-standard.md)
- [Releases and Embedded Assets](dev/release.md)
- [Contributing and Document Placement](dev/contributing.md)
- [PR / MR Workflow](dev/pull-requests.md)
- [Engineering Log](dev/ledgers/engineering-log.md), [A100 Bug Registry](dev/ledgers/a100-verification-bug-registry.md), [PPU Support Gap Register](dev/ledgers/ppu-support-gaps.md)

## Document Placement Rules

- Put descriptions of current behavior in `docs/`; put irreversible architectural trade-offs in `docs/adr/`.
- Put plans, acceptance records, and audits in `artifacts/`: ongoing work goes in `work/<YYYY-MM-DD>-<topic>/`. Once complete, move the entire topic directory into
  `archive/<YYYY-MM-DD>-<topic>/`. Put independently completed research in `research/<YYYY-MM-DD>-<topic>/`; archive resolved TODOs
  by month in `archive/todo-resolved/<YYYY-MM>.md`. Link to evidence in the main text instead of embedding process records. Record a plan document's status
  only in its own YAML frontmatter. `artifacts/README.md` and `artifacts/.ignore` are generated by
  `go run ./scripts/artifactsindex -write`; do not edit them manually.
- Do not use M numbers or relative paths outside the repository in current documentation. External research repository paths in historical logs are retained only as source references.
- `CLAUDE.md` is the sole authority for development conventions; `CONTEXT.md` is the complete glossary. Guides in this directory only expand on procedures and link to those files.
