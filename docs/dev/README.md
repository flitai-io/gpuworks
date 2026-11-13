# 开发者文档

GPUWorks 由两个 `CGO_ENABLED=0` 的静态 Go 二进制和一个 React 前端组成。控制面入口是
`cmd/gpuworks`，节点入口是 `cmd/gpuworks-agent`；前端位于 `ui/`，构建结果嵌入 server。

## 从 clone 到 demo

```bash
git clone <repository-url> gpuworks
cd gpuworks
make demo
```

`make demo` 会构建 UI、server 和各平台 agent，再启动本地 server 与真实 agent，验证节点接入、
离线和重连。没有 GPU 的机器也可运行；脚本结束时会清理本次进程与临时目录。
详细依赖、端口和前端联调见[本地搭建](setup.md)。

## 仓库地图

| 目录 | 职责 |
|---|---|
| `cmd/` | server、agent 与目录维护 CLI |
| `internal/server/` | REST、控制器、调度、存储、网关和 Web UI 嵌入 |
| `internal/agent/` | 连接、探测、runtime、采集和传输 |
| `internal/proto/` | server/agent 通信约定（wire contract），包括消息格式与字段 |
| `pkg/` | 可复用的估算、模型解析、评测和日志解析库 |
| `ui/` | React + Vite 前端 |
| `test/` | fake engine/model 和分层测试资产 |
| `docs/adr/` | 已接受的架构决策 |
| `artifacts/` | 计划、研究与验收证据（`work/`/`archive/`/`research/`，见 `artifacts/README.md`） |

开发规约以根目录 [`CLAUDE.md`](../../CLAUDE.md) 为准；术语以
[`CONTEXT.md`](../../CONTEXT.md) 为准；逐包职责的详细说明见
[代码地图](code-map.md)。

## 下一步

- [代码地图：逐包职责](code-map.md)
- [本地开发环境](setup.md)
- [测试分层和云端策略](testing.md)
- [AMD 支持设计：职责、准入与兼容边界](amd-support-design.md)
- [AMD 接入结构与硬件验收](amd-hardware-validation.md)
- [评测信息与报告规范](benchmark-reporting-standard.md)
- [逐请求引擎排队耗时的观测边界](engine-queue-observation.md)
- [社区镜像仓库浏览](community-registry-browse.md)
- [浏览器 E2E 测试规范](../../test/e2e/browser-e2e-testing-standard.md)
- [浏览器 E2E 组件逐项检查清单](../../test/e2e/browser-e2e-component-checklists.md)
- [发布构建](release.md)
- [贡献和文档规则](contributing.md)

