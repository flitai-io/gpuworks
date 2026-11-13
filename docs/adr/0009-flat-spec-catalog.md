# ADR 0009: 扁平档位目录取代五层证据模型

日期: 2026-08-30 · 状态: 已接受(取代 ADR 0007 的五层 schema 与 ADR 0008 的具名芯片类主体;chipmatch 谓词求值、量化门槛在数据不在代码等思想保留。完整设计见 `artifacts/archive/2026-08-24-inference-catalog/plan-template-catalog-v2.md`)。2026-09-15:决策 7 的 args JSON 列作为文件与存储格式不变;导入校验与部署时按 ADR 0020 把字典内 flag 拆进字段、字典外逐字透传,内部不再以原始 argv 参与估算。

## 背景

M29 实现部署到 mirror1 验收后,暴露两类问题:

1. **数据错误**:收编工具按「参数签名一字不差且 ≥2 卡」合并,B200/B300/GB200/GB300 四份 Blackwell recipe 天然同签名,以 4:2 票数压过 H100/H200 组,把 `--attention_config.use_fp4_indexer_cache=True`(sm100+ 专属)和 `--moe-backend deep_gemm_mega_moe`(sm90+)写进了 `nvidia_sm75_plus` 通用档位——通用配置在老卡上必然启动失败,与「95% 部署成功率」目标背道而驰。机械签名比对无法推导正确的通用配置,需要语义收编管线(另行设计,不属本 ADR)。
2. **结构过重,无法下手**:五层实体(Template→Variant→ChipSupport→Profile + Evidence/ValidationRun)带来——种子里 12,004 条 evidence 占体积 27%,而产品只在 admin 内部视图展示过;ValidationRun 全套 schema/校验/DTO/UI 类型俱在,实际数据 **0 条**;readiness/support_level/excluded_reason/warn/local_verified 五面旗子来自三套关注点,无人工填写者;手写导入需要伪造证据三件套与 sha256 profile_key,催生「内部严格格式 + 用户简化格式」双轨设计;前端为对抗目录粒度长出 40 个并行 flag 的二次收编列表。拓扑事实存两处且已互相矛盾(A100 档位字段 tp=1、args 里 `--tensor-parallel-size 2`)。

验证事实的真值已经由独立的内部研究基础设施承担(hygon 真机验证账本 179 条含 PASS/FAIL/镜像 digest/完整 argv;ascend 文档挖掘目录),目录库无需复制这套纪律。

## 决策

采用 gpustack `model-catalog.yaml` 派生的**扁平档位格式**,单一格式三用:内部存储、用户手写导入、目录管线产出,无翻译层。

