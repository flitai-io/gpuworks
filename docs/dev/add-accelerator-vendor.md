# 接入一个新的加速卡厂商

本页是照着做的步骤，末尾是评审这类合并请求时逐条核对的规范。为什么这样分工见 [ADR 0082](../adr/0082-vendor-and-runtime-seams.md)。

接入一个厂商，你写两样东西：一份**厂商定义**（它是谁、能做什么）和一个**驱动**（怎么从厂商工具读出卡、把哪些设备交给负载）。探测流程、PCI 花名册、设备怎么交给各个容器运行时，都由已有代码负责。

不需要改的地方：任何运行时驱动（`internal/agent/runtime/*`）、调度器、其他厂商的文件。如果发现非改不可，先停下来，那是接缝的缺口，单独提出来。

## 要动的文件

| 内容 | 位置 |
|---|---|
| 厂商定义 | 新增 `internal/accelerator/<厂商>.go`；在 `vendor.go` 里加厂商常量；在 `vendor_spec.go` 的 `vendorSpecs` 里加一行 |
| 驱动 | 新增 `internal/agent/gpu/<厂商>*.go`；在 `gpu.go` 的 `defaultProbers` 里加一行 |
| 夹具 | `internal/agent/gpu/testdata/`，真机上抓的工具原始输出 |
| 指标（有新指标时） | 在 `internal/platformmetrics` 里登记指标名 |
| Kubernetes（`runtimes` 列了 `kube` 时） | 在 `internal/accelerator/kube.go` 的 `ClassifyKubeResource` 里加扩展资源名 |

登记是两行，不能合成一行：定义在 server 与 agent 共用的包里，驱动只在 agent 里。

控制台的厂商顺序、名称与框架归属来自 server 下发的厂商字典（`GET /api/v1/accelerator-vendors`），支持矩阵里「定义里声明的支持范围」一表由定义生成，两者都不用手改；改完定义后重新生成表和控制台测试夹具（见第三步）。仍要手工补的是图标（`ui/src/components/vendorAssets.ts` 与 `ui/public/vendor-logos/`；没有图标时控制台画一个带首字母的圆标，颜色和字母可以在 `VendorLogo.tsx` 里指定）和 `common.json` 里 `vendor.<id>` 的译名。别的页面各自的厂商译名可以不补：模板页的（`inferenceTemplates.json`）没有时用 server 给的名字，部署页的（`deploy.json`）没有时显示 id 的大写。

## 第一步：厂商定义

在 `internal/accelerator/<厂商>.go` 里写一个 `vendorSpec` 值。每个字段的含义写在 `vendor_spec.go` 的 `vendorSpec` 上，字段按四组排列，照已有厂商的文件（例如 `hygon.go`）一行一个字段地写。

```go
// exampleVendor is Example. PCI identity measured 2026-12-01 on host-a:
// EX100 1234:0001 class 0x0302 bound to exdrv.
var exampleVendor = vendorSpec{
	id:         VendorExample,
	brand:      "Example",
	frameworks: []string{"exf"},
	named:      contains("example", "exf"),
	precedence: 45,

	tool:           "ex-smi",
	detectCommand:  "ex-smi",
	controlDevices: []string{"/dev/exctl"},
	pciID:          0x1234,
	pci:            &pciVendor{classes: []uint32{pciClass3D}, drivers: []string{"exdrv"}},

	runtimes:        []string{RuntimeDocker, RuntimeNerdctl},
	supports:        []Feature{FeatureMultiNode},
	ownCatalogImage: true,

	cardChecks: true,
}
```

规则只有一条：**没声明就是不支持。**

