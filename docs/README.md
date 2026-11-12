# GPUWorks 文档

这里的文档描述当前代码行为；一次性计划、验收记录和研究材料放在
[`artifacts/`](../artifacts/)（`work/` 进行中、`archive/` 已收口、`research/` 独立调研，
索引见 [`artifacts/README.md`](../artifacts/README.md)）。中文原文保存在 `docs/`，英文译文保存在
`docs-en/`，按相同相对路径对应；命令语法、HTTP 路径和 API 字段保持不变；说明性注释、提示和占位符译为英文。英文翻译分批补齐，站点英文首页显示当前覆盖率。

## 从哪里开始

| 你是…… | 先读 |
|---|---|
| 第一次接触 GPUWorks | [从零启动并部署第一个模型](tutorials/getting-started.md)，再看[控制台页面导览](reference/web-ui-tour.md) |
| 在 Mac 上本地试用 | [在 Mac 上运行第一个本地模型](tutorials/mac-local-model.md) |
| 在 Windows WSL2 上本机试用 | [在 Windows WSL2 上用 Docker 运行 GPUWorks](how-to/wsl2-with-docker.md) |
| 要在生产上线 | [安装和升级控制面](how-to/install-server.md) → [生产环境部署](how-to/deploy-production.md) → [升级与版本兼容](how-to/upgrade-and-compatibility.md) |
| 要升级到新版本 | [发布说明](releases/README.md) → [升级与版本兼容](how-to/upgrade-and-compatibility.md) |
| 遇到问题 | [排障指南](how-to/troubleshooting.md) |
| 在选方案 | [怎么选：dGPU、sleep/wake、多机、副本、PD](explanation/choosing-features.md)、[典型部署拓扑](explanation/topologies.md) |
| 对接 API | [OpenAI 兼容网关](reference/openai-gateway.md)、[REST API 全量路由](reference/api.md) |
| 参与开发 | [开发者文档总览](dev/README.md) |

## 用户入口

### 教程（tutorials）

- [从零启动并部署第一个模型](tutorials/getting-started.md)
- [一张 GPU 跑两个模型：dGPU 与 sleep/wake](tutorials/one-gpu-two-models.md)
- [在 Mac 上运行第一个本地模型](tutorials/mac-local-model.md)
- [把一个模型服务从一副本扩到两副本](tutorials/scale-model-replicas.md)
- [部署 PD 服务并观测请求链路](tutorials/first-pd-deployment.md)
- [运行一次性能评测并读懂报告](tutorials/benchmark-concurrency.md)
- [定位一次回答慢的请求](tutorials/trace-slow-request.md)

### 操作指南（how-to）

**安装与接入**

- [安装和升级控制面](how-to/install-server.md)
- [在 Windows WSL2 上用 Docker 运行 GPUWorks](how-to/wsl2-with-docker.md)
- [升级与版本兼容](how-to/upgrade-and-compatibility.md)
- [接入节点、升级与卸载 agent](how-to/connect-nodes.md)
- [租用云 GPU](how-to/rent-cloud-gpus.md)
- [接入 ZStack Cloud](how-to/onboard-zstack-cloud.md)
- [接入 Kubernetes 集群](how-to/connect-kubernetes.md)
- [接入公有云托管 Kubernetes（阿里云 ACK、DigitalOcean、Nebius）](how-to/connect-managed-kubernetes.md)
- [给控制台一个公网地址](how-to/expose-public-address.md)
- [生产环境部署](how-to/deploy-production.md)
- [配置节点网络](how-to/configure-networks.md)

**模型与镜像**

- [使用存储：本地目录、NFS 与 NFS 加本地缓存](how-to/storages.md)
- [下载、缓存和节点间同步模型](how-to/download-and-sync-models.md)
- [在节点之间同步模型和镜像](how-to/peer-sync-models.md)
- [配置镜像与模型拉取：来源规则、代理、同步组与凭据](how-to/configure-pull-settings.md)
- [从 HTTP 文件服务器导入模型与镜像](how-to/import-from-http.md)
- [离线环境部署](how-to/deploy-offline.md)

**部署与推理**

- [部署模型、选卡与解释调度结果](how-to/deploy-models.md)
- [在昆仑 P800 上部署模型](how-to/kunlun-p800.md)
- [多机推理](how-to/multi-node-inference.md)
- [用 MinerU 连接远程视觉模型](how-to/use-mineru.md)
- [PD 分离部署](how-to/pd-disaggregation.md)
- [启用和排查 dGPU](how-to/dgpu.md)
- [让实例 sleep / wake](how-to/sleep-wake.md)
- [使用调度分析：定时计划、出让策略、唤醒等位](how-to/scheduling-center.md)
- [节点失联重调度](how-to/node-failover.md)
- [运行批处理任务：Batch API 与控制台](how-to/batch-api.md)

