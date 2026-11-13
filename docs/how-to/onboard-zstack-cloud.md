# 接入 ZStack Cloud

将已有 ZStack Cloud 环境添加到 GPUWorks，同步 KVM 物理机和 GPU 资源，再为需要纳管的物理机安装 Agent。

## 版本与环境要求

**需要 ZStack Cloud 5.4.2 或更高版本。** 添加或编辑 ZStack Cloud 时，凭据表单会显示此提示。

该下限根据当前实现的源码依赖确定：AK 请求的日期格式要求 Cloud 5.4.0 起的解析能力，GPU 的 `allocateStatus` 字段从 Cloud 5.4.2 引入。5.4.2 的添加环境、资源同步、Agent 安装和 GPU 占用释放尚未完成全流程实测，版本下限不代表该版本已通过完整兼容性验证。

接入前确认：

- GPUWorks server 已运行，使用有云账号和节点管理权限的账户登录控制台。
- server 能访问 Cloud API；Cloud 的许可证与版本支持 AccessKey 认证。
- 待安装 Agent 的物理机为 KVM 主机，状态为已连接，并有可达的管理地址和 SSH 连接信息。
- Agent 发起连接时，物理机能访问 GPUWorks 管理节点接入地址；Server 发起连接时，server 能访问物理机的 Agent 监听端口。地址与端口条件见[接入节点](connect-nodes.md)。

## 准备连接凭据

在 Cloud 中准备管理员的 AccessKey，确认密钥已启用，所属账号有查询物理机、GPU 和资源配置，以及执行 GPU 资源联动和主机接入操作的权限。

| 字段 | 填写内容 |
|---|---|
| 名称 | 自定义的环境名称，用于在 GPUWorks 中区分 Cloud 环境 |
| API 接口地址 | Cloud 管理节点的 API Endpoint，例如 `http://MANAGEMENT_NODE_IP:8080/zstack`；将占位内容替换为实际地址 |
| AccessKey ID | 管理员的 AccessKey ID |
| AccessKey Secret | 与 ID 对应的 Secret |

Endpoint 支持包含 `/zstack` 上下文和可选的 `/v1` 后缀。使用 Cloud 本地 AccessKey，无需保存或刷新登录 Session；原有 Session Token 不能转换为 AccessKey，需要通过编辑凭据填写 ID 和 Secret。

GPUWorks 与 Cloud 的系统时间需要同步。Cloud 配置 `rest.checkTimeZone=false` 时会忽略时区偏移，两端系统时区也须一致。查询和资源变更请求均使用 AccessKey 签名，Secret 不随请求发送。

## 添加 ZStack Cloud 环境

1. 在左侧菜单的“算力资源”分组打开**外部平台**（路径 `/providers`）。
2. 点击**添加外部平台**，或选择快速接入中的 **ZStack Cloud** 卡片。
3. 在“外部平台”字段选择 ZStack Cloud，核对版本提示，填写名称、API 接口地址、AccessKey ID 和 AccessKey Secret。
4. 点击**添加**。GPUWorks 会调用 Cloud API 校验凭据，通过后才保存，并在已接入列表中显示该环境。

凭据校验成功表示当前账号可以访问所校验的 API；后续资源同步与 Agent 安装还需要对应权限和网络条件。添加失败时，按界面显示的原因检查 Endpoint、密钥状态、权限、版本及系统时间后重试。

## 同步物理机和 GPU

1. 在**已接入的外部平台**列表中找到该环境（“外部平台”列为 ZStack Cloud，“平台类型”列为专有云），点击**同步**；**同步全部**会同步列表中所有支持同步的外部平台，包括公有云账号。
2. 展开环境所在行，核对同步得到的主机、管理地址和 GPU 型号、数量。此处的实例对应 Cloud 管理的 KVM 物理机。
3. 同步失败时，查看账号状态和错误提示，修复 API、权限或版本问题后重新同步。

实例清单来自最近一次同步；要获取 Cloud 侧的最新资源信息，需重新点击同步。

## 安装 Agent 并确认接入

1. 在需要纳管的物理机行点击**一键装 Agent**。
2. 选择接入方式，核对管理节点或计算节点接入地址。需要指定 SSH 凭据时，在高级设置中填写一次可用的密码或私钥。
3. 点击**安装**，查看预检查和安装报告；失败时保留报告中的原因，修正后重试。
4. 安装完成后打开**计算节点**，确认对应主机在线，并核对 GPU 和运行时信息。

安装目录、启动方式及后续 Agent 升级和卸载见[接入节点、升级与卸载 Agent](connect-nodes.md)。

## 更新连接凭据

在**已接入的外部平台**列表中点击该环境的**编辑**，更新名称、Endpoint 或 AccessKey。未填写的凭据字段保留原值，“外部平台”字段不能修改。保存后重新同步，确认新凭据能够查询资源。

凭据加密保存，Secret 不在编辑表单中回显。
