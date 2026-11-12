# ADR 0017: 内置目录镜像引用改为纯 tag,取消 digest 不可变性强制要求

日期: 2026-09-07 · 状态: 已接受(取代 ADR 0010 中"镜像...仍通过不可变外部引用交付""必须补齐带 digest 的镜像"这部分镜像不可变性要求;ADR 0010 其余决策——多文件包结构、`assets/` 限制、权重/依赖不可变引用要求——不受影响)

## 背景

内置推理目录(`internal/server/inferencecatalog/seed/catalog/models/**/model.yaml`,约 280 个文件、882 处)的 `image:` 字段此前全部是 `tag@sha256:<digest>` 形式,由 `catalog_contract_test.go` 强制校验。

问题:上游 registry(Docker Hub、quay.io 等)不保证保留历史 digest——`:latest` 等 tag 被重新构建后,旧 digest 对应的 manifest/blob 可能被垃圾回收,导致该 digest **永久 404**,而不是等推理服务实际部署时才发现。实际报错形如:

```
all image sources failed: vllm/vllm-openai:latest@sha256:2286e8533ca8b6bc777594bae30524f1426ba46ca21797524e06df6a94b06635: exit status 1
```

镜像加速/多源转推救不了这个问题:agent 侧拉取候选链(`internal/agent/engine/engine.go` 的 `pullRefs`、`internal/server/store/deploy.go` 的 `imagePullRefsForImage`)只是把同一个失效 digest 换了个 registry 前缀,`@sha256:...` 后缀原样保留,所有候选源会一起失败,没有任何一条路径能拉到镜像。

## 决策

内置目录现有的 `image:` 字段一律改为纯 tag,去掉 digest 后缀;`catalog_contract_test.go` 里原先强制要求 image 必须带合法 digest 的正则校验直接删除,**不再对 image 字段的 digest 有无做任何强制要求**。也就是说以后新增/编辑某个 spec 时,image 写纯 tag 还是 `tag@sha256:...` 都合法,不会被测试拦截;如果某个 spec 确实带了 digest,行为和之前完全一样——照常按该 digest 去拉,拉不到照样在部署时报错(`all image sources failed: ...`),该谁来处理谁处理,不在目录层面预先判定对错。这与 ADR 0001 手动镜像目录"强制拒绝 digest"的硬校验不同——这里是不校验,不是反向强制。

不受影响的部分:

- 权重来源的 revision 不可变性(HF/ModelScope commit sha,`catalog_contract_test.go` 的 `immutableCatalogRevision` 校验)照旧;
- 目录包整体的 `PackageDigest` 内容寻址(`seed.go`)照旧;
- 部署/试验场路径对"调用方自己传入 digest 镜像"的软告警机制(`deployment/templates.go` 的 `rejectsDigestImage`/`ImageDigestWarning`、`deployment/evaluate.go` 的 `WarningImageDigestUnverified`、`agent/runtime/udocker` 拉取后按 digest 校验本地镜像)是通用兜底能力,不专属于内置目录的不可变性保证,继续保留,用于覆盖手工传参或外部导入目录包仍携带 digest 镜像的场景。

## 取舍

放弃了"内置目录镜像引用在发布时即被冻结、可复现"的保证,换取"tag 在上游被重建后依然能拉到镜像"的可用性——即镜像内容不再由目录本身保证不变,而是跟随上游 tag 的当前所指。这是本次决策认可的取舍,不再视为回退。
