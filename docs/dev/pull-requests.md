# GPUWorks PR / MR 操作指南

[PR / MR 模板](../../.github/PULL_REQUEST_TEMPLATE.md)

本指南说明如何准备、验证和提交 GitHub Pull Request（PR）或 GitLab Merge Request（MR）。
正文可以用中文或英文，只需填写一种语言。项目要求见
[贡献指南](../../CONTRIBUTING.zh-CN.md)、[开发规约](../../CLAUDE.md)和[测试指南](testing.md)。

开始前准备好本地 checkout、可推送的 fork 或工作分支，以及[开发环境](setup.md)。
以下命令均在仓库根目录执行。

## 1. 确定问题和范围

先搜索已有 issue、PR/MR 和文档。缺陷修复应写清触发条件、实际行为和预期行为；较大的功能或
架构改动，先与维护者确认方案。一个 PR 解决一个问题，避免混入格式化、依赖升级或其他无关修复。

涉及漏洞时，通过[安全策略](../../SECURITY.zh-CN.md)中的渠道私下报告，不在公开 PR 中发布利用细节。

## 2. 创建工作分支

以下命令假定 `upstream` 指向项目仓库、`origin` 指向自己的 fork。直接向项目仓库贡献时，
用 `origin` 替代 `upstream`。将 `TARGET_BRANCH` 替换为此次 PR 在托管平台上选定的目标分支，
并选择能说明用途的工作分支名，例如 `fix/agent-reconnect`、`feat/model-filter` 或 `docs/node-setup`。

```bash
git status --short
git remote -v
```

确认远端配置正确，妥善保存未提交的工作后再继续；不要用 reset 或清理命令覆盖别人的改动。
然后基于当前目标分支创建工作分支：

```bash
git fetch upstream
git switch -c fix/agent-reconnect upstream/TARGET_BRANCH
```

## 3. 实现并验证

先读相关代码、测试和 ADR。回归用例应能暴露原问题，并在修复后通过；覆盖必要的失败、超时、
取消和边界路径。API 改动核对 wire JSON 契约；UI 改动核对用户交互、状态和键盘操作。

同步更新受影响的文档。中文页面放在 `docs/`，英文译文放在 `docs-en/` 的相同相对路径。
按[贡献指南](../../CONTRIBUTING.zh-CN.md#文档约定)复核两版内容，更新
`docs-site/translations.json` 中的原文哈希。

运行项目要求的提交前检查；将下面的包路径替换为实际修改的包：

```bash
make test
go test -race -timeout 40m ./path/to/changed/package/...
make vet
git diff --check
```

`make test` 包含 UI、静态检查和默认 Go 套件，但不包含 L2/L3。改动涉及调度、部署、agent runtime
或容器行为时，还须执行 `make test-l2` 和 `make test-l3`。如需详细输出并指定 L3 超时，运行：

```bash
make test-l2
make test-l3 GO_TEST_FLAGS='-count=1 -v' L3_TIMEOUT=40m
```

检查输出中的跳过项。Docker 不可用时，容器用例可能跳过而命令仍返回 0，不能据此写“L3 通过”。
该目标的包级超时由 `L3_TIMEOUT` 控制；它不能替代作业的外部时限或资源限制。

使用专用测试环境，明确选择已获授权的容器运行时。独立 Docker socket 或数据目录不能隔离宿主网络，
不要在共享宿主网络命名空间内启动第二个 daemon。L4 真机或云端检查须具备所需资源及授权。
资源密集测试使用专用 worker，根据主机设置内存、交换空间、并发和运行时限。

记录被测 commit、实际命令、环境、退出码、跳过项和脱敏后的证据。结果使用 `PASS`、`FAIL`、
`SKIP`、`NOT_RUN` 或 `N/A`，所有非通过项均说明原因。超时属于未完成，不能记为通过；缺少硬件
属于未运行，不能记为不适用。仅当检查与改动无关时才写 `N/A`，并说明依据；模板不能免除项目要求。

GPU、镜像和模型相关改动须记录 GPU 型号及数量、CPU 架构、驱动、厂商软件栈、容器运行时、
镜像引用及摘要，以及适用时的模型 revision。镜像构建、CPU 验证、真实 GPU 推理和 Web 验证
分别记录；某个阶段或镜像摘要的证据不能证明另一个阶段或摘要已通过。

## 4. 审查差异并提交

每个提交聚焦一个问题，能够独立审查；提交说明解释问题、修复成立的原因和相关兼容性取舍。
只暂存本次改动的文件，将下面的路径替换为实际修改的文件：

```bash
git diff --stat
git diff
git add path/to/file path/to/test
git diff --cached
git diff --cached --check
git var GIT_AUTHOR_IDENT
git var GIT_COMMITTER_IDENT
```

确认暂存区没有凭据、私人连接信息、模型权重、大型构建产物或无关改动。核对姓名和邮箱后提交：

```bash
git commit -s
git log -1 --format=full
```

`-s` 添加本人的 `Signed-off-by` 尾注，它不是加密签名。只为本人签署，保留其他贡献者的署名；
amend、rebase 或应用 patch 后，重新核对作者和签署。后续每个提交都要检查暂存区差异。

## 5. 创建 PR / MR

将工作分支推送到自己的 fork 或有权限的仓库：

```bash
git push -u origin fix/agent-reconnect
```

选择正确的目标仓库、目标分支和工作分支。实现或验证尚未完成时，标记为 Draft。
按 [GitHub 模板说明](https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/creating-a-pull-request-template-for-your-repository)，
仓库唯一的 [PR 模板](../../.github/PULL_REQUEST_TEMPLATE.md)合入默认分支后，会自动填入新建 PR 的描述。
模板固定使用「问题 / 验证 / 说明」三个中文标题，填写提示放在 HTML 注释中，不设 checklist。
正文可以使用中文或英文；GitLab MR 可粘贴同一模板。

标题直接描述结果，例如 `Fix agent reconnection after a temporary connection failure` 或
`修复 agent 在临时断线后无法重连的问题`。正文先讲问题和修复后的行为，再讲方案、验证和限制。
只有完整解决托管平台上的 issue 时才使用 `Closes #123`；部分工作关联该 issue 并列出剩余事项。
外部工单填写编号及评审者可访问的链接。按三个小节填写实际内容；没有补充说明时写“无”。

## 6. 响应评审并完成合并

逐条回应评审意见，说明改动或保留原方案的理由。范围调整后，围绕最终实现更新标题和描述。
新增提交或处理冲突后，重跑受影响检查，更新被测 commit 和证据。重写分支历史前与共同使用者协调。

保留失败日志及其对应的 commit。怀疑是既有失败时，在相同环境下对照目标分支并报告两次结果。
后一次通过不能抹去前一次失败，必需检查失败也不能记为通过。

合并前确认评审意见已处理、必需检查通过，剩余的外部验收已有明确下一步。由具有权限的维护者
判断是否具备合并条件，并按仓库策略合并。所需用户文档、升级说明和回滚限制须随改动一起提交。