1. **两层结构**:模型条目(model_id、display_title、parameter_count、release_date、context_length)+ 档位列表(spec)。spec = name、scenario(低延迟/均衡/高吞吐,可省)、engine、image、weights{quantization, sources}、gpus 谓词、args、env、pre_start。weights.sources 是多下载源列表(`- huggingface: <id>` / `- modelscope: <id>`,每源各自 repo id,顺序即优先级,部署时按网络可达性与用户偏好选源)。Variant/ChipSupport/Profile 三个实体合并为 spec 一个概念。
2. **GPU 适配用内联谓词,不用具名类**:`gpus: {vendor, cc: ">=8.9", name: "910[bc]"}` 直接写在 spec 上,反转 ADR 0008 的具名类注册表与三段命名文法(其谓词求值思想经 `chipmatch` 保留:改吃内联谓词,特异度 = 谓词窄度,具体名 > 窄区间 > 开区间;平手时 API 要求调用方显式给出 `template_spec_name`,不得按目录顺序静默选择不同权重)。
3. **支持只有两态**:spec 命中 = 可部署;模型级 `unsupported` 列表(gpus 谓词 + reason + 可选 quantization 限定)命中 = 不支持并给原因。没有完整可执行参数的"兼容性参考"条目不入目录、不展示。readiness/support_level/excluded_reason/warn/local_verified 字段机制全部删除。
4. **证据与验证外置**:Evidence、ValidationRun 表与 manifest 节全部删除。溯源纪律留在目录生产线的 git 产物(收编决策报告、检查台账)与独立验证基础设施账本中;目录库只存产品要展示和裁决的事实。
5. **并行度是运行时推导,不是目录数据**(gpustack 同款):spec 无 tp/nodes 字段;部署时显存估算决定分几张卡,引擎组装推导 `tp = 分到的卡数`;特殊拓扑(pp/ep/多机)由 args 显式写并行参数,组装见显式值不推导。拓扑事实单一来源。
6. **pre_start 一等化**:`pre_start_argv`(单条 argv,与 hygon 研究账本字段同名同形)与 `pre_start_script`(多行脚本,agent 落文件挂载容器内执行,64KB 上限)二选一;容器内 entrypoint 包装(`sh -c 'pre && exec engine'`),随容器重启重跑,进部署快照。比脚本更重的场景走自定义镜像。
7. **存储两张表**:models + specs,args/env 存 JSON 列,列表筛选走 SQL。
8. **静态卡表仅服务展示与假想规划**:NVIDIA 清单取自 developer.nvidia.com/cuda/gpus,两层分类(Data Center | Workstation/Consumer × 架构代,第二层由 CC 映射推导);国产卡列举(华为 310P/910B/910C/950PR/950DT、海光 K100/K500/BW1000/BW1100、平头哥 ZW810E/M890)。不参与匹配裁决。「我的芯片」直接读 GPU 表 distinct(name, vendor, cc),无解析步骤。
9. **旧资产一次性全删,不留过渡**:五层实体与迁移、chip_classes/chip_registry 表、consolidate v1、前端子串探测与 40-flag 二次收编逻辑、evidence/validation 的 DTO 与 UI。功能未发布,mirror1 数据丢弃重导。
10. **国产代际兼容继承是目录生产规则,不是匹配器规则**:K100/K100AI 真机 PASS 档位的 name 谓词扩展到整个 BW 系列(只报系列的 `BW`,以及 BW100、BW150、BW1000、BW1100 等带型号数字的写法);910B/910B4 真机 PASS 档位扩展到 910C/950DT/950PR。继承只来自 PASS,同一精确配置在后继卡上的明确 FAIL 优先并中止该卡继承;K500 不在 K100 继承范围。扩展后的范围显式写入 spec 的 gpus 谓词,chipmatch 仍只求值数据。
11. **目录按 NVIDIA 通用档优先生产,再补专卡和国产档**:第一阶段从 vLLM recipes 与 SGLang cookbook 逐模型生成 NVIDIA 档位。recipe 的测试卡只提供验证边界,不自动成为目录约束;Luna 逐条剥离卡数、显存利用率、长度、批量和测试卡优化,保留 parser、dtype、trust-remote-code、格式等启动语义,形成至少保证 H20(CC 9.0)可运行的通用档,能够证明 A100(CC 8.0)可运行时优先放宽到 A100。只有不能安全下沉的特定镜像、量化/内核门槛或硬件专属 flag 才形成谓词更窄的补充档。第二阶段仅依据真机测试数据生成 Ascend/Hygon 档位,不得用文档声明、统一默认镜像或统一参数模板补数量。
12. **模型日期属于原始模型,不属于权重制品**:`release_date` 表示原始模型仓库的创建/首次可审计发布时间。优先取规范 Hugging Face/ModelScope 仓库创建时间;仓库尚未公开时可用固定 commit 下结构化 recipe 的发布日期,并在管线审计中标明 fallback。量化、转载或跨组织权重仓库的创建时间不得覆盖它。内置目录发布要求每个模型都有合法 `YYYY-MM-DD`;UI 在同一模型组织内按该日期倒序、`model_id` 升序稳定展示。

## 后果

- 正面:目录格式一个中级用户十分钟能手写(gpustack 已验证此形态可运营 3770 行的目录);内部=用户=管线三用消灭翻译层与双轨;克隆/编辑/YAML 导入的实现面大幅缩小;拓扑单一来源消灭字段/args 分叉;"页面上的命令 = 真正跑的命令"由 server 统一组装保证。
- 负面/约束:产品内不再有证据浏览与「已验证」徽标,可信度叙事整体转移到目录生产线质量(luna/terra/sol 管线 + lint);「兼容性参考」信息(有声明无配置)退出产品,国产卡覆盖显示以研究管线产出完整配置为准;具名类的中文展示名消失,UI 需从谓词现算标签(如「NVIDIA · CC≥8.9」)。
- 暂缓项(需要时再加,不预留结构):值级门控(flag 值→CC 门槛的机械 lint)、local_verified、脚本上传设施、readiness 三分类。