- `runtimes` 列出它的卡能在哪些运行时驱动上交给负载。节点用的运行时不在列表里，调度前拒绝。一个都不列，这家的卡就只被发现和监控，不承载负载；先接监控、后接部署的厂商就这样起步。
- `supports` 列出卡上提供的平台功能。没列的，server 在调度前拒绝，agent 在准入时拒绝。
- `partition`（dGPU 后端）、`gpuPlugins`、`healthProbe` 等没有就不写。
- 两个列表里只写在真机上验证过的项。
- `precedence` 决定一个字符串同时像两个厂商时归谁，数值小的先试。`named` 里不要用会被别的厂商名包含的短词。
- `detectCommand` 只在工具名是这家独有时填（昆仑芯的 `xpu-smi` 与 Intel 的同名，所以不填）。
- `pci` 以真机 `lspci -nn` 为准，在定义上方的注释里写明机器和日期。不要为了方便加整个 `0x03` 显示类，服务器的 BMC 显示芯片也是这一类。
- 上游 `pci.ids` 没有这张卡的名字时，在 `pciModels` 里写型号名和出处。

## 第二步：驱动

驱动实现 `gpu.Prober`。`Probe` 不自己写流程，交给 `probeCards`；你只回答两件事：工具装了没有，工具的输出怎么变成卡。

```go
func (p *ExampleProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	_, err := lookCLI("ex-smi")
	return probeCards(ctx, accelerator.VendorExample, p.hostRoot, err == nil, p.list, &p.seen)
}
```

`list` 的规则：

- 工具问不到就返回错误，不要同时返回卡。问不到时节点上报的是 PCI 花名册，由 `probeCards` 处理。
- 每张卡填全：序号、UUID、型号、PCI 地址、显存总量（字节）、厂商名；有健康结论时用 `SetCardFinding`。
- 行数对不上、序号或地址重复时整份拒绝，不把截断的输出当成完整清单。
- 执行工具用 `queryLogged` 或 `RunVendorCLI`，它们负责超时、卡死进程的隔离和在宿主机命名空间里执行。不直接用 `exec.Command`；`cliCommand` 只负责进宿主机命名空间，没有超时和卡死隔离，也不要直接用。现有驱动里有几处还是这样执行的（NVIDIA 与昇腾查运行时插件的版本、昇腾的 `docker info`、PPU 用 `ldconfig` 找共享库），不要照着写。

`p.seen` 是 `cardBaseline`：上一次问到答案时每个序号是哪张卡。授予和指标晚于探测运行，序号可能已经指向另一张卡，用它比对身份，不一致就拒绝。

指标名用 `internal/platformmetrics` 里登记过的。

驱动按需实现的可选接口。实现了框架就会用，没实现就是没有这项能力；后两个还要定义里有对应的声明才完整。这里列的是接入时最常用的几个，`internal/agent/gpu` 里还有别的按类型断言使用的接口（拓扑、NVLink、加速端口）。

| 接口 | 作用 | 定义里还要有 |
|---|---|---|
| `DriverAwareInjector` | 授予方式随运行时不同 | 无 |
| `ComputeProcessLister` | 卡上有哪些进程，用于判断平台之外的占用 | 无 |
| `HostRuntimeChecker` | 在主机上让厂商运行时实际用一张卡（逐卡运行时检查） | `checks` 里列 `CheckRuntime`。不列的话检查照样跑、失败照样把卡标成不可用，但结果不上报，控制台也不提供复查 |
| `ContainerRuntimeGrader` | 在镜像里跑运行时检查时，怎么判定输出 | `smokeFramework` 与 `smokeScript`，不写就不会在镜像里跑；`checks` 里的 `CheckRuntime`，作用同上一行 |

### 设备授予

`RuntimeInjection` 说「这个 workload 要哪些设备」，不写任何容器命令行选项：

```go
return RuntimeInjection{
	Env: map[string]string{"EXF_VISIBLE_DEVICES": visible},
	Grant: runtime.DeviceGrant{
		Devices: []runtime.Device{{Path: "/dev/exctl", Access: runtime.DeviceAccessAll}, {Path: node, Access: runtime.DeviceAccessAll}},
		Groups:  groupIDs,
	},
}
```

