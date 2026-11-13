# 本地开发环境

## 依赖

- Go 版本以 `go.mod` 为准（当前为 Go 1.26.11，与 CI 镜像 `golang:1.26.11` 一致）。
- Node.js 与 npm；前端依赖版本以 `ui/package.json` 和 lockfile 为准。
- GNU Make。构建或 L3/L4 测试还可能需要 Docker、SSH、`curl` 和目标厂商 CLI。

```bash
go version
node --version
npm --version
make demo
```

通过 `make run-server` 启动的本地 server 默认监听 `:8800`、数据写入 `./data`。并行开发实例依次使用 8800、8802、
8804、8806；手工测试密码使用 `password`，不要把测试密码用于生产。
`make demo` 使用独立的 `127.0.0.1:8099` 和临时目录，并为本次运行生成管理员密码；
脚本结束时会清理它创建的进程与临时目录。

## 浏览器 E2E

E2E 需要的依赖比构建更多，且分四层：只跑 Playwright 与系统 Chrome 的、加 `bin/gpuworks` 的、
加 `bin/gpuworks-agent` 的、再加 Docker daemon 的。多数开发改动落在前两层，不需要 GPU。
完整清单、每层覆盖多少脚本、以及缺依赖时的排查方法见
[test/e2e/RUNNING.md](../../test/e2e/RUNNING.md)。

```bash
# Python 依赖（无虚拟环境时也可用 --user）
python3 -m pip install -r requirements-e2e.txt
google-chrome --version          # 测试用 channel="chrome"，不下载 chromium

make build                       # 产出 bin/gpuworks 与 bin/gpuworks-agent
make test-e2e-smoke              # 最小链路：真实 Chrome + 隔离 server + SQLite
make test-e2e-docker-check       # 需要 Docker 的用例是否就绪
```

`check-libvgpu` 需要 `objdump`（`apt install binutils`）。**不要导出 `GO`、`CGO_ENABLED`、
`GOTOOLCHAIN`、`GOMAXPROCS`**：它们会污染 `cross-agents` 循环里的 `$(GO)` 展开，症状是
`/bin/sh: 4: build: not found`，与真实编译错误无关。需要换工具链时用 `make ... GO=<绝对路径>`。

## 分开启动后端与前端

在仓库根目录打开一个终端，启动后端并保持它运行：

```bash
make run-server
```

再打开一个终端，从仓库根目录进入 `ui/`，启动支持热更新的前端开发服务器：

```bash
cd ui
npm install
npm run dev
```

正式 `make ui` 会先做 i18n 检查，然后把产物写入 `internal/server/webui/dist`。前端开发
服务器只用于热更新；验证嵌入产物时应运行 `make ui` 后再启动 Go server。

## 本地秘密

地址、SSH 参数和云 key 只放在被 gitignore 的 `secrets/`。可创建
`secrets/test-environments.md` 记录本机环境，但不要提交真实值。环境变量的非敏感说明见
[配置参考](../reference/configuration.md)。

## 部署到长期验收环境

长期运行的验收 server 与真机（地址、SSH 方式、账号）记录在被 gitignore 的 `secrets/test-environments.md`，
不入仓库。更新这类 server 用 `gpuworks install` 重新安装同一个目录：它会复制自身、重写 service 并探活，
不需要手写 systemd 单元。

`install` 不做备份（只有 `upgrade` 会备份）。替换前先在验收机上复制一份旧二进制
`/opt/gpuworks/server/gpuworks`；想回滚时，从目标 commit 重新构建再 install。

构建命令在仓库根目录执行：

```bash
make server-linux-amd64          # 带 ui 和内嵌 agent，不要用 go build 代替
```

安装和探活命令在 Linux amd64 验收机执行。如果在其他机器构建，先把产物复制到验收机，
并把下面的 `./bin/gpuworks-linux-amd64` 替换为复制后的路径：

```bash
./bin/gpuworks-linux-amd64 install --dir /opt/gpuworks/server --addr :8800
curl --fail http://127.0.0.1:8800/healthz
```

部署后若服务启动失败，先看 `systemctl status gpuworks` 和 `journalctl -u gpuworks`。

## 常用检查

```bash
make test
make test-integration
make vet
git diff --check
```

更完整的分层、race、build-tag 和真机要求见[测试指南](testing.md)。
