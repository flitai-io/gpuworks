# 把部署另存为推理模板

**适用场景**：你在 GPUWorks 里把一个模型跑通了（服务在运行，API 能答），想把这套配置变成推理模板，留给自己或别的集群复用，或者贡献给官方目录，但不想手写 `model.yaml`。

完成后，你得到一份能直接导入的平铺 YAML 和一段测试记录。YAML 里的事实字段都来自部署实际运行的内容，不需要人工填写。

**前置条件**：部署处于运行中，至少有一个运行中的实例；你能登录控制台（导出不要求管理员，存进目录要求管理员）。

## 它从哪里取数据

| 模板字段 | 来源 |
|---|---|
| `image` | 实例实际运行的镜像 |
| `args` | 部署的启动参数：表单字段、模板参数与用户自定义参数合在一起，按引擎实际生成的命令行写出；平台自己注入的参数（模型路径、端口、服务名、TP/PP、分布式参数）不写 |
| `env` | 源模板的环境变量；部署自己加的环境变量默认不导出，见下文 |
| `gpus` | 实例所在卡的厂商与型号，型号写成只匹配这一型号的正则（如 `^NVIDIA H20$`） |
| `weights` | 来自模板的部署沿用创建时保存的模板快照的权重来源；公网自定义部署写实例读取副本解析到的 commit；本地注册模型保存逻辑来源身份，不保存主机路径或伪造 commit |
| `model_config`、`context_length`、`input_modalities` | 来自模板的部署取当前目录中的模型元数据（估算信息，可能随目录更新）；自定义部署取模型行的 config.json 与已登记信息，不声明它们是实例启动时的快照 |
| `api` | 实例不可变启动记录中的接口声明，用该声明选择实测请求 |
| `kv_bytes_per_token` | 只在有可靠来源时写：源模板或目录里同一模型已有的值。没有时不写，在注释里说明 |
| `placement` | 来自模板的部署沿用创建时保存的模板快照；自定义部署不写，由估算与调度决定卡数，实测卡数与每卡显存写在注释里 |

导出以一个固定实例的不可变启动记录为准，保存但尚未应用的新配置不会被当成已实测。若配置与运行实例不同、在实测期间发生变化，或网关请求由其他副本回答，会拒绝验证；请应用配置后重试。源目录后续更新不会替换已执行的权重、预启动脚本、拓扑或 API 声明。

一次成功只证明了一种卡，所以 `gpus` 只匹配实测的型号；要放宽到更多型号，由你或评审者明确改写。dGPU 显存上限是这次部署的资源选择，不是模型的要求，只写进注释。

## 在控制台里操作

在「部署」页找到运行中的部署，打开行末的「更多」菜单，选择「另存为模板」。对话框先实测 API，再显示：

- **API 实测**：每个操作的结果；未实测的操作会注明原因。
- **模板写了什么、缺什么**：与 YAML 开头注释相同的事实，例如验证用的卡、未写的 placement、缺少的每 token KV 字节数。
- **未导出的环境变量**：勾选确实需要且不含密钥的变量，点「重新生成」。
- **模板 YAML**：可以直接修改。

底部的「下载 YAML」「复制测试记录」任何人都能用；「保存到目录」只对管理员显示，实测失败时不可用。保存成功后可以从对话框打开模板。下面的步骤是同一流程的 API 用法。

## 本地模型怎样复用

本地目录注册的模型也能另存。模板保留引擎参数，并使用现有权重来源结构引用已注册模型的 `source_id`：

```yaml
weights:
  quantization: bf16
  sources:
    - local: company-tuned-model
```

这里的 `local` 不是下载提供者，值是注册身份，不是绝对路径、URL 或数据库 ID。名称带空格等情况会计算稳定的目录 `model_id`，不会修改原权重身份。模板中不包含权重或镜像文件；在另一个集群使用前，要先注册相同身份的本地权重。

管理员点「保存到目录」后，可在推理模板中按名称或权重身份搜索，再选保存的档位部署。保存的模板独立于原部署；请先另存再删除，已经删除的部署不会自动恢复参数。缺少本地注册时，部署解析返回 400 `MODEL_SOURCE_UNAVAILABLE`，不会先建立一个可下载的空模型，也不会将同名公网仓库当作该本地模型。已注册但没有可读副本等情况，仍由原有评估、存储权限和同步组规则判断；不会绕过这些规则。

