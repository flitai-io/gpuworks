# ADR 0063: 经用户确认，server 用云厂商现发的短期 kubeconfig 把接入 manifest 应用到托管集群

日期：2026-10-22 · 状态：已接受（修订 [ADR 0052](0052-kubernetes-nodes.md) 第 2 条"server 不持有 kubeconfig、不连 apiserver"）

## 背景

托管 Kubernetes（阿里云 ACK、DigitalOcean DOKS）接入时，用户要自己找 GPU 节点标签、执行 `kubectl apply`。而云账号已经在
gpuworks 里，集群清单、节点池与集群凭据都能从云 API 拿到。ADR 0052 定下 server 不碰集群，是为了不保管长期有效、权限很大的
kubeconfig。

## 决策

1. **集群从云账号实时列出，不落库**：provider 驱动可实现 `ClusterProvider`（列集群与 GPU 节点池、签发 kubeconfig）。用户选一个
   GPU 节点池接入时，建一个普通的集群条目，条目只多记来源（云账号、集群 ID）；节点选择器按节点池生成。
2. **server 只在用户确认的那一次操作里连集群**："应用到集群"与"从集群卸载"都先做服务端 dry-run，列出每个对象将被创建、更新或删除，
   用户确认后才执行。每次操作向云 API 现取一份 kubeconfig（ACK、DOKS 都是 15 分钟有效），用完即弃，**不落库、不进日志**。
   只认 kubeconfig 里自带的证书或 token，要运行程序（exec、auth-provider）的 kubeconfig 拒绝。
3. **只应用 gpuworks 自己的 manifest**：server-side apply（`fieldManager=gpuworks`）`kubemanifest.Render` 渲染出的固定几种对象，
   其他 kind 一律拒绝。卸载先删 DaemonSet（agent、占位 Pod、实例 Pod 随之删除），同一集群里其他 gpuworks namespace 还在用的
   集群级对象（PriorityClass）保留（已由 2026-11-12 修订取代：PriorityClass 按命名空间命名，卸载时删除条目自己的那个）。
4. **手动路径照旧**：每个条目仍有 `kubectl apply` 命令；API Server 没有公网地址的集群只能这样接入。手动建的条目没有来源，
   server 不会去连它。
5. **托管集群的节点归集群管**：云主机列表标出所属集群，安装 agent、开关机、释放与调整规格都被拒绝
   （`PROVIDER_INSTANCE_IN_CLUSTER`），避免与 DaemonSet、节点池抢同一台机器；列表项的 `actions` 由 server 给出，集群节点为空。
   判断依据是上次同步记下的所属集群，新加入、尚未同步的节点要先同步一次。

## 修订（2026-11-10）：一个云集群一个条目，持有它的多个同厂商 GPU 节点池

决策 1 里“用户选一个 GPU 节点池接入、节点选择器按节点池生成”改为：

1. **条目持有该集群的一组节点池。** 选中的节点池记在子表 `kube_cluster_pools`（条目、云厂商的节点池 ID、云厂商放节点池 ID 的节点标签键），
   不用 JSON 列。DaemonSet 用必需的 `nodeAffinity`（`In`，值为这些池的 ID）选节点；标签键由驱动给出：ACK 是
   `alibabacloud.com/nodepool-id`，DOKS 是 `doks.digitalocean.com/node-pool-id`。标签键随池一起存，云账号删除后 manifest 仍能渲染。
2. **一个集群仍只有一个条目**（决策不变）。要用它的更多节点池，改这个条目的节点池（`PUT /kube/clusters/{uuid}/pools`），再重新应用 manifest
   才在集群里生效；去掉的池上的 agent 随 DaemonSet 收缩而退出，其节点按 Kubernetes 节点的常规规则留在节点页、离线后清理。
3. ~~**一个条目的节点池必须同一厂商**（厂商决定 agent 挂载哪些宿主机路径），混选返回 `KUBE_CLUSTER_MIXED_VENDORS`。~~ 已由 2026-11-12 修订取代：条目没有厂商，节点池可以混选任意厂商。
4. **手动条目照旧**用自由格式的节点选择器，没有节点池记录；云条目不再接受节点选择器。
5. **旧条目迁移**（迁移 204）：来源是云账号集群、节点选择器恰好是一对 `key=value` 的条目，把这一对转成一条节点池记录并清空选择器；
   选择器是别的形状的条目保持原样，仍按选择器选节点，直到改它的节点池（改后只按节点池选节点）。
   因为改成了 `nodeAffinity`，旧条目下一次重新应用时 DaemonSet 的 Pod 模板会变，agent 滚动重启一次。

## 修订（2026-11-12）：条目没有厂商，节点池可以混选，应用时替换变了的 PriorityClass

1. **节点池不再限定同一厂商。** [ADR 0052](0052-kubernetes-nodes.md) 的 2026-11-12 修订取消了条目的厂商（每个 agent 自己识别节点的加速卡，不支持的节点待命），"厂商决定 agent 挂载哪些宿主机路径"的前提不再成立，所以创建与 `PUT …/pools` 不再检查厂商，`KUBE_CLUSTER_MIXED_VENDORS` 错误取消。一个云集群里不同厂商的节点池可以放进同一个条目。
2. **应用时替换变了的 PriorityClass。** PriorityClass 的数值与 `preemptionPolicy` 在 Kubernetes 里不可修改，server-side apply 会被拒绝。条目的优先级或允许抢占改了以后，"应用到集群"由 server 先删后建这个对象，应用结果里这一项的动作是 `replace`，dry run 同样列出。
3. **卸载删除条目自己的 PriorityClass。** PriorityClass 按命名空间命名（`gpuworks-inference-<命名空间>`），同一集群的其他条目不共用它，所以卸载把它与 DaemonSet、RBAC 对象、命名空间一起删掉；此前"其他 namespace 还在用的集群级对象保留"的规则不再需要。
4. **卸载有不依赖令牌的手动命令。** 条目 DTO 的 `remove_commands` 给出 kubectl 命令（删 DaemonSet、ClusterRoleBinding、ClusterRole、PriorityClass、命名空间），供 server 够不着的条目使用；云条目由 server 执行同样的删除。

## 取舍

- 不保存 kubeconfig，换来的是每次操作多一次云 API 调用，以及云账号必须有签发集群凭据的权限（ACK 还要求子账号在集群 RBAC 里
  有权限）。
- 不做"自动同步"或后台对账：集群变化（节点池扩缩）由 DaemonSet 自己跟随，server 不需要常连。
- 不引 client-go：manifest 只有九种对象，最小 REST 客户端足够，与 agent 侧的做法一致（ADR 0052 第 4 条）。
