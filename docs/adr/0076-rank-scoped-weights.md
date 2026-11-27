# ADR 0076: 按 rank 拆分的权重：子目录里写 rank 变量，创建时解析成每个 rank 一个模型制品

日期：2026-11-19 · 状态：已接受

## 背景

有的多机配方把权重预先切成每个 rank 一份：例如 4 台 GB10 上 TP4 的 DeepSeek-V4.1-Flash 配方，每个 rank 要自己的 `rank<r>.dsw` 与 Engram 表（约 128 GB），四份放在同一个仓库的 `rank0/` … `rank3/` 下，合计 510 GB。此前一个部署只有一个模型制品，组里每个节点都下载并挂载整份，既多占三倍磁盘与带宽，也让磁盘准入按整份计算。

模板的权重来源已经支持 `subfolder`，模型制品的身份（`source, source_id, revision, artifact_subfolder`）、agent 的下载与模型标记、节点间复制都按子目录区分；缺的只是"不同 rank 读不同子目录"。

## 决策

1. **形状：在 spec 自己的权重来源的 `subfolder` 里写 rank 变量**，如 `subfolder: rank${GPUWORKS_NODE_RANK}`。拼写规则与模板的启动字段同一条（`placement.Canonical`）：文档写带前缀的形式，不带前缀的 `${NODE_RANK}` 是别名，子目录不另立规则。子目录里只能出现这一个变量；同一 spec 的所有来源要么都写、要么都不写（只写一个的镜像源会让每个 rank 拿到整份仓库）。`weight_bytes` 与 `runtime_weight_bytes` 表示一个 rank 的大小。
2. **创建时展开成每个 rank 一个模型制品**：按部署的节点数 `n`，rank 0 的子目录按已有规则解析（选来源、revision、复用已登记的制品），其结果就是部署的 `model_id`；rank 1…n-1 用与 rank 0 相同的来源、仓库与 revision 加上自己的子目录解析（不会混用镜像或 commit）。整份仓库不能代替某个 rank 的子目录。
3. **绑定记在部署快照里**：`deploy.CatalogSelection.rank_weights` 是 `[{rank, subfolder, store_model_id}]`，`weight_subfolder` 保留模板原文（导出模板时原样写回）。与组件的 `store_model_id` 一样，绑定在创建时定下，以后不再从可变的目录重新解析。
4. **按 rank 读**：`planning.RankModelIDs(dep, r)` 是 rank `r` 读的模型（它自己的制品，加上各 rank 共用的组件与挂载）；控制器在每个 rank 的节点上只确保这些模型就绪（下载任务、复制、缓存填充都按这个制品），实例记录的读取与挂载路径也是它。`planning.DeploymentModelIDs` 仍是全部 rank 的并集，供下载归属、取消、驱逐保护使用；准备进度按节点所在的 rank 只列该 rank 的模型。
5. **准入与估算**：`planning.Input.WeightModel` 是该 rank 的制品（部署模型换上 rank 的身份），磁盘准入与磁盘不足的说明按它计算；显存估算把一个 rank 的权重乘以 rank 数作为整组权重，再按 TP×PP 分到每张卡。创建时的模型准入、保存位置与共享存储下载也按"每个 rank 的制品只对它自己的节点"。
6. **不支持的组合**：与 `components`、`pd_mode` 一起写在导入时拒绝；以 PD 方式部署时拒绝（`RANK_WEIGHTS_PD_UNSUPPORTED`）；请求显式给的 `model_id` 必须是 rank 0 的制品（`RANK_WEIGHTS_MODEL_NOT_RANK_ZERO`）。单机部署只读 rank 0。

## 取舍

