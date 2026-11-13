# 为 GPUWorks 贡献代码

[English](CONTRIBUTING.md) | 简体中文

感谢你考虑参与这个项目。GPUWorks 是一个 GPU 集群管理平台：把一批 GPU 主机接进来，部署
vLLM / SGLang 推理服务，再通过 OpenAI 兼容网关对外提供调用。它要和硬件、容器运行时、各家
厂商的软件栈打交道，所以在正确的位置提出一个准确的报告，或者做一个能被验证的小修复，比一大块
没人能验证的补丁更有价值。

本文只讲协作机制。代码怎么组织，请看 [docs/dev/README.md](docs/dev/README.md) 和
[docs/dev/code-map.md](docs/dev/code-map.md)。

- [社区行为准则](#社区行为准则)
- [动手之前](#动手之前)
- [可以贡献什么](#可以贡献什么)
- [开发环境](#开发环境)
- [构建与测试](#构建与测试)
- [代码约定](#代码约定)
- [文档约定](#文档约定)
- [提交与拉取请求](#提交与拉取请求)
- [安全](#安全)
- [许可证](#许可证)

## 社区行为准则

参与本项目的所有人都需要遵守 [COMMUNITY_GUIDELINES.zh-CN.md](COMMUNITY_GUIDELINES.zh-CN.md)。
遇到不当行为请按其中给出的方式私下反映，不要发在公开 issue 里。

## 动手之前

1. **先搜索。** 现有 issue、已关闭的 issue，以及
   [docs/how-to/troubleshooting.md](docs/how-to/troubleshooting.md)，已经覆盖了我们收到的
   相当一部分问题。
2. **确认版本。** 几乎每个缺陷报告都需要 `curl -s http://<server>:8800/healthz` 的输出。
   版本形如 `dev-<commit>`，没有 commit 我们无法判断这个问题是不是已经修了。
3. **尽量在全新安装上复现。** 依赖上一次安装残留状态的现象，很难据此定位。

## 可以贡献什么

| 你手上的东西 | 该去哪里 |
|---|---|
| 一个缺陷 | Bug 报告 issue 表单 |
| 使用疑问或文档错误 | 问题咨询 issue 表单 |
| 安全漏洞 | [SECURITY.zh-CN.md](SECURITY.zh-CN.md) —— **绝不要开公开 issue** |
| 一个补丁 | 拉取请求，见下文 |
| 硬件验证结果（某个 GPU 型号、驱动、运行时组合可用或不可用） | 填好环境信息的 issue。项目维护一张支持矩阵，更新它需要证据 |

在这里，硬件证据很重要。如果你要报告某个组合可用，请附上确切的驱动 / CUDA / CANN 版本、
容器运行时，以及你实际执行了什么。没有证据的说法进不了
[docs/reference/support-matrix.md](docs/reference/support-matrix.md)。

## 开发环境

依赖：

- **Go** —— 版本以 `go.mod` 为准（当前 Go 1.26.11，与 CI 镜像 `golang:1.26.11` 一致）。
- **Node.js 与 npm** —— 版本以 `ui/package.json` 和 lockfile 为准。
- **GNU Make**。
- **Docker**、SSH 和厂商 CLI 只有在跑容器级/云端测试层，或者构建交叉编译 agent 时才需要。

```bash
go version
node --version
npm --version

make build      # 前端 + 内嵌 agent + server + agent，产物到 ./bin
make demo       # 起一个临时 server，接入一个无 GPU 的 agent，验证接入与重连
```

本地 server 默认监听 `:8800`，数据写入 `./data`。全新安装的 admin 口令写在
`data/initial-credentials`。

前后端分开启动：

```bash
make run-server
cd ui && npm install && npm run dev
```

开发服务器只用于热更新。要验证真正发布的嵌入产物，请运行 `make ui` 后再启动 Go server。

地址、SSH 参数、云厂商 key 只放在被 gitignore 的 `secrets/` 目录。不要提交真实凭据，
也不要把它们粘进 issue 或拉取请求。

更多细节见 [docs/dev/setup.md](docs/dev/setup.md)。

## 构建与测试

```bash
make test              # 前端构建 + 各项静态检查 + go test + 前端测试
make vet               # go vet
make test-integration  # 用假 agent/runtime 跑产品级测试
```

`make test` **不包含**分层测试。改动涉及调度、部署、agent 运行时或容器行为时，必须显式再跑
分层套件 —— 在这些区域里，分层测试不是可选项：

```bash
make test-l2         # 多进程：真实 server/agent 二进制，假 GPU
make test-l3         # 容器级，需要本地 Docker daemon
```

推送之前，完整的一轮是：

```bash
make test
go test -race -timeout 40m ./<你改动的包>/...
go vet ./...
git diff --check
```

几个省时间但容易漏的点：

- `-timeout 40m` 不是随手写的。`-race` 下 server 的大包远超默认 10 分钟（我们实测 store 约
  17 分钟、controller 约 16 分钟）。超时 panic 说明**没跑完**：不能据此判定改动有问题，但也
  不能算通过。保留日志，查清为什么卡住（死锁、goroutine 泄漏、机器过载），成功重跑之后再算
  验证通过。
- Lint 配置在 [.golangci.yml](.golangci.yml)（`govet`、`errcheck`、`staticcheck`、`unused`、
  `ineffassign`、`misspell`、`gofmt`、`goimports`、`revive`、`bodyclose`、`nilerr`）。
  提交前运行 `make lint`，CI 执行同一目标。该目标使用仓库的 Go 版本将
  golangci-lint v1.64.8 编译到 `bin/tools`，执行完整规则并展示全部诊断。
  首次运行会下载开发工具；缺少内嵌 UI 时会先构建 UI。
- `goimports` 配了 `local-prefixes: gpuworks.ai/gpuworks`，本地包单独成组。
- 分层测试与云端测试策略见 [docs/dev/testing.md](docs/dev/testing.md)。

## 代码约定

这个项目用少量明确的规则代替风格争论：

- **单文件控制在 600 行左右。** 在文件变成杂物间之前拆开。
- **线上 DTO 与 store entity 分开。** 它们变化的原因不同。
- **事务提交后再发布事件**，并且显式写出来。不要在还可能回滚的事务里发事件。
- **先确定契约，再改实现。** 先读现有代码、测试和 ADR，再改实现与同一层的测试。
- **不要混入无关改动。** 一个拉取请求应该只有一个存在的理由。
- **只在代码说不清的地方写注释。** 用大段注释解释算法，通常意味着该抽出一个函数并给它命名。

如果现有模式与上面的规则冲突，**跟随现有模式**，并在拉取请求里把这个矛盾提出来，而不是悄悄
另立一套约定。

## 文档约定

`docs/` 按用途分目录，放错地方的内容会被要求移动：

| 目录 | 放什么 |
|---|---|
| `docs/tutorials/` | 从零开始走主线，不展开原理 |
| `docs/how-to/` | 解决一个具体任务，不重复入门教学 |
| `docs/reference/` | 精确字段、矩阵、协议，不写叙事式流程 |
| `docs/explanation/` | 为什么这样设计 |
| `docs/adr/` | 不可逆的架构取舍，编号 `NNNN-*.md` |
| `docs/dev/` | 开发操作 |

文档是双语成对的：`docs/` 是中文版，`docs-en/` 是英文版，一页对一页。改一页必须同一个拉取请求
里同时改两边，否则两份会漂移——`make check-doc-links` 只校验链接，不校验两份内容是否对齐。
代码、API 字段和 CLI 输出在两版里都保持原文。

`make check-translations`（同样包含在 `make test-unit` 里）比"成对"要求更多：每个 `docs-en/`
页面都必须匹配 `docs-site/translations.json` 里记录的**已复核** SHA-256，且标题与代码块和中文
版一致。所以改一页是三步，不是两步：

1. 改 `docs/` 下的中文页；
2. 改 `docs-en/` 下的英文页，并对照这次改动复核译文——哈希写着"已复核"却没人读英文，等于把
   这项校验废掉；
3. 用下面的脚本重新计算源文件哈希把这次复核记录下来，然后跑校验：

```bash
python3 - <<'PY'
import hashlib, json, pathlib

page = "how-to/connect-nodes.md"   # 相对 docs/ 的路径，与 translations.json 的键一致
digest = hashlib.sha256(pathlib.Path("docs", page).read_bytes()).hexdigest()

path = pathlib.Path("docs-site/translations.json")
data = json.loads(path.read_text())
data[page] = digest
path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
PY
make check-translations
```

如果某个代码块在两版里本来就该不同（注释翻译、本地化示例），把复核过的这一对登记到
`docs-site/localized-code-blocks.json`；否则校验会报"代码块变了但没复核"。若失败发生在这次
改动没碰过的页面上，那是既有的缺口——留给那一页的维护者，不要直接把源文件哈希盖上去。

写具体行为时先读代码。参考手册里的 flag 表必须以 `--help` 输出为准。相对链接和锚点由
`make check-doc-links` 校验（已包含在 `make test-unit` 里）。

## 提交与拉取请求

创建分支、记录验证结果和响应评审的步骤见 [PR / MR 操作指南](docs/dev/pull-requests.md)。
统一使用 [PR 模板](.github/PULL_REQUEST_TEMPLATE.md)，保留「问题 / 验证 / 说明」三个标题，
正文只需填写中文或英文中的一种。

- 从当前目标分支拉出分支，名字说清做什么（`fix/pd-scaling-loop`、`docs/connect-nodes-typo`）。
- 提交信息要说明问题与改动，而不是只写动了哪个文件。
- 拉取请求描述里请写清楚：
  - 解决什么问题，你是怎么验证的；
  - 你跑过哪些测试，哪些**没有**跑；
  - 有哪些需要外部验收（特定 GPU、云账号、运行时）而你自己无法验证的部分。

写"未验证"是可以接受并且有用的；写"应该没问题"不行。如果某条分支行为没有被测到，请明确说出来，
不要让评审者误以为已经覆盖。

CI 会跑静态检查、安全门禁、Go 与前端单测、分层编译检查和打包。流水线变红是阻断项，不是过场。

## 安全

不要在公开 issue 里报告漏洞，也不要在拉取请求里附带利用细节。请按
[SECURITY.zh-CN.md](SECURITY.zh-CN.md) 处理。

## 许可证

GPUWorks 以 **Apache License 2.0** 发布，全文见 [LICENSE](LICENSE)。

提交贡献即表示你同意按同一许可证授权给本项目（inbound = outbound），版权仍归你所有；同时你
确认有权提交该内容 —— 代码是你写的，或者它原有的许可证允许贡献到这里。

在把第三方代码粘进来之前，有两条义务需要知道：

- **引入的代码必须兼容。** Apache-2.0 不是 copyleft，但它与 GPL-2.0-only 也不兼容。不要把
  GPL-2.0 的代码复制进这个仓库。如果要引入其它许可证的代码，请在做之前先在拉取请求里提出来。
- **保留声明。** 不要删除你修改的文件里的版权、专利、商标或归属声明，保留所有 `NOTICE` 文件，
  并在实质修改某个文件时明确说明"该文件已修改"（第 4 条）。

Apache-2.0 还包含贡献者给出的明确专利授权（第 3 条），这也是项目选择它而不是 MIT 的主要理由。