- docker、nerdctl、kube、udocker 各自把 `Grant` 变成自己的形式。
- 要顶替 workload 自带的设置（别家的 `--gpus`、`--runtime`、某些设备映射）时写在 `Grant.Replaces` 里；要清掉的环境变量写在 `ClearEnv` 里。不要改 workload 的选项字符串。
- 授予方式随运行时不同时，实现 `DriverAwareInjector`，读 `target.View`（容器只看到被授予的设备、看到宿主机的设备、没有容器），不比较驱动名。
- 设备节点按 PCI 地址从 sysfs 找，并核对设备号，不假设「序号 N 就是 `/dev/xxxN`」。校验不过就返回 `UnsupportedReason`，不返回一份可能指错卡的授予。

## 第三步：夹具与测试

- 夹具是真机上抓的工具原始输出，原样放进 `testdata/`，文件名带工具名。
- 解析函数对夹具做精确断言：卡数、每张卡的地址和显存。
- 覆盖失败路径：工具超时、命令失败但有部分输出、空输出、乱码或截断的行、序号漂移后的授予。每个用例在去掉对应处理后必须失败。
- 整个 `Probe` 至少有一个经过 `probeCards` 的用例，把 `hostRoot` 指到 `t.TempDir()`；其中一个在 `Probe` 之后让同一序号换一张卡，断言授予被拒绝（`probe_baseline_test.go` 里有 PPU 和海光的写法）。
- 不写「把定义再抄一遍」的测试。定义本身就是陈述；测试证明的是行为。

加一个厂商后，下面这些已有测试会要求你更新，这是有意的：

| 测试 | 要做的 |
|---|---|
| `accelerator` 的 `TestVendorDefinitionsAreConsistent` | 按报错修定义。这个测试不点名厂商，没有例外名单可加 |
| `accelerator` 的 `TestKnownVendorsIsTheClosedSetNormalizeVendorProduces` | 加进期望的列表 |
| `accelerator` 的 `TestVendorRuntimeAdmission` | 加一行：这个厂商在哪些运行时上被接受。它决定调度前拒绝什么，所以写出来 |
| `agent/gpu` 的 `TestDefaultProbersPreserveExistingVendorOrder`、`TestMixedVendorAutoKeepsExistingProberPriority` | 加进期望的探测顺序。NVIDIA 排在所有带兼容版 `nvidia-smi` 的厂商之后 |
| `server/deploy` 的 `TestEngineProfileGoldens` | `GPUWORKS_UPDATE_PROFILE_GOLDEN=1 go test -run TestEngineProfileGoldens ./internal/server/deploy/` |
| `accelerator` 的 `TestSupportMatrixTableIsWrittenFromTheDefinitions` | `GPUWORKS_UPDATE_SUPPORT_MATRIX=1 go test -run TestSupportMatrixTableIsWrittenFromTheDefinitions ./internal/accelerator/`，然后 `python3 docs-site/check_translations.py --write docs/reference/support-matrix.md` 记下该页新的来源哈希 |
| `server/api` 的 `TestAcceleratorVendorsWireJSONIsTheConsoleFixture` | `GPUWORKS_UPDATE_VENDOR_FIXTURE=1 go test -run TestAcceleratorVendorsWireJSONIsTheConsoleFixture ./internal/server/api/` |

## 第四步：提交前

```bash
make test
make test-l2
make test-l3
go test -race ./internal/accelerator/... ./internal/agent/gpu/...
```

## 真机验证

离线测试只证明解析和流程。下面这些只有真机能回答，没验证过就不要写进 `runtimes`、`supports` 或支持矩阵：

- 容器里按什么编号选卡：选一张非 0 号的卡、选不连续的几张卡，容器里看到的是不是这几张、顺序对不对。
- 没被授予的卡在容器里确实用不了。
- 在每个列出的运行时上各跑一次真实的引擎启动和一次对话。
- 卡被直通或掉线后，序号重排时探测和授予的表现。

## 评审规范

评审接入厂商或运行时的合并请求时逐条核对。每一条都有一个可以执行的核对办法；说不出核对结果的，按不满足处理。