- **否决 `components[].rank`**：每个 rank 写一个组件，模板就绑死了节点数，四个几乎一样的条目要同步维护；组件在所有 rank 上都挂载、主组件又与部署模型重合，加上 rank 维度要改写组件的全部语义。子目录变量沿用现有的来源、制品身份与下载路径，只多一个展开。
- **rank 0 的制品当部署模型**：名称、服务名、推理配置、估算用的 config 都继续读一行模型；其他 rank 的行由平台创建，只作制品身份。代价是在模型页上一个部署对应 n 行同仓库不同子目录的模型。
- **挂载保留仓库相对路径**：与已有的子目录语义一致，容器里权重在 `${GPUWORKS_MODEL_PATH}/rank<r>/…`，配方用 `${GPUWORKS_MODEL_PATH}/rank${GPUWORKS_NODE_RANK}/…` 引用；不为此另造"挂载到子目录"的规则。
- **快照里多一个键**：rank 绑定无法在每次调和时可靠地推导（模型行可能被删除重建、revision 可能被解析到别处），与组件绑定同理记成创建时的事实；它属于快照这个整体读写的具名类型，旧快照没有这个键即"不按 rank 拆分"。
- **未做**：自动放置时优先把 rank `r` 放到已有 `rank<r>/` 的节点（目前 rank 顺序就是节点顺序，已有其他 rank 子目录的节点照样下载自己的那份）。

## 后果

- `deploy.RankWeight`、`deploy.ExpandRankSubfolder` 等子目录规则；`inferencecatalog` 的导入校验；`deployment` 的按 rank 解析与准入；`planning.RankModelIDs`、`planning.NodeModelIDs`；控制器的 `ensureDeploymentModelsPresent` 按 rank。
- agent 与协议不变：每个 rank 的制品就是一个普通的带子目录的模型。
- 新失败码 `RANK_WEIGHTS_MODEL_NOT_RANK_ZERO`、`RANK_WEIGHTS_PD_UNSUPPORTED`；不新增表或列。

2026-11-20 补充：按 rank 拆分的来源，revision 是制品身份的一部分，不是偏好。各 rank 是同一个 commit 的一组预切文件，所以已登记的制品只要 revision 已知且与模板现在写的不同，就不匹配（模板重新导入新 revision 后，创建部署为每个 rank 新建该 revision 的制品并下载；空 revision 仍表示未知，不算不同）。整仓库来源仍按 ADR 0018 把 revision 当偏好。同一规则下，部署页「使用已登记的模型」只列 rank 0 在该 revision 的制品：其余 rank 的制品由平台创建，选了会被 `RANK_WEIGHTS_MODEL_NOT_RANK_ZERO` 拒绝；显式指定其他 revision 的制品返回 `TEMPLATE_MODEL_MISMATCH`。不新增表、列或失败码。

2026-11-25 补充：组件的权重来源也可以按 rank 拆分，撤回决策 6 中"不能与 `components` 一起写"。起因是两台 GB10 上 TP2 的 TensorFold 配方：EXL3 主包两机相同（约 204 GB），Engram 表按 rank 预切（每个 rank 约 101 GB），只能整体当组件时每个节点要多下载另一个 rank 的约 94 GiB。规则与 spec 自己的权重相同、逐组决定：每组权重的来源要么都写 rank 变量、要么都不写；primary 组件就是 spec 自己的权重，二者必须一致，都拆时 rank 绑定仍是部署的 `rank_weights`。非 primary 组件的绑定记在快照该组件自己的 `rank_weights`（同一个 `deploy.RankWeight` 形状，组件的 `store_model_id` 为 rank 0 的制品，`subfolder` 保留模板原文）；`planning.RankModelIDs` 按 rank 取组件的制品，启动按解析时的 rank（`planning.ModelPaths.Rank`）挂载该 rank 的制品到组件的 `mount_path`；组件的 `weight_bytes` 是一个 rank 的大小，磁盘准入按 rank 计，显存估算乘以 rank 数，与主权重一致；创建时的准入、组件获取规划与部署评估的 `acquisition.components` 按"每个 rank 的制品只对它自己的节点"，后者每个 rank 一项。`pd_mode` 与 PD 部署仍拒绝（`RANK_WEIGHTS_PD_UNSUPPORTED` 同样覆盖组件）：PD 的 rank 按角色实例编号，不是一个组。旧快照的组件没有 `rank_weights` 即不按 rank 拆分。不新增表、列或失败码。