**运维与评测**

- [排障指南](how-to/troubleshooting.md)
- [查看指标与检索日志](how-to/monitor-and-logs.md)
- [查看用量分析](how-to/analyze-operations.md)
- [导出请求 trace 到 OpenTelemetry](how-to/export-request-traces.md)
- [为推理模板声明引擎指标](how-to/declare-engine-metrics.md)
- [把部署另存为推理模板](how-to/save-deployment-as-template.md)
- [运行评测并解读报告](how-to/benchmark.md)
- [使用 AI 助手](how-to/use-ai-helper.md)
- [通过 MCP 接入](how-to/use-mcp.md)

## 查表入口

### 参考（reference）

- [控制台页面导览](reference/web-ui-tour.md)
- [REST API 全量路由](reference/api.md)
- [OpenAI 兼容网关](reference/openai-gateway.md)
- [CLI 与安装脚本](reference/cli.md)
- [配置与环境变量](reference/configuration.md)
- [控制面安装布局](reference/install-layout.md)（含节点上的 agent 配置文件）
- [网络监控指标](reference/network-metrics.md)
- [推理指标词表](reference/inference-metrics.md)
- [请求时间线与阶段拆分](reference/request-timeline.md)
- [GPU、探测、部署与 dGPU 支持矩阵](reference/support-matrix.md)
- [昇腾 CANN 组合矩阵](reference/ascend-support-matrix.md)
- [镜像目录与推理镜像家族](reference/image-families.md)
- [部署启动参数来源](reference/deployment-launch-sources.md)
- [可执行推理目录包](reference/inference-catalog-package.md)
- [推理目录 model.yaml 语法参考](reference/inference-catalog-spec.md)
- [安全默认值清单](reference/security.md)
- [用户术语表](reference/glossary.md)

## 为什么这样设计

- [架构总览：server、agent 与 yamux](explanation/architecture.md)
- [runveil 与 udocker：无 docker 节点的容器运行时](explanation/runveil-and-udocker.md)
- [典型部署拓扑](explanation/topologies.md)
- [怎么选：dGPU、sleep/wake、多机、副本、PD](explanation/choosing-features.md)
- [调度与显存估算](explanation/scheduling-and-estimation.md)
- [出让策略：几个部署怎样共用一批 GPU](explanation/service-levels.md)
- [部署启动配置的语义](explanation/launch-config.md)
- [SGLang 日志注入的行为与边界](explanation/sglang-logging.md)
- [dGPU 的语义与边界](explanation/dgpu.md)
- [安全模型与信任边界](explanation/security-model.md)
- [架构决策记录（ADR）](adr/README.md)

## 发布说明

- [各版本发布说明](releases/README.md)：新增功能、行为变化、缺陷修复与升级须知，最新版本在前

## 开发者入口

- [开发者文档总览](dev/README.md)
- [代码地图：逐包职责](dev/code-map.md)
- [本地搭建](dev/setup.md)
- [测试指南与云端分层](dev/testing.md)
- [AMD 接入结构与硬件验收](dev/amd-hardware-validation.md)
- [评测信息与报告规范](dev/benchmark-reporting-standard.md)
- [逐请求引擎排队耗时的观测边界](dev/engine-queue-observation.md)
- [浏览器 E2E 测试规范](../test/e2e/browser-e2e-testing-standard.md)
- [发布与内嵌资产](dev/release.md)
- [贡献与文档落位](dev/contributing.md)
- [PR / MR 操作指南](dev/pull-requests.md)
- [工程台账](dev/ledgers/engineering-log.md)、[A100 缺陷台账](dev/ledgers/a100-verification-bug-registry.md)、[PPU 缺口台账](dev/ledgers/ppu-support-gaps.md)

## 文档落位规则

- 描述当前行为放 `docs/`；不可逆的架构取舍放 `docs/adr/`。
- 计划、验收和审计放 `artifacts/`：进行中放 `work/<YYYY-MM-DD>-<主题>/`，收口后整个主题目录搬进
  `archive/<YYYY-MM-DD>-<主题>/`，独立完成的调研放 `research/<YYYY-MM-DD>-<主题>/`；已解决的 TODO
  按月归档放 `archive/todo-resolved/<YYYY-MM>.md`。正文只链接证据，不内联过程记录。计划文档的状态
  只写在文件自身的 YAML frontmatter 里，`artifacts/README.md` 与 `artifacts/.ignore` 由
  `go run ./scripts/artifactsindex -write` 生成，不手改。
- 不在现行文档写 M 编号或仓库外相对路径。历史台账中的外部研究仓路径仅作为来源说明保留。
- `CLAUDE.md` 是开发规约的唯一权威；`CONTEXT.md` 是术语全集。本目录中的指南只展开做法并链接它们。