| # | 要求 | 怎么核对 |
|---|---|---|
| 1 | 只动了自己的文件 | `git diff --stat <base>` 里的生产文件只有：新厂商的定义与驱动文件、`vendor.go` 里一个厂商常量、`vendor_spec.go` 一行、`gpu.go` 一行、前端的图标与译名、生成的支持矩阵表与控制台夹具；有新指标时再加 `internal/platformmetrics` 里的登记，列了 `kube` 时再加 `kube.go` 里的扩展资源名。测试、golden 和其余文档不算在内。出现运行时驱动、调度器或别家的文件，要求拆成单独的合并请求 |
| 2 | 定义自洽 | `go test -run TestVendorDefinitionsAreConsistent ./internal/accelerator/` 通过，且这个测试文件没有被改动 |
| 3 | 预算没有变大 | `make check-architecture` 通过，`git diff <base> -- scripts/arch-constraints.txt` 里没有上调的数字 |
| 4 | 驱动不写容器命令行选项 | 表后的命令 4 没有输出 |
| 5 | 驱动不按运行时名字分支 | 表后的命令 5 没有输出。它匹配与驱动名（字面量或三组常量）的 `==`、`!=`、`case`、`EqualFold`、`Contains` |
| 6 | `Probe` 走 `probeCards` | 表后的命令 6 有一处；驱动里没有调用 `Roster`，没有自己的「上次看到的卡」类型。授予时要核对卡的身份的驱动（按序号找设备节点的都要；现有驱动里昇腾按芯片号拼 `/dev/davinciN` 而没有核对，是已知缺口，不要照写），传的是自己的基线而不是 `nil`，并有一个用例先跑 `Probe`、再让同一序号换一张卡，授予被拒绝 |
| 7 | 指标名是登记过的 | `go test -run TestEveryMetricLiteralIsRegistered ./internal/platformmetrics/` 通过 |
| 8 | `runtimes` 与 `supports` 的每一项有真机证据 | 合并请求的验证一节对每一项写了机器、驱动版本、镜像摘要和实际命令。没有证据的项从定义里删掉 |
| 9 | 夹具是原始输出 | 夹具文件的提交说明或同目录说明写了机器和日期；PCI 身份与 `pciModels` 的注释里有出处 |
| 10 | 失败路径的测试真的在测 | 评审者任选一个失败路径用例，去掉驱动里对应的处理，确认用例变红 |
| 11 | 没有抄表测试 | 新增的测试里没有把定义的字段值再列一遍的断言。`TestVendorRuntimeAdmission` 里的那一行除外：它写的是调度前接受和拒绝哪些运行时 |
| 12 | 文档只写验证过的事实 | 支持矩阵里新增的每一格在验证一节有对应的证据 |

第 4、5、6 条的命令（表格里写不下竖线，原样写在这里；把 `<厂商>` 换成文件名前缀）：

```sh
# 4：没有输出
grep -nE '"--(device|gpus|runtime|group-add|tmpfs)"' internal/agent/gpu/<厂商>*.go | grep -v '_test\.go'
# 5：没有输出。模式取自 scripts/arch-constraints.md 的 agent_runtime_name_comparisons
N='"(docker|nerdctl|udocker|kube|host)"|(agent)?runtime\.Driver[A-Z]|proto\.RuntimeDriver|kube\.DriverName|hostproc\.DriverName|accelerator\.Runtime(Docker|Nerdctl|Udocker|Kube|Host)'
grep -nE "(==|!=) *($N)|($N) *(==|!=)|case .*($N)|(EqualFold|Contains)\(.*($N)" internal/agent/gpu/<厂商>*.go | grep -v '_test\.go'
# 6：有一处；授予时核对身份的驱动，最后一个参数是自己的基线（&p.seen），不是 nil
grep -n 'probeCards(' internal/agent/gpu/<厂商>*.go | grep -v '_test\.go'
```

## 不要做的事

- 不在运行时驱动里加厂商分支，不在 udocker 里加从环境变量名推断厂商的函数。
- 不在接入厂商的同一个合并请求里改所有厂商共用的逻辑（检查的调度、占用的判定、花名册合并）。需要改就单独提，并对现有厂商做基线对照。
- 不把「设备节点存在」「环境变量设了」当成「能跑推理」。