部署自加环境变量仍默认排除。界面的本地权重说明同时写进 YAML 注释与测试记录，提醒它只引用权重，不打包文件。

## 1. 导出

```bash
curl --silent --show-error -X POST \
  -H "Authorization: Bearer $TOKEN" \
  http://<server>/api/v1/deployments/<deployment-id>/catalog-template
```

导出时 server 先对部署声明的 chat、completions、embeddings 操作各发一个最小请求（与健康检查相同，不计入用量）。视频、图片、语音等实测成本高的操作不发请求，在注释里标为未实测，照样可以导出。

返回的 `yaml` 开头是注释，例如：

```yaml
# Saved from GPUWorks deployment "olmo" (id 12).
# Verified on 1 x NVIDIA H20.
# No placement: the deployment ran on 1 card(s) with 97871 MiB each.
# Missing kv_bytes_per_token for vllm: compute it from config.json
# (scripts/estimate-calib/kv_layout.py) before proposing this template.
# Deployment env not exported: HF_TOKEN.
# Tested openai.chat: HTTP 200.
models:
  - model_id: allenai/olmo-2-0425-1b-instruct
    ...
```

注释不改变解析结果。字段与错误码见 [API 参考：另存为推理模板](../reference/api.md#另存为推理模板)。

## 2. 决定带上哪些环境变量

部署自己加的环境变量常含令牌（如 `HF_TOKEN`）与主机路径，默认全部不导出，名字列在 `excluded_env` 里。确认某个变量是模型需要的、且不含密钥，再在请求体里点名带上：

```bash
curl --silent --show-error -X POST \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"include_env": ["VLLM_USE_V1"]}' \
  http://<server>/api/v1/deployments/<deployment-id>/catalog-template
```

挂载、docker 参数等容器覆盖描述的是某一台主机，不导出。

## 3. 存进目录或提交上游

- **存进本集群的目录**：管理员把 `yaml` 交给 `POST /api/v1/inference-templates/evaluate` 预检，再用 `POST /api/v1/inference-templates/import` 导入。来自模板的部署导出的是原模型下的一个新用户档位，导入不影响原有档位。实测失败（`saveable: false`）的导出只供排查，不要导入。
- **贡献给官方目录**：把 `yaml` 与 `evidence`（GPU、驱动、镜像、引擎与实测结果的纯文本记录）一起提交 issue 或合并请求。注释里列出的缺项（例如 `kv_bytes_per_token`）由评审者补齐。

## 导出不了的情况

返回 409 `TEMPLATE_EXPORT_UNAVAILABLE` 时看 `params.reason`：

| reason | 原因与处理 |
|---|---|
| `not_running` | 部署没在运行或没有运行中的实例，先把它跑起来 |
| `distributed`、`pd` | 多机与 PD 部署暂不支持导出 |
| `template_assets`、`components` | 源模板带随包文件或组件模型，单个 YAML 装不下，改用模板编辑器 |
| `weights_not_public` | 当前来源（如 URL）不支持导出；可使用已注册的本地模型或 HF/ModelScope 仓库 |
| `revision_unpinned`、`revisions_differ` | 权重版本是分支名，或各副本解析到的 commit 不一致；把模型固定到一个 commit 后重新部署 |
| `launch_changed` | 配置或运行实例已变化，请应用配置并重启后重试 |
| `launch_snapshot_missing` | 缺少完整、可核验的实例启动快照，请用当前版本重启后重试 |
| `heterogeneous_gpus` | 实例使用不同型号的卡，单一型号选择器不能描述此组合 |
| `command_overridden` | 部署替换了引擎命令，启动方式无法写成模板参数 |
| `no_gpu_facts`、`template_missing`、`model_missing` | 实例没有绑定到具体卡，或源模板、模型已被删除 |

返回 409 `LAUNCH_NOT_EXPORTABLE` 表示导出的参数重建不出部署原来的命令行，平台拒绝给出近似结果；请把部署的启动参数与引擎版本反馈给维护者。
